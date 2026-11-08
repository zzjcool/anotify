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

	"github.com/zzjcool/anotify/internal/schema"
)

// ImplicitPort 是**单出口能力的隐式端口名**（CONTRACT-hide-ports.md §1）。
//
// 端口在协议里仍然存在（它是 emit 的路由键、是 edges.from_port 的取值），但用户
// 不再看见它：创建能力时只填「下游能力（候选）」，系统把唯一出口固定叫 out。
// 于是凡是"给人看"的地方都要把它当作**不存在**——内部分支判断仍然按端口走。
const ImplicitPort = "out"

// Edge 是 edges 表的一行：出口表是路由的唯一依据（契约 §2.4）。
//
// isOriginCapability 判断某个能力定义是不是内置起点。
//
// 起点没有节点声明（kind=origin），出口是「任意事件类型」。
// 注意这里按 name+version 查内置表，而不是看 outputs——
// 用户自定义的、恰好没有 outputs 的能力**不应该**获得通配出口。
func isOriginCapability(c *CapabilityVersion) bool {
	if c == nil {
		return false
	}
	bc, ok := IsBuiltinCapability(c.Name, c.Version)
	return ok && bc.Origin
}

// 一行 = 一条边；同一 (FromRef, FromPort) 可有多行 → 扇出（不变量 I7：
// 两端都是能力引用，图里没有机器）。
type Edge struct {
	// NamespaceID 是隔离维度（I11）：边属于某个命名空间，
	// 同一对能力可以在不同命名空间各有一条边，互不影响。
	NamespaceID string `json:"namespaceId"`

	// FromIsSource 决定 FromRef 字段怎么解释：
	//
	//   false（默认）  FromRef = 上游能力的**引用**（`analyze` 跟随 latest /
	//                  `analyze@1` 钉死，见 ref.go 与设计 §3.2）
	//                  fromPort 必须 ∈ 解析到的版本的 outputs（不变量 I2）
	//
	//   true            FromRef = **来源名**（"github"/"jira"/…）
	//                  fromPort = 事件类型，任意值（外部系统决定）
	//
	// 第二种就是「入口边」：机器入口 POST /v1/events?source=github 靠它找下游。
	// 它原先靠一个叫 github@1 的空壳能力当锚点，现在不需要了 —— 那种能力
	// 不执行代码、不产生任务、也不可配置，却会出现在能力列表上。
	FromIsSource bool   `json:"fromIsSource"`
	FromRef      string `json:"fromRef"`
	FromPort     string `json:"fromPort"`
	// ToRef 是下游能力的**引用**（`notify` / `notify@2`）。入口边同样只用引用：
	// 源的「出口能力」也走升级语义（钉的是它当初选的那一版，见 checkSourceTargets）。
	ToRef string `json:"toRef"`

	// Literals 是这条边的**常量覆盖**（设计 §3.3）：进入下游的 params = data ⊎ literals，
	// 字面量优先，所以 data 里不必再带这些键。"{}" = 没有覆盖。
	//
	// 用 json.RawMessage 而不是 map[string]any：写进库里的就是用户给的字节，
	// 读出来要**字节级一致**（不重排键、不给数字补 .0）——那是事后对账的唯一依据。
	// 用 json.RawMessage 而不是 string：出 JSON 时直接就是对象，不用前端再解一层。
	Literals json.RawMessage `json:"literals"`

	// Mapping 是这条边的**引用模板**（P2.1，设计 §11.5 选项 A）：入口边专用，
	// 进入下游的 params = ResolveParams(mapping, 事件 payload) ⊎ literals。
	//
	// 只有入口边（FromIsSource）允许非空：外部世界不会替你拼参数，而能力边的参数
	// 由实现自己 emit 的 data 提供（§8 已否决能力边的引用映射）。违反由
	// checkEdgeMapping 在保存时硬拒（ErrEdgeRejected）。
	//
	// 与 literals 同样用 json.RawMessage：写的就是用户给的字节，读回来要一字不差。
	Mapping   json.RawMessage `json:"mapping"`
	CreatedAt int64           `json:"createdAt"`

	// Priority 是这条边的**候选优先级**（契约 §3：小的优先）。
	//
	// 它不改变「这条边通不通」，只决定「同一个 (能力, 端口) 有一堆下游时先选谁」：
	// 派发取最小 priority 的那一组，组内轮盘。0 是默认值——加这一列之前建的边
	// 全部同级，行为与「一个端口只有一个下游」时完全一样。
	//
	// 为什么不进 edges 的主键：优先级是**同一条线的属性**，不是新的一条线。
	// 两行同 (from_ref, from_port, to_ref) 不同 priority 的边没有意义（谁是它在跑？），
	// 改优先级应当是原地改这条边（见 UpdateEdgePriority），而不是并排放两行。
	Priority int `json:"priority"`

	// Deprecated: P2.2 之前的字段名，**只用于输入兼容**，永不落库、永不出现在 JSON 里。
	//
	// 老客户端（控制台旧版本、e2e、节点侧的老调用）传的是 capability_versions.id；
	// CreateEdge 在 FromRef/ToRef 为空时读这两个字段，并把 id 解析成
	// `name@version`（钉死引用）——这是「输入兼容一个版本」，**不是**第二种存储形状：
	// 边上一律只存引用（P2.2 的全部意义就在这里）。
	//
	// 下一次清理（P2.2 的前端/mock/e2e 波）会连同这层兼容一起删掉。
	FromCapability string `json:"-"`
	ToCapability   string `json:"-"`
}

// LinkTarget 是「这条边通向哪里」+「路上带哪些常量/模板」，即 EdgesFrom 的一行。
//
// 为什么不是 []string：派发第 ① 步除了目标还得拿到这条边的字面量覆盖与引用模板
// （设计 §3.3 / §11.5），分两次查（先拿 id，再回头查 literals/mapping）会多一次往返，
// 也让「边」这个概念在两个包里各有一半。
//
// ToRef 是目标能力的**引用**（`notify` / `notify@2`），不是 id：
// 「指向哪一版」由引用本身回答，派发时再解析（设计 §3.2 / §7 P2.2）。
type LinkTarget struct {
	ToRef string `json:"toRef"`
	// Priority 原样来自 edges.priority（契约 §3 的候选优先级，小的优先）。
	//
	// 它必须跟着 EdgesFrom 一起出来：候选排序要用它，而「先拿目标、再回头查
	// priority」等于同一行查两遍，且两次之间那条边可能被改了。
	Priority int `json:"priority"`
	// Literals 原样来自 edges.literals（"{}" = 无覆盖）。派发前由 route 叠加到目标参数上。
	Literals json.RawMessage `json:"literals"`
	// Mapping 原样来自 edges.mapping（"{}" = 无模板）。只有入口边会非空；
	// 派发时 route 用事件 payload 求值成实际参数（能力边恒为 "{}"）。
	Mapping json.RawMessage `json:"mapping"`
}

// ErrEdgeRejected 表示边被配置期校验拒绝（出口未定义 / 跨命名空间）。
var ErrEdgeRejected = errors.New("edge_rejected")

// GraphVersion 是 graph_versions 表的一行：按需落版的图快照（契约 §10.2）。
type GraphVersion struct {
	ID string `json:"id"`
	// NamespaceID 是隔离维度；注意 content_hash **不含** namespace_id
	// （快照内容就是 edges 列表，命名空间已经由列表达）。
	NamespaceID string          `json:"namespaceId"`
	ContentHash string          `json:"contentHash"`
	Snapshot    json.RawMessage `json:"snapshot"`
	CreatedAt   int64           `json:"createdAt"`
}

// CreateEdge 新增一条边。
//
// 两端都用**引用**（P2.2，设计 §3.2）：`name` 跟随 latest / `name@version` 钉死。
// 存 id 已经是历史：id 每版一个，升级之后旧 id 永远指着旧版，升级永远不生效。
//
// 入参归一（唯一一处）：FromRef/ToRef 为空时读废弃字段 FromCapability/ToCapability，
// 并把能力 id 解析成 `name@version`（输入兼容，**不存 id**）。
//
// 配置期校验（契约 §6.2 / 不变量 I2 / A5 / I11 / 设计 §5「定义期（存 link）」）：
//   - to_ref 必须能解析（`name` → 这个名字存在；`name@v` → 那一版存在）
//   - from_port 必须 ∈ **解析到的上游版本**的 outputs（封闭集合，不能凭空发明出口）；
//     入口边例外：事件类型由外部系统决定，也可以写成 "*" = 任意事件类型
//   - 两端引用必须能在本**命名空间**里解析（I11 第 ① 条：跨命名空间引用一律拒绝）
//   - Literals 必须是 JSON 对象，每个键 ∈ 目标 params_schema.properties，
//     且值类型与声明**精确相等**（不转换）——拼错/写错当场拒，不要等派发时才剪枝
//   - Mapping 只允许出现在**入口边**，且必须是 JSON 对象、键 ∈ 目标
//     params_schema.properties、值是「常量或 {{点号路径}} 模板」（常量才查类型）。
//     能力边带非空 mapping → 硬拒（§8 已否决能力边的引用映射）
//
// 这里**不查环**（契约 §2）：`review → fix → review` 是合法编排，环由运行期 hop 预算
// （max_hops）兜底；静态环检测会把「大图里一个合法的共同汇点」也拒掉。
//
// 违反返回 ErrEdgeRejected / *CrossNamespaceError。重复边幂等（PK 冲突视为已存在）。
func (d *DB) CreateEdge(ctx context.Context, e *Edge) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create edge: begin tx: %w", err)
	}
	// 早退 / panic 都回滚：边表的校验全部基于读（目标存在 / 端口封闭 / 环），
	// 这些读与插入必须在同一快照里，否则「查完到写完」之间别人改一行就可能存下一条不合法边。
	defer func() { _ = tx.Rollback() }()
	if err := createEdgeTx(ctx, txExec{tx: tx}, e); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create edge: commit: %w", err)
	}
	return nil
}

// createEdgeTx 是建边的**唯一**实现（CreateEdge 只是给它开事务）。
//
// 所有读都经 ex：事务内传 txExec，才能看到同一次提交里刚写入的边、
// 刚建的能力（否则「能力 + 它的下游」一次提交会报「目标不存在」，
// 两条互指的边也检不出环）。
func createEdgeTx(ctx context.Context, ex queryExec, e *Edge) error {
	if e == nil {
		return fmt.Errorf("create edge: nil")
	}
	if e.NamespaceID == "" || e.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	// 归一：废弃字段名 → ref（输入兼容一个版本，见 Edge.FromCapability 的注释）。
	if strings.TrimSpace(e.FromRef) == "" {
		e.FromRef = e.FromCapability
	}
	if strings.TrimSpace(e.ToRef) == "" {
		e.ToRef = e.ToCapability
	}
	if strings.TrimSpace(e.FromRef) == "" || e.FromPort == "" || strings.TrimSpace(e.ToRef) == "" {
		return fmt.Errorf("%w: from/port/to all required", ErrEdgeRejected)
	}
	// 优先级必须与 PUT …/downstreams 同一条口径。
	//
	// 为什么必须在这里也拦：`createEdgeTx` 是 POST /capabilities 的 `links[]` 与
	// /batch 的 `links[]` 的写入口，而那个入口**没有** priority 的校验；只有
	// `ReplaceDownstreams` 查 [0,1000]。同一个值走 A 路径 400、走 B 路径静默入库——
	// 而 priority 直接决定候选顺序（小的先走），越界值会让“为什么这条线永远不跑”
	// 变成一件靠猜的事。
	if e.Priority < DownstreamPriorityMin || e.Priority > DownstreamPriorityMax {
		return fmt.Errorf("%w: priority_out_of_range: priority %d out of range [%d, %d]",
			ErrEdgeRejected, e.Priority, DownstreamPriorityMin, DownstreamPriorityMax)
	}

	// 右端（I11 第 ① 条）：引用必须在本命名空间里解析得到。
	// 解析不到 / 跨命名空间 → 都是客户端可修的错误（400）。
	toRef, to, err := resolveLinkRefOn(ctx, ex, e.NamespaceID, e.ToRef, "edges.to_ref")
	if err != nil {
		return asEdgeRejected(err, "to link ref %q", e.ToRef)
	}
	e.ToRef = toRef

	// 字面量覆盖的配置期硬拒（设计 §5）：常量不依赖运行时，所以能在这里判类型。
	// 归一化后的值回写 e.Literals（空 → "{}"），保证写入/响应都不会是 null。
	// 用**解析到的版本**的 schema 判：`name` 型 link 跟随 latest，改的就是最新那份契约。
	literals, err := checkEdgeLiterals(to, e.Literals)
	if err != nil {
		return err
	}
	e.Literals = literals

	// 引用模板的配置期校验（P2.1，设计 §11.5）：只有入口边允许，且键/形状/常量类型都要拦。
	// 与 literals 同一处校验、同一个哨兵错误（ErrEdgeRejected → HTTP 400，客户端可修）。
	mapping, err := checkEdgeMapping(to, e.Mapping, e.FromIsSource)
	if err != nil {
		return err
	}
	e.Mapping = mapping

	if e.FromIsSource {
		// 入口边：左端是来源名，出口是事件类型。
		//
		// **不校验出口名**（契约 §5.2）：事件类型由外部系统决定
		// （GitHub 发什么 type 就是什么），不可能预先枚举。
		// 这没有放松 I2 的本意 —— I2 防的是「凭空发明下游口子」，
		// 而来源没有 outputs 这个概念，无从封闭。
		//
		// from_port 可以是 "*"（= 任意事件类型）：源的目标（P2.1）就是它，
		// 新建入口边时用户也可以直接写它来省掉逐个 type 建边。
		//
		// 也要校验来源名不比能力名更容易撞车：来源名非空即可。
		// 来源名是**名字**（不是能力引用）：原样保存，不解析、不带版本后缀。
		e.FromRef = strings.TrimSpace(e.FromRef)
	} else {
		// 左端：能力引用，同样必须能解析（I11 第 ① 条）。
		fromRef, from, ferr := resolveLinkRefOn(ctx, ex, e.NamespaceID, e.FromRef, "edges.from_ref")
		if ferr != nil {
			return asEdgeRejected(ferr, "from link ref %q", e.FromRef)
		}
		e.FromRef = fromRef
		// 出口封闭集合（不变量 I2）：from_port 必须属于**解析到的那个版本**的 outputs。
		//
		// 为什么按解析到的版本判而不是按名字的并集：`analyze@1` 钉的是那一版的契约；
		// `analyze` 跟随 latest，那就按此刻的 latest 判——用户配的是「此刻这条线」，
		// 以后 latest 换了端口集合，由升级闸门（ErrUpgradeBreaking）拦下来。
		if !from.HasOutput(e.FromPort) {
			return fmt.Errorf("%w: port %q not in %s@%d outputs %v",
				ErrEdgeRejected, e.FromPort, from.Name, from.Version, from.Outputs)
		}
		// 这里**没有**环检测（契约 §2）：环是合法编排（`review → fix → review`）。
		// 兜底交给运行期 hop 预算（route/dispatch.go 的 max_hops）：它按**实际走过的**
		// 深度剪枝，而不是按「静态上能不能走回自己」——后者会把一条从未真的走回来的
		// 配置（大图里一个共同的汇点）也拒掉。
	}

	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	// 幂等键是 (namespace, 左端, 端口, 右端)**引用**：
	// `fixer` 与 `fixer@1` 是两条不同的 link（一个跟随、一个钉死），可以并存。
	//
	// priority 走 ON CONFLICT DO NOTHING 的**保留原值**语义：重复提交同一条边不该把
	// 它的优先级悄悄重置成传入值（那是一次不可见的改线）。要改优先级用 UpdateEdgePriority。
	if _, err := ex.execContext(ctx,
		`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(namespace_id, from_is_source, from_ref, from_port, to_ref) DO NOTHING`,
		e.NamespaceID, boolToInt(e.FromIsSource), e.FromRef, e.FromPort, e.ToRef,
		string(e.Literals), string(e.Mapping), e.Priority, e.CreatedAt); err != nil {
		return fmt.Errorf("insert edge: %w", err)
	}
	return nil
}

// UpdateEdgePriority 改一条边**现有**的候选优先级（契约 §3：小的优先）。
//
// 为什么是原地改一列而不是「删了重建」：priority 是同一条线的属性，不是新的一条线。
// 删了重建会换掉 created_at，也会在「重建失败」时把这条线弄丢（配置期没有事务兜底）。
//
// 定位口径与 DeleteEdge 一致（引用 / 老客户端的能力 id 都能匹配）。
func (d *DB) UpdateEdgePriority(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort, toKey string, priority int) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	fromKeys := []string{strings.TrimSpace(fromKey)}
	if !fromIsSource {
		fromKeys = d.refLookupKeys(ctx, namespaceID, fromKey)
	}
	toKeys := d.refLookupKeys(ctx, namespaceID, toKey)
	if len(fromKeys) == 0 || len(toKeys) == 0 {
		return ErrNotFound
	}

	args := make([]any, 0, 4+len(fromKeys)+len(toKeys))
	args = append(args, priority, namespaceID, boolToInt(fromIsSource), fromPort)
	for _, k := range fromKeys {
		args = append(args, k)
	}
	for _, k := range toKeys {
		args = append(args, k)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE edges SET priority = ?
		 WHERE namespace_id = ? AND from_is_source = ? AND from_port = ?
		   AND from_ref IN (`+sqlPlaceholders(len(fromKeys))+`)
		   AND to_ref IN (`+sqlPlaceholders(len(toKeys))+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("update edge priority: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update edge priority rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// asEdgeRejected 把「引用解析失败」翻成配置期硬拒（ErrEdgeRejected，HTTP 400）。
//
// 跨命名空间引用保持 *CrossNamespaceError（I11 有独立的错误类型，handler 按它翻 400），
// 其余一律包成 ErrEdgeRejected 并把具体原因带在文案里（哪个引用、要什么）。
func asEdgeRejected(err error, format string, args ...any) error {
	var cross *CrossNamespaceError
	if errors.As(err, &cross) {
		return cross
	}
	return fmt.Errorf("%w: %s: %v", ErrEdgeRejected, fmt.Sprintf(format, args...), err)
}

// boolToInt 把 bool 存成 SQLite 的 0/1（没有原生 bool 类型）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 边的字面量覆盖（P0-c，设计 §3.3） ----------

// EdgeLiteralsEmpty 是「这条边没有常量覆盖」的唯一表示。
//
// 用 '{}' 而不是空串 / NULL：一个值就能表达「没有覆盖」，读出/下发的 JSON 形状也统一，
// 前端不用区分 null 与空对象。
const EdgeLiteralsEmpty = `{}`

// normalizeEdgeLiterals 把从库里读出的字节归一成可直接下发/回显的形式：
// 空值（老库 / 空串）当 '{}'，其余**原样保留**（字节级往返一致，不重排键、不补 0）。
func normalizeEdgeLiterals(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(EdgeLiteralsEmpty)
	}
	return json.RawMessage(b)
}

// checkEdgeLiterals 是 link 层的配置期硬拒（设计 §5「定义期（存 link）」/ §7 P0.3）。
//
// 校验三条：
//
//	a. 必须是 **JSON 对象**（数组/标量/坏 JSON 一律拒；null 视为没有覆盖，见下）——参数只能是一个字典；
//	b. 每个键必须出现在**目标能力 params_schema 的 properties** 里（拼错当场拒）；
//	c. 值类型必须与 params_schema 声明的类型**精确相等**（integer 不收 string，
//	   string 不收 integer，不做任何转换）。
//
// 为什么 b 是真的硬拒（而不是“不认识的键先放着”）：字面量的全部价值是「这个常量
// 真的送到下游」，一个拼错的键在派发时会被静默忽略——正是本设计要消灭的失败模式。
// 代价是：目标声明为空 schema 的能力必须先声明字段才能用字面量（那是它自己的契约缺口）。
//
// 返回归一化后的字面量（未给 → '{}'）。违反返回 ErrEdgeRejected（客户端可修）。
func checkEdgeLiterals(to *CapabilityVersion, literals json.RawMessage) (json.RawMessage, error) {
	if len(literals) == 0 {
		return json.RawMessage(EdgeLiteralsEmpty), nil
	}

	// 先解成 any 再断言对象：直接 Unmarshal 到 map 会把 null 当成空 map（无法区分）。
	var raw any
	if err := json.Unmarshal(literals, &raw); err != nil {
		return nil, fmt.Errorf("%w: literals must be a JSON object: %v", ErrEdgeRejected, err)
	}
	if raw == nil {
		// JSON null = 「没有值」= 与字段缺省同义（可选项的通用约定，本仓 resultSchema 同口径）。
		// 不当错误：把 null 拒掉等于把「可选」变成「必须显式给 {}」，
		// 而 `{"literals": null}` 正是 JS / Go 结构体客户端最容易产生的形状。
		return json.RawMessage(EdgeLiteralsEmpty), nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: literals must be a JSON object, got %s", ErrEdgeRejected, JSONTypeName(raw))
	}
	if len(obj) == 0 {
		return literals, nil
	}

	props, err := paramsSchemaProperties(to)
	if err != nil {
		return nil, err
	}
	// 按名字排序后逐个判：同一个坏输入永远报同一个错（与 route.validateObject 同口径）
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec, declared := props[k]
		if !declared {
			return nil, fmt.Errorf("%w: literals.%s: %s@%d params_schema has no such field (declared: %v)",
				ErrEdgeRejected, k, to.Name, to.Version, sortedFields(props))
		}
		if msg := matchDeclaredType(spec, obj[k], "literals."+k); msg != "" {
			return nil, fmt.Errorf("%w: %s", ErrEdgeRejected, msg)
		}
	}
	// 原样返回用户给的字节：写进库的就是它，读出来也一字不差。
	return literals, nil
}

// paramsSchemaProperties 取目标能力 params_schema 的 properties（字段字典）。
//
// 空 schema / 非法 schema 当作「没有声明任何字段」而不是错误：
// 能力保存时已经把 schema 格式过一道（store.CreateCapability），走到这里还是坏的
// 就是数据被写坏了；此时任何字面量 key 都无法被声明，于是会被 b 拒掉（fail closed）。
func paramsSchemaProperties(to *CapabilityVersion) (map[string]any, error) {
	if to == nil || len(to.ParamsSchema) == 0 {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(to.ParamsSchema, &schema); err != nil {
		return nil, fmt.Errorf("%w: target %s@%d params_schema is not a JSON object: %v",
			ErrEdgeRejected, to.Name, to.Version, err)
	}
	props, _ := schema["properties"].(map[string]any)
	return props, nil
}

// sortedFields 把 field 字典的键排序后返回（给错误消息用：稳定、可读）。
func sortedFields(props map[string]any) []string {
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// matchDeclaredType 校验一个字面量的类型是否与 schema 声明**精确相等**，返回不合法原因。
//
// 返回 "" 的两种情形都是正常的：
//   - 声明里的 type 不存在（字段只声明了 minLength 之类的约束，没声明类型）→ 无类型可对；
//   - type 的写法非法（不是 string / []any）→ 忽略（与 route.validateValue 同口径：
//     不认识的 schema 关键字一律不拦）。
func matchDeclaredType(spec any, v any, path string) string {
	prop, ok := spec.(map[string]any)
	if !ok {
		return ""
	}
	want, ok := prop["type"]
	if !ok {
		return ""
	}
	switch t := want.(type) {
	case string:
		if MatchJSONType(t, v) {
			return ""
		}
		return fmt.Sprintf("%s: want type %s, got %s", path, t, JSONTypeName(v))
	case []any:
		wanted := make([]string, 0, len(t))
		for _, e := range t {
			name, ok := e.(string)
			if !ok {
				continue
			}
			wanted = append(wanted, name)
			if MatchJSONType(name, v) {
				return ""
			}
		}
		if len(wanted) == 0 {
			return ""
		}
		return fmt.Sprintf("%s: want type %s, got %s", path, strings.Join(wanted, "|"), JSONTypeName(v))
	}
	return ""
}

// MatchJSONType 判定 v 是否属于 JSON Schema 的 type `want`（受控子集，设计 §5）。
//
// **全仓唯一**的类型判定口径：route.validateValue（派发期）与 store.checkEdgeLiterals
// （配置期）必须用同一份实现，否则会出现「存的时候说类型对、派发时被剪枝」这种
// 自相矛盾的判定。route 包 import 了 store，反向不成立，所以这份最小原语放在
// 最底层，由 route 的 typeMatches 直接转发（见 internal/route/validate.go）。
// MatchJSONType 判定值是否属于某个 JSON Schema type。
//
// 实现在 internal/schema（零依赖、全仓唯一）：配置期（这里的边字面量）与派发期、
// 入口、投递期的判定必须完全一致，否则会出现「存的时候说类型对、派发时被剪枝」。
func MatchJSONType(want string, v any) bool { return schema.TypeMatches(want, v) }

// JSONTypeName 给人看的类型名（错误消息用）；与 MatchJSONType 同一来源。
func JSONTypeName(v any) string { return schema.TypeName(v) }

// DeleteEdge 删除一条边。边不存在返回 ErrNotFound。
//
// 定位参数是**引用**（`name` / `name@version`）；但 P2.2 之前的调用方传的是能力 id，
// 所以这里先归一成候选值（见 refLookupKeys）：
//
//   - `fixer@1`          → 只删钉死那一版的那条 link（两个引用是两件事，不能互相匹配）
//   - `fixer`            → 只删跟随 latest 的那条 link
//   - 能力 id（老客户端） → 钉死引用与同名跟随引用**都算命中**（用户的意思就是「这条线」）
func (d *DB) DeleteEdge(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort, toKey string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	// 入口边的左端是**来源名**，原样匹配（不解析：来源不是能力）。
	fromKeys := []string{strings.TrimSpace(fromKey)}
	if !fromIsSource {
		fromKeys = d.refLookupKeys(ctx, namespaceID, fromKey)
	}
	toKeys := d.refLookupKeys(ctx, namespaceID, toKey)
	if len(fromKeys) == 0 || len(toKeys) == 0 {
		return ErrNotFound
	}

	args := make([]any, 0, 3+len(fromKeys)+len(toKeys))
	args = append(args, namespaceID, boolToInt(fromIsSource), fromPort)
	for _, k := range fromKeys {
		args = append(args, k)
	}
	for _, k := range toKeys {
		args = append(args, k)
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM edges
		 WHERE namespace_id = ? AND from_is_source = ? AND from_port = ?
		   AND from_ref IN (`+sqlPlaceholders(len(fromKeys))+`)
		   AND to_ref IN (`+sqlPlaceholders(len(toKeys))+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("delete edge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete edge rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// sqlPlaceholders 返回 n 个逗号分隔的 ?，供 IN (...) 拼参数位（n 已由调用方保证 >= 1）。
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ListEdges 列出**命名空间内**的边（fromKey 非空时按它过滤）；空列表返回 []。
//
// fromKey 是「左端是谁」：来源名（入口边）或能力引用/能力 id。
// 过滤按**名字**匹配（RefName）：`fixer` 与 `fixer@1` 都是「fixer 的出边」，
// 这是控制台“某个能力的出边”那个视图的意思——按字符串精确匹配会把钉死引用漏掉。
func (d *DB) ListEdges(ctx context.Context, namespaceID, fromKey string) ([]*Edge, error) {
	return listEdgesOn(ctx, d, namespaceID, fromKey)
}

// listEdgesOn 是 ListEdges 的 ex 版（事务内的读必须看到本事务刚写的行）。
func listEdgesOn(ctx context.Context, ex queryExec, namespaceID, fromKey string) ([]*Edge, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	// 过滤在 Go 里做：边是配置期小对象（十几行），而“按名字匹配两个引用形态”
	// 用 SQL 得拼 LIKE/子串，既慢又容易把名字里带 @ 的边弄错。
	//
	// 过滤名字**先**算好：refNameKey 可能再查一次库，而结果集正占着连接
	// （`:memory:` 库连接池上限是 1，边读边查会自己锁死自己）。
	wantName := ""
	if fromKey != "" {
		wantName = refNameKeyOn(ctx, ex, namespaceID, fromKey)
	}
	q := `SELECT namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at
	        FROM edges WHERE namespace_id = ?
	        ORDER BY from_is_source, from_ref, from_port, to_ref`
	rows, err := ex.queryContext(ctx, q, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list edges: %w", err)
	}
	defer rows.Close()
	out := []*Edge{}
	for rows.Next() {
		var e Edge
		var isSrc int
		var literals, mapping string
		if err := rows.Scan(&e.NamespaceID, &isSrc, &e.FromRef, &e.FromPort, &e.ToRef,
			&literals, &mapping, &e.Priority, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		e.FromIsSource = isSrc == 1
		e.Literals = normalizeEdgeLiterals([]byte(literals))
		e.Mapping = normalizeEdgeMapping([]byte(mapping))
		if wantName != "" && RefName(e.FromRef) != wantName {
			continue
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EdgeShape 是一个能力在 edges 里的**接线事实**（推导量，不落库）。
//
// 为什么需要它（#383）：`outputs` 是**产物**里声明的端口集合，过去同时被当成拓扑的
// 真相 —— 于是「建时声明 outputs: []（终端）之后又加了下游」的能力，派发时节点拿到
// 的端口契约里没有那个出口，emit 当场报「终端能力」，新边永远不触发。裁定是
// **以边为准**：assign.ports = 声明的 outputs ∪ OutPorts，kind 也改看 HasOut/HasIn
// （route.DeriveKind）。两条推导的输入都是这个结构，读法因此只有一份。
type EdgeShape struct {
	// OutPorts 是这个能力作为左端出现过的 from_port（去重、升序）。
	//
	// 左端 `name` 与钉死的 `name@version` 都算这个能力的出边（与 EdgesFrom 同口径）：
	// 派发只知道「上游是哪个能力（名字）」，不该去猜用户配的是哪一版。
	OutPorts []string
	// HasIn 报告是否有边指向它。**能力边**（别人 --x--> 它）与**入口边**
	// （来源 --事件--> 它）都算：两者都是「有上游喂它」这件事。
	// 右端 `name` 与 `name@version` 同样都算。
	HasIn bool
}

// HasOut 报告是否有出边（= 至少一个出口在边上被用过）。
//
// nil 安全：调用方在「读不到形状」的降级路径上会传 nil（当作没有边）。
func (s *EdgeShape) HasOut() bool { return s != nil && len(s.OutPorts) > 0 }

// CapabilityEdgeShape 读一个能力按**名字**的接线事实（出边端口 + 有没有入边）。
//
// 按名字而不是 id：edges 两端存的是引用（P2.2），同一条边在升级前后必须能被同一跳
// 查到；`name@version` 型的边是同一个能力的出边（与 EdgesFrom 同一套 substr 匹配）。
//
// 能力不存在不报错：这个方法只回答「边怎么接的」，与能力行是否存在无关。
func (d *DB) CapabilityEdgeShape(ctx context.Context, namespaceID, name string) (*EdgeShape, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty capability name", ErrRefUnresolved)
	}
	out := &EdgeShape{OutPorts: []string{}}

	// 出边端口：能力边（from_is_source=0）的左端按名字匹配，两种引用形态都算。
	// 用 substr 而不是 LIKE：名字里可能有 `%`/`_`，那种情况下 LIKE 会误匹配。
	//
	// 这里把 rows 读完并**显式关闭**后再发第二个查询：`:memory:` 库的连接池上限是 1，
	// 行没关就再查会自己把自己锁死（见 ListEdges 的同类注释）。
	rows, err := d.QueryContext(ctx,
		`SELECT DISTINCT from_port FROM edges
		 WHERE namespace_id = ? AND from_is_source = 0
		   AND (from_ref = ? OR substr(from_ref, 1, length(?) + 1) = ? || '@')
		 ORDER BY from_port`, namespaceID, name, name, name)
	if err != nil {
		return nil, fmt.Errorf("read capability out ports: %w", err)
	}
	readPorts := func() error {
		defer rows.Close()
		for rows.Next() {
			var port string
			if err := rows.Scan(&port); err != nil {
				return fmt.Errorf("scan capability out port: %w", err)
			}
			out.OutPorts = append(out.OutPorts, port)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read capability out ports: %w", err)
		}
		return nil
	}
	if err := readPorts(); err != nil {
		return nil, err
	}

	// 入边：**不过滤** from_is_source —— 入口边（来源 --事件--> 能力）也是入边。
	var one int
	err = d.QueryRowContext(ctx,
		`SELECT 1 FROM edges
		 WHERE namespace_id = ?
		   AND (to_ref = ? OR substr(to_ref, 1, length(?) + 1) = ? || '@')
		 LIMIT 1`, namespaceID, name, name, name).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.HasIn = false
	case err != nil:
		return nil, fmt.Errorf("read capability in edges: %w", err)
	default:
		out.HasIn = true
	}
	return out, nil
}

// refNameKey 返回「按名字匹配这一端」用的名字：
//
//	`fixer` / `fixer@1` → fixer
//	能力 id（老客户端） → 该能力的名字
//	其余（来源名等）    → 原字符串
func (d *DB) refNameKey(ctx context.Context, namespaceID, raw string) string {
	return refNameKeyOn(ctx, d, namespaceID, raw)
}

// refNameKeyOn 是 refNameKey 的 ex 版（事务内的读必须经 ex）。
func refNameKeyOn(ctx context.Context, ex queryExec, namespaceID, raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if name, _, pinned := SplitRef(s); pinned {
		return name
	}
	if c, err := getCapabilityByIDOn(ctx, ex, namespaceID, s); err == nil {
		return c.Name
	}
	return s
}

// EdgesFrom 是派发第 ① 步的**唯一**查表入口（契约 §4）：
// 返回 (namespaceID, fromKey, fromPort) 的全部下游
// （目标**引用** + 该条边的字面量覆盖 + 该条边的引用模板）。
//
// fromIsSource 决定 fromKey 是能力名还是来源名（见 Edge.FromIsSource）。
//
// fromKey 对能力边是**能力名**（不是 id）：`name` 型 link 的左端就是名字本身，
// 按名字查才能一次拿全（`fixer` 与 `fixer@1` 两种左端各自是独立的 link 行，
// 但都写的是同一个能力名——名字相同才能被同一跳查到）。
//
// **入口边的端口匹配放宽**（P2.1）：入口边可以写 from_port="*"，含义是「这个源的任意
// 事件类型都走这条边」——源能收什么事件由外部系统决定，让用户为每个 type 手建一条边
// 是多余的心智负担（源的目标就是一个 "*"）。所以 fromIsSource=true 时
// 匹配 `from_port = ? OR from_port = '*'`；能力边的出口是能力的封闭契约（I2），不放宽。
//
// 精确端口优先于 "*"，且同一**目标引用**只返回一行：一条精确边 + 一条 "*" 边指向同一个
// 下游时，那个下游只能收到**一次**派发（否则一个事件会变成两个任务）。
//
// 除了 route 包，其他包不得再写 `FROM edges`（见 #56 的验收：全仓只允许 1 处）。
func (d *DB) EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort string) ([]LinkTarget, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if !fromIsSource {
		// 左端有两种引用形态，**都算这一跳的出边**：
		//
		//	analyze     跟随 latest（新建 link 的默认形态）
		//	analyze@1   钉死某一版（存量 link、以及显式的渐进发布）
		//
		// 派发只知道「上游是哪个能力（名字）」——它不该去猜用户配的是哪一版。
		// 所以按名字匹配：`from_ref = name` 或者 `from_ref` 以 `name@` 开头
		// （用 substr 而不是 LIKE：名字里可能有 `%`/`_`，那种情况下 LIKE 会误匹配）。
		rows, err := d.QueryContext(ctx,
			`SELECT to_ref, literals, mapping, priority FROM edges
			 WHERE namespace_id = ? AND from_is_source = 0 AND from_port = ?
			   AND (from_ref = ? OR substr(from_ref, 1, length(?) + 1) = ? || '@')
			 ORDER BY to_ref, priority`, namespaceID, fromPort, fromKey, fromKey, fromKey)
		if err != nil {
			return nil, fmt.Errorf("lookup edges: %w", err)
		}
		defer rows.Close()
		all, err := scanLinkTargets(rows)
		if err != nil {
			return nil, err
		}
		// 同一个目标引用只派一次：`review --fix--> fixer` 与 `review@1 --fix--> fixer`
		// 两条行同时存在时，`fixer` 不该收到两个任务（那是扇出，不是重放）。
		out := make([]LinkTarget, 0, len(all))
		seen := make(map[string]bool, len(all))
		for _, t := range all {
			if seen[t.ToRef] {
				continue
			}
			seen[t.ToRef] = true
			out = append(out, t)
		}
		return out, nil
	}

	// 入口边：精确端口排前面（(from_port = '*') 升序 = 精确匹配先出），再按目标排序；
	// 第一个见到的目标引用就是这条边生效的那一行。
	rows, err := d.QueryContext(ctx,
		`SELECT to_ref, literals, mapping, priority FROM edges
		 WHERE namespace_id = ? AND from_is_source = 1 AND from_ref = ?
		   AND (from_port = ? OR from_port = '*')
		 ORDER BY to_ref, (from_port = '*'), priority`, namespaceID, fromKey, fromPort)
	if err != nil {
		return nil, fmt.Errorf("lookup entry edges: %w", err)
	}
	defer rows.Close()
	all, err := scanLinkTargets(rows)
	if err != nil {
		return nil, err
	}
	out := make([]LinkTarget, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, t := range all {
		if seen[t.ToRef] {
			continue // 精确端口那行已经赢过一次；同一个下游不重复派发
		}
		seen[t.ToRef] = true
		out = append(out, t)
	}
	return out, nil
}

// scanLinkTargets 把 EdgesFrom 的结果集扫成 LinkTarget（含 literals / mapping 归一）。
func scanLinkTargets(rows *sql.Rows) ([]LinkTarget, error) {
	out := []LinkTarget{}
	for rows.Next() {
		var t LinkTarget
		var literals, mapping string
		if err := rows.Scan(&t.ToRef, &literals, &mapping, &t.Priority); err != nil {
			return nil, fmt.Errorf("scan edge target: %w", err)
		}
		t.Literals = normalizeEdgeLiterals([]byte(literals))
		t.Mapping = normalizeEdgeMapping([]byte(mapping))
		out = append(out, t)
	}
	return out, rows.Err()
}

// graphSnapshot 是 graph_versions.snapshot 的规范化结构。
type graphSnapshot struct {
	Edges []graphEdge `json:"edges"`
}

type graphEdge struct {
	From string `json:"from"`
	Port string `json:"port"`
	To   string `json:"to"`
	// Priority 是这条边的候选优先级（契约 §3：小的优先；0 = 默认，省略）。
	//
	// 为什么它必须进快照：改优先级就改了「同一个端口会落到谁头上」，而
	// graph_versions 是事后追责的底图 —— 只改 priority 不落版，就答不出
	// 「当时那个下游为什么是它」。0 省略，保证加这一列之前算出的 hash 不变。
	Priority int `json:"priority,omitempty"`
	// Literals 是这条边的常量覆盖（已归一成紧凑 JSON；空覆盖省略）。
	//
	// 为什么它必须进快照：graph_versions 是事后追责的底图。只改某条边的常量
	// （比如把收件人从 u_1 换成 u_2）同样是「改了线」，不落新版本就会让
	// 「当时那条链路长什么样」永远查不出来。
	Literals string `json:"literals,omitempty"`
	// Mapping 是这条边的引用模板（已归一成紧凑 JSON；空模板省略）。
	//
	// 与 Literals 同理：改一条入口边的模板同样是「改了线」（同一个事件会喂给下游
	// 不同的参数），不落新版本就答不出「当时到底按哪份模板求值的」。
	Mapping string `json:"mapping,omitempty"`
}

// buildGraphSnapshot 把一张图归一成可排序、可哈希的形状。
//
// 排序是关键：边的插入顺序不改变图的语义，同一份图必须得同一个 hash，
// 否则只会在 graph_versions 里堆垃圾（契约 §10.2 的「按需落版」就失效了）。
// literals 也归一：JSON 对象的键序/空白不同但语义相同时，hash 必须相同。
//
// 快照存的是**引用**不是 id（P2.2）：否则能力发新版时，边一行没改，图却算出
// 另一个 hash（因为 id 变了）——那会让 graph_versions 凭空多出一版，
// 「同一张图」也就无从比对。
func buildGraphSnapshot(edges []*Edge) (graphSnapshot, error) {
	snap := graphSnapshot{Edges: make([]graphEdge, 0, len(edges))}
	for _, e := range edges {
		lit, err := canonicalJSONObject(e.Literals)
		if err != nil {
			return graphSnapshot{}, fmt.Errorf("graph snapshot literals: %w", err)
		}
		mapping, err := canonicalJSONObject(e.Mapping)
		if err != nil {
			return graphSnapshot{}, fmt.Errorf("graph snapshot mapping: %w", err)
		}
		snap.Edges = append(snap.Edges, graphEdge{
			From: e.FromRef, Port: e.FromPort, To: e.ToRef,
			Literals: lit, Mapping: mapping, Priority: e.Priority,
		})
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		a, b := snap.Edges[i], snap.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Literals != b.Literals {
			return a.Literals < b.Literals
		}
		return a.Mapping < b.Mapping
	})
	return snap, nil
}

// canonicalLiterals 把字面量归一成紧凑 JSON；空/无覆盖返回 ""（omitempty 省略）。
func canonicalLiterals(raw json.RawMessage) (string, error) { return canonicalJSONObject(raw) }

// canonicalJSONObject 把 edges 上的一列 JSON 对象（literals / mapping）归一成紧凑 JSON；
// 空 / null / {} 返回 ""（omitempty 省略）。
//
// 两列共用同一份归一：graph hash 的稳定性只该有一处实现——键序/空白不同但语义相同的
// 配置必须算出同一个 hash，否则 graph_versions 里会堆垃圾（契约 §10.2 的「按需落版」失效）。
func canonicalJSONObject(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("not a JSON object: %w", err)
	}
	if len(obj) == 0 {
		return "", nil
	}
	data, err := json.Marshal(obj) // map 键自动排序 → 语义相同即字节相同
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// graphContentHash 计算当前图的 content_hash：全部边排序后取 sha256。
//
// 排序是关键：边的插入顺序不改变图的语义，同一份图必须得同一个 hash，
// 否则只会在 graph_versions 里堆垃圾（契约 §10.2 的「按需落版」就失效了）。
func graphContentHash(edges []*Edge) (string, error) {
	hash, _, err := graphSnapshotPayload(edges)
	return hash, err
}

// graphSnapshotPayload 一次算出图快照的 (content_hash, snapshot JSON)。
//
// 为什么合成一次：hash 的定义就是 sha256(快照 JSON)，落库的 snapshot 列又是同一份字节。
// 分开算会让派发路径把整张图归一 + Marshal **两遍**（#264：EnsureGraphVersion 原来
// 先 graphContentHash(edges) 再 buildGraphSnapshot(edges)，同一份图算两次）。
// 合成后口径不变（hash 仍等于 sha256(snapshot)），只是不重复劳动。
func graphSnapshotPayload(edges []*Edge) (hash, payload string, err error) {
	snap, err := buildGraphSnapshot(edges)
	if err != nil {
		return "", "", err
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return "", "", fmt.Errorf("marshal graph snapshot: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), string(data), nil
}

// EnsureGraphVersion 按需落版（契约 §10.2）：当前图 hash 不存在才写，返回图版本。
//
// 只在「被派发用到过」时留版——从没被任何一次派发用过的图，事后没人会去查它。
//
// #264：**先读后写**。原来是无条件 INSERT ... ON CONFLICT DO NOTHING 再回查，
// 于是**图根本没变**（稳态里每一次派发都是这种情况）也要拿一次写锁 + 写一行
// （哪怕 ON CONFLICT 最后没改任何字节）——池=8、`_txlock=immediate`，
// 这是一次实打实的写事务串行化，不是“空操作”。现在：
//
//  1. 算 hash → 先按 (namespace_id, content_hash) 读一次；
//     命中就直接返回那行（零写、零回查）；
//  2. 未命中才走原来的 INSERT ... ON CONFLICT DO NOTHING + 回查。
//
// 并发安全不变（这是选“先查后写”而不是“自己造 id 直接返回”的理由）：
// 两个并发请求同时 miss 时，两者都去 INSERT，ON CONFLICT(namespace_id, content_hash)
// 只让一个落行，另一个落空；**两者随后都按 hash 回查**，拿到的必然是同一行、
// 同一个 graph_id。绝不会出现“两个请求为了同一份图各自造一个 id”。
// 反过来，若不加 INSERT 自己造 id，两个并发请求就会在 tasks.graph_id 里留下两个
// 不同的 id 指向同一张图——那正是这个函数要避免的。
func (d *DB) EnsureGraphVersion(ctx context.Context, namespaceID string) (*GraphVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	edges, err := d.ListEdges(ctx, namespaceID, "")
	if err != nil {
		return nil, err
	}
	// 快照只归一一次：hash 与 snapshot 列取的是同一份字节（见 graphSnapshotPayload）。
	hash, snapJSON, err := graphSnapshotPayload(edges)
	if err != nil {
		return nil, err
	}

	// ① 图没变（稳态）：直接返回已有版本，不 INSERT、不回查。
	// 读不带写锁——这正是把「每次派发一次写事务」从热路径上拿掉的那一步。
	gv, err := d.getGraphVersionByHash(ctx, namespaceID, hash)
	if err == nil {
		return gv, nil
	}
	if !errors.Is(err, ErrNotFound) {
		// 读失败不能“当作没有然后去写”：那会在一时抖动后多写一行（幂等但白付一次写锁）。
		return nil, fmt.Errorf("read graph version by hash: %w", err)
	}

	// ② 新图：落版。ON CONFLICT 挡住并发下的第二个写者。
	if _, err := d.ExecContext(ctx,
		`INSERT INTO graph_versions (id, namespace_id, content_hash, snapshot, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(namespace_id, content_hash) DO NOTHING`,
		NewGraphVersionID(hash), namespaceID, hash, snapJSON, Now()); err != nil {
		return nil, fmt.Errorf("insert graph version: %w", err)
	}
	// 回查：可能是刚插的，也可能是并发下别人先插的（此时用已有的 id）
	gv, err = d.getGraphVersionByHash(ctx, namespaceID, hash)
	if err != nil {
		return nil, fmt.Errorf("read graph version after upsert: %w", err)
	}
	return gv, nil
}

// GetGraphVersion 按 ID 取图快照。
func (d *DB) GetGraphVersion(ctx context.Context, namespaceID, id string) (*GraphVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return d.scanGraphVersion(ctx,
		`SELECT id, namespace_id, content_hash, snapshot, created_at FROM graph_versions
		 WHERE namespace_id = ? AND id = ?`, namespaceID, id)
}

// getGraphVersionByHash 按 (namespace_id, content_hash) 取图版本。
func (d *DB) getGraphVersionByHash(ctx context.Context, namespaceID, hash string) (*GraphVersion, error) {
	return d.scanGraphVersion(ctx,
		`SELECT id, namespace_id, content_hash, snapshot, created_at FROM graph_versions
		 WHERE namespace_id = ? AND content_hash = ?`, namespaceID, hash)
}

// scanGraphVersion 执行查询并解析一行；未命中返回 ErrNotFound。
func (d *DB) scanGraphVersion(ctx context.Context, query string, args ...any) (*GraphVersion, error) {
	var g GraphVersion
	var snapshot string
	err := d.QueryRowContext(ctx, query, args...).Scan(&g.ID, &g.NamespaceID, &g.ContentHash, &snapshot, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan graph version: %w", err)
	}
	g.Snapshot = json.RawMessage(snapshot)
	return &g, nil
}

// GraphHasEdge 判断快照里是否含某条边（便于测试与调试）。
func (g *GraphVersion) GraphHasEdge(from, port, to string) bool {
	var snap graphSnapshot
	if err := json.Unmarshal(g.Snapshot, &snap); err != nil {
		return false
	}
	for _, e := range snap.Edges {
		if e.From == from && e.Port == port && e.To == to {
			return true
		}
	}
	return false
}
