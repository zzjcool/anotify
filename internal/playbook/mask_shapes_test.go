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
	"encoding/json"
	"strings"
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
//
// 历史教训：#338/F8 写下这份清单时没人消费它，staticcheck 把它判成 unused —— 于是这条
// 「回执里不许出现明文」的断言**从来没跑过**（一个恒真的校验等于没有校验）。现在它在
// `TestPlanUnappliedMaskAdditionalPropertiesDerivedParams` 第 ⑤ 步被真扫一遍，并且带
// 反向自检（明文必须真的进过待打码的原始数据，否则扫描本身是空断言）。
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

	// ⑤ `maskShapesSecrets` 的用处就在这里：整份**回执 JSON**（values + unapplied.declared /
	// actual + notes）里一个 writeOnly 明文都不许出现。这条断言以前是死的（清单没人消费，
	// staticcheck 判 unused），现在两边都盯：
	//   - 正向：清单里的每个明文都不在回执里；
	//   - 反向：每个明文确实在「打码前」的数据里（新值在执行事实 PlannedSource.Spec、
	//     旧值在库里旧行），否则上面的扫描是空断言。
	receiptJSON, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "i-f8", At: ts}))
	if err != nil {
		t.Fatalf("序列化回执失败: %v", err)
	}
	unmaskedFacts, err := json.Marshal(planned.Spec)
	if err != nil {
		t.Fatalf("序列化执行事实失败: %v", err)
	}
	oldTargets, err := json.Marshal(oldSrc.Spec.Targets)
	if err != nil {
		t.Fatalf("序列化旧行 targets 失败: %v", err)
	}
	for _, secret := range maskShapesSecrets {
		if strings.Contains(string(receiptJSON), secret) {
			t.Fatalf("回执里出现 writeOnly 明文 %q（values / unapplied.declared/actual / notes 任一字段）：%s",
				secret, receiptJSON)
		}
		if !strings.Contains(string(unmaskedFacts), secret) && !strings.Contains(string(oldTargets), secret) {
			t.Fatalf("语料自检失败：明文 %q 根本没进过打码前的数据，上面的扫描是空断言", secret)
		}
	}
}

// ---------- GAP-1：无法证明的关键字 ⇒ fail-closed 整值打码 ----------

// 背景（安全性质的 fail-open）：`spec.values` 用同文档 `$ref`（`#/$defs/X` / `#/definitions/X`）
// 间接声明 `writeOnly` 时，打码 walker 判「不敏感」，明文进回执 `values`。
//
// 采纳的修法：**fail-closed 整值打码**，不解析 `$ref`。判据是「节点上只要出现无法证明不含
// `writeOnly` 的关键字，就按『这里可能藏着 writeOnly』处理」（`cannotProveNoWriteOnly`）。
//
// 本节的语料是**测试自己写死**的关键字清单（不迭代产品侧的 `maskUnprovableKeywords`）：
// 产品把某个关键字从判据里去掉，这里立刻红。三个 walker 必须同判：
// `MaskValues`（回执 `values`）/ `refHitsWriteOnly`（param 链路）/ `subtreeHasWriteOnly`
// （祖先整块替换）——只改一处就会重演 #356 的裂缝。

// TestMaskUnprovableKeywordsMaskWholeValue 逐条验「关键字 ⇒ 整值打码」。
//
// 每条同时断言四件事（向量固定：`properties.hidden` 带该关键字、`properties.plain` 是普通叶）：
//  1. `MaskValues` 把 `hidden` **整值**换成 `***`；
//  2. `refHitsWriteOnly("hidden")` 与 `refHitsWriteOnly("hidden.pin")` 都判敏感；
//  3. `subtreeHasWriteOnly` 对该节点判真（祖先整块替换那一档）；
//  4. 同层普通叶 `plain` 不被误伤（多打码只在这个节点上，不往外扩散一整棵树）。
func TestMaskUnprovableKeywordsMaskWholeValue(t *testing.T) {
	// 每条：关键字 + 一个真实形状的取值 + 为什么它「藏得住 writeOnly」。
	cases := []struct {
		keyword string
		val     any
		why     string
	}{
		{"$ref", "#/$defs/Credential", "writeOnly 可能写在被引用节点里，不解引用就看不见"},
		{"$dynamicRef", "#Credential", "目标要到求值期按动态作用域才确定"},
		{"$recursiveRef", "#", "2019-09 的递归引用，同类求值关键字"},
		{"allOf", []any{map[string]any{"type": "object"}}, "组合分支可能带 writeOnly"},
		{"anyOf", []any{map[string]any{"type": "object"}}, "组合分支可能带 writeOnly"},
		{"oneOf", []any{map[string]any{"type": "object"}}, "组合分支可能带 writeOnly"},
		{"not", map[string]any{"type": "null"}, "取反的判定要跑一遍子 schema"},
		{"if", map[string]any{"type": "object"}, "条件分支可能带 writeOnly"},
		{"then", map[string]any{"type": "object"}, "条件分支可能带 writeOnly"},
		{"else", map[string]any{"type": "object"}, "条件分支可能带 writeOnly"},
		{"patternProperties", map[string]any{"^p": map[string]any{"type": "string"}}, "按键名匹配，walker 不读模式里的子 schema"},
		{"propertyNames", map[string]any{"pattern": "^[a-z]+$"}, "键名 schema：本仓子集不认识"},
		{"additionalItems", map[string]any{"type": "string"}, "draft-07 元组尾巴承载子 schema"},
		{"unevaluatedProperties", false, "判据本身是求值结果"},
		{"unevaluatedItems", false, "判据本身是求值结果"},
		{"dependentSchemas", map[string]any{"a": map[string]any{"type": "object"}}, "依赖触发的子 schema"},
		{"dependencies", map[string]any{"a": map[string]any{"type": "object"}}, "draft-07 依赖写法"},
		{"contains", map[string]any{"type": "string"}, "元素子 schema，walker 不读"},
		{"minContains", 1, "与 contains 一起才生效"},
		{"maxContains", 3, "与 contains 一起才生效"},
	}
	if len(cases) == 0 {
		t.Fatal("关键字语料为空：这条用例会退化成空断言")
	}
	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			node := map[string]any{tc.keyword: tc.val}
			sch := map[string]any{
				"type": "object",
				"properties": map[string]any{
					"hidden": node,
					"plain":  map[string]any{"type": "string"},
				},
			}
			values := map[string]any{
				"hidden": map[string]any{"pin": "kw-secret-" + tc.keyword},
				"plain":  "keep-me",
			}

			masked := MaskValues(sch, values)
			if got := masked["hidden"]; got != MaskedValue {
				t.Fatalf("`%s` 节点没被整值打码（%s）：%#v", tc.keyword, tc.why, got)
			}
			if got := masked["plain"]; got != "keep-me" {
				t.Fatalf("`%s` 同层普通叶被误伤：%#v", tc.keyword, got)
			}
			if !refHitsWriteOnly("hidden", sch) {
				t.Fatalf("refHitsWriteOnly(hidden) = false（`%s`：%s）：param 链路会漏明文", tc.keyword, tc.why)
			}
			if !refHitsWriteOnly("hidden.pin", sch) {
				t.Fatalf("refHitsWriteOnly(hidden.pin) = false（`%s`：%s）：param 链路会漏明文", tc.keyword, tc.why)
			}
			if refHitsWriteOnly("plain", sch) {
				t.Fatalf("refHitsWriteOnly(plain) = true（`%s` 误伤了同层普通叶）", tc.keyword)
			}
			if !subtreeHasWriteOnly(node) {
				t.Fatalf("subtreeHasWriteOnly(`%s` 节点) = false：祖先整块替换那一档会漏，节点=%#v",
					tc.keyword, node)
			}
		})
	}
}

// TestMaskProvableKeywordsDoNotOverMask 是反误伤：注解 / 纯值断言 / 定义容器**不得**触发整值
// 打码。
//
// 为什么这条必要：fail-closed 只允许往「多打码」方向偏，但把所有普通 schema 都打成 `***` 会让
// 回执失去可用性（`values` 是给人看「这次装的是哪一组值」的）。边界就钉在这里：
//   - 注解（`title` / `description` / `default` / `format` / `example`…）不承载子 schema；
//   - 纯值断言（`pattern` / `enum` / `maxLength` / `uniqueItems`…）只约束标量；
//   - 定义容器（`$defs` / `definitions`）本身不生效——只有 `$ref` 才危险。
func TestMaskProvableKeywordsDoNotOverMask(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type": "string", "title": "Project", "description": "slug",
				"format": "uri-reference", "pattern": "^[a-z][a-z0-9-]*$",
				"default": "a", "example": "a", "deprecated": false,
				"contentEncoding": "utf-8", "contentMediaType": "text/plain",
				"$comment": "no secret here",
			},
			"tags": map[string]any{
				"type": "array", "uniqueItems": true, "minItems": 1, "maxItems": 4,
				"items": map[string]any{"type": "string", "maxLength": 8, "minLength": 1},
			},
			"replicas": map[string]any{"type": "integer", "minimum": 1, "maximum": 9, "multipleOf": 1},
			// 定义容器本身不生效：`$defs` 里藏着 writeOnly 但没有 `$ref` 引用它 ⇒ 不该打码。
			"labels": map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"Unused": map[string]any{"type": "string", "writeOnly": true},
				},
				"definitions": map[string]any{
					"Unused7": map[string]any{"type": "string", "writeOnly": true},
				},
				"properties": map[string]any{"env": map[string]any{"type": "string", "enum": []any{"a", "b"}}},
			},
		},
	}
	values := map[string]any{
		"project":  "proj-1",
		"tags":     []any{"one", "two"},
		"replicas": 2,
		"labels":   map[string]any{"env": "a"},
	}
	masked := MaskValues(sch, values)
	if got := mustJSON(t, masked); got != mustJSON(t, values) {
		t.Fatalf("可证明的关键字触发了多余打码（反误伤）：\n got=%s\nwant=%s", got, mustJSON(t, values))
	}
	for _, ref := range []string{"project", "tags", "tags.0", "replicas", "labels", "labels.env"} {
		if refHitsWriteOnly(ref, sch) {
			t.Fatalf("refHitsWriteOnly(%q) = true：可证明的关键字被误判为敏感", ref)
		}
	}
	// 反向自检：把那个「藏着 writeOnly 的定义」真的引用一次，同一份 schema 立刻整值打码——
	// 说明上面「不打码」的原因是「没有 `$ref`」，不是判据瞎了。
	refNode, _ := sch["properties"].(map[string]any)["labels"].(map[string]any)
	if subtreeHasWriteOnly(refNode) {
		t.Fatalf("未引用的 `$defs` 定义影响了打码：%#v", refNode)
	}
	withRef := map[string]any{
		"type":       "object",
		"properties": map[string]any{"labels": map[string]any{"$ref": "#/$defs/Used"}},
		"$defs":      map[string]any{"Used": map[string]any{"type": "string", "writeOnly": true}},
	}
	if got := MaskValues(withRef, values)["labels"]; got != "***" {
		t.Fatalf("同一份定义被 `$ref` 引用后仍没打码：%#v", got)
	}
}

// TestMaskUnprovableKeywordTableMatchesPredicate 把产品侧的关键字表与判据对账。
//
// 为什么需要：`maskUnprovableKeywords` 是**理由表**（每个关键字为什么藏得住 writeOnly），
// `maskProvableKeywords` 是**豁免表**。判据 =「不在豁免表里 ⇒ 无法证明」。两张表若重叠或
// 理由表里的关键字其实被豁免，就会出现「表里写了、判据没接」这种写了等于没写。
func TestMaskUnprovableKeywordTableMatchesPredicate(t *testing.T) {
	if len(maskUnprovableKeywords) == 0 {
		t.Fatal("危险关键字表为空：fail-closed 的语义面被清空了")
	}
	if len(maskProvableKeywords) == 0 {
		t.Fatal("豁免关键字表为空：所有 schema 都会被整值打码")
	}
	// 契约（docs/design/v4/playbook.md §1.2 的 fail-closed note）逐字点名了下面这些关键字。
	// 判据是 allowlist 语义（不在豁免表即打码），所以**从理由表删掉一行不会让任何用例变红**——
	// 「文档承诺了、判据没写理由」就会静默发生。这里把文档点到名的关键字钉死。
	for _, kw := range []string{
		"$ref", "$dynamicRef", "$recursiveRef", "if", "then", "else", "not",
		"patternProperties", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "dependentSchemas",
	} {
		if _, ok := maskUnprovableKeywords[kw]; !ok {
			t.Fatalf("契约点名要 fail-closed 的关键字 %q 在理由表里没有条目（删表不会让别的用例变红，所以在这里钉住）", kw)
		}
	}
	for keyword, why := range maskUnprovableKeywords {
		if why == "" {
			t.Fatalf("关键字 %q 没写理由：审查者无法判断它为什么危险", keyword)
		}
		if maskProvableKeywords[keyword] {
			t.Fatalf("关键字 %q 同时出现在两张表里（豁免表说安全、危险表说危险）", keyword)
		}
		if !cannotProveNoWriteOnly(map[string]any{keyword: map[string]any{"type": "string"}}) {
			t.Fatalf("关键字 %q 在危险表里但判据判假（「表里写了、判据没接」）", keyword)
		}
	}
	// 豁免表里的每个关键字也自证一遍：单独出现时判据必须判假（否则豁免表是文档，不是判据）。
	// 注意要用**它认识的形态**：`items` / `properties` 这类结构关键字进豁免表，是因为
	// walker 会读它们的对象形态；换成别的形态就不在豁免面内（见 TestMaskDraft07TupleItemsMaskWholeValue）。
	for keyword := range maskProvableKeywords {
		if cannotProveNoWriteOnly(map[string]any{keyword: provableSampleValue(keyword)}) {
			t.Fatalf("关键字 %q 在豁免表里但判据判真", keyword)
		}
	}
}

// provableSampleValue 是豁免表自证用例里用的「该关键字认识的形态」。
//
// 默认给 `true`（注解/纯值断言的值不参与判定），结构关键字给 walker 真会走的那个形状。
func provableSampleValue(keyword string) any {
	switch keyword {
	case "properties":
		return map[string]any{"k": map[string]any{"type": "string"}}
	case "items":
		return map[string]any{"type": "string"}
	case "prefixItems":
		return []any{map[string]any{"type": "string"}}
	case "additionalProperties":
		return map[string]any{"type": "string"}
	}
	return true
}

// TestMaskDraft07TupleItemsMaskWholeValue 盯同一类 fail-open 的另一个入口：**关键字名认识、
// 值形态不认识**。
//
// draft-07 的 `items` 可以是 schema 列表（元组写法），而打码 walker 只读对象/布尔的 `items`
// （2020-12 口径：元组走 `prefixItems`）。列表形态里的 writeOnly 看不见，所以按无法证明处理：
// 整个数组值打码（而不是逐位置打码）——方向仍然是多打码。
func TestMaskDraft07TupleItemsMaskWholeValue(t *testing.T) {
	tuple := map[string]any{
		"type": "array",
		"items": []any{
			map[string]any{"type": "string", "writeOnly": true},
			map[string]any{"type": "string"},
		},
	}
	sch := map[string]any{"type": "object", "properties": map[string]any{"pair": tuple}}
	values := map[string]any{"pair": []any{"draft07-secret", "keep-me"}}

	if got := MaskValues(sch, values)["pair"]; got != MaskedValue {
		t.Fatalf("draft-07 元组写法（`items: [schema]`）没被整值打码（列表形态里的 writeOnly 会漏）：%#v", got)
	}
	if !refHitsWriteOnly("pair", sch) || !refHitsWriteOnly("pair.0", sch) {
		t.Fatal("refHitsWriteOnly 对 draft-07 元组写法判不敏感：param 链路会漏明文")
	}
	if !subtreeHasWriteOnly(tuple) {
		t.Fatal("subtreeHasWriteOnly 对 draft-07 元组写法判假：祖先整块替换那一档会漏")
	}

	// 反误伤对照：2020-12 的对象形态 `items` 仍然**逐元素**打码（不是整值替换），
	// 非 writeOnly 的兄弟元素保留——证明上面那一条修的是「形状不认识」，不是把数组一律打成 `***`。
	objItems := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"list": map[string]any{"type": "array", "items": map[string]any{"type": "string", "writeOnly": true}},
		},
	}
	got, ok := MaskValues(objItems, map[string]any{"list": []any{"a", "b"}})["list"].([]any)
	if !ok || len(got) != 2 || got[0] != MaskedValue || got[1] != MaskedValue {
		t.Fatalf("对象形态 `items` 的逐元素打码行为变了（回归）：%#v", MaskValues(objItems, map[string]any{"list": []any{"a", "b"}}))
	}
}
