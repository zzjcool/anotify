# Node Spec v4 · 节点（`anode`）

> 状态：**已定（语义冻结；实现未开始）**（2026-11-08；2026-11-09 按 `decisions.md` V4-D2/D5 对齐三层 `vars` 与 `envPassthrough`）。
> 本文只定义**节点侧**契约：它怎么配、它声明了什么、中枢知道多少。
> 配套：`capability.md`（能力）、`source.md`（源）、`playbook.md`（安装包）。
>
> **本设计从零开始**：不背历史实现、不留兼容层、不写迁移步骤、不保留旧字段的语义。
> 开发期只要"最干净的那一版"，取舍一律按"新装一台机器要什么"来判。

---

## 0. 定位

**节点 = 执行者**。一台真实机器跑一个 `anode` 进程；`name` **集群内唯一**，同名多实例不支持。

`anode.yaml` 只回答三个问题：

| 问题 | 字段 |
|---|---|
| 我是谁 | `name` / `server` / `token` |
| 我在哪听 | `listen` |
| **这台机器有什么** | `vars` / `limits`（本机写一份，中枢另有默认值） |

**节点可以服务多个命名空间**（一台机器被多个 ns 共用是常态，不是例外）

- **两处各有一份列表，生效 = 交集**：
  - `anode.yaml` 的 `namespaces`：机器所有者的**申报**（"可以给谁用"），缺省 `["default"]`；
  - 中枢 `Node.spec.namespaces`：管理员的**授权**，缺省 `["*"]`（= 不额外限制，以申报为准）。
  任一侧写 `"*"` = 全部。**这里不做"本地优先"**：它是权限，只能收紧、不能自我扩张。
- **派发**：能力属于某个 ns，只有 `ns ∈ 生效列表` 的节点才是候选；同 ns 没候选 → `NodesAvailable: False / NoMatchingNode`。
- **`token` 不绑 ns**：它是**节点身份令牌**，一个节点一份，与它服务多少个 ns 无关。引导顺序固定：管理员先 `POST /v1/nodes` 登记一个离线 `Node`（`metadata.name` 必填），再 `POST /v1/nodes/{name}/tokens` 签发令牌；Node 不存在时该端点 404，**签 token 不会隐式创建节点、也不会顺手替你选命名空间**。令牌**明文只返回这一次**（这不是一次性 bootstrap：它长期做节点身份），v4 不定义 `expiresAt` / 自动续签；再次签发 = 显式轮换，旧 token 立即失效（旧会话也随旧 token 一起失效）。`anode install` 只配已签发的令牌，**不做自动注册**。
- **`name` 集群内唯一**：节点是**集群级**对象，不再是某个 ns 下的资源。
- **`vars` 三层作用域不同**：本机 `vars` 与中枢**节点默认值**是**机器级**（所有 ns 共用）；中枢**命名空间默认值**是 **ns 级**（按任务所属 ns 取）。
  解析顺序 **local > node > namespace**：跟 ns 相关的值不要写本机或节点默认——一写就压住所有 ns（`config.md §2.1`）。
- **`limits` 只有两层**：本机 + 中枢节点默认值。`total` 是这台机器的总槽位，跨 ns 共享；`perCapability.<NAME>` 按能力名匹配，
  不同 ns 里的同名能力共用一个上限（它限制的是机器上的进程数）。**没有 ns 级配额**。

> 同步：`api-conventions.md` 按 `decisions.md` V4-D1 采用轻量 `/v1` 路由，`Node` 是集群级对象；
> 「谁能用哪台机器」由 `anode.yaml.namespaces` ∩ `Node.spec.namespaces` 回答，不需要 RBAC。

四条硬性质：

1. **三处可定义，逐层优先**。`vars` 本机、中枢节点默认值、中枢 ns 默认值各一份；**逐键合并，同键按 local > node > namespace**（`config.md §2`）。
   节点默认值管的是“这台机器跨 ns 的默认值”，ns 默认值只在该 ns 的任务里生效；节点页标出生效来源（`local` / `node`）。
   中枢只有「下发的那份 + 节点回报的 applied/revision」，快照过期只会让"状态显示旧了"，不会让任务跑错地方。细节见 `config.md §2` 与 §4。
2. **一个二进制、一个监听**。`anode` 只为能力进程开一个 emit 端点（见 `capability.md §4.0`），自身不对外提供任何服务。
3. **变量是"机器提供的东西"的名字**。能力在 `run.env` 里用 `fieldPath: vars.SOURCE_ROOT` 引用它，注入时用的键名由**能力自己**定；值不进能力定义、不进 digest、不进任何 API 响应，也不进没引用它的任务环境。
4. **所有同步由 `anode` 发起**。连上中枢的第一帧 `hello` 就带身份 + 本机配置 + `appliedRevision`；中枢**不主动查询**节点，`config.changed` 只是"有新版本"的提示（`config.md §4.2`）。

---

## 1. 完整形状

```yaml
# anode.yaml
name: node-a                       # 必填，集群内唯一
server: wss://anotify.example.com  # 必填，中枢地址
token: <node token>                # 必填，中枢签发的节点身份凭证（不绑 ns；明文只返回这一次）

namespaces: [default, team-a]      # 可选；这台机器允许被哪些 ns 使用，缺省 [default]；["*"] = 全部

listen: tcp://127.0.0.1:0          # 可选；emit 端点。默认内核挑端口

vars:                              # 可选；中枢也有一份，同键以本地为准
  SOURCE_ROOT: /home/ci/src
  TOOLCHAIN:   /opt/go1.25

limits:                            # 可选；并发预算（多维），同键本机压住节点默认
  total: 8                         # 这台机器同时最多跑几个任务，缺省 1
  perCapability:                   # 可选；按能力收紧（键 = 能力名；"*" = 未单列的能力）
    reviewer: 2

envPassthrough:                    # 可选；把本机已有的这些变量名透传给任务（值不离开本机）
  - SSH_AUTH_SOCK
  - GITHUB_TOKEN
```

---

## 2. 字段表

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `name` | string | ✅ | `^[a-z0-9][a-z0-9._-]{0,62}$` | **集群内**唯一；换名字 = 换一台节点机器 |
| `server` | string | ✅ | `ws://` / `wss://` | 中枢地址 |
| `token` | string | ✅ | 非空 | 节点身份凭证；**不绑 ns**；明文只返回一次，长期使用（无 `expiresAt`），再次签发即轮换且旧 token 失效 |
| `namespaces` | []string | 可选 | 元素 `^[a-z0-9][a-z0-9._-]{0,62}$` 或 `"*"`；去重；非空；缺省 `["default"]` | 这台机器**允许被哪些 ns 使用**（申报）；与中枢授权取**交集**生效，见 §0 |
| `listen` | string | 可选 | 见 §4，默认 `tcp://127.0.0.1:0` | emit 端点 |
| `vars` | map[名字]string | 可选 | 名字见 §3.1；值非空、≤4096 字节、不含 NUL | 节点变量；中枢也有一份，**同键本地优先**；能力侧 `fieldPath: vars.<NAME>` 引用 |
| `limits` | object | 可选 | `total` ≥1（默认 1）；`perCapability.<NAME>` ≥1，`NAME` 是能力名 `^[a-z0-9][a-z0-9._-]{0,62}$` 或 `"*"`；同键本机压住节点默认 | 并发预算，**多维**（见下） |
| `envPassthrough` | []string | 可选 | 变量名 `^[A-Za-z_][A-Za-z0-9_]*$`，≤32 项，去重；禁止 `PATH` / `HOME` / `ANOTIFY_*` | 凭证变量白名单：只写**名字**，值留在本机；完整名单不回传中枢、不进快照 |

- `limits` 是**节点级并发预算**，可多维。本期两个维度：
  - `total`：这台机器同时最多跑几个任务进程（一个任务占一个槽位，默认 1）。
  - `perCapability.<NAME>`：对这个能力额外收紧；没列的看 `"*"`，连 `"*"` 也没有 = 不限。
  有效并发 = `min(total 的空闲槽位, perCapability[NAME] ?? "*" ?? ∞, 能力 spec.concurrency.maxPerNode)`
  ——**节点只能收紧，不能突破能力自己声明的预算**。吃满时任务停在 `pending`（`waitingReason: waiting_for_slot`），
  不失败、不占槽位、不耗租约（`capability.md §2.3`）；派发器只在**有空闲槽位的节点**里挑（§7 D5）。
  要加新维度（按下游、按 `workDir` 组…）就加新 key，不改 `total` 的语义。
- **`vars` 三处可定义、逐层优先**：本机压住节点默认值，节点默认值压住 ns 默认值；本机没写的键仍用节点默认，再落到 ns。
- **`limits` 只有本机 + 节点两层**：没有 ns 级配额；ns 默认值不参与 limits。
  合并逐叶：`total` 覆盖，`perCapability` 递归到每个能力键；本机同名覆盖 node，未写继承。
  改本地要**重启**才生效（§5）；改节点默认只影响“本地没覆盖”的键，节点 apply 后**不用重启**（`config.md §4.2`）；
  ns 默认值只影响该 ns 的下一个任务，不写本机文件、不进 applied。
  节点页逐键标出生效来源（`local` / `node`），ns 页标 `namespace`，任务详情带 ns 上下文，避免“改了中枢却不生效”没人知道。
- 本地 `vars` 与中枢变量**可以同名**（这正是解析优先级的用途），本地不需要、也不允许再声明一份"覆盖"。
- `vars` 的值都是**非敏感、可上报、可展示**的（凭证不建模，见 `config.md §0`）；`envPassthrough` 只写变量名，值不离开本机。
- 未知字段一律**启动失败**（`unknown field: listen_` 这类拼错必须当场暴露，不能静默忽略）。

---

## 3. 变量（`vars`）

名字只有**一套**（`vars.<NAME>`），来源有三层：本机 → 中枢节点默认值 → 中枢 ns 默认值（`config.md §2`）。
本节只说**节点侧怎么提供**。

### 3.1 命名

```
键名（anode.yaml）   ^[A-Z][A-Z0-9_]{0,63}$      例：SOURCE_ROOT
能力侧引用            fieldPath: vars.SOURCE_ROOT
落到任务环境           由能力自己在其 run.env 里定      例：$SOURCE_ROOT
```

- **必须大写**：这个名字要原样当 `fieldPath` 的第二段（`vars.<NAME>`），与 `spec.requires.values` 的元素形状共用一套正则，不留第二种写法。
- **不加前缀、不做小写转换**：节点侧名字**永远不会原样变成环境变量名**（键名由能力选），所以不需要前缀去和"这台机器 shell 里碰巧有的"区分；写错大小写 = 启动报错，而不是运行时才炸。

### 3.2 注入（最小权限）

能力侧（`capability.md §2.3`）声明它要什么，键名自己选：

```yaml
run:
  env:
    SRC:      { valueFrom: { fieldRef: { fieldPath: vars.SOURCE_ROOT } } }
    TOOLCHAIN: { valueFrom: { fieldRef: { fieldPath: vars.TOOLCHAIN } } }
  workDir: $(vars.SRC)
```

这个任务的进程环境里**只多出两行**：

```
SRC=/home/ci/src
TOOLCHAIN=/opt/go1.25
```

| 规则 | 说明 |
|---|---|
| **白名单** | 只注入本能力 `run.env` 显式引用过的那几个。节点上另外 18 个 `vars` 一个都不出现 |
| **键名由能力定** | `SRC` / `TOOLCHAIN` 是能力选的；节点的 `vars.SOURCE_ROOT` **从不以这个名字**进入环境 |
| **环境是干净的** | 不继承节点/守护进程的完整环境。基线只有 `PATH` / `HOME`（`HOME` 取 `status.homeDir`），加上契约变量（`ANOTIFY_EMIT_ENDPOINT` / `ANOTIFY_TASK_TOKEN` / `ANOTIFY_TASK_ID` / `ANOTIFY_PARAM_*`）、`run.env` 与 `envPassthrough` 白名单。其它一律不继承 |
| **PATH 一致** | PATH 取 `anode` 守护进程启动时的基线；命令探测（`requires.commands`）与任务初始 PATH 是**同一份**。**暂不支持 `run.env` 覆盖 `PATH`**（写 `PATH` → 400 `env_invalid`）；脚本自行改 PATH 属于作者逻辑 |
| **HOME 可显式覆盖** | 默认 `status.homeDir`；`run.env` 显式写 `HOME` 时，字面量不是绝对路径 → 写入期 400 `env_invalid`；执行前不存在 / 不是目录 → `home_unavailable`（`failed`，不重试） |
| **凭证只走名字白名单** | `envPassthrough` 里的变量名，值取自本机进程环境；值不回传中枢、不进 `vars`、不主动记录 env 快照。`PATH` / `HOME` / `ANOTIFY_` 开头的名字禁止透传；完整名单只在本机 |
| **同名优先级** | `run.env` 与 `envPassthrough` 同名时，**显式 `run.env` 优先**；未列入白名单的非 `ANOTIFY_` 变量一律不继承 |
| **缺即不存在** | 节点与中枢都解析不出这个值 → 该键**不出现**（不是空串）。`workDir` 依赖它就报 `workdir_unavailable`；脚本里 `set -u` 当场炸 |

> 派发期（`task.accept` 前）已经保证 `requires.values` 可解析，所以"缺"只可能来自"中枢值被删、本机 `vars` 改了没重启、或者配置还没 apply"。
>
> **这是宿主权限执行，不是 sandbox**：透传的是本机已有的登录态/凭证。系统**不承诺**阻止脚本把密钥写进 stdout / 日志；
> 能力作者必须自己避免输出凭证。`envPassthrough` 只能缩小暴露面，不能把不安全的脚本变安全。
> systemd / container 必须给 `anode` 进程显式注入这些宿主变量；没注入就是缺失，节点不会去猜。

### 3.3 上报（中枢只存快照）

**上报**与**身份校验**是同一个动作：每次连上中枢，第一帧 `hello` 带
`{name, token, namespaces, localVars, localLimits, appliedRevision, appliedHash}`——中枢不需要问，节点也不需要等指令。
`hello` **不是注册**：该节点必须已由管理员 `POST /v1/nodes` 登记、并由 `POST /v1/nodes/{name}/tokens` 签发过令牌；
中枢侧没有这台 `Node` 或令牌已轮换作废时，连接直接被拒（不是“自动建一台”）。
执行帧（派发/接受拒绝、租约、结果、取消）见 `capability.md §8.5`，不在配置帧里重复。
帧里带的是 **ns 列表**（申报值），不是单个 `namespace`。
帧里**不带 `namespace`**：节点令牌是集群身份，**不绑 ns**（§0）；一个节点可以服务多个 ns。

| 时机 | 上报内容 |
|---|---|
| 握手成功（每次 `anode` 启动）/ 每次 apply 后 | `vars` / `limits`：**机器级生效值**（本机与节点默认值合并后，含来源 `local` / `node`）；附 revision 与时间戳。ns 默认值不在快照里 |
| 重连 | 全量重报，覆盖快照 |
| 运行期 | 本地改动不重报（要重启，§5）；中枢侧改动走 apply 协议（`config.md §4.2`） |

中枢侧只用它做两件事：`NodesAvailable` 判定、节点页展示。

快照只包含**机器级**值（本机 + 节点默认值）与来源 `local` / `node`；
**ns 默认值不进快照**（它按任务所属 ns 解析、随任务内存下发，`config.md §2.4`）。
任务 env 诊断只记**名字 + 来源**（`local` / `node` / `namespace` / `passthrough` / `builtin`），不记实际值（C11）；`envPassthrough` 只报告本次用到的名字，不报告完整名单。

`NodesAvailable` 的两个输入各有一份来源：本快照回答 `requires.values`；`requires.commands` 由对节点的命令探测（`LookPath`）回答，**不进本快照**。

### 3.4 三种读法，一个名字

| 消费者 | 写法 | 求值时机 |
|---|---|---|
| `spec.run.env` | `SRC: { valueFrom: { fieldRef: { fieldPath: vars.SOURCE_ROOT } } }` | 组环境时（**唯一**的取值边界） |
| `spec.run.workDir` | `workDir: $(vars.SRC)` — 只认 env 名，不直写 `fieldPath` | 执行前（chdir） |
| 脚本正文 | `cd "$SRC"` | 脚本自己读 |

三者是**同一个名字**：能力在 `run.env` 里选的那个键名（这里 `SRC`）。
节点的 `vars.SOURCE_ROOT` **只在第一行出现这一次**——再往下字段里写 `$(vars.SRC)`、脚本正文里写 `$SRC`，没有第二套记号。

### 3.5 缺失时的三层校验

| 层 | 检查什么 | 结果 |
|---|---|---|
| **写入期**（中枢，建/改能力） | `run.env` 引用的名字是否 ∈ `spec.requires.values`；`workDir` 的 `$(vars.NAME)` 是否声明在 `run.env` 里 | 400 `undeclared_need` / `undefined_env_ref`（另有 `unknown_field_path`、`non_path_field`） |
| **派发期**（`task.accept` 前，节点匹配） | 逐个 `requires.values` 解析：本机 `vars` → 节点默认值 → 任务所属 ns 默认值；机器级 `configRevision` 与本机 applied 一致 | 该节点不匹配 → `NodesAvailable: False / ValueMissing`（message 列出名字）；配置没 apply → `ConfigPending`；revision 不一致 → `stale_config` / `task.reject` |
| **运行期**（`lease.grant` 后启动前） | 用本地 applied 值 + 本次下发的值重新求值；校验 `workDir`（绝对、无 `..`、已存在、是目录、解软链）、`HOME`（绝对、存在、是目录）、`requires.commands` | `workdir_unavailable` / `home_unavailable` / `missing_requirement` → 任务 `failed`；attempt 已计，确定性配置错误不自动重试 |

**"已存在"是硬要求，节点不创建它**。目录不存在是**配置问题**；
自动 `mkdir` 会把"配错了"变成"能跑，但跑到错地方"——那是最难查的一类事故。

### 3.6 节点内置事实（`status.*`）

除 `vars` 之外，每个节点都固定提供下面这几个值，能力用同一个 `fieldPath` 引用：

| fieldPath | 值 |
|---|---|
| `metadata.name` | 节点名（= `anode.yaml` 的 `name`） |
| `status.hostIP` / `status.hostname` | 主 IP / 主机名 |
| `status.os` / `status.arch` | `linux` / `amd64` |
| `status.capacity.cpu` | 逻辑核数（注入时是字符串） |
| `status.workRoot` / `status.homeDir` | 任务目录的父目录 / `anode` 用户家目录（**目录类**） |

- **不用写进 `requires.values`**，也不参与节点匹配：所有节点都有。
- **不进上报快照**：值属于机器，任务执行时实时求。
- `workRoot` / `homeDir` 是**目录类**，能直接当 `workDir` 的最终值；其余是标量，
  `workDir: $(vars.X)` 而 `X` 指向标量 → 400 `non_path_field`。

---

## 4. `listen`

```yaml
listen: tcp://127.0.0.1:0      # 默认：内核挑端口，仅本机可连
# listen: unix:///run/anode.sock
# listen: tcp://127.0.0.1:7070 # 固定端口，便于抓包/防火墙
```

| 形态 | 用途 | 说明 |
|---|---|---|
| `tcp://127.0.0.1:0` | 默认 | 实际端口写进 `ANOTIFY_EMIT_ENDPOINT`，注入每个任务 |
| `tcp://127.0.0.1:<port>` | 固定端口 | 与上者等价，端口可预期 |
| `unix://<path>` | 收紧 | 只有同用户能连；代价是各语言要额外胶水 |
| `tcp://0.0.0.0:<port>` / 非 loopback IP | 由用户决定 | 能起；但 emit 令牌是明文的，暴露面等于没有鉴权——写它就是自己认账 |
| 其它 scheme（`http://` 之类） | — | 启动失败 `listen_invalid` |

**默认不开口子**：emit 端点是同机概念，能力进程就在这台机器上，默认 loopback + 随机端口。
容器 / 命名空间要跨边界，优先挂 unix socket；确实需要监听外部地址时照写，风险由配它的人负责。

---

## 5. 启动期校验

启动是**全有或全无**：任何一项不过就退出，不存在"半在线"。

| 情况 | 结果 |
|---|---|
| 文件不存在 / 不是合法 YAML / 有未知字段 | 启动失败，指到路径与行 |
| `name` / `server` / `token` 缺失或格式不合 | 启动失败，逐个列出 |
| `listen` 不是 `tcp://` / `unix://` 形态（§4 表末行） | 启动失败 `listen_invalid`，接受值写进 message |
| `limits.total` < 1 或 `perCapability` 的值 < 1 | 启动失败，指出实际值 |
| `perCapability` 的键不是能力名（`^[a-z0-9][a-z0-9._-]{0,62}$`）也不是 `"*"` | 启动失败，指出是哪一项 |
| `vars` 名/值不合规（名字不符正则、空串、含 NUL、超 4096 字节） | 启动失败，指出是哪一项 |
| `namespaces` 为空 / 元素不是 ns 名也不是 `"*"` / 有重复 | 启动失败，指出是哪一项 |
| `envPassthrough` 不是变量名 / 超过 32 项 / 有重复 / 含 `PATH`、`HOME` 或以 `ANOTIFY_` 开头 | 启动失败，指出是哪一项 |
| 中枢拒绝身份（token 无效 / 已轮换作废 / 中枢没有这台 `Node`） | 启动失败（不是“半在线”重试态） |

启动后 `anode.yaml` **不热更**：改 `namespaces` / `vars` / `limits` / `envPassthrough` / `listen` → 重启。
`vars` / `limits` **逐层本机优先**：本地改了重启生效，不动中枢 revision；节点默认值走 apply，只影响本地没覆盖的键（`config.md §4.2`）；
ns 默认值不写本机、不进 applied。

---

## 6. 与 `status` 的关系

`NodesAvailable` 的数据来自**上报快照 + 中枢配置**，且必须同时满足：节点在线、机器级配置已 applied、`ns ∈ 申报 ∩ 授权`、`requires.commands` 全部命中、`requires.values` 逐个解析（本机 → 节点默认 → **任务所属 ns 默认**，见 `config.md §2.2`）：

```yaml
conditions:
  - type: NodesAvailable
    status: "False"
    reason: ValueMissing        # 或 CommandMissing / NodeOffline / ConfigPending / NamespaceUnauthorized
    message: "node-b 与中枢都没有 vars.SOURCE_ROOT"
```

设计边界：

- **能力 `status` 只说"缺不缺"，不说值**。值写进能力文档就变成第二个真相，还会泄漏路径/主机信息。
- **想看值去看节点页**：`node-a` 的 `SOURCE_ROOT = /home/ci/src`（附上报时间）。
- **离线节点不参与匹配**：`online: false` 的节点既不满足 `requires.values`，也不提供槽位。
  一个节点掉线只影响"确实需要它"的能力（reason `NodeOffline`），不会连坐别家。
- **有效值带 ns 上下文**：节点页展示机器级（`local` / `node`）；某个 ns 的实际有效值要在 `(节点, ns)` 上下文里看，不能拿节点页的一份值宣称覆盖全部 ns。
- `reason` 取最具体的那一个：同时存在离线与解析不出值时，先报 `NodeOffline`。

---

## 7. 设计决策（已定，不留待定）

| 编号 | 议题 | 决策 | 理由 |
|---|---|---|---|
| **D1** | 变量要不要类型/检查（`check: dir` / `file`） | **不做**。`spec.requires.values` 只声明名字，不声明类型 | 类型检查需要把节点的文件系统状态塞进快照，快照会随机器变化而失真；宁可第一次执行就报 `workdir_unavailable` 且 message 说清是哪种缺 |
| **D2** | 节点离线时 `NodesAvailable` 怎么算 | **离线即不匹配**（`False / NodeOffline`） | 匹配的语义是"现在能不能跑"，不是"历史上能不能跑"；掉线只影响真正需要它的能力 |
| **D3** | `tcp://0.0.0.0` 允许吗 | **允许，但不做默认**：默认 `127.0.0.1:0`，写外部地址由配的人自己负责暴露面 | 节点是用户自己的机器，禁一个能配的东西只会逼人去改源码；默认收紧 + 文档说清风险，比白名单更实际 |
| **D4** | 值改了要不要重推部署 | **不重推** | 值不进 digest（`config.md C4`）；本地 `vars` 改了重启即生效，既不动中枢 revision，也不动能力定义 |
| **D5** | 变量能否在派发时参与节点选择 | **只做匹配，不做择优**。匹配集合内按"有空闲槽位 → `name` 升序"确定性选取 | 按变量打分需要中枢理解变量的语义；先用确定性规则，等真的需要扩容再谈 |
| **D6** | 一台机器被多个 ns 共用怎么办 | **节点是集群级对象**，生效 ns = `anode.yaml.namespaces`（申报）∩ `Node.spec.namespaces`（授权） | 共用机器是常态；两处列表 + 交集能在不加 RBAC 的前提下回答"谁能用哪台机器" |
| **D7** | `namespaces` 要不要本地优先（同 `vars` 那样） | **不**。仅交集，本地只能收紧 | 权限不能靠"本地赢"——否则任何一台机器都能把自己塞进任意 ns |
| **D8** | 凭证变量怎么给任务 | **不继承宿主 env**；`envPassthrough` 只写变量名、禁止 PATH/HOME/ANOTIFY_，值留在本机；`run.env` 同名优先；系统不承诺阻止脚本输出密钥 | 不把凭证塞进 `vars`（会外泄）；白名单可审计、可缩小暴露面，但仍是宿主权限而非 sandbox（`decisions.md` V4-D5） |

---

## 8. 实施顺序

| 步 | 内容 | 门禁 |
|---|---|---|
| **M1** | `anode.yaml` 解析（身份 / `namespaces` / `listen` / `vars` / `limits` / `envPassthrough`）+ `listen` + 注入 `ANOTIFY_EMIT_ENDPOINT` + `PATH`/`HOME` 基线 | `ws_protocol`、`edge_cases` |
| **M2** | `vars`/`limits` 上报快照 + `spec.requires.values` 解析（local → node → ns）+ `workDir`/`env` 展开 + 三层校验 + `envPassthrough` | `deploy`、`live_agent`、`edge_cases` |
| **M3** | 节点配置 apply：`config.get` / `config.applied`、`anode.applied.yaml`、`configRevision` 校验 | `ws_protocol`、`deploy` |

M2 依赖 `capability.md §2.3` 的 `spec.requires` 与 §4.0 的 emit 端点定义；M3 依赖 `config.md §4.2` 的 apply 协议。
