/* affected-map.mjs — 「改动文件 → 要跑的 e2e 套件」的**显式映射表**（唯一真源）
 *
 * 为什么需要一张表，而不是从代码里猜：
 *   e2e 套件是**黑盒**（起真二进制 + 真 Chrome），代码里没有一条边声明「我盯着
 *   web-src/pages/node.html」。所以映射只能人工断言，但人工断言会腐烂 ——
 *   于是本表被两层检查夹住（`affected.test.mjs`）：
 *
 *     ① 完整性：`scripts/e2e/suites/*.mjs` 里每个套件必须有行、有非空理由；
 *     ② 反向对账：从套件源码（含它 import 的 fixtures/lib）里抓出来的 `.html`
 *        字面量，必须被本表**覆盖**。给套件新增一次页面访问却没更新表 ⇒ 红。
 *     ③ 死 glob：表里的每条 glob 必须至少匹配一个 git 跟踪文件（文件被改名/删除
 *        后表要么跟着改，要么红）。
 *
 * 判据是 **fail-closed**（`affected.mjs` 里实现）：
 *   只有本表显式登记的路径才会「选中某个套件」；没登记的一律当成
 *   「不能判定 ⇒ 跑全套」。所以表腐烂的代价是**多跑**，不会变成漏跑；
 *   `NO_E2E_IMPACT` 是唯一的「可以不跑」白名单，每条都要写理由。
 *
 * 口径（自 2026-11-18 起）：
 *   · pages 里的名字 = `web-src/pages/<name>.html` 与生成物 `web/<name>.html`；
 *     页面被删除（如 `source` 合并进 `source-detail`）时两处都要删，否则本表腐烂；
 *   · `pages: ALL` = 该套件对**全部**页面做矩阵遍历（i18n / ui_shift 类）；
 *   · paths 里的 glob 用 `**` / `*`（见 affected.mjs 的 globMatch）。
 */

/** 改了不用跑 e2e 的路径（每条必须写理由；不在这个清单里的路径一律保守处理）。 */
export const NO_E2E_IMPACT = [
	{ glob: "docs/**", why: "文档，e2e 不读 docs/" },
	{ glob: "**/*.md", why: "Markdown 文档，e2e 不读" },
	{ glob: ".pi-orchestrator/**", why: "协调者工作区（报告/契约草稿），不是运行输入", unversioned: true },
	{ glob: ".pi/**", why: "agent 会话元数据，不是运行输入", unversioned: true },
	{ glob: ".pi-subagents/**", why: "子 agent 会话记录，不是运行输入", unversioned: true },
	{ glob: ".reports/**", why: "测量/报告产物，不是运行输入", unversioned: true },
	{ glob: ".github/**", why: "CI 编排；改动它不会改变被 e2e 断言的产品行为" },
	// scripts/ 下的「不是运行期输入」的部分。注意 install-anode.sh / hash.mjs / inject-head.mjs
	// 不在其中（那三个真的在 e2e 路径上），由 affected.test.mjs 的「优先级冲突」用例兜住。
	{ glob: "scripts/check-*.mjs", why: "静态检查脚本本身；改它不改变任何运行期行为" },
	{ glob: "scripts/check_*.py", why: "静态检查脚本本身（python 版）" },
	{ glob: "scripts/*.test.mjs", why: "node 单测；不被任何 e2e 套件执行" },
	{ glob: "scripts/ws_test.mjs", why: "独立的 WS 手工验证脚本，不在 e2e 注册表里" },
	{ glob: "scripts/push_e2e.mjs", why: "推送手工验证脚本，不在 e2e 注册表里" },
	{ glob: "scripts/ui-audit-mobile.mjs", why: "手机端取数脚本，产物不进 e2e 断言" },
	{ glob: "scripts/integration.sh", why: "对着已启动服务的集成脚本，与 e2e 编排无关" },
	{ glob: "scripts/mock.sh", why: "本地 mock 服务启动脚本，e2e 不用它" },
	{ glob: "scripts/dev.sh", why: "开发环境脚本，e2e 不用它" },
	{ glob: "scripts/cli_auth_smoke.sh", why: "手工冒烟脚本，e2e 用 cli_auth 套件" },
	{ glob: "examples/**", why: "示例 playbook/能力包；e2e 用的是内联 fixture，不读 examples/" },
	{ glob: "E2E_TESTING.md", why: "文档" },
	{ glob: ".gitignore", why: "版本控制元数据" },
	{ glob: "LICENSE", why: "许可证文本" },
];

/** 命中即**全量**（比逐套件映射更安全：这些面里没有「只影响一个套件」的东西）。 */
export const FULL_SURFACES = [
	{ glob: "scripts/e2e/lib/**", why: "共享 harness（startServer/seed 的公共实现，所有套件都走它）" },
	{ glob: "scripts/e2e/run_all.sh", why: "总编排（构建 + 前置 + 委托）" },
	{ glob: "scripts/e2e/parallel_runner.mjs", why: "执行器本体（分组/超时/判分）" },
	// 选择器本体改动 ⇒ 全量：它决定了「跑哪些」，改它的时候不该由它自己来决定验多少。
	{ glob: "scripts/e2e/affected.mjs", why: "选择器本体（谁被选中由它算）" },
	{ glob: "scripts/e2e/affected-map.mjs", why: "映射表本体（选择器的真源）" },
	{ glob: "scripts/e2e/harness_selftest.mjs", why: "harness 自测；它坏 = 所有套件的绿都可能是假绿" },
	{ glob: "scripts/check-tmp-leaks.mjs", why: "e2e 外层临时目录守卫" },
	{ glob: "scripts/dist-stamp.mjs", why: "e2e 前置的产物新鲜度门禁" },
	{ glob: "api/**", why: "五份 OpenAPI 真源：HTTP 形状的正本" },
	{ glob: "internal/store/schema.sql", why: "库结构（所有套件的持久化底座）" },
	{ glob: "cmd/devseed/**", why: "每个套件都用 devseed 播种身份/Key" },
	{ glob: "go.mod", why: "依赖图变了，无法局部判定" },
	{ glob: "go.sum", why: "依赖图变了，无法局部判定" },
];

/** 逐套件映射面。`why` 写的是「为什么是这些面」，不是复述页面名。 */
export const SUITE_SURFACES = {
	// ---- HTTP 类（13）----
	api_contract: {
		why: "v4 资源面 API 契约矩阵（状态码/信封/CAS/camelCase/空列表）+ 静态产物缓存头",
		// index 页：本套件断言 `/index.html` 的 Cache-Control（静态产物形状的一部分）。
		pages: ["index"],
		paths: [
			"internal/api/**",
			"internal/protocol/**",
			"internal/version/**",
			"internal/yamldoc/**",
			"internal/logging/**",
		],
	},
	contract: {
		why: "v4 冻结契约的运行时断言 + 直读 sqlite",
		paths: ["internal/schema/**", "internal/sourcespec/**", "internal/protocol/**", "internal/yamldoc/**"],
	},
	ws_protocol: {
		why: "节点协议帧级（真后端 + 真 WS 会话，节点由 lib/nodes 模拟）",
		paths: ["internal/protocol/**", "internal/ws/**"],
	},
	routing: {
		why: "派发四步 + 未触发原因（targets → 校参 → 候选节点 → 派发）",
		paths: ["internal/route/**", "internal/trigger/**", "internal/source/**", "internal/sourcespec/**"],
	},
	persistence: {
		why: "重启后 v4 资源文档与执行事实存活（起两次 server 读同一个库）",
		paths: ["internal/store/**"],
	},
	security: {
		why: "三类主体互不串用 + 跨 ns 隔离 + 限流背压",
		paths: ["internal/auth/**", "internal/authn/**", "internal/push/**"],
	},
	edge_cases: {
		why: "边界/并发/编码/尺寸/CAS/分页",
		paths: ["internal/api/**"],
	},
	passkeys: {
		why: "Passkey 管理端点（GET/POST/DELETE /v1/auth/passkeys）",
		paths: ["internal/auth/**"],
	},
	capacity: {
		why: "保留期清理 + 派发背压（起四次 server）",
		paths: ["internal/lease/**", "internal/resultspool/**"],
	},
	orphans: {
		why: "孤儿 pending 任务诊断与 abandon",
		paths: ["internal/lease/**"],
	},
	deploy: {
		why: "部署模型：能力产物 digest / 人工部署动作 / deployments 读模型",
		paths: ["internal/artifact/**", "internal/sourcespec/**", "internal/yamldoc/**"],
	},
	live_agent: {
		why: "真 anode 的 CLI 面 + 安装/启动/退出生命周期",
		paths: ["cmd/anode/**", "scripts/install-anode.sh", "web/install-anode.sh"],
	},
	node_v4: {
		why: "真 anode 核心链：hello → 部署 → 真执行 → result → 日志/产物/取消",
		paths: ["cmd/anode/**", "internal/protocol/**", "internal/ws/**", "internal/artifact/**"],
	},

	// ---- Chrome 类（15）----
	// pages 的来源：f3-cases.mjs 里每个注册 case 各自 goto 的页面。
	// pages: ALL 的来源：矩阵/批次遍历 PAGES（f3-i18n-cases / f3-ui-helper）。
	frontend: {
		why: "注册 case 逐个起真 server + 真 Chrome（real authority）；web/yamldoc.js 经预览 mock 参与每个页面的文档渲染（它一坏就是页面级 pageerror）",
		pages: ["admin", "capabilities", "capability", "sources", "source-detail", "events", "trace", "message", "index", "playbooks", "playbook", "nodes", "node", "namespace", "namespaces", "security", "docs", "cli-auth", "login"],
		paths: ["internal/sitegen/**", "cmd/sitegen/**", "web-src/layouts/**", "web-src/locales/**", "web/yamldoc.js"],
	},
	vars_ui: {
		why: "#476 节点页/命名空间页的可视化 vars：表格数据 = effectiveVars + requires.values，保存只走 PUT …/yaml（真 server + 真 Chrome，零 /config 写）；文档文本的序列化器是 web/yamldoc.js（#473），改它就会改变这两个页面的写面",
		pages: ["node", "namespace"],
		paths: ["web/vars-table.js", "web/yamldoc.js", "internal/server/resource_namespace_yaml.go"],
	},
	i18n: {
		why: "全部页面 × 4 语言的本地化 AC（批次遍历 PAGES）",
		pages: "ALL",
		paths: ["web-src/locales/**"],
	},
	i18n_coverage: {
		why: "全部页面渲染后的中文残留门禁（批次遍历 PAGES）",
		pages: "ALL",
		paths: ["web-src/locales/**"],
	},
	ui_shift: {
		why: "开页抖动几何矩阵：全部页面 × 4 语言 × 2 主题 × 2 视口",
		pages: "ALL",
		paths: ["web-src/layouts/**"],
	},
	ux_regression_19_23: {
		why: "#19–#23 控制台 UX 回归（member 无权限/通知拒绝/身份/空 passkey/零设备）",
		// 除 #19–#23 自己的页面外，这里还保守地收进了 f3-ui-cases 模块图里出现的页面
		// （capability/message/node/nodes/trace）：ux 用例与矩阵用例共用同一个 fixture 模块，
		// 少登记一次就是一次漏跑，多登记一次只是多跑。
		pages: ["index", "admin", "receivers", "security", "login", "capability", "message", "node", "nodes", "trace"],
	},
	cli_auth: {
		why: "CLI 设备授权全链路（cli-auth 页 + login 页）",
		pages: ["cli-auth", "login"],
		paths: ["web/agent-login.sh"],
	},
	passkey_enroll: {
		why: "Passkey 新设备授权全链路（security 页 + passkey-enroll 页）",
		pages: ["security", "passkey-enroll"],
	},
	admin_flow: {
		why: "首用户自动 admin + 管理端点 + 权限 + 前端渲染",
		pages: ["admin", "login", "index"],
	},
	deeplink: {
		why: "通知深链 → message 页任务详情",
		pages: ["message", "index", "trace", "login"],
	},
	auth_flow: {
		why: "Passkey 认证全流程（CDP 虚拟认证器）",
		pages: ["capabilities", "index", "login", "nodes", "sources"],
	},
	recovery_next: {
		why: "恢复码登录跳转（?next= 白名单）+ #453 重新生成失败时不造假码",
		pages: ["keys", "login", "security"],
	},
	events_paging: {
		why: "events 游标分页 + 资源分层",
		pages: ["events", "index", "sources", "trace"],
	},
	lang_hint: {
		why: "语言提示条（纯客户端 navigator.language）",
		pages: ["events", "index", "keys", "login"],
	},
	p2_guards: {
		why: "字体自托管 / 抽屉 a11y / reduce 语义 / View Transitions / #489 ⋯ 菜单 reduce 下不自激重排",
		// 源页只剩下 sources（行内 ⋯）；旧的 source 重复页已删除（#479 黑盒审计 B-4），
		// 所以第 5 段现在只有一处 placeMenu 拷贝可验。
		pages: ["docs", "events", "login", "sources"],
		paths: ["web/fonts/**"],
	},
	password_login: {
		why: "密码登录（第二套登录模型）全流程",
		pages: ["index", "login", "security"],
		paths: ["internal/auth/**"],
	},
};

/** 与页面无关、但共享给所有 Chrome 套件的运行期资源（改了就全跑 Chrome 类）。 */
export const SHARED_CHROME_SURFACES = [
	{ glob: "web/*.css", why: "设计系统/卡片/主题样式，所有页面共用" },
	{ glob: "web/*.js", why: "partials/flograph/graphs/mock 等页面共用脚本" },
	{ glob: "web/assets/**", why: "静态资源（图标/manifest）" },
	{ glob: "web/fonts/**", why: "自托管字体（p2_guards 明确断言）" },
	{ glob: "web/vendor/**", why: "第三方运行期资源" },
	// web/*.html 是 sitegen 生成物（不入库），所以标 generated；页面级精确匹配由 pageOf() 抢先处理。
	{ glob: "web/**/*.html", why: "**手写**页面（sitegen 产物也在这里）；按页面名再细分时会被 pages 路径抢先命中", generated: true },
	{ glob: "web-src/**", why: "站点源在 pages/layouts/locales 之外的部分（partials/helpers）" },
];
// 注：`web/*.html` 由 `pageOf()` 先行处理（按页面名精确选套件），走到 SHARED_CHROME_SURFACES
// 的那条只会兜住「页面名不认识」的情况 —— 那时上层已经是 fail-closed 全量。
