package server

// 能力启停 + 手动试跑的 HTTP 验收（capability-lifecycle L-D1..L-D8 / WIRE.md）。
//
// 覆盖的**契约点**（不是实现细节）：
//   - enable/disable 幂等：连调两次都 200、不动能力行的 resourceVersion、每次都留一条审计；
//   - 启停往返：`GET …/{name}` 的 `status.enabled` 跟着开关行走（无行 = true）；
//   - 试跑在 **disabled** 能力上仍然可用（L-D2：停用拦的是自动触发，不是人工验证）；
//   - 一个节点都没持有 digest → **200** + `warnings[{reason: capability_not_deployed}]`，
//     任务停在 `pending`（绝不是 4xx）；
//   - 参数不合 `spec.params` → **400** + `details.causes[].field` 指到参数路径；
//   - 指定不存在的版本 / 不存在的节点 → 404；
//   - 审计列表：最新在前、`limit` 夹取、空审计是 `{"items": []}`（不是 null）；
//   - 四个端点都要会话（匿名 401）；
//   - 试跑事件在事件流里带 `origin=test`，且默认事件流**不含**它（`?origin=test` 才看得到）。
//
// 夹具复用 `resource_capability_test.go` 的 `newCapTestEnv`（真 sqlite 文件 + 真会话中间件 +
// 真 ws.Hub）；本文件只加两件它没有的能力：可控时钟（审计倒序需要确定的时间）与把 taskAPI
// 挂到同一个 mux 上（事件 origin 用例要读 `/events`）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 为什么必须同一个 mux：试跑建的**事件**要经由 `GET …/events` 读回来（origin 口径），
// 两个域各自起一套夹具就会把「同一个 store 的两条读路径」测成两件无关的事。
func newCapLifecycleEnv(t *testing.T) *capTestEnv {
	t.Helper()
	env := newCapTestEnv(t)
	mux, ok := env.mux.(*http.ServeMux)
	if !ok {
		t.Fatalf("capTestEnv.mux 不是 *http.ServeMux（%T）", env.mux)
	}
	newTaskAPI(env.deps).register(mux)
	return env
}

// setClock 把 server 时钟换成可控的**秒**级时刻（deps.Now 是毫秒，见 api_deps.go）。
//
// 审计的「最新在前」按 `(created_at DESC, id DESC)` 排：两次动作落在同一秒时顺序由 id
// 决定（随机），那样断言倒序就是在断言随机数。用例需要确定的时间就把它钉住。
func (e *capTestEnv) setClock(sec int64) {
	e.deps.Now = func() int64 { return sec * 1000 }
}

func (e *capTestEnv) capEnabled(t *testing.T, ns, name string) bool {
	t.Helper()
	status := e.capabilityStatus(t, ns, name)
	enabled, ok := status["enabled"].(bool)
	if !ok {
		t.Fatalf("status.enabled 不是 bool: %#v", status["enabled"])
	}
	return enabled
}

func (e *capTestEnv) testReceipt(t *testing.T, ns, name string, body any) (int, map[string]any) {
	t.Helper()
	rec := e.do(t, http.MethodPost, e.capPath(ns, name, "test"), body)
	if rec.Code != http.StatusOK {
		return rec.Code, map[string]any{"body": rec.Body.String()}
	}
	return rec.Code, decodeCapJSON(t, rec)
}

func (e *capTestEnv) auditItems(t *testing.T, ns, name, query string) []any {
	t.Helper()
	rec := e.do(t, http.MethodGet, e.capPath(ns, name, "audit")+query, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	return capArray(t, decodeCapJSON(t, rec)["items"], "items")
}

// capabilityBodyWithParams 造一份带 `spec.params` 契约的写请求体（试跑参数校验用）。
func capabilityBodyWithParams(t *testing.T, name, script string, params map[string]any) map[string]any {
	t.Helper()
	body := capabilityBody(name, 0, script)
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("capabilityBody 的 spec 不是对象: %#v", body["spec"])
	}
	spec["params"] = params
	return body
}

// ---------- 1) enable/disable 幂等 ----------

// TestCapabilityLifecycleEnableDisableIdempotent 钉住「重复调 = 200 + 不动能力行 + 两条审计」。
//
// 幂等不是「第二次静默成功」：`resourceVersion` 必须逐字不变（开关不是能力内容，不产生新
// 版本），而审计**必须**多一条（「我又确认了一次」也是操作事实，设计 §3）。
func TestCapabilityLifecycleEnableDisableIdempotent(t *testing.T) {
	env := newCapTestEnv(t)
	created := env.createCapability(t, env.nsA, "toggle", "#!/bin/sh\necho hi\n")
	wantRV := capString(t, assertCapabilityEnvelope(t, created, "toggle"), "resourceVersion")

	for i := 0; i < 2; i++ {
		rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "toggle", "disable"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 disable: status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
		doc := decodeCapJSON(t, rec)
		meta := assertCapabilityEnvelope(t, doc, "toggle")
		if got := capString(t, meta, "resourceVersion"); got != wantRV {
			t.Fatalf("第 %d 次 disable 后 metadata.resourceVersion = %q，want %q（启停不改能力行）", i+1, got, wantRV)
		}
		status := capObject(t, doc["status"], "status")
		if enabled, _ := status["enabled"].(bool); enabled {
			t.Fatalf("第 %d 次 disable 的回执里 status.enabled 仍为 true", i+1)
		}
		if got := capString(t, meta, "name"); got != "toggle" {
			t.Fatalf("metadata.name = %q", got)
		}
	}

	items := env.auditItems(t, env.nsA, "toggle", "")
	if len(items) != 2 {
		t.Fatalf("审计应有 2 条 disable，实际 %d: %#v", len(items), items)
	}
	for i, item := range items {
		entry := capObject(t, item, "audit[]")
		if got := capString(t, entry, "action"); got != store.CapabilityAuditDisable {
			t.Fatalf("audit[%d].action = %q, want disable", i, got)
		}
		if got := capString(t, entry, "id"); got == "" {
			t.Fatalf("audit[%d].id 为空", i)
		}
		if _, ok := entry["detail"].(map[string]any); !ok {
			t.Fatalf("audit[%d].detail 不是对象: %#v", i, entry["detail"])
		}
	}
}

// ---------- 2) disable → enable 往返 ----------

// TestCapabilityLifecycleEnableDisableRoundTrip 钉住 `status.enabled` 是开关行的派生投影。
func TestCapabilityLifecycleEnableDisableRoundTrip(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "round-trip", "#!/bin/sh\necho round\n")

	// 无开关行 = 启用（默认值，不是「未知」）。
	if enabled := env.capEnabled(t, env.nsA, "round-trip"); !enabled {
		t.Fatal("新建能力的 status.enabled 应为 true（无开关行 = 启用）")
	}
	if rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "round-trip", "disable"),
		map[string]any{"reason": "上游在改接口"}); rec.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if enabled := env.capEnabled(t, env.nsA, "round-trip"); enabled {
		t.Fatal("disable 后 GET 的 status.enabled 仍为 true")
	}
	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "round-trip", "enable"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	assertCapabilityEnvelope(t, doc, "round-trip")
	status := capObject(t, doc["status"], "status")
	if enabled, _ := status["enabled"].(bool); !enabled {
		t.Fatal("enable 的回执里 status.enabled 仍为 false")
	}
	if enabled := env.capEnabled(t, env.nsA, "round-trip"); !enabled {
		t.Fatal("enable 后 GET 的 status.enabled 仍为 false")
	}
	// disable 的 reason 进审计（不进 status）。按 action 找那条：两次动作可能落在同一秒，
	// 顺序由 (created_at, id) 决定，不该在「reason 是否存在」这个断言上依赖它。
	items := env.auditItems(t, env.nsA, "round-trip", "")
	if len(items) != 2 {
		t.Fatalf("审计应有 2 条（disable + enable），实际 %d", len(items))
	}
	var disabledReason string
	var sawEnable bool
	for _, item := range items {
		entry := capObject(t, item, "audit[]")
		switch capString(t, entry, "action") {
		case store.CapabilityAuditDisable:
			detail := capObject(t, entry["detail"], "detail")
			disabledReason = capString(t, detail, "reason")
		case store.CapabilityAuditEnable:
			sawEnable = true
		}
	}
	if !sawEnable {
		t.Fatalf("审计里没有 enable 记录: %#v", items)
	}
	if disabledReason != "上游在改接口" {
		t.Fatalf("disable 审计的 detail.reason = %q, want 「上游在改接口」", disabledReason)
	}
}

// TestCapabilityDisableBodyIsStrict 钉住 disable 的 body 契约：`additionalProperties:false`、
// `reason.maxLength: 200`。静默忽略未知字段会让调用方以为「force 生效了」。
func TestCapabilityDisableBodyIsStrict(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "strict-body", "#!/bin/sh\necho strict\n")

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "strict-body", "disable"),
		map[string]any{"reason": strings.Repeat("长", 201)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reason 超过 200 字符应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != reasonValueInvalid {
		t.Fatalf("reason 超长应回 %s，实际 %q", reasonValueInvalid, got)
	}

	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "strict-body", "disable"),
		map[string]any{"reason": "维护", "force": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != reasonUnknownField {
		t.Fatalf("未知字段应回 %s，实际 %q", reasonUnknownField, got)
	}

	// 200 字符是合法的上界（含边界，不误伤）。
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "strict-body", "disable"),
		map[string]any{"reason": strings.Repeat("长", 200)})
	if rec.Code != http.StatusOK {
		t.Fatalf("reason 恰好 200 字符应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 3) disabled 能力上仍可试跑 ----------

// TestCapabilityTestAllowedWhileDisabled 钉住 L-D2：停用拦的是**自动**触发。
func TestCapabilityTestAllowedWhileDisabled(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "still-testable", "#!/bin/sh\necho still\n")
	if rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "still-testable", "disable"), nil); rec.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if enabled := env.capEnabled(t, env.nsA, "still-testable"); enabled {
		t.Fatal("前置条件不成立：能力应为停用")
	}

	code, receipt := env.testReceipt(t, env.nsA, "still-testable", nil)
	if code != http.StatusOK {
		t.Fatalf("disabled 能力上试跑应为 200，实际 %d: %#v", code, receipt)
	}
	if got := capString(t, receipt, "origin"); got != store.EventOriginTest {
		t.Fatalf("回执 origin = %q, want test", got)
	}
	tasks := capArray(t, receipt["tasks"], "tasks")
	if len(tasks) != 1 {
		t.Fatalf("回执 tasks 应有 1 条，实际 %d: %#v", len(tasks), tasks)
	}
	task := capObject(t, tasks[0], "tasks[0]")
	taskID := capString(t, task, "taskId")
	if taskID == "" {
		t.Fatal("tasks[0].taskId 为空")
	}
	if got := capString(t, task, "capability"); got != "still-testable" {
		t.Fatalf("tasks[0].capability = %q", got)
	}
	if got := capNumber(t, task, "version"); got != 1 {
		t.Fatalf("tasks[0].version = %d, want 1", got)
	}
	// 真任务：库里有行，phase=pending（不是「假装建了」）。
	stored, err := env.db.GetTask(context.Background(), env.nsA, taskID)
	if err != nil {
		t.Fatalf("读回试跑任务 %s: %v", taskID, err)
	}
	if stored.Phase != store.TaskPending {
		t.Fatalf("试跑任务 phase = %q, want pending", stored.Phase)
	}
	if stored.TraceID != capString(t, receipt, "traceId") {
		t.Fatalf("任务 trace = %q，回执 traceId = %q", stored.TraceID, receipt["traceId"])
	}
}

// ---------- 4) 未部署 → 200 + warning ----------

// TestCapabilityTestNotDeployedWarns 钉住「没有在线节点持有该 digest」不是 4xx。
func TestCapabilityTestNotDeployedWarns(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "lonely", "#!/bin/sh\necho lonely\n")

	code, receipt := env.testReceipt(t, env.nsA, "lonely", nil)
	if code != http.StatusOK {
		t.Fatalf("未部署时试跑应为 200（不是 4xx），实际 %d: %#v", code, receipt)
	}
	warnings := capArray(t, receipt["warnings"], "warnings")
	if len(warnings) != 1 {
		t.Fatalf("warnings 应有 1 条，实际 %d: %#v", len(warnings), warnings)
	}
	warning := capObject(t, warnings[0], "warnings[0]")
	if got := capString(t, warning, "reason"); got != capabilityWarningNotDeployed {
		t.Fatalf("warnings[0].reason = %q, want %s", got, capabilityWarningNotDeployed)
	}
	if got := capString(t, warning, "detail"); got == "" {
		t.Fatal("warnings[0].detail 为空（要能告诉用户「等什么」）")
	}
	if pruned := capArray(t, receipt["pruned"], "pruned"); len(pruned) != 0 {
		t.Fatalf("pruned 应为 []，实际 %#v", pruned)
	}
	tasks := capArray(t, receipt["tasks"], "tasks")
	if len(tasks) != 1 {
		t.Fatalf("回执 tasks 应有 1 条，实际 %d", len(tasks))
	}
	taskID := capString(t, capObject(t, tasks[0], "tasks[0]"), "taskId")
	stored, err := env.db.GetTask(context.Background(), env.nsA, taskID)
	if err != nil {
		t.Fatalf("读回试跑任务 %s: %v", taskID, err)
	}
	if stored.Phase != store.TaskPending {
		t.Fatalf("未部署的试跑任务 phase = %q, want pending（排队等节点，不是失败）", stored.Phase)
	}
}

// TestCapabilityTestNoWarningWhenNodeHoldsDigest 是上面那条的反向对照：证明 warning 是
// 算出来的，而不是恒回一条。
//
// 节点的可用性判据与派发路径**同一份**（EligibleNodesForTask）：在线 × 授权 × 配置已 apply
// × deployments 行 state=applied × 命令/值齐。这里把整条事实链都造出来。
func TestCapabilityTestNoWarningWhenNodeHoldsDigest(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "deployed", "#!/bin/sh\necho deployed\n")
	env.deployNode(t, "node-holds-it", store.NodeStatusOnline, nil)
	env.deployVersion(t, env.nsA, "deployed", 1, "node-holds-it")
	env.markDeploymentApplied(t, env.nsA, "deployed", 1, "node-holds-it")

	code, receipt := env.testReceipt(t, env.nsA, "deployed", nil)
	if code != http.StatusOK {
		t.Fatalf("试跑: status=%d %#v", code, receipt)
	}
	if warnings := capArray(t, receipt["warnings"], "warnings"); len(warnings) != 0 {
		t.Fatalf("节点已持有 digest 时 warnings 应为 []，实际 %#v", warnings)
	}
	// 显式指定那台节点：同样没有 warning。
	code, receipt = env.testReceipt(t, env.nsA, "deployed", map[string]any{"nodes": []string{"node-holds-it"}})
	if code != http.StatusOK {
		t.Fatalf("试跑（显式节点）: status=%d %#v", code, receipt)
	}
	if warnings := capArray(t, receipt["warnings"], "warnings"); len(warnings) != 0 {
		t.Fatalf("显式指定已持有的节点时 warnings 应为 []，实际 %#v", warnings)
	}
	// 显式给了不存在的节点 → 404（静默跳过会把笔误变成「跑到所有节点」）。
	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "deployed", "test"),
		map[string]any{"nodes": []string{"node-that-does-not-exist"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 5) 参数不合 schema → 400 + details.causes[] ----------

// TestCapabilityTestParamErrorIs400 钉住「参数校验失败」是调用方输入错误，且能定位到字段。
func TestCapabilityTestParamErrorIs400(t *testing.T) {
	env := newCapTestEnv(t)
	body := capabilityBodyWithParams(t, "param-check", "#!/bin/sh\necho check\n", map[string]any{
		"type":     "object",
		"required": []string{"diff"},
		"properties": map[string]any{
			"diff": map[string]any{"type": "string"},
		},
	})
	if rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body); rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"类型不符", map[string]any{"diff": 42}, "param_type"},
		{"缺必填", map[string]any{}, "param_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "param-check", "test"),
				map[string]any{"params": tc.params})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("参数不合 schema 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
			}
			doc := decodeCapJSON(t, rec)
			if got := capString(t, doc, "reason"); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			details := capObject(t, doc["details"], "details")
			causes := capArray(t, details["causes"], "details.causes")
			if len(causes) == 0 {
				t.Fatalf("details.causes 为空: %s", rec.Body.String())
			}
			first := capObject(t, causes[0], "causes[0]")
			field := capString(t, first, "field")
			if len(field) < len("params.") || field[:len("params.")] != "params." {
				t.Fatalf("causes[0].field = %q，应以 params. 开头（前端据此标到字段旁）", field)
			}
			if got := capString(t, first, "reason"); got != tc.want {
				t.Fatalf("causes[0].reason = %q, want %q", got, tc.want)
			}
			if got := capString(t, first, "message"); got == "" {
				t.Fatal("causes[0].message 为空")
			}
		})
	}
	// 参数错误**不该**建出任务（400 是「什么都没发生」，不是「建了个失败任务」）。
	var taskCount int
	if err := env.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, env.nsA).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("参数被拒时不应建任务，实际 %d 条", taskCount)
	}
}

// ---------- 6) 不存在的版本 / 能力 → 404 ----------

// TestCapabilityTestUnknownVersionIs404 钉住「显式版本不存在」是 404，不是 400 也不是 500。
func TestCapabilityTestUnknownVersionIs404(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "versioned", "#!/bin/sh\necho v1\n")

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "versioned", "test"),
		map[string]any{"version": 7})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的版本应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 能力本身不存在也是 404（隔离不泄露存在性）。
	for _, suffix := range []string{"enable", "disable", "test"} {
		rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "no-such-capability", suffix), nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 一个不存在的能力应 404，实际 %d body=%s", suffix, rec.Code, rec.Body.String())
		}
	}
}

// ---------- 7) 审计列表 ----------

// TestCapabilityLifecycleAuditList 钉住审计列表的形状与倒序（最新在前）、limit 夹取、空列表。
func TestCapabilityLifecycleAuditList(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "audited", "#!/bin/sh\necho audited\n")

	env.setClock(1_760_000_000)
	if rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "audited", "disable"),
		map[string]any{"reason": "窗口维护"}); rec.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	env.setClock(1_760_000_300)
	if code, receipt := env.testReceipt(t, env.nsA, "audited", nil); code != http.StatusOK {
		t.Fatalf("test: status=%d %#v", code, receipt)
	}

	items := env.auditItems(t, env.nsA, "audited", "")
	if len(items) != 2 {
		t.Fatalf("审计应有 2 条（disable + test），实际 %d: %#v", len(items), items)
	}
	newest := capObject(t, items[0], "audit[0]")
	older := capObject(t, items[1], "audit[1]")
	if got := capString(t, newest, "action"); got != store.CapabilityAuditTest {
		t.Fatalf("audit[0].action = %q, want test（最新在前）", got)
	}
	if got := capString(t, older, "action"); got != store.CapabilityAuditDisable {
		t.Fatalf("audit[1].action = %q, want disable", got)
	}
	at := capString(t, newest, "at")
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("audit[0].at = %q 不是 RFC3339: %v", at, err)
	}
	if want := time.Unix(1_760_000_300, 0).UTC().Format(time.RFC3339); at != want {
		t.Fatalf("audit[0].at = %q, want %q（UTC）", at, want)
	}
	if got := capString(t, newest, "actor"); got == "" {
		t.Fatal("audit[0].actor 为空（会话主体应被记下来）")
	}
	// test 的 detail 只记 traceId + version（不存 params 全文）。
	testDetail := capObject(t, newest["detail"], "audit[0].detail")
	if got := capString(t, testDetail, "traceId"); got == "" {
		t.Fatal("test 审计的 detail.traceId 为空")
	}
	if got := capNumber(t, testDetail, "version"); got != 1 {
		t.Fatalf("test 审计的 detail.version = %d, want 1", got)
	}

	// limit=1：只回一条（夹取交给 store）。
	if limited := env.auditItems(t, env.nsA, "audited", "?limit=1"); len(limited) != 1 {
		t.Fatalf("limit=1 应只回 1 条，实际 %d", len(limited))
	}
	// limit 非整数 → 400。
	if rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "audited", "audit")+"?limit=abc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 空审计：`{"items": []}`，不是 null（不变量 I9）。
	env.createCapability(t, env.nsA, "quiet", "#!/bin/sh\necho quiet\n")
	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "quiet", "audit"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("空审计: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode 空审计: %v (body=%s)", err, rec.Body.String())
	}
	if got := string(raw["items"]); got != "[]" {
		t.Fatalf("空审计的 items = %s，应为 []（不是 null）", got)
	}
}

// ---------- 8) 四个端点都要会话 ----------

// TestCapabilityLifecycleRequiresSession 钉住「未登录直 401」（鉴权只有一份：Session 中间件）。
func TestCapabilityLifecycleRequiresSession(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "guarded", "#!/bin/sh\necho guarded\n")

	cases := []struct {
		method string
		suffix string
		body   string
	}{
		{http.MethodPost, "enable", ""},
		{http.MethodPost, "disable", `{"reason":"x"}`},
		{http.MethodPost, "test", `{"version":1}`},
		{http.MethodGet, "audit", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, env.capPath(env.slugA, "guarded", tc.suffix),
			taskResRequestBody(t, tc.body))
		recorder := httptest.NewRecorder()
		env.mux.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("匿名 %s %s 应 401，实际 %d body=%s", tc.method, tc.suffix, recorder.Code, recorder.Body.String())
		}
	}
}

// ---------- 9) 试跑事件带 origin=test ----------

// TestCapabilityTestEventOriginInEventStream 钉住事件读面的 origin 口径：
// 默认事件流**不含**试跑（人工动作不冒充外部输入），`?origin=test` 只看试跑，非法值是 400。
func TestCapabilityTestEventOriginInEventStream(t *testing.T) {
	env := newCapLifecycleEnv(t)
	env.createCapability(t, env.nsA, "origin-marked", "#!/bin/sh\necho origin\n")

	code, receipt := env.testReceipt(t, env.nsA, "origin-marked", nil)
	if code != http.StatusOK {
		t.Fatalf("test: status=%d %#v", code, receipt)
	}
	traceID := capString(t, receipt, "traceId")

	eventsPath := "/v1/namespaces/" + env.nsA + "/events"
	// 默认口径：事件流不含 origin=test。
	rec := env.do(t, http.MethodGet, eventsPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if items := capArray(t, decodeCapJSON(t, rec)["items"], "items"); len(items) != 0 {
		t.Fatalf("默认事件流不应包含试跑事件，实际 %d 条: %s", len(items), rec.Body.String())
	}

	// ?origin=test：只看试跑，且事件 spec 上带 origin=test。
	rec = env.do(t, http.MethodGet, eventsPath+"?origin=test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events?origin=test: status=%d body=%s", rec.Code, rec.Body.String())
	}
	items := capArray(t, decodeCapJSON(t, rec)["items"], "items")
	if len(items) != 1 {
		t.Fatalf("?origin=test 应有 1 条试跑事件，实际 %d: %s", len(items), rec.Body.String())
	}
	spec := capObject(t, capObject(t, items[0], "items[0]")["spec"], "spec")
	if got := capString(t, spec, "origin"); got != store.EventOriginTest {
		t.Fatalf("事件 spec.origin = %q, want test", got)
	}
	if got := capString(t, spec, "traceUID"); got != traceID {
		t.Fatalf("事件 spec.traceUID = %q，回执 traceId = %q", got, traceID)
	}

	// 按 trace 查仍保留试跑（trace 全景必须完整）。
	rec = env.do(t, http.MethodGet, eventsPath+"?trace="+traceID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events?trace=: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if items := capArray(t, decodeCapJSON(t, rec)["items"], "items"); len(items) != 1 {
		t.Fatalf("按 trace 查应含试跑事件（1 条），实际 %d", len(items))
	}

	// 非法 origin → 400（静默忽略会把「筛选写错」变成「看到全部」）。
	rec = env.do(t, http.MethodGet, eventsPath+"?origin=robot", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 origin 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 6b) version: 0 = live（#644 第 1 条） ----------

// TestCapabilityTestVersionZeroIsLive 钉住「省略 / 0 = 当前 live 版本」与代码一致。
//
// schema 曾写 `minimum: 1` 而 description 写「省略 / 0 = live」：按 schema 生成的客户端永远发不出 0，
// 而 handler 的判据本来就是 `< 0` 才拒。别反过来把 0 当非法。
func TestCapabilityTestVersionZeroIsLive(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "live-version", "#!/bin/sh\necho live\n")

	versionOf := func(t *testing.T, receipt map[string]any) int {
		t.Helper()
		tasks := capArray(t, receipt["tasks"], "tasks")
		if len(tasks) != 1 {
			t.Fatalf("回执 tasks 应有 1 条，实际 %d: %#v", len(tasks), tasks)
		}
		return capNumber(t, capObject(t, tasks[0], "tasks[0]"), "version")
	}

	code, zero := env.testReceipt(t, env.nsA, "live-version", map[string]any{"version": 0})
	if code != http.StatusOK {
		t.Fatalf("version: 0 应解析成 live（200），实际 %d: %#v", code, zero)
	}
	if got := versionOf(t, zero); got != 1 {
		t.Fatalf("version: 0 落到的版本 = %d，want 1（live）", got)
	}

	code, omitted := env.testReceipt(t, env.nsA, "live-version", nil)
	if code != http.StatusOK {
		t.Fatalf("省略 version 应为 200，实际 %d: %#v", code, omitted)
	}
	if got, want := versionOf(t, omitted), versionOf(t, zero); got != want {
		t.Fatalf("省略 version 落到 %d，显式 0 落到 %d（两条解析必须同一条）", got, want)
	}

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "live-version", "test"),
		map[string]any{"version": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("version: -1 应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != reasonBadRequest {
		t.Fatalf("version: -1 的 reason = %q，want %q", got, reasonBadRequest)
	}
}

// TestCapabilityDeployVersionZeroIsLive 把同一个不变量钉在 deploy 上（review 第 1 轮第 3 条）。
//
// 修前三处不同向：schema `minimum: 1`、400 文案 `≥ 1`、handler 却只拒 `< 0`（`version <= 0`
// 走 ResolveRef = live）—— 0 被 schema 说成非法、被判据当成 live。修后三处同向：
// schema 下界 0、文案 `≥ 0 (0 = live version)`、0 与省略同一版。
func TestCapabilityDeployVersionZeroIsLive(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "deploy-live", "#!/bin/sh\necho live\n")

	deploy := func(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return env.do(t, http.MethodPost, env.capPath(env.nsA, "deploy-live", "deploy"), body)
	}

	rec := deploy(t, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy 省略 version: status=%d body=%s", rec.Code, rec.Body.String())
	}
	omitted := capNumber(t, decodeCapJSON(t, rec), "version")
	if omitted != 1 {
		t.Fatalf("deploy 省略 version 落到 %d，want 1（live）", omitted)
	}

	rec = deploy(t, map[string]any{"version": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy version: 0: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if zero := capNumber(t, decodeCapJSON(t, rec), "version"); zero != omitted {
		t.Fatalf("deploy version: 0 落到 %d，省略落到 %d（两条解析必须同一条）", zero, omitted)
	}

	// 负数 → 400，且文案与试跑共用一条（不许再把 0 说成非法）。
	rec = deploy(t, map[string]any{"version": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deploy version: -1: status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, doc, "reason"); got != reasonBadRequest {
		t.Fatalf("deploy version: -1 的 reason = %q，want %q", got, reasonBadRequest)
	}
	msg := capString(t, doc, "message")
	if !strings.Contains(msg, "≥ 0 (0 = live version)") {
		t.Fatalf("deploy version: -1 的 message = %q，want 含 “≥ 0 (0 = live version)”", msg)
	}
	if strings.Contains(msg, "≥ 1") {
		t.Fatalf("deploy version: -1 的 message = %q：不得再出现把 0 说成非法的 “≥ 1”", msg)
	}

	rec = deploy(t, map[string]any{"version": 99})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deploy version: 99 应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
