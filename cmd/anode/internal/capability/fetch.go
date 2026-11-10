// Package capability 实现节点的**人工部署**闭包：按 `capability.deploy` 的 refs
// 用节点身份 Bearer 拉取不可变 Artifact 文档、严格校验、原子缓存，并回
// `capability.deployed`（applied / failed）。
//
// 它不是自收敛控制器：只有收到一条显式 capability.deploy 帧才动作；没有
// 「定时扫描中枢 / 自动补齐」的入口，也不接受把整份脚本塞进 WS 帧。
//
// 响应 wire：父契约只说「GET /v1/node/capabilities/{digest} 获取不可变 Artifact
// 文档」。本实现把响应体当作**产物文档本身**（artifact.Parse 同时接受 YAML/JSON），
// 这是一个单点 seam（decodeArtifactDocument），若父裁决改成信封，只改这一处。
package capability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// ArtifactPathPrefix 是节点拉取产物的 HTTP 路径前缀（CONTRACT rev2 §7）。
const ArtifactPathPrefix = "/v1/node/capabilities/"

// maxArtifactBodyBytes 是响应体上限：产物本身 1 MiB 上限，留出 YAML 包装余量。
const maxArtifactBodyBytes = 8 << 20

// defaultFetchTimeout 是单次拉取的总超时（context 之外的第二道保护）。
const defaultFetchTimeout = 30 * time.Second

// Fetch 的封闭错误码（回执里给人看，不参与任何判断）。
const (
	CodeBadRef         = "bad_ref"
	CodeNamespaceDeny  = "namespace_denied"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeBadStatus      = "bad_status"
	CodeBadArtifact    = "bad_artifact"
	CodeDigestMismatch = "digest_mismatch"
	CodeRefMismatch    = "ref_mismatch"
	CodeNetwork        = "network"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// Error 是拉取/校验失败的结构化原因。
type Error struct {
	// Code 是封闭码（Code* 常量）。
	Code string
	// Status 是 HTTP 状态码（0 = 没走到响应）。
	Status int
	// Message 是人话原因。
	Message string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s（HTTP %d）：%s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("%s：%s", e.Code, e.Message)
}

// Fetcher 用节点身份拉取产物。
type Fetcher struct {
	baseURL string
	token   string
	http    *http.Client

	// AllowNamespace 是客户端的 ns 授权预检（生效 = 本机申报 ∩ 中枢授权）。
	// nil = 不做本地预检（测试/显式信任调用方）。
	AllowNamespace func(namespace string) bool
}

// NewFetcher 从 config.Server（ws:// / wss://）推出 HTTP(S) 源。
func NewFetcher(serverURL, token string) (*Fetcher, error) {
	base, err := HTTPBaseURL(serverURL)
	if err != nil {
		return nil, err
	}
	return &Fetcher{
		baseURL: base,
		token:   token,
		http:    &http.Client{Timeout: defaultFetchTimeout},
	}, nil
}

// HTTPBaseURL 把 ws(s):// 中枢地址转成 http(s):// 源（只保留 scheme+host）。
func HTTPBaseURL(serverURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("capability: server 地址不合法: %w", err)
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "http"
	case "wss", "https":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("capability: server 必须是 ws:// 或 wss://，实际 %q", serverURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("capability: server 缺少主机名: %q", serverURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// SetHTTPClient 允许测试注入（超时/Transport）。nil 恢复默认。
func (f *Fetcher) SetHTTPClient(client *http.Client) {
	if client == nil {
		f.http = &http.Client{Timeout: defaultFetchTimeout}
		return
	}
	f.http = client
}

// Fetch 为一条部署 refs 拉取并校验产物。
//
// 顺序：ref 形状 → 本机 ns 授权预检 → Bearer GET → artifact.Parse/Validate →
// 重算 digest 与 refs 比对 → metadata name/version 与 refs 比对。任何一步失败
// 都不返回产物（调用方只会回 failed）。
func (f *Fetcher) Fetch(ctx context.Context, ref protocol.DeployRefs) (*artifact.Artifact, string, error) {
	if err := validateRef(ref); err != nil {
		return nil, "", err
	}
	if f.AllowNamespace != nil && !f.AllowNamespace(ref.Namespace) {
		return nil, "", &Error{Code: CodeNamespaceDeny,
			Message: fmt.Sprintf("本机的生效命名空间不含 %q（申报 ∩ 授权）", ref.Namespace)}
	}
	if f.token == "" {
		return nil, "", &Error{Code: CodeUnauthorized, Message: "缺少节点身份令牌"}
	}
	endpoint := f.baseURL + ArtifactPathPrefix + url.PathEscape(ref.Digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", &Error{Code: CodeBadRef, Message: err.Error()}
	}
	req.Header.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+f.token)
	req.Header.Set("Accept", "application/json, application/yaml")
	// Never forward node credentials, even to a same-origin Location. Keep
	// caller transport/timeout/jar settings without mutating its redirect hook.
	requestClient := *f.http
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		// Do can echo a malformed Location before our redirect hook runs.
		return nil, "", &Error{Code: CodeNetwork, Message: fetchTransportMessage(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 有界丢弃正文，错误只含固定类别/状态，不回显上游内容。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, "", &Error{Code: statusCode(resp.StatusCode), Status: resp.StatusCode,
			Message: http.StatusText(resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBodyBytes+1))
	if err != nil {
		return nil, "", &Error{Code: CodeNetwork, Message: "读取响应体失败: " + fetchTransportMessage(err)}
	}
	if len(body) > maxArtifactBodyBytes {
		return nil, "", &Error{Code: CodeBadArtifact, Status: resp.StatusCode,
			Message: fmt.Sprintf("响应体超过 %d 字节", maxArtifactBodyBytes)}
	}
	a, err := decodeArtifactDocument(body)
	if err != nil {
		return nil, "", &Error{Code: CodeBadArtifact, Status: resp.StatusCode, Message: err.Error()}
	}
	digest, err := artifact.Digest(a)
	if err != nil {
		return nil, "", &Error{Code: CodeBadArtifact, Status: resp.StatusCode,
			Message: "算 digest: " + err.Error()}
	}
	if digest != ref.Digest {
		return nil, "", &Error{Code: CodeDigestMismatch, Status: resp.StatusCode,
			Message: fmt.Sprintf("请求 %s，响应内容 %s", ref.Digest, digest)}
	}
	if a.Metadata.Name != ref.Name || a.Metadata.Version != ref.Version {
		return nil, "", &Error{Code: CodeRefMismatch, Status: resp.StatusCode,
			Message: fmt.Sprintf("请求 %s@%d，响应 %s@%d",
				ref.Name, ref.Version, a.Metadata.Name, a.Metadata.Version)}
	}
	return a, digest, nil
}

// fetchTransportMessage keeps transport failure classification without
// retaining untrusted URLs or response values in the public error object.
func fetchTransportMessage(err error) string {
	var timeoutErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeoutErr) && timeoutErr.Timeout()):
		return "HTTP request timeout"
	case errors.Is(err, context.Canceled):
		return "HTTP request canceled"
	default:
		return "HTTP request failed"
	}
}

// decodeArtifactDocument 是响应 wire 的唯一 seam：当前 = 产物文档本身。
func decodeArtifactDocument(body []byte) (*artifact.Artifact, error) {
	return artifact.Parse(body)
}

func validateRef(ref protocol.DeployRefs) error {
	var problems []string
	if !digestPattern.MatchString(ref.Digest) {
		problems = append(problems, fmt.Sprintf("digest %q 不是 sha256:<64 hex>", ref.Digest))
	}
	if !namePattern.MatchString(ref.Name) {
		problems = append(problems, fmt.Sprintf("name %q 不合法", ref.Name))
	}
	if ref.Version < 1 {
		problems = append(problems, fmt.Sprintf("version 必须 ≥1，实际 %d", ref.Version))
	}
	if ref.Namespace == "" || !namePattern.MatchString(ref.Namespace) {
		problems = append(problems, fmt.Sprintf("namespace %q 不合法", ref.Namespace))
	}
	if len(problems) > 0 {
		return &Error{Code: CodeBadRef, Message: strings.Join(problems, "; ")}
	}
	return nil
}

func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRef
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	default:
		return CodeBadStatus
	}
}

// IsCode 报告错误链里是否有指定封闭码。
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
