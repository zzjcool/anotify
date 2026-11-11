package sitegen

import (
	"fmt"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// OpenAPIOperation 是 API 文档页里的一行「方法 + 路径」。
//
// 展示范围（真实能力，不夸大）：docs 页只渲染 `方法 + 绝对路径`，并按 tag 分组。
// 语言无关的 method/path 可以直接进 4 语页面；operation 的 summary/description
// 是 spec 单一源里的中文文本，进 /ja/ 页面就是 i18n 事故，所以**页面不渲染**。
//
// OperationID/Summary/Docs 是 R2 增加的**结构化保留**字段：有效的五文件内
// $ref（pathItem/parameter/requestBody/response/schema）必须能被证明解析过，
// 不能因为引用形式静默丢 operation；它们供 parser 对账、CLI 摘要与后续
// renderer 使用，**当前页面不展示参数表/请求体/响应/schema UI**。任何
// 「schema 文档已展示」的说法都超出本页真实支持范围。
type OpenAPIOperation struct {
	Method string // 大写方法名：GET / POST / PATCH / DELETE（PUT/HEAD/OPTIONS 同理）
	Class  string // 方法徽章的 CSS class（docs.html 的 .m-get / .m-post / ...）
	Path   string // 绝对路径：/v1/namespaces/{namespace}/capabilities

	OperationID string          // spec 里显式提供的 operationId（缺省为空；不发明名字）
	Summary     string          // spec 原文 summary；仅结构化保留，页面不渲染（见上）
	Docs        []OpenAPIDocRef // 结构化 $ref 文档（参数/请求体/响应/schema）；页面不渲染
}

// 结构化文档引用的种类。只覆盖五文件内可合法引用的对象类型。
const (
	DocRefParameter   = "parameter"
	DocRefRequestBody = "requestBody"
	DocRefResponse    = "response"
	DocRefSchema      = "schema"
)

// OpenAPIDocRef 是 operation 文档里保留的一条 $ref（以及它的解析结果）。
//
// 解析范围 = 当前 spec 文件内 `#/...` 或五份 canonical spec 之间的相对引用
// （如 `./resource.yaml#/components/schemas/Source`）。只做**一跳**解析：
// 组件内部的嵌套 $ref / 递归 schema 保留为引用结构，不无限展开（有界）。
type OpenAPIDocRef struct {
	Kind     string // DocRefParameter / DocRefRequestBody / DocRefResponse / DocRefSchema
	Name     string // 组件名 / 参数名（JSON Pointer 已解转义；内联对象不记录）
	Ref      string // 原始 $ref 字面量（含 ./ 与 #/ 写法）
	Origin   string // 写这条 $ref 的 canonical 文件名
	Target   string // 解析成功的定位：<file>#<pointer>；失败为空
	Resolved bool   // 是否在五份 canonical spec 内解析成功
}

// OpenAPIGroup 是按 spec tag 分好组的端点清单。
type OpenAPIGroup struct {
	Tag   string // spec 里的 tag 名（capabilities / events / …），模板据此拼 i18n key
	Total int    // 该组的端点数
	Ops   []OpenAPIOperation
}

// CanonicalSpecFiles 是 v4 API 文档的五个 spec 文件名，顺序即合并顺序。
//
// 与 docs/design/v4/api-conventions.md §2 的五个域一一对应：
// resource / events / agent / ingress / auth。旧的单文件 api/openapi.yaml
// **不再读取**：v4 路径是绝对 /v1/...，旧 spec 是相对 servers: /v1 的 v3 形状，
// 两者混在一起只会把 v3 端点伪装成 v4 事实。这里没有 fallback。
var CanonicalSpecFiles = []string{
	"resource.yaml",
	"events.yaml",
	"agent.yaml",
	"ingress.yaml",
	"auth.yaml",
}

// canonicalSpecs 是跨文件 $ref 唯一允许的来源（没有第六份 schema 真源）。
var canonicalSpecs = func() map[string]bool {
	out := make(map[string]bool, len(CanonicalSpecFiles))
	for _, name := range CanonicalSpecFiles {
		out[name] = true
	}
	return out
}()

// httpMethods 是 Path Item 下的 HTTP 方法键，顺序固定（渲染顺序另由 methodRank 决定）。
var httpMethods = []string{"get", "post", "put", "patch", "delete", "head", "options"}

// SpecFile 描述单个 canonical spec 文件的加载结果。
type SpecFile struct {
	Name       string // 文件名（resource.yaml / …）
	Present    bool   // 文件是否存在（缺失不是错误：docs 走真实空态并 warn）
	Operations int    // 该文件解析出的（过滤 deprecated 后的）端点数
}

// SpecSet 是一次 -spec-dir 加载的完整结果，供 CLI 打警告与出摘要。
type SpecSet struct {
	Groups []OpenAPIGroup // 五份 spec 合并后的分组（method+path 全局唯一、顺序确定）
	Files  []SpecFile     // 按 CanonicalSpecFiles 顺序，含缺失项
	// Duplicates 仅保留给 cmd/sitegen 的旧打印循环做编译兼容。
	// R2 起同 method+path 重复是**硬错误**（见 LoadSpecDir），该字段恒为空；
	// 不再有「静默保留首次出现」的语义。
	Duplicates []string
	Warnings   []string // 不致命但值得记一笔的解析告警（坏/外部 $ref 等，含 origin+ref）
}

// Missing 返回缺失的 spec 文件名（按 canonical 顺序）。
func (s *SpecSet) Missing() []string {
	var missing []string
	for _, f := range s.Files {
		if !f.Present {
			missing = append(missing, f.Name)
		}
	}
	return missing
}

// OperationCount 返回合并后的端点总数。
func (s *SpecSet) OperationCount() int {
	var n int
	for _, g := range s.Groups {
		n += len(g.Ops)
	}
	return n
}

// openAPIDoc 只解出 sitegen 需要的字段。
//
// Paths 保留在 typed decode 里是为了**结构校验**（paths 必须是 mapping、
// path item 不能是字符串等）：真正的 operation/ref 解析走 yaml.Node（见
// parseSpecDocument），因为 typed struct 会把 $ref、parameters 等兄弟键丢掉。
type openAPIDoc struct {
	Servers []openAPIServer            `yaml:"servers"`
	Tags    []openAPITag               `yaml:"tags"`
	Paths   map[string]openAPIPathItem `yaml:"paths"`
}

type openAPIServer struct {
	URL string `yaml:"url"`
}

type openAPITag struct {
	Name string `yaml:"name"`
}

type openAPIPathItem struct {
	Get     *openAPIOp `yaml:"get"`
	Post    *openAPIOp `yaml:"post"`
	Put     *openAPIOp `yaml:"put"`
	Patch   *openAPIOp `yaml:"patch"`
	Delete  *openAPIOp `yaml:"delete"`
	Head    *openAPIOp `yaml:"head"`
	Options *openAPIOp `yaml:"options"`
}

type openAPIOp struct {
	Tags       []string `yaml:"tags"`
	Deprecated bool     `yaml:"deprecated"`
}

// unknownTagKey 给没有 tags 的 operation 兜底（别让一个漏写 tags 的新端点
// 从页面上静默消失）。
const unknownTagKey = "other"

// methodBadgeClass 把方法映射到 docs.html 里已有的徽章样式。
// GET/HEAD/OPTIONS 都是"读"，共用 m-get；PUT 与 POST 都是"写"，共用 m-post。
func methodBadgeClass(method string) string {
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return "m-get"
	case "POST", "PUT":
		return "m-post"
	case "PATCH":
		return "m-patch"
	case "DELETE":
		return "m-delete"
	}
	return "m-get"
}

// methodRank 决定同一路径下多方法的展示顺序（读 → 写 → 改 → 删）。
func methodRank(method string) int {
	switch method {
	case "GET":
		return 0
	case "POST":
		return 1
	case "PUT":
		return 2
	case "PATCH":
		return 3
	case "DELETE":
		return 4
	case "HEAD":
		return 5
	case "OPTIONS":
		return 6
	}
	return 7
}

// serverPrefix 从 servers[].url 里取出路径前缀。
//
// v4 spec 统一写 servers: [{url: /}]（前缀为空，path 写全 /v1/...）；
// v3 旧 spec 是 servers: [{url: /v1}] + 相对 path。两种都得能读，但
// **不能让 /v1 被拼两遍**（v4 的 path 已经是绝对路径）。
//
// "https://host/v1" 这类绝对 URL 取其中的 path 部分；没有 servers 视为无前缀。
func serverPrefix(servers []openAPIServer) string {
	for _, s := range servers {
		u := strings.TrimSpace(s.URL)
		if u == "" {
			continue
		}
		if i := strings.Index(u, "://"); i >= 0 {
			rest := u[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				u = rest[j:]
			} else {
				u = ""
			}
		}
		u = strings.TrimSuffix(u, "/")
		if u == "" {
			return ""
		}
		if !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		return u
	}
	return ""
}

// displayPath 把 spec 里的 path 转成调用方实际请求的绝对路径。
//
// 规则（按顺序）：
//  1. 旧 v3 约定 /../x 表示「不在 servers 前缀下」→ /x；
//  2. servers 无前缀（v4 的 /）→ path 原样（必须是绝对路径，补前导 /）；
//  3. servers 前缀为 /v1 且 path 已经以 /v1 开头 → 原样（**不二次拼接**）；
//  4. 其余 → 前缀 + path。
func displayPath(p, prefix string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "/../") {
		return "/" + p[len("/../"):]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if prefix == "" || prefix == "/" {
		return p
	}
	if p == prefix || strings.HasPrefix(p, prefix+"/") {
		return p
	}
	return prefix + p
}

// specDocument 是单份 canonical spec 的解析输入：typed doc（servers/tags/
// 结构校验）+ yaml.Node 文档根（$ref 指针定位的底座）。
type specDocument struct {
	name string
	root *yaml.Node
	doc  openAPIDoc
}

// specResolver 在一组 canonical spec 文档根之间解析 $ref。
//
// 边界（契约）：
//   - 只读 spec-dir 下的五份 canonical 文件；不联网、不读绝对路径、
//     不允许父目录逃逸、不接受第六份 schema 真源；
//   - JSON Pointer 按标准解 ~0/~1，且不能靠字符串截取猜节点；
//   - path item 链有界（maxPathItemRefHops）并检测循环。
type specResolver struct {
	roots map[string]*yaml.Node
}

func newSpecResolver() *specResolver {
	return &specResolver{roots: make(map[string]*yaml.Node, len(CanonicalSpecFiles))}
}

func (r *specResolver) add(name string, root *yaml.Node) {
	r.roots[name] = root
}

// refProblemClass 区分 $ref 失败类别：path item 上多数是硬错误，
// 外部/远程引用按 API1 的「诚实 warning」处理；operation 内部深引用一律
// warning（operation 保留，结构化文档标注 unresolved）。
type refProblemClass int

const (
	refBadPointer refProblemClass = iota
	refBadEscape
	refRemote
	refAbsolute
	refEscape
	refSixthSource
	refMissingFile
)

type refProblem struct {
	class  refProblemClass
	detail string
}

func (p *refProblem) Error() string { return p.detail }

// resolve 解析一条 $ref 到具体 yaml.Node。
//
// 返回值：目标节点、目标 canonical 文件名、目标 JSON Pointer（带前导 /）。
// 失败时返回具体类别 + 说明；调用方负责拼 origin+ref 上下文。
func (r *specResolver) resolve(origin, ref string) (*yaml.Node, string, string, *refProblem) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, "", "", &refProblem{refBadPointer, "empty $ref (must be #/... or ./file.yaml#/...)"}
	}
	// 远程/协议相对引用：构建期离线，绝不联网读取。
	if strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "//") {
		return nil, "", "", &refProblem{refRemote, "remote reference is not followed (offline build; only the five canonical spec files are read)"}
	}
	hash := strings.IndexByte(trimmed, '#')
	if hash < 0 {
		return nil, "", "", &refProblem{refBadPointer, "reference has no #/ JSON pointer fragment"}
	}
	filePart := strings.TrimSpace(trimmed[:hash])
	fragment := trimmed[hash+1:]
	if !strings.HasPrefix(fragment, "/") {
		return nil, "", "", &refProblem{refBadPointer, "fragment must be a #/ JSON pointer"}
	}

	file := origin
	if filePart != "" {
		decoded, err := url.PathUnescape(filePart)
		if err != nil {
			return nil, "", "", &refProblem{refBadPointer, "file part is not valid percent-encoding: " + err.Error()}
		}
		if strings.ContainsAny(decoded, `\`) {
			return nil, "", "", &refProblem{refBadPointer, "file part must be a plain canonical filename (no path separators)"}
		}
		if strings.HasPrefix(decoded, "/") || filepath.IsAbs(decoded) || (len(decoded) >= 2 && decoded[1] == ':') {
			return nil, "", "", &refProblem{refAbsolute, "absolute paths are outside the spec-dir boundary"}
		}
		cleaned := pathpkg.Clean(decoded)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, "", "", &refProblem{refEscape, "parent-directory escape is outside the spec-dir boundary"}
		}
		if strings.Contains(cleaned, "/") {
			return nil, "", "", &refProblem{refSixthSource, fmt.Sprintf("%q is not one of the five canonical spec files", decoded)}
		}
		if !canonicalSpecs[cleaned] {
			return nil, "", "", &refProblem{refSixthSource, fmt.Sprintf("%q is not one of the five canonical spec files (no sixth schema source, no legacy openapi.yaml fallback)", decoded)}
		}
		file = cleaned
	}

	root, ok := r.roots[file]
	if !ok {
		return nil, "", "", &refProblem{refMissingFile, fmt.Sprintf("target spec file %s is not present in the spec-dir", file)}
	}
	node, pointer, problem := walkJSONPointer(root, fragment)
	if problem != nil {
		return nil, "", "", problem
	}
	return node, file, pointer, nil
}

// walkJSONPointer 按标准 JSON Pointer 走节点；支持 mapping 键与数组下标，
// 并解 ~0 (~) / ~1 (/) 与 percent-encoding。非法 ~ 转义是明确错误。
func walkJSONPointer(root *yaml.Node, fragment string) (*yaml.Node, string, *refProblem) {
	cur := derefNode(root)
	if cur == nil {
		return nil, "", &refProblem{refBadPointer, "target document is empty"}
	}
	parts := strings.Split(fragment[1:], "/")
	for _, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, "", &refProblem{refBadPointer, "pointer token is not valid percent-encoding: " + err.Error()}
		}
		token, ok := unescapeJSONPointer(decoded)
		if !ok {
			return nil, "", &refProblem{refBadEscape, fmt.Sprintf("pointer token %q has an invalid ~ escape (only ~0 and ~1 are defined)", decoded)}
		}
		switch cur.Kind {
		case yaml.MappingNode:
			cur = derefNode(mappingValue(cur, token))
			if cur == nil {
				return nil, "", &refProblem{refBadPointer, fmt.Sprintf("pointer target %q does not exist", token)}
			}
		case yaml.SequenceNode:
			idx, convErr := strconv.Atoi(token)
			if convErr != nil || idx < 0 || idx >= len(cur.Content) {
				return nil, "", &refProblem{refBadPointer, fmt.Sprintf("pointer array index %q is out of range or not an integer", token)}
			}
			cur = derefNode(cur.Content[idx])
			if cur == nil {
				return nil, "", &refProblem{refBadPointer, fmt.Sprintf("pointer array index %q is empty", token)}
			}
		default:
			return nil, "", &refProblem{refBadPointer, fmt.Sprintf("pointer token %q cannot descend into a non-container node", token)}
		}
	}
	return cur, "/" + strings.Join(parts, "/"), nil
}

// unescapeJSONPointer 单遍解 ~0/~1（不能用两次 ReplaceAll，否则 ~01 会错解）。
// 返回 false 表示出现了非标准 ~ 转义。
func unescapeJSONPointer(s string) (string, bool) {
	if !strings.ContainsRune(s, '~') {
		return s, true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '~' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", false
		}
		switch s[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return b.String(), true
}

// derefNode 跟随 YAML alias（锚点/别名不是 $ref，但节点定位要拿到实体）。
func derefNode(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// mappingValue 返回 mapping node 里 key 对应的 value node（没有则 nil）。
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	n = derefNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// scalarValue 返回 mapping node 里 key 的标量值（没有则空串）。
func scalarValue(n *yaml.Node, key string) string {
	v := mappingValue(n, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// lastPointerToken 取 JSON Pointer 的最后一个 token（解 ~0/~1 转义），
// 用于给保留的引用起一个可读名字。
func lastPointerToken(ref string) string {
	hash := strings.IndexByte(ref, '#')
	if hash < 0 {
		return ""
	}
	frag := strings.TrimPrefix(ref[hash+1:], "/")
	if frag == "" {
		return ""
	}
	parts := strings.Split(frag, "/")
	token := parts[len(parts)-1]
	if decoded, err := url.PathUnescape(token); err == nil {
		token = decoded
	}
	if unescaped, ok := unescapeJSONPointer(token); ok {
		return unescaped
	}
	return token
}

// maxPathItemRefHops 限制 path item 引用链长度；超过即明确错误（有界，不挂起）。
const maxPathItemRefHops = 16

// resolvePathItem 跟随 path item 的 $ref 链，返回最终 path item 节点。
//
// 语义：
//   - 链上每一跳都必须是五份 canonical spec 内的可解析指针；
//   - 循环 / 超过 maxPathItemRefHops / 目标不是 mapping → 返回错误（有界）；
//   - 远程 $ref → 返回 warning 并跳过该 path（API1 的「未知外部 ref 诚实
//     warning」，不是错误，也不联网/不 fallback 旧 openapi.yaml）。
func (r *specResolver) resolvePathItem(origin, apiPath, ref string) (*yaml.Node, string, []string, error) {
	curFile, curRef := origin, ref
	visited := make(map[string]int)
	var chain []string

	for hop := 0; ; hop++ {
		if hop > maxPathItemRefHops {
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q: reference chain exceeds %d hops (%s)",
				origin, apiPath, ref, maxPathItemRefHops, strings.Join(chain, " -> "))
		}
		node, targetFile, pointer, problem := r.resolve(curFile, curRef)
		if problem != nil {
			if problem.class == refRemote {
				return nil, "", []string{fmt.Sprintf("%s: path item %s: $ref %q skipped: %s",
					curFile, apiPath, curRef, problem.detail)}, nil
			}
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q: %s", curFile, apiPath, curRef, problem.detail)
		}
		key := targetFile + "#" + pointer
		if prevHop, seen := visited[key]; seen {
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q: reference cycle %s (hop %d -> hop %d)",
				origin, apiPath, ref, strings.Join(append(chain, key), " -> "), prevHop, hop)
		}
		visited[key] = hop
		chain = append(chain, key)

		if node.Kind != yaml.MappingNode {
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q resolves to %s which is not a mapping", origin, apiPath, ref, key)
		}
		nextRefNode := mappingValue(node, "$ref")
		if nextRefNode == nil {
			return node, targetFile, nil, nil
		}
		if nextRefNode.Kind != yaml.ScalarNode {
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q resolves to %s with a non-scalar $ref", origin, apiPath, ref, key)
		}
		if nextRefNode.Value == "" {
			return nil, "", nil, fmt.Errorf("%s: path item %s: $ref %q resolves to %s with an empty $ref", origin, apiPath, ref, key)
		}
		curFile, curRef = targetFile, nextRefNode.Value
	}
}

// LoadSpecDir 读取 dir 下五个 canonical spec 并合并成文档页分组。
//
// 语义（R2 起）：
//   - 文件缺失 → 记进 SpecSet.Files（Present=false）+ CLI 具名 warn，不是错误；
//   - 五份全缺失 / 目录不存在 → 合法空文档（docs 页显示真实空态）；
//   - 已存在但解析失败（坏 YAML / paths 不是 mapping 等）→ 返回 error；
//   - agent.yaml 这类协议文档没有 HTTP paths → 合法，0 operations；
//   - path item 的 $ref 在五份 canonical spec 内解析（跨文件/链式/循环有界）；
//   - 同 method+path 跨文件重复 = 硬错误；operationId 跨五文件全局唯一，
//     重复也是硬错误（只约束显式提供的 ID，不发明名字）；
//   - 合并顺序 = CanonicalSpecFiles 顺序；tag 顺序 = 首次出现的声明顺序。
func LoadSpecDir(dir string) (*SpecSet, error) {
	set := &SpecSet{
		Groups: []OpenAPIGroup{},
		Files:  make([]SpecFile, 0, len(CanonicalSpecFiles)),
	}

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// 整个目录都不存在 = 五份都缺：docs 真实空态，不伪造端点。
			for _, name := range CanonicalSpecFiles {
				set.Files = append(set.Files, SpecFile{Name: name})
			}
			return set, nil
		}
		return nil, fmt.Errorf("stat spec dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("spec dir %s is not a directory", dir)
	}

	// 第一遍：读全部存在的 canonical spec。跨文件 $ref 需要同时看到五份文档根，
	// 因此不能按文件逐个 load-then-forget。
	docs := make(map[string]*specDocument, len(CanonicalSpecFiles))
	for _, name := range CanonicalSpecFiles {
		filePath := filepath.Join(dir, name)
		fi, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				set.Files = append(set.Files, SpecFile{Name: name})
				continue
			}
			return nil, fmt.Errorf("stat api spec %s: %w", filePath, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("api spec %s is a directory", filePath)
		}
		sd, err := readSpecDocument(name, filePath)
		if err != nil {
			return nil, err
		}
		docs[name] = sd
		set.Files = append(set.Files, SpecFile{Name: name, Present: true})
	}

	resolver := newSpecResolver()
	for name, sd := range docs {
		resolver.add(name, sd.root)
	}

	// 第二遍：按 canonical 顺序解析 operation 并合并（错误顺序确定、可复现）。
	builder := &groupBuilder{}
	for i := range set.Files {
		name := set.Files[i].Name
		sd := docs[name]
		if sd == nil {
			continue
		}
		groups, warnings, err := parseSpecDocument(sd, resolver)
		if err != nil {
			return nil, err
		}
		set.Warnings = append(set.Warnings, warnings...)
		set.Files[i].Operations = countOps(groups)
		if err := builder.addGroups(name, groups); err != nil {
			return nil, err
		}
	}
	set.Groups = builder.finish()
	if set.Groups == nil {
		set.Groups = []OpenAPIGroup{}
	}
	return set, nil
}

// LoadOpenAPIGroups 解析单个 spec 文件，返回按 tag 分组的端点清单。
//
// 保留单文件入口是给单元测试与非常规打包用的；站点的默认路径是
// LoadSpecDir（五份 spec）。单文件没有别的 canonical spec 文档根，
// 因此跨文件 $ref 在这里不可解析（会按错误/警告规则显式报告）。
// 0 个 paths 是合法的（协议文档），不是错误；无效 YAML / 结构错误才返回 error。
func LoadOpenAPIGroups(filePath string) ([]OpenAPIGroup, error) {
	groups, _, err := loadSpecFile(filePath)
	if err != nil {
		return nil, err
	}
	var builder groupBuilder
	if err := builder.addGroups(filepath.Base(filePath), groups); err != nil {
		return nil, err
	}
	return builder.finish(), nil
}

// loadSpecFile 是单文件解析入口（测试与 LoadOpenAPIGroups 共用）：
// 只有当前文件的文档根，因此跨文件 $ref 在这里不可解析（会显式 warn/error）。
func loadSpecFile(filePath string) ([]OpenAPIGroup, []string, error) {
	name := filepath.Base(filePath)
	sd, err := readSpecDocument(name, filePath)
	if err != nil {
		return nil, nil, err
	}
	resolver := newSpecResolver()
	resolver.add(name, sd.root)
	return parseSpecDocument(sd, resolver)
}

// readSpecDocument 读取并做两遍 decode：yaml.Node（$ref 定位）与 typed doc
// （servers/tags + 结构校验：paths 必须是 mapping、path item 不能是标量）。
func readSpecDocument(name, filePath string) (*specDocument, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read openapi spec %s: %w", filePath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse openapi spec %s: %w", filePath, err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi spec %s: %w", filePath, err)
	}

	sd := &specDocument{name: name, doc: doc}
	if len(root.Content) == 0 {
		// 空文件 = 没有 paths/components：合法空文档（docs 会显示真实空态）。
		sd.root = &yaml.Node{Kind: yaml.MappingNode}
		return sd, nil
	}
	rootDoc := derefNode(root.Content[0])
	if rootDoc == nil || rootDoc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse openapi spec %s: document root is not a mapping", filePath)
	}
	sd.root = rootDoc
	return sd, nil
}

func countOps(groups []OpenAPIGroup) int {
	var n int
	for _, g := range groups {
		n += len(g.Ops)
	}
	return n
}

// parsedOperation 是单份 spec 解析出的 operation + 来源/分组信息。
type parsedOperation struct {
	op  OpenAPIOperation
	tag string
}

// parseSpecDocument 解析一份 spec 的 paths：解析 path item $ref（含跨文件链）、
// 收集 operation 与结构化文档引用、按 tag 分组（组内 path+method 排序确定）。
func parseSpecDocument(sd *specDocument, resolver *specResolver) ([]OpenAPIGroup, []string, error) {
	var warnings []string
	ops := make([]parsedOperation, 0, 32)
	usedTags := make(map[string]bool)

	pathsNode := mappingValue(sd.root, "paths")
	if pathsNode != nil {
		if pathsNode.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("%s: paths must be a mapping", sd.name)
		}
		for _, specPath := range sortedMappingKeys(pathsNode) {
			if strings.TrimSpace(specPath) == "" {
				continue
			}
			itemNode := derefNode(mappingValue(pathsNode, specPath))
			itemOrigin := sd.name
			if itemNode == nil {
				return nil, nil, fmt.Errorf("%s: path item %s is empty", sd.name, specPath)
			}
			if refNode := mappingValue(itemNode, "$ref"); refNode != nil {
				if refNode.Kind != yaml.ScalarNode {
					return nil, nil, fmt.Errorf("%s: path item %s: $ref must be a scalar string", sd.name, specPath)
				}
				ref := refNode.Value
				if ref == "" {
					return nil, nil, fmt.Errorf("%s: path item %s: empty $ref", sd.name, specPath)
				}
				target, targetFile, pathWarnings, err := resolver.resolvePathItem(sd.name, specPath, ref)
				warnings = append(warnings, pathWarnings...)
				if err != nil {
					return nil, nil, err
				}
				if target == nil {
					// 远程 ref：warning 已记录，该 path 不进文档（不做旧 spec fallback）。
					continue
				}
				itemNode = target
				itemOrigin = targetFile
			}
			if itemNode.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("%s: path item %s is not a mapping", sd.name, specPath)
			}
			opWarnings, err := parsePathItemOperations(itemOrigin, specPath, itemNode, resolver, sd.doc.Servers, &ops, usedTags)
			if err != nil {
				return nil, nil, err
			}
			warnings = append(warnings, opWarnings...)
		}
	}

	return buildTagGroups(sd.doc.Tags, ops, usedTags), warnings, nil
}

// parsePathItemOperations 解析一个 path item 下的七个 HTTP 方法。
func parsePathItemOperations(origin, specPath string, itemNode *yaml.Node, resolver *specResolver,
	servers []openAPIServer, ops *[]parsedOperation, usedTags map[string]bool) ([]string, error) {
	var warnings []string
	prefix := serverPrefix(servers)

	for _, method := range httpMethods {
		opNode := derefNode(mappingValue(itemNode, method))
		if opNode == nil {
			continue
		}
		// `get:` 这种没有值的键在 yaml 里是 null：等同一个空方法声明，跳过。
		if opNode.Kind == yaml.ScalarNode && (opNode.Tag == "!!null" || opNode.Value == "") {
			continue
		}
		if opNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: path item %s: operation %s must be a mapping", origin, specPath, strings.ToUpper(method))
		}
		if strings.EqualFold(scalarValue(opNode, "deprecated"), "true") {
			continue
		}

		tag := unknownTagKey
		if tags := scalarList(opNode, "tags"); len(tags) > 0 && tags[0] != "" {
			tag = tags[0]
		}
		usedTags[tag] = true

		opLabel := strings.ToUpper(method) + " " + specPath
		op := OpenAPIOperation{
			Method:      strings.ToUpper(method),
			Class:       methodBadgeClass(strings.ToUpper(method)),
			Path:        displayPath(specPath, prefix),
			OperationID: scalarValue(opNode, "operationId"),
			Summary:     scalarValue(opNode, "summary"),
		}
		warnings = append(warnings, resolver.collectOperationDocs(opNode, origin, opLabel, &op)...)
		*ops = append(*ops, parsedOperation{op: op, tag: tag})
	}
	return warnings, nil
}

// scalarList 读 mapping 里某个键的标量数组（非数组/非标量项忽略）。
func scalarList(n *yaml.Node, key string) []string {
	v := derefNode(mappingValue(n, key))
	if v == nil || v.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(v.Content))
	for _, item := range v.Content {
		item = derefNode(item)
		if item != nil && item.Kind == yaml.ScalarNode {
			out = append(out, item.Value)
		}
	}
	return out
}

// sortedMappingKeys 返回 mapping 的键（保持确定性，错误/分组顺序可复现）。
func sortedMappingKeys(n *yaml.Node) []string {
	n = derefNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	sort.Strings(keys)
	return keys
}

// buildTagGroups 按「文件声明顺序 → 未声明的按字典序」建组，并在组内按
// path + methodRank 排序（同 path 的 GET 一定排在 POST 前）。
func buildTagGroups(declaredTags []openAPITag, ops []parsedOperation, usedTags map[string]bool) []OpenAPIGroup {
	var order []string
	seen := make(map[string]bool, len(usedTags))
	for _, t := range declaredTags {
		if t.Name == "" || seen[t.Name] || !usedTags[t.Name] {
			continue
		}
		seen[t.Name] = true
		order = append(order, t.Name)
	}
	var extra []string
	for tag := range usedTags {
		if !seen[tag] {
			extra = append(extra, tag)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	groups := make([]OpenAPIGroup, 0, len(order))
	index := make(map[string]int, len(order))
	for _, tag := range order {
		index[tag] = len(groups)
		groups = append(groups, OpenAPIGroup{Tag: tag})
	}

	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].op.Path != ops[j].op.Path {
			return ops[i].op.Path < ops[j].op.Path
		}
		return methodRank(ops[i].op.Method) < methodRank(ops[j].op.Method)
	})
	for _, parsed := range ops {
		gi := index[parsed.tag]
		groups[gi].Ops = append(groups[gi].Ops, parsed.op)
		groups[gi].Total = len(groups[gi].Ops)
	}
	return groups
}

// collectOperationDocs 保留 operation 的结构化文档引用（参数/请求体/响应/
// schema）。只做一跳；引用无法在五文件内解析时记 warning（含 origin+ref），
// operation 本身保留——坏引用不能让整条 HTTP 路径从文档里消失。
func (r *specResolver) collectOperationDocs(opNode *yaml.Node, origin, opLabel string, op *OpenAPIOperation) []string {
	var warnings []string

	if params := derefNode(mappingValue(opNode, "parameters")); params != nil {
		if params.Kind != yaml.SequenceNode {
			warnings = append(warnings, fmt.Sprintf("%s: operation %s: parameters is not a sequence; parameter docs omitted", origin, opLabel))
		} else {
			for _, entry := range params.Content {
				r.collectParameter(entry, origin, opLabel, op, &warnings)
			}
		}
	}

	if rbNode := derefNode(mappingValue(opNode, "requestBody")); rbNode != nil {
		if rbNode.Kind != yaml.MappingNode {
			warnings = append(warnings, fmt.Sprintf("%s: operation %s: requestBody is not a mapping; request docs omitted", origin, opLabel))
		} else {
			node, nodeOrigin := r.resolveDocNode(rbNode, origin, opLabel, DocRefRequestBody, "", op, &warnings)
			if node != nil {
				r.collectMediaSchemaRefs(node, nodeOrigin, opLabel, op, &warnings)
			}
		}
	}

	if responses := derefNode(mappingValue(opNode, "responses")); responses != nil {
		if responses.Kind != yaml.MappingNode {
			warnings = append(warnings, fmt.Sprintf("%s: operation %s: responses is not a mapping; response docs omitted", origin, opLabel))
		} else {
			for i := 0; i+1 < len(responses.Content); i += 2 {
				status := responses.Content[i].Value
				respNode := derefNode(responses.Content[i+1])
				if respNode == nil {
					continue
				}
				if respNode.Kind != yaml.MappingNode {
					warnings = append(warnings, fmt.Sprintf("%s: operation %s: response %q is not a mapping; response docs omitted", origin, opLabel, status))
					continue
				}
				node, nodeOrigin := r.resolveDocNode(respNode, origin, opLabel, DocRefResponse, "", op, &warnings)
				if node != nil {
					r.collectMediaSchemaRefs(node, nodeOrigin, opLabel, op, &warnings)
				}
			}
		}
	}

	return warnings
}

// collectParameter 处理 operation 的一个 parameter（可能是 $ref，也可能内联）。
func (r *specResolver) collectParameter(entry *yaml.Node, origin, opLabel string, op *OpenAPIOperation, warnings *[]string) {
	entry = derefNode(entry)
	if entry == nil {
		return
	}
	if entry.Kind != yaml.MappingNode {
		*warnings = append(*warnings, fmt.Sprintf("%s: operation %s: parameter entry is not a mapping; parameter docs omitted", origin, opLabel))
		return
	}
	name := scalarValue(entry, "name")
	node, nodeOrigin := r.resolveDocNode(entry, origin, opLabel, DocRefParameter, name, op, warnings)
	if node == nil {
		return
	}
	r.collectSchemaRef(mappingValue(node, "schema"), nodeOrigin, opLabel, op, warnings)
}

// resolveDocNode 处理非 pathItem 对象上的 $ref：解析成功返回目标节点与目标
// 文件（组件内部的嵌套 $ref 以目标文件为 origin，相对引用不会解错文件）。
// 失败时保留 Ref/Origin（Resolved=false）并记 warning。
func (r *specResolver) resolveDocNode(n *yaml.Node, origin, opLabel, kind, name string, op *OpenAPIOperation, warnings *[]string) (*yaml.Node, string) {
	n = derefNode(n)
	if n == nil {
		return nil, ""
	}
	refNode := mappingValue(n, "$ref")
	if refNode == nil {
		return n, origin
	}
	if refNode.Kind != yaml.ScalarNode {
		*warnings = append(*warnings, fmt.Sprintf("%s: operation %s: %s $ref is not a scalar; structured docs omitted", origin, opLabel, kind))
		return n, origin
	}
	ref := refNode.Value
	if ref == "" {
		return n, origin
	}
	if name == "" {
		name = lastPointerToken(ref)
	}
	if name == "" {
		name = kind
	}
	target, targetFile := r.recordRef(kind, name, origin, ref, opLabel, op, warnings)
	if target == nil {
		return n, origin
	}
	return target, targetFile
}

// recordRef 解析并记录一条结构化引用；返回目标节点与目标文件。
func (r *specResolver) recordRef(kind, name, origin, ref, opLabel string, op *OpenAPIOperation, warnings *[]string) (*yaml.Node, string) {
	target, targetFile, pointer, problem := r.resolve(origin, ref)
	doc := OpenAPIDocRef{Kind: kind, Name: name, Ref: ref, Origin: origin}
	if problem != nil {
		*warnings = append(*warnings, fmt.Sprintf("%s: operation %s: %s $ref %q: %s (operation kept; structured docs marked unresolved)", origin, opLabel, kind, ref, problem.detail))
		op.Docs = append(op.Docs, doc)
		return nil, ""
	}
	doc.Target = targetFile + "#" + pointer
	doc.Resolved = true
	op.Docs = append(op.Docs, doc)
	return target, targetFile
}

// collectMediaSchemaRefs 从 requestBody/response 的 content.*.schema 里
// 保留一层 schema $ref（内联 schema 不展开：递归 schema 有界、不无限膨胀）。
func (r *specResolver) collectMediaSchemaRefs(node *yaml.Node, origin, opLabel string, op *OpenAPIOperation, warnings *[]string) {
	content := derefNode(mappingValue(node, "content"))
	if content == nil || content.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(content.Content); i += 2 {
		media := derefNode(content.Content[i+1])
		if media == nil || media.Kind != yaml.MappingNode {
			continue
		}
		r.collectSchemaRef(mappingValue(media, "schema"), origin, opLabel, op, warnings)
	}
}

// collectSchemaRef 记录 schema 上的直接 $ref；不展开 schema 内部结构。
func (r *specResolver) collectSchemaRef(schema *yaml.Node, origin, opLabel string, op *OpenAPIOperation, warnings *[]string) {
	schema = derefNode(schema)
	if schema == nil || schema.Kind != yaml.MappingNode {
		return
	}
	refNode := mappingValue(schema, "$ref")
	if refNode == nil {
		return
	}
	if refNode.Kind != yaml.ScalarNode {
		*warnings = append(*warnings, fmt.Sprintf("%s: operation %s: schema $ref is not a scalar; schema docs omitted", origin, opLabel))
		return
	}
	ref := refNode.Value
	if ref == "" {
		return
	}
	name := lastPointerToken(ref)
	if name == "" {
		name = "schema"
	}
	r.recordRef(DocRefSchema, name, origin, ref, opLabel, op, warnings)
}

// groupBuilder 把多份 spec 的分组合并成一份确定顺序的清单。
//
// R2 契约：operation 以 method+path 为键**全局唯一**；同 method+path 重复
// 直接报错（不再静默保留首次出现）。显式提供的 operationId 也全局唯一。
type groupBuilder struct {
	groupAt map[string]int
	groups  []OpenAPIGroup
	seenOp  map[string]string // "METHOD /path" -> 首次定义的文件
	seenID  map[string]string // operationId -> "文件 METHOD /path"
}

func (b *groupBuilder) addGroups(origin string, groups []OpenAPIGroup) error {
	if b.groupAt == nil {
		b.groupAt = make(map[string]int)
		b.seenOp = make(map[string]string)
		b.seenID = make(map[string]string)
	}
	for _, g := range groups {
		idx, ok := b.groupAt[g.Tag]
		if !ok {
			idx = len(b.groups)
			b.groupAt[g.Tag] = idx
			b.groups = append(b.groups, OpenAPIGroup{Tag: g.Tag})
		}
		for _, op := range g.Ops {
			key := op.Method + " " + op.Path
			if prevOrigin, exists := b.seenOp[key]; exists {
				return fmt.Errorf("duplicate operation %s: defined in %s and %s (same method+path must appear exactly once across the five specs)",
					key, prevOrigin, origin)
			}
			b.seenOp[key] = origin
			if op.OperationID != "" {
				if prev, exists := b.seenID[op.OperationID]; exists {
					return fmt.Errorf("duplicate operationId %q: used by %s and %s %s (operationId must be globally unique across the five specs)",
						op.OperationID, prev, origin, key)
				}
				b.seenID[op.OperationID] = origin + " " + key
			}
			b.groups[idx].Ops = append(b.groups[idx].Ops, op)
		}
	}
	return nil
}

func (b *groupBuilder) finish() []OpenAPIGroup {
	out := make([]OpenAPIGroup, 0, len(b.groups))
	for _, g := range b.groups {
		sort.SliceStable(g.Ops, func(i, j int) bool {
			if g.Ops[i].Path != g.Ops[j].Path {
				return g.Ops[i].Path < g.Ops[j].Path
			}
			return methodRank(g.Ops[i].Method) < methodRank(g.Ops[j].Method)
		})
		g.Total = len(g.Ops)
		if len(g.Ops) == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}
