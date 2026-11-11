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
//   - `actual` 侧（库里旧行）没有旧 values 的 provenance，按「同一 target 位置 + **唯一**同名
//     能力」继承 declared 侧的敏感性，对齐不上就退到该源全部敏感 key 的并集（只允许**多**打码）；
//   - `PlannedSource.Spec` / `snap.Targets` **一律不原地改**：新建源落库的执行事实读的是
//     它们，必须继续带明文。
//
// 两个入口：`sourceTargetSensitivity` 推导敏感性；`maskedUnappliedTargets` 是行 12 差异副本的
// 唯一打码入口（declared/actual 两侧一起算，结构错位时 fail-closed）。

// sourceTargetSensitivity 推导每个源、每个 target 上「引用了 writeOnly values 的 param key」。
//
// 返回与 `pkg.SourcePaths`（= 渲染后 `RenderResult.Sources`，Render 按同一顺序 append）
// **下标对齐**的二维表：外层下标 = 源，内层下标 = target，值 = 敏感 param key 集合。
//
// 长度契约：raw targets 与渲染后 targets 同长（`interpolateTree` 保序保长），所以每个源的
// 内层长度就是渲染后的 target 数。该契约由唯一打码入口 `maskedUnappliedTargets` 复核：
// 一旦不等长（不可达的防御分支），两侧**全部 params** 打码（fail-closed，绝不放行明文）。
func sourceTargetSensitivity(pkg *Package, sch map[string]any) ([][]map[string]bool, error) {
	if pkg == nil {
		return nil, nil
	}
	out := make([][]map[string]bool, len(pkg.SourcePaths))
	for i, path := range pkg.SourcePaths {
		tree, err := decodeDoc(pkg.Files[path], path)
		if err != nil {
			return nil, err
		}
		out[i] = rawTargetSensitivity(tree, sch)
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

// refHitsWriteOnly 报告一条 values 点号路径是否会带出 writeOnly 值。
//
// 判据两段（命中任一即真）：
//  1. **路径上**任一经停节点（含终点）自身 `writeOnly: true`；
//  2. 引用**解析到的那个节点**的**子树**里声明了 writeOnly 叶（`creds` 里带 `token`）。
//     第 2 条是必需的：`{{values.creds}}` 解析成整个对象，回执里就会带出 `creds.token` 的
//     明文，而同一份回执的 `values.creds.token` 却是 `"***"`（MaskValues 打码了）。
//     设计口径：整对象 / 整数组引用一旦会带出 writeOnly 叶，就按「整值 `***`」——与 whole
//     interpolation（`{{values.x}}` 独占整个值）的既有规则一致。
//
// 子树检查**只对终点**做，绝不回溯祖先：`{{values.creds.user}}` 解析到的 string 不带
// token，必须保持明文（不是「祖先里有密文就把子孙全打码」）。
//
// 下钻规则与 `maskValue` 同源：对象按 `properties`、数组按下标 + `items`。
// 路径不在 schema 里 → 非敏感（schema 没说是密文，就不猜）。
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
	return subtreeWriteOnly(cur)
}

// subtreeWriteOnly 报告一个 schema 节点的**子树**里是否声明了任何 writeOnly 叶。
//
// 只走 `properties` 与 `items`——与 `maskValue` / `MaskValues` 的打码模型同源：
// `additionalProperties` / `prefixItems` 上的 writeOnly 本来就不会被 MaskValues 打码，
// 这里跟随同一口径（这是 writeOnly 打码模型的**既有边界**，不是本 issue 新增的取舍）。
func subtreeWriteOnly(sch map[string]any) bool {
	if sch == nil {
		return false
	}
	for _, child := range schema.Properties(sch) {
		childSchema, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if isWriteOnly(childSchema) || subtreeWriteOnly(childSchema) {
			return true
		}
	}
	if itemSchema, ok := sch["items"].(map[string]any); ok {
		if isWriteOnly(itemSchema) || subtreeWriteOnly(itemSchema) {
			return true
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

// descendSchema 按一段路径下钻 schema 节点。
//
// 优先级（非歧义优先）：**先 `properties`，再「纯数字下标 + `items`」**。
// 理由：同一个节点可能同时带 `properties` 与 `items`（`type: ["object","array"]`，或对象
// 节点上多写了一个 `items`）。真实值的形态由 `properties` 决定（`maskValue` 对 map 也是走
// properties），所以键名命中 properties 时必须走对象分支；只有 properties 里没有这个键、
// 且段是纯数字、且 `items` 存在时，才按数组元素下钻（`{{values.list.0}}`）。
// 反过来（items 抢在 properties 前）会让 `{{values.dual.pin}}` 被判「路径不在 schema」→
// 非敏感 → 明文漏进回执。
func descendSchema(sch map[string]any, seg string) (map[string]any, bool) {
	if sch == nil {
		return nil, false
	}
	if child, ok := schema.Properties(sch)[seg].(map[string]any); ok {
		return child, true
	}
	itemSchema, hasItems := sch["items"].(map[string]any)
	if !hasItems {
		return nil, false
	}
	if _, isIndex := toIntFromString(seg); !isIndex {
		return nil, false
	}
	return itemSchema, true
}

// maskedUnappliedTargets 是行 12 差异副本的**唯一**打码入口：返回 (declared, actual)。
//
// 长度契约：`keys` 与 `declared` 同长（raw targets 与渲染后 targets 同长，Render 的
// `interpolateTree` 保序保长）。两者不等长是**不可达的防御分支**，此时两侧每个 target 的
// 所有 params 值都换成 `MaskedValue`（fail-closed：契约 D3 只允许多打码，绝不许 fail-open）。
//
// 绝不原地改入参：`PlannedSource.Spec.Targets` / 库快照必须继续带明文（新建源落库读它）。
func maskedUnappliedTargets(declared, actual []sourcespec.Target, keys []map[string]bool) (declaredOut, actualOut []sourcespec.Target) {
	if len(keys) != len(declared) {
		return maskAllParams(declared), maskAllParams(actual)
	}
	return maskTargets(declared, keys), maskActualTargets(declared, actual, keys)
}

// maskTargets 是 declared 侧打码：**新建切片 + 新建 params map**，敏感 key 的整值换成
// `MaskedValue`，其它 key 原样（不做深拷贝）。调用方保证 `keys` 与 `targets` 同长。
func maskTargets(targets []sourcespec.Target, keys []map[string]bool) []sourcespec.Target {
	out := make([]sourcespec.Target, len(targets))
	for i, t := range targets {
		out[i] = maskTargetAt(t, keys[i])
	}
	return out
}

// maskActualTargets 是 actual 侧打码（库里旧行没有旧 values 的 provenance，只能继承）。
//
// 对齐算法（唯一的兜底方向是“多打码”，绝不允许漏）：
//  0. 该能力名（`refName` 归一：`a-cap@1` 与 `a-cap` 是同名字段）在 declared 侧出现 **>1** 次
//     → 位置对齐没有语义（同名 target 之间无从对应包内顺序），直接走 ③ 并集兜底；
//  1. 同下标且能力名相同 → 用该 declared 下标的敏感 key 集；
//  2. 否则取第一个「未被占用」的同能力名 declared target；
//  3. 都不成立 → 用该源 declared 侧**全部**敏感 key 的并集。
//
// 长度不等（不可达）→ 全部 params 打码（fail-closed，与 maskedUnappliedTargets 同口径）。
func maskActualTargets(declared, actual []sourcespec.Target, keys []map[string]bool) []sourcespec.Target {
	if len(keys) != len(declared) {
		return maskAllParams(actual)
	}
	union := unionSensitiveKeys(keys)
	used := make([]bool, len(declared))
	counts := declaredNameCounts(declared)
	out := make([]sourcespec.Target, len(actual))
	for i, t := range actual {
		keySet := inheritSensitiveKeys(declared, keys, used, counts, i, t.Capability, union)
		out[i] = maskTargetAt(t, keySet)
	}
	return out
}

// declaredNameCounts 统计 declared 侧每个归一能力名出现了几次。
func declaredNameCounts(targets []sourcespec.Target) map[string]int {
	out := make(map[string]int, len(targets))
	for _, t := range targets {
		out[refName(strings.TrimSpace(t.Capability))]++
	}
	return out
}

// inheritSensitiveKeys 是 D3 的对齐：同名不唯一 → 并集；否则同下标同名 → 第一个未占用的
// 同名 → 并集。
func inheritSensitiveKeys(declared []sourcespec.Target, keys []map[string]bool, used []bool,
	counts map[string]int, index int, capability string, union map[string]bool) map[string]bool {
	name := refName(strings.TrimSpace(capability))
	if counts[name] > 1 {
		// 同名 target 不唯一：库里顺序与包内顺序无从对应，按并集多打码（不占用 used）。
		return union
	}
	// ① 同下标 + 同名。
	if index < len(declared) && !used[index] &&
		refName(strings.TrimSpace(declared[index].Capability)) == name {
		used[index] = true
		return keys[index]
	}
	// ② 第一个未被占用的同名 declared target。
	for j := range declared {
		if used[j] || refName(strings.TrimSpace(declared[j].Capability)) != name {
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

// maskAllParams 把每个 target 的**所有** params 值换成 `MaskedValue`（fail-closed 分支用）。
func maskAllParams(targets []sourcespec.Target) []sourcespec.Target {
	out := make([]sourcespec.Target, len(targets))
	for i, t := range targets {
		out[i] = t
		if len(t.Params) == 0 {
			continue
		}
		params := make(map[string]any, len(t.Params))
		for k := range t.Params {
			params[k] = MaskedValue
		}
		out[i].Params = params
	}
	return out
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
