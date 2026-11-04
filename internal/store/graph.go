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

// 一行 = 一条边；同一 (FromCapability, FromPort) 可有多行 → 扇出（不变量 I7：
// 两端都是 capability_id，图里没有机器）。
type Edge struct {
	// NamespaceID 是隔离维度（I11）：边属于某个命名空间，
	// 同一对能力可以在不同命名空间各有一条边，互不影响。
	NamespaceID string `json:"namespaceId"`

	// FromIsSource 决定 FromCapability 字段怎么解释（字段名为了兼容而保留）：
	//
	//   false（默认）  FromCapability = 上游能力的 capability_versions.id
	//                  fromPort 必须 ∈ 该能力的 outputs（不变量 I2）
	//
	//   true            FromCapability = **来源名**（"github"/"jira"/…）
	//                  fromPort = 事件类型，任意值（外部系统决定）
	//
	// 第二种就是「入口边」：机器入口 POST /v1/events?source=github 靠它找下游。
	// 它原先靠一个叫 github@1 的空壳能力当锚点，现在不需要了 —— 那种能力
	// 不执行代码、不产生任务、也不可配置，却会出现在能力列表上。
	FromIsSource   bool   `json:"fromIsSource"`
	FromCapability string `json:"fromCapability"`
	FromPort       string `json:"fromPort"`
	ToCapability   string `json:"toCapability"`

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
}

// LinkTarget 是「这条边通向哪里」+「路上带哪些常量/模板」，即 EdgesFrom 的一行。
//
// 为什么不是 []string：派发第 ① 步除了目标还得拿到这条边的字面量覆盖与引用模板
// （设计 §3.3 / §11.5），分两次查（先拿 id，再回头查 literals/mapping）会多一次往返，
// 也让「边」这个概念在两个包里各有一半。
//
// CapabilityID 是目标能力**定义 id**（不是 name@version，同 capability_versions.id）。
type LinkTarget struct {
	CapabilityID string `json:"capabilityId"`
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
// 配置期校验（契约 §6.2 / 不变量 I2 / A5 / I11 / 设计 §5「定义期（存 link）」）：
//   - from_port 必须 ∈ from 能力的 outputs（封闭集合，不能凭空发明出口）；入口边例外：
//     事件类型由外部系统决定，也可以写成 "*" = 任意事件类型
//   - 两端能力必须同**命名空间**（I11 第 ① 条：跨命名空间引用一律拒绝）
//   - Literals 必须是 JSON 对象，每个键 ∈ 目标 params_schema.properties，
//     且值类型与声明**精确相等**（不转换）——拼错/写错当场拒，不要等派发时才剪枝
//   - Mapping 只允许出现在**入口边**，且必须是 JSON 对象、键 ∈ 目标
//     params_schema.properties、值是「常量或 {{点号路径}} 模板」（常量才查类型）。
//     能力边带非空 mapping → 硬拒（§8 已否决能力边的引用映射）
//
// 违反返回 ErrEdgeRejected / *CrossNamespaceError。重复边幂等（PK 冲突视为已存在）。
func (d *DB) CreateEdge(ctx context.Context, e *Edge) error {
	if e == nil {
		return fmt.Errorf("create edge: nil")
	}
	if e.NamespaceID == "" || e.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if e.FromCapability == "" || e.FromPort == "" || e.ToCapability == "" {
		return fmt.Errorf("%w: from/port/to all required", ErrEdgeRejected)
	}
	// I11 第 ① 条（右端）：目标能力必须属于同命名空间。
	if err := d.checkCapabilityInNamespace(ctx, e.NamespaceID, e.ToCapability, "edges.to_capability"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: to capability %q: %v", ErrEdgeRejected, e.ToCapability, err)
		}
		return err
	}
	to, err := d.GetCapabilityByID(ctx, e.NamespaceID, e.ToCapability)
	if err != nil {
		return fmt.Errorf("%w: to capability %q: %v", ErrEdgeRejected, e.ToCapability, err)
	}

	// 字面量覆盖的配置期硬拒（设计 §5）：常量不依赖运行时，所以能在这里判类型。
	// 归一化后的值回写 e.Literals（空 → "{}"），保证写入/响应都不会是 null。
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
		// from_port 可以是 "*"（= 任意事件类型）：迁移 26 搬过来的源目标就是它，
		// 新建入口边时用户也可以直接写它来省掉逐个 type 建边。
		//
		// 也要校验来源名不比能力名更容易撞车：来源名非空即可。
	} else {
		// I11 第 ① 条（左端）：上游能力也必须属于同命名空间。
		//
		// 能力 id 全局唯一，把别处的 id 填进来在 SQL 层是合法的
		// （外键只保证行存在），所以这里必须显式校验。
		if err := d.checkCapabilityInNamespace(ctx, e.NamespaceID, e.FromCapability, "edges.from_capability"); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: from capability %q: %v", ErrEdgeRejected, e.FromCapability, err)
			}
			return err
		}
		from, err := d.GetCapabilityByID(ctx, e.NamespaceID, e.FromCapability)
		if err != nil {
			return fmt.Errorf("%w: from capability %q: %v", ErrEdgeRejected, e.FromCapability, err)
		}
		// 出口封闭集合（不变量 I2）：from_port 必须属于该能力的 outputs。
		if !from.HasOutput(e.FromPort) {
			return fmt.Errorf("%w: port %q not in %s@%d outputs %v",
				ErrEdgeRejected, e.FromPort, from.Name, from.Version, from.Outputs)
		}

		// 静态环检测（设计 §7 P2.3）。
		//
		// 为什么要在保存时拒，而不是只靠运行期 path 剪枝：
		//   - 环是**配置错误**，不是运行时状况。等它跑起来才发现，代价是一个真实的
		//     无限扇出（每跳都新建任务，直到深度/环剪枝兜住）——可见，但很难查。
		//   - 运行期 path 剪枝必须保留（它是防线的最后一层：图可能被改，旧任务还在跑），
		//     但那不该是唯一一层。
		//
		// 判据：从 to 出发沿已有边能否走回 from。走得到 → 加了这条就成环。
		// 只读当前命名空间的边（I11：图不跨命名空间）。
		if err := d.checkEdgeWouldCycle(ctx, e.NamespaceID, e.FromCapability, e.ToCapability); err != nil {
			return err
		}
	}

	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO edges (namespace_id, from_is_source, from_key, from_port, to_capability, literals, mapping, created_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(namespace_id, from_is_source, from_key, from_port, to_capability) DO NOTHING`,
		e.NamespaceID, boolToInt(e.FromIsSource), e.FromCapability, e.FromPort, e.ToCapability,
		string(e.Literals), string(e.Mapping), e.CreatedAt); err != nil {
		return fmt.Errorf("insert edge: %w", err)
	}
	return nil
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
func (d *DB) DeleteEdge(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort, toCapability string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM edges
		 WHERE namespace_id = ? AND from_is_source = ? AND from_key = ? AND from_port = ? AND to_capability = ?`,
		namespaceID, boolToInt(fromIsSource), fromKey, fromPort, toCapability)
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

// ListEdges 列出**命名空间内**的边（fromCapability 非空时按它过滤）；空列表返回 []。
func (d *DB) ListEdges(ctx context.Context, namespaceID, fromKey string) ([]*Edge, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	q := `SELECT namespace_id, from_is_source, from_key, from_port, to_capability, literals, mapping, created_at
	        FROM edges WHERE namespace_id = ?`
	args := []any{namespaceID}
	if fromKey != "" {
		q += ` AND from_key = ?`
		args = append(args, fromKey)
	}
	q += ` ORDER BY from_is_source, from_key, from_port, to_capability`
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list edges: %w", err)
	}
	defer rows.Close()
	out := []*Edge{}
	for rows.Next() {
		var e Edge
		var isSrc int
		var literals, mapping string
		if err := rows.Scan(&e.NamespaceID, &isSrc, &e.FromCapability, &e.FromPort, &e.ToCapability,
			&literals, &mapping, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		e.FromIsSource = isSrc == 1
		e.Literals = normalizeEdgeLiterals([]byte(literals))
		e.Mapping = normalizeEdgeMapping([]byte(mapping))
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EdgesFrom 是派发第 ① 步的**唯一**查表入口（契约 §4）：
// 返回 (namespaceID, fromKey, fromPort) 的全部下游
// （能力 id + 该条边的字面量覆盖 + 该条边的引用模板）。
//
// fromIsSource 决定 fromKey 是能力 id 还是来源名（见 Edge.FromIsSource）。
//
// **入口边的端口匹配放宽**（P2.1）：入口边可以写 from_port="*"，含义是「这个源的任意
// 事件类型都走这条边」——源能收什么事件由外部系统决定，让用户为每个 type 手建一条边
// 是多余的心智负担（迁移 26 搬过来的源目标就是一个 "*"）。所以 fromIsSource=true 时
// 匹配 `from_port = ? OR from_port = '*'`；能力边的出口是能力的封闭契约（I2），不放宽。
//
// 精确端口优先于 "*"，且同一目标能力只返回一行：一条精确边 + 一条 "*" 边指向同一个
// 下游时，那个下游只能收到**一次**派发（否则一个事件会变成两个任务）。
//
// 除了 route 包，其他包不得再写 `FROM edges`（见 #56 的验收：全仓只允许 1 处）。
func (d *DB) EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort string) ([]LinkTarget, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if !fromIsSource {
		rows, err := d.QueryContext(ctx,
			`SELECT to_capability, literals, mapping FROM edges
			 WHERE namespace_id = ? AND from_is_source = 0 AND from_key = ? AND from_port = ?
			 ORDER BY to_capability`, namespaceID, fromKey, fromPort)
		if err != nil {
			return nil, fmt.Errorf("lookup edges: %w", err)
		}
		defer rows.Close()
		return scanLinkTargets(rows)
	}

	// 入口边：精确端口排前面（(from_port = '*') 升序 = 精确匹配先出），再按目标排序；
	// 第一个见到的目标能力就是这条边生效的那一行。
	rows, err := d.QueryContext(ctx,
		`SELECT to_capability, literals, mapping FROM edges
		 WHERE namespace_id = ? AND from_is_source = 1 AND from_key = ?
		   AND (from_port = ? OR from_port = '*')
		 ORDER BY to_capability, (from_port = '*')`, namespaceID, fromKey, fromPort)
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
		if seen[t.CapabilityID] {
			continue // 精确端口那行已经赢过一次；同一个下游不重复派发
		}
		seen[t.CapabilityID] = true
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
		if err := rows.Scan(&t.CapabilityID, &literals, &mapping); err != nil {
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
			From: e.FromCapability, Port: e.FromPort, To: e.ToCapability,
			Literals: lit, Mapping: mapping,
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
	snap, err := buildGraphSnapshot(edges)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("marshal graph snapshot: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// EnsureGraphVersion 按需落版（契约 §10.2）：当前图 hash 不存在才写，返回图版本。
//
// 只在「被派发用到过」时留版——从没被任何一次派发用过的图，事后没人会去查它。
// 用 INSERT ... ON CONFLICT DO NOTHING 后回查，保证同一份图只有一行。
func (d *DB) EnsureGraphVersion(ctx context.Context, namespaceID string) (*GraphVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	edges, err := d.ListEdges(ctx, namespaceID, "")
	if err != nil {
		return nil, err
	}
	hash, err := graphContentHash(edges)
	if err != nil {
		return nil, err
	}
	snap, err := buildGraphSnapshot(edges)
	if err != nil {
		return nil, err
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal graph snapshot: %w", err)
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO graph_versions (id, namespace_id, content_hash, snapshot, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(namespace_id, content_hash) DO NOTHING`,
		NewGraphVersionID(hash), namespaceID, hash, string(snapJSON), Now()); err != nil {
		return nil, fmt.Errorf("insert graph version: %w", err)
	}
	// 回查：可能是刚插的，也可能是并发下别人先插的（此时用已有的 id）
	gv, err := d.getGraphVersionByHash(ctx, namespaceID, hash)
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

// checkEdgeWouldCycle 判定「再加一条 from → to 的边」会不会让图出现环。
//
// 算法：从 to 出发做一次 DFS（只看能力 → 能力的边，入口边没有左端、不可能成环），
// 看能否到达 from。能到达 = 新增这条边会闭环。
//
// 为什么不用 Tarjan / 拓扑排序：这里是**单次增量检查**，图是配置期的小对象
// （一条链路十几个节点），一次 DFS 足够且不需要维护额外状态；全量拓扑排序更适合
// 「整图校验」而不是「每加一条边校验一次」。
//
// 自环（from == to）单独判：DFS 从 to 出发本来就会立刻命中自己，但报错文案应当
// 直接说「自己连自己」，而不是绕一圈说「能走回自己」。
func (d *DB) checkEdgeWouldCycle(ctx context.Context, namespaceID, from, to string) error {
	if from == to {
		return fmt.Errorf("%w: self loop %s --%s--> itself", ErrEdgeRejected, from, to)
	}
	edges, err := d.ListEdges(ctx, namespaceID, "")
	if err != nil {
		return fmt.Errorf("cycle check: load edges: %w", err)
	}
	adj := make(map[string][]string, len(edges))
	for _, e := range edges {
		if e.FromIsSource {
			continue // 入口边没有上游能力，接不进环
		}
		adj[e.FromCapability] = append(adj[e.FromCapability], e.ToCapability)
	}

	// DFS：从 to 出发，看能否走到 from。
	seen := make(map[string]bool, len(adj))
	stack := []string{to}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == from {
			return fmt.Errorf("%w: adding %s → %s would close a cycle (path %s → … → %s)",
				ErrEdgeRejected, from, to, to, from)
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		stack = append(stack, adj[cur]...)
	}
	return nil
}
