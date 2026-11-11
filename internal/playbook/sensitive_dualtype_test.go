package playbook

// issue #356 收口：同一 schema 节点**同时带 `properties` 与 `items`**（`type: [object, array]`，
// 或对象节点上多写了一个 `items`）时的下钻优先级。
//
// 下钻必须**先 `properties`、再「纯数字下标 + items/prefixItems」**：真实值形态由
// `properties` 决定（`maskValue` 对 map 也走 properties）。反序（items 抢在 properties 前）
// 会让 `{{values.dual.pin}}` 被判「路径不在 schema」→ 非敏感 → 明文漏进回执 `unapplied[]`。
// 两条用例从 209e3d2 移植，适配本分支的多候选口径（`descendSchemaCandidates`）。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// TestDescendSchemaPrefersPropertiesOverItems 钉住下钻优先级：同一节点同时带 properties 与
// items 时，键名命中 properties 必须走对象分支；只有「properties 没有该键 + 纯数字段 +
// items/prefixItems 存在」才按数组元素下钻；properties 没有 + 非数字段 → 返回空（不落 items）。
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
		name string
		sch  map[string]any
		seg  string
		want []map[string]any
	}{
		{"properties 命中（writeOnly）优先于 items", dual, "pin", []map[string]any{pinSchema}},
		{"properties 命中（普通）优先于 items", dual, "tag", []map[string]any{tagSchema}},
		{"properties 没这个键 + 纯数字段 → items", dual, "0", []map[string]any{itemsSchema}},
		{"properties 没这个键 + 非数字段 → 不落 items", dual, "nope", nil},
		{"纯数组节点 + 数字段 → items", map[string]any{"type": "array", "items": itemsSchema}, "3", []map[string]any{itemsSchema}},
		{"纯数组节点 + 非数字段 → 不落 items", map[string]any{"type": "array", "items": itemsSchema}, "pin", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := descendSchemaCandidates(tc.sch, tc.seg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("descendSchemaCandidates(%q) = %#v, want %#v", tc.seg, got, tc.want)
			}
		})
	}

	// 路径级：properties 命中 writeOnly → 敏感；命中的是普通子孙 / 路径不存在 → 不敏感。
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

// TestPlanUnappliedMaskDualTypedNodePrefersProperties 是上一条的端到端版本：
// reviewer 实测的 `type: ["object","array"]` + properties 里 writeOnly 的形状，
// `{{values.dual.pin}}` 的 declared/actual 两侧都必须打码；同一节点上的普通子孙
// （`dual.tag`）与非敏感字面量（`fixedNumber`）必须保留明文。
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
