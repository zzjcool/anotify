package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 本文件是「执行事实」（task_attempts）的唯一归属：
//
//   - 每个 attempt 的机器配置快照（grant 那一瞬的 local > node 合并结果 + 内容 hash）；
//   - 每个 attempt 的物理槽绑定（instance_id / slot）、授权摘要（lease / token hash）；
//   - 每个 attempt 的生命周期诊断（phase / error / started / finished / reaped）。
//
// 逻辑任务仍然只有 tasks 一行（attempt 不是 child task，也不参与路由）。历史 attempt
// 的可定位性靠这张表，不靠 tasks 上「当前 attempt」的字段反推——重试会覆盖那些字段，
// 覆盖之后就再也找不到旧执行占的是哪个物理槽了（旧 ACK 因此烧掉槽位）。

// attempt 级回收原因（reclaim_reason）：记录「reaped 这个事实」是谁证明的。
// 它们是**诊断值**，不是 wire 字段（最终帧字段由 T3 冻结后由 G2 映射）。
const (
	// ReclaimReasonResult 是正常 task.result：结果到达本身就证明子进程已退出。
	ReclaimReasonResult = "result"
	// ReclaimReasonCancelAck 是 cancel.ack 带回的 processGroupReaped=true。
	ReclaimReasonCancelAck = "cancel_ack"
	// ReclaimReasonNodeIdle 是节点对具体实例的明确空闲事实（ReclaimStaleSlots）。
	ReclaimReasonNodeIdle = "node_instance_idle"
	// ReclaimReasonNodeReport 是节点按历史绑定上报的 reaped 事实（ReapTaskAttempt）。
	ReclaimReasonNodeReport = "node_report"
)

// attempt 级诊断相位（**不是**任务状态机）：grant 时 running，结束/重排时落成对应值。
// "requeued" 表示这一代执行被租约巡检作废（进程可能还在跑，需要等 reaped 事实）。
const (
	AttemptPhaseRunning  = "running"
	AttemptPhaseRequeued = "requeued"
)

// task_attempts 相关的哨兵错误。
var (
	// ErrNoAttemptSnapshot 表示任务当前 attempt 没有机器输入快照。
	//
	// 触发场景：任务还没 grant（attempt=0），或库里的 attempt 行缺失。此时**不能**
	// 回退去读「当前 node_configs」假装解析成功——那会把排队期间的配置变更算进一次
	// 早已开始的执行（同一 attempt 中途换值）。调用方（G2）应在 grant 之后取输入。
	ErrNoAttemptSnapshot = errors.New("no_attempt_snapshot")
	// ErrConfigNotApplied 表示 grant 时节点的 applied 机器配置与当前 revision/hash 不一致。
	//
	// 它不是执行失败：不计 attempt、不占槽，任务回 pending + waiting_for_config（带退避）。
	// 语义与 task.reject 的 stale_config 同源，但这个判断发生在授权事务里
	// （assign 与 grant 之间配置又变了的竞态）。
	ErrConfigNotApplied = errors.New("config_not_applied")
)

// TaskAttempt 是一次执行（attempt）的事实行。
//
// JSON 面只暴露诊断安全的身份/版本/时间：机器 vars 值、机器 limits 值、token hash
// 都不回显（它们是内部执行输入/凭证摘要）。MachineVarSources 只有名字 → 来源层。
type TaskAttempt struct {
	CapabilityID string                  `json:"-"`
	Artifacts    []protocol.TaskArtifact `json:"artifacts"`
	ResultJSON   json.RawMessage         `json:"-"`
	ExitCode     *int                    `json:"exitCode"`
	// Trusted first acceptance is not the Node process finish time.
	ResultAcceptedAt *int64 `json:"-"`
	TaskID           string `json:"taskId"`
	Attempt          int    `json:"attempt"`
	NodeID           string `json:"nodeId"`
	NodeName         string `json:"nodeName"`
	// InstanceID / Slot 是这个 attempt 占的物理槽（内部事实：释放只能按它精确判定）。
	InstanceID string `json:"-"`
	Slot       int    `json:"slot"`
	// LeaseID 是这一代执行的 lease（历史 ACK 用它核对「这是不是那次执行」）。
	LeaseID string `json:"leaseId"`
	// AttemptTokenHash 是 attempt token 的 sha256（明文只在下发时存在一次）。
	AttemptTokenHash string `json:"-"`
	// ConfigRevision / ConfigHash 是 grant 时机器 applied 的版本与内容摘要。
	ConfigRevision int64  `json:"configRevision"`
	ConfigHash     string `json:"configHash"`
	// MachineVars 是 local > node 合并后的机器 vars 快照（非敏感；不含 ns/token/凭证）。
	MachineVars map[string]string `json:"-"`
	// MachineVarSources 是每个机器 var 的来源层（local / node）——没有值，可公开。
	MachineVarSources map[string]string `json:"machineVarSources"`
	// MachineLimits 是 local > node 逐叶合并后的机器 limits 快照（内部执行输入）。
	MachineLimits map[string]any `json:"-"`
	// NamespaceVarsRevision 是任务接受时钉住的 ns 快照版本（跨 attempt 不变，仅诊断）。
	NamespaceVarsRevision int64 `json:"namespaceVarsRevision"`
	// Phase / Error 是 attempt 级诊断（running/requeued/终态；不是任务相位）。
	Phase string `json:"phase"`
	Error string `json:"error"`
	// StartedAt / FinishedAt 是这一代执行的起止；ReapedAt 是进程组回收事实。
	StartedAt     *int64 `json:"startedAt"`
	FinishedAt    *int64 `json:"finishedAt"`
	ReapedAt      *int64 `json:"reapedAt"`
	ReclaimReason string `json:"reclaimReason"`
}

// taskAttemptColumns 是统一的列顺序，scanTaskAttempt 依赖它。
const taskAttemptColumns = `task_id, attempt, node_id, node_name, instance_id, slot,
	lease_id, attempt_token_hash, config_revision, config_hash, machine_vars,
	machine_var_sources, machine_limits, namespace_vars_revision, phase, error,
	started_at, finished_at, reaped_at, reclaim_reason, capability_id, artifacts, result_json, result_accepted_at`

// scanTaskAttempt 从一行结果解析 TaskAttempt（三处 JSON 列在这里一次解完）。
func scanTaskAttempt(rows *sql.Rows) (*TaskAttempt, error) {
	var (
		a            TaskAttempt
		rawArtifacts string
		rawResult    string
		rawVars      string
		rawSources   string
		rawLimits    string
		startedAt    sql.NullInt64
		finishedAt   sql.NullInt64
		reapedAt     sql.NullInt64
		err          error
	)
	if err := rows.Scan(&a.TaskID, &a.Attempt, &a.NodeID, &a.NodeName, &a.InstanceID, &a.Slot,
		&a.LeaseID, &a.AttemptTokenHash, &a.ConfigRevision, &a.ConfigHash, &rawVars,
		&rawSources, &rawLimits, &a.NamespaceVarsRevision, &a.Phase, &a.Error,
		&startedAt, &finishedAt, &reapedAt, &a.ReclaimReason, &a.CapabilityID, &rawArtifacts, &rawResult, &a.ResultAcceptedAt); err != nil {
		return nil, fmt.Errorf("scan task attempt: %w", err)
	}
	a.Artifacts = []protocol.TaskArtifact{}
	if err := json.Unmarshal([]byte(rawArtifacts), &a.Artifacts); err != nil {
		return nil, fmt.Errorf("decode attempt artifacts: %w", err)
	}
	if a.Artifacts == nil {
		a.Artifacts = []protocol.TaskArtifact{}
	}
	a.ResultJSON = json.RawMessage(rawResult)
	if rawResult != "" {
		var result TaskResult
		if err := json.Unmarshal(a.ResultJSON, &result); err != nil {
			return nil, fmt.Errorf("decode attempt result: %w", err)
		}
		a.ExitCode = result.ExitCode
	}
	if a.MachineVars, err = decodeStringMap(rawVars); err != nil {
		return nil, fmt.Errorf("decode attempt machine vars: %w", err)
	}
	if a.MachineVarSources, err = decodeStringMap(rawSources); err != nil {
		return nil, fmt.Errorf("decode attempt machine var sources: %w", err)
	}
	if a.MachineLimits, err = decodeAnyMap(rawLimits); err != nil {
		return nil, fmt.Errorf("decode attempt machine limits: %w", err)
	}
	if startedAt.Valid {
		v := startedAt.Int64
		a.StartedAt = &v
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		a.FinishedAt = &v
	}
	if reapedAt.Valid {
		v := reapedAt.Int64
		a.ReapedAt = &v
	}
	return &a, nil
}

// getTaskAttemptOn 在给定句柄上按 (task_id, attempt) 读一行；未命中 ErrNotFound。
func getTaskAttemptOn(ctx context.Context, ex queryExec, taskID string, attempt int) (*TaskAttempt, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT `+taskAttemptColumns+` FROM task_attempts WHERE task_id = ? AND attempt = ?`,
		taskID, attempt)
	if err != nil {
		return nil, fmt.Errorf("get task attempt %s/%d: %w", taskID, attempt, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get task attempt %s/%d: %w", taskID, attempt, err)
		}
		return nil, ErrNotFound
	}
	return scanTaskAttempt(rows)
}

// GetTaskAttempt 读一次执行的事实（未命中 ErrNotFound）。
//
// 用它回答「第 N 次执行到底跑在哪台机器/哪个槽/哪版机器配置」——不靠 tasks 上
// 被重试覆盖过的当前字段反推历史。
func (d *DB) GetTaskAttempt(ctx context.Context, taskID string, attempt int) (*TaskAttempt, error) {
	if taskID == "" || attempt <= 0 {
		return nil, fmt.Errorf("get task attempt: taskID/attempt required")
	}
	return getTaskAttemptOn(ctx, d, taskID, attempt)
}

// ListTaskAttempts 按 attempt 升序列出逻辑任务的全部执行事实；空列表返回 []。
func (d *DB) ListTaskAttempts(ctx context.Context, taskID string) ([]*TaskAttempt, error) {
	if taskID == "" {
		return nil, fmt.Errorf("list task attempts: empty task_id")
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskAttemptColumns+` FROM task_attempts WHERE task_id = ? ORDER BY attempt`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task attempts %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []*TaskAttempt{}
	for rows.Next() {
		a, serr := scanTaskAttempt(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PreviousTaskAttempt 返回「当前 attempt 之前的那一次执行」（G2 的 previous 日志来源）。
//
// 它返回真实的 attempt 行（带那一次的 node/instance/slot），不是拿 task.NodeID 猜。
// 没有更早的 attempt（或任务还没 grant）→ ErrNotFound。
func (d *DB) PreviousTaskAttempt(ctx context.Context, taskID string) (*TaskAttempt, error) {
	if taskID == "" {
		return nil, fmt.Errorf("previous task attempt: empty task_id")
	}
	var current int
	err := d.QueryRowContext(ctx, `SELECT attempt FROM tasks WHERE id = ?`, taskID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("previous task attempt %s: read task: %w", taskID, err)
	}
	if current <= 1 {
		return nil, ErrNotFound
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskAttemptColumns+` FROM task_attempts
		  WHERE task_id = ? AND attempt < ? ORDER BY attempt DESC LIMIT 1`, taskID, current)
	if err != nil {
		return nil, fmt.Errorf("previous task attempt %s: %w", taskID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("previous task attempt %s: %w", taskID, err)
		}
		return nil, ErrNotFound
	}
	return scanTaskAttempt(rows)
}

// machineConfigSnapshot 是 grant 时从 node_configs **同一事务内**读出的机器配置事实。
//
// revision/appliedRevision/appliedHash 回答「配置生效了吗」；vars/limits 是
// local > node 合并后的内容快照（namespace 默认值不在其中：那是任务输入，随任务行钉住）。
type machineConfigSnapshot struct {
	revision        int64
	appliedRevision int64
	appliedHash     string
	vars            map[string]string
	varSources      map[string]string
	limits          map[string]any
	hash            string
}

// needsAppliedFact 报告这台节点是否被要求「已 apply 当前 revision」。
//
// 没有配置行（revision=0 且 applied=0）的节点从未收到过 config.changed：它没有可 apply
// 的东西，grant 不因此被拒（空配置的 hash 会在 attempt 快照里照算）。
func (s *machineConfigSnapshot) needsAppliedFact() bool {
	return s.revision > 0 || s.appliedRevision > 0 || s.appliedHash != ""
}

// appliedMatches 报告节点的 applied 事实是否就是当前配置（revision + 内容 hash 都要对）。
func (s *machineConfigSnapshot) appliedMatches() bool {
	return s.appliedRevision == s.revision && s.appliedHash == s.hash
}

// loadMachineSnapshotOn 在给定句柄上读机器配置并算出合并快照。
//
// 必须在 grant 的**同一个事务**里调用：grant 之后另开一个事务再读，会把「另一个
// 时刻」的机器值写进 attempt 快照（那正是同 attempt 中途漂移的根源）。
func loadMachineSnapshotOn(ctx context.Context, ex queryExec, nodeID string) (*machineConfigSnapshot, error) {
	snap := &machineConfigSnapshot{
		vars:       map[string]string{},
		varSources: map[string]string{},
		limits:     map[string]any{},
	}
	var rawVars, rawLimits, rawLocalVars, rawLocalLimits string
	err := ex.queryRowContext(ctx,
		`SELECT revision, applied_revision, applied_hash, vars, limits, local_vars, local_limits
		   FROM node_configs WHERE node_id = ?`, nodeID).
		Scan(&snap.revision, &snap.appliedRevision, &snap.appliedHash,
			&rawVars, &rawLimits, &rawLocalVars, &rawLocalLimits)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有机器配置：空 vars/limits，hash 按共享实现算出（空 map 归一成 {}）。
		hash, herr := MachineConfigHash(snap.vars, snap.limits)
		if herr != nil {
			return nil, fmt.Errorf("grant: machine config hash: %w", herr)
		}
		snap.hash = hash
		return snap, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}

	nodeVars, err := decodeStringMap(rawVars)
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}
	localVars, err := decodeStringMap(rawLocalVars)
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}
	nodeLimits, err := decodeAnyMap(rawLimits)
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}
	localLimits, err := decodeAnyMap(rawLocalLimits)
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}

	for name, value := range nodeVars {
		snap.vars[name] = value
		snap.varSources[name] = VarSourceNode
	}
	for name, value := range localVars {
		snap.vars[name] = value
		snap.varSources[name] = VarSourceLocal
	}
	snap.limits = mergeLimits(nodeLimits, localLimits)

	hash, err := MachineConfigHash(snap.vars, snap.limits)
	if err != nil {
		return nil, fmt.Errorf("read machine config: %w", err)
	}
	snap.hash = hash
	return snap, nil
}

// insertTaskAttemptOn 在 grant 事务里插一行执行快照。
func insertTaskAttemptOn(ctx context.Context, ex queryExec, a *TaskAttempt) error {
	varsJSON, err := marshalStringMap(a.MachineVars)
	if err != nil {
		return err
	}
	sourcesJSON, err := marshalStringMap(a.MachineVarSources)
	if err != nil {
		return err
	}
	limitsJSON, err := marshalAnyMap(a.MachineLimits)
	if err != nil {
		return err
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO task_attempts
		   (task_id, attempt, node_id, node_name, instance_id, slot, lease_id,
		    attempt_token_hash, config_revision, config_hash, machine_vars,
		    machine_var_sources, machine_limits, namespace_vars_revision, phase,
		    error, started_at, capability_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.TaskID, a.Attempt, a.NodeID, a.NodeName, a.InstanceID, a.Slot, a.LeaseID,
		a.AttemptTokenHash, a.ConfigRevision, a.ConfigHash, varsJSON,
		sourcesJSON, limitsJSON, a.NamespaceVarsRevision, a.Phase, a.Error, a.StartedAt, a.CapabilityID); err != nil {
		return fmt.Errorf("insert task attempt %s/%d: %w", a.TaskID, a.Attempt, err)
	}
	return nil
}

// finishAttemptOn 写 attempt 级诊断（phase/error/finished_at），可选同时落 reaped 事实。
//
// reaped=true 时按该 attempt 自己绑定的 (node, instance, task, attempt) 精确释放槽位：
// 旧 attempt 的回收只清它自己那一个槽，绝不碰同一任务的另一个 attempt 的占用。
func finishAttemptOn(ctx context.Context, ex queryExec, taskID string, attempt int,
	phase, errMsg string, now int64, reason string, reaped bool) (bool, error) {
	row, err := getTaskAttemptOn(ctx, ex, taskID, attempt)
	if errors.Is(err, ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	// Preserve uploaded partial metadata on expiry/cancel as well as legal results.
	manifest := row.Artifacts
	if len(row.ResultJSON) == 0 {
		manifest, err = uploadedManifestOn(ctx, ex, taskID, attempt)
		if err != nil {
			return false, err
		}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return false, err
	}
	q := `UPDATE task_attempts
          SET phase = ?, error = ?, finished_at = CASE WHEN result_json = '' THEN COALESCE(finished_at, ?) ELSE finished_at END, artifacts = ?`
	args := []any{phase, errMsg, now, string(manifestJSON)}
	if reaped {
		q += `, reaped_at = ?, reclaim_reason = ?`
		args = append(args, now, reason)
	}
	q += ` WHERE task_id = ? AND attempt = ?`
	args = append(args, taskID, attempt)
	res, err := ex.execContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("finish task attempt %s/%d: %w", taskID, attempt, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finish task attempt %s/%d rows: %w", taskID, attempt, err)
	}
	if reaped {
		if _, rerr := releaseSlotBindingOn(ctx, ex, row.NodeID, row.InstanceID, taskID, attempt, now); rerr != nil {
			return false, rerr
		}
	}
	return n > 0, nil
}

// reapAttemptOn 是「记录 reaped 事实 + 精确释放该 attempt 的槽」的唯一核心。
//
// nodeID/leaseID/instanceID 非空时逐一核对历史绑定（对不上 = ErrLeaseMismatch）：
// 旧 attempt 的授权可以已过期/已撤销，但「是哪一个绑定」必须证明。
// 已 reaped 的 attempt 是幂等的（返回 false, nil）。
func reapAttemptOn(ctx context.Context, ex queryExec, taskID string, attempt int,
	nodeID, leaseID, instanceID string, now int64, reason string) (bool, error) {
	row, err := getTaskAttemptOn(ctx, ex, taskID, attempt)
	if errors.Is(err, ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if nodeID != "" && row.NodeID != nodeID {
		return false, fmt.Errorf("%w: attempt %s/%d 归节点 %s，不是 %s",
			ErrLeaseMismatch, taskID, attempt, row.NodeID, nodeID)
	}
	if leaseID != "" && row.LeaseID != leaseID {
		return false, fmt.Errorf("%w: attempt %s/%d 的 lease %s != %s",
			ErrLeaseMismatch, taskID, attempt, row.LeaseID, leaseID)
	}
	if instanceID != "" && row.InstanceID != instanceID {
		return false, fmt.Errorf("%w: attempt %s/%d 的实例 %s != %s",
			ErrLeaseMismatch, taskID, attempt, row.InstanceID, instanceID)
	}
	if row.ReapedAt != nil {
		// 重复 ACK 幂等：不重复写事实，也不报错（槽已经在第一次回收里清过）。
		return false, nil
	}
	res, err := ex.execContext(ctx,
		`UPDATE task_attempts
		    SET reaped_at = ?, reclaim_reason = ?, finished_at = CASE WHEN result_json = '' THEN COALESCE(finished_at, ?) ELSE finished_at END
		  WHERE task_id = ? AND attempt = ? AND reaped_at IS NULL`,
		now, reason, now, taskID, attempt)
	if err != nil {
		return false, fmt.Errorf("reap task attempt %s/%d: %w", taskID, attempt, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reap task attempt %s/%d rows: %w", taskID, attempt, err)
	}
	if _, rerr := releaseSlotBindingOn(ctx, ex, row.NodeID, row.InstanceID, taskID, attempt, now); rerr != nil {
		return false, rerr
	}
	return n > 0, nil
}

// ReapedInputs 是「某个 attempt 的进程组已回收」这一事实在 store 内部的形状。
//
// 它不是 wire DTO，也不允许被当成 DTO 镜像：G2 按 T3 冻结的最终帧字段映射进来
// （共享 protocol.ValidateCancelAck 在 G2 强制 leaseId 与 slot presence；
// store 的 int slot 保持内部形状，不镜像 wire 指针）。
//
// 关键语义：旧授权**不需要仍然 active**（lease 可过期、任务可已重排/终态），
// 但必须证明「是哪个 attempt 的哪个绑定」：node 身份（token hash）+ 历史
// attempt 行里的 lease/instance/slot 全都对上才回收。
type ReapedInputs struct {
	TaskID        string
	Attempt       int
	NodeID        string
	NodeTokenHash string
	// LeaseID 与 Slot 是**必须**的历史绑定额定项（A3：node+taskId+attempt+leaseId+slot
	// 四项都得对上）。它们可以属于一个已经过期/已撤销的 attempt，但必须是那一次的真实值。
	LeaseID string
	Slot    int
	// InstanceID 是内部键（可选加固）：给了就一并核对。
	InstanceID string
	Now        int64
	// Reason 是诊断来源（空 → node_report）。
	Reason string
}

// ReapTaskAttempt 是**历史 attempt** 的物理回收入口（旧授权无需 active）。
//
// 它只做两件事：给该 attempt 记 reaped 事实、清它自己占的那个槽；**不碰**任务的
// phase / 当前 attempt / 当前 token / 新 attempt 的槽。重复调用幂等。
//
// 拒绝规则（fail closed，不 no-op 假成功）：
//   - 节点不存在或 token hash 对不上 → ErrNodeIdentityMismatch；
//   - attempt 行不存在，或 node/lease/instance/slot 与历史绑定不一致 → ErrLeaseMismatch。
//
// 旧 attempt 的 result / emit 仍由 CompleteTask / EmitAuthorized 拒（它们核对的是
// **当前** attempt + lease + 未过期租约）；本入口不放行任何业务提交。
func (d *DB) ReapTaskAttempt(ctx context.Context, in ReapedInputs) (bool, error) {
	if in.TaskID == "" || in.Attempt <= 0 {
		return false, fmt.Errorf("reap task attempt: taskID/attempt required")
	}
	if in.NodeID == "" || in.NodeTokenHash == "" {
		return false, fmt.Errorf("%w: 回收事实必须带当前节点身份", ErrNodeIdentityMismatch)
	}
	if in.LeaseID == "" || in.Slot < 0 {
		// 缺旧绑定的关键定额（lease/slot）：不能证明「停止的是哪一次执行的哪个槽」。
		return false, fmt.Errorf("%w: 历史回收必须带该 attempt 的 leaseId 与 slot", ErrLeaseMismatch)
	}
	now := in.Now
	if now == 0 {
		now = Now()
	}
	reason := in.Reason
	if reason == "" {
		reason = ReclaimReasonNodeReport
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("reap task attempt %s/%d: begin: %w", in.TaskID, in.Attempt, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	var tokenHash string
	err = ex.queryRowContext(ctx, `SELECT token_hash FROM nodes WHERE id = ?`, in.NodeID).Scan(&tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: node %s 不存在", ErrNodeIdentityMismatch, in.NodeID)
	}
	if err != nil {
		return false, fmt.Errorf("reap task attempt %s/%d: read node: %w", in.TaskID, in.Attempt, err)
	}
	if tokenHash == "" || tokenHash != in.NodeTokenHash {
		return false, fmt.Errorf("%w: node %s 的令牌与回收者不一致", ErrNodeIdentityMismatch, in.NodeID)
	}

	row, err := getTaskAttemptOn(ctx, ex, in.TaskID, in.Attempt)
	if errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("%w: attempt %s/%d 不存在", ErrLeaseMismatch, in.TaskID, in.Attempt)
	}
	if err != nil {
		return false, err
	}
	if row.NodeID != in.NodeID {
		return false, fmt.Errorf("%w: attempt %s/%d 跑在节点 %s，不是 %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.NodeID, in.NodeID)
	}
	if row.LeaseID != in.LeaseID {
		return false, fmt.Errorf("%w: attempt %s/%d 的 lease %s != %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.LeaseID, in.LeaseID)
	}
	if row.Slot != in.Slot {
		return false, fmt.Errorf("%w: attempt %s/%d 的 slot %d != %d",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.Slot, in.Slot)
	}
	if in.InstanceID != "" && row.InstanceID != in.InstanceID {
		return false, fmt.Errorf("%w: attempt %s/%d 的实例 %s != %s",
			ErrLeaseMismatch, in.TaskID, in.Attempt, row.InstanceID, in.InstanceID)
	}

	reaped, err := reapAttemptOn(ctx, ex, in.TaskID, in.Attempt, in.NodeID, in.LeaseID, in.InstanceID, now, reason)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("reap task attempt %s/%d: commit: %w", in.TaskID, in.Attempt, err)
	}
	return reaped, nil
}
