package protocol

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// A1（父批准）· 运行产物上行通道：节点 → 中枢的**唯一**产物传输。
//
// 不新增 WS 帧、不新增节点监听、不是新凭证模型：节点用自己的节点身份 Bearer +
// 当前 attempt 的授权（attempt / leaseId / attemptToken）向中枢已有的 HTTP 服务
// PUT 原始字节。
//
//	PUT /v1/node/tasks/{taskId}/artifacts/{sha256}
//	Authorization: Bearer <node-identity-token>
//	X-Anotify-Attempt: <整数>
//	X-Anotify-Lease-Id: <leaseId>
//	X-Anotify-Task-Token: <attemptToken>
//	X-Anotify-Artifact-Path: <URL 编码的逻辑相对路径>
//	Content-Type: application/octet-stream
//
// 语义边界（两侧共同遵守）：
//   - {sha256} 是**文件字节**的 SHA256（64 位小写 hex，不带 "sha256:"），不是 spec digest；
//   - body 是原始字节，不 JSON/base64 包一层；
//   - 201 首次接受，200 同 task+attempt+path+hash 幂等重传；同 path 不同 hash → 409；
//   - 授权按「收完 bytes 发布登记时」再同事务/CAS 校验，迟到文件不能凭先前验证过；
//   - 大小：单文件 16 MiB / 单 attempt 64 MiB / 最多 32 项（声明与实际都查，不截断）；
//   - 上传成功后才发 task.result；失败且仍有授权 → artifact_upload_failed。
//
// 这些常量只在这里定义一次：T1 handler 与节点客户端都用它们，不各写一份字面量。

const (
	// NodeTaskArtifactsPathPrefix 是节点上传产物的路径前缀（拼在中枢根上）。
	NodeTaskArtifactsPathPrefix = "/v1/node/tasks/"
	// NodeTaskArtifactsPathInfix 是 taskId 与 {sha256} 之间的固定段。
	NodeTaskArtifactsPathInfix = "/artifacts/"
	// ArtifactTransferContentType 是上传 body 的 media type（原始字节）。
	ArtifactTransferContentType = "application/octet-stream"

	// ArtifactHeaderAttempt 是当前 attempt 号（整数）。
	ArtifactHeaderAttempt = "X-Anotify-Attempt"
	// ArtifactHeaderLeaseID 是本次授权的 leaseId。
	ArtifactHeaderLeaseID = "X-Anotify-Lease-Id"
	// ArtifactHeaderTaskToken 是本次 attempt 的令牌（不进 query/日志）。
	ArtifactHeaderTaskToken = "X-Anotify-Task-Token"
	// ArtifactHeaderArtifactPath 是 URL 编码的托管逻辑路径。
	ArtifactHeaderArtifactPath = "X-Anotify-Artifact-Path"

	// MaxArtifactTransferBytes 是单文件上限（16 MiB）。
	MaxArtifactTransferBytes int64 = 16 << 20
	// MaxAttemptArtifactBytes 是单 attempt 合计上限（64 MiB）。
	MaxAttemptArtifactBytes int64 = 64 << 20
	// MaxArtifactTransferItems 是单 attempt 最多导出条目数。
	MaxArtifactTransferItems = 32
)

// 上传结论的 HTTP 状态（201 = 首次接受，200 = 同 key 幂等重传）。
const (
	ArtifactUploadStatusCreated = 201
	ArtifactUploadStatusOK      = 200
)

// A1 的上传错误码（封闭集合；只用于这条 HTTP，不是任务失败码）。
//
// 放在本文件而不是 enums.go：enums 是冻结的任务失败/剪枝码集合，A1 不广扩它。
const (
	// ArtifactErrPathInvalid 是逻辑路径不合法（绝对路径 / .. / NUL / 反斜杠 / 空段）。
	ArtifactErrPathInvalid = "artifact_path_invalid"
	// ArtifactErrHashMismatch 是实际字节 sha256 与路径里的不一致。
	ArtifactErrHashMismatch = "artifact_hash_mismatch"
	// ArtifactErrTooLarge 是超过单文件 / 单 attempt 的上限。
	ArtifactErrTooLarge = "artifact_too_large"
	// ArtifactErrConflict 是同 attempt 同逻辑 path 但不同 hash。
	ArtifactErrConflict = "artifact_conflict"

	// ReasonArtifactUploadFailed 是「产物上传确认失败且仍有授权」时的任务终态原因。
	// 它不是可自动重试的 infra 事实；已成功的托管 manifest 保留在结果里。
	ReasonArtifactUploadFailed = "artifact_upload_failed"
)

// ArtifactUploadResponse 是上传成功（200/201）的响应文档。
//
// 没有新资源 kind、没有 wrapper：它只是这次上传动作的确认，字段必须与请求对齐。
type ArtifactUploadResponse struct {
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// Validate 校验响应与请求四元组一致（节点侧不信任中枢回包里的不一致值）。
func (r *ArtifactUploadResponse) Validate() error {
	if r == nil {
		return fmt.Errorf("artifact upload: 响应为空")
	}
	if strings.TrimSpace(r.TaskID) == "" {
		return fmt.Errorf("artifact upload: 响应缺少 taskId")
	}
	if r.Attempt < 1 {
		return fmt.Errorf("artifact upload: 响应 attempt 必须 ≥1，实际 %d", r.Attempt)
	}
	if !ValidArtifactLogicalPath(r.Path) {
		return fmt.Errorf("artifact upload: 响应里的 path %q 不合法", r.Path)
	}
	if !ValidArtifactSHA256(r.SHA256) {
		return fmt.Errorf("artifact upload: 响应里的 sha256 %q 不是 64 位小写 hex", r.SHA256)
	}
	if r.Size < 0 || r.Size > MaxArtifactTransferBytes {
		return fmt.Errorf("artifact upload: 响应里的 size %d 超出范围", r.Size)
	}
	return nil
}

// DecodeArtifactUploadResponse 严格解码响应（未知字段 = 不对齐，拒绝）。
func DecodeArtifactUploadResponse(data []byte) (*ArtifactUploadResponse, error) {
	var resp ArtifactUploadResponse
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("artifact upload: 解码响应: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("artifact upload: 响应之后还有多余内容")
	}
	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return &resp, nil
}

// NodeArtifactUploadPath 拼出上传路径（taskId 与 sha256 都做路径转义）。
//
// sha256 必须先过 ValidArtifactSHA256；不接受 "sha256:" 前缀（那是 spec digest 的形状）。
func NodeArtifactUploadPath(taskID, sha256 string) (string, error) {
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("artifact upload: taskId 为空")
	}
	if !ValidArtifactSHA256(sha256) {
		return "", fmt.Errorf("artifact upload: sha256 %q 不是 64 位小写 hex", sha256)
	}
	return NodeTaskArtifactsPathPrefix + url.PathEscape(taskID) +
		NodeTaskArtifactsPathInfix + sha256, nil
}

// ValidArtifactSHA256 报告 s 是否是 64 位小写十六进制（不带 "sha256:" 前缀）。
func ValidArtifactSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// ValidArtifactLogicalPath 校验托管逻辑路径。
//
// 规则与 run.files 的相对路径同一取向：规范相对路径、`/` 分隔、无 NUL/反斜杠、
// 无空段 / "." / ".."、不以 "/" 开头。它只是**逻辑名**，永远不能拿去拼宿主路径
// （用户 workDir / 系统目录前缀都不允许出现在这里）。
func ValidArtifactLogicalPath(p string) bool {
	if p == "" || len(p) > 255 {
		return false
	}
	if strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
