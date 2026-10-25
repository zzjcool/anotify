package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/zzjcool/anotify/internal/api"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/executor"
	"github.com/zzjcool/anotify/internal/lease"
	"github.com/zzjcool/anotify/internal/push"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
	"github.com/zzjcool/anotify/internal/ws"
)

// App 是装配好的应用（持有需要关闭的资源）。
type App struct {
	Handler http.Handler
	DB      *store.DB
	AuthSvc *auth.Service
}

// NewMux 装配完整应用：/v1/* 动态 API（no-store），其余静态资源（CDN 缓存分级）。
func NewMux(cfg Config) http.Handler {
	return NewApp(context.Background(), cfg).Handler
}

// NewApp 装配完整应用（含后台 push 派发器）。
func NewApp(ctx context.Context, cfg Config) *App {
	// 1. 存储
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("database open failed",
			"event", "server.fatal",
			"component", "store",
			"error", err.Error(),
		)
		os.Exit(1)
	}
	slog.Info("database opened",
		"event", "server.db.opened",
		"path", cfg.DBPath,
	)

	// 3. 认证（WebAuthn + API Key）
	authSvc, err := auth.NewService(db, auth.Config{
		RPDisplayName: cfg.RPDisplay,
		RPID:          cfg.RPID,
		RPOrigins:     []string{cfg.RPOrigin},
		SessionTTL:    7 * 24 * time.Hour,
		SecureCookie:  cfg.RPOrigin != "" && len(cfg.RPOrigin) > 8 && cfg.RPOrigin[:5] == "https",
	})
	if err != nil {
		slog.Error("auth service init failed",
			"event", "server.fatal",
			"component", "auth",
			"error", err.Error(),
		)
		os.Exit(1)
	}
	keyValidator := asKeyValidator(authSvc.Keys())

	// 3.5 CLI 设备授权管理器
	cliAuthMgr := auth.NewCliAuthManager(db, 0)

	// 3.6 Passkey 设备授权管理器（复用 cli_auth_sessions 表，kind=passkey）
	enrollMgr := auth.NewPasskeyEnrollManager(db, authSvc, 0)

	// 4. Web Push（P-256 VAPID 密钥）
	//
	// #80 之后不再有"按用户的消息消费者"（那套依赖 broker 的消息订阅）。
	// 推送由中枢执行器在 notify.phone 任务里直接发（见 push.CentralNotifier）。
	var vapidSender push.Sender
	vapidCfg, vapidErr := push.LoadVAPID()
	if vapidErr == nil {
		vapidSender = push.NewVAPIDSender(vapidCfg)
	} else {
		slog.Warn("VAPID not configured, push disabled",
			"event", "push.vapid.missing",
			"error", vapidErr.Error(),
		)
	}

	// 5. HTTP 处理器
	//
	// #80：旧通知终端模型的 handler（NotifyHandler / ws.Handler /
	// notificationsHandler）已随 broker 包一起删除。事件中心用
	// /v1/events（api.EventHandler）+ 会话版 /v1/console/events。
	authH := &authHandler{svc: authSvc}
	devicesH := &devicesHandler{db: db}
	keysH := &keysHandler{keys: authSvc.Keys(), db: db}
	statsH := &statsHandler{db: db}
	cliAuthStaticFS := resolveStaticFS(cfg)
	cliAuthH := &cliAuthHandler{
		mgr:      cliAuthMgr,
		keys:     authSvc.Keys(),
		keysV:    keyValidator,
		db:       db,
		staticFS: cliAuthStaticFS,
	}

	enrollH := &passkeyEnrollHandler{mgr: enrollMgr, db: db}

	// 节点连接注册表：派发器通过它把 assign 帧推给节点。
	// 必须在 eventH 之前创建——派发器需要它作为 Publisher。
	nodeHub := ws.NewNodeHub()

	// 事件入口（#58）：触发器 + 派发器
	// 派发器带 Publisher，这样「派发成功」包含「assign 帧真的推出去了」，
	// 而不是仅仅把库里的状态改成 assigned（节点可能根本不在线）。
	dispatcher := route.New(db)
	dispatcher.Publisher = nodeHub
	eventH := &api.EventHandler{
		Store:    db,
		Keys:     keyValidator,
		Triggers: trigger.New(db, dispatcher),
	}

	// 控制台 API（#59）：8 个新端点，全部走组织维度（I10）
	consoleH := &consoleHandler{db: db, auth: authSvc}

	sessMW := authSvc.Sessions().Middleware
	// 管理后台：登录会话 + 超管权限（两层中间件）
	adminMW := sessMW(authSvc.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminH := &adminHandler{db: db, svc: authSvc}
		adminH.ServeHTTP(w, r)
	})))

	// 限速器信任代理头开关（仅在反代后开启）
	trustProxyHeaders = cfg.TrustProxy

	mux := http.NewServeMux()
	// --- 动态 API（no-store）---
	// 认证端点：register/login/logout 无需登录；me/sessions 需登录会话。
	// 注意 Go 1.22+ ServeMux 最长模式优先，/v1/auth/me 与 /v1/auth/sessions 会优先于 /v1/auth/。
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/sessions", noStore(sessMW(http.HandlerFunc(authH.sessions))))
	// Passkey 管理端点（需登录会话）：列表 / 补建凭证 / 重命名 / 删除。
	// 独立于 /v1/auth/ 兜底注册，确保走 sessMW 鉴权（否则返回 401 而非 404）。
	mux.Handle("/v1/auth/passkeys", noStore(sessMW(http.HandlerFunc(authH.passkeysRoot))))
	mux.Handle("/v1/auth/passkeys/", noStore(sessMW(http.HandlerFunc(authH.passkeysItem))))
	// 恢复码：regenerate/status 需登录；login 走 /v1/auth/ 兜底（匿名）。
	mux.Handle("/v1/auth/recovery/regenerate", noStore(sessMW(http.HandlerFunc(authH.recoveryRegenerate))))
	mux.Handle("/v1/auth/recovery/status", noStore(sessMW(http.HandlerFunc(authH.recoveryStatus))))
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))
	// VAPID 公钥（前端订阅用，无需登录也可读；单一事实源 = push.LoadVAPID）
	mux.Handle("/v1/vapid-public-key", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub := ""
		if vapidCfg != nil {
			pub = vapidCfg.PublicKey
		}
		writeJSON(w, 200, map[string]string{"publicKey": pub})
	})))
	// 以下需登录会话（Cookie）
	mux.Handle("/v1/devices", noStore(sessMW(devicesH)))
	mux.Handle("/v1/devices/", noStore(sessMW(devicesH)))
	mux.Handle("/v1/keys", noStore(sessMW(keysH)))
	mux.Handle("/v1/keys/", noStore(sessMW(keysH)))
	mux.Handle("/v1/stats", noStore(sessMW(statsH)))

	// --- 节点协议（#57）：WS，opaque token 鉴权在 Upgrade 之前 ---
	// 必须复用上面的 nodeHub：否则 handler 会惰性建一个**独立**的 hub，
	// 派发器往 nodeHub 里推，handler 往自己的 hub 里注册 —— 永远推不到。
	nodeH := ws.NewNodeHandler(db)
	nodeH.Hub = nodeHub
	// 任务成功回报 out 后派发下一跳（契约 §4 的递归应用）。
	// 不接这个的话整条链路只能走一步——第一跳完成后就永远停在那里。
	nodeH.NextHop = dispatcher

	// 租约巡检（契约 §3.3）：每 5s 回收过期租约并重派到期的 pending 任务。
	//
	// 没有它的话，节点异常下线后任务会永远停在 assigned/acked/running：
	// 租约到期没人扫 → 任务不回收 → 槽位永久占用 → 节点并发能力逐次泄漏。
	// 这是自愈机制，单轮失败只记日志，不能停摆。
	// 启动时为所有已存在组织补齐内置能力（#83）。
	//
	// 内置能力清单可能随版本升级新增；只在注册时播种的话，
	// 已有组织永远补不上，notify.phone 派发会一直 pruned/no_node。
	if n, err := db.BackfillBuiltinCapabilities(ctx); err != nil {
		slog.Warn("backfill builtin capabilities failed",
			"event", "server.builtin.backfill_failed",
			"error", err.Error(),
		)
	} else if n > 0 {
		slog.Info("builtin capabilities ready",
			"event", "server.builtin.backfill",
			"orgs", n,
		)
	}

	// 源（origin）的 cron 调度器。
	//
	// 与 sweeper / executor 并列：都是"后台自愈/驱动"循环，
	// 单轮失败只记日志继续，不能因一次 DB 抖动就永久停摆。
	//
	// 它需要 Dispatcher 才能把触发变成任务，所以只能在这里起
	// （不能放进 store 层——那会让 store 依赖 route）。
	srcTrigger := &source.Trigger{Store: db, Dispatcher: dispatcher}
	srcSched := &source.Scheduler{Store: db, Dispatcher: dispatcher}
	go srcSched.Run(ctx)
	slog.Info("source scheduler wired", "event", "server.source.ready")

	sweeper := lease.New(db)
	sweeper.Redispatch = func(sctx context.Context, task *store.Task) (bool, error) {
		ok, err := dispatcher.RedispatchTask(sctx, task)
		if err != nil {
			return false, err
		}
		if !ok {
			// 这次没派成（没节点/容量不足）：设退避，避免每 5s 空转打日志。
			// 不动 status——任务本来就是 pending。
			backoff := store.RetryBackoff(task.Attempts)
			if backoff > 0 {
				if serr := db.SetNotBefore(sctx, task.OrgID, task.ID, store.Now()+backoff); serr != nil {
					slog.Warn("set retry backoff failed",
						"event", "lease.sweeper.backoff_failed",
						"task_id", task.ID,
						"error", serr.Error(),
					)
				}
			}
		}
		return ok, nil
	}
	go sweeper.Run(ctx)

	// 中枢能力执行器（#83）：执行 notify.phone 这类没有节点执行者的内置能力。
	//
	// 它们在图里是正常的一条边，只是"执行者"是中枢自己而不是外部节点。
	// 没有它的话，notify.phone 派发时永远 pruned/no_node——推送发不出去。
	{
		var notifier executor.Notifier
		if vapidSender != nil {
			notifier = &push.CentralNotifier{Store: db, Sender: vapidSender}
		}
		centralExec := executor.New(db, notifier)
		go centralExec.Run(ctx, 0) // 0 = 默认间隔
	}
	mux.Handle("GET /v1/node/stream", noStore(nodeH))

	// --- 事件中心（#58 / #59）---
	// 事件入口用 API Key（机器调用），与控制台用会话是两套鉴权。
	mux.Handle("POST /v1/events", noStore(eventH))
	mux.Handle("GET /v1/events", noStore(eventH))
	// 控制台端点：登录会话 + 组织维度校验
	mux.Handle("GET /v1/tasks", noStore(sessMW(http.HandlerFunc(consoleH.tasks))))
	mux.Handle("GET /v1/tasks/{id}", noStore(sessMW(http.HandlerFunc(consoleH.task))))
	// 控制台读事件流：走会话 Cookie（机器入口的 GET /v1/events 走 Bearer Key）。
	mux.Handle("GET /v1/console/events", noStore(sessMW(http.HandlerFunc(consoleH.events))))
	mux.Handle("POST /v1/tasks/{id}/cancel", noStore(sessMW(http.HandlerFunc(consoleH.cancelTask))))
	mux.Handle("GET /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("POST /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("GET /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("POST /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("DELETE /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	// 源（origin）：事件的入口实例。增删改查 + 手动触发。
	//
	// 与控制台其它读接口一样走会话 Cookie。
	srcH := &sourceHandler{db: db, commander: srcTrigger, resolveOrg: consoleH.resolveOrg}
	mux.Handle("GET /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("POST /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("GET /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("PATCH /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("DELETE /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("POST /v1/sources/{id}/fire", noStore(sessMW(http.HandlerFunc(srcH.fire))))
	// webhook 入口**不需要会话**：鉴权是 URL 里的 token。
	// 外部系统（GitHub / 监控告警）不可能带我们的 Cookie。
	mux.Handle("POST /v1/hooks/{token}", noStore(http.HandlerFunc(srcH.hook)))

	mux.Handle("GET /v1/nodes", noStore(sessMW(http.HandlerFunc(consoleH.nodes))))
	mux.Handle("POST /v1/nodes/{id}/drain", noStore(sessMW(http.HandlerFunc(consoleH.drainNode))))

	// --- 管理后台（/v1/admin/*，需登录会话 + 超管权限） ---
	mux.Handle("/v1/admin/", noStore(adminMW))
	mux.Handle("/v1/admin", noStore(adminMW))

	// --- CLI 设备授权 ---
	// 匿名端点（无鉴权）
	mux.Handle("POST /v1/cli-auth/sessions", noStore(http.HandlerFunc(cliAuthH.create)))
	mux.Handle("GET /v1/cli-auth/sessions/{id}/poll", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliAuthH.poll(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/cli-auth/sessions/{id}/qr.txt", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliAuthH.qr(w, r, r.PathValue("id"))
	})))
	// 需登录会话（Cookie）
	mux.Handle("GET /v1/cli-auth/sessions/by-code", noStore(sessMW(http.HandlerFunc(cliAuthH.byCode))))
	mux.Handle("GET /v1/cli-auth/sessions/{id}", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliAuthH.get(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/cli-auth/sessions/{id}/approve", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliAuthH.approve(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/cli-auth/sessions/{id}/deny", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliAuthH.deny(w, r, r.PathValue("id"))
	}))))
	// Key 自检（Bearer）
	mux.Handle("GET /v1/keys/self", noStore(http.HandlerFunc(cliAuthH.keysSelf)))
	// 登录脚本分发（匿名）
	mux.Handle("GET /agent-login.sh", http.HandlerFunc(cliAuthH.agentLoginScript))

	// --- Passkey 设备授权（/v1/passkey-enroll/sessions/*） ---
	// 旧设备发起（需登录 Cookie）
	mux.Handle("POST /v1/passkey-enroll/sessions", noStore(sessMW(http.HandlerFunc(enrollH.create))))
	// 匿名端点（新设备用）
	mux.Handle("GET /v1/passkey-enroll/sessions/by-code", noStore(http.HandlerFunc(enrollH.byCode)))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.lookup(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/request", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.knock(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/poll", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.poll(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/complete", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.complete(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/qr.txt", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.qr(w, r, r.PathValue("id"))
	})))
	// 需登录 Cookie（旧设备用）
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/watch", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.watch(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/approve", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.approve(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/deny", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.deny(w, r, r.PathValue("id"))
	}))))
	mux.Handle("DELETE /v1/passkey-enroll/sessions/{id}", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.cancel(w, r, r.PathValue("id"))
	}))))

	// 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// --- 静态资源（CDN 缓存分级）---
	mux.Handle("/", resolveStatic(cfg))

	return &App{Handler: accessLog(mux), DB: db, AuthSvc: authSvc}
}
