package sourcespec

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func pollConfigOf(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	out, err := NormalizeConfig(KindPoll, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("归一化应通过：%v", err)
	}
	return out
}

func TestPollConfigDefaults(t *testing.T) {
	got := pollConfigOf(t, `{"url":"https://example.com/a","schedule":{"expr":"*/5 * * * *"}}`)
	want := `{"url":"https://example.com/a","schedule":{"expr":"*/5 * * * *","tz":"Local"},"timeoutMs":10000,"maxItems":100}`
	if string(got) != want {
		t.Fatalf("归一化字节\n得到 %s\n期望 %s", got, want)
	}
	again, err := NormalizeConfig(KindPoll, got)
	if err != nil || string(again) != string(got) {
		t.Fatalf("归一化必须幂等：%v / %s", err, again)
	}
	cfg := ParseConfig(KindPoll, got)
	if cfg.TimeoutMs != DefaultPollTimeoutMs || cfg.MaxItems != DefaultPollMaxItems {
		t.Fatalf("读侧默认值：%+v", cfg)
	}
	if cfg.URL != "https://example.com/a" || cfg.Schedule.Expr != "*/5 * * * *" {
		t.Fatalf("读侧字段：%+v", cfg)
	}
}

// 写入期 SSRF 判据（#532 验收 6 的前半）：私网/环回/链路本地字面量、localhost、.internal/.local。
func TestPollURLRejectsNonPublicHosts(t *testing.T) {
	bad := []string{
		`{"url":"http://127.0.0.1:9/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://127.1.2.3/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://169.254.169.254/latest/meta-data","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://10.0.0.5/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://172.16.3.4/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://192.168.1.1/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://[::1]/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://[fd00::1]/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://localhost/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://api.internal/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://printer.local/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://user:pass@example.com/x","schedule":{"expr":"* * * * *"}}`,
		`{"url":"ftp://example.com/x","schedule":{"expr":"* * * * *"}}`,
		`{"schedule":{"expr":"* * * * *"}}`,
		`{"url":"https://example.com/x"}`,
		`{"url":"https://example.com/x","schedule":{"expr":"61 * * * *"}}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *","tz":"Mars/Base"}}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"maxItems":501}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"timeoutMs":999}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"headers":{"X-A":"a\nb"}}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"secretRef":{"header":"Authorization"}}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"secretRef":{"name":"gh"}}`,
		`{"url":"https://example.com/x","schedule":{"expr":"* * * * *"},"envelop":{"items":"."}}`,
	}
	for _, raw := range bad {
		if _, err := NormalizeConfig(KindPoll, json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("应被拒（ErrInvalidConfig）：%s → %v", raw, err)
		}
	}
	// 私网地址必须带字段路径（前端红点定位 / e2e 断言 details.causes[].field）。
	var causes *CausesError
	_, err := NormalizeConfig(KindPoll, json.RawMessage(`{"url":"https://169.254.169.254/latest/meta-data","schedule":{"expr":"* * * * *"}}`))
	if !errors.As(err, &causes) || len(causes.Causes) == 0 || causes.Causes[0].Field != "config.url" {
		t.Fatalf("私网地址应给 config.url 的 cause，得到 %v", err)
	}
}

func TestPollURLAllowInsecure(t *testing.T) {
	ok := []string{
		`{"url":"http://localhost:18081/x","allowInsecure":true,"schedule":{"expr":"* * * * *"}}`,
		`{"url":"http://poll.e2e.test/x","allowInsecure":true,"schedule":{"expr":"* * * * *"}}`,
		`{"url":"http://127.0.0.1:18081/x","allowInsecure":true,"schedule":{"expr":"* * * * *"}}`,
	}
	for _, raw := range ok {
		pollConfigOf(t, raw)
	}
	bad := []string{
		`{"url":"http://example.com/x","allowInsecure":true,"schedule":{"expr":"* * * * *"}}`,
		`{"url":"http://localhost/x","schedule":{"expr":"* * * * *"}}`,
	}
	for _, raw := range bad {
		if _, err := NormalizeConfig(KindPoll, json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("应被拒：%s → %v", raw, err)
		}
	}
}

func TestPollEnvelopeItemsPointer(t *testing.T) {
	cfg := ParseConfig(KindPoll, pollConfigOf(t, `{"url":"https://example.com/a","schedule":{"expr":"* * * * *"},"envelope":{"items":"$.data","type":{"pointer":"type"}}}`))
	if cfg.Envelope == nil || cfg.Envelope.Items != "data" || cfg.Envelope.Type == nil || cfg.Envelope.Type.Pointer != "type" {
		t.Fatalf("items 指针与三指针应同套方言：%+v", cfg.Envelope)
	}
	// items 缺省 = 根；归一化后显式写出 "."（字节稳定）。
	got := pollConfigOf(t, `{"url":"https://example.com/a","schedule":{"expr":"* * * * *"},"envelope":{"type":{"pointer":"t"}}}`)
	if !json.Valid(got) || !contains(string(got), `"items":"."`) {
		t.Fatalf("items 缺省应归一成根：%s", got)
	}
	// webhook 不认 items（那里的载荷是单条投递）。
	if _, err := NormalizeConfig(KindWebhook, json.RawMessage(`{"envelope":{"items":"."}}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("webhook 不认 items：%v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestLookupItems(t *testing.T) {
	doc := map[string]any{"data": map[string]any{"items": []any{map[string]any{"id": "1"}}}}
	if items, ok := LookupItems(doc, "data.items"); !ok || len(items) != 1 {
		t.Fatalf("data.items：%v %v", items, ok)
	}
	root := []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}
	if items, ok := LookupItems(root, "."); !ok || len(items) != 2 {
		t.Fatalf("根数组：%v %v", items, ok)
	}
	if _, ok := LookupItems(map[string]any{"data": "x"}, "data"); ok {
		t.Fatal("非数组必须报 false（该轮失败，不是 0 条）")
	}
	if _, ok := LookupItems(doc, "data.missing"); ok {
		t.Fatal("取不到必须报 false")
	}
}

func TestNextPollTimeReusesCron(t *testing.T) {
	cfg := json.RawMessage(`{"url":"https://example.com/a","schedule":{"expr":"*/5 * * * *","tz":"UTC"},"timeoutMs":10000,"maxItems":100}`)
	after := time.Date(2026, 11, 22, 3, 2, 30, 0, time.UTC)
	got, err := NextPollTime(cfg, after)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 22, 3, 5, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	if _, err := NextPollTime(json.RawMessage(`{"url":"https://example.com/a","timeoutMs":10000,"maxItems":100}`), after); err == nil {
		t.Fatal("没有 schedule.expr 必须报错")
	}
}
