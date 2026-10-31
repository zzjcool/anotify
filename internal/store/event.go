package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Event 是 events 表的一行：事件日志的起点（链的根）。
//
// TraceID 由**触发器**生成并写进来（契约 §10 Q2），中枢不代生成、不猜。
// Payload 只存不解释（不变量 I6：中枢不解释任何用户提供的逻辑）。
type Event struct {
	ID string `json:"id"`
	// NamespaceID 是命名空间隔离维度：事件属于某个命名空间，trace 列表与统计都按它过滤。
	NamespaceID string          `json:"namespaceId"`
	TraceID     string          `json:"traceId"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	IdemKey     *string         `json:"idemKey,omitempty"`
	CreatedAt   int64           `json:"createdAt"`
}

// eventColumns 是统一的列顺序，scanEvent 依赖它。
const eventColumns = `id, namespace_id, trace_id, source, type, payload, idem_key, created_at`

// ErrReservedEventSource 表示有人把「保留来源名」当普通来源用。
//
// `source='route'` 是剪枝记录的保留值（#142：events 里 type='pruned.<reason>'、
// source='route' 的行就是一条剪枝）。业务事件若用了这个 source，会被
// ListEvents 的事件流过滤掉（**静默消失**）、并在 trace 视图里被当成剪枝。
// 这种丢数据的失败必须当场报错，不能默默接受。
//
// 文案不带动词前缀：它会被「insert event:」/「create source:」/「update source:」
// 三种上下文分别包装，自带前缀会叠成「insert event: insert event: …」。
var ErrReservedEventSource = errors.New(`"route" 是保留来源名（剪枝记录专用，#142）`)

// InsertEvent 幂等写入事件（契约 §10.1 / Q6）。
//
// 带 IdemKey 且冲突时返回 (false, nil)，并把 e 填成**已有行的 event_id + trace_id**
// ——不是 409：对发送方而言「已收过」和「本次收下」都该是 200。
// 不带 IdemKey 时允许重复（不是所有来源都有稳定投递 ID，硬造反而制造假幂等）。
func (d *DB) InsertEvent(ctx context.Context, e *Event) (inserted bool, err error) {
	if e == nil {
		return false, fmt.Errorf("insert event: nil")
	}
	if e.NamespaceID == "" || e.NamespaceID == AllNamespaces {
		return false, ErrNamespaceRequired
	}
	if e.Source == "" || e.Type == "" {
		return false, fmt.Errorf("insert event: source and type required")
	}
	if e.TraceID == "" {
		return false, fmt.Errorf("insert event: empty trace_id (触发器必须生成 trace_id)")
	}
	// 保留来源名（#142）：这是业务事件**唯一**的落库入口——webhook / cron /
	// watch / source.Trigger.Fire 全走它，所以这里就是防「用户建一个叫 route
	// 的源」的水闸。不拦的话该源的事件会被 ListEvents 的事件流静默过滤掉，
	// 并在 trace 视图里被误当成剪枝（两种错误都不可观测）。
	// 剪枝自己不走这里（走 InsertPruneEvent），不受影响。
	// 不在这里加 "insert event: " 前缀：两个调用方（trigger.Ingest /
	// source.Trigger.Fire）各自已包了一层上下文，再加就变成
	// 「insert event: insert event: …」。哨兵自己就说得清。
	if e.Source == "route" {
		return false, ErrReservedEventSource
	}
	payload := string(e.Payload)
	if payload == "" || !json.Valid(e.Payload) {
		payload = "{}"
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if e.ID == "" {
		e.ID = NewEventID()
	}

	// 幂等写：不能写 `ON CONFLICT(namespace_id, source, idem_key)`——SQLite 的
	// upsert 目标必须是完整唯一约束或主键，而这里用的是**部分**唯一索引
	// （`WHERE idem_key IS NOT NULL`），指定列会报
	// 「ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint」。
	// 用 INSERT OR IGNORE 让任何唯一冲突都静默跳过，再由 RowsAffected 判断是否插入。
	res, err := d.ExecContext(ctx,
		`INSERT OR IGNORE INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.ID, e.NamespaceID, e.TraceID, e.Source, e.Type, payload, e.IdemKey, e.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert event rows: %w", err)
	}
	if n == 1 {
		e.Payload = json.RawMessage(payload)
		return true, nil
	}

	// 冲突：回查已有行，把它填回 e（调用方拿到的是「已有的 event_id + trace_id」）
	if e.IdemKey == nil {
		// 理论上不可达（没有 idem_key 不会冲突），但保持防御
		return false, fmt.Errorf("insert event: conflict without idem_key")
	}
	existing, gerr := d.getEventByIdemKey(ctx, e.NamespaceID, e.Source, *e.IdemKey)
	if gerr != nil {
		return false, fmt.Errorf("read existing event after conflict: %w", gerr)
	}
	*e = *existing
	return false, nil
}

// InsertPruneEvent 幂等写入一条剪枝事件（source='route'，#142）。
//
// 已存在同一条（namespace_id, trace_id, source, type, payload）时不重复写，返回 (false, nil)。
// idem_key 恒为 NULL：剪枝是观察记录，不是一次投递，不占幂等键语义
// （也不能复用 InsertEvent 的 INSERT OR IGNORE——那条路径靠部分唯一索引生效，
// 剪枝没有 idem_key，冲突永远不会被识别）。
//
// 为什么去重是「一条 SQL 里的存在性判断」而不是先 SELECT 再 INSERT：
// 两条语句之间存在时间窗，同一条 trace 的并发派发（扇出）会双双落库。
func (d *DB) InsertPruneEvent(ctx context.Context, e *Event) (inserted bool, err error) {
	// 校验与默认值与 InsertEvent 保持一致（同一种行，不能两套口径）。
	if e == nil {
		return false, fmt.Errorf("insert prune event: nil")
	}
	if e.NamespaceID == "" || e.NamespaceID == AllNamespaces {
		return false, ErrNamespaceRequired
	}
	if e.Source == "" || e.Type == "" {
		return false, fmt.Errorf("insert prune event: source and type required")
	}
	if e.TraceID == "" {
		return false, fmt.Errorf("insert prune event: empty trace_id")
	}
	payload := string(e.Payload)
	if payload == "" || !json.Valid(e.Payload) {
		payload = "{}"
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if e.ID == "" {
		e.ID = NewEventID()
	}
	// 与实际执行 insert 的 SELECT 保持一致：NULL 是硬编码的，
	// 这里同步置空，避免调用方拿到一个「幂等键不为空」的幻象。
	e.IdemKey = nil

	res, err := d.ExecContext(ctx,
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
		 SELECT ?,?,?,?,?,?,NULL,?
		 WHERE NOT EXISTS (
		     SELECT 1 FROM events
		      WHERE namespace_id = ? AND trace_id = ? AND source = ? AND type = ? AND payload = ?
		 )`,
		e.ID, e.NamespaceID, e.TraceID, e.Source, e.Type, payload, e.CreatedAt,
		e.NamespaceID, e.TraceID, e.Source, e.Type, payload)
	if err != nil {
		return false, fmt.Errorf("insert prune event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert prune event rows: %w", err)
	}
	if n == 0 {
		return false, nil // 已有同一行：静默，不报错
	}
	e.Payload = json.RawMessage(payload)
	return true, nil
}

// getEventByIdemKey 按 (namespace_id, source, idem_key) 取已有事件。
//
// 幂等键的作用域是**命名空间**（唯一索引同口径）：同一个 idemKey 在 prod 与
// staging 各收一次是正常的，不该互相吞掉。
func (d *DB) getEventByIdemKey(ctx context.Context, namespaceID, source, idemKey string) (*Event, error) {
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events
		 WHERE namespace_id = ? AND source = ? AND idem_key = ?`, namespaceID, source, idemKey)
}

// scanEvent 执行查询并解析一行；未命中返回 ErrNotFound。
//
// 语句缓存（#242）：GetEvent / GetEventByTrace / getEventByIdemKey 共用本函数，
// 三者的 SQL 文本都是常量。
func (d *DB) scanEvent(ctx context.Context, query string, args ...any) (*Event, error) {
	var e Event
	var payload string
	err := d.cachedQueryRowContext(ctx, query, args...).Scan(
		&e.ID, &e.NamespaceID, &e.TraceID, &e.Source, &e.Type, &payload, &e.IdemKey, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	e.Payload = json.RawMessage(payload)
	return &e, nil
}

// GetEvent 按 (namespaceID, id) 取事件；未命中返回 ErrNotFound。
func (d *DB) GetEvent(ctx context.Context, namespaceID, id string) (*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events WHERE namespace_id = ? AND id = ?`, namespaceID, id)
}

// ListEvents 列出**命名空间内**事件，可按 traceID / source 过滤（"" = 不过滤）；空列表返回 []。
//
// 按 created_at DESC（最新在前）+ id DESC（同秒内稳定序），limit ≤ 0 时用默认 50。
//
// source 过滤用于"给这个源找一条真实样本"：参数编辑器拿最近一条事件的
// payload 当预览样本，用户就能看到 {{pull_request.title}} 到底会取到什么，
// 而不是盲写路径。source 是起点能力名（见 I2 入口边），与 sources 表无关。
//
// #142 剪枝事件（source='route'）的取舍：
//   - 按 trace 查（trace 视图）：**含**剪枝——「这条分支为什么没跑」就在这条 trace 里；
//   - 事件流（不带 traceId）：**不混**剪枝——否则一个扇出里被剪的边会把
//     「外部世界发生了什么」淹没成两倍长度；
//   - 显式给 source（包括 source=route）：听调用方的——它是主动来看某一类的。
//
// 过滤只在这里做一次：handler 再筛一遍就会出现「库里查得到、接口里没有」的第二种口径。
func (d *DB) ListEvents(ctx context.Context, namespaceID, traceID, source string, limit int) ([]*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
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
		q += ` AND source != 'route'`
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.cachedQueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
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

// GetEventByTrace 取某 trace 的根事件；未命中返回 ErrNotFound。
func (d *DB) GetEventByTrace(ctx context.Context, namespaceID, traceID string) (*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events
		 WHERE namespace_id = ? AND trace_id = ?
		 ORDER BY created_at, id LIMIT 1`, namespaceID, traceID)
}
