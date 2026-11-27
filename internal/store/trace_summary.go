package store

import (
	"context"
	"fmt"
)

// 一条 trace 的派发结果聚合：事件列表的 `status.dispatched` / `status.prune*` 用它。
//
// 为什么聚合在 store 而不在 server：`tasks` 与 `events` 是两张表的事实，「这条 trace
// 建了几个任务、剪了哪些边」只有 SQL 能回答；server 只做投影（internal/server/AGENTS.md
// §6：store 给事实，server 只投影）。
//
// 为什么批量：一条事件列表是 50~500 行，逐行调用 CountTasksByTrace 就是 N+1 次查询；
// 这里按 trace_id IN (...) 一次分组拿回（分块常量见下）。

// traceSummaryChunk 是 IN (...) 的分块大小：SQLite 的宿主参数上限是每语句 999
// （老版本）/32766（新版本），200 远低于两者，也不会把索引查询拆得太碎。
const traceSummaryChunk = 200

// TraceSummary 是一条 trace 的派发结果：建成的任务数 + 剪枝观察行。
//
// Pruned 里**含**未解析派发失败（`source=route`、`type=downstream_missing`）：它在库里
// 与剪枝留痕共用同一份边写，只有投影层能按 capability.md §6.2 把它从「剪枝」里摘出去
// （server 的 eventIsDispatchFailure）。store 不做展示裁剪。
type TraceSummary struct {
	// Tasks 是该 trace 的 tasks 行数（含 failed/abandoned：都是真的建过的任务）。
	Tasks int
	// Pruned 是该 trace 的 `source='route'` 观察行（按建行顺序 created_at, id）。
	Pruned []*Event
}

// SummarizeTraces 批量取若干 trace 的派发结果（未命中 / 无任务的 trace 不在 map 里，
// 读零值即「没派发、没剪枝」）。
//
// namespaceID 必须是具体 ns（`*` 不是查询条件，跨 ns 汇总由调用方逐 ns 取再合并）。
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

// summarizeTraceChunk 一次取一块 trace 的任务数与剪枝观察行。
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
		return fmt.Errorf("summarize traces: list prunes: %w", err)
	}
	defer prRows.Close()
	for prRows.Next() {
		e, err := scanEventRow(prRows)
		if err != nil {
			return err
		}
		sum := out[e.TraceID]
		sum.Pruned = append(sum.Pruned, e)
		out[e.TraceID] = sum
	}
	if err := prRows.Err(); err != nil {
		return fmt.Errorf("summarize traces: list prunes: %w", err)
	}
	return nil
}

// uniqueTraceIDs 去空、去重并保持首次出现顺序（IN 列表只关心集合，顺序只为稳定）。
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
