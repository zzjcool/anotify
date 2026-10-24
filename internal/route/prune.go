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
	// ReasonForeignUser params.to 指向的用户不属于本组织（契约 §10.1.1）。
	ReasonForeignUser PruneReason = "foreign_user"
	// ReasonPath 目标能力已在 path 里（环剪枝）。
	ReasonPath PruneReason = "path"
)

// PrunedRecord 是一条剪枝记录：静默（不打断业务），但**一律留痕**。
//
// Detail 是给人看的证据：比如 no_edge 时是出口名与上游能力，
// param_schema 时是具体哪个字段不合法，path 时是重复的那个能力。
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
