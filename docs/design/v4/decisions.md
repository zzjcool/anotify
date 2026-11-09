# v4 已批准决策（本轮权威增补）

> 状态：**已批准**（2026-11-09，用户确认 V4-D1～V4-D6）。
> 本文只记录**本轮批准的决定**与**对原文默认值的主动变更**，不重新定义能力/源/节点/安装包的字段。
> 文档阶段产物：不代表代码已实现；不使现有实现或旧测试成为新方案的限制。

## 0. 权威关系（发生冲突时按这个顺序读）

| 层 | 文档 | 角色 |
|---|---|---|
| 1 | `capability.md` / `source.md` / `anode.md` / `playbook.md` | **语义基线**：四种对象的字段、校验、运行语义以它们为准 |
| 2 | **本文件** | **本轮批准的决策增补**：与基线草案冲突处，以本文件为准；基线文档须按本文件回改，不能保留两套活规则 |
| 3 | `config.md` | 配置细节（D2 的展开）：三层 `vars`、同步协议、`configRevision` / `namespaceVarsRevision` |
| 4 | `api-conventions.md` | **派生**：只定义信封、路由、错误体、读写语义；不重新定义 `spec`，不新增产品功能 |
| 5 | `README.md` | 索引与全局词汇（`fieldPath` 封闭集合、三层配置入口） |

规则：

- **本轮用户批准的 V4-D1～D6 是最高增补权威**：与旧默认或相反语义冲突时，以本文件为准；四份主文档必须落实它。
- 四份主文档仍是字段级事实源；本文件不改字段名，只裁决**语义、默认值、边界**；未被本轮覆盖的原有语义仍以四主为基线。
- `config.md` / `api-conventions.md` / `README.md` 是派生：只能落实主文档 + 本文件，不能给主文档另一套活规则。
- `api-conventions.md` 出现“更 k8s、更完整”的替代设计时，**以本文件与主文档为准**；API 文档只能派生。
- 后续要翻案：改本文件对应条目并写明理由，再同步受影响的主文档；不要只在某一篇里改一半。

---

## V4-D1 轻量资源 API（不引完整 Kubernetes 框架）

**决定**

- 四份主文档（capability / source / anode / playbook）是语义基线；API 从它们派生。
- 保留信封：`apiVersion` / `kind` / `metadata` / `spec` / `status`；`status` **只读**（写请求带 `status` → 400）。
- 保留统一错误体与**必要的**并发写保护；已有 `if_revision` 一类接口可以收成统一的 CAS（`metadata.resourceVersion`）。
- 不新增 `metadata.labels` / `annotations` / `labelSelector` / `fieldSelector` 等产品功能；过滤沿用现有显式查询参数。
- `capability.status` 从 `events` / `tasks` / `deployments` / 节点上报等**事实读取时派生**：
  - 不独立落库、不需要 reconciler、没有 `lastTransitionTime`；
  - 上报的原始事实 / 快照可以保存（节点快照、事件、任务行），但必须与派生状态区分；
  - `status` 保留 `resolved` / `observed` **明细诊断**（数组不能改成仅计数，也不能声称“原样保留”却丢字段）。
- 保持 `metadata.version` 与 `name@version` 语义；内容更新按既有版本规则（`capability.md §3`），
  `next` 原地更新**不产生内容版本、不重推节点**。
- 暂不引入：独立 `CapabilityRevision` kind、`finalizers` / `deletionTimestamp`、级联卸载、
  `/apis` group 路由、discovery、CRD、全量 Kubernetes 控制循环。
- API 命名沿主文档（含 `Source.spec.kind`，不改名 `spec.type`）；路由沿轻量 `/v1` 口径，不引 `/apis/{group}/{version}`。
- `resourceVersion` 不冒充版本号：`metadata.version` 是能力内容修订，`resourceVersion` 只是并发令牌，二者不混同。
- 安装包继续是**自包含目录**：`playbook.yaml` + 引用文件、包内相对路径；`render` 与 body `dryRun` 是两个动作；
  重复 install 的条目补齐语义保留（见 V4-D3）。

**代价（接受）**

- 没有长期状态变迁历史；没有通用持续收敛能力；没有字段级所有权（SSA）。
- 去掉 labels/selectors 后，复杂过滤靠显式参数；这是有意的功能克制。

**明确不做**

- 不把派生 `status` 落库，不写 `observedGeneration`，不补 `lastTransitionTime`。
- 不为“统一形状”发明 `generation`、`CapabilityRevision`、`PlaybookInstall` 等新 kind。
- 不加 `/apis` 前缀、不加 discovery 端点、不做 `?propagationPolicy=`。

---

## V4-D2 三层 `vars` 作用域

**决定**：解析顺序 **本机 `local` > 中枢按节点 `node` > 中枢按任务 namespace `namespace`**。

| 层 | 位置 | 作用域 | 说明 |
|---|---|---|---|
| `local` | `anode.yaml` 的 `vars` | **机器级**，跨 ns 共用 | 本机最终真相；同键压住后两层 |
| `node` | 中枢节点配置的 `vars`（`config.md §4.1`） | **机器级**，跨 ns 共用 | 集中管理单台/批量默认值，不改机器文件 |
| `namespace` | 中枢命名空间配置的 `vars`（`config.md §3`） | **按任务所属 ns 取** | 同 ns 共享默认；不同 ns 可以有不同值 |

规则：

- 逐键解析，不是整份覆盖；缺失 = 该 env 键不出现（不是空串）。
- 本机与 node 是机器级：同一台机器上所有 ns 共用；namespace 层按**任务所属 ns**取，不能拿 A ns 的默认值跑 B ns 的任务。
- `limits` **只有本机 + node 两层**：跨 ns 共享同一组总槽位，**没有 ns 级配额**。
- `anode.applied.yaml` 只存**机器级合并结果**（local + node）；namespace 默认值**不写入** applied，
  只随任务在内存中传递；执行/匹配 = 机器生效值再落 ns 默认。
- 有效值展示必须带 **ns 上下文**，不能拿一份扁平 `effectiveVars` 假装覆盖全部 ns；
  每个键必须标明来源 `local` / `node` / `namespace`。
- 配置版本必须区分两个字段，不能压成一个：
  - `configRevision`：机器 applied 版本（节点配置 revision，节点回报、可校验 stale）；
  - `namespaceVarsRevision`：本次 namespace 值的版本（任务携带，只用于“这组 ns 默认是哪一版”）。
- 节点 `vars` 的静态源事实（本机值、node 默认值）可以记录/上报为快照；**任务 env 诊断只记名字 + 来源，不记实际值**（沿用 C11）。
- 运行中任务的已解析值**不会被后续配置更新改掉**；配置更新只影响下一个任务。
- **换节点重试时机器级值重新解析**：同一逻辑 task 跨 attempt 换到另一台节点，`local` / `node` 值按新节点重新解析，`configRevision` 跟着该 attempt 的节点；不能把 A 机的 `SOURCE_ROOT` 带到 B 机。
  `namespace` 默认值属于任务输入，必须在首次派发时留下可复现载体（原始 ns 配置 revision 或内部执行输入快照），重试沿用；节点只在内存使用，不写成公开 env 值。

**明确不做**

- 不做 ns 级并发配额；不把 namespace 默认值写进 `anode.applied.yaml`；不发明第三套配置帧。

---

## V4-D3 安装：只加不改 + 真冲突原子拒绝

**决定**

- **只加不改不删**，不自动覆盖已有路由；不自动更新已装行。
- 同一组 `values` 再装一次：**仍然重跑“比较 + 补齐”并返回一份新回执**（不是 409，也不是整包跳过）。
- 已有能力的 `next` 差异、已有源 `targets`（含 `params` 映射）差异：**不修改**，回执列 `unapplied[]`
  并写清 `declared` / `actual` / `reason`；体检必须显示，不能伪装全绿。
- 真正的冲突仍**整包拒绝、0 写入**（单事务）：
  - 已有同名源但 `kind` / `config` 真正不同；
  - 显式同版本不同内容。
  不改成“部分安装”。
- `config` 比较要做**默认值归一化**：
  - 省略的服务端生成字段（如 `config.token`）视为“保留已有值”，**不算冲突**；
  - 显式写出的 token 参与比较，但回执/差异报告**脱敏**（只给后 4 位或 `***`）；
  - 不能把所有省略字段都当任意等价——只有契约明确“服务端生成 / 可省略”的字段走保留规则。
- 更新包体**只更新模板**，不自动更新已装行；已装对象只在显式 install 后变化。
- 统一写入语义：`POST` 创建，同 slug 已存在 → **409 `slug_taken`**；改包用 **`PUT` 整体替换包体**，
  不要求“删除重建”，也不把同 slug `POST` 当覆盖。
- 删除包**只删清单**，保留已有运行资源；不引级联卸载。
- 安装回执 / `installed_from` 的**引用完整性**必须定义：回执不跟事件 30 天一起删，不能删出悬空外键（见 V4-D6）。

**明确不做**

- 不做“真正冲突也部分安装”；不做自动覆盖；不做安装时轮换 webhook token；
- 不做包体删除后的级联清理；不做 `propagationPolicy` 语义。

---

## V4-D4 安全执行：重试、租约、尝试、emit 幂等

**决定**

- 保留 `lifecycle` 现有字段，但**主动把 `idempotent` 默认值改为 `false`**（原文 `true`）。
- 只有**明确幂等**（`idempotent: true`）的任务，遇到**短暂基础设施失败 / 失联**才允许自动重试。
- 普通脚本非零退出、确定性配置错误、超时、取消：**默认终态，不重试**。
- 重试是**同一逻辑任务（同一 task 行）**，`attempt` 只是计数器；不新建 task_id，也不把 attempt 关系伪装成 `parentTaskId`（那是业务 emit 的父子关系）。
  可以另存 attempt 诊断事实，但逻辑任务仍是一行。
- 重试钉住原版本 / 原 digest / 原 params，以及本次任务的 namespace 默认输入；机器级 `local` / `node` 值按**本次执行节点**重新解析（见 D2）。
- `attempt` 在**通过配置校验、获得槽位 + 执行租约、进入启动时**递增；排队不计 attempt、不耗租约。
- `stale_config` 是**启动前的调度拒绝**，不是执行失败：不计 attempt、不消耗非幂等重试预算，任务回 `pending`（`waiting_for_config`）等待配置或换候选节点，带退避，不能热循环。
- **取消 / 放弃**：立即停止授权、下发停止并停止续租；节点 TERM 进程组，5s 后 KILL；槽位只在**进程组真正回收后**释放，不能数据库改终态就当节点空闲。
  原因要分清 `cancelled_by_user` / `abandoned_by_user` / 基础设施 `abandoned`。
- 租约技术默认（全目录一致，可按一致性调整参数，但不能一套文档一个值）：
  - 基础租约 **60s**，续租间隔 **20s**；
  - 失联后 **TERM 宽限 5s**，再 **KILL 整个进程组**；
  - 续租有 ACK 与本地 deadline；失联时**尽力**停止旧进程；
  - 旧 attempt 的 token **作废**，迟到的 commit / emit **拒绝**；
  - 不虚假保证外部 exactly-once，整体最多 at-least-once。
- 临时基础设施失败要有**明确 code**（如 `node_lost` / `lease_expired` / `infra_transient`），且必须来自运行器的**结构化事实**；
  不能把任意 shell 退出 5 当 infra：CLI exit 5 只表示“请求结果未知（可能已派发）”，脚本因此非零仍默认 `script_failed`，不自动整任务重试。
- `maxAttempts` 是**总尝试次数**（含第一次），不是“额外重试次数”；超过后按原因进 `failed` 或 `abandoned`，
  两者语义要分清。
- 中枢租约过期可以授权新 attempt；允许短时进程重叠，但旧 attempt 的令牌 / 结果 / emit 由中枢权威检查拒绝，节点本地只做预检。
- 重试有**退避**；退避不占租约。
- `emit` 增加调用方显式幂等键（可命名 `id`，CLI `--id`）：
  - 同一逻辑任务（同一 task 行）+ 同一 `id`，跨 attempt 的**同请求**只派发一次，并返回旧结果；
  - 不带 `id` 时每次 emit 仍产生新派发（行为不变）；
  - 同一 `id` 但 `to` / `data` 不同 → **409**（`emit_id_conflict`）；
  - 不按调用序号自动去重；不把跨 task 的同名 `id` 误合并；
  - 幂等记录不继承到不同版本实现（换版本 / 换参数不是同一逻辑任务）。
- `timeoutSec` 从**子进程启动**起算；默认 `0` 只表示“不设本地执行 timeout”，
  仍有运行任务续租，不是“固定总时长”。

**明确不做**

- 不把非幂等任务自动重跑；不把超时 / 取消当可重试；
- 不承诺 exactly-once，不把“emit 去重”当成下游业务副作用的事务保证。

---

## V4-D5 运行环境与凭证

**决定**

- **不继承完整宿主 env**（沿用“任务环境干净”）。
- 明确基线：`PATH` / `HOME`。
  - `PATH` 取**节点守护进程启动时的基线**，`requires.commands` 探测与任务初始 PATH 是同一份；不做按 task 动态探测的新框架。
  - **暂不支持 `run.env` 覆盖 `PATH`**：写 `PATH` → 400 `env_invalid`；脚本自行改 PATH 属于作者逻辑，不在 `requires.commands` 保障范围。
  - `HOME` 默认 `status.homeDir`；允许 `run.env` 显式 `HOME`，但必须是绝对目录并在执行前验证存在/是目录。
- 节点用**变量名白名单**显式透传本机已有凭证变量：`SSH_AUTH_SOCK`、指定的 CI 凭证变量。
  - 新增本机字段 `envPassthrough`（一个统一名字，不再开第二个字段）；
  - 值不塞 `vars`、不回传中枢、不主动记录 env 快照；
  - `PATH` / `HOME` / `ANOTIFY_` 开头的变量**禁止**列入 `envPassthrough`（PATH/HOME 有专门基线，契约变量保留）；
  - `run.env` 与白名单同名时的优先级：**显式 `run.env` 优先**；
  - 非 `ANOTIFY_` 且未列入白名单的变量，一律不继承；`run.env` 的其它同名显式值优先。
  - `envPassthrough` 的完整名单只在本机，不上报节点快照；systemd / container 必须给 `anode` 进程显式注入这些宿主变量，没注入就是缺失。
- v4 **不新增中枢凭证管理资源**（无 secrets 资源、无 at-rest 加密需求、无下发密钥）；透传的只是宿主已有权限。
- 这只是**宿主权限执行**，不是 sandbox：系统不承诺阻止脚本把密钥写进 stdout / 日志；能力作者对输出负责，文档必须写明。

**A2 实现期细则（正式修订，尚不代表实现完成）**

- **修订原因与授权**：用户已授权协调者安排技术细则；既有产物能力需要真实生产入口，固定用户 `workDir/.anotify-artifacts` 存在就拒绝会破坏第二次任务、重试和共享目录并发，故不采用。A2 正式新增基础契约变量 `ANOTIFY_ARTIFACT_DIR`，不是原清单已经包含它；原 `ANOTIFY_EMIT_ENDPOINT` / `ANOTIFY_TASK_TOKEN` / `ANOTIFY_TASK_ID` / `ANOTIFY_PARAM_<NAME>` 保留，参数前缀不变，D1～D6 产品语义不变。
- 节点在系统自有 `exportRoot` 下为每个 `(taskId, attempt)` 创建唯一目录（0700、唯一随机后缀），以新变量提供绝对路径；脚本显式写普通文件（例子见 `capability.md §4.0`）。节点仅安全枚举本 attempt 目录（禁止符号链接/非普通文件/越界）→ 按 A1 限额托管复制、上传 → `task.result.artifacts` 引用；空目录才可真实返回 `[]`，创建/注入/枚举失败或安全边界违规须以 `artifact_export_invalid` 或 `files_escape` 明确 `failed`，不得假空。
- `run.env` / `envPassthrough` 不得覆盖新变量或任何 `ANOTIFY_` 基础契约变量；新变量不进 `vars`、配置 hash 或公开 env 值快照。不新增 `run.artifacts`、第四 CLI、魔法 stdout 或额外宿主 env 继承。
- 同 task 跨 attempt/旧 attempt 重叠与多 task 共用 `workDir` 均使用不同导出目录，不混旧产物；不在用户目录内创建/删除专用导出目录。沿 V4-D6 的 result TTL 默认 7 天、显式 `0` 永久，只清系统自有 export/staging/托管副本，活动 attempt/上传不清理，用户 `workDir` 原文件不动。

**明确不做**

- 不做完整宿主 env 继承；不做凭证 UI / 下发 / 轮换；不声称能防恶意脚本外泄。

---

## V4-D6 有限保留与引用完整性

**决定**（主动改默认值）

| 对象 | 原文默认 | 批准后默认 | 说明 |
|---|---|---|---|
| 日志 / 产物 / 系统创建的任务目录 | 永久（清理默认关） | **7 天** | `resultTtlSec` 默认 `604800`；显式 `0` = 永久 |
| 事件 / trace 元数据及其去重记录 | 永久 | **30 天**（`2592000s`，可配置） | 同 `id` 只在窗口内保护事件接收 |
| 用户指定 `workDir` | — | **绝不删除** | 清理只能动系统创建的目录 |
| 安装回执 / `installed_from` | 未定义 | **不跟事件 30 天一起删** | 先清执行数据；引用未解除时延长保留，不许悬空外键 |
| 任务行（终态后） | 未定义 | **30 天**（`retention.taskTtlSec`，可配置） | 活动任务不清理；与事件/trace 各自计时 |

规则：

- 同 `id` 在保留窗口内只表示“事件已接收”，**不保证下游执行成功**；窗口过期后同 `id` **可以再次被接收**，必须明文说明。
- 修改 payload 或重新投递：用**新 `id`**；v4 **不加 replay API**。
- `run.files` 写入用户目录时，清理/覆盖边界必须界定：
  - 不自动递归删除用户路径；**用户 `workDir` 内任何原文件（含脚本生成的产物）不被 `resultTtlSec` 自动删**；
  - 需要托管保留的产物复制到**独立系统结果目录**，按 7 天清理副本，artifact API 指向托管副本；
  - 拒绝 `workDir` 与系统清理根重叠，避免误删祖先；符号链接与 `..` 在写入/执行边界挡掉。
- 中枢保留配置放系统配置（`retention.eventTtlSec` / `retention.taskTtlSec` / `retention.resultTtlSec`），不塞 `vars`、不混 node 限额。
- GC 引用保留规则：
  - 先清关联执行数据（日志/产物）；
  - 必要引用延长保留（如安装回执被 `installed_from` 指向）；
  - 不把 `installed_from` 当事件过期一起删。
- 不引历史 migration：开发库直接建 v4 baseline；API 文档里 `migration 12` 等旧方案删除/标明不采用。

**明确不做**

- 不做永久保留作为默认；不做 replay API；不删除用户指定 `workDir`；不做级联清理。

---

## 对原文默认值的主动变更（汇总）

| 项 | 原文 | 批准后 | 影响文档 |
|---|---|---|---|
| `spec.lifecycle.idempotent` | `true` | **`false`** | `capability.md §2.3` |
| `spec.lifecycle.resultTtlSec` | `0`（永久） | **`604800`**（7 天；`0` = 永久） | `capability.md §2.3` / `§8.6` |
| 事件 / trace / 去重保留 | 未定 / 永久 | **默认 30 天，可配置** | `source.md §3.3` / `capability.md §8.6` |
| 租约 / 续租 / 宽限 | 未定 | **60s / 20s / 5s，KILL 进程组** | `capability.md §2.3` / `§4.4` |
| `emit` 幂等键 | 无 | **可选 `id`（CLI `--id`）** | `capability.md §4.1` |
| 凭证透传 | 只有“不建模” | **节点 `envPassthrough` 名字白名单** | `anode.md §2/§3.2` / `capability.md §4.0` |
| `PATH` / `HOME` | 只说“环境干净” | **明确基线 + 探测/执行一致** | `capability.md §4.0` / `anode.md §3.2` |

## 刻意不做（全目录统一口径）

- 不落库派生 `status`、不引 reconciler、不补 `lastTransitionTime` / `observedGeneration`。
- 不引独立 `CapabilityRevision` kind、`generation`、`finalizers`、`deletionTimestamp`、级联卸载。
- 不新增 `labels` / `annotations` / selectors；不引 `/apis` group 路由、discovery、CRD、SSA。
- 不新增中枢凭证管理 / secrets 通道；不承诺 sandbox；不承诺 exactly-once。
- 不自动覆盖已有路由；不自动重跑非幂等任务；不自动扩张节点授权 ns。
- 不做永久保留默认；不加 replay API；不做历史 migration / 旧库升级。

---

## 场景自查（文档阶段）

| # | 场景 | 走查结果（文档口径） | 主要落点 |
|---|---|---|---|
| 1 | 正常执行 | 派发钉住 `version` / `digest` / `params`；节点用机器级 applied + 本次 ns 值组环境；`attempt` 在获槽 + 租约、进入启动时递增；日志/产物按 7 天保留 | `capability.md §2.3/§4/§8.3`、`config.md §2.4`、`anode.md §3.2` |
| 2 | 重复安装 | 同 values 再装仍走比较 + 补齐，**返回新回执**（不 409）；已装行不被自动更新；删包只删清单 | `playbook.md §4.2/§4.7/§4.9`、`api-conventions.md §7.3` |
| 3 | token 省略 | 省略服务端生成的 `config.token` → 保留已有值、不算冲突；显式 token 参与比较但回执脱敏；其它省略字段不搞“任意等价” | `playbook.md §4.2/§4.5` |
| 4 | 源 targets 差异 | 同名源 kind/config 等价但 `targets`/`params` 不同 → 不改 targets，回执 `unapplied[]` / `targets_unapplied`，`ready=false`，体检不全绿 | `playbook.md §4.2/§4.5/§6.2`、`api-conventions.md §7.3` |
| 5 | 多 ns | `vars` 三层 local > node > namespace；applied 只机器级；ns 默认随任务内存；展示带 `(节点, ns)` 上下文；`limits` 无 ns 配额 | `config.md §2/§4`、`README.md`、`api-conventions.md §7.4` |
| 6 | 失联重试 | 仅 `idempotent: true` 的短暂基础设施失败/失联可重试；租约 60s/续租 20s/TERM 5s 后 KILL 进程组；旧 attempt token 作废；最多 at-least-once | `capability.md §2.3/§8.3` |
| 7 | 取消 | 取消/放弃为终态，不自动重试；任务只读，走 cancel/abandon 动作端点；`cancelled` / `abandoned` 与 `failed` 分开 | `capability.md §2.3`、`api-conventions.md §7.6` |
| 8 | emit 超时重试 | 带 `--id` / `id`：同逻辑任务同请求只派发一次并返回旧结果；不带 id 每次不同；同 id 不同 to/data → 409；不按序号去重、不跨 task 合并、不继承不同版本 | `capability.md §4.1/§6.2` |
| 9 | 环境白名单 | 不继承完整宿主 env；基线 PATH/HOME；探测与执行 PATH 一致；`envPassthrough` 只写名字、值不回传；`ANOTIFY_` 禁止；`run.env` 同名优先；宿主权限非 sandbox | `capability.md §4.0`、`anode.md §2/§3.2`、`config.md C8` |
| 10 | 清理 | 日志/产物/系统目录默认 7 天，事件/trace/去重默认 30 天，显式 0 永久；用户 `workDir` 永不删；GC 先清执行数据、回执被引用时延长保留；无 replay API、无历史 migration | `capability.md §8.4`、`source.md §3.3`、`playbook.md §7.3`、`api-conventions.md §10.2` |

## 修改地图（本轮）

| 文档 | 改动性质 |
|---|---|
| `README.md` | 加 `decisions.md` / `api-conventions.md` 索引；配置源改三层；状态保留说明；`envPassthrough` 与 PATH/HOME |
| `decisions.md` | 新建：V4-D1～D6 批准记录、默认值变更、刻意不做、场景自查 |
| `config.md` | 重写为三层 `vars`（local / node / namespace）、`limits` 两层、applied 只机器级、双 revision、最小协议帧 |
| `capability.md` | `idempotent` 默认 false；7 天/30 天保留；租约/attempt/重试矩阵；emit `id`；PATH/HOME/`envPassthrough`；files 规范化；status 派生说明；非目标与决策行 |
| `source.md` | D3 改文件引用；事件 `id` 30 天窗口、过期可再接收、改 payload 用新 `id`；错误码机器形式 |
| `playbook.md` | 源 targets 差异 `unapplied`；config 归一化；POST 409 / PUT 更新；回执引用完整性；体检不全绿；D32–D35 |
| `anode.md` | 三层 vars；`envPassthrough`；PATH/HOME；token 不绑 ns；快照只机器级；D8 |
| `api-conventions.md` | 重写为派生契约：`/v1`、`spec.kind`、`metadata.version`、无 CapabilityRevision/labels/finalizers/reconciler/migration 12；保留信封/错误体/端点面；任务 env 诊断不记值 |

---

## R1 返修闭环（2026-11-09 复审）

| 编号 | 闭环位置（正文句子，不是新增决策表） |
|---|---|
| **R1 权威/status/vars/limits/NodesAvailable** | `decisions.md §0`（最高增补权威 + 派生关系）；`capability.md §2.4`（API 写 400 / CLI 导入剥离）、`§5`（NodesAvailable 全条件）；`config.md C12`（凭证租约不是执行租约）；`anode.md §2/§6`（limits 两层、ns 授权/在线/applied/commands/values） |
| **R2 路径/配置入口/CAS/部署/实例/删除/脱敏** | `api-conventions.md §5`（全 ns 路径、`/config`、`/system/config`）、`§7.1`（deploy、删除不阻塞）、`§7.2`（源读写分离）、`§7.3`（installs 折叠 + 单回执、lastInstallId、kept/values）、`§7.4/§7.5`（Node/Namespace config、无 seed）；`config.md §3.5/§4.4/§5.2`；`playbook.md §4.10/§7.2`；`source.md §3.1` |
| **R3 安装/版本/回执** | `playbook.md §4.2`（12 格、入口边仅新源）、`§4.4`（0 写入）、`§4.5`（回执/lastInstallId/脱敏）、`§4.6`（dryRun 不落回执行）、`§4.10`（折叠 + 单回执）、`§5`（install 不部署）；`capability.md §3`（foldKey + 显式版本不可变）、`§6.1`（`version_content_immutable`）；`api-conventions.md §3.1`（warnings）、`§4.2`（码表） |
| **R4 任务/重试/协议** | `capability.md §2.3`（一行 task、attempt、stale_config、cancel）、`§8.3`（waitingReason/attempts、不新建 task）、`§8.5`（配置子协议 + 执行帧 + ACK 时序）；`config.md §2.4`（换节点重解析机器值、ns 输入可复现）、`§5.2`（stale_config 不计 attempt）；`api-conventions.md §7.6`（phase 小写、waitingReason/nextAttemptAt/attempts） |
| **R5 emit/错误分类** | `capability.md §4.1`（exit 5 语义、剪枝回执 taskId:null、同 id 重试）、`§6.2`（param_* / no_target / event_schema 剪枝词表、script_failed/files_escape/stale_config/cancelled/abandoned）、`§6.2` 后错误域表；`api-conventions.md §4.2/§4.3`（Status vs 本地 emit vs Task.result、condition 词表、413） |
| **R6 环境/文件边界** | `capability.md §2.3`（写入期 vs 执行前 files 检查、0600/0700、覆盖副作用）、`§4.0`（PATH 基线、禁止 run.env PATH、HOME 显式、envPassthrough 限制）、`§10`（非目标）；`anode.md §2/§3.2/§5`；`config.md §1`（删除“中枢永远不是目录类”假断言）；`README.md` 取值语法 |
| **R7 保留/GC/物理清理** | `capability.md §8.4`（task 30 天、artifact 托管副本、workDir 原文件不删、引用组、deliveryId/eventId、保留配置）；`config.md §3.6`（`retention.*`，不进 vars）；`source.md §3.3`（deliveryId/窗口）；`playbook.md §7.3`（回执/installed_from）；`api-conventions.md §7.6/§7.7` |
| **R8 收尾/状态/门禁** | 各文档状态头统一为“语义冻结/实现未开始”（`decisions` 已批准）；`api-conventions.md §9/§10.3`（保留 vs 移除、门禁禁词）；`playbook.md §12`（12 格）；本表 |
