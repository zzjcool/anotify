package server

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

func captureInternalErrorLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

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

var leakMarkers = []string{"sql", "database is closed", "sqlite", ".db", "users.", "SELECT", "INSERT"}

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

func TestInternalErrorsAreSanitized(t *testing.T) {
	logs := captureInternalErrorLogs(t)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	prevCreate := rlCreate
	rlCreate = newFixedWindow(1000, time.Minute)
	t.Cleanup(func() { rlCreate = prevCreate })

	mux := http.NewServeMux()
	mux.Handle("/v1/keys", &keysHandler{db: db})
	mux.Handle("/v1/admin/", &adminHandler{db: db})
	mux.HandleFunc("POST /v1/cli-auth/sessions", (&cliAuthHandler{mgr: auth.NewCliAuthManager(db, 0), db: db}).create)
	h := accessLog(mux)

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
			req = req.WithContext(auth.WithUserID(req.Context(), c.user))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		rid := assertSanitized(t, c.name, rec)

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

func TestWriteInternalSanitizes(t *testing.T) {
	logs := captureInternalErrorLogs(t)

	const secret = "sqlite: SECRET-PATH /var/lib/anotify/anotify.db"
	rec := httptest.NewRecorder()
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

func TestWriteAdminErrSanitizesCause(t *testing.T) {
	logs := captureInternalErrorLogs(t)

	const secret = "sql: database is closed"
	rec := httptest.NewRecorder()
	rec.Header().Set(requestIDHeader, "rid-admin-500")
	writeAdminErr(rec, adminInternalErr(errors.New(secret)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Fatalf("adminErr 的 500 响应体泄露了内部错误原文：%s", body)
	}
	if body := rec.Body.String(); !strings.Contains(body, internalErrMsg) {
		t.Fatalf("adminErr 的 500 响应体 = %s, want 含 %q", body, internalErrMsg)
	}
	if !strings.Contains(logs.String(), secret) {
		t.Fatalf("原始错误必须进日志（原文没地方查了）：%s", logs.String())
	}
	if !strings.Contains(logs.String(), "rid-admin-500") {
		t.Fatalf("日志缺 request_id（对不上用户报障）：%s", logs.String())
	}
}

func TestWriteAdminErrNeverEchosMessageOn500(t *testing.T) {
	captureInternalErrorLogs(t)

	const secret = "sql: database is closed"
	rec := httptest.NewRecorder()
	writeAdminErr(rec, &adminErr{code: http.StatusInternalServerError, msg: secret})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Fatalf("500 即使漏了 cause 也不许把 msg 回给客户端：%s", body)
	}
	if body := rec.Body.String(); !strings.Contains(body, internalErrMsg) {
		t.Fatalf("500 响应体 = %s, want 含 %q", body, internalErrMsg)
	}
}

func TestWriteAdminErrKeepsUserFacing4xx(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAdminErr(rec, &adminErr{code: http.StatusConflict, msg: "不能修改自己的角色"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "不能修改自己的角色") {
		t.Fatalf("4xx 的用户面文案必须原样返回：%s", body)
	}
}
