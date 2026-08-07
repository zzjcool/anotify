package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/anotify/anotify/internal/api"
	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/push"
	"github.com/anotify/anotify/internal/store"
	"github.com/anotify/anotify/internal/ws"
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
		log.Fatalf("[server] 打开数据库失败: %v", err)
	}

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
		log.Fatalf("[server] 初始化认证失败: %v", err)
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
		log.Printf("[warn] VAPID 未配置，Web Push 派发器未启动: %v", vapidErr)
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

	// 静态文件系统（供 /agent-login.sh 分发；dev 模式用本地目录，否则 embed）
	cliAuthStaticFS := resolveStaticFS(cfg)
	cliAuthH := &cliAuthHandler{
		mgr:      cliAuthMgr,
		keys:     authSvc.Keys(),
		keysV:    keyValidator,
		db:       db,
		staticFS: cliAuthStaticFS,
	}

	enrollH := &passkeyEnrollHandler{mgr: enrollMgr}

	sessMW := authSvc.Sessions().Middleware

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
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))
	// Agent 上报（Bearer Key，内部自校验）
	mux.Handle("/v1/notify", noStore(notifyH))
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

	return &App{Handler: mux, Broker: bk, DB: db, AuthSvc: authSvc}
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
			log.Printf("[push] 用户 %s 派发器退出: %v", userID, err)
		}
	}()
}

// StartExisting 为启动时已存在的所有用户各起一个消费者。
func (m *dispatchManager) StartExisting() {
	users, err := m.db.ListAllUserIDs(m.ctx)
	if err != nil {
		log.Printf("[push] 列出用户失败: %v", err)
		return
	}
	for _, uid := range users {
		m.Ensure(uid)
	}
	log.Printf("[push] 已为 %d 个用户启动 push 消费者", len(users))
}
