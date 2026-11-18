package server

// retention_seed_test.go 覆盖 #005：`ANOTIFY_EVENT_RETENTION_DAYS` 从「只进启动日志」变成
// 「空库首次启动播种 system_config」，并且启动日志改报**生效值**（带 source）。
//
// 这里测的是接线（`reportEventRetention`），store 侧的播种语义有自己的单测
// （`internal/store/config_seed_test.go` 的 TestSeedSystemConfigIfEmpty）。
// 两层都必要：
//   - store 侧证明「只播种、不覆盖、拒绝非法值」这些事实本身成立；
//   - 这里证明「启动那一刻确实调用了它」，且 **日志里的数字是从库里读回来的生效值**。
//     后者是这个 bug 的核心：改之前日志打的是 env，所以一个「库里有 30 天、env 写 90」的
//     部署会在日志里说 90 天而实际删 30 天。断言只看日志 = 断言运维看到的那一面。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// newRetentionDBForTest 开一个真实 SQLite（:memory:，单连接）用来测保留期接线。
func newRetentionDBForTest(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// captureRetentionLogs 把 slog 输出抓到缓冲区并返回解析成 JSON 对象的闭包。
//
// 必须恢复 slog.Default：TestMain 装的 postclose 护栏（#259）也挂在 slog.Default 上，
// 不恢复就会让**本用例之后**的整包绕过护栏。
func captureRetentionLogs(t *testing.T) (*bytes.Buffer, func(event string) (map[string]any, bool)) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	find := func(event string) (map[string]any, bool) {
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("日志行不是 JSON（%v）：%q", err, line)
			}
			if m["event"] == event {
				return m, true
			}
		}
		return nil, false
	}
	return &buf, find
}

// mustLogEvent 取一条指定 event 的日志，没有就 Fatal（附完整输出，省得再跑一遍）。
func mustLogEvent(t *testing.T, buf *bytes.Buffer, find func(string) (map[string]any, bool), event string) map[string]any {
	t.Helper()
	m, ok := find(event)
	if !ok {
		t.Fatalf("启动日志里没有 event=%q：\n%s", event, buf.String())
	}
	return m
}

// TestEventRetentionDaysSet 钉住「env 是否被显式设置」这个布尔（与数值解析分开）。
//
// 为什么要单独的布尔：未设置与显式写 30 在数值上一样，但前者**不该写库**
// （保持「无行 = store 默认值」），后者要播种一次。用数值猜会把两者混成一个。
//
// 既有 TestEventRetentionDaysEnvParsing 的 4 条断言不受影响（本文件只新增，不改它）。
func TestEventRetentionDaysSet(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"未设置（空串，与 os.Getenv 对未设的返回值同形）", "", false},
		{"显式 0 = 永久保留", "0", true},
		{"显式默认值 30 也算设置过", "30", true},
		{"显式 90", "90", true},
		{"非法值（设置过，解析层回落默认）", "abc", true},
		{"前后空白仍是设置过", "  7  ", true},
		{"纯空白视为未设置", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", c.raw)
			if got := FromEnv().EventRetentionDaysSet; got != c.want {
				t.Fatalf("ANOTIFY_EVENT_RETENTION_DAYS=%q → EventRetentionDaysSet=%v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestReportEventRetentionWiring 钉住启动期接线：播种 + 报**生效值** + source 字段。
func TestReportEventRetentionWiring(t *testing.T) {
	ctx := context.Background()

	t.Run("env 显式 90 + 空库：播种并报 env_seed", func(t *testing.T) {
		t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "90")
		cfg := FromEnv() // 走真实解析，证明 env → 布尔 → 播种这条链是通的
		if !cfg.EventRetentionDaysSet {
			t.Fatal("env 显式设置时 EventRetentionDaysSet 必须是 true")
		}
		db := newRetentionDBForTest(t)
		buf, find := captureRetentionLogs(t)

		reportEventRetention(ctx, db, cfg)

		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get system config: %v", err)
		}
		// 播种口径：events 用 env，tasks/results 用 store 默认（env 只描述 events）。
		if got.RetentionEventTTLSec != 90*86400 {
			t.Fatalf("events TTL 应被播成 90 天（%d 秒），实际 %d", 90*86400, got.RetentionEventTTLSec)
		}
		if got.RetentionTaskTTLSec != store.DefaultRetentionTaskTTLSec ||
			got.RetentionResultTTLSec != store.DefaultRetentionResultTTLSec {
			t.Fatalf("task/result TTL 必须是 store 默认值：got %+v", got)
		}
		if got.ResourceVersion != 1 {
			t.Fatalf("播种行的 resourceVersion 必须是 1（首次 PUT 的前置条件），实际 %d", got.ResourceVersion)
		}

		seedLog := mustLogEvent(t, buf, find, "server.retention.seeded_from_env")
		if lvl := seedLog["level"]; lvl != "INFO" {
			t.Fatalf("seeded_from_env 应是 INFO，实际 %v", lvl)
		}
		if seedLog["events_retention_sec"] != float64(90*86400) {
			t.Fatalf("seeded_from_env 的 events_retention_sec = %v", seedLog["events_retention_sec"])
		}

		// 关键断言：主日志报的是**生效值**（从库里读回来的 90 天），不是 env 的回显。
		main := mustLogEvent(t, buf, find, "server.retention.configured")
		if main["source"] != eventRetentionSourceEnvSeed {
			t.Fatalf("source 应为 %q，实际 %v", eventRetentionSourceEnvSeed, main["source"])
		}
		if main["events_retention_sec"] != float64(90*86400) {
			t.Fatalf("events_retention_sec 应为 %d，实际 %v", 90*86400, main["events_retention_sec"])
		}
		if main["days"] != float64(90) {
			t.Fatalf("days 应由生效秒数反算成 90，实际 %v", main["days"])
		}
		// 文档引用的两个容量估算字段必须还在（#229 的运维口径）。
		if main["est_bytes_per_event"] == nil || main["est_bytes_per_event_1kb_payload"] == nil {
			t.Fatalf("容量估算字段丢了：%v", main)
		}
	})

	t.Run("env=0 + 空库：播种 0 并走 permanent 分支", func(t *testing.T) {
		t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "0")
		cfg := FromEnv()
		db := newRetentionDBForTest(t)
		buf, find := captureRetentionLogs(t)

		reportEventRetention(ctx, db, cfg)

		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get system config: %v", err)
		}
		if got.RetentionEventTTLSec != 0 {
			t.Fatalf("env=0 必须播成 0（永久保留），实际 %d", got.RetentionEventTTLSec)
		}

		perm := mustLogEvent(t, buf, find, "server.retention.permanent")
		if perm["source"] != eventRetentionSourceEnvSeed {
			t.Fatalf("source 应为 %q，实际 %v", eventRetentionSourceEnvSeed, perm["source"])
		}
		if perm["events_retention_sec"] != float64(0) {
			t.Fatalf("permanent 分支的 events_retention_sec 应为 0，实际 %v", perm["events_retention_sec"])
		}
		if perm["hint"] == nil || perm["hint"] == "" {
			t.Fatal("permanent 分支必须保留 hint（文档引用了它）")
		}
		if perm["est_bytes_per_event"] == nil {
			t.Fatal("permanent 分支必须保留 est_bytes_per_event")
		}
		if _, ok := find("server.retention.configured"); ok {
			t.Fatal("保留期 0 时不该出现 configured 分支")
		}
	})

	t.Run("已有行：env 不覆盖，报 env_ignored 且主日志用库里的值", func(t *testing.T) {
		db := newRetentionDBForTest(t)
		// 模拟「运维在控制台把保留期改成 90 天」（rv 1 → 2）。
		if err := db.PutSystemConfig(ctx, &store.SystemConfig{
			RetentionEventTTLSec:  90 * 86400,
			RetentionTaskTTLSec:   store.DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: store.DefaultRetentionResultTTLSec,
		}, 1); err != nil {
			t.Fatalf("put system config: %v", err)
		}
		// 重启时 env 里还是旧的 1 天 —— 它不能把 90 天改回去。
		cfg := Config{EventRetentionDays: 1, EventRetentionDaysSet: true}
		buf, find := captureRetentionLogs(t)

		reportEventRetention(ctx, db, cfg)

		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get system config: %v", err)
		}
		if got.RetentionEventTTLSec != 90*86400 {
			t.Fatalf("env 覆盖了已有行：events TTL = %d，want %d", got.RetentionEventTTLSec, 90*86400)
		}
		if got.ResourceVersion != 2 {
			t.Fatalf("忽略播种不应改动 resourceVersion（应仍为 2），实际 %d", got.ResourceVersion)
		}

		ignored := mustLogEvent(t, buf, find, "server.retention.env_ignored")
		if ignored["level"] != "WARN" {
			t.Fatalf("env_ignored 应是 WARN，实际 %v", ignored["level"])
		}
		if hint, _ := ignored["hint"].(string); !strings.Contains(hint, "PUT /v1/system/config") {
			t.Fatalf("env_ignored 的 hint 必须指向 PUT /v1/system/config，实际 %q", hint)
		}
		if _, ok := find("server.retention.seeded_from_env"); ok {
			t.Fatal("已有行时不该出现 seeded_from_env")
		}

		main := mustLogEvent(t, buf, find, "server.retention.configured")
		if main["source"] != eventRetentionSourceSystemConfig {
			t.Fatalf("source 应为 %q，实际 %v", eventRetentionSourceSystemConfig, main["source"])
		}
		// 这一条就是本 bug 的验收面：日志必须说 90 天（生效值），不能说 1 天（env）。
		if main["events_retention_sec"] != float64(90*86400) {
			t.Fatalf("events_retention_sec 应报库里的 %d 秒，实际 %v", 90*86400, main["events_retention_sec"])
		}
	})

	t.Run("env 未设 + 空库：不写库，报 default", func(t *testing.T) {
		t.Setenv("ANOTIFY_EVENT_RETENTION_DAYS", "")
		cfg := FromEnv()
		if cfg.EventRetentionDaysSet {
			t.Fatal("env 空串时 EventRetentionDaysSet 必须是 false")
		}
		db := newRetentionDBForTest(t)
		buf, find := captureRetentionLogs(t)

		reportEventRetention(ctx, db, cfg)

		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get system config: %v", err)
		}
		if got.RetentionEventTTLSec != store.DefaultRetentionEventTTLSec {
			t.Fatalf("未设置 env 时生效值应是 store 默认，实际 %d", got.RetentionEventTTLSec)
		}
		// 「没写库」的证据：此时再播种必须成功（= 表里确实没有行）。
		// 只断言 rv==1 不够 —— 播种行也写 rv=1。
		seeded, err := db.SeedSystemConfigIfEmpty(ctx, &store.SystemConfig{
			RetentionEventTTLSec:  7 * 86400,
			RetentionTaskTTLSec:   store.DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: store.DefaultRetentionResultTTLSec,
		})
		if err != nil {
			t.Fatalf("seed after report: %v", err)
		}
		if !seeded {
			t.Fatal("env 未设置时启动期不得写 system_config（表里本该没有行）")
		}
		if _, ok := find("server.retention.seeded_from_env"); ok {
			t.Fatal("env 未设置时不该出现 seeded_from_env")
		}
		if _, ok := find("server.retention.env_ignored"); ok {
			t.Fatal("env 未设置时不该出现 env_ignored")
		}

		main := mustLogEvent(t, buf, find, "server.retention.configured")
		if main["source"] != eventRetentionSourceDefault {
			t.Fatalf("source 应为 %q，实际 %v", eventRetentionSourceDefault, main["source"])
		}
		if main["events_retention_sec"] != float64(store.DefaultRetentionEventTTLSec) {
			t.Fatalf("events_retention_sec 应为 store 默认值，实际 %v", main["events_retention_sec"])
		}
	})

	t.Run("env 未设 + 上一次播种的行（rv=1 非默认值）：报 system_config", func(t *testing.T) {
		db := newRetentionDBForTest(t)
		// 上一次启动用 env=90 播的种：rv 仍是 1，值不是默认值。这次重启 env 已撤。
		if _, err := db.SeedSystemConfigIfEmpty(ctx, &store.SystemConfig{
			RetentionEventTTLSec:  90 * 86400,
			RetentionTaskTTLSec:   store.DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: store.DefaultRetentionResultTTLSec,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		buf, find := captureRetentionLogs(t)

		reportEventRetention(ctx, db, Config{})

		// 这条钉住 source 反推的边界：rv==1 不足以证明「没有行」，
		// 值不是默认值就说明它只可能来自库里（否则运维会以为「没人配过」而去改 env）。
		main := mustLogEvent(t, buf, find, "server.retention.configured")
		if main["source"] != eventRetentionSourceSystemConfig {
			t.Fatalf("source 应为 %q，实际 %v", eventRetentionSourceSystemConfig, main["source"])
		}
		if main["events_retention_sec"] != float64(90*86400) {
			t.Fatalf("events_retention_sec 应为 %d，实际 %v", 90*86400, main["events_retention_sec"])
		}
	})

	t.Run("播种失败不阻断启动", func(t *testing.T) {
		db := newRetentionDBForTest(t)
		buf, find := captureRetentionLogs(t)
		// 关掉库，逼播种与读配置都失败（等价于「启动早期库只读/损坏」）。
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		// 不许 panic / 不许 os.Exit：函数必须正常返回，剩下的启动流程继续。
		reportEventRetention(ctx, db, Config{EventRetentionDays: 90, EventRetentionDaysSet: true})

		failed := mustLogEvent(t, buf, find, "server.retention.seed_failed")
		if failed["level"] != "WARN" {
			t.Fatalf("seed_failed 应是 WARN，实际 %v", failed["level"])
		}
		if failed["error"] == nil {
			t.Fatal("seed_failed 必须带 error 字段")
		}
		// 读不到配置时仍要有一条可读的生效值日志（按 store 默认值），
		// 否则「启动日志里没有保留期那一行」会变成新的静默路径。
		main := mustLogEvent(t, buf, find, "server.retention.configured")
		if main["source"] != eventRetentionSourceDefault {
			t.Fatalf("读不到库时 source 应为 %q，实际 %v", eventRetentionSourceDefault, main["source"])
		}
		if main["events_retention_sec"] != float64(store.DefaultRetentionEventTTLSec) {
			t.Fatalf("读不到库时应报 store 默认值，实际 %v", main["events_retention_sec"])
		}
	})
}
