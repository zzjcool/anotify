package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 本文件钉住 status.observed 的**事实形状**与**查询计划**两件事。
//
// 为什么直插 SQL 而不是走 CreateTasks / EmitAuthorized：这里测的是读面（有哪些事实
// 会被看见、上限与窗口怎么截断），直插能精确控制 created_at / result / payload；
// 走写路径还要造节点租约与授权事实，测出来的仍然是同一批 SELECT。

// observedFixture 是一套最小事实：ns + 一个能力行 + 一个「最近任务」。
type observedFixture struct {
	db      *DB
	nsID    string
	capName string
	capID   string
	taskID  string
	traceID string
}

func newObservedFixture(t *testing.T) observedFixture {
	t.Helper()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	const capName = "observed-cap"
	cap := v4Cap(t, db, nsID, capName, 1)
	return observedFixture{db: db, nsID: nsID, capName: capName, capID: cap.ID}
}

// seedTask 直插一个任务行（digest/version 都写实，否则 tasks_resolved_pin_shape 会挡）。
func (f *observedFixture) seedTask(t *testing.T, taskID, traceID string, createdAt int64, phase string) {
	t.Helper()
	mustExec(t, f.db, `INSERT INTO tasks
		(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at)
		VALUES (?, ?, ?, ?, ?, 1, 'sha256:seed', '{}', ?, ?)`,
		taskID, f.nsID, traceID, f.capID, f.capName, phase, createdAt)
}

// seedRecentTask 造「最近任务」并把 fixture 的 taskID/traceID 指向它。
func (f *observedFixture) seedRecentTask(t *testing.T, createdAt int64) {
	t.Helper()
	f.taskID = NewTaskID()
	f.traceID = "trc_" + f.taskID
	f.seedTask(t, f.taskID, f.traceID, createdAt, TaskSucceeded)
}

// seedEmit 直插一条 emit 行（data/result 原样落库，与 EmitAuthorized 的列一致）。
func (f *observedFixture) seedEmit(t *testing.T, toRef, data, result string, createdAt int64) {
	t.Helper()
	mustExec(t, f.db, `INSERT INTO emits (id, task_id, to_ref, data, result, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		NewEventID(), f.taskID, toRef, data, result, createdAt)
}

// seedPruneEvent 直插一条 route 剪枝观察事件（shape 与 insertPruneFactEventOn 一致）。
func (f *observedFixture) seedPruneEvent(t *testing.T, traceID, eventType, capability, detail, payloadTaskID string, createdAt int64) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"capability": capability, "reason": eventType, "detail": detail,
		"taskId": payloadTaskID, "parentTaskId": payloadTaskID,
	})
	if err != nil {
		t.Fatalf("marshal prune payload: %v", err)
	}
	mustExec(t, f.db, `INSERT INTO events
		(id, namespace_id, trace_id, source, type, payload, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		NewEventID(), f.nsID, traceID, EventPruneSource, eventType, string(payload), EventReasonPruned, createdAt)
}

// ---------- 无数据 ----------

// TestObservedForCapabilityEmpty：没有任何事实时必须是**零值**，而且 Emits/Pruned
// 是空 slice 而不是 nil（契约：空列表不是 null，由 server 决定什么时候写 `{}`）。
func TestObservedForCapabilityEmpty(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)

	cases := []struct {
		name string
		nsID string
		cap  string
	}{
		{name: "能力从没跑过", nsID: nsID, cap: "never-ran"},
		{name: "不存在的命名空间", nsID: "ns_ghost", cap: "never-ran"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ObservedForCapability(ctx, tc.nsID, tc.cap)
			if err != nil {
				t.Fatalf("ObservedForCapability: %v", err)
			}
			if got.TaskID != "" || got.Total != 0 || got.Failed != 0 {
				t.Fatalf("无事实应为零值，得到 %+v", got)
			}
			if got.Emits == nil || len(got.Emits) != 0 {
				t.Fatalf("Emits 应为空切片，得到 %#v", got.Emits)
			}
			if got.Pruned == nil || len(got.Pruned) != 0 {
				t.Fatalf("Pruned 应为空切片，得到 %#v", got.Pruned)
			}
		})
	}

	// 参数缺失是**编程错误**（不是「没数据」）：这两条必须报错，否则空 ns 的调用
	// 会拿到一份看起来正常的空结果，问题被藏起来。
	for _, args := range [][2]string{{"", "cap"}, {"ns", ""}} {
		if _, err := db.ObservedForCapability(ctx, args[0], args[1]); err == nil {
			t.Fatalf("ObservedForCapability(%q, %q) 应报错", args[0], args[1])
		}
	}
}

// ---------- 有事实 ----------

// TestObservedForCapabilityWithFacts：emits 的三列、pruned 的三列、24h 计数都要
// 从真事实里读出来。emits 的 `ok` 规则用表驱动逐形状钉住——它是唯一一条「从回执
// JSON 反推语义」的判断，静默错一位就会把剪枝显示成成功。
func TestObservedForCapabilityWithFacts(t *testing.T) {
	ctx := context.Background()
	f := newObservedFixture(t)
	base := Now() - 600
	f.seedRecentTask(t, base)

	// 同一能力里另有两条窗口内任务：一条 failed、一条 succeeded（计数用）。
	f.seedTask(t, NewTaskID(), "trc_other", base-100, TaskFailed)
	f.seedTask(t, NewTaskID(), "trc_other2", base-200, TaskSucceeded)

	// emit 的 ok 判定：result 的每种现存形状一行（见 observedEmitOK 的注释）。
	emits := []struct {
		to      string
		data    string
		result  string
		fields  []string
		ok      bool
		comment string
	}{
		{to: "analyze-pr", data: `{"repo":"anotify","diff":"a.patch"}`,
			result: `{"taskId":"tsk_child","eventId":"emt_1","to":"analyze-pr","version":2}`,
			fields: []string{"diff", "repo"}, ok: true, comment: "正常派发：回执点名真实任务"},
		{to: "lint-fix", data: `{}`,
			result: "", fields: []string{}, ok: true, comment: "无 result（旧行）视为 ok"},
		{to: "legacy", data: ``,
			result: `{"taskIds":["tsk_a","tsk_b"]}`, fields: []string{}, ok: true,
			comment: "store 自动生成的回执（非空 taskIds）"},
		{to: "noop", data: `{"x":1}`,
			result: `{"taskIds":[]}`, fields: []string{"x"}, ok: false, comment: "空 taskIds = 没有派发事实"},
		{to: "lint-fix2", data: `"scalar"`,
			result: `{"taskId":null,"eventId":"emt_2","to":"lint-fix2","pruned":{"reason":"param_missing","detail":"missing required param: diff"}}`,
			fields: []string{}, ok: false, comment: "剪枝回执：收到但没建任务"},
		{to: "broken", data: `{}`, result: `not-json`, fields: []string{}, ok: false,
			comment: "坏字节不是派发成功的证据"},
	}
	for i, e := range emits {
		f.seedEmit(t, e.to, e.data, e.result, base+int64(i))
	}

	// 剪枝：两条属于最近任务，一条属于同 trace 的别的任务（必须被 payload.taskId 排除）。
	f.seedPruneEvent(t, f.traceID, protocol.ReasonParamMissing, "lint-fix", "missing required param: diff", f.taskID, base+10)
	f.seedPruneEvent(t, f.traceID, protocol.ReasonNoTarget, "notify", "", f.taskID, base+11)
	f.seedPruneEvent(t, f.traceID, protocol.ReasonParamMissing, "someone-else", "", "tsk_other_parent", base+12)

	got, err := f.db.ObservedForCapability(ctx, f.nsID, f.capName)
	if err != nil {
		t.Fatalf("ObservedForCapability: %v", err)
	}
	if got.TaskID != f.taskID {
		t.Fatalf("TaskID = %q, want %q（最近任务）", got.TaskID, f.taskID)
	}
	if got.Total != 3 || got.Failed != 1 {
		t.Fatalf("窗口计数 = (total %d, failed %d), want (3, 1)", got.Total, got.Failed)
	}

	if len(got.Emits) != len(emits) {
		t.Fatalf("Emits 有 %d 条，want %d", len(got.Emits), len(emits))
	}
	for i, want := range emits {
		item := got.Emits[i]
		if item.To != want.to || item.OK != want.ok {
			t.Fatalf("Emits[%d]（%s） = {to:%q ok:%v}, want {to:%q ok:%v}",
				i, want.comment, item.To, item.OK, want.to, want.ok)
		}
		if strings.Join(item.Fields, ",") != strings.Join(want.fields, ",") {
			t.Fatalf("Emits[%d]（%s） fields = %v, want %v（键名排序后）",
				i, want.comment, item.Fields, want.fields)
		}
	}

	wantPruned := []CapabilityObservedPrune{
		{To: "lint-fix", Reason: protocol.ReasonParamMissing, Detail: "missing required param: diff"},
		{To: "notify", Reason: protocol.ReasonNoTarget, Detail: ""},
	}
	if len(got.Pruned) != len(wantPruned) {
		t.Fatalf("Pruned 有 %d 条，want %d（payload.taskId 过滤掉了别的任务）: %#v",
			len(got.Pruned), len(wantPruned), got.Pruned)
	}
	for i, want := range wantPruned {
		if got.Pruned[i] != want {
			t.Fatalf("Pruned[%d] = %+v, want %+v", i, got.Pruned[i], want)
		}
	}
}

// ---------- 上限与窗口 ----------

// TestObservedForCapabilityLimits：emits / pruned 各自只回上限条，窗口外的任务不计入。
//
// 窗口用**把 created_at 写到窗口外**来造（不 sleep）：ObservedWindow 是常量，
// 25h 前的行必然落在 `created_at >= Now()-24h` 之外，与真实时钟无关。
func TestObservedForCapabilityLimits(t *testing.T) {
	ctx := context.Background()

	t.Run("emits 只回上限条", func(t *testing.T) {
		f := newObservedFixture(t)
		base := Now() - 7200
		f.seedRecentTask(t, base)
		total := ObservedEmitLimit + 10
		for i := 0; i < total; i++ {
			f.seedEmit(t, fmt.Sprintf("to-%03d", i), `{}`, "", base+int64(i))
		}
		got, err := f.db.ObservedForCapability(ctx, f.nsID, f.capName)
		if err != nil {
			t.Fatalf("ObservedForCapability: %v", err)
		}
		if len(got.Emits) != ObservedEmitLimit {
			t.Fatalf("Emits 回了 %d 条，want %d（插入 %d 条）", len(got.Emits), ObservedEmitLimit, total)
		}
		// 截断必须是「最早的那批」：按 created_at, id 正序取前 N 条。
		if got.Emits[0].To != "to-000" || got.Emits[ObservedEmitLimit-1].To != fmt.Sprintf("to-%03d", ObservedEmitLimit-1) {
			t.Fatalf("截断位置不对：首 %q 末 %q", got.Emits[0].To, got.Emits[ObservedEmitLimit-1].To)
		}
	})

	t.Run("pruned 只回上限条", func(t *testing.T) {
		f := newObservedFixture(t)
		base := Now() - 7200
		f.seedRecentTask(t, base)
		total := ObservedPruneLimit + 10
		for i := 0; i < total; i++ {
			f.seedPruneEvent(t, f.traceID, protocol.ReasonParamMissing, fmt.Sprintf("to-%03d", i), "", f.taskID, base+int64(i))
		}
		got, err := f.db.ObservedForCapability(ctx, f.nsID, f.capName)
		if err != nil {
			t.Fatalf("ObservedForCapability: %v", err)
		}
		if len(got.Pruned) != ObservedPruneLimit {
			t.Fatalf("Pruned 回了 %d 条，want %d（插入 %d 条）", len(got.Pruned), ObservedPruneLimit, total)
		}
		if got.Pruned[0].To != "to-000" {
			t.Fatalf("截断从 %q 开始，want to-000（正序前 N 条）", got.Pruned[0].To)
		}
	})

	t.Run("窗口外的任务不计入", func(t *testing.T) {
		f := newObservedFixture(t)
		// 唯一一条任务在 25h 前：它是「最近任务」，但落在 24h 窗口之外。
		f.seedTask(t, "tsk_old", "trc_old", Now()-int64(ObservedWindow.Seconds())-3600, TaskFailed)
		got, err := f.db.ObservedForCapability(ctx, f.nsID, f.capName)
		if err != nil {
			t.Fatalf("ObservedForCapability: %v", err)
		}
		if got.TaskID != "tsk_old" {
			t.Fatalf("TaskID = %q, want tsk_old（最近任务不受窗口限制）", got.TaskID)
		}
		if got.Total != 0 || got.Failed != 0 {
			t.Fatalf("窗口计数 = (total %d, failed %d), want (0, 0)：25h 前的行不该被算进来",
				got.Total, got.Failed)
		}
	})
}

// ---------- 查询计划门禁 ----------

// TestObservedCountsQueryPlan 钉住 24h 计数的计划：必须是 idx_tasks_cap_created 的
// 三段 seek（ns/capability/created_at），不能是全表扫——这条查询在能力详情与列表里
// 每次 GET 都跑一遍，全表扫会把 tasks 的行数直接乘进页面延迟。
//
// 为什么必须**多命名空间 + ANALYZE**：单 ns 的小库上规划器会认为 `namespace_id=?`
// 没有区分度而退化成 `SCAN tasks`（实测），那样的断言是假绿。真库是「一个 ns 一小片，
// 多个 ns 并存」且 Open 末尾会补统计（PRAGMA optimize），所以门禁按那个形状造数据。
func TestObservedCountsQueryPlan(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	cap := v4Cap(t, db, nsID, "plan-cap", 1)

	// 三个命名空间、每个 800 行、5 个能力：cap-a 占 1/5，够接近真实分布。
	namespaces := []string{nsID}
	for i := 0; i < 2; i++ {
		ns, err := db.CreateNamespace(ctx, fmt.Sprintf("plan-ns-%d", i), fmt.Sprintf("计划用 ns %d", i))
		if err != nil {
			t.Fatalf("create namespace: %v", err)
		}
		namespaces = append(namespaces, ns.ID)
	}
	capNames := []string{cap.Name, "plan-cap-b", "plan-cap-c", "plan-cap-d", "plan-cap-e"}
	byNS := map[string]map[string]*CapabilityVersion{}
	for _, ns := range namespaces {
		byNS[ns] = map[string]*CapabilityVersion{cap.Name: cap}
		for _, name := range capNames[1:] {
			byNS[ns][name] = v4Cap(t, db, ns, name, 1)
		}
	}
	now := Now()
	seq := 0
	for _, ns := range namespaces {
		for i := 0; i < 800; i++ {
			seq++
			capName := capNames[i%len(capNames)]
			capRow := byNS[ns][capName]
			phase := TaskSucceeded
			if i%7 == 0 {
				phase = TaskFailed
			}
			mustExec(t, db, `INSERT INTO tasks
				(id, namespace_id, trace_id, capability_id, capability, version, digest, params, phase, created_at, finished_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, ?)`,
				fmt.Sprintf("tsk_plan_%05d", seq), ns, fmt.Sprintf("trc_plan_%05d", seq),
				capRow.ID, capName, capRow.Version, capRow.Digest, phase, now-int64(i*60), now)
		}
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// 统计必须真的落了：没统计的计划断言同样是假绿（见 pending_backlog_test.go）。
	var stat string
	if err := db.QueryRow(`SELECT stat FROM sqlite_stat1 WHERE tbl = 'tasks' AND idx = 'idx_tasks_cap_created'`).
		Scan(&stat); err != nil {
		t.Fatalf("sqlite_stat1 里没有 idx_tasks_cap_created 的统计: %v", err)
	}
	t.Logf("sqlite_stat1[idx_tasks_cap_created] = %s", stat)

	plan := strings.Join(explainQueryPlan(t, db, observedCountsSQL,
		nsID, cap.Name, Now()-int64(ObservedWindow.Seconds())), "\n")

	// 「最近任务」只做证据不做断言：它走既有 idx_tasks_ns_created 的反向扫（零排序），
	// 或者在有统计时走新索引 + 一个 temp B-tree——两条都不是全表扫，规划器怎么选都能接受，
	// 所以这里只要求「没有 SCAN tasks」，并把实际计划打出来存档。
	recentPlan := strings.Join(explainQueryPlan(t, db,
		`SELECT id, trace_id FROM tasks
		  WHERE namespace_id = ? AND capability = ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`, nsID, cap.Name), "\n")
	t.Logf("EXPLAIN QUERY PLAN（最近任务）:\n%s", recentPlan)
	if strings.Contains(recentPlan, "SCAN tasks") {
		t.Errorf("最近任务出现 `SCAN tasks`（全表扫）:\n%s", recentPlan)
	}
	t.Logf("EXPLAIN QUERY PLAN（24h 窗口计数）:\n%s", plan)
	if !strings.Contains(plan, "idx_tasks_cap_created") {
		t.Errorf("24h 计数没有走 idx_tasks_cap_created（做了全表扫？）:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN tasks") {
		t.Errorf("24h 计数出现 `SCAN tasks`（全表扫）:\n%s", plan)
	}

	// 索引形状：列序 / 数量各一条（防「重名不同列」的老索引把门禁骗过去）。
	if n := countIndexRows(t, db, "tasks", "idx_tasks_cap_created"); n != 1 {
		t.Fatalf("idx_tasks_cap_created 有 %d 条, want 1", n)
	}
	cols := indexColumnsOn(t, db, "tasks", "idx_tasks_cap_created")
	if len(cols) != 4 || cols[0] != "namespace_id" || cols[1] != "capability" || cols[2] != "created_at" || cols[3] != "id" {
		t.Fatalf("idx_tasks_cap_created 的列序 = %v, want [namespace_id capability created_at id]", cols)
	}

	// 计划门禁必须测**产品真的跑的那条 SQL**：常量被换掉而这里还在测旧串 = 假绿。
	src, err := os.ReadFile("capability_observed.go")
	if err != nil {
		t.Fatalf("read capability_observed.go: %v", err)
	}
	if !strings.Contains(string(src), "queryRowContext(ctx, observedCountsSQL") {
		t.Fatalf("产品代码没有用 observedCountsSQL 跑窗口计数——本门禁在测一条没人跑的 SQL")
	}
}

// TestObservedPruneQueryPlan 钉住剪枝查询的计划：按 (ns, trace_id) 走既有 idx_events_trace，
// 不能退化成扫 events 全表。payload 里的 taskId 过滤写在 SQL 里（为了 LIMIT 作用在**匹配行**
// 上），所以这里也要确认那条 CASE/json_extract 没有把索引 seek 弄丢。
func TestObservedPruneQueryPlan(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)

	// 200 个 trace × 5 条 route 事件：够让统计成型（单 trace 只有几条，靠索引 seek）。
	now := Now()
	for i := 0; i < 200; i++ {
		traceID := fmt.Sprintf("trc_plan_%03d", i)
		for j := 0; j < 5; j++ {
			payload := fmt.Sprintf(`{"capability":"cap-%d","reason":"param_missing","detail":"","taskId":"tsk_plan_%03d"}`, j, i)
			mustExec(t, db, `INSERT INTO events
				(id, namespace_id, trace_id, source, type, payload, reason, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				NewEventID(), nsID, traceID, EventPruneSource, protocol.ReasonParamMissing,
				payload, EventReasonPruned, now-int64(i*60+j))
		}
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	plan := strings.Join(explainQueryPlan(t, db, observedPruneSQL,
		nsID, "trc_plan_007", EventPruneSource, "tsk_plan_007", ObservedPruneLimit), "\n")
	t.Logf("EXPLAIN QUERY PLAN（最近任务的剪枝事件）:\n%s", plan)
	if !strings.Contains(plan, "idx_events_trace") {
		t.Errorf("剪枝查询没有走 idx_events_trace:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN events") {
		t.Errorf("剪枝查询出现 `SCAN events`（全表扫）:\n%s", plan)
	}

	// 产品真的跑的是这条 SQL（不是测试里另写的一份）。
	if _, err := db.ObservedForCapability(ctx, nsID, "no-such-capability"); err != nil {
		t.Fatalf("ObservedForCapability: %v", err)
	}
}
