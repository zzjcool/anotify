#!/usr/bin/env node
/* P2 工程守卫 e2e
 *
 * 断言的都是一次性结构事实，不碰像素、不依赖时序采样：
 *   1) 字体自托管：页面零外部字体请求 + 实际 family 可用（document.fonts）
 *   2) 抽屉 a11y：390 视口下 role=dialog / aria-modal / Esc 关闭 / 焦点回归
 *   3) reduce 语义：--dur / --dur-modal-out 归零（动效 token 是唯一事实源）
 *   4) View Transitions：跨文档导航 opt-in 生效 + 全程 0 pageerror
 *   5) #489：源页 ⋯ 动作菜单在 reduce 下**不得自激重排**（Chrome ≥153 会冻死页面）
 *
 * 为什么不用固定 sleep：所有「等状态」都走 waitForSelector / waitForFunction，
 * 断言失败时把实际值打进 detail（例如实际请求 URL、aria-expanded 的值），
 * 避免红的时候只有一句「expected true」。第 5 段是唯一的例外：它断言的**就是**
 * 「2 秒内发生了几次」这种速率事实，所以那里显式采样一个固定窗口（口径写在函数上方）。
 */
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const DESKTOP = { width: 1440, height: 900 };
const MOBILE = { width: 390, height: 844 };

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

/* 外域字体请求收集器：只盯 googleapis / gstatic 两个产品明确不再使用的源。
 * 注意：加了 mockExternalCdn 的 context 也会触发 request 事件（route 只改响应），
 * 所以这里统计的是「页面有没有发出请求」，与是否被 mock 无关。 */
function watchExternalFonts(page) {
	const hits = [];
	page.on("request", (req) => {
		const url = req.url();
		if (/fonts\.googleapis\.com|fonts\.gstatic\.com/.test(url)) hits.push(url);
	});
	return hits;
}

function watchPageErrors(page) {
	const errors = [];
	page.on("pageerror", (e) => errors.push(String(e)));
	return errors;
}

/* 等字体真正 load 完再问 fonts.check：check() 对未加载的 @font-face 返回 false。 */
async function fontsReady(page) {
	await page.evaluate(() => document.fonts.ready.then(() => true));
}

/* ============================ #489 采样口径 ============================
 *
 * 缺陷（产品侧，P1）：Chrome ≥153 + prefers-reduced-motion: reduce 下，源页行内
 * ⋯ 菜单点开就**页面冻死**。机制是自激：
 *
 *   placeMenu() 写 menu.style.left/top
 *     → ui.css 的 reduce 兜底把 `*` 的 transition-duration 压到 0.001ms，
 *       而 transition-property 仍是初始值 `all` ⇒ 这次写入真的起了一个过渡
 *     → transitionend
 *     → document 上的 transitionend 监听器 → reflowMenus()
 *     → placeMenu() 再写 left/top ……
 *
 * Chrome ≤152 每一跳都被动画时钟（≥1 帧）限速，所以只是「慢」；≥153 起不再限速，
 * 变成同步死循环，主线程被自己的回调占满。实测（2s 采样）：1550 次 transitionend
 * （属性 left/top）、page.evaluate 4s 不返回；消融实验里只去掉 transitionend 监听
 * 或只加 `.act-menu > .menu { transition: none }`，两者都立刻恢复正常。
 *
 * 所以这里断言的是**两个可测物理量**，不是「有没有加保护机制」：
 *   ① 点开菜单后 2 秒内 transitionend 次数 < MENU_TE_BUDGET；
 *   ② 采样结束后 page.evaluate 能在 READ_BUDGET 内往返（冻死时它永不返回）。
 * 冻结时计数器读不出来 —— 那本身就是 ① 的失败证据（detail 里写明）。
 */
const MENU_TE_BUDGET = 10;
const MENU_SAMPLE_MS = 2000;
const MENU_READ_BUDGET = 2500;

/* Playwright 自带 Chromium 的位置（#489 的对照浏览器：140 不复现，≥153 必现）。
 * playwright-core 不会下载浏览器，所以 CI 上通常没有 —— 找不到就**明说**跳过那条腿，
 * 不伪造一条绿断言（那正是本仓最贵的假绿形态）。 */
function bundledChromium140() {
	const cache = path.join(homedir(), ".cache", "ms-playwright");
	if (!existsSync(cache)) return "";
	const candidates = [];
	for (const name of readdirSync(cache)) {
		if (!name.startsWith("chromium-")) continue;
		for (const sub of ["chrome-linux64/chrome", "chrome-linux/chrome"]) {
			const p = path.join(cache, name, sub);
			if (existsSync(p)) candidates.push(p);
		}
	}
	for (const bin of candidates) {
		let version = "";
		try {
			version = execFileSync(bin, ["--version"], { encoding: "utf8" });
		} catch {
			continue;
		}
		// 只认 140.x：对照腿的意义就是「老浏览器行为逐字不变」，换版本等于换命题。
		if (/\b140\./.test(version)) return bin;
	}
	return "";
}

/* 等页面自己安静下来：连续两帧没有新的 transitionend、且没有在跑的 CSS 过渡。
 * 为什么需要：采样窗要量的只有「点开菜单这一下造成的事件」，而详情页首屏一次流程图
 * 渲染就会甩出 80+ 条 transitionend（`scrollbar-color` 在 `transition-property:all`
 * 下每节点一条）。那是页面首屏的画，不是这次要断言的回路。
 * 这是**状态**等待（无过渡在跑 / 没新事件），不是 sleep —— 然后在采样前把计数器
 * 归零，窗口里就只剩本次点击的因果。 */
async function waitForTransitionQuiet(page, maxFrames = 180) {
	await page.evaluate(async (limit) => {
		let last = window.__te.n;
		let quiet = 0;
		for (let i = 0; i < limit; i++) {
			await new Promise((r) => requestAnimationFrame(r));
			const running = document.getAnimations
				? document
						.getAnimations()
						.filter((a) => a instanceof CSSTransition && a.playState === "running").length
				: 0;
			if (window.__te.n === last && running === 0) {
				if (++quiet >= 2) return;
			} else {
				quiet = 0;
				last = window.__te.n;
			}
		}
	}, maxFrames);
	await page.evaluate(() => {
		window.__te = { n: 0, props: {} };
	});
}

/* 一条腿 = 一个浏览器 + 一个页面：点开 ⋯ 菜单 → 采样 → 读回计数与几何。
 * reduced-motion 全程开着，断言里也验它真的生效 —— 用户场景必须留在测试里。 */
async function menuSelfReflowLeg(browser, { label, origin, session, path: pagePath, trigger }) {
	const ctx = await browser.newContext({
		locale: "zh-CN",
		viewport: DESKTOP,
		reducedMotion: "reduce",
	});
	await H.mockExternalCdn(ctx);
	// 计数器必须比页面脚本更早装：用 capture 阶段，先于页面自己的 transitionend 监听器。
	await ctx.addInitScript(() => {
		window.__te = { n: 0, props: {} };
		document.addEventListener(
			"transitionend",
			(e) => {
				window.__te.n++;
				const k = e.propertyName || "?";
				window.__te.props[k] = (window.__te.props[k] || 0) + 1;
			},
			true,
		);
	});
	await injectSession(ctx, session, origin);
	const page = await ctx.newPage();
	const errors = watchPageErrors(page);
	await page.goto(origin + pagePath, { waitUntil: "load", timeout: 20000 });
	await page.waitForSelector(trigger, { timeout: 15000 });

	const reducedOn = await page.evaluate(
		() => matchMedia("(prefers-reduced-motion: reduce)").matches,
	);
	H.check(
		`${label}：reduce 模拟真的生效（用户场景没被测试绕开）`,
		reducedOn === true,
		`matches=${reducedOn}`,
	);

	// 先等首屏那批过渡跑完并归零计数器，采样窗里只剩本次点击的因果
	await waitForTransitionQuiet(page);

	await page.locator(trigger).first().click({ timeout: 15000 });
	// 采样窗：Node 侧等 2s，不给渲染进程任何额外任务（挤它会让读数偏向「没冻死」）。
	await new Promise((r) => setTimeout(r, MENU_SAMPLE_MS));

	const readStart = Date.now();
	let snap = null;
	let readErr = "";
	try {
		snap = await Promise.race([
			page.evaluate(() => {
				const open = document.querySelector(".act-menu .menu:not(.hidden)");
				const trig = document.querySelector(".act-menu .act-more");
				const r = open ? open.getBoundingClientRect() : null;
				const t = trig ? trig.getBoundingClientRect() : null;
				return {
					te: window.__te.n,
					props: window.__te.props,
					menu: r ? { left: r.left, top: r.top, right: r.right, bottom: r.bottom } : null,
					trig: t ? { left: t.left, top: t.top, right: t.right, bottom: t.bottom } : null,
					vw: innerWidth,
					vh: innerHeight,
				};
			}),
			new Promise((_, rej) =>
				setTimeout(
					() => rej(new Error(`page.evaluate ${MENU_READ_BUDGET}ms 内没有返回`)),
					MENU_READ_BUDGET,
				),
			),
		]);
	} catch (e) {
		readErr = e.message;
	}
	const readMs = Date.now() - readStart;
	// 绿的时候也把读数打出来：这条套件以后要回答「到底几次」而不是「绿了」。
	console.log(
		`  · ${label} 采样：transitionend=${snap ? snap.te : "读不到"} 属性分布=${snap ? JSON.stringify(snap.props) : "-"} evaluate=${snap ? readMs + "ms" : "超时"}`,
	);

	// ② 交互性：冻死时 evaluate 永不返回（#485 里 52s 硬杀就是这么来的）。
	H.check(
		`${label}：${MENU_SAMPLE_MS}ms 采样后页面仍可交互（evaluate ${MENU_READ_BUDGET}ms 内往返）`,
		!!snap,
		readErr ? `${readErr}（主线程被自己的过渡回调占满）` : `往返 ${readMs}ms`,
	);
	// ① 速率：正常应为个位数（开合本身最多几个过渡），自激时上千。
	H.check(
		`${label}：点开 ⋯ 后 ${MENU_SAMPLE_MS}ms 内 transitionend < ${MENU_TE_BUDGET}`,
		!!snap && snap.te < MENU_TE_BUDGET,
		snap
			? `transitionend=${snap.te} 属性分布=${JSON.stringify(snap.props)}`
			: `计数器读不出来（页面冻死：${readErr}）`,
	);
	// #489 的签名本身：``transitionend prop=left``。这一条是硬零 —— 面板是
	// `transition: none`，写 left/top 不该再产生任何过渡；上面那条全局预算留出了
	// 触发按钮自己的 color/border/transform 过夜（实测 4~9 次）。
	H.check(
		`${label}：菜单自身的 left/top 过渡次数为 0（#489 的签名）`,
		!!snap && !snap.props.left && !snap.props.top,
		snap
			? `left=${snap.props.left || 0} top=${snap.props.top || 0} 全部=${JSON.stringify(snap.props)}`
			: "页面无响应",
	);

	if (snap) {
		const m = snap.menu;
		const t = snap.trig;
		H.check(
			`${label}：菜单出现在视口内（左右各留 8px，不越界）`,
			!!m && m.left >= 7.5 && m.right <= snap.vw - 7.5 && m.top >= 7.5 && m.bottom <= snap.vh - 7.5,
			m ? `menu=${JSON.stringify(m)} vw=${snap.vw} vh=${snap.vh}` : "没有可见的 .act-menu .menu",
		);
		H.check(
			`${label}：菜单贴着触发器（下方 8px，放不下才翻到上方）`,
			!!m && !!t && (Math.abs(m.top - (t.bottom + 8)) <= 1 || Math.abs(m.bottom - (t.top - 8)) <= 1),
			m && t ? `menuTop=${m.top} menuBottom=${m.bottom} trigBottom=${t.bottom} trigTop=${t.top}` : "缺矩形",
		);
		H.check(
			`${label}：菜单水平方向不左溢（#376 修的那个 63~77px）`,
			!!m && !!t && m.right <= snap.vw - 7.5,
			m ? `menuRight=${m.right} vw=${snap.vw}` : "缺矩形",
		);
	}

	/* ③ 「过渡风暴不会喂回菜单」：reduce 下 `transition-property` 仍是 `all`，页面上
	   任何一个元素的属性变化都会变成一条 transitionend。所以连发 200 次非菜单来源的
	   transitionend（模拟其它元素的过渡结束）：重排允许被它们触发，但**不允许**因此
	   多出任何新的过渡事件（那就意味着重排又在写会触发过渡的属性）。 */
	let storm = null;
	let stormErr = "";
	try {
		storm = await Promise.race([
			page.evaluate(async () => {
				const before = window.__te.n;
				const row = document.querySelector(".list-row:not(.act-menu)") || document.body;
				for (let i = 0; i < 200; i++) {
					row.dispatchEvent(
						new TransitionEvent("transitionend", {
							bubbles: true,
							propertyName: i % 2 ? "transform" : "background-color",
						}),
					);
				}
				// 让被合并到帧里的重排真的跑完
				await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
				const open = document.querySelector(".act-menu .menu:not(.hidden)");
				const menu = open ? open.getBoundingClientRect() : null;
				return {
					delta: window.__te.n - before,
					menu: menu ? { left: menu.left, top: menu.top, right: menu.right, bottom: menu.bottom } : null,
				};
			}),
			new Promise((_, rej) =>
				setTimeout(
					() => rej(new Error(`page.evaluate ${MENU_READ_BUDGET}ms 内没有返回`)),
					MENU_READ_BUDGET,
				),
			),
		]);
	} catch (e) {
		stormErr = e.message;
	}
	H.check(
		`${label}：200 次外来 transitionend 不引发任何新过渡（重排不写会触发过渡的属性）`,
		!!storm && storm.delta === 200,
		storm
			? `新增 transitionend=${storm.delta}（期望正好 200，多出来的就是重排自己触发的）`
			: `页面无响应：${stormErr}`,
	);
	if (snap && storm && storm.menu) {
		H.check(
			`${label}：风暴后菜单位置不变（重排是幂等的，不是每次重算重写）`,
			Math.abs(storm.menu.left - snap.menu.left) < 0.5 &&
				Math.abs(storm.menu.top - snap.menu.top) < 0.5,
			`前 left=${snap.menu.left} top=${snap.menu.top} / 后 left=${storm.menu.left} top=${storm.menu.top}`,
		);
	}

	// 交互仍正常：Esc 关菜单（顺带证明风暴后页面没卡在奇怪状态）
	await page.keyboard.press("Escape");
	const closed = await page
		.locator(".act-menu .menu")
		.first()
		.isHidden({ timeout: 3000 })
		.catch(() => false);
	H.check(`${label}：Esc 能关掉菜单（交互逻辑未受影响）`, closed === true, `hidden=${closed}`);

	H.check(`${label}：全程 0 pageerror`, errors.length === 0, errors[0] || "");

	await ctx.close();
	return snap;
}

async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: p2_guards（字体自托管 / 抽屉 a11y / reduce 语义 / View Transitions）===",
	);
	server = await H.startServer({ suiteName: "p2_guards", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox", "--disable-dev-shm-usage"],
	});
	const s = H.seed(server.dbPath, "p2_guards");
	const origin = server.base;

	// ---------- 1) 字体自托管 ----------
	console.log("--- 1) 字体自托管：0 外部请求 + 设计字体真的可用 ---");
	{
		// 这个 context 故意**不** mock 外域：真有 googleapis 引用时请求会真发出去，
		// 统计到的就是真问题，而不是被 mock 吞掉后的假绿。
		const ctx = await browser.newContext({ locale: "zh-CN", viewport: DESKTOP });
		await injectSession(ctx, s.session, origin);
		const page = await ctx.newPage();
		const fontHits = watchExternalFonts(page);
		const pageErrors = watchPageErrors(page);
		await page.goto(origin + "/", { waitUntil: "load", timeout: 20000 });
		await H.waitForAppReady(page, "workspace");
		await fontsReady(page);

		H.check(
			"index：零 fonts.googleapis.com / gstatic.com 请求",
			fontHits.length === 0,
			fontHits.join(", ") || "0 条外部字体请求",
		);
		const inter = await page.evaluate(() => ({
			check: document.fonts.check('16px "Inter"'),
			bodyFamily: getComputedStyle(document.body).fontFamily,
		}));
		H.check(
			'index：document.fonts.check(\'16px "Inter"\') 为 true',
			inter.check === true,
			`check=${inter.check} body font-family=${inter.bodyFamily}`,
		);
		H.check(
			"index：body 计算字体以 Inter 开头（自托管字体真的生效）",
			/^"?Inter"?/.test(inter.bodyFamily),
			`font-family=${inter.bodyFamily}`,
		);
		H.check(
			"index：0 pageerror（字体加载不抛错）",
			pageErrors.length === 0,
			pageErrors[0] || "",
		);
		await ctx.close();
	}

	{
		const ctx = await browser.newContext({
			locale: "zh-CN",
			viewport: DESKTOP,
		});
		await H.mockExternalCdn(ctx);
		const page = await ctx.newPage();
		const fontHits = watchExternalFonts(page);
		await page.goto(origin + "/login.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "login");
		await fontsReady(page);

		H.check(
			"login：零 fonts.googleapis.com / gstatic.com 请求",
			fontHits.length === 0,
			fontHits.join(", ") || "0 条外部字体请求",
		);
		const fraunces = await page.evaluate(() => ({
			check: document.fonts.check('16px "Fraunces"'),
			headline: (() => {
				const el = document.querySelector(".auth-headline, .auth-title");
				return el ? getComputedStyle(el).fontFamily : "(未找到 .auth-headline/.auth-title)";
			})(),
		}));
		H.check(
			'login：document.fonts.check(\'16px "Fraunces"\') 为 true',
			fraunces.check === true,
			`check=${fraunces.check} headline font-family=${fraunces.headline}`,
		);
		await ctx.close();
	}

	{
		const ctx = await browser.newContext({ locale: "zh-CN", viewport: DESKTOP });
		await injectSession(ctx, s.session, origin);
		const page = await ctx.newPage();
		await page.goto(origin + "/docs.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await fontsReady(page);
		const mono = await page.evaluate(() => {
			const el = document.querySelector(".mono");
			return {
				check: document.fonts.check('16px "JetBrains Mono"'),
				monoFamily: el ? getComputedStyle(el).fontFamily : "(页面无 .mono 元素)",
				preload: [...document.querySelectorAll("link")].filter((l) =>
					/fonts\.(googleapis|gstatic)\.com/.test(l.href),
				).length,
			};
		});
		H.check(
			'docs：document.fonts.check(\'16px "JetBrains Mono"\') 为 true',
			mono.check === true,
			`check=${mono.check} .mono font-family=${mono.monoFamily}`,
		);
		H.check(
			"docs：.mono 元素的计算字体包含 JetBrains Mono",
			/JetBrains Mono/.test(mono.monoFamily),
			`font-family=${mono.monoFamily}`,
		);
		H.check(
			"docs：head 里没有指向外部字体域名的 <link>",
			mono.preload === 0,
			`匹配 ${mono.preload} 条`,
		);
		await ctx.close();
	}

	// ---------- 2) 抽屉 a11y（390 视口）----------
	console.log("--- 2) 抽屉 a11y：role/aria-modal/Esc/焦点回归 ---");
	{
		const ctx = await browser.newContext({ locale: "zh-CN", viewport: MOBILE });
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, origin);
		const page = await ctx.newPage();
		const pageErrors = watchPageErrors(page);
		await page.goto(origin + "/", { waitUntil: "load", timeout: 20000 });
		await H.waitForAppReady(page, "workspace");
		await page.waitForSelector("#menu-btn", { timeout: 10000 });

		const menuBtn = await page.evaluate(() => {
			const b = document.getElementById("menu-btn");
			return {
				display: b ? getComputedStyle(b).display : "(no #menu-btn)",
				expanded: b ? b.getAttribute("aria-expanded") : null,
			};
		});
		H.check(
			"390：汉堡按钮存在且可见（display 非 none）",
			menuBtn.display !== "none" && menuBtn.display !== "(no #menu-btn)",
			`display=${menuBtn.display}`,
		);
		H.check(
			"390：抽屉初始 aria-expanded=false",
			menuBtn.expanded === "false",
			`aria-expanded=${menuBtn.expanded}`,
		);

		await page.click("#menu-btn");
		await page.waitForFunction(
			() => document.getElementById("sidebar")?.getAttribute("role") === "dialog",
			null,
			{ timeout: 5000 },
		);
		// openSidebar 的 focus 在 requestAnimationFrame 里做（等滑入一帧）。
		// 不等这一步就直接按 Esc，会出现「rAF 在关闭后才跑，把焦点抢回已隐藏的
		// #sidebar-close」的竞态 —— 所以这里必须等焦点真的进面板。
		await page.waitForFunction(
			() => {
				const sb = document.getElementById("sidebar");
				return !!sb && sb.contains(document.activeElement);
			},
			null,
			{ timeout: 5000 },
		);
		const opened = await page.evaluate(() => {
			const sb = document.getElementById("sidebar");
			const ov = document.getElementById("sidebar-overlay");
			const btn = document.getElementById("menu-btn");
			return {
				role: sb.getAttribute("role"),
				modal: sb.getAttribute("aria-modal"),
				expanded: btn.getAttribute("aria-expanded"),
				overlayHidden: ov.classList.contains("hidden"),
				focusInside: sb.contains(document.activeElement),
				activeId: document.activeElement?.id || document.activeElement?.tagName,
			};
		});
		H.check(
			"打开抽屉：sidebar role=dialog",
			opened.role === "dialog",
			`role=${opened.role}`,
		);
		H.check(
			"打开抽屉：sidebar aria-modal=true",
			opened.modal === "true",
			`aria-modal=${opened.modal}`,
		);
		H.check(
			"打开抽屉：overlay 可见 + aria-expanded=true",
			opened.overlayHidden === false && opened.expanded === "true",
			`overlayHidden=${opened.overlayHidden} aria-expanded=${opened.expanded}`,
		);
		H.check(
			"打开抽屉：焦点被送进面板内",
			opened.focusInside === true,
			`activeElement=${opened.activeId}`,
		);

		await page.keyboard.press("Escape");
		await page.waitForFunction(
			() =>
				document
					.getElementById("sidebar-overlay")
					?.classList.contains("hidden") === true,
			null,
			{ timeout: 5000 },
		);
		const closed = await page.evaluate(() => {
			const sb = document.getElementById("sidebar");
			const ov = document.getElementById("sidebar-overlay");
			const btn = document.getElementById("menu-btn");
			return {
				overlayDisplay: getComputedStyle(ov).display,
				overlayHiddenClass: ov.classList.contains("hidden"),
				expanded: btn.getAttribute("aria-expanded"),
				role: sb.getAttribute("role"),
				activeId: document.activeElement?.id || "",
				bodyOverflow: document.body.style.overflow,
			};
		});
		H.check(
			"Esc 关闭：overlay 回到 hidden（display:none）",
			closed.overlayHiddenClass === true && closed.overlayDisplay === "none",
			`class.hidden=${closed.overlayHiddenClass} display=${closed.overlayDisplay}`,
		);
		H.check(
			"Esc 关闭：aria-expanded 回到 false",
			closed.expanded === "false",
			`aria-expanded=${closed.expanded}`,
		);
		H.check(
			"Esc 关闭：焦点回到 #menu-btn",
			closed.activeId === "menu-btn",
			`activeElement=#${closed.activeId}`,
		);
		H.check(
			"Esc 关闭：抽屉态角色被摘掉（role 不残留 dialog）",
			closed.role === null,
			`role=${closed.role}`,
		);
		H.check(
			"Esc 关闭：body 滚动锁释放",
			closed.bodyOverflow === "",
			`body.style.overflow="${closed.bodyOverflow}"`,
		);
		H.check(
			"390 抽屉流程 0 pageerror",
			pageErrors.length === 0,
			pageErrors[0] || "",
		);
		await ctx.close();
	}

	// ---------- 3) reduce 语义 ----------
	console.log("--- 3) reduce 语义：动效 token 归零 ---");
	{
		const ctx = await browser.newContext({
			locale: "zh-CN",
			viewport: DESKTOP,
			reducedMotion: "reduce",
		});
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, origin);
		const page = await ctx.newPage();
		await page.goto(origin + "/", { waitUntil: "load", timeout: 20000 });
		await H.waitForAppReady(page, "workspace");
		const tokens = await page.evaluate(() => {
			const cs = getComputedStyle(document.documentElement);
			return {
				matches: matchMedia("(prefers-reduced-motion: reduce)").matches,
				dur: cs.getPropertyValue("--dur").trim(),
				modalOut: cs.getPropertyValue("--dur-modal-out").trim(),
			};
		});
		H.check(
			"reduce：matchMedia(prefers-reduced-motion) 为 true（emulation 生效）",
			tokens.matches === true,
			`matches=${tokens.matches}`,
		);
		H.check(
			"reduce：--dur 计算值为 0ms",
			tokens.dur === "0ms",
			`--dur="${tokens.dur}"`,
		);
		H.check(
			"reduce：--dur-modal-out 计算值为 0ms（弹层退场计时同源）",
			tokens.modalOut === "0ms",
			`--dur-modal-out="${tokens.modalOut}"`,
		);
		await ctx.close();
	}

	// ---------- 4) View Transitions ----------
	console.log("--- 4) View Transitions：跨文档导航 opt-in ---");
	{
		const ctx = await browser.newContext({ locale: "zh-CN", viewport: DESKTOP });
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, origin);
		// pagereveal 在新文档里触发；addInitScript 每个文档都会先跑，
		// 所以新文档的探针一定是干净的初值后再累加。
		await ctx.addInitScript(() => {
			window.__vtProbe = { reveals: 0, withViewTransition: 0 };
			addEventListener("pagereveal", (e) => {
				window.__vtProbe.reveals++;
				if (e.viewTransition) window.__vtProbe.withViewTransition++;
			});
		});
		const page = await ctx.newPage();
		const pageErrors = watchPageErrors(page);
		await page.goto(origin + "/", { waitUntil: "load", timeout: 20000 });
		await H.waitForAppReady(page, "workspace");

		const head = await page.evaluate(() => {
			const styles = [...document.head.querySelectorAll("style")].map(
				(s) => s.textContent || "",
			);
			return {
				inline: styles.some((t) => t.includes("@view-transition")),
				navAuto: styles.some((t) => /@view-transition[\s\S]*navigation:\s*auto/.test(t)),
			};
		});
		H.check(
			"index：head 内联 <style> 含 @view-transition（opt-in 必须内联）",
			head.inline === true,
			`inline=${head.inline}`,
		);
		H.check(
			"index：内联规则是 navigation: auto",
			head.navAuto === true,
			`navAuto=${head.navAuto}`,
		);

		await page.click('#page-main a[href="events.html"]');
		const landed = await H.waitForUrl(page, /events\.html/, 15000);
		H.check(
			"点击页面内链接后落在 events.html",
			/events\.html/.test(landed),
			`url=${landed}`,
		);
		H.check(
			"跨文档导航全程 0 pageerror",
			pageErrors.length === 0,
			pageErrors[0] || "",
		);

		const probe = await (async () => {
			// pagereveal 在新文档首帧后才触发，H.waitForUrl 可能在提交瞬间就返回 ——
			// 必须等探针真的累加，不能马上读（否则竞态读到的永远是初值 0）。
			try {
				await page.waitForFunction(
					() => window.__vtProbe && window.__vtProbe.reveals > 0,
					null,
					{ timeout: 5000 },
				);
			} catch {
				/* 等不到就照实报：下面的断言会打出 reveals 的实际值 */
			}
			return page.evaluate(() => window.__vtProbe || null);
		})();
		H.check(
			"新文档触发 pagereveal，且 viewTransition 非空",
			!!probe && probe.reveals > 0 && probe.withViewTransition > 0,
			probe
				? `reveals=${probe.reveals} withViewTransition=${probe.withViewTransition}`
				: "window.__vtProbe 未注入（addInitScript 未生效）",
		);
		const eventsHead = await page.evaluate(() =>
			[...document.head.querySelectorAll("style")].some((s) =>
				(s.textContent || "").includes("@view-transition"),
			),
		);
		H.check(
			"events：目标文档同样内联 @view-transition（双向 opt-in）",
			eventsHead === true,
			`inline=${eventsHead}`,
		);
		await ctx.close();
	}

	// ---------- 5) #489：⋯ 菜单在 reduce 下不得自激重排 ----------
	// 这一步**必须**用真 Chrome（channel: "chrome"）：缺陷的决定性变量是浏览器版本，
	// 不是压力，也不是代码里能模拟的东西（CI 157 / 本地 153 必现，Chromium 140 不复现）。
	console.log(
		"--- 5) #489 源页 ⋯ 菜单：reduce 下写坐标不得变成新的 transitionend ---",
	);
	{
		const SRC = "e2e-489-loop";
		const created = await H.req(origin, `/v1/namespaces/default/sources`, {
			session: s.session,
			method: "POST",
			body: {
				apiVersion: "anotify/v1",
				kind: "Source",
				metadata: { name: SRC },
				spec: { kind: "webhook", enabled: true, config: {}, targets: [] },
			},
		});
		H.check(
			"#489 造一条源（列表行才有 ⋯ 动作菜单）",
			created.status === 201,
			`status=${created.status} ${String(created.text || "").slice(0, 160)}`,
		);

		const chromeVersion = browser.version();
		console.log(
			`  被测浏览器（真实缺陷浏览器）：Chrome ${chromeVersion}（CI 上是 157，本地 153）`,
		);
		await menuSelfReflowLeg(browser, {
			label: `Chrome ${chromeVersion} 源页`,
			origin,
			session: s.session,
			path: "/sources.html",
			trigger: "#src-list .src-row .act-more",
		});
		// #491 这里原有一条「源详情页」腿：旧的 web-src/pages/source.html 是重复源页，
		// 页头 ⋯ 用的是同一套 placeMenu/reflowMenus 的**另一份拷贝**，所以两份都得验。
		// 黑盒审计 B-4 之后那一页被删除（/source.html 带参/无参一律 404，404 探针在
		// events_paging 与 f3-source），页面脚本的拷贝就只剩 sources.html 这一份 ——
		// 上面那条腿即全覆盖，再留一条 goto 已删页的腿只会 404 判红。
		// 不要静默删腿：将来谁再把 placeMenu 复制到第二个页面，这里就得再加一条腿。

		// Chromium 140 的对照腿：这个浏览器**从未**复现过缺陷（决定性变量是版本）。
		// 同一组断言在原版本上跑一遍 = 「老浏览器行为没被这次改动改坏」。
		const bundled = bundledChromium140();
		if (bundled) {
			const oldBrowser = await chromium.launch({
				executablePath: bundled,
				headless: true,
				args: ["--no-sandbox", "--disable-dev-shm-usage"],
			});
			const oldVersion = oldBrowser.version();
			console.log(`  对照浏览器（Playwright 自带）：Chromium ${oldVersion} ← ${bundled}`);
			try {
				await menuSelfReflowLeg(oldBrowser, {
					label: `Chromium ${oldVersion} 源页`,
					origin,
					session: s.session,
					path: "/sources.html",
					trigger: "#src-list .src-row .act-more",
				});
			} finally {
				await oldBrowser.close();
			}
		} else {
			// 不伪造断言：playwright-core 不下载浏览器，CI 上没有这一档。
			console.log(
				"  ⚠️ 未找到 Playwright 自带的 Chromium 140 ⇒ #489 的对照腿本次未执行" +
					"（本机事实，不是产品判断；装了 `npx playwright install chromium@140` 就会跑）",
			);
		}
	}

	const passed = H.summary("p2_guards");
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
