package v4test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 D2「任务输入钉版」的反例集（G1-4R 硬项）：
//
//   - namespace 层的值属于**任务输入**，在任务被接受时钉住；排队期间改 ns 配置
//     不该改变已接受任务的输入（否则同一条任务重试前后用的是两套值）；
//   - 机器层（local > node）按本次 attempt 的节点重新解析：换节点重试拿到新机器
//     自己的值，而不是上一台的；
//   - 公共 JSON 面不得回显 ns env 的实际值（只回显 namespaceVarsRevision 版本号）；
//   - 只能按名字点名取（不能把整份 ns 快照隐式灌成 run.env）。

// TestTaskNamespaceInputsPinnedAcrossConfigUpdate 验「先 A 后更新 B，任务仍是 A」。
func TestTaskNamespaceInputsPinnedAcrossConfigUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "inputs-pin")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)

	pinned := setNamespaceVars(t, db, ns, map[string]string{"SOURCE_ROOT": "ns-value-A"})
	task := newTask(t, db, ns, c, "trace-pin", "")
	if task.NamespaceVarsRevision != pinned.Revision || task.NamespaceVars["SOURCE_ROOT"] != "ns-value-A" {
		t.Fatalf("任务接受时必须钉住 ns 输入：rev=%d vars=%v（want rev=%d/A）",
			task.NamespaceVarsRevision, task.NamespaceVars, pinned.Revision)
	}

	// 排队期间把 ns 默认值改成 B：已接受的任务不跟着变。
	updated := setNamespaceVars(t, db, ns, map[string]string{"SOURCE_ROOT": "ns-value-B"})
	if updated.Revision == pinned.Revision {
		t.Fatalf("写入 ns 配置必须推进 namespaceVarsRevision")
	}
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.SOURCE_ROOT"})
	if err != nil {
		t.Fatalf("task inputs: %v", err)
	}
	got, ok := inputs["SOURCE_ROOT"]
	if !ok || got.Value != "ns-value-A" || got.Source != store.VarSourceNamespace {
		t.Fatalf("排队期间改配置不该影响已接受任务：%+v", inputs)
	}
	reread, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if reread.NamespaceVarsRevision != pinned.Revision {
		t.Fatalf("任务行的 namespaceVarsRevision 必须是接受时的版本 %d，得到 %d",
			pinned.Revision, reread.NamespaceVarsRevision)
	}

	// 配置更新只影响**下一个**任务。
	next := newTask(t, db, ns, c, "trace-next", "")
	if next.NamespaceVars["SOURCE_ROOT"] != "ns-value-B" || next.NamespaceVarsRevision != updated.Revision {
		t.Fatalf("新任务应钉住最新一版 ns 输入：rev=%d vars=%v", next.NamespaceVarsRevision, next.NamespaceVars)
	}
}

// TestTaskInputsMachineLayerReparsedPerNode 验跨节点重试：机器值换节点重解析、ns 值仍 A。
func TestTaskInputsMachineLayerReparsedPerNode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "inputs-node")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	pinned := setNamespaceVars(t, db, ns, map[string]string{"NS_VAR": "ns-A"})

	nodeA := newNode(t, db, "node-a", 1)
	nodeB := newNode(t, db, "node-b", 1)
	helloNode(t, db, nodeA, map[string]string{"M_VAR": "from-A"}, nil)
	helloNode(t, db, nodeB, map[string]string{"M_VAR": "from-B"}, nil)

	task := newTask(t, db, ns, c, "trace-node", "")
	// 排队之后把 ns 配置也改掉：跨节点重试仍须用 A。
	setNamespaceVars(t, db, ns, map[string]string{"NS_VAR": "ns-B"})

	leaseID, _, attempt := grant(t, db, task.ID, nodeA.ID, "asg-a")
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.NS_VAR", "vars.M_VAR"})
	if err != nil {
		t.Fatalf("task inputs on node A: %v", err)
	}
	if inputs["NS_VAR"].Value != "ns-A" || inputs["NS_VAR"].Source != store.VarSourceNamespace {
		t.Fatalf("ns 层必须是钉住的 A：%+v", inputs["NS_VAR"])
	}
	if inputs["M_VAR"].Value != "from-A" || inputs["M_VAR"].Source != store.VarSourceLocal {
		t.Fatalf("机器层必须来自本次节点：%+v", inputs["M_VAR"])
	}
	onA, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get granted task: %v", err)
	}
	if onA.NamespaceVarsRevision != pinned.Revision {
		t.Fatalf("grant 后残留的 ns 版本 = %d，want %d", onA.NamespaceVarsRevision, pinned.Revision)
	}

	// 租约过期 → 幂等能力重排 → 换到 nodeB 重试。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskPending)
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, _, attempt2, _, err := func() (string, string, int, int64, error) {
		if aerr := db.AssignTask(ctx, task.ID, nodeB.ID, "asg-b", attempt+1); aerr != nil {
			t.Fatalf("re-assign on node B: %v", aerr)
		}
		return db.GrantLease(ctx, task.ID, "asg-b", nodeB.ID, store.Now())
	}(); err != nil {
		t.Fatalf("re-grant on node B: %v", err)
	} else if attempt2 != 2 {
		t.Fatalf("换节点重试的 attempt = %d，want 2", attempt2)
	}
	_ = leaseID

	inputs, err = db.TaskInputs(ctx, task.ID, []string{"vars.NS_VAR", "vars.M_VAR"})
	if err != nil {
		t.Fatalf("task inputs on node B: %v", err)
	}
	if inputs["NS_VAR"].Value != "ns-A" {
		t.Fatalf("换节点不该改 ns 输入：%+v", inputs["NS_VAR"])
	}
	if inputs["M_VAR"].Value != "from-B" || inputs["M_VAR"].Source != store.VarSourceLocal {
		t.Fatalf("换节点后机器层必须重新解析成 B 机的值：%+v", inputs["M_VAR"])
	}
	onB, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task on B: %v", err)
	}
	if onB.NamespaceVarsRevision != pinned.Revision {
		t.Fatalf("重试后 ns 版本漂了：%d != %d", onB.NamespaceVarsRevision, pinned.Revision)
	}
	if onB.NodeID == nil || *onB.NodeID != nodeB.ID {
		t.Fatalf("重试后任务应归 node B：%v", onB.NodeID)
	}
}

// TestTaskNamespaceVarsNotInPublicJSON 验公共 JSON 面不回显 ns env 实际值。
func TestTaskNamespaceVarsNotInPublicJSON(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "inputs-json")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	pinned := setNamespaceVars(t, db, ns, map[string]string{"API_SECRET": "ns-secret-A"})
	task := newTask(t, db, ns, c, "trace-json", "")

	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "ns-secret-A") {
		t.Fatalf("公共 JSON 不得回显 ns 值：%s", text)
	}
	if !strings.Contains(text, `"namespaceVarsRevision"`) {
		t.Fatalf("公共 JSON 应能看见 ns 版本号（诊断用）：%s", text)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode task json: %v", err)
	}
	if decoded["namespaceVarsRevision"] != float64(pinned.Revision) {
		t.Fatalf("namespaceVarsRevision JSON = %v, want %d", decoded["namespaceVarsRevision"], pinned.Revision)
	}
	// 内部字段仍然带着值（执行输入），只是不上 JSON。
	if task.NamespaceVars["API_SECRET"] != "ns-secret-A" {
		t.Fatalf("内部快照应保留值：%v", task.NamespaceVars)
	}
}

// TestTaskInputsOnlyNamedKeys 验按名取：不点名就不出现在输入里（不隐式灌全量 ns env）。
func TestTaskInputsOnlyNamedKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "inputs-names")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	setNamespaceVars(t, db, ns, map[string]string{"WANTED": "v", "UNWANTED": "hidden"})
	task := newTask(t, db, ns, c, "trace-names", "")

	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.WANTED"})
	if err != nil {
		t.Fatalf("task inputs: %v", err)
	}
	if len(inputs) != 1 || inputs["WANTED"].Value != "v" {
		t.Fatalf("只该返回点名的键：%+v", inputs)
	}
	if _, ok := inputs["UNWANTED"]; ok {
		t.Fatalf("未点名的 ns 值不该出现：%+v", inputs)
	}
	empty, err := db.TaskInputs(ctx, task.ID, nil)
	if err != nil {
		t.Fatalf("task inputs (empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("不点名 = 空输入（不是全量）：%v", empty)
	}
	if _, err := db.TaskInputs(ctx, "tsk_missing", []string{"vars.WANTED"}); err == nil {
		t.Fatalf("未知任务应报错")
	}
}
