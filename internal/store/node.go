package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 节点状态（契约 §2.3 nodes.status）。
const (
	NodeStatusOnline   = "online"
	NodeStatusOffline  = "offline"
	NodeStatusDraining = "draining"
)

// 节点默认硬顶（契约 §2.3）。
const (
	DefaultMaxInstances   = 20
	DefaultMaxConcurrency = 8
)

// NodeNameMaxLen 是节点名的长度上限（1..63，与 slug 同口径的字符集）。
const NodeNameMaxLen = 63

// Node 是 nodes 表的一行（一台机器）。
//
// TokenHash 是 sha256(token)，明文只在创建时返回一次（契约 §10.3）；
// 所以它带 `json:"-"`，任何 API 响应都不会泄漏哈希。
type Node struct {
	ID string `json:"id"`
	// NamespaceID 是**隔离维度**（I10/I11）：机器属于且只属于一个命名空间，
	// 令牌、能力声明、槽位、粘性钉住都跟着它走；org_id 只是跨命名空间统计的锚点。
	NamespaceID    string   `json:"namespaceId"`
	OrgID          string   `json:"orgId"`
	Name           string   `json:"name"`
	Tags           []string `json:"tags"`
	Status         string   `json:"status"`
	TokenHash      string   `json:"-"`
	MaxInstances   int      `json:"maxInstances"`
	MaxConcurrency int      `json:"maxConcurrency"`
	LastSeenAt     *int64   `json:"lastSeenAt"`
	CreatedAt      int64    `json:"createdAt"`
}

// NodeInstance 是 node_instances 表的一行（并发槽位，由节点申报）。
//
// NamespaceID 从父行（nodes）取，不接受调用方传（契约 §2.3）。
type NodeInstance struct {
	ID string `json:"id"`
	// NamespaceID 是槽位所属命名空间（隔离维度，I10）。
	NamespaceID string  `json:"namespaceId"`
	NodeID      string  `json:"nodeId"`
	Slot        int     `json:"slot"`
	Concurrency int     `json:"concurrency"`
	BusyTask    *string `json:"busyTask"`
	Alive       bool    `json:"alive"`
	UpdatedAt   int64   `json:"updatedAt"`
}

// NodePin 是 node_pins 表的一行（粘性路由）。
//
// StickyKey 全局唯一（sha256 摘要），所以查/删按 key 定位即可；但行上仍要带
// NamespaceID —— 钉住的是「某个命名空间里的某台机器」。
type NodePin struct {
	StickyKey string `json:"stickyKey"`
	// NamespaceID 必填，org_id 由它派生（I10）。
	NamespaceID string `json:"namespaceId"`
	OrgID       string `json:"orgId"`
	NodeID      string `json:"nodeId"`
	Selector    string `json:"selector"`
	CreatedAt   int64  `json:"createdAt"`
	LastUsedAt  int64  `json:"lastUsedAt"`
}

// 节点注册 / 派发相关的哨兵错误。
var (
	// ErrUnknownCapability 节点声明了中枢不存在的能力名@版本（不变量 I2 / A3）。
	ErrUnknownCapability = errors.New("unknown_capability")
	// ErrTooManyInstances 申报实例数超过 nodes.max_instances 硬顶（契约 Q5）。
	ErrTooManyInstances = errors.New("too_many_instances")
	// ErrNoCapacity 节点没有空闲并发槽位。
	ErrNoCapacity = errors.New("no_capacity")
	// ErrNodeDraining 节点在排空（不再接新任务）。
	ErrNodeDraining = errors.New("node_draining")
	// ErrNodeExists 同命名空间内节点重名（HTTP 409）。
	//
	// 由表约束 UNIQUE(namespace_id, name) 触发，**不是**预查——预查会给并发注册
	// 留下 TOCTOU 窗口：两台同名的机器都能查空，然后一台插入失败变成 500。
	ErrNodeExists = errors.New("store: node name already exists")
)

// CapabilityRef 是节点声明自己具备的能力（只有 name + version，没有命令）。
//
// 这是 hello 帧 capabilities 字段的语义（老路径）：中枢只知道「它有哪些能力」，
// 不知道「它怎么跑」。带 manifest 的节点用 CapabilityDecl（多一个 Run）。
type CapabilityRef struct {
	Name    string `json:"capability"`
	Version int    `json:"version"`
}

// CapabilityDecl 是节点声明的一条能力：名字 + 版本 + 可选的执行方式。
//
// Run == nil 表示这一条**没有声明执行方式**（老 agent 的 capabilities 字段，
// 或 manifest 里没写这一项）→ run_spec 落 '{}'。
// Run != nil 表示节点声明了它打算怎么跑（run 的 JSON 原文）——中枢只校验形状、
// 只存不执行（I1 边界，见 docs/design/playbooks.md §4.2）。
type CapabilityDecl struct {
	Name    string
	Version int
	Run     json.RawMessage
	// RunRequired 表示这一条来自 manifest：按契约 run 必填（playbooks.md §4.1），
	// 缺失即配置错误，不能隐式默认成 bash。
	RunRequired bool
}

// NodeCapability 是「某节点声明了这个能力」的一行：能力定义 + 该节点上报的 run_spec。
//
// 嵌入 *CapabilityVersion 是为了不动既有调用点的 .Name/.Outputs 等字段访问
// （派发选点、控制台都只关心能力定义）。
type NodeCapability struct {
	*CapabilityVersion
	// RunSpec 是 run 的 JSON 原文；'{}' = 声明了能力但没声明执行方式。
	RunSpec json.RawMessage `json:"runSpec"`
}

// emptyRunSpec 是「声明了能力但没声明执行方式」的落库值。
const emptyRunSpec = "{}"

// capRunnerBash 是本期唯一支持的执行器类型（playbooks.md §4.1）。
//
// 只是**字符串比较**：中枢不调用它、不拼命令行、不检查这个值指向什么。
const capRunnerBash = "bash"

// CapabilityDeclError 是一份声明的**全部**问题（一次列全，不是只报第一条）。
//
// 为什么必须列全、且必须全拒（不留半份声明）：
//  1. 半装状态会让「为什么这台机器没接这个任务」无法解释 —— 排障成本远高于启动即失败；
//  2. 这是**配置错误**，应该在节点启动时就炸掉（fail fast），而不是运行期变成一台安静的空闲机器。
type CapabilityDeclError struct {
	Problems          []string // 每条自描述，顺序确定
	UnknownCapability bool     // 至少一条是"能力未定义/名字为空"→ errors.Is(err, ErrUnknownCapability)
}

// Error 把全部问题拼成一行（; 分隔，顺序确定）。
func (e *CapabilityDeclError) Error() string { return strings.Join(e.Problems, "; ") }

// Unwrap 让 errors.Is(err, ErrUnknownCapability) 对「混有问题项」的错误也成立：
// 上层（握手）据此把错误码定成 unknown_capability 而不是 invalid_manifest。
func (e *CapabilityDeclError) Unwrap() error {
	if e.UnknownCapability {
		return ErrUnknownCapability
	}
	return nil
}

// InstanceSpec 是节点申报的并发槽位（hello 帧的 instances 字段，契约 Q5）。
type InstanceSpec struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// NewNodeToken 生成节点 opaque token：ant_node_<base64url(24B)>（契约 §10.3，
// 与既有 api_keys 形态一致）。返回 (明文, sha256 哈希)。
func NewNodeToken() (token, hash string, err error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generate node token: %w", err)
	}
	token = "ant_node_" + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashNodeToken(token), nil
}

// HashNodeToken 计算 token 的 sha256（DB 只存哈希，明文只在创建时返回一次）。
func HashNodeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// nodeNamePattern 是节点名的合法形状（契约 D6）：1..63 个字符，只允许小写字母、
// 数字、点、下划线、短横线，且必须以字母或数字开头。
var nodeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateNodeName 校验节点名："" 表示合法，否则是给用户看的中文错误（含 "name"）。
//
// 规则严格按 nodeNamePattern（契约 D6），**不 trim**：`" mbp-16"` 这种带空白的输入
// 必须当场拒绝，而不是静默改对后落库——名字是运维用来认机器的主键，改一个字符
// 就可能变成另一台机器的名字。
//
// 与 ValidateVarDefs（internal/store/source.go）遵循同一约定（"" = 合法）。
// 注意：CreateNode **不**调用它——devseed 与老测试直接走 store 层，行为保持原样；
// 唯一调用点是 HTTP 写端点（POST /v1/nodes）。
func ValidateNodeName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "name 必填（节点名不能为空或全为空白字符）"
	}
	if len(name) > NodeNameMaxLen {
		return fmt.Sprintf("name 长度不能超过 %d 个字符（当前 %d 个）", NodeNameMaxLen, len(name))
	}
	if !nodeNamePattern.MatchString(name) {
		return fmt.Sprintf("name 「%s」不合法：只能包含小写字母、数字、点（.）、下划线（_）、"+
			"短横线（-），长度 1..%d，且必须以字母或数字开头", name, NodeNameMaxLen)
	}
	return ""
}

// CreateNode 创建节点，返回节点与**只出现一次**的明文 token。
//
// n.TokenHash 可预先由调用方设置（用于测试注入已知 token）；为空时自动生成。
//
// n.NamespaceID 必填（I10）：机器归属哪个命名空间，决定了它的令牌、能力声明、
// 槽位与粘性钉住活在哪片隔离域里。n.OrgID 由命名空间派生并**覆盖**——
// 调用方传错也写不出 `namespace.org_id != org_id` 的行（契约 §2.3）。
func (d *DB) CreateNode(ctx context.Context, n *Node) (token string, err error) {
	if n == nil {
		return "", fmt.Errorf("create node: nil")
	}
	if n.NamespaceID == "" || n.NamespaceID == AllNamespaces {
		return "", ErrNamespaceRequired
	}
	if n.Name == "" {
		return "", fmt.Errorf("create node: empty name")
	}
	if n.TokenHash == "" {
		token, n.TokenHash, err = NewNodeToken()
		if err != nil {
			return "", err
		}
	}
	if n.MaxInstances <= 0 {
		n.MaxInstances = DefaultMaxInstances
	}
	if n.MaxConcurrency <= 0 {
		n.MaxConcurrency = DefaultMaxConcurrency
	}
	if n.Status == "" {
		n.Status = NodeStatusOffline
	}
	if n.CreatedAt == 0 {
		n.CreatedAt = Now()
	}
	if n.Tags == nil {
		n.Tags = []string{}
	}
	tagsJSON, err := json.Marshal(n.Tags)
	if err != nil {
		return "", fmt.Errorf("marshal node tags: %w", err)
	}
	if n.ID == "" {
		n.ID = NewNodeID()
	}
	// org_id 从命名空间派生（I10：`namespace.org_id == org_id` 由构造保证，
	// 不是由调用方自觉遵守）。
	orgID, err := d.NamespaceOrgID(ctx, n.NamespaceID)
	if err != nil {
		return "", fmt.Errorf("create node: %w", err)
	}
	n.OrgID = orgID
	if _, err := d.ExecContext(ctx,
		`INSERT INTO nodes
		   (id, namespace_id, org_id, name, tags, status, token_hash, max_instances, max_concurrency, last_seen_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.NamespaceID, n.OrgID, n.Name, string(tagsJSON), n.Status, n.TokenHash,
		n.MaxInstances, n.MaxConcurrency, n.LastSeenAt, n.CreatedAt); err != nil {
		// 同命名空间重名（UNIQUE(namespace_id, name)）要让调用方能判定成 409，
		// 而不是从一句裸的 constraint failed 里去猜。
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %q", ErrNodeExists, n.Name)
		}
		return "", fmt.Errorf("insert node: %w", err)
	}
	return token, nil
}

// nodeColumns 是统一的列顺序，scanNodeRow 依赖它（与迁移 12 的 nodes 定义同序）。
const nodeColumns = `id, namespace_id, org_id, name, tags, status, token_hash,
	max_instances, max_concurrency, last_seen_at, created_at`

// prefixedNodeColumns 给 nodeColumns 的每一列加表别名前缀，供 join 查询使用。
//
// 为什么必须带前缀：`node_capabilities` 也有 `namespace_id`（#162 加的隔离列），
// 不限定表名的 `namespace_id` 会让 SQLite 报 ambiguous column name。
func prefixedNodeColumns(alias string) string {
	cols := []string{"id", "namespace_id", "org_id", "name", "tags", "status", "token_hash",
		"max_instances", "max_concurrency", "last_seen_at", "created_at"}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// scanNodeRow 从 rows 解析一行。
func scanNodeRow(rows *sql.Rows) (*Node, error) {
	var n Node
	var tags string
	if err := rows.Scan(&n.ID, &n.NamespaceID, &n.OrgID, &n.Name, &tags, &n.Status, &n.TokenHash,
		&n.MaxInstances, &n.MaxConcurrency, &n.LastSeenAt, &n.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &n.Tags); err != nil {
		return nil, fmt.Errorf("decode node tags: %w", err)
	}
	if n.Tags == nil {
		n.Tags = []string{} // 不变量 I9
	}
	return &n, nil
}

// GetNodeByTokenHash 按 token sha256 取节点（握手鉴权用，契约 §10.3）。
//
// 这是唯一一个「还不知道命名空间也能查」的节点查询：token 自带身份。
// 拿到的行**必须**带 NamespaceID —— 握手后整条连接上的所有操作都靠它定位
// 命名空间（hello 帧不带 namespace 字段，令牌绑在哪个命名空间就在哪片隔离域生效）；
// org_id 只是跨命名空间统计的锚点。token 全局唯一，所以本查询不看命名空间。
func (d *DB) GetNodeByTokenHash(ctx context.Context, tokenHash string) (*Node, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("get node by token hash: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node by token hash: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// GetNode 按 (namespaceID, id) 取节点（不变量 I10：必须带命名空间）。
//
// 节点 id 全局唯一，但读路径仍要过隔离维度：少了它，一个写错的 id 就能
// 把别的命名空间的机器读出来。
func (d *DB) GetNode(ctx context.Context, namespaceID, nodeID string) (*Node, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE namespace_id = ? AND id = ?`, namespaceID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("get node: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// GetNodeByName 按 (namespaceID, name) 取节点；未命中返回 ErrNotFound。
//
// 名字的唯一性也是命名空间内的（UNIQUE(namespace_id, name)）：prod 与 staging
// 可以各有一台同名机器。
func (d *DB) GetNodeByName(ctx context.Context, namespaceID, name string) (*Node, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE namespace_id = ? AND name = ?`, namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("get node by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node by name: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// ListNodes 列出命名空间内节点（空列表返回 []）。
func (d *DB) ListNodes(ctx context.Context, namespaceID string) ([]*Node, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE namespace_id = ? ORDER BY name, id`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	out := []*Node{}
	for rows.Next() {
		n, err := scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateNodeStatus 改节点状态（online/offline/draining），并刷新 last_seen_at。
func (d *DB) UpdateNodeStatus(ctx context.Context, namespaceID, nodeID, status string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	switch status {
	case NodeStatusOnline, NodeStatusOffline, NodeStatusDraining:
	default:
		return fmt.Errorf("update node status: invalid status %q", status)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET status = ?, last_seen_at = ? WHERE namespace_id = ? AND id = ?`,
		status, Now(), namespaceID, nodeID)
	if err != nil {
		return fmt.Errorf("update node status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update node status rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchNode 刷新 last_seen_at（任何上行帧都续命，契约 §3.3）。
func (d *DB) TouchNode(ctx context.Context, namespaceID, nodeID string, at int64) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if at == 0 {
		at = Now()
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE nodes SET last_seen_at = ? WHERE namespace_id = ? AND id = ?`,
		at, namespaceID, nodeID); err != nil {
		return fmt.Errorf("touch node: %w", err)
	}
	return nil
}

// ConfirmCapabilities 校验并写入节点声明的能力（契约 Q5 / 不变量 I2 / A3）。
//
// 声明的 name@version 必须在中枢已定义，否则返回 ErrUnknownCapability；
// 写入 node_capabilities 时只存 (node_id, capability_id, run_spec)——
// 没有命令列、没有出口、没有 kind。
//
// 语义是**全量替换**：节点这次声明什么，就把它之前的声明全换掉。
// （节点升级后不再支持某能力，不该还留在表里被派发选中。）
// 返回解析出的 capability_id 列表（顺序与入参一致）。
//
// 这是**老路径**（hello 不带 manifest）：签名与行为保持不变，run_spec 一律写 '{}'。
// 带 manifest 的节点走 ConfirmCapabilityDecls，两者共用私有实现
// confirmCapabilityDecls（校验规则只有一份）。
func (d *DB) ConfirmCapabilities(ctx context.Context, namespaceID, nodeID string, caps []CapabilityRef) ([]string, error) {
	decls := make([]CapabilityDecl, 0, len(caps))
	for _, ref := range caps {
		decls = append(decls, CapabilityDecl{Name: ref.Name, Version: ref.Version})
	}
	return d.confirmCapabilityDecls(ctx, namespaceID, nodeID, decls)
}

// ConfirmCapabilityDecls 是带 run 声明的注册路径（hello 带 manifest）：
// 逐条校验 run，并把 run_spec 与声明一起全量替换。
//
// 校验全拒 + 列全：任何一条不过就整体拒绝（不写任何行），错误里列出**全部**问题项
// （CapabilityDeclError）。已定义能力项与未定义能力项都会校验 run，
// 目的是让节点一次启动就把配置问题说清楚，而不是修一条重启一次。
//
// 能力的解析限定在**该节点所在命名空间**内（I11③）：在 prod 里声明 staging 的
// review@1 等同于声明一个不存在的能力（ErrUnknownCapability 语义不变）。
func (d *DB) ConfirmCapabilityDecls(ctx context.Context, namespaceID, nodeID string, decls []CapabilityDecl) ([]string, error) {
	return d.confirmCapabilityDecls(ctx, namespaceID, nodeID, decls)
}

// confirmCapabilityDecls 是两个 Confirm 的唯一实现：先校验并收集全部问题，
// 全过才 DELETE + INSERT（全量替换，run_spec 一起写）。
func (d *DB) confirmCapabilityDecls(ctx context.Context, namespaceID, nodeID string, decls []CapabilityDecl) ([]string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if nodeID == "" {
		return nil, fmt.Errorf("confirm capabilities: empty node_id")
	}

	// 节点行是唯一真相（I11③ + 契约 §2.3）：
	//   - 能力定义只在**节点所在命名空间**里按 name@version 解析（不在别处）；
	//   - node_capabilities.namespace_id 从这一行取，不接受调用方传。
	node, err := d.GetNode(ctx, namespaceID, nodeID)
	if err != nil {
		return nil, err
	}

	// run_spec 归一：'' → '{}'（没声明执行方式），其它按 JSON 原文存。
	specs := make([]string, len(decls))
	present := make([]bool, len(decls))
	for i, decl := range decls {
		specs[i], present[i] = runSpecOf(decl.Run)
	}

	// concurrency 越界要对 nodes.max_concurrency：节点行上面已经取过
	// （命名空间隔离与 node_capabilities.namespace_id 都要它），不再多查一次。
	maxConcurrency := node.MaxConcurrency

	ids := make([]string, len(decls))
	var (
		nameProblems []string // 能力名为空：{ref} 渲染不出来，单独一条
		unknownRefs  []string // 中枢没定义的能力：合成一条
		runProblems  []string // 每条声明一行，按声明顺序
		unknown      bool
	)
	for i, decl := range decls {
		if decl.Name == "" {
			nameProblems = append(nameProblems,
				fmt.Sprintf("capability name is empty (version %d)", decl.Version))
			unknown = true
			continue // 名字都没有，{ref} 渲染不出来，run 问题无法自描述
		}
		ref := fmt.Sprintf("%s@%d", decl.Name, decl.Version)
		// I11③：只在**本节点所在命名空间**解析能力定义。
		c, err := d.GetCapability(ctx, node.NamespaceID, decl.Name, decl.Version)
		switch {
		case errors.Is(err, ErrNotFound):
			// 未定义能力（含「只在别的命名空间定义过」）也继续校验 run：一次把配置问题说全
			unknownRefs = append(unknownRefs, ref)
			unknown = true
		case err != nil:
			return nil, err
		case c.NamespaceID != node.NamespaceID:
			// 数据被写坏时才可能走到：宁可 400 说清楚，也不要把别的命名空间的
			// 能力 id 写进本节点的声明（handler 把这错误一律翻成 400）。
			return nil, &CrossNamespaceError{
				What: "node_capabilities.capability_id",
				Want: node.NamespaceID,
				Got:  c.NamespaceID,
			}
		default:
			ids[i] = c.ID
		}
		runProblems = append(runProblems,
			validateCapabilityRun(ref, specs[i], present[i], decl.RunRequired, maxConcurrency)...)
	}

	// 顺序必须确定：先「能力不可解析」组（名字为空 → 未定义组一行），再按声明顺序的 run 问题。
	problems := make([]string, 0, len(nameProblems)+1+len(runProblems))
	problems = append(problems, nameProblems...)
	if len(unknownRefs) > 0 {
		problems = append(problems, "unknown capability: "+strings.Join(unknownRefs, ", "))
	}
	problems = append(problems, runProblems...)
	if len(problems) > 0 {
		return nil, &CapabilityDeclError{Problems: problems, UnknownCapability: unknown}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin confirm capabilities: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_capabilities WHERE node_id = ?`, nodeID); err != nil {
		return nil, fmt.Errorf("clear node capabilities: %w", err)
	}
	for i := range decls {
		// 同一能力重复声明时后者胜（与 hello 合并算法的「后者胜」一致，行为确定）。
		// namespace_id 从 node 行取（契约 §2.3），不是调用方传的。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_capabilities (node_id, capability_id, namespace_id, run_spec) VALUES (?,?,?,?)
			 ON CONFLICT(node_id, capability_id) DO UPDATE SET run_spec = excluded.run_spec`,
			nodeID, ids[i], node.NamespaceID, specs[i]); err != nil {
			return nil, fmt.Errorf("insert node capability: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit confirm capabilities: %w", err)
	}
	return ids, nil
}

// runSpecOf 把声明的 run 归一到落库形态。
//
// 缺失（nil / 空 / null / 全空白）→ '{}' 且 present=false；其它 → JSON 原文且 present=true。
// 存原文而不是重新编码：run 是节点自己的数据，中枢没有理由改写它
// （重新编码会动键序与格式，排障时看不出节点实际写的是什么）。
func runSpecOf(run json.RawMessage) (spec string, present bool) {
	text := strings.TrimSpace(string(run))
	if text == "" || text == "null" {
		return emptyRunSpec, false
	}
	return text, true
}

// validateCapabilityRun 校验一条声明的 run，返回**全部**问题（不是只报第一条）。
//
// 规则集就是契约 §3.4 那张表，一条不多一条少：不判路径是否存在/可执行、
// 不拆参数、不校验 timeout/env 的取值 —— 中枢不解释节点的执行方式（I1 边界）。
func validateCapabilityRun(ref, spec string, present, required bool, maxConcurrency int) []string {
	if !present {
		if required {
			// 来自 manifest 的条目必须显式写 run（playbooks.md §4.1）：
			// 不能隐式默认成 bash，否则老配置的语义要靠猜。
			return []string{ref + ": run is required"}
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(spec), &obj); err != nil || obj == nil {
		return []string{ref + ": run must be a JSON object"}
	}

	problems := make([]string, 0, 3)
	// runner：必须显式写出且只接受 bash（不 trim、不猜意图）
	var runner string
	if raw, ok := obj["runner"]; ok {
		_ = json.Unmarshal(raw, &runner)
	}
	switch runner {
	case "":
		problems = append(problems, ref+": run.runner is required (only bash)")
	case capRunnerBash:
	default:
		problems = append(problems, fmt.Sprintf("%s: run.runner %q is not supported (only bash)", ref, runner))
	}
	// command：非空字符串（只判「有没有写」，不解析内容）
	var command string
	if raw, ok := obj["command"]; ok {
		_ = json.Unmarshal(raw, &command)
	}
	if strings.TrimSpace(command) == "" {
		problems = append(problems, ref+": run.command is empty")
	}
	// concurrency：出现时必须在 [1, nodes.max_concurrency]；
	// 缺省 = 未声明 = 合法（不写默认值）。
	if raw, ok := obj["concurrency"]; ok {
		var num json.Number
		shown := strings.TrimSpace(string(raw))
		inRange := false
		if err := json.Unmarshal(raw, &num); err == nil && num.String() != "" {
			shown = num.String()
			if n, conv := num.Int64(); conv == nil && n >= 1 && n <= int64(maxConcurrency) {
				inRange = true
			}
		}
		if !inRange {
			problems = append(problems, fmt.Sprintf("%s: run.concurrency %s is out of range [1, %d]",
				ref, shown, maxConcurrency))
		}
	}
	return problems
}

// ListNodeCapabilities 返回节点声明的能力定义（含 outputs，供 kind 推导与派发选点）
// 以及该节点为每个能力上报的 run_spec。
//
// RunSpec 只用于展示与诊断（I1 边界）：派发选点走 NodesForCapability，看不到这一列。
// 顺序不变（name, version），空列表返回 []。
func (d *DB) ListNodeCapabilities(ctx context.Context, nodeID string) ([]*NodeCapability, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+prefixedCapabilityColumns("c")+`, nc.run_spec
		 FROM node_capabilities nc
		 JOIN capability_versions c ON c.id = nc.capability_id
		 WHERE nc.node_id = ?
		 ORDER BY c.name, c.version`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node capabilities: %w", err)
	}
	defer rows.Close()
	out := []*NodeCapability{}
	for rows.Next() {
		var c CapabilityVersion
		var paramsSchema, outputs, lifecycle, runSpec string
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.OrgID, &c.Name, &c.Version, &paramsSchema, &outputs,
			&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt, &runSpec); err != nil {
			return nil, fmt.Errorf("scan node capability: %w", err)
		}
		if err := json.Unmarshal([]byte(paramsSchema), &c.ParamsSchema); err != nil {
			return nil, fmt.Errorf("decode params_schema: %w", err)
		}
		if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
			return nil, fmt.Errorf("decode outputs: %w", err)
		}
		if c.Outputs == nil {
			c.Outputs = []string{}
		}
		if err := json.Unmarshal([]byte(lifecycle), &c.Lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle: %w", err)
		}
		if strings.TrimSpace(runSpec) == "" {
			runSpec = emptyRunSpec // 防御：手工改库/老数据不该让页面看到空串
		}
		out = append(out, &NodeCapability{CapabilityVersion: &c, RunSpec: json.RawMessage(runSpec)})
	}
	return out, rows.Err()
}

// prefixedCapabilityColumns 给能力列加表别名前缀，
// 避免与 join 表的同名列（id/namespace_id/org_id）混淆。
// 列顺序必须与下面的 scan 一致（scan 只取展示需要的子集，不含 display_name）。
func prefixedCapabilityColumns(alias string) string {
	cols := []string{"id", "namespace_id", "org_id", "name", "version", "params_schema", "outputs",
		"lifecycle", "sticky", "source", "content_hash", "created_at"}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// NodesForCapability 返回声明了该能力、且可接单的节点 ID（派发第 ③ 步的唯一查表入口）。
//
// 「可接单」= 节点状态不是 offline/draining。排序按 last_seen_at 新→旧，
// 让调用方（路由层）能在同分时偏向最近活跃的机器。
//
// 命名空间（I11③）：只在该命名空间内选机器 —— 同一个 capability_id 只可能
// 属于一个命名空间（能力 id 全局唯一且声明写入时已校验）。
func (d *DB) NodesForCapability(ctx context.Context, namespaceID, capabilityID string) ([]string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT n.id
		 FROM node_capabilities nc
		 JOIN nodes n ON n.id = nc.node_id
		 WHERE nc.capability_id = ? AND n.namespace_id = ?
		   AND n.status IN (?, ?)
		 ORDER BY COALESCE(n.last_seen_at, 0) DESC, n.id`,
		capabilityID, namespaceID, NodeStatusOnline, NodeStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("nodes for capability: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan node id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NodesDeclaringCapability 返回声明了该能力的**所有**节点（含 offline / draining）。
//
// 与 NodesForCapability 的区别是**用途**，不是实现细节：
//
//	NodesForCapability    派发入口 —— 只给现在能接单的机器
//	NodesDeclaringCapability  控制台展示 —— "谁声明了这个能力"是配置事实，
//	                          排空中的机器仍然声明着它，页面上必须看得到，
//	                          否则运维会以为这个能力没人管了
//
// 排序按状态（在线优先）+ 名字，让列表读起来稳定。
// 作用域是该命名空间（I11）：跨命名空间的机器不应出现在这张表里。
func (d *DB) NodesDeclaringCapability(ctx context.Context, namespaceID, capabilityID string) ([]*Node, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+prefixedNodeColumns("n")+`
		 FROM node_capabilities nc
		 JOIN nodes n ON n.id = nc.node_id
		 WHERE nc.capability_id = ? AND n.namespace_id = ?
		 ORDER BY CASE n.status WHEN 'online' THEN 0 WHEN 'draining' THEN 1 ELSE 2 END,
		          n.name, n.id`,
		capabilityID, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("nodes declaring capability: %w", err)
	}
	defer rows.Close()
	out := []*Node{}
	for rows.Next() {
		n, err := scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrNoCapacity 上面的哨兵错误。

// UpsertInstances 落库节点申报的实例槽位；超过 nodes.max_instances 返回 ErrTooManyInstances。
//
// 语义：节点每次 hello 申报的是一份**完整清单**，多余的老槽位删除（节点缩容）。
// slot 必须唯一且 ≥ 0，concurrency 至少 1。
//
// namespace_id 从 node 行取（契约 §2.3），不接受调用方传。
func (d *DB) UpsertInstances(ctx context.Context, namespaceID, nodeID string, specs []InstanceSpec) ([]*NodeInstance, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	node, err := d.GetNode(ctx, namespaceID, nodeID)
	if err != nil {
		return nil, err
	}
	if len(specs) > node.MaxInstances {
		return nil, fmt.Errorf("%w: declared %d instances, node max is %d",
			ErrTooManyInstances, len(specs), node.MaxInstances)
	}
	seen := make(map[int]bool, len(specs))
	for _, s := range specs {
		if s.Slot < 0 {
			return nil, fmt.Errorf("upsert instances: negative slot %d", s.Slot)
		}
		if seen[s.Slot] {
			return nil, fmt.Errorf("upsert instances: duplicate slot %d", s.Slot)
		}
		seen[s.Slot] = true
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin upsert instances: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 删掉本次没申报的旧槽位（节点缩容）
	if len(specs) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM node_instances WHERE node_id = ?`, nodeID); err != nil {
			return nil, fmt.Errorf("clear instances: %w", err)
		}
	} else {
		placeholders := make([]string, len(specs))
		args := make([]any, 0, len(specs)+1)
		args = append(args, nodeID)
		for i, s := range specs {
			placeholders[i] = "?"
			args = append(args, s.Slot)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_instances WHERE node_id = ? AND slot NOT IN (`+
				strings.Join(placeholders, ",")+`)`, args...); err != nil {
			return nil, fmt.Errorf("remove stale instances: %w", err)
		}
	}

	now := Now()
	for _, s := range specs {
		conc := s.Concurrency
		if conc <= 0 {
			conc = 1
		}
		// 已有槽位保留 busy_task（重启不该把正在跑的任务丢掉），只更新并发与存活标记。
		// namespace_id 取 node 行（父行先写入，所以这里一定拿得到）。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_instances (id, node_id, namespace_id, slot, concurrency, busy_task, alive, updated_at)
			 VALUES (?,?,?,?,?,NULL,1,?)
			 ON CONFLICT(node_id, slot) DO UPDATE SET
			   concurrency = excluded.concurrency,
			   alive = 1,
			   updated_at = excluded.updated_at`,
			NewInstanceID(), nodeID, node.NamespaceID, s.Slot, conc, now); err != nil {
			return nil, fmt.Errorf("upsert instance slot %d: %w", s.Slot, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit upsert instances: %w", err)
	}
	return d.ListInstances(ctx, nodeID)
}

// ListInstances 列出节点实例（空列表返回 []）。
// FreeSlotsByNode 一次查出多个节点的空闲槽位数（#145：消掉派发热路径上的 N+1）。
//
// 语义必须与 ListInstances 的口径一致：**alive 且 busy_task IS NULL** 的槽位算空闲。
// 用 LEFT JOIN 而不是 INNER：没申报槽位的节点仍要出现在结果里（free=0），
// 否则调用方会把它当成"查不到"而丢掉——它只是优先级最低，仍然可以接派发。
func (d *DB) FreeSlotsByNode(ctx context.Context, nodeIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = 0 // 先给 0：LEFT JOIN 没匹配到时不会出现在结果里
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := d.QueryContext(ctx,
		`SELECT n.id, COUNT(i.id)
		   FROM nodes n
		   LEFT JOIN node_instances i
		     ON i.node_id = n.id AND i.alive = 1 AND i.busy_task IS NULL
		  WHERE n.id IN (`+placeholders+`)
		  GROUP BY n.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("free slots by node: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var free int
		if err := rows.Scan(&id, &free); err != nil {
			return nil, fmt.Errorf("scan free slots: %w", err)
		}
		out[id] = free
	}
	return out, rows.Err()
}

func (d *DB) ListInstances(ctx context.Context, nodeID string) ([]*NodeInstance, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, node_id, namespace_id, slot, concurrency, busy_task, alive, updated_at
		 FROM node_instances WHERE node_id = ? ORDER BY slot`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	out := []*NodeInstance{}
	for rows.Next() {
		var in NodeInstance
		var alive int
		if err := rows.Scan(&in.ID, &in.NodeID, &in.NamespaceID, &in.Slot, &in.Concurrency,
			&in.BusyTask, &alive, &in.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		in.Alive = alive != 0
		out = append(out, &in)
	}
	return out, rows.Err()
}

// MarkInstancesDead 把节点全部实例标记为掉线（连接断开时调用）。
//
// 只是标记 alive=0，不删行：槽位申报是节点的事，中枢不替它决定「少了一个槽」。
// 也不清 busy_task——任务归属由租约巡检（SweepExpiredLeases）回收，那是唯一入口。
func (d *DB) MarkInstancesDead(ctx context.Context, nodeID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET alive = 0, updated_at = ? WHERE node_id = ?`,
		Now(), nodeID); err != nil {
		return fmt.Errorf("mark instances dead: %w", err)
	}
	return nil
}

// LeaseInstance 把任务占到一个空闲实例上（CAS：busy_task IS NULL → taskID）。
//
// 「实际并发 = min(实例 concurrency, 实现级 concurrency, nodes.max_concurrency)」
// 由调用方（路由层）算，这里只做最后一道 CAS 抢占：并发 UPDATE 下只有一个能成功。
// 返回 instance_id；没有空闲槽位返回 ErrNoCapacity。
func (d *DB) LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("lease instance: empty task_id")
	}
	// 先找最早的存活空闲槽位（ORDER BY slot 保证确定性），再由 CAS 更新抢占。
	// SQLite 单写 + 单连接使这两步不会与另一个写者交错；
	// 跨连接场景下 CAS 失败会重试下一轮，不会出现两个任务占同一槽。
	for attempt := 0; attempt < 3; attempt++ {
		var instanceID string
		err := d.QueryRowContext(ctx,
			`SELECT id FROM node_instances
			 WHERE node_id = ? AND alive = 1 AND busy_task IS NULL
			 ORDER BY slot LIMIT 1`, nodeID).Scan(&instanceID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNoCapacity
		}
		if err != nil {
			return "", fmt.Errorf("find free instance: %w", err)
		}
		res, err := d.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = ?, updated_at = ?
			 WHERE id = ? AND busy_task IS NULL`, taskID, Now(), instanceID)
		if err != nil {
			return "", fmt.Errorf("lease instance: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("lease instance rows: %w", err)
		}
		if n == 1 {
			return instanceID, nil
		}
		// CAS 失败：被另一个写者抢了，重试找下一个空闲槽
	}
	return "", ErrNoCapacity
}

// LeaseAndAssign 是派发的**一个事务**：占槽位 + pending→assigned + 写归属/attempts（#141）。
//
// 合并前派发要 3 次提交（LeaseInstance 的占槽 UPDATE 一次、AssignTask 里的
// Transition 一次、写归属/attempts 一次），中间失败会出现两种
// 脏状态：槽位占了任务却没派出去（槽位泄漏，要等巡检才收回），或任务已 assigned
// 而 node_id 还空着（调用方必须自己补 ReleaseInstance 才不留坑）。
//
// 顺序与语义都和拆分版一致，只是原子化：
//  1. 找最早的存活空闲槽位（口径与 LeaseInstance 完全一致）
//  2. CAS 占位（busy_task IS NULL → taskID）；RowsAffected==0 → ErrNoCapacity
//  3. transitionGuarded(pending → assigned)（状态写唯一入口）
//  4. 写 node/instance/lease_until 并 attempts+1
//
// 任一步失败都整体回滚——槽位不泄漏，attempts 也不会被提前 +1。
func (d *DB) LeaseAndAssign(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64) (instanceID string, err error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return "", ErrNamespaceRequired
	}
	if taskID == "" {
		// 与 LeaseInstance 的参数校验口径一致：先拒绝，连事务都不开（不占槽）
		return "", fmt.Errorf("lease and assign: empty task_id")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin lease and assign: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ① 空闲槽位：ORDER BY slot 保证确定性，且必须走 tx
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM node_instances
		 WHERE node_id = ? AND alive = 1 AND busy_task IS NULL
		 ORDER BY slot LIMIT 1`, nodeID).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoCapacity
	}
	if err != nil {
		return "", fmt.Errorf("lease and assign: find free instance: %w", err)
	}

	// ② CAS 占位：并发 UPDATE 下只有一个能成功。
	// 本进程内 MaxOpenConns(1) + 事务内单写者让它不可达，但条件必须留着——
	// 它是放开连接数/跨进程场景下的最后一道闸。
	res, err := tx.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = ?, updated_at = ?
		 WHERE id = ? AND busy_task IS NULL`, taskID, Now(), instanceID)
	if err != nil {
		return "", fmt.Errorf("lease and assign: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("lease and assign rows: %w", err)
	}
	if n == 0 {
		return "", ErrNoCapacity
	}

	// ③ 改状态（唯一写入口）+ ④ 写归属/attempts
	// 命名空间（I11）：任务与槽位必须在同一个命名空间里——nodeID 来自
	// NodesForCapability 的同一命名空间查询，taskID 由调用方按同一命名空间获取，
	// assignTaskTx 内的 WHERE 还会再过一次 namespace_id。
	if err := d.assignTaskTx(ctx, tx, namespaceID, taskID, nodeID, instanceID, leaseUntil); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit lease and assign: %w", err)
	}
	return instanceID, nil
}

// ReleaseInstance 释放实例占用（任务结束/回收时调用）。
func (d *DB) ReleaseInstance(ctx context.Context, nodeID, instanceID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND id = ?`, Now(), nodeID, instanceID); err != nil {
		return fmt.Errorf("release instance: %w", err)
	}
	return nil
}

// ReleaseInstanceByTask 按任务 ID 释放它占用的槽位（不知道 instance_id 时用）。
func (d *DB) ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND busy_task = ?`, Now(), nodeID, taskID); err != nil {
		return fmt.Errorf("release instance by task: %w", err)
	}
	return nil
}

// ---------- 粘性路由 ----------

// StickyKey 计算粘性键：sha256(trace_id ∥ capability_id ∥ selector)。
//
// 同一 trace 里同能力+同选择器永远落到同一台机器（例如同一个 worktree）。
// 用 \x00 分隔，避免拼接歧义（("ab","c") 与 ("a","bc")）。
func StickyKey(traceID, capabilityID, selector string) string {
	sum := sha256.Sum256([]byte(traceID + "\x00" + capabilityID + "\x00" + selector))
	return hex.EncodeToString(sum[:])
}

// GetPin 按 sticky_key 取粘性钉住；未命中返回 ErrNotFound。
//
// sticky_key 全局唯一（派发时按 trace/capability/selector 摘要算出），所以不需要
// 命名空间参数；但返回的行带 NamespaceID，调用方靠它确认钉住的机器与任务同域。
func (d *DB) GetPin(ctx context.Context, stickyKey string) (*NodePin, error) {
	var p NodePin
	err := d.QueryRowContext(ctx,
		`SELECT sticky_key, namespace_id, org_id, node_id, selector, created_at, last_used_at
		 FROM node_pins WHERE sticky_key = ?`, stickyKey).
		Scan(&p.StickyKey, &p.NamespaceID, &p.OrgID, &p.NodeID, &p.Selector, &p.CreatedAt, &p.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get pin: %w", err)
	}
	return &p, nil
}

// PutPin 写入（或刷新）粘性钉住。已存在时更新 last_used_at（钉住不换机器）。
//
// p.NamespaceID 必填（钉住的是「哪个命名空间里的哪台机器」），p.OrgID 由命名空间
// 派生并**覆盖**（I10）；p.NodeID 必须是该命名空间内的节点。
func (d *DB) PutPin(ctx context.Context, p *NodePin) error {
	if p == nil || p.StickyKey == "" || p.NodeID == "" {
		return fmt.Errorf("put pin: sticky_key/node_id required")
	}
	if p.NamespaceID == "" || p.NamespaceID == AllNamespaces {
		return fmt.Errorf("put pin: %w", ErrNamespaceRequired)
	}
	orgID, err := d.NamespaceOrgID(ctx, p.NamespaceID)
	if err != nil {
		return fmt.Errorf("put pin: %w", err)
	}
	p.OrgID = orgID
	now := Now()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	if p.LastUsedAt == 0 {
		p.LastUsedAt = now
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO node_pins (sticky_key, namespace_id, org_id, node_id, selector, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(sticky_key) DO UPDATE SET last_used_at = excluded.last_used_at`,
		p.StickyKey, p.NamespaceID, p.OrgID, p.NodeID, p.Selector, p.CreatedAt, p.LastUsedAt); err != nil {
		return fmt.Errorf("put pin: %w", err)
	}
	return nil
}

// DeletePin 删除粘性钉住（节点下线 / 换机器时用）。
func (d *DB) DeletePin(ctx context.Context, stickyKey string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM node_pins WHERE sticky_key = ?`, stickyKey); err != nil {
		return fmt.Errorf("delete pin: %w", err)
	}
	return nil
}

// DeletePinsForNode 清掉某节点的全部钉住（节点下线/排空时用）。
func (d *DB) DeletePinsForNode(ctx context.Context, nodeID string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM node_pins WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete pins for node: %w", err)
	}
	return nil
}

// SetNodeTokenHash 覆盖节点的 token 哈希（仅用于开发工具重建节点凭据）。
//
// 明文 token 只在 CreateNode 时返回一次，之后无法复原。开发场景（devseed
// 反复跑）需要一个"同一个节点、换一把钥匙"的入口，否则只能不断新建节点行，
// 留下大量永远离线的死节点。
//
// 作用域仍是命名空间（I10）：换钥匙也得说清楚换的是哪片隔离域里的节点。
func (d *DB) SetNodeTokenHash(ctx context.Context, namespaceID, nodeID, tokenHash string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if nodeID == "" || tokenHash == "" {
		return fmt.Errorf("set node token hash: empty node_id or hash")
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET token_hash = ? WHERE namespace_id = ? AND id = ?`, tokenHash, namespaceID, nodeID)
	if err != nil {
		return fmt.Errorf("update node token hash: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("set node token hash: %w", ErrNotFound)
	}
	return nil
}
