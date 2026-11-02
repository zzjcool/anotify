package sitegen

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// OpenAPIOperation 是 API 文档页里的一行「方法 + 路径」。
//
// 只保留渲染需要的东西：不把 summary/description 带进页面，因为 spec 是
// 单一中文源，而站点要出 4 种语言——把中文摘要塞进 /ja/ 页面就是 i18n 事故。
// 端点路径与方法名本身是语言无关的，分组标题走 i18n key（见 docs.html）。
type OpenAPIOperation struct {
	Method string // 大写方法名：GET / POST / PATCH / DELETE（PUT/HEAD/OPTIONS 同理）
	Class  string // 方法徽章的 CSS class（docs.html 的 .m-get / .m-post / ...）
	Path   string // 绝对路径：/v1/events、/agent-login.sh
}

// OpenAPIGroup 是按 spec tag 分好组的端点清单。
type OpenAPIGroup struct {
	Tag   string // spec 里的 tag 名（events / tasks / …），模板据此拼 i18n key
	Total int    // 该组的端点数
	Ops   []OpenAPIOperation
}

// openAPIDoc 只解出 sitegen 需要的字段。
//
// 刻意不用 map[string]map[string]yaml.Node：显式列出 7 个方法字段，能让
// parameters / servers / $ref 这些**不是方法**的兄弟键自然被忽略，也免去
// 手写「这个键是不是 HTTP 方法」的判断。
type openAPIDoc struct {
	Tags  []openAPITag               `yaml:"tags"`
	Paths map[string]openAPIPathItem `yaml:"paths"`
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

// apiPrefix 是 spec 里 servers[].url 的前缀：spec 的 path 是相对 /v1 写的。
const apiPrefix = "/v1"

// unknownTagKey 给没有 tags 的 operation 兜底（spec 当前 0 例，但别让
// 一个漏写 tags 的新端点从页面上静默消失）。
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

// displayPath 把 spec 的相对 path 转成调用方实际请求的绝对路径。
//
// spec 用 /../ 前缀标记「不在 /v1 下」的端点（如 /../agent-login.sh → /agent-login.sh），
// 这与 scripts/check_openapi.py 的 spec_to_absolute 保持同一套约定。
func displayPath(p string) string {
	if len(p) > 3 && p[:4] == "/../" {
		return p[3:]
	}
	if p == "" || p[0] != '/' {
		return apiPrefix + "/" + p
	}
	return apiPrefix + p
}

// LoadOpenAPIGroups 解析 OpenAPI spec，返回按 tag 分组的端点清单。
//
// 分组顺序：spec 顶层 tags 的声明顺序（作者意图），spec 里没声明但被用到的
// tag 追加在后面并按字典序排列；组内先按路径、再按方法排序。
// 标了 deprecated 的 operation 不进页面：旧「通知终端」端点仍在线但已弃用，
// 文档页展示它们只会让人照着旧模型写代码。
func LoadOpenAPIGroups(path string) ([]OpenAPIGroup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read openapi spec %s: %w", path, err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi spec %s: %w", path, err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("openapi spec %s has no paths", path)
	}

	type key struct{ method, path, tag string }
	var ops []key
	usedTags := map[string]bool{}
	for p, item := range doc.Paths {
		for method, op := range map[string]*openAPIOp{
			"GET":     item.Get,
			"POST":    item.Post,
			"PUT":     item.Put,
			"PATCH":   item.Patch,
			"DELETE":  item.Delete,
			"HEAD":    item.Head,
			"OPTIONS": item.Options,
		} {
			if op == nil || op.Deprecated {
				continue
			}
			tag := unknownTagKey
			if len(op.Tags) > 0 && op.Tags[0] != "" {
				tag = op.Tags[0]
			}
			usedTags[tag] = true
			ops = append(ops, key{method: method, path: displayPath(p), tag: tag})
		}
	}

	// 分组顺序：spec 声明顺序 → 未声明的按字典序补在后面。
	var order []string
	seen := map[string]bool{}
	for _, t := range doc.Tags {
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

	sort.Slice(ops, func(i, j int) bool {
		if ops[i].path != ops[j].path {
			return ops[i].path < ops[j].path
		}
		return methodRank(ops[i].method) < methodRank(ops[j].method)
	})
	for _, op := range ops {
		gi := index[op.tag]
		groups[gi].Ops = append(groups[gi].Ops, OpenAPIOperation{
			Method: op.method,
			Class:  methodBadgeClass(op.method),
			Path:   op.path,
		})
		groups[gi].Total = len(groups[gi].Ops)
	}

	return groups, nil
}
