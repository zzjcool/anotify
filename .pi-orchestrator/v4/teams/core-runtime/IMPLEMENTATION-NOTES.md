# T1 实施裁决记录（IMPLEMENTATION-NOTES）

> 本文件记录契约留白处的实现裁决（`.pi-legion/contract.md` D-I*），供父会话 review / 后续团队对齐。不改变冻结 v4 设计。
> 分支 `pi-subagent/v4-core-runtime-775b3657`。

## 已落地

| 编号 | 裁决 | 落点 | 状态 |
|---|---|---|---|
| D-I1 | `internal/protocol` 是节点 wire 唯一源（零本项目依赖），server/ws/anode 共用；无旧帧 alias | `internal/protocol/**` | ✅ G1-1 |
| D-I2 | `config.get` 请求 `{appliedRevision,appliedHash}`，中枢以 `welcome` 同形回答；`config.changed` 只带 `{revision}` | 同上 | ✅ G1-1 |
| D-I13 | hello 扩展：`agentVersion/agentCommit`、`facts{hostIP,hostname,os,arch,cpu,workRoot,homeDir}`、`capabilities[]`（commands 探测）、`instances[]`。理由：`NodesAvailable×CommandMissing`、Node.status、upgrade 依赖；不新增帧 | `internal/protocol/config_frames.go` | G1-2 |
| D-I14 | 失败原因码补 `timeout`（`timeoutSec` 到点，failed、不自动重试）；`impl_missing` 保留用于「spec 没有 run」 | protocol/artifact | G1-2/G1-6 |
| D-I3 | hello 上报 localVars/localLimits/申报 namespaces/appliedRevision/head；中枢算 machineVars=local⊕node；ns 默认不落 applied、不上报 | store/ws | G1-3/G2 |
| D-I4 | `<dataDir>/anode.applied.yaml`：0600、原子写、`{revision,hash,vars,limits}` 机器级合并 | cmd/anode | G2 |
| D-I5 | `playbooks` 存 files(JSON map)+source；`playbook_installs` 每次 install 一行；`installed_from` 首次来源；文件数 64 含 playbook.yaml；行6 顶层码 `version_content_immutable`、行10 `playbook_conflict`；`conflicts[]` 只在 400 错误体 | store/server | G1-2/G1-5 |
| D-I6 | edges 投影 `(namespace_id,from_is_source,from_ref,to_ref)`，无 port/priority/mapping/literals；next/targets 写入同事务重建；无写 Edge API | store | G1-3/G1-4 |
| D-I7 | 删除 sticky/node_pins、graph_versions、artifacts 镜像、task_logs、nodes.namespace_id、builtin/executor/notify-phone、github source kind；保留 auth/passkey/device/push/通知/upgrade | store/route/ws/server | G1/G2 |
| D-I8 | task 一行/逻辑任务；attempt 只在 grant +1；stale_config 不计；cancel 等 processGroupReaped 释放槽位；列名 snake_case | store | G1-4 |
| D-I9 | emits(task_id,emit_id) 部分唯一；同 id 同请求回旧结果，异 to/data 409 emit_id_conflict；无 id 每次新派发；跨 task 不合并 | store/route | G1-4/G2 |
| D-I10 | 优先零新依赖；protocol/schema/artifact 保持节点可编译（不链 sqlite/server） | 全局 | 持续 |
| D-I11 | attempt 退避 5s×2 上限 5m；stale_config 5s 起上限 60s，集中常量 | store/ws | G1-4 |
| D-I12 | 其它留白偏离须记录本文件并在 DONE 说明；跨团队可见变化先报父 | 全局 | 持续 |

## 文档缺口（已上报父会话，等待裁决/文档回改，不阻塞实现）

1. `capability.md §6.2` 封闭原因码缺 `timeout`（已按 D-I14 补，建议回写文档）。
2. `capability.md §6.1` 仍列 `impl_missing`，artifact 已不再产出 handler 语义；v4 需定义「spec 无 run」的稳定码（暂用 impl_missing）。
3. `capability.md §8.5` / `config.md §5` 的 hello 字段不足以支撑设计自身要求的节点匹配/status（已按 D-I13 扩展并保持同帧）。
4. `config.md` 未定义 `config.get` 字段（已按 D-I2 最小闭环）。
5. `playbook.md` 内部矛盾（note 码 5/6、文件数上限 64 是否含入口、行6/行10 顶层码、conflicts[] 位置）已按 D-I5 裁决。

## G1-1 实测发现（记录）

- `internal/protocol` 19 帧 + 枚举/错误码/emit DTO/applied 形状，19 个测试函数；`internal/artifact` v4 重写，foldKey/digest 边界、默认值归一化、next/displayName 不出版本、files/env/workDir/requires 校验；两包 `go test` 181 run 全绿（commit `61c7629`）。
- agent.tui 模板原取消 trap 缺陷已修（后台 hc wait + 内建 wait，TERM 可立即中断）；属模板脚本 bug 而非产品代码。
- 预期中间态：artifact v4 使 store/route/ws/server/cmd/agent 编译中断，按 G1→G4 收口，不使用 legacy/stub。
