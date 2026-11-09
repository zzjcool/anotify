package sourcespec

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// timeAt 解析 RFC3339（用例里的固定时刻）。
func timeAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("bad test time: " + s)
	}
	return t
}

// TestKindsRegistry 验 kind 注册表只有三个内置项，判定走查表。
func TestKindsRegistry(t *testing.T) {
	got := Kinds()
	want := []string{"cron", "webhook", "manual"}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Kinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// 返回的是拷贝：调用方改了不影响注册表。
	got[0] = "hacked"
	if !IsKind("cron") {
		t.Fatalf("Kinds() 必须返回拷贝")
	}
	if IsKind("github") {
		t.Fatalf("github 不在注册表里（v4 删除该 kind）")
	}
	err := ValidateSpec(Spec{Kind: "github"}, true)
	if !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("未知 kind 应 ErrInvalidKind，得到 %v", err)
	}
}

// TestNormalizeCron 验 cron 归一化：expr 必须解析、tz 默认 Local、丢弃未知字段。
func TestNormalizeCron(t *testing.T) {
	cfg, err := NormalizeConfig(KindCron, json.RawMessage(`{"expr":"0 3 * * *"}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	parsed := ParseConfig(KindCron, cfg)
	if parsed.TZ != "Local" {
		t.Fatalf("tz 默认应为 Local，得到 %q", parsed.TZ)
	}
	// 归一化是幂等的（落库字节稳定）。
	again, err := NormalizeConfig(KindCron, cfg)
	if err != nil {
		t.Fatalf("renormalize: %v", err)
	}
	if string(again) != string(cfg) {
		t.Fatalf("归一化应幂等：%s vs %s", again, cfg)
	}
	// 永不触发的表达式当场拒。
	if _, err := NormalizeConfig(KindCron, json.RawMessage(`{"expr":"61 * * * *"}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非法表达式应 ErrInvalidConfig，得到 %v", err)
	}
	// 缺 expr / 未知时区 / 未知字段都拒。
	for _, raw := range []string{
		`{"tz":"UTC"}`,
		`{"expr":"0 3 * * *","tz":"Mars/Olympus"}`,
		`{"expr":"0 3 * * *","token":"x"}`,
		`{"expr":"0 3 * * *","payload":[]}`,
		`[]`,
	} {
		if _, err := NormalizeConfig(KindCron, json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %s 应被拒，得到 %v", raw, err)
		}
	}
	// payload / payloadSchema / sample 保留。
	cfg, err = NormalizeConfig(KindCron, json.RawMessage(
		`{"expr":"0 3 * * *","payload":{"a":1},"payloadSchema":{"type":"object"},"sample":{"a":2}}`))
	if err != nil {
		t.Fatalf("normalize with payload: %v", err)
	}
	parsed = ParseConfig(KindCron, cfg)
	if parsed.Payload["a"] == nil || len(parsed.PayloadSchema) == 0 || parsed.Sample["a"] == nil {
		t.Fatalf("三个通用字段应保留：%+v", parsed)
	}
}

// TestNormalizeWebhook 验 webhook：token 空保留（由 store 生成）、payload 丢弃、secret 保留。
func TestNormalizeWebhook(t *testing.T) {
	cfg, err := NormalizeConfig(KindWebhook, json.RawMessage(
		`{"payload":{"ignored":true},"sample":{"environment":"prod"},"secret":"s3cr3t"}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	parsed := ParseConfig(KindWebhook, cfg)
	if parsed.Token != "" {
		t.Fatalf("空 token 应原样保留（生成是 store 的事）：%q", parsed.Token)
	}
	if parsed.Payload != nil {
		t.Fatalf("webhook 的 config.payload 应被丢弃：%v", parsed.Payload)
	}
	if parsed.Secret != "s3cr3t" || parsed.Sample["environment"] != "prod" {
		t.Fatalf("secret/sample 应保留：%+v", parsed)
	}
	// 写路径允许空 token；读路径要求已有 token。
	if err := ValidateSpec(Spec{Kind: KindWebhook, Config: cfg}, true); err != nil {
		t.Fatalf("写路径空 token 应允许：%v", err)
	}
	if err := ValidateSpec(Spec{Kind: KindWebhook, Config: cfg}, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("读路径空 token 应被拒，得到 %v", err)
	}
	withToken, err := NormalizeConfig(KindWebhook, json.RawMessage(`{"token":"src_x"}`))
	if err != nil {
		t.Fatalf("normalize token: %v", err)
	}
	if err := ValidateSpec(Spec{Kind: KindWebhook, Config: withToken}, false); err != nil {
		t.Fatalf("读路径有 token 应通过：%v", err)
	}
	// cron 专属字段在 webhook 上被拒。
	if _, err := NormalizeConfig(KindWebhook, json.RawMessage(`{"expr":"0 3 * * *"}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("webhook 上的 expr 应被拒，得到 %v", err)
	}
}

// TestNormalizeManual 验 manual 只有通用字段。
func TestNormalizeManual(t *testing.T) {
	cfg, err := NormalizeConfig(KindManual, json.RawMessage(`{"payload":{"a":1}}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if string(cfg) != `{"payload":{"a":1}}` {
		t.Fatalf("manual config = %s", cfg)
	}
	if _, err := NormalizeConfig(KindManual, json.RawMessage(`{"token":"x"}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("manual 上的 token 应被拒，得到 %v", err)
	}
}

// TestMergeConfigKeepsCredentials 验 PATCH 语义：省略 token/secret 保留已有。
func TestMergeConfigKeepsCredentials(t *testing.T) {
	cur := json.RawMessage(`{"token":"src_old","secret":"s3cr3t","sample":{"a":1}}`)
	patch := json.RawMessage(`{"payloadSchema":{"type":"object"}}`)
	merged, err := MergeConfig(KindWebhook, cur, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := ParseConfig(KindWebhook, merged)
	if got.Token != "src_old" || got.Secret != "s3cr3t" {
		t.Fatalf("省略凭证的 PATCH 必须保留：%+v", got)
	}
	if got.Sample != nil {
		t.Fatalf("未提及的普通字段应缺省（省略=清空）：%v", got.Sample)
	}
	// 显式给 token = 轮换。
	rotated, err := MergeConfig(KindWebhook, cur, json.RawMessage(`{"token":"src_new"}`))
	if err != nil {
		t.Fatalf("merge rotate: %v", err)
	}
	if got := ParseConfig(KindWebhook, rotated).Token; got != "src_new" {
		t.Fatalf("显式 token 应替换，得到 %q", got)
	}
	// 非 webhook 的 merge 就是纯归一化。
	merged, err = MergeConfig(KindCron, json.RawMessage(`{"expr":"0 1 * * *"}`), json.RawMessage(`{"expr":"0 2 * * *"}`))
	if err != nil {
		t.Fatalf("merge cron: %v", err)
	}
	if got := ParseConfig(KindCron, merged).Expr; got != "0 2 * * *" {
		t.Fatalf("cron merge 应取 PATCH 值，得到 %q", got)
	}
}

// TestMaskConfig 验脱敏：保留后 4 位、短串整体打码、坏 config 不漏原文。
func TestMaskConfig(t *testing.T) {
	// token 在 Go 侧拼出来（避免把疑似凭据的字面量写进源码）；secret 特意取短串。
	longToken := "src_tok_" + "abcdef1234"
	cfg := fmt.Sprintf(`{"token":%q,"secret":"abc","sample":{"a":1}}`, longToken)
	masked := MaskConfig(KindWebhook, json.RawMessage(cfg))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(masked, &fields); err != nil {
		t.Fatalf("mask 输出应是 JSON 对象：%v", err)
	}
	var token, secret string
	if err := json.Unmarshal(fields["token"], &token); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if err := json.Unmarshal(fields["secret"], &secret); err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	if token != "***"+longToken[len(longToken)-4:] {
		t.Fatalf("token 脱敏应保留后 4 位，得到 %q", token)
	}
	if secret != "***" {
		t.Fatalf("短串应整体打码，得到 %q", secret)
	}
	// 非凭证字段原样保留（脱敏不能顺手改配置）。
	if string(fields["sample"]) != `{"a":1}` {
		t.Fatalf("sample 不该被改动：%s", fields["sample"])
	}
	if got := string(MaskConfig(KindManual, json.RawMessage(`{bad`))); got != `{}` {
		t.Fatalf("坏 config 应整体打码成 {}，得到 %s", got)
	}
}

// TestSameConfig 验归一化后比较：缺省 tz 与显式 Local 是同一份配置。
func TestSameConfig(t *testing.T) {
	if !SameConfig(KindCron, json.RawMessage(`{"expr":"0 3 * * *"}`), json.RawMessage(`{"expr":"0 3 * * *","tz":"Local"}`)) {
		t.Fatalf("缺省 tz 与显式 Local 应等价")
	}
	if SameConfig(KindCron, json.RawMessage(`{"expr":"0 3 * * *"}`), json.RawMessage(`{"expr":"0 4 * * *"}`)) {
		t.Fatalf("不同表达式不该等价")
	}
	// token 显式参与比较。
	if SameConfig(KindWebhook, json.RawMessage(`{"token":"a"}`), json.RawMessage(`{"token":"b"}`)) {
		t.Fatalf("不同 token 不该等价")
	}
}

// TestSameTargets 验 targets 比较顺序无关、参数语义相同即可。
func TestSameTargets(t *testing.T) {
	a := []Target{
		{Capability: "analyze", Params: map[string]any{"env": "$(payload.environment)"}},
		{Capability: "notify"},
	}
	b := []Target{
		{Capability: "notify"},
		{Capability: "analyze", Params: map[string]any{"env": "$(payload.environment)"}},
	}
	if !SameTargets(a, b) {
		t.Fatalf("顺序不同但内容相同应等价（扇出顺序不是语义）")
	}
	c := []Target{{Capability: "analyze", Params: map[string]any{"env": "$(payload.other)"}}, {Capability: "notify"}}
	if SameTargets(a, c) {
		t.Fatalf("参数模板不同不该等价")
	}
}

// TestValidateParamsTemplate 验模板语法校验（只校形状，不校路径取不取得到）。
func TestValidateParamsTemplate(t *testing.T) {
	ok := []map[string]any{
		{},
		{"env": "$(payload.environment)"},
		{"payload": "$(payload)"},
		{"path": "reports/$(payload.environment)/$(payload.commit.sha).json"},
		{"nested": map[string]any{"items": []any{"$(payload.list.0)"}}},
		{"literal": "$(event.type)"},  // 别的根名 = 字面文本
		{"braces": "{{values.name}}"}, // 包层模板，源参数里就是字面
		{"const": 42},
	}
	for i, params := range ok {
		if err := ValidateParamsTemplate(params); err != nil {
			t.Fatalf("合法模板 #%d %v 被拒：%v", i, params, err)
		}
	}
	bad := []map[string]any{
		{"env": "$(vars.environment)"},
		{"env": "$(params.environment)"},
		{"path": "$(payload.a-b)"},
		{"path": "$(payload..x)"},
		{"path": "$(payload.)"},
		{"path": "$(payload"},
		{"path": "$(payloadx.a)"},
	}
	for i, params := range bad {
		if err := ValidateParamsTemplate(params); err == nil {
			t.Fatalf("非法模板 #%d %v 应被拒", i, params)
		}
	}
}

// TestRenderParams 验求值：整值引用保类型、混写拼字符串、取不到进 missing。
func TestRenderParams(t *testing.T) {
	payload := map[string]any{
		"environment": "prod",
		"number":      json.Number("12"),
		"reviewed":    true,
		"commit":      map[string]any{"sha": "abc123"},
		"list":        []any{"a", "b"},
		"nil":         nil,
	}
	target := Target{Capability: "analyze", Params: map[string]any{
		"env":      "$(payload.environment)",
		"flag":     "$(payload.reviewed)",
		"count":    "$(payload.number)",
		"path":     "reports/$(payload.environment)/$(payload.commit.sha).json",
		"padded":   " $(payload.environment)",
		"whole":    "$(payload)",
		"second":   "$(payload.list.1)",
		"literal":  "$(event.type)",
		"constant": 7,
		"nilly":    "$(payload.nil)",
	}}
	got, missing := RenderParams(target, payload)
	if len(missing) != 0 {
		t.Fatalf("不该有 missing：%v", missing)
	}
	if got["env"] != "prod" {
		t.Fatalf("整值引用应保原类型（string）：%#v", got["env"])
	}
	if got["flag"] != true {
		t.Fatalf("整值引用应保 boolean：%#v", got["flag"])
	}
	if got["count"] != json.Number("12") {
		t.Fatalf("整值引用应保数字原写法：%#v", got["count"])
	}
	if got["path"] != "reports/prod/abc123.json" {
		t.Fatalf("混写应拼字符串：%#v", got["path"])
	}
	if got["padded"] != " prod" {
		t.Fatalf("哪怕只多一个空格也是字符串拼接：%#v", got["padded"])
	}
	if _, ok := got["whole"].(map[string]any); !ok {
		t.Fatalf("$(payload) 应回整个对象：%#v", got["whole"])
	}
	if got["second"] != "b" {
		t.Fatalf("数字段应作为数组下标：%#v", got["second"])
	}
	if got["literal"] != "$(event.type)" {
		t.Fatalf("别的根名应保持字面：%#v", got["literal"])
	}
	if got["constant"] != 7 {
		t.Fatalf("常量原样：%#v", got["constant"])
	}
	if got["nilly"] != nil {
		t.Fatalf("整值引用取到 null 应是 nil：%#v", got["nilly"])
	}

	// 取不到的路径 → 该参数整条进 missing（不静默给空）。
	_, missing = RenderParams(Target{Capability: "x", Params: map[string]any{
		"ok":   "$(payload.environment)",
		"bad":  "$(payload.missing.path)",
		"deep": map[string]any{"x": "$(payload.missing)"},
	}}, payload)
	if len(missing) != 2 || missing[0] != "bad" || missing[1] != "deep" {
		t.Fatalf("missing 应含取不到的参数名（升序）：%v", missing)
	}
}

// TestNextCronTime 验 cron 下次触发计算（含 tz）。
func TestNextCronTime(t *testing.T) {
	cfg := json.RawMessage(`{"expr":"0 3 * * *","tz":"Local"}`)
	next, err := NextCronTime(cfg, timeAt("2026-11-10T04:00:00Z"))
	if err != nil {
		t.Fatalf("next cron: %v", err)
	}
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("下次触发应是次日 03:00，得到 %s", next)
	}
	if _, err := NextCronTime(json.RawMessage(`{"expr":"0 3 * * *","tz":"Mars/Olympus"}`), timeAt("2026-11-10T04:00:00Z")); err == nil {
		t.Fatalf("未知时区应报错")
	}
}
