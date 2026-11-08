package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
)

// TestFireAndEventsShareEntryEdgePath P2.1 的核心承诺：源的「触发」与外部系统的
// 「投递」是**同一条路**。
//
// 收敛之前，/v1/sources/{id}/fire 走 route.DispatchInput.Targets 旁路（源带着
// 自己的目标列表，跳过 edges 查表），而 /v1/events 走入口边。两条路各有各的参数
// 语义，任何一侧的修正都会漏掉另一侧——这正是 §11.5 要消灭的分叉。
//
// 这条用例用**真 store + 真 Dispatcher**跑两次同样的投递：
//   - POST /v1/sources/{id}/fire（源自己发起）
//   - trigger.Service.Ingest（POST /v1/events 的实现，api 层只是解码 + 调它）
//
// 断言两边落到任务行上的参数**完全一致**（都从同一条入口边的 mapping 求值），
// 且入口边是 CreateSource 建的 from_port='*' 那条（源能收任意事件类型）。
func TestFireAndEventsShareEntryEdgePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir + "/entry-edge.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Entry Edge Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new auth service: %v", err)
	}
	uid := store.NewUserID()
	ns, err := db.RegisterUser(ctx, &store.User{
		ID: uid, Username: "u-" + uid, DisplayName: "入口边", CreatedAt: store.Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · EntryEdge")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 下游能力：两个必填字段，其中 title 只能从事件 payload 里取到。
	capDef := &store.CapabilityVersion{
		NamespaceID: ns.ID, Name: "analyze", Version: 1,
		ParamsSchema: json.RawMessage(
			`{"type":"object","required":["title","n"],"properties":{"title":{"type":"string"},"n":{"type":"integer"}}}`),
		Outputs: []string{"done"},
		Source:  store.CapSourceCustom,
	}
	// 实现必须是**脚本形式**（由下面的 mbn 节点执行）。
	//
	// P2b 起 handler: noop 是合法但**不可执行**的占位：派发遇到它会剪枝
	// pruned/no_impl（契约 §3.1 / R1，见 route.ReasonNoImpl），一条任务都建不出来。
	// 而这条用例要验的正好是「源发起的触发能把任务建出来」。
	// 实现形式显式写进 Spec，withSpec 就会原样返回它（它只给缺实现的夹具补占位）。
	capDef.Spec = artifact.Spec{
		Params:  capDef.ParamsSchema,
		Outputs: capDef.Outputs,
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/bash\nexit 0\n"},
			TimeoutSec: 30,
		},
	}
	if err := db.CreateCapability(ctx, withSpec(capDef)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	node := &store.Node{NamespaceID: ns.ID, Name: "mbp"}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, ns.ID, node.ID,
		[]store.CapabilityRef{{Name: "analyze", Version: 1}}); err != nil {
		t.Fatalf("confirm capabilities: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, ns.ID, node.ID, []store.InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("upsert instances: %v", err)
	}

	// 源：目标与参数模板都由**建源**写成入口边（from_port='*'）。
	src := &store.Source{
		NamespaceID: ns.ID, Name: "github-hook", Kind: store.SourceKindGitHub, Enabled: true,
		Targets: []store.SourceTarget{{
			CapabilityID: capDef.ID,
			Params: map[string]any{
				"title": "{{pull_request.title}}",
				"n":     "{{pull_request.number}}",
			},
		}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}

	disp := route.New(db) // Publisher 为 nil：只落库 + 占槽位，任务一定建得出来
	consoleH := &consoleHandler{db: db, auth: svc}
	srcH := &sourceHandler{
		db:               db,
		commander:        &source.Trigger{Store: db, Dispatcher: disp},
		resolveNamespace: consoleH.resolveNamespace,
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sources/{id}/fire", noStore(svc.Sessions().Middleware(http.HandlerFunc(srcH.fire))))

	payload := `{"pull_request":{"title":"修个 bug","number":42}}`

	// ---------- 入口 A：POST /v1/sources/{id}/fire ----------
	req := httptest.NewRequest(http.MethodPost, "/v1/sources/"+src.ID+"/fire?namespaceId="+ns.ID,
		strings.NewReader(`{"type":"pull_request.opened","payload":`+payload+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fire status = %d, body = %s", rr.Code, rr.Body.String())
	}
	fireTasks := decodeFireTaskParams(t, rr.Body.Bytes())
	if len(fireTasks) != 1 {
		t.Fatalf("fire 建了 %d 个任务, want 1: %s", len(fireTasks), rr.Body.String())
	}

	// ---------- 入口 B：POST /v1/events（api 层就是 trigger.Ingest） ----------
	ing := trigger.New(db, disp)
	res, err := ing.Ingest(ctx, &trigger.IngestRequest{
		NamespaceID: ns.ID, Source: src.Name, Type: "pull_request.opened",
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Dispatch == nil || len(res.Dispatch.Tasks) != 1 {
		t.Fatalf("ingest 建了 %d 个任务, want 1（%+v）", len(res.Dispatch.Tasks), res.Dispatch)
	}
	var ingestParams map[string]any
	if err := json.Unmarshal(res.Dispatch.Tasks[0].Params, &ingestParams); err != nil {
		t.Fatalf("decode ingest task params: %v", err)
	}

	// ---------- 两边必须一模一样 ----------
	if got, want := fireTasks[0]["title"], "修个 bug"; got != want {
		t.Errorf("fire 的 title = %#v, want %#v（模板要从事件 payload 求值）", got, want)
	}
	if got, want := fireTasks[0]["n"], float64(42); got != want {
		t.Errorf("fire 的 n = %#v, want %#v", got, want)
	}
	if len(ingestParams) != len(fireTasks[0]) {
		t.Fatalf("两个入口的参数键数不同: fire=%v ingest=%v", fireTasks[0], ingestParams)
	}
	for k, want := range fireTasks[0] {
		if got := ingestParams[k]; got != want {
			t.Errorf("参数 %q 在两个入口上不一致: fire=%#v ingest=%#v（说明还有两条派发路径）", k, want, got)
		}
	}

	// 两条事件都落库了（各自一条 trace），但**目标只建了一次任务**（不会重复派发）
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE source = ?`, src.Name).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 2 {
		t.Errorf("事件行数 = %d, want 2（两个入口各一条）", n)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE capability = ?`, capDef.Name).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 2 {
		t.Errorf("任务行数 = %d, want 2（一次派发一个任务，没有双写）", n)
	}
}

// decodeFireTaskParams 从 fire 响应里取出每个任务的 params。
func decodeFireTaskParams(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Dispatch struct {
			Tasks []struct {
				Params json.RawMessage `json:"params"`
			} `json:"tasks"`
		} `json:"dispatch"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode fire response: %v (body=%s)", err, body)
	}
	out := make([]map[string]any, 0, len(resp.Dispatch.Tasks))
	for _, task := range resp.Dispatch.Tasks {
		var m map[string]any
		if len(task.Params) > 0 {
			if err := json.Unmarshal(task.Params, &m); err != nil {
				t.Fatalf("decode task params: %v", err)
			}
		}
		out = append(out, m)
	}
	return out
}
