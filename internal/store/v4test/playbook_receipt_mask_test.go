package v4test

// issue #356 / 契约 A2：回执 `unapplied[]` 的 writeOnly 派生 params 必须在**落库前**打码，
// 且已存源行的 `spec.targets` 仍是明文（执行事实没有被回执打码污染）。
//
// 与 `TestInstallWriteOnlyCompositeMasked` 的分工：那条验 `receipt.values` 的复合 writeOnly
// 打码；本条验 `receipt.unapplied[].declared/actual`（行 12 的差异副本）从 Plan 起就打码。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// pbMaskValuesSchema 覆盖四种 writeOnly 形态（对象 / 数组 / 整数 / 布尔）。
const pbMaskValuesSchema = `    type: object
    additionalProperties: false
    properties:
      project: { type: string }
      blob: { type: object, writeOnly: true, properties: { key: { type: string } } }
      list: { type: array, writeOnly: true, items: { type: string } }
      pin: { type: integer, writeOnly: true }
      flag: { type: boolean, writeOnly: true }`

// pbMaskSourceDoc 是源文档：params 里混了 writeOnly 派生值 + 非敏感字面量。
func pbMaskSourceDoc() string {
	return `metadata:
  name: 'a-on-push'
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: 'a-review'
      params:
        blob: '{{values.blob}}'
        list: '{{values.list}}'
        pin: '{{values.pin}}'
        flag: '{{values.flag}}'
        mixed: 'secret={{values.pin}}'
        fixedNumber: 77
        fixedBoolean: true
`
}

// pbMaskPackage 组出 f3 同形的包（一个能力 + 一个带 writeOnly 派生 params 的源）。
func pbMaskPackage() *pbPackage {
	p := newPBPackage("mask-pack")
	p.values = pbMaskValuesSchema
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "a-review", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbMaskSourceDoc()
	return p
}

// pbMaskOldValues / pbMaskNewValues 是两次安装的 values（writeOnly 值不同 → 行 12 差异）。
func pbMaskOldValues() map[string]any {
	return map[string]any{
		"project": "a",
		"blob":    map[string]any{"key": "obj-secret-old"},
		"list":    []any{"arr-secret-old"},
		"pin":     987654321,
		"flag":    true,
	}
}

func pbMaskNewValues() map[string]any {
	return map[string]any{
		"project": "a",
		"blob":    map[string]any{"key": "obj-secret-new"},
		"list":    []any{"arr-secret-new"},
		"pin":     987654322,
		"flag":    false,
	}
}

// pbMaskSecrets 是库里/响应里绝不允许出现的明文。
var pbMaskSecrets = []string{
	"obj-secret-old", "obj-secret-new", "arr-secret-old", "arr-secret-new",
	"987654321", "987654322",
}

// pbMaskTargetParams 把 unapplied[].declared/actual 解成 params（**同时**支持 Plan 里
// 的强类型 `[]sourcespec.Target` 与回执 JSON 读回的泛型 `[]any`）。
func pbMaskTargetParams(t *testing.T, v any) map[string]any {
	t.Helper()
	var target map[string]any
	switch list := v.(type) {
	case []sourcespec.Target:
		if len(list) != 1 {
			t.Fatalf("declared/actual 应有 1 个 target: %#v", v)
		}
		return map[string]any(list[0].Params)
	case []any:
		if len(list) != 1 {
			t.Fatalf("declared/actual 应有 1 个 target: %#v", v)
		}
		decoded, ok := list[0].(map[string]any)
		if !ok {
			t.Fatalf("target 应是对象: %#v", list[0])
		}
		target = decoded
	default:
		t.Fatalf("declared/actual 形状不对: %#v", v)
	}
	params, ok := target["params"].(map[string]any)
	if !ok {
		t.Fatalf("target.params 应是对象: %#v", target)
	}
	return params
}

// TestInstallReceiptUnappliedMaskedBeforePersist 是契约 A2 的全部断言点。
func TestInstallReceiptUnappliedMaskedBeforePersist(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-receipt-mask")

	p := pbMaskPackage()
	row := pbCreate(t, db, ns, p)

	// ① 首次安装：源 create，明文 targets 落库（这是第二次安装回执 actual 侧的事实来源）。
	pbInstall(t, db, ns, p.slug, pbMaskOldValues())

	// ② 同包再装一次，只换 writeOnly 值 → 行 12（config 等价、targets 不同）→ unapplied。
	receipt := pbInstall(t, db, ns, p.slug, pbMaskNewValues())
	if len(receipt.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", receipt.Unapplied)
	}
	if receipt.Unapplied[0].Reason != playbook.ReasonTargetsUnapplied {
		t.Fatalf("unapplied reason = %q", receipt.Unapplied[0].Reason)
	}
	for _, key := range []string{"blob", "list", "pin", "flag", "mixed"} {
		if got := pbMaskTargetParams(t, receipt.Unapplied[0].Declared)[key]; got != playbook.MaskedValue {
			t.Fatalf("安装回执 declared.params[%s] 没打码: %#v", key, got)
		}
		if got := pbMaskTargetParams(t, receipt.Unapplied[0].Actual)[key]; got != playbook.MaskedValue {
			t.Fatalf("安装回执 actual.params[%s] 没打码: %#v", key, got)
		}
	}

	// ③ 落库前就打码：playbook_installs 的**原始 JSON** 里没有明文，且带 "***"。
	var rawValues, rawReceipt string
	if err := db.QueryRow(`SELECT "values", receipt FROM playbook_installs WHERE id = ?`,
		receipt.Install.ID).Scan(&rawValues, &rawReceipt); err != nil {
		t.Fatalf("读回执行原文: %v", err)
	}
	for _, secret := range pbMaskSecrets {
		if strings.Contains(rawReceipt, secret) {
			t.Fatalf("回执原文泄露了明文 %q: %s", secret, rawReceipt)
		}
		if strings.Contains(rawValues, secret) {
			t.Fatalf("values 原文泄露了明文 %q: %s", secret, rawValues)
		}
	}

	// ④ GetPlaybookInstall 读回的 declared/actual 打码、非敏感字面量保留。
	stored, err := db.GetPlaybookInstall(ctx, ns, p.slug, receipt.Install.ID)
	if err != nil {
		t.Fatalf("读回执: %v", err)
	}
	if len(stored.Unapplied) != 1 {
		t.Fatalf("读回 unapplied = %+v", stored.Unapplied)
	}
	if stored.Unapplied[0].Kind != "source" || stored.Unapplied[0].Field != "targets" {
		t.Fatalf("读回 unapplied 定位不对: %+v", stored.Unapplied[0])
	}
	// 恒真断言修掉：整份回执的 `values` 必然带 "***"，不能拿它证明 unapplied 打码了。
	// 这里只看读回的 **unapplied 片段**：必须含 "***" 且不含任何明文。
	unappliedRaw, err := json.Marshal(stored.Unapplied)
	if err != nil {
		t.Fatalf("unapplied 序列化失败: %v", err)
	}
	if !strings.Contains(string(unappliedRaw), playbook.MaskedValue) {
		t.Fatalf("读回 unapplied 应有打码占位: %s", unappliedRaw)
	}
	for _, secret := range pbMaskSecrets {
		if strings.Contains(string(unappliedRaw), secret) {
			t.Fatalf("读回 unapplied 泄露了明文 %q: %s", secret, unappliedRaw)
		}
	}
	for _, side := range []struct {
		name string
		v    any
		// 非敏感字面量在 JSON 读回后是 float64 / bool。
		wantNumber  float64
		wantBoolean bool
	}{
		{"declared", stored.Unapplied[0].Declared, 77, true},
		{"actual", stored.Unapplied[0].Actual, 77, true},
	} {
		params := pbMaskTargetParams(t, side.v)
		for _, key := range []string{"blob", "list", "pin", "flag", "mixed"} {
			if got := params[key]; got != playbook.MaskedValue {
				t.Fatalf("读回 %s.params[%s] 没打码: %#v", side.name, key, got)
			}
		}
		if got := params["fixedNumber"]; got != side.wantNumber {
			t.Fatalf("读回 %s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
		if got := params["fixedBoolean"]; got != side.wantBoolean {
			t.Fatalf("读回 %s.params[fixedBoolean] 被误伤: %#v", side.name, got)
		}
	}

	// ⑤ 实例清单（折叠列表）里也没有明文。
	list, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("读实例清单: %v", err)
	}
	listRaw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("序列化实例清单: %v", err)
	}
	for _, secret := range pbMaskSecrets {
		if strings.Contains(string(listRaw), secret) {
			t.Fatalf("实例清单泄露了明文 %q: %s", secret, listRaw)
		}
	}

	// ⑥ 已存源行的 spec.targets 仍是明文（回执打码没有污染执行事实）。
	src := pbSource(t, db, ns, "a-on-push")
	if len(src.Targets) != 1 {
		t.Fatalf("源行 targets = %#v", src.Targets)
	}
	srcParams := src.Targets[0].Params
	blob, ok := srcParams["blob"].(map[string]any)
	if !ok || blob["key"] != "obj-secret-old" {
		t.Fatalf("源行的 blob 应保留首次安装的明文: %#v", srcParams["blob"])
	}
	if srcParams["pin"] != float64(987654321) {
		t.Fatalf("源行的 pin 应保留首次安装的明文: %#v", srcParams["pin"])
	}
	if srcParams["mixed"] != "secret=987654321" {
		t.Fatalf("源行的 mixed 应保留首次安装的明文: %#v", srcParams["mixed"])
	}

	// ⑦ valuesDigest 仍对打码前的 values 算，且与 RenderPlaybook 一致。
	rendered, err := db.RenderPlaybook(ctx, ns, p.slug, pbMaskNewValues())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if stored.ValuesDigest != rendered.ValuesDigest {
		t.Fatalf("valuesDigest 应与 render 一致: %s vs %s", stored.ValuesDigest, rendered.ValuesDigest)
	}
	if stored.ValuesDigest != receipt.ValuesDigest {
		t.Fatalf("落库回执 digest 与安装回执不一致: %s vs %s", stored.ValuesDigest, receipt.ValuesDigest)
	}
}
