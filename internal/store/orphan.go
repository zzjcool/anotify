package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 孤儿 pending 任务（/v1/orphans 的语义）。
//
// 「孤儿」是**派不出去**的 pending 任务，两种病因要分开处置：
//
//	no_capability  任务钉住的能力行已经不存在（能力/版本被删）——只能放弃或重放
//	no_online_node 能力还在，但没有任何在线节点能跑它（节点全下线 / 机器级配置未 apply）
//
// 为什么不自动 abandon：孤儿任务是**事故证据**（「这条事件本该被处理，却没被处理」），
// 自动删掉会把现场一起清掉。要不要自动化、阈值多少是运营策略，不是数据层的事。
// 本文件只提供「看得见」（ListOrphanPendingTasks）与「处置得了」（AbandonPendingTask）。

// 孤儿原因分类。
const (
	// OrphanNoCapability 表示任务钉住的能力行已不存在。
	OrphanNoCapability = "no_capability"
	// OrphanNoOnlineNode 表示能力还在，但没有在线节点能跑它。
	OrphanNoOnlineNode = "no_online_node"
)

// OrphanPendingTask 是一条派不出去的 pending 任务 + 它的空闲时长与原因。
type OrphanPendingTask struct {
	Task
	// IdleSec 是「创建至今」的秒数。用创建时间而不是 attempt，因为孤儿任务通常
	// attempt=0（连第一次派发都没发生过）。
	IdleSec int64  `json:"idleSec"`
	Reason  string `json:"reason"`
}

// ListOrphanPendingTasks 列出命名空间内「pending 且当前没有任何在线节点能跑」的任务。
//
// 判据分两类（见文件头），候选统一要求 `phase='pending'`：
//
//  1. no_capability：`tasks.capability_id` 在 capability_versions 里查不到（被删/被换版）；
//  2. no_online_node：能力行在，但**没有一台 online 节点满足**「ns 授权 + 部署了该 digest
//     + 机器级配置已 apply」。这与派发路径用的是**同一组**判据（EligibleNodes），
//     差别只是这里按行判定、不重复解析 spec。
//
// olderThanSec 是安全阀：刚建出来的任务在派发瞬间也可能短暂「没有在线节点」，
// 把它当孤儿会误报。<=0 按 1 小时兜底——这里不允许「立刻把所有 pending 都列成孤儿」。
// no_capability 那一类不受安全阀约束：能力行都没了，等再久也不会自己好。
//
// limit <=0 用 200，上限 1000：这是给人看的诊断视图，不是数据导出接口。
func (d *DB) ListOrphanPendingTasks(ctx context.Context, namespaceID string, olderThanSec, limit int) ([]OrphanPendingTask, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if olderThanSec <= 0 {
		olderThanSec = 3600
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	now := Now()
	cutoff := now - int64(olderThanSec)

	// ① 能力行已被删的 pending 任务（与 age 无关）。
	rows, err := d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks t
		  WHERE t.namespace_id = ? AND t.phase = ?
		    AND NOT EXISTS (SELECT 1 FROM capability_versions cv WHERE cv.id = t.capability_id)
		  ORDER BY t.created_at, t.id
		  LIMIT ?`, namespaceID, TaskPending, limit)
	if err != nil {
		return nil, fmt.Errorf("list orphan pending tasks: %w", err)
	}
	out := []OrphanPendingTask{}
	seen := map[string]bool{}
	for rows.Next() {
		t, serr := scanTask(rows)
		if serr != nil {
			rows.Close()
			return nil, serr
		}
		seen[t.ID] = true
		out = append(out, OrphanPendingTask{Task: *t, IdleSec: idleSec(now, t.CreatedAt), Reason: OrphanNoCapability})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list orphan pending tasks: %w", err)
	}
	rows.Close()

	// ② 能力还在、但超过安全阀且没有任何在线节点能满足派发前提的任务。
	if len(out) >= limit {
		return out, nil
	}
	rows, err = d.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks t
		  WHERE t.namespace_id = ? AND t.phase = ? AND t.created_at <= ?
		    AND EXISTS (SELECT 1 FROM capability_versions cv WHERE cv.id = t.capability_id)
		    AND NOT EXISTS (
		        SELECT 1 FROM nodes n
		         WHERE n.status = ?
		           AND EXISTS (SELECT 1 FROM deployments dp
		                        WHERE dp.node_id = n.id AND dp.capability_id = t.capability_id
		                          AND dp.digest = t.digest AND dp.state = ?))
		  ORDER BY t.created_at, t.id
		  LIMIT ?`, namespaceID, TaskPending, cutoff, NodeStatusOnline, DeploymentApplied, limit-len(out))
	if err != nil {
		return nil, fmt.Errorf("list orphan pending tasks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, serr := scanTask(rows)
		if serr != nil {
			return nil, serr
		}
		if seen[t.ID] {
			continue
		}
		out = append(out, OrphanPendingTask{Task: *t, IdleSec: idleSec(now, t.CreatedAt), Reason: OrphanNoOnlineNode})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list orphan pending tasks: %w", err)
	}
	return out, nil
}

// idleSec 是「创建至今」的秒数（负值归零：时钟回拨不该给出负数）。
func idleSec(now, createdAt int64) int64 {
	if now <= createdAt {
		return 0
	}
	return now - createdAt
}

// AbandonPendingTask 把一条仍处于 pending 的任务显式转成 abandoned，并写入原因。
//
// 必须走 CAS（`WHERE phase='pending'`）而不是「先查再改」：查与改之间任务可能刚被派发
// 出去（节点上线了），此时把它标成 abandoned 会把一条**正在执行**的任务变成终态，
// 节点随后回报的 result 会被状态机拒绝，事故现场就毁了。
//
// 返回 ErrNotFound / ErrInvalidTransition 由调用方翻译成 404 / 409：
// 「它已经不在 pending 了」是使用者需要知道的事实，不能假装成功。
func (d *DB) AbandonPendingTask(ctx context.Context, namespaceID, taskID, reason string) error {
	if namespaceID == "" || taskID == "" {
		return fmt.Errorf("store: namespaceID and taskID required")
	}
	if reason == "" {
		reason = protocol.ReasonAbandonedByUser
	}
	now := Now()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abandon pending task: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET phase = ?, finished_at = ?, error = ?, waiting_reason = '', next_attempt_at = NULL
		  WHERE namespace_id = ? AND id = ? AND phase = ?`,
		TaskAbandoned, now, reason, namespaceID, taskID, TaskPending)
	if err != nil {
		return fmt.Errorf("abandon pending task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 区分「不存在」与「已不在 pending」：调用方要翻 404 还是 409。
		var phase string
		qerr := tx.QueryRowContext(ctx,
			`SELECT phase FROM tasks WHERE namespace_id = ? AND id = ?`, namespaceID, taskID).Scan(&phase)
		if errors.Is(qerr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if qerr != nil {
			return fmt.Errorf("abandon pending task: read phase: %w", qerr)
		}
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, taskID, phase)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("abandon pending task: commit: %w", err)
	}
	return nil
}
