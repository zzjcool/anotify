package protocol

// Limits 是节点的并发预算（anode.md §2 / config.md §4.1）。
//
// 多维但本期只有两个维度：total（这台机器同时最多跑几个任务）与 perCapability
// （按能力收紧；"*" = 未单列的能力）。有效并发 = min(空闲槽位, perCapability[NAME]
// ?? "*" ?? ∞, 能力的 spec.concurrency.maxPerNode)——节点只能收紧，不能突破能力
// 自己声明的预算（capability.md §2.3）。
//
// 只有本机 + 节点默认两层，没有 ns 级配额；逐叶合并（本机同名键覆盖 node，未写
// 继承）。它出现在三个地方：hello.localLimits / welcome.nodeLimits / applied 文件。
type Limits struct {
	// Total 这台机器同时最多跑几个任务（缺省 1，config.md §4.1）。
	Total int `json:"total"`
	// PerCapability 按能力名收紧；键是能力名或 "*"（未单列的能力）。
	PerCapability map[string]int `json:"perCapability,omitempty"`
}

// LimitsWildcard 是 PerCapability 里「未单列的能力」那个键。
const LimitsWildcard = "*"

// DefaultLimitsTotal 是 total 的缺省值（anode.md §2：缺省 1）。
const DefaultLimitsTotal = 1
