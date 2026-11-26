package store

// capability_test_event_test.go 覆盖**人工试跑**的写入语义（capability-lifecycle §6.2 / §10）：
//
//   - 试跑是**真事件 + 真任务**：events.origin='test'、tasks 落 pending、同一 trace；
//   - 同一事务里追加 action='test' 的审计摘要（detail 只有 traceId + version，不含参数）；
//   - 能力行被删 / 身份不一致 / 没有任务 → 0 写（整个事务回滚）；
//   - origin 隔离：/stats 口径不计 test 行；事件流默认不含 test，显式 origin 才看它。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestEventOriginRoundtrip 断言 events.origin 的往返、默认值与非法值拒绝。
func TestEventOriginRoundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// 默认（未表态）→ 'source'：存量插入路径不需要改就拿到正确值。
	src := &Event{NamespaceID: nsID, TraceID: "tr-origin-1", Source: "github", Type: "push"}
	if _, err := db.InsertEvent(ctx, src, Now(), 0); err != nil {
		t.Fatalf("insert source event: %v", err)
	}
	got, err := db.GetEvent(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source event: %v", err)
	}
	if got.Origin != EventOriginSource {
		t.Fatalf("默认 origin 应为 source，得到 %q", got.Origin)
	}

	// 显式 'test' → 往返一致。
	test := &Event{NamespaceID: nsID, TraceID: "tr-origin-2", Source: EventTestSource,
		Origin: EventOriginTest, Type: "review"}
	if _, err := db.InsertEvent(ctx, test, Now(), 0); err != nil {
		t.Fatalf("insert test event: %v", err)
	}
	got, err = db.GetEvent(ctx, nsID, test.ID)
	if err != nil {
		t.Fatalf("get test event: %v", err)
	}
	if got.Origin != EventOriginTest {
		t.Fatalf("origin 往返不一致：得到 %q", got.Origin)
	}

	// 非法值：Go 侧当场拒绝（不靠数据库 CHECK 才报）。
	bad := &Event{NamespaceID: nsID, TraceID: "tr-origin-3", Source: "github",
		Origin: "replay", Type: "push"}
	if _, err := db.InsertEvent(ctx, bad, Now(), 0); err == nil {
		t.Fatal("origin=replay 不在封闭集合内，必须报错")
	}

	// 非法值：数据库 CHECK 也要拦住裸 SQL（这条约束不能只写在 Go 侧）。
	if _, err := db.Exec(
		`INSERT INTO events (id, namespace_id, trace_id, source, origin, type, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		NewEventID(), nsID, "tr-origin-4", "github", "replay", "push", Now()); err == nil {
		t.Fatal("events.origin 的 CHECK 必须拒绝封闭集合外的取值")
	}
}

// TestEventOriginListFilters 断言事件流/按 trace/显式 origin 三种口径。
func TestEventOriginListFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	base := Now()

	ext := &Event{NamespaceID: nsID, TraceID: "tr-filter-ext", Source: "github", Type: "push", CreatedAt: base}
	if _, err := db.InsertEvent(ctx, ext, base, 0); err != nil {
		t.Fatalf("insert external: %v", err)
	}
	tr := &Event{NamespaceID: nsID, TraceID: "tr-filter-test", Source: EventTestSource,
		Origin: EventOriginTest, Type: "review", CreatedAt: base + 1}
	if _, err := db.InsertEvent(ctx, tr, base+1, 0); err != nil {
		t.Fatalf("insert test: %v", err)
	}

	// 1) 事件流（无 traceId / 无 source）：默认**不含**试跑。
	stream, err := db.ListEvents(ctx, nsID, "", "", 50)
	if err != nil {
		t.Fatalf("list stream: %v", err)
	}
	if len(stream) != 1 || stream[0].ID != ext.ID {
		t.Fatalf("事件流默认应只含外部投递，得到 %+v", originIDsOf(stream))
	}

	// 2) 显式 ?origin=test：只看试跑。
	onlyTest, err := db.ListEventsWithOrigin(ctx, nsID, "", "", EventOriginTest, 50)
	if err != nil {
		t.Fatalf("list origin=test: %v", err)
	}
	if len(onlyTest) != 1 || onlyTest[0].ID != tr.ID {
		t.Fatalf("origin=test 应只返回试跑行，得到 %+v", originIDsOf(onlyTest))
	}

	// 3) 显式 ?origin=source：只看外部投递。
	onlySrc, err := db.ListEventsWithOrigin(ctx, nsID, "", "", EventOriginSource, 50)
	if err != nil {
		t.Fatalf("list origin=source: %v", err)
	}
	if len(onlySrc) != 1 || onlySrc[0].ID != ext.ID {
		t.Fatalf("origin=source 应只返回外部投递行，得到 %+v", originIDsOf(onlySrc))
	}

	// 4) 按 trace 查：**保留**试跑（trace 全景必须完整，否则出现「有任务、没根事件」）。
	byTrace, err := db.ListEvents(ctx, nsID, "tr-filter-test", "", 50)
	if err != nil {
		t.Fatalf("list by trace: %v", err)
	}
	if len(byTrace) != 1 || byTrace[0].ID != tr.ID {
		t.Fatalf("按 trace 查必须含试跑行，得到 %+v", originIDsOf(byTrace))
	}

	// 5) 游标分页与列表口径逐字一致。
	page, err := db.ListEventsBeforeWithOrigin(ctx, nsID, "", "", EventOriginTest, base+10, "", 50)
	if err != nil {
		t.Fatalf("page origin=test: %v", err)
	}
	if len(page) != 1 || page[0].ID != tr.ID {
		t.Fatalf("游标分页 origin=test 与列表口径不一致，得到 %+v", originIDsOf(page))
	}

	// 6) 闭集外的 origin 是调用错误，不是「静默不过滤」。
	if _, err := db.ListEventsWithOrigin(ctx, nsID, "", "", "replay", 50); err == nil {
		t.Fatal("origin=replay 必须报错（不能静默退化成自动口径）")
	}
}

// TestStatsExcludeTestOrigin 断言 /stats 口径不计试跑（L-D6）。
func TestStatsExcludeTestOrigin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	now := time.Now()
	since := startOfLocalDayUnix(now)

	for i := 0; i < 3; i++ {
		e := &Event{NamespaceID: nsID, TraceID: "tr-stat-ext", Source: "github", Type: "push"}
		if _, err := db.InsertEvent(ctx, e, Now(), 0); err != nil {
			t.Fatalf("insert external %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		e := &Event{NamespaceID: nsID, TraceID: "tr-stat-test", Source: EventTestSource,
			Origin: EventOriginTest, Type: "review"}
		if _, err := db.InsertEvent(ctx, e, Now(), 0); err != nil {
			t.Fatalf("insert test %d: %v", i, err)
		}
	}

	stats, err := db.messageStats(ctx, nsID, since, now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 3 {
		t.Fatalf("Total 必须排除试跑：want 3, got %d", stats.Total)
	}
	if stats.Today != 3 {
		t.Fatalf("Today 必须排除试跑：want 3, got %d", stats.Today)
	}
	sum := 0
	for _, d := range stats.Daily {
		sum += d.Count
	}
	if sum != 3 {
		t.Fatalf("Daily 热力图必须排除试跑：want 3, got %d（%+v）", sum, stats.Daily)
	}

	// 管理后台的全站口径同一条规则。
	sys, err := db.SystemStats(ctx, since)
	if err != nil {
		t.Fatalf("system stats: %v", err)
	}
	if sys.MessageCount != 3 || sys.TodayMessages != 3 {
		t.Fatalf("全站统计必须排除试跑：messageCount=%d today=%d, want 3/3",
			sys.MessageCount, sys.TodayMessages)
	}
}

// TestAcceptTestEventCreatesRealTask 断言试跑建真任务 + 真事件 + 审计摘要。
func TestAcceptTestEventCreatesRealTask(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	task := testEventTask(cap)
	receipt, err := db.AcceptTestEvent(ctx, CapabilityTestInputs{
		NamespaceID: nsID, Capability: "review", Version: 1,
		Payload: json.RawMessage(`{"diff":"a b"}`),
		Tasks:   []*Task{task},
		ActorID: userID,
		Now:     1700000000,
	})
	if err != nil {
		t.Fatalf("accept test event: %v", err)
	}
	if receipt.TraceID == "" || receipt.EventID == "" {
		t.Fatalf("回执必须带 traceId/eventId：%+v", receipt)
	}
	if receipt.Origin != EventOriginTest {
		t.Fatalf("回执 origin 必须是 test，得到 %q", receipt.Origin)
	}
	if len(receipt.Tasks) != 1 || receipt.Tasks[0].TaskID != task.ID {
		t.Fatalf("回执 tasks 必须带真任务 id：%+v", receipt.Tasks)
	}
	if receipt.Pruned == nil {
		t.Fatal("回执 pruned 必须是 []，不是 nil")
	}

	// 事件行：origin=test、source=固定文本、payload 原样。
	ev, err := db.GetEvent(ctx, nsID, receipt.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Origin != EventOriginTest || ev.Source != EventTestSource {
		t.Fatalf("试跑事件行不对：origin=%q source=%q", ev.Origin, ev.Source)
	}
	if ev.TraceID != receipt.TraceID || ev.Type != "review" {
		t.Fatalf("试跑事件身份不对：%+v", ev)
	}
	if string(ev.Payload) != `{"diff":"a b"}` {
		t.Fatalf("事件 payload 必须原样保存，得到 %s", ev.Payload)
	}

	// 任务行：真任务、pending、钉住版本与 digest。
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Phase != TaskPending || got.Capability != "review" || got.Version != 1 {
		t.Fatalf("试跑任务不对：%+v", got)
	}
	if got.Digest != cap.Digest || got.CapabilityID != cap.ID {
		t.Fatalf("任务必须钉住能力的 digest/uid：%+v", got)
	}
	if got.TraceID != receipt.TraceID {
		t.Fatalf("任务 trace 必须等于事件 trace：%q != %q", got.TraceID, receipt.TraceID)
	}

	// 审计：action=test，detail 只有 traceId + version（**不含参数**）。
	entries, err := db.ListCapabilityAudit(ctx, nsID, "review", 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != CapabilityAuditTest {
		t.Fatalf("试跑应留一条 action=test 审计：%+v", entries)
	}
	var detail map[string]any
	if err := json.Unmarshal(entries[0].Detail, &detail); err != nil {
		t.Fatalf("审计 detail 不是合法 JSON：%s", entries[0].Detail)
	}
	if detail["traceId"] != receipt.TraceID || detail["version"] != float64(1) {
		t.Fatalf("审计 detail 必须只有 traceId + version：%v", detail)
	}
	if strings.Contains(string(entries[0].Detail), "diff") {
		t.Fatalf("审计 detail 不许存参数全文，得到 %s", entries[0].Detail)
	}
}

// TestAcceptTestEventRejections 断言三种拒绝路径都是 0 写。
func TestAcceptTestEventRejections(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	// ① 没有任务：试跑必须建真任务，不建任务的调用是 bug。
	if _, err := db.AcceptTestEvent(ctx, CapabilityTestInputs{
		NamespaceID: nsID, Capability: "review", Version: 1, Now: Now(),
	}); err == nil {
		t.Fatal("没有任务的试跑必须报错")
	}

	// ② 任务身份与入参不一致（回执说 A、跑 B）。
	wrong := testEventTask(cap)
	wrong.Capability = "clean"
	if _, err := db.AcceptTestEvent(ctx, CapabilityTestInputs{
		NamespaceID: nsID, Capability: "review", Version: 1,
		Tasks: []*Task{wrong}, Now: Now(),
	}); err == nil {
		t.Fatal("任务身份不一致必须报错")
	}

	// ③ 不存在的能力 uid。
	ghost := testEventTask(cap)
	ghost.CapabilityID = "cap_ghost"
	if _, err := db.AcceptTestEvent(ctx, CapabilityTestInputs{
		NamespaceID: nsID, Capability: "review", Version: 1,
		Tasks: []*Task{ghost}, Now: Now(),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("能力行不存在应报 ErrNotFound，得到 %v", err)
	}

	// 三条拒绝都必须是 0 写：没有事件、没有任务、没有审计。
	var events, tasks, audits int
	for _, probe := range []struct {
		dst any
		sql string
	}{
		{&events, `SELECT COUNT(*) FROM events WHERE namespace_id = ?`},
		{&tasks, `SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`},
		{&audits, `SELECT COUNT(*) FROM capability_audit WHERE namespace_id = ?`},
	} {
		if err := db.QueryRow(probe.sql, nsID).Scan(probe.dst); err != nil {
			t.Fatalf("count: %v", err)
		}
	}
	if events != 0 || tasks != 0 || audits != 0 {
		t.Fatalf("被拒的试跑必须 0 写：events=%d tasks=%d audit=%d", events, tasks, audits)
	}
}

// testEventTask 造一条试跑任务行（身份钉在能力行上）。
func testEventTask(cap *CapabilityVersion) *Task {
	return &Task{
		ID:           NewTaskID(),
		NamespaceID:  cap.NamespaceID,
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Digest:       cap.Digest,
		Params:       json.RawMessage(`{"diff":"a b"}`),
		Phase:        TaskPending,
	}
}

// idsOf 给断言失败信息用（只打印 id，避免把整份 payload 倒进日志）。
func originIDsOf(events []*Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID+"("+e.Origin+")")
	}
	return out
}
