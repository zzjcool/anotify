package server

import (
	"log/slog"

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
}

// now 返回可信 server 时间（毫秒）。nil 时回落到 0，调用方应保证构造时注入。
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
