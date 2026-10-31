package server

// 本文件是 #259 的**回归护栏**：把「cleanup 跑在 db.Close() 之后」从偶发红变成
// 机器可检的不变量。机制照搬 internal/ws/postclose_guard_test.go（#255/#258），
// 这里只换成 server 包名 + 本包的夹具名（internal/server/wsfixture_test.go）。
//
// 失败签名（#259，内部复现）：
//
//	/tmp/TestNodeListAndDrain1104048964/001/ 失败后只剩 console.db-shm(32KB)
//	WARN mark instances dead failed ... error="sql: database is closed"
//
// `httptest.Server.Close()` **不等待**已劫持（WebSocket）连接（net/http/httptest
// server.go 的 `case http.StateHijacked, http.StateClosed` 会把连接从 s.wg 摘掉），
// 而 nodeSession.cleanup（internal/ws/node_handler.go）在 handler goroutine 里**同步**
// 写库（MarkInstancesDead / UpdateNodeStatus / ResolveLostTasks）；db.Close() 与它
// 并发就成了那条 WARN，残留的 SQLite 连接还会在 t.TempDir 的 RemoveAll 期间重建
// -wal/-shm → 随机某个用例红在「directory not empty」/「残留 console.db-shm」。
//
// 夹具侧的正解在 wsfixture_test.go 的 nodeStreamFixture.settle（handler 返回 →
// 才关库）。本文件负责**证明**它没退化：整包跑完只要出现过一条 `sql: database is closed`，
// TestMain 就让包变红并打印样例。
//
// 注意：这不是新增产品行为，也没有放宽任何断言——它只是把那条偶发红换成确定性红。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dbClosedMarker 是 SQLite「库已关」的稳定文案（modernc/sqlite 与 database/sql 都这么写）。
const dbClosedMarker = "database is closed"

// maxGuardSamples 是最多留几条样例日志（够定位即可，不必把几十条全存下来）。
const maxGuardSamples = 2

// postCloseGuard 收集「cleanup 跑在 db.Close() 之后」的证据。
//
// 计数用 atomic（Handle 会被多个连接 goroutine 并发调用）；样例用 mutex 保护。
var postCloseGuard = &dbClosedGuard{}

// dbClosedGuard 是护栏的累加器。
type dbClosedGuard struct {
	count atomic.Int64

	mu      sync.Mutex
	samples []string
}

// hit 记一次命中（附一条可打印的样例）。
func (g *dbClosedGuard) hit(sample string) {
	g.count.Add(1)
	g.mu.Lock()
	if len(g.samples) < maxGuardSamples {
		g.samples = append(g.samples, sample)
	}
	g.mu.Unlock()
}

// snapshot 返回 (命中次数, 样例副本)。
func (g *dbClosedGuard) snapshot() (int64, []string) {
	g.mu.Lock()
	samples := append([]string(nil), g.samples...)
	g.mu.Unlock()
	return g.count.Load(), samples
}

// dbClosedGuardHandler 包住一个**叶子** slog.Handler：记录原样转发给下层，只额外统计
// 含 `sql: database is closed` 的记录。下层绝不能回指 slog.Default() / stdlib
// defaultHandler（会经 log→slog 桥绕回自己，log.Logger.mu 不可重入 ⇒ 死锁，
// 详见 TestMain 上方注释）。
//
// WithAttrs / WithGroup 必须继续套上本包装（否则带着 attrs 的子 logger 会绕过护栏）。
type dbClosedGuardHandler struct {
	inner slog.Handler
}

func (h dbClosedGuardHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h dbClosedGuardHandler) Handle(ctx context.Context, r slog.Record) error {
	if recordHasDBClosed(r) {
		postCloseGuard.hit(formatGuardRecord(r))
	}
	return h.inner.Handle(ctx, r)
}

func (h dbClosedGuardHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return dbClosedGuardHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h dbClosedGuardHandler) WithGroup(name string) slog.Handler {
	return dbClosedGuardHandler{inner: h.inner.WithGroup(name)}
}

// recordHasDBClosed 判断这条记录是否在报「库已关」。
//
// 只看 msg 不够：真实命中是 `slog.Warn("mark instances dead failed", "error", err.Error())`
// ——文案在 error 字段里（可能是 *error / string / group 里的任意一种）。
func recordHasDBClosed(r slog.Record) bool {
	if strings.Contains(r.Message, dbClosedMarker) {
		return true
	}
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if attrHasDBClosed(a) {
			found = true
			return false // 提前结束遍历
		}
		return true
	})
	return found
}

// attrHasDBClosed 递归检查一个属性值（含 group）。
func attrHasDBClosed(a slog.Attr) bool {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return strings.Contains(v.String(), dbClosedMarker)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err != nil && strings.Contains(err.Error(), dbClosedMarker)
		}
		return strings.Contains(fmt.Sprint(v.Any()), dbClosedMarker)
	case slog.KindGroup:
		for _, ga := range v.Group() {
			if attrHasDBClosed(ga) {
				return true
			}
		}
	}
	return false
}

// formatGuardRecord 把记录渲染成一行样例（护栏报警时打印）。
func formatGuardRecord(r slog.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %q", r.Time.Format(time.RFC3339Nano), r.Level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Resolve())
		return true
	})
	return b.String()
}

// TestMain 是整包的唯一入口：装护栏 → 跑包 → 有命中就把退出码改红。
//
// 语义保持：① m.Run() 之后立刻把 slog.Default 恢复成原来的（此后不该再有被测代码
// 写日志，也不把包装带出去）；② 只用 os.Exit，不依赖 defer（os.Exit 不跑 defer，
// 恢复必须显式写）；③ 不碰 *testing.T 的任何语义（t.TempDir 清理、失败判定都不受影响）。
//
// 下层 handler 为什么**不能**用 prev.Handler()（= stdlib 的 defaultHandler）：
// defaultHandler 会经 `log` 包输出，而 `log` 包的默认 writer 又装了 log→slog 桥
// （log.init.0.func1 → slog.Default().Handler().Handle）。于是 Handle 会经桥
// **绕回自己**，再次进 defaultHandler → `log.(*Logger).output` 拿**同一把**
// log.Logger.mu —— 它不可重入 ⇒ 任何一条日志都永久死锁（#258 实测：handler
// goroutine 卡在 slog.Info("node connected") → log.output 的 Mutex.Lock 上，
// 整个包 15s 收尾超时）。所以这里直接落到 stderr 的 TextHandler：无回指、无共享锁。
// 副作用可接受：测试期日志格式从 `2026/10/31 20:55:19 INFO ...` 变成 slog TextHandler
// 格式（`time=... level=INFO msg=...`），没有任何测试解析日志文本。
func TestMain(m *testing.M) {
	prev := slog.Default()
	// next 必须是「叶子」handler：绝不回指 slog.Default() / log 包（见上方注释）。
	next := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(dbClosedGuardHandler{inner: next}))

	code := m.Run()

	// 恢复：m.Run 返回时所有用例的 cleanup（含 settle / srv.Close / db.Close /
	// t.TempDir 清理）都已经跑完，此后不再有被测代码需要被统计。
	slog.SetDefault(prev)

	if n, samples := postCloseGuard.snapshot(); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\n[postclose-guard] 捕获 %d 条含 %q 的日志：cleanup 跑在 db.Close() 之后（#259 竞态回归）\n",
			n, dbClosedMarker)
		for i, s := range samples {
			fmt.Fprintf(os.Stderr, "[postclose-guard] 样例 %d: %s\n", i+1, s)
		}
		fmt.Fprintf(os.Stderr,
			"[postclose-guard] 期望：nodeStreamFixture.settle 在 db.Close() 之前等所有 ServeHTTP 返回"+
				"（internal/server/wsfixture_test.go）；这条护栏不是新增产品行为，是 #259 那条偶发红的不变量化\n\n")
		os.Exit(1)
	}
	os.Exit(code)
}
