package trigger

import (
	"context"
	"encoding/json"
	"strconv"
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

func count(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("计数 %q: %v", query, err)
	}
	return n
}

func setNextFire(t *testing.T, f *fixture, at int64) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE sources SET next_fire_at = ? WHERE id = ?`, at, f.src.ID); err != nil {
		t.Fatalf("改 next_fire_at: %v", err)
	}
}

func TestCronDowntimeCoalescesToOneFireAndRealigns(t *testing.T) {
	f := newFixture(t, "* * * * *")
	now := time.Now().Truncate(time.Minute).Unix()
	setNextFire(t, f, now-3600)

	results, err := f.sched.RunDue(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("应触发一条源: %+v", results)
	}
	if results[0].ScheduledAt != now-3600 {
		t.Fatalf("补发应归到最早的错过计划时刻 %d, got %d", now-3600, results[0].ScheduledAt)
	}
	if results[0].Missed != 61 {
		t.Fatalf("停机 60 分钟错过 61 格, got missed=%d", results[0].Missed)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM events WHERE namespace_id = ? AND source = ?`,
		f.ns.ID, "cron-1"); n != 1 {
		t.Fatalf("补发必须合并成 1 条事件, got %d", n)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, f.ns.ID); n != 1 {
		t.Fatalf("补发必须只建 1 个任务, got %d", n)
	}
	stored, err := f.db.GetSourceByName(context.Background(), f.ns.ID, "cron-1")
	if err != nil {
		t.Fatalf("回读源: %v", err)
	}
	if stored.NextFireAt == nil || *stored.NextFireAt != now+60 {
		t.Fatalf("next_fire_at 应对齐到 now 之后的下一个整分钟 %d, got %v", now+60, stored.NextFireAt)
	}
	if again, err := f.sched.RunDue(context.Background(), now, 10); err != nil || len(again) != 0 {
		t.Fatalf("对齐后同一时刻不该再补发: %+v err=%v", again, err)
	}
}

func TestCronDeliveryIDDedupesRepeatedSlot(t *testing.T) {
	f := newFixture(t, "* * * * *")
	slot := *f.src.NextFireAt
	now := slot + 1

	first, err := f.sched.RunDue(context.Background(), now, 10)
	if err != nil || len(first) != 1 || first[0].Err != nil {
		t.Fatalf("首轮触发: %+v err=%v", first, err)
	}
	if first[0].Receipt == nil || first[0].Receipt.Duplicate {
		t.Fatalf("首轮不该是重复投递: %+v", first[0].Receipt)
	}
	// 模拟 MarkSourceFired 失败：库里 next_fire_at 还停在同一个计划时刻，下一轮仍是同一格。
	setNextFire(t, f, slot)

	second, err := f.sched.RunDue(context.Background(), now+30, 10)
	if err != nil || len(second) != 1 || second[0].Err != nil {
		t.Fatalf("第二轮触发: %+v err=%v", second, err)
	}
	if second[0].ScheduledAt != slot {
		t.Fatalf("第二轮应归到同一计划时刻 %d, got %d", slot, second[0].ScheduledAt)
	}
	if second[0].Receipt == nil || !second[0].Receipt.Duplicate {
		t.Fatalf("同一计划时刻的第二次触发必须命中投递幂等键（duplicate）: %+v", second[0].Receipt)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM events WHERE namespace_id = ? AND source = ?`,
		f.ns.ID, "cron-1"); n != 1 {
		t.Fatalf("同一计划时刻只该有 1 条事件, got %d", n)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, f.ns.ID); n != 1 {
		t.Fatalf("重复投递不该再建任务, got %d", n)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM event_deliveries WHERE delivery_id = ?`,
		"cron:"+f.src.ID+":"+strconv.FormatInt(slot, 10)); n != 1 {
		t.Fatalf("cron 必须写投递幂等键（1 行）, got %d", n)
	}
}
