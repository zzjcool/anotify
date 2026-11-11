package playbook

// issue #356 补口：**祖先继承**。引用一个「装着 writeOnly 叶的容器」时（整对象 / 整数组 /
// 整个数组元素），插值会把 writeOnly 明文整块带进回执 `unapplied[].declared/actual`，
// 所以敏感性必须在**最终解析节点**上判整棵子树（对齐前端 mock 的
// `pbPolicySecret(pbReferencePolicy(schema, path))`）。
//
// 反向红线同样重要：**下钻途中的中间节点不判子树**——root 的子树几乎必然含 writeOnly，
// 若在那里判，`{{values.project}}` 这类普通引用会被全量打码（过度打码 = 偏离 mock 语义）。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// ancestorSchemaDoc 是覆盖两条祖先形态的 spec.values：
//   - `auth`：对象祖先（writeOnly 叶 `token` 藏在 `properties` 里，旁边有普通叶 `label`）；
//   - `credList`：数组祖先（writeOnly 叶在 `items.properties` 里）；
//   - `project`：有默认值的普通字段（证明非敏感引用不被误伤）。
const ancestorSchemaDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      auth:
        type: object
        additionalProperties: false
        properties:
          token: { type: string, writeOnly: true }
          label: { type: string }
      credList:
        type: array
        items:
          type: object
          additionalProperties: false
          properties:
            token: { type: string, writeOnly: true }
            note:  { type: string }
`

// ancestorSourceName 是测试源名（一个源，一个 target）。
const ancestorSourceName = "a-on-push"

// ancestorSourceDoc 的 params 精确复刻 e2e f3 的四类祖先引用 + 一个非敏感兄弟叶
// + 两个非敏感字面量：
//   - authObj / authList / authItem：整对象 / 整数组 / 整个数组元素（本轮修复的三条）；
//   - mixed：引用 writeOnly **叶**（既有行为，不得退化）；
//   - label：解析到 auth 下的普通叶（不得过度打码）。
const ancestorSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params:
        authObj: '{{values.auth}}'
        authList: '{{values.credList}}'
        authItem: '{{values.credList.0}}'
        label: '{{values.auth.label}}'
        mixed: 'secret={{values.auth.token}}'
        fixedNumber: 77
        fixedBoolean: true
`

// 两轮安装的 writeOnly 明文（可全文搜索的唯一常量）；label 是**非敏感叶**，两轮也不同，
// 用来证明它两侧都保留原值。
const (
	ancestorOldToken    = "anc-obj-token-old-9f31c2"
	ancestorNewToken    = "anc-obj-token-new-7d40ab"
	ancestorOldArrToken = "anc-arr-token-old-5b19e8"
	ancestorNewArrToken = "anc-arr-token-new-2c66f1"
	ancestorOldLabel    = "anc-label-old-q1"
	ancestorNewLabel    = "anc-label-new-q2"
)

// ancestorSecrets 是回执全文里**绝不允许出现**的 writeOnly 明文（新旧两侧）。
var ancestorSecrets = []string{
	ancestorOldToken, ancestorNewToken, ancestorOldArrToken, ancestorNewArrToken,
}

// ancestorOldValues / ancestorNewValues 是两次安装的 values（writeOnly 值不同 → targets 差异）。
func ancestorOldValues() map[string]any {
	return map[string]any{
		"auth":     map[string]any{"token": ancestorOldToken, "label": ancestorOldLabel},
		"credList": []any{map[string]any{"token": ancestorOldArrToken, "note": "old-note"}},
	}
}

func ancestorNewValues() map[string]any {
	return map[string]any{
		"auth":     map[string]any{"token": ancestorNewToken, "label": ancestorNewLabel},
		"credList": []any{map[string]any{"token": ancestorNewArrToken, "note": "new-note"}},
	}
}

// ancestorSchema 用 Go 字面量重建 ancestorSchemaDoc（单测直接调敏感性函数，不经 YAML）。
func ancestorSchema() map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	writeOnlyStr := func() map[string]any {
		return map[string]any{"type": "string", "writeOnly": true}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "default": "a"},
			"auth": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"token": writeOnlyStr(),
					"label": str(),
				},
			},
			"credList": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"token": writeOnlyStr(),
						"note":  str(),
					},
				},
			},
			// prefixItems 是元组写法，语义与 items 同一条（子树含 writeOnly 即敏感）。
			"prefixList": map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"pin": writeOnlyStr(),
						},
					},
				},
			},
		},
	}
}

// ancestorBundle 组出与 f3 同形的包（一个能力 + 一个带祖先引用的源）。
func ancestorBundle(t *testing.T) *Package {
	t.Helper()
	return newBundle().
		setValues(ancestorSchemaDoc).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", ancestorSourceDoc).
		pkg(t)
}

// TestRefHitsWriteOnlyAncestorInheritance 是敏感性推导的语义表（逐条对齐 mock）。
func TestRefHitsWriteOnlyAncestorInheritance(t *testing.T) {
	sch := ancestorSchema()

	cases := []struct {
		name string
		ref  string
		want bool
	}{
		// 祖先继承（本轮修复的三条）。
		{"对象祖先", "auth", true},
		{"数组祖先", "credList", true},
		{"数组元素祖先", "credList.0", true},
		// 既有行为：解析到 writeOnly 叶（下钻途中命中），不得退化。
		{"writeOnly 叶", "credList.0.token", true},
		{"writeOnly 叶（对象内）", "auth.token", true},
		// prefixItems 元组：整数组引用落在数组节点上，其 prefixItems 子树含 writeOnly。
		// （逐下标 `prefixList.0` 不走：`descendSchema` 只认 `items`，与 `maskValue` 同源。）
		{"元组数组祖先", "prefixList", true},
		// 不过度打码：普通叶 / 普通字段 / 不存在的路径。
		{"普通兄弟叶", "auth.label", false},
		{"数组元素普通叶", "credList.0.note", false},
		{"普通字段", "project", false},
		{"不存在的路径", "missing.path", false},
		{"前缀命中但末端不存在", "auth.nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refHitsWriteOnly(tc.ref, sch); got != tc.want {
				t.Fatalf("refHitsWriteOnly(%q) = %v，期望 %v", tc.ref, got, tc.want)
			}
		})
	}

	// 反证「中间节点不判子树」：root 子树当然含 writeOnly，但普通引用不得因此变敏感。
	if !subtreeHasWriteOnly(sch) {
		t.Fatalf("前提不成立：root 子树应含 writeOnly")
	}
	if refHitsWriteOnly("project", sch) {
		t.Fatalf("root 子树含 writeOnly 不应传染给 {{values.project}}")
	}
	if !refHitsWriteOnly("auth", sch) {
		t.Fatalf("auth 子树含 writeOnly，整对象引用必须敏感")
	}
}

// TestValueReferencesWriteOnlyMixedStringAncestor 覆盖**混合串**里的祖先引用：
// 渲染阶段本来会 `render_type_mismatch`（对象拼字符串），但推导必须保守为真。
func TestValueReferencesWriteOnlyMixedStringAncestor(t *testing.T) {
	sch := ancestorSchema()

	if !valueReferencesWriteOnly("secret={{values.auth}}", sch) {
		t.Fatalf("混合串引用 auth 祖先应判敏感（保守为真）")
	}
	if !valueReferencesWriteOnly("secret={{values.credList}}", sch) {
		t.Fatalf("混合串引用 credList 祖先应判敏感")
	}
	// 容器形态的 param 值（嵌套 map / 数组）也要递归到字符串叶子。
	if !valueReferencesWriteOnly(map[string]any{"nested": []any{"x={{values.credList.0}}"}}, sch) {
		t.Fatalf("嵌套容器里的祖先引用应判敏感")
	}

	// 反向：非敏感兄弟叶、纯字面量、别的根名、非字符串标量一律不敏感。
	for _, v := range []any{
		"secret={{values.auth.label}}",
		"{{values.project}}",
		"no-reference",
		"{{payload.auth}}",
		"{{values.missing}}",
		77,
		true,
		nil,
	} {
		if valueReferencesWriteOnly(v, sch) {
			t.Fatalf("valueReferencesWriteOnly(%#v) 误判为敏感", v)
		}
	}
}

// TestPlanUnappliedMasksAncestorReferences 是 plan 级回归：两轮 install 走到行 12
// （config 等价、targets 不同），把四类祖先引用的 params 两侧整体打码，且不误伤兄弟叶，
// 执行事实（PlannedSource.Spec / values / valuesDigest）保持不变。
func TestPlanUnappliedMasksAncestorReferences(t *testing.T) {
	pkg := ancestorBundle(t)
	oldValues, newValues := ancestorOldValues(), ancestorNewValues()

	// ① 空库：源 create（明文 targets 进「库」，也是第二次安装的 actual 侧来源）。
	first, err := Plan(pkg, oldValues, newFakeExisting())
	if err != nil {
		t.Fatalf("首次安装计划失败: %v", err)
	}
	if len(first.Created.Sources) != 1 || first.Created.Sources[0] != ancestorSourceName {
		t.Fatalf("首次安装应新建源: %+v", first.Created)
	}
	oldRendered := mustRender(t, pkg, oldValues)
	oldSrc := findSource(oldRendered.Sources, ancestorSourceName)
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 %s: %+v", ancestorSourceName, oldRendered.Sources)
	}
	if len(oldSrc.Spec.Targets) != 1 {
		t.Fatalf("渲染后的 targets = %#v", oldSrc.Spec.Targets)
	}
	// 前提：渲染结果确实带明文（否则下面的打码断言会变成空断言）。
	oldParams := oldSrc.Spec.Targets[0].Params
	if got, ok := oldParams["authObj"].(map[string]any); !ok || got["token"] != ancestorOldToken {
		t.Fatalf("渲染后的 authObj 应带明文 token: %#v", oldParams["authObj"])
	}

	// ② 换一组 values → targets 不同 → 行 12：unapplied，不写库。
	ex := newFakeExisting().withSource(oldSrc.Name, oldSrc.Spec.Kind,
		string(oldSrc.Spec.Config), oldSrc.Spec.Targets...)
	plan, err := Plan(pkg, newValues, ex)
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}

	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	u := plan.Unapplied[0]
	if u.Kind != "source" || u.Ref != ancestorSourceName || u.Field != "targets" ||
		u.Reason != ReasonTargetsUnapplied {
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

	// 不变量 1：四类祖先引用的 params 在 declared / actual 两侧都整体打码。
	for _, key := range []string{"authObj", "authList", "authItem", "mixed"} {
		if got := declared[0].Params[key]; got != MaskedValue {
			t.Fatalf("declared.params[%s] 没打码: %#v", key, got)
		}
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("actual.params[%s] 没打码: %#v", key, got)
		}
	}

	// 不变量 2：非敏感兄弟叶保留明文，且两侧各是各轮的值。
	if got := declared[0].Params["label"]; got != ancestorNewLabel {
		t.Fatalf("declared.params[label] 被误伤: %#v", got)
	}
	if got := actual[0].Params["label"]; got != ancestorOldLabel {
		t.Fatalf("actual.params[label] 被误伤: %#v", got)
	}

	// 不变量 3：执行事实保持明文——计划里的源 spec 是新建源落库要读的那份（未被原地改）。
	planned := findPlannedSource(plan.Sources, ancestorSourceName)
	if planned == nil {
		t.Fatalf("计划里没有 %s: %+v", ancestorSourceName, plan.Sources)
	}
	if len(planned.Spec.Targets) != 1 {
		t.Fatalf("PlannedSource.Spec.Targets = %#v", planned.Spec.Targets)
	}
	plain := planned.Spec.Targets[0].Params
	if got, ok := plain["authObj"].(map[string]any); !ok || got["token"] != ancestorNewToken {
		t.Fatalf("PlannedSource.Spec 的 authObj 被打码/改动: %#v", plain["authObj"])
	}
	if got, ok := plain["authItem"].(map[string]any); !ok || got["token"] != ancestorNewArrToken {
		t.Fatalf("PlannedSource.Spec 的 authItem 被打码/改动: %#v", plain["authItem"])
	}
	list, ok := plain["authList"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("PlannedSource.Spec 的 authList 被打码/改动: %#v", plain["authList"])
	}
	if item, ok := list[0].(map[string]any); !ok || item["token"] != ancestorNewArrToken {
		t.Fatalf("PlannedSource.Spec 的 authList[0] 被打码/改动: %#v", list[0])
	}
	if got := plain["mixed"]; got != "secret="+ancestorNewToken {
		t.Fatalf("PlannedSource.Spec 的 mixed 被打码/改动: %#v", got)
	}

	// 不变量 4：plan.Values / valuesDigest 与 Render 完全一致（打码只发生在回执展示副本上）。
	newRendered := mustRender(t, pkg, newValues)
	if !reflect.DeepEqual(plan.Values, newRendered.Values) {
		t.Fatalf("plan.Values 与 Render 不一致: %#v vs %#v", plan.Values, newRendered.Values)
	}
	if plan.ValuesDigest != newRendered.ValuesDigest {
		t.Fatalf("valuesDigest 与 Render 不一致: %s vs %s", plan.ValuesDigest, newRendered.ValuesDigest)
	}
	if got, ok := plan.Values["auth"].(map[string]any); !ok || got["token"] != ancestorNewToken {
		t.Fatalf("plan.Values 的 writeOnly 值不该被打码（digest 对打码前算）: %#v", plan.Values["auth"])
	}

	// 不变量 5：回执序列化后全文不含任一 writeOnly 明文（新旧两侧）。
	receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_ancestor", At: ts, Namespace: "ns"})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range ancestorSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), MaskedValue) {
		t.Fatalf("回执里应有打码占位 %q: %s", MaskedValue, raw)
	}
	rcptDeclared, ok := receipt.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok {
		t.Fatalf("回执 unapplied.declared = %#v", receipt.Unapplied[0].Declared)
	}
	for _, key := range []string{"authObj", "authList", "authItem", "mixed"} {
		if got := rcptDeclared[0].Params[key]; got != MaskedValue {
			t.Fatalf("回执 unapplied.declared[%s] 没打码: %#v", key, got)
		}
	}

	// 不变量 6：notes 文案不含明文（message 只点名能力名与「有没有 params」）。
	found := false
	for _, note := range plan.Notes {
		if note.Code != NoteTargetsUnapplied || note.Ref != ancestorSourceName {
			continue
		}
		found = true
		for _, secret := range ancestorSecrets {
			if strings.Contains(note.Message, secret) {
				t.Fatalf("note 泄露了明文 %q: %s", secret, note.Message)
			}
		}
	}
	if !found {
		t.Fatalf("targets_unapplied 必须有 note: %+v", plan.Notes)
	}
}

// TestPlanUnappliedKeepsNonSensitiveSiblingLeaf 钉住「不过度打码」：祖先打码不得顺手把
// 同层的普通叶（`{{values.auth.label}}`）与固定字面量一起抹掉，回执里要看得见它们。
func TestPlanUnappliedKeepsNonSensitiveSiblingLeaf(t *testing.T) {
	pkg := ancestorBundle(t)
	oldValues, newValues := ancestorOldValues(), ancestorNewValues()

	oldRendered := mustRender(t, pkg, oldValues)
	oldSrc := findSource(oldRendered.Sources, ancestorSourceName)
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 %s: %+v", ancestorSourceName, oldRendered.Sources)
	}
	ex := newFakeExisting().withSource(oldSrc.Name, oldSrc.Spec.Kind,
		string(oldSrc.Spec.Config), oldSrc.Spec.Targets...)
	plan, err := Plan(pkg, newValues, ex)
	if err != nil {
		t.Fatalf("第二次安装计划失败: %v", err)
	}
	if len(plan.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", plan.Unapplied)
	}
	declared, ok := plan.Unapplied[0].Declared.([]sourcespec.Target)
	if !ok || len(declared) != 1 {
		t.Fatalf("declared targets = %#v", plan.Unapplied[0].Declared)
	}
	actual, ok := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if !ok || len(actual) != 1 {
		t.Fatalf("actual targets = %#v", plan.Unapplied[0].Actual)
	}

	// 普通叶原样保留（declared = 新值，actual = 旧值）。
	if got := declared[0].Params["label"]; got != ancestorNewLabel {
		t.Fatalf("declared.params[label] 被过度打码: %#v", got)
	}
	if got := actual[0].Params["label"]; got != ancestorOldLabel {
		t.Fatalf("actual.params[label] 被过度打码: %#v", got)
	}
	// 固定字面量（非字符串标量）两侧都保留，禁止按值匹配做全局替换。
	// 渲染后的 params 经 YAML→JSON 往返，数字是 float64。
	for _, side := range []struct {
		name string
		tg   []sourcespec.Target
	}{{"declared", declared}, {"actual", actual}} {
		if got := side.tg[0].Params["fixedNumber"]; got != float64(77) {
			t.Fatalf("%s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
		if got := side.tg[0].Params["fixedBoolean"]; got != true {
			t.Fatalf("%s.params[fixedBoolean] 被误伤: %#v", side.name, got)
		}
	}

	// 回执层面：普通叶明文可见、writeOnly 明文不可见——两件事必须同时成立。
	receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_ancestor_sibling", At: ts, Namespace: "ns"})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, label := range []string{ancestorOldLabel, ancestorNewLabel} {
		if !strings.Contains(string(raw), label) {
			t.Fatalf("非敏感叶 %q 不该被打码: %s", label, raw)
		}
	}
	for _, secret := range ancestorSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}
