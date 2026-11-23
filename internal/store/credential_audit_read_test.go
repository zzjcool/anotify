package store

import (
	"context"
	"errors"
	"testing"
)

func insertSecretAuditAt(t *testing.T, db *DB, nsID, name, action, actor string, at int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO namespace_secret_audit (id, namespace_id, name, action, actor_id, created_at)
		 VALUES (?,?,?,?,?,?)`,
		NewNamespaceSecretAuditID(), nsID, name, action, actor, at); err != nil {
		t.Fatalf("插凭据审计行: %v", err)
	}
}

func TestListNamespaceSecretAuditEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	got, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 0, 10)
	if err != nil {
		t.Fatalf("读空审计: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("空审计必须是 len 0 的非 nil 切片，得到 %#v", got)
	}
	if _, err := db.ListNamespaceSecretAudit(ctx, "", "", 0, 10); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("缺命名空间 = %v, want ErrNamespaceRequired", err)
	}
	if _, err := db.ListNamespaceSecretAudit(ctx, AllNamespaces, "", 0, 10); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("通配命名空间 = %v, want ErrNamespaceRequired", err)
	}
}

func TestListNamespaceSecretAuditLimitBounds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	base := Now()
	for i := 0; i < 205; i++ {
		insertSecretAuditAt(t, db, nsID, "gh-token", NamespaceSecretRotated, "usr_actor", base+int64(i))
	}
	cases := []struct{ limit, want int }{
		{0, AuditDefaultLimit},
		{1, 1},
		{50, 50},
		{200, 200},
		{201, AuditMaxLimit},
		{100000, AuditMaxLimit},
		{-5, AuditDefaultLimit},
	}
	for _, c := range cases {
		got, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 0, c.limit)
		if err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		if len(got) != c.want {
			t.Fatalf("limit %d → %d 条, want %d", c.limit, len(got), c.want)
		}
	}

	all, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 0, AuditMaxLimit)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	seen := map[string]bool{}
	for i, e := range all {
		if seen[e.ID] {
			t.Fatalf("审计行重复：%s", e.ID)
		}
		seen[e.ID] = true
		if i > 0 && e.CreatedAt > all[i-1].CreatedAt {
			t.Fatalf("未按时间倒序：%d > %d", e.CreatedAt, all[i-1].CreatedAt)
		}
		if e.Name != "gh-token" || e.ActorID != "usr_actor" || e.Action != NamespaceSecretRotated {
			t.Fatalf("审计行字段不对: %+v", e)
		}
	}
}

func TestListNamespaceSecretAuditBeforeCursor(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	const (
		oldest = int64(1_760_000_000)
		middle = oldest + 10
		newest = oldest + 20
	)
	insertSecretAuditAt(t, db, nsID, "gh-token", NamespaceSecretCreated, "usr_actor", oldest)
	insertSecretAuditAt(t, db, nsID, "gh-token", NamespaceSecretRotated, "usr_actor", middle)
	insertSecretAuditAt(t, db, nsID, "gh-token", NamespaceSecretDeleted, "usr_actor", newest)

	all, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 0, 10)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(all) != 3 || all[0].Action != NamespaceSecretDeleted || all[2].Action != NamespaceSecretCreated {
		t.Fatalf("顺序不对（最新在前）: %+v", all)
	}

	page1, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", 0, 2)
	if err != nil || len(page1) != 2 || page1[1].CreatedAt != middle {
		t.Fatalf("首页 limit=2: %v %+v", err, page1)
	}
	page2, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", page1[len(page1)-1].CreatedAt, 2)
	if err != nil {
		t.Fatalf("次页: %v", err)
	}
	if len(page2) != 1 || page2[0].CreatedAt != oldest {
		t.Fatalf("游标切页不重不漏: %+v", page2)
	}
	if len(page1)+len(page2) != 3 {
		t.Fatalf("两页合计 %d 条, want 3", len(page1)+len(page2))
	}

	beforeMiddle, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", middle, 10)
	if err != nil || len(beforeMiddle) != 1 || beforeMiddle[0].CreatedAt != oldest {
		t.Fatalf("before=middle 必须严格早于: %v %+v", err, beforeMiddle)
	}
	beforeNewest, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", newest, 10)
	if err != nil || len(beforeNewest) != 2 {
		t.Fatalf("before=newest 必须切掉最新一条: %v %+v", err, beforeNewest)
	}
	beforeOldest, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", oldest, 10)
	if err != nil || len(beforeOldest) != 0 {
		t.Fatalf("before=oldest 必须为空: %v %+v", err, beforeOldest)
	}
	beforeEpoch, err := db.ListNamespaceSecretAudit(ctx, nsID, "gh-token", -1, 10)
	if err != nil || len(beforeEpoch) != 0 {
		t.Fatalf("before=-1（早于纪元）必须为空: %v %+v", err, beforeEpoch)
	}
	if beforeOldest == nil {
		t.Fatal("空结果必须是 [] 不是 nil")
	}
}

func TestListNamespaceSecretAuditNamespaceAndNameScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, err := db.CreateNamespace(ctx, "other-ns", "另一个命名空间")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}

	insertSecretAuditAt(t, db, nsA, "shared", NamespaceSecretCreated, "usr_actor", 1_760_000_000)
	insertSecretAuditAt(t, db, nsA, "other", NamespaceSecretCreated, "usr_actor", 1_760_000_100)
	insertSecretAuditAt(t, db, nsB.ID, "shared", NamespaceSecretCreated, "usr_actor", 1_760_000_200)

	ownRows, err := db.ListNamespaceSecretAudit(ctx, nsA, "shared", 0, 10)
	if err != nil || len(ownRows) != 1 || ownRows[0].Name != "shared" {
		t.Fatalf("本 ns 本名必须只回自己那一行: %v %+v", err, ownRows)
	}
	otherRows, err := db.ListNamespaceSecretAudit(ctx, nsA, "other", 0, 10)
	if err != nil || len(otherRows) != 1 || otherRows[0].Name != "other" {
		t.Fatalf("同 ns 两条凭据互不串: %v %+v", err, otherRows)
	}
	crossRows, err := db.ListNamespaceSecretAudit(ctx, nsB.ID, "shared", 0, 10)
	if err != nil {
		t.Fatalf("读 nsB 同名凭据审计: %v", err)
	}
	if len(crossRows) != 1 || crossRows[0].CreatedAt != 1_760_000_200 {
		t.Fatalf("跨命名空间同名凭据必须只回本 ns 的行: %+v", crossRows)
	}
	nsAAll, err := db.ListNamespaceSecretAudit(ctx, nsA, "", 0, 10)
	if err != nil || len(nsAAll) != 2 {
		t.Fatalf("空 name = 本 ns 全部凭据: %v %+v", err, nsAAll)
	}
}
