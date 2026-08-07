package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"invalid", slog.LevelInfo},
	}
	for _, tt := range tests {
		got := parseLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"json", "json"},
		{"text", "text"},
		{"", "json"},
		{"invalid", "json"},
	}
	for _, tt := range tests {
		got := parseFormat(tt.input)
		if got != tt.want {
			t.Errorf("parseFormat(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsValidLevel(t *testing.T) {
	valid := []string{"debug", "info", "warn", "warning", "error", ""}
	invalid := []string{"trace", "fatal", "verbose"}
	for _, s := range valid {
		if !isValidLevel(s) {
			t.Errorf("isValidLevel(%q) should be true", s)
		}
	}
	for _, s := range invalid {
		if isValidLevel(s) {
			t.Errorf("isValidLevel(%q) should be false", s)
		}
	}
}

func TestIsValidFormat(t *testing.T) {
	valid := []string{"json", "text", ""}
	invalid := []string{"xml", "yaml", "csv"}
	for _, s := range valid {
		if !isValidFormat(s) {
			t.Errorf("isValidFormat(%q) should be true", s)
		}
	}
	for _, s := range invalid {
		if isValidFormat(s) {
			t.Errorf("isValidFormat(%q) should be false", s)
		}
	}
}

func TestNewRequestID(t *testing.T) {
	id := NewRequestID()
	if len(id) < 8 {
		t.Errorf("request_id too short: %s (len=%d)", id, len(id))
	}
	// Should be URL-safe
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			t.Errorf("request_id contains non-URL-safe char: %c in %s", c, id)
		}
	}
	// Should be unique
	id2 := NewRequestID()
	if id == id2 {
		t.Errorf("request_id should be unique, got same: %s", id)
	}
}

func TestRequestIDContext(t *testing.T) {
	rid := "test-rid-123"
	ctx := WithRequestID(t.Context(), rid)

	got := RequestID(ctx)
	if got != rid {
		t.Errorf("RequestID() = %q, want %q", got, rid)
	}
}

func TestUserIDContext(t *testing.T) {
	uid := "usr_123"
	ctx := WithUserID(t.Context(), uid)

	got := UserID(ctx)
	if got != uid {
		t.Errorf("UserID() = %q, want %q", got, uid)
	}
}

func TestFromContext(t *testing.T) {
	rid := "rid-test"
	uid := "uid-test"
	ctx := WithRequestID(t.Context(), rid)
	ctx = WithUserID(ctx, uid)

	args := FromContext(ctx)
	// Should contain request_id and user_id pairs
	if len(args) != 4 {
		t.Fatalf("FromContext() returned %d args, want 4", len(args))
	}
	if args[0] != "request_id" || args[1] != rid {
		t.Errorf("args[0:2] = %v %v, want request_id %s", args[0], args[1], rid)
	}
	if args[2] != "user_id" || args[3] != uid {
		t.Errorf("args[2:4] = %v %v, want user_id %s", args[2], args[3], uid)
	}
}

func TestFromContext_Empty(t *testing.T) {
	args := FromContext(t.Context())
	if len(args) != 0 {
		t.Errorf("FromContext(empty) returned %d args, want 0", len(args))
	}
}

// TestInit_JSONOutput 验证 Init 后 JSON 格式输出正确。
func TestInit_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	defer slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	slog.Info("test message", "event", "test.event", "key", "value")

	output := buf.String()
	if !strings.Contains(output, `"msg":"test message"`) {
		t.Errorf("JSON output missing msg: %s", output)
	}
	if !strings.Contains(output, `"event":"test.event"`) {
		t.Errorf("JSON output missing event field: %s", output)
	}
	if !strings.Contains(output, `"key":"value"`) {
		t.Errorf("JSON output missing key field: %s", output)
	}
}
