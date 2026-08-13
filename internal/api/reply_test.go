package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/store"
)

// stubRateLimiter is a test RateLimiter that allows N requests then blocks.
type stubRateLimiter struct {
	allowCount int
	max        int
}

func (s *stubRateLimiter) Allow(key string) bool {
	s.allowCount++
	if s.allowCount <= s.max {
		return true
	}
	return false
}

// setupReplyTest creates a NotifyHandler with a real broker+store for reply testing.
func setupReplyTest(t *testing.T) (*NotifyHandler, *store.DB, string) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	bk := broker.NewSQLite(db)
	uid := "usr_test_reply"

	// Create user
	if err := db.CreateUser(&store.User{ID: uid, Username: "test", DisplayName: "test", Role: store.RoleMember, CreatedAt: store.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	h := &NotifyHandler{
		Broker:           bk,
		Store:            db,
		ReplyRateLimiter: &stubRateLimiter{max: 100},
	}
	return h, db, uid
}

// publishOriginalMessage simulates an agent reporting a task notification.
// Returns the message ID.
func publishOriginalMessage(t *testing.T, h *NotifyHandler, uid string) string {
	t.Helper()
	now := time.Now().UTC()
	payload, _ := json.Marshal(NotifyRequest{
		AgentID:    "pi@testhost:a1b2",
		SessionID:  "sess_test_123",
		Cwd:        "/tmp/project",
		AgentState: "done",
		Title:      "task done",
		Body:       "completed",
		Kind:       "task",
	})
	msg := &broker.Message{
		ID:         store.NewMessageID(),
		UserID:     uid,
		Title:      "task done",
		AgentState: broker.AgentStateDone,
		Severity:   "info",
		Kind:       "task",
		Body:       "completed",
		DeviceTags: []string{"agent:sess_test_123"},
		Priority:   "normal",
		TTLSeconds: 86400,
		Payload:    payload,
		CreatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
	if err := h.Broker.Publish(context.Background(), msg); err != nil {
		t.Fatalf("publish original: %v", err)
	}
	return msg.ID
}

func TestReply_Success(t *testing.T) {
	h, _, uid := setupReplyTest(t)
	origID := publishOriginalMessage(t, h, uid)

	body := `{"replyTo":"` + origID + `","body":"继续改下样式"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	req.SetPathValue("uid", uid)
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	h.ServeReply(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ReplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Routed {
		t.Error("expected routed=true")
	}
	if resp.AgentRoute != "agent:sess_test_123" {
		t.Errorf("expected agent route agent:sess_test_123, got %s", resp.AgentRoute)
	}
	if resp.ID == "" {
		t.Error("expected non-empty message ID")
	}
}

func TestReply_MissingFields(t *testing.T) {
	h, _, uid := setupReplyTest(t)

	// Missing replyTo
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(`{"body":"hi"}`))
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()
	h.ServeReply(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing replyTo, got %d", rr.Code)
	}

	// Missing body
	req2 := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(`{"replyTo":"ntf_x"}`))
	req2 = req2.WithContext(auth.WithUserID(req2.Context(), uid))
	rr2 := httptest.NewRecorder()
	h.ServeReply(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing body, got %d", rr2.Code)
	}
}

func TestReply_OriginalNotFound(t *testing.T) {
	h, _, uid := setupReplyTest(t)

	body := `{"replyTo":"ntf_nonexistent","body":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	h.ServeReply(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent original, got %d", rr.Code)
	}
}

func TestReply_NoAgentIdentifier(t *testing.T) {
	h, db, uid := setupReplyTest(t)

	// Insert a message with no agentId in payload (like test-notify)
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{
		"source":  "test-notify",
		"agentId": "", // no agent
	})
	msg := &broker.Message{
		ID:         store.NewMessageID(),
		UserID:     uid,
		Title:      "test",
		AgentState: broker.AgentStateDone,
		Kind:       "task",
		Priority:   "normal",
		TTLSeconds: 86400,
		Payload:    payload,
		CreatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
	if err := h.Broker.Publish(context.Background(), msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	_ = db // keep reference
	body := `{"replyTo":"` + msg.ID + `","body":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	h.ServeReply(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for no agent identifier, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReply_RateLimited(t *testing.T) {
	h, _, uid := setupReplyTest(t)
	origID := publishOriginalMessage(t, h, uid)

	// Replace with a strict rate limiter (max 1)
	h.ReplyRateLimiter = &stubRateLimiter{max: 1}

	body := `{"replyTo":"` + origID + `","body":"first"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()
	h.ServeReply(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", rr.Code)
	}

	// Second request should be rate limited
	body2 := `{"replyTo":"` + origID + `","body":"second"}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body2))
	req2 = req2.WithContext(auth.WithUserID(req2.Context(), uid))
	rr2 := httptest.NewRecorder()
	h.ServeReply(rr2, req2)
	if rr2.Code != 429 {
		t.Fatalf("expected 429 for rate limited, got %d", rr2.Code)
	}
}

func TestReply_BodyTooLong(t *testing.T) {
	h, _, uid := setupReplyTest(t)
	origID := publishOriginalMessage(t, h, uid)

	longBody := strings.Repeat("x", maxReplyBodyLen+1)
	body := `{"replyTo":"` + origID + `","body":"` + longBody + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	h.ServeReply(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for body too long, got %d", rr.Code)
	}
}

func TestReply_Unauthorized(t *testing.T) {
	h, _, _ := setupReplyTest(t)

	body := `{"replyTo":"ntf_x","body":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reply", strings.NewReader(body))
	rr := httptest.NewRecorder()

	h.ServeReply(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rr.Code)
	}
}
