package store

import (
	"context"
	"fmt"
)

const traceSummaryChunk = 200

type TraceSummary struct {
	Tasks       int
	RouteEvents []*Event
}

func (d *DB) SummarizeTraces(ctx context.Context, namespaceID string, traceIDs []string) (map[string]TraceSummary, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	out := make(map[string]TraceSummary, len(traceIDs))
	ids := uniqueTraceIDs(traceIDs)
	for start := 0; start < len(ids); start += traceSummaryChunk {
		end := start + traceSummaryChunk
		if end > len(ids) {
			end = len(ids)
		}
		if err := d.summarizeTraceChunk(ctx, namespaceID, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) summarizeTraceChunk(ctx context.Context, namespaceID string, ids []string, out map[string]TraceSummary) error {
	ph := placeholdersFor(len(ids))
	args := append([]any{namespaceID}, anyArgs(ids)...)

	rows, err := d.cachedQueryContext(ctx,
		`SELECT trace_id, COUNT(*) FROM tasks
		  WHERE namespace_id = ? AND trace_id IN (`+ph+`)
		  GROUP BY trace_id`, args...)
	if err != nil {
		return fmt.Errorf("summarize traces: count tasks: %w", err)
	}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return fmt.Errorf("summarize traces: scan task count: %w", err)
		}
		sum := out[id]
		sum.Tasks = n
		out[id] = sum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("summarize traces: count tasks: %w", err)
	}
	rows.Close()

	pruneArgs := append([]any{namespaceID, EventPruneSource}, anyArgs(ids)...)
	prRows, err := d.cachedQueryContext(ctx,
		`SELECT `+eventColumns+` FROM events
		  WHERE namespace_id = ? AND source = ? AND trace_id IN (`+ph+`)
		  ORDER BY created_at, id`, pruneArgs...)
	if err != nil {
		return fmt.Errorf("summarize traces: list route events: %w", err)
	}
	defer prRows.Close()
	for prRows.Next() {
		e, err := scanEventRow(prRows)
		if err != nil {
			return err
		}
		sum := out[e.TraceID]
		sum.RouteEvents = append(sum.RouteEvents, e)
		out[e.TraceID] = sum
	}
	if err := prRows.Err(); err != nil {
		return fmt.Errorf("summarize traces: list route events: %w", err)
	}
	return nil
}

func uniqueTraceIDs(traceIDs []string) []string {
	seen := make(map[string]struct{}, len(traceIDs))
	out := make([]string, 0, len(traceIDs))
	for _, id := range traceIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
