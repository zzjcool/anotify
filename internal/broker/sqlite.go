package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// SQLiteBroker 是 Broker 的 SQLite 实现。
//
// Phase 1 单进程：进程内广播负责实时（快路径），DB 表负责回放/历史/崩溃恢复（稳路径）。
// seq 在事务内生成，保证每用户单调递增；进程内广播在事务提交后进行。
type SQLiteBroker struct {
	db *store.DB

	mu   sync.RWMutex
	subs map[string]map[*sqliteSub]struct{} // userID → 活跃订阅集合

	closed chan struct{}
	o      sync.Once
}

// NewSQLite 用已打开的 store.DB 构造一个 SQLiteBroker。
func NewSQLite(db *store.DB) *SQLiteBroker {
	return &SQLiteBroker{
		db:     db,
		subs:   make(map[string]map[*sqliteSub]struct{}),
		closed: make(chan struct{}),
	}
}

// 编译期接口断言。
var _ Broker = (*SQLiteBroker)(nil)

// Publish 实现 Broker.Publish。
//
// 事务内生成 seq 并插入 messages；提交后向该用户的活跃订阅做进程内广播。
func (b *SQLiteBroker) Publish(ctx context.Context, msg *Message) error {
	if msg.UserID == "" {
		return fmt.Errorf("publish: user_id 不能为空")
	}
	if msg.ID == "" {
		msg.ID = store.NewMessageID()
	}
	if msg.Title == "" {
		return fmt.Errorf("publish: title 不能为空")
	}
	if msg.AgentState == "" {
		msg.AgentState = AgentStateWorking
	}
	if msg.Priority == "" {
		msg.Priority = "normal"
	}
	if msg.TTLSeconds <= 0 {
		msg.TTLSeconds = 86400
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	msg.ExpiresAt = msg.CreatedAt.Add(time.Duration(msg.TTLSeconds) * time.Second)

	tagsJSON, err := json.Marshal(msg.DeviceTags)
	if err != nil {
		return fmt.Errorf("publish: 序列化 device_tags: %w", err)
	}
	payload := msg.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("publish: 开启事务: %w", err)
	}
	defer tx.Rollback() // 提交后调用是 no-op

	// 事务内生成 seq（每用户单调递增）。
	var seq int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE user_id=?`, msg.UserID).
		Scan(&seq)
	if err != nil {
		return fmt.Errorf("publish: 生成 seq: %w", err)
	}
	msg.Seq = seq

	_, err = tx.ExecContext(ctx, `
		INSERT INTO messages
		  (id, user_id, seq, title, agent_state, severity, kind, reply_to, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.UserID, msg.Seq, msg.Title, msg.AgentState, msg.Severity, msg.Kind, msg.ReplyTo, msg.Body, msg.Link,
		string(tagsJSON), msg.Priority, msg.TTLSeconds, string(payload),
		msg.CreatedAt.Unix(), msg.ExpiresAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("publish: 插入 messages: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("publish: 提交事务: %w", err)
	}

	// 提交成功后做进程内广播（快路径）。
	b.broadcast(msg.UserID, msg)
	return nil
}

// Subscribe 实现 Broker.Subscribe。
func (b *SQLiteBroker) Subscribe(ctx context.Context, userID string, tags []string) (Subscription, error) {
	if userID == "" {
		return nil, fmt.Errorf("subscribe: user_id 不能为空")
	}
	select {
	case <-b.closed:
		return nil, fmt.Errorf("subscribe: broker 已关闭")
	default:
	}

	sub := &sqliteSub{
		broker: b,
		userID: userID,
		tags:   tags,
		ch:     make(chan *Message, 64), // 带缓冲，慢消费不阻塞发布者
		done:   make(chan struct{}),
	}

	b.mu.Lock()
	if b.subs[userID] == nil {
		b.subs[userID] = make(map[*sqliteSub]struct{})
	}
	b.subs[userID][sub] = struct{}{}
	b.mu.Unlock()

	// 上下文取消时自动退订。
	go func() {
		select {
		case <-ctx.Done():
			sub.Close()
		case <-sub.done:
		}
	}()

	return sub, nil
}

// Ack 实现 Broker.Ack（high-water 只前进）。
func (b *SQLiteBroker) Ack(ctx context.Context, consumerID, userID string, seq int64) error {
	if consumerID == "" || userID == "" {
		return fmt.Errorf("ack: consumer_id/user_id 不能为空")
	}
	now := time.Now().Unix()
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO consumer_offsets (consumer_id, user_id, last_seq, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(consumer_id, user_id)
		DO UPDATE SET last_seq=MAX(last_seq, excluded.last_seq), updated_at=excluded.updated_at`,
		consumerID, userID, seq, now,
	)
	if err != nil {
		return fmt.Errorf("ack: 更新 consumer_offsets: %w", err)
	}
	return nil
}

// Replay 实现 Broker.Replay：返回 sinceSeq 之后的消息（按 seq 升序）。
func (b *SQLiteBroker) Replay(ctx context.Context, userID string, sinceSeq int64, limit int) ([]*Message, error) {
	if userID == "" {
		return nil, fmt.Errorf("replay: user_id 不能为空")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, user_id, seq, title, agent_state, severity, kind, reply_to, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
		FROM messages
		WHERE user_id=? AND seq>?
		ORDER BY seq ASC LIMIT ?`, userID, sinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("replay: 查询 messages: %w", err)
	}
	defer rows.Close()

	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replay: 遍历结果: %w", err)
	}
	return out, nil
}

// DeleteExpired 删除 created_at 早于 now-retentionSeconds 的历史消息（deliveries 级联删除）。
// 返回删除的行数。注意：这是历史保留清理，与"可投递性"（expires_at）是两回事。
func (b *SQLiteBroker) DeleteExpired(ctx context.Context, retentionSeconds int64) (int64, error) {
	if retentionSeconds <= 0 {
		retentionSeconds = 90 * 24 * 3600 // 默认 90 天
	}
	cutoff := time.Now().Unix() - retentionSeconds
	res, err := b.db.ExecContext(ctx, `DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired: 读取影响行数: %w", err)
	}
	return n, nil
}

// Close 实现 Broker.Close：关闭所有订阅并释放资源（幂等）。
func (b *SQLiteBroker) Close() error {
	b.o.Do(func() {
		close(b.closed)
		b.mu.Lock()
		for userID, set := range b.subs {
			for sub := range set {
				sub.closeOnce()
			}
			delete(b.subs, userID)
		}
		b.mu.Unlock()
	})
	return b.db.Close()
}

// broadcast 把消息投递给该用户所有匹配的活跃订阅（非阻塞）。
func (b *SQLiteBroker) broadcast(userID string, msg *Message) {
	b.mu.RLock()
	set := b.subs[userID]
	subs := make([]*sqliteSub, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	b.mu.RUnlock()

	for _, s := range subs {
		if !s.matchTags(msg.DeviceTags) {
			continue
		}
		// 非阻塞投递：channel 满了就跳过实时推送（消费方可走 Replay 补漏）。
		select {
		case s.ch <- msg:
		default:
		}
	}
}

// unsubscribe 从映射中移除订阅。
func (b *SQLiteBroker) unsubscribe(s *sqliteSub) {
	b.mu.Lock()
	if set, ok := b.subs[s.userID]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(b.subs, s.userID)
		}
	}
	b.mu.Unlock()
}

// sqliteSub 是 Subscription 的 SQLite 实现。
type sqliteSub struct {
	broker *SQLiteBroker
	userID string
	tags   []string
	ch     chan *Message
	done   chan struct{}
	once   sync.Once
}

var _ Subscription = (*sqliteSub)(nil)

func (s *sqliteSub) C() <-chan *Message { return s.ch }

func (s *sqliteSub) Close() error {
	s.closeOnce()
	return nil
}

func (s *sqliteSub) closeOnce() {
	s.once.Do(func() {
		close(s.done)
		s.broker.unsubscribe(s)
		// 注意：不关闭 ch，避免 broadcast 在持有读锁时向已关闭 channel 发送导致 panic。
		// channel 缓冲会被 GC 回收。
	})
}

// matchTags 实现标签路由：
//   - 订阅 tags 为空 = 全订阅（catch-all，接收一切）
//   - 消息 DeviceTags 为空 = 广播（所有订阅都收）
//   - 双方都有 = 求交集，≥1 个共同 tag 才投递（ANY）
func (s *sqliteSub) matchTags(msgTags []string) bool {
	if len(s.tags) == 0 || len(msgTags) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(s.tags))
	for _, t := range s.tags {
		set[t] = struct{}{}
	}
	for _, t := range msgTags {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

// scanMessage 从一行扫描出 Message。
func scanMessage(rows *sql.Rows) (*Message, error) {
	var m Message
	var tags string
	var payload string
	var createdAt, expiresAt int64
	err := rows.Scan(
		&m.ID, &m.UserID, &m.Seq, &m.Title, &m.AgentState, &m.Severity, &m.Kind, &m.ReplyTo, &m.Body, &m.Link,
		&tags, &m.Priority, &m.TTLSeconds, &payload, &createdAt, &expiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &m.DeviceTags); err != nil {
		return nil, fmt.Errorf("scan message: 解析 device_tags: %w", err)
	}
	m.Payload = []byte(payload)
	m.CreatedAt = time.Unix(createdAt, 0)
	m.ExpiresAt = time.Unix(expiresAt, 0)
	return &m, nil
}
