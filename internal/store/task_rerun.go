package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrTaskNotRerunnable = errors.New("task_not_rerunnable")

type RerunInputs struct {
	NamespaceID string
	TaskID      string
	Version     int
	Now         int64
}

func (d *DB) CreateRerunTask(ctx context.Context, in RerunInputs) (*Task, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if strings.TrimSpace(in.TaskID) == "" {
		return nil, fmt.Errorf("create rerun task: empty task id")
	}
	if in.Version < 0 {
		return nil, fmt.Errorf("create rerun task: version must be >= 0（0 = live）")
	}
	if in.Now == 0 {
		in.Now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create rerun task: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	if err := requireNamespaceOn(ctx, ex, in.NamespaceID); err != nil {
		return nil, err
	}
	orig, err := getTaskOn(ctx, ex, in.NamespaceID, in.TaskID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(orig.Capability)
	if name == "" {
		return nil, fmt.Errorf("%w: 任务 %s 没有能力名，无法确定要重跑什么", ErrTaskNotRerunnable, orig.ID)
	}

	var (
		capID   string
		version int
		digest  string
	)
	if in.Version > 0 {
		err = ex.queryRowContext(ctx,
			`SELECT id, version, digest FROM capability_versions
			  WHERE namespace_id = ? AND name = ? AND version = ? AND deleted_at IS NULL`,
			in.NamespaceID, name, in.Version).Scan(&capID, &version, &digest)
	} else {
		err = ex.queryRowContext(ctx,
			`SELECT id, version, digest FROM capability_versions
			  WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL
			  ORDER BY version DESC LIMIT 1`,
			in.NamespaceID, name).Scan(&capID, &version, &digest)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create rerun task: resolve capability %s: %w", name, err)
	}
	if capID == "" || digest == "" || version <= 0 {
		return nil, fmt.Errorf("create rerun task: capability %s@%d 没有真实 version/digest", name, version)
	}

	params := make([]byte, len(orig.Params))
	copy(params, orig.Params)
	task := &Task{
		NamespaceID:  in.NamespaceID,
		TraceID:      NewEventID(),
		CapabilityID: capID,
		Capability:   name,
		Version:      version,
		Digest:       digest,
		Params:       params,
		Phase:        TaskPending,
		CreatedAt:    in.Now,
		RerunOf:      orig.ID,
	}
	if err := d.createTasksTx(ctx, tx, []*Task{task}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create rerun task: commit: %w", err)
	}
	return task, nil
}

func getTaskOn(ctx context.Context, ex queryExec, namespaceID, id string) (*Task, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := ex.queryContext(ctx,
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
