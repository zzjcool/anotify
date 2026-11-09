package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// edgeRefsFrom 取某个源的入口边右端（按字面排序）。
func edgeRefsFrom(t *testing.T, db *store.DB, nsID, sourceName string) []string {
	t.Helper()
	edges, err := db.EdgesFrom(context.Background(), nsID, true, sourceName)
	if err != nil {
		t.Fatalf("edges from %s: %v", sourceName, err)
	}
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.ToRef)
	}
	return out
}

// TestSourceCronConfig 验 cron 三件事：表达式创建时真解析、tz 默认 Local、排好下次触发。
func TestSourceCronConfig(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-cron")

	s := &store.Source{
		NamespaceID: ns,
		Name:        "nightly",
		Kind:        store.SourceKindCron,
		Enabled:     true,
		Config:      json.RawMessage(`{"expr":"0 3 * * *"}`),
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create cron source: %v", err)
	}
	got, err := db.GetSource(ctx, ns, s.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cfg := sourcespec.ParseConfig(got.Kind, got.Config)
	if cfg.TZ != "Local" {
		t.Fatalf("tz 应归一为 Local，得到 %q", cfg.TZ)
	}
	if got.NextFireAt == nil {
		t.Fatalf("cron 源建源时就该算好 nextFireAt")
	}
	if got.Targets == nil || len(got.Targets) != 0 {
		t.Fatalf("空 targets 合法且必须是 []：%v", got.Targets)
	}

	// 永不触发的表达式当场拒（不等触发才发现）。
	bad := &store.Source{NamespaceID: ns, Name: "bad-cron", Kind: store.SourceKindCron,
		Config: json.RawMessage(`{"expr":"61 * * * *"}`)}
	if err := db.CreateSource(ctx, bad); !errors.Is(err, store.ErrInvalidSourceConfig) {
		t.Fatalf("非法 cron 表达式应 ErrInvalidSourceConfig，得到 %v", err)
	}
	// 未知字段拒。
	unknown := &store.Source{NamespaceID: ns, Name: "unknown-field", Kind: store.SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *","paylodSchema":{}}`)}
	if err := db.CreateSource(ctx, unknown); !errors.Is(err, store.ErrInvalidSourceConfig) {
		t.Fatalf("未知 config 字段应被拒，得到 %v", err)
	}
	// payloadSchema 必须是对象。
	badSchema := &store.Source{NamespaceID: ns, Name: "bad-schema", Kind: store.SourceKindCron,
		Config: json.RawMessage(`{"expr":"0 3 * * *","payloadSchema":[]}`)}
	if err := db.CreateSource(ctx, badSchema); !errors.Is(err, store.ErrInvalidSourceConfig) {
		t.Fatalf("非对象 payloadSchema 应被拒，得到 %v", err)
	}
}

// TestSourceWebhookToken 验 token 生成、hook 查找、停用即 404 语义。
func TestSourceWebhookToken(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-hook")

	s := &store.Source{
		NamespaceID: ns,
		Name:        "hook",
		Kind:        store.SourceKindWebhook,
		Enabled:     true,
		Config:      json.RawMessage(`{"payload":{"ignored":true},"payloadSchema":{"type":"object"}}`),
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create webhook source: %v", err)
	}
	cfg := sourcespec.ParseConfig(store.SourceKindWebhook, s.Config)
	if cfg.Token == "" {
		t.Fatalf("空 token 应由服务端生成")
	}
	if cfg.Payload != nil {
		t.Fatalf("webhook 的 config.payload 应被丢弃：%v", cfg.Payload)
	}
	found, err := db.LookupHook(ctx, cfg.Token)
	if err != nil {
		t.Fatalf("lookup hook: %v", err)
	}
	if found.ID != s.ID || found.NamespaceID != ns {
		t.Fatalf("hook 应按 token 找到源：%+v", found)
	}
	if _, err := db.LookupHook(ctx, "src_nope"); !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("未知 token 应 ErrSourceNotFound，得到 %v", err)
	}

	// 停用后：token 还在，但查找一律 NotFound（不区分「不存在」与「已停用」）。
	off := *s
	off.Enabled = false
	if err := db.UpdateSource(ctx, &off); err != nil {
		t.Fatalf("disable source: %v", err)
	}
	if _, err := db.LookupHook(ctx, cfg.Token); !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("停用的源应返回 NotFound，得到 %v", err)
	}
}

// TestSourcePatchKeepsCredentials 验 PATCH 语义：省略 token/secret = 保留已有。
func TestSourcePatchKeepsCredentials(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-patch")

	s := &store.Source{
		NamespaceID: ns, Name: "hook", Kind: store.SourceKindWebhook, Enabled: true,
		Config: json.RawMessage(`{"secret":"s3cr3t"}`),
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := sourcespec.ParseConfig(store.SourceKindWebhook, s.Config)
	if before.Token == "" || before.Secret != "s3cr3t" {
		t.Fatalf("建源后应有 token + secret：%+v", before)
	}
	// 只改 payloadSchema：token/secret 都不在 PATCH 里。
	patch := &store.Source{
		NamespaceID: ns, ID: s.ID, Kind: store.SourceKindWebhook, Enabled: true,
		Config: json.RawMessage(`{"payloadSchema":{"type":"object","required":["environment"]}}`),
	}
	if err := db.UpdateSource(ctx, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	after := sourcespec.ParseConfig(store.SourceKindWebhook, patch.Config)
	if after.Token != before.Token || after.Secret != "s3cr3t" {
		t.Fatalf("省略凭证的 PATCH 必须保留原值：token %q→%q secret %q→%q",
			before.Token, after.Token, before.Secret, after.Secret)
	}
	if len(after.PayloadSchema) == 0 {
		t.Fatalf("payloadSchema 应被写入")
	}
	// 显式给新 token = 轮换（旧 URL 立即失效）。
	rotate := &store.Source{
		NamespaceID: ns, ID: s.ID, Kind: store.SourceKindWebhook, Enabled: true,
		Config: json.RawMessage(`{"token":"src_rotated"}`),
	}
	if err := db.UpdateSource(ctx, rotate); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got := sourcespec.ParseConfig(store.SourceKindWebhook, rotate.Config).Token; got != "src_rotated" {
		t.Fatalf("显式 token 应参与比较/替换，得到 %q", got)
	}
	if _, err := db.LookupHook(ctx, before.Token); !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("轮换后旧 token 应失效，得到 %v", err)
	}
}

// TestSourceKindAndNameImmutable 验 kind 与 name 创建后不可改。
func TestSourceKindAndNameImmutable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-immutable")

	s := &store.Source{
		NamespaceID: ns, Name: "manual-one", Kind: store.SourceKindManual, Enabled: true,
		Config: json.RawMessage(`{"payload":{"a":1}}`),
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	same := &store.Source{NamespaceID: ns, ID: s.ID, Name: "manual-one", Kind: store.SourceKindCron,
		Enabled: true, Config: json.RawMessage(`{"expr":"0 3 * * *"}`)}
	if err := db.UpdateSource(ctx, same); !errors.Is(err, store.ErrSourceKindImmutable) {
		t.Fatalf("改 kind 应 ErrSourceKindImmutable，得到 %v", err)
	}
	renamed := &store.Source{NamespaceID: ns, ID: s.ID, Name: "manual-two", Kind: store.SourceKindManual,
		Enabled: true, Config: json.RawMessage(`{"payload":{"a":2}}`)}
	if err := db.UpdateSource(ctx, renamed); !errors.Is(err, store.ErrSourceNameImmutable) {
		t.Fatalf("改名应 ErrSourceNameImmutable，得到 %v", err)
	}
	// 重名建源被拒。
	dup := &store.Source{NamespaceID: ns, Name: "manual-one", Kind: store.SourceKindManual}
	if err := db.CreateSource(ctx, dup); !errors.Is(err, store.ErrSourceNameTaken) {
		t.Fatalf("重名应 ErrSourceNameTaken，得到 %v", err)
	}
	// 保留来源名被拒（否则它的事件会在事件流里静默消失）。
	reserved := &store.Source{NamespaceID: ns, Name: store.EventPruneSource, Kind: store.SourceKindManual}
	if err := db.CreateSource(ctx, reserved); !errors.Is(err, store.ErrReservedEventSource) {
		t.Fatalf("保留来源名应被拒，得到 %v", err)
	}
}

// TestSourceTargetsRebuildEdges 验 targets 变更**同事务**重建入口边，且模板语法被校验。
func TestSourceTargetsRebuildEdges(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-targets")
	analyze := createCap(t, db, testCap(ns, "analyze", 1, nil))
	notify := createCap(t, db, testCap(ns, "notify", 1, nil))

	s := &store.Source{
		NamespaceID: ns, Name: "fanout", Kind: store.SourceKindManual, Enabled: true,
		Config: json.RawMessage(`{"payload":{"environment":"prod"}}`),
		Targets: []sourcespec.Target{
			{Capability: "analyze", Params: map[string]any{"env": "$(payload.environment)"}},
		},
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := edgeRefsFrom(t, db, ns, "fanout"); len(got) != 1 || got[0] != "analyze" {
		t.Fatalf("入口边 = %v，want [analyze]", got)
	}
	// 换 targets：旧的边必须整条消失（否则会留下派不出去的残留）。
	upd := &store.Source{
		NamespaceID: ns, ID: s.ID, Name: "fanout", Kind: store.SourceKindManual, Enabled: true,
		Config: json.RawMessage(`{"payload":{"environment":"prod"}}`),
		Targets: []sourcespec.Target{
			{Capability: "notify", Params: map[string]any{"msg": "deploy $(payload.environment)"}},
		},
	}
	if err := db.UpdateSource(ctx, upd); err != nil {
		t.Fatalf("update targets: %v", err)
	}
	if got := edgeRefsFrom(t, db, ns, "fanout"); len(got) != 1 || got[0] != "notify" {
		t.Fatalf("替换后入口边 = %v，want [notify]", got)
	}
	// 行上的 targets 与边一致（同一份事实）。
	got, err := db.GetSource(ctx, ns, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Capability != "notify" {
		t.Fatalf("sources.targets 往返失真：%+v", got.Targets)
	}
	// 目标能力不存在 → 拒（不静默忽略）。
	missing := &store.Source{
		NamespaceID: ns, Name: "missing-target", Kind: store.SourceKindManual,
		Targets: []sourcespec.Target{{Capability: "ghost"}},
	}
	if err := db.CreateSource(ctx, missing); err == nil {
		t.Fatalf("不存在的目标能力应被拒")
	}
	// 模板语法：被禁根名/坏路径创建期拒。
	for _, params := range []map[string]any{
		{"env": "$(vars.environment)"},
		{"env": "$(params.environment)"},
		{"path": "$(payload.a-b)"},
		{"path": "$(payload..x)"},
	} {
		bad := &store.Source{
			NamespaceID: ns, Name: "bad-template", Kind: store.SourceKindManual,
			Targets: []sourcespec.Target{{Capability: analyze.Name, Params: params}},
		}
		if err := db.CreateSource(ctx, bad); !errors.Is(err, store.ErrInvalidSourceConfig) {
			t.Fatalf("模板 %v 应在创建期被拒，得到 %v", params, err)
		}
	}
	_ = notify
}

// TestSourceListAndDelete 验列表空 []、删除连带清边。
func TestSourceListAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "src-list")

	list, err := db.ListSources(ctx, ns)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("空列表必须是 []：%v %v", list, err)
	}
	createCap(t, db, testCap(ns, "analyze", 1, nil))
	s := &store.Source{
		NamespaceID: ns, Name: "one", Kind: store.SourceKindManual, Enabled: true,
		Targets: []sourcespec.Target{{Capability: "analyze"}},
	}
	if err := db.CreateSource(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.DeleteSource(ctx, ns, s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetSource(ctx, ns, s.ID); !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("删除后应 NotFound，得到 %v", err)
	}
	if got := edgeRefsFrom(t, db, ns, "one"); len(got) != 0 {
		t.Fatalf("删除源必须清掉入口边，剩 %v", got)
	}
}
