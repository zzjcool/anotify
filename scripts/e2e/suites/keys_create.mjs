#!/usr/bin/env node
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const SUITE = "keys_create";

let server, browser;

async function openKeysPage(ctx, base) {
	const page = await ctx.newPage();
	await page.goto(base + "/keys.html", { waitUntil: "load", timeout: 15000 });
	await H.waitForAppReady(page, "keys");
	await page.waitForSelector("#btn-create", { timeout: 10000 });
	return page;
}

async function serverKeysNamed(base, session, name) {
	const r = await H.req(base, "/v1/keys?limit=100", { session });
	const keys = Array.isArray(r.json?.keys) ? r.json.keys : [];
	return { all: keys, named: keys.filter((k) => k.name === name) };
}

async function rowsNamed(page, name) {
	return page.evaluate(
		(n) =>
			[...document.querySelectorAll("#keys-body tr")].filter((tr) =>
				(tr.textContent || "").includes(n),
			).length,
		name,
	);
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: keys_create（#616 双击生成 / 列表一致性）===");
	server = await H.startServer({ suiteName: SUITE, rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox", "--disable-dev-shm-usage"],
	});

	const seeded = H.seed(server.dbPath, "keys616_" + Date.now());
	const cookie = {
		name: "anotify_session",
		value: seeded.session,
		domain: new URL(server.base).hostname,
		path: "/",
		httpOnly: true,
	};

	console.log("--- #616 双击「生成」只允许 1 个 POST ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([cookie]);
		const page = await openKeysPage(ctx, server.base);
		const name = "c4-dbl-" + Date.now();
		let posts = 0;
		const postErrors = [];
		page.on("request", (req) => {
			if (req.method() === "POST" && /\/v1\/keys(\?|$)/.test(req.url()))
				posts += 1;
		});
		page.on("pageerror", (e) => postErrors.push(String(e)));

		const before = await serverKeysNamed(server.base, seeded.session, name);
		H.eq(`#616 前置：服务端尚无 ${name}`, before.named.length, 0);

		await page.locator("#btn-create").click();
		await page.waitForSelector("#create-step-1:not(.hidden)", {
			timeout: 5000,
		});
		await page.locator("#new-key-name").fill(name);
		await page.locator("#create-submit").dblclick();
		await page.waitForSelector("#create-step-2:not(.hidden)", {
			timeout: 15000,
		});
		await page.waitForLoadState("networkidle", { timeout: 15000 });
		H.eq(`#616 一次双击只发出 1 个 POST /v1/keys`, posts, 1);

		const plaintext = await page.locator("#new-key-value").innerText();
		H.check(
			`#616 弹层展示了明文（可复制的那把）`,
			typeof plaintext === "string" && plaintext.trim().length > 10,
			JSON.stringify(plaintext?.slice(0, 12)),
		);

		const after = await serverKeysNamed(server.base, seeded.session, name);
		H.eq(`#616 服务端同名称只有 1 把 Key`, after.named.length, 1);

		await page.locator("#create-done").click();
		await page.waitForFunction(
			(n) =>
				[...document.querySelectorAll("#keys-body tr")].some((tr) =>
					(tr.textContent || "").includes(n),
				),
			name,
			{ timeout: 10000 },
		);
		const rendered = await rowsNamed(page, name);
		const truth = await serverKeysNamed(server.base, seeded.session, name);
		H.eq(`#616 列表里该名称只有 1 行（不是本地插一条）`, rendered, 1);
		H.eq(
			`#616 列表行数与 GET /v1/keys 一致`,
			rendered,
			truth.named.length,
		);
		H.check(
			`#616 全程无 pageerror`,
			postErrors.length === 0,
			postErrors.join(" | "),
		);
		await page.close();
		await ctx.close();
	}

	console.log("--- #616 请求进行中：按钮 disabled + 生成中… ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([cookie]);
		const page = await openKeysPage(ctx, server.base);
		const name = "c4-inflight-" + Date.now();
		let release;
		const gate = new Promise((resolve) => {
			release = resolve;
		});
		let posts = 0;
		await page.route("**/v1/keys", async (route) => {
			if (route.request().method() !== "POST") return route.continue();
			posts += 1;
			await gate;
			return route.continue();
		});

		await page.locator("#btn-create").click();
		await page.locator("#new-key-name").fill(name);
		const issued = page.waitForRequest(
			(req) =>
				req.method() === "POST" && /\/v1\/keys(\?|$)/.test(req.url()),
			{ timeout: 10000 },
		);
		await page.locator("#create-submit").click();
		await issued;
		const busy = await page.evaluate(() => {
			const btn = document.getElementById("create-submit");
			return { disabled: btn?.disabled, label: btn?.textContent || "" };
		});
		H.check(`#616 进行中 #create-submit 为 disabled`, busy.disabled === true, JSON.stringify(busy));
		H.check(
			`#616 进行中文案切成「生成中…」`,
			/生成中/.test(busy.label),
			busy.label,
		);
		await page.locator("#create-submit").dispatchEvent("click");
		await page.locator("#create-submit").dispatchEvent("click");

		release();
		await page.waitForSelector("#create-step-2:not(.hidden)", {
			timeout: 15000,
		});
		await page.waitForLoadState("networkidle", { timeout: 15000 });
		H.eq(`#616 进行中的额外点击不再发 POST`, posts, 1);
		const created = await serverKeysNamed(server.base, seeded.session, name);
		H.eq(`#616 服务端同名称只有 1 把 Key`, created.named.length, 1);
		await page.close();
		await ctx.close();
	}

	console.log("--- #616 接口失败：按钮恢复可点 + 错误可见 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([cookie]);
		const page = await openKeysPage(ctx, server.base);
		const name = "c4-fail-" + Date.now();
		let posts = 0;
		await page.route("**/v1/keys", (route) => {
			if (route.request().method() !== "POST") return route.continue();
			posts += 1;
			return route.fulfill({
				status: 500,
				contentType: "application/json",
				body: '{"message":"boom-616"}',
			});
		});

		await page.locator("#btn-create").click();
		await page.locator("#new-key-name").fill(name);
		await page.locator("#create-submit").click();
		await page.waitForFunction(
			() =>
				(document.getElementById("create-error")?.textContent || "").includes(
					"boom-616",
				),
			null,
			{ timeout: 15000 },
		);
		const state = await page.evaluate(() => {
			const btn = document.getElementById("create-submit");
			const err = document.getElementById("create-error");
			return {
				disabled: btn?.disabled,
				label: btn?.textContent || "",
				errHidden: err?.classList.contains("hidden"),
				errText: err?.textContent || "",
				step1: !document
					.getElementById("create-step-1")
					?.classList.contains("hidden"),
			};
		});
		H.check(`#616 失败后按钮恢复可点`, state.disabled === false, JSON.stringify(state));
		H.check(
			`#616 失败后文案还原（不是「生成中…」）`,
			!/生成中/.test(state.label) && state.label.trim().length > 0,
			state.label,
		);
		H.check(
			`#616 失败原因显示在 #create-error`,
			state.errHidden === false && state.errText.includes("boom-616"),
			state.errText,
		);
		H.check(`#616 失败后仍停在第一步（可改名称重试）`, state.step1 === true, JSON.stringify(state));
		H.eq(`#616 失败只发 1 个 POST`, posts, 1);
		await page.close();
		await ctx.close();
	}

	console.log("--- #616 完成时重新拉取 GET /v1/keys ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await ctx.addCookies([cookie]);
		const page = await openKeysPage(ctx, server.base);
		const name = "c4-refetch-" + Date.now();
		const elsewhere = "c4-elsewhere-" + Date.now();

		await page.locator("#btn-create").click();
		await page.locator("#new-key-name").fill(name);
		await page.locator("#create-submit").click();
		await page.waitForSelector("#create-step-2:not(.hidden)", {
			timeout: 15000,
		});
		const other = await H.req(server.base, "/v1/keys", {
			method: "POST",
			session: seeded.session,
			body: { name: elsewhere, scopes: ["notify:send"] },
		});
		H.eq(`#616 别处创建的 Key 成功（造数）`, other.status, 200);

		await page.locator("#create-done").click();
		await page.waitForFunction(
			(n) =>
				[...document.querySelectorAll("#keys-body tr")].some((tr) =>
					(tr.textContent || "").includes(n),
				),
			elsewhere,
			{ timeout: 10000 },
		);
		H.ok(`#616 完成时重新拉取（别处创建的那把也出现在列表里）`);
		const rendered = await rowsNamed(page, elsewhere);
		H.eq(`#616 别处创建的那把只渲染 1 行`, rendered, 1);
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
	} finally {
		process.exit(1);
	}
});
