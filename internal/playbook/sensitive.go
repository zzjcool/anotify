package playbook

import (
	"strings"

	"github.com/zzjcool/anotify/internal/schema"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 回执 `unapplied[].declared/actual` 的 writeOnly 打码（issue #356）。
//
// 为什么不能「把 values 打码后再 Render 一遍」算差异：`MaskValues` 会把 writeOnly 的
// 值换成字符串 `"***"`，于是
//
//	① `Render` 第 ② 步用 `spec.values` 校验 values，`{type: integer, writeOnly: true}`
//	   的值变成 `"***"` → 必然 `values_invalid`；
//	② 即便绕过校验，`{{values.list.0}}` 这类**下标引用**在被打码的容器（`"***"`）上
//	   会 `values_missing`。
//
// 所以敏感性不靠二次渲染，而是走**同一条引用语法**（`tokenizeTemplate` + `values.` 点号路径）
// 在 raw（渲染前）源文档上做逐叶推导：一个 param 值只要（递归地）引用了 schema 里
// `writeOnly: true` 的 values 路径，该 param 值就**整体**打码成 `MaskedValue`——整对象 /
// 整数组 / 整数 / 布尔 / 混合串（`secret={{values.pin}}`）一律整值替换，不是子串替换。
//
// 打码只作用于回执的展示副本：
//   - `declared` 侧来自包体（精确到 target 下标 + param key）；
//   - `actual` 侧（库里旧行）没有旧 values 的 provenance，按「同一 target 位置 + 同名能力」
//     继承 declared 侧的敏感性，对齐不上就退到该源全部敏感 key 的并集（只允许**多**打码）；
//   - `PlannedSource.Spec` / `snap.Targets` **一律不原地改**：新建源落库的执行事实读的是
//     它们，必须继续带明文。

// sourceTargetSensitivity 推导每个源、每个 target 上「引用了 writeOnly values 的 param key」。
//
// 返回与 `pkg.SourcePaths`（= 渲染后 `RenderResult.Sources`，Render 按同一顺序 append）
// **下标对齐**的二维表：外层下标 = 源，内层下标 = target，值 = 敏感 param key 集合。
//
// 防御：raw `spec.targets` 形状不是列表、或长度与渲染后的 targets 不一致（结构错位）→
// 该源退回“长度 0 的精确表”，绝不按错位的下标套用敏感集；`actual` 侧的并集兜底会接住它
// （安全性由“多打码”方向保证）。
func sourceTargetSensitivity(pkg *Package, rendered []Source, sch map[string]any) ([][]map[string]bool, error) {
	if pkg == nil {
		return nil, nil
	}
	out := make([][]map[string]bool, len(pkg.SourcePaths))
	for i, path := range pkg.SourcePaths {
		tree, err := decodeDoc(pkg.Files[path], path)
		if err != nil {
			return nil, err
		}
		keys := rawTargetSensitivity(tree, sch)
		if i < len(rendered) && len(keys) != len(rendered[i].Spec.Targets) {
			keys = nil
		}
		out[i] = keys
	}
	return out, nil
}

// rawTargetSensitivity 从 raw（渲染前）源文档的 `spec.targets` 推每个 target 的敏感 param key。
//
// 按 YAML 列表下标保序：`interpolateTree` 不改数组长度、`decodeSourceWire` 按同一列表解码，
// 所以 raw 下标与渲染后 targets 下标一一对应。形状不是列表 → 返回空表（长度 0）。
func rawTargetSensitivity(tree map[string]any, sch map[string]any) []map[string]bool {
	spec, ok := tree["spec"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := spec["targets"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]bool, len(list))
	for i, item := range list {
		keys := map[string]bool{}
		out[i] = keys
		target, ok := item.(map[string]any)
		if !ok {
			continue
		}
		params, ok := target["params"].(map[string]any)
		if !ok {
			continue
		}
		for key, value := range params {
			if valueReferencesWriteOnly(value, sch) {
				keys[key] = true
			}
		}
	}
	return out
}

// valueReferencesWriteOnly 报告一个 param 值是否（递归地）引用了 writeOnly 的 values 路径。
//
// 只有字符串叶子可能含引用；其它标量字面量（数字 / 布尔 / null）永远非敏感——
// 这样 `fixedNumber: 77` / `fixedBoolean: true` 不会被“按值匹配”误伤。
func valueReferencesWriteOnly(v any, sch map[string]any) bool {
	switch val := v.(type) {
	case string:
		for _, tk := range tokenizeTemplate(val) {
			if tk.reference == "" {
				continue
			}
			if refHitsWriteOnly(tk.reference, sch) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, child := range val {
			if valueReferencesWriteOnly(child, sch) {
				return true
			}
		}
		return false
	case []any:
		for _, child := range val {
			if valueReferencesWriteOnly(child, sch) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// refHitsWriteOnly 报告一条 values 点号引用是否会把 writeOnly 明文带进回执。
//
// 两段判定：
//  1. **下钻途中**只看每一步节点**自身**的 `writeOnly`（与 `maskValue` 的递归口径一致）；
//  2. **走完整条路径后**，对**最终解析节点**再判整棵子树（`subtreeHasWriteOnly`）——
//     引用一个「里面装着 writeOnly 叶的容器」时，整对象 / 整数组 / 整个数组元素插值同样
//     会把明文带进 `unapplied[].declared/actual`，必须整值打码。
//
// 中间节点**不做**子树判定：root 的子树几乎必然含 writeOnly，那样 `{{values.project}}`
// 这种普通引用也会被打码，偏离 mock 语义（对齐前端 mock 的 `pbPolicySecret(pbReferencePolicy(...))`
// ——只在解析节点上判子树）。
//
// 下钻规则与 `maskValue` 同源：对象按 `properties`、数组按下标 + `items`。
// 路径不在 schema 里 → 非敏感（schema 没说是密文，就不猜）。空路径不是引用，不会走到这里。
//
// 逐条语义（与 mock 对齐）：
//   - `{{values.auth}}`（对象祖先，`properties.token.writeOnly`）→ true
//   - `{{values.credList}}`（数组祖先，`items` 子树里有 writeOnly）→ true
//   - `{{values.credList.0}}`（解析到 `items` 节点，其子树含 writeOnly）→ true
//   - `{{values.credList.0.token}}`（解析到 writeOnly 叶）→ true（下钻途中就命中）
//   - `{{values.auth.label}}`（label 本身是普通叶，auth 下的 writeOnly 兄弟**不传染**）→ false
//   - `{{values.project}}` / 不存在的路径 → false
//   - 混合串 `secret={{values.auth}}` 的**敏感性函数**→ true（渲染阶段本来会
//     `render_type_mismatch`，但推导必须保守为真：先打码，别赌渲染一定失败）
func refHitsWriteOnly(ref string, sch map[string]any) bool {
	cur := sch
	if isWriteOnly(cur) {
		return true
	}
	for _, seg := range splitValuePath(ref) {
		next, ok := descendSchema(cur, seg)
		if !ok {
			return false
		}
		if isWriteOnly(next) {
			return true
		}
		cur = next
	}
	return subtreeHasWriteOnly(cur)
}

// subtreeHasWriteOnly 报告一个 schema 节点**自身或其任意后代**是否声明了 `writeOnly: true`。
//
// 只在**引用最终解析到的节点**上调用（见 `refHitsWriteOnly`）：
//   - 节点自身 writeOnly → true；
//   - `properties` 下任一子节点递归为真 → true（`{{values.auth}}` 因 `auth.properties.token`）；
//   - `items` 递归为真 → true（`{{values.credList}}` 因 `credList.items.properties.token`）；
//   - `prefixItems` 任一元素递归为真 → true（元组写法的同一条规则）。
//
// 不做关键词猜测（`format` / 字段名一律不看）：只有 `writeOnly` 是密文的唯一真相。
func subtreeHasWriteOnly(sch map[string]any) bool {
	if sch == nil {
		return false
	}
	if isWriteOnly(sch) {
		return true
	}
	for _, child := range schema.Properties(sch) {
		node, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if subtreeHasWriteOnly(node) {
			return true
		}
	}
	if items, ok := sch["items"].(map[string]any); ok && subtreeHasWriteOnly(items) {
		return true
	}
	if prefix, ok := sch["prefixItems"].([]any); ok {
		for _, child := range prefix {
			node, ok := child.(map[string]any)
			if !ok {
				continue
			}
			if subtreeHasWriteOnly(node) {
				return true
			}
		}
	}
	return false
}

// splitValuePath 把点号路径拆成段（空路径 → 空切片）。
func splitValuePath(ref string) []string {
	if ref == "" {
		return nil
	}
	return strings.Split(ref, ".")
}

// descendSchema 按一段路径下钻 schema 节点（数组按下标 + `items`，对象按 `properties`）。
func descendSchema(sch map[string]any, seg string) (map[string]any, bool) {
	if sch == nil {
		return nil, false
	}
	if itemSchema, ok := sch["items"].(map[string]any); ok {
		// 数组：段必须是下标。`{{values.list.0}}` 与 `{{values.list}}` 两种写法都落在 items 上。
		if _, isIndex := toIntFromString(seg); !isIndex {
			return nil, false
		}
		return itemSchema, true
	}
	props := schema.Properties(sch)
	child, ok := props[seg].(map[string]any)
	if !ok {
		return nil, false
	}
	return child, true
}

// maskTargets 是 declared 侧打码：**新建切片 + 新建 params map**，敏感 key 的整值换成
// `MaskedValue`，其它 key 原样（不做深拷贝）。
//
// 绝不原地改入参：`PlannedSource.Spec.Targets` 必须继续带明文（新建源落库读它）。
func maskTargets(targets []sourcespec.Target, keys []map[string]bool) []sourcespec.Target {
	union := unionSensitiveKeys(keys)
	out := make([]sourcespec.Target, len(targets))
	for i, t := range targets {
		out[i] = maskTargetAt(t, keysForIndex(keys, i, union))
	}
	return out
}

// maskActualTargets 是 actual 侧打码（库里旧行没有旧 values 的 provenance，只能继承）。
//
// 对齐算法（唯一的兜底方向是“多打码”，绝不允许漏）：
//  1. 同下标且能力名相同（`refName` 归一：`a-cap@1` 与 `a-cap` 是同名字段）→ 用该 declared
//     下标的敏感 key 集；
//  2. 否则取第一个「未被占用」的同能力名 declared target；
//  3. 都不成立 → 用该源 declared 侧**全部**敏感 key 的并集。
func maskActualTargets(declared, actual []sourcespec.Target, keys []map[string]bool) []sourcespec.Target {
	union := unionSensitiveKeys(keys)
	used := make([]bool, len(declared))
	out := make([]sourcespec.Target, len(actual))
	for i, t := range actual {
		keySet := inheritSensitiveKeys(declared, keys, used, i, t.Capability, union)
		out[i] = maskTargetAt(t, keySet)
	}
	return out
}

// inheritSensitiveKeys 是 D3 的三步对齐：同下标同名 → 第一个未占用的同名 → 并集。
func inheritSensitiveKeys(declared []sourcespec.Target, keys []map[string]bool, used []bool,
	index int, capability string, union map[string]bool) map[string]bool {
	name := refName(strings.TrimSpace(capability))
	// ① 同下标 + 同名。
	if index < len(declared) && index < len(keys) && !used[index] &&
		refName(strings.TrimSpace(declared[index].Capability)) == name {
		used[index] = true
		return keys[index]
	}
	// ② 第一个未被占用的同名 declared target。
	for j := range declared {
		if used[j] || j >= len(keys) {
			continue
		}
		if refName(strings.TrimSpace(declared[j].Capability)) != name {
			continue
		}
		used[j] = true
		return keys[j]
	}
	// ③ 并集（保守多打码）。
	return union
}

// maskTargetAt 打码一个 target 的 params（无 params / 无敏感 key → 原值原样返回）。
func maskTargetAt(t sourcespec.Target, keySet map[string]bool) sourcespec.Target {
	if len(t.Params) == 0 || len(keySet) == 0 {
		return t
	}
	params := make(map[string]any, len(t.Params))
	masked := false
	for k, v := range t.Params {
		if keySet[k] {
			params[k] = MaskedValue
			masked = true
			continue
		}
		params[k] = v
	}
	if !masked {
		return t
	}
	t.Params = params
	return t
}

// keysForIndex 取某 target 下标的敏感 key 集；下标越界（结构错位）→ 并集兜底。
func keysForIndex(keys []map[string]bool, index int, union map[string]bool) map[string]bool {
	if index < len(keys) {
		return keys[index]
	}
	return union
}

// unionSensitiveKeys 求一个源 declared 侧全部敏感 param key 的并集（返回新 map）。
func unionSensitiveKeys(keys []map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range keys {
		for k, v := range set {
			if v {
				out[k] = true
			}
		}
	}
	return out
}
