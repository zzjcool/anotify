# V4 HTTP Wire 补充契约 · W1（父会话裁决）

状态：实施期字段/传输细节补齐，不改变用户批准 V4-D1～D6；本文件覆盖冻结文档中未定义的 wire 位置/相反旧示例，随后必须同步正文。所有团队按同一份实现，禁止由 mock/旧实现倒推契约。根分支设计文档由父指定唯一 doc writer 修改，T1/T2/T3不得抢写。

## W1 DELETE CAS

- 对既有资源的 PUT/PATCH/DELETE 保留 API §3.4 已定 CAS；不是只检查配置。POST 创建和 action POST 不套 DELETE 规则。
- PUT/PATCH 请求体使用 `metadata.resourceVersion`；DELETE **不带请求体**，唯一承载是 `?resourceVersion=<encoded-string>`（UI从资源当前 metadata.resourceVersion 构造并URL编码）。不并存 If-Match/body 两条协议。
- 缺400 `resource_version_required`，旧409 `resource_version_conflict`，`details.current.resourceVersion` 返回当前令牌；404仍表示资源不存在。保持字符串类型不由JS转Number。

## W2 能力版本读取

- `GET …/capabilities/{name}/versions` → `CapabilityList`，`items[]` 每项为完整 `Capability` 信封，metadata.name相同、metadata.version不同；按metadata.version整数降序，新→旧。
- 不引 `CapabilityVersion` / `CapabilityVersionList` / `CapabilityRevision` 新kind。裸引用name选择最大存在version；UI current标识由max(metadata.version)计算，不使用自造 `item.current` 字段。
- 单版本GET（如果现已指定该路径）同Capability信封；includeBody=true才带spec.run.files全文（map[路径]string，不是files[].content数组）。spec/status字段完整性与正文已有includeBody投影约定一致。

## W3 Playbook 资源传输

- POST/PUT统一资源信封，而非裸 `{slug,files}`：
  ```json
  {"apiVersion":"anotify/v1","kind":"Playbook","metadata":{"name":"project-chain","namespace":"default"},"spec":{"files":{"playbook.yaml":"...","capabilities/review.yaml":"..."}}}
  ```
- `metadata.name`就是slug。files/source二选一放在spec：直接上传`spec.files`为map[相对路径]string；OCI入口`spec.source`为字符串，服务端拉取一次后以解析的完整files包存储，并保存源provenance。不能同时给两者；body不得带status；PUT需metadata.resourceVersion。
- 包内`playbook.yaml`仍是唯一根清单，values/能力/源相对引用从该文件解析，**不要求请求再复制spec.values/spec.capabilities/spec.sources**。API示例不能继续暗示有第二份并行根清单。
- GET/list默认不包含heavy文件内容（spec可因投影为空），counts/体检/lastInstallId等在status；GET`?includeBody=true`的`spec.files`返回原始未渲染整个map。OCI provenance放`status.source`（已存在来源记录的投影，不作为独立配置源）。编辑/导出必须请求includeBody=true；默认摘要不能被当完整PUT模板。
- render响应保持Playbook主文档已有形状（渲染files/values/digest），不代替读取原始包体。普通install和单回执保持InstallReceipt已有顶层形状/created和skipped对象，不搬进资源status。
- `playbook_conflict` → HTTP400统一`Status`：code数字400，reason字符串playbook_conflict，原fields放`details.valuesDigest`、`details.conflicts[]`、`details.hint`；每conflict保留kind/ref/reason/declaredDigest/actualDigest/fields。不得裁掉12格判定/冲突明细。可同时给通用details.causes，但不能用causes丢失结构化矩阵。
- 信封强制检查仅资源文档/资源列表；InstallReceipt、配置子资源、NodeToken、动作回执/预览、日志等具体响应文档为明确例外，不伪装另一个资源kind或持久化独立对象。

## W4 Node 注册与令牌

- 初次引导先`POST /v1/nodes`创建离线Node信封（metadata.name必填），再`POST /v1/nodes/{name}/tokens`签发身份凭证。不存在Node→404，不能在签token时隐式创建/选择namespace。
- 创建默认授权namespaces=["*"]，配置vars/limits按现冻结默认；Node仍cluster对象，实际可用ns取本机申报∩中枢授权。原同名多实例不支持，不能签token时顺手归到某个ns。
- tokens动作可接受空body `{}`。响应文档：
  ```json
  {"apiVersion":"anotify/v1","kind":"NodeToken","node":{"name":"node-a"},"token":"<new-node-token>"}
  ```
- NodeToken是一次动作响应，不是独立资源、无GET/list/DELETE端点。'一次性令牌'改为'**明文只返回这一次**'，不是单次使用bootstrap；该token长期用于节点身份，v4暂不定义expiresAt/自动续签。再次POST显式轮换当前凭证，旧token失效（已有会话亦失去新授权）；UI需明确提示后再轮换。Node/read/config不回显token。
- anode install使用已签发节点token配置，不能以伪造自动注册替代前置管理动作。

## W5 配置子资源 Wrapper

不是独立resourcekind。GET Nodeconfig：
```json
{"metadata":{"resourceVersion":"R"},"namespaces":["*"],"vars":{},"limits":{"total":1,"perCapability":{}},"configRevision":"C","appliedRevision":"A","appliedHash":"sha256:..."}
```
PUT Nodeconfig仅接`metadata.resourceVersion/namespaces/vars/limits`（上述revision/applied字段只读，UI不提交）；返回同GET wrapper。GET/PUT namespaceconfig：`{metadata:{resourceVersion},vars:{},namespaceVarsRevision:"N"}`；PUT仅metadata/vars。systemconfig沿`{metadata,retention}`。
- Nodeconfig更新与Node.spec底层默认是同一份事实，根Node可写配置字段与子资源不能各用不同版本绕过CAS；T1统一它们的底层版本/事务。UI编辑一律GETconfig取得该CAS令牌，不拿configRevision/appliedRevision代替。
- batch沿既有请求`{updates:[{name,metadata:{resourceVersion},vars?,limits?,namespaces?}]}`，响应`{items:[{name,ok:true,config:<GET wrapper>}|{name,ok:false,error:<Status>} ]}`；逐节点原子、可部分成功，**不是Playbook整包原子规则**。返回错误不得带原始凭证值。

## W6 Source 动作与诊断读取（不依赖未交T1接口）

- manual fire `POST …/sources/{name}/fire` body`{payload?:object}`；省略payload使用该源已有config.payload/默认{}。不存在/停用的规则沿Source文档，必须做ns授权。不新增manual专属幂等/额外路由语法。
- events与hooks投递原有`{source?,type?,id?,payload?}`沿各入口，source在事件APIbody必填、hook由token定位；返回接收回执统一`{eventId,traceId,duplicate:boolean,tasks:[{taskId,capability,version}],pruned:[{capability?,reason,detail}]}`。manual fire复用此回执。入库剪枝也返回合法event/trace，duplicate返回原事件/trace，不重复派发。
- 保存源dry-run body`{payload?:object}`；未保存源dry-run body`{source:<Source写信封>,payload?:object}`。响应`{targets:[{capability,version?:int,params:object,state:"ready"|"pruned"|"missing",reason?:string,detail?:string}],pruned:[...]}`；是预览文档，无eventId/taskId、不入事件/任务/去重、不需要保存token或返secret。准确原因沿现封闭表。
- `GET /v1/console/events?namespace=NAME`必须逐ns授权；分页仅`limit`+不透明`continue`，返回`EventList`/metadata.continue/items Event信封。刷新第一最新页按metadata.uid去重，不暗加旧sinceId/nextCursor协议。source/traceId已有过滤可保留明确ns绑定；cursor必须绑定ns+筛选条件，不能借另一ns cursor读越权记录。
- tasks logs子资源响应`{taskId,attempt,lines:[{sequence,stream:"stdout"|"stderr",text,at}],truncated:boolean}`，tailLines/sinceSeconds/previous为已有查询规则；previous指定上一次attempt（无则空lines），不混淆逻辑task父子。日志来源本机档案与WS读取，但秘密不得被系统环境快照自动写日志（脚本打印仍作者责任）。
- tasks emissions子资源返回`{metadata:{continue},items:[{eventId,to,version,taskId:null|string,id?:string,pruned:null|{reason,detail}}]}`，每条emit接受事实，不是端口/自动业务seq模型；用eventId区分独立调用，显式id可重复请求但只一事实。默认最新在前；分页limit/continue。如页面需要载荷正文必须明确请求已有body扩展，不靠该摘要泄露原始宿主env。

## 7. 交接纪律

父会话同步上述正文后给T1/T2/T3同一版本；三个团队可据此继续，不等待尚未交的EXPORTED-INTERFACES猜旧wire。新公共Go签名由T1实际代码导出，不能反过来改HTTP形状。任何与批准产品语义真正相反的问题给父证据，不能用旧STATUS假设/自造mock通绿。
