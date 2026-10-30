package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// 九态任务状态机（契约 §3.1）。
const (
	TaskPending      = "pending"
	TaskAssigned     = "assigned"
	TaskAcked        = "acked"
	TaskRunning      = "running"
	TaskSucceeded    = "succeeded"
	TaskFailed       = "failed"
	TaskCanceled     = "canceled"
	TaskAbandoned    = "abandoned"
	TaskLeaseExpired = "lease_expired"
)

// 租约与时序参数（契约 §3.3）。
const (
	// LeaseTTLSec 是租约 TTL：lease_until = now + 30。
	LeaseTTLSec = 30
	// HeartbeatSec 是心跳间隔（welcome 帧下发）。
	HeartbeatSec = 10
	// AckWaitSec 是派发后等待 ack 的上限：20s 未 ack → lease_expired。
	AckWaitSec = 20
	// DeadAfterHeartbeats 是判死倍数：超过 2×心跳无上行帧即判死。
	DeadAfterHeartbeats = 2
	// LeaseSweepIntervalSec 是租约巡检周期：每 5s 扫一次过期租约。
	//
	// 取 5s 而不是 30s（= TTL）：最坏情况下任务多占 5s 才被发现，
	// 用户等待可接受；再小则空扫太频繁。
	LeaseSweepIntervalSec = 5
	// RetryBackoffSec 是重派退避序列（契约 §3.3）：第 n 次失败后等多久。
	//
	// 第 1/2/3 次失败 → 10s / 60s / 300s；超出序列则一直用最后一个。
	RetryBackoffSec = "10,60,300"
)

// ErrInvalidTransition 表示状态机拒绝该迁移（契约 §3.2 表外的一切组合）。
var ErrInvalidTransition = errors.New("invalid_transition")

// ErrTransitionGuardFailed 表示迁移的附加守卫条件不满足（状态本身没变）。
// 例如租约巡检发现任务刚被心跳续期——这不是错误，是「这次不该动它」。
var ErrTransitionGuardFailed = errors.New("transition_guard_failed")

// Task 是 tasks 表的一行：任务行 = OTel span。
//
// Out 三态必须靠 Out + Reported 两列表达（不变量 I5）：
//
//	Reported=false, Out=nil     → 没回报（漏报/超时），不派下一跳，也不伪造「到头」
//	Reported=true,  Out=nil     → 已回报，到头了
//	Reported=true,  Out=&"fix"  → 已回报，走 fix 出口
//
// Omitempty **不能**加在 Out 上：nil 与 "缺失字段" 在出站 JSON 里虽然都是 null，
// 但 Reported 才是权威判据（前端/节点都不该靠 out 是否存在来推断）。
//
// NamespaceID 是**命名空间隔离维度**：任务属于某个命名空间，读路径一律按它隔离。
type Task struct {
	ID           string          `json:"id"`
	NamespaceID  string          `json:"namespaceId"`
	TraceID      string          `json:"traceId"`
	ParentTaskID *string         `json:"parentTaskId"`
	Path         []string        `json:"path"`
	CapabilityID string          `json:"capabilityId"`
	Capability   string          `json:"capability"`
	Version      int             `json:"version"`
	Params       json.RawMessage `json:"params"`
	Status       string          `json:"status"`
	NodeID       *string         `json:"nodeId"`
	InstanceID   *string         `json:"instanceId"`
	LeaseUntil   *int64          `json:"leaseUntil"`
	Attempts     int             `json:"attempts"`
	Out          *string         `json:"out"`
	Reported     bool            `json:"reported"`
	Summary      string          `json:"summary"`
	Artifacts    []string        `json:"artifacts"`
	Error        string          `json:"error"`
	CapHash      string          `json:"capHash"`
	GraphID      string          `json:"graphId"`
	CreatedAt    int64           `json:"createdAt"`
	StartedAt    *int64          `json:"startedAt"`
	FinishedAt   *int64          `json:"finishedAt"`
	// NotBefore 是退避时间：早于此刻的 pending 任务不该被重派（契约 §3.3）。
	// nil 表示立刻可派。
	NotBefore *int64 `json:"notBefore"`
}

// taskColumns 是统一的列顺序，scanTask 依赖它。
const taskColumns = `id, namespace_id, trace_id, parent_task_id, path, capability_id, capability,
	version, params, status, node_id, instance_id, lease_until, attempts,
	out, reported, summary, artifacts, error, cap_hash, graph_id,
	created_at, started_at, finished_at, not_before`

// scanTask 从 rows 解析一行。
func scanTask(rows *sql.Rows) (*Task, error) {
	var t Task
	var path, params, artifacts string
	var reported int
	if err := rows.Scan(
		&t.ID, &t.NamespaceID, &t.TraceID, &t.ParentTaskID, &path, &t.CapabilityID, &t.Capability,
		&t.Version, &params, &t.Status, &t.NodeID, &t.InstanceID, &t.LeaseUntil, &t.Attempts,
		&t.Out, &reported, &t.Summary, &artifacts, &t.Error, &t.CapHash, &t.GraphID,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.NotBefore,
	); err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Reported = reported != 0
	if err := json.Unmarshal([]byte(path), &t.Path); err != nil {
		return nil, fmt.Errorf("decode task path: %w", err)
	}
	if t.Path == nil {
		t.Path = []string{} // 不变量 I9
	}
	if err := json.Unmarshal([]byte(params), &t.Params); err != nil {
		return nil, fmt.Errorf("decode task params: %w", err)
	}
	if err := json.Unmarshal([]byte(artifacts), &t.Artifacts); err != nil {
		return nil, fmt.Errorf("decode task artifacts: %w", err)
	}
	if t.Artifacts == nil {
		t.Artifacts = []string{}
	}
	return &t, nil
}

// CreateTasks 在单个事务内批量建任务：扇出必须原子（要么全成要么全不成）。
//
// 空入参是 no-op（不是错误）：扇出的每条边都可能被剪枝，最后一条不剩是正常情况。
//
// 命名空间（契约 §2.3 / §2.4）：每行必须带 NamespaceID（空 → ErrNamespaceRequired）；
// 带 ParentTaskID 时走 I11④——子任务强制继承父任务的命名空间，
// 显式给了不同值 → *CrossNamespaceError。
//
// #140 起内部拆成「开事务 + createTasksTx + 提交」三段，签名/行为/错误文案不变：
// 拆出的 createTasksTx 要能被 ReportResultAndCreateTasks 复用，与终态写入共用一个事务。
func (d *DB) CreateTasks(ctx context.Context, ts []*Task) error {
	if len(ts) == 0 {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.createTasksTx(ctx, tx, ts); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create tasks: %w", err)
	}
	return nil
}

// createTasksTx 是 CreateTasks 的事务内版本：在调用方事务里插任务行（#140）。
//
// 本体从 CreateTasks 整段搬移，校验顺序沿用旧版（nil → 必填字段 → 只能落 pending，
// 前面插入 I11④ 的父任务命名空间继承）；BeginTx/Commit/Rollback 归调用方。
// 只有和 reportResultTx 共用一个 tx 句柄，「父任务终态 + 下一跳子任务行」才能
// 同生共死（契约 §1.1）：子任务插入失败时整个事务回滚，父任务不会留在终态。
func (d *DB) createTasksTx(ctx context.Context, tx *sql.Tx, ts []*Task) error {
	if len(ts) == 0 {
		return nil
	}
	for i, t := range ts {
		if t == nil {
			return fmt.Errorf("create tasks: nil task at index %d", i)
		}
		if t.TraceID == "" || t.CapabilityID == "" || t.Capability == "" {
			return fmt.Errorf("create tasks: task %d missing namespace/trace/capability", i)
		}
		// I11④：子任务的 namespace_id 强制继承父任务的（契约 §2.4）。
		// 必须在「必填」校验之前：没给 namespace 的子任务直接继承，不算缺字段。
		if t.ParentTaskID != nil && *t.ParentTaskID != "" {
			if err := inheritParentNamespaceTx(ctx, tx, t); err != nil {
				return fmt.Errorf("create tasks: task %d: %w", i, err)
			}
		}
		if t.NamespaceID == "" || t.NamespaceID == AllNamespaces {
			return fmt.Errorf("create tasks: task %d: %w", i, ErrNamespaceRequired)
		}
		if t.ID == "" {
			t.ID = NewTaskID()
		}
		if t.Status == "" {
			t.Status = TaskPending
		}
		if t.Status != TaskPending {
			// 建任务只能落在 pending：其他状态必须经 Transition 推进（契约 §3.2）
			return fmt.Errorf("create tasks: task %d must start as pending, got %q", i, t.Status)
		}
		if t.CreatedAt == 0 {
			t.CreatedAt = Now()
		}
		if t.Path == nil {
			t.Path = []string{}
		}
		if t.Params == nil {
			t.Params = json.RawMessage("{}")
		}
		if t.Artifacts == nil {
			t.Artifacts = []string{}
		}
		pathJSON, err := json.Marshal(t.Path)
		if err != nil {
			return fmt.Errorf("marshal task path: %w", err)
		}
		artifactsJSON, err := json.Marshal(t.Artifacts)
		if err != nil {
			return fmt.Errorf("marshal task artifacts: %w", err)
		}
		reported := 0
		if t.Reported {
			reported = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks
			   (id, namespace_id, trace_id, parent_task_id, path, capability_id, capability,
			    version, params, status, node_id, instance_id, lease_until, attempts,
			    out, reported, summary, artifacts, error, cap_hash, graph_id,
			    created_at, started_at, finished_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.NamespaceID, t.TraceID, t.ParentTaskID, string(pathJSON), t.CapabilityID, t.Capability,
			t.Version, string(t.Params), t.Status, t.NodeID, t.InstanceID, t.LeaseUntil, t.Attempts,
			t.Out, reported, t.Summary, string(artifactsJSON), t.Error, t.CapHash, t.GraphID,
			t.CreatedAt, t.StartedAt, t.FinishedAt,
		); err != nil {
			return fmt.Errorf("insert task %d (%s): %w", i, t.Capability, err)
		}
	}
	return nil
}

// inheritParentNamespaceTx 实现 I11④：子任务的 namespace_id 必须与父任务一致（契约 §2.4）。
//
// 两档：
//   - 调用方没给（空）→ 直接继承父任务的，不必让每个调用点自己填；
//   - 给了但与父任务不同 → *CrossNamespaceError（handler 一律 400，不静默修正）。
//
// 父任务行不存在时**既不报错也不改**：那种引用指向的不是「另一个命名空间的对象」，
// 而是一个不存在的对象。本函数之前也不校验父任务存在性，这个洞不该由 I11 来补
// （补了反而会让「先建子任务行、后补父任务」的历史数据无法重放）。
//
// 前序任务在本批里先插后查，因此父任务即使是同批的前一行也能查到（同事务可见）。
func inheritParentNamespaceTx(ctx context.Context, tx *sql.Tx, t *Task) error {
	var parentNS string
	err := tx.QueryRowContext(ctx,
		`SELECT namespace_id FROM tasks WHERE id = ?`, *t.ParentTaskID).Scan(&parentNS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read parent namespace: %w", err)
	}
	if t.NamespaceID == "" {
		t.NamespaceID = parentNS
		return nil
	}
	if t.NamespaceID != parentNS {
		return &CrossNamespaceError{
			What: "tasks.parent_task_id",
			Want: parentNS,
			Got:  t.NamespaceID,
		}
	}
	return nil
}

// GetTask 按 (namespaceID, id) 取任务；未命中返回 ErrNotFound。
func (d *DB) GetTask(ctx context.Context, namespaceID, id string) (*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get task: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanTask(rows)
}

// ListTasks 列出命名空间内的任务，过滤条件为空表示不过滤（traceID/nodeID/status）；
// 空列表返回 []。
func (d *DB) ListTasks(ctx context.Context, namespaceID, traceID, nodeID, status string, limit int) ([]*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE namespace_id = ?`
	args := []any{namespaceID}
	if traceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, traceID)
	}
	if nodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ValidTransition 判定 from → to 是否在契约 §3.2 的迁移表内。
//
// 逐条对应契约表格，表外一律 false：
//
//	pending      → assigned | canceled
//	assigned     → acked | running | lease_expired | canceled
//	acked        → running | lease_expired | canceled
//
// 关于 `assigned → running`：契约 §3.2 的表格里没直接画这条，
// 但 §3.1 明确「running 由 progress 帧**或隐式**推进」。节点跳过 ack
// 直接报 progress/result 是正常时序（ack 只是「接单」，不是「必须经过」），
// 不允许这条会让这类节点永远推不动任务。
//
//	running      → succeeded | failed | lease_expired | canceled
//	lease_expired→ pending | abandoned
//	终态         → （无）
func ValidTransition(from, to string) bool {
	if IsTerminalStatus(from) {
		return false // 终态不可再变
	}
	switch from {
	case TaskPending:
		return to == TaskAssigned || to == TaskCanceled
	case TaskAssigned:
		return to == TaskAcked || to == TaskRunning ||
			to == TaskLeaseExpired || to == TaskCanceled
	case TaskAcked:
		return to == TaskRunning || to == TaskLeaseExpired || to == TaskCanceled
	case TaskRunning:
		return to == TaskSucceeded || to == TaskFailed ||
			to == TaskLeaseExpired || to == TaskCanceled
	case TaskLeaseExpired:
		return to == TaskPending || to == TaskAbandoned
	}
	return false
}

// IsTerminalStatus 判定是否终态（succeeded/failed/canceled/abandoned）。
func IsTerminalStatus(status string) bool {
	switch status {
	case TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned:
		return true
	}
	return false
}

// Transition 是**唯一**改 tasks.status 的入口（契约 §3）。
//
// 两步校验：
//  1. 迁移表：表外组合直接 ErrInvalidTransition（不碰库）
//  2. CAS：UPDATE ... WHERE id=? AND status=?；RowsAffected()==0 说明并发被抢或任务不存在
//
// 写 finished_at：进入终态时落时间戳。
func (d *DB) Transition(ctx context.Context, namespaceID, taskID, from, to string) error {
	return d.transitionTx(ctx, nil, namespaceID, taskID, from, to)
}

// transitionTx 是改 tasks.status 的**唯一**执行点。
//
// executor 为 nil 时直接用连接池（自动提交）；传入 *sql.Tx 则在调用方事务内执行
// （租约巡检需要「改状态 + 决定去向 + 释放槽位」原子完成）。
//
// 全仓（除本函数外）**不得**出现 `UPDATE tasks SET status`——
// TestStatusWriterIsUnique 会静态检查这一点。
func (d *DB) transitionTx(ctx context.Context, tx *sql.Tx, namespaceID, taskID, from, to string) error {
	return d.transitionGuarded(ctx, tx, namespaceID, taskID, from, to, "")
}

// transitionGuarded 是 transitionTx 的带守卫版本：extraWhere 会被 AND 到 UPDATE 上，
// 用来表达「迁移的附加前置条件」——例如租约巡检要求「租约确实已过期」。
//
// 为什么守卫必须和 UPDATE 同一条语句：分成「先查是否过期、再改状态」两步，
// 心跳插入两次查询之间就会把一条刚续期的任务误回收。
// 守卫跟 CAS 在同一句里，SQLite 的行锁保证判断与更新原子。
//
// extraWhere 只能是本包内的常量片段（不含用户输入），args 里追加对应的参数。
func (d *DB) transitionGuarded(ctx context.Context, tx *sql.Tx, namespaceID, taskID, from, to string, extraWhere string, extraArgs ...any) error {
	if !ValidTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}

	var (
		q    string
		args []any
	)
	if IsTerminalStatus(to) {
		q = `UPDATE tasks SET status = ?, finished_at = ? WHERE namespace_id = ? AND id = ? AND status = ?`
		args = []any{to, Now(), namespaceID, taskID, from}
	} else {
		q = `UPDATE tasks SET status = ? WHERE namespace_id = ? AND id = ? AND status = ?`
		args = []any{to, namespaceID, taskID, from}
	}
	if extraWhere != "" {
		q += " AND " + extraWhere
		args = append(args, extraArgs...)
	}

	var (
		res sql.Result
		err error
	)
	if tx != nil {
		res, err = tx.ExecContext(ctx, q, args...)
	} else {
		res, err = d.ExecContext(ctx, q, args...)
	}
	if err != nil {
		return fmt.Errorf("transition %s → %s: %w", from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition rows affected: %w", err)
	}
	if n == 0 {
		// 区分「任务不存在」「状态已被别人改走」「守卫条件不满足」
		var cur string
		var qerr error
		if tx != nil {
			qerr = tx.QueryRowContext(ctx,
				`SELECT status FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, taskID).Scan(&cur)
		} else {
			qerr = d.QueryRowContext(ctx,
				`SELECT status FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, taskID).Scan(&cur)
		}
		if errors.Is(qerr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if qerr != nil {
			return fmt.Errorf("transition: read current status: %w", qerr)
		}
		if cur == from {
			// 状态没变，说明是守卫条件不满足（例如租约刚被续期）
			return ErrTransitionGuardFailed
		}
		return fmt.Errorf("%w: task %s is %s, not %s", ErrInvalidTransition, taskID, cur, from)
	}
	return nil
}

// AssignTask 派发：pending → assigned，写 node/instance/lease_until 并 attempts+1。
//
// 两次写（改状态、写归属）现在在**一个事务**里（#141）；签名与语义不变。
// 先 CAS 改状态（迁移表 + 并发抢占），成功后再写归属信息——顺序反过来会出现
// 「任务已被别人派走，但 node_id 已经被覆盖」的脏数据。
func (d *DB) AssignTask(ctx context.Context, namespaceID, taskID, nodeID, instanceID string, leaseUntil int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin assign task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.assignTaskTx(ctx, tx, namespaceID, taskID, nodeID, instanceID, leaseUntil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit assign task: %w", err)
	}
	return nil
}

// assignTaskTx 是派发后半段的共用实现：改状态 + 写归属/attempts。
//
// AssignTask（拆开用，仍是单事务）与 LeaseAndAssign（占槽 + 派发一事务）共用它，
// 保证两条路径写进去的字段与 attempts 时机一字不差。
// transitionGuarded 传 tx：状态写仍走唯一入口，且不会去抢连接池的那根连接。
func (d *DB) assignTaskTx(ctx context.Context, tx *sql.Tx, namespaceID, taskID, nodeID, instanceID string, leaseUntil int64) error {
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskPending, TaskAssigned, ""); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET node_id = ?, instance_id = ?, lease_until = ?, attempts = attempts + 1
		 WHERE namespace_id = ? AND id = ?`,
		nodeID, nullIfEmpty(instanceID), leaseUntil, namespaceID, taskID); err != nil {
		return fmt.Errorf("assign task: %w", err)
	}
	return nil
}

// nullIfEmpty 把空串转成 NULL（instanceID 可为空，表示暂未绑槽位）。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ExtendLease 按心跳把租约推到 leaseUntil（只对 assigned/acked/running 生效）。
//
// 返回受影响行数 0 表示任务不在可续租状态（已终态/被回收），调用方据此判断
// 是否该给节点回 lease_expired 错误帧。
func (d *DB) ExtendLease(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error {
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		 WHERE namespace_id = ? AND id = ? AND status IN (?,?,?)`,
		leaseUntil, namespaceID, taskID, TaskAssigned, TaskAcked, TaskRunning)
	if err != nil {
		return fmt.Errorf("extend lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("extend lease rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExtendLeasesForNode 把节点上所有在跑任务的租约推到 leaseUntil（心跳批量续期）。
//
// 节点级别的心跳不该只续「一条」任务——它是一条连接的生命信号，
// 该连接上所有任务的租约一起前进（契约 §3.3「任何上行帧续命」）。
// 返回续到期的任务数。
func (d *DB) ExtendLeasesForNode(ctx context.Context, namespaceID, nodeID string, leaseUntil int64) (int, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		 WHERE namespace_id = ? AND node_id = ? AND status IN (?,?,?)`,
		leaseUntil, namespaceID, nodeID, TaskAssigned, TaskAcked, TaskRunning)
	if err != nil {
		return 0, fmt.Errorf("extend leases for node: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("extend leases for node rows: %w", err)
	}
	return int(n), nil
}

// ReportResult 记录节点回报的结果并推进到终态（#141：一个事务内完成）。
//
// reported=false 表示帧里缺 out 字段（没回报出口），此时 Out 留 NULL；
// reported=true 且 out=nil 表示「明确到头」（不变量 I5）。
//
// 节点可能没发 progress 帧就直接 result，所以 assigned/acked 先隐式进 running
// （契约 §3.1「running 由 progress 帧或隐式推进」）。
//
// 一个 BEGIN/COMMIT 里完成：隐式 running + started_at + running→终态 +
// 明细字段 + 释放槽位。拆开时最坏的中间态是「任务已终态、槽位却没释放」——
// 那一个并发额度会一直占到巡检或重启为止，而现在要么全成要么全不成。
//
// #140 起它是 ReportResultAndCreateTasks(next=nil) 的薄壳：签名、返回值、错误文案
// 全部保持不变，只是实现搬进了共用的事务内助手（reportResultTx）。
func (d *DB) ReportResult(ctx context.Context, namespaceID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string) error {
	return d.ReportResultAndCreateTasks(ctx, namespaceID, taskID, status, out, reported, summary, artifacts, errMsg, nil)
}

// ReportResultAndCreateTasks 在**一个事务**里完成「终态写入 + 下一跳建任务行」（#140）。
//
// 为什么必须同一个事务：旧实现是「终态先提交，下一跳在后台 goroutine 里查出口表
// 再建行」。进程在两者之间退出，就会留下父任务 succeeded + out=fix、子任务行不存在
// 的**永久空洞**——trace 看起来一切正常，链却永远停了（ListRedispatchable 只捞
// `status='pending' AND node_id IS NULL`，捞不到一条根本不存在的行）。
//
// 事务内顺序：先终态写（reportResultTx），后插子任务（createTasksTx）；next 为空
// 等价于旧的 ReportResult 行为。**子任务插入失败 → 整体回滚**：父任务不会留在终态，
// 不会出现「父 succeeded、子任务不存在」的空洞（宁可让父任务留在 running 被巡检退回
// 重派、重复执行一次幂等能力，也胜于永久断链）。
//
// 补偿路径（崩溃点回归，契约 §0 第 3 条）：本事务 COMMIT 之后，即使调用方的 publish
// 丢失 / 进程退出 / 返回错误，next 里的行已经落库并且是 `status='pending'` +
// `node_id IS NULL` + `attempts=0` —— 这正是 store.ListRedispatchable 的筛选条件，
// internal/lease 的巡检 sweeper（每 5s tick，Redispatch = Dispatcher.RedispatchTask）
// 会把它们捞回来重新派发。所以「终态 + 子任务行」一次提交就够了，publish 不需要也不该
// 进事务（网络操作不能进事务）。
//
// 状态写入仍只走 transitionGuarded（在 reportResultTx 内，同一个 tx 句柄）；
// 本函数不新增任何 `UPDATE tasks SET status` 语句。
func (d *DB) ReportResultAndCreateTasks(ctx context.Context, namespaceID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string, next []*Task) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin report result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.reportResultTx(ctx, tx, namespaceID, taskID, status, out, reported, summary, artifacts, errMsg); err != nil {
		return err
	}
	if len(next) > 0 {
		// 下一跳建行：与终态同一个事务（见本函数文档里的补偿路径与回滚语义）
		if err := d.createTasksTx(ctx, tx, next); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit report result: %w", err)
	}
	return nil
}

// reportResultTx 是 ReportResult 的事务内版本：调用方负责 BeginTx/Commit/Rollback。
//
// 本体从 ReportResult 整段搬移（#140），逻辑、错误文案、SQL 一字未改；唯一去掉的是
// 自己开事务/提交那一段——这样它才能与 createTasksTx 共用一个事务。
// 参数校验（终态白名单 / 空出口名）也跟着搬进来，保证两条入口的错误文案与哨兵值一致。
// 状态写入只用 transitionGuarded，且传的是调用方的同一个 tx 句柄。
func (d *DB) reportResultTx(ctx context.Context, tx *sql.Tx, namespaceID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string) error {
	if status != TaskSucceeded && status != TaskFailed {
		return fmt.Errorf("%w: result status must be succeeded|failed, got %q", ErrInvalidTransition, status)
	}
	if out != nil && *out == "" {
		// '' 不是合法出口名（契约 §2.5：out NULL 表示没回报，'' 不是合法值）
		return fmt.Errorf("report result: empty out string is not a valid port (use nil)")
	}

	// status 与 node_id 必须用 **tx** 读：DSN 是 MaxOpenConns(1)，走连接池
	// （d.GetTask）会和本事务抢同一根连接，直接自锁。
	var (
		cur    string
		nodeID sql.NullString
	)
	err := tx.QueryRowContext(ctx,
		`SELECT status, node_id FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, taskID).
		Scan(&cur, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("report result: read task: %w", err)
	}
	if IsTerminalStatus(cur) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, cur)
	}
	// 隐式进 running（result 帧本身表示节点已经在执行了）
	if cur == TaskAssigned || cur == TaskAcked {
		if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, cur, TaskRunning, ""); err != nil {
			return err
		}
		// 节点可能跳过 progress 帧直接 result，这里补 started_at，
		// 否则任务永远没有开始时间（可观测性缺一块）。
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE namespace_id = ? AND id = ?`,
			Now(), namespaceID, taskID); err != nil {
			return fmt.Errorf("report result: set started_at: %w", err)
		}
		cur = TaskRunning
	}
	if cur != TaskRunning {
		return fmt.Errorf("%w: task %s is %s, cannot report result", ErrInvalidTransition, taskID, cur)
	}
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskRunning, status, ""); err != nil {
		return err
	}

	reportedInt := 0
	if reported {
		reportedInt = 1
	}
	// reported=false 时 out 必须留 NULL：不伪造「到头」（不变量 I5）
	var outVal any
	if reported && out != nil {
		outVal = *out
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	artifactsJSON, err := json.Marshal(artifacts)
	if err != nil {
		return fmt.Errorf("marshal artifacts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks
		 SET out = ?, reported = ?, summary = ?, artifacts = ?, error = ?,
		     finished_at = COALESCE(finished_at, ?)
		 WHERE namespace_id = ? AND id = ?`,
		outVal, reportedInt, summary, string(artifactsJSON), errMsg, Now(), namespaceID, taskID); err != nil {
		return fmt.Errorf("report result details: %w", err)
	}

	// 释放槽位：并进同一个事务。
	// busy_task = taskID 条件必须保留——槽位可能已被巡检回收并给了别的任务，
	// 不看条件地置 NULL 会把别人的任务踢下槽。
	// RowsAffected()==0 不算错误（正是「巡检已回收」的合法情况）。
	if nodeID.String != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = NULL, updated_at = ?
			 WHERE node_id = ? AND busy_task = ?`, Now(), nodeID.String, taskID); err != nil {
			return fmt.Errorf("report result: release instance: %w", err)
		}
	}
	return nil
}

// MarkRunning 把 acked 推进到 running（progress 帧的显式推进）。
func (d *DB) MarkRunning(ctx context.Context, namespaceID, taskID string) error {
	task, err := d.GetTask(ctx, namespaceID, taskID)
	if err != nil {
		return err
	}
	if task.Status == TaskRunning {
		return nil // 幂等：多条 progress 帧不该报错
	}
	return d.Transition(ctx, namespaceID, taskID, task.Status, TaskRunning)
}

// StartTask 把 assigned/acked 推进到 running，并补 started_at（#141：一个事务）。
//
// 签名与语义不变，只是把「推进状态」与「写 started_at」两次提交压成一次。
func (d *DB) StartTask(ctx context.Context, namespaceID, taskID string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin start task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.startTaskTx(ctx, tx, namespaceID, taskID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit start task: %w", err)
	}
	return nil
}

// startTaskTx 在调用方事务内完成 progress 的前两步：推进到 running + 补 started_at。
//
// 当前状态必须用 **tx** 查（MaxOpenConns(1)：用 d.GetTask 会和本事务抢同一根
// 连接而自锁）。running 幂等：重复 progress 帧不报错，也不覆盖已有 started_at。
// 非 running 时走 transitionGuarded——终态 / lease_expired → running 仍然被拒。
func (d *DB) startTaskTx(ctx context.Context, tx *sql.Tx, namespaceID, taskID string) error {
	var cur string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, taskID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("start task: read current status: %w", err)
	}
	if cur != TaskRunning {
		if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, cur, TaskRunning, ""); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE namespace_id = ? AND id = ?`,
		Now(), namespaceID, taskID); err != nil {
		return fmt.Errorf("start task: set started_at: %w", err)
	}
	return nil
}

// ProgressTask 是 progress 帧的三合一（#141）：→running + started_at + 续租。
//
// 一次 Commit 取代过去的 StartTask + ExtendLease 两次提交。与拆分版的区别
// 只在原子性：不会出现「已经 running、租约却没续上」的中间态（那会被巡检
// 当成过期任务回收到 pending，节点还在跑、中枢已重派）。
//
// 错误语义：任务不存在 → ErrNotFound；终态 / lease_expired → running 非法迁移
// → ErrInvalidTransition；已在 running → 幂等成功。
func (d *DB) ProgressTask(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin progress task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.startTaskTx(ctx, tx, namespaceID, taskID); err != nil {
		return err
	}
	// 续租的条件串（assigned/acked/running）必须保留：终态任务不该被续命。
	// 同事务内刚推进到 running，所以 0 行只可能是状态被另一个写者改走了。
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		 WHERE namespace_id = ? AND id = ? AND status IN (?,?,?)`,
		leaseUntil, namespaceID, taskID, TaskAssigned, TaskAcked, TaskRunning)
	if err != nil {
		return fmt.Errorf("progress task: extend lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("progress task rows: %w", err)
	}
	if n == 0 {
		// 口径对齐 ExtendLease：续不到就是不在这三个状态里
		return fmt.Errorf("%w: task %s is not in assigned|acked|running", ErrNotFound, taskID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit progress task: %w", err)
	}
	return nil
}

// SweepExpiredLeases 扫过期租约（契约 §3.3），返回被回收的任务 ID。
//
// **全局扫描，不按命名空间过滤**：租约过期是客观事实，不能因为调用方不知道哪些
// 命名空间有活任务就漏回收。后续每条写操作都用任务行**自己的** namespace_id 定位
// （契约 §2.1：命名空间资源的作用域参数一律是 namespace_id）。
//
// 三步（每条任务一个事务，互不影响）：
//  1. CAS 进 lease_expired（中间态，不是终态）——只有仍处在 assigned/acked/running
//     且租约确实过期的任务会被选中，所以并发下不会误伤刚续期成功的任务
//  2. 决定去向（顺序即优先级）：
//     · attempts >= 能力 lifecycle.maxAttempts → abandoned（预算用尽，先判，防止无限重投）
//     · assigned 且 started_at IS NULL（领了单但没执行过）→ pending，与幂等性无关
//     · 能力声明 idempotent:false（重放不安全）→ abandoned
//     · 否则 → pending，并按 RetryBackoff 设退避
//  3. 释放它占的实例槽位
func (d *DB) SweepExpiredLeases(ctx context.Context, now int64) (reclaimed []string, err error) {
	if now == 0 {
		now = Now()
	}
	rows, err := d.QueryContext(ctx,
		`SELECT id, namespace_id, capability_id, status, attempts, node_id, started_at
		 FROM tasks
		 WHERE status IN (?,?,?) AND lease_until IS NOT NULL AND lease_until < ?
		 ORDER BY lease_until`,
		TaskAssigned, TaskAcked, TaskRunning, now)
	if err != nil {
		return nil, fmt.Errorf("scan expired leases: %w", err)
	}
	type expired struct {
		id, namespaceID, capabilityID, status, nodeID string
		attempts                                      int
		startedAt                                     sql.NullInt64
	}
	var batch []expired
	for rows.Next() {
		var e expired
		var nodeID sql.NullString
		if err := rows.Scan(&e.id, &e.namespaceID, &e.capabilityID, &e.status, &e.attempts, &nodeID, &e.startedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan expired lease: %w", err)
		}
		e.nodeID = nodeID.String
		batch = append(batch, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate expired leases: %w", err)
	}
	rows.Close()

	out := []string{}
	for _, e := range batch {
		ok, err := d.sweepOne(ctx, e.id, e.namespaceID, e.capabilityID, e.status, e.attempts, e.nodeID, e.startedAt, now)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, e.id)
		}
	}
	return out, nil
}

// sweepOne 回收一条过期租约，返回是否真的回收了（CAS 失败 = 别人先处理了）。
//
// namespaceID 来自任务行自己（扫描时 SELECT 出来的），不是调用方传的：全局巡检
// 手里没有「一个命名空间」，只能信任行上的事实。
//
// startedAt 是任务的 started_at：nil 表示**节点领了单但一行代码都没执行过**。
func (d *DB) sweepOne(ctx context.Context, taskID, namespaceID, capabilityID, status string, attempts int, nodeID string, startedAt sql.NullInt64, now int64) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin sweep lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ① assigned/acked/running → lease_expired（中间态，不是终态）
	//
	// 走 transitionGuarded：改 tasks.status 的 SQL 保持唯一（契约 §3），
	// 同时把「租约确实已过期」作为守卫条件并进同一条 UPDATE，
	// 避免「心跳续期」与「巡检回收」交错时误杀刚续约的任务。
	err = d.transitionGuarded(ctx, tx, namespaceID, taskID, status, TaskLeaseExpired,
		"lease_until IS NOT NULL AND lease_until < ?", now)
	if errors.Is(err, ErrTransitionGuardFailed) {
		return false, nil // 心跳赢了，跳过
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidTransition) {
		return false, nil // 被并发回收，或状态已不是扫描时看到的那个
	}
	if err != nil {
		return false, fmt.Errorf("mark lease expired: %w", err)
	}

	// ② 查能力的重试策略，决定回 pending 还是 abandoned
	var lifecycleJSON string
	err = tx.QueryRowContext(ctx,
		`SELECT lifecycle FROM capability_versions WHERE id = ?`, capabilityID).Scan(&lifecycleJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read capability lifecycle: %w", err)
	}
	var lc Lifecycle
	if lifecycleJSON != "" {
		if err := json.Unmarshal([]byte(lifecycleJSON), &lc); err != nil {
			return false, fmt.Errorf("decode capability lifecycle: %w", err)
		}
	}

	to := TaskPending
	finished := false
	reason := "lease_expired: attempts exhausted"
	switch {
	case attempts >= lc.MaxAttemptsOrDefault():
		// ① 重试预算用尽：无论如何都放弃。
		//
		// 这条**必须排在"从未开始"之前**：attempts 在派发成功时就 +1，
		// 一个永远起不来的任务会不断"回 pending → 再派发"。
		// 若"从未开始"无条件优先，它会无限重投、永不收敛。
		to = TaskAbandoned
		finished = true
	case status == TaskAssigned && !startedAt.Valid:
		// ② 领了单但**一行代码都没执行**：重投绝对安全，与幂等性无关。
		//
		// idempotent:false 只在**执行过**时才有意义；从没开始过的任务重投
		// 不存在副作用。这条证据比能力声明更可靠，所以优先于幂等性判断
		// ——以前这类任务被白丢，用户看到 abandoned 而节点根本没动过（#144）。
		to = TaskPending
	case !lc.IsIdempotent():
		// ③ 重放不安全的能力：宁可放弃也不重复执行
		to = TaskAbandoned
		finished = true
		reason = "lease_expired: capability is not idempotent"
	}

	// 状态推进同样走唯一写入点 transitionGuarded（lease_expired → pending | abandoned）
	if err := d.transitionGuarded(ctx, tx, namespaceID, taskID, TaskLeaseExpired, to, ""); err != nil {
		return false, fmt.Errorf("sweep transition to %s: %w", to, err)
	}
	if finished {
		// 补终态附带信息（abandoned 的 finished_at 已由 transitionGuarded 写入）
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE namespace_id = ? AND id = ?`,
			reason, namespaceID, taskID); err != nil {
			return false, fmt.Errorf("annotate abandoned task: %w", err)
		}
	} else {
		// 回 pending：清掉归属，保留 attempts（用于退避与上限判断），
		// 并按 RetryBackoff 设退避 —— 只清归属不设退避的话，这条任务
		// 下一秒就会被重新派发，退避形同虚设、坏节点会被反复打。
		notBefore := now + RetryBackoff(attempts)
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET node_id = NULL, instance_id = NULL, lease_until = NULL, not_before = ?
			 WHERE namespace_id = ? AND id = ?`, notBefore, namespaceID, taskID); err != nil {
			return false, fmt.Errorf("requeue task: %w", err)
		}
	}

	// ③ 释放实例槽位
	if nodeID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = NULL, updated_at = ?
			 WHERE node_id = ? AND busy_task = ?`, now, nodeID, taskID); err != nil {
			return false, fmt.Errorf("release instance on sweep: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit sweep lease: %w", err)
	}
	return true, nil
}

// CancelTask 取消任务（pending/assigned/acked/running → canceled）并释放槽位。
func (d *DB) CancelTask(ctx context.Context, namespaceID, taskID string) error {
	task, err := d.GetTask(ctx, namespaceID, taskID)
	if err != nil {
		return err
	}
	if IsTerminalStatus(task.Status) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, task.Status)
	}
	if task.Status == TaskLeaseExpired {
		return fmt.Errorf("%w: task %s is %s (only pending|abandoned allowed)",
			ErrInvalidTransition, taskID, task.Status)
	}
	if err := d.Transition(ctx, namespaceID, taskID, task.Status, TaskCanceled); err != nil {
		return err
	}
	if task.NodeID != nil {
		if err := d.ReleaseInstanceByTask(ctx, *task.NodeID, taskID); err != nil {
			return err
		}
	}
	return nil
}

// CountTasksByStatus 返回某 trace 下各状态的任务数（供 trace 视图/测试用）。
// traceID 为空时统计整个命名空间。
func (d *DB) CountTasksByStatus(ctx context.Context, namespaceID, traceID string) (map[string]int, error) {
	q := `SELECT status, COUNT(*) FROM tasks WHERE namespace_id = ?`
	args := []any{namespaceID}
	if traceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, traceID)
	}
	q += ` GROUP BY status`
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("count tasks by status: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan status count: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// RetryBackoff 返回第 attempts 次失败后的退避时长（秒）。
//
// 契约 §3.3：10s / 60s / 300s。超出序列则一直用最后一个值，
// 不会无限增长——重试有 attempts 上限兜底，退避只需单调不降。
func RetryBackoff(attempts int) int64 {
	seq := []int64{10, 60, 300}
	if attempts <= 0 {
		return 0
	}
	idx := attempts - 1
	if idx >= len(seq) {
		idx = len(seq) - 1
	}
	return seq[idx]
}

// ListRedispatchable 列出「可以立刻重派」的 pending 任务。
//
// **全局扫描，不按命名空间过滤**：重派是巡检的职责，不能因为调用方不知道哪些
// 命名空间有待派任务就整片饿死。命名空间从行上的 NamespaceID 拿（下游
// Dispatcher 派发时用它做作用域），不需要调用方传。
//
// 三个条件缺一不可：
//   - status = pending（回滚/新建后等待调度的）
//   - node_id IS NULL（没有归属；有归属的说明已经被派过）
//   - not_before IS NULL OR not_before <= now（退避期已过）
//
// 按 not_before、created_at 排序：先重试等得最久的，避免老任务饿死。
func (d *DB) ListRedispatchable(ctx context.Context, now int64, limit int) ([]*Task, error) {
	if now == 0 {
		now = Now()
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE status = ? AND node_id IS NULL
		   AND (not_before IS NULL OR not_before <= ?)
		 ORDER BY COALESCE(not_before, 0), created_at, id
		 LIMIT ?`,
		TaskPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list redispatchable: %w", err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate redispatchable: %w", err)
	}
	if out == nil {
		out = []*Task{} // 不变量 I9
	}
	return out, nil
}

// SetNotBefore 设置任务的退避时间（重派失败时调用）。
//
// 只对 pending 任务生效：其他状态设退避没有意义（它们不在调度池里）。
func (d *DB) SetNotBefore(ctx context.Context, namespaceID, taskID string, notBefore int64) error {
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET not_before = ? WHERE namespace_id = ? AND id = ? AND status = ?`,
		notBefore, namespaceID, taskID, TaskPending)
	if err != nil {
		return fmt.Errorf("set not_before: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	return nil
}

// CountOpenTasksSince 只数命名空间内 createdAt >= since 的未终态任务。
//
// 开发工具要「等本次生产的那批链跑完」，不能把历史遗留的 pending 也算进来
// ——否则永远等不到 0（那些行不会自己消失）。
func (d *DB) CountOpenTasksSince(ctx context.Context, namespaceID string, since int64) (int, error) {
	if namespaceID == "" {
		return 0, fmt.Errorf("count open tasks since: %w", ErrNamespaceRequired)
	}
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks
		 WHERE namespace_id = ? AND created_at >= ?
		   AND status NOT IN ('succeeded','failed','canceled','abandoned')`,
		namespaceID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open tasks since: %w", err)
	}
	return n, nil
}

// CountOpenTasks 返回命名空间内**未到终态**的任务数。
//
// 用途：开发/测试工具想知道「这一批链跑完了没有」。
// 终态集合与 ValidTransition 的口径保持一致（succeeded/failed/canceled/abandoned）。
func (d *DB) CountOpenTasks(ctx context.Context, namespaceID string) (int, error) {
	if namespaceID == "" {
		return 0, fmt.Errorf("count open tasks: %w", ErrNamespaceRequired)
	}
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks
		 WHERE namespace_id = ? AND status NOT IN ('succeeded','failed','canceled','abandoned')`,
		namespaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open tasks: %w", err)
	}
	return n, nil
}
