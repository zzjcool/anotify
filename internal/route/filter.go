// Package route 实现通知到设备的投递路由判定。
//
// 设计决策（来自技术方案讨论，必须严格实现）：
//
//	消息投递给设备 ⟺ 设备 enabled AND statusMatch(device.status_filter, msg.status) AND tagMatch
//
// tagMatch 规则：
//   - 消息无 deviceTags → 广播到所有 enabled 设备
//   - 设备无 tags → 接收一切（catch-all）
//   - 双方都有 tags → 求交集，≥1 个共同 tag 才投递（ANY，不是 ALL）
package route

import (
	"strings"

	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/store"
)

// StatusMatch 判定设备的状态过滤是否放行该消息状态。
//   - filter=all → 全部通过
//   - filter=error → 仅 msg.Status=error
//   - filter=success → 仅 msg.Status=success
//   - interrupted/info/warning 仅在 filter=all 时通过
func StatusMatch(filter, msgStatus string) bool {
	switch filter {
	case "", "all":
		return true
	case "error":
		return msgStatus == broker.StatusError
	case "success":
		return msgStatus == broker.StatusSuccess
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

// ShouldDeliver 综合判定一条消息是否应投递到一台设备。
func ShouldDeliver(dev *store.Device, msg *broker.Message) bool {
	if !dev.Enabled {
		return false
	}
	if !StatusMatch(dev.StatusFilter, msg.Status) {
		return false
	}
	return TagMatch(dev.Tags, msg.DeviceTags)
}

// FilterDevices 返回候选设备中应接收该消息的设备子集。
func FilterDevices(devices []*store.Device, msg *broker.Message) []*store.Device {
	var out []*store.Device
	for _, dev := range devices {
		if ShouldDeliver(dev, msg) {
			out = append(out, dev)
		}
	}
	return out
}
