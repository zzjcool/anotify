package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/schema"
)

func TestPrepareParamsPermissiveSchema(t *testing.T) {
	params, prune := PrepareParams(nil, map[string]any{"anything": "x", "n": 1})
	if prune != nil {
		t.Fatalf("空 schema 应该怎么传都过，得到剪枝 %+v", prune)
	}
	var got map[string]any
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("params 不是合法 JSON: %v", err)
	}
	if got["anything"] != "x" {
		t.Fatalf("空 schema 应原样透传，得到 %v", got)
	}
}

func TestPrepareParamsDefaultsRequiredAndExtra(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"file":{"type":"string"},"mode":{"type":"string","default":"fast"}},
		"required":["file"]
	}`)
	params, prune := PrepareParams(schema, map[string]any{"file": "a.go", "extra": "ignored"})
	if prune != nil {
		t.Fatalf("合法输入不该被剪枝: %+v", prune)
	}
	var got map[string]any
	_ = json.Unmarshal(params, &got)
	if got["file"] != "a.go" || got["mode"] != "fast" {
		t.Fatalf("default 未补齐或 extra 未忽略: %v", got)
	}
	if _, ok := got["extra"]; ok {
		t.Fatalf("payload 多余字段必须忽略（同名直取），得到 %v", got)
	}
}

func TestPrepareParamsMissingRequired(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}`)
	if _, prune := PrepareParams(schema, map[string]any{}); prune == nil {
		t.Fatal("缺必填参数必须剪枝")
	} else if prune.Reason != protocol.ReasonParamMissing {
		t.Fatalf("剪枝 reason = %q，want param_missing", prune.Reason)
	}
}

func TestPrepareParamsTypeMismatch(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)
	_, prune := PrepareParams(schema, map[string]any{"count": "not-a-number"})
	if prune == nil || prune.Reason != protocol.ReasonParamType {
		t.Fatalf("类型不符应剪枝 param_type，得到 %+v", prune)
	}
}

func TestPrepareParamsBadSchema(t *testing.T) {
	_, prune := PrepareParams(json.RawMessage(`[]`), map[string]any{})
	if prune == nil || prune.Reason != protocol.ReasonParamSchema {
		t.Fatalf("坏 schema 应剪枝 param_schema，得到 %+v", prune)
	}
}

func TestValidatePayloadSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"environment":{"type":"string"}},"required":["environment"]}`)
	if prune := ValidatePayload(schema, map[string]any{"environment": "prod"}); prune != nil {
		t.Fatalf("合法载荷不该剪枝: %+v", prune)
	}
	prune := ValidatePayload(schema, map[string]any{"environment": 3})
	if prune == nil || prune.Reason != protocol.ReasonEventSchema {
		t.Fatalf("载荷类型不符应剪枝 event_schema，得到 %+v", prune)
	}
}

func assertPayloadPrune(t *testing.T, prune *Prune, detail, field string) {
	t.Helper()
	if detail == "" {
		if prune != nil {
			t.Fatalf("应通过，得到剪枝 %+v", prune)
		}
		return
	}
	if prune == nil || prune.Reason != protocol.ReasonEventSchema {
		t.Fatalf("应剪枝 event_schema，得到 %+v", prune)
	}
	if prune.Detail != detail || prune.Field != field {
		t.Fatalf("剪枝 = %q / %q，want %q / %q", prune.Detail, prune.Field, detail, field)
	}
}

// TestValidatePayloadPropertiesOnlyUnchanged 是对照组：#516 的修正不许动既有输出（在旧实现上必须同样绿）。
func TestValidatePayloadPropertiesOnlyUnchanged(t *testing.T) {
	withRequired := `{"type":"object","properties":{"environment":{"type":"string"}},"required":["environment"]}`
	withoutRequired := `{"type":"object","properties":{"environment":{"type":"string"}}}`
	cases := []struct {
		name    string
		schema  string
		payload map[string]any
		detail  string
	}{
		{"没有 required 时缺字段放行", withoutRequired, map[string]any{}, ""},
		{"没有 required 时多余字段放行", withoutRequired, map[string]any{"environment": "prod", "extra": true}, ""},
		{"properties 声明的必填缺失照旧剪枝", withRequired, map[string]any{"other": 1}, "载荷缺 environment"},
		{"类型不符照旧剪枝", withRequired, map[string]any{"environment": json.Number("3")}, "参数 environment 类型应为 string，实际 number"},
		{"多个缺失按名字排序", `{"properties":{"b":{"type":"string"},"a":{"type":"string"}},"required":["b","a"]}`, map[string]any{}, "载荷缺 a, b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prune := ValidatePayload(json.RawMessage(tc.schema), tc.payload)
			if prune == nil {
				if tc.detail != "" {
					t.Fatalf("应剪枝 %q，实际通过", tc.detail)
				}
				return
			}
			if prune.Reason != protocol.ReasonEventSchema || prune.Detail != tc.detail {
				t.Fatalf("剪枝 = %q / %q，want event_schema / %q", prune.Reason, prune.Detail, tc.detail)
			}
		})
	}
}

// TestValidatePayloadRequiredWithoutProperties 是 #516 的主用例：required 独立于 properties。
func TestValidatePayloadRequiredWithoutProperties(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		payload map[string]any
		detail  string
		field   string
	}{
		{"只写 required 缺字段", `{"required":["action"]}`, map[string]any{}, "载荷缺 action", "payload.action"},
		{"键存在即通过（null 也算存在）", `{"required":["action"]}`, map[string]any{"action": nil}, "", ""},
		{"键存在即通过（任意值）", `{"required":["action"]}`, map[string]any{"action": 1}, "", ""},
		{"required 一半在 properties 里", `{"properties":{"a":{"type":"string"}},"required":["a","b"]}`, map[string]any{"a": "x"}, "载荷缺 b", "payload.b"},
		{"properties 里的必填缺失也落到 payload.<名>", `{"properties":{"a":{"type":"string"}},"required":["a"]}`, map[string]any{}, "载荷缺 a", "payload.a"},
		{"properties 的类型判定不受影响", `{"properties":{"a":{"type":"string"}},"required":["b"]}`, map[string]any{"a": json.Number("1")}, "参数 a 类型应为 string，实际 number", "payload.a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertPayloadPrune(t, ValidatePayload(json.RawMessage(tc.schema), tc.payload), tc.detail, tc.field)
		})
	}

	merged := json.RawMessage(`{"properties":{"a":{"type":"string"}},"required":["c","b","a"]}`)
	for i := 0; i < 32; i++ {
		assertPayloadPrune(t, ValidatePayload(merged, map[string]any{}), "载荷缺 a, b, c", "payload.a")
	}
}

func TestDeclaredRef(t *testing.T) {
	cases := []struct {
		name string
		next []string
		to   string
		want string
		ok   bool
	}{
		{name: "bare declared follows latest", next: []string{"fixer"}, to: "fixer", want: "fixer", ok: true},
		{name: "explicit pinned needs exact declaration", next: []string{"fixer"}, to: "fixer@3", ok: false},
		{name: "bare to hits unique pinned declaration", next: []string{"review@2"}, to: "review", want: "review@2", ok: true},
		{name: "explicit pinned exact match", next: []string{"review@2"}, to: "review@2", want: "review@2", ok: true},
		{name: "explicit pinned other version", next: []string{"review@2"}, to: "review@1", ok: false},
		{name: "other name", next: []string{"fixer"}, to: "other", ok: false},
		{name: "empty target", next: []string{"fixer"}, to: "", ok: false},
		{name: "ambiguous multiple pinned versions", next: []string{"review@2", "review@3"}, to: "review", ok: false},
		{name: "ambiguous explicit still exact", next: []string{"review@2", "review@3"}, to: "review@3", want: "review@3", ok: true},
		{name: "ambiguous bare plus pinned", next: []string{"review", "review@2"}, to: "review", ok: false},
		{name: "duplicate identical declaration is unique", next: []string{"review@2", "review@2"}, to: "review", want: "review@2", ok: true},
		{name: "bad pinned never matches", next: []string{"review@nope"}, to: "review", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := declaredRef(tc.next, tc.to)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("declaredRef(%v, %q) = %q,%v，want %q,%v", tc.next, tc.to, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDecodePayloadRejectsNonObject(t *testing.T) {
	if _, ok := DecodePayload(json.RawMessage(`[1,2]`)); ok {
		t.Fatal("数组不是合法 params 对象")
	}
	if _, ok := DecodePayload(json.RawMessage(`"text"`)); ok {
		t.Fatal("字符串不是合法 params 对象")
	}
	if got, ok := DecodePayload(nil); !ok || len(got) != 0 {
		t.Fatalf("nil 载荷应解成空对象，得到 %v/%v", got, ok)
	}
}

// 编译期断言：参数子集使用的剪枝码必须来自共享封闭词表。
var _ = func() bool {
	for _, reason := range []string{
		protocol.ReasonParamMissing, protocol.ReasonParamSchema, protocol.ReasonParamType,
		protocol.ReasonEventSchema,
	} {
		if !protocol.IsPruneReason(reason) {
			panic("route: 非剪枝词表原因码 " + reason)
		}
	}
	_ = context.Background
	_ = artifact.DefaultMaxAttempts
	return true
}

const patternParamsSchema = `{
	"type":"object",
	"properties":{
		"project":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
		"meta":{"type":"object","properties":{"dir":{"type":"string","pattern":"^[a-z]+$"}}},
		"tree":{"type":"object","properties":{
			"root":{"type":"object","properties":{"name":{"type":"string","pattern":"^[a-z]+$"}}}
		}},
		"count":{"type":["integer","string"],"pattern":"^n-[0-9]+$"},
		"note":{"pattern":"^[a-z]+$"}
	}
}`

func TestPrepareParamsPatternAcceptsMatchingValues(t *testing.T) {
	cases := []map[string]any{
		{"project": "a-b1"},
		{"meta": map[string]any{"dir": "abc"}},
		{"tree": map[string]any{"root": map[string]any{"name": "abc"}}},
		{"count": 7},
		{"count": "n-7"},
		{"note": 3}, // pattern 只约束字符串：非 string 值不判
	}
	for i, input := range cases {
		if _, prune := PrepareParams(json.RawMessage(patternParamsSchema), input); prune != nil {
			t.Fatalf("合法输入 #%d %v 被拒: %+v", i, input, prune)
		}
	}
}

func TestPrepareParamsPatternRejectsWithExactField(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		field string
	}{
		{"顶层", map[string]any{"project": "a/b"}, "params.project"},
		{"嵌套", map[string]any{"meta": map[string]any{"dir": "A"}}, "params.meta.dir"},
		{"深嵌套", map[string]any{"tree": map[string]any{"root": map[string]any{"name": "9x"}}}, "params.tree.root.name"},
		{"字符串值的 pattern", map[string]any{"count": "x"}, "params.count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, prune := PrepareParams(json.RawMessage(patternParamsSchema), tc.input)
			if prune == nil {
				t.Fatalf("输入 %v 应被拒", tc.input)
			}
			if prune.Reason != protocol.ReasonParamType {
				t.Fatalf("reason = %q，want param_type", prune.Reason)
			}
			if prune.Field != tc.field {
				t.Fatalf("field = %q，want %q", prune.Field, tc.field)
			}
			if !strings.Contains(prune.Detail, "pattern") || !strings.Contains(prune.Detail, "实际") {
				t.Fatalf("detail 应同时说明期望 pattern 与实际值，得到 %q", prune.Detail)
			}
		})
	}
}

func TestPrepareParamsBadPatternIsSchemaPrune(t *testing.T) {
	long := strings.Repeat("a", schema.MaxPatternLen+1)
	cases := []string{
		`{"type":"object","properties":{"p":{"type":"string","pattern":"["}}}`,
		`{"type":"object","properties":{"p":{"pattern":3}}}`,
		`{"type":"object","properties":{"p":{"type":"string","pattern":"` + long + `"}}}`,
	}
	for i, raw := range cases {
		_, prune := PrepareParams(json.RawMessage(raw), map[string]any{"p": "x"})
		if prune == nil || prune.Reason != protocol.ReasonParamSchema {
			t.Fatalf("坏 pattern #%d 应剪枝 param_schema，得到 %+v", i, prune)
		}
		if !strings.Contains(prune.Detail, "pattern") {
			t.Fatalf("坏 pattern #%d 的 detail 应点名 pattern，得到 %q", i, prune.Detail)
		}
	}
}

// unusableSchemas 是 #612 的 A/B 两组：写入期现在会 400 掉的形状。
var unusableSchemas = []struct {
	name string
	raw  string
}{
	{"顶层 type 不是 object", `{"type":"string"}`},
	{"顶层 type 是数组", `{"type":"array","items":{"type":"string"}}`},
	{"坏 pattern（RE2 编不过）", `{"type":"object","properties":{"repo":{"type":"string","pattern":"(["}}}`},
	{"pattern 不是字符串", `{"type":"object","properties":{"repo":{"type":"string","pattern":7}}}`},
	{"required 不是字符串数组", `{"type":"object","required":7}`},
	{"参数名就叫 pattern（属性值不是对象）", `{"type":"object","properties":{"pattern":3}}`},
	{"参数名以 .pattern 结尾", `{"type":"object","properties":{"x.pattern":3}}`},
	{"属性名就叫 pattern 漏写一层对象", `{"type":"object","properties":{"pattern":"^x$"}}`},
}

// TestRuntimeRejectsUnusableSchemas 是 #612 的运行期一半：这些 schema 写入期已经 400，
// 但**存量行**仍可能带着它们（升级前落库的）。运行期必须继续按剪枝处理——放行会让
// 形状未知的载荷进系统，panic/500 会把「配置写错了」报成「服务器坏了」。
func TestRuntimeRejectsUnusableSchemas(t *testing.T) {
	for _, tc := range unusableSchemas {
		t.Run("capability/"+tc.name, func(t *testing.T) {
			_, prune := PrepareParams(json.RawMessage(tc.raw), map[string]any{})
			if prune == nil || prune.Reason != protocol.ReasonParamSchema {
				t.Fatalf("应剪枝 param_schema，得到 %+v", prune)
			}
			if strings.TrimSpace(prune.Detail) == "" {
				t.Fatal("剪枝 detail 必须说清原因（否则只能猜）")
			}
		})
		t.Run("source/"+tc.name, func(t *testing.T) {
			prune := ValidatePayload(json.RawMessage(tc.raw), map[string]any{})
			if prune == nil || prune.Reason != protocol.ReasonEventSchema {
				t.Fatalf("应剪枝 event_schema，得到 %+v", prune)
			}
			if !strings.Contains(prune.Detail, "payloadSchema 不可用") {
				t.Fatalf("detail 必须点明是 payloadSchema 不可用，得到 %q", prune.Detail)
			}
		})
	}
}
