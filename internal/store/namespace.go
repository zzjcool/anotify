package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 命名空间（namespace）：组织之下的硬隔离边界。
//
// 设计见 docs/design/namespaces.md，契约 §2.1.1 / 不变量 I10、I11。
//
// 一句话：能力 / 节点 / 边 / 源 / 事件 / 任务 / 图快照**全部**属于某个命名空间，
// 组织级只剩身份（users / org_members）与密钥（api_keys）。
//
// 为什么 namespace_id 与 org_id 同时存在（I10）：
//   - `org_id` 是跨命名空间粗过滤与统计的锚点；
//   - `namespace_id` 才是**读路径**的隔离维度。
//     两者是包含关系：`namespace.org_id` 就是行上的 `org_id`。写入时由 store 层
//     从 `namespaces` 行派生 org_id（见 NamespaceOrgID），所以不可能写出不一致的行。
const (
	// DefaultNamespaceSlug 是每个组织自带的命名空间（建组织时同事务创建）。
	DefaultNamespaceSlug = "default"
	// DefaultNamespaceName 是默认命名空间的展示名。
	DefaultNamespaceName = "默认"
	// AllNamespaces 是显式跨命名空间汇总的标记（管理员视图）。
	// **只用于读**，且只在 HTTP 层解释 —— store 层不接受它。
	AllNamespaces = "*"
)

var (
	// ErrNamespaceRequired：命名空间资源的读写没带 namespace_id。
	// HTTP 层必须把它翻译成 400（**绝不回退默认命名空间**：回退等于把人写错的地方藏起来）。
	ErrNamespaceRequired = errors.New("store: namespace id required")
	// ErrNamespaceNotFound：命名空间不存在（HTTP 404）。
	ErrNamespaceNotFound = errors.New("store: namespace not found")
	// ErrNamespaceForbidden：命名空间存在但属于别的组织（HTTP 403）。
	ErrNamespaceForbidden = errors.New("store: namespace belongs to another org")
	// ErrNamespaceExists：同组织里 slug 已存在（HTTP 409）。
	ErrNamespaceExists = errors.New("store: namespace slug already exists")
	// ErrLastNamespace：不允许删最后一个命名空间（删了就没有「这套流程属于哪里」了）。
	ErrLastNamespace = errors.New("store: cannot delete the last namespace of an org")
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
	ID        string `json:"id"`
	OrgID     string `json:"orgId"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	// 计数（列表用；单取时为 0 也无所谓）
	CapabilityCount int `json:"capabilityCount"`
	NodeCount       int `json:"nodeCount"`
	SourceCount     int `json:"sourceCount"`
	EdgeCount       int `json:"edgeCount"`
}

// nsQueryer 让同一段 SQL 既能跑在 *DB 上，也能跑在事务里（建组织 / 建命名空间
// 必须与播种内置能力同事务）。
type nsQueryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// namespaceSlugPattern 是 slug 的合法字符集：它出现在 URL 与令牌作用域里，
// 只允许小写字母、数字、连字符，且以字母或数字开头。
var namespaceSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

const nsColumns = `id, org_id, slug, name, created_at`

// NamespaceOrgID 返回命名空间所属组织（I10 的唯一真相来源）。
//
// 命名空间资源的 store 方法都通过它派生 org_id：调用方给错 org 也写不出
// `namespace.org_id != org_id` 的行。
func (d *DB) NamespaceOrgID(ctx context.Context, namespaceID string) (string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return "", ErrNamespaceRequired
	}
	var orgID string
	err := d.QueryRowContext(ctx, `SELECT org_id FROM namespaces WHERE id = ?`, namespaceID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup namespace org: %w", err)
	}
	return orgID, nil
}

// namespaceOrgIDTx 是 NamespaceOrgID 的事务版（播种 / 建组织路径用）。
func namespaceOrgIDTx(ctx context.Context, tx *sql.Tx, namespaceID string) (string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return "", ErrNamespaceRequired
	}
	var orgID string
	err := tx.QueryRowContext(ctx, `SELECT org_id FROM namespaces WHERE id = ?`, namespaceID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup namespace org: %w", err)
	}
	return orgID, nil
}

// DefaultNamespaceID 返回组织的默认命名空间 id。
//
// 用途：**迁移与后台任务**（老数据回填、全局巡检），不是 HTTP 请求路径。
// HTTP 端点缺 namespaceId 一律 400，绝不回退到这里（契约 §6.2）。
func (d *DB) DefaultNamespaceID(ctx context.Context, orgID string) (string, error) {
	if orgID == "" {
		return "", ErrNamespaceRequired
	}
	var id string
	err := d.QueryRowContext(ctx,
		`SELECT id FROM namespaces WHERE org_id = ? ORDER BY created_at, id LIMIT 1`, orgID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup default namespace: %w", err)
	}
	return id, nil
}

// GetNamespace 按 (org, id) 取命名空间，区分三态：不存在 404 / 别的组织 403 / 命中。
func (d *DB) GetNamespace(ctx context.Context, orgID, id string) (*Namespace, error) {
	var ns Namespace
	err := d.QueryRowContext(ctx,
		`SELECT `+nsColumns+` FROM namespaces WHERE id = ?`, id).
		Scan(&ns.ID, &ns.OrgID, &ns.Slug, &ns.Name, &ns.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNamespaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}
	if ns.OrgID != orgID {
		return nil, ErrNamespaceForbidden
	}
	return &ns, nil
}

// ListNamespaces 列出组织的命名空间，带各类资源计数（UI 列表直接用）。
// 空列表返回 []（不变量 I9）。
func (d *DB) ListNamespaces(ctx context.Context, orgID string) ([]*Namespace, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT n.id, n.org_id, n.slug, n.name, n.created_at,
		        (SELECT count(*) FROM capability_versions c WHERE c.namespace_id = n.id),
		        (SELECT count(*) FROM nodes nd WHERE nd.namespace_id = n.id),
		        (SELECT count(*) FROM sources s WHERE s.namespace_id = n.id),
		        (SELECT count(*) FROM edges e WHERE e.namespace_id = n.id)
		   FROM namespaces n
		  WHERE n.org_id = ?
		  ORDER BY n.created_at, n.id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()
	out := []*Namespace{}
	for rows.Next() {
		var ns Namespace
		if err := rows.Scan(&ns.ID, &ns.OrgID, &ns.Slug, &ns.Name, &ns.CreatedAt,
			&ns.CapabilityCount, &ns.NodeCount, &ns.SourceCount, &ns.EdgeCount); err != nil {
			return nil, fmt.Errorf("scan namespace: %w", err)
		}
		out = append(out, &ns)
	}
	return out, rows.Err()
}

// CreateNamespace 建命名空间，并**在同一事务内**播种内置能力。
//
// 为什么必须同事务：新命名空间里没有 notify.phone 的话，连一个 end 节点都
// 声明不出来，推送直接失效；而"稍后补播种"在失败时没有任何人会发现。
func (d *DB) CreateNamespace(ctx context.Context, orgID, slug, name string) (*Namespace, error) {
	if orgID == "" {
		return nil, fmt.Errorf("create namespace: empty org_id")
	}
	slug = strings.TrimSpace(slug)
	if !namespaceSlugPattern.MatchString(slug) {
		return nil, fmt.Errorf("create namespace: invalid slug %q (只允许小写字母、数字、连字符，且以字母或数字开头)", slug)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}
	if err := d.ensureOrgExists(ctx, orgID); err != nil {
		return nil, err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create namespace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ns := &Namespace{ID: NewNamespaceID(), OrgID: orgID, Slug: slug, Name: name, CreatedAt: Now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces (id, org_id, slug, name, created_at) VALUES (?,?,?,?,?)`,
		ns.ID, ns.OrgID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %q", ErrNamespaceExists, slug)
		}
		return nil, fmt.Errorf("insert namespace: %w", err)
	}
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, ns.ID, orgID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create namespace: %w", err)
	}
	return ns, nil
}

// ensureOrgExists 校验组织存在（建命名空间前）。
func (d *DB) ensureOrgExists(ctx context.Context, orgID string) error {
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM orgs WHERE id = ?`, orgID).Scan(&n); err != nil {
		return fmt.Errorf("probe org: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameNamespace 只改展示名。slug 是稳定标识（出现在 URL 与令牌作用域里），不可改。
func (d *DB) RenameNamespace(ctx context.Context, orgID, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("rename namespace: empty name")
	}
	if _, err := d.GetNamespace(ctx, orgID, id); err != nil {
		return err
	}
	res, err := d.ExecContext(ctx,
		`UPDATE namespaces SET name = ? WHERE id = ? AND org_id = ?`, name, id, orgID)
	if err != nil {
		return fmt.Errorf("rename namespace: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rename namespace rows: %w", err)
	}
	if n == 0 {
		return ErrNamespaceNotFound
	}
	return nil
}

// DeleteNamespace 删命名空间。
//
// 两档：
//   - 默认：里面还有 sources / edges / tasks 时返回 *NamespaceBlockedError（HTTP 409），
//     把这些对象列出来——删命名空间 = 里面的机器全部掉线，不能当"清理配置"做。
//   - force=true：显式级联删除，连机器、能力、事件、任务、图快照一起删。
//
// **不允许删最后一个命名空间**：删了之后这个组织就没有"这套流程属于哪里"了，
// 而代码里不存在「没有命名空间」的分支（契约 §2.1.1）。
func (d *DB) DeleteNamespace(ctx context.Context, orgID, id string, force bool) error {
	if _, err := d.GetNamespace(ctx, orgID, id); err != nil {
		return err
	}
	var total int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM namespaces WHERE org_id = ?`, orgID).Scan(&total); err != nil {
		return fmt.Errorf("count namespaces: %w", err)
	}
	if total <= 1 {
		return ErrLastNamespace
	}

	blocking, err := d.namespaceBlockingObjects(ctx, id)
	if err != nil {
		return err
	}
	if len(blocking) > 0 && !force {
		return &NamespaceBlockedError{Blocking: blocking}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete namespace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 显式按依赖顺序删，而不是依赖 FK 级联的顺序：
	// tasks 对 capability_versions 有**不带级联**的外键，级联触发顺序若先删能力
	// 就会撞上还存在的 tasks 行。显式删除让顺序确定。
	stmts := []string{
		`DELETE FROM tasks WHERE namespace_id = ?`,
		`DELETE FROM events WHERE namespace_id = ?`,
		`DELETE FROM node_pins WHERE namespace_id = ?`,
		`DELETE FROM node_instances WHERE namespace_id = ?`,
		`DELETE FROM node_capabilities WHERE namespace_id = ?`,
		`DELETE FROM edges WHERE namespace_id = ?`,
		`DELETE FROM source_targets WHERE namespace_id = ?`,
		`DELETE FROM sources WHERE namespace_id = ?`,
		`DELETE FROM graph_versions WHERE namespace_id = ?`,
		`DELETE FROM nodes WHERE namespace_id = ?`,
		`DELETE FROM capability_versions WHERE namespace_id = ?`,
		`DELETE FROM namespaces WHERE id = ? AND org_id = ?`,
	}
	args := []any{id}
	for i, q := range stmts {
		a := args
		if i == len(stmts)-1 {
			a = []any{id, orgID}
		}
		if _, err := tx.ExecContext(ctx, q, a...); err != nil {
			return fmt.Errorf("delete namespace %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete namespace: %w", err)
	}
	return nil
}

// namespaceBlockingObjects 列出阻塞删除的对象（用于 409 的可读清单）。
func (d *DB) namespaceBlockingObjects(ctx context.Context, namespaceID string) ([]string, error) {
	probes := []struct {
		label string
		query string
	}{
		{"sources", `SELECT count(*) FROM sources WHERE namespace_id = ?`},
		{"edges", `SELECT count(*) FROM edges WHERE namespace_id = ?`},
		{"tasks", `SELECT count(*) FROM tasks WHERE namespace_id = ?`},
		{"nodes", `SELECT count(*) FROM nodes WHERE namespace_id = ?`},
	}
	out := []string{}
	for _, p := range probes {
		var n int
		if err := d.QueryRowContext(ctx, p.query, namespaceID).Scan(&n); err != nil {
			return nil, fmt.Errorf("probe %s in namespace: %w", p.label, err)
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%s=%d", p.label, n))
		}
	}
	return out, nil
}

// ensureDefaultNamespacesTx 给每个还没有命名空间的组织建一个 `default`（迁移 12 用）。
//
// 排序后逐个处理：老数据全部回填到这个默认命名空间，所以它必须是**最早**的那一行
// （`ORDER BY created_at, id LIMIT 1` 的取法见 defaultNSExpr）。
func ensureDefaultNamespacesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id FROM orgs ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("list orgs for default namespace: %w", err)
	}
	var orgIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan org id: %w", err)
		}
		orgIDs = append(orgIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate orgs: %w", err)
	}
	rows.Close()

	for _, orgID := range orgIDs {
		if _, err := ensureDefaultNamespaceTx(tx, orgID); err != nil {
			return err
		}
	}
	return nil
}

// ensureDefaultNamespaceTx 幂等地保证组织有默认命名空间，返回它。
func ensureDefaultNamespaceTx(tx *sql.Tx, orgID string) (*Namespace, error) {
	var ns Namespace
	err := tx.QueryRow(
		`SELECT `+nsColumns+` FROM namespaces WHERE org_id = ? ORDER BY created_at, id LIMIT 1`, orgID).
		Scan(&ns.ID, &ns.OrgID, &ns.Slug, &ns.Name, &ns.CreatedAt)
	if err == nil {
		return &ns, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe default namespace: %w", err)
	}
	ns = Namespace{
		ID:        NewNamespaceID(),
		OrgID:     orgID,
		Slug:      DefaultNamespaceSlug,
		Name:      DefaultNamespaceName,
		CreatedAt: Now(),
	}
	if _, err := tx.Exec(
		`INSERT INTO namespaces (id, org_id, slug, name, created_at) VALUES (?,?,?,?,?)`,
		ns.ID, ns.OrgID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert default namespace: %w", err)
	}
	return &ns, nil
}

// CreateDefaultNamespaceTx 在调用方事务内建 `default` 命名空间 + 播种内置能力
// （建组织时用：组织与它的第一个命名空间必须原子）。
func CreateDefaultNamespaceTx(ctx context.Context, tx *sql.Tx, orgID string) (*Namespace, error) {
	ns, err := ensureDefaultNamespaceTx(tx, orgID)
	if err != nil {
		return nil, err
	}
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, ns.ID, orgID); err != nil {
		return nil, err
	}
	return ns, nil
}
