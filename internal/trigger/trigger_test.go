package trigger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/trigger.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fixture struct {
	db    *store.DB
	ns    *store.Namespace
	src   *store.Source
	svc   *source.Service
	sched *Scheduler
}

func newFixture(t *testing.T, expr string) *fixture {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	capability := &store.CapabilityVersion{
		NamespaceID: ns.ID, Name: "analyze", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(ctx, capability); err != nil {
		t.Fatalf("建能力: %v", err)
	}
	src := &store.Source{
		NamespaceID: ns.ID, Name: "cron-1", Kind: sourcespec.KindCron,
		Config:  json.RawMessage(`{"expr":"` + expr + `","tz":"Local"}`),
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建 cron 源: %v", err)
	}
	stored, err := db.GetSourceByName(ctx, ns.ID, "cron-1")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	svc := source.New(db, route.New(db))
	return &fixture{db: db, ns: ns, src: stored, svc: svc, sched: New(db, svc, nil)}
}

func TestRunDueFiresCronAndAdvances(t *testing.T) {
	f := newFixture(t, "* * * * *")
	if f.src.NextFireAt == nil {
		t.Fatalf("cron 源必须有 next_fire_at: %+v", f.src)
	}
	now := *f.src.NextFireAt + 1
	results, err := f.sched.RunDue(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("应触发一条源: %+v", results)
	}
	if results[0].Receipt == nil || len(results[0].Receipt.Tasks) != 1 {
		t.Fatalf("cron 触发必须走同一条 Ingest 路径建任务: %+v", results[0])
	}
	stored, err := f.db.GetSourceByName(context.Background(), f.ns.ID, "cron-1")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	if stored.LastFiredAt == nil || *stored.LastFiredAt != now {
		t.Fatalf("last_fired_at = %v，want %d", stored.LastFiredAt, now)
	}
	if stored.NextFireAt == nil || *stored.NextFireAt <= now {
		t.Fatalf("next_fire_at 未推进: %v", stored.NextFireAt)
	}
	again, err := f.sched.RunDue(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("第二次 RunDue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("推进后不该再次到点: %+v", again)
	}
}

func TestRunDueNeverFireExpressionAdvancesWithoutHotLoop(t *testing.T) {
	f := newFixture(t, "* * * * *")
	// 把 config 改成永不成立（2 月 30 日）并把 next_fire_at 拉回过去。
	if _, err := f.db.Exec(`UPDATE sources SET config = ?, next_fire_at = ? WHERE id = ?`,
		`{"expr":"0 0 30 2 *","tz":"Local"}`, 100, f.src.ID); err != nil {
		t.Fatalf("改 cron 表达式: %v", err)
	}
	now := int64(1000)
	results, err := f.sched.RunDue(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("Ingest 本身应成功: %+v", results)
	}
	stored, err := f.db.GetSourceByName(context.Background(), f.ns.ID, "cron-1")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	if stored.NextFireAt == nil || *stored.NextFireAt < now+300*24*3600 {
		t.Fatalf("永不成立的表达式必须把 next_fire_at 推到远处（避免热循环），得到 %v", stored.NextFireAt)
	}
}

func TestRunDueErrorsAreNotSwallowed(t *testing.T) {
	f := newFixture(t, "* * * * *")
	// 关库制造真实错误：错误必须返回给调用方，而不是变成「0 条触发」。
	if err := f.db.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}
	if _, err := f.sched.RunDue(context.Background(), time.Now().Unix(), 10); err == nil {
		t.Fatal("列到点源失败必须返回错误（不吞）")
	}
}
