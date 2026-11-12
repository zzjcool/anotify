/* Anotify · 共享 UI 片段（侧栏 / 顶栏 / 工具函数）
 *
 * 纯静态、无构建。每个页面：
 *   <link rel="stylesheet" href="tokens.css" />
 *   <link rel="stylesheet" href="ui.css" />
 *   <script src="partials.js"></script>
 *   <script>Anotify.mountLayout({ active: "overview", title: "总览", subtitle: "你的通知一览" });</script>
 *
 * 颜色一律来自 tokens.css 变量，不硬编码色值。
 */
(() => {
	/* ---------- 安全 DOM 构建工具（避免 innerHTML，防 XSS） ---------- */
	function el(tag, attrs, ...children) {
		const node = document.createElement(tag);
		if (attrs) {
			for (const [k, v] of Object.entries(attrs)) {
				if (v == null) continue;
				if (k === "class") node.className = v;
				else if (k === "text") node.textContent = v;
				else if (k.startsWith("on") && typeof v === "function")
					node.addEventListener(k.slice(2).toLowerCase(), v);
				else node.setAttribute(k, v);
			}
		}
		for (const c of children) {
			if (c == null) continue;
			node.append(c.nodeType ? c : document.createTextNode(String(c)));
		}
		return node;
	}

	/* ---------- i18n 运行时 ---------- */
	// t 查 window.AnotifyI18n[key]，缺失时返回 fallback（默认 key 本身）。
	// 处理 AnotifyI18n 未加载（如 file:// 直开）：fallback 生效、不报错。
	function t(key, fallback) {
		const dict = window.AnotifyI18n;
		if (dict && Object.hasOwn(dict, key)) {
			return dict[key];
		}
		return fallback !== undefined ? fallback : key;
	}

	// 剪枝原因 → 人话。原因集合以 internal/route/prune.go 为准：
	// 契约 §6 删掉了 path，新增 max_hops / budget。
	// 缺 key 时回落原机器串（宁可露出英文，也不要显示空白）。
	function pruneReason(reason) {
		const r = String(reason || "");
		return t("prune." + r, r);
	}

	/* ---------- 减少动效（JS 侧） ----------
	 * CSS 有 tokens.css 的 --dur-* 归零 + 全局 * 闸门，但 JS 自己计时的动画
	 * （主题翻转遮罩、toast 退场）必须显式判断并直接落终值。 */
	const REDUCE_MOTION =
		typeof window.matchMedia === "function"
			? window.matchMedia("(prefers-reduced-motion: reduce)")
			: { matches: false };

	/* ---------- SVG 图标 ---------- */
	const SVG_NS = "http://www.w3.org/2000/svg";
	function icon(paths, cls) {
		const svg = document.createElementNS(SVG_NS, "svg");
		svg.setAttribute("class", cls || "h-4 w-4");
		svg.setAttribute("viewBox", "0 0 24 24");
		svg.setAttribute("fill", "none");
		svg.setAttribute("stroke", "currentColor");
		svg.setAttribute("stroke-width", "2");
		svg.setAttribute("stroke-linecap", "round");
		svg.setAttribute("stroke-linejoin", "round");
		for (const d of [].concat(paths)) {
			const p = document.createElementNS(SVG_NS, "path");
			p.setAttribute("d", d);
			svg.appendChild(p);
		}
		return svg;
	}

	/* 侧栏图标：一套 24×24 / stroke 1.75 / round 的线条图标（D4）。
	 * SH-3 之后每个图标的「形状 = 它代表的页面」都能一眼读出来：
	 *   总览=四方块、事件流=列表、源=闪电（事件从这进来）、
	 *   能力=拼图块（可组合的执行单元）、预案=流程图（两块 + 连线）、
	 *   节点=机器匣（一台机器）、API Keys=钥匙、API 文档=代码括号、安全与登录=锁。
	 * 原先「能力」用六边形+对勾（读起来像认证徽章）、「预案」是一团认不出的笔画、
	 * 「节点」是两台叠放的圆角方块（18px 下像信封）—— 三者都已换掉。 */
	const ICONS = {
		overview: [
			"M3 3h7v9H3z",
			"M14 3h7v5h-7z",
			"M14 12h7v9h-7z",
			"M3 16h7v5H3z",
		],
		/* 节点 = 跑 agent 的机器：一个「机器匣」剖面（上盖斜切 + 一条分隔线 +
		 * 两颗指示灯）。和「能力/预案」放在一起时轮廓完全不同，不再像信封。 */
		nodes: [
			"M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z",
			"M2 12h20",
			"M6 16h.01",
			"M10 16h.01",
		],
		/* 事件流 = 带项目符号的列表 */
		events: [
			"M8 6h13",
			"M8 12h13",
			"M8 18h13",
			"M3 6h.01",
			"M3 12h.01",
			"M3 18h.01",
		],
		/* 能力 = 拼图块：它是可组合的执行单元（预案就是把几块拼起来），
		 * 不是「认证通过」——旧图标（六边形+对勾）在任何产品里都表示已认证。 */
		capabilities: [
			"M15.39 4.39a1 1 0 0 0 1.68-.474 2.5 2.5 0 1 1 3.014 3.015 1 1 0 0 0-.474 1.68l1.683 1.682a2.414 2.414 0 0 1 0 3.414L19.61 15.39a1 1 0 0 1-1.68-.474 2.5 2.5 0 1 0-3.014 3.015 1 1 0 0 1 .474 1.68l-1.683 1.682a2.414 2.414 0 0 1-3.414 0L8.61 19.61a1 1 0 0 0-1.68.474 2.5 2.5 0 1 1-3.014-3.015 1 1 0 0 0 .474-1.68l-1.683-1.682a2.414 2.414 0 0 1 0-3.414L4.39 8.61a1 1 0 0 1 1.68.474 2.5 2.5 0 1 0 3.014-3.015 1 1 0 0 1-.474-1.68l1.683-1.682a2.414 2.414 0 0 1 3.414 0z",
		],
		/* 预案 = 流程图：左上「源/触发器」方块 → 连线 → 右下「能力」方块。
		 * 旧图标是三段碎笔画，18px 下读不出任何形状。 */
		playbooks: [
			"M5 3h4a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z",
			"M7 11v2a3 3 0 0 0 3 3h3",
			"M15 13h4a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2h-4a2 2 0 0 1-2-2v-4a2 2 0 0 1 2-2z",
		],
		keys: [
			"M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4",
		],
		api: ["M16 18l6-6-6-6", "M8 6l-6 6 6 6"],
		security: ["M3 11h18v11H3z", "M7 11V7a5 5 0 0 1 10 0v4"],
		home: ["M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
		sources: ["M13 2L3 14h8l-1 8 10-12h-8z"],
		logout: [
			"M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4",
			"M16 17l5-5-5-5",
			"M21 12H9",
		],
		/* 侧栏折叠按钮：展开态显示「收起」（双左箭头），收起态显示「展开」 */
		collapse: ["M11 17l-5-5 5-5", "M18 17l-5-5 5-5"],
		expand: ["M13 17l5-5-5-5", "M6 17l5-5-5-5"],
		/* 主题三态：跟随系统（monitor）/ 深色（moon）/ 浅色（sun） */
		themeAuto: [
			"M4 3h16a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z",
			"M8 21h8",
			"M12 17v4",
		],
		themeDark: ["M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"],
		themeLight: [
			"M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10z",
			"M12 1v2",
			"M12 21v2",
			"M4.22 4.22l1.42 1.42",
			"M18.36 18.36l1.42 1.42",
			"M1 12h2",
			"M21 12h2",
			"M4.22 19.78l1.42-1.42",
			"M18.36 5.64l1.42-1.42",
		],
	};

	/* ---------- 侧栏导航结构（IA：工作台 / 资源） ----------
	 * labelKey / groupLabelKey 指向 i18n key；渲染时用 t() 取值。
	 * 无 i18n.js 时 fallback 到中文原文，保持向后兼容。
	 *
	 * v2 分组只有两个：工作台 = 日常在看的（总览/事件流/源/能力/出口/预案/节点），
	 * 资源 = 配置与凭据（API Keys / API 文档 / 安全与登录）。退出登录不再是导航项，
	 * 收进底部的用户块（.side-user 里的图标按钮）——它不是页面，是账号动作。 */
	const NAV = [
		{
			groupLabelKey: "ui.nav.group_workspace",
			groupFallback: "工作台",
			items: [
				{
					id: "overview",
					labelKey: "common.nav.overview",
					fallback: "总览",
					href: "index.html",
					icon: "overview",
				},
				{
					id: "events",
					labelKey: "common.nav.events",
					fallback: "事件流",
					href: "events.html",
					icon: "events",
				},
				{
					id: "sources",
					labelKey: "common.nav.sources",
					fallback: "源",
					href: "sources.html",
					icon: "sources",
				},
				{
					id: "capabilities",
					labelKey: "common.nav.capabilities",
					fallback: "能力",
					href: "capabilities.html",
					icon: "capabilities",
				},
				{
					// 预案 = 把「能力 + 下游 + 触发器」打在一起的零件，
					// 所以紧挨能力之后、节点之前（节点是它的实现方）
					//
					// 注：出口页已经删掉了（CONTRACT-downstream-edit §5）——能力的下游
					// 只能在能力编辑里改，没有第二个页面。
					id: "playbooks",
					labelKey: "common.nav.playbooks",
					fallback: "预案",
					href: "playbooks.html",
					icon: "playbooks",
				},
				{
					id: "nodes",
					labelKey: "common.nav.nodes",
					fallback: "节点",
					href: "nodes.html",
					icon: "nodes",
				},
			],
		},
		{
			groupLabelKey: "ui.nav.group_resources",
			groupFallback: "资源",
			items: [
				{
					id: "keys",
					labelKey: "common.nav.keys",
					fallback: "API Keys",
					href: "keys.html",
					icon: "keys",
				},
				{
					// 集成原本是「接入 Agent / API 文档 / 技术方案」三个 tab 的一个分组。
					// 现在只留 API 文档（docs.html），并且并进主导航——单独一个 item 的
					// 分组标签只是噪音（还要多占一行 12px 小标题）。
					id: "api",
					labelKey: "common.nav.api",
					fallback: "API 文档",
					href: "docs.html",
					icon: "api",
				},
				{
					id: "security",
					labelKey: "common.nav.security",
					fallback: "安全与登录",
					href: "security.html",
					icon: "security",
				},
			],
		},
	];

	/* 品牌标（SH-1/SH-2）。三个元素原先互相打架：黑底方块 + 暗灰手写 “An”
	 * + 一颗无解释的红点 + 旁边再写一遍 cursive “Anotify”。改成：
	 *   · 方块：品牌色渐变（tokens.css 的 --brand-tile-*，两套主题都成立，
	 *     不再是纯黑墨块），白字实测 dark 6.8:1 / light 5.4:1；
	 *   · 删掉那颗红点：它没有语义（不表示在线/未读，顶栏铃铛才是通知位），
	 *     却在一屏里最抢眼。
	 *   · 字标不再重画一遍：方块就是品牌，旁边那句 cursive “Anotify” 保留
	 *     （与登录页一致），但只在展开态出现；折叠态只剩方块。 */
	function logo(size) {
		const s = size || "h-9 w-9";
		return el(
			"div",
			{ class: `brand-tile ${s} rounded-lg` },
			el("span", {
				class: "brand-mark font-script leading-none",
				/* 品牌字标保持 "An"（与旧版/登录页一致）；wave 2 曾为折叠态
				 * 紧凑改成单字 "A"，用户反馈与旧版不一致，恢复。 */
				text: "An",
			}),
		);
	}

	/* ---------- 主题（light / dark / auto） ----------
	 *
	 * 三层职责分得很开，改的时候别混：
	 *   · tokens.css    —— 两套色值，`<html data-theme>` 一换就全站生效；
	 *   · layout <head> —— 同步内联脚本，在首帧前就把 data-theme 落到 <html>
	 *                      （防 FOUC，不能 defer）；
	 *   · 本文件        —— 运行期三态切换（auto/dark/light）、持久化、跟随系统。
	 *
	 * `auto` 不是第三个主题，是「不指定」：实际渲染色由 prefers-color-scheme 决定。
	 * 所以 dataset.theme 永远只有 dark/light 两个值，"auto" 只活在 localStorage 与
	 * 按钮图标里——CSS 不需要知道 auto 的存在。
	 */
	const THEME_KEY = "anotify-theme"; // "auto" | "dark" | "light"
	const THEME_MODES = ["auto", "dark", "light"];
	const THEME_ICON_BY_MODE = {
		auto: "themeAuto",
		dark: "themeDark",
		light: "themeLight",
	};
	// {mode} 占位符里用的是「模式本身的名字」，不是解析后的主题名：
	// 选了 auto 却显示「当前主题：深色」会让人以为手动选了深色。
	const THEME_MODE_FALLBACK = { auto: "跟随系统", dark: "深色", light: "浅色" };

	function readThemeMode() {
		try {
			const v = localStorage.getItem(THEME_KEY);
			return THEME_MODES.includes(v) ? v : "auto";
		} catch (e) {
			return "auto"; /* 隐私模式下 localStorage 读不到 */
		}
	}

	function resolveTheme(mode) {
		if (mode === "dark" || mode === "light") return mode;
		try {
			return window.matchMedia("(prefers-color-scheme: dark)").matches
				? "dark"
				: "light";
		} catch (e) {
			return "dark";
		}
	}

	function themeToggleLabel(mode) {
		const name = t("ui.theme." + mode, THEME_MODE_FALLBACK[mode] || mode);
		return t("ui.theme.current", "当前主题：{mode}").replace("{mode}", name);
	}

	/* 图标+文案反映的是「模式」（auto 显示显示器图标），不是解析后的主题。
	   这样点一下就能预测下一次点击会到哪个模式。 */
	function paintThemeToggle(mode) {
		const btn = document.getElementById("theme-toggle");
		if (!btn) return; /* 非外壳页（login/focus）没有这个按钮 */
		while (btn.firstChild) btn.removeChild(btn.firstChild);
		btn.append(icon(ICONS[THEME_ICON_BY_MODE[mode] || "themeAuto"], "h-5 w-5"));
		const label = themeToggleLabel(mode);
		btn.setAttribute("aria-label", label);
		btn.setAttribute("title", label);
	}

	/* 主题翻转遮罩：切换前先把旧 --bg 铺满视口，同步写完 data-theme 后
	 * 200ms 淡出。只做视觉过渡，data-theme 不延迟（e2e 立即断言）。
	 * reduce 下返回 null（直接换主题，不插遮罩）。 */
	function createThemeVeil() {
		if (REDUCE_MOTION.matches || !document.body) return null;
		const prevBg = getComputedStyle(document.documentElement)
			.getPropertyValue("--bg")
			.trim();
		if (!prevBg) return null;
		document
			.querySelectorAll(".theme-flip-overlay")
			.forEach((n) => n.remove());
		const veil = document.createElement("div");
		veil.className = "theme-flip-overlay";
		veil.setAttribute("aria-hidden", "true");
		veil.style.background = prevBg;
		document.body.appendChild(veil);
		return veil;
	}

	/* 主题切换降并发（D4/D5）：换色那 220ms 把全站过渡关掉，避免一次换主题
	   同时起 100+ 条颜色过渡（实测 99–181 条 / 150ms 长帧）。只锁这个窗口，
	   遮罩自身的淡出在 CSS 里被排除。reduce 下完全跳过。 */
	const THEME_LOCK_MS = 220;
	let themeLockTimer = 0;
	function lockThemeTransitions() {
		if (REDUCE_MOTION.matches) return;
		const root = document.documentElement;
		root.classList.add("theme-switching");
		if (themeLockTimer) clearTimeout(themeLockTimer);
		themeLockTimer = setTimeout(() => {
			themeLockTimer = 0;
			root.classList.remove("theme-switching");
		}, THEME_LOCK_MS);
	}

	function fadeThemeVeil(veil) {
		if (!veil) return;
		const dur =
			parseFloat(
				getComputedStyle(document.documentElement)
					.getPropertyValue("--dur")
					.trim(),
			) || 200;
		requestAnimationFrame(() => {
			veil.classList.add("is-fading");
			setTimeout(() => veil.remove(), dur + 60);
		});
	}

	function applyTheme(mode, persist) {
		const m = THEME_MODES.includes(mode) ? mode : "auto";
		const theme = resolveTheme(m);
		const root = document.documentElement;
		/* 只有用户主动切换（persist）且主题真的变了才铺遮罩；
		 * 首帧 / 跟随系统（persist=false）不铺。 */
		const changed = persist && theme !== root.dataset.theme;
		const veil = changed ? createThemeVeil() : null;
		if (changed) lockThemeTransitions();
		root.dataset.theme = theme;
		root.style.colorScheme = theme;
		const meta = document.querySelector('meta[name="theme-color"]');
		if (meta)
			meta.setAttribute("content", theme === "dark" ? "#08080e" : "#f7f8fc");
		if (persist) {
			try {
				localStorage.setItem(THEME_KEY, m);
			} catch (e) {
				/* 写不进去就退化成“本次会话有效”，不影响渲染 */
			}
		}
		paintThemeToggle(m);
		document.dispatchEvent(
			new CustomEvent("anotify:theme", { detail: { theme, mode: m } }),
		);
		fadeThemeVeil(veil);
		return theme;
	}

	/* auto → dark → light → auto */
	function cycleTheme() {
		const next = { auto: "dark", dark: "light", light: "auto" };
		applyTheme(next[readThemeMode()] || "dark", true);
	}

	/* 系统主题变化：只在「跟随系统」时重算。手动选定的主题不该被系统设置抢走。 */
	try {
		const mq = window.matchMedia("(prefers-color-scheme: dark)");
		const onSchemeChange = () => {
			if (readThemeMode() === "auto") applyTheme("auto", false);
		};
		if (typeof mq.addEventListener === "function")
			mq.addEventListener("change", onSchemeChange);
		else if (typeof mq.addListener === "function") mq.addListener(onSchemeChange);
	} catch (e) {
		/* 老环境不支持 matchMedia：停在 layout 内联脚本算出的主题 */
	}

	/* 与 layout <head> 内联脚本对齐（幂等：同 mode → 同结果，不会闪） */
	applyTheme(readThemeMode(), false);

	/* ---------- 侧栏：折叠（桌面）/ 抽屉（移动） ---------- */
	const SIDEBAR_KEY = "anotify-sidebar"; // "collapsed" | "expanded"

	function readSidebarCollapsed() {
		try {
			return localStorage.getItem(SIDEBAR_KEY) === "collapsed";
		} catch (e) {
			return false; /* 默认展开 */
		}
	}

	/* 移动抽屉的对话语义：只在「抽屉态」挂 dialog 角色（D1/D3）。
	   桌面常驻侧栏是普通导航，不能标 aria-modal。 */
	function setDrawerDialog(sb, on) {
		if (!sb) return;
		if (on) {
			sb.setAttribute("role", "dialog");
			sb.setAttribute("aria-modal", "true");
			sb.setAttribute("aria-label", t("common.nav.menu", "菜单"));
		} else {
			sb.removeAttribute("role");
			sb.removeAttribute("aria-modal");
			sb.removeAttribute("aria-label");
		}
	}

	/* 移动端抽屉：开。三个状态必须成对维护——
	   state class（-translate-x-full / hidden）、body 滚动锁、aria-expanded。 */
	function openSidebar() {
		const sb = document.getElementById("sidebar");
		const ov = document.getElementById("sidebar-overlay");
		if (sb) sb.classList.remove("-translate-x-full");
		setDrawerDialog(sb, true);
		if (ov) ov.classList.remove("hidden");
		document.body.style.overflow = "hidden";
		const mb = document.getElementById("menu-btn");
		if (mb) mb.setAttribute("aria-expanded", "true");
		/* 抽屉打开后把焦点送进面板（关闭按钮），否则 Tab 仍从页面顶栏开始，
		 * 读屏用户不知道抽屉已经出现。等滑入动画一帧后再聚焦，避免浏览器
		 * 把 focus 滚动到不可见位置。 */
		requestAnimationFrame(() => {
			const closeBtn = document.getElementById("sidebar-close");
			if (closeBtn) closeBtn.focus();
		});
	}

	/* 移动端抽屉：关。`keepFocus === false` 时不抢焦点（点导航链接要跟住跳转）。 */
	function closeSidebar(keepFocus) {
		const sb = document.getElementById("sidebar");
		const ov = document.getElementById("sidebar-overlay");
		if (sb) sb.classList.add("-translate-x-full");
		setDrawerDialog(sb, false);
		if (ov) ov.classList.add("hidden");
		document.body.style.overflow = "";
		const mb = document.getElementById("menu-btn");
		if (mb) {
			mb.setAttribute("aria-expanded", "false");
			if (keepFocus !== false) mb.focus();
		}
	}

	/* ---------- 语言习惯的本机记忆（localStorage） ----------
	 *
	 * 语言提示条（.lang-hint）全站按「浏览器首选语言 ≠ 当前页语言」出现，所以需要
	 * 一份“用户自己表态过”的记忆，否则会一直唠叨：
	 *
	 *   chosen    用户主动选过的语言（侧栏/登录页语言切换器，或点过横幅的「切换」）。
	 *             有它就不再拿浏览器语言去劝，反而以它为准；它等于当前页语言时完全不弹。
	 *   dismissed 用户 X 掉的目标语言列表（按语言码记，不是全局静音）：
	 *             关掉英文提示后，若浏览器首选语言换成日文，新的日文提示仍然有效。
	 *
	 * 存储形状：{ "chosen": "en", "dismissed": ["en"] }，键名 anotify-lang-hint。
	 * localStorage 不可用（隐私模式/配额）时全部退化成“每次重新判断”，不影响渲染。 */
	const LANG_HINT_KEY = "anotify-lang-hint";
	function readLangHintState() {
		const empty = { chosen: "", dismissed: [] };
		try {
			const raw = JSON.parse(localStorage.getItem(LANG_HINT_KEY) || "null");
			/* 早期版本只存了一个目标语言数组，兼容一下 */
			if (Array.isArray(raw))
				return { chosen: "", dismissed: raw.filter((c) => typeof c === "string") };
			if (!raw || typeof raw !== "object") return empty;
			return {
				chosen: typeof raw.chosen === "string" ? raw.chosen : "",
				dismissed: Array.isArray(raw.dismissed)
					? raw.dismissed.filter((c) => typeof c === "string")
					: [],
			};
		} catch (_) {
			return empty;
		}
	}
	function writeLangHintState(state) {
		try {
			localStorage.setItem(LANG_HINT_KEY, JSON.stringify(state));
		} catch (_) {
			/* 写不进去：本次照常，只是下次还会提示 */
		}
	}
	function rememberDismissedLang(code) {
		const state = readLangHintState();
		if (!state.dismissed.some((c) => c.toLowerCase() === code.toLowerCase()))
			state.dismissed.push(code);
		writeLangHintState(state);
	}
	function rememberChosenLang(code) {
		const state = readLangHintState();
		state.chosen = code;
		writeLangHintState(state);
	}

	/* ---------- Language switcher dropdown (shared builder) ----------
	 * Creates a trigger button + popup menu from language data.
	 * Used by both the sidebar switcher (JS-built) and the login page
	 * switcher (template-rendered flat links enhanced by JS).
	 *
	 * @param {Object}   cfg
	 * @param {Array}    cfg.items       - [{code, label, href, isCurrent}]
	 * @param {string}   cfg.direction   - "up" (sidebar) or "down" (login)
	 * @param {string}   cfg.width        - "full" (sidebar) or "auto" (login)
	 * @param {string}   cfg.align        - "left" or "right" (menu horizontal alignment)
	 * @returns {{host: HTMLElement, open: Function, close: Function}}
	 */
	function createLangDropdown(cfg) {
		const { items, direction, width, align } = cfg;

		/* Trigger button: globe icon + current language name + chevron */
		const triggerClass =
			"lang-trigger flex " +
			(width === "full" ? "w-full " : "") +
			"items-center gap-2.5 rounded-lg px-3 py-2 text-sm";
		const trigger = el(
			"button",
			{
				class: triggerClass,
				"aria-haspopup": "true",
				"aria-expanded": "false",
				"aria-label": t(
					"common.lang.switcher_label",
					"\u5207\u6362\u8bed\u8a00",
				),
			},
			icon(
				[
					"M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z",
					"M2 12h20",
					"M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z",
				],
				"h-4 w-4",
			),
			el("span", {
				class: "text-sm",
				style: "color: var(--muted)",
				text: items.find((i) => i.isCurrent)?.label || items[0]?.label || "",
			}),
			icon(
				["M6 9l6 6 6-6"],
				"h-3.5 w-3.5 " +
					(width === "full" ? "ml-auto" : "ml-1") +
					" lang-chevron",
			),
		);

		/* Menu positioning: sidebar = up (bottom-full), login = down (top-full) */
		const menuPosClass =
			direction === "up"
				? "absolute bottom-full left-0 right-0 mb-2"
				: "absolute top-full " +
					(align === "right" ? "right-0" : "left-0") +
					" mt-2";
		const menu = el("div", {
			class: "lang-menu hidden z-50 p-1.5 " + menuPosClass + " min-w-[160px]",
			role: "menu",
			"aria-orientation": "vertical",
			style: "max-height: 60vh; overflow-y: auto",
		});

		const menuItems = [];
		for (const item of items) {
			const checkSlot = el("span", {
				class: "w-3.5 flex-shrink-0 text-center",
				style: "color: var(--accent); font-size: 0.75rem",
			});
			if (item.isCurrent) checkSlot.textContent = "\u2713";
			const a = el(
				"a",
				{
					href: item.href,
					hreflang: item.code,
					lang: item.code,
					class:
						"lang-menu-item flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm",
					role: "menuitem",
					style: item.isCurrent
						? "color: var(--text); background: var(--accent-soft)"
						: "color: var(--muted)",
				},
				checkSlot,
				el("span", { text: item.label }),
			);
			if (item.isCurrent) a.setAttribute("aria-current", "true");
			/* 用户自己选了语言 = 语言习惯（见 LANG_HINT_KEY）：以后一切以这次选择为准，
			 * 不再按浏览器语言劝他换 —— 否则“手动切到英文站”之后，英文页面上
			 * 反而会弹“本网站提供简体中文版本”。只在点非当前语言时记。 */
			if (!item.isCurrent)
				a.addEventListener("click", () => rememberChosenLang(item.code));
			menuItems.push(a);
			menu.append(a);
		}

		const host = el("div", { class: "relative" }, trigger, menu);

		/* --- Toggle / keyboard / outside-click --- */
		function openMenu() {
			menu.classList.remove("hidden");
			trigger.setAttribute("aria-expanded", "true");
			const current = menuItems.find(
				(m) => m.getAttribute("aria-current") === "true",
			);
			if (current) current.focus();
		}
		function closeMenu() {
			menu.classList.add("hidden");
			trigger.setAttribute("aria-expanded", "false");
		}
		function toggleMenu() {
			if (menu.classList.contains("hidden")) openMenu();
			else closeMenu();
		}

		trigger.addEventListener("click", toggleMenu);

		/* keyboard nav inside menu */
		menu.addEventListener("keydown", (e) => {
			const idx = menuItems.indexOf(document.activeElement);
			if (e.key === "ArrowDown") {
				e.preventDefault();
				menuItems[(idx + 1) % menuItems.length].focus();
			} else if (e.key === "ArrowUp") {
				e.preventDefault();
				menuItems[(idx - 1 + menuItems.length) % menuItems.length].focus();
			} else if (e.key === "Home") {
				e.preventDefault();
				menuItems[0].focus();
			} else if (e.key === "End") {
				e.preventDefault();
				menuItems[menuItems.length - 1].focus();
			} else if (e.key === "Escape") {
				e.preventDefault();
				closeMenu();
				trigger.focus();
			} else if (e.key === "Tab") {
				closeMenu();
			}
		});

		/* trigger keyboard: Enter/Space/ArrowDown to open */
		trigger.addEventListener("keydown", (e) => {
			if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
				e.preventDefault();
				openMenu();
			}
		});

		/* outside click closes */
		document.addEventListener("click", (e) => {
			if (!host.contains(e.target)) closeMenu();
		});

		return { host, open: openMenu, close: closeMenu };
	}

	/* ---------- Language switcher (sidebar) ----------
	 * Reads the template-rendered language link list (#lang-switcher-data)
	 * and enhances it into a trigger button + dropdown menu.
	 * The template renders <a> links at build time with correct hrefs and
	 * language metadata (from sitegen .Langs — single source of truth).
	 * This function appends query/hash, builds the dropdown, and removes
	 * the data block from the DOM after consuming it.
	 * Progressive enhancement: if the data block is missing (single-lang
	 * build or template error), returns null (no switcher rendered).
	 */
	function buildLangSwitcher() {
		const dataEl = document.getElementById("lang-switcher-data");
		if (!dataEl) return null; /* not rendered (single-lang build) */

		const linkEls = dataEl.querySelectorAll("a[hreflang]");
		if (linkEls.length <= 1) return null; /* single-language, no switcher */

		const qs = location.search || "";
		const hash = location.hash || "";

		const items = [];
		for (const a of linkEls) {
			const code = a.getAttribute("hreflang") || "";
			const label = a.textContent || code;
			let href = a.getAttribute("href") || "";
			/* Append query + hash if not already present (preserve deep links) */
			if (qs && !href.includes("?")) href += qs;
			if (hash && !href.includes("#")) href += hash;
			items.push({
				code,
				label,
				href,
				isCurrent: a.getAttribute("aria-current") === "true",
			});
		}

		/* Remove the data block from DOM (consumed) */
		dataEl.remove();

		const { host } = createLangDropdown({
			items,
			direction: "up",
			width: "full",
			align: "left",
		});
		host.id = "lang-switcher";
		return host;
	}

	/* ---------- Language switcher (login page) ----------
	 * Enhances the template-rendered flat link list (#lang-switcher-login)
	 * into a trigger button + dropdown menu.
	 * The template renders <a> links at build time with correct hrefs;
	 * this function reads them, appends query/hash, and builds the dropdown.
	 * Progressive enhancement: without JS, the flat links are still usable.
	 */
	function mountLoginLangSwitcher() {
		const hostEl = document.getElementById("lang-switcher-login");
		if (!hostEl) return; /* not on login page or single-lang build */

		const linkEls = hostEl.querySelectorAll("[data-lang-list] a[hreflang]");
		if (linkEls.length <= 1) return; /* single-language, no switcher */

		const qs = location.search || "";
		const hash = location.hash || "";

		const items = [];
		for (const a of linkEls) {
			const code = a.getAttribute("hreflang") || "";
			const label =
				a.querySelector("span:not(.lang-check):not(.sr-only)")?.textContent ||
				code;
			let href = a.getAttribute("href") || "";
			/* Append query + hash if not already present (preserve deep links) */
			if (qs && !href.includes("?")) href += qs;
			if (hash && !href.includes("#")) href += hash;
			items.push({
				code,
				label,
				href,
				isCurrent: a.getAttribute("aria-current") === "true",
			});
		}

		const { host: dropdown } = createLangDropdown({
			items,
			direction: "down",
			width: "auto",
			align: "right",
		});

		/* Replace the flat list with the dropdown (clear via DOM, not innerHTML) */
		while (hostEl.firstChild) hostEl.removeChild(hostEl.firstChild);
		hostEl.append(dropdown);
	}

	/* ---------- 布局挂载：在 body 开头注入「侧栏 + 主栏容器」 ----------
	 * 页面把主内容放在 <div id="page-main"></div> 里，本函数把它包进布局。
	 */
	// 侧栏顶部的「命名空间」切换器。
	//
	// 一个触发器 + 一个分节菜单。**刻意不用原生 <select>**：原生下拉在暗色
	// 界面里就是系统控件的样子（白底、方角、系统字体、没有悬浮反馈），
	// 放在侧栏顶部，跟整套设计像两个时代的拼贴。
	//
	// 命名空间是侧栏唯一的维度（#203）。组织那一层是幽灵 UI：组织端点
	// （orgs）在真后端根本不存在，探测 404 之后整个切换器永远不渲染，用户被
	// 锁死在 `default`。现在只认 `GET /v1/namespaces`（不带组织参数）；拿不到
	// （未登录 / 老后端没有该端点）就保持隐藏，页面行为一点不变 —— 不需要为
	// mock 和真后端分叉代码路径。
	//
	// v4 起列表是 `NamespaceList` 信封：条目读 `metadata.name`（slug，也是路径
	// 里的 {namespace}）与 `metadata.displayName`（人读名）。`data-ns-id` 属性
	// 保留给 e2e 定位，值就是 namespace name。
	async function mountNamespaceSwitcher(box, slot) {
		/* 失败路径：骨架让位（不再预留 53px）。侧栏一次性上移，用户看到的就是
		 * 「没有切换器」—— 与今天「加载失败就不渲染」的行为一致（已知遗留）。 */
		const giveUp = () => {
			const skel = slot ? slot.querySelector(".scope-skel") : null;
			if (skel) skel.remove();
			if (slot) slot.classList.add("scope-slot--empty");
		};
		let list = [];
		try {
			const r = await fetch("/v1/namespaces", { credentials: "include" });
			if (!r.ok) {
				giveUp();
				return;
			}
			/* v4 严格：200 但 `items` 不是数组（裸数组 / {namespaces:[]} / 缺字段）
			 * 一律当成加载失败。以前这里会静默回退，结果骨架永久脉动、
			 * #scope-switcher 永远 hidden —— 比直接不渲染更难排查。
			 * 空 `items:[]` 仍是合法列表 → 走 .scope-empty 空态。 */
			const j = await r.json().catch(() => null);
			if (!j || !Array.isArray(j.items)) {
				giveUp();
				return;
			}
			list = j.items
				.map((it) => {
					const meta = it && it.metadata;
					if (!meta || typeof meta.name !== "string" || !meta.name) return null;
					return {
						name: meta.name,
						displayName:
							typeof meta.displayName === "string" && meta.displayName
								? meta.displayName
								: meta.name,
					};
				})
				.filter(Boolean);
		} catch (e) {
			giveUp();
			return;
		}

		// 存着的选择可能已经失效（换了账号、删了这个命名空间），回落到第一个
		const nsId = list.some((n) => n.name === scopeNamespace())
			? scopeNamespace()
			: (list[0] || {}).name || "";

		const nsOf = () => list.find((n) => n.name === nsId) || list[0] || null;

		/* ---- 触发器：# + 命名空间 + 箭头（没有第二个维度了） ---- */
		const nsText = el("span", { class: "scope-ns" });
		const trigger = el(
			"button",
			{
				type: "button",
				class: "scope-trigger",
				"aria-haspopup": "menu",
				"aria-expanded": "false",
				"aria-label": t("scope.switcher_label", "切换命名空间"),
			},
			el("span", { class: "scope-hash", text: "#" }),
			el("span", { class: "scope-text" }, nsText),
			icon(["M6 9l6 6 6-6"], "h-3.5 w-3.5 scope-chevron"),
		);

		const menu = el("div", {
			class: "lang-menu scope-menu hidden",
			role: "menu",
			"aria-orientation": "vertical",
		});

		// 一条可选项。lead 固定是 #：侧栏只有命名空间这一个维度，
		// 不再有组织头像那种第二种 lead。
		const item = (ns) => {
			const checked = ns.name === nsId;
			const b = el(
				"button",
				{
					type: "button",
					class: "scope-item",
					role: "menuitemradio",
					"aria-checked": checked ? "true" : "false",
					// e2e 靠它定位条目（比人读名稳），别删。v4 起值 = namespace name
					"data-ns-id": ns.name,
				},
				el("span", { class: "scope-hash", text: "#" }),
				el("span", { class: "scope-item-name", "data-raw-content": "", text: ns.displayName }),
				el("span", { class: "scope-item-sub", "data-raw-content": "", text: ns.name }),
				checked ? icon(["M20 6L9 17l-5-5"], "h-3.5 w-3.5 scope-check") : null,
			);
			b.addEventListener("click", () => switchScope(ns.name));
			return b;
		};

		// 新建：v4 信封 `{apiVersion,kind,metadata:{name,displayName},spec:{}}`。
		// name 就是路径里的 {namespace}（slug 语义）；组织已经不是前端概念。
		async function createNamespace() {
			const slug = String(
				window.prompt(
					t("scope.new_ns_prompt", "新命名空间 slug（小写字母 / 数字 / 连字符）"),
					"",
				) || "",
			).trim();
			if (!slug) return;
			try {
				const r = await fetch("/v1/namespaces", {
					method: "POST",
					credentials: "include",
					headers: { "content-type": "application/json" },
					body: JSON.stringify({
						apiVersion: "anotify/v1",
						kind: "Namespace",
						metadata: { name: slug, displayName: slug },
						spec: {},
					}),
				});
				const j = await r.json().catch(() => null);
				if (!r.ok) {
					// 失败原因在 Status.message（不再是 {error}）
					window.alert(
						(j && (j.message || j.error)) ||
							t("scope.new_ns_failed", "创建失败"),
					);
					return;
				}
				const created = j && j.metadata && j.metadata.name;
				if (!created) {
					// 2xx 但回包里没有 metadata.name：不是“建好了”。报错、不 reload，
					// 否则用户会看到一个空刷新，以为成功但什么也没变。
					window.alert(
						(j && (j.message || j.error)) ||
							t("scope.new_ns_failed", "创建失败"),
					);
					return;
				}
				switchScope(created);
			} catch (e) {
				window.alert(t("scope.new_ns_failed", "创建失败"));
			}
		}

		const plus = el("button", {
			type: "button",
			class: "scope-group-btn",
			title: t("scope.new_ns", "新建命名空间"),
			"aria-label": t("scope.new_ns", "新建命名空间"),
			text: "+",
		});
		plus.addEventListener("click", createNamespace);

		// 空态：一个命名空间都没有时**替代**列表，绝不渲染空下拉 ——
		// 空下拉只会让人以为界面坏了，看不出「先新建一个」这个下一步。
		const emptyCta = el("button", {
			type: "button",
			class: "scope-empty-cta",
			text: t("scope.new_ns", "新建命名空间"),
		});
		emptyCta.addEventListener("click", createNamespace);
		const empty = el(
			"div",
			{ class: "scope-empty" },
			el("p", {
				class: "scope-empty-text",
				text: t("scope.empty_hint", "还没有命名空间，先建一个再开始。"),
			}),
			emptyCta,
		);

		const paint = () => {
			const n = nsOf();
			nsText.removeAttribute("data-raw-content");
			nsText.textContent = n
				? `${n.displayName} · ${n.name}`
				: t("scope.none", "（还没有命名空间）");
			if (n) nsText.setAttribute("data-raw-content", "");

			menu.textContent = "";
			menu.append(
				el(
					"div",
					{ class: "scope-group" },
					el("span", { text: t("scope.section_ns", "命名空间") }),
					plus,
				),
			);
			if (!list.length) {
				menu.append(empty);
				return;
			}
			for (const ns of list) menu.append(item(ns));
		};

		const onDocClick = (ev) => {
			if (menu.contains(ev.target) || trigger.contains(ev.target)) return;
			close();
		};
		const close = () => {
			menu.classList.add("hidden");
			trigger.setAttribute("aria-expanded", "false");
			document.removeEventListener("click", onDocClick, true);
		};
		// focusItem：只有**键盘**打开才把焦点送进菜单。
		// 鼠标点开时自动聚焦第一条会画出一圈 focus ring，
		// 看起来像"这条被选中了"，容易误读。
		const open = (focusItem) => {
			paint();
			menu.classList.remove("hidden");
			trigger.setAttribute("aria-expanded", "true");
			document.addEventListener("click", onDocClick, true);
			if (focusItem) {
				const first = menu.querySelector(".scope-item");
				if (first) first.focus();
			}
		};

		trigger.addEventListener("click", (ev) => {
			ev.stopPropagation();
			// detail === 0 ⇔ 由键盘（Enter/Space）触发，而不是鼠标
			if (menu.classList.contains("hidden")) open(ev.detail === 0);
			else close();
		});
		// 键盘：Esc 关、上下键在条目间移动（菜单的可访问性底线）
		menu.addEventListener("keydown", (ev) => {
			if (ev.key !== "ArrowDown" && ev.key !== "ArrowUp") return;
			ev.preventDefault();
			const items = Array.from(menu.querySelectorAll(".scope-item"));
			if (!items.length) return;
			const i = items.indexOf(document.activeElement);
			const next =
				ev.key === "ArrowDown"
					? (i + 1) % items.length
					: (i - 1 + items.length) % items.length;
			items[next].focus();
		});
		document.addEventListener("keydown", (ev) => {
			if (ev.key === "Escape" && !menu.classList.contains("hidden")) close();
		});

		box.append(el("div", { class: "scope-block" }, trigger, menu));
		/* 拆骨架与揭盖必须在**同一个同步块**里：中间只要有一帧只有骨架或只剩空槽
		 * （或者反过来「block 已插入但还 hidden」），nav 就会先上移再下移 —— 那正是
		 * 用户看到的一下。骨架 53px、真 block 53px，所以这一步在几何上是零位移。 */
		const skel = slot ? slot.querySelector(".scope-skel") : null;
		if (skel) skel.remove();
		box.classList.remove("hidden");
		paint();
		// 纠正落盘的选择：存着的那条可能已不存在，写回真正生效的 namespace name
		// （空列表时写空串，语义 = "确实没有命名空间"，别留一个不存在的值）。
		saveScope({ namespace: nsId });
	}

	/* 导航 id → 所属分组标题。顶栏上下文（SH-4）用：一个页面属于“工作台 / 资源 / 管理”，
	 * 这个信息侧栏只在展开态给得到，顶栏重复它就比重复页面名有用。
	 * NAV 之外的 active（admin）单独补一条；未知 active 返回 null。 */
	const NAV_GROUP_OF = (() => {
		const map = Object.create(null);
		for (const g of NAV) {
			for (const it of g.items) {
				map[it.id] = g.groupLabelKey
					? { labelKey: g.groupLabelKey, fallback: g.groupFallback }
					: null;
			}
		}
		map.admin = { labelKey: "common.nav.admin_section", fallback: "管理" };
		return map;
	})();

	function mountLayout(opts) {
		// 侧栏、首帧与之后的读写必须看到同一个显式范围：先 adopt，再挂任何 DOM。
		adoptUrlNamespace();
		const o = Object.assign(
			{
				active: "",
				title: "",
				subtitle: "",
				username: "zheng",
				// "deployment" = 这一页确实不随命名空间切换（今天是密钥 / 安全 / 管理
				// 三页；能力与节点已随命名空间）。页面必须标出来，否则用户切了
				// 命名空间发现列表没变，只会以为功能坏了。
				scopeLevel: "",
			},
			opts || {},
		);
		const pageMain = document.getElementById("page-main");
		if (!pageMain) {
			console.error('[partials] 需要 <div id="page-main"> 包裹主内容');
			return;
		}

		/* ----- 折叠态（桌面：图标轨 ↔ 展开） -----
		 * 状态只写到 #app-shell 的 data-collapsed 上，宽度收敛全部交给 ui.css 的
		 * 纯 CSS（#app-shell[data-collapsed="true"] { --shell-sidebar-w: ... }）。
		 * 这里**不重建 DOM、也不删任何文字节点**：e2e 取 a.side-link 的最后一个
		 * 文本子节点当 label（frontend.mjs:1813），删掉文案就取不到导航名了。 */
		let collapsedNow = readSidebarCollapsed();
		/* 顶栏上下文里的命名空间节点（SH-4）：由 header 创建时赋值，
		 * 命名空间列表回来后再画一次（存本地的选择可能已失效，切换器会回落到第一个）。 */
		let scopeCrumbEl = null;

		/* 命名空间名直接读 scope；拿不到就写与切换器同一句空态文案，不编一个假 namespace。 */
		function paintScopeCrumb() {
			if (!scopeCrumbEl) return;
			const ns = scopeNamespace();
			scopeCrumbEl.textContent = ns
				? "#" + ns
				: t("scope.none", "（还没有命名空间）");
			scopeCrumbEl.title = ns
				? t("uxaudit.shell.scope_hint", "当前命名空间，可在左侧切换")
				: t("scope.missing", "还没有确定命名空间");
			scopeCrumbEl.dataset.ns = ns;
		}

		function paintCollapseBtn() {
			/* 文案说的是「点一下会发生什么」：展开态按钮的动作是「收起侧栏」。 */
			const label = collapsedNow
				? t("ui.sidebar.expand", "展开侧栏")
				: t("ui.sidebar.collapse", "收起侧栏");
			collapseBtn.setAttribute("aria-label", label);
			collapseBtn.setAttribute("title", label);
			/* aria-expanded 描述「侧栏当前是否展开」，不是「按钮按没按下」 */
			collapseBtn.setAttribute(
				"aria-expanded",
				collapsedNow ? "false" : "true",
			);
			while (collapseBtn.firstChild)
				collapseBtn.removeChild(collapseBtn.firstChild);
			collapseBtn.append(
				icon(collapsedNow ? ICONS.expand : ICONS.collapse, "h-4 w-4"),
			);
		}

		function applyCollapsed(next, persist) {
			collapsedNow = !!next;
			root.dataset.collapsed = collapsedNow ? "true" : "false";
			if (persist !== false) {
				try {
					localStorage.setItem(
						SIDEBAR_KEY,
						collapsedNow ? "collapsed" : "expanded",
					);
				} catch (e) {
					/* 隐私模式写不进去：本次会话有效即可 */
				}
			}
			paintCollapseBtn();
			paintCollapsedHints();
		}

		/* 折叠态的可访问性兜底：CSS 把文字收起来了（display:none / font-size:0），
		 * 但鼠标悬停与屏幕阅读器仍需拿到名字——折叠时补 title，展开时移除
		 * （展开态已有可见文字，再挂 title 只会多一层系统 tooltip 噪音）。
		 * 这里的 title 是 .side-link[data-tip]::after 自定义 tooltip 的后备：
		 * 后者在导航内部滚动或极窄屏下会被裁切，title 永远能出。 */
		function paintCollapsedHints() {
			const links = root.querySelectorAll("a.side-link");
			for (const a of links) {
				const label = a.dataset.tip || a.dataset.label || "";
				if (collapsedNow) a.setAttribute("title", label);
				else a.removeAttribute("title");
			}
			/* 品牌链接折叠后只剩 logo mark，补个 title 告诉它去哪 */
			const brand = root.querySelector(".app-brand > a");
			if (brand) {
				if (collapsedNow) brand.setAttribute("title", "Anotify");
				else brand.removeAttribute("title");
			}
		}

		const collapseBtn = el("button", {
			id: "sidebar-collapse",
			type: "button",
			/* 尺寸/位置全由 .app-brand-btn 提供（≤1023px 时它 display:none：折叠只属于桌面） */
			class: "app-brand-btn",
			onclick: () => applyCollapsed(!collapsedNow),
		});

		/* ----- 侧栏导航 ----- */
		const adminNav = el("div", {
			id: "admin-nav-section",
			/* 初始 hidden（member 用户保持隐藏，e2e 断言 classList 含 hidden）；
			   超管由 loadMe 去 hidden 并注入 .side-label + a.side-link[admin.html]。
			   它在 <nav> 之外：e2e 的 `#sidebar nav .side-label` 只该看到两个分组标签。
			   **不加 px-\***：这一组和 nav 里的 .side-group 必须同基线，多一层 12px
			   内边距会让「管理」标题和「管理后台」文字整体右移（用户反馈的对齐问题）。 */
			class: "hidden side-group pt-4",
		});
		const nav = el("nav", { class: "app-nav" });
		for (const group of NAV) {
			const g = el("div", { class: "side-group" });
			if (group.groupLabelKey)
				g.append(
					el("div", {
						class: "side-label",
						text: t(group.groupLabelKey, group.groupFallback),
					}),
				);
			for (const item of group.items) {
				const isActive = item.id === o.active;
				const label = t(item.labelKey, item.fallback);
				const a = el("a", {
					href: item.href,
					class: `side-link ${isActive ? "active" : ""}`,
					/* data-tip：折叠态的自定义 tooltip（ui.css 里
a.side-link[data-tip]::after），文本节点仍在，tooltip 只是视觉层。
					   data-label 保留为历史名字（wave 1 的 CSS 与外部脚本可能引用）。 */
					"data-tip": label,
					"data-label": label,
				});
				if (isActive) a.setAttribute("aria-current", "page");
				/* 顺序不能改：图标 → 标签文本。e2e 取 a.lastChild.nodeType === 3 当 label
				   （frontend.mjs:1813），后面再 append 任何元素都会把 label 挤掉。 */
				a.append(icon(ICONS[item.icon]), label);
				g.append(a);
			}
			nav.append(g);
		}

		/* ----- 命名空间槽位（P0-1：开页不再把 nav 往下顶） -----
		 * `#scope-switcher` 保持「数据到了才去掉 hidden」的语义不变：
		 * scripts/e2e/suites/frontend.mjs:4497-4505 把
		 * `#scope-switcher:not(.hidden)` 当作「命名空间已加载完成」信号，随后在同一个
		 * evaluate 里立刻取 `.scope-trigger` 点开菜单断言空态。所以这里不是给它去
		 * hidden，而是在外面再包一层 `#scope-slot`：骨架 `.scope-skel` 用 CSS 占住与
		 * 真实 `.scope-block` 完全相同的 53px（展开/折叠同高），数据到达后在同一个
		 * 同步块里「skel.remove() → 去 hidden」，几何不变，nav 永不下移。 */
		const scopeBox = el("div", { id: "scope-switcher", class: "hidden" });
		const scopeSlot = el(
			"div",
			{ id: "scope-slot", class: "scope-slot" },
			el(
				"div",
				{ class: "scope-skel", "aria-hidden": "true" },
				el("div", { class: "scope-skel-dot" }),
				el("div", { class: "scope-skel-bar" }),
			),
			scopeBox,
		);
		const nsSwitcherReady = mountNamespaceSwitcher(scopeBox, scopeSlot);

		/* ----- 侧栏品牌行：logo + 品牌字 + 折叠按钮 ----- */
		const brandRow = el(
			"div",
			{ class: "app-brand" },
			(() => {
				const a = el("a", {
					href: "index.html",
					class: "flex items-center gap-2.5",
				});
				a.append(
					logo(),
					el("span", {
						/* 折叠态由 CSS 隐藏 .app-brand-text（display:none），文本节点保留 */
						class: "app-brand-text font-script text-xl leading-none",
						text: "Anotify",
					}),
				);
				return a;
			})(),
			collapseBtn,
			/* 移动抽屉的可见关闭按钮：抽屉从左侧滑出后，遮罩之外没有「关」的
			 * 可点目标，键盘用户也没有可聚焦的出口。桌面折叠按钮在 ≤1023 隐藏，
			 * 这里补一个只在移动端出现的 X（样式见 ui.css .sidebar-close）。 */
			el(
				"button",
				{
					id: "sidebar-close",
					type: "button",
					class: "app-icon-btn sidebar-close",
					"aria-label": t("ui.action.close", "关闭"),
					onclick: () => closeSidebar(),
				},
				icon(["M6 6l12 12", "M18 6L6 18"], "h-4 w-4"),
			),
		);

		/* ----- 侧栏底部：语言切换 + 用户块 ----- */
		const logoutLink = el(
			"a",
			{
				href: "#logout",
				class: "side-icon-btn",
				title: t("ui.menu.logout", "退出登录"),
				"aria-label": t("ui.menu.logout", "退出登录"),
			},
			icon(ICONS.logout, "h-4 w-4"),
		);
		const userBlock = el(
			"div",
			{ class: "side-user" },
			/* 头像本身是链接（进账户安全页）：折叠态下这是 footer 里唯一的入口，
			 * 所以它必须具备可点击语义，不能只是个装饰性 span。 */
			el("a", {
				href: "security.html",
				id: "sidebar-avatar",
				/* ui.css 没给这个 id 配套组件类（.app-avatar-btn 是按钮样式，
				 * 带 cursor:pointer + hover 背景，用在这里会看起来像按钮）。
				 * 这里用 token 内联：两套主题自动跟着走，不出现硬编码色值。 */
				class:
					"flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-sm font-medium",
				style:
					"background: var(--accent-soft); color: var(--accent-strong); border: 1px solid var(--line-strong)",
				"aria-label": t("ui.menu.security", "账户安全"),
				text: (o.username[0] || "…").toUpperCase(),
			}),
			el(
				"div",
				{ class: "side-user-main" },
				el("div", {
					id: "sidebar-username",
					/* textContent 必须只剩用户名：e2e 断言 trim() 后逐字相等 */
					class: "truncate",
					text: o.username || "…",
				}),
				el("div", {
					class: "side-user-meta",
					text: t("common.sidebar.deployment", "私有化部署 · 主工作台"),
				}),
			),
			logoutLink,
		);

		const sidebar = el(
			"aside",
			{
				id: "sidebar",
				/* -translate-x-full = 移动端抽屉的「关闭态」（今天就是这样：手机上首屏
				   侧栏必须在屏外，桌面由 lg:translate-x-0 抵消）。折叠态只改宽度，绝不碰
				   这两个状态类 —— 两者是正交的两套状态。宽度/固定定位/层级全由
				   .app-sidebar （ui.css，含 ≤1023px 的抽屉媒体查询）负责。 */
				class:
					"app-sidebar -translate-x-full transform transition-transform duration-300 lg:static lg:translate-x-0",
			},
			el(
				"div",
				{ class: "flex h-full flex-col" },
				brandRow,
				scopeSlot,
				nav,
				adminNav,
				el(
					"div",
					{ class: "app-sidebar-foot" },
					buildLangSwitcher(),
					userBlock,
				),
			),
		);

		const overlay = el("div", {
			id: "sidebar-overlay",
			class: "sidebar-overlay fixed inset-0 z-30 hidden lg:hidden",
			onclick: () => closeSidebar(),
		});

		/* ----- 顶栏 ----- */
		const menuBtn = el(
			"button",
			{
				id: "menu-btn",
				type: "button",
				class: "app-icon-btn lg:hidden",
				"aria-label": t("common.nav.menu", "菜单"),
				"aria-controls": "sidebar",
				"aria-expanded": "false",
				onclick: openSidebar,
			},
			icon(["M3 6h18", "M3 12h18", "M3 18h18"], "h-5 w-5"),
		);

		/* 汉堡只属于移动端。不能只靠 Tailwind 的 lg:hidden：ui.css 里的
		 * .app-icon-btn 也声明了 display（文件在后，同优先级赢），会把 lg:hidden
		 * 的 display:none 覆盖掉，结果是桌面顶栏多出一个汉堡、点一下还会锁住
		 * body 滚动。用内联 display 兜底（内联 > 任何选择器），顺带正确处理缩放。 */
		const mobileMQ = window.matchMedia("(max-width: 1023px)");
		const syncMenuBtn = () => {
			menuBtn.style.display = mobileMQ.matches ? "" : "none";
		};
		syncMenuBtn();
		if (typeof mobileMQ.addEventListener === "function")
			mobileMQ.addEventListener("change", syncMenuBtn);
		else if (typeof mobileMQ.addListener === "function")
			mobileMQ.addListener(syncMenuBtn);

		/* D3：从抽屉态放大到桌面后，侧栏不再是模态框 —— 只剥角色，不碰滚动锁。 */
		const syncDrawerRole = () => {
			if (mobileMQ.matches) return;
			setDrawerDialog(document.getElementById("sidebar"), false);
		};
		syncDrawerRole();
		if (typeof mobileMQ.addEventListener === "function")
			mobileMQ.addEventListener("change", syncDrawerRole);
		else if (typeof mobileMQ.addListener === "function")
			mobileMQ.addListener(syncDrawerRole);

		/* 主题三态按钮：图标/文案由 paintThemeToggle 画（依赖当前 mode，不是 resolved） */
		const themeBtn = el("button", {
			id: "theme-toggle",
			type: "button",
			class: "app-icon-btn",
			onclick: () => window.Anotify.theme.cycle(),
		});

		const bellBtn = el(
			"button",
			{
				type: "button",
				class: "app-icon-btn relative",
				"aria-label": t("common.nav.notification", "通知"),
				onclick: () => (location.href = "index.html#recent"),
			},
			icon(
				[
					"M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9",
					"M13.73 21a2 2 0 0 1-3.46 0",
				],
				"h-5 w-5",
			),
			el("span", {
				class: "absolute right-1.5 top-1.5 h-2 w-2 rounded-full",
				style: "background: var(--error)",
			}),
		);

		const avatarBtn = el("button", {
			id: "topbar-avatar",
			type: "button",
			/* 尺寸/圆形/主题色全由 .app-avatar-btn 提供（ui.css） */
			class: "app-avatar-btn",
			"aria-haspopup": "menu",
			"aria-expanded": "false",
			"aria-label": t("ui.menu.account", "账户菜单"),
			/* 内容只允许是首字母文本节点：loadMe 用 textContent 覆盖它 */
			text: (o.username[0] || "…").toUpperCase(),
		});

		const topbarMenu = el("div", {
			id: "topbar-menu",
			/* 定位/层级由 .menu 负责；hidden 是唯一的开合状态类 */
			class: "menu hidden",
			role: "menu",
		});
		topbarMenu.append(
			el(
				"a",
				{ class: "menu-item", role: "menuitem", href: "security.html" },
				icon(ICONS.security, "h-4 w-4"),
				t("ui.menu.security", "账户安全"),
			),
			el(
				"button",
				{
					type: "button",
					class: "menu-item",
					role: "menuitem",
					onclick: () => {
						setTopbarMenu(false, false);
						logout();
					},
				},
				icon(ICONS.logout, "h-4 w-4"),
				t("ui.menu.logout", "退出登录"),
			),
		);

		/* 头像菜单开合：Esc 关、点外部关、关闭后焦点回 #topbar-avatar */
		let topbarMenuOpen = false;
		function setTopbarMenu(open, focusAvatar) {
			topbarMenuOpen = !!open;
			topbarMenu.classList.toggle("hidden", !topbarMenuOpen);
			avatarBtn.setAttribute(
				"aria-expanded",
				topbarMenuOpen ? "true" : "false",
			);
			if (!topbarMenuOpen && focusAvatar) avatarBtn.focus();
		}
		avatarBtn.addEventListener("click", (e) => {
			e.stopPropagation();
			setTopbarMenu(!topbarMenuOpen, false);
		});
		document.addEventListener("click", (e) => {
			if (!topbarMenuOpen) return;
			if (topbarMenu.contains(e.target) || avatarBtn.contains(e.target)) return;
			setTopbarMenu(false, true);
		});
		document.addEventListener("keydown", (e) => {
			if (e.key !== "Escape" || !topbarMenuOpen) return;
			e.preventDefault();
			setTopbarMenu(false, true);
		});

		const header = el(
			"header",
			{
				class:
					"app-topbar sticky top-0 z-20 flex items-center justify-between gap-4 border-b px-5 sm:px-8",
			},
			el(
				"div",
				{ class: "flex items-center gap-3 min-w-0" },
				menuBtn,
				el(
					"div",
					{ class: "min-w-0" },
					/* 全站唯一的 h1。SH-4：它不再重复页面标题（.page-title 已经 28px 写了
					 * 一遍，16 页里有 15 页两份同文字叠在一屏），改成「上下文」：
					 *   工作台 · #default    /    资源 · 全部署通用
					 * 分组来自侧栏 IA（NAV 的 groupLabelKey），命名空间来自当前 scope。
					 * 页面名一个都没丢：完整标题挂在 h1 的 title 与 <title> 上。 */
					(() => {
						const h = el("h1", {
							class: "app-topbar-title",
							title: o.title || "",
						});
						const grp = NAV_GROUP_OF[o.active] || null;
						if (grp) {
							h.append(
								el("span", {
									class: "app-topbar-group",
									text: t(grp.labelKey, grp.fallback),
								}),
							);
						}
						if (o.scopeLevel === "deployment") {
							if (grp) h.append(el("span", { class: "app-topbar-sep", text: "·" }));
							h.append(
								el("span", {
									class: "scope-badge",
									title: t(
										"scope.deployment_wide_hint",
										"这一页的内容不随命名空间切换变化",
									),
									text: t("scope.deployment_wide", "全部署通用"),
								}),
							);
						} else {
							const scopeEl = el("span", {
								class: "app-topbar-scope",
							});
							if (grp) h.append(el("span", { class: "app-topbar-sep", text: "·" }));
							h.append(scopeEl);
							scopeCrumbEl = scopeEl;
							paintScopeCrumb();
						}
						return h;
					})(),
					/* 副标题去重：页面自己渲染了 .page-sub（hero 副标题）时，
					   顶栏不再渲染同向的 subtitle —— 否则 events / sources 这类页会在
					   两块相邻区域重复同一句话。运行时判定，不改 17 个页面的调用点。
					   优先看 [data-page-sub]（显式声明），再退回 .page-sub。 */
					pageMain.querySelector(".page-sub, [data-page-sub]")
						? null
						: el("p", { class: "app-topbar-sub", text: o.subtitle }),
				),
			),
			el(
				"div",
				{ class: "relative flex items-center gap-2" },
				themeBtn,
				bellBtn,
				avatarBtn,
				topbarMenu,
			),
		);

		/* ----- 重组 DOM ----- */
		const footer = el("footer", {
			class: "app-footer",
			text: t(
				"common.footer.copyright",
				"Anotify · MIT License · 私有化部署 · 数据仅存于你的服务器",
			),
		});
		pageMain.parentNode && pageMain.parentNode.removeChild(pageMain);
		const main = el("main", { class: "app-main flex-1 px-5 py-8 sm:px-8" });
		main.appendChild(pageMain);

		const appCol = el(
			"div",
			{ class: "app-col flex min-w-0 flex-1 flex-col" },
			header,
			main,
			footer,
		);
		const root = el(
			"div",
			{
				id: "app-shell",
				class: "app-shell flex min-h-dvh",
				"data-collapsed": collapsedNow ? "true" : "false",
			},
			sidebar,
			overlay,
			appCol,
		);
		document.body.prepend(root);
		/* P1：外壳已经挂载 —— 关掉 base.html <head> 里的预排版规则
		 * （html:not([data-shell-ready]) #page-main 的左/上内距）。必须同步、同一任务，
		 * 否则正常加载会先按预排版排一帧、再被真正的 .app-main 排一遍（双倍 padding）。 */
		document.documentElement.dataset.shellReady = "1";

		/* 首帧把折叠态与主题按钮画好（persist=false：这次只是回放已存的状态） */
		applyCollapsed(collapsedNow, false);
		paintThemeToggle(readThemeMode());

		/* 移动端抽屉：Esc 关闭；点导航链接自动关闭（不抢焦点，马上要跳页） */
		document.addEventListener("keydown", (e) => {
			if (e.key === "Escape" && !overlay.classList.contains("hidden"))
				closeSidebar();
		});

		/* 焦点陷阱（D2）：抽屉开着时 Tab / Shift+Tab 在面板内首尾循环。
		   不用 inert —— 它会挡住 e2e 的合成点击；也不改 tabindex。
		   守卫必须同时认断点：≥1024 时遮罩是靠 CSS（lg:hidden）藏的，`hidden` 类
		   还在，只查 hidden 会让「移动端开抽屉 → 放大到桌面」后仍被陷阱卡住
		   （桌面侧栏只是常驻导航，WCAG 2.1.2 键盘陷阱）。 */
		document.addEventListener("keydown", (e) => {
			if (e.key !== "Tab") return;
			if (!mobileMQ.matches || overlay.classList.contains("hidden")) return;
			const items = Array.prototype.filter.call(
				sidebar.querySelectorAll(
					'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
				),
				(n) => n.offsetWidth > 0 || n.offsetHeight > 0,
			);
			if (!items.length) return;
			const first = items[0];
			const last = items[items.length - 1];
			const active = document.activeElement;
			const inside = sidebar.contains(active);
			if (e.shiftKey) {
				if (!inside || active === first) {
					e.preventDefault();
					last.focus();
				}
			} else if (!inside || active === last) {
				e.preventDefault();
				first.focus();
			}
		});
		sidebar.addEventListener("click", (e) => {
			const a =
				e.target && e.target.closest ? e.target.closest("a.side-link") : null;
			if (a) closeSidebar(false);
		});

		/* 滚动渐入 */
		const io = new IntersectionObserver(
			(entries) =>
				entries.forEach(
					(e) => e.isIntersecting && e.target.classList.add("visible"),
				),
			{ threshold: 0.08 },
		);
		document.querySelectorAll(".reveal").forEach((n) => io.observe(n));

		/* 退出登录：拦截侧栏底部用户块里的 logout 图标 */
		document.querySelectorAll('a[href="#logout"]').forEach((a) =>
			a.addEventListener("click", (e) => {
				e.preventDefault();
				logout();
			}),
		);

		/* 异步填充真实用户名（覆盖占位） */
		loadMe().then((me) => {
			if (!me) return;
			const name = me.displayName || me.username || "";
			const initial = (name[0] || "A").toUpperCase();
			const su = document.getElementById("sidebar-username");
			const sa = document.getElementById("sidebar-avatar");
			const ta = document.getElementById("topbar-avatar");
			if (su) su.textContent = name;
			if (sa) sa.textContent = initial;
			if (ta) ta.textContent = initial;

			if (me.role !== "admin") return;

			/* 超管：在侧栏注入「管理后台」入口（dataset.mounted 保证幂等） */
			const section = document.getElementById("admin-nav-section");
			if (section && !section.dataset.mounted) {
				section.dataset.mounted = "1";
				section.classList.remove("hidden");
				section.append(
					el("div", {
						class: "side-label",
						text: t("common.nav.admin_section", "管理"),
					}),
				);
				const label = t("common.nav.admin", "管理后台");
				const a = el("a", {
					href: "admin.html",
					class: `side-link ${o.active === "admin" ? "active" : ""}`,
					"data-label": label,
				});
				if (o.active === "admin") a.setAttribute("aria-current", "page");
				a.append(
					icon(["M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"]),
					label,
				);
				section.append(a);
			}

			/* 超管：顶栏头像菜单里补一条「管理后台」（插在退出登录之前） */
			if (!topbarMenu.querySelector('a[href="admin.html"]')) {
				const adminItem = el(
					"a",
					{ class: "menu-item", role: "menuitem", href: "admin.html" },
					icon(["M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"], "h-4 w-4"),
					t("ui.menu.admin", "管理后台"),
				);
				topbarMenu.insertBefore(
					adminItem,
					topbarMenu.querySelector("button.menu-item"),
				);
			}
		});

		/* Language hint banner: must run after body.prepend(root) above
		 * so the banner lands as body's first child (above the layout root). */
		mountLangHint();
	}

	/* ---------- API 封装 ---------- */
	// 需登录的页面（工作台各页）；login.html 等公开页不设此标记
	const isAuthedPage = !/login\.html$/.test(location.pathname);

	/* ---------- 命名空间（当前范围） ---------- */
	//
	// 命名空间是隔离单位（docs/design/namespaces.md）：sources / edges / events /
	// tasks / stats / capabilities / nodes / playbooks 全都属于命名空间。
	// 组织那一层在 UI 上已经不存在了（#203），前端手里只有命名空间。
	//
	// v4：命名空间资源全部走显式路径 /v1/namespaces/{namespace}/...，scope 里
	// 只存 `{ namespace }`（= Namespace 的 metadata.name）。v3 的命名空间 query
	// 补参与扁平路径兼容层已彻底删除（不保留旧库/旧字段兜底）。
	//
	// 选择记在 localStorage：刷新、切页面、深链都保持同一个范围。
	const SCOPE_KEY = "anotify.scope";
	let scope = readScope();

	function readScope() {
		try {
			return JSON.parse(localStorage.getItem(SCOPE_KEY) || "{}") || {};
		} catch (e) {
			return {};
		}
	}
	function saveScope(v) {
		scope = Object.assign({}, scope, v || {});
		// v3 的 scope 里还有一个 `namespaceId`（旧扁平接口的 query 参数）。它已经
		// 没有任何消费方了，留着只会在 localStorage 里冒充“当前范围”，写回前清掉。
		delete scope.namespaceId;
		try {
			localStorage.setItem(SCOPE_KEY, JSON.stringify(scope));
		} catch (e) {
			/* 隐私模式下写不进去：退化成"本次会话有效"，不影响功能 */
		}
	}

	// 深链的显式命名空间（列表/详情链接都带 `?namespace=`）是权威范围。
	//
	// 没有这个参数时一律不动持久化 scope（默认行为不变）；带了就在**任何**读取、
	// 侧栏挂载或写入之前把 scope 对齐到 URL。用持久化的 default 去读/写 prod 的
	// 资源不会触发 CAS —— 那是另一个 namespace 里的合法资源 —— 只能靠这条前置。
	//
	// 只在**页面初始化**时采纳一次（`urlNamespaceAdopted`）：页内切换器 / 页内选择框
	// （事件页的 namespace filter 等）会把新范围写进 scope，之后每次请求都重新读 URL
	// 会把它顶回去 —— 深链优先只属于“进场”，不属于“每一条请求”。
	let urlNamespaceAdopted = false;
	function adoptUrlNamespace() {
		if (urlNamespaceAdopted) return scope;
		let explicit = "";
		try {
			explicit = (new URLSearchParams(location.search).get("namespace") || "").trim();
			urlNamespaceAdopted = true;
		} catch (e) {
			return scope; // 畸形 query 不是深链：保持持久化范围（下次还能重试）
		}
		if (explicit && explicit !== scopeNamespace()) saveScope({ namespace: explicit });
		return scope;
	}

	// 切换范围：持久化 scope 与 URL 的 `?namespace=` 一起改，再整页重载。
	// 只改 localStorage 的话，深链页 reload 时 adoptUrlNamespace() 会把用户刚选的
	// 范围顶回 URL 里那个旧值 —— 切换器会变成摆设。
	function switchScope(namespace) {
		saveScope({ namespace: namespace });
		try {
			const url = new URL(location.href);
			if (url.searchParams.has("namespace")) {
				url.searchParams.set("namespace", namespace);
				history.replaceState(null, "", url.pathname + url.search + url.hash);
			}
		} catch (e) {
			/* 没有 URL/history 的环境（file://、旧浏览器）退化为仅持久化 */
		}
		location.reload();
	}

	// scope 里的命名空间 name（= Namespace.metadata.name，路径里的 {namespace}）。
	function scopeNamespace() {
		const ns = scope && typeof scope.namespace === "string" ? scope.namespace : "";
		return ns.trim();
	}

	function scopeMissingError() {
		const err = new Error(t("scope.missing", "还没有确定命名空间"));
		err.reason = "namespace_required";
		return err;
	}

	/**
	 * v4 命名空间私有资源路径：`/v1/namespaces/{ns}/{plural}[/{name}]`。
	 *
	 * 拿不到 scope 就直接抛（`reason = namespace_required`）：宁可显示错误，
	 * 也不能静默请求 `/v1/namespaces//capabilities` 这种畸形 URL。
	 */
	function nsPath(plural, name) {
		const ns = scopeNamespace();
		if (!ns) throw scopeMissingError();
		const base = "/v1/namespaces/" + encodeURIComponent(ns) + "/" + String(plural);
		return name == null || name === "" ? base : base + "/" + encodeURIComponent(name);
	}

	/** cluster-scoped 资源路径：`/v1/{plural}[/{name}]`（nodes/namespaces/...）。 */
	function clusterPath(plural, name) {
		const base = "/v1/" + String(plural);
		return name == null || name === "" ? base : base + "/" + encodeURIComponent(name);
	}

	// 默认命名空间引导（#162）。
	//
	// 全新浏览器（清过缓存 / 换了设备）第一次打开页面时 scope 是空的；没有
	// scope 就拼不出 v4 路径，页面直接空白。这里在缺省时拉一次
	// `GET /v1/namespaces`，从 `items[].metadata.name` 里取 `default`（没有就取
	// 第一项）存进 scope。
	//
	// v4 严格：`items` 非数组（裸数组 / `{namespaces:[]}`）视为失败，不静默回退
	// 旧形状；失败不缓存（后端刚起来时下一次请求还能再试）。失败不阻塞页面，
	// 但也**不会**让已有请求变通：`nsApi`/`nsList`/`ready` 先 await 本函数再拼
	// ns 路径；引导失败时由 `nsPath()` 把“没有 scope”报成可显示错误。
	let scopeReady = null;
	function ensureScope() {
		// 所有 ns 路径/请求的唯一入口：先对齐 URL 的显式 namespace，再判断要不要引导。
		adoptUrlNamespace();
		if (scopeNamespace()) return Promise.resolve(scope);
		if (scopeReady) return scopeReady;
		scopeReady = fetch("/v1/namespaces", { credentials: "include" })
			.then((r) => (r.ok ? r.json() : null))
			.then((j) => {
				if (!j || !Array.isArray(j.items)) {
					// 畸形列表：不能当成空列表，也不能缓存这次结果
					scopeReady = null;
					return scope;
				}
				const names = j.items
					.map((it) => (it && it.metadata ? it.metadata.name : null))
					.filter((n) => typeof n === "string" && n);
				if (!names.length) return scope;
				saveScope({
					namespace: names.indexOf("default") >= 0 ? "default" : names[0],
				});
				return scope;
			})
			.catch(() => {
				// 失败不缓存：下一次请求还能再试（后端刚起来时常见）
				scopeReady = null;
				return scope;
			});
		return scopeReady;
	}

	/** `ready()`：页面在拼 ns 路径、渲染 ns 相关数据前先 `await ready()`。 */
	function ready() {
		return ensureScope();
	}

	/**
	 * 页面数据入口（ns 私有资源）：**先**引导 scope，**再**拼路径发请求。
	 *
	 * 直接写 `api(nsPath("capabilities"))` 是错的：参数先于 `api()` 求值，
	 * scope 还没引导就已经抛 `namespace_required`。这里把顺序固定住；
	 * 引导失败不兜底 —— 拿不到命名空间时 `nsPath()` 照常抛可显示错误。
	 */
	async function nsApi(plural, name, opts) {
		await ensureScope();
		return api(nsPath(plural, name), opts);
	}

	/** 列表版 `nsApi`：先引导 scope，再 GET `KindList`（严格 `items[]` 校验）。 */
	async function nsList(plural) {
		await ensureScope();
		return apiList(nsPath(plural));
	}

	/**
	 * loadSourceTopology —— 「一个源的配置拓扑」的唯一异步入口。
	 *
	 * 三个页面（首页「最新事件链路」/ 链路页 / 源详情页）都要把「源 + 能力表」喂给
	 * `AnotifyGraphs.sourceTopology`。**取数与降级规则只写这一份**，页面不要再各抄一遍：
	 *   · 取源走**列表**（源已删 / trace 来自未知源时，单条 GET 会打一条 4xx，
	 *     而"页面不该有失败请求"是观测断言里的硬约束）；
	 *   · 拿不到源 / 图库未加载 → 返回 null，调用方退回"只画执行链"：
	 *     不白屏，也不编造一条不存在的拓扑。
	 *
	 * @param {string} name 源名（metadata.name）
	 * @param {{sources?: Array, capabilities?: Array, edges?: Array}} [opts]
	 *        已经取到的资源（源详情页手上就有），省掉重复请求；缺省时自己拉。
	 * @returns {Promise<{nodes: Array, edges: Array, truncated: boolean}|null>}
	 */
	async function loadSourceTopology(name, opts) {
		const o = opts || {};
		const wanted = String(name == null ? "" : name).trim();
		if (!wanted) return null;
		const graphs = window.AnotifyGraphs;
		if (!graphs || typeof graphs.sourceTopology !== "function") return null;
		const asItems = (res) => (Array.isArray(res) ? res : itemsOf(res));
		try {
			const [sources, capabilities] = await Promise.all([
				o.sources ? asItems(o.sources) : nsList("sources").then(asItems),
				o.capabilities ? asItems(o.capabilities) : nsList("capabilities").then(asItems),
			]);
			const source = sources.find((s) => s && s.metadata && s.metadata.name === wanted) || null;
			if (!source) return null;
			return graphs.sourceTopology({ source, capabilities, edges: o.edges || [] });
		} catch (error) {
			return null;
		}
	}

	// apiError 把 v4 的 `Status` 错误体摊平成 Error 上的字段：
	//	 err.status / code / reason / message / details / currentResourceVersion。
	// `err.message` 同时保留在 `err.detail`（老页面/保留域仍在读 detail）。
	// 非 Status 的响应体（网关错误页、旧式 `{error}`）不伪造 reason，只留原文。
	function apiError(status, body) {
		const st = body && typeof body === "object" ? body : null;
		const isStatus = !!st && (st.kind === "Status" || typeof st.reason === "string");
		const message = st ? String(st.message || st.error || "") : "";
		const err = new Error(message || "HTTP " + status);
		err.status = status;
		err.detail = message;
		if (isStatus) {
			err.code = st.code != null ? st.code : status;
			err.reason = st.reason || "";
			err.details = st.details || null;
			const cur = st.details && st.details.current;
			err.currentResourceVersion = (cur && cur.resourceVersion) || null;
		}
		if (st && Array.isArray(st.warnings)) err.warnings = st.warnings;
		return err;
	}

	/**
	 * redirectToLogin 跳登录页：页名走白名单、查询串只允许安全字符，防 open redirect。
	 * 只由 api() 在**首个** 401 时调用一次（并发请求不会互相打断同一个导航）。
	 */
	function redirectToLogin() {
		const PAGES = [
			"index.html",
			"message.html",
			"receivers.html",
			"keys.html",
			"events.html",
			// trace 页 (#107)：同样是带查询串的深层链接
			"trace.html",
			"sources.html",
			// 源详情页 (#112)：页面私有的深层链接，必须留在白名单里，
			// 否则 401 后 next= 会回落到 index.html，把 ?id= 丢掉。
			"source.html",
			"capabilities.html",
			"nodes.html",
			"security.html",
			"admin.html",
			"docs.html",
			"cli-auth.html",
			"passkey-enroll.html",
		];
		const name = location.pathname.split("/").pop();
		const page = PAGES.includes(name) ? name : "index.html";
		// 保留查询串（如推送深链 ?msg=<id>），登录后仍能跳到目标消息。
		const qs = /^[?][\w\-=&%.]*$/.test(location.search) ? location.search : "";
		location.href = "login.html?next=" + encodeURIComponent(page + qs);
	}

	// 一次 401 只跳一次登录页：页面上多个并发请求（me / namespaces / 列表）会各自
	// 拿到 401，重复写 location.href 会让浏览器反复取消/重启同一个导航（表现为
	// net::ERR_ABORTED，登录页反而可能没加载完）。首个 401 负责跳转，其余只抛错。
	let loginRedirecting = false;

	async function api(path, opts) {
		const o = opts || {};
		// scope 引导顺手做一次（cluster 路径、保留域页面也受益；ensureScope 内部
		// 已 catch，不会 reject）。但它**改不了已经求值过的 path** —— ns 私有资源
		// 必须走 `nsApi`/`nsList`，先引导再拼路径。
		await ensureScope();
		const res = await fetch(path, Object.assign({ credentials: "include" }, o));
		// 401 = 未登录/会话过期：**首个** 401 负责跳登录页（见 loginRedirecting），
		// 不是“后端未连接”：如实跳转，不伪造数据。
		if (res.status === 401) {
			if (isAuthedPage && !loginRedirecting) {
				loginRedirecting = true;
				redirectToLogin();
			}
			throw new Error("HTTP 401");
		}
		if (!res.ok) {
			// 服务端的校验信息（"cron 表达式需要 5 段"、"metadata.resourceVersion
			// 缺失"）比 "HTTP 400" 有用得多；v4 统一是 Status 信封。
			let body = null;
			try {
				body = await res.json();
			} catch (e) {
				/* 响应不是 JSON（网关错误页等）：保持 body=null */
			}
			throw apiError(res.status, body);
		}
		// 204 No Content / 205 Reset Content 没有响应体（v4 的 DELETE 一律 204，见
		// internal/server/resource_*.go 的 `WriteHeader(http.StatusNoContent)`）。
		// 无条件 `res.json()` 会抛 SyntaxError，把一次**成功**的删除变成页面报错、
		// 列表不刷新（source-actions 的删除分支就是这么红的）。
		if (res.status === 204 || res.status === 205) return null;
		// 其它成功状态也允许「无内容」：先读原文，空体给 null 而不是解析失败；
		// 有体但不是 JSON（网关的 text/plain 200）同样按无内容处理，不伪造资源。
		const text = typeof res.text === "function" ? await res.text() : "";
		if (text.trim() === "") return null;
		try {
			return JSON.parse(text);
		} catch (e) {
			const err = new Error("HTTP " + res.status + " 响应不是合法 JSON");
			err.status = res.status;
			err.detail = text;
			err.cause = e;
			throw err;
		}
	}

	/* ---------- 当前用户 / 登出 ---------- */
	// loadMe 获取当前登录用户（/v1/auth/me）；未登录返回 null（api 会在 401 跳登录）。
	async function loadMe() {
		try {
			return await api("/v1/auth/me");
		} catch {
			return null;
		}
	}

	// logout 调后端吊销会话并跳回登录页。
	async function logout() {
		try {
			await fetch("/v1/auth/logout", {
				method: "POST",
				credentials: "include",
			});
		} catch {
			/* 忽略网络错误，仍跳登录页 */
		}
		location.href = "login.html";
	}

	/* ---------- 小工具 ---------- */
	function copyText(btn, getText) {
		const text =
			typeof getText === "function"
				? getText()
				: document.querySelector(getText).innerText;
		navigator.clipboard.writeText(text).then(() => {
			const orig = btn.textContent;
			btn.textContent = t("common.copy.copied", "已复制 ✓");
			setTimeout(() => (btn.textContent = orig), 1500);
		});
	}

	/* 共享 toast（#anotify-toast-host）。时序：入场 180ms → 停留 3600ms →
	 * 退场 160ms 后 remove。CSS 动画在 ui.css 的 .anotify-toast.is-in/.is-out；
	 * 位置（顶部居中）与层级（--z-toast，必须高于弹层）也在 ui.css —
	 * 这里只给类名，避免同一件事在 JS 的 Tailwind 串和 CSS 里各写一份。
	 * reduce 下不加动画类，直接 remove。退场时长从 --dur-mid 读，避免与
	 * ui.css 双份硬编码（reviewer minor）。 */
	function toastExitMs() {
		const raw = getComputedStyle(document.documentElement)
			.getPropertyValue("--dur-mid")
			.trim();
		return parseFloat(raw) || 160;
	}

	function toast(msg, kind) {
		let host = document.getElementById("anotify-toast-host");
		if (!host) {
			host = el("div", {
				id: "anotify-toast-host",
				class: "anotify-toast-host",
			});
			document.body.append(host);
		}
		/* kind 归一化：error / err / danger（以及布尔 true）都按错误处理。
		 * 旧实现只认精确的 "error"，其他失败调用会落回绿点。 */
		const isError =
			kind === "error" || kind === "err" || kind === "danger" || kind === true;
		const color = isError
			? "var(--error)"
			: kind === "warn"
				? "var(--warn)"
				: "var(--success)";
		const node = el(
			"div",
			{
				class:
					"card anotify-toast tx-name flex items-center gap-2.5 px-4 py-3 text-sm shadow-2xl" +
					(isError ? " is-error" : "") +
					(REDUCE_MOTION.matches ? "" : " is-in"),
				style: "background: var(--bg-raise)",
			},
			el("span", { class: "dot", style: `background: ${color}` }),
			msg,
		);
		host.append(node);
		setTimeout(() => {
			if (REDUCE_MOTION.matches) {
				node.remove();
				return;
			}
			node.classList.remove("is-in");
			node.classList.add("is-out");
			setTimeout(() => node.remove(), toastExitMs());
		}, 3600);
	}

	/* WebAuthn ArrayBuffer <-> base64url */
	function b64urlToBuf(b64) {
		const s = b64.replace(/-/g, "+").replace(/_/g, "/");
		const bin = atob(s + "=".repeat((4 - (s.length % 4)) % 4));
		const buf = new Uint8Array(bin.length);
		for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
		return buf.buffer;
	}
	function bufToB64url(buf) {
		const bytes = new Uint8Array(buf);
		let bin = "";
		for (const b of bytes) bin += String.fromCharCode(b);
		return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
	}

	/* 平台图标（按 platform 字段） */
	const PLATFORM_META = {
		ios: { icon: "📱", label: "iOS Safari" },
		mac: { icon: "💻", label: "macOS" },
		win: { icon: "🖥️", label: "Windows" },
		android: { icon: "🤖", label: "Android" },
		other: { icon: "🌐", label: t("common.platform.browser", "浏览器") },
	};

	function detectPlatform() {
		const ua = navigator.userAgent || "";
		if (/iPhone|iPad|iPod/.test(ua)) return "ios";
		if (/Android/.test(ua)) return "android";
		if (/Mac OS X/.test(ua)) return "mac";
		if (/Windows/.test(ua)) return "win";
		return "other";
	}

	function timeAgo(isoOrSec) {
		if (!isoOrSec) return "—";
		/* NOTE: local must not be named `t` — it would shadow the i18n t(). */
		const ts =
			typeof isoOrSec === "number" ? isoOrSec * 1000 : Date.parse(isoOrSec);
		if (Number.isNaN(ts)) return "—";
		const diff = Date.now() - ts;
		if (diff < 60e3) return t("common.time.just_now", "刚刚");
		if (diff < 3600e3)
			return t("common.time.minutes_ago", "{n} 分钟前").replace(
				"{n}",
				Math.floor(diff / 60e3),
			);
		if (diff < 86400e3)
			return t("common.time.hours_ago", "{n} 小时前").replace(
				"{n}",
				Math.floor(diff / 3600e3),
			);
		return t("common.time.days_ago", "{n} 天前").replace(
			"{n}",
			Math.floor(diff / 86400e3),
		);
	}

	/* ---------- 下拉刷新（触屏 / standalone PWA 公共能力） ----------
	 * 各列表/详情页一行启用：mountPullRefresh(onRefresh)
	 * 仅在触屏设备或 display-mode: standalone 时激活，桌面浏览器不启用。
	 * 指示器 DOM 由本函数自建，调用页无需改 HTML；文案走 i18n
	 * ui.index.pull_hint / pull_ready / pull_loading / pull_done。
	 */
	function mountPullRefresh(onRefresh, opts) {
		if (typeof onRefresh !== "function") return;
		const isTouch =
			"ontouchstart" in window ||
			window.matchMedia("(pointer: coarse)").matches ||
			window.matchMedia("(display-mode: standalone)").matches ||
			window.navigator.standalone === true;
		if (!isTouch) return;

		const THRESHOLD = (opts && opts.threshold) || 70;
		const MAX = (opts && opts.max) || 110;

		/* 指示器 DOM（fixed 顶部，初始上移隐藏在视口外） */
		const spinner = icon(
			["M21 12a9 9 0 1 1-6.219-8.56"],
			"h-4 w-4 tx-meta transition-transform duration-200",
		);
		spinner.id = "pull-spinner";
		const textEl = el("span", {
			id: "pull-text",
			text: t("ui.index.pull_hint", "下拉刷新"),
		});
		const pill = el(
			"div",
			{
				class:
					"card tx-meta flex items-center gap-2 rounded-full px-4 py-2 text-xs backdrop-blur-sm",
			},
			spinner,
			textEl,
		);
		const indicator = el(
			"div",
			{
				id: "pull-indicator",
				/* 不用 -translate-y-full class：inline transform 会覆盖 class，
				   回弹要靠 inline 设回 -100% 才能隐藏 */
				class:
					"pointer-events-none fixed inset-x-0 top-0 z-[60] flex items-center justify-center transition-transform duration-200 ease-out",
				style: "transform: translateY(-100%)",
				"aria-hidden": "true",
			},
			pill,
		);
		document.body.append(indicator);

		/* spin 动画 */
		const style = document.createElement("style");
		style.textContent =
			"@keyframes anotify-spin { to { transform: rotate(360deg); } }";
		document.head.append(style);

		let state = "idle"; /* idle | pulling | ready | loading */
		let startY = 0;
		let delta = 0;

		function setState(s) {
			state = s;
			const labels = {
				idle: t("ui.index.pull_hint", "下拉刷新"),
				pulling: t("ui.index.pull_hint", "下拉刷新"),
				ready: t("ui.index.pull_ready", "松开刷新"),
				loading: t("ui.index.pull_loading", "正在刷新…"),
				done: t("ui.index.pull_done", "已刷新"),
			};
			textEl.textContent = labels[s] || labels.idle;
			spinner.style.animation =
				s === "loading" ? "anotify-spin 0.8s linear infinite" : "";
		}

		function applyOffset(offset) {
			/* offset<=0 → 隐藏在视口外（-100%）；否则跟手下移 */
			indicator.style.transform =
				offset <= 0
					? "translateY(-100%)"
					: `translateY(${Math.max(0, offset)}px)`;
			if (state === "pulling" || state === "ready") {
				const ratio = Math.min(offset / THRESHOLD, 1);
				spinner.style.transform = `rotate(${ratio * 180}deg)`;
			}
		}

		async function trigger() {
			if (state === "loading") return;
			setState("loading");
			applyOffset(THRESHOLD);
			try {
				await onRefresh();
				setState("done");
			} catch {
				setState("done");
			} finally {
				setTimeout(() => {
					applyOffset(0);
					spinner.style.transform = "";
					setTimeout(() => setState("idle"), 200);
				}, 400);
			}
		}

		document.addEventListener(
			"touchstart",
			(e) => {
				if (state === "loading") return;
				if (window.scrollY > 0) return;
				startY = e.touches[0].clientY;
				delta = 0;
				setState("pulling");
			},
			{ passive: true },
		);

		document.addEventListener(
			"touchmove",
			(e) => {
				if (state !== "pulling" && state !== "ready") return;
			delta = e.touches[0].clientY - startY;
				if (delta <= 0) {
					applyOffset(0);
					return;
			}
				if (e.cancelable) e.preventDefault();
				const damped =
					delta < MAX ? delta : MAX + (delta - MAX) * 0.3;
				applyOffset(Math.min(damped, MAX + 20));
				if (damped >= THRESHOLD) {
					if (state !== "ready") {
						setState("ready");
						if (navigator.vibrate) navigator.vibrate(10);
					}
				} else if (state === "ready") {
					setState("pulling");
				}
			},
			{ passive: false },
		);

		document.addEventListener("touchend", () => {
			if (state === "ready") {
				trigger();
			} else if (state === "pulling") {
				applyOffset(0);
				spinner.style.transform = "";
				setTimeout(() => setState("idle"), 200);
			}
		});

		return { trigger };
	}

	/* 主题 API（契约 §4.3）：get() 返回**模式**（auto|dark|light），不是解析后的
	 * 主题（dark|light）——调用方想知道的是“用户选了什么”，不是“今天渲染成什么”。 */
	const themeApi = {
		get: () => readThemeMode(),
		set: (mode) => applyTheme(mode, true),
		cycle: () => cycleTheme(),
	};

	/**
	 * capLabel 统一决定"能力在界面上怎么显示"。
	 *
	 * 一个地方决定，所有页面照用 —— 否则 capabilities/edges/sources/图
	 * 各拼各的，用户给能力取了名字却只有某一页生效，比不取还困惑。
	 *
	 * 规则：
	 *   displayName 有值 → 「分析 PR」（干净、给人读）
	 *   displayName 为空 → 「analyze-pr@1」（回落到真名，不隐藏技术身份）
	 *
	 * 可选 withVersion=true 时，有 displayName 也带上 @版本 ——
	 * 用在"必须区分版本"的地方（同一能力的 @1 和 @2 并存时）。
	 *
	 * @param {object} cap  能力对象 { name, version, displayName }
	 * @param {object} [opts]
	 * @param {boolean} [opts.withVersion] 有 displayName 时也追加 @version
	 * @param {boolean} [opts.withRaw]     有 displayName 时括号里补真名
	 */
	function capLabel(cap, opts) {
		if (!cap) return "";
		const o = opts || {};
		const raw = cap.name + (cap.version != null ? "@" + cap.version : "");
		const dn = (cap.displayName || "").trim();
		if (!dn) return raw;
		let out = dn;
		if (o.withVersion && cap.version != null) out += "@" + cap.version;
		if (o.withRaw) out += "（" + raw + "）";
		return out;
	}

	/**
	 * PUSH_UI_ENABLED 控制「手机推送」这套能力的 UI 可见性。
	 *
	 * 产品节奏：推送还没到上线时候——能力 `notify.phone`（通知消息，kind=end）
	 * 和「接收设备」页（receivers.html）先对用户隐藏。后端能力、边、历史任务**都不受影响**，
	 * 所以放行时把这里改成 true 即可，不需要改后端、不需要数据迁移。
	 *
	 * 隐藏是**在 UI 层做**（不是删能力）：一旦某个命名空间已经建了指向 notify.phone 的边，
	 * 后端照常派发；这里只是不把它列出来、也不让新的入口出现。
	 */
	const PUSH_UI_ENABLED = false;

	const HIDDEN_CAP_NAMES = new Set(["notify.phone"]);

	// filterCaps 过滤掉「暂未开放」的能力，**只在"发现/选择"的地方调用**
	// （能力页列表、各处的目标/出口选择器）。
	//
	// 为什么不做成 api() 的全局过滤：边、源、预案里可能**已经引用**了被隐藏的能力，
	// 那些页面要拿它的 paramsSchema 渲染参数表单；全局过滤会让它们渲染成"未知能力"
	// 甚至直接崩（source 详情就是这么炸的）。隐藏的是"入口"，不是"数据"。
	function filterCaps(list) {
		if (PUSH_UI_ENABLED || !Array.isArray(list)) return list;
		return list.filter((c) => !(c && HIDDEN_CAP_NAMES.has(c.name)));
	}

	/**
	 * loadErrorNode 渲染「载入失败 + 重试」，是一个 DOM 节点，调用方自己塞进合适的位置。
	 *
	 * 为什么要有它：页面**绝不**在后端失败时伪造数据（#273）。演示数据只有 make mock
	 * 那条路——cmd/fe-preview 给每个 HTML 注入 /mock.js，由它在 fetch 层应答，
	 * 页面代码不需要知道"演示数据"的存在。真实部署下把失败画成示例数据，
	 * 只会让人把「服务挂了」看成「功能如此」。
	 *
	 * err 约定：fetch 层会带 status（见 api()）；网络错误没有 status。
	 */
	function loadErrorNode(err, onRetry) {
		/* F2：调用方有时只传 {status}（例如 events.html 早年传的是 {status: r.status}）。
		 * 直接 String(err) 会把 "[object Object]" 印到页面上。这里只接受
		 * 字符串 / 带 message 的对象，其余一律当作「没有细节」交给下面的兜底文案。 */
		const msg =
			typeof err === "string"
				? err
				: err && typeof err.message === "string"
					? err.message
					: "";
		const status = (err && err.status) || 0;
		// UI 标题走 t()；上游解码后的 message 是数据，单独以纯文本叶子展示。
		const reason = status
			? status >= 500
				? t("common.server_error", "服务端错误 HTTP {code}").replace("{code}", String(status))
				: t("common.request_rejected", "请求被拒绝 HTTP {code}").replace("{code}", String(status))
			: /Failed to fetch|NetworkError|load failed/i.test(msg)
				? t("common.backend_offline", "连不上服务端")
				: t("common.request_failed", "请求失败");
		const box = document.createElement("div");
		box.className = "flex flex-col items-center gap-3 px-6 py-10 text-center";
		const p = document.createElement("p");
		p.className = "text-sm tx-meta";
		p.textContent = t("common.load_failed_prefix", "载入失败：") + reason;
		box.appendChild(p);
		if (msg) {
			const detail = document.createElement("span");
			detail.className = "text-sm tx-meta";
			detail.setAttribute("data-raw-content", "");
			detail.textContent = msg;
			box.appendChild(detail);
		}
		if (typeof onRetry === "function") {
			const btn = document.createElement("button");
			btn.type = "button";
			btn.className = "btn btn-ghost btn-sm";
			btn.textContent = t("common.retry", "重试");
			btn.addEventListener("click", onRetry);
			box.appendChild(btn);
		}
		return box;
	}

	/* ---------- 弹层：共享开关 + 退场（D6/D7） ----------
	 * 9 个页内 modal 各自维护 hidden↔flex（见 web-src/pages/*.html）。这里只补
	 * 两件共享的事，不改各页的打开逻辑与 aria：
	 *   1) 退场：加 .is-closing 播 140ms 淡出，计时到点才切 hidden（原本硬切）；
	 *   2) 重开：清掉可能残留的 .is-closing —— 140ms 窗口内重开若不清，会卡在淡出态。
	 * 幂等：重复调用 / 已隐藏 / 传空都不抛错、不重复计时。 */
	const MODAL_CLOSE_TIMERS = new WeakMap();
	const MODAL_CLOSE_FALLBACK_MS = 140;

	function openModal(backdrop) {
		if (!backdrop || !backdrop.classList) return;
		const prev = MODAL_CLOSE_TIMERS.get(backdrop);
		if (prev) {
			clearTimeout(prev);
			MODAL_CLOSE_TIMERS.delete(backdrop);
		}
		backdrop.classList.remove("is-closing");
		backdrop.classList.remove("hidden");
		backdrop.classList.add("flex");
	}

	function closeModal(backdrop) {
		if (!backdrop || !backdrop.classList) return;
		if (backdrop.classList.contains("hidden")) {
			/* 已隐藏：只清残留退场态（幂等），不排计时 */
			backdrop.classList.remove("is-closing");
			return;
		}
		if (backdrop.classList.contains("is-closing")) return; /* 重复调用不叠加 */
		backdrop.classList.add("is-closing");
		/* 时长与 CSS 同源；reduce 下该 token 是 0ms，0 必须当有效值 */
		const raw = parseFloat(
			getComputedStyle(document.documentElement).getPropertyValue(
				"--dur-modal-out",
			),
		);
		const dur = Number.isFinite(raw) ? raw : MODAL_CLOSE_FALLBACK_MS;
		MODAL_CLOSE_TIMERS.set(
			backdrop,
			setTimeout(() => {
				MODAL_CLOSE_TIMERS.delete(backdrop);
				backdrop.classList.add("hidden");
				backdrop.classList.remove("flex");
				backdrop.classList.remove("is-closing");
			}, dur),
		);
	}

	/* ---------- agent 自升级：错误码 → 人话（节点列表页 / 节点详情页共用） ----------
	 * 事实源：`.pi-orchestrator/CONTRACT-agent-upgrade.md` §5.1（控制台读侧，冻结）。
	 *
	 * 为什么收在一处：`need_root` 要给出可直接粘的命令、`bad_sha` 要说清「已拒绝」，
	 * 两个页面各写一份映射迟早漂移；文案仍然只从 i18n 取，JS 里不硬编码句子。
	 *
	 * 三种码的来源不同，必须都接：
	 *   · `node_offline` / `agent_binary_unavailable` —— POST 的 409 / 501（Anotify.api 抛出
	 *     的 err.detail 是响应里的 error 串）；
	 *   · `need_root` / `bad_sha` / `download_failed` / `not_supported` —— 节点 upgrade_ack
	 *     失败后被中枢落库到 `nodes.agent_upgrade.error`，随节点列表读回。 */
	const UPGRADE_ERROR_CODES = [
		"need_root",
		"bad_sha",
		"download_failed",
		"not_supported",
		"node_offline",
		"agent_binary_unavailable",
	];

	/* upgradeErrorCode 把「HTTP 错误对象」收敛成一个可映射的码。
	 * 先按响应里的 error 串精确命中（契约里 error 就是码本身），再按状态码兜底：
	 * 409 只可能是 node_offline、501 只可能是 agent_binary_unavailable（§5）。 */
	function upgradeErrorCode(err) {
		if (!err) return "";
		const raw = String(err.error || err.detail || err.message || "");
		for (const code of UPGRADE_ERROR_CODES) {
			if (raw.indexOf(code) >= 0) return code;
		}
		if (err.status === 409) return "node_offline";
		if (err.status === 501) return "agent_binary_unavailable";
		return "";
	}

	/* upgradeErrorText 码 → 四语言人话。未识别的码原样回显：宁可露出机器串，
	 * 也不要把「为什么失败」换成一句「操作失败」。 */
	function upgradeErrorText(code) {
		if (!code) return "";
		const key = "nodes.upgrade.err." + code;
		const text = t(key, "");
		return text && text !== key ? text : String(code);
	}

	/* upgradeAckStale：`status=sent` 超过 60s 还没变 ok/failed → 节点大概率没回话
	 * （已经断线 / 进程被 kill / ack 丢在重连窗口里）。
	 *
	 * 为什么按 `at` 算而不是按「点了多久」：中枢把 agent_upgrade 落库了
	 * （契约 §5.1），刷新后 `at` 还在 —— 判据在刷新前后必须一致，否则用户一刷新
	 * 就永远看到「升级中」。 */
	const UPGRADE_ACK_TIMEOUT_SEC = 60;
	function upgradeAckStale(rec) {
		if (!rec || rec.status !== "sent") return false;
		const at = Number(rec.at) || 0;
		if (at <= 0) return false;
		return Date.now() / 1000 - at > UPGRADE_ACK_TIMEOUT_SEC;
	}

	/* ---------- v4 信封 / 列表 / 错误工具 ----------
	 *
	 * 页面不再直接摸 `res.items` / `res.status`：一律走这里的函数。
	 * “严格”是这个层的核心：旧形状（裸数组、`{namespaces:[]}`、扁平对象）一律报
	 * 错，不静默回退 —— 回退会把“后端/URL 错了”伪装成“数据是空的”，比报错难查。 */
	function deepCopy(v) {
		if (v === undefined) return undefined;
		return JSON.parse(JSON.stringify(v));
	}
	function isObj(v) {
		return !!v && typeof v === "object" && !Array.isArray(v);
	}
	function arr(v) {
		return Array.isArray(v) ? deepCopy(v) : [];
	}
	function obj(v) {
		return isObj(v) ? deepCopy(v) : {};
	}
	function metaOf(res) {
		return isObj(res && res.metadata) ? res.metadata : {};
	}
	function specOf(res) {
		return isObj(res && res.spec) ? res.spec : {};
	}

	function malformedListError(path, why) {
		const detail = why || "列表响应格式错误";
		const err = new Error(path ? detail + "：" + path : detail);
		err.reason = "malformed_list";
		return err;
	}

	/** 严格列表：返回 `items`（非数组直接抛 `malformed_list`）。 */
	function itemsOf(res) {
		if (!res || !Array.isArray(res.items)) {
			throw malformedListError("", "响应缺少 items 数组");
		}
		return res.items;
	}

	/** 顶层 `warnings[]`（与资源四键并列）；无 → `[]`。 */
	function warningsOf(res) {
		return res && Array.isArray(res.warnings) ? res.warnings : [];
	}

	/**
	 * `apiList(path)`：GET 一个 `*List` 并严格校验信封。
	 * 返回 `{items, warnings}`；`kind` 缺失或不是 `*List`、`items` 非数组都抛错。
	 */
	async function apiList(path) {
		const res = await api(path);
		if (!res || typeof res.kind !== "string" || !/List$/.test(res.kind)) {
			throw malformedListError(path, "响应不是 *List 信封");
		}
		return { items: itemsOf(res), warnings: warningsOf(res) };
	}

	function statusOf(o) {
		return isObj(o && o.status) ? o.status : {};
	}
	function readyOf(o) {
		return statusOf(o).ready === true;
	}
	function conditionOf(o, type) {
		const list = statusOf(o).conditions;
		if (!Array.isArray(list)) return null;
		return list.find((c) => c && c.type === type) || null;
	}
	function nodesOf(o) {
		const list = statusOf(o).nodes;
		if (!Array.isArray(list)) return [];
		return list.map((n) => ({
			name: n && n.name != null ? n.name : null,
			state: n && n.state != null ? n.state : null,
		}));
	}
	/**
	 * 部署候选（status.deployCandidates）：**部署**判据的投影，与 nodesOf（已持有哪一版）是
	 * 两件事。首次部署前 nodes 恒空、这里非空 —— 部署弹窗只认这一个列表。
	 */
	function deployCandidatesOf(o) {
		const list = statusOf(o).deployCandidates;
		if (!Array.isArray(list)) return [];
		return list.map((n) => ({
			name: n && n.name != null ? n.name : null,
			online: !!(n && n.online === true),
		}));
	}
	function resolvedNext(o) {
		const resolved = statusOf(o).resolved;
		const list = isObj(resolved) && Array.isArray(resolved.next) ? resolved.next : [];
		return list.map((e) => {
			if (typeof e === "string") {
				// `name@version` 写法（契约允许裸名或 name@version）
				const at = e.lastIndexOf("@");
				return at > 0
					? { name: e.slice(0, at), version: e.slice(at + 1), missing: false }
					: { name: e, version: null, missing: false };
			}
			if (!isObj(e)) return { name: null, version: null, missing: true };
			// 明细原样保留：`capability.md §2` 定的是
			// `{name, version, digest, ready, params, missing?}`（解析不到时还有
			// `reason`）。只留 name/version/missing 会把“为什么解析不到”丢掉。
			return deepCopy(e);
		});
	}
	function observedOf(o) {
		const st = statusOf(o);
		const ob = isObj(st.observed) ? st.observed : {};
		return {
			taskId: ob.taskId == null ? null : ob.taskId,
			emits: arr(ob.emits),
			pruned: arr(ob.pruned),
			tasks: obj(ob.tasks),
		};
	}

	/**
	 * 写请求体：深拷贝并剥离服务端字段。
	 * - 总是剔 `status`（写 status → 400 `status_not_writable`）；
	 * - `{create:true}` 再剔 uid/resourceVersion/creationTimestamp（服务端生成）；
	 * - name/namespace/version/displayName/spec 原样保留。
	 */
	function writable(res, opts) {
		const create = !!(opts && opts.create);
		const body = deepCopy(isObj(res) ? res : {});
		delete body.status;
		if (isObj(body.metadata)) {
			if (create) {
				delete body.metadata.uid;
				delete body.metadata.resourceVersion;
				delete body.metadata.creationTimestamp;
			}
		}
		return body;
	}

	function resourceVersionError() {
		const err = new Error(
			t(
				"common.resource_version_required",
				"写入缺少 resourceVersion，请刷新后重试",
			),
		);
		err.reason = "resource_version_required";
		return err;
	}

	/** PATCH/PUT body 注入 `metadata.resourceVersion`（CAS 不可选）。 */
	function withResourceVersion(body, rv) {
		if (rv == null || rv === "") throw resourceVersionError();
		const out = deepCopy(isObj(body) ? body : {});
		if (!isObj(out.metadata)) out.metadata = {};
		out.metadata.resourceVersion = String(rv);
		return out;
	}

	/** DELETE 的 CAS 传输：`?resourceVersion=<encoded>`（契约 §2.8.2 假设）。 */
	function deletePath(path, rv) {
		if (rv == null || rv === "") throw resourceVersionError();
		const str = String(path);
		return (
			str +
			(str.indexOf("?") >= 0 ? "&" : "?") +
			"resourceVersion=" +
			encodeURIComponent(String(rv))
		);
	}

	function isConflict(err) {
		return !!err && err.status === 409 && err.reason === "resource_version_conflict";
	}

	/** 编辑 JSON 字段的统一入口；失败抛可显示错误（原文由页面留着，不清空输入）。 */
	function parseJSON(text, label) {
		const src = text == null ? "" : String(text);
		try {
			return JSON.parse(src);
		} catch (e) {
			const err = new Error(
				(label ? label + " " : "") + "不是合法 JSON：" + e.message,
			);
			err.reason = "invalid_json";
			err.text = src;
			err.cause = e;
			throw err;
		}
	}

	/* ---------- v4 资源模型（纯投影，不做旧字段兜底） ----------
	 * 页面只消费这里的形状；缺字段给 null / []，不抛。
	 * 注意：这些函数只读已知 v4 字段 —— 不认识的字段（例如 v3 残留）不会
	 * 被“魔术重命名”成新字段，宁可显示空也不能把旧语义偷渡进来。 */
	function modelCapability(res) {
		const meta = metaOf(res);
		const st = statusOf(res);
		return {
			name: meta.name == null ? null : meta.name,
			namespace: meta.namespace == null ? null : meta.namespace,
			version: meta.version == null ? null : meta.version,
			displayName: meta.displayName == null ? null : meta.displayName,
			resourceVersion: meta.resourceVersion == null ? null : meta.resourceVersion,
			spec: obj(specOf(res)),
			status: obj(st),
			conditions: arr(st.conditions),
			ready: st.ready === true,
			nodes: nodesOf(res),
			deployCandidates: deployCandidatesOf(res),
			resolvedNext: resolvedNext(res),
			observed: observedOf(res),
			installedFrom: st.installedFrom == null ? null : st.installedFrom,
		};
	}

	/**
	 * Source 读投影。
	 *
	 * **绝不包含 token / secret**：GET 响应本来就不回显它们，这里再用白名单
	 * 逐字段投影一次（而不是整体展开 config），防止后端误发/页面误用。
	 */
	function modelSource(res) {
		const meta = metaOf(res);
		const spec = specOf(res);
		const st = statusOf(res);
		const cfg = isObj(spec.config) ? spec.config : {};
		return {
			name: meta.name == null ? null : meta.name,
			namespace: meta.namespace == null ? null : meta.namespace,
			resourceVersion: meta.resourceVersion == null ? null : meta.resourceVersion,
			displayName: meta.displayName == null ? null : meta.displayName,
			kind: spec.kind == null ? null : spec.kind,
			enabled: spec.enabled === true,
			config: {
				hookUrl: cfg.hookUrl != null ? cfg.hookUrl : st.hookUrl != null ? st.hookUrl : null,
				secretSet: cfg.secretSet === true,
				payloadSchema: cfg.payloadSchema == null ? null : deepCopy(cfg.payloadSchema),
				sample: cfg.sample == null ? null : deepCopy(cfg.sample),
				payload: cfg.payload == null ? null : deepCopy(cfg.payload),
				expr: cfg.expr == null ? null : cfg.expr,
				tz: cfg.tz == null ? null : cfg.tz,
			},
			targets: arr(spec.targets),
			health: st.health == null ? null : st.health,
			lastFiredAt: st.lastFiredAt == null ? null : st.lastFiredAt,
			nextFireAt: st.nextFireAt == null ? null : st.nextFireAt,
			// 契约 api-conventions §7.2：boolean（所有 target 可解析才是 true）。
			// 明细在 trace/events 的 dispatched/pruned 里，不在这里再存一份数组。
			targetsResolved: !!st.targetsResolved,
			installedFrom: st.installedFrom == null ? null : st.installedFrom,
		};
	}

	/** Package YAML reader shared ONLY by Playbook normalization and its mock.
	 * JSON is YAML too. Block/flow collections, quoted scalars and literal/folded
	 * blocks cover authored packages; unsupported YAML is an explicit error, never {}.
	 * The original spec.files map is not serialized by this reader or changed.
	 */
	function parsePlaybookDocument(source) {
		if (typeof source !== "string") throw new Error("Package document must be text");
		try { return JSON.parse(source); } catch (_) { /* Read authored YAML below. */ }
		const lines = source.replace(/\r\n?/g, "\n").split("\n");
		const fail = message => { throw new Error("YAML: " + message); };
		const clean = text => {
			let quote = "";
			for (let i = 0; i < text.length; i++) {
				const char = text[i];
				if (quote) {
					if (char === "\\" && quote === '"') { i++; continue; }
					if (char === quote) {
						if (quote === "'" && text[i + 1] === "'") { i++; continue; }
						quote = "";
					}
				} else if (char === "'" || char === '"') quote = char;
				else if (char === "#" && (i === 0 || /\s/.test(text[i - 1]))) return text.slice(0, i).trimEnd();
			}
			return text.trimEnd();
		};
		const scalar = text => {
			text = clean(text).trim();
			if (!text) return null;
			if (/^[!&*]|^<<:/.test(text)) fail("tags, anchors and aliases are not supported by this package reader");
			if (text.startsWith('"')) { try { return JSON.parse(text); } catch (_) { fail("invalid double-quoted scalar"); } }
			if (text.startsWith("'")) {
				if (!text.endsWith("'")) fail("unterminated single-quoted scalar");
				return text.slice(1, -1).replaceAll("''", "'");
			}
			if (text === "true") return true;
			if (text === "false") return false;
			if (text === "null" || text === "~") return null;
			if (/^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:e[+-]?\d+)?$/i.test(text)) return Number(text);
			return text;
		};
		function flow(text) {
			let pos = 0;
			const ws = () => { while (/\s/.test(text[pos] || "") && pos < text.length) pos++; };
			function token(key = false) {
				ws(); const start = pos;
				if (text[pos] === "'" || text[pos] === '"') {
					const quote = text[pos++]; let closed = false;
					while (pos < text.length) {
						if (text[pos] === "\\" && quote === '"') { pos += 2; continue; }
						if (text[pos++] === quote) {
							if (quote === "'" && text[pos] === "'") { pos++; continue; }
							closed = true; break;
						}
					}
					if (!closed) fail("unterminated flow scalar");
				} else while (pos < text.length && !",]}".includes(text[pos]) && !(key && text[pos] === ":")) pos++;
				return scalar(text.slice(start, pos));
			}
			function value() {
				ws(); const char = text[pos];
				if (char !== "[" && char !== "{") return token();
				pos++; const end = char === "[" ? "]" : "}"; const result = char === "[" ? [] : {};
				ws(); if (text[pos] === end) { pos++; return result; }
				while (pos < text.length) {
					if (char === "[") result.push(value());
					else {
						const key = token(true); ws(); if (text[pos++] !== ":") fail("flow mapping requires colon");
						if (typeof key !== "string" || Object.prototype.hasOwnProperty.call(result, key) || ["__proto__", "constructor", "prototype"].includes(key)) fail("invalid or duplicate mapping key");
						result[key] = value();
					}
					ws(); if (pos >= text.length) fail("unterminated flow collection");
					const separator = text[pos++];
					if (separator === end) return result;
					if (separator !== ",") fail("invalid flow delimiter");
					ws(); if (text[pos] === end) { pos++; return result; }
				}
				fail("unterminated flow collection");
			}
			const result = value(); ws(); if (pos !== text.length) fail("trailing flow content"); return result;
		}
		const inline = text => /^[\[{]/.test(text.trim()) ? flow(clean(text).trim()) : scalar(text);
		const indent = line => { if (/^\s*\t/.test(line)) fail("tab indentation is not supported"); return /^ */.exec(line)[0].length; };
		const blank = line => !clean(line).trim() || ["---", "..."].includes(clean(line).trim());
		let index = 0;
		const skip = () => { while (index < lines.length && blank(lines[index])) index++; };
		const pair = text => {
			let quote = "";
			for (let i = 0; i < text.length; i++) {
				const ch = text[i];
				if (quote) { if (ch === "\\" && quote === '"') i++; else if (ch === quote) quote = ""; }
				else if (ch === "'" || ch === '"') quote = ch;
				else if (ch === ":" && (i + 1 === text.length || /\s/.test(text[i + 1]))) return [scalar(text.slice(0, i)), text.slice(i + 1).trim()];
			}
			return null;
		};
		function blockValue(text, parentIndent) {
			if (/^[|>][+-]?$/.test(text)) {
				const chunk = []; let contentIndent = null;
				while (index < lines.length) {
					const line = lines[index];
					if (line.trim() && indent(line) <= parentIndent) break;
					if (line.trim() && contentIndent == null) contentIndent = indent(line);
					if (line.trim() && indent(line) < contentIndent) fail("invalid indentation in block scalar.");
					chunk.push(line.trim() ? line.slice(contentIndent) : ""); index++;
				}
				let result = chunk.join("\n");
				if (text[0] === ">") result = result.replace(/([^\n])\n(?=[^\n])/g, "$1 ");
				if (text.endsWith("-")) return result.replace(/\n+$/, "");
				if (text.endsWith("+")) return result + "\n";
				return result.replace(/\n+$/, "") + "\n";
			}
			if (text) return inline(text);
			skip(); return index < lines.length && indent(lines[index]) > parentIndent ? block(indent(lines[index])) : null;
		}
		function assign(out, entry, level) {
			const [key, text] = entry;
			if (typeof key !== "string" || !key || Object.prototype.hasOwnProperty.call(out, key) || ["__proto__", "constructor", "prototype", "<<"].includes(key)) fail("invalid or duplicate mapping key");
			out[key] = blockValue(text, level);
		}
		function block(level) {
			skip(); const sequence = /^-(?:\s|$)/.test(lines[index].slice(level)); const out = sequence ? [] : {};
			while (index < lines.length) {
				skip(); if (index >= lines.length || indent(lines[index]) < level) break;
				if (indent(lines[index]) !== level) fail("unexpected indentation at line " + (index + 1));
				const text = clean(lines[index].slice(level)); index++;
				if (sequence) {
					if (!/^-(?:\s|$)/.test(text)) fail("mixed sequence and mapping");
					const rest = text.slice(1).trim(); const entry = pair(rest);
					if (!entry) out.push(blockValue(rest, level));
					else {
						const item = {}; assign(item, entry, level + 2);
						skip();
						if (index < lines.length && indent(lines[index]) > level) {
							const continuation = block(indent(lines[index]));
							if (Array.isArray(continuation)) fail("sequence item continuation must be a mapping");
							for (const [k, v] of Object.entries(continuation)) { if (k in item) fail("duplicate sequence mapping key"); item[k] = v; }
						}
						out.push(item);
					}
				} else {
					const entry = pair(text); if (!entry) fail("expected mapping at line " + index); assign(out, entry, level);
				}
			}
			return out;
		}
		skip(); if (index === lines.length) fail("empty document");
		const result = block(indent(lines[index])); skip(); if (index !== lines.length) fail("trailing document"); return result;
	}

	function modelPlaybook(res) {
		const meta = metaOf(res);
		const spec = specOf(res);
		const st = statusOf(res);
		const manifest = isObj(spec.files) && typeof spec.files["playbook.yaml"] === "string"
			? parsePlaybookDocument(spec.files["playbook.yaml"]) : null;
		const packageSpec = manifest && isObj(manifest.spec) ? manifest.spec : {};
		const out = {
			slug: meta.name == null ? null : meta.name,
			namespace: meta.namespace == null ? null : meta.namespace,
			version: meta.version == null ? null : meta.version,
			displayName: meta.displayName == null ? null : meta.displayName,
			resourceVersion: meta.resourceVersion == null ? null : meta.resourceVersion,
			valuesSchema: packageSpec.values == null ? null : deepCopy(packageSpec.values),
			capabilities: arr(packageSpec.capabilities),
			sources: arr(packageSpec.sources),
			status: {
				ready: st.ready === true,
				unapplied: arr(st.unapplied),
				total: st.total == null ? null : st.total,
				conditions: arr(st.conditions),
				blocker: st.blocker == null ? null : deepCopy(st.blocker),
				items: arr(st.items),
				sources: obj(st.sources),
				lastInstallId: st.lastInstallId == null ? null : st.lastInstallId,
			},
		};
		// 只有 ?includeBody=true 的响应才带 files（原始未渲染模板）
		if (isObj(spec.files)) out.files = deepCopy(spec.files);
		return out;
	}

	modelPlaybook.parseDocument = parsePlaybookDocument;

	/** 回执里 created / skipped 的统一形状：`{capabilities:[], sources:[]}`。 */
	function createdBucket(v) {
		return {
			capabilities: arr(isObj(v) ? v.capabilities : null),
			sources: arr(isObj(v) ? v.sources : null),
		};
	}

	function modelReceipt(res) {
		const r = isObj(res) ? res : {};
		const install = isObj(r.install) ? r.install : {};
		const pb = isObj(r.playbook) ? r.playbook : {};
		return {
			playbook: isObj(r.playbook)
				? { name: pb.name == null ? null : pb.name, version: pb.version == null ? null : pb.version }
				: null,
			// 顶层 source 是安装来源（inline / oci:…），不是资源的 spec.source。
			source: r.source == null ? null : r.source,
			namespace: r.namespace == null ? null : r.namespace,
			install: {
				id: install.id == null ? null : install.id,
				at: install.at == null ? null : install.at,
				dryRun: install.dryRun === true,
			},
			values: r.values == null ? null : deepCopy(r.values),
			valuesDigest: r.valuesDigest == null ? null : r.valuesDigest,
			// playbook.md §4.5：created / skipped 是**对象**（能力、源两条轴各自一组），
			// 不是数组 —— 用 arr() 强转会把它变成 []，回执 12 格就少两格。
			created: createdBucket(r.created),
			skipped: createdBucket(r.skipped),
			upgraded: arr(r.upgraded),
			unapplied: arr(r.unapplied),
			external: arr(r.external),
			pending: arr(r.pending),
			notes: arr(r.notes),
			conflicts: arr(r.conflicts),
		};
	}

	function modelInstallGroup(group) {
		const g = isObj(group) ? group : {};
		return {
			valuesDigest: g.valuesDigest == null ? null : g.valuesDigest,
			values: g.values == null ? null : deepCopy(g.values),
			firstAt: g.firstAt == null ? null : g.firstAt,
			lastAt: g.lastAt == null ? null : g.lastAt,
			attempts: typeof g.attempts === "number" ? g.attempts : 0,
			lastInstallId: g.lastInstallId == null ? null : g.lastInstallId,
			capabilities: arr(g.capabilities),
			sources: arr(g.sources),
		};
	}

	/**
	 * Task 模型。
	 * `phase` 小写成小写（六态）；`envDiagnostics` **只保留 name/source**，
	 * 不显示值（契约：环境变量值只在本机）。
	 */
	function modelTask(res) {
		const meta = metaOf(res);
		const spec = specOf(res);
		const st = statusOf(res);
		const cap = isObj(spec.capability) ? spec.capability : null;
		const result = isObj(st.result) ? st.result : {};
		const attemptPhase = (p) => (p == null ? null : String(p).toLowerCase());
		return {
			name: meta.name == null ? null : meta.name,
			namespace: meta.namespace == null ? null : meta.namespace,
			resourceVersion: meta.resourceVersion == null ? null : meta.resourceVersion,
			creationTimestamp:
				meta.creationTimestamp == null ? null : meta.creationTimestamp,
			traceUID: spec.traceUID == null ? null : spec.traceUID,
			sequence: spec.sequence == null ? null : spec.sequence,
			sourceRef: spec.sourceRef == null ? null : spec.sourceRef,
			parentRef: spec.parentRef == null ? null : spec.parentRef,
			parentTaskId: spec.parentTaskId == null ? null : spec.parentTaskId,
			capability: cap
				? {
						name: cap.name == null ? null : cap.name,
						version: cap.version == null ? null : cap.version,
						digest: cap.digest == null ? null : cap.digest,
					}
				: null,
			params: spec.params == null ? null : deepCopy(spec.params),
			phase: attemptPhase(st.phase),
			waitingReason: st.waitingReason == null ? null : st.waitingReason,
			nextAttemptAt: st.nextAttemptAt == null ? null : st.nextAttemptAt,
			nodeName: st.nodeName == null ? null : st.nodeName,
			instanceId: st.instanceId == null ? null : st.instanceId,
			attempt: st.attempt == null ? null : st.attempt,
			maxAttempts: st.maxAttempts == null ? null : st.maxAttempts,
			leaseExpiresAt: st.leaseExpiresAt == null ? null : st.leaseExpiresAt,
			startedAt: st.startedAt == null ? null : st.startedAt,
			finishedAt: st.finishedAt == null ? null : st.finishedAt,
			configRevision: st.configRevision == null ? null : st.configRevision,
			namespaceVarsRevision:
				st.namespaceVarsRevision == null ? null : st.namespaceVarsRevision,
			envDiagnostics: (Array.isArray(st.envDiagnostics) ? st.envDiagnostics : [])
				.filter((e) => isObj(e))
				.map((e) => ({
					name: e.name == null ? null : e.name,
					source: e.source == null ? null : e.source,
				})),
			result: {
				summary: result.summary == null ? null : result.summary,
				artifacts: arr(result.artifacts),
				error: isObj(result.error) ? deepCopy(result.error) : null,
			},
			attempts: (Array.isArray(st.attempts) ? st.attempts : [])
				.filter((a) => isObj(a))
				.map((a) => ({
					attempt: a.attempt == null ? null : a.attempt,
					nodeName: a.nodeName == null ? null : a.nodeName,
					instanceId: a.instanceId == null ? null : a.instanceId,
					namespaceVarsRevision: a.namespaceVarsRevision == null ? null : a.namespaceVarsRevision,
					leaseExpiresAt: a.leaseExpiresAt == null ? null : a.leaseExpiresAt,
					envDiagnostics: arr(a.envDiagnostics).filter(isObj).map(e => ({ name: e.name, source: e.source })),
					configRevision: a.configRevision == null ? null : a.configRevision,
					phase: attemptPhase(a.phase),
					startedAt: a.startedAt == null ? null : a.startedAt,
					finishedAt: a.finishedAt == null ? null : a.finishedAt,
					reason: a.reason == null ? null : a.reason,
				})),
		};
	}

	/**
	 * Node 模型（cluster-scoped）。
	 * 三层配置：machineVars（机器）/ nodeVars（节点默认）/ namespaceVars（ns 上下文）；
	 * 双 revision：configRevision（机器级）≠ appliedRevision（节点回报）。
	 * `pending` = appliedRevision !== configRevision。
	 */
	function modelNode(res) {
		const meta = metaOf(res), spec = specOf(res), st = statusOf(res);
		// v4：`?namespace=` 的读上下文在**信封顶层**（resource.yaml · Node.context），
		// 不在 status 里 —— 把「读的是哪个 ns」塞进 status，会让它和「这个 ns 的
		// 值」混在一起（resource_node.go · nodeReadEnvelope）。
		const readContext = isObj(res && res.context) ? res.context : {};
		const context = readContext.namespace ? { namespace: readContext.namespace } : null;
		const configRevision = st.configRevision ?? null, appliedRevision = st.appliedRevision ?? null;
		const localLimits = obj(st.localLimits), nodeLimits = obj(spec.limits);
		// 实例槽位（v4）：`NodeInstanceRef` 只有 `{id, task, busy}`（api/resource.yaml）。
		// `id` 是实例身份（`nins_…`），**没有** v3 的 slot / taskId / assignmentId /
		// attempt / leaseId —— 那些字段在这里一律不读，也不“等价映射”成旧名，否则
		// 页面会显示一份服务端从未说过的事实。空位身份由调用方的下标提供。
		const instances = Array.isArray(st.instances) ? st.instances.filter(isObj).map(x => {
			// busy 缺失 = 未知；显式 false 才是 idle（终态 Task 不证明物理回收）
			const busy = typeof x.busy === "boolean" ? x.busy : null;
			return {
				id: x.id == null ? null : String(x.id),
				task: x.task == null ? null : String(x.task),
				busy,
				occupancy: busy === null ? "unknown" : busy ? "occupied" : "idle",
			};
		}) : null;
		return {
			name: meta.name ?? null, resourceVersion: meta.resourceVersion ?? null,
			spec: { namespaces: arr(spec.namespaces), vars: obj(spec.vars), limits: nodeLimits, tags: arr(spec.tags) },
			status: obj(st), machineVars: obj(st.machineVars), localVars: obj(st.localVars), nodeVars: obj(st.nodeVars),
			localLimits, machineLimits: isObj(st.machineLimits) ? obj(st.machineLimits) : {
				total: localLimits.total ?? nodeLimits.total ?? null,
				perCapability: { ...obj(nodeLimits.perCapability), ...obj(localLimits.perCapability) },
			},
			configRevision, appliedRevision, appliedHash: st.appliedHash ?? null,
			pending: String(appliedRevision ?? "") !== String(configRevision ?? ""),
			overrideStale: typeof st.overrideStale === "boolean" ? st.overrideStale : null,
			instances, effectiveNamespaces: arr(st.effectiveNamespaces), context,
			namespaceVars: context ? obj(st.namespaceVars) : null,
			effectiveVars: context ? obj(st.effectiveVars) : null,
		};
	}

	/**
	 * Edge 只读投影：v4 的 wire 是 `{from, to, kind}`（api/resource.yaml ·
	 * CapabilityEdge，`kind` ∈ `capability|source`），**没有** fromIsSource /
	 * fromRef / toRef / position / port / priority —— 那些是 v3 的边记录形状，
	 * 这里一律不读，也不改名映射（否则页面会显示服务端从未给出的字段）。
	 */
	function modelEdge(item) {
		const e = isObj(item) ? item : {};
		return {
			from: e.from == null ? null : String(e.from),
			to: e.to == null ? null : String(e.to),
			kind: e.kind === "source" || e.kind === "capability" ? e.kind : null,
		};
	}

	/** Event uses the W6 resource envelope only, never the obsolete flat row. */
	function modelEvent(row) {
		const r = isObj(row) ? row : {};
		const meta = metaOf(r);
		const spec = specOf(r);
		const st = statusOf(r);
		return {
			name: meta.name == null ? null : meta.name,
			uid: meta.uid == null ? null : meta.uid,
			creationTimestamp: meta.creationTimestamp == null ? null : meta.creationTimestamp,
			namespace: meta.namespace == null ? null : meta.namespace,
			reason: spec.reason == null ? null : spec.reason,
			type: spec.type == null ? null : spec.type,
			involvedObject: isObj(spec.involvedObject) ? deepCopy(spec.involvedObject) : null,
			traceUID: spec.traceUID == null ? null : spec.traceUID,
			eventType: spec.eventType == null ? null : spec.eventType,
			deliveryId: spec.deliveryId == null ? null : spec.deliveryId,
			payload: spec.payload == null ? null : deepCopy(spec.payload),
			signature: isObj(spec.signature) ? deepCopy(spec.signature) : null,
			dispatched: arr(st.dispatched),
			// pruned 原样透传（对象 `{reason,detail}` 或 null）：不转计数、不转数组。
			pruned: st.pruned == null ? null : deepCopy(st.pruned),
			deliveredAt: st.deliveredAt == null ? null : st.deliveredAt,
		};
	}

	function modelTrace(res) {
		const r = isObj(res) ? res : {};
		return {
			traceUID: r.traceUID == null ? null : r.traceUID,
			events: arr(r.events),
			tasks: arr(r.tasks),
			pruned: arr(r.pruned),
		};
	}

	const model = {
		capability: modelCapability,
		source: modelSource,
		playbook: modelPlaybook,
		receipt: modelReceipt,
		installGroup: modelInstallGroup,
		task: modelTask,
		node: modelNode,
		edge: modelEdge,
		event: modelEvent,
		trace: modelTrace,
	};

	/* v4 执行相位 → CSS 类名后缀（**唯一一份**，ST-5）。
	 * v4 的相位叫 `cancelled`（双 l），而 tokens.css / ui.css 里的历史拼写是
	 * `canceled`（--st-canceled / .st-canceled / .b-canceled）—— 这层归一只允许
	 * 存在一个地方，否则两处漂移时状态点会静默失去颜色（不是报错，是变成默认灰）。
	 * graphs.js / traceview.js 都从这里取，不再各拄一份。 */
	const STATUS_CLASS = {
		pending: "pending",
		running: "running",
		succeeded: "succeeded",
		failed: "failed",
		cancelled: "canceled",
		abandoned: "abandoned",
	};
	/** phase → 类名后缀；未知相位原样返回（不要编一个假状态）。 */
	function statusClass(st) {
		return STATUS_CLASS[st] || String(st || "pending");
	}

	/* ---------- 导出 ---------- */
	window.Anotify = {
		el,
		loadErrorNode,
		pushUIEnabled: PUSH_UI_ENABLED,
		filterCaps,
		icon,
		capLabel,
		mountLayout,
		mountPullRefresh,
		closeSidebar,
		openModal,
		closeModal,
		theme: themeApi,
		api,
		copyText,
		toast,
		t,
		pruneReason,
		upgradeErrorCode,
		upgradeErrorText,
		upgradeAckStale,
		b64urlToBuf,
		bufToB64url,
		PLATFORM_META,
		detectPlatform,
		timeAgo,
		loadMe,
		logout,
		mountLoginLangSwitcher,
		mountLangHint,
		/* v4 canonical scope / API / 模型层（页面与 scripts/v4-frontend.test.mjs 统一用） */
		SCOPE_KEY,
		readScope,
		saveScope,
		adoptUrlNamespace,
		switchScope,
		ensureScope,
		ready,
		nsApi,
		nsList,
		loadSourceTopology,
		nsPath,
		clusterPath,
		apiList,
		itemsOf,
		warningsOf,
		statusOf,
		readyOf,
		conditionOf,
		nodesOf,
		resolvedNext,
		observedOf,
		writable,
		withResourceVersion,
		deletePath,
		isConflict,
		parseJSON,
		model,
		/* v4 相位 → CSS 类名后缀（唯一一份，graphs.js / traceview.js 共用） */
		STATUS_CLASS,
		statusClass,
	};

	/* Auto-mount login page language switcher on load. */
	mountLoginLangSwitcher();

	/* ---------- Language hint banner (progressive enhancement) ----------
	 * Shows a dismissible top banner when the visitor's browser language
	 * differs from the current page language AND the site has that language.
	 * Pure client-side: no redirect, no build-time DOM.
	 * Must run AFTER mountLayout (which prepends the layout root) so the
	 * banner lands as body's first child.
	 *
	 * 全站生效（含 login）：早期只白名单了 login/index，用户反馈“别的页面看不到提示”。
	 * 现在由“浏览器首选语言 ≠ 当前页语言”决定，不再看页名；是否骚扰由本机记忆兜着
	 * （主动选过的语言 + 关掉过的目标语言，见 LANG_HINT_KEY）。 */
	function mountLangHint() {
		/* Idempotent: don't double-insert */
		if (document.querySelector(".lang-hint")) return;

		/* 1) Build available-languages map from <link rel=alternate> (exclude x-default).
		   Keys are canonical-case (e.g. "zh-CN") for i18n key lookup and
		   hreflang/lang attributes. A lowercase→canonical map is used for
		   case-insensitive matching against navigator.languages. */
		const alternates = {}; /* canonical code → href */
		const lowerToCanon = {}; /* lowercased code → canonical code */
		const links = document.head.querySelectorAll(
			'link[rel="alternate"][hreflang]',
		);
		for (const l of links) {
			const code = l.getAttribute("hreflang") || "";
			const lower = code.toLowerCase();
			if (!lower || lower === "x-default") continue;
			alternates[code] = l.getAttribute("href") || "";
			lowerToCanon[lower] = code;
		}
		if (Object.keys(alternates).length === 0) return; /* single-lang build */

		const currentLower = (document.documentElement.lang || "").toLowerCase();

		/* 2) User language preferences (lowercased for matching) */
		const prefs = (navigator.languages || [navigator.language] || [])
			.map((p) => (p || "").toLowerCase())
			.filter(Boolean);
		if (prefs.length === 0) return;

		/* 3) Resolve the browser's FIRST supported preference (exact match
		   first, then primary-subtag prefix). That first resolvable entry is
		   the user's preferred site language: if it equals the current page
		   language, the visitor is already where they want to be — no banner.
		   Never skip a matching-current entry to suggest a lower-priority
		   language (e.g. prefs ["zh-CN","en"] on a zh-CN page must NOT
		   produce an English hint). */
		let target = null;
		for (const pref of prefs) {			let resolved = null;
			/* exact match */
			for (const lower of Object.keys(lowerToCanon)) {
				if (lower === pref) {
					resolved = lowerToCanon[lower];
					break;
				}
			}
			/* primary-subtag prefix match */
			if (!resolved) {
				const primary = pref.split("-")[0];
				for (const lower of Object.keys(lowerToCanon)) {
					if (lower.split("-")[0] === primary) {
						resolved = lowerToCanon[lower];
						break;
					}
				}
			}
			if (resolved) {
				if (resolved.toLowerCase() !== currentLower) {
					target = { code: resolved, href: alternates[resolved] };
				}
				break; /* first resolvable preference decides, for better or worse */
			}
		}
		/* 3b) 语言习惯优先：用户主动选过语言就以那次选择为准（侧栏/登录页切换器，
		 * 或点过横幅的「切换」）。浏览器首选语言只是“没选过”时的默认值。
		 * 这顺手解决一个反向骚扰：中文浏览器手动切到英文站后，不该在英文页上
		 * 又劝他切回中文。注意必须放在下面的 return 之前 —— chosen 不等于当前页时
		 * 即使浏览器首选语言就是当前页，也仍然值得提示。 */
		const hintState = readLangHintState();
		if (hintState.chosen) {
			target =
				hintState.chosen.toLowerCase() === currentLower ||
				!alternates[hintState.chosen]
					? null
					: { code: hintState.chosen, href: alternates[hintState.chosen] };
		}
		if (!target)
			return; /* 已经在用户要的语言里，或浏览器首选语言 = 当前页、站点不支持他的语言 */

		/* 3c) 本机已关掉过同一个目标语言 → 不再弹。
		 * 记的是目标语言码而不是一个 bool：换了浏览器首选语言、或页面本身换了语言，
		 * 新的提示仍然是新信息（如已知英文的人到了日语页，提示中文是有效的）。 */
		if (
			hintState.dismissed.some(
				(c) => c.toLowerCase() === target.code.toLowerCase(),
			)
		)
			return;

		/* 4) Validate i18n strings resolved (prevent bare-key banner, §6.4) */
		const textKey = "common.lang.hint.text." + target.code;
		const actionKey = "common.lang.hint.action." + target.code;
		const dismissKey = "common.lang.hint.dismiss." + target.code;
		const text = t(textKey);
		const action = t(actionKey);
		const dismiss = t(dismissKey);
		if (text === textKey || action === actionKey || dismiss === dismissKey)
			return; /* i18n not loaded (file://) — silent */

		/* 5) Build DOM (el(), no innerHTML) */
		const globe = icon(
			[
				"M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z",
				"M2 12h20",
				"M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z",
			],
			"lang-hint-icon h-4 w-4 shrink-0",
		);

		const actionHref =
			target.href + (location.search || "") + (location.hash || "");

		const actionEl = el(
			"a",
			{
				class:
					"lang-hint-action shrink-0 ml-auto rounded-full px-3 py-1 text-[13px] font-medium",
				href: actionHref,
				hreflang: target.code,
				lang: target.code,
			},
			document.createTextNode(action),
		);

		const closeBtn = el(
			"button",
			{
				class: "lang-hint-close shrink-0 rounded-md p-1.5",
				type: "button",
				"aria-label": dismiss,
			},
			icon(["M18 6L6 18", "M6 6l12 12"], "h-4 w-4"),
		);
		/* 点「切换」= 用户表了态（选了目标语言）：记进语言习惯，之后以它为准。
		 * 不只在 dismissed 里记：否则回到原语言页会又弹一次，等于没记住用户的选择。 */
		actionEl.addEventListener("click", () => rememberChosenLang(target.code));

		const inner = el(
			"div",
			{
				class:
					"lang-hint-inner mx-auto flex max-w-6xl items-center gap-3 px-5 py-2 sm:px-8",
				lang: target.code,
			},
			globe,
			el(
				"p",
				{ class: "lang-hint-text text-[13px]" },
				document.createTextNode(text),
			),
			actionEl,
			closeBtn,
		);

		const regionLabel = t("common.lang.label", "Language");
		const banner = el(
			"div",
			{
				class: "lang-hint",
				role: "region",
				"aria-label": regionLabel,
			},
			inner,
		);

		document.body.prepend(banner);

		/* 5b) F8：提示条在流内（不是 overlay），会把整页推下去。侧栏是
		 * height:100dvh 的 sticky 元素，不修正就会比视口多出一条提示条的高度，
		 * 底部用户块被裁。把「横幅还占着多少视口高度」写给 --shell-top-offset
		 * （见 ui.css .app-sidebar）。
		 *
		 * 但这个偏移量必须跟着滚动走，不能写死成横幅高度：横幅会随页面滚出视口，
		 * 一旦滚出去它就不再占位，而侧栏还挂着 45px 的 top —— 顶栏是 sticky top:0，
		 * 于是下滑之后侧栏顶部永远比顶栏低一条横幅（截图里的那条空白）。
		 * 所以写的是「横幅还有多少没滚出视口」= clamp(横幅高 − scrollY, 0, 横幅高)：
		 * top 与 height 用同一个量，侧栏底边在整段滚动里都贴着视口底（F8 不复发），
		 * 滚过横幅后 top 归 0，与顶栏重新对齐。
		 *
		 * P0-2：首帧即终态。--instant 关掉 max-height/opacity 过渡，插入的同一任务内
		 * 就加 --open 并立刻量高。旧实现用双 rAF 展开 + ResizeObserver 逐帧回写，
		 * 结果 #app-shell 在首帧之后从 y=0 逐帧下滑 44–45px（banner 段 CLS 0.03+）。
		 * 窄屏两行按实际高度量，不写死 45。 */
		banner.classList.add("lang-hint--instant", "lang-hint--open");

		let bannerH = 0;
		let appliedOffset = -1; /* 已写进 CSS 变量的值，避免同一帧重复写 */
		const applyTopOffset = () => {
			/* 关闭后（断开连接）不再写变量：否则滞后一次 resize/字体/滚动回调会把
			 * 已归零的偏移写回一个高度，侧栏又莫名短一截。 */
			if (!banner.isConnected) return;
			const raw = bannerH - window.scrollY;
			/* 留 0.01 的容差：滚到头时把浮点残渣归成真 0，否则 top 永远差个亚像素，
			 * 侧栏就不会干净地贴回视口顶。 */
			const offset = raw <= 0.01 ? 0 : Math.round(raw * 100) / 100;
			if (offset === appliedOffset) return;
			appliedOffset = offset;
			document.documentElement.style.setProperty(
				"--shell-top-offset",
				offset + "px",
			);
		};

		/* 滚动时逐帧只写一个 CSS 变量（已按 rAF 合并、值不变就不写）：
		 * 不量布局、不碰别的元素，所以不会带回 CLS 那类抖动。 */
		let scrollRaf = 0;
		const onScroll = () => {
			if (scrollRaf) return;
			scrollRaf = requestAnimationFrame(() => {
				scrollRaf = 0;
				applyTopOffset();
			});
		};

		const syncTopOffset = () => {
			if (!banner.isConnected) return;
			bannerH = banner.getBoundingClientRect().height;
			appliedOffset = -1;
			applyTopOffset();
		};
		syncTopOffset();
		/* passive：滚动是热路径，别让监听器挡住合成器滚动 */
		window.addEventListener("scroll", onScroll, { passive: true });

		/* 只有两件事会改 banner 高度：字体加载完（换行点变）/ 旋转或缩放。
		 * 不用 ResizeObserver 逐帧写 —— 那正是外壳逐帧下滑的来源。 */
		if (document.fonts && document.fonts.ready)
			document.fonts.ready.then(syncTopOffset);
		let resizeTimer = 0;
		const onResize = () => {
			clearTimeout(resizeTimer);
			resizeTimer = setTimeout(syncTopOffset, 150);
		};
		window.addEventListener("resize", onResize);

		/* 6) Close：一次性瞬时收起（不跑过渡帧）：偏移归零 + 移除同步完成。
		 * 保留焦点转移：关掉横幅后焦点不能丢在 body 上。 */
		function closeHint() {
			window.removeEventListener("resize", onResize);
			window.removeEventListener("scroll", onScroll);
			clearTimeout(resizeTimer);
			if (scrollRaf) cancelAnimationFrame(scrollRaf);
			/* 用户明确拒绝了这个目标语言 → 记在本机，以后不再弹同一个提示。
			 * 必须在 banner.remove() 前写：target 是外层的语言对象，别和下面
			 * 的 focusTarget 搞混。 */
			rememberDismissedLang(target.code);
			/* 提示条消失 = 顶部偏移归零，侧栏恢复满高 */
			document.documentElement.style.setProperty("--shell-top-offset", "0px");
			banner.remove();
			/* Move focus to first focusable in header or main */
			const focusTarget =
				document.querySelector(
					"header a, header button, main a, main button",
				) || null;
			if (focusTarget) focusTarget.focus();
		}

		closeBtn.addEventListener("click", closeHint);

		/* Esc closes when focus is within the banner */
		banner.addEventListener("keydown", (e) => {
			if (e.key === "Escape") {
				e.preventDefault();
				closeHint();
			}
		});
	}

	/* Run lang-hint on login page (no mountLayout there).
	 * Workspace pages get mountLangHint() called at the end of mountLayout(). */
	if (/login\.html$/.test(location.pathname)) {
		mountLangHint();
	}
})();
