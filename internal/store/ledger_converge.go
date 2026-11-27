package store

import (
	"context"
	"fmt"
)

type LedgerConvergence struct {
	AttemptsReaped int64 `json:"attemptsReaped"`
	SlotsReleased  int64 `json:"slotsReleased"`
}

// 取消在飞等 cancel.ack 的宽限（秒）：宽限内 ack 才是权威事实（#660），过宽限仍占槽 = ack 不会再来（#682）。
const terminalSlotGrace int64 = 600

// ConvergeTerminalLedgers 收敛终态任务遗留的两本账，幂等；判据/边界见 docs/reports/issue-682-terminal-ledger-converge.md。
func (d *DB) ConvergeTerminalLedgers(ctx context.Context, now int64) (LedgerConvergence, error) {
	var out LedgerConvergence
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("converge terminal ledgers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	phases, phaseArgs := terminalPhaseIn() // 终态只有一份事实（IsTerminalPhase）
	stale := `SELECT id FROM tasks
	            WHERE phase IN (` + phases + `)
	              AND (cancel_requested_at IS NULL OR cancel_requested_at <= ?)`

	reapArgs := append([]any{now, ReclaimReasonSweepConverge, now}, phaseArgs...)
	reapArgs = append(reapArgs, now-terminalSlotGrace)
	res, err := ex.execContext(ctx,
		`UPDATE task_attempts
		    SET reaped_at = ?, reclaim_reason = ?, finished_at = COALESCE(finished_at, ?)
		  WHERE reaped_at IS NULL AND task_id IN (`+stale+`)`, reapArgs...)
	if err != nil {
		return out, fmt.Errorf("converge terminal ledgers: reap attempts: %w", err)
	}
	if out.AttemptsReaped, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("converge terminal ledgers: reap rows: %w", err)
	}

	slotArgs := append([]any{now}, phaseArgs...)
	slotArgs = append(slotArgs, now-terminalSlotGrace)
	res, err = ex.execContext(ctx,
		`UPDATE node_instances
		    SET busy_task = NULL, busy_attempt = 0, busy_lease_id = '',
		        busy_assignment_id = '', busy_expires_at = 0, updated_at = ?
		  WHERE busy_task IN (`+stale+`)`, slotArgs...)
	if err != nil {
		return out, fmt.Errorf("converge terminal ledgers: release slots: %w", err)
	}
	if out.SlotsReleased, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("converge terminal ledgers: release rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("converge terminal ledgers: commit: %w", err)
	}
	return out, nil
}
