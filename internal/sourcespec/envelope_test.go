package sourcespec

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestNormalizeEnvelope 钉住「写入期拒未知键与非法组合」：这类配置若被接受会静默失效。
func TestNormalizeEnvelope(t *testing.T) {
	ok, err := NormalizeConfig(KindWebhook, json.RawMessage(`{"envelope":{
		"type":{"header":"x-github-event","joinFrom":"$.action"},
		"deliveryId":{"pointer":"$.id"},
		"payload":{"whole":true}},"signature":{"scheme":"github-sha256"}}`))
	if err != nil {
		t.Fatalf("合法 envelope 应通过: %v", err)
	}
	cfg := ParseConfig(KindWebhook, ok)
	if cfg.Envelope.Type.Header != "x-github-event" || cfg.Envelope.Type.JoinFrom != "action" {
		t.Fatalf("type 来源不对（joinFrom 应归一成不带 $ 的形式）: %+v", cfg.Envelope.Type)
	}
	if cfg.Envelope.DeliveryID.Pointer != "id" || !cfg.Envelope.Payload.Whole {
		t.Fatalf("deliveryId/payload 来源不对: %+v", cfg.Envelope)
	}
	if cfg.Scheme() != SchemeGitHubSHA256 {
		t.Fatalf("scheme 应为 github-sha256, got %q", cfg.Scheme())
	}

	bad := map[string]string{
		"envelope 未知键":          `{"envelope":{"typo":1}}`,
		"envelope 空对象":          `{"envelope":{}}`,
		"type 双来源":              `{"envelope":{"type":{"header":"h","pointer":"p"}}}`,
		"type 无来源":              `{"envelope":{"type":{}}}`,
		"deliveryId 带 joinFrom": `{"envelope":{"deliveryId":{"header":"h","joinFrom":"a"}}}`,
		"payload 双来源":           `{"envelope":{"payload":{"whole":true,"pointer":"a"}}}`,
		"payload 无来源":           `{"envelope":{"payload":{}}}`,
		"payload.whole false":   `{"envelope":{"payload":{"whole":false}}}`,
		"指针非法（连字符）":             `{"envelope":{"type":{"pointer":"$.a-b"}}}`,
		"signature 未知键":         `{"signature":{"algo":"x"}}`,
		"scheme 非法值":            `{"signature":{"scheme":"md5"}}`,
	}
	for name, raw := range bad {
		if _, err := NormalizeConfig(KindWebhook, json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s：应被拒（ErrInvalidConfig），得到 %v", name, err)
		}
	}

	// 显式默认口径折叠成缺省（否则落库字节不同 → 安装比较误判漂移）。
	for _, raw := range []string{`{"signature":{"scheme":"anotify-sha256"}}`, `{"signature":{}}`} {
		out, err := NormalizeConfig(KindWebhook, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s 应通过: %v", raw, err)
		}
		if cfg := ParseConfig(KindWebhook, out); cfg.Signature != nil {
			t.Errorf("%s 应折叠成缺省（Signature=nil），得到 %+v", raw, cfg.Signature)
		}
	}

	// 白名单按 kind 封闭：manual 不认这两个键。
	if _, err := NormalizeConfig(KindManual, json.RawMessage(`{"envelope":{}}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("manual 不认 envelope，得到 %v", err)
	}
}

// TestPointerSyntax：$ 前缀可选、数字段是数组下标、非法段与空路径写入期拒。
func TestPointerSyntax(t *testing.T) {
	for _, raw := range []string{"$.a.b", "a.b", "$.a.0.b", "a.0"} {
		if _, err := ParsePointer(raw); err != nil {
			t.Fatalf("%q 应合法: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "$", "$.", "a..b", "a.", "a-b", "a b"} {
		if _, err := ParsePointer(raw); err == nil {
			t.Errorf("%q 应被拒", raw)
		}
	}
	root := map[string]any{"a": map[string]any{"b": json.Number("7")}, "list": []any{map[string]any{"k": "v"}}}
	if v, ok := LookupPointer(root, "a.b"); !ok || v != json.Number("7") {
		t.Fatalf("a.b 取值不对: %v %v", v, ok)
	}
	if v, ok := LookupPointer(root, "list.0.k"); !ok || v != "v" {
		t.Fatalf("数字段应作为数组下标: %v %v", v, ok)
	}
	if _, ok := LookupPointer(root, "a.missing"); ok {
		t.Fatal("取不到必须 ok=false")
	}
	if text, ok := ScalarText(map[string]any{}); ok || text != "" {
		t.Fatalf("对象不是标量（应回落）: %q %v", text, ok)
	}
}

// TestMatchTypes 钉住 match 的求值语义：nil = 全部命中、精确、大小写敏感。
func TestMatchTypes(t *testing.T) {
	var none *Match
	if !none.Matches("anything") {
		t.Fatal("没写 match = 全部命中")
	}
	m := &Match{Types: []string{"issues.opened", "issue_comment.created"}}
	if !m.Matches("issues.opened") || m.Matches("Issues.Opened") || m.Matches("issues") {
		t.Fatalf("match.types 必须精确且大小写敏感: %+v", m.Types)
	}
	for _, raw := range []string{`{}`, `{"types":[]}`, `{"types":[""]}`, `{"types":["a","a"]}`} {
		var tm Match
		if err := json.Unmarshal([]byte(raw), &tm); err != nil {
			t.Fatalf("%s 解码失败: %v", raw, err)
		}
		if err := normalizeMatch(&tm); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s 应被拒，得到 %v", raw, err)
		}
	}
	if err := ValidateTargets([]Target{{Capability: "x", Match: &Match{Types: []string{"a", " a "}}}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("归一后重复的 types 应被拒，得到 %v", err)
	}
}
