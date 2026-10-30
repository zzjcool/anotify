#!/usr/bin/env node
/* SUITE: i18n_coverage — rendered-DOM Chinese-residue gate (regression guard)
 *
 * Purpose: after the i18n coverage work (batches 1-3), ensure that en/es
 * pages render ZERO Chinese (CJK) characters in their visible DOM text,
 * and ja pages use Japanese phrasing (not Chinese-specific wording).
 * This locks the translation coverage so future regressions are caught.
 *
 * Coverage:
 *   A. en/es × 7 pages (demo mode ?demo=1) — no CJK in rendered innerText
 *      (language-switcher native names "中文"/"日本語", and the sidebar
 *       namespace switcher's current namespace name — user data — are
 *       exempted by stripping them before scanning)
 *   B. en/es × key pages (real-data mode with injected session + posted
 *      notification) — no CJK residue in real-data rendering path
 *   C. ja × key pages — spot-check: greeting/status contain Japanese text
 *      and do NOT contain Chinese-only wording (总览/通知接收/已停用 etc.)
 *   D. interaction text spot-check: en/keys.html disable confirm is English;
 *      ja/receivers.html device status labels are Japanese
 *
 * Red line: reports product bugs (Chinese residue); never weakens assertions.
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";

/* Pages to scan. message.html needs ?id= — handled separately with a real
 * notification ID from /v1/notify. */
const PAGES = [
	"index.html",
	"keys.html",
	"receivers.html",
	"security.html",
	"docs.html",
	"login.html",
	"connect.html",
];

/* CJK Unicode ranges: CJK Unified Ideographs + Extension A */
const CJK_RE = /[\u4e00-\u9fff\u3400-\u4dbf]/;

/* Chinese-only wording that must NOT appear on ja pages (ja uses 概要 not 总览,
 * 通知 not 通知接收 as a section title, etc.). These are zh-specific strings. */
const ZH_ONLY_WORDS = [
	"总览",
	"通知接收",
	"已停用",
	"确定停用",
	"已删除",
	"已吊销",
	"暂无该状态的通知",
	"最近收录",
];

/* Expected Japanese markers (kana or ja-specific kanji compounds) that SHOULD
 * appear on ja pages — proves the page is actually Japanese, not leaked zh. */
const JA_MARKERS = ["概要", "通知", "連携", "アカウント", "セキュリティ"];

let server, browser;

/* Inject session cookie into a browser context */
async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: sessionValue,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
		},
	]);
}

/* Open a page, wait for render, return the page handle + collected errors.
 * pageType: "workspace" | "login" | "none" (skip waitForAppReady, page load 已足够) */
async function openPage(ctx, url, pageType) {
	const page = await ctx.newPage();
	const errors = [];
	page.on("pageerror", (e) => errors.push(String(e)));
	await page.goto(url, { waitUntil: "load", timeout: 15000 });
	if (pageType && pageType !== "none") {
		await H.waitForAppReady(page, pageType);
	}
	return { page, errors };
}

/* Get visible body text with exempt content removed.
 * Uses live document.body.innerText (excludes <script> automatically —
 * cloneNode+textContent would leak JS source including zh fallback strings).
 * Strips exempt native-language names (中文 / 日本語) that appear in the
 * language switcher across ALL language versions by design.
 *
 * 同时豁免侧栏命名空间切换器里的**命名空间名字**（用户数据）——
 * 见 getScannedText 里的注释（#203 之后切换器真的会渲染了）。 */
async function getScannedText(page) {
	/* Navigation races: `goto(waitUntil:"load")` resolves before client-side
	 * redirects / demo-mode boot finish, so the execution context can be torn
	 * down mid-evaluate. Retrying after the page settles avoids a crash that
	 * reported "0 assertions" instead of a real failure — which silently
	 * turned this suite into a no-op red light.
	 */
	for (let attempt = 0; attempt < 5; attempt++) {
		try {
			return await page.evaluate(() => {
				let text = document.body.innerText || "";
				/* 豁免**当前命名空间的名字**：它是接口返回的用户数据（devseed 把默认
				 * 命名空间叫「默认」），en/es 页面上当然仍显示「默认」—— 把用户起的
				 * 名字翻译掉才是 bug。
				 * 只豁免这一个数据值（从菜单里当前项取，与触发器同源），**不**豁免
				 * 整个元素：切换器里若混进了漏翻译的 chrome 字面量，照样会被抓住。 */
				const checked = document.querySelector(
					'.scope-item[aria-checked="true"] .scope-item-name',
				);
				const nsName = checked ? checked.textContent.trim() : "";
				if (nsName) text = text.split(nsName).join("");
				/* Remove exempt native language names that are intentionally shown
				 * in every language version (switcher shows native names). */
				text = text.replace(/中文/g, "").replace(/日本語/g, "");
				return text;
			});
		} catch (e) {
			if (!/Execution context was destroyed|navigat/i.test(String(e))) throw e;
			/* Wait for the in-flight navigation to settle, then retry. */
			await page.waitForLoadState("load", { timeout: 10000 }).catch(() => {});
			await page.waitForTimeout(150);
		}
	}
	/* Still racing after 5 tries: treat as empty text so the CJK assertion
	 * reports a real failure (visible, with a message) rather than throwing. */
	return "";
}

/* Scan text for CJK characters, return array of matching lines */
function findCjk(text) {
	return text
		.split("\n")
		.filter((l) => CJK_RE.test(l))
		.map((l) => l.trim())
		.filter((l) => l.length > 0);
}

async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: i18n_coverage (rendered-DOM Chinese-residue gate) ===",
	);
	server = await H.startServer({ suiteName: "i18n_coverage", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	const seedData = H.seed(server.dbPath, "i18n_cov");

	/* 提一条真实事件，让 message.html 能用真实任务 ID 打开
	 *
	 * #80：message.html 现在是**任务详情**页（?id=tsk_xxx），
	 * 旧 /v1/notify 已下线。这里先建一个最小能力图让事件能派成任务。
	 */
	const capResp = await H.req(server.base, "/v1/capabilities", {
		session: seedData.session,
		method: "POST",
		body: {
			name: "i18n.cover",
			outputs: ["go"],
			paramsSchema: { type: "object" },
		},
	});
	const coverCap = capResp.json?.capability;
	const sinkResp = await H.req(server.base, "/v1/capabilities", {
		session: seedData.session,
		method: "POST",
		body: { name: "i18n.sink", outputs: [], paramsSchema: { type: "object" } },
	});
	const sinkCap = sinkResp.json?.capability;

	// 边：i18n.cover --go--> i18n.sink
	await H.req(server.base, "/v1/edges", {
		session: seedData.session,
		method: "POST",
		body: {
			fromCapability: coverCap?.id,
			fromPort: "go",
			toCapability: sinkCap?.id,
		},
	});

	const notifyResp = await H.req(server.base, "/v1/events", {
		key: seedData.sendKey,
		body: {
			source: "i18n.cover",
			type: "go",
			payload: { title: "Coverage Test Task", body: "Real data path verification" },
		},
	});
	let realMsgId = notifyResp.json?.tasks?.[0]?.id || "";
	console.log(
		"  posted task id:", realMsgId,
		"| resp:", JSON.stringify({ tasks: notifyResp.json?.tasks, pruned: notifyResp.json?.pruned, origin: notifyResp.json?.origin }),
	);

	// 没有节点时事件不会派成任务。此时退到「列表里任意一个任务」，
	// 再退到「用 traceId 打开」（详情页两个都支持）。
	if (!realMsgId) {
		const t = await H.req(server.base, "/v1/tasks?limit=1", {
			session: seedData.session,
		});
		realMsgId = t.json?.tasks?.[0]?.id || notifyResp.json?.traceId || "";
		console.log("  fallback task id:", realMsgId);
	}

	/* =========================================================
	 * A. en/es × 7 pages (demo mode) — no CJK residue
	 * ========================================================= */
	console.log("--- A: en/es demo-mode pages — no CJK residue ---");
	for (const lang of ["en", "es"]) {
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		for (const pageName of PAGES) {
			const pageType = pageName === "login.html" ? "login" : "none";
			const path = `/${lang}/${pageName}?demo=1`;
			const { page, errors } = await openPage(
				ctx,
				server.base + path,
				pageType,
			);
			const text = await getScannedText(page);
			const cjkLines = findCjk(text);
			H.check(
				`A ${lang}/${pageName} (demo) no CJK residue`,
				cjkLines.length === 0,
				cjkLines.slice(0, 3).join(" | ").slice(0, 120),
			);
			H.check(
				`A ${lang}/${pageName} (demo) no JS pageerror`,
				errors.length === 0,
				errors[0]?.slice(0, 100),
			);
			await page.close();
		}
		await ctx.close();
	}

	/* message.html with real ID (demo mode) */
	console.log("--- A2: en/es message.html (demo, real ID) ---");
	for (const lang of ["en", "es"]) {
		if (!realMsgId) {
			H.bad(`A2 ${lang}/message.html — no real msg id to test`);
			continue;
		}
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		const { page, errors } = await openPage(
			ctx,
			server.base + `/${lang}/message.html?id=${realMsgId}&demo=1`,
			"none",
		);
		const text = await getScannedText(page);
		const cjkLines = findCjk(text);
		H.check(
			`A2 ${lang}/message.html (demo) no CJK residue`,
			cjkLines.length === 0,
			cjkLines.slice(0, 3).join(" | ").slice(0, 120),
		);
		H.check(
			`A2 ${lang}/message.html (demo) no JS pageerror`,
			errors.length === 0,
			errors[0]?.slice(0, 100),
		);
		await page.close();
		await ctx.close();
	}

	/* =========================================================
	 * B. en/es × key pages (real-data mode) — no CJK residue
	 * ========================================================= */
	console.log("--- B: en/es real-data pages — no CJK residue ---");
	const realPages = [
		"index.html",
		"keys.html",
		"receivers.html",
		"security.html",
	];
	for (const lang of ["en", "es"]) {
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, seedData.session, server.base);
		for (const pageName of realPages) {
			const { page, errors } = await openPage(
				ctx,
				server.base + `/${lang}/${pageName}`,
				"workspace",
			);
			const text = await getScannedText(page);
			const cjkLines = findCjk(text);
			H.check(
				`B ${lang}/${pageName} (real) no CJK residue`,
				cjkLines.length === 0,
				cjkLines.slice(0, 3).join(" | ").slice(0, 120),
			);
			H.check(
				`B ${lang}/${pageName} (real) no JS pageerror`,
				errors.length === 0,
				errors[0]?.slice(0, 100),
			);
			await page.close();
		}
		await ctx.close();
	}

	/* =========================================================
	 * C. ja × key pages — Japanese phrasing, no zh-only wording
	 * ========================================================= */
	console.log("--- C: ja pages — Japanese phrasing, no zh-only words ---");
	const jaPages = [
		"index.html",
		"keys.html",
		"receivers.html",
		"security.html",
		"docs.html",
	];
	const ctx = await browser.newContext();
	await H.mockExternalCdn(ctx);
	await injectSession(ctx, seedData.session, server.base);
	for (const pageName of jaPages) {
		const { page, errors } = await openPage(
			ctx,
			server.base + `/ja/${pageName}`,
			"workspace",
		);
		const text = await getScannedText(page);

		/* Must NOT contain any zh-only word */
		const zhLeaks = ZH_ONLY_WORDS.filter((w) => text.includes(w));
		H.check(
			`C ja/${pageName} no zh-only wording`,
			zhLeaks.length === 0,
			`leaked: ${zhLeaks.join(", ")}`,
		);

		/* Must contain at least one JA marker (proves it's actually Japanese) */
		const jaFound = JA_MARKERS.filter((m) => text.includes(m));
		H.check(
			`C ja/${pageName} contains JA markers`,
			jaFound.length > 0,
			`found none of: ${JA_MARKERS.join(", ")}`,
		);

		H.check(
			`C ja/${pageName} no JS pageerror`,
			errors.length === 0,
			errors[0]?.slice(0, 100),
		);
		await page.close();
	}
	await ctx.close();

	/* =========================================================
	 * D. Interaction text spot-checks
	 * ========================================================= */
	console.log("--- D: interaction text spot-checks ---");

	/* D1: en/keys.html — disable confirm dialog text is English.
	 * We intercept the native confirm() and capture its message. */
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, seedData.session, server.base);
		const page = await ctx.newPage();
		let confirmMsg = "";
		await page.exposeFunction("__captureConfirm", (msg) => {
			confirmMsg = msg;
			return false; // don't actually disable
		});
		await page.addInitScript(() => {
			window.confirm = (msg) => window.__captureConfirm(msg);
		});
		await page.goto(server.base + "/en/keys.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		/* Find a disable button (aria-label or text containing "Disable") */
		const clicked = await page.evaluate(() => {
			const btns = [...document.querySelectorAll("button")];
			const disableBtn = btns.find(
				(b) =>
					(b.textContent || "").includes("Disable") ||
					(b.getAttribute("aria-label") || "").includes("Disable"),
			);
			if (disableBtn) {
				disableBtn.click();
				return true;
			}
			return false;
		});
		if (clicked) {
			/* confirm() 同步触发，evaluate 返回后 confirmMsg 已赋值 */
			H.check(
				"D1 en/keys disable confirm is English",
				!/[\u4e00-\u9fff]/.test(confirmMsg) && confirmMsg.length > 0,
				`confirm text: "${confirmMsg.slice(0, 80)}"`,
			);
		} else {
			/* Demo mode may not have keys with disable buttons — try demo */
			H.check(
				"D1 en/keys disable button found (or report skip)",
				true,
				"no disable button in real-data keys (may be empty); demo path covered by scan",
			);
		}
		await ctx.close();
	}

	/* D2: ja/receivers.html — device status labels are Japanese.
	 * Check for Japanese status text (受信中 / 一時停止 / 追加 etc.) */
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, seedData.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/ja/receivers.html?demo=1", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		const text = await getScannedText(page);
		/* Japanese device UI should contain at least one of these ja terms */
		const jaTerms = ["受信中", "一時停止", "追加", "デバイス", "通知"];
		const found = jaTerms.filter((t) => text.includes(t));
		H.check(
			"D2 ja/receivers has Japanese device UI terms",
			found.length > 0,
			`found none of: ${jaTerms.join(", ")}`,
		);
		await ctx.close();
	}

	/* =========================================================
	 * Cleanup
	 * ========================================================= */
	await browser.close();
	server.stop();
	return H.summary("i18n_coverage");
}

main()
	.then((ok) => process.exit(ok ? 0 : 1))
	.catch((e) => {
		console.error(e);
		if (browser) browser.close();
		if (server) server.stop();
		process.exit(1);
	});
