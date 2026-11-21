package playbook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"gopkg.in/yaml.v3"
)

// capNamePattern 与 capability.md §2 的能力名正则一致。
//
// 权威执行者是 `artifact.Validate`（渲染后走单建校验，不设第二套）；这里留一份只是为了
// 能给出 §2.3 要求的 `render_name_invalid`（点名"哪条模板 → 渲染成什么 → 为什么不合规"）。
// `TestRenderNamePatternMatchesArtifact` 钉住两者的判定一致。
var capNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Render 把包渲染成一组**普通文档**（playbook.md §2 / D19），release 取缺省（= 包名）：
// 等价于 RenderForRelease(pkg, values, "")，所以老包与本版之前逐字相同。
func Render(pkg *Package, values map[string]any) (*RenderResult, error) {
	return RenderForRelease(pkg, values, "")
}

// RenderForRelease 是带 release 身份的渲染：① 补 schema 默认 ② 校验 values
// ③ 结构内插值（`{{values.点号路径}}` 与 `{{release.name}}`，未知形态 400）。
// 渲染后立即走单建校验（D6，不设第二套），再查包内同名 / 成环 / 数量上限。 **纯函数**：只读包体、values 与 release，不读库、不判冲突、不产生回执。
func RenderForRelease(pkg *Package, values map[string]any, release string) (*RenderResult, error) {
	if pkg == nil {
		return nil, errf(CodeInvalidPlaybook, "Render 需要一个包（nil 不是空包）")
	}
	rel, err := NormalizeRelease(release, pkg.Name)
	if err != nil {
		return nil, err
	}
	scope := renderScope{release: rel}
	sch, err := pkg.schemaOf()
	if err != nil {
		return nil, err
	}
	eff, err := effectiveValues(sch, values)
	if err != nil {
		return nil, err
	}
	scope.values = eff
	digest, err := ValuesDigest(eff)
	if err != nil {
		return nil, err
	}
	if len(pkg.CapabilityPaths) > MaxCapabilities || len(pkg.SourcePaths) > MaxSources {
		return nil, errf(CodeTooManyItems, "包内有 %d 个能力 / %d 个源，上限 %d / %d",
			len(pkg.CapabilityPaths), len(pkg.SourcePaths), MaxCapabilities, MaxSources)
	}

	out := &RenderResult{
		Values:       eff,
		ValuesDigest: digest,
		Files:        make(map[string]string, len(pkg.CapabilityPaths)+len(pkg.SourcePaths)),
		Capabilities: make([]*artifact.Artifact, 0, len(pkg.CapabilityPaths)),
		Sources:      make([]Source, 0, len(pkg.SourcePaths)),
	}

	capPath := make(map[string]string, len(pkg.CapabilityPaths))
	for _, p := range pkg.CapabilityPaths {
		body, ok := pkg.Files[p]
		if !ok {
			return nil, errf(CodeFileMissing, "引用清单里的 %q 不在包内", p)
		}
		a, renderedBody, err := renderCapability(p, body, scope)
		if err != nil {
			return nil, err
		}
		name := a.Metadata.Name
		if prev, dup := capPath[name]; dup {
			return nil, errf(CodeDuplicateCapability,
				"渲染后同名能力 %q 出现两次（%s 与 %s）：需要同名多版本共存就拆成两个包", name, prev, p)
		}
		capPath[name] = p
		out.Capabilities = append(out.Capabilities, a)
		out.Files[p] = renderedBody
	}

	srcPath := make(map[string]string, len(pkg.SourcePaths))
	for _, p := range pkg.SourcePaths {
		body, ok := pkg.Files[p]
		if !ok {
			return nil, errf(CodeFileMissing, "引用清单里的 %q 不在包内", p)
		}
		src, renderedBody, err := renderSource(p, body, scope)
		if err != nil {
			return nil, err
		}
		if prev, dup := srcPath[src.Name]; dup {
			return nil, errf(CodeDuplicateSource,
				"渲染后同名源 %q 出现两次（%s 与 %s）：源的 targets 差异请拆包表达", src.Name, prev, p)
		}
		srcPath[src.Name] = p
		out.Sources = append(out.Sources, src)
		out.Files[p] = renderedBody
	}

	if err := checkCycle(out.Capabilities); err != nil {
		return nil, err
	}
	return out, nil
}

// renderCapability 渲染一份能力文档并跑单建校验。
func renderCapability(where, body string, scope renderScope) (*artifact.Artifact, string, error) {
	tree, err := decodeDoc(body, where)
	if err != nil {
		return nil, "", err
	}
	// §1.3：`status` 在导入时剥离（它是派生物；API 直接写 status 才 400）。
	delete(tree, "status")
	rendered, err := interpolateTree(tree, scope, where)
	if err != nil {
		return nil, "", err
	}
	if err := checkRenderedName(tree, rendered, where, "能力"); err != nil {
		return nil, "", err
	}

	text, err := marshalDoc(rendered)
	if err != nil {
		return nil, "", errf(CodeInvalidPlaybook, "%s 渲染后无法序列化: %v", where, err)
	}
	a, err := artifact.Parse([]byte(text))
	if err != nil {
		return nil, "", errf(CodeInvalidPlaybook, "%s 渲染后不是合法能力文档：%v", where, err)
	}
	return a, text, nil
}

// sourceDocWire 是源文档的外壳（source.md §2：apiVersion/kind/metadata/spec）。
type sourceDocWire struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   sourceMetaWire `json:"metadata"`
	Spec       sourceSpecWire `json:"spec"`
}

type sourceMetaWire struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

// sourceSpecWire 是 spec 段的严格形状：未知字段是错误（拼错的键静默失效最难查）。
//
// Enabled 用指针：`enabled` 省略 = true（source.md §2），用 bool 会把省略读成 false， 于是每个包里的每个源都会被当成"停用"。
type sourceSpecWire struct {
	Kind    string              `json:"kind"`
	Enabled *bool               `json:"enabled"`
	Config  json.RawMessage     `json:"config"`
	Targets []sourcespec.Target `json:"targets"`
}

// renderSource 渲染一份源文档并跑单建校验（sourcespec.ValidateSpec）。
func renderSource(where, body string, scope renderScope) (Source, string, error) {
	tree, err := decodeDoc(body, where)
	if err != nil {
		return Source{}, "", err
	}
	delete(tree, "status")
	rendered, err := interpolateTree(tree, scope, where)
	if err != nil {
		return Source{}, "", err
	}
	if err := checkRenderedSourceName(tree, rendered, where); err != nil {
		return Source{}, "", err
	}
	name := fieldString(rendered, "metadata", "name")

	text, err := marshalDoc(rendered)
	if err != nil {
		return Source{}, "", errf(CodeInvalidPlaybook, "%s 渲染后无法序列化: %v", where, err)
	}
	wire, err := decodeSourceWire(rendered, where)
	if err != nil {
		return Source{}, "", err
	}
	if wire.Kind != "" && wire.Kind != KindSource {
		return Source{}, "", errf(CodeInvalidPlaybook, "%s 的 kind 是 %q，源槽位要求 %q", where, wire.Kind, KindSource)
	}
	spec := sourcespec.Spec{
		Kind:    wire.Spec.Kind,
		Enabled: wire.Spec.Enabled == nil || *wire.Spec.Enabled,
		Config:  wire.Spec.Config,
		Targets: wire.Spec.Targets,
	}
	if spec.Targets == nil {
		spec.Targets = []sourcespec.Target{}
	}
	// 归一化 config：库里存的就是规范形态（缺省 tz、token 空 = 待生成、未知字段拒）。
	cfg, err := sourcespec.NormalizeConfig(spec.Kind, spec.Config)
	if err != nil {
		return Source{}, "", errf(CodeInvalidPlaybook, "%s 渲染后 config 不合法：%v", where, err)
	}
	spec.Config = cfg
	// forWrite=true：webhook 的 token 允许为空（创建时由服务端生成）。
	if err := sourcespec.ValidateSpec(spec, true); err != nil {
		return Source{}, "", errf(CodeInvalidPlaybook, "%s 渲染后不是合法源文档：%v", where, err)
	}
	return Source{Name: name, DisplayName: wire.Metadata.DisplayName, Spec: spec}, text, nil
}

// decodeSourceWire 严格解码源文档（未知字段是错误）。
func decodeSourceWire(tree map[string]any, where string) (sourceDocWire, error) {
	raw, err := json.Marshal(tree)
	if err != nil {
		return sourceDocWire{}, errf(CodeInvalidPlaybook, "%s 渲染后无法序列化: %v", where, err)
	}
	var wire sourceDocWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return sourceDocWire{}, errf(CodeInvalidPlaybook, "%s 渲染后不是合法源文档：%v", where, err)
	}
	return wire, nil
}

// marshalDoc 把渲染后的文档树序列化成 YAML（key 有序，同一份输入永远同一份字节）。
func marshalDoc(tree map[string]any) (string, error) {
	b, err := yaml.Marshal(tree)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// fieldString 取 `tree[a][b]` 的字符串值（任一层缺失 → 空串）。
func fieldString(tree map[string]any, a, b string) string {
	child, ok := tree[a].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := child[b].(string)
	return s
}

// checkRenderedName 校验渲染后的名字，并给出 §2.3 要求的三个信息 （哪条模板 → 渲染成什么 → 为什么不合规 + 建议）。**永远不自动清洗**（D25）。
//
// 缺失 metadata.name（连模板都没有）是包的形状问题（invalid_playbook）； 模板存在但渲染结果不合法（含渲染成空串）是 render_name_invalid。
func checkRenderedName(before, after map[string]any, where, what string) error {
	tmpl := fieldString(before, "metadata", "name")
	if tmpl == "" {
		return errf(CodeInvalidPlaybook, "%s 的 metadata.name 缺失或不是字符串", where)
	}
	name := fieldString(after, "metadata", "name")
	if capNamePattern.MatchString(name) {
		return nil
	}
	return errf(CodeRenderNameInvalid,
		"%s 的%s名不合法：模板 %q 渲染成 %q；名字必须匹配 %s（≤63）——"+
			"建议在 spec.values 里给参与名字的字段写 pattern / maxLength",
		where, what, tmpl, name, capNamePattern.String())
}

// checkRenderedSourceName 在 values 物化后校验源名，不借用能力名的正则。
// 保留模板/物化名/路径的领域错误投影，同时保留共享错误的 errors.Is 身份。
func checkRenderedSourceName(before, after map[string]any, where string) error {
	tmpl := fieldString(before, "metadata", "name")
	if tmpl == "" {
		return errf(CodeInvalidPlaybook, "%s 的 metadata.name 缺失或不是字符串", where)
	}
	name := fieldString(after, "metadata", "name")
	if err := sourcespec.ValidateName(name); err != nil {
		return fmt.Errorf("%w: %w", errf(CodeRenderNameInvalid,
			"%s 的源名不合法：模板 %q 渲染成 %q", where, tmpl, name), err)
	}
	return nil
}

// ---------- 结构内插值 ----------

// renderScope 是插值时仅有的两个事实源（values / release）：模板看不见库与包体其它字段。
type renderScope struct {
	values  map[string]any
	release string
}

// templateToken 是一段字符串里的一个原子：字面量，或一个模板引用（根名只有 values / release）。
type templateToken struct {
	literal   string
	root      string // "values" | "release"；空 = 字面量
	reference string // root 之内剩下的路径（values: `a.b.0`；release: `name`）
	// invalid 是不认识的引用原文（`{{release.foo}}`）：插值时 400，敏感性推导忽略它。
	invalid string
}

// interpolateTree 递归渲染一棵文档树的**字符串叶子**（键不渲染，D5）。
func interpolateTree(tree map[string]any, scope renderScope, where string) (map[string]any, error) {
	out, err := interpolateValue(tree, scope, where)
	if err != nil {
		return nil, err
	}
	rendered, ok := out.(map[string]any)
	if !ok {
		return nil, errf(CodeInvalidPlaybook, "%s 渲染后顶层不是映射", where)
	}
	return rendered, nil
}

// interpolateValue 递归渲染一个值；at 是给人看的字段位置（`capabilities/a.yaml: spec.next[0]`）。
func interpolateValue(v any, scope renderScope, at string) (any, error) {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for _, k := range sortedValueKeys(val) {
			child, err := interpolateValue(val[k], scope, at+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = child
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			child, err := interpolateValue(item, scope, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			out[i] = child
		}
		return out, nil
	case string:
		return interpolateString(val, scope, at)
	default:
		return v, nil
	}
}

// interpolateString 求值一个字符串叶子（§1.4 的类型规则）。
//
//   - 整个值恰好只有一个引用 → **保留原类型**（`replicas: '{{values.n}}'` 是数字）；
//
// - 掺了任何常量 → **字符串拼接**（非字符串按 JSON 文本拼入）； - 对象/数组拼进字符串 → `render_type_mismatch`。
//
// 只替换恰好匹配 `{{values.<路径>}}` / `{{release.name}}` 的片段；其它 `{{…}}`（`{{payload.x}}`）、
// `$NAME`、`${…}`、`$(…)` 原样透传。但 `release.` 开头的其它写法是 400——它看起来像 引用，静默当字面量会让作者以为名字按 release 分开了。
func interpolateString(s string, scope renderScope, at string) (any, error) {
	tokens := tokenizeTemplate(s)
	refs := 0
	for _, tk := range tokens {
		if tk.invalid != "" {
			return nil, errf(CodeInvalidPlaybook,
				"%s：不认识的模板引用 %s——包模板只认 {{values.点号路径}} 与 {{release.name}}（release 只有 name 一个字段）",
				at, tk.invalid).withDetails(map[string]any{
				"field":     at,
				"reference": tk.invalid,
			})
		}
		if tk.root != "" {
			refs++
		}
	}
	if refs == 0 {
		return s, nil
	}
	if len(tokens) == 1 && tokens[0].root != "" {
		return resolveToken(tokens[0], scope, at)
	}

	var b strings.Builder
	for _, tk := range tokens {
		if tk.root == "" {
			b.WriteString(tk.literal)
			continue
		}
		v, err := resolveToken(tk, scope, at)
		if err != nil {
			return nil, err
		}
		switch v.(type) {
		case map[string]any, []any:
			return nil, errf(CodeRenderTypeMismatch,
				"%s：%s 是 %s，不能拼进字符串（要整值引用就让它独占整个值）",
				at, refLabel(tk), typeName(v))
		}
		b.WriteString(stringifyValue(v))
	}
	return b.String(), nil
}

// resolveToken 求值一个引用 token；release.name 一定是字符串，不会触发类型不符。
func resolveToken(tk templateToken, scope renderScope, at string) (any, error) {
	if tk.root == "release" {
		return scope.release, nil
	}
	return lookupValue(scope.values, tk.reference, at)
}

// refLabel 是引用在错误消息里的全名（`values.a.b` / `release.name`）。
func refLabel(tk templateToken) string {
	if tk.root == "" {
		return tk.reference
	}
	return tk.root + "." + tk.reference
}

// tokenizeTemplate 把字符串拆成字面量与引用；其它 `{{…}}` 原样留在字面量里。
func tokenizeTemplate(s string) []templateToken {
	out := make([]templateToken, 0, 2)
	var lit strings.Builder
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		if open < 0 {
			lit.WriteString(s[i:])
			break
		}
		open += i
		lit.WriteString(s[i:open])
		closeAt := strings.Index(s[open:], "}}")
		if closeAt < 0 {
			// 没有闭合：整段是普通文本。
			lit.WriteString(s[open:])
			break
		}
		closeAt += open
		inner := s[open+2 : closeAt]
		root, ref, invalid := templateReference(inner)
		if root != "" || invalid != "" {
			if lit.Len() > 0 {
				out = append(out, templateToken{literal: lit.String()})
				lit.Reset()
			}
			out = append(out, templateToken{root: root, reference: ref, invalid: invalid})
			i = closeAt + 2
			continue
		}
		// 别的根名 / 别的写法：原样保留（含定界符）。
		lit.WriteString(s[open : closeAt+2])
		i = closeAt + 2
	}
	if lit.Len() > 0 {
		out = append(out, templateToken{literal: lit.String()})
	}
	return out
}

// templateReference 判定 `{{…}}` 是哪一种引用：`values.路径` / `release.name`（唯一字段）/
// `release.<别的>`（invalid，400）/ 其它（三者皆空 = 普通文本）。带空格的写法一律算普通 文本——两种引用同一条拼写口径，才不会出现「空格版生效、紧凑版不生效」。
func templateReference(inner string) (root, ref, invalid string) {
	if path, ok := valuesReference(inner); ok {
		return "values", path, ""
	}
	const prefix = "release."
	if !strings.HasPrefix(inner, prefix) {
		return "", "", ""
	}
	if inner == ReleaseRef {
		return "release", "name", ""
	}
	return "", "", "{{" + inner + "}}"
}

// valuesReference 判定 `{{…}}` 的内容是不是 `values.点号路径`，返回去掉根名的路径。
func valuesReference(inner string) (string, bool) {
	const prefix = "values."
	if !strings.HasPrefix(inner, prefix) {
		return "", false
	}
	rest := inner[len(prefix):]
	if rest == "" {
		return "", false
	}
	for _, seg := range strings.Split(rest, ".") {
		if !validValuesSegment(seg) {
			return "", false
		}
	}
	return rest, true
}

// validValuesSegment 是点号路径的路径段字符集：标识符或纯数字（数字段 = 数组下标）。
//
// 与 source.md §4.1 的路径规则同口径（连字符既可能是减号也可能是键名，两种解释都会让 用户猜；写成合法标识符最省事）。
func validValuesSegment(seg string) bool {
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

// lookupValue 按点号路径在有效 values 里取值；取不到 → `values_missing`（不静默给空）。
func lookupValue(values map[string]any, ref, at string) (any, error) {
	var cur any = values
	for _, seg := range strings.Split(ref, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, missingValue(ref, at)
			}
			cur = v
		case []any:
			idx, ok := toIntFromString(seg)
			if !ok || idx < 0 || idx >= len(node) {
				return nil, missingValue(ref, at)
			}
			cur = node[idx]
		default:
			return nil, missingValue(ref, at)
		}
	}
	return cur, nil
}

// missingValue 造一个 §2.3 形状的 values_missing（点名路径 + 两条出路）。
func missingValue(ref, at string) error {
	return errf(CodeValuesMissing,
		"%s：values.%s 没有默认值也没传；要么 install 时给，要么在 spec.values 里写 default",
		at, ref)
}

// stringifyValue 把引用值转成拼接用的文本（字符串原样、其余按 JSON 字面量，null → 空串）。
func stringifyValue(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case json.Number:
		return val.String()
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// typeName 给人看的类型名（错误消息用）。
func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "对象"
	case []any:
		return "数组"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// toIntFromString 把路径段转成数组下标。
func toIntFromString(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<20 {
			return 0, false
		}
	}
	return n, true
}

// sortedValueKeys 返回 map 的键（升序）——遍历与报错的确定性来源。
func sortedValueKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- 包视角校验：环 ----------

// checkCycle 在**渲染后的包内闭合子图**上做环检测（§3.2 / §2.2）。
//
// 指向包外的名字不参与（外面长什么样我们不知道）。`name@version` 也按**名字**闭合：
// v4 的边按名字解析、版本由 latest 决定，所以"指向谁"由名字定（§3.2 的"名字"就是这个意思）。
func checkCycle(caps []*artifact.Artifact) error {
	index := make(map[string]int, len(caps))
	names := make([]string, 0, len(caps))
	for i, a := range caps {
		index[a.Metadata.Name] = i
		names = append(names, a.Metadata.Name)
	}
	edges := make([][]int, len(caps))
	for i, a := range caps {
		for _, ref := range a.Spec.Next {
			name := refName(ref)
			if j, ok := index[name]; ok {
				edges[i] = append(edges[i], j)
			}
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int, len(caps))
	stack := make([]int, 0, len(caps))
	var walk func(int) error
	walk = func(i int) error {
		color[i] = gray
		stack = append(stack, i)
		for _, j := range edges[i] {
			switch color[j] {
			case gray:
				// 找到回边：把栈上从 j 到 i 的那一段连成环。
				cycle := make([]string, 0, len(stack))
				for k := len(stack) - 1; k >= 0; k-- {
					cycle = append([]string{names[stack[k]]}, cycle...)
					if stack[k] == j {
						break
					}
				}
				cycle = append(cycle, names[j])
				return errf(CodeCycle,
					"渲染后的包内 next 图成环：%s（包外名字不参与判定；跨包成环由 store 的边环检测兜底）",
					strings.Join(cycle, " → "))
			case white:
				if err := walk(j); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[i] = black
		return nil
	}
	for i := range caps {
		if color[i] != white {
			continue
		}
		if err := walk(i); err != nil {
			return err
		}
	}
	return nil
}

// refName 取 `name@version` 的名字部分。
func refName(ref string) string {
	if i := strings.LastIndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// capRef 把 (name, version) 拼成回执里的引用形式（省 version 时只给名字）。
func capRef(name string, version int) string {
	if version < 1 {
		return name
	}
	return fmt.Sprintf("%s@%d", name, version)
}
