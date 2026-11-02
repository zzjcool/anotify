# TASKS · console-declares

> 冻结契约见 `ISSUE.md` / `CONTRACT-artifact.md` / `CONTRACT-protocol.md`。
> 本文件只派生任务，**不产生新决策**；契约矛盾必须上报协调者。

## 依赖图

```
T1 artifact 包（新包，零依赖）
      │
T2 协议 + store（hello 瘦身 / deploy / apply / 新表 / 迁移 15）
      │
T3 server API（能力 run / deployments / templates）
      │
      ├──────────────┬──────────────┐
T4 agent 纯执行      T5 控制台 UI     T6 e2e
   （缓存/应用/       （能力页模板      （ws_protocol /
    workdir/          +表单+部署       capacity /
    standalone）      面板）           real_backend）
      │                │               │
      └────────┬───────┴───────────────┘
            T7 I1 改写 + 断言 + 文档
```

## T1 · `internal/artifact` 包

**owner**：协调者
**文件所有权**：`internal/artifact/**`（新建）
**验收**：`go test ./internal/artifact/...` 全绿；覆盖 §4 全部校验规则 + digest 确定性 + 9 个模板渲染

- [x] `artifact.go` — 类型、`Parse`、`Validate`、`Digest`、`CanonicalJSON`
- [x] `templates.go` — 9 个内置模板 + `Render(templateID, fields) (Artifact, error)`
- [x] `artifact_test.go` — 校验规则表驱动测试（每条规则一个反例）
- [x] `templates_test.go` — 每个模板渲染出的产物都能 `Parse` 通过

## T2 · 协议 + store

**owner**：协调者
**文件所有权**：`internal/ws/**`、`internal/store/{schema.sql,migrate15.go,node.go,artifact.go,deployment.go}`、`cmd/agent/{protocol.go,config.go}`
**验收**：`go test ./internal/{ws,store}/...` 全绿

- [x] 协议：hello 删 `capabilities`/`manifest`、加 `applied`；新增 `deploy`/`apply`；assign 加 `digest`/`trace_id`；`ProtocolVersion=2`
- [x] store：`schema.sql` 加 `artifacts`/`deployments`；`migrate15.go` 重建 `node_capabilities`（去 `run_spec`）
- [x] store：`ReconcileApplied`（hello 全量对账 → 重建 `node_capabilities`）
- [x] store：`CreateDeployment` / `ListDeployments` / `DeleteDeployment` / `MarkDeployResult`
- [x] store：`CreateArtifact` / `ListArtifacts` / `LatestArtifact`
- [x] ws：`node_handshake` 走 `ReconcileApplied`；`node_session` 处理 `apply` 帧；`node_hub` 加 `PublishDeploy`
- [x] route：`NodesForCapability` 一行不改（回归测试钉住）

## T3 · server API

**owner**：协调者
**文件所有权**：`internal/server/{console_handlers.go,mux.go,deploy_handlers.go}`、`api/openapi.yaml`
**验收**：`go test ./internal/server/...` 全绿 + `make contract` 通过

- [x] `GET /v1/capability-templates`
- [x] `POST /v1/capabilities` 支持 `run` + `requires`，编译产物 + 落 `artifacts`
- [x] `GET /v1/artifacts`
- [x] `POST/GET/DELETE /v1/deployments`（含选择器展开、掉线节点仍建行）
- [x] openapi 同步（`make contract` 会校验 mux 一致性）

## T4 · agent 纯执行

**owner**：worker（worktree `wt-agent`）
**文件所有权**：`cmd/agent/**`
**依赖**：T1、T2
**verify**：`go build ./... && go test ./cmd/agent/...`

- [x] `cache.go` — `~/.anotify/artifacts/<digest>/` 落盘、`index/<name>@<ver>` 索引、digest 校验
- [x] `apply.go` — 处理 `deploy` 帧：校验 → 探 `requires` → 原子落盘 → 回 `apply` 帧
- [x] `runner.go` — **去掉 `sh -c`**，改直接 exec entrypoint；`ANOTIFY_WORKDIR` 每任务独立目录
- [x] `runner.go` — 新增 `ANOTIFY_TRACE_ID` / `ANOTIFY_DIGEST`
- [x] `main.go` — `anotify-agent run ./x.yaml -p k=v` standalone 模式（不连中枢）
- [x] hello 发 `applied[]`；删 `capabilities`/`manifest` 构造
- [x] `config.go` — 删 `capabilities[]`/`runSpec`（node.yaml 只留 name/server/token/instances）

## T5 · 控制台 UI

**owner**：frontend worker（worktree `wt-ui`）
**文件所有权**：`web-src/pages/{capabilities.html,node.html,nodes.html}`、`web-src/locales/*.yaml`、`web/ui.css`
**依赖**：T3 的 API 形状（契约已冻结，可并行开工）
**verify**：`make fe && node scripts/check-classes.mjs`

- [x] 能力页：模板选择器（9 个）+ 动态表单 + 只读脚本预览 + 「脚本模式」textarea
- [x] 能力页：每个能力的部署状态（applied/failed/local 三色 + 逐台原因）
- [x] 部署面板：勾机器 / 按 tag 选择 → 部署 → 逐台结果
- [x] 节点页：已缓存产物列表 + digest + 本地变体黄标
- [x] i18n 四语言 key 齐全（`make e2e` 的 i18n_coverage 会抓）
- [x] **顺手修 `edges.html:541` 的 `JSON.stringify` bug**（现在建边必然 400）

## T6 · e2e

**owner**：tester worker（worktree `wt-e2e`，在 T4/T5 之后）
**文件所有权**：`scripts/e2e/suites/{ws_protocol,capacity,real_backend}.mjs`、`scripts/e2e/lib/{nodes,wsclient}.mjs`
**verify**：`make e2e`

- [x] `ws_protocol.mjs`：hello 新形状、deploy/apply 帧、协议版本 2
- [x] `capacity.mjs`：部署 → 派发链路（不再靠 hello 声明能力）
- [x] `real_backend.mjs`：真 agent 起来 → 控制台部署 → 任务成功
- [x] 新增断言数不得少于被删的断言数（1653 → 1764）

## T7 · I1 改写 + 断言 + 文档

**owner**：协调者
**文件所有权**：`docs/contracts/CONTRACT.md`、`Makefile`、`scripts/check_contract.py`、`docs/design/playbooks.md`、`DEVELOPMENT.md`
**verify**：`make contract && make ci`

- [x] CONTRACT.md：I1 改写为「执行边界」版本；新增 §6.3 部署协议
- [x] `Makefile` / `check_contract.py`：新增 A5/A6/A7 三条断言
- [x] `playbooks.md`：manifest/run 章节重画为 artifact/deploy
- [x] `schema.sql` 注释：`node_capabilities` 标注「只从 deployments 派生，不得手写」

## 落地结果（2026-11-01）

`make ci` 全绿；`make e2e` 23 套件 / **1768 断言 / 0 失败**（改前 1653）。

### 执行中发现并修掉的真 bug（都不是测试问题）

| 级别 | bug | 修法 |
|---|---|---|
| P0 | 中枢从没处理过上行 `apply` 帧（`node_session` 缺分支）→ **部署失败永远停在 pending**，「这台机器没装 claude」到不了控制台 | 补 `handleConfigFrame`/`handleApply` + 5 条库断言测试 |
| P1 | 同内容换版本建能力 → **500**（store 的 content-hash 幂等把版本归一回原版本，随后按请求版本反查产物 `ErrNotFound`） | 用归一后的 `c.ID/c.Version` 重建产物 |
| P0 | `edges.html` 建边必然 400（body 未 `JSON.stringify`）→ **控制台此前根本无法建图** | 一行修复 |
| P2 | 协议不匹配错误码是 `bad_frame`，契约写 `protocol_mismatch` | 新增 `ErrCodeProtocolMismatch` |
| P3 | 模板空列表序列化成 `null`（违反 I9） | `Templates()` 保证非 nil |
| P2 | 8 并发时 `fork/exec` 拿 `ETXTBSY`（fork 的子进程继承了另一 goroutine 正在写的 fd） | `forkLock` 让「写文件」与 `cmd.Start` 互斥 |
| **P1** | 控制台承诺的 `docker run -e ANOTIFY_TOKEN=...` **根本跑不起来**：`cmd/agent` 只认 node.yaml、从不读环境变量，仓库里也没有 agent 镜像 | 新增环境变量 bootstrap + `Dockerfile.agent` + `make docker-agent`；docker 命令补 `ANOTIFY_SERVER` |

### 契约修订（执行中发现的空白）

1. **`result.out` 在 v2 没有来源** → 定稿 `ANOTIFY_OUT_FILE` 约定（脚本写一行，节点校验 ∈ `spec.outputs`）。
2. **`deploy{action:remove}` 无法定位能力** → 定稿两种 action 都必填 `artifact`。
3. **`node_capabilities` 派生规则** → 从「严格 deployments 连表」改为「直接来自 `hello.applied[]`」，理由见 CONTRACT-protocol §8。

### 已知遗留（不阻塞）

- 产物目录 GC（`remove` 只摘索引，目录保留）
- 失败任务工作目录的 24h 清理
- `paramsSchema` 可完全绕过模板手写（已提供），但没有「schema 与字段双向同步」的 UI
- `nodeCapView.displayName` 恒空（既有 bug，`prefixedCapabilityColumns` 没 select display_name）

## 门禁

每完成一个 T：`go build ./... && go test ./<affected>/... -count=1`。
全部完成：`make ci && make e2e`。
