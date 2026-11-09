# T1 core-runtime · 进度

分支：`pi-subagent/v4-core-runtime-775b3657`（基线 0e0ff0f）
契约：`.pi-legion/contract.md`（rev1）

## 状态

| Gate | 任务 | 状态 | 备注 |
|---|---|---|---|
| G1 | G1-1 protocol+artifact | 进行中 | writer 已派发 |
| G1 | G1-2 schema baseline | 待派 | 依赖 G1-1 |
| G1 | G1-3 store 能力/节点/配置 | 待派 | |
| G1 | G1-4 store task/emit/source/retention | 待派 | |
| G1 | G1-5 store playbook | 待派 | |
| G1 | G1-6 G1 收口+EXPORTED-INTERFACES | 待派 | |
| G2 | G2-1..5 | 待派 | |
| G3 | G3-1..5 | 待派 | |
| G4 | G4-1..2 | 待派 | |

## 已确认事实（父侦察 + 本队 scout）

- v3 现状：outputs/port/priority/mapping/literals/handler；node.namespace_id；assign 计 attempt+授 token；单 JSON playbook；整 env 继承；unix seq emit；无三层配置/双 revision/attemptToken/lease.grant/playbook_installs/emit_id。
- v4 设计事实（详见 docs/design/v4 与 .pi-legion/contract.md D-I1..D-I12）。
- 中途 compile break 是预期：artifact v4 会打断 store/route/ws/server，直到 G1/G2/G3 依次收口；不得用 stub/legacy 兜底。

## 下一步

G1-1 verify 通过 → G1-2（schema v4 baseline）。

## 已完成 checkpoint

### G1-1 ✅ commit 61c7629
- internal/protocol 新建：19 帧（5 配置/10 执行/4 运维）+ 枚举/错误码/emit DTO/applied 形状；零本项目依赖。
- internal/artifact v4 重写：params/next/run/requires/lifecycle/concurrency、foldKey/digest、显式默认等价、next/displayName 不出版本、files/env/workDir/requires 校验、模板 v4。
- verify：gofmt/vet + `go test ./internal/protocol/... ./internal/artifact/...` 181 run 全绿。

### G1-2 ✅ commit 38cc153（centurion 独立复核四条 verify 全绿）
- schema.sql 单块 v4 baseline（22 表；node_configs/namespace_configs/system_config/emits/playbook_installs；删 node_capabilities/node_pins/graph_versions/artifacts/task_logs）。
- 旧库两条路径拒绝（schema_version>1；无账本但有业务表），不删数据。
- 能力领域 v4：foldKey 折叠、digest 含 version、显式同版不可变 version_content_immutable、next/displayName 原地更新（同事务重建 edges + capability.routing_changed 事件）。
- 删 builtin.go/builtin_test.go（v3 执行器）；playbook.go 删除，G1-5 按 v4 重写。
- protocol hello D-I13 扩展（agentVersion/agentCommit/facts/capabilities/instances）。
- 测试策略修订：旧 in-package 测试保持 HEAD 原样（G1-6 闭包）；新 v4 测试在 `internal/store/v4test/`（35 用例）。
- 实测坑：SQLite 保留字 `values` 必须带引号。
- 已知中间态：route/server/ws 仍 v3 编译失败（G1-3/G1-4/G3 迁移面）。
