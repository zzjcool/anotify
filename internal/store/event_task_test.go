package store

// event_task_test.go 覆盖 v4 事件入口（event.md）：
//
//	投递幂等 = (namespace, 源稳定 id, deliveryId) 的有界窗口；窗口过期后同一
//	deliveryId 是一次**新的**投递（窗口只压重复，不吞告警）。
//
// v3 的 idem_key 全表唯一、path 扇出建任务、以及「事件驱动建任务一次成功」
// 由 emit（EmitAuthorized 同事务）与 CreateTasks（任务事务）各自覆盖：
// 任务侧用例在 task_test.go，emit 侧在 emission_test.go。

import (
	"context"
	"errors"
	"testing"
)

// TestEventIdempotency 同一 deliveryId 在窗口内重复投递 → 返回已有事件，不新增行。
func TestEventIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	delivery := "dlv-1"
	first := v4Event(nsID, "github", "push")
	first.DeliveryID = &delivery
	if ok, err := db.InsertEvent(ctx, first, Now(), 300); err != nil || !ok {
		t.Fatalf("first insert: inserted=%v err=%v", ok, err)
	}

	second := v4Event(nsID, "github", "push")
	second.DeliveryID = &delivery
	ok, err := db.InsertEvent(ctx, second, Now(), 300)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if ok {
		t.Fatal("窗口内的重复投递被当成新事件插入")
	}
	if second.ID != first.ID {
		t.Fatalf("重复投递返回的事件 id = %q, want 已有 %q", second.ID, first.ID)
	}
	list, err := db.ListEvents(ctx, nsID, "", "github", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("重复投递留下 %d 行，want 1", len(list))
	}
}

// TestEventWithoutIdemKeyAllowsDuplicates 不带 deliveryId 的投递永远是新事件。
func TestEventWithoutIdemKeyAllowsDuplicates(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	a := v4Event(nsID, "github", "push")
	b := v4Event(nsID, "github", "push")
	b.TraceID = a.TraceID // 同 trace 也不去重（去重只认 deliveryId）
	for _, e := range []*Event{a, b} {
		inserted, err := db.InsertEvent(ctx, e, Now(), 300)
		if err != nil || !inserted {
			t.Fatalf("insert %s: inserted=%v err=%v", e.ID, inserted, err)
		}
	}
	if a.ID == b.ID {
		t.Fatal("无 deliveryId 的两次投递共用了事件 id")
	}
	list, err := db.ListEvents(ctx, nsID, "", "", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("事件数 = %d, want 2", len(list))
	}
}

// TestEventDeliveryWindowExpiry 窗口过期后同一 deliveryId 重新占窗 = 新事件。
func TestEventDeliveryWindowExpiry(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	delivery := "dlv-expire"
	now := int64(1_700_000_000)
	first := v4Event(nsID, "webhook", "in")
	first.DeliveryID = &delivery
	if ok, err := db.InsertEvent(ctx, first, now, 60); err != nil || !ok {
		t.Fatalf("first insert: %v %v", ok, err)
	}
	// 窗口内重复：吸收。
	dup := v4Event(nsID, "webhook", "in")
	dup.DeliveryID = &delivery
	if ok, err := db.InsertEvent(ctx, dup, now+30, 60); err != nil || ok {
		t.Fatalf("窗口内重复应被吸收: inserted=%v err=%v", ok, err)
	}
	// 窗口过期后（now+61 > expires_at）：新的一条。
	after := v4Event(nsID, "webhook", "in")
	after.DeliveryID = &delivery
	ok, err := db.InsertEvent(ctx, after, now+61, 60)
	if err != nil {
		t.Fatalf("insert after window: %v", err)
	}
	if !ok {
		t.Fatal("窗口过期后的投递仍被吸收（告警会被永久吞掉）")
	}
	if after.ID == first.ID {
		t.Fatal("过期后的投递复用了旧事件行")
	}
}

// TestEventDeliveryScopedPerSourceAndNamespace 同一 deliveryId 只在 (ns, 源) 内去重。
func TestEventDeliveryScopedPerSourceAndNamespace(t *testing.T) {
	db := newTestDB(t)
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "event-other")
	ctx := context.Background()

	delivery := "dlv-shared"
	insert := func(nsID, source string) *Event {
		e := v4Event(nsID, source, "in")
		e.DeliveryID = &delivery
		ok, err := db.InsertEvent(ctx, e, Now(), 300)
		if err != nil {
			t.Fatalf("insert %s/%s: %v", nsID, source, err)
		}
		if !ok {
			t.Fatalf("%s/%s 的首次投递被判重复（去重串了维度）", nsID, source)
		}
		return e
	}
	insert(nsA, "github")
	insert(nsA, "gitlab") // 同 ns、不同源
	insert(nsB, "github") // 不同 ns、同名源

	// 同名源在两个命名空间里是两条不同的事件。
	if _, err := db.GetEvent(ctx, nsB, ""); err == nil {
		t.Fatal("空事件 id 不该查到东西")
	}
}

// TestEventGetAndList 读写往返 + 过滤（trace/source/limit）。
func TestEventGetAndList(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	e1 := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), Now(), 0)
	e2 := v4InsertEvent(t, db, v4Event(nsID, "gitlab", "mr"), Now(), 0)
	e2.TraceID = e1.TraceID

	got, err := db.GetEvent(ctx, nsID, e1.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.Source != "github" || got.Type != "push" || got.TraceID != e1.TraceID {
		t.Fatalf("事件往返不一致: %+v", got)
	}
	byTrace, err := db.GetEventByTrace(ctx, nsID, e1.TraceID)
	if err != nil {
		t.Fatalf("get by trace: %v", err)
	}
	if byTrace.ID != e1.ID && byTrace.ID != e2.ID {
		t.Fatalf("by-trace 返回 %s，不在同 trace 的两条里", byTrace.ID)
	}
	all, err := db.ListEvents(ctx, nsID, e1.TraceID, "", 0)
	if err != nil {
		t.Fatalf("list by trace: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("同 trace 事件数 = %d, want 2", len(all))
	}
	onlyGitlab, err := db.ListEvents(ctx, nsID, "", "gitlab", 0)
	if err != nil {
		t.Fatalf("list by source: %v", err)
	}
	if len(onlyGitlab) != 1 || onlyGitlab[0].ID != e2.ID {
		t.Fatalf("source 过滤 = %+v", onlyGitlab)
	}
}

// TestEventEmptyListIsArray 空列表是 []。
func TestEventEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	list, err := db.ListEvents(context.Background(), nsID, "", "", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if list == nil {
		t.Fatal("空事件列表返回 nil，want 空切片")
	}
}

// TestEventValidation 必填字段与保留源都必须拒。
func TestEventValidation(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	cases := []struct {
		name string
		e    *Event
	}{
		{"no-source", &Event{NamespaceID: nsID, TraceID: "tr", Type: "push"}},
		{"no-type", &Event{NamespaceID: nsID, TraceID: "tr", Source: "github"}},
		{"no-trace", &Event{NamespaceID: nsID, Source: "github", Type: "push"}},
		{"reserved-source", &Event{NamespaceID: nsID, TraceID: "tr", Source: EventPruneSource, Type: "push"}},
		{"no-namespace", &Event{TraceID: "tr", Source: "github", Type: "push"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.InsertEvent(ctx, tc.e, Now(), 0); err == nil {
				t.Fatalf("非法事件被接受: %+v", tc.e)
			}
		})
	}
	if !errors.Is(func() error {
		_, err := db.InsertEvent(ctx, &Event{NamespaceID: nsID, TraceID: "tr", Source: EventPruneSource, Type: "push"}, Now(), 0)
		return err
	}(), ErrReservedEventSource) {
		t.Fatal("保留源应报 ErrReservedEventSource")
	}
}

// TestInsertPruneEventNeverDeduped 剪枝留痕永远留痕（同一 trace 多条也要各写一行）。
func TestInsertPruneEventNeverDeduped(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		e := &Event{NamespaceID: nsID, TraceID: "tr_prune", Type: "downstream_missing"}
		if err := db.InsertPruneEvent(ctx, e); err != nil {
			t.Fatalf("insert prune event %d: %v", i, err)
		}
	}
	list, err := db.ListEvents(ctx, nsID, "tr_prune", "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("剪枝留痕 = %d 行, want 2（剪枝不去重）", len(list))
	}
	for _, e := range list {
		if e.Source != EventPruneSource {
			t.Fatalf("剪枝事件的 source = %q, want %q", e.Source, EventPruneSource)
		}
	}
}
