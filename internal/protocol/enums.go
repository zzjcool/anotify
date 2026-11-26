package protocol

import (
	"fmt"
	"time"
)

// TaskPhase 是任务的相位（capability.md §8.3：小写，与 wire 一致）。
//
// 只有这六个值。没有 9 态、没有 "local"/"deployed" 之类：任务只有「在排队 / 在跑 /
// 终态」三种处境，终态再按原因分四类。
type TaskPhase string

const (
	TaskPhasePending   TaskPhase = "pending"
	TaskPhaseRunning   TaskPhase = "running"
	TaskPhaseSucceeded TaskPhase = "succeeded"
	TaskPhaseFailed    TaskPhase = "failed"
	TaskPhaseCancelled TaskPhase = "cancelled"
	TaskPhaseAbandoned TaskPhase = "abandoned"
)

// Valid 报告相位是否是封闭集合里的值。
func (p TaskPhase) Valid() bool {
	switch p {
	case TaskPhasePending, TaskPhaseRunning, TaskPhaseSucceeded,
		TaskPhaseFailed, TaskPhaseCancelled, TaskPhaseAbandoned:
		return true
	}
	return false
}

// Terminal 报告相位是否是终态（终态才起 taskTtlSec 计时）。
func (p TaskPhase) Terminal() bool {
	switch p {
	case TaskPhaseSucceeded, TaskPhaseFailed, TaskPhaseCancelled, TaskPhaseAbandoned:
		return true
	}
	return false
}

// WaitingReason 是 pending 的原因（capability.md §8.3）。
//
// 等待态不占槽位、不耗租约、不计 attempt，也不进 failed。
type WaitingReason string

const (
	// WaitingForSlot 节点槽位或能力并发预算吃满（capability.md §2.3）。
	WaitingForSlot WaitingReason = "waiting_for_slot"
	// WaitingRetryBackoff 失败后冷却再试（退避不占槽位、不耗租约）。
	WaitingRetryBackoff WaitingReason = "retry_backoff"
	// WaitingForConfig 机器级配置还没 applied（stale_config 回 pending，不计 attempt）。
	WaitingForConfig WaitingReason = "waiting_for_config"
)

// Valid 报告等待原因是否在封闭集合里。
func (w WaitingReason) Valid() bool {
	switch w {
	case WaitingForSlot, WaitingRetryBackoff, WaitingForConfig:
		return true
	}
	return false
}

// condition 四 type（capability.md §2.4）。读时派生，不落库。
const (
	ConditionImplemented        = "Implemented"
	ConditionNodesAvailable     = "NodesAvailable"
	ConditionDownstreamResolved = "DownstreamResolved"
	ConditionDeployed           = "Deployed"
)

// condition status 是字符串 "True" / "False"（不是 bool）。
const (
	ConditionStatusTrue  = "True"
	ConditionStatusFalse = "False"
)

// condition reason 封闭集合（capability.md §2.4 / api-conventions.md §4.3）。
const (
	ConditionReasonValid                 = "Valid"
	ConditionReasonInvalid               = "Invalid"
	ConditionReasonNoMatchingNode        = "NoMatchingNode"
	ConditionReasonCommandMissing        = "CommandMissing"
	ConditionReasonValueMissing          = "ValueMissing"
	ConditionReasonConfigPending         = "ConfigPending"
	ConditionReasonNodeOffline           = "NodeOffline"
	ConditionReasonNamespaceUnauthorized = "NamespaceUnauthorized"
	ConditionReasonNotFound              = "NotFound"
	ConditionReasonNotDeployed           = "NotDeployed"
	ConditionReasonStale                 = "Stale"
)

// conditionReasons 是「哪个 type 允许哪些 reason」的表。
//
// 用表而不是散落的 if：新增 reason 必须同时改这张表（capability.md §2.4：
// 「新增要改表，不能随手造」），前端与 API 也能直接拿它做校验。
var conditionReasons = map[string][]string{
	ConditionImplemented:        {ConditionReasonValid, ConditionReasonInvalid},
	ConditionNodesAvailable:     {ConditionReasonValid, ConditionReasonNoMatchingNode, ConditionReasonCommandMissing, ConditionReasonValueMissing, ConditionReasonConfigPending, ConditionReasonNodeOffline, ConditionReasonNamespaceUnauthorized},
	ConditionDownstreamResolved: {ConditionReasonValid, ConditionReasonNotFound},
	ConditionDeployed:           {ConditionReasonValid, ConditionReasonNotDeployed, ConditionReasonStale},
}

// IsConditionReason 报告 reason 是否是该 condition type 允许的值。
func IsConditionReason(conditionType, reason string) bool {
	for _, allowed := range conditionReasons[conditionType] {
		if allowed == reason {
			return true
		}
	}
	return false
}

// ConditionReasons 返回某个 type 允许的全部 reason（拷贝，调用方可安全修改）。
func ConditionReasons(conditionType string) []string {
	src := conditionReasons[conditionType]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// 任务失败 / 调度 / 派发原因码（capability.md §6.2 封闭集合）。
//
// 它们是任务结果与事件的 reason，也是节点侧可判别的失败分类。**不是** HTTP
// Status 的 reason 词表（虽然字面复用）：响应形状与判定时机不同，不要混用。
const (
	// emit 被拒（本地网关 / 中枢权威检查）。
	ReasonTargetNotDeclared = "target_not_declared"
	ReasonMissingTarget     = "missing_target"
	ReasonTokenInvalid      = "token_invalid"
	ReasonTaskNotRunning    = "task_not_running"
	ReasonEmitIDConflict    = "emit_id_conflict"
	// 结果未知（可能已派发）：只用于本地 emit 网关的 CLI 退出码 5，不是任务失败码。
	ReasonEmitUnreachable = "emit_unreachable"

	// 运行期：短暂基础设施失败（只有 idempotent: true 才允许自动重试）。
	ReasonNodeLost       = "node_lost"
	ReasonLeaseExpired   = "lease_expired"
	ReasonInfraTransient = "infra_transient"

	// 运行期：可判定失败（默认终态，不自动重试）。
	ReasonMaxAttemptsExceeded = "max_attempts_exceeded"
	// ReasonTimeout 是 lifecycle.timeoutSec 到点（capability.md §2.3：failed，不自动重跑）。
	//
	// 注意：capability.md §6.2 的表里没有单列这个码（只说「到点 → failed」），
	// 任务卡要求把它作为可判别原因保留，故在此显式定义（见 DONE 报告的契约说明）。
	ReasonTimeout            = "timeout"
	ReasonScriptFailed       = "script_failed"
	ReasonFilesEscape        = "files_escape"
	ReasonWorkDirUnavailable = "workdir_unavailable"
	ReasonHomeUnavailable    = "home_unavailable"
	ReasonMissingRequirement = "missing_requirement"
	// ReasonArtifactExportInvalid 是 A2 导出目录问题（创建/注入/枚举非法：符号链接、
	// 非普通文件、越界、超限）。它是确定性配置/文件边界错误：failed 且不自动重试，
	// **不是** infra 事实（不进 IsRetryableInfra）。
	ReasonArtifactExportInvalid = "artifact_export_invalid"

	// 用户动作。
	ReasonCancelledByUser = "cancelled_by_user"
	ReasonAbandonedByUser = "abandoned_by_user"

	// 调度（task.accept 之前的拒绝）：不计 attempt、不授 lease。
	ReasonStaleConfig = "stale_config"
	// ReasonSlotUnavailable 是 accept 前的容量拒绝（A3）：本机没有真实空槽 /
	// 预算吃满 / 已有占用。不计 attempt、不授 lease，中枢回 pending(waiting_for_slot)。
	ReasonSlotUnavailable = "slot_unavailable"

	// 派发期观察原因（剪枝，不是任务失败）。
	ReasonDownstreamMissing = "downstream_missing"
	ReasonParamMissing      = "param_missing"
	ReasonParamSchema       = "param_schema"
	ReasonParamType         = "param_type"
	ReasonNoTarget          = "no_target"
	ReasonEventSchema       = "event_schema"
	// ReasonCapabilityDisabled 是「这条边指向的能力被停用了」（capability-lifecycle §4.4）。
	// **剪枝**不是失败：事件照收 200，只是不建任务——回 4xx 会让调用方以为该重试，
	// 而停用是下游不接（与 downstream_missing / param_missing 同类）。
	ReasonCapabilityDisabled = "capability_disabled"
	// ReasonNoMatch 是「target 声明了 match.types，本次事件 type 不在里面」（#513 A1）。
	ReasonNoMatch = "no_match"

	ReasonMaxHops = "max_hops"
	ReasonBudget  = "budget"

	// 决策通道（#522）：HTTP 结论码 + `source='route'` 观察行 type。
	ReasonOutcomeNotDeclared   = "outcome_not_declared"   // 结局名不在声明表（403 + 审计行）
	ReasonDecisionAlreadyTaken = "decision_already_taken" // 同 task 第二次决策（409）
	ReasonNoDecision           = "no_decision"            // router 成功但没提交决策
	ReasonRouterFailed         = "router_failed"          // router 失败/放弃：不执行结局
	ReasonOutcomeDropped       = "outcome_dropped"        // 选中空动作结局（drop: []）
)

// pruneReasons 是「事件已接收、下游没建 task」的观察原因（capability.md §8.3）。
//
// 它们进 status.observed.pruned[].reason 与事件，**不是** 4xx：
// no_target 只表示源的 targets 为空；emit 缺 to 是 missing_target（400）。
var pruneReasons = []string{
	ReasonNoTarget,
	ReasonEventSchema,
	ReasonParamSchema,
	ReasonParamType,
	ReasonParamMissing,
	ReasonCapabilityDisabled,
	ReasonNoMatch,
	ReasonMaxHops,
	ReasonBudget,
}

// 决策通道观察行（#522）：不在 pruneReasons。
var routeObservationReasons = []string{
	ReasonNoDecision,
	ReasonRouterFailed,
	ReasonOutcomeDropped,
	ReasonOutcomeNotDeclared,
}

func IsRouteObservationReason(reason string) bool {
	return containsString(routeObservationReasons, reason)
}

func RouteObservationReasons() []string {
	out := make([]string, len(routeObservationReasons))
	copy(out, routeObservationReasons)
	return out
}

// IsPruneReason 报告 reason 是不是剪枝观察原因。
func IsPruneReason(reason string) bool {
	return containsString(pruneReasons, reason)
}

// PruneReasons 返回全部剪枝观察原因（拷贝）。
func PruneReasons() []string {
	out := make([]string, len(pruneReasons))
	copy(out, pruneReasons)
	return out
}

// retryableInfraReasons 是「idempotent: true 时才允许自动重试」的短暂基础设施失败。
//
// 只有运行器结构化判定的事实进得来：脚本退出码（包括 anode emit 的 exit 5）不是
// infra 事实（capability.md §2.3）。
var retryableInfraReasons = []string{
	ReasonNodeLost,
	ReasonLeaseExpired,
	ReasonInfraTransient,
}

// IsRetryableInfra 报告一个失败原因是否属于可自动重试的短暂基础设施失败。
func IsRetryableInfra(reason string) bool {
	return containsString(retryableInfraReasons, reason)
}

// TaskRejectReasons 是 task.reject 允许的 reason 集合（A3 扩为两项）。
//
// 只有 stale_config 与 slot_unavailable：缺命令/目录/导出错误是 lease.grant 之后
// 启动前的确定性失败，走 task.result（计 attempt）；未知 reason fail-closed。
var taskRejectReasons = []string{ReasonStaleConfig, ReasonSlotUnavailable}

// IsTaskRejectReason 报告 reason 是否是 task.reject 的合法理由。
// TaskRejectReasons 返回允许的 reject reason 快照（顺序固定）。
func TaskRejectReasons() []string { return append([]string(nil), taskRejectReasons...) }

func IsTaskRejectReason(reason string) bool {
	return containsString(taskRejectReasons, reason)
}

// CancelReason 是 task.cancel 的 reason（capability.md §8.5）。
//
// 三类：用户取消、用户放弃、基础设施放弃。节点对三者的动作一样（TERM → 5s →
// KILL 进程组），区别只在任务终态与谁该被通知。
type CancelReason string

const (
	CancelReasonUser            CancelReason = "cancelled_by_user"
	CancelReasonAbandonedByUser CancelReason = "abandoned_by_user"
	// CancelReasonInfra 是中枢侧的放弃（节点失联 / 租约丢失 / 重试顶替旧 attempt）。
	CancelReasonInfra CancelReason = "abandoned"
)

// Valid 报告取消原因是否在封闭集合里。
func (c CancelReason) Valid() bool {
	switch c {
	case CancelReasonUser, CancelReasonAbandonedByUser, CancelReasonInfra:
		return true
	}
	return false
}

// 租约参数（capability.md §2.3「全目录一致」）。
//
// 基础租约 60s、续租间隔 20s、TERM 宽限 5s 后 KILL 整个进程组。三个数必须来自
// 同一处，否则节点与中枢各算各的 deadline，症状是「任务莫名其妙被 KILL」。
const (
	// LeaseTTL 是租约时长的**协议下限**（历史默认值也是它）。
	// 中枢可以用 ANOTIFY_LEASE_TTL 调大，但不得短于它：一次隧道重建（502 /
	// TLS 握手重置）实测可达 40–90s，60s 的租约撞上这种抖动就直接判死整个
	// attempt（#661 现场：10–37 分钟的 implement 一步被 60s 抖动整单作废）。
	LeaseTTL = 60 * time.Second
	// DefaultLeaseTTL 是中枢未显式配置时授予的租约时长。
	//
	// 为什么是 180s 而不是协议下限 60s：中枢侧的租约是「节点最后一次续租 + TTL」，
	// 而 60s 只够 3 个续租周期，任何一次超过 60s 的断连都会让中枢判失联并把任务
	// 推向终态。180s（= 9 个续租周期）覆盖了实测的隧道重建窗口，代价只是节点失联
	// 到被回收最多晚 120s（回收的旧路径语义不变，见 store.RecoverLostLeases）。
	DefaultLeaseTTL = 180 * time.Second
	// LeaseRenewInterval 是节点续租间隔（默认租约的九分之一）。
	LeaseRenewInterval = 20 * time.Second
	// LeaseTermGrace 是 TERM 之后到 KILL 进程组的宽限期。
	LeaseTermGrace = 5 * time.Second
	// LeaseRenewRatio 是「租约时长至少几个续租间隔」。
	//
	// 少于 3 个就没有任何抗抖动余量（一次丢两个 ACK 就到期），所以配置检查把它当硬线。
	LeaseRenewRatio = 3
)

// ValidateLeaseTTL 校验 (ttl, renew) 的组合是否自洽：非法配置必须**启动即拒**，
// 不能在运行期才表现为「任务莫名其妙被判失联」。
//
// 两条判据都是不同的事故形态，别合并：
//   - ttl < LeaseTTL：一次隧道重建就够判死（本可以避免的整单作废）；
//   - ttl < LeaseRenewRatio*renew：续租间隔相对租约太长，丢一两个 ACK 就到期。
func ValidateLeaseTTL(ttl, renew time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("租约时长必须为正数（当前 %v）", ttl)
	}
	if ttl < LeaseTTL {
		return fmt.Errorf("租约时长 %v 低于协议下限 %v", ttl, LeaseTTL)
	}
	if renew <= 0 {
		return fmt.Errorf("续租间隔必须为正数（当前 %v）", renew)
	}
	if min := time.Duration(LeaseRenewRatio) * renew; ttl < min {
		return fmt.Errorf("租约时长 %v 不能小于 %d×续租间隔（%v）：抖动余量不足",
			ttl, LeaseRenewRatio, min)
	}
	return nil
}

// 退避参数（D-I11，写常量集中一处）。
const (
	// RetryBackoffBase 是 attempt 之间的首次退避。
	RetryBackoffBase = 5 * time.Second
	// RetryBackoffMax 是 attempt 之间退避的上限。
	RetryBackoffMax = 5 * time.Minute
	// RetryBackoffFactor 是指数退避的倍数。
	RetryBackoffFactor = 2
	// StaleConfigBackoffBase 是 stale_config 回 pending 的首次退避。
	StaleConfigBackoffBase = 5 * time.Second
	// StaleConfigBackoffMax 是 stale_config 退避上限（不能热循环）。
	StaleConfigBackoffMax = 60 * time.Second
)

// containsString 是小的封闭集合查找（列表都很短，线性扫比建 map 更直白）。
func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
