package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func seedSummaryTask(t *testing.T, db *DB, nsID, capID, capName, traceID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO tasks
		(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at)
		VALUES (?, ?, ?, ?, ?, 1, 'sha256:seed', '{}', ?, ?)`,
		NewTaskID(), nsID, traceID, capID, capName, TaskPending, Now())
}

func seedSummaryPrune(t *testing.T, db *DB, nsID, traceID, reason, detail string, createdAt int64) {
	t.Helper()
	payload := fmt.Sprintf(`{"capability":"cap","reason":%q,"detail":%q}`, reason, detail)
	mustExec(t, db, `INSERT INTO events
		(id, namespace_id, trace_id, source, type, payload, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		NewEventID(), nsID, traceID, EventPruneSource, reason, payload, EventReasonPruned, createdAt)
}

func TestSummarizeTraces(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	otherNS := newTestExtraNS(t, db, "trace-summary-b")
	capA := v4Cap(t, db, nsID, "summary-cap", 1)
	capB := v4Cap(t, db, otherNS, "summary-cap", 1)

	seedSummaryTask(t, db, nsID, capA.ID, capA.Name, "trace-a")
	seedSummaryTask(t, db, nsID, capA.ID, capA.Name, "trace-a")
	seedSummaryPrune(t, db, nsID, "trace-a", "param_missing", "需要 n", 100)
	seedSummaryPrune(t, db, nsID, "trace-a", "downstream_missing", "目标不存在", 200)
	seedSummaryTask(t, db, nsID, capA.ID, capA.Name, "trace-b")
	seedSummaryTask(t, db, otherNS, capB.ID, capB.Name, "trace-a")
	seedSummaryPrune(t, db, otherNS, "trace-a", "no_target", "别的 ns", 100)

	got, err := db.SummarizeTraces(ctx, nsID, []string{"trace-a", "trace-b", "trace-missing", ""})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	a, ok := got["trace-a"]
	if !ok {
		t.Fatalf("trace-a 不在聚合结果里: %v", got)
	}
	if a.Tasks != 2 {
		t.Fatalf("trace-a.Tasks = %d, want 2", a.Tasks)
	}
	if len(a.RouteEvents) != 2 {
		t.Fatalf("trace-a.RouteEvents = %d 条, want 2", len(a.RouteEvents))
	}
	if a.RouteEvents[0].Type != "param_missing" || a.RouteEvents[1].Type != "downstream_missing" {
		t.Fatalf("trace-a.RouteEvents 顺序 = %q,%q（want 按 created_at 升序）",
			a.RouteEvents[0].Type, a.RouteEvents[1].Type)
	}
	if a.RouteEvents[0].Source != EventPruneSource || a.RouteEvents[0].Reason != EventReasonPruned {
		t.Fatalf("RouteEvents 行形状 = %+v（want source=route / reason=Pruned）", a.RouteEvents[0])
	}

	b := got["trace-b"]
	if b.Tasks != 1 || len(b.RouteEvents) != 0 {
		t.Fatalf("trace-b = %+v, want 1 task / 0 prune", b)
	}
	if _, ok := got["trace-missing"]; ok {
		t.Fatalf("没有被引用的 trace 不该出现在 map 里（读零值即可）")
	}

	other, err := db.SummarizeTraces(ctx, otherNS, []string{"trace-a"})
	if err != nil {
		t.Fatalf("summarize other ns: %v", err)
	}
	if o := other["trace-a"]; o.Tasks != 1 || len(o.RouteEvents) != 1 || o.RouteEvents[0].Type != "no_target" {
		t.Fatalf("跨 ns 结果串了：%+v", o)
	}

	empty, err := db.SummarizeTraces(ctx, nsID, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空输入 = %v / %v, want 空 map", empty, err)
	}

	if _, err := db.SummarizeTraces(ctx, AllNamespaces, []string{"trace-a"}); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("AllNamespaces 应报 ErrNamespaceRequired, got %v", err)
	}
}

func TestSummarizeTracesChunked(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	capA := v4Cap(t, db, nsID, "chunk-cap", 1)

	ids := make([]string, 0, traceSummaryChunk+2)
	for i := 0; i < traceSummaryChunk+2; i++ {
		ids = append(ids, fmt.Sprintf("trace-chunk-%03d", i))
	}
	seedSummaryTask(t, db, nsID, capA.ID, capA.Name, ids[0])
	seedSummaryPrune(t, db, nsID, ids[traceSummaryChunk], "param_missing", "跨块", 1)
	seedSummaryTask(t, db, nsID, capA.ID, capA.Name, ids[len(ids)-1])

	got, err := db.SummarizeTraces(ctx, nsID, ids)
	if err != nil {
		t.Fatalf("summarize chunked: %v", err)
	}
	if got[ids[0]].Tasks != 1 {
		t.Fatalf("首块任务数丢了: %+v", got[ids[0]])
	}
	if len(got[ids[traceSummaryChunk]].RouteEvents) != 1 {
		t.Fatalf("末块剪枝丢了: %+v", got[ids[traceSummaryChunk]])
	}
	if got[ids[len(ids)-1]].Tasks != 1 {
		t.Fatalf("末块任务数丢了: %+v", got[ids[len(ids)-1]])
	}
}
