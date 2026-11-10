package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// MachineConfigHash 是机器级配置（本机 ⊕ 节点默认值）的内容哈希。
//
// 它只回答一个问题：「两台机器/两个时刻的**生效机器配置**是不是同一份」。
// 因此输入只有两样东西：
//
//   - vars：机器级生效变量（local > node 合并结果）；
//   - limits：机器级生效并发预算（local > node 逐叶合并结果，缺省 total=1）。
//
// 明确**不包含** revision（版本号变化不代表内容变化）、namespace 默认值
// （ns 级、随任务内存下发，不进 applied）、token、宿主凭证、envPassthrough
// （完整名单只在本机，不上报）。两侧（中枢算期望值 / 节点算 appliedHash）
// 只调用这一份实现，禁止第二份 hash 算法。
//
// 归一化后取 canonical JSON（`vars` / `limits` 两个键，map 键由
// encoding/json 排序），SHA-256 输出 `sha256:<64 hex>`（与 artifact 的 digest
// 同前缀）。空 map 与 nil 一律写成 `{}`；`limits.total` 未设置（0）归一为
// 缺省 1；`limits.perCapability` 同理缺省 `{}`。
//
// 负的 total 不是「未设置」，无法归一成缺省值：返回 error（正常配置在
// 写入期就被拒，这里只是不静默吞掉非法输入）。
func MachineConfigHash(vars map[string]string, limits Limits) (string, error) {
	canonical, err := machineConfigCanonicalJSON(vars, limits)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// machineConfigDoc 是 hash 的 canonical 文档形状。
//
// 字段顺序即字节顺序：`vars` 在前、`limits` 在后；两级 map 的键都由
// encoding/json 按字节序排序，所以同一内容永远得到同一串字节。
type machineConfigDoc struct {
	Vars   map[string]string   `json:"vars"`
	Limits machineConfigLimits `json:"limits"`
}

// machineConfigLimits 是 canonical 文档里的 limits。
//
// 两个字段都**不加 omitempty**：空 perCapability 必须写成 `{}`（而不是省略或
// null），否则「没配 perCapability」和「配了个空的」会在 hash 上分叉。
type machineConfigLimits struct {
	Total         int            `json:"total"`
	PerCapability map[string]int `json:"perCapability"`
}

// machineConfigCanonicalJSON 产出 hash 的输入字节（归一 + canonical 排序）。
func machineConfigCanonicalJSON(vars map[string]string, limits Limits) ([]byte, error) {
	total := limits.Total
	if total == 0 {
		total = DefaultLimitsTotal
	}
	if total < 0 {
		return nil, fmt.Errorf("protocol: MachineConfigHash: limits.total 不能为负：%d", total)
	}
	// 复制一份：不把调用方的 nil / map 别名带进编码，也避免后续被并发改。
	normVars := make(map[string]string, len(vars))
	for name, value := range vars {
		normVars[name] = value
	}
	perCapability := make(map[string]int, len(limits.PerCapability))
	for name, value := range limits.PerCapability {
		perCapability[name] = value
	}
	raw, err := json.Marshal(machineConfigDoc{
		Vars: normVars,
		Limits: machineConfigLimits{
			Total:         total,
			PerCapability: perCapability,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("protocol: MachineConfigHash: 编码 canonical JSON: %w", err)
	}
	return raw, nil
}
