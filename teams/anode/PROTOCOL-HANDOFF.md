# PROTOCOL-HANDOFF · V4-ANODE N1/E1 共享协议冻结（唯一最终版）

- 任务：V4-ANODE-N1 + 父批准 E1（emit WS 传输）；团队契约 `.pi-legion/contract.md` rev3；父权威 `.pi-orchestrator/v4/CONTRACT.md` rev2 §7、`/tmp/anotify-v4-emit-ws-approval.md`
- 状态：**已冻结，交父转 T1**。本文件是唯一最终交接；**T1 只消费 179e999 + 本文件**，不要消费 caecd9d 的旧快照（N1 初版，缺 E1 两帧与 `NodeStreamPath`）
- 唯一作者：T3/N1 worker；`internal/protocol/**` 从此冻结，后续缺口先报父协调，不得各自改字段
- 支持快照：父授权纯支持 commit **f7e9a6c**（来源 T1 `ba21a89` 的 protocol/artifact，非 T3 原创）；`internal/artifact/**` 本任务只读、未改
- 产品提交（全部只动 `internal/protocol/`）：
  - **caecd9d** `feat(protocol): 冻结 N1 共享协议（env 常量/机器配置 hash/部署两帧）`（N1 初版）
  - **179e999** `feat(protocol): 补齐 E1 emit 转发两帧与节点 WS 端点常量`（**最终协议**：本文件描述的完整快照）
- 纯 protocol 补丁（**外部导出，不入库**；生成物不再提交到生产 repo）：

| 补丁 | 基准 | 目标 | SHA256 |
|---|---|---|---|
| `/tmp/anotify-v4-anode-final/protocol-only.patch` | `f7e9a6c` | `179e999`（累计，仅 `internal/protocol/`） | `3c7d7f3761ba5eac129bb0a2b0e7a079a4aba351e48e3f0943e466f5e1aa01ed` |
| `/tmp/anotify-v4-anode-final/protocol-emit.patch` | `caecd9d` | `179e999`（E1 增量，仅 `internal/protocol/`） | `738bbdd8531e50156b404e31df14051ac45bbef1f229a83cb3ead6974973bb69` |

`/tmp/anotify-v4-anode-final/SHA256SUMS` 覆盖两个补丁。两个补丁都已做 `git apply --check` + `git apply` + 与 `179e999` 的 `internal/protocol/` **逐字节相同**校验（详见 `teams/anode/E1-CLOSE.md`）。原跟踪文件 `teams/anode/protocol-only.patch` 已移除：补丁正文含 diff 上下文（`空格+tab`），留在工作树会被生产 checker 的 `git diff --check` 误报；生成物迁移到 `/tmp`，repo 内只保留本说明与 SHA256。

帧总数：**23** = 配置 5 + 执行 10 + 运维 4 + 人工部署 2 + emit 转发 2。

---

## 1. 任务环境契约变量与参数归一（env.go，caecd9d）

```go
const (
    EnvEmitEndpoint = "ANOTIFY_EMIT_ENDPOINT" // 协议+地址+端口，脚本拼 /emit
    EnvTaskToken    = "ANOTIFY_TASK_TOKEN"    // Authorization: Bearer <token>
    EnvTaskID       = "ANOTIFY_TASK_ID"       // 只读，日志关联
    EnvParamPrefix  = "ANOTIFY_PARAM_"        // 参数变量前缀
)

func ParamEnvName(name string) string
```

`ParamEnvName` 规则（与 artifact 私有 `paramEnvName` **逐字一致**，见 `internal/artifact/artifact.go`）：

1. `a`-`z` 抬成 `A`-`Z`；`A`-`Z` / `0`-`9` 原样；
2. 其余**每个 rune** 换成一个 `_`（非 ASCII 多字节 rune 仍只算一个 `_`）；
3. **不插 camel 大小写边界**：`sha→SHA`、`repo-url→REPO_URL`、`repoUrl→REPOURL`、`参数→ANOTIFY_PARAM___`；
4. 输出 = `EnvParamPrefix + 归一结果`，不返回 error。

撞名（`sha`/`SHA`/`Sha`、`repo-url`/`repo.url`/`repo_url`）是**预期**：本函数不消歧、不报错；写入期由 artifact 用**同一个函数**检测并拒绝 `param_env_collision`（capability.md §2.3）。T1 后续把 artifact 的私有函数替换为本函数即可（artifact 当前只读，由 T1 在其闭包改）。

## 2. 机器配置 hash（hash.go，caecd9d）

```go
func MachineConfigHash(vars map[string]string, limits Limits) (string, error)
```

canonical 文档（**只有** `vars` / `limits` 两个顶层键；两级 map 键由 `encoding/json` 按字节序排序）：

```json
{"vars":{"A":"1","B":"2"},"limits":{"total":8,"perCapability":{"*":1,"review":2}}}
```

归一与形状：

- `limits.Total == 0` → 缺省 `DefaultLimitsTotal`（=1）；`total < 0` → error（非法值不静默归一）；
- nil / 空 `vars` → `{}`；nil / 空 `perCapability` → `{}`（`{}` 不是 null、不是省略）；
- 输出 `sha256:<64 hex>`（与 artifact digest 同前缀）；
- **不含** revision / namespace 默认 / token / 宿主凭证 / envPassthrough；
- 两侧（中枢算期望值、节点算 appliedHash）只调用这一份实现，禁止第二份 hash 算法。

golden 值（T1 可交叉验证；`TestMachineConfigHashGolden` 同时用手写 canonical 字符串独立复算）：

| 输入 | canonical | hash |
|---|---|---|
| `vars=nil, limits=Limits{}` | `{"vars":{},"limits":{"total":1,"perCapability":{}}}` | `sha256:7f8ef08104bf2220506e2a2f35f735dff34bd66ee19637c637465be8a38785bd` |
| `vars={"A":"1"}, limits={Total:2}` | `{"vars":{"A":"1"},"limits":{"total":2,"perCapability":{}}}` | `sha256:04a4070924fad973f3813be4317b0bc268d107b7fb5f0494f71a9b68581bc8f1` |
| `vars={"A":"1","B":"2"}, limits={Total:8, perCapability{"*":1,"review":2}}` | `{"vars":{"A":"1","B":"2"},"limits":{"total":8,"perCapability":{"*":1,"review":2}}}` | `sha256:80fa17cfdaa7c73612f7d349c7231d5acbe40b5a367fdb39cb91d23d248bee68` |

注：`encoding/json` 默认 HTML 转义（`<`→`\u003c`、`&`→`\u0026`）参与 canonical 字节，已用 golden 固定；两侧同实现即可，不要求跨语言。

## 3. 人工部署两帧（deploy_frames.go，caecd9d）

```go
const (
    FrameCapabilityDeploy   = "capability.deploy"   // 中枢 → 节点（下行）
    FrameCapabilityDeployed = "capability.deployed" // 节点 → 中枢（上行）
)

type DeployRefs struct {              // 两帧匿名嵌入，wire 上平铺
    Namespace string `json:"namespace"`
    Name      string `json:"name"`
    Version   int    `json:"version"`
    Digest    string `json:"digest"`
}

type CapabilityDeployFrame struct {
    Type      string `json:"type"`
    RequestID string `json:"requestId"`
    DeployRefs
}

type DeployStatus string
const (
    DeployStatusApplied DeployStatus = "applied"
    DeployStatusFailed  DeployStatus = "failed"
)
func (s DeployStatus) Valid() bool

type CapabilityDeployedFrame struct {
    Type      string       `json:"type"`
    RequestID string       `json:"requestId"`
    DeployRefs
    Status    DeployStatus `json:"status"`
    Error     string       `json:"error,omitempty"` // 仅 failed
}
```

wire 键快照：

- deploy：`digest,name,namespace,requestId,type,version`
- deployed（failed）：`digest,error,name,namespace,requestId,status,type,version`；applied 时**省略** `error`

语义边界：WS 帧只传 refs（帧有 1 MiB 软上限），节点用自身节点身份 Bearer `GET /v1/node/capabilities/{digest}` 取完整 Artifact（见 §5）；人工部署不是自收敛——离线节点重连后由中枢重放**已明确请求**的未完成部署，不新增帧、不做通用 reconciler。`status` 封闭 `applied|failed`；`error` 只是人话，不参与判断。

## 4. emit 中转两帧（emit_frames.go，179e999）

父批准全文：`/tmp/anotify-v4-emit-ws-approval.md`。这是**唯一**的 WS emit transport；不新增业务路由、不造 seq/去重模型。

```go
const (
    FrameTaskEmit    = "task.emit"     // 节点 → 中枢（上行）
    FrameTaskEmitAck = "task.emit.ack" // 中枢 → 节点（下行）
)

type TaskEmitFrame struct {
    Type         string `json:"type"`         // "task.emit"
    RequestID    string `json:"requestId"`    // RPC 关联（每次 HTTP emit 新生成）
    TaskID       string `json:"taskId"`
    Attempt      int    `json:"attempt"`
    LeaseID      string `json:"leaseId"`
    AttemptToken string `json:"attemptToken"`
    EmitRequest                        // 匿名嵌入 → to/id/data 平铺，与 HTTP 同解释
}

func (f *TaskEmitFrame) Validate() error

type TaskEmitAckFrame struct {
    Type       string        `json:"type"`              // "task.emit.ack"
    RequestID  string        `json:"requestId"`
    TaskID     string        `json:"taskId"`
    Attempt    int           `json:"attempt"`
    HTTPStatus int           `json:"httpStatus"`          // 200/400/401/409/500
    Response   *EmitResponse `json:"response,omitempty"`  // 仅成功（含剪枝）
    Error      *EmitError    `json:"error,omitempty"`     // 仅错误
}

func (f *TaskEmitAckFrame) Validate() error
func IsEmitErrorCode(code string) bool // EmitError 封闭码集合
```

`task.emit` 字段表：

| Go 字段 | wire | 必填 | 说明 |
|---|---|---|---|
| `Type` | `type` | 是 | `task.emit` |
| `RequestID` | `requestId` | 是 | 一次 RPC 的关联 id；**不是**业务幂等键/调用序号，不持久化 |
| `TaskID` | `taskId` | 是 | 中枢按它 + 连接节点身份做当前授权校验 |
| `Attempt` | `attempt` | 是 | ≥1 |
| `LeaseID` | `leaseId` | 是 | 当前租约；旧 attempt/过期租约拒绝 |
| `AttemptToken` | `attemptToken` | 是 | 节点转发本地预检过的令牌；**ack 不回显** |
| `EmitRequest.To` | `to` | 否 | 缺 = HTTP 语义 `missing_target` 400 / CLI 2，**不是**帧错误 |
| `EmitRequest.ID` | `id` | 否 | 唯一业务幂等键（同 task 内），跨 attempt 可重试 |
| `EmitRequest.Data` | `data` | 否 | 省略 = 空对象；给了必须是 JSON **对象**（数组/标量/null 拒绝） |

`task.emit.ack` 字段表：

| Go 字段 | wire | 必填 | 说明 |
|---|---|---|---|
| `Type` | `type` | 是 | `task.emit.ack` |
| `RequestID` | `requestId` | 是 | 与请求相同；节点按它找 waiter |
| `TaskID` | `taskId` | 是 | 节点核对后才交给原 HTTP 请求 |
| `Attempt` | `attempt` | 是 | 同上 |
| `HTTPStatus` | `httpStatus` | 是 | 成功固定 200；错误必须等于 `EmitHTTPStatus(error.Error)` |
| `Response` | `response` | 成功时 | 复用 HTTP `EmitResponse`（剪枝时 `taskId:null` + `pruned`），与 `error` 互斥 |
| `Error` | `error` | 错误时 | 复用 HTTP `EmitError`（封闭码），与 `response` 互斥 |

关键语义（T1/节点共同遵守）：

- **payload 只有一份解释**：to/id/data 来自匿名嵌入的 `EmitRequest`；HTTP 入口、CLI、WS 帧不各写一套。缺 to 仍是 `missing_target` 400 / CLI 2；非对象 data 是参数错误（`param_type` 剪枝或 400），不能默默接受数组/标量。
- **互斥**：成功**只** `response`、错误**只** `error`。没有 Status 信封；剪枝是成功 200（`taskId:null` + `pruned`），不是 4xx。
- **不回显 token**：ack 没有 `attemptToken`/`leaseId`/`namespace`；也不带 `data`/`to`/`id`。令牌不进第二条帧、不进日志。
- **namespace 不可调用方传**：帧里没有 namespace 字段；中枢按「任务的 namespace」与「节点申报 ∩ 授权」推导。未知字段（包括手工塞进来的 `namespace`）按共享 codec 纪律被忽略，无法覆盖权威推导。
- **授权与幂等是 T1 的**：中枢先验证连接节点身份、任务当前 running、授权 namespace、attempt/leaseId/attemptToken 与租约未过期；旧授权不能因业务 id 已有回执而绕过。接受、业务 id 判等/冲突（同 id 异 to/data → 409 `emit_id_conflict`）、建/剪枝下游必须同一权威事务/CAS 边界。`requestId` 只做 RPC 配对。
- **Validate 的边界**：`Decode` **不调用** Validate（坏 JSON 才是 `bad_frame`）；`TaskEmitFrame.Validate` 故意**不要求 to**（否则中枢无法回 HTTP 语义），只校验关联身份必填、attempt≥1、data 为对象。`TaskEmitAckFrame.Validate` 校验互斥、成功 200、错误码封闭且 `httpStatus == EmitHTTPStatus(code)`；未知码不是可信 ack（应走传输层「结果未知」，不伪造业务结论）。
- **本包不做运行器**：按 requestId 的有界 in-flight 等待表、30s 级 timeout、HTTP 上下文取消/断线清理、核对 taskId/attempt、丢弃迟到/不匹配 ack、超时后的未知结果（CLI 5）与「绝不自动重放无 id 请求」都是 N2 节点网关的实现；协议包只提供帧与校验。

## 5. 节点端点常量与 Artifact 获取（179e999）

```go
const NodeBinaryPath = "/v1/node/binary" // 升级包下载（GET，中枢根上）
const NodeStreamPath = "/v1/node/stream" // 节点 WS 连接（GET 升级，中枢根上）
```

- 两个常量并列在 `internal/protocol/ops_frames.go`；节点与中枢只消费共享常量，不各写一份字面量（`TestNodeStreamPath` 钉值）。
- 中枢侧：空 path 或 `/` **归一**为 `NodeStreamPath`；显式配置的 path（反代前缀）**原样使用**；不提供 legacy 别名。
- **Artifact GET（父已批准）**：节点用自身节点身份 Bearer `GET /v1/node/capabilities/{digest}`，**200 响应体直接是完整 `artifact.Artifact` JSON**（`apiVersion` / `kind` / `metadata` / `spec`，`spec` 含 `run.files`），**没有 wrapper、没有 status 信封**。节点客户端做 `artifact.Validate` + 重算 digest + refs（namespace/name/version/digest）校验后才缓存；T1 handler 负责节点身份、ns 交集、明确 deployment target 验证。本包里**没有**这条 GET 的共享常量/镜像类型：不要造中枢镜像，T1 直接复用 `internal/artifact.Artifact`。

## 6. codec / 方向表（179e999）

- `Decode` 统一按 `type` 分流，新增 `case FrameTaskEmit` → `*TaskEmitFrame`、`case FrameTaskEmitAck` → `*TaskEmitAckFrame`；未知 type 仍返回 `(*UnknownFrame, nil)`（不是断连）。
- `upstreamFrames` + `FrameTaskEmit`（节点 → 中枢）；`downstreamFrames` + `FrameTaskEmitAck`（中枢 → 节点）。
- 既有配置 5 帧、执行 10 帧、运维 4 帧、部署 2 帧的语义/字段**未改**；`EmitRequest`/`EmitResponse`/`EmitError`/`EmitHTTPStatus`/`EmitExitCode` 未改。
- 未知字段一律忽略（与全包既有纪律一致）：一条老/新帧不该因为多一个键就整帧被拒。严格性在**生产侧 wire 键快照**（不许多发字段）与两帧的 `Validate()`，不在 `Decode`。

## 7. 具名测试（全部在 `internal/protocol/`）

N1（caecd9d）：

- `env_test.go`：`TestEnvContractVariables`、`TestParamEnvNameUppercase` / `Digits` / `Symbols` / `NoCamelBoundary` / `Collisions`
- `hash_test.go`：`TestMachineConfigHashGolden` / `Defaults` / `CanonicalKeys` / `OrderInsensitive` / `ContentSensitive` / `NegativeTotal` / `Format`
- `deploy_frames_test.go`：`TestDeployFrameNamesAndDirection`、`TestCapabilityDeployFrameWire`、`TestCapabilityDeployedFrameWire`、`TestDeployStatus`、`TestDeployFramesShareRefs`

E1（179e999）：

- `emit_frames_test.go`：
  - `TestTaskEmitFrameNamesAndDirection`（wire 名 + 上/下行）
  - `TestTaskEmitFrameWire`（键快照 + 往返 + 可选 id/data 省略 + 无 namespace/node/seq）
  - `TestTaskEmitFrameEmbeddedEmitRequest`（HTTP body 键值逐字出现在 WS 帧）
  - `TestTaskEmitFrameUnknownFieldsIgnored`（未知 namespace/node/seq 被忽略、重编码不透传）
  - `TestTaskEmitFrameValidate`（requestId/taskId/attempt≥1/leaseId/attemptToken）
  - `TestTaskEmitNonObjectData`（数组/字符串/数字/bool/null 拒绝；缺 data 通过）
  - `TestTaskEmitMissingTargetKeepsHTTPSemantics`（缺 to 时 Decode/Validate 放行 + 400/CLI2）
  - `TestTaskEmitAckFrameWire`（成功/错误键快照、无 token/leaseId/namespace/seq）
  - `TestTaskEmitAckMutualExclusion`（response/error 同时/都不出现都拒绝）
  - `TestTaskEmitAckStatusMapping`（封闭错误码 ↔ 400/401/409/500；成功=200；错配/未知码拒绝）
  - `TestTaskEmitAckUnknownFieldsIgnored`（不回声 token/leaseId/namespace/seq）
  - `TestTaskEmitRequestIDIsCorrelationOnly`（requestId 与业务 id 分离；ack 无 id/seq）
  - `TestTaskEmitAckPrunedSuccess`（剪枝 200 + `taskId:null` + pruned 往返）
- `node_endpoints_test.go`：`TestNodeStreamPath`（`/v1/node/stream`，与 `NodeBinaryPath` 并列、不同、根相对）
- 更新：`TestFrameNamesAreWireValues`（23 帧全集 = 上行 ∪ 下行、互不相交）、`TestFrameRoundTripAndKeys`（新增 task.emit / task.emit.ack fixture）

## 8. 机械验证（`179e999` 上实跑，见 `teams/anode/E1-CLOSE.md` 全文）

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1
# ok internal/protocol / internal/artifact / cmd/anode/internal/{applied,capability,config,hostenv,hub,listen,node}（exit 0）

GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/protocol/... ./cmd/anode/...
# （无输出，exit 0）

git diff --check
# （无输出，exit 0）

gofmt -l internal/protocol/   # 空
```

补丁校验：两份 patch 分别在 `f7e9a6c` / `caecd9d` 的 `internal/protocol` 快照上 `git apply --check` + `git apply` PASS，应用后与 `179e999` 逐字节相同；`sha256sum -c SHA256SUMS` OK。

## 9. 缺口 / 待父协调（本包不私造）

1. **冻结设计文档缺章节**：`docs/design/v4/capability.md §8.5` 只有配置 5 帧 + 执行 10 帧；deploy 两帧只见于父 CONTRACT rev2 §7，emit 两帧只见于 `/tmp/anotify-v4-emit-ws-approval.md`。冻结设计不可由 T3 改；wire 以本 HANDOFF + 两份父权威为准，建议父/T1 在 T1 文档侧补节声明。
2. **hash 的 `total<0` 语义**：归一时负值报 error（不静默变 1）。正常配置在写入/启动校验期就应被拒（anode.md §2：`limits.total ≥ 1`），N2 配置解析负责；若父认为 hash 应无条件成功，需修订本函数（已过冻结需走协调）。
3. **`perCapability` 条目值不做 hash 侧校验**：hash 只归一缺省，`<1` 由 N2 配置校验拒绝；不在 hash 里再造一套校验规则。
4. **artifact 仍用私有 `paramEnvName`**：T1 后续闭包改为调用 `protocol.ParamEnvName`（同一实现）；`MachineConfigHash` 的期望值计算（T1 中枢侧）必须直接 import protocol。
5. **emit 网关运行时**（有界 in-flight、timeout、迟到 ack、CLI 5 未知结果）属于 N2；本包只交帧/校验/方向，不造转发运行器。

## 10. 未决 scope

- 本 HANDOFF 覆盖 N1 + E1（协议冻结）；N2B（grant 执行/lease/emit 网关/env/files/GC）、N3 均未开始，不属于本提交。
- 未改 Makefile / go.mod / api / store / ws / server / artifact / cmd/anode / 冻结设计 / settings/env / 其他 worktree。
