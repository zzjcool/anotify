package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 事件（events 表）：只存不解释的原始输入，是链的根。
//
// 幂等键是 **deliveryId**（外部投递 id，source.md §3.3 / D7）：
// 同一 (namespace, **源稳定 id**, deliveryId) 在**保留窗口内**只算一条事件、不重复派发；
// 窗口过期后同一个 id 可以再次被接收（当新事件）。窗口长度就是 events 的保留期
// （system_config.retention_event_ttl_sec，0 = 永久 → 去重窗口也是永久）。
//
// 去重键的第二段必须是**源的稳定 id**（sources.id），不是源名：源可以删掉后用同一个
// 名字重建，那是**另一个源**。拿名字当键会把「旧源的投递窗」与「新源的同 id 投递」
// 合成一条，新源的第一条事件被静默吞掉。事件行上的 source 仍是人类可读的源名
// （列表/详情要给人看），两者用途不同、不能混用。
//
// 没有 deliveryId 的投递每次都是新事件：不是所有来源都有稳定投递 id，硬造一个
// 只会制造假幂等（把两次真的不同的事件吞掉一次）。

// 事件 reason 的稳定机器码（event.reason 列）。
const (
	// EventReasonIngested 是正常接收。
	EventReasonIngested = "Ingested"
	// EventReasonPruned 是「事件已接收、下游没建 task」的剪枝留痕行。
	EventReasonPruned = "Pruned"
)

// EventPruneSource 是剪枝留痕的保留来源名。
//
// `source='route'` 的行**不是外部事件**：它们是中枢自己的观察记录。事件流
// （不带 traceId）过滤掉它们，否则一次扇出里被剪的边会把「外部世界发生了什么」
// 淹没成两倍长度；按 trace 查时保留（「这条分支为什么没跑」就在这条 trace 里）。
const EventPruneSource = "route"

// ErrReservedEventSource 表示有人把保留来源名当普通来源用。
//
// 业务事件若用了 `source='route'`，会被事件流过滤掉（**静默消失**）、并在 trace
// 视图里被当成剪枝。这种丢数据的失败必须当场报错。
var ErrReservedEventSource = sourcespec.ErrReservedName

// Event 是 events 表的一行。
type Event struct {
	ID          string          `json:"id"`
	NamespaceID string          `json:"namespaceId"`
	TraceID     string          `json:"traceId"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	// DeliveryID 是外部投递幂等键（可选）。同一源窗口内重复投递 → 返回已有行。
	DeliveryID *string `json:"deliveryId"`
	Reason     string  `json:"reason"`
	CreatedAt  int64   `json:"createdAt"`
}

// eventColumns 是统一的列顺序，scanEvent 依赖它。
const eventColumns = `id, namespace_id, trace_id, source, type, payload, delivery_id, reason, created_at`

// InsertEvent 写入一条外部事件（带**投递去重窗**的幂等）。
//
// 判据走独立的 event_deliveries 表（source.md §3.3 D7/D12）：
//
//	窗口内（expires_at > now，或 expires_at = 0 永久） → 回已有事件，inserted=false
//	窗口已过                                        → **当成新事件**：新 eventId 落库，
//	                                                  去重记录换成新 eventId/acceptedAt/expiresAt
//	没有 deliveryId                                 → 每次插入新行（不比较、不去重）
//
// 为什么不靠 sweeper：窗口判定必须在**接收事务里**按 expires_at 现算。靠后台清理
// 会让「窗口刚过、sweeper 还没跑」这段窗口里的投递被错误地吸收掉——而 sweeper 可能
// 一小时才跑一次。
//
// 也不依赖 events 上的唯一索引：旧事件可能因为被 task 引用而**活过**去重窗
// （见 DeleteExpiredEvents 的引用保护），此时同 id 的新投递必须仍然能收进来。
// 去重是「窗口」，不是「永久的投递 id 唯一性」。
func (d *DB) InsertEvent(ctx context.Context, e *Event, now int64, windowSec int64) (inserted bool, err error) {
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
	if e.Source == EventPruneSource {
		return false, ErrReservedEventSource
	}
	if now == 0 {
		now = Now()
	}
	payload := string(e.Payload)
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		payload = "{}"
	}
	if e.Reason == "" {
		e.Reason = EventReasonIngested
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	if e.ID == "" {
		e.ID = NewEventID()
	}

	// 无幂等键：直接插（并发也不会冲突——去重表只对带 id 的投递生效）。
	if e.DeliveryID == nil || *e.DeliveryID == "" {
		e.DeliveryID = nil
		if err := d.insertEventRow(ctx, e, payload); err != nil {
			return false, err
		}
		return true, nil
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("insert event: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// 源解析与窗口占用必须在**同一个事务**里：否则「解析出旧源 id → 源被删/同名重建
	// → 占窗」之间会用一个已经不成立的源身份去占用一条新源的投递窗口。
	sourceKey, err := resolveDeliverySourceKeyOn(ctx, ex, e.NamespaceID, e.Source)
	if err != nil {
		return false, err
	}
	hit, err := claimDeliveryOn(ctx, ex, e, sourceKey, now, windowSec)
	if err != nil {
		return false, err
	}
	if hit != nil {
		*e = *hit
		return false, nil
	}
	if err := insertEventRowOn(ctx, ex, e, payload); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("insert event: commit: %w", err)
	}
	return true, nil
}

// deliverySourceKeyPrefix 是「源行不存在」时的临时去重键前缀。
//
// 源 id 一律是 `src_...`（base62，无冒号），所以 `name:<源名>` 与任何真实源 id
// 都不相交：临时键只会在「这条投递来自一个当前查不到的源」时出现，
// 将来同名源建出来（拿到 `src_...` id）也不会与它混窗。
const deliverySourceKeyPrefix = "name:"

// resolveDeliverySourceKeyOn 在同一事务里把「事件上的源名」解析成**源的稳定 id**。
//
// 解析不到源行（外部入口在源已删除后的竞态、或内部直写事件）时退回带前缀的名字键：
// 那种投递没有稳定身份可钉，用名字当临时身份至少保证「同一次投递在窗口内只收一次」，
// 而前缀保证它不会冒充任何真实源。
func resolveDeliverySourceKeyOn(ctx context.Context, ex queryExec, namespaceID, sourceName string) (string, error) {
	var id string
	err := ex.queryRowContext(ctx,
		`SELECT id FROM sources WHERE namespace_id = ? AND name = ?`, namespaceID, sourceName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return deliverySourceKeyPrefix + sourceName, nil
	}
	if err != nil {
		return "", fmt.Errorf("insert event: resolve source id: %w", err)
	}
	return id, nil
}

// claimDeliveryOn 在事务内「占用」这次投递的窗口（sourceKey = 源稳定 id）：
//
//	返回 (nil, nil)      → 窗口可写，调用方插入**新事件行**
//	返回 (已有事件, nil) → 窗口内的重复投递，调用方原样回给发送方
//
// 实现是一条**条件 upsert**：只有旧记录已过期（或还没有记录）才覆盖它。
// 覆盖失败（RowsAffected==0）就说明窗口内的记录还在——那条记录指向的事件就是答案。
// 两个并发投递在 SQLite 的 BEGIN IMMEDIATE 下天然串行，而这里的语义在
// 「任一时刻只有一个写者」之外也成立（第二次会看到第一次的提交并命中窗口）。
func claimDeliveryOn(ctx context.Context, ex queryExec, e *Event, sourceKey string, now int64, windowSec int64) (*Event, error) {
	expiresAt := int64(0)
	if windowSec > 0 {
		expiresAt = now + windowSec
	}
	res, err := ex.execContext(ctx,
		`INSERT INTO event_deliveries (namespace_id, source, delivery_id, event_id, accepted_at, expires_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(namespace_id, source, delivery_id) DO UPDATE SET
		      event_id    = excluded.event_id,
		      accepted_at = excluded.accepted_at,
		      expires_at  = excluded.expires_at
		  WHERE event_deliveries.expires_at > 0 AND event_deliveries.expires_at <= ?`,
		e.NamespaceID, sourceKey, *e.DeliveryID, e.ID, now, expiresAt, now)
	if err != nil {
		return nil, fmt.Errorf("insert event: claim delivery window: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("insert event rows: %w", err)
	}
	if n > 0 {
		return nil, nil // 占到了窗口（新事件或过期后的再次接收）
	}
	// 窗口内命中的重复投递：回它指向的那个事件。
	var eventID string
	if err := ex.queryRowContext(ctx,
		`SELECT event_id FROM event_deliveries
		  WHERE namespace_id = ? AND source = ? AND delivery_id = ?`,
		e.NamespaceID, sourceKey, *e.DeliveryID).Scan(&eventID); err != nil {
		return nil, fmt.Errorf("insert event: read delivery record: %w", err)
	}
	hit, err := getEventByIDOn(ctx, ex, e.NamespaceID, eventID)
	if err == nil {
		return hit, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// 记录指向的事件行已经不在了（事件被清理但窗口未尽）：这条投递不能被
	// 「吸收到一个不存在的事件」上，直接按新事件接收并刷新记录。
	if _, err := ex.execContext(ctx,
		`DELETE FROM event_deliveries WHERE namespace_id = ? AND source = ? AND delivery_id = ?`,
		e.NamespaceID, sourceKey, *e.DeliveryID); err != nil {
		return nil, fmt.Errorf("insert event: drop dangling delivery record: %w", err)
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO event_deliveries (namespace_id, source, delivery_id, event_id, accepted_at, expires_at)
		 VALUES (?,?,?,?,?,?)`,
		e.NamespaceID, sourceKey, *e.DeliveryID, e.ID, now, expiresAt); err != nil {
		return nil, fmt.Errorf("insert event: reclaim delivery window: %w", err)
	}
	return nil, nil
}

// InsertPruneEvent 写一条剪枝留痕（source='route'、reason=Pruned、type=剪枝码）。
//
// **永远留痕**：剪枝是「事件收到了但没派出去」的唯一证据，去重会把同一 trace 里
// 多条相同的剪枝压成一条，反而掩盖了扇出规模。所以这里不做幂等（也不带 deliveryId）。
func (d *DB) InsertPruneEvent(ctx context.Context, e *Event) error {
	return insertPruneFactEventOn(ctx, d, e)
}

// insertPruneFactEventOn 是剪枝/失败观察事件的唯一边写实现（*DB 与 tx 共用）。
//
// 它跟 InsertPruneEvent 是同一份语义：**永远留痕**（不做投递幂等、不带 deliveryId），
// source 固定为中枢观察来源 `route`，reason 固定 `Pruned`，type 是闭集剪枝码。
// 失败 task 的 downstream_missing 观察也走这里（type=downstream_missing）。
func insertPruneFactEventOn(ctx context.Context, ex queryExec, e *Event) error {
	if e == nil {
		return fmt.Errorf("insert prune event: nil")
	}
	if e.NamespaceID == "" || e.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if e.TraceID == "" {
		return fmt.Errorf("insert prune event: empty trace_id")
	}
	if e.Type == "" {
		return fmt.Errorf("insert prune event: empty type (剪枝码)")
	}
	if !protocol.IsPruneReason(e.Type) && e.Type != protocol.ReasonDownstreamMissing {
		return fmt.Errorf("insert prune event: %q 不是剪枝原因码（允许：%v）", e.Type, protocol.PruneReasons())
	}
	payload := string(e.Payload)
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		payload = "{}"
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if e.ID == "" {
		e.ID = NewEventID()
	}
	e.Source = EventPruneSource
	e.Reason = EventReasonPruned
	e.DeliveryID = nil
	return insertEventRowOn(ctx, ex, e, payload)
}

// insertEventRow 在连接池上插一行（内部路径；事务内走 insertEventRowOn）。
func (d *DB) insertEventRow(ctx context.Context, e *Event, payload string) error {
	return insertEventRowOn(ctx, d, e, payload)
}

// insertEventRowOn 插一行事件（ex 可以是 *DB 或 txExec）。
func insertEventRowOn(ctx context.Context, ex queryExec, e *Event, payload string) error {
	var deliveryID any
	if e.DeliveryID != nil && *e.DeliveryID != "" {
		deliveryID = *e.DeliveryID
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, delivery_id, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.NamespaceID, e.TraceID, e.Source, e.Type, payload, deliveryID, e.Reason, e.CreatedAt); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	e.Payload = json.RawMessage(payload)
	return nil
}

// insertIngestEventOn 插一行**接收事务**事件，并把当时回执作为内部持久事实写入。
//
// receipt 是 event/trace/task ids + pruned facts 的快照：窗口内重复投递只能回这份
// 持久字节（不能第二次查询当前可变资源重算，那会给出与第一次不同的答案）。
func insertIngestEventOn(ctx context.Context, ex queryExec, e *Event, payload string, receipt []byte) error {
	if len(receipt) == 0 || !json.Valid(receipt) {
		return fmt.Errorf("insert event: invalid receipt")
	}
	var deliveryID any
	if e.DeliveryID != nil && *e.DeliveryID != "" {
		deliveryID = *e.DeliveryID
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, delivery_id, reason, receipt, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.NamespaceID, e.TraceID, e.Source, e.Type, payload, deliveryID, e.Reason,
		string(receipt), e.CreatedAt); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	e.Payload = json.RawMessage(payload)
	return nil
}

// readEventReceiptOn 读一条事件行上持久化的接收回执（事务内）。
//
// 空列 / `{}` = 这行不是接收事务写的（没有 receipt 事实）→ ErrSourceEventReceiptMissing：
// 宁可明确报错也不凭空造一份「看起来像第一次」的回执。
func readEventReceiptOn(ctx context.Context, ex queryExec, namespaceID, eventID string) (json.RawMessage, error) {
	var raw string
	err := ex.queryRowContext(ctx,
		`SELECT receipt FROM events WHERE namespace_id = ? AND id = ?`, namespaceID, eventID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceEventReceiptMissing
	}
	if err != nil {
		return nil, fmt.Errorf("read event receipt: %w", err)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" || !json.Valid([]byte(trimmed)) {
		return nil, ErrSourceEventReceiptMissing
	}
	return json.RawMessage(trimmed), nil
}

// getEventByIDOn 按 (namespace, id) 取事件（事务内读）；未命中 ErrNotFound。
func getEventByIDOn(ctx context.Context, ex queryExec, namespaceID, id string) (*Event, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	if err != nil {
		return nil, fmt.Errorf("read event by id: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read event by id: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanEventRow(rows)
}

// scanEventRow 从一行结果解析 Event。
func scanEventRow(rows *sql.Rows) (*Event, error) {
	var (
		e          Event
		payload    string
		deliveryID sql.NullString
	)
	if err := rows.Scan(&e.ID, &e.NamespaceID, &e.TraceID, &e.Source, &e.Type,
		&payload, &deliveryID, &e.Reason, &e.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	if deliveryID.Valid {
		v := deliveryID.String
		e.DeliveryID = &v
	}
	e.Payload = json.RawMessage(payload)
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	return &e, nil
}

// GetEvent 按 (namespaceID, id) 取事件；未命中返回 ErrNotFound。
func (d *DB) GetEvent(ctx context.Context, namespaceID, id string) (*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE namespace_id = ? AND id = ?`, namespaceID, id)
	if err != nil {
		return nil, fmt.Errorf("get event: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get event: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanEventRow(rows)
}

// GetEventByTrace 取某 trace 的第一条事件（真实事件优先于剪枝留痕）；未命中 ErrNotFound。
func (d *DB) GetEventByTrace(ctx context.Context, namespaceID, traceID string) (*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+eventColumns+` FROM events
		  WHERE namespace_id = ? AND trace_id = ?
		  ORDER BY CASE WHEN source = ? THEN 1 ELSE 0 END, created_at, id
		  LIMIT 1`, namespaceID, traceID, EventPruneSource)
	if err != nil {
		return nil, fmt.Errorf("get event by trace: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get event by trace: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanEventRow(rows)
}

// ListEvents 列出命名空间内事件（最新在前），可按 traceID / source 过滤；空列表返回 []。
//
// source 过滤的取舍（api-conventions §7.7）：
//   - 按 trace 查：**含**剪枝留痕（那条 trace 的「为什么没跑」就在里面）；
//   - 事件流（无 traceId、无 source）：**不含**剪枝（source='route'）；
//   - 显式给 source（包括 route）：听调用方的。
func (d *DB) ListEvents(ctx context.Context, namespaceID, traceID, source string, limit int) ([]*Event, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q, args := eventListQuery(namespaceID, traceID, source, limit)
	rows, err := d.cachedQueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
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

// eventListQuery 拼 ListEvents 的 SQL（分页版 event_page.go 必须与它逐字等价，
// 差异只允许是游标谓词）。
func eventListQuery(namespaceID, traceID, source string, limit int) (string, []any) {
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
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return q, args
}
