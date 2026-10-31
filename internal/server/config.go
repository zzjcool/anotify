// Package server 装配 HTTP 服务：路由、中间件、静态资源（CDN 缓存分级）。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 保留期 / 背压的默认值（#229 / #228）。
//
// 为什么必须是可被引用的常量而不是散落的字面量：解析层、接线层的启动日志、
// 以及测试都要引用同一个事实。默认值一旦出现第二份副本（比如一处写 30、
// 一处写 30*86400），改动时就只会改一半，症状是「配置明明改过但线上没生效」。
const (
	// defaultEventRetentionDays 是 events 的默认保留天数（#229，用户拍板 30 天）。
	defaultEventRetentionDays = 30
	// defaultPendingMaxAgeSec 是派发背压的默认阈值（#228）：最老 pending 任务的等待秒数。
	defaultPendingMaxAgeSec = 120

	// maxEventRetentionDays / maxPendingMaxAgeSec 是**防溢出上限**（不是产品策略）。
	//
	// 超上限时**钳到上限**（而不是回落默认）——两条路径（env 解析 / 直接写 Config）语义必须一致：
	//   - 保留期：填一个天文数字的人想的是「几乎永不删」，回落成 30 天等于**加速删**，方向反了；
	//   - 背压阈值：填一个天文数字的人想的是「几乎不触发」，回落成 120 秒等于**变严**，也反了。
	// 钳到上限（100 年 / 7 天）既保住「他想要的方向」，又保证乘法绝不回绕。
	// 非数字这类**看不懂**的输入仍然回落默认（见 retentionDaysFromEnv）。
	//
	// 为什么需要它们：两个值都会被乘上一个常数后变成 `int` / `time.Duration`：
	//   EventRetentionDays * 86400            → int
	//   PendingMaxAgeSec   * time.Second      → time.Duration（int64 纳秒，上限 ≈ 292 年）
	// `strconv.Atoi` 在 64 位上可以吃下到 9.2e18，乘完会**回绕**。回绕的方向极其危险：
	//   - 保留天数回绕成一个小正数 → cutoff = now - 小正数 → 一次删掉几乎全库（不可逆）；
	//   - 背压阈值回绕成 290ms 左右 → 全站新事件立刻开始 503。
	// 手滑多打几个 0 是很常见的事故来源，所以这里**按「非法值回落默认」处理**
	// （与既有的「写错一个 env 不该把防线关掉」同一原则），而不是让它回绕。
	maxEventRetentionDays = 36500     // 100 年：再长就等于永久保留，不如显式配 0
	maxPendingMaxAgeSec   = 7 * 86400 // 7 天：超过这个量级已经不是「背压」，是「长期停机」
)

// Config 服务配置（全部可用环境变量覆盖）。
type Config struct {
	Addr        string // 监听地址，默认 :8080
	DBPath      string // SQLite 路径，默认 ./anotify.db
	CDNPrefix   string // 静态资源 CDN 前缀（如 https://cdn.trycloudflare.com）；空=本地 embed
	StaticDir   string // 本地静态目录（开发期），默认 ./dist
	VAPIDPublic string // VAPID 公钥
	VAPIDPriv   string // VAPID 私钥（仅服务端）
	VAPIDSub    string // VAPID subject（mailto:）
	RPDisplay   string // WebAuthn Relying Party 显示名
	RPID        string // WebAuthn RP ID（域名）
	RPOrigin    string // WebAuthn Origin（含协议）
	TrustProxy  bool   // 信任 X-Forwarded-For（仅在反代 cloudflared/nginx 后开启）
	LogLevel    string // 日志级别：debug/info/warn/error，默认 info
	LogFormat   string // 日志格式：json/text，默认 json

	// Pprof 打开 /debug/pprof/*（性能评估用，默认关闭）。
	//
	// 为什么要有开关而不是始终开：pprof 端点会泄露 goroutine 栈与堆分布，
	// 生产环境不该默认暴露。评估/排障时用 ANOTIFY_PPROF=1 打开，
	// 采样完就关（pprof 的 CPU 采样本身有 ~5% 开销）。
	Pprof bool

	// BootID 是**本次进程启动**的标识，由 /health 回显。
	//
	// 为什么需要它：/health 只回 {"ok":true} 时，调用方无法区分「我起的那个进程」
	// 和「同一个端口上别人的/上次残留的进程」——两者都返回 200。
	// e2e 的并行套件、以及"上一次 e2e 的 server 没退干净"这两种场景，
	// 都会让测试**静默连上别人的后端**，症状是随机的 401 和造数 undefined
	// （表现为 0通过/0失败，看起来像超时或代码坏了，排查成本极高，见 issue #215）。
	//
	// 默认随机生成（每次启动都不同）；显式设 ANOTIFY_BOOT_ID 时用它，
	// 便于外部在启动前就知道该等哪个实例。
	BootID string

	// HookRatePerMin / HookRatePerIPPerMin 是 POST /v1/hooks/{token} 的双维度限流额度
	// （每分钟），见 ratelimit.go 的 rateLimitedHook。
	//
	// 为什么两维：这个端点的 token 是攻击者手里可以换的东西，只按 token 限流等于
	// 换个 token 就绕过；只按 IP 限流则会把「一个出口 IP + 批量告警」按 60/min 卡死
	// （issue #245：批量告警从第 61 条起全 429）。两维都过才放行。
	//
	// 取值语义（解析见 hookRateFromEnv / hookRateLimits）：
	//   - 空 / "0" / 非法值 → 用默认（600 每 token、1200 每 IP）；
	//   - 负数（如 -1，或字面的 "off"）→ **关闭该维度**（显式运维选择，
	//     configureHookRateLimit 会打 WARN 留痕）；
	//   - 正数 → 原样使用。
	HookRatePerMin      int
	HookRatePerIPPerMin int

	// SessionCacheTTL / APIKeyCacheTTL 是 #241 / #227 引入的两个进程内缓存
	// 的存活时间（两条独立的缓存，失效策略不同，故意不合并）。
	//
	// 语义（解析见 cacheTTLFromEnv）：
	//   - 未设置 / 空 / "0"   → 用 auth 包的默认值（会话 10s、API Key 60s）；
	//   - "off" / "-1" / ...   → **关闭该缓存**（所有请求走完整 DB / argon2 校验）；
	//   - "30s" / "30"         → 原值（纯数字按秒）。
	//
	// 为什么要给运维这个旋钮：这两个 TTL 同时是「超管禁用用户 / 吊销 Key 之后，
	// 本进程还要多久才一定不再放行」的上界（正常路径有同步失效钩子，TTL 兜的是
	// 绕过钩子的写路径，例如运维直接改库）。合规要求严格时可以调到 1s 或直接关掉，
	// 代价只是吞吐。
	SessionCacheTTL time.Duration
	APIKeyCacheTTL  time.Duration

	// EventRetentionDays 是 events 的保留天数（#229）。0 = **永久保留**（不清理）。
	//
	// 语义（解析见 retentionDaysFromEnv）：
	//   - 未设置 / 空     → 默认 30 天；
	//   - "0"             → 0 = 永久保留（events 会持续增长，启动日志会 WARN）；
	//   - 负数            → 0（同样是永久保留；解析层不报错，由接线层打 WARN 留痕）；
	//   - 非数字（手滑）  → 默认 30 天（写错一个 env 不该改变保留策略的方向：
	//     宁可照默认多留 30 天，也不要因为一个拼写错误变成永久保留）。
	//
	// 为什么要给运维这个旋钮：events 是「只进不出」的增长来源，30 天是默认折中；
	// 合规要求长期留存时可以调大，0 是显式承担容量风险的运维选择
	// （注意 auto_vacuum=0：清理让文件「平台化」而不是变小，见 store/retention.go）。
	EventRetentionDays int

	// PendingMaxAgeSec 是派发背压阈值（#228）：最老 pending 任务的等待秒数**超过**它，
	// 新事件入口返回 503。0 = 不启用（永远放行）。
	//
	// 语义（解析见 pendingMaxAgeFromEnv）：未设置 / 空 → 120；"0" → 0（不启用）；
	// 负数 → 0（不启用）；非数字 → 120（手滑不改变默认保护）。
	//
	// 判定口径刻意是**等待时长**而不是 pending 行数：行数随命名空间数 / 能力数 / 流量口径
	// 漂移，跨部署不可比；「最老的 pending 等了多久」才与部署规模无关。
	PendingMaxAgeSec int
}

// hookRateLimits 把两个字段归宿成最终额度（perToken, perIP）。
//
// 为什么要单开一个 helper：Config 不只由 FromEnv 构造（测试、内嵌部署会直接写结构体），
// 字段零值必须等同“用默认”，否则漏填就把防线关掉了。
// 保留负数原样返回：那是“关闭该维度”的信号，不能在解析层被吃掉。
func (c Config) hookRateLimits() (perToken, perIP int) {
	return hookRateOrDefault(c.HookRatePerMin, defaultHookRatePerMin),
		hookRateOrDefault(c.HookRatePerIPPerMin, defaultHookRatePerIPPerMin)
}

// hookRateOrDefault：0/未填 → 默认值；负数原样透传（= 关闭信号的载体）。
func hookRateOrDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// hookRateFromEnv 读取一个 hook 限流额度环境变量。
//
// 语义（契约 §2.2）：
//   - 未设置 / 空 / "0" / 非法值 → def（宁愿用默认，也不能因为写错一个 env 把防线关掉）；
//   - 负数（如 "-1"）或 "off"/"false"/"disable" → 负数（关闭该维度，由 configureHookRateLimit 打 WARN）；
//   - 正数 → 原值。
func hookRateFromEnv(key string, def int) int {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "", "0":
		return def
	case "off", "false", "disable", "disabled", "none":
		return -1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n == 0 {
		// 非法值不关闭限流：宁可少一个可配项，也不能让手滑变成降级。
		return def
	}
	return n
}

// retentionDaysFromEnv 读取 ANOTIFY_EVENT_RETENTION_DAYS（events 保留天数，#229）。
//
// 语义（契约 §1.3.1）：
//   - 未设置 / 空    → def（30 天）
//   - "0"            → 0 = **永久保留**（不清理）
//   - 负数           → 0（同样归一到永久保留）
//   - 非数字（手滑） → def（30 天）
//
// 负数与非法值都不在这里报错、也不在这里打日志：解析层只负责给出一个**确定的数**，
// 「负数被解释成永久保留」这类需要留痕的判断交给接线层（mux.go）打 WARN。
// 理由：FromEnv 在每次进程启动（以及测试）里都会被调用，把留痕塞进解析层会让同一条
// 告警出现在它不该出现的地方，而真正的运维可见性靠启动日志那一次就够。
//
// 非法值刻意回落默认而不是 0：0 是「永久保留」这种不可逆方向的显式选择，
// 一个拼写错误绝不能把保留期变成永久。
func retentionDaysFromEnv(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def // 手滑不改变默认策略
	}
	if n <= 0 {
		return 0 // "0" 是显式选择永久保留；负数一并归一
	}
	if n > maxEventRetentionDays {
		return maxEventRetentionDays // 溢出保护：钳到上限，理由见 maxEventRetentionDays 的注释
	}
	return n
}

// pendingMaxAgeFromEnv 读取 ANOTIFY_PENDING_MAX_AGE_SEC（派发背压阈值，#228）。
//
// 语义（契约 §1.3.1）：未设置 / 空 → def（120）；"0" → 0（**不启用**）；
// 负数 → 0（不启用）；非数字 → def（120）。
//
// 0 与负数都归一到「不启用」而不是「阈值为 0 秒 = 永远超阈」：后者会把全站新事件
// 挡在门外，是本旋钮最危险的失效方向。运维写 -1 的意图几乎必然是「关掉它」。
func pendingMaxAgeFromEnv(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def // 手滑不改变默认保护
	}
	if n <= 0 {
		return 0
	}
	if n > maxPendingMaxAgeSec {
		return maxPendingMaxAgeSec // 溢出保护：钳到上限，理由见 maxEventRetentionDays 的注释
	}
	return n
}

// EventRetentionSec 把天数换成 store.RetentionOptions.EventRetentionSec 的口径（秒）。
//
// 0 保持 0（= store.EventRetentionDisabled = **永久保留**），**不要**在这里把它变成
// 「保留 0 秒」或某个正数默认值：store 侧 <= 0 的含义是「跳过整个 events 阶段」，
// 补成默认值会让「不配置就不删 events」这条保证失效。
// 负数同样归一到 0（FromEnv 已经归一过，这里是防直接写结构体的调用方）。
func (c Config) EventRetentionSec() int {
	if c.EventRetentionDays <= 0 {
		return 0
	}
	const secPerDay = 24 * 60 * 60
	days := c.EventRetentionDays
	// 直接写 Config 结构体的调用方（测试、内嵌部署）可能绕过 FromEnv 的钳制，
	// 所以这里再钳一次。超出上限按上限算（而不是归零）：归零 = 永久保留，
	// 那不是「配了个很大的数」的人想要的语义。
	if days > maxEventRetentionDays {
		days = maxEventRetentionDays
	}
	return days * secPerDay
}

// PendingMaxAge 把秒换成 time.Duration 并**钳位**（防 time.Duration 溢出）。
//
// 为什么不在 mux.go 里直接 time.Duration(sec) * time.Second：那是一次乘法，
// int64 纳秒上限约 9.22e9 秒，超大值会回绕成很小的数 → 全站立刻开始 503。
// 钳到 maxPendingMaxAgeSec（7 天）：超过这个量级已经不是背压，是长期停机。
func (c Config) PendingMaxAge() time.Duration {
	n := c.PendingMaxAgeSec
	if n <= 0 {
		return 0
	}
	if n > maxPendingMaxAgeSec {
		n = maxPendingMaxAgeSec
	}
	return time.Duration(n) * time.Second
}

// cacheTTLFromEnv 读取一个缓存 TTL 环境变量。
//
// 语义：
//   - 未设置 / 空 / "0"        → def（默认值）
//   - "off"/"false"/"disable"/"-1" → -1（关闭该缓存；调用方据此走完整校验路径）
//   - 其它 → 按 Go duration 解析（"500ms"/"2m"），解析不了再按「纯整数秒」解析
//   - 仍然解析不了（手滑）      → def
//
// 非法值回落到默认而不是关闭缓存：写错一个 env 不该把性能优化整个关掉；
// 缓存本身不承担正确性（失效有同步钩子 + TTL 上界），所以保守方向是「照常开」。
func cacheTTLFromEnv(key string, def time.Duration) time.Duration {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "", "0":
		return def
	case "off", "false", "disable", "disabled", "none", "-1":
		return -1
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	// 纯数字按秒（"30" == 30s），排障时不用记单位。
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// FromEnv 从环境变量加载配置。
func FromEnv() Config {
	return Config{
		Addr:      get("ANOTIFY_ADDR", ":8080"),
		DBPath:    get("ANOTIFY_DB", "./anotify.db"),
		CDNPrefix: get("ANOTIFY_CDN_PREFIX", ""),
		// 默认空 → 使用 embed 内嵌前端（生产单二进制）；
		// 开发期显式设 ANOTIFY_STATIC=./web 走本地目录。
		StaticDir:   get("ANOTIFY_STATIC", ""),
		VAPIDPublic: get("ANOTIFY_VAPID_PUBLIC_KEY", ""),
		VAPIDPriv:   get("ANOTIFY_VAPID_PRIVATE_KEY", ""),
		VAPIDSub:    get("ANOTIFY_VAPID_SUBJECT", "mailto:notify@example.com"),
		RPDisplay:   get("ANOTIFY_RP_DISPLAY", "Anotify"),
		RPID:        get("ANOTIFY_RP_ID", "localhost"),
		RPOrigin:    get("ANOTIFY_RP_ORIGIN", "http://localhost:8080"),
		TrustProxy:  get("ANOTIFY_TRUST_PROXY", "") != "",
		LogLevel:    get("ANOTIFY_LOG_LEVEL", "info"),
		LogFormat:   get("ANOTIFY_LOG_FORMAT", "json"),
		BootID:      bootIDFromEnv(),
		Pprof:       get("ANOTIFY_PPROF", "") != "",

		// hooks 入口双维度限流（issue #245）：每 token 600/min、每 IP 1200/min 为默认。
		HookRatePerMin:      hookRateFromEnv("ANOTIFY_HOOK_RATE_PER_MIN", defaultHookRatePerMin),
		HookRatePerIPPerMin: hookRateFromEnv("ANOTIFY_HOOK_RATE_PER_IP_PER_MIN", defaultHookRatePerIPPerMin),

		// 鉴权热路径缓存（#241 / #227）：0=用默认、负数=关缓存。
		SessionCacheTTL: cacheTTLFromEnv("ANOTIFY_SESSION_CACHE_TTL", 0),
		APIKeyCacheTTL:  cacheTTLFromEnv("ANOTIFY_APIKEY_CACHE_TTL", 0),

		// events 保留期（#229，默认 30 天，0 = 永久保留）与派发背压阈值
		// （#228，默认 120 秒，0 = 不启用）。
		EventRetentionDays: retentionDaysFromEnv("ANOTIFY_EVENT_RETENTION_DAYS", defaultEventRetentionDays),
		PendingMaxAgeSec:   pendingMaxAgeFromEnv("ANOTIFY_PENDING_MAX_AGE_SEC", defaultPendingMaxAgeSec),
	}
}

// bootIDFromEnv 取 ANOTIFY_BOOT_ID，未设则随机生成 8 字节（16 位十六进制）。
func bootIDFromEnv() string {
	if v := os.Getenv("ANOTIFY_BOOT_ID"); v != "" {
		return v
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 拿不到随机数不该阻止启动：退化成固定值，身份校验会失效但服务可用。
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// Validate 校验关键配置。
func (c Config) Validate() error {
	if c.VAPIDPublic == "" || c.VAPIDPriv == "" {
		return fmt.Errorf("缺少 VAPID 配置（ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY）")
	}
	return nil
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
