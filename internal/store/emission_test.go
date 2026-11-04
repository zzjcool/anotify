package store

import (
	"context"
	"encoding/json"
	"errors"
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

// TestEmissionSeqStatus 缺口判定用的 (条数, 最大 seq)，**只看 seq < before**。
//
// 那个界是并发正确性的一部分：整表计数会把「并发重发同一个 seq」判成缺口
// （route 的并发用例抓到过，报 "missing seq 2..0"）。
func TestEmissionSeqStatus(t *testing.T) {
	db, _, task := emissionFixture(t)
	ctx := context.Background()

	count, maxSeq, err := db.EmissionSeqBefore(ctx, task.ID, 1)
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
	count, maxSeq, err = db.EmissionSeqBefore(ctx, task.ID, 4)
	if err != nil {
		t.Fatalf("seq status: %v", err)
	}
	if count != 3 || maxSeq != 3 {
		t.Fatalf("status = (%d,%d), want (3,3)", count, maxSeq)
	}
	// 界生效：seq 1..3 落库后，判「4 之前是否齐全」要看到 3 条；
	// 判「2 之前」只该看到 1 条（seq=1）——库里躺着 seq≥2 的行不影响这个判定。
	count, maxSeq, err = db.EmissionSeqBefore(ctx, task.ID, 2)
	if err != nil {
		t.Fatalf("seq status before 2: %v", err)
	}
	if count != 1 || maxSeq != 1 {
		t.Fatalf("status(before 2) = (%d,%d), want (1,1)", count, maxSeq)
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

// TestTaskTokenColumnMigrated tasks.token 列存在且默认空串（baseline 结构门禁）。
func TestTaskTokenColumnMigrated(t *testing.T) {
	db, _, task := emissionFixture(t)
	if !columnNames(t, db, "tasks")["token"] {
		t.Fatal("tasks 表没有 token 列（baseline 漏了）")
	}
	var tok string
	if err := db.QueryRow(`SELECT token FROM tasks WHERE id = ?`, task.ID).Scan(&tok); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if tok != "" {
		t.Fatalf("新任务的 token = %q, want 空串（首次发布 assign 时才生成）", tok)
	}
}

// TestEmissionsTableMigrated emissions 表与它的列存在（baseline 结构门禁）。
func TestEmissionsTableMigrated(t *testing.T) {
	db, _, _ := emissionFixture(t)
	if !tableExists(t, db, "emissions") {
		t.Fatal("emissions 表不存在（baseline 漏了）")
	}
	cols := columnNames(t, db, "emissions")
	for _, want := range []string{"task_id", "seq", "port", "data", "routed", "pruned", "created_at"} {
		if !cols[want] {
			t.Errorf("emissions 缺列 %q", want)
		}
	}
}
