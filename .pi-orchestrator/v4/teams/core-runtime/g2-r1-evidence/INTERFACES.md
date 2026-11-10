# G2-R1 · 实际运行期接口（编译通过，G3 直接消费，不复制）

- 分支：`pi-subagent/core-g2-resume-52fc54d4`
- 本卡描述 **R1 返修后**的 actual runtime exports（以 `go doc` 实测为准）。
- 只读依赖：`internal/store`（S1 `5dc3f27`）、`internal/protocol`（T3/N1 冻结）、`internal/artifact`、`internal/sourcespec`。本轮未改它们。

## 1. `internal/route`

```go
type Clock func() int64
func New(db *store.DB) *Router
func NewWithClock(db *store.DB, now Clock) *Router
func (r *Router) DB() *store.DB
func (r *Router) Now() int64

func (r *Router) PlanFromSource(ctx context.Context, src *store.Source, traceID string, payload map[string]any) (*Plan, error)
func (r *Router) PlanFromEmit(ctx context.Context, parent *store.Task, to string, data json.RawMessage) (*Plan, error)
func (r *Router) Emit(ctx context.Context, in EmitInput) (EmitResult, error)

func (p *Plan) Ready() []Target
func (p *Plan) Tasks(parentTaskID string) []*store.Task
func (p *Plan) PruneInputs() []store.PruneInputs              // 纯剪枝（param_*/no_target/event_schema）
func (p *Plan) FailureInputs() []store.DispatchFailureInputs  // downstream_missing → failed Task 事实
```

`Target.State ∈ ready / pruned / missing`；`missing` 不是剪枝：`Pruned` 为空、`FailureInputs()` 有值。

**R1 变更（C3/C6）**

| 接口/行为 | R1 前 | R1 后 |
|---|---|---|
| `PlanFromSource` 未解析目标 | 进 `Pruned`（被当剪枝） | `Targets[].State=missing`，`Pruned` 不含 `downstream_missing` |
| `prunedTarget` | Reason 由调用方兜 `Pruned[0]` | 每个 `Target` 自带真实 `Reason`/`Detail` |
| emit `to` 声明解析 | 裸声明放行任意 `name@v`；钉版 `name@v` 声明 + 裸 to 走 latest | `declaredRef()`：裸 to 必须唯一命中声明（裸名→latest，唯一钉版→v）；显式 `name@v` 只匹配精确声明；歧义沿 `ErrNotDeclared` 拒；全局 `ResolveRef` 不变 |
| emit 回执 `eventId` | 裸写剪枝事件后取事件 id | `emits` 接受事实 id（预生成 `store.NewEventID()`，与落库同一个值；replay 同 body） |

## 2. `internal/source`

```go
type Clock func() int64
func New(db *store.DB, router *route.Router) *Service
func NewWithClock(db *store.DB, router *route.Router, now Clock) *Service
func (s *Service) Ingest(ctx context.Context, in Input) (*Receipt, error)
func (s *Service) Fire(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Receipt, error)
func (s *Service) Hook(ctx context.Context, token string, body HookBody) (*Receipt, error)
func (s *Service) Preview(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Preview, error)
func (s *Service) PreviewSource(ctx context.Context, src *store.Source, payload json.RawMessage) (*Preview, error)

type Receipt struct {
    EventID, TraceID string
    Duplicate        bool
    Tasks            []TaskRef      // 正常任务 + 未解析 failed 任务（真实请求名/明确声明 version）
    Pruned           []route.Prune  // 只含真实纯剪枝
}
type TaskRef struct { TaskID, Capability string; Version int `json:"version,omitempty"` }
```

**R1 变更（C3）**：`Ingest` 从「InsertEvent → 剪枝裸写 → CreateTasks（多事务）」改为**单次** `store.AcceptSourceEvent`：

- 事件 + 投递窗 + 源 `resource_version` CAS + `namespaceVarsRevision` pin + 正常任务 + failed Task/错误事件 + 纯剪枝 + 完整 receipt 同一事务（0 回滚）。
- 窗口内重复投递返回第一次持久化的同一份 receipt 字节（`Duplicate=true`，不重算、不新建）。
- failed Task 通过 `receipt.Tasks` 投影（W6 原字段），不新增独立 Resource、不进 `pruned`。

## 3. `internal/ws`

```go
func NewHub(db *store.DB, router *route.Router, opts ...Option) *Hub
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (h *Hub) DispatchOnce(ctx context.Context, limit int) (int, error)
func (h *Hub) Deploy(ctx context.Context, namespaceID, name string, version int, nodeIDs []string) ([]DeployOutcome, error)
func (h *Hub) CancelTask(ctx context.Context, namespaceID, taskID, reason string, graceSec int) error
func (h *Hub) RequestLogs(ctx context.Context, namespaceID, taskID string, attempt int,
    previous bool, tailLines, sinceSeconds int, timeout time.Duration) (*LogsResult, error)
func (h *Hub) RequestUpgrade(ctx context.Context, nodeID, targetVersion, sha256Hex, urlPath string) error
func (h *Hub) Online(nodeID string) bool
func (h *Hub) RunDispatchLoop(ctx context.Context, interval time.Duration)

type LogsResult struct {
    Frame      *protocol.LogsResponseFrame
    Empty      bool // previous 且无上一次执行（真实空，不是离线/过期）
    Structured bool // success + lines 非 null（真实逐条记录）
}
type DeployOutcome struct { Deployment *store.Deployment; RequestID string; Sent bool }
```

**R1 变更（C1/C2/C4/C5/C7）**

| 点 | R1 后 |
|---|---|
| dispatch `configRevision`（C1） | 与 welcome 同源：`GetNodeConfig(node).Revision`；不再用 `nodes.resource_version` |
| dispatch 候选（C2） | `store.ProposeTaskAssignment`（未到期复用同一 assignment/proposedAttempt/deadline）；`task.reject` → `store.RejectAssignmentAuthorized`（sess.auth/nodeID） |
| 部署回执（C4） | `store.MarkDeployResultAuthorized`（requestId=`dep_`+id、capability UID/name/version、digest、当前 credential/ns 同一事务）；未知/错/撤销/轮换 0 写 |
| 日志 waiter（C5） | cap 1 + 交付前移除 = 每请求最多一次；非阻塞 send；错 requestId/task/attempt 给明确 `ErrLogsCorrelation`；断开唤醒 `ErrNodeOffline`；`tailLines/sinceSeconds` 原样转发；成功 lines 严格校验（sequence 递增/stream/at） |
| 断连 lifecycle（C5） | `MarkNodeOffline/MarkInstancesDead` 有 5s deadline；被顶掉的旧会话不改节点事实、不误唤醒日志 |
| 握手（C7） | 保持 fail closed：坏 token/名字不符/缺 token/首帧非 hello/坏 JSON/方向错误 → 断开且不注册 online（新增具名测试） |

## 4. 只读消费的 store 权威入口（本轮实际调用）

- `store.(*DB).AcceptSourceEvent(ctx, SourceEventInputs) (json.RawMessage, bool, error)` — `internal/store/source_event.go`
- `store.(*DB).ProposeTaskAssignment(ctx, AssignmentProposalInputs) (AssignmentProposal, error)` — `assignment_authority.go`
- `store.(*DB).RejectAssignmentAuthorized(ctx, NodeAuthorization, AssignmentRejectionInputs) error` — `assignment_authority.go`
- `store.(*DB).EmitAuthorized(ctx, EmitAuth, *EmitRecord, []*Task) (json.RawMessage, bool, error)` — `emission.go`（`PlannedPrunes` / `PlannedFailures` 为同事务非 wire 字段）
- `store.(*DB).CreateDeployments(...)` / `MarkDeployResultAuthorized(...)` — `deployment.go`
- `protocol.LogsRequestFrame{TaskID,RequestID,Attempt,TailLines,SinceSeconds}` + `LogsResponseFrame{Lines *[]LogLine, Error}` — `internal/protocol/ops_frames.go`

## 5. 已知接缝（如实登记，不伪造）

1. **emit `downstream_missing` 的 failed-task 公开投影**：`store.EmitFailureFact` 不返回它在同事务里创建的 failed task id，`protocol.EmitResponse/TaskEmitAckFrame` 也没有 tasks 字段。因此 emit 侧只能：落真实 failed Task + 错误事件（trace 可查）、回执 `pruned=nil`、`to`/`version` 用真实请求 ref/声明 version、`taskId=null`；**不伪造 taskId**。source 侧（W6 receipt 有 `tasks` 数组）已完整投影 failed task。若父要求 emit ack 也带 failed task id，需要扩展 store `EmitFailureFact` 或 emit wire 的冻结契约。
2. **`RequestLogs` 签名新增 `tailLines/sinceSeconds`**：G3 尚未接线；这是 shared `LogsRequestFrame` 已有字段，中枢只转发。
