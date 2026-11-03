package route

import (
	"encoding/json"

	"github.com/zzjcool/anotify/internal/schema"
)

// ValidateEventPayload 用触发器的 result_schema 校验一次事件的 payload（设计 §4.2）。
//
// 它是入口校验的唯一实现，**复用**参数校验的同一个受控子集引擎（internal/schema）：
// 同一个 payload 在入口（事件对源的承诺）与在边上（参数对能力的承诺）必须用
// 同一套语义，否则会出现「入口说过、派发又剪」这种无法自圆的判定。
//
// 与 validateParams 只差两点：路径前缀是 payload（错误里直接看得见是哪个字段、
// 期望什么、实得什么），以及 schema 不合法时的消息是针对 result_schema 的。
//
// 返回 "" = 通过。
func ValidateEventPayload(raw json.RawMessage, payload map[string]any) string {
	if len(raw) == 0 {
		return ""
	}
	root, err := schema.FromRaw(raw)
	if err != nil {
		// schema 本身坏了：拒绝派发（不能拿坏契约放行任何事件）。
		// 与 validateParams 同一口径：schema 语法错误不该静默当"接受一切"。
		return "invalid result_schema in source config"
	}
	if len(root) == 0 {
		return "" // {} = 不约束任何字段（等价于没声明）
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return schema.Value(root, payload, "payload")
}

// validateValue 用 JSON Schema 的受控子集校验值，返回不合法原因（"" = 通过）。
//
// 实现整体在 internal/schema（零依赖，节点侧也要用同一份）。这里保留薄包装，
// 是为了让 route 内部的调用点与错误文案不受包边界影响。
func validateValue(sch map[string]any, v any, path string) string {
	return schema.Value(sch, v, path)
}

// typeMatches 判定值是否属于某个 JSON Schema type（配置期与派发期同一份语义）。
func typeMatches(want string, v any) bool { return schema.TypeMatches(want, v) }

// jsonTypeName 给人看的类型名（错误消息用）；与 typeMatches 同一来源。
func jsonTypeName(v any) string { return schema.TypeName(v) }

// lookupProp 在 properties 里找字段模式（键名大小写敏感，符合 JSON Schema）。
//
// schema 包内部有同一份实现；这里保留一个包装是因为 params.go 的模板校验要在
// 「已声明的键 / 未声明的键」之间分流，而 schema 包不导出这个内部细节。
func lookupProp(props map[string]any, name string) (any, bool) {
	if props == nil {
		return nil, false
	}
	v, ok := props[name]
	return v, ok
}
