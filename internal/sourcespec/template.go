package sourcespec

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 参数模板（source.md §4.1 / D1 / D11）。
//
// 唯一合法形态是 `$(payload)` 或 `$(payload.点号路径)`：
//
//	env:    $(payload.environment)                       # 整值只有一个引用 → 保留原类型
//	report: reports/$(payload.environment)/$(payload.x)  # 掺了常量 → 结果一定是字符串
//
// 设计边界（照抄 §4.1，别在这里放宽）：
//   - 没有算术、没有函数、没有条件、没有默认值、没有管道；
//   - 根名只认 `payload`；**其它根名一律字面文本**（`$(event.x)` 不是错误）；
//   - `$(vars.…)` / `$(params.…)` 直接拒——那两个根名只在能力 `workDir` 里有意义，
//     写在这里一定是弄错了上下文，不能静默当字面；
//   - 以 `payload` 开头却写不完整（`$(payload.a-b)`、`$(payload.`、`$(payload..x)`）
//     创建期直接拒；
//   - 路劲取不到 → 剪枝 `param_missing`，**不静默给空**（默认值归能力参数 schema）。

// templateToken 是一段模板里的一个原子：要么是字面量，要么是一个 payload 引用。
type templateToken struct {
	// literal 非空表示这段是字面量（reference == nil）。
	literal string
	// reference 是引用路径（不含 `$(...)` 包装），如 "payload.commit.sha"。
	reference string
}

// payloadRoot 是唯一被解析的根名。
const payloadRoot = "payload"

// ValidateParamsTemplate 校验一份参数模板的**语法**（不校验路径取不取得到）。
//
// 递归整个 params 对象：值里任何字符串都可能含 `$(…)`；对象/数组里的字符串同样校
// （模板就是参数值，嵌套不改变这一事实）。
func ValidateParamsTemplate(params map[string]any) error {
	return walkTemplate(params, "")
}

// walkTemplate 递归校验 params 的每个字符串值。
func walkTemplate(v any, path string) error {
	switch val := v.(type) {
	case map[string]any:
		for _, key := range SortedKeys(val) {
			sub := path + "." + key
			if path == "" {
				sub = key
			}
			if err := walkTemplate(val[key], sub); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for i, item := range val {
			if err := walkTemplate(item, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	case string:
		if _, err := parseTemplate(val); err != nil {
			if path == "" {
				return err
			}
			return errf("参数 %s：%v", path, err)
		}
		return nil
	default:
		// 数字/布尔/null 是常量，没有模板可解。
		return nil
	}
}

// parseTemplate 把一串参数值拆成字面量与引用的序列。
//
// 只认 `$(`…`)`；`{{…}}` 是包层模板的定界符（装包时一次性组装），在这里就是普通文本
// ——不解析、不报错（§4.1）。闭合缺失的 `$(` 只有在**看起来像 payload 引用**时才拒：
// 为避免把 `echo $(date` 这类普通文本判错，判据是「`$(` 之后紧邻 payload 或 payload.」。
func parseTemplate(s string) ([]templateToken, error) {
	out := []templateToken{}
	var lit strings.Builder
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], "$(")
		if j < 0 {
			lit.WriteString(s[i:])
			break
		}
		j += i
		lit.WriteString(s[i:j])
		k := strings.Index(s[j:], ")")
		if k < 0 {
			// 没有闭合：只有「像 payload 引用」才拒，其余整段当字面量。
			if startsLikePayloadRef(s[j+2:]) {
				return nil, errf("引用 %q 没有闭合", s[j:])
			}
			lit.WriteString(s[j:])
			break
		}
		k += j
		inner := s[j+2 : k]
		cleaned, isRef, err := classifyRef(inner)
		if err != nil {
			return nil, err
		}
		if !isRef {
			// 不是我们解析的引用：原样保留（§4.1「其余 $(…) 一律字面」）。
			lit.WriteString(s[j : k+1])
			i = k + 1
			continue
		}
		if lit.Len() > 0 {
			out = append(out, templateToken{literal: lit.String()})
			lit.Reset()
		}
		out = append(out, templateToken{reference: cleaned})
		i = k + 1
	}
	if lit.Len() > 0 {
		out = append(out, templateToken{literal: lit.String()})
	}
	return out, nil
}

// startsLikePayloadRef 报告 `$(` 之后的内容是否**看起来是**一个 payload 引用。
//
// 判据取「以 payload 开头」：`$(payload.` 与 `$(payloadx)` 都会命中——后者是
// 以 payload 开头的普通文本，但闭合缺失时把它当字面量又会掩盖真实的笔误，
// 所以宁可在创建期报一句「引用没有闭合」（比静默留下一个永不解析的串好）。
func startsLikePayloadRef(rest string) bool {
	return strings.HasPrefix(rest, payloadRoot)
}

// classifyRef 判一个 `$(…)` 的内容该怎么处理：
//
//	("payload" / "payload.a.b", true, nil)   → 引用
//	("", false, nil)                          → 字面量（别的根名/普通文本）
//	(err)                                     → 形状错误（被禁根名、坏路径）
func classifyRef(inner string) (string, bool, error) {
	name := strings.TrimSpace(inner)
	if name == payloadRoot {
		return payloadRoot, true, nil
	}
	if strings.HasPrefix(name, payloadRoot+".") {
		if err := validateRefPath(name); err != nil {
			return "", false, err
		}
		return name, true, nil
	}
	// 被禁的根名：`vars.*` / `params.*` 只在能力 workDir 里有意义。
	for _, banned := range []string{"vars", "params"} {
		if name == banned || strings.HasPrefix(name, banned+".") {
			return "", false, errf("参数里不能引用 %q（只有能力 workDir 认 %s.*）", "$("+name+")", banned)
		}
	}
	// 以 payload 开头但不是合法路径（`payload-x`、`payloadx.a`）：这不是「别的根名」，
	// 而是一个一眼看着像引用却永远不会解析的串——创建期拒掉。
	if strings.HasPrefix(name, payloadRoot) {
		return "", false, errf("引用 %q 不是合法的 payload 路径（只认 $(payload) 与 $(payload.点号路径)）", "$("+name+")")
	}
	return "", false, nil
}

// validateRefPath 校验 payload 点号路径的语法。
func validateRefPath(path string) error {
	segs := strings.Split(path, ".")
	if len(segs) < 2 || segs[0] != payloadRoot {
		return errf("引用 %q 不是合法的 payload 路径", "$("+path+")")
	}
	for _, seg := range segs[1:] {
		if !validPathSegment(seg) {
			return errf("引用 %q 的路径段 %q 不合法（只允许字母/数字/下划线，数字段表示数组下标）",
				"$("+path+")", seg)
		}
	}
	return nil
}

// validPathSegment 是路径段字符集：标识符或纯数字（数组下标）。
//
// 连字符明确拒绝（`$(payload.a-b)` 是文档点名的创建期错误）：
// 它既可能是「减号」也可能是「键名里真有连字符」，两种解释都会让用户猜——拒掉最省事。
func validPathSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9':
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		default:
			return false
		}
	}
	return true
}

// RenderParams 按一条载荷求值参数模板。
//
// 返回：
//   - params：求值后的参数（**只含取得到的项**）；
//   - missing：求值失败**参数名**（升序、去重）。缺任何一个引用的参数整条丢弃，
//     调用方据此留一条 param_missing 剪枝痕——不静默给空串（§4.1）。
//
// 类型规则只由一件事决定（§4.1）：整个值**恰好只有一个引用** → 保留原类型；
// 其余（掺了任何常量，哪怕只是首尾空格）→ 字符串拼接。
func RenderParams(target Target, payload map[string]any) (map[string]any, []string) {
	out := make(map[string]any, len(target.Params))
	missing := []string{}
	for _, key := range SortedKeys(target.Params) {
		value, ok := renderValue(target.Params[key], payload)
		if !ok {
			missing = append(missing, key)
			continue
		}
		out[key] = value
	}
	return out, missing
}

// renderValue 求值一个参数值；ok=false 表示有引用取不到。
func renderValue(v any, payload map[string]any) (any, bool) {
	switch val := v.(type) {
	case string:
		return renderString(val, payload)
	case map[string]any:
		out := make(map[string]any, len(val))
		for _, key := range SortedKeys(val) {
			sub, ok := renderValue(val[key], payload)
			if !ok {
				return nil, false
			}
			out[key] = sub
		}
		return out, true
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			sub, ok := renderValue(item, payload)
			if !ok {
				return nil, false
			}
			out[i] = sub
		}
		return out, true
	default:
		return v, true // 常量
	}
}

// renderString 求值一个字符串参数（整值引用保类型 / 混写拼字符串）。
func renderString(s string, payload map[string]any) (any, bool) {
	tokens, err := parseTemplate(s)
	if err != nil {
		// 校验过得去的模板不会在这里失败；真失败按「取不到」处理（保守）。
		return nil, false
	}
	hasRef := false
	for _, tk := range tokens {
		if tk.reference != "" {
			hasRef = true
			break
		}
	}
	if !hasRef {
		return s, true
	}
	// 整值只有一个引用 → 原类型。
	if len(tokens) == 1 && tokens[0].reference != "" {
		return lookupPath(payload, tokens[0].reference)
	}
	var b strings.Builder
	for _, tk := range tokens {
		if tk.reference == "" {
			b.WriteString(tk.literal)
			continue
		}
		val, ok := lookupPath(payload, tk.reference)
		if !ok {
			return nil, false
		}
		b.WriteString(stringify(val))
	}
	return b.String(), true
}

// lookupPath 按点号路径取值；任一段取不到 → ok=false。
//
// 数字段在数组上当下标用（D11）；在对象上就是普通的数字键。
func lookupPath(payload map[string]any, path string) (any, bool) {
	segs := strings.Split(path, ".")
	if len(segs) == 0 || segs[0] != payloadRoot {
		return nil, false
	}
	var cur any = payload
	for _, seg := range segs[1:] {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// stringify 把引用值转成拼接用的文本。
//
// 对象/数组用 compact JSON（可预测、可还原），字符串原样，其余按 JSON 字面量
// （数字保留原始写法，true/false/null 与 JSON 一致）。null 拼成空串：
// 「有个字段但它是 null」与「字段缺值」在拼接语境里给同一结果，不会拼出 "<nil>"。
func stringify(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case json.Number:
		return val.String()
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
