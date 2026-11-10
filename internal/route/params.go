package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 参数对齐（capability.md §4.2 / §2.3 / source.md §4.1）。
//
// v4 的派发期参数只有两条来源：
//
//   - 能力 emit：上游 payload 的字段 → 下游 params 的**同名字段**（多余字段忽略）；
//   - 源事件：源的 `targets[].params` 模板（`$(payload.路径)`）求值结果。
//
// 两条来源最终都要过**下游能力的 `spec.params` JSON Schema**：有 default 的字段缺失
// 用 default；必填且无 default 的字段缺失 → 剪枝 `param_missing`；类型不符 →
// `param_type`；schema 本身不可用 → `param_schema`。剪枝不是 4xx（capability.md
// §4.1 / §6.2）：事件已接收、下游不建 task，回执 `taskId:null`。
//
// 实现的 JSON Schema 子集是**显式**的（只支持 type / properties / required / default，
// 嵌套对象同样按子集校验，数组元素类型不递归）：剩下的关键字（oneOf / pattern /
// minimum …）被忽略而不是被当成失败。理由是写路径（artifact.Validate）已经保证
// schema 形状可用，而中枢不该在执行期发明一个半吊子 JSON Schema 实现；把子集写进
// 接口文档比假装支持全部关键字安全。

// Prune 是一次「事件/emit 已接收、下游没有建 task」的观察原因（capability.md §8.3）。
//
// 它对应落库的 `source='route'` 剪枝事件行与回执 `pruned[]`；reason 取封闭词表，
// 不新造词（protocol.IsPruneReason / protocol.ReasonDownstreamMissing）。
type Prune struct {
	// Capability 是下游能力（解析得到时用解析名，解析不到时用声明引用）。
	Capability string `json:"capability,omitempty"`
	// Reason 是封闭原因码：no_target / event_schema / param_missing / param_schema /
	// param_type / downstream_missing。
	Reason string `json:"reason"`
	// Detail 是给人看的补充（缺哪个参数、哪个字段类型不对），不参与任何判定。
	Detail string `json:"detail,omitempty"`
}

// paramProperty 是 JSON Schema 子集里的一个属性声明。
type paramProperty struct {
	types      []string
	def        any
	hasDefault bool
	nested     *paramSchema
}

// paramSchema 是 spec.params / payloadSchema 解析出的可用子集。
type paramSchema struct {
	// permissive 表示「{} / 省略 / null」：怎么传都过（capability.md §2.3）。
	permissive bool
	properties map[string]paramProperty
	required   map[string]bool
	order      []string
}

// parseParamSchema 解析参数 schema 子集；不可用时返回 param_schema 剪枝。
func parseParamSchema(raw json.RawMessage) (*paramSchema, *Prune) {
	s := &paramSchema{properties: map[string]paramProperty{}, required: map[string]bool{}}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		s.permissive = true
		return s, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc == nil {
		return nil, &Prune{Reason: protocol.ReasonParamSchema, Detail: "参数 schema 不是 JSON 对象"}
	}
	if rawType, ok := doc["type"]; ok {
		if !schemaIsObjectType(rawType) {
			return nil, &Prune{Reason: protocol.ReasonParamSchema, Detail: "参数 schema.type 必须是 object"}
		}
	}
	if rawProps, ok := doc["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(rawProps, &props); err != nil || props == nil {
			return nil, &Prune{Reason: protocol.ReasonParamSchema, Detail: "参数 schema.properties 不是对象"}
		}
		for name, rawProp := range props {
			prop, prune := parseParamProperty(name, rawProp)
			if prune != nil {
				return nil, prune
			}
			s.properties[name] = prop
			s.order = append(s.order, name)
		}
		sort.Strings(s.order)
	}
	if rawRequired, ok := doc["required"]; ok {
		var names []string
		if err := json.Unmarshal(rawRequired, &names); err != nil {
			return nil, &Prune{Reason: protocol.ReasonParamSchema, Detail: "参数 schema.required 必须是字符串数组"}
		}
		for _, name := range names {
			s.required[name] = true
		}
	}
	return s, nil
}

// parseParamProperty 解析一个属性（type/default/嵌套 properties）。
func parseParamProperty(name string, raw json.RawMessage) (paramProperty, *Prune) {
	var prop paramProperty
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return prop, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc == nil {
		return prop, &Prune{Reason: protocol.ReasonParamSchema,
			Detail: fmt.Sprintf("参数 %s 的 schema 不是对象", name)}
	}
	if rawType, ok := doc["type"]; ok {
		types, terr := schemaTypes(rawType)
		if terr != nil {
			return prop, &Prune{Reason: protocol.ReasonParamSchema,
				Detail: fmt.Sprintf("参数 %s：%v", name, terr)}
		}
		prop.types = types
	}
	if rawDefault, ok := doc["default"]; ok {
		dec := json.NewDecoder(bytes.NewReader(rawDefault))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return prop, &Prune{Reason: protocol.ReasonParamSchema,
				Detail: fmt.Sprintf("参数 %s 的 default 不是合法 JSON", name)}
		}
		prop.def = v
		prop.hasDefault = true
	}
	if rawProps, ok := doc["properties"]; ok {
		// 嵌套对象按同一子集校验：把 properties 重新包成一份 schema 交给解析器。
		wrapped, err := json.Marshal(map[string]json.RawMessage{
			"type":       json.RawMessage(`"object"`),
			"properties": rawProps,
		})
		if err == nil {
			// required 可能在外层；嵌套的 required 也在同一份 doc 里。
			if rawRequired, rok := doc["required"]; rok {
				var merged map[string]json.RawMessage
				if json.Unmarshal(wrapped, &merged) == nil {
					merged["required"] = rawRequired
					if again, merr := json.Marshal(merged); merr == nil {
						wrapped = again
					}
				}
			}
			nested, prune := parseParamSchema(wrapped)
			if prune != nil {
				return prop, prune
			}
			prop.nested = nested
		}
	}
	return prop, nil
}

// schemaIsObjectType 判断 schema.type 是否接受 object（字符串或数组两种写法）。
func schemaIsObjectType(raw json.RawMessage) bool {
	types, err := schemaTypes(raw)
	if err != nil {
		return false
	}
	for _, t := range types {
		if t == "object" {
			return true
		}
	}
	return false
}

// schemaTypes 解析 type 字段（字符串或字符串数组）。
func schemaTypes(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, fmt.Errorf("type 为空")
		}
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("type 必须是字符串或字符串数组")
	}
	for _, t := range many {
		if t == "" {
			return nil, fmt.Errorf("type 数组里有空值")
		}
	}
	return many, nil
}

// PrepareParams 把一份输入对齐到下游能力的参数契约（emit 同名直取 / 源模板求值后共用）。
//
// 返回：规范化后的 params（JSON 对象字节）或一条剪枝。空 schema 表示「怎么传都过」，
// 此时输入原样成为 params（仍会序列化成对象）。
func PrepareParams(schema json.RawMessage, input map[string]any) (json.RawMessage, *Prune) {
	s, prune := parseParamSchema(schema)
	if prune != nil {
		return nil, prune
	}
	out := make(map[string]any, len(input))
	if s.permissive {
		for name, value := range input {
			out[name] = value
		}
		return marshalObject(out), nil
	}
	missing := []string{}
	for _, name := range s.order {
		prop := s.properties[name]
		if value, ok := input[name]; ok {
			if detail := prop.check(name, value); detail != "" {
				return nil, &Prune{Reason: protocol.ReasonParamType, Detail: detail}
			}
			out[name] = value
			continue
		}
		if prop.hasDefault {
			out[name] = prop.def
			continue
		}
		if s.required[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &Prune{Reason: protocol.ReasonParamMissing, Detail: "需要 " + strings.Join(missing, ", ")}
	}
	return marshalObject(out), nil
}

// ValidatePayload 校验源投递载荷是否符合 `config.payloadSchema`（event_schema 剪枝）。
//
// 返回 nil 表示通过；否则 reason 固定是 event_schema（source.md §6）。载荷 schema
// 不可用时同样按 event_schema 拒绝——那不是投递方的错，但不能放一批形状未知的
// 载荷进去（写路径已拒绝坏 schema，这里是防御）。
func ValidatePayload(schema json.RawMessage, payload map[string]any) *Prune {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return nil
	}
	s, prune := parseParamSchema(schema)
	if prune != nil {
		return &Prune{Reason: protocol.ReasonEventSchema, Detail: "payloadSchema 不可用：" + prune.Detail}
	}
	if s.permissive {
		return nil
	}
	missing := []string{}
	for _, name := range s.order {
		prop := s.properties[name]
		value, ok := payload[name]
		if !ok {
			if s.required[name] {
				missing = append(missing, name)
			}
			continue
		}
		if detail := prop.check(name, value); detail != "" {
			return &Prune{Reason: protocol.ReasonEventSchema, Detail: detail}
		}
	}
	if len(missing) > 0 {
		return &Prune{Reason: protocol.ReasonEventSchema, Detail: "载荷缺 " + strings.Join(missing, ", ")}
	}
	return nil
}

// check 校验一个值是否符合属性声明；detail 为空表示通过。
func (p paramProperty) check(name string, value any) string {
	if len(p.types) > 0 {
		matched := false
		for _, t := range p.types {
			if valueMatchesType(t, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("参数 %s 类型应为 %s，实际 %s", name, strings.Join(p.types, "|"), jsonKind(value))
		}
	}
	if p.nested != nil {
		obj, ok := value.(map[string]any)
		if ok {
			if detail := p.nested.checkObject(obj, name); detail != "" {
				return detail
			}
		}
	}
	return ""
}

// checkObject 用子集规则校验一个对象值（不含 default 补全）。
func (s *paramSchema) checkObject(obj map[string]any, prefix string) string {
	if s.permissive {
		return ""
	}
	missing := []string{}
	for _, name := range s.order {
		prop := s.properties[name]
		value, ok := obj[name]
		if !ok {
			if s.required[name] {
				missing = append(missing, name)
			}
			continue
		}
		if detail := prop.check(prefix+"."+name, value); detail != "" {
			return detail
		}
	}
	if len(missing) > 0 {
		return "缺少 " + prefix + "." + strings.Join(missing, ", "+prefix+".")
	}
	return ""
}

// valueMatchesType 判断一个解码后的 JSON 值是否符合 schema 类型名。
func valueMatchesType(want string, value any) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case json.Number, float64, float32, int, int64, int32:
			return true
		}
		return false
	case "integer":
		switch v := value.(type) {
		case json.Number:
			return !strings.ContainsAny(string(v), ".eE")
		case float64:
			return v == float64(int64(v))
		case int, int64, int32:
			return true
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	}
	// 未知类型名（JSON Schema 扩展）：不做判定，放行而不是误剪。
	return true
}

// jsonKind 是给人看的 JSON 类型名（detail 用）。
func jsonKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case json.Number, float64:
		return "number"
	}
	return "unknown"
}

// marshalObject 把 map 序列成 JSON 对象（确定性：encoding/json 对 map 键排序）。
//
// 空 map 输出 `{}` 而不是 `null`：空参数是「没有参数」，不是「没有值」。
func marshalObject(m map[string]any) json.RawMessage {
	if m == nil {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// DecodePayload 把一段 JSON 解成对象（UseNumber：数字保留原写法，参数原样透传）。
//
// 空 / null → 空对象。不是对象 → ok=false（调用方按 param_schema 剪枝）。
func DecodePayload(raw json.RawMessage) (map[string]any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, true
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return nil, false
	}
	obj, ok := probe.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}
