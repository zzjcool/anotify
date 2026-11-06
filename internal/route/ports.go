package route

import "sort"

// MergePorts 返回一个能力**有效出口集合**：产物声明的 outputs ∪ 边上出现过的 from_port。
//
// 为什么以边为准（#383，真实缺口不是假想）：用户在能力编辑里给一个建时声明
// `outputs: []`（终端）的能力加了下游 —— 边建好了，但派发时只下发 `outputs`（空）
// → 节点拿到的端口契约里没有那个出口 → 脚本 emit 当场报「终端能力」，那条新边永远
// 不触发。既然边上已经存在一个出口，它就必须可用：**拓扑的真相是边**，`outputs`
// 只是产物里的端口声明（digest 不变，也不回写）。
//
// 规则是**并集，不是替换**：历史多出口能力（outputs 有 a、b，只有 a 接了边）不能把
// 声明过但暂时没接边的端口吞掉 —— 那会让已经在 emit b 的实现当场被拒。
//
// 输出**去重 + 升序**：同一份拓扑必须给出同一份帧字节（重发/审计要能逐字节比），
// 顺序只由名字决定，与 edges 的插入顺序无关。空串被丢掉（它不是可上报的出口名）。
//
// 纯函数（不查库、不改写入参）：调用方常把 capDef.Outputs 直接传进来，改它等于改
// 只读的能力定义缓存。返回值保证非 nil（空集合是 [] 不是 null，不变量 I9）。
func MergePorts(declared, edgePorts []string) []string {
	seen := make(map[string]bool, len(declared)+len(edgePorts))
	out := make([]string, 0, len(declared)+len(edgePorts))
	for _, list := range [][]string{declared, edgePorts} {
		for _, port := range list {
			if port == "" || seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, port)
		}
	}
	sort.Strings(out)
	return out
}
