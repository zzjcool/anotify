package playbook

import (
	"strings"

	"github.com/zzjcool/anotify/internal/schema"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 回执 `unapplied[].declared/actual` 的 writeOnly 打码（issue #356）。
//
// 为什么不能「把 values 打码后再 Render 一遍」算差异：`MaskValues` 会把 writeOnly 的 值换成字符串 `"***"`，于是
//
//	① `Render` 第 ② 步用 `spec.values` 校验 values，`{type: integer, writeOnly: true}`
//	   的值变成 `"***"` → 必然 `values_invalid`；
//	② 即便绕过校验，`{{values.list.0}}` 这类**下标引用**在被打码的容器（`"***"`）上
//	   会 `values_missing`。
//
// 所以敏感性不靠二次渲染，而是走**同一条引用语法**（`tokenizeTemplate` + `values.` 点号路径） 在 raw（渲染前）源文档上做逐叶推导：一个 param 值只要（递归地）引用了 schema 里
// `writeOnly: true` 的 values 路径，该 param 值就**整体**打码成 `MaskedValue`——整对象 /
// 整数组 / 整数 / 布尔 / 混合串（`secret={{values.pin}}`）一律整值替换，不是子串替换。
//
// 打码只作用于回执的展示副本： - `declared` 侧来自包体（精确到 target 下标 + param key；raw 形状推不出 key 时整组打码）；
//   - `actual` 侧（库里旧行）没有旧 values 的 provenance，按**同名能力的并集**继承 declared
// 侧敏感性；**没有任何同名能力可对齐**（出口改名 / 旧包删了该能力）就整组打码 ——fail-closed，方向只允许**多**打码；
// - `PlannedSource.Spec` / `snap.Targets` **一律不原地改**：新建源落库的执行事实读的是 它们，必须继续带明文。
//
// actual 侧的同名对齐还有一条**按 key 的 fail-closed**（见 `inheritSensitivity` 的 `knownParams`）：库里的旧 param key 只要不在当前包**同名能力的任一 target** 上出现，
// 就整值打码——覆盖“把 writeOnly 派生 param 改名/删除”的两轮安装场景（旧 key 不在任何
// 敏感集里，但也不能因此漏明文）。代价是**被删除的非敏感旧 param 也会被多打码**；方向仍然 只允许“多打码”，不按值匹配，也不往 target 里补键。

// targetSensitivity 是一个 target 上「哪些 param 值由 writeOnly values 派生」的推导结果。
//
// 两档（顺序即优先级）： - `allParams`：raw 源文档在这里推不出具体 param key（整块 `targets` / 整块 `params` 写成
// 一条整值模板，key 要到渲染后才存在），只能按**整组打码**——只许多打码，不允许漏； - `keys`：逐 key 精确命中（`allParams=false` 时生效）。
//
// 为什么必须有 allParams 这一档：`targets: '{{values.tlist}}'` / `params: '{{values.pobj}}'`
// 渲染后是合法的 target 列表 / params 对象（`decodeSourceWire` 只要求渲染后形状合法），
// 但 raw 侧没有 key 可对。若此处退回“空 key 集”，`unapplied[].declared/actual` 会把整块
// writeOnly 明文原样带进回执（实测：`params: '{{values.pobj}}'` 的 pin 明文进回执）。
type targetSensitivity struct {
	allParams bool
	keys      map[string]bool
	// knownParams 是「当前包里同名能力的全部 param key 并集」，只在 actual 侧继承时由 inheritSensitivity 填充（declared 侧为 nil）。旧行里出现、而当前包里已不存在的 key
	// 一律打码：改名/删除的 param 旧值没有旧 values provenance，不能靠敏感集归因。
	knownParams map[string]bool
}

// sourceTargetSensitivity 推导每个源、每个 target 上「引用了 writeOnly values 的 param」。
//
// 返回与 `pkg.SourcePaths`（= 渲染后 `RenderResult.Sources`，Render 按同一顺序 append） **下标对齐**的二维表：外层下标 = 源，内层下标 = target。
//
// 防御：raw `spec.targets` 形状不是列表、或长度与渲染后的 targets 不一致（结构错位）→
// 不按错位的下标套用精确 key，改走 `opaqueTargetSensitivity`（能证明引用了 writeOnly 就 整组打码，推不出引用则不打码——渲染阶段会自行拒绝非法形状）。
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
// 引用的是普通 values（或根本没有引用）则保持不敏感——不做“形状不认识就全打码”的猜测， 否则 `targets: '{{values.tlist}}'` 这类普通模板会被无谓抹掉。
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
// 按 YAML 列表下标保序：`interpolateTree` 不改数组长度、`decodeSourceWire` 按同一列表解码， 所以 raw 下标与渲染后 targets 下标一一对应。形状不是列表 → 返回空表（长度 0）。
//
// 单个 target 的两处“推不出 key”同样走 allParams： - 列表项本身不是对象（`- '{{values.tobj}}'`）；
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
// 只有字符串叶子可能含引用；其它标量字面量（数字 / 布尔 / null）永远非敏感—— 这样 `fixedNumber: 77` / `fixedBoolean: true` 不会被“按值匹配”误伤。
func valueReferencesWriteOnly(v any, sch map[string]any) bool {
	switch val := v.(type) {
	case string:
		for _, tk := range tokenizeTemplate(val) {
			// 只看 values 引用：release 引用不是 values 路径，带不进 writeOnly 值。
			if tk.root != "values" || tk.reference == "" {
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

// ---------- fail-closed：无法证明的关键字（GAP-1） ----------

// maskUnprovableKeywords 是「节点上一旦出现就无法证明它不含 writeOnly」的关键字表（值为理由）。
//
// 为什么需要这张表（GAP-1 的根因）：`internal/schema` 的受控子集明确「不认识的关键字一律 忽略（不报错）」（该包注释 + 不变量 I6：中枢不做求值器）。打码只看**内联结构关键字**
// （properties / items / prefixItems / additionalProperties），于是「一个我看不懂、但要紧的
// 关键字」会被当成「这里没有 writeOnly」——`$ref` 后面的 writeOnly 就这样明文进了回执
// `values`（回归用例见 mask_shapes_test.go 的 TestMaskUnprovableKeywordsMaskWholeValue）。
//
// 判据（刻意**不求值**）：节点上出现下面任何一个关键字，就按「这里可能藏着 writeOnly」 处理，整值打码成 `MaskedValue`。这个取舍是产品口径，写在这里而不是只写在测试里：
//   - 多打码是**可用性**问题（`$ref` 指向的定义其实没有 writeOnly 也照样打码），漏明文是 **安全**问题；方向只允许前者。
//   - 「证明这个关键字不含 writeOnly」需要一个求值器（解引用、跑 if/then、按键名匹配 patternProperties……），那正是 I6 禁止中枢做的事；证明不了，就按不安全处理。
//   - 所以等价类语义是「出现需要求值的关键字 ⇒ 整值打码」，**不是**「解引用后看被引用节点 有没有 writeOnly」。后者会把中枢变成求值器，远程 `$ref` 还会引入外部请求面。
//     契约口径就是下面这张理由表与 `maskProvableKeywords` 的 allowlist 语义。
//
// 这张表是**理由表**（审查者看「为什么这个关键字危险」），判据的默认面在 `maskProvableKeywords` 那边：不在豁免表里的关键字——包括本表没枚举的、未来的新 draft 关键字——同样算无法证明。
// 表里每一条都由 TestMaskUnprovableKeywordTableMatchesPredicate 与判据对账。
var maskUnprovableKeywords = map[string]string{
	"$ref":                  "引用：writeOnly 可能写在被引用节点里，不解引用就看不见",
	"$dynamicRef":           "动态引用：目标要到求值期按动态作用域才确定",
	"$recursiveRef":         "递归引用：同类求值关键字（draft 2019-09 写法）",
	"allOf":                 "组合：任一子 schema 都可能带 writeOnly，本仓子集不认识该关键字",
	"anyOf":                 "组合：同上",
	"oneOf":                 "组合：同上",
	"not":                   "取反：子 schema 里可能有 writeOnly，而「是否被否定掉」得求值才知道",
	"if":                    "条件：分支子 schema 可能带 writeOnly",
	"then":                  "条件：同上（不写 if 时它本身不生效，但仍算无法证明）",
	"else":                  "条件：同上",
	"patternProperties":     "按键名模式的子 schema：walker 只读 properties / additionalProperties，模式里的 writeOnly 看不见",
	"propertyNames":         "键名 schema：本仓子集不认识，无法排除它对值的作用",
	"additionalItems":       "draft-07 元组尾巴：承载子 schema，walker 不读",
	"unevaluatedProperties": "对「未被其它关键字求值过的属性」生效：判据本身就是求值结果",
	"unevaluatedItems":      "同上（数组侧）",
	"dependentSchemas":      "依赖触发的子 schema：生效与否取决于值里出现了哪些键",
	"dependencies":          "draft-07 的依赖写法（schema 形态承载子 schema）",
	"contains":              "元素子 schema：walker 不读，元素里的 writeOnly 看不见",
	"minContains":           "contains 的计数门槛：与 contains 一起才生效，单独看无法判定",
	"maxContains":           "同上",
}

// maskProvableKeywords 是「能证明不含 writeOnly」的关键字表：要么 walker 自己会读它
// （properties / items / prefixItems / additionalProperties / writeOnly），要么它只是注解 或纯值断言（不承载子 schema ⇒ 藏不了 writeOnly）。
//
// 为什么要有这张**白名单**（而不是只列危险表）：GAP-1 的真正成因是「不认识的关键字被当成
// 安全」。危险表只挡得住已经想到的关键字，挡不住下一个 draft 新加的求值关键字；白名单让 「没见过」默认落到危险侧——不认识 ⇒ 无法证明 ⇒ 整值打码。
//
// 名字进这张表**不等于免检**：同一个关键字在不同 draft 里有不同形态（`items` 在 draft-07 可以 是 schema 列表），walker 只读它认识的那个形态，所以判据还要过 `maskKeywordShapeProvable`。
var maskProvableKeywords = map[string]bool{
	// walker 会读的结构关键字
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "prefixItems": true, "writeOnly": true, "readOnly": true,
	// 纯值断言：只约束标量/长度/计数，不承载子 schema
	"enum": true, "const": true, "uniqueItems": true, "multipleOf": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true,
	"pattern": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true,
	"minProperties": true, "maxProperties": true,
	// 注解 / 标识 / 定义容器：本身不作用于当前值（定义要被 `$ref` 引用才生效， 而 `$ref` 在危险表里）
	"title": true, "description": true, "default": true, "examples": true,
	"example": true, "deprecated": true, "format": true,
	"contentMediaType": true, "contentEncoding": true,
	"$comment": true, "$schema": true, "$id": true, "$anchor": true,
	"$dynamicAnchor": true, "$recursiveAnchor": true, "$vocabulary": true,
	"$defs": true, "definitions": true,
}

// cannotProveNoWriteOnly 报告一个 schema 节点上是否有「无法证明不含 writeOnly」的关键字。
//
// 三个 walker **共用同一个判据**，这是 #356 之后的不变量：
//   - `maskValue`（values.go，回执 `values` 的叶子级打码）→ 整值换成 `MaskedValue`；
//
// - `refHitsWriteOnly`（引用/param 链路）→ 判敏感（param 整值打码）； - `subtreeHasWriteOnly`（祖先整块替换）→ 判真。
//
// 一处改、三处生效；只改一处会重演「一个 walker 打码、另一个漏明文」的裂缝。
func cannotProveNoWriteOnly(sch map[string]any) bool {
	for key, val := range sch {
		if !maskProvableKeywords[key] {
			// 危险表里的（理由见上），以及本仓子集从未见过的关键字（未来 draft / 厂商扩展）， 一律算无法证明 —— 「不认识」不等于「安全」，这正是 GAP-1 的教训。
			return true
		}
		if !maskKeywordShapeProvable(key, val) {
			return true
		}
	}
	return false
}

// maskKeywordShapeProvable 报告「walker 会读的结构关键字」的**值形态**是不是我们能走的那种。
//
// 为什么单靠关键字名不够：同名关键字在不同 draft 里有不同形态。draft-07 的 `items` 可以是 **schema 列表**（元组写法），而 walker 只读对象 / 布尔的 `items`——列表形态里的 writeOnly
// 看不见。这类「形状不认识」与「关键字不认识」是同一个 fail-open，所以同样按无法证明处理。
func maskKeywordShapeProvable(key string, val any) bool {
	switch key {
	case "properties":
		_, ok := val.(map[string]any)
		return ok
	case "items", "additionalProperties":
		switch val.(type) {
		case map[string]any, bool:
			return true
		}
		return false
	case "prefixItems":
		_, ok := val.([]any)
		return ok
	}
	return true
}

// refHitsWriteOnly 报告一条 values 点号引用是否会把 writeOnly 明文带进回执。
//
// 两段判定： 1. **下钻途中**看每一步节点**自身**的 `writeOnly`（与 `maskValue` 的递归口径一致），
//
//	   以及它是否带「无法证明」的关键字（`cannotProveNoWriteOnly`：`$ref` 等，见上）；
//	2. **走完整条路径后**，对**最终解析节点**再判整棵子树（`subtreeHasWriteOnly`）——
//	   引用一个「里面装着 writeOnly 叶的容器」时，整对象 / 整数组 / 整个数组元素插值同样
//	   会把明文带进 `unapplied[].declared/actual`，必须整值打码。
//
// 中间节点**不做**子树判定：root 的子树几乎必然含 writeOnly，那样 `{{values.project}}`
// 这种普通引用也会被打码，偏离 mock 语义（对齐前端 mock 的 `pbPolicySecret(pbReferencePolicy(...))` ——只在解析节点上判子树）。
//
// 下钻规则：对象按 `properties`（未命中回落 `additionalProperties` 的任意键）、
// 数组按下标 + `items` + `prefixItems`（两分支**并查**，因为引用可能同时被两边约束—— 任一分支命中 writeOnly 就敏感，只许多打码）。
// 路径不在 schema 里 → 非敏感（schema 没说是密文，就不猜）。空路径不是引用，不会走到这里。
//
// 逐条语义（与 mock 对齐）： - `{{values.auth}}`（对象祖先，`properties.token.writeOnly`）→ true
// - `{{values.credList}}`（数组祖先，`items` 子树里有 writeOnly）→ true - `{{values.credList.0}}`（解析到 `items` 节点，其子树含 writeOnly）→ true
//   - `{{values.credList.0.token}}`（解析到 writeOnly 叶）→ true（下钻途中就命中）
//   - `{{values.auth.label}}`（label 本身是普通叶，auth 下的 writeOnly 兄弟**不传染**）→ false
//   - `{{values.token}}` / `{{values.token.pin}}`（`token` 是 `$ref` → 无法证明）→ true
//   - `{{values.project}}` / 不存在的路径 → false - 混合串 `secret={{values.auth}}` 的**敏感性函数**→ true（渲染阶段本来会
//     `render_type_mismatch`，但推导必须保守为真：先打码，别赌渲染一定失败）
func refHitsWriteOnly(ref string, sch map[string]any) bool {
	curs := []map[string]any{sch}
	for _, seg := range splitValuePath(ref) {
		var next []map[string]any
		for _, cur := range curs {
			// 自身 writeOnly，或带「无法证明」的关键字（`$ref` / `if` / `patternProperties`…）：
			// 后者意味着这个节点的子树里有看不见的约束，按有 writeOnly 处理（fail-closed）。
			if isWriteOnly(cur) || cannotProveNoWriteOnly(cur) {
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

// subtreeHasWriteOnly 报告一个 schema 节点**自身或其任意后代**是否声明了 `writeOnly: true`， 以及它是否带「无法证明」的关键字（`cannotProveNoWriteOnly`）。
//
// 调用点：`refHitsWriteOnly`（引用最终解析到的节点）、`maskValue` 的 `additionalProperties`
// 兜底、`arrayElementSchema`（元组按位）。 - 节点自身 writeOnly / 带无法证明的关键字（`$ref` 等）→ true；
//   - `properties` 下任一子节点递归为真 → true（`{{values.auth}}` 因 `auth.properties.token`）；
//   - `items` 递归为真 → true（`{{values.credList}}` 因 `credList.items.properties.token`）；
//   - `prefixItems` 任一元素递归为真 → true（元组写法的同一条规则）；
//
// - `additionalProperties`（对象 schema 形态）递归为真 → true（任意键都可能是密文， 与 `maskValue` 的兜底打码口径对齐）。
//
// 不做关键词猜测（`format` / 字段名一律不看）：只有 `writeOnly` 是密文的唯一真相。
// 唯一例外是 `maskUnprovableKeywords` 里那类「要求值才能判定」的关键字——它们不是「猜 字段名」，而是「承认自己证明不了」（GAP-1）。
func subtreeHasWriteOnly(sch map[string]any) bool {
	if sch == nil {
		return false
	}
	if isWriteOnly(sch) || cannotProveNoWriteOnly(sch) {
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
// 下钻优先级：**先 `properties`，未命中回落 `additionalProperties`（任意键）， 再「纯数字下标 + `prefixItems`/`items`」**。
// 同一个节点可能同时带 `properties` 与 `items`（`type: ["object","array"]`，或对象节点上
// 多写了一个 `items`）。真实值的形态由 `properties` 决定（`maskValue` 对 map 也走 properties），
// 所以键名命中 properties 时必须走对象分支；只有 properties 里没有这个键、**且段是纯数字**、 且 `items`/`prefixItems` 存在时，才按数组元素下钻（`{{values.list.0}}`）。
// 反过来（items 抢在 properties 前）会让 `{{values.dual.pin}}` 被判「路径不在 schema」→ 非敏感 → 明文漏进回执 `unapplied[]`。
//
// 其它规则： - 带 `items` 的数组节点上，非下标段没有对象字段可下钻（既有口径，`additionalProperties`
// 也没声明时返回空）； - `prefixItems[idx]` 不是对象 schema（布尔 `true` / `null`）→ 不产生候选，但**不阻断**
//
//	`items` 候选（否则相对「下标段一律走 items」的旧行为是 fail-open 回归）。
func descendSchemaCandidates(sch map[string]any, seg string) []map[string]any {
	if sch == nil {
		return nil
	}
	// 带「无法证明」关键字（`$ref` / `patternProperties` / `if`…）的节点：它的子 schema 推不出来 （`$ref` 指向的定义、模式匹配到的子 schema 都在别处）。把**本节点**当唯一候选返回，
	// 于是后续路径仍在「无法证明」的判据下被检查 —— 三处 walker 同判（`cannotProveNoWriteOnly`），
	// 不允许这里返回空集把引用悄悄判成非敏感。`refHitsWriteOnly` 当前会在调用前先判，这一层 是给未来的新调用点兜的：少写一次判据就漏明文，这正是 #356 的形态。
	if cannotProveNoWriteOnly(sch) {
		return []map[string]any{sch}
	}
	if child, ok := schema.Properties(sch)[seg].(map[string]any); ok {
		return []map[string]any{child}
	}
	var out []map[string]any
	// `properties` 未命中：声明了对象形态的 `additionalProperties` 就按「任意键」回落 （`values` 里键名由安装方自己定，schema 只能靠 additionalProperties 说它是密文）。
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

// maskTargets 是 declared 侧打码：**新建切片 + 新建 params map**，敏感 param 的整值换成 `MaskedValue`，其它 key 原样（不做深拷贝）。
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
// 对齐算法见 `inheritSensitivity`（同名能力的并集）；兜底方向只有“多打码”：没有任何同名能力 可对齐（含包里把出口全删了、`len(declared)==0`）→ 该 target 整组打码。
func maskActualTargets(declared, actual []sourcespec.Target, sets []targetSensitivity) []sourcespec.Target {
	out := make([]sourcespec.Target, len(actual))
	for i, t := range actual {
		out[i] = maskTargetAt(t, inheritSensitivity(declared, sets, t.Capability))
	}
	return out
}

// inheritSensitivity 是 D3 的 actual 侧对齐：取**同名能力的全部 declared target 的并集**。
//
// 为什么不用“同下标优先”：`sourcespec.SameTargets` 明确把 target 顺序当**非语义** （config.go：“顺序不同但内容相同 = 等价”，扇出顺序不是语义），所以库里顺序与包里合法地不同。
// 按位置绑定会把 A target 的敏感集套到 B target 上——B 里其他 key 名的 writeOnly 明文就漏了
// （实测：两条同名 `a-review`，一条 `token`、一条 `secret`，库顺序相反 → 两侧明文进回执）。 取**同名并集**与顺序无关，且方向只会多打码。
//
// 名字对不上（出口改名 / 旧包已删该能力 / 包里一个 target 都没有）→ 没有任何 key 名可继承， 而库里旧行可能正是上一版由 writeOnly values 派生的值。此时**整组打码**（`allParams`），
// 与「`len(declared)==0`」同口径：绝不回退到“空集 = 零打码”。
//
// 名字对得上时还返回 `knownParams`（同名 target 的全部 param key 并集）：旧行里出现、而当前 包里已不存在的 key 也一律打码（`maskTargetAt`）。这条覆盖“同名能力内把 writeOnly 派生
// param 改名/删除”的两轮安装——旧 key 不在任何敏感集里，但旧值仍可能来自 writeOnly。
//
// 口径「退到该源全部敏感集的并集」在并集为空时会整侧零打码（review major），
// 回归见 `TestPlanUnappliedActualMasksWholeGroupWhenDeclaredRenamesExport`。
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

// maskTargetAt 打码一个 target 的 params。
//
// 早退只在「确实没有可打码的东西」时成立：declared 侧（knownParams==nil）没有敏感 key 就是没有；actual 侧即使没有任何敏感 key，也要逐 key 扫一遍——改名/删除的旧 key 不在
// `keys` 里，但仍在 `knownParams` 之外，必须打码。把 actual 侧也按 `len(keys)==0` 早退， 会漏掉「新包该能力已无 writeOnly 引用、但旧行还带着改名前的 writeOnly 派生值」这一档。
func maskTargetAt(t sourcespec.Target, set targetSensitivity) sourcespec.Target {
	if len(t.Params) == 0 {
		return t
	}
	if !set.allParams && len(set.keys) == 0 && set.knownParams == nil {
		return t
	}
	params := make(map[string]any, len(t.Params))
	masked := false
	for k, v := range t.Params {
		// knownParams 非 nil 只出现在 actual 侧：旧 key 在当前包同名能力里已不存在 （改名/删除）→ 没有 provenance 可归因，整值打码（多打码方向安全）。
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
