package playbook

// 生成式**不变量**测试：`writeOnly` 打码（issue #356）。
//
// 为什么要一条 property 而不是第 10 个大用例：这一族在 #356 上挂了 9 个 fix —— 同一份
// JSON-Schema 上跑着多个独立 walker：
//
//	① `MaskValues` / `maskValue`（values.go）：回执 `values` 的打码，**按值形态**递归；
//	② `refHitsWriteOnly` / `valueReferencesWriteOnly`（sensitive.go）：回执
//	   `unapplied[].declared/actual` 的敏感性推导，**按引用路径**下钻。
//
// 每次漏一种容器形状（`properties` / `additionalProperties` / `items` / `prefixItems` /
// `type:[object,array]` / 祖先继承）就补一个手写大用例，于是**下一种形状仍然漏**。判据应该
// 是「两个 walker 对同一个叶子的判定必须一致」这条不变量，而不是 N 个样例。本文件把它写成
// 生成式测试：固定种子、可重跑、无 flaky。
//
// 三条判据（同一份随机 schema + values 上同时成立）：
//
//	P1 **walker 一致**：`MaskValues` 把叶子打成了 `MaskedValue`（含「祖先整块被打码」）
//	   ⇔ `refHitsWriteOnly(该叶子的引用, schema)` 判定它命中 writeOnly。不一致即 bug。
//	P2 **无明文泄漏**：每个「语义上必须打码」的叶子都注入可识别哨兵（`secret-<seed>-…`），
//	   打码后的整棵结构 / 整份回执 JSON（values + unapplied.declared/actual + notes）/
//	   valuesDigest 里都不许出现它。
//	P3 **顺序无关**：同一份 schema/values 换 key 的插入顺序（以及 schema 自己的 key 插入
//	   顺序）后，打码结果 / digest / 回执 JSON 必须字节相同——防「依赖 map 迭代顺序」的 bug。
//
// 生成器覆盖的容器形态见 `TestMaskPropertyGeneratorCoversShapes` 的账本（形状没被覆盖到就红，
// 防止生成器悄悄退化成「只覆盖 properties」）。`$ref` 现在按 **fail-closed** 参与严格断言：
// 节点上一出现 `$ref` 就整值打码（不解引用，见 sensitive.go 的 `cannotProveNoWriteOnly`），
// 所以生成器把 `$ref` 节点下的每个叶子都标成「语义上必须打码」，并让被引用定义**可以**带
// writeOnly（即 GAP-1 的真实形状）；两种同文档写法（`#/$defs/X` 与 `#/definitions/X`）都造。
//
// 纪律（与 helpers_test.go 同：不 skip、不弱化断言）：失败时打印**最小复现**——seed + 可粘贴的
// schema/values 字面量 + 叶子路径，做到「看到红就知道是哪条路径分叉」。

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

// maskPropertySeeds 是固定种子数（100 个）：整测试必须远在 5s 内跑完。
const maskPropertySeeds = 100

// maskPropertyGap 是一条**已知缺口**：两个 walker 一致、但语义上应该打码却没打码的形状。
//
// 与「失败清单」的区别：缺口不是 walker 之间的分叉（所以 P1 绿），而是两个 walker **共同**
// 的 fail-open。它必须显式记账（不能静默 skip），由父会话裁决「算 bug 还是算契约外」。
type maskPropertyGap struct {
	ID    string // 缺口编号 + issue 占位
	Shape string // 命中的 schema 形状
	Why   string // 为什么当前不处理
	Fix   string // 两条候选修法（产品侧）
}

// maskPropertyKnownGaps 是**已知缺口清单**（当前为空）。
//
// 纪律：缺口不是 walker 之间的分叉（所以 P1 绿），而是两个 walker **共同**的 fail-open。
// 它必须显式记账（不能静默 skip），由父会话裁决「算 bug 还是算契约外」。GAP-1（`writeOnly`
// 藏在同文档 `$ref` 后面）已修（fail-closed 整值打码，正向断言见
// `TestMaskValuesRefKeywordMasksWholeValue`），条目已删除；新缺口按同一格式追加。
var maskPropertyKnownGaps []maskPropertyGap

// ---------- 生成器的中间表示 ----------

// propShape 是「一份 schema + 一份与之类型一致的 values」的**同一份形状**。
//
// 为什么先生成 shape 再分别物化 schema / values：schema 与 values 必须是**同一个形状**，
// 否则会遇到「schema 说是对象、values 给了字符串」这种产品代码本来就不承诺的组合 ——
// 那类不一致会让 P1 报出一条假分叉（`maskValue` 对非容器值原样透传，而
// `refHitsWriteOnly` 仍按 schema 判子树），把测试变成噪音源。
type propShape struct {
	kind      string // object / array / tuple / dual / ref / string / integer / boolean
	writeOnly bool

	// object / dual：声明的键
	props []propField
	// object / dual：未声明的键（只有 additionalProperties 为 schema 或 true 时才生成）
	undeclared []string
	// addl 是 additionalProperties 关键字的写法："" / "true" / "false" / "schema"
	addl      string
	addlShape *propShape

	// array / dual：元素形状
	items *propShape
	// tuple：prefixItems（按位置）
	prefix []*propShape

	// ref：`$ref` 指向的定义名（前缀决定写在哪个容器：`D*` → `$defs`，`E*` → `definitions`）
	ref string
}

// propField 是对象上的一个声明键。
type propField struct {
	name  string
	shape *propShape
}

// propLeaf 是生成器造 values 时**顺手记下**的一个标量叶子。
//
// 关键：`Secret` 是**生成期就知道的事实**（这个叶子自己声明了 writeOnly，或它的某个祖先
// 节点声明了 writeOnly → 整块打码），不是测试事后用 schema 反推出来的第二份实现。
type propLeaf struct {
	Path   string // values 点号路径（= 引用写法，如 `envs.dev1.pin` / `tuple.0`）
	Kind   string // string / integer / boolean
	Secret bool   // 语义上必须被打码
	Sent   string // 字符串叶子的哨兵明文（非字符串为空）
}

// propNode 是生成器记下的一个**容器节点**（对象 / 数组 / 元组 / `$ref`）。
//
// 为什么要叶片以外再记节点：`unapplied[]` 的敏感性是按 **param** 粒度的 —— 一个 param 引用
// `{{values.credList}}`（数组祖先）时，只要该节点子树里有 writeOnly，**整个 param** 就打成
// `***`（`refHitsWriteOnly` 的「祖先/子树」那一档，sensitive.go）。这一档是 #356 里真实漏过
// 明文的形状（“敏感是叶子级的、打码是 param 级的”），只比叶子会漏掉它。
//
// `Sensitive` = 语义上「引用这个节点的 param 必须被打码」：自己/祖先声明了 writeOnly，或子树
// 里任何后代声明了 writeOnly。与 `refHitsWriteOnly` 同口径，但这里是生成器自己的形状事实。
type propNode struct {
	Path      string
	Sensitive bool
}

// propBuilder 是确定性生成器（`math/rand` + 固定种子 ⇒ 每个 seed 永远同一份语料）。
type propBuilder struct {
	r      *rand.Rand
	seed   int64
	defs   map[string]*propShape // 同文档定义：`D*` 落在 `$defs`、`E*` 落在 `definitions`
	inDef  bool                  // 造定义时禁止再生成 `$ref`（避免自引用/互引用）
	leafN  int
	leaves []propLeaf
	nodes  []propNode
	cov    map[string]int // 形状覆盖账本（TestMaskPropertyGeneratorCoversShapes 断言非空）
}

// seedSentinel 造一个哨兵明文/普通明文：`%04d` 的叶子编号保证两条哨兵互不为前缀
// （否则 `strings.Contains` 会出现假泄漏），`path` 只为失败信息可读。
func seedSentinel(prefix string, seed int64, n int, path string) string {
	return fmt.Sprintf("%s-%d-%04d-%s", prefix, seed, n, path)
}

// ---------- 生成：shape → schema / values ----------

// genShape 生成一个形状；depth 是**剩余嵌套层数**（0 = 只能是标量叶）。
//
// 容器节点也可能整块 `writeOnly: true`（`writeOnly-container` 账本项）：这时整对象/整数组
// 必须被整值替换，两个 walker 都要在**下钻途中**就命中（`refHitsWriteOnly` 每步先看
// `isWriteOnly(cur)`）。
func (b *propBuilder) genShape(depth int) *propShape {
	if depth <= 0 || b.r.Intn(6) == 0 {
		return b.genLeafShape()
	}
	var out *propShape
	switch b.r.Intn(10) {
	case 0, 1, 2:
		out = b.genObjectShape(depth)
	case 3, 4:
		b.cov["items"]++
		out = &propShape{kind: "array", items: b.genShape(depth - 1)}
	case 5:
		out = b.genTupleShape(depth)
	case 6, 7:
		out = b.genDualShape(depth)
	case 8:
		if b.inDef || len(b.defs) == 0 {
			return b.genLeafShape()
		}
		out = b.genRefShape()
	default:
		return b.genLeafShape()
	}
	if b.r.Intn(8) == 0 {
		out.writeOnly = true
		b.cov["writeOnly-container"]++
	}
	return out
}

// genLeafShape 生成一个标量叶形状（writeOnly 概率 ~1/4，由 genLeafShape 自己决定）。
func (b *propBuilder) genLeafShape() *propShape {
	out := &propShape{}
	switch b.r.Intn(3) {
	case 0:
		out.kind = "string"
	case 1:
		out.kind = "integer"
	default:
		out.kind = "boolean"
	}
	if b.r.Intn(4) == 0 {
		out.writeOnly = true
		b.cov["writeOnly-leaf-"+out.kind]++
	}
	return out
}

// genObjectShape 生成对象形状：`properties` + 四种 `additionalProperties` 写法。
func (b *propBuilder) genObjectShape(depth int) *propShape {
	out := &propShape{kind: "object"}
	n := 1 + b.r.Intn(3)
	for i := 0; i < n; i++ {
		out.props = append(out.props, propField{
			name:  fmt.Sprintf("k%d", i),
			shape: b.genShape(depth - 1),
		})
	}
	b.cov["properties"]++
	switch b.r.Intn(4) {
	case 0:
		// 不写 additionalProperties（= 默认 true，但**不是** schema 形态）
	case 1:
		out.addl = "true"
		b.cov["additionalProperties-bool-true"]++
	case 2:
		out.addl = "false"
		b.cov["additionalProperties-bool-false"]++
	default:
		out.addl = "schema"
		out.addlShape = b.genShape(depth - 1)
		b.cov["additionalProperties-schema"]++
	}
	// 未声明的键只在 additionalProperties 允许时生成（false 会被 values_invalid 拒掉）。
	switch out.addl {
	case "true", "schema":
		for i := 0; i < b.r.Intn(3); i++ {
			out.undeclared = append(out.undeclared, fmt.Sprintf("u%d", i))
		}
	}
	return out
}

// genTupleShape 生成元组形状（`prefixItems`）。
//
// **故意不同时**让 `prefixItems[i]` 与 `items` 都带 writeOnly：那是契约里写明的歧义形状
// （`arrayElementSchema` 只取一个 schema、`refHitsWriteOnly` 把两个分支并查——见
// sensitive.go 的注释），它属于「按位打码 vs 并查」的口径选择，不是漏。这里守「至多一侧
// 带 writeOnly」，把口径歧义留给已经被手写用例钉住的 `TestMaskValuesPrefixItemsWriteOnlyTuple`。
//
// 口径对齐（关键）：`maskValue` 对第 i 个元素只取**一个** schema —— `arrayElementSchema`
// 的规则是「`prefixItems[i]` 子树有 writeOnly 就用它，否则回落到 `items`」。生成器必须造出
// **与这条规则一致**的 (schema, values)：
//   - 元素值按 `effectiveElem`（与 `arrayElementSchema` 同口径）物化，否则会造出
//     「声明 schema 一个类型、值按另一个 schema 造」的假语料——那类值连校验都过不了；
//   - `prefixItems[i]` 自己不带 writeOnly 时，把它按 `items` 的副本归一化（只脱 writeOnly）：
//     声明 schema 与实战打码 schema 因此类型一致，值又能真的被 `items` 的 writeOnly 打码
//     （契约写明的「旧口径不回退」：`prefixItems` 普通 + `items.writeOnly` ⇒ 元素整体打码）。
func (b *propBuilder) genTupleShape(depth int) *propShape {
	out := &propShape{kind: "tuple"}
	switch b.r.Intn(3) {
	case 0:
		// prefixItems only（items 缺失 ⇒ 超出 prefix 的元素不受约束）
		for i := 0; i < 1+b.r.Intn(3); i++ {
			out.prefix = append(out.prefix, b.genShape(depth-1))
		}
		b.cov["prefixItems"]++
	case 1:
		// prefix 全普通 + items 可能 writeOnly（「旧口径不回退」形状）
		out.items = b.genShape(depth - 1)
		for i := 0; i < 1+b.r.Intn(2); i++ {
			plain := cloneShape(out.items)
			stripWriteOnly(plain)
			out.prefix = append(out.prefix, plain)
		}
		b.cov["prefixItems+items"]++
	default:
		// prefix 可能带 writeOnly + items 是普通 schema
		for i := 0; i < 1+b.r.Intn(2); i++ {
			out.prefix = append(out.prefix, b.genShape(depth-1))
		}
		plain := b.genShape(depth - 1)
		stripWriteOnly(plain)
		out.items = plain
		b.cov["prefixItems+items"]++
	}
	// 归一化：prefixItems[i] 不带 writeOnly ⇒ 实战打码会回落到 items，声明 schema 也换成它。
	for i := range out.prefix {
		if out.items != nil && !shapeHasWriteOnly(out.prefix[i]) {
			plain := cloneShape(out.items)
			stripWriteOnly(plain)
			out.prefix[i] = plain
		}
	}
	return out
}

// effectiveElem 是数组第 i 个元素**实战会用的** schema（与 `arrayElementSchema` 同口径）：
// `prefixItems[i]` 子树有 writeOnly 就用它，否则回落 `items`；两者都没有时取 `prefixItems[i]`
// （`items` 缺失 ⇒ 该位置不受约束，按 prefix 声明的类型物化值，结果同样是「不打码」）。
func effectiveElem(sh *propShape, i int) *propShape {
	if i < len(sh.prefix) && shapeHasWriteOnly(sh.prefix[i]) {
		return sh.prefix[i]
	}
	if sh.items != nil {
		return sh.items
	}
	if i < len(sh.prefix) {
		return sh.prefix[i]
	}
	return nil
}

// shapeHasWriteOnly 报告一个形状里是否声明了 writeOnly（自身或任意后代）。
// 与产品侧 `subtreeHasWriteOnly` 同口径，但这是生成器**自己**的形状事实（不读产品结果）。
func shapeHasWriteOnly(sh *propShape) bool {
	if sh == nil {
		return false
	}
	if sh.kind == "ref" {
		// fail-closed：`$ref` 一律算「可能有 writeOnly」（产品侧 `cannotProveNoWriteOnly` 同判）。
		return true
	}
	if sh.writeOnly {
		return true
	}
	for _, f := range sh.props {
		if shapeHasWriteOnly(f.shape) {
			return true
		}
	}
	for _, p := range sh.prefix {
		if shapeHasWriteOnly(p) {
			return true
		}
	}
	return shapeHasWriteOnly(sh.items) || shapeHasWriteOnly(sh.addlShape)
}

// cloneShape 深拷贝一个形状（归一化时不能改到被共享的 `items` 本体）。
func cloneShape(sh *propShape) *propShape {
	if sh == nil {
		return nil
	}
	out := &propShape{
		kind:      sh.kind,
		writeOnly: sh.writeOnly,
		addl:      sh.addl,
		ref:       sh.ref,
		prefix:    make([]*propShape, 0, len(sh.prefix)),
	}
	out.undeclared = append(out.undeclared, sh.undeclared...)
	for _, f := range sh.props {
		out.props = append(out.props, propField{name: f.name, shape: cloneShape(f.shape)})
	}
	for _, p := range sh.prefix {
		out.prefix = append(out.prefix, cloneShape(p))
	}
	out.items = cloneShape(sh.items)
	out.addlShape = cloneShape(sh.addlShape)
	return out
}

// genDualShape 生成 `type: [object, array]`：properties 与 items 同时存在，值任选一侧。
func (b *propBuilder) genDualShape(depth int) *propShape {
	out := &propShape{kind: "dual"}
	for i := 0; i < b.r.Intn(3); i++ {
		out.props = append(out.props, propField{
			name:  fmt.Sprintf("k%d", i),
			shape: b.genShape(depth - 1),
		})
	}
	out.items = b.genShape(depth - 1)
	b.cov["type-object+array"]++
	return out
}

// genRefShape 生成一个 `$ref` 节点（指向同文档定义；定义可能带 writeOnly，也可能不带）。
func (b *propBuilder) genRefShape() *propShape {
	names := make([]string, 0, len(b.defs))
	for name := range b.defs {
		names = append(names, name)
	}
	// 键序无关地选一个定义：用 r.Intn 在**排序后**的名字切片上取，保证跨版本/跨平台确定性。
	sortStrings(names)
	name := names[b.r.Intn(len(names))]
	// 账本：两种同文档写法各记一维（`#/$defs/X` 与 `#/definitions/X`），并把
	// 「被引用定义里真的有 writeOnly」（GAP-1 的原形）与「没有（保守多打码）」分开记。
	b.cov["$ref"]++
	b.cov["$ref-"+refContainer(name)]++
	if def, ok := b.defs[name]; ok && shapeHasWriteOnly(def) {
		b.cov["writeOnly-behind-ref"]++
	} else {
		b.cov["ref-def-plain"]++
	}
	return &propShape{kind: "ref", ref: name}
}

// refContainer 把定义名映射到它所在的同文档容器关键字。
//
// 两种写法（2020-12 的 `$defs` 与 draft-07 习惯的 `definitions`）都必须被语料覆盖：
// fail-closed 下它们同判（见 `cannotProveNoWriteOnly`），只测一种写法等于没测另一种。
func refContainer(name string) string {
	if strings.HasPrefix(name, "E") {
		return "definitions"
	}
	return "$defs"
}

// stripWriteOnly 清掉子树里的全部 writeOnly（用于元组的「至多一侧」纪律）。
func stripWriteOnly(sh *propShape) {
	if sh == nil {
		return
	}
	sh.writeOnly = false
	for _, f := range sh.props {
		stripWriteOnly(f.shape)
	}
	for _, p := range sh.prefix {
		stripWriteOnly(p)
	}
	stripWriteOnly(sh.items)
	stripWriteOnly(sh.addlShape)
}

// buildSchema 把形状物化成 JSON-Schema（map 形态，就是 `pkg.schemaOf()` 解出来的样子）。
func (b *propBuilder) buildSchema(sh *propShape, depth int) map[string]any {
	out := map[string]any{}
	switch sh.kind {
	case "string", "integer", "boolean":
		out["type"] = sh.kind
	case "object", "dual":
		if sh.kind == "dual" {
			out["type"] = []any{"object", "array"}
		} else {
			out["type"] = "object"
		}
		if len(sh.props) > 0 {
			props := make(map[string]any, len(sh.props))
			for _, f := range sh.props {
				props[f.name] = b.buildSchema(f.shape, depth+1)
			}
			out["properties"] = props
		}
		switch sh.addl {
		case "true":
			out["additionalProperties"] = true
		case "false":
			out["additionalProperties"] = false
		case "schema":
			out["additionalProperties"] = b.buildSchema(sh.addlShape, depth+1)
		}
		if sh.kind == "dual" && sh.items != nil {
			out["items"] = b.buildSchema(sh.items, depth+1)
		}
		if depth >= 2 {
			b.cov["nested>=2"]++
		}
		if depth >= 3 {
			b.cov["nested>=3"]++
		}
	case "array":
		out["type"] = "array"
		out["items"] = b.buildSchema(sh.items, depth+1)
	case "tuple":
		out["type"] = "array"
		prefix := make([]any, 0, len(sh.prefix))
		for _, p := range sh.prefix {
			prefix = append(prefix, b.buildSchema(p, depth+1))
		}
		out["prefixItems"] = prefix
		if sh.items != nil {
			out["items"] = b.buildSchema(sh.items, depth+1)
		}
	case "ref":
		out["$ref"] = "#/" + refContainer(sh.ref) + "/" + sh.ref
	}
	if sh.writeOnly {
		out["writeOnly"] = true
	}
	return out
}

// buildValue 物化一份与该形状**类型一致**的 values，并顺手登记每个标量叶子（含哨兵）。
//
// secret = 祖先节点已声明 writeOnly ⇒ 这一整棵子树都要被打码（整值替换，不是子串替换）。
func (b *propBuilder) buildValue(sh *propShape, path string, secret bool) any {
	secret = secret || sh.writeOnly
	switch sh.kind {
	case "string", "integer", "boolean":
		return b.buildLeafValue(sh.kind, path, secret)
	case "ref":
		// fail-closed：`$ref` 节点整值打码（不解引用就证明不了被引用定义里没有 writeOnly），
		// 所以这整棵子树的叶子「语义上必须打码」——哪怕被引用定义自己没写 writeOnly。
		b.recordNode(path, true)
		def, ok := b.defs[sh.ref]
		if !ok {
			return b.buildLeafValue("string", path, true)
		}
		return b.buildValue(def, path, true)
	case "object", "dual":
		b.recordNode(path, secret || shapeHasWriteOnly(sh))
		if sh.kind == "dual" && sh.items != nil && b.r.Intn(2) == 0 {
			b.cov["dual-array-value"]++
			return b.buildArrayValue(sh, path, secret)
		}
		if sh.kind == "dual" {
			b.cov["dual-object-value"]++
		}
		out := make(map[string]any, len(sh.props)+len(sh.undeclared))
		for _, f := range sh.props {
			out[f.name] = b.buildValue(f.shape, joinPropPath(path, f.name), secret)
		}
		for _, name := range sh.undeclared {
			switch sh.addl {
			case "schema":
				out[name] = b.buildValue(sh.addlShape, joinPropPath(path, name), secret)
			default: // "true"：任意键，没有子 schema ⇒ 当普通字符串叶
				out[name] = b.buildLeafValue("string", joinPropPath(path, name), secret)
			}
		}
		return out
	case "array", "tuple":
		b.recordNode(path, secret || shapeHasWriteOnly(sh))
		return b.buildArrayValue(sh, path, secret)
	}
	// 不可能走到：形状只有上面这些 kind。
	return b.buildLeafValue("string", path, secret)
}

// recordNode 登记一个可被 param 引用的容器节点（根不计：`{{values}}` 不是合法引用）。
func (b *propBuilder) recordNode(path string, sensitive bool) {
	if path == "" {
		return
	}
	b.nodes = append(b.nodes, propNode{Path: path, Sensitive: sensitive})
}

// joinPropPath 拼一个点号路径（根为空串时不要拼出前导点）。
func joinPropPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// buildArrayValue 按位置物化数组值：第 i 个元素用 `effectiveElem`（与 `arrayElementSchema`
// 同口径），超出 `prefixItems` 的用 `items`。
func (b *propBuilder) buildArrayValue(sh *propShape, path string, secret bool) any {
	n := b.r.Intn(4)
	if sh.kind == "tuple" {
		n = b.r.Intn(len(sh.prefix) + 3)
	}
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		elemPath := joinPropPath(path, fmt.Sprintf("%d", i))
		elem := effectiveElem(sh, i)
		if elem == nil {
			// 超出 prefixItems 且没有 items：JSON Schema 不约束该位置。
			// 仍然登记成普通字符串叶，让 P1（walker 一致）也覆盖「未约束元素」。
			out = append(out, b.buildLeafValue("string", elemPath, secret))
			continue
		}
		out = append(out, b.buildValue(elem, elemPath, secret))
	}
	return out
}

// buildLeafValue 物化一个标量叶并登记它的哨兵。
func (b *propBuilder) buildLeafValue(kind, path string, secret bool) any {
	b.leafN++
	switch kind {
	case "string":
		prefix := "plain"
		if secret {
			prefix = "secret"
		}
		v := seedSentinel(prefix, b.seed, b.leafN, path)
		b.leaves = append(b.leaves, propLeaf{Path: path, Kind: kind, Secret: secret, Sent: v})
		return v
	case "integer":
		b.leaves = append(b.leaves, propLeaf{Path: path, Kind: kind, Secret: secret})
		return 1000 + b.r.Intn(1<<20)
	default: // boolean
		b.leaves = append(b.leaves, propLeaf{Path: path, Kind: kind, Secret: secret})
		return b.r.Intn(2) == 0
	}
}

// newPropBuilder 为一个种子造生成器，并先造同文档定义（`D*` → `$defs`，`E*` → `definitions`）。
//
// 定义**不再脱 writeOnly**：语料必须覆盖 GAP-1 的真实形状（writeOnly 藏在 `$ref` 后面），
// 同时也会造出「定义无 writeOnly 但引用仍被整值打码」的保守档（两者都记在账本里）。
func newPropBuilder(seed int64) *propBuilder {
	b := &propBuilder{
		r:    rand.New(rand.NewSource(seed)),
		seed: seed,
		defs: map[string]*propShape{},
		cov:  map[string]int{},
	}
	b.inDef = true
	for i := 0; i < 1+b.r.Intn(2); i++ {
		b.defs[fmt.Sprintf("D%d", i)] = b.genShape(1 + b.r.Intn(2))
	}
	// 固定造一个 `definitions`（draft-07 写法）侧的定义：两种容器都要非空，账本才能各记一维。
	b.defs["E0"] = b.genShape(1 + b.r.Intn(2))
	b.inDef = false
	return b
}

// build 造一份完整的 (schema, values, leaves, nodes)：根永远是对象（values 必须是 JSON 对象）。
func (b *propBuilder) build() (map[string]any, map[string]any, []propLeaf, []propNode) {
	root := b.genObjectShape(1 + b.r.Intn(3))
	// 根上的 writeOnly 不生成：根被打码时 `MaskValues` 会返回**空 map**（`"***"` 不是对象），
	// 那属于「整份 values 都不该进回执」的另一个口径（契约要求 values 是对象），不是本文件的判据。
	root.writeOnly = false
	sch := b.buildSchema(root, 0)
	// 两种同文档容器都写出来（`$ref` 分别指到 D* / E*）：语料因此同时覆盖 `#/$defs/X`
	// 与 `#/definitions/X` 两种写法。
	defsByWord := map[string]map[string]any{}
	for name, def := range b.defs {
		word := refContainer(name)
		if defsByWord[word] == nil {
			defsByWord[word] = map[string]any{}
		}
		defsByWord[word][name] = b.buildSchema(def, 2)
	}
	for word, defs := range defsByWord {
		sch[word] = defs
	}
	// 根的形状与 buildSchema 一致（根上的 writeOnly 已清掉，见上）。
	values, _ := b.buildValue(root, "", false).(map[string]any)
	b.cov["root-object"]++
	return sch, values, b.leaves, b.nodes
}

// ---------- 断言工具 ----------

// maskedLeafState 读「打码后的树里这个叶子是否被打码」。
//
// 三种结局都算**已打码**：叶子自己变成 `MaskedValue`、某个祖先整块变成 `MaskedValue`
// （整对象/整数组替换）、叶子因为祖先被打码而从树上消失。这与
// `refHitsWriteOnly`（下钻途中命中 writeOnly 也返回 true）口径对齐。
func maskedLeafState(masked any, path string) bool {
	if masked == MaskedValue {
		return true
	}
	cur := masked
	for _, seg := range strings.Split(path, ".") {
		if cur == MaskedValue {
			return true
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return false
			}
			cur = v
		case []any:
			idx, ok := toIntFromString(seg)
			if !ok || idx < 0 || idx >= len(node) {
				return false
			}
			cur = node[idx]
		default:
			return false
		}
	}
	return cur == MaskedValue
}

// mustJSON 序列化（失败即 Fatal）：失败信息与泄漏扫描都用它。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}

// minRepro 打印最小复现：seed + 可粘贴的 schema/values 字面量 + 叶子路径。
func minRepro(seed int64, sch, values map[string]any, leaf propLeaf, extra string) string {
	return fmt.Sprintf(
		"\n=== 最小复现（seed=%d，叶子 %q，%s）===\n"+
			"schema (%#v):\n%s\nvalues (%#v):\n%s\n"+
			"复跑：go test ./internal/playbook -run '%s' -count=1 -v\n",
		seed, leaf.Path, extra,
		sch, mustJSONPlain(sch),
		values, mustJSONPlain(values),
		"TestMaskPropertyWalkersAgree",
	)
}

// mustJSONPlain 是 minRepro 用的「失败路径也可用」的序列化（不进 t.Fatalf）。
func mustJSONPlain(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// reorderValue 造一份**内容等价、key 插入顺序不同**的副本（数组顺序有语义，不 shuffle）。
//
// Go 的 map 没有可观察的插入顺序，所以这里真正能钉住的是「结果不依赖任何遍历顺序」：
// 每个 map 的键序都换一遍，schema 的 properties 序、params 序、以及各处 `for k := range`
// 的顺序跟着变；若哪一处把「先遍历到的键」当成了语义（如取第一个命中），结果就会分叉。
func reorderValue(v any, r *rand.Rand) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sortStrings(keys)
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		out := make(map[string]any, len(val))
		for _, k := range keys {
			out[k] = reorderValue(val[k], r)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = reorderValue(item, r)
		}
		return out
	default:
		return v
	}
}

// ---------- P1 + P2 + P3：100 个固定种子的主测试 ----------

// TestMaskPropertyWalkersAgree 是这条不变量测试的主体：100 个固定种子上同时验 P1/P2/P3。
//
// 每个种子的完整流程：
//
//	① 生成 schema + values + 叶子/节点账本（生成期就记下「哪些叶子/容器语义上必须打码」）；
//	② 直接对 (schema, values) 跑 `MaskValues` 与 `refHitsWriteOnly` 的逐叶对比（P1）；
//	③ 装一个真实包、跑 `Render` + `Plan` + `BuildReceipt`，验回执里无哨兵明文（P2），
//	   并在 **param 粒度**对账 `unapplied[].declared/actual` 的「必须打码 ⇔ 确实 `***`」；
//	④ 打乱 schema/values 的 key 插入顺序各两轮，验结果字节相同（P3）。
func TestMaskPropertyWalkersAgree(t *testing.T) {
	totalLeaves, totalSecret := 0, 0
	unappliedSeen, unappliedMaskedParams, unappliedPlainParams := 0, 0, 0
	for seed := int64(0); seed < maskPropertySeeds; seed++ {
		b := newPropBuilder(seed)
		sch, values, leaves, nodes := b.build()
		totalLeaves += len(leaves)
		masked := MaskValues(sch, values)

		// ---------- P1：两个 walker 对同一叶子的判定必须一致 ----------
		for _, leaf := range leaves {
			if leaf.Secret {
				totalSecret++
			}
			maskHit := maskedLeafState(masked, leaf.Path)
			refHit := refHitsWriteOnly(leaf.Path, sch)
			if maskHit != refHit {
				t.Fatalf("P1 walker 分叉：%s叶子 %q 上 MaskValues 判 %v、refHitsWriteOnly 判 %v"+
					"（schema 形状与叶子路径见下）%s",
					leaf.Kind, leaf.Path, maskHit, refHit,
					minRepro(seed, sch, values, leaf, "walker 判定不一致"))
			}
			// 生成期事实（语义上必须打码）与两个 walker 的判定也必须一致：
			// 只在上面的 walker 一致之后再对账，否则会掩盖真正的分叉方向。
			if leaf.Secret != maskHit {
				t.Fatalf("P1 生成期事实与打码结果不符：%s叶子 %q 生成期标为 %v、MaskValues 判 %v%s",
					leaf.Kind, leaf.Path, leaf.Secret, maskHit,
					minRepro(seed, sch, values, leaf, "打码方向与生成期事实不符"))
			}
		}

		// ---------- P2：回执里绝不允许出现哨兵明文 ----------
		maskedJSON := mustJSON(t, masked)
		for _, leaf := range leaves {
			if !leaf.Secret || leaf.Sent == "" {
				continue
			}
			if strings.Contains(maskedJSON, leaf.Sent) {
				t.Fatalf("P2 明文泄漏：打码后的 values 里仍有哨兵 %q%s",
					leaf.Sent, minRepro(seed, sch, values, leaf, "MaskValues 漏打码"))
			}
		}

		// ---------- P2（端到端）：真实包的 Render + Plan + BuildReceipt ----------
		refs := propRefs(leaves, nodes)
		pkg := propPackage(t, sch, refs)
		plan, err := Plan(pkg, values, propExisting(seed, refs))
		if err != nil {
			t.Fatalf("seed=%d 计划失败: %v\nschema=%s\nvalues=%s", seed,
				err, mustJSONPlain(sch), mustJSONPlain(values))
		}
		receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
			InstallMeta{ID: fmt.Sprintf("i-%d", seed), At: ts})
		// 回执 values 必须与直接调用一致（同一条打码链路，不允许有第二份口径）。
		if !reflect.DeepEqual(receipt.Values, masked) {
			t.Fatalf("P1 回执 values 与直接 MaskValues 不一致：receipt=%s direct=%s",
				mustJSONPlain(receipt.Values), mustJSONPlain(masked))
		}
		receiptJSON := mustJSON(t, receipt)
		if !strings.Contains(receiptJSON, plan.ValuesDigest) {
			t.Fatalf("seed=%d 回执缺 valuesDigest", seed)
		}
		// 语料自检 + param 粒度对账：`unapplied[].declared/actual` 是 #356 的第二条链路，
		// 打码是**按 param key 整值替换**的 —— 每个 param 必须满足「语义上该打码 ⇔ 就是 `***`」。
		if len(receipt.Unapplied) > 0 {
			unappliedSeen++
			sensitive := map[string]bool{}
			for _, ref := range refs {
				sensitive[ref.Key] = ref.Sensitive
			}
			for _, entry := range receipt.Unapplied {
				for _, side := range []struct {
					name  string
					value any
				}{{"declared", entry.Declared}, {"actual", entry.Actual}} {
					targets, ok := side.value.([]sourcespec.Target)
					if !ok {
						t.Fatalf("seed=%d unapplied.%s 不是 targets: %#v", seed, side.name, side.value)
					}
					for _, target := range targets {
						for key, want := range sensitive {
							got, ok := target.Params[key]
							if !ok {
								t.Fatalf("seed=%d unapplied.%s 丢了 param %q: %#v",
									seed, side.name, key, target.Params)
							}
							if got == MaskedValue {
								unappliedMaskedParams++
							} else {
								unappliedPlainParams++
							}
							if (got == MaskedValue) != want {
								t.Fatalf("P1 param 分叉（不变量②）：unapplied.%s 的 param %q（引用 `values.%s`）"+
									"语义上必须打码=%v，实际值=%#v%s",
									side.name, key, refPath(refs, key), want, got,
									minRepro(seed, sch, values, propLeaf{Path: refPath(refs, key)}, "param 级打码不一致"))
							}
						}
					}
				}
			}
		}
		for _, leaf := range leaves {
			if !leaf.Secret || leaf.Sent == "" {
				continue
			}
			// 哨兵必须真的到过回执数据（否则下面的「不许出现」是空断言）。
			if !strings.Contains(mustJSON(t, plan.Values), leaf.Sent) {
				t.Fatalf("P2 语料自检失败：哨兵 %q 不在有效 values 里%s",
					leaf.Sent, minRepro(seed, sch, values, leaf, "哨兵未进 plan.Values"))
			}
			if strings.Contains(receiptJSON, leaf.Sent) {
				t.Fatalf("P2 明文泄漏：回执 JSON 里仍有哨兵 %q（values/unapplied/notes 任一字段）%s\n回执：%s",
					leaf.Sent, minRepro(seed, sch, values, leaf, "回执漏明文"), receiptJSON)
			}
			if strings.Contains(plan.ValuesDigest, leaf.Sent) {
				t.Fatalf("P2 valuesDigest 里出现明文 %q%s",
					leaf.Sent, minRepro(seed, sch, values, leaf, "digest 含明文"))
			}
		}

		// ---------- P3：顺序无关 ----------
		// 两路证据：
		//  ① 同一份输入**重跑多次**：Go 的 map 遍历顺序每次 range 都重新随机，重跑就是在换
		//     真实迭代序（改 map 插入顺序在 Go 里不可观察，只能靠这个采样）；
		//  ② 把 schema/values 每个 map 的键序重新排一遍再算：结果必须字节相同。
		baseMasked := mustJSON(t, masked)
		baseDigest, err := ValuesDigest(values)
		if err != nil {
			t.Fatalf("seed=%d digest: %v", seed, err)
		}
		for round := 0; round < 2; round++ {
			// ① 重跑：Plan 是纯函数，回执必须与上一轮字节相同（map 迭代序每次不同）。
			repeatPlan, err := Plan(pkg, values, propExisting(seed, refs))
			if err != nil {
				t.Fatalf("seed=%d 重跑计划失败: %v", seed, err)
			}
			repeatReceipt := BuildReceipt(repeatPlan, repeatPlan.Values, repeatPlan.ValuesDigest,
				InstallMeta{ID: fmt.Sprintf("i-%d", seed), At: ts})
			if got := mustJSON(t, repeatReceipt); got != receiptJSON {
				t.Fatalf("P3 顺序相关：同一份输入重跑后回执变了（round=%d）\n第一份：%s\n第二份：%s%s",
					round, receiptJSON, got,
					minRepro(seed, sch, values, propLeaf{Path: "(整棵树)"}, "回执依赖 map 迭代顺序"))
			}
			// ② 键序重排后重算
			rr := rand.New(rand.NewSource(seed*100 + int64(round) + 1))
			sch2 := reorderValue(sch, rr).(map[string]any)
			values2 := reorderValue(values, rr).(map[string]any)
			masked2 := MaskValues(sch2, values2)
			if got := mustJSON(t, masked2); got != baseMasked {
				t.Fatalf("P3 顺序相关：换 key 顺序后打码结果变了（round=%d）\n第一份：%s\n第二份：%s\n%s",
					round, baseMasked, got,
					minRepro(seed, sch, values, propLeaf{Path: "(整棵树)"}, "打码结果依赖遍历顺序"))
			}
			digest2, err := ValuesDigest(values2)
			if err != nil {
				t.Fatalf("seed=%d digest2: %v", seed, err)
			}
			if digest2 != baseDigest {
				t.Fatalf("P3 顺序相关：换 key 顺序后 valuesDigest 变了（round=%d）：%s vs %s",
					round, baseDigest, digest2)
			}
			// 逐叶判定也必须与第一份一致（防「打码结果碰巧相同、敏感性判定却变了」）。
			for _, leaf := range leaves {
				if got := maskedLeafState(masked2, leaf.Path); got != maskedLeafState(masked, leaf.Path) {
					t.Fatalf("P3 顺序相关：叶子 %q 的打码判定变了（round=%d）%s",
						leaf.Path, round,
						minRepro(seed, sch2, values2, leaf, "敏感性判定依赖遍历顺序"))
				}
				if got := refHitsWriteOnly(leaf.Path, sch2); got != refHitsWriteOnly(leaf.Path, sch) {
					t.Fatalf("P3 顺序相关：叶子 %q 的 ref 判定变了（round=%d）%s",
						leaf.Path, round,
						minRepro(seed, sch2, values2, leaf, "ref 判定依赖遍历顺序"))
				}
			}
		}
	}
	// 语料自检：100 个种子里必须真的既有秘密叶、又有公开叶（否则 P2 会退化成空断言）。
	if totalLeaves < 200 {
		t.Fatalf("语料太薄：100 个种子只生成 %d 个叶子", totalLeaves)
	}
	if totalSecret == 0 || totalSecret == totalLeaves {
		t.Fatalf("语料失衡：%d 个叶子中 %d 个是 writeOnly（应当两头都有）", totalLeaves, totalSecret)
	}
	// `unapplied[].declared/actual` 是 #356 的第二条链路：它必须真的被跑到、且必须两侧都
	// 出现过「打了码」与「没打码」的 param，否则 param 粒度的对账就是空断言。
	if unappliedSeen == 0 || unappliedMaskedParams == 0 || unappliedPlainParams == 0 {
		t.Fatalf("语料太薄：%d 个种子里 unapplied 出现 %d 次、打码 param %d 个、未打码 param %d 个",
			maskPropertySeeds, unappliedSeen, unappliedMaskedParams, unappliedPlainParams)
	}
	// 语料规模（给审阅者一眼看到这条测试真的跑了多少东西）。
	t.Logf("语料：%d 个种子 / %d 个叶子（%d 个 writeOnly）/ unapplied %d 次 / 打码 param %d 个 / 未打码 param %d 个",
		maskPropertySeeds, totalLeaves, totalSecret, unappliedSeen, unappliedMaskedParams, unappliedPlainParams)
}

// ---------- 端到端夹具：真实包 + 真实回执 ----------

// propRef 是一条 source target 的 param：它引用哪个 values 路径，以及**语义上是否必须打码**。
//
// 这是 param 粒度的事实（不是叶子粒度）：声明/实际两侧的打码都是**按 param key 整值替换**的，
// 所以断言也落在 param 上：`masked ⇔ Sensitive`。
type propRef struct {
	Key       string
	Path      string
	Sensitive bool
}

// propRefs 把叶子与容器节点拼成 param 清单（key 序稳定，便于跨轮次逐 key 对比）。
//
// 为什么要同时引用「叶子」和「容器祖先」：`unapplied[]` 的敏感推导对引用**容器**的 param
// 走的是「子树含 writeOnly」那一档（sensitive.go 的祖先继承）。只测叶子会漏掉
// 「叶子级敏感 / param 级打码」这个真实漏过的形状（issue #356）。
func propRefs(leaves []propLeaf, nodes []propNode) []propRef {
	refs := make([]propRef, 0, len(leaves)+len(nodes))
	add := func(path string, sensitive bool) {
		refs = append(refs, propRef{Key: fmt.Sprintf("p%02d", len(refs)), Path: path, Sensitive: sensitive})
	}
	for _, leaf := range leaves {
		add(leaf.Path, leaf.Secret)
	}
	for _, node := range nodes {
		add(node.Path, node.Sensitive)
	}
	return refs
}

// refPath 返回某个 param key 引用的 values 路径（失败信息用）。
func refPath(refs []propRef, key string) string {
	for _, ref := range refs {
		if ref.Key == key {
			return ref.Path
		}
	}
	return key
}

// propPackage 把生成器的一份 schema/values 装成**真实包**：values schema 进 `spec.values`，
// 每条 propRef 变成一个 source target 的 param（这样 `unapplied[].declared/actual` 的打码
// 链路才真的被走到）。
func propPackage(t *testing.T, sch map[string]any, refs []propRef) *Package {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("metadata:\n  name: prop-src\nspec:\n  kind: manual\n  enabled: true\n  config: {}\n")
	if len(refs) == 0 {
		sb.WriteString("  targets: []\n")
	} else {
		sb.WriteString("  targets:\n    - capability: audit\n      params:\n")
		for _, ref := range refs {
			fmt.Fprintf(&sb, "        %s: '{{values.%s}}'\n", ref.Key, ref.Path)
		}
	}
	// `$defs` / `$ref` 是合法 JSON（YAML 是 JSON 的超集），直接缩进进 spec.values。
	b := newBundle().
		setValues(mustJSONPlain(sch)).
		cap("capabilities/audit.yaml", strings.ReplaceAll(fixerDoc, "{{values.project}}-fixer", "audit")).
		source("sources/prop.yaml", sb.String())
	return b.pkg(t)
}

// propExisting 造「库里旧行」：同名源的旧 targets（同样的 param key、不同（旧）明文），
// 这样 Plan 走第 12 格 → `unapplied[]` 的 declared/actual 两侧都被打码链路覆盖。
func propExisting(seed int64, refs []propRef) Existing {
	f := newFakeExisting()
	if len(refs) == 0 {
		return f.withSource("prop-src", "manual", "{}")
	}
	params := make(map[string]any, len(refs))
	for _, ref := range refs {
		params[ref.Key] = fmt.Sprintf("old-%d-%s", seed, ref.Key)
	}
	return f.withSource("prop-src", "manual", "{}", sourcespec.Target{Capability: "audit", Params: params})
}

// ---------- 生成器覆盖账本 ----------

// TestMaskPropertyGeneratorCoversShapes 断言生成器**真的覆盖**每一种容器形态。
//
// 为什么单独一条：property 测试最大的退化方式是「生成器悄悄只剩 properties」——测试照样绿，
// 但覆盖已经塌回那个「修一个漏一个」的形态。这条账本把每种形状的计数钉住，删任何一种分支都会失败。
func TestMaskPropertyGeneratorCoversShapes(t *testing.T) {
	cov := map[string]int{}
	for seed := int64(0); seed < maskPropertySeeds; seed++ {
		b := newPropBuilder(seed)
		b.build()
		for k, v := range b.cov {
			cov[k] += v
		}
	}
	want := []string{
		"properties",
		"additionalProperties-bool-true",
		"additionalProperties-bool-false",
		"additionalProperties-schema",
		"items",
		"prefixItems",
		"prefixItems+items",
		"type-object+array",
		"$ref",
		// 同文档引用的两种写法各一维：`#/$defs/X`（2020-12）与 `#/definitions/X`（draft-07 习惯）。
		// fail-closed 下两者必须同判，只覆盖一种等于只钉住一半。
		"$ref-$defs",
		"$ref-definitions",
		// GAP-1 的原形（writeOnly 藏在 `$ref` 后面）与保守档（被引用定义无 writeOnly 仍打码）。
		"writeOnly-behind-ref",
		"ref-def-plain",
		"nested>=2",
		"nested>=3",
		"writeOnly-leaf-string",
		"writeOnly-leaf-integer",
		"writeOnly-container",
		"dual-object-value",
		"dual-array-value",
		"root-object",
	}
	for _, key := range want {
		if cov[key] == 0 {
			keys := make([]string, 0, len(cov))
			for k := range cov {
				keys = append(keys, k)
			}
			sortStrings(keys)
			t.Fatalf("生成器没覆盖形状 %q（当前账本：%v）", key, cov)
		}
	}
	// 账本全量：审阅者能直接看到每种形状被造了多少次。
	keys := make([]string, 0, len(cov))
	for k := range cov {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		t.Logf("覆盖帐本 %-28s %d", k, cov[k])
	}
}

// ---------- 已修缺口（GAP-1）：`$ref` ⇒ 整值打码 ----------

// TestMaskValuesRefKeywordMasksWholeValue 是 GAP-1 修好之后的正向断言：`spec.values` 里出现
// 同文档 `$ref` 时**整值打码**（不解引用、不返回值）。
//
// 两种写法都盯：`#/$defs/X`（2020-12）与 `#/definitions/X`（draft-07 习惯），本包对两者的
// 处理完全相同——都不解析，所以都必须整值打码。
//
// 并且**故意**分两档：
//   - 被引用定义里**有** writeOnly（GAP-1 的原始形状，曾经明文进回执）；
//   - 被引用定义里**没有** writeOnly（保守档：仍然整值打码）。后者是刻意的取舍：中枢不是
//     求值器，不解引用就证明不了安全，所以按密文处置（多打码是可用性问题，漏明文是安全问题）。
//
// 三处 walker 必须同判：`MaskValues`（整值）、`refHitsWriteOnly`（param 链路）、
// `subtreeHasWriteOnly`（祖先整块替换）。
func TestMaskValuesRefKeywordMasksWholeValue(t *testing.T) {
	cases := []struct {
		name      string
		word      string // 定义容器的关键字
		defSecret bool   // 被引用定义里是否真的有 writeOnly
	}{
		{"2020-12 `$defs` / 定义含 writeOnly", "$defs", true},
		{"2020-12 `$defs` / 定义无 writeOnly（保守多打码）", "$defs", false},
		{"draft-07 `definitions` / 定义含 writeOnly", "definitions", true},
		{"draft-07 `definitions` / 定义无 writeOnly（保守多打码）", "definitions", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defPin := map[string]any{"type": "string"}
			if tc.defSecret {
				defPin["writeOnly"] = true
			}
			sch := map[string]any{
				"type": "object",
				"properties": map[string]any{
					"token": map[string]any{"$ref": "#/" + tc.word + "/Credential"},
					"note":  map[string]any{"type": "string"},
				},
				tc.word: map[string]any{
					"Credential": map[string]any{
						"type":       "object",
						"properties": map[string]any{"pin": defPin},
					},
				},
			}
			const sentinel = "secret-0-0001-token.pin"
			values := map[string]any{
				"token": map[string]any{"pin": sentinel},
				"note":  "plain-note",
			}

			// ① `MaskValues`：`$ref` 节点整值打码，同一层的普通叶不被误伤。
			masked := MaskValues(sch, values)
			if got := masked["token"]; got != MaskedValue {
				t.Fatalf("`$ref` 节点没被整值打码（GAP-1 fail-open）：%#v", got)
			}
			if got := masked["note"]; got != "plain-note" {
				t.Fatalf("同一层的普通叶被误伤：%#v", got)
			}

			// ② `refHitsWriteOnly`：引用 `$ref` 节点或其下的任何路径都判敏感。
			if !refHitsWriteOnly("token", sch) {
				t.Fatal("refHitsWriteOnly(token) = false：param 链路会漏明文")
			}
			if !refHitsWriteOnly("token.pin", sch) {
				t.Fatal("refHitsWriteOnly(token.pin) = false：param 链路会漏明文")
			}
			if refHitsWriteOnly("note", sch) {
				t.Fatal("refHitsWriteOnly(note) = true：普通叶被误判为敏感")
			}

			// ③ `subtreeHasWriteOnly`：`$ref` 节点自身就是「无法证明」。
			refNode, _ := sch["properties"].(map[string]any)["token"].(map[string]any)
			if !subtreeHasWriteOnly(refNode) {
				t.Fatalf("subtreeHasWriteOnly($ref 节点) = false：祖先整块替换那一档会漏，节点=%#v", refNode)
			}

			// ④ 端到端：回执里不许出现明文，但执行事实（plan.Values / PlannedSource.Spec）继续带明文。
			refs := []propRef{{Key: "p00", Path: "token.pin", Sensitive: true}}
			plan, err := Plan(propPackage(t, sch, refs), values, propExisting(0, refs))
			if err != nil {
				t.Fatalf("`$ref` 回执包计划失败: %v", err)
			}
			receipt := BuildReceipt(plan, plan.Values, plan.ValuesDigest,
				InstallMeta{ID: "i-ref", At: ts})
			receiptJSON := mustJSON(t, receipt)
			if strings.Contains(receiptJSON, sentinel) {
				t.Fatalf("回执里仍有 `$ref` 后面的明文 %q：%s", sentinel, receiptJSON)
			}
			if !strings.Contains(receiptJSON, MaskedValue) {
				t.Fatalf("回执里既没有明文也没有打码占位，断言退化成空断言：%s", receiptJSON)
			}
			if got := plan.Values["token"].(map[string]any)["pin"]; got != sentinel {
				t.Fatalf("执行事实（plan.Values）被动过，应当继续带明文：%#v", got)
			}
			planned := findPlannedSource(plan.Sources, "prop-src")
			if planned == nil || len(planned.Spec.Targets) != 1 {
				t.Fatalf("计划里没有 prop-src 或 target 数不为 1: %#v", planned)
			}
			if got := planned.Spec.Targets[0].Params["p00"]; got != sentinel {
				t.Fatalf("PlannedSource.Spec 被打码（执行事实不能打码）：%#v", got)
			}
		})
	}
}

// TestMaskPropertyNoKnownGaps 断言缺口账本当前为空。
//
// 缺口必须**显式记账**（不许静默 skip）：新发现「两个 walker 一致、但语义上该打码却没打」
// 的形状时，先往 `maskPropertyKnownGaps` 加条目 + tripwire，再由父会话裁决产品修法。
// GAP-1 已修（见上一条用例），这条防它或下一个缺口被悄悄加回来。
func TestMaskPropertyNoKnownGaps(t *testing.T) {
	if len(maskPropertyKnownGaps) != 0 {
		ids := make([]string, 0, len(maskPropertyKnownGaps))
		for _, gap := range maskPropertyKnownGaps {
			ids = append(ids, gap.ID+" / "+gap.Shape)
		}
		t.Fatalf("缺口账本非空（缺口必须先红后绿、不能长期挂在账本上）：%v", ids)
	}
}
