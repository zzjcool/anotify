package v4test

// issue #338 / F8：writeOnly 打码必须跟上 `additionalProperties` 与 `prefixItems` 两种容器
// 写法——这两类派生的 params 在**落库前**就必须是 `"***"`，`playbook_installs.values` /
// `receipt` 的原始 JSON 里不得出现明文（回执是响应文档：落库即定型，读侧只是转发）。
//
// 与 `TestInstallReceiptUnappliedMaskedBeforePersist` 的分工：那条钉的是「values 走
// `properties` / `items`」的四种 writeOnly 形态；本条补的是「values 走
// `additionalProperties`（任意键/嵌套 properties）与 `prefixItems`（元组）」的形态。

import (
	"context"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// pbAddlValuesSchema 是 F8 的两种形态 + 两条必须不误伤的反例（plainMap / tuple[1]）。
const pbAddlValuesSchema = `    type: object
    additionalProperties: false
    properties:
      project: { type: string }
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
        additionalProperties: { type: string }`

// pbAddlSourceDoc 让 params 逐个引用到每种形态；fixedNumber 是不许被误伤的字面量。
const pbAddlSourceDoc = `metadata:
  name: 'a-on-push'
spec:
  kind: webhook
  enabled: true
  config: {}
  targets:
    - capability: 'a-review'
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

// pbAddlPackage 组出「一个能力 + 一个带 additionalProperties/prefixItems 派生 params 的源」。
func pbAddlPackage() *pbPackage {
	p := newPBPackage("addlprops-pack")
	p.values = pbAddlValuesSchema
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "a-review", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbAddlSourceDoc
	return p
}

// pbAddlOldValues / pbAddlNewValues 是两轮安装的 values（writeOnly 值不同 → targets 差异）。
func pbAddlOldValues() map[string]any {
	return map[string]any{
		"project":  "a",
		"secrets":  map[string]any{"alpha": "ap-secret-old", "beta": "bp-secret-old"},
		"envs":     map[string]any{"dev1": map[string]any{"pin": 424242, "label": "dev1-label"}},
		"tuple":    []any{"tup-secret-old", "tail-kept"},
		"plainMap": map[string]any{"note": "plain-note"},
	}
}

func pbAddlNewValues() map[string]any {
	return map[string]any{
		"project":  "a",
		"secrets":  map[string]any{"alpha": "ap-secret-new", "beta": "bp-secret-new"},
		"envs":     map[string]any{"dev1": map[string]any{"pin": 434343, "label": "dev1-label"}},
		"tuple":    []any{"tup-secret-new", "tail-kept"},
		"plainMap": map[string]any{"note": "plain-note"},
	}
}

// pbAddlSecrets 是库里/响应里绝不允许出现的明文。
var pbAddlSecrets = []string{
	"ap-secret-old", "ap-secret-new", "bp-secret-old", "bp-secret-new",
	"424242", "434343", "tup-secret-old", "tup-secret-new",
}

// TestInstallReceiptAdditionalPropertiesMaskedBeforePersist 是 F8 的落库侧断言点。
func TestInstallReceiptAdditionalPropertiesMaskedBeforePersist(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-receipt-addlprops")

	p := pbAddlPackage()
	row := pbCreate(t, db, ns, p)

	// ① 首轮：源 create（明文 targets 落库，是第二轮 actual 侧的事实来源）。
	first := pbInstall(t, db, ns, p.slug, pbAddlOldValues())

	// ② 换 writeOnly 值再装 → 行 12（targets 差异）→ unapplied，两侧都只有 "***"。
	receipt := pbInstall(t, db, ns, p.slug, pbAddlNewValues())
	if len(receipt.Unapplied) != 1 {
		t.Fatalf("应有且只有 1 条 unapplied: %+v", receipt.Unapplied)
	}
	if receipt.Unapplied[0].Reason != playbook.ReasonTargetsUnapplied {
		t.Fatalf("unapplied reason = %q", receipt.Unapplied[0].Reason)
	}
	for _, key := range []string{"byKey", "byKeyOther", "nestedPin", "tuplePin"} {
		if got := pbMaskTargetParams(t, receipt.Unapplied[0].Declared)[key]; got != playbook.MaskedValue {
			t.Fatalf("安装回执 declared.params[%s] 没打码: %#v", key, got)
		}
		if got := pbMaskTargetParams(t, receipt.Unapplied[0].Actual)[key]; got != playbook.MaskedValue {
			t.Fatalf("安装回执 actual.params[%s] 没打码: %#v", key, got)
		}
	}
	// 非敏感项不被误伤（普通叶 / 元组后半 / 非 writeOnly 的 additionalProperties / 字面量）。
	for _, side := range []struct {
		name string
		v    any
	}{{"declared", receipt.Unapplied[0].Declared}, {"actual", receipt.Unapplied[0].Actual}} {
		params := pbMaskTargetParams(t, side.v)
		if got := params["nestedLabel"]; got != "dev1-label" {
			t.Fatalf("%s.params[nestedLabel] 被误伤: %#v", side.name, got)
		}
		if got := params["tupleTail"]; got != "tail-kept" {
			t.Fatalf("%s.params[tupleTail] 被误伤: %#v", side.name, got)
		}
		if got := params["plainExtra"]; got != "plain-note" {
			t.Fatalf("%s.params[plainExtra] 被误伤: %#v", side.name, got)
		}
		if got := params["fixedNumber"]; got != float64(9) {
			t.Fatalf("%s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
	}

	// ③ 落库原文：两轮回执的 values / receipt 里都没有明文，且有打码占位。
	rows, err := db.Query(`SELECT "values", receipt FROM playbook_installs WHERE playbook_id = ?`,
		row.ID)
	if err != nil {
		t.Fatalf("读回执行原文: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var rawValues, rawReceipt string
		if err := rows.Scan(&rawValues, &rawReceipt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		for _, secret := range pbAddlSecrets {
			if strings.Contains(rawValues, secret) {
				t.Fatalf("values 原文泄露了明文 %q: %s", secret, rawValues)
			}
			if strings.Contains(rawReceipt, secret) {
				t.Fatalf("回执原文泄露了明文 %q: %s", secret, rawReceipt)
			}
		}
		if !strings.Contains(rawValues, playbook.MaskedValue) {
			t.Fatalf("落库 values 应有打码占位: %s", rawValues)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen < 2 {
		t.Fatalf("应至少两轮回执落库，实际 %d", seen)
	}

	// ④ 读回收据：declared/actual 仍是打码态，values 也是打码态。
	stored, err := db.GetPlaybookInstall(ctx, ns, p.slug, receipt.Install.ID)
	if err != nil {
		t.Fatalf("读回执: %v", err)
	}
	if len(stored.Unapplied) != 1 {
		t.Fatalf("读回 unapplied = %+v", stored.Unapplied)
	}
	storedSecrets, ok := stored.Values["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("读回 values.secrets 形状不对: %#v", stored.Values["secrets"])
	}
	for _, key := range []string{"alpha", "beta"} {
		if got := storedSecrets[key]; got != playbook.MaskedValue {
			t.Fatalf("读回 values.secrets[%s] 没打码: %#v", key, got)
		}
	}
	// 首轮那条也读一次（每次 install 都写一行，读侧一视同仁）。
	if _, err := db.GetPlaybookInstall(ctx, ns, p.slug, first.Install.ID); err != nil {
		t.Fatalf("读回首轮回执: %v", err)
	}

	// ⑤ 已存源行的 spec.targets 仍是明文（回执打码没有污染执行事实）。
	src := pbSource(t, db, ns, "a-on-push")
	if len(src.Targets) != 1 {
		t.Fatalf("源行 targets = %#v", src.Targets)
	}
	srcParams := src.Targets[0].Params
	if srcParams["byKey"] != "ap-secret-old" {
		t.Fatalf("源行的 byKey 应保留首次安装的明文: %#v", srcParams["byKey"])
	}
	if srcParams["tuplePin"] != "tup-secret-old" {
		t.Fatalf("源行的 tuplePin 应保留首次安装的明文: %#v", srcParams["tuplePin"])
	}
}
