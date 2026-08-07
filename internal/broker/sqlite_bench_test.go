package broker

import (
	"context"
	"testing"
)

// BenchmarkPublish 度量单条消息入队的成本（事务 + seq 生成 + 进程内广播）。
// 这是 Agent 上报路径的核心热点（每条通知一次 Publish）。
func BenchmarkPublish(b *testing.B) {
	bk := newTestBroker(b)
	ctx := context.Background()
	m := msg("usr_bench", "构建完成", []string{"ops"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次迭代用全新消息（ID 置空让 Publish 生成唯一 ID，避免主键冲突）。
		m.ID = ""
		if err := bk.Publish(ctx, m); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}

// BenchmarkPublishWithSubscribers 度量存在活跃订阅时的发布成本（含进程内广播投递）。
func BenchmarkPublishWithSubscribers(b *testing.B) {
	bk := newTestBroker(b)
	ctx := context.Background()
	uid := "usr_bench"

	// 模拟若干活跃订阅（进程内广播要遍历它们）。
	const subs = 10
	opened := make([]Subscription, 0, subs)
	for i := 0; i < subs; i++ {
		sub, err := bk.Subscribe(ctx, uid, nil)
		if err != nil {
			b.Fatalf("subscribe: %v", err)
		}
		opened = append(opened, sub)
	}
	defer func() {
		for _, s := range opened {
			s.Close()
		}
	}()

	m := msg(uid, "构建完成", []string{"ops"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.ID = "" // 每次生成唯一 ID
		if err := bk.Publish(ctx, m); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}

// BenchmarkReplay 度量断线续传（Replay）的读取成本：从已入队的 N 条消息中读回一批。
// 先预置 WARM 条消息，然后每次 Replay 读 lastBatch 条。
func BenchmarkReplay(b *testing.B) {
	bk := newTestBroker(b)
	ctx := context.Background()
	uid := "usr_bench"

	const (
		warm      = 10000 // 预置历史消息数
		lastBatch = 100   // 每次回读的条数
	)
	for i := 0; i < warm; i++ {
		if err := bk.Publish(ctx, msg(uid, "m", nil)); err != nil {
			b.Fatalf("warm publish: %v", err)
		}
	}
	sinceSeq := int64(warm - lastBatch)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := bk.Replay(ctx, uid, sinceSeq, lastBatch)
		if err != nil {
			b.Fatalf("replay: %v", err)
		}
		if len(got) != lastBatch {
			b.Fatalf("replay 返回 %d 条，期望 %d", len(got), lastBatch)
		}
	}
}

// BenchmarkAck 度量消费位移（Ack high-water）的更新成本。
func BenchmarkAck(b *testing.B) {
	bk := newTestBroker(b)
	ctx := context.Background()
	uid := "usr_bench"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := bk.Ack(ctx, "dev_bench", uid, int64(i)); err != nil {
			b.Fatalf("ack: %v", err)
		}
	}
}

// BenchmarkSubscribe 度量建立订阅的成本。
func BenchmarkSubscribe(b *testing.B) {
	bk := newTestBroker(b)
	ctx := context.Background()
	uid := "usr_bench"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sub, err := bk.Subscribe(ctx, uid, nil)
		if err != nil {
			b.Fatalf("subscribe: %v", err)
		}
		sub.Close()
	}
}
