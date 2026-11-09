# V4 五份 OpenAPI 文件与 operation 归属 · API1（父冻结）

文件名沿 `docs/design/v4/api-conventions.md §2`，不得另起命名或保留旧 single openapi 作为最终备用真相。T1 为五文件/校验器/HTTP实现唯一作者，T2 sitegen/文档页只读取它们。当前尚未有实际五 spec 产物，此文件是明确实施契约，不伪造已交 schema。

| 文件 | 负责 operation / DTO |
|---|---|
| `api/resource.yaml` | Capability/Source/Playbook/Node/Namespace/Task 的管理和只读状态；能力版本/人工deploy；包原始内容/render/install/折叠installs/单回执；Node登记/token/drain/upgrade与node/ns/system config、batch；Task cancel/abandon/logs/emissions、**用户Task运行产物下载**；只读edge/deployment投影和公共capability-templates。资源信封/KindList/Status及配置、动作响应例外按W1-W6。 |
| `api/events.yaml` | **只读** Event/Trace：namespace events列表/详情、trace读取、console events（显式ns授权与cursor）、事件/任务聚合stats等既有只读工作台数据。Event/Trace信封及continue读模型定义在此，不定义投递写入。 |
| `api/agent.yaml` | 仅节点身份认证的出站/中枢节点通道：`GET /v1/node/binary`、`GET /v1/node/stream`、`GET /v1/node/capabilities/{digest}`、**`PUT /v1/node/tasks/{taskId}/artifacts/{sha256}`**；以及已冻结protocol frame/DTO的schema文档（无新增资源kind）。NodeStreamPath/部署/配置/执行/emit/logs字段T3共享Go为唯一源，T1规范对账，不私镜像。内容和上传是stream/Artifact例外，不套status四键。 |
| `api/ingress.yaml` | 写入或预览触发：`POST /v1/namespaces/{namespace}/events`、`POST /v1/hooks/{token}`、`POST /v1/namespaces/{namespace}/sources/{name}/fire`、`POST /v1/namespaces/{namespace}/sources/dry-run`、`POST /v1/namespaces/{namespace}/sources/{name}/dry-run`。接收/预览回执沿W6，无新`/ingress/events`路径。中枢WS task.emit属于agent协议，不伪装公开HTTP ingress；本机POST /emit是T3节点同机网关，不进中枢HTTPpaths。 |
| `api/auth.yaml` | 所有未退役的独立身份/账户/passkey/password/recovery/keys/CLI-auth/session/admin/device/VAPID/订阅/独立消息通知功能及现有登入脚本入口。这些域保留现有合法shape与鉴权，不把删除中枢能力executor误删独立推送服务；不混入旧资源API兼容端点。 |

## 文件边界与引用规则

- 归属单位是 **HTTP method + 规范路径**，不是仅 path。`GET .../events` 在 events.yaml、相同path的 `POST .../events` 在 ingress.yaml 可以共存；合并时必须按method合并，不能后文件覆盖整path导致漏operation。
- 相同method+path不得在多文件重复定义；不同method同path必须共用一致的path参数/作用域定义。operationId跨五文件全局唯一。
- 每份有完整OpenAPI info/paths/components及security声明；允许 `agent.yaml` 的协议描述部分仅components/extensions没有HTTPpaths，但实际Node HTTP operations仍必须在agent spec出现。无协议多余 discovery/CRD 控制面。
- 路径写真实 `/v1/...` 全路径，servers不再添加另一个 `/v1` 前缀；namespace参数名保持`namespace`，T2不得只因占位名字等效就让门控丢operation。
- 资源公共 `Status`/metadata等组件可由resource.yaml统一定义；Event/Trace可由events.yaml定义，ingress引用Source或接收回执时用本五文件相对 `$ref`。允许已冻结五文件内部有效引用，不增加第六个schema真源；ref循环仅在合法schema内处理、operation不能无故递归无限展开。
- T1checker/parity必须验证本五文件内部 `$ref` 可解析、HTTP operation/handler鉴权/DTO属性对账。T2 sitegen必须读取五文件并正确保留各operation；有效跨文件ref不能因fixture简化被永久skip，未知外部/缺失ref要诚实warning，不自动回退旧api/openapi.yaml或假空成功。
- five-spec缺失阶段T2具名fixture测试不等T1真实规范已交。真实files落地后父cross验证：同path GET/POST不丢、跨文件refs、全部缺empty/部分缺warn、Node原始bytes内容例外、namespace门控和四语言文档页operation列表。

## 与分工和最终门禁

- Node token签发管理是resource.yaml（用户权限），Node token认证的binary/stream/content/upload是agent.yaml，不能因为名字都叫node而把两种鉴权混同。
- 用户task产物下载是resource.yaml namespace授权子资源，node原bytes上传是agent.yaml身份+attempt授权；能力spec内容GET又是不同下行。A1/A2的三个概念不能混。
- source资源CRUD在resource.yaml，manualfire/dry-run在ingress.yaml；同一路径概念的文档章节不决定操作文件，以上method+path表是具体裁决。
- 保留 `api/openapi.yaml` 只能是实施中尚未替换的旧文件，不供新UI/parser自动fallback；最终T1退役或明确非权威汇总生成产物，不能同时手写两套。
- 本表只补文件归属，不改变已批准D1-D6/W1-W6/A1-A3的行为或响应；真实Go接口及HTTP实现仍须原全Go/contract/e2equick与父最终全量CI/E2E，不以五yaml文件存在视为完成。
