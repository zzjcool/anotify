package store

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrInvalidTransition 表示状态机拒绝该迁移（契约 §3.2 表外的一切组合）。
var ErrInvalidTransition = errors.New("invalid_transition")

// Task 是 tasks 表的一行：任务行 = OTel span。
//
// Out 三态必须靠 Out + Reported 两列表达（不变量 I5）：
//
//	Reported=false, Out=nil → 没回报（漏报/超时），不派下一跳
//	Reported=true,  Out=nil → 已回报，到头了
//	Reported=true,  Out=&"fix" → 已回报，走 fix 出口
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

// CreateTasks 在单个事务内批量建任务：扇出必须原子（要么全成要么全不成）。
func (d *DB) CreateTasks(ctx context.Context, ts []*Task) error {
	return ErrNotImplemented
}

// GetTask 按 (orgID, id) 取任务；未命中返回 ErrNotFound。
func (d *DB) GetTask(ctx context.Context, orgID, id string) (*Task, error) {
	return nil, ErrNotImplemented
}

// ListTasks 列出任务，过滤条件为空表示不过滤（traceID/nodeID/status）；
// 空列表返回 []。
func (d *DB) ListTasks(ctx context.Context, orgID, traceID, nodeID, status string, limit int) ([]*Task, error) {
	return nil, ErrNotImplemented
}

// Transition 是**唯一**改 tasks.status 的入口（契约 §3）。
//
// 按 §3.2 迁移表校验，表外组合返回 ErrInvalidTransition；
// CAS 式 UPDATE ... WHERE id=? AND status=?，RowsAffected()==0 说明并发被抢或不存在。
func (d *DB) Transition(ctx context.Context, orgID, taskID, from, to string) error {
	return ErrNotImplemented
}

// SweepExpiredLeases 扫过期租约（契约 §3.3）：
// status IN (assigned,acked,running) AND lease_until < now →
// attempts 未用尽回 pending，否则（或能力声明 idempotent:false）abandoned。
// 返回被回收的任务 ID。
func (d *DB) SweepExpiredLeases(ctx context.Context, now int64) (reclaimed []string, err error) {
	return nil, ErrNotImplemented
}

// AssignTask 派发：pending → assigned，写 node/instance/lease_until 并 attempts+1。
func (d *DB) AssignTask(ctx context.Context, orgID, taskID, nodeID, instanceID string, leaseUntil int64) error {
	return ErrNotImplemented
}

// ExtendLease 按心跳把租约推到 leaseUntil（只对 assigned/acked/running 生效）。
func (d *DB) ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error {
	return ErrNotImplemented
}

// ReportResult 记录节点回报的结果并推进到终态。
//
// reported=false 表示帧里缺 out 字段（没回报出口），此时 Out 必须留 NULL；
// reported=true 且 out=nil 表示「明确到头」（不变量 I5）。
func (d *DB) ReportResult(ctx context.Context, orgID, taskID, status string, out *string, reported bool, summary string, artifacts []string, errMsg string) error {
	return ErrNotImplemented
}

// ValidTransition 判定 from → to 是否在契约 §3.2 的迁移表内。
func ValidTransition(from, to string) bool {
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
