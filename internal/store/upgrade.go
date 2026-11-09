package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件是 P2.2 的**升级闸门**（设计 typed-pipeline §3.6）。
//
// 为什么闸门必须先于「引用语义」落地（capability.md §3）：一旦边可以按名字跟随
// latest，把一个会破坏下游的新版本推上去就等于**静默改线**——没人点过任何东西，
// 线上那条链却换了契约。闸门把这一步变成一次显式决定：
//
//	加可选入参        → 直接通过（只增不减，旧 link 仍然成立）
//	改入参类型 / 加必填 → 拒绝，并列出受影响的 link，除非调用方传 force
//
// 「受影响」的判据只有一个：这条 link **会解析到这一版**。
// 所以 `name` 型 link 会被拦（它跟随 latest），`name@version` 型不受影响
// （它钉死了旧版，新版本与它无关）。

// ErrUpgradeBreaking 是「新版本会破坏已有的 name 型 link」的哨兵错误。
//
// handler 用 errors.Is 判它 → HTTP 409 + 结构化 body；具体的「变了什么 + 哪些 link」
// 在 *UpgradeBreakingError 里（errors.As 取），供前端直接列出来让人确认。
var ErrUpgradeBreaking = errors.New("upgrade_breaking")

// UpgradeChangeKind 是「变了什么」的机器可读类别。
type UpgradeChangeKind string

const (
	// ChangeParamTypeChanged 某个入参的类型变了：上游照旧喂旧类型 → 派发时被 param_schema 剪掉。
	ChangeParamTypeChanged UpgradeChangeKind = "param_type_changed"
	// ChangeParamAddedRequired 新增了必填入参：已有的上游根本不知道要喂它。
	ChangeParamAddedRequired UpgradeChangeKind = "param_added_required"
)

// UpgradeChange 是一条破坏性变更（Kind + 给人看的说明）。
type UpgradeChange struct {
	Kind   UpgradeChangeKind `json:"kind"`
	Detail string            `json:"detail"`
}

// UpgradeLink 是一条**会受影响**的边：`FromRef --> ToRef`。
//
// 两端都用**引用**（不是 id）：用户看到的就是他当初配的那一行，
// 而不是一串需要在能力表里反查的 id。v4 没有端口（一次运行一个结果），
// 所以受影响判定只看「这条边会不会解析到新版」。
type UpgradeLink struct {
	FromRef string `json:"fromRef"`
	ToRef   string `json:"toRef"`
	// Reason 说明这条 link 为什么会被影响（会拿到新契约）。
	Reason string `json:"reason"`
}

// String 渲染成 `from_ref --> to_ref`（错误消息与日志用）。
func (l UpgradeLink) String() string {
	return fmt.Sprintf("%s --> %s", l.FromRef, l.ToRef)
}

// UpgradeImpact 是「把这个版本变成 latest」的影响面。
//
// Breaking=false 时 Changes/Links 都是**空数组**（不是 null，不变量 I9）：
// 前端可以直接遍历，不必为 nil 判空。
type UpgradeImpact struct {
	Capability string `json:"capability"`
	// FromVersion 是拿来比对的上一版 latest（0 = 之前没有任何版本，谈不上破坏）。
	FromVersion int             `json:"fromVersion"`
	ToVersion   int             `json:"toVersion"`
	Breaking    bool            `json:"breaking"`
	Changes     []UpgradeChange `json:"changes"`
	Links       []UpgradeLink   `json:"links"`
}

// UpgradeBreakingError 是闸门的结构化拒绝。
//
// 必须带**完整**影响面（不是只有一句错误文案）：用户在 UI 上要做的是
// 「看清楚哪条线会断，然后决定 force 还是改版本」，光有一句话做不到这个决定。
type UpgradeBreakingError struct {
	Impact *UpgradeImpact
}

// Error 渲染成一句可读的话 + 受影响 link 列表（日志 / 纯文本客户端够用）。
func (e *UpgradeBreakingError) Error() string {
	if e == nil || e.Impact == nil {
		return "upgrade rejected: breaking change"
	}
	parts := make([]string, 0, len(e.Impact.Changes)+len(e.Impact.Links)+1)
	parts = append(parts, fmt.Sprintf(
		"升级 %s@%d 会破坏已有的 name 型 link（%d 处契约变化，%d 条受影响 link）",
		e.Impact.Capability, e.Impact.ToVersion, len(e.Impact.Changes), len(e.Impact.Links)))
	for _, c := range e.Impact.Changes {
		parts = append(parts, string(c.Kind)+": "+c.Detail)
	}
	for _, l := range e.Impact.Links {
		parts = append(parts, l.String())
	}
	if len(e.Impact.Links) > 0 {
		parts = append(parts, "确认要改线请带 force=true 重试")
	}
	return strings.Join(parts, "; ")
}

// Unwrap 让 errors.Is(err, ErrUpgradeBreaking) 成立（handler 只认哨兵）。
func (e *UpgradeBreakingError) Unwrap() error { return ErrUpgradeBreaking }

// CheckUpgrade 算出「把 c 变成 latest」的影响面（设计 §3.6）。
//
// 三种输入都是合法调用：
//   - c 是第一个版本（没有上一版）→ 不算破坏，返回空影响面
//   - c 不是最新的（版本号 <= 现有 latest）→ 它不会成为 latest，返回空影响面
//   - c 比 latest 新 → 比对契约，算出变化与受影响的 link
//
// handler 在**写入前**调它把影响面回给前端（让用户先看清楚再决定）；
// CreateCapability 内部再调一次做真正的拦截（两处同一份实现，不可能分叉）。
func (d *DB) CheckUpgrade(ctx context.Context, namespaceID string, c *CapabilityVersion) (*UpgradeImpact, error) {
	return checkUpgradeOn(ctx, d, namespaceID, c)
}

// checkUpgradeOn 是 CheckUpgrade 的 ex 版。
//
// 事务内必须走它：闸门读的是「当前 latest」与「当前的边」，这两件事必须与
// 接下来的写入是同一个快照，否则闸门算的是一个已经过期的影响面。
func checkUpgradeOn(ctx context.Context, ex queryExec, namespaceID string, c *CapabilityVersion) (*UpgradeImpact, error) {
	if c == nil {
		return nil, fmt.Errorf("check upgrade: nil capability")
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	impact := &UpgradeImpact{
		Capability: c.Name,
		ToVersion:  c.Version,
		Changes:    []UpgradeChange{},
		Links:      []UpgradeLink{},
	}
	if c.Name == "" || c.Version <= 0 {
		return impact, nil
	}
	// v4 的契约只有 params（JSON Schema）：next 改线是原地更新、不产生新版本，
	// 也没有端口。闸门因此只比 params。
	prev, err := getCapabilityLatestOn(ctx, ex, namespaceID, c.Name)
	if errors.Is(err, ErrNotFound) {
		return impact, nil // 首个版本：没有可比对的契约，也就谈不上破坏
	}
	if err != nil {
		return nil, fmt.Errorf("check upgrade %s@%d: load previous: %w", c.Name, c.Version, err)
	}
	impact.FromVersion = prev.Version
	if c.Version <= prev.Version {
		return impact, nil // 不会成为 latest → 闸门不管（append-only 也不允许它覆盖）
	}

	impact.Changes = diffContracts(prev, c)
	if len(impact.Changes) == 0 {
		return impact, nil
	}

	links, err := upgradeAffectedLinksOn(ctx, ex, namespaceID, prev, c)
	if err != nil {
		return nil, err
	}
	impact.Links = links
	impact.Breaking = true
	return impact, nil
}

// diffContracts 比对上一版与新版契约，返回**破坏性**变更（§3.6 那张表）。
//
// 只报这三类：删 port / 改入参类型 / 新增必填。其余（加可选入参、加 port、
// 删一个可选入参、改 display 之类的装饰字段）都放行——闸门的价值在于
// 「拦得准」，把无害变更也拦下来只会训练用户无脑点 force。
func diffContracts(prev, next *CapabilityVersion) []UpgradeChange {
	changes := []UpgradeChange{}

	// 入参：类型变了 / 新增必填（v4 没有端口，删掉出口不再是破坏性变更）。
	prevProps, _ := schemaProperties(prev.Spec.Params)
	nextProps, nextRequired := schemaProperties(next.Spec.Params)

	// 稳定顺序：同一份输入永远报同样的顺序
	keys := make([]string, 0, len(prevProps))
	for k := range prevProps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		oldSpec, ok := prevProps[k].(map[string]any)
		if !ok {
			continue
		}
		newSpec, ok := nextProps[k].(map[string]any)
		if !ok {
			continue // 新版没有这个字段 = 删入参（不破坏旧上游：它照旧喂，字段被忽略）
		}
		if !sameDeclaredType(oldSpec["type"], newSpec["type"]) {
			changes = append(changes, UpgradeChange{
				Kind: ChangeParamTypeChanged,
				Detail: fmt.Sprintf("params.%s 的类型 %s → %s",
					k, describeDeclaredType(oldSpec["type"]), describeDeclaredType(newSpec["type"])),
			})
		}
	}

	newKeys := make([]string, 0, len(nextProps))
	for k := range nextProps {
		if _, ok := prevProps[k]; !ok {
			newKeys = append(newKeys, k)
		}
	}
	sort.Strings(newKeys)
	for _, k := range newKeys {
		if !nextRequired[k] {
			continue // 新加的可选入参：旧上游不喂也合法 → 放行
		}
		changes = append(changes, UpgradeChange{
			Kind:   ChangeParamAddedRequired,
			Detail: fmt.Sprintf("params.%s 是新增的**必填**入参（已接好的上游不知道要喂它）", k),
		})
	}
	return changes
}

// upgradeAffectedLinks 列出会被这套变更影响的 link（§3.6「列出受影响的 link」）。
//
// 两类受影响者，判据都落在「这条 link 会不会解析到新版」上：
//
//   - 改入参类型 / 新增必填：**右端**跟随 latest 的 link（`to_ref` 是裸名字）。
//     钉死的 to_ref 不受影响（它钉的就是旧版）。
//
// 同一条 link 两类都命中时合并成一行，reason 拼起来。
func (d *DB) upgradeAffectedLinks(ctx context.Context, namespaceID string, prev, next *CapabilityVersion) ([]UpgradeLink, error) {
	return upgradeAffectedLinksOn(ctx, d, namespaceID, prev, next)
}

// upgradeAffectedLinksOn 是 upgradeAffectedLinks 的 ex 版。
func upgradeAffectedLinksOn(ctx context.Context, ex queryExec, namespaceID string, prev, next *CapabilityVersion) ([]UpgradeLink, error) {
	// 入参契约真的变了才去查右端（v4 只有这一类破坏性变更）。
	prevProps, prevReq := schemaProperties(prev.Spec.Params)
	nextProps, nextReq := schemaProperties(next.Spec.Params)
	paramsChanged := false
	for k, spec := range prevProps {
		oldSpec, ok := spec.(map[string]any)
		if !ok {
			continue
		}
		if newSpec, ok := nextProps[k].(map[string]any); ok && !sameDeclaredType(oldSpec["type"], newSpec["type"]) {
			paramsChanged = true
		}
	}
	for k := range nextProps {
		if _, existed := prevProps[k]; !existed && nextReq[k] && !prevReq[k] {
			paramsChanged = true
		}
	}
	if !paramsChanged {
		return []UpgradeLink{}, nil
	}

	edges, err := listEdgesOn(ctx, ex, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("upgrade impact: load edges: %w", err)
	}
	out := make([]UpgradeLink, 0, 8)
	seen := make(map[string]int, len(edges))
	for _, e := range edges {
		reasons := make([]string, 0, 1)
		if paramsChanged {
			if toName, _, toPinned := SplitRef(e.ToRef); !toPinned && toName == next.Name {
				reasons = append(reasons, "跟随 latest，会拿到新版的入参契约")
			}
		}
		if len(reasons) == 0 {
			continue
		}
		key := e.FromRef + "\x00" + e.ToRef
		if i, ok := seen[key]; ok {
			out[i].Reason = out[i].Reason + "；" + strings.Join(reasons, "；")
			continue
		}
		seen[key] = len(out)
		out = append(out, UpgradeLink{
			FromRef: e.FromRef, ToRef: e.ToRef,
			Reason: strings.Join(reasons, "；"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromRef != out[j].FromRef {
			return out[i].FromRef < out[j].FromRef
		}
		return out[i].ToRef < out[j].ToRef
	})
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

// schemaProperties 取 params 的 properties 与 required（坏 schema 当作空）。
//
// 与 route 的派发期校验同一口径：读不懂的 schema 不拦任何东西
// （能力保存时已经把 schema 格式过一道；走到这里还坏只能是手改库）。
func schemaProperties(raw json.RawMessage) (map[string]any, map[string]bool) {
	props := map[string]any{}
	required := map[string]bool{}
	if len(raw) == 0 {
		return props, required
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return props, required
	}
	if p, ok := root["properties"].(map[string]any); ok {
		props = p
	}
	if arr, ok := root["required"].([]any); ok {
		for _, v := range arr {
			if name, ok := v.(string); ok {
				required[name] = true
			}
		}
	}
	return props, required
}

// sameDeclaredType 比对两处 schema 的 `type` 声明是否等价（缺省 == 缺省）。
func sameDeclaredType(a, b any) bool {
	ca, errA := canonicalJSONValue(a)
	cb, errB := canonicalJSONValue(b)
	if errA != nil || errB != nil {
		return true // 读不懂的类型声明不拦（与 validateValue 同口径）
	}
	return ca == cb
}

// canonicalJSONValue 把任意 JSON 值渲染成稳定字符串（键排序）。
func canonicalJSONValue(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	data, err := json.Marshal(sortJSON(v))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// describeDeclaredType 把 schema 的 type 声明渲染成人看的样子（缺省写「未声明」）。
func describeDeclaredType(v any) string {
	if v == nil {
		return "未声明"
	}
	if s, err := canonicalJSONValue(v); err == nil {
		return s
	}
	return fmt.Sprintf("%v", v)
}
