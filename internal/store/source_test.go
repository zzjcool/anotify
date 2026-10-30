package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCronParsing(t *testing.T) {
	cases := []struct {
		expr    string
		wantErr bool
	}{
		{"0 3 * * *", false},
		{"*/15 * * * *", false},
		{"0 0 1 * *", false},
		{"0 0 * * 1-5", false},
		{"5,10,15 * * * *", false},
		{"0-30/5 * * * *", false},
		{"0 3 * *", true},     // 只有 4 段
		{"0 3 * * * *", true}, // 6 段（带秒）不支持
		{"@daily", true},      // 宏不支持
		{"60 * * * *", true},  // 分超范围
		{"0 24 * * *", true},  // 时超范围
		{"*/0 * * * *", true}, // 步长非法
		{"abc * * * *", true}, // 非数字
		{"* * * * L", true},   // L 扩展不支持
	}
	for _, c := range cases {
		_, err := ParseCron(c.expr)
		if c.wantErr && err == nil {
			t.Errorf("ParseCron(%q) 应该报错，但通过了", c.expr)
		}
		if !c.wantErr && err != nil {
			t.Errorf("ParseCron(%q) 应该通过，但报错: %v", c.expr, err)
		}
	}
}

func TestCronNext(t *testing.T) {
	// 每晚 3 点
	s, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	got := s.Next(base)
	want := time.Date(2026, 3, 16, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}

	// 每 15 分钟
	s2, _ := ParseCron("*/15 * * * *")
	got2 := s2.Next(time.Date(2026, 3, 15, 10, 7, 0, 0, time.UTC))
	want2 := time.Date(2026, 3, 15, 10, 15, 0, 0, time.UTC)
	if !got2.Equal(want2) {
		t.Errorf("Next(*/15) = %v, want %v", got2, want2)
	}

	// Next 必须严格大于 after（同一分钟不重复触发）
	s3, _ := ParseCron("* * * * *")
	at := time.Date(2026, 3, 15, 10, 7, 0, 0, time.UTC)
	got3 := s3.Next(at)
	if !got3.After(at) {
		t.Errorf("Next 应当严格大于入参, got %v at %v", got3, at)
	}
}

func TestCronDomDowOrSemantics(t *testing.T) {
	// 标准 cron：dom 和 dow 都受限时是 OR
	// "每月 1 号 或 每周一"
	s, err := ParseCron("0 0 1 * 1")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-01 是周日 → dom 命中
	if got := s.Next(time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dom 命中失败: %v", got)
	}
	// 2026-03-02 是周一 → dow 命中
	if got := s.Next(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dow 命中失败: %v", got)
	}
}

func TestCronNeverFires(t *testing.T) {
	// 2 月 30 日永远不会到来
	s, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !got.IsZero() {
		t.Errorf("永不成立的表达式应返回零值, got %v", got)
	}
}

func TestSourceCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.target", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		OrgID:   orgID,
		Name:    "每晚体检",
		Kind:    SourceKindCron,
		Config:  json.RawMessage(`{"expr":"0 3 * * *"}`),
		Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if src.ID == "" {
		t.Fatal("CreateSource 应生成 ID")
	}

	// 往返一致性：存什么读什么
	got, err := db.GetSource(ctx, orgID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != src.Name || got.Kind != SourceKindCron || !got.Enabled {
		t.Errorf("往返不一致: %+v", got)
	}
	if len(got.Targets) != 1 || got.Targets[0].CapabilityID != capA.ID {
		t.Errorf("出口不一致: %v", got.Targets)
	}
	// cron 建源时就该算好首次触发时刻
	if got.NextFireAt == nil {
		t.Error("cron 源应有 nextFireAt")
	}

	// 列表
	list, err := db.ListSources(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应 1 个源, got %d", len(list))
	}

	// 删除
	if err := db.DeleteSource(ctx, orgID, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSource(ctx, orgID, src.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("删后应 ErrSourceNotFound, got %v", err)
	}
}

func TestSourceValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.t2", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	// 没有出口 → 拒绝（否则事件永远派不出去）
	err := db.CreateSource(ctx, &Source{
		OrgID: orgID, Name: "无出口", Kind: SourceKindManual, Targets: nil,
	})
	if err == nil {
		t.Error("没有出口能力的源应被拒绝")
	}

	// 未知 kind
	err = db.CreateSource(ctx, &Source{
		OrgID: orgID, Name: "怪种类", Kind: "nope", Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if !errors.Is(err, ErrInvalidSourceKind) {
		t.Errorf("应 ErrInvalidSourceKind, got %v", err)
	}

	// 非法 cron 表达式
	err = db.CreateSource(ctx, &Source{
		OrgID: orgID, Name: "坏表达式", Kind: SourceKindCron,
		Config:  json.RawMessage(`{"expr":"61 * * * *"}`),
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Errorf("应 ErrInvalidSourceConfig, got %v", err)
	}

	// 重名
	ok := &Source{OrgID: orgID, Name: "重名", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, ok); err != nil {
		t.Fatal(err)
	}
	dup := &Source{OrgID: orgID, Name: "重名", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, dup); !errors.Is(err, ErrSourceNameTaken) {
		t.Errorf("重名应 ErrSourceNameTaken, got %v", err)
	}
}

func TestSourceWebhookToken(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.t3", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		OrgID: orgID, Name: "hook", Kind: SourceKindWebhook,
		Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	var wc WebhookConfig
	if err := json.Unmarshal(src.Config, &wc); err != nil {
		t.Fatal(err)
	}
	if wc.Token == "" {
		t.Fatal("webhook 源应自动生成 token")
	}
	// 按 token 找源
	found, err := db.GetSourceByWebhookToken(ctx, wc.Token)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != src.ID {
		t.Errorf("按 token 找回的源不对: %s vs %s", found.ID, src.ID)
	}
	// 停用后 token 不再有效（停用的源不该接受事件）
	if err := db.UpdateSource(ctx, &Source{
		ID: src.ID, OrgID: orgID, Name: src.Name, Kind: src.Kind,
		Config: src.Config, Enabled: false, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSourceByWebhookToken(ctx, wc.Token); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("停用后 token 应失效, got %v", err)
	}
}

func TestSourceKindImmutable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.t4", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{OrgID: orgID, Name: "keep", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	// 试图把 manual 改成 cron
	err := db.UpdateSource(ctx, &Source{
		ID: src.ID, OrgID: orgID, Name: "keep", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *"}`), Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if err == nil {
		t.Error("kind 不应允许修改")
	}
}

func TestDueCronSources(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.t5", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		OrgID: orgID, Name: "due", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *"}`), Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSource(ctx, orgID, src.ID)

	// 还没到点 → 不应被取出
	if due, _ := db.DueCronSources(ctx, *got.NextFireAt-60, 10); len(due) != 0 {
		t.Errorf("未到点不该取出, got %d", len(due))
	}
	// 到点 → 应被取出
	if due, _ := db.DueCronSources(ctx, *got.NextFireAt, 10); len(due) != 1 {
		t.Errorf("到点应取出 1 个, got %d", len(due))
	}
}

// TestCreateSourceRejectsReservedName 源名不许叫 route（#142 §9.2）。
//
// 源名就是它产生事件的 source（入口边左端 / POST /v1/events）。
// 叫 route 的源产生的事件会被事件流静默过滤、在 trace 视图里被当成剪枝，
// 所以必须在**配置期**就拒绝，而不是等事件消失后再去猜。
func TestCreateSourceRejectsReservedName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.reserved", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"route", "  route  "} {
		err := db.CreateSource(ctx, &Source{
			OrgID: orgID, Name: name, Kind: SourceKindManual, Enabled: true,
			Targets: []SourceTarget{{CapabilityID: capA.ID}},
		})
		if err == nil {
			t.Fatalf("CreateSource(name=%q) 应该被拒绝", name)
		}
		if !errors.Is(err, ErrReservedEventSource) {
			t.Fatalf("CreateSource(name=%q) 错误不可判: %v", name, err)
		}
	}
	// 一行都没建出来（拒绝发生在落库前）
	list, err := db.ListSources(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("被拒的源还是落库了: %d 个", len(list))
	}

	// 名字里包含 route 但不等于 route 的源照常可建（不能误伤）
	if err := db.CreateSource(ctx, &Source{
		OrgID: orgID, Name: "route 备份", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}); err != nil {
		t.Fatalf("名字只是含有 route 的源被误伤: %v", err)
	}
}

// TestUpdateSourceRejectsReservedName 已有源不许改名为 route（#142 §9.2 延伸）。
//
// 只在 CreateSource 拦是不够的：先建一个正常源、再 PATCH 改名成 route，
// 就绕过了那道水闸，该源的事件照样会被事件流静默吞掉。
func TestUpdateSourceRejectsReservedName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	capA := mkCap(orgID, "demo.rename", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		OrgID: orgID, Name: "原来的名字", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}

	for _, name := range []string{"route", "  route  "} {
		cur, err := db.GetSource(ctx, orgID, src.ID)
		if err != nil {
			t.Fatalf("get source: %v", err)
		}
		cur.Name = name
		err = db.UpdateSource(ctx, cur)
		if err == nil {
			t.Fatalf("UpdateSource(name=%q) 应该被拒绝", name)
		}
		if !errors.Is(err, ErrReservedEventSource) {
			t.Fatalf("UpdateSource(name=%q) 错误不可判: %v", name, err)
		}
		// 改动不应该落库（名字保持原样）
		after, err := db.GetSource(ctx, orgID, src.ID)
		if err != nil {
			t.Fatalf("get source after reject: %v", err)
		}
		if after.Name != "原来的名字" {
			t.Fatalf("被拒的改名还是生效了：name = %q", after.Name)
		}
	}

	// 名字只是含有 route 的照常能改（不能误伤）
	cur, err := db.GetSource(ctx, orgID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cur.Name = "route 备份"
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("名字只是含有 route 的改名被误伤: %v", err)
	}
	after, err := db.GetSource(ctx, orgID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if after.Name != "route 备份" {
		t.Fatalf("正常改名没生效：name = %q", after.Name)
	}
}
