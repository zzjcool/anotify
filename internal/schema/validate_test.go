package schema

import (
	"strings"
	"testing"
)

func TestValidateSchemaAcceptsSupportedSubset(t *testing.T) {
	sch := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"a"},
		"properties": map[string]any{
			"a": map[string]any{"type": []any{"string", "null"}, "pattern": "^x+$"},
			"b": map[string]any{"type": "integer", "minimum": 1, "maximum": 10},
			"c": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"d": map[string]any{
				"type":        "array",
				"prefixItems": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}},
				"items":       map[string]any{"type": "boolean"},
			},
			"e": map[string]any{"enum": []any{"dev", 1, nil}, "default": "dev"},
			"f": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "number"}},
			"g": true,
			"h": map[string]any{"$ref": "#/$defs/x", "writeOnly": true},
		},
		"$defs": map[string]any{"x": map[string]any{"type": "string"}},
	}
	if probs := ValidateSchema(sch); len(probs) != 0 {
		t.Fatalf("合法子集不该报问题: %v", probs)
	}
	if probs := ValidateSchema(map[string]any{}); len(probs) != 0 {
		t.Fatalf("空 schema 接受一切: %v", probs)
	}
	if probs := ValidateSchema(nil); len(probs) != 0 {
		t.Fatalf("nil schema 不该 panic/报问题: %v", probs)
	}
}

func TestValidateSchemaAcceptsGoTypedLists(t *testing.T) {
	sch := map[string]any{"type": "object", "required": []string{"a"}, "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	if probs := ValidateSchema(sch); len(probs) != 0 {
		t.Fatalf("[]string 形态的 required 也是字符串数组: %v", probs)
	}
}

func TestValidateSchemaUnknownTypeNames(t *testing.T) {
	cases := []struct {
		name    string
		sch     map[string]any
		path    string
		message string
	}{
		{"根 type 笔误", map[string]any{"type": "str"}, "type", `未知类型 "str"`},
		{"嵌套 type 笔误", map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "str"}},
		}, "properties.a.type", `未知类型 "str"`},
		{"type 数组里的笔误", map[string]any{"type": []any{"string", "integer", "float"}},
			"type[2]", `未知类型 "float"`},
		{"type 数组元素非字符串", map[string]any{"type": []any{"string", 1}}, "type[1]", "必须是字符串"},
		{"type 数组为空", map[string]any{"type": []any{}}, "type", "不能为空"},
		{"type 是数字", map[string]any{"type": 7}, "type", "字符串或字符串数组"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := ValidateSchema(tc.sch)
			if len(probs) == 0 {
				t.Fatalf("应报问题，实际没有")
			}
			if probs[0].Path != tc.path {
				t.Fatalf("路径 = %q，期望 %q（全部：%v）", probs[0].Path, tc.path, probs)
			}
			if !strings.Contains(probs[0].Msg, tc.message) {
				t.Fatalf("原因 = %q，应含 %q", probs[0].Msg, tc.message)
			}
		})
	}
}

func TestValidateSchemaSelfContradictions(t *testing.T) {
	cases := []struct {
		name  string
		sch   map[string]any
		path  string
		wants string
	}{
		{"required 不是数组", map[string]any{"required": "a"}, "required", "字符串数组"},
		{"required 含非字符串", map[string]any{"required": []any{"a", 1}}, "required", "字符串数组"},
		{"enum 不是数组", map[string]any{"enum": "dev"}, "enum", "必须是数组"},
		{"enum 是空数组", map[string]any{"enum": []any{}}, "enum", "空数组"},
		{"pattern 编译不过", map[string]any{
			"properties": map[string]any{"a": map[string]any{"type": "string", "pattern": "["}},
		}, "properties.a.pattern", "不可用"},
		{"pattern 非字符串", map[string]any{"pattern": 3}, "pattern", "必须是字符串"},
		{"minimum 非数字", map[string]any{"minimum": "1"}, "minimum", "必须是数字"},
		{"minLength 非数字", map[string]any{
			"properties": map[string]any{"a": map[string]any{"type": "string", "minLength": true}},
		}, "properties.a.minLength", "必须是数字"},
		{"properties 不是对象", map[string]any{"properties": []any{}}, "properties", "必须是对象"},
		{"字段模式不是 schema", map[string]any{
			"properties": map[string]any{"a": "string"},
		}, "properties.a", "schema 必须是对象或布尔"},
		{"items 不是 schema", map[string]any{
			"properties": map[string]any{"a": map[string]any{"type": "array", "items": "string"}},
		}, "properties.a.items", "schema 必须是对象或布尔"},
		{"prefixItems 不是数组", map[string]any{
			"properties": map[string]any{"a": map[string]any{"prefixItems": map[string]any{"type": "string"}}},
		}, "properties.a.prefixItems", "必须是数组"},
		{"prefixItems 元素不是 schema", map[string]any{
			"properties": map[string]any{"a": map[string]any{"prefixItems": []any{"string"}}},
		}, "properties.a.prefixItems[0]", "schema 必须是对象或布尔"},
		{"additionalProperties 不是 schema", map[string]any{"additionalProperties": 1},
			"additionalProperties", "schema 必须是对象或布尔"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := ValidateSchema(tc.sch)
			if len(probs) == 0 {
				t.Fatalf("应报问题，实际没有")
			}
			if probs[0].Path != tc.path {
				t.Fatalf("路径 = %q，期望 %q（全部：%v）", probs[0].Path, tc.path, probs)
			}
			if !strings.Contains(probs[0].Msg, tc.wants) {
				t.Fatalf("原因 = %q，应含 %q", probs[0].Msg, tc.wants)
			}
		})
	}
}

func TestValidateSchemaReportsEveryNestedProblem(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "str"},
			"b": map[string]any{"type": "string", "pattern": "["},
		},
	}
	probs := ValidateSchema(sch)
	if len(probs) != 2 {
		t.Fatalf("两处独立问题都要报，实际 %v", probs)
	}
	if probs[0].Path != "properties.a.type" || probs[1].Path != "properties.b.pattern" {
		t.Fatalf("路径应按 properties 键名排序且稳定，实际 %v", probs)
	}
	if got, want := probs[1].String(), "properties.b.pattern"; !strings.HasPrefix(got, want) {
		t.Fatalf("String() = %q，应以 %q 开头", got, want)
	}
}
