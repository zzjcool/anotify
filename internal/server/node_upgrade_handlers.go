package server

import "strings"

// 本文件是「agent 自升级」在 v4 HTTP 面的**装配缝**（CONTRACT-agent-upgrade §2 / api-conventions §8.2）。
//
// v3 → v4 的职责重排（读完这一段再看代码，否则会以为少了东西）：
//
//   - **端点实现**搬进了 Node 资源域：`POST /v1/nodes/{name}/upgrade` 由
//     `resource_node.go` 的 `nodeAPI.upgradeNode` 实现（v4 的节点是 cluster-scoped 资源，
//     路径从 `{id}` 变成 `{name}`；会话鉴权由 `nodeAPI.register` 用 `deps.session` 包裹）。
//     本文件**不再**实现一份同名 handler；「离线 / 无二进制 / 版本相同」三种不发帧的
//     判定只在 `upgradeNode` 里有一份（v3 的分裂点已收敛）。
//   - **升级源的事实**留在这里：二进制在启动时一次性读入内存（`agentBinary`：字节 + sha256 +
//     版本三者同一份快照），本文件把它适配成资源域要的 `nodeUpgradeSource`
//     （`TargetVersion()` / `SHA256()` 两个只读事实）。适配器是**唯一**的注入点：
//     `mux.go` 在装配时 `nodeAPI.setUpgradeSource(newAgentUpgradeSource(agentBin))`。
//   - 未注入（或文件不存在 = 功能未启用）→ 接口是 nil → `POST /upgrade` 回
//     501 `agent_binary_unavailable`，与 `GET /v1/node/binary` 的 501 同语义。
//     这是合法部署形态（绝大多数开发/裸机部署本来就不带 agent 二进制），不是启动错误。
//
// 下发通道是 `ws.Hub.RequestUpgrade(ctx, nodeID, targetVersion, sha256, urlPath)`：
// v3 的 `ws.NodeFrameUpgrade` / `ws.NodeBinaryPath` / `NodeUpgradePublisher` 已随
// NodeHandler 退役，帧类型在共享协议包（`protocol.UpgradeFrame` / `protocol.NodeBinaryPath`）。
// hub 自己负责「先落 sent 事实（MarkAgentUpgradeSent）再发帧」——**装配层不要再补一次写库**，
// 那会让同一次下发在 nodes.agent_upgrade 上留两条事实。

// isUniqueVersion 报告版本号是否**唯一标识构建内容**。
//
// `dev` 与含 `dirty` 的版本号不满足：同一版本号可以对应任意多次不同构建。生产构建来自
// 干净 tag（VERSION=v1.5.0），版本号与内容一一对应。升级端点的「已最新」判定只能用
// 这种版本号：否则对脏构建点升级会被误判成 up_to_date，节点永远不会被替换。
func isUniqueVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return false
	}
	return !strings.Contains(v, "dirty")
}

// agentUpgradeSource 把 `*agentBinary` 适配成资源域的只读升级源投影。
//
// 用值类型而不是指针：`nodeAPI.upgrade` 是接口字段，如果塞一个 nil 的 `*agentBinary`
// 进去，`a.upgrade != nil` 会是真的，501 分支就落到「二进制未装载」那条更模糊的路径上。
// 值类型 + 构造时判空让「未启用」在接口层面就是 nil，语义只有一处。
type agentUpgradeSource struct {
	bin *agentBinary
}

// TargetVersion 是本次下发的目标版本（= 中枢自己的版本）。
func (s agentUpgradeSource) TargetVersion() string { return s.bin.targetVersion() }

// SHA256 是二进制内容的 sha256（64 位小写十六进制，不带 "sha256:" 前缀）。
func (s agentUpgradeSource) SHA256() string { return s.bin.sha256 }

// newAgentUpgradeSource 构造升级源；未带二进制 → 返回**未包装的 nil 接口**。
func newAgentUpgradeSource(bin *agentBinary) nodeUpgradeSource {
	if bin == nil || !bin.enabled() {
		return nil
	}
	return agentUpgradeSource{bin: bin}
}
