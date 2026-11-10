package store

// 源（source）的数据层验收 —— v4 迁移。
//
// v4 的源模型与 v3 的差异（source.md §2–§5）：
//   - **出口是引用**：targets 里写 `name` / `name@version`（sourcespec.Target），
//     不再是 capability_id 列；写入期必须能在同命名空间内解析。
//   - **没有 vars / resultSchema**：参数由 target.params 的 `$(payload.…)` 模板表达，
//     载荷契约叫 `payloadSchema`（校验的是**投递进来的** payload，不是结果）。
//   - **config 是封闭字段集**：每种 kind 只认自己的键，未知键在创建期就报错
//     （不再有「整份重写、静默丢键」这种形状）。
//   - webhook 的 token 由 store 生成（空 = 待生成），停用后 LookupHook 一律 ErrSourceNotFound。
//   - 源名不可改（改名 = 断掉入口边）：reserved/rename 两道水闸在 v4 合并成"名字不可变"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/sourcespec"
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
		_, err := sourcespec.ParseCron(c.expr)
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
	s, err := sourcespec.ParseCron("0 3 * * *")
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
	s2, _ := sourcespec.ParseCron("*/15 * * * *")
	got2 := s2.Next(time.Date(2026, 3, 15, 10, 7, 0, 0, time.UTC))
	want2 := time.Date(2026, 3, 15, 10, 15, 0, 0, time.UTC)
	if !got2.Equal(want2) {
		t.Errorf("Next(*/15) = %v, want %v", got2, want2)
	}

	// Next 必须严格大于 after（同一分钟不重复触发）
	s3, _ := sourcespec.ParseCron("* * * * *")
	at := time.Date(2026, 3, 15, 10, 7, 0, 0, time.UTC)
	got3 := s3.Next(at)
	if !got3.After(at) {
		t.Errorf("Next 应当严格大于入参, got %v at %v", got3, at)
	}
}

func TestCronDomDowOrSemantics(t *testing.T) {
	// 标准 cron：dom 和 dow 都受限时是 OR
	// "每月 1 号 或 每周一"
	s, err := sourcespec.ParseCron("0 0 1 * 1")
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
	s, err := sourcespec.ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !got.IsZero() {
		t.Errorf("永不成立的表达式应返回零值, got %v", got)
	}
}

// sourceFixtureCap 建一个可被 targets 引用的能力（裸名字 = 跟随 latest）。
func sourceFixtureCap(t *testing.T, db *DB, nsID, name string) *CapabilityVersion {
	t.Helper()
	return v4Cap(t, db, nsID, name, 1)
}

// cronSrc 造一条 cron 源的声明（targets 用能力**名字**引用）。
func cronSrc(nsID, name, expr, capability string) *Source {
	return &Source{
		NamespaceID: nsID,
		Name:        name,
		Kind:        SourceKindCron,
		Config:      json.RawMessage(`{"expr":` + jsonQuote(expr) + `}`),
		Enabled:     true,
		Targets:     []sourcespec.Target{{Capability: capability}},
	}
}

// jsonQuote 把字符串编成 JSON 字面量（测试里拼 config 用）。
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestSourceCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.target")

	src := cronSrc(nsID, "每晚体检", "0 3 * * *", "demo.target")
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
	if len(got.Targets) != 1 || got.Targets[0].Capability != "demo.target" {
		t.Errorf("出口不一致: %v", got.Targets)
	}
	// 入口边是 targets 的投影：from_is_source=1、from_ref=源名、to_ref=能力引用。
	if edges, eerr := db.ListEdges(ctx, nsID); eerr != nil {
		t.Fatalf("list edges: %v", eerr)
	} else if len(edges) != 1 || !edges[0].FromIsSource || edges[0].FromRef != "每晚体检" || edges[0].ToRef != "demo.target" {
		t.Errorf("入口边投影不对: %+v", edges)
	}
	// cron 建源时就该算好首次触发时刻
	if got.NextFireAt == nil {
		t.Fatal("cron 源应有 nextFireAt")
	}
	if *got.NextFireAt <= got.CreatedAt {
		t.Errorf("nextFireAt 应在创建之后: next=%d created=%d", *got.NextFireAt, got.CreatedAt)
	}

	// 列表
	list, err := db.ListSources(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应 1 个源, got %d", len(list))
	}

	// 按名字取（触发路径的常态）
	byName, err := db.GetSourceByName(ctx, nsID, src.Name)
	if err != nil {
		t.Fatalf("get source by name: %v", err)
	}
	if byName.ID != src.ID {
		t.Errorf("按名字取回的源不对: %s vs %s", byName.ID, src.ID)
	}

	// 删除（v4 是 CAS 入口：必须带当前 resourceVersion）
	if err := db.DeleteSource(ctx, nsID, src.ID, got.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSource(ctx, nsID, src.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("删后应 ErrSourceNotFound, got %v", err)
	}
	// 删源必须同时清掉入口边投影（否则路由会指向一个不存在的入口）
	if edges, eerr := db.ListEdges(ctx, nsID); eerr != nil {
		t.Fatalf("list edges: %v", eerr)
	} else if len(edges) != 0 {
		t.Errorf("删源后入口边应清空, got %+v", edges)
	}
}

func TestSourceValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.t2")

	// 零出口：A-continue 实测 v4 **不拒**（checkSourceTargetsOn → sourcespec.ValidateTargets
	// 对空列表返回 nil，`internal/store/source.go:266` / `internal/sourcespec/config.go:210`）。
	// v3 断言过「没有出口 → 拒绝（否则事件永远派不出去）」，v4 目前没有这道闸门；
	// 已在交付报告里作为产品缺口上报，本文件暂不钉住该行为（不弱断言、也不假装它存在）。
	var err error

	// 未知 kind
	err = db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "怪种类", Kind: "nope",
		Targets: []sourcespec.Target{{Capability: "demo.t2"}},
	})
	if !errors.Is(err, ErrInvalidSourceKind) {
		t.Errorf("应 ErrInvalidSourceKind, got %v", err)
	}

	// 非法 cron 表达式（创建期真的解析一遍）
	err = db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "坏表达式", Kind: SourceKindCron,
		Config:  json.RawMessage(`{"expr":"61 * * * *"}`),
		Targets: []sourcespec.Target{{Capability: "demo.t2"}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Errorf("应 ErrInvalidSourceConfig, got %v", err)
	}

	// 出口引用必须可解析（v4 的 targets 是引用，不是 id）
	err = db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "指向空气", Kind: SourceKindManual,
		Targets: []sourcespec.Target{{Capability: "does.not.exist"}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) && !strings.Contains(err.Error(), "不存在") {
		t.Errorf("出口引用不存在应被拒, got %v", err)
	}

	// 重名
	ok := &Source{NamespaceID: nsID, Name: "重名", Kind: SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{Capability: "demo.t2"}}}
	if err := db.CreateSource(ctx, ok); err != nil {
		t.Fatal(err)
	}
	dup := &Source{NamespaceID: nsID, Name: "重名", Kind: SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{Capability: "demo.t2"}}}
	if err := db.CreateSource(ctx, dup); !errors.Is(err, ErrSourceNameTaken) {
		t.Errorf("重名应 ErrSourceNameTaken, got %v", err)
	}
}

func TestSourceWebhookToken(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.t3")

	src := &Source{
		NamespaceID: nsID, Name: "hook", Kind: SourceKindWebhook,
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.t3"}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	wc := sourcespec.ParseConfig(SourceKindWebhook, src.Config)
	if wc.Token == "" {
		t.Fatal("webhook 源应自动生成 token")
	}
	if !strings.HasPrefix(wc.Token, "src_") {
		t.Errorf("token 形状应是 src_<随机>，实际 %q", wc.Token)
	}
	// 按 token 找源
	found, err := db.LookupHook(ctx, wc.Token)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != src.ID {
		t.Errorf("按 token 找回的源不对: %s vs %s", found.ID, src.ID)
	}
	// 空 token 一律 NotFound（不区分「没这个 token」与「源停用」）
	if _, err := db.LookupHook(ctx, "   "); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("空 token 应 ErrSourceNotFound, got %v", err)
	}

	// 停用后 token 不再有效（停用的源不该接受事件）。
	// v4 的 UpdateSource 是 CAS + PATCH 语义：省略 config = 保留（含 token）。
	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Enabled = false
	cur.Config = nil
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("停用源: %v", err)
	}
	after, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Fatal("停用没生效")
	}
	// token 必须原样保留（PATCH 省略 config 不该换掉 hook URL）
	if got := sourcespec.ParseConfig(SourceKindWebhook, after.Config).Token; got != wc.Token {
		t.Errorf("停用顺手换了 token: %q → %q", wc.Token, got)
	}
	if _, err := db.LookupHook(ctx, wc.Token); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("停用后 token 应失效, got %v", err)
	}
}

func TestSourceKindImmutable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.t4")

	src := &Source{NamespaceID: nsID, Name: "keep", Kind: SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{Capability: "demo.t4"}}}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 试图把 manual 改成 cron
	cur.Kind = SourceKindCron
	cur.Config = json.RawMessage(`{"expr":"0 3 * * *"}`)
	err = db.UpdateSource(ctx, cur)
	if !errors.Is(err, ErrSourceKindImmutable) {
		t.Fatalf("kind 不可改：应 ErrSourceKindImmutable, got %v", err)
	}
	after, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Kind != SourceKindManual {
		t.Errorf("被拒的改 kind 还是生效了: %s", after.Kind)
	}
}

func TestDueCronSources(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.t5")

	src := cronSrc(nsID, "due", "0 3 * * *", "demo.t5")
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextFireAt == nil {
		t.Fatal("cron 源应有 nextFireAt")
	}

	// 还没到点 → 不应被取出
	if due, derr := db.ListDueSources(ctx, *got.NextFireAt-60, 10); derr != nil {
		t.Fatalf("list due: %v", derr)
	} else if len(due) != 0 {
		t.Errorf("未到点不该取出, got %d", len(due))
	}
	// 到点 → 应被取出
	due, err := db.ListDueSources(ctx, *got.NextFireAt, 10)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 || due[0].ID != src.ID {
		t.Errorf("到点应取出 1 个（%s）, got %+v", src.ID, due)
	}

	// 触发后把 next_fire_at 往后推：同一个时刻不该再取到（防重复触发）
	next := *got.NextFireAt + 86400
	if err := db.MarkSourceFired(ctx, src.ID, *got.NextFireAt, next); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	if again, derr := db.ListDueSources(ctx, *got.NextFireAt, 10); derr != nil {
		t.Fatalf("list due after fire: %v", derr)
	} else if len(again) != 0 {
		t.Errorf("已触发过的源在同一时刻不该再取出, got %d", len(again))
	}
	// 停用的源不该被调度
	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Enabled = false
	cur.Config = nil
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("停用: %v", err)
	}
	if due, derr := db.ListDueSources(ctx, next+86400, 10); derr != nil {
		t.Fatalf("list due disabled: %v", derr)
	} else if len(due) != 0 {
		t.Errorf("停用的源不该被调度, got %d", len(due))
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
	sourceFixtureCap(t, db, nsID, "demo.reserved")

	for _, name := range []string{"route", "  route  "} {
		err := db.CreateSource(ctx, &Source{
			NamespaceID: nsID, Name: name, Kind: SourceKindManual, Enabled: true,
			Targets: []sourcespec.Target{{Capability: "demo.reserved"}},
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
		Targets: []sourcespec.Target{{Capability: "demo.reserved"}},
	}); err != nil {
		t.Fatalf("名字只是含有 route 的源被误伤: %v", err)
	}
}

// TestUpdateSourceRejectsReservedName：v4 里改名这条路**整体关闭**——
// 源名是入口边左端（路由与事件流都按名字 join），改名等于静默断线，
// 所以 UpdateSource 对任何改名都返回 ErrSourceNameImmutable（比只拦 route 更强）。
// 含 route 的名字照旧可以在**创建**时避开（见上一条）。
func TestUpdateSourceRejectsReservedName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.rename")

	src := &Source{
		NamespaceID: nsID, Name: "原来的名字", Kind: SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{Capability: "demo.rename"}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}

	for _, name := range []string{"route", "  route  ", "route 备份"} {
		cur, err := db.GetSource(ctx, nsID, src.ID)
		if err != nil {
			t.Fatalf("get source: %v", err)
		}
		cur.Name = name
		err = db.UpdateSource(ctx, cur)
		if !errors.Is(err, ErrSourceNameImmutable) {
			t.Fatalf("UpdateSource(name=%q) 应 ErrSourceNameImmutable, got %v", name, err)
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

	// 真正可改的东西照常能改（不能因为守名字把别的字段也锁死）
	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cur.DisplayName = "改个显示名"
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("改显示名被误伤: %v", err)
	}
	after, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if after.DisplayName != "改个显示名" {
		t.Fatalf("正常改名（显示名）没生效：%q", after.DisplayName)
	}
}

// TestSourceConfigPayloadOnlyForKeepingKinds：payload 的保留规则按 kind 分——
// cron/manual 保留（静态打底数据），webhook 直接丢弃（载荷来自投递体）。
func TestSourceConfigPayloadOnlyForKeepingKinds(t *testing.T) {
	// 1) webhook：非对象 payload 也**不**报错（它本就会被丢掉），token 原样保留
	out, err := sourcespec.NormalizeConfig(SourceKindWebhook, json.RawMessage(`{"token":"src_x","payload":[1,2]}`))
	if err != nil {
		t.Fatalf("webhook 不该因非对象 payload 被拒（它本就会被丢弃）: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("webhook 输出 config 不是对象: %v (%s)", err, out)
	}
	if raw, ok := fields["payload"]; ok {
		t.Errorf("webhook 输出不该保留 payload, got %s", raw)
	}
	if got := sourcespec.ParseConfig(SourceKindWebhook, out).Token; got != "src_x" {
		t.Errorf("token 应原样保留, got %q", got)
	}

	// 2) cron 仍拒：它是会保留 payload 的 kind，非对象打底数据不许进库
	if _, err := sourcespec.NormalizeConfig(SourceKindCron,
		json.RawMessage(`{"expr":"0 3 * * *","payload":[1,2]}`)); !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("cron 的非对象 payload 应仍被拒, got %v", err)
	}

	// 3) manual：合法对象 payload 原样保留（往返）
	out, err = sourcespec.NormalizeConfig(SourceKindManual, json.RawMessage(`{"payload":{"a":1}}`))
	if err != nil {
		t.Fatalf("manual 的对象 payload 不该被拒: %v", err)
	}
	if p := sourcespec.ParseConfig(SourceKindManual, out).Payload; fmt.Sprint(p["a"]) != "1" {
		t.Errorf("manual payload 往返不一致: %+v (%s)", p, out)
	}

	// 4) 落到库上：历史行里残留的非对象 payload 不该挡住任何一次 PATCH
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.payload-kind")
	src := &Source{
		NamespaceID: nsID, Name: "带坏 payload 的钩子", Kind: SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"src_x","payload":[1,2]}`),
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.payload-kind"}},
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
		if p := sourcespec.ParseConfig(SourceKindWebhook, got.Config).Payload; p != nil {
			t.Errorf("%s: 解析出的 payload 应为 nil, got %+v", when, p)
		}
	}
	assertNoStoredPayload("建源后")

	// 模拟历史行：老版本把非对象 payload 写进了 webhook 的 config
	if _, err := db.ExecContext(ctx,
		`UPDATE sources SET config = ? WHERE namespace_id = ? AND id = ?`,
		`{"token":"src_x","payload":[1,2]}`, nsID, src.ID); err != nil {
		t.Fatalf("造历史行: %v", err)
	}

	// PATCH 语义：拿到带历史坏 payload 的 config 再存回，不该 400
	legacy, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy.DisplayName = "改个名不该 400"
	if err := db.UpdateSource(ctx, legacy); err != nil {
		t.Fatalf("改 webhook 源不该被历史坏 payload 挡住: %v", err)
	}
	assertStored := func() {
		t.Helper()
		got, err := db.GetSource(ctx, nsID, src.ID)
		if err != nil {
			t.Fatalf("取源: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got.Config, &fields); err != nil {
			t.Fatalf("config 不是对象: %v (%s)", err, got.Config)
		}
		if raw, ok := fields["payload"]; ok {
			t.Errorf("PATCH 之后库里仍留着 payload, got %s", raw)
		}
	}
	assertStored()
}

// TestSourceConfigRoundTrip 建源/改源都不许把已声明的 config 字段抹掉。
//
// v3 的 vars/resultSchema 在 v4 里被移除（参数改由 target.params 模板表达、
// 契约改叫 payloadSchema），所以这里钉住的是 v4 的字段集：
// payload / payloadSchema / sample（cron、manual）与 token/secret/payloadSchema（webhook）。
func TestSourceConfigRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.vars")

	rawSchema := `{"type":"object","required":["environment"]}`

	// cron：payload + payloadSchema + sample 都要活过建源
	src := &Source{
		NamespaceID: nsID, Name: "带变量的定时源", Kind: SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *",
			"payload":{"env":"prod"},
			"payloadSchema":` + rawSchema + `,
			"sample":{"env":"staging"}}`),
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.vars"}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("建源: %v", err)
	}
	got, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	cc := sourcespec.ParseConfig(SourceKindCron, got.Config)
	if cc.Expr != "0 3 * * *" || cc.TZ != "Local" {
		t.Errorf("cron 规范化结果不对: %+v", cc)
	}
	if cc.Payload["env"] != "prod" {
		t.Errorf("建源后 payload 往返不一致: %+v", cc.Payload)
	}
	if string(cc.PayloadSchema) != rawSchema {
		t.Errorf("建源后 payloadSchema 往返不一致: %s", cc.PayloadSchema)
	}
	if cc.Sample["env"] != "staging" {
		t.Errorf("建源后 sample 往返不一致: %+v", cc.Sample)
	}

	// PATCH 只改 expr（模拟 handler：拿当前 config 改一个键再存回）：
	// payload / payloadSchema / sample 必须仍在。
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
	cc = sourcespec.ParseConfig(SourceKindCron, after.Config)
	if cc.Expr != "0 4 * * *" {
		t.Errorf("改 expr 没生效: %+v", cc)
	}
	if cc.Payload["env"] != "prod" {
		t.Errorf("改 expr 后 payload 丢了: %+v", cc.Payload)
	}
	if string(cc.PayloadSchema) != rawSchema {
		t.Errorf("改 expr 后 payloadSchema 丢了: %s", cc.PayloadSchema)
	}
	if cc.Sample["env"] != "staging" {
		t.Errorf("改 expr 后 sample 丢了: %+v", cc.Sample)
	}

	// manual：payload 同样要保留
	msrc := &Source{
		NamespaceID: nsID, Name: "手动带参", Kind: SourceKindManual,
		Config:  json.RawMessage(`{"payload":{"sha":"deadbeef"}}`),
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.vars"}},
	}
	if err := db.CreateSource(ctx, msrc); err != nil {
		t.Fatalf("建 manual 源: %v", err)
	}
	mgot, err := db.GetSource(ctx, nsID, msrc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := sourcespec.ParseConfig(SourceKindManual, mgot.Config).Payload; p["sha"] != "deadbeef" {
		t.Errorf("manual payload 往返不一致: %+v", p)
	}

	// 未知键在 v4 是**创建期错误**（closed field set），不再有「静默丢键」
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "带旧字段", Kind: SourceKindCron,
		Config:  json.RawMessage(`{"expr":"0 3 * * *","vars":[{"name":"a"}]}`),
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.vars"}},
	}); !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("未知 config 键（v3 的 vars）应被拒, got %v", err)
	}
}

// TestSourceTargetsParamsValidation 出口的参数模板按 `$(payload.路径)` 校验：
// 坏路径/缺闭合在**配置期**就拒（不是等投递时静默丢参数）。
func TestSourceTargetsParamsValidation(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   string // "" = 合法
	}{
		{"常量", map[string]any{"issue": "always"}, ""},
		{"引用整个 payload", map[string]any{"x": "$(payload)"}, ""},
		{"引用点号路径", map[string]any{"x": "$(payload.pull_request.title)"}, ""},
		{"数组下标", map[string]any{"x": "$(payload.commits.0.sha)"}, ""},
		{"混写常量与引用", map[string]any{"x": "sha=$(payload.head)"}, ""},
		{"嵌套对象里的引用", map[string]any{"outer": map[string]any{"inner": "$(payload.a)"}}, ""},
		{"路径段带连字符", map[string]any{"x": "$(payload.a-b)"}, "路径段"},
		{"路径段为空", map[string]any{"x": "$(payload.a..b)"}, "路径段"},
		{"引用 vars", map[string]any{"x": "$(vars.a)"}, "不能引用"},
		{"引用 params", map[string]any{"x": "$(params.a)"}, "不能引用"},
		{"像 payload 却不是合法路径", map[string]any{"x": "$(payloadx.a)"}, "不是合法的 payload 路径"},
		{"缺少闭合", map[string]any{"x": "$(payload.a"}, "没有闭合"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := sourcespec.ValidateParamsTemplate(c.params)
			if c.want == "" {
				if err != nil {
					t.Fatalf("应合法, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒（%s）", c.want)
			}
			if !errors.Is(err, ErrInvalidSourceConfig) {
				t.Fatalf("错误不可判（server 会变成 500）: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误消息里应有 %q, got %v", c.want, err)
			}
		})
	}

	// 落到 CreateSource：坏模板 → 可判的哨兵错误，且不建出半个源
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.badtpl")
	err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "坏模板", Kind: SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{
			Capability: "demo.badtpl",
			Params:     map[string]any{"issue": "$(payload.a-b)"},
		}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("CreateSource 应透出 ErrInvalidSourceConfig, got %v", err)
	}
	if list, _ := db.ListSources(ctx, nsID); len(list) != 0 {
		t.Fatalf("被拒的源还是落库了: %d 个", len(list))
	}
}

// TestSourcePayloadSchemaRoundTrip 是 payloadSchema 的「存什么读什么」单测。
//
// 连**字节**都比对：NormalizeConfig 明确不做重编码，用户写的 schema 与库里存的
// 必须一字不差，否则「我配的和系统看到的不一样」永远查不清。
func TestSourcePayloadSchemaRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.schema")

	rawSchema := `{"type":"object","required":["pull_request"],"properties":{"pull_request":{"type":"object","properties":{"title":{"type":"string"}}}}}`
	for _, kind := range SourceKinds() {
		cfg := `{"payloadSchema":` + rawSchema + `}`
		switch kind {
		case SourceKindCron:
			cfg = `{"expr":"0 3 * * *","payloadSchema":` + rawSchema + `}`
		case SourceKindWebhook:
			cfg = `{"token":"src_tok","payloadSchema":` + rawSchema + `}`
		}
		src := &Source{
			NamespaceID: nsID, Name: "带契约的源-" + kind, Kind: kind,
			Config: json.RawMessage(cfg), Enabled: true,
			Targets: []sourcespec.Target{{Capability: "demo.schema"}},
		}
		if err := db.CreateSource(ctx, src); err != nil {
			t.Fatalf("%s 建源: %v", kind, err)
		}
		got, err := db.GetSource(ctx, nsID, src.ID)
		if err != nil {
			t.Fatalf("%s 取源: %v", kind, err)
		}
		parsed := sourcespec.ParseConfig(kind, got.Config)
		if string(parsed.PayloadSchema) != rawSchema {
			t.Errorf("%s payloadSchema 往返不一致:\n got %s\nwant %s", kind, parsed.PayloadSchema, rawSchema)
		}
		// 别的字段不能被这次改动弄丢
		switch kind {
		case SourceKindCron:
			if parsed.Expr != "0 3 * * *" {
				t.Errorf("%s cron 配置坏了: %s", kind, got.Config)
			}
		case SourceKindWebhook:
			if parsed.Token != "src_tok" {
				t.Errorf("%s token 丢了: %s", kind, got.Config)
			}
		}
	}
}

// TestSourcePayloadSchemaValidation 是配置期校验：必须是 JSON 对象；非法直接拒。
func TestSourcePayloadSchemaValidation(t *testing.T) {
	ok := []string{
		`{"type":"object","required":["title"]}`,
		`{}`,   // 空对象 = 不约束任何字段（合法，等价于没声明）
		`null`, // 显式 null = 清空/没有契约
	}
	for _, s := range ok {
		cfg := json.RawMessage(`{"expr":"0 3 * * *","payloadSchema":` + s + `}`)
		out, err := sourcespec.NormalizeConfig(SourceKindCron, cfg)
		if err != nil {
			t.Fatalf("NormalizeConfig 应接受 payloadSchema=%s: %v", s, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out, &fields); err != nil {
			t.Fatalf("输出 config 不是对象: %v (%s)", err, out)
		}
		// null 与缺省同义：输出里不留 payloadSchema:null 这种「伪声明」
		if s == `null` {
			if _, has := fields["payloadSchema"]; has {
				t.Errorf("显式 null 不该在输出里留键: %s", out)
			}
		} else if string(fields["payloadSchema"]) != s {
			t.Errorf("payloadSchema 没被原样保留: %s, want %s", fields["payloadSchema"], s)
		}
	}

	bad := []string{`[1,2]`, `"x"`, `1`, `true`}
	for _, s := range bad {
		_, err := sourcespec.NormalizeConfig(SourceKindCron,
			json.RawMessage(`{"expr":"0 3 * * *","payloadSchema":`+s+`}`))
		if !errors.Is(err, ErrInvalidSourceConfig) {
			t.Fatalf("payloadSchema=%s 应被拒且错误可判（server 会变成 500）: %v", s, err)
		}
		if !strings.Contains(err.Error(), "payloadSchema") {
			t.Errorf("错误消息里要有字段名 payloadSchema, got %v", err)
		}
	}

	// 落到 CreateSource 也仍然是可判的哨兵错误，且不会建出半个源
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.badschema")
	err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "坏契约", Kind: SourceKindManual, Enabled: true,
		Config:  json.RawMessage(`{"payloadSchema":[1,2]}`),
		Targets: []sourcespec.Target{{Capability: "demo.badschema"}},
	})
	if !errors.Is(err, ErrInvalidSourceConfig) {
		t.Fatalf("CreateSource 应透出 ErrInvalidSourceConfig, got %v", err)
	}
	if list, _ := db.ListSources(ctx, nsID); len(list) != 0 {
		t.Fatalf("被拒的源还是落库了: %d 个", len(list))
	}
}

// TestUpdateSourceKeepsPayloadSchema 改源（UpdateSource）不许把 payloadSchema 抹掉。
func TestUpdateSourceKeepsPayloadSchema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	sourceFixtureCap(t, db, nsID, "demo.updschema")

	rawSchema := `{"type":"object","required":["title"]}`
	src := &Source{
		NamespaceID: nsID, Name: "改配置的源", Kind: SourceKindCron,
		Config:  json.RawMessage(`{"expr":"0 3 * * *","payloadSchema":` + rawSchema + `}`),
		Enabled: true, Targets: []sourcespec.Target{{Capability: "demo.updschema"}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Config = json.RawMessage(`{"expr":"0 4 * * *","payloadSchema":` + rawSchema + `}`)
	if err := db.UpdateSource(ctx, got); err != nil {
		t.Fatalf("改源: %v", err)
	}
	again, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parsed := sourcespec.ParseConfig(SourceKindCron, again.Config); string(parsed.PayloadSchema) != rawSchema {
		t.Errorf("改源后 payloadSchema 丢了: %s (%s)", parsed.PayloadSchema, again.Config)
	}
	if again.ResourceVersion != got.ResourceVersion {
		t.Errorf("UpdateSource 应把新 resourceVersion 回写到入参：%d → %d", got.ResourceVersion, again.ResourceVersion)
	}
}
