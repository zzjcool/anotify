package broker

import (
	"context"
	"testing"
	"time"

	"github.com/anotify/anotify/internal/store"
)

// newTestBroker 用 :memory: 打开一个测试 broker。
func newTestBroker(t *testing.T) *SQLiteBroker {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	b := NewSQLite(db)
	t.Cleanup(func() { b.Close() })
	return b
}

func msg(userID, title string, tags []string) *Message {
	return &Message{
		UserID:     userID,
		Title:      title,
		Status:     StatusSuccess,
		Body:       "body of " + title,
		DeviceTags: tags,
		TTLSeconds: 86400,
	}
}

// TestPublishReplaySeqMonotonic：Publish 后 Replay 能读回，且 seq 单调递增。
func TestPublishReplaySeqMonotonic(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	for i := 0; i < 3; i++ {
		if err := b.Publish(ctx, msg(uid, "m"+string(rune('a'+i)), nil)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	got, err := b.Replay(ctx, uid, 0, 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("replay 返回 %d 条，期望 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq 非单调递增: got[%d].Seq=%d got[%d].Seq=%d", i-1, got[i-1].Seq, i, got[i].Seq)
		}
	}
	// seq 应从 1 开始连续
	if got[0].Seq != 1 || got[2].Seq != 3 {
		t.Fatalf("seq 期望 1..3，实际 %d..%d", got[0].Seq, got[2].Seq)
	}

	// Replay with sinceSeq 只返回之后的
	got2, err := b.Replay(ctx, uid, 1, 100)
	if err != nil {
		t.Fatalf("replay sinceSeq=1: %v", err)
	}
	if len(got2) != 2 || got2[0].Seq != 2 {
		t.Fatalf("replay sinceSeq=1 期望 seq 2,3，实际 %v", seqs(got2))
	}

	// 不同用户 seq 各自独立
	if err := b.Publish(ctx, msg("usr_2", "other", nil)); err != nil {
		t.Fatalf("publish other user: %v", err)
	}
	got3, _ := b.Replay(ctx, "usr_2", 0, 100)
	if len(got3) != 1 || got3[0].Seq != 1 {
		t.Fatalf("其他用户 seq 应从 1 开始，实际 %v", seqs(got3))
	}
}

func seqs(ms []*Message) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Seq
	}
	return out
}

// TestSubscribeReceivesPublished：Subscribe 实时收到 Publish 的消息。
func TestSubscribeReceivesPublished(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	sub, err := b.Subscribe(ctx, uid, nil) // 全订阅
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	want := msg(uid, "hello", nil)
	if err := b.Publish(ctx, want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-sub.C():
		if got.ID != want.ID || got.Title != "hello" {
			t.Fatalf("收到消息不匹配: got %+v want id=%s", got, want.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时未收到订阅消息")
	}
}

// TestSubscribeTagFilter：tags 过滤正确。
func TestSubscribeTagFilter(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	cases := []struct {
		name     string
		subTags  []string
		msgTags  []string
		wantRecv bool
	}{
		{"订阅为空=全订阅,消息有tag", nil, []string{"ops"}, true},
		{"订阅为空=全订阅,消息无tag", nil, nil, true},
		{"订阅有tag,消息无tag=广播", []string{"ops"}, nil, true},
		{"交集≥1=投递", []string{"ops", "build"}, []string{"build"}, true},
		{"无交集=不投递", []string{"ops"}, []string{"home"}, false},
		{"多对多任一命中", []string{"a", "b"}, []string{"c", "b"}, true},
		{"多对多无命中", []string{"a", "b"}, []string{"c", "d"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := b.Subscribe(ctx, uid, tc.subTags)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer sub.Close()

			m := msg(uid, "x", tc.msgTags)
			if err := b.Publish(ctx, m); err != nil {
				t.Fatalf("publish: %v", err)
			}

			select {
			case <-sub.C():
				if !tc.wantRecv {
					t.Fatalf("不应收到却收到了: subTags=%v msgTags=%v", tc.subTags, tc.msgTags)
				}
			case <-time.After(300 * time.Millisecond):
				if tc.wantRecv {
					t.Fatalf("应收到却没收到: subTags=%v msgTags=%v", tc.subTags, tc.msgTags)
				}
			}
		})
	}
}

// TestAckHighWaterForwardOnly：Ack high-water 只前进。
func TestAckHighWaterForwardOnly(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"
	cid := "dev_1"

	if err := b.Ack(ctx, cid, uid, 5); err != nil {
		t.Fatalf("ack 5: %v", err)
	}
	// 较小的 seq 不应回退 high-water
	if err := b.Ack(ctx, cid, uid, 3); err != nil {
		t.Fatalf("ack 3: %v", err)
	}

	var lastSeq int64
	err := b.db.QueryRowContext(ctx,
		`SELECT last_seq FROM consumer_offsets WHERE consumer_id=? AND user_id=?`, cid, uid).
		Scan(&lastSeq)
	if err != nil {
		t.Fatalf("查询 consumer_offsets: %v", err)
	}
	if lastSeq != 5 {
		t.Fatalf("high-water 应为 5（只前进），实际 %d", lastSeq)
	}

	// 更大的 seq 应前进
	if err := b.Ack(ctx, cid, uid, 9); err != nil {
		t.Fatalf("ack 9: %v", err)
	}
	_ = b.db.QueryRowContext(ctx,
		`SELECT last_seq FROM consumer_offsets WHERE consumer_id=? AND user_id=?`, cid, uid).
		Scan(&lastSeq)
	if lastSeq != 9 {
		t.Fatalf("high-water 应前进到 9，实际 %d", lastSeq)
	}
}

// TestDeleteExpired：过期清理删除旧消息（deliveries 级联删）。
func TestDeleteExpired(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	// 一条旧消息（created_at 在 100 天前）
	old := msg(uid, "old", nil)
	old.CreatedAt = time.Now().Add(-100 * 24 * time.Hour)
	if err := b.Publish(ctx, old); err != nil {
		t.Fatalf("publish old: %v", err)
	}
	// 给旧消息配一条 delivery，验证级联删除
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO deliveries (message_id, consumer_id, channel, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?)`, old.ID, "dev_1", "webpush", "sent", time.Now().Unix(), time.Now().Unix())
	if err != nil {
		t.Fatalf("插入 delivery: %v", err)
	}

	// 一条新消息
	if err := b.Publish(ctx, msg(uid, "new", nil)); err != nil {
		t.Fatalf("publish new: %v", err)
	}

	// 保留 90 天 → 旧消息（100 天前）应被删
	n, err := b.DeleteExpired(ctx, 90*24*3600)
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 条，实际 %d", n)
	}

	got, err := b.Replay(ctx, uid, 0, 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("剩余应为 new，实际 %v", titles(got))
	}

	// deliveries 应级联删除
	var cnt int
	_ = b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE message_id=?`, old.ID).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("旧消息的 deliveries 应级联删除，实际剩 %d", cnt)
	}
}

func titles(ms []*Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Title
	}
	return out
}

// TestPublishRoundTripFields：字段（含 payload/device_tags/expires_at）完整往返。
func TestPublishRoundTripFields(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	m := msg(uid, "round", []string{"ops", "手机"})
	m.Status = StatusError
	m.Link = "pi://session/abc"
	m.Payload = []byte(`{"agentId":"a1","sessionId":"s1"}`)
	m.TTLSeconds = 3600
	if err := b.Publish(ctx, m); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// ExpiresAt 应 = CreatedAt + ttl
	if got := m.ExpiresAt.Unix() - m.CreatedAt.Unix(); got != 3600 {
		t.Fatalf("expires_at - created_at 应为 3600，实际 %d", got)
	}

	got, err := b.Replay(ctx, uid, 0, 1)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(got))
	}
	g := got[0]
	if g.Status != StatusError || g.Link != "pi://session/abc" {
		t.Fatalf("status/link 往返错误: %+v", g)
	}
	if string(g.Payload) != `{"agentId":"a1","sessionId":"s1"}` {
		t.Fatalf("payload 往返错误: %s", g.Payload)
	}
	if len(g.DeviceTags) != 2 || g.DeviceTags[0] != "ops" || g.DeviceTags[1] != "手机" {
		t.Fatalf("device_tags 往返错误: %v", g.DeviceTags)
	}
}

// TestSubscribeCloseStopsDelivery：Close 后不再收到消息。
func TestSubscribeCloseStopsDelivery(t *testing.T) {
	b := newTestBroker(t)
	ctx := context.Background()
	uid := "usr_1"

	sub, err := b.Subscribe(ctx, uid, nil)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub.Close()

	if err := b.Publish(ctx, msg(uid, "after-close", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-sub.C():
		t.Fatal("Close 后不应再收到消息")
	case <-time.After(200 * time.Millisecond):
		// 符合预期
	}
}
