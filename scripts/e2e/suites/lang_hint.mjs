#!/usr/bin/env node
/* SUITE: lang_hint — language hint banner (progressive enhancement)
 *
 * Verifies the language hint banner per lang-hint-requirements.md §8 ACs.
 * The banner is pure client-side: it detects navigator.language mismatch
 * and shows a dismissible top bar linking to the same page in the detected
 * language. No redirect, no build-time DOM.
 *
 * AC coverage:
 *   AC-1   matching language → no banner (zh-CN browser on zh-CN page, etc.)
 *   AC-2   mismatch + supported → banner shown in target language text
 *   AC-3   unsupported language (fr, ko) → no banner
 *   AC-4   traditional Chinese prefix-match (zh-TW → zh-CN target per coordinator ruling)
 *   AC-5   click action navigates to same page in target lang, query+hash preserved
 *   AC-6   关掉后记住（本机 localStorage）：刷新不再弹；清掉记忆后再弹
 *   AC-7   build-time HTML has zero banner markup (crawler/SEO safe)
 *   AC-9   mobile 390 viewport: no horizontal overflow
 *   scope  全站生效：keys/events 等工作台页也弹（不再只 login/index）
 *   AC-11  横幅滚出视口后侧栏与顶栏重新对齐（F8 不复发）
 *
 * NOTE on copy: the design doc (lang-hint-design.md §6.2) is the source of
 * truth for banner text. The requirements doc §5.1 suggested shorter copy
 * ("View this page in English?") but the design finalized longer copy
 * ("This site is also available in English"). Tests assert the design-doc
 * copy (what the implementation shipped). This is a documentation note, not
 * a product bug.
 *
 * Red line: this suite reports product bugs; it never weakens assertions.
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const LANGS = ["zh-CN", "en", "ja", "es"];

/* Expected banner text per target language (from design doc §6.2).
 * These are the ACTUAL strings shipped in i18n.{lang}.js. */
const BANNER_TEXT = {
	"zh-CN": "本网站提供简体中文版本",
	en: "This site is also available in English",
	ja: "このサイトは日本語でもご利用いただけます",
	es: "Este sitio también está disponible en español",
};
const BANNER_ACTION = {
	"zh-CN": "切换到简体中文",
	en: "Switch to English",
	ja: "日本語に切り替える",
	es: "Cambiar a español",
};

/* lang → URL prefix (default zh-CN = root) */
function prefix(lang) {
	return lang === "zh-CN" ? "" : "/" + lang;
}

/* Check if a lang-hint banner is present in the page DOM */
async function getBanner(page) {
	return page.$(".lang-hint");
}

/* Navigate to a page with a specific browser locale and return the page handle.
 * locale sets navigator.language / navigator.languages in Chromium. */
async function openWithLocale(browser, base, path, locale) {
	const ctx = await browser.newContext({ locale });
	await H.mockExternalCdn(ctx);
	const pg = await ctx.newPage();
	const errors = [];
	pg.on("pageerror", (e) => errors.push(String(e)));
	await pg.goto(base + path, { waitUntil: "load" });
	/* Wait for JS mount (including mountLangHint) to complete */
	const pageType = /login\.html/.test(path) ? "login" : "workspace";
	await H.waitForAppReady(pg, pageType);
	return { ctx, pg, errors };
}

let server, browser;

async function main() {
	H.startTimer();
	console.log("=== SUITE: lang_hint (language hint banner) ===");
	server = await H.startServer({ suiteName: "lang_hint", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	const seedData = H.seed(server.dbPath, "lang_hint_tester");

	/* =========================================================
	 * AC-7: build-time HTML has zero banner markup
	 * (crawler / no-JS sees nothing — pure client-side injection)
	 * ========================================================= */
	console.log("--- AC-7: build-time HTML has no banner markup ---");
	for (const lang of LANGS) {
		const r = await H.req(server.base, prefix(lang) + "/login.html");
		H.check(
			`AC-7 ${lang}/login.html HTML has no .lang-hint DOM`,
			!r.text.includes("lang-hint"),
			"found lang-hint in static HTML",
		);
		H.check(
			`AC-7 ${lang}/login.html HTML has no banner text snippet`,
			!r.text.includes("This site is also available") &&
				!r.text.includes("本网站提供简体中文版本") &&
				!r.text.includes("このサイトは日本語でも"),
			"found banner text in static HTML",
		);
	}
	/* index.html too */
	{
		const r = await H.req(server.base, prefix("en") + "/");
		H.check(
			`AC-7 en/index.html (via /) HTML has no .lang-hint DOM`,
			!r.text.includes("lang-hint"),
			"found lang-hint in static HTML",
		);
	}

	/* =========================================================
	 * AC-1: matching language → no banner
	 * ========================================================= */
	console.log("--- AC-1: matching language → no banner ---");
	{
		const cases = [
			{
				locale: "zh-CN",
				path: "/login.html",
				label: "zh-CN browser on /login.html",
			},
			{
				locale: "en-US",
				path: "/en/login.html",
				label: "en-US browser on /en/login.html",
			},
			{
				locale: "ja-JP",
				path: "/ja/login.html",
				label: "ja-JP browser on /ja/login.html",
			},
			{
				locale: "es-ES",
				path: "/es/login.html",
				label: "es-ES browser on /es/login.html",
			},
		];
		for (const c of cases) {
			const { pg, ctx } = await openWithLocale(
				browser,
				server.base,
				c.path,
				c.locale,
			);
			const banner = await getBanner(pg);
			H.check(
				`AC-1 ${c.label} → no banner`,
				!banner,
				banner ? "banner found" : "",
			);
			await ctx.close();
		}
	}

	/* =========================================================
	 * AC-2: mismatch + supported → banner shown in target language
	 * ========================================================= */
	console.log("--- AC-2: mismatch + supported → banner in target lang ---");
	{
		/* AC-2.1: en-US browser on zh-CN login → English banner */
		const { pg, ctx, errors } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-2.1 en-US on /login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-2.1 banner text is English`,
				text.includes(BANNER_TEXT.en),
				`got "${text.trim()}"`,
			);
			const actionText = (await pg.textContent(".lang-hint-action")) || "";
			H.check(
				`AC-2.1 action button is "${BANNER_ACTION.en}"`,
				actionText.trim() === BANNER_ACTION.en,
				`got "${actionText.trim()}"`,
			);
			/* AC-8.2: text node has lang attribute for TTS */
			const innerLang = await pg.getAttribute(".lang-hint-inner", "lang");
			H.check(
				`AC-8.2 .lang-hint-inner lang="en"`,
				innerLang === "en",
				`got "${innerLang}"`,
			);
		}
		H.check(
			"AC-2.1 no JS console errors",
			errors.length === 0,
			errors.join("; "),
		);
		await ctx.close();
	}
	{
		/* AC-2.2: ja-JP browser on en login → Japanese banner */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/en/login.html",
			"ja-JP",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-2.2 ja-JP on /en/login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-2.2 banner text is Japanese`,
				text.includes(BANNER_TEXT.ja),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-2.3: es-419 (Latin American Spanish, prefix match) on zh-CN login → Spanish banner */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"es-419",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-2.3 es-419 on /login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-2.3 banner text is Spanish`,
				text.includes(BANNER_TEXT.es),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-2.4: zh-Hans-CN (script-tagged simplified) on en login → Chinese banner */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/en/login.html",
			"zh-Hans-CN",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-2.4 zh-Hans-CN on /en/login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-2.4 banner text is Chinese`,
				text.includes(BANNER_TEXT["zh-CN"]),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * AC-3: unsupported language → no banner
	 * ========================================================= */
	console.log("--- AC-3: unsupported language → no banner ---");
	{
		const cases = [
			{ locale: "fr-FR", path: "/login.html", label: "fr-FR on /login.html" },
			{
				locale: "de-DE",
				path: "/en/login.html",
				label: "de-DE on /en/login.html",
			},
			{
				locale: "ko-KR",
				path: "/ja/login.html",
				label: "ko-KR on /ja/login.html",
			},
		];
		for (const c of cases) {
			const { pg, ctx } = await openWithLocale(
				browser,
				server.base,
				c.path,
				c.locale,
			);
			const banner = await getBanner(pg);
			H.check(`AC-3 ${c.label} → no banner`, !banner, "banner found");
			await ctx.close();
		}
	}

	/* =========================================================
	 * AC-4: traditional Chinese prefix-match (coordinator ruling)
	 *   zh-TW on /en/login.html → Chinese banner (target zh-CN ≠ current en)
	 *   zh-HK on /login.html → no banner (target zh-CN = current zh-CN)
	 *   zh-Hant on /es/login.html → Chinese banner
	 *   zh (bare) on /en/login.html → Chinese banner (prefix match)
	 * ========================================================= */
	console.log("--- AC-4: traditional Chinese prefix-match ---");
	{
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/en/login.html",
			"zh-TW",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-4.1 zh-TW on /en/login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-4.1 banner text is Chinese`,
				text.includes(BANNER_TEXT["zh-CN"]),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}
	{
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"zh-HK",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-4.2 zh-HK on /login.html (zh-CN page) → no banner",
			!banner,
			"banner shown (should not — target=current)",
		);
		await ctx.close();
	}
	{
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/es/login.html",
			"zh-Hant",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-4.3 zh-Hant on /es/login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-4.3 banner text is Chinese`,
				text.includes(BANNER_TEXT["zh-CN"]),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-4.4: bare "zh" (no region) on /en/login.html → Chinese banner (prefix match)
		 * Note: requirements §4.1 says bare "zh" → no hint, but coordinator revised
		 * AC-4.4 to "出中文横幅（前缀匹配 zh-CN）". The implementation uses
		 * primary-subtag prefix match: "zh".split("-")[0] = "zh" which matches
		 * "zh-CN".split("-")[0] = "zh". So bare "zh" DOES produce a banner on
		 * non-Chinese pages. We assert the revised AC-4.4 behavior. */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/en/login.html",
			"zh",
		);
		const banner = await getBanner(pg);
		H.check(
			"AC-4.4 bare zh on /en/login.html → banner shown",
			!!banner,
			"no banner",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`AC-4.4 banner text is Chinese`,
				text.includes(BANNER_TEXT["zh-CN"]),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * AC-5: click action navigates to same page in target lang,
	 *       query and hash preserved
	 * ========================================================= */
	console.log("--- AC-5: action click navigates correctly ---");
	{
		/* AC-5.1: en-US on /login.html → click Switch → /en/login.html */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		const action = await pg.$(".lang-hint-action");
		H.check("AC-5.1 action link exists", !!action, "no action link");
		if (action) {
			await action.click();
			await pg
				.waitForURL("**/en/login.html", { timeout: 5000 })
				.catch(() => {});
			const finalPath = new URL(pg.url()).pathname;
			H.check(
				`AC-5.1 clicked → /en/login.html`,
				finalPath === "/en/login.html",
				`got ${finalPath}`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-5.2: ja-JP on /en/login.html → click → /ja/login.html */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/en/login.html",
			"ja-JP",
		);
		const action = await pg.$(".lang-hint-action");
		if (action) {
			await action.click();
			await pg
				.waitForURL("**/ja/login.html", { timeout: 5000 })
				.catch(() => {});
			const finalPath = new URL(pg.url()).pathname;
			H.check(
				`AC-5.2 clicked → /ja/login.html`,
				finalPath === "/ja/login.html",
				`got ${finalPath}`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-5.3: en-US on /login.html?foo=bar → click → /en/login.html?foo=bar */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html?foo=bar",
			"en-US",
		);
		const action = await pg.$(".lang-hint-action");
		if (action) {
			await action.click();
			await pg
				.waitForURL(/\/en\/login\.html/, { timeout: 5000 })
				.catch(() => {});
			const u = new URL(pg.url());
			H.check(
				`AC-5.3 query preserved (?foo=bar)`,
				u.searchParams.get("foo") === "bar",
				`search="${u.search}"`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-5.4: es-ES on /ja/login.html?x=1#top → click → /es/login.html?x=1#top */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/ja/login.html?x=1#top",
			"es-ES",
		);
		const action = await pg.$(".lang-hint-action");
		if (action) {
			await action.click();
			await pg
				.waitForURL(/\/es\/login\.html/, { timeout: 5000 })
				.catch(() => {});
			const u = new URL(pg.url());
			H.check(
				`AC-5.4 query preserved (?x=1)`,
				u.searchParams.get("x") === "1",
				`search="${u.search}"`,
			);
			H.check(
				`AC-5.4 hash preserved (#top)`,
				u.hash === "#top",
				`hash="${u.hash}"`,
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * AC-6: close 记住（本机 localStorage）；刷新不再弹；清掉记忆再弹
	 * ========================================================= */
	console.log("--- AC-6: close + 本机记忆 ---");
	{
		/* AC-6.1: show banner, click close → banner gone */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		let banner = await getBanner(pg);
		H.check("AC-6.1 banner shown before close", !!banner, "no banner");
		if (banner) {
			const closeBtn = await pg.$(".lang-hint-close");
			H.check("AC-6.1 close button exists", !!closeBtn, "no close button");
			if (closeBtn) {
				await closeBtn.click();
				/* Wait for close animation + DOM removal */
				await pg.waitForSelector(".lang-hint", { state: "detached", timeout: 5000 }).catch(() => {});
				banner = await getBanner(pg);
				H.check(
					"AC-6.1 banner gone after close",
					!banner,
					"banner still present",
				);
			}
		}
		await ctx.close();
	}
	{
		/* AC-6.2: 关掉后刷新 → 不再弹（这台机器记住了）
		 * AC-6.3: 清掉 localStorage → 又弹（记忆可清） */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		const closeBtn = await pg.$(".lang-hint-close");
		H.check("AC-6.2 前置：先关掉横幅", !!closeBtn, "no close button");
		if (closeBtn) {
			await closeBtn.click();
			await pg.waitForSelector(".lang-hint", { state: "detached", timeout: 5000 }).catch(() => {});
		}
		const stored = await pg.evaluate(() =>
			localStorage.getItem("anotify-lang-hint"),
		);
		H.check(
			"AC-6.2 关闭后把目标语言记进 localStorage",
			!!stored && stored.includes("en"),
			`storage=${stored}`,
		);
		await pg.reload({ waitUntil: "load" });
		await H.waitForAppReady(pg, "login");
		H.check(
			"AC-6.2 刷新后不再弹（同一台电脑）",
			!(await getBanner(pg)),
			"banner reappeared after dismiss",
		);
		/* 清掉记忆 → 回到“从没看过”的状态 */
		await pg.evaluate(() => localStorage.clear());
		await pg.reload({ waitUntil: "load" });
		await H.waitForAppReady(pg, "login");
		H.check(
			"AC-6.3 清掉记忆后重新弹",
			!!(await getBanner(pg)),
			"no banner after clearing storage",
		);
		await ctx.close();
	}
	{
		/* AC-6.4: 记忆是按目标语言记的，不是一个全局静音开关：
		 * 关掉英文提示之后，若浏览器首选语言换成了日文（目标 ja），新的提示仍然出。 */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		await pg.click(".lang-hint-close");
		await pg.waitForSelector(".lang-hint", { state: "detached", timeout: 5000 }).catch(() => {});
		await ctx.addInitScript(() => {
			Object.defineProperty(navigator, "languages", {
				get: () => ["ja-JP", "ja"],
			});
			Object.defineProperty(navigator, "language", { get: () => "ja-JP" });
		});
		await pg.goto(server.base + "/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg, "login");
		const other = await getBanner(pg);
		H.check(
			"AC-6.4 换了一个目标语言（ja）→ 新提示照弹",
			!!other,
			"no banner for a different target language",
		);
		if (other) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				"AC-6.4 新提示文案是日文",
				text.includes(BANNER_TEXT.ja),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}
	{
		/* AC-6.5: 点「切换」= 记下语言习惯（chosen=en）：
		 *   · 目标语言页（/en/）上不弹（已经是他的语言）
		 *   · 回到原语言页（zh）仍提供切回英文（指向 chosen，而不是反复换目标）*/
		const ctx = await browser.newContext({ locale: "en-US" });
		await H.mockExternalCdn(ctx);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg, "login");
		const action = await pg.$(".lang-hint-action");
		H.check("AC-6.5 前置：有切换按钮", !!action, "no action link");
		if (action) {
			await action.click();
			await pg.waitForURL("**/en/login.html", { timeout: 5000 }).catch(() => {});
			await H.waitForAppReady(pg, "login");
			const state = await pg.evaluate(() =>
				JSON.parse(localStorage.getItem("anotify-lang-hint") || "null"),
			);
			H.check(
				"AC-6.5 点切换后记下 chosen=en",
				!!state && state.chosen === "en",
				`storage=${JSON.stringify(state)}`,
			);
			H.check(
				"AC-6.5 目标语言页（/en/）上不弹",
				!(await getBanner(pg)),
				"banner shown on the accepted language page",
			);
			await pg.goto(server.base + "/login.html", { waitUntil: "load" });
			await H.waitForAppReady(pg, "login");
			H.check(
				"AC-6.5 回到原语言页仍提供切回英文（不是默默丢掉选择）",
				!!(await getBanner(pg)),
				"no banner on the original-language page after accepting the switch",
			);
		}
		await ctx.close();
	}
	{
		/* AC-6.6: 用侧栏语言切换器自己选语言 = 语言习惯。
		 * 中文浏览器手动切到英文站后，英文页上**不该**弹“本网站提供简体中文版本”；
		 * 再回到中文页时以他选的英文为准。 */
		const ctx = await browser.newContext({ locale: "zh-CN" });
		await H.mockExternalCdn(ctx);
		const u = new URL(server.base);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seedData.session,
				domain: u.hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/", { waitUntil: "load" });
		await H.waitForAppReady(pg, "workspace");
		H.check(
			"AC-6.6 前置：中文浏览器在中文页 → 无横幅",
			!(await getBanner(pg)),
			"banner on matching-language page",
		);
		/* 打开侧栏语言下拉，点 English */
		await pg.click(".app-sidebar .lang-trigger");
		const enItem = await pg.$(
			'.app-sidebar .lang-menu a[hreflang="en"]',
		);
		H.check("AC-6.6 前置：切换器里有 English", !!enItem, "no en item");
		if (enItem) {
			await enItem.click();
			await pg.waitForURL(/\/en\//, { timeout: 8000 }).catch(() => {});
			await H.waitForAppReady(pg, "workspace");
			const state = await pg.evaluate(() =>
				JSON.parse(localStorage.getItem("anotify-lang-hint") || "null"),
			);
			H.check(
				"AC-6.6 切换器选过的语言被记住",
				!!state && state.chosen === "en",
				`storage=${JSON.stringify(state)}`,
			);
			H.check(
				"AC-6.6 英文页上不弹中文提示（不反向骚扰）",
				!(await getBanner(pg)),
				"banner shown on the language the user picked",
			);
			/* 回中文页：提示以“他选的英文”为准 */
			await pg.goto(server.base + "/", { waitUntil: "load" });
			await H.waitForAppReady(pg, "workspace");
			const back = await getBanner(pg);
			H.check(
				"AC-6.6 回中文页时提示回他选的英文",
				!!back,
				"no banner towards the chosen language",
			);
			if (back) {
				const text = (await pg.textContent(".lang-hint-text")) || "";
				H.check(
					"AC-6.6 提示文案是英文（chosen 而不是浏览器语言）",
					text.includes(BANNER_TEXT.en),
					`got "${text.trim()}"`,
				);
				const innerLang = await pg.getAttribute(".lang-hint-inner", "lang");
				H.check(
					"AC-6.6 提示 lang=en",
					innerLang === "en",
					`got "${innerLang}"`,
				);
			}
		}
		await ctx.close();
	}

	/* =========================================================
	 * AC-8: accessibility
	 * ========================================================= */
	console.log("--- AC-8: accessibility ---");
	{
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		const closeBtn = await pg.$(".lang-hint-close");
		if (closeBtn) {
			const ariaLabel = await closeBtn.getAttribute("aria-label");
			H.check(
				`AC-8.1 close button aria-label is English dismiss`,
				ariaLabel === "Dismiss language hint",
				`got "${ariaLabel}"`,
			);
		} else {
			H.bad("AC-8.1 close button not found");
		}
		/* AC-8.3: keyboard Tab reaches the action link */
		await pg.keyboard.press("Tab");
		const focused = await pg.evaluate(() => {
			const el = document.activeElement;
			return el ? el.className : "";
		});
		H.check(
			`AC-8.3 first Tab focuses banner action link`,
			focused.includes("lang-hint-action"),
			`focused class="${focused}"`,
		);
		await ctx.close();
	}

	/* =========================================================
	 * AC-9: mobile 390 viewport — no horizontal overflow
	 * ========================================================= */
	console.log("--- AC-9: mobile 390 no horizontal overflow ---");
	{
		const ctx = await browser.newContext({
			locale: "en-US",
			viewport: { width: 390, height: 844 },
		});
		await H.mockExternalCdn(ctx);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg, "login");
		const overflow = await pg.evaluate(() => {
			return (
				document.documentElement.scrollWidth -
				document.documentElement.clientWidth
			);
		});
		H.check(
			`AC-9 mobile 390 no horizontal overflow`,
			overflow <= 1,
			`overflow=${overflow}px`,
		);
		/* Verify banner is visible and close/action are clickable on mobile */
		const banner = await getBanner(pg);
		H.check("AC-9 banner visible on mobile", !!banner, "no banner on mobile");
		if (banner) {
			const actionBox = await pg
				.$eval(".lang-hint-action", (el) => {
					const r = el.getBoundingClientRect();
					return {
						w: r.width,
						h: r.height,
						visible: r.width > 0 && r.height > 0,
					};
				})
				.catch(() => null);
			H.check(
				"AC-9 action button visible on mobile",
				actionBox && actionBox.visible,
				"action not visible",
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * Scope: 全站生效（登录页 + 所有工作台页）。
	 * 早期只白名单 login/index，用户反馈“别的页面看不到提示”，
	 * 于 2026-11 改为按语言不匹配判定、不再看页名；骚扰由本机记忆兜着。
	 * ========================================================= */
	console.log("--- scope: banner 全站生效 ---");
	{
		/* keys.html requires auth — inject session */
		const ctx = await browser.newContext({ locale: "en-US" });
		await H.mockExternalCdn(ctx);
		const u = new URL(server.base);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seedData.session,
				domain: u.hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const pg = await ctx.newPage();
		for (const path of ["/keys.html", "/events.html"]) {
			await pg.goto(server.base + path, { waitUntil: "load" });
			await H.waitForAppReady(pg, "workspace");
			const banner = await getBanner(pg);
			H.check(
				`scope ${path} (en-US browser) → banner shown`,
				!!banner,
				`no banner on ${path}`,
			);
			if (banner) {
				const text = (await pg.textContent(".lang-hint-text")) || "";
				H.check(
					`scope ${path} banner text is English`,
					text.includes(BANNER_TEXT.en),
					`got "${text.trim()}"`,
				);
			}
		}
		/* 当前语言页不弹：/en/keys.html + en-US 浏览器 */
		await pg.goto(server.base + "/en/keys.html", { waitUntil: "load" });
		await H.waitForAppReady(pg, "workspace");
		H.check(
			"scope /en/keys.html (en-US browser) → no banner",
			!(await getBanner(pg)),
			"banner shown on matching-language page",
		);
		await ctx.close();
	}

	/* =========================================================
	 * index.html: banner shows for mismatched locale
	 * (requires auth — inject session so page doesn't redirect)
	 * ========================================================= */
	console.log("--- index.html banner for mismatched locale ---");
	{
		const ctx = await browser.newContext({ locale: "ja-JP" });
		await H.mockExternalCdn(ctx);
		const u = new URL(server.base);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seedData.session,
				domain: u.hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/", { waitUntil: "load" });
		await H.waitForAppReady(pg, "workspace");
		const banner = await getBanner(pg);
		H.check(
			"index.html ja-JP browser → banner shown",
			!!banner,
			"no banner on index.html",
		);
		if (banner) {
			const text = (await pg.textContent(".lang-hint-text")) || "";
			H.check(
				`index.html banner text is Japanese`,
				text.includes(BANNER_TEXT.ja),
				`got "${text.trim()}"`,
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * AC-10 (regression, user-reported): multi-entry language list
	 * must NOT skip the current-page match to suggest a lower-
	 * priority language. Real browsers send e.g. ["zh-CN","en"] —
	 * the FIRST resolvable preference decides.
	 * ========================================================= */
	console.log("--- AC-10: multi-entry navigator.languages regression ---");
	{
		/* AC-10.1: prefs [zh-CN, en] on the zh-CN page → no banner
		 * (previously: zh-CN was skipped as "matches current" and en was
		 * suggested — the exact false-positive the user reported). */
		const ctx1 = await browser.newContext({ locale: "zh-CN" });
		await H.mockExternalCdn(ctx1);
		await ctx1.addInitScript(() => {
			Object.defineProperty(navigator, "languages", {
				get: () => ["zh-CN", "zh", "en"],
			});
		});
		const pg1 = await ctx1.newPage();
		await pg1.goto(server.base + "/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg1, "login");
		const banner1 = await getBanner(pg1);
		H.check(
			"AC-10.1 prefs [zh-CN,zh,en] on /login.html → no banner",
			!banner1,
			banner1 ? "banner shown (false positive regression)" : "",
		);
		await ctx1.close();

		/* AC-10.2: prefs [zh-CN, en] on the ENGLISH page → Chinese banner
		 * (first preference zh-CN ≠ current en → hint is correct). */
		const ctx2 = await browser.newContext({ locale: "zh-CN" });
		await H.mockExternalCdn(ctx2);
		await ctx2.addInitScript(() => {
			Object.defineProperty(navigator, "languages", {
				get: () => ["zh-CN", "zh", "en"],
			});
		});
		const pg2 = await ctx2.newPage();
		await pg2.goto(server.base + "/en/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg2, "login");
		const banner2 = await getBanner(pg2);
		H.check(
			"AC-10.2 prefs [zh-CN,zh,en] on /en/login.html → Chinese banner",
			!!banner2,
			"no banner (expected Chinese hint)",
		);
		if (banner2) {
			const text = (await pg2.textContent(".lang-hint-text")) || "";
			H.check(
				"AC-10.2 banner text is Chinese",
				text.includes(BANNER_TEXT["zh-CN"]),
				`got "${text.trim()}"`,
			);
		}
		await ctx2.close();

		/* AC-10.3: prefs [fr, ja] on the zh-CN page → Japanese banner
		 * (first UNSUPPORTED entry is skipped; first SUPPORTED wins). */
		const ctx3 = await browser.newContext({ locale: "fr-FR" });
		await H.mockExternalCdn(ctx3);
		await ctx3.addInitScript(() => {
			Object.defineProperty(navigator, "languages", {
				get: () => ["fr-FR", "fr", "ja"],
			});
		});
		const pg3 = await ctx3.newPage();
		await pg3.goto(server.base + "/login.html", { waitUntil: "load" });
		await H.waitForAppReady(pg3, "login");
		const banner3 = await getBanner(pg3);
		H.check(
			"AC-10.3 prefs [fr,ja] on /login.html → Japanese banner (skip unsupported)",
			!!banner3,
			"no banner (expected Japanese hint)",
		);
		if (banner3) {
			const text = (await pg3.textContent(".lang-hint-text")) || "";
			H.check(
				"AC-10.3 banner text is Japanese",
				text.includes(BANNER_TEXT.ja),
				`got "${text.trim()}"`,
			);
		}
		await ctx3.close();
	}

	/* =========================================================
	 * AC-11 (regression, user-reported): 语言提示条滚出视口后，侧栏
	 * 必须跟着回到视口顶部，与 sticky 顶栏对齐。
	 * 旧实现把 --shell-top-offset 写死成横幅高度：下滑之后侧栏顶部
	 * 永远比顶栏低一条横幅（那条 45px 空白），高度也短一条。
	 * ========================================================= */
	console.log("--- AC-11: sidebar realigns after banner scrolls away ---");
	{
		const ctx = await browser.newContext({
			locale: "ja-JP",
			viewport: { width: 1440, height: 900 },
		});
		await H.mockExternalCdn(ctx);
		const u = new URL(server.base);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seedData.session,
				domain: u.hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/", { waitUntil: "load" });
		await H.waitForAppReady(pg, "workspace");

		const geo = () =>
			pg.evaluate(() => {
				const rect = (sel) => {
					const el = document.querySelector(sel);
					if (!el) return null;
					const r = el.getBoundingClientRect();
					const round = (v) => Math.round(v * 10) / 10;
					return { top: round(r.top), height: round(r.height), bottom: round(r.bottom) };
				};
				return {
					vh: window.innerHeight,
					scrollY: Math.round(window.scrollY),
					maxScroll:
						document.documentElement.scrollHeight - window.innerHeight,
					offset: getComputedStyle(document.documentElement)
						.getPropertyValue("--shell-top-offset")
						.trim(),
					banner: rect(".lang-hint"),
					sidebar: rect(".app-sidebar"),
					topbar: rect(".app-topbar"),
				};
			});

		/* 滚到 y 后等 --shell-top-offset 收敛到「应得值」：变量是 rAF 里写的，
		 * 直接量几何会抢在回调之前。返回实际 scrollY（可能被文档高度截断）。 */
		const scrollAndSettle = async (y, bannerH) => {
			await pg.evaluate((yy) => window.scrollTo(0, yy), y);
			const actual = await pg.evaluate(
				() =>
					new Promise((res) =>
						requestAnimationFrame(() =>
							requestAnimationFrame(() => res(Math.round(window.scrollY))),
						),
					),
			);
			const raw = bannerH - actual;
			const want = raw <= 0.01 ? 0 : Math.round(raw * 100) / 100;
			await pg.waitForFunction(
				(expect) =>
					getComputedStyle(document.documentElement)
						.getPropertyValue("--shell-top-offset")
						.trim() === expect,
				want + "px",
				{ timeout: 4000 },
			);
			return actual;
		};

		const at0 = await geo();
		H.check(
			"AC-11 前置：横幅、侧栏、顶栏都在",
			!!(at0.banner && at0.sidebar && at0.topbar),
			JSON.stringify(at0),
		);
		if (at0.banner && at0.sidebar && at0.topbar) {
			const bh = at0.banner.height;
			H.check(
				"AC-11 未滚动：侧栏顶在横幅之后（流内原位）",
				Math.abs(at0.sidebar.top - bh) <= 1,
				`sidebar.top=${at0.sidebar.top} bannerH=${bh}`,
			);
			H.check(
				"AC-11 未滚动：侧栏底边贴住视口底（F8 不复发）",
				Math.abs(at0.sidebar.bottom - at0.vh) <= 1,
				`sidebar.bottom=${at0.sidebar.bottom} vh=${at0.vh}`,
			);

			/* 半程：顶部必须跟顶栏一起上移，不能“钉”在横幅高度上 */
			const midY = await scrollAndSettle(Math.round(bh / 2), bh);
			const mid = await geo();
			H.check(
				"AC-11 滚到横幅半高：侧栏顶部与顶栏对齐（不滞后）",
				Math.abs(mid.sidebar.top - mid.topbar.top) <= 1,
				`scrollY=${midY} sidebar.top=${mid.sidebar.top} topbar.top=${mid.topbar.top}`,
			);
			H.check(
				"AC-11 滚到横幅半高：侧栏底边仍贴视口底",
				Math.abs(mid.sidebar.bottom - mid.vh) <= 1,
				`sidebar.bottom=${mid.sidebar.bottom} vh=${mid.vh}`,
			);

			/* 完全滚过横幅：偏移归零，侧栏回到 0 且满高 */
			H.check(
				"AC-11 前置：首页可滚动超过横幅高度",
				at0.maxScroll >= bh,
				`maxScroll=${at0.maxScroll} bannerH=${bh}`,
			);
			const farY = await scrollAndSettle(Math.round(bh) + 120, bh);
			const far = await geo();
			H.check(
				"AC-11 滚过横幅：--shell-top-offset 归零",
				far.offset === "0px",
				`scrollY=${farY} offset=${far.offset}`,
			);
			H.check(
				"AC-11 滚过横幅：侧栏顶部与顶栏同高（都是 0）",
				Math.abs(far.sidebar.top) <= 1 &&
					Math.abs(far.sidebar.top - far.topbar.top) <= 1,
				`scrollY=${farY} sidebar.top=${far.sidebar.top} topbar.top=${far.topbar.top}`,
			);
			H.check(
				"AC-11 滚过横幅：侧栏恢复满高（= 视口高）",
				Math.abs(far.sidebar.height - far.vh) <= 1 &&
					Math.abs(far.sidebar.bottom - far.vh) <= 1,
				`sidebar.height=${far.sidebar.height} vh=${far.vh} bottom=${far.sidebar.bottom}`,
			);
		}
		await ctx.close();
	}

	/* =========================================================
	 * Cleanup
	 * ========================================================= */
	await browser.close();
	server.stop();
	return H.summary("lang_hint");
}

main()
	.then((ok) => process.exit(ok ? 0 : 1))
	.catch((e) => {
		console.error("FATAL:", e);
		if (browser) browser.close();
		if (server) server.stop();
		process.exit(1);
	});
