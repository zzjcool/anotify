// Package schema 是 Anotify 唯一的 JSON Schema 校验实现。
//
// 为什么单独一个包（而不是留在 route / store 里）：
//
//	同一套语义要在**三个地方**跑，且必须有完全一致的结果——
//	  1. 配置期：store.CreateEdge 校验边上的字面量覆盖（键在不在、类型对不对）
//	  2. 入口：   route/source 校验事件 payload 是否满足触发器的 result_schema
//	  3. 投递期： 边缘节点（cmd/agent）校验一次 emit 的 data 是否满足该端口的 expects
//
// 第 3 条决定了它不能依赖 store（store 会把 sqlite 链进节点二进制）。所以这里
// **零依赖**：只用标准库。route 与 store 都转发到这里，语义只有一份。
//
// 为什么只支持受控子集（不变量 I6：中枢不解释任何用户提供的逻辑）：
// 完整 JSON Schema 包含 $ref / if-then-else / patternProperties / 远程 $ref 等
// 需要「求值」的特性，会把中枢变成一个可以无限扩展的攻击面。这里只支持
// **声明式、可一次扫描判定**的关键字，遇到不认识的**一律忽略**（不报错）。
//
// 支持的子集：
//
//	type                     object | array | string | number | integer | boolean | null
//	                        （也支持 type 为数组，如 ["string","null"]）
//	properties               对象的字段模式
//	required                 必填字段名数组
//	additionalProperties     false 时禁止未声明字段
//	items / prefixItems      数组元素模式
//	minItems / maxItems      数组长度
//	enum                     取值必须在此列表内
//	minLength / maxLength    字符串长度
//	pattern                  字符串正则（RE2，编译失败视为 schema 有问题）
//	minimum / maximum        数值范围
//	minProperties/maxProperties 对象字段数
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// FromRaw 解析一份原始 schema。
//
// 三种结果：nil schema + nil error（没声明 = 不校验）、map + nil、nil + error（坏了）。
// 坏 schema 必须由调用方明确处理——「拿坏契约放行」是最坏的降级方式。
func FromRaw(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return root, nil
}

// Value 用 JSON Schema 的受控子集校验值，返回不合法原因（"" = 通过）。
//
// path 用于生成可读的错误位置（params.to / data.attachments[0] / payload.diff 等）。
// 空 schema（len==0）接受一切——「没声明约束」和「约束为空」在这里是同一件事。
func Value(sch map[string]any, v any, path string) string {
	if len(sch) == 0 {
		return "" // 空 schema = 接受一切
	}

	// enum：最先判，语义最强
	if raw, ok := sch["enum"]; ok {
		if list, ok := raw.([]any); ok && len(list) > 0 {
			if !enumContains(list, v) {
				return fmt.Sprintf("%s: value %v not in enum %v", path, v, list)
			}
		}
	}

	// type
	if raw, ok := sch["type"]; ok {
		if msg := checkType(raw, v, path); msg != "" {
			return msg
		}
	}

	// 按值的实际类型做进一步校验（type 未声明时也尽力而为）
	switch val := v.(type) {
	case map[string]any:
		return valueObject(sch, val, path)
	case []any:
		return valueArray(sch, val, path)
	case string:
		return valueString(sch, val, path)
	case float64:
		return valueNumber(sch, val, path)
	case int:
		return valueNumber(sch, float64(val), path)
	case int64:
		return valueNumber(sch, float64(val), path)
	}
	return ""
}

// Required 返回 schema 里声明的必填字段名（已排序，便于稳定输出与测试）。
func Required(sch map[string]any) []string {
	raw, ok := sch["required"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Properties 返回 schema 里声明的字段模式（可能为 nil）。
func Properties(sch map[string]any) map[string]any {
	props, _ := sch["properties"].(map[string]any)
	return props
}

// checkType 校验 type 关键字（字符串或字符串数组）。
func checkType(raw any, v any, path string) string {
	var wanted []string
	switch t := raw.(type) {
	case string:
		wanted = []string{t}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				wanted = append(wanted, s)
			}
		}
	default:
		return "" // 非法的 type 声明：忽略（schema 语法错误不该 block 业务）
	}
	if len(wanted) == 0 {
		return ""
	}
	for _, w := range wanted {
		if TypeMatches(w, v) {
			return ""
		}
	}
	return fmt.Sprintf("%s: want type %s, got %s", path, strings.Join(wanted, "|"), TypeName(v))
}

// TypeMatches 判定值是否属于某个 JSON Schema type。
//
// 这是**全仓唯一**的类型判定：配置期（边字面量）、入口（事件 payload）、
// 派发期（参数）、投递期（emit 的 data）都走它。四处的口径必须一致，
// 否则会出现「存的时候说类型对、派发时被剪枝」这种无法自圆的判定。
func TypeMatches(want string, v any) bool {
	switch want {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		return IsNumber(v)
	case "integer":
		switch n := v.(type) {
		case int, int64:
			return true
		case float64:
			return n == math.Trunc(n)
		}
		return false
	}
	return false
}

// IsNumber 判定是否为 JSON 解码出的数字（float64）或整数。
func IsNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	}
	return false
}

// TypeName 给人看的类型名（错误消息用）；与 TypeMatches 同一来源。
func TypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", t)
	}
}

// valueObject 校验对象：required / properties / additionalProperties / minProperties。
func valueObject(sch map[string]any, obj map[string]any, path string) string {
	props, _ := sch["properties"].(map[string]any)

	// required
	if raw, ok := sch["required"]; ok {
		if list, ok := raw.([]any); ok {
			for _, e := range list {
				name, ok := e.(string)
				if !ok {
					continue
				}
				if _, present := obj[name]; !present {
					return fmt.Sprintf("%s: missing required field %q", path, name)
				}
			}
		}
	}

	// minProperties / maxProperties
	if msg := checkNumberBound(sch, "minProperties", float64(len(obj)), path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(sch, "maxProperties", float64(len(obj)), path, false); msg != "" {
		return msg
	}

	// 字段逐个校验
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names) // 稳定的错误顺序（同一个坏输入总报同一个错）
	for _, name := range names {
		child, declared := lookupProp(props, name)
		if declared {
			if sub, ok := child.(map[string]any); ok {
				if msg := Value(sub, obj[name], path+"."+name); msg != "" {
					return msg
				}
			}
			continue
		}
		// 未声明的字段：additionalProperties=false 时拒绝
		if raw, ok := sch["additionalProperties"]; ok {
			if allowed, isBool := raw.(bool); isBool && !allowed {
				return fmt.Sprintf("%s: unexpected field %q (additionalProperties=false)", path, name)
			}
			if sub, isMap := raw.(map[string]any); isMap {
				if msg := Value(sub, obj[name], path+"."+name); msg != "" {
					return msg
				}
			}
		}
	}
	return ""
}

// lookupProp 在 properties 里找字段模式（键名大小写敏感，符合 JSON Schema）。
func lookupProp(props map[string]any, name string) (any, bool) {
	if props == nil {
		return nil, false
	}
	v, ok := props[name]
	return v, ok
}

// valueArray 校验数组：items / prefixItems / minItems / maxItems。
func valueArray(sch map[string]any, arr []any, path string) string {
	if msg := checkNumberBound(sch, "minItems", float64(len(arr)), path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(sch, "maxItems", float64(len(arr)), path, false); msg != "" {
		return msg
	}

	// prefixItems（2020-12）：按位置分别校验
	var prefix []any
	if raw, ok := sch["prefixItems"]; ok {
		prefix, _ = raw.([]any)
	}
	// items：对剩余元素（或全部元素，若无 prefixItems）校验
	itemsSchema, hasItems := sch["items"].(map[string]any)

	for i, e := range arr {
		var sub map[string]any
		switch {
		case i < len(prefix):
			sub, _ = prefix[i].(map[string]any)
		case hasItems:
			sub = itemsSchema
		}
		if sub == nil {
			continue
		}
		if msg := Value(sub, e, fmt.Sprintf("%s[%d]", path, i)); msg != "" {
			return msg
		}
	}
	return ""
}

// valueString 校验字符串：minLength / maxLength / pattern。
func valueString(sch map[string]any, s string, path string) string {
	// 长度按 rune 计（JSON Schema 也是字符数，不是字节数）
	n := float64(len([]rune(s)))
	if msg := checkNumberBound(sch, "minLength", n, path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(sch, "maxLength", n, path, false); msg != "" {
		return msg
	}
	if raw, ok := sch["pattern"]; ok {
		if pat, ok := raw.(string); ok && pat != "" {
			if msg := matchPattern(pat, s, path); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// valueNumber 校验数字：minimum / maximum。
func valueNumber(sch map[string]any, n float64, path string) string {
	if msg := checkNumberBound(sch, "minimum", n, path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(sch, "maximum", n, path, false); msg != "" {
		return msg
	}
	return ""
}

// checkNumberBound 检查 minXxx / maxXxx 一类的数值边界。
//
// isMin=true 检查下界（schema 键 lowerKey），否则上界。
func checkNumberBound(sch map[string]any, key string, actual float64, path string, isMin bool) string {
	raw, ok := sch[key]
	if !ok {
		return ""
	}
	bound, ok := toFloat(raw)
	if !ok {
		return "" // 非法声明：忽略
	}
	if isMin && actual < bound {
		return fmt.Sprintf("%s: %v < %s %v", path, actual, key, bound)
	}
	if !isMin && actual > bound {
		return fmt.Sprintf("%s: %v > %s %v", path, actual, key, bound)
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// enumContains 判定值是否在枚举列表里（深比较 JSON 语义）。
func enumContains(list []any, want any) bool {
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return false
	}
	for _, e := range list {
		gotJSON, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if string(gotJSON) == string(wantJSON) {
			return true
		}
	}
	return false
}

// matchPattern 用 RE2 正则校验字符串。
//
// 用 Go 的 regexp（RE2）而非 PCRE：RE2 保证线性时间，不会有灾难性回溯，
// 这是「可以安全执行用户提供的 pattern」的前提（不变量 I6 的边界内）。
// 编译失败视为 schema 本身有问题 → 拒绝（不能拿坏 pattern 静默放行）。
func matchPattern(pattern, s, path string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("%s: invalid pattern %q in params_schema: %v", path, pattern, err)
	}
	if !re.MatchString(s) {
		return fmt.Sprintf("%s: %q does not match pattern %q", path, s, pattern)
	}
	return ""
}
