package store

// 入口边上的**引用模板**（edges.mapping）——定义期校验（P2.1，typed-pipeline §11.5 选项 A）。
//
// 为什么只有入口边能写模板：触发入口边的是**外部世界**（GitHub / 定时器 / 手工点一下），
// 它不会替你拼参数——`pull_request.title` 怎么变成下游的 `params.title` 只能由配置
// 声明。能力边相反：参数怎么拼是实现的内部事务（§8 已否决能力边的引用映射），
// 能力边的参数由 emit 的 data 提供，中间再插一层搬运只会制造晚发现的
// param_missing / param_type。所以：
//
//	from_is_source=1（入口边）：literals 允许，mapping 允许
//	from_is_source=0（能力边）：literals 允许，mapping **保存时硬拒**
//
// 校验口径必须与 route 包里的配置期判定一致（route.ValidateParamTemplates 管形状、
// route.ValidateLiteralParams 管「能确定的字面量类型」）。但 route import 了 store，
// 反向不成立，所以这里实现的是同一套判定的**最小副本**：只描述「常量或 {{点号路径}}
// 模板」的形状 + 常量走 schema.TypeMatches（与派发期同一个类型原语）。
// 改动任何一边都必须同步另一边；回归用例见 store/edge_mapping_test.go 与
// route/dispatch_test.go 的 param_* 文案断言。
//
// 与 literals 的关系：两者语义相反，**不合并**。
//   - literals 是常量：配置期就能定死类型，所以定义期硬拒类型错；
//   - mapping 是引用模板：类型要触发时求值后才知道，所以定义期只判形状。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// EdgeMappingEmpty 是「这条边没有引用模板」的唯一表示（与 EdgeLiteralsEmpty 同口径：
// 一个值就能表达「没有」，读写下发的 JSON 形状也统一）。
const EdgeMappingEmpty = `{}`

// mappingPlaceholderRE 是占位符的**唯一**语法（`{{点号路径}}`）。
//
// 与 route/params.go 的 placeholderPattern 逐字一致：占位符里没有可以放运算符 /
// 括号 / 函数调用的位置——这是不变量 I6 的结构性保证，不是运行时检查。
// 它在这里只用于**配置期形状判定**（闭合、路径形态）；真正的求值不在 store。
var mappingPlaceholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}`)

// normalizeEdgeMapping 把从库里读出的字节归一成可直接下发/回显的形式：
// 空值（老库 / 空串）当 '{}'，其余**原样保留**（字节级往返一致，与 literals 同口径）。
func normalizeEdgeMapping(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(EdgeMappingEmpty)
	}
	return json.RawMessage(b)
}

// checkEdgeMapping 是 link 层的配置期硬拒（设计 §11.5）。
//
// 校验五条：
//
//	a. 必须是 **JSON 对象**（数组/标量/坏 JSON 一律拒；null 视为没有模板）；
//	b. 非空模板 + 能力边（fromIsSource=false）→ 硬拒（理由见文件头）；
//	c. 每个键必须出现在**目标能力 params_schema 的 properties** 里（拼错当场拒）；
//	d. 每个值必须是「常量」或「{{点号路径}} 模板」两种形状之一：模板的语法错误
//	   （半截 `{{`、`{{a + b}}`、`a..b`）当场拒——它们到触发时只会变成一条
//	   说不清原因的 param_missing；
//	e. **常量**的值类型必须与 params_schema 声明精确相等（沿用 schema.TypeMatches）；
//	   模板值跳过类型判定——类型要等触发时求值后才知道，配置期判它必然误拒
//	   （`"{{n}}"` 硬塞给 integer 字段是合法配置）。
//
// 返回归一化后的模板（未给 → '{}'）。违反返回 ErrEdgeRejected（客户端可修）。
func checkEdgeMapping(to *CapabilityVersion, raw json.RawMessage, fromIsSource bool) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(EdgeMappingEmpty), nil
	}

	// 先解成 any 再断言对象：直接 Unmarshal 到 map 会把 null 当成空 map（无法区分）。
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("%w: mapping must be a JSON object: %v", ErrEdgeRejected, err)
	}
	if probe == nil {
		// JSON null = 「没有模板」= 与字段缺省同义（与 literals 同一约定）。
		return json.RawMessage(EdgeMappingEmpty), nil
	}
	obj, ok := probe.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: mapping must be a JSON object, got %s", ErrEdgeRejected, JSONTypeName(probe))
	}
	if len(obj) == 0 {
		// 空对象也归一成 '{}'（不保留用户的白空格）：库里只有一种「没有模板」的写法。
		return json.RawMessage(EdgeMappingEmpty), nil
	}
	if !fromIsSource {
		return nil, fmt.Errorf("%w: mapping 只允许出现在入口边（fromIsSource=true）："+
			"能力边的参数由 emit 的 data 提供，怎么拼是实现的内部事务", ErrEdgeRejected)
	}

	props, err := paramsSchemaProperties(to)
	if err != nil {
		return nil, err
	}
	// 按名字排序后逐个判：同一个坏输入永远报同一个错（与 checkEdgeLiterals 同口径）
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec, declared := props[k]
		if !declared {
			return nil, fmt.Errorf("%w: mapping.%s: %s@%d params_schema has no such field (declared: %v)",
				ErrEdgeRejected, k, to.Name, to.Version, sortedFields(props))
		}
		if msg := mappingValueError(spec, obj[k], "mapping."+k); msg != "" {
			return nil, fmt.Errorf("%w: %s", ErrEdgeRejected, msg)
		}
	}
	// 原样返回用户给的字节：写进库的就是它，读出来也一字不差。
	return raw, nil
}

// mappingValueError 判一个模板值的形状，返回不合法原因（"" = 通过）。
//
// 四种形态，判定顺序即口径（与 route 的 ValidateParamTemplates / ResolveParams 对齐）：
//
//	① 字符串含完整占位符  → 模板：只查占位符语法，不做类型判定
//	② 字符串无占位符       → 常量：查占位符残骸（半截 `{{`）后按 schema 判类型
//	③ 映射声明 {from}/{var} → 模板：查 from/var/as 的形状，不做类型判定
//	④ 其余（数字/布尔/数组/对象/null）→ 常量：按 schema 判类型
func mappingValueError(spec any, v any, path string) string {
	switch t := v.(type) {
	case string:
		if msg := mappingPlaceholderError(path, t); msg != "" {
			return msg
		}
		if mappingPlaceholderRE.MatchString(t) {
			return "" // 引用模板：类型要触发时求值后才知道
		}
		return matchDeclaredType(spec, v, path)
	case map[string]any:
		_, hasFrom := t["from"]
		_, hasVar := t["var"]
		// 互斥错优先（与 route.ValidateParamTemplates 的顺序一致）：
		// from+var 并存时，即使这个对象因为别的原因也不是映射声明，病因仍然是"只能选一个"。
		if hasFrom && hasVar {
			return fmt.Sprintf("%s 同时写了 from 和 var，只能选一个", path)
		}
		if !isMappingDecl(t) {
			// 只写了可选键（{default} / {as}）的对象明显是想写映射却缺了真正引用的东西；
			// 其余对象是字面量，按常量判类型。
			if msg := incompleteMappingError(path, t); msg != "" {
				return msg
			}
			return matchDeclaredType(spec, v, path)
		}
		if hasVar {
			name, isStr := t["var"].(string)
			if !isStr || name == "" {
				return fmt.Sprintf("%s 的变量名为空", path)
			}
			// 变量是否**已声明**只能由持有 config.vars 的调用方判（server 层的
			// validateTargetParams）。store 看不到源的变量声明，所以这里只判形状。
		}
		if hasFrom {
			from, isStr := t["from"].(string)
			if !isStr || invalidTemplatePath(from) {
				return fmt.Sprintf("%s 的 from 路径不合法：「%v」", path, t["from"])
			}
		}
		if asRaw, ok := t["as"]; ok {
			as, isStr := asRaw.(string)
			if !isStr || !validTemplateAs(as) {
				return fmt.Sprintf("%s 的 as 只能是 string/number/boolean/json", path)
			}
		}
		return ""
	default:
		return matchDeclaredType(spec, v, path)
	}
}

// isMappingDecl 判断一个对象是不是映射声明（旧形态，向后兼容）。
//
// 形状（契约 §3）：**恰好**含 `from` / `var` 之一，其余键只能是 `default` / `as`；
// 多一个键就不是映射声明（当字面量对象处理）。`from` / `var` 的值必须是字符串
// ——它是路径/变量名，`{var:1}` 这种写法不是声明。
//
// 与 route.IsMapping 逐条一致：判定规则只该有一份语义，两份实现必须同步。
func isMappingDecl(m map[string]any) bool {
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

// incompleteMappingError 处理「有映射的可选键、却没有 from/var」的对象。
//
// 空对象是**字面量**（`{"opts": {}}` 完全合法），不是缺引用的映射声明。
func incompleteMappingError(field string, m map[string]any) string {
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
	return fmt.Sprintf("%s 的映射声明缺少 from 或 var", field)
}

// mappingPlaceholderError 做占位符的**闭合检查**（配置期），返回不合法原因。
//
// 三段判断（顺序即优先级，与 route.placeholderFormError 逐条一致）：
//
//  1. 连 `{{` 都没有 → 直接放行。只含 `}}` 是合法字面量：
//     `{"a":{"b":1}}` 这种当字面量传的 JSON 片段必须能保存。
//  2. `{{` 与 `}}` 出现次数不等 → 半截占位符（`"PR {{title"`），原因是「没写全」。
//  3. 次数相等还不够：`{{x + 1}}` / `{{f(x)}}` / `{{.a}}` 也是 1:1，但中间不是合法
//     占位符。把完整占位符抹掉后剩下的文本里不能再有 `{{` 或 `}}`——这时原因是
//     「写了但不合法」，必须换文案，否则用户照着「数量不一致」永远修不好。
//
// 最后再查每个**完整**占位符的路径形态（`a..b` / 首尾点）。
func mappingPlaceholderError(field, s string) string {
	opens := strings.Count(s, "{{")
	closes := strings.Count(s, "}}")
	if opens == 0 {
		return ""
	}
	if opens != closes {
		return fmt.Sprintf("%s：占位符没有闭合（{{ 与 }} 数量不一致）", field)
	}

	var rest strings.Builder
	last := 0
	for _, loc := range mappingPlaceholderRE.FindAllStringIndex(s, -1) {
		rest.WriteString(s[last:loc[0]])
		last = loc[1]
	}
	rest.WriteString(s[last:])
	if r := rest.String(); strings.Contains(r, "{{") || strings.Contains(r, "}}") {
		return fmt.Sprintf("%s：占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）", field)
	}
	for _, m := range mappingPlaceholderRE.FindAllStringSubmatch(s, -1) {
		if invalidTemplatePath(m[1]) {
			return fmt.Sprintf("%s 的路径不合法：「%s」", field, m[1])
		}
	}
	return ""
}

// invalidTemplatePath 判断一个取值路径的**形态**是否合法（与 route.invalidRefPath 同口径）。
//
// 只拦三种明显的坏形态（`..` / 首 `.` / 尾 `.`）：它们的语义永远是「这里少写了一段」。
// 其它拼错（`pull_requst.title`）只有触发时才知道。
func invalidTemplatePath(p string) bool {
	return p == "" || strings.Contains(p, "..") || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".")
}

// validTemplateAs 判断配置期允许的 `as` 取值（设计 §3.1：只有四个）。
func validTemplateAs(as string) bool {
	switch as {
	case "string", "number", "boolean", "json":
		return true
	}
	return false
}
