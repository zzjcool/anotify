// 设备投递路由判定。
//
// 设计决策（必须严格实现）：
//
//	投递给设备 ⟺ 设备 enabled AND scopeMatch(device.event_scope, agentState) AND tagMatch
//
// scopeMatch 规则：
//   - eventScope=all → 全部通过（收全生命周期）
//   - eventScope=final → 仅终态通过（done/interrupted/error）
//
// tagMatch 规则：
//   - 消息无 deviceTags → 广播到所有 enabled 设备
//   - 设备无 tags → 接收一切（catch-all）
//   - 双方都有 tags → 求交集，≥1 个共同 tag 才投递（ANY，不是 ALL）
package push

import (
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// 终态 agentState（与契约 §5 一致）。
//
// 以前借 broker.IsTerminal，但 broker 是旧消息总线（#80 删除）。
// 设备过滤只需要"哪些状态算终态"这一条规则，不该为此依赖整个消息模型。
const (
	AgentStateDone        = "done"
	AgentStateInterrupted = "interrupted"
	AgentStateError       = "error"
)

// IsTerminalAgentState 判定 agentState 是否为终态。
func IsTerminalAgentState(state string) bool {
	return state == AgentStateDone || state == AgentStateInterrupted || state == AgentStateError
}

// ScopeMatch 判定设备的订阅范围是否放行该消息的 agentState。
//   - filter=all（或空）→ 全部通过（收全生命周期事件）
//   - filter=final → 仅终态通过（done/interrupted/error）
func ScopeMatch(filter, agentState string) bool {
	switch filter {
	case "", "all":
		return true
	case "final":
		return IsTerminalAgentState(agentState)
	default:
		return false
	}
}

// TagMatch 判定设备标签与消息路由标签是否匹配。
func TagMatch(deviceTags, msgTags []string) bool {
	// 消息无 deviceTags → 广播（匹配所有设备）
	if len(msgTags) == 0 {
		return true
	}
	// 设备无 tags → catch-all，接收一切
	if len(deviceTags) == 0 {
		return true
	}
	// 双方都有 tags → 求交集（ANY），大小写不敏感
	set := make(map[string]struct{}, len(deviceTags))
	for _, t := range deviceTags {
		set[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	for _, t := range msgTags {
		if _, ok := set[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// Deliverable 是"要投递的东西"的最小描述。
//
// 只有这两个字段参与设备过滤；以前的 broker.Message 还带 ID/Seq/Title
// 等消息实体字段，但过滤从来看不到它们。收窄成最小结构让 push 不再
// 依赖旧消息模型（#80）。
type Deliverable struct {
	AgentState string
	DeviceTags []string
}

// ShouldDeliver 综合判定一条投递是否应发到一台设备。
func ShouldDeliver(dev *store.Device, d Deliverable) bool {
	if !dev.Enabled {
		return false
	}
	if !ScopeMatch(dev.EventScope, d.AgentState) {
		return false
	}
	return TagMatch(dev.Tags, d.DeviceTags)
}

// FilterDevices 返回候选设备中应接收该投递的设备子集。
func FilterDevices(devices []*store.Device, d Deliverable) []*store.Device {
	out := []*store.Device{}
	for _, dev := range devices {
		if ShouldDeliver(dev, d) {
			out = append(out, dev)
		}
	}
	return out
}
