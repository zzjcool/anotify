package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 统一错误体 Status（api-conventions §4.1 / §4.2）：v3 各 handler 各自写 `{"error": "..."}`，
// 客户端无法按码分支。机器可读 reason 的封闭集合与 CAS 的 details.current.resourceVersion
// 必须只有一份实现，否则每个 handler 抄一遍迟早分叉。
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

// apiError 是领域 handler 直接构造的 Status 错误：`errStatus(400, "slug_taken", "…")` 交给
// writeAPIError 落成响应体，reason 字面只在构造点出现一次。
type apiError struct {
	Code    int
	Reason  string
	Message string
	// Details 是可选的结构化细节（YAML 文档与 #519 的组合子校验用它）；nil 时不出现。
	Details *statusDetails
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

// writeAPIError 把任意 error 翻成 Status 响应：apiError（handler 显式语义）优先，其次 store
// 的封闭 sentinel，最后 500——把「我们没预料到」报成 400 会掩盖产品 bug。
func writeAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		writeStatus(w, ae.Code, ae.Reason, ae.Message, ae.Details)
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

	var locked *store.CapabilityLockedError
	if errors.As(err, &locked) {
		writeStatus(w, http.StatusConflict, playbook.CodeDependencyLocked, locked.Error(), capabilityLockDetails(locked))
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
		// #519：带路径校验失败逐条落进 details.causes[]（红点点到具体那行配置）。
		writeStatus(w, http.StatusBadRequest, sourceConfigReason(err), err.Error(), sourceConfigCauses(err))
	case errors.Is(err, store.ErrSourceTargetInvalid):
		// targets 指向不存在的出口能力是**调用方输入错误**（改引用就好），不是服务端故障。
		// 不加这一条就落 default 500：客户端会把它当瞬时故障去重试一个永远不会成功的写。
		// reason 复用 source 域的既有封闭码（targets 的形状错也走它），不新造词。
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
	// #510：release 不在这个包名下 → 404；installId 与本次 release 对不上 ⇒ 客户端视图过期（409，不删不猜）。
	case errors.Is(err, store.ErrPlaybookReleaseNotFound):
		writeStatus(w, http.StatusNotFound, "release_not_found", err.Error(), nil)
	// #511：钉住的包版本既不在历史里、也不是当前版本 → 404（不回退成「装当前版」：那就成了静默装错）。
	case errors.Is(err, store.ErrPlaybookVersionNotFound):
		writeStatus(w, http.StatusNotFound, "version_not_found", err.Error(), nil)
	case errors.Is(err, store.ErrPlaybookInstallMismatch):
		writeStatus(w, http.StatusConflict, "install_conflict", err.Error(), nil)
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

func capabilityLockDetails(e *store.CapabilityLockedError) *statusDetails {
	causes := make([]statusCause, 0, len(e.Consumers))
	for _, c := range e.Consumers {
		causes = append(causes, statusCause{
			Reason:  playbook.CodeDependencyLocked,
			Field:   "name",
			Message: fmt.Sprintf("预案 %q 的 release %q 依赖它（先卸载该 release）", c.Playbook, c.Release),
		})
	}
	if len(causes) == 0 {
		return nil
	}
	return &statusDetails{Causes: causes}
}

// sourceConfigReason 决定 `invalid_source_config` 的顶层 reason（#519）：纯「一个未知键」的错
// 沿用文档层的 `unknown_field`（客户端已按它分支，也与 YAML 写面同口径），其余仍是既有码。
func sourceConfigReason(err error) string {
	causes := sourceConfigCauseList(err)
	if len(causes) == 1 && causes[0].Reason == sourcespec.CauseReasonUnknownField {
		return reasonUnknownField
	}
	return "invalid_source_config"
}

// sourceConfigCauses 把带路径校验失败翻成 details.causes[]（#519）：field 补 `spec.`——它是
// **文档路径**（与 YAML 写面同写法），前端拿它去文档树定位；sourcespec 不该知道信封。
func sourceConfigCauses(err error) *statusDetails {
	causes := sourceConfigCauseList(err)
	if len(causes) == 0 {
		return nil
	}
	out := make([]statusCause, 0, len(causes))
	for _, cause := range causes {
		field := cause.Field
		if field != "" {
			field = "spec." + field
		}
		out = append(out, statusCause{Reason: cause.Reason, Message: cause.Message, Field: field})
	}
	return &statusDetails{Causes: out}
}

// sourceConfigCauseList 取出 sourcespec 的结构化结论。
func sourceConfigCauseList(err error) []sourcespec.Cause {
	var causes *sourcespec.CausesError
	if !errors.As(err, &causes) {
		return nil
	}
	return causes.Causes
}
