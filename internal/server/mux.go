package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/api"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/lease"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/push"
	"github.com/zzjcool/anotify/internal/resultspool"
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

	// Spool 是中枢自有的产物字节池（#332）：nil = 托管根不可用（产物端点回 503）。
	Spool *resultspool.Store
	// ArtifactGC 是产物字节回收驱动（nil = 未配置周期，或 spool 不可用）。
	// 它由 NewApp 用装配期的 ctx 启动（`go ArtifactGC.Run(ctx)`）；测试可直接
	// 调 RunOnce 做**真实**回收验证，不需要等 ticker。
	ArtifactGC *resultspool.GC

	// background 追踪 NewApp 起的后台循环（背压采样 + 派发 / cron / 租约三条）。
	//
	// 它只为一件事存在：让「关停」有确定的 happens-before。取消 ctx 只是**信号**，
	// 循环可能正卡在一次库查询里；调用方若紧接着 db.Close()，那条查询就会拿到
	// `sql: database is closed`（#259 / #340 r3 的收尾竞态）。WaitBackground 返回
	// 才能保证「后台写者全部退出」——见测试夹具 newProductionAppForTest 的关停顺序。
	background sync.WaitGroup
}

// WaitBackground 阻塞到 NewApp 启动的所有后台循环返回（前提：先取消传给 NewApp 的 ctx）。
//
// 生产入口 cmd/server 不关库（进程直接退出），所以不需要调用；需要「关库」的调用方
// （测试夹具 / 将来的优雅停机）必须先 cancel 再 WaitBackground 再 db.Close。
func (a *App) WaitBackground() {
	if a == nil {
		return
	}
	a.background.Wait()
}

// trackBackground 登记一个后台 goroutine：调用方拿到 done 后必须 defer 调用它。
func (a *App) trackBackground() func() {
	a.background.Add(1)
	return a.background.Done
}

// NewMux 装配完整应用：/v1/* 动态 API（no-store），其余静态资源（CDN 缓存分级）。
func NewMux(cfg Config) http.Handler {
	return NewApp(context.Background(), cfg).Handler
}

// NewApp 装配完整应用（含后台派发 / cron / 租约三条循环）。
//
// v4 的装配顺序（本文件是唯一的 mux 写入者，别在别处再挂路由）：
//
//	store → auth → cli-auth / passkey-enroll → VAPID → 保留域 handler →
//	背压 → 会话中间件 → 限流 → route.Router / ws.Hub / source.Service / trigger / lease.Sweeper
//	→ 资源域 register(mux) → 节点协议 → 健康检查 / pprof / 静态兜底
//
// 刻意**不**再装配的东西（v4 退役，见 331 卡 D0.2 / D0.3）：旧中枢执行器包（连带
// notify.phone / push.CentralNotifier 的接线）、`ws.NewNodeHandler`（hub 自己
// 就是 /v1/node/stream 的 handler）、`db.BackfillBuiltinCapabilities`（内置能力播种随
// 中央执行域一起退役）、`source.Trigger` / `source.Scheduler`（换成 source.Service +
// trigger.Scheduler）、`lease.New` + Redispatch/Nudge/SetNotBefore 那一套重派回调
// （租约回收由 ws.Hub 的派发循环接手），以及 v3 扁平**资源**路由
// （/v1/capabilities、/v1/sources、/v1/playbooks、/v1/nodes、/v1/events 等）。
//
// 注意「保留域读模型」不是退役项（api-conventions §8.4/§8.5）：/v1/stats、/v1/orphans、
// /v1/namespaces/{ns}/deployments 仍由 newRetainedReadAPI 注册，形状不动、只统一错误体。
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

	// events 保留期（#229）：默认 30 天，0 = 永久保留。
	//
	// 为什么启动时必须打这一条：events 清理**不可逆**，而「默认 30 天」是一个真实的
	// 产品决策（用户拍板）。运维必须在重启后就能从日志里读到「这台机器会删 30 天前的
	// 事件」或者「这台机器永不删事件、磁盘会一直涨」——两种后果完全不同，不能只留在文档里。
	//
	// 数字口径（`scripts/perf/c/s6-capacity.sh` a 段实测，与批次 4 的 s2-retention.sh
	// 同量级）：一条空 payload 的 ingest 在磁盘上是 **679.9 B**（含派发剪枝留痕的额外行，
	// 一条 ingest 落 2 行 events），1KB payload 是 **1908.7 B**。按 38 ev/s 算，
	// 30 天窗口 ≈ 62.4 GiB（空 payload）/ 175.1 GiB（1KB）。
	// 完整的容量表与复现口径见 docs/perf/capacity-and-backpressure.md。
	//
	// 注意 `days=0` 有两个来源（env 未设 + 显式 0 / 负数都会被归一），日志里不能谎称
	// 「你设的是 0」——那会让「保留期被悄悄关掉」这条路径无法追查，所以 hint 里明写。
	if cfg.EventRetentionSec() > 0 {
		slog.Info("event retention configured",
			"event", "server.retention.configured",
			"days", cfg.EventRetentionDays,
			"events_retention_sec", cfg.EventRetentionSec(),
			"est_bytes_per_event", 679.9,
			"est_bytes_per_event_1kb_payload", 1908.7,
			"note", "events 超过保留期会被分批删除（不可逆）；idem_key 去重窗口缩成保留期同长；auto_vacuum=0 → 文件是平台化而不是变小",
		)
	} else {
		slog.Warn("event retention disabled: events are kept forever",
			"event", "server.retention.permanent",
			"days", 0,
			"est_bytes_per_event", 679.9,
			"note", "events 永久保留：磁盘会随写入量线性增长，必须自行做容量监控与告警（auto_vacuum=0，删了也不会还盘）",
			"hint", "ANOTIFY_EVENT_RETENTION_DAYS 未设置时默认 30；显式设 0 **或负数**都会落到这条分支（解析层把负数归一到 0），所以「没配」与「配错成负数」在这里长得一样——请确认部署配置",
			"est_30d_bytes_at_38ev_per_sec_gib", 62.4,
		)
	}

	// 3. 认证（WebAuthn + API Key）
	authSvc, err := auth.NewService(db, auth.Config{
		RPDisplayName: cfg.RPDisplay,
		RPID:          cfg.RPID,
		RPOrigins:     []string{cfg.RPOrigin},
		SessionTTL:    7 * 24 * time.Hour,
		SecureCookie:  cfg.RPOrigin != "" && len(cfg.RPOrigin) > 8 && cfg.RPOrigin[:5] == "https",
		// 鉴权热路径缓存（#227 / #241）：额度由 env 决定，0=默认、负数=关闭。
		// 两条缓存必须分开传入（失效策略不同，见各自构造器注释）。
		SessionCacheTTL: cfg.SessionCacheTTL,
		KeyCacheTTL:     cfg.APIKeyCacheTTL,
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

	// App 从这一刻起就存在：后台循环的 WaitGroup 挂在它上面（关停顺序见 App.background）。
	// Handler 在装配结束时赋值——中途的代码只往 mux 上挂路由，不读 app.Handler。
	app := &App{DB: db, AuthSvc: authSvc}

	// 3.5 CLI 设备授权管理器
	cliAuthMgr := auth.NewCliAuthManager(db, 0)

	// 3.6 Passkey 设备授权管理器（复用 cli_auth_sessions 表，kind=passkey）
	enrollMgr := auth.NewPasskeyEnrollManager(db, authSvc, 0)

	// 4. Web Push（P-256 VAPID 密钥）
	//
	// v4 只保留**密钥的装载与公钥下发**（GET /v1/vapid-public-key）：随旧中枢执行器包
	// 退役的 `push.CentralNotifier`（notify.phone 的内置执行者）带走了「谁发推送」这件事，
	// v4 里推送由节点的能力进程自己发（能力正文带它需要的密钥材料），中枢不再构造 Sender。
	// VAPID 缺失仍然只 WARN：绝大多数开发/裸机部署本来就没配推送，那不是启动错误。
	var vapidCfg *push.VAPIDConfig
	vapidCfg, vapidErr := push.LoadVAPID()
	if vapidErr != nil {
		vapidCfg = nil
		slog.Warn("VAPID not configured, push disabled",
			"event", "push.vapid.missing",
			"error", vapidErr.Error(),
		)
	}

	// 5. 保留域 HTTP 处理器（auth / admin / devices / keys / cli-auth / passkey-enroll）
	//
	// 这些域**不**走资源信封（api-conventions §8.3「保持现有路径与请求体」），所以它们
	// 的 handler 仍在各自的文件里，本函数只负责挂路由与中间件。
	authH := &authHandler{svc: authSvc}
	devicesH := &devicesHandler{db: db}
	keysH := &keysHandler{keys: authSvc.Keys(), db: db}
	cliAuthStaticFS := resolveStaticFS(cfg)
	cliAuthH := &cliAuthHandler{
		mgr:      cliAuthMgr,
		keys:     authSvc.Keys(),
		keysV:    keyValidator,
		db:       db,
		staticFS: cliAuthStaticFS,
	}

	enrollH := &passkeyEnrollHandler{mgr: enrollMgr, db: db}

	// 自带 agent 二进制（CONTRACT-agent-upgrade §2）：中枢同时是节点的**升级源**。
	// 文件不存在 = 功能未启用（loadAgentBinary 只 WARN），此时 GET /v1/node/binary
	// 返回 501、控制台不显示升级按钮——「不升级」是合法部署形态，不是启动错误。
	agentBin := loadAgentBinary(cfg.AgentBin, serverVersion())

	// 派发背压（#228）：判定信号 = **最老 pending 任务的等待时长**，超阈时**新事件入口**返回 503。
	//
	// 设计约束（请求路径不查 DB / 阈值 0 = 不启用 / 只拒新事件不动已入库任务 / 持续超阈
	// 过久要 fail-open）全部写在 internal/server/backpressure.go 的文件头，改动前先读那里。
	//
	// 编译期断言：*store.DB 必须实现 pendingAgeSource（backpressure.go 里的窄接口）。
	// 少了这一行，store 侧 OldestPendingCreatedAt 签名一漂移，背压就会**静默失效**——
	// 症状是「配了阈值但永远不拒」，与「确实没有积压」在日志上无法区分。
	var _ pendingAgeSource = (*store.DB)(nil)
	bp := newBackpressureGuard(db, backpressureOptions{
		// PendingMaxAge 而不是 time.Duration(sec)*time.Second：后者在超大值上会回绕
		// 成很小的数（= 全站立刻 503），钳位在 Config.PendingMaxAge 里。
		MaxAge: cfg.PendingMaxAge(),
	})
	// MaxAge <= 0 时 Run 立即返回（不启用的部署一个 goroutine、一次采样都不该多付）。
	// 采样循环是**后台写者**之一：它读库，所以必须进 background（关库前等它退出）。
	doneBP := app.trackBackground()
	go func() {
		defer doneBP()
		bp.Run(ctx)
	}()
	slog.Info("dispatch backpressure configured",
		"event", "server.backpressure.configured",
		"pending_max_age_sec", int(cfg.PendingMaxAge()/time.Second),
		"enabled", cfg.PendingMaxAge() > 0,
		"note", "0 = 不启用；超阈时 POST /v1/namespaces/{ns}/events · POST /v1/hooks/{token} · POST …/sources/{name}/fire 返回 503+Retry-After，读端点不受影响",
	)

	sessMW := authSvc.Sessions().Middleware
	// 管理后台：登录会话 + 超管权限（两层中间件）
	adminMW := sessMW(authSvc.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminH := &adminHandler{db: db, svc: authSvc}
		adminH.ServeHTTP(w, r)
	})))

	// 限速器信任代理头开关（仅在反代后开启）
	trustProxyHeaders = cfg.TrustProxy

	// hooks 入口双维度限流（#245）：额度由 env 决定。
	//
	// 必须在挂路由**之前**完成：包级限速器默认是 60/min（旧值），若只在服务处理请求时
	// 惰性重建，第一波请求就会先吃旧额度（正好是 issue #245 描述的那个 bug）。
	// perToken/perIP 为负数表示**关闭该维度**（显式运维选择，configureHookRateLimit 会打 WARN）。
	{
		perToken, perIP := cfg.hookRateLimits()
		configureHookRateLimit(perToken, perIP)
		slog.Info("hook rate limit configured",
			"event", "server.hook_rate.configured",
			"per_min_token", perToken,
			"per_min_ip", perIP,
		)
	}

	// 6. v4 运行期依赖（冻结契约 D1：资源域只拿这一份 serverDeps）
	//
	// 四个组件互相引用但都不持有对方的后台循环：Router 只做投影、Hub 做会话与派发、
	// source.Service 做入口归一化、trigger.Scheduler 只把 cron 到期的源喂给 Service。
	// 依赖方向单向（scheduler → service → router → store），所以可以在一个地方一次装好。
	router := route.New(db)
	hub := ws.NewHub(db, router, ws.WithLogger(slog.Default()))
	sources := source.New(db, router)
	sched := trigger.New(db, sources, slog.Default())
	sweeper := lease.NewSweeper(db, slog.Default())

	// 产物托管池（#332 / A1）：中枢自有的 0700 目录 + 0600 对象文件。
	//
	// 建不出来时**不**让整个进程起不来：/v1 的其余域与该能力无关，产物端点会回
	// 503 `artifact_upload_unavailable`（诚实的「没配置好」，不是假成功）。
	// 但日志必须 ERROR 级别——否则产物上传会静默降级成「节点侧 artifact_upload_failed」。
	spool, spoolErr := resultspool.New(cfg.ArtifactSpoolPath())
	if spoolErr != nil {
		slog.Error("artifact spool unavailable: uploads/downloads will return 503",
			"event", "server.artifact.spool.unavailable",
			"component", "resultspool",
			"path", cfg.ArtifactSpoolPath(),
			"error", spoolErr.Error(),
		)
		spool = nil
	} else {
		slog.Info("artifact spool configured",
			"event", "server.artifact.spool.configured",
			"path", spool.Root(),
			"gc_interval_sec", int(cfg.ArtifactGCInterval()/time.Second),
			"note", "副本 0700/0600；保留期 7 天由 store 的 retention_result_ttl_sec 决定（0 = 永久）",
		)
	}

	deps := &serverDeps{
		DB: db, Router: router, Sources: sources, Hub: hub, Sweeper: sweeper,
		// Now 是**毫秒**时钟（api_deps.go 的契约）：store.Now 是秒，不能直接注入，
		// 否则 stats 的 371 天窗口与 cancel_requested_at 会按 1970 年算（#340 r2 M-1）。
		// #332 的产物端点同样按这个契约取值（下载用 taskNowSeconds 折算成 store 的秒）。
		Log: slog.Default(), Now: nowMilli, Session: sessMW, Backpressure: bp.Wrap,
		Spool: spool,
		// namespace id → slug 缓存（slug 不可变，见 serverDeps.display 的注释）。
		display: &nsDisplay{},
	}

	// 产物字节 GC（#332）：Expire→删私有文件→Ack 的单一有界驱动。
	//
	// 保留期事实全部在 store（expires_at / 0=永久 / 活跃 ticket 保护），这里只负责
	// 「到点了就把字节删掉并记账」。周期 0 = 不启动（显式运维选择）。
	var artifactGC *resultspool.GC
	if spool != nil && cfg.ArtifactGCInterval() > 0 {
		gc, err := resultspool.NewGC(newArtifactGCStore(db), spool, resultspool.GCOptions{
			Interval: cfg.ArtifactGCInterval(),
			Now:      store.Now,
			Log:      slog.Default(),
		})
		if err != nil {
			slog.Error("artifact gc disabled: invalid configuration",
				"event", "server.artifact.gc.disabled",
				"error", err.Error(),
			)
		} else {
			artifactGC = gc
			go func() { _ = gc.Run(ctx) }()
		}
	}

	// 产物字节池与 GC 驱动挂到 App 上（#332）：测试直接取用做真实回收验证。
	app.Spool = spool
	app.ArtifactGC = artifactGC

	// 一条事件入口（Bearer key，机器调用）——与控制台的会话 Cookie 是两套鉴权。
	//
	// 顺序与 v3 的 POST /v1/events 一致：`noStore(bp.Wrap(eventH))`，即**背压在外、
	// 鉴权在 handler 内**。这不违反「背压在会话中间件内侧」那条纪律（那条说的是带
	// sessMW 的控制台端点：先 401 再谈积压），投递入口本来就不走会话中间件。
	eventH := api.NewEventHandler(sources, keyValidator)
	// ingress 的路径段是 slug，store 引用 ns_ id：解析收口复用 server 包同一份判据
	// （否则「事件能投递、资源列表 404」这类分叉又要付一次账）。
	eventH.ResolveNamespace = func(ctx context.Context, userID, raw string) (string, error) {
		ns, err := resolveNamespaceForUser(ctx, db, userID, raw)
		if err != nil {
			return "", err
		}
		return ns.ID, nil
	}

	// 后台循环（各自 go，单轮失败只记日志不停摆）：
	//
	//   - 派发：扫可派发任务 → 记录候选 assignment → 推 task.dispatch（250ms，交互延迟
	//     目标；每轮上限 100 条由 Hub 自己给）；
	//   - cron：把到期的源交给 source.Service 触发（1min，cron 的最小粒度就是分钟）；
	//   - 租约巡检：回收过期租约 + 保留期清理 + SQLite 统计维护（lease.DefaultInterval
	//     = 30s；保留期秒数来自 system config，不再从 mux 注入）。
	//
	// 三条循环都在 ctx 结束时正常退出（返回 ctx.Err()），所以这里只把**非取消**的
	// 退出记成 WARN：正常关库时的 context canceled 不该在日志里长得像故障。
	app.runBackgroundLoop(ctx, "dispatch", func(loopCtx context.Context) error {
		hub.RunDispatchLoop(loopCtx, 250*time.Millisecond)
		return nil
	})
	app.runBackgroundLoop(ctx, "cron", func(loopCtx context.Context) error {
		return sched.Run(loopCtx, time.Minute)
	})
	app.runBackgroundLoop(ctx, "lease", func(loopCtx context.Context) error {
		return sweeper.Run(loopCtx, lease.DefaultInterval)
	})
	// 派发预算（v3 契约 §2）在 v4 只有配置面（route.Router 不再消费），但启动日志仍要
	// 把它写出来：运维改了 env 却没生效时，至少要能在日志里看到「配的是多少」。
	slog.Info("dispatch budgets configured",
		"event", "server.budget.configured",
		"max_hops", cfg.MaxHops,
		"trace_task_budget", cfg.TraceTaskBudget,
		"note", "v4 保留配置与校验；中枢侧派发（route.Router + ws.Hub）不再消费这两个预算",
	)

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
	// 密码登录（契约 §2）：两条**显式**路由，保证语义化状态码——
	//   - login 匿名（限速在 handler 里做，不包 sessMW，否则未登录的合法请求会被 401）
	//   - 管理端点包 sessMW：未登录 401，而不是被 /v1/auth/ 兜底成 404/405。
	mux.Handle("/v1/auth/password/login", noStore(http.HandlerFunc(authH.passwordLogin)))
	// 用密码注册（契约「追加：用密码注册」§A2）：**匿名**端点，与 password/login 同族。
	// 与 login 不同，它只能显式注册：/v1/auth/ 的兜底 handler 不覆盖这个同族前缀，
	// 漏挂的话请求会落到静态资源（SPA 404），而那条路径没有任何测试会红。
	mux.Handle("/v1/auth/password/register", noStore(http.HandlerFunc(authH.passwordRegister)))
	mux.Handle("/v1/auth/password", noStore(sessMW(http.HandlerFunc(authH.password))))
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

	// --- v4 资源域（每个 domain 一个 register；路径为 /v1/namespaces/{ns}/… 或 cluster 级）---
	//
	// 顺序任意（ServeMux 按最具体模式匹配，不看注册顺序）。构造器名由契约冻结
	// （D1 / 331 卡），它们是这些路由的**唯一**注册点——本文件不再手写任何扁平资源路径，
	// 也不做「旧路径 → 新路径」的重定向（v4 明确不回兼容）。
	newCapabilityAPI(deps).register(mux)
	newSourceAPI(deps).register(mux)
	newPlaybookAPI(deps).register(mux)
	newNamespaceAPI(deps).register(mux)
	newTaskAPI(deps).register(mux)
	// 保留域读模型（§8.4）：stats / orphans / deployments。不是资源，也不套信封。
	newRetainedReadAPI(deps).register(mux)
	// Node 域多一步注入：它是升级源（agent 二进制）的消费方。
	//
	// 注入的是「目标版本 + 内容 sha256」两个事实（nodeUpgradeSource），不是整个
	// *agentBinary：资源域不需要字节，也不需要知道文件路径。未带二进制时
	// newAgentUpgradeSource 返回 nil 接口 → POST /v1/nodes/{name}/upgrade 回 501。
	nodeAPI := newNodeAPI(deps)
	nodeAPI.setUpgradeSource(newAgentUpgradeSource(agentBin))
	nodeAPI.register(mux)

	// --- 节点协议（#57）：WS，opaque token 鉴权在 hello 帧（Upgrade 之后）---
	//
	// v4 的 Hub **自己就是** /v1/node/stream 的 handler：会话建立、配置对齐、派发、
	// 执行子协议、日志与部署通知都在 hub 里。v3 的 nodeH（ws.NewNodeHandler）已退役，
	// 它身上的 Emit / Expects / OnCapacityFreed 三个注入点也随之消失——emit 与槽位释放
	// 现在都由 hub 直接处理（见 internal/ws 的包注释）。
	mux.Handle("GET /v1/node/stream", noStore(hub))
	// 二进制下载走 **node token**（不是会话）：调用方是节点上的 agent / CLI。
	mux.Handle("GET /v1/node/binary", noStore(&agentBinaryHandler{bin: agentBin, db: db}))
	// 运行产物上行（#332 / A1）：同样是 **node token**（不是会话），所以刻意挂在
	// 资源域之外——域内 register 一律包 session 中间件（节点请求会被 401）。
	// 授权不走中间件：handler 自己用 Bearer → token hash 定位节点，再由
	// store 的 Begin/Register 在事务内复核 attempt/lease/token/相位/撤销。
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+protocol.NodeTaskArtifactsPathInfix+"{sha256}",
		noStore(newArtifactUploadHandler(db, spool, slog.Default())))

	// --- 事件投递入口（Bearer key + 背压）---
	//
	// doc：api-conventions §8.1。事件**列表**（GET 同一路径）与 traces / console events
	// 由任务资源域注册（resource_task.go 的 newTaskAPI）：那是读模型，走会话，与本入口
	// 的两套鉴权互不干扰。
	mux.Handle("POST /v1/namespaces/{namespace}/events", noStore(bp.Wrap(eventH)))

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

	// 健康检查。除 ok 外还回 boot（本次进程启动标识），见 Config.BootID：
	// 只回 {"ok":true} 时调用方分不清"我起的进程"和"同端口上别人的进程"，
	// e2e 并行/残留进程会因此静默连错后端（issue #215）。
	bootID := cfg.BootID
	if bootID == "" {
		bootID = "unknown"
	}
	healthBody, _ := json.Marshal(map[string]any{"ok": true, "boot": bootID})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(healthBody)
	})

	// 性能评估（ANOTIFY_PPROF=1）：/debug/pprof/*
	//
	// 手动挂载而不是 `import _ "net/http/pprof"`：后者会注册到 DefaultServeMux，
	// 而本服务的请求根本不走 DefaultServeMux —— 那样打开开关也采不到任何东西。
	if cfg.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		slog.Warn("pprof 已开启（性能评估用）", "event", "server.pprof.enabled")
	}

	// --- 静态资源（CDN 缓存分级）---
	mux.Handle("/", resolveStatic(cfg))

	// 顺序不能掉：gzip 在 accessLog **外侧**。
	//
	// 原因不在性能，而在一个静默的类型断言：ratelimit.go 的 writeRateLimited 把限流
	// 维度递给最外层日志时写的是 `if sw, ok := w.(*statusWriter)`。gzip 若套在
	// accessLog 内侧，handler 收到的 w 就变成 *gzipResponseWriter，这条断言静默失败
	// → #245 修好的「429 日志带 retry_after / rate_limit」当场回归，且没有测试会红
	// （回归用例：gzip_test.go 的 TestGzipJSONKeepsStatusWriter）。
	// 当前顺序下：handler → *statusWriter → gzipResponseWriter → 底层 conn，
	// Hijack 逐层透传，WS 升级（/v1/node/stream）与访问日志均不受影响。
	app.Handler = gzipJSON(accessLog(mux))
	return app
}

// runBackgroundLoop 启动一条后台循环，并把「非取消」的退出记成 WARN。
//
// 三条循环（派发 / cron / 租约）都遵循同一份纪律：单轮失败只记日志、继续下一轮；
// 整个 Run 只在 ctx 结束时返回 ctx.Err()。所以这里把 ctx.Err() 当正常关闭处理——
// 否则每次优雅停机都会在日志里留下三条像故障的 WARN。
func (a *App) runBackgroundLoop(ctx context.Context, name string, run func(context.Context) error) {
	done := a.trackBackground()
	go func() {
		defer done()
		err := run(ctx)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		slog.Warn("background loop exited",
			"event", "server.loop.exited",
			"loop", name,
			"error", err.Error(),
		)
	}()
}
