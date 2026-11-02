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
	"log/slog"
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
	// NamespaceID 是**隔离维度**（I11）：机器属于且只属于一个命名空间，
	// 令牌、能力声明、槽位、粘性钉住都跟着它走。
	NamespaceID    string   `json:"namespaceId"`
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
	// NamespaceID 是槽位所属命名空间（隔离维度，I11）。
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
	// NamespaceID 必填（钉住的是「某个命名空间里的某台机器」）。
	NamespaceID string `json:"namespaceId"`
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

// NodeCapability 是「某节点拥有这个能力」的一行：能力定义。
//
// v2 起这里**不再有 run_spec**：执行方式属于产物（artifacts）、归属属于部署
// （deployments），node_capabilities 只从 deployments 派生（契约 D4）。
//
// 嵌入 *CapabilityVersion 是为了不动既有调用点的 .Name/.Outputs 等字段访问
// （派发选点、控制台都只关心能力定义）。
type NodeCapability struct {
	*CapabilityVersion
}

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
// n.NamespaceID 必填：机器归属哪个命名空间，决定了它的令牌、能力声明、
// 槽位与粘性钉住活在哪片隔离域里（契约 §2.3）。
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
	if _, err := d.ExecContext(ctx,
		`INSERT INTO nodes
		   (id, namespace_id, name, tags, status, token_hash, max_instances, max_concurrency, last_seen_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.NamespaceID, n.Name, string(tagsJSON), n.Status, n.TokenHash,
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
const nodeColumns = `id, namespace_id, name, tags, status, token_hash,
	max_instances, max_concurrency, last_seen_at, created_at`

// prefixedNodeColumns 给 nodeColumns 的每一列加表别名前缀，供 join 查询使用。
//
// 为什么必须带前缀：`node_capabilities` 也有 `namespace_id`（#162 加的隔离列），
// 不限定表名的 `namespace_id` 会让 SQLite 报 ambiguous column name。
func prefixedNodeColumns(alias string) string {
	cols := []string{"id", "namespace_id", "name", "tags", "status", "token_hash",
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
	if err := rows.Scan(&n.ID, &n.NamespaceID, &n.Name, &tags, &n.Status, &n.TokenHash,
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
// token 全局唯一，所以本查询不看命名空间。
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

// GetNode 按 (namespaceID, id) 取节点（不变量 I11：必须带命名空间）。
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

// AppliedRef 是节点在 hello 里陈述的一条「我缓存了这份产物」。
//
// 注意它**不是**能力声明：能力归属的唯一写者是控制台的部署（契约 D4）。
// 中枢拿到 applied 只做两件事 —— 对账 node_capabilities，以及把 deployments 的
// observed 状态标成 applied / local（发现本地变体）。
type AppliedRef struct {
	Name    string `json:"capability"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// ConfirmCapabilities 是 ReconcileApplied 的兼容入口（devseed / 测试造数据用）。
//
// 保留它是为了让「播种一个节点」不必先造部署行；语义完全一致：
// 仍然只认中枢已定义的能力，仍然全量替换。
func (d *DB) ConfirmCapabilities(ctx context.Context, namespaceID, nodeID string, caps []CapabilityRef) ([]string, error) {
	applied := make([]AppliedRef, 0, len(caps))
	for _, ref := range caps {
		applied = append(applied, AppliedRef{Name: ref.Name, Version: ref.Version})
	}
	return d.ReconcileApplied(ctx, namespaceID, nodeID, applied)
}

// ReconcileApplied 用 hello.applied[] 全量重建这台节点的能力归属（契约 D4）。
//
// 两步，同事务：
//  1. 逐条解析 name@version → capability_id（**只在本节点所在命名空间**内解析）。
//     任一条未定义 → ErrUnknownCapability，整帧拒绝、一行不写。
//  2. DELETE + INSERT 重建 node_capabilities（全量替换语义保留）。
//
// 顺带把 deployments 的 observed 列对账成 applied / local：
//   - applied：节点缓存里的 digest 与中枢期望一致
//   - local  ：不一致 —— 用户手改过 = 本地变体，中枢永不覆盖（契约 §7）
//
// **它不会做的事**：不会因为 applied 里出现一条没部署过的能力就给它归属。
// 那会让中枢数据库开始静默撒谎（页面显示「这台机器能跑」，真的派过去却 ack 后 failed）。
// 唯一的归属写者是控制台的部署。
func (d *DB) ReconcileApplied(ctx context.Context, namespaceID, nodeID string, applied []AppliedRef) ([]string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if nodeID == "" {
		return nil, fmt.Errorf("reconcile applied: empty node_id")
	}

	// 节点行是唯一真相：能力定义只在它所属的命名空间里解析（I11③）。
	node, err := d.GetNode(ctx, namespaceID, nodeID)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(applied))
	var (
		nameProblems []string
		unknownRefs  []string
		unknown      bool
	)
	for i, a := range applied {
		if a.Name == "" {
			nameProblems = append(nameProblems,
				fmt.Sprintf("capability name is empty (version %d)", a.Version))
			unknown = true
			continue
		}
		ref := fmt.Sprintf("%s@%d", a.Name, a.Version)
		c, err := d.GetCapability(ctx, node.NamespaceID, a.Name, a.Version)
		switch {
		case errors.Is(err, ErrNotFound):
			unknownRefs = append(unknownRefs, ref)
			unknown = true
		case err != nil:
			return nil, err
		case c.NamespaceID != node.NamespaceID:
			// 数据被写坏时才可能走到：宁可 400 说清楚，也不要把别的命名空间的
			// 能力 id 写进本节点的声明。
			return nil, &CrossNamespaceError{
				What: "node_capabilities.capability_id",
				Want: node.NamespaceID,
				Got:  c.NamespaceID,
			}
		default:
			ids[i] = c.ID
		}
	}

	problems := make([]string, 0, len(nameProblems)+1)
	problems = append(problems, nameProblems...)
	if len(unknownRefs) > 0 {
		problems = append(problems, "unknown capability: "+strings.Join(unknownRefs, ", "))
	}
	if len(problems) > 0 {
		return nil, &CapabilityDeclError{Problems: problems, UnknownCapability: unknown}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reconcile applied: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_capabilities WHERE node_id = ?`, nodeID); err != nil {
		return nil, fmt.Errorf("clear node capabilities: %w", err)
	}
	for i := range applied {
		if ids[i] == "" {
			continue
		}
		// 同一 name@version 重复出现时后者胜（与旧实现的「后者胜」一致，行为确定）。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_capabilities (node_id, capability_id, namespace_id) VALUES (?,?,?)
			 ON CONFLICT(node_id, capability_id) DO NOTHING`,
			nodeID, ids[i], node.NamespaceID); err != nil {
			return nil, fmt.Errorf("insert node capability: %w", err)
		}
		if _, err := d.markDeployedDigest(ctx, tx, nodeID, ids[i], applied[i].Digest); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reconcile applied: %w", err)
	}
	return ids, nil
}

// ListNodeCapabilities 返回节点拥有的能力定义（含 outputs，供 kind 推导与派发选点）。
//
// 顺序（name, version）不变，空列表返回 []。
// 数据来源是 node_capabilities —— 它只从 deployments 派生（契约 D4），
// 所以「这台机器能跑什么」永远等于「控制台给它部署了什么」。
func (d *DB) ListNodeCapabilities(ctx context.Context, nodeID string) ([]*NodeCapability, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+prefixedCapabilityColumns("c")+`
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
		var paramsSchema, outputs, lifecycle string
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.Name, &c.Version, &paramsSchema, &outputs,
			&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt); err != nil {
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
		out = append(out, &NodeCapability{CapabilityVersion: &c})
	}
	return out, rows.Err()
}

// InstanceStat 是一台节点的槽位汇总（只读投影，供批量视图用）。
//
// 口径与 ListInstances 完全一致：Total 是**行数**（不看 alive —— 掉线的槽位
// 仍然是节点申报过的槽位，控制台页面上要显示它，与旧实现 len(insts) 对齐）。
type InstanceStat struct {
	Total int // node_instances 行数（与 ListInstances 同口径：不看 alive）
	Busy  int // busy_task IS NOT NULL 的行数
}

// nodeIDBatchSize 是一条 IN 查询里最多塞多少个 node_id。
//
// SQLite 的宿主参数个数有上限（老版本是 999），而 `/v1/nodes?namespaceId=*` 的节点数
// **没有上限**（store.ListNodes 不分页）。不分批的话，节点一多就整页 500 —— 旧实现是
// 每节点一条单参数查，不会有这个问题，所以这里必须分批把那个退路补回来。
const nodeIDBatchSize = 400

// uniqueNodeIDs 去重（IN 里重复只让参数列表变长，没有语义价值）。
func uniqueNodeIDs(nodeIDs []string) []string {
	out := make([]string, 0, len(nodeIDs))
	seen := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ListCapabilitiesByNode 批量取多台节点声明的能力（替代每节点一次 ListNodeCapabilities）。
//
// 为什么需要它：控制台列表页对每台节点各查一次能力，SQL 条数随节点数线性增长
// （#223：N=100 时整页 201 条串行 SQL）。这里合并成 IN 查询，条数与 N 无关
// （每 nodeIDBatchSize 个 id 一条，节点数正常时就是 1 条）。
//
// 语义必须与 ListNodeCapabilities **逐字一致**（否则列表页与详情页会对同一台机器
// 给出不同的能力/顺序/runSpec）：
//   - 每节点内顺序仍是 c.name, c.version 升序（ORDER BY 带上 node_id 不破坏这一点）
//   - params_schema / outputs / lifecycle 的反序列化、Outputs nil → []、
//     run_spec 空串 → emptyRunSpec 全部照抄单节点实现
//   - 没有能力的节点**不出现在** map 里（调用方按 nil → 空切片处理）
//   - nodeIDs 为空 → 空 map + nil error；重复 id 去重
//
// 降级粒度（审查 minor-3）：单行 JSON 坏掉时**只丢这台节点**的能力（等价于旧实现
// 「这台节点的 ListNodeCapabilities 返回 error → caps=nil」），并留一条 WARN；
// 绝不因为一行坏数据把整页所有节点的能力都清空。
func (d *DB) ListCapabilitiesByNode(ctx context.Context, nodeIDs []string) (map[string][]*NodeCapability, error) {
	ids := uniqueNodeIDs(nodeIDs)
	out := make(map[string][]*NodeCapability, len(ids))
	broken := make(map[string]struct{})
	for start := 0; start < len(ids); start += nodeIDBatchSize {
		end := start + nodeIDBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := d.listCapabilitiesChunk(ctx, ids[start:end], out, broken); err != nil {
			return nil, err
		}
	}
	// 一台节点只要有一行坏数据，就整台降级（与旧实现的 per-node 语义一致）。
	for id := range broken {
		delete(out, id)
	}
	return out, nil
}

// listCapabilitiesChunk 是 ListCapabilitiesByNode 的一批。
func (d *DB) listCapabilitiesChunk(ctx context.Context, ids []string, out map[string][]*NodeCapability, broken map[string]struct{}) error {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := d.QueryContext(ctx,
		`SELECT nc.node_id, `+prefixedCapabilityColumns("c")+`
		 FROM node_capabilities nc
		 JOIN capability_versions c ON c.id = nc.capability_id
		 WHERE nc.node_id IN (`+placeholders+`)
		 ORDER BY nc.node_id, c.name, c.version`, args...)
	if err != nil {
		return fmt.Errorf("list node capabilities by node: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		var c CapabilityVersion
		var paramsSchema, outputs, lifecycle string
		if err := rows.Scan(&nodeID, &c.ID, &c.NamespaceID, &c.Name, &c.Version, &paramsSchema,
			&outputs, &lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt); err != nil {
			return fmt.Errorf("scan node capability by node: %w", err)
		}
		if err := decodeCapabilityJSON(&c, paramsSchema, outputs, lifecycle); err != nil {
			// 坏数据只降级这一台（见 ListCapabilitiesByNode 的降级粒度注释）：
			// 静默 continue 会让页面少一条能力却没人知道，所以必须留声。
			slog.Warn("skip node capability with invalid JSON",
				"event", "store.node_capabilities.bad_json",
				"node_id", nodeID,
				"capability_id", c.ID,
				"error", err.Error(),
			)
			broken[nodeID] = struct{}{}
			continue
		}
		out[nodeID] = append(out[nodeID], &NodeCapability{CapabilityVersion: &c})
	}
	return rows.Err()
}

// decodeCapabilityJSON 反序列化能力定义里的三列 JSON。
//
// 抽出来只为让批量路径与单节点路径共用同一份解码规则（空值判据、nil→[] 都在这里），
// 把「两处各写一遍」的漂移风险消掉。
func decodeCapabilityJSON(c *CapabilityVersion, paramsSchema, outputs, lifecycle string) error {
	if err := json.Unmarshal([]byte(paramsSchema), &c.ParamsSchema); err != nil {
		return fmt.Errorf("decode params_schema: %w", err)
	}
	if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
		return fmt.Errorf("decode outputs: %w", err)
	}
	if c.Outputs == nil {
		c.Outputs = []string{}
	}
	if err := json.Unmarshal([]byte(lifecycle), &c.Lifecycle); err != nil {
		return fmt.Errorf("decode lifecycle: %w", err)
	}
	return nil
}

// InstanceStatsByNode 批量取多台节点的槽位总数/占用数（替代每节点一次 ListInstances）。
//
// 这里不返回槽位明细：列表页只要两个数字，把 N×M 行拉回来再在 Go 里数
// 是把内存和 GC 也一起做成 N+1（详情页仍然走 ListInstances）。
//
// 语义与旧实现对齐（len(insts) / busy_task 非空计数）：
//   - Total 不看 alive（掉线的槽位照样是申报过的槽位）
//   - 没有槽位的节点**不出现在** map 里（调用方按零值处理）
//   - nodeIDs 为空 → 空 map + nil error；重复 id 去重；按 nodeIDBatchSize 分批
func (d *DB) InstanceStatsByNode(ctx context.Context, nodeIDs []string) (map[string]InstanceStat, error) {
	ids := uniqueNodeIDs(nodeIDs)
	out := make(map[string]InstanceStat, len(ids))
	for start := 0; start < len(ids); start += nodeIDBatchSize {
		end := start + nodeIDBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		err := func() error {
			rows, err := d.QueryContext(ctx,
				`SELECT node_id, COUNT(id), COUNT(busy_task)
				 FROM node_instances
				 WHERE node_id IN (`+placeholders+`)
				 GROUP BY node_id`, args...)
			if err != nil {
				return fmt.Errorf("instance stats by node: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var nodeID string
				var stat InstanceStat
				if err := rows.Scan(&nodeID, &stat.Total, &stat.Busy); err != nil {
					return fmt.Errorf("scan instance stats: %w", err)
				}
				out[nodeID] = stat
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prefixedCapabilityColumns 给能力列加表别名前缀，
// 避免与 join 表的同名列（id/namespace_id）混淆。
// 列顺序必须与下面的 scan 一致（scan 只取展示需要的子集，不含 display_name）。
func prefixedCapabilityColumns(alias string) string {
	cols := []string{"id", "namespace_id", "name", "version", "params_schema", "outputs",
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

// ReclaimStaleSlots 清掉「槽位指向的任务已经不在这台机器上跑了」的钉子（#218 兜底）。
//
// 为什么需要它：`busy_task` 的语义是「这个槽位现在被哪条任务占着」。正常路径下
// 它由 result / 巡检回收 / 连接结束时的主动释放（ResolveLostTasks）清掉。但中枢
// 在「已派发、未回收」的窗口里被 kill -9（或旧版本升级上来）时，没人再清它，
// 槽位就永久卡住——节点显示 online 却再也接不到任何任务。
//
// 所以节点每次 hello 时做一次幂等修复：**只清**那种任务已经不归本节点在飞的槽位
// （任务已终态 / 已回 pending 等重派 / 已被改派到别的节点），仍在 assigned/acked/running
// 且属于本节点的任务一行都不动（不抢跑租约巡检）。
//
// 返回清掉的槽位数。
func (d *DB) ReclaimStaleSlots(ctx context.Context, namespaceID, nodeID string, now int64) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	if now == 0 {
		now = Now()
	}
	res, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		  WHERE node_id = ? AND namespace_id = ? AND busy_task IS NOT NULL
		    AND NOT EXISTS (
		      SELECT 1 FROM tasks t
		       WHERE t.id = node_instances.busy_task
		         AND t.namespace_id = ?
		         AND t.node_id = node_instances.node_id
		         AND t.status IN (?,?,?))`,
		now, nodeID, namespaceID, namespaceID, TaskAssigned, TaskAcked, TaskRunning)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale slots: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale slots rows: %w", err)
	}
	return int(n), nil
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
		`SELECT sticky_key, namespace_id, node_id, selector, created_at, last_used_at
		 FROM node_pins WHERE sticky_key = ?`, stickyKey).
		Scan(&p.StickyKey, &p.NamespaceID, &p.NodeID, &p.Selector, &p.CreatedAt, &p.LastUsedAt)
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
// p.NamespaceID 必填（钉住的是「哪个命名空间里的哪台机器」）；
// p.NodeID 必须是该命名空间内的节点。
func (d *DB) PutPin(ctx context.Context, p *NodePin) error {
	if p == nil || p.StickyKey == "" || p.NodeID == "" {
		return fmt.Errorf("put pin: sticky_key/node_id required")
	}
	if p.NamespaceID == "" || p.NamespaceID == AllNamespaces {
		return fmt.Errorf("put pin: %w", ErrNamespaceRequired)
	}
	now := Now()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	if p.LastUsedAt == 0 {
		p.LastUsedAt = now
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO node_pins (sticky_key, namespace_id, node_id, selector, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(sticky_key) DO UPDATE SET last_used_at = excluded.last_used_at`,
		p.StickyKey, p.NamespaceID, p.NodeID, p.Selector, p.CreatedAt, p.LastUsedAt); err != nil {
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
// 作用域仍是命名空间（I11）：换钥匙也得说清楚换的是哪片隔离域里的节点。
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
