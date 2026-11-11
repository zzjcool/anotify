package playbook

// issue #356 补口：**整块模板**（raw 形状推不出 param key）。
//
// `sensitive.go` 的精确档按 raw `spec.targets[i].params[key]` 推导敏感 key。但源文档允许把
// **整块** `targets` / **整块** `params` / **整个 target 项**写成一条整值模板：
//
//	targets: '{{values.tlist}}'
//	params:  '{{values.pobj}}'
//	targets: [ '{{values.tobj}}' ]
//
// 这几种写法渲染后都是合法源文档（`decodeSourceWire` 只要求渲染后形状合法），但 raw 侧
// 没有任何 param key 可对——精确档会推出空集，于是 `unapplied[].declared/actual` 把整块
// writeOnly 明文原样带进回执（修复前实测泄漏）。契约要求「只许多打码、不允许漏」，所以
// 这一档必须 fail-closed：只要整块引用了 writeOnly values，就把该 target 的 params 整组打码。
//
// 反向红线：整块模板引用的是**普通** values 时不得打码（否则用户看不出真实差异）。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// shapeValuesDoc 覆盖三种「整块模板」来源：writeOnly 的对象 / 数组 / target 项。
const shapeValuesDoc = `
    type: object
    additionalProperties: false
    properties:
      project: { type: string, default: a }
      pobj:
        type: object
        writeOnly: true
        properties:
          pin: { type: integer }
          label: { type: string }
      tlist:
        type: array
        writeOnly: true
        items:
          type: object
          properties:
            capability: { type: string }
            params: { type: object }
      tobj:
        type: object
        writeOnly: true
        properties:
          capability: { type: string }
          params: { type: object }
      tplain:
        type: array
        items:
          type: object
          properties:
            capability: { type: string }
            params: { type: object }
`

// shapeSourceDoc 是三种整块模板写法 + 一个普通模板写法（反例）。
const shapeSourceDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: a-review
      params: '{{values.pobj}}'
`

const shapeTargetsTemplateDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets: '{{values.tlist}}'
`

const shapeTargetItemTemplateDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - '{{values.tobj}}'
`

const shapePlainTemplateDoc = `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: {}
  targets: '{{values.tplain}}'
`

// shapeSecret* 是 writeOnly 明文（可全文搜索）；shapePlainPin 是**非敏感**模板里的值。
const (
	shapeSecretOldPin = 424242
	shapeSecretNewPin = 525252
	shapeSecretOldTag = "shape-tag-old-secret"
	shapeSecretNewTag = "shape-tag-new-secret"
	shapePlainOldPin  = 616161
	shapePlainNewPin  = 727272
)

// shapeSecrets 是回执里绝不允许出现的明文。
var shapeSecrets = []string{
	"424242", "525252", shapeSecretOldTag, shapeSecretNewTag,
}

// shapePlanFor 用两轮 values 走一次 plan（第一轮落「库」，第二轮触发 targets 差异）。
func shapePlanFor(t *testing.T, sourceDoc string, oldValues, newValues map[string]any) *PlanResult {
	t.Helper()
	pkg := newBundle().
		setValues(shapeValuesDoc).
		cap("capabilities/review.yaml", maskCapDoc).
		source("sources/on-push.yaml", sourceDoc).
		pkg(t)
	oldRendered := mustRender(t, pkg, oldValues)
	oldSrc := findSource(oldRendered.Sources, "a-on-push")
	if oldSrc == nil {
		t.Fatalf("渲染结果里没有 a-on-push: %+v", oldRendered.Sources)
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
	if plan.Unapplied[0].Reason != ReasonTargetsUnapplied {
		t.Fatalf("unapplied reason = %q", plan.Unapplied[0].Reason)
	}
	return plan
}

// TestPlanUnappliedMasksTemplatedParams 覆盖「整块 params 模板」：raw 里没有 param key，
// 只能整组打码；否则 writeOnly 对象里的每个字段都会明文进回执。
func TestPlanUnappliedMasksTemplatedParams(t *testing.T) {
	plan := shapePlanFor(t, shapeSourceDoc,
		map[string]any{"pobj": map[string]any{"pin": shapeSecretOldPin, "label": shapeSecretOldTag}},
		map[string]any{"pobj": map[string]any{"pin": shapeSecretNewPin, "label": shapeSecretNewTag}})

	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	for _, side := range []struct {
		name string
		tg   []sourcespec.Target
	}{{"declared", declared}, {"actual", actual}} {
		params := side.tg[0].Params
		if len(params) != 2 {
			t.Fatalf("%s.params = %#v", side.name, params)
		}
		for key := range params {
			if got := params[key]; got != MaskedValue {
				t.Fatalf("%s.params[%s] 没打码（整块 params 模板必须整组打码）: %#v", side.name, key, got)
			}
		}
	}

	// 执行事实保持明文（新建源落库读的是 PlannedSource.Spec）。
	planned := findPlannedSource(plan.Sources, "a-on-push")
	if planned == nil {
		t.Fatalf("计划里没有 a-on-push: %+v", plan.Sources)
	}
	if got, ok := planned.Spec.Targets[0].Params["pin"]; !ok || got != float64(shapeSecretNewPin) {
		t.Fatalf("PlannedSource.Spec.params[pin] 被打码/改动: %#v", planned.Spec.Targets[0].Params)
	}

	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_shape_params", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range shapeSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}

// TestPlanUnappliedMasksTemplatedTargetItem 覆盖「整个 target 项是模板」：
// raw 列表项不是对象 → 精确档无 key 可对，整组打码。
func TestPlanUnappliedMasksTemplatedTargetItem(t *testing.T) {
	plan := shapePlanFor(t, shapeTargetItemTemplateDoc,
		map[string]any{"tobj": map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapeSecretOldPin, "label": shapeSecretOldTag},
		}},
		map[string]any{"tobj": map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapeSecretNewPin, "label": shapeSecretNewTag},
		}})

	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	for _, side := range []struct {
		name string
		tg   []sourcespec.Target
	}{{"declared", declared}, {"actual", actual}} {
		params := side.tg[0].Params
		if len(params) == 0 {
			t.Fatalf("%s.params 为空，断言会变成空断言: %#v", side.name, side.tg[0])
		}
		for key := range params {
			if got := params[key]; got != MaskedValue {
				t.Fatalf("%s.params[%s] 没打码（整 target 项模板必须整组打码）: %#v", side.name, key, got)
			}
		}
	}

	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_shape_item", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range shapeSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}

// TestPlanUnappliedMasksTemplatedTargets 覆盖「整块 targets 列表是模板」：
// raw `spec.targets` 不是列表 → 与渲染后的长度必然不一致 → 走 opaque 档。
func TestPlanUnappliedMasksTemplatedTargets(t *testing.T) {
	plan := shapePlanFor(t, shapeTargetsTemplateDoc,
		map[string]any{"tlist": []any{map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapeSecretOldPin, "label": shapeSecretOldTag},
		}}},
		map[string]any{"tlist": []any{map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapeSecretNewPin, "label": shapeSecretNewTag},
		}}})

	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	for _, side := range []struct {
		name string
		tg   []sourcespec.Target
	}{{"declared", declared}, {"actual", actual}} {
		params := side.tg[0].Params
		if len(params) == 0 {
			t.Fatalf("%s.params 为空，断言会变成空断言: %#v", side.name, side.tg[0])
		}
		for key := range params {
			if got := params[key]; got != MaskedValue {
				t.Fatalf("%s.params[%s] 没打码（整块 targets 模板必须整组打码）: %#v", side.name, key, got)
			}
		}
	}

	raw, err := json.Marshal(BuildReceipt(plan, plan.Values, plan.ValuesDigest,
		InstallMeta{ID: "ins_shape_targets", At: ts, Namespace: "ns"}))
	if err != nil {
		t.Fatalf("回执序列化失败: %v", err)
	}
	for _, secret := range shapeSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("回执里泄露了明文 %q: %s", secret, raw)
		}
	}
}

// TestPlanUnappliedKeepsPlainTemplatedTargets 是反向红线：整块模板引用的是**普通** values
// （非 writeOnly）时不得打码，否则「新旧差异」对用户不可见（过度打码）。
func TestPlanUnappliedKeepsPlainTemplatedTargets(t *testing.T) {
	plan := shapePlanFor(t, shapePlainTemplateDoc,
		map[string]any{"tplain": []any{map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapePlainOldPin},
		}}},
		map[string]any{"tplain": []any{map[string]any{
			"capability": "a-review",
			"params":     map[string]any{"pin": shapePlainNewPin},
		}}})

	declared := plan.Unapplied[0].Declared.([]sourcespec.Target)
	actual := plan.Unapplied[0].Actual.([]sourcespec.Target)
	if got := declared[0].Params["pin"]; got != float64(shapePlainNewPin) {
		t.Fatalf("非敏感模板的 declared.params[pin] 被过度打码: %#v", got)
	}
	if got := actual[0].Params["pin"]; got != float64(shapePlainOldPin) {
		t.Fatalf("非敏感模板的 actual.params[pin] 被过度打码: %#v", got)
	}
}
