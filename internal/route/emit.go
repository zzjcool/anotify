package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 能力 emit（capability.md §4.1 / §8.5、V4-D4）。
//
// 一条不可协商的顺序：**先授权、后幂等回放**。`store.EmitAuthorized` 在同一个事务里
// 依次做「当前节点凭证 / ns 授权 / 当前 attempt+lease+token 未过期且未取消」→
// 「(taskId,id) 幂等/冲突」→「写 emit 记录 + 建子任务」。所以已经失效的执行**不能**
// 靠「我上次 emit 过」拿到旧回执继续跑——幂等不是绕过授权的后门。
//
// requestId 只属于 RPC 关联（节点 ↔ 中枢的帧配对），**不是**业务去重键：业务幂等域
// 是 (逻辑 task, 显式 id)。无 id 每次新派发；同 id 同请求回旧结果；同 id 不同请求 409。
// 这个文件不发明第二份 wire DTO：回执/错误直接复用 protocol.EmitResponse / EmitError。

// EmitError 是 emit 通道的可判别拒绝（reason 取 capability.md §6.2 封闭词表）。
//
// 它同时带 HTTP 状态与 CLI 退出码（protocol 的同一份映射），让 WS 回包与 G3 的
// HTTP 网关不可能各算一套。
type EmitError struct {
	Reason string
	Err    error
}

func (e *EmitError) Error() string {
	if e == nil {
		return "emit error"
	}
	if e.Err != nil {
		return fmt.Sprintf("emit: %s: %v", e.Reason, e.Err)
	}
	return "emit: " + e.Reason
}

// Unwrap 让 errors.Is/As 能穿透到 store 哨兵。
func (e *EmitError) Unwrap() error { return e.Err }

// HTTPStatus 是本地 emit 网关应回的状态码（200/400/401/409/500）。
func (e *EmitError) HTTPStatus() int { return protocol.EmitHTTPStatus(e.Reason) }

// ExitCode 是 CLI 退出码（0/2/3/4/5/6）。
func (e *EmitError) ExitCode() int { return protocol.EmitExitCode(e.Reason) }

// Body 是本地 emit 网关的错误体（{error,message}，不是公共 API 的 Status）。
func (e *EmitError) Body() protocol.EmitError {
	msg := ""
	if e.Err != nil {
		msg = e.Err.Error()
	}
	return protocol.EmitError{Error: e.Reason, Message: msg}
}

// EmitInput 是一次 emit 的调用事实。
type EmitInput struct {
	// Auth 是认证身份 + 当前执行三件套（由 WS 会话从连接凭证构造，绝不来自请求体）。
	Auth store.EmitAuth
	// To 是目标引用（必须在父任务钉住版本的 next 里）。
	To string
	// EmitID 是可选显式幂等键（业务域：同一逻辑 task 内）。
	EmitID string
	// Data 是请求体（下游 params 的同名直取来源）。
	Data json.RawMessage
}

// EmitResult 是一次 emit 的结论。
type EmitResult struct {
	// Response 是回给节点的回执（含剪枝形状 taskId:null + pruned）。
	Response protocol.EmitResponse
	// Duplicate 表示这是同 id 同请求的幂等回放（没有新建任务）。
	Duplicate bool
}

// Emit 执行一次能力 emit：解析 next → 参数对齐 → 授权事务落库（emit 记录 + 子任务）。
func (r *Router) Emit(ctx context.Context, in EmitInput) (EmitResult, error) {
	if in.Auth.TaskID == "" {
		return EmitResult{}, &EmitError{Reason: protocol.ReasonTokenInvalid}
	}
	if in.To == "" {
		return EmitResult{}, &EmitError{Reason: protocol.ReasonMissingTarget}
	}
	// 令牌 → 父任务（设计 §4.0：身份来自令牌，不来自通道）。这一步只用于读出
	// 「父任务钉住了哪个能力版本」来做 next 校验；**授权判定仍在 EmitAuthorized 事务里**。
	parent, err := r.db.VerifyAttemptToken(ctx, in.Auth.TaskID, in.Auth.AttemptToken)
	if err != nil {
		return EmitResult{}, mapEmitStoreError(err)
	}
	plan, err := r.PlanFromEmit(ctx, parent, in.To, in.Data)
	if err != nil {
		if errors.Is(err, ErrNotDeclared) {
			return EmitResult{}, &EmitError{Reason: protocol.ReasonTargetNotDeclared, Err: err}
		}
		return EmitResult{}, err
	}
	tasks := plan.Tasks(parent.ID)
	response := protocol.EmitResponse{To: in.To}
	if len(tasks) > 0 {
		taskID := tasks[0].ID
		response.TaskID = &taskID
		response.To = tasks[0].Capability
		response.Version = tasks[0].Version
	} else {
		// 剪枝回执：eventId 来自真实留痕行（source='route'），不是编造的 id。
		if len(plan.Pruned) > 0 {
			prune := plan.Pruned[0]
			response.Pruned = &protocol.EmitPruned{Reason: prune.Reason, Detail: prune.Detail}
			if eventID, perr := r.recordPrune(ctx, parent.NamespaceID, parent.TraceID, parent.Capability, in.To, prune); perr == nil {
				response.EventID = eventID
			}
		}
		if len(plan.Targets) > 0 {
			response.Version = plan.Targets[0].Version
			if plan.Targets[0].Capability != "" {
				response.To = plan.Targets[0].Capability
			}
		}
	}
	body, err := json.Marshal(response)
	if err != nil {
		return EmitResult{}, fmt.Errorf("emit: 编码回执: %w", err)
	}
	record := &store.EmitRecord{
		TaskID:    parent.ID,
		ToRef:     in.To,
		Data:      emitData(in.Data),
		Result:    body,
		CreatedAt: r.Now(),
	}
	if in.EmitID != "" {
		emitID := in.EmitID
		record.EmitID = &emitID
	}
	stored, first, err := r.db.EmitAuthorized(ctx, in.Auth, record, tasks)
	if err != nil {
		return EmitResult{}, mapEmitStoreError(err)
	}
	var out protocol.EmitResponse
	if err := json.Unmarshal(stored, &out); err != nil {
		return EmitResult{}, fmt.Errorf("emit: 解码已存回执: %w", err)
	}
	return EmitResult{Response: out, Duplicate: !first}, nil
}

// emitData 归一化落库的请求体字节：空 / 坏 JSON 一律存 `{}`（回执已经按剪枝处理）。
func emitData(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}

// recordPrune 写一条剪枝留痕（source='route'），返回真实事件 id。
//
// 剪枝永远留痕、且**不做幂等**（store.InsertPruneEvent 的既定语义）：同 id 的
// emit 重放会再留一条观察痕，但它不影响回执（回执来自已存的 emit 结果）。
func (r *Router) recordPrune(ctx context.Context, namespaceID, traceID, parentCapability, to string, prune Prune) (string, error) {
	payload, err := json.Marshal(map[string]string{"to": to, "detail": prune.Detail})
	if err != nil {
		payload = json.RawMessage(`{}`)
	}
	event := &store.Event{
		NamespaceID: namespaceID,
		TraceID:     traceID,
		Type:        prune.Reason,
		Payload:     payload,
	}
	if err := r.db.InsertPruneEvent(ctx, event); err != nil {
		return "", err
	}
	return event.ID, nil
}

// mapEmitStoreError 把 store 的授权/幂等哨兵翻成 emit 的封闭原因码。
//
// 身份不符与 ns 未授权都回 token_invalid（401）：对调用方而言「这把令牌不能做这件事」
// 是同一类回答，且不泄漏节点/ns 的存在性。任何未识别的错误保持 generic（映射成 500），
// 绝不静默当成成功。
func mapEmitStoreError(err error) error {
	if err == nil {
		return nil
	}
	var emitErr *EmitError
	if errors.As(err, &emitErr) {
		return emitErr
	}
	switch {
	case errors.Is(err, store.ErrEmitIDConflict):
		return &EmitError{Reason: protocol.ReasonEmitIDConflict, Err: err}
	case errors.Is(err, store.ErrTaskNotRunning):
		return &EmitError{Reason: protocol.ReasonTaskNotRunning, Err: err}
	case errors.Is(err, store.ErrAttemptTokenInvalid):
		return &EmitError{Reason: protocol.ReasonTokenInvalid, Err: err}
	case errors.Is(err, store.ErrLeaseMismatch):
		return &EmitError{Reason: protocol.ReasonTaskNotRunning, Err: err}
	case errors.Is(err, store.ErrNodeIdentityMismatch), errors.Is(err, store.ErrEmitNamespaceDenied):
		return &EmitError{Reason: protocol.ReasonTokenInvalid, Err: err}
	case errors.Is(err, store.ErrNotFound):
		return &EmitError{Reason: protocol.ReasonTokenInvalid, Err: err}
	case errors.Is(err, store.ErrInvalidTransition):
		return &EmitError{Reason: protocol.ReasonTaskNotRunning, Err: err}
	}
	return err
}
