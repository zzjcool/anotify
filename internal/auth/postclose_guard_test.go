package auth

// 本文件是 #259 的**回归护栏**：把「关库之后还有人在写库」从偶发红变成机器可检的不变量。
// 结构照搬 internal/ws/postclose_guard_test.go（#255 同一类病症），另加一条夹具侧的
// 确定性判据上报（reportPostCloseRace，见下方「两道判据」）。
//
// #259 的 CI 红长这样：
//
//	TestSessionInvalidateUser: TempDir RemoveAll cleanup: unlinkat ...: directory not empty
//	（失败现场 /tmp/TestSessionInvalidateUser.../001/ 只剩一个 sessions.db-shm，32KB）
//
// 失败签名读法：RemoveAll 已经把 sessions.db 删掉了，却还有连接活着并**重建**了 -shm；
// 而 store.Open / db.Close() 本身是干净的（400 次开关探针零遗留），所以必然是
// 「db.Close() 与某个仍在跑的 DB 使用者并发」。并发使用者的症状日志是
// `sql: database is closed`（database/sql 对已 Close 的池返回的固定文案）——
// internal/auth 的产品代码里会写裸 DB 错误的地方只有两处：SessionManager.Middleware
// （`WARN session invalid ... reason=<err>`，但会话路径会先映射成 errSessionInvalid，
// 见下）与 webauthn 注册失败（webauthn.go 的 `slog.Error("register: register user with
// namespace failed", "error", nerr.Error())`）。
//
// 于是护栏的判据是：**整包跑完不许出现过一条含 `database is closed` 的日志**，
// 命中就让 TestMain 把退出码改红并打印样例。这不是新增产品行为，也没有放宽任何断言：
// 它只是把那条偶发红（随机某个用例红在「directory not empty」）换成确定性红。
//
// ---------- 为什么下层 handler 必须是「叶子」 ----------
//
// 绝不能回指 slog.Default() / stdlib 的 defaultHandler：defaultHandler 经 `log` 包输出，
// 而 `log` 包的默认 writer 装了 log→slog 桥（log.init.0.func1 → slog.Default().Handler()），
// 于是 Handle 会经桥**绕回自己** → `log.(*Logger).output` 抢**同一把** log.Logger.mu
// （不可重入）⇒ 任何一条日志都永久死锁（internal/ws 实测：整个包 15s 收尾超时）。
// 所以这里直接落到 stderr 的 TextHandler：无回指、无共享锁。副作用可接受：
// 测试期日志格式变成 slog TextHandler 格式（time=... level=... msg=...），
// 本包没有任何测试解析日志文本。
//
// ---------- 两道判据（为什么不只数日志） ----------
//
// 事实核对（别被 issue 里的猜测带偏）：`internal/auth` 的会话路径把 DB 错误统一映射成
// `errSessionInvalid`（session.go:49「auth: 会话无效」，避免泄露会话是否存在）——
// 所以**故意关库**的用例只会打出 `reason="auth: 会话无效"`，不含 `database is closed`；
// 反过来，真正逃逸到 db.Close() 之后的写者（touchSession 的失败被静默忽略、
// GetSession 的错误被映射）也**不会**把 `database is closed` 写进日志。
//
// ⇒ 只靠数日志在 auth 包里既不会误报、也永远咬不到东西。所以本护栏是两道：
//
//	① 主判据（确定性，见 session_cache_test.go 的 closeDBForTempDir）：
//	   关库前 InUse==0、关库后 OpenConnections==0、目录里无 *.db-wal|*.db-shm。
//	   任一不成立 ⇒ 夹具调用 reportPostCloseRace() 上报本护栏（附 `database is closed`
//	   这句「写者会看到什么」的证据），TestMain 打印样例并 exit=1。
//	② 副判据（日志旁路，照搬 #258）：整包跑完不允许出现含 `database is closed` 的
//	   slog 记录。今天 auth 的产品路径不会打这句，但一旦有人把裸 DB 错误直接写进日志，
//	   这条会在 CI 里立刻点名（而不是等 TempDir 随机红）。

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

// postCloseGuard 收集「关库之后还有人写库」的证据。
//
// 计数用 atomic（Handle 会被多个 goroutine 并发调用）；样例用 mutex 保护。
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
// 含 `database is closed` 的记录。下层绝不能回指 slog.Default() / stdlib
// defaultHandler（会经 log→slog 桥绕回自己 ⇒ 死锁，见文件头注释）。
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
// 只看 msg 不够：本包真实命中的写法是
// `slog.Warn("session invalid", "reason", err.Error(), ...)`
// ——文案在 **attr** 里（可能是 *error / string / group 里的任意一种）。
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

// reportPostCloseRace 由夹具在**确定性判据**命中时调用（见 closeDBForTempDir）。
//
// 传进来的 sample 必须是「机器事实」而不是猜测：夹具把 InUse/OpenConnections/残留文件名
// 原样写进来，并明确写出并发写者会看到的固定文案（`sql: database is closed`），
// 这样 TestMain 的汇总里既有证据、也点明后果。
func reportPostCloseRace(sample string) {
	postCloseGuard.hit(sample)
}

// TestMain 是整包的唯一入口：装护栏 → 跑包 → 有命中就把退出码改红。
//
// 语义保持：① m.Run() 之后立刻把 slog.Default 恢复成原来的（此后不该再有被测代码
// 写日志，也不把包装带出去）；② 只用 os.Exit，不依赖 defer（os.Exit 不跑 defer，
// 恢复必须显式写）；③ 不碰 *testing.T 的任何语义（t.TempDir 清理、失败判定都不受影响）。
func TestMain(m *testing.M) {
	prev := slog.Default()
	// next 必须是「叶子」handler：绝不回指 slog.Default() / log 包（见文件头注释）。
	next := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(dbClosedGuardHandler{inner: next}))

	code := m.Run()

	// 恢复：m.Run 返回时所有用例的 cleanup（含 closeDBForTempDir 的关库断言 /
	// t.TempDir 清理）都已经跑完，此后不再有被测代码需要被统计。
	slog.SetDefault(prev)

	if n, samples := postCloseGuard.snapshot(); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\n[postclose-guard] 捕获 %d 条含 %q 的日志：关库之后仍有 DB 使用者（#259 竞态回归）\n",
			n, dbClosedMarker)
		for i, s := range samples {
			fmt.Fprintf(os.Stderr, "[postclose-guard] 样例 %d: %s\n", i+1, s)
		}
		fmt.Fprintf(os.Stderr,
			"[postclose-guard] 期望：任何请求/写库都在 db.Close() 之前返回——"+
				"closeDBForTempDir 会断言 InUse==0、OpenConnections==0、且关库后无 -wal/-shm 残留；"+
				"任一不成立就调用 reportPostCloseRace 上报本护栏。"+
				"这条护栏不是新增产品行为，是 #259 那条 CI 红的不变量化\n\n")
		os.Exit(1)
	}
	os.Exit(code)
}
