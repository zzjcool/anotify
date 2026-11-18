package yamldoc

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// jsonValue 把（已校验过的）YAML 子树转成 encoding/json 能编码的值。
//
// 为什么不用 `node.Decode(&any)`：那条路会把标量解成 Go 值，再交给 json.Marshal，
// 于是浮点走回 `1e+09` 记法（就是本仓修过的那个 bug）。这里自己控标量：
// 整数字面量原文照抄，浮点按需归一成十进制，字符串原样。
func (w *walker) jsonValue(n *yaml.Node, path string) any {
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			m[key] = w.jsonValue(n.Content[i+1], joinPath(path, key))
		}
		return m
	case yaml.SequenceNode:
		// 长度 0 也必须是 `[]` 而不是 `null`（仓库红线：空列表返回 []）。
		out := make([]any, len(n.Content))
		for i, c := range n.Content {
			out[i] = w.jsonValue(c, indexPath(path, i))
		}
		return out
	case yaml.ScalarNode:
		return w.scalarValue(n, path)
	}
	// 别名 / 文档节点：拒绝集已挡掉，正常到不了这里。
	return nil
}

// scalarValue 把一个标量转成 JSON 值（json.Number 保留数字字面量）。
func (w *walker) scalarValue(n *yaml.Node, path string) any {
	switch n.ShortTag() {
	case yamlNullTag:
		return nil
	case yamlBoolTag:
		var b bool
		if err := n.Decode(&b); err == nil {
			return b
		}
		return n.Value
	case yamlIntTag:
		return w.intValue(n, path)
	case yamlFloatTag:
		return w.floatValue(n, path)
	default:
		// !!str 原样；!!timestamp 按字符串（JSON 没有时间类型，
		// 下游字段是 string，改成 RFC3339 反而会改值）。
		return n.Value
	}
}

// intValue 处理 !!int。
func (w *walker) intValue(n *yaml.Node, path string) any {
	lit := n.Value
	// 十进制字面量原文照抄——这是「1700000000 不能变成 1.7e+09」的唯一保证。
	// 走 Decode(&int64) 再格式化对 2^53 内的值也等价，但中间类型一旦被改成
	// float64（本仓真这么错过一次），超过 2^53 的 id / 时间戳就会静默改值。
	//
	// 与 floatValue 对称的判断：**匹配 `-?[0-9]+` 不等于「合法 JSON number」**。
	// `010` / `00` / `-010` / `01067` 这类带前导零的写法在 YAML 里是八进制
	// （yaml.v3：010 → 8、01067 → 567），原文照抄会让 encoding/json 在编码阶段报
	// `json: invalid number literal "01067"` —— 一份合法 YAML 被 400 拒掉。
	// 所以只照抄同时满足 JSON number 语法的字面量（RFC 8259 不允许前导零）；
	// 其余回落到下面的 Decode(&int64)，输出 yaml.v3 解出的十进制值。
	//
	// 为什么不用「去掉前导零」的字符串变换代替回落：那会把 010 变成 10，**改值**。
	// 为什么 plainIntPattern 也不能省：isJSONNumber 是 JSON 语法判据，它会放行
	// `1e3`（`!!int 1e3` 这种显式标 int 的写法），而 `1e3` 进 JSON 后解不进 int64
	// 目标字段——必须由 plainIntPattern 先把它挡到回落分支里。
	if plainIntPattern.MatchString(lit) && isJSONNumber(lit) {
		return json.Number(lit)
	}
	var i int64
	if err := n.Decode(&i); err == nil {
		return json.Number(strconv.FormatInt(i, 10))
	}
	// 超出 int64 但还在 uint64 内的写法（`0xFFFFFFFFFFFFFFFF`）：
	// 解成 int64 会越界，但十进制字面量仍能无损表达，别把它判死。
	if u, err := strconv.ParseUint(strings.ReplaceAll(lit, "_", ""), 0, 64); err == nil {
		return json.Number(strconv.FormatUint(u, 10))
	}
	// `!!int 1e3`（显式标 int 但字面量是浮点）：yaml.v3 的 Decode 会在 tag 推导阶段
	// 就报 `cannot decode !!float 1e3 as a !!int`（连解成 float64 都不行），
	// 所以这里直接解析字面量，再转成十进制——契约要求这类写法也输出十进制。
	if f, ok := parseFloatLiteral(lit); ok {
		return json.Number(formatFloat(f))
	}
	return w.badNumber(n, path)
}

// floatValue 处理 !!float。
func (w *walker) floatValue(n *yaml.Node, path string) any {
	lit := n.Value
	// 合法且不带指数的 JSON number → 原文照抄（1.5 / 1.50 保形）。
	// 带指数的要归一成十进制：JSON 语法允许 `1e9`，但下游字段可能是整数
	// （encoding/json 拒绝把 `1e9` 解进 int64），而 Go 的 json.Marshal(float64)
	// 又会输出 `1e+09` —— 两头都不安全，十进制是唯一两边都能吃的写法。
	if isJSONNumber(lit) && !strings.ContainsAny(lit, "eE") {
		return json.Number(lit)
	}
	if f, ok := parseFloatLiteral(lit); ok {
		return json.Number(formatFloat(f))
	}
	return w.badNumber(n, path)
}

// badNumber 记一条 fatal 问题并返回 nil（占位，不参与任何成功路径）。
//
// `.inf` / `.nan` 这类值在 JSON 里没有字面量，也没有无损替代（`null` 会改语义），
// 所以只能拒。消息不带字面量（契约 §1.7）。
func (w *walker) badNumber(n *yaml.Node, path string) any {
	w.numProblems = append(w.numProblems, Problem{
		Reason:  reasonValueInvalid,
		Path:    path,
		Line:    n.Line,
		Message: "数值无法表示成 JSON 数字（只支持十进制有限值）",
	})
	return nil
}

// parseFloatLiteral 解析 YAML 浮点/整数标量字面量（YAML 允许 `_` 分隔符），
// 只接受有限值：JSON 没有 Inf / NaN 字面量。
func parseFloatLiteral(lit string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(lit, "_", ""), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// formatFloat 用 'f' 记法（无指数）输出最短往返表示。
func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// isJSONNumber 判断字面量是否是合法 JSON number（RFC 8259 语法）。
func isJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[i] == '-' {
		i++
		if i == len(s) {
			return false
		}
	}
	switch {
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if i == len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i == len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	return i == len(s)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
