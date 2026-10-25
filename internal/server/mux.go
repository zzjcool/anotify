package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/api"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/lease"
	"github.com/zzjcool/anotify/internal/push"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
	"github.com/zzjcool/anotify/internal/ws"
)

// App 是装配好的应用（持有需要关闭的资源）。
type App struct {
	Handler http.Handler
	Broker  broker.Broker
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

	// 2. Broker
	bk := broker.NewSQLite(db)

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

	// 4. Web Push 派发器（后台 goroutine，动态按需为每个用户启动消费）
	var dm *dispatchManager
	vapidCfg, vapidErr := push.LoadVAPID()
	if vapidErr == nil {
		d := &push.Dispatcher{Broker: bk, Store: db, Sender: push.NewVAPIDSender(vapidCfg)}
		dm = newDispatchManager(ctx, d, db)
	} else {
		slog.Warn("VAPID not configured, push disabled",
			"event", "push.vapid.missing",
			"error", vapidErr.Error(),
		)
	}

	// 5. HTTP 处理器
	notifyH := &api.NotifyHandler{Broker: bk, Keys: keyValidator, Store: db}
	// 上报成功后确保该用户的 push 消费者已启动（覆盖运行期新注册用户）
	if dm != nil {
		notifyH.OnPublished = dm.Ensure
	}
	streamH := ws.NewHandler(bk, keyValidator)
	authH := &authHandler{svc: authSvc}
	devicesH := &devicesHandler{db: db}
	keysH := &keysHandler{keys: authSvc.Keys(), db: db}
	notifsH := &notificationsHandler{bk: bk, db: db}
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

	// 启动时已存在的用户先各起一个 push 消费者
	if dm != nil {
		dm.StartExisting()
	}

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
	// Agent 上报（Bearer Key，内部自校验）
	mux.Handle("/v1/notify", noStore(notifyH))
	// 工作台测试上报（会话 Cookie 鉴权）：已登录用户在网页直接发测试通知到自己设备
	mux.Handle("/v1/test-notify", noStore(sessMW(http.HandlerFunc(notifyH.ServeTestNotify))))
	// WS 长连接（Bearer Key，内部自校验）
	mux.Handle("/v1/stream", noStore(streamH))
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
	mux.Handle("/v1/notifications", noStore(sessMW(notifsH)))
	mux.Handle("/v1/notifications/", noStore(sessMW(notifsH)))
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
	mux.Handle("GET /v1/node/stream", noStore(nodeH))

	// --- 事件中心（#58 / #59）---
	// 事件入口用 API Key（机器调用），与控制台用会话是两套鉴权。
	mux.Handle("POST /v1/events", noStore(eventH))
	mux.Handle("GET /v1/events", noStore(eventH))
	// 控制台端点：登录会话 + 组织维度校验
	mux.Handle("GET /v1/tasks", noStore(sessMW(http.HandlerFunc(consoleH.tasks))))
	// 控制台读事件流：走会话 Cookie（机器入口的 GET /v1/events 走 Bearer Key）。
	mux.Handle("GET /v1/console/events", noStore(sessMW(http.HandlerFunc(consoleH.events))))
	mux.Handle("POST /v1/tasks/{id}/cancel", noStore(sessMW(http.HandlerFunc(consoleH.cancelTask))))
	mux.Handle("GET /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("POST /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("GET /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("POST /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("DELETE /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
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

	return &App{Handler: accessLog(mux), Broker: bk, DB: db, AuthSvc: authSvc}
}

// dispatchManager 动态管理 per-user push 消费者：上报/启动时按需为每个用户
// 启动一个 broker 消费 goroutine，避免为不活跃用户常驻订阅。
type dispatchManager struct {
	ctx context.Context
	d   *push.Dispatcher
	db  *store.DB
	mu  sync.Mutex
	run map[string]struct{}
}

func newDispatchManager(ctx context.Context, d *push.Dispatcher, db *store.DB) *dispatchManager {
	return &dispatchManager{ctx: ctx, d: d, db: db, run: make(map[string]struct{})}
}

// Ensure 确保某用户的 push 消费者已启动（幂等）。
func (m *dispatchManager) Ensure(userID string) {
	if userID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.run[userID]; ok {
		return
	}
	m.run[userID] = struct{}{}
	go func() {
		if err := m.d.Run(m.ctx, userID); err != nil && err != context.Canceled {
			slog.Error("push consumer exited",
				"event", "push.consumer.exit",
				"user_id", userID,
				"error", err.Error(),
			)
		}
	}()
}

// StartExisting 为启动时已存在的所有用户各起一个消费者。
func (m *dispatchManager) StartExisting() {
	users, err := m.db.ListAllUserIDs(m.ctx)
	if err != nil {
		slog.Error("failed to list users for push consumers",
			"event", "push.consumer.list_failed",
			"error", err.Error(),
		)
		return
	}
	for _, uid := range users {
		slog.Info("push consumer started",
			"event", "push.consumer.start",
			"user_id", uid,
		)
		m.Ensure(uid)
	}
	slog.Info("push consumers started",
		"event", "push.consumer.start",
		"count", len(users),
	)
}
