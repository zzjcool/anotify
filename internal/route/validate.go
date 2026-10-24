package route

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// validateValue 用 JSON Schema 的一个**受控子集**校验值，返回不合法原因（"" = 通过）。
//
// 为什么只支持子集（不变量 I6：中枢不解释任何用户提供的逻辑）：
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
//
// path 用于生成可读的错误位置（params.to / params.attachments[0] 等）。
func validateValue(schema map[string]any, v any, path string) string {
	if len(schema) == 0 {
		return "" // 空 schema = 接受一切
	}

	// enum：最先判，语义最强
	if raw, ok := schema["enum"]; ok {
		if list, ok := raw.([]any); ok && len(list) > 0 {
			if !enumContains(list, v) {
				return fmt.Sprintf("%s: value %v not in enum %v", path, v, list)
			}
		}
	}

	// type
	if raw, ok := schema["type"]; ok {
		if msg := checkType(raw, v, path); msg != "" {
			return msg
		}
	}

	// 按值的实际类型做进一步校验（type 未声明时也尽力而为）
	switch val := v.(type) {
	case map[string]any:
		return validateObject(schema, val, path)
	case []any:
		return validateArray(schema, val, path)
	case string:
		return validateString(schema, val, path)
	case float64:
		return validateNumber(schema, val, path)
	case int:
		return validateNumber(schema, float64(val), path)
	case int64:
		return validateNumber(schema, float64(val), path)
	}
	return ""
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
		if typeMatches(w, v) {
			return ""
		}
	}
	return fmt.Sprintf("%s: want type %s, got %s", path, strings.Join(wanted, "|"), jsonTypeName(v))
}

// typeMatches 判定值是否属于某个 JSON Schema type。
//
// 注意 integer 在 Go 侧解码后仍是 float64，用「是否为整数」判定。
func typeMatches(want string, v any) bool {
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
		return isNumber(v)
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

func isNumber(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	}
	return false
}

// jsonTypeName 给人看的类型名。
func jsonTypeName(v any) string {
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

// validateObject 校验对象：required / properties / additionalProperties / minProperties。
func validateObject(schema map[string]any, obj map[string]any, path string) string {
	props, _ := schema["properties"].(map[string]any)

	// required
	if raw, ok := schema["required"]; ok {
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
	if msg := checkNumberBound(schema, "minProperties", float64(len(obj)), path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(schema, "maxProperties", float64(len(obj)), path, false); msg != "" {
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
				if msg := validateValue(sub, obj[name], path+"."+name); msg != "" {
					return msg
				}
			}
			continue
		}
		// 未声明的字段：additionalProperties=false 时拒绝
		if raw, ok := schema["additionalProperties"]; ok {
			if allowed, isBool := raw.(bool); isBool && !allowed {
				return fmt.Sprintf("%s: unexpected field %q (additionalProperties=false)", path, name)
			}
			if sub, isMap := raw.(map[string]any); isMap {
				if msg := validateValue(sub, obj[name], path+"."+name); msg != "" {
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

// validateArray 校验数组：items / prefixItems / minItems / maxItems。
func validateArray(schema map[string]any, arr []any, path string) string {
	if msg := checkNumberBound(schema, "minItems", float64(len(arr)), path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(schema, "maxItems", float64(len(arr)), path, false); msg != "" {
		return msg
	}

	// prefixItems（2020-12）：按位置分别校验
	var prefix []any
	if raw, ok := schema["prefixItems"]; ok {
		prefix, _ = raw.([]any)
	}
	// items：对剩余元素（或全部元素，若无 prefixItems）校验
	itemsSchema, hasItems := schema["items"].(map[string]any)

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
		if msg := validateValue(sub, e, fmt.Sprintf("%s[%d]", path, i)); msg != "" {
			return msg
		}
	}
	return ""
}

// validateString 校验字符串：minLength / maxLength / pattern。
func validateString(schema map[string]any, s string, path string) string {
	// 长度按 rune 计（JSON Schema 也是字符数，不是字节数）
	n := float64(len([]rune(s)))
	if msg := checkNumberBound(schema, "minLength", n, path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(schema, "maxLength", n, path, false); msg != "" {
		return msg
	}
	if raw, ok := schema["pattern"]; ok {
		if pat, ok := raw.(string); ok && pat != "" {
			if msg := matchPattern(pat, s, path); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// validateNumber 校验数字：minimum / maximum。
func validateNumber(schema map[string]any, n float64, path string) string {
	if msg := checkNumberBound(schema, "minimum", n, path, true); msg != "" {
		return msg
	}
	if msg := checkNumberBound(schema, "maximum", n, path, false); msg != "" {
		return msg
	}
	return ""
}

// checkNumberBound 检查 minXxx / maxXxx 一类的数值边界。
//
// isMin=true 检查下界（schema 键 lowerKey），否则上界。
func checkNumberBound(schema map[string]any, key string, actual float64, path string, isMin bool) string {
	raw, ok := schema[key]
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
// 这是「中枢可以安全执行用户提供的 pattern」的前提（不变量 I6 的边界内）。
// 编译失败视为 schema 本身有问题 → 拒绝派发（不能拿坏 pattern 静默放行）。
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
