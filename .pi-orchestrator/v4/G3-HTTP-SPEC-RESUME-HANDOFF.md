# G3 接续实施冻结（HTTP / 受权产物传输 / exact-five OpenAPI）

## 前置条件与事实

原 `teams/core-runtime/task-cards/G3.md`、HTTP-HANDOFF-CHECKLIST、父D1-D6/W1-W6/A1/A3/五spec+API-ref/auth/result/本地化契约不变，本卡补齐owner/接缝/后续task表示。

已事实摸底（g3-http-scope-facts / g3-tests-spec-facts / artifact-download-seam-facts）：现server/api为旧扁平v3装配、50server testfiles/345Test + api18Test；五spec0/5/resultspool/check_envelope未创建。旧HTTP编译依赖退休executor/wsNodeHandler，不能先跑旧baseline当开发进展。G2-R1 criticalmajor已经有限返修+三fresh及父4门禁清，emit未解析failure回执E1另单writer修，**须E1完成sourcefreeze/fresh清major/父验收后再G3源码writer**，禁止同WT并发写。

## G3-D1 owner与分段

唯一当前G2隔离WT单writer串行G3a→G3b→G3c，可用组长调度但只能一个源码writer，组长仅专属契约/证据不竞写。保留已有S1/Node/runtime源快照，禁止又开旧recovery树/回写父生产。作用域：

- `internal/server/**`、`internal/api/**`、`internal/auth/**`仅装配与退役builtin期望（功能不退），`internal/authn/**`仅用户ns/Node授权接线；新`internal/resultspool/**`。
- `api/{resource,events,agent,ingress,auth}.yaml`，**删除旧api/openapi.yaml**（不保非权威第六聚合）。exact-five为唯一真源；前端sitegen五文件消费者另归T2，不因legacyconsumer暂时没改保留旧真源回退。
- 明确checker launchlist：`scripts/check_contract.py`、`scripts/check_openapi.py`、新`scripts/check_envelope.py`；`docs/contracts/CONTRACT.md`只同步v4已冻结机械边界，不新增产品语义。
- owner例外仅下D4授权 `internal/store/artifact_manifest.go` 的download选择函数及直接混合expiry测试，不改schema/result/registry/namespace/C3写authority。其他store/runtime/protocol/artifact/Node/Go依赖/Makefile/cmdserver/devseed/T2/UI/sitegen/E2Esuites只读。
- **Makefile仍G4 owner**，G3不改其配方；现makecontract调用两checker和TestOpenAPIParity，可在同名checker/Go测试下实现five-spec规则，无需新增入口才算可验收。新envelope规则可由现check_contract.py调用新模块/Go契约test串接；G4再统一工具链更新，不能G3偷偷替Makefile迁移cmd/server/seed。

## G3-D2 HTTP资源/认证/保留域

- 资源`/v1/namespaces/{namespace}/...`按name/path身份，lightweight envelopes apiVersion/kind/metadata/spec/status；statuses readonly derived，不回兼容旧flat routes/kinds/ports/edgeCRUD/执行命令。
- CAS只metadata.resourceVersion绑定UID/revision，DELETE只query?resourceVersion，missing400/conflict409/currentopaqueversion，create严`camelCase`、空列表[]、spec版本immutable规则；允许v4baseline无迁移但旧库明确拒不删数据。
- Capability/source/node/namespace/playbook字段按主文档+W1W6。Playbook files/source xor/rootmanifest、install只加不改/冲突0写/values每次newreceipt、source-name共享validator（namespacedbadnames不能install绕过）与secretswriteOnly provenance保。
- Node注册再Token明文one-shot、secret/hash不泄漏；管理动作由用户namespace权限CAS，Node执行上传身份由currentNodeBearer+attempt/lease/token，Cookie仅auth身份域/me等现功能不套Resource Status。
- Task只读事实/操作Task cancel/history等真实runtime；未解析`downstream_missing` failedTask例外沿S1/E1：无解析capUID/digest，未声明version不公开0，保请求basename/declaredpositiveversion、attempt0/noNode过程result。不能prunednull冒failure，不能NULL扫描隐藏Task。
- Source Fire/hook/events/dryrun统一真实runtime receipt，不新增内部failedTasks资源字段；纯pruned noTask，failedTask必须在tasks里有真实ID。EmitterHTTP与NodeWS只用现sharedEmitResponse（TaskID可failed，未知Version字段省略按E1）；不镜像第二DTO。
- EventList continuation/cursor/trace/event/task/emissions/out真实公形状，W6/status派生，不event-onlyrawstore字段直接泄漏。
- **保全独立域** auth/password/passkey/recovery/CLIapikey/session/admin/devices/VAPID/broker/backpressure/POSTnotify/WebPush。旧centralnotify.phone能力+任务断言/执行器刻意退役，不恢复中央exec、不借“notify-phone已删”删除独立投递；原否定安全/权限断言不弱。

## G3-D3 日志/产物上传与结果事实

- HTTP logs用户namespace/task授权后调用真实Hub.RequestLogs(ns,task,previous,tail,since...)，并明确绑定actualattempt/historicalnode；输入非负tail<=5000/since非负，不提前截断后另过滤；真实lines保持sequence/Node采集at，不raw拼时间序；previousnone200attempt0/[]唯一例外；offline503/timeout504/noarchive410/unreadable-corruptbinding502，crossns404。不能缺索引/离线假空成功。
- A1上报出站PUTtask-artifactSHA路径/头/NodeBearer/attemptleaseattempttoken用冻结原文和共享实际字段，无新增匿名/globalartifactCRUD；currentdb授权与限额16MiB/attempt64MiB32objects/bodydigest/上传registry同事实，lease/cancel/凭证撤销只按现authorized接口。不能拿metadata ArtifactManifest当已上传真实bytes。
- resultspool托管副本只中枢临时目录0700/文件0600、完整body/hash校验/atomic落盘/没有上传不登记、错误清理只ownedtemp/managedstore，不读/删用户workdir，不Node离线回本机原文件。active/reference/D6 7d副本/30dTaskmanifest/0永久沿已批准策略。
- Node结果事实C3b raw完整纳秒/nullableexit/serverNow/可靠retry/cancelACK/historicalslot标准不重定义，不handlerprecheck后裸完成。有缺public写入口明确报父signature，不copy旧Finish兼容。

## G3-D4 SHA下载选择裁决（无新pathquery）

现冻结HTTP route和store签名只按namespace/task/hash，无path入参。**不新增path选择query/端点**：授权针对该Task当前reportedresultmanifest里实际ResultRef；同一SHA在该manifest多个路径引用时，只要有任一仍live、已托管副本的合法引用，200返回该真实live条目（如多个live用path排序首live给safebasenameDisposition），不能首expired提前410遮其后live。

- SHA至少被当前任务manifest引用但所有副本已过期→410 artifact_expired；没有当前任务ResultRef、非reported/unknownhash/跨ns/task或只有别task/历史attempt同hashlive→404。
- `ExpiredAt`或可信ExpiresAt<=now的条目本身仍expired，不通过别task/ns/globalsha字节复活。只有**同一授权当前manifest**另live引用才可按hash请求取内容；metadata每path的expired事实不回改。
- 仅servermanaged副本/真实registry，不能Nodeoffline/用户filefallback。
- 必新增同sha两个path混live/expired、正反字典序、全部expired、历史attempt-vs-current、跨ns/task、expiredowner不能借其他ownerlivebytes；原C3manifest/upload/auth tests不删。

## G3-D5 exact-five OpenAPI与机械checkers

五文件分配沿API-SPEC-HANDOFF：resource私有ResourceCRUD/admin配置操作；events观察Task/Event/Trace/List/日志/下载读取；agentNodestream/节点出站受权upload；ingress外部事件/hook/emit入口；auth保独立认证与devices/keys/notify等现非Resource域（具体已有handoff归属优先）。归属键(method,canonicalpath)，同path不同method不相互覆盖，全operationId唯一。

只五文件内部/相对crossfile$ref可解析，pathItems在components.pathItems合法，禁止第六truth/ref/旧singlefallback。每registeredHandlermethod/path必须一份spec，每specoperation实际有handler；security(anonymous/session/ConsoleBearer/NodeBearer)实际吻合；DTO/状态/字段反射或真实HTTP反例双向，不仅检查路径存在。envelope/statusreadonly/kind/camelCase/[]/cursor元数据及opaqueCAS/fieldwriteOnly/provenance列强断言。

check_contract.py原A2/A4/A5/A6/A7升级v4真实边界，不为绿禁用os/exec prohibition/内容寻址/上传auth/registry/错误0写；Pythonpy_compile语法与五specparity/ref脚本+Go TestOpenAPIParity实际执行。只有Go yaml现测试库可复用，外部yaml工具缺不安装写node_modules/env；不足精确报父。

UI/sitegen原单fileproducer后需T2有限reader迁移真实five集合，当前G3不能动也不能假五空spec通过；给真实five文档+operationinventory供父T2接线。不以旧前端mock195PASS顶实际fivebackendcross。

## G3-D6 tests/验收与止损

原server345/api18测试先具名inventory，按当前安全事实迁移，不wholefiledeprecated清空auth/设备/namespace/CAS/delete/secret/digest/FK/backpressure断言；仅旧v3flat/centralexec专属语义退役有具体理由/newcase替代实际安全意图，不復v3HTTPwire。新W1W6/Resource/status/source/Taskreceipt/log/NodeToken/Playbook/config/actions受权错误拒全部真实httptest，newupload/downloadBytes/hash/limit/expired/crossns/cancelrotate真实NodeBearer/registry，five-parity-positive/negative mutation都覆盖。

固定verify：canonicalroot/gomod+只读gofmt internal/server/api/auth/authn/resultspool +gitdiffcheck（D4store只读fmt也含单file）；govet这些包；gotest这些包-count1；govet/gotest store+六runtime+protocolNode相关回归按source改动实范围；Python checker语法+five checker/Go TestOpenAPIParity；收尾原 `go test ./... -count1` / `make contract` **仍必跑**，G4/cmdserver/seed旧接口静态断裂要如实SAFE_BLOCKED列owner而非复兼容/不跑命令冒已全通过。G4后的最终e2equick/fullE2E/CI/build/fingerprint仍全目标不减。

每长命令900s受管name/fulltee/真实exit，探针socket/contextdeadline/timeout60，同root2failSAFE_BLOCKED；不得不停compile旧baseline/重建manager或全仓改scope。子任务每launch填完整verify，最多一个源码writer，fresh三角附全部D决策/刻意不做，criticalmajor精确返修复审。

owner-onlylocalcommit供父验收，不push/MR/merge/rebase/reset/squash/env/settings/.git/node_modules/用户data/旧WTsessionlog清理。最终exactchangedfiles/fiveOperationinventory/runtime接口/public错误/测试具名mapping/门禁真实exitlogs/当前不兼容G4/T2具体remaining；G3DONE不等整个v4已CI/E2E完工。
