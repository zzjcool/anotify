package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
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

// ErrCapabilityInvalid 表示能力的正文（spec）不合法（客户端可修 → HTTP 400）。
//
// **内容**错误既不是冲突也不是 500。真正的形状判定在
// artifact.Validate（规则只有一份，见 Capability Spec v3 §3.1），这里只负责把它
// 归成一个 store 层的哨兵错误，供 handler 翻 400。
var ErrCapabilityInvalid = errors.New("capability_invalid")

// ErrVersionConflict 表示 (namespace, name, version) 已经存在，但内容**不同**。
//
// 版本行不可变（不变量 I8）：同一个 name@version 不许换内容覆盖，也不会被静默
// 改写成别的版本号。v3 起 content_hash ≡ digest（吃 name + version + spec），
// 所以「同内容不同版本」天然算不出同一个 hash —— 跨版本内容折叠（旧行为：同内容
// 换版本号会被折叠成已有那一行）随之一并取消，撞 (name,version) 只可能是一次
// 真的「改定义」，必须报冲突让人显式开新版本。
var ErrVersionConflict = errors.New("store: capability version already exists with different content")

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
	// Spec 是能力的**正文**（Capability Spec v3 §4）：params / outputs / requires /
	// lifecycle / sticky + 实现（run 或 handler）。
	//
	// 其余字段里的 ParamsSchema / Outputs / Lifecycle / Sticky 是**写入时由它算出的
	// 只读派生列**（热路径要的是列，不该每次解一份 JSON）。方向永远单向：Spec →
	// 派生列，调用方设的派生字段值一律被覆盖，不允许从派生字段反推正文。
	//
	// 不进 JSON 响应（`json:"-"`）：响应里给什么形状由端点决定（列表要剥掉文件正文、
	// 换成 files:{count,bytes}；详情才给全文），能力行这个中间结构不该顺手把 1 MiB
	// 的文件正文塞进每一条列表响应。请求/响应形状的切换是 P1-b2/P4 的事。
	Spec artifact.Spec `json:"-"`
}

// Artifact 把行拼成一份完整产物：metadata 来自列（name / version / displayName），
// 正文来自 Spec 字段。
//
// 「行 → 产物」只有这一处拼装：校验、digest、回填、读取时拼产物都走它。多拼一份的后果是
// 某天有人少填一个字段，digest 与产物正文悄悄不一致（症状是部署永远被判成 stale）。
func (c *CapabilityVersion) Artifact() artifact.Artifact {
	if c == nil {
		return artifact.Artifact{}
	}
	return artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata: artifact.Metadata{
			Name:        c.Name,
			Version:     c.Version,
			DisplayName: c.DisplayName,
		},
		Spec: c.Spec,
	}
}

// Digest 返回 "sha256:<hex>" 形式的内容寻址标识。
//
// content_hash 列存的是**裸 hex**（不带前缀），两种写法是同一个值：库里留着裸 hex
// 是因为它已经是既有列，digest 形式只是它加上标准前缀（契约 §4）。
func (c *CapabilityVersion) Digest() string {
	if c == nil || c.ContentHash == "" {
		return ""
	}
	return artifact.DigestPrefix + c.ContentHash
}

// HandlerSpec 构造一份 handler 形式（内置执行器）的规格正文。
//
// 三处共用，避免各拼一份：内置能力播种（notify-phone）、预案安装（manifest 的
// runner 契约不解释 → 一律 noop 占位）、migration 8 的回填。
//
// outputs 为空时给 []（不是 nil）：空列表纪律（不变量 I9）+ 它参与 digest，
// nil 与 [] 必须是同一份规范字节。
func HandlerSpec(handler string, params json.RawMessage, outputs []string, lifecycle json.RawMessage) artifact.Spec {
	out := make([]string, 0, len(outputs))
	out = append(out, outputs...)
	return artifact.Spec{
		Params:    params,
		Outputs:   out,
		Lifecycle: lifecycle,
		Handler:   handler,
	}
}

// canonicalizeSpec 把一份产物归一成落库/算 digest/回填**共用**的那份形状。
//
// 返回的 spec 与 full 来自同一次 canonical 序列化：full 是整份产物（apiVersion /
// kind / metadata / spec，不含 displayName），spec 是它的 spec 子文档。两者必须
// 出自同一次序列化——分两次拼的话，某天会得到「存的正文」与「算 digest 的正文」
// 不是同一串字节，而症状（digest 与内容不一致）极难查。
func canonicalizeSpec(a *artifact.Artifact) (artifact.Spec, []byte, error) {
	full, err := artifact.CanonicalJSON(a)
	if err != nil {
		return artifact.Spec{}, nil, fmt.Errorf("canonical json: %w", err)
	}
	var doc struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(full, &doc); err != nil {
		return artifact.Spec{}, nil, fmt.Errorf("decode canonical spec: %w", err)
	}
	var spec artifact.Spec
	if err := json.Unmarshal(doc.Spec, &spec); err != nil {
		return artifact.Spec{}, nil, fmt.Errorf("decode canonical spec: %w", err)
	}
	return spec, full, nil
}

// deriveFromSpec 是**唯一**一次「正文 → 派生列」的推导（契约 §4 的「一个写者」）。
//
// 三件必须成立：
//   - 空 outputs 归一成 []（不变量 I9，也参与 digest：nil 与 [] 不能是两份字节）；
//   - params 为空时落 '{}'（既有列语义：空 schema 就是一个空对象）；
//   - lifecycle 是 RawMessage，解进结构体失败就是内容错误（400），不是 500。
//
// 调用方设过的 ParamsSchema / Outputs / Lifecycle / Sticky 会在这里被**覆盖**：
// 反向（从派生字段反推正文）被明令禁止，那会让「正文」变成列的函数，
// 于是「只改脚本不产生新版本」这个 bug 换个马甲又回来了。
func deriveFromSpec(c *CapabilityVersion) error {
	if c == nil {
		return fmt.Errorf("derive capability: nil")
	}
	if c.Spec.Outputs == nil {
		c.Spec.Outputs = []string{}
	}
	c.ParamsSchema = c.Spec.Params
	if len(c.ParamsSchema) == 0 {
		c.ParamsSchema = json.RawMessage("{}")
	}
	c.Outputs = c.Spec.Outputs
	c.Sticky = c.Spec.Sticky
	if len(c.Spec.Lifecycle) > 0 {
		var lc Lifecycle
		if err := json.Unmarshal(c.Spec.Lifecycle, &lc); err != nil {
			return fmt.Errorf("%w: spec.lifecycle 不是合法的 lifecycle 对象: %v", ErrCapabilityInvalid, err)
		}
		c.Lifecycle = lc
	} else {
		c.Lifecycle = Lifecycle{}
	}
	return nil
}

// NormalizeCapability 返回能力的 content_hash（**裸 hex**，不带 sha256: 前缀）。
//
// v3 起口径就是 digest（契约 §3.2/§4）：
//
//	content_hash = hex(sha256(canonical{metadata.name, metadata.version, spec}))
//
// 计算**只**走 artifact.Digest：自己再写一份必然漂移，而漂移的症状是「同一份能力
// 算出两个 hash」——部署永远被判成 stale，谁也看不出根因。
//
// 由此三件事自动成立，不需要额外规则：
//   - 改 displayName 不变 hash（它不在 digest 里，PATCH 因此天然不漂）；
//   - 改 spec 任一字段（含 lifecycle / sticky / handler / 文件正文）都变 hash；
//   - 换 version 也变 hash，所以「同内容不同版本」不再折叠（见 ErrVersionConflict）。
func NormalizeCapability(c *CapabilityVersion) (string, error) {
	if c == nil {
		return "", fmt.Errorf("normalize capability: nil")
	}
	a := c.Artifact()
	digest, err := artifact.Digest(&a)
	if err != nil {
		return "", fmt.Errorf("digest capability %s@%d: %w", c.Name, c.Version, err)
	}
	return strings.TrimPrefix(digest, artifact.DigestPrefix), nil
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
	// ① 先校验**正文**：缺实现 / 实现二义 / handler 不在白名单 / params 不是对象……
	//    形状规则只有一份（artifact.Validate）。失败 = 客户端可修 → 400 语义的错。
	//
	//    还没分配版本号时先用占位版本 1 过一遍：version 只进 digest、不进正文，
	//    所以归一结果与最终版本无关（定了版本后要重算一遍规范字节，因为 digest 吃版本）。
	assignVersion := c.Version <= 0
	if assignVersion {
		c.Version = 1
	}
	specJSON, err := prepareCapabilitySpec(c)
	if err != nil {
		return err
	}
	// ② 版本分配：**省略 version（<=0）= 内容寻址的创建**（契约 §10-B）。
	//
	//    同一 (name, spec) 已经在库里 → 直接复用那一行：重复 POST / 客户端重试
	//    不该长出第二份一模一样的版本（能力没有 DELETE 端点，重复行是永久的）。
	//
	//    **显式给了 version 就永不折叠**：x@1 与 x@2 各占一行，哪怕内容相同。
	//    这是必须的——边里写的是 name@version，把 x@2 折成 x@1 会让 to_ref 悬空。
	if assignVersion {
		existing, lerr := getCapabilityBySpecOn(ctx, ex, c.NamespaceID, c.Name, specJSON)
		switch {
		case lerr == nil && existing != nil:
			return foldIntoExistingOn(ctx, ex, c, existing)
		case lerr != nil && !errors.Is(lerr, ErrNotFound):
			return fmt.Errorf("lookup capability by spec: %w", lerr)
		}
		next, nerr := nextCapabilityVersionOn(ctx, ex, c.NamespaceID, c.Name)
		if nerr != nil {
			return nerr
		}
		c.Version = next
		// 版本进了 digest → 规范字节必须用定下来的版本重算一遍。
		if specJSON, err = prepareCapabilitySpec(c); err != nil {
			return err
		}
	}
	// ③ 派生列：单向、覆盖式。调用方设过的派生字段在这里一律被覆盖（契约 §4 的
	//    「一个写者」）——反过来从派生字段拼正文会让「内容」变成列的函数，
	//    于是「只改脚本不产生新版本」那个 bug 换个马甲又回来了。
	if err := deriveFromSpec(c); err != nil {
		return err
	}
	// ④ content_hash ≡ digest（吃 name + version + 整份 spec）
	hash, err := NormalizeCapability(c)
	if err != nil {
		return err
	}
	c.ContentHash = hash

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
		   (id, namespace_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at, spec)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.NamespaceID, c.Name, c.DisplayName, c.Version, schema, string(outputsJSON),
		string(lifecycleJSON), c.Sticky, c.Source, hash, c.CreatedAt,
		string(specJSON))
	if err == nil {
		// ⑤ 没有第五步：P1 期间这里还写一行 artifacts 镜像（只为保住
		//    deployments.artifact_id 的外键）。P2 删掉了那张表，产物改为
		//    **从这一行正文算出来**（Capability Spec v3 §11.2，见 currentArtifactOn）。
		//    能力行落库 = 产物就有了，不需要第二张表跟着保持一致。
		return nil
	}
	if !isUniqueConstraintError(err) {
		return fmt.Errorf("insert capability: %w", err)
	}

	// 冲突只可能是两种：同内容已存在（并发重放 → 幂等返回已有行），
	// 或者同 name@version 换内容（真错 → ErrVersionConflict）。
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
		return foldIntoExistingOn(ctx, ex, c, existing)
	}
	if gerr != nil && !errors.Is(gerr, ErrNotFound) {
		return fmt.Errorf("lookup existing capability after conflict: %w", gerr)
	}
	// 同 name@version 已存在且内容不同：**报冲突**，不改写版本号、不覆盖旧行
	// （版本行不可变，不变量 I8）。旧行为会把它「折叠」成已有那一行，
	// 于是「换了脚本却还是旧版本」在界面上看起来像成功了——这是要消灭的形态。
	if _, e2 := getCapabilityOn(ctx, ex, c.NamespaceID, c.Name, c.Version); e2 == nil {
		return fmt.Errorf("%w: %s@%d（版本行不可变；改内容请开新版本）",
			ErrVersionConflict, c.Name, c.Version)
	}
	return fmt.Errorf("insert capability: %w", err)
}

// prepareCapabilitySpec 校验正文、把它归一化成 canonical 形状并写回 c.Spec，
// 返回「spec 子文档的字节」（capability_versions.spec 列用）。
//
// 归一化走的是 canonicalizeSpec —— 与算 digest 同一次序列化。存的正文与算 digest 的
// 正文必须是同一串字节，否则 digest 与内容会悄悄对不上（部署永远收敛不了，且看不出根因）。
//
// P2 之后这里**不再**返回「整份产物」的字节：没有 artifacts 表可写，整份产物由
// artifactOf(c) 在读取时从这一行拼出来（同一套 canonical 序列化，逐字节一致）。
func prepareCapabilitySpec(c *CapabilityVersion) (specJSON []byte, err error) {
	a := c.Artifact()
	if verr := artifact.Validate(&a); verr != nil {
		return nil, fmt.Errorf("%w: %s", ErrCapabilityInvalid, verr.Error())
	}
	spec, _, cerr := canonicalizeSpec(&a)
	if cerr != nil {
		return nil, cerr
	}
	c.Spec = spec
	raw, merr := json.Marshal(c.Spec)
	if merr != nil {
		return nil, fmt.Errorf("marshal capability spec: %w", merr)
	}
	return raw, nil
}

// foldIntoExistingOn 把这次请求折叠成库里已有的那一行（幂等创建）。
//
// **display_name 要单独处理**：它不参与 hash/digest，所以「内容相同」不代表
// 「显示名相同」。如果这次请求带了 displayName 而旧行没有（或不同），就把它落库
// —— 否则用户填的名字会被静默吞掉，界面上看起来「我明明改了却没生效」。
//
// 这里只 UPDATE 装饰字段，不违反 append-only（I8）：能力定义本身
// （name/schema/outputs/lifecycle/sticky）一个字节都没动。
func foldIntoExistingOn(ctx context.Context, ex queryExec, c, existing *CapabilityVersion) error {
	if existing == nil {
		return nil
	}
	if want := strings.TrimSpace(c.DisplayName); want != "" && want != existing.DisplayName {
		if uerr := setCapabilityDisplayNameOn(ctx, ex, c.NamespaceID, existing.ID, want); uerr != nil {
			return fmt.Errorf("update display name on idempotent create: %w", uerr)
		}
		existing.DisplayName = want
	}
	*c = *existing
	return nil
}

// getCapabilityBySpecOn 按「同一份正文」找已有版本（事务内外共用一份 SQL）。
//
// spec 列存的是 canonical JSON（键序固定），所以这里可以做**字节相等**比较 ——
// 这就是契约 §10-B 的「省略 version 时按内容寻址」。
//
// 注意它只服务于**省略版本号**的创建：显式版本永不折叠（否则边里的
// name@version 会指向一行并不存在的名字，to_ref 悬空）。
func getCapabilityBySpecOn(ctx context.Context, ex queryExec, namespaceID, name string, spec []byte) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND name = ? AND spec = ?`,
		namespaceID, name, string(spec))
}

// nextCapabilityVersionOn 返回某 name 的下一个版本号（无定义时返回 1）。
//
// 与 NextCapabilityVersion 的区别只是「读走哪个连接」：写入路径必须在事务内算，
// 否则两次并发创建会拿到同一个 next，后一个直接撞 UNIQUE。
func nextCapabilityVersionOn(ctx context.Context, ex queryExec, namespaceID, name string) (int, error) {
	var maxV sql.NullInt64
	if err := ex.queryRowContext(ctx,
		`SELECT MAX(version) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		namespaceID, name).Scan(&maxV); err != nil {
		return 0, fmt.Errorf("next capability version: %w", err)
	}
	if !maxV.Valid {
		return 1, nil
	}
	return int(maxV.Int64) + 1, nil
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

// capabilityColumns 是统一的列顺序，scanCapabilityRow 依赖它。
//
// spec 排在最后：追加在末尾就不会让任何按位置拼列的读路径错位（这一份列清单
// 同时被 GetCapability / GetCapabilityByID / getCapabilityByHash / ListCapabilities 用）。
const capabilityColumns = `id, namespace_id, name, display_name, version, params_schema,
	outputs, lifecycle, sticky, source, content_hash, created_at, spec`

// scanCapability 执行查询并解析一行；未命中返回 ErrNotFound（事务外；读走语句缓存）。
func (d *DB) scanCapability(ctx context.Context, query string, args ...any) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, d, query, args...)
}

// scanCapabilityOn 是 scanCapability 的 ex 版：所有读都经 ex，
// 事务内传 txExec 才能看到本事务刚写入的行（见 txexec.go）。
func scanCapabilityOn(ctx context.Context, ex queryExec, query string, args ...any) (*CapabilityVersion, error) {
	var c CapabilityVersion
	// 语句缓存（#242）：scanCapabilityOn 被 GetCapability / GetCapabilityByID /
	// getCapabilityByHash 共用，三者的 SQL 文本都是常量（拼的只是列名/条件名）。
	// 事务内（ex=txExec）**不能**用缓存：那些语句绑的是池里另一条连接，看不到本事务的写。
	err := scanCapabilityRow(ex.queryRowContext(ctx, query, args...), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// scanCapabilityRow 把一行 capability_versions 解进 c（列顺序 = capabilityColumns）。
//
// 抽成一份是因为它之前被抄了两遍（单行读 + ListCapabilities），而加了 spec 列之后
// 任何一份漏解都会变成「列表里没有正文、详情里有」这种只在某个页面复现的怪事。
//
// spec 是**读出来就用**的：不在这里从派生列重算正文（写的时候已经由 spec 算出，
// 读时再算一遍只会掩盖不同步——那正是契约 §4 要消灭的两种真相）。
func scanCapabilityRow(row scanner, c *CapabilityVersion) error {
	var paramsSchema, outputs, lifecycle, spec string
	if err := row.Scan(
		&c.ID, &c.NamespaceID, &c.Name, &c.DisplayName, &c.Version, &paramsSchema, &outputs,
		&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt, &spec,
	); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(paramsSchema), &c.ParamsSchema); err != nil {
		return fmt.Errorf("decode capability params_schema: %w", err)
	}
	if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
		return fmt.Errorf("decode capability outputs: %w", err)
	}
	if c.Outputs == nil {
		c.Outputs = []string{} // 不变量 I9：空列表是 [] 不是 null
	}
	if err := json.Unmarshal([]byte(lifecycle), &c.Lifecycle); err != nil {
		return fmt.Errorf("decode capability lifecycle: %w", err)
	}
	// 空 spec 只可能出现在「migration 8 的回填没跑到」的库上（回填是幂等的，
	// 跑过就填满）。这里按零值读出来而不是报错：报错会让整个能力列表打不开，
	// 而真正该报的那个地方是写入/部署路径（那里 Validate 会拒）。
	if strings.TrimSpace(spec) != "" {
		if err := json.Unmarshal([]byte(spec), &c.Spec); err != nil {
			return fmt.Errorf("decode capability spec: %w", err)
		}
	}
	return nil
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
		if err := scanCapabilityRow(rows, &c); err != nil {
			return nil, fmt.Errorf("scan capability row: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NextCapabilityVersion 返回某 name 的下一个版本号（无定义时返回 1）。
//
// 写入路径**不要**用它来定版本：带版本的创建分两步，中间有窗口（另一个写者
// 可能已经占了这个号）。store 内部的写入路径走 nextCapabilityVersionOn（事务内）。
// 它保留给「只想看看下一个号是多少」的调用方与测试。
func (d *DB) NextCapabilityVersion(ctx context.Context, namespaceID, name string) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	return nextCapabilityVersionOn(ctx, d, namespaceID, name)
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
