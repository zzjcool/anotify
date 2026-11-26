package source

import (
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// TestIngestEntryDepthStartsAtOne ⑤：源入口也走预算，根任务 depth 从 1 起算。
func TestIngestEntryDepthStartsAtOne(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "worker", "")
	src := testSource(t, db, ns.ID, "hook-budget", sourceOpts{
		Kind: sourcespec.KindWebhook, Config: `{}`,
		Targets: []sourcespec.Target{{Capability: "worker"}}, Enabled: true,
	})

	tight := NewWithClock(db, route.New(db).WithBudget(route.Budget{MaxHops: 1, TraceTaskBudget: 100}), nil)
	receipt, err := tight.Ingest(testCtx(), Input{Source: src, DeliveryID: "d-tight"})
	if err != nil {
		t.Fatalf("剪枝不是 4xx: %v", err)
	}
	if receipt.EventID == "" || receipt.TraceID == "" {
		t.Fatalf("剪枝也必须留下事件（事件照收）: %+v", receipt)
	}
	if len(receipt.Tasks) != 0 {
		t.Fatalf("剪枝不许建任务: %+v", receipt.Tasks)
	}
	if len(receipt.Pruned) != 1 || receipt.Pruned[0].Reason != protocol.ReasonMaxHops {
		t.Fatalf("剪枝原因 = %+v，want max_hops", receipt.Pruned)
	}
	if n := countRows(t, db, "tasks"); n != 0 {
		t.Fatalf("tasks 表应仍为空，实际 %d 行", n)
	}
	var traced int
	if err := db.QueryRowContext(testCtx(),
		`SELECT COUNT(*) FROM events WHERE trace_id = ? AND source = 'route' AND type = ?`,
		receipt.TraceID, protocol.ReasonMaxHops).Scan(&traced); err != nil {
		t.Fatalf("统计剪枝事件: %v", err)
	}
	if traced != 1 {
		t.Fatalf("max_hops 留痕行数 = %d，want 1", traced)
	}

	room := NewWithClock(db, route.New(db).WithBudget(route.Budget{MaxHops: 2, TraceTaskBudget: 100}), nil)
	accepted, err := room.Ingest(testCtx(), Input{Source: src, DeliveryID: "d-room"})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(accepted.Tasks) != 1 || len(accepted.Pruned) != 0 {
		t.Fatalf("depth 1 < max_hops 2 必须建任务: %+v", accepted)
	}
	depth, err := db.AncestorDepth(testCtx(), ns.ID, accepted.Tasks[0].TaskID)
	if err != nil || depth != 1 {
		t.Fatalf("入口任务深度 = %d（err=%v），want 1", depth, err)
	}
}
