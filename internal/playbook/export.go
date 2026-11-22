package playbook

import "sort"

// ReleaseObjects 汇总一个 release 全部回执里记下的对象名（#536 的导出集合）。
//
// 只认回执的 created：能力 `name@version` 按名字去重（导出给当前生效版本），源是裸名；
// 两个列表各按名字升序（顺序是契约的一部分）。刻意不做名字前缀匹配。
func ReleaseObjects(receipts []Receipt) (capabilities, sources []string) {
	caps, srcs := map[string]bool{}, map[string]bool{}
	for i := range receipts {
		for _, ref := range receipts[i].Created.Capabilities {
			if name := refName(ref); name != "" {
				caps[name] = true
			}
		}
		for _, name := range receipts[i].Created.Sources {
			if name != "" {
				srcs[name] = true
			}
		}
	}
	return sortedSet(caps), sortedSet(srcs)
}

// sortedSet 把去重集合落成升序切片（空集合是 [] 不是 nil）。
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
