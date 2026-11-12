package playbook

// issue #356 补口：**actual 侧按能力名取并集**。
//
// `sourcespec.SameTargets` 明确把 target 顺序当**非语义**（顺序不同但内容相同 = 等价），
// 所以库里旧行的顺序与包里声明合法地不同。如果 actual 侧按**位置**绑定 declared 的敏感集，
// 就会把 A target 的敏感集套到 B target 上：B 里其他 key 名的 writeOnly 明文照旧进回执。
//
// 本文件钉住三件事：
//  1. 同名多 target + 库顺序相反 → 两侧每个 target 都拿到**该能力名下全部敏感 key 的并集**；
//  2. 能力名对不上（旧包已删 / 出口改名）→ 该 actual target **整组打码**（fail-closed）；
//  3. 非敏感引用（`{{values.project}}`）端到端保持明文，不被并集打码误伤。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

const actualUnionValuesDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      tok1: { type: string, writeOnly: true }
      tok2: { type: string, writeOnly: true }
`

// actualUnionSourceDoc 是**两条同名 target**（各带一个不同的 writeOnly 派生 param
// + 同一个非敏感引用 project）。
const actualUnionSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        token: '{{values.tok1}}'
        project: '{{values.project}}'
    - capability: a-review
      params:
        secret: '{{values.tok2}}'
        project: '{{values.project}}'
`

const (
	actualUnionOldTok1 = "actual-union-old-tok1"
	actualUnionOldTok2 = "actual-union-old-tok2"
	actualUnionNewTok1 = "actual-union-new-tok1"
	actualUnionNewTok2 = "actual-union-new-tok2"
)

// actualUnionPlan 复刻「库里旧行顺序与包里相反」的合法库状态。
//
// 库里旧行（actual）故意把两条同名 target 反序存：第二条（secret）在前、第一条（token）在后。
// 按位置绑定的实现会把 declared[0] 的 {token} 套到 actual[0]（secret）上 → secret 明文泄漏。
func actualUnionPlan(t *testing.T) *PlanResult {
	t.Helper()
	pkg := newBundle().
		setValues(actualUnionValuesDoc).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", actualUnionSourceDoc).
		pkg(t)
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "a-review", Params: map[string]any{
			"secret": actualUnionOldTok2, "project": "a",
		}},
		sourcespec.Target{Capability: "a-review", Params: map[string]any{
			"token": actualUnionOldTok1, "project": "a",
		}},
	)
	plan, err := Plan(pkg, map[string]any{
		"tok1": actualUnionNewTok1, "tok2": actualUnionNewTok2, "project": "b",
	}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	if plan.Unapplied[0].Reason != ReasonTargetsUnapplied {
		t.Fatalf("unapplied reason = %q", plan.Unapplied[0].Reason)
	}
	return plan
}

// TestPlanUnappliedActualUnionByCapabilityName 钉住「同名并集 + 顺序无关」。
func TestPlanUnappliedActualUnionByCapabilityName(t *testing.T) {
	plan := actualUnionPlan(t)

	declared, ok := plan.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || len(declared) != 2 {
		t.Fatalf("declared targets = %#v", plan.Unapplied[0].Declared)
	}
	actual, ok := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if !ok || len(actual) != 2 {
		t.Fatalf("actual targets = %#v", plan.Unapplied[0].Actual)
	}

	// declared：各自的 writeOnly 派生 param 打码，非敏感 project 保留新值。
	if got := declared[0].Params["token"]; got != MaskedValue {
		t.Fatalf("declared[0].params[token] 没打码: %#v", got)
	}
	if got := declared[1].Params["secret"]; got != MaskedValue {
		t.Fatalf("declared[1].params[secret] 没打码: %#v", got)
	}
	for i := range declared {
		if got := declared[i].Params["project"]; got != "b" {
			t.Fatalf("declared[%d].params[project] 被误伤: %#v", i, got)
		}
	}

	// actual：**每个**同名 target 都拿到并集 {token, secret}——顺序无关。
	// 反序的 actual[0] 带的是 secret，按位置绑定时它只会拿到 {token}，secret 就漏了。
	if got := actual[0].Params["secret"]; got != MaskedValue {
		t.Fatalf("actual[0]（库里的 secret target）没打码：位置绑定会漏: %#v", actual[0].Params)
	}
	if got := actual[1].Params["token"]; got != MaskedValue {
		t.Fatalf("actual[1]（库里的 token target）没打码: %#v", actual[1].Params)
	}
	// 并集不得往 target 里塞它本来没有的 key（打码 ≠ 补键）。
	if _, ok := actual[0].Params["token"]; ok {
		t.Fatalf("actual[0] 本来没有 token，不该被塞进来: %#v", actual[0].Params)
	}
	for i := range actual {
		for key, v := range actual[i].Params {
			if key == "project" {
				if v != "a" {
					t.Fatalf("actual[%d].params[project] 被误伤: %#v", i, v)
				}
				continue
			}
			if v != MaskedValue {
				t.Fatalf("actual[%d].params[%s] 没打码: %#v", i, key, v)
			}
		}
	}

	// 回执全文不含旧 writeOnly 明文（新值也不该出现）。
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_actual_union", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{
		actualUnionOldTok1, actualUnionOldTok2, actualUnionNewTok1, actualUnionNewTok2,
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	// 非敏感引用在回执里可见（declared 新值 / actual 旧值各一份）。
	for _, want := range []string{`"project":"b"`, `"project":"a"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("非敏感引用 %q 不该被打码: %s", want, raw)
		}
	}
}

// TestPlanUnappliedActualNameMismatchFallsBackToUnion 钉住「能力名对不上」这一档。
//
// 函数名保留为历史场景名（`-run` 与 issue 记录要对得上），打码口径已随 review major 修订：
// 不再是「退到该源敏感集并集」（并集为空时会整侧零打码 → fail-open），而是**整组打码**。
func TestPlanUnappliedActualNameMismatchFallsBackToUnion(t *testing.T) {
	pkg := newBundle().
		setValues(actualUnionValuesDoc).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", actualUnionSourceDoc).
		pkg(t)
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "gone-cap", Params: map[string]any{
			"token": actualUnionOldTok1, "secret": actualUnionOldTok2, "project": "a",
		}},
	)
	plan, err := Plan(pkg, map[string]any{
		"tok1": actualUnionNewTok1, "tok2": actualUnionNewTok2, "project": "b",
	}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	actual, ok := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 {
		t.Fatalf("actual targets = %#v", plan.Unapplied[0].Actual)
	}
	if len(actual[0].Params) == 0 {
		t.Fatalf("actual.params 为空，断言会变成空断言: %#v", actual[0])
	}
	// 名字对不上 → 没有 key 名可继承 → 整组打码（含非敏感 project，多打码方向安全）。
	for key := range actual[0].Params {
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("名字对不上时 actual.params[%s] 必须整组打码: %#v", key, got)
		}
	}
	// 回执全文不得含任何一轮的 writeOnly 明文。
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_actual_mismatch", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{
		actualUnionOldTok1, actualUnionOldTok2, actualUnionNewTok1, actualUnionNewTok2,
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	// declared 侧仍有同名能力可对齐：非敏感引用照旧明文（证明不是整侧零渲染）。
	declared, ok := plan.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || len(declared) != 2 {
		t.Fatalf("declared targets = %#v", plan.Unapplied[0].Declared)
	}
	for i := range declared {
		if got := declared[i].Params["project"]; got != "b" {
			t.Fatalf("declared[%d].params[project] 被过度打码: %#v", i, got)
		}
	}
}

// TestPlanUnappliedActualMasksWholeGroupWhenDeclaredRenamesExport 是 review major（fail-open）的
// 具名回归：出口改名后 actual 侧**整组**打码，旧明文零出现。
//
// 复现路径（review 实测）：第一轮出口 `a-review{token: '{{values.tok}}'}`（tok 是 writeOnly），
// 值已明文落库；第二轮把出口换成 `a-fixer`，且新源里已经没有**任何** writeOnly 引用（该源敏感集
// 并集为空）。修复前 `inheritSensitivity` 在「名字对不上」时回退到 union（= 空集）→ `maskTargetAt`
// 因 `!allParams && len(keys)==0` 原样返回 ⇒ actual 整侧零打码，旧明文随回执落库，还能被
// GET/history 读回。
//
// 修复口径：没有任何 declared 同名能力可对齐 → 该 actual target 整组 params 打码
// （`targetSensitivity{allParams:true}`），与 `len(declared)==0` 的兜底同口径。
//
// 多打码方向安全：actual 旧行的非敏感字面量（`note`）会被一并抹掉——库里没有旧 values 的
// provenance，宁可把这一组整个打码，也不允许漏一条 writeOnly 明文。
func TestPlanUnappliedActualMasksWholeGroupWhenDeclaredRenamesExport(t *testing.T) {
	const valuesDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      tok: { type: string, writeOnly: true }
`
	// 第二轮：出口改名 a-fixer，params 只剩非敏感引用 → 该源敏感集并集为空。
	const renamedSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-fixer
      params:
        project: '{{values.project}}'
`
	const fixerCapDoc = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: a-fixer
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/usr/bin/env bash\ntrue\n"
`
	const (
		renamedOldToken = "actual-renamed-old-token"
		renamedOldNote  = "actual-renamed-old-note"
		renamedNewToken = "actual-renamed-new-token"
	)
	pkg := newBundle().
		setValues(valuesDoc).
		cap("capabilities/fixer.yaml", fixerCapDoc).
		source("sources/on-push.yaml", renamedSourceDoc).
		pkg(t)
	// 库里旧行 = 第一轮渲染结果：a-review 的 token 明文已经落库。
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "a-review", Params: map[string]any{
			"token": renamedOldToken, "note": renamedOldNote,
		}},
	)
	plan, err := Plan(pkg, map[string]any{"tok": renamedNewToken, "project": "b"}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 || plan.Unapplied[0].Reason != ReasonTargetsUnapplied {
		t.Fatalf("应有且只有 1 条 targets unapplied: %+v", plan.Unapplied)
	}
	declared, ok := plan.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || len(declared) != 1 || declared[0].Capability != "a-fixer" {
		t.Fatalf("declared targets = %#v", plan.Unapplied[0].Declared)
	}
	actual, ok := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 || actual[0].Capability != "a-review" {
		t.Fatalf("actual targets = %#v", plan.Unapplied[0].Actual)
	}
	// declared 侧没有 writeOnly 引用：保持新值明文（不能把两侧一起抹掉）。
	if got := declared[0].Params["project"]; got != "b" {
		t.Fatalf("declared.params[project] 被过度打码: %#v", got)
	}
	// actual 侧：没有任何同名能力可对齐 → 整组打码（含非敏感 note，多打码方向安全）。
	if len(actual[0].Params) == 0 {
		t.Fatalf("actual.params 为空，断言会变成空断言: %#v", actual[0])
	}
	for key := range actual[0].Params {
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("出口改名后 actual.params[%s] 必须整组打码（修复前是明文 %#v）: %#v",
				key, got, actual[0].Params)
		}
	}
	// 回执全文：旧明文（token / note）与新 writeOnly 明文都不得出现。
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_actual_renamed", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{renamedOldToken, renamedOldNote, renamedNewToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	// 非敏感引用仍可读（declared 侧的新值），证明不是整侧零渲染。
	if !strings.Contains(string(raw), `"project":"b"`) {
		t.Fatalf("declared 侧非敏感引用不该被打码: %s", raw)
	}
}

// TestDescendSchemaPrefixItemsNonObjectFallsBackToItems 钉住 delta review 发现的回归：
// `prefixItems[idx]` 是**布尔 schema**（`true` = 不约束）时，不能就此判死；改前（`0824c47`）
// 同一下标一律走 `items`，改后必须保持一致，否则 `{{values.tuple.0.token}}` 会漏判敏感。
func TestDescendSchemaPrefixItemsNonObjectFallsBackToItems(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tuple": map[string]any{
				"type":        "array",
				"prefixItems": []any{true},
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"token": map[string]any{"type": "string", "writeOnly": true},
					},
				},
			},
		},
	}
	for _, ref := range []string{"tuple.0", "tuple.0.token"} {
		if !refHitsWriteOnly(ref, sch) {
			t.Fatalf("refHitsWriteOnly(%q) = false：布尔 prefixItems 必须回落 items", ref)
		}
	}
	// 布尔 prefixItems 且没有 items：推不出密文，保持非敏感（不猜）。
	noItems := map[string]any{"type": "object", "properties": map[string]any{
		"tuple": map[string]any{"type": "array", "prefixItems": []any{true}},
	}}
	if refHitsWriteOnly("tuple.0.token", noItems) {
		t.Fatalf("没有 items 也没有对象 schema 时不该猜成敏感")
	}
}

// TestPlanUnappliedActualMasksWhenDeclaredRemovesAllTargets 钉住「声明侧 0 个 target」：
// 新包里把源的出口全删了，旧行没有任何 key 名可继承 → 必须整组打码（否则整侧零打码泄漏）。
func TestPlanUnappliedActualMasksWhenDeclaredRemovesAllTargets(t *testing.T) {
	const valuesDoc = `
    type: object
    additionalProperties: false
    properties:
      pin: { type: integer, writeOnly: true }
`
	const emptyTargetsDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets: []
`
	pkg := newBundle().setValues(valuesDoc).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", emptyTargetsDoc).
		pkg(t)
	const oldPin = 313131
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "a-review", Params: map[string]any{"pin": oldPin, "note": "old-note"}},
	)
	plan, err := Plan(pkg, map[string]any{"pin": 424243}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	declared, ok := plan.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || len(declared) != 0 {
		t.Fatalf("declared 应为空: %#v", plan.Unapplied[0].Declared)
	}
	actual, ok := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 {
		t.Fatalf("actual targets = %#v", plan.Unapplied[0].Actual)
	}
	for key := range actual[0].Params {
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("声明侧清空后 actual.params[%s] 必须整组打码: %#v", key, got)
		}
	}
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_empty_declared", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{"313131", "424243", "old-note"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}

// TestRefHitsWriteOnlyPrefixItemsCandidateUnion 钉住「prefixItems 与 items 两分支并查」：
//
// `MaskValues` 对数组**一律按 items** 打码，所以同一个数组元素在回执 `values` 里可能已被
// 打码（items 声明了 writeOnly），而 `unapplied` 若只信 `prefixItems`（`[{}]` 或普通对象）
// 就会把同一 token 以明文带出来。并查口径保证「values 里是 *** 的，unapplied 里也至少是 ***」。
func TestRefHitsWriteOnlyPrefixItemsCandidateUnion(t *testing.T) {
	cases := []struct {
		name string
		sch  map[string]any
		ref  string
		want bool
	}{
		{
			name: "prefixItems 空对象 + items 有 writeOnly：整元素引用要敏感",
			sch: map[string]any{"type": "object", "properties": map[string]any{
				"tuple": map[string]any{
					"type":        "array",
					"prefixItems": []any{map[string]any{}},
					"items": map[string]any{"type": "object", "properties": map[string]any{
						"token": map[string]any{"type": "string", "writeOnly": true},
					}},
				},
			}},
			ref: "tuple.0", want: true,
		},
		{
			name: "prefixItems 普通对象 + items 有 writeOnly：整元素引用要敏感",
			sch: map[string]any{"type": "object", "properties": map[string]any{
				"tuple": map[string]any{
					"type": "array",
					"prefixItems": []any{map[string]any{"type": "object", "properties": map[string]any{
						"label": map[string]any{"type": "string"},
					}}},
					"items": map[string]any{"type": "object", "properties": map[string]any{
						"token": map[string]any{"type": "string", "writeOnly": true},
					}},
				},
			}},
			ref: "tuple.0", want: true,
		},
		{
			name: "prefixItems 普通对象 + items 有 writeOnly：prefix 自己的普通叶不传染",
			sch: map[string]any{"type": "object", "properties": map[string]any{
				"tuple": map[string]any{
					"type": "array",
					"prefixItems": []any{map[string]any{"type": "object", "properties": map[string]any{
						"label": map[string]any{"type": "string"},
					}}},
					"items": map[string]any{"type": "object", "properties": map[string]any{
						"token": map[string]any{"type": "string", "writeOnly": true},
					}},
				},
			}},
			ref: "tuple.0.label", want: false,
		},
		{
			name: "prefixItems 之外的下标走 items",
			sch: map[string]any{"type": "object", "properties": map[string]any{
				"tuple": map[string]any{
					"type":        "array",
					"prefixItems": []any{map[string]any{}},
					"items": map[string]any{"type": "object", "properties": map[string]any{
						"token": map[string]any{"type": "string", "writeOnly": true},
					}},
				},
			}},
			ref: "tuple.3.token", want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refHitsWriteOnly(tc.ref, tc.sch); got != tc.want {
				t.Fatalf("refHitsWriteOnly(%q) = %v，期望 %v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestMergeSensitivityNilMapGuard 钉住防御性初始化：dst 是零值时写 keys 不能 panic。
func TestMergeSensitivityNilMapGuard(t *testing.T) {
	var dst targetSensitivity
	mergeSensitivity(&dst, targetSensitivity{keys: map[string]bool{"pin": true}})
	if !dst.keys["pin"] {
		t.Fatalf("mergeSensitivity 后 keys = %#v", dst.keys)
	}
	mergeSensitivity(&dst, targetSensitivity{allParams: true})
	if !dst.allParams {
		t.Fatalf("mergeSensitivity 没传播 allParams")
	}
}

// TestPlanUnappliedActualMasksRenamedParam 钉住「同名能力内 param 改名」的 fail-closed：
// 第一轮 `a-review{tokenRef:'{{values.tok}}'}`（tok writeOnly，明文落库）；第二轮把 param
// 改名为 `token`，能力名不变。旧 key `tokenRef` 不在新包的敏感集里——但它同样没有旧 values
// 的 provenance，所以必须打码（含同组被删掉的非敏感 `note`，多打码方向安全）；
// 仍存在于新包里的非敏感 `project` 保留旧值。
func TestPlanUnappliedActualMasksRenamedParam(t *testing.T) {
	const valuesDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      tok: { type: string, writeOnly: true }
`
	const renamedSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        token: '{{values.tok}}'
        project: '{{values.project}}'
`
	const reviewCapDoc = `
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
	const (
		oldTok  = "rename-old-secret"
		oldNote = "rename-old-note"
		newTok  = "rename-new-secret"
	)
	pkg := newBundle().
		setValues(valuesDoc).
		cap("capabilities/review.yaml", reviewCapDoc).
		source("sources/on-push.yaml", renamedSourceDoc).
		pkg(t)
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "a-review", Params: map[string]any{
			"tokenRef": oldTok, "note": oldNote, "project": "a",
		}},
	)
	plan, err := Plan(pkg, map[string]any{"tok": newTok, "project": "b"}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if got := declared[0].Params["token"]; got != MaskedValue {
		t.Fatalf("declared.params[token] 没打码: %#v", got)
	}
	if got := declared[0].Params["project"]; got != "b" {
		t.Fatalf("declared.params[project] 被误伤: %#v", got)
	}
	// 旧 key 在当前包里不存在 → 打码；同组被删的非敏感 note 也打码（多打码安全）。
	if got := actual[0].Params["tokenRef"]; got != MaskedValue {
		t.Fatalf("改名后的旧 key tokenRef 必须打码（修复前是明文 %#v）", got)
	}
	if got := actual[0].Params["note"]; got != MaskedValue {
		t.Fatalf("被删除的旧 key note 必须打码（多打码方向），得到 %#v", got)
	}
	// 仍存在于新包同名 target 的非敏感 key → 保留旧值。
	if got := actual[0].Params["project"]; got != "a" {
		t.Fatalf("actual.params[project] 应保留旧值 a，得到 %#v", got)
	}
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_actual_rename", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{oldTok, oldNote, newTok} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	for _, want := range []string{`"project":"b"`, `"project":"a"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("非敏感 project 不该被打码: %s", raw)
		}
	}
}

// TestPlanUnappliedActualMasksRenamedParamWhenNoSensitiveKeys 钉住「新包同名能力已无任何
// writeOnly 引用、但旧行仍带着改名前的 writeOnly 派生值」这一档：
// 第二轮 params 只剩非敏感 project（敏感 key 集为空），旧 key `tokenRef` 既不在 keys、
// 也不在 knownParams → 仍必须打码。按 `len(keys)==0` 早退会在这里漏明文。
func TestPlanUnappliedActualMasksRenamedParamWhenNoSensitiveKeys(t *testing.T) {
	const valuesDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      tok: { type: string, writeOnly: true }
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
        project: '{{values.project}}'
`
	const reviewCapDoc = `
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
	const (
		oldTok  = "rename-empty-keys-old-secret"
		oldNote = "rename-empty-keys-old-note"
	)
	pkg := newBundle().
		setValues(valuesDoc).
		cap("capabilities/review.yaml", reviewCapDoc).
		source("sources/on-push.yaml", sourceDoc).
		pkg(t)
	ex := newFakeExisting().withSource("a-on-push", sourcespec.KindWebhook, `{}`,
		sourcespec.Target{Capability: "a-review", Params: map[string]any{
			"tokenRef": oldTok, "note": oldNote, "project": "a",
		}},
	)
	plan, err := Plan(pkg, map[string]any{"tok": "unused", "project": "b"}, ex)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if got := actual[0].Params["tokenRef"]; got != MaskedValue {
		t.Fatalf("敏感 key 集为空时，改名前的旧 key tokenRef 仍必须打码（得到 %#v）", got)
	}
	if got := actual[0].Params["note"]; got != MaskedValue {
		t.Fatalf("被删除的旧 key note 必须打码（多打码方向），得到 %#v", got)
	}
	if got := actual[0].Params["project"]; got != "a" {
		t.Fatalf("仍存在的非敏感 key project 应保留旧值 a，得到 %#v", got)
	}
	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_actual_rename_empty_keys", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range []string{oldTok, oldNote} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}
