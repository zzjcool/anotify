package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- P2.2：边按引用 + 升级闸门（HTTP 层） ----------

// TestEdgeCreateWithRefs 下游的 `toRef` 有两种引用形态：
// 裸名字**原样**落库（跟随 latest），钉死引用也原样落库。
//
// **改写说明**：造数从 `POST /v1/edges` 换成
// `PUT /v1/capabilities/{name}/downstreams`（能力边的唯一写入口，replace 语义）。
// 两条边用一次 PUT 提交（`notify` + `notify@1`），断言不变。
func TestEdgeCreateWithRefs(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	env.seedCapability(t, "notify", 1, nil)

	rr := env.replaceDownstreams(t, "review", []map[string]any{
		{"toRef": "notify"},   // 跟随 latest
		{"toRef": "notify@1"}, // 钉死那一版
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got, _ := decode(t, rr)["name"].(string); got != "review" {
		t.Fatalf("响应里的 name = %q, want review", got)
	}
	links, _ := decode(t, rr)["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("links = %v, want 2", links)
	}
	if ref, _ := links[0].(map[string]any)["toRef"].(string); ref != "notify" {
		t.Fatalf("links[0].toRef = %q, want notify", ref)
	}
	if ref, _ := links[1].(map[string]any)["toRef"].(string); ref != "notify@1" {
		t.Fatalf("links[1].toRef = %q, want notify@1", ref)
	}
	// 列表回的是引用（不是 id）
	rr = env.do(t, http.MethodGet, env.withNS("/v1/edges"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	edges, _ := decode(t, rr)["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2: %v", len(edges), edges)
	}
	for _, raw := range edges {
		e := raw.(map[string]any)
		if _, hasID := e["toCapability"]; hasID {
			t.Fatalf("响应里还在用旧字段名 toCapability（P2.2 之后只有 toRef）: %v", e)
		}
		if ref, _ := e["toRef"].(string); ref != "notify" && ref != "notify@1" {
			t.Fatalf("toRef = %q, want notify 或 notify@1", ref)
		}
		// 左端恒为能力**名字**（一个能力的所有版本共用同一组出边，契约 §1）
		if ref, _ := e["fromRef"].(string); ref != "review" {
			t.Fatalf("fromRef = %q, want review（裸名字，不是 name@version）", ref)
		}
		if port, _ := e["fromPort"].(string); port != store.ImplicitPort {
			t.Fatalf("fromPort = %q, want %q（隐式出口）", port, store.ImplicitPort)
		}
	}
}

// TestDownstreamsLegacyIDInputCompat 传能力 id：仍然能存，但**落库的是引用**。
//
// **改写说明**：原用例走 `POST /v1/edges` 的两端 id 兼容；能力边的写入口迁到
// `PUT /v1/capabilities/{name}/downstreams` 之后，左端是路径里的**名字**
// （不再有「用 id 指左端」这回事），右端仍然接受 id（输入兼容一个版本）。
// 断言因此变成更强的口径：库里两边都是引用，一个 id 都没有。
func TestDownstreamsLegacyIDInputCompat(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	to := env.seedCapability(t, "notify", 1, nil)

	rr := env.replaceDownstreams(t, "review", []map[string]any{{"toRef": to.ID}})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// 库里没有 id：左端是名字（跟随 latest），右端是 name@version（钉死 = 「就是这个 id」的意图）
	var fromRef, toRef string
	if err := env.db.QueryRow(`SELECT from_ref, to_ref FROM edges WHERE namespace_id = ?`, env.nsID).
		Scan(&fromRef, &toRef); err != nil {
		t.Fatalf("read edge: %v", err)
	}
	if fromRef != "review" || toRef != store.FormatRef("notify", to.Version) {
		t.Fatalf("库里的引用 = (%q → %q), want (%q → %q)",
			fromRef, toRef, "review", store.FormatRef("notify", to.Version))
	}
}

// TestEdgeCreateRejectsUnknownRef 解析不到的下游 → 400 unknown_target（客户端可修）。
//
// **改写说明**：入口换成 PUT downstreams；机器码按契约 §2 从通用的
// `edge_rejected` 细化为 `unknown_target`（带名字），断言因此更具体而不是更宽。
func TestEdgeCreateRejectsUnknownRef(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})

	rr := env.replaceDownstreams(t, "review", []map[string]any{{"toRef": "ghost@9"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rr.Code, rr.Body.String())
	}
	body := decode(t, rr)
	if body["code"] != "unknown_target" {
		t.Fatalf("code = %v, want unknown_target", body["code"])
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "ghost@9") {
		t.Fatalf("错误里没点名坏引用: %v", body["error"])
	}
}

// TestDeleteEdgeByRef DELETE 按引用定位（新字段名与旧字段名都认）。
//
// **改写说明**：造数换成 PUT downstreams（能力边的唯一写入口），DELETE 断言不变。
func TestDeleteEdgeByRef(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	env.seedCapability(t, "notify", 1, nil)

	if rr := env.replaceDownstreams(t, "review", []map[string]any{{"toRef": "notify"}}); rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	path := env.withNS("/v1/edges?fromRef=review&fromPort=" + store.ImplicitPort + "&toRef=notify")
	if rr := env.do(t, http.MethodDelete, path, nil); rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var n int
	if err := env.db.QueryRow(`SELECT count(*) FROM edges WHERE namespace_id = ?`, env.nsID).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 0 {
		t.Fatalf("删完还剩 %d 条边", n)
	}
}

// TestCreateCapabilityUpgradeGate 升级闸门在 HTTP 层的形状：
// 破坏性升级 → 409 upgrade_breaking + upgradeImpact（列出受影响 link）；force → 200。
func TestCreateCapabilityUpgradeGate(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	// v1：issue 必填，出口 done + failed
	v1 := &store.CapabilityVersion{
		NamespaceID: env.nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`),
		Outputs:      []string{"done", "failed"},
		Source:       store.CapSourceCustom,
	}
	if err := env.db.CreateCapability(ctx, v1); err != nil {
		t.Fatalf("create fixer@1: %v", err)
	}
	from := env.seedCapability(t, "review", 1, []string{"fix"})
	// 一条**跟随 latest** 的 link（只有这种会被闸门影响）
	if err := env.db.CreateEdge(ctx, &store.Edge{
		NamespaceID: env.nsID, FromRef: from.Name, FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create link: %v", err)
	}

	breaking := map[string]any{
		"namespaceId": env.nsID,
		"name":        "fixer",
		"paramsSchema": map[string]any{
			"type":     "object",
			"required": []string{"issue", "note"}, // 新增必填
			"properties": map[string]any{
				"issue": map[string]any{"type": "string"},
				"note":  map[string]any{"type": "string"},
			},
		},
		"outputs": []string{"done"}, // 删 port
	}

	rr := env.do(t, http.MethodPost, "/v1/capabilities", breaking)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409（body=%s）", rr.Code, rr.Body.String())
	}
	body := decode(t, rr)
	if body["code"] != "upgrade_breaking" {
		t.Fatalf("code = %v, want upgrade_breaking", body["code"])
	}
	impact, _ := body["upgradeImpact"].(map[string]any)
	if impact == nil {
		t.Fatalf("拒绝里没有 upgradeImpact: %v", body)
	}
	if impact["breaking"] != true {
		t.Fatalf("upgradeImpact.breaking = %v, want true", impact["breaking"])
	}
	links, _ := impact["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("upgradeImpact.links = %v, want 1 条（review --fix--> fixer）", impact["links"])
	}
	link := links[0].(map[string]any)
	if link["fromRef"] != "review" || link["port"] != "fix" || link["toRef"] != "fixer" {
		t.Fatalf("受影响 link = %v, want review --fix--> fixer", link)
	}
	// 版本没被写进去
	if _, err := env.db.GetCapability(ctx, env.nsID, "fixer", 2); err == nil {
		t.Fatal("闸门拒了却还是写进了 fixer@2")
	}

	// force=true 越过闸门
	breaking["force"] = true
	rr = env.do(t, http.MethodPost, "/v1/capabilities", breaking)
	if rr.Code != http.StatusOK {
		t.Fatalf("force status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	okBody := decode(t, rr)
	if okBody["upgradeImpact"] == nil {
		t.Fatalf("成功响应里也应带 upgradeImpact（前端要说明这一版动了什么）: %v", okBody)
	}
	if _, err := env.db.GetCapability(ctx, env.nsID, "fixer", 2); err != nil {
		t.Fatalf("force 之后 fixer@2 应该存在: %v", err)
	}
}

// TestCreateCapabilityAdditiveNoGate 加可选入参 → 不拦（不需要 force）。
func TestCreateCapabilityAdditiveNoGate(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	v1 := &store.CapabilityVersion{
		NamespaceID: env.nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}}}`),
		Outputs:      []string{"done"},
		Source:       store.CapSourceCustom,
	}
	if err := env.db.CreateCapability(ctx, v1); err != nil {
		t.Fatalf("create fixer@1: %v", err)
	}
	rr := env.do(t, http.MethodPost, "/v1/capabilities", map[string]any{
		"namespaceId": env.nsID,
		"name":        "fixer",
		"paramsSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"issue": map[string]any{"type": "string"},
				"note":  map[string]any{"type": "string"}, // 新增可选
			},
		},
		"outputs": []string{"done", "extra"}, // 加 port
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	impact, _ := decode(t, rr)["upgradeImpact"].(map[string]any)
	if impact == nil || impact["breaking"] != false {
		t.Fatalf("upgradeImpact = %v, want breaking=false", impact)
	}
}

// ---------- 边列表的派生字段：引用 + 解析后的 id ----------

// listEdgeByToRef 取列表里 toRef 命中的那一条边（测试用）。
func listEdgeByToRef(t *testing.T, env *consoleEnv, toRef string) map[string]any {
	t.Helper()
	rr := env.do(t, http.MethodGet, env.withNS("/v1/edges"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rr.Code, rr.Body.String())
	}
	edges, _ := decode(t, rr)["edges"].([]any)
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		if e["toRef"] == toRef {
			return e
		}
	}
	t.Fatalf("列表里没有 toRef=%q 的边: %v", toRef, edges)
	return nil
}

// TestListEdgesReturnsResolvedIDs 列表除引用外还回「引用此刻解析到哪一版」的 id。
//
// UI 既要显示**引用**（用户写进去的那个字符串），又要拿 id 去做详情链接 ——
// 但它不该自己实现 name→latest，那是第二份解析语义。所以 id 由服务端派生。
// 入口边的 fromCapabilityId 恒为空串：左端是来源名，没有「哪一版」。
func TestListEdgesReturnsResolvedIDs(t *testing.T) {
	env := newConsoleEnv(t)
	from := env.seedCapability(t, "review", 1, []string{"out"})
	to := env.seedCapability(t, "notify", 1, nil)

	// 能力边：右端是**裸名字**（跟随 latest）；左端恒为能力名（PUT downstreams）
	if rr := env.replaceDownstreams(t, "review", []map[string]any{{"toRef": "notify"}}); rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// 入口边：左端是来源名，右端钉死 `notify@1`
	if rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID: env.nsID, FromIsSource: true,
		FromRef: "github", FromPort: "push", ToRef: "notify@1",
	}); rr.Code != http.StatusOK {
		t.Fatalf("create entry status = %d, body=%s", rr.Code, rr.Body.String())
	}

	capEdge := listEdgeByToRef(t, env, "notify")
	if capEdge["fromRef"] != "review" {
		t.Fatalf("fromRef = %v, want review", capEdge["fromRef"])
	}
	if capEdge["fromCapabilityId"] != from.ID {
		t.Fatalf("fromCapabilityId = %v, want %s（引用 review 解析到的版本）",
			capEdge["fromCapabilityId"], from.ID)
	}
	if capEdge["toCapabilityId"] != to.ID {
		t.Fatalf("toCapabilityId = %v, want %s", capEdge["toCapabilityId"], to.ID)
	}

	entry := listEdgeByToRef(t, env, "notify@1")
	if entry["fromRef"] != "github" || entry["fromIsSource"] != true {
		t.Fatalf("入口边的左端 = %v / fromIsSource=%v, want github / true",
			entry["fromRef"], entry["fromIsSource"])
	}
	// 空串（不是 null）：JSON 里必须是字符串，否则前端 `e.fromCapabilityId || ""` 之外的
	// 判空写法会各自长出一个分支
	if got, ok := entry["fromCapabilityId"].(string); !ok || got != "" {
		t.Fatalf("入口边的 fromCapabilityId = %#v, want 空字符串", entry["fromCapabilityId"])
	}
	// 钉死引用照样能解析到那一版
	if entry["toCapabilityId"] != to.ID {
		t.Fatalf("钉死引用的 toCapabilityId = %v, want %s", entry["toCapabilityId"], to.ID)
	}
}

// TestListEdgesResolvedIDFollowsLatest 裸名字 link 在能力发新版后解析到**新版**，
// 钉死 link 不动 —— 这就是「跟随 latest / 钉死」两种引用语义在 API 上的差别。
//
// 客户端如果自己去把 id 写进边（P2.2 之前的形状），发新版后这里会一直停在 v1：
// 升级永远不生效。所以这条断言是语义级的，不只是字段名。
func TestListEdgesResolvedIDFollowsLatest(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	v1 := env.seedCapability(t, "notify", 1, nil)

	// 同一个能力扇出到两个引用：一个是裸名字（跟随），一个是钉死 v1
	if rr := env.replaceDownstreams(t, "review", []map[string]any{
		{"toRef": "notify"},
		{"toRef": "notify@1"},
	}); rr.Code != http.StatusOK {
		t.Fatalf("create downstreams status = %d, body=%s", rr.Code, rr.Body.String())
	}

	if got := listEdgeByToRef(t, env, "notify")["toCapabilityId"]; got != v1.ID {
		t.Fatalf("升级前裸名字解析到 %v, want %s", got, v1.ID)
	}

	// 发新版（加一个可选参数：不破坏已有 link，不需要过闸门）
	v2 := env.seedCapability(t, "notify", 2, nil)

	followed := listEdgeByToRef(t, env, "notify")
	if followed["toRef"] != "notify" {
		t.Fatalf("toRef = %v, want notify（裸名字原样保存）", followed["toRef"])
	}
	if followed["toCapabilityId"] != v2.ID {
		t.Fatalf("发新版后裸名字的 toCapabilityId = %v, want %s（跟随 latest）",
			followed["toCapabilityId"], v2.ID)
	}
	pinned := listEdgeByToRef(t, env, "notify@1")
	if pinned["toCapabilityId"] != v1.ID {
		t.Fatalf("钉死引用的 toCapabilityId = %v, want %s（升级不生效）",
			pinned["toCapabilityId"], v1.ID)
	}
}

// TestDeleteEntryEdgeByRef 入口边的删除也走**引用口径**。
//
// 控制台删除入口边发的是 `fromRef=<来源名>&fromIsSource=1`（旧字段名
// fromCapability 仍然兼容，但前端已经改口）。这条用例钉住新口径：
// 来源名不是任何能力，解析路径与能力边不同，两边都得删得掉。
func TestDeleteEntryEdgeByRef(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "notify", 1, nil)

	if rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID: env.nsID, FromIsSource: true,
		FromRef: "github", FromPort: "push", ToRef: "notify",
	}); rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	path := env.withNS("/v1/edges?fromIsSource=1&fromRef=github&fromPort=push&toRef=notify")
	if rr := env.do(t, http.MethodDelete, path, nil); rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var n int
	if err := env.db.QueryRow(`SELECT count(*) FROM edges WHERE namespace_id = ?`, env.nsID).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 0 {
		t.Fatalf("删完还剩 %d 条边", n)
	}
}
