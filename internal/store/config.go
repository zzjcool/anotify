package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 三层配置（config.md §2/§3/§4）：系统保留期、命名空间默认值、节点默认值 + 本机快照。
//
// 三种值、三层作用域，但只有**两套版本**（config.md §2.4 / C18）：
//
//	resource_version  写并发令牌（CAS），每次成功写入 +1
//	revision          配置生效版本；命名空间与节点各有一份，互不混用
//	applied_revision  节点回报的机器级 revision（只对节点默认值有意义）
//
// ns 默认值（namespace_configs）与节点默认值（node_configs）各有自己的 revision；
// 任务帧同时带 configRevision（机器级）与 namespaceVarsRevision（ns 级），
// 但两者在库里是两行，绝不共用一个计数器。
//
// 本机（anode.yaml）的 vars/limits 不落中枢业务表：它们由 hello 上报，
// 作为**快照**存在 node_configs.local_vars / local_limits，只用于匹配与展示。
// 它永远不参与「节点默认值」的 apply 状态（D-I3：ns 默认值不落 applied、不上报；
// 本机快照也不是中枢配置）。

// 配置值校验上限（config.md §3.1 / anode.md §2）。
const (
	// VarsMaxEntries 是一个 vars 映射最多几项（本机 / 节点默认 / ns 默认同一上限）。
	VarsMaxEntries = 64
	// VarValueMaxBytes 是单个 vars 值的字节上限。
	VarValueMaxBytes = 4096
	// NamespaceWildcard 是「全部命名空间」的字面量（节点申报与授权共用）。
	NamespaceWildcard = "*"
)

// 系统保留期默认值（config.md §3.6）。
const (
	DefaultRetentionEventTTLSec  int64 = 2592000 // 30 天
	DefaultRetentionTaskTTLSec   int64 = 2592000 // 30 天
	DefaultRetentionResultTTLSec int64 = 604800  // 7 天
)

// 配置 API 的稳定错误码（server 按 sentinel 翻 400/409，Code 只用于响应体）。
const (
	CodeValueInvalid            = "value_invalid"
	CodeResourceVersionRequired = "resource_version_required"
	CodeResourceVersionConflict = "resource_version_conflict"
)

// 配置写入的哨兵错误（config.md §5.2）。
var (
	// ErrConfigInvalid：中枢配置（节点 / ns / 系统）的名字、值或形状不合规 → 400 value_invalid。
	ErrConfigInvalid = errors.New("store: config value invalid")
	// ErrResourceVersionRequired：写配置没带 resourceVersion 前置条件 → 400 resource_version_required。
	ErrResourceVersionRequired = errors.New("store: resource version required")
	// ErrResourceVersionConflict：resourceVersion 已旧 → 409 resource_version_conflict。
	ErrResourceVersionConflict = errors.New("store: resource version conflict")
)

// varNamePattern 是 vars 键名（三层共用一套；anode.md §3.1）。
var configVarNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// configNamespacePattern 是命名空间名 / 节点申报 / 授权列表元素的形状（api-conventions §7.4）。
var configNamespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// varSource* 是 EffectiveVar.Source 的封闭取值（config.md C11）。
const (
	VarSourceLocal     = "local"
	VarSourceNode      = "node"
	VarSourceNamespace = "namespace"
)

// EffectiveVar 是一个名字在 (节点, ns) 上下文里的生效值 + 来源层。
//
// 来源必须能看见（config.md §2.3）：节点页标 local / node，任务详情标
// local / node / namespace。值本身是非敏感的（不是秘密），所以可以一起给。
type EffectiveVar struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// SystemConfig 是中枢系统配置（config.md §3.6）：保留期三项 + CAS 令牌。
type SystemConfig struct {
	RetentionEventTTLSec  int64 `json:"retentionEventTtlSec"`
	RetentionTaskTTLSec   int64 `json:"retentionTaskTtlSec"`
	RetentionResultTTLSec int64 `json:"retentionResultTtlSec"`
	ResourceVersion       int64 `json:"resourceVersion"`
}

// NamespaceConfig 是一个命名空间的默认 vars（config.md §3）。
//
// Revision 是 namespaceVarsRevision（配置生效版本，每次写入 +1）；
// ResourceVersion 是写并发令牌。两者不能压成一个（C18）。
type NamespaceConfig struct {
	NamespaceID     string            `json:"namespaceId"`
	Vars            map[string]string `json:"vars"`
	Revision        int64             `json:"revision"`
	ResourceVersion int64             `json:"resourceVersion"`
}

// NodeConfig 是一台节点的中枢侧默认值与上报快照（config.md §4）。
//
//	Namespaces   授权列表（ns 名或 "*"），与节点申报取交集；不是逐层优先
//	Vars/Limits  节点默认值（机器级，跨 ns 共用）
//	Revision     本行的 configRevision（中枢生成）
//	Applied*     节点 hello 回报的机器级快照（revision/hash/at）
//	LocalVars/LocalLimits  本机上报值（只用于匹配与展示，不是中枢配置）
type NodeConfig struct {
	NodeID          string            `json:"nodeId"`
	Namespaces      []string          `json:"namespaces"`
	Vars            map[string]string `json:"vars"`
	Limits          map[string]any    `json:"limits"`
	Revision        int64             `json:"revision"`
	ResourceVersion int64             `json:"resourceVersion"`
	AppliedRevision int64             `json:"appliedRevision"`
	AppliedHash     string            `json:"appliedHash"`
	AppliedAt       *int64            `json:"appliedAt"`
	LocalVars       map[string]string `json:"localVars"`
	LocalLimits     map[string]any    `json:"localLimits"`
}

// NodeConfigFailure 是批量写节点配置里**逐台**的失败（一台失败不连坐其它台）。
type NodeConfigFailure struct {
	NodeID string `json:"nodeId"`
	Err    error  `json:"-"`
}

// NodeCapabilityReport 是 RecordHello 的 capabilities[] 参数（一帧多条）。
//
// 元素形状复用协议唯一源（internal/protocol，D-I13）：store 不再定义第二份
// 「能力名 + 命令探测结果」的形状，避免中枢侧两份字段名漂移。
type NodeCapabilityReport = []DeclaredCapability

// DeclaredCapability 是 hello.capabilities[] 的一条（能力名 + 命令探测结果）。
type DeclaredCapability = protocol.HelloCapability

// NodeFacts 是 hello.facts（内置机器事实，anode.md §3.6）。
type NodeFacts = protocol.NodeFacts

// ValidateVars 校验一份 vars 映射（本机 / 节点默认 / ns 默认同一套规则）。
//
// 规则（config.md §3.1）：键 `^[A-Z][A-Z0-9_]{0,63}$`；值非空、≤4096 字节、无 NUL；
// 最多 64 项。违反一律 ErrConfigInvalid（400 value_invalid），**不做部分接受**。
func ValidateVars(vars map[string]string) error {
	if len(vars) > VarsMaxEntries {
		return fmt.Errorf("%w: vars 最多 %d 项，实际 %d", ErrConfigInvalid, VarsMaxEntries, len(vars))
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 错误顺序确定：同一份坏输入永远报同一个键
	for _, k := range keys {
		if !configVarNamePattern.MatchString(k) {
			return fmt.Errorf("%w: vars 键 %q 不合法（需匹配 ^[A-Z][A-Z0-9_]{0,63}$）", ErrConfigInvalid, k)
		}
		v := vars[k]
		if v == "" {
			return fmt.Errorf("%w: vars.%s 不能是空串（缺失就不写这一项）", ErrConfigInvalid, k)
		}
		if len(v) > VarValueMaxBytes {
			return fmt.Errorf("%w: vars.%s 超过 %d 字节（实际 %d）", ErrConfigInvalid, k, VarValueMaxBytes, len(v))
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("%w: vars.%s 含 NUL", ErrConfigInvalid, k)
		}
	}
	return nil
}

// ValidateConfigNamespaces 校验一份 ns 列表（节点申报 / 授权）：元素是 ns 名或 "*"。
//
// 允许空列表（显式“谁都不授权”）；缺省值由调用方补 ["*"]（config.md §4.1）。
func ValidateConfigNamespaces(list []string) error {
	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == NamespaceWildcard {
			continue
		}
		if !configNamespacePattern.MatchString(name) {
			return fmt.Errorf("%w: namespaces 元素 %q 不合法（ns 名或 \"*\"）", ErrConfigInvalid, raw)
		}
	}
	return nil
}

// ValidateLimits 校验节点 limits 的形状（anode.md §2）。
//
// 只认两个维度：total（≥1）与 perCapability（能力名 / "*" → ≥1）。
// limits 只有本机 + 节点两层；它没有 ns 级配额，也不接受未知维度。
func ValidateLimits(limits map[string]any) error {
	for k, v := range limits {
		switch k {
		case "total":
			if _, ok := positiveInt(v); !ok {
				return fmt.Errorf("%w: limits.total 必须是 ≥1 的整数，实际 %v", ErrConfigInvalid, v)
			}
		case "perCapability":
			obj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: limits.perCapability 必须是对象", ErrConfigInvalid)
			}
			keys := make([]string, 0, len(obj))
			for name := range obj {
				keys = append(keys, name)
			}
			sort.Strings(keys)
			for _, name := range keys {
				if name != NamespaceWildcard && !configNamespacePattern.MatchString(name) {
					return fmt.Errorf("%w: limits.perCapability 键 %q 不是能力名也不是 \"*\"", ErrConfigInvalid, name)
				}
				if _, ok := positiveInt(obj[name]); !ok {
					return fmt.Errorf("%w: limits.perCapability.%s 必须是 ≥1 的整数", ErrConfigInvalid, name)
				}
			}
		default:
			return fmt.Errorf("%w: limits 未知维度 %q（只认 total / perCapability）", ErrConfigInvalid, k)
		}
	}
	return nil
}

// positiveInt 把 JSON 解出来的数值归一成正整数；bool=false 表示不是正整数。
func positiveInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n >= 1
	case int64:
		return int(n), n >= 1
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int(n), n >= 1
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), i >= 1
	default:
		return 0, false
	}
}

// ---------- JSON 列编解码（本文件是配置 JSON 的唯一入口） ----------

// marshalStringMap 把字符串映射编成 JSON；nil 归一成 `{}`（不变量 I9）。
func marshalStringMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal string map: %w", err)
	}
	return string(b), nil
}

// decodeStringMap 解一列字符串映射；空串 / `{}` 都归一成非 nil 空映射。
func decodeStringMap(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode string map: %w", err)
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

// marshalAnyMap 把任意 JSON 对象编成字符串；nil 归一成 `{}`。
func marshalAnyMap(m map[string]any) (string, error) {
	if m == nil {
		m = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal json map: %w", err)
	}
	return string(b), nil
}

// decodeAnyMap 解一列 JSON 对象（数字保留成 json.Number：limits 的往返不丢整数形态）。
func decodeAnyMap(raw string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json map: %w", err)
	}
	if v == nil {
		return out, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("decode json map: want object, got %T", v)
	}
	return obj, nil
}

// marshalStringList 把字符串列表编成 JSON；nil 归一成 `[]`。
func marshalStringList(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("marshal string list: %w", err)
	}
	return string(b), nil
}

// decodeStringList 解一列字符串数组；空串 / `[]` 都归一成非 nil 空切片。
func decodeStringList(raw string) ([]string, error) {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode string list: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// ---------- 系统配置（config.md §3.6） ----------

// GetSystemConfig 读中枢系统配置；没有行时返回默认保留期。
//
// 没有行时 ResourceVersion = 1：单行表（id 恒为 1）在语义上已经存在，
// 首次 PUT 必须能带一个合法的前置条件（缺失 = 400），所以默认值也给出可用令牌。
func (d *DB) GetSystemConfig(ctx context.Context) (*SystemConfig, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("get system config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cfg := &SystemConfig{
		RetentionEventTTLSec:  DefaultRetentionEventTTLSec,
		RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
		RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		ResourceVersion:       1,
	}
	err = tx.QueryRowContext(ctx,
		`SELECT retention_event_ttl_sec, retention_task_ttl_sec,
		        retention_result_ttl_sec, resource_version
		   FROM system_config WHERE id = 1`).
		Scan(&cfg.RetentionEventTTLSec, &cfg.RetentionTaskTTLSec,
			&cfg.RetentionResultTTLSec, &cfg.ResourceVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get system config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("get system config: commit: %w", err)
	}
	return cfg, nil
}

// PutSystemConfig 写中枢系统配置（CAS：必须带当前 resourceVersion）。
//
// 缺 rv → ErrResourceVersionRequired（400）；rv 已旧 → ErrResourceVersionConflict（409）。
// 成功后 cfg.ResourceVersion 就地 +1（单行表恒存在）。
func (d *DB) PutSystemConfig(ctx context.Context, cfg *SystemConfig, rv int64) error {
	if cfg == nil {
		return fmt.Errorf("put system config: nil")
	}
	if rv <= 0 {
		return fmt.Errorf("%w: 系统配置写入必须带 resourceVersion", ErrResourceVersionRequired)
	}
	for name, v := range map[string]int64{
		"retention.eventTtlSec":  cfg.RetentionEventTTLSec,
		"retention.taskTtlSec":   cfg.RetentionTaskTTLSec,
		"retention.resultTtlSec": cfg.RetentionResultTTLSec,
	} {
		if v < 0 {
			return fmt.Errorf("%w: %s 不能为负（0 = 永久）", ErrConfigInvalid, name)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put system config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur := int64(1) // 无行 = 默认行（见 GetSystemConfig）
	err = tx.QueryRowContext(ctx,
		`SELECT resource_version FROM system_config WHERE id = 1`).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("put system config: read current: %w", err)
	}
	if rv != cur {
		return fmt.Errorf("%w: 系统配置 resourceVersion %d 已旧（当前 %d）",
			ErrResourceVersionConflict, rv, cur)
	}
	next := cur + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO system_config
		   (id, retention_event_ttl_sec, retention_task_ttl_sec,
		    retention_result_ttl_sec, resource_version, updated_at)
		 VALUES (1,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   retention_event_ttl_sec = excluded.retention_event_ttl_sec,
		   retention_task_ttl_sec = excluded.retention_task_ttl_sec,
		   retention_result_ttl_sec = excluded.retention_result_ttl_sec,
		   resource_version = excluded.resource_version,
		   updated_at = excluded.updated_at`,
		cfg.RetentionEventTTLSec, cfg.RetentionTaskTTLSec, cfg.RetentionResultTTLSec,
		next, Now()); err != nil {
		return fmt.Errorf("put system config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("put system config: commit: %w", err)
	}
	cfg.ResourceVersion = next
	return nil
}

// ---------- 命名空间默认值（config.md §3） ----------

// GetNamespaceConfig 读某个命名空间的默认 vars；没有配置行时返回空默认。
//
// 空默认的 ResourceVersion = 1（与 namespace_configs 列默认同值），
// 让首次 PUT 能带一个合法前置条件。命名空间不存在 → ErrNamespaceNotFound。
func (d *DB) GetNamespaceConfig(ctx context.Context, namespaceID string) (*NamespaceConfig, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("get namespace config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := namespaceExistsOn(ctx, txExec{tx: tx}, namespaceID); err != nil {
		return nil, err
	}
	cfg := &NamespaceConfig{
		NamespaceID:     namespaceID,
		Vars:            map[string]string{},
		ResourceVersion: 1,
	}
	var rawVars string
	err = tx.QueryRowContext(ctx,
		`SELECT vars, revision, resource_version FROM namespace_configs WHERE namespace_id = ?`,
		namespaceID).Scan(&rawVars, &cfg.Revision, &cfg.ResourceVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get namespace config: %w", err)
	}
	if err == nil {
		vars, derr := decodeStringMap(rawVars)
		if derr != nil {
			return nil, fmt.Errorf("get namespace config: %w", derr)
		}
		cfg.Vars = vars
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("get namespace config: commit: %w", err)
	}
	return cfg, nil
}

// PutNamespaceConfig 写某个命名空间的默认 vars（CAS + 单调 revision）。
//
// 每次成功写入：revision +1（namespaceVarsRevision）、resource_version +1。
// 值校验见 ValidateVars；违反 → ErrConfigInvalid。
func (d *DB) PutNamespaceConfig(ctx context.Context, namespaceID string, vars map[string]string, rv int64) (*NamespaceConfig, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if rv <= 0 {
		return nil, fmt.Errorf("%w: 命名空间配置写入必须带 resourceVersion", ErrResourceVersionRequired)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	if err := ValidateVars(vars); err != nil {
		return nil, err
	}
	varsJSON, err := marshalStringMap(vars)
	if err != nil {
		return nil, err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("put namespace config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := namespaceExistsOn(ctx, txExec{tx: tx}, namespaceID); err != nil {
		return nil, err
	}
	var curRevision, curRV int64
	// 无行 = 默认行（revision 0 / resource_version 1），见 GetNamespaceConfig。
	curRV = 1
	err = tx.QueryRowContext(ctx,
		`SELECT revision, resource_version FROM namespace_configs WHERE namespace_id = ?`,
		namespaceID).Scan(&curRevision, &curRV)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("put namespace config: read current: %w", err)
	}
	if rv != curRV {
		return nil, fmt.Errorf("%w: 命名空间配置 resourceVersion %d 已旧（当前 %d）",
			ErrResourceVersionConflict, rv, curRV)
	}
	nextRevision, nextRV := curRevision+1, curRV+1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespace_configs
		   (namespace_id, vars, revision, resource_version, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(namespace_id) DO UPDATE SET
		   vars = excluded.vars,
		   revision = excluded.revision,
		   resource_version = excluded.resource_version,
		   updated_at = excluded.updated_at`,
		namespaceID, varsJSON, nextRevision, nextRV, Now()); err != nil {
		return nil, fmt.Errorf("put namespace config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("put namespace config: commit: %w", err)
	}
	return &NamespaceConfig{
		NamespaceID:     namespaceID,
		Vars:            vars,
		Revision:        nextRevision,
		ResourceVersion: nextRV,
	}, nil
}

// namespaceExistsOn 在事务内确认命名空间存在（不存在 → ErrNamespaceNotFound）。
func namespaceExistsOn(ctx context.Context, ex queryExec, namespaceID string) error {
	var one int
	err := ex.queryRowContext(ctx,
		`SELECT 1 FROM namespaces WHERE id = ?`, namespaceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNamespaceNotFound
	}
	if err != nil {
		return fmt.Errorf("check namespace exists: %w", err)
	}
	return nil
}

// ---------- 节点默认值 + 本机快照（config.md §4） ----------

// GetNodeConfig 读一台节点的中枢侧默认值与上报快照；没有配置行时返回空默认。
//
// 空默认：namespaces = ["*"]（不额外限制，以节点申报为准）、vars/limits 空、
// revision 0、resource_version 1（首次 PUT 的合法前置条件）。
// 节点不存在 → ErrNotFound（不是「返回空默认」：那会让一次写错 id 的读看起来成功）。
func (d *DB) GetNodeConfig(ctx context.Context, nodeID string) (*NodeConfig, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("get node config: empty node_id")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("get node config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cfg, err := scanNodeConfigOn(ctx, txExec{tx: tx}, nodeID)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		// 没有配置行：节点必须存在（否则是调用方写错 id）。
		if err := nodeExistsOn(ctx, txExec{tx: tx}, nodeID); err != nil {
			return nil, err
		}
		cfg = emptyNodeConfig(nodeID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("get node config: commit: %w", err)
	}
	return cfg, nil
}

// emptyNodeConfig 是「没有配置行」的默认形状（authorization 缺省 ["*"]）。
func emptyNodeConfig(nodeID string) *NodeConfig {
	return &NodeConfig{
		NodeID:          nodeID,
		Namespaces:      []string{NamespaceWildcard},
		Vars:            map[string]string{},
		Limits:          map[string]any{},
		ResourceVersion: 1,
		LocalVars:       map[string]string{},
		LocalLimits:     map[string]any{},
	}
}

// scanNodeConfigOn 读一行 node_configs；没有行返回 (nil, nil)。
func scanNodeConfigOn(ctx context.Context, ex queryExec, nodeID string) (*NodeConfig, error) {
	var (
		rawNamespaces, rawVars, rawLimits, rawLocalVars, rawLocalLimits string
		appliedAt                                                       sql.NullInt64
	)
	cfg := &NodeConfig{NodeID: nodeID}
	err := ex.queryRowContext(ctx,
		`SELECT namespaces, vars, limits, local_vars, local_limits,
		        revision, resource_version, applied_revision, applied_hash, applied_at
		   FROM node_configs WHERE node_id = ?`, nodeID).
		Scan(&rawNamespaces, &rawVars, &rawLimits, &rawLocalVars, &rawLocalLimits,
			&cfg.Revision, &cfg.ResourceVersion, &cfg.AppliedRevision, &cfg.AppliedHash, &appliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if cfg.Namespaces, err = decodeStringList(rawNamespaces); err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if cfg.Vars, err = decodeStringMap(rawVars); err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if cfg.Limits, err = decodeAnyMap(rawLimits); err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if cfg.LocalVars, err = decodeStringMap(rawLocalVars); err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if cfg.LocalLimits, err = decodeAnyMap(rawLocalLimits); err != nil {
		return nil, fmt.Errorf("get node config: %w", err)
	}
	if appliedAt.Valid {
		v := appliedAt.Int64
		cfg.AppliedAt = &v
	}
	return cfg, nil
}

// nodeExistsOn 在事务内确认节点存在（不存在 → ErrNotFound）。
func nodeExistsOn(ctx context.Context, ex queryExec, nodeID string) error {
	var one int
	err := ex.queryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, nodeID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check node exists: %w", err)
	}
	return nil
}

// PutNodeConfig 写一台节点的中枢侧默认值（CAS：cfg.ResourceVersion 是前置条件）。
//
// 每次成功写入：revision +1（configRevision）、resource_version +1；
// 不碰 applied_* 与本机快照（那是节点回报/上报的事实）。
// 成功后把新 revision/resourceVersion 就地写回 cfg。
func (d *DB) PutNodeConfig(ctx context.Context, cfg *NodeConfig) error {
	if cfg == nil {
		return fmt.Errorf("put node config: nil")
	}
	if cfg.NodeID == "" {
		return fmt.Errorf("put node config: empty node_id")
	}
	if cfg.ResourceVersion <= 0 {
		return fmt.Errorf("%w: 节点配置写入必须带 resourceVersion", ErrResourceVersionRequired)
	}
	namespaces := normalizeNamespaceList(cfg.Namespaces)
	// 只有「没给」（nil）才补缺省 ["*"]：显式空列表 = 谁都不授权。
	// 反过来（空 → 全授权）是权限扩张，绝不能替调用方做。
	if cfg.Namespaces == nil {
		namespaces = []string{NamespaceWildcard}
	}
	if err := ValidateConfigNamespaces(namespaces); err != nil {
		return err
	}
	if cfg.Vars == nil {
		cfg.Vars = map[string]string{}
	}
	if err := ValidateVars(cfg.Vars); err != nil {
		return err
	}
	if cfg.Limits == nil {
		cfg.Limits = map[string]any{}
	}
	if err := ValidateLimits(cfg.Limits); err != nil {
		return err
	}
	namespacesJSON, err := marshalStringList(namespaces)
	if err != nil {
		return err
	}
	varsJSON, err := marshalStringMap(cfg.Vars)
	if err != nil {
		return err
	}
	limitsJSON, err := marshalAnyMap(cfg.Limits)
	if err != nil {
		return err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put node config: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := nodeExistsOn(ctx, txExec{tx: tx}, cfg.NodeID); err != nil {
		return err
	}
	var curRevision, curRV int64
	curRV = 1 // 无行 = 默认行（见 GetNodeConfig）
	err = tx.QueryRowContext(ctx,
		`SELECT revision, resource_version FROM node_configs WHERE node_id = ?`,
		cfg.NodeID).Scan(&curRevision, &curRV)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("put node config: read current: %w", err)
	}
	if cfg.ResourceVersion != curRV {
		return fmt.Errorf("%w: 节点 %s 的配置 resourceVersion %d 已旧（当前 %d）",
			ErrResourceVersionConflict, cfg.NodeID, cfg.ResourceVersion, curRV)
	}
	nextRevision, nextRV := curRevision+1, curRV+1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_configs
		   (node_id, namespaces, vars, limits, revision, resource_version, updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   namespaces = excluded.namespaces,
		   vars = excluded.vars,
		   limits = excluded.limits,
		   revision = excluded.revision,
		   resource_version = excluded.resource_version,
		   updated_at = excluded.updated_at`,
		cfg.NodeID, namespacesJSON, varsJSON, limitsJSON,
		nextRevision, nextRV, Now()); err != nil {
		return fmt.Errorf("put node config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("put node config: commit: %w", err)
	}
	cfg.Namespaces = namespaces
	cfg.Revision = nextRevision
	cfg.ResourceVersion = nextRV
	return nil
}

// PutNodeConfigBatch 批量写节点默认值：逐台独立事务、各自 CAS、各自 revision+1。
//
// 返回失败清单（一台失败不连坐其它台）；空清单 = 全部成功。
// 每台的失败原因原样保留（*ErrResourceVersionConflict / ErrConfigInvalid / ErrNotFound）。
func (d *DB) PutNodeConfigBatch(ctx context.Context, cfgs []*NodeConfig) []NodeConfigFailure {
	failed := []NodeConfigFailure{}
	for _, cfg := range cfgs {
		if err := d.PutNodeConfig(ctx, cfg); err != nil {
			nodeID := ""
			if cfg != nil {
				nodeID = cfg.NodeID
			}
			failed = append(failed, NodeConfigFailure{NodeID: nodeID, Err: err})
		}
	}
	return failed
}

// RecordApplied 记录节点回报的机器级配置已生效（config.md §4.2 的 config.applied）。
//
// 只碰 applied_* 三列：revision 是中枢的配置生效版本，节点只能**回报它应用了哪一版**，
// 不能改写它（那会让「配置没生效」凭空变成「已生效」）。
// 没有配置行时补一行默认值（节点先于中枢配置连上是正常时序）。
func (d *DB) RecordApplied(ctx context.Context, nodeID string, revision int64, hash string) error {
	if nodeID == "" {
		return fmt.Errorf("record applied: empty node_id")
	}
	if revision < 0 {
		return fmt.Errorf("record applied: negative revision %d", revision)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record applied: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := nodeExistsOn(ctx, txExec{tx: tx}, nodeID); err != nil {
		return err
	}
	now := Now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_configs
		   (node_id, applied_revision, applied_hash, applied_at, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   applied_revision = excluded.applied_revision,
		   applied_hash = excluded.applied_hash,
		   applied_at = excluded.applied_at,
		   updated_at = excluded.updated_at`,
		nodeID, revision, hash, now, now); err != nil {
		return fmt.Errorf("record applied: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record applied: commit: %w", err)
	}
	return nil
}

// RecordHello 落一次 hello 上报（config.md §4.2 / anode.md §3.3, D-I3）。
//
// 写两处，同一个事务：
//  1. nodes：facts / declared_capabilities / declared_namespaces / agent 版本 / last_seen_at
//     —— 都是**节点自报的事实**，中枢只存快照；
//  2. node_configs：local_vars / local_limits（本机值快照，只用于匹配与展示）。
//
// **不改节点默认值**（vars/limits/namespaces/revision）：默认值是中枢配置，
// hello 只上报本机那份；两者同名时按 local > node > namespace 逐键解析。
// 节点不存在 → ErrNotFound（token 定位失败必须响，不能静默丢配置）。
func (d *DB) RecordHello(ctx context.Context, nodeID string, facts NodeFacts,
	capabilities NodeCapabilityReport, declaredNamespaces []string,
	localVars map[string]string, localLimits map[string]any,
	agentVersion, agentCommit string) error {
	if nodeID == "" {
		return fmt.Errorf("record hello: empty node_id")
	}
	declaredNamespaces = normalizeNamespaceList(declaredNamespaces)
	if err := ValidateConfigNamespaces(declaredNamespaces); err != nil {
		return err
	}
	if localVars == nil {
		localVars = map[string]string{}
	}
	if err := ValidateVars(localVars); err != nil {
		return err
	}
	if localLimits == nil {
		localLimits = map[string]any{}
	}
	if err := ValidateLimits(localLimits); err != nil {
		return err
	}
	if capabilities == nil {
		capabilities = NodeCapabilityReport{}
	}
	for i := range capabilities {
		if capabilities[i].Commands == nil {
			capabilities[i].Commands = []protocol.HelloCommand{}
		}
	}

	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("record hello: marshal facts: %w", err)
	}
	capsJSON, err := json.Marshal(capabilities)
	if err != nil {
		return fmt.Errorf("record hello: marshal capabilities: %w", err)
	}
	namespacesJSON, err := marshalStringList(declaredNamespaces)
	if err != nil {
		return err
	}
	localVarsJSON, err := marshalStringMap(localVars)
	if err != nil {
		return err
	}
	localLimitsJSON, err := marshalAnyMap(localLimits)
	if err != nil {
		return err
	}
	now := Now()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record hello: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE nodes
		    SET facts = ?, declared_capabilities = ?, declared_namespaces = ?,
		        agent_version = ?, agent_commit = ?, last_seen_at = ?,
		        resource_version = resource_version + 1
		  WHERE id = ?`,
		string(factsJSON), string(capsJSON), namespacesJSON,
		agentVersion, agentCommit, now, nodeID)
	if err != nil {
		return fmt.Errorf("record hello: update node: %w", err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil {
		return fmt.Errorf("record hello: rows: %w", rerr)
	} else if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_configs (node_id, local_vars, local_limits, updated_at)
		 VALUES (?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   local_vars = excluded.local_vars,
		   local_limits = excluded.local_limits,
		   updated_at = excluded.updated_at`,
		nodeID, localVarsJSON, localLimitsJSON, now); err != nil {
		return fmt.Errorf("record hello: upsert local snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record hello: commit: %w", err)
	}
	return nil
}

// EffectiveVars 解析一批名字在 (node, ns) 上下文里的逐键生效值。
//
// 优先级 local > node > namespace（config.md §2.1）：每个名字独立判定，
// 不是整份覆盖；三层都没有 = 该名字**不出现**（缺失不注入空串）。
func (d *DB) EffectiveVars(ctx context.Context, nodeID, namespaceID string, names []string) (map[string]EffectiveVar, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("effective vars: empty node_id")
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	localVars, nodeVars, nsVars, err := d.loadVarsLayers(ctx, nodeID, namespaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]EffectiveVar, len(names))
	for _, raw := range names {
		name, ok := varRefName(raw)
		if !ok {
			continue
		}
		if v, hit := localVars[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceLocal}
			continue
		}
		if v, hit := nodeVars[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceNode}
			continue
		}
		if v, hit := nsVars[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceNamespace}
		}
	}
	return out, nil
}

// loadVarsLayers 一次读出三层 vars（机器级两层 + 任务 ns 一层）。
func (d *DB) loadVarsLayers(ctx context.Context, nodeID, namespaceID string) (local, node, ns map[string]string, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load vars layers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := nodeExistsOn(ctx, txExec{tx: tx}, nodeID); err != nil {
		return nil, nil, nil, err
	}
	if err := namespaceExistsOn(ctx, txExec{tx: tx}, namespaceID); err != nil {
		return nil, nil, nil, err
	}
	local, node = map[string]string{}, map[string]string{}
	var rawLocal, rawNode string
	qerr := tx.QueryRowContext(ctx,
		`SELECT local_vars, vars FROM node_configs WHERE node_id = ?`, nodeID).
		Scan(&rawLocal, &rawNode)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return nil, nil, nil, fmt.Errorf("load vars layers: node config: %w", qerr)
	}
	if qerr == nil {
		if local, err = decodeStringMap(rawLocal); err != nil {
			return nil, nil, nil, fmt.Errorf("load vars layers: node local vars: %w", err)
		}
		if node, err = decodeStringMap(rawNode); err != nil {
			return nil, nil, nil, fmt.Errorf("load vars layers: node vars: %w", err)
		}
	}
	ns = map[string]string{}
	var rawNS string
	qerr = tx.QueryRowContext(ctx,
		`SELECT vars FROM namespace_configs WHERE namespace_id = ?`, namespaceID).Scan(&rawNS)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return nil, nil, nil, fmt.Errorf("load vars layers: namespace config: %w", qerr)
	}
	if qerr == nil {
		if ns, err = decodeStringMap(rawNS); err != nil {
			return nil, nil, nil, fmt.Errorf("load vars layers: namespace vars: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("load vars layers: commit: %w", err)
	}
	return local, node, ns, nil
}

// varRefName 把 `vars.<NAME>`（或裸名）归一成名字；返回 ok=false 表示不是 vars 引用。
func varRefName(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	name := strings.TrimPrefix(s, "vars.")
	if name == "" {
		return "", false
	}
	return name, true
}

// ResolveRequiredValues 判定 `spec.requires.values` 里每个 `vars.*` 名字都能解析。
//
// 只处理 `vars.*`：`status.*` / `metadata.*` 是节点内置事实，不参与匹配
// （anode.md §3.6：它们不用写进 requires.values）。格式坏掉的元素按缺失处理
// （宁可少派，也不把拼错的引用当成"不需要值"）。
//
// missing 保留调用方给的**原字符串**（`vars.SOURCE_ROOT`），直接可回显给用户。
func (d *DB) ResolveRequiredValues(ctx context.Context, nodeID, namespaceID string, values []string) (bool, []string, error) {
	names := make([]string, 0, len(values))
	for _, raw := range values {
		s := strings.TrimSpace(raw)
		if !strings.HasPrefix(s, "vars.") {
			continue // 内置事实（status./metadata.）不参与
		}
		name, ok := varRefName(s)
		if !ok {
			continue
		}
		names = append(names, name)
	}
	effective, err := d.EffectiveVars(ctx, nodeID, namespaceID, names)
	if err != nil {
		return false, nil, err
	}
	missing := []string{}
	for _, raw := range values {
		s := strings.TrimSpace(raw)
		if !strings.HasPrefix(s, "vars.") {
			continue
		}
		name, ok := varRefName(s)
		if !ok {
			missing = append(missing, s) // 形状坏 = 缺失（不静默当"无需值"）
			continue
		}
		if _, hit := effective[name]; !hit {
			missing = append(missing, s)
		}
	}
	return len(missing) == 0, missing, nil
}

// EffectiveLimits 返回一台节点的机器级生效并发预算（local ⊕ node）。
//
// 只有两层（anode.md §2 / C16）：total 同名覆盖；perCapability 递归到每个能力键，
// local 同名覆盖 node、未写继承。没有 ns 层。
func (d *DB) EffectiveLimits(ctx context.Context, nodeID string) (map[string]any, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("effective limits: empty node_id")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effective limits: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := nodeExistsOn(ctx, txExec{tx: tx}, nodeID); err != nil {
		return nil, err
	}
	nodeLimits, localLimits := map[string]any{}, map[string]any{}
	var rawNode, rawLocal string
	qerr := tx.QueryRowContext(ctx,
		`SELECT limits, local_limits FROM node_configs WHERE node_id = ?`, nodeID).
		Scan(&rawNode, &rawLocal)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return nil, fmt.Errorf("effective limits: %w", qerr)
	}
	if qerr == nil {
		if nodeLimits, err = decodeAnyMap(rawNode); err != nil {
			return nil, fmt.Errorf("effective limits: node limits: %w", err)
		}
		if localLimits, err = decodeAnyMap(rawLocal); err != nil {
			return nil, fmt.Errorf("effective limits: local limits: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effective limits: commit: %w", err)
	}
	return mergeLimits(nodeLimits, localLimits), nil
}

// mergeLimits 递归逐叶合并两层 limits：后者（local）同名叶子覆盖前者（node）。
func mergeLimits(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		if childOverride, ok := v.(map[string]any); ok {
			if childBase, ok := out[k].(map[string]any); ok {
				out[k] = mergeLimits(childBase, childOverride)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// AuthorizedNamespaces 返回一台节点的**生效**命名空间列表（申报 ∩ 授权）。
//
// 两边各有一份列表：node_configs.namespaces 是中枢授权（缺省 ["*"]），
// nodes.declared_namespaces 是本机申报（hello 上报）。任一侧 "*" = 全部；
// 这是权限，只能收紧，**不做** local 优先（anode.md D7）。
// 返回 ns 名（slug）或 ["*"]（两边都是 "*"）。
func (d *DB) AuthorizedNamespaces(ctx context.Context, nodeID string) ([]string, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("authorized namespaces: empty node_id")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("authorized namespaces: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var rawDeclared string
	err = tx.QueryRowContext(ctx,
		`SELECT declared_namespaces FROM nodes WHERE id = ?`, nodeID).Scan(&rawDeclared)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("authorized namespaces: %w", err)
	}
	declared, err := decodeStringList(rawDeclared)
	if err != nil {
		return nil, fmt.Errorf("authorized namespaces: %w", err)
	}
	authorized := []string{NamespaceWildcard}
	var rawAuthorized string
	qerr := tx.QueryRowContext(ctx,
		`SELECT namespaces FROM node_configs WHERE node_id = ?`, nodeID).Scan(&rawAuthorized)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return nil, fmt.Errorf("authorized namespaces: %w", qerr)
	}
	if qerr == nil {
		if authorized, err = decodeStringList(rawAuthorized); err != nil {
			return nil, fmt.Errorf("authorized namespaces: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("authorized namespaces: commit: %w", err)
	}
	return intersectNamespaces(authorized, declared), nil
}

// intersectNamespaces 计算「申报 ∩ 授权」，两边都支持 "*"。
//
// 结果：两边都 "*" → ["*"]；一侧 "*" → 另一侧；否则取交集（按授权顺序）。
func intersectNamespaces(authorized, declared []string) []string {
	authz := normalizeNamespaceList(authorized)
	decl := normalizeNamespaceList(declared)
	authAll, declAll := hasWildcard(authz), hasWildcard(decl)
	switch {
	case authAll && declAll:
		return []string{NamespaceWildcard}
	case authAll:
		return decl
	case declAll:
		return authz
	}
	declSet := make(map[string]struct{}, len(decl))
	for _, n := range decl {
		declSet[n] = struct{}{}
	}
	out := make([]string, 0, len(authz))
	seen := make(map[string]struct{}, len(authz))
	for _, n := range authz {
		if _, ok := declSet[n]; !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// hasWildcard 报告列表里是否有 "*"。
func hasWildcard(list []string) bool {
	for _, n := range list {
		if n == NamespaceWildcard {
			return true
		}
	}
	return false
}

// namespacesAllow 报告生效列表是否覆盖某个 ns 名。
func namespacesAllow(effective []string, namespaceName string) bool {
	if hasWildcard(effective) {
		return true
	}
	for _, n := range effective {
		if n == namespaceName {
			return true
		}
	}
	return false
}

// normalizeNamespaceList 去空白、去空项、去重（保持首次出现顺序）。
func normalizeNamespaceList(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
