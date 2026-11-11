package playbook

// issue #338 / F8：`writeOnly` 打码与敏感推导必须覆盖 schema 的**全部容器形态**，不能只认
// `properties` / `items`（docs/design/v4/playbook.md:474 无条件承诺「这类字段在回执里是
// `"***"`」）：
//   - 对象里 `properties` 没声明的键 → 回落 `additionalProperties`（任意键）。该 schema
//     自身/子树含 writeOnly 时，回执 `values` 与 `unapplied[].declared/actual` 都不许出现明文；
//   - 数组按位置 → `prefixItems[i]`（元组写法），`items` 仍管其余。
//
// 三条红线（与本文件的反例一起钉死）：
//   - 非 writeOnly 的 `additionalProperties` 行为**零变化**（不多打码、不按值替换）；
//   - 祖先继承口径不变：引用一个「里面装着 writeOnly 叶的容器」仍整体打码；
//   - 执行事实（渲染结果 / PlannedSource.Spec / valuesDigest）保持明文与不变。

import (
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// TestMaskValuesAdditionalPropertiesWriteOnly 是 F8 ①：`additionalProperties:
// {writeOnly: true, type: string}` → **任意键**都打码，`properties` 命中的键按 properties。
func TestMaskValuesAdditionalPropertiesWriteOnly(t *testing.T) {
	sch := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"project": map[string]any{"type": "string"}},
		"additionalProperties": map[string]any{"type": "string", "writeOnly": true},
	}
	values := map[string]any{
		"project": "a",
		"alpha":   "any-key-secret-1",
		"beta":    "any-key-secret-2",
	}
	masked := MaskValues(sch, values)
	if got := masked["project"]; got != "a" {
		t.Fatalf("properties 命中的普通键被误伤: %#v", got)
	}
	for _, key := range []string{"alpha", "beta"} {
		if got := masked[key]; got != MaskedValue {
			t.Fatalf("additionalProperties.writeOnly 的任意键 %q 没打码: %#v", key, got)
		}
	}
	// 打码不能回写调用方。
	if values["alpha"] != "any-key-secret-1" || values["beta"] != "any-key-secret-2" {
		t.Fatalf("MaskValues 改了调用方: %#v", values)
	}
}

// TestMaskValuesAdditionalPropertiesNestedProperties 是 F8 ②：`additionalProperties`
// 自身带 `properties` 时按该 schema **递归**——`pin` 打码、同层 `label` 保留。
func TestMaskValuesAdditionalPropertiesNestedProperties(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"secrets": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pin":   map[string]any{"type": "integer", "writeOnly": true},
						"label": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	values := map[string]any{"secrets": map[string]any{
		"dev1": map[string]any{"pin": 424242, "label": "kept-1"},
		"dev2": map[string]any{"pin": 434343, "label": "kept-2"},
	}}
	masked := MaskValues(sch, values)
	secrets, ok := masked["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets 被打成了非对象: %#v", masked["secrets"])
	}
	for key, label := range map[string]string{"dev1": "kept-1", "dev2": "kept-2"} {
		entry, ok := secrets[key].(map[string]any)
		if !ok {
			t.Fatalf("secrets.%s 被打成了非对象: %#v", key, secrets[key])
		}
		if got := entry["pin"]; got != MaskedValue {
			t.Fatalf("additionalProperties.properties.pin 没打码（%s）: %#v", key, got)
		}
		if got := entry["label"]; got != label {
			t.Fatalf("同一层的普通 label 被误伤（%s）: %#v", key, got)
		}
	}
}

// TestMaskValuesAdditionalPropertiesNotWriteOnlyKept 是 F8 ③ 反例：`additionalProperties`
// 不带 writeOnly（对象 schema / 布尔二态）时行为与改动前一致——一个字节都不许变。
func TestMaskValuesAdditionalPropertiesNotWriteOnlyKept(t *testing.T) {
	cases := []struct {
		name  string
		addl  any
		value map[string]any
	}{
		{
			name: "对象 schema（无 writeOnly）",
			addl: map[string]any{"type": "string"},
			value: map[string]any{
				"extra": "plain-1", "other": "plain-2",
			},
		},
		{
			name:  "布尔 true 不是 schema",
			addl:  true,
			value: map[string]any{"extra": "plain-1"},
		},
		{
			name:  "布尔 false",
			addl:  false,
			value: map[string]any{"extra": "plain-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]any{"project": "a"}
			for k, v := range tc.value {
				values[k] = v
			}
			sch := map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"project": map[string]any{"type": "string"}},
				"additionalProperties": tc.addl,
			}
			masked := MaskValues(sch, values)
			for key, want := range values {
				if got := masked[key]; got != want {
					t.Fatalf("非 writeOnly 的 additionalProperties 误伤了 %q: %#v", key, got)
				}
			}
		})
	}

	// 嵌套一层：additionalProperties schema 里的普通 properties 同样不传染。
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"labels": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type":       "object",
					"properties": map[string]any{"note": map[string]any{"type": "string"}},
				},
			},
		},
	}
	masked := MaskValues(sch, map[string]any{"labels": map[string]any{
		"l1": map[string]any{"note": "keep-me"},
	}})
	note := masked["labels"].(map[string]any)["l1"].(map[string]any)["note"]
	if note != "keep-me" {
		t.Fatalf("非 writeOnly 的嵌套 additionalProperties 误伤: %#v", note)
	}
}

// TestMaskValuesPrefixItemsWriteOnlyTuple 是 F8 ④：元组数组按**位置**打码（`prefixItems[i]`），
// `items` 仍管剩余元素；「prefixItems 只是普通 schema 而 items 带 writeOnly」的歧义形状
// 保持旧口径（整体打码），不留缝。
func TestMaskValuesPrefixItemsWriteOnlyTuple(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"head": map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{"type": "string", "writeOnly": true},
					map[string]any{"type": "string"},
				},
				"items": map[string]any{"type": "string"},
			},
			"tail": map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "string", "writeOnly": true},
				},
				"items": map[string]any{"type": "string"},
			},
		},
	}
	values := map[string]any{
		"head": []any{"tuple-secret-0", "kept-1", "kept-2"},
		"tail": []any{"kept-a", "tuple-secret-1"},
	}
	masked := MaskValues(sch, values)

	head := masked["head"].([]any)
	wantHead := []any{MaskedValue, "kept-1", "kept-2"}
	for i := range wantHead {
		if head[i] != wantHead[i] {
			t.Fatalf("head[%d] = %#v want %#v（prefixItems 元组应按位置打码）", i, head[i], wantHead[i])
		}
	}
	tail := masked["tail"].([]any)
	wantTail := []any{"kept-a", MaskedValue}
	for i := range wantTail {
		if tail[i] != wantTail[i] {
			t.Fatalf("tail[%d] = %#v want %#v", i, tail[i], wantTail[i])
		}
	}

	// 反例：prefixItems 无 writeOnly + items 带 writeOnly → 元素仍整体打码（旧口径不回退）。
	ambiguous := map[string]any{
		"type":        "array",
		"prefixItems": []any{map[string]any{"type": "string"}},
		"items":       map[string]any{"type": "string", "writeOnly": true},
	}
	got := maskValue(ambiguous, []any{"elem-0", "elem-1"}).([]any)
	for i, want := range []any{MaskedValue, MaskedValue} {
		if got[i] != want {
			t.Fatalf("歧义形状 [%d] = %#v want %#v（items.writeOnly 必须整体打码）", i, got[i], want)
		}
	}
}

// TestRefHitsWriteOnlyAdditionalProperties 是 F8 要求 2：`descendSchemaCandidates` 在
// `properties` 未命中时回落 `additionalProperties`（任意键），使 `{{values.obj.someKey}}`
// 也能判敏感；祖先继承（子树判定）继续生效，非 writeOnly 的 additionalProperties 不误伤。
func TestRefHitsWriteOnlyAdditionalProperties(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string"},
			"secrets": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string", "writeOnly": true},
			},
			"envs": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pin":   map[string]any{"type": "integer", "writeOnly": true},
						"label": map[string]any{"type": "string"},
					},
				},
			},
			"plainMap": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
	}
	cases := []struct {
		ref  string
		want bool
		why  string
	}{
		{"secrets", true, "容器祖先的子树含 writeOnly（既有口径）"},
		{"secrets.alpha", true, "properties 未命中 → additionalProperties 任意键"},
		{"secrets.anyOtherKey", true, "任意键都算（键名由安装方自己定）"},
		{"secrets.deep.nested", true, "下钻途中命中 additionalProperties.writeOnly"},
		{"envs", true, "祖先继承：子树里有 writeOnly"},
		{"envs.dev1", true, "任意键 + 子树里 pin 是 writeOnly"},
		{"envs.dev1.pin", true, "任意键 → writeOnly 叶"},
		{"envs.dev1.label", false, "同层普通叶不传染"},
		{"plainMap.anything", false, "非 writeOnly 的 additionalProperties 不误伤"},
		{"project", false, "普通字段"},
		{"nope", false, "路径不在 schema 里"},
	}
	for _, tc := range cases {
		if got := refHitsWriteOnly(tc.ref, sch); got != tc.want {
			t.Errorf("refHitsWriteOnly(%q) = %v want %v（%s）", tc.ref, got, tc.want, tc.why)
		}
	}
}

// maskShapesValuesSchema 把 F8 的四种形态放进一份真实的 spec.values：
// additionalProperties 任意键（含嵌套 properties）+ prefixItems 元组 + 非 writeOnly 的
// additionalProperties（必须不误伤）。
const maskShapesValuesSchema = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      secrets:
        type: object
        additionalProperties: { type: string, writeOnly: true }
      envs:
        type: object
        additionalProperties:
          type: object
          properties:
            pin:   { type: integer, writeOnly: true }
            label: { type: string }
      tuple:
        type: array
        prefixItems:
          - { type: string, writeOnly: true }
          - { type: string }
        items: { type: string }
      plainMap:
        type: object
        additionalProperties: { type: string }
`

// maskShapesSourceDoc 让每个 param 都引到一种形态上；fixedNumber 是不许被误伤的字面量。
const maskShapesSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        byKey: '{{values.secrets.alpha}}'
        byKeyOther: '{{values.secrets.beta}}'
        nestedPin: '{{values.envs.dev1.pin}}'
        nestedLabel: '{{values.envs.dev1.label}}'
        tuplePin: '{{values.tuple.0}}'
        tupleTail: '{{values.tuple.1}}'
        plainExtra: '{{values.plainMap.note}}'
        fixedNumber: 9
`

// maskShapesSecrets 是回执里绝不允许出现的明文（新旧两轮）。
var maskShapesSecrets = []string{
	"ap-secret-old", "ap-secret-new", "bp-secret-old", "bp-secret-new",
	"424242", "434343", "tup-secret-old", "tup-secret-new",
}

// TestPlanUnappliedMaskAdditionalPropertiesDerivedParams 是 F8 的端到端版本：引用
// `additionalProperties` / `prefixItems` 派生的 params 在 unapplied 的 declared/actual
// 两侧都必须整值打码；同一条链上渲染结果与 valuesDigest 保持明文/不变。
func TestPlanUnappliedMaskAdditionalPropertiesDerivedParams(t *testing.T) {
	pkg := newBundle().
		setValues(maskShapesValuesSchema).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", maskShapesSourceDoc).
		pkg(t)

	oldValues := map[string]any{
		"secrets":  map[string]any{"alpha": "ap-secret-old", "beta": "bp-secret-old"},
		"envs":     map[string]any{"dev1": map[string]any{"pin": 424242, "label": "dev1-label"}},
		"tuple":    []any{"tup-secret-old", "tail-kept"},
		"plainMap": map[string]any{"note": "plain-note"},
	}
	newValues := map[string]any{
		"secrets":  map[string]any{"alpha": "ap-secret-new", "beta": "bp-secret-new"},
		"envs":     map[string]any{"dev1": map[string]any{"pin": 434343, "label": "dev1-label"}},
		"tuple":    []any{"tup-secret-new", "tail-kept"},
		"plainMap": map[string]any{"note": "plain-note"},
	}

	// ① 首轮：源 create，明文 targets 落库（第二轮 actual 侧的事实来源）。
	if _, err := Plan(pkg, oldValues, newFakeExisting()); err != nil {
		t.Fatalf("首次安装计划失败: %v", err)
	}
	oldSrc := findSource(mustRender(t, pkg, oldValues).Sources, "a-on-push")
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 a-on-push")
	}

	// ② 换 writeOnly 值再 Plan → targets 差异 → unapplied。
	plan, err := Plan(pkg, newValues, newFakeExisting().
		withSource(oldSrc.Name, oldSrc.Spec.Kind, string(oldSrc.Spec.Config), oldSrc.Spec.Targets...))
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	u := plan.Unapplied[0]
	declared, ok := u.Declared.([]sourcespec.Target)
	if !ok || len(declared) != 1 {
		t.Fatalf("declared targets = %#v", u.Declared)
	}
	actual, ok := u.Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 {
		t.Fatalf("actual targets = %#v", u.Actual)
	}

	// 敏感 param（additionalProperties 任意键 / 嵌套 properties / prefixItems 元组）整体打码。
	for _, key := range []string{"byKey", "byKeyOther", "nestedPin", "tuplePin"} {
		if got := declared[0].Params[key]; got != MaskedValue {
			t.Fatalf("declared.params[%s] 没打码: %#v", key, got)
		}
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("actual.params[%s] 没打码: %#v", key, got)
		}
	}
	// 非敏感项不被误伤：普通叶（nestedLabel）、元组后半（tupleTail）、非 writeOnly 的
	// additionalProperties（plainExtra）、字面量（fixedNumber）。
	for _, side := range []struct {
		name   string
		params map[string]any
	}{
		{"declared", declared[0].Params},
		{"actual", actual[0].Params},
	} {
		if got := side.params["nestedLabel"]; got != "dev1-label" {
			t.Fatalf("%s.params[nestedLabel] 被误伤: %#v", side.name, got)
		}
		if got := side.params["tupleTail"]; got != "tail-kept" {
			t.Fatalf("%s.params[tupleTail] 被误伤: %#v", side.name, got)
		}
		if got := side.params["plainExtra"]; got != "plain-note" {
			t.Fatalf("%s.params[plainExtra] 被误伤: %#v", side.name, got)
		}
		if got := side.params["fixedNumber"]; got != float64(9) {
			t.Fatalf("%s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
	}

	// ③ 回执 values：同一套形态按 schema 打码；digest 仍对**打码前**的值算。
	maskedValues := MaskValues(plan.schema, newValues)
	secrets := maskedValues["secrets"].(map[string]any)
	for _, key := range []string{"alpha", "beta"} {
		if got := secrets[key]; got != MaskedValue {
			t.Fatalf("回执 values.secrets[%s] 没打码: %#v", key, got)
		}
	}
	tuple := maskedValues["tuple"].([]any)
	if tuple[0] != MaskedValue || tuple[1] != "tail-kept" {
		t.Fatalf("回执 values.tuple 打码错: %#v", tuple)
	}
	if got := maskedValues["plainMap"].(map[string]any)["note"]; got != "plain-note" {
		t.Fatalf("回执 values.plainMap 被误伤: %#v", got)
	}
	// digest 仍对**打码前**的有效 values 算（defaults 已补），不是对打码后的值算。
	if plan.ValuesDigest == "" {
		t.Fatal("plan 缺 valuesDigest")
	}
	plainDigest, err := ValuesDigest(plan.Values)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if plan.ValuesDigest != plainDigest {
		t.Fatalf("valuesDigest 应对打码前的值算: %s vs %s", plan.ValuesDigest, plainDigest)
	}
	if got := plan.Values["secrets"].(map[string]any)["alpha"]; got != "ap-secret-new" {
		t.Fatalf("plan.Values 应是打码前的有效 values: %#v", got)
	}
	maskedDigest, err := ValuesDigest(maskedValues)
	if err != nil {
		t.Fatalf("masked digest: %v", err)
	}
	if maskedDigest == plan.ValuesDigest {
		t.Fatalf("valuesDigest 不该等于打码后 values 的 digest（说明打码过早参与了身份）")
	}

	// ④ 执行事实保持明文（打码只作用于回执的展示副本）。
	planned := findPlannedSource(plan.Sources, "a-on-push")
	if planned == nil {
		t.Fatalf("计划里没有 a-on-push")
	}
	plain := planned.Spec.Targets[0].Params
	if plain["byKey"] != "ap-secret-new" || plain["tuplePin"] != "tup-secret-new" {
		t.Fatalf("PlannedSource.Spec 被打码/改动: %#v", plain)
	}
}
