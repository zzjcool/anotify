package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// router 的判决与结局派发（#522）。

var (
	ErrDecisionInvalid      = errors.New("invalid_decision")
	ErrDecisionNotRouter    = errors.New("not_a_router_task")
	ErrOutcomeNotDeclared   = errors.New(protocol.ReasonOutcomeNotDeclared)
	ErrDecisionAlreadyTaken = errors.New(protocol.ReasonDecisionAlreadyTaken)
)

type Decision struct {
	Outcome string         `json:"outcome"`
	Params  map[string]any `json:"params,omitempty"`
	At      int64          `json:"at"`
}

func ParseDecision(raw json.RawMessage) *Decision {
	if len(raw) == 0 {
		return nil
	}
	var decision Decision
	if err := json.Unmarshal(raw, &decision); err != nil || decision.Outcome == "" {
		return nil
	}
	return &decision
}

func ParseOutcomes(raw json.RawMessage) map[string][]sourcespec.OutcomeAction {
	if len(raw) == 0 {
		return nil
	}
	var outcomes map[string][]sourcespec.OutcomeAction
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		return nil
	}
	return outcomes
}

type DecisionInput struct {
	TaskID  string
	Token   string
	Outcome string
	Params  map[string]any
	Now     int64
}

type DecisionRecord struct {
	TaskID  string
	Outcome string
	Params  map[string]any
	Actions int
	At      int64
}

// RecordDecision 校验「凭据 ↔ 任务」、结局归属与幂等，然后落一次判决。
func (d *DB) RecordDecision(ctx context.Context, in DecisionInput) (*DecisionRecord, error) {
	if strings.TrimSpace(in.TaskID) == "" || strings.TrimSpace(in.Token) == "" ||
		strings.TrimSpace(in.Outcome) == "" {
		return nil, fmt.Errorf("%w: taskId / 凭据 / outcome 都不能为空", ErrDecisionInvalid)
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("record decision %s: begin: %w", in.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	var (
		namespaceID string
		traceID     string
		capability  string
		phase       string
		rawOutcomes string
		rawDecision string
		tokenHash   string
		expiresAt   sql.NullInt64
	)
	err = ex.queryRowContext(ctx,
		`SELECT namespace_id, trace_id, capability, phase, outcomes, decision,
		        decision_token_hash, decision_expires_at
		   FROM tasks WHERE id = ?`, in.TaskID).
		Scan(&namespaceID, &traceID, &capability, &phase, &rawOutcomes, &rawDecision, &tokenHash, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s 不存在", ErrAttemptTokenInvalid, in.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("record decision %s: read: %w", in.TaskID, err)
	}
	if tokenHash == "" || !constantTimeHexEqual(hashAttemptToken(in.Token), tokenHash) {
		return nil, fmt.Errorf("%w: task %s 的决策凭据不匹配", ErrAttemptTokenInvalid, in.TaskID)
	}
	if expiresAt.Valid && expiresAt.Int64 < in.Now {
		return nil, fmt.Errorf("%w: task %s 的决策凭据已过期", ErrAttemptTokenInvalid, in.TaskID)
	}
	outcomes := ParseOutcomes(json.RawMessage(rawOutcomes))
	if len(outcomes) == 0 {
		return nil, fmt.Errorf("%w: task %s 没有 outcomes 表", ErrDecisionNotRouter, in.TaskID)
	}
	if strings.TrimSpace(rawDecision) != "" {
		return nil, fmt.Errorf("%w: task %s 已经判决过", ErrDecisionAlreadyTaken, in.TaskID)
	}
	if IsTerminalPhase(phase) {
		return nil, fmt.Errorf("%w: task %s 已结束（%s）", ErrTaskNotRunning, in.TaskID, phase)
	}
	actions, declared := outcomes[in.Outcome]
	if !declared {
		payload, _ := json.Marshal(map[string]any{
			"taskId":     in.TaskID,
			"capability": capability,
			"outcome":    in.Outcome,
			"declared":   sortedOutcomeNames(outcomes),
		})
		if eerr := insertPruneFactEventOn(ctx, ex, &Event{
			NamespaceID: namespaceID,
			TraceID:     traceID,
			Type:        protocol.ReasonOutcomeNotDeclared,
			Payload:     payload,
			CreatedAt:   in.Now,
		}); eerr != nil {
			return nil, fmt.Errorf("record decision %s: audit: %w", in.TaskID, eerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return nil, fmt.Errorf("record decision %s: commit audit: %w", in.TaskID, cerr)
		}
		return nil, fmt.Errorf("%w: 结局 %q 不在 task %s 的声明表里（已声明：%v）",
			ErrOutcomeNotDeclared, in.Outcome, in.TaskID, sortedOutcomeNames(outcomes))
	}
	decision := Decision{Outcome: in.Outcome, Params: in.Params, At: in.Now}
	raw, err := json.Marshal(decision)
	if err != nil {
		return nil, fmt.Errorf("record decision %s: marshal: %w", in.TaskID, err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET decision = ?, decision_at = ?
		  WHERE id = ? AND decision = ''`,
		string(raw), in.Now, in.TaskID)
	if err != nil {
		return nil, fmt.Errorf("record decision %s: write: %w", in.TaskID, err)
	}
	if n, aerr := res.RowsAffected(); aerr != nil {
		return nil, fmt.Errorf("record decision %s: rows: %w", in.TaskID, aerr)
	} else if n != 1 {
		return nil, fmt.Errorf("%w: task %s 已经判决过", ErrDecisionAlreadyTaken, in.TaskID)
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, fmt.Errorf("record decision %s: commit: %w", in.TaskID, cerr)
	}
	return &DecisionRecord{
		TaskID: in.TaskID, Outcome: in.Outcome, Params: in.Params,
		Actions: len(actions), At: in.Now,
	}, nil
}

type OutcomeDispatchInputs struct {
	NamespaceID  string
	TraceID      string
	ParentTaskID string
	Now          int64
	Tasks        []*Task
	Failures     []DispatchFailureInputs
	Prunes       []PruneInputs
}

func (d *DB) CreateOutcomeDispatch(ctx context.Context, in OutcomeDispatchInputs) error {
	if err := prepareOutcomeDispatch(&in); err != nil {
		return err
	}
	if !in.hasActions() {
		return nil // 空动作结局（drop）不建行：留痕由调用方单独写
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("outcome dispatch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.createOutcomeDispatchTx(ctx, tx, in); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("outcome dispatch: commit: %w", err)
	}
	return nil
}

func (in OutcomeDispatchInputs) hasActions() bool {
	return len(in.Tasks) > 0 || len(in.Failures) > 0 || len(in.Prunes) > 0
}

func prepareOutcomeDispatch(in *OutcomeDispatchInputs) error {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	for i, t := range in.Tasks {
		if t == nil {
			return fmt.Errorf("outcome dispatch: nil task at index %d", i)
		}
		if in.ParentTaskID != "" {
			if t.ParentTaskID == nil || *t.ParentTaskID == "" {
				parent := in.ParentTaskID
				t.ParentTaskID = &parent
			} else if *t.ParentTaskID != in.ParentTaskID {
				return fmt.Errorf("outcome dispatch: task %d parent %q != %q", i, *t.ParentTaskID, in.ParentTaskID)
			}
		}
		if t.NamespaceID != "" && t.NamespaceID != in.NamespaceID {
			return fmt.Errorf("outcome dispatch: task %d namespace %q != %q", i, t.NamespaceID, in.NamespaceID)
		}
		if t.TraceID != "" && t.TraceID != in.TraceID {
			return fmt.Errorf("outcome dispatch: task %d trace %q != %q", i, t.TraceID, in.TraceID)
		}
	}
	for i, p := range in.Prunes {
		if !protocol.IsPruneReason(p.Reason) {
			return fmt.Errorf("outcome dispatch: prune %d reason %q 不是剪枝码（downstream_missing 走失败 Task）", i, p.Reason)
		}
	}
	return nil
}

func (d *DB) createOutcomeDispatchTx(ctx context.Context, tx *sql.Tx, in OutcomeDispatchInputs) error {
	ex := txExec{tx: tx}
	if err := d.createTasksTx(ctx, tx, in.Tasks); err != nil {
		return err
	}
	for _, failure := range in.Failures {
		row, ferr := newDispatchFailureTask(in.NamespaceID, in.TraceID, nil, failure, in.Now)
		if ferr != nil {
			return ferr
		}
		if err := insertDispatchFailureTaskOn(ctx, ex, row, failure, in.Now); err != nil {
			return err
		}
	}
	for _, prune := range in.Prunes {
		payload, _ := json.Marshal(map[string]any{
			"capability": prune.Capability, "reason": prune.Reason, "detail": prune.Detail,
		})
		if err := insertPruneFactEventOn(ctx, ex, &Event{
			NamespaceID: in.NamespaceID, TraceID: in.TraceID,
			Type: prune.Reason, Payload: payload, CreatedAt: in.Now,
		}); err != nil {
			return err
		}
	}
	return nil
}

type FailureFact struct {
	TaskID      string
	Observation Event
	Dispatch    OutcomeDispatchInputs
}

// RecordFailureFact 在同一事务里先证「本任务还没有失败事实」：cancel.ack 无回执，重连即重放（#700）。
func (d *DB) RecordFailureFact(ctx context.Context, in FailureFact) (bool, error) {
	if in.TaskID == "" {
		return false, fmt.Errorf("failure fact: empty task id")
	}
	if in.Dispatch.hasActions() {
		if err := prepareOutcomeDispatch(&in.Dispatch); err != nil {
			return false, err
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failure fact: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	var seen int
	if err := ex.queryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE namespace_id = ? AND trace_id = ? AND type IN (?, ?)
		   AND CASE WHEN json_valid(payload) THEN json_extract(payload, '$.taskId') END = ?`,
		in.Observation.NamespaceID, in.Observation.TraceID, protocol.ReasonTaskFailed,
		protocol.ReasonRouterFailed, in.TaskID).Scan(&seen); err != nil {
		return false, fmt.Errorf("failure fact: 查重: %w", err)
	}
	if seen > 0 {
		return false, nil
	}
	event := in.Observation
	if err := insertPruneFactEventOn(ctx, ex, &event); err != nil {
		return false, err
	}
	if in.Dispatch.hasActions() {
		if err := d.createOutcomeDispatchTx(ctx, tx, in.Dispatch); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failure fact: commit: %w", err)
	}
	return true, nil
}

func (d *DB) InsertRouteObservation(ctx context.Context, namespaceID, traceID, reason string, payload map[string]any) error {
	if !protocol.IsRouteObservationReason(reason) {
		return fmt.Errorf("route observation: %q 不是决策通道观察码（允许：%v）",
			reason, protocol.RouteObservationReasons())
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("route observation: marshal: %w", err)
	}
	return insertPruneFactEventOn(ctx, d, &Event{
		NamespaceID: namespaceID, TraceID: traceID, Type: reason, Payload: raw, CreatedAt: Now(),
	})
}

func sortedOutcomeNames(outcomes map[string][]sourcespec.OutcomeAction) []string {
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func constantTimeHexEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
