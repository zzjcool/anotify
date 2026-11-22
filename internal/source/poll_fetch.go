package source

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// PollMaxBodyBytes 与 webhook 入站同口径：超限 = 本轮失败，不截断（#532 语义 6）。
const PollMaxBodyBytes = 4 << 20

type resolverFunc func(ctx context.Context, host string) ([]net.IP, error)

func strictTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: blockPrivateDial}
	return &http.Transport{
		Proxy:             nil,
		DialContext:       dialer.DialContext,
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:      4,
	}
}

// blockPrivateDial 是 DNS rebinding 的第二道闸：判实际 dial 的 IP（第一道在写入期，第三道是解析预检）。
func blockPrivateDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("poll: dial 地址无法解析 %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("poll: 无法识别 dial 地址 %q", address)
	}
	if kind := sourcespec.BlockedIPKind(ip); kind != "" {
		return fmt.Errorf("解析到私网地址: %s（%s）", host, kind)
	}
	return nil
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("poll: 重定向超过 3 跳")
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("poll: 不跟随跨主机重定向（%s → %s）", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

func (r *Poller) checkResolvedHost(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("poll: url 没有 host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if kind := sourcespec.BlockedIPKind(ip); kind != "" {
			return fmt.Errorf("解析到私网地址: %s（%s）", host, kind)
		}
		return nil
	}
	ips, err := r.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("poll: 解析 %s 失败: %w", host, err)
	}
	for _, ip := range ips {
		if kind := sourcespec.BlockedIPKind(ip); kind != "" {
			return fmt.Errorf("解析到私网地址: %s → %s（%s）", host, ip, kind)
		}
	}
	return nil
}

type fetchResult struct {
	cursor      string
	notModified bool
	doc         any
	retryAfter  time.Duration
}

func (r *Poller) fetch(ctx context.Context, src *store.Source, cfg sourcespec.Config) (fetchResult, error) {
	target, err := url.Parse(cfg.URL)
	if err != nil {
		return fetchResult{}, fmt.Errorf("poll: url 无法解析: %w", err)
	}
	client := r.strict
	if cfg.AllowInsecure {
		client = r.insecure
	} else if err := r.checkResolvedHost(ctx, target.Hostname()); err != nil {
		return fetchResult{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return fetchResult{}, fmt.Errorf("poll: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range cfg.Headers {
		req.Header.Set(key, value)
	}
	if src.PollCursor != "" {
		req.Header.Set("If-None-Match", src.PollCursor)
	}
	if cfg.SecretRef != nil {
		secret, err := r.db.GetNamespaceSecret(ctx, src.NamespaceID, cfg.SecretRef.Name)
		if err != nil {
			return fetchResult{}, fmt.Errorf("poll: 取凭据 %q 失败: %w", cfg.SecretRef.Name, err)
		}
		req.Header.Set(cfg.SecretRef.Header, cfg.SecretRef.Prefix+secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fetchResult{}, fmt.Errorf("poll: 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return fetchResult{notModified: true, cursor: src.PollCursor}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fetchResult{retryAfter: retryAfterOf(resp)}, statusError(resp)
	}
	body, err := readLimited(resp.Body)
	if err != nil {
		return fetchResult{}, err
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return fetchResult{}, fmt.Errorf("poll: 响应不是 JSON: %w", err)
	}
	cursor := src.PollCursor
	if tag := strings.TrimSpace(resp.Header.Get("ETag")); tag != "" {
		cursor = tag
	} else if modified := strings.TrimSpace(resp.Header.Get("Last-Modified")); modified != "" {
		cursor = modified
	}
	return fetchResult{cursor: cursor, doc: doc}, nil
}

func retryAfterOf(resp *http.Response) time.Duration {
	delay, _ := retryAfter(resp.Header.Get("Retry-After"), time.Now())
	return delay
}

func statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		if header := resp.Header.Get("Retry-After"); strings.TrimSpace(header) != "" {
			return fmt.Errorf("poll: HTTP 429（Retry-After: %s）", strings.TrimSpace(header))
		}
	}
	return fmt.Errorf("poll: HTTP %d", resp.StatusCode)
}

func retryAfter(header string, now time.Time) (time.Duration, bool) {
	text := strings.TrimSpace(header)
	if text == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(text)
	if err != nil {
		return 0, false
	}
	delay := at.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func readLimited(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, PollMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("poll: 读响应失败: %w", err)
	}
	if len(raw) > PollMaxBodyBytes {
		return nil, fmt.Errorf("poll: 响应体超过上限 %d 字节（不截断，本轮失败）", PollMaxBodyBytes)
	}
	return raw, nil
}

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}
