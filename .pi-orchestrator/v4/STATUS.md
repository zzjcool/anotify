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
| T1 core-runtime (`v4-core-runtime`) | 核心Go/store/调度/server/API+工具链，G1→G2→G3→G4 | implementing；rev2将protocol/anode交T3，消费冻结快照；父go test全量+contract+e2equick门禁不变 |
| T2 frontend (`v4-frontend-recovery`) | 静态前端+sitegen+前端测试，复用F1接续F2→F3 | 原v4-frontend因checkpoint final提前terminal未交付；新隔离centurion已启动恢复，model显式gpt-6.1-sol，父make fe+sitegen+Node三门禁不变 |
| T3 anode (`v4-anode`) | 纯共享protocol+节点本机执行/emit/config/GC/install，N1→N2→N3 | 新隔离centurion已派gpt-6.1-sol；先N1协议接缝/handoff；父protocol/artifact/anode测试+vet+diffcheck门禁 |

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

HTTP wire补充（T2 action-needed）：父派v4-wire-doc-check/node-check确认DELETE CAS承载/版本list kind/Playbook裸body与信封冲突/裸冲突Status/Node token与config响应/source预览和诊断读取未定义。父已定 W1-W6于 `WIRE-HANDOFF.md`（不改变D1-D6），同步T1/T2/T3；唯一docworker v4-wire-docs-align在父checkout精准更新5份对应设计正文，团队仍不得改design。T2F2b/c可按新wire继续；旧T1EXPORTED-INTERFACES未交不是猜旧wire的依据。N1T3已进入implementing，支持commit f7e9a6c，anode-n1单writer运行。

T2wait解析问题：父doctor+只读元数据证实f3-test-facts已retired（17:26:52）而execution仍旧running快照；holder runId为r-0156e040、自身r-6e8fb3fb不同，storeRoot在/root/code/anotify。已给T2准确holder/cwd做必要一次status核对，不再盲wait/continue，不越级操作孙任务。不让提前只读F3scout退役阻塞F2a/c；事实有限恢复，缺口实际F3再派fresh。详见RUNTIME-ISSUES.md，尚不宣称确认具体harness退役根因。

T2 F2a浏览器初轮未全过：12项中创建/部署/删除/语言locator超时，版本/诊断部分PASS，已有真实日志与截图；不得宣称F2a绿。父要求区分产品bug与定位器/ready同步问题，保持核心点击最终状态断言，不删断言/假fixture；最多2次盲重试不限制有证据的修复后验证。再次通知T2完整WIRE-HANDOFF已可绝对读取（Playbook/Node/source/actions均已裁决），不因分支尚无正文commit空等/造猜测mock。docworker正文同步进行中。

T3 emit WS缺口已父批准：task.emit/task.emit.ack共享传输（详/tmp/anotify-v4-emit-ws-approval.md与CONTRACT§7），复用HTTP DTO/错误而非资源Status；requestId仅关联、业务id仍task级；中枢验权+id判等+接受/派发同事务，网关有界等待/未知结果CLI5不自动重发无id。已同步T1/T3，N1同次或第二纯协议commit给唯一最终HANDOFF，不分两套镜像。

T2 F2a真实小闭包已回完整verdict：1b9cd85提交，f2a-close wait accepted(verified)；makefe/Node59/59/sitegen/Vocabulary+Chrome4视口failed0/12真实交互PASS、consoleErrors[]，包含全字段创建/版本/CAS409保输入/immutable400/人工部署v1节点stale/DELETE409再确认/prod缺失/4lang。原FAIL为证据脚本定位/模态框/持久scope时序，准确修复不删覆盖，失败快照保留。scope已按W1-W6，不再等待wire；恢复分支最新head `ca450e7`，F2a证据提交完。已派唯一writer `f2b1-source`（gpt-6.1-sol）实现sources/source两页/对应4locale/mock sourceactions/测试/CSS/截图，验收makefe/Node/sitegen/Vocabulary+两页四视口真实表单CAS/HMAC脱敏GET/fire/零写preview/pruned。后续串行F2b2总览/事件trace/message→F2c包/node→F3，范围不减。父仅记录阶段verified非最终整队验收。WIRE正文两路fresh复审已过W1-W3/W5W6/安全，但config旧hello'注册'一句major+minor收紧正由v4-wire-last-fixes小writer精准修复；不重审全量/不改D1-D6。

T1最新G1-4 checkpoint：task/emission/event/source/retention/deployment/stats/orphan已重写，纯sourcespec与对应外部行为测试落盘；stats用例找到taskRefCounts漏namespace参数，已修且定向PASS，当前sourcespec独立测试中；整体G1-4未交verdict。确认rev2protocol/anode所有权归T3，T1不再派nodewriter、不cherry-pick整个他队；后续G1-5纯包/render+store安装→G1-6新行为/旧测试闭包/接口→G2serverruntime（等N1纯快照）→G3HTTP→G4原全Go/contract/e2equick门禁。已明确不降低测试深度，当前无阻塞。

T1 G1-4内部GC任务卡错误已父裁决修复：playbook_installs不能随liveplaybook FK级联删；保留原包id/slug快照，installed_from回执实际引用仍保护，重建同slug不可混旧来源。自动event/task/result sweeper不清安装回执；独立显式cutoff内部primitive只在引用安全事务下可用，不加默认后台/API。delivery去重独立ns+稳定sourceID+id时间窗索引，与延长event/trace实体留存分开；expired id可原子新event且不被events永久UNIQUE挡住，窗口内旧回执可读，GC事件组按task引用安全。要求fakeclock/并发/删包/同slug重建/receipt过30天/旧event引用但dedup过窗等测试。错误来自团队内部卡，不改父D3/D6。

N1交接：T3 caecd9d（支持f7e9a6c）已冻结9文件protocol补丁+f463189交接文档；父copy不可变 `/tmp/anotify-v4-n1-handoff/`、cmp commitdiff一致、SHA记录，隔离archive applycheck/apply+protocol/artifact tests+vet独立PASS。已发T1在单writer安全边界消费，新增常量/ParamEnvName/MachineConfigHash/人工deploy两帧。T3mail'E1仍待批'是旧状态，父已再次明确批准task.emit/ack、补base caecd9d第二纯delta和最终唯一HANDOFF；不等完整节点。父async_run工具本代registry suspended未启动任务，短独立检查改为有界timeout60 bash完成，不改runtime/不放宽验收。

T3N2传输小裁决：标准WS /v1/node/stream（沿已冻路径，不造/node/ws或/agent/ws），server空path补此路径/显式reverseproxy path保留、凭证不query；共享NodeStreamPath由T3增补delta、T1消费。Node GETcapabilities/digest直接Artifact JSON全files、不额外wrapper/status，Node ns+明确部署target鉴权与客户端digest/refs验证。已同步T1/T3和CONTRACT，不阻塞strictconfig/applied/cache。

WIRE正文worker已DONE（5份文档），主工具acceptance unknown故父不声称机械verified；父自跑原diffcheck+具名定义/14个JSON例子解析，并派v4-wire-final-review/security-review定向复核，不全量重审。T2继续按已下发HANDOFF，Playbook模型仅spec.files不兼容顶层res.files，冲突仅Status.details；正文review后父提交同步。

T2 F2b1 worker缺async工具已诚实报告；centurion托管named preview/长验证并保存日志，保持唯一源码writer。父确认可用，要求ready后暂停受影响写入→makefe真退出0/新产物标记→Chrome，避免生成/证据跑旧页；pipefail真exit/失败快照。Source旧manualfire幂等字段按W6替换，但events/hooks去重、剪枝、preview0写/CAS保输入/HMAC脱敏与PATCH保持secret等行为覆盖不得削弱。当前实现中未PASS。

18:31 T3 L2核查：实际commits N2A `4a8b2f6`（22文件config/applied/cache/WS基础）和E1 `179e999`+`08ef49a`，当前anode-e1-fix正在修Decode unknown字段/ACK互斥接收路径验证；确认有进展，仅延提醒30m。E1未作为最终无缺陷快照转T1，等增量修复后交唯一HANDOFF，然后N2B执行/lease/emit/GC。N2A独立close已补验收，整节点未完成。

WIRE正文最终修复已完成。最后worker的extension验收因父命令中shell双引号内反引号导致unexpectedEOF（验证基础设施引用错误，不是正文不通过）；父已按相同断言用安全heredoc独立运行PASS、14JSON解析PASS、diffcheckPASS。只读config259/273确认hello身份校验非注册、首见只配置行；playbook472-474打码落库明确。两路review原major仅hello旧句已修，minor安全收紧均落实，正文wire补充已提交 `2fe8597`，未包含用户settings。父生成只读doc支持快照 `/tmp/anotify-v4-wire-docs-handoff/docs-only.patch`+SHA（base设计368b489→2fe8597），发3团队在单writer安全边界原样同步；仅5design+WIRE-HANDOFF，不合整父分支、不得自行改design。未跑实现build/e2e。

18:36 T1旧L2核查：已真实新提交52b17a6(task/emit/事件/GC)、19827b3(N1纯协议同步)、4cdea61(纯目录包render/安装计划)，当前g1-5a修复仍有diff，非空转。3fresh checkpointreview发现cancel恢复/RequestCancel竞态、逻辑task ns输入载体、资源CAS原子入口及槽位/过期结果等major，接口清单已落但暂非冻结。父已批准按原语义串行修复：g1-5a→G1-4R→G1-5b→G1-6，drain只停新接单不当失联、取消持久意图不可expiry复活、slot等真实reaped、CAS同事务，ns输入钉住机器换节点重解析；具名竞态/rollback/跨ns测试。仅延L2提醒30m，未改变原验收/范围。

A1运行产物/日志接缝已父批准（ARTIFACT-HANDOFF.md）：logs原帧增attempt/实际attempt，previous找历史node档案；托管bytes用Node出站HTTPPUT到中枢（现身份+attempt授权、流式+完成CAS），不另开node监听/大artifactWS帧；Task实际manifest授权下载ns子路径/7d expired410，user原files不碰。T3shared常量和client、T1spool/引用/GC/handler、T2链接与过期状态已同步。不采纳第二意见把node出站HTTP误当新node公开监听的前提。脚本/runner实际如何登记输出清单仍有事实缺口，已要求T3最小事实再父定，不能永远empty[]假完成/擅加第四CLI/run字段。

T1新checkpoint已见：G1-5a c654058纯render/12格计划测试通过，但完整store install尚未做；当前g1-4r修major竞态/D2输入钉住，W1-W6支持已08b7a18同步，接口清单暂态非最终。

18:51 T2旧L2检查：5d79bbf Source双页闭包已真实提交，26d1a79父wire同步，新唯一writer f2b2-diagnostics做index/events/trace/message四页，mock/helper/test有实际diff，非空转；L2延30m验收不改。团队async registry admission closed与父旧generation工具同类，已明确不能假称父原工具可托管/无限重试/改settings。长验证ready后发精确cwd/cmd/env/sourcecommit/日志/预算，由父安排新managed验证上下文；短独立timeout60可分case保完整断言，不能裸长期bash/tail吞exit。源码阶段继续，不以工具问题虚报PASS/重派并发writer。

18:56 T3旧L2核查：实际867be7e E1严格Decode修复、63570df A1logs attempt/artifact headerDTO已提交，N2B clock/execenv/resultstore新目录实施中，不卡死，不因旧通知重启。父capture纯protocol **caecd9d→63570df** 增量 `/tmp/anotify-v4-e1-a1-handoff/protocol-delta.patch`（SHA61498bf4…），干净协议目录，隔离archive applycheck/apply+protocol/artifact test+vet独立PASS；fresh短review v4-e1-a1-handoff-review **4/4 PASS/无major**，父现已转T1在G1-4R单writer安全边界消费caecd9d→63570df增量；不得再apply旧N1或消费179e999缺strict校验版本。包括E1 strictDecode/Validate和NodeStreamPath/A1attempt-headerDTO，legacyws镜像待T1G2退役。N2B生产产物登记入口仍待最小事实，scope不能以空数组省略。

19:05复核T1旧18:33 L2：当前G1-4R task/node/orphan/schema有真实修复、新task_inputs_test落盘，未DONE/未宣称测试完成，组长等唯一writer；同旧notice早于18:36已处理不重复延期/重启。父要求下一checkpoint按取消竞态/ns输入/drain/槽位/过期结果具名测试报PASS/FAIL，不以diff/读文件量代验收；组长context大，任务卡/接口已落盘，避免SOP工具探索分心，G1-4R收口后真实G1-5b安装事务继续。

A2产物生产入口正式父裁决：不接受固定userworkDir/.anotify-artifacts存在就拒绝（破坏重复任务/重试/共享目录）。最低复杂度第二意见建议独立system attempt目录，父采纳：ANOTIFY_ARTIFACT_DIR新增基础变量（原清单正式+1）指每task/attempt唯一0700 exportdir，脚本显式写files、安全枚举/托管copy/A1upload；空才[]、创建枚举错误failed；不新增runfield/CLI/stdout魔法/宿主继承、不碰用户原目录。T3当前单writer实现并给EnvArtifactDir纯delta；T1/T2已通知。根docwriter v4-artifact-env-docs显式同步4份基础env/decisions，不能假称原4变量包含此项，不回头全量重审产品D1-D6。

19:21 T3 L2实际核查：新增runner/app/emitgw/execenv/ops/resultstore/runner_test，上传client b44ff6a与shared控制字符/header编解码8a8173f已提交，非空转；父延提醒30m，要求运行链具名test PASS/FAIL和63570df后纯协议delta（T1已有635不能重复整N1）。A2docworker accepted verified（4docs基础env/执行目录清单），父独立同断言+diffcheckPASS，fresh v4-a2-export-doc-review定向读4短段 **ok:true/A2全部一致**，仅minor错误码artifact_export_invalid未进封闭词表。父已精准补capability结果码表/错误域和API同词表，不重开全量评审；新增env正式补充不假称原4项已有，不跑实现测试，准备提交A2支持。

尚未宣称任何 v4 能力完成；父最终review/跨团队集成门禁未开始。
