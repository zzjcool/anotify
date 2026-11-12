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
// 每槽最多一个能力进程（v4 不再有 per-slot concurrency 乘数：容量 = 真实 slot 记录，
// 预算 = 机器级 limits）。
// BusyTask 是占用它的逻辑任务；BusyAttempt / BusyLeaseID 是具体那一代执行
// （task_attempts 的 attempt 与它那次 grant 的 lease）：同一逻辑任务换 attempt
// 重试时可能同时占着旧 attempt 的槽，释放必须按这三列精确判定。
type NodeInstance struct {
	ID          string  `json:"id"`
	NodeID      string  `json:"nodeId"`
	Slot        int     `json:"slot"`
	BusyTask    *string `json:"busyTask"`
	BusyAttempt int     `json:"busyAttempt"`
	BusyLeaseID string  `json:"busyLeaseId"`
	Alive       bool    `json:"alive"`
	UpdatedAt   int64   `json:"updatedAt"`
}

// 节点注册 / 派发相关的哨兵错误。
var (
	// ErrTooManyInstances 申报实例数超过 nodes.max_instances 硬顶。
	ErrTooManyInstances = errors.New("too_many_instances")
	// ErrNoCapacity 节点没有任何可用实例槽位（原始绑定原语用；grant 路径用 ErrSlotUnavailable）。
	ErrNoCapacity = errors.New("no_capacity")
	// ErrSlotUnavailable 表示节点接受的 slot 此刻不可授权（未申报/已占/超出当前预算）。
	//
	// 语义与 task.reject 的 `slot_unavailable` 同源：不执行、不授 lease、不计 attempt，
	// 任务回 pending + waiting_for_slot（有界退避）。绝不用 stale_config 伪装容量拒绝。
	ErrSlotUnavailable = errors.New("slot_unavailable")
	// ErrNodeDraining 节点在排空（不再接新任务）。
	ErrNodeDraining = errors.New("node_draining")
	// ErrNodeExists 节点重名（集群内唯一，HTTP 409）。
	ErrNodeExists = errors.New("store: node name already exists")
)

// InstanceSpec 是节点申报的并发槽位（hello.instances[]，D-I13）。
//
// v4 每槽最多一个能力进程：没有 per-slot 乘数（wire 上的旧 concurrency 字段已移除），
// 容量只看真实 slot 记录与机器级 limits。
type InstanceSpec struct {
	Slot int `json:"slot"`
}

// SlotObservation 是当前连接对某个物理槽位「此刻在跑什么」的**明确事实**。
//
// 它是槽位回收的唯一可信输入（G2 从 T3 最终帧映射；store 内部形状不镜像 wire）：
//   - Busy 三态：nil = 未断言（未知），true = 明确仍在跑，false = 明确无进程；
//   - busy=true 带 taskId/attempt/leaseId（已授权的执行）或 assignmentId/proposedAttempt
//     （尚未 grant 的 reservation，**不伪造 lease**）；
//   - busy=false 时必须带相符的历史 taskId/attempt/(leaseId)：无匹配引用的 idle 记录
//     不能清旧 busy（rehello / 缺字段都不等于进程已死）。
//
// C3a 起 Busy=true 的 reservation 事实会落进**真实槽账本**（node_instances：
// busy_task + busy_attempt=proposedAttempt + busy_lease_id 为空 + busy_assignment_id
// + busy_expires_at），前提是能对上中枢当前候选（pending/同节点/同 assignment/同 proposed/
// 窗口有效）。预算统计因此能看见「节点已经接住、但还没 grant」的真实占用；节点谎报一条
// 对不上候选的 reservation 只会被忽略（不制造容量）。执行类的 Busy=true 不重绑：节点
// 不能凭一句 busy 重新定义某次历史执行的绑定。
//
// `nil` 数组 / 空数组 / 没提到某个 slot 都**不表示空闲**。
type SlotObservation struct {
	Slot int
	// Busy 为 nil 表示这次没有为该 slot 给出明确事实（未知），不得用于回收。
	Busy *bool
	// TaskID / Attempt / LeaseID 是这个占用的执行标识。
	TaskID  string
	Attempt int
	LeaseID string
	// AssignmentID / ProposedAttempt 是尚未 grant 的 reservation 标识（无 lease）。
	AssignmentID    string
	ProposedAttempt int
	// ObservedAt 是节点采集这条事实的时刻（0 = 不设保护）。观察到之后才建立的
	// 绑定（updated_at 更晚）不能被旧快照误放。
	ObservedAt int64
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
	facts, declared_capabilities, declared_namespaces,
	COALESCE((SELECT c.resource_version FROM node_configs c WHERE c.node_id = nodes.id), 1)`

// scanNodeRow 从 rows 解析一行。
func scanNodeRow(rows scanner) (*Node, error) {
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
// DeleteNode uses an immutable-ID revision; public callers use DeleteNodeByName.
func (d *DB) DeleteNode(ctx context.Context, nodeID string, expectedRV int64) error {
	if expectedRV <= 0 {
		return ErrResourceVersionRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete node: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	n, err := getNodeOn(ctx, ex, "id", nodeID)
	if err != nil {
		return err
	}
	if err := checkResourceRevision(expectedRV, n.ID, n.ResourceVersion); err != nil {
		return err
	}
	if err := deleteNodeOn(ctx, ex, n); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertInstances 落库节点申报的实例槽位；超过 nodes.max_instances 返回 ErrTooManyInstances。
//
// nil/空清单不构成新断言，保留账本。非空清单缩容只删除未占用的老槽位；
// occupied 槽即使超预算也保留到精确历史回收。slot 必须唯一且 ≥ 0，
// 已有槽位重报只更新 liveness，不改 busy_task/attempt/lease 绑定。
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

	if len(specs) > 0 {
		placeholders := make([]string, len(specs))
		args := make([]any, 0, len(specs)+1)
		args = append(args, nodeID)
		for i, s := range specs {
			placeholders[i] = "?"
			args = append(args, s.Slot)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_instances WHERE node_id = ? AND (busy_task IS NULL OR busy_task = '') AND slot NOT IN (`+
				strings.Join(placeholders, ",")+`)`, args...); err != nil {
			return nil, fmt.Errorf("remove stale instances: %w", err)
		}
	}

	now := Now()
	for _, s := range specs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_instances (id, node_id, slot, busy_task, busy_attempt, busy_lease_id, alive, updated_at)
			 VALUES (?,?,?,NULL,0,'',1,?)
			 ON CONFLICT(node_id, slot) DO UPDATE SET
			   alive = 1,
			   updated_at = excluded.updated_at`,
			NewInstanceID(), nodeID, s.Slot, now); err != nil {
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
		`SELECT id, node_id, slot, busy_task, busy_attempt, busy_lease_id, alive, updated_at
		 FROM node_instances WHERE node_id = ? ORDER BY slot`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	out := []*NodeInstance{}
	for rows.Next() {
		var in NodeInstance
		var alive int
		if err := rows.Scan(&in.ID, &in.NodeID, &in.Slot,
			&in.BusyTask, &in.BusyAttempt, &in.BusyLeaseID, &alive, &in.UpdatedAt); err != nil {
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

// MarkNodeOffline 是「会话死亡」的唯一入口：节点转 offline，并把它的全部槽位标成
// **不可派发**（alive=0）。
//
// **不释放 busy 账本**：offline 只说明连接断了，不等于机器上的进程组已回收。容量在
// 节点回来且给出明确实例事实（ReclaimStaleSlots）之前保持占用——否则中枢以为槽空了
// 而旧进程还在跑，同一槽会被派第二条任务。
func (d *DB) MarkNodeOffline(ctx context.Context, nodeID string, now int64) error {
	if nodeID == "" {
		return fmt.Errorf("mark node offline: empty node_id")
	}
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark node offline: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE nodes SET status = ?, last_seen_at = ?, resource_version = resource_version + 1
		  WHERE id = ?`, NodeStatusOffline, now, nodeID)
	if err != nil {
		return fmt.Errorf("mark node offline: %w", err)
	}
	if err := nodeRowsAffected(res, "mark node offline"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE node_instances SET alive = 0, updated_at = ? WHERE node_id = ?`, now, nodeID); err != nil {
		return fmt.Errorf("mark node offline: instances: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark node offline: commit: %w", err)
	}
	return nil
}

// recordReservationOn 把节点「Busy=true 的 reservation」事实落进真实槽账本。
//
// 只接受能对上中枢当前候选的事实（taskId + assignmentId + proposedAttempt + 同节点 +
// pending + 窗口未过 + 未取消）：节点不能凭一句 busy 就制造一笔占预算的幽灵预约。
// 写入形态：busy_task=taskID、busy_attempt=proposedAttempt、busy_lease_id 为空
// （**不伪造 lease**）、busy_assignment_id=assignmentId、busy_expires_at=该候选的有界窗口。
//
// 同一物理槽上的旧占用只能被三种情况替换：空槽、**同一个 assignment** 自己的重复观测
// （幂等刷新），或一条**已过 recorded 窗口的 lease-free reservation**（它从未 grant、
// 因此不可能有进程；执行绑定 lease 非空，永远走不到这条）。旧 assignment 的观测对不上
// 当前候选、也清不掉新 reservation（新 reservation 的窗口未过）。
func recordReservationOn(ctx context.Context, ex queryExec, nodeID string, obs SlotObservation, now int64) error {
	if obs.TaskID == "" || obs.AssignmentID == "" || obs.ProposedAttempt <= 0 {
		return nil
	}
	var expiresAt int64
	err := ex.queryRowContext(ctx,
		`SELECT assignment_expires_at FROM tasks
		  WHERE id = ? AND phase = ? AND node_id = ? AND assignment_id = ?
		    AND proposed_attempt = ? AND cancel_requested_at IS NULL
		    AND assignment_expires_at IS NOT NULL AND assignment_expires_at > ?`,
		obs.TaskID, TaskPending, nodeID, obs.AssignmentID, obs.ProposedAttempt, now).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		// 对不上中枢事实（旧 assignment / 已 grant / 已取消 / 窗口过期）：忽略。
		return nil
	}
	if err != nil {
		return fmt.Errorf("record reservation: read task: %w", err)
	}
	var instanceID string
	err = ex.queryRowContext(ctx,
		`SELECT id FROM node_instances WHERE node_id = ? AND slot = ?`, nodeID, obs.Slot).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record reservation: read instance: %w", err)
	}
	if _, err := ex.execContext(ctx,
		`UPDATE node_instances
		    SET busy_task = ?, busy_attempt = ?, busy_lease_id = '',
		        busy_assignment_id = ?, busy_expires_at = ?, updated_at = ?
		  WHERE id = ?
		    AND (busy_task IS NULL
		      OR (busy_task = ? AND busy_lease_id = '' AND busy_assignment_id = ?)
		      OR (busy_lease_id = '' AND busy_expires_at > 0 AND busy_expires_at <= ?))`,
		obs.TaskID, obs.ProposedAttempt, obs.AssignmentID, expiresAt, now, instanceID,
		obs.TaskID, obs.AssignmentID, now); err != nil {
		return fmt.Errorf("record reservation: %w", err)
	}
	return nil
}

// ReclaimStaleSlots 按**明确的实例事实**回收槽位绑定（绝不读任务相位做推断）。
//
// 旧判定是「槽位指向的任务已不在 running → 清槽」——那是拿数据库状态冒充物理回收
// 事实：租约过期只吊销授权，进程组可能还在跑；hello 重连也不证明旧进程已死
// （rehello ≠ reaped）。现在只有满足全部条件的观察才会回收：
//   - Busy 显式为 false（缺字段/未提到的 slot 都是未知，不是空）；
//   - 观察里带相符的历史 taskId/attempt/leaseId（无匹配引用的 idle 记录不能清旧 busy）；
//   - 观察时刻不早于该绑定的建立时刻（旧快照不能放掉它没看见的新占用）。
//
// 回收只清**那一条** (instance, task, attempt) 绑定，并把该 attempt 记上 reaped 事实；
// 不改变任务 phase / token / 新 attempt 的槽。返回真正回收的槽数（无事实 = 0，不是假成功）。
func (d *DB) ReclaimStaleSlots(ctx context.Context, nodeID string, observations []SlotObservation, now int64) (int, error) {
	if nodeID == "" {
		return 0, fmt.Errorf("reclaim stale slots: empty node_id")
	}
	if now == 0 {
		now = Now()
	}
	if len(observations) == 0 {
		// 没申报任何实例事实：什么都不知道（不是「全部空闲」）。
		return 0, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale slots: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, nodeID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("reclaim stale slots: read node: %w", err)
	}

	ex := txExec{tx: tx}
	released := 0
	for _, obs := range observations {
		if obs.Busy == nil || obs.Slot < 0 {
			// 未知（未断言）或坏 slot：什么都不能做。
			continue
		}
		if *obs.Busy {
			// 明确的「仍在跑」：执行绑定不重绑；reservation 在能对上中枢当前候选时
			// 落进真实槽账本（现有字段，不伪造 lease），供三层预算统计。
			if rerr := recordReservationOn(ctx, ex, nodeID, obs, now); rerr != nil {
				return released, fmt.Errorf("reclaim stale slots: %w", rerr)
			}
			continue
		}
		if obs.TaskID == "" {
			continue
		}
		var (
			instanceID     string
			busyTask       string
			busyAttempt    int
			busyLease      string
			busyAssignment string
			updatedAt      int64
		)
		err := ex.queryRowContext(ctx,
			`SELECT id, COALESCE(busy_task,''), busy_attempt, busy_lease_id, busy_assignment_id, updated_at
			   FROM node_instances WHERE node_id = ? AND slot = ?`, nodeID, obs.Slot).
			Scan(&instanceID, &busyTask, &busyAttempt, &busyLease, &busyAssignment, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue // 节点报的是它已不申报的 slot：没有可回收的绑定
		}
		if err != nil {
			return released, fmt.Errorf("reclaim stale slots: read instance: %w", err)
		}
		if busyTask == "" {
			continue
		}
		if obs.Attempt > 0 && obs.LeaseID != "" {
			// 执行类 idle：引用必须与中枢旧绑定完全相符（task + attempt + lease）。
			if busyTask != obs.TaskID || busyAttempt != obs.Attempt || busyLease != obs.LeaseID {
				continue
			}
			if obs.ObservedAt > 0 && updatedAt > obs.ObservedAt {
				// 绑定的生成晚于事实采集：旧快照不能释放一条它没看见的占用。
				continue
			}
			if _, rerr := reapAttemptOn(ctx, ex, busyTask, busyAttempt, nodeID, busyLease, instanceID,
				now, ReclaimReasonNodeIdle); rerr != nil && !errors.Is(rerr, ErrNotFound) {
				return released, fmt.Errorf("reclaim stale slots: %w", rerr)
			}
			if _, rerr := releaseSlotBindingOn(ctx, ex, nodeID, instanceID, busyTask, busyAttempt, now); rerr != nil {
				return released, rerr
			}
			released++
			continue
		}
		// reservation 类 idle（busy_lease_id=''）：必须带相符 assignment/proposed，
		// 只清 reservation 本身（没有 attempt 事实可写、也绝不伪造 reaped）。
		// assignment 必须精确相等：旧 assignment 的 idle 观测（哪怕同 proposed）不能
		// 清掉新候选的预约——那是两条不同的事实。
		if obs.AssignmentID == "" || obs.ProposedAttempt <= 0 {
			continue
		}
		if busyTask != obs.TaskID || busyAttempt != obs.ProposedAttempt || busyLease != "" ||
			busyAssignment != obs.AssignmentID {
			continue
		}
		if obs.ObservedAt > 0 && updatedAt > obs.ObservedAt {
			continue
		}
		res, uerr := ex.execContext(ctx,
			`UPDATE node_instances
			    SET busy_task = NULL, busy_attempt = 0, busy_lease_id = '',
			        busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
			  WHERE id = ? AND busy_task = ? AND busy_attempt = ? AND busy_lease_id = ''
			    AND busy_assignment_id = ?`,
			now, instanceID, obs.TaskID, obs.ProposedAttempt, obs.AssignmentID)
		if uerr != nil {
			return released, fmt.Errorf("reclaim stale slots: clear reservation: %w", uerr)
		}
		if n, rerr := res.RowsAffected(); rerr != nil {
			return released, fmt.Errorf("reclaim stale slots: reservation rows: %w", rerr)
		} else if n > 0 {
			released++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reclaim stale slots: commit: %w", err)
	}
	return released, nil
}

// LeaseSlot 把一个任务的**具体 attempt** 绑到一个**已指定的**槽位上（CAS：busy_task IS NULL）。
//
// 正常执行路径必须走 GrantLease（消费节点 task.accept 回报的真实 slot，同事务映射
// node+slot → instance）；中枢**不**自行随机挑槽。这个公开原语只留给运维/测试直接
// 验证绑定行为；它同样写入 busy_attempt / busy_lease_id，不允许产生「不知道属于
// 哪一代」的占用。
//
// 返回 instance_id；slot 不存在/不存活/已被占 → ErrSlotUnavailable。
func (d *DB) LeaseSlot(ctx context.Context, nodeID, taskID string, attempt int, leaseID string, slot int) (string, error) {
	if taskID == "" || attempt <= 0 || leaseID == "" || slot < 0 {
		return "", fmt.Errorf("lease slot: taskID/attempt/leaseID/slot required")
	}
	instanceID, err := bindSlotOn(ctx, d, nodeID, slot, taskID, attempt, leaseID, "", 0, Now())
	return instanceID, err
}

// bindSlotOn 是「把一次具体执行绑到指定 slot」的唯一 SQL 点（tx 内 / 池上共用）。
//
// 不随机挑槽：slot 是节点 task.accept 预留的真实空闲槽；中枢只复核它属于这台
// 节点、存活、且此刻没有其它绑定，再用 CAS 写上执行身份。
//
// grant 路径（proposedAttempt>0）只能消费**本次候选自己**的 reservation：task +
// proposed + assignment 三项都得对上。旧 assignment 的预约即使同 task/同 proposed
// 也不能被新候选复用（否则新执行占着旧预约，预算与回收都会错位）。LeaseSlot 原语
// （proposedAttempt=0）只接空槽。
func bindSlotOn(ctx context.Context, ex queryExec, nodeID string, slot int, taskID string,
	attempt int, leaseID, assignmentID string, proposedAttempt int, now int64) (string, error) {
	var instanceID string
	err := ex.queryRowContext(ctx,
		`SELECT id FROM node_instances WHERE node_id = ? AND slot = ? AND alive = 1`, nodeID, slot).
		Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: node %s slot %d 未申报或不可用", ErrSlotUnavailable, nodeID, slot)
	}
	if err != nil {
		return "", fmt.Errorf("bind slot: read instance: %w", err)
	}
	// CAS：空槽，或**本次候选自己**的已接受 reservation（busy_lease_id='' 是 reservation
	// 与 execution 的唯一判别；assignment 是同一条预约的身份）。别人的 reservation/
	// 执行（含旧 assignment 的预约）一律不能抢。
	q := `UPDATE node_instances
	        SET busy_task = ?, busy_attempt = ?, busy_lease_id = ?,
	            busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
	      WHERE id = ? AND busy_task IS NULL`
	args := []any{taskID, attempt, leaseID, now, instanceID}
	if proposedAttempt > 0 {
		q = `UPDATE node_instances
		        SET busy_task = ?, busy_attempt = ?, busy_lease_id = ?,
		            busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
		      WHERE id = ? AND (busy_task IS NULL
		        OR (busy_task = ? AND busy_attempt = ? AND busy_lease_id = ''
		            AND busy_assignment_id = ?))`
		args = append(args, taskID, proposedAttempt, assignmentID)
	}
	res, err := ex.execContext(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("bind slot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("bind slot rows: %w", err)
	}
	if n != 1 {
		return "", fmt.Errorf("%w: node %s slot %d 已有未回收的占用", ErrSlotUnavailable, nodeID, slot)
	}
	return instanceID, nil
}

// ReleaseSlot 精确释放一次占槽：instance + task + attempt 三者都对上才清。
//
// 为什么必须带 attempt：同一逻辑任务换 attempt 重试时可能同时占着旧 attempt 的槽，
// 只按 (instance, task) 释放会把旧进程还在用的槽放给下一个任务。attempt 对不上的
// 绑定（槽已归新 attempt）一行都不动。
func (d *DB) ReleaseSlot(ctx context.Context, nodeID, instanceID, taskID string, attempt int) error {
	_, err := releaseSlotBindingOn(ctx, d, nodeID, instanceID, taskID, attempt, Now())
	return err
}

// releaseSlotBindingOn 是精确释槽的唯一 SQL 点（tx 内 / 池上共用）。
//
// `busy_attempt = 0` 的防御分支只对待「没有 attempt 身份的遗留行」：那种行没有更具体
// 的身份可核，而 instance 是全局主键——它上面只可能有一个占用者。v4 grant 永不产生
// 这种行（每条占用都绑到具体 attempt）。
func releaseSlotBindingOn(ctx context.Context, ex queryExec, nodeID, instanceID, taskID string, attempt int, now int64) (int64, error) {
	if instanceID == "" || taskID == "" {
		return 0, nil
	}
	q := `UPDATE node_instances
	        SET busy_task = NULL, busy_attempt = 0, busy_lease_id = '',
	            busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
	       WHERE id = ? AND busy_task = ?`
	args := []any{now, instanceID, taskID}
	if nodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	if attempt > 0 {
		q += ` AND (busy_attempt = ? OR busy_attempt = 0)`
		args = append(args, attempt)
	}
	res, err := ex.execContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("release lease slot %s/%s: %w", nodeID, instanceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("release lease slot rows: %w", err)
	}
	return n, nil
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

	// ① ns 名（授权/申报列表存的是 ns 名，不是内部 id）。
	namespaceName, err := d.namespaceSlug(ctx, namespaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: %w", err)
	}

	// ② 能力身份（name@version）由 digest 反查：digest 是部署身份，每 ns 唯一。
	var capName string
	var capVersion int
	err = d.QueryRowContext(ctx,
		`SELECT name, version FROM capability_versions WHERE namespace_id = ? AND digest = ? AND deleted_at IS NULL`,
		namespaceID, digest).Scan(&capName, &capVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: read capability: %w", err)
	}

	// ③ 任务 ns 的默认值（第三层）：配置诊断/普通派发用**当前**值；
	// 已经接受的任务按钉住的快照走 EligibleNodesForTask。
	nsVars, err := d.namespaceVarsQueried(ctx, namespaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes: %w", err)
	}

	return d.nodeEligibility(ctx, namespaceID, namespaceName, nsVars, capName, capVersion, spec, digest)
}

// EligibleNodesForTask 是派发路径的入口：**任务钉住的 ns 输入快照**参与判定（D2）。
//
// 为什么不能用 EligibleNodes（当前 ns 配置）：一条已接受的任务如果因为排队期间
// ns 默认值被改过，用当前值判「能不能跑」会得出与它实际执行输入不一致的结论
// （典型症状：能力要求 vars.SOURCE_ROOT，当前配置有、任务快照里没有 → 派发说可用，
// 节点上却拿不到值）。孤儿视图与调度必须用这个函数。
func (d *DB) EligibleNodesForTask(ctx context.Context, t *Task) ([]*Node, map[string]string, error) {
	if t == nil {
		return nil, nil, fmt.Errorf("eligible nodes for task: nil task")
	}
	if t.NamespaceID == "" || t.NamespaceID == AllNamespaces {
		return nil, nil, ErrNamespaceRequired
	}
	namespaceName, err := d.namespaceSlug(ctx, t.NamespaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes for task %s: %w", t.ID, err)
	}
	// 能力身份按**任务钉住的 capability_id** 取（不是按 digest 猜）：同一 ns 里
	// 不同名字的能力可以共享同一份 spec 内容，digest 反查会有多义。
	// This internal scheduling entry uses the Task's pinned UID, never live name resolution.
	capRow, err := getPinnedCapabilityByIDOn(ctx, d, t.NamespaceID, t.CapabilityID)
	if err != nil {
		return nil, nil, fmt.Errorf("eligible nodes for task %s: read capability: %w", t.ID, err)
	}
	capName, capVersion, spec := capRow.Name, capRow.Version, capRow.Spec
	digest := t.Digest
	if strings.TrimSpace(digest) == "" {
		// 任务行没有钉 digest（建任务时 caller 没给）：没有任何节点能被证明持有它，
		// 按 NotDeployed 报（不能当成「随便哪台都能跑」）。
		return nil, map[string]string{}, nil
	}
	return d.nodeEligibility(ctx, t.NamespaceID, namespaceName, t.NamespaceVars, capName, capVersion, spec, digest)
}

// DeployCandidateNodes 返回「可以把能力**部署**到本命名空间」的节点。
//
// 契约（api/resource.yaml CapabilityDeployRequest.nodes）写的是「省略 = 全部已授权且在线
// 节点」—— 所以这里的判据只有两条：ns ∈ 申报 ∩ 授权，且不在 draining（在线由调用方
// 按连接事实再筛：store 不知道 WS 会话）。
//
// **不能**拿 EligibleNodes 当部署候选（真实死结）：它是**派发**判据，其中一条是
// 「deployments 已有该 digest 且 applied」。于是刚建好的能力谁都不可用 ——
// 没部署过 ⇒ 没有候选 ⇒ 不选目标时目标为空 ⇒ 什么都不部署 ⇒ 永远没有候选。
// 现场症状：控制台「没有候选节点；不选也能提交（由服务端决定）」，提交回 200 但
// deployed: []，能力永远停在 NotDeployed。
func (d *DB) DeployCandidateNodes(ctx context.Context, namespaceID string) ([]*Node, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	namespaceName, err := d.namespaceSlug(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("deploy candidates: %w", err)
	}

	nodes := []*Node{}
	rows, err := d.queryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("deploy candidates: list nodes: %w", err)
	}
	for rows.Next() {
		n, serr := scanNodeRow(rows)
		if serr != nil {
			rows.Close()
			return nil, serr
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("deploy candidates: iterate nodes: %w", err)
	}
	rows.Close()

	// 授权列表在 node_configs.namespaces（缺配置行 = 缺省 ["*"]，与授权判定同一默认）。
	authored := map[string][]string{}
	crows, err := d.queryContext(ctx, `SELECT node_id, namespaces FROM node_configs`)
	if err != nil {
		return nil, fmt.Errorf("deploy candidates: read node configs: %w", err)
	}
	for crows.Next() {
		var nodeID, rawNamespaces string
		if err := crows.Scan(&nodeID, &rawNamespaces); err != nil {
			crows.Close()
			return nil, fmt.Errorf("deploy candidates: scan node config: %w", err)
		}
		decoded, derr := decodeStringList(rawNamespaces)
		if derr != nil {
			crows.Close()
			return nil, fmt.Errorf("deploy candidates: %w", derr)
		}
		authored[nodeID] = decoded
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, fmt.Errorf("deploy candidates: iterate node configs: %w", err)
	}
	crows.Close()

	out := []*Node{}
	for _, n := range nodes {
		// 排空中 = 跑完手上的任务后不再接新活，不往里推新部署。
		if n.Status == NodeStatusDraining {
			continue
		}
		authorized, ok := authored[n.ID]
		if !ok {
			authorized = []string{NamespaceWildcard}
		}
		if !namespacesAllow(intersectNamespaces(authorized, n.DeclaredNamespaces), namespaceName) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// namespaceSlug 取 ns 名（授权/申报列表存的是名字，不是内部 id）。
func (d *DB) namespaceSlug(ctx context.Context, namespaceID string) (string, error) {
	var slug string
	err := d.QueryRowContext(ctx, `SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read namespace: %w", err)
	}
	return slug, nil
}

// namespaceVarsQueried 读某 ns 当前的默认 vars（没有配置行 = 空默认）。
func (d *DB) namespaceVarsQueried(ctx context.Context, namespaceID string) (map[string]string, error) {
	out := map[string]string{}
	var raw string
	err := d.QueryRowContext(ctx,
		`SELECT vars FROM namespace_configs WHERE namespace_id = ?`, namespaceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read namespace config: %w", err)
	}
	vars, derr := decodeStringMap(raw)
	if derr != nil {
		return nil, derr
	}
	return vars, nil
}

// nodeEligibility 是判定核心：给定 ns 名 / ns 默认值 / 能力身份 / digest，逐节点算可用性。
//
// 调用方（EligibleNodes / EligibleNodesForTask / 孤儿视图）只决定「ns 默认值从哪来」，
// 判定本身只有这一份实现——否则「能派发」与「孤儿」会给出两个互相矛盾的答案。
func (d *DB) nodeEligibility(ctx context.Context, namespaceID, namespaceName string, nsVars map[string]string,
	capName string, capVersion int, spec artifact.Spec, digest string) ([]*Node, map[string]string, error) {
	// 只读路径**不**开事务：DSN 是 `_txlock=immediate`，读事务会先拿写锁，
	// 把并发派发串行化。这里每条查询各自读最新快照——最坏情况只是某个 reason
	// 用到了另一秒的配置，不会伪造出「可派发」（保守方向的偏差）。

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
