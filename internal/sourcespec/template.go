package sourcespec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 参数模板：`$(payload)` / `$(payload.点号路径)`，outcome 动作另可写 `$(decision.点号路径)`（#522）。
// 没有算术/函数/条件/默认值/管道；`$(vars.…)`/`$(params.…)` 拒；写不完整（`$(payload.a-b)`）创建期拒；
// 路径取不到 → 剪枝 `param_missing`，不静默给空。求值两段：建任务求 payload、判决执行求 decision。

// templateToken 是一段模板里的一个原子：要么是字面量，要么是一个 payload 引用。
type templateToken struct {
	// literal 非空表示这段是字面量（reference == nil）。
	literal string
	// reference 是引用路径（不含 `$(...)` 包装），如 "payload.commit.sha"。
	reference string
}

// payloadRoot 是事件载荷的根名（§4.1 的唯一根名）。
const payloadRoot = "payload"

const decisionRoot = "decision"

var (
	payloadRoots = []string{payloadRoot}
	actionRoots  = []string{payloadRoot, decisionRoot}
)

// 递归整个 params 对象：值里任何字符串都可能含 `$(…)`；对象/数组里的字符串同样校
// （模板就是参数值，嵌套不改变这一事实）。
func ValidateParamsTemplate(params map[string]any) error {
	return walkTemplate(params, "", payloadRoots)
}

func ValidateActionParamsTemplate(params map[string]any) error {
	return walkTemplate(params, "", actionRoots)
}

func walkTemplate(v any, path string, roots []string) error {
	switch val := v.(type) {
	case map[string]any:
		for _, key := range SortedKeys(val) {
			sub := path + "." + key
			if path == "" {
				sub = key
			}
			if err := walkTemplate(val[key], sub, roots); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for i, item := range val {
			if err := walkTemplate(item, path+"["+strconv.Itoa(i)+"]", roots); err != nil {
				return err
			}
		}
		return nil
	case string:
		if _, err := parseTemplateWith(val, roots); err != nil {
			if path == "" {
				return err
			}
			return errf("参数 %s：%v", path, err)
		}
		return rejectUnownedRoot(val, path, roots)
	default:
		// 数字/布尔/null 是常量，没有模板可解。
		return nil
	}
}

func rejectUnownedRoot(value, path string, roots []string) error {
	for _, root := range roots {
		if root == decisionRoot {
			return nil
		}
	}
	tokens, err := parseTemplateWith(value, actionRoots) // 全根名才看得见 decision 引用
	if err != nil {
		return nil // 语法错上面已报过
	}
	for _, tok := range tokens {
		if tok.reference == decisionRoot || strings.HasPrefix(tok.reference, decisionRoot+".") {
			where := "参数 " + path
			if path == "" {
				where = "该值"
			}
			return errf("%s 不能引用 $(%s)：$(%s.…) 只能出现在 outcomes 动作里", where, tok.reference, decisionRoot)
		}
	}
	return nil
}

// parseTemplateWith 拆字面量与引用（只认 `$(`…`)`）；闭合缺失只有像根名引用时才拒。
func parseTemplateWith(s string, roots []string) ([]templateToken, error) {
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
			if startsLikeRef(s[j+2:], roots) {
				return nil, errf("引用 %q 没有闭合", s[j:])
			}
			lit.WriteString(s[j:])
			break
		}
		k += j
		inner := s[j+2 : k]
		cleaned, isRef, err := classifyRefWith(inner, roots)
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

func startsLikeRef(rest string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(rest, root) {
			return true
		}
	}
	return false
}

// classifyRefWith 判一个 `$(…)` 的内容：引用 / 字面量（别的根名） / 形状错误（被禁根名、坏路径）。
func classifyRefWith(inner string, roots []string) (string, bool, error) {
	name := strings.TrimSpace(inner)
	for _, root := range roots {
		if name == root {
			return root, true, nil
		}
		if strings.HasPrefix(name, root+".") {
			if err := validateRefPath(name, root); err != nil {
				return "", false, err
			}
			return name, true, nil
		}
	}
	// 被禁的根名：`vars.*` / `params.*` 只在能力 workDir 里有意义。
	for _, banned := range []string{"vars", "params"} {
		if name == banned || strings.HasPrefix(name, banned+".") {
			return "", false, errf("参数里不能引用 %q（只有能力 workDir 认 %s.*）", "$("+name+")", banned)
		}
	}
	// 以某个允许根名开头但不是合法路径（`payload-x`、`payloadx.a`）：这不是「别的根名」，
	// 而是一个一眼看着像引用却永远不会解析的串——创建期拒掉。
	for _, root := range roots {
		if strings.HasPrefix(name, root) {
			return "", false, errf("引用 %q 不是合法的 %s 路径（只认 $(%s) 与 $(%s.点号路径)）",
				"$("+name+")", root, root, root)
		}
	}
	return "", false, nil
}

func validateRefPath(path, root string) error {
	segs := strings.Split(path, ".")
	if len(segs) < 2 || segs[0] != root {
		return errf("引用 %q 不是合法的 %s 路径", "$("+path+")", root)
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
// 连字符明确拒绝（`$(payload.a-b)` 是文档点名的创建期错误）：它既可能是「减号」也可能是「键名里真有连字符」，两种解释都会让用户猜——拒掉最省事。
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

// RenderParams 按一条载荷求值 target 参数模板：params 只含取得到的项，missing 是失败参数名
// （升序去重）。缺任何引用的参数整条丢弃（调用方留 param_missing 剪枝痕）。类型规则：整个值**恰好只有一个引用** → 保留原类型；其余 → 字符串拼接。
func RenderParams(target Target, payload map[string]any) (map[string]any, []string) {
	return renderParamsWith(target.Params, renderRoots{payloadRoot: payload})
}

// 结局动作里的 `$(decision.…)` 此刻**不展开**（判决还没发生），原样留在渲染结果里，
// 由 RenderDecisionParams 在判决执行时补齐——这是两段求值的唯一约定。
func RenderTarget(t Target, payload map[string]any) (map[string]any, map[string][]OutcomeAction, []string) {
	roots := renderRoots{payloadRoot: payload}
	params, missing := renderParamsWith(t.Params, roots)
	if len(t.Outcomes) == 0 {
		return params, nil, missing
	}
	outcomes := make(map[string][]OutcomeAction, len(t.Outcomes))
	names := make([]string, 0, len(t.Outcomes))
	for name := range t.Outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		actions := t.Outcomes[name]
		rendered := make([]OutcomeAction, 0, len(actions))
		for i, action := range actions {
			actionParams, gaps := renderParamsWith(action.Params, roots)
			for _, gap := range gaps {
				missing = append(missing, fmt.Sprintf("outcomes.%s[%d].params.%s", name, i, gap))
			}
			rendered = append(rendered, OutcomeAction{Capability: action.Capability, Params: actionParams})
		}
		outcomes[name] = rendered
	}
	sort.Strings(missing)
	return params, outcomes, missing
}

// RenderDecisionParams 用一次判决的 params 补齐动作模板里的 `$(decision.…)` 引用（#522）。
//
// 判决执行时 payload 段已经求完（RenderTarget），所以这里只有 decision 根名在手上；
// 取不到 → 该动作按 param_missing 剪枝（不静默丢字段）。
func RenderDecisionParams(params map[string]any, decision map[string]any) (map[string]any, []string) {
	return renderParamsWith(params, renderRoots{decisionRoot: decision})
}

// renderRoots 是一次求值**手上已有**的根名 → 文档。
//
// 不在表里的根名表示「还没产生」（而不是「取不到」）：保留引用原文，交给下一段求值。
type renderRoots map[string]map[string]any

// renderParamsWith 是 RenderParams 的根名参数化版本。
func renderParamsWith(params map[string]any, roots renderRoots) (map[string]any, []string) {
	out := make(map[string]any, len(params))
	missing := []string{}
	for _, key := range SortedKeys(params) {
		value, ok := renderValue(params[key], roots)
		if !ok {
			missing = append(missing, key)
			continue
		}
		out[key] = value
	}
	return out, missing
}

// renderValue 求值一个参数值；ok=false 表示有引用取不到。
func renderValue(v any, roots renderRoots) (any, bool) {
	switch val := v.(type) {
	case string:
		return renderString(val, roots)
	case map[string]any:
		out := make(map[string]any, len(val))
		for _, key := range SortedKeys(val) {
			sub, ok := renderValue(val[key], roots)
			if !ok {
				return nil, false
			}
			out[key] = sub
		}
		return out, true
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			sub, ok := renderValue(item, roots)
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

// renderString 求值一个字符串参数（整值引用保类型 / 混写拼字符串 / 未产生的根名原样保留）。
func renderString(s string, roots renderRoots) (any, bool) {
	tokens, err := parseTemplateWith(s, actionRoots)
	if err != nil {
		// 校验过得去的模板不会在这里失败；真失败按「取不到」处理（保守）。
		return nil, false
	}
	// 整值只有一个引用且这个根名在手上 → 原类型。
	if len(tokens) == 1 && tokens[0].reference != "" {
		doc, available := roots[rootOf(tokens[0].reference)]
		if !available {
			return s, true
		}
		return lookupPath(doc, tokens[0].reference)
	}
	var b strings.Builder
	resolved := false
	for _, tk := range tokens {
		if tk.reference == "" {
			b.WriteString(tk.literal)
			continue
		}
		doc, available := roots[rootOf(tk.reference)]
		if !available {
			// 还没产生的根名：按原文写回，交给下一段求值。
			b.WriteString("$(" + tk.reference + ")")
			continue
		}
		val, ok := lookupPath(doc, tk.reference)
		if !ok {
			return nil, false
		}
		resolved = true
		b.WriteString(stringify(val))
	}
	if !resolved {
		// 一个可用引用都没有：原样返回（保留用户写的原文，包括里面的空格）。
		return s, true
	}
	return b.String(), true
}

// rootOf 取一个引用路径的根名段（`payload.a.b` → `payload`）。
func rootOf(path string) string {
	root, _, _ := strings.Cut(path, ".")
	return root
}

// lookupPath 按点号路径取值（第一段是根名，调用方已按根名选好文档）；任一段取不到 → false。
//
// 数字段在数组上当下标用（D11）；在对象上就是普通的数字键。路径走 walkPath，
// 与 envelope 的指针共用同一份实现。
func lookupPath(doc map[string]any, path string) (any, bool) {
	segs := strings.Split(path, ".")
	if len(segs) == 1 {
		return doc, true
	}
	return walkPath(doc, segs[1:])
}

// walkPath 沿段列表下行（数字段在数组上是下标，在对象上是数字键）。
func walkPath(cur any, segs []string) (any, bool) {
	for _, seg := range segs {
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
// （数字保留原始写法，true/false/null 与 JSON 一致）。null 拼成空串：「有个字段但它是 null」与「字段缺值」在拼接语境里给同一结果，不会拼出 "<nil>"。
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
