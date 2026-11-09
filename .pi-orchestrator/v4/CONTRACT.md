# V4 全量实施冻结契约 · rev1

状态：用户已授权实施；父会话冻结分工与技术边界。设计基线 `368b489`。
语义权威：`docs/design/v4/decisions.md` V4-D1～D6 为批准增补，四主文档为其余语义基线；`config.md`/`api-conventions.md` 是派生接口约定。实现不得自行修改已冻结设计迁就旧代码/测试。

## 1. 交付范围与非目标

- 全量切到 v4，不提供 v3 API/字段/端口/WS 帧/旧数据库迁移兼容分支。`schema.sql` 直建 v4 baseline，旧库明确拒绝，绝不自动删用户库。
- 能力只有脚本实现；删旧 handler/outputs/ports/priority/sticky/中枢能力 executor/notify-phone。认证、passkey、设备、VAPID、消息/推送等未被设计退役的独立域不得误删。
- 保留 spec/status 信封与统一错误，status 读取时从事实派生，保留 resolved/observed 明细。不造持久化派生 status、reconciler、generation/独立 CapabilityRevision、labels/selectors、finalizer/级联卸载。
- 不加多中枢、RBAC、sandbox、中枢凭证模型、replay API，不擅自扩大功能。
- `.pi/settings.json` 是用户既有改动；禁止修改、暂存或提交它。禁止改 `.env*`、`.git`、`node_modules`。没有自动重置用户修改、删除用户目录的权限。

## 2. 团队与文件所有权（唯一 writer）

| 团队 | 独占文件 | 不得改 |
|---|---|---|
| T1 core-runtime | 全部 `internal/` **除 `internal/sitegen/`**；`cmd/` **除 `cmd/sitegen/`、`cmd/fe-preview/`**；`api/`；Go依赖文件（仅必要新增）、`Makefile`、Dockerfile(s)、compose文件；`scripts/` **除下方 T2 列出的文件**；`docs/contracts/CONTRACT.md`、安装/开发/测试/README/CHANGELOG说明；`.gitignore` 中新二进制忽略项 | `web/`、`web-src/`、T2文件、已冻结 `docs/design/v4/`、根 `.pi-orchestrator/v4/{CONTRACT,PLAN,STATUS}.md`、用户 settings/env/依赖目录 |
| T2 frontend | `web-src/**`；跟踪的 `web/**` 静态 JS/CSS/SW/assets（不提交生成HTML/hash dist）；`internal/sitegen/**`、`cmd/sitegen/**`、`cmd/fe-preview/**`；`scripts/v4-frontend.test.mjs`；`scripts/e2e/suites/frontend.mjs`、`i18n.mjs`、`i18n_coverage.mjs`、`ui_shift.mjs`、`ux_regression_19_23.mjs` | T1文件（尤其Makefile/api/schema/harness）、已冻结v4设计、根编排状态 |
| 父会话 | 根编排契约/状态、集成决策、仅必要跨团队缝隙、最终合并及验收 | 不在团队写入中抢改其文件 |

两个 centurion 各一个隔离 worktree，团队成员 `worktree:false`。每个 worktree 同时只有一个 writer；可以并行只读 scout/review。成员不得自己再派写入 worker。centurion 负责按串行里程碑调度 writer，不在共享文件并发写。

## 3. T1 内部阶段与交付规则

T1 是一个 Go 原子交付单元，避免隔离的小模块因同 module 类型变更互相无法验收，且不靠兼容层恢复编译。

1. **G1 领域与存储**：artifact/spec/foldKey/digest；v4 baseline；cluster Node、配置三层、Task/attempt/lease事实、next/targets投影、目录 Playbook/render/回执/原子安装、去重与GC。先独立验证 artifact/store/protocol；允许中间旧依赖暂时编译失败，记录原因，不宣称整体通过，不合入父分支。
2. **G2 执行与节点**：route/trigger/source/ws/lease + 新 `cmd/anode`；更新节点二进制/安装器；同帧定义、grant授权、HTTP emit、取消/结果/环境/物理产物清理。
3. **G3 HTTP与系统装配**：namespace路径、spec/status DTO、CAS、派生状态、源/能力/包/任务/配置/部署、人工部署、auth/device未退役域；生成新的 OpenAPI 与契约门禁，删除旧生产兼容结构。
4. **G4 测试/工具链与整体验证**：devseed、E2E harness/WS fixtures及 T1 套件、命名/安装/构建/静态契约说明。全 Go 编译与相关单测、contract、E2E quick 通过后才能整队交付。

G1后产出可读的 `EXPORTED-INTERFACES.md`（团队自己的进度目录）记录实际 store/route/protocol 方法签名；后续成员按它实现，不再发明另一个公共类型。可变的内部实现接口不要求用户逐字段批准；与文档不一致则先上报父会话。

## 4. 跨团队接口冻结

### HTTP / UI

- 私有资源只使用 `/v1/namespaces/{namespace}/...`：capabilities/sources/playbooks/tasks/events/traces/edges/deployments。namespace和node管理为 `/v1/namespaces`、`/v1/nodes`；hooks为 `/v1/hooks/{token}`（token定位ns）。不提供旧扁平资源接口供新UI兜底。
- 资源读写按 `api-conventions.md`：`apiVersion/kind/metadata/spec/status`（列表为 KindList.items）；响应可选顶层 `warnings[]`。camelCase，空数组为 `[]`，错误是 Status。不可用旧命名数组/key作为兜底。
- `metadata.name` 沿四主命名正则，`metadata.version`/`name@version` 保留；省略namespace由明确path/CLI补齐，显式与path不符400。
- API直接写status400，CLI/UI导入剥离status后提交。配置CAS必须传 `metadata.resourceVersion`，缺400、旧409；`resourceVersion`不是 `configRevision/namespaceVarsRevision`。
- 包POST创建同slug409、PUT整体换模板；GET `?includeBody=true` 给未渲染 files；render只读模板不读安装冲突库；install.body.dryRun 0持久写入，不落回执；普通install每次新回执；installs按 valuesDigest折叠，单回执通过installId读取。
- 源读取 `hookUrl`（授权读）/`secretSet`，不回显token/secret字段；写/读示例分开。源PATCH targets同事务重建入口投影。
- task.phase小写，waitingReason/nextAttemptAt/attempt/maxAttempts，envDiagnostics只名字+来源。Playbook未生效项不能伪装全绿。
- 认证/通知等未退役域原表达保持，但不在资源UI里保留旧资源compat adapter。

### 域模型 / 存储

- foldKey取 name+执行正文，排除 version/next/displayName；省略version先判等/分配再算含version的digest。显式同版内容不可变；next/displayName按既定例外原地更新、digest不变、不推节点。
- Edge是投影：能力 `spec.next` 和源 `spec.targets`；无port/priority/mapping/literals。无直接写Edge API。
- vars `local > node > namespace`，limits仅local/node两层并递归逐叶合并；Node cluster、授权=申报∩中枢授权；applied只机器层，namespace默认随任务内存使用。
- 同逻辑 Task 一行，attempt计数器；grant后attempt生效；重试钉版/params/ns输入，新执行节点重解析机器值；每attempt运行期间值固定。非幂等默认false，maxAttempts总尝试默认3。
- 安装只INSERT新对象，不改已有路由，不删资源；能力next/源targets差异unapplied；kind/config/显式版本真conflict单事务整包0写。归一化defaults，省略自动token保留已有，显式token参与比较且差异脱敏。
- 用户显式删除可留下逻辑缺失引用，引用方状态报告，不新增in_use/finalizer。已被task钉住执行事实保留到引用安全GC。install不自动部署，保留人工部署动作。

### Node协议 / 执行

- 一个零依赖共享协议类型源（建议新增 `internal/protocol/` 或移出原 ws 的纯协议子包）。server/anode直接使用同一份Go定义，禁止各写snake/camel镜像；节点二进制不引SQLite/server依赖。不提供旧frame aliases。
- 配置子协议：hello/welcome/config.changed/config.get/config.applied，具体字段按config文档，只有机器值快照+revision/hash，未回传凭证值/完整白名单。
- 执行10帧按capability §8.5：task.dispatch/accept/reject、lease.grant/renew/ack、task.result/result.ack、task.cancel/cancel.ack。dispatch只assignmentId/proposedAttempt、不授lease/token；stale_config仅accept前reject不计attempt；grant由中枢生成leaseId/attemptToken并计attempt。固定默认TTL60s/续租20s/TERM宽限5s后KILL进程组；节点本地ACK deadline失效自行停，旧attempt结果/emit中枢权威拒绝。cancel槽位等processGroupReaped再释放。
- grant后启动前 missing_requirement/workdir_unavailable/home_unavailable 等确定性错误failed已计attempt，不重试；stale_config回pending/waiting_for_config不耗执行预算，不热循环。仅显式幂等任务的可靠infra事实/失联允许自动重试，普通script非零/timeout/cancel不自动重试。
- 本机一个HTTP emit网关默认loopback内核端口，可按文档unix形态；转中枢权威派发。请求 `{to,id?,data}`，Bearer attemptToken；无to missing_target400/CLI2；no_target仅Source.targets为空剪枝。可选id以逻辑(taskId,id)跨attempt去重，异to/data409；无id每次不同，不按旧seq自动业务去重；剪枝200 taskId:null/eventId/pruned，exit0。exit5只表示结果未知，可能已派发，不能凭shell退出码推断infra自动整任务重跑。
- 只有serve/install/emit三个公开子命令；改二进制与服务/config名为anode/anode.yaml/anode.service，不加旧命名alias。curl安装脚本只负责下载并调用anode install（或与它保持单源的最小引导），不能存在两套不同初始化语义。
- PATH=守护进程启动基线且探测/执行同源，本轮run.env不能覆盖PATH；HOME可显式绝对目录；envPassthrough完整名单仅本机、禁PATH/HOME/ANOTIFY_、只按显式变量名透传且run.env同名优先。值不进vars/中枢/诊断快照，实际用到名字可诊断。files静态路径与限额写入校验；真实symlink边界执行前files_escape。0600/入口0700，告知用户目录同名覆盖副作用。

### 保留与物理清理

- 系统配置 `retention.resultTtlSec`默认604800、eventTtlSec/taskTtlSec默认2592000；0永久。task行终态计时，活动task不GC；事件/trace/id窗各自计时及引用组安全保留。
- 默认system创建任务/结果目录7天；artifact复制到独立托管结果目录，API指托管副本；不删除用户workDir内任何原文件/产物，拒绝其与系统GC根重叠。回执/installedFrom引用不随30天事件窗口破坏。
- deliveryId业务去重窗与内部eventId不同；为引用延长实体保留不延长既定业务去重窗。observed最近一个逻辑task，清理后如实空数组，不杜撰历史。

## 5. 测试/验收与防伪绿灯

- 每个 writer 必带verify数组。阶段验收只跑该阶段闭包，但整体交付必须 `go test ./... -count=1`、`make contract`、E2E quick（T1）及 `make fe`、sitegen测试、新前端单测、页面检查（T2）。
- 可删除纯旧语义专属ports/priority/迁移测试，但必须在团队测试映射表列旧用例→新契约用例；不全删测试、不skip、不弱化断言迁就产品bug。认证/passkey/设备等保留功能回归不得丢。
- 必测：digest/next不出版本；字段往返；ns授权+三层配置+CAS；12格安装+dryRun+源targets差异+token省略；重复delivery/id不同payload/过窗；emit跨attempt幂等和剪枝；stale无attempt/grant唯一授权/旧attempt拒绝；取消回收；非幂等不重跑；干净环境/凭证白名单；files_escape/用户目录GC安全；raw包体/实例溯源/派生status。
- 父会话交叉集成后重建 `make fe` 和 `make build`，跑全量 `make e2e`、`make ci`、目标页面dark/light及桌面/手机 `web_verify`。全量E2E只在最终集成门禁跑，不让每个小阶段重复全量。
- 中间Go依赖编译失败是阶段状态，不是完成；不得用legacy structs/stubs/fallback接口掩盖。团队交付须真实v4闭环，不得自行缩减核心范围/以时间不足宣布全量完成。

## 6. 上报与集成

- 团队自己的进度、接口清单、测试映射、问题写入 `.pi-orchestrator/v4/teams/{core-runtime|frontend}/`，不要改父STATUS/PLAN/CONTRACT。
- 父会话先fresh review（正确性/覆盖/简洁性），重大项返修；在当前 `feat/p9-capability-v4` 集成分支按 T1→T2 合并，T2不得merge T1或反之。中间代码提交仅留各自交付分支。
- 契约矛盾必须上报父会话决定，不擅自改冻结v4设计。文档在另一分支看不到时要任务卡内容，不另建同名权威。
- 实施交付可能由harness生成隔离分支/MR，只有父会话决定最终合入；不推当前集成分支或用户settings。
