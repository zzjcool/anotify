package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
)

// 本文件覆盖 #235 的存储侧：游标分页查询 ListEventsBefore。
//
// 它守的是一条**等价性**：ListEventsBefore 与 ListEvents 只在「有没有游标谓词」上
// 不同（那是第二份描述「哪些事件属于这个视图」的 SQL，见 event_page.go 顶部注释）。
// 所以这里的第一条用例不是"随便翻几页"，而是把 ListEventsBefore(游标=+∞) 与
// ListEvents 在**四种口径**下逐条比对 —— 将来只改 event.go 的剪枝规则时，
// 只有它能把分叉抓红。

// seedPageEvents 在当前命名空间灌一批可预期的行，返回根事件 trace id。
//
// 数据形状刻意混合四种口径需要的所有维度：
//   - 两个 source（github / cron）：显式 source 过滤有用
//   - 三条 trace：显式 traceID 过滤有用
//   - 两条 source='route' 的剪枝行（走 InsertPruneEvent）：事件流必须剪掉它们，
//     而按 trace 查又必须含它们 —— 这正是最容易分叉的地方
//
// created_at 只到 100n 的量级：远小于哨兵 math.MaxInt64，+∞ 游标因此是"第一页"。
func seedPageEvents(t *testing.T, db *DB, nsID string) {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		traceID, source, typ string
		createdAt            int64
	}{
		{"trace_a", "github", "pull_request.opened", 1000},
		{"trace_a", "github", "pull_request.synchronize", 1001},
		{"trace_b", "github", "pull_request.opened", 1002},
		{"trace_b", "cron", "schedule.tick", 1003},
		{"trace_c", "cron", "schedule.tick", 1004},
		{"trace_c", "github", "issue.comment", 1005},
		{"trace_a", "cron", "schedule.tick", 1006},
		{"trace_b", "github", "issue.comment", 1007},
	}
	for i, r := range rows {
		e := &Event{
			ID:          fmt.Sprintf("evt_seed_%02d", i),
			NamespaceID: nsID,
			TraceID:     r.traceID,
			Source:      r.source,
			Type:        r.typ,
			CreatedAt:   r.createdAt,
		}
		if _, err := db.InsertEvent(ctx, e); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	// 剪枝行（source='route'）：事件流要剪掉、trace 视图要含
	for i, p := range []struct {
		traceID string
		at      int64
	}{
		{"trace_a", 1008},
		{"trace_b", 1009},
	} {
		e := &Event{
			ID:          fmt.Sprintf("evt_prune_%02d", i),
			NamespaceID: nsID,
			TraceID:     p.traceID,
			Source:      "route",
			Type:        "pruned.no_edge",
			Payload:     []byte(prunePayloadNoEdge),
			CreatedAt:   p.at,
		}
		if _, err := db.InsertPruneEvent(ctx, e); err != nil {
			t.Fatalf("seed prune event %d: %v", i, err)
		}
	}
}

// idsOf 取出事件 id 列表（保序）。
func idsOf(events []*Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

// TestListEventsBeforeEquivalence 是**等价性**断言（event_page.go 顶部注释点名的那条）。
//
// 判据：ListEventsBefore(游标 = +∞, limit = N) 与 ListEvents(limit = N) 在四种口径下
// **逐条 id 相同、顺序相同**。+∞ 哨兵的理由：created_at 是 int64 秒级时间戳，
// 不可能等于 math.MaxInt64（那是 2920 亿年后），所以 `created_at < MaxInt64` 恒真、
// `id < ""` 永不参与 ⇒ 谓词退化成恒真，两者只差"有没有那段 SQL"。
func TestListEventsBeforeEquivalence(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	seedPageEvents(t, db, nsID)

	// 空命名空间（纯事件流口径下的"什么都没有"）：两种写法同样要一致
	emptyNS := newTestExtraNS(t, db, "empty-space")

	flavors := []struct {
		name     string
		nsID     string
		traceID  string
		source   string
		limit    int
		minCount int // 断言等价之外，顺带钉住"这一口径确实筛到了东西/确实剪了枝"
	}{
		// ① 纯事件流：不带 traceId / source（数据里没有 route 行）
		{"纯事件流（无剪枝数据）", emptyNS, "", "", 50, 0},
		// ② source=""+traceID=""：事件流口径，但数据里**有** source=route 的剪枝行
		{"事件流（source 与 traceId 都空，数据含剪枝行）", nsID, "", "", 50, 8},
		// ③ 显式 source
		{"显式 source=cron", nsID, "", "cron", 50, 3},
		{"显式 source=route（剪枝本身也是一类事件）", nsID, "", "route", 50, 2},
		// ④ 显式 traceID（含剪枝行）
		{"显式 traceId=trace_a（含剪枝）", nsID, "trace_a", "", 50, 4},
	}
	for _, f := range flavors {
		t.Run(f.name, func(t *testing.T) {
			want, err := db.ListEvents(ctx, f.nsID, f.traceID, f.source, f.limit)
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			got, err := db.ListEventsBefore(ctx, f.nsID, f.traceID, f.source, math.MaxInt64, "", f.limit)
			if err != nil {
				t.Fatalf("ListEventsBefore: %v", err)
			}
			if len(want) != f.minCount {
				t.Fatalf("ListEvents 返回 %d 条, want %d（夹具自己变了？口径断言失去意义）", len(want), f.minCount)
			}
			if len(got) != len(want) {
				t.Fatalf("条数不一致：ListEventsBefore=%d, ListEvents=%d（两份 SQL 分叉了）",
					len(got), len(want))
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Fatalf("第 %d 条对不上：ListEventsBefore=%s, ListEvents=%s（顺序或剪枝规则分叉）\n"+
						"before=%v\nwant  =%v", i, got[i].ID, want[i].ID, idsOf(got), idsOf(want))
				}
			}
		})
	}

	// limit 夹取也必须同一套规则（契约 §3.1 专门点了这一条：`> 500` 究竟是 500 还是 50？）。
	// 答案是**与 ListEvents 保持同一实现**，即 `<= 0 || > 500 → 50`（那个折回是已知缺陷
	// #236，但 event.go 一行不动，所以本函数原样复刻）。这条子用例把「复刻得住」钉住：
	// 两边给同样的非法 limit，必须返回同样多的行 —— 将来有人只改一边的夹取就会红。
	// 需要一个"行数 > 折回值 50"的命名空间，才看得出 50 这个数（否则两边都是"全部行"）。
	clampNS := newTestExtraNS(t, db, "clamp-space")
	for i := 0; i < 60; i++ {
		if _, err := db.InsertEvent(ctx, &Event{
			ID:          fmt.Sprintf("evt_clamp_%02d", i),
			NamespaceID: clampNS,
			TraceID:     fmt.Sprintf("trace_clamp_%02d", i),
			Source:      "github",
			Type:        "push",
			CreatedAt:   int64(3000 + i),
		}); err != nil {
			t.Fatalf("seed clamp event %d: %v", i, err)
		}
	}
	for _, badLimit := range []int{0, -1, 501, 1000} {
		t.Run(fmt.Sprintf("limit 夹取一致（limit=%d）", badLimit), func(t *testing.T) {
			want, err := db.ListEvents(ctx, clampNS, "", "", badLimit)
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			got, err := db.ListEventsBefore(ctx, clampNS, "", "", math.MaxInt64, "", badLimit)
			if err != nil {
				t.Fatalf("ListEventsBefore: %v", err)
			}
			if len(want) != 50 {
				t.Fatalf("ListEvents(limit=%d) 返回 %d 条, want 50（store 的折回规则）", badLimit, len(want))
			}
			if len(got) != len(want) {
				t.Fatalf("limit=%d 时条数不一致：ListEventsBefore=%d, ListEvents=%d（夹取规则分叉）",
					badLimit, len(got), len(want))
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Fatalf("limit=%d 第 %d 条对不上：%s vs %s", badLimit, i, got[i].ID, want[i].ID)
				}
			}
		})
	}

	// 分页窗口的等价性：从某条已知行往后取，应该正好等于 ListEvents 去掉更旧那一段。
	t.Run("游标窗口 = ListEvents 的后缀", func(t *testing.T) {
		all, err := db.ListEvents(ctx, nsID, "", "", 50)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		// 以第 3 条为锚（下标 3），往后取 2 条应为 all[4:6]
		anchor := all[3]
		got, err := db.ListEventsBefore(ctx, nsID, "", "", anchor.CreatedAt, anchor.ID, 50)
		if err != nil {
			t.Fatalf("ListEventsBefore: %v", err)
		}
		want := all[4:]
		if len(got) != len(want) {
			t.Fatalf("游标后 %d 条, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("第 %d 条对不上：got=%s want=%s", i, got[i].ID, want[i].ID)
			}
		}
	})
}

// TestListEventsBeforeCursorSameCreatedAt 是**游标边界**断言（同 created_at 多行）。
//
// 为什么这条最要紧：同一秒批量落库是常态（一次扇出里好几条事件同 created_at），
// 只用 `created_at < ?` 做游标会在页边界**整段丢行**（同 created_at 的剩余行永远取不到）；
// 破平局必须带 id。这里用 7+5+3 个同 created_at 的分组，页大小 1 / 3 / 全量 三种都翻一遍：
// 并集 == 全量集合、无重复、无遗漏。
func TestListEventsBeforeCursorSameCreatedAt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)

	// 三组同 created_at：id 按字典序可预期，方便断言"同秒内按 id DESC"
	groups := []struct {
		at    int64
		count int
	}{
		{2000, 7},
		{2001, 5},
		{2002, 3},
	}
	total := 0
	for _, g := range groups {
		for i := 0; i < g.count; i++ {
			e := &Event{
				ID:          fmt.Sprintf("evt_%d_%02d", g.at, i),
				NamespaceID: nsID,
				TraceID:     "trace_same",
				Source:      "github",
				Type:        "push",
				CreatedAt:   g.at,
			}
			if _, err := db.InsertEvent(ctx, e); err != nil {
				t.Fatalf("seed: %v", err)
			}
			total++
		}
	}

	full, err := db.ListEvents(ctx, nsID, "", "", 500)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(full) != total {
		t.Fatalf("全量 %d 条, want %d", len(full), total)
	}
	wantIDs := map[string]bool{}
	for _, e := range full {
		wantIDs[e.ID] = true
	}

	for _, pageSize := range []int{1, 3, total} {
		t.Run(fmt.Sprintf("页大小=%d", pageSize), func(t *testing.T) {
			seen := map[string]int{}
			var order []*Event
			beforeAt, beforeID := int64(math.MaxInt64), ""
			for pages := 0; ; pages++ {
				if pages > total+2 {
					t.Fatalf("翻页没有收敛（死循环）：已翻 %d 页", pages)
				}
				page, err := db.ListEventsBefore(ctx, nsID, "", "", beforeAt, beforeID, pageSize)
				if err != nil {
					t.Fatalf("ListEventsBefore: %v", err)
				}
				if len(page) == 0 {
					break
				}
				if len(page) > pageSize {
					t.Fatalf("一页 %d 条, 超过页大小 %d", len(page), pageSize)
				}
				for _, e := range page {
					seen[e.ID]++
					order = append(order, e)
				}
				last := page[len(page)-1]
				beforeAt, beforeID = last.CreatedAt, last.ID
			}

			// 无重复
			for id, n := range seen {
				if n > 1 {
					t.Fatalf("id %s 出现了 %d 次（页边界重复）", id, n)
				}
			}
			// 无遗漏
			for id := range wantIDs {
				if seen[id] == 0 {
					t.Fatalf("id %s 一页都没被翻到（页边界丢行）", id)
				}
			}
			if len(seen) != total {
				t.Fatalf("并集 %d 条, want %d", len(seen), total)
			}
			// 翻页顺序整体仍是 created_at DESC, id DESC
			for i := 1; i < len(order); i++ {
				prev, cur := order[i-1], order[i]
				if prev.CreatedAt < cur.CreatedAt ||
					(prev.CreatedAt == cur.CreatedAt && prev.ID < cur.ID) {
					t.Fatalf("第 %d 条破坏了 created_at DESC, id DESC：%s@%d → %s@%d",
						i, prev.ID, prev.CreatedAt, cur.ID, cur.CreatedAt)
				}
			}
			// 与全量逐条一致（顺序也一样）
			if len(order) != len(full) {
				t.Fatalf("翻页总数 %d, want %d", len(order), len(full))
			}
			for i := range full {
				if order[i].ID != full[i].ID {
					t.Fatalf("第 %d 条与全量不一致：翻页=%s, 全量=%s", i, order[i].ID, full[i].ID)
				}
			}
		})
	}
}

// TestListEventsBeforeEmptyIsSlice 空结果必须是 []*Event{} 而不是 nil（不变量 I9）。
//
// 判据是 `out != nil && len(out) == 0`：nil 切片编成 JSON 是 `null`，
// 前端拿到 `null` 会当成"没连上"，而不是"这一页是空的"。
func TestListEventsBeforeEmptyIsSlice(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)

	// ① 空命名空间
	out, err := db.ListEventsBefore(ctx, nsID, "", "", math.MaxInt64, "", 10)
	if err != nil {
		t.Fatalf("ListEventsBefore: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("空命名空间返回 out=%v (nil=%v), want 非 nil 空切片", out, out == nil)
	}

	// ② 有数据，但游标比最旧的一条还旧 → 同样空
	seedPageEvents(t, db, nsID)
	out, err = db.ListEventsBefore(ctx, nsID, "", "", 1, "", 10)
	if err != nil {
		t.Fatalf("ListEventsBefore: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("翻到最旧之后返回 out=%v (nil=%v), want 非 nil 空切片", out, out == nil)
	}

	// ③ 命名空间缺失仍是错误（不是空切片）：`*` 不能传进 store
	if _, err := db.ListEventsBefore(ctx, AllNamespaces, "", "", math.MaxInt64, "", 10); err == nil {
		t.Fatal("namespaceId=* 应返回 ErrNamespaceRequired，不能静默当空结果")
	}
}

// TestListEventsBeforeUsesRetentionIndex 断言游标查询走 idx_events_retention。
//
// 为什么必须钉住：游标分页是**每次翻页一次**的查询，退化成全表扫会让第 10 页比第 1 页
// 慢一个数量级（events 表是最大的表）。而"用上索引"这件事没有任何功能测试能发现。
//
// 断言跑在**真实语句**上（eventPageQuery 是 ListEventsBefore 的 SQL 生成器，
// 不是测试里抄的一份副本）：换谓词/换排序当场会被这里逮住。
//
// 实测计划（modernc.org/sqlite v1.45.0 / SQLite 3.5x，见下面 log 输出）：
//
//	SEARCH events USING INDEX idx_events_retention (namespace_id=?)
//	USE TEMP B-TREE FOR LAST TERM OF ORDER BY
//
// 也就是说：范围/排序的主干走 idx_events_retention，只在 id 破平局那一项上
// 用临时 B 树（(namespace_id, created_at) 索引里没有 id，这是预期且可接受的）。
// 因此断言写成「含 idx_events_retention」+「不含『SCAN events』」——
// 后者才是退化信号（全表扫）。`SCAN events USING INDEX ...` 这种"索引全扫"也算退化，
// 所以下面用**独立词**匹配，不用简单子串。
func TestListEventsBeforeUsesRetentionIndex(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	seedPageEvents(t, db, nsID)

	q, args := eventPageQuery(nsID, "", "", math.MaxInt64, "", 10)
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	detailAt := -1
	for i, c := range cols {
		if c == "detail" {
			detailAt = i
		}
	}
	if detailAt < 0 {
		detailAt = len(cols) - 1 // 老版本 SQLite 也是最后一列
	}

	var details []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		switch v := vals[detailAt].(type) {
		case []byte:
			details = append(details, string(v))
		case string:
			details = append(details, v)
		case nil:
			details = append(details, "")
		default:
			details = append(details, fmt.Sprintf("%v", v))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(details) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN 没有返回任何行")
	}
	plan := ""
	for _, d := range details {
		plan += d + "\n"
	}
	t.Logf("实测计划：\n%s", plan)

	if !strings.Contains(plan, "idx_events_retention") {
		t.Fatalf("计划里没有 idx_events_retention（游标分页会退化成全表扫）:\n%s", plan)
	}
	for _, line := range details {
		// "SCAN events"（全表扫）与 "SCAN events USING INDEX ..."（索引全扫）都算退化；
		// "SEARCH events USING INDEX ..." 才是我们想要的。
		if strings.Contains(line, "SCAN events") {
			t.Fatalf("计划里有全表/索引全扫，游标分页会随事件量线性变慢:\n%s", plan)
		}
	}
}
