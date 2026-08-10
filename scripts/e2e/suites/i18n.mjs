#!/usr/bin/env node
/* SUITE: i18n — multi-language internationalization verification
 *
 * Covers the acceptance criteria from i18n-requirements.md §6:
 *   AC-1.1  4 langs × 7 pages all accessible (200), html lang correct, i18n.{lang}.js loadable
 *   AC-1.2  no raw dotted-keys leaked into rendered HTML (regex scan of i18n key patterns)
 *   AC-2.1  head hreflang = 5 tags (zh-CN/en/ja/es/x-default), identical across lang versions
 *   AC-2.2  hreflang links point to real pages (200, no 404)
 *   AC-2.3  no hardcoded external domain in hreflang href
 *   AC-3.1  every page (28) has a language switcher with 4 language options
 *   AC-3.2  current language has visible selected state (aria-current / class)
 *   AC-3.3  cross-language navigation URL derivation (en/keys → ja/keys → keys)
 *   AC-3.4  query string preserved on switch
 *   AC-3.5  page text actually changes per language (nav labels in 4 langs)
 *   AC-3.6  switcher usable at mobile viewport (390), no horizontal overflow
 *   AC-4.1  /en/login.html demo mode enters → lands on /en/index.html (language preserved)
 *   AC-5.1  README.md + README.en.md exist, top cross-links, same H2 count
 *   AC-5.2  README.en.md relative links valid
 *   AC-7.3  28 pages: no console JS errors, no horizontal overflow (desktop 1280)
 *
 * Red line: this suite reports product bugs; it never weakens assertions to pass.
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import { readFileSync, existsSync } from "node:fs";
import path from "node:path";

const RP = "localhost";
const LANGS = ["zh-CN", "en", "ja", "es"];
/* default lang (first) is at root; others at /{lang}/ */
const PAGES = [
	"index.html",
	"login.html",
	"receivers.html",
	"keys.html",
	"security.html",
	"docs.html",
	"message.html",
	"connect.html",
];
/* Pages that require authentication (will redirect to login without session) */
const GUARDED = [
	"index.html",
	"receivers.html",
	"keys.html",
	"security.html",
	"message.html",
	"connect.html",
];

/* Map lang → URL prefix. Default lang = "" (root). */
function langPrefix(lang) {
	return lang === LANGS[0] ? "" : "/" + lang;
}

/* Build path for a page in a given language (relative to server base). */
function pagePath(lang, page) {
	const prefix = langPrefix(lang);
	return `${prefix}/${page}`;
}

/* Inject session cookie into context (for accessing guarded pages). */
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

/* Resolve a page path to its served URL, handling index.html → directory.
 * FileServer redirects /index.html → ./ (301). We request the directory
 * path directly to get 200 + the HTML content.
 * Returns { path, isRedirect } where path is what to fetch. */
function servedPath(lang, page) {
	const prefix = langPrefix(lang);
	if (page === "index.html") {
		return { path: prefix + "/", isDir: true };
	}
	return { path: prefix + "/" + page, isDir: false };
}

let server, browser;

async function main() {
	H.startTimer();
	console.log("=== SUITE: i18n (multi-language internationalization) ===");
	server = await H.startServer({ suiteName: "i18n", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	/* Seed a user for accessing guarded pages */
	const seedData = H.seed(server.dbPath, "i18n_tester");

	/* =========================================================
	 * AC-1.1 / AC-1.3: 4 langs × 7 pages accessible, html lang correct
	 * AC-1.2: no raw dotted-keys in rendered HTML
	 * （合并：一次 HTTP fetch 同时做 AC-1.1 + AC-1.3 + AC-1.2）
	 * ========================================================= */
	console.log("--- AC-1.1/1.3: page accessibility + html lang ---");
	console.log("--- AC-1.2: no raw dotted-keys leaked ---");
	/* Patterns: match known i18n key prefixes as standalone text (not in URLs) */
	const i18nKeyPatterns = [
		/\bcommon\.nav\.\w+\b/g,
		/\bcommon\.lang\.\w+\b/g,
		/\bcommon\.brand\.\w+\b/g,
		/\bcommon\.footer\.\w+\b/g,
		/\bcommon\.sidebar\.\w+\b/g,
		/\bindex\.(greeting|title|subtitle|loading|demo_badge|heatmap|kpi|quickstart|recent)\w*/g,
		/\bkeys\.(create|save|security|col|desc|header|my|title|subtitle|view|api)\w*/g,
		/\blogin\.(login|signup|status|tab|title|subtitle|trust|welcome|ios|recovery)\w*/g,
		/\breceivers\.(pair|push|ws|channel|demo|tags|title)\w*/g,
		/\bsecurity\.(add|concept|intro|passkey|recovery|session|title|subtitle)\w*/g,
		/\bmessage\.(body|deliveries|fields|loading|open|title)\w*/g,
		/\bdocs\.(title|subtitle|toc)\w*/g,
		/\bconnect\.(title|subtitle|status|health|step|step1|step2|step3|alt|journey|help)\w*/g,
	];
	for (const lang of LANGS) {
		for (const page of PAGES) {
			const { path } = servedPath(lang, page);
			const r = await H.req(server.base, path);
			const expectedLang = lang;
			H.check(
				`AC-1.1 ${lang}/${page} accessible (200)`,
				r.status === 200,
				`status=${r.status}`,
			);
			/* Extract <html lang="..."> */
			const m = r.text.match(/<html\s+lang="([^"]*)"/);
			H.check(
				`AC-1.3 ${lang}/${page} html lang="${expectedLang}"`,
				m && m[1] === expectedLang,
				`got "${m?.[1]}"`,
			);
			/* AC-1.2: no raw dotted-keys（复用同一个 r.text） */
			let stripped = r.text.replace(/(href|src)="[^"]*"/g, '$1=""');
			stripped = stripped.replace(/<script\b[\s\S]*?<\/script>/gi, "");
			const leaked = [];
			for (const re of i18nKeyPatterns) {
				const matches = stripped.match(re);
				if (matches) leaked.push(...matches);
			}
			H.check(
				`AC-1.2 ${lang}/${page} no raw dotted-keys`,
				leaked.length === 0,
				leaked.slice(0, 3).join(", "),
			);
		}
	}

	/* i18n.{lang}.js loadable — the server serves hashed files
	 * (e.g. i18n.zh-CN.104d7971.js), so we parse the <script> tag from HTML */
	console.log("--- AC-1.1: i18n.{lang}.js loadable ---");
	for (const lang of LANGS) {
		/* Fetch a page to find the hashed i18n script tag */
		const pageR = await H.req(server.base, langPrefix(lang) + "/keys.html");
		const scriptMatch = pageR.text.match(
			new RegExp(`src="(/i18n\\.${lang}\\.[a-f0-9]+\\.js)"`),
		);
		H.check(
			`AC-1.1 i18n.${lang}.js script tag found in HTML`,
			!!scriptMatch,
			"no script tag",
		);
		if (scriptMatch) {
			const hashedPath = scriptMatch[1];
			const r = await H.req(server.base, hashedPath);
			H.check(
				`AC-1.1 i18n.${lang}.js (hashed) accessible (200)`,
				r.status === 200,
				`status=${r.status}`,
			);
			H.check(
				`AC-1.1 i18n.${lang}.js has window.AnotifyI18n`,
				r.text.includes("window.AnotifyI18n"),
				"missing window.AnotifyI18n",
			);
			const m = r.text.match(/window\.AnotifyI18n\s*=\s*(\{.*\})/s);
			H.check(
				`AC-1.1 i18n.${lang}.js JSON parseable`,
				!!m &&
					(() => {
						try {
							JSON.parse(m[1]);
							return true;
						} catch {
							return false;
						}
					})(),
				m ? "parse failed" : "no match",
			);
		}
	}

	/* =========================================================
	 * AC-2.1: hreflang = 5 tags, identical across lang versions
	 * ========================================================= */
	console.log("--- AC-2.1: hreflang tags ---");
	const hreflangSamplePages = ["keys.html", "receivers.html", "login.html"];
	for (const page of hreflangSamplePages) {
		/* Collect hreflang sets from all 4 lang versions */
		const sets = {};
		for (const lang of LANGS) {
			const { path } = servedPath(lang, page);
			const r = await H.req(server.base, path);
			const tags = [];
			const re =
				/<link\s+rel="alternate"\s+hreflang="([^"]*)"\s+href="([^"]*)"\s*\/>/g;
			let m;
			while ((m = re.exec(r.text)) !== null) {
				tags.push(`${m[1]}:${m[2]}`);
			}
			sets[lang] = tags;
		}
		/* Each version should have exactly 5 tags */
		for (const lang of LANGS) {
			H.check(
				`AC-2.1 ${lang}/${page} has 5 hreflang tags`,
				sets[lang].length === 5,
				`got ${sets[lang].length}`,
			);
		}
		/* All 4 versions should have identical tag sets */
		const ref = JSON.stringify(sets[LANGS[0]].sort());
		const allSame = LANGS.every((l) => JSON.stringify(sets[l].sort()) === ref);
		H.check(
			`AC-2.1 ${page} hreflang sets identical across 4 langs`,
			allSame,
			"sets differ",
		);
		/* Verify the 5 expected hreflang values are present */
		const codes = sets[LANGS[0]].map((t) => t.split(":")[0]).sort();
		H.eq(
			`AC-2.1 ${page} hreflang codes`,
			codes.join(","),
			["en", "es", "ja", "x-default", "zh-CN"].join(","),
		);
	}

	/* =========================================================
	 * AC-2.2: hreflang links point to real pages (200)
	 * AC-2.3: no hardcoded external domain in hreflang
	 * ========================================================= */
	console.log("--- AC-2.2/2.3: hreflang links valid + no external domain ---");
	{
		const r = await H.req(server.base, pagePath("en", "keys.html"));
		const re =
			/<link\s+rel="alternate"\s+hreflang="[^"]*"\s+href="([^"]*)"\s*\/>/g;
		let m;
		while ((m = re.exec(r.text)) !== null) {
			const href = m[1];
			/* AC-2.3: no external domain (should be relative paths like /keys.html) */
			H.check(
				`AC-2.3 hreflang href="${href}" is relative (no external domain)`,
				href.startsWith("/") && !href.includes("://"),
				"contains external domain",
			);
			/* AC-2.2: the page exists (200 or 301 for index.html) */
			const pr = await H.req(server.base, href);
			H.check(
				`AC-2.2 hreflang "${href}" returns 200 or 301`,
				pr.status === 200 || pr.status === 301,
				`status=${pr.status}`,
			);
		}
	}

	/* =========================================================
	 * AC-3.1/3.2: switcher exists + current lang selected state
	 * AC-7.3: no JS errors + no horizontal overflow (desktop 1280)
	 * （合并：28 页只遍历 1 遍，每 lang 1 ctx，内层 page 复用）
	 * ========================================================= */
	console.log("--- AC-3.1/3.2: switcher presence + selected state ---");
	console.log("--- AC-7.3: 28 pages desktop overflow check (merged) ---");
	for (const lang of LANGS) {
		const ctx = await browser.newContext();
		/* Inject session for guarded pages so they don't redirect to login */
		await injectSession(ctx, seedData.session, server.base);
		const pg = await ctx.newPage();
		await pg.setViewportSize({ width: 1280, height: 800 });
		for (const page of PAGES) {
			const pageErrors = [];
			pg.removeAllListeners("pageerror");
			pg.on("pageerror", (e) => pageErrors.push(String(e)));
			await pg.goto(server.base + pagePath(lang, page), {
				waitUntil: "load",
				timeout: 15000,
			});
			const pageType = page === "login.html" ? "login" : "workspace";
			await H.waitForAppReady(pg, pageType);

			/* AC-3.1: switcher exists with 4 language options.
			 * Sidebar pages: switcher built by JS (buildLangSwitcher → #lang-switcher).
			 * Login pages: dropdown enhanced from build-time links (#lang-switcher-login). */
			const switcherInfo = await pg.evaluate(() => {
				/* Sidebar switcher (JS-built dropdown) */
				const sidebar = document.querySelector("#lang-switcher");
				if (sidebar) {
					const items = sidebar.querySelectorAll("a[hreflang]");
					return {
						type: "sidebar",
						count: items.length,
						current: !!sidebar.querySelector('[aria-current="true"]'),
					};
				}
				/* Login page switcher (build-time flat links) */
				const loginNav = document.querySelector("#lang-switcher-login");
				if (loginNav) {
					const items = loginNav.querySelectorAll("a[hreflang]");
					return {
						type: "login",
						count: items.length,
						current: !!loginNav.querySelector('[aria-current="true"]'),
					};
				}
				return { type: "none", count: 0, current: false };
			});

			H.check(
				`AC-3.1 ${lang}/${page} has switcher`,
				switcherInfo.type !== "none",
				"no switcher found",
			);
			H.check(
				`AC-3.1 ${lang}/${page} switcher has 4 options`,
				switcherInfo.count === 4,
				`got ${switcherInfo.count} (${switcherInfo.type})`,
			);
			H.check(
				`AC-3.2 ${lang}/${page} current lang has selected state`,
				switcherInfo.current,
				"no aria-current=true",
			);

			/* AC-7.3: no JS errors on each page */
			H.check(
				`AC-7.3 ${lang}/${page} no console JS errors`,
				pageErrors.length === 0,
				pageErrors[0]?.slice(0, 100),
			);

			/* AC-7.3: no horizontal overflow (desktop 1280) */
			const overflowCount = await pg.evaluate(() => {
				const vw = window.innerWidth;
				let n = 0;
				for (const el of document.querySelectorAll("body *")) {
					const r = el.getBoundingClientRect();
					if (
						r.right > vw + 5 &&
						!el.closest("pre") &&
						!el.closest(".overflow-x-auto") &&
						!el.closest(".code-body") &&
						!el.closest("code")
					)
						n++;
				}
				return n;
			});
			H.check(
				`AC-7.3 desktop ${lang}/${page} no horizontal overflow`,
				overflowCount === 0,
				`${overflowCount} elements overflow`,
			);
		}
		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-3.3: cross-language URL derivation
	 * Uses authenticated session to access guarded pages.
	 * ========================================================= */
	console.log("--- AC-3.3: cross-language URL derivation ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, seedData.session, server.base);
		const pg = await ctx.newPage();
		await pg.setViewportSize({ width: 1280, height: 800 });

		/* /en/keys.html → open sidebar dropdown → click 日本語 → /ja/keys.html */
		await pg.goto(server.base + "/en/keys.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(pg, "workspace");
		/* Open the sidebar dropdown */
		const trigger = await pg.$("#lang-switcher button");
		H.check(
			"AC-3.3 en/keys sidebar switcher trigger exists",
			!!trigger,
			"no trigger button",
		);
		if (trigger) {
			await trigger.click();
			await pg.waitForSelector('#lang-switcher a[hreflang="ja"]', {
				state: "visible",
				timeout: 5000,
			});
		}
		/* Find the ja link in the now-open dropdown */
		const jaLink = await pg.$('#lang-switcher a[hreflang="ja"]');
		H.check(
			"AC-3.3 en/keys → ja link exists in dropdown",
			!!jaLink,
			"no ja link after opening dropdown",
		);
		if (jaLink) {
			await jaLink.click();
			await pg.waitForURL("**/ja/keys.html", { timeout: 8000 });
			const finalUrl = pg.url();
			H.check(
				"AC-3.3 en/keys → ja lands on /ja/keys.html",
				finalUrl.includes("/ja/keys.html"),
				`got ${finalUrl}`,
			);
		}

		/* /ja/login.html → open dropdown → click 中文 → /login.html (root, default lang) */
		await pg.goto(server.base + "/ja/login.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(pg, "login");
		/* Login page switcher is now a dropdown (v1.1); open it first */
		const loginTrigger = await pg.$(
			"#lang-switcher-login button[aria-haspopup]",
		);
		if (loginTrigger) {
			await loginTrigger.click();
			await pg.waitForSelector('#lang-switcher-login a[hreflang="zh-CN"]', {
				state: "visible",
				timeout: 5000,
			});
		}
		const zhLink = await pg.$('#lang-switcher-login a[hreflang="zh-CN"]');
		H.check("AC-3.3 ja/login → zh link exists", !!zhLink, "no zh link");
		if (zhLink) {
			await zhLink.click();
			await pg.waitForURL("**/login.html*", { timeout: 8000 });
			const finalUrl = pg.url();
			/* Should be /login.html (default lang = root, no prefix) */
			H.check(
				"AC-3.3 ja/login → zh lands on /login.html (root)",
				finalUrl.endsWith("/login.html") &&
					!finalUrl.includes("/en/") &&
					!finalUrl.includes("/ja/") &&
					!finalUrl.includes("/es/"),
				`got ${finalUrl}`,
			);
		}

		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-3.4: query string preserved on switch
	 * ========================================================= */
	console.log("--- AC-3.4: query string preserved ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, seedData.session, server.base);
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/en/receivers.html?msg=ntf_test1", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(pg, "workspace");
		/* Open dropdown and click ES */
		const trigger = await pg.$("#lang-switcher button");
		if (trigger) {
			await trigger.click();
			await pg.waitForSelector('#lang-switcher a[hreflang="es"]', {
				state: "visible",
				timeout: 5000,
			});
		}
		const esLink = await pg.$('#lang-switcher a[hreflang="es"]');
		H.check(
			"AC-3.4 en/receivers?msg= → es link exists",
			!!esLink,
			"no es link after opening dropdown",
		);
		if (esLink) {
			await esLink.click();
			await pg.waitForURL("**/es/receivers.html*", { timeout: 8000 });
			const finalUrl = pg.url();
			H.check(
				"AC-3.4 switch to es preserves ?msg=ntf_test1",
				finalUrl.includes("/es/receivers.html") &&
					finalUrl.includes("msg=ntf_test1"),
				`got ${finalUrl}`,
			);
		}
		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-3.5: page text changes per language
	 * Read the i18n.{lang}.js content to verify nav labels differ.
	 * Also verify in-browser that the sidebar nav text changes.
	 * ========================================================= */
	console.log("--- AC-3.5: text changes per language ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, seedData.session, server.base);
		const pg = await ctx.newPage();
		const navTexts = {};
		for (const lang of LANGS) {
			await pg.goto(server.base + pagePath(lang, "index.html"), {
				waitUntil: "load",
				timeout: 15000,
			});
			await H.waitForAppReady(pg, "workspace");
			/* Get all sidebar nav item texts, excluding the language switcher.
			 * The nav items are .side-link elements inside #sidebar nav. */
			const navText = await pg.evaluate(() => {
				const nav = document.querySelector("#sidebar nav");
				if (!nav) return "";
				const links = nav.querySelectorAll("a.side-link");
				const texts = [...links]
					.map((a) => (a.textContent || "").trim())
					.filter((t) => t.length > 0);
				return texts.join(" | ");
			});
			navTexts[lang] = navText;
		}
		/* All 4 should be different (different languages) */
		const unique = new Set(Object.values(navTexts));
		H.check(
			"AC-3.5 nav text differs across 4 languages",
			unique.size === 4,
			`only ${unique.size} unique: ${JSON.stringify(navTexts)}`,
		);
		/* Spot-check known values from locale YAMLs */
		H.check(
			'AC-3.5 zh-CN nav contains "总览"',
			navTexts["zh-CN"].includes("总览"),
			`got "${navTexts["zh-CN"]}"`,
		);
		H.check(
			'AC-3.5 en nav contains "Overview"',
			navTexts["en"].includes("Overview"),
			`got "${navTexts["en"]}"`,
		);
		H.check(
			'AC-3.5 ja nav contains "概要"',
			navTexts["ja"].includes("概要"),
			`got "${navTexts["ja"]}"`,
		);
		H.check(
			'AC-3.5 es nav contains "Resumen"',
			navTexts["es"].includes("Resumen"),
			`got "${navTexts["es"]}"`,
		);
		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-3.6: mobile viewport switcher usable, no overflow
	 * ========================================================= */
	console.log("--- AC-3.6: mobile viewport (390) ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, seedData.session, server.base);
		const pg = await ctx.newPage();
		await pg.setViewportSize({ width: 390, height: 844 });
		await pg.goto(server.base + "/en/keys.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(pg, "workspace");

		/* On mobile the sidebar is hidden behind a hamburger menu.
		 * Open the menu first, then the lang switcher. */
		const menuBtn = await pg.$(
			'button[aria-label="Menu"], button[aria-label*="菜单"], #menu-btn, [aria-label*="menu"]',
		);
		if (menuBtn) {
			try {
				await menuBtn.click();
				await pg.waitForSelector("#sidebar nav a.side-link, #lang-switcher", {
					timeout: 5000,
				});
			} catch {
				/* menu may already be open */
			}
		}

		/* Try to open the lang switcher dropdown */
		const trigger = await pg.$("#lang-switcher button");
		if (trigger) {
			await trigger.click();
			await pg.waitForSelector('#lang-switcher a[hreflang="ja"]', {
				state: "visible",
				timeout: 5000,
			});
		}
		/* Check a language link is clickable */
		const jaLink = await pg.$('#lang-switcher a[hreflang="ja"]');
		H.check(
			"AC-3.6 mobile: switcher dropdown openable + ja link present",
			!!jaLink,
			"no ja link on mobile",
		);

		/* Check no horizontal overflow at 390px */
		const overflowCount = await pg.evaluate(() => {
			const vw = window.innerWidth;
			let n = 0;
			for (const el of document.querySelectorAll("body *")) {
				const r = el.getBoundingClientRect();
				if (
					r.right > vw + 5 &&
					!el.closest("pre") &&
					!el.closest(".overflow-x-auto") &&
					!el.closest("code")
				)
					n++;
			}
			return n;
		});
		H.check(
			"AC-3.6 mobile: no horizontal overflow at 390px",
			overflowCount === 0,
			`${overflowCount} elements overflow`,
		);

		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-4.1: login demo mode preserves language
	 * demoEnter() uses relative "index.html?demo=1&u=..." which resolves
	 * to /{lang}/index.html when on /{lang}/login.html.
	 * When backend IS connected (E2E), index.html hits 401 → auth guard
	 * redirects to "login.html?next=..." (also relative → stays in /{lang}/).
	 * Both paths preserve language via relative URLs.
	 * We verify: (a) demoEnter source uses relative path, (b) auth guard
	 * redirect also uses relative path, (c) actual redirect stays in /en/.
	 * ========================================================= */
	console.log("--- AC-4.1: login demo mode language preservation ---");
	{
		/* (a) Verify demoEnter source uses relative path ("index.html" not "/index.html") */
		const loginR = await H.req(server.base, "/en/login.html");
		H.check(
			'AC-4.1 demoEnter uses relative path "index.html" (not absolute)',
			loginR.text.includes('"index.html?demo=1') &&
				!loginR.text.includes('"/index.html?demo=1'),
			"demoEnter uses absolute path or not found",
		);

		/* (b) Verify auth guard in partials.js uses relative "login.html" */
		/* The hashed partials.js is loaded by the page; fetch it from the script tag */
		const partialsMatch = loginR.text.match(/src="([^"]*partials[^"]*\.js)"/);
		H.check(
			"AC-4.1 partials.js script tag found",
			!!partialsMatch,
			"no partials.js script tag",
		);
		if (partialsMatch) {
			const pR = await H.req(server.base, partialsMatch[1]);
			H.check(
				'AC-4.1 auth guard uses relative "login.html" (not absolute)',
				pR.text.includes("login.html?next=") &&
					!pR.text.includes('"/login.html?next='),
				"auth guard uses absolute path",
			);
		}

		/* (c) Actual redirect test: navigate to /en/index.html (guarded),
		 * verify it redirects to /en/login.html (language preserved) */
		const ctx = await browser.newContext();
		const pg = await ctx.newPage();
		await pg.goto(server.base + "/en/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await pg.waitForURL("**/en/login.html*", { timeout: 8000 });
		const finalUrl = pg.url();
		H.check(
			"AC-4.1 /en/index.html (guarded) → redirects to /en/login.html",
			finalUrl.includes("/en/login.html"),
			`got ${finalUrl}`,
		);

		/* Also test /ja/ path */
		await pg.goto(server.base + "/ja/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await pg.waitForURL("**/ja/login.html*", { timeout: 8000 });
		const finalUrlJa = pg.url();
		H.check(
			"AC-4.1 /ja/index.html (guarded) → redirects to /ja/login.html",
			finalUrlJa.includes("/ja/login.html"),
			`got ${finalUrlJa}`,
		);

		await pg.close();
		await ctx.close();
	}

	/* =========================================================
	 * AC-5.1/5.2: README bilingual
	 * ========================================================= */
	console.log("--- AC-5.1/5.2: README bilingual ---");
	const rootDir = H.ROOT_DIR;
	const readmeZh = path.join(rootDir, "README.md");
	const readmeEn = path.join(rootDir, "README.en.md");
	H.check("AC-5.1 README.md exists", existsSync(readmeZh));
	H.check("AC-5.1 README.en.md exists", existsSync(readmeEn));

	if (existsSync(readmeZh) && existsSync(readmeEn)) {
		const zhContent = readFileSync(readmeZh, "utf8");
		const enContent = readFileSync(readmeEn, "utf8");

		/* Cross-links: within 5 lines after first H1 */
		const zhHead = zhContent.split("\n").slice(0, 8).join("\n");
		const enHead = enContent.split("\n").slice(0, 8).join("\n");
		H.check(
			"AC-5.1 README.md links to README.en.md",
			zhHead.includes("README.en.md"),
			"no link to README.en.md in header",
		);
		H.check(
			"AC-5.1 README.en.md links to README.md",
			enHead.includes("README.md"),
			"no link to README.md in header",
		);

		/* H2 count should match */
		const zhH2 = (zhContent.match(/^## /gm) || []).length;
		const enH2 = (enContent.match(/^## /gm) || []).length;
		H.eq("AC-5.2 README H2 count matches", enH2, zhH2);

		/* Internal relative links in README.en.md still valid */
		const mdLinks = [...enContent.matchAll(/\]\(([^)]+\.md)\)/g)].map(
			(m) => m[1],
		);
		for (const link of mdLinks) {
			const fullPath = path.join(rootDir, link);
			H.check(
				`AC-5.2 README.en.md link "${link}" exists`,
				existsSync(fullPath),
				"file not found",
			);
		}
	}
	/* AC-7.3 overflow check 已合并到 AC-3.1/3.2 循环中（同一页面遍历一次完成） */

	const passed = H.summary("i18n");
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error("Suite error:", e);
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
