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

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 节点领域（v4，anode.md §0/§2/§3.3/§6，api-conventions §7.4）。
//
// 节点是**集群级**对象：不绑命名空间。一台机器能服务哪些 ns 由两份列表的交集回答：
//
//	node_configs.namespaces      管理员授权（缺省 ["*"]）
//	nodes.declared_namespaces    本机申报（hello 上报，缺省 ["default"]）
//
// 生效 = 申报 ∩ 授权（权限只能收紧，不做「本地优先」）。节点的身份令牌不绑 ns。
// 执行资格由 hello 上报的事实（facts / declared_capabilities / appliedRevision）
// 与中枢配置（node_configs.revision）+ 部署事实共同决定：见 EligibleNodes。

// 节点状态（api-conventions §7.4 / anode.md §0）。
const (
	NodeStatusOnline   = "online"
	NodeStatusOffline  = "offline"
	NodeStatusDraining = "draining"
)

// 节点默认硬顶（schema 默认值，api-conventions §7.4）。
const (
	DefaultMaxInstances   = 20
	DefaultMaxConcurrency = 8
)

// NodeNameMaxLen 是节点名的长度上限（1..63，与能力/ns 名同口径的字符集）。
const NodeNameMaxLen = 63

// Node 是 nodes 表的一行（一台机器）。
//
// TokenHash 是 sha256(token)，明文只在创建时返回一次，所以它带 `json:"-"`，
// 任何 API 响应都不会泄漏哈希。Facts / DeclaredCapabilities / DeclaredNamespaces
// 都是 hello 上报的**事实快照**：中枢不据此推断，只据此匹配与展示。
type Node struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
	// Status ∈ online / offline / draining。
	Status         string `json:"status"`
	TokenHash      string `json:"-"`
	MaxInstances   int    `json:"maxInstances"`
	MaxConcurrency int    `json:"maxConcurrency"`
	// AgentVersion / AgentCommit 是节点上跑的那个 anode 的版本信息（空串 = 未知）。
	// AgentVersion/AgentCommit 由 hello 自报（D-I13）；两者都是事实，不参与 digest。
	AgentVersion string `json:"agentVersion"`
	AgentCommit  string `json:"agentCommit"`
	// AgentUpgrade 是最近一次升级尝试的状态（零值 = 从未尝试）。
	AgentUpgrade AgentUpgrade `json:"agentUpgrade"`
	LastSeenAt   *int64       `json:"lastSeenAt"`
	CreatedAt    int64        `json:"createdAt"`
	// ResourceVersion 是节点对象的写并发令牌（配置另有 node_configs.resource_version）。
	ResourceVersion int64 `json:"resourceVersion"`
	// Facts 是 hello.facts（hostIP/hostname/os/arch/cpu/workRoot/homeDir）。
	Facts NodeFacts `json:"facts"`
	// DeclaredCapabilities 是 hello.capabilities[]：能力名 + 命令探测结果 + 持有版本。
	DeclaredCapabilities []DeclaredCapability `json:"declaredCapabilities"`
	// DeclaredNamespaces 是本机申报的 ns 列表（ns 名或 "*"）。
	DeclaredNamespaces []string `json:"declaredNamespaces"`
}

// 升级尝试的状态（nodes.agent_upgrade 的 status）。
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

// AgentUpgrade 是一次升级尝试的结果（落库形状 = 出线形状）。
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
// 槽位是**机器级**的（一台机器上的并发预算跨 ns 共享），所以没有 namespace_id。
type NodeInstance struct {
	ID          string  `json:"id"`
	NodeID      string  `json:"nodeId"`
	Slot        int     `json:"slot"`
	Concurrency int     `json:"concurrency"`
	BusyTask    *string `json:"busyTask"`
	Alive       bool    `json:"alive"`
	UpdatedAt   int64   `json:"updatedAt"`
}

// 节点注册 / 派发相关的哨兵错误。
var (
	// ErrTooManyInstances 申报实例数超过 nodes.max_instances 硬顶。
	ErrTooManyInstances = errors.New("too_many_instances")
	// ErrNoCapacity 节点没有空闲并发槽位。
	ErrNoCapacity = errors.New("no_capacity")
	// ErrNodeDraining 节点在排空（不再接新任务）。
	ErrNodeDraining = errors.New("node_draining")
	// ErrNodeExists 节点重名（集群内唯一，HTTP 409）。
	ErrNodeExists = errors.New("store: node name already exists")
)

// InstanceSpec 是节点申报的并发槽位（hello.instances[]，D-I13）。
type InstanceSpec struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// NewNodeToken 生成节点 opaque token：ant_node_<base64url(24B)>
// （与既有 api_keys 形态一致）。返回 (明文, sha256 哈希)。
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

// nodeNamePattern 是节点名的合法形状：1..63 个字符，只允许小写字母、数字、
// 点、下划线、短横线，且必须以字母或数字开头（anode.md §2）。
var nodeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateNodeName 校验节点名："" 表示合法，否则是给用户看的中文错误（含 "name"）。
//
// 规则严格按 nodeNamePattern，**不 trim**：带空白的输入必须当场拒绝，而不是静默改对后落库。
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

// nodeColumns 是统一的列顺序，scanNodeRow 依赖它（与 schema.sql 的 nodes 定义同序）。
const nodeColumns = `id, name, tags, status, token_hash, max_instances, max_concurrency,
	last_seen_at, created_at, agent_version, agent_commit, agent_upgrade,
	facts, declared_capabilities, declared_namespaces, resource_version`

// scanNodeRow 从 rows 解析一行。
func scanNodeRow(rows *sql.Rows) (*Node, error) {
	var (
		n                         Node
		tags, agentUpgrade, facts string
		declaredCaps, declaredNS  string
	)
	if err := rows.Scan(&n.ID, &n.Name, &tags, &n.Status, &n.TokenHash,
		&n.MaxInstances, &n.MaxConcurrency, &n.LastSeenAt, &n.CreatedAt,
		&n.AgentVersion, &n.AgentCommit, &agentUpgrade,
		&facts, &declaredCaps, &declaredNS, &n.ResourceVersion); err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &n.Tags); err != nil {
		return nil, fmt.Errorf("decode node tags: %w", err)
	}
	if n.Tags == nil {
		n.Tags = []string{}
	}
	// 空串 = 早期行的防御（列默认 '{}'）→ 零值 = 从未尝试。
	if agentUpgrade != "" && agentUpgrade != "{}" {
		if err := json.Unmarshal([]byte(agentUpgrade), &n.AgentUpgrade); err != nil {
			return nil, fmt.Errorf("decode node agent upgrade: %w", err)
		}
	}
	if strings.TrimSpace(facts) != "" && facts != "{}" {
		if err := json.Unmarshal([]byte(facts), &n.Facts); err != nil {
			return nil, fmt.Errorf("decode node facts: %w", err)
		}
	}
	if strings.TrimSpace(declaredCaps) != "" {
		if err := json.Unmarshal([]byte(declaredCaps), &n.DeclaredCapabilities); err != nil {
			return nil, fmt.Errorf("decode node declared capabilities: %w", err)
		}
	}
	if n.DeclaredCapabilities == nil {
		n.DeclaredCapabilities = []DeclaredCapability{}
	}
	for i := range n.DeclaredCapabilities {
		if n.DeclaredCapabilities[i].Commands == nil {
			n.DeclaredCapabilities[i].Commands = []protocol.HelloCommand{}
		}
	}
	namespaces, err := decodeStringList(declaredNS)
	if err != nil {
		return nil, fmt.Errorf("decode node declared namespaces: %w", err)
	}
	n.DeclaredNamespaces = namespaces
	return &n, nil
}

// CreateNode 创建节点，返回**只出现一次**的明文 token。
//
// n.TokenHash 可预先由调用方设置（测试注入已知 token）；为空时自动生成。
// n.Name 集群内唯一（UNIQUE(name)）→ 撞名返回 ErrNodeExists。
func (d *DB) CreateNode(ctx context.Context, n *Node) (token string, err error) {
	if n == nil {
		return "", fmt.Errorf("create node: nil")
	}
	if strings.TrimSpace(n.Name) == "" {
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
	if n.ResourceVersion == 0 {
		n.ResourceVersion = 1
	}
	if n.Tags == nil {
		n.Tags = []string{}
	}
	if n.DeclaredCapabilities == nil {
		n.DeclaredCapabilities = []DeclaredCapability{}
	}
	if n.DeclaredNamespaces == nil {
		n.DeclaredNamespaces = []string{}
	}
	tagsJSON, err := json.Marshal(n.Tags)
	if err != nil {
		return "", fmt.Errorf("marshal node tags: %w", err)
	}
	factsJSON, err := json.Marshal(n.Facts)
	if err != nil {
		return "", fmt.Errorf("marshal node facts: %w", err)
	}
	capsJSON, err := json.Marshal(n.DeclaredCapabilities)
	if err != nil {
		return "", fmt.Errorf("marshal node declared capabilities: %w", err)
	}
	namespacesJSON, err := marshalStringList(n.DeclaredNamespaces)
	if err != nil {
		return "", err
	}
	if n.ID == "" {
		n.ID = NewNodeID()
	}
	upgradeJSON, err := json.Marshal(n.AgentUpgrade)
	if err != nil {
		return "", fmt.Errorf("marshal node agent upgrade: %w", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO nodes
		   (id, name, tags, status, token_hash, max_instances, max_concurrency,
		    last_seen_at, created_at, agent_version, agent_commit, agent_upgrade,
		    facts, declared_capabilities, declared_namespaces, resource_version)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.Name, string(tagsJSON), n.Status, n.TokenHash,
		n.MaxInstances, n.MaxConcurrency, n.LastSeenAt, n.CreatedAt,
		n.AgentVersion, n.AgentCommit, string(upgradeJSON),
		string(factsJSON), string(capsJSON), namespacesJSON, n.ResourceVersion); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %q", ErrNodeExists, n.Name)
		}
		return "", fmt.Errorf("insert node: %w", err)
	}
	return token, nil
}

// GetNode 按 id 取节点；未命中返回 ErrNotFound。
func (d *DB) GetNode(ctx context.Context, nodeID string) (*Node, error) {
	if nodeID == "" {
		return nil, ErrNotFound
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, nodeID)
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

// GetNodeByName 按集群内唯一名取节点；未命中返回 ErrNotFound。
func (d *DB) GetNodeByName(ctx context.Context, name string) (*Node, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrNotFound
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE name = ?`, strings.TrimSpace(name))
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

// GetNodeByTokenHash 按 token sha256 取节点（握手鉴权用）。
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

// ListNodes 列出集群内全部节点（空列表返回 []）。
func (d *DB) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes ORDER BY name, id`)
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
func (d *DB) UpdateNodeStatus(ctx context.Context, nodeID, status string) error {
	switch status {
	case NodeStatusOnline, NodeStatusOffline, NodeStatusDraining:
	default:
		return fmt.Errorf("update node status: invalid status %q", status)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET status = ?, last_seen_at = ?, resource_version = resource_version + 1
		  WHERE id = ?`, status, Now(), nodeID)
	if err != nil {
		return fmt.Errorf("update node status: %w", err)
	}
	return nodeRowsAffected(res, "update node status")
}

// nodeRowsAffected 把「更新了 0 行」翻译成 ErrNotFound（节点行的几个 UPDATE 共用）。
//
// 0 行只可能是 id 不对：静默成功会让一次写错 id 的调用看起来生效了。
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

// TouchNode 刷新 last_seen_at（任何上行帧都续命）。
func (d *DB) TouchNode(ctx context.Context, nodeID string, at int64) error {
	if at == 0 {
		at = Now()
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET last_seen_at = ? WHERE id = ?`, at, nodeID)
	if err != nil {
		return fmt.Errorf("touch node: %w", err)
	}
	return nodeRowsAffected(res, "touch node")
}

// UpdateNodeAgentVersion 记录节点自报的 agent 版本（hello.agentVersion，D-I13）。
//
// 只写 version，**不碰** agent_commit：hello 帧里没有 commit，而 commit 是
// upgrade_ack 的产物（见 RecordAgentUpgrade）。空串是事实（未知），不保留上一版的旧值。
func (d *DB) UpdateNodeAgentVersion(ctx context.Context, nodeID, version string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_version = ?, resource_version = resource_version + 1
		  WHERE id = ?`, version, nodeID)
	if err != nil {
		return fmt.Errorf("update node agent version: %w", err)
	}
	return nodeRowsAffected(res, "update node agent version")
}

// RecordNodeFacts 记录节点上报的内置事实（hello.facts，D-I13）。
func (d *DB) RecordNodeFacts(ctx context.Context, nodeID string, facts NodeFacts) error {
	raw, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("encode node facts: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET facts = ?, resource_version = resource_version + 1
		  WHERE id = ?`, string(raw), nodeID)
	if err != nil {
		return fmt.Errorf("record node facts: %w", err)
	}
	return nodeRowsAffected(res, "record node facts")
}

// MarkAgentUpgradeSent 记录一次已下发的升级尝试（agent_upgrade.status=sent）。
//
// 只写 agent_upgrade，不碰版本列 —— 帧刚推出去，节点还没换二进制；
// 版本列要等 upgrade_ack 才是事实（提前改成目标版本就是谎报成功）。
func (d *DB) MarkAgentUpgradeSent(ctx context.Context, nodeID, targetVersion string, at int64) error {
	up := AgentUpgrade{Status: AgentUpgradeSent, TargetVersion: targetVersion, At: at}
	raw, err := json.Marshal(up)
	if err != nil {
		return fmt.Errorf("encode agent upgrade: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_upgrade = ?, resource_version = resource_version + 1
		  WHERE id = ?`, string(raw), nodeID)
	if err != nil {
		return fmt.Errorf("mark agent upgrade sent: %w", err)
	}
	return nodeRowsAffected(res, "mark agent upgrade sent")
}

// RecordAgentUpgrade 记录一次**成功**的 agent 升级（upgrade_ack ok=true）。
//
// 一个 UPDATE 同时写三样：agent_version / agent_commit / agent_upgrade(status=ok)。
// 版本与状态必须一起变，否则会列出「agentVersion=新版本、agentUpgrade 还是 sent」。
func (d *DB) RecordAgentUpgrade(ctx context.Context, nodeID, version, commit string, at int64) error {
	up := AgentUpgrade{Status: AgentUpgradeOK, TargetVersion: version, At: at}
	raw, err := json.Marshal(up)
	if err != nil {
		return fmt.Errorf("encode agent upgrade: %w", err)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET agent_version = ?, agent_commit = ?, agent_upgrade = ?,
		        resource_version = resource_version + 1
		  WHERE id = ?`, version, commit, string(raw), nodeID)
	if err != nil {
		return fmt.Errorf("record node agent upgrade: %w", err)
	}
	return nodeRowsAffected(res, "record node agent upgrade")
}

// FailAgentUpgrade 记录一次**失败**的 agent 升级（upgrade_ack ok=false）。
//
// 只改 agent_upgrade：status=failed + 错误短码 + 时刻。**版本列保持旧值** ——
// 节点上的二进制没换，把它写成目标版本会让控制台谎报升级成功。
// targetVersion 沿用上一次 sent 写进去的值（ack 里没有它），先读后写在同一事务。
func (d *DB) FailAgentUpgrade(ctx context.Context, nodeID, errCode string, at int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fail agent upgrade begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT agent_upgrade FROM nodes WHERE id = ?`, nodeID).Scan(&raw)
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
		`UPDATE nodes SET agent_upgrade = ?, resource_version = resource_version + 1
		  WHERE id = ?`, string(out), nodeID); err != nil {
		return fmt.Errorf("fail agent upgrade write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fail agent upgrade commit: %w", err)
	}
	return nil
}

// SetNodeTokenHash 覆盖节点的 token 哈希（仅用于开发工具重建节点凭据）。
//
// 明文 token 只在 CreateNode 时返回一次，之后无法复原；devseed 反复跑需要一个
// 「同一个节点、换一把钥匙」的入口，否则只能不断新建节点行。
func (d *DB) SetNodeTokenHash(ctx context.Context, nodeID, tokenHash string) error {
	if nodeID == "" || tokenHash == "" {
		return fmt.Errorf("set node token hash: empty node_id or hash")
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET token_hash = ?, resource_version = resource_version + 1
		  WHERE id = ?`, tokenHash, nodeID)
	if err != nil {
		return fmt.Errorf("update node token hash: %w", err)
	}
	return nodeRowsAffected(res, "set node token hash")
}

// DeleteNode 删除一台节点（槽位 / 节点配置 / 部署随 FK 级联删除）。
func (d *DB) DeleteNode(ctx context.Context, nodeID string) error {
	res, err := d.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	return nodeRowsAffected(res, "delete node")
}

// UpsertInstances 落库节点申报的实例槽位；超过 nodes.max_instances 返回 ErrTooManyInstances。
//
// 语义：节点每次 hello 申报的是一份**完整清单**，多余的老槽位删除（节点缩容）。
// slot 必须唯一且 ≥ 0，concurrency 至少 1；已有槽位保留 busy_task（重启不该把在跑的任务丢掉）。
func (d *DB) UpsertInstances(ctx context.Context, nodeID string, specs []InstanceSpec) ([]*NodeInstance, error) {
	node, err := d.GetNode(ctx, nodeID)
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
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_instances (id, node_id, slot, concurrency, busy_task, alive, updated_at)
			 VALUES (?,?,?,?,NULL,1,?)
			 ON CONFLICT(node_id, slot) DO UPDATE SET
			   concurrency = excluded.concurrency,
			   alive = 1,
			   updated_at = excluded.updated_at`,
			NewInstanceID(), nodeID, s.Slot, conc, now); err != nil {
			return nil, fmt.Errorf("upsert instance slot %d: %w", s.Slot, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit upsert instances: %w", err)
	}
	return d.ListInstances(ctx, nodeID)
}

// ListInstances 列出节点槽位（空列表返回 []）。
func (d *DB) ListInstances(ctx context.Context, nodeID string) ([]*NodeInstance, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, node_id, slot, concurrency, busy_task, alive, updated_at
		 FROM node_instances WHERE node_id = ? ORDER BY slot`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	out := []*NodeInstance{}
	for rows.Next() {
		var in NodeInstance
		var alive int
		if err := rows.Scan(&in.ID, &in.NodeID, &in.Slot, &in.Concurrency,
			&in.BusyTask, &alive, &in.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		in.Alive = alive != 0
		out = append(out, &in)
	}
	return out, rows.Err()
}

// FreeSlotsByNode 一次查出多个节点的空闲槽位数（LEFT JOIN：没申报槽位的节点给 0）。
//
// 口径：**alive 且 busy_task IS NULL** 的槽位算空闲（与 ListInstances 一致）。
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

// MarkInstancesDead 把节点全部实例标记为掉线（连接断开时调用）。
//
// 只是标记 alive=0，不删行：槽位申报是节点的事，中枢不替它决定「少了一个槽」。
// 也不清 busy_task——任务归属由租约巡检回收，那是唯一入口。
func (d *DB) MarkInstancesDead(ctx context.Context, nodeID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET alive = 0, updated_at = ? WHERE node_id = ?`,
		Now(), nodeID); err != nil {
		return fmt.Errorf("mark instances dead: %w", err)
	}
	return nil
}

// ReclaimStaleSlots 清掉「槽位指向的任务已经不在这台机器上跑」的绑定（兜底）。
//
// 中枢在「已派发、未回收」的窗口里被 kill -9 时没人清 busy_task，槽位会永久卡住。
// 节点每次 hello 时做一次幂等修复：**只清**那种任务已经不归本节点在跑（任务行不存在、
// 已回 pending、已终态、已被改派）的槽位；仍在 running 且属于本节点的任务一行都不动。
//
// v4 语义：槽位只在 `cancel.ack.processGroupReaped=true`（或租约回收）之后释放；
// 这里清掉的只是「任务已不在 running」的残影，不抢跑租约巡检。
func (d *DB) ReclaimStaleSlots(ctx context.Context, nodeID string, now int64) (int, error) {
	if now == 0 {
		now = Now()
	}
	res, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		  WHERE node_id = ? AND busy_task IS NOT NULL
		    AND NOT EXISTS (
		      SELECT 1 FROM tasks t
		       WHERE t.id = node_instances.busy_task
		         AND t.node_id = node_instances.node_id
		         AND t.phase = ?)`,
		now, nodeID, string(protocol.TaskPhaseRunning))
	if err != nil {
		return 0, fmt.Errorf("reclaim stale slots: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale slots rows: %w", err)
	}
	return int(n), nil
}

// LeaseSlot 把任务占到一个空闲槽位上（CAS：busy_task IS NULL → taskID）。
//
// 占槽与任务绑定是同一件事：busy_task 记录「这个槽位现在归哪条任务」。
// 取消/失败**不在这里释放**——要等节点回报进程组真正回收（`cancel.ack`）或租约回收，
// 否则数据库改终态就当节点空闲，会让同一槽位同时跑两条任务。
//
// 返回 instance_id；没有空闲槽位返回 ErrNoCapacity。
func (d *DB) LeaseSlot(ctx context.Context, nodeID, taskID string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("lease slot: empty task_id")
	}
	// 先找最早的存活空闲槽位（ORDER BY slot 保证确定性），再由 CAS 更新抢占。
	// 两步之间没有事务包裹，所以必须靠 CAS 兜：并发下可能被别人抢
	// （RowsAffected==0），重试下一轮换下一个空闲槽。
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
			return "", fmt.Errorf("lease slot: find free instance: %w", err)
		}
		res, err := d.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = ?, updated_at = ?
			 WHERE id = ? AND busy_task IS NULL`, taskID, Now(), instanceID)
		if err != nil {
			return "", fmt.Errorf("lease slot: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("lease slot rows: %w", err)
		}
		if n == 1 {
			return instanceID, nil
		}
		// CAS 失败：被另一个写者抢了，重试找下一个空闲槽。
	}
	return "", ErrNoCapacity
}

// ReleaseSlot 释放实例占用（节点回报进程组已回收 / 租约回收时调用）。
func (d *DB) ReleaseSlot(ctx context.Context, nodeID, instanceID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND id = ? AND busy_task IS NOT NULL`, Now(), nodeID, instanceID); err != nil {
		return fmt.Errorf("release slot: %w", err)
	}
	return nil
}

// ReleaseSlotByTask 按任务 ID 释放它占用的槽位（不知道 instance_id 时用）。
func (d *DB) ReleaseSlotByTask(ctx context.Context, nodeID, taskID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND busy_task = ?`, Now(), nodeID, taskID); err != nil {
		return fmt.Errorf("release slot by task: %w", err)
	}
	return nil
}

// InstanceStat 是一台节点的槽位汇总（只读投影，供批量视图用）。
//
// 口径与 ListInstances 完全一致：Total 是**行数**（不看 alive —— 掉线的槽位
// 仍然是节点申报过的槽位，控制台页面上要显示它）。
type InstanceStat struct {
	Total int // node_instances 行数（与 ListInstances 同口径：不看 alive）
	Busy  int // busy_task IS NOT NULL 的行数
}

// nodeIDBatchSize 是一条 IN 查询里最多塞多少个 node_id（SQLite 宿主参数上限）。
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

// InstanceStatsByNode 批量取多台节点的槽位总数/占用数（替代每节点一次 ListInstances）。
//
// 没有槽位的节点**不出现在** map 里（调用方按零值处理）；nodeIDs 为空 → 空 map。
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

// nodeConfigSnapshot 是 EligibleNodes 用到的节点配置投影（不含本机快照的原始列）。
type nodeConfigSnapshot struct {
	namespaces      []string
	vars            map[string]string
	localVars       map[string]string
	revision        int64
	appliedRevision int64
}

// EligibleNodes 返回能执行 (namespace, capability@digest) 的节点，以及**逐节点**不可用原因。
//
// 判定条件（capability.md §2.4 NodesAvailable）：在线、ns ∈ 申报 ∩ 授权、
// 机器级配置已 applied（node_configs.applied_revision == revision）、
// deployments 有该 digest（applied）、hello 申报的对应能力 commands 全部 found、
// spec.requires.values 全部可解析（local > node > 任务 ns）。
//
// 返回的 reasons 是 nodeID → reason 的**全集**（"" = 可用），reason 取
// protocol.ConditionReason* 的封闭值（NodeOffline / NamespaceUnauthorized /
// ConfigPending / NotDeployed / CommandMissing / ValueMissing）。没有节点时 map 为空，
// 调用方按 NoMatchingNode 处理。**无候选不是 error**：它是正常的派发结果。
func (d *DB) EligibleNodes(ctx context.Context, namespaceID string, spec artifact.Spec, digest string) ([]*Node, map[string]string, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, nil, ErrNamespaceRequired
	}
	if strings.TrimSpace(digest) == "" {
		return nil, nil, fmt.Errorf("eligible nodes: empty digest")
	}

	// 只读路径**不**开事务：DSN 是 `_txlock=immediate`，读事务会先拿写锁，
	// 把并发派发串行化。这里每条查询各自读最新快照——最坏情况只是某个 reason
	// 用到了另一秒的配置，不会伪造出「可派发」（保守方向的偏差）。

	// ① ns 名（授权/申报列表存的是 ns 名，不是内部 id）。
	var namespaceName string
	err := d.QueryRowContext(ctx, `SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&namespaceName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNamespaceNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: read namespace: %w", err)
	}

	// ② 能力身份（name@version）由 digest 反查：digest 是部署身份，每 ns 唯一。
	var capName string
	var capVersion int
	err = d.QueryRowContext(ctx,
		`SELECT name, version FROM capability_versions WHERE namespace_id = ? AND digest = ?`,
		namespaceID, digest).Scan(&capName, &capVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: read capability: %w", err)
	}

	// ③ 任务 ns 的默认值（第三层）。
	nsVars := map[string]string{}
	var rawNSVars string
	qerr := d.QueryRowContext(ctx,
		`SELECT vars FROM namespace_configs WHERE namespace_id = ?`, namespaceID).Scan(&rawNSVars)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("eligible nodes: read namespace config: %w", qerr)
	}
	if qerr == nil {
		if nsVars, err = decodeStringMap(rawNSVars); err != nil {
			return nil, nil, fmt.Errorf("eligible nodes: %w", err)
		}
	}

	// ④ 全部节点（集群级：没有「本 ns 的节点」这种过滤，判定交给逐节点 reason）。
	nodes := []*Node{}
	rows, err := d.queryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY name, id`)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: list nodes: %w", err)
	}
	for rows.Next() {
		n, serr := scanNodeRow(rows)
		if serr != nil {
			rows.Close()
			return nil, nil, serr
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("eligible nodes: iterate nodes: %w", err)
	}
	rows.Close()

	// ⑤ 逐节点配置快照（一次读完，避免 N+1）。
	configs := map[string]nodeConfigSnapshot{}
	crows, err := d.queryContext(ctx,
		`SELECT node_id, namespaces, vars, local_vars, revision, applied_revision
		   FROM node_configs`)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: read node configs: %w", err)
	}
	for crows.Next() {
		var (
			nodeID, rawNamespaces, rawVars, rawLocalVars string
			revision, appliedRevision                    int64
		)
		if err := crows.Scan(&nodeID, &rawNamespaces, &rawVars, &rawLocalVars,
			&revision, &appliedRevision); err != nil {
			crows.Close()
			return nil, nil, fmt.Errorf("eligible nodes: scan node config: %w", err)
		}
		snap := nodeConfigSnapshot{revision: revision, appliedRevision: appliedRevision}
		if snap.namespaces, err = decodeStringList(rawNamespaces); err != nil {
			crows.Close()
			return nil, nil, fmt.Errorf("eligible nodes: %w", err)
		}
		if snap.vars, err = decodeStringMap(rawVars); err != nil {
			crows.Close()
			return nil, nil, fmt.Errorf("eligible nodes: %w", err)
		}
		if snap.localVars, err = decodeStringMap(rawLocalVars); err != nil {
			crows.Close()
			return nil, nil, fmt.Errorf("eligible nodes: %w", err)
		}
		configs[nodeID] = snap
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, nil, fmt.Errorf("eligible nodes: iterate node configs: %w", err)
	}
	crows.Close()

	// ⑥ 部署事实：哪些节点持有这个 digest（state=applied 才算「持有」）。
	//
	// **pending 不算候选**（G1-4 实现裁决）：pending 的含义是「中枢希望这台机器持有，
	// 但还没收到确认」——此时节点上多半还没有这份字节，派过去只会换来一次注定失败的
	// 执行（消耗的是有限的 attempt 预算，且对非幂等能力是永久损失）。
	// 正确顺序是：deploy 收敛成 applied → 下一轮 EligibleNodes 自然把该节点纳入候选。
	// `state=local` 是 v3 的历史态，v4 不再产生；旧行按 pending 处理（下一次对账会收敛）。
	deployed := map[string]bool{}
	drows, err := d.queryContext(ctx,
		`SELECT node_id FROM deployments
		  WHERE namespace_id = ? AND digest = ? AND state = ?`,
		namespaceID, digest, DeploymentApplied)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: read deployments: %w", err)
	}
	for drows.Next() {
		var nodeID string
		if err := drows.Scan(&nodeID); err != nil {
			drows.Close()
			return nil, nil, fmt.Errorf("eligible nodes: scan deployment: %w", err)
		}
		deployed[nodeID] = true
	}
	if err := drows.Err(); err != nil {
		drows.Close()
		return nil, nil, fmt.Errorf("eligible nodes: iterate deployments: %w", err)
	}
	drows.Close()

	eligible := []*Node{}
	reasons := make(map[string]string, len(nodes))
	for _, n := range nodes {
		cfg, hasCfg := configs[n.ID]
		if !hasCfg {
			cfg = nodeConfigSnapshot{
				namespaces: []string{NamespaceWildcard},
				vars:       map[string]string{},
				localVars:  map[string]string{},
			}
		}
		reason := nodeIneligibilityReason(n, cfg, nsVars, namespaceName, deployed[n.ID], capName, capVersion, spec)
		reasons[n.ID] = reason
		if reason == "" {
			eligible = append(eligible, n)
		}
	}
	return eligible, reasons, nil
}

// nodeIneligibilityReason 判定一台节点是否可执行；"" = 可用。
//
// 优先级按「最具体的事实优先」：离线 > 授权 > 配置未 apply > 未部署 > 缺命令 > 缺值。
// 同时存在离线与缺值时先报 NodeOffline（anode.md §6 的裁定）。
func nodeIneligibilityReason(n *Node, cfg nodeConfigSnapshot, nsVars map[string]string,
	namespaceName string, deployed bool, capName string, capVersion int, spec artifact.Spec) string {
	if n.Status != NodeStatusOnline {
		return protocol.ConditionReasonNodeOffline
	}
	if !namespacesAllow(intersectNamespaces(cfg.namespaces, n.DeclaredNamespaces), namespaceName) {
		return protocol.ConditionReasonNamespaceUnauthorized
	}
	if cfg.appliedRevision != cfg.revision {
		return protocol.ConditionReasonConfigPending
	}
	if !deployed {
		return protocol.ConditionReasonNotDeployed
	}
	if commandMissing(n.DeclaredCapabilities, capName, capVersion, spec.Requires.Commands) {
		return protocol.ConditionReasonCommandMissing
	}
	for _, raw := range spec.Requires.Values {
		name, ok := varRefName(raw)
		if !ok {
			if strings.HasPrefix(strings.TrimSpace(raw), "vars.") {
				return protocol.ConditionReasonValueMissing
			}
			continue // 内置事实（status./metadata.）不参与匹配
		}
		_, localHit := cfg.localVars[name]
		_, nodeHit := cfg.vars[name]
		_, nsHit := nsVars[name]
		if !localHit && !nodeHit && !nsHit {
			return protocol.ConditionReasonValueMissing
		}
	}
	return ""
}

// commandMissing 判定节点申报的命令探测里是否缺了能力声明的命令。
//
// 节点没有申报这个能力（name@version）时：要求为空 = 不用命令 → 不算缺；
// 有要求但探测结果未知 → 按缺处理（宁可少派，也不派给没探明命令的机器）。
func commandMissing(declared []DeclaredCapability, capName string, capVersion int, required []string) bool {
	if len(required) == 0 {
		return false
	}
	var probe *DeclaredCapability
	for i := range declared {
		if declared[i].Name == capName && declared[i].Version == capVersion {
			probe = &declared[i]
			break
		}
	}
	if probe == nil {
		return true
	}
	found := make(map[string]bool, len(probe.Commands))
	for _, c := range probe.Commands {
		found[c.Name] = c.Found
	}
	for _, want := range required {
		if !found[want] {
			return true
		}
	}
	return false
}

// NodeAvailabilityReason 把 EligibleNodes 的逐节点原因汇总成 NodesAvailable condition 的 reason。
//
// 返回 "" = 至少一台可用（condition True / reason Valid）；
// 否则取**最具体**的那一个（anode.md §6：同时存在离线与解析不出值时先报 NodeOffline）：
// NodeOffline > NamespaceUnauthorized > ConfigPending > NotDeployed > CommandMissing > ValueMissing。
// 一个节点都没有（reasons 为空）→ NoMatchingNode（capability.md §2.4 的兜底 reason）。
func NodeAvailabilityReason(reasons map[string]string) string {
	precedence := []string{
		protocol.ConditionReasonNodeOffline,
		protocol.ConditionReasonNamespaceUnauthorized,
		protocol.ConditionReasonConfigPending,
		protocol.ConditionReasonNotDeployed,
		protocol.ConditionReasonCommandMissing,
		protocol.ConditionReasonValueMissing,
	}
	sawEligible := false
	seen := make(map[string]bool, len(precedence))
	for _, reason := range reasons {
		if reason == "" {
			sawEligible = true
			continue
		}
		seen[reason] = true
	}
	if sawEligible {
		return ""
	}
	for _, reason := range precedence {
		if seen[reason] {
			return reason
		}
	}
	return protocol.ConditionReasonNoMatchingNode
}
