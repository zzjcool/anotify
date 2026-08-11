package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/store"
)

// fakeBroker 捕获 Publish 的消息。
type fakeBroker struct {
	published []*broker.Message
	err       error
}

func (f *fakeBroker) Publish(ctx context.Context, m *broker.Message) error {
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, m)
	return nil
}
func (f *fakeBroker) Subscribe(ctx context.Context, userID string, tags []string) (broker.Subscription, error) {
	return &fakeSub{ch: make(chan *broker.Message)}, nil
}
func (f *fakeBroker) Ack(ctx context.Context, consumerID, userID string, seq int64) error {
	return nil
}
func (f *fakeBroker) Replay(ctx context.Context, userID string, sinceSeq int64, limit int) ([]*broker.Message, error) {
	return nil, nil
}
func (f *fakeBroker) Close() error { return nil }

type fakeSub struct{ ch chan *broker.Message }

func (s *fakeSub) C() <-chan *broker.Message { return s.ch }
func (s *fakeSub) Close() error              { return nil }

// keyValidatorStub 按预置的 key→(user,scopes) 校验。
func keyValidatorStub(valid map[string]struct {
	user   string
	scopes []string
}) authn.KeyValidator {
	return authn.KeyValidatorFunc(func(ctx context.Context, key string) (string, []string, error) {
		if v, ok := valid[key]; ok {
			return v.user, v.scopes, nil
		}
		return "", nil, errors.New("invalid key")
	})
}

func newNotifyHandler(b broker.Broker, kv authn.KeyValidator, db *store.DB) *NotifyHandler {
	return &NotifyHandler{Broker: b, Keys: kv, Store: db}
}

func post(t *testing.T, h http.Handler, body, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestNotifySuccess 有效 Key + 合法 body → 发布并返回命中设备。
func TestNotifySuccess(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// 外键父用户 + 一台 enabled 设备
	if _, err := db.InsertUser(context.Background(), "usr_1", "tester", "Tester"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	dev := &store.Device{
		ID: "d1", UserID: "usr_1", Enabled: true, StatusFilter: "all",
		Endpoint: "https://push.example.com/d1", P256dh: "p", Auth: "a",
	}
	if err := db.UpsertDevice(context.Background(), dev); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	kv := keyValidatorStub(map[string]struct {
		user   string
		scopes []string
	}{"ant_live_good": {"usr_1", []string{authn.ScopeNotifySend}}})

	fb := &fakeBroker{}
	h := newNotifyHandler(fb, kv, db)

	body := `{"title":"部署完成","status":"success","body":"构建成功","deviceTags":[" 手机 ","手机","工作"]}`
	rr := post(t, h, body, "Bearer ant_live_good")

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", rr.Code, rr.Body)
	}
	var resp NotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("response should contain message id")
	}
	if resp.Matched != 1 {
		t.Fatalf("matched=%d, want 1", resp.Matched)
	}

	// broker 收到了消息
	if len(fb.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(fb.published))
	}
	msg := fb.published[0]
	if msg.UserID != "usr_1" || msg.Status != broker.StatusSuccess || msg.Title != "部署完成" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	// deviceTags 归一化：trim + 去重（" 手机 "/"手机" → 1 个）
	if len(msg.DeviceTags) != 2 {
		t.Fatalf("deviceTags=%v, want 2 normalized tags", msg.DeviceTags)
	}
}

// TestNotifyInvalidKey 无效 Key → 401。
func TestNotifyInvalidKey(t *testing.T) {
	kv := keyValidatorStub(nil)
	fb := &fakeBroker{}
	h := newNotifyHandler(fb, kv, nil)

	rr := post(t, h, `{"title":"x","status":"success"}`, "Bearer ant_live_bad")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
	if len(fb.published) != 0 {
		t.Fatal("should not publish on invalid key")
	}
}

// TestNotifyMissingKey 无 Key → 401。
func TestNotifyMissingKey(t *testing.T) {
	kv := keyValidatorStub(nil)
	h := newNotifyHandler(&fakeBroker{}, kv, nil)
	rr := post(t, h, `{"title":"x","status":"success"}`, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

// TestNotifyInsufficientScope scope 不足 → 403。
func TestNotifyInsufficientScope(t *testing.T) {
	kv := keyValidatorStub(map[string]struct {
		user   string
		scopes []string
	}{"ant_recv_only": {"usr_1", []string{authn.ScopeNotifyReceive}}})
	h := newNotifyHandler(&fakeBroker{}, kv, nil)

	rr := post(t, h, `{"title":"x","status":"success"}`, "Bearer ant_recv_only")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rr.Code)
	}
}

// TestNotifyBadRequests 各类 400。
func TestNotifyBadRequests(t *testing.T) {
	kv := keyValidatorStub(map[string]struct {
		user   string
		scopes []string
	}{"k": {"usr_1", []string{authn.ScopeNotifySend}}})
	h := newNotifyHandler(&fakeBroker{}, kv, nil)

	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{not json`},
		{"缺 title", `{"status":"success"}`},
		{"非法 status", `{"title":"x","status":"bogus"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := post(t, h, c.body, "Bearer k")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400, body=%s", rr.Code, rr.Body)
			}
		})
	}
}

// TestNotifyZeroDevices 用户无设备 → 200 但 matched=0（Agent 可见）。
func TestNotifyZeroDevices(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.InsertUser(context.Background(), "usr_nothing", "nobody", "Nobody"); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	kv := authn.KeyValidatorFunc(func(ctx context.Context, key string) (string, []string, error) {
		return "usr_nothing", []string{authn.ScopeNotifySend}, nil
	})

	h := newNotifyHandler(&fakeBroker{}, kv, db)
	rr := post(t, h, `{"title":"x","status":"success"}`, "Bearer k")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", rr.Code, rr.Body)
	}
	var resp NotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Matched != 0 {
		t.Fatalf("matched=%d, want 0", resp.Matched)
	}
}

// TestNormalizeTags 归一化规则。
func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		in   []string
		want int
	}{
		{nil, 0},
		{[]string{}, 0},
		{[]string{"", "  "}, 0},
		{[]string{"a", "a", "A"}, 1}, // 大小写不敏感去重
		{[]string{" a ", "b"}, 2},    // trim
		{[]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, 10}, // 上限 10
	}
	for _, c := range cases {
		if got := len(normalizeTags(c.in)); got != c.want {
			t.Fatalf("normalizeTags(%v) len=%d, want %d", c.in, got, c.want)
		}
	}
	// 超长截断
	long := strings.Repeat("x", 40)
	out := normalizeTags([]string{long})
	if len(out[0]) != maxTagLen {
		t.Fatalf("tag len=%d, want %d", len(out[0]), maxTagLen)
	}
}

// postTest 构造一个带登录会话 userID 的 POST /v1/test-notify 请求。
// 直接调用 ServeTestNotify（会话鉴权版），从 context 取 userID（模拟 sessMW 注入）。
func postTest(t *testing.T, h *NotifyHandler, userID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/test-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req = req.WithContext(auth.WithUserID(req.Context(), userID))
	}
	rr := httptest.NewRecorder()
	h.ServeTestNotify(rr, req)
	return rr
}

// TestServeTestNotifySuccess 已登录会话 + 合法 body → 发布并返回命中设备。
func TestServeTestNotifySuccess(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.InsertUser(context.Background(), "usr_1", "tester", "Tester"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	dev := &store.Device{
		ID: "d1", UserID: "usr_1", Enabled: true, StatusFilter: "all",
		Endpoint: "https://push.example.com/d1", P256dh: "p", Auth: "a",
	}
	if err := db.UpsertDevice(context.Background(), dev); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	fb := &fakeBroker{}
	h := newNotifyHandler(fb, nil, db)

	body := `{"title":"测试通知","status":"success","body":"hi"}`
	rr := postTest(t, h, "usr_1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", rr.Code, rr.Body)
	}
	var resp NotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("response should contain message id")
	}
	if resp.Matched != 1 {
		t.Fatalf("matched=%d, want 1", resp.Matched)
	}
	if len(fb.published) != 1 {
		t.Fatalf("broker published %d, want 1", len(fb.published))
	}
	m := fb.published[0]
	if m.UserID != "usr_1" {
		t.Fatalf("msg.UserID=%q, want usr_1", m.UserID)
	}
	if m.Title != "测试通知" {
		t.Fatalf("msg.Title=%q, want 测试通知", m.Title)
	}
}

// TestServeTestNotifyDefaults 缺省 title/status → 默认值（测试通知 / info）。
func TestServeTestNotifyDefaults(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.InsertUser(context.Background(), "usr_2", "u2", "U2"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	fb := &fakeBroker{}
	h := newNotifyHandler(fb, nil, db)

	rr := postTest(t, h, "usr_2", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", rr.Code, rr.Body)
	}
	m := fb.published[0]
	if m.Title != "测试通知" {
		t.Fatalf("default title=%q, want 测试通知", m.Title)
	}
	if m.Status != broker.StatusInfo {
		t.Fatalf("default status=%q, want %s", m.Status, broker.StatusInfo)
	}
}

// TestServeTestNotifyNoSession 未登录（无 userID）→ 401。
func TestServeTestNotifyNoSession(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	h := newNotifyHandler(&fakeBroker{}, nil, db)
	rr := postTest(t, h, "", `{"title":"x"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401, body=%s", rr.Code, rr.Body)
	}
}

// TestServeTestNotifyBadStatus 非法 status → 400。
func TestServeTestNotifyBadStatus(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.InsertUser(context.Background(), "usr_3", "u3", "U3"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	h := newNotifyHandler(&fakeBroker{}, nil, db)
	rr := postTest(t, h, "usr_3", `{"status":"bogus"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body=%s", rr.Code, rr.Body)
	}
}

// TestServeTestNotifyMethodNotAllowed 非 POST → 405。
func TestServeTestNotifyMethodNotAllowed(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	h := newNotifyHandler(&fakeBroker{}, nil, db)
	req := httptest.NewRequest(http.MethodGet, "/v1/test-notify", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), "usr_x"))
	rr := httptest.NewRecorder()
	h.ServeTestNotify(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rr.Code)
	}
}
