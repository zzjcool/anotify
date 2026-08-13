package store

import (
	"context"
	"errors"
	"testing"
)

// 验证 UpdateDevice 按 id 全字段更新 name/enabled/event_scope/tags，
// 且不影响 p256dh/auth/endpoint（那些是订阅凭证，不可变）。
func TestUpdateDevice(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid, _ := db.InsertUser(ctx, "", "u1", "u1")

	dev := &Device{
		ID: NewDeviceID(), UserID: uid, Name: "旧名", Platform: "ios",
		Enabled: true, EventScope: "all", Tags: []string{"a"},
		Endpoint: "https://push.example.com/x", P256dh: "p1", Auth: "a1", CreatedAt: Now(),
	}
	if err := db.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	// 改配置
	dev.Name = "新名"
	dev.Enabled = false
	dev.EventScope = "final"
	dev.Tags = []string{"手机", "工作"}
	if err := db.UpdateDevice(ctx, dev); err != nil {
		t.Fatalf("UpdateDevice: %v", err)
	}

	got, err := db.ListDevices(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 台设备, got %d", len(got))
	}
	g := got[0]
	if g.Name != "新名" || g.Enabled != false || g.EventScope != "final" || len(g.Tags) != 2 {
		t.Errorf("配置未更新: name=%s enabled=%v filter=%s tags=%v", g.Name, g.Enabled, g.EventScope, g.Tags)
	}
	if g.P256dh != "p1" || g.Auth != "a1" || g.Endpoint != "https://push.example.com/x" {
		t.Errorf("订阅凭证被意外修改: endpoint=%s", g.Endpoint)
	}

	// 更新不存在的设备 → ErrNotFound
	missing := &Device{ID: "dev_nonexistent", Name: "x", Tags: []string{}}
	if err := db.UpdateDevice(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("更新不存在设备应返回 ErrNotFound, got %v", err)
	}
}
