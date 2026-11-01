package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/pprof"
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
	go bp.Run(ctx)
	slog.Info("dispatch backpressure configured",
		"event", "server.backpressure.configured",
		"pending_max_age_sec", int(cfg.PendingMaxAge()/time.Second),
		"enabled", cfg.PendingMaxAge() > 0,
		"note", "0 = 不启用；超阈时 POST /v1/events · POST /v1/hooks/{token} · POST /v1/sources/{id}/fire 返回 503+Retry-After，读端点不受影响",
	)

	// 控制台 API（#59）：8 个新端点，全部走组织维度（I10）
	consoleH := &consoleHandler{db: db, auth: authSvc}

	// 统计（/v1/stats）是**命名空间级**资源（契约 §6.2）：
	// 它需要和资源端点**同一套**命名空间解析，所以复用 consoleH 的解析函数
	// （注入而不是内联：错误码映射只有一处真相，抄一份必然漂移）。
	statsH := &statsHandler{
		db:               db,
		resolveNamespace: consoleH.resolveNamespace,
	}

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
			"namespaces", n,
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
	// events 保留期（#229）：把部署配置注入巡检。默认 30 天（cfg.EventRetentionSec() 已换算成秒）；
	// 0 = 永久保留（store 侧 EventRetentionDisabled → 整个 events 阶段跳过）。
	//
	// 注意 0 在这里的含义是「永久保留」而**不是**「保留 0 秒」——lease 侧不再替调用方补默认值，
	// 补了就会让「不显式配置 = 删 30 天前的事件」变成不可逆的默认行为。
	sweeper.RetentionEventSec = cfg.EventRetentionSec()
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
				// 作用域是任务自己的命名空间（#162）：SetNotBefore 的 WHERE 是
				// `namespace_id = ? AND id = ?`，传错 id 会一行也更新不到 → 退避永远
				// 设不上（返回 ErrNotFound）→ 没节点的任务每 5s 被重扫一次、日志刷屏。
				if serr := db.SetNotBefore(sctx, task.NamespaceID, task.ID, store.Now()+backoff); serr != nil {
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
	mux.Handle("POST /v1/events", noStore(bp.Wrap(eventH)))
	mux.Handle("GET /v1/events", noStore(eventH))
	// 控制台端点：登录会话 + 组织维度校验
	mux.Handle("GET /v1/tasks", noStore(sessMW(http.HandlerFunc(consoleH.tasks))))
	mux.Handle("GET /v1/tasks/{id}", noStore(sessMW(http.HandlerFunc(consoleH.task))))
	// trace 视图（#142）：一条 trace 的全景（events + tasks + pruned）。
	// 剪枝从 #142 起落进了 events（source='route'），这个端点是「为什么没跑」的查询出口。
	mux.Handle("GET /v1/traces/{id}", noStore(sessMW(http.HandlerFunc(consoleH.trace))))
	// 控制台读事件流：走会话 Cookie（机器入口的 GET /v1/events 走 Bearer Key）。
	mux.Handle("GET /v1/console/events", noStore(sessMW(http.HandlerFunc(consoleH.events))))
	mux.Handle("POST /v1/tasks/{id}/cancel", noStore(sessMW(http.HandlerFunc(consoleH.cancelTask))))
	mux.Handle("GET /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("POST /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	// PATCH 只改 display_name（装饰字段），不是能力定义的新版本。
	mux.Handle("PATCH /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("GET /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("POST /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	mux.Handle("DELETE /v1/edges", noStore(sessMW(http.HandlerFunc(consoleH.edges))))
	// 源（origin）：事件的入口实例。增删改查 + 手动触发。
	//
	// 与控制台其它读接口一样走会话 Cookie。
	// 源是命名空间资源（契约 §6.2）：除 webhook 入口外，每个方法都要 namespaceId。
	srcH := &sourceHandler{
		db:               db,
		commander:        srcTrigger,
		resolveNamespace: consoleH.resolveNamespace,
	}
	mux.Handle("GET /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("POST /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("GET /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("PATCH /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("DELETE /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	// fire 走 sessMW **里面**再包背压：未登录的请求先被 401 挡掉，
	// 不让外部从 503/200 的差别里读出「中枢现在积压了吗」。
	mux.Handle("POST /v1/sources/{id}/fire", noStore(sessMW(bp.Wrap(http.HandlerFunc(srcH.fire)))))
	mux.Handle("POST /v1/sources/{id}/dry-run", noStore(sessMW(http.HandlerFunc(srcH.dryRun))))
	// 草稿试运行：还没保存的配置也能先验一遍（弹窗里改完就想试）
	mux.Handle("POST /v1/sources/dry-run", noStore(sessMW(http.HandlerFunc(srcH.dryRun))))
	// webhook 入口**不需要会话**：鉴权是 URL 里的 token。
	// 外部系统（GitHub / 监控告警）不可能带我们的 Cookie。
	mux.Handle("POST /v1/hooks/{token}", noStore(bp.Wrap(http.HandlerFunc(srcH.hook))))

	// --- 预案（playbook，#193 / 契约 §6）---
	// 5 个端点**全部**用 resolveNamespaceCreate：缺参 400 / `*` 400 / 不存在 404 /
	// 不存在 404（预案不提供跨命名空间汇总，体检结论只能按单个 ns 算）。
	pbH := &playbookHandler{db: db, console: consoleH}
	mux.Handle("GET /v1/playbooks", noStore(sessMW(http.HandlerFunc(pbH.list))))
	mux.Handle("POST /v1/playbooks", noStore(sessMW(http.HandlerFunc(pbH.create))))
	mux.Handle("GET /v1/playbooks/{idOrSlug}", noStore(sessMW(http.HandlerFunc(pbH.byKey))))
	mux.Handle("DELETE /v1/playbooks/{idOrSlug}", noStore(sessMW(http.HandlerFunc(pbH.byKey))))
	mux.Handle("POST /v1/playbooks/{idOrSlug}/install", noStore(sessMW(http.HandlerFunc(pbH.install))))

	// --- 命名空间自身（#162 / 契约 §6.1）---
	// 组织级资源：请求**不带** namespaceId（问"这个命名空间属于哪个命名空间"是自指）。
	// slug 建了不可改（PATCH 只改 name），删除有 force 档（见 namespace_handlers.go）。
	nsH := &namespaceHandler{db: db}
	mux.Handle("GET /v1/namespaces", noStore(sessMW(http.HandlerFunc(nsH.list))))
	mux.Handle("POST /v1/namespaces", noStore(sessMW(http.HandlerFunc(nsH.create))))
	mux.Handle("PATCH /v1/namespaces/{id}", noStore(sessMW(http.HandlerFunc(nsH.byID))))
	mux.Handle("DELETE /v1/namespaces/{id}", noStore(sessMW(http.HandlerFunc(nsH.byID))))

	mux.Handle("POST /v1/nodes", noStore(sessMW(http.HandlerFunc(consoleH.createNode))))
	mux.Handle("GET /v1/nodes", noStore(sessMW(http.HandlerFunc(consoleH.nodes))))
	mux.Handle("GET /v1/nodes/{id}", noStore(sessMW(http.HandlerFunc(consoleH.nodeByID))))
	mux.Handle("POST /v1/nodes/{id}/drain", noStore(sessMW(http.HandlerFunc(consoleH.drainNode))))

	// --- 部署模型（console-declares epic，契约 CONTRACT-protocol §11）---
	//
	// 这是 v2 的核心：能力归属不再由节点在 hello 里自报，而是「控制台部署 + 节点确认」。
	// hub 注入是**可空**的：没有连接层时部署仍会建行（state=pending），
	// 只是逐台返回 pushed=false —— 与「节点离线」完全同构，UI 不必区别对待。
	depH := &deployHandler{db: db, console: consoleH}
	if nodeHub != nil {
		depH.hub = nodeHub
	}
	mux.Handle("GET /v1/capability-templates", noStore(sessMW(http.HandlerFunc(depH.capabilityTemplates))))
	mux.Handle("GET /v1/artifacts", noStore(sessMW(http.HandlerFunc(depH.artifacts))))
	mux.Handle("GET /v1/deployments", noStore(sessMW(http.HandlerFunc(depH.deployments))))
	mux.Handle("POST /v1/deployments", noStore(sessMW(http.HandlerFunc(depH.deployments))))
	mux.Handle("DELETE /v1/deployments", noStore(sessMW(http.HandlerFunc(depH.deployments))))

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
	return &App{Handler: gzipJSON(accessLog(mux)), DB: db, AuthSvc: authSvc}
}
