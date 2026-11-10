package api

// 本文件是 v4 **事件投递入口**的验收（api-conventions §8.1 / source.md §5.1）：
//
//	POST /v1/namespaces/{namespace}/events
//
// v3 的 /v1/events 套件（namespaceId 走 query/body、GET 列表、跨 ns 归并、limit 夹取）
// 随端点一起退役：列表是**资源**（GET 同一路径由任务/事件资源域注册），不在本包。
// 留下的五组断言只钉投递入口自己的契约：
//
//	① 回执形状（W6：eventId / traceId / duplicate / tasks[] / pruned[]，空列表是 []）
//	② 幂等：同一 id 重复投递 → duplicate=true + **原** event/trace，不重复派发
//	③ source 必填（不回退默认源）
//	④ 未授权（缺 key / 错 key / scope 不足）
//	⑤ 命名空间边界（不存在 → 404；`*` → 400；别的 ns 的源在这里不可见 → 404）
//
// 用真 SQLite + 真 source.Service（不是 mock）：回执是 store 事务的**投影**，
// mock 掉 Ingest 就只能证明 handler 会转发参数，证明不了「eventId/traceId 真的来自
// 那一条事件」。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// eventIngressFixture 是一套跑起来的事件投递入口（真 DB + 真 mux + 真 httptest）。
type eventIngressFixture struct {
	db      *store.DB
	nsID    string
	nsSlug  string
	otherNS string

	otherSlug string
	userID    string
	apiKey    string
	handler   *EventHandler
	server    *httptest.Server
}

// newEventIngressFixture 建两个命名空间、一个能力、两个同名源（用于证明 ns 隔离）。
func newEventIngressFixture(t *testing.T) *eventIngressFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	userID, err := db.InsertUser(ctx, "", "ingress-"+store.NewUserID(), "Ingress")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	other, err := db.CreateNamespace(ctx, "staging", "Staging")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// 两个命名空间各自一个同名能力：源 targets 必须能解析到**本 ns** 的能力才会建任务
	// （store 在建源时就校验出口，跨 ns 引用不接受），所以能力也要成对建。
	for _, nsID := range []string{ns.ID, other.ID} {
		if err := db.CreateCapability(ctx, &store.CapabilityVersion{
			NamespaceID: nsID, Name: "analyze", Version: 1,
			Spec: artifact.Spec{
				Params: json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`),
				Run: artifact.Run{
					Entrypoint: "run.sh",
					Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
				},
			},
		}); err != nil {
			t.Fatalf("create capability in %s: %v", nsID, err)
		}
	}
	// 两个命名空间里都有同名源：跨 ns 的「同一个名字」不能互相可见。
	createIngressSource(t, db, ns.ID, "manual-1")
	createIngressSource(t, db, other.ID, "manual-1")

	apiKey := "ant_ingress_" + store.NewSessionID()
	keys := authn.KeyValidatorFunc(func(_ context.Context, k string) (string, []string, error) {
		switch k {
		case apiKey:
			return userID, []string{authn.ScopeNotifySend}, nil
		case "ant_read_only":
			// 有 key 但没有 notify:send：403 而不是 401（换 key 与加 scope 是两件事）。
			return userID, []string{authn.ScopeNotifyReceive}, nil
		}
		return "", nil, authn.ErrForbidden
	})

	handler := NewEventHandler(source.New(db, route.New(db)), keys)
	// 生产装配里这个解析器由 server 包注入（namespace_scope.go 的唯一判据）：
	// 路径段可能是 slug（canonical）或 store id（CLI/手写）。
	handler.ResolveNamespace = func(ctx context.Context, uid, raw string) (string, error) {
		// 与生产实现逐条对齐：`*` 只允许出现在跨 ns 读端点，投递入口一律 400。
		if strings.TrimSpace(raw) == store.AllNamespaces {
			return "", store.ErrNamespaceRequired
		}
		all, lerr := db.ListNamespaces(ctx)
		if lerr != nil {
			return "", lerr
		}
		for _, cand := range all {
			if cand.Slug == raw || cand.ID == raw {
				if aerr := db.AuthorizeNamespace(ctx, uid, cand.ID); aerr != nil {
					return "", aerr
				}
				return cand.ID, nil
			}
		}
		return "", store.ErrNamespaceNotFound
	}
	mux := http.NewServeMux()
	// 路径参数名与 mux.go 的注册逐字一致（{namespace}）。
	mux.Handle("POST /v1/namespaces/{namespace}/events", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &eventIngressFixture{
		db: db, nsID: ns.ID, nsSlug: ns.Slug, otherNS: other.ID, otherSlug: other.Slug, userID: userID,
		apiKey: apiKey, handler: handler, server: server,
	}
}

// createIngressSource 建一个 manual 源：targets 一条边 → analyze，参数从 payload 取。
func createIngressSource(t *testing.T, db *store.DB, namespaceID, name string) {
	t.Helper()
	src := &store.Source{
		NamespaceID: namespaceID, Name: name, Kind: sourcespec.KindManual,
		Config:  json.RawMessage(`{"payload":{"env":"prod"}}`),
		Targets: []sourcespec.Target{{Capability: "analyze", Params: map[string]any{"env": "$(payload.env)"}}},
		Enabled: true,
	}
	if err := db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create source %s: %v", name, err)
	}
}

// post 发一次投递，返回状态码与**保留的** body（错误体要读 reason）。
//
// key 传空串 = 不带 Authorization（未授权用例）。
func (f *eventIngressFixture) post(t *testing.T, namespace, key string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost,
		f.server.URL+"/v1/namespaces/"+namespace+"/events", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
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

// decodeReceipt 解出回执（成功路径）。
func decodeReceipt(t *testing.T, raw []byte) source.Receipt {
	t.Helper()
	var out source.Receipt
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode receipt %s: %v", raw, err)
	}
	return out
}

// decodeStatus 解出 Status（错误路径）。
func decodeStatus(t *testing.T, raw []byte) statusDoc {
	t.Helper()
	var out statusDoc
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode status %s: %v", raw, err)
	}
	return out
}

// ---------- ① 回执形状 ----------

// TestEventIngressReceipt 成功回执：W6 五个字段齐全、tasks 一条、空列表是 [] 不是 null。
func TestEventIngressReceipt(t *testing.T) {
	f := newEventIngressFixture(t)

	code, raw := f.post(t, f.nsID, f.apiKey, map[string]any{
		"source":  "manual-1",
		"type":    "deploy.finished",
		"payload": map[string]any{"env": "staging"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", code, raw)
	}

	// 空列表必须是 `[]`：`null` 会让前端把「确实没有」误判成「字段缺失」（I12）。
	if strings.Contains(string(raw), `"pruned":null`) {
		t.Fatalf("pruned 不能是 null（body=%s）", raw)
	}

	receipt := decodeReceipt(t, raw)
	if receipt.EventID == "" || receipt.TraceID == "" {
		t.Fatalf("回执必须有 eventId / traceId：%s", raw)
	}
	if receipt.Duplicate {
		t.Fatalf("首次投递不是重复投递：%s", raw)
	}
	if len(receipt.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1 条（源声明了一条到 analyze 的边）", receipt.Tasks)
	}
	task := receipt.Tasks[0]
	if task.TaskID == "" || task.Capability != "analyze" || task.Version != 1 {
		t.Fatalf("task 回执 = %+v, want {taskId:*, capability:analyze, version:1}", task)
	}
	if len(receipt.Pruned) != 0 {
		t.Fatalf("pruned = %+v, want 空", receipt.Pruned)
	}

	// eventId 必须真的落在那条事件上（回执不是 handler 自己编的 id）。
	event, err := f.db.GetEvent(context.Background(), f.nsID, receipt.EventID)
	if err != nil {
		t.Fatalf("按回执 eventId 读事件: %v", err)
	}
	if event.TraceID != receipt.TraceID {
		t.Fatalf("事件 traceId = %q, 回执 = %q", event.TraceID, receipt.TraceID)
	}
	if event.Source != "manual-1" {
		t.Fatalf("事件 source = %q, want manual-1", event.Source)
	}
	if event.Type != "deploy.finished" {
		t.Fatalf("事件 type = %q, want deploy.finished（body.type 生效）", event.Type)
	}
}

// TestEventIngressTypeFallsBackToSourceName type 省略 → 事件类型回落源名（source.md §5）。
func TestEventIngressTypeFallsBackToSourceName(t *testing.T) {
	f := newEventIngressFixture(t)
	code, raw := f.post(t, f.nsID, f.apiKey, map[string]any{
		"source": "manual-1", "payload": map[string]any{"env": "x"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", code, raw)
	}
	receipt := decodeReceipt(t, raw)
	event, err := f.db.GetEvent(context.Background(), f.nsID, receipt.EventID)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if event.Type != "manual-1" {
		t.Fatalf("事件 type = %q, want manual-1（缺 type 时回落源名）", event.Type)
	}
}

// ---------- ② 幂等 ----------

// TestEventIngressDuplicate 同一 id 重复投递 → duplicate=true 且返回**原** event/trace。
func TestEventIngressDuplicate(t *testing.T) {
	f := newEventIngressFixture(t)
	body := map[string]any{
		"source": "manual-1", "id": "delivery-42", "payload": map[string]any{"env": "prod"},
	}

	code, raw := f.post(t, f.nsID, f.apiKey, body)
	if code != http.StatusOK {
		t.Fatalf("第一次投递 status = %d, want 200（body=%s）", code, raw)
	}
	first := decodeReceipt(t, raw)
	if first.Duplicate {
		t.Fatalf("第一次投递不该是 duplicate：%s", raw)
	}

	code, raw = f.post(t, f.nsID, f.apiKey, body)
	if code != http.StatusOK {
		t.Fatalf("重复投递 status = %d, want 200（body=%s）", code, raw)
	}
	second := decodeReceipt(t, raw)
	if !second.Duplicate {
		t.Fatalf("同一 id 第二次投递必须 duplicate=true：%s", raw)
	}
	if second.EventID != first.EventID || second.TraceID != first.TraceID {
		t.Fatalf("重复投递必须回原 event/trace：first=(%s,%s) second=(%s,%s)",
			first.EventID, first.TraceID, second.EventID, second.TraceID)
	}

	// 只落一条事件（重复投递不产生第二条 event 行）。
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE namespace_id = ? AND source = ?`,
		f.nsID, "manual-1").Scan(&count); err != nil {
		t.Fatalf("统计 events: %v", err)
	}
	if count != 1 {
		t.Fatalf("events 行数 = %d, want 1（重复投递不该重复入库）", count)
	}
}

// ---------- ③ source 必填 ----------

// TestEventIngressSourceRequired 缺 source → 400，且**不回退**默认源。
func TestEventIngressSourceRequired(t *testing.T) {
	f := newEventIngressFixture(t)

	code, raw := f.post(t, f.nsID, f.apiKey, map[string]any{"payload": map[string]any{"env": "x"}})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", code, raw)
	}
	status := decodeStatus(t, raw)
	if status.Kind != kindStatus || status.Reason != reasonBadRequest {
		t.Fatalf("错误体 = %+v, want kind=Status reason=BadRequest", status)
	}
	if status.Code != http.StatusBadRequest {
		t.Fatalf("Status.code = %d, want 400（冗余一份 HTTP 码）", status.Code)
	}
}

// ---------- ④ 未授权 ----------

// TestEventIngressUnauthorized 缺 key / 错 key → 401；scope 不足 → 403。
func TestEventIngressUnauthorized(t *testing.T) {
	f := newEventIngressFixture(t)
	body := map[string]any{"source": "manual-1", "payload": map[string]any{"env": "x"}}

	t.Run("没有 Authorization → 401", func(t *testing.T) {
		code, raw := f.post(t, f.nsID, "", body)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", code, raw)
		}
		if got := decodeStatus(t, raw).Reason; got != reasonUnauthorized {
			t.Fatalf("reason = %q, want %q", got, reasonUnauthorized)
		}
	})

	t.Run("错的 key → 401", func(t *testing.T) {
		code, raw := f.post(t, f.nsID, "ant_wrong_key", body)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（body=%s）", code, raw)
		}
	})

	t.Run("有 key 但缺 notify:send → 403", func(t *testing.T) {
		code, raw := f.post(t, f.nsID, "ant_read_only", body)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403（body=%s）", code, raw)
		}
		if got := decodeStatus(t, raw).Reason; got != reasonForbidden {
			t.Fatalf("reason = %q, want %q", got, reasonForbidden)
		}
	})
}

// ---------- ⑤ 命名空间边界 ----------

// TestEventIngressNamespaceScope 路径里的命名空间是唯一作用域：
//
//	不存在 → 404；`*` → 400（跨 ns 投递没有定义）；别的 ns 的同名源在这里不可见 → 404。
func TestEventIngressNamespaceScope(t *testing.T) {
	f := newEventIngressFixture(t)
	body := map[string]any{"source": "manual-1", "payload": map[string]any{"env": "x"}}

	t.Run("命名空间不存在 → 404", func(t *testing.T) {
		code, raw := f.post(t, "ns_does_not_exist", f.apiKey, body)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404（body=%s）", code, raw)
		}
		if got := decodeStatus(t, raw).Reason; got != reasonNotFound {
			t.Fatalf("reason = %q, want %q", got, reasonNotFound)
		}
	})

	t.Run("命名空间 * → 400（可以写进哪个 ns 没有定义）", func(t *testing.T) {
		code, raw := f.post(t, "*", f.apiKey, body)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body=%s）", code, raw)
		}
	})

	t.Run("别的命名空间的源不可见 → 404", func(t *testing.T) {
		// 两个 ns 里都有叫 manual-1 的源：路径里的 ns 决定用哪一个。
		// 这里问的是「staging 里的源能不能从 default 投递」——不能，但也不是错误配置：
		// 直接 404（源在这个作用域里不存在），与 source.md §5 的措辞一致。
		code, raw := f.post(t, f.otherNS, f.apiKey, map[string]any{
			"source": "unknown-source", "payload": map[string]any{"env": "x"},
		})
		if code != http.StatusNotFound {
			t.Fatalf("不存在的源 status = %d, want 404（body=%s）", code, raw)
		}

		// 正向对照：同一个名字在它自己的 ns 里是可见的。
		code, raw = f.post(t, f.otherNS, f.apiKey, body)
		if code != http.StatusOK {
			t.Fatalf("另一个 ns 自己的源 status = %d, want 200（body=%s）", code, raw)
		}
		receipt := decodeReceipt(t, raw)
		event, err := f.db.GetEvent(context.Background(), f.otherNS, receipt.EventID)
		if err != nil {
			t.Fatalf("读 staging 的事件: %v", err)
		}
		if event.NamespaceID != f.otherNS {
			t.Fatalf("事件落在 %q，want %q（路径里的 ns 是唯一作用域）", event.NamespaceID, f.otherNS)
		}
	})
}

// TestEventIngressDisabledSource 停用的源不接收投递（404，与 webhook 同口径）。
func TestEventIngressDisabledSource(t *testing.T) {
	f := newEventIngressFixture(t)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE sources SET enabled = 0 WHERE namespace_id = ? AND name = ?`, f.nsID, "manual-1"); err != nil {
		t.Fatalf("disable source: %v", err)
	}
	code, raw := f.post(t, f.nsID, f.apiKey, map[string]any{
		"source": "manual-1", "payload": map[string]any{"env": "x"},
	})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（停用的源不接收投递；body=%s）", code, raw)
	}
}

// TestEventIngressSlugPath 钉住「路径段是 slug」这条身份口径。
//
// 回归的是一个真实缺陷：ingress 曾把路径段直接当 store 的 namespace_id 用，于是
// canonical 路径 POST /v1/namespaces/default/events → 404，而用内部 id
// POST /v1/namespaces/ns_xxx/events → 200。两种路径必须都指向**同一条**命名空间行。
func TestEventIngressSlugPath(t *testing.T) {
	f := newEventIngressFixture(t)
	createIngressSource(t, f.db, f.nsID, "slug-src")

	// 1) slug 路径（canonical）：必须 200，且事件真的落在 ns_ id 上。
	code, body := f.post(t, f.nsSlug, f.apiKey, map[string]any{
		"source": "slug-src", "id": "slug-1", "payload": map[string]any{"env": "prod"},
	})
	if code != http.StatusOK {
		t.Fatalf("slug 路径投递 = %d, want 200; body=%s", code, body)
	}
	var receipt struct {
		EventID string `json:"eventId"`
		Tasks   []struct {
			TaskID string `json:"taskId"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, body)
	}
	if receipt.EventID == "" || len(receipt.Tasks) != 1 {
		t.Fatalf("slug 路径回执不完整: %s", body)
	}
	var eventNS string
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT namespace_id FROM events WHERE id = ?`, receipt.EventID).Scan(&eventNS); err != nil {
		t.Fatalf("query event namespace: %v", err)
	}
	if eventNS != f.nsID {
		t.Fatalf("事件的 namespace_id = %q，want store id %q（不是路径 slug）", eventNS, f.nsID)
	}

	// 2) 同一个源的 id 路径（兼容）必须与 slug 路径等价：同一条事件被判重复。
	code, body = f.post(t, f.nsID, f.apiKey, map[string]any{
		"source": "slug-src", "id": "slug-1", "payload": map[string]any{"env": "prod"},
	})
	if code != http.StatusOK {
		t.Fatalf("id 路径投递 = %d, want 200; body=%s", code, body)
	}
	var dup struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(body, &dup); err != nil {
		t.Fatalf("decode duplicate: %v (%s)", err, body)
	}
	if !dup.Duplicate {
		t.Fatalf("同一 id 用 slug/id 两条路径投递必须去重（duplicate=true），body=%s", body)
	}

	// 3) 另一个 ns 的 slug 看不到本 ns 的源 → 404（不泄露存在性）。
	if code, body := f.post(t, f.otherSlug, f.apiKey, map[string]any{"source": "slug-src"}); code != http.StatusNotFound {
		t.Fatalf("跨 ns 投递 = %d, want 404; body=%s", code, body)
	}
}
