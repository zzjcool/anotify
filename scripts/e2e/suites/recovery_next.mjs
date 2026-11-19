#!/usr/bin/env node
/* SUITE: recovery_next — #32 恢复码登录跳转 + #453 重新生成恢复码不造假码
 *
 * 锁死 #31 行为：
 *  1. 有 ?next=<白名单页> → 登录后落到该页（与 Passkey nextTarget 一致）
 *  2. 无 next → 默认 security.html?from=recovery
 *
 * #453：安全页「重新生成恢复码」在状态接口失败时曾**不发请求**就渲染一组 DEMO 假码
 * 并提示「请立即保存」（用户存下假码 → 真丢设备时锁号）。这里用 mock 把 status /
 * regenerate 都打成 500，锁死「失败只报错、页面上不出现任何码」；成功路径仍走真
 * 后端，断言渲染的码不是占位。
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
		args: ["--no-sandbox", "--disable-dev-shm-usage"],
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

	// ---------- 3. #453 状态接口 500 → 页面不出现任何码 ----------
	console.log("--- #453 状态接口 500：页面不出现任何码 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seeded.session,
				domain: new URL(server.base).hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const page = await ctx.newPage();
		let regenCalls = 0;
		await page.route("**/v1/auth/recovery/status", (route) =>
			route.fulfill({
				status: 500,
				contentType: "application/json",
				body: '{"message":"status down"}',
			}),
		);
		await page.route("**/v1/auth/recovery/regenerate", (route) => {
			regenCalls += 1;
			return route.fulfill({
				status: 500,
				contentType: "application/json",
				body: '{"message":"regen down"}',
			});
		});
		await page.goto(server.base + "/security.html", {
			waitUntil: "domcontentloaded",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "security");
		// 状态的失败是异步的：等 hint 真的切到「无法加载」再断言，不等时间。
		await page.waitForFunction(
			() =>
				(
					document.getElementById("recovery-hint")?.textContent || ""
				).includes("无法加载恢复码状态"),
			null,
			{ timeout: 10000 },
		);
		const idle = await page.evaluate(() => ({
			codes: document.querySelectorAll("#recovery-codes code").length,
			gridText: (
				document.getElementById("recovery-codes")?.textContent || ""
			).trim(),
			body: document.body.innerText,
		}));
		H.check(
			"#453 状态失败：不渲染任何码",
			idle.codes === 0 && idle.gridText === "",
			JSON.stringify({ codes: idle.codes, gridText: idle.gridText }),
		);
		H.check(
			"#453 状态失败：无「请立即保存」提示",
			!/请立即保存|立即复制保存/.test(idle.body),
			JSON.stringify(idle.body.slice(0, 200)),
		);
		H.check(
			"#453 状态失败：无 DEMO 占位码",
			!/DEMO/i.test(idle.body),
			JSON.stringify(idle.body.slice(0, 200)),
		);

		// 点「重新生成」：必须真的发请求（不是本地造码），失败只报错。
		await page.click("#recovery-regen-btn");
		const confirmOk = page.locator("#anotify-dialog [data-dialog-ok]");
		await confirmOk.waitFor({ state: "visible", timeout: 5000 });
		await confirmOk.click();
		// 等这次重新生成真的落定：按钮在 finally 里恢复可点，toast 已经出现。
		// 不直接等「生成失败」文案 —— 那样一旦回归成假码成功路径，就只能看到超时，
		// 看不到「页面出现了码」这条真正的红。
		await page.waitForFunction(
			() => {
				const btn = document.getElementById("recovery-regen-btn");
				const el = document.getElementById("toast");
				return (!!btn && !btn.disabled && !!el && el.classList.contains("show"));
			},
			null,
			{ timeout: 10000 },
		);
		const failed = await page.evaluate(() => ({
			codes: document.querySelectorAll("#recovery-codes code").length,
			gridText: (
				document.getElementById("recovery-codes")?.textContent || ""
			).trim(),
			toast: (document.getElementById("toast")?.textContent || "").trim(),
			body: document.body.innerText,
		}));
		H.check(
			"#453 生成失败：toast 是「生成失败，请重试」",
			/生成失败，请重试/.test(failed.toast),
			failed.toast,
		);
		H.check(
			"#453 生成失败：没有「已生成/请立即保存」的假成功",
			!/已生成新恢复码|请立即保存/.test(failed.toast),
			failed.toast,
		);
		H.check(
			"#453 生成失败：真的发出了 regenerate 请求（不是本地造码）",
			regenCalls > 0,
			`regenCalls=${regenCalls}`,
		);
		H.check(
			"#453 生成失败：页面仍无任何码",
			failed.codes === 0 && failed.gridText === "",
			JSON.stringify({ codes: failed.codes, gridText: failed.gridText }),
		);
		H.check(
			"#453 生成失败：无「请立即保存」提示",
			!/请立即保存|立即复制保存/.test(failed.body),
			JSON.stringify(failed.body.slice(0, 200)),
		);
		await page.close();
		await ctx.close();
	}

	// ---------- 4. 成功路径：8 枚真码 + 「请立即复制保存」 ----------
	console.log("--- 成功路径：8 枚恢复码 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([
			{
				name: "anotify_session",
				value: seeded.session,
				domain: new URL(server.base).hostname,
				path: "/",
				httpOnly: true,
			},
		]);
		const page = await ctx.newPage();
		await page.goto(server.base + "/security.html", {
			waitUntil: "domcontentloaded",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "security");
		await page.waitForFunction(
			() =>
				!(
					document.getElementById("recovery-hint")?.textContent || ""
				).includes("加载中"),
			null,
			{ timeout: 10000 },
		);
		await page.click("#recovery-regen-btn");
		const confirmOk = page.locator("#anotify-dialog [data-dialog-ok]");
		await confirmOk.waitFor({ state: "visible", timeout: 5000 });
		await confirmOk.click();
		await page.waitForFunction(
			() => document.querySelectorAll("#recovery-codes code").length === 8,
			null,
			{ timeout: 10000 },
		);
		const ok = await page.evaluate(() => ({
			codes: Array.from(
				document.querySelectorAll("#recovery-codes code"),
			).map((x) => x.textContent),
			hint: (
				document.getElementById("recovery-hint")?.textContent || ""
			).trim(),
			toast: (document.getElementById("toast")?.textContent || "").trim(),
		}));
		H.check(
			"#453 成功路径：渲染 8 枚恢复码",
			ok.codes.length === 8 &&
				new Set(ok.codes).size === 8 &&
				ok.codes.every((c) => c.length >= 8 && !/DEMO/i.test(c)),
			JSON.stringify(ok.codes),
		);
		H.check(
			"#453 成功路径：提示「请立即复制保存」",
			/请立即复制保存/.test(ok.hint),
			ok.hint,
		);
		H.check(
			"#453 成功路径：成功 toast 出现「请立即保存」",
			/请立即保存/.test(ok.toast),
			ok.toast,
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
