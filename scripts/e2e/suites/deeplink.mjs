#!/usr/bin/env node
/* SUITE: deeplink — 推送点击深链：message.html?id=<id> 展示该消息完整信息
 *
 * 覆盖 case：
 *  A. 登录后访问 message.html?id=<id> → 展示完整字段（标题/正文/ID/Seq/标签/优先级/TTL/时间）
 *  B. 详情页含真实投递记录（deliveries），无记录时空态提示
 *  C. 上报带 link 的消息 → 详情页含「打开 Agent 会话」入口且 href 正确
 *  D. /v1/notifications/{id} 契约：属主 200 / 未登录 401 / 不存在 404
 *  E. 首页 index 弹层：字段补全（含 消息ID/优先级/标签/TTL）+ 「查看完整页面」入口
 *  F. 未登录点深链 → 跳登录页时 next 保留 message.html?id= 深链
 *  G. 不存在的 id → 页面显示「消息不存在」降级、无 JS pageerror
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

let server, browser;

async function injectSession(ctx, sessionValue, base) {
	let u;
	try {
		u = new URL(base);
	} catch {
		throw new Error("server.base 非法: " + base);
	}
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

async function main() {
	console.log("=== SUITE: deeplink（推送点击 → 消息详情页 message.html）===");
	server = await H.startServer({ rpId: "localhost" });
	const s = H.seed(server.dbPath, "deeplink");

	// 上报一条带全部字段 + link 的消息
	const r1 = await H.req(server.base, "/v1/notify", {
		key: s.sendKey,
		body: {
			title: "深链验证-构建完成",
			status: "success",
			body: "共 47 个文件变更，已发布到生产环境。",
			link: "pi://session/sess_deeplink1",
			agentId: "deploy-bot",
			sessionId: "sess_dl_1",
			cwd: "/home/z/prod-backend",
			model: "claude-sonnet-4",
			durationMs: 24000,
			priority: "high",
			ttl: 3600,
			deviceTags: ["手机"],
		},
	});
	H.eq("notify 全字段 → 200", r1.status, 200);
	const id1 = r1.json && r1.json.id;
	H.check("notify 返回消息 ID", typeof id1 === "string" && id1.length > 0);

	// 一条无 link 的消息
	const r2 = await H.req(server.base, "/v1/notify", {
		key: s.sendKey,
		body: {
			title: "深链验证-无外链",
			status: "error",
			body: "3 个测试用例未通过。",
		},
	});
	H.eq("notify 不带 link → 200", r2.status, 200);
	const id2 = r2.json && r2.json.id;

	// ---- D. 单条消息 API 契约 ----
	console.log("--- /v1/notifications/{id} 契约 ---");
	{
		const d = await H.req(server.base, "/v1/notifications/" + id1, {
			session: s.session,
		});
		H.eq("属主取详情 → 200", d.status, 200);
		H.check(
			"详情含完整字段",
			d.json &&
				d.json.id === id1 &&
				d.json.title === "深链验证-构建完成" &&
				d.json.priority === "high" &&
				d.json.ttlSeconds === 3600 &&
				Array.isArray(d.json.deviceTags) &&
				d.json.deviceTags.includes("手机") &&
				Array.isArray(d.json.deliveries),
			JSON.stringify(d.json && d.json.id) + " / " + (d.json && d.json.priority),
		);
		H.eq(
			"未登录取详情 → 401",
			(await H.req(server.base, "/v1/notifications/" + id1)).status,
			401,
		);
		H.eq(
			"不存在 id → 404",
			(
				await H.req(server.base, "/v1/notifications/ntf_no_such", {
					session: s.session,
				})
			).status,
			404,
		);
	}

	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});

	// ---- A/B/C. 详情页完整展示（带 link + 全字段）----
	console.log("--- message.html 完整字段展示 ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));

		await page.goto(server.base + "/message.html?id=" + id1, {
			waitUntil: "load",
			timeout: 15000,
		});
		await page.waitForTimeout(1500);
		const txt = await page.evaluate(() => document.body.innerText);

		H.check("详情页标题", txt.includes("深链验证-构建完成"));
		H.check("详情页正文", txt.includes("共 47 个文件变更"));
		H.check("详情页含 消息ID", txt.includes(id1));
		H.check("详情页含 Agent", txt.includes("deploy-bot"));
		H.check("详情页含 会话", txt.includes("sess_dl_1"));
		H.check("详情页含 项目CWD", txt.includes("/home/z/prod-backend"));
		H.check("详情页含 模型", txt.includes("claude-sonnet-4"));
		H.check("详情页含 优先级(高)", txt.includes("高"));
		H.check("详情页含 路由标签(手机)", txt.includes("手机"));
		H.check("详情页含 TTL", /1 小时|3600/.test(txt));

		// C. link → 「打开 Agent 会话」入口
		const link = await page.evaluate(() => {
			const a = document.getElementById("msg-link-btn");
			return a && !a.classList.contains("hidden")
				? a.getAttribute("href")
				: null;
		});
		H.eq("详情页 link href=上报 link", link, "pi://session/sess_deeplink1");

		// B. 投递记录区存在（无设备匹配 → 空态）
		const dcount = await page.evaluate(
			() => document.getElementById("delivery-count").textContent,
		);
		H.check("投递记录计数渲染", /\d+\/\d+ 已送达/.test(dcount), dcount);

		H.check("详情页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 100));
		await page.close();
		await ctx.close();
	}

	// ---- G. 不存在的 id → 降级 ----
	console.log("--- 未命中 id 降级 ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/message.html?id=ntf_not_exist", {
			waitUntil: "load",
			timeout: 15000,
		});
		await page.waitForTimeout(1500);
		const txt = await page.evaluate(() => document.body.innerText);
		H.check("未命中显示「消息不存在」", txt.includes("消息不存在"));
		H.check(
			"未命中降级无 JS pageerror",
			errs.length === 0,
			errs[0]?.slice(0, 100),
		);
		await page.close();
		await ctx.close();
	}

	// ---- E. 首页弹层：字段补全 + 「查看完整页面」入口 ----
	console.log("--- 首页弹层字段补全 ---");
	{
		const ctx = await browser.newContext();
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/index.html", {
			waitUntil: "load",
			timeout: 15000,
		});
		await page.waitForTimeout(2000);
		// 最新通知置顶：id2（后上报）应在列表第一行
		const firstRow = await page.evaluate(
			() =>
				document.querySelector("#notif-list .notif-row")?.innerText || "",
		);
		H.check(
			"最新通知置顶（第一行 = 后上报的 id2）",
			firstRow.includes("深链验证-无外链"),
			firstRow.slice(0, 60),
		);
		// 点击「无外链」那条（id2）打开弹层
		await page.click("#notif-list .notif-row:has-text('深链验证-无外链')");
		await page.waitForSelector("#detail-modal.flex", { timeout: 5000 });
		const body = await page.evaluate(
			() => document.getElementById("detail-body").innerText,
		);
		H.check("弹层含 消息ID 字段", body.includes(id2), body.slice(0, 120));
		H.check("弹层含 优先级 字段", body.includes("优先级"));
		H.check("弹层含 TTL 字段", body.includes("TTL"));
		H.check("弹层含 路由标签 字段", body.includes("路由标签"));
		const fullLink = await page.evaluate(() => {
			const links = [...document.querySelectorAll("#detail-body a")].map((a) =>
				a.getAttribute("href"),
			);
			return links.find((h) => h && h.startsWith("message.html?id=")) || null;
		});
		H.eq("弹层含「查看完整页面」链接", fullLink, "message.html?id=" + id2);
		H.check(
			"首页弹层无 JS pageerror",
			errs.length === 0,
			errs[0]?.slice(0, 100),
		);
		await page.close();
		await ctx.close();
	}

	// ---- F. 未登录点深链 → next 保留 message.html?id= ----
	console.log("--- 未登录深链 → next 保留 ---");
	{
		const ctx = await browser.newContext();
		const page = await ctx.newPage();
		await page.goto(server.base + "/message.html?id=" + id1, {
			waitUntil: "load",
			timeout: 15000,
		});
		await page.waitForTimeout(2000);
		const u = page.url();
		H.check("未登录点深链 → 跳登录页", u.includes("login.html"), u);
		let next = "";
		try {
			next = new URL(u).searchParams.get("next") || "";
		} catch {
			/* u 非法时 next 为空，下面断言会失败 */
		}
		H.check(
			"next 保留 message.html?id= 深链",
			next.includes("message.html") && next.includes("id=" + id1),
			`next=${next}`,
		);
		await page.close();
		await ctx.close();
	}

	const passed = H.summary("deeplink");
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
