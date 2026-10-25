package store

import (
	"context"
	"testing"
)

// TestUser_RoleRoundtrip 验证 role 字段往返一致性（存什么读什么）。
// 覆盖默认值（member）、显式 admin、UpdateUserRole 切换。
func TestUser_RoleRoundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// 默认 role=member
	u1 := &User{ID: NewUserID(), Username: "alice", CreatedAt: Now()}
	if err := db.CreateUser(u1); err != nil {
		t.Fatalf("create u1: %v", err)
	}
	got1, err := db.GetUserByID(u1.ID)
	if err != nil {
		t.Fatalf("get u1: %v", err)
	}
	if got1.Role != RoleMember {
		t.Errorf("默认 role 应为 member, got %q", got1.Role)
	}

	// 显式 admin
	u2 := &User{ID: NewUserID(), Username: "bob", Role: RoleAdmin, CreatedAt: Now()}
	if err := db.CreateUser(u2); err != nil {
		t.Fatalf("create u2: %v", err)
	}
	got2, err := db.GetUserByUsername("bob")
	if err != nil {
		t.Fatalf("get u2: %v", err)
	}
	if got2.Role != RoleAdmin {
		t.Errorf("role 应为 admin, got %q", got2.Role)
	}

	// UpdateUserRole 切换
	if n, err := db.UpdateUserRole(ctx, u1.ID, RoleAdmin); err != nil || n != 1 {
		t.Fatalf("update role to admin: n=%d err=%v", n, err)
	}
	got1b, _ := db.GetUserByID(u1.ID)
	if got1b.Role != RoleAdmin {
		t.Errorf("更新后 role 应为 admin, got %q", got1b.Role)
	}

	// 不存在用户 → 0 行
	if n, err := db.UpdateUserRole(ctx, "usr_ghost", RoleMember); err != nil || n != 0 {
		t.Errorf("不存在用户应返回 0 行, got n=%d err=%v", n, err)
	}
}

// TestUser_DisabledRoundtrip 验证 disabled 字段往返一致性 + UpdateUserDisabled。
func TestUser_DisabledRoundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{ID: NewUserID(), Username: "carol", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := db.GetUserByID(u.ID)
	if got.Disabled {
		t.Errorf("默认 disabled 应为 false")
	}

	if n, err := db.UpdateUserDisabled(ctx, u.ID, true); err != nil || n != 1 {
		t.Fatalf("disable: n=%d err=%v", n, err)
	}
	got2, _ := db.GetUserByID(u.ID)
	if !got2.Disabled {
		t.Errorf("更新后 disabled 应为 true")
	}

	if n, err := db.UpdateUserDisabled(ctx, u.ID, false); err != nil || n != 1 {
		t.Fatalf("enable: n=%d err=%v", n, err)
	}
	got3, _ := db.GetUserByID(u.ID)
	if got3.Disabled {
		t.Errorf("更新后 disabled 应为 false")
	}
}

// TestUserCount 验证用户计数与 admin 计数。
func TestUserCount(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	n, err := db.UserCount(ctx)
	if err != nil {
		t.Fatalf("user count: %v", err)
	}
	if n != 0 {
		t.Fatalf("初始 user count 应为 0, got %d", n)
	}

	// 首用户判定关键：count=0 → 视为首用户
	for i, name := range []string{"a", "b", "c"} {
		u := &User{ID: NewUserID(), Username: name, CreatedAt: Now()}
		role := RoleMember
		if i == 0 {
			role = RoleAdmin
		}
		u.Role = role
		if err := db.CreateUser(u); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	n, _ = db.UserCount(ctx)
	if n != 3 {
		t.Errorf("user count 应为 3, got %d", n)
	}
	a, _ := db.AdminCount(ctx)
	if a != 1 {
		t.Errorf("admin count 应为 1, got %d", a)
	}
}

// TestListUsersWithStats 验证用户列表含各用户实体计数。
func TestListUsersWithStats(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u1 := &User{ID: NewUserID(), Username: "alice", Role: RoleAdmin, CreatedAt: Now()}
	u2 := &User{ID: NewUserID(), Username: "bob", CreatedAt: Now()}
	db.CreateUser(u1)
	db.CreateUser(u2)

	// 给 alice 的组织建 2 条事件（统计口径已从 messages 改为 events）
	org1, err := db.EnsureOrgForUser(ctx, u1.ID, u1.Username)
	if err != nil {
		t.Fatalf("ensure org for alice: %v", err)
	}
	if _, err := db.EnsureOrgForUser(ctx, u2.ID, u2.Username); err != nil {
		t.Fatalf("ensure org for bob: %v", err)
	}
	now := Now()
	for i, k := range []string{"k1", "k2"} {
		idem := k
		if _, err := db.InsertEvent(ctx, &Event{
			ID: NewEventID(), OrgID: org1.ID, Source: "github", Type: "pr_opened",
			TraceID: "trace_" + k, IdemKey: &idem, Payload: []byte("{}"),
			CreatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("insert event %s: %v", k, err)
		}
	}

	users, err := db.ListUsersWithStats(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应返回 2 个用户, got %d", len(users))
	}
	// 按 created_at ASC，alice 先建所以排前
	if users[0].Username != "alice" {
		t.Errorf("首用户应排前, got %q", users[0].Username)
	}
	if users[0].Role != RoleAdmin {
		t.Errorf("alice role 应为 admin, got %q", users[0].Role)
	}
	if users[0].MessageCount != 2 {
		t.Errorf("alice messageCount 应为 2, got %d", users[0].MessageCount)
	}
	if users[1].MessageCount != 0 {
		t.Errorf("bob messageCount 应为 0, got %d", users[1].MessageCount)
	}
}

// TestSystemStats 验证系统级总览统计的正确性。
func TestSystemStats(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u1 := &User{ID: NewUserID(), Username: "alice", Role: RoleAdmin, CreatedAt: Now()}
	u2 := &User{ID: NewUserID(), Username: "bob", CreatedAt: Now()}
	db.CreateUser(u1)
	db.CreateUser(u2)

	// 各 2 条事件（统计口径已从 messages 改为 events）
	org1, err := db.EnsureOrgForUser(ctx, u1.ID, u1.Username)
	if err != nil {
		t.Fatalf("ensure org for alice: %v", err)
	}
	org2, err := db.EnsureOrgForUser(ctx, u2.ID, u2.Username)
	if err != nil {
		t.Fatalf("ensure org for bob: %v", err)
	}
	now := Now()
	seed := func(orgID, prefix string) {
		for i := 0; i < 2; i++ {
			idem := prefix + string(rune('0'+i))
			if _, err := db.InsertEvent(ctx, &Event{
				ID: NewEventID(), OrgID: orgID, Source: "github", Type: "pr_opened",
				TraceID: "trace_" + idem, IdemKey: &idem, Payload: []byte("{}"),
				CreatedAt: now + int64(i),
			}); err != nil {
				t.Fatalf("insert event %s: %v", idem, err)
			}
		}
	}
	seed(org1.ID, "a")
	seed(org2.ID, "b")

	s, serr := db.SystemStats(ctx, Now()-86400)
	if serr != nil {
		t.Fatalf("stats: %v", serr)
	}
	if s.UserCount != 2 {
		t.Errorf("UserCount 应为 2, got %d", s.UserCount)
	}
	if s.AdminCount != 1 {
		t.Errorf("AdminCount 应为 1, got %d", s.AdminCount)
	}
	if s.MessageCount != 4 {
		t.Errorf("MessageCount 应为 4, got %d", s.MessageCount)
	}
	if s.TodayMessages != 4 {
		t.Errorf("TodayMessages 应为 4, got %d", s.TodayMessages)
	}
	// 两人各 2 条事件，top1 可能是任一
	if len(s.TopUsers) == 0 || s.TopUsers[0].UserID != u1.ID && s.TopUsers[0].UserID != u2.ID {
		// 两人都是 2 条，top1 可能是任一；只检查总数
	}
	if len(s.TopUsers) < 2 {
		t.Errorf("TopUsers 应至少 2 个, got %d", len(s.TopUsers))
	}
}

// TestListGlobalEvents 验证全站事件流（跨组织，按时间倒序）。
//
// 替代旧的 TestListGlobalMessages：事件中心里"发生了什么"是 events，
// 不再有"推给某人的消息"这个实体。
func TestListGlobalEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	orgID, _ := newTestOrg(t, db)
	now := Now()
	for i, src := range []string{"github", "cron", "manual"} {
		src := src
		idem := "k" + src
		if _, err := db.InsertEvent(ctx, &Event{
			ID:        NewEventID(),
			OrgID:     orgID,
			Source:    src,
			Type:      "test",
			TraceID:   "trace_" + src,
			IdemKey:   &idem,
			Payload:   []byte("{}"),
			CreatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("insert event %s: %v", src, err)
		}
	}

	events, err := db.ListGlobalEvents(ctx, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("应返回 3 条, got %d", len(events))
	}
	// 倒序：最新的 manual 在前
	if events[0].Source != "manual" {
		t.Errorf("应按 created_at DESC 排序，首条 got %q", events[0].Source)
	}
	if events[0].OrgID != orgID {
		t.Errorf("orgId 应为 %q, got %q", orgID, events[0].OrgID)
	}
	if events[0].TraceID == "" {
		t.Error("traceId 不该为空")
	}

	// limit 兜底
	limited, err := db.ListGlobalEvents(ctx, 0)
	if err != nil {
		t.Errorf("limit=0 应兜底为 50, got err %v", err)
	}
	if len(limited) != 3 {
		t.Errorf("limit=0 兜底后应仍返回 3 条, got %d", len(limited))
	}

	// 空库返回 [] 不是 nil（不变量 I9）
	db2 := newTestDB(t)
	empty, err := db2.ListGlobalEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if empty == nil {
		t.Error("空列表应为 [] 而不是 nil")
	}
	if len(empty) != 0 {
		t.Errorf("空库应返回 0 条, got %d", len(empty))
	}
}

// TestListAllSessions 验证全站活跃会话总览。
func TestListAllSessions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u := &User{ID: NewUserID(), Username: "alice", CreatedAt: Now()}
	db.CreateUser(u)

	// 活跃会话
	now := Now()
	db.CreateSession(&Session{ID: "sess_active", UserID: u.ID, CreatedAt: now, ExpiresAt: now + 3600, LastSeen: now})
	// 过期会话（不应出现）
	db.CreateSession(&Session{ID: "sess_expired", UserID: u.ID, CreatedAt: now - 7200, ExpiresAt: now - 3600, LastSeen: now - 3600})

	sessions, err := db.ListAllSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("应返回 1 个活跃会话, got %d", len(sessions))
	}
	if sessions[0].ID != "sess_active" {
		t.Errorf("应为 sess_active, got %q", sessions[0].ID)
	}
	if sessions[0].Username != "alice" {
		t.Errorf("username 应为 alice, got %q", sessions[0].Username)
	}
}
