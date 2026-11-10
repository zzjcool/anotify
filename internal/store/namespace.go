package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 命名空间（namespace）：**唯一**的隔离边界（#204 / #205）。
//
// 设计见 docs/design/namespaces.md，契约 §2.1.1 / 不变量 I11。
// （I10「一切租户数据先过 org_id」随组织层一起作废，重写留给阶段 3。）
//
// 一句话：能力 / 边 / 源 / 事件 / 任务 / 部署 / 预案**全部**属于某个命名空间。
// 节点是集群级对象（不绑 ns）：一台机器服务哪些 ns 由「申报 ∩ 授权」决定，
// 这里的 NodeCount 是**推导量**（在线且被授权使用该 ns 的机器数），不是所有权。
// 组织（org）这一层已经删除：一套部署 = 一个团队，`namespace_id`
// 是行上唯一的隔离维度，没有比它更粗的过滤锚点了。
const (
	// DefaultNamespaceSlug 是注册时给新命名空间取的 slug 基名。
	// 全局唯一（UNIQUE(slug)），所以第二个及以后的用户拿到
	// `default-2`、`default-3`…
	DefaultNamespaceSlug = "default"
	// DefaultNamespaceName 是默认命名空间的展示名。
	DefaultNamespaceName = "默认"
	// AllNamespaces 是显式跨命名空间汇总的标记（管理员视图）。
	// **只用于读**，且只在 HTTP 层解释 —— store 层不接受它。
	AllNamespaces = "*"
	// maxNamespaceSlugLen 是 slug 的长度上限（含首字符，见 namespaceSlugPattern）。
	maxNamespaceSlugLen = 31
)

var (
	// ErrNamespaceRequired：命名空间资源的读写没带 namespace_id。
	// HTTP 层必须把它翻译成 400（**绝不回退默认命名空间**：回退等于把人写错的地方藏起来）。
	ErrNamespaceRequired = errors.New("store: namespace id required")
	// ErrNamespaceNotFound：命名空间不存在（HTTP 404）。
	ErrNamespaceNotFound = errors.New("store: namespace not found")
	// ErrAuthRequired：调用方没有给出一个**存在的**已登录用户（HTTP 401）。
	ErrAuthRequired = errors.New("store: login required")
	// ErrNamespaceExists：slug 已被占用（HTTP 409）。唯一域是全局。
	ErrNamespaceExists = errors.New("store: namespace slug already exists")
	// ErrLastNamespace：不允许删最后一个命名空间（删了就没有「这套流程属于哪里」了）。
	ErrLastNamespace = errors.New("store: cannot delete the last namespace of the deployment")
)

// CrossNamespaceError 是 I11（跨命名空间不引用）的违反。
//
// 为什么要有独立错误类型而不是拼字符串：HTTP 层要把这四处违反一律翻成 400
// （不是静默忽略——静默忽略会让人以为配置生效了），而 400 需要能识别出来。
type CrossNamespaceError struct {
	What string // 违反的是哪条引用，如 "edges.to_capability"
	Want string // 期望的 namespace_id
	Got  string // 实际指向的 namespace_id
}

func (e *CrossNamespaceError) Error() string {
	return fmt.Sprintf("cross-namespace reference rejected (%s): want namespace %s, got %s",
		e.What, e.Want, e.Got)
}

// NamespaceBlockedError 表示命名空间里还有阻塞删除的对象（HTTP 409）。
//
// 删命名空间会级联删掉机器（令牌失效、全部掉线），所以默认不 force 时必须
// 把阻塞对象列清楚，让用户知道自己在拆什么。
type NamespaceBlockedError struct {
	Blocking []string // 如 ["sources=2", "edges=5", "tasks=13"]
}

func (e *NamespaceBlockedError) Error() string {
	return "namespace is not empty: " + strings.Join(e.Blocking, ", ")
}

// Namespace 是 namespaces 表的一行（列表查询带资源计数，UI 直接用）。
type Namespace struct {
	ID              string `json:"id"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	CreatedAt       int64  `json:"createdAt"`
	ResourceVersion int64  `json:"resourceVersion"`
	// 计数（列表用；单取时为 0 也无所谓）
	CapabilityCount int `json:"capabilityCount"`
	NodeCount       int `json:"nodeCount"`
	SourceCount     int `json:"sourceCount"`
	EdgeCount       int `json:"edgeCount"`
}

// nsQueryer 让同一段 SQL 既能跑在 *DB 上，也能跑在事务里
// （建命名空间 / 注册用户 / 装预案都必须与播种内置能力同事务）。
type nsQueryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// namespaceSlugPattern 是 slug 的合法字符集：它出现在 URL 与令牌作用域里，
// 只允许小写字母、数字、连字符，且以字母或数字开头。
var namespaceSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

const nsColumns = `id, slug, name, created_at, resource_version`

// DefaultNamespaceID 返回本部署**最早插入**的命名空间 id。
//
// 用途：**迁移与后台任务**（老数据回填、全局巡检），不是 HTTP 请求路径。
// HTTP 端点缺 namespaceId 一律 400，绝不回退到这里（契约 §6.2）。
//
// 排序用 rowid 而不是 (created_at, id)：created_at 是**秒级**时间戳，同一秒内
// 建两个命名空间（注册 + 立刻建一个）会撞在一起，只能靠插入顺序（rowid）才是
// 确定的「最早」—— 与旧 PrimaryOrgOf 的取法一致。
func (d *DB) DefaultNamespaceID(ctx context.Context) (string, error) {
	var id string
	err := d.QueryRowContext(ctx,
		`SELECT id FROM namespaces ORDER BY rowid LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup default namespace: %w", err)
	}
	return id, nil
}

// GetNamespace 按 id 取命名空间；不存在返回 ErrNamespaceNotFound（HTTP 404）。
func (d *DB) GetNamespace(ctx context.Context, id string) (*Namespace, error) {
	if id == "" || id == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	var ns Namespace
	err := d.cachedQueryRowContext(ctx,
		`SELECT `+nsColumns+` FROM namespaces WHERE id = ?`, id).
		Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.CreatedAt, &ns.ResourceVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNamespaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}
	return &ns, nil
}

// ListNamespaces 列出本部署的全部命名空间，带各类资源计数（UI 列表直接用）。
// 空列表返回 []（不变量 I9）。
func (d *DB) ListNamespaces(ctx context.Context) ([]*Namespace, error) {
	rows, err := d.cachedQueryContext(ctx,
		// v4 的 nodes 是集群级对象（不绑 ns），NodeCount 由「在线 × 申报 ∩ 授权」
		// 在 Go 侧推导（见下面 nodeCountsByNamespace）：它是展示计数，不是所有权。
		`SELECT n.id, n.slug, n.name, n.created_at, n.resource_version,
		        (SELECT count(*) FROM capability_versions c WHERE c.namespace_id = n.id AND c.deleted_at IS NULL),
		        0,
		        (SELECT count(*) FROM sources s WHERE s.namespace_id = n.id),
		        (SELECT count(*) FROM edges e WHERE e.namespace_id = n.id)
		   FROM namespaces n
		  ORDER BY n.created_at, n.rowid`)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	out := []*Namespace{}
	for rows.Next() {
		var ns Namespace
		if err := rows.Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.CreatedAt, &ns.ResourceVersion,
			&ns.CapabilityCount, &ns.NodeCount, &ns.SourceCount, &ns.EdgeCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan namespace: %w", err)
		}
		out = append(out, &ns)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// 先把结果集读完并关闭，再发第二个查询（`:memory:` 库连接池上限是 1，
	// 边读边查会自己把自己锁死）。
	rows.Close()

	counts, err := d.nodeCountsByNamespace(ctx, out)
	if err != nil {
		return nil, err
	}
	for _, ns := range out {
		ns.NodeCount = counts[ns.Slug]
	}
	return out, nil
}

// nodeCountsByNamespace 推导每个命名空间的在线节点数（Node.status 的 counts.nodes）。
//
// 一台节点算某个 ns 的「在线节点」当且仅当：status=online，且
// ns ∈ node_configs.namespaces（授权，缺省 ["*"]）∩ nodes.declared_namespaces（申报）。
// 这与 EligibleNodes 的授权判定同一份实现（intersectNamespaces），不另写一套口径。
func (d *DB) nodeCountsByNamespace(ctx context.Context, namespaces []*Namespace) (map[string]int, error) {
	counts := make(map[string]int, len(namespaces))
	if len(namespaces) == 0 {
		return counts, nil
	}
	rows, err := d.QueryContext(ctx,
		`SELECT COALESCE(c.namespaces, '["*"]'), n.declared_namespaces
		   FROM nodes n
		   LEFT JOIN node_configs c ON c.node_id = n.id
		  WHERE n.status = ?`, NodeStatusOnline)
	if err != nil {
		return nil, fmt.Errorf("count namespace nodes: %w", err)
	}
	defer rows.Close()
	type lists struct{ authorized, declared []string }
	online := []lists{}
	for rows.Next() {
		var rawAuthorized, rawDeclared string
		if err := rows.Scan(&rawAuthorized, &rawDeclared); err != nil {
			return nil, fmt.Errorf("count namespace nodes: %w", err)
		}
		authorized, err := decodeStringList(rawAuthorized)
		if err != nil {
			return nil, fmt.Errorf("count namespace nodes: %w", err)
		}
		declared, err := decodeStringList(rawDeclared)
		if err != nil {
			return nil, fmt.Errorf("count namespace nodes: %w", err)
		}
		online = append(online, lists{authorized: authorized, declared: declared})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count namespace nodes: %w", err)
	}
	for _, ns := range namespaces {
		for _, n := range online {
			if namespacesAllow(intersectNamespaces(n.authorized, n.declared), ns.Slug) {
				counts[ns.Slug]++
			}
		}
	}
	return counts, nil
}

// CreateNamespace 建命名空间（同一事务内完成）。
//
// v4 不在命名空间里播种任何锚点能力/源：链尾是普通脚本能力，没有中枢内置执行器，
// 因此「新建的 ns 必须自带一份某能力」这个前提不存在了。
//
// slug 全局唯一：已占用返回 ErrNamespaceExists（HTTP 409）。
func (d *DB) CreateNamespace(ctx context.Context, slug, name string) (*Namespace, error) {
	slug = strings.TrimSpace(slug)
	if !namespaceSlugPattern.MatchString(slug) {
		return nil, fmt.Errorf("create namespace: invalid slug %q (只允许小写字母、数字、连字符，且以字母或数字开头)", slug)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create namespace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ns := &Namespace{ID: NewNamespaceID(), Slug: slug, Name: name, CreatedAt: Now(), ResourceVersion: 1}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces (id, slug, name, created_at) VALUES (?,?,?,?)`,
		ns.ID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %q", ErrNamespaceExists, slug)
		}
		return nil, fmt.Errorf("insert namespace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create namespace: %w", err)
	}
	return ns, nil
}

// RenameNamespace 只改展示名。slug 是稳定标识（出现在 URL 与令牌作用域里），不可改。
// RenameNamespace only changes display metadata; slug remains immutable.
func (d *DB) RenameNamespace(ctx context.Context, id, name string, expectedRV int64) error {
	if expectedRV <= 0 {
		return ErrResourceVersionRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename namespace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	ns, err := getNamespaceOn(ctx, ex, "id", id)
	if err != nil {
		return err
	}
	if err := checkResourceRevision(expectedRV, ns.ID, ns.ResourceVersion); err != nil {
		return err
	}
	if err := renameNamespaceOn(ctx, ex, ns, name); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteNamespace 删命名空间。
//
// 两档（api-conventions §7.5：沿用现有删除语义，不做新的级联协议）：
//   - 默认：里面还有能力 / 源 / 预案 / 任务 / 事件 / 部署时返回 *NamespaceBlockedError（HTTP 409），
//     把这些对象列出来——删命名空间不能当"清理配置"做。
//   - force=true：显式级联删除，连能力、源、预案、任务、事件、部署一起删。
//
// 节点（集群级）不在删除范围：一台机器可能同时服务别的 ns，删一个 ns 不该顺手删机器。
//
// **不允许删最后一个命名空间**：删了之后这套部署就没有"这套流程属于哪里"了，
// 而代码里不存在「没有命名空间」的分支（契约 §2.1.1）。
func (d *DB) DeleteNamespace(ctx context.Context, id string, force bool, expectedRV int64) error {
	if expectedRV <= 0 {
		return ErrResourceVersionRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete namespace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	ns, err := getNamespaceOn(ctx, ex, "id", id)
	if err != nil {
		return err
	}
	if err := checkResourceRevision(expectedRV, ns.ID, ns.ResourceVersion); err != nil {
		return err
	}
	if err := deleteNamespaceOn(ctx, ex, id, force); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteNamespaceOn(ctx context.Context, ex queryExec, id string, force bool) error {
	var total int
	if err := ex.queryRowContext(ctx, "SELECT count(*) FROM namespaces").Scan(&total); err != nil {
		return fmt.Errorf("count namespaces: %w", err)
	}
	if total <= 1 {
		return ErrLastNamespace
	}
	blocking, err := namespaceBlockingObjectsOn(ctx, ex, id)
	if err != nil {
		return err
	}
	if len(blocking) > 0 && !force {
		return &NamespaceBlockedError{Blocking: blocking}
	}
	// All guards run BEFORE the first delete in the same UID/CAS transaction.
	if force {
		var unreaped, occupied int
		if err := ex.queryRowContext(ctx, `SELECT COUNT(*) FROM task_attempts a JOIN tasks t ON t.id=a.task_id WHERE t.namespace_id=? AND a.reaped_at IS NULL`, id).Scan(&unreaped); err != nil {
			return fmt.Errorf("namespace unreaped guard: %w", err)
		}
		// Any busy binding to a task in this namespace is physical/unknown occupancy,
		// including reservation, terminal, offline, or mismatched historical ledger.
		if err := ex.queryRowContext(ctx, `SELECT COUNT(*) FROM node_instances i JOIN tasks t ON t.id=i.busy_task WHERE t.namespace_id=?`, id).Scan(&occupied); err != nil {
			return fmt.Errorf("namespace occupancy guard: %w", err)
		}
		if unreaped > 0 || occupied > 0 {
			return &NamespaceBlockedError{Blocking: []string{fmt.Sprintf("unreaped executions=%d, busy/unknown namespace ledger=%d; cancel/abandon and wait for reliable process-group reap before force", unreaped, occupied)}}
		}
	}
	if err := namespaceExternalReferencesOn(ctx, ex, id); err != nil {
		return err
	}
	stmts := []string{
		`DELETE FROM emits WHERE task_id IN (SELECT id FROM tasks WHERE namespace_id = ?)`,
		`DELETE FROM tasks WHERE namespace_id = ?`,
		`DELETE FROM playbook_installs WHERE namespace_id = ?`,
		`DELETE FROM playbooks WHERE namespace_id = ?`,
		`DELETE FROM event_deliveries WHERE namespace_id = ?`,
		`DELETE FROM events WHERE namespace_id = ?`,
		`DELETE FROM edges WHERE namespace_id = ?`,
		`DELETE FROM sources WHERE namespace_id = ?`,
		`DELETE FROM deployments WHERE namespace_id = ?`,
		`DELETE FROM capability_versions WHERE namespace_id = ?`,
		`DELETE FROM namespace_configs WHERE namespace_id = ?`,
		`DELETE FROM namespaces WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := ex.execContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete namespace %s: %w", id, err)
		}
	}
	return nil
}

// namespaceExternalReferencesOn prevents explicit namespace clearing from
// cascading through owners outside this namespace. Never compare names/digests.
func namespaceExternalReferencesOn(ctx context.Context, ex queryExec, id string) error {
	probes := []string{
		`SELECT COUNT(*) FROM tasks t JOIN capability_versions c ON c.id=t.capability_id WHERE c.namespace_id=? AND t.namespace_id<>c.namespace_id`,
		`SELECT COUNT(*) FROM task_attempts a JOIN tasks t ON t.id=a.task_id JOIN capability_versions c ON c.id=a.capability_id WHERE c.namespace_id=? AND t.namespace_id<>c.namespace_id`,
		`SELECT COUNT(*) FROM task_artifact_manifests m JOIN capability_versions c ON c.id=m.capability_id WHERE c.namespace_id=? AND m.namespace_id<>c.namespace_id`,
		`SELECT COUNT(*) FROM deployments d JOIN capability_versions c ON c.id=d.capability_id WHERE c.namespace_id=? AND d.namespace_id<>c.namespace_id`,
		`SELECT COUNT(*) FROM playbook_installs p JOIN capability_versions c ON EXISTS(SELECT 1 FROM json_tree(p.receipt) j WHERE j.type='text' AND j.value=c.id) WHERE c.namespace_id=? AND p.namespace_id<>c.namespace_id`,
		`SELECT COUNT(*) FROM capability_versions c JOIN playbook_installs p ON p.id=c.installed_from WHERE p.namespace_id=? AND c.namespace_id<>p.namespace_id`,
		`SELECT COUNT(*) FROM sources s JOIN playbook_installs p ON p.id=s.installed_from WHERE p.namespace_id=? AND s.namespace_id<>p.namespace_id`,
		`SELECT COUNT(*) FROM tasks t JOIN playbook_installs p ON p.id=t.trace_id WHERE p.namespace_id=? AND t.namespace_id<>p.namespace_id`,
		`SELECT COUNT(*) FROM events e JOIN playbook_installs p ON p.id=e.trace_id WHERE p.namespace_id=? AND e.namespace_id<>p.namespace_id`,
		`SELECT COUNT(*) FROM tasks child JOIN tasks parent ON parent.id=child.parent_task_id WHERE parent.namespace_id=? AND child.namespace_id<>parent.namespace_id`,
	}
	for _, q := range probes {
		var n int
		if err := ex.queryRowContext(ctx, q, id).Scan(&n); err != nil {
			return fmt.Errorf("namespace reference guard: %w", err)
		}
		if n > 0 {
			return &NamespaceBlockedError{Blocking: []string{"retained references owned by another namespace; force cannot destroy them"}}
		}
	}
	return nil
}

func namespaceBlockingObjectsOn(ctx context.Context, ex queryExec, namespaceID string) ([]string, error) {
	probes := []struct {
		label string
		query string
	}{
		{"capabilities", `SELECT count(*) FROM capability_versions WHERE namespace_id = ?`},
		{"sources", `SELECT count(*) FROM sources WHERE namespace_id = ?`},
		{"playbooks", `SELECT count(*) FROM playbooks WHERE namespace_id = ?`},
		{"tasks", `SELECT count(*) FROM tasks WHERE namespace_id = ?`},
		{"events", `SELECT count(*) FROM events WHERE namespace_id = ?`},
		{"deployments", `SELECT count(*) FROM deployments WHERE namespace_id = ?`},
	}
	out := []string{}
	for _, p := range probes {
		var n int
		if err := ex.queryRowContext(ctx, p.query, namespaceID).Scan(&n); err != nil {
			return nil, fmt.Errorf("probe %s in namespace: %w", p.label, err)
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%s=%d", p.label, n))
		}
	}
	return out, nil
}

// namespaceSlugWithSuffix 拼出 `base-<n>`，并按 namespaceSlugPattern 的 31 字符上限截断 base。
func namespaceSlugWithSuffix(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	if room := maxNamespaceSlugLen - len(suffix); len(base) > room {
		base = base[:room]
	}
	return base + suffix
}

// uniqueNamespaceSlugTx 返回一个**全局未占用**的 slug：base，或 base-2、base-3…
//
// slug 是全局唯一的（UNIQUE(slug)），而注册时会固定取 `default` 作基名
// —— 第二个用户起就必须退让成 `default-2`。
func uniqueNamespaceSlugTx(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	for n := 1; n < 10000; n++ {
		cand := base
		if n > 1 {
			cand = namespaceSlugWithSuffix(base, n)
		}
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM namespaces WHERE slug = ?`, cand).Scan(&taken); err != nil {
			return "", fmt.Errorf("probe namespace slug %q: %w", cand, err)
		}
		if taken == 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free namespace slug left for %q", base)
}

// createDefaultNamespaceTx 在调用方事务内建一个 `default*` 命名空间（**不播种**，
// 播种由调用方决定顺序：注册要求「用户 + 命名空间 + 内置能力」全在一个事务里）。
//
// slug 全局唯一后多了一个竞争点：两个并发注册都可能看到 `default` 空位，
// 后写者会撞 UNIQUE(slug)。撞了就**重算一次 slug**（`default-2`、`default-3`…），
// 不把裸的约束错误抛给调用方（那会变成一个莫名其妙的 500）。
// 重试在同一事务内是安全的：SQLite 的唯一约束失败只回滚那一条语句（ABORT），
// 事务仍可用 —— 与 InstallPlaybook 里「唯一冲突不是真错误」同一处理方式。
func createDefaultNamespaceTx(ctx context.Context, tx *sql.Tx) (*Namespace, error) {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		slug, err := uniqueNamespaceSlugTx(ctx, tx, DefaultNamespaceSlug)
		if err != nil {
			return nil, err
		}
		ns := &Namespace{
			ID:              NewNamespaceID(),
			Slug:            slug,
			Name:            DefaultNamespaceName,
			CreatedAt:       Now(),
			ResourceVersion: 1,
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO namespaces (id, slug, name, created_at) VALUES (?,?,?,?)`,
			ns.ID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
			if isUniqueConstraintError(err) {
				lastErr = err
				continue // 有人抢先占了这个 slug，换一个再来
			}
			return nil, fmt.Errorf("insert default namespace: %w", err)
		}
		return ns, nil
	}
	return nil, fmt.Errorf("insert default namespace: slug still taken after %d attempts: %w", attempts, lastErr)
}
