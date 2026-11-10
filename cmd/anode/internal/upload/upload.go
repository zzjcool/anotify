// Package upload 是节点 → 中枢的运行产物 HTTP 上行客户端（A1 唯一通道）。
//
// 只上传**系统托管副本**（resultstore 复制出来的那份），不读用户 workDir 任意路径；
// body 是原始字节流；凭证只走 header；同 path/hash/attempt 的传输失败可幂等重试
// （有界），但绝不因此重跑整个任务。
package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/internal/protocol"
)

// RequestTimeout 是单次上传请求的有界等待（A1 建议 30s）。
const RequestTimeout = 30 * time.Second

// DefaultRetries 是传输层重试的额外次数（总共 3 次）。
const DefaultRetries = 2

// maxResponseBytes 是响应体上限（响应只是五字段确认）。
const maxResponseBytes = 64 << 10

// Error 是一次上传的可判别失败。
type Error struct {
	// Code ∈ A1 错误码 / unauthorized / transport。
	Code    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upload: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("upload: %s: %s", e.Code, e.Message)
}

// IsCode 报告错误码是否匹配。
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client 是上传客户端。
type Client struct {
	// BaseURL 是中枢 HTTP 根（ws:// → http://）。
	BaseURL string
	// NodeToken 是节点身份令牌（Authorization: Bearer）。
	NodeToken string
	// HTTP 是注入点（测试用 httptest，生产用带超时的默认 client）。
	HTTP *http.Client
	// Retries 是传输失败的重试次数（额外次数）。
	Retries int
	// Backoff 是两次重试之间的等待（默认 200ms → 1s）。
	Backoff func(attempt int) time.Duration
}

// NewClient 用节点配置的 server/token 构造（server 必须是 ws:// / wss://）。
func NewClient(serverURL, nodeToken string) (*Client, error) {
	base, err := capability.HTTPBaseURL(serverURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(nodeToken) == "" {
		return nil, errors.New("upload: 缺少节点身份令牌")
	}
	return &Client{BaseURL: base, NodeToken: nodeToken, Retries: DefaultRetries}, nil
}

// Request 是一次上传的输入（大小/hash 都来自安全复制后的实际副本）。
type Request struct {
	TaskID       string
	Attempt      int
	LeaseID      string
	AttemptToken string
	// Path 是托管逻辑路径（相对结果目录，不含宿主前缀）。
	Path string
	// SHA256 是文件字节摘要（64 位小写 hex）。
	SHA256 string
	// Size 是文件字节数。
	Size int64
	// LocalPath 是 resultstore 托管副本的绝对路径。
	LocalPath string
}

// Upload 上传一份托管副本；201/200 都算成功，响应必须与请求对齐。
func (c *Client) Upload(ctx context.Context, req Request) (*protocol.ArtifactUploadResponse, error) {
	if err := validate(req); err != nil {
		return nil, err
	}
	target, err := protocol.NodeArtifactUploadPath(req.TaskID, req.SHA256)
	if err != nil {
		return nil, &Error{Code: protocol.ArtifactErrPathInvalid, Message: err.Error()}
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + target

	retries := c.Retries
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, retryable, err := c.once(ctx, url, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable || attempt == retries {
			break
		}
		if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, url string, req Request) (*protocol.ArtifactUploadResponse, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	f, err := os.Open(req.LocalPath)
	if err != nil {
		return nil, false, &Error{Code: "transport", Message: fmt.Sprintf("打开托管副本: %v", err)}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, &Error{Code: "transport", Message: fmt.Sprintf("stat 托管副本: %v", err)}
	}
	if !info.Mode().IsRegular() {
		return nil, false, &Error{Code: protocol.ArtifactErrPathInvalid,
			Message: fmt.Sprintf("托管副本 %s 不是普通文件", req.LocalPath)}
	}
	if info.Size() != req.Size {
		return nil, false, &Error{Code: protocol.ArtifactErrHashMismatch,
			Message: fmt.Sprintf("托管副本大小 %d 与清单 %d 不一致", info.Size(), req.Size)}
	}

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPut, url, f)
	if err != nil {
		return nil, false, &Error{Code: "transport", Message: err.Error()}
	}
	httpReq.ContentLength = req.Size
	httpReq.Header.Set("Content-Type", protocol.ArtifactTransferContentType)
	httpReq.Header.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+c.NodeToken)
	httpReq.Header.Set(protocol.ArtifactHeaderAttempt, fmt.Sprintf("%d", req.Attempt))
	httpReq.Header.Set(protocol.ArtifactHeaderLeaseID, req.LeaseID)
	httpReq.Header.Set(protocol.ArtifactHeaderTaskToken, req.AttemptToken)
	encodedPath, err := protocol.EncodeArtifactPathHeader(req.Path)
	if err != nil {
		return nil, false, &Error{Code: protocol.ArtifactErrPathInvalid, Message: err.Error()}
	}
	httpReq.Header.Set(protocol.ArtifactHeaderArtifactPath, encodedPath)

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}
	// Node and attempt credentials belong only to the configured endpoint.
	// Copy the injected client: preserve transport/timeout/jar without mutating
	// its redirect policy, and reject even caller-permitted same-origin hops.
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := requestClient.Do(httpReq)
	if err != nil {
		// Do may echo an invalid Location before CheckRedirect runs. Never
		// attach that untrusted URL/error to a task failure.
		return nil, true, &Error{Code: "transport", Message: transportMessage(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, true, &Error{Code: "transport", Status: resp.StatusCode,
			Message: "读取响应失败: " + transportMessage(err)}
	}
	if len(body) > maxResponseBytes {
		return nil, false, &Error{Code: "transport", Status: resp.StatusCode,
			Message: "响应体超过 64 KiB（不静默截断）"}
	}

	switch resp.StatusCode {
	case protocol.ArtifactUploadStatusCreated, protocol.ArtifactUploadStatusOK:
		parsed, err := protocol.DecodeArtifactUploadResponse(body)
		if err != nil {
			return nil, false, &Error{Code: "transport", Status: resp.StatusCode, Message: "上传确认无效"}
		}
		if parsed.TaskID != req.TaskID || parsed.Attempt != req.Attempt ||
			parsed.Path != req.Path || parsed.SHA256 != req.SHA256 || parsed.Size != req.Size {
			return nil, false, &Error{Code: "transport", Status: resp.StatusCode,
				Message: "响应与请求不一致"}
		}
		return parsed, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, false, &Error{Code: "unauthorized", Status: resp.StatusCode,
			Message: "上传授权失败"}
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge:
		return nil, false, &Error{Code: errorCode(body), Status: resp.StatusCode, Message: "上传请求被拒绝"}
	default:
		// 5xx / 其它：传输层可重试（仍有授权时）。不回显响应正文。
		return nil, true, &Error{Code: "transport", Status: resp.StatusCode,
			Message: "上传 HTTP 请求失败"}
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(attempt)
	}
	switch attempt {
	case 0:
		return 200 * time.Millisecond
	default:
		return time.Second
	}
}

// transportMessage preserves timeout/cancellation diagnostics without echoing
// untrusted HTTP response values (including malformed redirect Locations).
func transportMessage(err error) string {
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

// errorCode 只接受固定 A1 错误枚举，不透传任意 error/message/reason/details。
func errorCode(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		switch payload.Error {
		case protocol.ArtifactErrPathInvalid, protocol.ArtifactErrHashMismatch,
			protocol.ArtifactErrTooLarge, protocol.ArtifactErrConflict:
			return payload.Error
		}
	}
	return "transport"
}

func validate(req Request) error {
	if strings.TrimSpace(req.TaskID) == "" {
		return &Error{Code: protocol.ArtifactErrPathInvalid, Message: "taskId 为空"}
	}
	if req.Attempt < 1 {
		return &Error{Code: protocol.ArtifactErrPathInvalid, Message: "attempt 必须 ≥1"}
	}
	if strings.TrimSpace(req.LeaseID) == "" || strings.TrimSpace(req.AttemptToken) == "" {
		return &Error{Code: "unauthorized", Message: "缺少 leaseId / attemptToken"}
	}
	if !protocol.ValidArtifactLogicalPath(req.Path) {
		return &Error{Code: protocol.ArtifactErrPathInvalid, Message: fmt.Sprintf("逻辑路径不合法: %q", req.Path)}
	}
	if !protocol.ValidArtifactSHA256(req.SHA256) {
		return &Error{Code: protocol.ArtifactErrHashMismatch, Message: fmt.Sprintf("sha256 不合法: %q", req.SHA256)}
	}
	if req.Size < 0 || req.Size > protocol.MaxArtifactTransferBytes {
		return &Error{Code: protocol.ArtifactErrTooLarge,
			Message: fmt.Sprintf("文件大小 %d 超出单文件上限 %d", req.Size, protocol.MaxArtifactTransferBytes)}
	}
	if !strings.HasPrefix(req.LocalPath, "/") {
		return &Error{Code: protocol.ArtifactErrPathInvalid, Message: "LocalPath 必须是绝对路径"}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
