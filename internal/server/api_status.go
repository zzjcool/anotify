package server

import (
	"errors"
	"net/http"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 统一错误体 Status（api-conventions §4.1 / §4.2）。
//
// 为什么要有这一层：v3 各 handler 各自写 `{"error": "..."}`，错误码只体现在 HTTP 码
// 上，客户端无法稳定分支。v4 契约把机器可读 reason 收敛成封闭集合，并规定
// CAS 冲突要在 details.current.resourceVersion 里回当前令牌——这两件事必须只有一份
// 实现，否则每个 handler 抄一遍迟早分叉。
const (
	apiVersionV1 = "anotify/v1"
	kindStatus   = "Status"

	// 通用 reason（api-conventions §4.2 通用段）。
	reasonBadRequest              = "BadRequest"
	reasonUnauthorized            = "Unauthorized"
	reasonForbidden               = "Forbidden"
	reasonNotFound                = "NotFound"
	reasonAlreadyExists           = "AlreadyExists"
	reasonConflict                = "Conflict"
	reasonMethodNotAllowed        = "MethodNotAllowed"
	reasonInternal                = "InternalError"
	reasonResourceVersionRequired = "resource_version_required"
	reasonResourceVersionInvalid  = "resource_version_invalid"
	reasonResourceVersionConflict = "resource_version_conflict"
	reasonKindMismatch            = "kind_mismatch"
	reasonNamespaceMismatch       = "namespace_mismatch"
	reasonNameImmutable           = "name_immutable"
	reasonUnknownField            = "unknown_field"
	reasonStatusNotWritable       = "status_not_writable"
	reasonImmutableField          = "immutable_field"
	reasonTooManyRequests         = "TooManyRequests"
	reasonValueInvalid            = "value_invalid"
)

type statusDoc struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	Code       int            `json:"code"`
	Reason     string         `json:"reason"`
	Message    string         `json:"message,omitempty"`
	Details    *statusDetails `json:"details,omitempty"`
}

type statusDetails struct {
	Causes            []statusCause  `json:"causes,omitempty"`
	Current           map[string]any `json:"current,omitempty"`
	RetryAfterSeconds int            `json:"retryAfterSeconds,omitempty"`
}

type statusCause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

// apiError 是领域 handler 直接构造的 Status 错误（code + reason + message）。
//
// 用法：handler 里 `return errStatus(400, "slug_taken", "…")`，再由 writeAPIError 统一
// 落成响应体。这样 reason 字面只在构造点出现一次。
type apiError struct {
	Code    int
	Reason  string
	Message string
}

func (e *apiError) Error() string { return e.Message }

// errStatus 构造一个 apiError。
func errStatus(code int, reason, message string) error {
	return &apiError{Code: code, Reason: reason, Message: message}
}

// writeStatus 写一个 Status 响应。
func writeStatus(w http.ResponseWriter, code int, reason, message string, details *statusDetails) {
	writeJSON(w, code, statusDoc{
		APIVersion: apiVersionV1,
		Kind:       kindStatus,
		Status:     "Failure",
		Code:       code,
		Reason:     reason,
		Message:    message,
		Details:    details,
	})
}

// writeAPIError 把任意 error 翻成 Status 响应。
//
// 映射顺序：apiError（handler 显式语义）优先，其次 store 的封闭 sentinel，最后 500。
// 未知错误必须给 500 而不是 400：把「我们没预料到」报成调用方错误会掩盖产品 bug。
func writeAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		writeStatus(w, ae.Code, ae.Reason, ae.Message, nil)
		return
	}

	// CAS 冲突要带 details.current.resourceVersion（W1：客户端据此重建令牌）。
	var conflict *store.ResourceVersionConflictError
	if errors.As(err, &conflict) {
		writeStatus(w, http.StatusConflict, reasonResourceVersionConflict, "resourceVersion is stale", &statusDetails{
			Current: map[string]any{"resourceVersion": conflict.CurrentResourceVersion},
		})
		return
	}

	var cross *store.CrossNamespaceError
	if errors.As(err, &cross) {
		writeStatus(w, http.StatusBadRequest, reasonNamespaceMismatch, cross.Error(), nil)
		return
	}

	var vc *store.VersionConflictError
	if errors.As(err, &vc) {
		writeStatus(w, http.StatusBadRequest, "version_content_immutable", vc.Error(), nil)
		return
	}

	var blocked *store.NamespaceBlockedError
	if errors.As(err, &blocked) {
		writeStatus(w, http.StatusConflict, reasonConflict, "namespace is not empty", &statusDetails{
			Causes: []statusCause{{Reason: reasonConflict, Message: "namespace has dependent resources", Field: "blocking"}},
		})
		return
	}

	switch {
	case errors.Is(err, store.ErrAuthRequired):
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "authentication required", nil)
	case errors.Is(err, store.ErrNamespaceRequired):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, "namespace is required", nil)
	case errors.Is(err, store.ErrNamespaceNotFound):
		writeStatus(w, http.StatusNotFound, reasonNotFound, "namespace not found", nil)
	case errors.Is(err, store.ErrNamespaceExists):
		writeStatus(w, http.StatusConflict, reasonAlreadyExists, "namespace already exists", nil)
	case errors.Is(err, store.ErrLastNamespace):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, "不允许删除最后一个命名空间", nil)
	case errors.Is(err, store.ErrResourceVersionRequired):
		writeStatus(w, http.StatusBadRequest, reasonResourceVersionRequired,
			"resourceVersion is required (PATCH/PUT via metadata.resourceVersion, DELETE via ?resourceVersion=)", nil)
	case errors.Is(err, store.ErrResourceVersionInvalid):
		writeStatus(w, http.StatusBadRequest, reasonResourceVersionInvalid, "resourceVersion is malformed", nil)
	case errors.Is(err, store.ErrResourceVersionConflict):
		// 没有具体 current 的兜底：仍是 409，客户端可重新 GET 取令牌。
		writeStatus(w, http.StatusConflict, reasonResourceVersionConflict, "resourceVersion is stale", nil)
	case errors.Is(err, store.ErrConfigInvalid):
		writeStatus(w, http.StatusBadRequest, reasonValueInvalid, err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		writeStatus(w, http.StatusNotFound, reasonNotFound, "not found", nil)
	case errors.Is(err, store.ErrSourceNotFound):
		writeStatus(w, http.StatusNotFound, reasonNotFound, "source not found", nil)
	case errors.Is(err, store.ErrSourceNameTaken):
		writeStatus(w, http.StatusConflict, "source_name_used", "source name already used", nil)
	case errors.Is(err, store.ErrSourceKindImmutable):
		writeStatus(w, http.StatusBadRequest, "source_kind_immutable", "spec.kind is immutable", nil)
	case errors.Is(err, store.ErrInvalidSourceKind):
		writeStatus(w, http.StatusBadRequest, "invalid_source_kind", err.Error(), nil)
	case errors.Is(err, store.ErrInvalidSourceConfig):
		writeStatus(w, http.StatusBadRequest, "invalid_source_config", err.Error(), nil)
	case errors.Is(err, store.ErrReservedEventSource):
		// 保留来源名（`route`）是**调用方输入错误**，不是服务端故障：v3 有专门用例钉它
		// 必须是 400（§4.2 的 reason 封闭集合里没有 5xx 用户错误码）。不加这一条就会落到
		// default 500，把「名字选错了」报成「服务器坏了」。
		writeStatus(w, http.StatusBadRequest, "invalid_source_config", err.Error(), nil)
	case errors.Is(err, store.ErrCapabilityInvalid):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error(), nil)
	case errors.Is(err, store.ErrVersionConflict):
		writeStatus(w, http.StatusBadRequest, "version_content_immutable", err.Error(), nil)
	case errors.Is(err, store.ErrSlugTaken):
		writeStatus(w, http.StatusConflict, "slug_taken", "playbook slug already exists", nil)
	case errors.Is(err, store.ErrEmitIDConflict):
		writeStatus(w, http.StatusConflict, "emit_id_conflict", err.Error(), nil)
	case errors.Is(err, store.ErrEmitNamespaceDenied):
		writeStatus(w, http.StatusForbidden, reasonForbidden, err.Error(), nil)
	case errors.Is(err, store.ErrNodeIdentityMismatch):
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "node identity mismatch", nil)
	case errors.Is(err, store.ErrNodeExists):
		writeStatus(w, http.StatusConflict, reasonAlreadyExists, "node already exists", nil)
	case errors.Is(err, store.ErrNodeDraining):
		writeStatus(w, http.StatusConflict, reasonConflict, "node is draining", nil)
	// A1/#332 产物域的封闭错误码（上传动作端点自己有 {"error":…} 动作文档，
	// 但下载/其它资源路径会把这些 sentinel 传到这里，落 default 500 会把
	// 「副本已过期」报成「服务器坏了」）。
	case errors.Is(err, store.ErrArtifactExpired):
		writeStatus(w, http.StatusGone, reasonArtifactExpired, "artifact copy expired", nil)
	case errors.Is(err, store.ErrArtifactConflict):
		writeStatus(w, http.StatusConflict, protocol.ArtifactErrConflict, err.Error(), nil)
	case errors.Is(err, store.ErrArtifactTooLarge):
		writeStatus(w, http.StatusRequestEntityTooLarge, protocol.ArtifactErrTooLarge, err.Error(), nil)
	case errors.Is(err, store.ErrArtifactPathInvalid):
		writeStatus(w, http.StatusBadRequest, protocol.ArtifactErrPathInvalid, err.Error(), nil)
	case errors.Is(err, store.ErrArtifactHashMismatch):
		writeStatus(w, http.StatusBadRequest, protocol.ArtifactErrHashMismatch, err.Error(), nil)
	case errors.Is(err, store.ErrArtifactUploadExpired):
		writeStatus(w, http.StatusConflict, reasonConflict, "artifact upload ticket expired", nil)
	case errors.Is(err, store.ErrArtifactReference):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error(), nil)
	case errors.Is(err, store.ErrNoCapacity), errors.Is(err, store.ErrSlotUnavailable):
		writeStatus(w, http.StatusConflict, reasonConflict, err.Error(), nil)
	case errors.Is(err, store.ErrTooManyInstances):
		writeStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error(), nil)
	case errors.Is(err, store.ErrNamespaceInputsChanged):
		writeStatus(w, http.StatusConflict, reasonConflict, err.Error(), nil)
	case errors.Is(err, store.ErrSourceEventReceiptMissing):
		writeStatus(w, http.StatusInternalServerError, reasonInternal, err.Error(), nil)
	default:
		writeStatus(w, http.StatusInternalServerError, reasonInternal, err.Error(), nil)
	}
}
