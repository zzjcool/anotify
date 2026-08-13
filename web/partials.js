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

	const ICONS = {
		overview: [
			"M3 3h7v9H3z",
			"M14 3h7v5h-7z",
			"M14 12h7v9h-7z",
			"M3 16h7v5H3z",
		],
		receivers: [
			"M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9",
			"M13.73 21a2 2 0 0 1-3.46 0",
		],
		keys: [
			"M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4",
		],
		agent: ["M4 17l6-6-6-6", "M12 19h8"],
		api: ["M16 18l6-6-6-6", "M8 6l-6 6 6 6"],
		scheme: [
			"M4 19.5A2.5 2.5 0 0 1 6.5 17H20",
			"M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z",
		],
		security: ["M3 11h18v11H3z", "M7 11V7a5 5 0 0 1 10 0v4"],
		home: ["M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
	};

	/* ---------- 侧栏导航结构（IA：工作台 / 集成 / 账户） ----------
	 * labelKey / groupLabelKey 指向 i18n key；渲染时用 t() 取值。
	 * 无 i18n.js 时 fallback 到中文原文，保持向后兼容。 */
	const NAV = [
		{
			items: [
				{
					id: "overview",
					labelKey: "common.nav.overview",
					fallback: "总览",
					href: "index.html",
					icon: "overview",
				},
				{
					id: "receivers",
					labelKey: "common.nav.receivers",
					fallback: "通知接收",
					href: "receivers.html",
					icon: "receivers",
				},
				{
					id: "keys",
					labelKey: "common.nav.keys",
					fallback: "API Keys",
					href: "keys.html",
					icon: "keys",
				},
			],
		},
		{
			groupLabelKey: "common.nav.integration",
			groupFallback: "集成",
			items: [
				{
					id: "agent",
					labelKey: "common.nav.agent",
					fallback: "接入 Agent",
					href: "connect.html",
					icon: "agent",
				},
				{
					id: "api",
					labelKey: "common.nav.api",
					fallback: "API 文档",
					href: "docs.html",
					icon: "api",
				},
				{
					id: "scheme",
					labelKey: "common.nav.scheme",
					fallback: "技术方案",
					href: "docs.html#scheme",
					icon: "scheme",
				},
			],
		},
		{
			groupLabelKey: "common.nav.account",
			groupFallback: "账户",
			items: [
				{
					id: "security",
					labelKey: "common.nav.security",
					fallback: "安全与登录",
					href: "security.html",
					icon: "security",
				},
				{
					id: "logout",
					labelKey: "common.nav.logout",
					fallback: "退出登录",
					href: "#logout",
					icon: "home",
				},
			],
		},
	];

	function logo(size) {
		const s = size || "h-9 w-9";
		const wrap = el("div", { class: "relative" });
		wrap.append(
			el(
				"div",
				{
					class: `flex ${s} items-center justify-center rounded-lg bg-black ring-1 ring-white/15`,
				},
				el("span", {
					class: "font-script text-lg leading-none text-white",
					text: "An",
				}),
			),
			el(
				"span",
				{
					class:
						"absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center",
				},
				el("span", { class: "absolute h-3 w-3 rounded-full bg-red-500" }),
				el("span", {
					class: "absolute h-3 w-3 animate-ping rounded-full bg-red-500/70",
				}),
			),
		);
		return wrap;
	}

	function closeSidebar() {
		const sb = document.getElementById("sidebar");
		const ov = document.getElementById("sidebar-overlay");
		if (sb) sb.classList.add("-translate-x-full");
		if (ov) ov.classList.add("hidden");
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
	function mountLayout(opts) {
		const o = Object.assign(
			{ active: "", title: "", subtitle: "", username: "zheng" },
			opts || {},
		);
		const pageMain = document.getElementById("page-main");
		if (!pageMain) {
			console.error('[partials] 需要 <div id="page-main"> 包裹主内容');
			return;
		}

		/* ----- 侧栏 ----- */
		const adminNav = el("div", {
			id: "admin-nav-section",
			class: "hidden px-3 pt-4",
		});
		const nav = el("nav", { class: "flex-1 overflow-y-auto px-3 py-4" });
		for (const group of NAV) {
			if (group.groupLabelKey)
				nav.append(
					el("div", {
						class: "side-label",
						text: t(group.groupLabelKey, group.groupFallback),
					}),
				);
			const box = el("div", { class: "space-y-1" });
			for (const item of group.items) {
				const a = el("a", {
					href: item.href,
					class: `side-link ${item.id === o.active ? "active" : ""}`,
				});
				a.append(icon(ICONS[item.icon]), t(item.labelKey, item.fallback));
				box.append(a);
			}
			nav.append(box);
		}

		const sidebar = el(
			"aside",
			{
				id: "sidebar",
				class:
					"fixed inset-y-0 left-0 z-40 w-60 shrink-0 -translate-x-full transform border-r border-white/[0.06] transition-transform duration-300 lg:static lg:translate-x-0",
				style: "background: var(--bg-raise)",
			},
			el(
				"div",
				{ class: "flex h-full flex-col" },
				el(
					"div",
					{
						class:
							"flex h-16 items-center gap-3 border-b border-white/[0.05] px-5",
					},
					(() => {
						const a = el("a", {
							href: "index.html",
							class: "flex items-center gap-2.5",
						});
						a.append(
							logo(),
							el("span", {
								class: "font-script text-xl leading-none text-white",
								text: "Anotify",
							}),
						);
						return a;
					})(),
				),
				adminNav,
				nav,
				el(
					"div",
					{ class: "border-t border-white/[0.05] px-3 pt-3" },
					buildLangSwitcher(),
				),
				el(
					"div",
					{ class: "border-t border-white/[0.05] p-3" },
					el(
						"div",
						{ class: "flex items-center gap-3 rounded-xl px-2 py-2" },
						el("div", {
							class:
								"flex h-9 w-9 items-center justify-center rounded-full bg-gradient-to-br from-indigo-500/30 to-violet-500/30 ring-1 ring-white/15 text-sm font-medium",
							id: "sidebar-avatar",
							text: (o.username[0] || "…").toUpperCase(),
						}),
						el(
							"div",
							{ class: "min-w-0 flex-1" },
							el("div", {
								class: "truncate text-sm text-zinc-200",
								id: "sidebar-username",
								text: o.username || "…",
							}),
							el("div", {
								class: "truncate text-[11px] text-zinc-500",
								text: t("common.sidebar.deployment", "私有化部署 · 主工作台"),
							}),
						),
						el("span", { class: "badge-dot dot dot-success" }),
					),
				),
			),
		);

		const overlay = el("div", {
			id: "sidebar-overlay",
			class: "sidebar-overlay fixed inset-0 z-30 hidden lg:hidden",
			onclick: closeSidebar,
		});

		/* ----- 顶栏 ----- */
		const menuBtn = el(
			"button",
			{
				id: "menu-btn",
				class: "btn-ghost rounded-lg p-2 lg:hidden",
				"aria-label": t("common.nav.menu", "菜单"),
				onclick: () => {
					sidebar.classList.remove("-translate-x-full");
					overlay.classList.remove("hidden");
				},
			},
			icon(["M3 6h18", "M3 12h18", "M3 18h18"], "h-5 w-5"),
		);
		const header = el(
			"header",
			{
				class:
					"sticky top-0 z-20 flex h-16 items-center justify-between gap-4 border-b border-white/[0.06] px-5 backdrop-blur-md sm:px-8",
				style: "background: color-mix(in srgb, var(--bg) 80%, transparent)",
			},
			el(
				"div",
				{ class: "flex items-center gap-3" },
				menuBtn,
				el(
					"div",
					{},
					el("h1", {
						class: "text-base font-semibold text-white",
						text: o.title,
					}),
					el("p", { class: "text-[11px] text-zinc-500", text: o.subtitle }),
				),
			),
			el(
				"div",
				{ class: "flex items-center gap-3" },
				el(
					"button",
					{
						class:
							"relative rounded-lg p-2 text-zinc-400 hover:bg-white/5 hover:text-white transition-colors",
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
						class: "absolute right-1.5 top-1.5 h-2 w-2 rounded-full bg-red-500",
					}),
				),
				el("div", {
					class:
						"flex h-8 w-8 items-center justify-center rounded-full bg-gradient-to-br from-indigo-500/40 to-violet-500/40 ring-1 ring-white/15 text-xs font-medium",
					id: "topbar-avatar",
					text: (o.username[0] || "…").toUpperCase(),
				}),
			),
		);

		/* ----- 重组 DOM ----- */
		const rightCol = el("div", { class: "flex min-w-0 flex-1 flex-col" });
		const footer = el("footer", {
			class:
				"border-t border-white/[0.04] px-5 py-5 text-center text-[11px] text-zinc-600 sm:px-8",
			text: t(
				"common.footer.copyright",
				"Anotify · MIT License · 私有化部署 · 数据仅存于你的服务器",
			),
		});
		pageMain.parentNode && pageMain.parentNode.removeChild(pageMain);
		const main = el("main", { class: "flex-1 px-5 py-8 sm:px-8" });
		main.appendChild(pageMain);
		rightCol.append(header, main, footer);

		const root = el(
			"div",
			{ class: "flex min-h-dvh" },
			sidebar,
			overlay,
			rightCol,
		);
		document.body.prepend(root);

		/* 滚动渐入 */
		const io = new IntersectionObserver(
			(entries) =>
				entries.forEach(
					(e) => e.isIntersecting && e.target.classList.add("visible"),
				),
			{ threshold: 0.08 },
		);
		document.querySelectorAll(".reveal").forEach((n) => io.observe(n));

		/* 退出登录：拦截 NAV 里的 logout 链接 */
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

			/* 超管：在侧栏注入「管理后台」入口 */
			if (me.role === "admin") {
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
					const a = el("a", {
						href: "admin.html",
						class: `side-link ${o.active === "admin" ? "active" : ""}`,
					});
					a.append(
						icon(["M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"]),
						t("common.nav.admin", "管理后台"),
					);
					section.append(a);
				}
			}
		});

		/* Language hint banner: must run after body.prepend(root) above
		 * so the banner lands as body's first child (above the layout root). */
		mountLangHint();
	}

	/* ---------- API 封装：失败时回退到 demo ---------- */
	// 需登录的页面（工作台各页）；login.html 等公开页不设此标记
	const isAuthedPage = !/login\.html$/.test(location.pathname);

	async function api(path, opts, demo) {
		const o = opts || {};
		try {
			const res = await fetch(
				path,
				Object.assign({ credentials: "include" }, o),
			);
			// 401 = 未登录/会话过期：跳登录页（不是“后端未连接”，不进入演示模式）
			if (res.status === 401) {
				if (isAuthedPage) {
					// 保留查询串（如推送深链 ?msg=<id>），登录后仍能跳到目标消息。
					// 页名走白名单、查询串只允许安全字符，防 open redirect。
					const PAGES = [
						"index.html",
						"message.html",
						"receivers.html",
						"keys.html",
						"security.html",
						"admin.html",
						"docs.html",
						"cli-auth.html",
						"passkey-enroll.html",
						"connect.html",
					];
					const name = location.pathname.split("/").pop();
					const page = PAGES.includes(name) ? name : "index.html";
					const qs = /^[?][\w\-=&%.]*$/.test(location.search)
						? location.search
						: "";
					const back = encodeURIComponent(page + qs);
					location.href = "login.html?next=" + back;
				}
				throw new Error("HTTP 401");
			}
			if (!res.ok) throw new Error("HTTP " + res.status);
			return await res.json();
		} catch (e) {
			if (demo && typeof demo.then === "function") return demo;
			if (demo !== undefined) return demo;
			throw e;
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

	function toast(msg, kind) {
		let host = document.getElementById("anotify-toast-host");
		if (!host) {
			host = el("div", {
				id: "anotify-toast-host",
				class: "fixed bottom-5 right-5 z-[80] space-y-2",
			});
			document.body.append(host);
		}
		const color =
			kind === "error"
				? "var(--error)"
				: kind === "warn"
					? "var(--warn)"
					: "var(--success)";
		const t = el(
			"div",
			{
				class:
					"card flex items-center gap-2.5 px-4 py-3 text-sm text-zinc-200 shadow-2xl",
				style: "background: var(--bg-raise)",
			},
			el("span", { class: "dot", style: `background: ${color}` }),
			msg,
		);
		host.append(t);
		setTimeout(() => t.remove(), 3600);
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

	/* ---------- 导出 ---------- */
	window.Anotify = {
		el,
		icon,
		mountLayout,
		closeSidebar,
		api,
		copyText,
		toast,
		t,
		b64urlToBuf,
		bufToB64url,
		PLATFORM_META,
		detectPlatform,
		timeAgo,
		loadMe,
		logout,
		mountLoginLangSwitcher,
		mountLangHint,
	};

	/* Auto-mount login page language switcher on load. */
	mountLoginLangSwitcher();

	/* ---------- Language hint banner (progressive enhancement) ----------
	 * Shows a dismissible top banner when the visitor's browser language
	 * differs from the current page language AND the site has that language.
	 * Pure client-side: no redirect, no storage, no build-time DOM.
	 * Must run AFTER mountLayout (which prepends the layout root) so the
	 * banner lands as body's first child. */
	function mountLangHint() {
		/* Only on index/login pages */
		const page = document.body.getAttribute("data-page") || "";
		if (page !== "index.html" && page !== "login.html") return;

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
		for (const pref of prefs) {
			let resolved = null;
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
		if (!target)
			return; /* preferred language is current page, or none supported */

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
		const reduced =
			window.matchMedia &&
			window.matchMedia("(prefers-reduced-motion: reduce)").matches;

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

		/* 6) Animate open (reduced-motion = instant) */
		if (reduced) {
			banner.classList.add("lang-hint--open");
		} else {
			requestAnimationFrame(() =>
				requestAnimationFrame(() => banner.classList.add("lang-hint--open")),
			);
		}

		/* 7) Close: animate out then remove; move focus to first focusable in header/main */
		function closeHint() {
			if (reduced) {
				banner.remove();
			} else {
				banner.classList.remove("lang-hint--open");
				const cleanup = () => banner.remove();
				banner.addEventListener("transitionend", cleanup, { once: true });
				setTimeout(cleanup, 250); /* fallback */
			}
			/* Move focus to first focusable in header or main */
			const target =
				document.querySelector(
					"header a, header button, main a, main button",
				) || null;
			if (target) target.focus();
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
