package server

// issue #356 / 契约 A3：writeOnly 派生的 params 只能以 `"***"` 出现在**读侧**——
// `GET …/installs/{id}`、`GET …/installs`、`GET …/playbooks/{slug}`（status.unapplied）
// 三处响应体全文不含明文；`POST …/render` 的 params 仍是明文（S5：render 不改）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// pbMaskValuesBlock 是包内 spec.values（四种 writeOnly 形态：对象 / 数组 / 整数 / 布尔）。
const pbMaskValuesBlock = `    type: object
    additionalProperties: false
    properties:
      project: { type: string }
      blob: { type: object, writeOnly: true, properties: { key: { type: string } } }
      list: { type: array, writeOnly: true, items: { type: string } }
      pin: { type: integer, writeOnly: true }
      flag: { type: boolean, writeOnly: true }`

// pbMaskEntryDoc 组装入口文档（values 用上面那份 schema）。
func pbMaskEntryDoc(slug string, caps, srcs []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: " + slug + "\n")
	b.WriteString("spec:\n  values:\n" + pbMaskValuesBlock + "\n")
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

// pbMaskSourceDoc 是带 writeOnly 派生 params 的 webhook 源。
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

// pbMaskBundle 是「一个能力 + 一个带 writeOnly 派生 params 的源」的包体。
func pbMaskBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbMaskEntryDoc(slug, []string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"}),
		"capabilities/review.yaml": pbTestCapabilityDoc("a-review", nil),
		"sources/on-push.yaml":     pbMaskSourceDoc(),
	}
}

// pbMaskOldValues / pbMaskNewValues 是两次 install 的 values（writeOnly 值不同）。
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

// pbMaskSecrets 是响应体里绝不允许出现的明文（新旧 writeOnly 真实值）。
var pbMaskSecrets = []string{
	"obj-secret-old", "obj-secret-new", "arr-secret-old", "arr-secret-new",
	"987654321", "987654322",
}

// pbMaskParamsOf 把 unapplied[].declared/actual（JSON 读回的泛型形态）解成 params。
func pbMaskParamsOf(t *testing.T, v any) map[string]any {
	t.Helper()
	list, ok := v.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("declared/actual 应是 1 元素数组: %#v", v)
	}
	target, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("target 应是对象: %#v", list[0])
	}
	params, ok := target["params"].(map[string]any)
	if !ok {
		t.Fatalf("target.params 应是对象: %#v", target)
	}
	return params
}

// TestPlaybookResourceUnappliedMaskedOnRead 是契约 A3 的全部断言点。
func TestPlaybookResourceUnappliedMaskedOnRead(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "mask-pack"
	env.create(t, slug, pbMaskBundle(slug))

	// ① 首次安装：源 create（明文 targets 落库）。
	rec := env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbMaskOldValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var firstRcpt struct {
		Install struct {
			ID string `json:"id"`
		} `json:"install"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &firstRcpt); err != nil {
		t.Fatalf("decode 首次安装回执: %v (%s)", err, rec.Body.String())
	}
	if firstRcpt.Install.ID == "" {
		t.Fatalf("首次安装回执缺 id: %s", rec.Body.String())
	}
	// f3 的断言扫的是**两轮**回执：首轮虽然没 unapplied，但 values 打码同样不能泄漏。
	for _, secret := range pbMaskSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("首次安装回执泄露了明文 %q: %s", secret, rec.Body.String())
		}
	}

	// ② 换 writeOnly 值再装 → 行 12 的 targets 差异（回执 unapplied 里必须只有 "***"）。
	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbMaskNewValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rcpt struct {
		Kind         string `json:"kind"`
		ValuesDigest string `json:"valuesDigest"`
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
	if rcpt.Kind != "InstallReceipt" || rcpt.Install.ID == "" {
		t.Fatalf("安装回执形状不对: %s", rec.Body.String())
	}
	if len(rcpt.Unapplied) != 1 {
		t.Fatalf("应有 1 条 unapplied: %s", rec.Body.String())
	}

	// ③ GET …/installs/{id}：全文不含明文，params 是 "***"。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs/"+rcpt.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("单回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range pbMaskSecrets {
		if strings.Contains(body, secret) {
			t.Fatalf("单回执响应泄露了明文 %q: %s", secret, body)
		}
	}
	var single struct {
		Unapplied []struct {
			Kind     string `json:"kind"`
			Field    string `json:"field"`
			Declared any    `json:"declared"`
			Actual   any    `json:"actual"`
		} `json:"unapplied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode 单回执: %v", err)
	}
	if len(single.Unapplied) != 1 || single.Unapplied[0].Kind != "source" || single.Unapplied[0].Field != "targets" {
		t.Fatalf("单回执 unapplied 定位不对: %+v", single.Unapplied)
	}
	for _, side := range []struct {
		name string
		v    any
	}{{"declared", single.Unapplied[0].Declared}, {"actual", single.Unapplied[0].Actual}} {
		params := pbMaskParamsOf(t, side.v)
		for _, key := range []string{"blob", "list", "pin", "flag", "mixed"} {
			if got := params[key]; got != playbook.MaskedValue {
				t.Fatalf("单回执 %s.params[%s] 没打码: %#v", side.name, key, got)
			}
		}
		if got := params["fixedNumber"]; got != float64(77) {
			t.Fatalf("单回执 %s.params[fixedNumber] 被误伤: %#v", side.name, got)
		}
		if got := params["fixedBoolean"]; got != true {
			t.Fatalf("单回执 %s.params[fixedBoolean] 被误伤: %#v", side.name, got)
		}
	}

	// ③b 首轮 install 的**单回执**也扫一遍（GET 读回的是落库的 receipt 原文）。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs/"+firstRcpt.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("首轮回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, secret := range pbMaskSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("首轮回执响应泄露了明文 %q: %s", secret, rec.Body.String())
		}
	}

	// ④ GET …/installs（折叠列表）同样不含明文。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("实例清单应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, secret := range pbMaskSecrets {
		if strings.Contains(body, secret) {
			t.Fatalf("实例清单泄露了明文 %q: %s", secret, body)
		}
	}

	// ⑤ GET …/playbooks/{slug} 的 status.unapplied 是从回执历史派生的，同样打码。
	rec = env.do(t, "GET", env.path("/"+slug), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, secret := range pbMaskSecrets {
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
		for _, key := range []string{"blob", "list", "pin", "flag", "mixed"} {
			if got := params[key]; got != playbook.MaskedValue {
				t.Fatalf("status.unapplied %s.params[%s] 没打码: %#v", side, key, got)
			}
		}
	}

	// ⑥ POST …/render 的 params 仍是明文（S5：打码只影响回执的展示副本）。
	rec = env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{"values": pbMaskNewValues()})
	if rec.Code != http.StatusOK {
		t.Fatalf("render 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"obj-secret-new", "arr-secret-new", "secret=987654322"} {
		if !strings.Contains(body, want) {
			t.Fatalf("render 输出应保留明文 %q（/render 不改）: %s", want, body)
		}
	}
	// f3 另外两条断言在 HTTP 层钉住：`/render` 与 `/install` 的 valuesDigest 一致；
	// `/render` 的 params 保留原始**类型**（pin 是数字，不是 "***" 也不是字符串）。
	var rend struct {
		ValuesDigest string `json:"valuesDigest"`
		Sources      []struct {
			Name string `json:"name"`
			Spec struct {
				Targets []struct {
					Capability string         `json:"capability"`
					Params     map[string]any `json:"params"`
				} `json:"targets"`
			} `json:"spec"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rend); err != nil {
		t.Fatalf("decode render: %v (%s)", err, body)
	}
	if rend.ValuesDigest != rcpt.ValuesDigest {
		t.Fatalf("/render 与 /install 的 valuesDigest 不一致: %q vs %q", rend.ValuesDigest, rcpt.ValuesDigest)
	}
	if len(rend.Sources) != 1 || len(rend.Sources[0].Spec.Targets) != 1 {
		t.Fatalf("render sources 形状不对: %+v", rend.Sources)
	}
	params := rend.Sources[0].Spec.Targets[0].Params
	newPin, ok := pbMaskNewValues()["pin"].(int)
	if !ok {
		t.Fatalf("测试取值类型变了: %T", pbMaskNewValues()["pin"])
	}
	if got := params["pin"]; got != float64(newPin) {
		t.Fatalf("render params[pin] 应保留原始数值 %d: %#v", newPin, got)
	}
	blob, ok := params["blob"].(map[string]any)
	if !ok || blob["key"] != "obj-secret-new" {
		t.Fatalf("render params[blob] 应是明文对象: %#v", params["blob"])
	}
}
