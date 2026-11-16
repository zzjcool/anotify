> 总任务：https://github.com/zzjcool/anotify/issues/330
> 任务标识：V4_11；发现于 #335 real 模式 `capability-actions`（fixture-only 不可解，必须修产品）。
> 一个工作树一个源码 writer；base = 最新 `origin/v4/integration`。

# 现象（契约违背）

`docs/design/v4/capability.md` §2.4/§5 明确要求 Capability `status.nodes[].state ∈ {deployed, stale, missing}`，来源 `deployments × node_capabilities`（`:358`、`:633`）。真实实现：

- `internal/server/resource_capability.go:721-745` `deployedState` 只查**当前版本行**的 deployments（`DeploymentFilter{CapabilityID: row.ID}`），而 `deployment.Digest` 是该部署指向的版本行的 digest（`internal/store/deployment.go` 的 `deploymentSelect` join `capability_versions c ON c.id = d.capability_id`）→ `deployment.Digest == row.Digest` **恒真** → 只可能 deployed/missing。
- `capabilityNodeStale`（`resource_capability.go:46`）**零赋值**（死常量）：节点持有旧版本 digest 时，旧版本的 deployment 行不会出现在当前行的查询里；新版本行则根本没有 deployments。
- 后果 ①：人工部署 v3 到节点后再发 v4，节点应显示 stale，实际显示「当前行下没有该节点」（信息丢失）。
- 后果 ②：**首次人工部署**（该能力从未部署过）时 `status.nodes` 恒空 → 前端部署模态框 `#deploy-nodes` 没有任何候选 checkbox（`web-src/pages/capabilities.html:2078-2099` 从 `status.nodes` 渲染），#335 real `capability-actions` 因此超时；契约允许省略 nodes 由服务端选，但 UI 连“已授权候选节点”都拿不到，属产品可用性缺陷（部署只能靠 API）。

# 目标

1. `status.nodes` 按**能力名**聚合该 ns 下所有版本的 deployments（`deployments` 对 (node, capability name) 至多一行：`CreateDeployments` 会 supersede 旧行，见 `store/deployment.go:246-270`），逐节点：
   - `state=applied` 且 `deployment.digest == row.digest` → `deployed`；
   - `state=applied` 且 digest 不同 → `stale`（旧 digest）；
   - 其它（pending/failed/local）→ `missing`。
   `Deployed` condition 的计数/原因同步按此口径（有 deployed 才 true）。
2. 首次部署的候选：`status.nodes` 至少要在「已部署过旧版本」时给出 stale 行；**不要求**列出从未部署过的节点（那属于「不选 = 服务端按策略选」的契约分支）。#335 的 fixture 会先预部署 v2 再用 v1 人工部署，依赖本条 + stale 正确。
3. 不改 wire 形状/字段（`nodes[].state` 闭集已有 stale）；不改 store schema；不改 deploy 端点语义。

# 实现提示（已确认，不要重新侦察）

- store 侧可加 `DeploymentFilter.CapabilityName`（join `capability_versions` on `namespace_id + name`，`deleted_at IS NULL`，按 node 取最新一行；`ListDeployments` 现有过滤在 `store/deployment.go`）。
- 或者新增只读查询 `ListDeploymentsByCapabilityName(ctx, nsID, name)`，保持现有 `ListDeployments` 调用方不变。
- `deployedState` 改用 name 聚合，并删除/改写「stale 不可判」的注释（`resource_capability.go:718-720`）。
- `node_capabilities` 的加入是否必要：契约写 `deployments × node_capabilities`；当前 nodes[] 只要 deployments 事实即可（保持现状），若引入 join 必须有测试证明不改变既有可见集。

# 验收

- [ ] store 测试：同名能力 v1/v2 两个版本行，部署 v1 到 node-a、v2 到 node-b；按名查询返回两台各一行（不重复、不含其它能力）。
- [ ] server 测试：当前行 v3 时 `status.nodes` = `[{node-a, stale}, {node-b, stale}]`；把 v3 部署到 node-a 后 `[{node-a, deployed}, {node-b, stale}]`；pending 部署 → `missing`；`Deployed` condition 与计数一致；首次（无任何 deployments）→ `nodes: []` 且 condition false/not_deployed（现状保持）。
- [ ] 旧行为不回归：`resource_capability_test.go` / `deployment*_test.go` 既有断言不删不弱化；`go test ./internal/store/... ./internal/server/... -count=1`、`make contract`、gofmt/vet 真实 exit 0。
- [ ] 不改 `docs/design/v4/capability.md`（它已经是目标语义）。

# 完成定义

真实 API 下，能力部署到旧版本后升级到新版本，`GET …/capabilities/{name}` 的 `status.nodes[].state` 出现 `stale`；#335 的 real `capability-actions` 能拿到候选 checkbox（由 #335 收口方复跑，不在本 issue 改 f3 断言）。
