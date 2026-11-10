package store

// source_event_test.go 是 S1-D2 的**原子接收**白盒验收：
//
//   - 一次事务写 event + delivery window + 正常任务 + 失败 Task/错误事件 + 剪枝 + receipt；
//   - 任一点真实故障（trigger/约束/取消）→ 全部回滚，window 也不占；
//   - 重复 delivery 回第一次的**同一份 receipt 字节**（不重算、不新建任务）；
//   - 无 deliveryId 也原子；窗口过期后可再接收；源身份/namespace pin 失配 0 写。
//
// 纪律：故障注入只用真实 SQLite trigger/constraint（不引入生产测试开关），
// 断言同时看「行不存在」与「重试真的能成功」——只 COUNT=0 不能证明 window 没被占。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// ingressEnv 是原子接收用例的最小环境。
type ingressEnv struct {
	db     *DB
	nsID   string
	source *Source
	cap    *CapabilityVersion
}

// newIngressEnv 建 ns + 源 + 一条已解析能力。源用 manual kind（本用例不触发调度）。
func newIngressEnv(t *testing.T) *ingressEnv {
	t.Helper()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	src := &Source{NamespaceID: nsID, Name: "entry", Kind: SourceKindManual, Enabled: true}
	if err := db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return &ingressEnv{db: db, nsID: nsID, source: src, cap: v4Cap(t, db, nsID, "ingest-cap", 1)}
}

// normalTask 造一条计划里的正常任务（trace/ID 留给接收事务补）。
func (e *ingressEnv) normalTask() *Task {
	return &Task{
		NamespaceID:  e.nsID,
		CapabilityID: e.cap.ID,
		Capability:   e.cap.Name,
		Version:      e.cap.Version,
		Digest:       e.cap.Digest,
		Params:       json.RawMessage(`{"n":1}`),
	}
}

// inputs 造一份「四类事实齐活」的接收输入（正常任务 + 未解析失败 + 剪枝 + receipt）。
func (e *ingressEnv) inputs(deliveryID, traceID, typ string, now int64) SourceEventInputs {
	return SourceEventInputs{
		NamespaceID:                   e.nsID,
		SourceID:                      e.source.ID,
		ExpectedSourceResourceVersion: e.source.ResourceVersion,
		ExpectedNamespaceVarsRevision: 0,
		TraceID:                       traceID,
		Type:                          typ,
		Payload:                       json.RawMessage(`{"hello":"world"}`),
		DeliveryID:                    deliveryID,
		WindowSec:                     3600,
		Now:                           now,
		Tasks:                         []*Task{e.normalTask()},
		Failures: []DispatchFailureInputs{
			{Capability: "ghost@2", Version: 2, Detail: "下游 ghost@2 解析不到"},
		},
		Prunes: []PruneInputs{
			{Capability: e.cap.Name, Reason: protocol.ReasonParamMissing, Detail: "需要 needs"},
		},
	}
}

// rowCounts 汇总一次接收应该产生的全部事实（用于 0 写断言）。
type rowCounts struct{ events, tasks, deliveries int }

func countIngressRows(t *testing.T, db *DB, nsID, traceID, sourceID, deliveryID string) rowCounts {
	t.Helper()
	var c rowCounts
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE namespace_id = ? AND trace_id = ?`, nsID, traceID).Scan(&c.events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE namespace_id = ? AND trace_id = ?`, nsID, traceID).Scan(&c.tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if deliveryID != "" {
		if err := db.QueryRow(`SELECT COUNT(*) FROM event_deliveries WHERE namespace_id = ? AND source = ? AND delivery_id = ?`,
			nsID, sourceID, deliveryID).Scan(&c.deliveries); err != nil {
			t.Fatalf("count deliveries: %v", err)
		}
	}
	return c
}

// TestAcceptSourceEventAtomicIngress 正常路径：一个事务写全四类事实 + receipt 往返。
func TestAcceptSourceEventAtomicIngress(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	in := e.inputs("deliver-1", "trace-in", "entry", 1000)

	raw, first, err := e.db.AcceptSourceEvent(ctx, in)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !first {
		t.Fatal("首次接收必须 first=true")
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if receipt.EventID == "" || receipt.TraceID != "trace-in" {
		t.Fatalf("receipt event/trace = %+v", receipt)
	}
	if len(receipt.Tasks) != 1 || receipt.Tasks[0].TaskID == "" || receipt.Tasks[0].Capability != e.cap.Name {
		t.Fatalf("receipt tasks = %+v", receipt.Tasks)
	}
	if len(receipt.FailedTasks) != 1 || receipt.FailedTasks[0].Version != 2 ||
		receipt.FailedTasks[0].Reason != protocol.ReasonDownstreamMissing || receipt.FailedTasks[0].TaskID == "" {
		t.Fatalf("receipt failedTasks = %+v", receipt.FailedTasks)
	}
	if len(receipt.Pruned) != 1 || receipt.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("receipt pruned = %+v", receipt.Pruned)
	}

	// 事件行：来源是源名、payload 原样、receipt 是内部持久事实（逐字节等于返回值）。
	ev, err := e.db.GetEvent(ctx, e.nsID, receipt.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Source != e.source.Name || ev.Type != "entry" || ev.Reason != EventReasonIngested {
		t.Fatalf("event row = %+v", ev)
	}
	if string(ev.Payload) != `{"hello":"world"}` {
		t.Fatalf("event payload = %s", ev.Payload)
	}
	var stored string
	if err := e.db.QueryRow(`SELECT receipt FROM events WHERE id = ?`, receipt.EventID).Scan(&stored); err != nil {
		t.Fatalf("read receipt column: %v", err)
	}
	if stored != string(raw) {
		t.Fatalf("receipt 列不是返回的同一份字节:\n stored %s\n return %s", stored, raw)
	}

	// 正常任务：pending，真实 version/digest pin。
	task, err := e.db.GetTask(ctx, e.nsID, receipt.Tasks[0].TaskID)
	if err != nil {
		t.Fatalf("get normal task: %v", err)
	}
	if task.Phase != TaskPending || task.Attempt != 0 || task.CapabilityID != e.cap.ID ||
		task.Digest != e.cap.Digest || task.Version != e.cap.Version {
		t.Fatalf("normal task = %+v", task)
	}

	// 失败任务：NULL ref、failed/attempt=0、无 Node/lease/过程时间/结果事实。
	failed, err := e.db.GetTask(ctx, e.nsID, receipt.FailedTasks[0].TaskID)
	if err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	if failed.CapabilityID != "" || failed.Phase != TaskFailed || failed.Attempt != 0 {
		t.Fatalf("failed task identity = %+v", failed)
	}
	if failed.Version != 2 || failed.Digest != "" || failed.Error != protocol.ReasonDownstreamMissing {
		t.Fatalf("failed task facts = %+v", failed)
	}
	if failed.NodeID != nil || failed.LeaseID != "" || failed.LeaseUntil != nil || failed.StartedAt != nil ||
		failed.ExitCode != nil || failed.ResultAcceptedAt != nil || len(failed.ResultJSON) != 0 ||
		failed.Reported || failed.AssignmentID != "" || failed.AssignmentExpiresAt != nil {
		t.Fatalf("失败任务不能带 Node/租约/候选/结果事实: %+v", failed)
	}
	if failed.FinishedAt == nil {
		t.Fatal("失败任务必须有真实 server 完成时刻")
	}

	// 失败 Task 与错误事件同事务：trace 里能看到 downstream_missing 观察行。
	traceEvents, err := e.db.ListEvents(ctx, e.nsID, "trace-in", "", 50)
	if err != nil {
		t.Fatalf("list trace events: %v", err)
	}
	var sawFailure, sawPrune bool
	for _, ev := range traceEvents {
		switch ev.Type {
		case protocol.ReasonDownstreamMissing:
			sawFailure = true
		case protocol.ReasonParamMissing:
			sawPrune = true
		}
	}
	if !sawFailure || !sawPrune {
		t.Fatalf("trace 里缺观察事件: failure=%v prune=%v (%d 行)", sawFailure, sawPrune, len(traceEvents))
	}
}

// TestAcceptSourceEventDuplicateReplaysStoredReceipt 窗口内重复：同一份字节、不新建任何事实。
func TestAcceptSourceEventDuplicateReplaysStoredReceipt(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	firstRaw, first, err := e.db.AcceptSourceEvent(ctx, e.inputs("deliver-dup", "trace-dup", "entry", 1000))
	if err != nil || !first {
		t.Fatalf("first accept: %v %v", first, err)
	}
	raw, dup, err := e.db.AcceptSourceEvent(ctx, e.inputs("deliver-dup", "trace-dup", "entry", 1000))
	if err != nil {
		t.Fatalf("duplicate accept: %v", err)
	}
	if dup {
		t.Fatal("窗口内重复必须 first=false")
	}
	if string(raw) != string(firstRaw) {
		t.Fatalf("重复投递必须回第一次的完整 receipt:\n got %s\n want %s", raw, firstRaw)
	}
	if c := countIngressRows(t, e.db, e.nsID, "trace-dup", e.source.ID, "deliver-dup"); c.events != 3 || c.tasks != 2 || c.deliveries != 1 {
		t.Fatalf("重复投递产生了新事实: %+v (want events=3 tasks=2 deliveries=1)", c)
	}
}

// TestAcceptSourceEventConcurrentDuplicate 并发同 delivery：恰好一个 first=true，receipt 相同。
func TestAcceptSourceEventConcurrentDuplicate(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	type result struct {
		raw   json.RawMessage
		first bool
		err   error
	}
	const n = 2
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个 goroutine 用自己的任务切片：入口会回填 ID/trace，共享会数据竞争。
			in := e.inputs("deliver-conc", "trace-conc", "entry", 1000)
			raw, first, err := e.db.AcceptSourceEvent(ctx, in)
			results[i] = result{raw: raw, first: first, err: err}
		}(i)
	}
	wg.Wait()
	var firstRaw json.RawMessage
	firstCount := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent accept %d: %v", i, r.err)
		}
		if r.first {
			firstCount++
			firstRaw = r.raw
		}
	}
	if firstCount != 1 {
		t.Fatalf("并发重复投递应恰好一个 first=true，得到 %d", firstCount)
	}
	for i, r := range results {
		if string(r.raw) != string(firstRaw) {
			t.Fatalf("并发重复回执不一致（%d）: %s != %s", i, r.raw, firstRaw)
		}
	}
	if c := countIngressRows(t, e.db, e.nsID, "trace-conc", e.source.ID, "deliver-conc"); c.events != 3 || c.tasks != 2 || c.deliveries != 1 {
		t.Fatalf("并发重复产生了多余事实: %+v", c)
	}
}

// TestAcceptSourceEventWithoutDeliveryIsAtomic 无 deliveryId：每次新事件，仍原子。
func TestAcceptSourceEventWithoutDeliveryIsAtomic(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		in := e.inputs("", "trace-nodelivery", "entry", int64(1000+i))
		if _, first, err := e.db.AcceptSourceEvent(ctx, in); err != nil || !first {
			t.Fatalf("accept %d: first=%v err=%v", i, first, err)
		}
	}
	if c := countIngressRows(t, e.db, e.nsID, "trace-nodelivery", e.source.ID, ""); c.events != 6 {
		t.Fatalf("无 deliveryId 的两次接收应各写 3 行事件，得到 %d", c.events)
	}
}

// TestAcceptSourceEventWindowExpiry 窗口过期后同 id 当新事件接收（新 eventId、新事实）。
func TestAcceptSourceEventWindowExpiry(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	in := e.inputs("deliver-exp", "trace-exp", "entry", 1000)
	firstRaw, first, err := e.db.AcceptSourceEvent(ctx, in)
	if err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	var firstReceipt SourceEventReceipt
	if err := json.Unmarshal(firstRaw, &firstReceipt); err != nil {
		t.Fatal(err)
	}
	in2 := e.inputs("deliver-exp", "trace-exp-2", "entry", 1000+3601)
	secondRaw, second, err := e.db.AcceptSourceEvent(ctx, in2)
	if err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if !second {
		t.Fatal("窗口过期后同 id 必须按新事件接收")
	}
	var secondReceipt SourceEventReceipt
	if err := json.Unmarshal(secondRaw, &secondReceipt); err != nil {
		t.Fatal(err)
	}
	if secondReceipt.EventID == firstReceipt.EventID {
		t.Fatal("窗口过期后必须得到新 eventId")
	}
}

// TestAcceptSourceEventIdentityMismatchZeroWrites 源 CAS/跨 ns/ABA/namespace revision 失配 → 0 写、不占窗。
func TestAcceptSourceEventIdentityMismatchZeroWrites(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()

	// 源 resource_version 变旧（ABA/CAS）。
	if err := e.db.UpdateSource(ctx, &Source{
		ID: e.source.ID, NamespaceID: e.nsID, DisplayName: "changed",
		ResourceVersion: e.source.ResourceVersion,
	}); err != nil {
		t.Fatalf("update source: %v", err)
	}
	stale := e.inputs("deliver-stale", "trace-stale", "entry", 1000)
	if _, _, err := e.db.AcceptSourceEvent(ctx, stale); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧源版本应 ErrResourceVersionConflict，得到 %v", err)
	}
	if c := countIngressRows(t, e.db, e.nsID, "trace-stale", e.source.ID, "deliver-stale"); c.events != 0 || c.tasks != 0 || c.deliveries != 0 {
		t.Fatalf("源 CAS 失败留下了写: %+v", c)
	}

	// 源被删后同名重建（ABA）：旧 SourceID 不存在 → 0 写。
	cur, err := e.db.GetSourceByName(ctx, e.nsID, e.source.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.DeleteSource(ctx, e.nsID, cur.ID, cur.ResourceVersion); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	recreated := &Source{NamespaceID: e.nsID, Name: e.source.Name, Kind: SourceKindManual, Enabled: true}
	if cerr := e.db.CreateSource(ctx, recreated); cerr != nil {
		t.Fatalf("recreate source: %v", cerr)
	}
	oldIDInputs := SourceEventInputs{
		NamespaceID: e.nsID, SourceID: e.source.ID,
		ExpectedSourceResourceVersion: 1, ExpectedNamespaceVarsRevision: 0,
		Type: "entry", Now: 1000,
	}
	if _, _, err := e.db.AcceptSourceEvent(ctx, oldIDInputs); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("ABA 旧 SourceID 应 ErrSourceNotFound，得到 %v", err)
	}

	// namespace 输入 revision 失配。
	if _, err := e.db.PutNamespaceConfig(ctx, e.nsID, map[string]string{"K": "v"}, 1); err != nil {
		t.Fatalf("put namespace config: %v", err)
	}
	fresh := &SourceEventInputs{
		NamespaceID: e.nsID, SourceID: recreated.ID,
		ExpectedSourceResourceVersion: recreated.ResourceVersion, ExpectedNamespaceVarsRevision: 0,
		TraceID: "trace-rev", Type: "entry", Now: 2000,
	}
	if _, _, err := e.db.AcceptSourceEvent(ctx, *fresh); !errors.Is(err, ErrNamespaceInputsChanged) {
		t.Fatalf("namespace revision 失配应 ErrNamespaceInputsChanged，得到 %v", err)
	}
	if c := countIngressRows(t, e.db, e.nsID, "trace-rev", recreated.ID, ""); c.events != 0 || c.tasks != 0 {
		t.Fatalf("namespace pin 失败留下了写: %+v", c)
	}
}

// TestAcceptSourceEventFaultInjectionRollsBackEverything 五个真实故障点 → 全 0 写、window 未占、重试成功。
func TestAcceptSourceEventFaultInjectionRollsBackEverything(t *testing.T) {
	cases := []struct {
		name    string
		trigger string
	}{
		{"event_row", `CREATE TRIGGER s1_fail_event BEFORE INSERT ON events
			WHEN NEW.type = 'boom-event' BEGIN SELECT RAISE(ABORT, 'injected event failure'); END`},
		{"receipt_column", `CREATE TRIGGER s1_fail_receipt BEFORE INSERT ON events
			WHEN NEW.receipt <> '{}' BEGIN SELECT RAISE(ABORT, 'injected receipt failure'); END`},
		{"delivery_window", `CREATE TRIGGER s1_fail_window BEFORE INSERT ON event_deliveries
			BEGIN SELECT RAISE(ABORT, 'injected window failure'); END`},
		{"normal_task", `CREATE TRIGGER s1_fail_task BEFORE INSERT ON tasks
			WHEN NEW.capability_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected normal task failure'); END`},
		{"failure_task", `CREATE TRIGGER s1_fail_failure BEFORE INSERT ON tasks
			WHEN NEW.capability_id IS NULL BEGIN SELECT RAISE(ABORT, 'injected failure task failure'); END`},
		{"prune_event", `CREATE TRIGGER s1_fail_prune BEFORE INSERT ON events
			WHEN NEW.source = 'route' AND NEW.type = 'param_missing' BEGIN SELECT RAISE(ABORT, 'injected prune failure'); END`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIngressEnv(t)
			ctx := context.Background()
			mustExec(t, e.db, tc.trigger)
			in := e.inputs("deliver-fail", "trace-fail", "boom-event", 1000)
			if _, _, err := e.db.AcceptSourceEvent(ctx, in); err == nil {
				t.Fatal("注入的故障没有让接收失败")
			}
			if c := countIngressRows(t, e.db, e.nsID, "trace-fail", e.source.ID, "deliver-fail"); c.events != 0 || c.tasks != 0 || c.deliveries != 0 {
				t.Fatalf("故障点 %s 留下了半成品: %+v", tc.name, c)
			}
			// 故障移除后同一 delivery 必须能真正接收（window 没有被失败事务占掉）。
			if _, err := e.db.Exec(`DROP TRIGGER ` + triggerName(tc.name)); err != nil {
				t.Fatalf("drop trigger: %v", err)
			}
			if _, first, err := e.db.AcceptSourceEvent(ctx, e.inputs("deliver-fail", "trace-fail", "boom-event", 1000)); err != nil || !first {
				t.Fatalf("故障移除后重试: first=%v err=%v", first, err)
			}
		})
	}
}

// triggerName 把子用例名映射回 trigger 名（DROP TRIGGER 需要精确名字）。
func triggerName(caseName string) string {
	switch caseName {
	case "event_row":
		return "s1_fail_event"
	case "receipt_column":
		return "s1_fail_receipt"
	case "delivery_window":
		return "s1_fail_window"
	case "normal_task":
		return "s1_fail_task"
	case "failure_task":
		return "s1_fail_failure"
	default:
		return "s1_fail_prune"
	}
}

// TestAcceptSourceEventZeroFanoutIsLegal 0 fanout（如 no_target）：事件照常接收，receipt 空列表是 []。
func TestAcceptSourceEventZeroFanoutIsLegal(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	in := SourceEventInputs{
		NamespaceID:                   e.nsID,
		SourceID:                      e.source.ID,
		ExpectedSourceResourceVersion: e.source.ResourceVersion,
		TraceID:                       "trace-empty",
		Type:                          "entry",
		Now:                           1000,
		Prunes:                        []PruneInputs{{Reason: protocol.ReasonNoTarget, Detail: "源没有 targets"}},
	}
	raw, first, err := e.db.AcceptSourceEvent(ctx, in)
	if err != nil || !first {
		t.Fatalf("accept 0-fanout: %v %v", first, err)
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Tasks == nil || receipt.FailedTasks == nil || receipt.Pruned == nil {
		t.Fatalf("空列表必须是 [] 而不是 null: %s", raw)
	}
	if len(receipt.Tasks) != 0 || len(receipt.FailedTasks) != 0 || len(receipt.Pruned) != 1 {
		t.Fatalf("0 fanout receipt = %+v", receipt)
	}
}

// TestSourceEventReceiptMissingOnLegacyWindow 旧路径留下的窗口记录没有 receipt → 明确报错，不造回执。
func TestSourceEventReceiptMissingOnLegacyWindow(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	// 旧入口（InsertEvent）占窗：它的 events.receipt 是默认 '{}'。
	legacy := &Event{NamespaceID: e.nsID, TraceID: "trace-legacy", Source: e.source.Name, Type: "entry"}
	delivery := "deliver-legacy"
	legacy.DeliveryID = &delivery
	if inserted, err := e.db.InsertEvent(ctx, legacy, 1000, 3600); err != nil || !inserted {
		t.Fatalf("legacy insert: %v %v", inserted, err)
	}
	in := SourceEventInputs{
		NamespaceID: e.nsID, SourceID: e.source.ID,
		ExpectedSourceResourceVersion: e.source.ResourceVersion,
		TraceID:                       "trace-legacy", Type: "entry", DeliveryID: delivery, WindowSec: 3600, Now: 1001,
	}
	if _, _, err := e.db.AcceptSourceEvent(ctx, in); !errors.Is(err, ErrSourceEventReceiptMissing) {
		t.Fatalf("旧窗口记录应 ErrSourceEventReceiptMissing，得到 %v", err)
	}
}

// TestSourceEventStrictLayoutAndReopen 新列 strict layout / 重开幂等 / 旧布局明确拒绝且不删数据。
func TestSourceEventStrictLayoutAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1-layout.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	nsID, _ := newTestNS(t, db)
	src := &Source{NamespaceID: nsID, Name: "entry", Kind: SourceKindManual, Enabled: true}
	if err := db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	raw, _, err := db.AcceptSourceEvent(context.Background(), SourceEventInputs{
		NamespaceID: nsID, SourceID: src.ID, ExpectedSourceResourceVersion: src.ResourceVersion,
		TraceID: "trace-layout", Type: "entry", DeliveryID: "deliver-layout", WindowSec: 60, Now: 1000,
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 重开：严格 layout 通过，receipt 作为内部持久事实仍在。
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var eventID string
	if err := db2.QueryRow(`SELECT id FROM events WHERE trace_id = 'trace-layout'`).Scan(&eventID); err != nil {
		t.Fatalf("read event: %v", err)
	}
	stored, err := readEventReceiptOn(context.Background(), db2, nsID, eventID)
	if err != nil || string(stored) != string(raw) {
		t.Fatalf("reopen receipt = %s (%v), want %s", stored, err, raw)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}

	// 旧布局：删掉 receipt 列（marker=4 仍在）→ 明确拒绝，且数据不动。
	rawDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	rawDB.SetMaxOpenConns(1)
	if _, err := rawDB.Exec(`ALTER TABLE events DROP COLUMN receipt`); err != nil {
		t.Fatalf("drop receipt: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	if bad, err := Open(path); err == nil {
		_ = bad.Close()
		t.Fatal("缺 receipt 列的旧布局被接受了")
	} else if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("应 ErrSchemaInvalid，得到 %v", err)
	}
	rawDB, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var events int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM events WHERE trace_id = 'trace-layout'`).Scan(&events); err != nil {
		t.Fatalf("拒绝路径删了数据: %v", err)
	}
	if events != 1 {
		t.Fatalf("拒绝路径改动了事件数据: %d", events)
	}
}
