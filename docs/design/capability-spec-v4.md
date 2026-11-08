# Capability Spec v4 · 冻结契约

> 状态：**冻结**（2026-11-08）。实施中若要改本文，先改本文再改代码。
> 取代关系：本文取代 `capability-spec-v3.md` 的 §3（outputs 是端口名）、§13.3（出口可见性）。
> 其余部分（§1 七名词、§3.1 实现两种形式、§4 存储、§6 回填）**继续有效**。
> 用户用词沿用 `capability-spec-v3.md §13`：能力 / 源 / 下游 / 节点 / 部署 / 事件 / 任务。

---

## 0. 一句话

**能力声明三件事：我要什么（`params`）、交给谁（`next`）、怎么跑（`run`）。**
中间没有第四样东西：没有端口、没有独立的「边」、没有参数映射。

---

## 1. 四样东西的消亡

v3 里用户要理解四层：能力 → 出口（ports）→ 边（edges）→ 参数映射（mapping/literals）。v4 只剩一层。

| v3 的概念 | v4 |
|---|---|
| `spec.outputs: [needs-fix, approved]`（端口名） | **删除**。一个能力只有一个结果 |
| `outputs.needs-fix → [fixer]`（分支） | **删除**。以后用「抽象能力」表达，本期不做 |
| `edges`（用户实体、`PUT …/downstreams`） | **降级为派生行**。用户视角不存在 |
| `edges.mapping` / `edges.literals` | **删除**。改成同名直取 + default |
| `edges.priority` | **删除**。一个下游只有一条边，没有候选 |
| `emit --port X` | 删除 `--port` |
| `emit --to Y`（flag） | 目标改**位置参数**：`emit Y` |

> 为什么 priority 也删：它是「同一端口多个候选，小的先走」的排序键。端口没了，一条边只对应一个目标，没有候选可排——排序需求消失。

---

## 2. 完整文档

```yaml
apiVersion: anotify/v1
kind: Capability

metadata:
  name: review                    # 必填，^[a-z0-9][a-z0-9._-]{0,62}$
  namespace: prod                 # 必填（CLI 里可由当前上下文提供）
  version: 3                      # 可选；省略 = 内容寻址（v3 §10-B 规则不变）
  displayName: 代码审查            # 可选；不进 digest

spec:
  params:                         # 我要什么（JSON Schema，省略 = {}）
    type: object
    properties:
      repo: { type: string }
      mode: { type: string, default: safe }
    required: [repo]

  next: [fixer, apply-fix@2]       # 交给谁（封闭集合；省略 = [] = 终端能力）

  run:                            # 怎么跑（与 handler 二选一，v3 §3.1 不变）
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        anotify-agent emit fixer    --data '{"file":"a.go"}'
        anotify-agent emit apply-fix --data '{"text":"ok"}'
    timeoutSec: 900
    env: { GIT_TERMINAL_PROMPT: "0" }
    workDir: "~/src/{{repo}}"

  requires: [git]                 # 节点要装什么
  lifecycle: { idempotent: true, resultTtlSec: 3600 }
  sticky: worktree

status:                           # 全部派生。apply 时一律忽略
  ready: false
  version: 3
  digest: sha256:ab12f3…

  conditions:
    - { type: Implemented,        status: "True",  reason: Valid }
    - { type: NodesAvailable,     status: "False", reason: NoMatchingNode,
        message: "没有节点同时声明 git" }
    - { type: DownstreamResolved, status: "True" }
    - { type: Deployed,           status: "False", reason: NotDeployed,
        message: "1 台节点持有旧 digest（ab12f2…）" }

  nodes:
    - { name: node-a, state: deployed }
    - { name: node-b, state: stale }

  resolved:
    next:
      - { name: fixer,    version: 2, digest: sha256:9c1…, ready: true,
          requires: { file: { type: string } } }
      - { name: apply-fix, missing: true, reason: NotFound }

  observed:
    lastRun: 2026-11-08T13:20:00Z
    emits:
      - { to: fixer,    fields: [file],        ok: true }
      - { to: apply-fix, fields: [text, extra], ok: true }
    pruned:
      - { to: fixer, reason: param_missing, detail: "需要 file" }
    tasks: { total: 12, failed: 1 }
```

### 2.1 `next` 的三种写法

```yaml
next: []                    # 终端：跑完就结束（等价于省略）
next: [fixer]               # 一个下游
next: [fixer, apply-fix@2]   # 多个；裸名跟随 latest，@ver 钉死版本
```

- 元素必须是**能力引用**（`name` 或 `name@ver`），解析不到 → 见 §6。
- **省略 = `[]` = 终端**。「忘了写 next」不是错误状态——终端是合法且常见的（链尾多半是通知）。

---

## 3. digest 边界（逐字段，不许有「大概」）

```
digest = sha256( canonical{
  metadata.name,
  metadata.version,
  spec.params,
  spec.requires,
  spec.lifecycle,
  spec.sticky,
  spec.run            (entrypoint / files / timeoutSec / env / workDir)
  spec.handler        (handler 形式时)
} )
```

**不在 digest 里**：

| 字段 | 为什么 |
|---|---|
| `metadata.displayName` | 纯展示；v3 已有此结论（改它不该让部署变 stale） |
| `spec.next` | **路由属于图，不属于内容**。改线不该产生新版本 |

这条边界的直接推论，必须写进验收：

> **改 `next` 不产生新版本、不触发任何节点重推。**
> 改 `run.files` 一定产生新版本。

`next` 仍然是 `spec` 的一部分（同一份文档、同一份 YAML、一起导出导入），只是**不参与内容寻址**。这和 `displayName` 是同一种处理，理由也相同：它不是「节点要跑的那份东西」。

---

## 4. 运行语义

### 4.1 `emit <name> --data <json>`

- 一次 `emit` 触发**一个**下游：建一条任务，目标 = `name` 解析到的版本，版本与 digest **钉在任务上**。
- `name` 必须 ∈ 本能力 `spec.next`。不在里面 → 运行期失败，reason `unknown_target`（**不静默吞掉**）。
- 不写目标（`emit --data …`）→ 运行期失败，reason `missing_target`。
- 一次任务里可以 `emit` 0..N 次；每次是一轮独立派发。

### 4.2 参数对齐：同名直取

```
上游 payload 的字段 → 下游 params 的同名字段
```

- payload 里多余的字段 → 忽略。
- 下游 `params` 里**有 default** 的字段缺失 → 用 default。
- 下游 `params` 里**必填且无 default** 的字段缺失 → 剪枝 `param_missing`，**留痕**（事件 + `status.observed.pruned`）。
- **没有 mapping、没有 literals、没有表达式。** 需要改形状就在上游把字段名说对。

### 4.3 一次派发内的引用快照

一次 `emit` 的解析结果在整次派发内复用（`refCache`）。同一批扇出不出现「半新半旧」。

---

## 5. status 的推导规则

| 字段 | 来源 | 什么时候变 False |
|---|---|---|
| `digest` / `version` | `capability_versions` | — |
| `Implemented` | `artifact.Validate` | 校验不过（正常写不进来；用于回填/迁移遗留行） |
| `NodesAvailable` | `node_capabilities` × `requires` | 没有节点同时声明全部 requires |
| `DownstreamResolved` | `next` 每个名字能否解析 | 任一目标不存在 |
| `Deployed` | `deployments` | 有节点持有旧 digest，或一台都没推 |
| `nodes[].state` | `deployments` × `node_capabilities` | `deployed` / `stale` / `missing` |
| `resolved.next[]` | `ResolveRef` + 目标的 `params` | 目标缺失 → `missing: true` |
| `observed.*` | `events` / `tasks` | — |

`ready = 前四条 condition 全 True`。

**全部现算，不落库、不新增表。** 这也是 `status` 永远不会与现实不一致的原因。

**没有 `lastTransitionTime`**：它要求存状态历史，与「status 不落库」冲突。要知道「何时变的」，查 `deployments.updated_at` 或事件时间。

---

## 6. 校验与错误码

| 码 | 时机 | 语义 |
|---|---|---|
| `impl_missing` / `impl_ambiguous` / `unknown_handler` | 写入 | v3 §3.1 不变 |
| `unknown_target` | 写入（显式创建时） | `next` 里的名字解析不到 → 400 |
| `downstream_missing` | 派发 | 名字当时解析不到 → 任务失败 + 事件 |
| `param_missing` | 派发 | 同名直取取不到且无 default → 剪枝 |
| `missing_target` | 派发 | `emit` 没给目标 |

**顺序自由与严格校验的折中**（v3 §12.3 的 D10 语义延续）：

- **单建**（`POST /capabilities`）：`next` 里的名字必须在库里解析得到，否则 400 `unknown_target`。
- **预案 install**：允许先装上游后装下游，装完由 `status.DownstreamResolved` 与体检报告缺哪个。

---

## 7. 与 v3 的差异（一表看完）

| 项 | v3 | v4 |
|---|---|---|
| `spec.outputs` | `[端口名]`，封闭集合 | **改名为 `next`，元素是下游能力引用** |
| 分支 | `outputs: {port: [target]}` | 无。一个能力一个结果 |
| 边 | 用户实体，`PUT …/downstreams` | 派生行（`edges` 由 `next` 生成） |
| `mapping` / `literals` | 每条边可配 | 无 |
| `priority` | 每条边可配 | 无（没有候选可排） |
| `emit` | `--port X --to Y` | `emit Y`（位置参数） |
| digest | 含 `outputs` | 含 `next` 的**目标集**吗？不 —— **完全不含路由** |
| 一次 emit 的扇出 | 该端口的所有边 | 无扇出；扇出 = 多次 emit |

---

## 8. 迁移：**不做迁移**（开发期决策，2026-11-08）

> 本仓库处于开发期，**没有历史包袱**。因此本期明确不写：反推、回填、"拒绝迁移"、
> 多端口数据的人工改写清单。v3 的 §6/§8 那种谨慎程度是给有线上数据的项目用的，此处不适用。

`migration 12` 只做 DDL，四件事：

1. `edges` 重建：去掉 `from_port` / `mapping` / `literals` / `priority`，
   主键改成 `(namespace_id, from_is_source, from_ref, to_ref)`。
2. `capability_versions.outputs` 列**删除**：它原本是 `spec.outputs` 的热路径镜像，
   而 v4 不再有「端口」这个概念，边也不再按端口校验——没有读者了。
3. `artifact.Spec` 的形状变化发生在 JSON 内部，**不动表结构**。
4. `spec`（TEXT）里的旧内容不搬：v3 形状的 spec（有 `outputs`、无 `next`）在新代码下
   校验不过，开发库**删掉重建**即可。

配套要求（这是编译级/校验级要求，不是迁移）：

- devseed、e2e 造数、单测夹具必须同步改成 v4 形状；
- **协议直接去掉 `port` 字段**：开发期只有一种 agent，不需要考虑新老互通
  （若将来要滚动升级，再按 agent 版本号协商——那时是有历史包袱，不假装现在没有）。

## 9. 分期

| 期 | 内容 | 门禁 |
|---|---|---|
| **P9-a** | `spec.next` 形状 + digest 边界调整 + `edges` 派生 + migration 12 | `go test ./...`、`make contract`、e2e deploy/ws_protocol |
| **P9-b** | agent CLI：`emit <name>`（去 `--port` / `--to`）、`unknown_target` / `missing_target` | `live_agent`、`routing` |
| **P9-c** | 派发：同名直取（删 mapping/literals/priority 的读路径）+ 一次派发内快照 | `routing`、`edge_cases` |
| **P9-d** | `status` 端点 + `anotify explain` / `skeleton` | api_contract |
| **P9-e** | 前端：能力页 `next` 编辑、status 面板、删「端口/候选/优先级」相关 UI | `frontend`、i18n_coverage |
| **P9-f** | 预案 manifest：name/next 形态（删 edges 段与 branches） | `contract`、`real_backend` |

---

## 10. 非目标（明确不做）

| 不做 | 理由 |
|---|---|
| **分支（一个能力多个结果）** | 本期不需要。将来用「抽象能力」表达：那是一个新 kind，不是给 `next` 加键 |
| **CRD / `spec.type`** | 内置只留 bash 一种形态。类型系统会引入第二个 breaking 轴，等 `next` 稳定后单开一期 |
| **`mapping` / `literals` / 表达式** | 它们抹平「参数对不上」这个信号。改名或拆能力是更好的答案 |
| **`priority` / 候选排序** | 没有候选可排 |
| **status 落库** | 派生值落库就是第二真相 |
| **`lastTransitionTime`** | 同上 |

---

## 11. 三个可跑示例

### 11.1 单下游：审查 → 修

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review
  namespace: prod
spec:
  params:
    type: object
    properties:
      repo: { type: string }
    required: [repo]
  next: [fixer]
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        if grep -rq 'TODO' .; then
          # fixer 要 file（必填无 default）—— 只给它要的
          anotify-agent emit fixer --data '{"file":"a.go"}'
        fi
```

**注意**：审查通过时**不 emit**，链条在这里停住。v4 没有「分支」，但「不 emit」本身就是「这次不通往下走」——这是唯一需要的条件控制。

### 11.2 扇出：一次任务交给两个下游

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review
  namespace: prod
spec:
  params:
    type: object
    properties:
      repo: { type: string }
    required: [repo]
  next: [fixer, apply-fix]        # 扇出 = 脚本 emit 两次
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        anotify-agent emit fixer    --data '{"file":"a.go"}'
        anotify-agent emit apply-fix --data '{"text":"审查完成"}'
```

两次 emit 各带各的字段，各自只对**那一个**下游的 `params` 负责。缺 `file` 只剪 fixer 那条，apply-fix 照常。

### 11.3 链尾：用内置能力发手机推送

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: deploy
  namespace: prod
spec:
  params:
    type: object
    properties:
      sha: { type: string }
    required: [sha]
  next: [notify]                 # 内置能力，按名字引用
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        ./deploy.sh "$ANOTIFY_PARAM_SHA"
        anotify-agent emit notify --data '{"text":"deploy ok"}'
```

`notify` 是内置的中枢执行能力（`handler: notify-phone`），按名字引用即可，不需要部署到任何节点。

### 11.4 两处约定

- **`metadata.namespace` 是文档信封字段**：不进产物、不进 digest、也不上节点。CLI 可以由当前上下文（`anotify config set namespace`）提供，YAML 里写它就覆盖上下文。
- **`next` 里的 `name@ver`** 只在需要钉死下游版本时写。日常写裸名字（跟随 latest）——那才是「升级下游不用改上游」的形态。
