# EPIC: 控制台定义能力，节点纯执行（Capability as Portable Artifact）

状态：进行中
分支：`feat/console-declares`
日期：2026-11-01

## 一句话

把「能力」从「节点手写 node.yaml 自报 + 中枢只存命令不执行」改成
**「控制台唯一创作面 + 内容寻址的可移植产物 + 节点纯执行」**。

用户感知：在控制台贴一段脚本、选个模板填个表单、点部署，它就在自己的机器上能跑了。
有版本、有 digest、能审计、能回滚、能拷到别的机器上脱离中枢单跑。

## 为什么（现状的三个痛点，都有代码证据）

1. **两个地方定义同一个能力**：中枢 `POST /v1/capabilities` 定义字段（`capability_versions`），
   机器上手写 `node.yaml` 的 `run.command`。中间靠人抄 YAML + SSH + 重启 agent。
   `docs/design/playbooks.md:36` 自己把它列为「现存真问题④：没有任何地方能声明这台机器怎么跑这个能力」。
2. **硬顺序反人类**：能力没在中枢先定义 → hello 整帧被拒 `unknown_capability` → agent 根本连不上。
   正确顺序应该是「机器先上线，能力后部署」。
3. **命令已经进中枢了，但没人承认**：`node_capabilities.run_spec` 这一列正存着 command 原文
   （`internal/store/migrate.go:145`），而 `make contract` 的守卫只 grep `argv|shell|script`，
   `command` 这个词正好漏过去。I1 作为「字节级无命令」的约束**今天已经为假**。

## 裁决（已定，不再讨论）

| # | 裁决 | 理由 |
|---|---|---|
| D1 | **机器侧的 YAML 是编译产物，不是手写源文件** | 单一写者才不打架。人随时可以改，改了就成「本地变体」，中枢永不覆盖 |
| D2 | **只有一种执行器：脚本（bash）** | `runner: agent` 是模板，不是新类型。有脚本就能做任何事 |
| D3 | **产物是自包含的**（脚本内容内联，不引用外部 blob） | 这是「拷到别的机器直接能跑」成立的前提 |
| D4 | **`node_capabilities` 从「节点自报的事实」降级为「已应用部署的派生索引」** | 派发选点代码一行不用改，`NodesForCapability` 语义不变 |
| D5 | **中枢持有的是「带参数契约 + 版本 + digest 的可移植产物」，不是「任意 shell」** | 安全边界挪到物理位置：节点启动参数（并发/资源/网络）中枢推不动 |
| D6 | **I1 改写为**：中枢不得执行用户代码；节点执行的一切必须内容寻址、不可变、可审计、受节点启动参数约束 | 旧 I1 已是假命题，必须改写而不是绕过守卫 |
| D7 | **不做**：`runner: agent` 一等公民、镜像构建、控制台存 LLM 密钥、Monaco 编辑器、可视化拖拽编排 | 见 spec 的「本期不做」 |

## 用户流程（验收基准）

```
[新建能力] → 选类型
   ├─ 空白脚本
   ├─ 脚本模板（HTTP / Git / 测试 / 文件 / 通知）
   └─ Agent 模板（Claude Code / Codex / PI / 自定义 CLI）
        ↓ 填表单（prompt / model / tools / 超时）
        ↓ 实时显示编译出来的 bash（只读，可切到脚本模式编辑）
    保存 → 生成 v1 + digest
        ↓
[部署] → 选机器（勾选或按 tag）→ 推
        ↓ 逐台回报：applied / failed(缺少 claude)
        ↓
[试运行] → 填参数 → 看 stdout + exit code + 耗时
```

## 验收标准（硬门禁）

1. `make ci` 全绿（含新的 I1 静态断言）
2. `make e2e` 全绿，不减少既有断言数
3. 手工验：全新机器起 agent → 控制台建能力 → 部署 → 试运行成功（全程不碰机器）
4. 手工验：把产物 YAML 拷到另一台机器 → `anotify-agent run ./x.yaml -p k=v` 直接跑通（不连中枢）
5. 手工验：手改产物 YAML → 中枢显示「本地变体」，且再次部署不覆盖

## 里程碑

| M | 内容 | 出口条件 |
|---|---|---|
| M1 | 产物格式 + 协议 + store + 派发派生 | `go test ./internal/{artifact,ws,store,route}/...` 全绿 |
| M2 | agent 纯执行（缓存/应用/workdir/standalone） | `go test ./cmd/agent/...` 全绿 |
| M3 | 控制台（能力页模板+表单、部署面板） | `make fe` + `web_verify` 零错误 |
| M4 | e2e + 文档 + I1 改写 | `make e2e` 全绿 |

## 工作区约定

- 冻结契约：`CONTRACT-artifact.md`（产物格式）、`CONTRACT-protocol.md`（帧与状态机）
- 任务板：`TASKS.md`
- 并行 worker 必须各自 worktree，只碰自己任务卡里列的文件
