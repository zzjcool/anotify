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

	/* ---------- 侧栏导航结构（IA：工作台 / 集成 / 账户） ---------- */
	const NAV = [
		{
			items: [
				{ id: "overview", label: "总览", href: "index.html", icon: "overview" },
				{
					id: "receivers",
					label: "通知接收",
					href: "receivers.html",
					icon: "receivers",
				},
				{ id: "keys", label: "API Keys", href: "keys.html", icon: "keys" },
			],
		},
		{
			label: "集成",
			items: [
				{
					id: "agent",
					label: "接入 Agent",
					href: "index.html#quickstart",
					icon: "agent",
				},
				{ id: "api", label: "API 文档", href: "docs.html", icon: "api" },
				{
					id: "scheme",
					label: "技术方案",
					href: "docs.html#scheme",
					icon: "scheme",
				},
			],
		},
		{
			label: "账户",
			items: [
				{
					id: "security",
					label: "安全与登录",
					href: "security.html",
					icon: "security",
				},
				{ id: "logout", label: "退出登录", href: "#logout", icon: "home" },
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
		const nav = el("nav", { class: "flex-1 overflow-y-auto px-3 py-4" });
		for (const group of NAV) {
			if (group.label)
				nav.append(el("div", { class: "side-label", text: group.label }));
			const box = el("div", { class: "space-y-1" });
			for (const item of group.items) {
				const a = el("a", {
					href: item.href,
					class: `side-link ${item.id === o.active ? "active" : ""}`,
				});
				a.append(icon(ICONS[item.icon]), item.label);
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
				nav,
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
								text: "私有化部署 · 主工作台",
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
				"aria-label": "菜单",
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
						"aria-label": "通知",
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
			text: "Anotify · MIT License · 私有化部署 · 数据仅存于你的服务器",
		});
		pageMain.parentNode && pageMain.parentNode.removeChild(pageMain);
		const main = el("main", { class: "flex-1 px-5 py-8 sm:px-8" });
		main.appendChild(pageMain);
		rightCol.append(header, main, footer);

		const root = el(
			"div",
			{ class: "flex min-h-screen" },
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
		});
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
					const back = encodeURIComponent(
						location.pathname.split("/").pop() || "index.html",
					);
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
			btn.textContent = "已复制 ✓";
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
		other: { icon: "🌐", label: "浏览器" },
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
		const t =
			typeof isoOrSec === "number" ? isoOrSec * 1000 : Date.parse(isoOrSec);
		if (Number.isNaN(t)) return "—";
		const diff = Date.now() - t;
		if (diff < 60e3) return "刚刚";
		if (diff < 3600e3) return Math.floor(diff / 60e3) + " 分钟前";
		if (diff < 86400e3) return Math.floor(diff / 3600e3) + " 小时前";
		return Math.floor(diff / 86400e3) + " 天前";
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
		b64urlToBuf,
		bufToB64url,
		PLATFORM_META,
		detectPlatform,
		timeAgo,
		loadMe,
		logout,
	};
})();
