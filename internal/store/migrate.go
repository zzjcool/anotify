package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersionTable 记录已应用的迁移版本。
//
// 它是**记账**，不是布局身份：「哪些迁移块跑过」。v3 的库同样记着 1..11，
// v4 baseline 也记 1——只看记账，一个 v3 库和一个刚建好的 v4 库长得一模一样。
// 表结构到底是哪一代由 layoutFormatV4（PRAGMA user_version）单独记录。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// baselineVersion 是 v4 baseline 的版本号（schema.sql 唯一的一块）。
const baselineVersion = 1

// headVersion 是本版本代码认识的**最高**迁移版本（= schema.sql 的最后一块）。
//
// v4 开发期只有一块 baseline，没有历史增量块：`-- +migration 1 baseline` 之后
// 不许再出现任何 v3 的迁移块（v3→v4 升级路径不存在，见 ErrUnsupportedFormat）。
// 每追加一块 v4 迁移都要同步 +1。
const headVersion = 1

// layoutFormatV4 是 v4 **布局格式**标记（SQLite 的 PRAGMA user_version）。
//
// 为什么不能拿 schema_version=1 当布局身份：记账回答的是「跑过哪些迁移块」，
// 不回答「表结构是哪一代」；v3 的库同样记着 schema_version=1（一直到 11）。
// 所以 v4 用独立标记记录布局，并且只在**一个地方**写它：
// 空库新建时，baseline DDL、记账行与这个标记落在**同一个事务**里，要么全有要么全无。
//
// 于是三件事被彻底分开：
//   - marker=4：这个文件里的表结构就是 v4 baseline（再逐表/列/约束校验，见 validateV4Layout）；
//   - 有 schema_version 却没 marker：不是 v4 建的库（v3 或半写）→ ErrUnsupportedFormat，
//     **绝不补 marker** 当成新库（补了就等于把 v3 的旧表当成 v4 用，缺列/缺表会在业务路径上炸）；
//   - marker 既不是 4 也不是 0：未知布局 → ErrUnsupportedFormat。
const layoutFormatV4 = 4

// 两个具名拒绝理由：Open 的失败必须能按类分辨（调用方/测试不该靠匹配中文文案）。
var (
	// ErrUnsupportedFormat：这个数据目录不是「v4 新建的库」——v3/更旧的库（有旧业务表，
	// 或有 schema_version 记账但没布局标记）、未知的布局标记、或比本代码更高的记账版本。
	// 拒绝时**不改写任何东西**：不补标记、不建 v4 表、不删用户数据。
	ErrUnsupportedFormat = errors.New("store: 数据目录不是 v4 新建的库（不支持旧库升级）")
	// ErrSchemaInvalid：库自称是 v4 布局（PRAGMA user_version=4），但结构与 baseline 不一致
	// ——半写（缺记账）、缺表、缺列、残留 v3 列、缺关键唯一约束。
	// 程序**不会**在坏库上做 `CREATE IF NOT EXISTS` 修补（那只会把「缺列」藏成「查询时才炸」），
	// 也不会删除用户数据；只把缺什么说清楚。
	ErrSchemaInvalid = errors.New("store: v4 布局标记与库结构不一致")
)

// legacyBusinessTables 是「没有 schema_version 但已经有业务表」的判定清单。
//
// 这种库只可能来自 v3 或更早：v4 的 Open 只在一次事务里建 baseline，
// 所以「业务表在、记账表不在、布局标记也不是 4」= 旧库。命中就拒绝，绝不尝试兼容。
var legacyBusinessTables = []string{
	"users",
	"passkeys",
	"sessions",
	"api_keys",
	"devices",
	"namespaces",
	"capability_versions",
	"nodes",
	"tasks",
	"events",
	"edges",
	"sources",
	"playbooks",
	"deployments",
}

// migrationHeader 匹配分块注释，例如：`-- +migration 2 add_column`
var migrationHeader = regexp.MustCompile(`^--\s*\+migration\s+(\d+)\s+(\w+)\s*$`)

// migration 是 schema.sql 里的一块（一个编号版本）。
type migration struct {
	version int
	name    string
	body    string // 去掉分块头之后的 SQL；不含任何 statement
}

// migrations 是解析 schema.sql 得到的迁移列表（按 version 升序）。
type migrationSet []migration

// parseMigrations 把 schema.sql 按 `-- +migration N name` 切块。
//
// 约束（parseMigrations 会强制检查）：
//   - 版本号从 1 开始连续递增，不跳号、不重复（漏一个编号会导致「以为迁移过」）
//   - 每块必须有非空的名字
func parseMigrations(src string) (migrationSet, error) {
	var out migrationSet
	var cur *migration
	var buf strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.body = strings.TrimSpace(buf.String())
		out = append(out, *cur)
		buf.Reset()
	}
	for _, line := range strings.Split(src, "\n") {
		if m := migrationHeader.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			v, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("parse migration version %q: %w", m[1], err)
			}
			cur = &migration{version: v, name: m[2]}
			continue
		}
		if cur == nil {
			continue // 文件头的说明文字
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	for i, mg := range out {
		if want := i + 1; mg.version != want {
			return nil, fmt.Errorf("migration sequence broken: at position %d got version %d (want %d)", i, mg.version, want)
		}
	}
	return out, nil
}

// migrate 应用所有未记账的迁移，并返回本次实际应用的版本数。
//
// modify（测试注入用）在校验完版本序列后、执行前被调用，可修改待执行的迁移
// ——用于断言「迁移失败必须回滚且不记账」。
//
// v4 baseline 全是 DDL，没有钩子；migrateWith 的钩子表保留给后续新块。
func migrate(db *sql.DB, modify func(*migrationSet)) (int, error) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		return 0, fmt.Errorf("parse schema.sql: %w", err)
	}
	if modify != nil {
		modify(&set)
	}
	return migrateWith(db, set, migrationHooks)
}

// migrationHooks 是「SQL 表达不了」的变更（数据搬迁）的钩子表。
//
// 约定见 migrateWith：hook 返回 error 时整个迁移回滚；hook 必须**幂等**。
// v4 baseline 不需要钩子（它建的是一个空库的全部结构，没有要搬的数据）。
var migrationHooks = map[int]func(*sql.Tx) error{}

// migrateWith 执行迁移：新库的 baseline（DDL + 记账 + 布局标记）一个事务，
// 之后的版本各自一个事务。
//
// **顺序是这里最重要的东西**：格式判定必须在任何写操作之前完成。
// 旧实现先 `CREATE TABLE IF NOT EXISTS schema_version`、再按记账决定要不要跑 baseline，
// 于是「schema_version=1 的 v3 库」会跳过 baseline，然后在业务路径上炸「no such column」。
// 现在先探测（只读）→ 不匹配就具名拒绝 → 只有真正空的新库才会被建。
func migrateWith(db *sql.DB, set migrationSet, hooks map[int]func(*sql.Tx) error) (applied int, err error) {
	probe, err := probeDatabaseFormat(db)
	if err != nil {
		return 0, err
	}
	if probe.marker == layoutFormatV4 {
		// 已经是 v4 布局：**只校验，不修复**。坏库必须在这里被点名，
		// 而不是被 CREATE IF NOT EXISTS 自愈成「看起来正常」。
		if err := validateV4Layout(db, probe); err != nil {
			return 0, err
		}
	} else {
		if err := requireBrandNewDatabase(probe); err != nil {
			return 0, err
		}
		if len(set) == 0 || set[0].version != baselineVersion {
			return 0, fmt.Errorf(
				"migrate: 空库必须先有一块 version %d 的 baseline（实际首块 = %v）",
				baselineVersion, migrationVersions(set))
		}
	}

	for _, m := range set {
		if probe.versions[m.version] {
			continue
		}
		// 新库的首块 baseline：DDL、记账行、布局标记在同一个事务里（见 applyBaselineMigration）。
		apply := applyMigration
		if probe.marker != layoutFormatV4 && m.version == baselineVersion {
			apply = applyBaselineMigration
		}
		if err := apply(db, m, hooks[m.version]); err != nil {
			return applied, fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		applied++
	}
	return applied, nil
}

// migrationVersions 把迁移序列的版本号拍成切片（错误消息用）。
func migrationVersions(set migrationSet) []int {
	out := make([]int, 0, len(set))
	for _, m := range set {
		out = append(out, m.version)
	}
	return out
}

// databaseFormat 是「这个数据目录现在是什么」的只读探测结果。
type databaseFormat struct {
	// marker 是 PRAGMA user_version：0 = 从没写过布局标记（v3 与全新空库都是 0）。
	marker int
	// ledger 表示 schema_version 表存在。
	ledger bool
	// versions 是已记账的版本；未知非零格式不读取 ledger。
	versions map[int]bool
	// tables 是现有用户表名；objects 包括表/视图/触发器/索引。
	// 只排除字面 sqlite_ 前缀，sqliteXkeep 不是内部对象。
	tables  []string
	objects []string
}

// probeDatabaseFormat 只读地摸清现状：布局标记、记账表、现有用户表。
//
// 它不建表、不写标记、不改任何东西——顺序错了（先建记账表再判定）就会把
// 「没有记账表」这条旧库证据自己抹掉。
func probeDatabaseFormat(db *sql.DB) (databaseFormat, error) {
	f := databaseFormat{versions: map[int]bool{}}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&f.marker); err != nil {
		return f, fmt.Errorf("probe layout marker (PRAGMA user_version): %w", err)
	}
	tables, err := userTables(db)
	if err != nil {
		return f, err
	}
	f.tables = tables
	f.objects, err = userSchemaObjects(db)
	if err != nil {
		return f, err
	}
	for _, name := range tables {
		if name == "schema_version" {
			f.ledger = true
			break
		}
	}
	// 未知非零标记直接按格式拒绝；无标记的旧 ledger 只读版本以保留既有具名诊断。
	if !f.ledger || (f.marker != 0 && f.marker != layoutFormatV4) {
		return f, nil
	}
	versions, err := appliedVersions(db)
	if err != nil {
		kind := ErrSchemaInvalid
		if f.marker == 0 {
			kind = ErrUnsupportedFormat
		}
		return f, fmt.Errorf("%w：schema_version 结构或记录损坏：%w", kind, err)
	}
	f.versions = versions
	return f, nil
}

// userSchemaObjects 列出所有非 SQLite 内部的主库对象（不是仅用户表）。
func userSchemaObjects(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT type || ':' || name FROM sqlite_master WHERE substr(name, 1, 7) <> 'sqlite_' ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("list schema objects: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan schema object: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// userTables 列出用户表（sqlite_ 前缀是 SQLite 自己的内部表/统计表）。
func userTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 7) <> 'sqlite_' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tables: %w", err)
	}
	return out, nil
}

// requireBrandNewDatabase 拦住所有「不是 v4 新建的库」。
//
// 为什么必须**明说**而不是尽力兼容：v4 baseline 是全新结构（列、表、语义都变了），
// 升级路径根本不存在。就这么打开的话，缺列/缺表会在第一次查询时以
// `no such column` / `no such table` 炸开，而且是在业务路径上炸——半死不活，
// 还看不出根因。
//
// 为什么**不**静默删数据重建、也不补标记：这是用户的数据目录。删不删由人决定；
// 补标记更糟——把一个 v3 库标成 v4，等于用错误的结构继续写入新数据。
func requireBrandNewDatabase(f databaseFormat) error {
	if f.marker != 0 {
		return fmt.Errorf(
			"%w：数据目录带着未知的布局标记（PRAGMA user_version=%d，v4 只认 %d）。"+
				"程序不会改写标记、不会自动删除数据；请换一个空数据目录",
			ErrUnsupportedFormat, f.marker, layoutFormatV4)
	}
	if f.ledger {
		return fmt.Errorf("%w：数据目录已有 schema_version 记账（最高版本 %d，共 %d 行）却没有 v4 布局标记（PRAGMA user_version=0）。schema_version 只记录迁移，不代表 v4 布局；程序不会补写标记、也不会自动删除数据", ErrUnsupportedFormat, maxVersion(f.versions), len(f.versions))
	}
	// 空库必须没有任何用户 schema 对象；不碰撞的视图也不属于新库。
	if len(f.objects) > 0 {
		if legacy := legacyTablesIn(f.tables); len(legacy) > 0 {
			return fmt.Errorf("%w：数据目录里已有旧版业务表（%s）但没有 schema_version；v4 不支持旧库升级。程序不会自动删除数据", ErrUnsupportedFormat, strings.Join(legacy, ", "))
		}
		return fmt.Errorf("%w：数据目录已有用户 schema 对象（%s）；v4 只支持真正空的新库。程序不会自动删除数据", ErrUnsupportedFormat, strings.Join(f.objects, ", "))
	}
	return nil
}

// legacyTablesIn 挑出 f.tables 里已知的旧版业务表（错误消息点名用）。
func legacyTablesIn(tables []string) []string {
	var out []string
	for _, name := range tables {
		for _, legacy := range legacyBusinessTables {
			if name == legacy {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// columnRequirement 是「这张表必须有这些列」。
type columnRequirement struct {
	table   string
	columns []string
}

// v4RetiredColumns 是 v3 有、v4 明确删掉的列。
//
// marker=4 的库里出现它们，说明这个文件不是「完整应用过 v4 baseline」的 v4 库
// （半写、或者被旧版本代码/手工改动过）→ ErrSchemaInvalid。
// 注意 nodes.status 是 v4 自己的列（offline|online|draining），不是 v3 的 tasks.status。
var v4RetiredColumns = []columnRequirement{
	{"nodes", []string{"namespace_id"}},
	{"node_instances", []string{"namespace_id"}},
	{"capability_versions", []string{
		"outputs", "params_schema", "lifecycle", "sticky", "source", "content_hash", "balance", "handler",
	}},
	{"tasks", []string{
		"status", "attempts", "instance_id", "cap_hash", "graph_id", "path", "token", "not_before",
	}},
	{"edges", []string{"from_port", "literals", "mapping", "priority", "position"}},
}

// validateV4Layout 对实际 PRAGMA 事实与嵌入 baseline 的参照结构做只读比对。
// 参照库只在独立内存中创建一次；待打开的用户库从不接受 DDL、探针 DML 或修补。
func validateV4Layout(db *sql.DB, f databaseFormat) error {
	if !f.ledger {
		return fmt.Errorf("%w：有 v4 布局标记但没有 schema_version 记账表（baseline DDL、记账行与标记必须同事务写成）", ErrSchemaInvalid)
	}
	if !f.versions[baselineVersion] {
		return fmt.Errorf("%w：有 v4 布局标记但没有 baseline（版本 %d）记账行；程序不会补记账", ErrSchemaInvalid, baselineVersion)
	}
	if maxDone := maxVersion(f.versions); maxDone > headVersion {
		return fmt.Errorf("%w：schema_version 记账版本 %d 高于本版本代码认识的 %d", ErrSchemaInvalid, maxDone, headVersion)
	}
	want, err := baselineLayout()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(f.tables))
	for _, name := range f.tables {
		present[name] = true
	}
	var names []string
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, table := range names {
		if !present[table] {
			problems = append(problems, "缺表 "+table)
			continue
		}
		got, err := readTableLayout(db, table)
		if err != nil {
			return fmt.Errorf("%w：读取 %s 结构：%w", ErrSchemaInvalid, table, err)
		}
		for _, req := range v4RetiredColumns {
			if req.table != table {
				continue
			}
			for _, col := range req.columns {
				if _, ok := got.columns[col]; ok {
					problems = append(problems, "残留 v3 列 "+table+"."+col)
				}
			}
		}
		for col, expected := range want[table].columns {
			actual, ok := got.columns[col]
			if !ok {
				problems = append(problems, "缺列 "+table+"."+col)
			} else if actual != expected {
				problems = append(problems, "列定义/主键身份不符 "+table+"."+col)
			}
		}
		for col := range got.columns {
			if _, ok := want[table].columns[col]; !ok {
				problems = append(problems, "多余列 "+table+"."+col)
			}
		}
		if got.withoutRowID != want[table].withoutRowID || got.strict != want[table].strict || !reflect.DeepEqual(primaryKeys(got), primaryKeys(want[table])) {
			problems = append(problems, "主键身份不符 "+table)
		}
		if !reflect.DeepEqual(got.foreignKeys, want[table].foreignKeys) {
			problems = append(problems, "外键定义不符 "+table)
		}
		// Only the immutable UID primary key may cover tombstones. Every other
		// unique key must use the exact live predicate, including extra indexes.
		if table == "capability_versions" {
			for _, actual := range got.unique {
				if !actual.primary && (!actual.partial || !actual.livePredicate) {
					problems = append(problems, "能力唯一键必须仅约束 live 行 "+table)
				}
			}
		}
		for _, required := range want[table].unique {
			matched := false
			for _, actual := range got.unique {
				// capability 的 live 唯一键必须精确匹配，不能以全表唯一阻止删除后重建。
				partialMatches := !actual.partial || (required.partial && actual.emitPredicate)
				if table == "capability_versions" && required.partial {
					partialMatches = actual.partial && actual.livePredicate
				}
				if reflect.DeepEqual(actual.columns, required.columns) && partialMatches {
					matched = true
					break
				}
			}
			if !matched {
				var cols []string
				for _, col := range required.columns {
					cols = append(cols, col.name.String)
				}
				problems = append(problems, "缺唯一约束 "+table+" ("+strings.Join(cols, ", ")+")")
			}
		}
		// CHECK 约束是 PRAGMA 看不见的业务不变量（未解析失败行的形状、单行配置表的 id）。
		// 参照侧与实测侧走同一个 reader（baselineLayout 从内嵌 schema.sql 自动派生），
		// 所以「换名字、改表达式、加约束、删约束」都会被抓到——逐条等价，不是「包含」。
		for _, problem := range compareChecks(table, want[table].checks, got.checks) {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w：%s。程序不会在坏库上做 CREATE IF NOT EXISTS 修补，也不会删除用户数据", ErrSchemaInvalid, strings.Join(problems, "；"))
	}
	return nil
}

// baselineLayout 的唯一事实源是 schema.sql，而非另一份易漏字段/身份的手写清单。
var baselineLayoutCache struct {
	sync.Once
	tables map[string]tableLayout
	err    error
}

func baselineLayout() (map[string]tableLayout, error) {
	baselineLayoutCache.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			baselineLayoutCache.err = err
			return
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		set, err := parseMigrations(schemaSQL)
		if err == nil && (len(set) != 1 || set[0].version != baselineVersion) {
			err = fmt.Errorf("expected one v4 baseline")
		}
		if err == nil {
			_, err = db.Exec(schemaVersionTable)
		}
		if err == nil {
			_, err = db.Exec(set[0].body)
		}
		if err != nil {
			baselineLayoutCache.err = err
			return
		}
		names, err := userTables(db)
		if err != nil {
			baselineLayoutCache.err = err
			return
		}
		baselineLayoutCache.tables = make(map[string]tableLayout, len(names))
		for _, name := range names {
			layout, err := readTableLayout(db, name)
			if err != nil {
				baselineLayoutCache.err = err
				return
			}
			baselineLayoutCache.tables[name] = layout
		}
	})
	if baselineLayoutCache.err != nil {
		return nil, fmt.Errorf("read embedded v4 baseline: %w", baselineLayoutCache.err)
	}
	return baselineLayoutCache.tables, nil
}

type columnLayout struct {
	typ                 string
	notNull, pk, hidden int
	defaultValue        sql.NullString
}

type indexColumn struct {
	name      sql.NullString
	ordinary  bool // cid=-2 是表达式，cid=-1 是 rowid；不能忽略后拿前缀匹配。
	desc      int
	collation string
}

type uniqueIndex struct {
	columns                                        []indexColumn
	primary, partial, emitPredicate, livePredicate bool
}

type tableLayout struct {
	columns              map[string]columnLayout
	unique               []uniqueIndex
	foreignKeys          []string          // 每条 FK 一组，按 seq 保留复合键顺序，忽略 SQLite 分配的 id。
	checks               []checkConstraint // 按 name 排序；PRAGMA 不暴露 CHECK，只能从 DDL 取。
	withoutRowID, strict int
}

func readTableLayout(db *sql.DB, table string) (tableLayout, error) {
	out := tableLayout{columns: map[string]columnLayout{}}
	if err := db.QueryRow(`SELECT wr, strict FROM pragma_table_list WHERE schema='main' AND name=?`, table).Scan(&out.withoutRowID, &out.strict); err != nil {
		return out, err
	}
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?)`, table)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var name string
		var col columnLayout
		if err := rows.Scan(&name, &col.typ, &col.notNull, &col.defaultValue, &col.pk, &col.hidden); err != nil {
			_ = rows.Close()
			return out, err
		}
		col.typ = strings.ToUpper(col.typ)
		out.columns[name] = col
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	out.foreignKeys, err = foreignKeysDB(db, table)
	if err != nil {
		return out, err
	}
	out.unique, err = uniqueIndexesDB(db, table)
	if err != nil {
		return out, err
	}
	// CHECK 只能从建表 DDL 取。DDL 读不出/解析不了时返回 error 由调用方包装成
	// ErrSchemaInvalid：fail closed，不许把「无法判定」当成「没有 CHECK」放行。
	out.checks, err = checksFromDDL(db, table)
	return out, err
}

func foreignKeysDB(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT id, seq, "table", "from", "to", on_update, on_delete, match FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[int]string{}
	for rows.Next() {
		var id, seq int
		var target, from, to, update, deleteAction, match string
		if err := rows.Scan(&id, &seq, &target, &from, &to, &update, &deleteAction, &match); err != nil {
			return nil, err
		}
		groups[id] += fmt.Sprintf("%d:%q:%q:%q:%q:%q:%q;", seq, target, from, to, update, deleteAction, match)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, group := range groups {
		out = append(out, group)
	}
	sort.Strings(out)
	return out, nil
}

func uniqueIndexesDB(db *sql.DB, table string) ([]uniqueIndex, error) {
	rows, err := db.Query(`SELECT name, origin, partial FROM pragma_index_list(?) WHERE "unique"=1 ORDER BY seq`, table)
	if err != nil {
		return nil, err
	}
	type info struct {
		name, origin string
		partial      bool
	}
	var indexes []info
	for rows.Next() {
		var i info
		if err := rows.Scan(&i.name, &i.origin, &i.partial); err != nil {
			_ = rows.Close()
			return nil, err
		}
		indexes = append(indexes, i)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []uniqueIndex
	for _, i := range indexes {
		cols, err := indexKeyColumnsDB(db, i.name)
		if err != nil {
			return nil, err
		}
		index := uniqueIndex{columns: cols, primary: i.origin == "pk", partial: i.partial}
		if i.partial && (table == "emits" || table == "capability_versions") {
			var ddl string
			if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, i.name).Scan(&ddl); err != nil {
				return nil, err
			}
			if table == "emits" {
				index.emitPredicate = validEmitPredicate(ddl)
			} else {
				index.livePredicate = validNullPredicate(ddl, "DELETED_AT", false)
			}
		}
		out = append(out, index)
	}
	return out, nil
}

func indexKeyColumnsDB(db *sql.DB, index string) ([]indexColumn, error) {
	// key=0 的辅助 rowid/covering 列不是唯一键；key=1 的所有列（含 NULL 表达式）全保留。
	rows, err := db.Query(`SELECT cid, name, "desc", coll FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indexColumn
	for rows.Next() {
		var cid int
		var col indexColumn
		if err := rows.Scan(&cid, &col.name, &col.desc, &col.collation); err != nil {
			return nil, err
		}
		col.ordinary = cid >= 0
		out = append(out, col)
	}
	return out, rows.Err()
}

func primaryKeys(layout tableLayout) [][]indexColumn {
	var keys [][]indexColumn
	for _, i := range layout.unique {
		if i.primary {
			keys = append(keys, i.columns)
		}
	}
	return keys
}

// checkConstraint 是表上的一条 CHECK 约束。
//
// 为什么不用 PRAGMA：SQLite 的 pragma_table_xinfo / index_list / foreign_key_list 都
// 不暴露 CHECK，它只存在于 sqlite_master.sql 的 DDL 文本里——所以必须从 DDL 取，
// 走与 validNullPredicate 同一套 token 词法（注释/引号不能拿来伪造）。
//
// name：显式 CONSTRAINT 名原样保留；列级 CHECK 用列名本身；无名表级 CHECK 用 "check#<n>"
// （n = 该表无名表级约束的 1-based 序号）。展示名由 compareChecks 统一加 "<表>." 前缀，
// 所以错误文案里表名只出现一次（tasks.tasks_unresolved_failure_shape / system_config.id /
// tasks.check#1）。参照侧与实测侧走同一个 reader，所以「换名 + 换表达式」会同时表现为缺一条/多一条。
type checkConstraint struct {
	name   string
	tokens string
}

// ddlToken 是 DDL 分词结果中的一项。
//
// bare 只对「裸词/裸标点」为真：引号标识符（"CHECK"）与字符串字面量（'CHECK'）
// 永远不会被当成 CREATE/CHECK 这类关键字，否则一个列名就能伪造一条约束。
type ddlToken struct {
	text string // 去引号后的原样文本（保留大小写）：具名约束/表名/列名用它
	norm string // 比较用的规范形式：标识符与裸词转大写，字符串字面量原样
	bare bool
}

// isBare 报告 token 是不是「裸 token 且规范形式等于 want」。
func isBare(tok ddlToken, want string) bool { return tok.bare && tok.norm == want }

// tokenizeSchemaDDL 用与 validNullPredicate 同一套 SQL 词法切 DDL，并丢弃注释。
//
// 先丢注释是必须的：baseline 的 tasks DDL 里就有一行注释提「两条 CHECK」，
// 不丢注释的话「写注释」就能凭空造出一条约束（反之也能把一条真约束注释掉）。
func tokenizeSchemaDDL(ddl string) []ddlToken {
	var out []ddlToken
	for _, raw := range schemaSQLToken.FindAllString(ddl, -1) {
		if strings.HasPrefix(raw, "--") || strings.HasPrefix(raw, "/*") {
			continue
		}
		switch {
		case raw[0] == '\'':
			// 字符串字面量原样保留：'failed' 与 'FAILED' 是不同的值，不能一起大写化。
			out = append(out, ddlToken{text: raw, norm: raw})
		case len(raw) >= 2 && (raw[0] == '"' || raw[0] == '`' || raw[0] == '['):
			quote := string(raw[0])
			text := strings.ReplaceAll(raw[1:len(raw)-1], quote+quote, quote)
			out = append(out, ddlToken{text: text, norm: strings.ToUpper(text)})
		default:
			out = append(out, ddlToken{text: raw, norm: strings.ToUpper(raw), bare: true})
		}
	}
	return out
}

// checksFromDDL 从 sqlite_master 的建表 DDL 里读出该表的全部 CHECK 约束。
//
// fail closed：DDL 取不到（空/NULL/查不到表）或不符合 CREATE TABLE 的语法形状，
// 一律返回 error 由调用方包成 ErrSchemaInvalid——「读不出 CHECK」绝不能被当成
// 「没有 CHECK」放行，那正是「掏空 CHECK 的库能过门禁」这个 bug 的根。
func checksFromDDL(db *sql.DB, table string) ([]checkConstraint, error) {
	var ddl sql.NullString
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
		return nil, fmt.Errorf("读取 %s 的建表 DDL：%w", table, err)
	}
	if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
		return nil, fmt.Errorf("%s 没有可读的建表 DDL（无法判定 CHECK 约束）", table)
	}
	return parseChecksFromDDL(table, ddl.String)
}

// parseChecksFromDDL 按 CREATE TABLE 的括弧结构把 DDL 切成「定义项」，逐项取 CHECK。
// 任何不符合这个形状的 DDL（缺括号、括号不平衡、CHECK 后不是括号）都返回 error。
func parseChecksFromDDL(table, ddl string) ([]checkConstraint, error) {
	malformed := func(reason string) error {
		return fmt.Errorf("%s 的建表 DDL 无法解析：%s", table, reason)
	}
	tokens := tokenizeSchemaDDL(ddl)
	i := 0
	if i >= len(tokens) || !isBare(tokens[i], "CREATE") {
		return nil, malformed("不是 CREATE 语句")
	}
	i++
	if i < len(tokens) && (isBare(tokens[i], "TEMP") || isBare(tokens[i], "TEMPORARY")) {
		i++
	}
	if i >= len(tokens) || !isBare(tokens[i], "TABLE") {
		return nil, malformed("CREATE 后不是 TABLE")
	}
	i++
	if i < len(tokens) && isBare(tokens[i], "IF") {
		if i+2 >= len(tokens) || !isBare(tokens[i+1], "NOT") || !isBare(tokens[i+2], "EXISTS") {
			return nil, malformed("IF 后不是 NOT EXISTS")
		}
		i += 3
	}
	if i >= len(tokens) {
		return nil, malformed("缺表名")
	}
	if tokens[i].text != table {
		return nil, malformed("DDL 的表名不是 " + table)
	}
	i++
	if i >= len(tokens) || !isBare(tokens[i], "(") {
		return nil, malformed("表名后不是括号")
	}
	// 看表级括号的整体范围。
	end, depth := -1, 0
	for j := i; j < len(tokens); j++ {
		switch {
		case isBare(tokens[j], "("):
			depth++
		case isBare(tokens[j], ")"):
			depth--
			if depth == 0 {
				end = j
			}
		}
		if depth < 0 {
			break
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, malformed("表定义括号不平衡")
	}
	// 深度 0 的逗号切分定义项（列定义/表级约束）。
	var items [][]ddlToken
	var item []ddlToken
	level := 0
	for j := i + 1; j < end; j++ {
		tok := tokens[j]
		switch {
		case isBare(tok, "("):
			level++
		case isBare(tok, ")"):
			level--
			if level < 0 {
				return nil, malformed("定义项内括号不平衡")
			}
		}
		if level == 0 && isBare(tok, ",") {
			items = append(items, item)
			item = nil
			continue
		}
		item = append(item, tok)
	}
	if level != 0 {
		return nil, malformed("定义项内括号不平衡")
	}
	if len(item) > 0 {
		items = append(items, item)
	}

	var checks []checkConstraint
	unnamedTableLevel := 0
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		var base string
		scan := 0
		switch {
		case isBare(item[0], "CONSTRAINT"):
			if len(item) < 2 {
				return nil, malformed("CONSTRAINT 后缺约束名")
			}
			base = item[1].text // 具名约束：名字原样保留
			scan = 2
		case isBare(item[0], "CHECK") || isBare(item[0], "PRIMARY") || isBare(item[0], "UNIQUE") || isBare(item[0], "FOREIGN"):
			// 无名表级约束：序号按无名表级约束的出现顺序（两侧同源，序号必然一致）。
			unnamedTableLevel++
			base = fmt.Sprintf("check#%d", unnamedTableLevel)
		default:
			// 列定义：只取列名；表名前缀由 compareChecks 统一加（文案里表名只出现一次）。
			// 列名本身不是 CHECK，从第二个 token 起扫。
			base = item[0].text
			scan = 1
		}
		seen := 0
		for j := scan; j < len(item); j++ {
			if !isBare(item[j], "CHECK") {
				continue
			}
			if j+1 >= len(item) || !isBare(item[j+1], "(") {
				return nil, malformed("CHECK 后不是括号")
			}
			level, closeIdx := 0, -1
			for k := j + 1; k < len(item); k++ {
				switch {
				case isBare(item[k], "("):
					level++
				case isBare(item[k], ")"):
					level--
					if level == 0 {
						closeIdx = k
					}
				}
				if closeIdx >= 0 {
					break
				}
			}
			if closeIdx < 0 {
				return nil, malformed("CHECK 表达式括号不平衡")
			}
			var parts []string
			for k := j + 2; k < closeIdx; k++ {
				parts = append(parts, item[k].norm)
			}
			if len(parts) == 0 {
				return nil, malformed("CHECK 表达式为空")
			}
			seen++
			name := base
			if seen > 1 {
				name = fmt.Sprintf("%s#%d", base, seen)
			}
			checks = append(checks, checkConstraint{name: name, tokens: strings.Join(parts, " ")})
			j = closeIdx
		}
	}
	sort.Slice(checks, func(a, b int) bool { return checks[a].name < checks[b].name })
	// 同名 CHECK 出现 ≥2 次直接拒绝（fail closed），绝不折叠。
	//
	// SQLite 允许重名约束，而“逐条等价”靠名字配对：一旦用 map[name]tokens 折叠，
	// 胜出的是谁就取决于声明顺序——重名恒真约束插在真约束**之前**就能骗过门禁
	// （实测 Open 返回 nil）。重名本身就是结构漂移，先拒它，比对才谈得上逐条等价。
	for i := 1; i < len(checks); i++ {
		if checks[i].name == checks[i-1].name {
			return nil, malformed("CHECK 约束名重复 " + checks[i].name)
		}
	}
	return checks, nil
}

// compareChecks 逐条比对参照与实测的 CHECK：缺失、表达式被改、多余都算缺陷。
// 与「多余列」对称——只查「包含」等于允许偷偷加一条更严的 CHECK（同样是结构漂移）。
// 展示名统一是 "<表>.<name>"：表名只在这里加一次，name 内不带表名前缀。
func compareChecks(table string, want, got []checkConstraint) []string {
	wantTokens := make(map[string]string, len(want))
	for _, c := range want {
		wantTokens[c.name] = c.tokens
	}
	gotTokens := make(map[string]string, len(got))
	for _, c := range got {
		gotTokens[c.name] = c.tokens
	}
	var problems []string
	for _, c := range want {
		actual, ok := gotTokens[c.name]
		if !ok {
			problems = append(problems, "缺 CHECK 约束 "+table+"."+c.name)
			continue
		}
		if actual != c.tokens {
			problems = append(problems, "CHECK 约束与 baseline 不一致 "+table+"."+c.name)
		}
	}
	for _, c := range got {
		if _, ok := wantTokens[c.name]; !ok {
			problems = append(problems, "多余 CHECK 约束 "+table+"."+c.name)
		}
	}
	return problems
}

// PRAGMA 不暴露部分索引谓词；只放行实际 SQL 中 baseline 的非 NULL emit 谓词。
// 先按 SQLite 注释/引号规则取 token，不能让注释或对象名伪造 WHERE 文本。
var schemaSQLToken = regexp.MustCompile(`(?s)--[^\n]*(?:\n|$)|/\*.*?\*/|"(?:[^"]|"")*"|` + "`(?:[^`]|``)*`" + `|\[[^\]]*\]|'(?:[^']|'')*'|[A-Za-z_][A-Za-z_0-9]*|[^\s]`)

func validEmitPredicate(ddl string) bool {
	return validNullPredicate(ddl, "EMIT_ID", true)
}

// Only the two baseline NULL predicates are recognized; operator grouping stays intact.
func validNullPredicate(ddl, column string, notNull bool) bool {
	var tokens []string
	for _, token := range schemaSQLToken.FindAllString(ddl, -1) {
		if strings.HasPrefix(token, "--") || strings.HasPrefix(token, "/*") {
			continue
		}
		switch token[0] {
		case '"', '`', '[':
			quote := string(token[0])
			token = "\x00" + strings.ReplaceAll(token[1:len(token)-1], quote+quote, quote)
		}
		tokens = append(tokens, strings.ToUpper(token))
	}
	where := -1
	for i, token := range tokens {
		if token == "WHERE" {
			where = i
			break
		}
	}
	if where < 0 {
		return false
	}
	predicate := tokens[where+1:]
	if len(predicate) > 0 && predicate[len(predicate)-1] == ";" {
		predicate = predicate[:len(predicate)-1]
	}
	// 只剥离包住整个表达式的括号，不能把 IS (NOT NULL) 的操作数分组抹掉。
	unwrap := func(expr []string) []string {
		for len(expr) >= 2 && expr[0] == "(" && expr[len(expr)-1] == ")" {
			depth, end := 0, -1
			for i, token := range expr {
				switch token {
				case "(":
					depth++
				case ")":
					depth--
				}
				if depth == 0 {
					end = i
					break
				}
			}
			if end != len(expr)-1 {
				break
			}
			expr = expr[1:end]
		}
		return expr
	}
	predicate = unwrap(predicate)
	operatorTokens := []string{"IS", "NULL"}
	if notNull {
		operatorTokens = []string{"IS", "NOT", "NULL"}
	}
	if len(predicate) < len(operatorTokens)+1 {
		return false
	}
	operator := len(predicate) - len(operatorTokens)
	// 只允许配对括号包住精确列标识；不抹除操作数/运算符分组。
	identifier := unwrap(predicate[:operator])
	return len(identifier) == 1 && strings.TrimPrefix(identifier[0], "\x00") == column &&
		reflect.DeepEqual(predicate[operator:], operatorTokens)
}

// maxVersion 返回记账版本里的最高值（空 = 0）。
func maxVersion(versions map[int]bool) int {
	maxDone := 0
	for v := range versions {
		if v > maxDone {
			maxDone = v
		}
	}
	return maxDone
}

// applyMigration 在单个事务内执行一个迁移并记账。
func applyMigration(db *sql.DB, m migration, hook func(*sql.Tx) error) error {
	return applyMigrationTx(db, m, hook, false)
}

// applyBaselineMigration 是**空库首块**专用：baseline DDL、记账行与 v4 布局标记
// 全部落在同一个事务里。
//
// 为什么必须同一个事务：分开写就会出现「有表没标记」（下次 Open 会被当旧库拒掉）
// 或「有标记没表」（marker=4 的半写库）——两种半成品都要人来手工收拾。
// 一次事务失败就整体回滚，库里只会剩下「空」这一种状态。
func applyBaselineMigration(db *sql.DB, m migration, hook func(*sql.Tx) error) error {
	return applyMigrationTx(db, m, hook, true)
}

// applyMigrationTx 是两者的实现：newBaseline=true 时先写布局标记、建记账表，
// 再执行 SQL 块、钩子，最后写记账行；任何一步失败都整体回滚。
func applyMigrationTx(db *sql.DB, m migration, hook func(*sql.Tx) error, newBaseline bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if newBaseline {
		// PRAGMA user_version 是事务性的（写在库头里，随事务提交/回滚），
		// 所以它能和 DDL、记账行一起原子落盘。这里先写，任何后续失败都会把它一起回滚。
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, layoutFormatV4)); err != nil {
			return fmt.Errorf("write layout marker: %w", err)
		}
		if _, err := tx.Exec(schemaVersionTable); err != nil {
			return fmt.Errorf("create schema_version: %w", err)
		}
	}
	if m.body != "" {
		if _, err := tx.Exec(m.body); err != nil {
			return fmt.Errorf("exec sql: %w", err)
		}
	}
	if hook != nil {
		if err := hook(tx); err != nil {
			return fmt.Errorf("hook: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, Now(),
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// appliedVersions 读出已记账的版本。
func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version, applied_at, typeof(version), typeof(applied_at) FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_version: %w", err)
	}
	defer rows.Close()
	done := make(map[int]bool)
	for rows.Next() {
		var v int
		var at int64
		var versionType, timeType string
		if err := rows.Scan(&v, &at, &versionType, &timeType); err != nil {
			return nil, fmt.Errorf("scan schema_version: %w", err)
		}
		if v <= 0 || done[v] || versionType != "integer" || timeType != "integer" {
			return nil, fmt.Errorf("invalid schema_version record: version=%d, applied_at=%d, types=%s/%s, duplicate=%t", v, at, versionType, timeType, done[v])
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_version: %w", err)
	}
	return done, nil
}

// SchemaVersion 返回当前库的最高已应用版本（0 = 空库）。供健康检查/测试使用。
//
// 注意它返回的是**记账**版本（v4 baseline = 1），不是布局格式标记
// （PRAGMA user_version = 4，见 layoutFormatV4）。
func (d *DB) SchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
