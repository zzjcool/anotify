/* Anotify · 工作台共享布局（侧边栏 / 顶栏 / 移动抽屉）
 * 用法（各页面内联脚本）：
 *   document.getElementById("layout-root").innerHTML = AnotifyLayout.render({
 *     active: "keys",            // overview | receivers | keys | agent | docs | scheme | security
 *     title: "API Keys",
 *     subtitle: "Agent 用它上报通知",
 *     actions: '<button …>+ 新建 Key</button>',   // 顶栏右侧按钮（可选）
 *   });
 *   AnotifyLayout.mount();       // 绑定抽屉/头像等交互
 */
(function () {
	"use strict";

	const NAV = [
		{
			label: "",
			items: [
				{ id: "overview", text: "总览", href: "index.html", icon: "grid" },
				{ id: "receivers", text: "通知接收", href: "receivers.html", icon: "bell" },
				{ id: "keys", text: "API Keys", href: "keys.html", icon: "key" },
			],
		},
		{
			label: "集成",
			items: [
				{ id: "agent", text: "接入 Agent", href: "docs.html#agent", icon: "terminal" },
				{ id: "docs", text: "API 文档", href: "docs.html", icon: "code" },
			],
		},
		{
			label: "账户",
			items: [
				{ id: "security", text: "安全与登录", href: "security.html", icon: "lock" },
				{ id: "home", text: "返回首页", href: "../public/index.html", icon: "home" },
			],
		},
	];

	const ICONS = {
		grid: '<rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/>',
		bell: '<path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/>',
		key: '<path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/>',
		terminal: '<polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/>',
		code: '<polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/>',
		lock: '<rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
		home: '<path d="M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
	};

	function iconSvg(name) {
		return (
			'<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
			(ICONS[name] || "") +
			"</svg>"
		);
	}

	function renderNav(active) {
		return NAV.map(function (group) {
			const links = group.items
				.map(function (it) {
					const cls = it.id === active ? "side-link active" : "side-link";
					return (
						'<a href="' + it.href + '" class="' + cls + '">' + iconSvg(it.icon) + "<span>" + it.text + "</span></a>"
					);
				})
				.join("");
			const label = group.label ? '<div class="side-label">' + group.label + "</div>" : "";
			return label + '<div class="space-y-1">' + links + "</div>";
		}).join("");
	}

	function render(opts) {
		const active = opts.active || "";
		const actions = opts.actions || "";
		return (
			'<aside id="sidebar" class="fixed inset-y-0 left-0 z-40 w-60 shrink-0 -translate-x-full transform border-r border-white/[0.06] bg-[#08080d] transition-transform duration-300 lg:static lg:translate-x-0">' +
			'  <div class="flex h-full flex-col">' +
			'    <div class="flex h-16 items-center gap-3 border-b border-white/[0.05] px-5">' +
			'      <a href="../public/index.html" class="flex items-center gap-2.5">' +
			'        <div class="relative">' +
			'          <div class="flex h-9 w-9 items-center justify-center rounded-lg bg-black ring-1 ring-white/15">' +
			'            <span class="font-script text-lg leading-none text-white">An</span>' +
			"          </div>" +
			'          <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center">' +
			'            <span class="absolute h-3 w-3 rounded-full bg-red-500"></span>' +
			'            <span class="absolute h-3 w-3 animate-ping rounded-full bg-red-500/70"></span>' +
			"          </span>" +
			"        </div>" +
			'        <span class="font-script text-xl leading-none text-white">Anotify</span>' +
			"      </a>" +
			"    </div>" +
			'    <nav class="flex-1 overflow-y-auto px-3 py-4">' +
			renderNav(active) +
			"    </nav>" +
			'    <div class="border-t border-white/[0.05] p-3">' +
			'      <div class="flex items-center gap-3 rounded-xl px-2 py-2">' +
			'        <div class="flex h-9 w-9 items-center justify-center rounded-full bg-gradient-to-br from-indigo-500/30 to-violet-500/30 ring-1 ring-white/15 text-sm font-medium">A</div>' +
			'        <div class="min-w-0 flex-1">' +
			'          <div class="truncate text-sm text-zinc-200">admin</div>' +
			'          <div class="truncate text-[11px] text-zinc-500">私有化部署 · 主工作台</div>' +
			"        </div>" +
			'        <span class="badge-dot dot dot-success"></span>' +
			"      </div>" +
			"    </div>" +
			"  </div>" +
			"</aside>" +
			'<div id="sidebar-overlay" class="sidebar-overlay fixed inset-0 z-30 hidden lg:hidden"></div>' +
			'<div class="flex min-w-0 flex-1 flex-col">' +
			'  <header class="sticky top-0 z-20 flex h-16 items-center justify-between gap-4 border-b border-white/[0.06] bg-[#050508]/80 px-5 backdrop-blur-md sm:px-8">' +
			'    <div class="flex items-center gap-3">' +
			'      <button id="menu-btn" class="btn-ghost rounded-lg p-2 lg:hidden" aria-label="菜单">' +
			'        <svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="18" x2="21" y2="18"/></svg>' +
			"      </button>" +
			"      <div>" +
			'        <h1 class="text-base font-semibold text-white">' +
			(opts.title || "") +
			"</h1>" +
			'        <p class="text-[11px] text-zinc-500">' +
			(opts.subtitle || "") +
			"</p>" +
			"      </div>" +
			"    </div>" +
			'    <div class="flex items-center gap-3">' +
			actions +
			'      <div class="flex h-8 w-8 items-center justify-center rounded-full bg-gradient-to-br from-indigo-500/40 to-violet-500/40 ring-1 ring-white/15 text-xs font-medium">A</div>' +
			"    </div>" +
			"  </header>" +
			'  <main class="flex-1 px-5 py-8 sm:px-8">' +
			'    <div class="mx-auto max-w-6xl space-y-8" id="page-content">' +
			(opts.content || "") +
			"    </div>" +
			"  </main>" +
			'  <footer class="border-t border-white/[0.04] px-5 py-5 text-center text-[11px] text-zinc-600 sm:px-8">' +
			"    Anotify · MIT License · 私有化部署 · 数据仅存于你的服务器" +
			"  </footer>" +
			"</div>"
		);
	}

	function mount() {
		const sidebar = document.getElementById("sidebar");
		const overlay = document.getElementById("sidebar-overlay");
		const menuBtn = document.getElementById("menu-btn");
		if (menuBtn && sidebar && overlay) {
			menuBtn.addEventListener("click", function () {
				sidebar.classList.remove("-translate-x-full");
				overlay.classList.remove("hidden");
			});
			overlay.addEventListener("click", function () {
				sidebar.classList.add("-translate-x-full");
				overlay.classList.add("hidden");
			});
		}
		// 滚动渐入
		const io = new IntersectionObserver(
			function (entries) {
				entries.forEach(function (e) {
					if (e.isIntersecting) e.target.classList.add("visible");
				});
			},
			{ threshold: 0.08 },
		);
		document.querySelectorAll(".reveal").forEach(function (el) {
			io.observe(el);
		});
	}

	// DOM 构造小工具
	function el(tag, attrs, ...children) {
		attrs = attrs || {};
		const node = document.createElement(tag);
		for (const k of Object.keys(attrs)) {
			const v = attrs[k];
			if (k === "class") node.className = v;
			else if (k === "text") node.textContent = v;
			else if (k === "html") node.innerHTML = v;
			else if (k.slice(0, 2) === "on") node.addEventListener(k.slice(2).toLowerCase(), v);
			else node.setAttribute(k, v);
		}
		for (const c of children) {
			if (c == null) continue;
			node.append(c.nodeType ? c : document.createTextNode(c));
		}
		return node;
	}

	window.AnotifyLayout = { render: render, mount: mount, el: el };
})();
