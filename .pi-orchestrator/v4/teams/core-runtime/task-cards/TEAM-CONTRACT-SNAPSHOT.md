# V4-CORE-RUNTIME · T1 实施冻结契约（内部 rev2；父所有权修订 21567b7 已批准）

## 父批准覆盖条款（2026-11-09；优先于下方历史任务表）

父修订唯一内容源：`/tmp/anotify-v4-core-protocol-carveout.md`（已全文读取）。不 cherry-pick 整个父分支。
- **T3 唯一 owner**：`internal/protocol/**`、`cmd/anode/**`、`Dockerfile.anode`、`scripts/install-anode.sh`、`docs/anode-install.md`。T1 不再原创写这些文件、不派 anode writer；下方 G2-4 中这些内容已移交 T3，旧历史表不授予 T1 写权。
- **T1 保留 owner**：artifact/store/route/ws/server/api、Makefile、旧 `cmd/agent`/executor 退役、旧安装脚本/安装自测、compose/构建/OpenAPI/checker/非前端E2E。T3 对 artifact 仅只读；T1 artifact 后续直接消费 N1 环境常量/ParamEnvName。
- **N1 具名接口**：`MachineConfigHash(vars,Limits)` canonical `{vars,limits}` SHA256，空nil→{}；不含revision/ns defaults/token/passthrough。`capability.deploy{requestId,namespace,name,version,digest}` / `capability.deployed{同引用,status:applied|failed,error}`；节点身份 GET `/v1/node/capabilities/{digest}` 获取 Artifact 全文，T1 校验 ns + 明确的本节点部署意图，T3 cache/client。
- 人工部署不把 files 塞 WS；离线重连只重放明确未完成部署意图，install 不部署，不造 reconciler。协议快照由父传，T1 只授权同步指定 protocol 原码，不合 T3 产品分支、不私造镜像类型。
- G1-4 当前不打断：owner 明确为 `internal/store/**` + `internal/sourcespec/**`（纯 Source 规格/模板/cron 校验，零 store/route/server 依赖）。不得原创修改 protocol；其现有 go test 可只读执行。
- **E1父批准传输**：task.emit/task.emit.ack只由T3写protocol；T1store权威接受输入携Node身份/taskId/attempt/leaseId/attemptToken/now，同一事务验权（ns交集、running、node匹配、当前attempt/lease/token/租约未过期）后判显式id/写emit+子任务/剪枝；重复id不绕授权、requestId不是业务seq。本队旧任务卡「调用方先VerifyAttemptToken」错误已纠正。
- **GC父裁决（2026-11-09）**：删除包清单保留回执，playbook_installs.playbook_id历史id不级联删；保留receipt原包id/slug/ns快照；installed_from实际回执/保留trace引用仍保护。自动event/task/result sweeper不清receipt，独立显式cutoff+引用安全primitive可留但不暴露API或默认后台。event/trace被task引用组安全保留；delivery业务窗口独立(ns,stableSourceID,deliveryID,eventId,acceptedAt,expiresAt)，接收事务按expiresAt判等，不依赖sweeper；events本体不可永恒UNIQUE delivery。过窗可新event、旧引用保留不延长去重窗。详见父mail裁决，不用本隊原错误任务卡迁就。
- 测试深度和最终门禁不减；旧 in-package 测试可否推迟 G4 尚未批准，暂保留 G1-6 闭包计划。

状态：centurion 冻结，2026-11-10；rev2 修订：G1-1 完成记录、D-I13/D-I14、G1-2 范围调整（schema+capability 领域+编译桥，rev1 低估了 artifact 类型变更对 store 的编译耦合）。基线 `0e0ff0f`（v4 设计 `368b489`，父契约 `.pi-orchestrator/v4/CONTRACT.md` rev1）。
权威顺序：父契约 + `docs/design/v4/{capability,source,anode,playbook}.md`（语义基线） > `decisions.md` V4-D1~D6（增补） > `config.md` > `api-conventions.md` > 本文件（只冻结**任务边界、文件所有权、内部接口落点与文档留白处的实现裁决**，不新增产品语义）。
任何冲突：先停、写进 PROGRESS/邮件上报，不改冻结文档、不擅自扩大范围。

## 0. 工作区与纪律（所有成员必须遵守）

- 唯一工作区：本 worktree，分支 `pi-subagent/v4-core-runtime-775b3657`。不得写父 checkout、不得 push、不得 merge 父分支或 main。
- 同一时刻只有一个 writer（`worktree:false`）。只读 scout/review 可并行。
- 提交：`type(scope): 中文描述`；只 `git add` 本任务卡列出的文件；禁止提交 `.pi/settings.json`、`.env*`、`.git`、`node_modules`、`web/`、`web-src/`、`internal/sitegen/`、`cmd/sitegen/`、`cmd/fe-preview/`、T2 前端脚本、`docs/design/v4/**`、父 `.pi-orchestrator/v4/{CONTRACT,PLAN,STATUS}.md`。
- 禁止 `sudo`、交互式命令、`rm -rf` 根/家目录、直接访问 legion.db、裸长命令；长命令用 `async_run` 命名保存日志，探针加 `timeout 60`。
- 真实进度写 `.pi-orchestrator/v4/teams/core-runtime/PROGRESS.md`（由 centurion 维护；worker 在 DONE 报告里给可粘贴内容即可）。
- 不以「整体编译未过」为理由造 stub/legacy struct/fallback/弱断言；中间态如实记录。仅纯 v3 语义专属测试可退役，且必须写入 `TEST-MAPPING.md`（旧用例 → 新契约用例/新文件）。认证/passkey/设备/push/通知 API 回归不得丢。
- 每个 worker 必须给：改动文件清单、新增/改写测试名、verify 命令与输出、发现的产品 bug、遗留风险、`DONE <task-id>`。

## 1. 文件所有权（T1 独占；相对仓库根）

允许写：
- `internal/**`，**除** `internal/sitegen/**`
- `cmd/**`，**除** `cmd/sitegen/**`、`cmd/fe-preview/**`
- `api/**`、`go.mod`/`go.sum`（仅必要新依赖；优先零新依赖）
- `Makefile`、`Dockerfile`、`Dockerfile.agent`、`docker-compose*.yml`
- `scripts/**`，**除** `scripts/v4-frontend.test.mjs`、`scripts/e2e/suites/{frontend,i18n,i18n_coverage,ui_shift,ux_regression_19_23}.mjs`
- `docs/contracts/CONTRACT.md`、根 `README.md`/`README.en.md`/`DEVELOPMENT.md`/`E2E_TESTING.md`/`CHANGELOG.md` 中与 v4 实现直接相关的章节、`.gitignore`（仅新二进制忽略项）
- `.pi-orchestrator/v4/teams/core-runtime/**`（团队进度/接口/测试映射）

禁止写：`web/**`、`web-src/**`、`internal/sitegen/**`、`cmd/sitegen/**`、`cmd/fe-preview/**`、T2 脚本（上列）、`docs/design/v4/**`、父 `.pi-orchestrator/v4/{CONTRACT,PLAN,STATUS}.md`、`.pi/settings.json`、`.env*`、`.git`、`node_modules`。

新增/删除的固定落点：
- 新增 `internal/protocol/**`（零依赖协议唯一源）。
- `cmd/agent/**` → `cmd/anode/**`（`git mv`，改名后旧路径删除）；`internal/executor/**` 删除。
- `internal/store/schema.sql` 直接重建为 v4 baseline（单块 `-- +migration 1 baseline`，旧库 `Open` 明确报错，不自动删数据）。

## 2. 文档留白处的实现裁决（D-I，内部实现；主文档若相反以主文档为准）

- **D-I1 协议包**：所有节点 wire 帧/枚举/错误码/emit 网关 DTO/applied 文件形状只在 `internal/protocol` 定义一次；`internal/server`/`internal/ws`/`cmd/anode` 都 import 它。包不得 import 任何本项目包。不提供旧帧 alias。
- **D-I2 `config.get`**：文档只给帧名。请求 `{appliedRevision,appliedHash}`；中枢用 `welcome` 帧回答（同形）。`config.changed` 只带 `{revision}`。
- **D-I3 hello**：节点上报 `localVars/localLimits/namespaces(本机申报)/appliedRevision/appliedHash`；中枢算 `machineVars = local ⊕ node 默认`，节点快照三分（localVars/nodeVars/machineVars）；ns 默认值不落 applied、不上报。`envPassthrough` 名单与值都不上报。
- **D-I4 applied 文件**：`<dataDir>/anode.applied.yaml`，0600、临时文件+rename 原子写，内容 `{revision, hash, vars, limits}`（机器级合并结果）。
- **D-I5 playbooks 落库**：`playbooks(id,namespace_id,slug,name,descr,version,files,source,created_at,updated_at)`；`files` = canonical JSON `map[path]content`（含 `playbook.yaml` 原文）；新增 `playbook_installs(id,namespace_id,playbook_id,values,values_digest,receipt,source,created_at)`；`capability_versions/sources` 各加 `installed_from`（首次来源，复用不覆盖）。文件数上限 64 含 `playbook.yaml`。顶层冲突码：行 6=`version_content_immutable`，行 10=`playbook_conflict`；`conflicts[]` 只在 400 错误体，不在回执。
- **D-I6 edges 投影**：`edges(namespace_id,from_is_source,from_ref,to_ref)`，无 port/priority/mapping/literals；能力 `next` 与源 `targets` 写入时同事务整体重建；无写 Edge API。源 targets 元素 `{capability, params}`。
- **D-I7 删除的 v3 结构**：`node_pins`/sticky、`graph_versions`、`artifacts` 镜像表、`task_logs`（日志在节点）、`nodes.namespace_id`、`capability_versions.outputs/sticky/handler/content_hash` 旧语义、`edges.literals/mapping/priority`、`sources.kind=github`、`executor`/notify-phone 中枢能力。保留：auth/passkey/session/apikey/devices/push/通知 API/cli-auth/admin/stats/orphans/upgrade。
- **D-I8 tasks**：一个逻辑 task 一行；列含 `attempt`、`max_attempts`、`node_id/node_name`、`config_revision`、`waiting_reason`、`next_attempt_at`、`lease_id`、`lease_until`、`attempt_token_hash`；attempt 只在 `lease.grant` 生效时 +1；`stale_config` 不计 attempt；取消等 `processGroupReaped` 才释放槽位。
- **D-I9 emits 幂等**：`emits(task_id, emit_id NULL, to_ref, data, result, created_at)`；`UNIQUE(task_id,emit_id) WHERE emit_id IS NOT NULL`；同 id 同请求回旧结果，异 to/data → 409 `emit_id_conflict`；无 id 每次新派发；跨 task 不合并；不按旧 seq 业务去重。
- **D-I10 依赖零新增**：优先用 stdlib + 现有依赖；`internal/protocol`、`internal/schema`、`internal/artifact` 保持节点可编译（不链 sqlite/server）。
- **D-I11 退避参数**：attempt 间 5s 起、指数 ×2、上限 5m；`stale_config` 回 pending 5s 起、上限 60s。写常量集中一处。
- **D-I12 契约未定义的 `config.get`/`hello` 细节、playbooks 列形状**：若实现中确需偏离 D-I，写入 `IMPLEMENTATION-NOTES.md` 并在 DONE 里说明；跨团队（HTTP/前端可见）的变化必须先报父。
- **D-I13 hello 帧扩展**（G1-1 报告的文档缺口，内部协议裁决）：`capability.md §8.5`/`config.md §5` 的 hello 最小字段之外，同一帧必须携带 `agentVersion/agentCommit`、内置事实 `facts{hostIP,hostname,os,arch,cpu,workRoot,homeDir}`、`capabilities[]`（`requires.commands` 探测结果与可执行能力名）、`instances[]`（槽位/并发/busy）。理由：`NodesAvailable×CommandMissing`、`Node.status`、upgrade 都依赖这些事实；它们仍只走 hello/welcome 同一子协议，不新增帧。字段名 camelCase，加入 `internal/protocol` 单一源。
- **D-I15 集群节点/配置内部形状**（G1-3）：`nodes` 增 `facts/declared_capabilities/declared_namespaces`；`node_configs` 增 `local_vars/local_limits`（hello 快照）；配置 CAS 空行默认 `resourceVersion=1`；`node_configs.namespaces` 显式空列表=谁都不授权、仅 nil 补 `["*"]`；`EligibleNodes` 无候选不是 error、逐节点 reason 汇总；`EdgesFrom` 返回 `[]EdgeTarget{ToRef}`、`ListEdges` 返回完整 Edge。`edges.position` 文档示例与 D-I6 冲突，按 D-I6 不建列（待父回写文档）。
- **D-I14 失败原因码补 `timeout`**：`capability.md §6.2` 封闭表未列，但 `timeoutSec` 到点必须可判别；`internal/protocol` 增加 `ReasonTimeout="timeout"`，任务终态 `failed`、不自动重试。另：`impl_missing` 保留用于「spec 没有 run」（artifact 校验）；G1-6 对齐。

## 3. 阶段与任务序列（串行；上一个任务 verify 通过才派下一个）

每阶段结束更新 `.pi-orchestrator/v4/teams/core-runtime/PROGRESS.md` 并 commit checkpoint。

### G1 领域与存储（闭包：artifact/protocol/store）

| ID | 任务 | 所有权 | verify |
|---|---|---|---|
| G1-1 | ✅ 完成 `61c7629`：`internal/protocol` 新建 + `internal/artifact` v4；verify 两包全绿 181 run | 已完成 | 已完成 |
| G1-2 | schema v4 baseline + migrate 旧库拒绝 + **capability 领域 v4**（`capability.go` 重写：Spec 存储/foldKey/digest/版本分配/next 原地更新；删 `builtin.go`/HandlerSpec/`playbook.go`；`identity.go` 去掉播种调用；`deployment.go`/`graph.go`/`downstreams.go`/`node.go`/`namespace.go`/`capability_links.go` 仅做 artifact v4 类型编译闭包，不写 legacy 桥）+ 通用接线。**旧 in-package 测试保持 HEAD 原样，G1-6 统一迁移；本任务新测试放外部包 `internal/store/v4test/`** | `internal/store/**`（生产文件）+ `internal/protocol/**`；旧 `*_test.go` 不得删改（builtin_test.go 除外，需映射） | `go build ./internal/store/` 且 `go test ./internal/store/v4test/ -count=1` 且 `go test ./internal/protocol/... -count=1` |
| G1-3 | store 命名空间/集群 Node/三层配置与双 revision/边投影（`namespace.go`、`node.go`、`graph.go`/`ref.go`/`downstreams.go`/`edge_*.go`、`config` 新文件 + 对应测试） | 上列 | `go build ./internal/store/` 且 `go test ./internal/store/ -run 'TestNamespace|TestNode|TestConfig|TestEdge|TestRef|TestGraph' -count=1` |
| G1-4 | store task/attempt/lease/emits/事件去重/source/trigger 事实/retention GC/deployment/stats（`task.go`、`emission.go`、`event*.go`、`source.go`、`retention.go`、`deployment.go`、`stats.go`、`cron.go`、`downstreams.go` 等 + 测试） | 上列 | `go build ./internal/store/` 且 `go test ./internal/store/ -run 'TestTask|TestEmission|TestEvent|TestSource|TestRetention|TestDeploy|TestStats|TestCron' -count=1` |
| G1-5 | store playbook 目录包/render/12 格/回执/installs 折叠/installed_from + `playbook_installs`（`playbook.go` + 测试） | `internal/store/playbook.go`、`internal/store/playbook_test.go` | `go build ./internal/store/` 且 `go test ./internal/store/ -run 'TestPlaybook' -count=1` |
| G1-6 | G1 收口：清剩余 store 测试编译/语义、认证/passkey/设备回归保留、`TEST-MAPPING.md`、`EXPORTED-INTERFACES.md`（store/artifact/protocol 实际签名） | 余下 `internal/store/**` 测试与文档 | `go test ./internal/artifact/... ./internal/protocol/... ./internal/store/... -count=1` |

### G2 执行与节点（闭包：route/ws/lease/source/trigger/anode）

| ID | 任务 | 所有权 | verify |
|---|---|---|---|
| G2-1 | route v4：dispatch（next 投影、无 port/priority）、accept/reject/grant 编排、emit HTTP→中枢幂等、prune/params/kind、node/source 解析；删 ports/sticky | `internal/route/**` | `go build ./internal/route/` 且 `go test ./internal/route/ -run 'TestDispatch|TestEmit|TestPrune|TestParams|TestKind' -count=1` |
| G2-2 | ws v4：改用 `internal/protocol`、hello/welcome/config 5 帧、10 执行帧、lease renew/ack、cancel/reap、旧 attempt 拒绝、hub/session | `internal/ws/**` | `go build ./internal/ws/` 且 `go test ./internal/ws/ -run 'TestNode|TestProtocol|TestLease|TestCancel' -count=1` |
| G2-3 | lease/source/trigger/logging：租约 60/20/5、sweeper 与槽位回收、源三种 kind/targets/去重窗、cron、retention sweeper | `internal/lease/**`、`internal/source/**`、`internal/trigger/**`、`internal/logging/**` | `go test ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... -count=1` |
| G2-4 | `cmd/anode`：`git mv` 改名、`anode.yaml`（三层/envPassthrough/listen）、serve/install/emit 三子命令、runner 干净环境/PATH/HOME/files_escape/托管产物、emit 网关、runstore、upgrade；删 `cmd/agent`、`internal/executor`；Dockerfile.agent/install 脚本改名 | `cmd/anode/**`、删除 `cmd/agent/**`、删除 `internal/executor/**`、`Dockerfile.agent`、`scripts/install-agent.sh`→`install-anode.sh`、`scripts/check_agent_skill.py`→`check_anode_skill.py`（含配套） | `go build ./cmd/anode/` 且 `go test ./cmd/anode/ -count=1` |
| G2-5 | G2 收口：ws/anode 全测、route 全测、EXPORTED-INTERFACES 更新 | 上述包 | `go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./cmd/anode/... -count=1` |

### G3 HTTP 与系统装配（闭包：server/api + make contract）

| ID | 任务 | 所有权 | verify |
|---|---|---|---|
| G3-1 | mux/中间件/信封 DTO/Status 错误体/CAS/warnings/ns 路径骨架；删旧扁平路由与写 Edge/downstreams/deployments 端点 | `internal/server/{mux,middleware,adapter,config,backpressure,embed,static}.go` 等 | `go build ./internal/server/` |
| G3-2 | capability/source/playbook/deploy/task/event/trace/edge/deployment 资源 handler + 派生 status（resolved/observed/conditions） | `internal/server/{capability,source,playbook,deploy,console,emission,task_logs,namespace}_*.go`、`internal/api/**` | `go build ./internal/server/ && go test ./internal/server/ -run 'TestCapability|TestSource|TestPlaybook|TestDeploy|TestStatus|TestEvent|TestTask' -count=1` |
| G3-3 | Node/Namespace config、system/config、nodes cluster 化、upgrade、hooks/ingress、auth/device/push 保留域接线 | `internal/server/{node*,namespace*,admin,auth,cli_auth,passkey_enroll}*.go` | `go build ./internal/server/ && go test ./internal/server/ -run 'TestNode|TestNamespace|TestConfig|TestAuth|TestPasskey|TestDevice|TestCLIAuth' -count=1` |
| G3-4 | `api/openapi.yaml` 重写 + `scripts/check_openapi.py`/新增 `check_envelope.py` + parity 测试（禁词、reason 封闭、四键/items/warnings） | `api/**`、`internal/server/openapi_parity_test.go`、`scripts/check_*.py` | `make contract` |
| G3-5 | G3 收口：server/api 全测、保留域回归、EXPORTED-INTERFACES/测试映射更新 | 上述包 | `go test ./internal/server/... ./internal/api/... -count=1` 且 `make contract` |

### G4 工具链与整体验证

| ID | 任务 | 所有权 | verify |
|---|---|---|---|
| G4-1 | `cmd/devseed` v4、e2e harness/WS fixtures/T1 套件（live_agent/ws_protocol/deploy/contract 等）改 v4、Makefile 目标与命名、compose/Dockerfile、构建/安装/静态契约说明、根 README/DEVELOPMENT/CHANGELOG 相关章节 | `cmd/devseed/**`、`scripts/**`（T2 除外）、`Makefile`、`Dockerfile*`、`docker-compose*.yml`、根文档、`.gitignore` | `go build ./...`（允许 sitegen/fe-preview 既有产物） |
| G4-2 | 全量门禁：`go test ./... -count=1`、`make contract`、`make e2e-quick`；补 `TEST-MAPPING.md` 终稿、交付报告 | 全部 T1 文件 | 上述三条 |

> T2 并行的 `make fe`/sitegen/前端单测不在 T1 门禁内；若全 Go 门禁因 sitegen 未集成**确属**阻塞，报告父会话最小依赖，不越界、不假绿。

## 4. 接口冻结（跨包调用面）

- **协议**：唯一源 `internal/protocol`；帧名/字段严格按 `capability.md §8.5` 与 `config.md §5`。`internal/ws` 只做传输/会话状态机，路由与权威判定在 route/store。
- **store**：包内方法签名由 G1 实际实现冻结进 `EXPORTED-INTERFACES.md`；后续 route/ws/server 只能按该文件调用，不得各自发明第二套公共类型。
- **route**：对外暴露 `Dispatcher`（派发计划/提交）与 `Emit`（幂等派发）两组 API；节点侧不 import route。
- **anode**：不 import `internal/store`/`internal/server`/sqlite；只 import `internal/protocol`、`internal/artifact`、`internal/schema`、`internal/version`。
- **HTTP**：路径/信封/错误码以 `api-conventions.md` 为唯一表面；`resourceVersion` 只做 CAS，`metadata.version` 只做内容版本，`configRevision`/`namespaceVarsRevision` 只做配置版本。
- **status**：只读派生，不落库、无 reconciler/generation/lastTransitionTime。

## 5. 测试与防伪绿灯

- 只跑本阶段闭包 + 本任务 verify；不每轮跑全量 E2E。G4-2 才跑全量。
- 退役规则：仅纯 v3 语义（ports/priority/mapping/literals/sticky、handler/executor、旧迁移、旧 assign/token/seq emit、旧扁平路由、旧 9 态）专属测试可删；每条删/改登记 `TEST-MAPPING.md`。auth/passkey/device/push/notify 回归必须保留。
- 测试节奏（rev2）：G1-2..G1-5 **生产代码优先**，旧 in-package 测试保持 HEAD 原样（不可编译是已知中间态）；每个任务的新 v4 测试放外部包（store: `internal/store/v4test/`；route 等按需）或各自包内（若该包测试可编译）。G1-6 统一做 store 测试闭包（修/重写/映射），G2-5/G3-5 各自包闭包。**禁止用批量删除测试来换编译绿灯。**
- 新增测试必测（按任务落位）：foldKey/digest 边界（next/displayName 不出版本）、显式同版不可变、字段往返、ns 授权+三层 vars+CAS、12 格 install+dryRun+targets 差异+token 省略脱敏、事件 id 窗/重复/异 payload、emit 同 id/异 id/异 to、stale_config 不计 attempt、grant 唯一授权、旧 attempt 拒绝、cancel 等 reap、非幂等不重跑、干净环境/凭证白名单、files_escape/GC 不碰 workDir、raw 包体/实例溯源/派生 status。
- 任何 `skip`/弱化断言/legacy 兜底 = rejected。全仓测试不过但有明确阻塞点时如实标 draft。

## 6. 上报与升级

- 阶段 checkpoint：`PROGRESS.md` 记录完成/未完成/下一步/阻塞；每任务一次 commit。
- 契约矛盾/需要父裁决：`legion_mail` 给父（附文件:行号、证据、建议），继续做不受影响的部分。
- 收队：全 G 门禁绿后交分支名 + HEAD sha + 范围/测试映射/verify 输出/遗留风险；父 fresh review 与跨队全量 E2E 通过才算完成。团队不 merge。
