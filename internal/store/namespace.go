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
// 设计见 docs/design/namespaces.md，契约 §2.1.1 / 不变量 I10、I11。
//
// 一句话：能力 / 节点 / 边 / 源 / 事件 / 任务 / 图快照**全部**属于某个命名空间。
// 组织（org）这一层已经删除（迁移 14）：一套部署 = 一个团队，`namespace_id`
// 是行上唯一的隔离维度，没有比它更粗的过滤锚点了。
const (
	// DefaultNamespaceSlug 是注册时给新命名空间取的 slug 基名。
	// 全局唯一（迁移 14 起 UNIQUE(slug)），所以第二个及以后的用户拿到
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
	// ErrNamespaceExists：slug 已被占用（HTTP 409）。迁移 14 起唯一域是全局。
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
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
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

const nsColumns = `id, slug, name, created_at`

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
	err := d.QueryRowContext(ctx,
		`SELECT `+nsColumns+` FROM namespaces WHERE id = ?`, id).
		Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.CreatedAt)
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
	rows, err := d.QueryContext(ctx,
		`SELECT n.id, n.slug, n.name, n.created_at,
		        (SELECT count(*) FROM capability_versions c WHERE c.namespace_id = n.id),
		        (SELECT count(*) FROM nodes nd WHERE nd.namespace_id = n.id),
		        (SELECT count(*) FROM sources s WHERE s.namespace_id = n.id),
		        (SELECT count(*) FROM edges e WHERE e.namespace_id = n.id)
		   FROM namespaces n
		  ORDER BY n.created_at, n.rowid`)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()
	out := []*Namespace{}
	for rows.Next() {
		var ns Namespace
		if err := rows.Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.CreatedAt,
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
//
// slug 全局唯一（迁移 14）：已占用返回 ErrNamespaceExists（HTTP 409）。
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

	ns := &Namespace{ID: NewNamespaceID(), Slug: slug, Name: name, CreatedAt: Now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces (id, slug, name, created_at) VALUES (?,?,?,?)`,
		ns.ID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %q", ErrNamespaceExists, slug)
		}
		return nil, fmt.Errorf("insert namespace: %w", err)
	}
	if err := SeedBuiltinCapabilitiesTx(ctx, tx, ns.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create namespace: %w", err)
	}
	return ns, nil
}

// RenameNamespace 只改展示名。slug 是稳定标识（出现在 URL 与令牌作用域里），不可改。
func (d *DB) RenameNamespace(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("rename namespace: empty name")
	}
	if _, err := d.GetNamespace(ctx, id); err != nil {
		return err
	}
	res, err := d.ExecContext(ctx,
		`UPDATE namespaces SET name = ? WHERE id = ?`, name, id)
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
//   - 默认：里面还有 sources / edges / tasks / nodes 时返回 *NamespaceBlockedError（HTTP 409），
//     把这些对象列出来——删命名空间 = 里面的机器全部掉线，不能当"清理配置"做。
//   - force=true：显式级联删除，连机器、能力、事件、任务、图快照一起删。
//
// **不允许删最后一个命名空间**：删了之后这套部署就没有"这套流程属于哪里"了，
// 而代码里不存在「没有命名空间」的分支（契约 §2.1.1）。
func (d *DB) DeleteNamespace(ctx context.Context, id string, force bool) error {
	if _, err := d.GetNamespace(ctx, id); err != nil {
		return err
	}
	var total int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM namespaces`).Scan(&total); err != nil {
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
		`DELETE FROM playbooks WHERE namespace_id = ?`,
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
		`DELETE FROM namespaces WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
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

// uniqueNamespaceSlugTx 返回一个**全局未占用**的 slug：base，或 base-2、base-3…
//
// 迁移 14 之后 slug 是全局唯一的（UNIQUE(slug)），而注册时会固定取
// `default` 作基名 —— 第二个用户起就必须退让成 `default-2`。
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
func createDefaultNamespaceTx(ctx context.Context, tx *sql.Tx) (*Namespace, error) {
	slug, err := uniqueNamespaceSlugTx(ctx, tx, DefaultNamespaceSlug)
	if err != nil {
		return nil, err
	}
	ns := &Namespace{
		ID:        NewNamespaceID(),
		Slug:      slug,
		Name:      DefaultNamespaceName,
		CreatedAt: Now(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces (id, slug, name, created_at) VALUES (?,?,?,?)`,
		ns.ID, ns.Slug, ns.Name, ns.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert default namespace: %w", err)
	}
	return ns, nil
}
