#!/usr/bin/env node
/* SUITE: frontend — 前端渲染 + 路由守卫 + 真实数据
 *
 * 覆盖 case：
 *  A. 路由守卫：未登录访问 index/receivers/keys/security → 自动跳 login.html
 *  B. 已登录访问 index → 不显示「演示数据」徽章（demo-badge 隐藏），渲染真实数据（哪怕空）
 *  C. login.html 是公开页：未登录正常渲染、不跳转
 *  D. 全部 6 页（index/login/receivers/keys/security/docs）在 桌面1280 + 移动390 两视口：
 *     无 JS pageerror、无横向溢出、能滚动到底
 *     （/v1/* 的 401/404 是预期降级，不算失败；demo-badge 显示是后端未连接的预期行为）
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
const PAGES = ["index.html", "login.html", "receivers.html", "keys.html", "security.html", "docs.html"];
const GUARDED = ["index.html", "receivers.html", "keys.html", "security.html"];
const VIEWPORTS = [
	{ name: "桌面1280", width: 1280, height: 800 },
	{ name: "移动390", width: 390, height: 844 },
];

let server, browser;

// 收集单页在指定视口下的渲染问题
async function checkPage(ctx, url, viewportName, viewport) {
	const page = await ctx.newPage();
	await page.setViewportSize({ width: viewport.width, height: viewport.height });
	const pageErrors = [];
	page.on("pageerror", (e) => pageErrors.push(String(e)));
	try {
		await page.goto(url, { waitUntil: "load", timeout: 15000 });
		await page.waitForTimeout(1200); // 等渲染 + 数据加载
	} catch (e) {
		H.bad(`${viewportName} ${url.split("/").pop()} 加载`, String(e).slice(0, 80));
		await page.close();
		return;
	}
	const name = url.split("/").pop();

	// JS 错误
	H.check(`${viewportName} ${name} 无 JS pageerror`, pageErrors.length === 0, pageErrors[0]?.slice(0, 100));

	// 横向溢出（排除 pre/overflow-x-auto 容器）
	const overflowCount = await page.evaluate(() => {
		const vw = window.innerWidth;
		let n = 0;
		for (const el of document.querySelectorAll("body *")) {
			const r = el.getBoundingClientRect();
			if (r.right > vw + 5 && !el.closest("pre") && !el.closest(".overflow-x-auto")) n++;
		}
		return n;
	});
	H.check(`${viewportName} ${name} 无横向溢出`, overflowCount === 0, `${overflowCount} 个元素超出`);

	// 能滚动到底（若可滚动）
	const canScrollToBottom = await page.evaluate(async () => {
		const sh = document.documentElement.scrollHeight;
		const vh = window.innerHeight;
		if (sh <= vh) return true; // 不可滚动也算通过
		window.scrollTo(0, sh);
		await new Promise((r) => setTimeout(r, 300));
		return window.scrollY >= sh - vh - 5;
	});
	H.check(`${viewportName} ${name} 可滚动到底`, canScrollToBottom);

	await page.close();
}

// 注入 SESSION cookie 到 context
async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
	await ctx.addCookies([{
		name: "anotify_session", value: sessionValue,
		domain: u.hostname, path: "/", httpOnly: true,
		secure: u.protocol === "https:",
	}]);
}

async function main() {
	console.log("=== SUITE: frontend（前端渲染 + 路由守卫 + 真实数据）===");
	server = await H.startServer({ rpId: RP });
	browser = await chromium.launch({ channel: "chrome", headless: true, args: ["--no-sandbox"] });

	// ---- A. 路由守卫：未登录访问受保护页 → 跳 login.html ----
	console.log("--- 路由守卫（未登录→跳登录）---");
	for (const p of GUARDED) {
		const ctx = await browser.newContext();
		const page = await ctx.newPage();
		try {
			await page.goto(server.base + "/" + p, { waitUntil: "load", timeout: 15000 });
			// 等客户端 JS 触发 401 → 跳转
			await page.waitForTimeout(2000);
			const finalUrl = page.url();
			H.check(`未登录访问 ${p} → 跳转到 login.html`, finalUrl.includes("login.html"), `最终 ${finalUrl}`);
		} catch (e) {
			H.bad(`未登录访问 ${p} 路由守卫`, String(e).slice(0, 80));
		}
		await page.close();
		await ctx.close();
	}

	// ---- C. login.html 公开页：未登录正常渲染不跳 ----
	console.log("--- login 公开页 ---");
	{
		const ctx = await browser.newContext();
		const page = await ctx.newPage();
		await page.goto(server.base + "/login.html", { waitUntil: "load", timeout: 15000 });
		await page.waitForTimeout(1200);
		H.check("login.html 未登录停留本页(不跳转)", page.url().includes("login.html"), `最终 ${page.url()}`);
		const hasForm = await page.evaluate(() => !!document.body && document.body.innerText.length > 20);
		H.check("login.html 正常渲染有内容", hasForm);
		await page.close();
		await ctx.close();
	}

	// ---- B. 已登录访问 index → 不显示演示徽章（真实数据）----
	console.log("--- 已登录真实数据 ---");
	{
		// 用 devseed 建会话（无需 WebAuthn），注入 cookie
		const s = H.seed(server.dbPath, "frontend");
		// 先上报一条真实通知，验证真实数据渲染
		await H.req(server.base, "/v1/notify", { key: s.sendKey, body: { title: "前端真实数据验证", status: "success" } });

		const ctx = await browser.newContext();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		await page.goto(server.base + "/index.html", { waitUntil: "load", timeout: 15000 });
		await page.waitForTimeout(2000); // 等数据加载

		H.check("已登录访问 index 停留本页(不跳登录)", page.url().includes("index.html"), `最终 ${page.url()}`);

		// demo-badge 应隐藏（有真实数据，非演示模式）
		const badgeHidden = await page.evaluate(() => {
			const b = document.getElementById("demo-badge");
			if (!b) return "missing";
			return b.classList.contains("hidden");
		});
		H.check("已登录 index 不显示「演示数据」徽章", badgeHidden === true, `badgeHidden=${badgeHidden}`);

		// 真实通知应出现在列表中
		const hasReal = await page.evaluate(() => document.body.innerText.includes("前端真实数据验证"));
		H.check("已登录 index 显示真实通知数据", hasReal);

		await page.close();
		await ctx.close();
	}

	// ---- D. 全部 6 页 × 2 视口渲染检查（未登录态，验证纯渲染；受保护页会跳 login，跳后渲染 login 也算无 JS 错误）----
	console.log("--- 全页面 × 双视口渲染 ---");
	for (const vp of VIEWPORTS) {
		const ctx = await browser.newContext();
		for (const p of PAGES) {
			await checkPage(ctx, server.base + "/" + p, vp.name, vp);
		}
		await ctx.close();
	}

	const passed = H.summary("frontend");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => { console.error(e); try { await browser?.close(); server?.stop(); } catch { /* ignore */ } process.exit(1); });
