package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

func auditReadSrc(nsID, name string) *Source {
	return &Source{
		NamespaceID: nsID,
		Name:        name,
		Kind:        SourceKindPoll,
		Config:      json.RawMessage(`{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"}}`),
		Enabled:     true,
		Targets:     []sourcespec.Target{},
	}
}

func appendAuditAt(t *testing.T, db *DB, nsID, sourceID, actor string, at int64) *SourceAuditEntry {
	t.Helper()
	entry, err := db.AppendSourceAudit(context.Background(), SourceAuditInput{
		NamespaceID: nsID, SourceID: sourceID, Action: SourceAuditPolledManually,
		ActorID: actor, Detail: json.RawMessage(`{"ok":true,"items":1}`), Now: at,
	})
	if err != nil {
		t.Fatalf("追加审计: %v", err)
	}
	return entry
}

func TestListSourceAuditEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	src := auditReadSrc(nsID, "poll-empty")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}

	got, err := db.ListSourceAudit(ctx, nsID, src.ID, 0, 10)
	if err != nil {
		t.Fatalf("读空审计: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("空审计必须是 len 0 的非 nil 切片，得到 %#v", got)
	}

	if _, err := db.ListSourceAudit(ctx, "", src.ID, 0, 10); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("缺命名空间 = %v, want ErrNamespaceRequired", err)
	}
	if _, err := db.ListSourceAudit(ctx, AllNamespaces, src.ID, 0, 10); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("通配命名空间 = %v, want ErrNamespaceRequired", err)
	}
}

func TestListSourceAuditLimitBounds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	src := auditReadSrc(nsID, "poll-limits")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}

	base := Now()
	for i := 0; i < 205; i++ {
		appendAuditAt(t, db, nsID, src.ID, "usr_actor", base+int64(i))
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
		got, err := db.ListSourceAudit(ctx, nsID, src.ID, 0, c.limit)
		if err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		if len(got) != c.want {
			t.Fatalf("limit %d → %d 条, want %d", c.limit, len(got), c.want)
		}
	}

	all, err := db.ListSourceAudit(ctx, nsID, src.ID, 0, AuditMaxLimit)
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
		if string(e.Detail) != `{"ok":true,"items":1}` {
			t.Fatalf("detail 往返不一致: %s", e.Detail)
		}
	}
}

func TestListSourceAuditBeforeCursor(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	src := auditReadSrc(nsID, "poll-cursor")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}
	const (
		oldest = int64(1_760_000_000)
		middle = oldest + 10
		newest = oldest + 20
	)
	appendAuditAt(t, db, nsID, src.ID, "usr_actor", oldest)
	appendAuditAt(t, db, nsID, src.ID, "usr_actor", middle)
	appendAuditAt(t, db, nsID, src.ID, "usr_actor", newest)

	all, err := db.ListSourceAudit(ctx, nsID, src.ID, 0, 10)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(all) != 3 || all[0].CreatedAt != newest || all[2].CreatedAt != oldest {
		t.Fatalf("顺序不对（最新在前）: %+v", all)
	}

	page1, err := db.ListSourceAudit(ctx, nsID, src.ID, 0, 2)
	if err != nil || len(page1) != 2 || page1[1].CreatedAt != middle {
		t.Fatalf("首页 limit=2: %v %+v", err, page1)
	}
	page2, err := db.ListSourceAudit(ctx, nsID, src.ID, page1[len(page1)-1].CreatedAt, 2)
	if err != nil {
		t.Fatalf("次页: %v", err)
	}
	if len(page2) != 1 || page2[0].CreatedAt != oldest {
		t.Fatalf("游标切页不重不漏: %+v", page2)
	}
	if len(page1)+len(page2) != 3 {
		t.Fatalf("两页合计 %d 条, want 3", len(page1)+len(page2))
	}
	seen := map[string]bool{}
	for _, e := range append(append([]*SourceAuditEntry{}, page1...), page2...) {
		if seen[e.ID] {
			t.Fatalf("跨页重复: %s", e.ID)
		}
		seen[e.ID] = true
	}

	beforeMiddle, err := db.ListSourceAudit(ctx, nsID, src.ID, middle, 10)
	if err != nil || len(beforeMiddle) != 1 || beforeMiddle[0].CreatedAt != oldest {
		t.Fatalf("before=middle 必须严格早于: %v %+v", err, beforeMiddle)
	}
	beforeNewest, err := db.ListSourceAudit(ctx, nsID, src.ID, newest, 10)
	if err != nil || len(beforeNewest) != 2 {
		t.Fatalf("before=newest 必须切掉最新一条: %v %+v", err, beforeNewest)
	}
	beforeOldest, err := db.ListSourceAudit(ctx, nsID, src.ID, oldest, 10)
	if err != nil || len(beforeOldest) != 0 {
		t.Fatalf("before=oldest 必须为空: %v %+v", err, beforeOldest)
	}
	beforeEpoch, err := db.ListSourceAudit(ctx, nsID, src.ID, -1, 10)
	if err != nil || len(beforeEpoch) != 0 {
		t.Fatalf("before=-1（早于纪元）必须为空: %v %+v", err, beforeEpoch)
	}
	if beforeOldest == nil {
		t.Fatal("空结果必须是 [] 不是 nil")
	}
}

func TestListSourceAuditNamespaceAndSourceScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, err := db.CreateNamespace(ctx, "other-ns", "另一个命名空间")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}

	srcA := auditReadSrc(nsA, "shared")
	if err := db.CreateSource(ctx, srcA); err != nil {
		t.Fatalf("建 nsA 源: %v", err)
	}
	srcB := auditReadSrc(nsB.ID, "shared")
	if err := db.CreateSource(ctx, srcB); err != nil {
		t.Fatalf("建 nsB 同名源: %v", err)
	}
	srcA2 := auditReadSrc(nsA, "second")
	if err := db.CreateSource(ctx, srcA2); err != nil {
		t.Fatalf("建 nsA 第二个源: %v", err)
	}

	row := appendAuditAt(t, db, nsA, srcA.ID, "usr_actor", 1_760_000_000)
	appendAuditAt(t, db, nsA, srcA2.ID, "usr_actor", 1_760_000_100)

	ownRows, err := db.ListSourceAudit(ctx, nsA, srcA.ID, 0, 10)
	if err != nil || len(ownRows) != 1 || ownRows[0].ID != row.ID {
		t.Fatalf("本 ns 本源必须只回自己那一行: %v %+v", err, ownRows)
	}
	siblingRows, err := db.ListSourceAudit(ctx, nsA, srcA2.ID, 0, 10)
	if err != nil || len(siblingRows) != 1 || siblingRows[0].SourceID != srcA2.ID {
		t.Fatalf("同 ns 两个源互不串: %v %+v", err, siblingRows)
	}
	crossRows, err := db.ListSourceAudit(ctx, nsB.ID, srcB.ID, 0, 10)
	if err != nil {
		t.Fatalf("读 nsB 同名源的审计: %v", err)
	}
	if crossRows == nil || len(crossRows) != 0 {
		t.Fatalf("跨命名空间同名源不许读到别人的行: %#v", crossRows)
	}
	mismatched, err := db.ListSourceAudit(ctx, nsB.ID, srcA.ID, 0, 10)
	if err != nil {
		t.Fatalf("用别的命名空间读 nsA 的源: %v", err)
	}
	if len(mismatched) != 0 {
		t.Fatalf("(nsB, nsA 的 sourceID) 必须为空（命名空间过滤不能只靠 id）: %#v", mismatched)
	}
}
