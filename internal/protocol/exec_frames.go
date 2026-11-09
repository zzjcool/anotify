package protocol

import (
	"encoding/json"
	"time"
)

// 执行子协议的 10 个帧（capability.md §8.5）。
//
// 时序：dispatch → accept/reject → lease.grant → 启动 →
// (lease.renew/lease.ack)* → task.result/result.ack；task.cancel 可随时抢占，
// 进入 cancel.ack。
//
// 一条不变量：**派发不是授权**。task.dispatch 只带 assignmentId / proposedAttempt，
// 不授 lease、不带 attemptToken；执行权只来自中枢的 lease.grant（leaseId +
// attemptToken 都由中枢生成）。节点收到 grant 之后才启动子进程，attempt 才计数。
const (
	FrameTaskDispatch = "task.dispatch"
	FrameTaskAccept   = "task.accept"
	FrameTaskReject   = "task.reject"
	FrameLeaseGrant   = "lease.grant"
	FrameLeaseRenew   = "lease.renew"
	FrameLeaseAck     = "lease.ack"
	FrameTaskResult   = "task.result"
	FrameResultAck    = "result.ack"
	FrameTaskCancel   = "task.cancel"
	FrameCancelAck    = "cancel.ack"
)

// CapabilityRef 是任务钉住的目标能力（`{name,version,digest}`）。
//
// 没有 port：v4 的产出只有一个通道（emit），目标版本与 digest 钉在任务上。
type CapabilityRef struct {
	// Name 是能力名（谱系，不是某个版本）。
	Name string `json:"name"`
	// Version 是解析到的版本号（省略 version 的能力由内容寻址分配）。
	Version int `json:"version"`
	// Digest 是部署身份（artifact.Digest），双保险：version 之外再钉一次内容。
	Digest string `json:"digest"`
}

// TaskDispatchFrame 是中枢 → 节点的派发（**不授执行权**）。
//
// 节点先校验 digest 与 `configRevision == applied`，再决定 accept / reject。
// namespaceVars 是本任务所属 ns 的默认值（只含被引用的名字），随任务内存下发，
// 不落盘、不进 applied；namespaceVarsRevision 只描述这组值是哪一版。
type TaskDispatchFrame struct {
	Type string `json:"type"`
	// TaskID 是逻辑任务 id（一个逻辑 task 一行，attempt 是计数器）。
	TaskID string `json:"taskId"`
	// AssignmentID 是本次派发的关联 id（诊断用，不是执行授权）。
	AssignmentID string `json:"assignmentId"`
	// ProposedAttempt 是拟定的 attempt 序号（grant 生效时才算数）。
	ProposedAttempt int `json:"proposedAttempt"`
	// Namespace 是任务所属 ns（决定 ns 默认值与授权判定）。
	Namespace string `json:"namespace"`
	// Capability 是钉住的目标版本与 digest。
	Capability CapabilityRef `json:"capability"`
	// Params 是任务输入（同名直取对齐过，见 capability.md §4.2）。
	Params json.RawMessage `json:"params,omitempty"`
	// NamespaceVars 是本次任务可用的 ns 默认值（只含被引用的名字）。
	NamespaceVars map[string]string `json:"namespaceVars"`
	// NamespaceVarsRevision 是这组 ns 默认值的版本。
	NamespaceVarsRevision string `json:"namespaceVarsRevision"`
	// ConfigRevision 是本派发期望的机器级配置版本；与节点 applied 不一致 → stale_config。
	ConfigRevision string `json:"configRevision"`
}

// TaskAcceptFrame 是节点在配置校验通过后的接受。
//
// 发出它之前节点必须已完成：digest 校验、configRevision == applied、
// requires.commands 探测、ns ∈ 申报 ∩ 授权。失败一律走 task.reject。
type TaskAcceptFrame struct {
	Type            string `json:"type"`
	TaskID          string `json:"taskId"`
	AssignmentID    string `json:"assignmentId"`
	ProposedAttempt int    `json:"proposedAttempt"`
	Node            string `json:"node"`
}

// TaskRejectFrame 是节点对派发的拒绝。
//
// reason 是封闭集合，**只有** stale_config（capability.md §8.5）：机器级配置还没
// applied → 不计 attempt、不授 lease、不消耗非幂等重试预算，中枢回 pending /
// waiting_for_config 并换节点或退避。
type TaskRejectFrame struct {
	Type            string `json:"type"`
	TaskID          string `json:"taskId"`
	AssignmentID    string `json:"assignmentId"`
	ProposedAttempt int    `json:"proposedAttempt"`
	Node            string `json:"node"`
	// Reason 只能是 ReasonStaleConfig。
	Reason string `json:"reason"`
}

// LeaseGrantFrame 是中枢 → 节点的执行授权。
//
// leaseId / attemptToken 由中枢生成（节点不得自生成 lease）；grant 生效时 attempt
// 才计数，节点收到后才启动子进程。attemptToken 就是任务脚本里的
// ANOTIFY_TASK_TOKEN，emit 网关按它把请求归属到 task/attempt。
type LeaseGrantFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	Node    string `json:"node"`
	// LeaseID 是本次授权的租约 id（renew/ack/result 都必须回带同一个）。
	LeaseID string `json:"leaseId"`
	// AttemptToken 是本次 attempt 的一次性高熵令牌（重试后旧 token 立即作废）。
	AttemptToken string `json:"attemptToken"`
	// ExpiresAt 是本授权到期的绝对时间（中枢时钟）。
	ExpiresAt time.Time `json:"expiresAt"`
}

// LeaseRenewFrame 是节点绕**同一个中枢 leaseId** 的续租请求。
type LeaseRenewFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	LeaseID string `json:"leaseId"`
}

// LeaseAckFrame 是中枢对续租的确认：节点以「最后一次 ACK + 本地 deadline」判定
// 失联，到期自行 TERM，5s 后 KILL 整个进程组。
type LeaseAckFrame struct {
	Type      string    `json:"type"`
	TaskID    string    `json:"taskId"`
	Attempt   int       `json:"attempt"`
	LeaseID   string    `json:"leaseId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// TaskArtifact 是节点托管到系统结果目录的产物副本（不是用户 workDir 原文件）。
type TaskArtifact struct {
	// Path 是系统托管副本的相对路径（相对结果目录）。
	Path string `json:"path"`
	// SHA256 是副本内容的 64 位十六进制摘要（不带 "sha256:" 前缀）。
	SHA256 string `json:"sha256"`
}

// TaskResultError 是任务失败原因（`Task.status.result.error`）。
type TaskResultError struct {
	// Reason 是 capability.md §6.2 的封闭码（script_failed / files_escape / …）。
	Reason string `json:"reason"`
	// Message 是人话，可以变，不参与任何判断。
	Message string `json:"message,omitempty"`
}

// TaskResult 是「脚本跑完了」的执行结果通道。
//
// 脚本没有隐式返回值路由（没有 out/port），但**节点有执行结果通道**：退出码、
// summary、托管产物、失败原因都由节点显式上报，中枢权威校验 attempt/lease/token。
type TaskResult struct {
	// Summary 是唯一的结论句：成功 = 做了什么，失败 = 为什么（失败时必填）。
	Summary string `json:"summary"`
	// Artifacts 是系统托管的结果副本；用户 workDir 里的原文件永不进这里。
	// 空时给 []（不是 nil）——wire 上的空列表纪律。
	Artifacts []TaskArtifact `json:"artifacts"`
	// Error 只在失败 / 放弃时非空。
	Error *TaskResultError `json:"error,omitempty"`
}

// TaskResultFrame 是节点 → 中枢的终态上报。
type TaskResultFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	Node    string `json:"node"`
	LeaseID string `json:"leaseId"`
	// Phase 是终态（succeeded / failed / cancelled / abandoned）。
	Phase TaskPhase `json:"phase"`
	// ExitCode 是子进程退出码；被信号杀死 / 未启动时为空。
	ExitCode *int `json:"exitCode,omitempty"`
	// Result 是结果正文。
	Result TaskResult `json:"result"`
	// StartedAt 是子进程启动时间（timeoutSec 从这里起算）。
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt 是子进程结束 / 判定终态的时间。
	FinishedAt time.Time `json:"finishedAt"`
}

// ResultAckFrame 是中枢对结果上报的权威确认。
//
// accepted=false 表示中枢拒绝了这次结果（旧 attempt / lease 对不上 / token 作废）；
// 节点收到后不得据此改本地已结束的事实，只用于诊断与本地清理。
type ResultAckFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	// Accepted 是中枢是否接受这条结果。
	Accepted bool `json:"accepted"`
}

// TaskCancelFrame 要求节点停止某任务（可随时抢占）。
//
// 节点先 TERM，LeaseTermGrace（5s）后 KILL 整个进程组；槽位在**进程组真正回收
// 后**才释放——中枢不能只改 DB 状态就当节点空闲。
type TaskCancelFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	// Reason 是 CancelReason（cancelled_by_user / abandoned_by_user / abandoned）。
	Reason CancelReason `json:"reason"`
	// GraceSec 是 TERM 到 KILL 的宽限期（秒）；缺省用 LeaseTermGrace。
	GraceSec int `json:"graceSec"`
}

// CancelAckFrame 是节点对取消的回报。
//
// 只有 ProcessGroupReaped=true 才算槽位回收；false 表示还没收干净（进程不保证
// 停得住），中枢必须继续等而不是释放槽位。
type CancelAckFrame struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	// ProcessGroupReaped 报告进程组是否已经真正回收。
	ProcessGroupReaped bool `json:"processGroupReaped"`
}
