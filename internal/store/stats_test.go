package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// 运行期统计维护（#265）的数据层验收。
//
// 这组用例守两件事：
//  1. 常态下 **不** 无条件 ANALYZE（成本远大于收益，大表上会挡住写）
//  2. 只执行 SQLite 自己给的 `ANALYZE ...` 建议，别的字符串一律不碰
//     （"把数据库返回的字符串当 SQL 执行"必须锁在最小面上）

// fakeStatsQueryer 让用例能指定 SQLite 的"建议"。
type fakeStatsQueryer struct {
	suggestions []string
	executed    []string
	execErr     error
}

func (f *fakeStatsQueryer) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	// maintainStatsOn 只查一条：PRAGMA optimize
	rows := &sql.Rows{} // 占位，真正的实现对 *sql.Rows 不可构造
	_ = rows
	return nil, errFakeNotConstructible
}

func (f *fakeStatsQueryer) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	f.executed = append(f.executed, q)
	return nil, f.execErr
}

var errFakeNotConstructible = sql.ErrConnDone

// TestMaintainStatsFreshDBIsQuiet 新库上没有漂移 → 不该执行任何 ANALYZE。
//
// 这是「不要无条件 ANALYZE」的护栏：真跑一遍 MaintainStats，断言 statements 为空。
func TestMaintainStatsFreshDBIsQuiet(t *testing.T) {
	db := newTestDB(t)
	ran, err := db.MaintainStats(context.Background())
	if err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("新库不该需要 ANALYZE，实际执行了 %v", ran)
	}
}

// TestMaintainStatsOnlyRunsAnalyzeSuggestions 是对「建议校验」的守卫。
//
// 实现上 `maintainStatsOn` 接受一个最小接口，但 *sql.Rows 无法在测试里构造，
// 所以这里直接测**过滤函数**本身（把建议 → 是否放行 的逻辑抽出来验）。
func TestMaintainStatsSuggestionFilter(t *testing.T) {
	ok := []string{`ANALYZE "tasks"`, `analyze tasks`, `  ANALYZE sqlite_master `}
	for _, s := range ok {
		if !isAnalyzeSuggestion(s) {
			t.Errorf("%q 应该被放行", s)
		}
	}
	bad := []string{"", "DROP TABLE tasks", "DELETE FROM tasks", "PRAGMA ignore_check_constraints=1"}
	for _, s := range bad {
		if isAnalyzeSuggestion(s) {
			t.Errorf("%q 绝不能被当 SQL 执行", s)
		}
	}
}

// taskStat1Rows 读 sqlite_stat1 里 tasks 的行数估计（= stat 列的第一个整数，也是 ANALYZE
// 记下的**表行数**；stat1 的每条索引条目都以它开头）。
//
// 复用 pendingStat1Entries 的「idx = stat」口径，避免第二套读法；解析取最大值
// （同一张表的各条索引条目首位相同，取 max 只是不依赖行序）。
func taskStat1Rows(t *testing.T, db *DB) (rows int64, ok bool) {
	t.Helper()
	for _, entry := range pendingStat1Entries(t, db) {
		_, stat, cut := strings.Cut(entry, " = ")
		if !cut {
			t.Fatalf("sqlite_stat1 条目形状意外：%q", entry)
		}
		fields := strings.Fields(stat)
		if len(fields) == 0 {
			t.Fatalf("sqlite_stat1 条目的 stat 为空：%q", entry)
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("sqlite_stat1 的 stat 首位不是整数：%q (%v)", entry, err)
		}
		if !ok || n > rows {
			rows, ok = n, true
		}
	}
	return rows, ok
}

// countTasks 返回 tasks 的真实行数（与 stat1 的估计对照，仅断言/日志用）。
func countTasks(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// TestMaintainStatsRefreshesDriftedStat1 是 #265 的**证伪/验收**核心用例。
//
// 要证伪的假设是：`MaintainStats`（= 6h 维护任务调的东西）能刷新**运行期漂移**的统计。
// 如果它只是再跑一遍 `PRAGMA optimize=0x2`，那么「行数涨 10 倍后复查」那条规则（需要
// 0x10000 位 + 该连接查询过该表）在 Open 时刻够不到，运行期也未必够得到——它就是安慰剂。
//
// 判定方式：造出真实漂移，跑一次 MaintainStats，看 `sqlite_stat1` 里 tasks 的行数估计**变了没有**。
//
//   - 没变（= 修复前的实测结果）→ 本用例失败，说明 #265 只在字面上被修。
//   - 变了、且接近真实行数 → 本用例通过，说明维护任务真的能救回规划器的估计。
func TestMaintainStatsRefreshesDriftedStat1(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "stats-drift", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(cap)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	// ① 先灌 100 条，手工 ANALYZE 建出 stat1 基线。
	mkSeqTaskBatch(t, db, nsID, "trace_stats_base", cap, 100)
	if _, err := db.Exec(`ANALYZE "tasks"`); err != nil {
		t.Fatalf("baseline analyze: %v", err)
	}
	base, ok := taskStat1Rows(t, db)
	if !ok {
		t.Fatal("ANALYZE 之后 sqlite_stat1 里没有 tasks 的条目（基线没建出来）")
	}
	if base < 90 || base > 110 {
		t.Fatalf("基线 stat1 行数估计 = %d，want ≈100", base)
	}

	// ② 再灌 2400 条：100 → 2500（25×），漂移远超 SQLite 的「10 倍」阈值。
	mkSeqTaskBatch(t, db, nsID, "trace_stats_grow", cap, 2400)
	real := countTasks(t, db)
	if real < 10*base {
		t.Fatalf("真实行数 %d 没到 10× 基线 %d，漂移没造出来", real, base)
	}

	// ③ 跑一次运行期维护（这就是 6h 任务做的事）。
	ran, err := db.MaintainStats(ctx)
	if err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	t.Logf("MaintainStats 执行的语句 = %v；真实行数 = %d，基线估计 = %d", ran, real, base)

	// ④ 核心断言：stat1 里的估计必须跟着涨上来，而不是留在 100。
	after, ok := taskStat1Rows(t, db)
	if !ok {
		t.Fatal("MaintainStats 之后 sqlite_stat1 里没有 tasks 的条目")
	}
	if after == base {
		t.Fatalf("证伪成立：MaintainStats 之后 stat1 仍是 %d（真实行数 %d，%d×）——"+
			"它就是再跑一遍 PRAGMA optimize=0x2，够不到运行期漂移，#265 只是字面上被修",
			after, real, real/base)
	}
	if after < real*8/10 {
		t.Fatalf("MaintainStats 之后 stat1 估计 = %d，离真实行数 %d 太远（刷新没到位）", after, real)
	}
}

// TestMaintainStatsKeepsQuietWhenStat1IsFresh 统计新鲜时漂移检查必须静默。
//
// 漂移检查的代价是一条 `count(*)`，但不能“顺手 ANALYZE”：只要判定写错（比如拿错了
// 前缀平均行数去比），6h 任务就会变成「每轮都全表 ANALYZE」，正好是 #265 要避免的形态。
func TestMaintainStatsKeepsQuietWhenStat1IsFresh(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "stats-fresh", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(cap)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	mkSeqTaskBatch(t, db, nsID, "trace_stats_fresh", cap, 100)
	if _, err := db.Exec(`ANALYZE "tasks"`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	base, ok := taskStat1Rows(t, db)
	if !ok {
		t.Fatal("ANALYZE 之后没有 tasks 的 stat1 条目")
	}

	ran, err := db.MaintainStats(ctx)
	if err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	for _, s := range ran {
		if strings.Contains(strings.ToLower(s), statsDriftTable) {
			t.Fatalf("stat1 估计(%d)与真实行数一致时不该 ANALYZE tasks，实际执行了 %v", base, ran)
		}
	}
}

// TestStatsDrifted 是漂移判定的纯函数钉子（阈值 10×、一侧为 0 的边界）。
func TestStatsDrifted(t *testing.T) {
	cases := []struct {
		est, cur int64
		want     bool
	}{
		{0, 0, false},
		{100, 100, false},
		{100, 999, false}, // 恰好不到 10×
		{100, 1000, true}, // 恰好 10×（#265 的门槛）
		{2500, 100, true}, // 反向漂移（删数据/清理）也要重算
		{100, 2500, true}, // 涨 25×（真实场景）
		{0, 2500, true},   // 有基线但是 0，真实却很多
		{2500, 0, true},   // 表被清空
		{1, 9, false},
		{1, 10, true},
	}
	for _, c := range cases {
		if got := statsDrifted(c.est, c.cur); got != c.want {
			t.Errorf("statsDrifted(est=%d, cur=%d) = %v, want %v", c.est, c.cur, got, c.want)
		}
	}
}

// TestMaintainStatsDoesNotTouchData 统计维护不能改任何业务数据（它只该动 sqlite_stat*）。
func TestMaintainStatsDoesNotTouchData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{ID: NewUserID(), Username: "stats-user", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.MaintainStats(ctx); err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	got, err := db.GetUserByUsername("stats-user")
	if err != nil || got.ID != u.ID {
		t.Fatalf("统计维护动了业务数据：err=%v got=%+v", err, got)
	}
	_ = strings.TrimSpace("") // 保持 strings 依赖显式（过滤实现用它）
}

// TestMaintainStatsSkipsDriftCheckWhenTableEmpty 空表不该因为「漂移检测」而反复 ANALYZE。
//
// 漂移检测的实现会拿 `SELECT count(*)` 与 stat1 估计比较；空表的估计是 0/缺失，
// 这里钉住「没有可比较的基线时不动手」，避免每轮维护都白跑一次 ANALYZE。
func TestMaintainStatsSkipsDriftCheckWhenTableEmpty(t *testing.T) {
	db := newTestDB(t)
	ran, err := db.MaintainStats(context.Background())
	if err != nil {
		t.Fatalf("maintain stats: %v", err)
	}
	for _, s := range ran {
		if strings.Contains(strings.ToLower(s), "tasks") {
			t.Fatalf("空表上不该 ANALYZE tasks，实际执行了 %v", ran)
		}
	}
}
