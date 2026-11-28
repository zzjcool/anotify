// Package server 装配 HTTP 服务：路由、中间件、静态资源（CDN 缓存分级）。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
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

	// defaultAgentBinPath 是中枢自带的**节点二进制**（v4 叫 anode）默认路径（CONTRACT-agent-upgrade §2）。
	// 容器镜像把它放在 /app/anode（Dockerfile 的 COPY --from=build /anode /app/anode）；
	// 文件不存在 = 自升级功能**未启用**（不是错误）。
	// v3 的 /app/agent（cmd/agent）已随节点产品换代为 anode 退役：默认路径若留在旧值上，
	// 只会让裸机/非镜像部署静默“没带二进制”（仅靠镜像里的 ANOTIFY_AGENT_BIN 兜底）。
	defaultAgentBinPath = "/app/anode"

	// maxEventRetentionDays / maxPendingMaxAgeSec 是**防溢出上限**（不是产品策略）。
	//
	// 两个值都会被乘上常数变成 int / time.Duration，而 Atoi 能吃下到 9.2e18：乘完会
	// **回绕**，方向极其危险（保留期回绕 → 一次删掉几乎全库；背压回绕成 290ms → 全站
	// 立刻 503）。所以超上限钳到上限（保住「想永不删/几乎不触发」的方向），看不懂的
	// 输入仍回落默认（见 retentionDaysFromEnv）。手滑多打几个 0 是常见事故来源。
	maxEventRetentionDays = 36500     // 100 年：再长就等于永久保留，不如显式配 0
	maxPendingMaxAgeSec   = 7 * 86400 // 7 天：超过这个量级已经不是「背压」，是「长期停机」

	// 派发预算的默认值（CONTRACT.md A7）：直接引 route 的两个常量——同一个 env
	// 出现两个默认值就是「升级前后行为不一致」的来源。
	defaultMaxHops         = route.DefaultMaxHops
	defaultTraceTaskBudget = route.DefaultTraceTaskBudget

	// 自助注册策略（#574）：默认 first-user-only = 升级后立刻关闭匿名注册（见 CHANGELOG）；
	// invite 是第 2 步的值（未落地，配了拒绝启动）；closed 是派生状态，不是配置取值。
	registrationFirstUserOnly = "first-user-only"
	registrationOpen          = "open"
	registrationInvite        = "invite"
	registrationClosed        = "closed"

	defaultRegistration = registrationFirstUserOnly
)

// Config 服务配置（全部可用环境变量覆盖）。
type Config struct {
	Addr       string // 监听地址，默认 :8080
	DBPath     string // SQLite 路径，默认 ./anotify.db
	CDNPrefix  string // 静态资源 CDN 前缀（如 https://cdn.trycloudflare.com）；空=本地 embed
	StaticDir  string // 本地静态目录（开发期），默认 ./dist
	RPDisplay  string // WebAuthn Relying Party 显示名
	RPID       string // WebAuthn RP ID（域名）
	RPOrigin   string // WebAuthn Origin（含协议）
	TrustProxy bool   // 信任 X-Forwarded-For（仅在反代 cloudflared/nginx 后开启）
	LogLevel   string // 日志级别：debug/info/warn/error，默认 info
	LogFormat  string // 日志格式：json/text，默认 json

	// Pprof 打开 /debug/pprof/*（性能评估用，默认关闭）。
	//
	// 为什么要有开关而不是始终开：pprof 端点会泄露 goroutine 栈与堆分布，
	// 生产环境不该默认暴露。评估/排障时用 ANOTIFY_PPROF=1 打开，
	// 采样完就关（pprof 的 CPU 采样本身有 ~5% 开销）。
	Pprof bool

	// BootID 是**本次进程启动**的标识，由 /health 回显。
	//
	// 为什么需要它：/health 只回 {"ok":true} 时无法区分「我起的那个进程」与「同端口
	// 上别人的/上次残留的进程」——两者都 200，e2e 会**静默连上别人的后端**（随机 401、
	// 造数 undefined，见 issue #215）。默认每次启动随机生成；显式设 ANOTIFY_BOOT_ID
	// 时用它，便于外部在启动前就知道该等哪个实例。
	BootID string

	// HookRatePerMin / HookRatePerIPPerMin 是 POST /v1/hooks/{token} 的双维度限流额度
	// （每分钟），见 ratelimit.go 的 rateLimitedHook。
	//
	// 为什么两维：token 是攻击者能换的东西，只按 token 限流换个 token 就绕过；只按 IP
	// 限流会把「一个出口 IP + 批量告警」按 60/min 卡死（issue #245：从第 61 条起全 429）。
	// 取值语义（hookRateFromEnv）：空/"0"/非法 → 默认（600/1200）；负数或 "off" → 关闭
	// 该维度（configureHookRateLimit 打 WARN 留痕）；正数 → 原样使用。
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
	// 为什么给运维这个旋钮：这两个 TTL 是「超管禁用用户 / 吊销 Key 后本进程还要多久
	// 才一定不再放行」的上界（正常路径有同步失效钩子，TTL 兜的是直接改库这类绕过）。
	// 合规严格时可调到 1s 或关掉，代价只是吞吐。
	SessionCacheTTL time.Duration
	APIKeyCacheTTL  time.Duration

	// AgentBin 是中枢自带的 agent 二进制路径（CONTRACT-agent-upgrade §2）。
	//
	// 它是**升级源**：节点用 GET /v1/node/binary 下载它，控制台据此下发升级。
	// 空 → 用 defaultAgentBinPath；文件不存在 → 功能未启用（启动只 WARN，不是错误：
	// 绝大多数开发/裸机部署本来就不带 agent 二进制）。
	AgentBin string

	// EventRetentionDays 是 events 的保留天数（#229）。0 = **永久保留**（不清理）。
	//
	// 语义（解析见 retentionDaysFromEnv）：
	//   - 未设置 / 空     → 默认 30 天；
	//   - "0"             → 0 = 永久保留（events 会持续增长，启动日志会 WARN）；
	//   - 负数            → 0（同样是永久保留；解析层不报错，由接线层打 WARN 留痕）；
	//   - 非数字（手滑）  → 默认 30 天（写错一个 env 不该改变保留策略的方向：
	//     宁可照默认多留 30 天，也不要因为一个拼写错误变成永久保留）。
	//
	// 为什么给运维这个旋钮：events 是「只进不出」的增长来源，30 天是默认折中；合规要求
	// 长期留存可调大，0 是显式承担容量风险（auto_vacuum=0：清理让文件平台化而非变小）。
	EventRetentionDays int

	// EventRetentionDaysSet 表示 ANOTIFY_EVENT_RETENTION_DAYS 被**显式设置**过（非空）。
	//
	// 为什么需要单独一个布尔（#005）：env 现在的职责只是「空库首次启动播种 system_config」，
	// 而「未设置」与「显式设成默认值 30」在数值上相同、语义却完全不同：
	//   - 未设置 → **不写库**（保住「无行 = store 默认值」这条事实，别把默认值固化进库，
	//     否则以后改默认值，老库永远不会跟上）；
	//   - 显式设 → 空库时播种一次（保留运维已有的部署习惯），已有行则忽略并 WARN。
	//
	// 用它来区分，而不是拿 EventRetentionDays == defaultEventRetentionDays 猜：
	// 「显式 30」与「根本没配」在启动日志和库里必须是两件不同的事。
	EventRetentionDaysSet bool

	// PendingMaxAgeSec 是派发背压阈值（#228）：最老 pending 任务的等待秒数**超过**它，
	// 新事件入口返回 503。0 = 不启用（永远放行）。
	//
	// 语义（解析见 pendingMaxAgeFromEnv）：未设置 / 空 → 120；"0" → 0（不启用）；
	// 负数 → 0（不启用）；非数字 → 120（手滑不改变默认保护）。
	//
	// 判定口径刻意是**等待时长**而不是 pending 行数：行数随命名空间数 / 能力数 / 流量口径
	// 漂移，跨部署不可比；「最老的 pending 等了多久」才与部署规模无关。
	PendingMaxAgeSec int

	// MaxHops / TraceTaskBudget 是派发的两个预算（CONTRACT.md A7），由 route.Router
	// 每次派发消耗。
	//
	// 语义（解析见 positiveIntFromEnv）：未设置 / 空 → 默认（hop 16 / trace 512）；
	// 正整数 → 原值；其它（0 / 负数 / 非数字）→ **启动即失败**（Validate 报错），
	// 不像保留期那样回落默认：预算是一条防线（环 + 无限扇出的兜底），一个写错的
	// env 把防线悄悄换回默认值，比拒绝启动更难查。
	MaxHops         int
	TraceTaskBudget int

	// StatsIntervalSec 是运行期 SQLite 统计维护的周期（秒，#265）。
	//
	// 语义：
	//   - 未设置 / 空      → 0 = 用 lease 层默认值（6 小时）
	//   - "0"              → **禁用**周期维护（只在启动时问一次）
	//   - 正数              → 指定周期（上限 7 天）
	//   - 非数字（手滑）    → 0 = 用默认值（不因为拼错就把维护关掉）
	//
	// 为什么"0 = 禁用"而"未设置 = 默认"：这两件事语义完全不同，必须能区分。
	// 用同一个 0 表示两者会让"关掉周期 ANALYZE"变成不可达（运维想关只能改代码）。
	StatsIntervalSec int

	// ArtifactSpoolDir 是中枢托管运行产物字节的**私有系统目录**（#332 / A1）。
	//
	// 语义（解析见 artifactSpoolDirFromEnv）：
	//   - 未设置 / 空   → 用 DBPath 同目录下的 artifacts/（例如 ./anotify.db → ./artifacts）；
	//   - 显式路径       → 原值。
	//
	// 为什么默认跟着数据库走：产物副本与「引用它的 manifest 行」必须一起备份/一起迁移，
	// 分开放会让恢复后出现「元数据在、字节在别处」的静默 410。目录由 resultspool 在
	// 首次需要时创建（0700），不在这里 mkdir：配置文件解析不该有副作用。
	ArtifactSpoolDir string

	// ArtifactGCIntervalSec 是产物字节 GC（ExpireArtifactObjects + purge + ack）的周期（秒）。
	//
	// 语义（解析见 artifactGCIntervalFromEnv，与 StatsIntervalSec 同构）：
	//   - 未设置 / 手滑 → 300（5 分钟：过期副本的回收延迟上限，足够小且不刷库）；
	//   - "0"           → **禁用**周期回收（显式运维选择；已过期副本仍会被下次启用时清掉）；
	//   - 正数           → 原值（上限 7 天）。
	//
	// 保留期事实（7 天副本 / 30 天事实 / 0 永久）在 store 侧（expires_at），这里只是驱动器。
	ArtifactGCIntervalSec int

	// CapabilityGCIntervalSec 是已删能力（capability tombstone）**物理回收**的周期（秒，issue 003）。
	// 语义同 ArtifactGCIntervalSec：未设置/手滑 → 3600；"0"/负数 → 禁用；正数 → 原值（上限 1 天）。
	// 默认 1 小时而不是 5 分钟：purge 全程持 `BEGIN IMMEDIATE` 写锁扫 tombstone 与回执
	// `json_tree`（#628/#551），按分钟跑等于持续与事件入口抢写锁。
	CapabilityGCIntervalSec int

	// LeaseTTL 是中枢授予/续租的租约时长（#661）：未设置/空 → 协议默认 180s；
	// "180s"/"3m"/"180"（纯数字按秒）→ 原值；其它（0/负数/低于下限/写错）→ 启动即拒。
	// 写错就拒而不是回落默认：缩小的租约是**静默**的危险（#661 隧道抖一下整单失联），
	// 而回落值往往正好比运维想配的更小。
	LeaseTTL time.Duration

	// Registration 是自助注册策略（#574，ANOTIFY_REGISTRATION）：first-user-only（默认，
	// 仅空库首个用户可注册，之后 403）/ open（任何人可注册，启动 WARN）/ invite（未实现，拒绝启动）。
	// 闸门在 store 的注册事务里（判定与 INSERT 同事务），这里只解析、校验、装配期注入。
	Registration string
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

// retentionDaysSetFromEnv 报告保留期 env 是否被**显式设置**（非空）。
//
// 判据必须与 retentionDaysFromEnv 的入口判断逐字一致（都是 `TrimSpace != ""`）：
// 两个函数一旦漂移，就会出现「解析层认为没配（回落 30）而接线层认为配了（去播种 30）」
// 或者反过来的组合，而两种都表现为静默的错值。
//
// 空串等价于「未设置」：os.Getenv 对「未设」与「设为空」都返回 ""，用数值猜
// （== 默认值）则会把「显式写 30」误判成「没配」。
func retentionDaysSetFromEnv(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
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

// StatsInterval 把配置换成 lease 层要的周期。
//
// 三种取值对应三种语义，别合并：
//
//	0（未设置） → 0   = lease 用默认值 6h
//	> 0         → 秒数
//	-1          → **禁用**（ANOTIFY_STATS_INTERVAL_SEC=0 的显式选择）
func (c Config) StatsInterval() time.Duration {
	switch {
	case c.StatsIntervalSec > 0:
		return time.Duration(c.StatsIntervalSec) * time.Second
	case c.StatsIntervalSec == 0:
		return 0 // 用默认
	default:
		return -1 // 禁用
	}
}

// statsIntervalFromEnv 读取 ANOTIFY_STATS_INTERVAL_SEC（#265）。
//
// 未设置/非法 → 0（用默认）；"0" → -1（禁用）；正数 → 原值（上限 7 天）。
func statsIntervalFromEnv(key string) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0 // 手滑不改变默认策略
	}
	if n <= 0 {
		return -1 // 显式禁用
	}
	const maxSec = 7 * 24 * 3600
	if n > maxSec {
		return maxSec // 溢出/误配保护：再长就等于"不维护"了
	}
	return n
}

// ArtifactSpoolPath 归一化产物托管根（空 → DBPath 同目录的 artifacts/）。
//
// 归一化只在这里做一次：config 的零值必须等同「用默认」（测试与内嵌部署会直接
// 构造 Config），handler 与 mux 都调这个方法，不各自猜。
func (c Config) ArtifactSpoolPath() string {
	if strings.TrimSpace(c.ArtifactSpoolDir) != "" {
		return strings.TrimSpace(c.ArtifactSpoolDir)
	}
	dir := filepath.Dir(strings.TrimSpace(c.DBPath))
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "artifacts")
}

// ArtifactGCInterval 把配置换算成 GC 周期：<=0 = 禁用（不启 goroutine）。
//
// 与 StatsInterval 的区别：这里 0 与「未设置」都是禁用，因为默认值已经由
// artifactGCIntervalFromEnv 填成 300 —— 直接构造 Config 的调用方（测试）拿到的是
// 明确的「不启动」，不会凭空多一条后台循环。
func (c Config) ArtifactGCInterval() time.Duration {
	if c.ArtifactGCIntervalSec <= 0 {
		return 0
	}
	return time.Duration(c.ArtifactGCIntervalSec) * time.Second
}

// CapabilityGCInterval 把配置换算成 tombstone 回收周期：<=0 = 禁用（不启 goroutine）。
//
// 与 ArtifactGCInterval 逐字同构：0 与「未设置」都是禁用，因为默认值已经由
// capabilityGCIntervalFromEnv 填成 3600 —— 直接构造 Config 的调用方（测试、内嵌部署）
// 拿到的是明确的「不启动」，不会凭空多一条后台循环。
func (c Config) CapabilityGCInterval() time.Duration {
	if c.CapabilityGCIntervalSec <= 0 {
		return 0
	}
	return time.Duration(c.CapabilityGCIntervalSec) * time.Second
}

// capabilityGCIntervalFromEnv 读 ANOTIFY_CAPABILITY_GC_INTERVAL_SEC（issue 003）。
//
// 未设置/非法 → 3600（默认 1 小时）；"0"/负数 → 0（禁用）；正数 → 原值（上限 1 天）。
//
// 非法值回落默认而不是 0：`0` 是「关掉回收」这种不可逆方向（tombstone 只增不减）的
// 显式选择，一个拼写错误绝不能替运维做出这个决定。
func capabilityGCIntervalFromEnv(key string) int {
	const defSec = 3600
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defSec
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defSec // 手滑不改变默认策略（与 artifactGCIntervalFromEnv 同原则）
	}
	if n <= 0 {
		return 0 // 显式禁用
	}
	const maxSec = 24 * 3600
	if n > maxSec {
		return maxSec
	}
	return n
}

// artifactGCIntervalFromEnv 读 ANOTIFY_ARTIFACT_GC_INTERVAL_SEC（#332）。
//
// 未设置/非法 → 300（默认 5 分钟）；"0"/负数 → 0（禁用）；正数 → 原值（上限 7 天）。
func artifactGCIntervalFromEnv(key string) int {
	const defSec = 300
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defSec
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defSec // 手滑不改变默认策略（与 retentionDaysFromEnv 同原则）
	}
	if n <= 0 {
		return 0 // 显式禁用
	}
	const maxSec = 7 * 24 * 3600
	if n > maxSec {
		return maxSec
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

// LeaseGrantTTL 归一化 `ANOTIFY_LEASE_TTL`：0（未配置 / 直接构造 Config）= 协议默认；
// 归一化只在这个方法里做一次，mux/测试都调它，不各自猜零值语义。
func (c Config) LeaseGrantTTL() time.Duration {
	if c.LeaseTTL == 0 {
		return protocol.DefaultLeaseTTL
	}
	return c.LeaseTTL
}

// registrationFromEnv 读 ANOTIFY_REGISTRATION：空 → def，其余归一后交给 Validate 判值域
// （安全策略不做「非法值回落默认」——手滑的正确方向是拒绝启动，不是换回默认值）。
func registrationFromEnv(key, def string) string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return def
	}
	return raw
}

// registrationMode 归一到生效值（未设置 / 空 = 默认）。
func (c Config) registrationMode() string {
	if raw := strings.ToLower(strings.TrimSpace(c.Registration)); raw != "" {
		return raw
	}
	return defaultRegistration
}

// RegistrationPolicy 把配置换成 store 的注册闸门；未知取值一律 first-user-only（fail-closed）。
func (c Config) RegistrationPolicy() store.RegistrationPolicy {
	if c.registrationMode() == registrationOpen {
		return store.RegistrationOpen
	}
	return store.RegistrationFirstUserOnly
}

// leaseTTLFromEnv 读 ANOTIFY_LEASE_TTL（#661）。与同文件其它 *_from_env 不同，这里
// 是「写错就拒」：未设置/空 → def（180s）；"180s"/"3m"/"180"（纯数字按秒）→ 原值；
// 其它（含 0 / 负数 / 写错）→ -1 哨兵，由 Validate 带着原值报错。缩小的租约是**静默**
// 的危险（隧道抽一下任务就被判失联），而手滑的值往往正好比默认小。
func leaseTTLFromEnv(key string, def time.Duration) time.Duration {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return -1
		}
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return -1
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
		StaticDir:  get("ANOTIFY_STATIC", ""),
		RPDisplay:  get("ANOTIFY_RP_DISPLAY", "Anotify"),
		RPID:       get("ANOTIFY_RP_ID", "localhost"),
		RPOrigin:   get("ANOTIFY_RP_ORIGIN", "http://localhost:8080"),
		TrustProxy: get("ANOTIFY_TRUST_PROXY", "") != "",
		LogLevel:   get("ANOTIFY_LOG_LEVEL", "info"),
		LogFormat:  get("ANOTIFY_LOG_FORMAT", "json"),
		BootID:     bootIDFromEnv(),
		Pprof:      get("ANOTIFY_PPROF", "") != "",

		// 自助注册策略（#574）：默认 first-user-only，非法值由 Validate 拦在启动那一刻。
		Registration: registrationFromEnv("ANOTIFY_REGISTRATION", defaultRegistration),

		// hooks 入口双维度限流（issue #245）：每 token 600/min、每 IP 1200/min 为默认。
		HookRatePerMin:      hookRateFromEnv("ANOTIFY_HOOK_RATE_PER_MIN", defaultHookRatePerMin),
		HookRatePerIPPerMin: hookRateFromEnv("ANOTIFY_HOOK_RATE_PER_IP_PER_MIN", defaultHookRatePerIPPerMin),

		// 鉴权热路径缓存（#241 / #227）：0=用默认、负数=关缓存。
		SessionCacheTTL: cacheTTLFromEnv("ANOTIFY_SESSION_CACHE_TTL", 0),
		APIKeyCacheTTL:  cacheTTLFromEnv("ANOTIFY_APIKEY_CACHE_TTL", 0),

		// 自带 agent 二进制（CONTRACT-agent-upgrade §2）：未配置时用容器内默认路径。
		AgentBin: get("ANOTIFY_AGENT_BIN", defaultAgentBinPath),

		// events 保留期（#229，默认 30 天，0 = 永久保留）与派发背压阈值
		// （#228，默认 120 秒，0 = 不启用）。
		//
		// 保留期的**生效值**在 store 的 system_config（#005）：这两个字段只负责
		// 「解析出一个候选值 + 它是否被显式设置」，接线层（mux.go）在空库首次启动时
		// 用它播种一次，之后运行期改保留期走 PUT /v1/system/config。
		EventRetentionDays:    retentionDaysFromEnv("ANOTIFY_EVENT_RETENTION_DAYS", defaultEventRetentionDays),
		EventRetentionDaysSet: retentionDaysSetFromEnv("ANOTIFY_EVENT_RETENTION_DAYS"),
		PendingMaxAgeSec:      pendingMaxAgeFromEnv("ANOTIFY_PENDING_MAX_AGE_SEC", defaultPendingMaxAgeSec),
		StatsIntervalSec:      statsIntervalFromEnv("ANOTIFY_STATS_INTERVAL_SEC"),

		// 运行产物托管目录与字节 GC 周期（#332）。默认目录 = DBPath 同目录 artifacts/。
		ArtifactSpoolDir:      get("ANOTIFY_ARTIFACT_SPOOL_DIR", ""),
		ArtifactGCIntervalSec: artifactGCIntervalFromEnv("ANOTIFY_ARTIFACT_GC_INTERVAL_SEC"),

		// 已删能力（tombstone）物理回收周期（issue 003）：默认 1 小时，0 = 禁用。
		CapabilityGCIntervalSec: capabilityGCIntervalFromEnv("ANOTIFY_CAPABILITY_GC_INTERVAL_SEC"),

		// 租约时长（#661）：默认 180s，非法值由 Validate 拦在启动那一刻。
		LeaseTTL: leaseTTLFromEnv("ANOTIFY_LEASE_TTL", protocol.DefaultLeaseTTL),

		// 派发预算（v3 契约 §2）：非法值由 Validate 拦在启动那一刻。
		//
		// v4 的 route.Router 不消费这两个值（中枢不再有跳数预算），默认值因此落在
		// 本包的 defaultMaxHops / defaultTraceTaskBudget（理由见那两条常量）。
		MaxHops:         positiveIntFromEnv("ANOTIFY_MAX_HOPS", defaultMaxHops),
		TraceTaskBudget: positiveIntFromEnv("ANOTIFY_TRACE_TASK_BUDGET", defaultTraceTaskBudget),
	}
}

// positiveIntFromEnv 读一个**必须为正整数**的额度环境变量。
//
// 与同文件里其它 *_from_env 的区别（那些都是「写错了回落默认」）：
//   - 未设置 / 空 → def（正常的「我没配」）；
//   - 其它任何解析不了 / <= 0 的值 → 0（一个**哨兵**，表示「配置非法」），
//     由 Validate() 变成启动失败。
//
// 为什么用 0 当哨兵而不是在这里报错：FromEnv 的签名是「只读 env，不返回错误」，
// 而校验失败必须在进程启动时**一次说清**（cmd/server 只调一次 Validate）。
// 0 永远不会是合法值（预算是正数），所以它不会与合法配置混淆。
func positiveIntFromEnv(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0 // 哨兵：配置非法（Validate 会带着原值报错）
	}
	return n
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
	// 派发预算必须是正整数（契约 §2）：非法值**启动即失败**，不静默用默认。
	// 报错带上 env 里的原值，否则运维只会看到「必须是正整数」却不知道自己配了什么。
	for _, it := range []struct {
		key string
		got int
	}{
		{"ANOTIFY_MAX_HOPS", c.MaxHops},
		{"ANOTIFY_TRACE_TASK_BUDGET", c.TraceTaskBudget},
	} {
		if it.got <= 0 {
			return fmt.Errorf("%s 必须是正整数（当前 %q）", it.key, os.Getenv(it.key))
		}
	}
	// 租约时长（#661）：低于协议下限 / 与续租间隔不成比例都**启动即拒**。
	// 带上 env 里的原值，否则运维只会看到「低于下限」却不知道自己配了什么。
	if err := protocol.ValidateLeaseTTL(c.LeaseGrantTTL(), protocol.LeaseRenewInterval); err != nil {
		return fmt.Errorf("ANOTIFY_LEASE_TTL 非法（当前 %q）：%w", os.Getenv("ANOTIFY_LEASE_TTL"), err)
	}
	// 自助注册策略（#574）：值域外的取值启动即拒；invite 单独报「尚未实现」。
	switch c.registrationMode() {
	case registrationFirstUserOnly, registrationOpen:
	case registrationInvite:
		return fmt.Errorf("ANOTIFY_REGISTRATION=invite 尚未实现（邀请表随 #574 第 2 步落地）；"+
			"请用 %q（空库首注册，默认）或 %q", registrationFirstUserOnly, registrationOpen)
	default:
		return fmt.Errorf("ANOTIFY_REGISTRATION 非法（当前 %q）：只接受 %q / %q",
			c.registrationMode(), registrationFirstUserOnly, registrationOpen)
	}
	return nil
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
