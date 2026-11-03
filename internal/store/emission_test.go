package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// 本文件覆盖 P1.1 的 emissions 表与 P1.2 的任务令牌（store 层语义）。
//
// 路由层的用例（route/emit_test.go）验的是「怎么用」，这里验的是**存储语义本身**：
// 幂等键、往返一致、缺口判定、令牌只生成一次。两者分开的理由是：任何一个出问题
// 时的排查方向完全不同（一个在判定，一个在 SQL）。

// emissionFixture 建库 + 命名空间 + 一条上游任务行（emissions 有外键指向它）。
func emissionFixture(t *testing.T) (*DB, string, *Task) {
	t.Helper()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)
	ctx := context.Background()

	capDef := &CapabilityVersion{
		NamespaceID:  ns,
		Name:         "review",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"},
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	task := &Task{
		NamespaceID:  ns,
		TraceID:      NewTaskID(),
		Path:         []string{capDef.ID},
		CapabilityID: capDef.ID,
		Capability:   capDef.Name,
		Version:      capDef.Version,
		Params:       json.RawMessage(`{}`),
		CapHash:      capDef.ContentHash,
		GraphID:      "grf_test",
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return db, ns, task
}

// TestEmissionRoundTrip 存什么读什么（新增字段必须验往返一致性）。
func TestEmissionRoundTrip(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	in := &Emission{
		TaskID: task.ID,
		Seq:    3,
		Port:   "fix",
		Data:   json.RawMessage(`{"issue":"#42","n":1}`),
		Routed: 2,
		Pruned: 1,
	}
	first, err := db.InsertEmission(ctx, in)
	if err != nil {
		t.Fatalf("insert emission: %v", err)
	}
	if !first {
		t.Fatal("首次插入返回 first=false")
	}
	if in.CreatedAt == 0 {
		t.Error("插入后没有回填 CreatedAt")
	}

	got, err := db.GetEmission(ctx, task.ID, 3)
	if err != nil {
		t.Fatalf("get emission: %v", err)
	}
	if got.TaskID != task.ID || got.Seq != 3 || got.Port != "fix" {
		t.Errorf("往返后身份字段漂移：%+v", got)
	}
	if string(got.Data) != string(in.Data) {
		t.Errorf("data 往返后变了：%s → %s（审计要的是当时的字节）", in.Data, got.Data)
	}
	if got.Routed != 2 || got.Pruned != 1 {
		t.Errorf("计数漂移：routed=%d pruned=%d, want 2/1", got.Routed, got.Pruned)
	}
	if got.CreatedAt != in.CreatedAt {
		t.Errorf("created_at 漂移：%d → %d", in.CreatedAt, got.CreatedAt)
	}

	// 缺省 data 归一成 {}（前端/审计不用区分 null 与空对象）。
	if _, err := db.InsertEmission(ctx, &Emission{TaskID: task.ID, Seq: 4, Port: "fix"}); err != nil {
		t.Fatalf("insert emission without data: %v", err)
	}
	empty, err := db.GetEmission(ctx, task.ID, 4)
	if err != nil {
		t.Fatalf("get emission 4: %v", err)
	}
	if string(empty.Data) != "{}" {
		t.Errorf("缺省 data = %s, want {}", empty.Data)
	}
}

// TestInsertEmissionIdempotent 重复 (task_id, seq) 什么都不写（首次结果不被覆盖）。
func TestInsertEmissionIdempotent(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	if _, err := db.InsertEmission(ctx, &Emission{
		TaskID: task.ID, Seq: 1, Port: "fix", Data: json.RawMessage(`{"a":1}`), Routed: 1,
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	again, err := db.InsertEmission(ctx, &Emission{
		TaskID: task.ID, Seq: 1, Port: "push", Data: json.RawMessage(`{"b":2}`), Routed: 5, Pruned: 5,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if again {
		t.Fatal("重复插入返回 first=true（幂等键没生效）")
	}
	got, err := db.GetEmission(ctx, task.ID, 1)
	if err != nil {
		t.Fatalf("get emission: %v", err)
	}
	if got.Port != "fix" || got.Routed != 1 || string(got.Data) != `{"a":1}` {
		t.Fatalf("首次结果被覆盖：%+v", got)
	}
	if list, _ := db.ListEmissions(ctx, task.ID); len(list) != 1 {
		t.Fatalf("emissions 行数 = %d, want 1", len(list))
	}
}

// TestEmissionSeqStatus 缺口判定用的 (条数, 最大 seq)。
func TestEmissionSeqStatus(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	count, maxSeq, err := db.EmissionSeqStatus(ctx, task.ID)
	if err != nil {
		t.Fatalf("seq status: %v", err)
	}
	if count != 0 || maxSeq != 0 {
		t.Fatalf("空表 status = (%d,%d), want (0,0)", count, maxSeq)
	}

	for seq := int64(1); seq <= 3; seq++ {
		if _, err := db.InsertEmission(ctx, &Emission{TaskID: task.ID, Seq: seq, Port: "fix"}); err != nil {
			t.Fatalf("insert seq %d: %v", seq, err)
		}
	}
	count, maxSeq, err = db.EmissionSeqStatus(ctx, task.ID)
	if err != nil {
		t.Fatalf("seq status: %v", err)
	}
	if count != 3 || maxSeq != 3 {
		t.Fatalf("status = (%d,%d), want (3,3)", count, maxSeq)
	}
}

// TestEmissionListOrderedAndScoped 按 seq 升序、只含本任务、空列表是 []。
func TestEmissionListOrderedAndScoped(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	if list, err := db.ListEmissions(ctx, task.ID); err != nil {
		t.Fatalf("list emissions: %v", err)
	} else if list == nil || len(list) != 0 {
		t.Fatalf("空列表 = %#v, want 空切片（不是 nil）", list)
	}

	// 乱序插入，读出来必须是 1,2,3。
	for _, seq := range []int64{3, 1, 2} {
		if _, err := db.InsertEmission(ctx, &Emission{TaskID: task.ID, Seq: seq, Port: "fix"}); err != nil {
			t.Fatalf("insert seq %d: %v", seq, err)
		}
	}
	list, err := db.ListEmissions(ctx, task.ID)
	if err != nil {
		t.Fatalf("list emissions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("行数 = %d, want 3", len(list))
	}
	for i, e := range list {
		if e.Seq != int64(i+1) {
			t.Fatalf("第 %d 行 seq = %d, want %d（必须升序）", i, e.Seq, i+1)
		}
	}

	// 别的任务的行不能混进来。
	other := &Task{
		NamespaceID: task.NamespaceID, TraceID: NewTaskID(), Path: []string{},
		CapabilityID: task.CapabilityID, Capability: task.Capability, Version: 1,
		Params: json.RawMessage(`{}`), CapHash: "h", GraphID: "g",
	}
	if err := db.CreateTasks(ctx, []*Task{other}); err != nil {
		t.Fatalf("create other task: %v", err)
	}
	if list, err := db.ListEmissions(ctx, other.ID); err != nil || len(list) != 0 {
		t.Fatalf("别的任务能读到 %d 条 emission（err=%v）", len(list), err)
	}
}

// TestGetEmissionNotFound 未命中返回 ErrNotFound（不是 nil, nil）。
func TestGetEmissionNotFound(t *testing.T) {
	db, _, task := emissionFixture(t)
	if _, err := db.GetEmission(context.Background(), task.ID, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(err, ErrNotFound) = false, err = %v", err)
	}
}

// TestInsertEmissionRejectsBadInput 非法输入直接拒（失败在 SQL 之前）。
func TestInsertEmissionRejectsBadInput(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		e    *Emission
	}{
		{"nil", nil},
		{"空 task_id", &Emission{Seq: 1}},
		{"seq=0", &Emission{TaskID: task.ID}},
		{"seq 负数", &Emission{TaskID: task.ID, Seq: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := db.InsertEmission(ctx, c.e); err == nil {
				t.Fatal("非法输入被接受了")
			}
			if _, err := db.InsertEmissionAndCreateTasks(ctx, c.e, nil); err == nil {
				t.Fatal("非法输入被 InsertEmissionAndCreateTasks 接受了")
			}
		})
	}
}

// TestEnsureTaskToken 令牌只生成一次，之后每次读都是同一个值。
func TestEnsureTaskToken(t *testing.T) {
	db, ns, task := emissionFixture(t)
	ctx := context.Background()

	first := NewTaskToken()
	got, err := db.EnsureTaskToken(ctx, ns, task.ID, first)
	if err != nil {
		t.Fatalf("ensure token: %v", err)
	}
	if got != first {
		t.Fatalf("首次 ensure token = %q, want %q（刚写进去的那个）", got, first)
	}

	// 第二次带另一个 candidate：必须返回**库里那份**（断线重连重发 assign 的形状）。
	again, err := db.EnsureTaskToken(ctx, ns, task.ID, NewTaskToken())
	if err != nil {
		t.Fatalf("ensure token again: %v", err)
	}
	if again != first {
		t.Fatalf("第二次 ensure 返回 %q, want %q（令牌不能每次重发都变）", again, first)
	}

	// 令牌格式：tok_ 前缀 + 足够长的随机串。
	if len(first) < 20 || first[:4] != "tok_" {
		t.Fatalf("token = %q, want tok_<随机> 形状", first)
	}

	// 未知任务 → ErrNotFound（调用方据此判「任务已没了」）。
	if _, err := db.EnsureTaskToken(ctx, ns, "tsk_nope", NewTaskToken()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知任务 ensure token 返回 %v, want ErrNotFound", err)
	}
	// 缺命名空间 → ErrNamespaceRequired。
	if _, err := db.EnsureTaskToken(ctx, "", task.ID, NewTaskToken()); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("空命名空间返回 %v, want ErrNamespaceRequired", err)
	}
}

// TestEnsureTaskTokenConcurrent 并发派发同一条任务时，令牌必须收敛成一个值。
//
// 这就是 CAS（`WHERE token` 为空串）存在的理由：没有它，「两条连接同时推同一条
// 任务」会让两边拿到不同令牌，节点侧一半的 emit 被拒成 token 无效。
func TestEnsureTaskTokenConcurrent(t *testing.T) {
	db, ns, task := emissionFixture(t)
	ctx := context.Background()

	const workers = 8
	results := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := db.EnsureTaskToken(ctx, ns, task.ID, NewTaskToken())
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			results[i] = tok
		}(i)
	}
	wg.Wait()

	want := results[0]
	if want == "" {
		t.Fatal("没有任何 worker 拿到令牌")
	}
	for i, got := range results {
		if got != want {
			t.Fatalf("worker %d 拿到 %q，其他拿到 %q（令牌必须是同一个）", i, got, want)
		}
	}
}

// TestTaskTokenColumnMigrated 迁移 23：tasks.token 列存在且默认空串。
func TestTaskTokenColumnMigrated(t *testing.T) {
	db, _, task := emissionFixture(t)
	if !columnNames(t, db, "tasks")["token"] {
		t.Fatal("tasks 表没有 token 列（迁移 23 没跑）")
	}
	var tok string
	if err := db.QueryRow(`SELECT token FROM tasks WHERE id = ?`, task.ID).Scan(&tok); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if tok != "" {
		t.Fatalf("新任务的 token = %q, want 空串（首次发布 assign 时才生成）", tok)
	}
}

// TestEmissionsTableMigrated 迁移 22：emissions 表与它的唯一键存在。
func TestEmissionsTableMigrated(t *testing.T) {
	db, _, _ := emissionFixture(t)
	if !tableExists(t, db, "emissions") {
		t.Fatal("emissions 表不存在（迁移 22 没跑）")
	}
	cols := columnNames(t, db, "emissions")
	for _, want := range []string{"task_id", "seq", "port", "data", "routed", "pruned", "created_at"} {
		if !cols[want] {
			t.Errorf("emissions 缺列 %q", want)
		}
	}
}

// TestMigration22And23Upgrade 老库（v21）升级：emissions 表建出来、tasks.token 补上，
// 且老任务行一行不丢（补列是幂等 ALTER，不改任何既有数据）。
//
// 为什么这条必须有：新库路径（CREATE TABLE 里就带 token）与升级路径是两条代码路径，
// 只有升级路径会真的跑迁移 22/23 的 SQL/hook。缺了它，「老库起来了但 emit 全挂」
// （没有 emissions 表）或「发布 assign 时报 no such column: token」就只能在线上发现。
func TestMigration22And23Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v21.db")
	db21 := openAtVersion(t, path, 21)
	if v := mustMaxVersion(t, db21); v != 21 {
		t.Fatalf("夹具版本 = %d, want 21", v)
	}
	// 前提证明：v21 库上这两样都还不存在。
	if tableExists(t, db21, "emissions") {
		t.Fatal("v21 夹具里已经有 emissions 表了，夹具没停在该停的位置")
	}
	if columnNames(t, db21, "tasks")["token"] {
		t.Fatal("v21 夹具里 tasks 已经有 token 列了，夹具没停在该停的位置")
	}

	// 造一条 v21 的任务行（走 SQL：此刻的任务列集合里还没有 token）。
	nsID := "ns_m23"
	mustExec(t, db21, `INSERT INTO namespaces (id, slug, name, created_at) VALUES (?, 'legacy-m23', 'legacy', 1000)`, nsID)
	const insCap = `INSERT INTO capability_versions
		(id, namespace_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
		VALUES (?, ?, ?, '', 1, '{}', '["fix"]', '{}', '', 'custom', ?, 1000)`
	mustExec(t, db21, insCap, "cap_m23", nsID, "review", "hash_m23")
	const insTask = `INSERT INTO tasks
		(id, namespace_id, trace_id, path, capability_id, capability, version, params, status,
		 cap_hash, graph_id, created_at)
		VALUES (?, ?, 'trc_m23', '["cap_m23"]', 'cap_m23', 'review', 1, '{}', 'pending',
		 'hash_m23', 'grf_m23', 1000)`
	mustExec(t, db21, insTask, "tsk_m23", nsID)
	if err := db21.Close(); err != nil {
		t.Fatalf("close v21 db: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v21 库: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	if v, want := mustMaxVersion(t, upgraded), testTipVersion(t); v != want {
		t.Fatalf("升级后版本 = %d, want %d", v, want)
	}

	if !tableExists(t, upgraded, "emissions") {
		t.Fatal("升级后仍然没有 emissions 表")
	}
	if !columnNames(t, upgraded, "tasks")["token"] {
		t.Fatal("升级后 tasks 仍然没有 token 列")
	}
	// 老任务行还在，且 token 是空串（= 尚未生成，与加列之前的语义一致）。
	var token string
	if err := upgraded.QueryRow(`SELECT token FROM tasks WHERE id = 'tsk_m23'`).Scan(&token); err != nil {
		t.Fatalf("读升级后的老任务行: %v", err)
	}
	if token != "" {
		t.Fatalf("老任务的 token = %q, want 空串", token)
	}
	// 升级后的库能正常写 emission（外键指向那条老任务行，也顺带验了外键可用）。
	if _, err := upgraded.InsertEmission(context.Background(), &Emission{
		TaskID: "tsk_m23", Seq: 1, Port: "fix",
	}); err != nil {
		t.Fatalf("升级后写 emission: %v", err)
	}
}
