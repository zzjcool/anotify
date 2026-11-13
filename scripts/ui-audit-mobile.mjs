#!/usr/bin/env node
/**
 * ui-audit-mobile.mjs — 手机端机械审计（#383 iOS 聚焦缩放 / #382 触控热区 / #384 横滚 / #385 首屏）
 *
 * 纯 node + 仓库自带的 playwright-core，不装新依赖；用系统 Chromium 无头跑。
 * 量五类事实 + 二类"缺席即失败"守卫，任一声明的断言失败即 `exit 1`：
 *
 *   0. js-error      页面 pageerror / console error —— 页面 JS 挂了就没资格算绿
 *   1. input-font    所有可见 input/select/textarea 的计算字号（移动档须 ≥16px，否则 iOS 聚焦自动放大）
 *   2. touch-targets 每个可点元素的**有效命中矩形**（clip-aware）。先与所有 overflow≠visible
 *                    祖先的矩形及视口求交，再用 document.elementsFromPoint 沿四条边/中线逐点
 *                    探测：被 .flograph{overflow:hidden} 之类裁掉的部分不算命中，低于阈值判红。
 *                    输出同时给「盒尺寸」与「有效命中尺寸」两列。
 *   3. overflow      每个可滚容器的 scrollWidth/clientWidth，以及页面级 documentElement.scrollWidth
 *   4. lost          右溢出且没有任何可滚祖先的元素（= 真的看不到，0 才算过关）
 *   5. coverage      覆盖下限：每个视口实际量到的「可见输入数 / 可点目标数」低于下限即失败。
 *                    页面 JS 挂了、数据没渲染、选择器漂了时 minFont 会是 "-"，一律判失败，
 *                    不再"没量到就跳过"。
 *
 * 就绪判据（不再用固定 sleep 当信号）：goto 之后跑 waitForFunction 哨兵 ——
 * document.readyState=complete + webfont 全部 loaded（字体迟到会改度量）+ 所有 ≤2s 的
 * 短定时器（含 mock 的 120–300ms 延迟）归零 + DOM 连续 2 帧无变更；超时（30s）按失败记 —— 页面有长动画时哨兵会等它跑完（index/
 * source/trace 的图动画约 8s），这正是"等到真的稳定"而不是"睡够 600ms"。目标站是 mock 时还会顺手 MockApi.latency(0)
 * （只去掉人为延迟、不改数据），让等待由"数据已渲染"而不是"睡够 600ms"决定。
 *
 * 用法（每个 flag 都有同名大写环境变量给默认值）：
 *
 *   BASE=http://127.0.0.1:7784 OUT=/tmp/audit node scripts/ui-audit-mobile.mjs
 *   node scripts/ui-audit-mobile.mjs --base http://127.0.0.1:7784 --viewports 390x844,360x640
 *   node scripts/ui-audit-mobile.mjs --checks input-font,overflow   # 只跑指定检查
 *   node scripts/ui-audit-mobile.mjs --pages index,keys            # 只跑几页
 *   node scripts/ui-audit-mobile.mjs --min-font 17                 # 临时调严阈值（自证可失败）
 *   node scripts/ui-audit-mobile.mjs --list-pages
 *   node scripts/ui-audit-mobile.mjs --help
 *
 * 参数（括号内是环境变量名）：
 *   --base URL            被测站点根地址（BASE，默认 http://127.0.0.1:7784）
 *   --out DIR             JSON + 汇总落盘目录（OUT，默认 <repo>/.pi-orchestrator/out/ui-audit-mobile）
 *   --viewports LIST      `390x844,360x640`（VIEWPORTS，默认 `390x844,360x640`）
 *   --checks LIST         input-font,touch-targets,overflow,lost（CHECKS，默认全部）
 *   --pages LIST          只跑指定页 slug（PAGES）
 *   --min-font N          输入类最小字号（MIN_FONT，默认 16）
 *   --min-target N        可点元素最小**有效命中**边长（MIN_TARGET，默认 44）
 *   --min-inputs N        每个视口的可见输入覆盖下限（MIN_INPUTS，默认 18；0 = 关闭该守卫）
 *                         —— 19 页 × 390 档实测 20 个**首屏可见**输入（#376 把 playbooks 的
 *                         导入表单收进弹层后少了 3 个；#383 的 21 个含弹层内输入）。
 *                         低于 18 就说明有页面没渲染/选择器漂了；minFont="-" 必红。
 *   --min-targets N       每个视口的可点目标覆盖下限（MIN_TARGETS，默认 200；0 = 关闭该守卫）
 *                         —— 19 页 × 390 档实测 230；低于 200 说明整页/整段控件没渲染
 *   --theme dark|light    主题（THEME，默认 dark）
 *   --shots               顺带把每页截图存到 <out>/shots/
 *   --no-probe            跳过命中探测（只量视觉盒，快；**不是** clip-aware，不作为 #382 证据）
 *   --all-targets         所有可点元素都参与判红（ALL_TARGETS=1；默认只判 #382 列举的控件族）
 *   --json-only           不打印人类可读汇总
 *   --list-pages          打印页面清单后退出
 *   --help / -h           本说明
 *
 * 退出码：0 = 全部断言通过；1 = 有断言失败；2 = 运行环境问题（站点不可达、缺 playwright 等）。
 *
 * 输出（schema 稳定，供 #382/#384/#385 复用；本段与实现同步维护）：
 *   {
 *     "generatedAt": ISO, "base": "...", "theme": "dark",
 *     "minFont": 16, "minTarget": 44, "minInputs": 18, "minTargets": 200,
 *     "viewports": [{ "id": "390x844", "width": 390, "height": 844 }],
 *     "pages": [{
 *        "slug": "nodes", "path": "/nodes.html", "viewport": "390x844", "readyMs": 1734,
 *        "pageScrollWidth": 390, "viewportWidth": 390,
 *        "inputs":   [{ "sel": "#result-ttl", "fontSize": 16, "ok": true }],
 *        "targets":  [{ "sel": "button.btn.btn-sm", "scope": "btn-sm",
 *                       "w": 98, "h": 31, "clipW": 98, "clipH": 31,
 *                       "hitW": 98, "hitH": 31, "probed": true, "ok": false }],
 *        "scrollables": [{ "sel": ".table-wrap", "cw": 308, "sw": 620, "over": 312 }],
 *        "lost":     [{ "sel": "table.keys", "right": 932 }],
 *        "meta":     { "viewport": "width=device-width, initial-scale=1.0", "zoomBlocked": false },
 *        "errors":   [],
 *        "error":    "（仅加载失败时出现）"
 *     }],
 *     "summary": {
 *        "pages": 38, "viewports": ["390x844", "360x640"], "checks": ["input-font", ...],
 *        "minFont": 16, "minTarget": 44, "minInputs": 18, "minTargets": 200,
 *        "coverage": { "390x844": { "pages": 19, "inputs": 20, "targets": 230, "minFont": 16 } },
 *        "byCheck": { "js-error": 0, "input-font": 0, "touch-targets": 0, "overflow": 0,
 *                     "lost": 0, "viewport-meta": 0, "coverage": 0, "load": 0 },
 *        "failures": [{ "check": "touch-targets", "slug": "trace", "viewport": "390x844", "detail": "..." }]
 *     },
 *     "ok": false
 *   }
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// 就绪哨兵的常数：只跟踪 ≤2s 的短定时器；DOM 静默 150ms 后再连续 2 帧无变更才算稳定。
const TIMER_TRACK_MAX_MS = 2000;
const QUIET_MS = 150;
const READY_TIMEOUT_MS = 30000;

// ---------------------------------------------------------------- 页面清单
// 19 页 = 生产站点的全部页面；参数页用固定 mock 参数（scripts/mock.sh 的假数据）。
export const PAGES = [
	{ slug: "index", path: "/index.html", anon: false },
	{ slug: "events", path: "/events.html", anon: false },
	{ slug: "sources", path: "/sources.html", anon: false },
	{ slug: "source", path: "/source.html?namespace=default&name=deploy-hook", anon: false },
	{ slug: "capabilities", path: "/capabilities.html", anon: false },
	{ slug: "playbooks", path: "/playbooks.html", anon: false },
	{ slug: "playbook", path: "/playbook.html?namespace=default&id=nightly", anon: false },
	{ slug: "nodes", path: "/nodes.html", anon: false },
	{ slug: "node", path: "/node.html?namespace=default&id=anode-1", anon: false },
	{ slug: "keys", path: "/keys.html", anon: false },
	{ slug: "receivers", path: "/receivers.html", anon: false },
	{ slug: "security", path: "/security.html", anon: false },
	{ slug: "admin", path: "/admin.html", anon: false },
	{ slug: "docs", path: "/docs.html", anon: false },
	{ slug: "trace", path: "/trace.html?traceUID=trace_mock_t1", anon: false },
	{ slug: "message", path: "/message.html?id=tsk_mock_1", anon: false },
	{ slug: "login", path: "/login.html", anon: true },
	{ slug: "cli-auth", path: "/cli-auth.html", anon: true },
	{ slug: "passkey-enroll", path: "/passkey-enroll.html", anon: true },
];

const ALL_CHECKS = ["input-font", "touch-targets", "overflow", "lost"];

// 默认覆盖下限：19 页实测规模（2026-11-13，epic/369-ui-review + #383）留一点余量。
// 定成"实测值"而不是 0 —— 守卫的意义就是抓"整页没渲染 / 一个输入都没量到"的假绿。
const DEFAULT_MIN_INPUTS = 18;
const DEFAULT_MIN_TARGETS = 200;

// ---------------------------------------------------------------- 参数
function parseArgs(argv) {
	const a = {
		base: process.env.BASE || "http://127.0.0.1:7784",
		out: process.env.OUT || path.join(ROOT, ".pi-orchestrator/out/ui-audit-mobile"),
		viewports: process.env.VIEWPORTS || "390x844,360x640",
		checks: process.env.CHECKS || ALL_CHECKS.join(","),
		pages: process.env.PAGES || "",
		minFont: Number(process.env.MIN_FONT || 16),
		minTarget: Number(process.env.MIN_TARGET || 44),
		minInputs: Number(process.env.MIN_INPUTS ?? DEFAULT_MIN_INPUTS),
		minTargets: Number(process.env.MIN_TARGETS ?? DEFAULT_MIN_TARGETS),
		theme: process.env.THEME || "dark",
		shots: false,
		probe: true,
		allTargets: process.env.ALL_TARGETS === "1",
		jsonOnly: false,
		help: false,
		listPages: false,
	};
	for (let i = 2; i < argv.length; i++) {
		const k = argv[i];
		const v = () => argv[++i];
		switch (k) {
			case "--base": a.base = v(); break;
			case "--out": a.out = v(); break;
			case "--viewports": a.viewports = v(); break;
			case "--checks": a.checks = v(); break;
			case "--pages": a.pages = v(); break;
			case "--min-font": a.minFont = Number(v()); break;
			case "--min-target": a.minTarget = Number(v()); break;
			case "--min-inputs": a.minInputs = Number(v()); break;
			case "--min-targets": a.minTargets = Number(v()); break;
			case "--theme": a.theme = v(); break;
			case "--shots": a.shots = true; break;
			case "--no-probe": a.probe = false; break;
			case "--all-targets": a.allTargets = true; break;
			case "--json-only": a.jsonOnly = true; break;
			case "--list-pages": a.listPages = true; break;
			case "--help": case "-h": a.help = true; break;
			default:
				console.error(`未知参数：${k}（--help 看用法）`);
				process.exit(2);
		}
	}
	a.base = a.base.replace(/\/+$/, "");
	for (const key of ["minFont", "minTarget", "minInputs", "minTargets"]) {
		if (!Number.isFinite(a[key]) || a[key] < 0) {
			console.error(`--${key.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase())} 需要 ≥0 的数字，收到：${a[key]}`);
			process.exit(2);
		}
	}
	a.checks = a.checks.split(",").map((s) => s.trim()).filter(Boolean);
	for (const c of a.checks) {
		if (!ALL_CHECKS.includes(c)) {
			console.error(`未知检查项：${c}（可选：${ALL_CHECKS.join(", ")}）`);
			process.exit(2);
		}
	}
	a.viewports = a.viewports.split(",").map((s) => {
		const [w, h] = s.trim().split("x").map(Number);
		if (!w || !h) {
			console.error(`视口格式应为 WxH，收到：${s}`);
			process.exit(2);
		}
		return { id: `${w}x${h}`, width: w, height: h };
	});
	if (a.pages) {
		const want = new Set(a.pages.split(",").map((s) => s.trim()));
		for (const p of PAGES) if (!want.has(p.slug)) want.delete(p.slug);
		a.pages = PAGES.filter((p) => want.has(p.slug));
	} else {
		a.pages = PAGES;
	}
	return a;
}

const HELP = fs
	.readFileSync(fileURLToPath(import.meta.url), "utf8")
	.split("\n")
	// 文件头注释（/** ... */）就是 --help 正文，保证文档只有一份
	.slice(1)
	.filter((l) => l.startsWith(" *") || l.startsWith(" */"))
	.map((l) => l.replace(/^\s?\*+\s?/, "").replace(/\s*\*\/$/, ""))
	.join("\n");

// ---------------------------------------------------------------- 就绪哨兵（注入到页面最早）
// 在页面自己的脚本（尤其 web/mock.js，它会覆写 window.fetch）之前装好：
//   - 包装 setTimeout/clearTimeout，跟踪"还在跑的 ≤2s 短定时器"（mock 的 120–300ms
//     延迟、页面渲染链上的一次性定时器都在内）；
//   - MutationObserver 记 DOM 变更计数与最后一次活动时间。
// 就绪 = 没有短定时器在跑 + 距最后一次 DOM 活动 ≥ QUIET_MS + 连续 2 帧计数不变。
// 这样"数据还没揭示"就绝不可能被当成"页面已就绪"，也不需要任何固定 sleep。
function installAuditHooks() {
	const st = (window.__audit = { timers: 0, mutations: 0, lastActivity: performance.now() });
	const live = new Set();
	const oSet = window.setTimeout;
	const oClear = window.clearTimeout;
	window.setTimeout = function (fn, delay, ...rest) {
		const d = Number(delay) || 0;
		// 长定时器（自动刷新、倒计时等）与"数据已渲染"无关，不参与就绪判定
		if (d > 2000 || typeof fn !== "function") return oSet.call(window, fn, delay, ...rest);
		let id = 0;
		id = oSet.call(window, function () {
			live.delete(id);
			st.timers = live.size;
			st.lastActivity = performance.now();
			return fn.apply(this, arguments);
		}, delay, ...rest);
		live.add(id);
		st.timers = live.size;
		st.lastActivity = performance.now();
		return id;
	};
	window.clearTimeout = function (id) {
		if (live.delete(id)) st.timers = live.size;
		return oClear.call(window, id);
	};
	try {
		new MutationObserver(() => {
			st.mutations++;
			st.lastActivity = performance.now();
		}).observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
	} catch {}
}

// 在页面里轮询到"稳定"为止；由 page.waitForFunction 调用，返回 true 才算就绪。
function settleProbe(opts) {
	const st = window.__audit;
	const deadline = performance.now() + opts.timeoutMs;
	const frame = () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
	return (async () => {
		while (performance.now() < deadline) {
			const idle = !st || (st.timers === 0 && performance.now() - st.lastActivity >= opts.quietMs);
			// 字体也会改度量（webfont 迟到 → 文本宽度重排），所以算就绪的一部分
			const fontsReady = !document.fonts || document.fonts.status === "loaded";
			if (document.readyState === "complete" && idle && fontsReady) {
				const m = st ? st.mutations : 0;
				await frame();
				if ((!st || st.timers === 0) && (!st || st.mutations === m)) return true;
			} else {
				await frame();
			}
		}
		return false;
	})();
}

// ---------------------------------------------------------------- 页面内测量脚本
// 在浏览器里跑，返回纯数据。所有探针都用 1px 步进，最多向外 64px。
function measureInPage(opts) {
	const { minFont, minTarget, probe, probeAll } = opts;
	const vw = innerWidth;
	const vh = innerHeight;

	const sel = (el) => {
		const tag = el.tagName.toLowerCase();
		const id = el.id ? `#${el.id}` : "";
		const cls =
			typeof el.className === "string" && el.className.trim()
				? "." + el.className.trim().split(/\s+/).slice(0, 3).join(".")
				: "";
		// 表格/列表里的同类元素很多，带上父级索引便于定位
		let nth = "";
		const p = el.parentElement;
		if (!id && p) {
			const i = [...p.children].indexOf(el);
			if (i > 0) nth = `:nth-child(${i + 1})`;
		}
		return (tag + id + cls + nth).slice(0, 96);
	};
	const visible = (el) => {
		const cs = getComputedStyle(el);
		if (cs.display === "none" || cs.visibility === "hidden" || cs.opacity === "0") return false;
		const r = el.getBoundingClientRect();
		return r.width > 0 && r.height > 0;
	};
	const rnd = (n) => Math.round(n * 10) / 10;

	// ---- 1) 输入类字号
	const inputs = [];
	for (const el of document.querySelectorAll("input, select, textarea")) {
		if (el.type === "hidden" || !visible(el)) continue;
		const fs = parseFloat(getComputedStyle(el).fontSize);
		inputs.push({ sel: sel(el), fontSize: rnd(fs), ok: fs >= minFont - 0.01 });
	}

	// ---- 2) 可点元素
	// 审计集合：真正的"控件"。正文里的行内文字链（display:inline 且无按钮外观）按
	// WCAG 2.5.5 的豁免不算，否则任何一句话里的链接都会报红，噪声压过信号。
	const isControl = (el) => {
		const tag = el.tagName.toLowerCase();
		if (tag === "button" || tag === "select" || tag === "textarea") return true;
		if (tag === "input") return !["hidden", "file"].includes(el.type);
		if (el.getAttribute("role") === "button") return true;
		if (el.hasAttribute("onclick")) return true;
		if (tag === "a" && el.getAttribute("href") && el.getAttribute("href") !== "#") {
			const cs = getComputedStyle(el);
			return (
				el.classList.contains("btn") ||
				el.classList.contains("tab") ||
				el.classList.contains("copy-btn") ||
				cs.display !== "inline"
			);
		}
		return false;
	};

	// #382 的断言范围 = issue 明确列举的控件族；其余可点元素（侧栏导航 .side-link、
	// 基础 .btn 36px、页头 .app-icon-btn 等）只统计不判红 —— 它们不在本车道
	// 的文件所有权内（见 CONTRACT §3/§4），硬判红等于把别人的活记成自己的债。
	const scopeOf = (el) => {
		const cl = el.classList;
		if (cl.contains("btn-sm")) return "btn-sm";
		if (cl.contains("copy-btn")) return "copy-btn";
		if (cl.contains("tab")) return "tab";
		if (cl.contains("flograph-legend-btn")) return "flograph-legend-btn";
		if (el.closest(".flograph-ctrl")) return "flograph-ctrl";
		if (el.tagName === "BUTTON" && !String(el.className || "").trim()) return "unclassed-button";
		return "";
	};

	// 命中链里是否有它（或它的子树）：elementsFromPoint 会按绘制顺序返回该点上的
	// 所有元素，被 overflow:hidden 裁掉 / 被别的层压住时，el 根本不在链上。
	const hitChainHas = (el, x, y) => {
		if (x < 0 || y < 0 || x >= vw || y >= vh) return false;
		const chain = document.elementsFromPoint(x, y);
		for (const h of chain) if (h === el || el.contains(h)) return true;
		return false;
	};

	// 元素实际被裁剪后的盒子：与所有 overflow≠visible 的祖先矩形、以及视口求交。
	// 这是"有效命中"的上界 —— 被 .flograph{overflow:hidden} 裁掉的 4px 不可能是命中区。
	const clipOf = (el, r) => {
		let l = Math.max(0, r.left), t = Math.max(0, r.top);
		let ri = Math.min(vw, r.right), b = Math.min(vh, r.bottom);
		for (let p = el.parentElement; p && p !== document.documentElement; p = p.parentElement) {
			const cs = getComputedStyle(p);
			const clipsX = cs.overflowX !== "visible";
			const clipsY = cs.overflowY !== "visible";
			if (!clipsX && !clipsY) continue;
			const pr = p.getBoundingClientRect();
			if (clipsX) { l = Math.max(l, pr.left); ri = Math.min(ri, pr.right); }
			if (clipsY) { t = Math.max(t, pr.top); b = Math.min(b, pr.bottom); }
		}
		return { left: l, top: t, right: ri, bottom: b, width: Math.max(0, ri - l), height: Math.max(0, b - t) };
	};

	// 有效命中矩形：从"裁剪盒"出发，先向内收到真能命中的边（防遮挡），
	// 再向外扩到命中链仍有它的最远处（::after 等伪元素热区）。
	// 与旧实现的关键差别：base 不是视觉矩形而是裁剪盒本身，所以被父级裁掉的部分
	// 不会白算成命中区（旧版 44×44 的按钮实际只有 44×40，就是漏在这里）。
	const LIMIT = 64;
	const effectiveHit = (el, r) => {
		const c = clipOf(el, r);
		if (c.width <= 0 || c.height <= 0) return { hitW: 0, hitH: 0, clipW: rnd(c.width), clipH: rnd(c.height) };
		let l = Math.round(c.left), ri = Math.round(c.right) - 1;
		let t = Math.round(c.top), b = Math.round(c.bottom) - 1;
		let midX = Math.round((c.left + c.right) / 2);
		let midY = Math.round((c.top + c.bottom) / 2);
		// 纵向：先收到能命中的边，再向外扩
		let g = 0;
		while (t < b && g++ < LIMIT && !hitChainHas(el, midX, t)) t++;
		g = 0;
		while (b > t && g++ < LIMIT && !hitChainHas(el, midX, b)) b--;
		g = 0;
		while (g++ < LIMIT && t - 1 >= 0 && hitChainHas(el, midX, t - 1)) t--;
		g = 0;
		while (g++ < LIMIT && b + 1 < vh && hitChainHas(el, midX, b + 1)) b++;
		midY = Math.round((t + b) / 2);
		// 横向：用新的纵向中线，同样先收后扩
		g = 0;
		while (l < ri && g++ < LIMIT && !hitChainHas(el, l, midY)) l++;
		g = 0;
		while (ri > l && g++ < LIMIT && !hitChainHas(el, ri, midY)) ri--;
		g = 0;
		while (g++ < LIMIT && l - 1 >= 0 && hitChainHas(el, l - 1, midY)) l--;
		g = 0;
		while (g++ < LIMIT && ri + 1 < vw && hitChainHas(el, ri + 1, midY)) ri++;
		return {
			hitW: Math.max(0, ri - l + 1),
			hitH: Math.max(0, b - t + 1),
			clipW: rnd(c.width),
			clipH: rnd(c.height),
		};
	};

	// 视口外的元素探不到：先滚到中间再探（视觉尺寸不随滚动变化，只更新命中值）。
	// 用 scrollIntoView 而不是 window.scrollTo：有些页面（docs 的折叠块）真正的
	// 滚动上下文不在 window 上，scrollIntoView 会自己找对祖先。
	const bringIntoView = (el) => {
		const r = el.getBoundingClientRect();
		if (r.top >= 0 && r.bottom <= vh) return r;
		el.scrollIntoView({ block: "center", inline: "nearest", behavior: "instant" });
		return el.getBoundingClientRect();
	};

	// 用户真的能看到/点到它吗？折叠的 <details>、被 overflow:hidden 完全裁掉的内容，
	// getBoundingClientRect 照样给坐标，但 elementFromPoint 命中的是别人。
	// 这类元素不参与断言（它们本来就不在屏幕上）。
	const userVisible = (el, r) => {
		const xs = [r.left + r.width / 2, r.left + r.width / 4, r.left + (r.width * 3) / 4];
		const ys = [r.top + r.height / 2, r.top + r.height / 4, r.top + (r.height * 3) / 4];
		for (const y of ys) for (const x of xs) if (hitChainHas(el, Math.round(x), Math.round(y))) return true;
		return false;
	};

	const targets = [];
	const controls = [
		...document.querySelectorAll("button, select, textarea, input, a[href], [role='button'], [onclick]"),
	].filter((el) => visible(el) && isControl(el));
	for (const el of controls) {
		const r0 = el.getBoundingClientRect();
		// 完全在视口左右之外（收起的抽屉）不算"可点元素"
		if (r0.right <= 0 || r0.left >= vw) continue;
		const scope = scopeOf(el);
		// 无论探不探命中都先滚进视图：保证 --no-probe 与默认模式量到的是同一批目标
		const r = bringIntoView(el);
		if (!userVisible(el, r)) continue;
		const box = { w: rnd(r.width), h: rnd(r.height) };
		let hitW = box.w;
		let hitH = box.h;
		let clipW = box.w;
		let clipH = box.h;
		let probed = false;
		// 默认只探测 #382 的控件族（性能与范围纪律）；--all-targets 时全探。
		if (probe && (probeAll || scope)) {
			const s = effectiveHit(el, r);
			hitW = rnd(s.hitW);
			hitH = rnd(s.hitH);
			clipW = s.clipW;
			clipH = s.clipH;
			probed = true;
		}
		targets.push({
			sel: sel(el),
			scope,
			w: box.w,
			h: box.h,
			clipW,
			clipH,
			hitW,
			hitH,
			probed,
			ok: hitW >= minTarget - 0.01 && hitH >= minTarget - 0.01,
		});
	}

	// ---- 3) 可滚容器
	const scrollables = [];
	for (const el of document.querySelectorAll("body *")) {
		if (!visible(el)) continue;
		const cs = getComputedStyle(el);
		if (cs.overflowX === "visible") continue;
		const over = el.scrollWidth - el.clientWidth;
		if (over > 1) {
			scrollables.push({ sel: sel(el), cw: el.clientWidth, sw: el.scrollWidth, over });
		}
	}

	// ---- 4) 真·丢失（右溢出且没有可滚祖先）
	const scrollerOf = (el) => {
		for (let p = el.parentElement; p && p !== document.documentElement; p = p.parentElement) {
			const ox = getComputedStyle(p).overflowX;
			if (ox === "auto" || ox === "scroll") return p;
		}
		return null;
	};
	const lost = [];
	for (const el of document.querySelectorAll("body *")) {
		if (!visible(el)) continue;
		const r = el.getBoundingClientRect();
		if (r.right <= vw + 1) continue;
		if (scrollerOf(el)) continue;
		// 自身就是可滚容器时不算丢失（内容可滚到）
		const cs = getComputedStyle(el);
		if (cs.overflowX === "auto" || cs.overflowX === "scroll") continue;
		lost.push({ sel: sel(el), right: Math.round(r.right) });
	}

	// ---- 5) viewport meta（iOS 缩放守卫：不得出现 maximum-scale / user-scalable=no）
	const mv = document.querySelector('meta[name="viewport"]');
	const metaContent = mv ? mv.getAttribute("content") || "" : "";
	const zoomBlocked = /maximum-scale\s*=\s*1\b|user-scalable\s*=\s*(no|0)\b/i.test(metaContent);

	return {
		pageScrollWidth: document.documentElement.scrollWidth,
		viewportWidth: vw,
		inputs,
		targets,
		scrollables,
		lost,
		meta: { viewport: metaContent, zoomBlocked },
	};
}

// ---------------------------------------------------------------- 依赖定位
// playwright-core 是仓库 devDependency（没有就 `npm i`）。git worktree 里通常没有
// node_modules，而同一个仓库的主 checkout 一般装过 —— 依次尝试，找到即用。
async function loadPlaywright() {
	const candidates = ["playwright-core"];
	const common = path.resolve(ROOT);
	candidates.push(path.join(common, "node_modules/playwright-core/index.mjs"));
	try {
		const { execFileSync } = await import("node:child_process");
		const gitCommon = execFileSync("git", ["rev-parse", "--git-common-dir"], { cwd: ROOT, encoding: "utf8" }).trim();
		// worktree 的 --git-common-dir 指向主仓库的 .git，其父目录就是主 checkout
		const mainRepo = path.dirname(path.resolve(ROOT, gitCommon));
		if (mainRepo !== common) candidates.push(path.join(mainRepo, "node_modules/playwright-core/index.mjs"));
	} catch {}
	const errs = [];
	for (const c of candidates) {
		try {
			const mod = await import(c);
			if (mod.chromium) return mod;
		} catch (e) {
			errs.push(`${c}: ${e.message}`);
		}
	}
	console.error("✗ 找不到 playwright-core。在仓库根跑 `npm i` 后再试。已尝试：");
	for (const e of errs) console.error(`  - ${e}`);
	return null;
}

// ---------------------------------------------------------------- 主流程
async function main() {
	const a = parseArgs(process.argv);
	if (a.help) {
		console.log(HELP);
		return 0;
	}
	if (a.listPages) {
		for (const p of PAGES) console.log(`${p.slug}\t${p.path}`);
		return 0;
	}

	const pw = await loadPlaywright();
	if (!pw) return 2;
	const { chromium } = pw;

	// 站点可达性：先 ping 一次，别让 19×2 页全部超时才报错
	const probeUrl = `${a.base}${PAGES[0].path}`;
	try {
		const res = await fetch(probeUrl, { signal: AbortSignal.timeout(5000) });
		if (!res.ok) throw new Error(`HTTP ${res.status}`);
	} catch (e) {
		console.error(`✗ 站点不可达：${probeUrl}（${e.message}）`);
		console.error(`  先起 mock：NO_TUNNEL=1 MOCK_PORT=<port> ./scripts/mock.sh`);
		return 2;
	}

	fs.mkdirSync(a.out, { recursive: true });
	if (a.shots) fs.mkdirSync(path.join(a.out, "shots"), { recursive: true });

	const browser = await chromium.launch({
		executablePath: "/usr/bin/chromium",
		args: ["--no-sandbox", "--disable-dev-shm-usage"],
	});
	const results = [];

	try {
		for (const vp of a.viewports) {
			for (const page of a.pages) {
				const ctx = await browser.newContext({
					viewport: { width: vp.width, height: vp.height },
					locale: "zh-CN",
					isMobile: vp.width < 500,
					hasTouch: vp.width < 500,
					deviceScaleFactor: 2,
				});
				await ctx.addInitScript(installAuditHooks);
				await ctx.addInitScript(
					({ theme, anon }) => {
						try {
							localStorage.setItem("anotify-theme", theme);
							if (anon) sessionStorage.setItem("anotify.mock.loggedOut", "1");
						} catch {}
					},
					{ theme: a.theme, anon: page.anon },
				);
				const p = await ctx.newPage();
				const errors = [];
				p.on("pageerror", (e) => errors.push(`pageerror: ${String(e).slice(0, 160)}`));
				p.on("console", (m) => {
					if (m.type() === "error") errors.push(`console: ${m.text().slice(0, 160)}`);
				});
				const row = { slug: page.slug, path: page.path, viewport: vp.id, errors };
				try {
					const t0 = Date.now();
					await p.goto(`${a.base}${page.path}`, { waitUntil: "domcontentloaded", timeout: 30000 });
					// mock 站：把人为延迟置 0（只去噪、不改数据）；真站点没有 MockApi，no-op
					await p.evaluate(() => {
						try {
							if (window.MockApi && typeof window.MockApi.latency === "function") window.MockApi.latency(0);
						} catch {}
					});
					// 确定性就绪哨兵：短定时器归零 + DOM 2 帧稳定，替代旧的固定 sleep 600
					await p.waitForFunction(settleProbe, { quietMs: QUIET_MS, timeoutMs: READY_TIMEOUT_MS }, { timeout: READY_TIMEOUT_MS + 5000, polling: "raf" });
					row.readyMs = Date.now() - t0;
					// 关掉动画，避免过渡期测量
					await p.addStyleTag({
						content: "*,*::before,*::after{animation-duration:0s!important;transition:none!important}",
					});
					await p.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
					Object.assign(row, await p.evaluate(measureInPage, {
						minFont: a.minFont,
						minTarget: a.minTarget,
						probe: a.probe,
						probeAll: a.allTargets,
					}));
					if (a.shots) {
						await p.screenshot({ path: path.join(a.out, "shots", `${page.slug}-${vp.id}.png`), fullPage: false });
					}
				} catch (e) {
					row.error = String(e.message || e).slice(0, 300);
				}
				await ctx.close();
				results.push(row);
				process.stderr.write(`  ${vp.id}\t${page.slug}${row.error ? " ✗" : ""}\n`);
			}
		}
	} finally {
		await browser.close();
	}

	// ---------------------------------------------------------------- 断言
	const failures = [];
	// 覆盖统计（按视口）：量到多少输入/目标，以及最小字号
	const coverage = {};
	for (const vp of a.viewports) coverage[vp.id] = { pages: 0, inputs: 0, targets: 0, minFont: null };
	for (const r of results) {
		const c = coverage[r.viewport];
		if (!c) continue;
		c.pages++;
		if (r.inputs) {
			c.inputs += r.inputs.length;
			for (const i of r.inputs) if (c.minFont === null || i.fontSize < c.minFont) c.minFont = i.fontSize;
		}
		if (r.targets) c.targets += r.targets.length;
	}
	for (const r of results) {
		if (r.error) {
			failures.push({ check: "load", slug: r.slug, viewport: r.viewport, detail: r.error });
			continue;
		}
		// 页面 JS 挂了 = 失败。旧版只把错误写进 JSON 的 errors 数组，照样 exit 0（假绿）。
		for (const e of r.errors || []) {
			failures.push({ check: "js-error", slug: r.slug, viewport: r.viewport, detail: e });
		}
		if (a.checks.includes("input-font")) {
			for (const i of r.inputs || []) {
				if (!i.ok) failures.push({ check: "input-font", slug: r.slug, viewport: r.viewport, detail: `${i.sel} = ${i.fontSize}px < ${a.minFont}px` });
			}
		}
		if (a.checks.includes("touch-targets")) {
			for (const t of r.targets || []) {
				if (!t.ok && (a.allTargets || t.scope))
					failures.push({
						check: "touch-targets",
						slug: r.slug,
						viewport: r.viewport,
						detail: `${t.sel} 盒 ${t.w}×${t.h} / 有效命中 ${t.hitW}×${t.hitH} < ${a.minTarget}×${a.minTarget}${t.probed ? "" : "（未探测，用视觉盒）"}`,
					});
			}
		}
		if (a.checks.includes("overflow")) {
			if (r.pageScrollWidth > r.viewportWidth + 1) {
				failures.push({
					check: "overflow",
					slug: r.slug,
					viewport: r.viewport,
					detail: `documentElement.scrollWidth ${r.pageScrollWidth} > viewport ${r.viewportWidth}`,
				});
			}
		}
		if (a.checks.includes("lost")) {
			for (const l of r.lost || []) {
				failures.push({ check: "lost", slug: r.slug, viewport: r.viewport, detail: `${l.sel} right=${l.right} 且无滚祖先` });
			}
		}
		// viewport meta 守卫（任何检查组合都跑：这是 #383 的硬约束）
		if (r.meta && r.meta.zoomBlocked) {
			failures.push({ check: "viewport-meta", slug: r.slug, viewport: r.viewport, detail: `禁用了用户缩放：${r.meta.viewport}` });
		}
	}

	// 覆盖下限：不量到东西就不许算绿（缺席即失败）。
	for (const vp of a.viewports) {
		const c = coverage[vp.id];
		if (a.checks.includes("input-font") && a.minInputs > 0 && c.inputs < a.minInputs) {
			failures.push({
				check: "coverage",
				slug: "-",
				viewport: vp.id,
				detail: `本视口只量到 ${c.inputs} 个可见输入 < 下限 ${a.minInputs}（minFont=${c.minFont === null ? "-" : c.minFont}）`,
			});
		}
		if (a.checks.includes("touch-targets") && a.minTargets > 0 && c.targets < a.minTargets) {
			failures.push({
				check: "coverage",
				slug: "-",
				viewport: vp.id,
				detail: `本视口只量到 ${c.targets} 个可点目标 < 下限 ${a.minTargets}`,
			});
		}
	}

	const summary = {
		pages: results.length,
		viewports: a.viewports.map((v) => v.id),
		checks: a.checks,
		minFont: a.minFont,
		minTarget: a.minTarget,
		minInputs: a.minInputs,
		minTargets: a.minTargets,
		coverage,
		byCheck: Object.fromEntries(
			[...ALL_CHECKS, "viewport-meta", "js-error", "coverage", "load"].map((c) => [c, failures.filter((f) => f.check === c).length]),
		),
		failures,
	};
	const out = {
		generatedAt: new Date().toISOString(),
		base: a.base,
		theme: a.theme,
		minFont: a.minFont,
		minTarget: a.minTarget,
		minInputs: a.minInputs,
		minTargets: a.minTargets,
		viewports: a.viewports,
		pages: results,
		summary,
		ok: failures.length === 0,
	};

	const jsonPath = path.join(a.out, "ui-audit-mobile.json");
	fs.writeFileSync(jsonPath, JSON.stringify(out, null, 2));

	if (!a.jsonOnly) {
		// 人类可读汇总：每页一行
		const pad = (s, n) => String(s).padEnd(n);
		console.log("");
		console.log(
			`${pad("view", 9)}${pad("page", 16)}${pad("in", 5)}${pad("minFs", 7)}${pad("tgt", 5)}${pad("box<", 6)}${pad("hit<", 6)}${pad("worst effective", 32)}${pad("scrollW", 12)}${pad("lost", 5)}jsErr`,
		);
		for (const r of results) {
			if (r.error) {
				console.log(`${pad(r.viewport, 9)}${pad(r.slug, 16)}LOAD ERROR  ${r.error}`);
				continue;
			}
			const badIn = (r.inputs || []).filter((i) => !i.ok);
			const minFont = Math.min(...(r.inputs || []).map((i) => i.fontSize), Infinity);
			// 判红只算 #382 列举的控件族（scope 非空）；其余计入 +N，仅统计
			const gated = (r.targets || []).filter((t) => a.allTargets || t.scope);
			const boxBad = gated.filter((t) => t.w < a.minTarget - 0.01 || t.h < a.minTarget - 0.01).length;
			const badT = gated.filter((t) => !t.ok);
			const worst = badT.slice().sort((x, y) => Math.min(x.hitW, x.hitH) - Math.min(y.hitW, y.hitH))[0];
			console.log(
				pad(r.viewport, 9) +
					pad(r.slug, 16) +
					pad((r.inputs || []).length, 5) +
					pad(Number.isFinite(minFont) ? minFont : "-", 7) +
					pad((r.targets || []).length, 5) +
					pad(boxBad, 6) +
					pad(badT.length, 6) +
					pad(worst ? `${worst.w}×${worst.h}→${worst.hitW}×${worst.hitH} ${worst.sel}`.slice(0, 31) : "-", 32) +
					pad(`${r.pageScrollWidth}/${r.viewportWidth}`, 12) +
					pad((r.lost || []).length, 5) +
					(r.errors ? r.errors.length : 0),
			);
		}
		console.log("");
		console.log(`覆盖下限（每个视口）：输入 ≥ ${a.minInputs}，可点目标 ≥ ${a.minTargets}（0 表示关闭该守卫）`);
		for (const vp of a.viewports) {
			const c = coverage[vp.id];
			console.log(
				`  ${pad(vp.id, 9)}pages ${c.pages}  输入 ${c.inputs}  目标 ${c.targets}  minFont ${c.minFont === null ? "-" : c.minFont}`,
			);
		}
		console.log("");
		if (failures.length) {
			console.log(`✗ 断言失败 ${failures.length} 条：`);
			for (const f of failures.slice(0, 60)) console.log(`  [${f.check}] ${f.viewport} ${f.slug}: ${f.detail}`);
			if (failures.length > 60) console.log(`  …还有 ${failures.length - 60} 条，见 ${jsonPath}`);
		} else {
			console.log("✓ 全部断言通过");
		}
		console.log(`\nJSON: ${jsonPath}`);
	}

	return failures.length ? 1 : 0;
}

main()
	.then((code) => process.exit(code))
	.catch((e) => {
		console.error(e);
		process.exit(2);
	});
