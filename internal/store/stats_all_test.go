package store

// 「全部命名空间」聚合读（MessageStatsAll）的验收 + 性能证据。
//
// 契约（.reports/overview-allns-contract.md D3/D4）：`/v1/stats?namespace=*`
// 的 store 侧只接受**显式**的 nsID 列表（不接 AllNamespaces），口径与单 ns 共用
// accumulateMessageStats 里的同一份 SQL。本文件钉住五件事：
//
//  1. 逐字段求和 == 两个单 ns 结果之和（total/today/succeededTasks/finishedTasks）；
//  2. Daily 跨 ns 同日**合并成一行**（不重复计同一天）且升序；
//  3. 空 nsID 列表 = 全零 + `daily: []`（不报错）；`*` / 空串元素 = ErrNamespaceRequired；
//  4. 聚合 SQL 的索引计划（EXPLAIN QUERY PLAN 断言，直接跑生产常量，不抄副本）；
//  5. 3 ns × 40k 事件的 benchmark（`-benchtime 1x` 可跑，量出 ms 级成本）。
//
// 时区纪律沿用 stats_tz_test.go：全部用 time.FixedZone + 可注入的 now，
// 绝不依赖 time.Local / TZ 环境变量 / 真实时钟。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// statsAllExtraNS 在**同一个部署**里再建一个命名空间，并给它建一条真实能力行。
//
// tasks.capability_id 是外键且能力属于命名空间，所以跨 ns 不能复用同一个 id
// （复用会撞 capability_versions 的主键，也会让「这条任务属于哪个 ns」失真）。
func statsAllExtraNS(t *testing.T, db *DB, slug string) (nsID string, cap *CapabilityVersion) {
	t.Helper()
	nsID = newTestExtraNS(t, db, slug)
	c := withSpec(mkCap(nsID, "review", 1, nil))
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability in %s: %v", slug, err)
	}
	return nsID, c
}

// seedStatsAllTasks 造 len(phases) 条任务并把第 i 条推进到 phases[i]。
//
// 走真实 CreateTasks（外键/CHECK 都在），再直接改相位——统计只读 phase 列，
// 状态机迁移本身不是本文件的验收对象（stats_tz_test.go 同口径）。
func seedStatsAllTasks(t *testing.T, db *DB, nsID string, cap *CapabilityVersion, phases ...string) {
	t.Helper()
	ctx := context.Background()
	tasks := make([]*Task, 0, len(phases))
	for range phases {
		tasks = append(tasks, mkTask(nsID, "tr_"+NewEventID(), cap))
	}
	if err := db.CreateTasks(ctx, tasks); err != nil {
		t.Fatalf("create tasks in %s: %v", nsID, err)
	}
	for i, phase := range phases {
		pushToPhase(t, db, tasks[i].ID, phase)
	}
}

// mergeDayCountsTest 是**测试侧独立**实现的按 day 求和（不调用生产 mergeDaily）：
// 用生产实现去验证生产实现等于自己证明自己。
func mergeDayCountsTest(groups ...[]DayCount) []DayCount {
	sum := map[string]int{}
	for _, g := range groups {
		for _, dc := range g {
			sum[dc.Day] += dc.Count
		}
	}
	out := make([]DayCount, 0, len(sum))
	for day, n := range sum {
		out = append(out, DayCount{Day: day, Count: n})
	}
	slices.SortFunc(out, func(a, b DayCount) int { return strings.Compare(a.Day, b.Day) })
	return out
}

// TestMessageStatsAllSumsPerNamespace 是主验收：逐字段求和 == 两个单 ns 之和。
//
// 为什么必须逐字段比而不是只比 total：四个计数来自三条不同 SQL（事件两条、任务
// 一条），任何一条在「循环里忘了累加」都会让某一列静默变成 0 或只算最后一个 ns。
func TestMessageStatsAllSumsPerNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, capB := statsAllExtraNS(t, db, "stats-all-b")
	capA := statsTestCapability(t, db, nsA)

	loc := tzFixtureLocations()[0]
	today := localDay(loc, 12, 0, 0)
	yesterday := today.AddDate(0, 0, -1)

	// 事件：A 三条（昨天 1 + 今天 2）、B 两条（今天 1 + 窗口外 1）。
	// 窗口外那条只影响 total（total 没有时间下界），不影响 today/daily。
	insertEventAt(t, db, nsA, yesterday)
	insertEventAt(t, db, nsA, today)
	insertEventAt(t, db, nsA, today.Add(time.Hour))
	insertEventAt(t, db, nsB, today)
	insertEventAt(t, db, nsB, today.AddDate(0, 0, -5))

	// 任务：A 1 成功 / 1 失败 / 1 pending，B 1 成功 / 1 取消 / 1 running。
	// → SucceededTasks 1+1=2，FinishedTasks 2+2=4（pending/running 不进分母）。
	seedStatsAllTasks(t, db, nsA, capA, TaskSucceeded, TaskFailed, TaskPending)
	seedStatsAllTasks(t, db, nsB, capB, TaskSucceeded, TaskCancelled, TaskRunning)

	now := today.Add(2 * time.Hour)
	sinceSec := yesterday.Unix() // 热力图窗口从昨天 00:00 起

	singleA, err := db.messageStats(ctx, nsA, sinceSec, now)
	if err != nil {
		t.Fatalf("messageStats(A): %v", err)
	}
	singleB, err := db.messageStats(ctx, nsB, sinceSec, now)
	if err != nil {
		t.Fatalf("messageStats(B): %v", err)
	}
	// 前提断言：两边都非零，否则「A+B」可能只是 0+0（弱断言会掩盖漏累加）。
	if singleA.Total == 0 || singleB.Total == 0 || singleA.Today == 0 || singleB.SucceededTasks == 0 {
		t.Fatalf("夹具失效：单 ns 结果必须非零，得到 A=%+v B=%+v", *singleA, *singleB)
	}

	all, err := db.messageStatsAll(ctx, []string{nsA, nsB}, sinceSec, now)
	if err != nil {
		t.Fatalf("messageStatsAll: %v", err)
	}
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"Total", all.Total, singleA.Total + singleB.Total},
		{"Today", all.Today, singleA.Today + singleB.Today},
		{"SucceededTasks", all.SucceededTasks, singleA.SucceededTasks + singleB.SucceededTasks},
		{"FinishedTasks", all.FinishedTasks, singleA.FinishedTasks + singleB.FinishedTasks},
	} {
		if tc.got != tc.want {
			t.Errorf("MessageStatsAll.%s = %d, want %d（= A+B 的单 ns 之和）", tc.name, tc.got, tc.want)
		}
	}

	// daily：逐 day 合并 == 两个单 ns 结果的按 day 之和（同日不重复计）。
	wantDaily := mergeDayCountsTest(singleA.Daily, singleB.Daily)
	if !slices.Equal(all.Daily, wantDaily) {
		t.Fatalf("MessageStatsAll.Daily = %v, want %v（按 day 合并）", all.Daily, wantDaily)
	}

	// 顺序无关：ns 列表的顺序不能改变任何一列（Daily 恒升序）。
	rev, err := db.messageStatsAll(ctx, []string{nsB, nsA}, sinceSec, now)
	if err != nil {
		t.Fatalf("messageStatsAll(倒序): %v", err)
	}
	if rev.Total != all.Total || rev.Today != all.Today ||
		rev.SucceededTasks != all.SucceededTasks || rev.FinishedTasks != all.FinishedTasks {
		t.Fatalf("ns 顺序改变了聚合结果：%+v vs %+v", *rev, *all)
	}
	if !slices.Equal(rev.Daily, all.Daily) {
		t.Fatalf("ns 顺序改变了 daily：%v vs %v", rev.Daily, all.Daily)
	}
}

// TestMessageStatsAllEmptyListIsZeroNotError 钉住「没有命名空间」这个合法空集合：
// 全零 + `daily: []`，**不报错**（JSON 里 daily 必须是数组，不能是 null）。
func TestMessageStatsAllEmptyListIsZeroNotError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = newTestNS(t, db)
	now := localDay(tzFixtureLocations()[0], 12, 0, 0)

	for _, ids := range [][]string{nil, {}} {
		s, err := db.messageStatsAll(ctx, ids, 0, now)
		if err != nil {
			t.Fatalf("空 nsID 列表不该报错（ids=%#v）: %v", ids, err)
		}
		if s == nil {
			t.Fatalf("空 nsID 列表返回 nil Stats（ids=%#v）", ids)
		}
		if s.Total != 0 || s.Today != 0 || s.SucceededTasks != 0 || s.FinishedTasks != 0 {
			t.Fatalf("空集合必须是全零，得到 %+v", *s)
		}
		if s.Daily == nil || len(s.Daily) != 0 {
			t.Fatalf("空集合的 daily 必须是非 nil 空切片（wire 上是 []），得到 %#v", s.Daily)
		}
	}

	// 公开入口（真实时钟）同样不把空集合当错误。
	if _, err := db.MessageStatsAll(ctx, []string{}, 0); err != nil {
		t.Fatalf("MessageStatsAll(空列表) err = %v, want nil", err)
	}
}

// TestMessageStatsAllDailyMergesSameDayAscending 钉住热力图口径：不同 ns 的同一天
// 合并成**一行**，且整体升序（371 天窗口不会因为 ns 数量被拉长）。
func TestMessageStatsAllDailyMergesSameDayAscending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := statsAllExtraNS(t, db, "stats-all-daily-b")

	// UTC+5:30：半小时偏移的时区，`Unix() - Hour()*3600` 这类日界写法会在它上面算错。
	loc := tzFixtureLocations()[2]
	today := localDay(loc, 0, 0, 0)
	d1 := today.AddDate(0, 0, -2)
	d2 := today.AddDate(0, 0, -1)

	// 目的是让两个 ns 的「天集合」交错：A 有 d1/today，B 有 d2/today。
	insertEventAt(t, db, nsA, d1.Add(9*time.Hour))
	insertEventAt(t, db, nsA, today.Add(9*time.Hour))
	insertEventAt(t, db, nsB, d2.Add(9*time.Hour))
	insertEventAt(t, db, nsB, today.Add(10*time.Hour))
	insertEventAt(t, db, nsB, today.Add(11*time.Hour))

	s, err := db.messageStatsAll(ctx, []string{nsA, nsB}, d1.Unix(), today.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("messageStatsAll: %v", err)
	}
	want := []DayCount{
		{Day: d1.Format("2006-01-02"), Count: 1},
		{Day: d2.Format("2006-01-02"), Count: 1},
		{Day: today.Format("2006-01-02"), Count: 3}, // A 1 + B 2 合并成一行
	}
	if !slices.Equal(s.Daily, want) {
		t.Fatalf("Daily = %v, want %v（同日合并 + 升序）", s.Daily, want)
	}
	// 同一天只能出现一次：拼接实现会得到 5 行（A 两天 + B 两天，多一行 today）。
	if len(s.Daily) != 3 {
		t.Fatalf("Daily 有 %d 行, want 3（同一 day 必须合并，不能每个 ns 各一行）", len(s.Daily))
	}
	if s.Today != 3 {
		t.Fatalf("Today = %d, want 3（两 ns 今日事件之和）", s.Today)
	}
	if s.Total != 5 {
		t.Fatalf("Total = %d, want 5", s.Total)
	}
}

// TestMessageStatsAllRejectsAllNamespacesElement 守防御口径：store 只接受确定的
// nsID，`*` / 空串（无论出现在哪个位置）一律 ErrNamespaceRequired——`*` 必须在
// server 侧先展开成「逐 ns 授权过的 id 列表」。
func TestMessageStatsAllRejectsAllNamespacesElement(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	now := localDay(tzFixtureLocations()[0], 12, 0, 0)

	for _, ids := range [][]string{
		{AllNamespaces},
		{""},
		{nsA, AllNamespaces}, // 坏元素在合法元素之后也必须当场拒绝（不能半聚合）
		{nsA, ""},
	} {
		if _, err := db.messageStatsAll(ctx, ids, 0, now); !errors.Is(err, ErrNamespaceRequired) {
			t.Fatalf("messageStatsAll(%#v) err = %v, want ErrNamespaceRequired", ids, err)
		}
	}
}

// TestMessageStatsAggregateQueryPlans 断言 5 条聚合语句的索引计划（走哪个索引、
// 有没有全表/索引全扫）。
//
// 为什么必须钉住：stats 是首页每次加载都要打到的读路径，events 是最大的表；退化成
// 全表扫没有任何功能测试能发现（结果一样，只是慢）。断言跑在**生产常量**上
// （statsTodaySQL / statsTasksSQL / statsDailySQL 是 accumulateMessageStats 实际
// 执行的语句），所以换谓词/换索引名当场会在这里红。
//
// 退化判据用「SEARCH … USING INDEX」而不是简单子串：`SCAN events`（全表扫）与
// `SCAN events USING INDEX …`（索引全扫）都算退化。
func TestMessageStatsAggregateQueryPlans(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	insertEventAt(t, db, nsID, localDay(tzFixtureLocations()[0], 9, 0, 0))

	now := localDay(tzFixtureLocations()[0], 12, 0, 0)
	plans := map[string][]string{
		"total": explainQueryPlan(t, db, statsTotalSQL, nsID),
		"today": explainQueryPlan(t, db, statsTodaySQL, nsID, startOfLocalDayUnix(now)),
		"tasks": explainQueryPlan(t, db, statsTasksSQL, TaskSucceeded, nsID, TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned),
		"daily": explainQueryPlan(t, db, statsDailySQL, 0, nsID, 0),
	}
	for _, name := range []string{"total", "today", "tasks", "daily"} {
		t.Logf("%s 计划：%s", name, strings.Join(plans[name], " / "))
	}

	for _, tc := range []struct{ stmt, index string }{
		{"today", "idx_events_retention"},
		{"daily", "idx_events_retention"},
		{"tasks", "idx_tasks_retention"},
	} {
		plan := strings.Join(plans[tc.stmt], "\n")
		if !strings.Contains(plan, tc.index) {
			t.Fatalf("%s 查询没有走 %s（会退化成全表/索引全扫）:\n%s", tc.stmt, tc.index, plan)
		}
		for _, line := range plans[tc.stmt] {
			if strings.Contains(line, "SCAN events") || strings.Contains(line, "SCAN tasks") {
				t.Fatalf("%s 查询计划里有全表/索引全扫，会随事件量线性变慢:\n%s", tc.stmt, plan)
			}
		}
	}
}

// ---------- 性能证据：3 ns × 40k events 的聚合成本 ----------

// statsAllBenchDB 造一个**文件**后备的库（生产也是文件库，WAL + 连接池 8），
// 返回 3 个命名空间 id 与 userID，每个 ns 各有 eventsPerNS 条事件 + 若干任务。
func statsAllBenchDB(tb testing.TB, eventsPerNS int) (db *DB, nsIDs []string, userID string) {
	tb.Helper()
	ctx := context.Background()
	var err error
	db, err = Open(filepath.Join(tb.TempDir(), "stats-all-bench.db"))
	if err != nil {
		tb.Fatalf("open bench db: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	u := &User{ID: NewUserID(), Username: "u-" + NewUserID(), CreatedAt: Now()}
	primary, err := db.RegisterUser(ctx, u)
	if err != nil {
		tb.Fatalf("register user: %v", err)
	}
	userID = u.ID
	nsIDs = []string{primary.ID}
	for i := 2; i <= 3; i++ {
		ns, cerr := db.CreateNamespace(ctx, fmt.Sprintf("bench-%d", i), fmt.Sprintf("bench %d", i))
		if cerr != nil {
			tb.Fatalf("create bench namespace %d: %v", i, cerr)
		}
		nsIDs = append(nsIDs, ns.ID)
	}

	base := time.Now().AddDate(0, 0, -30).Unix()
	for _, nsID := range nsIDs {
		seedStatsBenchEvents(tb, db, nsID, eventsPerNS, base)
		seedStatsBenchTasks(tb, db, nsID, eventsPerNS/80)
	}
	return db, nsIDs, userID
}

// seedStatsBenchEvents 用一条事务批量落 n 条事件。
//
// 为什么不用 InsertEvent：40k×3 次单行事务的种子时间会盖过被测查询本身，而这里要
// 量的只是**读**路径。事件摊到近 30 天，日桶才有 30 行（否则 GROUP BY 只分一组）。
func seedStatsBenchEvents(tb testing.TB, db *DB, nsID string, n int, baseUnix int64) {
	tb.Helper()
	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("begin bench seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		tb.Fatalf("prepare bench seed: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < n; i++ {
		at := baseUnix + int64(i%30)*86400 + int64(i/30)
		if _, err := stmt.Exec(NewEventID(), nsID, "tr_bench", "bench", "tick", "{}",
			EventReasonIngested, at); err != nil {
			tb.Fatalf("seed bench event %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit bench seed: %v", err)
	}
}

// seedStatsBenchTasks 落 n 条真实任务（1/4 succeeded、1/4 failed、其余 pending），
// 让 tasks 聚合的终态集合有真实行可数。
func seedStatsBenchTasks(tb testing.TB, db *DB, nsID string, n int) {
	tb.Helper()
	if n <= 0 {
		return
	}
	ctx := context.Background()
	cap := withSpec(mkCap(nsID, "review", 1, nil))
	if err := db.CreateCapability(ctx, cap); err != nil {
		tb.Fatalf("create bench capability: %v", err)
	}
	tasks := make([]*Task, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, mkTask(nsID, "tr_"+NewEventID(), cap))
	}
	if err := db.CreateTasks(ctx, tasks); err != nil {
		tb.Fatalf("create bench tasks: %v", err)
	}
	for i, tk := range tasks {
		switch i % 4 {
		case 0:
			if _, err := db.Exec(`UPDATE tasks SET phase=? WHERE id=?`, TaskSucceeded, tk.ID); err != nil {
				tb.Fatalf("bench task phase: %v", err)
			}
		case 1:
			if _, err := db.Exec(`UPDATE tasks SET phase=? WHERE id=?`, TaskFailed, tk.ID); err != nil {
				tb.Fatalf("bench task phase: %v", err)
			}
		}
	}
}

// BenchmarkMessageStatsAll 是「全部命名空间」视图的真实成本（3 ns × 40k events）。
//
// 跑法：go test ./internal/store/ -run '^$' -bench 'BenchmarkMessageStats' -benchtime 1x
func BenchmarkMessageStatsAll(b *testing.B) {
	db, nsIDs, _ := statsAllBenchDB(b, 40000)
	ctx := context.Background()
	now := time.Now()
	sinceSec := now.AddDate(0, 0, -371).Unix()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.messageStatsAll(ctx, nsIDs, sinceSec, now); err != nil {
			b.Fatalf("messageStatsAll: %v", err)
		}
	}
}

// BenchmarkMessageStatsSingle 是对照组：同一个夹具上只算一个 ns（40k events），
// 用来证明「全部命名空间」的成本是 3 次逐 ns 查询，不是额外的 N² 或全表扫。
func BenchmarkMessageStatsSingle(b *testing.B) {
	db, nsIDs, _ := statsAllBenchDB(b, 40000)
	ctx := context.Background()
	now := time.Now()
	sinceSec := now.AddDate(0, 0, -371).Unix()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.messageStats(ctx, nsIDs[0], sinceSec, now); err != nil {
			b.Fatalf("messageStats: %v", err)
		}
	}
}
