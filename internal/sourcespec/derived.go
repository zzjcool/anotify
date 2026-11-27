package sourcespec

// `_derived` 是 match 叶子里的派生字段命名空间（#512「策略是数据」里计数/算术那一半）：值从载荷算出，不是载荷里真有的键。
// 只收纯函数（同一份事件永远算出同一个值）——墙钟与随机数会破坏「派发可重放」，要时钟的留在能力里（#522）。

import (
	"strings"
)

const (
	// DerivedPrefix 是派生字段的根名：`field: "_derived.links"`。
	DerivedPrefix = "_derived"
	// DerivedIsPullRequest 是「这次事件的对象是不是 PR」：`issue.pull_request` 或顶层 `pull_request` 存在即真。
	DerivedIsPullRequest = "isPullRequest"
	// DerivedLinks 数正文里的链接条数（任意深度、键名为 body 的字符串值；`html_url` 之类不算）。
	DerivedLinks = "links"
)

// derivedNames 是派生字段的封闭集合（写入期校验与「可选」消息都取自它）。
var derivedNames = []string{DerivedIsPullRequest, DerivedLinks}

// derived 是「按需算、算一次就够」的派生值：一条事件最多 2×32 行叶子，共用同一份。
type derived struct {
	payload map[string]any
	links   int
	scanned bool
}

func (d *derived) lookup(name string) (any, bool) {
	if d == nil {
		return nil, false
	}
	switch name {
	case DerivedIsPullRequest:
		return d.isPullRequest(), true
	case DerivedLinks:
		return d.countLinks(), true
	}
	return nil, false
}

func (d *derived) isPullRequest() bool {
	if _, ok := LookupPointer(d.payload, "issue.pull_request"); ok {
		return true
	}
	_, ok := LookupPointer(d.payload, "pull_request")
	return ok
}

func (d *derived) countLinks() int {
	if !d.scanned {
		d.links, d.scanned = countBodyLinks(d.payload), true
	}
	return d.links
}

// countBodyLinks 递归找「键名 body」的字符串值，把 `http://` / `https://` 各算一条。
func countBodyLinks(node any) int {
	switch value := node.(type) {
	case map[string]any:
		total := 0
		for key, item := range value {
			if text, ok := item.(string); ok && key == "body" {
				total += strings.Count(text, "http://") + strings.Count(text, "https://")
				continue
			}
			total += countBodyLinks(item)
		}
		return total
	case []any:
		total := 0
		for _, item := range value {
			total += countBodyLinks(item)
		}
		return total
	}
	return 0
}

func derivedField(field string) (string, bool) {
	if field == DerivedPrefix {
		return "", true
	}
	if name, ok := strings.CutPrefix(field, DerivedPrefix+"."); ok {
		return name, true
	}
	return "", false
}
