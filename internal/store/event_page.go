package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// ListEventsBefore 是 ListEvents 的**游标版**：从 (beforeCreatedAt, beforeID) 这一行
// 之前（更旧）开始取，其余语义与 ListEvents 逐字一致（#235）。
//
// ⚠️ 本文件是**第二份**描述「哪些事件属于这个视图」的 SQL。
// 第一份在 internal/store/event.go 的 ListEvents 里。两份必须永远等价：
// 同样的 `source != 'route'` 剪枝（仅当 source=="" 且 traceID=="" 时）、同样的
// `ORDER BY created_at DESC, id DESC`、同样的 limit 夹取、同样的列顺序与 scan。
// 唯一允许的差异就是游标谓词 `AND (created_at < ? OR (created_at = ? AND id < ?))`。
//
// 将来有人只改 event.go 的剪枝规则（比如再加一条「某类 source 不进事件流」）时，
// **只有 internal/store/event_page_test.go 的 TestListEventsBeforeEquivalence
// 能抓到这次分叉**——它把四种口径（纯事件流 / source=""+traceID="" / 显式 source /
// 显式 traceID）下 ListEventsBefore(+∞) 与 ListEvents 的结果逐条比对 id 与顺序。
// 改剪枝规则时请顺手让它继续绿，别把那份测试一起删掉。
//
// 为什么不在 event.go 里加个参数：并行批次正在改同一片代码，event.go 一行不动是
// 前提（见批次 4B 冻结契约 §1/§3.1）；两份 SQL 的代价由上面的等价性测试兜住。
func (d *DB) ListEventsBefore(ctx context.Context, namespaceID, traceID, source string,
	beforeCreatedAt int64, beforeID string, limit int) ([]*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	// limit 夹取**必须**与 ListEvents 保持同一实现（含 `> 500` 折回 50 这个已知缺陷）：
	// 两个函数的差别只允许是游标谓词，否则游标分页的第 1 页与后续页会取到不同的窗口。
	// 缺陷本身由 handler 侧夹住（#236：调用方永远传 ≤ 500），不在这里改 —— event.go 不能动。
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q, args := eventPageQuery(namespaceID, traceID, source, beforeCreatedAt, beforeID, limit)

	rows, err := d.cachedQueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events before: %w", err)
	}
	defer rows.Close()
	// 空结果返回 []*Event{} 而不是 nil（不变量 I9）：前端拿 `null` 会误判「未连接」。
	out := []*Event{}
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.NamespaceID, &e.TraceID, &e.Source, &e.Type, &payload, &e.IdemKey, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// eventPageQuery 拼出 ListEventsBefore 真正执行的 SQL 与参数。
//
// 为什么单独抽出来：event_page_test.go 的 EXPLAIN QUERY PLAN 断言必须跑在**真实**
// 语句上。把 SQL 复制一份到测试里写着更省事，但那是在断言测试自己的副本 ——
// 真实查询换了谓词（比如有人把 OR 改成子查询）测试照样绿，索引退化当场漏过去。
//
// 调用前 namespaceID/limit 已校验（本函数不做业务校验，只拼 SQL）。
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
		// 'route' 是 route.PruneEventSource 的字面量：store 不 import route
		// （route 依赖 store，反向 import 会成环），所以这里写常量字符串。
		// 这条剪枝必须与 ListEvents 一字不差，否则事件流与它的分页会是两个口径。
		q += ` AND source != 'route'`
	}
	// 游标：严格「更旧」。同一 created_at 可能有多行（同秒批量落库是常态），
	// 所以破平局必须带 id —— 只按 created_at 比较会在页边界丢行或重复行。
	q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
	args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return q, args
}
