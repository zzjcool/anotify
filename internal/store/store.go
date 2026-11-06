// Package store 提供 SQLite 数据访问层。
//
// 迁移机制：schema.sql 是单一事实源，按 `-- +migration N name` 切块，
// 每个编号版本在一个事务内应用并记入 schema_version 表；失败必须返回错误，
// 不允许静默跳过（见 migrate.go）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotImplemented 是「还没实现的 store 方法」的哨兵（重构期脚手架，#55）。
//
// 今天没有任何实现返回它：TestNoStubRemains 用它当门禁——新增方法若是空壳，
// 必须返回本错误，这样那条用例会在 CI 里立刻点名。
var ErrNotImplemented = errors.New("store: not implemented")

// defaultMaxOpenConns 是连接池默认上限（#240）。
//
// 为什么不再是 1：WAL + `busy_timeout` 下 SQLite 本来就是「多读单写」——
//
//	读之间可以并行，把连接池压到 1 只是把并发读换成**应用层排队**
//	（实测读端点吞吐只有 /health 的 2.5%–11%，p50 随并发线性恶化，见 #240）。
//
// 写不再靠「全库只有一条连接」避免 `database is locked`，而是靠
// 事务一律 `BEGIN IMMEDIATE`（DSN 的 `_txlock=immediate`）+ `busy_timeout` 等锁：
// 写锁在事务开始时就拿，拿不到就等，绝不在事务中途「读升级成写」时撞 BUSY。
const defaultMaxOpenConns = 8

// maxConnsEnv 覆盖连接池上限（正整数；缺失/非法回落到 defaultMaxOpenConns）。
const maxConnsEnv = "ANOTIFY_DB_MAX_CONNS"

// DB 包装 *sql.DB。
type DB struct {
	*sql.DB
	// stmts 是只读路径的 per-DB 语句缓存（#242，见 stmt.go）。
	// 用值类型而非指针：零值就是一个能用的空缓存，包内直造的测试 DB
	// （`&DB{DB: sqlDB}`）不会因为忘了初始化而静默退化成直查。
	stmts stmtCache
}

// Open 打开（并迁移）一个 SQLite 数据库。path 可为 ":memory:"。
//
// 连接池上限默认 defaultMaxOpenConns，可用环境变量 ANOTIFY_DB_MAX_CONNS 覆盖
// （压测/低内存机器上调小，`:memory:` 恒为 1，见 openWithOptions）。
func Open(path string) (*DB, error) {
	return openWith(path, nil)
}

// OpenWithConns 与 Open 相同，但显式指定连接池上限（测试/压测用）。
// n <= 0 时用 defaultMaxOpenConns —— 注意这与 Open 不同：Open 会读
// ANOTIFY_DB_MAX_CONNS，而显式注入的 n 不会再回落到环境变量。
func OpenWithConns(path string, maxConns int) (*DB, error) {
	return openWithOptions(path, nil, maxConns)
}

// openWith 是 Open 的可注入版本：modify 用于测试注入坏迁移。
func openWith(path string, modify func(*migrationSet)) (*DB, error) {
	return openWithOptions(path, modify, maxConnsFromEnv())
}

// openWithOptions 建连接池、开库、跑迁移。
//
// 两点与 #240 之前的版本不同（都与「单写」有关，与「单读」无关）：
//
//  1. 连接池是 N 而不是 1：只读查询真的能并行（WAL 下读者互不阻塞、也不阻塞写者）。
//  2. DSN 带 `_txlock=immediate`：每个 `Begin/BeginTx` 用 `BEGIN IMMEDIATE` 开局，
//     写锁在**事务起点**就拿到。这条是「多连接」能安全跑写的前提——
//     deferred 事务先做读、再升级成写时会立刻拿到 SQLITE_BUSY（busy_timeout 不生效，
//     因为等待会破坏它已读到的快照），表现为用户可见的 `database is locked`。
func openWithOptions(path string, modify func(*migrationSet), maxConns int) (*DB, error) {
	if maxConns <= 0 {
		maxConns = defaultMaxOpenConns
	}
	// `:memory:` 的特殊语义：SQLite 的内存库是**每连接一份**，
	// 连接池 >1 时第二条连接会看到一个全新的空库（迁移都白跑了）。
	// 所以内存库恒为单连接——这不是性能开关，是语义约束。
	//
	// 判据是**精确字符串** `":memory:"`（本函数唯一支持的内存库写法）。
	// 谁要是引入 `file::memory:?...` / `""` 之类的其它「每连接一份」写法，
	// 这条守卫会静默失效、池保持 N，症状是「迁移跑在一条连接、查询走另一条空库」的
	// 空表而不是报错——加新写法时必须同时处理这里。
	if path == ":memory:" && maxConns > 1 {
		maxConns = 1
	}

	// modernc.org/sqlite 驱动名 "sqlite"
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	// 写事务一律 BEGIN IMMEDIATE（见 openWithOptions 注释第 2 点）。
	dsn += "&_txlock=immediate"
	// 注意 busy_timeout(5000) 现在也是**进程内写队列的上限**：池 >1 之后，第二个写者
	// 排在 SQLite 的 busy handler 里（5s 硬上限），而不是 database/sql 的连接队列（无超时）。
	// 所以单个写事务必须远短于 5s（现有写路径都是小事务/批次分片）；真出现长写事务要
	// 显式调大 busy_timeout，否则后来者会拿到 database is locked。

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	// 空闲连接留满整个池：SQLite 的连接很便宜（无网络握手），
	// 但反复开关会重复应用 pragma，且并发读会退回「串行新建连接」。
	db.SetMaxIdleConns(maxConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := migrate(db, modify); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 统计信息维护（reviewer 3A · MAJOR-1）：SQLite 官方推荐的「启动时/关闭前」姿势——
	// 统计新鲜时它是一次纯检查，成本趋近 0。
	//
	// 注意它承诺的范围（别读成「统计永不过期」）：这一句**只覆盖「缺失」**——表上有索引、
	// sqlite_stat1 里却一条该表的条目都没有。「过期」（行数漂移后估计仍停在上次的值）
	// 由**运行期维护**负责：本文件的 `DB.MaintainStats` + `lease/stats.go` 的 6h 任务，
	// 判定逻辑见下面的 ② 与 `refreshDriftedStats`。
	//
	// 本批它**只承诺一件事**：把「库很干净、规划器却是盲的」里那条**够得到**的补上——
	//   ① 新库空表建好之后再灌到几万行：建库时没有任何 ANALYZE（空表上一行 stat1
	//      都不写），等于没写统计；重开一次 Open，
	//      mask 0x2 的判定规则是「表上有索引、但 sqlite_stat1 里没有该表的条目」——
	//      与会话无关、也不需要行数变化（实测：新库灌完 2400 行，重开一次 Open 就补出全量统计），
	//      正是这一刻，于是补出全量统计。
	//
	// ② 「统计随行数漂移、optimize 会顺手刷新」**不成立**（第二轮复审 + #265 实测）：
	//   mask 0x2 够不到运行期漂移。「行数涨 10 倍后复查」那条规则要 `0x10000` 位，而
	//   `0x2` 根本没有那一位。实测两遍，结论一致：
	//     · 100 行 ANALYZE 后重开一次 Open（100 → 2100 行，21×）：stat1 里仍是 100；
	//     · 100 行 ANALYZE 后灌到 2500 行（25×）、**同一连接跑过 tasks 查询**，直接调
	//       `PRAGMA optimize=0x2`：返回空建议集，stat1 里仍是 100。
	//   也就是说「运行期调一遍 optimize 就能刷新统计」是假象——这正是 #265 一开始只被
	//   **字面上**修掉的原因（6h 维护任务当时跑的也是这条 `=0x2`）。
	//
	//   现在的分工：`PRAGMA optimize=0x2` 只管「缺失」（不会误判、代价趋近 0），
	//   **「过期」由 `refreshDriftedStats` 自己做确定性判定**——拿 `sqlite_stat1` 里的行数
	//   估计与 `SELECT count(*) FROM tasks` 比，差 ≥10 倍就 `ANALYZE "tasks"`。
	//   为什么不干脆改用 `PRAGMA optimize=0x10002`：0x10000 的判定挂在**连接级**的
	//   表使用记录上，而维护任务从连接池里拿到哪条连接不确定（实测那条路径能刷、但判定
	//   依据是「本连接用过该表」，冷启动/新连接上不保证成立）；而且它会对**任意**漂移表
	//   下手，与「后台只碰 tasks」的约定不符。自己比一次行数是确定性的，成本是一条
	//   `count(*)`（走最小索引，百万行量级 ~几十毫秒，6h 一次）。
	//
	// 为什么是 `=0x2` 而不是裸 `PRAGMA optimize`（**本行与冻结计划的一处刻意偏差，实测驱动**）：
	//   裸写等价于 mask 0x0fffe，其中 0x10 位会把每次 ANALYZE 降级成
	//   `analysis_limit = min(analysis_limit, SQLITE_DEFAULT_OPTIMIZE_LIMIT=2000)` 的**采样**分析
	//   （见 sqlite 源码 pragma.c 的 "0x00010 Run all ANALYZE operations using an analysis_limit"
	//   那段注释）。采样只按样本算「每个前缀值平均多少行」，不会按全表重新放大，于是 2 万行、
	//   单一命名空间的数据被记成 idx_tasks_ns_created="20000 2001 3 1"（真值 20000）——
	//   「按命名空间扫」被低估 10 倍，规划器又把 status= 稀有值 的列表查询抢回新索引，
	//   实测 20k 行仍 13~15ms（全量 ANALYZE 后是 0.17ms）。去掉 0x10（= 只传 0x2）就只剩
	//   「表上有索引但没有 stat1 条目 → 补一次」这一条判定规则（其余规则见 ②），
	//   但分析本身是全量的：实测 stat1 与手工 ANALYZE 完全一致。
	//   若要回退成裸 `PRAGMA optimize`，把下面这行的 `=0x2` 删掉即可（代价见上）。
	//
	// 与形状门禁的关系：本语句写的 sqlite_stat1 / sqlite_stat4 是 `sqlite_` 前缀内部表，
	// masterDDL（schema_baseline_test.go）用 `name NOT LIKE 'sqlite_%'` 把它们过滤掉了——
	// 所以它**不会**破坏「结构 / 重开幂等」那道门禁（统计是运行期数据，不是 schema 形状）。
	// 走裸 `*sql.DB` 而不是 `d.stmts`：此刻 DB 还没构造出来（语句缓存是紧随其后才建的）；
	// 而 `PRAGMA optimize` 本身在 `maintainStatsOn` 里是 `QueryContext`（要读回建议行）+ 对
	// 每条建议 `ExecContext`，不是一条 `db.Exec`。
	// 失败**不致命**（第二轮复审 minor-1：实测这里曾因 `database is locked (5)` 让 Open 4.7s 后
	// 整体失败）：本语句是**可选优化**，它挂掉只影响「规划器可能暂时选不到最优索引」——
	// 查询结果一律正确，功能不变。ANALYZE 是纯优化，把「统计没刷新」升级成「服务起不来」
	// 不划算。所以只记一条 warning。
	//
	// 对比：将来若某个迁移块里要写统计（`ANALYZE <表>`），那是**确定性**保证（写在迁移事务内，
	// 失败就该让迁移失败、整体回滚）——两者定位不同，别混。
	// 也不要给它加重试循环：只会把启动变慢，而且下一次 Open 会再试一遍。
	// 观测优先（#265）：把 optimize 的**建议**打出来，运维才知道统计到底多久该刷一次。
	// 它通常是空行（= 不需要），非空时形如 `ANALYZE "tasks"`。
	if suggestions, err := dbMaintainStatsOnce(db); err != nil {
		slog.Warn("store: sqlite 统计维护失败（不影响正确性，只是规划器可能暂时选不到最优索引）",
			"event", "store.open.optimize_failed",
			"error", err.Error())
	} else if len(suggestions) > 0 {
		slog.Debug("store: 启动时按建议刷新了统计",
			"event", "store.open.stats_refreshed",
			"statements", suggestions)
	}
	return &DB{DB: db, stmts: newStmtCache()}, nil
}

// maxConnsFromEnv 读 ANOTIFY_DB_MAX_CONNS；非法值回落到默认（不静默改成 0，
// 那会让 database/sql 变成「不限制连接数」——在 SQLite 上是灾难）。
func maxConnsFromEnv() int {
	v := os.Getenv(maxConnsEnv)
	if v == "" {
		return defaultMaxOpenConns
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultMaxOpenConns
	}
	return n
}

// Now 返回当前 unixepoch 秒（供各写入点统一使用）。
func Now() int64 { return time.Now().Unix() }

// MaintainStats 是一次「运行期」的统计维护：先问 SQLite 需不需要刷新（按它的建议执行），
// 再对会持续增长的大表做一次**确定性**漂移检查（#265）。
//
// 为什么不能只跑 `PRAGMA optimize=0x2`（这是 #265 的第一版，已证伪）：它够不到
// **运行期漂移**。某个命名空间的 tasks 从 100 行涨到 1 万行时，sqlite_stat1 里可能仍记着
// 100，规划器据此选错计划——表现是「索引明明建好了但查询很慢」，而且没有任何报错。
// 实测（`TestMaintainStatsRefreshesDriftedStat1`）：100 → 2500 行（25×）后调 `=0x2`，
// 返回**空建议集**，stat1 停在 100；所以「按需」那一半是安慰剂，必须自己判。
//
// 两个层次（代价与收益对齐，既不无条件 ANALYZE 全库，也不把自己骗过去）：
//   - `PRAGMA optimize=0x2`：只处理「表上有索引但没有 stat1 条目」（统计**缺失**），
//     统计新鲜时是一次纯检查，成本趋近 0；
//   - `refreshDriftedStats`：只针对 `tasks`，把 stat1 里的行数估计与真实行数比，
//     差 ≥10 倍才 `ANALYZE "tasks"`（统计**过期**）。一条 `count(*)` 的成本，
//     换来的是规划器不会拿着 100 行的估计去扫 100 万行。
//
// 返回被执行的语句（用于日志/测试断言）。空切片 = 这次不需要做什么。
//
// 安全：执行的语句必须来自 SQLite 自己的建议且形如 `ANALYZE ...`（见 isAnalyzeSuggestion），
// 或来自本包自己写死的 `ANALYZE "tasks"`。
func (d *DB) MaintainStats(ctx context.Context) ([]string, error) {
	return maintainStatsOn(ctx, d.DB, statsTriggerMaintenance)
}

// statsQueryer 是 dbMaintainStatsOnce / MaintainStats 共用的最小接口：
// Open 阶段还没有 *DB（它由本函数末尾构造），只能把手上的 *sql.DB 传进来。
type statsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dbMaintainStatsOnce 是 Open 阶段的版本（只跑一次、不抛错，结果用于日志）。
func dbMaintainStatsOnce(db *sql.DB) ([]string, error) {
	return maintainStatsOn(context.Background(), db, statsTriggerOpen)
}

// 统计维护的两个触发点，只用于日志区分（运维看到 `lease.stats.refreshed` 时
// 要能分清是启动顺手刷的还是 6h 任务刷的）。
const (
	statsTriggerOpen        = "open"
	statsTriggerMaintenance = "maintenance"
)

func maintainStatsOn(ctx context.Context, q statsQueryer, trigger string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA optimize=0x2`)
	if err != nil {
		return nil, fmt.Errorf("store: pragma optimize: %w", err)
	}
	suggestions := make([]string, 0, 2)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: read optimize suggestion: %w", err)
		}
		if s != "" {
			suggestions = append(suggestions, s)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate optimize suggestions: %w", err)
	}
	rows.Close()

	ran := make([]string, 0, len(suggestions))
	for _, s := range suggestions {
		// 只放行 SQLite 的建议形状（`ANALYZE "tasks"` / `ANALYZE sqlite_master`）。
		if !isAnalyzeSuggestion(s) {
			slog.Warn("store: 忽略非 ANALYZE 形状的 optimize 建议",
				"event", "store.stats.unexpected_suggestion",
				"suggestion", s)
			continue
		}
		if _, err := q.ExecContext(ctx, s); err != nil {
			// 统计刷新失败不影响正确性：记一条 warning，下次再试。
			slog.Warn("store: 统计刷新失败（不影响正确性，规划器可能暂时选不到最优索引）",
				"event", "store.stats.analyze_failed",
				"statement", s,
				"error", err.Error())
			continue
		}
		ran = append(ran, s)
	}

	// 建议集跑完之后再做漂移检查：两者是**或**的关系而不是二选一——
	// 建议集非空（刚补完「缺失」的统计）时也要看一眼 tasks 是不是「过期」了；
	// 反过来，漂移检查自己做 ANALYZE 之后 stat1 就新鲜了，下一轮它自己会静默。
	// 放在这里而不是只在 MaintainStats 里，是为了让「重启一次」也能救回被漂移坑掉的库
	// （启动时顺手刷新一次，比等 6h 更早）。
	if stmt, err := refreshDriftedStats(ctx, q, trigger); err != nil {
		// 不动正确性：只是这一轮没刷新。下一轮（或下一次 Open）会再试。
		slog.Warn("store: 行数漂移检查失败（不影响正确性，规划器可能暂时选不到最优索引）",
			"event", "store.stats.drift_check_failed",
			"trigger", trigger,
			"error", err.Error())
	} else if stmt != "" {
		ran = append(ran, stmt)
	}
	return ran, nil
}

// 漂移检查的参数：只关注 `tasks`——库里唯一会从「几百行」长到「百万行」的表，
// 也是按命名空间/status 列表查询选索引时最容易被过期估计坑到的表。
const (
	statsDriftTable = "tasks"
	// 10 倍是 SQLite 自己 `PRAGMA optimize` 的 0x10000 位用的量级（也是
	// 「规划器会开始选错索引」的量级）；比它小的差异对索引选择几乎没影响，不值一次 ANALYZE。
	statsDriftFactor = 10
)

// refreshDriftedStats 是「统计过期」那一半（#265）：拿 sqlite_stat1 里的行数估计
// 与真实行数比，差 ≥10 倍就重新 ANALYZE 一次。
//
// 为什么不直接改成 `PRAGMA optimize=0x10002`：那一位置上的判定依赖**连接级**的
// 「本连接用过该表」记录，而维护任务从连接池里拿哪条连接不确定（冷启动、新连接上
// 不成立）；它还会对任意漂移表动手，与「后台只碰 tasks」的约束不符。
// 这里只读一次 stat1 + 一次 count(*)，判定确定、不依赖连接历史。
//
// 返回实际执行的语句（空串 = 不需要动）。任何失败都只记 warning：统计是纯优化，
// 把「这轮没刷新」升级成「任务失败」不划算。
//
// 代价说明：`ANALYZE "tasks"` 是**全量**分析（不用 analysis_limit 采样，见 Open 里
// `=0x2` 那段注释里的 10× 低估），在百万行表上是一次秒级写事务，期间写者要排队
// （busy_timeout 5000）。所以它不能常态跑：入口条件是「差 ≥10 倍」（漂移慢、不常发生），
// 刷新完 stat1 与真实行数一致，下一轮自己就静默了；频率上限是 6h 一次（lease/stats.go）。
func refreshDriftedStats(ctx context.Context, q statsQueryer, trigger string) (string, error) {
	est, ok, err := stat1RowEstimate(ctx, q, statsDriftTable)
	if err != nil {
		return "", err
	}
	if !ok {
		// 没有基线：那是 `PRAGMA optimize=0x2` 的活（「缺失」），这里不越界。
		return "", nil
	}
	cur, err := countTableRows(ctx, q, statsDriftTable)
	if err != nil {
		return "", err
	}
	if !statsDrifted(est, cur) {
		return "", nil
	}
	stmt := `ANALYZE "tasks"`
	if _, err := q.ExecContext(ctx, stmt); err != nil {
		slog.Warn("store: 统计刷新失败（不影响正确性，规划器可能暂时选不到最优索引）",
			"event", "store.stats.analyze_failed",
			"trigger", trigger,
			"statement", stmt,
			"prev_rows", est,
			"cur_rows", cur,
			"error", err.Error())
		return "", nil
	}
	// 事件名沿用 6h 维护任务那一条（`lease.stats.refreshed`），并带上这次刷新的**原因**：
	// 运维要能一眼看出「是行数翻了 N 倍才刷的」，而不是又一条无信息的 "refreshed"。
	slog.Info("refreshed sqlite statistics",
		"event", "lease.stats.refreshed",
		"trigger", trigger,
		"table", statsDriftTable,
		"prev_rows", est,
		"cur_rows", cur,
		"statement", stmt)
	return stmt, nil
}

// stat1RowEstimate 读 sqlite_stat1 里某表的行数估计（= stat 列的第一个整数）。
//
// stat1 每条索引条目都以「该表行数」开头（后面是这个索引各前缀值的平均行数），
// 所以随便取一条即可。ok=false 表示没有基线（sqlite_stat1 不存在，或该表没有条目）。
func stat1RowEstimate(ctx context.Context, q statsQueryer, table string) (est int64, ok bool, err error) {
	rows, err := q.QueryContext(ctx, `SELECT stat FROM sqlite_stat1 WHERE tbl = ? LIMIT 1`, table)
	if err != nil {
		// 从没跑过 ANALYZE 时 sqlite_stat1 整张表都不存在——这就是「没有基线」，不是错误。
		if strings.Contains(err.Error(), "no such table") {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: read sqlite_stat1: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	var stat string
	if err := rows.Scan(&stat); err != nil {
		return 0, false, fmt.Errorf("store: scan sqlite_stat1: %w", err)
	}
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		return 0, false, nil
	}
	n, perr := strconv.ParseInt(fields[0], 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("store: parse sqlite_stat1 stat %q: %w", stat, perr)
	}
	return n, true, nil
}

// countTableRows 数一张表的真实行数（table 只来自本包写死的常量，不做外部输入）。
func countTableRows(ctx context.Context, q statsQueryer, table string) (int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT count(*) FROM "+table)
	if err != nil {
		return 0, fmt.Errorf("store: count %s: %w", table, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("store: count %s: %w", table, err)
		}
		return 0, fmt.Errorf("store: count %s: empty result", table)
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: scan count %s: %w", table, err)
	}
	return n, nil
}

// statsDrifted 判定 stat1 里的行数估计还能不能代表真实行数。
//
// 一侧为 0（空表刚建统计、或表被清空）时比值没意义，直接算漂移——此时 ANALYZE 本身极便宜。
func statsDrifted(est, cur int64) bool {
	switch {
	case est == cur:
		return false
	case est <= 0 || cur <= 0:
		return true
	default:
		return cur >= est*statsDriftFactor || est >= cur*statsDriftFactor
	}
}

// isAnalyzeSuggestion 判定一条 PRAGMA optimize 的返回值能否当 SQL 执行。
//
// 这是「把数据库返回的字符串当 SQL 跑」这件事的**唯一**闸门：只放行以 ANALYZE 开头的语句，
// 其余一律忽略并留一条 warning（#265 的护栏用例会遍历各种危险形状验证它）。
func isAnalyzeSuggestion(s string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "ANALYZE")
}
