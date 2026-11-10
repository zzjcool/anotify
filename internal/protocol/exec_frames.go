package protocol

import (
	"encoding/json"
	"fmt"
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
	// AssignmentExpiresAt 是中枢给出的**派发窗口**（初值 30s，A3）。
	//
	// 中枢与节点用同一个窗口判 pending reservation 的有效期：Node 从 accept 到
	// grant 的有界期限就是它。它不含执行 lease/token/slot 授权。
	AssignmentExpiresAt time.Time `json:"assignmentExpiresAt"`
}

// TaskAcceptFrame 是节点在配置校验通过后的接受。
//
// 发出它之前节点必须已完成：digest 校验、configRevision == applied、
// ns ∈ 申报 ∩ 授权，并原子预留真实空闲 slot。accept 前 task.reject 只允许
// stale_config | slot_unavailable；requires.commands / HOME / workDir / export
// 的确定性错误在 lease.grant 后报告 failed，计入已授权 attempt。
type TaskAcceptFrame struct {
	Type            string `json:"type"`
	TaskID          string `json:"taskId"`
	AssignmentID    string `json:"assignmentId"`
	ProposedAttempt int    `json:"proposedAttempt"`
	// Node 是已登记 Node.metadata.name / anode.yaml.name，不是 DB 内部 UID。
	// 服务端用已认证 session 映射 DB id，不能用 body.node 选择另一个身份。
	Node string `json:"node"`
	// Slot 是节点**原子预留的真实空闲槽号**（0..N-1，A3）。
	//
	// 用 *int 表达 presence：0 是合法槽号，缺失（nil）或负数无效。accept 回报后
	// 中枢的 grant 必须回同一个 slot；节点也只在自己预留的 slot 上启动进程。
	Slot *int `json:"slot"`
}

// TaskRejectFrame 是节点对派发的拒绝。
//
// reason 是 A3 封闭集合：stale_config | slot_unavailable。两者均不计 attempt、
// 不授 lease；前者是机器级配置未 applied，后者是真实 slot/预算不可用。
// 缺命令、HOME、workDir、export 等执行准备错误不能伪装为 pre-grant reject。
type TaskRejectFrame struct {
	Type            string `json:"type"`
	TaskID          string `json:"taskId"`
	AssignmentID    string `json:"assignmentId"`
	ProposedAttempt int    `json:"proposedAttempt"`
	// Node 是已登记 Node.metadata.name / anode.yaml.name，不是 DB 内部 UID。
	// 服务端用已认证 session 映射 DB id，不能用 body.node 选择另一个身份。
	Node string `json:"node"`
	// Reason 是封闭集合：ReasonStaleConfig | ReasonSlotUnavailable（A3）。
	//
	// slot_unavailable = 本机没有真实空槽/预算吃满/已有占用：不执行、不授 lease、
	// 不计 attempt；中枢回 pending(waiting_for_slot)。不能用 stale_config 伪装容量
	// 不一致；未知 reason fail-closed。
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
	// Node 是已登记 Node.metadata.name / anode.yaml.name，不是 DB 内部 UID。
	// 服务端用已认证 session 映射 DB id，不能用 body.node 选择另一个身份。
	Node string `json:"node"`
	// LeaseID 是本次授权的租约 id（renew/ack/result 都必须回带同一个）。
	LeaseID string `json:"leaseId"`
	// AttemptToken 是本次 attempt 的一次性高熵令牌（重试后旧 token 立即作废）。
	AttemptToken string `json:"attemptToken"`
	// ExpiresAt 是本授权到期的绝对时间（中枢时钟）。
	ExpiresAt time.Time `json:"expiresAt"`
	// AssignmentID / Slot 回带本次派发的 assignment 与节点在 accept 时预留的槽
	// （A3）。节点必须找到完全匹配的 pending reservation（taskId+assignmentId+
	// proposedAttempt+slot）才允许启动，不能收到任意 grant 就执行。
	AssignmentID string `json:"assignmentId"`
	Slot         *int   `json:"slot"`
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
	// Node 是已登记 Node.metadata.name / anode.yaml.name，不是 DB 内部 UID。
	// 服务端用已认证 session 映射 DB id，不能用 body.node 选择另一个身份。
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
	// LeaseID / Slot 是这次取消要定位的**历史绑定**（A3）。
	//
	// 取消必须按完整历史绑定（taskId+attempt+leaseId，slot 有 presence 时也要匹配）
	// 定位：同一逻辑 task 的新 attempt 不能被旧 attempt 的取消误停。
	LeaseID string `json:"leaseId"`
	Slot    *int   `json:"slot"`
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
	// LeaseID / Slot 回带本次回收的历史绑定（A3）：中枢按历史 task_attempts 的
	// node+taskId+attempt+leaseId+slot 核验并 CAS 释放旧 occupancy。错误 lease/slot
	// 的 ACK 不能清掉同 task 新 attempt 的槽。
	LeaseID string `json:"leaseId"`
	Slot    *int   `json:"slot"`
	// ProcessGroupReaped 报告进程组是否已经真正回收。
	ProcessGroupReaped bool `json:"processGroupReaped"`
}

// ValidSlot 报告有 presence 的槽号是否合法：非 nil 且 >= 0（0 是合法槽号）。
func ValidSlot(slot *int) bool {
	return slot != nil && *slot >= 0
}

// ValidateDispatch 校验 task.dispatch 的必填关联字段与派发窗口（A3）。
//
// 注意：它**不是** Validate()——执行帧保持 Decode 的宽松语义（未知字段忽略），
// presence/合法值由调用方显式调用这些具名校验函数。
func ValidateDispatch(f *TaskDispatchFrame) error {
	switch {
	case f == nil:
		return errField("dispatch", "帧为空")
	case f.Type != FrameTaskDispatch:
		return errField("dispatch", "type 不是 task.dispatch")
	case f.TaskID == "":
		return errField("dispatch", "缺少 taskId")
	case f.AssignmentID == "":
		return errField("dispatch", "缺少 assignmentId")
	case f.ProposedAttempt < 1:
		return errField("dispatch", "proposedAttempt 必须 ≥1")
	case f.Namespace == "":
		return errField("dispatch", "缺少 namespace")
	case f.ConfigRevision == "":
		return errField("dispatch", "缺少 configRevision")
	case f.AssignmentExpiresAt.IsZero():
		return errField("dispatch", "缺少 assignmentExpiresAt")
	}
	return nil
}

// ValidateAccept 校验 task.accept 的必填字段与 slot presence（0 合法）。
func ValidateAccept(f *TaskAcceptFrame) error {
	switch {
	case f == nil:
		return errField("accept", "帧为空")
	case f.TaskID == "":
		return errField("accept", "缺少 taskId")
	case f.AssignmentID == "":
		return errField("accept", "缺少 assignmentId")
	case f.ProposedAttempt < 1:
		return errField("accept", "proposedAttempt 必须 ≥1")
	case f.Node == "":
		return errField("accept", "缺少 node")
	case !ValidSlot(f.Slot):
		return errField("accept", "slot 必须有 presence 且 ≥0（0 合法）")
	}
	return nil
}

// ValidateGrant 校验 lease.grant 的必填字段、assignment 关联与 slot presence。
func ValidateGrant(f *LeaseGrantFrame) error {
	switch {
	case f == nil:
		return errField("grant", "帧为空")
	case f.TaskID == "":
		return errField("grant", "缺少 taskId")
	case f.Attempt < 1:
		return errField("grant", "attempt 必须 ≥1")
	case f.Node == "":
		return errField("grant", "缺少 node")
	case f.LeaseID == "":
		return errField("grant", "缺少 leaseId")
	case f.AttemptToken == "":
		return errField("grant", "缺少 attemptToken")
	case f.ExpiresAt.IsZero():
		return errField("grant", "缺少 expiresAt")
	case f.AssignmentID == "":
		return errField("grant", "缺少 assignmentId")
	case !ValidSlot(f.Slot):
		return errField("grant", "slot 必须有 presence 且 ≥0（0 合法）")
	}
	return nil
}

// ValidateCancel 校验 task.cancel 的历史绑定字段（leaseId 和 slot presence 必填）。
func ValidateCancel(f *TaskCancelFrame) error {
	switch {
	case f == nil:
		return errField("cancel", "帧为空")
	case f.TaskID == "":
		return errField("cancel", "缺少 taskId")
	case f.Attempt < 1:
		return errField("cancel", "attempt 必须 ≥1")
	case f.LeaseID == "":
		return errField("cancel", "缺少 leaseId（必须按历史绑定定位）")
	case !ValidSlot(f.Slot):
		return errField("cancel", "slot 必须有 presence 且 ≥0（0 合法）")
	}
	return nil
}

// ValidateCancelAck 校验 cancel.ack 的历史绑定字段。
func ValidateCancelAck(f *CancelAckFrame) error {
	switch {
	case f == nil:
		return errField("cancel.ack", "帧为空")
	case f.TaskID == "":
		return errField("cancel.ack", "缺少 taskId")
	case f.Attempt < 1:
		return errField("cancel.ack", "attempt 必须 ≥1")
	case f.LeaseID == "":
		return errField("cancel.ack", "缺少 leaseId")
	case !ValidSlot(f.Slot):
		return errField("cancel.ack", "slot 必须有 presence 且 ≥0（0 合法）")
	}
	return nil
}

func errField(frame, msg string) error {
	return &frameFieldError{frame: frame, msg: msg}
}

type frameFieldError struct {
	frame string
	msg   string
}

func (e *frameFieldError) Error() string {
	return fmt.Sprintf("protocol: %s: %s", e.frame, e.msg)
}
