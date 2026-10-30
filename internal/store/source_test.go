package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.target", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		NamespaceID: nsID,
		Name:        "每晚体检",
		Kind:        SourceKindCron,
		Config:      json.RawMessage(`{"expr":"0 3 * * *"}`),
		Enabled:     true,
		Targets:     []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if src.ID == "" {
		t.Fatal("CreateSource 应生成 ID")
	}

	// 往返一致性：存什么读什么
	got, err := db.GetSource(ctx, nsID, src.ID)
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
	list, err := db.ListSources(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应 1 个源, got %d", len(list))
	}

	// 删除
	if err := db.DeleteSource(ctx, nsID, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSource(ctx, nsID, src.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("删后应 ErrSourceNotFound, got %v", err)
	}
}

func TestSourceValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.t2", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	// 没有出口 → 拒绝（否则事件永远派不出去）
	err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "无出口", Kind: SourceKindManual, Targets: nil,
	})
	if err == nil {
		t.Error("没有出口能力的源应被拒绝")
	}

	// 未知 kind
	err = db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "怪种类", Kind: "nope", Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if !errors.Is(err, ErrInvalidSourceKind) {
		t.Errorf("应 ErrInvalidSourceKind, got %v", err)
	}

	// 非法 cron 表达式
	err = db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "坏表达式", Kind: SourceKindCron,
		Config:  json.RawMessage(`{"expr":"61 * * * *"}`),
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Errorf("应 ErrInvalidSourceConfig, got %v", err)
	}

	// 重名
	ok := &Source{NamespaceID: nsID, Name: "重名", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, ok); err != nil {
		t.Fatal(err)
	}
	dup := &Source{NamespaceID: nsID, Name: "重名", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, dup); !errors.Is(err, ErrSourceNameTaken) {
		t.Errorf("重名应 ErrSourceNameTaken, got %v", err)
	}
}

func TestSourceWebhookToken(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.t3", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		NamespaceID: nsID, Name: "hook", Kind: SourceKindWebhook,
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
		ID: src.ID, NamespaceID: nsID, Name: src.Name, Kind: src.Kind,
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
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.t4", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{NamespaceID: nsID, Name: "keep", Kind: SourceKindManual, Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}}}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	// 试图把 manual 改成 cron
	err := db.UpdateSource(ctx, &Source{
		ID: src.ID, NamespaceID: nsID, Name: "keep", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *"}`), Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if err == nil {
		t.Error("kind 不应允许修改")
	}
}

func TestDueCronSources(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.t5", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		NamespaceID: nsID, Name: "due", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *"}`), Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSource(ctx, nsID, src.ID)

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
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.reserved", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"route", "  route  "} {
		err := db.CreateSource(ctx, &Source{
			NamespaceID: nsID, Name: name, Kind: SourceKindManual, Enabled: true,
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
	list, err := db.ListSources(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("被拒的源还是落库了: %d 个", len(list))
	}

	// 名字里包含 route 但不等于 route 的源照常可建（不能误伤）
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "route 备份", Kind: SourceKindManual, Enabled: true,
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
	nsID, _ := newTestNS(t, db)
	capA := mkCap(nsID, "demo.rename", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	src := &Source{
		NamespaceID: nsID, Name: "原来的名字", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}

	for _, name := range []string{"route", "  route  "} {
		cur, err := db.GetSource(ctx, nsID, src.ID)
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
		after, err := db.GetSource(ctx, nsID, src.ID)
		if err != nil {
			t.Fatalf("get source after reject: %v", err)
		}
		if after.Name != "原来的名字" {
			t.Fatalf("被拒的改名还是生效了：name = %q", after.Name)
		}
	}

	// 名字只是含有 route 的照常能改（不能误伤）
	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cur.Name = "route 备份"
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("名字只是含有 route 的改名被误伤: %v", err)
	}
	after, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if after.Name != "route 备份" {
		t.Fatalf("正常改名没生效：name = %q", after.Name)
	}
}

// ---- 源变量（#114）：VarDefs / ConfigPayload / ValidateVarDefs / 配置保留 ----

// TestValidateVarDefs 变量声明的配置期校验（契约 §2.4 / §4 第 16 条）。
//
// 断言逐字比对错误文案：这些话会原样进 400 的响应体给用户看，
// 措辞变了就等于契约变了。
func TestValidateVarDefs(t *testing.T) {
	cases := []struct {
		name string
		defs []VarDef
		want string // "" = 合法
	}{
		{"空列表合法", nil, ""},
		{"空列表合法（空切片）", []VarDef{}, ""},
		{"合法 from", []VarDef{{Name: "a", From: "pull_request.title"}}, ""},
		{"合法常量", []VarDef{{Name: "env", Value: "prod", HasValue: true}}, ""},
		{"合法常量 null", []VarDef{{Name: "env", Value: nil, HasValue: true}}, ""},
		{"名字以数字开头", []VarDef{{Name: "9bad", From: "a"}},
			"变量名「9bad」不合法（只能是字母/数字/下划线，且不以数字开头）"},
		{"名字带减号", []VarDef{{Name: "a-b", From: "a"}},
			"变量名「a-b」不合法（只能是字母/数字/下划线，且不以数字开头）"},
		{"名字里有空格", []VarDef{{Name: "a b", From: "a"}},
			"变量名「a b」不合法（只能是字母/数字/下划线，且不以数字开头）"},
		{"名字首尾有空白", []VarDef{{Name: " a ", From: "a"}},
			"变量名「 a 」不合法（只能是字母/数字/下划线，且不以数字开头）"},
		{"名字是空白", []VarDef{{Name: "   ", From: "a"}}, "每个变量都要有名字"},
		{"重名", []VarDef{{Name: "a", From: "x"}, {Name: "a", Value: 1, HasValue: true}},
			"变量名「a」重复"},
		{"from 与 value 并存", []VarDef{{Name: "a", From: "x", Value: 1, HasValue: true}},
			"变量「a」不能同时有 from 和 value"},
		{"两个都没有", []VarDef{{Name: "a"}}, "变量「a」需要 from（payload 路径）"},
		{"from 含 ..", []VarDef{{Name: "a", From: "a..b"}},
			"变量「a」的 from 路径不合法：「a..b」"},
		{"from 以点开头", []VarDef{{Name: "a", From: ".a"}},
			"变量「a」的 from 路径不合法：「.a」"},
		{"from 以点结尾", []VarDef{{Name: "a", From: "a."}},
			"变量「a」的 from 路径不合法：「a.」"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidateVarDefs(c.defs); got != c.want {
				t.Errorf("ValidateVarDefs(%+v) = %q, want %q", c.defs, got, c.want)
			}
		})
	}
}

// TestSourceVarDefsParsing VarDefs() 认两种形态，坏数据读侧容忍（→ nil）。
func TestSourceVarDefsParsing(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   []VarDef
	}{
		{"引用 payload 路径", `{"vars":[{"name":"a","from":"pull_request.title"}]}`,
			[]VarDef{{Name: "a", From: "pull_request.title"}}},
		{"旧形态常量", `{"vars":[{"name":"env","value":"prod"}]}`,
			[]VarDef{{Name: "env", Value: "prod", HasValue: true}}},
		{"常量 null 也算有值", `{"vars":[{"name":"env","value":null}]}`,
			[]VarDef{{Name: "env", HasValue: true}}},
		{"多个变量保序", `{"vars":[{"name":"b","from":"x"},{"name":"a","from":"y"}]}`,
			[]VarDef{{Name: "b", From: "x"}, {Name: "a", From: "y"}}},
		{"没有 vars", `{"expr":"0 3 * * *"}`, nil},
		{"vars 不是数组", `{"vars":{"a":"x"}}`, nil},
		{"vars 里有非对象项", `{"vars":["a"]}`, nil},
		{"坏 JSON", `{"vars":`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Source{Config: json.RawMessage(c.config)}
			if got := s.VarDefs(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("VarDefs() = %+v, want %+v", got, c.want)
			}
		})
	}

	// VarNames 保持顺序
	s := &Source{Config: json.RawMessage(`{"vars":[{"name":"b","from":"x"},{"name":"a","value":1}]}`)}
	if got, want := s.VarNames(), []string{"b", "a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("VarNames() = %v, want %v", got, want)
	}
	if got := (&Source{Config: json.RawMessage(`{}`)}).VarNames(); len(got) != 0 {
		t.Errorf("没有 vars 时 VarNames 应为空, got %v", got)
	}
}

// TestSourceConfigPayloadParsing ConfigPayload() 只认 JSON object。
func TestSourceConfigPayloadParsing(t *testing.T) {
	got := (&Source{Config: json.RawMessage(`{"payload":{"env":"prod","n":3}}`)}).ConfigPayload()
	if got["env"] != "prod" {
		t.Errorf("payload 取错: %+v", got)
	}
	if n, ok := got["n"].(float64); !ok || n != 3 {
		t.Errorf("payload.n 应是数字 3: %+v", got["n"])
	}

	for _, cfg := range []string{
		`{"payload":[1,2]}`,
		`{"payload":"x"}`,
		`{"payload":1}`,
		`{"payload":null}`,
		`{"payload":`,
		`{}`,
		`{"expr":"0 3 * * *"}`,
	} {
		if p := (&Source{Config: json.RawMessage(cfg)}).ConfigPayload(); p != nil {
			t.Errorf("ConfigPayload(%s) 应为 nil, got %+v", cfg, p)
		}
	}
}

// TestSourceConfigKeepsVars 建源/改源都不许把 vars 与 payload 抹掉（契约 §4 第 17/18 条）。
//
// 「存什么读什么」是硬要求：之前的 ValidateSourceConfig 整份重写 config，
// 只回写 {expr,tz} / {token,event} / {}，vars 与 payload 会静默消失。
func TestSourceConfigKeepsVars(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, nsID := newTestOrgNS(t, db)
	capA := mkCap(nsID, "demo.vars", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}

	wantDefs := []VarDef{{Name: "a", From: "pull_request.title"}}

	// cron：vars + payload 都要活过建源
	src := &Source{
		OrgID: orgID, NamespaceID: nsID, Name: "带变量的定时源", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *",
			"vars":[{"name":"a","from":"pull_request.title"}],
			"payload":{"env":"prod"}}`),
		Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}
	got, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if defs := got.VarDefs(); !reflect.DeepEqual(defs, wantDefs) {
		t.Errorf("建源后 vars 往返不一致: %+v, want %+v", defs, wantDefs)
	}
	if names := got.VarNames(); !reflect.DeepEqual(names, []string{"a"}) {
		t.Errorf("建源后 VarNames 不一致: %v", names)
	}
	if p := got.ConfigPayload(); p["env"] != "prod" {
		t.Errorf("建源后 payload 往返不一致: %+v", p)
	}
	// 现有行为不能回归：tz 仍然补成 Local、表达式仍被真解析
	var cc CronConfig
	if err := json.Unmarshal(got.Config, &cc); err != nil {
		t.Fatalf("cron config 坏了: %v", err)
	}
	if cc.Expr != "0 3 * * *" || cc.TZ != "Local" {
		t.Errorf("cron 规范化结果不对: %+v", cc)
	}

	// PATCH 只改 expr（模拟 handler：拿当前 config 改一个键再存回）：
	// vars 与 payload 必须仍在。
	cfgMap := map[string]json.RawMessage{}
	if err := json.Unmarshal(got.Config, &cfgMap); err != nil {
		t.Fatal(err)
	}
	cfgMap["expr"] = json.RawMessage(`"0 4 * * *"`)
	patched, err := json.Marshal(cfgMap)
	if err != nil {
		t.Fatal(err)
	}
	got.Config = patched
	if err := db.UpdateSource(ctx, got); err != nil {
		t.Fatalf("改源: %v", err)
	}
	after, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after.Config, &cc); err != nil {
		t.Fatalf("cron config 坏了: %v", err)
	}
	if cc.Expr != "0 4 * * *" {
		t.Errorf("改 expr 没生效: %+v", cc)
	}
	if defs := after.VarDefs(); !reflect.DeepEqual(defs, wantDefs) {
		t.Errorf("改 expr 后 vars 丢了: %+v, want %+v", defs, wantDefs)
	}
	if p := after.ConfigPayload(); p["env"] != "prod" {
		t.Errorf("改 expr 后 payload 丢了: %+v", p)
	}

	// manual：payload + vars（含旧形态常量）同样要保留
	manualWant := []VarDef{{Name: "env", Value: "prod", HasValue: true}}
	msrc := &Source{
		OrgID: orgID, NamespaceID: nsID, Name: "手动带参", Kind: SourceKindManual,
		Config:  json.RawMessage(`{"vars":[{"name":"env","value":"prod"}],"payload":{"sha":"deadbeef"}}`),
		Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, msrc); err != nil {
		t.Fatalf("建 manual 源: %v", err)
	}
	mgot, err := db.GetSource(ctx, nsID, msrc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if defs := mgot.VarDefs(); !reflect.DeepEqual(defs, manualWant) {
		t.Errorf("manual vars 往返不一致: %+v, want %+v", defs, manualWant)
	}
	if p := mgot.ConfigPayload(); p["sha"] != "deadbeef" {
		t.Errorf("manual payload 往返不一致: %+v", p)
	}

	// webhook：只保留 token/event/vars，payload 不保留（投递体才是 payload）
	wsrc := &Source{
		OrgID: orgID, NamespaceID: nsID, Name: "带变量的钩子", Kind: SourceKindWebhook,
		Config:  json.RawMessage(`{"vars":[{"name":"a","from":"x"}],"payload":{"env":"prod"}}`),
		Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, wsrc); err != nil {
		t.Fatalf("建 webhook 源: %v", err)
	}
	wgot, err := db.GetSource(ctx, nsID, wsrc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if defs := wgot.VarDefs(); !reflect.DeepEqual(defs, []VarDef{{Name: "a", From: "x"}}) {
		t.Errorf("webhook vars 往返不一致: %+v", defs)
	}
	if p := wgot.ConfigPayload(); p != nil {
		t.Errorf("webhook 不该保留 config.payload, got %+v", p)
	}
}

// TestValidateSourceConfigPayloadOnlyForKeepingKinds A17：payload 的对象性校验
// 只对**会保留** payload 的 kind（cron/manual）生效；webhook/github 的 payload 直接
// 丢弃，既不校验也不报错。
//
// 回归背景：校验曾经对所有 kind 生效，于是历史 webhook 行里残留一个非对象 payload
// 时，任何一次 PATCH（哪怕只改名字）都会莫名 400 —— 而那个 payload 本来就会被丢掉。
func TestValidateSourceConfigPayloadOnlyForKeepingKinds(t *testing.T) {
	// 1) 配置期：webhook/github 带非对象 payload 也能过，输出不含 payload，token 原样保留
	for _, kind := range []string{SourceKindWebhook, SourceKindGitHub} {
		out, err := ValidateSourceConfig(kind, json.RawMessage(`{"token":"src_x","payload":[1,2]}`))
		if err != nil {
			t.Fatalf("%s 不该因非对象 payload 被拒（它本就会被丢弃）: %v", kind, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out, &fields); err != nil {
			t.Fatalf("%s 输出 config 不是对象: %v (%s)", kind, err, out)
		}
		if raw, ok := fields["payload"]; ok {
			t.Errorf("%s 输出不该保留 payload, got %s", kind, raw)
		}
		var wc WebhookConfig
		if err := json.Unmarshal(out, &wc); err != nil {
			t.Fatalf("%s 输出 config 坏了: %v", kind, err)
		}
		if wc.Token != "src_x" {
			t.Errorf("%s token 应原样保留, got %q", kind, wc.Token)
		}
	}

	// 2) cron 仍拒：它是会保留 payload 的 kind，非对象打底数据不许进库
	if _, err := ValidateSourceConfig(SourceKindCron,
		json.RawMessage(`{"expr":"0 3 * * *","payload":[1,2]}`)); !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("cron 的非对象 payload 应仍被拒, got %v", err)
	}

	// 3) manual：合法对象 payload 原样保留（往返）
	out, err := ValidateSourceConfig(SourceKindManual, json.RawMessage(`{"payload":{"a":1}}`))
	if err != nil {
		t.Fatalf("manual 的对象 payload 不该被拒: %v", err)
	}
	if p := (&Source{Config: out}).ConfigPayload(); p["a"] != float64(1) {
		t.Errorf("manual payload 往返不一致: %+v (%s)", p, out)
	}

	// 4) 落到库上：建源/改源都不因非对象 payload 被拒，且库里不残留 payload
	db := newTestDB(t)
	ctx := context.Background()
	orgID, nsID := newTestOrgNS(t, db)
	capA := mkCap(nsID, "demo.payload-kind", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}
	src := &Source{
		OrgID: orgID, NamespaceID: nsID, Name: "带坏 payload 的钩子", Kind: SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"src_x","payload":[1,2]}`),
		Enabled: true, Targets: []SourceTarget{{CapabilityID: capA.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建 webhook 源不该被非对象 payload 挡住: %v", err)
	}
	assertNoStoredPayload := func(when string) {
		t.Helper()
		got, err := db.GetSource(ctx, nsID, src.ID)
		if err != nil {
			t.Fatalf("%s: 取源: %v", when, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got.Config, &fields); err != nil {
			t.Fatalf("%s: config 不是对象: %v (%s)", when, err, got.Config)
		}
		if raw, ok := fields["payload"]; ok {
			t.Errorf("%s: 库里不该有 payload, got %s", when, raw)
		}
		if p := got.ConfigPayload(); p != nil {
			t.Errorf("%s: ConfigPayload 应为 nil, got %+v", when, p)
		}
	}
	assertNoStoredPayload("建源后")

	// 模拟历史行：老版本把非对象 payload 写进了 webhook 的 config
	if _, err := db.ExecContext(ctx,
		`UPDATE sources SET config = ? WHERE org_id = ? AND id = ?`,
		`{"token":"src_x","payload":[1,2]}`, orgID, src.ID); err != nil {
		t.Fatalf("造历史行: %v", err)
	}

	// PATCH 语义：GetSource 拿到带历史坏 payload 的 config 再存回，不该 400
	legacy, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Name = "改个名不该 400"
	if err := db.UpdateSource(ctx, legacy); err != nil {
		t.Fatalf("改 webhook 源不该被历史坏 payload 挡住: %v", err)
	}
	assertNoStoredPayload("改源后")
}

// TestValidateSourceConfigRejectsBadVars 非法 vars / 非对象 payload 必须被拒，
// 且错误可判（server 层把哨兵映射成 400）、消息里能看见变量名。
func TestValidateSourceConfigRejectsBadVars(t *testing.T) {
	cases := []struct {
		name, kind, config, wantSub string
	}{
		{"非法变量名", SourceKindCron, `{"expr":"0 3 * * *","vars":[{"name":"9bad","from":"a"}]}`,
			"9bad"},
		{"重名", SourceKindManual, `{"vars":[{"name":"a","from":"x"},{"name":"a","value":1}]}`,
			"变量名「a」重复"},
		{"from 与 value 并存", SourceKindManual,
			`{"vars":[{"name":"a","from":"x","value":1}]}`,
			"变量「a」不能同时有 from 和 value"},
		{"变量项不是对象", SourceKindManual, `{"vars":["a"]}`, "变量项必须是对象"},
		{"vars 不是数组", SourceKindCron,
			`{"expr":"0 3 * * *","vars":{"a":"x"}}`, "config.vars 必须是数组"},
		{"payload 是数组", SourceKindCron,
			`{"expr":"0 3 * * *","payload":[1,2]}`, "config.payload 必须是 JSON 对象"},
		{"payload 是字符串", SourceKindManual, `{"payload":"x"}`, "config.payload 必须是 JSON 对象"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := ValidateSourceConfig(c.kind, json.RawMessage(c.config))
			if err == nil {
				t.Fatalf("应该被拒, got config %s", out)
			}
			if !errors.Is(err, ErrInvalidSourceConfig) {
				t.Fatalf("错误不可判（server 会变成 500）: %v", err)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("错误消息里应有 %q, got %v", c.wantSub, err)
			}
			if out != nil {
				t.Errorf("被拒时不该返回 config: %s", out)
			}
		})
	}

	// 落到 CreateSource 也仍然是可判的哨兵错误（不是被包成普通 error）
	db := newTestDB(t)
	ctx := context.Background()
	orgID, nsID := newTestOrgNS(t, db)
	capA := mkCap(nsID, "demo.badvars", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatal(err)
	}
	err := db.CreateSource(ctx, &Source{
		OrgID: orgID, NamespaceID: nsID, Name: "坏变量", Kind: SourceKindManual, Enabled: true,
		Config:  json.RawMessage(`{"vars":[{"name":"a","from":"x"},{"name":"a","from":"y"}]}`),
		Targets: []SourceTarget{{CapabilityID: capA.ID}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("CreateSource 应透出 ErrInvalidSourceConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "a") {
		t.Fatalf("CreateSource 的错误消息里应有变量名, got %v", err)
	}
	if list, _ := db.ListSources(ctx, orgID); len(list) != 0 {
		t.Fatalf("被拒的源还是落库了: %d 个", len(list))
	}
}
