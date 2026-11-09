package config

import "sort"

// 生效命名空间 = 本机申报 ∩ 中枢授权（anode.md §0 D6/D7）。
//
// 这是**权限**，不是配置默认值：只能收紧、不能自我扩张。任一侧写 "*" = 全部；
// 两边都 "*" 仍是 "*"。结果排序后返回，便于比较与展示；交集为空 = 这台机器
// 不服务任何 ns（不是错误，但所有部署/派发都会被拒）。

// EffectiveNamespaces 返回 declared ∩ authorized。
func EffectiveNamespaces(declared, authorized []string) []string {
	declaredAll := containsWildcard(declared)
	authorizedAll := containsWildcard(authorized)

	// 两侧都声明全部：保留 "*"（不是展开成无限列表）。
	if declaredAll && authorizedAll {
		return []string{Wildcard}
	}

	var source []string
	switch {
	case declaredAll:
		source = authorized
	case authorizedAll:
		source = declared
	default:
		auth := make(map[string]bool, len(authorized))
		for _, ns := range authorized {
			auth[ns] = true
		}
		out := make([]string, 0, len(declared))
		for _, ns := range declared {
			if auth[ns] {
				out = append(out, ns)
			}
		}
		sort.Strings(out)
		return dedupe(out)
	}

	out := make([]string, 0, len(source))
	for _, ns := range source {
		if ns == "" || ns == Wildcard {
			// "*" 已经被上面的分支处理；空串不是合法 ns。
			continue
		}
		out = append(out, ns)
	}
	sort.Strings(out)
	return dedupe(out)
}

// AllowsNamespace 报告某个 ns 是否在生效列表里（"*" 放行所有合法 ns）。
func AllowsNamespace(effective []string, namespace string) bool {
	if namespace == "" {
		return false
	}
	for _, ns := range effective {
		if ns == Wildcard {
			return namePattern.MatchString(namespace)
		}
		if ns == namespace {
			return true
		}
	}
	return false
}

// NamespaceValidator 是「这个 ns 是否被本节点授权」的判定函数（部署校验用）。
type NamespaceValidator func(namespace string) bool

func containsWildcard(list []string) bool {
	for _, v := range list {
		if v == Wildcard {
			return true
		}
	}
	return false
}

func dedupe(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
