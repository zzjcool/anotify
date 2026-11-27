package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompilePatternCacheAndLimits(t *testing.T) {
	first, err := CompilePattern("^[a-z][a-z0-9-]*$")
	if err != nil {
		t.Fatalf("合法 pattern 编译失败: %v", err)
	}
	second, err := CompilePattern("^[a-z][a-z0-9-]*$")
	if err != nil {
		t.Fatalf("第二次编译失败: %v", err)
	}
	if first != second {
		t.Fatal("同一个 pattern 应命中编译缓存（返回同一个 *regexp.Regexp）")
	}

	if _, err := CompilePattern("["); err == nil {
		t.Fatal("坏正则必须在编译期被拒")
	}
	long := strings.Repeat("a", MaxPatternLen+1)
	if _, err := CompilePattern(long); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超长 pattern 应被长度上限拒绝，得到 %v", err)
	}
}

func TestValuePatternOnlyConstrainsStrings(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
			"count":   map[string]any{"pattern": "^n-[0-9]+$"},
		},
	}
	if msg := Value(sch, map[string]any{"project": "abc", "count": 7}, "params"); msg != "" {
		t.Fatalf("合法值与非字符串值都应通过，得到 %q", msg)
	}
	msg := Value(sch, map[string]any{"project": "a/b"}, "params")
	if !strings.Contains(msg, "pattern") || !strings.Contains(msg, "a/b") {
		t.Fatalf("不匹配时应同时给出 pattern 与实际值，得到 %q", msg)
	}
}

func TestValueRejectsByFalseChildSchemas(t *testing.T) { // ValidateSchema 放行布尔子 schema，求值就必须执行它：false 不接受任何值（#642 返工点）
	cases := []struct {
		name string
		sch  map[string]any
		bad  any
		at   string
		ok   any
	}{
		{
			name: "properties 下的 false 字段",
			sch:  map[string]any{"type": "object", "properties": map[string]any{"pin": false}},
			bad:  map[string]any{"pin": "1234"},
			at:   "values.pin",
			ok:   map[string]any{},
		},
		{
			name: "嵌套 properties 下的 false 字段",
			sch: map[string]any{"type": "object", "properties": map[string]any{
				"outer": map[string]any{"type": "object", "properties": map[string]any{"inner": false}},
			}},
			bad: map[string]any{"outer": map[string]any{"inner": 1}},
			at:  "values.outer.inner",
			ok:  map[string]any{"outer": map[string]any{}},
		},
		{
			name: "additionalProperties 子 schema 里的 false 字段",
			sch:  map[string]any{"type": "object", "additionalProperties": map[string]any{"properties": map[string]any{"pin": false}}},
			bad:  map[string]any{"extra": map[string]any{"pin": "x"}},
			at:   "values.extra.pin",
			ok:   map[string]any{"extra": map[string]any{"other": "x"}},
		},
		{
			name: "items false 拒绝任何元素",
			sch:  map[string]any{"type": "array", "items": false},
			bad:  []any{1},
			at:   "values[0]",
			ok:   []any{},
		},
		{
			name: "prefixItems 之后的 items false",
			sch:  map[string]any{"type": "array", "prefixItems": []any{map[string]any{"type": "string"}}, "items": false},
			bad:  []any{"a", "b"},
			at:   "values[1]",
			ok:   []any{"a"},
		},
		{
			name: "prefixItems 里的 false 只拒该位置",
			sch:  map[string]any{"type": "array", "prefixItems": []any{map[string]any{"type": "string"}, false}},
			bad:  []any{"a", "b"},
			at:   "values[1]",
			ok:   []any{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if probs := ValidateSchema(tc.sch); len(probs) != 0 {
				t.Fatalf("布尔子 schema 是合法写法，ValidateSchema 不该报问题: %v", probs)
			}
			msg := Value(tc.sch, tc.bad, "values")
			if !strings.Contains(msg, tc.at) || !strings.Contains(msg, "false") {
				t.Fatalf("false 子 schema 必须拒绝该值、点名 %q 并提到 schema false，得到 %q", tc.at, msg)
			}
			if msg := Value(tc.sch, tc.ok, "values"); msg != "" {
				t.Fatalf("不触发 false 的值应通过，得到 %q", msg)
			}
		})
	}
}

func TestValueAcceptsByTrueChildSchemas(t *testing.T) { // true 是「不约束」，不能顺手当成 false
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"any":  true,
			"list": map[string]any{"type": "array", "items": true},
		},
		"additionalProperties": true,
	}
	if probs := ValidateSchema(sch); len(probs) != 0 {
		t.Fatalf("true 子 schema 是合法写法: %v", probs)
	}
	value := map[string]any{"any": map[string]any{"k": []any{1, 2}}, "list": []any{"x", 1}, "extra": 3.5}
	if msg := Value(sch, value, "values"); msg != "" {
		t.Fatalf("true 子 schema 不该拦任何值，得到 %q", msg)
	}
}

func TestFromRawRejectsRootBoolean(t *testing.T) { // 布尔**子** schema 受支持，但 spec 根必须是对象：根布尔要报错，不能当成「不约束」
	for _, raw := range []string{"true", "false"} {
		if _, err := FromRaw([]byte(raw)); err == nil {
			t.Fatalf("根 schema %s 应报错（values 契约要求对象）", raw)
		}
	}
	if sch, err := FromRaw(nil); err != nil || sch != nil {
		t.Fatalf("没声明 schema = 不校验，得到 %v / %v", sch, err)
	}
}

func TestValueUnderstandsJSONNumbers(t *testing.T) {
	sch := map[string]any{"type": "object", "properties": map[string]any{
		"n":     map[string]any{"type": "integer", "minimum": 1, "maximum": 10},
		"ratio": map[string]any{"type": "number", "minimum": 0.5},
	}}
	good := []map[string]any{
		{"n": json.Number("10")},
		{"n": json.Number("3"), "ratio": json.Number("0.5")},
		{"ratio": json.Number("1e3")},
	}
	for i, v := range good {
		if msg := Value(sch, v, "params"); msg != "" {
			t.Fatalf("合法输入 #%d %v 被拒：%s", i, v, msg)
		}
	}
	bad := []map[string]any{
		{"n": json.Number("0")},
		{"n": json.Number("11")},
		{"n": json.Number("1.5")},
		{"ratio": json.Number("0.4")},
	}
	for i, v := range bad {
		if msg := Value(sch, v, "params"); msg == "" {
			t.Fatalf("非法输入 #%d %v 应被拒", i, v)
		}
	}
	if msg := Value(map[string]any{"enum": []any{json.Number("1"), "a"}}, json.Number("1"), "x"); msg != "" {
		t.Fatalf("enum 的数值比较应跨 float64/json.Number，得到 %q", msg)
	}
}
