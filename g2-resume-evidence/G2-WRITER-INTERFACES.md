# G2 · Writer 公开运行期接口（供 G3 / 父集成）

- 分支：`pi-subagent/core-g2-resume-52fc54d4`
- 基线：`5441426`（父冻结卡）；source code commit：`e552bd3`（本文与 TEST-MAPPING G2 行在其后单独 commit）
- 所有权：本文件由 G2 source writer 写；**G3 只能消费，不要复制结构体/wire 镜像**。
- 共享 wire 的唯一事实源仍是 `internal/protocol`（T3/Node-log 补口后由父重新授权消费）；
  G2 不新增帧、不改 `LogsResponseFrame`、不建私有 DTO。

本文件列的是**实际编译通过的 Go 签名**（`go doc -all` 校验），不是计划稿。内部适配器
（如 `fixture_test`）不在公开面。

---

## 1. `internal/route` — 纯路由投影（无 WS、无写库）

```go
type Clock func() int64

func New(db *store.DB) *Router
func NewWithClock(db *store.DB, now Clock) *Router

func (r *Router) DB() *store.DB
func (r *Router) Now() int64

// 源事件 → 目标计划：src.Targets 逐行求值 $(payload.路径)；解析不到的能力记
// downstream_missing，参数缺失/类型不符按封闭词表剪枝。traceID 由调用方（source）给。
func (r *Router) PlanFromSource(ctx context.Context, src *store.Source, traceID string, payload map[string]any) (*Plan, error)

// 能力 emit → 目标计划：to 必须 ∈ 父任务钉住版本的 spec.next（否则 ErrNotDeclared）；
// 参数走同名直取（多余字段忽略、default 补齐、缺必填剪枝）。
func (r *Router) PlanFromEmit(ctx context.Context, parent *store.Task, to string, data json.RawMessage) (*Plan, error)

// 能力 emit 的唯一入口：VerifyAttemptToken 取父任务 → PlanFromEmit →
// store.EmitAuthorized（先授权后幂等回放）→ 回执。
func (r *Router) Emit(ctx context.Context, in EmitInput) (EmitResult, error)

// 参数/载荷校验（source 与 emit 共用同一份语义）。
func PrepareParams(schema json.RawMessage, input map[string]any) (json.RawMessage, *Prune)
func ValidatePayload(schema json.RawMessage, payload map[string]any) *Prune
func DecodePayload(raw json.RawMessage) (map[string]any, bool) // 非对象 → ok=false

type Prune struct {
    Capability string `json:"capability,omitempty"`
    Reason     string `json:"reason"` // 封闭词表
    Detail     string `json:"detail,omitempty"`
}

const (
    StateReady   = "ready"
    StatePruned  = "pruned"
    StateMissing = "missing"
)

type Target struct {
    Ref          string          `json:"ref"`
    Capability   string          `json:"capability,omitempty"`
    Version      int             `json:"version,omitempty"`
    Digest       string          `json:"digest,omitempty"`
    CapabilityID string          `json:"-"`
    Params       json.RawMessage `json:"params,omitempty"`
    State        string          `json:"state"`
    Reason       string          `json:"reason,omitempty"`
    Detail       string          `json:"detail,omitempty"`
}

type Plan struct {
    NamespaceID string
    TraceID     string
    Targets     []Target
    Pruned      []Prune
}

func (p *Plan) Ready() []Target
func (p *Plan) Tasks(parentTaskID string) []*store.Task // 一律 pending，无执行授权

type EmitInput struct {
    Auth   store.EmitAuth   // 会话构造：TaskID/NodeID/NodeTokenHash/Attempt/LeaseID/AttemptToken/Now
    To     string
    EmitID string          // 可选业务幂等键（同 task 域）
    Data   json.RawMessage
}

type EmitResult struct {
    Response  protocol.EmitResponse
    Duplicate bool          // 同 id 同请求的幂等回放
}

type EmitError struct{ Reason string; Err error }
func (e *EmitError) Error() string
func (e *EmitError) Unwrap() error
func (e *EmitError) HTTPStatus() int   // protocol.EmitHTTPStatus
func (e *EmitError) ExitCode() int     // protocol.EmitExitCode
func (e *EmitError) Body() protocol.EmitError
```

要用的 store 入口（只读消费）：
`ResolveRef`、`GetCapabilityByID`、`VerifyAttemptToken`（task.go:1150）、
`EmitAuthorized`（emission.go:122）、`InsertPruneEvent`（event.go:248）。

---

## 2. `internal/source` — 三入口统一接收（manual / webhook / cron 共用一份回执）

```go
func New(db *store.DB, router *route.Router) *Service
func NewWithClock(db *store.DB, router *route.Router, now Clock) *Service

func (s *Service) Now() int64
func (s *Service) Router() *route.Router

type Input struct {
    Source     *store.Source
    Type       string          // webhook 的 type；空 → 源名
    DeliveryID string          // 外部幂等键（webhook id）；空 = 不参与去重
    Payload    json.RawMessage // 投递体里的 payload；空 → 用配置打底
}

func (s *Service) Ingest(ctx context.Context, in Input) (*Receipt, error)
func (s *Service) Fire(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Receipt, error)
func (s *Service) Hook(ctx context.Context, token string, body HookBody) (*Receipt, error)

type HookBody struct{ Type, ID string; Payload json.RawMessage }

type TaskRef struct {
    TaskID     string `json:"taskId"`
    Capability string `json:"capability"`
    Version    int    `json:"version"`
}

type Receipt struct {
    EventID   string        `json:"eventId"`
    TraceID   string        `json:"traceId"`
    Duplicate bool          `json:"duplicate"`
    Tasks     []TaskRef     `json:"tasks"`  // 空 = []
    Pruned    []route.Prune `json:"pruned"` // 空 = []
}

type PreviewTarget struct {
    Capability string          `json:"capability"`
    Version    int             `json:"version,omitempty"`
    Params     json.RawMessage `json:"params,omitempty"`
    State      string          `json:"state"` // ready/pruned/missing
    Reason     string          `json:"reason,omitempty"`
    Detail     string          `json:"detail,omitempty"`
}
type Preview struct {
    Targets []PreviewTarget `json:"targets"`
    Pruned  []route.Prune   `json:"pruned"`
}

func (s *Service) Preview(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Preview, error)
func (s *Service) PreviewSource(ctx context.Context, src *store.Source, payload json.RawMessage) (*Preview, error)

var (
    ErrBadPayload     = errors.New("source: payload 必须是 JSON 对象") // 400，不产生事件
    ErrSourceDisabled = errors.New("source: 源已停用")                // webhook 对外仍是 404
)
```

- 停用来源：`Hook` 走 `store.LookupHook`（不存在/停用同一 `ErrSourceNotFound`），
  `Fire` 返回 `ErrSourceDisabled`。
- 载荷装配：cron/manual 以 `config.payload` 打底 + 本次输入一层覆盖；webhook 不读 `config.payload`。
- 试运行完全不落库（事件/任务/去重表/剪枝行/回执都不写）。

---

## 3. `internal/trigger` — cron 调度

```go
func New(db *store.DB, sources *source.Service, logger *slog.Logger) *Scheduler
func NewWithClock(db *store.DB, sources *source.Service, logger *slog.Logger, now func() int64) *Scheduler

type FireResult struct {
    SourceID, SourceName, NamespaceID string
    Receipt *source.Receipt
    Err     error
}

func (s *Scheduler) RunDue(ctx context.Context, now int64, limit int) ([]FireResult, error)
func (s *Scheduler) Run(ctx context.Context, tick time.Duration) error
```

下一次触发从**上次计划时刻**推算；Ingest 失败也推进（防热循环）并把错误放进 `FireResult.Err`；
`RunDue` 的第二个返回值只汇总「列源/推进失败」这类 sweep 级错误。

---

## 4. `internal/lease` — 巡检/保留/统计（真实 store）

```go
func NewSweeper(db *store.DB, logger *slog.Logger) *Sweeper

type Report struct {
    LeasesRequeued  int                  `json:"leasesRequeued"`
    LeasesAbandoned int                  `json:"leasesAbandoned"`
    Retention       store.RetentionStats `json:"retention"`
    StatsRefreshed  []string             `json:"statsRefreshed"`
}
func (s *Sweeper) RunOnce(ctx context.Context, now int64) (Report, error)
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) error
func (s *Sweeper) MarkDisconnected(ctx context.Context, nodeID string, now int64) error
func (s *Sweeper) RetentionConfig(ctx context.Context) (*store.SystemConfig, error)
func (s *Sweeper) TaskPhaseStats(ctx context.Context, namespaceID string) (*store.TaskStats, error)

type Occupancy struct{ Total, Busy, Free int }
func (s *Sweeper) OccupancyByNode(ctx context.Context, nodeIDs []string) (map[string]Occupancy, error)
```

涉及 store：`RecoverLostLeases`（不释放物理槽）、`RunRetention`、`MaintainStats`、
`FreeSlotsByNode`、`InstanceStatsByNode`、`TaskStats`、`MarkNodeOffline`。

---

## 5. `internal/ws` — 节点会话与执行/部署/日志编排

```go
func NewHub(db *store.DB, router *route.Router, opts ...Option) *Hub
func WithLogger(*slog.Logger) Option
func WithClock(Clock) Option
func WithHelloTimeout(time.Duration) Option
func WithWriteTimeout(time.Duration) Option

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) // GET 升级；路径用 protocol.NodeStreamPath
func (h *Hub) DB() *store.DB
func (h *Hub) Router() *route.Router
func (h *Hub) Now() int64
func (h *Hub) Online(nodeID string) bool

// 派发：只写 assignment/proposedAttempt/30s 窗口；不发 lease/token/slot。
// 返回 dispatched 条数；没有候选/无空槽的任务保持 pending。
func (h *Hub) DispatchOnce(ctx context.Context, limit int) (int, error)
func (h *Hub) RunDispatchLoop(ctx context.Context, interval time.Duration)

// 取消：先 RequestCancel；running 且在线才发带完整历史绑定的 task.cancel。
func (h *Hub) CancelTask(ctx context.Context, namespaceID, taskID, reason string, graceSec int) error

type DeployOutcome struct {
    Deployment *store.Deployment
    RequestID  string
    Sent       bool // false = 节点离线（pending 意图保留，重连重放）
}
func (h *Hub) Deploy(ctx context.Context, namespaceID, name string, version int, nodeIDs []string) ([]DeployOutcome, error)

type LogsResult struct {
    Frame      *protocol.LogsResponseFrame
    Empty      bool // previous 且确实没有上一 attempt（仅此场景 attempt 0）
    Structured bool // 恒 false：共享帧尚无 sequence/stream/text/at（父 pending）
}
func (h *Hub) RequestLogs(ctx context.Context, namespaceID, taskID string, attempt int, previous bool, timeout time.Duration) (*LogsResult, error)

func (h *Hub) RequestUpgrade(ctx context.Context, nodeID, targetVersion, sha256Hex, urlPath string) error

type Session struct{ /* 已认证连接；身份 hello 时固定 */ }
func (s *Session) NodeID() string
func (s *Session) NodeName() string
func (s *Session) Auth() store.NodeAuthorization // {NodeID, NodeTokenHash}
func (s *Session) Send(ctx context.Context, frame any) error

var (
    ErrNodeOffline     = errors.New("ws: 节点不在线")
    ErrNoAttempt       = errors.New("ws: 没有可定位的执行 attempt")
    ErrLogsTimeout     = errors.New("ws: 等待日志回包超时")
    ErrLogsCorrelation = errors.New("ws: 日志回包与请求不匹配")
    ErrSessionClosed   = errors.New("ws: 会话已关闭")
)
```

### 5.1 上行帧处理（全部经共享 `protocol.Decode` + `IsUpstream/IsDownstream`）

| 帧 | 处理 | 关键校验/事务 |
|---|---|---|
| `hello`（含 rehello） | `authenticate`（token hash → 当前 nodes 行，name 必须一致）→ `RecordHello` → online → `config.applied` 可选 appliedRevision/hash → 实例事实 → welcome → 重放 pending 部署 | 身份固定为 `NodeAuthorization{NodeID, TokenHash}`；rehello **不**释放 busy |
| `config.get` | 回 welcome（同形复用） | — |
| `config.applied` | `RecordApplied` + 实例快照 `UpsertInstances/ReclaimStaleSlots` | 只按明确 `busy=false + 匹配历史引用` 回收 |
| `task.accept` | 形状 `ValidateAccept`（slot presence/0 合法）→ 节点名一致 → `GrantLeaseAuthorized` → cap/attempt/token/expiresAt 与同一 slot 的 `lease.grant` | 身份轮换 → 断开；stale_config/slot_unavailable → 不 grant（store 已回 pending+退避） |
| `task.reject` | 封闭 reason（stale_config / slot_unavailable）→ `RejectAssignment` | 未知 reason 断开 |
| `lease.renew` | `RenewLeaseAuthorized` → `lease.ack`；`expiry == now` 拒绝 | 过期/取消/绑定不符不 ACK |
| `task.result` | 终态/节点名/attempt/lease 校验 → `CompleteTaskClassifiedAuthorized` → 仅 Recorded/Duplicate 回 `result.ack{accepted:true}` | generic error 不 ACK；不捏 token、不加延迟 |
| `cancel.ack` | `protocol.ValidateCancelAck` 先 → `AckCancelAuthorized(auth, ReapedInputs{task,attempt,node,tokenHash,lease,slot,now}, processGroupReaped)` | 错 lease/slot/缺历史绑定 fail closed；重复精确 ACK 幂等 |
| `task.emit` | 严格 Decode/Validate → `route.Emit`（auth 来自会话）→ `task.emit.ack`（200/400/401/409；非可判别错误不回 ack） | 先授权后幂等；`requestId` 只配对 |
| `logs_response` | 按 `requestId` + `taskId` + `attempt` 配对交付；不匹配丢弃/报错 | 晚到/串请求不会安错 |
| `capability.deployed` | ns slug → id、节点 ns 授权、refs 四元组 + `requestId == "dep_"+deploymentID` → `MarkDeployResult` | 未请求过的部署不落事实 |
| `upgrade_ack` | ok → `RecordAgentUpgrade`；失败 → `FailAgentUpgrade`（不改版本列） | 未知错误码归一为 "" |
| 未知帧 | 忽略并继续 | 方向错误/坏 JSON → 断开（无 T3 error 帧可回） |

### 5.2 下行帧

`welcome`、`task.dispatch`、`lease.grant`、`lease.ack`、`result.ack`、`task.cancel`、
`capability.deploy`、`logs_request`、`upgrade`、`task.emit.ack`。全部用 `protocol` 类型，
`ws` 不定义第二份 wire。

### 5.3 日志 HTTP 五态（G3 负责翻译，G2 只给可判别结果）

| G3 状态 | G2 信号 |
|---|---|
| 200 + `attempt:0` + 空数组 | `LogsResult.Empty == true`（`previous=true` 且无上一 attempt；仅此场景 attempt 0） |
| 404 任务不存在/跨 ns | `store.ErrNotFound`（`GetTask`） |
| 404 没有该 attempt | `ws.ErrNoAttempt`（未 grant / attempt 行不存在） |
| 400 非法查询 | G3 参数校验（`attempt < 0` 等；G2 只接受 `attempt>=0` 与 bool previous） |
| 503 节点离线 | `ws.ErrNodeOffline` |
| 504 超时 | `ws.ErrLogsTimeout` |
| 410 真实 attempt 但档案不在 | 帧 `Error == protocol.LogsErrNoLocalArchive`（原样转发，不冒充空成功） |
| 502 非法/错误绑定响应 | `ws.ErrLogsCorrelation`；未来 Node 的 `logs_unreadable` 原样转发由 G3 映射 |

G2 **不**落 `task_logs`、不补 `at`/`sequence`、不按 server 时钟二次过滤、不重排/重编号。

---

## 6. G3 集成顺序（建议）

1. 构造：`db` → `router := route.New(db)` → `sourceSvc := source.New(db, router)` →
   `hub := ws.NewHub(db, router)` → `scheduler := trigger.New(db, sourceSvc, logger)` →
   `sweeper := lease.NewSweeper(db, logger)`。
2. HTTP：`GET /v1/node/stream` → `hub.ServeHTTP`；事件/fire/dry-run/hook → `sourceSvc`；
   cancel/deploy/logs/upgrade → `hub`；edges/deployments/tasks 读 → store。
3. 后台：`go hub.RunDispatchLoop(ctx, 1s)`、`go scheduler.Run(ctx, 1s)`、
   `go sweeper.Run(ctx, 30s)`。
4. 日志 HTTP 按 §5.3 翻译；结构化记录等父集成 Node-log 补口后再消费（当前 `Structured=false`）。

---

## 7. 明确不可用/未提供

- 没有 `internal/executor`（v4 无中枢内置执行器）与 `cmd/agent`（旧 Node 产品）。
- 没有 v3 的 `LinkTarget/NodePin/GraphVersion/Emission/ports/literals/priority/cursor`。
- 没有 `route` 的 HTTP handler、没有状态派生（`status.resolved/observed` 属 G3 读投影）。
- 没有日志结构化记录、没有事件+任务跨表原子入口（见 VERDICT pending，含精确签名）。
