package store

// hardening_ctx_test.go 是 R5 的「ctx 取消」覆盖补齐（防线已在生产代码里，这里只补
// 可执行的证据）：
//
//	① AcceptSourceEvent  已取消的 ctx → context.Canceled、events/tasks/deliveries 全 0；
//	                       同一个 deliveryId 换 fresh ctx 必须还能 first=true（投递窗没被占）；
//	② AcceptSourceEvent  管道中途取消（唯一连接被占、调用已在排队等连接时 cancel）→ 从 DB
//	                       层返回 context.Canceled，0 写，同一 deliveryId 重试成功；
//	③ EmitAuthorized / CompleteTaskClassifiedAuthorized  已取消的 ctx → context.Canceled、
//	                       0 写（emit 行/计划事实/结果列都不许留半截），fresh ctx 重试能落库。
//
// 判定一次「取消真的生效」需要两个断言同时成立：错误可判为 context.Canceled **且**
// 库里 0 写。只断言其中一个都会被「fail closed 但已经写了」或「没写但也没报错」骗过去。
//
// 为什么 ② 必须是 pool=1 的库：pool=N 时调用会拿到另一条连接直接跑完，取消根本不可观测。
// 唯一连接被占住后，database/sql 的 WaitCount 增长是「这个调用确实排在等连接」的
// **确定性信号**（不是 sleep 猜出来的），取消发生在它排队期间，返回路径只能来自 ctx。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// acceptCall 是一次 AcceptSourceEvent 的完整返回（goroutine 收活用）。
type acceptCall struct {
	raw   json.RawMessage
	first bool
	err   error
}

// TestAcceptSourceEventCanceledContextWritesNothing 已取消的 ctx：拒绝 + 0 写 + 投递窗未被占。
func TestAcceptSourceEventCanceledContextWritesNothing(t *testing.T) {
	e := newIngressEnv(t)
	const (
		deliveryID = "dl-ctx"
		traceID    = "tr_ctx"
		eventType  = "push"
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, first, err := e.db.AcceptSourceEvent(ctx, e.inputs(deliveryID, traceID, eventType, Now()))
	if err == nil {
		t.Fatal("已取消的 ctx 仍然接收成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消路径的错误必须可判为 context.Canceled，得到 %v", err)
	}
	if first || raw != nil {
		t.Fatalf("取消路径不该给出回执: first=%v raw=%s", first, raw)
	}
	if c := countIngressRows(t, e.db, e.nsID, traceID, e.source.ID, deliveryID); c != (rowCounts{}) {
		t.Fatalf("取消路径留下了写（含投递窗）: %+v", c)
	}

	// 同一个 deliveryId 换 fresh ctx：取消没吃掉投递窗，必须真的接收。
	raw, first, err = e.db.AcceptSourceEvent(context.Background(), e.inputs(deliveryID, traceID, eventType, Now()))
	if err != nil {
		t.Fatalf("取消后重试: %v", err)
	}
	if !first {
		t.Fatal("取消后第一次成功接收必须 first=true（投递窗没被取消的事务占掉）")
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if len(receipt.Tasks) != 1 || len(receipt.FailedTasks) != 1 || len(receipt.Pruned) != 1 {
		t.Fatalf("回执不是四类事实齐活: %+v", receipt)
	}
	// 事实数量：ingest + prune + downstream_missing 三条事件；正常任务 + 失败任务两条行；
	// 一条投递窗。少一条就是「取消那次真的吃掉了一部分」。
	if c := countIngressRows(t, e.db, e.nsID, traceID, e.source.ID, deliveryID); c != (rowCounts{events: 3, tasks: 2, deliveries: 1}) {
		t.Fatalf("接收后的事实数量 = %+v, want {3 2 1}", c)
	}
}

// TestAcceptSourceEventMidflightCancelRollsBack 事务中途取消：调用已排队等唯一连接时 cancel，
// 必须从 DB 层返回 context.Canceled（不是「等连接解除后跑完再报错」），0 写且重试可用。
func TestAcceptSourceEventMidflightCancelRollsBack(t *testing.T) {
	db, err := OpenWithConns(filepath.Join(t.TempDir(), "midflight.db"), 1)
	if err != nil {
		t.Fatalf("open pool=1 db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 环境用包内既有 fixture 建（ns + 源 + 能力都走真实入口）。
	nsID, _ := newTestNS(t, db)
	src := &Source{NamespaceID: nsID, Name: "entry", Kind: SourceKindManual, Enabled: true}
	if err := db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	e := &ingressEnv{db: db, nsID: nsID, source: src, cap: v4Cap(t, db, nsID, "midflight-cap", 1)}
	const (
		deliveryID = "dl-midflight"
		traceID    = "tr_midflight"
		eventType  = "push"
	)

	// 占住**唯一**连接并起一个写事务：之后任何入口都会卡在等连接。
	hold, err := db.Begin() // DSN 带 _txlock=immediate → 这里就是 BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("hold the only conn: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if rerr := hold.Rollback(); rerr != nil {
			t.Errorf("release held transaction: %v", rerr)
		}
	}
	defer release()

	before := db.Stats().WaitCount
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outCh := make(chan acceptCall, 1)
	go func() {
		raw, first, cerr := db.AcceptSourceEvent(cctx, e.inputs(deliveryID, traceID, eventType, Now()))
		outCh <- acceptCall{raw: raw, first: first, err: cerr}
	}()

	// 确定性信号：调用进入「等唯一连接」。Timeout 只是轮询上限，判据是 WaitCount。
	pollDeadline := time.Now().Add(5 * time.Second)
	for db.Stats().WaitCount == before {
		if time.Now().After(pollDeadline) {
			cancel()
			release()
			t.Fatalf("5s 内没观察到调用排队等连接（WaitCount 停在 %d）", before)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 中途取消：调用必须**在写事务仍被占住时**就返回。仍挂着 = ctx 没传到事务起点。
	cancel()
	var out acceptCall
	select {
	case out = <-outCh:
	case <-time.After(5 * time.Second):
		release() // 先放锁再收尸，避免把测试进程挂住
		out = <-outCh
		t.Fatalf("cancel 之后调用没有立刻返回（仍在等连接，ctx 没被事务起点采纳）: err=%v", out.err)
	}
	if out.err == nil {
		t.Fatalf("取消后调用成功了（first=%v）", out.first)
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("中途取消必须返回 context.Canceled，得到 %v", out.err)
	}
	if out.first || out.raw != nil {
		t.Fatalf("中途取消不该给出回执: first=%v raw=%s", out.first, out.raw)
	}
	release()

	if c := countIngressRows(t, db, nsID, traceID, e.source.ID, deliveryID); c != (rowCounts{}) {
		t.Fatalf("中途取消留下了写（含投递窗）: %+v", c)
	}

	// fresh ctx 重试同一 deliveryId：取消没有占掉投递窗。
	raw, first, err := db.AcceptSourceEvent(context.Background(), e.inputs(deliveryID, traceID, eventType, Now()))
	if err != nil {
		t.Fatalf("取消后重试: %v", err)
	}
	if !first {
		t.Fatal("取消后第一次成功接收必须 first=true")
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if len(receipt.Tasks) != 1 || len(receipt.FailedTasks) != 1 || len(receipt.Pruned) != 1 {
		t.Fatalf("回执不是四类事实齐活: %+v", receipt)
	}
	if c := countIngressRows(t, db, nsID, traceID, e.source.ID, deliveryID); c != (rowCounts{events: 3, tasks: 2, deliveries: 1}) {
		t.Fatalf("接收后的事实数量 = %+v, want {3 2 1}", c)
	}
}

// TestEmitAuthorizedCanceledContextWritesNothing 已取消的 ctx：emit/计划事实 0 写，emitId 没被吃掉。
func TestEmitAuthorizedCanceledContextWritesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "ctx-emit-node", "tr_emit_ctx")

	tasksBefore, err := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if err != nil {
		t.Fatalf("list tasks before: %v", err)
	}
	eventsBefore, err := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if err != nil {
		t.Fatalf("list events before: %v", err)
	}

	emitID := "emit-ctx"
	// 同一份 body 用两次：一次取消、一次 fresh ctx（幂等键必须还没被吃）。
	body := func() *EmitRecord {
		return &EmitRecord{
			TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"ctx":1}`),
			PlannedPrunes: []EmitPruneFact{
				{Capability: "review", Reason: protocol.ReasonParamMissing, Detail: "需要 diff"},
			},
			PlannedFailures: []EmitFailureFact{
				{Capability: "ghost", Version: 2, Detail: "下游 ghost@2 解析不到"},
			},
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	raw, first, err := db.EmitAuthorized(cctx, auth, body(), []*Task{mkTask(nsID, parent.TraceID, c)})
	if err == nil {
		t.Fatal("已取消的 ctx 仍然 emit 成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消路径的错误必须可判为 context.Canceled，得到 %v", err)
	}
	if first || raw != nil {
		t.Fatalf("取消路径不该给出回执: first=%v raw=%s", first, raw)
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("取消路径写了 emit 行: %v", lerr)
	}
	tasksAfter, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	eventsAfter, _ := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if len(tasksAfter) != len(tasksBefore) || len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("取消路径产生了计划事实: tasks %d→%d events %d→%d",
			len(tasksBefore), len(tasksAfter), len(eventsBefore), len(eventsAfter))
	}

	// fresh ctx 用同一个 emitId 重试：取消没吃掉幂等键，必须 first=true。
	result, first, err := db.EmitAuthorized(ctx, auth, body(), []*Task{mkTask(nsID, parent.TraceID, c)})
	if err != nil {
		t.Fatalf("取消后重试 emit: %v", err)
	}
	if !first {
		t.Fatal("取消后第一次成功 emit 必须 first=true（emitId 没被取消的事务占掉）")
	}
	if len(result) == 0 || !json.Valid(result) {
		t.Fatalf("emit result 非法: %s", result)
	}
	if _, err := db.LookupEmit(ctx, parent.ID, emitID); err != nil {
		t.Fatalf("重试后 emit 行缺失: %v", err)
	}
	// 子任务 1 + 失败任务 1；事件 prune 1 + downstream_missing 1。
	tasksAfter, _ = db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	eventsAfter, _ = db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if len(tasksAfter) != len(tasksBefore)+2 || len(eventsAfter) != len(eventsBefore)+2 {
		t.Fatalf("重试的事实数量: tasks %d→%d events %d→%d",
			len(tasksBefore), len(tasksAfter), len(eventsBefore), len(eventsAfter))
	}
}

// TestCompleteTaskClassifiedAuthorizedCanceledContextWritesNothing 已取消的 ctx：结果列 0 写，
// attempt/lease 仍然可用（fresh ctx 重试能真的落库）。
func TestCompleteTaskClassifiedAuthorizedCanceledContextWritesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, node, task, auth, leaseID := emitFixture(t, db, nsID, "review", "ctx-complete-node", "tr_complete_ctx")
	nodeAuth := v4Auth(t, db, node.ID)

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	disp, err := db.CompleteTaskClassifiedAuthorized(cctx, nodeAuth, task.ID, auth.Attempt, leaseID, Now(), txResult())
	if err == nil {
		t.Fatalf("已取消的 ctx 仍然接受了结果（disposition=%q）", disp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消路径的错误必须可判为 context.Canceled，得到 %v", err)
	}
	if disp != "" {
		t.Fatalf("取消路径不该给 disposition: %q", disp)
	}

	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after cancel: %v", err)
	}
	if got.Phase != TaskRunning {
		t.Fatalf("取消后任务相位 = %q, want running", got.Phase)
	}
	if got.ResultAcceptedAt != nil {
		t.Fatalf("取消后写了 result_accepted_at = %d", *got.ResultAcceptedAt)
	}
	if got.Reported {
		t.Fatal("取消后写了 reported")
	}
	if len(got.ResultJSON) != 0 {
		t.Fatalf("取消后写了 result_json: %s", got.ResultJSON)
	}
	// 半截写也会体现在投影列上：summary/artifacts/exitCode 一条都不许动。
	if got.Summary != "" || len(got.Artifacts) != 0 || got.ExitCode != nil {
		t.Fatalf("取消路径写了结果投影: summary=%q artifacts=%v exitCode=%v", got.Summary, got.Artifacts, got.ExitCode)
	}

	// fresh ctx 重试：取消没吃掉 attempt/lease，结果必须真的被记录。
	disp, err = db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, leaseID, Now(), txResult())
	if err != nil {
		t.Fatalf("取消后重试 complete: %v", err)
	}
	if disp != TaskResultRecorded {
		t.Fatalf("重试 disposition = %q, want %q", disp, TaskResultRecorded)
	}
	got, err = db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after retry: %v", err)
	}
	if got.Phase != TaskSucceeded {
		t.Fatalf("重试后任务相位 = %q, want succeeded", got.Phase)
	}
	if got.ResultAcceptedAt == nil || len(got.ResultJSON) == 0 || !got.Reported {
		t.Fatalf("重试后结果事实不完整: acceptedAt=%v resultJSON=%s reported=%v",
			got.ResultAcceptedAt, got.ResultJSON, got.Reported)
	}
}
