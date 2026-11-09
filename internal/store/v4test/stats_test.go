package v4test

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestStatsByPhase 验按相位的计数（六个相位恒出现，缺失计 0）、按能力/节点。
func TestStatsByPhase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "stats")
	analyze := createCap(t, db, testCap(ns, "analyze", 1, nil))
	notify := createCap(t, db, testCap(ns, "notify", 1, nil))
	node := newNode(t, db, "node-a", 3)
	now := store.Now()

	// 1 条 pending、1 条 running、1 条 succeeded。
	newTask(t, db, ns, notify, "t-pending", "")
	running := newTask(t, db, ns, analyze, "t-running", "")
	grant(t, db, running.ID, node.ID, "asg-running")
	finishedTask(t, db, ns, analyze, node.ID, "t-done", now-1)

	stats, err := db.TaskStats(ctx, ns)
	if err != nil {
		t.Fatalf("task stats: %v", err)
	}
	if len(stats.ByPhase) != 6 {
		t.Fatalf("六个相位必须全出现（前端不必补零），得到 %d", len(stats.ByPhase))
	}
	byPhase := map[string]int{}
	for _, pc := range stats.ByPhase {
		byPhase[pc.Phase] = pc.Count
	}
	want := map[string]int{
		store.TaskPending: 1, store.TaskRunning: 1, store.TaskSucceeded: 1,
		store.TaskFailed: 0, store.TaskCancelled: 0, store.TaskAbandoned: 0,
	}
	for phase, n := range want {
		if byPhase[phase] != n {
			t.Fatalf("相位 %s 计数 = %d, want %d", phase, byPhase[phase], n)
		}
	}
	if len(stats.ByCapability) != 2 {
		t.Fatalf("按能力应有两行：%+v", stats.ByCapability)
	}
	if stats.ByCapability[0].Ref != "analyze" || stats.ByCapability[0].Count != 2 {
		t.Fatalf("analyze 应有 2 条任务且排在前：%+v", stats.ByCapability)
	}
	if len(stats.ByNode) != 1 || stats.ByNode[0].Ref != "node-a" || stats.ByNode[0].Count != 2 {
		t.Fatalf("按节点统计错：%+v", stats.ByNode)
	}
	// 未派发的任务没有 node_name，不该出现在按节点统计里。
	if stats.ByNode[0].Ref == "" {
		t.Fatalf("空 node_name 不该进按节点统计")
	}
}

// TestStatsEmptyAndWorkspace 验空库统计形状（[] 而不是 null）与工作台口径。
func TestStatsEmptyAndWorkspace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "stats-empty")
	user := &store.User{Username: "stats-user", DisplayName: "Stats User", Role: store.RoleAdmin}
	if err := db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	stats, err := db.TaskStats(ctx, ns)
	if err != nil {
		t.Fatalf("task stats: %v", err)
	}
	if stats.ByCapability == nil || stats.ByNode == nil || len(stats.ByCapability) != 0 || len(stats.ByNode) != 0 {
		t.Fatalf("空库必须是 []：%+v", stats)
	}
	for _, pc := range stats.ByPhase {
		if pc.Count != 0 {
			t.Fatalf("空库相位计数应为 0：%+v", pc)
		}
	}

	ws, err := db.MessageStats(ctx, ns, user.ID, store.Now()-86400)
	if err != nil {
		t.Fatalf("message stats: %v", err)
	}
	if ws.Total != 0 || ws.Today != 0 || ws.Delivered != 0 || ws.DeliveryTotal != 0 {
		t.Fatalf("空库工作台统计应全 0：%+v", ws)
	}
	if ws.Daily == nil {
		t.Fatalf("Daily 必须是 []")
	}

	// 一条事件 + 一条成功任务 → 计数动起来。
	if ok, err := db.InsertEvent(ctx, evt(ns, "trace-stats", "hook", ""), store.Now(), 0); err != nil || !ok {
		t.Fatalf("insert event: %v %v", ok, err)
	}
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	finishedTask(t, db, ns, c, node.ID, "trace-done", store.Now()-1)

	ws, err = db.MessageStats(ctx, ns, user.ID, store.Now()-86400)
	if err != nil {
		t.Fatalf("message stats after: %v", err)
	}
	if ws.Total != 1 || ws.Delivered != 1 || ws.DeliveryTotal != 1 {
		t.Fatalf("工作台统计口径错：%+v", ws)
	}
	if len(ws.Daily) != 1 {
		t.Fatalf("热力图应有一个日桶：%+v", ws.Daily)
	}
}
