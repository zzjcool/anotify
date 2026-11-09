package config

import (
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 机器级合并（config.md §2 / §4.1）：
//
//   - vars 逐键：local > node；本地没写的键继承节点默认值，两边都没有就不出现；
//   - limits 只有两层、逐叶：total 未写（0）才继承 node，perCapability 逐能力键
//     覆盖，未写继承；两边都没有 total 时归一为缺省 1；
//   - namespace 默认值**不参与**机器级合并（它随任务内存下发，不落 applied）。
//
// 合并结果就是 applied 的内容，hash 必须调用 protocol.MachineConfigHash——
// 中枢算期望值、节点算 appliedHash 只允许同一份实现，禁止第二份 hash 算法。

// MergeVars 返回 local > node 的逐键合并结果（永不为 nil）。
func MergeVars(local, node map[string]string) map[string]string {
	out := make(map[string]string, len(local)+len(node))
	for name, value := range node {
		out[name] = value
	}
	for name, value := range local {
		out[name] = value
	}
	return out
}

// MergeLimits 返回 local > node 的逐叶合并结果。
//
// 0 表示「这一层没设」：local.Total==0 才用 node.Total，两个都是 0 时取缺省 1；
// perCapability 逐键覆盖（"*" 是普通键，不特殊合并）；值为 0 的条目视为未设。
func MergeLimits(local, node protocol.Limits) protocol.Limits {
	out := protocol.Limits{}
	switch {
	case local.Total > 0:
		out.Total = local.Total
	case node.Total > 0:
		out.Total = node.Total
	default:
		out.Total = protocol.DefaultLimitsTotal
	}
	merged := make(map[string]int, len(local.PerCapability)+len(node.PerCapability))
	for name, v := range node.PerCapability {
		if v > 0 {
			merged[name] = v
		}
	}
	for name, v := range local.PerCapability {
		if v > 0 {
			merged[name] = v
		}
	}
	if len(merged) > 0 {
		out.PerCapability = merged
	}
	return out
}

// MergeMachine 返回机器级生效值（vars + limits），不写盘、不涉 revision。
func MergeMachine(local *Config, nodeVars map[string]string, nodeLimits protocol.Limits) (map[string]string, protocol.Limits) {
	vars := MergeVars(local.Vars, nodeVars)
	limits := MergeLimits(local.LocalLimits(), nodeLimits)
	return vars, limits
}

// MachineHash 是机器级配置 hash 的唯一入口（直接调用冻结的 protocol 实现）。
func MachineHash(vars map[string]string, limits protocol.Limits) (string, error) {
	hash, err := protocol.MachineConfigHash(vars, limits)
	if err != nil {
		return "", fmt.Errorf("计算机器配置 hash: %w", err)
	}
	return hash, nil
}

// HashMachine 是 MergeMachine + MachineHash 的便捷组合（applied 内容与 hash 同源）。
func HashMachine(local *Config, nodeVars map[string]string, nodeLimits protocol.Limits) (map[string]string, protocol.Limits, string, error) {
	vars, limits := MergeMachine(local, nodeVars, nodeLimits)
	hash, err := MachineHash(vars, limits)
	if err != nil {
		return nil, protocol.Limits{}, "", err
	}
	return vars, limits, hash, nil
}
