# V4 日志 attempt 与运行产物传输补充 · A1（父批准）

范围：落实已批准 task 日志/托管产物读取，不新增 Artifact 独立资源、额外节点监听、凭证中心或历史协议兼容。T3为纯协议/node唯一作者，T1为中枢HTTP/store作者；两侧按此补最小接缝。

## 1. 日志 attempt

- 保留 `logs_request` / `logs_response` 帧名，request新增可选 `attempt` 整数>=1。省略表示该逻辑task最近一次有档案的attempt。
- 成功response必须回实际`attempt`；错误/无档案不能伪造一个latest冒充请求的历史。HTTP `previous=true` 由T1根据保留的attempt事实确定上一attempt，显式传给node；无上一attempt直接空lines。
- Node按(taskId,attempt)读取精确本机档案；重试曾换node时，T1必须将请求发给对应attempt的历史node，不发给当前node然后返回另一次日志。node离线/档案过期返回真实不可读，不伪造空成功。
- `requestId`只请求配对；响应taskId/attempt要验证。帧JSON标签/Decode/键快照fixture同步，已有日志请求限制仍有效。HTTP输出沿 W6 的 lines/stream/sequence/text/at；实际档案若没有可生成的结构化数据，先向父报告最小缺口，不猜时间戳或跨流顺序。

## 2. 唯一运行产物通道：node出站 HTTP PUT 到中枢

不采用新增 artifact.begin/chunk/commit WS帧。node向已存在中枢服务发HTTP请求不会新增node公开监听，使用既有Node身份+当前attempt授权，不是新凭证模型。

- 上行：`PUT /v1/node/tasks/{taskId}/artifacts/{sha256}`。
- `{sha256}`是完整文件字节SHA256的64位小写hex，不带`sha256:`；不是能力spec digest。Body是`application/octet-stream`原始字节，不JSON/base64大包。
- Headers：`Authorization: Bearer <node-identity-token>`、`X-Anotify-Attempt`（整数）、`X-Anotify-Lease-Id`、`X-Anotify-Task-Token`（attemptToken）、`X-Anotify-Artifact-Path`（URL编码的逻辑相对路径）。这些共享具名常量由T3 protocol定义，T1直接使用；不把凭证放query/日志。
- Path是托管manifest的逻辑名，不能用它拼宿主原文件路径。必须是规范相对路径，无绝对路径/.. /NUL/backslash，不包含系统路径/用户workDir前缀。多逻辑路径同hash可以指向相同托管bytes，metadata按task+attempt+逻辑path记录。
- 201首次接受，200同task+attempt+path+hash再次上传（幂等），响应文档`{taskId,attempt,path,sha256,size}`，无新可写资源kind。相同逻辑path不同hash同attempt冲突409 `artifact_conflict`，不偷偷覆盖已登记的内容。
- Node身份/task归属/ns授权/当前running/attempt/leaseId/attemptToken与未过期必须验证；开始可预检，但**收完bytes发布登记时再同事务/CAS检查**，取消/旧attempt不能凭先前验证接受迟到文件。旧token或node身份失效按现有401/409错误域拒绝。
- 大小初始技术上限：单文件16MiB、单attempt64MiB、最多32项；声明/实际长度都检查，实际写入有上限，超限413 `artifact_too_large`，不静默截断。数值后续可统一配置，不扩成新配额产品。
- HTTP等待有界（node建议30秒每请求，服务端读/闲置超时明确），以流复制到独立系统temp，不把全文放内存，不在长网络读取期间持DB写事务。完成核实bytes长度+sha256；不匹配400 `artifact_hash_mismatch`，非法路径400 `artifact_path_invalid`。
- 通过权威CAS后，原子发布独立中枢系统结果目录中的托管副本并登记该attempt引用；不读/服务node用户workDir，更不删除用户原件。发布/DB失败需补偿清理；孤儿temp有有界TTL，在正在上传/活动任务中不能删有效文件。

## 3. 与结果提交、重试、清理的关系

- node本地先安全复制需要导出的文件到独立系统结果目录、生成path/hash清单；只从这些安全托管副本上传，不开放用户任意path读取接口。
- 上传所需副本成功（201/200）后才发`task.result`；result.artifacts仍`[{path,sha256}]`，server仅允许当前attempt已经验证登记的引用。不能拿另一个task/ns的已知hash作为引用借读别人文件。
- 上传与结果提交期间正常续租仍继续；进程结束时的finishedAt保留，不把传输耗时伪装脚本执行时长。取消/lease失效即停上传与授权；迟到result仍拒绝。
- 断线/超时可用同path/hash/attempt幂等重试上传，必须仍持有效lease，次数/总时间有界；不因此整任务重跑或把未知接受当没有上传。确认失败且仍有授权时，以结构化`artifact_upload_failed`终止结果（保留已成功托管的manifest和错误，不伪造成功空artifacts）；仅有可靠infra事实且任务明确幂等时才按既定D4自动重试。
- 本次只补现有TaskResult.artifacts传输，不擅自新增第四CLI命令、run.artifacts配置字段、额外宿主环境继承或JSON stdout魔法。生产运行如何明确登记导出文件必须由T3基于已有runner输出机制给出最小事实；若缺入口，先报父，不能用永远空数组隐藏未实现需求。
- 中枢托管bytes按resultTtlSec默认7天；有效/活动上传不清理；manifest/task事实按taskTtlSec默认30天/引用保护。用户workDir原文件不受这个TTL影响。已到期副本不从node回源，不因task元数据还存在伪装可下载。

## 4. 用户下载仅 Task 子资源

- `GET /v1/namespaces/{namespace}/tasks/{taskId}/artifacts/{sha256}`，现有登录/Bearer用户ns鉴权+该task结果manifest中的实际引用。
- 200原始字节流，`Content-Type: application/octet-stream`，`Content-Disposition`使用登记逻辑path的安全basename（禁止CRLF/路径逃逸）；SHA和task引用是定位条件，不直接读任意文件。
- 未在该task结果引用中404，托管副本已清理但manifest仍在410 `artifact_expired`。不因为hash相同允许跨task/ns未授权下载；不新建全局artifact列表/CRUD资源。两类artifact要文档区分：能力spec GET /node/capabilities/digest与任务输出bytes不是同通道。
- 本下载和node上传为流式响应/动作文档，资源信封checker需要明确例外；API规范放agent/node spec与resource/task子路径相应域，不能套四键或用大JSON编码数据。

## 5. 实施与测试分工

- T3：logs attempt/共享上传路径与header常量；本机安全copy/manifest及HTTP上传客户端、lease保持/取消、精确日志档案；所有protocol变更只它一方写，给纯delta及更新HANDOFF。
- T1：Node流上传handler/两次授权CAS/中枢安全spool+manifest持久化/结果引用校验/TTL/Task下载鉴权；HTTP日志previous定位历史node。不得自行另造共享DTO或Artifact资源。
- T2：Task结果中只展示已登记path/hash链接，过期/离线/不可读状态明确，ns路径沿现helper；不从node地址/userworkDir路径拼下载URL。
- 必测：二次上传200无重复引用、异hash409；大小与hash错误/非法路径；上传途中取消或lease过期不能登记、旧attempt不能提交result引用；跨task/ns已知hash读拒绝；7天过期410/30天manifest仍可诊断；userworkDir/原文件不被删；previous在跨node重试仍定位正确attempt。
