package route

import "fmt"

// ---------- 剪枝原因（契约 §7） ----------

// PruneReason 是「这条边没触发」的机器可读原因。
//
// 注意：**上游没回报出口不是剪枝**——那是正常结束的分支，压根不查表，
// 不产生 PrunedRecord（契约 §7 表格第 ① 行）。
type PruneReason string

const (
	// ReasonNoEdge 查 edges 表 0 行。
	ReasonNoEdge PruneReason = "no_edge"
	// ReasonParamSchema 参数不过目标能力的 params_schema。
	ReasonParamSchema PruneReason = "param_schema"
	// ReasonNoNode 有边、参数也对，但没有节点声明该能力。
	ReasonNoNode PruneReason = "no_node"
	// ReasonForeignUser params.to 指向的用户在本部署里不存在（契约 §10.1.1）。
	// 常量名与 wire 值保留历史拼写（跨版本事件可读），语义已随迁移 14 收窄：
	// 组织层删除后不再有「外组织用户」，只剩「查无此人」。
	ReasonForeignUser PruneReason = "foreign_user"
	// ReasonMaxHops 跳数预算用尽（契约 §2）：这条链已经走到 max_hops 深度，不再继续。
	//
	// 它**取代**了旧的 path 剪枝（"目标已在路径里"）：环是合法编排，靠深度兜底；
	// Detail 里带完整 path，所以「它是怎么走到这么深的」仍然可查。
	ReasonMaxHops PruneReason = "max_hops"
	// ReasonBudget 同一 trace 的任务预算用尽（契约 §2）：已建任务数 >= 预算。
	//
	// 它防的是「配置/实现写错导致的无限扇出」——一个环不会无限跑，
	// 但一条每秒 emit 的链会把 trace 撑爆，而这与 hop 深度无关。
	ReasonBudget PruneReason = "budget"
	// ReasonParamMissing 参数引用的路径在 payload/别名表里取不到，且没有 default。
	ReasonParamMissing PruneReason = "param_missing"
	// ReasonParamType 参数取值结果转不成目标 params_schema 要求的类型。
	ReasonParamType PruneReason = "param_type"
	// ReasonEventSchema 事件 payload 不过源的 resultSchema（设计 §4.2 / §7 P0.2）。
	//
	// 与 ReasonParamSchema 的区别是**层级**不同：param_schema 是「这条边的参数
	// 不对」——逐 link 判、扇出里其他 link 照常派发；event_schema 是「这一整个
	// 事件不合契约」——一条 link 都不判，事件照常落库、一条都不派发。
	//
	// 产生点在**入口**（source.Trigger.Fire 调 Dispatch 之前），不在派发四步里：
	// 它判的是外部世界给的东西，不是中枢自己的配置。
	ReasonEventSchema PruneReason = "event_schema"
)

// PrunedRecord 是一条剪枝记录：静默（不打断业务），但**一律留痕**。
//
// Detail 是给人看的证据：比如 no_edge 时是出口名与上游能力，
// param_schema 时是具体哪个字段不合法，max_hops 时是完整 path，
// budget 时是「已经建了多少个任务 / 预算是多少」。
type PrunedRecord struct {
	Reason       PruneReason `json:"reason"`
	CapabilityID string      `json:"capabilityId,omitempty"`
	Port         string      `json:"port,omitempty"`
	Detail       string      `json:"detail"`
}

// String 便于日志与测试输出。
func (p PrunedRecord) String() string {
	return fmt.Sprintf("%s(%s): %s", p.Reason, p.CapabilityID, p.Detail)
}

// ---------- 剪枝留痕（#142） ----------

// PruneEventSource 是剪枝事件在 events 表里的 source 值
// （与 triggers 的 source 区分开：后者是 github/cron/… 这类来源名）。
//
// 为什么复用 events 表而不是新表：剪枝就是「这条链在这里断了」的观察记录，
// 与事件同属一条 trace、同一套命名空间维度，没有独立生命周期；
// 建新表还要新迁移、新出口、新前端，换来的只是同一件事的第二种存法。
const PruneEventSource = "route"

// PruneEventType 把原因映射成事件类型：pruned.<reason>（no_edge → pruned.no_edge）。
//
// 事件流里按 type 就能直接筛出某一类剪枝（`pruned.no_node` 拉出来就是
// 「所有因为没节点而没跑的分支」），不需要再解析 payload。
func PruneEventType(r PruneReason) string {
	return "pruned." + string(r)
}

// PruneEventPayload 是 pruned.<reason> 事件的 payload。
//
// 字段名 camelCase（仓库约定）；空值省略，detail 必填——它要能回答
// 「凭什么说这条边没走」（比如 param_schema 时是哪个字段不合法）。
//
// 这个结构必须能由 (PrunedRecord, DispatchInput) 唯一确定：
// 事件是**派发当场**留的痕，事后不会再有输入能补出它。
type PruneEventPayload struct {
	// FromCapability 是上游标识：普通能力边时是能力 id，入口边时是来源名（github/cron/…）。
	FromCapability   string `json:"fromCapability,omitempty"`
	Port             string `json:"port,omitempty"`             // 上游回报的出口名
	TargetCapability string `json:"targetCapability,omitempty"` // 被剪枝的目标能力 id；no_edge 时为空
	Detail           string `json:"detail"`                     // 给人看的证据
}
