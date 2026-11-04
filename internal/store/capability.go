package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 能力来源（契约 §2.2 source 列）。
const (
	CapSourceBuiltin = "builtin"
	CapSourceCustom  = "custom"
)

// ErrCapabilityReserved 表示试图用内置能力的保留名字创建/覆盖定义。
//
// 内置起点（github/cron/http/manual）的名字是保留的：findOrigin 取该 name 的
// 最高版本，若允许用户开 github@2，source=github 就会解析到自定义版本上——
// 而自定义版本不享受起点的「任意事件类型」出口，建好的边全废且无任何提示。
var ErrCapabilityReserved = errors.New("capability name reserved")

// Lifecycle 是能力定义里的生命周期参数（JSON 存在 lifecycle 列）。
type Lifecycle struct {
	ExpiresInSec int  `json:"expiresInSec,omitempty"`
	Retryable    bool `json:"retryable,omitempty"`
	MaxAttempts  int  `json:"maxAttempts,omitempty"`
	ResultTTLSec int  `json:"resultTtlSec,omitempty"`
	// Idempotent 为 false 表示重放不安全：租约过期后直接 abandoned，不回 pending（契约 §3.2）。
	Idempotent *bool `json:"idempotent,omitempty"`
}

// IsIdempotent 默认 true：没声明就当可重试（与契约 §3.2 的 "实现 idempotent:false → abandoned" 对应）。
func (l Lifecycle) IsIdempotent() bool {
	if l.Idempotent == nil {
		return true
	}
	return *l.Idempotent
}

// DefaultMaxAttempts 是 lifecyle 未声明 maxAttempts 时的默认重试上限。
const DefaultMaxAttempts = 3

// MaxAttempts 返回有效重试上限：生命周期的 maxAttempts，未声明用默认值。
func (l Lifecycle) MaxAttemptsOrDefault() int {
	if l.MaxAttempts > 0 {
		return l.MaxAttempts
	}
	return DefaultMaxAttempts
}

// CapabilityVersion 是 capability_versions 表的一行：一个 name@version 的不可变定义（不变量 I8）。
//
// Outputs 是出口名的唯一来源，同时是 edges.from_port 的封闭集合（不变量 I2）。
// 没有 Kind 字段——kind 由 DeriveKind 推导，不落库（不变量 I3）。
type CapabilityVersion struct {
	ID string `json:"id"`
	// NamespaceID 是**隔离维度**（I11）：能力定义属于命名空间，
	// 同一个 name@version 在 prod 与 staging 是两个不同的定义。
	NamespaceID string `json:"namespaceId"`
	Name        string `json:"name"`
	// DisplayName 是**纯展示**标签（用户自己取的名字，如「分析 PR」）。
	// 空串表示没取，前端回落到 name@version。
	// 它不参与路由、不参与 content_hash —— 改它不该产生新版本。
	DisplayName  string          `json:"displayName"`
	Version      int             `json:"version"`
	ParamsSchema json.RawMessage `json:"paramsSchema"`
	Outputs      []string        `json:"outputs"`
	Lifecycle    Lifecycle       `json:"lifecycle"`
	Sticky       string          `json:"sticky"`
	Source       string          `json:"source"`
	ContentHash  string          `json:"contentHash"`
	CreatedAt    int64           `json:"createdAt"`
}

// capHashInput 是参与 content_hash 计算的规范化结构。
//
// 参与：name（能力身份）、params_schema（键排序后）、outputs（排序后）、lifecycle、sticky。
// 不参与：version / id / created_at —— 「同内容不同版本」要能被识别成同内容，
// 否则 UNIQUE(namespace_id, content_hash) 永远不冲突，幂等创建就失效了。
//
// 为什么 name 必须参与：`UNIQUE(namespace_id, content_hash)` 是**命名空间内**唯一的。
// 若 name 不参与，`fix1` 与 `fix2`（schema/outputs 恰好相同但名字不同）
// 会被当成「同一份内容」，第二个 CreateCapability 会把 fix2 的行改写成 fix1——
// 两个不同能力被默默合并成一个，edges 两端指向同一个 capability_id。
type capHashInput struct {
	Name         string          `json:"name"`
	ParamsSchema json.RawMessage `json:"params_schema"`
	Outputs      []string        `json:"outputs"`
	Lifecycle    Lifecycle       `json:"lifecycle"`
	Sticky       string          `json:"sticky"`
}

// NormalizeCapability 计算 content_hash：把 params_schema / outputs / lifecycle / sticky
// 规范化（键排序、稳定序列化）后取 sha256。同内容必须得同 hash。
func NormalizeCapability(c *CapabilityVersion) (string, error) {
	if c == nil {
		return "", fmt.Errorf("normalize capability: nil")
	}
	// params_schema：JSON 目标的键顺序不稳定（map 遍历），必须先归一化。
	// 空 / 非法 JSON 一律归一为空对象，避免「没写 schema」和「写了 {}」算出不同 hash。
	schema := json.RawMessage("{}")
	if len(c.ParamsSchema) > 0 && json.Valid(c.ParamsSchema) {
		normalized, err := canonicalJSON(c.ParamsSchema)
		if err != nil {
			return "", fmt.Errorf("normalize params_schema: %w", err)
		}
		schema = normalized
	}
	// outputs：集合语义，排序后参与 hash（["a","b"] 与 ["b","a"] 是同一份定义）。
	outputs := make([]string, 0, len(c.Outputs))
	for _, o := range c.Outputs {
		if o == "" {
			continue // 空出口名无意义，不参与 hash（也进不了 edges）
		}
		outputs = append(outputs, o)
	}
	sort.Strings(outputs)

	payload, err := json.Marshal(capHashInput{
		Name:         c.Name,
		ParamsSchema: schema,
		Outputs:      outputs,
		Lifecycle:    c.Lifecycle,
		Sticky:       c.Sticky,
	})
	if err != nil {
		return "", fmt.Errorf("marshal capability for hash: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON 把 JSON 归一化为键排序的紧凑形式。
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal json: %w", err)
	}
	out, err := json.Marshal(sortJSON(v))
	if err != nil {
		return nil, fmt.Errorf("marshal canonical json: %w", err)
	}
	return out, nil
}

// sortJSON 递归地把 map 转成键排序的紧凑结构（json.Marshal 对 map 的键本身
// 就是按字节序排序的，这里只是显式走一遍，避免依赖未来实现细节）。
func sortJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = sortJSON(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sortJSON(e)
		}
		return out
	default:
		return v
	}
}

// CreateCapability 追加一个能力版本（**不**越过升级闸门：等价于 force=false）。
//
// 同内容（content_hash 相同）不产生第二行：返回 nil 且把 c 填成已有那行——
// 这是「幂等创建」，不是错误。
//
// 注意 UNIQUE(namespace_id, name, version) 仍然生效：同一 name 的同版本不允许
// 用**不同内容**覆盖（那是改定义，不是追加版本）。
//
// 新版本成为 latest 且**破坏**已有 name 型 link 时返回 *UpgradeBreakingError
// （errors.Is(err, ErrUpgradeBreaking)）：那是一次「改线」，必须由人显式确认，
// 见 CreateCapabilityForce / CheckUpgrade（typed-pipeline §3.6）。
func (d *DB) CreateCapability(ctx context.Context, c *CapabilityVersion) error {
	return d.CreateCapabilityForce(ctx, c, false)
}

// CreateCapabilityForce 与 CreateCapability 相同，但可以由调用方显式越过升级闸门。
//
// force=true 表示「我知道哪些 link 会断，仍然要推」：闸门只记录/列出影响面，
// 不再拒绝。它绝不修改 link 本身——改线仍然是用户的动作（把 link 钉到旧版，
// 或者去改上游），中枢不替任何人做这个决定。
//
// 每次调用是一个**独立事务**：写入与幂等回读、升级闸门的读都在同一快照里
// （见 createCapabilityTx）。要把它和建边、产物放进同一个事务，用
// CreateCapabilityWithLinks / CreateCapabilityWithLinksAndArtifact。
func (d *DB) CreateCapabilityForce(ctx context.Context, c *CapabilityVersion, force bool) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create capability: begin tx: %w", err)
	}
	// 早退 / panic 都回滚：append-only 表里留下半条数据是不可撤的。
	defer func() { _ = tx.Rollback() }()
	if err := createCapabilityTx(ctx, txExec{tx: tx}, c, force); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create capability: commit: %w", err)
	}
	return nil
}

// createCapabilityTx 是建能力的**唯一**实现（CreateCapabilityForce 只是给它开事务）。
//
// ex 决定「读走哪里」：事务外传 *DB，事务内传 txExec。所有读都必须经 ex——
// 走 d 的话，事务里读不到本事务刚写的行（见 txexec.go 的注释）。
func createCapabilityTx(ctx context.Context, ex queryExec, c *CapabilityVersion, force bool) error {
	if c == nil {
		return fmt.Errorf("create capability: nil")
	}
	if c.NamespaceID == "" || c.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if c.Name == "" {
		return fmt.Errorf("create capability: empty name")
	}
	if c.Version <= 0 {
		return fmt.Errorf("create capability: version must be > 0, got %d", c.Version)
	}
	// 内置起点的名字是**保留**的，用户不能开新版本接管它。
	//
	// 为什么必须拦：findOrigin 取该 name 的**最高版本**。用户建了 github@2
	// 之后，source=github 就会解析到 @2——而 @2 是自定义能力，不享受起点的
	// 「任意事件类型」出口，于是建好的边全废、事件派不出去，且没有任何提示。
	// 这个失败方式极其隐蔽，所以在写入处直接拒绝。
	//
	// 注意要放行 source=builtin：内置能力自己的播种（ensureOneBuiltin）
	// 也走这条路径，否则会把播种自己也拦死。
	if c.Source != CapSourceBuiltin {
		if bc, ok := IsBuiltinCapabilityName(c.Name); ok && bc.Origin {
			return fmt.Errorf("%w: %q is a builtin origin and its name is reserved", ErrCapabilityReserved, c.Name)
		}
	}
	hash, err := NormalizeCapability(c)
	if err != nil {
		return err
	}

	// 升级闸门（P2.2，设计 §3.6）：只有当这个版本**成为 latest** 时才拦。
	// 放在写入之前：不能先写进去再问（问了也没用，append-only 撤不回）。
	//
	// 同内容的请求先放过：那种请求会被下面的幂等路径折叠成**已有的那一行**
	// （它不会成为新版本），拦住它只会让一个「什么都没变」的调用失败。
	if !force {
		existing, herr := getCapabilityByHashOn(ctx, ex, c.NamespaceID, hash)
		switch {
		case herr == nil && existing != nil:
			// 幂等重放：不改线，不过闸门
		case herr != nil && !errors.Is(herr, ErrNotFound):
			return fmt.Errorf("check existing capability by hash: %w", herr)
		default:
			impact, ierr := checkUpgradeOn(ctx, ex, c.NamespaceID, c)
			if ierr != nil {
				return ierr
			}
			if impact.Breaking {
				return &UpgradeBreakingError{Impact: impact}
			}
		}
	}
	if c.Source == "" {
		c.Source = CapSourceCustom
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = Now()
	}
	if c.ID == "" {
		c.ID = NewCapabilityID(hash)
	}
	outputs := c.Outputs
	if outputs == nil {
		outputs = []string{}
	}
	outputsJSON, err := json.Marshal(outputs)
	if err != nil {
		return fmt.Errorf("marshal outputs: %w", err)
	}
	lifecycleJSON, err := json.Marshal(c.Lifecycle)
	if err != nil {
		return fmt.Errorf("marshal lifecycle: %w", err)
	}
	schema := string(c.ParamsSchema)
	if schema == "" {
		schema = "{}"
	}

	_, err = ex.execContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.NamespaceID, c.Name, c.DisplayName, c.Version, schema, string(outputsJSON),
		string(lifecycleJSON), c.Sticky, c.Source, hash, c.CreatedAt)
	if err == nil {
		c.ContentHash = hash
		return nil
	}
	if !isUniqueConstraintError(err) {
		return fmt.Errorf("insert capability: %w", err)
	}

	// 冲突：要么是同内容（幂等，返回已有行），要么是同 name@version 换内容（真错）。
	existing, gerr := getCapabilityByHashOn(ctx, ex, c.NamespaceID, hash)
	if gerr == nil && existing != nil {
		// 同内容已存在 → 幂等返回已有行。
		//
		// **但 display_name 要单独处理**：它不参与 hash，所以"内容相同"
		// 不代表"显示名相同"。如果这次请求带了 displayName 而旧行没有
		// （或不同），就把它落库 —— 否则用户填的名字会被静默吞掉，
		// 界面上看起来"我明明改了却没生效"。
		//
		// 这里只 UPDATE 装饰字段，不违反 append-only（I8）：
		// 能力定义本身（name/schema/outputs/lifecycle/sticky）一个字节都没动。
		if want := strings.TrimSpace(c.DisplayName); want != "" && want != existing.DisplayName {
			if uerr := setCapabilityDisplayNameOn(ctx, ex, c.NamespaceID, existing.ID, want); uerr != nil {
				return fmt.Errorf("update display name on idempotent create: %w", uerr)
			}
			existing.DisplayName = want
		}
		*c = *existing
		return nil
	}
	if errors.Is(gerr, ErrNotFound) || gerr == nil {
		// 不是 content_hash 冲突，那就是 (namespace_id, name, version) 冲突
		if _, e2 := getCapabilityOn(ctx, ex, c.NamespaceID, c.Name, c.Version); e2 == nil {
			return fmt.Errorf("create capability %s@%d: version already exists with different content (append-only, 见不变量 I8)", c.Name, c.Version)
		}
	}
	if gerr != nil && !errors.Is(gerr, ErrNotFound) {
		return fmt.Errorf("lookup existing capability after conflict: %w", gerr)
	}
	return fmt.Errorf("insert capability: %w", err)
}

// getCapabilityByHash 按 (namespace_id, content_hash) 取已有版本（事务外；读走语句缓存）。
//
// 唯一键在 #162 之后是 UNIQUE(namespace_id, content_hash)：同内容的幂等判定
// 只在**命名空间内**成立（prod 与 staging 可以各有一份同样的定义）。
func (d *DB) getCapabilityByHash(ctx context.Context, namespaceID, hash string) (*CapabilityVersion, error) {
	return getCapabilityByHashOn(ctx, d, namespaceID, hash)
}

// getCapabilityByHashOn 是 getCapabilityByHash 的 ex 版（事务内外共用一份 SQL）。
func getCapabilityByHashOn(ctx context.Context, ex queryExec, namespaceID, hash string) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions WHERE namespace_id = ? AND content_hash = ?`,
		namespaceID, hash)
}

// capabilityColumns 是统一的列顺序，scanCapabilityOn 依赖它。
const capabilityColumns = `id, namespace_id, name, display_name, version, params_schema,
	outputs, lifecycle, sticky, source, content_hash, created_at`

// scanCapability 执行查询并解析一行；未命中返回 ErrNotFound（事务外；读走语句缓存）。
func (d *DB) scanCapability(ctx context.Context, query string, args ...any) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, d, query, args...)
}

// scanCapabilityOn 是 scanCapability 的 ex 版：所有读都经 ex，
// 事务内传 txExec 才能看到本事务刚写入的行（见 txexec.go）。
func scanCapabilityOn(ctx context.Context, ex queryExec, query string, args ...any) (*CapabilityVersion, error) {
	var c CapabilityVersion
	var paramsSchema, outputs, lifecycle string
	// 语句缓存（#242）：scanCapabilityOn 被 GetCapability / GetCapabilityByID /
	// getCapabilityByHash 共用，三者的 SQL 文本都是常量（拼的只是列名/条件名）。
	// 事务内（ex=txExec）**不能**用缓存：那些语句绑的是池里另一条连接，看不到本事务的写。
	err := ex.queryRowContext(ctx, query, args...).Scan(
		&c.ID, &c.NamespaceID, &c.Name, &c.DisplayName, &c.Version, &paramsSchema, &outputs,
		&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan capability: %w", err)
	}
	if err := json.Unmarshal([]byte(paramsSchema), &c.ParamsSchema); err != nil {
		return nil, fmt.Errorf("decode capability params_schema: %w", err)
	}
	if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
		return nil, fmt.Errorf("decode capability outputs: %w", err)
	}
	if c.Outputs == nil {
		c.Outputs = []string{} // 不变量 I9：空列表是 [] 不是 null
	}
	if err := json.Unmarshal([]byte(lifecycle), &c.Lifecycle); err != nil {
		return nil, fmt.Errorf("decode capability lifecycle: %w", err)
	}
	return &c, nil
}

// GetCapability 按 name + version 取能力定义；未命中返回 ErrNotFound。
func (d *DB) GetCapability(ctx context.Context, namespaceID, name string, version int) (*CapabilityVersion, error) {
	return getCapabilityOn(ctx, d, namespaceID, name, version)
}

// getCapabilityOn 是 GetCapability 的 ex 版（事务内读必须走它，否则看不到刚写的行）。
func getCapabilityOn(ctx context.Context, ex queryExec, namespaceID, name string, version int) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND name = ? AND version = ?`, namespaceID, name, version)
}

// GetCapabilityByID 按 ID 取能力定义；未命中返回 ErrNotFound。
// 必须同时校验 namespace_id（命名空间隔离）：能力 id 全局唯一，但读路径仍要过隔离维度。
func (d *DB) GetCapabilityByID(ctx context.Context, namespaceID, id string) (*CapabilityVersion, error) {
	return getCapabilityByIDOn(ctx, d, namespaceID, id)
}

// getCapabilityByIDOn 是 GetCapabilityByID 的 ex 版。
func getCapabilityByIDOn(ctx context.Context, ex queryExec, namespaceID, id string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions WHERE namespace_id = ? AND id = ?`, namespaceID, id)
}

// SetCapabilityDisplayName 只更新 display_name 一列。
//
// **不碰 content_hash**：显示名不参与 hash 计算（见 capHashInput 注释），
// 所以改它不会产生新版本，也不会让 UNIQUE(namespace_id, content_hash) 冲突。
//
// 这是这张表上唯一的 UPDATE。其余字段全都 append-only（不变量 I8）——
// display_name 是装饰字段，不属于"能力定义"，改它不影响任何旧 trace 的解释。
func (d *DB) SetCapabilityDisplayName(ctx context.Context, namespaceID, id, displayName string) error {
	return setCapabilityDisplayNameOn(ctx, d, namespaceID, id, displayName)
}

// setCapabilityDisplayNameOn 是 SetCapabilityDisplayName 的 ex 版：
// 幂等建能力（同内容折叠）时要在**同一个事务**里把显示名写进去。
func setCapabilityDisplayNameOn(ctx context.Context, ex queryExec, namespaceID, id, displayName string) error {
	if namespaceID == "" || namespaceID == AllNamespaces || id == "" {
		return ErrNamespaceRequired
	}
	res, err := ex.execContext(ctx,
		`UPDATE capability_versions SET display_name = ? WHERE namespace_id = ? AND id = ?`,
		displayName, namespaceID, id)
	if err != nil {
		return fmt.Errorf("set display name: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set display name rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCapabilities 列出命名空间内全部能力定义（按 name、version 升序；空列表返回 []）。
func (d *DB) ListCapabilities(ctx context.Context, namespaceID string) ([]*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? ORDER BY name, version`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer rows.Close()
	out := []*CapabilityVersion{}
	for rows.Next() {
		var c CapabilityVersion
		var paramsSchema, outputs, lifecycle string
		if err := rows.Scan(
			&c.ID, &c.NamespaceID, &c.Name, &c.DisplayName, &c.Version, &paramsSchema, &outputs,
			&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan capability row: %w", err)
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
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NextCapabilityVersion 返回某 name 的下一个版本号（无定义时返回 1）。
func (d *DB) NextCapabilityVersion(ctx context.Context, namespaceID, name string) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	var maxV sql.NullInt64
	if err := d.QueryRowContext(ctx,
		`SELECT MAX(version) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		namespaceID, name).Scan(&maxV); err != nil {
		return 0, fmt.Errorf("next capability version: %w", err)
	}
	if !maxV.Valid {
		return 1, nil
	}
	return int(maxV.Int64) + 1, nil
}

// CapabilityNamespace 返回能力定义所属的命名空间 id（I11 四处校验共用）。
//
// 为什么按 id 全局查一次：能力 id 是全局主键，但「别的命名空间的 id 被填进来」
// 在 SQL 层是合法的（外键只保证行存在，不保证同命名空间）——
// 结构防的是"写不出来"，防不了"写错了"，所以 I11 必须显式校验。
func (d *DB) CapabilityNamespace(ctx context.Context, capabilityID string) (string, error) {
	if capabilityID == "" {
		return "", ErrNotFound
	}
	var ns string
	err := d.QueryRowContext(ctx,
		`SELECT namespace_id FROM capability_versions WHERE id = ?`, capabilityID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup capability namespace: %w", err)
	}
	return ns, nil
}

// checkCapabilityInNamespace 是 I11 的公共校验：能力定义必须属于 wantNS。
//
// what 用来说明违反的是哪条引用（"edges.to_capability" / "sources.targets" /
// "node_capabilities"），让人一眼看出是哪一端写错了。
// 未命中返回 ErrNotFound（handler 翻 404），跨命名空间返回 *CrossNamespaceError（handler 翻 400）。
func (d *DB) checkCapabilityInNamespace(ctx context.Context, wantNS, capabilityID, what string) error {
	ns, err := d.CapabilityNamespace(ctx, capabilityID)
	if err != nil {
		return err
	}
	if ns != wantNS {
		return &CrossNamespaceError{What: what, Want: wantNS, Got: ns}
	}
	return nil
}

// HasOutput 判定出口名是否属于该能力定义（不变量 I2 / A5 的封闭集合检查）。
func (c *CapabilityVersion) HasOutput(port string) bool {
	for _, o := range c.Outputs {
		if o == port {
			return true
		}
	}
	return false
}
