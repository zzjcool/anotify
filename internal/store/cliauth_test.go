package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestCliAuthSession_RoundTrip(t *testing.T) {
	db := newTestDB(t)

	s := &CliAuthSession{
		ID:              "cas_test1",
		SecretHash:      "abc123hash",
		UserCode:        "ABCD1234",
		DeviceName:      "my-macbook",
		ScopesRequested: []string{"notify:send", "notify:receive"},
		Status:          CliAuthPending,
		CreatedAt:       1000,
		ExpiresAt:       1600,
	}

	// Create → Get by ID
	if err := db.CreateCliAuthSession(s); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetCliAuthSession("cas_test1")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	assertCliAuthSession(t, got, s)

	// Get by code
	gotByCode, err := db.GetCliAuthSessionByCode("ABCD1234")
	if err != nil {
		t.Fatalf("get by code: %v", err)
	}
	assertCliAuthSession(t, gotByCode, s)

	// NULL 字段往返一致性：批准前 ScopesGranted / UserID / KeyID / ConsumedAt 应为零值
	if got.ScopesGranted != nil {
		t.Errorf("批准前 ScopesGranted 应为 nil, got %v", got.ScopesGranted)
	}
	if got.UserID != "" {
		t.Errorf("批准前 UserID 应为空, got %q", got.UserID)
	}
	if got.KeyID != "" {
		t.Errorf("批准前 KeyID 应为空, got %q", got.KeyID)
	}
	if got.ConsumedAt.Valid {
		t.Errorf("批准前 ConsumedAt 应为 NULL, got %v", got.ConsumedAt)
	}
}

func TestCliAuthSession_ApproveDeny_AtomicConflict(t *testing.T) {
	db := newTestDB(t)

	s := &CliAuthSession{
		ID:              "cas_approve1",
		SecretHash:      "hash",
		UserCode:        "WXYZ9999",
		DeviceName:      "server-1",
		ScopesRequested: []string{"notify:send"},
		Status:          CliAuthPending,
		CreatedAt:       1000,
		ExpiresAt:       1600,
	}
	if err := db.CreateCliAuthSession(s); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 批准成功
	ok, err := db.UpdateCliAuthSessionApproved("cas_approve1", "usr_1", []string{"notify:send"}, 1100)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !ok {
		t.Fatalf("approve 应成功")
	}

	// 验证批准后字段
	got, _ := db.GetCliAuthSession("cas_approve1")
	if got.Status != CliAuthApproved {
		t.Errorf("status got %q want %q", got.Status, CliAuthApproved)
	}
	if got.UserID != "usr_1" {
		t.Errorf("userID got %q want usr_1", got.UserID)
	}
	if len(got.ScopesGranted) != 1 || got.ScopesGranted[0] != "notify:send" {
		t.Errorf("scopesGranted got %v", got.ScopesGranted)
	}

	// 重复批准（已非 pending）→ ok=false
	ok2, err := db.UpdateCliAuthSessionApproved("cas_approve1", "usr_2", []string{"notify:send"}, 1200)
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if ok2 {
		t.Errorf("重复批准应返回 ok=false")
	}

	// Deny 测试
	s2 := &CliAuthSession{
		ID: "cas_deny1", SecretHash: "h", UserCode: "DENY0001",
		DeviceName: "dev", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	db.CreateCliAuthSession(s2)
	ok3, err := db.UpdateCliAuthSessionDenied("cas_deny1")
	if err != nil || !ok3 {
		t.Fatalf("deny: ok=%v err=%v", ok3, err)
	}
	// 已 denied 的不能 deny
	ok4, _ := db.UpdateCliAuthSessionDenied("cas_deny1")
	if ok4 {
		t.Errorf("denied 后再 deny 应返回 ok=false")
	}
}

func TestCliAuthSession_ExpiredMigration(t *testing.T) {
	db := newTestDB(t)

	s := &CliAuthSession{
		ID: "cas_expire1", SecretHash: "h", UserCode: "EXPR0001",
		DeviceName: "dev", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	db.CreateCliAuthSession(s)

	// MarkExpired 把 pending → expired
	if err := db.MarkCliAuthSessionExpired("cas_expire1"); err != nil {
		t.Fatalf("mark expired: %v", err)
	}
	got, _ := db.GetCliAuthSession("cas_expire1")
	if got.Status != CliAuthExpired {
		t.Errorf("status got %q want %q", got.Status, CliAuthExpired)
	}

	// 已 expired 的再 mark 不影响
	_ = db.MarkCliAuthSessionExpired("cas_expire1")
	got2, _ := db.GetCliAuthSession("cas_expire1")
	if got2.Status != CliAuthExpired {
		t.Errorf("expired 后再 mark 不应改变状态, got %q", got2.Status)
	}
}

func TestCliAuthSession_ConsumeAndCreateKey(t *testing.T) {
	db := newTestDB(t)
	// 先建用户（FK 约束）
	u := &User{ID: "usr_consume", Username: "consume-test", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// pending 会话
	s := &CliAuthSession{
		ID: "cas_consume1", SecretHash: "h", UserCode: "CONS0001",
		DeviceName: "dev", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	db.CreateCliAuthSession(s)

	// 未 approved 时消费 → alreadyConsumed=true（UPDATE 0 行）
	key := &APIKey{
		ID: "evt_key1", UserID: "usr_consume", Name: "agent:dev",
		KeyHash: "$argon2id$fake", Prefix: "ant_send_abc",
		Scopes: []string{"notify:send"}, Enabled: true, CreatedAt: Now(),
	}
	already, err := db.ConsumeCliAuthSessionAndCreateKey("cas_consume1", key, 1500)
	if err != nil {
		t.Fatalf("consume pending: %v", err)
	}
	if !already {
		t.Errorf("未 approved 的会话消费应返回 alreadyConsumed=true")
	}
	// 不应创建 Key
	if _, err := db.GetAPIKeyByPrefix("ant_send_abc"); err == nil {
		t.Errorf("未 approved 时不应创建 Key")
	}

	// 批准后消费
	_, _ = db.UpdateCliAuthSessionApproved("cas_consume1", "usr_consume", []string{"notify:send"}, 1400)

	already2, err := db.ConsumeCliAuthSessionAndCreateKey("cas_consume1", key, 1500)
	if err != nil {
		t.Fatalf("consume approved: %v", err)
	}
	if already2 {
		t.Errorf("approved 会话首次消费应返回 alreadyConsumed=false")
	}

	// 验证会话 consumed + key_id 回填
	got, _ := db.GetCliAuthSession("cas_consume1")
	if got.Status != CliAuthConsumed {
		t.Errorf("status got %q want %q", got.Status, CliAuthConsumed)
	}
	if got.KeyID != "evt_key1" {
		t.Errorf("keyID got %q want evt_key1", got.KeyID)
	}
	if !got.ConsumedAt.Valid || got.ConsumedAt.Int64 != 1500 {
		t.Errorf("consumedAt got %v want 1500", got.ConsumedAt)
	}

	// Key 已创建
	gotKey, err := db.GetAPIKeyByPrefix("ant_send_abc")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if gotKey.Name != "agent:dev" || !gotKey.Enabled {
		t.Errorf("key got %+v", gotKey)
	}

	// 二次消费 → alreadyConsumed=true，不再创建 Key
	already3, err := db.ConsumeCliAuthSessionAndCreateKey("cas_consume1", key, 1600)
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	if !already3 {
		t.Errorf("已 consumed 的会话二次消费应返回 alreadyConsumed=true")
	}
}

func TestCliAuthSession_DeleteExpired(t *testing.T) {
	db := newTestDB(t)

	// 两个会话，一个已过期（expiresAt=500），一个未过期（expiresAt=9999）
	s1 := &CliAuthSession{
		ID: "cas_old1", SecretHash: "h", UserCode: "OLD00001",
		DeviceName: "dev", ScopesRequested: []string{"notify:send"},
		Status: CliAuthExpired, CreatedAt: 100, ExpiresAt: 500,
	}
	s2 := &CliAuthSession{
		ID: "cas_new1", SecretHash: "h", UserCode: "NEW00001",
		DeviceName: "dev", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 9999,
	}
	db.CreateCliAuthSession(s1)
	db.CreateCliAuthSession(s2)

	// 删除 expiresAt < 1000 的（即 s1）
	if err := db.DeleteExpiredCliAuthSessions(1000); err != nil {
		t.Fatalf("delete expired: %v", err)
	}

	if _, err := db.GetCliAuthSession("cas_old1"); err == nil {
		t.Errorf("过期会话应被删除")
	}
	if _, err := db.GetCliAuthSession("cas_new1"); err != nil {
		t.Errorf("未过期会话应保留: %v", err)
	}
}

func TestCliAuthSession_UserCodeUnique(t *testing.T) {
	db := newTestDB(t)

	s1 := &CliAuthSession{
		ID: "cas_dup1", SecretHash: "h", UserCode: "UNIQ0001",
		DeviceName: "dev1", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	if err := db.CreateCliAuthSession(s1); err != nil {
		t.Fatalf("create s1: %v", err)
	}

	// 重复 userCode 应返回 ErrDuplicateUserCode（可判别的 typed error）
	s2 := &CliAuthSession{
		ID: "cas_dup2", SecretHash: "h", UserCode: "UNIQ0001",
		DeviceName: "dev2", ScopesRequested: []string{"notify:send"},
		Status: CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	err := db.CreateCliAuthSession(s2)
	if err == nil {
		t.Errorf("重复 userCode 应报 UNIQUE 约束错误")
	}
	if !errors.Is(err, ErrDuplicateUserCode) {
		t.Errorf("重复 userCode 应返回 ErrDuplicateUserCode, got %v", err)
	}
}

func TestCliAuthSession_NotFound(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.GetCliAuthSession("cas_ghost"); err != ErrNotFound {
		t.Errorf("GetCliAuthSession 不存在应返回 ErrNotFound, got %v", err)
	}
	if _, err := db.GetCliAuthSessionByCode("GHOST000"); err != ErrNotFound {
		t.Errorf("GetCliAuthSessionByCode 不存在应返回 ErrNotFound, got %v", err)
	}
}

// assertCliAuthSession 对比核心字段。
func assertCliAuthSession(t *testing.T, got, want *CliAuthSession) {
	t.Helper()
	if got.ID != want.ID || got.SecretHash != want.SecretHash ||
		got.UserCode != want.UserCode || got.DeviceName != want.DeviceName ||
		got.Status != want.Status || got.CreatedAt != want.CreatedAt ||
		got.ExpiresAt != want.ExpiresAt {
		t.Errorf("字段不匹配:\n got  %+v\n want %+v", got, want)
	}
	if len(got.ScopesRequested) != len(want.ScopesRequested) {
		t.Errorf("ScopesRequested len got %d want %d", len(got.ScopesRequested), len(want.ScopesRequested))
	} else {
		for i, sc := range want.ScopesRequested {
			if got.ScopesRequested[i] != sc {
				t.Errorf("ScopesRequested[%d] got %q want %q", i, got.ScopesRequested[i], sc)
			}
		}
	}
}

// 确保 consumed_at NULL 往返不 panic。
var _ = sql.NullInt64{}
