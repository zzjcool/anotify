# T1 HTTP/协议接缝实施清单（非权威副本）

本文件只作 G3 任务验收映射。父权威：`/root/code/anotify/.pi-orchestrator/v4/WIRE-HANDOFF.md`（W1-W6，已全文读取）；禁止在本worktree重建同名权威文件。父唯一doc writer同步设计，T1不写docs/design/v4。
父protocol/anode carveout：`/tmp/anotify-v4-core-protocol-carveout.md`（父提交21567b7）。T3唯一owner protocol/anode。N1纯快照等父传；不cherry-pick整分支。

## G3 冻结验收映射

- **W1 CAS**：既有资源 PUT/PATCH body.metadata.resourceVersion；DELETE唯一query.resourceVersion、无body/If-Match别名；缺400 resource_version_required，旧409 resource_version_conflict + details.current.resourceVersion（字符串）。POST创建/action不套DELETE规则。资源不存在仍404。
- **W2 版本**：versions为CapabilityList，每项完整Capability信封；metadata.version整数降序；裸名选择最大存在版；不造CapabilityVersionKind/currentflag。单版includeBody spec.run.files是map[path]string。
- **W3 包**：POST/PUT Playbook资源信封，metadata.name=slug；spec.files map 或spec.source字符串二选一；playbook.yaml唯一根清单，无第二份spec.values/capabilities/sources。GET includeBody→spec.files原始未渲染map；默认heavy字段省略，统计/体检/lastInstallId在status；OCI来源status.source。InstallReceipt保持主文档top-level（created/skipped对象），非资源信封。
- **W3 冲突**：HTTP400 Status.reason=playbook_conflict、code数字400；details完整valuesDigest/conflicts[]/hint；每conflict kind/ref/reason/declaredDigest/actualDigest/fields不得丢。不能用causes替代12格矩阵。
- **W4 Node**：POST nodes创建离线cluster Node→POST nodes/{name}/tokens显式签长身份token（plaintext只返回一次，无expires、无隐式建节点）；签发响应NodeToken动作文档{apiVersion,kind,node:{name},token}，无独立CRUD；再次POST轮换，旧token/已有会话失去新授权。
- **W5 config**：非独立resourcekind wrapper；Nodeconfig GET {metadata.resourceVersion,namespaces,vars,limits,configRevision,appliedRevision,appliedHash}，PUT只metadata/namespaces/vars/limits。Namespaceconfig {metadata,vars,namespaceVarsRevision}；systemconfig {metadata,retention}。Node根spec/config子资源**同底层事实同CAS**，不能用两套版本绕过。UI token取GETconfig非revision。
- **W5 batch**：request {updates:[{name,metadata:{resourceVersion},vars?,limits?,namespaces?}]}；response {items:[{name,ok:true,config:<wrapper>}|{name,ok:false,error:<Status>}]}；每节点原子可部分成功，不套Playbook整包原子。
- **W6 source fire**：{payload?:object}，省略用config.payload/{}，必须ns授权；统一ingress回执{eventId,traceId,duplicate,tasks:[{taskId,capability,version}],pruned:[{capability?,reason,detail}]}；duplicate同旧event/trace、不派发。
- **W6 dryRun**：保存源{payload?}；未保存{source:<Source写信封>,payload?}；响应{targets:[{capability,version?,params,state:ready|pruned|missing,reason?,detail?}],pruned:[...]}；无eventId/taskId，0事件/任务/去重/secret持久写入。
- **W6 console events**：显式namespace逐ns授权、limit+不透明continue、EventList metadata.continue/items；cursor绑定ns+过滤。无旧sinceId/nextCursor别名。
- **W6 logs**：{taskId,attempt,lines:[{sequence,stream:stdout|stderr,text,at}],truncated}；query tailLines/sinceSeconds/previous（previous上一个attempt，无则[]）。不读取公开env值快照。
- **W6 emissions**：{metadata:{continue},items:[{eventId,to,version,taskId:null|string,id?,pruned:null|{reason,detail}}]}；独立调用eventId区分，显式id重复只一事实、最新在前，limit/continue。

## T3 N1 与 G2/G3 对接

- 共享ParamEnvName/环境常量：artifact消费共享函数，不倒置import。
- MachineConfigHash(vars,Limits)：canonical vars/limits，map nil={}，sha256:；排除revision/ns/token/passthrough。节点与中枢只这一份hash函数。
- 人工部署下行capability.deploy {requestId,namespace,name,version,digest}；上行capability.deployed {同引用,status:applied|failed,error}。
- Node身份GET /v1/node/capabilities/{digest}授权：节点允许ns + 明确本节点deploy target；T1 server生产/鉴权/回执事实，T3 cache/client；不把files塞WS。
- 离线重连只重放明确未完成deploy意图，install不自动deploy，不造reconciler。

## A1 日志/运行产物传输（父已批准，后续G1-6/G2/G3落实）

父权威`/root/code/anotify/.pi-orchestrator/v4/ARTIFACT-HANDOFF.md`已全文读，T1不重建同名文件、不改T3protocol。
- logs_request可选attempt≥1；成功response实际attempt。HTTP previous由保留attempt事实找上一attempt历史node，不从currentnode伪造；taskId/attempt/requestId关联校验，不捏造跨流sequence/time。
- node出站PUT `/v1/node/tasks/{taskId}/artifacts/{sha256}`：64lowerhex，octet-stream；Node Bearer、共享Attempt/Lease/TaskToken/ArtifactPath header常量T3定义。path是URL编码逻辑规范相对名，不拼userworkDir。
- 201首次/同task+attempt+path+hash200幂等；同path异hash409 artifact_conflict；非法path400/hash不符400/超16MiB文件64MiBattempt32项413。
- 网络读前预检，限流式copy系统temp；**不得持DB写事务读网络**。完成发布登记同事务再次Node身份/ns/task.running/currentattempt/lease/token/expiry/cancel CAS验权；temp/DB/原子publish失败补偿，无迟到旧授权登记。
- result.artifacts只能引用当前attempt已登记manifest（[{path,sha256}]），不能借另task/ns同hash；传输期间续租，finishedAt仍子进程实际时间；结构artifact_upload_failed不能被任意script exit映射infra自动重跑。
- 独立中枢sysresultspool默认7天bytes，task/manifest30天；活动上传不GC，托管文件过期不从node回源，userworkDir原件绝不删。
- User下载 `/v1/namespaces/{ns}/tasks/{taskId}/artifacts/{sha256}`：实际task manifest引用+ns auth；200 raw、安全basename disposition；未引用404/expired410。无全局Artifact资源CRUD、无大JSON wrapper。
- 必测二次上传/冲突/尺寸hashpath/途中cancel或expiry/旧attemptresult拒绝/跨taskns读拒绝/7天410+30天manifest/用户原件安全/previous跨node定位。

## 尚未执行（真实状态）

G1仍在实施；以上仅任务卡冻结，不代表HTTP已实现。G3需真实httptest覆盖每条wire、错误/空数组/CAS与授权，OpenAPI/checker闭合，父freshreview+跨队E2E最终验收。
