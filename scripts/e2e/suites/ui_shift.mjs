#!/usr/bin/env node
/* SUITE: ui_shift — 开页抖动（layout shift）回归门禁
 *
 * 锁死这一批「开页抖动」修复，防止它们被后来的 UI 改动悄悄打回去。
 * 抖动是**最难靠肉眼 review 抓到**的一类回归：功能全对、断言全绿，
 * 只是首帧之后元素逐帧滑动，用户看到的是页面"跳一下"。
 * 所以这里不解构视觉，只对**几何量**做去重断言：
 * 只要某个量在加载期出现过两个不同的值，就是抖动。
 *
 * 四条修复各对应一组断言：
 *  1) 语言条首帧即终态（partials.js mountLangHint 的 --instant 同步展高）
 *     index(en-US) 从首帧起 1.5s 采样：.app-nav 的 y、#app-shell 的 y 各自只有一个值。
 *     旧实现用双 rAF 展开 + ResizeObserver 逐帧回写 --shell-top-offset，
 *     外壳会在首帧之后从 y=0 逐帧下滑 44–45px（banner 段 CLS 0.03+）。
 *  2) 命名空间槽位 #scope-slot 预留 53px（展开/折叠同高）
 *     加载完成后槽位高度 52–54px；#scope-switcher 去 hidden 前后 nav.y 不变
 *     （MutationObserver 记录 class 变化瞬间的 nav.y）。
 *     旧实现里数据到达才插入 53px 的 .scope-block，nav 被整体下推 53px。
 *  3) 折叠态同样不抖：localStorage anotify-sidebar=collapsed 后 index 加载，
 *     nav.y 唯一、#sidebar 宽 76。
 *  4) 慢 JS 预排版（base.html 的 html:not([data-shell-ready]) #page-main 规则）
 *     partials.js 延迟 1200ms 再挂外壳：这段窗口里 #page-main 必须已经在
 *     264/76 的位置（而不是先 0 再跳过去），.page 的 x 全程唯一（展开 296 / 折叠 158）。
 *     旧实现挂外壳前页面是"裸的"：整页先按 x=0 排一遍，外壳一到整页右移 264px。
 *
 * 视口固定 1440×900：这个宽度下展开态 .page 内容宽 1112 < 1200（不居中 → 296），
 * 折叠态内容宽 1300 > 1200（居中 → 158）—— 两种模式都能量到有意义的数字。
 *
 * 采样用 addInitScript 装的 rAF 探针：抖动发生在首帧附近，等 load 之后
 * 再用 evaluate 采样只剩终态，拍不到"抖"。断言失败时把采样序列原样打出来。
 *
 * 用真实后端（H.startServer）而不是 mock：命名空间槽位、事件骨架都依赖
 * 真实端点的到达时刻，mock 全是同步的，反而测不出这段竞态窗口。
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const VIEWPORT = { width: 1440, height: 900 };

let server, browser;

async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: sessionValue,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
			secure: u.protocol === "https:",
		},
	]);
}

/* 采样探针：从文档首帧起逐帧记录几何量。
 *
 * 必须自包含（Playwright 会把函数体序列化进页面），所以不能引用外部变量。
 * 元素还没出现时记 null（不是 0）——「不存在」和「在 0」是两件事，
 * 后者恰恰是预排版要防的那种状态。
 */
function shiftProbe() {
	const round = (v) => Math.round(v * 100) / 100;
	const y = (sel) => {
		const el = document.querySelector(sel);
		return el ? round(el.getBoundingClientRect().y) : null;
	};
	const x = (sel) => {
		const el = document.querySelector(sel);
		return el ? round(el.getBoundingClientRect().x) : null;
	};
	const h = (sel) => {
		const el = document.querySelector(sel);
		return el ? round(el.getBoundingClientRect().height) : null;
	};
	const s = {
		frames: 0,
		stop: false,
		t: [], // 相对文档首帧的毫秒时间戳（诊断用）
		nav: [], // .app-nav 的 y
		shell: [], // #app-shell 的 y（null = 外壳还没挂）
		slotH: [], // #scope-slot 的高度
		pageMainX: [], // #page-main 的 x
		pageX: [], // .page 的 x
		switcherVisible: [], // #scope-switcher 是否已去 hidden
		bannerH: [], // .lang-hint 的高度
		scopeMutations: [], // #scope-switcher class 变化瞬间的快照
	};
	window.__shift = s;

	const tick = () => {
		s.frames++;
		s.t.push(Math.round(performance.now()));
		s.nav.push(y(".app-nav"));
		s.shell.push(y("#app-shell"));
		s.slotH.push(h("#scope-slot"));
		s.pageMainX.push(x("#page-main"));
		s.pageX.push(x(".page"));
		const sw = document.getElementById("scope-switcher");
		s.switcherVisible.push(sw ? !sw.classList.contains("hidden") : null);
		s.bannerH.push(h(".lang-hint"));
		if (!s.stop) requestAnimationFrame(tick);
	};
	requestAnimationFrame(tick);

	/* 命名空间切换器：记录「去 hidden 之前」的 nav.y，再观察 class 变化。
	 * 用 MutationObserver 而不是猜时间窗：class 变化与 skel.remove() 在
	 * 同一个同步块里（partials.js mountNamespaceSwitcher），所以回调里量到的
	 * nav.y 就是"数据到达之后"的终态值，两者相等 = 槽位真的零位移。 */
	let tries = 0;
	const watchScope = () => {
		const sw = document.getElementById("scope-switcher");
		if (!sw) {
			if (++tries < 1500) requestAnimationFrame(watchScope); // 最多约 25s
			return;
		}
		s.scopeMutations.push({
			stage: "before",
			hidden: sw.classList.contains("hidden"),
			navY: y(".app-nav"),
			shellY: y("#app-shell"),
		});
		new MutationObserver(() => {
			s.scopeMutations.push({
				stage: "mutation",
				hidden: sw.classList.contains("hidden"),
				navY: y(".app-nav"),
				shellY: y("#app-shell"),
			});
		}).observe(sw, { attributes: true, attributeFilter: ["class"] });
	};
	requestAnimationFrame(watchScope);
}

async function addProbe(ctx) {
	await ctx.addInitScript(shiftProbe);
}

/* 把采样序列压成「值×连续帧数」，失败时贴出来定位。
 * 例：`0×1→45×89` 表示第一帧是 0、之后 89 帧都是 45（这就是抖动）。 */
function seq(arr, max = 420) {
	const runs = [];
	for (const v of arr) {
		const last = runs[runs.length - 1];
		if (last && last.v === v) last.n++;
		else runs.push({ v, n: 1 });
	}
	return runs
		.map((r) => (r.n > 1 ? `${r.v}×${r.n}` : `${r.v}`))
		.join("→")
		.slice(0, max);
}

const uniq = (arr) => {
	const out = [];
	for (const v of arr) if (!out.includes(v)) out.push(v);
	return out;
};
const nonNull = (arr) => arr.filter((v) => v !== null);

/* 读探针：先停表，再把整份采样取回主进程。 */
async function readProbe(page) {
	return page.evaluate(() => {
		window.__shift.stop = true;
		return window.__shift;
	});
}

/* 统一建 context：locale + 固定视口 + 外部 CDN mock + 采样探针。 */
async function newCtx({ locale = "zh-CN", collapsed = false } = {}) {
	const ctx = await browser.newContext({ locale, viewport: VIEWPORT });
	await H.mockExternalCdn(ctx);
	await addProbe(ctx);
	if (collapsed) {
		// 在页面脚本之前写好折叠偏好：base 的 inline script 要读它来设 --pre-sidebar-w。
		await ctx.addInitScript(() => {
			try {
				localStorage.setItem("anotify-sidebar", "collapsed");
			} catch (e) {
				/* 隐私模式：折叠态用例会因 pre-sidebar-w 缺失而红，属于真问题 */
			}
		});
	}
	return ctx;
}

async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: ui_shift（开页抖动回归：语言条 / 命名空间槽位 / 折叠态 / 预排版）===",
	);
	server = await H.startServer({ suiteName: "ui_shift", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	// 真后端造数：首页「最近事件」卡片有真数据，骨架 → 真实列表的切换才是
	// 有身高变化的那一段（空态反而比骨架矮）。
	const s = H.seed(server.dbPath, "ui_shift");
	const ing = await H.req(server.base, "/v1/events", {
		key: s.sendKey,
		body: {
			source: "ui_shift.src",
			type: "go",
			payload: { title: "抖动回归种子事件" },
		},
	});
	H.check(
		"种子：/v1/events 收下一条事件（首页事件卡片有真数据）",
		ing.status === 200 && !!ing.json,
		`status=${ing.status} ${String(ing.text).slice(0, 120)}`,
	);

	// ---------- 1) index(en-US)：语言条不再把外壳/nav 顶下去 ----------
	console.log("--- 1) 语言条首帧即终态（index + en-US）---");
	{
		const ctx = await newCtx({ locale: "en-US" });
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await page.waitForTimeout(1500); // 采样窗口跑满 1.5s（探针从首帧起就在记）
		const probe = await readProbe(page);

		const banner = await page.evaluate(() => {
			const b = document.querySelector(".lang-hint");
			if (!b) return null;
			const r = b.getBoundingClientRect();
			return {
				h: Math.round(r.height * 100) / 100,
				open: b.classList.contains("lang-hint--open"),
				lang: b.querySelector(".lang-hint-inner")
					? b.querySelector(".lang-hint-inner").getAttribute("lang")
					: null,
				text: (b.textContent || "").replace(/\s+/g, " ").trim().slice(0, 60),
			};
		});
		// 前置：这条用例测的是「语言条插入时的位移」，语言条没出来就等于没测。
		H.check(
			"1) 前置：en-US 浏览中文首页确实弹出了语言条",
			!!banner && banner.h > 0 && banner.open === true && banner.lang === "en",
			JSON.stringify(banner),
		);

		const navY = nonNull(probe.nav);
		const navUniq = uniq(navY);
		H.check(
			`1) .app-nav 的 y 从首帧到稳定只有 1 个值（采样 ${navY.length} 帧 / 共 ${probe.frames} 帧）`,
			navY.length > 0 && navUniq.length === 1,
			`去重=${JSON.stringify(navUniq)} 序列=${seq(navY)}`,
		);

		const shellY = nonNull(probe.shell);
		const shellUniq = uniq(shellY);
		H.check(
			`1) #app-shell 的 y 从首帧到稳定只有 1 个值（采样 ${shellY.length} 帧）`,
			shellY.length > 0 && shellUniq.length === 1,
			`去重=${JSON.stringify(shellUniq)} 序列=${seq(shellY)}`,
		);

		// 语言条在流内（不是 overlay），外壳被它整体推下去 —— 位移量级是真的，
		// 不是"因为量到 0 所以唯一"。这条也顺带证明工具条确实占了版面。
		H.check(
			"1) #app-shell 的 y 等于语言条高度（提示条在流内、已整体让位）",
			shellUniq.length === 1 &&
				!!banner &&
				Math.abs(shellUniq[0] - banner.h) <= 1,
			`shellY=${shellUniq[0]} bannerH=${banner && banner.h}`,
		);

		// 采样窗口必须真的覆盖"首帧"：至少 60 帧（1.5s ≈ 90 帧）
		H.check(
			`1) 采样窗口覆盖首帧之后的 1.5s（${probe.frames} 帧 ≥ 60）`,
			probe.frames >= 60,
			`frames=${probe.frames}`,
		);

		await page.close();
		await ctx.close();
	}

	// ---------- 2) 命名空间槽位：53px 预留，去 hidden 前后 nav 不动 ----------
	console.log("--- 2) #scope-slot 预留 53px（数据到达零位移）---");
	{
		const ctx = await newCtx();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await page.waitForTimeout(1200);
		const probe = await readProbe(page);

		const slot = await page.evaluate(() => {
			const el = document.getElementById("scope-slot");
			const sw = document.getElementById("scope-switcher");
			return {
				h: el ? Math.round(el.getBoundingClientRect().height * 100) / 100 : null,
				switcherVisible: sw ? !sw.classList.contains("hidden") : null,
				skelLeft: !!document.querySelector(".scope-skel"),
				hasTrigger: !!document.querySelector("#scope-switcher .scope-trigger"),
			};
		});
		H.check(
			"2) #scope-slot 加载完成后高度在 52–54px（预留的那一格）",
			slot.h !== null && slot.h >= 52 && slot.h <= 54,
			`height=${slot.h}`,
		);
		H.check(
			"2) 命名空间数据已到达：骨架已换成真切换器",
			slot.switcherVisible === true && slot.skelLeft === false && slot.hasTrigger === true,
			JSON.stringify(slot),
		);

		// MutationObserver：class 变化前 / 变化后各一个 nav.y
		const muts = probe.scopeMutations.filter((m) => m.navY !== null);
		const mutNav = uniq(muts.map((m) => m.navY));
		H.check(
			`2) #scope-switcher 去 hidden 前后 nav.y 不变（观察器记到 ${muts.length} 个状态）`,
			muts.length >= 2 && mutNav.length === 1,
			`navY=${JSON.stringify(mutNav)} 记录=${JSON.stringify(muts.slice(0, 6))}`,
		);

		// 帧级交叉验证：骨架期 vs 数据到达后，nav.y 也必须相同
		const framesByVis = probe.switcherVisible.map((vis, i) => ({
			vis,
			y: probe.nav[i],
		}));
		const ySkel = uniq(
			framesByVis.filter((f) => f.vis === false && f.y !== null).map((f) => f.y),
		);
		const yData = uniq(
			framesByVis.filter((f) => f.vis === true && f.y !== null).map((f) => f.y),
		);
		H.check(
			"2) 帧级采样：骨架期与数据到达后的 nav.y 相同",
			yData.length === 1 &&
				(ySkel.length === 0 || (ySkel.length === 1 && ySkel[0] === yData[0])),
			`骨架期=${JSON.stringify(ySkel)} 数据后=${JSON.stringify(yData)} 序列=${seq(nonNull(probe.nav))}`,
		);

		// 槽位高度全程恒定（53px 从骨架期就在，不是数据到了才出现）
		const slotHs = uniq(nonNull(probe.slotH));
		H.check(
			"2) #scope-slot 高度从首帧到稳定只有 1 个值（骨架期就占住 53px）",
			slotHs.length === 1 && slotHs[0] >= 52 && slotHs[0] <= 54,
			`去重=${JSON.stringify(slotHs)} 序列=${seq(nonNull(probe.slotH))}`,
		);

		await page.close();
		await ctx.close();
	}

	// ---------- 3) 折叠态：nav 不跳、侧栏 76 ----------
	console.log("--- 3) 折叠态（anotify-sidebar=collapsed）---");
	{
		const ctx = await newCtx({ collapsed: true });
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await page.waitForTimeout(1500);
		const probe = await readProbe(page);

		const geo = await page.evaluate(() => {
			const sb = document.getElementById("sidebar");
			const shell = document.getElementById("app-shell");
			return {
				sidebarW: sb ? Math.round(sb.getBoundingClientRect().width * 100) / 100 : null,
				collapsed:
					shell && shell.dataset ? shell.getAttribute("data-collapsed") : null,
				preW: getComputedStyle(document.documentElement)
					.getPropertyValue("--pre-sidebar-w")
					.trim(),
				storage: (() => {
					try {
						return localStorage.getItem("anotify-sidebar");
					} catch (e) {
						return null;
					}
				})(),
			};
		});
		H.check(
			"3) 前置：折叠偏好真的生效（localStorage / data-collapsed / --pre-sidebar-w 76px）",
			geo.storage === "collapsed" &&
				geo.collapsed === "true" &&
				geo.preW === "76px",
			JSON.stringify(geo),
		);
		H.check(
			"3) 折叠态 #sidebar 宽 76px",
			geo.sidebarW === 76,
			`width=${geo.sidebarW}`,
		);
		const navY = nonNull(probe.nav);
		const navUniq = uniq(navY);
		H.check(
			`3) 折叠态 .app-nav 的 y 只有 1 个值（采样 ${navY.length} 帧）`,
			navY.length > 0 && navUniq.length === 1,
			`去重=${JSON.stringify(navUniq)} 序列=${seq(navY)}`,
		);
		const slotHs = uniq(nonNull(probe.slotH));
		H.check(
			"3) 折叠态 #scope-slot 同样恒为 52–54px（展开/折叠同高）",
			slotHs.length === 1 && slotHs[0] >= 52 && slotHs[0] <= 54,
			`去重=${JSON.stringify(slotHs)}`,
		);

		await page.close();
		await ctx.close();
	}

	// ---------- 4) 慢 JS 预排版：外壳挂载前 #page-main 就在正确位置 ----------
	console.log("--- 4) 慢 JS 预排版（partials.js 延迟 1200ms）---");
	for (const mode of ["expanded", "collapsed"]) {
		const collapsed = mode === "collapsed";
		// 期望值（1440 宽）：#page-main 首值 = 侧栏宽；.page 的 x 全程恒定。
		const wantMainX = collapsed ? 76 : 264;
		const wantPageX = collapsed ? 158 : 296;
		const ctx = await newCtx({ collapsed });
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		// partials.js 就是挂外壳的那段 JS。延迟 1200ms ≈ 70 帧：预排版规则
		// （html:not([data-shell-ready]) #page-main）必须在这段窗口里把页面摆好。
		//
		// 路径不能写 `**/partials.js*` 这种死名字：产物是**指纹化**的
		// （实测请求 /partials.0dc01f51.js），glob 会一个都不匹配 ——
		// 那样 page.route 静默失效，用例就变成「延迟根本没发生」的假绿。
		await page.route(/\/partials(\.[0-9a-f]+)?\.js/, async (route) => {
			/* 1200ms 而不是 500ms：探针的首帧可能被启动开销拖到 300ms+，
			 * 只剩 200ms 的窗口在满负载机器上只有 4~7 帧，
			 * 「外壳挂载前 ≥10 帧」这条就变成看机器脸色的 flake。
			 * 窗口拉长不降低断言强度（仍然要求 ≥10 帧真实采样）。 */
			await new Promise((r) => setTimeout(r, 1200));
			await route.continue();
		});
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await page.waitForTimeout(900);
		const probe = await readProbe(page);

		// 外壳挂载前的帧：shell 为 null 而 #page-main 已存在
		const pre = probe.pageMainX
			.map((v, i) => ({ x: v, shell: probe.shell[i], page: probe.pageX[i] }))
			.filter((o) => o.shell === null && o.x !== null);
		const preX = uniq(pre.map((o) => o.x));
		const prePageX = uniq(pre.map((o) => o.page));

		H.check(
			`4) [${mode}] 外壳挂载前采到 ${pre.length} 帧（partials.js 延迟生效，≥10 帧）`,
			pre.length >= 10,
			`preFrames=${pre.length} frames=${probe.frames} 首帧时刻=${probe.t[0]}ms 外壳出现于第 ${probe.shell.indexOf(true)} 帧（${probe.t[probe.shell.indexOf(true)]}ms）`,
		);
		H.check(
			`4) [${mode}] 预排版：#page-main 首个 x 就是 ${wantMainX}（不是 0）`,
			probe.pageMainX.length > 0 &&
				probe.pageMainX[0] === wantMainX &&
				preX.length === 1 &&
				preX[0] === wantMainX,
			`首值=${probe.pageMainX[0]} 挂载前去重=${JSON.stringify(preX)} 全序列=${seq(probe.pageMainX, 200)}`,
		);
		H.check(
			`4) [${mode}] .page 的 x 从首次出现到稳定只有 1 个值 = ${wantPageX}`,
			prePageX.length === 1 &&
				prePageX[0] === wantPageX &&
				uniq(nonNull(probe.pageX)).length === 1 &&
				uniq(nonNull(probe.pageX))[0] === wantPageX,
			`挂载前去重=${JSON.stringify(prePageX)} 全程去重=${JSON.stringify(uniq(nonNull(probe.pageX)))} 序列=${seq(nonNull(probe.pageX), 200)}`,
		);

		await page.close();
		await ctx.close();
	}

	const passed = H.summary("ui_shift");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
