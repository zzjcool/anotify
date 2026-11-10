package playbook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/schema"
)

// MaskedValue 是 writeOnly 字段进回执时的占位（§1.2 / D16）。
const MaskedValue = "***"

// schemaOf 解析包的 `spec.values`（JSON Schema）。Parse 已经保证它是对象，所以这里
// 失败只可能是「Package 被手工构造错了」，报 invalid_playbook 而不是 panic。
func (p *Package) schemaOf() (map[string]any, error) {
	if p == nil {
		return nil, errf(CodeInvalidPlaybook, "包为空")
	}
	raw := p.Values
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errf(CodeInvalidPlaybook, "spec.values 不是合法 JSON Schema: %v", err)
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

// ValuesDigest 返回有效 values 的 canonical sha256（`sha256:<64 hex>`）。
//
// 两条约定（§4.5 / D16）：
//   - **对打码前**的有效 values 算：这样"同一组值"仍然可比较（回执里的 `***` 不参与身份）；
//   - canonical：encoding/json 对 map 键排序、对数字用最短表示，所以排版/键序无关。
func ValuesDigest(values map[string]any) (string, error) {
	if values == nil {
		values = map[string]any{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("playbook: 序列化 values: %w", err)
	}
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// MaskValues 返回回执用的 values：`writeOnly: true` 的字段换成 `"***"`（§1.2 / D16）。
//
// 打码跟着 schema 走（不是猜字段名）：对象按 `properties` 递归、数组按 `items` 递归。
// 打码只影响回执，不影响渲染与 digest。
func MaskValues(sch map[string]any, values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	masked, _ := maskValue(sch, values).(map[string]any)
	if masked == nil {
		return map[string]any{}
	}
	return masked
}

// maskValue 递归打码一个值。
func maskValue(sch map[string]any, v any) any {
	if isWriteOnly(sch) {
		return MaskedValue
	}
	switch val := v.(type) {
	case map[string]any:
		props := schema.Properties(sch)
		out := make(map[string]any, len(val))
		for k, child := range val {
			childSchema, ok := props[k].(map[string]any)
			if !ok {
				out[k] = child
				continue
			}
			out[k] = maskValue(childSchema, child)
		}
		return out
	case []any:
		itemSchema, _ := sch["items"].(map[string]any)
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = maskValue(itemSchema, child)
		}
		return out
	default:
		return v
	}
}

// isWriteOnly 报告 schema 节点是否声明了 `writeOnly: true`（合法 JSON Schema 关键字）。
func isWriteOnly(sch map[string]any) bool {
	if sch == nil {
		return false
	}
	flag, ok := sch["writeOnly"].(bool)
	return ok && flag
}

// effectiveValues 是渲染第 ①②步：填默认值 → 校验 values（§2 的三步里的前两步）。
//
// 顺序不能换：`required` 判的是**有效 values**（= 传入 + default），先校验会把
// "有默认值但没传"的键误报成 values_missing。
func effectiveValues(sch map[string]any, in map[string]any) (map[string]any, error) {
	out := deepCopyMap(in)
	if out == nil {
		out = map[string]any{}
	}
	fillDefaults(sch, out)
	if err := checkRequired(sch, out, "values"); err != nil {
		return nil, err
	}
	if msg := schema.Value(sch, out, "values"); msg != "" {
		return nil, errf(CodeValuesInvalid, "%s（检查 spec.values 的 type / pattern / enum / additionalProperties）", msg)
	}
	return out, nil
}

// fillDefaults 按 schema 的 `properties` 递归补 `default`（§1.2）。
//
// **数组元素与 additionalProperties 不填**（文档定死）：默认值只写在声明的 properties 上，
// 在别的地方猜默认值等于发明第二份真相。
func fillDefaults(sch map[string]any, obj map[string]any) {
	if sch == nil || obj == nil {
		return
	}
	props := schema.Properties(sch)
	if len(props) == 0 {
		return
	}
	for _, name := range sortedSchemaKeys(props) {
		propSchema, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		cur, present := obj[name]
		if !present {
			if def, has := propSchema["default"]; has {
				obj[name] = deepCopyValue(def)
				cur = obj[name]
				present = true
			}
		}
		if !present {
			continue
		}
		if child, isObj := cur.(map[string]any); isObj {
			fillDefaults(propSchema, child)
		}
	}
}

// checkRequired 递归校验 `required`：缺键报 `values_missing` 并给出两条出路（§2.3 的消息要点）。
func checkRequired(sch map[string]any, obj map[string]any, at string) error {
	if sch == nil || obj == nil {
		return nil
	}
	for _, name := range schema.Required(sch) {
		if _, ok := obj[name]; !ok {
			return errf(CodeValuesMissing,
				"%s.%s 没有默认值也没传：要么 install 时给，要么在 spec.values 里写 default",
				at, name)
		}
	}
	props := schema.Properties(sch)
	for _, name := range sortedSchemaKeys(props) {
		child, ok := obj[name].(map[string]any)
		if !ok {
			continue
		}
		propSchema, isMap := props[name].(map[string]any)
		if !isMap {
			continue
		}
		if err := checkRequired(propSchema, child, at+"."+name); err != nil {
			return err
		}
	}
	return nil
}

// deepCopyMap 深拷贝一个 JSON 对象（打码/补默认值都不能改调用方的 map）。
func deepCopyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue 深拷贝一个 JSON 值（map / slice 递归；标量按值）。
func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return deepCopyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return v
	}
}

// sortedSchemaKeys 返回 properties 的键（升序）——默认值填充与错误消息的确定性来源。
func sortedSchemaKeys(props map[string]any) []string {
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
