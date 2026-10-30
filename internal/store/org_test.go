package store

import (
	"context"
	"errors"
	"testing"
)

// TestOrgCreateAndMember 组织 + 成员往返。
func TestOrgCreateAndMember(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	orgID := orgIDOfNamespace(t, db, nsID)

	o, err := db.GetOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if o.ID != orgID || o.Name == "" || o.CreatedAt == 0 {
		t.Fatalf("org round-trip mismatch: %+v", o)
	}

	members, err := db.ListOrgMembers(ctx, orgID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	if members[0].UserID != userID || members[0].Role != OrgRoleOwner {
		t.Fatalf("member mismatch: %+v", members[0])
	}

	ok, err := db.IsOrgMember(ctx, orgID, userID)
	if err != nil || !ok {
		t.Fatalf("IsOrgMember = %v, %v; want true, nil", ok, err)
	}
	// foreign_user 剪枝的基础：不存在的成员必须返回 false
	ok, err = db.IsOrgMember(ctx, orgID, "usr_not_a_member")
	if err != nil {
		t.Fatalf("IsOrgMember: %v", err)
	}
	if ok {
		t.Fatal("IsOrgMember returned true for non-member")
	}
}

// TestOrgRoles 角色读取与更新。
func TestOrgRoles(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	orgID := orgIDOfNamespace(t, db, nsID)

	role, ok, err := db.OrgRoleOf(ctx, orgID, userID)
	if err != nil || !ok || role != OrgRoleOwner {
		t.Fatalf("OrgRoleOf = %q, %v, %v; want owner, true, nil", role, ok, err)
	}
	// 幂等重复加入会更新角色
	if err := db.AddOrgMember(ctx, orgID, userID, OrgRoleMember); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	role, ok, err = db.OrgRoleOf(ctx, orgID, userID)
	if err != nil || !ok || role != OrgRoleMember {
		t.Fatalf("after update OrgRoleOf = %q, %v, %v; want member", role, ok, err)
	}
	// 非成员
	_, ok, err = db.OrgRoleOf(ctx, orgID, "usr_nobody")
	if err != nil {
		t.Fatalf("OrgRoleOf non-member: %v", err)
	}
	if ok {
		t.Fatal("OrgRoleOf reported membership for non-member")
	}
}

// TestPrimaryOrgOf 主组织：最早加入的那个。
func TestPrimaryOrgOf(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	firstNS, userID := newTestNS(t, db)
	firstOrg := orgIDOfNamespace(t, db, firstNS)
	second, err := db.CreateOrg(ctx, "second-"+NewUserID())
	if err != nil {
		t.Fatalf("create second org: %v", err)
	}
	if err := db.AddOrgMember(ctx, second.ID, userID, OrgRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	got, err := db.PrimaryOrgOf(ctx, userID)
	if err != nil {
		t.Fatalf("PrimaryOrgOf: %v", err)
	}
	if got != firstOrg {
		t.Fatalf("PrimaryOrgOf = %q, want first org %q", got, firstOrg)
	}
	// 无组织 → ErrNotFound（代码里不存在「没有组织」的分支，靠这里兜底报错）
	if _, err := db.PrimaryOrgOf(ctx, "usr_orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PrimaryOrgOf orphan err = %v, want ErrNotFound", err)
	}
}

// TestListOrgsByUser 用户组织列表（空列表是 [] 不是 nil，不变量 I9 的 store 侧保证）。
func TestListOrgsByUser(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	orgID := orgIDOfNamespace(t, db, nsID)
	orgs, err := db.ListOrgsByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListOrgsByUser: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != orgID {
		t.Fatalf("orgs = %+v, want [%s]", orgs, orgID)
	}

	empty, err := db.ListOrgsByUser(ctx, "usr_none")
	if err != nil {
		t.Fatalf("ListOrgsByUser empty: %v", err)
	}
	if empty == nil {
		t.Fatal("ListOrgsByUser returned nil slice for empty result (invariant I9 needs [])")
	}
	if len(empty) != 0 {
		t.Fatalf("want 0 orgs, got %d", len(empty))
	}
}

// TestGetOrgNotFound 未命中返回 ErrNotFound（不是 nil, nil）。
func TestGetOrgNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.GetOrg(context.Background(), "org_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOrg err = %v, want ErrNotFound", err)
	}
}

// TestCreateOrgRejectsEmptyName 空名字直接拒绝。
func TestCreateOrgRejectsEmptyName(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.CreateOrg(context.Background(), ""); err == nil {
		t.Fatal("CreateOrg accepted empty name")
	}
}

// TestOrgNameUnique 组织名唯一。
func TestOrgNameUnique(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	name := "dup-" + NewUserID()
	if _, err := db.CreateOrg(ctx, name); err != nil {
		t.Fatalf("first org: %v", err)
	}
	if _, err := db.CreateOrg(ctx, name); err == nil {
		t.Fatal("CreateOrg accepted duplicate name")
	}
}
