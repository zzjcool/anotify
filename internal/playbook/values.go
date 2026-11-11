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
// 打码跟着 schema 走（不是猜字段名、也不按值匹配）：对象按 `properties` 递归、未声明的键
// 回落 `additionalProperties`（当其子树含 writeOnly）、数组按位置（`prefixItems[i]` 优先，
// `items` 管其余）。打码只影响回执，不影响渲染与 digest。
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

// maskValue 递归打码一个值（只按 schema 判密文，不按值猜）。
//
// 三条分支（顺序即优先级）：
//   - 节点自身 `writeOnly: true` → 整值换成 `MaskedValue`；
//   - 对象：键命中 `properties` 走该子 schema；**未命中的键**若节点上的
//     `additionalProperties` 是「自身/子树含 writeOnly 的对象 schema」→ 按它递归打码
//     （任意键都可能是密文）；否则原样透传——非 writeOnly 的 `additionalProperties`
//     行为完全不变（一次多余深拷贝都不做）；
//   - 数组：按位置取 schema（见 `arrayElementSchema`）。
func maskValue(sch map[string]any, v any) any {
	if isWriteOnly(sch) {
		return MaskedValue
	}
	switch val := v.(type) {
	case map[string]any:
		props := schema.Properties(sch)
		var addl map[string]any
		if node, ok := additionalPropertiesSchema(sch); ok && subtreeHasWriteOnly(node) {
			addl = node
		}
		out := make(map[string]any, len(val))
		for k, child := range val {
			if childSchema, ok := props[k].(map[string]any); ok {
				out[k] = maskValue(childSchema, child)
				continue
			}
			if addl != nil {
				out[k] = maskValue(addl, child)
				continue
			}
			out[k] = child
		}
		return out
	case []any:
		prefix, _ := sch["prefixItems"].([]any)
		itemSchema, _ := sch["items"].(map[string]any)
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = maskValue(arrayElementSchema(prefix, itemSchema, i), child)
		}
		return out
	default:
		return v
	}
}

// arrayElementSchema 返回数组第 i 个元素该用的打码 schema。
//
// 2020-12 语义：`prefixItems[i]` 管第 i 个元素、`items` 管其余。打码每个位置只留**一个**
// schema，取法按「谁声明了 writeOnly」，方向只允许**多**打码：
//   - `prefixItems[i]` 自身/子树含 writeOnly → 用它（元组按位精确打码）；
//   - 否则回落 `items`——包括「`prefixItems[i]` 只是普通 schema 而 `items` 带 writeOnly」
//     这种歧义形状：旧口径（数组一律按 items）保留，元素仍整体打码，不留缝。
func arrayElementSchema(prefix []any, items map[string]any, i int) map[string]any {
	if i < len(prefix) {
		if node, ok := prefix[i].(map[string]any); ok && subtreeHasWriteOnly(node) {
			return node
		}
	}
	return items
}

// additionalPropertiesSchema 返回节点上声明为**对象 schema** 的 `additionalProperties`。
// `true` / `false` 这类布尔关键字不是 schema（也不是「对象任意键」的声明），返回 false。
func additionalPropertiesSchema(sch map[string]any) (map[string]any, bool) {
	if sch == nil {
		return nil, false
	}
	node, ok := sch["additionalProperties"].(map[string]any)
	return node, ok
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
