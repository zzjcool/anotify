# V4 全量实施状态

## 请求与边界

用户授权：按已冻结的 v4 设计安排分工并全量改造；开发阶段，无历史兼容/旧数据迁移包袱。
设计基线：`docs/design/v4/`，批准决定 `decisions.md` V4-D1～D6，文档提交 `368b489`。
当前集成分支：`feat/p9-capability-v4`。
保护：`.pi/settings.json` 是用户已有未提交改动，所有团队不得修改或纳入提交；不得修改 `.env`、`.git`、`node_modules`。

## 交付纪律

1. 多路只读 scout 摸清现状与测试入口；不运行基线 build/test。
2. 父会话据事实冻结接口、文件所有权、集成顺序和机械验收。
3. 独立 centurion 团队在隔离 worktree 实施，团队成员不另开 worktree；所有 writer 有父级指定 verify。
4. 共享文件单一所有者，冻结文档不得被实现迁就；遇到契约矛盾先上报父会话。
5. fresh-context 对抗式 review 是集成门禁；重大问题返工。父会话独立跑相关测试、全量 E2E、前端逐页验证及最终构建。
6. 不保留 v3 资源/端口/边写接口或旧数据库升级兼容层；认证、设备通知等未被设计退役的功能不得误删。
7. 不自动推送远端；只有父会话做最终集成。

## 当前工作

| 团队/任务 | 范围 | 状态 |
|---|---|---|
| v4-plan-backend | Go/store/route/API/source/playbook/config 差距与所有权 | 只读侦察完成 |
| v4-plan-node | 节点执行/emit/租约/协议/安装命名 | 只读侦察完成 |
| v4-plan-frontend-tests | 静态前端/API/OpenAPI/测试工具链 | 只读侦察完成 |
| v4-plan-second-opinion | 仅分工第二意见 | 完成；采纳Go单一原子团队+独立前端，不采纳削减E2E或核心范围建议；其报告分支/MR不纳入产品改动 |
| T1 core-runtime (`v4-core-runtime`) | 全Go核心+协议/API+工具链，G1→G2→G3→G4 | 已派隔离centurion，四阶段状态机planning→implementing→reviewing→verifying→done；父指定go test全量+contract+e2equick门禁 |
| T2 frontend (`v4-frontend-recovery`) | 静态前端+sitegen+前端测试，复用F1接续F2→F3 | 原v4-frontend因checkpoint final提前terminal未交付；新隔离centurion已启动恢复，model显式gpt-6.1-sol，父make fe+sitegen+Node三门禁不变 |

已冻结 `PLAN.md` 与 `CONTRACT.md`（含文件所有权、跨团队HTTP/协议/模型接口、验收命令）。AGENTS/DEVELOPMENT已明确用户批准v4baseline例外。

分工契约提交：`0e0ff0f`。两个团队已启动；各工作树同时只一个writer，root不抢写。阶段进度在各teams目录或subagent通知中收集；若预算不足要求明确blocked和可恢复checkpoint，不允许裁减核心范围虚报DONE。

规划超时处理：T1 planning超过10分钟提示已处理。子团队回执确认legion_phase成功进入implementing，契约 `.pi-legion/contract.md`，首writer g1-1已提交 `61c7629 feat(protocol): v4 协议唯一源与能力产物规格`（只在团队分支），零依赖internal/protocol+artifact，正在阶段verify/收尾；无阻塞，后续串行schema→模型配置→task/source/GC→playbook→G1闭包接口清单。预计旧依赖暂编译失败按阶段记录，不compat/stub、不合父。

T2 planning超时处理：父只读其session最新8条确认已经实施，F1a在团队分支提交e4f6bd0（partials.js/API模型+新JS测试，30/30测试PASS），review-f1a指出scope初始化组合断裂与created回执object被当array等major，团队安排在f1b后返修；f1b rewrite mock仍有实际进度。centurion当前长wait阻止steer及时落地，父用subagent abort仅中断其当前wait（context/子worker保留），continue要求立即planning→implementing并简报phase/提交/当前writer/返修顺序。无团队退役/无重启writer/无丢弃代码。建议scope用显式async helper而非假namespace sentinel，回执按冻结object形状修。阶段状态回执待收集，尚未验收F1。

L2提醒处理（notice15:45，核查时16:19）：两团队存活且session有实施证据，不因历史30分钟提醒停止/重启。T1已到G1-2重建schema/migrate，存在真实diff和已知旧依赖编译失败；要求组长明确扩改边界，禁legacy/stub桥与长时间静默。T2回执确认phase已为implementing、F1a已提交/30testsPASS但2major待修、F1b mock重写收尾；按父意见使用async nsApi并修created对象。已分别extend仅L2提醒30分钟，不改变阶段deadline/verify/进程；继续checkpoint观察。

T1最新checkpoint（仅团队分支，未集成）：G1-2 commit `38cc153`，单块v4baseline/旧库拒绝/foldKey→version→digest/显式版内容不可变/next原地更新/hello字段已实现；团队自测与centurion复核报告production build/protocol/外部`internal/store/v4test` 35用例PASS。旧in-package测试未删未弱化，暂无法编译，留G1-6统一v4闭包，不视为全量绿。旧playbook.go整文件删除且明确G1-5重写，未用假桥；builtin专属删除已映射。扩改所有权记录完成，具体表名与文档存储布局草案差异仍需EXPORTED-INTERFACES映射。G1-3最新checkpoint：config.go三层vars来源+机器limits递归合并/ns授权交集/node与ns双revision/3套CAS和batch已实现；node.go集群化、槽位、EligibleNodes六类reason；graph投影替换、旧pin/sticky/port/priority文件移除。团队报告store production build PASS，新增config v4test 8用例通过（优先级/缺值/limits/ns和node及system CAS/batch/往返）；node/edge/namespace用例仍写，尚未G1-3 DONE，旧in-package测试仍G1-6闭包。后续G1-4 task/租约/emit/source/GC→G1-5playbook→G1-6全store测试。无产品契约阻塞。

16:50 L2检查：父status/session+两worktree只读git记录确认实际进展。T1 G1-3新增config.go与schema真实diff；T2新增commits `73670dc`（v4mock+图投影）、`9c6d15a`（scope组合入口与回执模型修复）。最新checkpoint：团队报告F1 Node测试52/52、make fe/TestVocabulary/sitegen PASS，复审major已修，minor记录；父尚未最终独立验收。F2a capabilities页已重写（6611→2188行，版本/编辑器/status/deploy/CAS/409）+4locale+mock测试，正在修缺i18n key并跑verify/浏览器，未DONE/未提交。F2b/F2c/F3任务卡与接口要点已落teams/frontend/TASKS.md，后续不重复主组长探索。两个L2提醒再延30分钟，仅提醒不改截止/verify、不重启；要求下一checkpoint报告行为与测试而非笼统读文件。frontend组长上下文较大，已要求文件化任务卡/接口/checkpoint，减少重复探索。

16:59 T2 implementing阶段90分钟提示：父检查worktree确认capabilities页/4locale/mock/tests真实diff，session最新等待F2a收尾，决定继续当前实施，不abort/不丢上下文，不把提醒当失败。阶段时间是advisory，未声称修改phase期限，也不提前跳review/虚报完成；已steer要求下个checkpoint提供F2a具体失败/命令/验收和剩余页面范围，按原F2→F3推进。没有减少功能/skip测试。

17:08 T2恢复事件：原centurion把working进度写final，harness自动terminal（acceptance unknown），continue CHILD_TERMINAL，doctor确认不能恢复；不是T2完成。父已只读snapshot旧worktree从0e0ff0f起全部T2变更及untracked TASKS/preview到 `/tmp/anotify-v4-frontend-recovery/`（patch+tar+SHA256SUMS），未修改旧worktree/未丢代码。启动 `v4-frontend-recovery` 新隔离worktree按snapshot接F2a验证和后续F2b/c/F3，禁止新team写旧worktree，继续单writer/原verify/功能范围。要求checkpoint只mail/file、不能final等子任务；同提醒已发T1。新team已回执SHA256两包、git apply --check/apply/tar均完成，未改旧worktree，复用F1按TASKS接F2a；本地contract已冻结，状态机已planning→implementing；恢复成果checkpoint提交 `aad4b15`（包括F1+尚未验收F2a，非完成提交）。唯一writer `f2a-close` 正在能力页/4locale/mock/tests闭包，阶段verify固定make fe/Node/sitegen/Vocabulary+Chrome四视口交互/CAS保输入/版本部署诊断；两个只读scout先取后续wire与测试接缝，不重扫全仓。父确认无reviewer角色时fresh scout三角度只读review可用，但必须结构化ok/findings且major返工，不把recon完成当verdict；状态机verdict_ok是否接受须以真实工具receipt为准，若有role限制报告不得绕过。旧STATUS的DELETE CAS/token假设非权威，按冻结API校准。

17:22 T1 L2核查：a39ce4e G1-3和ba21a89 checkpoint已提交；G1-4 task/emission/sourcespec有真实diff，未卡死。最新组长记录有因预计session时间而拟压测试/改阶段顺序倾向，父已禁止减验收/假DONE；仅abort当前长wait保留context和g1-4writer，continue及时传达策略。可讨论旧测试机械闭包延G4但必须新行为测试完整+实际接口冻结，最终原门禁不变；尚未批准改顺序。父派只读v4-node-split-readiness评估已稳定protocol/artifact支撑T3独立anode并行，暂让T1不启动anodewriter，等明确所有权修订；不是新增产品scope。

17:22提速编排rev2：只读v4-node-split-readiness核实T1protocol仅stdlib、artifact仅stdlib+yaml、两目录干净；已可脱离store闭包编译节点。父修订CONTRACT/PLAN：T1→T3交protocol唯一所有权及新增cmd/anode/Dockerfile.anode/install-anode/docs，T1继续server/store/route/Makefile与旧agent退役。T3先补环境常量/参数归一、机器配置共享hash、人工部署投递与node身份artifactGET接缝，不造控制器。父已capture纯依赖补丁 `/tmp/anotify-v4-node-foundation/foundation.patch`+SHA；具体T1修订卡 `/tmp/anotify-v4-core-protocol-carveout.md`。拆分是提速不减范围/测试，最终集成T1→T3→T2；T1 implementing120分钟advisory仍继续真实G1-4。

尚未宣称任何 v4 能力完成；父最终review/跨团队集成门禁未开始。
