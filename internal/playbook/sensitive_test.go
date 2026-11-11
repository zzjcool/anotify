package playbook

// issue #356 第二轮 review：打码入口与 schema 下钻的单元级回归。
//
// 这一组用例直接打两处内部机制（同包，unexported 可测）：
//   - `maskedUnappliedTargets`：declared/actual 的对齐算法（必修 3）与 fail-closed 分支（必修 4）；
//   - `descendSchema` / `refHitsWriteOnly`：items 与 properties 的优先级（必修 2）。
//
// 断言落在「哪些 param 被打码 / 哪些必须保留」上——打码是回执的展示口径，多打码可以接受，
// 漏打码是缺陷，所以每个 case 都同时钉住「敏感的全 *** 」与「非敏感的原值保留」。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// cloneTargets 深拷贝 targets（用来证明打码入口**不原地改**入参）。
func cloneTargets(targets []sourcespec.Target) []sourcespec.Target {
	out := make([]sourcespec.Target, len(targets))
	for i, t := range targets {
		out[i] = t
		if t.Params == nil {
			continue
		}
		params := make(map[string]any, len(t.Params))
		for k, v := range t.Params {
			params[k] = deepCopyValue(v)
		}
		out[i].Params = params
	}
	return out
}

// targetParams 把一层的 params 归一成非 nil map（比较用）。
func targetParams(t sourcespec.Target) map[string]any {
	if t.Params == nil {
		return map[string]any{}
	}
	return t.Params
}

// TestPlanUnappliedActualMaskAlignment 钉住必修 3（同名不唯一时不许按位置继承）与必修 4
// （结构错位 fail-closed）：直接打 `maskedUnappliedTargets` 的 table 驱动。
func TestPlanUnappliedActualMaskAlignment(t *testing.T) {
	pin := map[string]bool{"pin": true}
	num := map[string]bool{"num": true}
	other := map[string]bool{"other": true}

	cases := []struct {
		name         string
		declared     []sourcespec.Target
		actual       []sourcespec.Target
		keys         []map[string]bool
		wantDeclared []map[string]any
		wantActual   []map[string]any
	}{
		{
			name:         "① 同下标同名 → 用该下标的敏感集",
			declared:     []sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "new"}}},
			actual:       []sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "old", "keep": "x"}}},
			keys:         []map[string]bool{pin},
			wantDeclared: []map[string]any{{"pin": MaskedValue}},
			wantActual:   []map[string]any{{"pin": MaskedValue, "keep": "x"}},
		},
		{
			name: "② declared/actual 顺序颠倒（两个不同能力名）→ 按名字配对",
			declared: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{"pin": "new"}},
				{Capability: "b-cap", Params: map[string]any{"num": 3}},
			},
			actual: []sourcespec.Target{
				{Capability: "b-cap", Params: map[string]any{"num": 9}},
				{Capability: "a-cap", Params: map[string]any{"pin": "old"}},
			},
			keys:         []map[string]bool{pin, num},
			wantDeclared: []map[string]any{{"pin": MaskedValue}, {"num": MaskedValue}},
			wantActual:   []map[string]any{{"num": MaskedValue}, {"pin": MaskedValue}},
		},
		{
			name: "③ actual 的能力名不在 declared 里 → 并集兜底（多打码）",
			declared: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{"pin": "new"}},
				{Capability: "b-cap", Params: map[string]any{"num": 3}},
			},
			actual:       []sourcespec.Target{{Capability: "gone-cap", Params: map[string]any{"pin": "old", "num": 7, "other": "o"}}},
			keys:         []map[string]bool{pin, num},
			wantDeclared: []map[string]any{{"pin": MaskedValue}, {"num": MaskedValue}},
			wantActual:   []map[string]any{{"pin": MaskedValue, "num": MaskedValue, "other": "o"}},
		},
		{
			name: "④ 同名重复 + 顺序颠倒 → 并集兜底（旧实现会按位置拿到空 key 集）",
			declared: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{}},
				{Capability: "a-cap", Params: map[string]any{"pin": "new"}},
			},
			actual:       []sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "old"}}},
			keys:         []map[string]bool{{}, pin},
			wantDeclared: []map[string]any{{}, {"pin": MaskedValue}},
			wantActual:   []map[string]any{{"pin": MaskedValue}},
		},
		{
			name: "④b 同名重复（两个都带不同 key）→ 并集，两个 key 一起打码",
			declared: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{"pin": "new", "keep": "k"}},
				{Capability: "a-cap", Params: map[string]any{"other": "o"}},
			},
			actual: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{"other": "oo"}},
				{Capability: "a-cap", Params: map[string]any{"pin": "old"}},
			},
			keys:         []map[string]bool{pin, other},
			wantDeclared: []map[string]any{{"pin": MaskedValue, "keep": "k"}, {"other": MaskedValue}},
			wantActual:   []map[string]any{{"other": MaskedValue}, {"pin": MaskedValue}},
		},
		{
			name: "⑤ len(keys) != len(declared)（不可达的防御分支）→ 两侧全部 params 打码",
			declared: []sourcespec.Target{
				{Capability: "a-cap", Params: map[string]any{"pin": "new"}},
				{Capability: "b-cap", Params: map[string]any{"num": 3}},
			},
			actual:       []sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "old", "fixed": 7}}},
			keys:         []map[string]bool{pin},
			wantDeclared: []map[string]any{{"pin": MaskedValue}, {"num": MaskedValue}},
			wantActual:   []map[string]any{{"pin": MaskedValue, "fixed": MaskedValue}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declaredIn := cloneTargets(tc.declared)
			actualIn := cloneTargets(tc.actual)

			gotDeclared, gotActual := maskedUnappliedTargets(tc.declared, tc.actual, tc.keys)

			if len(gotDeclared) != len(tc.wantDeclared) {
				t.Fatalf("declared 长度 = %d, want %d", len(gotDeclared), len(tc.wantDeclared))
			}
			for i := range tc.wantDeclared {
				if got := targetParams(gotDeclared[i]); !reflect.DeepEqual(got, tc.wantDeclared[i]) {
					t.Fatalf("declared[%d].params = %#v, want %#v", i, got, tc.wantDeclared[i])
				}
			}
			if len(gotActual) != len(tc.wantActual) {
				t.Fatalf("actual 长度 = %d, want %d", len(gotActual), len(tc.wantActual))
			}
			for i := range tc.wantActual {
				if got := targetParams(gotActual[i]); !reflect.DeepEqual(got, tc.wantActual[i]) {
					t.Fatalf("actual[%d].params = %#v, want %#v", i, got, tc.wantActual[i])
				}
			}

			// 不变量：绝不原地改入参（PlannedSource.Spec / 库快照必须继续带明文）。
			if !reflect.DeepEqual(tc.declared, declaredIn) {
				t.Fatalf("declared 入参被原地改了: %#v → %#v", declaredIn, tc.declared)
			}
			if !reflect.DeepEqual(tc.actual, actualIn) {
				t.Fatalf("actual 入参被原地改了: %#v → %#v", actualIn, tc.actual)
			}
		})
	}
}

// TestDescendSchemaPrefersPropertiesOverItems 钉住必修 2：同一节点同时带 properties 与
// items 时，键名命中 properties 必须走对象分支；只有「properties 没有 + 纯数字段 + items
// 存在」才按数组元素下钻。
func TestDescendSchemaPrefersPropertiesOverItems(t *testing.T) {
	pinSchema := map[string]any{"type": "integer", "writeOnly": true}
	tagSchema := map[string]any{"type": "string"}
	itemsSchema := map[string]any{"type": "integer"}
	dual := map[string]any{
		"type":       []any{"object", "array"},
		"properties": map[string]any{"pin": pinSchema, "tag": tagSchema},
		"items":      itemsSchema,
	}

	cases := []struct {
		name  string
		sch   map[string]any
		seg   string
		want  map[string]any
		found bool
	}{
		{"properties 命中（writeOnly）优先于 items", dual, "pin", pinSchema, true},
		{"properties 命中（普通）优先于 items", dual, "tag", tagSchema, true},
		{"properties 没这个键 + 纯数字段 → items", dual, "0", itemsSchema, true},
		{"properties 没这个键 + 非数字段 → 不落 items", dual, "nope", nil, false},
		{"纯数组节点 + 数字段 → items", map[string]any{"type": "array", "items": itemsSchema}, "3", itemsSchema, true},
		{"纯数组节点 + 非数字段 → 不落 items", map[string]any{"type": "array", "items": itemsSchema}, "pin", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := descendSchema(tc.sch, tc.seg)
			if ok != tc.found {
				t.Fatalf("descendSchema(%q) found = %v, want %v", tc.seg, ok, tc.found)
			}
			if !tc.found {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("descendSchema(%q) = %#v, want %#v", tc.seg, got, tc.want)
			}
		})
	}

	// 路径级：properties 命中 writeOnly → 敏感；命中的是普通子孙 → 不敏感。
	root := map[string]any{"type": "object", "properties": map[string]any{"dual": dual}}
	if !refHitsWriteOnly("dual.pin", root) {
		t.Fatalf("dual.pin 应判定为敏感（properties 命中 writeOnly，不得被 items 抢走）")
	}
	if refHitsWriteOnly("dual.tag", root) {
		t.Fatalf("dual.tag 不该判定为敏感")
	}
	if refHitsWriteOnly("dual.nope", root) {
		t.Fatalf("路径不在 schema 里时应判非敏感")
	}
}

// TestPlanUnappliedMaskDualTypedNodePrefersProperties 是必修 2 的端到端版本：
// reviewer 实测的 `type: ["object","array"]` + properties 里 writeOnly 的形状，
// `{{values.dual.pin}}` 必须打码；同一节点上的普通子孙必须保留明文。
func TestPlanUnappliedMaskDualTypedNodePrefersProperties(t *testing.T) {
	const valuesSchema = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      dual:
        type: [object, array]
        properties:
          pin: { type: integer, writeOnly: true }
          tag: { type: string }
        items: { type: integer }
`
	const sourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        dualPin: '{{values.dual.pin}}'
        dualTag: '{{values.dual.tag}}'
        fixedNumber: 9
`
	pkg := newBundle().
		setValues(valuesSchema).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", sourceDoc).
		pkg(t)

	oldValues := map[string]any{"dual": map[string]any{"pin": 6660001, "tag": "tag-alpha"}}
	newValues := map[string]any{"dual": map[string]any{"pin": 7770002, "tag": "tag-beta"}}

	if _, err := Plan(pkg, oldValues, newFakeExisting()); err != nil {
		t.Fatalf("首次安装计划失败: %v", err)
	}
	oldSrc := findSource(mustRender(t, pkg, oldValues).Sources, "a-on-push")
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 a-on-push")
	}
	plan, err := Plan(pkg, newValues, newFakeExisting().
		withSource(oldSrc.Name, oldSrc.Spec.Kind, string(oldSrc.Spec.Config), oldSrc.Spec.Targets...))
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有 1 条 unapplied: %+v", plan.Unapplied)
	}
	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)

	if got := declared[0].Params["dualPin"]; got != MaskedValue {
		t.Fatalf("declared.params[dualPin]（type:[object,array] 节点的 properties.writeOnly）没打码: %#v", got)
	}
	if got := declared[0].Params["dualTag"]; got != "tag-beta" {
		t.Fatalf("declared.params[dualTag] 不该被打码: %#v", got)
	}
	if got := declared[0].Params["fixedNumber"]; got != float64(9) {
		t.Fatalf("declared.params[fixedNumber] 被误伤: %#v", got)
	}
	if got := actual[0].Params["dualPin"]; got != MaskedValue {
		t.Fatalf("actual.params[dualPin] 没打码: %#v", got)
	}
	if got := actual[0].Params["dualTag"]; got != "tag-alpha" {
		t.Fatalf("actual.params[dualTag] 不该被打码: %#v", got)
	}

	receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_dual", At: ts, Namespace: "ns"})
	dual, ok := receipt.Values["dual"].(map[string]any)
	if !ok || dual["pin"] != MaskedValue || dual["tag"] != "tag-beta" {
		t.Fatalf("回执 values.dual 打码口径不对: %#v", receipt.Values["dual"])
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{"6660001", "7770002"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执泄露了明文 %q: %s", secret, raw)
		}
	}
}

// ---------- 负向用例（reviewer must-add）：不该打码的一个都不许打 ----------

// maskPlainValuesSchema 里三个字段刻意成对：不含 writeOnly 子孙的对象/数组，以及一个含
// writeOnly 叶的对象。用来证明 `subtreeWriteOnly` **没有**粗化成「有 properties 就打码」。
const maskPlainValuesSchema = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      plainObj:
        type: object
        properties:
          a: { type: string }
      plainArr:
        type: array
        items: { type: string }
      secObj:
        type: object
        properties:
          tok: { type: string, writeOnly: true }
`

// maskPlainSourceDoc 同时引用：整对象（无 writeOnly 叶）、整数组（无 writeOnly 叶）、
// 普通叶子、以及带 writeOnly 叶的整对象。
const maskPlainSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        plainWhole: '{{values.plainObj}}'
        plainArrWhole: '{{values.plainArr}}'
        plainLeaf: '{{values.plainObj.a}}'
        secWhole: '{{values.secObj}}'
        fixedNumber: 3
`

// maskPlainValues 组一次安装的 values（只有 secObj.tok 是密文）。
func maskPlainValues(leaf, item, token string) map[string]any {
	return map[string]any{
		"plainObj": map[string]any{"a": leaf},
		"plainArr": []any{item},
		"secObj":   map[string]any{"tok": token},
	}
}

// TestPlanUnappliedMaskKeepsNonWriteOnlyWholeObject 是打码方向的**负向**回归（review 必补项）：
// 整对象/整数组引用只有在子树里真的声明了 writeOnly 时才整值 `***`；子树干净的必须原样保留。
//
// 钉住三件事：
//  1. `plainWhole`（`{{values.plainObj}}`，对象有 properties 但无 writeOnly 叶）**保留明文**——
//     证明 `subtreeWriteOnly` 不是「有 properties 就打码」；
//  2. `plainArrWhole`（`{{values.plainArr}}`，数组无 writeOnly items）与 `plainLeaf`
//     （`{{values.plainObj.a}}`）同样保留明文；
//  3. `secWhole`（`{{values.secObj}}`，子树里有 writeOnly 叶）是 `MaskedValue`，且回执 JSON 里
//     `plainObj` 的明文仍在、`secObj.tok` 的明文不在。
func TestPlanUnappliedMaskKeepsNonWriteOnlyWholeObject(t *testing.T) {
	pkg := newBundle().
		setValues(maskPlainValuesSchema).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", maskPlainSourceDoc).
		pkg(t)

	oldValues := maskPlainValues("plain-a-old", "plain-item-old", "tok-alpha-secret")
	newValues := maskPlainValues("plain-a-new", "plain-item-new", "tok-beta-secret")

	// 第一次：源 create（明文 targets 落库 → 第二次的 actual 侧）。
	if _, err := Plan(pkg, oldValues, newFakeExisting()); err != nil {
		t.Fatalf("首次安装计划失败: %v", err)
	}
	oldSrc := findSource(mustRender(t, pkg, oldValues).Sources, "a-on-push")
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 a-on-push")
	}

	// 第二次：只换 values（含 writeOnly）→ targets 差异 → 行 12。
	plan, err := Plan(pkg, newValues, newFakeExisting().
		withSource(oldSrc.Name, oldSrc.Spec.Kind, string(oldSrc.Spec.Config), oldSrc.Spec.Targets...))
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有 1 条 unapplied: %+v", plan.Unapplied)
	}
	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if len(declared) != 1 || len(actual) != 1 {
		t.Fatalf("target 数不对: declared=%#v actual=%#v", declared, actual)
	}

	for _, side := range []struct {
		name string
		got  map[string]any
		leaf string
		item string
	}{
		{"declared", declared[0].Params, "plain-a-new", "plain-item-new"},
		{"actual", actual[0].Params, "plain-a-old", "plain-item-old"},
	} {
		if got, ok := side.got["plainWhole"].(map[string]any); !ok || got["a"] != side.leaf {
			t.Fatalf("%s.params[plainWhole]（无 writeOnly 子孙的整对象）被误伤: %#v", side.name, side.got["plainWhole"])
		}
		if got, ok := side.got["plainArrWhole"].([]any); !ok || !reflect.DeepEqual(got, []any{side.item}) {
			t.Fatalf("%s.params[plainArrWhole]（无 writeOnly 子孙的整数组）被误伤: %#v", side.name, side.got["plainArrWhole"])
		}
		if got := side.got["plainLeaf"]; got != side.leaf {
			t.Fatalf("%s.params[plainLeaf] 被误伤: %#v", side.name, got)
		}
		if got := side.got["secWhole"]; got != MaskedValue {
			t.Fatalf("%s.params[secWhole]（子树里有 writeOnly 叶）没打码: %#v", side.name, got)
		}
		if got := side.got["fixedNumber"]; got != float64(3) {
			t.Fatalf("%s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
	}

	// 回执 JSON：普通整对象/整数组的明文仍在（不误伤），密文不在。
	receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_plain", At: ts, Namespace: "ns"})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, want := range []string{"plain-a-new", "plain-item-new", "plain-a-old", "plain-item-old"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("回执里应保留明文 %q（无 writeOnly 子孙，不许打码）: %s", want, raw)
		}
	}
	for _, secret := range []string{"tok-alpha-secret", "tok-beta-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执泄露了 writeOnly 明文 %q: %s", secret, raw)
		}
	}
}

// ---------- 守卫直测（reviewer optional）：maskActualTargets 的短 keys fail-closed ----------

// TestMaskActualTargetsShortKeysFailsClosed 把 `maskActualTargets` 里那条「从唯一入口
// `maskedUnappliedTargets` 不可达」的长度守卫变成**有效且有测试**：keys 比 declared 短时
// 必须 fail-closed（actual 每个 target 的所有 params 都 `MaskedValue`），既不许 panic，
// 也不许原样放行，更不许只打一部分。
func TestMaskActualTargetsShortKeysFailsClosed(t *testing.T) {
	declared := []sourcespec.Target{
		{Capability: "a-cap", Params: map[string]any{"pin": "new"}},
		{Capability: "a-cap@2", Params: map[string]any{"num": 3}},
	}
	actual := []sourcespec.Target{
		{Capability: "a-cap", Params: map[string]any{"pin": "old", "fixed": 7}},
		{Capability: "gone-cap", Params: map[string]any{"whatever": "x"}},
	}
	// len(keys)=1 < len(declared)=2。
	got := maskActualTargets(declared, actual, []map[string]bool{{"pin": true}})

	if len(got) != len(actual) {
		t.Fatalf("actual 长度 = %d, want %d", len(got), len(actual))
	}
	for i := range got {
		if len(got[i].Params) != len(actual[i].Params) {
			t.Fatalf("actual[%d].params 键集合被改了: %#v", i, got[i].Params)
		}
		for k, v := range got[i].Params {
			if v != MaskedValue {
				t.Fatalf("actual[%d].params[%s] 应 fail-closed 打码，实际 %#v", i, k, v)
			}
		}
	}

	// 守卫同样不能原地改入参。
	if actual[0].Params["pin"] != "old" || actual[0].Params["fixed"] != 7 || actual[1].Params["whatever"] != "x" {
		t.Fatalf("actual 入参被原地改了: %#v", actual)
	}

	// keys 比 declared **长**也是「不等长」，同一守卫必须同样 fail-closed。
	longer := maskActualTargets(
		[]sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "new"}}},
		[]sourcespec.Target{{Capability: "a-cap", Params: map[string]any{"pin": "old"}}},
		[]map[string]bool{{"pin": true}, {"num": true}},
	)
	if got := longer[0].Params["pin"]; got != MaskedValue {
		t.Fatalf("keys 更长时 actual.params[pin] 应 fail-closed 打码，实际 %#v", got)
	}
}
