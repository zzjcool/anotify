# v4 设计文档

| 文件 | 内容 | 状态 |
|---|---|---|
| **本文件** | 全局约定：`fieldPath` 取值语法 + 三层配置入口 | 已定（语义冻结；实现未开始；决策 C1–C19 见 `config.md`） |
| `decisions.md` | **本轮已批准决策 V4-D1～D6**：权威增补、默认值变更、刻意不做 | **已批准**（2026-11-09；与其它文档冲突时以它为准） |
| `config.md` | 配置：本机 / 中枢节点 / 中枢命名空间三层、逐键合并、同步协议、`configRevision` / `namespaceVarsRevision`、错误码 | 已定（语义冻结；实现未开始；C1–C19，rev3） |
| `capability.md` | 能力：`params` / `next` / `requires` / `run` + 派生 `status`、执行/重试/保留 | 已定（语义冻结；实现未开始）（2026-11-09 对齐：`requires.commands`/`values`、`valueFrom`、并发预算、V4-D4/D6） |
| `source.md` | 源（触发器）：三种内置 kind（kind 为开放集合）、config、投递体协议、targets | 已定（语义冻结；实现未开始）（D0–D12；事件去重窗口按 V4-D6） |
| `playbook.md` | 安装包：**一个目录**（`playbook.yaml` + 每个能力/源一个文件）+ OCI 分发 | 已定（语义冻结；实现未开始）（第四版，D1–D35） |
| `anode.md` | 节点：`anode.yaml`（身份 / `listen` / `vars` / `limits` / `envPassthrough`）+ 上报 | 已定（语义冻结；实现未开始；D1–D8 见 §7） |
| `api-conventions.md` | API 派生：信封、`/v1` 路由、错误体、读写语义（不新增产品功能） | 已定（语义冻结；实现未开始） |

> **状态保留**：能力/源/节点的 `status` 继续作为只读面存在，全部从事实（events / tasks / deployments / 节点上报）**读取时派生**；
> 不落库、不需要 reconciler、不加 `lastTransitionTime` / `observedGeneration`（见 `decisions.md` V4-D1）。

## 命名与二进制（v4 一并改）

| 现在 | v4 |
|---|---|
| `anotify-agent` | **`anode`**（和 AI 的 agent 撞名，改掉） |
| `cmd/agent/` | `cmd/anode/` |
| `node.yaml` | **`anode.yaml`**（+ 新增 `listen` / `vars` / `limits` / `envPassthrough`；中枢另有节点默认值与 ns 默认值） |
| systemd 单元 `anotify-agent.service` | `anode.service` |

配置文件名 `node.yaml` → **`anode.yaml`** = 身份三件套 + `listen` + `vars` / `limits` / `envPassthrough`（本机值；中枢另有节点默认值与 ns 默认值，**逐键 local > node > namespace**）。
完整形状见 `config.md §4.5`。

**只有一个二进制**，三个子命令：

```bash
anode serve        # 守护进程（systemd 里跑这个）
anode install      # 装成系统服务
anode emit <name> [--id <key>]  # 能力脚本里唯一的命令；可选显式幂等键
```

全仓约 460 处 / 71 个文件需要同步替换（含 Dockerfile、install skill、e2e 夹具、文档）。
漏一处的后果是"新环境下装不上"，所以它必须在 P9 的第一刀里做完、且由 e2e 验证。

v3 的文档仍在上级目录（`capability-spec-v3.md`），**已落地并推送**，作为现状参考。
v4 是 breaking change：`spec.outputs` 改名 `next`、边不再是用户实体、
`handler` 实现形式删除。见 `capability.md §7`。

## 三层配置源：本机 → 中枢节点 → 中枢命名空间

值只有**一种**：`vars.<NAME>`，三处可定义，逐键合并。解析顺序 **本机 `local` > 中枢按节点 `node` > 中枢按任务 namespace `namespace`**（`decisions.md` V4-D2）。
判据一句话：**换一台机器要不要重新配一遍？要 → 写本机；同一台机器所有 ns 都该一样 → 写节点默认；只属于某个 ns → 写命名空间默认。**

| 来源 | 放什么 | 作用域 | 优先级 | 能力侧引用 |
|---|---|---|---|---|
| **本机**（`anode.yaml`，一台一份） | 项目工作目录、工具链、`limits` | 机器级，跨 ns 共用 | 高（同键压住后两层） | `vars.SOURCE_ROOT` |
| **中枢节点**（单台 / 批量默认值） | 这台机器上所有 ns 共用的值、`limits` | 机器级，跨 ns 共用 | 中 | `vars.TOOLCHAIN` |
| **中枢命名空间**（ns 共用一份） | 共享端点、镜像源、统一默认值 | 按**任务所属 ns** 取 | 低（兜底） | `vars.NOTIFY_BASE` |

能力定义里只写名字（`vars.<NAME>`），**不写来源**：逐个键解析，前一层有就赢，没有才落到下一层。
`limits` 只有**本机 + 节点**两层，跨 ns 共享总槽位，**没有 ns 级配额**。
`anode.applied.yaml` 只存**机器级合并结果**；namespace 默认值只随任务在内存里传递，不写 applied。
有效值展示必须带 ns 上下文（不能一份扁平 `effectiveVars` 假装覆盖全部 ns），每个键标来源 `local` / `node` / `namespace`。

**凭证不建模**：`vars` 都是非敏感值（会上报、展示、进日志）；需要 token 的能力用机器上已有的登录态（`gh auth` / ssh key / CI 环境变量）。
节点可用 `anode.yaml.envPassthrough` 按**变量名白名单**透传 `SSH_AUTH_SOCK` / 指定 CI 凭证变量：值不进 `vars`、不回传中枢、不主动记 env 快照；`run.env` 同名时显式 `run.env` 优先（`decisions.md` V4-D5）。
`PATH` 取 `anode` 启动时的基线，命令探测与任务初始 PATH 同一份，`run.env` **不支持覆盖 PATH**（400 `env_invalid`）；`HOME` 默认 `status.homeDir`，可显式覆盖但必须是绝对目录。
仍是**宿主权限执行，不是 sandbox**，系统不阻止脚本把密钥写进 stdout。

形状、逐键合并、同步协议、权限、错误码、决策 C1–C19：见 **`config.md`**。
注册与同步全部由 **anode 主动发起**（首帧 `hello` 带机器级生效配置与 `appliedRevision`）；中枢从不主动查询节点，只回应。
namespace 默认值不写进 `anode.applied.yaml`，只随任务在内存里下发。

## 取值语法（全局约定）

值（机器事实 + 三层共享值）只有**一个入口**：能力的 `run.env`。
写法是 k8s 的 `valueFrom.fieldRef`，引用名是 `status.hostIP` 这种路径：

```yaml
run:
  env:
    SOURCE_ROOT:                          # 本机值 → 节点默认 → ns 默认，逐键解析
      valueFrom:
        fieldRef:
          fieldPath: vars.SOURCE_ROOT
    NOTIFY_BASE:                          # 通常是 ns 默认；前两层写了同键就赢
      valueFrom:
        fieldRef:
          fieldPath: vars.NOTIFY_BASE
    HOST_IP:
      valueFrom:
        fieldRef:
          fieldPath: status.hostIP        # 节点内置事实
    JOBS: "4"                             # 字面量
  workDir: $(vars.SOURCE_ROOT)           # 引用本能力 env 里声明过的名字
```

脚本里就是 `"$SOURCE_ROOT"` / `"$NOTIFY_BASE"`——**一个值一个名字，从 env 进来，各处都这么读**。

| 规则 | 说明 |
|---|---|
| 值只能在 `run.env` 里进来 | `workDir` 和脚本只消费 env 名；不自动注入、不隐式继承 |
| `fieldPath` 是**封闭集合** | 不认识 → 400 `unknown_field_path`；要加 path 先改本节 |
| `status.*` / `metadata.*` 是内置事实 | 所有节点都有，**不写进 `requires.values`**，不参与节点匹配 |
| `vars.*` 都要写进 `spec.requires.values` | 用了没声明 → 400 `undeclared_need`（原 `undeclared_node_var`）；值从哪来（local → node → ns）见 `config.md §2` |
| `run.env` 是白名单 | 节点上可能有一堆 `vars`，任务也只拿到它引用过的那几个 |
| 机器事实（`vars.*` / `status.*`）只能经 `run.env` 引用 | 不写进 `params` 默认值、不写进源 `config`——那些是文档，放进去就是第二个真相。`params` 是**任务输入**，另说：`workDir` 里可以用 `$(params.X)`（`capability.md §2`） |
| `$NAME` 只在**脚本正文**里 | 脚本是 shell：`$NAME` / `${…}` / `$(cmd)` 都归它；`workDir` 与源参数是**字段**，写 `$(vars.X)` / `$(params.X)` / `$(payload.X)`，由派发器解析 |
| `env` 值不做 env 之间拼接 | 不写 `PATH: /x:$A`；要拼在脚本里拼 |
| 字段与脚本正文分开 | `{{…}}` = 包层安装期（`playbook.md`）；`$(payload…)` = 源参数的触发期（`source.md §4.1`）；`$(vars.X)` / `$(params.X)` = 能力 `workDir`（`capability.md §2`）——都在派发前解析。`run.files` 的**脚本正文**里 `$NAME` / `$()` 归 shell，我们不碰。包层模板只替换 `{{values.*}}`，其它 `{{…}}` 是字面量（`playbook.md §1.4`） |

### `fieldPath` 封闭集合

“对象”只有一个：**节点**——`metadata.*` 是身份，`status.*` 是它现在是什么，
`vars.*` 是这台机器“能提供什么”（本机 → 节点默认 → 任务所属 ns 默认，见 `config.md §2`）。

| fieldPath | 值 | 来源 | 类型 |
|---|---|---|---|
| `metadata.name` | 节点名 | 节点 | string |
| `status.hostIP` | 节点主 IP | 节点内置 | string |
| `status.hostname` | 主机名 | 节点内置 | string |
| `status.os` | `linux` | 节点内置 | string |
| `status.arch` | `amd64` | 节点内置 | string |
| `status.capacity.cpu` | 逻辑核数（注入时是 `"8"`） | 节点内置 | string |
| `status.workRoot` | 每次任务独立目录的父目录 | 节点内置 | string（绝对路径） |
| `status.homeDir` | `anode` 用户家目录 | 节点内置 | string（绝对路径） |
| `vars.<NAME>` | 本机值 → 节点默认值 → 任务所属 ns 默认值，逐键解析 | 本机 / 中枢节点 / 中枢 ns | string |

注入时全部是字符串（env 语义），要数字自己转。

**目录类**（能当 `workDir` 的最终值）：`status.workRoot` / `status.homeDir` / 任一 `vars.<NAME>`。
`status.capacity.cpu` 这类标量不是 → `workDir: $(vars.X)` 指向它 400 `non_path_field`。
路径属于机器，建议优先放本机；中枢也能存目录，不做“来源级 400”；`workDir` 的最终值在执行前按绝对路径/存在/目录验证。
