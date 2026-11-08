package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersionTable 记录已应用的迁移版本。
//
// 为什么要有它：旧实现用一串 `_, _ = db.Exec("ALTER TABLE ...")` 补列，
// 所有错误被静默吞掉——真实迁移失败会被当成「已经迁移过」，库就坏在无声处。
// 现在每个迁移是编号版本：已记账则跳过，未记账则在一个事务里执行 + 记账，
// 任何一步失败都返回错误（不吞错）。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// baselineVersion 是 squash 出来的那块终态 DDL 的版本号。
//
// schema.sql 从 `-- +migration 1 baseline`（squash 后的终态 DDL）开始，
// 往后的新变更按 `-- +migration 2 …` 追加块（见本文件头的说明）。
// baseline 本身**不再改动**：改它等于让「已发布的库」与「新建的库」分叉。
const baselineVersion = 1

// headVersion 是本版本代码认识的**最高**迁移版本（= schema.sql 的最后一块）。
//
// 它只用来识别「squash 之前建的库」：那种库的 schema_version 里记着 v2..v28，
// 比本代码认识的最高版本还高，且结构与本仓库的终态 DDL 完全不同 → 明确拒绝
// （见 legacyVersionError）。每追加一块迁移都要同步 +1，否则新建的库会被自己拒绝。
const headVersion = 10

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
// 第三个返回参数是「某版本需要用 Go 逻辑实现」的钩子表：SQL 表达不了的变更
// （条件 ALTER TABLE——SQLite 没有 ADD COLUMN IF NOT EXISTS、数据搬迁、表重建）
// 写在钩子里，SQL 块留空。约定：hook 返回 error 时整个迁移回滚；hook 必须**幂等**
// ——同一版本在同一个事务内可能因重试被执行多次。
//
// baseline 全是 DDL，没有钩子，所以这里传 nil。机制本身是保留的：从 `-- +migration 2`
// 往后追加块时，需要 Go 逻辑就把它加进 map。
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
// 约定见 migrateWith：hook 返回 error 时整个迁移回滚；hook 必须**幂等**——
// 同一版本在旧库升级/新建库两条路径上都可能跑到，而重跑必须得到同一结果。
var migrationHooks = map[int]func(*sql.Tx) error{
	8:  backfillCapabilitySpec,
	10: referenceCapabilityFromDeployments,
}

// backfillCapabilitySpec 是 migration 8 的钩子：给正文为空的老行补 spec（契约 §6）。
//
// 三类输入一份输出（都只写 spec 列 + 重算 content_hash，不动任何既有列）：
//
//	① 有产物行的能力 → 正文 = 该产物的 spec 字段（canonical）
//	② notify.phone       → {handler: "notify-phone", params: <原 schema>, outputs: <原>}
//	③ 其它零产物能力 → {handler: "noop",        params: <原 schema>, outputs: <原>}
//
// 三条硬要求（契约 §6.2）：
//   - **幂等**：只碰正文为空（spec = ”）的行，已填的不动；同一输入重跑得到同一 hash；
//   - 回填后必须能过 artifact.Validate（否则缺陷会拖到第一次部署/派发才暴露）；
//   - 拿到 noop 占位的能力**打进日志**（人工补实现的清单）。
//
// 为什么不能给空壳编一份 `exit 0` 正文：那是伪造用户代码（契约 §6 明令禁止）。
// 占位必须是平台认领的 noop —— 列表页能标「未实现」，部署面板也不会把它当能跑的能力。
//
// 为什么必须同时重算 content_hash：v3 起 content_hash ≡ digest（吃 name+version+spec）。
// 不重算的话库里留的是旧口径的 hash，与 spec 对不上，往后任何「同内容重提交」都算不出
// 同一个值，幂等折叠永远命不中——而症状是「什么都对，就是每次都新建一行」。
func backfillCapabilitySpec(tx *sql.Tx) error {
	type pending struct {
		id, namespaceID, name      string
		version                    int
		params, outputs, lifecycle string
	}
	// 先全读出来再写：同一个事务里「边读游标边写表」在 SQLite 上会让游标看到自己的写，
	// 而这里的判定条件是 spec = ''，读到一半被改写会让行为依结果集而变。
	rows, err := tx.Query(`SELECT id, namespace_id, name, version, params_schema, outputs, lifecycle
	                         FROM capability_versions WHERE spec = ''`)
	if err != nil {
		return fmt.Errorf("query capabilities without spec: %w", err)
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.namespaceID, &p.name, &p.version,
			&p.params, &p.outputs, &p.lifecycle); err != nil {
			rows.Close()
			return fmt.Errorf("scan capability without spec: %w", err)
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate capabilities without spec: %w", err)
	}
	rows.Close()
	if len(todo) == 0 {
		return nil
	}

	var noopRefs []string
	for _, p := range todo {
		spec, kind, aerr := specForBackfill(tx, p.namespaceID, p.id, p.name,
			p.params, p.outputs, p.lifecycle)
		if aerr != nil {
			return fmt.Errorf("backfill spec for %s@%d: %w", p.name, p.version, aerr)
		}
		c := &CapabilityVersion{
			NamespaceID: p.namespaceID,
			Name:        p.name,
			Version:     p.version,
			Spec:        spec,
		}
		// 回填出来的正文必须自己站得住（Validate 一次），否则宁可让迁移失败并指名道姓，
		// 也不能把一个坏正文写进库、等第一次部署/派发时才发现（契约 §6.2）。
		// 回填只需要 spec 子文档的字节：产物是读的时候从这一行拼出来的（P2 删了镜像表）。
		specJSON, cerr := prepareCapabilitySpec(c)
		if cerr != nil {
			return fmt.Errorf("backfilled spec for %s@%d is invalid (老库里的 schema/outputs "+
				"可能不合当前规则，先人工修好再升级): %w", p.name, p.version, cerr)
		}
		hash, herr := NormalizeCapability(c)
		if herr != nil {
			return fmt.Errorf("hash backfilled cap %s@%d: %w", p.name, p.version, herr)
		}
		if _, uerr := tx.Exec(
			`UPDATE capability_versions SET spec = ?, content_hash = ? WHERE id = ? AND spec = ''`,
			string(specJSON), hash, p.id,
		); uerr != nil {
			return fmt.Errorf("write backfilled spec for %s@%d: %w", p.name, p.version, uerr)
		}
		if kind != "" {
			noopRefs = append(noopRefs, fmt.Sprintf("%s@%d", p.name, p.version))
		}
	}
	if len(noopRefs) > 0 {
		slog.Warn("能力正文回填：这些能力尚无实现，已装为 noop 占位（请补实现）",
			"event", "store.migration.spec_backfill_noop",
			"count", len(noopRefs),
			"capabilities", strings.Join(noopRefs, ", "))
	}
	return nil
}

// deploymentsCapabilityDDL 是 migration 10 重建出来的 deployments（终态形状，契约 §11.1）。
//
// 放在 Go 里而不是 schema.sql 的迁移块里：重建的**顺序**（新建 → 拷 → 删旧 → 改名）
// 与逐行映射是一件事，拆成「SQL 块 + hook」只会让读者以为其中一半能单独跑。
// 表名先叫 deployments_new，拷完再改名——SQLite 没有 ALTER TABLE ... ALTER COLUMN。
const deploymentsCapabilityDDL = `CREATE TABLE deployments_new (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    digest        TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'pending',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (node_id, capability_id, digest)
)`

// referenceCapabilityFromDeployments 是 migration 10 的钩子：把 deployments 从
// 「引用 artifacts 行」改写成「引用能力行」，然后拆掉 artifacts 镜像表（契约 §11.1）。
//
// 三条硬要求：
//
//   - **一行都不丢**：拷贝走 LEFT JOIN，映射不到能力行的旧行让迁移失败并指名道姓，
//     而不是被 INNER JOIN 静默筛掉（丢一行部署 = 那台机器上的能力从此没有期望）。
//   - **幂等**：重跑时如果 deployments 已经是终态（有 capability_id、没有 artifact_id），
//     只补最后一步（确保镜像表不在）。旧库升级与新建库都会跑到这里，两条路必须同结果。
//   - **不编造**：映射不到就不迁，绝不猜一个 capability_id 出来。
func referenceCapabilityFromDeployments(tx *sql.Tx) error {
	cols, err := tableColumnsTx(tx, "deployments")
	if err != nil {
		return fmt.Errorf("probe deployments columns: %w", err)
	}
	if len(cols) == 0 {
		return fmt.Errorf("deployments 表不存在（迁移顺序坏了？）")
	}
	if cols["capability_id"] && !cols["artifact_id"] {
		// 终态：重建已经做过。这里只把最后一步补齐（镜像表不能留）。
		if _, derr := tx.Exec(`DROP TABLE IF EXISTS artifacts`); derr != nil {
			return fmt.Errorf("drop artifacts mirror: %w", derr)
		}
		return nil
	}
	if !cols["artifact_id"] {
		return fmt.Errorf("deployments 既没有 artifact_id 也没有 capability_id，认不出这是什么结构")
	}
	hasArtifacts, err := tableExistsTx(tx, "artifacts")
	if err != nil {
		return fmt.Errorf("probe artifacts: %w", err)
	}
	if !hasArtifacts {
		return fmt.Errorf("deployments 还带着 artifact_id 外键，但 artifacts 表已不存在——库不自洽")
	}

	type legacyDeployment struct {
		id, namespaceID, nodeID, artifactID, digest, state, reason string
		createdAt, updatedAt                                       int64
		capabilityID                                               sql.NullString
	}
	// 先全部读出来再写：同一个事务里「边读游标边重建表」会把游标下悬空的表删掉。
	rows, err := tx.Query(`SELECT d.id, d.namespace_id, d.node_id, d.artifact_id, d.digest,
	                              d.state, d.reason, d.created_at, d.updated_at,
	                              a.capability_id
	                         FROM deployments d
	                    LEFT JOIN artifacts a ON a.id = d.artifact_id
	                     ORDER BY d.id`)
	if err != nil {
		return fmt.Errorf("query legacy deployments: %w", err)
	}
	var legacy []legacyDeployment
	for rows.Next() {
		var d legacyDeployment
		if err := rows.Scan(&d.id, &d.namespaceID, &d.nodeID, &d.artifactID, &d.digest,
			&d.state, &d.reason, &d.createdAt, &d.updatedAt, &d.capabilityID); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy deployment: %w", err)
		}
		legacy = append(legacy, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate legacy deployments: %w", err)
	}
	rows.Close()

	if _, err := tx.Exec(`DROP TABLE IF EXISTS deployments_new`); err != nil {
		return fmt.Errorf("drop stale deployments_new: %w", err)
	}
	if _, err := tx.Exec(deploymentsCapabilityDDL); err != nil {
		return fmt.Errorf("create deployments_new: %w", err)
	}
	for _, d := range legacy {
		if !d.capabilityID.Valid || d.capabilityID.String == "" {
			return fmt.Errorf("部署行 %s 的 artifact_id=%s 在 artifacts 里找不到（不丢行、也不编造能力：请人工处理这个库）",
				d.id, d.artifactID)
		}
		if _, err := tx.Exec(
			`INSERT INTO deployments_new
			   (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			d.id, d.namespaceID, d.nodeID, d.capabilityID.String, d.digest,
			d.state, d.reason, d.createdAt, d.updatedAt); err != nil {
			return fmt.Errorf("copy deployment %s: %w", d.id, err)
		}
	}

	// 删旧表会连带删掉它上面的索引（idx_deploy_node / idx_deploy_artifact），
	// 所以改名之后按新形状重建：索引是查询形状的一部分，不能少。
	if _, err := tx.Exec(`DROP TABLE deployments`); err != nil {
		return fmt.Errorf("drop legacy deployments: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE deployments_new RENAME TO deployments`); err != nil {
		return fmt.Errorf("rename deployments_new: %w", err)
	}
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_deploy_node ON deployments(node_id)`,
		`CREATE INDEX IF NOT EXISTS idx_deploy_cap ON deployments(capability_id)`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("recreate deployment index: %w", err)
		}
	}

	// 最后一步：镜像表没有读者了（契约 §11.1）。
	if _, err := tx.Exec(`DROP TABLE IF EXISTS artifacts`); err != nil {
		return fmt.Errorf("drop artifacts mirror: %w", err)
	}
	return nil
}

// tableExistsTx 判断表是否存在。
func tableExistsTx(tx *sql.Tx, name string) (bool, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// tableColumnsTx 返回表上的列名集合（表不存在时返回空 map）。
func tableColumnsTx(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// specForBackfill 算出某个老能力该拿到的正文，返回 (正文, 占位类型, error)。
//
// 占位类型非空时表示「这是编造的（noop/notify-phone）」，调用方据此打清单；
// 从产物里读到正文时返回空串——那是用户内容，不是占位。
func specForBackfill(tx *sql.Tx, namespaceID, capabilityID, name, params, outputs, lifecycle string) (artifact.Spec, string, error) {
	// ① 有产物 → 正文就是产物里的 spec（契约 §6 第一行）。取最新那份：
	// 老库可能给同一能力留过多行产物（旧版允许历史行），最新的才代表现状。
	//
	// 这里读的是**只在版本 ≤9 存在**的 artifacts 表：migration 8 的钩子只可能跑在
	// 那之前的库上（migration 10 才删表）。仍然探测一次，是为了让「直接重跑 hook」
	// （契约 §6.2 的幂等要求）在表已经删掉的库上也安全地退回 ②③，而不是撞 no such table。
	hasArtifacts, perr := tableExistsTx(tx, "artifacts")
	if perr != nil {
		return artifact.Spec{}, "", fmt.Errorf("probe artifacts: %w", perr)
	}
	var raw string
	var err error
	if hasArtifacts {
		err = tx.QueryRow(
			`SELECT spec FROM artifacts WHERE namespace_id = ? AND capability_id = ?
			  ORDER BY created_at DESC, id DESC LIMIT 1`, namespaceID, capabilityID).Scan(&raw)
	} else {
		err = sql.ErrNoRows
	}
	switch {
	case err == nil:
		var doc struct {
			Spec artifact.Spec `json:"spec"`
		}
		if jerr := json.Unmarshal([]byte(raw), &doc); jerr != nil {
			return artifact.Spec{}, "", fmt.Errorf("decode artifact spec: %w", jerr)
		}
		return doc.Spec, "", nil
	case !errors.Is(err, sql.ErrNoRows):
		return artifact.Spec{}, "", fmt.Errorf("probe artifact: %w", err)
	}

	// ②③ 零产物：notify.phone 是平台自带的手机推送动作（中枢执行器），其余一律 noop 占位。
	lc := lifecycleRaw(lifecycle)
	outs := decodeStringList(outputs)
	if name == NotifyPhoneCapability.Name {
		return HandlerSpec(artifact.HandlerNotifyPhone, json.RawMessage(params), outs, lc),
			artifact.HandlerNotifyPhone, nil
	}
	return HandlerSpec(artifact.HandlerNoop, json.RawMessage(params), outs, lc),
		artifact.HandlerNoop, nil
}

// lifecycleRaw 把 lifecycle 列原样搬进正文；空列 / `null` / `{}` 一律当作「没声明」
// 返回 nil（omitempty 会把它整个省掉，于是规范字节里不出现这个键）。
func lifecycleRaw(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil
	}
	return json.RawMessage(trimmed)
}

// decodeStringList 解 outputs 列；坏值退回空列表（这是老数据，不能因此让迁移失败——
// 它只影响 spec 里的出口声明，不影响库能不能打开）。
func decodeStringList(raw string) []string {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// migrateWith 执行迁移：每个版本一个事务，成功才记账。
func migrateWith(db *sql.DB, set migrationSet, hooks map[int]func(*sql.Tx) error) (applied int, err error) {
	if _, err := db.Exec(schemaVersionTable); err != nil {
		return 0, fmt.Errorf("create schema_version: %w", err)
	}
	done, err := appliedVersions(db)
	if err != nil {
		return 0, err
	}
	if err := legacyVersionError(done); err != nil {
		return 0, err
	}
	for _, m := range set {
		if done[m.version] {
			continue
		}
		if err := applyMigration(db, m, hooks[m.version]); err != nil {
			return applied, fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		applied++
	}
	return applied, nil
}

// legacyVersionError 拦住「squash 之前建的库」。
//
// 判据：schema_version 里出现过比 baseline 更高的版本——baseline 只记 1，所以高了
// 一定是老库（v2..v28）。
//
// 为什么必须**明说**而不是尽力兼容：squash 把 28 块增量迁移合并成了一份终态 DDL，
// 升级路径（逐块 ALTER / 重建表 / 数据搬迁）已经整条删掉。老库就这么打开的话，
// 缺列会在第一次查询时以 `no such column` 炸开，而且是在业务路径上炸——半死不活，
// 还看不出根因。这里直接给出可操作的一句话。
//
// 为什么**不**静默删数据重建：这是用户的数据目录。删不删由人决定，程序只负责说清
// 「删掉就能起来，要留的数据先用 cmd/devseed 之类的工具重放」。
func legacyVersionError(done map[int]bool) error {
	maxDone := 0
	for v := range done {
		if v > maxDone {
			maxDone = v
		}
	}
	if maxDone <= headVersion {
		return nil
	}
	return fmt.Errorf(
		"store: 这个库是 squash 之前的版本（v%d）；本版本不再支持增量升级，请删除数据目录后重启（数据可先用 cmd/devseed 重建）",
		maxDone)
}

// applyMigration 在单个事务内执行一个迁移并记账。
func applyMigration(db *sql.DB, m migration, hook func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
	rows, err := db.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_version: %w", err)
	}
	defer rows.Close()
	done := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_version: %w", err)
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_version: %w", err)
	}
	return done, nil
}

// SchemaVersion 返回当前库的最高已应用版本（0 = 空库）。供健康检查/测试使用。
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
