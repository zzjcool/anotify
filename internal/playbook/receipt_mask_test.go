package playbook

// issue #356 / 契约 A1：回执 `unapplied[]` 的 writeOnly values 派生 params 必须在**构造
// UnappliedEntry 时**就整体打码（`MaskedValue`），且执行事实（PlannedSource.Spec /
// values / valuesDigest / notes 文案）零变化。
//
// 场景精确复刻 e2e f3 `playbook-secret-history`：
//  1. 空库 Plan → 源 create，渲染出的 targets 落库（明文，含 writeOnly 派生值）；
//  2. 换一组 writeOnly values 再 Plan → 行 12（config 等价、targets 不同）→ unapplied[]。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// maskValuesSchema 覆盖四种 writeOnly 形态（对象 / 数组 / 整数 / 布尔）：它们的渲染结果
// 分别是 map / []any / int / bool，"再渲染一次打码后的 values"必然 values_invalid。
const maskValuesSchema = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      blob: { type: object, writeOnly: true, properties: { key: { type: string } } }
      list: { type: array, writeOnly: true, items: { type: string } }
      pin:  { type: integer, writeOnly: true }
      flag: { type: boolean, writeOnly: true }
`

// maskCapDoc 是一个不引用 values 的普通能力（源的出口目标）。
const maskCapDoc = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: a-review
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/usr/bin/env bash\ntrue\n"
`

// maskSourceDoc 的 params 里混了四种 writeOnly 派生值 + 混合串 + 两个非敏感字面量
// （fixedNumber / fixedBoolean 必须**不被误伤**）。
const maskSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        blob: '{{values.blob}}'
        list: '{{values.list}}'
        pin: '{{values.pin}}'
        flag: '{{values.flag}}'
        mixed: 'secret={{values.pin}}'
        fixedNumber: 77
        fixedBoolean: true
`

// maskSensitiveKeys 是必须整体打码的 param key（对象 / 数组 / 整数 / 布尔 / 混合串）。
var maskSensitiveKeys = []string{"blob", "list", "pin", "flag", "mixed"}

// maskOldValues / maskNewValues 是两次安装的 values（writeOnly 值不同 → targets 差异）。
func maskOldValues() map[string]any {
	return map[string]any{
		"blob": map[string]any{"key": "obj-secret-old"},
		"list": []any{"arr-secret-old"},
		"pin":  987654321,
		"flag": true,
	}
}

func maskNewValues() map[string]any {
	return map[string]any{
		"blob": map[string]any{"key": "obj-secret-new"},
		"list": []any{"arr-secret-new"},
		"pin":  987654322,
		"flag": false,
	}
}

// maskSecrets 是回执里**绝不允许出现**的明文（新旧 writeOnly 真实值）。
var maskSecrets = []string{
	"obj-secret-old", "obj-secret-new", "arr-secret-old", "arr-secret-new",
	"987654321", "987654322",
}

// maskBundle 组出 f3 同形的包（一个能力 + 一个带 writeOnly 派生 params 的源）。
func maskBundle(t *testing.T) *Package {
	t.Helper()
	return newBundle().
		setValues(maskValuesSchema).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", maskSourceDoc).
		pkg(t)
}

// TestPlanUnappliedTargetsMaskWriteOnly 是契约 A1 的全部断言点。
func TestPlanUnappliedTargetsMaskWriteOnly(t *testing.T) {
	pkg := maskBundle(t)

	// ① 空库：源 create（这一步把明文 targets 写进"库"，也是第二次安装的 actual 侧来源）。
	first, err := Plan(pkg, maskOldValues(), newFakeExisting())
	if err != nil {
		t.Fatalf("首次安装计划失败: %v", err)
	}
	if len(first.Created.Sources) != 1 || first.Created.Sources[0] != "a-on-push" {
		t.Fatalf("首次安装应新建源: %+v", first.Created)
	}
	oldRendered := mustRender(t, pkg, maskOldValues())
	pkgSrc := findSource(oldRendered.Sources, "a-on-push")
	if pkgSrc == nil {
		t.Fatalf("渲染结果里没有 a-on-push: %+v", oldRendered.Sources)
	}
	if got, ok := pkgSrc.Spec.Targets[0].Params["blob"].(map[string]any); !ok || got["key"] != "obj-secret-old" {
		t.Fatalf("渲染后的 targets 应带明文 writeOnly 派生值: %#v", pkgSrc.Spec.Targets)
	}

	// ② 同名源/config 等价但 targets 不同（writeOnly 换了值）→ 行 12：unapplied，不写库。
	ex := newFakeExisting().withSource(pkgSrc.Name, pkgSrc.Spec.Kind,
		string(pkgSrc.Spec.Config), pkgSrc.Spec.Targets...)
	plan, err := Plan(pkg, maskNewValues(), ex)
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}

	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	u := plan.Unapplied[0]
	if u.Kind != "source" || u.Ref != "a-on-push" || u.Field != "targets" || u.Reason != ReasonTargetsUnapplied {
		t.Fatalf("unapplied 定位不对: %+v", u)
	}
	declared, ok := u.Declared.([]sourcespec.Target)
	if !ok || len(declared) != 1 || declared[0].Capability != "a-review" {
		t.Fatalf("declared targets = %#v", u.Declared)
	}
	actual, ok := u.Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 || actual[0].Capability != "a-review" {
		t.Fatalf("actual targets = %#v", u.Actual)
	}

	// 不变量 1：declared / actual 两侧的敏感 param 全部整值打码（含对象/数组/整数/布尔/混合串）。
	for _, key := range maskSensitiveKeys {
		if got := declared[0].Params[key]; got != MaskedValue {
			t.Fatalf("declared.params[%s] 没打码: %#v", key, got)
		}
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("actual.params[%s] 没打码: %#v", key, got)
		}
	}
	// 不变量 2：非敏感字面量不被误伤（禁止按值相等做全局替换）。
	// 渲染后的 params 经过 YAML→JSON 往返，数字是 float64（比较用 jsonNumber）。
	if got := declared[0].Params["fixedNumber"]; got != float64(77) {
		t.Fatalf("declared.params[fixedNumber] 被误伤: %#v", got)
	}
	if got := declared[0].Params["fixedBoolean"]; got != true {
		t.Fatalf("declared.params[fixedBoolean] 被误伤: %#v", got)
	}
	if got := actual[0].Params["fixedNumber"]; got != float64(77) {
		t.Fatalf("actual.params[fixedNumber] 被误伤: %#v", got)
	}
	if got := actual[0].Params["fixedBoolean"]; got != true {
		t.Fatalf("actual.params[fixedBoolean] 被误伤: %#v", got)
	}

	// 不变量 3：执行事实保持明文——计划里的源 spec 是新建源落库要读的那份。
	planned := findPlannedSource(plan.Sources, "a-on-push")
	if planned == nil {
		t.Fatalf("计划里没有 a-on-push: %+v", plan.Sources)
	}
	if len(planned.Spec.Targets) != 1 {
		t.Fatalf("PlannedSource.Spec.Targets = %#v", planned.Spec.Targets)
	}
	plain := planned.Spec.Targets[0].Params
	blob, ok := plain["blob"].(map[string]any)
	if !ok || blob["key"] != "obj-secret-new" {
		t.Fatalf("PlannedSource.Spec 的 blob 被打码/改动: %#v", plain["blob"])
	}
	if !reflect.DeepEqual(plain["list"], []any{"arr-secret-new"}) {
		t.Fatalf("PlannedSource.Spec 的 list 被打码/改动: %#v", plain["list"])
	}
	if plain["pin"] != float64(987654322) {
		t.Fatalf("PlannedSource.Spec 的 pin 被打码/改动: %#v", plain["pin"])
	}
	if plain["flag"] != false {
		t.Fatalf("PlannedSource.Spec 的 flag 被打码/改动: %#v", plain["flag"])
	}
	if plain["mixed"] != "secret=987654322" {
		t.Fatalf("PlannedSource.Spec 的 mixed 被打码/改动: %#v", plain["mixed"])
	}

	// 不变量 4：plan.Values / valuesDigest 与 Render 完全一致，且 values 仍是明文（S5）。
	newRendered := mustRender(t, pkg, maskNewValues())
	if !reflect.DeepEqual(plan.Values, newRendered.Values) {
		t.Fatalf("plan.Values 与 Render 不一致: %#v vs %#v", plan.Values, newRendered.Values)
	}
	if plan.ValuesDigest != newRendered.ValuesDigest {
		t.Fatalf("valuesDigest 与 Render 不一致: %s vs %s", plan.ValuesDigest, newRendered.ValuesDigest)
	}
	if plan.Values["pin"] != 987654322 {
		t.Fatalf("plan.Values 的 writeOnly 值不该被打码（digest 对打码前算）: %#v", plan.Values["pin"])
	}

	// 不变量 5：回执序列化后全文不含新旧明文，且 unapplied 里是 "***"。
	receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_mask", At: ts, Namespace: "ns"})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range maskSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), MaskedValue) {
		t.Fatalf("回执里应有打码占位 %q: %s", MaskedValue, raw)
	}
	if len(receipt.Unapplied) != 1 {
		t.Fatalf("回执 unapplied = %+v", receipt.Unapplied)
	}
	rcptDeclared, ok := receipt.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || rcptDeclared[0].Params["pin"] != MaskedValue {
		t.Fatalf("回执 unapplied.declared 没打码: %#v", receipt.Unapplied[0].Declared)
	}

	// 不变量 6：notes 文案不含明文（message 只点名能力名与「有没有 params」）。
	foundNote := false
	for _, note := range plan.Notes {
		if note.Code != NoteTargetsUnapplied || note.Ref != "a-on-push" {
			continue
		}
		foundNote = true
		for _, secret := range maskSecrets {
			if strings.Contains(note.Message, secret) {
				t.Fatalf("note 泄露了明文 %q: %s", secret, note.Message)
			}
		}
	}
	if !foundNote {
		t.Fatalf("targets_unapplied 必须有 note: %+v", plan.Notes)
	}
}

// findPlannedSource 按名字找计划里的源执行指令。
func findPlannedSource(srcs []PlannedSource, name string) *PlannedSource {
	for i := range srcs {
		if srcs[i].Name == name {
			return &srcs[i]
		}
	}
	return nil
}
