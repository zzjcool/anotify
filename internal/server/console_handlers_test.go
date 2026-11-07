package server

// #219 修复后的两条「接线缺口」在这里收口（都不是新功能，而是漏接的一条路径）：
//
//	A. 取消任务也是「槽位释放」，但它此前没接巡检的 Nudge → 腾出的槽位最坏空转 5s；
//	B. 容量不足（store.ErrNoCapacity）此前被当成「没有节点」写 RetryBackoff（10s 起），
//	   而 ListRedispatchable 按 not_before 过滤 → 槽位一空那次的即时重派看不见这条任务。
//
// 为什么单独一个文件：这两条测的都是**装配**（handler 的 nudge 钩子 / mux 的
// redispatchHook），夹具要能注入 nudge、自建 dispatcher，与 console_test.go 的
// 通用夹具不同。两边共用同一套 helper（consoleEnv.do / seedCapability / closeDBForTempDir）。

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// newCancelNudgeEnv 建一个只挂 `POST /v1/tasks/{id}/cancel` 的控制台环境，
// 并把 consoleHandler.nudge 接到一个计数器上（生产接线见 mux.go 的 consoleH.nudge）。
//
// 与 newConsoleEnv 的唯一差别就是那个 nudge：不注入它，本文件要断言的
// 「取消成功 → 通知巡检」根本无从观察（nil = 未装配，走的是老路径）。
func newCancelNudgeEnv(t *testing.T) (*consoleEnv, *int32) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(dir + "/cancel_nudge.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	uid, err := db.InsertUser(ctx, "", "canceluser", "Cancel User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defNS, err := db.CreateNamespace(ctx, "default", "默认")
	if err != nil {
		t.Fatalf("create default namespace: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · cancel nudge")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	var nudges int32
	h := &consoleHandler{db: db, auth: svc, nudge: func() { atomic.AddInt32(&nudges, 1) }}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/tasks/{id}/cancel",
		noStore(svc.Sessions().Middleware(http.HandlerFunc(h.cancelTask))))

	return &consoleEnv{
		mux: mux, db: db, svc: svc, userID: uid, nsID: defNS.ID,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}, &nudges
}

// seedPendingTask 建一条 pending 任务（能力只是行上的一个字段，取消不校验它）。
func seedPendingTask(t *testing.T, db *store.DB, nsID, capID, capName, traceID string) *store.Task {
	t.Helper()
	task := &store.Task{
		NamespaceID: nsID, TraceID: traceID, CapabilityID: capID,
		Capability: capName, Version: 1,
	}
	if err := db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// TestCancelTaskNudgesSweeper 取消成功必须通知巡检（#219 缺口 A）；失败一律不通知。
//
// 为什么「通知」必须被断言：store.CancelTask 在终态落库后 ReleaseInstanceByTask，
// 但槽位释放这件事只有 handler 知道该告诉谁。漏接的症状是**没有报错、没有日志**，
// 只是那台机器空着一个槽等到下一轮巡检（最坏 5s）。
func TestCancelTaskNudgesSweeper(t *testing.T) {
	env, nudges := newCancelNudgeEnv(t)
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})
	task := seedPendingTask(t, env.db, env.nsID, capDef.ID, capDef.Name, "trace_cancel")

	// 成功：取消 pending 任务 → 200 且巡检被叫醒**一次**。
	rr := env.do(t, http.MethodPost, env.withNS("/v1/tasks/"+task.ID+"/cancel"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(nudges); got != 1 {
		t.Fatalf("nudge count after successful cancel = %d, want 1", got)
	}

	// 失败 ①：已终态 → 409，槽位没有被释放，不该叫醒巡检。
	rr = env.do(t, http.MethodPost, env.withNS("/v1/tasks/"+task.ID+"/cancel"), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second cancel status = %d, want 409, body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(nudges); got != 1 {
		t.Fatalf("nudge count after 409 = %d, want 1 (失败分支不能通知)", got)
	}

	// 失败 ②：任务不存在 → 404，同样不通知。
	rr = env.do(t, http.MethodPost, env.withNS("/v1/tasks/tsk_nope/cancel"), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(nudges); got != 1 {
		t.Fatalf("nudge count after 404 = %d, want 1 (失败分支不能通知)", got)
	}
}

// TestCancelRunningTaskFreesSlotAndNudges 取消一条**占着槽位**的任务：
// 槽位真的被释放了，并且巡检被叫醒（这才是 #219 说的「最坏 5s 空槽」那个场景）。
func TestCancelRunningTaskFreesSlotAndNudges(t *testing.T) {
	env, nudges := newCancelNudgeEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})
	task := seedPendingTask(t, env.db, env.nsID, capDef.ID, capDef.Name, "trace_running")

	node := &store.Node{NamespaceID: env.nsID, Name: "mbp-cancel"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// 只有一个槽位：取消后它必须重新空闲，否则就是槽位泄漏。
	if _, err := env.db.UpsertInstances(ctx, env.nsID, node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("declare instances: %v", err)
	}
	if _, err := env.db.LeaseAndAssign(ctx, env.nsID, task.ID, node.ID, store.Now()+60); err != nil {
		t.Fatalf("lease+assign: %v", err)
	}
	if busy := busySlots(t, env.db, node.ID); busy != 1 {
		t.Fatalf("busy slots before cancel = %d, want 1", busy)
	}

	rr := env.do(t, http.MethodPost, env.withNS("/v1/tasks/"+task.ID+"/cancel"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if busy := busySlots(t, env.db, node.ID); busy != 0 {
		t.Fatalf("busy slots after cancel = %d, want 0（槽位没释放）", busy)
	}
	if got := atomic.LoadInt32(nudges); got != 1 {
		t.Fatalf("nudge count after cancel = %d, want 1（释放的槽位必须立刻被重派用到）", got)
	}
}

// busySlots 数一台节点当前被占的槽位数（读真库，不信内存状态）。
func busySlots(t *testing.T, db *store.DB, nodeID string) int {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	busy := 0
	for _, in := range insts {
		if in.BusyTask != nil {
			busy++
		}
	}
	return busy
}

// newRedispatchFixture 建真库里的一套「可重派」场景：
// 一条 pending 任务（Attempts=1，= 已被重派过一次，退避会落在 10s 那一档）
// + 一台声明了该能力、**只有 1 个槽位**的节点；返回时那个槽位已被别人占满。
//
// 为什么必须是真库：要断言的东西正是库里的 not_before（闭包的副作用），
// 以及「下一轮 ListRedispatchable 能不能捞到它」——这两条都在 store 的 SQL 里。
func newRedispatchFixture(t *testing.T) (db *store.DB, nsID, nodeID string, task *store.Task) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(dir + "/redispatch.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	defNS, err := db.CreateNamespace(ctx, "default", "默认")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	nsID = defNS.ID

	capDef := &store.CapabilityVersion{
		NamespaceID:  nsID,
		Name:         "work",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{},
		Source:       store.CapSourceBuiltin,
	}
	if err := db.CreateCapability(ctx, withSpec(capDef)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	node := &store.Node{NamespaceID: nsID, Name: "mbp-full"}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	nodeID = node.ID
	if _, err := db.UpsertInstances(ctx, nsID, nodeID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("declare instances: %v", err)
	}
	// 「节点声明了这个能力」= node_capabilities 里有一行。生产里它只从部署派生（契约 D4），
	// ConfirmCapabilities 是 store 给 devseed / 测试留的同一语义入口（全量替换）。
	if _, err := db.ConfirmCapabilities(ctx, nsID, nodeID,
		[]store.CapabilityRef{{Name: capDef.Name, Version: capDef.Version}}); err != nil {
		t.Fatalf("declare node capability: %v", err)
	}

	task = &store.Task{
		NamespaceID: nsID, TraceID: "trace_redis", CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1,
		// Attempts=1：派发时就会 +1，所以任何「被重派过一次」的任务都在这一档，
		// 退避 = RetryBackoff(1) = 10s（= 缺口 B 的实际症状）。
		Attempts: 1,
	}
	if err := db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 把唯一的槽位占满：此刻 LeaseAndAssign 必然返回 store.ErrNoCapacity。
	if _, err := db.LeaseInstance(ctx, nodeID, "tsk_someone_else"); err != nil {
		t.Fatalf("occupy the only slot: %v", err)
	}
	return db, nsID, nodeID, task
}

// isRedispatchable 问「下一轮巡检（ListRedispatchable）能不能看见这条任务」。
func isRedispatchable(t *testing.T, db *store.DB, taskID string) bool {
	t.Helper()
	got, err := db.ListRedispatchable(context.Background(), store.Now(), 100)
	if err != nil {
		t.Fatalf("list redispatchable: %v", err)
	}
	for _, task := range got {
		if task.ID == taskID {
			return true
		}
	}
	return false
}

// TestRedispatchHookNoBackoffOnCapacityShortage 容量不足**不写退避**（#219 缺口 B）。
//
// 这是缺口 B 的精确回归：有节点、节点也声明了能力，只是槽位一时占满。
// 修复前 RedispatchTask 返回 (false, nil)（与「没有节点」同形）→ 闭包写
// not_before = now+10s → ListRedispatchable 过滤掉它 → 节点在 5s 内空出槽位并
// Nudge 时，这条任务**不在候选里**，白等 10s 起。
func TestRedispatchHookNoBackoffOnCapacityShortage(t *testing.T) {
	db, nsID, nodeID, task := newRedispatchFixture(t)
	ctx := context.Background()

	hook := redispatchHook(db, route.New(db))
	ok, err := hook(ctx, task)
	if err != nil {
		t.Fatalf("容量不足被当成错误返回: %v（应当吞成 false,nil）", err)
	}
	if ok {
		t.Fatal("槽位占满却报「派出去了」")
	}

	// ① 库里不能有退避：not_before 必须还是 NULL。
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.NotBefore != nil {
		t.Fatalf("容量不足写了退避 not_before=%d（now=%d）：槽位一空那次 Nudge 会看不见这条任务",
			*got.NotBefore, store.Now())
	}
	if got.Status != store.TaskPending || got.NodeID != nil {
		t.Fatalf("失败的重派改了任务：status=%q node=%v", got.Status, got.NodeID)
	}
	// 占满的那个槽位不受影响（LeaseAndAssign 是事务，失败什么都不写）。
	if busy := busySlots(t, db, nodeID); busy != 1 {
		t.Fatalf("busy slots after failed redispatch = %d, want 1", busy)
	}

	// ② 下一轮巡检立刻能捞到它（= 槽位一空就能马上派出去）。
	if !isRedispatchable(t, db, task.ID) {
		t.Fatal("容量不足后任务从 ListRedispatchable 里消失了（被 not_before 挡在门外）")
	}

	// ③ 槽位真空出来之后，同一个 hook 立刻能派成功——证明不写退避确实换来了即时重派。
	if err := db.ReleaseInstanceByTask(ctx, nodeID, "tsk_someone_else"); err != nil {
		t.Fatalf("release slot: %v", err)
	}
	if busy := busySlots(t, db, nodeID); busy != 0 {
		t.Fatalf("busy slots after release = %d, want 0", busy)
	}
	ok, err = hook(ctx, task)
	if err != nil {
		t.Fatalf("槽位空出来后的重派失败: %v", err)
	}
	if !ok {
		t.Fatal("槽位已空却仍然没派出去")
	}
	assigned, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if assigned.Status != store.TaskAssigned {
		t.Fatalf("status = %q, want assigned", assigned.Status)
	}
}

// TestRedispatchHookBacksOffWhenNoNodeDeclaresCapability 对照用例：「没有节点」仍然退避。
//
// 这是缺口 B 修复的**边界**：不设退避只针对容量不足。真没节点（或节点全下线）时
// 退避必须保留，否则每 5s 空转重扫、日志刷屏（契约 §3.3 的 10s/60s/300s）。
func TestRedispatchHookBacksOffWhenNoNodeDeclaresCapability(t *testing.T) {
	db, nsID, nodeID, task := newRedispatchFixture(t)
	ctx := context.Background()

	// 让「这台节点不再声明该能力」→ NodesForCapability 返回空。
	if _, err := db.ConfirmCapabilities(ctx, nsID, nodeID, nil); err != nil {
		t.Fatalf("clear node capabilities: %v", err)
	}

	hook := redispatchHook(db, route.New(db))
	ok, err := hook(ctx, task)
	if ok || err != nil {
		t.Fatalf("redispatch = (%v, %v), want (false, nil)", ok, err)
	}

	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.NotBefore == nil {
		t.Fatal("没有节点时反而没写退避（每 5s 空转重扫）")
	}
	// attempts=1 → RetryBackoff(1) = 10s（契约 §3.3）。给它一点余量，只钉量级。
	if wait := *got.NotBefore - store.Now(); wait < 5 || wait > 15 {
		t.Fatalf("退避 = %ds, want ≈%ds（RetryBackoff(1)）", wait, store.RetryBackoff(1))
	}
	if isRedispatchable(t, db, task.ID) {
		t.Fatal("退避中的任务不该出现在 ListRedispatchable 里")
	}
}
