package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anotify/anotify/internal/authn"
	"github.com/anotify/anotify/internal/broker"
)

// TestMatchTags 覆盖 WS 订阅过滤。
func TestMatchTags(t *testing.T) {
	cases := []struct {
		name    string
		sub     []string
		msgTags []string
		want    bool
	}{
		{"空订阅=全订阅，带tag消息", nil, []string{"ops"}, true},
		{"空订阅=全订阅，广播消息", nil, nil, true},
		{"订阅命中", []string{"ops"}, []string{"ops"}, true},
		{"订阅未命中", []string{"ops"}, []string{"builds"}, false},
		{"订阅者收到广播消息", []string{"ops"}, nil, true},
		{"多订阅任一命中", []string{"ops", "builds"}, []string{"builds"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchTags(c.sub, c.msgTags); got != c.want {
				t.Fatalf("matchTags(%v,%v)=%v, want %v", c.sub, c.msgTags, got, c.want)
			}
		})
	}
}

// TestParseResumeSeq 解析 resume token / Last-Event-Id。
func TestParseResumeSeq(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"evt_1042", 1042},
		{"1042", 1042},
		{"evt_abc", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseResumeSeq(c.in); got != c.want {
			t.Fatalf("parseResumeSeq(%q)=%d, want %d", c.in, got, c.want)
		}
	}
}

// memBroker 是一个线程安全的内存 Broker，用于 WS 端到端测试。
type memBroker struct {
	mu    sync.Mutex
	subs  []chan *broker.Message
	queue []*broker.Message
	seq   int64
}

func (b *memBroker) Publish(ctx context.Context, m *broker.Message) error {
	b.mu.Lock()
	b.seq++
	m.Seq = b.seq
	b.queue = append(b.queue, m)
	subs := b.subs
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- m:
		default:
		}
	}
	return nil
}

func (b *memBroker) Subscribe(ctx context.Context, userID string, tags []string) (broker.Subscription, error) {
	ch := make(chan *broker.Message, 16)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return &memSub{ch: ch}, nil
}

func (b *memBroker) Ack(ctx context.Context, consumerID, userID string, seq int64) error {
	return nil
}

func (b *memBroker) Replay(ctx context.Context, userID string, sinceSeq int64, limit int) ([]*broker.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*broker.Message
	for _, m := range b.queue {
		if m.Seq > sinceSeq {
			out = append(out, m)
		}
	}
	return out, nil
}

func (b *memBroker) Close() error { return nil }

type memSub struct{ ch chan *broker.Message }

func (s *memSub) C() <-chan *broker.Message { return s.ch }
func (s *memSub) Close() error              { return nil }

func recvKeyValidator(userID string) authn.KeyValidator {
	return authn.KeyValidatorFunc(func(ctx context.Context, key string) (string, []string, error) {
		if key == "ant_recv_good" {
			return userID, []string{authn.ScopeNotifyReceive}, nil
		}
		return "", nil, http.ErrNoCookie // 任意错误
	})
}

// wsURL 把 http URL 转 ws URL。
func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

// readFrame 读一帧并解码。
func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) *Frame {
	t.Helper()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return &f
}

// TestStreamHandshakeAndLive 端到端：hello → 实时 notification → ping/pong。
func TestStreamHandshakeAndLive(t *testing.T) {
	b := &memBroker{}
	h := NewHandler(b, recvKeyValidator("usr_1"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer ant_recv_good")
	c, _, err := websocket.Dial(ctx, wsURL(srv)+"/v1/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// 1) 应收到 hello
	hello := readFrame(t, ctx, c)
	if hello.Type != FrameHello {
		t.Fatalf("first frame=%s, want hello", hello.Type)
	}
	if hello.Protocol != 1 || hello.HeartbeatSec <= 0 {
		t.Fatalf("bad hello: %+v", hello)
	}

	// 2) 发布一条消息 → 应收到 notification
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.Publish(context.Background(), &broker.Message{
			ID: "ntf_live", UserID: "usr_1", Title: "构建完成",
			Status: broker.StatusSuccess, TTLSeconds: 60,
		})
	}()
	n := readFrame(t, ctx, c)
	if n.Type != FrameNotification || n.EventID != "ntf_live" {
		t.Fatalf("frame=%+v, want notification ntf_live", n)
	}

	// 3) ping → pong
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	p := readFrame(t, ctx, c)
	if p.Type != FramePong {
		t.Fatalf("frame=%s, want pong", p.Type)
	}
}

// TestStreamReplay 断线续传：带 Last-Event-Id 连接 → 先补漏 → replay_end。
func TestStreamReplay(t *testing.T) {
	b := &memBroker{}
	// 预置 3 条历史
	for _, id := range []string{"ntf_1", "ntf_2", "ntf_3"} {
		b.Publish(context.Background(), &broker.Message{
			ID: id, UserID: "usr_1", Title: id, Status: broker.StatusInfo,
		})
	}

	h := NewHandler(b, recvKeyValidator("usr_1"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer ant_recv_good")
	hdr.Set("Last-Event-Id", "evt_1") // 从 seq=1 之后补
	c, _, err := websocket.Dial(ctx, wsURL(srv)+"/v1/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if f := readFrame(t, ctx, c); f.Type != FrameHello {
		t.Fatalf("frame=%s, want hello", f.Type)
	}
	// 应补 seq>1 的两条：ntf_2, ntf_3
	f1 := readFrame(t, ctx, c)
	if f1.Type != FrameNotification || f1.EventID != "ntf_2" {
		t.Fatalf("replay frame=%+v, want ntf_2", f1)
	}
	f2 := readFrame(t, ctx, c)
	if f2.Type != FrameNotification || f2.EventID != "ntf_3" {
		t.Fatalf("replay frame=%+v, want ntf_3", f2)
	}
	// 然后 replay_end
	f3 := readFrame(t, ctx, c)
	if f3.Type != FrameReplayEnd {
		t.Fatalf("frame=%s, want replay_end", f3.Type)
	}
}

// TestStreamUnauthorized 无效 Key → 握手失败（非 101）。
func TestStreamUnauthorized(t *testing.T) {
	b := &memBroker{}
	h := NewHandler(b, recvKeyValidator("usr_1"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer ant_recv_bad")
	_, resp, err := websocket.Dial(ctx, wsURL(srv)+"/v1/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		t.Fatal("dial should fail with bad key")
	}
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("should not upgrade with bad key")
	}
}

// TestStreamSubscribeFilter 订阅标签后只收命中消息。
func TestStreamSubscribeFilter(t *testing.T) {
	b := &memBroker{}
	h := NewHandler(b, recvKeyValidator("usr_1"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer ant_recv_good")
	c, _, err := websocket.Dial(ctx, wsURL(srv)+"/v1/stream", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if f := readFrame(t, ctx, c); f.Type != FrameHello {
		t.Fatalf("want hello, got %s", f.Type)
	}

	// 订阅 ops 标签
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","tags":["ops"]}`)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub := readFrame(t, ctx, c)
	if sub.Type != FrameSubscribed {
		t.Fatalf("frame=%s, want subscribed", sub.Type)
	}

	// 发布 builds 消息（不命中）和 ops 消息（命中）
	b.Publish(context.Background(), &broker.Message{ID: "ntf_builds", UserID: "usr_1", Title: "b", Status: "info", DeviceTags: []string{"builds"}})
	time.Sleep(20 * time.Millisecond)
	b.Publish(context.Background(), &broker.Message{ID: "ntf_ops", UserID: "usr_1", Title: "o", Status: "info", DeviceTags: []string{"ops"}})

	// 下一条应是 ops（builds 被过滤）
	n := readFrame(t, ctx, c)
	if n.Type != FrameNotification || n.EventID != "ntf_ops" {
		t.Fatalf("frame=%+v, want ntf_ops (builds filtered)", n)
	}
}
