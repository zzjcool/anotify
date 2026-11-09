package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// evt 造一条投递（deliveryID 为空 = 无幂等键）。
func evt(nsID, traceID, source, deliveryID string) *store.Event {
	e := &store.Event{
		NamespaceID: nsID,
		TraceID:     traceID,
		Source:      source,
		Type:        "deploy.succeeded",
		Payload:     json.RawMessage(`{"environment":"prod"}`),
	}
	if deliveryID != "" {
		id := deliveryID
		e.DeliveryID = &id
	}
	return e
}

// TestEventDeliveryWindowIdempotent 验去重窗：窗口内不重复入库；**过窗后旧事件仍在、
// 同 id 可以当新事件接收**（新 eventId）；无 deliveryId 每次新行。
func TestEventDeliveryWindowIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-window")
	window := int64(60)
	now := store.Now()

	first := evt(ns, "trace-1", "hook", "deliver-1")
	inserted, err := db.InsertEvent(ctx, first, now, window)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}

	// 同 id 再来一次（窗口内）：不插，回已有行。
	dup := evt(ns, "trace-2", "hook", "deliver-1")
	inserted, err = db.InsertEvent(ctx, dup, now+10, window)
	if err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	if inserted {
		t.Fatalf("窗口内重复投递不该入库")
	}
	if dup.ID != first.ID || dup.TraceID != "trace-1" {
		t.Fatalf("重复投递应回已有 event/trace：id=%s trace=%s", dup.ID, dup.TraceID)
	}

	// 窗口过期（fake clock 推到 window 之后）：同 id 当成**新事件**接收，
	// 旧事件行**不因为去重过期而被删**（去重是窗口，不是实体生命周期）。
	late := evt(ns, "trace-3", "hook", "deliver-1")
	inserted, err = db.InsertEvent(ctx, late, now+window+1, window)
	if err != nil {
		t.Fatalf("late insert: %v", err)
	}
	if !inserted {
		t.Fatalf("窗口过期后同 id 应可再次被接收")
	}
	if late.ID == first.ID {
		t.Fatalf("窗口过期后应是新事件行（新 eventId）")
	}
	if _, err := db.GetEvent(ctx, ns, first.ID); err != nil {
		t.Fatalf("去重过期不该删旧事件实体：%v", err)
	}
	rows, err := db.ListEvents(ctx, ns, "", "hook", 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("旧事件应仍在（去重过期 ≠ 删实体），得到 %d 行", len(rows))
	}
	// 再次重复（新窗口内）→ 命中**新**事件。
	dup2 := evt(ns, "trace-4", "hook", "deliver-1")
	if inserted, err := db.InsertEvent(ctx, dup2, now+window+2, window); err != nil || inserted {
		t.Fatalf("刷新后的窗口应继续去重：inserted=%v err=%v", inserted, err)
	}
	if dup2.ID != late.ID {
		t.Fatalf("新窗口应指向新事件：%s != %s", dup2.ID, late.ID)
	}

	// 没有 deliveryId：每次都是新事件。
	a := evt(ns, "trace-5", "hook", "")
	b := evt(ns, "trace-6", "hook", "")
	if ok, err := db.InsertEvent(ctx, a, now, window); err != nil || !ok {
		t.Fatalf("no-id insert a: %v %v", ok, err)
	}
	if ok, err := db.InsertEvent(ctx, b, now, window); err != nil || !ok {
		t.Fatalf("no-id insert b: %v %v", ok, err)
	}
	if a.ID == b.ID {
		t.Fatalf("无 deliveryId 的两次投递必须是两条事件")
	}
}

// TestEventDeliveryWindowPermanent 验 windowSec=0（永久窗口）：再久也不重复接收。
func TestEventDeliveryWindowPermanent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-permanent")
	now := store.Now()

	first := evt(ns, "trace-p", "hook", "deliver-p")
	if inserted, err := db.InsertEvent(ctx, first, now, 0); err != nil || !inserted {
		t.Fatalf("first: %v %v", inserted, err)
	}
	// 十年后再投一次：窗口永久 → 仍然去重。
	again := evt(ns, "trace-p2", "hook", "deliver-p")
	if inserted, err := db.InsertEvent(ctx, again, now+10*365*24*3600, 0); err != nil || inserted {
		t.Fatalf("永久窗口不该允许重复接收：inserted=%v err=%v", inserted, err)
	}
	if again.ID != first.ID {
		t.Fatalf("永久窗口应回第一条事件")
	}
}

// TestEventDeliveryConcurrentSameWindowOnlyOnce 验并发同窗只收一次。
//
// 多个 goroutine 同时投同一个 deliveryId：只有一个 inserted=true，且库里只有一条事件。
func TestEventDeliveryConcurrentSameWindowOnlyOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-concurrent")
	now := store.Now()

	const n = 6
	type result struct {
		inserted bool
		id       string
		err      error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			e := evt(ns, "trace-cc", "hook", "deliver-cc")
			ok, err := db.InsertEvent(ctx, e, now, 3600)
			results <- result{inserted: ok, id: e.ID, err: err}
		}()
	}
	close(start)

	insertedCount := 0
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent insert: %v", r.err)
		}
		if r.inserted {
			insertedCount++
		}
	}
	if insertedCount != 1 {
		t.Fatalf("并发同窗应只收一次，得到 %d 次", insertedCount)
	}
	rows, err := db.ListEvents(ctx, ns, "", "hook", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("并发同窗应只有一条事件，得到 %d", len(rows))
	}
}

// TestEventPruneAlwaysRecorded 验剪枝留痕永远写（不幂等），且强制 reason/source/type。
func TestEventPruneAlwaysRecorded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-prune")

	for i := 0; i < 2; i++ {
		e := &store.Event{
			NamespaceID: ns,
			TraceID:     "trace-prune",
			Source:      "hook", // 会被强制改成 route
			Type:        pruneType,
			Payload:     json.RawMessage(`{"param":"env"}`),
		}
		if err := db.InsertPruneEvent(ctx, e); err != nil {
			t.Fatalf("insert prune %d: %v", i, err)
		}
		if e.Source != store.EventPruneSource || e.Reason != store.EventReasonPruned {
			t.Fatalf("剪枝行应强制 source=route / reason=Pruned，得到 %s/%s", e.Source, e.Reason)
		}
	}
	pruned, err := db.ListEvents(ctx, ns, "trace-prune", store.EventPruneSource, 50)
	if err != nil {
		t.Fatalf("list prunes: %v", err)
	}
	if len(pruned) != 2 {
		t.Fatalf("剪枝必须永远留痕（两次 → 两行），得到 %d", len(pruned))
	}
	// 事件流（无 trace、无 source）不含剪枝。
	stream, err := db.ListEvents(ctx, ns, "", "", 50)
	if err != nil {
		t.Fatalf("list stream: %v", err)
	}
	if len(stream) != 0 {
		t.Fatalf("事件流不该混入剪枝，得到 %d 行", len(stream))
	}
	// 未知剪枝码与保留来源名都被拒。
	if err := db.InsertPruneEvent(ctx, &store.Event{NamespaceID: ns, TraceID: "t", Type: "script_failed"}); err == nil {
		t.Fatalf("非剪枝码不该被当成剪枝留痕")
	}
	if _, err := db.InsertEvent(ctx, evt(ns, "t2", store.EventPruneSource, ""), store.Now(), 0); !errors.Is(err, store.ErrReservedEventSource) {
		t.Fatalf("业务事件用保留来源名应被拒，得到 %v", err)
	}
}

// TestEventPageEquivalence 验分页版与列表版口径逐字一致（差异只允许游标谓词）。
func TestEventPageEquivalence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-page")
	base := store.Now()
	for i := 0; i < 5; i++ {
		e := evt(ns, "trace-page", "hook", "")
		e.CreatedAt = base - int64(i)
		if ok, err := db.InsertEvent(ctx, e, base, 0); err != nil || !ok {
			t.Fatalf("insert %d: %v %v", i, ok, err)
		}
	}
	all, err := db.ListEvents(ctx, ns, "", "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	page, err := db.ListEventsBefore(ctx, ns, "", "", base+1, "", 50)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(all) != len(page) {
		t.Fatalf("两种口径行数不同：%d vs %d", len(all), len(page))
	}
	for i := range all {
		if all[i].ID != page[i].ID {
			t.Fatalf("第 %d 行不同：%s vs %s", i, all[i].ID, page[i].ID)
		}
	}
	// 游标：从第 2 行之前继续取，不应重复也不应跳行。
	if len(all) >= 3 {
		next, err := db.ListEventsBefore(ctx, ns, "", "", all[1].CreatedAt, all[1].ID, 50)
		if err != nil {
			t.Fatalf("page next: %v", err)
		}
		if len(next) != len(all)-2 {
			t.Fatalf("游标后应剩 %d 行，得到 %d", len(all)-2, len(next))
		}
		for i := range next {
			if next[i].ID != all[i+2].ID {
				t.Fatalf("游标页第 %d 行错位", i)
			}
		}
	}
}

// TestEventRoundTrip 验字段往返（payload/deliveryId/reason）与空列表。
func TestEventRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "evt-roundtrip")
	in := evt(ns, "trace-rt", "hook", "deliver-rt")
	if ok, err := db.InsertEvent(ctx, in, store.Now(), 0); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	got, err := db.GetEvent(ctx, ns, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeliveryID == nil || *got.DeliveryID != "deliver-rt" || got.Reason != store.EventReasonIngested {
		t.Fatalf("字段往返失真：%+v", got)
	}
	if string(got.Payload) != `{"environment":"prod"}` {
		t.Fatalf("payload 不该被改写：%s", got.Payload)
	}
	byTrace, err := db.GetEventByTrace(ctx, ns, "trace-rt")
	if err != nil || byTrace.ID != in.ID {
		t.Fatalf("get by trace: %v", err)
	}
	empty, err := db.ListEvents(ctx, ns, "trace-missing", "", 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("空列表必须是 []：%v %v", empty, err)
	}
	_ = protocol.ReasonParamMissing
}
