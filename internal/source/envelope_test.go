package source

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// hookSourceWithConfig 造一行 webhook 源（hookInput 只读 kind / config，不落库）。
func hookSourceWithConfig(t *testing.T, cfg string) *store.Source {
	t.Helper()
	return &store.Source{Kind: sourcespec.KindWebhook, Config: json.RawMessage(cfg)}
}

// githubHeaders 模拟 handler 注入的头读取。
func githubHeaders(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestHookInputLegacyControl 是对照组：没有 envelope 时三元组逐字取自投递体（#513 之前
// 的行为）。新功能不能只靠「旧用例还绿」。
func TestHookInputLegacyControl(t *testing.T) {
	src := hookSourceWithConfig(t, `{"token":"src_x"}`)
	raw := json.RawMessage(`{"type":"t-old","id":"delivery-1","payload":{"env":"prod"}}`)
	in, err := hookInput(src, HookBody{
		Type: "t-old", ID: "delivery-1", Payload: json.RawMessage(`{"env":"prod"}`), Raw: raw,
		Header: githubHeaders(map[string]string{"x-github-event": "issues"}),
	})
	if err != nil {
		t.Fatalf("hookInput: %v", err)
	}
	if in.Type != "t-old" || in.DeliveryID != "delivery-1" || string(in.Payload) != `{"env":"prod"}` {
		t.Fatalf("无 envelope 时三元组必须逐字取自投递体: %+v", in)
	}
}

// TestHookInputEnvelopeMapping：GitHub 形态（头 + joinFrom、指针 id、whole payload）
// 与三条回落（头缺 / 指针取不到 / payload 坏）。
func TestHookInputEnvelopeMapping(t *testing.T) {
	cfg := `{"token":"src_x","envelope":{
		"type":{"header":"x-github-event","joinFrom":"$.action"},
		"deliveryId":{"header":"x-github-delivery"},
		"payload":{"whole":true}}}`
	src := hookSourceWithConfig(t, cfg)
	raw := json.RawMessage(`{"action":"opened","issue":{"title":"hello"}}`)

	in, err := hookInput(src, HookBody{Raw: raw, Header: githubHeaders(map[string]string{
		"x-github-event": "issues", "x-github-delivery": "gh-1",
	})})
	if err != nil {
		t.Fatalf("hookInput: %v", err)
	}
	if in.Type != "issues.opened" {
		t.Fatalf("头 + joinFrom 应拼成 issues.opened, got %q", in.Type)
	}
	if in.DeliveryID != "gh-1" {
		t.Fatalf("deliveryId 应来自头, got %q", in.DeliveryID)
	}
	if string(in.Payload) != string(raw) {
		t.Fatalf("payload.whole 应是整个投递体, got %s", in.Payload)
	}

	// 回落 1：头缺 → type 空（由 Ingest 回落源名），joinFrom 不拼接（不能拼出 ".opened"）。
	in, err = hookInput(src, HookBody{Raw: raw, Header: githubHeaders(nil)})
	if err != nil {
		t.Fatalf("头缺也不该报错: %v", err)
	}
	if in.Type != "" {
		t.Fatalf("头缺时 type 必须为空（交给源名回落），got %q", in.Type)
	}

	// 回落 2：pointer 取不到 → 该维为空（payloadSchema 之类的下游判据照常跑）。
	pcfg := `{"token":"src_x","envelope":{"type":{"pointer":"$.data.type"},"payload":{"pointer":"$.data.object"}}}`
	psrc := hookSourceWithConfig(t, pcfg)
	body := json.RawMessage(`{"data":{"type":"push","object":{"a":1}}}`)
	in, err = hookInput(psrc, HookBody{Raw: body})
	if err != nil {
		t.Fatalf("hookInput: %v", err)
	}
	if in.Type != "push" || string(in.Payload) != `{"a":1}` {
		t.Fatalf("指针来源不对: %+v", in)
	}

	// payload 指针取不到 / 类型不对 → ErrBadPayload（400），不静默当 `{}`。
	for _, bad := range []string{`{"data":{"type":"push"}}`, `{"data":{"type":"push","object":[1]}}`} {
		if _, err := hookInput(psrc, HookBody{Raw: json.RawMessage(bad)}); !errors.Is(err, ErrBadPayload) {
			t.Errorf("payload 指针取不到/非对象应 ErrBadPayload（400）, body=%s got %v", bad, err)
		}
	}

	// type 指针取到对象：按「取不到」处理（回落），不把 JSON 塞进事件类型。
	if in, err := hookInput(hookSourceWithConfig(t, `{"token":"src_x","envelope":{"type":{"pointer":"$.data"}}}`),
		HookBody{Raw: body}); err != nil || in.Type != "" {
		t.Fatalf("非标量 type 应回落成空: %+v %v", in, err)
	}
}

// TestHookScheme 钉住「头名由 config.signature.scheme 选，缺省不变」。
func TestHookScheme(t *testing.T) {
	plain := hookSourceWithConfig(t, `{"token":"src_x","secret":"s"}`)
	if got := HookScheme(plain); got != sourcespec.SchemeAnotifySHA256 {
		t.Fatalf("缺省 scheme 应是 anotify-sha256, got %q", got)
	}
	gh := hookSourceWithConfig(t, `{"token":"src_x","secret":"s","signature":{"scheme":"github-sha256"}}`)
	if got := HookScheme(gh); got != sourcespec.SchemeGitHubSHA256 {
		t.Fatalf("scheme 应为 github-sha256, got %q", got)
	}
	if HookScheme(nil) != sourcespec.SchemeAnotifySHA256 {
		t.Fatal("nil 源按缺省口径（不 panic）")
	}
	// 两种口径的值形状相同：同一份签名头在两个 scheme 下都能验（算法没分叉）。
	body := []byte(`{"a":1}`)
	value := SignHookBody("s", body)
	if err := VerifyHookSignature("s", body, value); err != nil {
		t.Fatalf("github-sha256 与缺省口径共用验签实现: %v", err)
	}
}
