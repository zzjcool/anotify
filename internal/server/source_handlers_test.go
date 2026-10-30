package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestWriteSourceErrReservedNameIs400 保留来源名的错误必须映射成 400。
//
// 为什么值得单独测：这是本单新增的**用户可见状态码**。它落到 default 就是 500，
// 而 500 会让用户以为中枢坏了（实际改个名字就好），且 mux 级测试没有
// /v1/sources 的 harness（console_test.go 不挂它），e2e 里的源名也全是正常名
// —— 没有这几行，这条映射零覆盖（变异验证已证明：删掉那个 case 全绿）。
//
// 直接调 writeSourceErr 而不起 mux：它只依赖 w/err/op，零依赖，
// 所以 `&sourceHandler{}` 就够；顺带把「其它错误仍走 500」一并钉住
// （避免将来把别的 store 错误误吞成 400，那会把服务端故障伪装成用户输入问题）。
func TestWriteSourceErrReservedNameIs400(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"两层包装的保留名错误", fmt.Errorf("create source: %w", store.ErrReservedEventSource), http.StatusBadRequest},
		{"改名路径的保留名错误", fmt.Errorf("update source: %w", store.ErrReservedEventSource), http.StatusBadRequest},
		{"其它错误仍走 default（500）", fmt.Errorf("create source: 磁盘满了"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h := &sourceHandler{}
			h.writeSourceErr(rr, c.err, "create")
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, c.want, rr.Body.String())
			}
		})
	}

	// 400 的 body 要能让人知道「改个名字就好」，所以必须带上保留名说明
	t.Run("400 的 body 含保留名说明", func(t *testing.T) {
		rr := httptest.NewRecorder()
		(&sourceHandler{}).writeSourceErr(rr, fmt.Errorf("create source: %w", store.ErrReservedEventSource), "create")
		if body := rr.Body.String(); !strings.Contains(body, "route") {
			t.Fatalf("body = %s, want 含 \"route\" 的说明", body)
		}
	})
}

// ---------- 源是命名空间资源（契约 §6.2） ----------
//
// 下面这组用例钉住的是「源的每个方法都要显式 namespaceId」这条契约：
// 写端点与单对象端点不允许 `*`，缺参一律 400（**不回退默认命名空间**），
// 别的命名空间的 id 必须 404（不是 200，也不是 403——源 id 全局唯一，
// 在本命名空间查不到就是"这个命名空间里没有这个源"）。
//
// 用 nsTestEnv（namespace_handlers_test.go）：同一套真 SQLite + 真会话中间件。

// TestSourcesListRequiresNamespace GET 列表缺 namespaceId → 400。
//
// 列表是允许 `*` 的读端点，但"允许 *"不等于"可以不传"：
// 不传 = 调用方写错了（改代码），`*` = 明确要跨命名空间汇总。
func TestSourcesListRequiresNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodGet, "/v1/sources", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "namespaceId") {
		t.Fatalf("400 的 body 要说清缺的是 namespaceId：%s", body)
	}
}

// TestSourcesCreateRequiresNamespace 写入端点缺 namespaceId → 400（不允许 *）。
func TestSourcesCreateRequiresNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	capID := env.builtinCapID(t, env.nsID, "notify.phone")

	rr := env.do(t, http.MethodPost, "/v1/sources", map[string]any{
		"name": "src-no-ns", "kind": store.SourceKindManual, "targets": []string{capID},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 namespaceId status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	// `*` 在写端点没有定义（写一条源属于哪个命名空间？）→ 400
	rr = env.do(t, http.MethodPost, "/v1/sources?namespaceId=*", map[string]any{
		"name": "src-all", "kind": store.SourceKindManual, "targets": []string{capID},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("namespaceId=* status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestSourcesCreateAndViewCarryNamespaceId 建源成功，且响应里的 source 带 namespaceId。
//
// 响应带 namespaceId 是硬要求（契约 §4「所有资源响应带 namespaceId」）：
// 前端切命名空间后要能确认"这条源真的是我当前空间的"，
// 少了这个字段就只能靠"列表里有没有它"去猜。
func TestSourcesCreateAndViewCarryNamespaceId(t *testing.T) {
	env := newNSTestEnv(t)
	capID := env.builtinCapID(t, env.nsID, "notify.phone")

	rr := env.do(t, http.MethodPost, "/v1/sources?namespaceId="+env.nsID, map[string]any{
		"name": "src-a", "kind": store.SourceKindManual, "config": map[string]any{},
		"targets": []string{capID},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	view, _ := nsDecode(t, rr)["source"].(map[string]any)
	if view["namespaceId"] != env.nsID {
		t.Fatalf("source.namespaceId = %v, want %s（响应必须带 namespaceId）", view["namespaceId"], env.nsID)
	}
	// 出口能力被解析出来了（不是 Missing），说明视图是按本命名空间查的能力
	caps, _ := view["targetCaps"].([]any)
	if len(caps) != 1 {
		t.Fatalf("targetCaps = %v, want 1 条", view["targetCaps"])
	}
	if missing, _ := caps[0].(map[string]any)["missing"].(bool); missing {
		t.Fatalf("targetCaps = %v, want 命中本命名空间的能力定义", caps[0])
	}

	// body 里带 namespaceId 也可以（契约 §4 的取值顺序：query → body）
	rr = env.do(t, http.MethodPost, "/v1/sources", map[string]any{
		"name": "src-b", "kind": store.SourceKindManual, "config": map[string]any{},
		"namespaceId": env.nsID, "targets": []string{capID},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("body namespaceId status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestSourcesListScopedToNamespace 列表只看本命名空间；别的命名空间是空列表（[] 不是 null）。
func TestSourcesListScopedToNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")
	env.createSource(t, env.nsID, "src-in-default")

	rr := env.do(t, http.MethodGet, "/v1/sources?namespaceId="+env.nsID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	list, _ := nsDecode(t, rr)["sources"].([]any)
	if len(list) != 1 {
		t.Fatalf("sources = %v, want 1 条（本命名空间）", list)
	}

	rr = env.do(t, http.MethodGet, "/v1/sources?namespaceId="+other.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("staging status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	raw := nsDecode(t, rr)["sources"]
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("sources = %v (%T), want []（空列表不能是 null，不变量 I9）", raw, raw)
	}
	if len(list) != 0 {
		t.Fatalf("staging sources = %v, want 空", list)
	}
}

// TestSourcesListAllNamespaces namespaceId=* 跨命名空间汇总（管理员视图）。
func TestSourcesListAllNamespaces(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")
	env.createSource(t, env.nsID, "src-in-default")
	env.createSource(t, other.ID, "src-in-staging")

	rr := env.do(t, http.MethodGet, "/v1/sources?namespaceId=*", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	list, _ := nsDecode(t, rr)["sources"].([]any)
	if len(list) != 2 {
		t.Fatalf("sources = %v, want 2 条（两个命名空间各一）", list)
	}
	// 每条的 namespaceId 必须各是各的（聚合视图下不能都写成同一个）
	seen := map[string]bool{}
	for _, item := range list {
		nsID, _ := item.(map[string]any)["namespaceId"].(string)
		seen[nsID] = true
	}
	if !seen[env.nsID] || !seen[other.ID] {
		t.Fatalf("namespaceId 集合 = %v, want 含 %s 与 %s", seen, env.nsID, other.ID)
	}
}

// TestSourcesSingleObjectIsNamespaceScoped 单对象端点：别的命名空间 → 404，`*` → 400。
func TestSourcesSingleObjectIsNamespaceScoped(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")
	src := env.createSource(t, env.nsID, "src-in-default")

	// 换到 staging 去查 default 里的源：查不到（id 全局唯一，但作用域不同）
	rr := env.do(t, http.MethodGet, "/v1/sources/"+src.ID+"?namespaceId="+other.ID, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("跨命名空间 GET status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}

	// 单对象读不支持 `*`
	rr = env.do(t, http.MethodGet, "/v1/sources/"+src.ID+"?namespaceId=*", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("namespaceId=* status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	// PATCH 同理：作用域不对 → 404，且不能改到别的命名空间的那条
	rr = env.do(t, http.MethodPatch, "/v1/sources/"+src.ID+"?namespaceId="+other.ID, map[string]any{"name": "hacked"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("跨命名空间 PATCH status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
	got, err := env.db.GetSource(context.Background(), env.nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got.Name != "src-in-default" {
		t.Fatalf("name = %q, want src-in-default（跨命名空间 PATCH 不能生效）", got.Name)
	}

	// 正确作用域下 PATCH 成功
	rr = env.do(t, http.MethodPatch, "/v1/sources/"+src.ID+"?namespaceId="+env.nsID, map[string]any{"name": "renamed"})
	if rr.Code != http.StatusOK {
		t.Fatalf("同命名空间 PATCH status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestSourcesFireRequiresNamespace 触发是写操作：缺 namespaceId → 400，跨命名空间 → 404。
//
// 正向 200 依赖 internal/source 把 src.NamespaceID 填进 InsertEvent（阶段 2），
// 所以这里只钉住 handler 侧的作用域解析（本单的改动面）。
func TestSourcesFireRequiresNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")
	src := env.createSource(t, env.nsID, "src-in-default")

	rr := env.do(t, http.MethodPost, "/v1/sources/"+src.ID+"/fire", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 namespaceId status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	rr = env.do(t, http.MethodPost, "/v1/sources/"+src.ID+"/fire?namespaceId="+other.ID, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("跨命名空间 fire status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(env.dispatcher.got) != 0 {
		t.Fatalf("作用域不对时不该派发任何任务，got=%d", len(env.dispatcher.got))
	}
}

// TestSourcesDryRunRequiresNamespace 试运行同样必填 namespaceId（它要推演本空间的派发）。
//
// 草稿路径（POST /v1/sources/dry-run）的 namespaceId 可以放在 body.namespaceId，
// 也可以放在 body.source.namespaceId —— 两者都测。
func TestSourcesDryRunRequiresNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")
	capID := env.builtinCapID(t, env.nsID, "notify.phone")

	draft := map[string]any{
		"name": "draft", "kind": store.SourceKindManual,
		"enabled": true, "targets": []map[string]any{{"capabilityId": capID}},
	}
	// 1) 缺 namespaceId → 400
	rr := env.do(t, http.MethodPost, "/v1/sources/dry-run", map[string]any{"source": draft})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 namespaceId status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	// 2) body.namespaceId 给了但属于别的 org 之外……先测正常路径：带 namespaceId → 200
	rr = env.do(t, http.MethodPost, "/v1/sources/dry-run", map[string]any{"namespaceId": env.nsID, "source": draft})
	if rr.Code != http.StatusOK {
		t.Fatalf("带 namespaceId status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(env.dispatcher.got) != 1 {
		t.Fatalf("试运行应该走一次派发推演，got=%d", len(env.dispatcher.got))
	}
	if !env.dispatcher.got[0].DryRun {
		t.Fatalf("试运行的 DispatchInput.DryRun 必须是 true（不能真的落库/推帧）")
	}

	// 3) namespaceId 走 body.source.namespaceId 也行，且以参数为准（覆盖草稿里手写的值）
	env.dispatcher.got = nil
	draftOther := map[string]any{
		"name": "draft2", "kind": store.SourceKindManual, "enabled": true,
		"namespaceId": other.ID, // 手写的草稿值
		"targets":     []map[string]any{{"capabilityId": capID}},
	}
	rr = env.do(t, http.MethodPost, "/v1/sources/dry-run?namespaceId="+env.nsID, map[string]any{"source": draftOther})
	if rr.Code != http.StatusOK {
		t.Fatalf("query 优先 status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(env.dispatcher.got) != 1 {
		t.Fatalf("应该走一次派发推演，got=%d", len(env.dispatcher.got))
	}

	// 4) 不存在的命名空间 → 404
	rr = env.do(t, http.MethodPost, "/v1/sources/dry-run?namespaceId=ns_nope", map[string]any{"source": draft})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知命名空间 status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestHooksDoNotRequireNamespace webhook 入口是外部路径：**不需要** namespaceId。
//
// 这是契约 §6.2 唯一的例外：鉴权就是 URL 里的 token（全局唯一），
// 源行自己带着 namespace_id。要求外部系统先知道 namespaceId 才能回调是荒谬的
// （GitHub 不会为此改代码），所以这里必须钉住"没有 namespaceId 也不会 400"。
func TestHooksDoNotRequireNamespace(t *testing.T) {
	env := newNSTestEnv(t)

	// token 不存在 → 404（与"命名空间缺失"的 400 必须区分开）
	rr := env.doAnon(t, http.MethodPost, "/v1/hooks/nosuchtoken", map[string]any{"type": "ping"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知 token status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}

	// token 有效：不能因为"缺 namespaceId"被拒（命中别的错误说明命名空间没被要求）
	src := &store.Source{
		NamespaceID: env.nsID, Name: "hook-src", Kind: store.SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"tok-abc"}`),
		Enabled: true,
		Targets: []store.SourceTarget{{CapabilityID: env.builtinCapID(t, env.nsID, "notify.phone")}},
	}
	if err := env.db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create webhook source: %v", err)
	}
	rr = env.doAnon(t, http.MethodPost, "/v1/hooks/tok-abc", map[string]any{"type": "ping"})
	if rr.Code == http.StatusBadRequest {
		t.Fatalf("webhook 入口不该要求 namespaceId，却返回 400（body=%s）", rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "namespaceId is required") {
		t.Fatalf("webhook 入口不该要求 namespaceId：%s", body)
	}
}
