package store

// event_prune_test.go 覆盖 v4 剪枝留痕（event.md §剪枝）：
//
//	剪枝是「事件收到了但没派出去」的唯一证据，来源固定为保留名 route；
//	它**不去重**（同一 trace 多条剪枝必须各留一行，否则掩盖扇出规模）；
//	事件流（无 source/trace 过滤）里看不见它，按 source=route 或 trace 才看得到。
//
// v3 的 idem_key 去重/IdemKey 列已删除：去重只认 deliveryId 的有界窗口。

import (
	"context"
	"encoding/json"
	"testing"
)

// TestInsertPruneEventDefaults 剪枝留痕的固定形状：source=route、reason=Pruned、payload 归一。
func TestInsertPruneEventDefaults(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	e := &Event{NamespaceID: nsID, TraceID: "tr_prune_defaults", Type: "downstream_missing"}
	if err := db.InsertPruneEvent(ctx, e); err != nil {
		t.Fatalf("insert prune event: %v", err)
	}
	if e.ID == "" || e.CreatedAt == 0 {
		t.Fatalf("剪枝留痕未回填 id/created_at: %+v", e)
	}
	got, err := db.GetEvent(ctx, nsID, e.ID)
	if err != nil {
		t.Fatalf("get prune event: %v", err)
	}
	if got.Source != EventPruneSource {
		t.Fatalf("source = %q, want %q", got.Source, EventPruneSource)
	}
	if got.Reason != protocolPruned() {
		t.Fatalf("reason = %q, want %q", got.Reason, protocolPruned())
	}
	if !json.Valid(got.Payload) || string(got.Payload) != "{}" {
		t.Fatalf("空 payload 应归一成 {}，得到 %s", got.Payload)
	}
}

// protocolPruned 是剪枝留痕的 reason 常量（A 轮不复制协议包，用字面量=契约值）。
func protocolPruned() string { return "Pruned" }

// TestInsertPruneEventValidation 剪枝入口只收剪枝原因码 + 必须带 trace。
func TestInsertPruneEventValidation(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	cases := []struct {
		name string
		e    *Event
	}{
		{"nil", nil},
		{"no-trace", &Event{NamespaceID: nsID, Type: "downstream_missing"}},
		{"no-type", &Event{NamespaceID: nsID, TraceID: "tr"}},
		{"bad-reason", &Event{NamespaceID: nsID, TraceID: "tr", Type: "not_a_prune_reason"}},
		{"no-namespace", &Event{TraceID: "tr", Type: "downstream_missing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.InsertPruneEvent(ctx, tc.e); err == nil {
				t.Fatalf("非法剪枝留痕被接受: %+v", tc.e)
			}
		})
	}
}

// TestListEventsHidesPrunesFromStream 事件流默认隐藏剪枝留痕；点名 source/trace 才看得到。
func TestListEventsHidesPrunesFromStream(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	real := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), Now(), 0)
	prune := &Event{NamespaceID: nsID, TraceID: real.TraceID, Type: "downstream_missing"}
	if err := db.InsertPruneEvent(ctx, prune); err != nil {
		t.Fatalf("insert prune: %v", err)
	}

	stream, err := db.ListEvents(ctx, nsID, "", "", 50)
	if err != nil {
		t.Fatalf("list stream: %v", err)
	}
	if len(stream) != 1 || stream[0].ID != real.ID {
		t.Fatalf("事件流 = %+v, want 只有真实事件 %s", stream, real.ID)
	}
	prunes, err := db.ListEvents(ctx, nsID, "", EventPruneSource, 50)
	if err != nil {
		t.Fatalf("list by prune source: %v", err)
	}
	if len(prunes) != 1 || prunes[0].ID != prune.ID {
		t.Fatalf("按 route 过滤 = %+v, want 剪枝行 %s", prunes, prune.ID)
	}
	// trace 视图包含两条（诊断要能看到「这一跳被剪掉了」）。
	byTrace, err := db.ListEvents(ctx, nsID, real.TraceID, "", 50)
	if err != nil {
		t.Fatalf("list by trace: %v", err)
	}
	if len(byTrace) != 2 {
		t.Fatalf("同 trace 行数 = %d, want 2（真实事件 + 剪枝留痕）", len(byTrace))
	}
}

// TestInsertEventRejectsReservedSource 普通入口不能冒充剪枝来源。
func TestInsertEventRejectsReservedSource(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	_, err := db.InsertEvent(ctx, &Event{
		NamespaceID: nsID, TraceID: "tr", Source: EventPruneSource, Type: "push",
	}, Now(), 0)
	if err == nil {
		t.Fatal("普通插入用了保留来源名 route")
	}
	// 源名也不能叫 route（避免同名源与剪枝留痕混流）。
	if err := db.CreateSource(ctx, &Source{NamespaceID: nsID, Name: EventPruneSource, Kind: SourceKindWebhook}); err == nil {
		t.Fatal("源名允许叫 route，会与剪枝留痕混流")
	}
}
