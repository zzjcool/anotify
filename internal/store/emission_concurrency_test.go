package store

// emission_concurrency_test.go 是 R5 的「同 emitId 重试 / 并发调用」覆盖补齐（防线已在
// production 代码里，这里只补可执行的证据）：
//
//	① prune 计划事实写失败 → 整个 emit 事务回滚（emit 行 / 子任务 / 失败任务 / 事件全 0 写）；
//	   故障移除后**同一个 emitId + 同一份 body + 同一份 PlannedPrunes/PlannedFailures**
//	   必须还能 first=true（幂等键没被失败尝试占掉），再调一次 first=false 且回执字节逐字相同；
//	② 同 emitId 的并发调用在两个阶段里各自收敛：
//	   A. 故障在飞（prune 写失败）时两个调用同时起跑 → 两个都失败、0 写（失败的尝试不许
//	      留下「半个派发」）；
//	   B. 故障解除后同样并发起跑 → **恰好一个 first=true、一个 first=false**，回执字节相同，
//	      最终只有一份事实（1 emit 行 / 1 正常子任务 / 1 失败任务 / 1 剪枝事件）。
//
// 	本文件的覆盖边界（如实）：用例库是 :memory:，而 Open 对 ":memory:" 强制池=1
//	（store.go 的 openWithOptions，语义约束：内存库是每连接一份），所以这里的「并发」是
//	**多个调用方同时起跑 + DB 串行执行**，验的是「同 emitId 的并发请求在两个阶段各自
//	收敛」；它**不**覆盖 emission.go 里 `isUniqueViolation → 回读对方 result` 的真实唯一
//	冲突分支。那条分支在当前 DSN 下是防御性代码、没有触发路径：DSN 带 `_txlock=immediate`
//	（store.go 的 dsn 拼接），写锁在事务**起点**取得，两个同 emitId 的事务不可能都通过
//	lookupEmitOn，后到者走 replay 分支——实测（给该分支插日志后重跑 10 次
//	internal/store/v4test 的 TestEmitConcurrentSameIDOnlyOnce + 整个 store 套件）命中 0 次。
//	base 的 TestEmitConcurrentSameIDOnlyOnce（openTestDB = 文件库、池=8、6 个 goroutine 同 id）
//	断言的「恰好一次 first=true、恰好一条 emission、只建一组子任务」仍然成立，但它不经过该分支。
//
// 同步只用 barrier(channel close) + WaitGroup，不用 sleep 当同步。池=1 也顺带消掉了
// SQLITE_BUSY 这类与环境有关的噪音：本文件里**任何**非注入失败的错误都是真回归，一律
// 直接报错并打出具体错误（不 retry、不 continue、不白名单）。
//
// 故障注入只用真实 SQLite trigger（不引入生产测试开关）：RAISE(ABORT) 只回滚当前语句，
// 所以「吞掉错误继续提交」是这条防线真实可能的退化方式——本文件就是钉住它。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// emitCall 是一次 EmitAuthorized 的完整返回（goroutine 收活用）。
type emitCall struct {
	result json.RawMessage
	first  bool
	err    error
}

// traceFacts 是一次 emit 计划在 trace 里留下的可数事实（父任务本身不算）。
type traceFacts struct {
	children int // 父任务之外的正常子任务
	failures int // NULL capability ref 的未解析失败任务
	events   int // trace 里的全部事件行
	prunes   int // param_missing 剪枝观察事件
	missing  int // downstream_missing 观察事件
}

// gatherTraceFacts 用现成只读 API 数一遍 trace 事实（不裸写 SQL，除了 emits 计数）。
func gatherTraceFacts(t *testing.T, db *DB, nsID, traceID, parentID string) traceFacts {
	t.Helper()
	ctx := context.Background()
	var f traceFacts
	tasks, err := db.ListTasksByTrace(ctx, nsID, traceID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for _, task := range tasks {
		// 失败任务（NULL capability ref）与正常子任务是两类事实，分开数。
		if task.CapabilityID == "" {
			f.failures++
			continue
		}
		if task.ID != parentID {
			f.children++
		}
	}
	events, err := db.ListEvents(ctx, nsID, traceID, "", 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	f.events = len(events)
	for _, ev := range events {
		switch ev.Type {
		case protocol.ReasonParamMissing:
			f.prunes++
		case protocol.ReasonDownstreamMissing:
			f.missing++
		}
	}
	return f
}

// countEmitRows 数 (task, emitId) 上的 emit 行（唯一索引的实测面）。
func countEmitRows(t *testing.T, db *DB, taskID, emitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM emits WHERE task_id = ? AND emit_id = ?`, taskID, emitID).Scan(&n); err != nil {
		t.Fatalf("count emits: %v", err)
	}
	return n
}

// concurrentEmit 让 n 个调用方在**同一时刻**（close(start)）对同一个 emitId 起跑。
// 每个调用方各自造一份 body 与子任务对象——真实总线上的并发请求也不共享对象。
func concurrentEmit(db *DB, auth EmitAuth, rec func() *EmitRecord, tasks func() []*Task, n int) []emitCall {
	outs := make([]emitCall, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			raw, first, err := db.EmitAuthorized(context.Background(), auth, rec(), tasks())
			outs[i] = emitCall{result: raw, first: first, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	return outs
}

// TestEmitAuthorizedPruneFailureThenSameEmitIDRetry prune 写失败 → 整事务回滚；同 id 可重试。
func TestEmitAuthorizedPruneFailureThenSameEmitIDRetry(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "prune-retry-node", "tr_emit_prune_retry")

	emitID := "emit-prune-retry"
	// 同一份 body 用三次（失败 / 重试 / 再重试）：emitId、data、计划事实全一致。
	rec := func() *EmitRecord {
		return &EmitRecord{
			TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"a":1}`),
			PlannedPrunes: []EmitPruneFact{
				{Capability: "review", Reason: protocol.ReasonParamMissing, Detail: "需要 diff"},
			},
			PlannedFailures: []EmitFailureFact{
				{Capability: "ghost", Version: 2, Detail: "下游 ghost@2 解析不到"},
			},
		}
	}
	tasks := func() []*Task { return []*Task{mkTask(nsID, parent.TraceID, c)} }

	mustExec(t, db, `CREATE TRIGGER s1_emit_prune_retry_fail BEFORE INSERT ON events
		WHEN NEW.source = 'route' AND NEW.type = 'param_missing'
		BEGIN SELECT RAISE(ABORT, 'injected prune write failure'); END`)

	_, first, err := db.EmitAuthorized(ctx, auth, rec(), tasks())
	if err == nil {
		t.Fatalf("prune 写失败被吞了（first=%v）", first)
	}
	if !strings.Contains(err.Error(), "injected") {
		t.Fatalf("错误不是注入的 prune 失败: %v", err)
	}
	if first {
		t.Fatal("失败路径不能是 first=true")
	}

	// 0 写：emit 行 / 子任务 / 失败任务 / 剪枝与失败观察事件一条都不许留。
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("prune 写失败后 emit 行还在: %v", lerr)
	}
	if n := countEmitRows(t, db, parent.ID, emitID); n != 0 {
		t.Fatalf("prune 写失败后 emit 行数 = %d, want 0", n)
	}
	if f := gatherTraceFacts(t, db, nsID, parent.TraceID, parent.ID); f != (traceFacts{}) {
		t.Fatalf("prune 写失败后留下事实: %+v", f)
	}
	// 失败 Task 与它的 downstream_missing 事件在 prune 之前写：它们必须一起回滚。
	if tasks, terr := db.ListTasksByTrace(ctx, nsID, parent.TraceID); terr != nil || len(tasks) != 1 || tasks[0].ID != parent.ID {
		t.Fatalf("prune 写失败后 trace 里只剩父任务这一行: tasks=%d err=%v", len(tasks), terr)
	}

	// 故障解除：同一个 emitId + 同一份 body 重试必须 first=true —— 失败的尝试没吃掉幂等键。
	mustExec(t, db, `DROP TRIGGER s1_emit_prune_retry_fail`)
	result, first, err := db.EmitAuthorized(ctx, auth, rec(), tasks())
	if err != nil {
		t.Fatalf("故障解除后重试: %v", err)
	}
	if !first {
		t.Fatal("故障解除后第一次成功 emit 必须 first=true")
	}
	if len(result) == 0 || !json.Valid(result) {
		t.Fatalf("emit result 非法: %s", result)
	}
	if n := countEmitRows(t, db, parent.ID, emitID); n != 1 {
		t.Fatalf("重试后 emit 行数 = %d, want 1", n)
	}
	if f := gatherTraceFacts(t, db, nsID, parent.TraceID, parent.ID); f != (traceFacts{children: 1, failures: 1, events: 2, prunes: 1, missing: 1}) {
		t.Fatalf("重试后的事实 = %+v, want {children:1 failures:1 events:2 prunes:1 missing:1}", f)
	}

	// 再调一次同 emitId 同 body：first=false 且**回执字节完全相同**，行数不再增加。
	replayed, dup, err := db.EmitAuthorized(ctx, auth, rec(), tasks())
	if err != nil {
		t.Fatalf("同 id 重放: %v", err)
	}
	if dup {
		t.Fatal("同 id 同 body 重放必须 first=false")
	}
	if string(replayed) != string(result) {
		t.Fatalf("重放回执不是同一份字节:\n first  %s\n replay %s", result, replayed)
	}
	if n := countEmitRows(t, db, parent.ID, emitID); n != 1 {
		t.Fatalf("重放后 emit 行数 = %d, want 1", n)
	}
	if f := gatherTraceFacts(t, db, nsID, parent.TraceID, parent.ID); f != (traceFacts{children: 1, failures: 1, events: 2, prunes: 1, missing: 1}) {
		t.Fatalf("重放产生了附带写: %+v", f)
	}
}

// TestEmitAuthorizedConcurrentSameEmitIDWithPruneFailurePhases 同 emitId 的并发调用在两个
// prune 失败阶段各自收敛：阶段 A（故障在飞）两个调用都失败且 0 写；阶段 B（故障解除）恰好
// 一胜一负、回执字节相同、最终只有一份事实。
//
// 池=1（`:memory:`）→ DB 串行：本用例证的是「并发请求的收敛与幂等」；唯一索引冲突后回读
// 对方 result 的分支由 v4test 的 TestEmitConcurrentSameIDOnlyOnce（文件库、池=8）覆盖。
func TestEmitAuthorizedConcurrentSameEmitIDWithPruneFailurePhases(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "concurrent-node", "tr_emit_concurrent")

	emitID := "emit-concurrent"
	rec := func() *EmitRecord {
		return &EmitRecord{
			TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"a":2}`),
			PlannedPrunes: []EmitPruneFact{
				{Capability: "review", Reason: protocol.ReasonParamMissing, Detail: "需要 diff"},
			},
			PlannedFailures: []EmitFailureFact{
				{Capability: "ghost", Version: 2, Detail: "下游 ghost@2 解析不到"},
			},
		}
	}
	tasks := func() []*Task { return []*Task{mkTask(nsID, parent.TraceID, c)} }

	// ---- 阶段 A：故障在飞，两个调用同时起跑 ----
	mustExec(t, db, `CREATE TRIGGER s1_emit_concurrent_prune_fail BEFORE INSERT ON events
		WHEN NEW.source = 'route' AND NEW.type = 'param_missing'
		BEGIN SELECT RAISE(ABORT, 'injected prune write failure'); END`)

	phaseA := concurrentEmit(db, auth, rec, tasks, 2)
	for i, out := range phaseA {
		if out.err == nil {
			t.Fatalf("阶段 A 第 %d 个调用成功了（first=%v）：注入的 prune 失败没生效", i, out.first)
		}
		if out.first || out.result != nil {
			t.Fatalf("阶段 A 第 %d 个调用返回了回执: first=%v result=%s", i, out.first, out.result)
		}
		// 与阶段 B 同口径：池=1 下不存在 BUSY 来源，任何非注入失败都是真回归 → 直接报错。
		if !strings.Contains(out.err.Error(), "injected") {
			t.Fatalf("阶段 A 第 %d 个调用的错误不是注入的 prune 失败: %v", i, out.err)
		}
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("阶段 A 写了 emit 行: %v", lerr)
	}
	if n := countEmitRows(t, db, parent.ID, emitID); n != 0 {
		t.Fatalf("阶段 A emit 行数 = %d, want 0", n)
	}
	if f := gatherTraceFacts(t, db, nsID, parent.TraceID, parent.ID); f != (traceFacts{}) {
		t.Fatalf("阶段 A 留下了子任务/失败任务/事件: %+v", f)
	}
	// trace 里只剩父任务那一行（两个失败尝试都不许留下「半个派发」）。
	if only, terr := db.ListTasksByTrace(ctx, nsID, parent.TraceID); terr != nil || len(only) != 1 || only[0].ID != parent.ID {
		t.Fatalf("阶段 A 后 trace 里只剩父任务: tasks=%d err=%v", len(only), terr)
	}

	// ---- 阶段 B：故障解除，同样两个调用同时起跑 ----
	mustExec(t, db, `DROP TRIGGER s1_emit_concurrent_prune_fail`)

	phaseB := concurrentEmit(db, auth, rec, tasks, 2)
	var outcomes []emitCall
	var winners, losers int
	var winResult, loseResult json.RawMessage
	for i, out := range phaseB {
		if out.err != nil {
			// 阶段 B 不允许 BUSY 型运气：池=1 + busy_timeout=5000 下它不该出现，出现就是真问题。
			t.Fatalf("阶段 B 第 %d 个调用失败: %v", i, out.err)
		}
		if !json.Valid(out.result) {
			t.Fatalf("阶段 B 第 %d 个调用回执非法: %s", i, out.result)
		}
		if out.first {
			winners++
			winResult = out.result
		} else {
			losers++
			loseResult = out.result
		}
		outcomes = append(outcomes, out)
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("阶段 B 必须恰好一胜一负，得到 first=true %d / first=false %d（%+v）", winners, losers, outcomes)
	}
	if string(winResult) != string(loseResult) {
		t.Fatalf("胜者/败者回执不是同一份字节:\n win  %s\n lose %s", winResult, loseResult)
	}

	// 最终只有一份事实。
	if n := countEmitRows(t, db, parent.ID, emitID); n != 1 {
		t.Fatalf("阶段 B 后 emit 行数 = %d, want 1", n)
	}
	if f := gatherTraceFacts(t, db, nsID, parent.TraceID, parent.ID); f != (traceFacts{children: 1, failures: 1, events: 2, prunes: 1, missing: 1}) {
		t.Fatalf("阶段 B 后的事实 = %+v, want {children:1 failures:1 events:2 prunes:1 missing:1}", f)
	}
	stored, err := db.LookupEmit(ctx, parent.ID, emitID)
	if err != nil {
		t.Fatalf("阶段 B 后 emit 行缺失: %v", err)
	}
	if string(stored.Result) != string(winResult) {
		t.Fatalf("落库回执与返回回执不同:\n stored %s\n return %s", stored.Result, winResult)
	}
}
