// Package yamldoc 把一份单文档 YAML 严格转换成 canonical JSON，并给出带行号 / 字段路径的问题定位。
//
// 它服务于「从 YAML 创建资源」的写端点（`POST …/capabilities/yaml`、`POST …/sources/yaml`，
// 见 docs/design/v4 的 yaml-create 契约）：
//
//	POST 的 JSON body 里带一整份 YAML 正文 → yamldoc.Decode(text, shape)
//	→ Result.JSON 走现有 JSON 解码路径（严格解码进 spec 结构体）
//	→ 语义校验仍由 store / artifact / sourcespec 负责（本包不做任何业务判断）。
//
// 三条设计约束（改之前先读）：
//
//  1. **叶子包**：只依赖 gopkg.in/yaml.v3 + 标准库。server 与 playbook 将来都要用它，
//     所以不得 import internal/store、internal/artifact、internal/server、internal/sourcespec
//     （分层倒挂 + import 循环都会从这一行开始烂）。
//  2. **数字字面量保真**：`1700000000` 必须原样进 JSON，不能变成 `1.7e+09`。
//     本仓真出过这个 bug（`internal/server/resource_source.go` 的时间戳注释），
//     根因是「先解成 float64 再编码」这条路径会改写整数字面量。
//  3. **错误消息不带标量值**：只带字段路径 +「期望 / 实际类型」。
//     理由是 YAML 里有 `spec.config.token` / `secret` 这类凭据，错误消息会被原样回给前端
//     并可能进日志；把标量拼进消息等于开了一条凭据回显通道（契约 §1.7 红线）。
//
// 刻意不做的（与契约 §9 的 D1/D2 一致）：多文档导入、锚点 / 别名展开、自定义 tag、
// 非字符串 mapping 键。这些都是**拒绝集**：一份文档只报一条，带行号。
package yamldoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 问题短码：封闭集合，服务端原样填进 `Status.details.causes[].reason`。
const (
	reasonInvalidYAML  = "invalid_yaml"
	reasonUnknownField = "unknown_field"
	reasonValueInvalid = "value_invalid"
)

// Problem 是一条带定位的问题。Line 是 1-based；Path 为空表示文档级问题。
type Problem struct {
	Reason  string // invalid_yaml | unknown_field | value_invalid
	Path    string // "spec.run.fils" / "spec.targets[0].capability" / ""
	Line    int    // 1-based；0 = 无法定位
	Message string // 人话（不含标量值）
}

// String 渲染成 `第 7 行 spec.run.fils：未知字段「fils」（允许：…）`。
func (p Problem) String() string {
	switch {
	case p.Line > 0 && p.Path != "":
		return fmt.Sprintf("第 %d 行 %s：%s", p.Line, p.Path, p.Message)
	case p.Line > 0:
		return fmt.Sprintf("第 %d 行：%s", p.Line, p.Message)
	case p.Path != "":
		return fmt.Sprintf("%s：%s", p.Path, p.Message)
	default:
		return p.Message
	}
}

// Error 汇总一份文档上的全部问题（一次报全部，与 artifact.Error 同口径）。
//
// 只有「结构问题」（未知键、类型不符、必填缺失）会被攒成多条；
// 语法错 / 拒绝集 / 无法表示成 JSON 的数字是 fatal，一份文档只有一条
// （列全它们没有意义：第一处坏掉之后的位置信息不再可信）。
type Error struct{ Problems []Problem }

// Error 多条问题用 "\n" 连接（Problem.String 已带行号与路径）。
func (e *Error) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// Reason 取第一条问题的 reason（服务端把它当顶层 `Status.reason`）。
func (e *Error) Reason() string {
	if len(e.Problems) == 0 {
		return ""
	}
	return e.Problems[0].Reason
}

// Causes 供 handler 当作 `Status.details.causes` 直接下发（与契约 §1.6 的 1:1 映射）：
// Field 是点号路径，Message 带「第 N 行：」前缀且不含路径（路径在 Field 里）。
//
// 为什么前缀放在这里而不是留给服务端拼：details.causes[].message 的形状是契约冻结的
// （§1.6 的样例就是 `第 3 行：必填字段缺失`），而这个结构体的唯一用途就是下发它；
// 让两个地方都能拼，只会拼出「第 3 行：第 3 行：…」。
func (e *Error) Causes() []Cause {
	out := make([]Cause, 0, len(e.Problems))
	for _, p := range e.Problems {
		out = append(out, Cause{Reason: p.Reason, Field: p.Path, Message: p.causeMessage()})
	}
	return out
}

// causeMessage 是 details.causes[].message 的形状（带行号前缀、不带路径）。
func (p Problem) causeMessage() string {
	if p.Line > 0 {
		return fmt.Sprintf("第 %d 行：%s", p.Line, p.Message)
	}
	return p.Message
}

// Cause 是 details.causes[] 的一条（字段名与契约 §1.6 的 details.causes 一致）。
type Cause struct {
	Reason  string // invalid_yaml | unknown_field | value_invalid
	Field   string // 点号路径，可能为空（文档级问题）
	Message string // 人话，带「第 N 行：」前缀（契约 §1.6），不含标量值
}

// Shape 描述一份写请求文档允许的形状。
type Shape struct {
	// Root：顶层允许键 → 期望类型；类型为 nil 表示「存在但不检查」（如 status）。
	Root map[string]reflect.Type
	// Meta：metadata 段允许键 → 期望类型（由 objectMeta 类型 + 呼叫方的白名单裁剪）。
	// 非空时优先于 Root["metadata"] 的类型推导——白名单是**安全边界**，不能靠 struct tag 全量放行。
	Meta map[string]reflect.Type
	// Required：必须出现的点号路径，只支持 Root 层与 metadata 段（如 "metadata.name"）。
	Required []string
}

// Result 是严格解析结果。
type Result struct {
	// JSON 是 canonical JSON（键名原样保留、数字用字面量原文）；喂给现有 JSON 解码路径。
	// 确定性：对象键按字典序（map 经 encoding/json 天然排序），数组保持原序。
	JSON []byte
	// LineOf 返回某个点号路径在原文里的 1-based 行号（0 = 没有）。
	LineOf func(path string) int
}

// Decode 严格解析一份单文档 YAML。
//
// 语法错 / 多文档 / 锚点别名 / 自定义 tag / 非字符串键 / 重复键 → *Error（带行号），不返回 Result。
// 结构（未知键、类型不符、必填缺失）问题也走 *Error（一次报全部）。
func Decode(data []byte, shape Shape) (*Result, error) {
	root, ferr := singleDocument(data)
	if ferr != nil {
		return nil, ferr
	}
	// 顶层非映射属于拒绝集：没有它，后面所有路径都失去意义。
	if root.Kind != yaml.MappingNode {
		return nil, fatal(Problem{
			Reason:  reasonInvalidYAML,
			Line:    root.Line,
			Message: "顶层必须是映射，实际是" + yamlKind(root),
		})
	}
	// 拒绝集先扫全树（含 opaque 子树）：它比结构问题更「基础」——
	// 一份带别名/重复键的文档，报「未知字段」只会误导人。
	if p := rejectAt(root); p != nil {
		return nil, fatal(*p)
	}

	w := &walker{shape: shape, lines: make(map[string]int, 16)}
	w.lines[""] = root.Line
	w.walkMappingPairs(root, "", shape.Root, true)
	w.checkRequired(root, shape)
	if len(w.problems) > 0 {
		return nil, &Error{Problems: w.problems}
	}

	// 结构没问题才转值：转换期只剩「数字无法表示为 JSON」这一种 fatal 问题
	// （例如 YAML 的 .inf / .nan：JSON 没有对应字面量，也没有无损的替代写法）。
	val := w.jsonValue(root, "")
	if len(w.numProblems) > 0 {
		return nil, &Error{Problems: w.numProblems}
	}
	raw, err := marshalCanonical(val)
	if err != nil {
		return nil, fatal(Problem{Reason: reasonValueInvalid, Message: "无法序列化成 JSON：" + err.Error()})
	}
	return &Result{JSON: raw, LineOf: w.lineOf}, nil
}

// StructType 把 reflect.TypeOf(x) 规范化成可走树的类型（去指针/取元素）。
func StructType(v any) reflect.Type { return indirect(reflect.TypeOf(v)) }

func fatal(p Problem) *Error { return &Error{Problems: []Problem{p}} }

// singleDocument 取出一份且仅一份 YAML 文档的根节点。
//
// yaml.Unmarshal 只解第一份文档（`---` 之后的内容被静默丢弃），所以必须再用 Decoder
// 显式解第二份：多文档是拒绝集（契约 D1：一次只创建一份资源），
// 静默丢掉用户写的后半段比报错更糟。
func singleDocument(data []byte) (*yaml.Node, *Error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fatal(syntaxProblem(err))
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
		return nil, fatal(syntaxProblem(err))
	}
	var second yaml.Node
	switch err := dec.Decode(&second); {
	case err == nil:
		return nil, fatal(Problem{
			Reason:  reasonInvalidYAML,
			Line:    second.Line,
			Message: "文档包含多份 YAML 文档（一次只能创建一份资源）",
		})
	case !errors.Is(err, io.EOF):
		// 第二份文档本身有语法错：行号取 yaml.v3 报的位置，比第二份的起始行更准。
		return nil, fatal(syntaxProblem(err))
	}
	// 空文档：全空白 / 全是注释 → 根本没有节点。
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fatal(Problem{Reason: reasonInvalidYAML, Message: "文档为空"})
	}
	if doc.Kind != yaml.DocumentNode {
		return &doc, nil
	}
	root := doc.Content[0]
	// `null` / `~` / `---` 单独一行：yaml.v3 给出一个 !!null 空标量，按「文档为空」处理
	// （与 artifact.Parse 同措辞），否则下游只会看到「顶层必须是映射」这种间接说法。
	if root.Kind == yaml.ScalarNode && root.ShortTag() == yamlNullTag {
		return nil, fatal(Problem{Reason: reasonInvalidYAML, Line: root.Line, Message: "文档为空"})
	}
	return root, nil
}

// syntaxProblem 把 yaml.v3 的语法错翻成带行号的 Problem。
//
// yaml.v3 的语法错只有一句文本（形如 `yaml: line 3: did not find expected key`），
// 没有结构化行号，所以这里正则取第一个 `line N`。
func syntaxProblem(err error) Problem {
	line := 0
	if m := linePattern.FindStringSubmatch(err.Error()); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			line = n
		}
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	msg = linePrefixPattern.ReplaceAllString(msg, "")
	// 解析器错误只描述 token，理论上不含标量值；但 yaml.v3 的 TypeError 会带值
	// （`cannot unmarshal !!str `secret` into int`），这里统一抹掉反引号片段，
	// 避免将来解码路径变化时漏出凭据（契约 §1.7 红线）。
	msg = backtickPattern.ReplaceAllString(msg, "「…」")
	return Problem{Reason: reasonInvalidYAML, Line: line, Message: "YAML 语法错误：" + msg}
}

// yamlKind 返回节点的人话类型名（用于错误消息，绝不含值）。
func yamlKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.DocumentNode:
		return "文档"
	case yaml.MappingNode:
		return "映射"
	case yaml.SequenceNode:
		return "序列"
	case yaml.AliasNode:
		return "别名"
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case yamlStrTag, yamlTimestampTag:
			return "字符串"
		case yamlIntTag:
			return "整数"
		case yamlFloatTag:
			return "浮点数"
		case yamlBoolTag:
			return "布尔"
		case yamlNullTag:
			return "null"
		}
		return "标量"
	}
	return "未知节点"
}

// rejectAt 从根开始按文档顺序找第一个违反拒绝集的节点。
//
// 拒绝集（契约 §2 实现要点 2）：别名、锚点、自定义 tag、非字符串 mapping 键、重复键。
// 它们一律 invalid_yaml 且 fatal：不支持展开的理由是安全（billion-laughs / 环），
// 而「报一半然后继续」会让行号失去意义。
func rejectAt(n *yaml.Node) *Problem {
	if p := nodeReject(n); p != nil {
		return p
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if p := rejectAt(c); p != nil {
				return p
			}
		}
	case yaml.MappingNode:
		seen := make(map[string]int, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			// 合并键要在 tag 检查**之前**判：`<<` 的 tag 是 !!merge，
			// 先撞 tag 检查会给出「不支持的自定义标签「!!merge」」这种看不懂的说法。
			// 没有别名之后合并键也没意义（`<<: *a` 的别名已在拒绝集里被拒）。
			if k.Value == "<<" && k.ShortTag() == yamlMergeTag {
				return &Problem{
					Reason:  reasonInvalidYAML,
					Line:    k.Line,
					Message: "不支持合并键「<<」（锚点/别名一律不展开）",
				}
			}
			if p := nodeReject(k); p != nil {
				return p
			}
			if k.Kind != yaml.ScalarNode || k.ShortTag() != yamlStrTag {
				return &Problem{
					Reason:  reasonInvalidYAML,
					Line:    k.Line,
					Message: "映射的键必须是字符串，实际是" + yamlKind(k),
				}
			}
			if first, dup := seen[k.Value]; dup {
				return &Problem{
					Reason:  reasonInvalidYAML,
					Line:    k.Line,
					Message: fmt.Sprintf("重复的键「%s」（首次出现在第 %d 行）", k.Value, first),
				}
			}
			seen[k.Value] = k.Line
			if p := rejectAt(v); p != nil {
				return p
			}
		}
	}
	return nil
}

// nodeReject 检查单个节点自身的拒绝条件（别名 / 锚点 / tag）。
func nodeReject(n *yaml.Node) *Problem {
	if n.Kind == yaml.AliasNode {
		// 消息不带别名值：`*` 后面跟的是锚点名，而锚点名常常是用户随手写的路径。
		return &Problem{Reason: reasonInvalidYAML, Line: n.Line, Message: "不支持别名（* 引用）：锚点/别名一律不展开"}
	}
	if n.Anchor != "" {
		return &Problem{Reason: reasonInvalidYAML, Line: n.Line, Message: "不支持锚点（& 定义）：锚点/别名一律不展开"}
	}
	if !allowedTag(n.ShortTag()) {
		return &Problem{
			Reason:  reasonInvalidYAML,
			Line:    n.Line,
			Message: fmt.Sprintf("不支持的自定义标签「%s」（只接受 YAML 内置类型）", n.ShortTag()),
		}
	}
	return nil
}

// allowedTag 是允许的 YAML 内置 tag 集合。
//
// !!timestamp 也在内：yaml.v3 会把 `2020-01-02` 这种**裸标量**自动解析成 !!timestamp，
// 把它当自定义 tag 拒掉会误伤「名字/值刚好长得像日期」的合法字符串。
// 它按字符串输出到 JSON（见 scalarValue）。
func allowedTag(tag string) bool {
	switch tag {
	case "", yamlStrTag, yamlIntTag, yamlFloatTag, yamlBoolTag, yamlNullTag,
		yamlMapTag, yamlSeqTag, yamlTimestampTag:
		return true
	}
	return false
}

func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 不转义 < > &：脚本内容里很常见，转义后语义不变但字节不可读，
	// 也让「数字保真」之外的字节保真更容易用肉眼看 diff。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// indirect 去掉全部指针层（nil 安全）。
func indirect(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// sortedKeys 供「允许：a, b, c」这类消息使用（顺序稳定，便于断言）。
func sortedKeys(m map[string]reflect.Type) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
