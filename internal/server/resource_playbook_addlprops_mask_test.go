package server

// issue #338 / F8（读侧）：`additionalProperties` / `prefixItems` 派生的 params 只能以
// `"***"` 出现在响应里——`POST …/install`、`GET …/installs/{id}`、`GET …/installs`、
// `GET …/playbooks/{slug}`（status.unapplied）四处响应体全文不含明文；
// `POST …/render` 的 params 仍是明文（S5：render 不改）。
//
// 与 `TestPlaybookResourceUnappliedMaskedOnRead` 的分工：那条钉的是 values 走
// `properties` / `items`；本条补 `additionalProperties`（任意键/嵌套）与 `prefixItems`（元组）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// pbAddlValuesBlock 是包内 spec.values（additionalProperties + prefixItems 两种形态，
// 外加 plainMap / tuple[1] 两条必须不误伤的反例）。
const pbAddlValuesBlock = `    type: object
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

// pbAddlEntryDoc 组装入口文档（values 用上面那份 schema）。
func pbAddlEntryDoc(slug string, caps, srcs []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: " + slug + "\n")
	b.WriteString("spec:\n  values:\n" + pbAddlValuesBlock + "\n")
	writeKey := func(key string, paths []string) {
		b.WriteString("  " + key + ":\n")
		for _, p := range paths {
			b.WriteString("    - " + p + "\n")
		}
	}
	writeKey("capabilities", caps)
	writeKey("sources", srcs)
	return b.String()
}

// pbAddlSourceDoc 是带 additionalProperties/prefixItems 派生 params 的 webhook 源。
func pbAddlSourceDoc() string {
	return `metadata:
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
}

// pbAddlBundle 是「一个能力 + 一个带 additionalProperties/prefixItems 派生 params 的源」。
func pbAddlBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbAddlEntryDoc(slug, []string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"}),
		"capabilities/review.yaml": pbTestCapabilityDoc("a-review", nil),
		"sources/on-push.yaml":     pbAddlSourceDoc(),
	}
}

// pbAddlOldValues / pbAddlNewValues 是两轮 install 的 values（writeOnly 值不同 → targets 差异）。
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

// pbAddlSecrets 是响应体里绝不允许出现的明文（新旧两轮 writeOnly 真实值）。
var pbAddlSecrets = []string{
	"ap-secret-old", "ap-secret-new", "bp-secret-old", "bp-secret-new",
	"424242", "434343", "tup-secret-old", "tup-secret-new",
}

// TestPlaybookResourceAdditionalPropertiesMaskedOnRead 是 F8 的读侧断言点。
func TestPlaybookResourceAdditionalPropertiesMaskedOnRead(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "addlprops-pack"
	env.create(t, slug, pbAddlBundle(slug))

	// ① 首轮安装：源 create（明文 targets 落库）。
	rec := env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbAddlOldValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, secret := range pbAddlSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("首次安装回执泄露了明文 %q: %s", secret, rec.Body.String())
		}
	}

	// ② 换 writeOnly 值再装 → 行 12 的 targets 差异（回执 unapplied 里必须只有 "***"）。
	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbAddlNewValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rcpt struct {
		Values       map[string]any `json:"values"`
		ValuesDigest string         `json:"valuesDigest"`
		Install      struct {
			ID string `json:"id"`
		} `json:"install"`
		Unapplied []struct {
			Declared any `json:"declared"`
			Actual   any `json:"actual"`
		} `json:"unapplied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rcpt); err != nil {
		t.Fatalf("decode 安装回执: %v (%s)", err, rec.Body.String())
	}
	if len(rcpt.Unapplied) != 1 || rcpt.Install.ID == "" {
		t.Fatalf("安装回执形状不对: %s", rec.Body.String())
	}
	if got := rcpt.Values["secrets"].(map[string]any)["alpha"]; got != playbook.MaskedValue {
		t.Fatalf("安装回执 values.secrets.alpha 没打码: %#v", got)
	}

	// ③ GET …/installs/{id}：全文不含明文，params 是 "***"。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs/"+rcpt.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("单回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range pbAddlSecrets {
		if strings.Contains(body, secret) {
			t.Fatalf("单回执响应泄露了明文 %q: %s", secret, body)
		}
	}
	var single struct {
		Values    map[string]any `json:"values"`
		Unapplied []struct {
			Declared any `json:"declared"`
			Actual   any `json:"actual"`
		} `json:"unapplied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode 单回执: %v", err)
	}
	if len(single.Unapplied) != 1 {
		t.Fatalf("单回执 unapplied 应有 1 条: %s", body)
	}
	for _, side := range []struct {
		name string
		v    any
	}{{"declared", single.Unapplied[0].Declared}, {"actual", single.Unapplied[0].Actual}} {
		params := pbMaskParamsOf(t, side.v)
		for _, key := range []string{"byKey", "byKeyOther", "nestedPin", "tuplePin"} {
			if got := params[key]; got != playbook.MaskedValue {
				t.Fatalf("单回执 %s.params[%s] 没打码: %#v", side.name, key, got)
			}
		}
		// 非敏感项不被误伤（普通叶 / 元组后半 / 非 writeOnly 的 additionalProperties / 字面量）。
		if got := params["nestedLabel"]; got != "dev1-label" {
			t.Fatalf("单回执 %s.params[nestedLabel] 被误伤: %#v", side.name, got)
		}
		if got := params["tupleTail"]; got != "tail-kept" {
			t.Fatalf("单回执 %s.params[tupleTail] 被误伤: %#v", side.name, got)
		}
		if got := params["plainExtra"]; got != "plain-note" {
			t.Fatalf("单回执 %s.params[plainExtra] 被误伤: %#v", side.name, got)
		}
		if got := params["fixedNumber"]; got != float64(9) {
			t.Fatalf("单回执 %s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
	}

	// ④ GET …/installs（折叠列表）同样不含明文。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("实例清单应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, secret := range pbAddlSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("实例清单泄露了明文 %q: %s", secret, rec.Body.String())
		}
	}

	// ⑤ GET …/playbooks/{slug} 的 status.unapplied 是从回执历史派生的，同样打码。
	rec = env.do(t, "GET", env.path("/"+slug), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, secret := range pbAddlSecrets {
		if strings.Contains(body, secret) {
			t.Fatalf("详情 status 泄露了明文 %q: %s", secret, body)
		}
	}
	var detail pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode 详情: %v", err)
	}
	if len(detail.Status.Unapplied) != 1 {
		t.Fatalf("status.unapplied 应有 1 条: %+v", detail.Status.Unapplied)
	}
	entry, ok := detail.Status.Unapplied[0].(map[string]any)
	if !ok {
		t.Fatalf("status.unapplied[0] 应是对象: %#v", detail.Status.Unapplied[0])
	}
	for _, side := range []string{"declared", "actual"} {
		params := pbMaskParamsOf(t, entry[side])
		for _, key := range []string{"byKey", "byKeyOther", "nestedPin", "tuplePin"} {
			if got := params[key]; got != playbook.MaskedValue {
				t.Fatalf("status.unapplied %s.params[%s] 没打码: %#v", side, key, got)
			}
		}
	}

	// ⑥ POST …/render 的 params 仍是明文（S5），且 valuesDigest 与打码无关。
	rec = env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{"values": pbAddlNewValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("render 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"ap-secret-new", "bp-secret-new", "tup-secret-new"} {
		if !strings.Contains(body, want) {
			t.Fatalf("render 输出应保留明文 %q（/render 不改）: %s", want, body)
		}
	}
	var rend struct {
		ValuesDigest string `json:"valuesDigest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rend); err != nil {
		t.Fatalf("decode render: %v (%s)", err, body)
	}
	if rend.ValuesDigest != rcpt.ValuesDigest {
		t.Fatalf("/render 与 /install 的 valuesDigest 不一致: %q vs %q", rend.ValuesDigest, rcpt.ValuesDigest)
	}
}
