package protocol

import "encoding/json"

// emit 网关（capability.md §4.0/§4.1）。
//
// 节点上只有一个监听：能力脚本带着本次任务的令牌 POST 到
// ANOTIFY_EMIT_ENDPOINT，节点做本地预检后转发中枢，中枢才是权威（建下游 task、
// id 幂等、跨节点派发、旧 attempt 拒绝）。本文件只是这条 HTTP 的 DTO 与退出码，
// 不含转发逻辑。
const (
	// EmitPath 是 emit 网关的 HTTP 路径（拼在 ANOTIFY_EMIT_ENDPOINT 之后）。
	EmitPath = "/emit"
	// EmitAuthScheme 是凭据头的 scheme：`Authorization: Bearer <attemptToken>`。
	// 凭据走头，不进 payload，也不进 argv。
	EmitAuthScheme = "Bearer"
	// EmitAuthHeader 是凭据头。
	EmitAuthHeader = "Authorization"
	// EmitContentType 是请求体的 media type。
	EmitContentType = "application/json"
)

// EmitRequest 是 `anode emit <name> --data <json>` 的线上请求形状。
//
// `to` 必须是本能力 `spec.next` 里的一个名字（`name` 或 `name@version`）；
// 不给 `to` 是 missing_target（400 / CLI 2），**不是**剪枝。
//
// `id` 是**可选**显式幂等键：同一逻辑 task + 同一 id 的同一请求只派发一次，
// 重复请求返回旧结果；同 id 但 to/data 不同 → 409 emit_id_conflict。不带 id
// 每次都是新派发（不按调用序号自动去重）。
type EmitRequest struct {
	To   string          `json:"to"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// EmitPruned 是「事件已接收、下游没建 task」的剪枝回执。
//
// reason ∈ param_missing / param_schema / param_type（emit 路径）或 no_target /
// event_schema（源投递路径）。剪枝**不是 4xx**：回执 taskId 为 null、CLI 退出码 0。
type EmitPruned struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// EmitResponse 是 emit 的成功回执（含剪枝）。
//
// TaskID 用指针：剪枝回执要真的写 `"taskId":null`（不是省略、不是空串）——前端与
// 脚本据此区分「已建任务」和「接收了但没派发」。非 null 时它是**本次 emit 真实创建**
// 的任务 id：正常派发是子任务；下游缺失（downstream_missing）是 phase=failed 的
// **派发失败 Task**（已建行、无执行事实，不是「已成功执行」）。
type EmitResponse struct {
	TaskID  *string `json:"taskId"`
	EventID string  `json:"eventId,omitempty"`
	To      string  `json:"to"`
	// Version 是回执目标的版本：已解析成功或明确声明了正版本时恒返回；
	// 未声明且未解析（裸 ref 的下游缺失）没有版本事实，字段省略而不是公开哨兵 0。
	Version int         `json:"version,omitempty"`
	Pruned  *EmitPruned `json:"pruned,omitempty"`
}

// EmitError 是本地 emit 网关的错误体（`{error, message}` + CLI 退出码）。
//
// 它不是公共 API 的 Status：不要拿它当 HTTP 资源的错误信封。
type EmitError struct {
	// Error 是封闭码（target_not_declared / missing_target / token_invalid /
	// task_not_running / emit_id_conflict / emit_unreachable）。
	Error string `json:"error"`
	// Message 是人话，可以变，不参与任何判断。
	Message string `json:"message,omitempty"`
}

// CLI 退出码（capability.md §4.1「返回值就是判据」）。
//
// 脚本据此决定要不要 `set -e` 退出。除 0 以外都只写一行 JSON 到 stdout，
// 原因码同时写 stderr 的一行。**这些常量只是文档化的判据**：网关不解析它们，
// CLI 负责把结果翻译成退出码。
const (
	// EmitExitOK 接受，任务已建（或剪枝：taskId=null + pruned）。
	EmitExitOK = 0
	// EmitExitBadRequest 目标不在 next 里 / 没给目标（400）。
	EmitExitBadRequest = 2
	// EmitExitTokenInvalid 令牌无效或过期（401）。
	EmitExitTokenInvalid = 3
	// EmitExitTaskNotRunning 任务已结束（I8，409）。
	EmitExitTaskNotRunning = 4
	// EmitExitUnknown 端点不可达 / 请求结果未知：**可能已派发**，不能凭它推断
	// infra 失败并整任务重跑（要安全重发就带同一个 --id）。
	EmitExitUnknown = 5
	// EmitExitIDConflict 同 id 不同请求（409）。
	EmitExitIDConflict = 6
)

// EmitExitCode 把 emit 的结论码翻译成 CLI 退出码。
//
// 未知码 → EmitExitUnknown：宁可让脚本认为「结果未知」，也不要错误地报成
// 「确定失败」（那会诱使作者加重试，而重试可能重复派发）。
func EmitExitCode(reason string) int {
	switch reason {
	case "":
		return EmitExitOK
	case ReasonTargetNotDeclared, ReasonMissingTarget:
		return EmitExitBadRequest
	case ReasonTokenInvalid:
		return EmitExitTokenInvalid
	case ReasonTaskNotRunning:
		return EmitExitTaskNotRunning
	case ReasonEmitIDConflict:
		return EmitExitIDConflict
	}
	return EmitExitUnknown
}

// EmitHTTPStatus 把 emit 的结论码翻译成网关的 HTTP 状态码。
//
// 剪枝不是错误（200）；空 reason = 已派发（200）。
func EmitHTTPStatus(reason string) int {
	switch reason {
	case "":
		return 200
	case ReasonTargetNotDeclared, ReasonMissingTarget:
		return 400
	case ReasonTokenInvalid:
		return 401
	case ReasonTaskNotRunning, ReasonEmitIDConflict:
		return 409
	}
	return 500
}
