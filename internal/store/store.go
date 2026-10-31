// Package store 提供 SQLite 数据访问层。
//
// 迁移机制：schema.sql 是单一事实源，按 `-- +migration N name` 切块，
// 每个编号版本在一个事务内应用并记入 schema_version 表；失败必须返回错误，
// 不允许静默跳过（见 migrate.go）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	// 它只在**统计缺失或过期**时才真的跑 ANALYZE，统计新鲜时是一次纯检查，成本趋近 0。
	//
	// 本批它**只承诺一件事**：把「库很干净、规划器却是盲的」里那条**够得到**的补上——
	//   ① 新库空表建好之后再灌到几万行：建库时 ANALYZE 扫的是空表（迁移 16 的
	//      `ANALYZE tasks` 在空表上一行 stat1 都不写），等于没写统计；重开一次 Open，
	//      mask 0x2 的判定规则是「表上有索引、但 sqlite_stat1 里没有该表的条目」——
	//      与会话无关、也不需要行数变化（实测：新库灌完 2400 行，重开一次 Open 就补出全量统计），
	//      正是这一刻，于是补出全量统计。
	//
	// ② 「统计随行数漂移、optimize 会顺手刷新」**不成立，本批不承诺**（第二轮复审实测）：
	//   mask 0x2 在 Open 时刻**够不到**运行期漂移。「行数涨 10 倍后复查」那条规则需要
	//   `0x10000` 位，**且**该连接查询过该表（sqlite 的判定是「本次会话用过的表、行数估计与
	//   stat1 差 10 倍以上」），而 Open 时还没跑任何查询。实测 100 → 2100 行（21×）后重开，
	//   stat1 里仍是 100。真要做运行期刷新，得周期性在运行期跑 optimize——那是另一个 issue，
	//   不在本批范围。
	//
	// 为什么是 `=0x2` 而不是裸 `PRAGMA optimize`（**本行与冻结计划的一处刻意偏差，实测驱动**）：
	//   裸写等价于 mask 0x0fffe，其中 0x10 位会把每次 ANALYZE 降级成
	//   `analysis_limit = min(analysis_limit, SQLITE_DEFAULT_OPTIMIZE_LIMIT=2000)` 的**采样**分析
	//   （见 sqlite 源码 pragma.c 的 "0x00010 Run all ANALYZE operations using an analysis_limit"
	//   那段注释）。采样只按样本算「每个前缀值平均多少行」，不会按全表重新放大，于是 2 万行、
	//   单一命名空间的数据被记成 idx_tasks_ns_created="20000 2001 3 1"（真值 20000）——
	//   「按命名空间扫」被低估 10 倍，规划器又把 status= 稀有值 的列表查询抢回新索引，
	//   实测 20k 行仍 13~15ms（全量 ANALYZE 后是 0.17ms）。去掉 0x10（= 只传 0x2）保留全部
	//   「该不该分析」的判定规则，但分析本身是全量的：实测 stat1 与手工 ANALYZE 完全一致。
	//   若要回退成裸 `PRAGMA optimize`，把下面这行的 `=0x2` 删掉即可（代价见上）。
	//
	// 与形状门禁的关系：本语句写的 sqlite_stat1 / sqlite_stat4 是 `sqlite_` 前缀内部表，
	// schemaShape（migrate15_test.go）用 `name NOT LIKE 'sqlite_%'` 把它们过滤掉了——
	// 所以它**不会**破坏「新库/老库形状逐行一致」那道门禁（统计是运行期数据，不是 schema 形状）。
	// 走 db.Exec 而不是 d.stmts：此刻 DB 还没构造出来，且这条 SQL 一生只跑一次。
	// 失败**不致命**（第二轮复审 minor-1：实测这里曾因 `database is locked (5)` 让 Open 4.7s 后
	// 整体失败）：本语句是**可选优化**，它挂掉只影响「规划器可能暂时选不到最优索引」——
	// 查询结果一律正确，功能不变。ANALYZE 是纯优化，把「统计没刷新」升级成「服务起不来」
	// 不划算。所以只记一条 warning。
	//
	// 对比：迁移 16 里那句 `ANALYZE tasks` 是升级路径上的**确定性**保证（写在迁移事务内，
	// 失败就该让迁移失败、整体回滚）——两者定位不同，别混。
	// 也不要给它加重试循环：只会把启动变慢，而且下一次 Open 会再试一遍。
	if _, err := db.Exec(`PRAGMA optimize=0x2`); err != nil {
		slog.Warn("store: sqlite 统计维护失败（不影响正确性，只是规划器可能暂时选不到最优索引）",
			"event", "store.open.optimize_failed",
			"error", err.Error())
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
