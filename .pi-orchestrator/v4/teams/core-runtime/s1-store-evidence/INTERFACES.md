# S1 · store 受保护接缝实施接口卡（D1–D7，唯一 store writer）

- 分支/树：`pi-subagent/core-g2-resume-52fc54d4`（canonical G2 worktree），基线 `323c969` docs / source `cba97d4`。
- 依据：父卡 `.pi-orchestrator/v4/STORE-INGRESS-FAILURE-HANDOFF.md`（D1–D6）+ `.pi-orchestrator/v4/STORE-DEPLOYMENT-AUTH-ADDENDUM.md`（D7）。
- 所有权：只 `internal/store/**`（含 `schema.sql`、`*_test.go`、`v4test` 直接 store 测试）。runtime/protocol/artifact/Node/server/API/Go 依赖/FE/Makefile 只读。
- 本卡只列**实际会新增的 Go 面**（包内私有 helper 不列）；字段名冻结，语义按 D1–D7。

## D1 · 未解析失败 Task 的内部表示（无新 public 入口）

- `tasks.capability_id` 改为 **nullable**（SQL NULL）；`Task.CapabilityID` 仍是 `string`：
  scan 用受控 `NULL → ""` 映射，反向只在失败 fact 路径写 NULL。不 pointer 化全部字段。
- 失败 Task 行只由 store 的失败 fact helper 写：
  `phase=failed`、`attempt=0`、`error='downstream_missing'`、`digest=''`、`capability`=请求引用/名字、
  `version`=明确声明版本或 **0（= 没有声明/解析事实）**、`params='{}'`、`created_at=finished_at=server now`；
  `node_id/node_name/lease*/attempt_token_hash/assignment*/reported/result_json/started_at` 全空。
- SQL CHECK（schema 内）拒绝：NULL ref 的非 failed / attempt>0 / 非空 Node/lease/token/grant /
  `error != 'downstream_missing'` / `digest != ''`；resolved ref 继续 FK + `digest <> '' AND version > 0`。
- 普通入口（`CreateTasks`/`AssignTask`/grant/lifecycle）拒绝未解析失败行；`GetTask`/`ListTasks`/
  `ListTasksByTrace`/trace 统计必须能读 NULL ref 行（不隐藏、不 scan 失败）。

## D2 · `AcceptSourceEvent`（新增 public）

```go
type SourceEventInputs struct {
    NamespaceID string
    SourceID string                 // sources.id（稳定 id，不是源名）
    ExpectedSourceResourceVersion int64 // sources.resource_version CAS（必填 > 0）
    ExpectedNamespaceVarsRevision int64 // namespace_configs.revision pin（无配置行=0）
    TraceID string                  // 空 → store 生成
    Type string                     // 空 → 源名
    Payload json.RawMessage
    DeliveryID string               // 空 = 每次新事件（无去重窗）
    WindowSec int64                 // 投递去重窗；0 = 永久
    Now int64                       // 0 → Now()
    Tasks []*Task                   // 已解析正常任务（pending，capability_id/digest/version pin 住）
    Failures []DispatchFailureInputs
    Prunes []PruneInputs
}

func (d *DB) AcceptSourceEvent(ctx context.Context, in SourceEventInputs) (receipt json.RawMessage, first bool, err error)

type PruneInputs struct {
    Capability string
    Reason     string // protocol.IsPruneReason 闭集（downstream_missing 不走这里）
    Detail     string
}

type DispatchFailureInputs struct {
    Capability string // 请求的引用/名字（必填）
    Version    int    // 0 = 请求未声明版本；>0 = 明确声明
    Reason     string // 只允许 downstream_missing（空 = downstream_missing）
    Detail     string // 投影 message
}
```

接收回执（store 内部持久事实，存 `events.receipt` 列，不是新 Resource）：

```go
type SourceEventReceipt struct {
    EventID     string                      `json:"eventId"`
    TraceID     string                      `json:"traceId"`
    Tasks       []SourceEventReceiptTask    `json:"tasks"`       // 正常 pending 任务
    FailedTasks []SourceEventReceiptFailure `json:"failedTasks"` // 未解析失败任务
    Pruned      []SourceEventReceiptPrune   `json:"pruned"`      // 纯剪枝
}
type SourceEventReceiptTask struct {
    TaskID, Capability string; Version int
}
type SourceEventReceiptFailure struct {
    TaskID, Capability string; Version int `json:"version,omitempty"`; Reason, Message string
}
type SourceEventReceiptPrune struct {
    Capability, Reason, Detail string
}
```

语义（一次 `BEGIN IMMEDIATE` 事务）：

1. 当前源身份（`sources.id` + `resource_version` CAS）+ 命名空间存在 + namespace 输入 revision pin；
   源不存在/跨 ns/ABA/修订失配 → 错误、0 写、**不占投递窗**。
2. 投递窗判定在 tx 内（`event_deliveries`）；窗口内重复 → 返回第一次持久化的**同一份 receipt 字节**，
   `first=false`，不建任务、不占新窗、不重算可变资源。
3. 新事件：`events`（含 receipt 列）+ 正常 Task 批 + 失败 Task/错误事件 + 剪枝事件**同一事务**；
   任一点真实失败（含 ctx/约束/触发）→ 全部回滚（含 window 占用）。
4. 无 `DeliveryID` 同样原子，只是不去重。允许 0 fanout（如 `no_target`）。
5. 失败任务与其 `source='route' / type=downstream_missing` 错误事件同事务；错误投影沿用现有
   `reason` + `message` 对象，不回填 Node result。

新增错误哨兵：

```go
var ErrSourceEventReceiptMissing = errors.New("store: source event receipt missing") // 窗口内重复但原事件没有持久 receipt
var ErrNamespaceInputsChanged    = errors.New("store: namespace inputs revision changed")
```

## D3 · `EmitAuthorized` 同事务计划事实（签名不变）

```go
// EmitRecord 新增非 wire 计划事实（没有 JSON 协议别名）：
type EmitPruneFact struct { Capability, Reason, Detail string }
type EmitFailureFact struct { Capability string; Version int; Reason, Detail string }

type EmitRecord struct {
    // 原有字段不变
    PlannedPrunes   []EmitPruneFact   `json:"-"`
    PlannedFailures []EmitFailureFact `json:"-"`
}
```

- `EmitAuthorized(ctx, EmitAuth, *EmitRecord, []*Task) (json.RawMessage, bool, error)` 签名不变。
- 事务内顺序不变：当前 credential/ns/lease/token/cancel 授权 → 幂等 replay 判定（命中即 0 附带写）
  → 正常任务 → 失败 Task+错误事件 → 剪枝事件 → emit 行。任一 prune/failure 写失败整事务回滚，
  错误不吞；剪枝也从本次事务落，runtime 不再提前裸写。

## D4 · proposal / reject 原子 authority（新增 public）

```go
type AssignmentProposalInputs struct {
    TaskID string
    NodeID string
    ProposedAttempt int    // 必须 >= 1
    AssignmentID string    // 空 = store 生成 asg_…
    Now int64
}
type AssignmentProposal struct {
    TaskID, AssignmentID, NodeID, NodeName string
    ProposedAttempt int
    ExpiresAt int64
    Reused bool // true = 命中未到期原提案（完整返回原绑定，不覆写）
}
func (d *DB) ProposeTaskAssignment(ctx context.Context, in AssignmentProposalInputs) (AssignmentProposal, error)

type AssignmentRejectionInputs struct {
    TaskID, AssignmentID, NodeID string
    ProposedAttempt int
    Reason string // 闭集 stale_config | slot_unavailable
    Now int64
}
func (d *DB) RejectAssignmentAuthorized(ctx context.Context, auth NodeAuthorization, in AssignmentRejectionInputs) error
```

- Proposal：同 tx 检查 pending/未取消 + cap 存在（未解析失败行拒绝）+ 目标 Node 在线 +
  当前 ns 授权；未到期的同 Task 提案原样返回（多 sweep 不 churn）；`expiry <= now` 视到期才可换新绑定。
  不授 lease/attempt/busy。
- Reject：同 tx 核当前 credential hash + ns 仍可用 + 任务提案确属该 Node + assignment/proposed 精确匹配 +
  提案未过期；错/旧/撤销/过期 → 错误、0 写、不伤新提案。reason 闭集，不计 attempt，bounded backoff。
- 旧 Grant/Renew/Complete/AckCancel/`RejectAssignment`/`AssignTask` authority 不重构、不降安全。

## D7 · 部署意图/回执事务授权（新增 public）

```go
type DeploymentResultInputs struct {
    DeploymentID string
    RequestID string        // 必须 == "dep_"+DeploymentID（关联，不选身份）
    NamespaceID string
    CapabilityID string     // capability version UID
    Capability string
    Version int
    Digest string
    State string            // applied | failed
    Reason string
    Now int64
}
func (d *DB) MarkDeployResultAuthorized(ctx context.Context, auth NodeAuthorization, in DeploymentResultInputs) error
var ErrDeploymentReceiptConflict = errors.New("store: deployment receipt conflict")
```

- `CreateDeployments` 签名不变：同一事务先把全部显式 Node 逐一验存在 + 当前 `declared ∩ authorized`
  覆盖目标 ns，再写；任一未授权/不存在 → 全包 0 写，不 supersede 先前合法 intent。offline 不影响授权
  （已授权离线节点仍是 pending）。
- `MarkDeployResultAuthorized` 同事务：当前 credential hash / ns 许可 / Node 绑定 / 当前 intent
  （id + capability UID/name/version/digest + requestId）一致且未被 supersede，才落 state/error。
  重复精确同结果幂等；已 receipt 后 changed body → `ErrDeploymentReceiptConflict`；旧凭证/撤销 ns/
  未请求 intent/错 request/digest/跨 Node 全部 0 写。旧内部 `MarkDeployResult` 保留，不成为新 wire 入口。

## Schema / 严格 Open

- 只改 v4 baseline：`events` 追加 `receipt TEXT NOT NULL DEFAULT '{}'`；`tasks.capability_id` 去 NOT NULL，
  加两条 CHECK（未解析失败形状、resolved pin 形状）。不新增表、不加 Resource、不引事务框架。
- 严格 layout 仍由 schema.sql 派生（多列/缺列/列定义差异都会 `ErrSchemaInvalid`）；旧 v4 布局库明确拒绝、
  不删数据；重开幂等。`receipt` 参与往返与 layout 门禁。

## 不提供（刻意）

- 不新增 wire DTO/帧、不改 runtime/protocol/Node/FE；G3 之后投影失败 Task 的 version/digest 省略。
- 不把全部 Task 字段 pointer 化；不加 receipt 独立 Resource/API；不新增 reconciler/Revision。
- 不弱化 G1/C3 既有断言；fixture 只在授权/真实 pin 缺口处补 setup 并注明原因。
