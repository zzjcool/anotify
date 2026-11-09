package store

import (
	"context"
	"fmt"
)

// ListEventsBefore 是 ListEvents 的**游标版**：从 (beforeCreatedAt, beforeID) 这一行
// 之前（更旧）开始取，其余语义与 ListEvents 逐字一致。
//
// 两份 SQL 必须永远等价：同样的 `source <> 'route'` 剪枝（仅当 source=="" 且
// traceID=="" 时）、同样的 ORDER BY、同样的 limit 夹取、同样的列顺序与 scan。
// 唯一允许的差异就是游标谓词 `AND (created_at < ? OR (created_at = ? AND id < ?))`。
// 等价性由 v4test 的 TestEventPageEquivalence 兜住（改剪枝规则时它会红）。
func (d *DB) ListEventsBefore(ctx context.Context, namespaceID, traceID, source string,
	beforeCreatedAt int64, beforeID string, limit int) ([]*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	// limit 夹取必须与 ListEvents 同一实现：两个函数的差别只允许是游标谓词，
	// 否则游标分页的第 1 页与后续页会取到不同窗口。
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q, args := eventPageQuery(namespaceID, traceID, source, beforeCreatedAt, beforeID, limit)

	rows, err := d.cachedQueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events before: %w", err)
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		e, err := scanEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// eventPageQuery 拼出 ListEventsBefore 真正执行的 SQL 与参数。
//
// 单独抽出来是为了让 EXPLAIN QUERY PLAN 断言跑在**真实**语句上：把 SQL 复制一份到
// 测试里写着更省事，但那是在断言测试自己的副本——真实查询换了谓词测试照样绿。
func eventPageQuery(namespaceID, traceID, source string,
	beforeCreatedAt int64, beforeID string, limit int) (string, []any) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE namespace_id = ?`
	args := []any{namespaceID}
	if traceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, traceID)
	}
	if source != "" {
		q += ` AND source = ?`
		args = append(args, source)
	} else if traceID == "" {
		q += ` AND source <> ?`
		args = append(args, EventPruneSource)
	}
	// 游标：严格「更旧」。同一 created_at 可能有多行（同秒批量落库是常态），
	// 所以破平局必须带 id——只按 created_at 比较会在页边界丢行或重复行。
	q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
	args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return q, args
}
