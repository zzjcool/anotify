package sourcespec

// targets[].match 的字段级组合子（#519 A2）：`types` 之外加 `all`/`any` 两组叶子判据。三组同时出现 = AND、
// `all` 内 AND、`any` 内 OR；叶子取不到值/类型不符判 false 而**不是**错误（`_derived.` 见 derived.go）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 叶子的封闭 op 词表（与 #512 规则表逐字一致）。
const (
	MatchOpEQ       = "=="
	MatchOpNE       = "!="
	MatchOpGT       = ">"
	MatchOpGE       = ">="
	MatchOpLT       = "<"
	MatchOpLE       = "<="
	MatchOpContains = "contains"
	MatchOpMatches  = "matches"
	MatchOpIn       = "in"
	MatchOpExists   = "exists"
)

// MatchMaxRows 是单个组合子的行数上限（成本闸）；组合子不嵌套，一次求值 ≤ 2×32 行。
const MatchMaxRows = 32

// matchOps 是 op 的封闭集合（校验与「可选」消息都取自它）。
var matchOps = []string{
	MatchOpEQ, MatchOpNE, MatchOpGT, MatchOpGE, MatchOpLT, MatchOpLE,
	MatchOpContains, MatchOpMatches, MatchOpIn, MatchOpExists,
}

// MatchLeaf 是一条字段级判据：`field`（点号路径 + 数字下标，与 envelope 指针同方言）取值，
// `op` 与 `value` 比较，`negate` 翻转整行（字段缺失时也成立——`!=` 做不到这件事）。
type MatchLeaf struct {
	Field  string `json:"field"`
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
	Negate bool   `json:"negate,omitempty"`
}

// Match 是 targets[].match：三组都是「不写就不参与判定」，全不写 = 恒命中。
type Match struct {
	// Types 是精确、大小写敏感的事件类型白名单。
	Types []string `json:"types,omitempty"`
	// All 的每一行都必须成立；Any 至少一行成立。两者与 Types 之间是 AND。
	All []MatchLeaf `json:"all,omitempty"`
	Any []MatchLeaf `json:"any,omitempty"`
	// raw 是解码留底：未知键只有它认得出来，判定要等到 ValidateTargets（那里才有下标）。
	raw json.RawMessage
}

// UnmarshalJSON 只留底原始字节：读路径容忍坏行，键集交给 normalizeMatch（那里下标）。
func (m *Match) UnmarshalJSON(data []byte) error {
	m.raw = append(m.raw[:0], data...)
	wire := struct {
		Types []string    `json:"types"`
		All   []MatchLeaf `json:"all"`
		Any   []MatchLeaf `json:"any"`
	}{}
	// 尽力回填已知字段；坏形状留给 normalizeMatch 按路径报（这里解码失败不改变结论）。
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&wire)
	m.Types, m.All, m.Any = wire.Types, wire.All, wire.Any
	return nil
}

// Matches 报告是否命中（nil = 恒命中）；detail 是剪枝用的「为什么不命中」。
func (m *Match) Matches(eventType string, payload map[string]any) (bool, string) {
	if m == nil {
		return true, ""
	}
	if len(m.Types) > 0 && !containsText(m.Types, eventType) {
		return false, fmt.Sprintf("事件 type %q 不在 match.types %v 里", eventType, m.Types)
	}
	derivedValues := &derived{payload: payload}
	for i, leaf := range m.All {
		if !leaf.eval(payload, derivedValues) {
			return false, fmt.Sprintf("match.all[%d]（%s）不成立", i, leaf.describe())
		}
	}
	if len(m.Any) > 0 {
		hit := false
		for _, leaf := range m.Any {
			if leaf.eval(payload, derivedValues) {
				hit = true
				break
			}
		}
		if !hit {
			return false, fmt.Sprintf("match.any 的 %d 行全部不成立", len(m.Any))
		}
	}
	return true, ""
}

// eval 求值一行：字段取不到 → false（`negate` 再翻转）。
func (l MatchLeaf) eval(payload map[string]any, d *derived) bool {
	got, found := l.left(payload, d)
	ok := false
	if l.Op == MatchOpExists {
		ok = found
	} else if found {
		ok = compareLeaf(l.Op, got, l.Value)
	}
	if l.Negate {
		ok = !ok
	}
	return ok
}

// left 取一行的左值：`_derived.` 前缀走派生表，其余是载荷里的指针。
func (l MatchLeaf) left(payload map[string]any, d *derived) (any, bool) {
	if name, isDerived := derivedField(l.Field); isDerived {
		return d.lookup(name)
	}
	return LookupPointer(payload, l.Field)
}

// compareLeaf 是「字段值 vs 字面量」的比较表（类型不符一律 false）。
func compareLeaf(op string, got, want any) bool {
	switch op {
	case MatchOpEQ:
		return jsonEqual(got, want)
	case MatchOpNE:
		return !jsonEqual(got, want)
	case MatchOpGT, MatchOpGE, MatchOpLT, MatchOpLE:
		left, ok := numericValue(got)
		right, okRight := numericValue(want)
		if !ok || !okRight {
			return false
		}
		switch op {
		case MatchOpGT:
			return left > right
		case MatchOpGE:
			return left >= right
		case MatchOpLT:
			return left < right
		default:
			return left <= right
		}
	case MatchOpContains:
		text, ok := got.(string)
		part, okPart := want.(string)
		return ok && okPart && strings.Contains(text, part)
	case MatchOpMatches:
		text, ok := got.(string)
		pattern, okPattern := want.(string)
		if !ok || !okPattern {
			return false
		}
		// 读路径上手改坏的 pattern 按「不命中」处理，不 500。
		re, err := regexp.Compile(pattern)
		return err == nil && re.MatchString(text)
	case MatchOpIn:
		items, ok := want.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if jsonEqual(got, item) {
				return true
			}
		}
		return false
	}
	return false
}

// jsonEqual 是 `==` 的判据：数字按值比（`3` == `3.0`），数组/对象深比较。
func jsonEqual(a, b any) bool {
	if left, ok := numericValue(a); ok {
		right, okRight := numericValue(b)
		return okRight && left == right
	}
	switch value := a.(type) {
	case nil:
		return b == nil
	case string:
		other, ok := b.(string)
		return ok && value == other
	case bool:
		other, ok := b.(bool)
		return ok && value == other
	case []any:
		other, ok := b.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for i := range value {
			if !jsonEqual(value[i], other[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := b.(map[string]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for key, item := range value {
			theirs, has := other[key]
			if !has || !jsonEqual(item, theirs) {
				return false
			}
		}
		return true
	}
	return false
}

// numericValue 把 JSON 数字的任何 Go 表示折成 float64（载荷 UseNumber，也可能是 int）。
func numericValue(v any) (float64, bool) {
	switch value := v.(type) {
	case json.Number:
		f, err := value.Float64()
		return f, err == nil
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case int32:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint64:
		return float64(value), true
	}
	return 0, false
}

// describe 把一行写成人话（只进剪枝 detail，不参与判定）。
func (l MatchLeaf) describe() string {
	text := l.Field + " " + l.Op
	if l.Op != MatchOpExists {
		if encoded, err := json.Marshal(l.Value); err == nil {
			text += " " + string(encoded)
		}
	}
	if l.Negate {
		text += " negate"
	}
	return text
}

// containsText 报告字符串是否在列表里（types 白名单的唯一判据）。
func containsText(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ---------- 写入期校验 ----------

// matchCheck 累积一条 match 的校验问题。
type matchCheck struct {
	prefix string
	causes []Cause
}

func (c *matchCheck) add(field, reason, format string, args ...any) {
	c.causes = append(c.causes, Cause{Field: field, Reason: reason, Message: fmt.Sprintf(format, args...)})
}

// unknown 是未知键（reason 沿用文档层的 unknown_field，客户端已按它分支）。
func (c *matchCheck) unknown(field, scope, allowed string) {
	c.add(field, CauseReasonUnknownField, "%s 不认字段 %q（允许：%s）", scope, lastSegment(field), allowed)
}

// normalizeMatch 校验并归一一条 match：所有问题一起报、每条带位置，原始字节用完即弃。
func normalizeMatch(m *Match, prefix string) error {
	if m == nil {
		return nil
	}
	raw, err := m.sourceJSON()
	if err != nil {
		return err
	}
	c := &matchCheck{prefix: prefix}
	fields, ok := c.object(raw)
	if !ok {
		return &CausesError{Causes: c.causes}
	}
	for _, key := range sortedFieldKeys(fields) {
		switch key {
		case "types", "all", "any":
		default:
			c.unknown(prefix+"."+key, "match", "all / any / types")
		}
	}
	types := c.types(fields["types"], prefix+".types")
	all := c.leaves(fields["all"], prefix+".all")
	anyRows := c.leaves(fields["any"], prefix+".any")
	if len(types) == 0 && len(all) == 0 && len(anyRows) == 0 && len(c.causes) == 0 {
		c.add(prefix, CauseReasonValueInvalid, "至少要声明 types / all / any 之一（不写 match 才是全部命中）")
	}
	if len(c.causes) > 0 {
		return &CausesError{Causes: c.causes}
	}
	m.Types, m.All, m.Any, m.raw = types, all, anyRows, nil
	return nil
}

// object 解出 match 的顶层键值表（空/null 当空对象；非对象报一条）。
func (c *matchCheck) object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return map[string]json.RawMessage{}, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		c.add(c.prefix, CauseReasonValueInvalid, "必须是 JSON 对象（{types, all, any}）")
		return nil, false
	}
	return fields, true
}

// types 校验事件类型白名单（非空数组、元素非空且不重复）。
func (c *matchCheck) types(raw json.RawMessage, path string) []string {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		c.add(path, CauseReasonValueInvalid, "必须是字符串数组（如 [\"issues.opened\"]）")
		return nil
	}
	if len(list) == 0 {
		c.add(path, CauseReasonValueInvalid, "不能是空数组（空 types = 永不派发的死声明；要全部命中就别写 match）")
		return nil
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for i, item := range list {
		trimmed := strings.TrimSpace(item)
		switch {
		case trimmed == "":
			c.add(fmt.Sprintf("%s[%d]", path, i), CauseReasonValueInvalid, "不能为空")
		case seen[trimmed]:
			c.add(fmt.Sprintf("%s[%d]", path, i), CauseReasonValueInvalid, "与前面重复（%q）", trimmed)
		default:
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	return out
}

// leaves 校验一个组合子：非空、不超行数上限、逐行交给 leaf。
func (c *matchCheck) leaves(raw json.RawMessage, path string) []MatchLeaf {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		c.add(path, CauseReasonValueInvalid, "必须是数组（每行一条 {field, op, value, negate}）")
		return nil
	}
	if len(rows) == 0 {
		c.add(path, CauseReasonValueInvalid, "不能是空组合子（空组合子恒成立，等于没写它）")
		return nil
	}
	if len(rows) > MatchMaxRows {
		c.add(path, CauseReasonValueInvalid, "有 %d 行，超过上限 %d 行（成本闸，一次事件最多求值这么多行）", len(rows), MatchMaxRows)
		return nil
	}
	out := make([]MatchLeaf, 0, len(rows))
	for i, rowRaw := range rows {
		leaf, ok := c.leaf(rowRaw, fmt.Sprintf("%s[%d]", path, i))
		if ok {
			out = append(out, leaf)
		}
	}
	return out
}

// leaf 校验一行判据：键集、field 路径、op 词表、value 形状、negate 类型。
func (c *matchCheck) leaf(raw json.RawMessage, path string) (MatchLeaf, bool) {
	before := len(c.causes)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		c.add(path, CauseReasonValueInvalid, "必须是对象（{field, op, value, negate}）")
		return MatchLeaf{}, false
	}
	for _, key := range sortedFieldKeys(fields) {
		switch key {
		case "field", "op", "value", "negate":
		default:
			c.unknown(path+"."+key, "组合子行", "field / negate / op / value")
		}
	}
	leaf := MatchLeaf{}
	leaf.Field = c.text(fields, "field", path+".field", "必须是非空字符串（点号路径，如 issue.labels.0.name）")
	if leaf.Field != "" {
		parsed, err := ParsePointer(leaf.Field)
		switch {
		case err != nil:
			c.add(path+".field", CauseReasonValueInvalid, "%v", err)
		default:
			leaf.Field = parsed
			if name, isDerived := derivedField(parsed); isDerived && !containsText(derivedNames, name) {
				c.add(path+".field", CauseReasonValueInvalid, "不认识的派生字段 %q（可选：%s）",
					name, strings.Join(derivedNames, " / "))
			}
		}
	}
	leaf.Op = c.text(fields, "op", path+".op",
		fmt.Sprintf("必须是非空字符串（可选：%s）", strings.Join(matchOps, " / ")))
	if leaf.Op != "" && !isMatchOp(leaf.Op) {
		c.add(path+".op", CauseReasonValueInvalid, "不认识的 op %q（可选：%s）", leaf.Op, strings.Join(matchOps, " / "))
	}
	if rawValue, hasValue := fields["value"]; leaf.Op != MatchOpExists && isMatchOp(leaf.Op) {
		if !hasValue {
			c.add(path+".value", CauseReasonValueInvalid, "op %q 需要 value", leaf.Op)
		} else if value, err := decodeAny(rawValue); err != nil {
			c.add(path+".value", CauseReasonValueInvalid, "%v", err)
		} else if c.checkValue(leaf.Op, value, path+".value") {
			leaf.Value = value
		}
	}
	if rawNegate, hasNegate := fields["negate"]; hasNegate && !isNull(rawNegate) {
		var negate bool
		if err := json.Unmarshal(rawNegate, &negate); err != nil {
			c.add(path+".negate", CauseReasonValueInvalid, "必须是布尔值")
		}
		leaf.Negate = negate
	}
	if len(c.causes) > before {
		return MatchLeaf{}, false
	}
	return leaf, true
}

// checkValue 按 op 钉住 value 形状：死行是配置错误，不是「运行期 false」。
func (c *matchCheck) checkValue(op string, value any, path string) bool {
	switch op {
	case MatchOpMatches:
		pattern, ok := value.(string)
		if !ok {
			c.add(path, CauseReasonValueInvalid, "matches 的 value 必须是字符串（RE2 正则）")
			return false
		}
		if _, err := regexp.Compile(pattern); err != nil {
			c.add(path, CauseReasonValueInvalid, "正则编不过（RE2）：%v", err)
			return false
		}
	case MatchOpContains:
		if _, ok := value.(string); !ok {
			c.add(path, CauseReasonValueInvalid, "contains 的 value 必须是字符串")
			return false
		}
	case MatchOpGT, MatchOpGE, MatchOpLT, MatchOpLE:
		if _, ok := numericValue(value); !ok {
			c.add(path, CauseReasonValueInvalid, "op %s 的 value 必须是数字", op)
			return false
		}
	case MatchOpIn:
		items, ok := value.([]any)
		if !ok {
			c.add(path, CauseReasonValueInvalid, "in 的 value 必须是数组（如 [\"bug\", \"regression\"]）")
			return false
		}
		if len(items) == 0 {
			c.add(path, CauseReasonValueInvalid, "in 的 value 不能是空数组（没有可命中的值）")
			return false
		}
	}
	return true
}

func (c *matchCheck) text(fields map[string]json.RawMessage, key, path, requirement string) string {
	raw, ok := fields[key]
	if !ok || isNull(raw) {
		c.add(path, CauseReasonValueInvalid, "必填（%s）", requirement)
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		c.add(path, CauseReasonValueInvalid, "%s", requirement)
		return ""
	}
	return strings.TrimSpace(text)
}

// sourceJSON 取校验用的原始字节：解码来的对象用留底字节，构造出来的对象重新编码。
func (m *Match) sourceJSON() (json.RawMessage, error) {
	if len(m.raw) > 0 {
		return m.raw, nil
	}
	return marshal(m)
}

func isMatchOp(op string) bool {
	return containsText(matchOps, op)
}

func lastSegment(path string) string {
	if idx := strings.LastIndexByte(path, '.'); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
