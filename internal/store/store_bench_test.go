package store

import (
	"context"
	"fmt"
	"testing"
)

// benchStore 打开内存库并插入外键父用户。
func benchStore(b *testing.B) *DB {
	b.Helper()
	db, err := Open(":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.InsertUser(context.Background(), "usr_bench", "tester", "Tester"); err != nil {
		b.Fatalf("insert user: %v", err)
	}
	return db
}

// BenchmarkListEnabledDevices 度量投递候选集读取：给定用户，列出全部 enabled 设备。
// 这是每条消息派发前必经的查询（push/ws 两个消费者都会调用）。
func BenchmarkListEnabledDevices(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	// 预置 200 台设备（一半 enabled），贴近真实规模。
	for i := 0; i < 200; i++ {
		dev := &Device{
			ID:           fmt.Sprintf("dev_bench_%03d", i),
			UserID:       "usr_bench",
			Enabled:      i%2 == 0,
			StatusFilter: "all",
			Endpoint:     fmt.Sprintf("https://push.example.com/%d", i),
			P256dh:       "p", Auth: "a",
			Tags: []string{"ops"},
		}
		if err := db.UpsertDevice(ctx, dev); err != nil {
			b.Fatalf("upsert device: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		devs, err := db.ListEnabledDevices(ctx, "usr_bench")
		if err != nil {
			b.Fatalf("list enabled devices: %v", err)
		}
		if len(devs) != 100 {
			b.Fatalf("应列出 100 台 enabled 设备，实际 %d", len(devs))
		}
	}
}

// BenchmarkGetMessage 度量按 ID 取单条消息（消息详情页 / WS 回放单条）的读取成本。
func BenchmarkGetMessage(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	// 预置若干消息，取中间一条做基准（贴近真实随机访问）。
	for i := 0; i < 100; i++ {
		if err := db.InsertTestMessage(ctx, fmt.Sprintf("ntf_bench_%03d", i), "usr_bench", int64(i+1), "success"); err != nil {
			b.Fatalf("insert message: %v", err)
		}
	}
	target := "ntf_bench_050"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := db.GetMessage(ctx, "usr_bench", target)
		if err != nil {
			b.Fatalf("get message: %v", err)
		}
		if m == nil {
			b.Fatal("消息不存在")
		}
	}
}
