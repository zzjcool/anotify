package playbook

// issue #356 补口：**actual 侧按能力名取并集**。
//
// `sourcespec.SameTargets` 明确把 target 顺序当**非语义**（顺序不同但内容相同 = 等价），
// 所以库里旧行的顺序与包里声明合法地不同。如果 actual 侧按**位置**绑定 declared 的敏感集，
// 就会把 A target 的敏感集套到 B target 上：B 里其他 key 名的 writeOnly 明文照旧进回执。
//
// 本文件钉住三件事：
//  1. 同名多 target + 库顺序相反 → 两侧每个 target 都拿到**该能力名下全部敏感 key 的并集**；
//  2. 能力名对不上（旧包已删）→ 退到该源全部敏感集的并集（保守兜底）；
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

// TestPlanUnappliedActualNameMismatchFallsBackToUnion 钉住「能力名对不上 → 全局并集」。
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
	for _, key := range []string{"token", "secret"} {
		if got := actual[0].Params[key]; got != MaskedValue {
			t.Fatalf("名字对不上时 actual.params[%s] 应走并集兜底打码: %#v", key, got)
		}
	}
	if got := actual[0].Params["project"]; got != "a" {
		t.Fatalf("并集兜底不得把非敏感 project 打码: %#v", got)
	}
}
