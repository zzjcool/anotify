package server

// #624：500 的错误响应不再透传内部错误原文。
//
// 为什么需要这一条：原文（`sql: database is closed` / `UNIQUE constraint failed:
// users.username` / 文件路径）曾直接出现在响应体里，任何一个能触发 500 的调用方
// 都能免费拿到库结构与部署路径；而排障真正需要的原文在日志里、且日志必须能和
// 用户看到的 requestId 对上。
//
// 手法：真 store（`:memory:`）+ 真 handler + 真 accessLog 中间件，然后把库关掉
// —— 每个 handler 的读路径都会拿到 `sql: database is closed`。不关库拿不到这条
// 确定性的 500（写入路径要先过鉴权，鉴权本身就会先红）。
//
// 为什么三条以上的 handler：只测一条等于测 `writeInternal` 本身；这里要钉的是
// 「所有 500 出口都换过口径」——各自的调用点不同（keys / admin / cli-auth）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// captureInternalErrorLogs 把 slog 输出抓进缓冲区。
//
// 必须在用例结束恢复 slog.Default：本包 TestMain 在 slog.Default 上挂了
// 「关库之后还在写库」的检测器（postclose_guard_test.go），不恢复会让它漏掉
// 本用例之后的记录（同 retention_seed_test.go 的 captureRetentionLogs）。
func captureInternalErrorLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// internalErrorLog 找出 event=server.internal_error 且 request_id 匹配的那条日志。
func internalErrorLog(t *testing.T, buf *bytes.Buffer, requestID string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v (%s)", err, line)
		}
		if m["event"] == "server.internal_error" && m["request_id"] == requestID {
			return m
		}
	}
	t.Fatalf("没找到 request_id=%q 的 server.internal_error 日志：%s", requestID, buf.String())
	return nil
}

// leakMarkers 是**绝不允许**出现在响应体里的内部串。
var leakMarkers = []string{"sql", "database is closed", "sqlite", ".db", "users.", "SELECT", "INSERT"}

// assertSanitized 断言一次 500 响应的三条性质：固定文案、无内部串、带 request id。
// 返回响应头里的 request id（调用方拿它去日志里对账）。
func assertSanitized(t *testing.T, label string, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("%s: status = %d, want 500（body=%s）", label, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, marker := range leakMarkers {
		if strings.Contains(body, marker) {
			t.Errorf("%s: 500 响应体泄露了内部错误原文（含 %q）：%s", label, marker, body)
		}
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%s: 500 响应体不是 JSON: %v (%s)", label, err, body)
	}
	if len(got) != 1 || got["error"] != internalErrMsg {
		t.Errorf("%s: 500 响应体 = %s, want exactly {\"error\":%q}", label, body, internalErrMsg)
	}
	rid := rec.Header().Get(requestIDHeader)
	if rid == "" {
		t.Fatalf("%s: 响应头 %s 为空（前端靠它显示报障用的 requestId）", label, requestIDHeader)
	}
	return rid
}

// TestInternalErrorsAreSanitized 是 #624 第 1 条的整链断言：三个不同 handler 的 500
// 都只回固定文案，而原文与同一个 request id 出现在日志里。
func TestInternalErrorsAreSanitized(t *testing.T) {
	logs := captureInternalErrorLogs(t)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// cli-auth 的 create 是匿名端点，自带 IP 维限流：用例自己给足额度并把全局窗口还回去，
	// 免得依赖其他用例跑过的次数（同 recovery_test.go 的做法）。
	prevCreate := rlCreate
	rlCreate = newFixedWindow(1000, time.Minute)
	t.Cleanup(func() { rlCreate = prevCreate })

	mux := http.NewServeMux()
	mux.Handle("/v1/keys", &keysHandler{db: db})
	mux.Handle("/v1/admin/", &adminHandler{db: db})
	mux.HandleFunc("POST /v1/cli-auth/sessions", (&cliAuthHandler{mgr: auth.NewCliAuthManager(db, 0), db: db}).create)
	h := accessLog(mux)

	// 关库：handler 里的第一条读查询就会拿到 `sql: database is closed`。
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		user   string
	}{
		{"GET /v1/keys", http.MethodGet, "/v1/keys", "", "usr_internal_err"},
		{"GET /v1/admin/users", http.MethodGet, "/v1/admin/users", "", ""},
		{"GET /v1/admin/stats", http.MethodGet, "/v1/admin/stats", "", ""},
		{"POST /v1/cli-auth/sessions", http.MethodPost, "/v1/cli-auth/sessions", `{"deviceName":"t","scopes":["notify:send"]}`, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.user != "" {
			// keys 的 list 要求已登录的 user_id（会话中间件在本用例之外，这里直接注入）。
			req = req.WithContext(auth.WithUserID(req.Context(), c.user))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		rid := assertSanitized(t, c.name, rec)

		// 同一个 request id 必须能在日志里找到那条原文：排障靠这条线索。
		entry := internalErrorLog(t, logs, rid)
		if lvl, _ := entry["level"].(string); lvl != "ERROR" {
			t.Errorf("%s: 500 日志 level = %v, want ERROR", c.name, entry["level"])
		}
		if raw, _ := entry["error"].(string); !strings.Contains(raw, "database is closed") {
			t.Errorf("%s: 日志 error = %v, want 含原始错误串 %q", c.name, entry["error"], "database is closed")
		}
		if ua, ok := entry["request_id"].(string); !ok || ua != rid {
			t.Errorf("%s: 日志 request_id = %v, want %q", c.name, entry["request_id"], rid)
		}
	}
}

// TestRequestIDHeaderOnProductionChain 钉住「每个响应都带 request id 头」这件事走的是**真实装配**
// （gzipJSON(accessLog(mux))），而不是只有上一个用例里手搭的那条链。
// 前端读的就是这个响应头（web/partials.js 的 responseRequestID），
// 少了它，「带上 requestId 联系管理员」这句文案就无从兑现。
func TestRequestIDHeaderOnProductionChain(t *testing.T) {
	app := newProductionAppForTest(t)
	seen := make(map[string]string, 2)
	for _, path := range []string{"/health", "/v1/keys"} {
		rr := appDo(t, app, http.MethodGet, path, nil)
		rid := rr.Header().Get(requestIDHeader)
		if rid == "" {
			t.Fatalf("GET %s 的响应缺 %s 头（装配链是 gzipJSON(accessLog(mux))）", path, requestIDHeader)
		}
		seen[path] = rid
	}
	if seen["/health"] == seen["/v1/keys"] {
		t.Fatalf("两个请求拿到同一个 request id %q（应是每请求一个新值）", seen["/health"])
	}
}

// TestWriteInternalSanitizes 直接钉 500 出口本身：任意内部原文都不进响应体，
// 但必须原样进日志（否则这条 500 就变成「查不到原因」的哑错误）。
func TestWriteInternalSanitizes(t *testing.T) {
	logs := captureInternalErrorLogs(t)

	const secret = "sqlite: SECRET-PATH /var/lib/anotify/anotify.db"
	rec := httptest.NewRecorder()
	// accessLog 在真实链路上会先写好这个头；这里显式给一个值，同时钉住
	// 「logInternal 从响应头回读 request id」这条约定。
	rec.Header().Set(requestIDHeader, "rid-sanitize-test")
	writeInternal(rec, errors.New(secret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "SECRET-PATH") || strings.Contains(body, "anotify.db") {
		t.Fatalf("响应体泄露了内部错误原文：%s", body)
	}
	if !strings.Contains(body, internalErrMsg) {
		t.Fatalf("响应体 = %s, want 含 %q", body, internalErrMsg)
	}
	if !strings.Contains(logs.String(), secret) {
		t.Fatalf("日志里没有原始错误串（原文没地方查了）：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "rid-sanitize-test") {
		t.Fatalf("日志里没有 request_id（对不上用户报障）：%s", logs.String())
	}
}
