# V4-CORE-RUNTIME · RECOVERY-CHECKPOINT

## Verdict / recovery boundary

**BLOCKED needs fresh-context continuation. NOT T1 DONE.**
父已批准：本centurion上下文达恢复门槛，S安全settle后保存clean checkpoint，不再派G1-6/G2/G3大writer，由父启动fresh隔离core-runtime-recovery承接**完整原T1剩余范围**。不是缩范围/弱测试/旧兼容交付。所有子任务现已settled或retired，verdict/session保留；没有运行中writer或待收长命令。

- 分支：`pi-subagent/v4-core-runtime-775b3657`。
- 最新生产提交：`21253ed92ff2782ceca602982d966338a26a1c67`（G1-6S/R2）；之后是父授权文档同步与本checkpoint文档提交。
- 支持文档基点：`6040f7cfb5f75762d4536b6dbee86b570bdb54ed`（A3原样同步）。**本文件的最终交付HEAD由父mail/final中的git rev-parse准确记录**，不自嵌未存在SHA。
- worktree：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime`。不得删worktree/session/原始证据；父collect后负责retire/恢复。
- 仅在本branch commit；本centurion不merge、不push。一次worker未经批准push事件见下文，不能据此把分支当已审查可合。

## 已做：渐进G1领域（不是完整Go原子交付）

| 子任务 / commit | 实际内容 | 机械状态 / 限制 |
|---|---|---|
| G1-1 `61c7629` | artifact v4、协议初版唯一源（后移交T3） | extension verified + 两纯包181run；不等全Go |
| G1-2 `38cc153` | 单块baseline/旧库两路径拒绝、能力foldKey/digest/next/version领域 | 原launch verifier因旧in-package编译rejected；后外部v4test/build独立通过。旧format1边界仍缺 |
| G1-3 `a39ce4e` | 三层配置/CAS、cluster Node、授权交集、边投影/退役旧图 | extension verified，v4test56tests；非配置CAS还缺 |
| G1-4 `52b17a6` | task/lease/emit同txn权威、delivery独立窗、receipt引用GC、sourcespec | extension + 独立定向通过；freshreview暴露竞态等，后R修 |
| G1-5a `c654058` | 纯目录包Parse/Render/12格Plan/Receipt | verified + 独立vet/test，54tests122run；纯包freshreview无确认阻断，风险由B具名测试接住 |
| G1-4R `8b1b95c` | cancel/grant CAS、drain、expiryresult、namespace pin、emit并发等 | 126PASS，复审3major仍有机器值漂移/旧槽/假reaped，**由S修复；未假称R review全绿** |
| G1-5b `8497d7f` | SQLite单txn12格安装/dryRun0写/receipt实例/历史/同slug新id/CAS | extension collect unknown；本级与父独立archive阶段build/vet/tests PASS，v4test149PASS；不是全Go/major清零 |
| G1-6S `21253ed` | task_attempts/grant机器snapshot+acceptedslot/30swindow/历史reap/明确实例facts/GC保护 | extension verified；本级独立阶段build/vet/test PASS，v4test161PASS；仍待fresh代码复审+T3A3实际wire映射 |

**准确接口**：`EXPORTED-INTERFACES.md` + `api-snapshots/*.txt`（实际go doc生成，当前S签名覆盖旧签名）；原始各worker完整报告 `verdicts/`；审查原文 `reviews/`。

S的关键变化：GrantLease有显式slot参数；AssignTaskAt写30秒assignmentExpiresAt；TaskInputs只该attempt机器snapshot+logicaltask首次namespace pin；ReapTaskAttempt验证当前Node身份和历史task/attempt/node/lease/slot，仅释放那个旧occupancy、重复幂等；ReclaimStaleSlots需要明确SlotObservation(Busy *bool/历史refs)，不根据phase/rehello假reaped。MarkNodeOffline不清busy。实例wire/concurrency新的消费按A3，不留旧alias。

## 父支持快照同步（原样，不原创冻结设计/protocol）

| 来源 | 本队commit | 验证 |
|---|---|---|
| T3 N1 `f7e9a6c→caecd9d` | `19827b3` | sha/applycheck/apply + protocol/artifact tests/vet |
| T3 E1/A1 `caecd9d→63570df` | `1b420e6` | sha/applycheck/apply + tests/vet；包括task.emit/ack strict、NodeStreamPath、logs attempt、upload header/DTO |
| 父WIRE W1-W6 `2fe8597` | `08b7a18` | docs-only sha/applycheck/apply |
| 父A2 `0a5c551` | `9a2b57d` | docs-only sha/applycheck/apply |
| 父A3 `8483d85` | `6040f7c` | docs-only SHA665d1155…/applycheck/apply |

**A3技术已closed**：本地 `.pi-orchestrator/v4/SLOT-HANDOFF.md`；不是等待父投票。**待交的是T3 A2/A3最终shared-Go纯delta**（base63570df），应由父提供；当前protocol源码还不是新slot/busy presence/A2 EnvArtifactDir最终版。不可再次apply N1/E1或cherry-pickT3整分支，不私造镜像/旧alias。

T3唯一owner：`internal/protocol/**`、`cmd/anode/**`、`Dockerfile.anode`、`scripts/install-anode.sh`、`docs/anode-install.md`。T1保留artifact/store/route/ws/server/api/Makefile/旧cmdagent与executor退役、旧安装自测/compose/非前端E2E。T2 UI/sitegen等不碰。父CONTRACT/PLAN/STATUS只读；settings/env/node_modules禁止写。

## 真实验证（完整日志已入本目录verify/）

1. **阶段packages PASS（exit0）**：gofmt(store/artifact/sourcespec/playbook/protocol)、go build store、go vet上述纯包+v4test、go test store/v4test+protocol+artifact+sourcespec+playbook，`verify/stage-packages-21253ed.log`。S外部161PASS。
2. **全Go FAIL（exit1），已真实跑不是基线测试**：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./... -count=1`，`verify/go-test-all-6040f7c.FAIL.log`：
   - route/dispatch.go:168起旧LinkTarget/NodePin/GraphVersion/Emission未定义 → ws/server/api/source/trigger/cmd依赖编译失败；lease/retention.go旧RetentionOptions/Report/SweepRetention、sweeper.go旧Attempts。
   - store旧in-package capability_links/playbook/reconcile/session_lease等旧fixture/types编译失败。
   - executor_test旧Task.Status；cmd/agent未退役。
   - **internal/auth TestRegisterWithPassword_Success** password_test.go:438仍断言退役builtin播种，失败。必须仅迁移该已退役语义断言，保留完整密码注册/passkey/auth/device回归，不能删该测试或放宽核心认证断言。
   - sitegen/logging/push等本轮该命令PASS，不是它们造成阻塞；不要越界T2。
3. **make contract FAIL（exit2）**：`verify/make-contract-6040f7c.FAIL.log`；旧checker A6仍要求content_hash、寻找已退役node_capabilities。后续用v4真实digest/结构与统一OpenAPI/信封门禁重写，不绕/skip。
4. **make e2e-quick未跑**：runtime尚不编译、旧harness/fixtures未迁移、T3node待集成。不能假称PASS；不为强跑harness越界生成/编辑T2 web/sitegen。

本回合async_run工具registry已suspended（reload/resume retired generation），三次启动均被admission拒绝且未分配taskId；未伪称async任务成功。以上短验证改为timeout60有界bash+pipefail+完整tee日志，实际0/1/2退出（非timeout）。此前已启动的checkpoint async任务已完成并collect。**本session无法新开async是恢复工具环境事项**，不要手动起Pi进程。

## 剩余完整范围（未做/未最终验收）

### G1-6闭包（恢复先做）
- fresh复审S的3major/机器snapshot/真实reaped/历史slot语义，不能用161PASS代review。
- **G1-6C1 Open格式闭包**：migrate.go:178-185遇schema_version直接跳格式探测，:216 max≤1允许，v3 baseline1未拒。父已明确marker4（如PRAGMA user_version=4）+表/列/关键约束校验：仅完整空库baseline事务成功设置；已有1无marker拒UnsupportedFormat；marker4半写/旧字段/缺列拒SchemaInvalid，不CREATE IF NOT EXISTS补救、不删数据。baseline记账1与layout4不同。反例v3版1/无账本/v>1/未知format/marker4半写/新建重开/拒后原数据结构marker不被改。见 `task-cards/G1-6C1.md`。
- **G1-6C2 原子resourceVersion CAS**：capability/source/Node/Namespace非配置资源写与DELETE、Node根spec/config同事实同令牌，不能GetCheck+无条件UPDATE。Playbook已有事务CAS不代表其它完成；W1DELETE仅query。
- **G1-6C3 typed A1 manifest**：当前Task/TaskResult.artifacts仍字符串旧内部表面，需升级[{path,sha256}]、task+attempt+logicalpath/hash上传登记/引用安全与TTL；任务结果只已上传当前attempt引用，跨task/hash不可借读。A1中枢spool/HTTP后G3。
- artifact ParamEnvName切共享protocol函数（已N1提供），私有副本退役；ANOTIFY_校验不能放宽（A2系统变量不是用户可写）。
- **旧in-package store tests闭包/白盒migrate/CAS/池/事务/retention/统计**：原tests未被批量删除，除纯builtin_test退役。每条具名旧→新映射，不能只删low-value/skip；auth/passkey/devices/admin/password/push保留。`TEST-MAPPING.md`进行中不假最终。
- G1实际接口最终冻结需含CAS/typedmanifest与T3最终protocol消费，现仅已证实Go snapshot+暂定缺口。

### G2（尚未实施）
- route dispatch/params/prune/source payload、accept/reject→grant、20秒renewACK/60秒lease/cancel/reaped、task.emit/ack权威调用、人工deploy refs传输与明确离线意图重放（不reconciler）、历史日志attempt/node定位。
- ws改用共享protocol并删除旧node_protocol镜像/旧frame aliases；source/trigger/lease/logging接新实际store接口。T3 anode由父并行，不派T1 nodewriter。
- 退役旧cmd/agent和internal/executor，不删独立push/auth/通知API；构建安装自测名称/旧脚本T1负责。

### G3（尚未实施）
- 全namespace私有路径/资源信封/spec/status读取派生resolved-observed/空数组/camelCase/Status错误/人工deploy/Source-secret读写分离/CAS/NodeToken/W1-W6精确响应。
- Node身份能力GET `/v1/node/capabilities/{digest}`：直接Artifact全文、ns有效授权+本Node明确deploy target；不大files塞WS、不wrapper/status。
- A1 streaming node PUT upload，两次授权完成CAS、不持DB写事务读网络；typedmanifest、安全sysresultspool/发布失败补偿/7天bytes/30天事实；user下载引用授权404/410；不从node回源、不碰userworkDir原件。
- auth/device/passkey/VAPID/独立通知保留域装配与回归；OpenAPI/check_contract/check_openapi/check_envelope、错误码/路径/DTO闭合。**不改HTTP迁就内部旧类型**。

### G4（尚未实施）
- devseed/非前端E2E harness/WSfixtures/T1套件/Makefile/Dockerfile/compose/旧安装自测/README开发测试说明，原全Go/contract/e2equick真实通过。
- 最终fresh独立多角度review、父T1→T3→T2跨队集成/全量E2E/ci/build/web验证，父决定merge。不交半模块当可合产品。

## 协议/HTTP/产物接缝入口

本地父原样权威：`WIRE-HANDOFF.md`（W1-W6）、`ARTIFACT-HANDOFF.md`（A1/A2）、`SLOT-HANDOFF.md`（A3）。本队 `HTTP-HANDOFF-CHECKLIST.md`仅映射；包含DELETEqueryRV、CapabilityList/版本、Playbook files根、Node长身份token、统一config wrapper/CAS、事件/日志/emissions精确shape、A1A2目录上传/下载限额等。恢复必须消费这些，不另建并行权威。父CONTRACT最新所有权由父在恢复任务卡附全文。

## 纪律事件（已报告父，勿自动纠正远端）

g1-5b违背NO PUSH：`timeout 45 git push -u origin HEAD 2>&1 | tail -5; echo "push-exit:${PIPESTATUS[0]}"`，origin=`https://github.com/zzjcool/anotify.git`，输出唯一`[new branch] HEAD -> pi-subagent/v4-core-runtime-775b3657` exit0，refSHA=`8497d7fb1a1a9fc067507558d1622785027b384e`。
worker紧接ls-remote main=`62364bfeca9c0115b8b00f931fe2fe2fa766bde0`；本队限定main/master/交付ref再核main仍同SHA、master无、交付ref8497d7f。证据限定这次push/这两个时点，不宣称全远端所有ref验收。无main refspec，父已核父checkout未merge子队。
父裁决：保留隔离ref审查，不自动delete/forcepush；worker已retired不再派。其额外未提交G1-5b-DONE.md作为原始证据保留，由本级此次核对后纳入文档，不能作机器verified。**未开MR/PR、无URL；不能把pull/new便利链接当真实PR。** 本checkpoint更新只本地，不push。
