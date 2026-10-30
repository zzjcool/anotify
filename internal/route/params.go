package route

// 参数模板求值（#114）：源事件 payload（+ 源声明的别名表 config.vars）→ 实际参数。
//
// 本文件是 I6「中枢不解释任何用户提供的逻辑」的**结构性**实现：
//
//	用户提供的是「文本 + 几个取值点」（占位符），不是「一段要被执行的程序」。
//
// 保证手段有两条，都在语法层面：
//
//  1. 占位符正则只允许「标识符 + 点号」（`{{a.b.0.c}}`）——括号、运算符、
//     空格分隔的表达式在语法上就没有位置，`{{a + b}}` 不会被当作运算求值
//     （配置期直接拒绝，见 ValidateParamTemplates）。
//  2. **单趟替换**：遍历**原始字符串**里的占位符边扫边拼，替换进去的值
//     绝不再被扫描。所以 payload `{"a":"{{b}}"}` 解析 `{{a}}` 得到的是字面
//     文本 `{{b}}`，不是 `B`——没有递归展开，也就不存在无限展开。
//
// 仍然**不做**的：条件 `{{#if}}`、循环 `{{#each}}`、函数 `{{f(x)}}`、
// 默认值表达式、过滤器、JSONPath/正则过滤。这些才需要真正的解释器。

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// placeholderPattern 是占位符的**唯一**语法。
//
// 必须带捕获组（ref 在 m[1]）：取值路径是这里唯一的产物，正则里没有任何
// 可以放运算符/括号/函数调用的位置——这是 I6 的结构性保证，不靠运行时检查。
const placeholderPattern = `\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}`

// wholeRefPattern 是「整个值就是一个引用」的锚定形态（首尾锚定）。
//
// 整值引用与混写的语义不同：整值引用**保留原始类型**（数字还是数字），
// 混写结果一定是字符串。区分它们只能靠锚定——`" {{x}}"` 带前导空格就是混写。
const wholeRefPattern = `^\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}$`

// arrayIndexPattern 是数组下标段的**唯一**合法形态：纯十进制非负整数。
//
// 它比 strconv.Atoi 严格得多：Atoi 会接受 `+1`，还会接受 `01` 这类前导零写法，
// 而契约 §2.1 要求 `+1` / `01` / `-1` / `1e2` / `0.0` **全部 miss**——路径段
// 是「文本里的数组下标」，不是「Go 的整数解析」。
const arrayIndexPattern = `^(0|[1-9][0-9]*)$`

var (
	placeholderRE = regexp.MustCompile(placeholderPattern)
	wholeRefRE    = regexp.MustCompile(wholeRefPattern)
	arrayIndexRE  = regexp.MustCompile(arrayIndexPattern)
)

// asCoerceTypes 是对象形态 `as` 能声明的目标类型（触发期认的全部集合）。
//
// 配置期更严（只允许 string/number/boolean/json，见 ValidateParamTemplates）：
// 配置期收敛是为了让用户少写无意义的东西，触发期放宽是为了不让历史配置
// 在改严之后突然失效。
var asCoerceTypes = map[string]bool{
	"string":  true,
	"number":  true,
	"integer": true,
	"boolean": true,
	"array":   true,
	"object":  true,
	"json":    true,
}

// ParamMiss 是一个「声明了取值但取不到、也没有 default」的参数。
//
// Ref 已渲染成用户能对照的形态：`{{x}}`（占位符原文）/ `payload.x`（from 映射）/
// `$var`（变量映射）。三者分开是为了让用户一眼看出自己写的是哪一种。
type ParamMiss struct {
	Field string
	Ref   string
}

// ParamTypeErr 是一个「取到了值但转不成目标类型」的参数。
//
// Want 是目标类型（as 或 params_schema 声明的 type），Got 是转之前的原值——
// 报告里要能回答「期望 integer，求值得到 "abc"」这样的问题。
type ParamTypeErr struct {
	Field string
	Want  string
	Got   any
}

// HasPlaceholder 判断字符串里是否含至少一个**完整**占位符。
//
// 注意它不作为「有没有写占位符」的唯一判据：半截的 `"PR {{title"`
// 匹配不到完整占位符，必须由 ValidateParamTemplates 的闭合检查兜住
// （否则用户会以为设置生效了）。
func HasPlaceholder(s string) bool {
	return placeholderRE.MatchString(s)
}

// WholeRef 取出「整个值就是一个引用」里的引用；不是则 ok=false。
func WholeRef(s string) (ref string, ok bool) {
	m := wholeRefRE.FindStringSubmatch(s)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// RenderTemplate 单趟替换字符串里的占位符。
//
// 实现要点：`FindAllStringSubmatchIndex(s, -1)` 一次性拿到**原始字符串**里
// 所有占位符的位置，然后顺序拼接「原文片段 + 替换值」。替换值不参与扫描，
// 所以它里面即使含有 `{{x}}` 也原样留在输出里（禁止递归展开）。
//
// 取不到值的占位符**原样保留**（`{{x}}` 不丢）并记进 missing：
// 报错时用户能直接看到自己写了什么，而不是对着一个空字符串猜。
//
// “取不到”的判定完全在 lookup 里：ResolveParams 传给这里的 lookup 已经把
// nil（包括 payload 的显式 `null`）归为未命中（契约 §8.5 A16），所以走求值链路时
// `"v={{x}}"` 绝不会静默变成 `"v="`；nil → "" 分支只服务于“调用方真的
// 报了 found=true 且值就是 nil”这种低层用法。
func RenderTemplate(s string, lookup func(ref string) (any, bool)) (text string, missing []string) {
	matches := placeholderRE.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		ref := s[m[2]:m[3]]
		b.WriteString(s[last:start])

		var (
			val   any
			found bool
		)
		if lookup != nil {
			val, found = lookup(ref)
		}
		if found {
			b.WriteString(stringifyValue(val))
		} else {
			missing = append(missing, ref)
			b.WriteString(s[start:end]) // 原样保留 {{x}}
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String(), missing
}

// GetPath 按点号路径从一个 JSON 对象取值（`a.b.0.c`），取不到返回 ok=false。
//
// 规则（契约 §2.1）：
//   - 对象段取**自有键**（Go 的 map 天然只有自有键，不存在原型链那种泄漏）；
//   - 数组段必须匹配 `^(0|[1-9][0-9]*)$` 且在界内：`+1` / `01` / `-1` / `1e2` /
//     `0.0` 全部 miss（先过正则再 Atoi，顺序不能反）；
//   - 中途撞到标量（字符串/数字/布尔）→ miss，绝不「猜」下一步。
//
// nil 的归属（契约 §8.5 A16）：本函数是**纯路径行走器**，路径真存在但值是显式
// `null` 时返回 `(nil, true)`——“路径存在”与“取到了可用的值”是两件事，行走器
// 不该替调用方合并。**nil 一律当未命中** 的语义统一在求值层（ResolveParams 的
// lookup）实现：见下面的 lookup。这样 ResolveVars 之类只需要“取路径”的调用方
// 不被语义影响，而 `{{x}}` / `混写` / `{from}` 三种引用形态共用同一个判据。
func GetPath(obj map[string]any, path string) (any, bool) {
	if obj == nil || path == "" {
		return nil, false
	}
	var cur any = obj
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			// 先按形态筛（拒绝 `+1` / `01` / `-1` / `1e2`），再解析
			if !arrayIndexRE.MatchString(seg) {
				return nil, false
			}
			i, err := strconv.Atoi(seg)
			if err != nil || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			// 中间是标量/ nil：路径到此为止取不到东西
			return nil, false
		}
	}
	return cur, true
}

// IsMapping 判断一个值是不是对象形态的映射声明（旧形态，向后兼容）。
//
// 形状（契约 §3）：**恰好**含 `from` / `var` 之一，其余键只能是 `default` / `as`；
// 多一个键就不是映射声明（当字面量对象处理）。`from` / `var` 的值必须是字符串
// ——它是路径/变量名，`{var:1}` 这种写法不是声明（配置期会报错，这里不认）。
func IsMapping(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	hasFrom, hasVar := false, false
	for k, raw := range m {
		switch k {
		case "from":
			hasFrom = true
		case "var":
			hasVar = true
		case "default", "as":
			// 可选键，形状不变
		default:
			return false // 多一个键就不是映射声明
		}
		if k == "from" || k == "var" {
			if _, isStr := raw.(string); !isStr {
				return false
			}
		}
	}
	return hasFrom != hasVar // 恰好一个
}

// IsVarRef 判断一个值是不是「显式引用某个已声明变量」的 `{var:"name"}`。
//
// 与 `{from}` / `{{x}}` 的区别：它**只**查别名表，不回退 payload 路径。
// 所以配置期必须已经声明过这个变量（拼错立刻 400），而 `{{x}}` 不要求声明。
func IsVarRef(v any) bool {
	if !IsMapping(v) {
		return false
	}
	m, _ := v.(map[string]any)
	name, ok := m["var"].(string)
	return ok && name != ""
}

// CoerceValue 按目标类型转换一个求值结果；转不了返回 (nil,false)。
//
// 为什么需要它：整值引用保留原类型（数字还是数字），但混写模板一定是字符串。
// 目标字段声明 integer 时，模板拼出来的 "31" 该转成 31；而 "abc" 转不了 ——
// 那种情况必须剪枝报 param_type，**不能**退回原值（那是静默的数据错误）。
//
// want 为空（字段没有声明类型）时原样返回，不做任何猜测。
func CoerceValue(v any, want string) (any, bool) {
	if want == "" {
		return v, true
	}
	switch want {
	case "integer", "number":
		switch n := v.(type) {
		case int:
			return n, true
		case int64:
			return n, true
		case float64:
			if want == "integer" && n != math.Trunc(n) {
				return nil, false // integer 拒绝非整数
			}
			return n, true
		case string:
			s := strings.TrimSpace(n)
			if s == "" {
				return nil, false
			}
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, false
			}
			if want == "integer" && f != math.Trunc(f) {
				return nil, false
			}
			return f, true
		case bool:
			// true/false 在 JSON 里等价 1/0，是常见写法，别一律拒绝
			if n {
				return float64(1), true
			}
			return float64(0), true
		}
		return nil, false

	case "boolean":
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			switch strings.TrimSpace(b) {
			case "true":
				return true, true
			case "false":
				return false, true
			}
			return nil, false
		case int:
			return boolFromNumber(float64(b), int64(b))
		case int64:
			return boolFromNumber(float64(b), b)
		case float64:
			return boolFromNumber(b, int64(b))
		}
		return nil, false

	case "string":
		switch s := v.(type) {
		case string:
			return s, true
		case nil:
			return nil, false // null 转 "null"/"<nil>" 毫无意义，宁可剪枝
		case map[string]any, []any:
			b, err := json.Marshal(s)
			if err != nil {
				return nil, false
			}
			return string(b), true
		default:
			return fmt.Sprint(s), true
		}

	case "array":
		if _, ok := v.([]any); !ok {
			return nil, false
		}
		return v, true

	case "object":
		if _, ok := v.(map[string]any); !ok {
			return nil, false
		}
		return v, true

	default:
		// json 以及任何未知目标：原样直传（未知目标不该让值消失）
		return v, true
	}
}

// boolFromNumber 把 1/0 转成布尔；其它数字不是布尔（不猜）。
func boolFromNumber(f float64, i int64) (any, bool) {
	if f != float64(i) {
		return nil, false // 非整数（如 1.5）不认
	}
	switch i {
	case 1:
		return true, true
	case 0:
		return false, true
	}
	return nil, false
}

// SchemaPropType 取某个字段在 params_schema 里声明的类型（取不到返回 ""）。
//
// 参数值可能来自模板，类型「求出来才知道」：同一个 `{{pull_request.additions}}`
// 既可能是数字也可能被拼进字符串。目标字段需要什么类型，只能问 schema。
func SchemaPropType(schema json.RawMessage, field string) string {
	if len(schema) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return ""
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		return ""
	}
	prop, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, ok := prop["type"].(string)
	if !ok {
		return ""
	}
	return t
}

// ResolveVars 求值源的别名表（config.vars）：常量原样、from 路径按 payload 取值。
//
// 取不到的变量**不进表**（也不报错）：它可能是这次事件没带的字段
// （比如 push 事件没有 pull_request）。真正的「取不到」由参数求值时报出来
// （param_missing），那里才知道用户到底用了哪个变量、字段名叫什么。
func ResolveVars(defs []store.VarDef, payload map[string]any) map[string]any {
	out := make(map[string]any, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			continue
		}
		if d.HasValue {
			out[d.Name] = d.Value
			continue
		}
		if v, ok := GetPath(payload, d.From); ok {
			out[d.Name] = v
		}
	}
	return out
}

// ValidateParamTemplates 在**配置期**校验参数模板的形式（"" = 通过）。
//
// 只查形式，不查可达性：`{{pull_request.title}}` 无需任何声明就合法，
// 拼错会在触发时以 param_missing 报出来（并原样回显用户写的引用）。
//
// 顺序很关键：字符串**先做闭合检查，再判断有没有占位符**。
// 写了半截的 `"PR {{title"` 时正则匹配不到任何完整占位符，若先判
// HasPlaceholder 就会直接跳过这个字段——用户会以为设置生效了。
//
// 错误消息逐字冻结（契约 §2.2 / §2.4）。
func ValidateParamTemplates(template map[string]any, declaredVars []string) string {
	if len(template) == 0 {
		return ""
	}
	declared := make(map[string]bool, len(declaredVars))
	for _, n := range declaredVars {
		declared[n] = true
	}

	// 按 key 排序遍历：同一个坏模板永远报同一个错（错误顺序稳定）
	for _, k := range sortedKeys(template) {
		v := template[k]

		if s, ok := v.(string); ok {
			if msg := placeholderFormError(k, s); msg != "" {
				return msg
			}
			for _, m := range placeholderRE.FindAllStringSubmatch(s, -1) {
				if invalidRefPath(m[1]) {
					return fmt.Sprintf("参数 %s 的路径不合法：「%s」", k, m[1])
				}
			}
			continue
		}

		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		raw, hasFrom := m["from"]
		nameRaw, hasVar := m["var"]
		// 顺序即口径（契约 §8.5 A15）：
		//  ① from 与 var 并存 → 互斥错（哪怕整个对象因为别的原因不是映射声明）；
		//  ② 不是映射声明（IsMapping 为假）→ **按字面量放过**。派发期同样按字面量
		//     原样传（ResolveParams 的映射分支也用 IsMapping 判定），配置期若在这里
		//     拦下来，就会出现「配置期拒了、派发期明明能传」的口径分裂；
		//  ③ 只有 IsMapping 为真才跑 from/var/as 的逐项校验。
		if hasFrom && hasVar {
			return fmt.Sprintf("参数 %s 同时写了 from 和 var，只能选一个", k)
		}
		if !IsMapping(m) {
			// 例外：`{default:...}` / `{as:...}` 这种「只写了可选键」的对象不是
			// 合法字面量语义，明显是想写映射声明却缺了真正要引用的东西。
			if msg := incompleteMappingError(k, m); msg != "" {
				return msg
			}
			continue
		}

		if hasVar {
			name, isStr := nameRaw.(string)
			if !isStr || name == "" {
				return fmt.Sprintf("参数 %s 的变量名为空", k)
			}
			// {var} 是显式引用：必须已声明（issue 的「坑①」）
			if !declared[name] {
				return fmt.Sprintf("参数 %s 引用了未声明的变量「%s」", k, name)
			}
		}
		if hasFrom {
			from, isStr := raw.(string)
			if !isStr || invalidRefPath(from) {
				return fmt.Sprintf("参数 %s 的 from 路径不合法：「%v」", k, raw)
			}
		}
		if asRaw, ok := m["as"]; ok {
			as, isStr := asRaw.(string)
			if !isStr || !validAsConfig(as) {
				return fmt.Sprintf("参数 %s 的 as 只能是 string/number/boolean/json", k)
			}
		}
	}
	return ""
}

// ValidateLiteralParams 在配置期校验「能确定的那些参数值」（契约 §8.5 A14）。
//
// 为什么需要它：配置期不能直接拿整份模板跑 ValidateParams——`required` 会误拒
// 「字面量 + 模板混用」的模板。比如 schema 要求 `to` 必填、模板写的是
// `{"to":"{{a}}","title":"hi"}`：`to` 的**值**确实要触发时才知道，但**键**
// 已经写了，必填已经满足。拿「剔掉占位符的子集」去跑 ValidateParams 就丢了键信息。
//
// 四条口径：
//  1. 顶层 required 按**模板键集合**判：键在即满足（不论其值是字面量、占位符还是
//     映射声明）。嵌套对象里的 required 不单独判——它跟着第 2 条走。
//  2. 值**完全解析得出**（containsUnresolved 为假）且 properties 里有对象 schema 的
//     字段，用**派发期同一个** validateValue 校验，路径 `params.<key>`。
//     所以 `{"n":"abc"}` 对 integer 这种当场能判的错依然当场报。
//  3. 含未解析内容（占位符 / 映射声明，含**嵌套**）的整棵子树跳过：类型要触发时
//     才成立，派发期的 ValidateParams 兜底。
//  4. additionalProperties 为 bool false 时，模板里出现未在 properties 声明的键
//     一律拒（值是不是占位符都算——键名本来就不依赖求值）。
//
// 对象级 minProperties / maxProperties **不判**：模板的键集合不等于求值后的键集合
// （值可能是映射声明），配置期判它只会误伤。
//
// schema 为空 / 非法一律返回 ""：schema 的好坏是**能力定义**的问题，不该记到源配置
// 账上（派发期的 ValidateParams 会用 invalid params_schema 拦下）。
func ValidateLiteralParams(schema json.RawMessage, template map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return ""
	}
	if len(root) == 0 {
		return ""
	}
	props, _ := root["properties"].(map[string]any)

	// ① 顶层 required：看**键在不在**，不看值是什么形态
	if raw, ok := root["required"]; ok {
		if list, ok := raw.([]any); ok {
			for _, e := range list {
				name, ok := e.(string)
				if !ok {
					continue
				}
				if _, present := template[name]; !present {
					return fmt.Sprintf("params: missing required field %q", name)
				}
			}
		}
	}

	// ② / ④ 逐字段：能确定的字面量校验，未声明的键按 additionalProperties 处理。
	// 按 key 排序遍历：错误顺序稳定（与 validateObject 一致）。
	for _, k := range sortedKeys(template) {
		v := template[k]
		sub, declared := lookupProp(props, k)
		if declared {
			if subSchema, isMap := sub.(map[string]any); isMap && !containsUnresolved(v) {
				if msg := validateValue(subSchema, v, "params."+k); msg != "" {
					return msg
				}
			}
			continue
		}
		if raw, ok := root["additionalProperties"]; ok {
			if allowed, isBool := raw.(bool); isBool && !allowed {
				return fmt.Sprintf("params: unexpected field %q (additionalProperties=false)", k)
			}
		}
	}
	return ""
}

// containsUnresolved 判断一个模板值里是否还含「配置期算不出来」的内容（契约 §8.5 A14）。
//
// 三类算不出来：
//   - 字符串里含至少一个**完整**占位符（`placeholderRE.MatchString`）——它的值要
//     触发时才存在；半截的 `"PR {{x"` 这里算字面量（虽然 ValidateParamTemplates
//     会单独拒它，两件事互不重叠）；
//   - 映射声明 `{from}` / `{var}`（IsMapping）——整个值由别的字段/变量决定；
//   - 上面两类出现在**嵌套**的 map / slice 里。
//
// 其余（数字/布尔/null/无占位符字符串/纯字面量容器）都是「现在就能确定」的。
func containsUnresolved(v any) bool {
	switch t := v.(type) {
	case string:
		return placeholderRE.MatchString(t)
	case map[string]any:
		if IsMapping(t) {
			return true
		}
		for _, e := range t {
			if containsUnresolved(e) {
				return true
			}
		}
		return false
	case []any:
		for _, e := range t {
			if containsUnresolved(e) {
				return true
			}
		}
		return false
	}
	return false
}

// placeholderFormError 做占位符的**闭合检查**（配置期）。
//
// 三段判断（顺序即优先级，契约 §8.3 A4/A11）：
//  1. 连 `{{` 都没有 → **直接放行**。只含 `}}` 是合法字面量：
//     `{"a":{"b":1}}` 这种当字面量传的 JSON 片段必须能保存——它的 `}}`
//     是 JSON 的收尾括号，不是占位符的残骸。
//  2. `{{` 与 `}}` 出现次数不等 → 半截占位符（`"PR {{title"`、`"{{x}} {{"`），
//     原因是「没写全」，文案是「没有闭合」。
//  3. 次数相等还不够：`{{x + 1}}` / `{{f(x)}}` / `{{#if x}}` / `{{.a}}` / `{{$x}}`
//     也都是 1:1，但中间不是合法占位符。把所有**完整占位符**抹掉后，
//     剩下的文本里不能再有 `{{` 或 `}}`——这时原因是「写了但不合法」，
//     必须换文案，否则用户照着「数量不一致」的提示永远修不好。
func placeholderFormError(field, s string) string {
	opens := strings.Count(s, "{{")
	closes := strings.Count(s, "}}")
	if opens == 0 {
		// 没有 `{{` 就没有占位符可言：`}}` 只是普通字面量（JSON 片段）
		return ""
	}
	if opens != closes {
		return fmt.Sprintf("参数 %s：占位符没有闭合（{{ 与 }} 数量不一致）", field)
	}

	var rest strings.Builder
	last := 0
	for _, loc := range placeholderRE.FindAllStringIndex(s, -1) {
		rest.WriteString(s[last:loc[0]])
		last = loc[1]
	}
	rest.WriteString(s[last:])
	if r := rest.String(); strings.Contains(r, "{{") || strings.Contains(r, "}}") {
		return fmt.Sprintf("参数 %s：占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）", field)
	}
	return ""
}

// incompleteMappingError 处理「有映射的可选键、却没有 from/var」的对象。
func incompleteMappingError(field string, m map[string]any) string {
	// 空对象是**字面量**（`{"opts": {}}` 完全合法），不是缺引用的映射声明：
	// 下面那个循环对空 map 一次也不执行，若不先返回，会落到末尾的错误上，
	// 造成「配置期 400、派发期原样传 {}」的分裂。
	if len(m) == 0 {
		return ""
	}
	for k := range m {
		switch k {
		case "default", "as":
			// 可选键，继续看
		default:
			return "" // 有其它键 → 当字面量对象，不拦
		}
	}
	// 全部键都落在 {default,as} 里：那不是字面量，是缺了引用的映射声明
	return fmt.Sprintf("参数 %s 的映射声明缺少 from 或 var", field)
}

// invalidRefPath 判断一个取值路径的**形态**是否合法（配置期用）。
//
// 只拦三种明显的坏形态（`..` / 首 `.` / 尾 `.`）：它们的语义永远是「这里少写了
// 一段」。其它拼错（`pull_requst.title`）无法在配置期判定——那要触发时才知道。
func invalidRefPath(p string) bool {
	return p == "" || strings.Contains(p, "..") || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".")
}

// validAsConfig 判断配置期允许的 `as` 取值（设计 §3.1：只有四个）。
func validAsConfig(as string) bool {
	switch as {
	case "string", "number", "boolean", "json":
		return true
	}
	return false
}

// ResolveParams 把一份参数模板按 payload / 别名表求值成实际参数。
//
// 顺序（每个字段独立判定）：
//
//	① 整值引用 `"{{x}}"`           → **保留原类型**，再按 schema 类型转换
//	② 混写 `"PR #{{n}}"`           → 单趟替换，结果一律字符串，再按 schema 转换
//	③ 对象形态 `{from}` / `{var}`  → as 优先、否则 schema 类型
//	④ 其余字面量                   → 原样克隆，**不转换**（尊重用户写的类型）
//
// 三种失败一律**上报**、绝不静默跳过：
//   - 取不到且没有 default → missing 里一条 ParamMiss（Ref 区分三种引用形态）
//   - 取到了但转不成目标类型 → 立刻返回 ParamTypeErr
//   - 参数 map 为空（nil）→ 返回空 map（不是 nil，调用方不必再判空）
func ResolveParams(template map[string]any, payload map[string]any, vars map[string]any, schema json.RawMessage) (params map[string]any, missing []ParamMiss, typeErr *ParamTypeErr) {
	params = make(map[string]any, len(template))
	if len(template) == 0 {
		return params, nil, nil
	}
	if vars == nil {
		vars = map[string]any{}
	}

	// lookup 解析一个 `{{ref}}`：先剥掉可选的前导 `$`，再查别名表；未命中再当
	// payload 点号路径取。名字冲突时别名优先（D.3）。命中后还要过一道 nil 关：
	// **nil 一律当未命中**。
	//
	// nil = 未命中 的选型（契约 §8.5 A16）：
	//   - payload 里的显式 `null`（JSON 里“字段在、值空”）与“字段不在”在用户眼里
	//     是同一件事——“这次事件没有这个值”。混写 `"v={{x}}"` 若把 null 字符串化成
	//     空串，用户看到的是无提示的 `"v="`，这是静默的数据错误，比直接 param_missing
	//     难查得多；
	//   - 别名表里的 nil 同理（常量变量的值可以写成 null）。
	//
	// 实现位置选择：统一在这一层判，而不是改 GetPath。GetPath 保留“路径存在但值
	// 是 null → (nil,true)”的能力（ResolveVars 之类只需要取路径语义的调用方不受
	// 影响），而三种引用形态（整值 / 混写 / 映射的 from）都走这个 lookup → 口径天然一致。
	//
	// `$` 剥除是**防御性兼容**，不是「支持 {{$x}}」：
	//   - 占位符正则以 `[A-Za-z_]` 起始、不含 `$`，所以 `{{$x}}` 匹配不到任何
	//     完整占位符（配置期被拒「占位符不合法」，存量值派发期原样当字面量传）；
	//   - 这里剥的是**映射声明** `{from:"$v"}` 的旧写法（文档里曾把变量写成
	//     `$name`），只影响对象形态的 from 路径，与 `{{$x}}` 无关。
	lookup := func(ref string) (any, bool) {
		key := strings.TrimPrefix(strings.TrimSpace(ref), "$")
		if v, ok := vars[key]; ok && v != nil {
			return v, true
		}
		v, hit := GetPath(payload, key)
		if !hit || v == nil {
			return nil, false // A16：路径命中到显式 null 也算未命中
		}
		return v, true
	}

	keys := sortedKeys(template)
	for _, k := range keys {
		v := template[k]
		want := ""
		var value any
		resolved := false
		skipCoerce := false

		if s, ok := v.(string); ok {
			if ref, isWhole := WholeRef(s); isWhole {
				// ① 整值引用：保留原类型
				got, hit := lookup(ref)
				if !hit {
					missing = append(missing, ParamMiss{Field: k, Ref: "{{" + ref + "}}"})
					continue
				}
				value, resolved = got, true
				want = SchemaPropType(schema, k)
			} else if HasPlaceholder(s) {
				// ② 混写：单趟替换，结果必字符串
				text, miss := RenderTemplate(s, lookup)
				value, resolved = text, true
				want = SchemaPropType(schema, k)
				for _, ref := range miss {
					missing = append(missing, ParamMiss{Field: k, Ref: "{{" + ref + "}}"})
				}
				if len(miss) > 0 {
					// 值本身就不完整：让调用方按 param_missing 剪枝，
					// 别在这里先报一个「类型不对」把真正的病因盖住。
					skipCoerce = true
				}
			}
		}

		if !resolved {
			if m, ok := v.(map[string]any); ok && IsMapping(m) {
				// ③ 对象形态（旧形态，向后兼容）
				if as := asCoerceType(m); as != "" {
					want = as
				} else {
					want = SchemaPropType(schema, k)
				}
				if hit, ok := resolveMapping(m, vars, lookup); ok {
					value, resolved = hit, true
				} else if d, hasDefault := m["default"]; hasDefault {
					value, resolved = cloneJSON(d), true
				} else {
					missing = append(missing, ParamMiss{Field: k, Ref: mappingMissRef(m)})
					continue
				}
			} else {
				// ④ 字面量：原样克隆，不做类型转换
				value = cloneJSON(v)
				resolved = true
			}
		}

		if !skipCoerce && want != "" {
			coerced, ok := CoerceValue(value, want)
			if !ok {
				return nil, missing, &ParamTypeErr{Field: k, Want: want, Got: value}
			}
			value = coerced
		}
		params[k] = value
	}
	return params, missing, nil
}

// resolveMapping 求值一个对象形态映射：`{var}` 只查别名表，`{from}` 先别名表再 payload。
//
// `{var}` 命中的判定与 `{{x}}` 的 lookup **完全一致**：值存在**且非 nil** 才算命中。
// 别名表里的 nil 表示「这个变量这次没有值」：ResolveVars 不会把取不到的路径塞进表，
// 但常量变量的值本身可以声明成 null。把它当成「取到了 null」会让 `{var}` 与 `{{x}}`
// 对同一份数据得出不同结论——统一为未命中，才能走同一个 default / param_missing
// 降级规则（契约 §8.1 A7 / §8.5 A16）。
//
// `from` 分支不需要自己再判 nil：它直接走 lookup，而 lookup 已经把 nil（含 payload
// 显式 `null`）归为未命中。
func resolveMapping(m map[string]any, vars map[string]any, lookup func(string) (any, bool)) (any, bool) {
	if IsVarRef(m) {
		// {var} 是显式引用，**不**回退 payload 路径
		name, _ := m["var"].(string)
		if v, ok := vars[name]; ok && v != nil {
			return v, true
		}
		return nil, false
	}
	from, _ := m["from"].(string)
	return lookup(from)
}

// mappingMissRef 渲染映射未命中时回显给用户的原始引用文本（契约 §2.2 的三种 detail）。
//
// 用 IsVarRef 而不是自己再读一次 `m["var"]`：判断「这是不是一个变量引用」的规则
// （形状合法 + var 是非空字符串）只该有一份，散开写就会漂移。
func mappingMissRef(m map[string]any) string {
	if IsVarRef(m) {
		name, _ := m["var"].(string)
		return "$" + name
	}
	from, _ := m["from"].(string)
	return "payload." + from
}

// asCoerceType 取映射声明里合法的 `as`（不合法/缺失返回 ""，由 schema 兜底）。
func asCoerceType(m map[string]any) string {
	as, ok := m["as"].(string)
	if !ok || !asCoerceTypes[as] {
		return ""
	}
	return as
}

// stringifyValue 把取到的值塞进模板文本时的字符串化规则。
//
// nil → ""（不留 `<nil>` 这种噪音）；对象/数组 → JSON（这是唯一不丢结构的
// 文本表示）；其它 → fmt.Sprint。刻意**不做** HTML/JSON 转义：
// 值是喂给能力的原始数据，中转擅自转义会让「路径里本来就有引号」变两次。
//
// 注意 nil 分支在求值链路里**不可达**：ResolveParams 的 lookup 把 nil 当未命中
// （契约 §8.5 A16），nil 根本走不到这里，而是变成 param_missing。
func stringifyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any, []any:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprint(t)
	default:
		return fmt.Sprint(t)
	}
}

// cloneJSON 深拷贝一份 JSON 形状的值（避免多个目标共享同一份 map/切片）。
func cloneJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = cloneJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneJSON(e)
		}
		return out
	default:
		return v
	}
}

// sortedKeys 返回排序后的 key（求值/校验都要确定性顺序）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
