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
	return &DB{db}, nil
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
