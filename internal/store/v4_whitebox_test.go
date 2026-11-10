package store

// v4_whitebox_test.go 是 B 轮的**真白盒**验收：直接调用包内真实的事务/解析 helper，
// 钉住那些只能从里面看见、又绝不能退化的性质。
//
// 为什么必须白盒：
//   - baseline 的 DDL、记账行与布局标记（PRAGMA user_version）必须在**同一个事务**里
//     落盘。只从公开面 Open 观察“失败后再开一次能成功”，无法区分“回滚了”与
//     “写了半截但被后续 CREATE IF NOT EXISTS 自愈”；
//   - 布局身份（marker=4）与迁移记账（schema_version）是两件事，坏库必须被点名拒绝、
//     **绝不修复**；
//   - 写事务的统一入口（DSN `_txlock=immediate` 下的 BeginTx）的 commit/rollback/
//     取消/关闭语义是所有写路径的地基。
//
// 纪律：这里只调用生产代码里真实存在的 helper（applyBaselineMigration / applyMigration /
// migrate / probeDatabaseFormat / validateV4Layout / userSchemaObjects），
// 不引入生产侧测试钩子、不伪造 reaped/lease 事实、不 skip。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rawMigrationDB 打开一个裸 sqlite 句柄（不跑迁移），用于在**空库**上观察初始化事务。
func rawMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migrationState 读「布局标记 + 用户对象集合」——失败事务不许留下任何一项。
func migrationState(t *testing.T, db *sql.DB) (int, []string) {
	t.Helper()
	var marker int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&marker); err != nil {
		t.Fatalf("read layout marker: %v", err)
	}
	objects, err := userSchemaObjects(db)
	if err != nil {
		t.Fatalf("read user schema objects: %v", err)
	}
	return marker, objects
}

// assertPristineMigrationState 失败后必须是**空库**（marker=0，一个对象都没有）。
func assertPristineMigrationState(t *testing.T, db *sql.DB) {
	t.Helper()
	marker, objects := migrationState(t, db)
	if marker != 0 || len(objects) != 0 {
		t.Fatalf("失败的初始化留下了 marker=%d objects=%v；初始化事务没有整体回滚", marker, objects)
	}
}

// TestWhiteboxParseMigrationsContinuousLedger 解析器只接受从 1 开始、连续、不重复的账本。
func TestWhiteboxParseMigrationsContinuousLedger(t *testing.T) {
	src := "-- +migration 1 first\nSELECT 1;\n-- +migration 2 second\nSELECT 2;\n-- +migration 3 third\nSELECT 3;"
	set, err := parseMigrations(src)
	if err != nil {
		t.Fatalf("parse continuous ledger: %v", err)
	}
	if got := migrationVersions(set); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("账本序列 = %v, want [1 2 3]", got)
	}
	for i, want := range []string{"SELECT 1;", "SELECT 2;", "SELECT 3;"} {
		if set[i].body != want || set[i].name == "" {
			t.Fatalf("第 %d 块 = %+v, want body %q 且 name 非空", i, set[i], want)
		}
	}
	for name, text := range map[string]string{
		"starts_above_one": "-- +migration 2 second\nSELECT 2;",
		"gap":              "-- +migration 1 first\nSELECT 1;\n-- +migration 3 third\nSELECT 3;",
		"duplicate":        "-- +migration 1 first\nSELECT 1;\n-- +migration 1 duplicate\nSELECT 2;",
		"reverse":          "-- +migration 1 first\nSELECT 1;\n-- +migration 2 second\nSELECT 2;\n-- +migration 1 reverse\nSELECT 3;",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMigrations(text); err == nil {
				t.Fatalf("坏账本 %q 被接受", name)
			}
		})
	}
}

// TestWhiteboxParseMigrationsMalformedHeaderIsNotABlock 畸形分块头**不能**产生迁移块。
//
// 解析器按 `-- +migration N <word>` 切块；头写坏了（缺名字/缺编号）整段就退化成
// 上一块的正文。这条不是“容忍坏输入”：它保证坏头不会**冒充**一个版本被执行，
// 同时 schema.sql 必须靠 `len(set) == headVersion` 把这种退化成块的错误抓出来。
func TestWhiteboxParseMigrationsMalformedHeaderIsNotABlock(t *testing.T) {
	for name, src := range map[string]string{
		"missing_name":       "-- +migration 1\nCREATE TABLE x (id TEXT);",
		"missing_version":    "-- +migration name\nCREATE TABLE x (id TEXT);",
		"non_numeric_verion": "-- +migration one first\nCREATE TABLE x (id TEXT);",
	} {
		t.Run(name, func(t *testing.T) {
			set, err := parseMigrations(src)
			if err != nil {
				t.Fatalf("畸形分块头应是“没有块”，实际报错: %v", err)
			}
			if len(set) != 0 {
				t.Fatalf("畸形分块头产生了 %d 个迁移块: %+v（坏头绝不能冒充版本）", len(set), set)
			}
		})
	}
}

// TestWhiteboxBaselineDDLFailureRollsBackMarkerAndLedger baseline DDL 中途报错 →
// 布局标记、记账表与已建的表全部回滚；同一个库随后能真正初始化（不需要删库）。
func TestWhiteboxBaselineDDLFailureRollsBackMarkerAndLedger(t *testing.T) {
	db := rawMigrationDB(t)
	m := migration{version: baselineVersion, name: "broken_baseline",
		body: `CREATE TABLE half_done (id TEXT PRIMARY KEY); CREATE TABLE ))) broken;`}
	err := applyBaselineMigration(db, m, nil)
	if err == nil || !strings.Contains(err.Error(), "exec sql") {
		t.Fatalf("baseline DDL 应报 exec sql 错误，得到 %v", err)
	}
	assertPristineMigrationState(t, db)

	// 同一个库可以直接重新初始化：失败事务没有留下需要人清理的半成品。
	if n, err := migrate(db, nil); err != nil || n != 1 {
		t.Fatalf("失败回滚后重新初始化: applied=%d err=%v, want 1/nil", n, err)
	}
	probe, err := probeDatabaseFormat(db)
	if err != nil {
		t.Fatalf("probe after re-init: %v", err)
	}
	if probe.marker != layoutFormatV4 {
		t.Fatalf("重新初始化后 marker = %d, want %d", probe.marker, layoutFormatV4)
	}
	if err := validateV4Layout(db, probe); err != nil {
		t.Fatalf("重新初始化后的 baseline 结构不完整: %v", err)
	}
}

// TestWhiteboxBaselineLedgerWriteFailureRollsBackAllDDL 记账行写入失败（真实初始化事务里
// 最后一步）→ 连已经建好的全部 DDL 与布局标记一起回滚。
//
// 注入手段是**事务内**创建一个拒绝 INSERT 的触发器（hook 是真的跑在那个事务里），
// 不是生产侧的测试开关。
func TestWhiteboxBaselineLedgerWriteFailureRollsBackAllDDL(t *testing.T) {
	db := rawMigrationDB(t)
	m := migration{version: baselineVersion, name: "ledger_fail", body: schemaSQL}
	hook := func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_ledger BEFORE INSERT ON schema_version
			BEGIN SELECT RAISE(ABORT, 'injected ledger write failure'); END`)
		return err
	}
	err := applyBaselineMigration(db, m, hook)
	if err == nil || !strings.Contains(err.Error(), "record version") ||
		!strings.Contains(err.Error(), "injected ledger write failure") {
		t.Fatalf("记账写入应报错并带注入原因，得到 %v", err)
	}
	assertPristineMigrationState(t, db)
}

// TestWhiteboxBaselineHookFailureRollsBackMarkerAndData hook 失败时，**事务内**已经写下的
// 布局标记与业务行必须一起回滚（marker 是 PRAGMA user_version，随事务提交/回滚）。
func TestWhiteboxBaselineHookFailureRollsBackMarkerAndData(t *testing.T) {
	db := rawMigrationDB(t)
	sentinel := errors.New("injected baseline hook failure")
	m := migration{version: baselineVersion, name: "hook_fail",
		body: `CREATE TABLE partial_data (value TEXT); INSERT INTO partial_data VALUES ('must rollback');`}
	err := applyBaselineMigration(db, m, func(tx *sql.Tx) error {
		// 先证明注入点确实在事务中途：marker 与数据在这个事务里都看得见。
		var marker, rows int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&marker); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM partial_data`).Scan(&rows); err != nil {
			return err
		}
		if marker != layoutFormatV4 || rows != 1 {
			t.Fatalf("注入点不在事务中途: marker=%d rows=%d", marker, rows)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("hook 错误被吞或替换: %v", err)
	}
	assertPristineMigrationState(t, db)
}

// TestWhiteboxLaterMigrationFailurePreservesCheckpoint 后续迁移失败只回滚它自己：
// 已提交的 baseline（marker + 结构 + 记账时间）一行都不许变。
func TestWhiteboxLaterMigrationFailurePreservesCheckpoint(t *testing.T) {
	db := rawMigrationDB(t)
	if _, err := migrate(db, nil); err != nil {
		t.Fatalf("baseline migrate: %v", err)
	}
	beforeMarker, beforeObjects := migrationState(t, db)
	var beforeTime int64
	if err := db.QueryRow(`SELECT applied_at FROM schema_version WHERE version=1`).Scan(&beforeTime); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}
	// 重复的主键记账会失败在 SQL 之后：用来证明新 DDL 也一起回滚。
	err := applyMigration(db, migration{version: 1, name: "duplicate_ledger",
		body: `CREATE TABLE duplicate_half (value TEXT); INSERT INTO duplicate_half VALUES ('rollback');`}, nil)
	if err == nil || !strings.Contains(err.Error(), "record version") {
		t.Fatalf("重复记账应报 record version 错误，得到 %v", err)
	}
	afterMarker, afterObjects := migrationState(t, db)
	var afterTime int64
	if err := db.QueryRow(`SELECT applied_at FROM schema_version WHERE version=1`).Scan(&afterTime); err != nil {
		t.Fatalf("read applied_at after: %v", err)
	}
	if beforeMarker != afterMarker || !reflect.DeepEqual(beforeObjects, afterObjects) || beforeTime != afterTime {
		t.Fatalf("失败迁移改动了检查点: marker %d->%d objects %v->%v time %d->%d",
			beforeMarker, afterMarker, beforeObjects, afterObjects, beforeTime, afterTime)
	}
}

// TestWhiteboxMigrationClosedConnectionFailsWithoutSuccess 连接已关闭时初始化必须在
// Begin 就失败，而不是“看起来成功”。
func TestWhiteboxMigrationClosedConnectionFailsWithoutSuccess(t *testing.T) {
	db := rawMigrationDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	err := applyBaselineMigration(db, migration{version: baselineVersion, name: "closed", body: schemaSQL}, nil)
	if err == nil || !strings.Contains(err.Error(), "begin") {
		t.Fatalf("已关闭连接的初始化应报 begin 错误，得到 %v", err)
	}
}

// TestWhiteboxMigrationCancelledContextDoesNotChangeLedger 取消的 context 不许改动库：
// BeginTx 立刻返回 context.Canceled，检查点保持不变。
func TestWhiteboxMigrationCancelledContextDoesNotChangeLedger(t *testing.T) {
	db := rawMigrationDB(t)
	if _, err := migrate(db, nil); err != nil {
		t.Fatalf("baseline migrate: %v", err)
	}
	marker, objects := migrationState(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if tx, err := db.BeginTx(ctx, nil); !errors.Is(err, context.Canceled) {
		if tx != nil {
			_ = tx.Rollback()
		}
		t.Fatalf("取消的 BeginTx err = %v, want context.Canceled", err)
	}
	afterMarker, afterObjects := migrationState(t, db)
	if marker != afterMarker || !reflect.DeepEqual(objects, afterObjects) {
		t.Fatal("被取消的事务改动了 baseline")
	}
}

// TestWhiteboxOpenWritesMarkerAndLedgerAtomically 成功初始化：marker=4、记账=1、
// 结构通过校验；再 Open/再 migrate 都是 0 应用（幂等）。
func TestWhiteboxOpenWritesMarkerAndLedgerAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	if applied, err := migrate(db.DB, nil); err != nil || applied != 0 {
		t.Fatalf("已初始化的库再 migrate: applied=%d err=%v, want 0/nil", applied, err)
	}
	probe, err := probeDatabaseFormat(db.DB)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.marker != layoutFormatV4 || !probe.ledger {
		t.Fatalf("probe = marker %d ledger %v, want marker %d + ledger", probe.marker, probe.ledger, layoutFormatV4)
	}
	if !probe.versions[baselineVersion] {
		t.Fatalf("记账里没有 baseline 版本 %d: %v", baselineVersion, probe.versions)
	}
	if err := validateV4Layout(db.DB, probe); err != nil {
		t.Fatalf("新建库结构校验: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if applied, err := migrate(db2.DB, nil); err != nil || applied != 0 {
		t.Fatalf("重开再 migrate: applied=%d err=%v, want 0/nil", applied, err)
	}
}

// TestWhiteboxStrictMalformedLayoutRefusedWithoutRepair 自称 v4 布局（marker=4）但结构
// 不完整的库必须被具名拒绝，且**绝不补建**缺的表。
func TestWhiteboxStrictMalformedLayoutRefusedWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-v4.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := raw.Exec(schemaVersionTable); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (1, 1700000000)`); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	beforeMarker, beforeObjects := migrationState(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	if db, err := Open(path); err == nil {
		_ = db.Close()
		t.Fatal("marker=4 但没有表的半写库被接受了：必须报 ErrSchemaInvalid")
	} else if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("应报 ErrSchemaInvalid，得到 %v", err)
	}

	raw2, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer func() { _ = raw2.Close() }()
	afterMarker, afterObjects := migrationState(t, raw2)
	if beforeMarker != afterMarker || !reflect.DeepEqual(beforeObjects, afterObjects) {
		t.Fatalf("拒绝路径修复/改动了坏库: marker %d->%d objects %v->%v",
			beforeMarker, afterMarker, beforeObjects, afterObjects)
	}
}

// TestWhiteboxUnsupportedLayoutRefusedWithoutRepair 未知布局标记（既不是 0 也不是 4）
// 必须被具名拒绝，且不改写任何东西。
func TestWhiteboxUnsupportedLayoutRefusedWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown-layout.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE keep_me (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	beforeMarker, beforeObjects := migrationState(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	if db, err := Open(path); err == nil {
		_ = db.Close()
		t.Fatal("未知布局标记被接受了：必须报 ErrUnsupportedFormat")
	} else if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("应报 ErrUnsupportedFormat，得到 %v", err)
	}

	raw2, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer func() { _ = raw2.Close() }()
	afterMarker, afterObjects := migrationState(t, raw2)
	if beforeMarker != afterMarker || !reflect.DeepEqual(beforeObjects, afterObjects) {
		t.Fatalf("拒绝路径改动了未知布局的库: marker %d->%d objects %v->%v",
			beforeMarker, afterMarker, beforeObjects, afterObjects)
	}
}

// TestWhiteboxWriteTransactionCommitRollbackClosed 写事务入口的四个基本语义：
// commit 落盘、rollback 丢弃、closed 连接报错、取消的 context 不开始事务。
//
// DSN 是 `_txlock=immediate`：BeginTx 即拿写锁，所以这里同时是“写与写串行”的地基。
func TestWhiteboxWriteTransactionCommitRollbackClosed(t *testing.T) {
	db, path := newTestDBFile(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin commit txn: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE wb_commit (value TEXT)`); err != nil {
		t.Fatalf("create wb_commit: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO wb_commit (value) VALUES ('kept')`); err != nil {
		t.Fatalf("insert wb_commit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !tableExists(t, db, "wb_commit") {
		t.Fatal("commit 之后表不存在")
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin rollback txn: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE wb_rollback (value TEXT)`); err != nil {
		t.Fatalf("create wb_rollback: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if tableExists(t, db, "wb_rollback") {
		t.Fatal("rollback 之后表还在：写事务没有原子性")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if tx, err := db.BeginTx(cctx, nil); !errors.Is(err, context.Canceled) {
		if tx != nil {
			_ = tx.Rollback()
		}
		t.Fatalf("取消的 BeginTx err = %v, want context.Canceled", err)
	}

	closed, err := Open(path)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("close second handle: %v", err)
	}
	if tx, err := closed.BeginTx(ctx, nil); err == nil {
		_ = tx.Rollback()
		t.Fatal("已关闭的连接还能开始写事务")
	}
	if _, err := closed.Exec(`INSERT INTO wb_commit (value) VALUES ('after-close')`); err == nil {
		t.Fatal("已关闭的连接还能执行写语句")
	}
}

// TestWhiteboxNoStubRemains v3 的 `TestNoStubRemains` 在 v4 恢复（静态版）：
// 生产代码里不许再出现 `return ErrNotImplemented` 之类的空壳实现。
//
// 为什么静态扫描而不是逐个调用：v4 的公开面比 v3 大，逐个调用表会随接口漂移；
// “空壳”这个信号的本质是**返回哨兵**，扫源码最直接，且不会漏掉新增方法。
func TestWhiteboxNoStubRemains(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // 注释里的说明不算实现
			}
			// 哨兵声明本身不算空壳；空壳是「返回它」。
			if strings.Contains(trimmed, "return") && strings.Contains(trimmed, "ErrNotImplemented") {
				t.Errorf("%s:%d 仍有 ErrNotImplemented 空壳: %s", name, i+1, trimmed)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有扫到任何生产文件（工作目录不对？）")
	}
}
