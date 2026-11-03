package route

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 下游目录 / expects 推导（设计 §3.4） ----------
//
// 节点不掌握拓扑，但实现（脚本 / LLM agent）必须知道「我这个出口能去哪、对面要什么」——
// 否则它拼不出对的下游 params，只能被拒了再猜。
//
// 所以派发时随 assign 下发一份**推导量**（不落库）：
//
//	expects[port].schema  = 该 port 所有下游 params_schema（各自减去 literals 键）的并集
//	expects[port].targets = 逐条下游（能力名 / 版本 / 它还需要哪些字段）
//
// schema 是机器门槛（节点 emit 时自检），targets 是给人/LLM 看的（主动拼对参数）。
//
// 两条刻意的设计（都来自 §4.3 与 §12 的否决记录）：
//   - **无 link 的 port 不出现在 map 里**：没有下游 = 没有约束，不是「要求空对象」。
//   - 并集只用于**提示**，中枢派发时仍然逐 link 校验（第二道）。并集当硬门槛会把
//     「扇出部分成功」变成「整体失败」——那正是被否决的方案。

// PortExpect 是一个端口的产出要求（对应 ws.PortExpect 的线上形状）。
//
// Schema 为空 = 这个 port 有下游但下游什么都不要（或者要求的字段全被 link 的
// 字面量覆盖了）；此时只做结构校验。
type PortExpect struct {
	Schema  json.RawMessage `json:"schema,omitempty"`
	Targets []ExpectTarget  `json:"targets,omitempty"`
}

// ExpectTarget 描述一个下游：谁、哪一版、它还需要哪些字段（已减去被字面量覆盖的）。
type ExpectTarget struct {
	Capability string          `json:"capability"`
	Version    int             `json:"version,omitempty"`
	Needs      json.RawMessage `json:"needs,omitempty"`
}

// BuildExpects 推导「这个能力的这些出口各自要被怎样消费」。
//
// 参数是**能力定义 id**（edges.from_key 现在的口径），不是能力名：
// link 的右端目前引用 capability_versions.id，而一个名字可以有多版本、
// 各版本的 id 不同——按名字解析会（在升级后）查到另一版的行，推导出错误的 expects。
// P2 把 link 改成名字引用（typed-pipeline §7 P2.2）时，这里的入参跟着换成名字。
//
// 逐 port：查 (capability, port) 的 link → 逐条算出「下游要什么（去掉 literals）」→
// 并集当 Schema、逐条当 Targets。查询失败一律向上抛（派发时不带 expects 会退化成
// 「节点不知道下游要什么」，那比一次可见的失败更难查）。
func (d *Dispatcher) BuildExpects(ctx context.Context, namespaceID, capabilityID string, ports []string) (map[string]PortExpect, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("route: build expects: dispatcher has no store")
	}
	if namespaceID == "" || namespaceID == store.AllNamespaces {
		return nil, fmt.Errorf("route: build expects: %w", store.ErrNamespaceRequired)
	}
	out := make(map[string]PortExpect, len(ports))
	if len(ports) == 0 {
		return out, nil
	}
	if capabilityID == "" {
		return nil, fmt.Errorf("route: build expects: empty capability id")
	}
	// 先确认能力定义存在。不校验的话，「能力 id 写错了」与「这个 port 没接下游」
	// 会得到**完全相同**的结果（空 expects）——而前者是数据坏了的信号，
	// 告退成「节点不知道下游要什么」只会让问题晚一步、难一步被发现。
	if _, err := d.Store.GetCapabilityByID(ctx, namespaceID, capabilityID); err != nil {
		return nil, fmt.Errorf("route: build expects: load capability %s: %w", capabilityID, err)
	}

	for _, port := range ports {
		// 同一个 port 的全部 link（1:N 扇出）。没有 link → 这个 port 没有约束，
		// 不出现在 map 里（而不是给一个空 schema：那会让节点把「没下游」和
		// 「下游什么都不要」混成一件事）。
		targets, err := d.Store.EdgesFrom(ctx, namespaceID, false, capabilityID, port)
		if err != nil {
			return nil, fmt.Errorf("route: build expects: edges from %s port %q: %w", capabilityID, port, err)
		}
		if len(targets) == 0 {
			continue
		}

		exp := PortExpect{Targets: make([]ExpectTarget, 0, len(targets))}
		var required []string
		props := map[string]any{}
		for _, tgt := range targets {
			to, err := d.Store.GetCapabilityByID(ctx, namespaceID, tgt.CapabilityID)
			if err != nil {
				return nil, fmt.Errorf("route: build expects: load target %s: %w", tgt.CapabilityID, err)
			}
			needs, err := needsSchema(to, tgt.Literals)
			if err != nil {
				return nil, err
			}
			exp.Targets = append(exp.Targets, ExpectTarget{
				Capability: to.Name,
				Version:    to.Version,
				Needs:      needs.raw,
			})
			required = append(required, needs.required...)
			// 同一字段在多个下游里出现时保留**先到的**那份定义。
			// 类型冲突在保存 link 时已硬拒（设计 §5「同一 port 的多个下游要求
			// 同一字段不同类型 → 拒」），所以这里不需要（也不该）报错。
			for k, v := range needs.properties {
				if _, ok := props[k]; !ok {
					props[k] = v
				}
			}
		}
		exp.Schema, err = unionSchema(required, props)
		if err != nil {
			return nil, fmt.Errorf("route: build expects: union for port %q: %w", port, err)
		}
		out[port] = exp
	}
	return out, nil
}

// schemaNeeds 是一个下游「去掉字面量之后还需要什么」。
type schemaNeeds struct {
	// raw 是可直接下发的 JSON（空=下游什么都没要）。
	raw json.RawMessage
	// required / properties 是 raw 的两部分，供并集累加。
	required   []string
	properties map[string]any
}

// needsSchema 从一个下游能力的 params_schema 里减掉这条 link 的字面量键。
//
// 为什么要减：字面量是配置里的常量（§3.3），进入下游的 params = data ⊎ literals
// 且字面量优先。所以被覆盖的键**不该出现在 expects 里**——否则实现会以为
// 「我还得提供 to」，而它给了也没用（会被常量盖掉）。
func needsSchema(to *store.CapabilityVersion, literals json.RawMessage) (*schemaNeeds, error) {
	drop := map[string]bool{}
	if len(literals) > 0 {
		var lit map[string]any
		if err := json.Unmarshal(literals, &lit); err == nil {
			for k := range lit {
				drop[k] = true
			}
		}
		// 坏 literals 不在这里报错：配置期（store.CreateEdge）已硬拒，
		// 走到这里坏掉说明数据被手改；此时「不提示减掉哪些键」比让整次派发
		// 拿不到 expects 更轻（派发期对每条 link 的校验仍是 fail-closed）。
	}

	var root map[string]any
	if len(to.ParamsSchema) > 0 {
		if err := json.Unmarshal(to.ParamsSchema, &root); err != nil {
			return nil, fmt.Errorf("route: build expects: %s@%d params_schema is not a JSON object: %w",
				to.Name, to.Version, err)
		}
	}
	props, _ := root["properties"].(map[string]any)
	kept := make(map[string]any, len(props))
	for k, v := range props {
		if !drop[k] {
			kept[k] = v
		}
	}
	var required []string
	if list, ok := root["required"].([]any); ok {
		for _, r := range list {
			name, ok := r.(string)
			if !ok || drop[name] {
				continue
			}
			required = append(required, name)
		}
	}
	sort.Strings(required)

	needs := &schemaNeeds{required: required, properties: kept}
	raw, err := marshalNeedsSchema(required, kept)
	if err != nil {
		return nil, err
	}
	needs.raw = raw
	return needs, nil
}

// unionSchema 把多个下游的要求并成一份 schema（required 取并、properties 取并）。
//
// 返回 nil 表示并集为空（下游什么都不要）——上游此时不该看到一份空 schema，
// 那会被误读成「必须传空对象」。omitempty 会把它整个省掉。
func unionSchema(required []string, props map[string]any) (json.RawMessage, error) {
	if len(required) == 0 && len(props) == 0 {
		return nil, nil
	}
	// 去重后排序：同一个字段常被多个下游同时要求，而输出必须与 link 的插入顺序无关。
	seen := make(map[string]bool, len(required))
	uniq := make([]string, 0, len(required))
	for _, r := range required {
		if seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}
	sort.Strings(uniq)
	return marshalNeedsSchema(uniq, props)
}

// marshalNeedsSchema 生成 {"type":"object","properties":…,"required":[…]}。
//
// 固定带上 type:object：下游拿到的 params 永远是字典，显式写出来比让节点
// 各自猜「空 schema 意味着什么」更清楚。
func marshalNeedsSchema(required []string, props map[string]any) (json.RawMessage, error) {
	if len(required) == 0 && len(props) == 0 {
		return nil, nil
	}
	if required == nil {
		required = []string{}
	}
	if props == nil {
		props = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal expects schema: %w", err)
	}
	return raw, nil
}
