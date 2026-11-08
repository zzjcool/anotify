# Capability Spec v4 · 冻结契约

> 状态：**冻结**（2026-11-08）。实施中若要改本文，先改本文再改代码。
> 取代关系：本文取代 `../capability-spec-v3.md` 的 §3（outputs 是端口名）、§13.3（出口可见性）。
> 其余部分（§1 七名词、§3.1 实现两种形式、§4 存储、§6 回填）**继续有效**。
> 用户用词沿用 `../capability-spec-v3.md §13`：能力 / 源 / 下游 / 节点 / 部署 / 事件 / 任务。

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

  run:                            # 怎么跑
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        anotify-agent emit fixer    --data '{"file":"a.go"}'
        anotify-agent emit apply-fix --data '{"text":"ok"}'
    env: { GIT_TERMINAL_PROMPT: "0" }
    workDir: "~/src/{{repo}}"

  requires: [git]                 # 节点要装什么

  lifecycle:                      # 生命周期策略（一次运行能活多久、能不能重放）
    idempotent: true
    resultTtlSec: 3600
    timeoutSec: 900               # 从 run 挪进来：超时本来属于生命周期

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

### 2.2 `metadata`（身份）

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `name` | string | ✅ | `^[a-z0-9][a-z0-9._-]{0,62}$`（≤63） | 能力**名**，也是谱系：`next` 引用它、任务记它。改名 = 另一个能力 |
| `version` | int | 可选 | ≥1 | 省略 = **内容寻址**：按 `(name, 执行部分)` 复用已有行；显式给就永不折叠 |
| `displayName` | string | 可选 | 任意 | 纯展示（列表/标题）。**不进 digest**，改它不会让部署变 stale |

命名空间不在这里：它由 API/CLI 上下文决定。

### 2.3 `spec`（正文）

#### `spec.params` — 我要什么

| 项 | 值 |
|---|---|
| 类型 | JSON Schema（对象） |
| 必填 | 否，省略 = `{}`（怎么传都过） |
| 消费方 | 派发时校验上游 payload；建源/改源时校验源配置 |

#### `spec.next` — 交给谁

| 项 | 值 |
|---|---|
| 类型 | `[]string` |
| 元素 | `name`（跟随 latest）或 `name@version`（钉死） |
| 上限 | 32 个（沿用 `MaxOutputs`） |
| 省略 | = `[]` = **终端能力**（跑完就结束），不是错误 |
| 消费方 | 写入时校验目标存在（见 §6）；派生 `edges` 行；`emit <name>` 必须 ∈ 它 |

#### `spec.run` — 怎么跑（脚本形式，唯一形式）

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `entrypoint` | string | ✅ | 必须是 `files` 里的一个 key；内容必须以 `#!` 开头 | 否则报 `entrypoint_no_shebang` |
| `files` | map[路径]正文 | ✅ | ≥1 个；路径为**相对路径**，不含 `\`、不含 NUL，≤255 字符；单文件 ≤256 KiB；合计 ≤1 MiB | 全部展开进工作目录；多文件原样保留（不会被压扁） |
| `env` | map[string]string | 可选 | 键匹配 `^[A-Za-z_][A-Za-z0-9_]*$`；**不得以 `ANOTIFY_` 开头**（保留给契约变量）；空值 = 由节点环境提供 | |
| `workDir` | string | 可选 | 空 = 每次任务一个独立目录；否则**绝对路径**或 `~` / `~/…`（`~` 由节点展开成 agent 用户家目录）；≤1024 字节；不含 NUL、不含 `..` 段 | 参与 digest。字面量存库，所以与机器无关 |

> `timeoutSec` **不在 run 里** —— 它是生命周期的一部分，见下。

#### `spec.requires` — 节点要装什么

| 项 | 值 |
|---|---|
| 类型 | `[]string` |
| 上限 | 16 个 |
| 语义 | 节点必须预装的**外部命令**。部署时节点 `LookPath` 探测；缺任何一条 → 该节点跑不了它（任务报 `missing_requirement`，并把探过的 PATH 带上） |
| 消费方 | 部署前的节点匹配、`NodesAvailable` condition |

#### `spec.lifecycle` — 一次运行的生命周期

| 字段 | 类型 | 默认 | 取值 | 语义 |
|---|---|---|---|---|
| `idempotent` | bool | **true** | true / false | `false` = 重放不安全：租约过期后任务直接 `abandoned`，不回 `pending` 重跑 |
| `maxAttempts` | int | **3** | ≥1 | 重试上限 |
| `resultTtlSec` | int | **0** | ≥0 | 结果保留期（秒）。`0` = **永不清理**（清理任务跳过它） |
| `timeoutSec` | int | **0** | 0..86400（24h） | 本地执行超时。`0` = 不设本地超时，交给中枢租约兜底 |
| `retryable` | bool | — | — | ⚠️ **保留字段，当前无消费者**（v3 遗留，是否删除待定） |
| `expiresInSec` | int | — | — | ⚠️ **保留字段，当前无消费者**（v3 遗留，是否删除待定） |

### 2.4 `status`（全部派生，apply 时一律忽略）

| 字段 | 类型 | 值 | 说明 |
|---|---|---|---|
| `ready` | bool | — | 四条 `conditions` 全为 true 才 true |
| `version` | int | — | 实际解析到的版本（省略 version 时由内容寻址分配） |
| `digest` | string | `sha256:…` | 内容指纹 = 执行部分的身份（见 §3） |
| `conditions` | array | 见下 | 为什么没就绪 |
| `nodes` | array | `{name, state}` | state ∈ `deployed` / `stale` / `missing` |
| `resolved` | object | 见下 | 从图反推（每个 `next` 各算各的） |
| `observed` | object | 见下 | 上次真实运行长什么样 |

**`conditions[].type` 只有这四个**：

| type | false 的含义 | 谁能让它变 true |
|---|---|---|
| `Implemented` | spec 过不了校验（迁移/回填遗留的坏行） | 改能力定义 |
| `NodesAvailable` | 没有节点同时满足 `requires` | 接机器 / 在机器上装依赖 |
| `DownstreamResolved` | `next` 里某个名字解析不到 | 补那个能力，或改 `next` |
| `Deployed` | 没有节点持有当前 digest | 点部署 |

每条形如 `{type, status(bool), reason(稳定机器码), message(人话)}`。`reason` 取值举例：`Valid` / `NoMatchingNode` / `NotFound` / `NotDeployed`。

**`resolved.next[]`**：`{name, version, digest, ready, requires, missing?}`。
`requires` 是该下游**自己**的必填参数（同名直取），不是多个下游的并集——每次 `emit` 只对一个下游负责。

**`observed`**：

| 字段 | 值 |
|---|---|
| `lastRun` | 最近一次运行的时间 |
| `emits[]` | `{to, fields[], ok}` —— 这次运行里每一次 emit 交给了谁、带了哪些字段 |
| `pruned[]` | `{to, reason, detail}` —— 被剪掉的下游及原因（如 `param_missing`） |
| `tasks{}` | `{total, failed}` —— 够判断「最近是不是老挂」，不做统计报表 |

## 3. digest 边界（逐字段，不许有「大概」）

```
digest = sha256( canonical{
  metadata.name,
  metadata.version,
  spec.params,
  spec.requires,
  spec.lifecycle,      (idempotent / resultTtlSec / timeoutSec / …)
  spec.run            (entrypoint / files / env / workDir)
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
| `impl_missing` | 写入 | 没有 `run`（v4 只有脚本一种实现形式） |
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
| **handler（中枢内置执行器）** | 本期**删除**。只保留脚本形式（`run`）。需要时再单开一期，不提前造 |
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
  next: [notify]                 # 另一个能力：发通知
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        ./deploy.sh "$ANOTIFY_PARAM_SHA"
        anotify-agent emit notify --data '{"text":"deploy ok"}'
```

`notify` 是另一个普通能力（脚本形式，跑在节点上）。它必须存在——否则这个 `next` 解析不到，`status.DownstreamResolved` 会是 false。

### 11.4 两处约定

- **`metadata.namespace` 是文档信封字段**：不进产物、不进 digest、也不上节点。CLI 可以由当前上下文（`anotify config set namespace`）提供，YAML 里写它就覆盖上下文。
- **`next` 里的 `name@ver`** 只在需要钉死下游版本时写。日常写裸名字（跟随 latest）——那才是「升级下游不用改上游」的形态。
