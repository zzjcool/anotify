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
type Task struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"orgId"`
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
}

// taskColumns 是统一的列顺序，scanTask 依赖它。
const taskColumns = `id, org_id, trace_id, parent_task_id, path, capability_id, capability,
	version, params, status, node_id, instance_id, lease_until, attempts,
	out, reported, summary, artifacts, error, cap_hash, graph_id,
	created_at, started_at, finished_at`

// scanTask 从 rows 解析一行。
func scanTask(rows *sql.Rows) (*Task, error) {
	var t Task
	var path, params, artifacts string
	var reported int
	if err := rows.Scan(
		&t.ID, &t.OrgID, &t.TraceID, &t.ParentTaskID, &path, &t.CapabilityID, &t.Capability,
		&t.Version, &params, &t.Status, &t.NodeID, &t.InstanceID, &t.LeaseUntil, &t.Attempts,
		&t.Out, &reported, &t.Summary, &artifacts, &t.Error, &t.CapHash, &t.GraphID,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt,
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
func (d *DB) CreateTasks(ctx context.Context, ts []*Task) error {
	if len(ts) == 0 {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, t := range ts {
		if t == nil {
			return fmt.Errorf("create tasks: nil task at index %d", i)
		}
		if t.OrgID == "" || t.TraceID == "" || t.CapabilityID == "" || t.Capability == "" {
			return fmt.Errorf("create tasks: task %d missing org/trace/capability", i)
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
			   (id, org_id, trace_id, parent_task_id, path, capability_id, capability,
			    version, params, status, node_id, instance_id, lease_until, attempts,
			    out, reported, summary, artifacts, error, cap_hash, graph_id,
			    created_at, started_at, finished_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.OrgID, t.TraceID, t.ParentTaskID, string(pathJSON), t.CapabilityID, t.Capability,
			t.Version, string(t.Params), t.Status, t.NodeID, t.InstanceID, t.LeaseUntil, t.Attempts,
			t.Out, reported, t.Summary, string(artifactsJSON), t.Error, t.CapHash, t.GraphID,
			t.CreatedAt, t.StartedAt, t.FinishedAt,
		); err != nil {
			return fmt.Errorf("insert task %d (%s): %w", i, t.Capability, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create tasks: %w", err)
	}
	return nil
}

// GetTask 按 (orgID, id) 取任务；未命中返回 ErrNotFound。
func (d *DB) GetTask(ctx context.Context, orgID, id string) (*Task, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE org_id = ? AND id = ?`, orgID, id)
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

// ListTasks 列出任务，过滤条件为空表示不过滤（traceID/nodeID/status）；
// 空列表返回 []。
func (d *DB) ListTasks(ctx context.Context, orgID, traceID, nodeID, status string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE org_id = ?`
	args := []any{orgID}
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
//	assigned     → acked | lease_expired | canceled
//	acked        → running | lease_expired | canceled
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
		return to == TaskAcked || to == TaskLeaseExpired || to == TaskCanceled
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
func (d *DB) Transition(ctx context.Context, orgID, taskID, from, to string) error {
	return d.transitionTx(ctx, nil, orgID, taskID, from, to)
}

// transitionTx 是改 tasks.status 的**唯一**执行点。
//
// executor 为 nil 时直接用连接池（自动提交）；传入 *sql.Tx 则在调用方事务内执行
// （租约巡检需要「改状态 + 决定去向 + 释放槽位」原子完成）。
//
// 全仓（除本函数外）**不得**出现 `UPDATE tasks SET status`——
// TestStatusWriterIsUnique 会静态检查这一点。
func (d *DB) transitionTx(ctx context.Context, tx *sql.Tx, orgID, taskID, from, to string) error {
	return d.transitionGuarded(ctx, tx, orgID, taskID, from, to, "")
}

// transitionGuarded 是 transitionTx 的带守卫版本：extraWhere 会被 AND 到 UPDATE 上，
// 用来表达「迁移的附加前置条件」——例如租约巡检要求「租约确实已过期」。
//
// 为什么守卫必须和 UPDATE 同一条语句：分成「先查是否过期、再改状态」两步，
// 心跳插入两次查询之间就会把一条刚续期的任务误回收。
// 守卫跟 CAS 在同一句里，SQLite 的行锁保证判断与更新原子。
//
// extraWhere 只能是本包内的常量片段（不含用户输入），args 里追加对应的参数。
func (d *DB) transitionGuarded(ctx context.Context, tx *sql.Tx, orgID, taskID, from, to string, extraWhere string, extraArgs ...any) error {
	if !ValidTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}

	var (
		q    string
		args []any
	)
	if IsTerminalStatus(to) {
		q = `UPDATE tasks SET status = ?, finished_at = ? WHERE org_id = ? AND id = ? AND status = ?`
		args = []any{to, Now(), orgID, taskID, from}
	} else {
		q = `UPDATE tasks SET status = ? WHERE org_id = ? AND id = ? AND status = ?`
		args = []any{to, orgID, taskID, from}
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
				`SELECT status FROM tasks WHERE org_id = ? AND id = ?`, orgID, taskID).Scan(&cur)
		} else {
			qerr = d.QueryRowContext(ctx,
				`SELECT status FROM tasks WHERE org_id = ? AND id = ?`, orgID, taskID).Scan(&cur)
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
// 先 CAS 改状态（迁移表 + 并发抢占），成功后再写归属信息——顺序反过来会出现
// 「任务已被别人派走，但 node_id 已经被覆盖」的脏数据。
func (d *DB) AssignTask(ctx context.Context, orgID, taskID, nodeID, instanceID string, leaseUntil int64) error {
	if err := d.Transition(ctx, orgID, taskID, TaskPending, TaskAssigned); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE tasks SET node_id = ?, instance_id = ?, lease_until = ?, attempts = attempts + 1
		 WHERE org_id = ? AND id = ?`,
		nodeID, nullIfEmpty(instanceID), leaseUntil, orgID, taskID); err != nil {
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
func (d *DB) ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error {
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		 WHERE org_id = ? AND id = ? AND status IN (?,?,?)`,
		leaseUntil, orgID, taskID, TaskAssigned, TaskAcked, TaskRunning)
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
func (d *DB) ExtendLeasesForNode(ctx context.Context, orgID, nodeID string, leaseUntil int64) (int, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		 WHERE org_id = ? AND node_id = ? AND status IN (?,?,?)`,
		leaseUntil, orgID, nodeID, TaskAssigned, TaskAcked, TaskRunning)
	if err != nil {
		return 0, fmt.Errorf("extend leases for node: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("extend leases for node rows: %w", err)
	}
	return int(n), nil
}

// ReportResult 记录节点回报的结果并推进到终态。
//
// reported=false 表示帧里缺 out 字段（没回报出口），此时 Out 留 NULL；
// reported=true 且 out=nil 表示「明确到头」（不变量 I5）。
//
// 节点可能没发 progress 帧就直接 result，所以 assigned/acked 先隐式进 running
// （契约 §3.1「running 由 progress 帧或隐式推进」）。
func (d *DB) ReportResult(ctx context.Context, orgID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string) error {
	if status != TaskSucceeded && status != TaskFailed {
		return fmt.Errorf("%w: result status must be succeeded|failed, got %q", ErrInvalidTransition, status)
	}
	if out != nil && *out == "" {
		// '' 不是合法出口名（契约 §2.5：out NULL 表示没回报，'' 不是合法值）
		return fmt.Errorf("report result: empty out string is not a valid port (use nil)")
	}
	task, err := d.GetTask(ctx, orgID, taskID)
	if err != nil {
		return err
	}
	if IsTerminalStatus(task.Status) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, task.Status)
	}
	// 隐式进 running（result 帧本身表示节点已经在执行了）
	cur := task.Status
	if cur == TaskAssigned || cur == TaskAcked {
		if err := d.Transition(ctx, orgID, taskID, cur, TaskRunning); err != nil {
			return err
		}
		// 节点可能跳过 progress 帧直接 result，这里补 started_at，
		// 否则任务永远没有开始时间（可观测性缺一块）。
		if _, err := d.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE org_id = ? AND id = ?`,
			Now(), orgID, taskID); err != nil {
			return fmt.Errorf("report result: set started_at: %w", err)
		}
		cur = TaskRunning
	}
	if cur != TaskRunning {
		return fmt.Errorf("%w: task %s is %s, cannot report result", ErrInvalidTransition, taskID, cur)
	}
	if err := d.Transition(ctx, orgID, taskID, TaskRunning, status); err != nil {
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
	if _, err := d.ExecContext(ctx,
		`UPDATE tasks
		 SET out = ?, reported = ?, summary = ?, artifacts = ?, error = ?,
		     finished_at = COALESCE(finished_at, ?)
		 WHERE org_id = ? AND id = ?`,
		outVal, reportedInt, summary, string(artifactsJSON), errMsg, Now(), orgID, taskID); err != nil {
		return fmt.Errorf("report result details: %w", err)
	}
	return nil
}

// MarkRunning 把 acked 推进到 running（progress 帧的显式推进）。
func (d *DB) MarkRunning(ctx context.Context, orgID, taskID string) error {
	task, err := d.GetTask(ctx, orgID, taskID)
	if err != nil {
		return err
	}
	if task.Status == TaskRunning {
		return nil // 幂等：多条 progress 帧不该报错
	}
	return d.Transition(ctx, orgID, taskID, task.Status, TaskRunning)
}

// StartTask 把 assigned/acked 推进到 running，并从 dropped 该槽位的角度补 started_at。
func (d *DB) StartTask(ctx context.Context, orgID, taskID string) error {
	if err := d.MarkRunning(ctx, orgID, taskID); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE org_id = ? AND id = ?`,
		Now(), orgID, taskID); err != nil {
		return fmt.Errorf("start task: %w", err)
	}
	return nil
}

// SweepExpiredLeases 扫过期租约（契约 §3.3），返回被回收的任务 ID。
//
// 三步（每条任务一个事务，互不影响）：
//  1. CAS 进 lease_expired（中间态，不是终态）——只有仍处在 assigned/acked/running
//     且租约确实过期的任务会被选中，所以并发下不会误伤刚续期成功的任务
//  2. 决定去向：
//     · attempts >= 能力 lifecycle.maxAttempts → abandoned
//     · 能力声明 idempotent:false（重放不安全）→ abandoned
//     · 否则 → pending，等下一次派发
//  3. 释放它占的实例槽位
func (d *DB) SweepExpiredLeases(ctx context.Context, now int64) (reclaimed []string, err error) {
	if now == 0 {
		now = Now()
	}
	rows, err := d.QueryContext(ctx,
		`SELECT id, org_id, capability_id, status, attempts, node_id
		 FROM tasks
		 WHERE status IN (?,?,?) AND lease_until IS NOT NULL AND lease_until < ?
		 ORDER BY lease_until`,
		TaskAssigned, TaskAcked, TaskRunning, now)
	if err != nil {
		return nil, fmt.Errorf("scan expired leases: %w", err)
	}
	type expired struct {
		id, orgID, capabilityID, status, nodeID string
		attempts                                int
	}
	var batch []expired
	for rows.Next() {
		var e expired
		var nodeID sql.NullString
		if err := rows.Scan(&e.id, &e.orgID, &e.capabilityID, &e.status, &e.attempts, &nodeID); err != nil {
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
		ok, err := d.sweepOne(ctx, e.id, e.orgID, e.capabilityID, e.status, e.attempts, e.nodeID, now)
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
func (d *DB) sweepOne(ctx context.Context, taskID, orgID, capabilityID, status string, attempts int, nodeID string, now int64) (bool, error) {
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
	err = d.transitionGuarded(ctx, tx, orgID, taskID, status, TaskLeaseExpired,
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
	if !lc.IsIdempotent() {
		// 重放不安全的能力：宁可放弃也不重复执行
		to = TaskAbandoned
		finished = true
	} else if attempts >= lc.MaxAttemptsOrDefault() {
		to = TaskAbandoned
		finished = true
	}

	// 状态推进同样走唯一写入点 transitionGuarded（lease_expired → pending | abandoned）
	if err := d.transitionGuarded(ctx, tx, orgID, taskID, TaskLeaseExpired, to, ""); err != nil {
		return false, fmt.Errorf("sweep transition to %s: %w", to, err)
	}
	if finished {
		// 补终态附带信息（abandoned 的 finished_at 已由 transitionGuarded 写入）
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE org_id = ? AND id = ?`,
			"lease_expired: attempts exhausted", orgID, taskID); err != nil {
			return false, fmt.Errorf("annotate abandoned task: %w", err)
		}
	} else {
		// 回 pending：清掉归属，保留 attempts（用于退避与上限判断）
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET node_id = NULL, instance_id = NULL, lease_until = NULL
			 WHERE org_id = ? AND id = ?`, orgID, taskID); err != nil {
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
func (d *DB) CancelTask(ctx context.Context, orgID, taskID string) error {
	task, err := d.GetTask(ctx, orgID, taskID)
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
	if err := d.Transition(ctx, orgID, taskID, task.Status, TaskCanceled); err != nil {
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
func (d *DB) CountTasksByStatus(ctx context.Context, orgID, traceID string) (map[string]int, error) {
	q := `SELECT status, COUNT(*) FROM tasks WHERE org_id = ?`
	args := []any{orgID}
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
