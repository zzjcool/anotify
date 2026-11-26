package store

import (
	"context"
	"fmt"
)

// 存量收敛（#660）：旧版 abandon 留下的行（reaped NULL + busy_* 指着已终结任务）只能
// 按「任务已是终态」收敛（只覆盖没有取消在飞的），第二轮计数归零。
type LedgerConvergence struct {
	AttemptsReaped int64 `json:"attemptsReaped"`
	SlotsReleased  int64 `json:"slotsReleased"`
}

// ConvergeAbandonedLedgers 收敛存量并返回本轮计数（now 为 0 时取 Now()）。
func (d *DB) ConvergeAbandonedLedgers(ctx context.Context, now int64) (LedgerConvergence, error) {
	var out LedgerConvergence
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	const abandonedNoCancel = `SELECT id FROM tasks WHERE phase = ? AND cancel_requested_at IS NULL`
	res, err := ex.execContext(ctx,
		`UPDATE task_attempts
		    SET reaped_at = ?, reclaim_reason = ?, finished_at = COALESCE(finished_at, ?)
		  WHERE reaped_at IS NULL AND task_id IN (`+abandonedNoCancel+`)`,
		now, ReclaimReasonSweepConverge, now, TaskAbandoned)
	if err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: reap attempts: %w", err)
	}
	if out.AttemptsReaped, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: reap rows: %w", err)
	}

	res, err = ex.execContext(ctx,
		`UPDATE node_instances
		    SET busy_task = NULL, busy_attempt = 0, busy_lease_id = '',
		        busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
		  WHERE busy_task IN (`+abandonedNoCancel+`)`,
		now, TaskAbandoned)
	if err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: release slots: %w", err)
	}
	if out.SlotsReleased, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: release rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("converge abandoned ledgers: commit: %w", err)
	}
	return out, nil
}
