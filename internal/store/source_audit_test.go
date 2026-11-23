package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestMarkSourcePolledKeepNextFire 「立即拉取一次」只跳过排期那一列：游标/失败事实照写，
// next_fire_at 逐字保留（成功与失败都一样）。
func TestMarkSourcePolledKeepNextFire(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.keep")

	src := pollSrc(nsID, "poll-keep", `{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"}}`, "demo.keep")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建 poll 源: %v", err)
	}
	before, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil || before.NextFireAt == nil {
		t.Fatalf("取源: %v %+v", err, before)
	}

	if err := db.MarkSourcePolled(ctx, src.ID, PollOutcome{
		Success: true, Cursor: `"e1"`, PolledAt: 1000, NextFire: 999999, KeepNextFire: true,
	}); err != nil {
		t.Fatalf("手动成功写回: %v", err)
	}
	got, _ := db.GetSource(ctx, nsID, src.ID)
	if got.PollCursor != `"e1"` || got.LastPollAt == nil || *got.LastPollAt != 1000 {
		t.Fatalf("手动轮也必须推进游标与 lastPollAt: %+v", got)
	}
	if got.NextFireAt == nil || *got.NextFireAt != *before.NextFireAt {
		t.Fatalf("手动成功轮改了排期: %v → %v", before.NextFireAt, got.NextFireAt)
	}

	if err := db.MarkSourcePolled(ctx, src.ID, PollOutcome{
		PolledAt: 2000, NextFire: 120, KeepNextFire: true, Failures: 1, Error: "HTTP 500",
	}); err != nil {
		t.Fatalf("手动失败写回: %v", err)
	}
	got, _ = db.GetSource(ctx, nsID, src.ID)
	if got.PollCursor != `"e1"` || got.ConsecutiveFailures != 1 || got.LastError != "HTTP 500" {
		t.Fatalf("手动失败轮的事实不对: %+v", got)
	}
	if got.NextFireAt == nil || *got.NextFireAt != *before.NextFireAt {
		t.Fatalf("手动失败轮把排期推进了: %v → %v", before.NextFireAt, got.NextFireAt)
	}
}

// TestAppendSourceAudit 审计行只增：动作在封闭集合内、detail 必须是 JSON，actor 原样留存。
func TestAppendSourceAudit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.audit")

	src := pollSrc(nsID, "poll-audit", `{"url":"https://example.com/feed","schedule":{"expr":"*/5 * * * *"}}`, "demo.audit")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建 poll 源: %v", err)
	}

	detail := json.RawMessage(`{"items":2,"created":2,"duplicates":0,"ok":true}`)
	entry, err := db.AppendSourceAudit(ctx, SourceAuditInput{
		NamespaceID: nsID, SourceID: src.ID, Action: SourceAuditPolledManually,
		ActorID: "usr_actor", Detail: detail, Now: 1700,
	})
	if err != nil {
		t.Fatalf("追加审计: %v", err)
	}
	var (
		action, actor, stored string
		createdAt             int64
	)
	if err := db.QueryRow(
		`SELECT action, actor_id, detail, created_at FROM source_audit WHERE id = ?`, entry.ID).
		Scan(&action, &actor, &stored, &createdAt); err != nil {
		t.Fatalf("回读审计: %v", err)
	}
	if action != SourceAuditPolledManually || actor != "usr_actor" || stored != string(detail) || createdAt != 1700 {
		t.Fatalf("审计行不一致: %q %q %q %d", action, actor, stored, createdAt)
	}

	if _, err := db.AppendSourceAudit(ctx, SourceAuditInput{
		NamespaceID: nsID, SourceID: src.ID, Action: "source.created",
	}); err == nil {
		t.Fatal("封闭集合外的动作必须被拒")
	}
	if _, err := db.AppendSourceAudit(ctx, SourceAuditInput{
		NamespaceID: nsID, SourceID: src.ID, Action: SourceAuditPolledManually, Detail: json.RawMessage(`{`),
	}); err == nil {
		t.Fatal("非法 detail 必须被拒")
	}
	if _, err := db.AppendSourceAudit(ctx, SourceAuditInput{
		SourceID: src.ID, Action: SourceAuditPolledManually,
	}); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("缺命名空间 = %v，want ErrNamespaceRequired", err)
	}
}
