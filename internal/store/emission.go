package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// ErrEmitIDConflict 表示同一逻辑任务里同一个 emit id 被用于**不同**的请求（to/data）：
// emit 网关的 409 `emit_id_conflict`，调用方必须换一个 id。静默按第一次的请求重放
// 会让「我改了 data 怎么没生效」变成一个查不出来的问题。
var ErrEmitIDConflict = errors.New("emit_id_conflict")

// ErrEmitNamespaceDenied 表示节点未获授权访问任务所属命名空间：**授权**失败（不是
// 身份失败），单独一个哨兵是为了让上层把「不该派给它」与「伪造身份」分开记日志。
var ErrEmitNamespaceDenied = errors.New("namespace_unauthorized")

// ErrNodeIdentityMismatch 表示 emit 声明的节点身份与库里那条节点不一致。
//
// 它挡的是「拿到别人令牌的进程替自己冒领」：一次 emit 的执行权属于
// 某个节点 + 某个 attempt，两者都必须和权威事实对上。
var ErrNodeIdentityMismatch = errors.New("node_identity_mismatch")

// EmitPruneFact 是本次 emit 计划里被剪枝的一条观察事实（非 wire，不参与幂等回放比对）。
//
// 计划事实必须进同一个写事务：它是「已经失去执行权」也挡不住的半截事实的唯一防线
// （提前另一事务写，会出现授权/幂等都过了、剪枝却没落）。
type EmitPruneFact struct {
	Capability string
	Reason     string
	Detail     string
}

// EmitFailureFact 是本次 emit 计划里未解析的下游目标（downstream_missing）：建的是
// **失败 Task**（phase=failed、attempt=0、NULL capability ref），不是剪枝（§6.2）。
type EmitFailureFact struct {
	Capability string
	Version    int
	Reason     string
	Detail     string
	// TaskID 是调用方预分配、要在回执里先给出的任务 id（可选）。
	// 省略 → store 现场 NewTaskID；给定 → 落库必须用同一个 id（否则回执就是假身份）。
	TaskID string
}

// EmitRejectionFact 是「这次 emit 被中枢拒绝、什么都没派发」的观察事实（#636）：与计划事实
// 同事务落地（没执行权就留不下），不建任务、不进幂等域。
type EmitRejectionFact struct {
	To         string // 真实请求的引用（`name` / `name@version`），不解析、不规范化
	Capability string // 观察面用的目标名（basename）；空 → 落库时回退到 To
	Reason     string // 取值见 emitRejectionReasons
	Detail     string
}

// emitRejectionReasons 是「emit 被中枢拒绝」的观察码闭集（#636）：与剪枝共用观察行通道
// （source='route'），但仍是 4xx 拒绝，所以不在 protocol.IsPruneReason 里。
var emitRejectionReasons = []string{protocol.ReasonTargetNotDeclared}

func isEmitRejectionReason(reason string) bool {
	for _, allowed := range emitRejectionReasons {
		if allowed == reason {
			return true
		}
	}
	return false
}

// EmitRecord 是一次 emit 的落库行（capability.md §4.1 / D-I9）。
//
// 它是**幂等的唯一依据**：同一个 (task_id, emit_id) 只派发一次，重复请求回放同一结果。
// EmitID 是**可选**的：不给就每次都是新派发（不按调用序号自动去重——那会制造假幂等）。
//
// **不存业务 seq、不存 RPC requestId**：序号是传输层的事（重连重发靠 emit_id 幂等），
// 把 requestId 当业务去重键会让「同一次执行换个传输发两次」被判成两次派发。
//
// ToRef 是目标引用（`name` / `name@version`），Data 是请求体原样字节，
// Result 是首次派发的结果 JSON（含建出的 taskIds / 剪枝原因），CreatedAt 是首次落库时刻。
type EmitRecord struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"taskId"`
	EmitID    *string         `json:"emitId"`
	ToRef     string          `json:"toRef"`
	Data      json.RawMessage `json:"data"`
	Result    json.RawMessage `json:"result"`
	CreatedAt int64           `json:"createdAt"`
	// PlannedPrunes / PlannedFailures 是**同事务计划事实**（没有 JSON 协议别名）：
	// 一次 emit 的剪枝观察与下游缺失失败，与 emit 行/子任务同生共死。
	PlannedPrunes    []EmitPruneFact    `json:"-"`
	PlannedFailures  []EmitFailureFact  `json:"-"`
	PlannedRejection *EmitRejectionFact `json:"-"` // 「这次 emit 被拒」的观察事实（#636）
}

// EmitAuth 是一次 emit 的**授权事实**（store 内部形状，不镜像任何 wire DTO）。
//
// 授权参数必须进同一个事务：`VerifyAttemptToken` 与派发之间若有窗口（任务被取消 /
// 租约过期 / 被重派），那个窗口里建出的下游任务看起来完全合法、无法回收。
//
// NodeTokenHash 是节点身份令牌的 sha256（明文永远不进 store）：
// 中枢按它判「这还是不是那台机器」，而不是只看 task.node_id（那是一个可伪造的字符串）。
type EmitAuth struct {
	// TaskID 是发起 emit 的逻辑任务。
	TaskID string
	// NodeID 是提交这次 emit 的节点。
	NodeID string
	// NodeTokenHash 是节点身份令牌的 sha256（与 nodes.token_hash 比对）。
	NodeTokenHash string
	// Attempt / LeaseID / AttemptToken 是本次执行的三件套（过期或错一个就拒）。
	Attempt      int
	LeaseID      string
	AttemptToken string
	// Now 是判定租约未过期的时刻（0 → Now()）。
	Now int64
}

// LookupEmit 按 (task_id, emit_id) 取一条幂等记录；未命中返回 ErrNotFound。
//
// emitID 为空直接 ErrNotFound：没有幂等键就没有可查的记录（调用方据此走「总是新建」）。
func (d *DB) LookupEmit(ctx context.Context, taskID, emitID string) (*EmitRecord, error) {
	if taskID == "" {
		return nil, fmt.Errorf("lookup emit: empty task_id")
	}
	if emitID == "" {
		return nil, ErrNotFound
	}
	return lookupEmitOn(ctx, d, taskID, emitID)
}

// lookupEmitOn 是 LookupEmit 的 ex 版本（事务内读必须走它，否则看不到本事务的写）。
func lookupEmitOn(ctx context.Context, ex queryExec, taskID, emitID string) (*EmitRecord, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT `+emitColumns+` FROM emits WHERE task_id = ? AND emit_id = ?`, taskID, emitID)
	if err != nil {
		return nil, fmt.Errorf("lookup emit %s#%s: %w", taskID, emitID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("lookup emit %s#%s: %w", taskID, emitID, err)
		}
		return nil, ErrNotFound
	}
	return scanEmit(rows)
}

// EmitAuthorized 在**同一个事务**里完成「授权校验 + 幂等记录 + 建下一跳任务行」。
//
// 事务内顺序（不可调整）：
//
//  1. 节点身份：nodes 行存在，token_hash == auth.NodeTokenHash；
//  2. 命名空间授权：authorized(node_configs.namespaces) ∩ declared(nodes.declared_namespaces)
//     覆盖任务所属 ns；
//  3. 执行授权：task.phase=running、task.node_id=auth.NodeID、
//     attempt/lease_id 相等、sha256(auth.AttemptToken)==attempt_token_hash、
//     lease_until 未过期、**没有取消在飞**；
//  4. 幂等：emit_id 非空且已存在 → to_ref/data 相同回旧 result（first=false），
//     不同 → ErrEmitIDConflict（**第 3 步不过就一律拒，已存在也不能绕过**）；
//  5. 写入：插入 emit 行 + 同事务建子任务（任何一条失败整体回滚）。
//
// 计划事实（PlannedPrunes）的 reason 必须是 protocol.IsPruneReason，校验位置在
// 授权与 replay 之后、**任何写之前**——downstream_missing 是失败 Task 的观察码，
// 不是剪枝，不能从这里混进 status.observed.pruned。
//
// 第 4 步为什么必须在授权之后：一条已经失效的执行若能靠「我上次 emit 过」拿到回执，
// 就会认为自己仍有执行权、继续跑完写终态——幂等变成绕过授权的后门。
func (d *DB) EmitAuthorized(ctx context.Context, auth EmitAuth, rec *EmitRecord, tasks []*Task) (json.RawMessage, bool, error) {
	if rec == nil {
		return nil, false, fmt.Errorf("emit: nil record")
	}
	if rec.TaskID == "" || rec.ToRef == "" {
		return nil, false, fmt.Errorf("emit: empty task_id/to_ref")
	}
	if auth.TaskID == "" || auth.NodeID == "" || auth.NodeTokenHash == "" ||
		auth.LeaseID == "" || auth.AttemptToken == "" || auth.Attempt <= 0 {
		return nil, false, fmt.Errorf("emit: 授权参数不完整（task/node/nodeTokenHash/attempt/lease/attemptToken 都必填）")
	}
	if auth.TaskID != rec.TaskID {
		return nil, false, fmt.Errorf("%w: auth.taskID %s != emit.taskID %s",
			ErrLeaseMismatch, auth.TaskID, rec.TaskID)
	}
	if auth.Now == 0 {
		auth.Now = Now()
	}
	data := rec.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("emit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	if err := authorizeEmitOn(ctx, ex, auth); err != nil {
		return nil, false, err
	}

	// 授权已过，才谈幂等。
	if rec.EmitID != nil && *rec.EmitID != "" {
		existing, lerr := lookupEmitOn(ctx, ex, rec.TaskID, *rec.EmitID)
		switch {
		case lerr == nil:
			if existing.ToRef != rec.ToRef || !sameJSON(existing.Data, data) {
				return nil, false, fmt.Errorf("%w: task %s emit id %s was used for to=%q",
					ErrEmitIDConflict, rec.TaskID, *rec.EmitID, existing.ToRef)
			}
			return existing.Result, false, nil // 回滚空事务（只读命中）
		case !errors.Is(lerr, ErrNotFound):
			return nil, false, lerr
		}
	}

	// 计划事实的闭集校验先于**任何写**：非法剪枝码一旦落库，就是一条
	// 「把失败伪装成剪枝」的假事实（事件 type 就是那个 reason）。
	if err := validateEmitPlannedPrunes(rec); err != nil {
		return nil, false, err
	}
	if err := validateEmitPlannedRejection(rec); err != nil {
		return nil, false, err
	}

	if err := d.createTasksTx(ctx, tx, tasks); err != nil {
		return nil, false, err
	}
	failureTaskIDs, err := d.writeEmitPlannedFactsOn(ctx, ex, rec, auth.Now)
	if err != nil {
		return nil, false, err
	}
	result, err := emitResult(rec, tasks, failureTaskIDs)
	if err != nil {
		return nil, false, err
	}
	if err := insertEmitTx(ctx, ex, rec, data, result); err != nil {
		// 并发下另一条请求先插了同一个 (task_id, emit_id)：唯一索引会在这里报冲突。
		// 那种情况按幂等命中处理（回读对方那份结果），而不是把错误抛给调用方。
		if rec.EmitID != nil && *rec.EmitID != "" && isUniqueViolation(err) {
			if rerr := tx.Rollback(); rerr != nil {
				return nil, false, fmt.Errorf("emit: rollback after conflict: %w", rerr)
			}
			existing, lerr := d.LookupEmit(ctx, rec.TaskID, *rec.EmitID)
			if lerr != nil {
				return nil, false, fmt.Errorf("emit: reread after conflict: %w", lerr)
			}
			if existing.ToRef != rec.ToRef || !sameJSON(existing.Data, data) {
				return nil, false, fmt.Errorf("%w: task %s emit id %s was used for to=%q",
					ErrEmitIDConflict, rec.TaskID, *rec.EmitID, existing.ToRef)
			}
			return existing.Result, false, nil
		}
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("emit: commit: %w", err)
	}
	return result, true, nil
}

// validateEmitPlannedPrunes 把 Emit 侧的计划剪枝收紧到与 Source 接收同一个闭集：
// 剪枝只允许 protocol.IsPruneReason；downstream_missing 是失败 Task 的观察事件，
// 不是剪枝，不能从这里混进 status.observed.pruned。
//
// 为什么不能只靠 insertPruneFactEventOn（event.go）的守卫：它为了失败 Task 的
// downstream_missing 观察事件而**显式允许**那个码，所以 Emit 计划里的剪枝必须
// 在写之前用更紧的闭集单独校验（否则 downstream_missing / 空码 / 任意失败码
// 都能从这条路径变成剪枝留痕）。
func validateEmitPlannedPrunes(rec *EmitRecord) error {
	for i, p := range rec.PlannedPrunes {
		if !protocol.IsPruneReason(p.Reason) {
			return fmt.Errorf("emit %s: planned prune %d reason %q 不是剪枝码（闭集：%v；downstream_missing 走失败 Task）",
				rec.TaskID, i, p.Reason, protocol.PruneReasons())
		}
	}
	return nil
}

// validateEmitPlannedRejection：reason ∈ emitRejectionReasons，且不得带 emit_id（假幂等）。
func validateEmitPlannedRejection(rec *EmitRecord) error {
	r := rec.PlannedRejection
	if r == nil {
		return nil
	}
	if !isEmitRejectionReason(r.Reason) {
		return fmt.Errorf("emit %s: planned rejection reason %q 不是拒绝码（闭集：%v）",
			rec.TaskID, r.Reason, emitRejectionReasons)
	}
	if rec.EmitID != nil && *rec.EmitID != "" {
		return fmt.Errorf("emit %s: 被拒绝的 emit 不得带 emit_id（拒绝不进幂等域）", rec.TaskID)
	}
	return nil
}

// writeEmitPlannedFactsOn 在 emit 的**同一个事务**里写计划事实：
//
//   - 未解析失败 Task（downstream_missing）+ 它的错误观察事件；
//   - 纯剪枝观察事件；被拒绝的 emit（#636）的拒绝行。
//
// 授权与历史 replay 已在前面完成（命中 replay 直接返回，不执行本函数——回放不得
// 附带任何新写）。任何一条写失败都返回错误，由调用方整事务回滚；不吞 prune 错误。
//
// 返回值是**真实创建**的失败 Task id（按计划顺序）：调用方据此核对回执点名的
// taskId 不是预先编好、实际没落库的假身份。
func (d *DB) writeEmitPlannedFactsOn(ctx context.Context, ex queryExec, rec *EmitRecord, now int64) ([]string, error) {
	if len(rec.PlannedFailures) == 0 && len(rec.PlannedPrunes) == 0 && rec.PlannedRejection == nil {
		return nil, nil
	}
	if now == 0 {
		now = Now()
	}
	// 计划事实挂在发起 emit 的逻辑任务上（namespace/trace/父子关系都以它为准）。
	var (
		namespaceID string
		traceID     string
	)
	err := ex.queryRowContext(ctx,
		`SELECT namespace_id, trace_id FROM tasks WHERE id = ?`, rec.TaskID).Scan(&namespaceID, &traceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s", ErrTaskNotRunning, rec.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("emit %s: read parent task: %w", rec.TaskID, err)
	}
	parent := rec.TaskID
	failureTaskIDs := make([]string, 0, len(rec.PlannedFailures))
	for _, f := range rec.PlannedFailures {
		// 同一个 typed input 同时用于构造行与落库：调用方预分配的 TaskID 从这里
		// 一路带到 INSERT，回执里的 id 与真实行不可能分叉。
		input := DispatchFailureInputs{
			Capability: f.Capability, Version: f.Version, Reason: f.Reason, Detail: f.Detail,
			TaskID: f.TaskID,
		}
		row, ferr := newDispatchFailureTask(namespaceID, traceID, &parent, input, now)
		if ferr != nil {
			return nil, ferr
		}
		if err := insertDispatchFailureTaskOn(ctx, ex, row, input, now); err != nil {
			return nil, err
		}
		failureTaskIDs = append(failureTaskIDs, row.ID)
	}
	for _, p := range rec.PlannedPrunes {
		// Reason 已由 validateEmitPlannedPrunes 按剪枝闭集校验过；这里不再放宽，
		// 事件类型就是那个 reason（观察面不另外存副本）。
		payload, merr := json.Marshal(map[string]any{
			"capability":   p.Capability,
			"reason":       p.Reason,
			"detail":       p.Detail,
			"taskId":       rec.TaskID,
			"parentTaskId": rec.TaskID,
		})
		if merr != nil {
			return nil, fmt.Errorf("emit %s: marshal prune: %w", rec.TaskID, merr)
		}
		ev := &Event{NamespaceID: namespaceID, TraceID: traceID, Type: p.Reason, Payload: payload, CreatedAt: now}
		if err := insertPruneFactEventOn(ctx, ex, ev); err != nil {
			return nil, err
		}
	}
	if r := rec.PlannedRejection; r != nil {
		capability := r.Capability
		if capability == "" {
			capability = r.To
		}
		payload, merr := json.Marshal(map[string]any{
			"to":           r.To,
			"capability":   capability,
			"reason":       r.Reason,
			"detail":       r.Detail,
			"taskId":       rec.TaskID,
			"parentTaskId": rec.TaskID,
		})
		if merr != nil {
			return nil, fmt.Errorf("emit %s: marshal rejection: %w", rec.TaskID, merr)
		}
		ev := &Event{NamespaceID: namespaceID, TraceID: traceID, Type: r.Reason, Payload: payload, CreatedAt: now}
		if err := insertPruneFactEventOn(ctx, ex, ev); err != nil {
			return nil, err
		}
	}
	return failureTaskIDs, nil
}

// authorizeEmitOn 做 EmitAuthorized 的第 1-3 步（节点身份 / ns 授权 / 执行授权）。
//
// 只用 tx 句柄读：授权判定必须看到与本事务写入一致的快照，否则
// 「刚被取消的任务」在池上可能还是 running。
func authorizeEmitOn(ctx context.Context, ex queryExec, auth EmitAuth) error {
	// ① 节点身份（token_hash 是唯一可判据的身份事实）。
	var (
		nodeTokenHash string
		rawDeclared   string
	)
	err := ex.queryRowContext(ctx,
		`SELECT token_hash, declared_namespaces FROM nodes WHERE id = ?`, auth.NodeID).
		Scan(&nodeTokenHash, &rawDeclared)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node %s 不存在", ErrNodeIdentityMismatch, auth.NodeID)
	}
	if err != nil {
		return fmt.Errorf("emit: read node identity: %w", err)
	}
	if nodeTokenHash == "" || nodeTokenHash != auth.NodeTokenHash {
		return fmt.Errorf("%w: node %s 的令牌与提交者不一致", ErrNodeIdentityMismatch, auth.NodeID)
	}

	// ② 任务行（执行授权的权威事实）。
	var (
		namespaceID  string
		phase        string
		nodeID       sql.NullString
		attempt      int
		leaseID      string
		tokenHash    string
		leaseUntil   sql.NullInt64
		cancelReq    sql.NullInt64
		cancelReason sql.NullString
	)
	err = ex.queryRowContext(ctx,
		`SELECT namespace_id, phase, node_id, attempt, lease_id, attempt_token_hash,
		        lease_until, cancel_requested_at, cancel_reason
		   FROM tasks WHERE id = ?`, auth.TaskID).
		Scan(&namespaceID, &phase, &nodeID, &attempt, &leaseID, &tokenHash,
			&leaseUntil, &cancelReq, &cancelReason)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %s 不存在", ErrTaskNotRunning, auth.TaskID)
	}
	if err != nil {
		return fmt.Errorf("emit: read task: %w", err)
	}
	if phase != TaskRunning {
		return fmt.Errorf("%w: task %s is %s (reason %s)",
			ErrTaskNotRunning, auth.TaskID, phase, protocol.ReasonTaskNotRunning)
	}
	if cancelReq.Valid {
		return fmt.Errorf("%w: task %s 已请求取消（%s），执行授权已收回",
			ErrTaskNotRunning, auth.TaskID, cancelReason.String)
	}
	if !nodeID.Valid || nodeID.String != auth.NodeID {
		return fmt.Errorf("%w: task %s 归节点 %v，不是 %s",
			ErrLeaseMismatch, auth.TaskID, nodeID.String, auth.NodeID)
	}
	if attempt != auth.Attempt || leaseID != auth.LeaseID {
		return fmt.Errorf("%w: task %s 的 attempt/lease 已变（have %d/%s, want %d/%s）",
			ErrLeaseMismatch, auth.TaskID, attempt, leaseID, auth.Attempt, auth.LeaseID)
	}
	if tokenHash == "" || hashAttemptToken(auth.AttemptToken) != tokenHash {
		return fmt.Errorf("%w: task %s 的尝试令牌不匹配", ErrAttemptTokenInvalid, auth.TaskID)
	}
	if !leaseUntil.Valid || leaseUntil.Int64 <= auth.Now {
		return fmt.Errorf("%w: task %s 的租约已过期", ErrLeaseMismatch, auth.TaskID)
	}

	// ③ 命名空间授权：申报 ∩ 授权必须覆盖任务所属 ns。
	// 缺失 node_configs 行时默认 ["*"]（与 AuthorizedNamespaces 同口径）。
	authorized := []string{NamespaceWildcard}
	var rawAuthorized string
	qerr := ex.queryRowContext(ctx,
		`SELECT namespaces FROM node_configs WHERE node_id = ?`, auth.NodeID).Scan(&rawAuthorized)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return fmt.Errorf("emit: read node config: %w", qerr)
	}
	if qerr == nil {
		decoded, derr := decodeStringList(rawAuthorized)
		if derr != nil {
			return fmt.Errorf("emit: %w", derr)
		}
		authorized = decoded
	}
	declared, derr := decodeStringList(rawDeclared)
	if derr != nil {
		return fmt.Errorf("emit: %w", derr)
	}
	var namespaceName string
	if serr := ex.queryRowContext(ctx,
		`SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&namespaceName); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			return fmt.Errorf("%w: 任务所属命名空间 %s 不存在", ErrNamespaceRequired, namespaceID)
		}
		return fmt.Errorf("emit: read namespace: %w", serr)
	}
	if !namespacesAllow(intersectNamespaces(authorized, declared), namespaceName) {
		return fmt.Errorf("%w: 节点 %s 未获授权访问命名空间 %s",
			ErrEmitNamespaceDenied, auth.NodeID, namespaceName)
	}
	return nil
}

// emitResult 决定这次派发的回执：调用方给了就用它，否则按建出的任务行生成。
func emitResult(rec *EmitRecord, tasks []*Task, failureTaskIDs []string) (json.RawMessage, error) {
	if len(rec.Result) > 0 {
		// 调用方给的 result 就是回放原字节，不改写；但要核对它点名的 taskId 确实是
		// **本事务真实创建**的任务行（正常子任务或失败 Task）：失败 Task 的回执身份
		// 由 route 预分配、store 用同一 id 落库，任何分叉都会变成假回执。
		if err := checkReceiptTaskID(rec.Result, tasks, failureTaskIDs); err != nil {
			return nil, err
		}
		return rec.Result, nil
	}
	if r := rec.PlannedRejection; r != nil {
		// 兜底回执必须是拒绝形状（taskId:null + pruned），不能落到 `{"taskIds":[]}`。
		b, err := json.Marshal(map[string]any{
			"taskId": nil,
			"to":     rec.ToRef,
			"pruned": map[string]any{"reason": r.Reason, "detail": r.Detail},
		})
		if err != nil {
			return nil, fmt.Errorf("emit: marshal rejection result: %w", err)
		}
		return json.RawMessage(b), nil
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t == nil || t.ID == "" {
			continue
		}
		ids = append(ids, t.ID)
	}
	b, err := json.Marshal(map[string]any{"taskIds": ids})
	if err != nil {
		return nil, fmt.Errorf("emit: marshal result: %w", err)
	}
	return json.RawMessage(b), nil
}

// checkReceiptTaskID 核对回执里的 taskId（若有且非 null）是本次事务创建的任务：
// taskId:null（剪枝回执）与没有该字段的旧结果不做限制（它们不声称任何身份）。
func checkReceiptTaskID(result json.RawMessage, tasks []*Task, failureTaskIDs []string) error {
	var probe struct {
		TaskID *string `json:"taskId"`
	}
	if err := json.Unmarshal(result, &probe); err != nil || probe.TaskID == nil {
		return nil
	}
	want := *probe.TaskID
	for _, t := range tasks {
		if t != nil && t.ID == want {
			return nil
		}
	}
	for _, id := range failureTaskIDs {
		if id == want {
			return nil
		}
	}
	return fmt.Errorf("emit: 回执 taskId %q 不是本事务创建的任务（拒绝伪造回执身份）", want)
}

// insertEmitTx 在调用方事务里插一行幂等记录（唯一索引负责「同 id 只能一条」）。
func insertEmitTx(ctx context.Context, ex queryExec, rec *EmitRecord, data, result json.RawMessage) error {
	if rec.ID == "" {
		rec.ID = newID("emt")
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = Now()
	}
	var emitID any
	if rec.EmitID != nil && *rec.EmitID != "" {
		emitID = *rec.EmitID
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO emits (id, task_id, emit_id, to_ref, data, result, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		rec.ID, rec.TaskID, emitID, rec.ToRef, string(data), string(result), rec.CreatedAt); err != nil {
		return fmt.Errorf("emit %s -> %s: %w", rec.TaskID, rec.ToRef, err)
	}
	rec.Data = data
	rec.Result = result
	return nil
}

// ListEmits 按创建顺序列出某任务的全部 emit（trace 视图用）；空列表返回 []。
func (d *DB) ListEmits(ctx context.Context, taskID string) ([]*EmitRecord, error) {
	if taskID == "" {
		return nil, fmt.Errorf("list emits: empty task_id")
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+emitColumns+` FROM emits WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list emits %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []*EmitRecord{}
	for rows.Next() {
		e, err := scanEmit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// emitColumns 是统一的列顺序，scanEmit 依赖它。
const emitColumns = `id, task_id, emit_id, to_ref, data, result, created_at`

// scanEmit 从一行结果解析 EmitRecord。
func scanEmit(rows *sql.Rows) (*EmitRecord, error) {
	var (
		rec    EmitRecord
		emitID sql.NullString
		data   string
		result string
	)
	if err := rows.Scan(&rec.ID, &rec.TaskID, &emitID, &rec.ToRef, &data, &result, &rec.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan emit: %w", err)
	}
	if emitID.Valid {
		v := emitID.String
		rec.EmitID = &v
	}
	rec.Data = json.RawMessage(data)
	if len(rec.Data) == 0 {
		rec.Data = json.RawMessage("{}")
	}
	rec.Result = json.RawMessage(result)
	return &rec, nil
}
