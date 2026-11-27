package route

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/schema"
)

// 参数对齐（capability.md §4.2 / §2.3 / source.md §4.1）。
//
// v4 的派发期参数只有两条来源：
//
//   - 能力 emit：上游 payload 的字段 → 下游 params 的**同名字段**（多余字段忽略）；
//   - 源事件：源的 `targets[].params` 模板（`$(payload.路径)`）求值结果。
//
// 两条来源都要过**下游能力的 `spec.params` JSON Schema**：有 default 的缺失字段用 default；
// 必填无 default → 剪枝 `param_missing`；类型不符 → `param_type`；schema 不可用 →
// `param_schema`。剪枝不是 4xx（§4.1/§6.2）：事件已接收、下游不建 task，回执 `taskId:null`。
//
// JSON Schema 子集是**显式**的（白名单见 schema.ParseParams）：type / properties / required /
// additionalProperties / items / prefixItems / enum / min·max / 长度 / 数量 / pattern / default；
// 表外关键字与未知 type 名写入期就 400（param_schema，#580），求值只有 schema.Value 一份实现。

// Prune 是一次「事件/emit 已接收、下游没有建 task」的观察原因（§8.3）：落库成
// `source='route'` 的剪枝事件行 + 回执 `pruned[]`；reason 取封闭词表，不新造词。
type Prune struct {
	// Capability 是下游能力（解析得到时用解析名，解析不到时用声明引用）。
	Capability string `json:"capability,omitempty"`
	// Reason 是封闭原因码：no_target / event_schema / param_missing / param_schema / param_type / downstream_missing。
	Reason string `json:"reason"`
	// Detail 是给人看的补充（缺哪个参数、哪个字段类型不对），不参与任何判定。
	Detail string `json:"detail,omitempty"`
	// Field 是导致剪枝的参数路径（如 `params.diff`），只填 400 的 details.causes[].field。**不进 wire**。
	Field string `json:"-"`
}

// paramSchema 是子集解析结果的别名：判据在 internal/schema（写入期与运行期同一份）。
type paramSchema = schema.Params

// parseParamSchema 解析参数 schema 子集；不可用时返回 param_schema 剪枝。
//
// **判据只有一处**：schema.ParseParams。写入期（artifact.Validate / sourcespec）拒的
// 就是这里剪的（#612）——两边各写一套必然漂移，症状是「建得成、永不派发」。
func parseParamSchema(raw json.RawMessage) (*paramSchema, *Prune) {
	s, problems := schema.ParseParams(raw)
	if len(problems) > 0 {
		return nil, &Prune{Reason: protocol.ReasonParamSchema, Detail: problems[0].Msg}
	}
	return s, nil
}

// PrepareParams 把一份输入对齐到下游能力的参数契约（emit 同名直取 / 源模板求值后共用）。
//
// 返回：规范化后的 params（JSON 对象字节）或一条剪枝。空 schema 表示「怎么传都过」，
// 此时输入原样成为 params（仍会序列化成对象）。
func PrepareParams(raw json.RawMessage, input map[string]any) (json.RawMessage, *Prune) {
	s, prune := parseParamSchema(raw)
	if prune != nil {
		return nil, prune
	}
	out := make(map[string]any, len(input))
	if s.Permissive {
		for name, value := range input {
			out[name] = value
		}
		return marshalObject(out), nil
	}
	missing := []string{}
	for _, name := range s.Order {
		prop := s.Properties[name]
		if value, ok := input[name]; ok {
			if issue := prop.Check(name, value); issue != nil {
				return nil, &Prune{Reason: protocol.ReasonParamType, Detail: issue.Detail, Field: "params." + issue.Field}
			}
			if issue := prop.CheckKeywords(name, value); issue != nil {
				return nil, &Prune{Reason: protocol.ReasonParamType, Detail: issue.Detail, Field: "params." + issue.Field}
			}
			out[name] = value
			continue
		}
		if prop.HasDefault {
			out[name] = prop.Default
			continue
		}
		if s.Required[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &Prune{Reason: protocol.ReasonParamMissing,
			Detail: "需要 " + strings.Join(missing, ", "), Field: "params." + missing[0]}
	}
	if name := undeclaredKey(s, input); name != "" { // additionalProperties:false：多余字段即错（#580）
		return nil, &Prune{Reason: protocol.ReasonParamType,
			Detail: "参数 " + name + " 不在 schema.properties 里（additionalProperties=false）",
			Field:  "params." + name}
	}
	return marshalObject(out), nil
}

func undeclaredKey(s *paramSchema, input map[string]any) string { // 排序后的第一个未声明输入键
	if !s.RejectsAdditional {
		return ""
	}
	extra := make([]string, 0)
	for name := range input {
		if _, declared := s.Properties[name]; !declared {
			extra = append(extra, name)
		}
	}
	if len(extra) == 0 {
		return ""
	}
	sort.Strings(extra) // map 遍历无序：报哪个多余字段要看得到可复现的答案
	return extra[0]
}

// ValidatePayload 校验源投递载荷是否符合 `config.payloadSchema`（event_schema 剪枝）。
//
// 返回 nil 表示通过；否则 reason 固定是 event_schema（source.md §6）。载荷 schema 不可用时同样按
// event_schema 拒绝——那不是投递方的错，但不能放一批形状未知的载荷进去（写路径已拒绝坏 schema，这里是防御）。
func ValidatePayload(raw json.RawMessage, payload map[string]any) *Prune {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return nil
	}
	s, prune := parseParamSchema(raw)
	if prune != nil {
		return &Prune{Reason: protocol.ReasonEventSchema, Detail: "payloadSchema 不可用：" + prune.Detail}
	}
	if s.Permissive {
		return nil
	}
	missing := []string{}
	for _, name := range s.Order {
		prop := s.Properties[name]
		value, ok := payload[name]
		if !ok {
			if s.Required[name] {
				missing = append(missing, name)
			}
			continue
		}
		if issue := prop.Check(name, value); issue != nil {
			return &Prune{Reason: protocol.ReasonEventSchema, Detail: issue.Detail, Field: "payload." + issue.Field}
		}
		if issue := prop.CheckKeywords(name, value); issue != nil {
			return &Prune{Reason: protocol.ReasonEventSchema, Detail: issue.Detail, Field: "payload." + issue.Field}
		}
	}
	// required 独立于 properties：只写 required 时上面一个名字都看不到 —— 这里只查「键存在」（null 也算）。
	for name := range s.Required {
		if _, declared := s.Properties[name]; declared {
			continue
		}
		if _, ok := payload[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing) // required 是 map：排序后 detail/Field 才可复现
		return &Prune{Reason: protocol.ReasonEventSchema, Detail: "载荷缺 " + strings.Join(missing, ", "),
			Field: "payload." + missing[0]}
	}
	if name := undeclaredKey(s, payload); name != "" {
		return &Prune{Reason: protocol.ReasonEventSchema,
			Detail: "载荷字段 " + name + " 不在 payloadSchema.properties 里（additionalProperties=false）",
			Field:  "payload." + name}
	}
	return nil
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
