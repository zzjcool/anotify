package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 任务相位（capability.md §8.3：小写，与 wire 一致）。
//
// 六个值就是全部：任务只有「在排队 / 在跑 / 终态」三种处境，终态再按原因分四类。
// 没有 v3 的 9 态（assigned/acked/lease_expired 是**派发过程的步骤**，不是相位：
// 它们要么还在 pending（等槽 / 退避 / 等配置），要么已经 running（授了租约）。
const (
	TaskPending   = string(protocol.TaskPhasePending)
	TaskRunning   = string(protocol.TaskPhaseRunning)
	TaskSucceeded = string(protocol.TaskPhaseSucceeded)
	TaskFailed    = string(protocol.TaskPhaseFailed)
	// TaskCancelled 是「用户取消了，且进程组已回收」。
	TaskCancelled = string(protocol.TaskPhaseCancelled)
	// TaskAbandoned 是「不再尝试了」：用户放弃、基础设施原因、尝试次数用尽。
	TaskAbandoned = string(protocol.TaskPhaseAbandoned)
)

// TaskPhases 返回全部相位（封闭集合，供校验与统计复用）。
func TaskPhases() []string {
	return []string{TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned}
}

// IsTaskPhase 报告 phase 是否在封闭集合里。
func IsTaskPhase(phase string) bool {
	for _, p := range TaskPhases() {
		if p == phase {
			return true
		}
	}
	return false
}

// IsTerminalPhase 报告相位是否终态（终态才起 taskTtlSec 计时、才被保留期清理）。
func IsTerminalPhase(phase string) bool {
	return protocol.TaskPhase(phase).Terminal()
}

// 等待原因（capability.md §8.3）。等待态不占槽位、不耗租约、不计 attempt，也不进 failed。
const (
	WaitingForSlot      = string(protocol.WaitingForSlot)
	WaitingRetryBackoff = string(protocol.WaitingRetryBackoff)
	WaitingForConfig    = string(protocol.WaitingForConfig)
)

// 任务相关的哨兵错误。
var (
	// ErrInvalidTransition 表示状态机拒绝该迁移（或该行已不在期望相位）。
	ErrInvalidTransition = errors.New("invalid_transition")
	// ErrLeaseMismatch 表示 attempt / lease_id 与库里的权威事实不一致。
	//
	// 它覆盖三种同源的事故：旧 attempt 令牌、过期的租约、被重派后拿旧租约回报。
	// 一律报错、不"尽力而为"——静默接受会让两次执行都写进同一行。
	ErrLeaseMismatch = errors.New("lease_mismatch")
	// ErrAttemptTokenInvalid 表示尝试令牌对不上（emit / task.result 的权威校验）。
	ErrAttemptTokenInvalid = errors.New("attempt_token_invalid")
	// ErrTaskNotRunning 表示任务已不在 running（emit 的 409 task_not_running）。
	ErrTaskNotRunning = errors.New("task_not_running")
	// ErrMaxAttemptsExceeded 表示 grant 时发现尝试预算已用尽（任务转终态）。
	ErrMaxAttemptsExceeded = errors.New("max_attempts_exceeded")
	// ErrNoAssignment 表示任务没有待 grant 的派发候选（assignment_id 对不上）。
	ErrNoAssignment = errors.New("no_assignment")
)

// MaxTaskParamsBytes 是单条任务 params 的硬上限（120 KiB = 122880 字节）。
//
// 这是**传输层**限制：节点把整份 params 塞进子进程环境变量，而 Linux 对单个
// env/argv 串的硬上限是 MAX_ARG_STRLEN = 131072，超过 execve 直接 E2BIG。
// 留 ~8KiB 余量给前缀与其它 argv 后取 120KiB。
const MaxTaskParamsBytes = 120 << 10

// ErrParamsTooLarge 是 params 超限的哨兵（errors.Is 可用）。
var ErrParamsTooLarge = errors.New("params_too_large")

// ParamsTooLargeError 带「谁的、多大、上限多少」，让入口都能诊断。
type ParamsTooLargeError struct {
	TaskID string
	Size   int
	Limit  int
}

func (e *ParamsTooLargeError) Error() string {
	id := e.TaskID
	if id == "" {
		id = "<new>"
	}
	return fmt.Sprintf("params too large: task %s carries %d bytes, limit is %d bytes "+
		"(nodes receive params through an env var; shrink the payload or pass a file reference)", id, e.Size, e.Limit)
}

// Unwrap 让 errors.Is(err, ErrParamsTooLarge) 成立。
func (e *ParamsTooLargeError) Unwrap() error { return ErrParamsTooLarge }

// CheckTaskParamsSize 校验一条任务的 params 体积（空 = 放过）。
func CheckTaskParamsSize(taskID string, params json.RawMessage) error {
	if len(params) == 0 {
		return nil
	}
	if len(params) > MaxTaskParamsBytes {
		return &ParamsTooLargeError{TaskID: taskID, Size: len(params), Limit: MaxTaskParamsBytes}
	}
	return nil
}

// Task 是 tasks 表的一行：**一个逻辑 task 一行**。
//
// 路由不在这张表上：出口的唯一载体是 emit 记录（`emits`）。任务行只记
// 「执行这件事怎么样了」，以及**执行授权的全部事实**（attempt / lease / token hash）。
type Task struct {
	ID           string          `json:"id"`
	NamespaceID  string          `json:"namespaceId"`
	TraceID      string          `json:"traceId"`
	ParentTaskID *string         `json:"parentTaskId"`
	CapabilityID string          `json:"capabilityId"`
	Capability   string          `json:"capability"`
	Version      int             `json:"version"`
	Digest       string          `json:"digest"`
	Params       json.RawMessage `json:"params"`
	Phase        string          `json:"phase"`
	NodeID       *string         `json:"nodeId"`
	NodeName     string          `json:"nodeName"`
	// ConfigRevision 是派发时节点配置的生效版本（诊断用）。
	ConfigRevision int64 `json:"configRevision"`
	// Attempt 是**已授予的租约数**（只在 grant 生效时 +1）。
	Attempt int `json:"attempt"`
	// MaxAttempts 是总尝试预算（派发时从 spec.lifecycle 钉住，默认 3）。
	MaxAttempts int `json:"maxAttempts"`
	// AssignmentID / ProposedAttempt 是「这次候选派给谁」的事实：assign 只是记候选，
	// 不授执行权；grant 必须带着同一个 assignment_id 来换租约。
	AssignmentID    string `json:"assignmentId"`
	ProposedAttempt int    `json:"proposedAttempt"`
	// Idempotent 是派发时钉住的幂等声明（重试判定只看它，不回读能力）。
	Idempotent bool `json:"idempotent"`
	// NamespaceVars 是**任务被接受时**钉住的命名空间默认输入快照（D2）。
	//
	// 它是内部执行输入：不进任何公共 DTO（`json:"-"`），避免 ns env 值随任务列表/
	// 详情泄漏；需要值的地方只通过 TaskInputs 按名字点名取。
	NamespaceVars map[string]string `json:"-"`
	// NamespaceVarsRevision 是上面那份快照对应的 namespaceVarsRevision（D2 的两个
	// 配置版本之一）：诊断可以回显**版本号**，值本身不回显。
	NamespaceVarsRevision int64 `json:"namespaceVarsRevision"`
	// LeaseID / LeaseUntil 是执行授权；AttemptTokenHash 只存 sha256，明文只在 grant 时返回一次。
	LeaseID    string `json:"leaseId"`
	LeaseUntil *int64 `json:"leaseUntil"`
	// LeaseInstanceID 是这次 attempt 绑定的物理槽位（node_instances.id，内部事实）。
	// 释放槽位必须按它精确释放：同一逻辑任务换 attempt 重试时可能同时占着旧 attempt
	// 的槽（旧进程未回收），按 task_id 一把清会把别人的槽也放了。
	LeaseInstanceID  string `json:"-"`
	AttemptTokenHash string `json:"-"`
	// WaitingReason 只有 pending 有意义；NextAttemptAt 是退避后的最早可派时刻（nil = 立刻）。
	WaitingReason string `json:"waitingReason"`
	NextAttemptAt *int64 `json:"nextAttemptAt"`
	// CancelReason / CancelRequestedAt 记录取消意图（running 时要等 cancel.ack 回收进程组）。
	CancelReason      string   `json:"cancelReason"`
	CancelRequestedAt *int64   `json:"cancelRequestedAt"`
	Reported          bool     `json:"reported"`
	Summary           string   `json:"summary"`
	Artifacts         []string `json:"artifacts"`
	Error             string   `json:"error"`
	CreatedAt         int64    `json:"createdAt"`
	StartedAt         *int64   `json:"startedAt"`
	FinishedAt        *int64   `json:"finishedAt"`
}

// taskColumns 是统一的列顺序，scanTask 依赖它。
const taskColumns = `id, namespace_id, trace_id, parent_task_id, capability_id, capability,
	version, digest, params, phase, node_id, node_name, config_revision,
	attempt, max_attempts, assignment_id, proposed_attempt, idempotent,
	lease_id, lease_until, lease_instance_id, attempt_token_hash, waiting_reason, next_attempt_at,
	cancel_reason, cancel_requested_at, reported, summary, artifacts, error,
	created_at, started_at, finished_at, namespace_vars, namespace_vars_revision`

// scanTask 从一行结果解析 Task（三处 JSON 列在这里一次解完）。
func scanTask(rows *sql.Rows) (*Task, error) {
	var (
		t          Task
		params     string
		artifacts  string
		nsVars     string
		idempotent int
		reported   int
	)
	if err := rows.Scan(
		&t.ID, &t.NamespaceID, &t.TraceID, &t.ParentTaskID, &t.CapabilityID, &t.Capability,
		&t.Version, &t.Digest, &params, &t.Phase, &t.NodeID, &t.NodeName, &t.ConfigRevision,
		&t.Attempt, &t.MaxAttempts, &t.AssignmentID, &t.ProposedAttempt, &idempotent,
		&t.LeaseID, &t.LeaseUntil, &t.LeaseInstanceID, &t.AttemptTokenHash, &t.WaitingReason, &t.NextAttemptAt,
		&t.CancelReason, &t.CancelRequestedAt, &reported, &t.Summary, &artifacts, &t.Error,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &nsVars, &t.NamespaceVarsRevision,
	); err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Idempotent = idempotent != 0
	t.Reported = reported != 0
	t.Params = json.RawMessage(params)
	if len(t.Params) == 0 {
		t.Params = json.RawMessage("{}")
	}
	t.NamespaceVars = map[string]string{}
	if trimmed := strings.TrimSpace(nsVars); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &t.NamespaceVars); err != nil {
			return nil, fmt.Errorf("decode task namespace vars: %w", err)
		}
		if t.NamespaceVars == nil {
			t.NamespaceVars = map[string]string{}
		}
	}
	t.Artifacts = []string{}
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
// 建出来的行一律是 pending、attempt=0、没有任何租约事实。
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

// createTasksTx 是 CreateTasks 的事务内版本（emit/终态写入与建行共用一个 tx）。
//
// 校验顺序：nil → 必填字段 → 父任务命名空间继承（I11④）→ 命名空间必填 →
// 「只能落 pending」→ params 体积。**同一批里一条不合格就一条都不建**。
func (d *DB) createTasksTx(ctx context.Context, tx *sql.Tx, ts []*Task) error {
	if len(ts) == 0 {
		return nil
	}
	// 同一次扇出里同 ns 的任务共一份快照（同一事务里读，不可能两样）。
	nsCache := map[string]namespaceInputs{}
	for i, t := range ts {
		if t == nil {
			return fmt.Errorf("create tasks: nil task at index %d", i)
		}
		if t.TraceID == "" || t.CapabilityID == "" || t.Capability == "" {
			return fmt.Errorf("create tasks: task %d missing trace/capability", i)
		}
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
		if t.Phase == "" {
			t.Phase = TaskPending
		}
		if t.Phase != TaskPending {
			// 建任务只能落在 pending：其它相位必须经 grant / 终态入口推进。
			return fmt.Errorf("create tasks: task %d must start as pending, got %q", i, t.Phase)
		}
		if t.MaxAttempts <= 0 {
			t.MaxAttempts = artifact.DefaultMaxAttempts
		}
		if t.CreatedAt == 0 {
			t.CreatedAt = Now()
		}
		if t.Params == nil {
			t.Params = json.RawMessage("{}")
		}
		if err := CheckTaskParamsSize(t.ID, t.Params); err != nil {
			return fmt.Errorf("create tasks: task %d: %w", i, err)
		}
		// D2：**接受即为任务输入钉版**。ns 默认值属于任务输入，不是层叠配置：
		// 它必须留下可复现载体，否则排队期间改一次 ns 配置，已接受的任务就换了一
		// 套输入（重试前/后不一致）。机器层（local > node）不在这里钉：它按
		// attempt 的节点重新解析（TaskInputs）。
		pin, ok := nsCache[t.NamespaceID]
		if !ok {
			loaded, perr := loadNamespaceInputsOn(ctx, txExec{tx: tx}, t.NamespaceID)
			if perr != nil {
				return fmt.Errorf("create tasks: task %d: %w", i, perr)
			}
			pin = loaded
			nsCache[t.NamespaceID] = pin
		}
		t.NamespaceVars = pin.vars
		t.NamespaceVarsRevision = pin.revision
		nsVarsJSON, err := marshalStringMap(pin.vars)
		if err != nil {
			return fmt.Errorf("create tasks: task %d: %w", i, err)
		}
		if t.Artifacts == nil {
			t.Artifacts = []string{}
		}
		artifactsJSON, err := json.Marshal(t.Artifacts)
		if err != nil {
			return fmt.Errorf("marshal task artifacts: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks
			   (id, namespace_id, trace_id, parent_task_id, capability_id, capability,
			    version, digest, params, phase, max_attempts, idempotent, created_at, artifacts,
			    namespace_vars, namespace_vars_revision)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.NamespaceID, t.TraceID, t.ParentTaskID, t.CapabilityID, t.Capability,
			t.Version, t.Digest, string(t.Params), t.Phase, t.MaxAttempts, boolToInt(t.Idempotent),
			t.CreatedAt, string(artifactsJSON), nsVarsJSON, pin.revision,
		); err != nil {
			return fmt.Errorf("insert task %d (%s): %w", i, t.Capability, err)
		}
	}
	return nil
}

// namespaceInputs 是任务接受时钉住的第三层输入（ns 默认值）。
//
// revision 就是 config.md 的 namespaceVarsRevision：它只说明「这组 ns 默认是哪一版」，
// 与任务行上的 configRevision（机器 applied 版本）是两件事（C18：不能压成一个）。
type namespaceInputs struct {
	vars     map[string]string
	revision int64
}

// loadNamespaceInputsOn 在同一事务/句柄上读某个 ns 的默认值与版本。
// 没有配置行 = 空默认 + revision 0（与 GetNamespaceConfig 的「空默认」同口径）。
func loadNamespaceInputsOn(ctx context.Context, ex queryExec, namespaceID string) (namespaceInputs, error) {
	out := namespaceInputs{vars: map[string]string{}}
	var raw string
	err := ex.queryRowContext(ctx,
		`SELECT vars, revision FROM namespace_configs WHERE namespace_id = ?`, namespaceID).
		Scan(&raw, &out.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read namespace inputs: %w", err)
	}
	vars, derr := decodeStringMap(raw)
	if derr != nil {
		return out, fmt.Errorf("read namespace inputs: %w", derr)
	}
	out.vars = vars
	return out, nil
}

// TaskInputs 解析一条任务在一次执行里**实际可用**的输入值（名字 → 值 + 来源层）。
//
// 两层判据（D2）：
//   - namespace 层只读任务接受时钉住的快照（NamespaceVars）与它的 revision：
//     后续 namespace 配置更新不会改动已接受任务的输入，重试/换节点也一样；
//   - local / node 是机器级值，按任务**当前节点**（本次 attempt 的 node_id）重新解析：
//     换节点重试拿到那台机器自己的值，而不是上一台的。
//
// 只返回 names 点名的键（不把整份 ns 快照灌进 run.env、也不隐式给全部键）；
// 三层都没有的名字不出现在结果里（缺失 = 该 env 键不出现，不是空串）。
//
// G2 应在 grant 时调一次，并把结果用于该 attempt（同一 attempt 期间不重算）。
func (d *DB) TaskInputs(ctx context.Context, taskID string, names []string) (map[string]EffectiveVar, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task inputs: empty task_id")
	}
	t, err := d.getTaskByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return d.taskInputsOn(ctx, d, t, names)
}

// taskInputsOn 是 TaskInputs 的句柄版（内部/测试复用）。
func (d *DB) taskInputsOn(ctx context.Context, ex queryExec, t *Task, names []string) (map[string]EffectiveVar, error) {
	out := make(map[string]EffectiveVar, len(names))
	if len(names) == 0 {
		return out, nil
	}
	local, node := map[string]string{}, map[string]string{}
	if t.NodeID != nil && *t.NodeID != "" {
		var rawLocal, rawNode string
		err := ex.queryRowContext(ctx,
			`SELECT local_vars, vars FROM node_configs WHERE node_id = ?`, *t.NodeID).
			Scan(&rawLocal, &rawNode)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("task inputs: read node config: %w", err)
		}
		if err == nil {
			if local, err = decodeStringMap(rawLocal); err != nil {
				return nil, fmt.Errorf("task inputs: node local vars: %w", err)
			}
			if node, err = decodeStringMap(rawNode); err != nil {
				return nil, fmt.Errorf("task inputs: node vars: %w", err)
			}
		}
	}
	for _, raw := range names {
		name, ok := varRefName(raw)
		if !ok {
			continue
		}
		if v, hit := local[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceLocal}
			continue
		}
		if v, hit := node[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceNode}
			continue
		}
		if v, hit := t.NamespaceVars[name]; hit {
			out[name] = EffectiveVar{Value: v, Source: VarSourceNamespace}
		}
	}
	return out, nil
}

// inheritParentNamespaceTx 实现 I11④：子任务的 namespace_id 必须与父任务一致。
//
// 两档：没给（空）→ 继承；给了但与父不同 → *CrossNamespaceError（不静默修正）。
// 父任务行不存在时既不报错也不改（那种引用指向的对象不存在，不是「另一个命名空间」）。
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
		return &CrossNamespaceError{What: "tasks.parent_task_id", Want: parentNS, Got: t.NamespaceID}
	}
	return nil
}

// GetTask 按 (namespaceID, id) 取任务；未命中返回 ErrNotFound。
func (d *DB) GetTask(ctx context.Context, namespaceID, id string) (*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
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

// TaskFilter 是任务列表的过滤条件（零值 = 不过滤）。
//
// Limit 的语义与 api-conventions §7.6 一致：<=0 用默认 50，>500 夹到 500。
type TaskFilter struct {
	Phase      string
	TraceID    string
	NodeID     string
	Capability string
	Limit      int
	Offset     int
}

// ListTasks 列出命名空间内的任务（最新在前）；空列表返回 []。
func (d *DB) ListTasks(ctx context.Context, namespaceID string, f TaskFilter) ([]*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE namespace_id = ?`
	args := []any{namespaceID}
	if f.Phase != "" {
		q += ` AND phase = ?`
		args = append(args, f.Phase)
	}
	if f.TraceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, f.TraceID)
	}
	if f.NodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, f.NodeID)
	}
	if f.Capability != "" {
		q += ` AND capability = ?`
		args = append(args, f.Capability)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, f.Offset)

	rows, err := d.cachedQueryContext(ctx, q, args...)
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

// ListTasksByTrace 按 trace 列出全部任务（**建行顺序**，供 trace 视图按时间线展示）。
func (d *DB) ListTasksByTrace(ctx context.Context, namespaceID, traceID string) ([]*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if traceID == "" {
		return nil, fmt.Errorf("list tasks by trace: empty trace_id")
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		  WHERE namespace_id = ? AND trace_id = ?
		  ORDER BY created_at, id`, namespaceID, traceID)
	if err != nil {
		return nil, fmt.Errorf("list tasks by trace: %w", err)
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

// CountTasksByTrace 数某 trace 的任务行（trace 列表的「多少步」）。
func (d *DB) CountTasksByTrace(ctx context.Context, namespaceID, traceID string) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE namespace_id = ? AND trace_id = ?`,
		namespaceID, traceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tasks by trace: %w", err)
	}
	return n, nil
}

// ListDispatchableTasks 取出「此刻可以派发」的 pending 任务（跨命名空间，调度器用）。
//
// 判据：phase=pending 且 (next_attempt_at IS NULL OR next_attempt_at <= now)，
// 且**没有取消在飞**（cancel_requested_at IS NULL）：已请求取消的任务永不再进派发候选。
// 退避中的与等待配置的任务不会被捞出来——它们的 next_attempt_at 在未来。
func (d *DB) ListDispatchableTasks(ctx context.Context, now int64, limit int) ([]*Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		  WHERE phase = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		    AND cancel_requested_at IS NULL
		  ORDER BY created_at, id
		  LIMIT ?`, TaskPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list dispatchable tasks: %w", err)
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

// AssignTask 记录**派发候选**：这次想派给哪个节点。
//
// 它**不授执行权**：phase 仍是 pending、没有 lease、没有 attempt token。
// 真正授权发生在 GrantLease（节点 accept 之后）——这样「派了但节点没接」
// 不会消耗尝试预算，也不会让一条 pending 任务凭空显示成 running。
//
// assignmentID 是这次候选的身份（中枢生成，随 task.assign 帧下发）；节点 grant 时
// 必须带回同一个值，否则视为旧派发（ErrNoAssignment）。
//
// 两条硬条件写在**同一条 UPDATE** 里（不先读后写）：
//   - cancel_requested_at IS NULL：已请求取消的任务永不再被选点；
//   - 目标节点必须仍 online（EXISTS 子查询）：draining/offline 的节点不接新任务。
//
// 前面的节点状态读只用于给出准确错误，真正的判定在 UPDATE 的 WHERE 里（原子）。
func (d *DB) AssignTask(ctx context.Context, taskID, nodeID, assignmentID string, proposedAttempt int) error {
	if taskID == "" || nodeID == "" || assignmentID == "" {
		return fmt.Errorf("assign task: taskID/nodeID/assignmentID required")
	}
	spec, err := d.taskLifecycle(ctx, taskID)
	if err != nil {
		return fmt.Errorf("assign task %s: %w", taskID, err)
	}
	// 槽位已在此前抢过；节点不存在/非 online → 不该选它（EligibleNodes 的硬条件）。
	var nodeStatus string
	nerr := d.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id = ?`, nodeID).Scan(&nodeStatus)
	if errors.Is(nerr, sql.ErrNoRows) {
		return fmt.Errorf("%w: node %s", ErrNotFound, nodeID)
	}
	if nerr != nil {
		return fmt.Errorf("assign task %s: read node: %w", taskID, nerr)
	}
	if nodeStatus != NodeStatusOnline {
		return fmt.Errorf("%w: node %s is %s（不再接新任务）", ErrInvalidTransition, nodeID, nodeStatus)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET assignment_id = ?, node_id = ?, proposed_attempt = ?,
		        idempotent = ?, max_attempts = ?
		  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL
		    AND EXISTS (SELECT 1 FROM nodes WHERE id = ? AND status = ?)`,
		assignmentID, nodeID, proposedAttempt,
		boolToInt(spec.Idempotent), spec.EffectiveMaxAttempts(),
		taskID, TaskPending, nodeID, NodeStatusOnline)
	if err != nil {
		return fmt.Errorf("assign task %s: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("assign task %s rows: %w", taskID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: task %s is not dispatchable（pending 且未取消才可派）", ErrInvalidTransition, taskID)
	}
	return nil
}

// taskLifecycle 读能力 spec 的 lifecycle（派发时钉住 idempotent / maxAttempts 用）。
//
// 能力行已被删掉时返回默认值（幂等 false、总尝试 3）：那条任务注定会被孤儿视图看见，
// 不该因为它连 assign 都做不了——「钉住的默认值 + 找不到能力」比「整个派发失败」更好排查。
func (d *DB) taskLifecycle(ctx context.Context, taskID string) (artifact.Lifecycle, error) {
	var specJSON string
	err := d.QueryRowContext(ctx,
		`SELECT cv.spec FROM tasks t JOIN capability_versions cv ON cv.id = t.capability_id
		  WHERE t.id = ?`, taskID).Scan(&specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		// 两种可能：任务不存在，或能力行已删。再查一次任务本身以区分。
		var n int
		if qerr := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, taskID).Scan(&n); qerr != nil {
			return artifact.Lifecycle{}, fmt.Errorf("probe task: %w", qerr)
		}
		if n == 0 {
			return artifact.Lifecycle{}, ErrNotFound
		}
		return artifact.Lifecycle{}, nil
	}
	if err != nil {
		return artifact.Lifecycle{}, fmt.Errorf("read capability lifecycle: %w", err)
	}
	var spec artifact.Spec
	if trimmed := strings.TrimSpace(specJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &spec); err != nil {
			return artifact.Lifecycle{}, fmt.Errorf("decode capability spec: %w", err)
		}
	}
	return spec.Lifecycle, nil
}

// GrantLease 授予一次执行租约（**每次执行唯一授权入口**）。
//
// 事务内完成五件事，缺一样都会留下可观测的坏状态：
//  1. 校验 assignment / 节点 / 相位（旧派发的 grant 必须被拒）；
//  2. attempt + 1（**唯一**的 +1 点），超预算直接转终态；
//  3. 生成 leaseId + 随机 attempt token（库里只存 sha256，明文只返回一次）；
//  4. phase=running / started_at / lease_until=now+60s / 钉住 idempotent；
//  5. 占槽（node_instances.busy_task）。
//
// 槽位先由 node.go 的 LeaseSlot 抢（它是自己的 CAS 事务），再在事务里写授权事实；
// 授权失败时把槽位还回去——不能因为「节点上没这个槽了」把一条 pending 任务改成 running。
func (d *DB) GrantLease(ctx context.Context, taskID, assignmentID, nodeID string, now int64) (leaseID, attemptToken string, attempt int, expiresAt int64, err error) {
	if taskID == "" || nodeID == "" {
		return "", "", 0, 0, fmt.Errorf("grant lease: taskID/nodeID required")
	}
	if now == 0 {
		now = Now()
	}
	// 先读一次做快速判定（真正的权威校验在事务里用 CAS 重做一遍）。
	cur, err := d.getTaskByID(ctx, taskID)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, err)
	}
	if cur.Phase != TaskPending {
		return "", "", 0, 0, fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, cur.Phase)
	}
	if assignmentID != "" && cur.AssignmentID != "" && cur.AssignmentID != assignmentID {
		return "", "", 0, 0, fmt.Errorf("%w: task %s assignment %s != %s",
			ErrNoAssignment, taskID, cur.AssignmentID, assignmentID)
	}

	// 槽位：抢不到就是 waiting_for_slot（不消耗 attempt、不改相位）。
	instanceID, err := d.LeaseSlot(ctx, nodeID, taskID)
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			d.markWaitingForSlot(ctx, taskID)
		}
		return "", "", 0, 0, err
	}
	// 授权失败要把槽位还回去（grant 是唯一一次「先占后写」）。
	committed := false
	defer func() {
		if !committed {
			_ = d.ReleaseSlot(ctx, nodeID, instanceID)
		}
	}()

	leaseID = newID("lse")
	attemptToken = NewTaskToken()
	tokenHash := hashAttemptToken(attemptToken)
	expiresAt = now + int64(protocol.LeaseTTL.Seconds())

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// 事务内重读（并发下上面的读可能已过期），并把 lifecycle 钉在同一事务里。
	var (
		phase        string
		rowAssign    string
		rowNode      sql.NullString
		rowAttempt   int
		maxAttempts  int
		capabilityID string
		cancelReq    sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, assignment_id, node_id, attempt, max_attempts, capability_id,
		        cancel_requested_at
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &rowAssign, &rowNode, &rowAttempt, &maxAttempts, &capabilityID, &cancelReq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, 0, ErrNotFound
	}
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: read: %w", taskID, err)
	}
	if phase != TaskPending {
		return "", "", 0, 0, fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if cancelReq.Valid {
		// 取消已经发出：永不再授新执行权（即使它又变回了 pending）。
		return "", "", 0, 0, fmt.Errorf("%w: task %s 已请求取消，不再授权", ErrInvalidTransition, taskID)
	}
	if assignmentID != "" && rowAssign != "" && rowAssign != assignmentID {
		return "", "", 0, 0, fmt.Errorf("%w: task %s assignment %s != %s",
			ErrNoAssignment, taskID, rowAssign, assignmentID)
	}
	if rowNode.Valid && rowNode.String != "" && rowNode.String != nodeID {
		return "", "", 0, 0, fmt.Errorf("%w: task %s is assigned to %s, not %s",
			ErrNoAssignment, taskID, rowNode.String, nodeID)
	}

	lc, lerr := lifecycleOn(ctx, txExec{tx: tx}, capabilityID)
	if lerr != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: %w", taskID, lerr)
	}
	nextAttempt := rowAttempt + 1
	maxAttempts = lc.EffectiveMaxAttempts()

	if nextAttempt > maxAttempts {
		// 预算用尽：不再授租约，直接终态。错误码是调度层的 max_attempts_exceeded，
		// 所以相位取 abandoned（不是能力自己的失败）。
		//
		// 已经抢到的槽位必须在**同一事务里**还回去：这条路径不产生 running 行，
		// 没人会再为它走 CompleteTask/AckCancel，漏在这里就是一个永久 busy_task。
		if _, err := tx.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = NULL, updated_at = ?
			  WHERE node_id = ? AND id = ? AND busy_task = ?`,
			now, nodeID, instanceID, taskID); err != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: release slot: %w", taskID, err)
		}
		if ok, ferr := d.finishTxGuarded(ctx, tx, taskID, TaskPending, TaskAbandoned, "", nil, now); ferr != nil {
			return "", "", 0, 0, ferr
		} else if !ok {
			return "", "", 0, 0, fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE id = ?`, protocol.ReasonMaxAttemptsExceeded, taskID); err != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: write reason: %w", taskID, err)
		}
		if err := tx.Commit(); err != nil {
			return "", "", 0, 0, fmt.Errorf("grant lease %s: commit terminal: %w", taskID, err)
		}
		committed = true
		return "", "", 0, 0, fmt.Errorf("%w: task %s used %d/%d attempts",
			ErrMaxAttemptsExceeded, taskID, rowAttempt, maxAttempts)
	}

	var (
		nodeName string
		nodeStat string
	)
	if qerr := tx.QueryRowContext(ctx, `SELECT name, status FROM nodes WHERE id = ?`, nodeID).
		Scan(&nodeName, &nodeStat); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return "", "", 0, 0, fmt.Errorf("%w: node %s", ErrNotFound, nodeID)
		}
		return "", "", 0, 0, fmt.Errorf("grant lease %s: read node: %w", taskID, qerr)
	}
	if nodeStat != NodeStatusOnline {
		// draining/offline 的节点不接新租约（drain 只停新派发，在跑的续租不受影响）。
		return "", "", 0, 0, fmt.Errorf("%w: node %s is %s（不再接新任务）",
			ErrInvalidTransition, nodeID, nodeStat)
	}
	// config_revision 是「这次执行是在哪一版机器配置下开始的」——诊断用，缺行按 0。
	var configRevision int64
	if qerr := tx.QueryRowContext(ctx,
		`SELECT revision FROM node_configs WHERE node_id = ?`, nodeID).Scan(&configRevision); qerr != nil &&
		!errors.Is(qerr, sql.ErrNoRows) {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: read node config revision: %w", taskID, qerr)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks
		    SET phase = ?, attempt = ?, assignment_id = '', proposed_attempt = 0,
		        idempotent = ?, max_attempts = ?, node_id = ?, node_name = ?, config_revision = ?,
		        lease_id = ?, lease_until = ?, lease_instance_id = ?, attempt_token_hash = ?,
		        waiting_reason = '', next_attempt_at = NULL,
		        started_at = COALESCE(started_at, ?)
		  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
		TaskRunning, nextAttempt, boolToInt(lc.Idempotent), maxAttempts, nodeID, nodeName, configRevision,
		leaseID, expiresAt, instanceID, tokenHash, now, taskID, TaskPending)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: write: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", "", 0, 0, fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = ?, updated_at = ?
		  WHERE node_id = ? AND id = ? AND busy_task = ?`, taskID, now, nodeID, instanceID, taskID); err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: bind slot: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", 0, 0, fmt.Errorf("grant lease %s: commit: %w", taskID, err)
	}
	committed = true
	return leaseID, attemptToken, nextAttempt, expiresAt, nil
}

// markWaitingForSlot 记一条「为什么还在排队」（best-effort，失败不影响主流程）。
func (d *DB) markWaitingForSlot(ctx context.Context, taskID string) {
	_, _ = d.ExecContext(ctx,
		`UPDATE tasks SET waiting_reason = ? WHERE id = ? AND phase = ?`,
		WaitingForSlot, taskID, TaskPending)
}

// RenewLease 续租：校验 attempt + lease_id + 相位 + **没有取消在飞**，把租约推到 now+60s。
//
// 返回新的到期时刻（= 下一份 ACK 的 deadline）。任何一项对不上都是
// ErrLeaseMismatch：节点该用**本次** grant 拿到的 leaseId 续，旧租约没有续的权利。
//
// **取消一发起就立刻停止续租**（`cancel_requested_at IS NULL` 是硬条件）：
// 授权一旦被撤回，继续续期等于让一个「已经被要求停下」的执行体保持合法身份——
// 它随后还能 emit、还能覆盖终态，取消就形同虚设。
func (d *DB) RenewLease(ctx context.Context, taskID string, attempt int, leaseID string, now int64) (int64, error) {
	if taskID == "" || leaseID == "" {
		return 0, fmt.Errorf("renew lease: taskID/leaseID required")
	}
	if now == 0 {
		now = Now()
	}
	expiresAt := now + int64(protocol.LeaseTTL.Seconds())
	res, err := d.ExecContext(ctx,
		`UPDATE tasks SET lease_until = ?
		  WHERE id = ? AND phase = ? AND attempt = ? AND lease_id = ?
		    AND cancel_requested_at IS NULL`,
		expiresAt, taskID, TaskRunning, attempt, leaseID)
	if err != nil {
		return 0, fmt.Errorf("renew lease %s: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("%w: task %s attempt %d lease %s", ErrLeaseMismatch, taskID, attempt, leaseID)
	}
	return expiresAt, nil
}

// RejectAssignment 处理节点的 `task.reject`：**只有 stale_config 是合法理由**。
//
// 语义（capability.md §2.3）：不计 attempt、不占槽、不改 node 归属，回 pending 并
// 置 waiting_for_config + next_attempt_at=退避（5s 起、上限 60s，D-I11）。
// 这条路径的存在意义是「机器级配置还没 apply 完」——它不是执行失败，
// 所以绝不能消耗尝试预算（否则一次配置延迟就能把任务打成 abandoned）。
func (d *DB) RejectAssignment(ctx context.Context, taskID, assignmentID, reason string, now int64) error {
	if !protocol.IsTaskRejectReason(reason) {
		return fmt.Errorf("%w: task.reject 只允许 stale_config，得到 %q", ErrInvalidTransition, reason)
	}
	if now == 0 {
		now = Now()
	}
	var (
		phase     string
		rowAssign string
		attempt   int
	)
	err := d.QueryRowContext(ctx,
		`SELECT phase, assignment_id, attempt FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &rowAssign, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reject assignment %s: %w", taskID, err)
	}
	if phase != TaskPending {
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if assignmentID != "" && rowAssign != "" && rowAssign != assignmentID {
		return fmt.Errorf("%w: task %s assignment %s != %s", ErrNoAssignment, taskID, rowAssign, assignmentID)
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE tasks
		    SET assignment_id = '', proposed_attempt = 0, node_id = NULL, node_name = '',
		        waiting_reason = ?, next_attempt_at = ?
		  WHERE id = ? AND phase = ?`,
		WaitingForConfig, now+StaleConfigBackoff(attempt), taskID, TaskPending); err != nil {
		return fmt.Errorf("reject assignment %s: %w", taskID, err)
	}
	return nil
}

// TaskResult 是一次执行的结果（task.result 帧的领域形态）。
//
// Reason 是封闭集合里的失败/结束原因码（protocol.Reason*）；成功时可为空。
// 进程组是否回收**不在这里**：那是 cancel.ack 的 processGroupReaped 字段
// （契约 D-I8：取消等 reap 才释放槽位），结果的到达本身就意味着这次执行结束了。
type TaskResult struct {
	Phase     string
	Reason    string
	Summary   string
	Artifacts []string
	Error     string
}

// CompleteTask 写入一次执行结果（中枢权威校验 attempt + lease_id + 未过期）。
//
// **取消在飞时一律拒**（ErrTaskNotRunning）：那条执行的结局由 cancel.ack 裁定，
// 谁能改写终态必须只有一个答案。授权已经在 RequestCancel 那一刻收回
// （见 VerifyAttemptToken / RenewLease），result 路径不该是绕过它的后门。
//
// 正常结束（没有取消在飞）→ 落终态并释放槽位：result 帧本身就意味着进程已退出。
// 槽位在「取消等 reap」这条路上只由 AckCancel(reaped=true) 释放（契约 D-I8）。
func (d *DB) CompleteTask(ctx context.Context, taskID string, attempt int, leaseID string, result TaskResult) error {
	return d.CompleteTaskAt(ctx, taskID, attempt, leaseID, Now(), result)
}

// CompleteTaskAt 是 CompleteTask 的显式时钟版（G2 可用帧/连接时刻传入；测试可注入）。
//
// 五条授权事实全部在同**一**个事务里校验（BEGIN IMMEDIATE 下读—写是原子的）：
// 当前 attempt、当前 lease_id、phase=running、未取消、**租约未过期**。
// 过期租约的 result 必须拒：那一代执行已经失去授权（巡检随时会重派），
// 接受它会与下一次尝试争着写同一行终态。
func (d *DB) CompleteTaskAt(ctx context.Context, taskID string, attempt int, leaseID string, now int64, result TaskResult) error {
	if !IsTerminalPhase(result.Phase) || result.Phase == TaskCancelled {
		return fmt.Errorf("%w: result phase must be succeeded|failed|abandoned, got %q",
			ErrInvalidTransition, result.Phase)
	}
	if now == 0 {
		now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete task %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		phase           string
		rowAttempt      int
		rowLease        string
		nodeID          sql.NullString
		cancelReq       sql.NullInt64
		rowCancelReason sql.NullString
		leaseUntil      sql.NullInt64
		instanceID      string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, attempt, lease_id, node_id, cancel_requested_at, cancel_reason, lease_until,
		        lease_instance_id
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &rowAttempt, &rowLease, &nodeID, &cancelReq, &rowCancelReason, &leaseUntil, &instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("complete task %s: read: %w", taskID, err)
	}
	if IsTerminalPhase(phase) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, phase)
	}
	if phase != TaskRunning {
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if rowAttempt != attempt || rowLease != leaseID {
		return fmt.Errorf("%w: task %s attempt/lease mismatch (have %d/%s, want %d/%s)",
			ErrLeaseMismatch, taskID, rowAttempt, rowLease, attempt, leaseID)
	}
	if cancelReq.Valid {
		return fmt.Errorf("%w: task %s 已请求取消（%s），结果由 cancel.ack 裁定",
			ErrTaskNotRunning, taskID, rowCancelReason.String)
	}
	if !leaseUntil.Valid || leaseUntil.Int64 <= now {
		return fmt.Errorf("%w: task %s 的租约已过期（结果不再被接受）", ErrLeaseMismatch, taskID)
	}

	ok, err := d.finishTxGuarded(ctx, tx, taskID, TaskRunning, result.Phase, result.Summary, result.Artifacts, now)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET reported = 1, error = ? WHERE id = ?`, normalizeTaskError(result), taskID); err != nil {
		return fmt.Errorf("complete task %s: write result: %w", taskID, err)
	}

	if err := releaseLeaseSlotTx(ctx, tx, nodeID.String, instanceID, taskID, now); err != nil {
		return fmt.Errorf("complete task %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete task %s: commit: %w", taskID, err)
	}
	return nil
}

// normalizeTaskError 决定 error 列写什么：失败/放弃必须有原因（不变量：失败一定有说明）。
func normalizeTaskError(result TaskResult) string {
	msg := result.Error
	if msg == "" {
		msg = result.Reason
	}
	if msg == "" && (result.Phase == TaskFailed || result.Phase == TaskAbandoned) {
		msg = result.Summary
	}
	if msg == "" && (result.Phase == TaskFailed || result.Phase == TaskAbandoned) {
		msg = result.Phase
	}
	return msg
}

// RequestCancel 记录一次取消意图（取消与 grant 的**唯一线性化点**）。
//
//   - pending：没有执行者，直接终态（cancelled / abandoned），不需要等 ack；
//   - running：只落 cancel_reason + cancel_requested_at，等节点 cancel.ack 带回
//     processGroupReaped=true 才置终态并释放槽位（进程可能还在跑）。
//
// 读与写必须在**同一个事务**里（DSN `_txlock=immediate`，写锁在事务起点拿下）：
// 先读后写的两步之间如果被 grant 插进来，就会把一条**正在跑**的任务无条件改成已取消，
// 而节点上的进程还在用着那个槽位。现在两种赢法都是对的：
//   - 取消先赢（pending → 终态）：随后的 grant 看到终态/取消标记，直接拒；
//   - grant 先赢（pending → running）：取消走 running 分支，只记意图，等 reap。
//
// 重复取消是幂等的（返回 nil，保留第一次的原因）：网关重试/两个管理员同时点
// 不该变成 409。已终态 → ErrInvalidTransition（调用方翻 409）。
func (d *DB) RequestCancel(ctx context.Context, taskID, reason string, now int64) error {
	if reason == "" {
		reason = protocol.ReasonCancelledByUser
	}
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("request cancel %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		phase     string
		cancelReq sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, cancel_requested_at FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &cancelReq)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("request cancel %s: read: %w", taskID, err)
	}
	if IsTerminalPhase(phase) {
		return fmt.Errorf("%w: task %s already %s", ErrInvalidTransition, taskID, phase)
	}

	switch phase {
	case TaskPending:
		// 条件 UPDATE：取消与 grant 的竞争在这里分出胜负（不先读后写）。
		res, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET phase = ?, finished_at = ?, summary = '', artifacts = '[]',
			        lease_until = NULL, lease_id = '', waiting_reason = '', next_attempt_at = NULL,
			        assignment_id = '', proposed_attempt = 0,
			        cancel_reason = ?, cancel_requested_at = ?, error = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			cancelPhase(reason), now, reason, now, reason, taskID, TaskPending)
		if uerr != nil {
			return fmt.Errorf("request cancel %s: write: %w", taskID, uerr)
		}
		if n, rerr := res.RowsAffected(); rerr != nil {
			return fmt.Errorf("request cancel %s: rows: %w", taskID, rerr)
		} else if n == 0 {
			return fmt.Errorf("%w: task %s 已不在可取消状态", ErrInvalidTransition, taskID)
		}
	case TaskRunning:
		if cancelReq.Valid {
			return nil // 幂等：第一次的意图与原因不被覆盖
		}
		res, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET cancel_reason = ?, cancel_requested_at = ?
			  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL`,
			reason, now, taskID, TaskRunning)
		if uerr != nil {
			return fmt.Errorf("request cancel %s: write: %w", taskID, uerr)
		}
		if n, rerr := res.RowsAffected(); rerr != nil {
			return fmt.Errorf("request cancel %s: rows: %w", taskID, rerr)
		} else if n == 0 {
			return fmt.Errorf("%w: task %s 已不在可取消状态", ErrInvalidTransition, taskID)
		}
	default:
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("request cancel %s: commit: %w", taskID, err)
	}
	return nil
}

// cancelPhase 把取消原因翻成终态相位（用户取消 → cancelled，其余 → abandoned）。
func cancelPhase(reason string) string {
	if reason == protocol.ReasonCancelledByUser {
		return TaskCancelled
	}
	return TaskAbandoned
}

// AckCancel 处理节点的 `cancel.ack`。
//
// **只有 processGroupReaped=true 才置终态、才释放槽位**：reaped=false 表示节点收到了
// 取消但进程组还没死透，此时把任务标成终态会让同一槽位跑两条任务。
// attempt 必须等于库里的当前 attempt（旧 attempt 的 ack 属于上一代执行，拒掉）。
//
// 已终态的取消仍可被一次 reaped=true 的 ack **幂等地补上槽位回收**：租约巡检可能
// 已经把一条已取消的任务先收成终局（它不释放槽位），节点随后才带着「进程组已回收」
// 的事实回来——那个事实才是释放物理槽位的依据，不能让它的 acks 白跑。
//
// 槽位只按**这个 attempt 绑定的那个实例**释放（tasks.lease_instance_id）：同一逻辑
// 任务换 attempt 重试时可能同时占着旧 attempt 的槽（旧进程还没回收），按 task_id
// 一把清会把上一个 attempt 的槽放给下一个任务。attempt 对不上的 ack 一律不释放任何槽。
func (d *DB) AckCancel(ctx context.Context, taskID string, attempt int, processGroupReaped bool, now int64) error {
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ack cancel %s: begin: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		phase        string
		rowAttempt   int
		nodeID       sql.NullString
		cancelReason sql.NullString
		cancelReq    sql.NullInt64
		instanceID   string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, attempt, node_id, cancel_reason, cancel_requested_at, lease_instance_id
		   FROM tasks WHERE id = ?`, taskID).
		Scan(&phase, &rowAttempt, &nodeID, &cancelReason, &cancelReq, &instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("ack cancel %s: read: %w", taskID, err)
	}
	// **先核 attempt**：旧 attempt 的 reap 不能碰新 attempt 的槽（即使 busy_task 同名）。
	if rowAttempt != attempt {
		return fmt.Errorf("%w: task %s attempt %d != %d", ErrLeaseMismatch, taskID, rowAttempt, attempt)
	}
	if IsTerminalPhase(phase) {
		if !cancelReq.Valid {
			// 不是取消造成终态（succeeded/failed 后的迟到 ack）：不接。
			return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
		}
		if processGroupReaped {
			if err := releaseLeaseSlotTx(ctx, tx, nodeID.String, instanceID, taskID, now); err != nil {
				return fmt.Errorf("ack cancel %s: %w", taskID, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ack cancel %s: commit: %w", taskID, err)
		}
		return nil
	}
	if phase == TaskPending {
		// 任务已被租约巡检重排（旧 attempt 作废），旧 attempt 自己的进程组回收事实
		// 仍然有效：它只释放**那个 attempt 绑定的槽**，不改相位、不动新 attempt。
		if processGroupReaped {
			if err := releaseLeaseSlotTx(ctx, tx, nodeID.String, instanceID, taskID, now); err != nil {
				return fmt.Errorf("ack cancel %s: %w", taskID, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ack cancel %s: commit: %w", taskID, err)
		}
		return nil
	}
	if phase != TaskRunning {
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if !processGroupReaped {
		// 收到了，但还没回收完：不改相位、不释放槽位，**也不清租约**。
		//
		// 不清租约是刻意的：清掉之后 RecoverLostLeases 就再也捞不到这条任务
		// （租约没到期 = 还"活着"），节点万一在 TERM→KILL 之间掉线，任务会永久卡在
		// running 且槽位永久钉死。留着租约，最坏情况只是等一次过期回收。
		return nil
	}

	reason := cancelReason.String
	if reason == "" {
		reason = protocol.ReasonAbandonedByUser
	}
	ok, err := d.finishTxGuarded(ctx, tx, taskID, TaskRunning, cancelPhase(reason), "", nil, now)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: task %s changed phase concurrently", ErrInvalidTransition, taskID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET error = ? WHERE id = ?`, reason, taskID); err != nil {
		return fmt.Errorf("ack cancel %s: write reason: %w", taskID, err)
	}
	if err := releaseLeaseSlotTx(ctx, tx, nodeID.String, instanceID, taskID, now); err != nil {
		return fmt.Errorf("ack cancel %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ack cancel %s: commit: %w", taskID, err)
	}
	return nil
}

// RecoverLostLeases 回收**真正失联**的 running 任务（调度巡检的唯一入口）。
//
// 两类候选人：租约到点的 running 任务，以及**所在节点已 offline** 的 running 任务。
// `draining` **不是**失联：drain 的语义是「不再接新任务、把存量跑完」，在跑的
// 任务照常续租/回报结果（只有 offline 才是「这台机器不在了」）。
//
// 决策分三条，判据来自任务行上钉住的 idempotent（不回读能力，避免「能力改过一次就换了语义」）：
//
//	已请求取消                    → 终局（cancelPhase(原因)），**绝不 retry**
//	幂等 且 attempt < maxAttempts → 回 pending + retry_backoff + 退避（会重跑）
//	否则                          → abandoned（infra 原因，绝不重跑非幂等能力）
//
// **物理槽位不在常规路径释放**：只有节点已 offline/不存在（会话死亡事实）时才清
// busy_task；仍 online 的节点上的旧进程可能还在跑，槽位由节点后续事实释放
// （AckCancel 的 reaped=true，或节点重新上报时 ReclaimStaleSlots）。
// 取消终局一律不释放槽位：reaped 未 ACK 前那个槽位必须保持占用。
//
// 返回 (重排条数, 直接终态条数)。
func (d *DB) RecoverLostLeases(ctx context.Context, now int64) (int, int, error) {
	if now == 0 {
		now = Now()
	}
	// 只把 offline（或节点行已不在）当失联：draining 仍算「在跑」。
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		  WHERE phase = ? AND (
		        (lease_until IS NOT NULL AND lease_until <= ?)
		     OR (node_id IS NOT NULL AND node_id <> '' AND NOT EXISTS (
		            SELECT 1 FROM nodes n WHERE n.id = tasks.node_id AND n.status IN (?,?))))`,
		TaskRunning, now, NodeStatusOnline, NodeStatusDraining)
	if err != nil {
		return 0, 0, fmt.Errorf("recover lost leases: %w", err)
	}
	lost := []*Task{}
	for rows.Next() {
		t, serr := scanTask(rows)
		if serr != nil {
			rows.Close()
			return 0, 0, serr
		}
		lost = append(lost, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("recover lost leases: iterate: %w", err)
	}
	rows.Close()

	requeued, abandoned := 0, 0
	for _, t := range lost {
		rq, err := d.recoverOne(ctx, t, now)
		if err != nil {
			return requeued, abandoned, err
		}
		if rq {
			requeued++
		} else {
			abandoned++
		}
	}
	return requeued, abandoned, nil
}

// recoverOne 处理一条过期租约（一个事务）。
//
// CAS 双守卫 `phase='running' AND attempt=?`：判断与写入之间任务可能刚被续租/
// 被取消/被重派，那种情况下这一行一行都不该动。
//
// 槽位释放只由**会话死亡事实**（节点 offline / 节点行已不在）触发；取消终局一律不清。
func (d *DB) recoverOne(ctx context.Context, t *Task, now int64) (requeued bool, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("recover lease %s: begin: %w", t.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		phase        string
		attempt      int
		cancelReq    sql.NullInt64
		cancelReason sql.NullString
		instanceID   string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT phase, attempt, cancel_requested_at, cancel_reason, lease_instance_id FROM tasks WHERE id = ?`, t.ID).
		Scan(&phase, &attempt, &cancelReq, &cancelReason, &instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recover lease %s: read: %w", t.ID, err)
	}
	if phase != TaskRunning || attempt != t.Attempt {
		return false, nil // 并发下已经被别人处理过
	}

	// 「真失联」= 节点行不在了或已 offline；draining / online 都不算。
	nodeLost := true
	if t.NodeID != nil && *t.NodeID != "" {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id = ?`, *t.NodeID).Scan(&status)
		switch {
		case err == nil:
			nodeLost = status == NodeStatusOffline
		case errors.Is(err, sql.ErrNoRows):
			nodeLost = true
		default:
			return false, fmt.Errorf("recover lease %s: read node: %w", t.ID, err)
		}
	}

	switch {
	case cancelReq.Valid:
		// 已请求取消：授权回收 = 直接终局，**绝不重排/重试**。
		// 原因码保留用户的取消原因（不是 lease_expired）：用户看到的是「我取消了」。
		reason := cancelReason.String
		if reason == "" {
			reason = protocol.ReasonAbandonedByUser
		}
		ok, ferr := d.finishTxGuarded(ctx, tx, t.ID, TaskRunning, cancelPhase(reason), "", nil, now)
		if ferr != nil {
			return false, ferr
		}
		if !ok {
			return false, nil
		}
		if _, werr := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE id = ?`, reason, t.ID); werr != nil {
			return false, fmt.Errorf("recover lease %s: write reason: %w", t.ID, werr)
		}
		// 槽位不在这里释放（落到下面统一的 nodeLost 分支）：
		// 节点仍 online 时进程组 reap 还没被确认，物理槽必须保持占用；
		// 节点真 offline（会话死亡事实）才允许回收。
	case t.Idempotent && t.Attempt < t.MaxAttempts:
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET phase = ?, waiting_reason = ?, next_attempt_at = ?,
			        node_id = NULL, node_name = '', lease_id = '', lease_until = NULL,
			        attempt_token_hash = '', assignment_id = '', proposed_attempt = 0
			  WHERE id = ? AND phase = ? AND attempt = ? AND cancel_requested_at IS NULL`,
			TaskPending, WaitingRetryBackoff, now+RetryBackoff(t.Attempt),
			t.ID, TaskRunning, t.Attempt); uerr != nil {
			return false, fmt.Errorf("recover lease %s: requeue: %w", t.ID, uerr)
		}
		requeued = true
	default:
		reason := protocol.ReasonLeaseExpired
		if t.Idempotent && t.Attempt >= t.MaxAttempts {
			reason = protocol.ReasonMaxAttemptsExceeded
		}
		ok, ferr := d.finishTxGuarded(ctx, tx, t.ID, TaskRunning, TaskAbandoned, "", nil, now)
		if ferr != nil {
			return false, ferr
		}
		if !ok {
			return false, nil
		}
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE tasks SET error = ? WHERE id = ?`, reason, t.ID); uerr != nil {
			return false, fmt.Errorf("recover lease %s: write reason: %w", t.ID, uerr)
		}
	}
	if nodeLost {
		// 会话死亡事实（节点 offline/行已不在）才允许回收物理槽位；按这次 attempt
		// 绑定的实例精确释放（不碰同名 task 的另一个 attempt 的槽）。
		var nodeID string
		if t.NodeID != nil {
			nodeID = *t.NodeID
		}
		if uerr := releaseLeaseSlotTx(ctx, tx, nodeID, instanceID, t.ID, now); uerr != nil {
			return false, fmt.Errorf("recover lease %s: %w", t.ID, uerr)
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return false, fmt.Errorf("recover lease %s: commit: %w", t.ID, cerr)
	}
	return requeued, nil
}

// VerifyAttemptToken 用明文令牌换任务（emit / task.result 的权威校验）。
//
// 判据四条，缺一不可：库里 sha256 与传入一致、相位是 running、**没有取消在飞**、
// **租约尚未过期**。旧 attempt 的令牌必然对不上：每次 grant 都会换一个随机令牌并
// 覆盖哈希，于是「上一代执行的进程在重派后继续 emit」会被稳定拒掉。
//
// 过期租约返回 ErrLeaseMismatch（与 emit 接受事务里的同一条判据同口径）：
// 这个函数被声明为 task.result 的权威校验，不能声称「令牌对就算授权」却漏掉到期。
// 真正不可绕过的版本仍是 EmitAuthorized / CompleteTaskAt：它们在同一个事务里
// 重做全部判定，本函数只是快速失败与诊断。
//
// 取消一发起就**立刻收回令牌**（返回 ErrTaskNotRunning → 网关 409 task_not_running）：
// 被要求停下的执行体不该还能 emit 出新任务、也不该还能写终态——它的结局由
// cancel.ack（processGroupReaped）裁定。
func (d *DB) VerifyAttemptToken(ctx context.Context, taskID, token string) (*Task, error) {
	if taskID == "" || token == "" {
		return nil, fmt.Errorf("%w: taskID/token required", ErrAttemptTokenInvalid)
	}
	t, err := d.getTaskByID(ctx, taskID)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: task %s not found", ErrAttemptTokenInvalid, taskID)
	}
	if err != nil {
		return nil, err
	}
	if t.AttemptTokenHash == "" || hashAttemptToken(token) != t.AttemptTokenHash {
		return nil, fmt.Errorf("%w: token does not match task %s", ErrAttemptTokenInvalid, taskID)
	}
	if t.Phase != TaskRunning {
		return nil, fmt.Errorf("%w: task %s is %s (reason %s)",
			ErrTaskNotRunning, taskID, t.Phase, protocol.ReasonTaskNotRunning)
	}
	if t.CancelRequestedAt != nil {
		return nil, fmt.Errorf("%w: task %s 已请求取消（%s），执行授权已收回",
			ErrTaskNotRunning, taskID, t.CancelReason)
	}
	if t.LeaseUntil == nil || *t.LeaseUntil <= Now() {
		return nil, fmt.Errorf("%w: task %s 的租约已过期", ErrLeaseMismatch, taskID)
	}
	return t, nil
}

// 为什么终态**保留** attempt_token_hash：它是「最后一次发出的令牌」的记录，
// 让 VerifyAttemptToken 能把「令牌是错的」（401 token_invalid）与「令牌对、但任务
// 已经结束」（409 task_not_running）分开——这对节点侧排障是决定性的区别。
// 下一次 grant 会覆盖它，租约回收（回 pending）会清掉它（那一代执行已作废）。

// hashAttemptToken 是尝试令牌的唯一摘要口径（sha256 hex）。
func hashAttemptToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// releaseLeaseSlotTx 按「node + 这次 attempt 绑定的槽位」精确释放物理槽位。
//
// 为什么不能按 task_id 释放：同一逻辑任务换 attempt 重试时会同时占着旧 attempt 的槽
// （旧进程还没回收），按 task_id 一把清会把旧进程还在用的槽位放给下一个任务
// ——同一槽位跑两条任务，比泄漏严重得多。instanceID 为空 = 这次 attempt 没绑过槽
// （例如授权失败回滚），什么都不做。
func releaseLeaseSlotTx(ctx context.Context, tx *sql.Tx, nodeID, instanceID, taskID string, now int64) error {
	if instanceID == "" {
		return nil // 这次 attempt 没绑过槽（授权失败回滚 / 空行）
	}
	// 实例 id 是全局主键，nodeID 只是额外的防御性谓词：重排会把 task.node_id 清空，
	// 旧 attempt 的确认 reap 仍要能按实例释放它自己那个槽。
	q := `UPDATE node_instances SET busy_task = NULL, updated_at = ?
	       WHERE id = ? AND busy_task = ?`
	args := []any{now, instanceID, taskID}
	if nodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("release lease slot %s/%s: %w", nodeID, instanceID, err)
	}
	return nil
}

// finishTxGuarded 是「进入终态」的唯一 SQL 点：相位 + finished_at + summary/artifacts，
// 清掉执行授权事实（终态行不该再持有租约证据），并以 fromPhase 为 CAS 守卫
// （fromPhase 空 = 不守）。
//
// 传 tx 而不是直接用连接池：所有调用点都已经在事务里，走池既看不到本事务的写，
// 又平白多占一条连接（SQLite 单写，会直接把自己锁死）。
//
// 返回 false 表示该行已不在 fromPhase（并发下被别人推进过），调用方必须放弃本次写。
func (d *DB) finishTxGuarded(ctx context.Context, tx *sql.Tx, taskID, fromPhase, phase, summary string,
	artifacts []string, now int64) (bool, error) {
	if !IsTerminalPhase(phase) {
		return false, fmt.Errorf("%w: finish requires terminal phase, got %q", ErrInvalidTransition, phase)
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	artifactsJSON, err := json.Marshal(artifacts)
	if err != nil {
		return false, fmt.Errorf("finish task %s: marshal artifacts: %w", taskID, err)
	}
	q := `UPDATE tasks SET phase = ?, finished_at = ?, summary = ?, artifacts = ?,
		        lease_until = NULL, lease_id = '', waiting_reason = '', next_attempt_at = NULL
		  WHERE id = ?`
	args := []any{phase, now, summary, string(artifactsJSON), taskID}
	if fromPhase != "" {
		q += ` AND phase = ?`
		args = append(args, fromPhase)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("finish task %s: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finish task %s: rows: %w", taskID, err)
	}
	// reason 的落点是 error 列（由调用方按语义写）：相位已经表达了终态类别，
	// 具体原因（script_failed / cancelled_by_user / …）必须留在 error 里才可判别。
	return n > 0, nil
}

// getTaskByID 是不带命名空间的读（内部路径用：taskID 全局唯一）。
func (d *DB) getTaskByID(ctx context.Context, taskID string) (*Task, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get task %s: %w", taskID, err)
		}
		return nil, ErrNotFound
	}
	return scanTask(rows)
}

// lifecycleOn 在事务内读能力 spec 的 lifecycle（grant 时钉住幂等/预算用）。
func lifecycleOn(ctx context.Context, ex queryExec, capabilityID string) (artifact.Lifecycle, error) {
	var specJSON string
	err := ex.queryRowContext(ctx, `SELECT spec FROM capability_versions WHERE id = ?`, capabilityID).Scan(&specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return artifact.Lifecycle{}, nil
	}
	if err != nil {
		return artifact.Lifecycle{}, fmt.Errorf("read capability lifecycle: %w", err)
	}
	var spec artifact.Spec
	if trimmed := strings.TrimSpace(specJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &spec); err != nil {
			return artifact.Lifecycle{}, fmt.Errorf("decode capability spec: %w", err)
		}
	}
	return spec.Lifecycle, nil
}

// RetryBackoff 返回第 attempt 次失败之后的退避秒数（D-I11：5s 起、×2、上限 5m）。
//
// attempt 是**已授予的**尝试次数：attempt=1 表示第一次刚失败 → 退避 5s。
func RetryBackoff(attempt int) int64 {
	return backoffSeconds(attempt, protocol.RetryBackoffBase.Seconds(),
		protocol.RetryBackoffMax.Seconds(), protocol.RetryBackoffFactor)
}

// StaleConfigBackoff 返回 stale_config 回 pending 的退避秒数（5s 起、上限 60s）。
func StaleConfigBackoff(attempt int) int64 {
	return backoffSeconds(attempt, protocol.StaleConfigBackoffBase.Seconds(),
		protocol.StaleConfigBackoffMax.Seconds(), protocol.RetryBackoffFactor)
}

// backoffSeconds 是两档退避的共用形状：base × factor^(n-1)，封顶 max。
func backoffSeconds(n int, base, max float64, factor int) int64 {
	if n < 1 {
		n = 1
	}
	sec := base
	for i := 1; i < n; i++ {
		sec *= float64(factor)
		if sec >= max {
			return int64(max)
		}
	}
	if sec > max {
		return int64(max)
	}
	return int64(sec)
}

// ---------- 聚合读（背压 / 统计用） ----------

// CountTasksByPhase 返回命名空间内各相位的任务数（缺失的相位不出现在结果里）。
func (d *DB) CountTasksByPhase(ctx context.Context, namespaceID, traceID string) (map[string]int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	q := `SELECT phase, COUNT(*) FROM tasks WHERE namespace_id = ?`
	args := []any{namespaceID}
	if traceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, traceID)
	}
	q += ` GROUP BY phase`
	rows, err := d.cachedQueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("count tasks by phase: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var phase string
		var n int
		if err := rows.Scan(&phase, &n); err != nil {
			return nil, fmt.Errorf("scan phase count: %w", err)
		}
		out[phase] = n
	}
	return out, rows.Err()
}

// CountOpenTasks 数「还没结束」的任务（pending + running），按命名空间。
func (d *DB) CountOpenTasks(ctx context.Context, namespaceID string) (int, error) {
	return d.CountOpenTasksSince(ctx, namespaceID, 0)
}

// CountOpenTasksSince 数 since 之后创建的未结束任务（背压判定）。
func (d *DB) CountOpenTasksSince(ctx context.Context, namespaceID string, since int64) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	var n int
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks
		  WHERE namespace_id = ? AND phase IN (?,?) AND created_at >= ?`,
		namespaceID, TaskPending, TaskRunning, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open tasks: %w", err)
	}
	return n, nil
}

// OldestPendingCreatedAt 返回最早一条 pending 的创建时刻（0 = 没有 pending）。
func (d *DB) OldestPendingCreatedAt(ctx context.Context) (int64, error) {
	var oldest sql.NullInt64
	if err := d.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM tasks WHERE phase = ?`, TaskPending).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("oldest pending: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	return oldest.Int64, nil
}
