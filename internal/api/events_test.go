package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
)

// closeDBForTempDir 是夹具的确定性收口（#259，同 internal/auth / internal/server 的判据）：
// 关库前 InUse==0、关库后 OpenConnections==0、目录里无 *.db-wal|*.db-shm。
//
// 为什么断言而不是只 Close：CI 上出现过 `TempDir RemoveAll cleanup: ... directory not empty`
// （失败现场只剩一个 `*.db-shm`）——那是「关库之后还有活着的 SQLite 连接重建 WAL 文件」。
// 只做 db.Close() 的话，这个竞态会在 t.TempDir 的 RemoveAll 里随机炸掉某个用例；
// 在这里断言就变成确定性的「收尾竞态」失败，并且点名是哪个条件不成立。
func closeDBForTempDir(t *testing.T, db *store.DB, dir string) {
	t.Helper()
	// 断言失败会走 t.Fatalf（跳过余下语句）：defer 兜底关库，避免失败路径上再制造一层 TempDir 噪声。
	defer func() { _ = db.Close() }()
	if n := db.Stats().InUse; n != 0 {
		t.Fatalf("收尾竞态：%d 条 DB 连接仍被 goroutine 持有（应在 db.Close 前等它们返回）", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("收尾竞态：db.Close 失败（有 goroutine 在关库窗口里用库？）: %v", err)
	}
	if n := db.Stats().OpenConnections; n != 0 {
		t.Fatalf("收尾竞态：db.Close 返回后仍有 %d 条连接没释放（有并发使用者）", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读临时目录失败: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") {
			t.Fatalf("收尾竞态：db.Close 后仍残留 WAL 文件 %s（有连接在 RemoveAll 期间重建文件）", name)
		}
	}
}

// eventFixture 是一套带真 SQLite + 真 Dispatcher 的事件入口环境。
//
// 用**真** route.Dispatcher（而不是 mock）是刻意的：这个套件要证明的正是
// 「一次 POST 能落出一条 event 并派发出首批 task」这条端到端链路。
type eventFixture struct {
	db      *store.DB
	nsID    string
	userID  string
	apiKey  string
	handler *EventHandler
	server  *httptest.Server
}

// newEventFixture 建组织 + 用户 + 能力图 + 节点，然后挂上 EventHandler。
func newEventFixture(t *testing.T) *eventFixture {
	t.Helper()
	ctx := context.Background()

	// 收尾走 closeDBForTempDir（#259）：关库钉在 t.TempDir 的 RemoveAll 之前，并断言无并发使用者。
	dir := t.TempDir()
	db, err := store.Open(dir + "/events.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	// 用户 + default 命名空间（RegisterUser 一步到位，同事务播种内置能力）。
	// 组织层已在迁移 14 删除（#204）：命名空间是唯一隔离边界。
	user := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	nsRow, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID := nsRow.ID

	// 目标能力：review@1（有节点声明 → 会建任务）
	//
	// #110：不再需要一个叫 github@1 的"起点能力"。入口边的左端直接是
	// 来源名 "github"，见下面 CreateEdge 的 FromIsSource。
	review := &store.CapabilityVersion{
		NamespaceID:  nsID,
		Name:         "review",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["number"],"properties":{"number":{"type":"number"}}}`),
		Outputs:      []string{"fix"},
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, review); err != nil {
		t.Fatalf("create review capability: %v", err)
	}
	// 入口边：来源 "github" 的 pull_request.opened → review
	if err := db.CreateEdge(ctx, &store.Edge{
		NamespaceID:    nsID,
		FromIsSource:   true,
		FromCapability: "github",
		FromPort:       "pull_request.opened",
		ToCapability:   review.ID,
	}); err != nil {
		t.Fatalf("create entry edge: %v", err)
	}
	// 节点声明 review 并有 2 个槽位
	node := &store.Node{NamespaceID: nsID, Name: "mbp-" + store.NewUserID()}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, nsID, node.ID,
		[]store.CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm capabilities: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, nsID, node.ID,
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

	return &eventFixture{db: db, nsID: nsID, userID: user.ID, apiKey: apiKey, handler: h, server: srv}
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

// postRaw 发请求并**保留** body（错误路径要读错误体；post 会 defer close）。
func (f *eventFixture) postRaw(t *testing.T, body any) (int, []byte) {
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// do 发一个任意方法 + 路径（可带 query）的请求，返回状态码与**保留的** body。
//
// body 为 nil 时不带请求体（GET/DELETE）；否则按 JSON 编码。
func (f *eventFixture) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// ---------- #58 验收：端到端 ----------

// TestEventToDispatch POST /v1/events → 产生 event + 首批 task（#58 验收）。
func TestEventToDispatch(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		NamespaceID: f.nsID,
		Source:      "github", Type: "pull_request.opened",
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
	// #110：origin 现在是"来源名原样回显"，不再是能力 name@version
	if out.Origin != "github" {
		t.Fatalf("origin = %q, want the source name \"github\"", out.Origin)
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
	events, err := f.db.ListEvents(context.Background(), f.nsID, out.TraceID, "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events in db = %d, want 1", len(events))
	}
	tasks, err := f.db.ListTasks(context.Background(), f.nsID, out.TraceID, "", "", 10)
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
		NamespaceID: f.nsID,
		Source:      "github", Type: "pull_request.opened",
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
	tasks, err := f.db.ListTasks(context.Background(), f.nsID, out1.TraceID, "", "", 10)
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
		NamespaceID: f.nsID,
		Source:      "github", Type: "pull_request.closed", // 没有对应边
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

// TestEventUnknownSourceAccepted #110：来源没有任何入口边时**不再 422**。
//
// 旧模型要求 source 必须对应一个"起点能力"，否则 422 + 可用来源列表。
// 来源不再是能力之后这个检查没有依据了 —— 而且它挡住了一个真实场景：
// 先让 webhook 打着（事件照收），再回来建边。事件不该因为这个被丢掉。
//
// 正确行为：200 + 事件入库 + pruned 里记一条 no_edge（回答"为什么没触发"）。
func TestEventUnknownSourceAccepted(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		NamespaceID: f.nsID,
		Source:      "no.such.origin", Type: "issue.created",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200（事件照收，来源可以还没配边）", resp.StatusCode)
	}
	if out.EventID == "" {
		t.Error("eventId is empty: the event WAS stored, response must say so")
	}
	if len(out.Tasks) != 0 {
		t.Errorf("tasks = %d, want 0（没有入口边就派不出去）", len(out.Tasks))
	}
	if out.Origin != "no.such.origin" {
		t.Errorf("origin = %q, want the source name echoed back", out.Origin)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != string(route.ReasonNoEdge) {
		t.Fatalf("pruned = %+v, want one no_edge record", out.Pruned)
	}
}

// TestEventParamsSchemaPruned 参数不过目标能力 schema → 200 + pruned/param_schema。
func TestEventParamsSchemaPruned(t *testing.T) {
	f := newEventFixture(t)

	resp, out := f.post(t, EventIngestRequest{
		NamespaceID: f.nsID,
		Source:      "github", Type: "pull_request.opened",
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
	body, _ := json.Marshal(EventIngestRequest{NamespaceID: f.nsID, Source: "github", Type: "push"})

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

// TestEventNamespaceAuthorization 事件入口的命名空间收口（#204 / 不变量 I10、I11）。
//
// **改写说明（不允许静默削弱，原用例名 TestEventCrossOrgForbidden）**：
// 原用例证明两件事：① 指定别人的 orgId → 403；② 指定别人组织的 namespaceId → 403。
// 组织层在迁移 14 删除后「别人的命名空间」这个概念不存在了（#204 安全语义：
// 本部署任何已登录用户都可以访问任何存在的命名空间），所以 403 分支整体消失。
// 但**必须证明的隔离性质不但没消失，反而更多了**，于是这里换成更强的组合：
//
//	① 未知命名空间 → 404（不再用 403/404 区分「别人的」和「不存在的」——没有别人了）
//	② 另一个真实存在的命名空间：写入**成功**（新语义），但事件只落在那个命名空间，
//	   绝不落到自己那个 —— 证明 namespaceId 真的被当成隔离维度，不是被忽略/被顶掉
//	③ 缺 namespaceId → 400（绝不回退默认命名空间）
//	④ namespaceId=* 在写端点 → 400（`*` 只用于读汇总）
//
// ②③④ 是原用例没有的；① 替换掉原来的两条 403（对应关系写在下面各子用例里）。
func TestEventNamespaceAuthorization(t *testing.T) {
	f := newEventFixture(t)
	ctx := context.Background()

	// 同一部署里的第二个命名空间（不是「别人的」——本部署没有这个概念）
	otherNS, err := f.db.CreateNamespace(ctx, "other-space", "另一个空间")
	if err != nil {
		t.Fatalf("create other namespace: %v", err)
	}

	// ① 未知命名空间 → 404（替换原「别的组织 namespaceId → 403」）
	t.Run("未知 namespaceId → 404", func(t *testing.T) {
		resp, _ := f.post(t, EventIngestRequest{
			NamespaceID: "ns_does_not_exist_xxx", Source: "github", Type: "push",
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404（不存在与「别人的」已无区别）", resp.StatusCode)
		}
	})

	// ② 写入另一个存在的命名空间：允许，但必须落在那个命名空间，不能串到自己这边
	t.Run("写另一个存在的命名空间 → 200 且只落在那个命名空间", func(t *testing.T) {
		resp, body := f.post(t, EventIngestRequest{
			NamespaceID: otherNS.ID, Source: "github", Type: "push",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200（本部署内命名空间共享）; body=%+v", resp.StatusCode, body)
		}
		got, lerr := f.db.ListEvents(ctx, otherNS.ID, "", "", 10)
		if lerr != nil {
			t.Fatalf("list events: %v", lerr)
		}
		if len(got) != 1 {
			t.Fatalf("别的命名空间里应有 1 条事件, got %d", len(got))
		}
		// 关键：不能串到自己的命名空间（namespaceId 必须真的被当隔离维度用）
		mine, lerr := f.db.ListEvents(ctx, f.nsID, "", "", 10)
		if lerr != nil {
			t.Fatalf("list own events: %v", lerr)
		}
		if len(mine) != 0 {
			t.Fatalf("写别的命名空间串到了自己的命名空间: %d 条事件", len(mine))
		}
	})

	// ③ 缺 namespaceId → 400（绝不回退默认命名空间）
	t.Run("缺 namespaceId → 400", func(t *testing.T) {
		status, raw := f.postRaw(t, EventIngestRequest{
			Source: "github", Type: "push",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（不回退默认命名空间）; body=%s", status, raw)
		}
	})

	// ④ 写端点不接受 `*`
	t.Run("namespaceId=* 在写端点 → 400", func(t *testing.T) {
		status, raw := f.postRaw(t, EventIngestRequest{
			NamespaceID: "*", Source: "github", Type: "push",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（`*` 只用于读汇总）; body=%s", status, raw)
		}
	})

	// 收尾：被拒的请求一条事件都没留下（原用例的「不泄漏」意图保留）
	for _, ns := range []string{f.nsID, otherNS.ID} {
		got, lerr := f.db.ListEvents(ctx, ns, "", "", 10)
		if lerr != nil {
			t.Fatalf("list events: %v", lerr)
		}
		want := 0
		if ns == otherNS.ID {
			want = 1 // 只有 ② 那一条成功的
		}
		if len(got) != want {
			t.Fatalf("ns %s 有 %d 条事件, want %d（被拒的请求不该留痕）", ns, len(got), want)
		}
	}
}

// TestEventList GET /v1/events 按 trace 过滤，空列表返回 []。
func TestEventList(t *testing.T) {
	f := newEventFixture(t)

	// 先造两条不同 trace 的事件
	_, a := f.post(t, EventIngestRequest{NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`)})
	_, b := f.post(t, EventIngestRequest{NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":2}`)})

	get := func(t *testing.T, query string) (int, map[string]any) {
		t.Helper()
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
	// 列表端点同样必填 namespaceId（契约 §6.2）：测试里显式带上本组织的默认命名空间。
	base := "?namespaceId=" + f.nsID

	// 全部
	status, out := get(t, base)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	all, _ := out["events"].([]any)
	if len(all) != 2 {
		t.Fatalf("all events = %d, want 2", len(all))
	}

	// 按 trace 过滤 → 1 条
	status, out = get(t, base+"&traceId="+a.TraceID)
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
	status, out = get(t, base+"&traceId=trace_does_not_exist")
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

// TestEventPruneNotInEventStream 剪枝不进事件流，但按 trace 查得到（#142 验收）。
//
// 两边都要证：
//   - 事件流（GET /v1/events 不带 traceId）回答「外部世界发生了什么」，
//     混进剪枝会把真实事件淹掉；
//   - 按 trace 查回答「这条链为什么这样」，剪枝必须在那儿——
//     否则「为什么没跑」依然无解，Issue #142 就没修好。
func TestEventPruneNotInEventStream(t *testing.T) {
	f := newEventFixture(t)

	// pull_request.closed 没有对应边 → 一条 no_edge 剪枝
	resp, out := f.post(t, EventIngestRequest{
		NamespaceID: f.nsID,
		Source:      "github", Type: "pull_request.closed",
		Payload: json.RawMessage(`{"number":9}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != string(route.ReasonNoEdge) {
		t.Fatalf("pruned = %+v, want 一条 no_edge", out.Pruned)
	}

	// 事件流：只回根事件
	req, _ := http.NewRequest(http.MethodGet,
		f.server.URL+"/v1/events?namespaceId="+f.nsID+"&traceId=", nil)
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", streamResp.StatusCode)
	}
	var stream struct {
		Events []store.Event `json:"events"`
	}
	if err := json.NewDecoder(streamResp.Body).Decode(&stream); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stream.Events) != 1 {
		t.Fatalf("事件流 = %d 条, want 1（只有根事件，剪枝不该混进来）", len(stream.Events))
	}
	if stream.Events[0].Source == route.PruneEventSource {
		t.Fatalf("事件流里出现了剪枝行: %+v", stream.Events[0])
	}

	// trace 视图：剪枝在里面（库层直查，trace 页走的正是这条路径）
	events, err := f.db.ListEvents(context.Background(), f.nsID, out.TraceID, "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("trace %s 的事件 = %d 条, want 2（根事件 + 剪枝）", out.TraceID, len(events))
	}
	var prune *store.Event
	for _, e := range events {
		if e.Source == route.PruneEventSource {
			prune = e
		}
	}
	if prune == nil {
		t.Fatal("按 trace 查不到剪枝行（#142 的核心诉求没实现）")
	}
	if prune.Type != route.PruneEventType(route.ReasonNoEdge) {
		t.Fatalf("剪枝 type = %q, want %q", prune.Type, route.PruneEventType(route.ReasonNoEdge))
	}
	var payload route.PruneEventPayload
	if err := json.Unmarshal(prune.Payload, &payload); err != nil {
		t.Fatalf("剪枝 payload 不是合法 JSON: %v", err)
	}
	if payload.FromCapability != "github" {
		t.Errorf("payload.fromCapability = %q, want github（入口边时是来源名）", payload.FromCapability)
	}
	if payload.Port != "pull_request.closed" {
		t.Errorf("payload.port = %q, want pull_request.closed", payload.Port)
	}
	if payload.Detail == "" {
		t.Error("payload.detail 为空（要能回答「为什么没跑」）")
	}
	if prune.IdemKey != nil {
		t.Errorf("剪枝带了 idem_key = %v, want NULL", *prune.IdemKey)
	}
	// 剪枝记录也是事件，必须落在同一个命名空间里（否则 trace 视图会跨命名空间漏水）
	if prune.NamespaceID != f.nsID {
		t.Errorf("剪枝行的 namespaceId = %q, want %q", prune.NamespaceID, f.nsID)
	}
}

// ---------- #162 验收：命名空间必填（契约 §6.2，绝不回退默认命名空间） ----------

// TestEventNamespaceRequired POST 缺 namespaceId → 400，且**不落库**。
//
// 这是 #162 最容易被写错的一条：回退默认命名空间能让所有用例变绿，
// 但等于把「调用方写错了」藏成「悄悄写进了 default」——是最难查的串数据。
// 所以除了断言状态码，还要断言库里一条事件都没有。
func TestEventNamespaceRequired(t *testing.T) {
	f := newEventFixture(t)

	status, raw := f.postRaw(t, EventIngestRequest{
		Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（缺 namespaceId）; body=%s", status, raw)
	}

	events, err := f.db.ListEvents(context.Background(), f.nsID, "", "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("缺 namespaceId 的请求落库了 %d 条事件（回退了默认命名空间？）", len(events))
	}
}

// TestEventNamespaceFromQuery body 不带但 query 带 namespaceId → 200。
//
// 契约 §4 允许两个来源（query 与 body），body 优先；两者都没有才 400。
func TestEventNamespaceFromQuery(t *testing.T) {
	f := newEventFixture(t)

	status, raw := f.do(t, http.MethodPost, "/v1/events?namespaceId="+f.nsID, EventIngestRequest{
		Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`),
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200（query 提供了 namespaceId）; body=%s", status, raw)
	}
	events, err := f.db.ListEvents(context.Background(), f.nsID, "", "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].NamespaceID != f.nsID {
		t.Fatalf("事件没落在解析出的命名空间上: %+v", events)
	}
}

// TestEventNamespaceStarRejected `*` 是读端点专用的跨命名空间标记，写端点 → 400。
func TestEventNamespaceStarRejected(t *testing.T) {
	f := newEventFixture(t)

	status, raw := f.postRaw(t, EventIngestRequest{
		NamespaceID: store.AllNamespaces, Source: "github", Type: "push",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（写端点不接受 namespaceId=*）; body=%s", status, raw)
	}
}

// TestEventNamespaceNotFound 不存在的 namespaceId → 404（不是 400、更不是默认命名空间）。
func TestEventNamespaceNotFound(t *testing.T) {
	f := newEventFixture(t)

	status, raw := f.postRaw(t, EventIngestRequest{
		NamespaceID: "ns_does_not_exist", Source: "github", Type: "push",
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, raw)
	}
}

// TestEventListNamespaceRequired GET 缺 namespaceId → 400（列表端点同样必填）。
func TestEventListNamespaceRequired(t *testing.T) {
	f := newEventFixture(t)

	status, raw := f.do(t, http.MethodGet, "/v1/events", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（缺 namespaceId）; body=%s", status, raw)
	}
}

// TestEventListAllNamespaces namespaceId=* 跨命名空间合并，再按时间截断。
func TestEventListAllNamespaces(t *testing.T) {
	f := newEventFixture(t)
	ctx := context.Background()

	second, err := f.db.CreateNamespace(ctx, "staging", "预发")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}

	// 两个命名空间各一条事件（第二条在 staging 里没有任何边 → 只落库 + 一条 no_edge 剪枝）
	if status, raw := f.do(t, http.MethodPost, "/v1/events", EventIngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`),
	}); status != http.StatusOK {
		t.Fatalf("default ns post status = %d; body=%s", status, raw)
	}
	if status, raw := f.do(t, http.MethodPost, "/v1/events", EventIngestRequest{
		NamespaceID: second.ID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":2}`),
	}); status != http.StatusOK {
		t.Fatalf("staging ns post status = %d; body=%s", status, raw)
	}

	list := func(query string) []store.Event {
		t.Helper()
		status, raw := f.do(t, http.MethodGet, "/v1/events"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d for %q; body=%s", status, query, raw)
		}
		var out struct {
			Events []store.Event `json:"events"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %q: %v", query, err)
		}
		return out.Events
	}

	// 单命名空间：各 1 条，且 namespaceId 对得上
	only := list("?namespaceId=" + f.nsID)
	if len(only) != 1 || only[0].NamespaceID != f.nsID {
		t.Fatalf("default ns 的事件 = %+v, want 1 条且 namespaceId=%s", only, f.nsID)
	}
	only = list("?namespaceId=" + second.ID)
	if len(only) != 1 || only[0].NamespaceID != second.ID {
		t.Fatalf("staging ns 的事件 = %+v, want 1 条且 namespaceId=%s", only, second.ID)
	}

	// namespaceId=*：两个命名空间合并 → 2 条
	all := list("?namespaceId=" + store.AllNamespaces)
	if len(all) != 2 {
		t.Fatalf("namespaceId=* 返回 %d 条, want 2", len(all))
	}
	seen := map[string]int{}
	for _, e := range all {
		seen[e.NamespaceID]++
	}
	if seen[f.nsID] != 1 || seen[second.ID] != 1 {
		t.Fatalf("合并结果按命名空间分布 = %v, want 每个 1 条", seen)
	}

	// limit 截断发生在**合并之后**（不是每个命名空间各取 limit）
	limited := list("?namespaceId=" + store.AllNamespaces + "&limit=1")
	if len(limited) != 1 {
		t.Fatalf("namespaceId=* &limit=1 返回 %d 条, want 1（合并后截断）", len(limited))
	}
}

// TestEventNamespaceErrorMapping 是错误 → 状态码的直测（#162 验收）。
//
// 为什么要直测映射而不只走 HTTP：I11 跨命名空间引用（*store.CrossNamespaceError）
// 在 /v1/events 这条路上很难自然触发（建边时就被 store 拦下了），
// 但契约要求 handler 用 errors.As 把它翻成 400——这条映射必须自己被钉住。
func TestEventNamespaceErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "跨命名空间引用 → 400",
			err:  fmt.Errorf("ingest: insert event: %w", &store.CrossNamespaceError{What: "edges.to_capability", Want: "ns_a", Got: "ns_b"}),
			want: http.StatusBadRequest,
		},
		{
			name: "缺 namespaceId → 400",
			err:  fmt.Errorf("ingest: empty namespace_id: %w", store.ErrNamespaceRequired),
			want: http.StatusBadRequest,
		},
		{
			name: "命名空间不存在 → 404",
			err:  fmt.Errorf("get namespace: %w", store.ErrNamespaceNotFound),
			want: http.StatusNotFound,
		},
		{
			// #204 删掉组织层后 ErrNamespaceForbidden 不存在了（没有「别人的命名空间」），
			// 收口换来的是「未登录」这一条：AuthorizeNamespace 在 userID 为空/用户不存在时
			// 返回 ErrAuthRequired。这条分支同样必须被钉住，否则收口一旦漏判就是静默放行。
			name: "未登录（收口 ErrAuthRequired）→ 401",
			err:  fmt.Errorf("authorize namespace: %w", store.ErrAuthRequired),
			want: http.StatusUnauthorized,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !writeNamespaceError(rec, c.err) {
				t.Fatalf("writeNamespaceError 没认出来: %v", c.err)
			}
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}

	// 非命名空间错误：不写响应，交给调用方按原逻辑处理（不能把 500 也吞成 400）
	rec := httptest.NewRecorder()
	if writeNamespaceError(rec, errors.New("db is locked")) {
		t.Fatal("无关错误不该被命名空间映射处理")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("无关错误不该写响应体: %s", rec.Body.String())
	}
	// nil 错误：什么都不做（ingest 把它串在正常流程里当「是否是这类错误」用）
	rec = httptest.NewRecorder()
	if writeNamespaceError(rec, nil) {
		t.Fatal("nil 错误不该被处理")
	}
}

// ---------- #236 验收：limit 口径统一（硬顶 500 + 回显生效值） ----------

// seedEvents 直接往库里灌 n 条事件（**不走** dispatcher）。
//
// 为什么要绕开 POST：这里要证明的是「列表端点的 limit 口径」，需要的是足够长的
// 事件流（> 硬顶），而派发链路（边/节点/任务）与它无关、只会让夹具变脆。
// created_at 递增：顺序确定，才能断言「返回的是最新的一批」。
func (f *eventFixture) seedEvents(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	base := store.Now() - int64(n)
	for i := 0; i < n; i++ {
		e := &store.Event{
			NamespaceID: f.nsID,
			TraceID:     "trace_seed_" + strconv.Itoa(i),
			Source:      "github",
			Type:        "push",
			Payload:     json.RawMessage(`{"n":` + strconv.Itoa(i) + `}`),
			CreatedAt:   base + int64(i),
		}
		if _, err := f.db.InsertEvent(ctx, e); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

// listEvents 发 GET /v1/events，返回 (状态码, 事件, 响应里的 limit 字段)。
//
// limit 用指针接：字段**缺失**与「值恰好是默认值」必须能区分开——#236 要证明
// 的正是「生效值被回显」，字段缺失时不能悄悄当成通过。
func (f *eventFixture) listEvents(t *testing.T, query string) (int, []store.Event, *int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/events"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Logf("非 200：status=%d body=%s", resp.StatusCode, raw)
		return resp.StatusCode, nil, nil
	}
	var out struct {
		Events []store.Event `json:"events"`
		Limit  *int          `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out.Events, out.Limit
}

// TestEventListLimitCapHandlerClamps GET /v1/events 的 limit 收口（#236）。
//
// 病根：store.ListEvents 对 `limit > 500` **折回默认值 50**（不是钳到 500），
// 而 handler 曾把 1000 当上限往下传 ⇒ `limit=501..1000` 返回 50 条。
// 这里钉住三件事：
//   - 超上限被钳到 500（不是 50，也不是原样）；
//   - 响应**回显生效值** `limit`（调用方才能分辨「要到了」与「被截断」）；
//   - 缺省 / 非法 / ≤0 仍回 100（沿用宽松语义，不新增 400）。
func TestEventListLimitCapHandlerClamps(t *testing.T) {
	f := newEventFixture(t)
	const seeded = 520 // > 硬顶 500：能同时观察「钳到 500」与「500 < 520」
	f.seedEvents(t, seeded)

	cases := []struct {
		name  string
		query string // 追加在 ?namespaceId=<ns> 之后的片段
		want  int
	}{
		{"limit=1000 → 硬顶 500", "&limit=1000", 500},
		{"limit=501（刚过上限）→ 500", "&limit=501", 500},
		{"limit=520（超过已灌数）→ 500", "&limit=520", 500},
		{"limit=500（正好硬顶）→ 500", "&limit=500", 500},
		{"limit=100 → 100", "&limit=100", 100},
		{"limit 缺省 → 100", "", 100},
		{"limit=abc（非整数）→ 100", "&limit=abc", 100},
		{"limit=0 → 100", "&limit=0", 100},
		{"limit=-5 → 100", "&limit=-5", 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, events, echoed := f.listEvents(t, "?namespaceId="+f.nsID+c.query)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if len(events) != c.want {
				t.Fatalf("返回 %d 条, want %d（#236：超上限必须钳到 500，不能折回 50）", len(events), c.want)
			}
			if echoed == nil {
				t.Fatal("响应里没有 limit 字段：生效值必须回显（#236）")
			}
			if *echoed != c.want {
				t.Fatalf("响应 limit = %d, want %d（回显的是**生效值**）", *echoed, c.want)
			}
			// 钳到 500 时必须是**最新**的 500 条，不是随便 500 条
			if len(events) > 0 && c.want >= seeded {
				if events[0].CreatedAt < events[len(events)-1].CreatedAt {
					t.Fatal("事件不是 created_at DESC（最新在前）")
				}
			}
		})
	}

	// namespaceId=* 走 listAcrossNamespaces：同一套 limit 口径（传下去的已夹住），
	// 否则跨命名空间汇总会重演 #236（每个命名空间各回 50 条）。
	t.Run("namespaceId=* + limit=1000 → 500", func(t *testing.T) {
		status, events, echoed := f.listEvents(t, "?namespaceId="+store.AllNamespaces+"&limit=1000")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if len(events) != 500 {
			t.Fatalf("namespaceId=* 返回 %d 条, want 500", len(events))
		}
		if echoed == nil || *echoed != 500 {
			t.Fatalf("响应 limit = %v, want 500", echoed)
		}
	})
}
