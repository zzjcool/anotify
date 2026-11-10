package server

import (
	"log/slog"
	"net/http"

	"github.com/zzjcool/anotify/internal/lease"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// serverDeps 是 v4 HTTP 资源层共享的运行期依赖（冻结契约 D1）。
//
// 这里刻意只放「已由 v4 runtime 导出」的真实接缝：store（事实）/ route.Router（规划与
// emit）/ source.Service（源触发与 ingress）/ ws.Hub（节点会话、派发、日志、部署）/
// lease.Sweeper（租约与保留期巡检）。旧 v3 的 executor（中枢执行器）与 broker 包在
// v4 已被删除，不得以 stub 恢复（331 卡 D0.2/D0.3）。
type serverDeps struct {
	DB      *store.DB
	Router  *route.Router
	Sources *source.Service
	Hub     *ws.Hub
	Sweeper *lease.Sweeper
	Log     *slog.Logger
	Now     func() int64

	// Session 是会话 Cookie 中间件（未登录直 401）。资源域与保留域共用同一份，
	// 不允许各域自己再包一层鉴权：两份实现迟早对「未登录」给不同状态码。
	Session func(http.Handler) http.Handler
	// Backpressure 是事件/触发入口的背压中间件（只包写入口，读端点不受影响）。
	Backpressure func(http.Handler) http.Handler

	// display 是 namespace id → slug 的缓存。
	//
	// 为什么可以缓存而不失效：store.RenameNamespace 只改展示名，slug 是稳定标识
	// （contract: 「slug 是稳定标识（出现在 URL 与令牌作用域里），不可改」）。
	// 缓存容量 = 命名空间数量（部署级个位数到几十），换来的是列表投影里不需要
	// 每行一次查询。nil 时自动退化为每次查询（测试里直接构造 deps 也不会崩）。
	display *nsDisplay
}

// session 用会话中间件包住 handler；未注入时直接透传（仅测试构造可能为 nil）。
func (d *serverDeps) session(h http.Handler) http.Handler {
	if d == nil || d.Session == nil {
		return h
	}
	return d.Session(h)
}

// backpressure 用背压中间件包住 handler；未注入时透传。
func (d *serverDeps) backpressure(h http.Handler) http.Handler {
	if d == nil || d.Backpressure == nil {
		return h
	}
	return d.Backpressure(h)
}

// nowMilli 是 serverDeps.Now 的**生产实现**：契约单位是毫秒。
//
// 为什么需要这一层而不是直接注入 store.Now：store 的时钟是 **秒**（`internal/store/store.go`
// `Now() int64 { return time.Now().Unix() }`，供所有写入点统一使用），而本包对外的
// `Now` 契约是**毫秒**（D1）。两者混用不会被编译器发现，只会静默算错：
//
//   - `retained_reads.go` 的 stats 热力图窗口 = `taskNowSeconds(d) - 371*86400`；把秒当初
//     毫秒再 `/1000` 得到 1.76e6（1970-01-21），`since` 变成负数 ⇒ `created_at >= since`
//     恒真 ⇒ 热力图混入全部历史；
//   - `store.RequestCancel` 的 `now` 参数于是落成 1970 年。
//
// 单位对齐是**装配的责任**，不是各 handler 的责任：handler 只读 `d.now()`（毫秒）并在
// 需要秒的下游显式换算（`taskNowSeconds`）。测试夹具也必须注入毫秒——否则夹具会把这类
// 单位错位遮住（这正是 #340 r2 review 的 M-1）。
func nowMilli() int64 { return store.Now() * 1000 }

// now 返回可信 server 时间（**毫秒**）。nil 时回落到 0，调用方应保证构造时注入。
func (d *serverDeps) now() int64 {
	if d == nil || d.Now == nil {
		return 0
	}
	return d.Now()
}

// logger 返回非 nil logger，避免 handler 里到处判空。
func (d *serverDeps) logger() *slog.Logger {
	if d == nil || d.Log == nil {
		return slog.Default()
	}
	return d.Log
}
