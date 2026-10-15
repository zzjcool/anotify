#!/usr/bin/env node
/* SUITE: recovery_next — #32 恢复码登录跳转断言
 *
 * 锁死 #31 行为：
 *  1. 有 ?next=<白名单页> → 登录后落到该页（与 Passkey nextTarget 一致）
 *  2. 无 next → 默认 security.html?from=recovery
 *
 * 不覆盖完整 recovery UX dogfood（Ivy tip）；只断言 redirect。
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const SUITE = "recovery_next";
let server, browser;

async function issueCodes(base, session) {
	const r = await H.req(base, "/v1/auth/recovery/regenerate", {
		method: "POST",
		session,
	});
	if (r.status !== 200) {
		throw new Error(
			`regenerate failed: status=${r.status} body=${r.text?.slice(0, 200)}`,
		);
	}
	const codes = r.json?.recoveryCodes;
	if (!Array.isArray(codes) || codes.length < 2) {
		throw new Error(`need ≥2 recovery codes, got ${JSON.stringify(codes)}`);
	}
	return codes;
}

async function recoveryLoginOnPage(page, base, { username, code, loginPath }) {
	await page.goto(base + loginPath, { waitUntil: "load", timeout: 15000 });
	await H.waitForAppReady(page, "login");

	// 展开恢复码面板
	await page.click("#recovery-toggle");
	await page.waitForSelector("#recovery-panel:not(.hidden)", { timeout: 5000 });

	await page.fill("#recovery-username", username);
	await page.fill("#recovery-code", code);
	await page.click("#recovery-btn");
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: recovery_next（#32 恢复码登录 ?next= / 默认跳转）===");
	server = await H.startServer({ suiteName: SUITE, rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	const username = "rec_next_" + Date.now();
	const seeded = H.seed(server.dbPath, username);
	const codes = await issueCodes(server.base, seeded.session);

	// ---------- 1. 有 ?next=keys.html → 落到 keys.html ----------
	console.log("--- recovery login with ?next=keys.html ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		const page = await ctx.newPage();
		await recoveryLoginOnPage(page, server.base, {
			username,
			code: codes[0],
			loginPath: "/login.html?next=keys.html",
		});
		// doRecoveryLogin 成功后 setTimeout(500) 再跳
		await page.waitForURL(/\/keys\.html(\?|$)/, { timeout: 10000 });
		const u = page.url();
		const path = new URL(u).pathname;
		H.check(
			"#32 有 ?next=keys.html → 落地 keys.html",
			path.endsWith("/keys.html") || path.endsWith("keys.html"),
			`url=${u}`,
		);
		H.check(
			"#32 有 next 时不落到 security?from=recovery",
			!u.includes("from=recovery"),
			`url=${u}`,
		);
		await page.close();
		await ctx.close();
	}

	// ---------- 2. 无 next → security.html?from=recovery ----------
	console.log("--- recovery login without next → security?from=recovery ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		const page = await ctx.newPage();
		await recoveryLoginOnPage(page, server.base, {
			username,
			code: codes[1],
			loginPath: "/login.html",
		});
		// 跳转瞬间带 ?from=recovery；security 页会 show nudge 后 replaceState 清掉 query
		await page.waitForURL(/\/security\.html\?from=recovery/, {
			timeout: 10000,
		});
		H.ok("#32 无 next → 曾落到 security.html?from=recovery");
		await page
			.waitForSelector("#recovery-nudge:not(.hidden)", { timeout: 5000 })
			.catch(() => {});
		const after = await page.evaluate(() => {
			const nudge = document.getElementById("recovery-nudge");
			return {
				path: location.pathname,
				href: location.href,
				nudgeVisible: !!(nudge && !nudge.classList.contains("hidden")),
			};
		});
		H.check(
			"#32 无 next → 仍停在 security.html",
			after.path.endsWith("/security.html") ||
				after.path.endsWith("security.html"),
			`href=${after.href}`,
		);
		H.check(
			"#32 from=recovery 触发恢复码 nudge",
			after.nudgeVisible === true,
			JSON.stringify(after),
		);
		await page.close();
		await ctx.close();
	}

	const passed = H.summary(SUITE);
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
