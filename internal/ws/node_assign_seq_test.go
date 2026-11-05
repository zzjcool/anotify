package ws

// assign 帧里的 `attempts` / `seq_in_trace` 的回归钉子。
//
// 中枢侧的事实源只有**库里那一行**：route 在派发成功后把内存里的 task.Attempts 写死成 1
// （internal/route/dispatch.go），而 task.SeqInTrace 在内存里恒为 0。所以这条链路上
// 有两个各自都可能错的环节，本文件把它们逐个钉住：
//
//	① assignFrameFor 是纯函数 —— 只从入参取数（好测、也不会偷偷去读库）；
//	② PublishAssign 在**发帧之前**用库里回读的 fresh 覆盖这两个字段，
//	   并且体积闸门量的是**最终要发的字节**；
//	③ 老行（seq_in_trace = 0）发出去的帧里字段缺席（omitempty），
//	   节点据此退回到 attempt —— 不要把 0 当成“第一次执行”。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// seqAssignTask 建一条 pending 任务（同一 trace + review@1），并返回它的行。
func seqAssignTask(t *testing.T, f *nodeFixture, traceID string) *store.Task {
	t.Helper()
	ctx := context.Background()
	capv, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability review@1: %v", err)
	}
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: traceID, CapabilityID: capv.ID,
		Capability: capv.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: capv.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// assignViaProd 走生产的派发路径（LeaseAndAssign：占槽 + pending→assigned 一个事务）。
func assignViaProd(t *testing.T, f *nodeFixture, task *store.Task) {
	t.Helper()
	if _, err := f.db.LeaseAndAssign(context.Background(), f.nsID, task.ID, f.node.ID,
		store.Now()+store.AckWaitSec); err != nil {
		t.Fatalf("lease and assign %s: %v", task.ID, err)
	}
}

// TestAssignFrameForIsPure ①：assignFrameFor 只从入参取数。
//
// 这条不是废话：它的入参在生产里恰好是**不可信**的那份副本（route 硬编码 attempts=1、
// seq 恒 0）。把“纯函数照抄入参”和“发布路径用库里的值覆盖”分成两条断言，
// 才能在覆盖那一步出问题时立刻看出是哪一半坏了。
func TestAssignFrameForIsPure(t *testing.T) {
	frame := assignFrameFor(&store.Task{
		ID: "tsk_pure", Capability: "review", Version: 1,
		Params: json.RawMessage(`{}`), Attempts: 3, SeqInTrace: 7,
	})
	if frame.Attempts != 3 || frame.SeqInTrace != 7 {
		t.Fatalf("assignFrameFor 的 attempts/seq = %d/%d，want 3/7（纯函数只照入参）",
			frame.Attempts, frame.SeqInTrace)
	}

	data, err := Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["attempts"] != float64(3) {
		t.Errorf("线上帧 attempts = %v，want 3", wire["attempts"])
	}
	if wire["seq_in_trace"] != float64(7) {
		t.Errorf("线上帧 seq_in_trace = %v，want 7", wire["seq_in_trace"])
	}
}

// TestPublishAssignCarriesLibraryAttemptsAndSeq ②：经 PublishAssign 发出去的帧里，
// attempts / seq_in_trace 与库里那一行逐条一致。
//
// 三条任务同一 trace + 同一能力（扇出形状），所以库里必然是 attempts=1、seq={1,2,3}。
func TestPublishAssignCarriesLibraryAttemptsAndSeq(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}, {Slot: 2, Concurrency: 1}})

	const n = 3
	batch := make([]*store.Task, n)
	for i := range batch {
		batch[i] = seqAssignTask(t, f, "trace_assign_seq")
		assignViaProd(t, f, batch[i])
	}

	for i, task := range batch {
		// 发送方刻意给一份“route 形状”的内存副本（attempts=1、seq=0）：
		// 帧里必须出现库里的值，不是这份。
		stale := *task
		stale.Attempts = 1
		stale.SeqInTrace = 0
		if err := f.handler.hub().PublishAssign(context.Background(), f.nsID, f.node.ID, &stale); err != nil {
			t.Fatalf("publish assign #%d: %v", i, err)
		}
		frame := readUntil(t, conn, NodeFrameAssign)

		row := mustGetTask(t, f, task.ID)
		if row.Attempts != 1 {
			t.Fatalf("前提不成立：库里 attempts = %d，want 1", row.Attempts)
		}
		if row.SeqInTrace != i+1 {
			t.Fatalf("前提不成立：库里 seq_in_trace = %d，want %d", row.SeqInTrace, i+1)
		}
		if frame["attempts"] != float64(row.Attempts) {
			t.Errorf("第 %d 帧 attempts = %v，want %d（库里那份）", i, frame["attempts"], row.Attempts)
		}
		if frame["seq_in_trace"] != float64(row.SeqInTrace) {
			t.Errorf("第 %d 帧 seq_in_trace = %v，want %d（库里那份）", i, frame["seq_in_trace"], row.SeqInTrace)
		}
	}
}

// TestPublishAssignUsesLibraryValuesNotInMemory ②（回归主菜）：内存里的 attempts/seq
// 是陈旧值时，帧里仍是**库里**的权威值。
//
// 这条钉的正是「重派」这个场景：任务被巡检回收后重派，库里的 attempts 变成 2，
// 而调用方手里的 task（route.launch 传进来的那份）attempts 恒为 1、seq 恒为 0。
// 跟着内存走的话，节点会把第二次执行记成 a1、并在 runs/ 目录里与第一次撞名。
func TestPublishAssignUsesLibraryValuesNotInMemory(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}})

	ctx := context.Background()
	first := seqAssignTask(t, f, "trace_assign_redispatch")
	// 同一 trace 的第二条：让目标任务的 seq 不是 1（免得“恰好都等于 1”蒙对）。
	second := seqAssignTask(t, f, "trace_assign_redispatch")
	assignViaProd(t, f, first)
	assignViaProd(t, f, second)

	// 把 second 退回 pending 再派一次：库里 attempts = 2、seq = 2。
	// 重派前先释放/归还它上次占的槽位（租约巡检重排也是这个顺序，见 SweepExpiredLeases）。
	if err := f.db.ReleaseInstanceByTask(ctx, f.node.ID, second.ID); err != nil {
		t.Fatalf("release instance of %s: %v", second.ID, err)
	}
	if err := f.db.Transition(ctx, f.nsID, second.ID, store.TaskAssigned, store.TaskLeaseExpired); err != nil {
		t.Fatalf("transition lease_expired: %v", err)
	}
	if err := f.db.Transition(ctx, f.nsID, second.ID, store.TaskLeaseExpired, store.TaskPending); err != nil {
		t.Fatalf("transition pending: %v", err)
	}
	assignViaProd(t, f, second)

	row := mustGetTask(t, f, second.ID)
	if row.Attempts != 2 || row.SeqInTrace != 2 {
		t.Fatalf("前提不成立：库里 attempts/seq = %d/%d，want 2/2", row.Attempts, row.SeqInTrace)
	}

	// route 形状的内存副本：attempts 写死 1、seq 从未设置。
	stale := *second
	stale.Attempts = 1
	stale.SeqInTrace = 0
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, &stale); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	frame := readUntil(t, conn, NodeFrameAssign)
	if frame["attempts"] != float64(2) {
		t.Errorf("帧 attempts = %v，want 2（库里那份；内存里是 1）", frame["attempts"])
	}
	if frame["seq_in_trace"] != float64(2) {
		t.Errorf("帧 seq_in_trace = %v，want 2（库里那份；内存里是 0）", frame["seq_in_trace"])
	}
}

// TestPublishAssignOmitsZeroSeq ③：老行（加这一列之前落库、seq_in_trace = 0）的帧里
// 这个字段**缺席**——节点据此退回 attempt，而不是把它当成“第一次执行”。
func TestPublishAssignOmitsZeroSeq(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	task := seqAssignTask(t, f, "trace_assign_legacy")
	assignViaProd(t, f, task)
	// 模拟老行：这一列加进来之前落库的任务读出来就是 0。
	if _, err := f.db.Exec(`UPDATE tasks SET seq_in_trace = 0 WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("rewind seq_in_trace: %v", err)
	}

	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, mustGetTask(t, f, task.ID)); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	frame := readUntil(t, conn, NodeFrameAssign)
	if _, ok := frame["seq_in_trace"]; ok {
		t.Errorf("seq_in_trace = 0 被序列化出来了（%v）：老节点/老行靠字段缺席判断“未知”", frame["seq_in_trace"])
	}
	// attempts 照常带出去（它是 fencing token，不能省）。
	if frame["attempts"] != float64(1) {
		t.Errorf("帧 attempts = %v，want 1", frame["attempts"])
	}
}
