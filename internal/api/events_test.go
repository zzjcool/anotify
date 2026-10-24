package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
)

// eventFixture 是一套带真 SQLite + 真 Dispatcher 的事件入口环境。
//
// 用**真** route.Dispatcher（而不是 mock）是刻意的：这个套件要证明的正是
// 「一次 POST 能落出一条 event 并派发出首批 task」这条端到端链路。
type eventFixture struct {
	db      *store.DB
	orgID   string
	userID  string
	apiKey  string
	handler *EventHandler
	server  *httptest.Server
}

// newEventFixture 建组织 + 用户 + 能力图 + 节点，然后挂上 EventHandler。
func newEventFixture(t *testing.T) *eventFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 用户 + 组织（RegisterUserWithOrg 一步到位，owner 关系会自动建）。
	// 第二个参数是 firstUserAdmin：库是空的，这个用户就是管理员。
	user := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	org, err := db.RegisterUserWithOrg(ctx, user, true)
	if err != nil {
		t.Fatalf("register user with org: %v", err)
	}

	// origin 能力：github@1，出口 pull_request.opened
	origin := &store.CapabilityVersion{
		OrgID: org.ID, Name: "github", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"pull_request.opened"},
		Source:       store.CapSourceBuiltin,
	}
	if err := db.CreateCapability(ctx, origin); err != nil {
		t.Fatalf("create origin capability: %v", err)
	}
	// 目标能力：review@1（有节点声明 → 会建任务）
	review := &store.CapabilityVersion{
		OrgID: org.ID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["number"],"properties":{"number":{"type":"number"}}}`),
		Outputs:      []string{"fix"},
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, review); err != nil {
		t.Fatalf("create review capability: %v", err)
	}
	// 出口表：github 的 pull_request.opened → review
	if err := db.CreateEdge(ctx, &store.Edge{
		OrgID: org.ID, FromCapability: origin.ID,
		FromPort: "pull_request.opened", ToCapability: review.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	// 节点声明 review 并有 2 个槽位
	node := &store.Node{OrgID: org.ID, Name: "mbp-" + store.NewUserID()}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, org.ID, node.ID,
		[]store.CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm capabilities: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, org.ID, node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}}); err != nil {
		t.Fatalf("upsert instances: %v", err)
	}

	// 真 Dispatcher（route 包）+ 真 trigger.Service
	disp := route.New(db)
	svc := trigger.New(db, disp)

	apiKey := "ant_test_" + store.NewSessionID()
	keys := authn.KeyValidatorFunc(func(ctx context.Context, k string) (string, []string, error) {
		if k == apiKey {
			return user.ID, []string{authn.ScopeNotifySend, authn.ScopeNotifyReceive}, nil
		}
		return "", nil, authn.ErrForbidden
	})

	h := &EventHandler{Store: db, Keys: keys, Triggers: svc}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &eventFixture{db: db, orgID: org.ID, userID: user.ID, apiKey: apiKey, handler: h, server: srv}
}

// post 发一个 POST /v1/events 请求。
func (f *eventFixture) post(t *testing.T, body any) (*http.Response, EventIngestResponse) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/events", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var out EventIngestResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp, out
}

// ---------- #58 验收：端到端 ----------

// TestEventToDispatch POST /v1/events → 产生 event + 首批 task（#58 验收）。
func TestEventToDispatch(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":42}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.EventID == "" {
		t.Fatal("no eventId in response")
	}
	if out.TraceID == "" {
		t.Fatal("no traceId in response")
	}
	if !out.Inserted {
		t.Fatal("inserted = false on first request")
	}
	if out.Origin != "github@1" {
		t.Fatalf("origin = %q, want github@1", out.Origin)
	}
	// 首批任务建出来了
	if len(out.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(out.Tasks))
	}
	if out.Tasks[0].Capability != "review" {
		t.Fatalf("task capability = %q, want review", out.Tasks[0].Capability)
	}
	if out.Tasks[0].Status != store.TaskAssigned {
		t.Fatalf("task status = %q, want assigned", out.Tasks[0].Status)
	}

	// 库里确实有 event + task
	events, err := f.db.ListEvents(context.Background(), f.orgID, out.TraceID, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events in db = %d, want 1", len(events))
	}
	tasks, err := f.db.ListTasks(context.Background(), f.orgID, out.TraceID, "", "", 10)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks in db = %d, want 1", len(tasks))
	}
	if tasks[0].TraceID != out.TraceID {
		t.Fatalf("task trace_id = %q, want %q", tasks[0].TraceID, out.TraceID)
	}
}

// TestEventIdempotentHTTP 同一 idemKey 两次 → 第二次 inserted=false、同一 trace，且不重复派发。
func TestEventIdempotentHTTP(t *testing.T) {
	f := newEventFixture(t)
	body := EventIngestRequest{
		Source: "github", Type: "pull_request.opened",
		IdemKey: "delivery-xyz",
		Payload: json.RawMessage(`{"number":7}`),
	}

	resp1, out1 := f.post(t, body)
	if resp1.StatusCode != http.StatusOK || !out1.Inserted {
		t.Fatalf("first: status=%d inserted=%v", resp1.StatusCode, out1.Inserted)
	}
	resp2, out2 := f.post(t, body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (not 409)", resp2.StatusCode)
	}
	if out2.Inserted {
		t.Fatal("second request reported inserted=true")
	}
	if out2.EventID != out1.EventID || out2.TraceID != out1.TraceID {
		t.Fatalf("idempotent hit returned different ids: %+v vs %+v", out1, out2)
	}
	// 幂等命中不重复派发
	if len(out2.Tasks) != 0 {
		t.Fatalf("idempotent hit dispatched %d tasks, want 0", len(out2.Tasks))
	}
	// 库里只有一个任务
	tasks, err := f.db.ListTasks(context.Background(), f.orgID, out1.TraceID, "", "", 10)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d after idempotent replay, want 1", len(tasks))
	}
}

// TestEventNoEdgePruned 出口表 0 行 → 200 + pruned/no_edge，不建任务。
func TestEventNoEdgePruned(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		Source: "github", Type: "pull_request.closed", // 没有对应边
		Payload: json.RawMessage(`{"number":1}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("tasks = %d, want 0", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != string(route.ReasonNoEdge) {
		t.Fatalf("pruned = %+v, want one no_edge", out.Pruned)
	}
}

// TestEventUnknownOriginStill200 没有 origin 能力 → 200（事件收下了），但无任务。
func TestEventUnknownOriginStill200(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		Source: "jira", Type: "issue.created", // jira 没定义
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown origin is a config problem, not client error)", resp.StatusCode)
	}
	if out.EventID == "" {
		t.Fatal("event was not created")
	}
	if out.Origin != "" {
		t.Fatalf("origin = %q, want empty", out.Origin)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("tasks = %d, want 0", len(out.Tasks))
	}
}

// TestEventParamsSchemaPruned 参数不过目标能力 schema → 200 + pruned/param_schema。
func TestEventParamsSchemaPruned(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"title":"no number field"}`), // 缺 required number
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("tasks = %d, want 0", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != string(route.ReasonParamSchema) {
		t.Fatalf("pruned = %+v, want param_schema", out.Pruned)
	}
}

// TestEventAuthRequired 无 key / 错 key → 401/403。
func TestEventAuthRequired(t *testing.T) {
	f := newEventFixture(t)
	body, _ := json.Marshal(EventIngestRequest{Source: "github", Type: "push"})

	t.Run("no key → 401", func(t *testing.T) {
		resp, err := http.Post(f.server.URL+"/v1/events", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("bad key → 401/403", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/events", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer wrong-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 401 or 403", resp.StatusCode)
		}
	})
}

// TestEventCrossOrgForbidden 指定别人的 orgId → 403（不变量 I10）。
func TestEventCrossOrgForbidden(t *testing.T) {
	f := newEventFixture(t)
	// 另一个组织（当前用户不是成员）
	other, err := f.db.CreateOrg(context.Background(), "other-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}

	resp, _ := f.post(t, EventIngestRequest{
		Source: "github", Type: "push", OrgID: other.ID,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (cross-org write must be rejected)", resp.StatusCode)
	}
	// 不该有事件落进那个组织
	events, err := f.db.ListEvents(context.Background(), other.ID, "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("cross-org write leaked %d events", len(events))
	}
}

// TestEventList GET /v1/events 按 trace 过滤，空列表返回 []。
func TestEventList(t *testing.T) {
	f := newEventFixture(t)

	// 先造两条不同 trace 的事件
	_, a := f.post(t, EventIngestRequest{Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`)})
	_, b := f.post(t, EventIngestRequest{Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":2}`)})

	get := func(query string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/events"+query, nil)
		req.Header.Set("Authorization", "Bearer "+f.apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return resp.StatusCode, out
	}

	// 全部
	status, out := get("")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	all, _ := out["events"].([]any)
	if len(all) != 2 {
		t.Fatalf("all events = %d, want 2", len(all))
	}

	// 按 trace 过滤 → 1 条
	status, out = get("?traceId=" + a.TraceID)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	filtered, _ := out["events"].([]any)
	if len(filtered) != 1 {
		t.Fatalf("filtered events = %d, want 1", len(filtered))
	}
	if ev, _ := filtered[0].(map[string]any); ev["traceId"] != a.TraceID {
		t.Fatalf("wrong event returned: %v", ev)
	}
	_ = b

	// 不存在的 trace → 空数组（不是 null）
	status, out = get("?traceId=trace_does_not_exist")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	raw, ok := out["events"].([]any)
	if !ok {
		t.Fatalf("events = %v (%T), want []", out["events"], out["events"])
	}
	if len(raw) != 0 {
		t.Fatalf("events = %d, want 0", len(raw))
	}
}

// TestEventMethodNotAllowed PUT /v1/events → 405。
func TestEventMethodNotAllowed(t *testing.T) {
	f := newEventFixture(t)
	req, _ := http.NewRequest(http.MethodPut, f.server.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
