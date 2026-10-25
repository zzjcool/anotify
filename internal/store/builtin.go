package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 内置能力清单（契约设计见 design/wiki/08-scope.html）。
//
// 内置能力与自定义能力走同一套表、同一套版本化，唯一区别是 `source` 列。
// 但有一类内置能力**没有节点执行者**：`notify.phone` 这类终端能力是中枢
// 自己的能力——没人会写一个"发推送节点"。所以派发时它们跳过「找节点」，
// 由中枢执行器执行（见 route.Dispatcher 与 internal/executor）。
//
// 为什么放在 store 而不是 route：这是**能力定义数据**，和 params_schema
// 一样属于"什么是一个合法能力"，不是路由算法。
type BuiltinCapability struct {
	Name         string
	Version      int
	ParamsSchema string // JSON
	Outputs      []string
	// Central 表示由中枢执行（不找节点）。终端能力为 true。
	Central bool
}

// NotifyPhoneCapability 是 `notify.phone@1` 的定义。
//
// params 就是通知内容本身——**没有模板替换、没有表达式求值**（不变量 I6）：
// 上游事件带什么就推什么。`to` 走 foreign_user 校验（契约 §10.1.1）。
var NotifyPhoneCapability = BuiltinCapability{
	Name:    "notify.phone",
	Version: 1,
	ParamsSchema: `{
		"type": "object",
		"required": ["to", "title"],
		"properties": {
			"to": {"type": "string"},
			"title": {"type": "string"},
			"body": {"type": "string"},
			"link": {"type": "string"},
			"agentState": {"type": "string"},
			"deviceTags": {"type": "array", "items": {"type": "string"}}
		}
	}`,
	Outputs: []string{},
	Central: true,
}

// BuiltinCapabilities 是随项目发布的全部内置能力。
//
// 2026-08 现状：只有 notify.phone 是中枢执行的。github/cron/watch/http
// 是**事件来源**，不是任务能力——它们由 trigger 层处理，不走节点。
// review/fix/test 等需要节点执行，用户按需自己定义。
var BuiltinCapabilities = []BuiltinCapability{
	NotifyPhoneCapability,
}

// EnsureBuiltinCapabilities 为一个组织补齐所有内置能力（幂等）。
//
// 幂等性靠 capability_versions 的 UNIQUE(org_id, name, version)：
// 已存在就跳过，不会因为重复调用而开新版本。
//
// 为什么不放进注册流程而是单独调用：内置能力可能随版本升级新增
// （比如以后加 notify.webhook），那时已有组织也需要补上——
// 启动时对全部组织跑一遍比"只在注册时播种"更可靠。
func (d *DB) EnsureBuiltinCapabilities(ctx context.Context, orgID string) error {
	if orgID == "" {
		return fmt.Errorf("ensure builtin capabilities: empty org_id")
	}
	for _, bc := range BuiltinCapabilities {
		if err := d.ensureOneBuiltin(ctx, orgID, bc); err != nil {
			return err
		}
	}
	return nil
}

// ensureOneBuiltin 播种一个内置能力（若该 org 还没有这个 name@version）。
func (d *DB) ensureOneBuiltin(ctx context.Context, orgID string, bc BuiltinCapability) error {
	// 已存在就跳过（不改内容：capability_versions 是 append-only，不变量 I8）
	if _, err := d.GetCapability(ctx, orgID, bc.Name, bc.Version); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("probe builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}

	outputs := bc.Outputs
	if outputs == nil {
		outputs = []string{} // 不变量 I9：空列表是 [] 不是 null
	}

	// 直接交给 CreateCapability：它内部算 content_hash、生成 ID、
	// 并把「同内容幂等」与「同名版本换内容」区分开。
	// 这里不重复实现 hash——两份实现必然漂移。
	c := &CapabilityVersion{
		OrgID:        orgID,
		Name:         bc.Name,
		Version:      bc.Version,
		ParamsSchema: json.RawMessage(bc.ParamsSchema),
		Outputs:      outputs,
		Lifecycle:    Lifecycle{},
		Source:       CapSourceBuiltin,
	}
	if err := d.CreateCapability(ctx, c); err != nil {
		return fmt.Errorf("seed builtin capability %s@%d: %w", bc.Name, bc.Version, err)
	}
	return nil
}

// IsBuiltinCapability 判断某个 name@version 是不是中枢执行的内置能力。
//
// 派发器用它决定「是否跳过找节点」：内置终端能力没有节点执行者，
// 查 node_capabilities 必然是 0 行，若照常走第 ③ 步就会 pruned/no_node——
// 推送能力永远发不出去。
func IsBuiltinCapability(name string, version int) (BuiltinCapability, bool) {
	for _, bc := range BuiltinCapabilities {
		if bc.Name == name && bc.Version == version {
			return bc, true
		}
	}
	return BuiltinCapability{}, false
}

// CentralCapabilityNames 返回中枢执行的能力名（派发器与执行器共用）。
func (d *DB) CentralCapabilityNames() []string {
	var out []string
	for _, bc := range BuiltinCapabilities {
		if bc.Central {
			out = append(out, bc.Name)
		}
	}
	return out
}

// ClaimCentralTasks 领取中枢执行的 pending 任务，并推进到 running。
//
// 为什么跳过 assigned/acked：那两个状态的含义是「已派给节点，等它接单/开始」。
// 中枢执行没有节点参与，走那两个状态会让状态机语义失真（也会让租约巡检
// 去扫它们——而中枢任务没有租约，扫不到就会一直挂着）。
//
// 并发安全：用 transitionGuarded 的 CAS（status=pending 作为守卫），
// 多实例部署时只有一个能领到同一条任务。
func (d *DB) ClaimCentralTasks(ctx context.Context, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	names := d.CentralCapabilityNames()
	if len(names) == 0 {
		return []*Task{}, nil
	}

	// 先查候选（pending + 中枢能力名）
	placeholders := make([]string, len(names))
	args := make([]any, 0, len(names)+limit)
	for i, n := range names {
		placeholders[i] = "?"
		args = append(args, n)
	}
	q := `SELECT ` + taskColumns + ` FROM tasks
	      WHERE status = ? AND node_id IS NULL AND capability IN (` +
		strings.Join(placeholders, ",") + `)
	      ORDER BY created_at, id LIMIT ?`
	queryArgs := append([]any{TaskPending}, args...)
	queryArgs = append(queryArgs, limit)

	rows, err := d.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query central tasks: %w", err)
	}
	candidates := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate central tasks: %w", err)
	}
	rows.Close()

	// 逐条 CAS 推进到 running（pending → running 不在合法迁移表里，
	// 所以走 pending → assigned → running：assigned 在这里只表示"被中枢认领"）。
	claimed := make([]*Task, 0, len(candidates))
	for _, t := range candidates {
		if err := d.transitionGuarded(ctx, nil, t.OrgID, t.ID, TaskPending, TaskAssigned, ""); err != nil {
			if errors.Is(err, ErrTransitionGuardFailed) || errors.Is(err, ErrInvalidTransition) {
				continue // 别人先领了
			}
			return claimed, fmt.Errorf("claim central task %s: %w", t.ID, err)
		}
		if err := d.transitionGuarded(ctx, nil, t.OrgID, t.ID, TaskAssigned, TaskRunning, ""); err != nil {
			return claimed, fmt.Errorf("start central task %s: %w", t.ID, err)
		}
		if _, err := d.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE org_id = ? AND id = ?`,
			Now(), t.OrgID, t.ID); err != nil {
			return claimed, fmt.Errorf("stamp central task start: %w", err)
		}
		t.Status = TaskRunning
		claimed = append(claimed, t)
	}
	return claimed, nil
}

// FinishTask 把 running 的中枢任务推进到终态并写回报。
//
// 与 ReportResult 的区别：ReportResult 是**节点**回报路径（要求任务有
// node_id 归属、会校验租约）。中枢任务没有归属，所以走这条。
func (d *DB) FinishTask(ctx context.Context, orgID, taskID, status string, out *string, reported bool, summary, errMsg string) error {
	if status != TaskSucceeded && status != TaskFailed {
		return fmt.Errorf("%w: finish status must be succeeded|failed, got %q", ErrInvalidTransition, status)
	}
	if out != nil && *out == "" {
		return fmt.Errorf("finish task: empty out string is not a valid port (use nil)")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// running → succeeded|failed（走唯一写入口，守卫 status=running）
	if err := d.transitionGuarded(ctx, tx, orgID, taskID, TaskRunning, status, ""); err != nil {
		return fmt.Errorf("finish central task %s: %w", taskID, err)
	}

	rep := 0
	if reported {
		rep = 1
	}
	var outVal any
	if out != nil {
		outVal = *out
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET out = ?, reported = ?, summary = ?, error = ? WHERE org_id = ? AND id = ?`,
		outVal, rep, summary, errMsg, orgID, taskID); err != nil {
		return fmt.Errorf("write central task result: %w", err)
	}
	return tx.Commit()
}
