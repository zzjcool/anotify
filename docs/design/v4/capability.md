# Capability Spec v4 · 冻结契约

> 状态：**已定（语义冻结；实现未开始）**（2026-11-08 冻结；2026-11-09 按 `decisions.md` V4-D1/D2/D4/D5/D6 同步执行、重试、保留与环境规则）。
> 本文是 `kind: Capability` 的**唯一定义**：字段、语义、校验、错误码、存储形状、分期，全部以本文为准。
> 纪律：实施中若要改契约，**先改本文再改代码**。正文与 §12 决策记录冲突时以 §12 为准；要翻案就改对应那一行并写明理由，不要只改正文。
> 分工：节点侧见 `anode.md`，源见 `source.md`，安装包见 `playbook.md`。本文对它们只引用、不定义。

**术语**（全文只用这七个词）

| 词 | 含义 |
|---|---|
| 能力（Capability） | 一次运行的**定义**：要什么参数、交给谁、怎么跑。 |
| 节点（Node） | 装了 `anode` 的机器。能力只在节点上跑。 |
| 源（Source） | 谁触发链条的第一次运行（cron / webhook / manual），见 `source.md`。 |
| 下游（Downstream） | `spec.next` 里的一个目标。一次 emit 只对一个下游负责。 |
| 任务（Task） | 能力的一次具体运行。目标版本与 digest 钉在任务上。 |
| 部署（Deployment） | 「某节点持有某 digest」这条事实。 |
| 事件（Event） | 运行期留下的痕：emit、剪枝、失败原因。 |

---

## 0. 一句话

**能力声明三件事：我要什么（`params`）、交给谁（`next`）、怎么跑（`run`）。**
中间没有第四样东西：没有端口、没有独立的「边」、没有参数映射。

**所有能力都在节点上跑。中枢只做派发与记账，不执行任何能力。**
因此也**不存在中枢内置的推送能力**——链尾就是终端（`next: []`），要通知就自己写一个脚本能力，或交给外部系统。

---

## 1. 一个能力一个结果

模型小到只有一层，靠的是把五个概念**明确地不做**：

| 不存在的概念 | 为什么 |
|---|---|
| **端口**（`outputs` / `emit --port X`） | 一次运行的结果只有「跑完」或「跑挂」，没有第二种「结果名」。给它取名是凭空造一个用户要维护的分类 |
| **分支**（一个能力多个结果） | `if` 是脚本本来就有的能力。条件控制交给有 `if` 的语言，不交给 YAML |
| **边**（用户实体、`PUT …/downstreams`） | 边是 `next` 的投影。让用户维护两份等价事实，只会得到两份不一致 |
| **参数映射**（`mapping` / `literals` / 表达式） | 它抹平「参数对不上」这个信号。改名或拆能力是更好的答案 |
| **优先级**（`priority` / 候选排序） | 一个下游只有一条路，没有候选可排 |

推论，也是唯一需要的条件控制：

- **扇出 = 多次 emit**：一次 emit 只交给一个下游，各自的字段各自负责。
- **不 emit = 这次不往下走**：审查没通过就 `exit 0` 不 emit，链条在这里停住，不需要「approved 分支」。

---

## 2. 完整文档

```yaml
apiVersion: anotify/v1
kind: Capability

metadata:
  name: review                    # 必填，^[a-z0-9][a-z0-9._-]{0,62}$
  namespace: prod                 # 必填（CLI 里可由当前上下文提供）
  version: 3                      # 可选；省略 = 内容寻址
  displayName: 代码审查            # 可选；不进 digest

spec:
  params:                         # 我要什么（JSON Schema，省略 = {}）
    type: object
    properties:
      repo: { type: string }
      mode: { type: string, default: safe }
    required: [repo]

  next: [fixer, notify@2]         # 交给谁（封闭集合；省略 = [] = 终端能力）

  run:                            # 怎么跑
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail

        # ① CLI：薄封装，最常用
        anode emit fixer --data '{"file":"a.go"}'

        # ② 直接调接口：与上一行完全等价，任何语言都能这么发
        curl -fsS "$ANOTIFY_EMIT_ENDPOINT/emit" \
          -H "Authorization: Bearer $ANOTIFY_TASK_TOKEN" \
          -H 'Content-Type: application/json' \
          -d '{"to":"notify","data":{"text":"ok"}}'
    env:
      GIT_TERMINAL_PROMPT: "0"
      SOURCE_ROOT:                          # 仓库根在哪，机器说了算（local → node → ns）
        valueFrom:
          fieldRef: { fieldPath: vars.SOURCE_ROOT }
    workDir: $(vars.SOURCE_ROOT)            # 只能引用本能力 env 里声明过的名字

  requires:                       # 节点要提供什么（省略 = {} = 什么都不缺）
    commands: [git]               # 要装的命令
    values: [vars.SOURCE_ROOT]    # 要能解析出的值（local → node → ns）

  lifecycle:                      # 一次运行能活多久、能不能重放
    idempotent: false             # 默认 false：只有明确幂等才能自动重试
    resultTtlSec: 604800          # 默认 7 天；0 = 永不清理
    timeoutSec: 900               # 从子进程启动起算；0 = 不设本地执行超时

  concurrency:
    maxPerNode: 1                 # 这台节点上最多同时跑几个（默认 1）

status:                           # 全部派生。API 写 400；CLI 导入/apply 提交前剥离
  ready: false
  version: 3
  digest: sha256:ab12f3…

  conditions:
    - { type: Implemented,        status: "True",  reason: Valid }
    - { type: NodesAvailable,     status: "False", reason: NoMatchingNode,
        message: "没有节点同时声明 git 与 vars.SOURCE_ROOT" }
    - { type: DownstreamResolved, status: "True" }
    - { type: Deployed,           status: "False", reason: NotDeployed,
        message: "1 台节点持有旧 digest（ab12f2…）" }

  nodes:
    - { name: node-a, state: deployed }
    - { name: node-b, state: stale }

  # “人工部署”弹窗的目标列表（**部署**判据，与 nodes 是两件事）：
  # 授权覆盖本 ns 且不在排空中；首次部署前 nodes 恒空，但这儿非空。
  deployCandidates:
    - { name: node-a, online: true }
    - { name: node-c, online: false }

  resolved:
    next:
      - { name: fixer,    version: 2, digest: sha256:9c1…, ready: true,
          params: { required: [file] } }
      - { name: notify, missing: true, reason: NotFound }

  observed:
    taskId: cron-9f2-003
    emits:
      - { to: fixer,    fields: [file],        ok: true }
      - { to: notify, fields: [text, extra], ok: true }
    pruned:
      - { to: fixer, reason: param_missing, detail: "需要 file" }
    tasks: { total: 12, failed: 1 }
```

### 2.1 `next` 的三种写法

```yaml
next: []                    # 终端：跑完就结束（等价于省略）
next: [fixer]               # 一个下游
next: [fixer, notify@2]     # 多个；裸名跟随 latest，@ver 钉死版本
```

### 2.2 `metadata`（身份）

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `name` | string | ✅ | `^[a-z0-9][a-z0-9._-]{0,62}$`（≤63） | 能力**名**，也是谱系：`next` 引用它、任务记它。改名 = 另一个能力 |
| `version` | int | 可选 | ≥1 | 省略 = **内容寻址**：按 `(name, 执行部分)` 复用已有行；显式给就永不折叠 |
| `displayName` | string | 可选 | 任意 | 纯展示（列表/标题）。**不进 digest**，改它不会让部署变 stale |

命名空间不在这里：它由 API/CLI 的明确上下文决定。文档里可省略 `metadata.namespace`；
CLI 的 `--namespace` 显式选择请求 path，API 直接把 `metadata.namespace` 写进请求 body 时：
与 path 不同 → 400 `namespace_mismatch`；既无 path 上下文也未给 → 400（不静默猜）。
节点 token 是集群身份，不绑 ns；任务所属 ns 由它自己解析到的能力/源决定。

### 2.3 `spec`（正文）

#### `spec.params` — 我要什么

| 项 | 值 |
|---|---|
| 类型 | JSON Schema（对象） |
| 必填 | 否，省略 = `{}`（怎么传都过） |
| 消费方 | 派发时校验上游 payload；建源/改源时校验源配置 |

运行期每个顶层字段注入成环境变量 `ANOTIFY_PARAM_<NAME>`：

- `NAME` = 参数名全大写、非 `[A-Z0-9]` 的字符替换成 `_`，**不插大小写边界**：`sha` → `ANOTIFY_PARAM_SHA`，`repo-url` → `ANOTIFY_PARAM_REPO_URL`，`repoUrl` → `ANOTIFY_PARAM_REPOURL`；
- 两个参数名归一后撞同一个变量名 → 写入期 400 `param_env_collision`（不改名就发不进来，避免"谁覆盖谁"的隐式行为）；
- 非字符串值按 JSON 文本注入（对象/数组也是 JSON 文本），布尔注入 `true` / `false`。

#### `spec.next` — 交给谁

| 项 | 值 |
|---|---|
| 类型 | `[]string` |
| 元素 | `name`（跟随 latest）或 `name@version`（钉死） |
| 上限 | 32 个 |
| 省略 | = `[]` = **终端能力**（跑完就结束），不是错误 |
| 消费方 | 写入时校验目标存在（见 §6）；派生 `edges` 行；`emit <name>` 必须 ∈ 它 |

#### `spec.run` — 怎么跑（唯一实现形式）

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `entrypoint` | string | ✅ | 必须是 `files` 里的一个 key；内容必须以 `#!` 开头 | 否则报 `entrypoint_no_shebang` |
| `files` | map[路径]正文 | ✅ | ≥1 个；路径为**相对路径**，不含 `\`、不含 NUL，≤255 字符；**禁止 `..` 越界**；单文件 ≤256 KiB；合计 ≤1 MiB。**写入期只校验字符串/限额**，不解析符号链接 | 全部展开进工作目录；多文件原样保留。**执行前**做实际目录边界检查：解析符号链接后必须仍在允许目录内，越界 → `files_escape`（非重试配置错误）。全部 `chmod 0600`，`entrypoint` 额外 `0700`；覆盖用户 `workDir` 同名文件会同时改正文与权限，作者需避开重名 |
| `env` | map[string]string 或 `{valueFrom}` | 可选 | 键匹配 `^[A-Za-z_][A-Za-z0-9_]*$`；**不得以 `ANOTIFY_` 开头**（保留给契约变量）；值 = 字面量或 `valueFrom.fieldRef`（`fieldPath` 见下） | 机器事实的**唯一入口**；键名就是脚本里用的 `$NAME`；空串就是空串 |
| `workDir` | string | 可选 | 空 = 每次任务一个独立目录（**默认**）；否则绝对路径模板，可含 `$(vars.NAME)`（必须 ∈ 本能力 `env`）或 `$(params.NAME)`（必须 ∈ `spec.params`）；≤1024 字节；不含 NUL | **不直写 `fieldPath`**；解析后必须是绝对路径、不含 `..` 段、**已存在**（节点不创建）；并发安全由 `spec.concurrency.maxPerNode` 与能力负责，**不做路径锁** |

**文件权限位不是一个配置项**：

- `files` 全部展开后 `chmod 0600`；`entrypoint` 额外 `chmod 0700`，由节点按 shebang 直接执行；
- 要跑兄弟脚本就写 `bash deploy.sh`，不写 `./deploy.sh` —— 没有权限位字段，就不会有"忘了 +x"这种运行到一半才炸的错。

> `timeoutSec` **不在 run 里** —— 它是生命周期的一部分，见下。

#### 取值语法：值从 `env` 进来，字段用 `$(…)`、脚本用 `$NAME`

机器事实只有**一个入口**：`run.env`。每个键的值二选一——字面量，或 `valueFrom.fieldRef` 按 k8s 风格路径引用：

```yaml
run:
  env:
    SOURCE_ROOT:
      valueFrom:
        fieldRef: { fieldPath: vars.SOURCE_ROOT }       # local → node → ns 默认
    NOTIFY_BASE:
      valueFrom:
        fieldRef: { fieldPath: vars.NOTIFY_BASE }       # 通常是 ns 默认值，同 ns 共用
    HOST_IP:
      valueFrom:
        fieldRef: { fieldPath: status.hostIP }          # 节点内置事实
    JOBS: "4"                                          # 字面量
  workDir: $(vars.SOURCE_ROOT)                          # 引用本能力 env 里声明过的名字
  # workDir: /data/$(params.project)                    # 要按参数分目录：引用 spec.params（payload 经参数进来）
```

脚本里就是 `"$SOURCE_ROOT"` / `"$NOTIFY_BASE"` / `"$PWD"`——和任何别的环境变量一样，**脚本正文里没有第二套记号**。

| 规则 | 说明 |
|---|---|
| `fieldPath` 是 k8s 风格路径，封闭集合见 `README.md` | `status.*` / `metadata.*` 是内置事实；`vars.*` 是「这台机器能提供什么」 |
| `vars.*` 必须写进 `spec.requires.values` | 否则 400 `undeclared_need`；内置 path 不用写 |
| 定义里不写来源 | 解析顺序固定 **local → node → namespace**（`config.md §2`）：本机有值就赢，没有才落到节点默认，再落到任务所属 ns 默认 |
| `workDir` 里的 `$(vars.X)` 必须 ∈ `run.env` | 否则 400 `undefined_env_ref` |
| `workDir` 里的 `$(params.X)` 必须 ∈ `spec.params` | 否则 400 `undefined_param_ref`；参数 schema 建议用 `pattern` 把 `..` / `/` 挡在写入期 |
| `workDir` 解析后必须绝对、无 `..`、已存在 | 否则 400 `workdir_invalid`（写入期能判定的）或 `workdir_unavailable`（执行前才知道的） |
| `env` 值不做 env 之间的拼接 | 不写 `PATH: /x:$SOURCE_ROOT`；要拼就在脚本里拼 |
| 能力定义里出现 `{{…}}` 不算错 | 包层模板只在安装包时组装 `{{values.*}}`（`playbook.md §1.4`）；其余 `{{…}}` 就是字面文本。`$(…)` 另说：它在 `workDir` 里合法（见下），在 `run.files` 正文里归 shell |

**为什么变量不自动注入**：一台机器上可能有一堆 `vars`，一个只读仓库的 review 任务没有理由拿到全部。
`run.env` 就是白名单——**只有这里显式引用过的，才会出现在任务环境里**，键名也由能力自己定（`$SOURCE_ROOT`，不是 `$ANOTIFY_NODE_SOURCE_ROOT`）。

两条边界，有意为之：

- **`~` 不是一种语法**：家目录也是机器事实，写 `fieldPath: status.homeDir`。
- **参数化目录要显式引用**：只接受 `$(params.X)`，不接受 `$ANOTIFY_PARAM_*` 这种“隐式拿到所有参数”的写法——引用哪个参数必须写出来，才能对着 `spec.params` 校验；`..` / 相对路径由写入期 `pattern` 与执行前 `workdir_unavailable` 两道挡。**payload 不直接进 `workDir`**：能力看不到载荷形状，值应由源写进 `params` 再进来。

> **同一个 `$(…)`，分两处用**：k8s 里 `$(VAR)` 由 kubelet 在启动前展开——和我们一样，是**字段**。
> 所以 `workDir` 里的 `$(vars.X)` / `$(params.X)` 归我们解析；而 `run.files` 的**脚本正文**里
> `$(…)` 是 shell 的**命令替换**，我们不碰。边界是“字段 vs 正文”，不是字符——两处各有唯一解释。


#### `spec.requires` — 节点要提供什么

两类依赖，一个字段：**要装的命令** 和 **要能解析出的值**。

```yaml
spec:
  requires:                       # 省略 = {} = 什么都不缺
    commands: [git, jq]           # 节点要装的命令
    values: [vars.SOURCE_ROOT]    # 要能解析出的值
```

| 子键 | 类型 | 上限 | 元素 | 谁回答它 |
|---|---|---|---|---|
| `commands` | `[]string` | 16 | 命令名，不含 `\`、不含 NUL | 节点 `LookPath` 探测（`node_capabilities`，静态快照） |
| `values` | `[]string` | 16 | `vars.<NAME>`，`NAME` 匹配 `^[A-Z][A-Z0-9_]{0,63}$`；**不写来源** | 派发时逐名字解析：本机 `vars` → 节点默认值 → 任务所属 ns 默认值（`config.md §2`） |

- 两个子键都可省略（= `[]`）；省略整个 `requires` = `{}`。
- `commands` 缺一条 → 该节点不匹配（`CommandMissing`，message 带上探过的 PATH）；漏到运行期则任务报 `missing_requirement`。
- `values` 缺一条 → 该节点不匹配（`ValueMissing`）；配置还没 apply → `ConfigPending`。
- `status.*` / `metadata.*` 这类内置事实**不用**写（所有节点都有）。

**`NodesAvailable` = `commands` 全部命中 × `values` 全部可解析 × 节点在线 × 机器级配置已 applied × `ns ∈ 申报 ∩ 授权`。**

| 缺什么 | reason | 用户怎么修 |
|---|---|---|
| 命令 | `CommandMissing` / `NoMatchingNode` | 装包（`apt install git`） |
| 值 | `ValueMissing` / `ConfigPending` | 填配置（节点页 / 中枢配置页） |
| 节点 | `NodeOffline` | 让它上线 |

**为什么显式声明，不从用法推导**：

- `values` 理论上能从 `run.env` 推导（脚本只读 env）——但推导结果也得存下来才能在写入期回答“这台机器能不能跑”，不如写明白；
- `commands` 从来推导不出来：脚本正文里的 `git` 是任意字符串，只有声明才能变成可查询事实。

**冗余怎么处理**：写了没用 → 无害，不校验；用了没声明 → 写入期 400。

> 写入期**不校验「中枢有没有这个值」**（值也可能来自节点，两边都没有是派发期才知道的事），也**不探测命令**（`LookPath` 只在节点上做）。
>
> 为什么命令和值不拆成两个平级字段：它们回答的是同一个问题——“这台节点能不能跑我”——也共用一个 condition、一处 UI 提示、一次匹配。拆开就会多一份校验、多一处上限、多一个要用户记住的名字；`commands` / `values` 两个子键已经把区别说清了。

#### `spec.lifecycle` — 一次运行的生命周期

| 字段 | 类型 | 默认 | 取值 | 语义 |
|---|---|---|---|---|
| `idempotent` | bool | **false** | true / false | `true` = 明确幂等，允许对**短暂基础设施失败 / 失联**自动重试；`false` = 重放不安全，任何失败默认终态 |
| `maxAttempts` | int | **3** | ≥1 | **总尝试次数（含第一次）**，不是“额外重试次数”；超过后按原因进 `failed` / `abandoned` |
| `resultTtlSec` | int | **604800**（7 天） | ≥0 | 结果（日志 / 产物 / 系统创建的任务目录）保留期（秒）。`0` = **永不清理** |
| `timeoutSec` | int | **0** | 0..86400（24h） | 本地执行超时，**从子进程启动起算**。`0` = 不设本地执行超时，但运行中的任务继续续租（不是固定总时长） |

**重试、租约与 attempt（V4-D4）**

| 失败 / 触发 | 自动重试？ | 终态 |
|---|---|---|
| `idempotent: true` + 短暂基础设施失败（`node_lost` / `lease_expired` / `infra_transient`） | ✅ 在 `maxAttempts` 内 | 超过则 `abandoned`（失联 / 租约丢失）或 `failed`（可判定失败） |
| `idempotent: false`（默认）的任何失败 | ❌ | `failed` / `abandoned` |
| 普通脚本非零退出（含 `anode emit` 退出 5 导致脚本失败） | ❌ | `failed` / `script_failed`（保留退出码与日志；exit 5 不是 infra 事实） |
| 确定性配置错误（`workdir_unavailable` / `home_unavailable` / `missing_requirement` / 参数校验失败） | ❌ | `failed` |
| `timeoutSec` 到点 | ❌ | `failed`（不因超时自动重跑） |
| 用户取消 / 放弃 | ❌ | `cancelled_by_user` / `abandoned_by_user` |
| `stale_config`（`task.accept` 前配置校验失败） | ❌（不计 attempt） | 回 `pending` / `waiting_for_config`，带退避或换候选节点 |
| `slot_unavailable`（`task.accept` 前无真实空槽 / 预算已满） | ❌（不计 attempt） | 回 `pending` / `waiting_for_slot`，带有界退避；不是脚本失败 |

- **同一逻辑 task 只有一行**：`attempt` 是计数器，不新建 task_id；`parentTaskId` / `parentRef` 只表示业务 emit 的父子，不表示 attempt 父子。
  历史执行绑定另存于 `task_attempts`（每 attempt 一行，含 node / lease / slot），不能被当前 task 行覆盖；公开诊断投影可选，逻辑任务仍是一行。
- **重试是同一逻辑任务**：钉住原版本、原参数、原 digest，以及本次任务的 namespace 默认输入；不改写“跑的是哪一版”，也不重新做参数对齐。
- **换节点重试重新解析机器值**：`local` / `node` 值按**本次执行节点**重新解析，`configRevision` 跟着该 attempt 的节点；不能把 A 机目录带到 B 机。
  每个 attempt 解析完成后，运行期间不再受后续配置更新影响；ns 输入由中枢保留可复现载体（不写成公开 env 值）。
- **attempt 计数**：配置校验与真实空槽原子预留在 `task.accept` 前；`attempt++` 只在**中枢 `lease.grant` 事务生效、授权启动时**发生。排队（`waiting_for_slot`）不计 attempt、不耗租约；grant 后未能启动也不能倒减 attempt 或改成 accept 前拒绝。
- **租约参数（全目录一致）**：基础租约 **60s**、续租间隔 **20s**、TERM 宽限 **5s** 后 **KILL 整个进程组**。
  续租必须有 ACK 与本地 deadline；失联时**尽力**停止旧进程（外部进程不保证停得住）。
- **旧 attempt 作废**：重试开始后，旧 attempt 的任务 token 立刻失效；迟到的 commit / emit / 结果由**中枢权威检查**拒绝（`token_invalid` / `task_not_running`），节点本地只做预检。
- **启动前调度拒绝不是执行失败**：`task.reject.reason` 封闭为 `stale_config | slot_unavailable`，均发生在 accept / grant 前，不计 attempt、不授 lease、不消耗非幂等重试预算；分别回 `pending` 的 `waiting_for_config` / `waiting_for_slot`，带有界退避，不能热循环。无槽不能伪报 `stale_config`；未知 reason fail-closed。`missing_requirement` / workDir / HOME / export 错误仍在 grant 后计 attempt 并 `failed`，不进入 reject。
- **infra 事实只能来自运行器**：`node_lost` / `lease_expired` / `infra_transient` 由运行器结构化判定；脚本的退出码（包括 emit 的 exit 5）不是 infra 事实。
- **只承诺 at-least-once**：不虚假保证外部 exactly-once；业务副作用仍需作者自己幂等。
- **退避**：attempt 间有退避，退避不新增槽位占用、不耗租约；旧执行的物理占用仍须等真正回收才释放。具体参数实现时定，但语义必须是“冷却后再试”，不是忙等。
- **`failed` vs `abandoned`**：`failed` = 有明确失败原因（脚本 / 配置 / 超时）；`abandoned` = 节点失联 / 租约丢失且无法确认结果。两者都进终态，不互相顶替。
- **取消 / 放弃的收尾**：立即停止授权、下发停止并停止续租；节点按历史 task / attempt / lease / slot 绑定先 TERM，5s 后 KILL 整个进程组。**槽位在进程组真正回收后才释放**；旧回收事实只清匹配的旧占用，不清新 attempt，也不恢复旧 result / emit 授权。终态、重连或新 hello 都不是回收证据。

#### `spec.concurrency` — 能同时跑几个

| 字段 | 类型 | 默认 | 取值 | 语义 |
|---|---|---|---|---|
| `maxPerNode` | int | **1** | ≥1 | 同一能力在**一台节点**上最多同时跑几个任务 |

实际并发 = `min(该节点空闲槽位, maxPerNode)`。两个数各管一头：

| 数 | 在哪 | 管什么 |
|---|---|---|
| 节点槽位 `limits.total` | 中枢的节点配置 / 本机 `anode.yaml`（`config.md §4`） | **这台机器**能同时跑几个任务，与是哪个能力无关 |
| 能力预算 `maxPerNode` | 本节 | **这个能力**能吃几个，与是哪台机器无关 |

`limits` 只有**本机 + 节点**两层（`config.md C16`）：跨 ns 共享同一组槽位，**没有 ns 级配额**。

- **超了不失败**：任务停在 `pending`（`waitingReason: waiting_for_slot`），不占槽位、不耗租约；
- **`timeoutSec` 从进程启动起算**，排队时间不计入；
- **不做路径锁**：`workDir` 指向共享目录且 `maxPerNode > 1` → 写入期 warning `workdir_shared_concurrency`（不拦，但得确认那个目录真能并发）；
- 跨节点的全局总量（这个能力全世界最多几个）本期不做，见 §10。

> 为什么不用 `workDir` 派生串行：路径锁是**隐式耦合**——两个能力指向同一物理目录会互堵，只读任务也被堵死；而预算是**声明**，能写入期校验、能在 `status` 里解释「为什么这个任务在等」。

### 2.4 `status`（全部派生；API 写 400，CLI 导入剥离）

| 字段 | 类型 | 值 | 说明 |
|---|---|---|---|
| `ready` | bool | — | 四条 `conditions` 全为 `"True"` 才 true |
| `version` | int | — | 实际解析到的版本（省略 version 时由内容寻址分配） |
| `digest` | string | `sha256:…` | 内容指纹 = 执行部分的身份（见 §3） |
| `conditions` | array | 见下 | 为什么没就绪 |
| `nodes` | array | `{name, state}` | state ∈ `deployed` / `stale` / `missing` |
| `resolved` | object | 见下 | 从图反推（每个 `next` 各算各的） |
| `observed` | object | 见下 | 最近一个逻辑任务的真实运行（见 §5） |
| `installedFrom` | string? | 安装回执 id | 只记**初次来源**；后续复用不覆盖（`playbook.md §7.2`） |

`status` **不是 apply/写入输入**：API 直接写 → 400 `status_not_writable`；CLI 的 apply/import 在提交前**剥离 `status`**（既定 apply 忽略语义在 CLI 侧落实），不是让 API 静默忽略。

**`conditions[].type` 只有这四个**：

| type | false 的含义 | 谁能让它变 true |
|---|---|---|
| `Implemented` | spec 过不了校验（只可能来自直接改库；正常写入路径写不进来） | 改能力定义 |
| `NodesAvailable` | 没有节点同时满足：在线（online）、机器级配置已 applied、`ns ∈ 申报 ∩ 授权`、`requires.commands` 全部命中、`requires.values` 全部可解析 | 接机器 / 装包 / 补节点或 ns 配置 / 修授权 |
| `DownstreamResolved` | `next` 里某个名字解析不到 | 补那个能力，或改 `next` |
| `Deployed` | 没有节点持有当前 digest | **人工点部署**（`POST …/capabilities/{name}/deploy`，见 §5） |

**condition reason 全集**（新增要改表，不能随手造）：

| type | 允许的 reason |
|---|---|
| `Implemented` | `Valid` / `Invalid` |
| `NodesAvailable` | `Valid` / `NoMatchingNode` / `CommandMissing` / `ValueMissing` / `ConfigPending` / `NodeOffline` / `NamespaceUnauthorized` |
| `DownstreamResolved` | `Valid` / `NotFound` |
| `Deployed` | `Valid` / `NotDeployed` / `Stale` |

词义补充：`NamespaceUnauthorized` = 任务 ns 不在该节点「申报 ∩ 授权」；`NoMatchingNode` = 其它条件不满足的兜底；
`Stale` = 节点持有旧 digest；`NotDeployed` = 一台节点都没有当前 digest。

每条形如 `{type, status, reason, message}`：

- `status` 是**字符串** `"True"` / `"False"`（不是 bool）——以后要加 `"Unknown"` 不用改类型，跨工具渲染也不用特判；
- `reason` 是稳定机器码，取值举例：`Valid` / `NoMatchingNode` / `CommandMissing` / `ValueMissing` / `ConfigPending` / `NodeOffline` / `NotFound` / `NotDeployed`；
- `message` 是人话，可以变，不参与任何判断。

> **能力 `status` 只说"缺哪个名字"，不说变量的值**（值属于机器事实，写进能力文档就是第二真相，还会泄漏路径/主机信息）。想看值去看节点页。

**`resolved.next[]`**：`{name, version, digest, ready, params, missing?}`。
`params.required` 是该下游**自己**的必填参数（同名直取），不是多个下游的并集——每次 `emit` 只对一个下游负责。

**`observed`**：

| 字段 | 值 |
|---|---|
| `taskId` | 最近一个逻辑 task 的 id（不杜撰时间；无保留历史时为空） |
| `emits[]` | `{to, fields[], ok}` —— 这次运行里每一次 emit 交给了谁、带了哪些字段 |
| `pruned[]` | `{to, reason, detail}` —— 被剪掉的下游及原因（如 `param_missing`） |
| `tasks{}` | `{total, failed}` —— 够判断「最近是不是老挂」，不做统计报表 |

## 3. digest 边界（逐字段，不许有「大概」）

```
# 第一步：折叠键（判“是不是同一行”），显式排除 version / next / displayName
foldKey = sha256( canonical{
  metadata.name,
  spec.params,
  spec.requires,
  spec.lifecycle,
  spec.concurrency,
  spec.run
} )

# 第二步：部署 digest（含分配/显式给的 version）
digest = sha256( canonical{
  metadata.name,
  metadata.version,
  spec.params,
  spec.requires,       (commands / values)
  spec.lifecycle,      (idempotent / maxAttempts / resultTtlSec / timeoutSec)
  spec.concurrency,    (maxPerNode)
  spec.run            (entrypoint / files / env / workDir)
} )
```

**不在 digest 里**：

| 字段 | 为什么 |
|---|---|
| `metadata.displayName` | 纯展示；改它不该让部署变 stale |
| `spec.next` | **路由属于图，不属于内容**。改线不该产生新版本 |

**折叠键与 digest 是两步**：省略 `version` 时，先用 `foldKey`（不含 `version` / `next` / `displayName`）判断是否复用已有行、没有则分配下一个 `version`；
再用**含 `version` 的 digest** 作为部署身份。这样“只改 next”不会长出新版本，也不会把旧行误认成新内容。

**显式 `version` 的内容不可变**：同一 `(namespace, name, version)` 的正文（`spec.params/requires/lifecycle/concurrency/run`）不同时，**必须换 version**；
直接改 run/params 等同版内容 → 400 `version_content_immutable`。`next` / `displayName` 是既定例外，可原地变化。

这条边界的直接推论，必须写进验收：

> **改 `next` 不产生新版本、不触发任何节点重推。** 改 `run.files` 一定产生新版本。

`next` 仍然是 `spec` 的一部分（同一份文档、同一份 YAML、一起导出导入），只是**不参与内容寻址**；处理方式和 `displayName` 相同，理由也相同：它不是「节点要跑的那份东西」。

**改 `next` 到底发生了什么**（原文如此，不留解释空间）：

1. 同一 `(namespace, name, version)` 行的 `spec.next` **原地更新**，`digest` 不变，不新增 `capability_versions` 行；
2. 该能力的 `edges` 行在事务内整体替换（见 §8）；
3. 落一条事件 `capability.routing_changed`（旧 `next` → 新 `next`）；
4. **不重推任何节点**（节点上跑的东西没变）；下一次派发按新路由走。

> 反过来：想"把新路由也变成新版本"，就显式改 `version` —— 那是另一条能力版本，不是本文推荐的日常用法。

---

## 4. 运行语义

### 4.0 谁在 emit：一个接口、一个令牌

节点上只有一个守护进程 **`anode`**，它**只开一个监听**。能力不关心自己是什么形态
（脚本、二进制、进程内的实现都一样），要做的事只有一件：
**带着本次任务的令牌，调这个 HTTP 接口。**

```yaml
# anode.yaml —— 默认就这样，不用配
listen: tcp://127.0.0.1:0        # :0 = 内核挑一个空闲端口
# 显式要外部可达（自己负责暴露面）：
# listen: tcp://0.0.0.0:7070
# listen: unix:///run/anode.sock
```

启动后把**真实地址**注入每个任务：

```bash
ANOTIFY_EMIT_ENDPOINT = http://127.0.0.1:41237   # 带协议与端口
ANOTIFY_TASK_TOKEN    = <本次任务的高熵令牌>
```

```bash
# 脚本里（CLI 只是薄封装）
anode emit fixer --data '{"file":"a.go"}'

# 它等价于一次普通 HTTP 请求 —— 任何语言的标准库都能发
POST $ANOTIFY_EMIT_ENDPOINT/emit
Authorization: Bearer $ANOTIFY_TASK_TOKEN      # 凭据走头，不进 payload
Content-Type: application/json

{ "to": "fixer", "data": { "file": "a.go" } }
```

**`ANOTIFY_` 基础契约变量如下（A2 在原清单外正式新增 `ANOTIFY_ARTIFACT_DIR`，原变量与参数前缀不变）**（`PATH` / `HOME` 基线与 `envPassthrough` 见下）：

| 变量 | 值 |
|---|---|
| `ANOTIFY_EMIT_ENDPOINT` | 本次任务的 emit 端点（协议 + 地址 + 端口） |
| `ANOTIFY_TASK_TOKEN` | 本次任务的高熵令牌 |
| `ANOTIFY_TASK_ID` | 本次任务 id（只读，用于日志关联） |
| `ANOTIFY_PARAM_<NAME>` | `spec.params` 的顶层字段（规则见 §2.3） |
| `ANOTIFY_ARTIFACT_DIR` | 节点在系统自有 `exportRoot` 下为本 `(taskId, attempt)` 创建的唯一导出目录（0700）的绝对路径 |

**产物登记入口（V4-D5 A2）**：脚本显式把待导出的普通文件写入该目录，例如：

```bash
mkdir -p "$ANOTIFY_ARTIFACT_DIR/reports"; cp report.md "$ANOTIFY_ARTIFACT_DIR/reports/report.md"
```

节点完成后仅安全枚举本 attempt 目录（禁止符号链接、非普通文件、越界），按 A1 限额托管复制、上传，成功后才以 `task.result.artifacts` 的 `[{path,sha256}]` 引用；`path` 相对此导出目录。空目录可真实返回 `[]`；创建/注入/枚举失败或安全边界违规必须以 `artifact_export_invalid` 或 `files_escape` 明确置 `failed`，不能假装空成功。每次创建使用唯一随机后缀，同 task 重试/旧 attempt 重叠及多 task 共用 `workDir` 都不混产物；不使用固定 `workDir/.anotify-artifacts` 存在就拒绝的约定，也不在用户目录内创建/删除专用导出目录。
新变量由节点注入，`run.env` / `envPassthrough` 不得覆盖；不进 `vars`、配置 hash 或公开 env 值快照。不新增 `run.artifacts`、第四 CLI、魔法 stdout 或额外宿主 env 继承。

任务环境是**干净的**：除上表、`PATH` / `HOME` 基线、`run.env` 显式声明的键与 `envPassthrough` 白名单以外，不继承节点/守护进程的完整环境。
基线只有 `PATH` / `HOME`（`HOME` 取 `status.homeDir`）：

- **PATH**：取 **`anode` 守护进程启动时的基线**，`requires.commands` 探测与任务初始 PATH 是**同一份**；
  不引入按 task 动态探测 PATH 的框架。
  **暂不支持 `run.env` 覆盖 `PATH`**：写 `PATH` → 400 `env_invalid`。脚本自行改 PATH 属于作者逻辑，不在 `requires.commands` 保障范围。
- **HOME**：默认 `status.homeDir`；允许 `run.env` 显式 `HOME`。字面量不是绝对路径 → **写入期 400 `env_invalid`**；执行前不存在 / 不是目录 → **`home_unavailable`**（任务 `failed`，确定性配置错误，不自动重试）。
- **凭证透传**：节点可配置 `envPassthrough`（只写变量名，`anode.md §3.2`），把本机已有的 `SSH_AUTH_SOCK` /
  指定 CI 凭证变量带入任务；值不回传中枢、不进 `vars`、不主动记录 env 快照。
  `PATH` / `HOME` / `ANOTIFY_` 开头的变量**禁止**列入 `envPassthrough`；`run.env` 与白名单同名时**显式 `run.env` 优先**；
  未列入白名单的非 `ANOTIFY_` 变量一律不继承。任务用到的名字可在 `envDiagnostics` 标 `passthrough`，不回传值/完整名单。
- **不是 sandbox**：透传的是宿主已有权限。系统**不承诺**阻止脚本把密钥写进 stdout / 日志，能力作者对输出负责。

节点的 `vars` **不会自动注入**——它们是节点的资源清单，`run.env` 是白名单，
只有显式引用过的那几个才会以能力自己选的名字出现在任务环境里。

**为什么默认是 TCP 而不是 unix socket**：这是「任何语言都能调」的前提。
TCP + JSON 在 Python / Node / Go / Java 里都是标准库五行以内；unix socket 在
Python（要手搓 `http.client`）、Java（几乎得手写 HTTP 报文）里都要额外胶水。
`:0` 让内核挑端口，所以也不用用户配、不会撞端口。

**身份来自令牌，不来自通道**：`ANOTIFY_TASK_TOKEN` 是高熵、每任务一个的凭据，
守护进程按 `token → 任务` 查出 task / trace / 能力版本。

**本地 emit 网关只做预检，中枢才是权威**：节点检查 token、任务是否运行态、目标是否 ∈ `next` 之后，
把 emit 请求**转发中枢**；下游 task 的创建、`id` 幂等、跨节点派发、旧 attempt 拒绝都由中枢完成。
节点不得绕过中枢在本地自建下游任务。所以：

- 跨用户、跨容器、跨机器的调用者都是同一条路径；
- 脚本 fork 出的子进程继承 env，仍然只能发**父任务**的 emit；
- 没有第二种调用形态（没有"进程内句柄"那条路径）——一种凭据，一条代码路径，一份测试面。

**不变量 I8（emit 的存活窗口）**：

> 一个 emit 必须被归属到一个**当前处于运行态**的任务。任务结束（完成 / 失败 / cancel /
> 租约过期）后，迟到的 emit 一律**拒绝**，不补发、不重投。

这条由守护进程在**一处**守：收到请求先 `token → task`，任务不在运行态直接拒。
（所以实现上换 socket 布局、换监听方式都不影响它——它是语义，不是实现。）
重试开始后，旧 attempt 的任务 token 立刻作废；迟到请求同样按 I8 拒绝。

要「慢活」就把那部分做成**下游能力**，让链条继续；不允许一个已经结束的任务从背后补一刀，
否则「这份产物属于哪次运行」永远说不清。

### 4.1 `emit <name> --data <json>`

- 一次 `emit` 触发**一个**下游：建一条任务，目标 = `name` 解析到的版本，版本与 digest **钉在任务上**。
- `name` 必须 ∈ 本能力 `spec.next`。不在里面 → 运行期拒绝，`target_not_declared`（**不静默吞掉**）。
- 不写目标（`emit --data …`）→ 运行期拒绝，`400` / CLI `2` / `missing_target`，**不派发**。
  这不是剪枝：`no_target` 只用于“源事件没有 `targets`”的场景（事件 200 接收但无 task），不是第二种 emit 模式。

一次任务里可以 `emit` 0..N 次；每次是一轮独立派发。

**显式幂等键（V4-D4，可选）**

```bash
anode emit fixer --id review-a.go --data '{"file":"a.go"}'
# 等价：POST /emit  {"to":"fixer", "id":"review-a.go", "data":{"file":"a.go"}}
```

| 情况 | 行为 |
|---|---|
| 不带 `id` | 每次 emit 都是新派发（行为不变） |
| 同一 task + 同一 `id`，跨 attempt 的**同请求** | 只派发一次；重复请求返回**旧结果**（`taskId` / `to` / `version` 不变） |
| 同一 task + 同一 `id`，但 `to` / `data` 不同 | **409 `emit_id_conflict`**，不静默覆盖 |
| 不同 task 用了相同 `id` | **不合并**（幂等域只在本逻辑任务内） |
| 不同版本实现 / 不同参数 | 不是同一逻辑任务，不继承旧结果 |
| 只用调用序号区分 | 不做自动去重；要幂等就显式给 `id` |

`id` 只防“同一次逻辑派发”被重复提交，并不把下游业务副作用变成 exactly-once；
下游是否幂等仍由下游能力自己负责。

**返回值就是判据**（脚本据此决定要不要 `set -e` 退出）：

| 结果 | HTTP | CLI 退出码 | CLI stdout |
|---|---|---|---|
| 接受，任务已建 | `200` | `0` | `{"taskId":"…","to":"fixer","version":2}` |
| 目标不在 `next` / 没给目标 | `400` | `2` | `{"error":"target_not_declared"}` / `{"error":"missing_target"}` |
| 令牌无效或过期 | `401` | `3` | `{"error":"token_invalid"}` |
| 任务已结束（I8） | `409` | `4` | `{"error":"task_not_running"}` |
| 同 `id` 不同请求 | `409` | `6` | `{"error":"emit_id_conflict"}` |
| 参数剪枝（`param_missing` / `param_schema` / `param_type`） | `200` | `0` | `{"taskId":null,"eventId":"…","to":"fixer","version":2,"pruned":{"reason":"param_missing","detail":"需要 file"}}` |
| 端点不可达 / 请求结果未知（可能已派发） | — | `5` | `{"error":"emit_unreachable"}` |

非零退出时只写**一行** JSON 到 stdout，原因码同时写 stderr 的人话一行——脚本可以 `if ! anode emit x …` 判断，不用抓字符串。

- **剪枝不是 4xx**：事件已被接收、下游没有建 task；回执 `taskId: null` + `pruned.reason`，CLI 退出码 `0`。不要把“成功接收但没派发”伪装成已建任务。
- **退出码 5 不等于 infra 失败**：它只表示“结果未知（可能已派发）”。脚本因此非零仍然是 `script_failed`，不会自动整任务重试；要安全重试就带 `--id` 用**相同规范化请求**重发（请求体必须可复现）。
- **缺 `id` 不能自动安全重发**：系统不保证“没派发”，也不按调用序号去重。
- 剪枝回执与建 task 回执一样进入幂等结果存储：同 task + 同 `id` 的重复请求返回同一份回执（含 `taskId:null`）。

### 4.2 参数对齐：同名直取

```
上游 payload 的字段 → 下游 params 的同名字段
```

- payload 里多余的字段 → 忽略。
- 下游 `params` 里**有 default** 的字段缺失 → 用 default。
- 下游 `params` 里**必填且无 default** 的字段缺失 → 剪枝 `param_missing`，**留痕**（事件 + `status.observed.pruned`），emit 回执 `taskId:null`（见 §4.1）。
- 类型不符 / schema 不符同样按剪枝处理（`param_type` / `param_schema`），不是 4xx；词表见 §6.2。
- **没有 mapping、没有 literals、没有表达式。** 需要改形状就在上游把字段名说对。

### 4.3 一次派发内的引用快照

一次 `emit` 的解析结果在整次派发内复用（`refCache`）。同一批扇出不出现「半新半旧」。

---

## 5. status 的推导规则

| 字段 | 来源 | 什么时候变 False |
|---|---|---|
| `digest` / `version` | `capability_versions` | — |
| `Implemented` | `artifact.Validate` | 校验不过（正常写不进来） |
| `NodesAvailable` | 在线状态 × `node_capabilities` × `requires.commands` × 节点「applied 配置」快照 × 节点默认值 × 任务 ns 默认值 × `requires.values` × `ns ∈ 申报 ∩ 授权` | 命令缺 → `CommandMissing`；值解析不出来 → `ValueMissing`；配置没 apply → `ConfigPending`；离线 → `NodeOffline`；ns 未授权 → `NamespaceUnauthorized` |
| `DownstreamResolved` | `next` 每个名字能否解析 | 任一目标不存在 |
| `Deployed` | `deployments` | 有节点持有旧 digest，或一台都没推 |
| `nodes[].state` | `deployments` × `node_capabilities` | `deployed` / `stale` / `missing`（**已经持有**这份能力的机器；首次部署前恒 `[]`） |
| `deployCandidates[]` | `nodes` × `node_configs`（申报 ∩ 授权）+ 活跃会话 | 部署弹窗的目标列表：`{name, online}`。判据只有“授权覆盖本 ns、不在 draining”，**不看是否已部署** |
| `resolved.next[]` | `ResolveRef` + 目标的 `params` | 目标缺失 → `missing: true` |
| `observed.*` | `events` / `tasks` | — |

**人工部署（不是控制器）**：`POST /v1/namespaces/{namespace}/capabilities/{name}/deploy` body `{version?, nodes?}`；
服务端把目标版本的 digest 写成/更新到 `deployments` 事实（目标节点可指定；**不传 = 全部已授权且在线节点**），
节点在连接/同步时按事实拉取。**install 不会自动部署**，也没有“节点自动收敛”的控制器。

**部署回执也带探测结果**：`capability.deployed`（applied）随帧回 `commands: [{name, found, path?}]`
（= 该能力 `requires.commands` 在本机的探测，与 `hello.capabilities[]` 同口径）。中枢收到后把这一项
并进节点申报（`MergeDeclaredCapability`）。**为什么必须**：申报历史上只有握手 `hello` 一份来源，而部署
落地时节点不会重连 —— 只靠 hello 的话 `NodesAvailable` 会一直停在 `CommandMissing`，能力部署成功却
永远不可派发（症状：`ready` 永远 false）。`failed` 回执不合并（没拿到字节不等于申报有新事实）。

目标的判据是**部署**判据（`DeployCandidateNodes`）：`ns ∈ 申报 ∩ 授权`、不在 `draining`、省略时还要求在线。
**不能**用派发判据（`EligibleNodes`，它多一条“这台机器已经 applied 过该 digest”）——那样首次部署的候选恒空，
`deploy` 会回 200 但 `deployed: []`，能力永远停在 `NotDeployed`。钉住这条的回归用例在
`internal/store`（候选）与 `internal/server`（省略目标 = 新能力也能部署）两处。

`ready = 前四条 condition 全 "True"`。

**`observed` 的窗口**：只看**最近一个逻辑 task** —— `emits` / `pruned` 是该 task 的派发事件（跨 attempt 的有效事实合并），
`tasks` 统计明确是该 trace 的任务；不把多次运行混在一起。清理后数组为空，并注明“无保留历史”；不要杜撰“最近运行时间”。

**全部现算，不落库、不新增表。** 这也是 `status` 永远不会与现实不一致的原因。
`status` 保留为只读面：仍会返回 `version` / `digest` / `conditions` / `nodes` / `resolved` / `observed`，
其中 `resolved` / `observed` 的**明细数组不能改成仅计数**（它们是诊断依据）。

**没有 `observedGeneration` / `lastTransitionTime`**：它们是落库状态的产物，与「status 从事实现算」冲突。
也不需要 reconciler：每次读 `status` 时从 `events` / `tasks` / `deployments` / 节点上报等事实派生；
要知道「何时变的」，查 `deployments.updated_at` 或事件时间。

> 为什么保留 `status` 但不去掉派生：用户需要看得见“能不能跑 / 缺什么 / 跑到哪”；
> 但它是事实的**读时投影**，不是第二份真相，更不是一个需要控制循环去收敛的对象（`decisions.md` V4-D1）。

---

## 6. 校验与错误码

### 6.1 写入期（`POST/PUT`，返回 400）

| 码 | 语义 |
|---|---|
| `impl_missing` | 没有 `run`（脚本是唯一实现形式） |
| `entrypoint_no_shebang` | `entrypoint` 不在 `files` 里，或内容不以 `#!` 开头 |
| `files_invalid` | **写入期**只校验路径字符串/限额：绝对路径、含 `..` 越界、含 `\`/NUL、超 255 字符，或超限（单文件 256 KiB / 合计 1 MiB）。不解析符号链接 |
| `env_invalid` | `env` 键不匹配 `^[A-Za-z_][A-Za-z0-9_]*$`、以 `ANOTIFY_` 开头，写了 `PATH`（本轮不支持覆盖 PATH），或 `HOME` 字面量不是绝对路径 |
| `workdir_invalid` | `workDir` 超 1024 字节、含 NUL、字面量不是绝对路径、引用写法不在 `$(vars.X)` / `$(params.X)` 之列 |
| `undeclared_need` | `run.env` 的 `fieldPath` 用了 `vars.*`，但没写进 `spec.requires.values` |
| `unknown_field_path` | `fieldPath` 不在 `README.md`「`fieldPath` 封闭集合」里 |
| `non_path_field` | `workDir: $(vars.X)`，而 env 里 `X` 的 `fieldPath` 不是目录类（如 `status.capacity.cpu`） |
| `undefined_env_ref` | `workDir` 引用了 `run.env` 里没声明的名字 |
| `undefined_param_ref` | `workDir` 引用了 `spec.params` 里没声明的参数 |
| `concurrency_invalid` | `spec.concurrency.maxPerNode` < 1 或不是整数 |
| `param_env_collision` | 两个参数名归一后撞同一个 `ANOTIFY_PARAM_*` |
| `requires_invalid` | `requires` 不是对象；`commands` 元素含 `\` 或 NUL、超 16 个；`values` 元素不是 `vars.<NAME>` 形状（`NAME` = `[A-Z][A-Z0-9_]{0,63}`）或超 16 个；出现未知子键 |
| `unknown_target` | `next` 里的名字在库里解析不到（只用于**单建**，见 §6.3） |
| `version_content_immutable` | 显式 `version` 已存在且执行正文不同；必须换 version（`next` / `displayName` 例外） |

### 6.2 运行期 / 派发期（任务失败 / emit 被拒，留事件）

| 码 | 时机 | 语义 |
|---|---|---|
| `target_not_declared` | emit | 目标不在本能力 `spec.next` 里 |
| `missing_target` | emit | `emit` 没给目标 → `400` / CLI `2`，不派发（不是剪枝） |
| `token_invalid` | emit | 令牌不属于任何任务，或已作废 |
| `task_not_running` | emit | 任务已结束（I8） |
| `emit_id_conflict` | emit | 同一逻辑任务同一 `id` 的请求体不同（409） |
| `node_lost` | 运行 | 节点失联（可重试的短暂基础设施失败，前提 `idempotent: true`） |
| `lease_expired` | 运行 | 租约过期 / 续租失败（同上） |
| `infra_transient` | 运行 | 其他可判定的短暂基础设施失败（同上） |
| `max_attempts_exceeded` | 运行 | 超过 `maxAttempts` 总尝试次数，按原因转 `failed` / `abandoned` |
| `downstream_missing` | 派发 | 名字当时解析不到 → 任务失败 + 事件 |
| `param_missing` | 派发 | 同名直取取不到且无 default → 剪枝（不失败，回执 `taskId:null`） |
| `param_schema` | 派发 | 参数不满足下游 JSON Schema → 剪枝（不失败） |
| `param_type` | 派发 | 整个值只有一个引用时类型不匹配 → 剪枝（不失败） |
| `no_target` | 派发 | **源**的 `targets` 为空：事件 200 接收、剪枝留痕、不建 task；emit 缺目标不走这里（走 `missing_target` 400） |
| `event_schema` | 投递 | payload 不符合源 `payloadSchema` → 剪枝（不失败，不建 task） |
| `script_failed` | 运行 | 脚本非零退出；**默认不自动重试**（含 emit exit 5 导致的非零） |
| `files_escape` | 运行（执行前） | `run.files` 展开后越出允许目录（符号链接/实际路径）；**非重试配置错误** |
| `artifact_export_invalid` | 运行 / 结果收集 | A2 系统导出目录创建、注入或枚举失败，或导出文件不符合普通文件 / 路径 / 限额约束；明确 `failed`，不返回假空产物，不自动重试 |
| `stale_config` | 调度（`task.accept` 前） | 机器级 `configRevision` 与本机 applied 不一致；不计 attempt、不授 lease、回 pending / waiting_for_config，带退避 |
| `slot_unavailable` | 调度（`task.accept` 前） | 无真实空槽、本地已有占用或预算吃满；不计 attempt、不授 lease、回 pending / waiting_for_slot，带有界退避；不是 script_failed |
| `cancelled_by_user` | 运行 | 用户取消；节点 TERM/KILL 进程组，槽位回收后释放 |
| `abandoned_by_user` | 运行 | 用户放弃；同上 |
| `workdir_unavailable` | 运行（`lease.grant` 后启动前） | `workDir` 展开后不可用：变量缺失 / 不是绝对路径 / 含 `..` / 不存在 / 不是目录；attempt 已计，`failed`，不自动重试 |
| `home_unavailable` | 运行（`lease.grant` 后启动前） | `HOME` 解析后不存在 / 不是目录；attempt 已计，`failed`，确定性配置错误不自动重试 |
| `missing_requirement` | 运行（`lease.grant` 后启动前） | 节点缺 `requires.commands` 里的命令（报错带探过的 PATH）；attempt 已计，`failed`，不自动重试 |
| `ValueMissing` / `ConfigPending` | 部署（condition reason） | `requires.values` 里某个名字节点与中枢都解析不出 / 节点配置还没 applied（不是错误码，是 `NodesAvailable` 的 reason） |

两张表都是**封闭集合**：新增码要改本文。

**错误域不要混用响应体**：

| 域 | 载体 | 例子 |
|---|---|---|
| 公共 API | `Status.reason` | `unknown_field_path` / `emit_id_conflict` |
| 本地 emit 网关 | `{error, message}` + CLI 退出码 | `target_not_declared` / `emit_unreachable` |
| 任务结果 | `Task.status.result.error.reason` | `script_failed` / `files_escape` / `artifact_export_invalid` / `workdir_unavailable` / `home_unavailable` |
| Node 启动前调度拒绝 | `task.reject.reason`；公开 Task 仅投影等待态 | `stale_config` → `waiting_for_config`；`slot_unavailable` → `waiting_for_slot`，不是任务结果失败 |

字面码可以复用，但响应形状与判定时机不同；不要把本地 emit error 当 HTTP `Status`，也不要把剪枝当 4xx。

### 6.3 单建严格、预案宽松

- **单建**（`POST /capabilities`）：`next` 里的名字必须在库里解析得到，否则 400 `unknown_target`——单建有确定的上下文，没必要让用户先拿到一个坏能力。
- **预案 install**（`playbook.md`）：允许先装上游后装下游，装完由 `status.DownstreamResolved` 与体检报告指出缺哪个。

### 6.4 警告（不拦截，响应顶层可选 `warnings[]`）

`warnings[]` 与信封四键**并列**，是响应附加字段，不是资源对象字段；严格信封检查不能把它吞掉。

| 码 | 触发 | 提示 |
|---|---|---|
| `workdir_shared_concurrency` | `workDir` 非空（共享目录）且 `concurrency.maxPerNode > 1` | 该目录会被多个任务同时写；不确定能并发就写 `maxPerNode: 1` |

---

## 7. 执行形式：只有脚本

| 项 | 规定 |
|---|---|
| 实现形式 | **只有 `spec.run`（脚本）一种**。没有 `handler`、没有 `spec.type`、没有"进程内实现" |
| 谁执行 | 节点上的 `anode` 起子进程执行 `entrypoint`；中枢只派发、记账、收事件，**从不执行能力代码** |
| 唯一出口 | 子进程只能通过 `ANOTIFY_EMIT_ENDPOINT` + `ANOTIFY_TASK_TOKEN` 派下游（§4.0）。没有第二种出口，也没有返回值通道 |
| 链尾 | 没有内置推送能力。终端能力就是 `next: []` 的普通脚本，通知由它自己发（§11.3） |
| 系统预置的示例能力 | 和用户建的**没有任何执行差异**：同样是 `run` + `files`，同样要声明 `requires`/部署 |

为什么不留"中枢内置执行器"：它会造出两个执行位置（中枢 / 节点）、两套可见性规则、两套测试面。
一旦有第二种实现形式，"能力"就要能表示"这条能力在谁那跑"——那是新的一层，正是 §1 要避免的东西。

---

## 8. 存储与协议形状

### 8.1 `edges`：派生表

**两类派生投影**：能力边来自 `spec.next`；源入口边来自源的 `spec.targets`。**用户不可直接写 Edge**，也没有 `PUT …/downstreams`；源的 `targets` 通过创建/`PATCH` 源维护，能力的 `next` 通过写能力维护。

| 列 | 说明 |
|---|---|
| `namespace_id` | 命名空间 |
| `from_is_source` | 上游是源还是能力 |
| `from_ref` | 上游引用（源 id 或能力 name） |
| `to_ref` | 下游引用：能力边原样保存 `next` 的写法（裸名 `fixer` 或钉版 `notify@2`）；源入口边保存 `targets[].capability` |
| `position` | 在来源数组（`next` / `targets`）里的下标，保证派生顺序与声明一致 |

主键 `(namespace_id, from_is_source, from_ref, to_ref)`。

- 只有这几列：没有 `from_port`、没有 `mapping`、没有 `literals`、没有 `priority`（§1 五个概念不存在，就没有它们的落点）。
- 重建时机：写能力的同一事务内 `DELETE` 该能力 `from_ref` 的出边再 `INSERT`；**创建源或 PATCH `source.spec.targets` 的同一事务内，删除并重建该源的全部入口边**。都是整体替换，不做差分。
- 安装只对**本次新建**的源写入口边；复用已有源不写 `targets` / 入口边（差异进 `unapplied[]`，见 `playbook.md §4.2`）。
- 读取时机：能力边供 `DownstreamResolved` 解析与前端图渲染；源入口边供源触发派发（`source.md §5`）与前端图渲染。能力派发**不走** `edges`（emit 直接给出目标，见 §4.1）。

### 8.2 `capability_versions`

| 列 | 说明 |
|---|---|
| `namespace_id` / `name` / `version` | 身份；`version` 省略时由内容寻址分配 |
| `digest` | 只覆盖 §3 列出的字段 |
| `spec`（JSON） | 完整 `spec`，**含 `next`**（`next` 不入 digest，但必须存——它是路由的原始事实） |

- 不存任何"端口"的镜像列：能力的 `spec.next` 是能力边的唯一事实，源的 `spec.targets` 是入口边的唯一事实；`edges` 只是两者的投影。
- `next` 变更 = 同一行原地更新（§3），所以"这个版本有哪些出边"永远只有一份。

### 8.3 任务与事件

- **任务**钉住目标引用：`to_name` + `to_version` + `to_digest` + `payload` + 令牌摘要 + 租约/截止时间。派发后上游再改 `next` 也不影响这条已建任务。
- **重试字段**：同一逻辑 task 一行；`attempt`（配置校验在 accept 前，**中枢 `lease.grant` 生效、进入启动时**递增）、`max_attempts`（总尝试次数）、
  `node_name` / `config_revision` 记**当前 attempt** 的执行节点与配置版本；旧 attempt 的令牌摘要作废，迟到 commit / emit / 结果由中枢拒绝。
  `task_attempts` 保留每次历史 node / attempt / lease / slot 与执行输入快照，供回收、历史日志、产物引用与 GC；不被当前 task 行覆盖，不新建 task_id，也不使用 `parent_attempt`（`parentTaskId` / `parentRef` 只表示业务 emit 父子）。
- **phase 小写**：`pending` / `running` / `succeeded` / `failed` / `cancelled` / `abandoned`，与 wire 一致。
- **等待态**：`waitingReason` ∈ `waiting_for_slot` / `retry_backoff` / `waiting_for_config`；带 `nextAttemptAt`，不计 `failed`、不占租约、不计 attempt。
- **命名口径**：存储列用 snake_case（`max_attempts` / `node_name` / `config_revision`），wire 用 camelCase（`maxAttempts` / `nodeName` / `configRevision`）；语义同名，不另造词。
- **emit 去重记录**：`(task_id, emit_id)` → 首次派发结果（含剪枝回执 `taskId:null`）；作用域仅在本逻辑任务内，不跨 task 合并，
  不按调用序号自动去重；不同版本 / 不同参数不继承。
- **运行中值不漂移**：每个 attempt 解析出的 env（机器级 + ns 级）就是该 attempt 的值；解析完成后后续配置更新不改它。
  机器级执行快照在 grant 事务固定，按本次执行节点重新解析；namespace 在逻辑 task 首次解析时钉住，ns 默认输入由中枢保留可复现载体（不是公开 env 快照）。
- **事件**记 `emit`（`to`、`fields[]`、`ok`）与 `prune`（`to`、`reason`、`detail`），`status.observed` 全部由它们现算。
  剪枝 reason（`no_target` / `event_schema` / `param_schema` / `param_type` / `param_missing`）是**观察原因**，不是任务失败码；其中 `no_target` 只表示“源没有 targets”，emit 缺 `to` 是 `missing_target` 错误。
- **结果保留期**由 `lifecycle.resultTtlSec` 决定，默认 **7 天**，`0` = 清理任务跳过它。

### 8.4 保留与 GC（V4-D6）

| 对象 | 默认保留 | 说明 |
|---|---|---|
| 日志 / 产物 / **系统创建**的任务目录 | **7 天**（`resultTtlSec` 默认 `604800`） | 显式 `0` = 永久；清理任务只动自己创建并记录的路径，含系统自有 export/staging/托管副本；活动 attempt 不清理 |
| 用户指定 `workDir` | **永不删除** | 清理不递归删除用户路径；`run.files` 写入只覆盖允许目录内的文件，不跟随链接写出。**用户目录内任何原文件（含脚本生成的产物）都不被 resultTtl 自动删** |
| 需托管保留的 artifact | **7 天副本**（显式 `0` = 永久） | 仅从本 attempt 的 `ANOTIFY_ARTIFACT_DIR` 安全枚举、复制到**独立系统结果目录**再按 `resultTtlSec` 清理；活动 attempt/上传不清理；artifact API 指向托管副本，不指向用户原文件 |
| 任务行（终态后） | **30 天**（`retention.taskTtlSec`，可配置） | 活动任务不清理；终态后开始计时；与事件/trace 各自计时 |
| 事件 / trace 元数据 | **30 天**（`retention.eventTtlSec` 默认 `2592000`，可配置） | 事件从接收时间、trace 从结束时间各自计时；过期物理删除（不做软删） |
| 事件去重记录（`(source, deliveryId)`） | **30 天**，与事件同窗口 | 从首次接收时计时；窗口内同 `deliveryId` 不重复派发，窗口外可再次接收；与实体引用延长保留分开 |
| 安装回执 / `installed_from` | **不跟事件 30 天一起删** | 先清关联执行数据；回执被 `installed_from` 引用时延长保留，不允许悬空外键 |

规则：

- 同 `id` 只保护**事件接收**，不保证下游执行成功；执行失败不是“删掉 `id` 就能重投”的理由。
- `status.observed` 只看**保留窗口内**的事件 / 任务；过期后历史诊断自然缩短，这不是状态丢失（status 本来就不落库）。
- v4 **不加 replay API**：重放由调用方用新 `id` 重新投递。
- **引用组整体清理**：被引用实体与引用方视为一个保留组，最后一个引用释放后才可清理；
  GC 顺序：先清关联执行数据（日志/产物）→ 再考虑事件/去重；任何仍被任务/回执引用的行不得先删。
- **物理边界**：只删系统创建并记录的目录/托管副本；拒绝 `workDir` 与系统清理根重叠（防止误删祖先）；不递归删用户目录。
- **外部 deliveryId 与内部 eventId 分开**：`deliveryId` 是调用方幂等键，`eventId` 是内部事件行 id；去重记录按 `(source, deliveryId)`，事件行不要求 `deliveryId` 全局唯一，窗口过期后可重收不冲突。
- 保留配置是中枢系统配置（`config.md §3.6`），不是 `vars`、不是 node 限额。
- **逻辑删除 ≠ 事实删除**：删能力/源/包后，被任务钉住的 version / digest / attempt 事实保留到对应任务保留期结束；
  “解析目录里看不见”和“保留执行事实”分开处理。
- 开发库直接建 v4 baseline，不引历史 migration（`decisions.md` V4-D6）。

### 8.5 协议（中枢 ↔ `anode`）

**配置子协议**见 `config.md §4.2`（`hello` / `welcome` / `config.changed` / `config.get` / `config.applied`）。
下面是最小**执行子协议**；两者合起来才是完整帧集合，不要再把“配置 5 帧”当全部。

| 帧 | 方向 | 必需字段 | 时序 / 说明 |
|---|---|---|---|
| `task.dispatch` | 中枢 → 节点 | `taskId, assignmentId, proposedAttempt, assignmentExpiresAt, namespace, capability{name,version,digest}, params, namespaceVars, namespaceVarsRevision, configRevision` | 目标用 `{name,version,digest}`，**没有 `port`**；先校验 digest 与 `configRevision == applied`。中枢给出统一派发窗口（初值 30s）；**不携带 lease / attemptToken / slot 授权，不授执行权** |
| `task.accept` | 节点 → 中枢 | `taskId, assignmentId, proposedAttempt, node, slot` | 配置通过且 Node 已原子预留真实空槽才接受；`slot` 必填且有 presence，整数 `0..N-1`，0 合法，缺失 / 负数无效；accept 本身不计 attempt、不授 lease |
| `task.reject` | 节点 → 中枢 | `taskId, assignmentId, proposedAttempt, node, reason` | `reason` **封闭为 `stale_config` / `slot_unavailable`**；两者 pre-accept / pre-grant，不执行、不计 attempt、不发 grant；未知 reason fail-closed |
| `lease.grant` | 中枢 → 节点 | `taskId, assignmentId, attempt, node, slot, leaseId, attemptToken, expiresAt` | 回显 accepted 的同一 slot 与 assignmentId；中枢事务复核 assignment / slot / Node / ns 权限与预算，生成 lease / token 并持久计 attempt；Node 只在匹配 pending reservation 后启动，不任意收到 grant 就执行 |
| `lease.renew` | 节点 → 中枢 | `taskId, attempt, leaseId` | 围绕**同一个中枢 leaseId** 续租；节点不得自生成 lease |
| `lease.ack` | 中枢 → 节点 | `taskId, attempt, leaseId, expiresAt` | 节点以最后一次 ACK + 本地 deadline 判定失联；到期自行 TERM，5s 后 KILL 进程组 |
| `task.result` | 节点 → 中枢 | `taskId, attempt, node, leaseId, phase, exitCode?, result{summary,artifacts,error}, startedAt, finishedAt` | 脚本没有隐式返回值路由 ≠ 节点没有执行结果通道；结果由节点显式上报 |
| `result.ack` | 中枢 → 节点 | `taskId, attempt, accepted` | 中枢权威校验 attempt/lease/token；旧 attempt 的结果拒绝 |
| `task.cancel` | 中枢 → 节点 | `taskId, attempt, leaseId, slot, reason, graceSec` | slot 有 presence（0 合法）；按完整历史执行绑定定位，不能停止同 task 的新 attempt；reason 区分用户取消 / 放弃 / 基础设施 abandoned，TERM 后 5s KILL 进程组 |
| `cancel.ack` | 节点 → 中枢 | `taskId, attempt, leaseId, slot, processGroupReaped` | slot 有 presence（0 合法）；真正回收后才发 `processGroupReaped: true`。中枢验证连接 Node 身份及历史 task_attempts 的 node / task / attempt / lease / slot，只 CAS 清匹配旧占用，重复 ACK 幂等 |

- **时序**：`dispatch → accept/reject → lease.grant → 启动 → (lease.renew/lease.ack)* → task.result/result.ack`；`task.cancel` 可随时抢占并进入 `cancel.ack`。
- 派发帧验证 assignment 关联；已授权执行帧验证 `taskId + attempt + node + leaseId`，取消 / 回收还须匹配 slot。节点本地只预检，中枢是 emit / 结果 / 旧 attempt 的权威。
- `assignmentId` / `proposedAttempt` 只是派发关联信息，不是执行授权；`leaseId` / `attemptToken` 只在中枢 `lease.grant` 时生成，`lease.renew` / `lease.ack` 始终围绕同一个 `leaseId`。
- **物理槽与预留**：slot 是 Node 的整数槽号，不是数据库 instanceId，每槽最多一个能力进程。候选容量只是已知事实提示；grant 必须原子绑定 accepted 的 node + slot 到 instance，不能另挑槽。不存在的 Node、超预算、slot 有未回收占用或 assignment 已过期 / 取消均不得 grant。
- **统一窗口**：reservation（taskId + assignmentId + proposedAttempt + slot）沿 dispatch.assignmentExpiresAt 到期，双方不各猜 30s 起点。同一有效 assignment 的重复 dispatch / accept 返回同一 reservation，不占第二槽；不同 assignment 不共享。未获执行权且本机确知从未创建进程时，才可在过期 / 断连 / assignment 失败后释放预留。
- **晚到 grant**：过期 / 不匹配 reservation 的 grant 不执行；若中枢已计 attempt，走现有结构化基础设施失败 / lease 回收，不倒减 attempt，不伪装成 pre-accept 拒绝。
- **历史回收**：旧 token 不再 current 不妨碍报告真实回收；自然结束、lease 失效或重连后也可补报 cancel.ack，不要求旧执行仍 running 或收到当前 cancel。无匹配历史绑定不能清任何槽；旧 ACK 不清新 occupancy、不恢复旧 result / emit 授权。不能用 task phase 或终态清理代替物理回收。
- **hello / applied 实例事实**：`hello.instances` 每项 slot 与 busy 均有 presence（slot 0 有效）；缺 busy、缺 instances 或空默认数组均为未知 / 无新断言，不是 idle；重复 slot / 非法项不更新安全账本。busy=true 的执行 / 回收中占用给 taskId / attempt / leaseId；pre-grant reservation 可给 taskId / assignmentId / proposedAttempt，不伪造 leaseId 或运行 attempt 授权。busy=false 只有携带相符历史 taskId / attempt / leaseId 才能清该旧 slot 绑定，否则需真实 cancel.ack；无匹配引用的 idle 不清旧 busy。
- **配置与重连**：`config.applied` 可附与 hello 同型的 instances 快照，未附不更新实例账本；本轮删除旧每 instance 多份模型的 wire concurrency，不增加预算，limits.total / perCapability / maxPerNode 不变。预算缩小仍报告超范围存量直至回收；重启 / 重连须恢复或核查持久执行记录与进程组，未确认 reaped 保持 busy / unknown，不能因内存为空或新 hello 到达假报空闲。
- 中枢租约过期可授权新 attempt，允许不同空槽上的短时进程重叠，但未回收旧槽不得复用；不虚假保证外部 exactly-once。
- 节点上报：`node_capabilities`（`requires.commands` 探测结果）+ **机器级**变量快照（本机 + 节点默认值，值只用于匹配与节点页，不进能力侧 `status`，见 §2.4）；ns 默认值不进快照、只随任务内存下发。

### 8.6 落库方式

- 开发库**直接重建**：库里没有需要保留的数据，新形状就是唯一形状。
- DDL 按仓库 `AGENTS.md` 定的 schema 纪律落地（追加迁移块、幂等），**只建新形状、不搬数据、不回填、不做双形状兼容读**。
- 造数必须同步成 v4 形状：`devseed`、e2e 夹具、单测夹具、`playbook.md` 的 manifest 示例。
- **协议里没有 `port` 字段**，也不做版本协商：开发期只有一种 agent。真到了要滚动升级那天再按 agent 版本号协商 —— 那时有真实约束，那时再解决。

---

## 9. 分期

| 期 | 内容 | 门禁 |
|---|---|---|
| **P9-0** | 节点守护进程 `anode`（`cmd/anode`、`anode.yaml`、systemd 单元、Dockerfile、install skill、e2e 夹具） | `go build ./...`、e2e deploy / live_agent / ws_protocol |
| **P9-a** | `spec.next` 形状 + digest 边界 + `edges` 派生 + DDL | `go test ./...`、`make contract`、e2e deploy/ws_protocol |
| **P9-b** | 节点 CLI：`emit <name> --data [--id]`、完整回执（含剪枝 `taskId:null`）与退出码（§4.1） | `live_agent`、`routing` |
| **P9-c** | 派发：同名直取 + 一次派发内快照 + 并发预算（`limits.total` × `maxPerNode`，超限停在 `pending`） | `routing`、`edge_cases` |
| **P9-d** | `status` 端点 + `anotify explain` / `skeleton` | api_contract |
| **P9-e** | 前端：能力页 `next` 编辑、status 面板、图渲染（无端口/候选/优先级） | `frontend`、i18n_coverage |
| **P9-f** | 预案 manifest：`name` / `next` 形态 | `contract`、`real_backend` |
| **P9-g** | 变量与配置：中枢配置（`config.md` M1）+ 节点配置下发（M2/M3）+ 解析优先级 local → node → namespace（M4）+ `spec.requires.values` + `run.env` 的 `fieldPath` 引用 + **最小权限注入**（只注入被引用过的）+ 三层校验 | `deploy`、`live_agent`、`ws_protocol`、`edge_cases` |

---

## 10. 非目标（明确不做）

| 不做 | 理由 |
|---|---|
| **分支（一个能力多个结果）** | 本期不需要。将来用「抽象能力」表达：那是一个新 kind，不是给 `next` 加键 |
| **CRD / `spec.type`** | 内置只留脚本一种形态。类型系统会引入第二个 breaking 轴，等 `next` 稳定后单开一期 |
| **`mapping` / `literals` / 表达式** | 它们抹平「参数对不上」这个信号。改名或拆能力是更好的答案 |
| **`priority` / 候选排序** | 没有候选可排 |
| **`handler`（中枢内置执行器）** | 中枢执行能力会造出第二个执行位置与第二套测试面（§7） |
| **内置推送能力（`notify-phone`）** | 没有中枢执行的能力，也就没有系统预置的特殊能力。种子清单里只有普通脚本示例，和用户建的没有执行差异 |
| **`status` 落库** | 派生值落库就是第二真相 |
| **`lastTransitionTime` / `observedGeneration` / reconciler** | 都需要存状态历史或控制循环，与「status 从事实现算」冲突（`decisions.md` V4-D1） |
| **独立 `CapabilityRevision` kind / `generation`** | 版本就是 `capability_versions` 的行，`next` 原地更新不产生版本；不再套一层对象 |
| **`finalizers` / 级联卸载 / `propagationPolicy`** | 只删清单、保留已建资源；不引所有权协议 |
| **labels / selectors** | API 不新增这类产品功能；过滤用显式参数 |
| **replay API / 历史 migration** | 开发期直接建 v4 baseline；重放由调用方用新 `id` 重投 |
| **exactly-once / sandbox** | 只承诺 at-least-once；节点是宿主权限执行，不阻止脚本输出密钥 |
| **协议兼容层 / 双读分支** | 开发期只有一种 agent。多形状兼容是给有线上数据的项目用的 |
| **跨节点总并发（`maxGlobal`）** | 先有单节点预算；全局配额要有租约/抢占语义，等真需要再开 |
| **`run.env` 覆盖 `PATH` / 按 task 动态探测 PATH** | 本轮 PATH = 节点启动基线，探测与执行同一份；脚本自己改 PATH 是作者逻辑（V4-D5 技术选择） |
| **覆盖用户文件时“保留旧权限”** | 保持 `files` 既定的 0600 / entrypoint 0700；覆盖同名文件会改正文和权限，作者需避开重名 |
| **按 `next` / `targets` 阻止删除能力/源（in_use）** | 主文档允许显式拆链；删除不级联，缺失由 `DownstreamResolved` / 源体检报告，不加新限制 |

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
          anode emit fixer --data '{"file":"a.go"}'
        fi
```

**注意**：审查通过时**不 emit**，链条在这里停住。没有「分支」，但「不 emit」本身就是「这次不通往下走」——这是唯一需要的条件控制。

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
  next: [fixer, notify]        # 扇出 = 脚本 emit 两次
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        anode emit fixer  --data '{"file":"a.go"}'
        anode emit notify --data '{"text":"审查完成"}'
```

两次 emit 各带各的字段，各自只对**那一个**下游的 `params` 负责。缺 `file` 只剪 fixer 那条，notify 照常。

### 11.3 链尾：终端能力自己发通知

没有内置推送能力，链尾就是一个 `next: []` 的普通脚本能力。注意它自己声明了要什么、在哪跑：

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: deploy
spec:
  params:
    type: object
    properties:
      sha: { type: string }
    required: [sha]
  next: [notify]
  run:
    entrypoint: run.sh
    env:
      SOURCE_ROOT:                          # 仓库根：节点说了算
        valueFrom:
          fieldRef: { fieldPath: vars.SOURCE_ROOT }
    workDir: $(vars.SOURCE_ROOT)             # 引用 env 里声明过的名字
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        bash deploy.sh "$ANOTIFY_PARAM_SHA"   # 无权限位：用 bash 调，不写 ./deploy.sh
        anode emit notify --data '{"text":"deploy ok"}'
      deploy.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        echo "deploying $1 in $PWD"
  requires:
    commands: [git]
    values: [vars.SOURCE_ROOT]
---
apiVersion: anotify/v1
kind: Capability
metadata:
  name: notify
spec:
  params:
    type: object
    properties:
      text: { type: string }
    required: [text]
  next: []                       # 终端：链到这里结束
  run:
    entrypoint: run.sh
    env:
      NOTIFY_URL:                           # 只引用名字：local → node → ns 默认
        valueFrom:
          fieldRef: { fieldPath: vars.NOTIFY_URL }
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        # 通知由这个能力自己发（调外部系统），中枢不参与
        curl -fsS -X POST "$NOTIFY_URL" \
          -H 'Content-Type: application/json' \
          -d "$(printf '{"text":"%s"}' "$ANOTIFY_PARAM_TEXT")"
  requires:
    values: [vars.NOTIFY_URL]
```

`notify` 必须真的存在——否则 `deploy` 的 `next` 解析不到，`status.DownstreamResolved` 会是 false。

### 11.4 两处约定

- **例子按新环境约定可理解**：任务环境只有 `PATH` / `HOME` 基线、契约变量、`run.env` 与节点 `envPassthrough`；
  例子里的 `anode` / `bash` / `curl` 都来自基线 PATH，`$SOURCE_ROOT` / `$NOTIFY_URL` 都来自 `run.env`，没有依赖未声明的宿主变量。
- **`metadata.namespace` 是文档信封字段**：不进产物、不进 digest、也不上节点。CLI 可以由当前上下文（`anotify config set namespace`）提供，YAML 里写它就覆盖上下文。
- **`next` 里的 `name@ver`** 只在需要钉死下游版本时写。日常写裸名字（跟随 latest）——那才是「升级下游不用改上游」的形态。

---

## 12. 决策记录

### 12.1 已定（不翻案，除非在这里改一行并写理由）

| 决策 | 理由 |
|---|---|
| 一个能力一个结果；没有端口 | 结果只有「跑完」和「跑挂」两种，没有第三种可命名 |
| 扇出 = 多次 emit，每次显式给目标 | 谁收到什么写在脚本里，可读可查，不需要在 YAML 里映射 |
| `emit` 目标用位置参数 | 目标只有一个，`--to` 是多余的 flag |
| 同名直取 + default，无 mapping/literals/表达式 | 抹平「参数对不上」就丢了最该被看见的信号 |
| digest 含 `params`/`requires`/`lifecycle`/`run`，不含 `next`/`displayName` | 只对「节点要跑的那份东西」做内容寻址；路由属于图 |
| 改 `next` 原地更新，不产生新版本、不重推节点 | 同上；改线不该让所有节点重跑一遍部署 |
| `status` 全部现算，不落库 | 派生值落库 = 第二真相，迟早不一致 |
| 不要 `lastTransitionTime` | 它要求存历史，与上一条冲突；要时间查 `deployments.updated_at` 或事件 |
| `conditions[].status` 是字符串 `"True"`/`"False"` | 以后加 `"Unknown"` 不用改类型；示例、前端、CLI 一个口径 |
| 节点变量：名字进 digest，值不进 | 同一份能力在任何机器上语义一致；值属于机器事实 |
| `env` 无隐式继承，任务环境干净 | 隐式继承不可校验、不可审计，密钥更是无从约束 |
| 环境基线只有 `PATH` / `HOME`，凭证只走 `envPassthrough` 名字白名单 | 不继承宿主 env；探测与执行 PATH 一致；值不回传中枢；仍是宿主权限而非 sandbox（V4-D5） |
| `idempotent` 默认 `false`，只对幂等任务重试短暂基础设施失败 | 普通脚本非零 / 配置错 / 超时 / 取消不自动重跑；最多 at-least-once（V4-D4） |
| 租约 60s / 续租 20s / TERM 5s 后 KILL 进程组 | 失联可检测，旧 attempt 作废；不虚假保证外部 exactly-once |
| `resultTtlSec` 默认 7 天，事件/去重默认 30 天，显式 `0` 永久 | 有限保留；不删用户 `workDir`；回执不跟事件一起删出悬空引用（V4-D6） |
| emit 可选显式 `id`，同逻辑任务同请求去重，不同请求 409 | 不按调用序号自动去重，不跨 task 合并，不继承不同版本实现 |
| 三层 `vars`：local > node > namespace | 机器级两层跨 ns 共用，ns 层按任务所属 ns 取；展示带 ns 上下文（V4-D2） |
| 同一逻辑 task 一行，`attempt` 只是计数器 | 不新建 task_id；`parentTaskId` 只表示业务 emit 父子；换节点重试重新解析机器级值（V4-D4） |
| `task.reject` 封闭为 `stale_config` / `slot_unavailable`，accept 前拒绝不计 attempt | 分别回 pending / waiting_for_config 或 waiting_for_slot，带退避，不消耗非幂等重试预算；grant 后失败不能倒退 |
| CLI exit 5 只表示请求结果未知，不是 infra 事实 | 脚本非零默认 `script_failed`；infra 重试只认运行器结构化事实 |
| emit 剪枝回执 `taskId:null` + `pruned`，CLI exit 0 | 事件已接收但没建 task；不伪装成功派发，回执也进幂等结果 |
| 产物显式写本 attempt 的 `ANOTIFY_ARTIFACT_DIR`，托管复制/上传后结果引用 | 默认 7 天清系统 export/staging/副本，显式 0 永久；活动 attempt 不清理，用户 workDir 原文件不动 |
| 任务行终态后保留 30 天 | `retention.taskTtlSec`；活动任务不清理，与事件/去重各自计时 |
| 节点缺变量在**派发期（accept 前）**拦，不派下去再失败 | 派下去再失败等于把配置问题变成运行时故障 |
| `workDir` 默认每次任务一个独立目录 | 并发安全优先；要共享目录的人显式声明并自己负责并发 |
| `run` 是唯一实现形式，中枢不执行能力 | 两个执行位置 = 两套可见性 + 两套测试面 |
| emit 凭据是任务令牌，不是通道 | 跨用户/容器/机器一条路径，一份测试面 |
| I8：emit 只归属运行态任务 | 否则「这份产物属于哪次运行」永远说不清 |
| `files` 没有权限位；entrypoint 0700，其余 0600 | 权限位是第二套契约；`bash x.sh` 已经够用 |
| 单建严格（`unknown_target` 400）/ 预案宽松 | 单建有确定上下文；预案必须允许先装上游后装下游 |
| 能力定义里的 `{{…}}` 是字面文本 | 包层 values 只在装包时组装，触发期只解 `$(payload.*)`——两件事；`{{…}}` 不解析不拒绝，`${…}` / `$(…)` 留给脚本自己 |
| 参数按 `ANOTIFY_PARAM_<NAME>` 注入，归一冲突即 400 | 让"谁覆盖谁"不可能发生，而不是靠约定 |
| 机器事实只从 `run.env` 进来，用 `valueFrom.fieldRef.fieldPath` | 一个入口、一套名字；路径是封闭集合，写入期就能校验 |
| 节点变量**不自动注入**，`run.env` 就是白名单 | 最小权限：只读任务不该拿到节点上的全部变量；键名也由能力自己定（`$SOURCE_ROOT`，不是 `$ANOTIFY_NODE_SOURCE_ROOT`） |
| `workDir` 用 `$(vars.X)` / `$(params.X)` 引用，不直写 `fieldPath` | 值的来源只留一处（`run.env` / `spec.params`），否则同一个值有两种写法、两条校验路径；参数化目录也因此能对着 schema 校验 |
| `status.*` / `metadata.*` 不写进 `requires.values` | 所有节点都有这些事实，写进去只会制造噪声 |
| `requires` 一个字段装两类依赖（`commands` / `values`） | 它们回答同一个问题（这台节点能不能跑我）、共用一个 condition；拆两个顶层字段就是多一份校验、多一处上限 |
| 去掉 `~`，家目录走 `status.homeDir` | 一个字符串魔法换成一个有名字的事实 |
| `$(…)` 只在**字段**里归我们（`workDir`），**脚本正文**里归 shell | k8s 里 `$(VAR)` 是 kubelet 启动前展开，同为“字段”语义；按“字段 vs 正文”划分后，同一个记号在两处各有唯一解释，不需要第三套语法 |
| 不做 `workDir` 路径锁，改成两个显式预算（`limits.total` × `maxPerNode`） | 锁是隐式耦合（跨能力对不上、只读并发也被堵）；预算是声明，能校验、能解释 |

### 12.2 待定

**无。** D6 已关闭（见 §12.1 末两行）：不做路径锁，改成「节点槽位 `limits.total` × 能力预算 `maxPerNode`」两个显式数；
共享目录的并发安全由能力负责，写入期用 warning `workdir_shared_concurrency` 提示。

节点侧的决策 D1–D5 见 `anode.md §7`，本文不复述。

### 12.3 与其它文档的边界

| 文档 | 定义什么 | 本文如何对待 |
|---|---|---|
| `config.md` | 配置侧：三层 `vars`（本机 / 节点 / ns）、逐键合并、同步协议、`configRevision` / `namespaceVarsRevision`、权限、错误码 | 只引用：本文只定义能力**怎么引用**值；值放哪、怎么解析由它定义 |
| `anode.md` | 节点侧：`anode.yaml`（身份 / `listen` / `vars` / `limits`）、上报、注入、emit 端点实现 | 只引用：`vars.*` 的名字与值在这里是**外部事实** |
| `source.md` | 源：`cron` / `webhook` / `manual`、源配置与 `$(payload.x)` 触发期求值 | 只引用：能力只消费「一次运行的 payload」，不问它从哪来 |
| `playbook.md` | 安装包：manifest 裁剪、回执、包级 status 聚合、包外引用 | 只引用：包内能力的 `spec.*` 形状完全由本文定义 |
| `README.md` | **取值语法**（字面量 / `valueFrom` / 拼接）与 `fieldRef.fieldPath` 封闭集合——四份文档共用的词汇 | 只引用：本文只加「哪些字段接哪种写法」的能力侧约束 |

本文不定义、也不要在此处回答：源怎么触发、节点怎么装、包怎么回执。
