# issue #331 · 冻结实施契约（cent-331）

base = `b30e998767544f475363815e5c03037cf9de0a1f` (= origin/v4/integration)
branch = `issue/331-http-resources`

权威：`COMMON.md` > issue 原文 `V4-01-http-resources.md` > `G3-HTTP-SPEC-RESUME-HANDOFF.md`(D1/D2/D6) >
`WIRE-HANDOFF.md`(W1–W6) > `S1`/`E1` > `docs/design/v4/api-conventions.md`（派生）> 旧代码注释。

## D0 范围决定（本 issue 冻结）

| # | 决定 | 理由 |
|---|---|---|
| D0.1 | `internal/server` 的 v3 扁平资源 handler 一律**具名退役**（删文件，逐 Test 名迁移/退役写入 `docs/v4/g3a-test-inventory.md`），保留域（auth/admin/keys/devices/cli-auth/passkey/notify/push/backpressure/static/health/pprof）不动 | G3-D2「不回兼容旧 flat routes/kinds/ports/edgeCRUD/执行命令」 |
| D0.2 | 旧中央执行域（`internal/executor`、`ws.NewNodeHandler`、`notify.phone` builtin 播种）**删除不 stub** | D2 刻意退役 |
| D0.3 | `internal/broker` 已不存在：背压沿用 server 私有 `backpressureGuard`（`OldestPendingCreatedAt` 窄接口），**不新建包** | 现状事实 |
| D0.4 | Task 取消/放弃沿用 `store.RequestCancel`（store 无 `CancelTask`） | 现状事实 |
| D0.5 | 列表：资源列表用 `limit`（无 cursor，`store.ListTasks` 是 OFFSET 分页）；只有 `/v1/console/events` 与 `tasks/{name}/emissions` 用不透明 `continue`（绑 ns + 筛选） | W6 / api-conventions §3.3 |
| D0.6 | Node 是 cluster-scoped `/v1/nodes`；token 轮换 = `store.NewNodeToken()` + `store.SetNodeTokenHash()`，明文只回一次 | W4 |
| D0.7 | 不新增 `internal/resultspool/**`（#332 owner） | 331 卡文件所有权 |
| D0.8 | `cmd/**` 编译失败属 `#334`：只写报告，不改 `cmd/` | 331 卡 |
| D0.9 | 不新增 store 底层入口；缺签名只报告 | 331 卡 |

## D1 共享骨架（centurion 写，worker 只读，禁止改）

文件：`internal/server/api_deps.go`、`api_status.go`、`api_envelope.go`、`api_cas.go`、`api_cursor.go`

```go
// api_deps.go
type serverDeps struct {
    DB      *store.DB
    Router  *route.Router
    Sources *source.Service
    Hub     *ws.Hub
    Sweeper *lease.Sweeper
    Log     *slog.Logger
    Now     func() int64
}
func (d *serverDeps) now() int64

// api_status.go
type statusDoc struct {          // 统一错误体（api-conventions §4.1）
    APIVersion string        `json:"apiVersion"` // "anotify/v1"
    Kind       string        `json:"kind"`       // "Status"
    Status     string        `json:"status"`     // "Failure"
    Code       int           `json:"code"`
    Reason     string        `json:"reason"`
    Message    string        `json:"message,omitempty"`
    Details    *statusDetails `json:"details,omitempty"`
}
type statusDetails struct {
    Causes    []statusCause `json:"causes,omitempty"`
    Current   map[string]any `json:"current,omitempty"`
    RetryAfterSeconds int     `json:"retryAfterSeconds,omitempty"`
}
type statusCause struct { Reason, Message, Field string } // json: reason/message/field
func writeStatus(w http.ResponseWriter, code int, reason, message string, details *statusDetails)
// writeAPIError(w, err) 把 store/领域 sentinel 映射成 Status；未知错误 → 500 InternalError
func writeAPIError(w http.ResponseWriter, err error)
// 参与映射的 sentinel（worker 的新 sentinel 必须注册进这里，用 apiError{code,reason,msg}）
type apiError struct { Code int; Reason, Message string }
func (e *apiError) Error() string
func errStatus(code int, reason, message string) error   // 领域 handler 用

// api_envelope.go
type objectMeta struct {
    Name, Namespace, UID string
    Version *int
    ResourceVersion string
    CreationTimestamp string
    DisplayName string
}
type apiEnvelope struct { APIVersion, Kind string; Metadata objectMeta; Spec any; Status any }
type listMeta struct { Continue string `json:"continue,omitempty"` }
type apiList struct { APIVersion, Kind string; Metadata listMeta; Items any }
func newEnvelope(kind string, meta objectMeta, spec, status any) apiEnvelope
func newList(kind string, meta listMeta, items any) apiList
func writeEnvelope(w http.ResponseWriter, code int, v any)   // 保证 Items nil → []
// decodeWriteRequest 严格解码写请求（未知顶层字段 → 400 unknown_field；带 status → 400 status_not_writable）
//   body 顶层只允许 apiVersion/kind/metadata/spec；metadata 只允许白名单键。
func decodeWriteEnvelope(r *http.Request, wantKind string, specDst any, metaKeys ...string) (objectMeta, error)
func writeList(w http.ResponseWriter, code int, kind string, meta listMeta, items any)

// api_cas.go
func casTokenFromQuery(r *http.Request) (string, error)   // 缺失 → 400 resource_version_required
func casTokenFromMeta(m objectMeta) (string, error)       // 同上（PUT/PATCH body）
func writeCASError(w http.ResponseWriter, err error) bool // store CAS 族 → Status；true=已写
func metaVersion(v int) *int

// api_cursor.go  (W6: cursor 绑 ns + 筛选条件)
func encodeCursor(scope, filter, id string, at int64) string
func decodeCursor(raw, scope, filter string) (id string, at int64, err error) // 不匹配 → 400 无 detail 泄露
```

## D2 wire 形状（逐 kind，来源 api-conventions §7 / W1–W6）

- 对象恒四键 `apiVersion/kind/metadata/spec/status`；无事实 `status` 给 `{}`；空列表 `[]`；JSON camelCase。
- `metadata.resourceVersion` = `store.ResourceVersion(uid, revision)`（store 已有，明文 `rv1:<uid>:<rev>`；HTTP 不解析、只透传与比较）。
- CAS：
  - `PUT`/`PATCH` 已有资源 → 令牌只在 body `metadata.resourceVersion`；缺失 400 `resource_version_required`，过期 409 `resource_version_conflict` + `details.current.resourceVersion`。
  - `DELETE` → 无 body；令牌只在 `?resourceVersion=`；缺失 400、冲突 409（同上）、不存在 404。
  - `POST` 创建与 action POST（deploy/install/render/fire/tokens/cancel/abandon/dry-run）不套 CAS。
- capabilities：`GET …/capabilities`（List）、`POST`、`GET/PATCH/DELETE …/capabilities/{name}`、`GET …/capabilities/{name}/versions`（`CapabilityList`，`metadata.version` 降序，完整信封）、`GET …/capabilities/{name}/versions/{version}`、`POST …/capabilities/{name}/deploy`（body `{version?, nodes?}` → deployments 事实）。
- sources：`GET/POST …/sources`、`GET/PATCH/DELETE …/sources/{name}`、`POST …/sources/{name}/fire`、`POST …/sources/{name}/dry-run`、`POST …/sources/dry-run`、`POST /v1/hooks/{token}`、`POST …/events`（投递入口，非资源创建）。GET 不回显 `config.token/secret`，给 `config.hookUrl/secretSet` + `status.hookUrl`。
- playbooks：`GET/POST …/playbooks`、`GET/PUT/DELETE …/playbooks/{slug}`、`POST …/playbooks/{slug}/render`、`POST …/playbooks/{slug}/install`、`GET …/playbooks/{slug}/installs`、`GET …/playbooks/{slug}/installs/{installId}`。`spec.files` 与 `spec.source` 互斥；`POST` 同 slug → 409 `slug_taken`；`PUT` 要 `metadata.resourceVersion`；`GET ?includeBody=true` 才回 `spec.files`；install 冲突 → 400 `playbook_conflict` + `details.valuesDigest/conflicts[]/hint`。
- nodes（cluster）：`GET/POST /v1/nodes`、`GET/PATCH/DELETE /v1/nodes/{name}`、`POST /v1/nodes/{name}/tokens`（`NodeToken` 动作文档）、`GET/PUT /v1/nodes/{name}/config`（wrapper）、`POST /v1/nodes/config/batch`、`POST /v1/nodes/{name}/drain`、`POST /v1/nodes/{name}/upgrade`。
- namespaces（cluster）：`GET/POST /v1/namespaces`、`GET/PATCH/DELETE /v1/namespaces/{name}`、`GET/PUT /v1/namespaces/{name}/config`、`GET /v1/namespaces/{name}/stats`、`GET/PUT /v1/system/config`。
- tasks：`GET …/tasks`（limit/phase/trace/node/capability）、`GET …/tasks/{name}`、`POST …/tasks/{name}/cancel`、`POST …/tasks/{name}/abandon`、`GET …/tasks/{name}/logs?tailLines&sinceSeconds&previous`、`GET …/tasks/{name}/emissions?limit&continue`。Task 不可 POST/DELETE；`spec` 不可变。
- events/traces：`GET …/events`、`GET …/traces/{traceId}`、`GET /v1/console/events?namespace=`（EventList + 不透明 continue，绑 ns+filter）。
- 例外文档（不套四键）：InstallReceipt / config wrapper / NodeToken / logs / emissions / preview(fire dry-run) / `/v1/node/capabilities/{digest}` / 读模型。

## D3 文件所有权（同一时刻唯一 writer）

| worker | 可写文件 | 内容 |
|---|---|---|
| centurion | `api_deps.go` `api_status.go` `api_envelope.go` `api_cas.go` `api_cursor.go` `namespace_scope.go` `docs/v4/g3a-test-inventory.md` `.pi-legion/**` | 共享骨架、作用域 helper、inventory 文档、集成修复 |
| W-asm | `mux.go` `config.go` `agent_binary.go` `node_upgrade_handlers.go` `internal/api/events.go` `internal/api/events_test.go` | 装配与适配器 |
| W-cap | `resource_capability.go` `resource_capability_test.go` | capability + deploy |
| W-src | `resource_source.go` `resource_source_test.go` | source + ingress + hooks + fire/dry-run |
| W-pb | `resource_playbook.go` `resource_playbook_test.go` | playbook |
| W-node | `resource_node.go` `resource_node_test.go` | node + tokens + node config |
| W-ns | `resource_namespace.go` `resource_namespace_test.go` | namespace + ns/system config + stats |
| W-task | `resource_task.go` `resource_task_test.go` | task + logs + emissions + events/traces + console events |

**只读（越界即拒绝）**：`internal/store/**`、`internal/{source,route,lease,ws,protocol,artifact,schema,sourcespec,playbook,logging,push}/**`、`cmd/**`、`api/**`、`Makefile`、`web*`、`scripts/**`、其他 worker 的文件、`internal/server` 保留域文件（`auth_handlers.go` `admin_handlers.go` `cli_auth_handlers.go` `passkey_enroll_handlers.go` `handlers.go` `backpressure.go` `ratelimit.go` `middleware.go` `static.go` `qr.go` `embed.go` `version_seam.go` `adapter.go` `gzip.go`）—— 需改动时在 verdict 里报告，由 centurion 裁决。

## D4 已退役文件（centurion 已 git rm；旧实现参考 `git show b30e998:<path>`）

handler：`console_handlers.go` `source_handlers.go` `playbook_handlers.go` `deploy_handlers.go` `namespace_handlers.go` `task_logs_handlers.go` `emission_handlers.go`
测试：`console_test.go` `console_handlers_test.go` `console_events_page_test.go` `console_downstreams_test.go` `console_edge_literals_test.go` `console_edge_refs_test.go` `capability_artifact_test.go` `capability_batch_test.go` `capability_links_test.go` `capability_run_workdir_test.go` `capability_spec_p1b2_test.go` `capability_spec_p4_test.go` `deploy_test.go` `nodes_batch_test.go` `namespace_handlers_test.go` `source_handlers_test.go` `source_entry_edge_test.go` `playbook_handlers_test.go` `task_logs_test.go` `task_logs_wire_test.go` `emission_handlers_test.go` `dispatch_budget_test.go` `node_create_test.go` `wsfixture_test.go`

保留测试（必须继续通过）：auth/admin/keys/devices/cli-auth/passkey/password/recovery/backpressure/gzip/static/qr/logging_ac/status_writer/middleware_latency/embed/config/config_stats/hook_ratelimit/merge_fastpath/openapi_parity/vocabulary/task_out_retired/zz_spec_fixture/postclose_guard/auth_cache_wiring。

## D5 验收（exit code 必须真实）

见 331 卡「验收」全部 7 条 + `docs/v4/g3a-test-inventory.md` 交付 + fresh review 无 critical/major。
