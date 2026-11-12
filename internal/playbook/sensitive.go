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
//   - `declared` 侧来自包体（精确到 target 下标 + param key；raw 形状推不出 key 时整组打码）；
//   - `actual` 侧（库里旧行）没有旧 values 的 provenance，按**同名能力的并集**继承 declared
//     侧敏感性；**没有任何同名能力可对齐**（出口改名 / 旧包删了该能力）就整组打码
//     ——fail-closed，方向只允许**多**打码；
//   - `PlannedSource.Spec` / `snap.Targets` **一律不原地改**：新建源落库的执行事实读的是
//     它们，必须继续带明文。
//
// actual 侧的同名对齐还有一条**按 key 的 fail-closed**（见 `inheritSensitivity` 的
// `knownParams`）：库里的旧 param key 只要不在当前包**同名能力的任一 target** 上出现，
// 就整值打码——覆盖“把 writeOnly 派生 param 改名/删除”的两轮安装场景（旧 key 不在任何
// 敏感集里，但也不能因此漏明文）。代价是**被删除的非敏感旧 param 也会被多打码**；方向仍然
// 只允许“多打码”，不按值匹配，也不往 target 里补键。

// targetSensitivity 是一个 target 上「哪些 param 值由 writeOnly values 派生」的推导结果。
//
// 两档（顺序即优先级）：
//   - `allParams`：raw 源文档在这里推不出具体 param key（整块 `targets` / 整块 `params` 写成
//     一条整值模板，key 要到渲染后才存在），只能按**整组打码**——只许多打码，不允许漏；
//   - `keys`：逐 key 精确命中（`allParams=false` 时生效）。
//
// 为什么必须有 allParams 这一档：`targets: '{{values.tlist}}'` / `params: '{{values.pobj}}'`
// 渲染后是合法的 target 列表 / params 对象（`decodeSourceWire` 只要求渲染后形状合法），
// 但 raw 侧没有 key 可对。若此处退回“空 key 集”，`unapplied[].declared/actual` 会把整块
// writeOnly 明文原样带进回执（实测：`params: '{{values.pobj}}'` 的 pin 明文进回执）。
type targetSensitivity struct {
	allParams bool
	keys      map[string]bool
	// knownParams 是「当前包里同名能力的全部 param key 并集」，只在 actual 侧继承时由
	// inheritSensitivity 填充（declared 侧为 nil）。旧行里出现、而当前包里已不存在的 key
	// 一律打码：改名/删除的 param 旧值没有旧 values provenance，不能靠敏感集归因。
	knownParams map[string]bool
}

// sourceTargetSensitivity 推导每个源、每个 target 上「引用了 writeOnly values 的 param」。
//
// 返回与 `pkg.SourcePaths`（= 渲染后 `RenderResult.Sources`，Render 按同一顺序 append）
// **下标对齐**的二维表：外层下标 = 源，内层下标 = target。
//
// 防御：raw `spec.targets` 形状不是列表、或长度与渲染后的 targets 不一致（结构错位）→
// 不按错位的下标套用精确 key，改走 `opaqueTargetSensitivity`（能证明引用了 writeOnly 就
// 整组打码，推不出引用则不打码——渲染阶段会自行拒绝非法形状）。
func sourceTargetSensitivity(pkg *Package, rendered []Source, sch map[string]any) ([][]targetSensitivity, error) {
	if pkg == nil {
		return nil, nil
	}
	out := make([][]targetSensitivity, len(pkg.SourcePaths))
	for i, path := range pkg.SourcePaths {
		tree, err := decodeDoc(pkg.Files[path], path)
		if err != nil {
			return nil, err
		}
		sets := rawTargetSensitivity(tree, sch)
		if i < len(rendered) && len(sets) != len(rendered[i].Spec.Targets) {
			sets = opaqueTargetSensitivity(tree, sch, len(rendered[i].Spec.Targets))
		}
		out[i] = sets
	}
	return out, nil
}

// opaqueTargetSensitivity 是 raw `targets` 形状推不出时的保守档。
//
// 只有「整块 targets 引用了 writeOnly values」才把每个渲染后的 target 标成整组打码；
// 引用的是普通 values（或根本没有引用）则保持不敏感——不做“形状不认识就全打码”的猜测，
// 否则 `targets: '{{values.tlist}}'` 这类普通模板会被无谓抹掉。
func opaqueTargetSensitivity(tree map[string]any, sch map[string]any, targets int) []targetSensitivity {
	out := make([]targetSensitivity, targets)
	spec, ok := tree["spec"].(map[string]any)
	if !ok || !valueReferencesWriteOnly(spec["targets"], sch) {
		return out
	}
	for i := range out {
		out[i] = targetSensitivity{allParams: true}
	}
	return out
}

// rawTargetSensitivity 从 raw（渲染前）源文档的 `spec.targets` 推每个 target 的敏感性。
//
// 按 YAML 列表下标保序：`interpolateTree` 不改数组长度、`decodeSourceWire` 按同一列表解码，
// 所以 raw 下标与渲染后 targets 下标一一对应。形状不是列表 → 返回空表（长度 0）。
//
// 单个 target 的两处“推不出 key”同样走 allParams：
//   - 列表项本身不是对象（`- '{{values.tobj}}'`）；
//   - `params` 不是对象（`params: '{{values.pobj}}'`）。
//
// 两种写法渲染后都可能合法，raw 侧只有「这一整块引用了 writeOnly」这一个事实可用。
func rawTargetSensitivity(tree map[string]any, sch map[string]any) []targetSensitivity {
	spec, ok := tree["spec"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := spec["targets"].([]any)
	if !ok {
		return nil
	}
	out := make([]targetSensitivity, len(list))
	for i, item := range list {
		keys := map[string]bool{}
		out[i] = targetSensitivity{keys: keys}
		target, ok := item.(map[string]any)
		if !ok {
			out[i] = targetSensitivity{allParams: valueReferencesWriteOnly(item, sch)}
			continue
		}
		params, present := target["params"]
		if !present || params == nil {
			continue
		}
		values, ok := params.(map[string]any)
		if !ok {
			out[i] = targetSensitivity{allParams: valueReferencesWriteOnly(params, sch)}
			continue
		}
		for key, value := range values {
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
// 下钻规则：对象按 `properties`（未命中回落 `additionalProperties` 的任意键）、
// 数组按下标 + `items` + `prefixItems`（两分支**并查**，因为引用可能同时被两边约束——
// 任一分支命中 writeOnly 就敏感，只许多打码）。
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
	curs := []map[string]any{sch}
	for _, seg := range splitValuePath(ref) {
		var next []map[string]any
		for _, cur := range curs {
			if isWriteOnly(cur) {
				return true
			}
			next = append(next, descendSchemaCandidates(cur, seg)...)
		}
		if len(next) == 0 {
			return false
		}
		curs = next
	}
	for _, cur := range curs {
		if subtreeHasWriteOnly(cur) {
			return true
		}
	}
	return false
}

// subtreeHasWriteOnly 报告一个 schema 节点**自身或其任意后代**是否声明了 `writeOnly: true`。
//
// 只在**引用最终解析到的节点**上调用（见 `refHitsWriteOnly`）：
//   - 节点自身 writeOnly → true；
//   - `properties` 下任一子节点递归为真 → true（`{{values.auth}}` 因 `auth.properties.token`）；
//   - `items` 递归为真 → true（`{{values.credList}}` 因 `credList.items.properties.token`）；
//   - `prefixItems` 任一元素递归为真 → true（元组写法的同一条规则）；
//   - `additionalProperties`（对象 schema 形态）递归为真 → true（任意键都可能是密文，
//     与 `maskValue` 的兜底打码口径对齐）。
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
	if addl, ok := additionalPropertiesSchema(sch); ok && subtreeHasWriteOnly(addl) {
		return true
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

// descendSchemaCandidates 返回「一段路径」在一个 schema 节点上可下钻到的**全部候选**。
//
// 为什么是多个候选（而不是一个）：数组元素可能同时被 `prefixItems[i]` 与 `items` 约束
// （2020-12：`prefixItems` 管前 N 个，`items` 管其余；`maskValue` 按位置优先 `prefixItems[i]`
// 且只在它没有 writeOnly 时回落 `items`）。若只信其中一支，`prefixItems: [{}]` + `items.writeOnly`
// 这类包会出现「回执 `values` 里是 `***`、`unapplied` 里同一值是明文」的裂缝。
// 于是两个分支都作为候选并查：任一分支命中 writeOnly 即敏感（只许多打码）。
//
// 下钻优先级：**先 `properties`，未命中回落 `additionalProperties`（任意键），
// 再「纯数字下标 + `prefixItems`/`items`」**。
// 同一个节点可能同时带 `properties` 与 `items`（`type: ["object","array"]`，或对象节点上
// 多写了一个 `items`）。真实值的形态由 `properties` 决定（`maskValue` 对 map 也走 properties），
// 所以键名命中 properties 时必须走对象分支；只有 properties 里没有这个键、**且段是纯数字**、
// 且 `items`/`prefixItems` 存在时，才按数组元素下钻（`{{values.list.0}}`）。
// 反过来（items 抢在 properties 前）会让 `{{values.dual.pin}}` 被判「路径不在 schema」→
// 非敏感 → 明文漏进回执 `unapplied[]`。
//
// 其它规则：
//   - 带 `items` 的数组节点上，非下标段没有对象字段可下钻（既有口径，`additionalProperties`
//     也没声明时返回空）；
//   - `prefixItems[idx]` 不是对象 schema（布尔 `true` / `null`）→ 不产生候选，但**不阻断**
//     `items` 候选（否则相对「下标段一律走 items」的旧行为是 fail-open 回归）。
func descendSchemaCandidates(sch map[string]any, seg string) []map[string]any {
	if sch == nil {
		return nil
	}
	if child, ok := schema.Properties(sch)[seg].(map[string]any); ok {
		return []map[string]any{child}
	}
	var out []map[string]any
	// `properties` 未命中：声明了对象形态的 `additionalProperties` 就按「任意键」回落
	// （`values` 里键名由安装方自己定，schema 只能靠 additionalProperties 说它是密文）。
	if addl, ok := additionalPropertiesSchema(sch); ok {
		out = append(out, addl)
	}
	idx, isIndex := toIntFromString(seg)
	if !isIndex {
		return out
	}
	if prefix, ok := sch["prefixItems"].([]any); ok && idx < len(prefix) {
		if node, ok := prefix[idx].(map[string]any); ok {
			out = append(out, node)
		}
	}
	if items, ok := sch["items"].(map[string]any); ok {
		out = append(out, items)
	}
	return out
}

// maskTargets 是 declared 侧打码：**新建切片 + 新建 params map**，敏感 param 的整值换成
// `MaskedValue`，其它 key 原样（不做深拷贝）。
//
// 绝不原地改入参：`PlannedSource.Spec.Targets` 必须继续带明文（新建源落库读它）。
func maskTargets(targets []sourcespec.Target, sets []targetSensitivity) []sourcespec.Target {
	union := unionSensitiveKeys(sets)
	out := make([]sourcespec.Target, len(targets))
	for i, t := range targets {
		set := union
		if i < len(sets) {
			set = sets[i]
		}
		out[i] = maskTargetAt(t, set)
	}
	return out
}

// maskActualTargets 是 actual 侧打码（库里旧行没有旧 values 的 provenance，只能继承）。
//
// 对齐算法见 `inheritSensitivity`（同名能力的并集）；兜底方向只有“多打码”：没有任何同名能力
// 可对齐（含包里把出口全删了、`len(declared)==0`）→ 该 target 整组打码。
func maskActualTargets(declared, actual []sourcespec.Target, sets []targetSensitivity) []sourcespec.Target {
	out := make([]sourcespec.Target, len(actual))
	for i, t := range actual {
		out[i] = maskTargetAt(t, inheritSensitivity(declared, sets, t.Capability))
	}
	return out
}

// inheritSensitivity 是 D3 的 actual 侧对齐：取**同名能力的全部 declared target 的并集**。
//
// 为什么不用“同下标优先”：`sourcespec.SameTargets` 明确把 target 顺序当**非语义**
// （config.go：“顺序不同但内容相同 = 等价”，扇出顺序不是语义），所以库里顺序与包里合法地不同。
// 按位置绑定会把 A target 的敏感集套到 B target 上——B 里其他 key 名的 writeOnly 明文就漏了
// （实测：两条同名 `a-review`，一条 `token`、一条 `secret`，库顺序相反 → 两侧明文进回执）。
// 取**同名并集**与顺序无关，且方向只会多打码。
//
// 名字对不上（出口改名 / 旧包已删该能力 / 包里一个 target 都没有）→ 没有任何 key 名可继承，
// 而库里旧行可能正是上一版由 writeOnly values 派生的值。此时**整组打码**（`allParams`），
// 与「`len(declared)==0`」同口径：绝不回退到“空集 = 零打码”。
//
// 名字对得上时还返回 `knownParams`（同名 target 的全部 param key 并集）：旧行里出现、而当前
// 包里已不存在的 key 也一律打码（`maskTargetAt`）。这条覆盖“同名能力内把 writeOnly 派生
// param 改名/删除”的两轮安装——旧 key 不在任何敏感集里，但旧值仍可能来自 writeOnly。
//
// 历史口径是「退到该源全部敏感集的并集」，一旦新包里该源已无任何 writeOnly 引用（并集为空），
// `maskTargetAt` 因 `!allParams && len(keys)==0` 原样返回 ⇒ actual 整侧零打码（review major，
// 明文落库并可被 GET/history 读回）。回归见
// `TestPlanUnappliedActualMasksWholeGroupWhenDeclaredRenamesExport`。
func inheritSensitivity(declared []sourcespec.Target, sets []targetSensitivity,
	capability string) targetSensitivity {
	name := refName(strings.TrimSpace(capability))
	out := targetSensitivity{keys: map[string]bool{}, knownParams: map[string]bool{}}
	matched := false
	for j := range declared {
		if j >= len(sets) || refName(strings.TrimSpace(declared[j].Capability)) != name {
			continue
		}
		matched = true
		mergeSensitivity(&out, sets[j])
		for key := range declared[j].Params {
			out.knownParams[key] = true
		}
	}
	if !matched {
		return targetSensitivity{allParams: true}
	}
	return out
}

// mergeSensitivity 把 src 的敏感性并入 dst（allParams 传染，key 取并）。
func mergeSensitivity(dst *targetSensitivity, src targetSensitivity) {
	if src.allParams {
		dst.allParams = true
	}
	if dst.keys == nil && len(src.keys) > 0 {
		dst.keys = map[string]bool{}
	}
	for k, v := range src.keys {
		if v {
			dst.keys[k] = true
		}
	}
}

// maskTargetAt 打码一个 target 的 params（无 params / 无敏感项 → 原值原样返回）。
func maskTargetAt(t sourcespec.Target, set targetSensitivity) sourcespec.Target {
	if len(t.Params) == 0 || (!set.allParams && len(set.keys) == 0) {
		return t
	}
	params := make(map[string]any, len(t.Params))
	masked := false
	for k, v := range t.Params {
		// knownParams 非 nil 只出现在 actual 侧：旧 key 在当前包同名能力里已不存在
		// （改名/删除）→ 没有 provenance 可归因，整值打码（多打码方向安全）。
		if set.allParams || set.keys[k] || (set.knownParams != nil && !set.knownParams[k]) {
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

// unionSensitiveKeys 求一个源 declared 侧全部敏感集的并集（allParams 传染，key 取并）。
func unionSensitiveKeys(sets []targetSensitivity) targetSensitivity {
	out := targetSensitivity{keys: map[string]bool{}}
	for _, set := range sets {
		mergeSensitivity(&out, set)
	}
	return out
}
