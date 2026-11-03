package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖 P1.6 的 HTTP 侧：`GET /v1/namespaces/{ns}/tasks/{taskId}/emissions`。
//
// 三条判据（都是「看得见」这件事的最小充分条件）：
//  1. 有序：按 seq 升序返回，且 data 是**原样 JSON 对象**而不是字符串；
//  2. 空是 `[]` 不是 `null`（不变量 I9）——前端不能把「还没 emit」和「查不到」混成一件事；
//  3. 跨命名空间不可见：拿别的命名空间的 taskId 读 → 404（与「不存在」逐字节相同）。
//
// 夹具沿用 console_test.go 的 newConsoleEnv（真 SQLite + 真会话中间件 + 真 mux），
// 不另造一套：这里要证明的是「同一个命名空间收口在两个端点形状（query / 路径段）上
// 给出同一套错误码」。

// emissionsPage 是响应形状（只取本套件关心的字段）。
//
// Emissions 用 json.RawMessage 接：本套件既要判「解出来是什么」，也要判
// 「空的时候原始字节是不是 `[]`」——解成 []any 的话 null 与 [] 都会变成 length 0，
// 那条不变量当场失效。
type emissionsPage struct {
	TaskID      string          `json:"taskId"`
	NamespaceID string          `json:"namespaceId"`
	Emissions   json.RawMessage `json:"emissions"`
}

// seedEmissionTask 在指定命名空间建一个任务行（emissions 有外键指向它）。
func seedEmissionTask(t *testing.T, env *consoleEnv, nsID, traceID, capName string) *store.Task {
	t.Helper()
	capDef := env.seedCapabilityIn(t, nsID, capName, 1, []string{"fix"})
	task := &store.Task{
		NamespaceID: nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: capDef.ContentHash, GraphID: "grf_x",
	}
	if err := env.db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// getEmissions 取某任务在某命名空间下的 emissions，返回 (状态码, 解析后的页, 原始体)。
func (e *consoleEnv) getEmissions(t *testing.T, nsID, taskID string) (int, emissionsPage, string) {
	t.Helper()
	path := fmt.Sprintf("/v1/namespaces/%s/tasks/%s/emissions", nsID, taskID)
	rr := e.do(t, http.MethodGet, path, nil)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		return rr.Code, emissionsPage{}, body
	}
	var page emissionsPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode emissions of %s: %v (body=%s)", taskID, err, body)
	}
	return rr.Code, page, body
}

// TestTaskEmissionsOrderedAndShape 落库顺序打乱，读出来必须按 seq 升序，且形状固定。
func TestTaskEmissionsOrderedAndShape(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	task := seedEmissionTask(t, env, env.nsID, "trace_emit", "emitter")

	// 刻意**乱序**插入：读端的保证来自 ORDER BY seq，不是插入顺序。
	// 内容是 §3.1 的形状：同端口两次 + 另一端口一次，其中一次 routed=2。
	rows := []*store.Emission{
		{TaskID: task.ID, Seq: 3, Port: "push", Data: json.RawMessage(`{"to":"u_2"}`), Routed: 0, Pruned: 1, CreatedAt: 3000},
		{TaskID: task.ID, Seq: 1, Port: "fix", Data: json.RawMessage(`{"issue":"#42","repo":"acme/x"}`), Routed: 2, Pruned: 0, CreatedAt: 1000},
		{TaskID: task.ID, Seq: 2, Port: "fix", Data: json.RawMessage(`{"issue":"#43"}`), Routed: 1, Pruned: 0, CreatedAt: 2000},
	}
	for _, e := range rows {
		if _, err := env.db.InsertEmission(ctx, e); err != nil {
			t.Fatalf("insert emission seq=%d: %v", e.Seq, err)
		}
	}

	code, page, body := env.getEmissions(t, env.nsID, task.ID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if page.TaskID != task.ID {
		t.Errorf("taskId = %q, want %q", page.TaskID, task.ID)
	}
	if page.NamespaceID != env.nsID {
		t.Errorf("namespaceId = %q, want %q（作用域必须回显）", page.NamespaceID, env.nsID)
	}

	var got []struct {
		Seq       int64           `json:"seq"`
		Port      string          `json:"port"`
		Data      json.RawMessage `json:"data"`
		Note      *string         `json:"note"`
		Routed    int             `json:"routed"`
		Pruned    int             `json:"pruned"`
		CreatedAt int64           `json:"createdAt"`
	}
	if err := json.Unmarshal(page.Emissions, &got); err != nil {
		t.Fatalf("decode emissions: %v (raw=%s)", err, page.Emissions)
	}
	if len(got) != 3 {
		t.Fatalf("emissions = %d, want 3 (raw=%s)", len(got), page.Emissions)
	}
	for i, want := range []int64{1, 2, 3} {
		if got[i].Seq != want {
			t.Errorf("emissions[%d].seq = %d, want %d（必须按 seq 升序）", i, got[i].Seq, want)
		}
	}
	if got[0].Port != "fix" || got[1].Port != "fix" || got[2].Port != "push" {
		t.Errorf("ports = %q/%q/%q, want fix/fix/push", got[0].Port, got[1].Port, got[2].Port)
	}
	if got[0].Routed != 2 || got[0].Pruned != 0 {
		t.Errorf("seq=1 routed/pruned = %d/%d, want 2/0", got[0].Routed, got[0].Pruned)
	}
	if got[2].Routed != 0 || got[2].Pruned != 1 {
		t.Errorf("seq=3 routed/pruned = %d/%d, want 0/1（剪枝也要看得见）", got[2].Routed, got[2].Pruned)
	}
	if got[0].CreatedAt != 1000 {
		t.Errorf("seq=1 createdAt = %d, want 1000", got[0].CreatedAt)
	}
	// data 必须是 JSON **对象**（前端直接 .foo），不是被再编码一次的字符串。
	var obj map[string]any
	if err := json.Unmarshal(got[0].Data, &obj); err != nil {
		t.Fatalf("data 不是 JSON 对象：%v（raw=%s）", err, got[0].Data)
	}
	if obj["issue"] != "#42" || obj["repo"] != "acme/x" {
		t.Errorf("data = %v, want issue=#42 repo=acme/x（原样返回，不重排不改写）", obj)
	}
	// note 字段必须在场（哪怕是空串）：响应形状固定，前端不必判字段存不存在。
	if got[0].Note == nil {
		t.Error("emissions[0] 缺 note 字段（形状必须固定）")
	}
}

// TestTaskEmissionsEmptyIsArray 没 emit 过的任务：`emissions` 必须是 `[]`，不是 null。
func TestTaskEmissionsEmptyIsArray(t *testing.T) {
	env := newConsoleEnv(t)
	task := seedEmissionTask(t, env, env.nsID, "trace_none", "silent")

	code, page, body := env.getEmissions(t, env.nsID, task.ID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if raw := strings.TrimSpace(string(page.Emissions)); raw != "[]" {
		t.Fatalf("emissions = %s, want []（空列表不能是 null，I9）", raw)
	}
}

// TestTaskEmissionsCrossNamespaceInvisible 跨命名空间的 taskId 一律 404。
//
// 为什么是 404 而不是 403：组织层删除后（#204）本部署内不存在「有、但属于别人」
// 这一态 —— 存在就 200，不存在就 404，探测与正常访问的回答必须一致。
func TestTaskEmissionsCrossNamespaceInvisible(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	other := env.seedNamespace(t, "staging", "预发")
	task := seedEmissionTask(t, env, other.ID, "trace_other", "emitter")
	if _, err := env.db.InsertEmission(ctx, &store.Emission{
		TaskID: task.ID, Seq: 1, Port: "fix", Data: json.RawMessage(`{"a":1}`), Routed: 1,
	}); err != nil {
		t.Fatalf("insert emission: %v", err)
	}

	// 带着**另一个**命名空间的路径读同一个 taskId → 看不到（404，与「不存在」同文案）。
	code, _, body := env.getEmissions(t, env.nsID, task.ID)
	if code != http.StatusNotFound {
		t.Fatalf("跨命名空间读 emissions → %d, want 404；body=%s", code, body)
	}
	if !strings.Contains(body, "task not found") {
		t.Errorf("body = %s, want 与「任务不存在」逐字节相同（不泄露存在性）", body)
	}

	// 同一路径换成正确的命名空间 → 200（证明上一条不是「端点整体坏了」）。
	if code, page, body := env.getEmissions(t, other.ID, task.ID); code != http.StatusOK {
		t.Fatalf("同命名空间读 emissions → %d, want 200；body=%s", code, body)
	} else if strings.TrimSpace(string(page.Emissions)) == "[]" {
		t.Fatal("同命名空间读到的 emissions 是空的（种子没生效）")
	}
}

// TestTaskEmissionsNamespaceScopes 命名空间作用域的参数错误与其它资源端点同口径。
func TestTaskEmissionsNamespaceScopes(t *testing.T) {
	env := newConsoleEnv(t)
	task := seedEmissionTask(t, env, env.nsID, "trace_scope", "emitter")

	// `*` 用在单对象子资源上无定义 → 400（与 /v1/tasks/{id} 一致）。
	if code, _, body := env.getEmissions(t, store.AllNamespaces, task.ID); code != http.StatusBadRequest {
		t.Errorf("namespaceId=* → %d, want 400；body=%s", code, body)
	}
	// 不存在的命名空间 → 404。
	if code, _, body := env.getEmissions(t, "ns_does_not_exist", task.ID); code != http.StatusNotFound {
		t.Errorf("不存在的命名空间 → %d, want 404；body=%s", code, body)
	}
	// 未登录 → 401（路径式端点也必须在同一道会话收口之后）。
	path := fmt.Sprintf("/v1/namespaces/%s/tasks/%s/emissions", env.nsID, task.ID)
	if rr := env.doNoCookie(t, http.MethodGet, path); rr.Code != http.StatusUnauthorized {
		t.Errorf("无会话 → %d, want 401；body=%s", rr.Code, rr.Body.String())
	}
}
