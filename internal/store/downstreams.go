package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件是「在能力里整体替换下游」（CONTRACT-downstream-edit §2）的写入入口。
//
// 为什么是 replace 而不是 append / delete：
//
//	用户在能力编辑里看到的是**这组下游**（名字 + 优先级）。保存时的语义只能是
//	「这组就是全部」——否则「删掉一个」只能靠哨兵值（空字符串 / null）表达，
//	而哨兵值会被任何一个漏传字段的客户端无意中触发。
//
// 一条能力出边 = (from_ref=能力名, from_port='out', to_ref, priority, literals)：
//   - from_ref 是**名字**（不是版本）：一个能力的所有版本共用同一组出边；
//   - from_port 固定隐式出口 `out`（ImplicitPort）——界面与 API 都不出现它；
//   - to_ref 是引用（`name` 跟随 latest / `name@version` 钉死）。
//
// 事务边界：**全部校验都跑完之后才动第一行**（任一失败 → 400 且一个字节都不写）。
// 校验与写入必须在同一个事务里（读走 tx）：否则「查完到写完」之间别人加了新版本，
// literals 就会按一个已经过期的 params_schema 落库。

// DownstreamsMaxLinks 是一次整体替换允许的最大下游条数（契约 §2`too_many_links`）。
//
// 上限的作用不是性能，而是**误操作护栏**：把整个命名空间的能力都塞进一个端口
// 通常不是用户想要的，而是某个循环写出来的。
const DownstreamsMaxLinks = 100

// 候选优先级的合法区间（契约 §1：小的优先，0 = 默认）。
const (
	DownstreamPriorityMin = 0
	DownstreamPriorityMax = 1000
)

// DownstreamSpec 是「整体替换下游」请求里的一条。
//
// 字段口径与 Edge 的对应关系：
//
//	ToRef    → Edge.ToRef（`name` 跟随 latest / `name@version` 钉死；能力 id 也接受并归一）
//	Priority → Edge.Priority（小的优先；缺省 0）
//	Literals → Edge.Literals（常量覆盖，键与类型受**解析到的目标版本** params_schema 约束）
//
// 没有 Mapping：能力边不接受引用模板（设计 §8）。
type DownstreamSpec struct {
	ToRef    string          `json:"toRef"`
	Priority int             `json:"priority"`
	Literals json.RawMessage `json:"literals"`
	// FromPort 是**出边挂在哪个出口上**，缺省 = 隐式出口 `out`。
	//
	// 为什么还要这个字段：界面早就只有一个出口了（CONTRACT-hide-ports），但模型里
	// 端口仍然存在，而且**历史数据的多出口能力**（`outputs: ["a","b"]`）与 e2e 的
	// 具名端口用例都要能表达「这条边挂在 go 上」。把它砍掉会让「一次下游保存」只能
	// 作用于 out，具名端口的能力就再也没法用这个端点维护自己的拓扑。
	FromPort string `json:"fromPort,omitempty"`
}

// DownstreamLink 是落库后的一条下游（回执）。
//
// 除用户提交的字段外还带两个**派生**字段：解析到的能力定义 id 与版本。
// 前端回显要用 id 做详情链接、用版本显示「跟随到哪一版」，而它不该自己实现
// name→latest（那是第二份解析语义，两边一漂就是「页面说 v2、派发走 v3」）。
type DownstreamLink struct {
	ToRef    string          `json:"toRef"`
	Priority int             `json:"priority"`
	Literals json.RawMessage `json:"literals"`
	// FromPort 是这条边挂的出口（回执里带回来，缺省 out）。
	FromPort string `json:"fromPort,omitempty"`
	// ToCapabilityID 是 toRef 在**写入那一刻**解析到的能力定义 id。
	ToCapabilityID string `json:"toCapabilityId"`
	// ResolvedVersion 是同一个定义的能力版本号（`name` 型引用 = 此刻的 latest）。
	ResolvedVersion int `json:"resolvedVersion"`
}

// DownstreamError 是「整体替换下游」的**客户端可修**错误（HTTP 400）。
//
// Code 是响应里的机器码（契约 §2：unknown_target / self_link / too_many_links /
// duplicate_target / priority_out_of_range）。为什么要一个独立类型而不是若干哨兵：
// 前端要按 code 高亮**哪一行**（self_link 是「不能连自己」、duplicate 是「重复」），
// 只给一句中文文案的话每个客户端都得再解析一遍字符串。
type DownstreamError struct {
	Code string
	Err  error
}

func (e *DownstreamError) Error() string {
	if e == nil || e.Err == nil {
		return "downstreams rejected"
	}
	return e.Err.Error()
}

func (e *DownstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// downstreamRejected 造一个带机器码的「客户端可修」错误。
func downstreamRejected(code, format string, args ...any) error {
	return &DownstreamError{Code: code, Err: fmt.Errorf(format, args...)}
}

// ReplaceCapabilityDownstreams 用 links **整体替换**某个能力的出边
// （from_is_source=0、from_port='out'），返回落库后的集合（与 links 同序）。
//
// 空切片是合法输入：清空 = 这个能力没有下游（终端能力）。
//
// 校验（全部先做，任一失败一个字节都不写）：
//
//	unknown_target         toRef 在本命名空间解析不到（跨命名空间引用保持 *CrossNamespaceError）
//	self_link              toRef 指向能力自己（一个能力接回自己没有意义，环检测已删）
//	too_many_links         条目数 > DownstreamsMaxLinks
//	priority_out_of_range  priority ∉ [0, 1000]
//	duplicate_target       同一个 toRef（按归一后的引用判）出现两次
//	ErrEdgeRejected        literals 不满足目标 params_schema（与 CreateEdge 同一份校验）
//
// 左端能力必须存在（ErrNotFound → handler 404）：这个端点只改**已有能力**的出边，
// 名字打错时给出 404 而不是「悄悄建了一组没人能走的边」。
//
// 只动这个能力的**隐式出口**上的边：入口边（from_is_source=1）、别人的边、以及
// 多出口能力别的端口上的边一条都不碰。一次成功的 replace 之后，这个能力在 `out`
// 上的下游**按 toRef** 恰好是提交的那组（没提交的被删、提交的被 upsert）。
func (d *DB) ReplaceCapabilityDownstreams(ctx context.Context, namespaceID, name string, links []DownstreamSpec) ([]*DownstreamLink, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	// 路径里给的是**名字**：`name@version` 的版本部分在这里没有意义（所有版本共用
	// 同一组出边），所以只取名字部分——`notify` 与 `notify@3` 是同一个左端。
	name, _, _ = SplitRef(strings.TrimSpace(name))
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty capability name", ErrRefUnresolved)
	}
	// 条数上限与库里无关，先判（省一次事务）。
	if len(links) > DownstreamsMaxLinks {
		return nil, downstreamRejected("too_many_links",
			"downstreams: %d links submitted, at most %d allowed", len(links), DownstreamsMaxLinks)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("replace downstreams: begin tx: %w", err)
	}
	// 早退 / panic 都回滚：校验与写入之间不能留下半个拓扑。
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① 左端必须存在（校验的第一条就是这个能力本身）。
	capDef, err := getCapabilityLatestOn(ctx, ex, namespaceID, name)
	if err != nil {
		return nil, err
	}
	// 允许的端口集合 = 产物声明的 ∪ 库里已有出边的。空集合也能写（第一次接下游时
	// 取缺省 out：这就是"终端能力在编辑里加了一条下游"的那个真实场景）。
	allowedPorts, err := capabilityPortsOn(ctx, ex, namespaceID, name, capDef)
	if err != nil {
		return nil, err
	}

	// ② 全部校验：目标能解析 / 不是自己 / 优先级在区间 / literals 合法 / 不重复。
	// 这一步**只读**，所以下面任何一条失败都还没有写过库。
	out := make([]*DownstreamLink, 0, len(links))
	seen := make(map[string]bool, len(links))
	seenPort := make(map[string]bool, len(links))
	for i, l := range links {
		toRef, to, err := resolveLinkRefOn(ctx, ex, namespaceID, l.ToRef, fmt.Sprintf("downstreams[%d].toRef", i))
		if err != nil {
			// 跨命名空间有独立的错误类型（I11）：保持它，让 handler 走统一的 400 文案。
			var cross *CrossNamespaceError
			if errors.As(err, &cross) {
				return nil, err
			}
			// 只有「解析不到 / 引用是空的」才是 unknown_target（客户端可修）。
			// 其它（scan 失败 / context 取消 / 库坏了）**不能**披成 400：那会把一个
			// 内部故障报成「目标不存在」，用户改一辈子名字也修不好。
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRefUnresolved) {
				return nil, downstreamRejected("unknown_target",
					"downstreams[%d]: unknown target %q", i, strings.TrimSpace(l.ToRef))
			}
			return nil, fmt.Errorf("downstreams[%d]: resolve %q: %w", i, strings.TrimSpace(l.ToRef), err)
		}
		// 钉死引用归一成规范形态（`a@01` → `a@1`）：同一个 (名字, 版本) 只能有一条边，
		// 否则重复判定会被前导零这种写法绕过（落成两行、主键不同、派发各走一遍）。
		if _, version, pinned := SplitRef(toRef); pinned {
			toRef = FormatRef(to.Name, version)
		}
		if to.Name == name {
			return nil, downstreamRejected("self_link",
				"downstreams[%d]: %q cannot link to itself", i, name)
		}
		// 端口：缺省 out；非空但不在「声明过的 ∪ 已有出边的」里 → 拒绝。
		// 这一条保住不变量 I2（出口封闭）：允许的端口集合**只能**由产物声明或
		// 已有边扩展，不能凭空造一个新端口名出来。
		port := strings.TrimSpace(l.FromPort)
		if port == "" {
			port = ImplicitPort
		}
		if !allowedPorts[port] {
			return nil, downstreamRejected("unknown_port",
				"downstreams[%d]: port %q is not declared by this capability", i, port)
		}
		if seenPort[port+"/"+toRef] {
			return nil, downstreamRejected("duplicate_target",
				"downstreams[%d]: duplicate target %q on port %q", i, toRef, port)
		}
		seenPort[port+"/"+toRef] = true

		if l.Priority < DownstreamPriorityMin || l.Priority > DownstreamPriorityMax {
			return nil, downstreamRejected("priority_out_of_range",
				"downstreams[%d]: priority %d out of range [%d, %d]",
				i, l.Priority, DownstreamPriorityMin, DownstreamPriorityMax)
		}
		// 按**归一后**的引用判重：`notify@1` 与能力 id 指向同一行，重复提交两次
		// 会撞主键（第二次变成 ON CONFLICT DO UPDATE）——那会让「重复」悄悄变成「覆盖」。
		if seen[toRef] {
			return nil, downstreamRejected("duplicate_target",
				"downstreams[%d]: duplicate target %q", i, toRef)
		}
		seen[toRef] = true
		// literals 的配置期硬拒与 CreateEdge 完全同源（同一份 checkEdgeLiterals）：
		// 键 ∈ 目标 params_schema.properties、值类型精确相等。
		literals, err := checkEdgeLiterals(to, l.Literals)
		if err != nil {
			return nil, fmt.Errorf("downstreams[%d]: %w", i, err)
		}
		out = append(out, &DownstreamLink{
			ToRef:           toRef,
			Priority:        l.Priority,
			Literals:        literals,
			FromPort:        port,
			ToCapabilityID:  to.ID,
			ResolvedVersion: to.Version,
		})
	}

	// ③ 删掉本次没提交的出边，**按端口分组**。
	//
	// WHERE 里三件事一个都不能少：from_is_source=0 不动入口边；from_ref=名字 不动
	// 别人的边；from_port=? 只动**本次提交过**的端口——历史的**多出口能力**
	// （`outputs` 长度 > 1）在界面上是只读保留的（CONTRACT-hide-ports §5：不替用户
	// 做破坏性改写），它们挂在别的端口上的边不归这条「下游整体替换」管。
	byPort := make(map[string][]string, 2)
	for _, l := range out {
		byPort[l.FromPort] = append(byPort[l.FromPort], l.ToRef)
	}
	// 提交了空数组 = 清空：这时没有任何端口信息，按隐式出口处理（与下面 upsert
	// 的缺省一致；具名端口的多出口能力要靠显式提交那一组才算维护它，见注释）。
	if len(out) == 0 {
		byPort[ImplicitPort] = nil
	}
	ports := make([]string, 0, len(byPort))
	for port := range byPort {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	for _, port := range ports {
		refs := byPort[port]
		if len(refs) == 0 {
			if _, err := ex.execContext(ctx,
				`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ? AND from_port = ?`,
				namespaceID, name, port); err != nil {
				return nil, fmt.Errorf("clear downstreams on %q: %w", port, err)
			}
			continue
		}
		args := make([]any, 0, 4+len(refs))
		args = append(args, namespaceID, name, port)
		for _, ref := range refs {
			args = append(args, ref)
		}
		if _, err := ex.execContext(ctx,
			`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ? AND from_port = ?
			   AND to_ref NOT IN (`+sqlPlaceholders(len(refs))+`)`,
			args...); err != nil {
			return nil, fmt.Errorf("prune downstreams on %q: %w", port, err)
		}
	}

	// ④ 其余 upsert：同一条边改的是 priority / literals（**不**刷新 created_at——
	//    它是这条线第一次被接上的时刻，不是最后一次被编辑的时刻）。
	now := Now()
	for _, l := range out {
		if _, err := ex.execContext(ctx,
			`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
			 VALUES (?,0,?,?,?,?,'{}',?,?)
			 ON CONFLICT(namespace_id, from_is_source, from_ref, from_port, to_ref)
			 DO UPDATE SET priority = excluded.priority, literals = excluded.literals`,
			namespaceID, name, l.FromPort, l.ToRef, string(l.Literals), l.Priority, now); err != nil {
			return nil, fmt.Errorf("upsert downstream %q: %w", l.ToRef, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("replace downstreams: commit: %w", err)
	}
	return out, nil
}

// capabilityPortsOn 返回「这个能力的出边允许挂哪些端口」= 产物声明的 ∪ 库里已有出边的。
//
// 空集合时补上隐式出口 `out`：这是"建时声明 outputs: []（终端），后来在编辑里加了
// 一条下游"的真实场景——不补的话第一次接下游就会被 unknown_port 挡回去。
//
// 注意它只用于**校验**，不改正文：next 属于能力行（改线是原地更新，见
// capability.go 的 UpdateNext），派发侧按「声明的 next ∪ 库里的出边」理解这张图。
//
// v4 没有端口概念，本函数名与端口语义都是 v3 残留，随 G1-3 重写 downstreams 一并删除。
func capabilityPortsOn(ctx context.Context, ex txExec, namespaceID, name string, capDef *CapabilityVersion) (map[string]bool, error) {
	// v4 没有端口：能力侧的「可挂下游」就是它声明的 spec.next。
	allowed := make(map[string]bool, len(capDef.Spec.Next)+2)
	for _, p := range capDef.Spec.Next {
		if p = strings.TrimSpace(p); p != "" {
			allowed[p] = true
		}
	}
	rows, err := ex.queryContext(ctx,
		`SELECT DISTINCT from_port FROM edges
		  WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`, namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("list existing ports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan existing port: %w", err)
		}
		if p != "" {
			allowed[p] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list existing ports: %w", err)
	}
	// 隐式出口**永远**允许：界面模型里一个能力只有一个出口，它叫 out
	// （CONTRACT-hide-ports §1）。历史的具名端口（outputs: ["fix"]）是产物声明，
	// 两种都在集合里——这里补 out 而不是放宽校验，具名端口写错照样 unknown_port。
	allowed[ImplicitPort] = true
	return allowed, nil
}
