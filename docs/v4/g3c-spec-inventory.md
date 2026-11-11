# G3c · exact-five OpenAPI 清单与机械规则（#333）

> 状态：**冻结**（本文件是 #333 的实现契约，也是给 #335 frontend/sitegen 的输入清单）。
> 基线：`issue/333-five-openapi` @ `289896f`（= `origin/issue/332-artifact-transport`，含 #331+#332 的 v4 路由/DTO）。
> 权威顺序：用户本轮指令 > issue 原文 > G3-D5/D6 + W1–W6/A1/E1 > `docs/design/v4/*` > 旧代码注释。
> **代码是权威**：本文件记录的每个 path/method/字段都以 `289896f` 的真实 mux/handler/DTO 为准；
> 与冻结文档冲突处见 §5「已知缺口与裁决」。

## 1. exact-five：五份真源

| 文件 | 内容边界 | operation 数 |
|---|---|---|
| `api/resource.yaml` | Capability / Source / Playbook / Namespace / Node / Task 的管理与只读状态、只读 edge/deployment 投影、公共 capability-templates | 53 |
| `api/events.yaml` | **只读** Event / Trace / 控制台事件流 / 集群聚合读模型（stats、orphans） | 5 |
| `api/agent.yaml` | 节点身份鉴权的出站/中枢节点通道（binary / stream / 能力包体下行 / 产物上行）+ 已冻结 protocol frame/DTO 文档 | 4 |
| `api/ingress.yaml` | 写入或预览触发（events 投递、hook、fire、dry-run） | 5 |
| `api/auth.yaml` | 独立身份域：auth/passkey/password/recovery、keys、devices、VAPID、CLI 设备授权、passkey 注册、admin、登入脚本 | 58 |
| | **合计** | **125** |

- 旧 `api/openapi.yaml`（v3 单文件真源，6192 行）**删除**，任何消费者不得回退（`scripts/check_openapi.py` 的 exact-five 检查会红）。
- `api/` 下只允许这五个 `.yaml`；第六个文件即失败。
- 每份都有 `info` / `servers` / `tags` / `paths` / `components`，并且**每个 operation 显式声明 `security`**（匿名写 `security: []`）。

## 2. 归属与命名机械规则

- 归属键 = **(METHOD, canonical path)**；canonical path 把所有 `{...}` 归一成 `{}` 后比较。
- 同 path 不同 method 不互相覆盖：`GET /v1/namespaces/{namespace}/events` 在 `events.yaml`、`POST` 同 path 在 `ingress.yaml`。
- 同 (METHOD, canonical path) 不得跨文件/同文件重复；**`operationId` 跨五文件全局唯一**。
- path 写真实全路径 `/v1/...`；`servers: [{url: /}]`（**不再补 `/v1`**，否则拼出 `/v1/v1/…`）。
- namespace 位置的占位符必须字面是 `{namespace}`（实现内部用 `{ns}`，spec 统一成 `{namespace}`）。
- 唯一非 `/v1` 的 spec 路径：`/agent-login.sh`（`curl … | sh` 登入口）。
  `/health`、`/debug/pprof/*`、`/` 静态兜底**不在**五份 spec 内（显式排除，理由见 `check_openapi.py` 的 `NON_SPEC_ROUTES`）。

## 3. 共享组件契约（全部定义在 `api/resource.yaml`，其余四份用相对 `$ref` 引用）

跨文件引用形状：`./resource.yaml#/components/schemas/Status`、`./resource.yaml#/components/responses/Status400`。
**安全方案不可跨文件**：每份文件在自己 `components.securitySchemes` 里定义它用到的方案，四份定义必须逐字一致。

### 3.1 securitySchemes（白名单，跨文件逐字一致）

```yaml
session:      {type: apiKey, in: cookie, name: anotify_session}
consoleKey:   {type: http, scheme: bearer}      # 控制台 API key
nodeToken:    {type: http, scheme: bearer}      # 节点身份令牌
adminSession: {type: apiKey, in: cookie, name: anotify_session}   # 会话 + 超管（两层中间件）
```

### 3.2 schemas

| 名称 | 契约 |
|---|---|
| `ObjectMeta` | `name`(必填) `namespace?` `uid?` `version?`(int，仅 Capability/Playbook) `resourceVersion?` `creationTimestamp?` `displayName?`；**没有** labels/annotations/generation/finalizers |
| `ListMeta` | `continue?`（不透明 cursor，仅 console events 与 task emissions 会填） |
| `Status` | 错误信封：`apiVersion`(anotify/v1) `kind`(Status) `status`(Failure) `code`(int) `reason`(机器码) `message?` `details?` |
| `StatusDetails` | `causes?[{reason?,message?,field?}]` `current?{resourceVersion?}` `retryAfterSeconds?` |
| `StatusCause` | `reason` `message` `field` |
| `AuthError` | `{error: string}` —— **身份域**（auth/admin/devices/keys/passkey-enroll/cli-auth）除 409 外的全部错误体，含中间件与 hook 限流 |
| `AuthFlow` | 身份流程会话的 409 体：`oneOf [{status}, AuthError]` —— 终态冲突回 `{"status":"<当前状态>"}`、已消费回 `{"error"}` |
| `NodeUploadError` | `{error: <封闭码>}`，码集 `artifact_path_invalid|artifact_hash_mismatch|artifact_too_large|artifact_conflict|unauthorized|forbidden|artifact_upload_unavailable|internal` |
| `BackpressureError` | `{error,pending_age_sec,max_pending_age_sec,retry_after_sec}`（snake_case 由实现冻结） |

### 3.3 responses（错误响应**必须** `$ref` 具名组件，名字与状态码对齐）

`Status400/401/403/404/405/409/410/413/429/500/501/502/503/504`、
`AuthError400/401/403/404/405/409/429/500`、`AuthFlow409`、
`NodeUploadError400/401/403/409/413/503`、`Backpressure503`。

`check_openapi.py` 强制：任何 4xx/5xx 响应必须是 `$ref`，且组件名 == `<Family><status>`，
Family ∈ {Status, AuthError, AuthFlow, NodeUploadError, Backpressure}。这条把「哪些错误形状是例外」变成显式清单，而不是靠缺 schema 蒙过去。

### 3.4 components.pathItems

path item 用 `$ref: '#/components/pathItems/<Name>'`（跨文件 `./x.yaml#/components/pathItems/<Name>` 亦支持），
用于承载「同 path 在另一份文件里只有另一个 method」以及多参数共享的 path item。当前使用：

| 文件 | pathItem | 说明 |
|---|---|---|
| `events.yaml` | `NamespaceEvents` | `GET /v1/namespaces/{namespace}/events`（读模型） |
| `ingress.yaml` | `NamespaceEventIngest` | `POST` 同 path（投递入口，Bearer key） |
| `agent.yaml` | `NodeStream` / `NodeBinary` / `NodeCapabilityContent` / `NodeArtifactUpload` | 四条节点通道 |

## 4. operation 清单（给 #335 的 inventory）

`security`：`—`=匿名（`security: []`）｜`S`=session｜`K`=consoleKey｜`N`=nodeToken｜`A`=adminSession

### 4.1 `api/resource.yaml`（53）

| # | METHOD | path | security | operationId |
|---|---|---|---|---|
| 1 | GET | `/v1/namespaces/{namespace}/capabilities` | S | `listCapabilities` |
| 2 | POST | `/v1/namespaces/{namespace}/capabilities` | S | `createCapability` |
| 3 | GET | `/v1/namespaces/{namespace}/capabilities/{name}` | S | `getCapability` |
| 4 | PATCH | `/v1/namespaces/{namespace}/capabilities/{name}` | S | `patchCapability` |
| 5 | DELETE | `/v1/namespaces/{namespace}/capabilities/{name}` | S | `deleteCapability` |
| 6 | GET | `/v1/namespaces/{namespace}/capabilities/{name}/versions` | S | `listCapabilityVersions` |
| 7 | GET | `/v1/namespaces/{namespace}/capabilities/{name}/versions/{version}` | S | `getCapabilityVersion` |
| 8 | POST | `/v1/namespaces/{namespace}/capabilities/{name}/deploy` | S | `deployCapability` |
| 9 | GET | `/v1/namespaces/{namespace}/edges` | S | `listEdges` |
| 10 | GET | `/v1/capability-templates` | S | `listCapabilityTemplates` |
| 11 | GET | `/v1/namespaces/{namespace}/sources` | S | `listSources` |
| 12 | POST | `/v1/namespaces/{namespace}/sources` | S | `createSource` |
| 13 | GET | `/v1/namespaces/{namespace}/sources/{name}` | S | `getSource` |
| 14 | PATCH | `/v1/namespaces/{namespace}/sources/{name}` | S | `patchSource` |
| 15 | DELETE | `/v1/namespaces/{namespace}/sources/{name}` | S | `deleteSource` |
| 16 | GET | `/v1/namespaces/{namespace}/playbooks` | S | `listPlaybooks` |
| 17 | POST | `/v1/namespaces/{namespace}/playbooks` | S | `createPlaybook` |
| 18 | GET | `/v1/namespaces/{namespace}/playbooks/{slug}` | S | `getPlaybook` |
| 19 | PUT | `/v1/namespaces/{namespace}/playbooks/{slug}` | S | `replacePlaybook` |
| 20 | DELETE | `/v1/namespaces/{namespace}/playbooks/{slug}` | S | `deletePlaybook` |
| 21 | POST | `/v1/namespaces/{namespace}/playbooks/{slug}/render` | S | `renderPlaybook` |
| 22 | POST | `/v1/namespaces/{namespace}/playbooks/{slug}/install` | S | `installPlaybook` |
| 23 | GET | `/v1/namespaces/{namespace}/playbooks/{slug}/installs` | S | `listPlaybookInstalls` |
| 24 | GET | `/v1/namespaces/{namespace}/playbooks/{slug}/installs/{installId}` | S | `getPlaybookInstall` |
| 25 | GET | `/v1/namespaces` | S | `listNamespaces` |
| 26 | POST | `/v1/namespaces` | S | `createNamespace` |
| 27 | GET | `/v1/namespaces/{name}` | S | `getNamespace` |
| 28 | PATCH | `/v1/namespaces/{name}` | S | `patchNamespace` |
| 29 | DELETE | `/v1/namespaces/{name}` | S | `deleteNamespace` |
| 30 | GET | `/v1/namespaces/{name}/config` | S | `getNamespaceConfig` |
| 31 | PUT | `/v1/namespaces/{name}/config` | S | `putNamespaceConfig` |
| 32 | GET | `/v1/namespaces/{name}/stats` | S | `getNamespaceStats` |
| 33 | GET | `/v1/system/config` | S | `getSystemConfig` |
| 34 | PUT | `/v1/system/config` | S | `putSystemConfig` |
| 35 | GET | `/v1/nodes` | S | `listNodes` |
| 36 | POST | `/v1/nodes` | S | `createNode` |
| 37 | POST | `/v1/nodes/config/batch` | S | `batchNodeConfig` |
| 38 | GET | `/v1/nodes/{name}` | S | `getNode` |
| 39 | PATCH | `/v1/nodes/{name}` | S | `patchNode` |
| 40 | DELETE | `/v1/nodes/{name}` | S | `deleteNode` |
| 41 | POST | `/v1/nodes/{name}/tokens` | S | `issueNodeToken` |
| 42 | GET | `/v1/nodes/{name}/config` | S | `getNodeConfig` |
| 43 | PUT | `/v1/nodes/{name}/config` | S | `putNodeConfig` |
| 44 | POST | `/v1/nodes/{name}/drain` | S | `drainNode` |
| 45 | POST | `/v1/nodes/{name}/upgrade` | S | `upgradeNode` |
| 46 | GET | `/v1/namespaces/{namespace}/tasks` | S | `listTasks` |
| 47 | GET | `/v1/namespaces/{namespace}/tasks/{taskId}` | S | `getTask` |
| 48 | POST | `/v1/namespaces/{namespace}/tasks/{taskId}/cancel` | S | `cancelTask` |
| 49 | POST | `/v1/namespaces/{namespace}/tasks/{taskId}/abandon` | S | `abandonTask` |
| 50 | GET | `/v1/namespaces/{namespace}/tasks/{taskId}/logs` | S | `getTaskLogs` |
| 51 | GET | `/v1/namespaces/{namespace}/tasks/{taskId}/emissions` | S | `listTaskEmissions` |
| 52 | GET | `/v1/namespaces/{namespace}/tasks/{taskId}/artifacts/{sha256}` | S | `downloadTaskArtifact` |
| 53 | GET | `/v1/namespaces/{namespace}/deployments` | S | `listDeployments` |

### 4.2 `api/events.yaml`（5）

| # | METHOD | path | security | operationId |
|---|---|---|---|---|
| 1 | GET | `/v1/namespaces/{namespace}/events` | S | `listEvents` |
| 2 | GET | `/v1/namespaces/{namespace}/traces/{traceId}` | S | `getTrace` |
| 3 | GET | `/v1/console/events` | S | `listConsoleEvents` |
| 4 | GET | `/v1/stats` | S | `getStats` |
| 5 | GET | `/v1/orphans` | S | `listOrphans` |

### 4.3 `api/agent.yaml`（4）

| # | METHOD | path | security | operationId |
|---|---|---|---|---|
| 1 | GET | `/v1/node/stream` | N | `nodeStream` |
| 2 | GET | `/v1/node/binary` | N | `getNodeBinary` |
| 3 | GET | `/v1/node/capabilities/{digest}` | N | `getNodeCapabilityContent` |
| 4 | PUT | `/v1/node/tasks/{taskId}/artifacts/{sha256}` | N | `uploadTaskArtifact` |

### 4.4 `api/ingress.yaml`（5）

| # | METHOD | path | security | operationId |
|---|---|---|---|---|
| 1 | POST | `/v1/namespaces/{namespace}/events` | K | `ingestEvent` |
| 2 | POST | `/v1/hooks/{token}` | — | `fireHook` |
| 3 | POST | `/v1/namespaces/{namespace}/sources/{name}/fire` | S | `fireSource` |
| 4 | POST | `/v1/namespaces/{namespace}/sources/dry-run` | S | `previewInlineSource` |
| 5 | POST | `/v1/namespaces/{namespace}/sources/{name}/dry-run` | S | `previewSource` |

### 4.5 `api/auth.yaml`（58）

| # | METHOD | path | security | operationId |
|---|---|---|---|---|
| 1 | POST | `/v1/auth/register/options` | — | `registerOptions` |
| 2 | POST | `/v1/auth/register` | — | `register` |
| 3 | POST | `/v1/auth/login/options` | — | `loginOptions` |
| 4 | POST | `/v1/auth/login` | — | `login` |
| 5 | POST | `/v1/auth/logout` | — | `logout` |
| 6 | POST | `/v1/auth/recovery/login` | — | `recoveryLogin` |
| 7 | POST | `/v1/auth/password/login` | — | `passwordLogin` |
| 8 | POST | `/v1/auth/password/register` | — | `passwordRegister` |
| 9 | GET | `/v1/auth/me` | S | `getMe` |
| 10 | GET | `/v1/auth/sessions` | S | `listAuthSessions` |
| 11 | GET | `/v1/auth/passkeys` | S | `listPasskeys` |
| 12 | POST | `/v1/auth/passkeys/register/options` | S | `passkeyRegisterOptions` |
| 13 | POST | `/v1/auth/passkeys/register` | S | `passkeyRegister` |
| 14 | PATCH | `/v1/auth/passkeys/{id}` | S | `renamePasskey` |
| 15 | DELETE | `/v1/auth/passkeys/{id}` | S | `deletePasskey` |
| 16 | GET | `/v1/auth/password` | S | `getPasswordStatus` |
| 17 | POST | `/v1/auth/password` | S | `setPassword` |
| 18 | DELETE | `/v1/auth/password` | S | `deletePassword` |
| 19 | POST | `/v1/auth/recovery/regenerate` | S | `regenerateRecoveryCodes` |
| 20 | GET | `/v1/auth/recovery/status` | S | `getRecoveryStatus` |
| 21 | GET | `/v1/vapid-public-key` | — | `getVAPIDPublicKey` |
| 22 | GET | `/v1/devices` | S | `listDevices` |
| 23 | POST | `/v1/devices` | S | `upsertDevice` |
| 24 | PATCH | `/v1/devices/{id}` | S | `patchDevice` |
| 25 | DELETE | `/v1/devices/{id}` | S | `deleteDevice` |
| 26 | GET | `/v1/keys` | S | `listAPIKeys` |
| 27 | POST | `/v1/keys` | S | `createAPIKey` |
| 28 | POST | `/v1/keys/{id}/revoke` | S | `revokeAPIKey` |
| 29 | GET | `/v1/keys/self` | K | `getKeySelf` |
| 30 | POST | `/v1/cli-auth/sessions` | — | `createCLIAuthSession` |
| 31 | GET | `/v1/cli-auth/sessions/{id}/poll` | — | `pollCLIAuthSession` |
| 32 | GET | `/v1/cli-auth/sessions/{id}/qr.txt` | — | `getCLIAuthQRCode` |
| 33 | GET | `/v1/cli-auth/sessions/by-code` | S | `findCLIAuthSessionByCode` |
| 34 | GET | `/v1/cli-auth/sessions/{id}` | S | `getCLIAuthSession` |
| 35 | POST | `/v1/cli-auth/sessions/{id}/approve` | S | `approveCLIAuthSession` |
| 36 | POST | `/v1/cli-auth/sessions/{id}/deny` | S | `denyCLIAuthSession` |
| 37 | POST | `/v1/passkey-enroll/sessions` | S | `createPasskeyEnrollSession` |
| 38 | GET | `/v1/passkey-enroll/sessions/by-code` | — | `findPasskeyEnrollSessionByCode` |
| 39 | GET | `/v1/passkey-enroll/sessions/{id}` | — | `getPasskeyEnrollSession` |
| 40 | POST | `/v1/passkey-enroll/sessions/{id}/request` | — | `requestPasskeyEnrollment` |
| 41 | GET | `/v1/passkey-enroll/sessions/{id}/poll` | — | `pollPasskeyEnrollSession` |
| 42 | POST | `/v1/passkey-enroll/sessions/{id}/complete` | — | `completePasskeyEnrollment` |
| 43 | GET | `/v1/passkey-enroll/sessions/{id}/qr.txt` | — | `getPasskeyEnrollQRCode` |
| 44 | GET | `/v1/passkey-enroll/sessions/{id}/watch` | S | `watchPasskeyEnrollSession` |
| 45 | POST | `/v1/passkey-enroll/sessions/{id}/approve` | S | `approvePasskeyEnrollment` |
| 46 | POST | `/v1/passkey-enroll/sessions/{id}/deny` | S | `denyPasskeyEnrollment` |
| 47 | DELETE | `/v1/passkey-enroll/sessions/{id}` | S | `cancelPasskeyEnrollment` |
| 48 | GET | `/v1/admin` | A | `getAdminIndex` |
| 49 | GET | `/v1/admin/stats` | A | `getAdminStats` |
| 50 | GET | `/v1/admin/users` | A | `listAdminUsers` |
| 51 | GET | `/v1/admin/users/{id}` | A | `getAdminUser` |
| 52 | PATCH | `/v1/admin/users/{id}` | A | `patchAdminUser` |
| 53 | PATCH | `/v1/admin/users/{id}/role` | A | `changeAdminUserRole` |
| 54 | POST | `/v1/admin/users/{id}/disable` | A | `disableAdminUser` |
| 55 | POST | `/v1/admin/users/{id}/enable` | A | `enableAdminUser` |
| 56 | GET | `/v1/admin/messages` | A | `listAdminMessages` |
| 57 | GET | `/v1/admin/sessions` | A | `listAdminSessions` |
| 58 | GET | `/agent-login.sh` | — | `getAgentLoginScript` |

## 5. 已知缺口与裁决（写进 PR，供协调者裁决）

1. ~~**`GET /v1/node/capabilities/{digest}` 实现缺失**~~ → **已补齐，不再计入 SAFE_BLOCKED**：
   - **已由 `fix/node-capability-content` 补齐（81e4ce2）**：handler 在 `internal/server/node_capability.go`
     （`registerNodeCapabilityContent` 注册 GET 本体 + 非 GET 的显式 405 两条 pattern），
     `mux.go` 单点接线；spec 落 `agent.yaml` 的 `pathItems/NodeCapabilityContent`，
     operation = `getNodeCapabilityContent`（security = `nodeToken`）。
   - 原缺口事实（留档）：契约见 `api-conventions.md:813`、`internal/protocol/deploy_frames.go:8`；
     节点侧 `cmd/anode/internal/capability/fetch.go:30` 早已在调用，中枢侧当时无 handler。
   - 当时裁决「不写进 `agent.yaml`」的依据是本 issue 的规则（以代码真实实现为准生成 spec + parity 禁止
     「spec 有、handler 无」），故当时列为 SAFE_BLOCKED；实现落地后该豁免自动失效，现已写入 spec 并由 parity 实测。
2. **三套非 Status 错误体是保留域的实现事实**（不是遗漏）：`AuthError`（**身份域全部 4xx/5xx**，
   `writeErr` 与中间件都是 `{"error":...}`）、`NodeUploadError`（A1 封闭 4 码）、
   `BackpressureError`（snake_case 503）。已在 spec 里显式建模为具名族，
   由 `check_openapi.py` 强制「错误响应必须有具名组件且名字与状态码对齐」。
   → 若产品要统一成 `Status`，属 handler 改造（不在 #333 文件所有权内），本 issue 只如实描述。
3. **`GET /v1/namespaces/{name}/stats` 归 `resource.yaml`**（Namespace 子资源），
   而「事件/任务聚合」`GET /v1/stats`、`GET /v1/orphans` 归 `events.yaml`。
   依据：`API-SPEC-HANDOFF.md` 把「Namespace 的管理和只读状态」给 resource、「事件/任务聚合 stats」给 events。
7. **`GET …/capabilities/{name}?version=` 不生效（实现事实，已从 spec 移除该参数）**：
   `internal/server/resource_capability.go` 的读端点只用 `newestVersionOf`，全文件没有
   `Query().Get("version")`；按版本读要用 `GET …/capabilities/{name}/versions/{version}`。
   → spec 不再声明 `version` 参数（声明一个实现忽略的输入比不写更糟）。若产品要恢复该参数，
   owner = capability 域（本 issue 不改 handler）。
8. **`GET /v1/node/stream` 的鉴权发生在 WS 升级后的 hello 帧**（`internal/ws/hub.go`），
   不是 HTTP 中间件；spec 记 `nodeToken` 并在描述里写明「token 在首帧 hello 校验」，parity 测试对该条走
   例外期望（401 不在升级前产生）。
5. **`/health`、`/debug/pprof/*`、静态兜底**不在五份 spec（显式排除清单在 `check_openapi.py`）。
6. ~~**sitegen 单文件读取器**~~ → **已由 #345 解决**：`internal/sitegen/openapi.go` 现按五份 canonical
   spec 读（多 path + 跨文件/链式 `pathItems` `$ref` 解析，第六源/remote/绝对路径一律拒绝），
   `cmd/sitegen/main.go` 随之改为读 spec 目录。不再有「`log.Fatalf` 或静默丢 operation」的缺口。
   剩余只读边界不变：本清单仍是 #335 的输入，`make fe` / `make ci` 的产物样式归前端 issue。

## 6. 门禁（`make contract` 现有入口，无需改 Makefile）

| 命令 | 职责 |
|---|---|
| `python3 scripts/check_contract.py` | schema.sql 不变量（A2/A4/A5/A6 v4 边界 + A7 节点不持能力归属 + A8 上传授权/registry 边界），并串接 `check_envelope.py` |
| `python3 scripts/check_openapi.py` | exact-five、归属（method+canonical path）、operationId 唯一、跨文件/`pathItems`/禁止 remote·绝对·逃逸 ref、security 方案、错误响应具名族（含自测） |
| `python3 scripts/check_envelope.py` | 信封/status 只读/camelCase/`[]`/cursor/CAS/writeOnly/错误 envelope 的字段级机械断言（含自测） |
| `go test ./internal/server -run TestOpenAPIParity` | 真实 mux/handler 双向、security 实测、DTO 双向字段、跨文件 `$ref`、负向变异（含 `[no tests to run]` 假绿守卫） |
