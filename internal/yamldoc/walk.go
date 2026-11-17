package yamldoc

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// jsonUnmarshalerType / rawMessageType 用于判定 opaque 子树。
var (
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	rawMessageType      = reflect.TypeOf(json.RawMessage(nil))
	jsonNumberType      = reflect.TypeOf(json.Number(""))
)

// desc 描述「这个位置允许什么」：要么是 Shape 显式给的键表，要么是 Go 类型。
//
// 两者互斥；都为空表示**不检查**（opaque）。用键表而不是类型的理由是
// metadata 段：安全边界是呼叫方的白名单，不能由 objectMeta 的 struct tag 全量决定。
type desc struct {
	keys map[string]reflect.Type // 非 nil：键表由 Shape 显式给出（顶层 / metadata 段）
	typ  reflect.Type            // 非 nil：按 Go 类型走树
}

// walker 累积结构问题与 path → Line 映射。
type walker struct {
	shape       Shape
	problems    []Problem
	numProblems []Problem // 转换期致命问题（数字无法表示为 JSON）
	lines       map[string]int
}

func (w *walker) add(p Problem) { w.problems = append(w.problems, p) }

// lineOf 是 Result.LineOf 的实现。
func (w *walker) lineOf(path string) int { return w.lines[path] }

// walk 按 desc 校验一个节点，并记录它的行号。
func (w *walker) walk(n *yaml.Node, path string, d desc) {
	if n == nil {
		return
	}
	// null 对任何期望类型都合法：encoding/json 把 null 解进标量/结构体/映射/切片
	// 都不报错（留零值）。这里放行是与下游解码行为对齐，不是放宽校验——
	// 否则 spec: ~（空 spec）会被判成「spec 必须是映射」，而 JSON 路径下它是合法的。
	if isNullScalar(n) {
		return
	}
	if d.typ == nil && d.keys == nil {
		// 不检查：只记行号（Shape 里类型为 nil 的键，如 status）。
		w.recordLines(n, path)
		return
	}
	if d.keys != nil {
		w.walkMappingPairs(n, path, d.keys, false)
		return
	}
	w.walkType(n, path, d.typ)
}

// walkMappingPairs 走一个映射节点：键表决定「允许哪些键、每个键期望什么类型」。
//
// metadataSlot 只对顶层键表为真：命中 metadata 且 Shape.Meta 非空时改用白名单。
func (w *walker) walkMappingPairs(n *yaml.Node, path string, keys map[string]reflect.Type, metadataSlot bool) {
	if n.Kind != yaml.MappingNode {
		w.typeProblem(n, path, "映射")
		return
	}
	allowed := sortedKeys(keys)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		name := k.Value
		child := joinPath(path, name)
		// LineOf 取**键**的行号：人找字段时看的是 `spec.run.files:` 那一行。
		w.lines[child] = k.Line
		expected, ok := keys[name]
		if !ok {
			w.add(Problem{
				Reason:  reasonUnknownField,
				Path:    child,
				Line:    k.Line,
				Message: unknownFieldMessage(name, allowed),
			})
			continue
		}
		if metadataSlot && name == "metadata" && len(w.shape.Meta) > 0 {
			w.walk(v, child, desc{keys: w.shape.Meta})
			continue
		}
		w.walk(v, child, desc{typ: expected})
	}
}

// walkType 按 Go 类型走树。
func (w *walker) walkType(n *yaml.Node, path string, t reflect.Type) {
	t = indirect(t)
	if t == nil {
		w.recordLines(n, path)
		return
	}
	// opaque 判定见 isOpaque：这类类型的字段语义由它自己的 UnmarshalJSON 定义，
	// 本包无从知道合法键，硬校验只会误伤（例如 EnvValue 的 {value, valueFrom}）。
	if isOpaque(t) {
		w.recordLines(n, path)
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		w.walkMappingPairs(n, path, jsonFields(t), false)
	case reflect.Map:
		// 只有 map[string]T 能进 JSON 对象；其它键类型整体 opaque（不猜）。
		if t.Key().Kind() != reflect.String {
			w.recordLines(n, path)
			return
		}
		if n.Kind != yaml.MappingNode {
			w.typeProblem(n, path, "映射")
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			child := joinPath(path, k.Value)
			w.lines[child] = k.Line
			w.walk(v, child, desc{typ: t.Elem()})
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			w.typeProblem(n, path, "序列")
			return
		}
		for i, c := range n.Content {
			child := indexPath(path, i)
			w.lines[child] = c.Line
			w.walk(c, child, desc{typ: t.Elem()})
		}
	case reflect.String:
		w.expectScalar(n, path, "字符串", yamlStrTag, yamlTimestampTag)
	case reflect.Bool:
		w.expectScalar(n, path, "布尔", yamlBoolTag)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		w.expectScalar(n, path, "整数", yamlIntTag)
	case reflect.Float32, reflect.Float64:
		// 整数标量也是合法输入（`ratio: 1`）——与 encoding/json 一致。
		w.expectScalar(n, path, "数字", yamlIntTag, yamlFloatTag)
	default:
		w.recordLines(n, path)
	}
}

// expectScalar 要求节点是标量且 tag ∈ want。
func (w *walker) expectScalar(n *yaml.Node, path, want string, tags ...string) {
	got := n.ShortTag()
	for _, t := range tags {
		if got == t {
			return
		}
	}
	w.typeProblem(n, path, want)
}

func (w *walker) typeProblem(n *yaml.Node, path, want string) {
	w.add(Problem{
		Reason:  reasonValueInvalid,
		Path:    path,
		Line:    n.Line,
		Message: "必须是" + want + "，实际是" + yamlKind(n),
	})
}

// checkRequired 检查 Shape.Required（只支持 Root 层与 metadata 段）。
//
// 缺失的行号取**父节点**的行（契约 §2 实现要点 6）；父节点用 walk 记下的键行号，
// 这样报的是 `metadata:` 那一行，而不是它第一个子键的行。
func (w *walker) checkRequired(root *yaml.Node, shape Shape) {
	if len(shape.Required) == 0 {
		return
	}
	index := mappingIndex(root)
	for _, path := range shape.Required {
		segs := strings.Split(path, ".")
		curPath := ""
		node := root
		keys := index
		for _, seg := range segs {
			if node == nil {
				break
			}
			if node.Kind != yaml.MappingNode {
				// 空节点（`metadata:` 后面什么都没有）等于这一层什么都没写：
				// 必填项确实缺失，行号取这一层的行。
				// 非空的非映射（`metadata: hello`）不重复报：类型问题已经在 walk 里报过。
				if isNullScalar(node) {
					w.add(Problem{
						Reason:  reasonInvalidYAML,
						Path:    path,
						Line:    w.lines[curPath],
						Message: "必填字段缺失",
					})
				}
				break
			}
			child, ok := keys[seg]
			if !ok {
				w.add(Problem{
					Reason:  reasonInvalidYAML,
					Path:    path,
					Line:    w.lines[curPath],
					Message: "必填字段缺失",
				})
				break
			}
			curPath = joinPath(curPath, seg)
			node = child
			keys = mappingIndex(child)
		}
	}
}

// recordLines 递归记录一棵（不校验的）子树的路径 → 行号，让 LineOf 也能查到 opaque 内部。
func (w *walker) recordLines(n *yaml.Node, path string) {
	if _, ok := w.lines[path]; !ok {
		w.lines[path] = n.Line
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			child := joinPath(path, k.Value)
			if _, ok := w.lines[child]; !ok {
				w.lines[child] = k.Line
			}
			w.recordLines(v, child)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			w.recordLines(c, indexPath(path, i))
		}
	}
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func indexPath(parent string, i int) string { return fmt.Sprintf("%s[%d]", parent, i) }

func isNullScalar(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == yamlNullTag
}

func unknownFieldMessage(name string, allowed []string) string {
	if len(allowed) == 0 {
		return fmt.Sprintf("未知字段「%s」（该位置不接受任何字段）", name)
	}
	return fmt.Sprintf("未知字段「%s」（允许：%s）", name, strings.Join(allowed, ", "))
}

// isOpaque 判定「这棵子树的语义不归本包管」。
//
// 判据（契约 §2 实现要点 5）：json.RawMessage、any（interface）、以及任何实现了
// json.Unmarshaler 的类型。为什么必须放行而不是硬校验：
//   - RawMessage / any 的全部意义就是「原样透传」，它们没有可枚举的键；
//   - 实现 UnmarshalJSON 的类型（本仓 artifact.EnvValue / Requires / Concurrency，
//     以及 time.Time）把「合法形状」定义在自己的方法里，本包复制一份等于双写规则，
//     必然漂移；而且这些方法能接受标量或对象两种形状（EnvValue 就是）。
//
// 指针接收者与值接收者都要覆盖：PointerTo(t).Implements 覆盖指针接收者的方法集
// （EnvValue 就是 `func (v *EnvValue) UnmarshalJSON`）。
// 用 PointerTo 而不是同样语义的 PtrTo：后者从 Go 1.22 起被标记弃用，
// 仓库的 Go 静态检查（staticcheck SA1019）会报出来。
func isOpaque(t reflect.Type) bool {
	if t == nil {
		return true
	}
	t = indirect(t)
	if t == nil {
		return true
	}
	if t == rawMessageType || t == jsonNumberType {
		return true
	}
	if t.Kind() == reflect.Interface {
		return true
	}
	if t.Implements(jsonUnmarshalerType) {
		return true
	}
	return reflect.PointerTo(t).Implements(jsonUnmarshalerType)
}

// jsonFields 把结构体摊平成「JSON 键 → 字段类型」，规则与 encoding/json 一致：
// 取 tag 名（omitempty 不影响键名）、`json:"-"` 跳过、无 tag 用字段名、
// 匿名内嵌结构体按 Go 规则提升（本层具名字段优先）。
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	collectFields(t, out, 0)
	return out
}

func collectFields(t reflect.Type, out map[string]reflect.Type, depth int) {
	if depth > 4 {
		// 内嵌深度上限：防病态自引用类型把这里变成无底洞。
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || f.Anonymous || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := out[name]; !dup {
			out[name] = f.Type
		}
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous || f.Tag.Get("json") == "-" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" {
			// 带 tag 的匿名字段就是普通字段（encoding/json 也这样处理）。
			if _, dup := out[name]; !dup {
				out[name] = f.Type
			}
			continue
		}
		ft := indirect(f.Type)
		if ft == nil || ft.Kind() != reflect.Struct {
			continue
		}
		collectFields(ft, out, depth+1)
	}
}

// mappingIndex 把映射节点的键映射到值节点（重复键已在拒绝集里被拒，这里取第一个）。
func mappingIndex(n *yaml.Node) map[string]*yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if _, dup := out[n.Content[i].Value]; !dup {
			out[n.Content[i].Value] = n.Content[i+1]
		}
	}
	return out
}
