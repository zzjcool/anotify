package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// emit 中转的 2 个 WS 帧（父 E1 批准，见 /tmp/anotify-v4-emit-ws-approval.md）。
//
// 节点上的 HTTP emit 网关是本地入口（loopback / unix socket，Bearer attemptToken），
// 它只做本地预检（目标是否在本能力 next 里、令牌是否还是本地记录的那一个）；**权威
// 判定在中枢**：中枢先验证连接节点身份、任务当前 running、所属授权 namespace、
// attempt / leaseId / attemptToken 与租约未过期，再在同一个事务/CAS 边界里接受、
// 判 id 幂等并派发下游。所以能力脚本的一次 emit 需要一条「节点把同一个请求转给
// 中枢、再把中枢结论带回」的 WS 通道，就是这两帧。
//
// 纪律：
//   - task.emit 的 to/id/data **匿名嵌入 EmitRequest**：HTTP 入口与 WS 转发只有
//     一份 payload 解释（缺 target 仍是 HTTP 语义的 missing_target，不是另一种
//     payload 形状）。中枢据此复用 EmitHTTPStatus / EmitExitCode，不造第二套码。
//   - namespace 不在帧里：它由中枢按「任务的 namespace」与「节点申报 ∩ 授权」推导，
//     调用方不能通过 WS 帧覆盖。requestId 只是一次 RPC 的关联 id，不是业务幂等键、
//     不是调用序号；跨 attempt 的业务判等只用显式 id（同 task + 同 id）。
//   - ack 只带关联身份（requestId/taskId/attempt）+ 结论，**不回显 attemptToken**，
//     也不回显 leaseId：令牌不该被复制进第二条帧/日志。
//   - 成功只有 response（含剪枝：taskId:null + pruned），错误只有 error；两者互斥。
//     没有 Status 信封——本地 emit 不是资源 API，HTTP DTO 就是结论。
//
// 有界等待/超时/迟到 ack 丢弃是节点网关的实现（N2 后续），不是这一包的运行器：
// 本文件只有帧、校验与方向。
const (
	// FrameTaskEmit 是节点 → 中枢的 emit 转发请求（上行）。
	FrameTaskEmit = "task.emit"
	// FrameTaskEmitAck 是中枢 → 节点的 emit 结论（下行）。
	FrameTaskEmitAck = "task.emit.ack"
)

// TaskEmitFrame 把本地 emit 请求原样转给中枢。
//
// 字段表（父批准，顺序即文档顺序）：
//
//	type, requestId, taskId, attempt, leaseId, attemptToken, to, id?, data?
//
// `to`/`id`/`data` 的语义**逐字等于** HTTP EmitRequest（见 emit.go）：
//   - to 缺省 = 本地/中枢按 missing_target 400 处理，**不是**帧错误：本帧的
//     Validate 故意不要求 to，否则 Decode 阶段就把它判成坏帧，中枢再也没机会
//     回 HTTP 语义的 400/CLI 2。
//   - data 省略 = 空对象语义；给了就必须是 JSON **对象**。数组/标量/字符串/null
//     是参数错误（param_type 剪枝或本地 400），不能被默默当成"另一个对象"接受。
//   - id 是唯一的业务幂等键（同 task 内生效），requestId 不是。
type TaskEmitFrame struct {
	Type string `json:"type"`
	// RequestID 关联本次 RPC 与 ack；节点为每次 HTTP emit 生成新的，不持久化。
	RequestID string `json:"requestId"`
	// TaskID / Attempt / LeaseID / AttemptToken 是中枢做当前授权校验的身份四元组：
	// 旧 attempt / 过期租约 / 旧令牌一律拒绝，不因业务 id 已存在而绕过。
	TaskID       string `json:"taskId"`
	Attempt      int    `json:"attempt"`
	LeaseID      string `json:"leaseId"`
	AttemptToken string `json:"attemptToken"`
	// EmitRequest 匿名嵌入 → to/id/data 平铺在顶层，与 HTTP 请求同解释。
	EmitRequest
}

// TaskEmitAckFrame 是中枢对一次 task.emit 的结论。
//
// 字段表（父批准）：type, requestId, taskId, attempt, httpStatus, response?, error?
//
// response 与 error 严格互斥——成功只 response（200，含剪枝回执），错误只 error
// （httpStatus 与 EmitHTTPStatus(error.Error) 一致）。没有 attemptToken/leaseId/
// namespace，也没有业务 seq：迟到或不匹配的 ack 由节点网关按 requestId + taskId +
// attempt 丢弃（属于 N2 网关，不在本包）。
type TaskEmitAckFrame struct {
	Type string `json:"type"`
	// RequestID 必须与对应 task.emit 的 requestId 相同；节点据此找 waiter。
	RequestID string `json:"requestId"`
	// TaskID / Attempt 是回执的关联身份，节点必须核对后才会把结论交给原 HTTP 请求。
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	// HTTPStatus 是这次结论在本地 emit 网关上的 HTTP 状态码（200/400/401/409/500）。
	HTTPStatus int `json:"httpStatus"`
	// Response 仅成功（含剪枝）时出现；与 Error 互斥。
	Response *EmitResponse `json:"response,omitempty"`
	// Error 仅失败时出现；与 Response 互斥。
	Error *EmitError `json:"error,omitempty"`
}

// Validate 检查 task.emit 的必填关联身份与 data 形状。
//
// **不要求 to**：缺 to 必须继续走到中枢的 HTTP 语义（missing_target → 400 / CLI 2），
// 在这里拒绝等于把可回答的请求变成断连。也不校验 to 是否在本能力 next 里——那是
// 中枢权威（target_not_declared）。
func (f *TaskEmitFrame) Validate() error {
	if f.RequestID == "" {
		return fmt.Errorf("protocol: task.emit 缺 requestId（RPC 关联必填）")
	}
	if f.TaskID == "" {
		return fmt.Errorf("protocol: task.emit 缺 taskId")
	}
	if f.Attempt < 1 {
		return fmt.Errorf("protocol: task.emit attempt = %d，必须 >= 1", f.Attempt)
	}
	if f.LeaseID == "" {
		return fmt.Errorf("protocol: task.emit 缺 leaseId")
	}
	if f.AttemptToken == "" {
		return fmt.Errorf("protocol: task.emit 缺 attemptToken")
	}
	if len(f.Data) > 0 && !isJSONObject(f.Data) {
		return fmt.Errorf("protocol: task.emit data 必须是 JSON 对象，实际是 %s", jsonValueKind(f.Data))
	}
	return nil
}

// Validate 检查 ack 的互斥与状态对应。
//
//   - response / error 恰好出现一个；
//   - 成功（response）的 httpStatus 必须是 200（剪枝也是 200，不是 4xx）；
//   - 错误（error）的 httpStatus 必须等于 EmitHTTPStatus(error.Error)，且错误码
//     属于 EmitError 文档化的封闭集合（未知码不当作可信 ack——那应该走传输层
//     的「结果未知」，不能伪造一个业务结论）。
//
// 它不要求 Response/Error 的内部字段（taskId/eventId/pruned）：那些是 HTTP DTO
// 的既有形状，由 T1 的同一份解释决定，本包不复制第二套校验。
func (f *TaskEmitAckFrame) Validate() error {
	if f.RequestID == "" {
		return fmt.Errorf("protocol: task.emit.ack 缺 requestId")
	}
	if f.TaskID == "" {
		return fmt.Errorf("protocol: task.emit.ack 缺 taskId")
	}
	if f.Attempt < 1 {
		return fmt.Errorf("protocol: task.emit.ack attempt = %d，必须 >= 1", f.Attempt)
	}
	switch {
	case f.Response != nil && f.Error != nil:
		return fmt.Errorf("protocol: task.emit.ack 同时带 response 与 error（严格互斥）")
	case f.Response == nil && f.Error == nil:
		return fmt.Errorf("protocol: task.emit.ack 必须恰好带 response 或 error")
	}
	if f.Response != nil {
		if f.HTTPStatus != 200 {
			return fmt.Errorf("protocol: task.emit.ack 成功回执 httpStatus = %d，必须是 200", f.HTTPStatus)
		}
		return nil
	}
	if !IsEmitErrorCode(f.Error.Error) {
		return fmt.Errorf("protocol: task.emit.ack 未知 emit 错误码 %q", f.Error.Error)
	}
	if want := EmitHTTPStatus(f.Error.Error); f.HTTPStatus != want {
		return fmt.Errorf("protocol: task.emit.ack httpStatus = %d，与错误码 %q 的 %d 不符",
			f.HTTPStatus, f.Error.Error, want)
	}
	return nil
}

// emitErrorCodes 是 EmitError.Error 的封闭集合（见 emit.go 的文档注释）。
var emitErrorCodes = map[string]bool{
	ReasonTargetNotDeclared: true,
	ReasonMissingTarget:     true,
	ReasonTokenInvalid:      true,
	ReasonTaskNotRunning:    true,
	ReasonEmitIDConflict:    true,
	ReasonEmitUnreachable:   true,
}

// IsEmitErrorCode 报告 code 是否是 EmitError 的封闭错误码。
func IsEmitErrorCode(code string) bool {
	return emitErrorCodes[code]
}

// strictFrame 标记「Decode 必须严格解码并在返回前 Validate」的帧。
//
// 目前只有父 E1 批准的两帧（task.emit / task.emit.ack）：它们承载节点转发的
// 权威请求与中枢回执，多一个键就可能是伪造的 namespace/令牌回显，不能像普通
// 配置/执行帧那样默默忽略（父批准原文：「两帧纳入统一 Decode/方向表/严格字段
// 校验」）。其它帧只实现 Decode，没有 Validate，不走这条路径。
type strictFrame interface {
	Validate() error
}

// decodeStrictEmit 是 task.emit / task.emit.ack 的唯一解码入口。
//
//   - json.Decoder.DisallowUnknownFields：未知字段在**顶层与嵌套结构**
//     （EmitResponse / EmitPruned / EmitError）都拒绝。调用方不能塞 namespace，
//     也不能往 ack 里注入 attemptToken/leaseId 让它被静默忽略。
//   - 解码成功后**立即** Validate：ACK 的 response/error 互斥、关联身份必填、
//     httpStatus 与错误码对应都在接收路径上生效，不靠调用方「记得调」。
//   - 与 json.Unmarshal 对齐：Decoder 只读第一个 JSON 值，顶层值之后还有别的
//     内容同样是坏帧（显式检查 EOF）。
//   - 缺 to 仍是合法帧：Validate 故意不要求 to，留给中枢按 HTTP 语义回
//     missing_target 400 / CLI 2——可回答的业务语义不在这里变成断连。
func decodeStrictEmit(frameType string, data []byte, target strictFrame) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return nil, fmt.Errorf("protocol: 解码 %s: %w", frameType, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("protocol: 解码 %s: 顶层 JSON 值之后还有多余内容", frameType)
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	return target, nil
}

// isJSONObject 判断 data 是否是 JSON 对象（空 = 未提供由调用方先排除）。
func isJSONObject(data json.RawMessage) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// jsonValueKind 给错误信息一个可读的 JSON 类型名（不解析整份 data）。
func jsonValueKind(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}
