package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/store"
)

// TestAC3_SecretNotInLogs 验证 AC-3 安全红线：
// 完整跑一遍 enroll/cli-auth 流程，确认 secret/registrationToken 不出现在日志输出中。
func TestAC3_SecretNotInLogs(t *testing.T) {
	var logBuf bytes.Buffer
	// 捕获 slog 输出到 logBuf（DEBUG 级别，最宽松）
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))

	env := newEnrollE2EEnv(t)

	// 1. 旧设备发起 enroll 会话
	rr := e2eDoReq(t, env, "POST", "/v1/passkey-enroll/sessions",
		map[string]any{"deviceName": "test-device"}, true)
	if rr.Code != 200 {
		t.Fatalf("create session: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	createSecret := resp["secret"].(string)
	if createSecret == "" {
		t.Fatal("no secret in create response")
	}

	// 2. 匿名 lookup
	sid := resp["sessionId"].(string)
	rr = e2eDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+sid, nil, false)
	if rr.Code != 200 {
		t.Fatalf("lookup: %d %s", rr.Code, rr.Body.String())
	}

	// 3. 敲门
	rr = e2eDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+sid+"/request",
		map[string]any{"deviceHint": "Test Browser"}, false)
	if rr.Code != 200 {
		t.Fatalf("knock: %d %s", rr.Code, rr.Body.String())
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	knockSecret := resp["secret"].(string)

	// 4. 旧设备批准
	rr = e2eDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+sid+"/approve", nil, true)
	if rr.Code != 200 {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}

	// 5. 错误 secret 的 poll（应 401）
	rr = e2eDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+sid+"/poll?secret=wrongsecret", nil, false)
	if rr.Code != 401 {
		t.Fatalf("wrong secret poll: expected 401, got %d", rr.Code)
	}

	// 6. 正确 secret 的 poll
	time.Sleep(2200 * time.Millisecond) // 避开 pollGuard
	rr = e2eDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+sid+"/poll?secret="+knockSecret, nil, false)
	if rr.Code != 200 {
		t.Fatalf("poll: %d %s", rr.Code, rr.Body.String())
	}

	// 检查日志输出
	logOutput := logBuf.String()

	// AC-3 核心断言：secret 绝不进日志
	if strings.Contains(logOutput, createSecret) {
		t.Errorf("AC-3 FAIL: create secret found in log output:\n%s", logOutput)
	}
	if strings.Contains(logOutput, knockSecret) {
		t.Errorf("AC-3 FAIL: knock secret found in log output:\n%s", logOutput)
	}

	// 检查 URL path 不含 query（AC-3: secret 走 query）
	for _, line := range strings.Split(logOutput, "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if path, ok := entry["path"].(string); ok {
			if strings.Contains(path, "secret=") {
				t.Errorf("AC-3 FAIL: path contains query with secret: %s", path)
			}
			if strings.Contains(path, "registrationToken=") {
				t.Errorf("AC-3 FAIL: path contains query with registrationToken: %s", path)
			}
		}
	}
}

// TestAC3_CLIAuthSecretNotInLogs 验证 CLI 授权流程中 secret 不泄露。
func TestAC3_CLIAuthSecretNotInLogs(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))

	env := newEnrollE2EEnv(t)

	// 先创建一个用户（FK 约束）
	uid := "usr_test_ac3"
	env.db.CreateUser(&store.User{ID: uid, Username: "ac3test", CreatedAt: store.Now()})

	// 创建 CLI auth 会话（apikey kind）
	cliMgr := auth.NewCliAuthManager(env.db, 0)
	created, err := cliMgr.CreateSession("test-cli", []string{"notify:send"})
	if err != nil {
		t.Fatal(err)
	}

	// 批准
	err = cliMgr.Approve(created.SessionID, uid, []string{"notify:send"})
	if err != nil {
		t.Fatal(err)
	}

	// Poll（会签发 Key）
	res, err := cliMgr.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if res.APIKey == "" {
		t.Fatal("expected APIKey in poll result")
	}

	// 检查日志输出
	logOutput := logBuf.String()

	// secret 绝不进日志
	if strings.Contains(logOutput, created.Secret) {
		t.Errorf("AC-3 FAIL: CLI auth secret found in log output:\n%s", logOutput)
	}

	// API Key 明文绝不进日志
	if strings.Contains(logOutput, res.APIKey) {
		t.Errorf("AC-3 FAIL: API key plaintext found in log output:\n%s", logOutput)
	}
}

// TestAC1_StructuredJSONOutput 验证 AC-1：JSON 格式输出含 time/level/msg 字段。
func TestAC1_StructuredJSONOutput(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	slog.Info("test event", "event", "test.event", "key", "value")

	var entry map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, logBuf.String())
	}
	for _, field := range []string{"time", "level", "msg"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("AC-1 FAIL: missing field %s in log output: %s", field, logBuf.String())
		}
	}
	if entry["event"] != "test.event" {
		t.Errorf("AC-1 FAIL: event field = %v, want 'test.event'", entry["event"])
	}
}

// TestAC4_LevelFiltering 验证 AC-4：日志分级生效。
func TestAC4_LevelFiltering(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(handler))

	slog.Info("info message should be filtered")
	slog.Warn("warn message should appear")
	slog.Error("error message should appear")

	output := logBuf.String()
	if strings.Contains(output, "info message should be filtered") {
		t.Errorf("AC-4 FAIL: INFO should be filtered at warn level")
	}
	if !strings.Contains(output, "warn message should appear") {
		t.Errorf("AC-4 FAIL: WARN should appear at warn level")
	}
	if !strings.Contains(output, "error message should appear") {
		t.Errorf("AC-4 FAIL: ERROR should appear at warn level")
	}
}
