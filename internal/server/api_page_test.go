package server

// 本文件覆盖 #377 的 HTTP 侧：八个列表端点的 `limit` / `continue`。
//
// 分工：
//   - 存储层的「不漏不重 / 并列排序键 / 跨页写入」在 internal/store/page_test.go；
//   - 这里守 handler 的三件事：
//     1. 参数口径（缺省 50、非整数 400、<=0 按 50、>500 钳到 500）；
//     2. 游标是**不透明且绑定作用域**的（坏令牌 / 别人的令牌 → 400，不静默回退第一页）；
//     3. 响应形状（末页不带 continue；非信封列表末页回 ""；空列表是 `[]` 不是 `null`）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 口径单测（不依赖 HTTP 装配） ----------

func TestPageLimitHelpers(t *testing.T) {
	req := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/list?"+query, nil)
	}
	cases := []struct {
		query   string
		want    int
		wantErr bool
	}{
		{"", pageDefaultLimit, false},
		{"limit=", pageDefaultLimit, false},
		{"limit=10", 10, false},
		{"limit=0", pageDefaultLimit, false},  // <=0 按 50
		{"limit=-7", pageDefaultLimit, false}, // 同上
		{"limit=500", pageLimitCap, false},    // 恰好上限
		{"limit=501", pageLimitCap, false},    // 越界钳位，不报错
		{"limit=99999", pageLimitCap, false},  // 同上
		{"limit=abc", 0, true},                // 非整数 → 400
		{"limit=10&limit=20", 10, false},      // 取第一个（Query().Get 的口径）
		{"other=1", pageDefaultLimit, false},  // 没写 limit
		{"limit=%2010%20", 10, false},         // 两侧空白被 TrimSpace 吃掉
	}
	for _, c := range cases {
		got, err := pageLimit(req(c.query))
		if c.wantErr {
			if err == nil {
				t.Fatalf("%q: 期望 400，但解析成功", c.query)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: 不该报错: %v", c.query, err)
		}
		if got != c.want {
			t.Fatalf("%q: pageLimit=%d want %d", c.query, got, c.want)
		}
		// auth/admin 域（错误体是 {error}）同一口径。
		plain, msg := pageLimitPlain(req(c.query))
		if msg != "" {
			t.Fatalf("%q: pageLimitPlain 不该报错: %s", c.query, msg)
		}
		if plain != c.want {
			t.Fatalf("%q: pageLimitPlain=%d want %d", c.query, plain, c.want)
		}
	}
	if _, msg := pageLimitPlain(req("limit=nope")); msg == "" {
		t.Fatal("pageLimitPlain 对非整数应返回错误文本")
	}
}

func TestPageCursorHelpers(t *testing.T) {
	// 往返：binding 一致、键个数一致。
	token := encodePageCursor("sources|ns_1", "1700000000", "src_1")
	keys, err := decodePageCursor(token, "sources|ns_1", 2)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(keys) != 2 || keys[0] != "1700000000" || keys[1] != "src_1" {
		t.Fatalf("游标往返不一致: %v", keys)
	}

	// 空串 = 第一页（不是错误）。
	if keys, err := decodePageCursor("", "sources|ns_1", 2); err != nil || keys != nil {
		t.Fatalf("空游标应为 (nil,nil)，got (%v,%v)", keys, err)
	}

	// 坏令牌：base64 / JSON / 键个数 / 空键值。
	for _, raw := range []string{"not-base64!!", "e30", encodePageCursor("sources|ns_1", "only-one"), encodePageCursor("sources|ns_1", "1", "")} {
		if _, err := decodePageCursor(raw, "sources|ns_1", 2); err == nil {
			t.Fatalf("坏令牌 %q 应 400", raw)
		}
	}

	// 跨作用域：绑定不同 → 400，且错误文本明确（不是「变成第一页」）。
	_, err = decodePageCursor(token, "sources|ns_2", 2)
	if err == nil {
		t.Fatal("跨作用域游标应 400")
	}
	if got := pageCursorMessage(err); got != "continue token does not belong to this query" {
		t.Fatalf("跨作用域错误文本不对: %q", got)
	}

	// pageCut：多取一行只用于判定，不发给客户端。
	rows := []int{1, 2, 3, 4}
	got, cont := pageCut(rows, 3, "b", func(v int) []string { return []string{fmt.Sprint(v)} })
	if len(got) != 3 || cont == "" {
		t.Fatalf("pageCut: 应截到 3 条且给 continue，got len=%d cont=%q", len(got), cont)
	}
	if got[len(got)-1] != 3 {
		t.Fatalf("pageCut: 游标必须来自本页最后一条，got %v", got)
	}
	got, cont = pageCut([]int{1, 2}, 3, "b", func(v int) []string { return []string{fmt.Sprint(v)} })
	if len(got) != 2 || cont != "" {
		t.Fatalf("pageCut: 末页不该有 continue，got len=%d cont=%q", len(got), cont)
	}
}

// ---------- handler 层：分页 + 参数校验 ----------

// sourcePageNames 取 List 信封里的 metadata.name 序列。
func sourcePageNames(t *testing.T, doc map[string]any) ([]string, string) {
	t.Helper()
	items, ok := doc["items"].([]any)
	if !ok {
		t.Fatalf("items 必须是数组（空列表是 [] 不是 null）: %T", doc["items"])
	}
	names := make([]string, 0, len(items))
	for _, raw := range items {
		meta := sourceResObj(t, sourceResObj(t, raw)["metadata"])
		names = append(names, sourceResString(t, meta["name"]))
	}
	meta, _ := doc["metadata"].(map[string]any)
	cont, _ := meta["continue"].(string)
	return names, cont
}

func sourceNameOf(t *testing.T, item any) string {
	t.Helper()
	meta := sourceResObj(t, sourceResObj(t, item)["metadata"])
	return sourceResString(t, meta["name"])
}

func TestSourcesListPagingHTTP(t *testing.T) {
	env := newSourceResEnv(t)

	var created []string
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("src-%d", i)
		env.createSource(map[string]any{"kind": "manual"}, map[string]any{"name": name})
		created = append(created, name)
	}

	// 全量（缺省 limit=50）：顺序是创建顺序的镜像（fixed clock → id DESC）。
	all := env.doc(env.request(http.MethodGet, env.sourcesPath(), nil))
	fullNames, fullCont := sourcePageNames(t, all)
	if len(fullNames) != 5 || fullCont != "" {
		t.Fatalf("缺省 limit 应一次给全 5 条且不带 continue，got %v cont=%q", fullNames, fullCont)
	}
	items, _ := all["items"].([]any)
	// id 是「毫秒时间 + 随机」，同一毫秒内不保证按创建顺序可读；这里只断言
	// 「缺省一页就是全集」（顺序由 ListSources 的 ORDER BY 决定，翻页必须与之逐条一致）。
	if len(items) != len(created) {
		t.Fatalf("缺省 limit 应给全 5 条，got %d", len(items))
	}
	seenCreated := map[string]bool{}
	for _, name := range created {
		seenCreated[name] = true
	}
	for _, item := range items {
		if !seenCreated[sourceNameOf(t, item)] {
			t.Fatalf("列表里出现夹具没建过的源: %v", item)
		}
	}

	// 分页：limit=2 走完，拼起来必须与全量一致（不漏不重）。
	var got []string
	target := env.sourcesPath() + "?limit=2"
	for page := 0; page < 10; page++ {
		rr := env.request(http.MethodGet, target, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		names, cont := sourcePageNames(t, env.doc(rr))
		got = append(got, names...)
		if cont == "" {
			break
		}
		target = env.sourcesPath() + "?limit=2&continue=" + cont
	}
	if len(got) != len(fullNames) {
		t.Fatalf("翻页拿到 %d 条，全量 %d 条（丢行/重复）: %v vs %v", len(got), len(fullNames), got, fullNames)
	}
	for i := range got {
		if got[i] != fullNames[i] {
			t.Fatalf("翻页顺序与全量不一致 idx %d: %s vs %s", i, got[i], fullNames[i])
		}
	}

	// 空列表：另一个命名空间没有任何源 → items 是 []，不是 null。
	emptyNS, err := env.db.CreateNamespace(context.Background(), "empty-ns", "空")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	doc := env.doc(env.request(http.MethodGet, "/v1/namespaces/"+emptyNS.ID+"/sources", nil))
	arr, ok := doc["items"].([]any)
	if !ok || arr == nil || len(arr) != 0 {
		t.Fatalf("空列表必须是 []（不是 null），got %T %v", doc["items"], doc["items"])
	}
}

func TestSourcesListRejectsBadPagingParams(t *testing.T) {
	env := newSourceResEnv(t)
	env.createSource(map[string]any{"kind": "manual"}, map[string]any{"name": "only"})

	// 非整数 limit → 400 Status 信封（reason=BadRequest，与既有接口同族）。
	rr := env.request(http.MethodGet, env.sourcesPath()+"?limit=abc", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc 应 400，got %d %s", rr.Code, rr.Body.String())
	}
	doc := env.doc(rr)
	if doc["reason"] != "BadRequest" {
		t.Fatalf("400 的 reason 应为 BadRequest，got %v", doc["reason"])
	}

	// 坏游标 / 跨作用域游标 → 400（不静默回退第一页）。
	for _, raw := range []string{"garbage", encodePageCursor("capabilities|"+env.nsID, "100", "cap")} {
		rr := env.request(http.MethodGet, env.sourcesPath()+"?continue="+raw, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("游标 %q 应 400，got %d %s", raw, rr.Code, rr.Body.String())
		}
	}

	// limit=999 钳到 500：请求成功（越界不报错），且返回的行数不超过钳位值。
	rr = env.request(http.MethodGet, env.sourcesPath()+"?limit=999", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("limit 越界不应报错，got %d %s", rr.Code, rr.Body.String())
	}
}

func TestNodesListPagingHTTP(t *testing.T) {
	env := newNodeTestEnv(t)
	for _, name := range []string{"node-c", "node-a", "node-b"} {
		env.createNode(name, nil)
	}

	full := env.do(http.MethodGet, "/v1/nodes", nil)
	if full.Code != http.StatusOK {
		t.Fatalf("全量列表 %d: %s", full.Code, full.Body.String())
	}
	var fullDoc map[string]any
	if err := json.Unmarshal(full.Body.Bytes(), &fullDoc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fullItems, _ := fullDoc["items"].([]any)
	if len(fullItems) != 3 {
		t.Fatalf("应 3 台节点，got %d", len(fullItems))
	}

	var names []string
	target := "/v1/nodes?limit=2"
	for page := 0; page < 10; page++ {
		rr := env.do(http.MethodGet, target, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		items, ok := doc["items"].([]any)
		if !ok {
			t.Fatalf("items 必须是数组: %T", doc["items"])
		}
		for _, item := range items {
			meta, _ := item.(map[string]any)["metadata"].(map[string]any)
			names = append(names, fmt.Sprint(meta["name"]))
		}
		meta, _ := doc["metadata"].(map[string]any)
		cont, _ := meta["continue"].(string)
		if cont == "" {
			break
		}
		target = "/v1/nodes?limit=2&continue=" + cont
	}
	want := []string{"node-a", "node-b", "node-c"}
	if len(names) != len(want) {
		t.Fatalf("节点翻页 %v，want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("节点翻页顺序 %v，want %v", names, want)
		}
	}
}

func TestKeysListPagingHTTP(t *testing.T) {
	env := newWiringEnv(t)
	for i := 0; i < 3; i++ {
		if _, _, err := env.svc.Keys().CreateKey(env.userID, fmt.Sprintf("key-%d", i), []string{"notify:send"}); err != nil {
			t.Fatalf("create key: %v", err)
		}
	}

	var ids []string
	target := "/v1/keys?limit=2"
	for page := 0; page < 10; page++ {
		rr := env.do(t, http.MethodGet, target)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		keys, ok := doc["keys"].([]any)
		if !ok || keys == nil {
			t.Fatalf("keys 必须是数组（不是 null）: %T", doc["keys"])
		}
		if int(doc["count"].(float64)) != len(keys) {
			t.Fatalf("count 必须是本页条数，got %v vs %d", doc["count"], len(keys))
		}
		for _, k := range keys {
			ids = append(ids, fmt.Sprint(k.(map[string]any)["id"]))
		}
		cont, _ := doc["continue"].(string)
		if cont == "" {
			break
		}
		target = "/v1/keys?limit=2&continue=" + cont
	}
	if len(ids) != 3 {
		t.Fatalf("翻页应拿到 3 个 key，got %d: %v", len(ids), ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("key 翻页重复: %s", id)
		}
		seen[id] = true
	}

	// 非整数 limit → 400 且错误体是 auth 域的 {error}。
	rr := env.do(t, http.MethodGet, "/v1/keys?limit=abc")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc 应 400，got %d %s", rr.Code, rr.Body.String())
	}
	var errDoc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &errDoc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := errDoc["error"]; !ok {
		t.Fatalf("auth 域 400 必须是 {error}，got %v", errDoc)
	}

	// 别人的游标 → 400。
	other := encodePageCursor("keys|usr_someone_else", "1", "key_x")
	if rr := env.do(t, http.MethodGet, "/v1/keys?continue="+other); rr.Code != http.StatusBadRequest {
		t.Fatalf("跨用户游标应 400，got %d %s", rr.Code, rr.Body.String())
	}
}

func TestAdminUsersListPagingHTTP(t *testing.T) {
	env := newAdminTestEnv(t)

	var ids []string
	target := "/v1/admin/users?limit=1"
	for page := 0; page < 10; page++ {
		rec := adminReq(t, env, http.MethodGet, target, nil, env.adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		users, ok := doc["users"].([]any)
		if !ok || users == nil {
			t.Fatalf("users 必须是数组: %T", doc["users"])
		}
		if len(users) > 1 {
			t.Fatalf("limit=1 却返回 %d 条", len(users))
		}
		for _, u := range users {
			ids = append(ids, fmt.Sprint(u.(map[string]any)["id"]))
		}
		cont, _ := doc["continue"].(string)
		if cont == "" {
			break
		}
		target = "/v1/admin/users?limit=1&continue=" + cont
	}
	if len(ids) != 2 {
		t.Fatalf("应翻到 2 个用户（admin/member），got %d: %v", len(ids), ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("用户翻页出现重复: %v", ids)
	}

	if rec := adminReq(t, env, http.MethodGet, "/v1/admin/users?limit=abc", nil, env.adminCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc 应 400，got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthSessionsAndPasskeysPagingHTTP(t *testing.T) {
	env := newPasskeysTestEnv(t)
	now := store.Now()

	// 4 条额外未过期会话 + 1 条过期会话（过期行必须被过滤，不是靠前端判）。
	var liveIDs []string
	for i := 0; i < 4; i++ {
		s := &store.Session{
			ID: store.NewSessionID(), UserID: env.userID, DeviceName: fmt.Sprintf("dev-%d", i),
			CreatedAt: now + int64(i+1), ExpiresAt: now + 3600, LastSeen: now,
		}
		if err := env.db.CreateSession(s); err != nil {
			t.Fatalf("create session: %v", err)
		}
		liveIDs = append(liveIDs, s.ID)
	}
	expired := &store.Session{
		ID: store.NewSessionID(), UserID: env.userID, DeviceName: "expired",
		CreatedAt: now + 100, ExpiresAt: now - 60, LastSeen: now - 60,
	}
	if err := env.db.CreateSession(expired); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	var got []string
	target := "/v1/auth/sessions?limit=2"
	for page := 0; page < 10; page++ {
		rr := pkReq(t, env, http.MethodGet, target, nil, true)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		list, ok := doc["sessions"].([]any)
		if !ok || list == nil {
			t.Fatalf("sessions 必须是数组: %T", doc["sessions"])
		}
		for _, s := range list {
			id := fmt.Sprint(s.(map[string]any)["id"])
			if id == expired.ID {
				t.Fatal("过期会话不该出现在 /v1/auth/sessions")
			}
			got = append(got, id)
		}
		cont, _ := doc["continue"].(string)
		if cont == "" {
			break
		}
		target = "/v1/auth/sessions?limit=2&continue=" + cont
	}
	// 夹具自带 1 条登录会话 + 4 条额外 live = 5；过期那条不算。
	if len(got) != 5 {
		t.Fatalf("会话翻页应拿到 5 条（不含过期），got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("会话翻页重复: %s", id)
		}
		seen[id] = true
	}

	// ---- passkeys：3 条，limit=2 翻两页 ----
	uid := env.userID
	for i := 0; i < 3; i++ {
		p := &store.Passkey{
			ID: fmt.Sprintf("pk_page_%d", i), UserID: uid, PublicKey: []byte{byte(i + 1)},
			Name: fmt.Sprintf("pk-%d", i), Transports: []string{}, CreatedAt: now + int64(i),
		}
		if err := env.db.CreatePasskey(p); err != nil {
			t.Fatalf("create passkey: %v", err)
		}
	}
	var pkIDs []string
	target = "/v1/auth/passkeys?limit=2"
	for page := 0; page < 10; page++ {
		rr := pkReq(t, env, http.MethodGet, target, nil, true)
		if rr.Code != http.StatusOK {
			t.Fatalf("passkey page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		list, ok := doc["passkeys"].([]any)
		if !ok || list == nil {
			t.Fatalf("passkeys 必须是数组: %T", doc["passkeys"])
		}
		for _, p := range list {
			pkIDs = append(pkIDs, fmt.Sprint(p.(map[string]any)["id"]))
		}
		cont, _ := doc["continue"].(string)
		if cont == "" {
			break
		}
		target = "/v1/auth/passkeys?limit=2&continue=" + cont
	}
	if len(pkIDs) != 3 {
		t.Fatalf("passkey 翻页应拿到 3 条，got %d: %v", len(pkIDs), pkIDs)
	}

	// 会话与 passkey 的游标互不通用（binding 不同）→ 400。
	sessCursor := encodePageCursor("sessions|"+uid, "1", "sess_x")
	if rr := pkReq(t, env, http.MethodGet, "/v1/auth/passkeys?continue="+sessCursor, nil, true); rr.Code != http.StatusBadRequest {
		t.Fatalf("跨列表游标应 400，got %d %s", rr.Code, rr.Body.String())
	}
}
