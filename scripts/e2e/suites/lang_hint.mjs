#!/usr/bin/env node
/* SUITE: lang_hint — language hint banner (progressive enhancement)
 *
 * Verifies the language hint banner per lang-hint-requirements.md §8 ACs.
 * The banner is pure client-side: it detects navigator.language mismatch
 * and shows a dismissible top bar linking to the same page in the detected
 * language. No redirect, no storage, no build-time DOM.
 *
 * AC coverage:
 *   AC-1   matching language → no banner (zh-CN browser on zh-CN page, etc.)
 *   AC-2   mismatch + supported → banner shown in target language text
 *   AC-3   unsupported language (fr, ko) → no banner
 *   AC-4   traditional Chinese prefix-match (zh-TW → zh-CN target per coordinator ruling)
 *   AC-5   click action navigates to same page in target lang, query+hash preserved
 *   AC-6   close button dismisses; refresh re-shows (no storage)
 *   AC-7   build-time HTML has zero banner markup (crawler/SEO safe)
 *   AC-9   mobile 390 viewport: no horizontal overflow
 *   scope  only login.html + index.html show banner; keys.html does not
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
	const pg = await ctx.newPage();
	const errors = [];
	pg.on("pageerror", (e) => errors.push(String(e)));
	await pg.goto(base + path, { waitUntil: "load" });
	/* Give the banner a moment to mount (rAF animation) */
	await pg.waitForTimeout(300);
	return { ctx, pg, errors };
}

let server, browser;

async function main() {
	console.log("=== SUITE: lang_hint (language hint banner) ===");
	server = await H.startServer({ rpId: RP });
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
	 * AC-6: close dismisses; refresh re-shows (no storage)
	 * ========================================================= */
	console.log("--- AC-6: close + refresh (no storage) ---");
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
				/* Wait for close animation + removal */
				await pg.waitForTimeout(350);
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
		/* AC-6.2: after refresh, banner reappears (no storage) */
		const { pg, ctx } = await openWithLocale(
			browser,
			server.base,
			"/login.html",
			"en-US",
		);
		/* Close it first */
		const closeBtn = await pg.$(".lang-hint-close");
		if (closeBtn) {
			await closeBtn.click();
			await pg.waitForTimeout(350);
		}
		/* Reload */
		await pg.reload({ waitUntil: "load" });
		await pg.waitForTimeout(300);
		const banner = await getBanner(pg);
		H.check(
			"AC-6.2 banner reappears after refresh (no storage)",
			!!banner,
			"no banner after refresh",
		);
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
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/login.html", { waitUntil: "load" });
		await pg.waitForTimeout(400);
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
	 * Scope: only login.html + index.html show banner;
	 *        keys.html and other workspace pages do not
	 * ========================================================= */
	console.log("--- scope: only login + index pages show banner ---");
	{
		/* keys.html requires auth — inject session */
		const ctx = await browser.newContext({ locale: "en-US" });
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
		await pg.goto(server.base + "/keys.html", { waitUntil: "load" });
		await pg.waitForTimeout(400);
		const banner = await getBanner(pg);
		H.check(
			"scope keys.html (en-US browser) → no banner",
			!banner,
			"banner found on keys.html",
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
		await pg.waitForTimeout(500);
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
