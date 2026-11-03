package route

import (
	"context"
	"errors"
	"testing"
)

// ---------- emit 的两条护栏（P1 收尾） ----------
//
// 这两条都是「emit 能凭空产生下游任务」的推论：
//  1. 归属：任务派给 A，B 不能替它 emit（否则任一节点可伪造别人的产出）
//  2. 备注：note 不参与路由，但必须落库——排障时「第 3 次 emit 在说什么」
//     经常比 port/data 更有信息量。

// TestEmitRejectsForeignNode 守住归属栅栏。
func TestEmitRejectsForeignNode(t *testing.T) {
	f := newEmitFixture(t)
	capDef := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object","properties":{"diff":{"type":"string"}}}`)
	task := f.newTask(t, capDef)

	// 把任务派给 nd_owner（真实流程里由派发/ack 写进去；这里直插最省事）。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE tasks SET node_id = ? WHERE id = ?`, "nd_owner", task.ID); err != nil {
		t.Fatalf("assign node: %v", err)
	}

	// 别人的帧：拒，且**不落 emission**（拒收必须是无副作用的）。
	_, err := f.d.Emit(context.Background(), EmitInput{
		NamespaceID: f.ns, TaskID: task.ID, Seq: 1, Port: "fix", NodeID: "nd_intruder",
	})
	if !errors.Is(err, ErrEmitForeignNode) {
		t.Fatalf("别的节点 emit err = %v, want ErrEmitForeignNode", err)
	}
	if got := f.emissions(t, task.ID); len(got) != 0 {
		t.Fatalf("被拒的 emit 落了库：%d 条", len(got))
	}
	if got := f.childTasks(t, task.ID); len(got) != 0 {
		t.Fatalf("被拒的 emit 建了下游：%d 条", len(got))
	}

	// 自己的帧：放行。
	out, err := f.d.Emit(context.Background(), EmitInput{
		NamespaceID: f.ns, TaskID: task.ID, Seq: 1, Port: "fix", NodeID: "nd_owner",
	})
	if err != nil {
		t.Fatalf("本节点 emit: %v", err)
	}
	if out.Seq != 1 {
		t.Fatalf("seq = %d, want 1", out.Seq)
	}

	// 空 NodeID（内部调用/老任务行）不做归属校验：历史任务必须能继续跑。
	if _, err := f.d.Emit(context.Background(), EmitInput{
		NamespaceID: f.ns, TaskID: task.ID, Seq: 2, Port: "fix",
	}); err != nil {
		t.Fatalf("内部调用（无 NodeID）被归属栅栏挡住：%v", err)
	}
}

// TestEmitPersistsNote 守住「note 必须落库」。
func TestEmitPersistsNote(t *testing.T) {
	f := newEmitFixture(t)
	capDef := f.addCap(t, "review", 1, []string{"fix"}, `{}`)
	task := f.newTask(t, capDef)

	if _, err := f.d.Emit(context.Background(), EmitInput{
		NamespaceID: f.ns, TaskID: task.ID, Seq: 1, Port: "fix", Note: "第 2 项：空指针",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	got := f.emissions(t, task.ID)
	if len(got) != 1 {
		t.Fatalf("emissions = %d, want 1", len(got))
	}
	if got[0].Note != "第 2 项：空指针" {
		t.Fatalf("note 没落库：%q", got[0].Note)
	}
	// 幂等重发不改首次结果（note 也一样）。
	if _, err := f.d.Emit(context.Background(), EmitInput{
		NamespaceID: f.ns, TaskID: task.ID, Seq: 1, Port: "fix", Note: "改过的备注",
	}); err != nil {
		t.Fatalf("重复 seq 应当幂等成功: %v", err)
	}
	got = f.emissions(t, task.ID)
	if len(got) != 1 || got[0].Note != "第 2 项：空指针" {
		t.Fatalf("幂等重发覆盖了首次 note：%+v", got)
	}
}
