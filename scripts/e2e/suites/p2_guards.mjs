#!/usr/bin/env node
/* P2 工程守卫 e2e
 *
 * 断言的都是一次性结构事实，不碰像素、不依赖时序采样：
 *   1) 字体自托管：页面零外部字体请求 + 实际 family 可用（document.fonts）
 *   2) 抽屉 a11y：390 视口下 role=dialog / aria-modal / Esc 关闭 / 焦点回归
 *   3) reduce 语义：--dur / --dur-modal-out 归零（动效 token 是唯一事实源）
 *   4) View Transitions：跨文档导航 opt-in 生效 + 全程 0 pageerror
 *
 * 为什么不用固定 sleep：所有「等状态」都走 waitForSelector / waitForFunction，
 * 断言失败时把实际值打进 detail（例如实际请求 URL、aria-expanded 的值），
 * 避免红的时候只有一句「expected true」。
 */
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

async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: p2_guards（字体自托管 / 抽屉 a11y / reduce 语义 / View Transitions）===",
	);
	server = await H.startServer({ suiteName: "p2_guards", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
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
