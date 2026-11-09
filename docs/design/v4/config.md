# Config Spec v4 · 配置（本机 / 中枢节点 / 中枢命名空间）

> 状态：**已定（语义冻结；实现未开始）**（2026-11-09，rev3：按 `decisions.md` V4-D2 改为**三层 `vars`**；`limits` 保持两层）。
> 本文只回答四个问题：**值放哪、怎么解析、怎么生效、怎么被引用**。
> 配套：`capability.md`（`run.env` 怎么引用这些值）、`anode.md`（节点进程 / 注入 / 校验）、`README.md`（`fieldPath` 封闭集合）。
>
> **一条总纲**：配置值只有**一种**——`vars.<NAME>`。同一个名字可以在**三处**定义，
> 逐键合并、优先级 **本机 `local` > 中枢按节点 `node` > 中枢按任务 namespace `namespace`**。
> 没有 `secrets`，也不下发任何凭证；凭证只走节点本机的 `envPassthrough` 名字白名单（`anode.md §3.2`）。

---

## 0. 定位：三层定义，逐键解析

| | 本机（`anode.yaml`） | 中枢节点默认值 | 中枢命名空间默认值 |
|---|---|---|---|
| 作用域 | 一台机器 | 一台机器（跨 ns 共用） | **一个命名空间**（按任务所属 ns 取） |
| 角色 | 这台机器的**实际值**（最终真相） | 集中管理单台 / 批量的默认值 | 同 ns 共用的默认值 |
| 典型值 | 项目工作目录、单机工具链 | 这台机器上所有 ns 都该一样的值 | 共享端点、镜像源、统一默认值 |
| 编辑 | 改文件 → 重启 `anode` | 中枢 UI / API（单台或批量） | 中枢 UI / API |
| 生效 | 重启后（同键压住后两层） | 节点 apply 后（本地没写的键） | 下一个任务（前两层没写的键） |
| 优先级 | 高 | 中 | 低（兜底） |
| 参与节点匹配 | ✅ 缺满三层才不匹配 | ✅ | ✅ |

判定规则一句话：**换一台机器要不要重新配一遍？要 → 写本机；同一台机器所有 ns 都该一样 → 写节点默认；只属于某个 ns → 写命名空间默认。**

三条理由：

1. **装一台新机器只写差异**：路径、并发这些每台不同的写 `anode.yaml`；单机但跨 ns 的默认值写中枢节点配置；共享值写 ns 配置。
2. **改默认值不该 ssh 到 N 台机器**：中枢节点默认值改一次，所有"本地没覆盖这个键"的节点下一个任务生效；
   ns 默认值改一次，该 ns 的所有节点下一个任务生效。
3. **本机是最终真相**：执行时用机器级生效值（`anode.applied.yaml`）再落本次任务的 ns 默认；
   中枢短暂不可用不影响已在跑的任务。

> **凭证不建模（v4）**：`vars` 里的值都是**可上报、可展示**的非敏感值。
> 需要 token / key 的能力（如 `gh`、私有仓库）用机器上已有的登录态（`gh auth login`、ssh key、CI 注入的环境变量），
> 不由中枢下发、也不进 `vars`。节点可用 `envPassthrough` 按变量名白名单透传**本机已有**的凭证变量，
> 值不回传中枢、不记录；要"中枢统一管凭证"是另一层设计，v4 明确不做（C8）。

---

## 1. 值放哪：归属判定

| 值 | 放哪 | 为什么 |
|---|---|---|
| 项目工作目录 `/home/ci/src` | 本机 `vars` | 每台机器不同，中枢推不出来 |
| 工具链 `/opt/go1.25`（这台机器专属） | 本机 `vars` | 同上 |
| 工具链（同一台机器所有 ns 共用） | 中枢节点默认 `vars` | 集中管理单台 / 批量，不必改机器文件 |
| 共享端点（推送网关、镜像源） | 中枢命名空间默认 `vars` | 同 ns 一份，改一次该 ns 全生效 |
| 并发预算 `limits` | 本机 + 中枢节点默认（两层） | 跟机器核数走；**没有 ns 级配额** |
| `listen`（emit 端点） / `envPassthrough` | 本机 `anode.yaml` | 只在这台机器上有意义；凭证值不离开机器 |
| token / API key / 部署密钥 | **哪都不放**（除本机透传名单） | v4 不建模凭证（C8）；`envPassthrough` 只写变量名，不写值 |

两条反例，写出来防止再讨论：

- **不建议把机器路径放中枢**：`vars` 本身没有“来源类型”，中枢也可以存目录；但路径属于机器，放中枢会让所有节点共用同一个目录假设。`workDir` 仍只认目录类字段，且执行前会验证绝对路径/存在/目录；不建议不等于 400（`non_path_field` 只针对标量 fieldPath）。
- **不要把凭证塞进 `vars`**：`vars` 的值会上报、会在节点页展示、会进任务日志；放进去等于公开。

---

## 2. 解析：local → node → namespace

### 2.1 顺序与作用域

能力 `run.env` 写 `fieldPath: vars.SOURCE_ROOT`。派发时对**每个名字独立**解析：

| 第一来源（优先） | 第二来源 | 第三来源（兜底） |
|---|---|---|
| 本机 `anode.yaml` 的 `vars.<NAME>` | 中枢**节点默认值**的 `vars.<NAME>` | 中枢**任务所属命名空间**的 `vars.<NAME>` |

```yaml
# 本机 anode.yaml
vars:
  SOURCE_ROOT: /home/ci/src     # 本机赢：后两层同名值被压住
# 中枢节点默认值里也可以有一份 SOURCE_ROOT，只在这台机器没写时才生效
# 中枢 ns 默认值里的同名键，只在前两层都没有时才生效
```

| 规则 | 说明 |
|---|---|
| **逐个键解析** | 不是整份覆盖：`SOURCE_ROOT` 用本机、`NOTIFY_BASE` 落 ns，互不影响 |
| **机器级两层跨 ns 共用** | 本机与节点默认值对这台机器上的所有 ns 是同一份；**不能**按 ns 取不同值 |
| **ns 层按任务所属 ns 取** | 同一个名字在 `team-a` 与 `team-b` 可以有不同默认值；A ns 的值绝不带进 B ns 的任务 |
| **缺失 = 不注入** | 三层都没有 → 该 env 键不出现（空串不等于缺失）；`workDir` 依赖它则 `workdir_unavailable` |
| **只看声明过的** | 解析范围由 `run.env` 决定（白名单），不是把三层的值全灌进任务环境 |
| **中枢的值不落盘在节点** | 节点默认值合并进 `anode.applied.yaml`；ns 默认值随任务下发、只在进程环境里活一次（§3.4） |
| **来源可查** | 节点页逐键标 `local` / `node`；任务详情记每个 env 的来源 `local` / `node` / `namespace`（C11），不记值 |
| **运行中不漂移** | 任务开始时解析出的值就是这次任务的值；后续配置更新不改它 |

### 2.2 匹配怎么用这三层

`NodesAvailable` 的数据 = 机器级生效快照（local + node 合并）+ **任务所属 ns 的默认值**，按 §2.1 逐键解析：

- `requires.values` 的每个元素都能解析出值 → 这台节点匹配。
- 三层都解析不出来 → 这台节点不匹配，reason `ValueMissing`，message 列出缺的名字与**任务 ns**。
- **本机 / 节点配置还没 apply 完的节点不参与匹配**（`ConfigPending`）：宁可少派，不按「还没生效的配置」派。
- ns 默认值不参与节点 apply 状态：它由中枢在派发时按 ns 解析，节点只校验机器级 revision。

### 2.3 优先级要看得见

优先级是内置规则，但**不能变成隐形差异**：

| 位置 | 显示什么 |
|---|---|
| 节点页 | 机器级逐键标 `local` / `node`：「`SOURCE_ROOT` 来自本机（节点默认也有一份，被压住）」 |
| 命名空间配置页 | 该 ns 的默认值 + 「`SOURCE_ROOT` 被 2 台节点的本机/节点值压住」 |
| 任务详情 | 每个 env 的解析来源（`local` / `node` / `namespace`，名字级，不记值） |
| 轮换提示 | 上层默认值更新后，压住它的节点 / ns 标 `override_stale`（「上层已更新，你本地还是旧值」） |
| 有效值预览 | **必须带 ns 上下文**：`(节点, ns)` 一组结果；不允许一份扁平 `effectiveVars` 假装覆盖全部 ns |

> 为什么不做「显式声明才覆盖」：那会让每台机器多一份 `overrides` 列表，本质是同一件事的第二份真相。
> 一条全局规则（逐层优先）+ 处处可见，比"每处都声明一次"更难漏。

### 2.4 机器级 `applied` 与两个配置版本：`configRevision` / `namespaceVarsRevision`

| 概念 | 存什么 | 谁维护 | 用途 |
|---|---|---|---|
| `machineApplied` | 本机 + 节点默认值的合并结果 | 节点，落 `anode.applied.yaml` | 任务执行的基础值；节点页展示 |
| `configRevision` | 节点配置（节点默认值）的 revision | 中枢生成，节点回报 `appliedRevision` / `appliedHash` | 判断机器级配置是否已 apply（`stale_config` / `ConfigPending`） |
| `namespaceVars` | 本次任务所属 ns 的默认值（只含被引用的名字） | 中枢在派发时解析 | 随任务下发，**不写 applied** |
| `namespaceVarsRevision` | 该 ns 配置的 revision | 中枢生成 | 任务诊断 / 回执里说明“这组 ns 默认是哪一版” |

- **两个版本不能压成一个**：`configRevision` 决定节点是否拒接（`stale_config`）；
  `namespaceVarsRevision` 只描述本次任务拿到的那组 ns 值。任务帧同时带两个字段。
- **ns 值不在节点落盘**：节点拿到的只是一次任务要用的那几个键，任务结束即丢；中枢必须保留可复现的原始 ns 输入（配置 revision 或内部执行输入快照）。
- **重试的机器级值按执行节点重新解析**：同一逻辑 task 换到另一台节点执行时，`local` / `node` 值按新节点重新解析，`configRevision` 跟着该 attempt 的节点；不能把 A 机的目录带到 B 机。
- **ns 默认值随任务钉住**：同一逻辑任务跨 attempt 沿用首次派发时的 ns 输入，不因中途改 ns 配置而换值；每个 attempt 解析完成后，运行期间不再受配置更新影响。

---

## 3. 中枢命名空间默认值

### 3.1 形状与命名

```yaml
# 命名空间配置（设置页 / API 里就是这一段；该 ns 内生效）
vars:
  NOTIFY_BASE: https://push.example.com     # 能力里写 fieldPath: vars.NOTIFY_BASE
```

| 项 | 规则 |
|---|---|
| 键名 | `^[A-Z][A-Z0-9_]{0,63}$`（与本机变量同一套正则，不留第二种写法） |
| 值 | 非空、≤4096 字节、不含 NUL；一律字符串 |
| 数量 | 每个命名空间 ≤64 项 |
| 重名 | 与本机 / 节点默认值可以有同名键（这正是优先级要解决的），**不需要也不允许**在定义里区分 |
| 未知字段 | 写入 400 `value_invalid` |

### 3.2 存储与轮换

| 项 | 决定 |
|---|---|
| 真相 | 中枢 DB（`config_values` 行，按 namespace 分区），**不是文件** |
| 加密 | 不做（C7）：值里不放密钥（C8），没有加密的理由 |
| 轮换 | 写新值即生效，**只影响该 ns 的下一个任务**；在跑的任务用开始时那份 |
| 被压住的节点 | 不受影响 → 标 `override_stale`（§2.3） |
| 版本化 | 不存历史值；每次写入 bump `namespaceVarsRevision`（只增计数，不做历史） |
| digest | 不进（C4）：改值不产生新能力版本、不触发重部署 |

### 3.3 权限

| 动作 | 要求 |
|---|---|
| 看名字 / 值 / 更新时间 / 被哪些能力引用 | 命名空间成员（值非敏感，可以看） |
| 增删改 | 命名空间管理员 |

### 3.4 下发（值怎么到节点）

| 规则 | 说明 |
|---|---|
| **只随任务下发** | 派发时只带这次任务解析出来的那几项；不整包推送、不落节点盘 |
| **只在该 ns 的任务里出现** | 节点服务多个 ns 时，A ns 的默认值不会出现在 B ns 的任务环境 |
| **只在内存里** | 不写 `anode.yaml`、不写 `anode.applied.yaml`、不写临时文件 |
| **可审计** | 任务详情记**用到哪些名字、从哪一层来、ns revision 是多少**（C11），不记值 |

### 3.5 命名空间默认值的读写入口

不是一个新的完整 kind，而是命名空间的配置子资源：

```
GET /v1/namespaces/{name}/config
→ 200 { metadata: { resourceVersion }, vars: { ... }, namespaceVarsRevision: "12" }

PUT /v1/namespaces/{name}/config
  body { metadata: { resourceVersion }, vars: { ... } }
  只接 metadata.resourceVersion + vars（namespaceVarsRevision 只读，UI 不提交）
  缺 resourceVersion → 400 resource_version_required
  旧 resourceVersion → 409 resource_version_conflict
  成功后 bump namespaceVarsRevision（配置生效版本，不是写并发令牌），并返回与 GET 相同的 wrapper
```

- `vars` 是**命名空间默认值**（§3.1 的规则）；这里没有 `limits`（limits 只在节点配置里）。
- 读权限 = ns 成员（值非敏感）；写权限 = ns 管理员。
- 写入只影响该 ns 的下一个任务；已运行任务/已钉住输入不变。

### 3.6 中枢系统配置（保留期）

保留期是**中枢系统配置**，不是 `vars`，也不是节点限额；单独存放、单独授权：

| 键 | 默认 | 说明 |
|---|---|---|
| `retention.eventTtlSec` | `2592000`（30 天） | 事件 / trace 元数据；`0` = 永久 |
| `retention.taskTtlSec` | `2592000`（30 天） | 终态任务行；**活动任务不清理** |
| `retention.resultTtlSec` | `604800`（7 天） | 日志 / 产物 / 系统结果目录的默认值；能力自己的 `resultTtlSec` 优先，`0` = 永久 |

```
GET /v1/system/config        → { metadata: { resourceVersion }, retention: { ... } }
PUT /v1/system/config        → 只接 metadata.resourceVersion + retention，返回同 GET wrapper；缺 / 旧 resourceVersion 同上（CAS）
```

去重窗口与实体保留分开：去重记录跟 `retention.eventTtlSec` 同窗口；被任务/回执引用的实体必须延长保留（`capability.md §8.4` / `playbook.md §7.3`）。

---

## 4. 中枢节点默认值

### 4.1 形状

本机写 `anode.yaml`（§4.5）；中枢也可以给每台节点存一份**机器级默认值**（外加 ns 授权）：

```yaml
# 节点配置（中枢侧；单台或批量编辑）
namespaces: [default, team-a]  # 授权：与本机申报取交集（不是默认值）
vars:                          # 本机没写的键才用这里的；跨 ns 共用
  SOURCE_ROOT: /home/ci/src
limits:                        # 并发预算，见 anode.md §2；跨 ns 共享
  total: 8
```

- `namespaces` 是**授权**而非默认值：生效 = 本机 `anode.yaml.namespaces` ∩ 这里（`anode.md §0`）。
  它**不适用「逐层优先」——只能收紧**，也没有“落到下一层”这回事（两边都得包含）。
- `vars` 的键/值规则与 §3.1 相同（上限 64 项），但**不是按 ns 的**：一台机器只有一份。
- `limits` 形状与语义见 `anode.md §2`（`total` / `perCapability`）；只有本机 + 节点两层。
  合并时 `total` 与 `perCapability` 内部都**递归逐叶**：本机同名键覆盖 node，未写继承；没有 ns 层。
- `listen` / `envPassthrough` **不在**中枢：它们只在这台机器上有意义，留在 `anode.yaml`。

**wire 形状（配置子资源 wrapper，不是独立 kind）**：

`GET /v1/nodes/{name}/config`

```json
{ "metadata": { "resourceVersion": "R" },
  "namespaces": ["*"], "vars": {}, "limits": { "total": 1, "perCapability": {} },
  "configRevision": "C", "appliedRevision": "A", "appliedHash": "sha256:…" }
```

`PUT` 只接 `metadata.resourceVersion` / `namespaces` / `vars` / `limits`，返回**同 GET wrapper**；`configRevision` / `appliedRevision` / `appliedHash` 是只读派生字段，UI **不提交**，也不拿它们当 CAS 令牌（一律先 `GET …/config` 取 `metadata.resourceVersion`）。Node 配置与 `Node.spec` 的底层默认值是**同一份事实**（同一底层版本 / 同一事务），不能各用一个版本绕过 CAS。

### 4.2 同步：所有动作由 anode 发起

中枢**从不主动去问节点**：**每次连上（启动 / 断线重连）anode 第一帧就把自己的机器级配置报上来**，中枢只回应。
ns 默认值**没有独立的同步帧**：它随任务派发帧下发（§2.4），避免出现第二套配置协议。

```
anode                                          中枢
 │ hello {name, token, namespaces,               │
 │        localVars, localLimits,                │
 │        appliedRevision, appliedHash} ────────►│
 │                                                │  该 Node 已由 API 登记；首见 → 只建这台的配置默认行（可为空），不建 Node 本体
 │ ◄────── welcome {configRevision, nodeVars,    │
 │                  nodeLimits, namespaces}      │
 │ 本机 + 节点默认值逐键合并 → 落盘 anode.applied.yaml → 生效
 │ ─ config.applied {revision, hash,             │
 │                   machineVars} ──────────────►│
 │                                                        │
 │ … 中枢里有人改了这台的默认值 …                        │
 │ ◄────── config.changed {revision}   （只是提示，不是命令）
 │ 自己决定什么时候拉：config.get → 合并 → 应用 → config.applied
```

| 规则 | 说明 |
|---|---|
| **谁发起** | 全部同步动作由 anode 发起：`hello`（身份校验 + 上报 + 拉默认值）、`config.get`、`config.applied`。中枢只回应 |
| **中枢只提示** | `config.changed` 只带 revision，不带值也不带命令；节点丢了这条提示也不会漂移（下次 `hello` 自己对齐） |
| **只改节点默认值** | apply 只更新中枢那份节点默认值；**本机写过的键不受影响**（那是本地真相）；**ns 默认值不参与 apply** |
| **落盘** | `anode.applied.yaml`（0600，人不要改）= **本机 + 节点默认值**的合并结果；任务执行读它 |
| **原子** | 写临时文件 + `rename`；写成功前不切生效值 |
| **幂等** | 同一 revision 重复下发 → 直接用，不重复写盘 |
| **任务绑定** | `task.dispatch` 带 `configRevision`（机器级）+ `namespaceVarsRevision`（ns 级）；节点在 **`task.accept` 之前**校验机器级 revision，不一致 → `task.reject`（`stale_config`），中枢换台节点重派；ns 值随任务自带，节点不校验历史 ns revision。`task.dispatch` 不授 lease/执行权 |
| **重启** | 启动读 `anode.applied.yaml` → `hello` 带上 `appliedRevision` → 中枢回当前 revision → 不同才拉 |

帧里还带 `namespaces`：`hello` 带上本机申报的列表，`welcome` 回 `Node.spec.namespaces`（授权列表）；
两者取交集得到生效 ns（§4.1）。它不是“配置”，不走 revision，也不绑 token（节点令牌是集群身份，不绑 ns）。

**这只是配置子协议的完整集合**：`hello` / `welcome` / `config.changed` / `config.get` / `config.applied`；
ns 默认值不单独开帧，只出现在任务派发帧里。

执行帧（派发/接受拒绝、租约 grant/renew/ack、结果、取消）是另一组最小协议，见 `capability.md §8.5`；
不要因为这里列了 5 个配置帧就以为执行只有这些帧。

### 4.3 状态机

| 状态 | 含义 | 参与匹配 |
|---|---|---|
| `pending` | 节点默认值改了，节点还没回报（离线 / 正在跑的任务不打断） | ❌ |
| `applied` | 节点已合并落盘，`revision` + `hash` 与中枢一致 | ✅ |
| `drift` | 节点上的实际值与期望不一致（手改 `anode.applied.yaml` / 落盘失败），message 指到字段 | ❌ |
| `override_stale` | 上层默认值更新过，但这台机器用自己的值压住了它（§2.3）；可与 `applied` 并存 | ✅ |

`namespaces` 不走这个状态机：本机列表改了要**重启**（设备级配置）；中枢授权缩小则**立即生效**（下一次派发就看不到这台机器），不需要节点做任何事。

ns 默认值也不走这个状态机：它没有“节点 applied”概念，改完只影响该 ns 的下一次派发。

### 4.4 批量编辑

- `POST /v1/nodes/config/batch`，body `{updates: [{name, metadata: {resourceVersion}, vars?, limits?, namespaces?}]}`：对每台各生成新 revision，逐台 CAS。
- 响应 `{items: [{name, ok: true, config: <同 GET wrapper>} | {name, ok: false, error: <Status>}]}`：**逐节点原子、可部分成功**（不是 Playbook 那种整包原子规则）；错误里**不得带原始凭证值**。
- 单台写带 `metadata.resourceVersion`；批量写逐台带 `{name, metadata:{resourceVersion}}`。缺失 → 400 `resource_version_required`；旧版本 → 409 `resource_version_conflict`。
  `resourceVersion` 是写并发令牌，**不是** `configRevision` / `namespaceVarsRevision`（那是配置生效版本）。

### 4.5 `anode.yaml`

```yaml
name: node-a
server: wss://anotify.example.com
token: <node token>              # 中枢签发的节点身份令牌；明文只返回这一次，再次签发 = 轮换（旧 token 失效）

namespaces: [default, team-a] # 这台机器允许被哪些 ns 使用（申报），缺省 [default]

listen: tcp://127.0.0.1:0     # 只在这台机器上有意义

vars:                         # 本机值：同键压住节点默认值 / ns 默认值
  SOURCE_ROOT: /home/ci/src
  TOOLCHAIN:   /opt/go1.25

limits:                       # 并发预算（形状见 anode.md §2）；跨 ns 共享
  total: 8

envPassthrough:               # 只写变量名；值留在本机，不回传中枢
  - SSH_AUTH_SOCK
  - GITHUB_TOKEN
```

- 改本机 `vars` / `limits` / `envPassthrough`：**重启 `anode`** 生效（文件只在启动时读），不动中枢 revision。
- `envPassthrough` 只写变量名，禁止 `PATH` / `HOME` / `ANOTIFY_*`；完整名单只在本机，不上报。systemd / container 必须给 `anode` 进程显式注入这些宿主变量，否则任务里就是缺失。
- 改本机 `namespaces`：同样**重启**生效；它不是“配置”，不进 revision，只作为**申报列表**随 `hello` 上报。
- 改中枢节点默认值：节点 apply 后生效，**不用重启**；只影响本机没写的键。
- ns 默认值：改中枢即生效，只影响该 ns 的下一个任务；不写本机文件。
- 状态和来源逐键可见（`local` / `node` / `namespace`），避免"改了中枢却不生效"没人知道。

---

## 5. 校验三层与错误码

### 5.1 三层

| 层 | 时机 | 检查 | 结果 |
|---|---|---|---|
| 写入期 | 建/改能力、写配置 | 名字形状、`requires.values` 声明 | 400，当场拒 |
| 派发期 | `task.accept` 之前 | 按 §2.1 逐键解析 `requires.values`：local → node → 任务 ns；机器级配置是否已 apply；能力所属 ns 是否 ∈ 节点生效 ns（§4.1）；**机器级 `configRevision` 与本机 applied 一致** | 不匹配 → `NodesAvailable` condition；`stale_config` → `task.reject`，回 pending / 换节点（**不计 attempt、不授 lease**） |
| 运行期 | `lease.grant` 后、启动前 | 本地重新求值 `workDir`（绝对路径、已存在、是目录、无 `..`、符号链接解后仍在允许目录）、`HOME`（绝对、存在、是目录）、`requires.commands` | `workdir_unavailable` / `home_unavailable` / `missing_requirement` → `failed`（此 attempt 已计，确定性配置错误不自动重试） |

写入期**不校验「中枢有没有这个值」**：值也可能来自本机或节点默认，三层都没有是派发期才知道的事。

### 5.2 错误码

| 码 | 层 | 含义 |
|---|---|---|
| `value_invalid` | 写入 | 中枢配置（节点 / ns）名字/值不合规、超上限、未知字段 |
| `undeclared_need` | 写入 | `run.env` 引用了 `vars.*`，但没写进 `spec.requires.values`（原 `undeclared_node_var`） |
| `value_in_use` | 写入 | 删除被能力引用的中枢值（409；只针对配置值，不是能力/源删除阻塞） |
| `resource_version_required` | 写入 | 配置写缺 `resourceVersion` 前置条件（400） |
| `resource_version_conflict` | 写入 | 配置写带的 `resourceVersion` 已旧（409；不再使用 `if_revision` 字段） |
| `ValueMissing` | 部署 | 三层都没有这个值（`NodesAvailable` condition 的 reason；message 带任务 ns） |
| `ConfigPending` | 部署 | 该节点的机器级配置还没 apply |
| `stale_config` | 调度（`task.accept` 前） | 任务带的机器级 `configRevision` 与本地 applied 不一致；不计 attempt、不授 lease、不当作执行失败，回 pending / 换候选 |
| `override_stale` | 展示 | 上层默认值更新，被下层值压住的节点 / ns 未更新（不是错误） |

---

## 6. 决策

| 编号 | 议题 | 结论 |
|---|---|---|
| **C1** | 配置字段怎么组织 | **一种值**：`vars.<NAME>`。没有 `secrets`、没有 `hub.*`：三处定义、逐键合并 |
| **C2** | 配置真相在哪 | 本机 `anode.yaml` 存**这台机器的值**；中枢 DB 存**节点默认值**与**ns 默认值**；执行读机器级合并后的 `anode.applied.yaml` 再落任务 ns 默认 |
| **C3** | 值怎么到节点 | 节点默认值走 apply、合并进 `anode.applied.yaml`；ns 默认值**只随任务下发（内存）**；本机值不离开那台机器 |
| **C4** | 值进不进 digest | 不进（沿用 `anode.md D4`）：改值不等于能力定义变了 |
| **C5** | 依赖怎么声明 | `spec.requires.values` 只装名字：`vars.<NAME>`，**不写来源** |
| **C6** | 三层作用域 | 本机 / 节点默认值 = 机器级（跨 ns 共用）；ns 默认值 = 按任务所属 ns 取 |
| **C7** | 要不要 at-rest 加密 | 不做：值里不放密钥（C8），没有加密的理由 |
| **C8** | 要不要 `secrets` | **v4 不做**。凭证用机器上已有的登录态（`gh auth` / ssh key / CI 环境变量）；节点可 `envPassthrough` 按**名字白名单**透传，值不回传、不记录；名单禁止 `PATH` / `HOME` / `ANOTIFY_*`，完整名单只在本机、不上报。`vars` 值一律按非敏感对待（上报、展示、进日志） |
| **C9** | 谁能看/改中枢配置 | 成员可看（值非敏感）；增删改 = 管理员（节点默认值按节点授权，ns 默认值按 ns 权限） |
| **C10** | 节点默认能不能从中枢改 | **能**（单台 / 批量）：中枢给的是**默认值**，本机写了就压住，不冲突 |
| **C11** | 任务详情记不记来源 | 记**名字 + 来源**（`local` / `node` / `namespace`）+ ns revision，**不记值** |
| **C12** | 凭证租约 / 凭证代理 | v4 不做（**不是**任务执行租约；执行租约见 `capability.md §2.3`） |
| **C13** | 同名优先级 | 内置规则 **local > node > namespace**，不需要 `overrides` 声明；但必须可见（节点页 / ns 页 / 任务详情 / `override_stale`） |
| **C14** | 三层怎么合并 | **逐键合并、逐层优先**；本机改重启生效，节点默认走 apply，ns 默认只影响该 ns 的下一个任务 |
| **C15** | 一台机器被多个 ns 共用 | **允许且是常态**：节点是集群级对象，生效 ns = 本机申报 ∩ 中枢授权（**不是逐层优先**） |
| **C16** | `limits` 有几层 | **只有本机 + 节点**两层；跨 ns 共享总槽位，**没有 ns 级配额** |
| **C17** | `anode.applied.yaml` 存什么 | **只存机器级合并结果**（local + node）；ns 默认值**不写入**，只随任务内存传递 |
| **C18** | 配置版本怎么表达 | 两个字段：`configRevision`（机器级，节点可校验 stale）与 `namespaceVarsRevision`（本次 ns 值版本，任务诊断用）；不可压成一个 |
| **C19** | 有效值怎么展示 | **必须带 ns 上下文**：`(节点, ns)` 一组结果；每键标 `local` / `node` / `namespace`；不允许扁平 `effectiveVars` 假装覆盖全部 ns |

---

## 7. 存储（实现布局草案；不影响批准语义）

| 表 / 列 | 内容 |
|---|---|
| `config_values` | `(namespace_id, name)` 主键；`value`、`revision`、`updated_at` = **ns 默认值** |
| `node_configs` | `(node_id)` 主键；`revision`、`namespaces`(JSON 数组)、`vars`(JSON)、`limits`(JSON)、`updated_at`、`applied_revision`、`applied_at`、`applied_hash` = **节点默认值** |
| 本机 `vars` / `limits` | **不落中枢表**：本机值只出现在节点上报的机器级快照里 |
| `envPassthrough` | **完整名单只在本机，不进 Node 快照、不落中枢表**；任务实际用到的名字可出现在任务 `envDiagnostics`（来源 `passthrough`），不报值、不报完整名单 |

- 加列走 `-- +migration N` 块（见 `DEVELOPMENT.md`「schema 变更」），不改 baseline；
  v4 开发库直接建 v4 baseline、不做历史 migration（`decisions.md` V4-D6）。
- ns 默认值没有“节点 applied”列：它不是一个节点状态。

---

## 8. 实施顺序

| 步 | 内容 | 门禁 |
|---|---|---|
| **M1** | 中枢 ns 默认值：表 + API + 权限 + 写入期校验 + revision | 契约 / authz |
| **M2** | 中枢节点默认值：表 + revision + 单台/批量 API | `nodes` / `ws_protocol` |
| **M3** | 配置子协议（anode 发起）：`hello` 带机器级配置与 applied 版本 + `config.get` / `config.applied` + 状态机 + 匹配只认 `applied`；执行帧见 `capability.md §8.5` | `ws_protocol` / `deploy` |
| **M4** | 三层解析（local → node → ns）+ 来源标记 + 任务带双 revision + ns 上下文展示 | `live_agent` / `edge_cases` |

M2 依赖 `anode.md` 的握手与上报；M4 依赖 `capability.md §2.3` 的 `spec.requires.values` 与 `run.env` 求值。

---

## 9. 遗留（不影响定稿；P2 已定）

| 编号 | 事 | 现状 |
|---|---|---|
| **P1** | 中枢 `vars` 的真实用例还只有共享端点 | 先做，等 playbook 里出现第二个需求再评估 |
| **P2** | 本机 `limits` 与节点默认 `limits` 的合并粒度 | **已定**：`total` 与 `perCapability` 内部都**递归逐叶合并**；本机同名键覆盖 node，未写继承；只有这两层，无 ns 层 |
| **P3** | 以后要"中枢统一管凭证" | 必须单开 secret 通道（不进 `vars`、不上报、日志打码），v4 明确不做 |
| **P4** | ns 默认值的 revision 是否要保留历史 | v4 只留当前值 + 单调 revision，不存历史；要“这组值哪一版”看任务帧 |
