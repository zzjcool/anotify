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
	// AgentVersion / AgentCommit 是节点上跑的那个 agent 的版本信息（契约 §3/§5）。
	//
	// AgentVersion 由 hello 帧自报（老 agent 不带 = 空串 = 未知）；AgentCommit 只由
	// upgrade_ack 落库（hello 帧没有这个字段）。两者都是 camelCase 出线：
	// 控制台的节点列表直接靠这两个键显示「这台机器跑的是哪一版」。
	AgentVersion string `json:"agentVersion"`
	AgentCommit  string `json:"agentCommit"`
	// AgentUpgrade 是最近一次升级尝试的状态（CONTRACT-agent-upgrade §5.1）。
	//
	// 恒存在（零值 = 从未尝试）：控制台刷新页面/服务重启后仍要看到上次升级的结果，
	// 否则 UI 的失败映射（need_root → 提示用户去节点跑 sudo）就是死代码。
	AgentUpgrade AgentUpgrade `json:"agentUpgrade"`
	LastSeenAt   *int64       `json:"lastSeenAt"`
	CreatedAt    int64        `json:"createdAt"`
}

// 升级尝试的状态（nodes.agent_upgrade 的 status，CONTRACT-agent-upgrade §5.1）。
const (
	// AgentUpgradeNone 表示从未尝试过升级（零值）。
	AgentUpgradeNone = ""
	// AgentUpgradeSent 表示 upgrade 帧已下发，等 upgrade_ack。
	AgentUpgradeSent = "sent"
	// AgentUpgradeOK 表示 ack 报成功（版本列已被 ack 更新）。
	AgentUpgradeOK = "ok"
	// AgentUpgradeFailed 表示 ack 报失败（版本列保持旧值）。
	AgentUpgradeFailed = "failed"
)

// AgentUpgrade 是一次升级尝试的结果。
//
// 它同时是**落库形状**（nodes.agent_upgrade 的 JSON）与**出线形状**（GET /v1/nodes
// 每台节点的 agentUpgrade）：一份 struct 只有一个字段名真相，避免「库里一个键、
// 接口另一个键」这种只能靠人眼对的东西。json tag 一律 camelCase（项目 API 规范）。
type AgentUpgrade struct {
	// Status ∈ "" / sent / ok / failed。
	Status string `json:"status"`
	// TargetVersion 是这次尝试的目标版本（中枢当时的 agent 版本）。
	TargetVersion string `json:"targetVersion"`
	// Error 是失败短码（need_root / bad_sha / download_failed / not_supported）；空 = 无错误。
	Error string `json:"error"`
	// At 是 sent/ok/failed 那一刻的 unix 秒。
	At int64 `json:"at"`
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
	// PinID 是这一**行**的身份（#315）：RollbackPin 的条件删除按它匹配，
	// 而不是按秒级的 CreatedAt（同一秒同 key 同节点的两次派发会撞车）。
	//
	// 由 PutPin 在新建时就地生成并写回调用方的 struct；已存在的行不会被后到的
	// upsert 冒领（ON CONFLICT 只刷 last_used_at）。加这一列之前建的老行是 ""，
	// 它们不属于任何一次「本次派发」，所以永远不匹配回滚条件。
	PinID      string `json:"pinId"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
}

// 节点注册 / 派发相关的哨兵错误。
var (
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

// CapabilityDeclError 是一份**形状坏掉**的声明的全部问题（一次列全，不是只报第一条）。
//
// 只剩一类问题：`applied[]` 里能力名为空。那不是「中枢没有这个能力」（那是陈旧
// 缓存，见 ReconcileApplied 的注释），而是节点自己拼错了帧 —— 这类必须响。
//
// 为什么列全：修一条重启一次的成本远高于一次看全。
type CapabilityDeclError struct {
	Problems []string // 每条自描述，顺序确定
}

// Error 把全部问题拼成一行（; 分隔，顺序确定）。
func (e *CapabilityDeclError) Error() string { return strings.Join(e.Problems, "; ") }

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

// nodeColumns 是统一的列顺序，scanNodeRow 依赖它（与 schema.sql 的 nodes 定义同序）。
const nodeColumns = `id, namespace_id, name, tags, status, token_hash,
	max_instances, max_concurrency, last_seen_at, created_at, agent_version, agent_commit,
	agent_upgrade`

// prefixedNodeColumns 给 nodeColumns 的每一列加表别名前缀，供 join 查询使用。
//
// 为什么必须带前缀：`node_capabilities` 也有 `namespace_id`（#162 加的隔离列），
// 不限定表名的 `namespace_id` 会让 SQLite 报 ambiguous column name。
func prefixedNodeColumns(alias string) string {
	cols := []string{"id", "namespace_id", "name", "tags", "status", "token_hash",
		"max_instances", "max_concurrency", "last_seen_at", "created_at",
		"agent_version", "agent_commit", "agent_upgrade"}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// scanNodeRow 从 rows 解析一行。
func scanNodeRow(rows *sql.Rows) (*Node, error) {
	var n Node
	var tags, agentUpgrade string
	if err := rows.Scan(&n.ID, &n.NamespaceID, &n.Name, &tags, &n.Status, &n.TokenHash,
		&n.MaxInstances, &n.MaxConcurrency, &n.LastSeenAt, &n.CreatedAt,
		&n.AgentVersion, &n.AgentCommit, &agentUpgrade); err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &n.Tags); err != nil {
		return nil, fmt.Errorf("decode node tags: %w", err)
	}
	if n.Tags == nil {
		n.Tags = []string{} // 不变量 I9
	}
	// 空串 = 迁移前就存在的行（列默认 '{}'，但防御旧行）→ 零值 = 从未尝试。
	if agentUpgrade != "" && agentUpgrade != "{}" {
		if err := json.Unmarshal([]byte(agentUpgrade), &n.AgentUpgrade); err != nil {
			return nil, fmt.Errorf("decode node agent upgrade: %w", err)
		}
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
	return nodeRowsAffected(res, "update node status")
}

// UpdateNodeAgentVersion 记录节点自报的 agent 版本（hello.agent_version，契约 §3）。
//
// 只写 version，**不碰** agent_commit：hello 帧里没有 commit，而 commit 是
// upgrade_ack 的产物（见 RecordAgentUpgrade）。老 agent 不带这个字段时写入空串
// ——「现在这台机器上的版本未知」是事实，保留一个上一版的旧值才是撒谎。
//
// 行不存在返回 ErrNotFound（与 UpdateNodeStatus 同口径）：作用域写错时必须响，
// 不能静默影响 0 行然后让人以为写成功了。
func (d *DB) UpdateNodeAgentVersion(ctx context.Context, namespaceID, nodeID, version string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_version = ? WHERE namespace_id = ? AND id = ?`,
		version, namespaceID, nodeID)
	if err != nil {
		return fmt.Errorf("update node agent version: %w", err)
	}
	return nodeRowsAffected(res, "update node agent version")
}

// MarkAgentUpgradeSent 记录一次已下发的升级尝试（agent_upgrade.status=sent）。
//
// 只写 agent_upgrade，不碰版本列 —— 帧刚推出去，节点还没换二进制；
// 版本列要等 upgrade_ack 才是事实（提前改成目标版本就是谎报成功）。
func (d *DB) MarkAgentUpgradeSent(ctx context.Context, namespaceID, nodeID, targetVersion string, at int64) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	up := AgentUpgrade{Status: AgentUpgradeSent, TargetVersion: targetVersion, At: at}
	raw, err := json.Marshal(up)
	if err != nil {
		return fmt.Errorf("encode agent upgrade: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_upgrade = ? WHERE namespace_id = ? AND id = ?`,
		string(raw), namespaceID, nodeID)
	if err != nil {
		return fmt.Errorf("mark agent upgrade sent: %w", err)
	}
	return nodeRowsAffected(res, "mark agent upgrade sent")
}

// RecordAgentUpgrade 记录一次**成功**的 agent 升级（upgrade_ack ok=true）。
//
// 一个 UPDATE 同时写三样：agent_version / agent_commit / agent_upgrade(status=ok)。
// 与 UpdateNodeAgentVersion 分开的原因：ack 是「升级完成」的权威事实，版本与状态
// 必须一起变——只更新其中一个会出现「列出 agentVersion=新版本、agentUpgrade 还是 sent」
// 这种自相矛盾的视图。
//
// 升级后版本变了，旧 commit 就一定过期了——所以不做「空 commit 保留旧值」的兜底：
// ack 没带 commit 就写空串（未知），留着上一版的 commit 才是错误关联。
func (d *DB) RecordAgentUpgrade(ctx context.Context, namespaceID, nodeID, version, commit string, at int64) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	// ok 时「节点跑着的版本」就是这次尝试的目标版本（ack 只在替换成功后发）。
	up := AgentUpgrade{Status: AgentUpgradeOK, TargetVersion: version, At: at}
	raw, err := json.Marshal(up)
	if err != nil {
		return fmt.Errorf("encode agent upgrade: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_version = ?, agent_commit = ?, agent_upgrade = ?
		 WHERE namespace_id = ? AND id = ?`,
		version, commit, string(raw), namespaceID, nodeID)
	if err != nil {
		return fmt.Errorf("record node agent upgrade: %w", err)
	}
	return nodeRowsAffected(res, "record node agent upgrade")
}

// FailAgentUpgrade 记录一次**失败**的 agent 升级（upgrade_ack ok=false）。
//
// 只改 agent_upgrade：status=failed + 错误短码 + 时刻。**版本列保持旧值** ——
// 节点上的二进制没换，把它写成目标版本会让控制台谎报升级成功。
//
// targetVersion 沿用上一次 sent 写进去的值（ack 里没有它）：先读后写放在同一个
// 事务里，避免与控制台的另一次下发交错时把目标版本写丢。
func (d *DB) FailAgentUpgrade(ctx context.Context, namespaceID, nodeID, errCode string, at int64) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fail agent upgrade begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT agent_upgrade FROM nodes WHERE namespace_id = ? AND id = ?`,
		namespaceID, nodeID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("fail agent upgrade read: %w", err)
	}
	var up AgentUpgrade
	if raw != "" && raw != "{}" {
		// 解不出来就当从未尝试：失败状态本身比「保留一个坏 JSON」更重要。
		_ = json.Unmarshal([]byte(raw), &up)
	}
	up.Status = AgentUpgradeFailed
	up.Error = errCode
	up.At = at
	out, err := json.Marshal(up)
	if err != nil {
		return fmt.Errorf("encode agent upgrade: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET agent_upgrade = ? WHERE namespace_id = ? AND id = ?`,
		string(out), namespaceID, nodeID); err != nil {
		return fmt.Errorf("fail agent upgrade write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fail agent upgrade commit: %w", err)
	}
	return nil
}

// nodeRowsAffected 把「更新了 0 行」翻译成 ErrNotFound（节点行的几个 UPDATE 共用）。
//
// 0 行只可能是「作用域/ id 不对」：静默成功会让一次写错命名空间的调用看起来生效了。
func nodeRowsAffected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows: %w", op, err)
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
// observed 状态标成 applied / pending（发现不一致就要重新下发）。
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
	ids, _, err := d.ReconcileApplied(ctx, namespaceID, nodeID, applied)
	return ids, err
}

// ReconcileApplied 用 hello.applied[] 全量重建这台节点的能力归属（契约 D4）。
//
// 两步，同事务：
//  1. 逐条解析 name@version → capability_id（**只在本节点所在命名空间**内解析）。
//     未定义的那条**不拒绝整帧**，而是作为「陈旧缓存」回给调用方（返回值第 2 项）
//     —— 中枢随后在 welcome 里让节点删掉它。
//  2. DELETE + INSERT 重建 node_capabilities（全量替换语义保留）。
//
// # 为什么未定义的能力不再拒绝整帧（曾经的 unknown_capability）
//
// 节点缓存是**派生物**，中枢是唯一真相。控制台删掉一个能力之后，节点的缓存里
// 必然还留着它 —— 这是正常的管理动作，不是协议违规。旧行为是整帧拒绝：
//
//	hello(applied=[touchnow@1]) → 400 unknown_capability → agent 视为致命错误退出
//	→ systemd 3 秒后重启 → 再拒 → 无限崩溃循环（节点永久离线，日志每 3 秒刷一次）
//
// 也就是说「删一个能力」会打死所有在线节点。现在的行为：认下认识的、把不认识的
// 回给调用方去剪掉（ws 层放进 welcome.prune，agent 删缓存与索引）。
//
// 顺带把 deployments 的 observed 列对账成 applied / pending：
//   - applied：节点缓存里的 digest 与中枢期望一致
//   - pending：不一致 —— 中枢马上会把期望的那份重新下发（中枢覆盖，见 ws.resendDeployments）
//
// **它不会做的事**：不会因为 applied 里出现一条没部署过的能力就给它归属。
// 那会让中枢数据库开始静默撒谎（页面显示「这台机器能跑」，真的派过去却 ack 后 failed）。
// 唯一的归属写者是控制台的部署。
// 返回值：
//   - ids：与 applied 逐个对应（未定义的那条是 ""）
//   - stale：中枢已经没有的能力引用（节点应当剪掉的陈旧缓存）
//
// 只有**形状坏掉**的 applied（空能力名）才返回错误：那不是陈旧缓存，是节点 bug。
func (d *DB) ReconcileApplied(ctx context.Context, namespaceID, nodeID string, applied []AppliedRef) (ids []string, stale []AppliedRef, err error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, nil, ErrNamespaceRequired
	}
	if nodeID == "" {
		return nil, nil, fmt.Errorf("reconcile applied: empty node_id")
	}

	// 节点行是唯一真相：能力定义只在它所属的命名空间里解析（I11③）。
	node, err := d.GetNode(ctx, namespaceID, nodeID)
	if err != nil {
		return nil, nil, err
	}

	ids = make([]string, len(applied))
	var (
		nameProblems []string
		unknownRefs  []string
	)
	for i, a := range applied {
		if a.Name == "" {
			// 形状坏掉 = 节点 bug，不是陈旧缓存：照旧拒绝整帧（这条必须响）。
			nameProblems = append(nameProblems,
				fmt.Sprintf("capability name is empty (version %d)", a.Version))
			continue
		}
		ref := fmt.Sprintf("%s@%d", a.Name, a.Version)
		c, err := d.GetCapability(ctx, node.NamespaceID, a.Name, a.Version)
		switch {
		case errors.Is(err, ErrNotFound):
			// 陈旧缓存：记下来，交给 welcome.prune 让节点删掉，不拒绝整帧。
			unknownRefs = append(unknownRefs, ref)
			stale = append(stale, AppliedRef{Name: a.Name, Version: a.Version, Digest: a.Digest})
		case err != nil:
			return nil, nil, err
		case c.NamespaceID != node.NamespaceID:
			// 数据被写坏时才可能走到：宁可 400 说清楚，也不要把别的命名空间的
			// 能力 id 写进本节点的声明。
			return nil, nil, &CrossNamespaceError{
				What: "node_capabilities.capability_id",
				Want: node.NamespaceID,
				Got:  c.NamespaceID,
			}
		default:
			ids[i] = c.ID
		}
	}
	if len(unknownRefs) > 0 {
		// 只留痕：中枢认得清「这不是我的能力」，节点马上会在 welcome.prune 里收到它。
		slog.Warn("node reports capabilities the hub no longer has (stale cache; pruning)",
			"event", "node.reconcile.stale",
			"node_id", nodeID,
			"stale", strings.Join(unknownRefs, ", "))
	}

	if len(nameProblems) > 0 {
		return nil, nil, &CapabilityDeclError{Problems: nameProblems}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin reconcile applied: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_capabilities WHERE node_id = ?`, nodeID); err != nil {
		return nil, nil, fmt.Errorf("clear node capabilities: %w", err)
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
			return nil, nil, fmt.Errorf("insert node capability: %w", err)
		}
		if _, err := d.markDeployedDigest(ctx, tx, nodeID, ids[i], applied[i].Digest); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit reconcile applied: %w", err)
	}
	return ids, stale, nil
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
// 实际并发由 instances（槽位数）决定，每槽一个任务；node_instances.concurrency
// 与 nodes.max_concurrency 目前只是记录，不参与调度（#220/#252）。
// 这里只做最后一道 CAS 抢占：并发 UPDATE 下只有一个能成功。
// 返回 instance_id；没有空闲槽位返回 ErrNoCapacity。
func (d *DB) LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("lease instance: empty task_id")
	}
	// 先找最早的存活空闲槽位（ORDER BY slot 保证确定性），再由 CAS 更新抢占。
	// 这两步之间没有事务包裹（走连接池），所以必须靠 CAS 兜：并发下可能被别人抢
	// （RowsAffected==0），重试下一轮换下一个空闲槽，结果是不会有两个任务占同一槽位。
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

// LeaseAndAssign 是派发的**一个事务**：占槽位 + pending→assigned + 写归属/attempts + 落令牌（#141 / #264）。
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
//  4. 写 node/instance/lease_until 并 attempts+1，同时落任务令牌（token 为空时才生成，
//     已有则保留——见 assignTaskTx 的 token 段）
//
// 任一步失败都整体回滚——槽位不泄漏，attempts 也不会被提前 +1。
//
// 令牌为什么在这里（#264）：它原来在发布路径上（ws.PublishAssign → EnsureTaskToken），
// 那是**每帧一次独立写事务**，跟这里抢同一把写锁（池=8、`_txlock=immediate`）。
// 折进本事务后派发少一次提交，且令牌与 assigned 同生共死。
//
// 粘性 pin 为什么也在这里（#264）：同理（原来是 PublishAssign 成功之后再单独写一行），
// 现作为可选入参 pin 一起落库；推帧失败的回滚由 requeue 路径负责（条件删除，
// 只删本次新建的那条），见 route.launch 与 rollbackPinAfterPublishFailure 的注释。
func (d *DB) LeaseAndAssign(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64) (string, error) {
	return d.LeaseAndAssignWithPin(ctx, namespaceID, taskID, nodeID, leaseUntil, nil)
}

// LeaseAndAssignWithPin 是 LeaseAndAssign + 「同一个事务里落一条粘性 pin」（#264）。
//
// 为什么要合并：pin 原来在 route 的 launch/redispatch 里**等推帧成功之后**再单写一行
// （每次派发一次独立写事务，跟派发抢同一把写锁）。折进来之后：派发少一次提交，
// 且「任务 assigned」与「钉在哪台机器上」原子成立——不会出现「库里已 assigned、
// 但 pin 还没写」的窗口被并发的下一跳看到（那会让同一 trace 的下一跳按负载换机器，
// sticky 静默失效）。
//
// pin 为 nil 时行为与 LeaseAndAssign 完全一致（非 sticky 能力走的就是这条路）。
//
// 推帧失败怎么办（语义必须不变：推不出去的任务**不能**留下 pin）：
// 回滚发生在 route 的 requeue 路径上，用 RollbackPin 做**条件删除**（只删本次事务
// 新建的那条）。为什么不在"推成功之后才提交 pin"：
//   - 那等于把这次写留在发布路径上，本次合并的收益直接归零；
//   - 失败路径本来就写 3 次（释放槽位 / 过期租约 / 巡检回收），多一次条件 DELETE
//     是罕见路径上可以接受的代价，而成功路径省下的是每一次。
func (d *DB) LeaseAndAssignWithPin(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64, pin *NodePin) (instanceID string, err error) {
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
	// 本进程内经 BEGIN IMMEDIATE 串行化后它仍不可达，但条件必须留着——
	// 连接数已放开到 8，它是并发/跨进程场景下的最后一道闸。
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

	// ⑤ 粘性 pin（可选）：与上面三件事同一个事务。写失败 → 整笔回滚（任务留在
	// pending、槽位不泄漏），比"派发出去了但没钉住"更好查：后者是静默的语义降级。
	if pin != nil {
		if err := putPinExec(ctx, tx, pin); err != nil {
			return "", err
		}
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
		`SELECT sticky_key, namespace_id, node_id, selector, pin_id, created_at, last_used_at
		 FROM node_pins WHERE sticky_key = ?`, stickyKey).
		Scan(&p.StickyKey, &p.NamespaceID, &p.NodeID, &p.Selector, &p.PinID, &p.CreatedAt, &p.LastUsedAt)
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
// 新建时它会就地生成 p.PinID 并写回（行身份，回滚靠它认自己那一行）；
// 已存在的行不会被这次调用改写身份（只有 last_used_at 变），见 putPinExec。
//
// p.NamespaceID 必填（钉住的是「哪个命名空间里的哪台机器」）；
// p.NodeID 必须是该命名空间内的节点。
func (d *DB) PutPin(ctx context.Context, p *NodePin) error {
	return putPinExec(ctx, d, p)
}

// execContexter 是 `*sql.DB` 与 `*sql.Tx` 的公共执行面：
// 让同一条写语句既能独立执行（PutPin），也能落在调用方的事务里（LeaseAndAssignWithPin）。
type execContexter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// putPinExec 是 PutPin 的唯一实现（独立执行与事务内执行共用同一段 SQL/校验）。
//
// p.CreatedAt / p.LastUsedAt / p.PinID 为空时就地补上——**会把值写回调用方的 struct**：
// route 的回滚路径要靠 PinID 做"只删本次新建那条"的条件删除（见 RollbackPin）。
// 已存在的 pin 走 ON CONFLICT 分支：只刷新 last_used_at，created_at 与 pin_id 都保持
// 原样（钉住不换机器，行身份也不换主人）——所以那条条件不会命中旧行。
func putPinExec(ctx context.Context, ex execContexter, p *NodePin) error {
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
	if p.PinID == "" {
		p.PinID = NewPinID()
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO node_pins (sticky_key, namespace_id, node_id, selector, pin_id, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(sticky_key) DO UPDATE SET last_used_at = excluded.last_used_at`,
		p.StickyKey, p.NamespaceID, p.NodeID, p.Selector, p.PinID, p.CreatedAt, p.LastUsedAt); err != nil {
		return fmt.Errorf("put pin: %w", err)
	}
	return nil
}

// RollbackPin 撤掉一条**本次派发新建的**粘性 pin（推帧失败的回滚，#264）。
//
// 判据是 pin_id 全等（而非按 sticky_key 删，也不再比秒级的 created_at）：
// pin 可能是**更早**的派发留下的（同一 trace 的上一跳），或者并发派发刚建的；
// 只按 key 删会把别人的钉住一起清掉，那是「一次推送失败让整条 trace 的粘性失效」。
//   - 已存在的行：pin_id 是**建它那次**派发的（putPinExec 的 ON CONFLICT 只刷
//     last_used_at，不覆盖 pin_id）→ 条件不命中 → 不动；
//   - 本次新建的行：pin_id 就是 putPinExec 写进 p.PinID 的那个值 → 删掉。
//
// 为什么不能继续用 created_at 做第三列（#315）：它是**秒级**的。同一秒里同一
// (key, node) 上先后两次派发会拿到相同的 created_at，于是一次回滚能删掉**别人
// 刚建的那一行**。pin_id 是毫秒时间有序 + 随机的真唯一值，不受秒级粒度影响。
// node_id 仍留在条件里：它让「同 key 但机器不同」的病态数据也不会被误删。
//
// 返回是否真的删了一行（deleted=false 表示"这条 pin 本来就不属于本次派发"）。
func (d *DB) RollbackPin(ctx context.Context, stickyKey, nodeID, pinID string) (deleted bool, err error) {
	if stickyKey == "" || nodeID == "" || pinID == "" {
		return false, fmt.Errorf("rollback pin: sticky_key/node_id/pin_id required")
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM node_pins WHERE sticky_key = ? AND node_id = ? AND pin_id = ?`,
		stickyKey, nodeID, pinID)
	if err != nil {
		return false, fmt.Errorf("rollback pin: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rollback pin rows: %w", err)
	}
	return n > 0, nil
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
