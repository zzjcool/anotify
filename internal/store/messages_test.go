package store

import (
	"context"
	"testing"
	"time"
)

// TestGetMessageRoundTrip 往返一致性：存什么读什么（含 tags/priority/ttl/payload/时间）。
func TestGetMessageRoundTrip(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid, _ := db.InsertUser(ctx, "", "u1", "u1")

	now := time.Now().UTC().Truncate(time.Second)
	in := &MessageRow{
		ID:         "ntf_rt_1",
		UserID:     uid,
		Seq:        7,
		Title:      "构建完成",
		AgentState: "done",
		Body:       "共 47 个文件变更",
		Link:       "pi://session/s1",
		DeviceTags: []string{"手机", "工作"},
		Priority:   "high",
		TTLSeconds: 3600,
		Payload:    []byte(`{"agentId":"deploy-bot","durationMs":24000}`),
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
	}
	if err := db.InsertMessage(ctx, in); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	got, err := db.GetMessage(ctx, uid, "ntf_rt_1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got == nil {
		t.Fatal("GetMessage 返回 nil，期望命中")
	}
	if got.ID != in.ID || got.Title != in.Title || got.Body != in.Body ||
		got.Link != in.Link || got.Priority != "high" || got.TTLSeconds != 3600 ||
		got.Seq != 7 || got.AgentState != "done" {
		t.Fatalf("字段不一致: %+v", got)
	}
	if len(got.DeviceTags) != 2 || got.DeviceTags[0] != "手机" || got.DeviceTags[1] != "工作" {
		t.Fatalf("DeviceTags 不一致: %v", got.DeviceTags)
	}
	if string(got.Payload) != string(in.Payload) {
		t.Fatalf("Payload 不一致: %s", got.Payload)
	}
	if !got.CreatedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("时间不一致: created=%v expires=%v", got.CreatedAt, got.ExpiresAt)
	}
}

// TestGetMessageNotFoundAndOwner 未命中返回 (nil,nil)；非属主也返回 (nil,nil)（防越权读）。
func TestGetMessageNotFoundAndOwner(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid1, _ := db.InsertUser(ctx, "", "u1", "u1")
	uid2, _ := db.InsertUser(ctx, "", "u2", "u2")

	if err := db.InsertTestMessage(ctx, "ntf_own", uid1, 1, "success"); err != nil {
		t.Fatal(err)
	}

	// 不存在
	got, err := db.GetMessage(ctx, uid1, "ntf_no_such")
	if err != nil || got != nil {
		t.Fatalf("不存在应返回 (nil,nil)，got=%v err=%v", got, err)
	}
	// 非属主（越权）
	got, err = db.GetMessage(ctx, uid2, "ntf_own")
	if err != nil || got != nil {
		t.Fatalf("非属主应返回 (nil,nil)，got=%v err=%v", got, err)
	}
	// 属主正常命中
	got, err = db.GetMessage(ctx, uid1, "ntf_own")
	if err != nil || got == nil {
		t.Fatalf("属主应命中，got=%v err=%v", got, err)
	}
}

// TestListDeliveriesByMessage 列出一条消息的全部投递记录（升序），并与其他消息隔离。
func TestListDeliveriesByMessage(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid, _ := db.InsertUser(ctx, "", "u1", "u1")

	if err := db.InsertTestMessage(ctx, "ntf_d1", uid, 1, "success"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTestMessage(ctx, "ntf_d2", uid, 2, "error"); err != nil {
		t.Fatal(err)
	}

	// d1: 两台设备，一送一败
	if err := db.RecordDelivery(ctx, "ntf_d1", "dev_a", ChannelWebPush, DeliverySent, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDelivery(ctx, "ntf_d1", "dev_b", ChannelWebPush, DeliveryFailed, "410 gone"); err != nil {
		t.Fatal(err)
	}
	// d2: 一条 websocket 记录（隔离校验）
	if err := db.RecordDelivery(ctx, "ntf_d2", "conn_x", ChannelWebSocket, DeliveryDelivered, ""); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListDeliveriesByMessage(ctx, "ntf_d1")
	if err != nil {
		t.Fatalf("ListDeliveriesByMessage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ntf_d1 应有 2 条投递记录，got %d", len(rows))
	}
	if rows[0].DeviceID != "dev_a" || rows[0].Status != DeliverySent || rows[0].Channel != ChannelWebPush {
		t.Fatalf("第一条记录不符: %+v", rows[0])
	}
	if rows[1].DeviceID != "dev_b" || rows[1].Status != DeliveryFailed || rows[1].Error != "410 gone" {
		t.Fatalf("第二条记录不符: %+v", rows[1])
	}
	if rows[0].Attempts != 1 || rows[0].CreatedAt.IsZero() || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("attempts/时间字段不符: %+v", rows[0])
	}

	// 隔离：d2 只有 1 条
	rows2, err := db.ListDeliveriesByMessage(ctx, "ntf_d2")
	if err != nil || len(rows2) != 1 {
		t.Fatalf("ntf_d2 应有 1 条记录，got %d err=%v", len(rows2), err)
	}

	// 空：无记录的消息返回空列表（非 nil 错误）
	rows3, err := db.ListDeliveriesByMessage(ctx, "ntf_none")
	if err != nil || len(rows3) != 0 {
		t.Fatalf("无记录应返回空列表，got %d err=%v", len(rows3), err)
	}
}
