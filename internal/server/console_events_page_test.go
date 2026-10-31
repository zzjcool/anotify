package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖 #235 的 HTTP 侧：`GET /v1/console/events` 的游标分页（`before` / `nextCursor`）。
//
// 夹具沿用 console_test.go 的 newConsoleEnv（真 SQLite + 真会话中间件 + 真 mux），
// 不另造一套 —— 这里要证明的是「同一个 handler 在带/不带 before 时说的话一致」。
//
// 判据全部落在**可观测事实**上：并集无重复、无遗漏、每页内部 created_at DESC、
// nextCursor 为空 ⇔ **服务端已确认**没有更旧的行（多看一行；`limit=500` 时退化为旧判据，
// 见 TestConsoleEventsCursorDegradesAtLimitCap）、坏游标 → 400。

// eventsPage 是 /v1/console/events 响应的形状（只取本套件关心的字段）。
//
// limit / nextCursor 用指针接：字段**缺失**与「值恰好是零值」必须能区分开——
// #236 / #235 要证明的正是「生效值被回显」「游标被回显」，字段缺失时不能悄悄通过。
type eventsPage struct {
	Events      []store.Event `json:"events"`
	NamespaceID string        `json:"namespaceId"`
	Limit       *int          `json:"limit"`
	NextCursor  *string       `json:"nextCursor"`
}

// getEventsPage 发一次 GET /v1/console/events 并解出 eventsPage。
func (e *consoleEnv) getEventsPage(t *testing.T, path string) eventsPage {
	t.Helper()
	rr := e.do(t, http.MethodGet, path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s → %d, body=%s", path, rr.Code, rr.Body.String())
	}
	var page eventsPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v (body=%s)", path, err, rr.Body.String())
	}
	if page.Limit == nil {
		t.Fatalf("GET %s 的响应没有 limit 字段（生效值必须回显，#236）", path)
	}
	if page.NextCursor == nil {
		t.Fatalf("GET %s 的响应没有 nextCursor 字段（#235）", path)
	}
	return page
}

// seedConsoleEvents 往指定命名空间灌 n 条事件，created_at 从 base 起按 step 递增。
//
// 显式给 ID（`<tag>_evt_<i>`）与 created_at：分页断言要能说清「哪一条在第几页」。
// step 用来在跨命名空间场景里**交错**时间轴（两个命名空间各取奇偶），
// 这样「每个命名空间各按 before 查、合并、再截断」这条路径才会真正被打到。
func seedConsoleEvents(t *testing.T, db *store.DB, nsID, tag string, n int, base, step int64) []*store.Event {
	t.Helper()
	ctx := context.Background()
	out := make([]*store.Event, 0, n)
	for i := 0; i < n; i++ {
		e := &store.Event{
			ID:          fmt.Sprintf("%s_evt_%03d", tag, i),
			NamespaceID: nsID,
			TraceID:     fmt.Sprintf("trace_%s_%03d", tag, i),
			Source:      "github",
			Type:        "push",
			Payload:     json.RawMessage(`{}`),
			CreatedAt:   base + int64(i)*step,
		}
		if _, err := db.InsertEvent(ctx, e); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
		out = append(out, e)
	}
	return out
}

// assertStrictlyDesc 断言一页内部按 created_at DESC（同秒按 id DESC）。
//
// 「每页内部有序」独立于「翻页无重复/无遗漏」：前者塌了说明合并/排序错了，
// 后者塌了说明游标谓词错了，两种失败的归因完全不同。
func assertStrictlyDesc(t *testing.T, page []store.Event, where string) {
	t.Helper()
	for i := 1; i < len(page); i++ {
		prev, cur := page[i-1], page[i]
		if prev.CreatedAt < cur.CreatedAt ||
			(prev.CreatedAt == cur.CreatedAt && prev.ID < cur.ID) {
			t.Fatalf("%s 第 %d 条破坏了 created_at DESC, id DESC：%s@%d → %s@%d",
				where, i, prev.ID, prev.CreatedAt, cur.ID, cur.CreatedAt)
		}
	}
}

// collectPages 从「第一页」开始按 nextCursor 一路翻到底，返回按页展开的全部事件。
//
// nsID 显式传入（不固定用默认命名空间）：`namespaceId=*` 的跨命名空间翻页要用同一套
// 翻页逻辑，否则两条路径的判据会有两份实现。
// 上限 100 页：翻不收敛（游标没前进）时必须当场失败，而不是把测试跑成死循环。
func (e *consoleEnv) collectPages(t *testing.T, path, nsID string, limit int) [][]store.Event {
	t.Helper()
	pages := make([][]store.Event, 0, 4)
	cursor := ""
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatalf("翻页没有收敛（游标没前进？），已翻 %d 页", i)
		}
		q := fmt.Sprintf("%s?limit=%d", path, limit)
		if cursor != "" {
			q += "&before=" + cursor // URL 安全 base64：不需要再转义
		}
		page := e.getEventsPage(t, withNSID(q, nsID))
		if len(page.Events) > limit {
			t.Fatalf("第 %d 页返回 %d 条, 超过 limit=%d", i+1, len(page.Events), limit)
		}
		if *page.Limit != limit {
			t.Fatalf("第 %d 页响应 limit = %d, want %d", i+1, *page.Limit, limit)
		}
		assertStrictlyDesc(t, page.Events, fmt.Sprintf("第 %d 页", i+1))
		pages = append(pages, page.Events)
		if *page.NextCursor == "" {
			return pages
		}
		// 不满一页却给了游标：行数 < 生效 limit 就一定没有更旧的行了
		// （反方向不成立：满页也可能已经到底，那正是下面「多看一行」要分辨的）
		if len(page.Events) < limit {
			t.Fatalf("第 %d 页只有 %d 条（< limit=%d）却给了 nextCursor=%q",
				i+1, len(page.Events), limit, *page.NextCursor)
		}
		cursor = *page.NextCursor
	}
}

// flattenPages 把按页的分组摊平成一串，顺带统计每个 id 出现次数。
func flattenPages(pages [][]store.Event) ([]store.Event, map[string]int) {
	all := make([]store.Event, 0)
	seen := map[string]int{}
	for _, p := range pages {
		for _, e := range p {
			all = append(all, e)
			seen[e.ID]++
		}
	}
	return all, seen
}

// TestConsoleEventsCursorPaging 是 #235 的主用例：250 条事件按 limit=100 翻三页（100/100/50）。
//
// 证明四件事：
//   - 页大小正确（100/100/50），最后一页 nextCursor 为空（到最旧了）；
//   - 三页并集 == 库里全部 250 条，**无重复、无遗漏**（游标在页边界不丢行，这是
//     「同 created_at 只按时间比较」最容易踩的坑）；
//   - 每页内部严格 created_at DESC；
//   - 不满一页不给游标；最后一页（50 条）游标为空。满页是否给游标看「还有没有下一行」
//     （服务端多看一行确认，见 TestConsoleEventsNoEmptyPageOnExactFullPage /
//     TestConsoleEventsLastPageOneRow 两条边界）。
func TestConsoleEventsCursorPaging(t *testing.T) {
	env := newConsoleEnv(t)
	seeded := seedConsoleEvents(t, env.db, env.nsID, "page", 250, 1700000000, 1)

	pages := env.collectPages(t, "/v1/console/events", env.nsID, 100)
	if len(pages) != 3 {
		t.Fatalf("翻了 %d 页, want 3（250 条 / 100）", len(pages))
	}
	for i, want := range []int{100, 100, 50} {
		if len(pages[i]) != want {
			t.Fatalf("第 %d 页 %d 条, want %d", i+1, len(pages[i]), want)
		}
	}

	all, seen := flattenPages(pages)
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("id %s 出现在 %d 页里（游标页边界重复）", id, n)
		}
	}
	if len(all) != len(seeded) {
		t.Fatalf("三页共 %d 条, want %d", len(all), len(seeded))
	}
	// 无遗漏：库里每一条都被翻到过；且整体仍是全局 DESC（跨页单调）
	byID := map[string]*store.Event{}
	for _, e := range seeded {
		byID[e.ID] = e
	}
	for id := range byID {
		if seen[id] == 0 {
			t.Fatalf("id %s 一页都没翻到（游标丢行）", id)
		}
	}
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if prev.CreatedAt < cur.CreatedAt ||
			(prev.CreatedAt == cur.CreatedAt && prev.ID < cur.ID) {
			t.Fatalf("跨页顺序破坏 created_at DESC（第 %d 条）：%s → %s", i, prev.ID, cur.ID)
		}
	}

	// 「回到最新」再翻一次：第一页必须是同样的最新 100 条（游标不改变首屏窗口）
	first := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100"))
	if len(first.Events) != 100 || first.Events[0].ID != all[0].ID {
		t.Fatalf("首屏第一条 = %q, want %q（无 before 的第一页）", first.Events[0].ID, all[0].ID)
	}
}

// TestConsoleEventsLimitCap limit=1000 被夹到 500 且响应回显生效值（#236 / V2）。
//
// 顺带覆盖 nextCursor 的两种形态：250 条 + limit=500 → 不满一页 → ""；
// limit=100 → 满页 → 非空。
func TestConsoleEventsLimitCap(t *testing.T) {
	env := newConsoleEnv(t)
	seedConsoleEvents(t, env.db, env.nsID, "cap", 250, 1700000000, 1)

	page := env.getEventsPage(t, env.withNS("/v1/console/events?limit=1000"))
	if *page.Limit != 500 {
		t.Fatalf("响应 limit = %d, want 500（超硬顶要钳到 500，#236）", *page.Limit)
	}
	if len(page.Events) != 250 {
		t.Fatalf("返回 %d 条, want 250（全部事件，少于硬顶）", len(page.Events))
	}
	if len(page.Events) > 500 {
		t.Fatalf("返回 %d 条, 超过硬顶 500", len(page.Events))
	}
	// 不满一页 ⇒ 没有更旧的了
	if *page.NextCursor != "" {
		t.Fatalf("不满一页时 nextCursor = %q, want \"\"", *page.NextCursor)
	}

	// 满页 ⇒ 必须给游标（否则前端点不到「更旧」）
	full := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100"))
	if len(full.Events) != 100 {
		t.Fatalf("limit=100 返回 %d 条, want 100", len(full.Events))
	}
	if *full.NextCursor == "" {
		t.Fatal("满页（100/100）却没有 nextCursor：翻不到更旧的一页")
	}

	// 空库/空窗口：不足一页 → ""（同样的判据，不是特例）
	empty := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100&source=nothing"))
	if len(empty.Events) != 0 || *empty.NextCursor != "" {
		t.Fatalf("空窗口 events=%d nextCursor=%q, want 0 和 \"\"", len(empty.Events), *empty.NextCursor)
	}
}

// TestConsoleEventsInvalidCursor before 解析失败 → 400 {"error":"invalid before cursor"}。
//
// **不静默降级**是这条用例的全部意义：把坏游标当「没有游标」会让翻页莫名跳回最新，
// 那正是 #236 的病根（悄悄换一个窗口），不能在新参数上重犯。
func TestConsoleEventsInvalidCursor(t *testing.T) {
	env := newConsoleEnv(t)
	seedConsoleEvents(t, env.db, env.nsID, "bad", 5, 1700000000, 1)

	cases := []struct {
		name   string
		before string
	}{
		{"纯乱码", "not-a-cursor!!"},
		{"base64 合法但缺分隔符", "MTcwMDAwMDAwMA"},                // "1700000000"
		{"createdAt 不是整数", "YWJjOmV2dF8x"},                  // "abc:evt_1"
		{"id 为空", "MTcwMDAwMDAwMDo="},                       // "1700000000:"
		{"createdAt 为负", "LTE6ZXZ0XzE="},                    // "-1:evt_1"
		{"标准 base64 带 padding", "MTcwMDAwMDAwMDpldnRfMQ=="}, // RawURLEncoding 不接受 padding
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := env.do(t, http.MethodGet,
				env.withNS("/v1/console/events?limit=10&before="+c.before), nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400；body=%s", rr.Code, rr.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
			}
			if body["error"] != "invalid before cursor" {
				t.Fatalf("error = %v, want \"invalid before cursor\"", body["error"])
			}
			// 不能顺手把事件也返回（400 就是 400，不带半页数据）
			if _, ok := body["events"]; ok {
				t.Fatalf("400 响应里不该有 events：%s", rr.Body.String())
			}
		})
	}

	// 合法游标的对照组：同一个 (createdAt,id) 编码后必须能用（否则上面的 400 是假绿）
	events, err := env.db.ListEvents(context.Background(), env.nsID, "", "", 5)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	last := events[len(events)-1]
	good := encodeEventCursor(last.CreatedAt, last.ID)
	rr := env.do(t, http.MethodGet, env.withNS("/v1/console/events?limit=10&before="+good), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("合法游标 status = %d, want 200；body=%s", rr.Code, rr.Body.String())
	}
}

// TestConsoleEventsCursorAcrossNamespaces namespaceId=* + before：跨两个命名空间翻页，
// 并集无重复、无遗漏。
//
// 为什么这条最容易被写错：`*` 是「每个命名空间各查一次、合并、再截断到 limit」。
// 游标必须对**每一个**命名空间都按同一个 (created_at,id) 下界去查 —— 若只给某一个
// 命名空间传 before（或每个命名空间用自己的「本页最后一行」），合并结果就会缺行或重行。
// 夹具把两个命名空间的时间轴**交错**（ns2 取奇数秒），使任何「按命名空间分组翻页」
// 的实现都会当场露馅。
func TestConsoleEventsCursorAcrossNamespaces(t *testing.T) {
	env := newConsoleEnv(t)
	other := env.seedNamespace(t, "other-space", "另一个空间")

	nsA := seedConsoleEvents(t, env.db, env.nsID, "a", 130, 1700000000, 2) // 偶数秒
	nsB := seedConsoleEvents(t, env.db, other.ID, "b", 120, 1700000001, 2) // 奇数秒
	total := len(nsA) + len(nsB)

	pages := env.collectPages(t, "/v1/console/events", store.AllNamespaces, 100)
	if len(pages) != 3 {
		t.Fatalf("翻了 %d 页, want 3（%d 条 / 100）", len(pages), total)
	}
	all, seen := flattenPages(pages)
	if len(all) != total {
		t.Fatalf("跨命名空间翻页共 %d 条, want %d", len(all), total)
	}
	for _, e := range append(append([]*store.Event{}, nsA...), nsB...) {
		if seen[e.ID] == 0 {
			t.Fatalf("id %s（命名空间 %s）一页都没翻到", e.ID, e.NamespaceID)
		}
		if seen[e.ID] > 1 {
			t.Fatalf("id %s 出现了 %d 次（跨命名空间页边界重复）", e.ID, seen[e.ID])
		}
	}
	// 合并后仍必须是全局 DESC（两个命名空间交错的时间轴被正确归并）
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if prev.CreatedAt < cur.CreatedAt ||
			(prev.CreatedAt == cur.CreatedAt && prev.ID < cur.ID) {
			t.Fatalf("跨命名空间顺序破坏 created_at DESC（第 %d 条）：%s@%d → %s@%d",
				i, prev.ID, prev.CreatedAt, cur.ID, cur.CreatedAt)
		}
	}
	// 每一页都应当**同时**含两个命名空间的事件（交错时间轴 + 合并后截断的证明）
	for i, p := range pages {
		nss := map[string]bool{}
		for _, e := range p {
			nss[e.NamespaceID] = true
		}
		if len(nss) != 2 {
			t.Fatalf("第 %d 页只含 %d 个命名空间的事件, want 2（说明没合并/截断错了）", i+1, len(nss))
		}
	}
}

// TestConsoleEventsNoEmptyPageOnExactFullPage 是「多看一行」的核心用例（#235 收尾）：
// 库里正好 200 条、页大小 100 时，第 2 页（第 101..200 条）必须给 `nextCursor == ""`。
//
// 旧实现只按「行数 >= limit ⇒ 给游标」判「还有更旧的」，于是第 2 页满 100 条就误报
// 「还有更旧的」，用户点「更旧」会先看到**一个空页**，再确认「已到最旧」。
// 契约 §3.2 的意图是「nextCursor 为空 ⇔ 真的没有更旧的行」，所以这里断言的是
// 「第 2 页满页但游标为空」——满页与「还有更旧」不是同一件事。
func TestConsoleEventsNoEmptyPageOnExactFullPage(t *testing.T) {
	env := newConsoleEnv(t)
	seedConsoleEvents(t, env.db, env.nsID, "exact", 200, 1700000000, 1)

	first := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100"))
	if len(first.Events) != 100 {
		t.Fatalf("第 1 页 %d 条, want 100", len(first.Events))
	}
	if *first.NextCursor == "" {
		t.Fatal("第 1 页之后还有 100 条更旧的行，却给了空 nextCursor")
	}

	second := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100&before="+*first.NextCursor))
	if len(second.Events) != 100 {
		t.Fatalf("第 2 页 %d 条, want 100（库里正好 200 条）", len(second.Events))
	}
	// 旧实现这里是非空 → 前端会多显示一个「更旧」，点了只得到空页。
	if *second.NextCursor != "" {
		t.Fatalf("第 2 页已是最后一页（200/200），nextCursor = %q，want \"\"（否则用户要多看一个空页）",
			*second.NextCursor)
	}

	// 反向确认：接口**确实**没有更旧的行了（空游标不是「查询出错悄悄返回空」）。
	// 用第 2 页末行的 (created_at,id) 手工造一个游标再翻，应当得到 0 条。
	last := second.Events[len(second.Events)-1]
	beyond := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100&before="+
		encodeEventCursor(last.CreatedAt, last.ID)))
	if len(beyond.Events) != 0 || *beyond.NextCursor != "" {
		t.Fatalf("越过最旧一行后 events=%d nextCursor=%q, want 0 和 \"\"",
			len(beyond.Events), *beyond.NextCursor)
	}
}

// TestConsoleEventsLastPageOneRow 是「多看一行」的另一侧边界：201 条 / limit=100。
//
// 第 2 页满 100 条时**确实**还有 1 条更旧的 ⇒ 必须给游标；第 3 页只有 1 条且游标为空。
// 这条与上一条互为对照：只断言「满页不给游标」会掩盖「其实还有一行」的漏行 bug。
func TestConsoleEventsLastPageOneRow(t *testing.T) {
	env := newConsoleEnv(t)
	seedConsoleEvents(t, env.db, env.nsID, "one", 201, 1700000000, 1)

	first := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100"))
	if len(first.Events) != 100 || *first.NextCursor == "" {
		t.Fatalf("第 1 页 events=%d nextCursor=%q, want 100 和 非空",
			len(first.Events), *first.NextCursor)
	}
	second := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100&before="+*first.NextCursor))
	if len(second.Events) != 100 {
		t.Fatalf("第 2 页 %d 条, want 100", len(second.Events))
	}
	// 第 2 页满页，且库里还剩第 201 条 ⇒ 游标必须非空（多看一行才看得见的那一条）。
	if *second.NextCursor == "" {
		t.Fatal("后面还有 1 条更旧的行（201 条中的第 201 条），却给了空 nextCursor（漏行）")
	}
	third := env.getEventsPage(t, env.withNS("/v1/console/events?limit=100&before="+*second.NextCursor))
	if len(third.Events) != 1 {
		t.Fatalf("第 3 页 %d 条, want 1", len(third.Events))
	}
	if *third.NextCursor != "" {
		t.Fatalf("第 3 页（最后 1 条）nextCursor = %q, want \"\"", *third.NextCursor)
	}
}

// TestConsoleEventsCursorDegradesAtLimitCap 记录一个**已知上限**（不需要修，只防回归）。
//
// 「多看一行」要 `perNS = limit+1`，但 store 的列表查询对 `limit > 500` 会兜底成 50
// （#236 的病根，不能改），所以 `perNS` 必须 ≤ listLimitCap：`limit == 500` 时
// `perNS == limit`，取不到那多出来的一行 ⇒ 只能退化成旧判据（行数 >= limit ⇒ 给游标）。
// 因此「库里正好 500 条 + limit=500」仍会多给一个游标（用户会多看一个空页），
// 这是硬顶带来的已知限制，而不是漏改。
func TestConsoleEventsCursorDegradesAtLimitCap(t *testing.T) {
	env := newConsoleEnv(t)
	seedConsoleEvents(t, env.db, env.nsID, "capfull", listLimitCap, 1700000000, 1)

	page := env.getEventsPage(t, env.withNS(fmt.Sprintf("/v1/console/events?limit=%d", listLimitCap)))
	if *page.Limit != listLimitCap {
		t.Fatalf("响应 limit = %d, want %d", *page.Limit, listLimitCap)
	}
	if len(page.Events) != listLimitCap {
		t.Fatalf("返回 %d 条, want %d", len(page.Events), listLimitCap)
	}
	// 已知退化：拿不到「多一行」，无法确认，只能按旧判据给游标。
	if *page.NextCursor == "" {
		t.Fatalf("limit=%d 时多看不了下一行，按旧判据（行数 >= limit）应当给游标；给空游标会\n"+
			"掩盖「store 只返回了 500 条、可能还有更旧的」的歧义", listLimitCap)
	}
}
