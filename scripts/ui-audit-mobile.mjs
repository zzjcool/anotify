#!/usr/bin/env node
/**
 * ui-audit-mobile.mjs — 手机端机械审计（#383 iOS 聚焦缩放 / #382 触控热区 / #384 横滚 / #385 首屏）
 *
 * 纯 node + 仓库自带的 playwright-core，不装新依赖；用系统 Chromium 无头跑。
 * 量四类事实，任一声明的断言失败即 `exit 1`：
 *
 *   1. input-font    所有可见 input/select/textarea 的计算字号（移动档须 ≥16px，否则 iOS 聚焦自动放大）
 *   2. touch-targets 每个可点元素的**命中矩形**（含 ::after 等伪元素扩展，用 elementFromPoint 实测）
 *   3. overflow      每个可滚容器的 scrollWidth/clientWidth，以及页面级 documentElement.scrollWidth
 *   4. lost          右溢出且没有任何可滚祖先的元素（= 真的看不到，0 才算过关）
 *
 * 用法（全部可用同名环境变量给默认值）：
 *
 *   BASE=http://127.0.0.1:7784 OUT=/tmp/audit node scripts/ui-audit-mobile.mjs
 *   node scripts/ui-audit-mobile.mjs --base http://127.0.0.1:7784 --viewports 390x844,360x640
 *   node scripts/ui-audit-mobile.mjs --checks touch-targets     # 只跑一项
 *   node scripts/ui-audit-mobile.mjs --pages index,keys         # 只跑几页
 *   node scripts/ui-audit-mobile.mjs --list-pages
 *   node scripts/ui-audit-mobile.mjs --help
 *
 * 参数：
 *   --base URL            被测站点根地址（env BASE，默认 http://127.0.0.1:7784）
 *   --out DIR             JSON + 汇总落盘目录（env OUT，默认 <repo>/.pi-orchestrator/out/ui-audit-mobile）
 *   --viewports LIST      `390x844,360x640`（env VIEWPORTS，默认 `390x844,360x640`）
 *   --checks LIST         input-font,touch-targets,overflow,lost（env CHECKS，默认全部）
 *   --pages LIST          只跑指定页 slug（env PAGES）
 *   --min-font N          输入类最小字号（默认 16）
 *   --min-target N        可点元素最小命中边长（默认 44）
 *   --theme dark|light    主题（默认 dark）
 *   --shots               顺带把每页截图存到 <out>/shots/
 *   --no-probe            跳过 elementFromPoint 命中探测（只量视觉矩形，快 5 倍，用于粗筛）
 *   --json-only           不打印人类可读汇总
 *   --help / -h           本说明
 *
 * 退出码：0 = 全部断言通过；1 = 有断言失败；2 = 运行环境问题（站点不可达等）。
 *
 * 输出（schema 稳定，供 #384/#385 复用）：
 *   {
 *     "generatedAt": ISO,
 *     "base": "...", "minFont": 16, "minTarget": 44,
 *     "viewports": [{ "id": "390x844", "width": 390, "height": 844 }],
 *     "pages": [{
 *        "slug": "index", "path": "/index.html", "viewport": "390x844",
 *        "pageScrollWidth": 390, "viewportWidth": 390,
 *        "inputs":   [{ "sel": "#ev-limit", "fontSize": 12.8, "ok": false }],
 *        "targets":  [{ "sel": "button.btn.btn-sm", "w": 60, "h": 30, "hitW": 60, "hitH": 44, "ok": false }],
 *        "scrollables": [{ "sel": ".table-wrap", "cw": 308, "sw": 620 }],
 *        "lost":     [{ "sel": "table.keys", "right": 932 }],
 *        "meta":     { "viewport": "width=device-width, initial-scale=1.0", "zoomBlocked": false },
 *        "errors":   []
 *     }],
 *     "summary": { "pages": 38, "checks": 4, "failures": [ ... ] },
 *     "ok": true
 *   }
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

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

// ---------------------------------------------------------------- 页面内测量脚本
// 在浏览器里跑，返回纯数据。所有探针都用 1px 步进，最多向外 64px，
// 命中判定 = elementFromPoint 落在自身或子树内（即"手指点在这一点真的能触发它"）。
function measureInPage(opts) {
	const { minFont, minTarget, probe } = opts;
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

	// 命中探测：从视觉矩形四条边各向外 1px 步进，找 elementFromPoint 仍命中自身
	// （或自身子树）的最远边界。垂直带确定后，横向探测取新带的中心线。
	const LIMIT = 64;
	const hitSpan = (el, r) => {
		const own = (x, y) => {
			if (x < 0 || y < 0 || x >= vw || y >= vh) return false;
			const hit = document.elementFromPoint(x, y);
			return !!hit && (hit === el || el.contains(hit));
		};
		const cx = Math.min(vw - 1, Math.max(0, Math.round(r.left + r.width / 2)));
		const x0 = Math.min(vw - 1, Math.max(0, Math.round(r.left)));
		const x1 = Math.min(vw - 1, Math.max(0, Math.round(r.right) - 1));
		const y0 = Math.min(vh - 1, Math.max(0, Math.round(r.top)));
		const y1 = Math.min(vh - 1, Math.max(0, Math.round(r.bottom) - 1));
		let up = 0;
		while (up < LIMIT && y0 - up - 1 >= 0 && own(cx, y0 - up - 1)) up++;
		let down = 0;
		while (down < LIMIT && y1 + down + 1 < vh && own(cx, y1 + down + 1)) down++;
		const my = Math.min(vh - 1, Math.max(0, Math.round((y0 - up + y1 + down) / 2)));
		let left = 0;
		while (left < LIMIT && x0 - left - 1 >= 0 && own(x0 - left - 1, my)) left++;
		let right = 0;
		while (right < LIMIT && x1 + right + 1 < vw && own(x1 + right + 1, my)) right++;
		return { hitW: r.width + left + right, hitH: r.height + up + down };
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

	// 用户真的能看到/点到它吗？折叠的 <details>、被 overflow:hidden 裁掉的内容，
	// getBoundingClientRect 照样给坐标，但 elementFromPoint 命中的是别人。
	// 这类元素不参与断言（它们本来就不在屏幕上）。
	const hitIsSelf = (el, x, y) => {
		if (x < 0 || y < 0 || x >= vw || y >= vh) return false;
		const h = document.elementFromPoint(x, y);
		return !!h && (h === el || el.contains(h));
	};
	const userVisible = (el, r) => {
		const xs = [r.left + r.width / 2, r.left + r.width / 4, r.left + (r.width * 3) / 4];
		const ys = [r.top + r.height / 2, r.top + r.height / 4, r.top + (r.height * 3) / 4];
		for (const y of ys) for (const x of xs) if (hitIsSelf(el, Math.round(x), Math.round(y))) return true;
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
		const r = probe ? bringIntoView(el) : r0;
		const onScreen = userVisible(el, r);
		if (!onScreen) continue;
		let hitW = r.width;
		let hitH = r.height;
		let probed = false;
		if (probe && scope) {
			const s = hitSpan(el, r);
			hitW = s.hitW;
			hitH = s.hitH;
			probed = true;
		}
		targets.push({
			sel: sel(el),
			scope,
			w: rnd(r.width),
			h: rnd(r.height),
			hitW: rnd(hitW),
			hitH: rnd(hitH),
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
					await p.goto(`${a.base}${page.path}`, { waitUntil: "networkidle", timeout: 30000 });
					await p.waitForTimeout(600);
					// 关掉动画，避免过渡期测量
					await p.addStyleTag({
						content: "*,*::before,*::after{animation-duration:0s!important;transition:none!important}",
					});
					Object.assign(row, await p.evaluate(measureInPage, { minFont: a.minFont, minTarget: a.minTarget, probe: a.probe }));
					if (a.shots) {
						await p.screenshot({ path: path.join(a.out, "shots", `${page.slug}-${vp.id}.png`), fullPage: false });
					}
				} catch (e) {
					row.error = String(e.message || e).slice(0, 300);
					errors.push(row.error);
				}
				await ctx.close();
				results.push(row);
				process.stderr.write(`  ${vp.id}\t${page.slug}\n`);
			}
		}
	} finally {
		await browser.close();
	}

	// ---------------------------------------------------------------- 断言
	const failures = [];
	for (const r of results) {
		if (r.error) {
			failures.push({ check: "load", slug: r.slug, viewport: r.viewport, detail: r.error });
			continue;
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
						detail: `${t.sel} hit ${t.hitW}×${t.hitH} < ${a.minTarget}×${a.minTarget}${t.probed ? "" : "（未探测，用视觉矩形）"}`,
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

	const summary = {
		pages: results.length,
		viewports: a.viewports.map((v) => v.id),
		checks: a.checks,
		minFont: a.minFont,
		minTarget: a.minTarget,
		byCheck: Object.fromEntries(
			[...ALL_CHECKS, "viewport-meta", "load"].map((c) => [c, failures.filter((f) => f.check === c).length]),
		),
		failures,
	};
	const out = {
		generatedAt: new Date().toISOString(),
		base: a.base,
		theme: a.theme,
		minFont: a.minFont,
		minTarget: a.minTarget,
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
		console.log(`${pad("view", 9)}${pad("page", 16)}${pad("in<16", 7)}${pad("minFont", 9)}${pad("tgt", 5)}${pad("tgt<44", 8)}${pad("worst", 26)}${pad("scrollW", 9)}${pad("lost", 5)}err`);
		for (const r of results) {
			if (r.error) {
				console.log(`${pad(r.viewport, 9)}${pad(r.slug, 16)}LOAD ERROR  ${r.error}`);
				continue;
			}
			const badIn = (r.inputs || []).filter((i) => !i.ok);
			const minFont = Math.min(...(r.inputs || []).map((i) => i.fontSize), Infinity);
			// 判红只算 #382 列举的控件族（scope 非空）；其余计入 oos 列，仅统计
			const badT = (r.targets || []).filter((t) => !t.ok && (a.allTargets || t.scope));
			const oos = (r.targets || []).filter((t) => !t.ok && !t.scope).length;
			const worst = badT.slice().sort((x, y) => Math.min(x.hitW, x.hitH) - Math.min(y.hitW, y.hitH))[0];
			console.log(
				pad(r.viewport, 9) +
					pad(r.slug, 16) +
					pad(badIn.length, 7) +
					pad(Number.isFinite(minFont) ? minFont : "-", 9) +
					pad((r.targets || []).length, 5) +
					pad(badT.length + (oos ? `+${oos}` : ""), 8) +
					pad(worst ? `${worst.hitW}×${worst.hitH} ${worst.sel}`.slice(0, 25) : "-", 26) +
					pad(`${r.pageScrollWidth}/${r.viewportWidth}`, 9) +
					pad((r.lost || []).length, 5) +
					(r.errors ? r.errors.length : 0),
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
