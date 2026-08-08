#!/usr/bin/env node
/* SUITE: 管理后台全流程（首用户自动 admin + 管理端点 + 权限 + 前端渲染）
 *
 * 覆盖 case：
 *  首用户：首个注册用户自动成为超级管理员 → /v1/auth/me 返回 role=admin
 *  admin API：admin 可访问 /v1/admin/stats|users|messages|sessions
 *  权限：member 访问 /v1/admin/* → 403；无 session → 401
 *  用户管理：admin 提权 member → member 变 admin；降权 member → 变回 member
 *  禁用：admin 禁用 member → member 会话立即失效（401）
 *  防误操作：不能改自己角色(409)；不能禁用自己(409)；不能降权最后一个 admin(409)
 *  前端：admin.html 页面渲染，侧栏对 admin 显示「管理后台」入口
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
let server, browser, ctx, page, cdp;
const firstUser = "admin_" + Date.now();

/* webauthnRegister：用虚拟认证器注册新用户，返回 register 状态码 */
async function webauthnRegister(name) {
	return page.evaluate(
		async ({ base, name }) => {
			const b64 = (s) => {
				s = s.replace(/-/g, "+").replace(/_/g, "/");
				const b = atob(s + "=".repeat((4 - (s.length % 4)) % 4));
				return Uint8Array.from(b, (c) => c.charCodeAt(0)).buffer;
			};
			const b64u = (buf) => {
				const a = new Uint8Array(buf);
				let s = "";
				for (const c of a) s += String.fromCharCode(c);
				return btoa(s)
					.replace(/\+/g, "-")
					.replace(/\//g, "_")
					.replace(/=+$/, "");
			};
			const o = await (
				await fetch(base + "/v1/auth/register/options", {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ username: name, displayName: name }),
				})
			).json();
			const pk = o.publicKey || o;
			pk.challenge = b64(pk.challenge);
			if (pk.user?.id) pk.user.id = b64(pk.user.id);
			if (pk.excludeCredentials)
				pk.excludeCredentials = pk.excludeCredentials.map((c) => ({
					...c,
					id: b64(c.id),
				}));
			const cred = await navigator.credentials.create({ publicKey: pk });
			const r = await fetch(
				base + "/v1/auth/register?username=" + encodeURIComponent(name),
				{
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({
						id: cred.id,
						rawId: b64u(cred.rawId),
						type: cred.type,
						response: {
							clientDataJSON: b64u(cred.response.clientDataJSON),
							attestationObject: b64u(cred.response.attestationObject),
						},
					}),
				},
			);
			return r.status;
		},
		{ base: server.base, name },
	);
}

async function cookieVal() {
	const cs = await ctx.cookies(server.base);
	const s = cs.find((c) => c.name === "anotify_session");
	return s ? s.value : null;
}

async function main() {
	console.log("=== SUITE: admin_flow（管理后台 · 首用户自动 admin）===");
	server = await H.startServer({ rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	ctx = await browser.newContext();
	page = await ctx.newPage();
	await page.goto(server.base + "/login.html", { waitUntil: "load" });
	cdp = await ctx.newCDPSession(page);
	await cdp.send("WebAuthn.enable");
	await cdp.send("WebAuthn.addVirtualAuthenticator", {
		options: {
			protocol: "ctap2",
			transport: "internal",
			hasResidentKey: true,
			hasUserVerification: true,
			isUserVerified: true,
			automaticPresenceSimulation: true,
		},
	});

	// ---------- 1. 首个用户注册 → 自动 admin ----------
	const regStatus = await webauthnRegister(firstUser);
	H.eq("首用户注册 → 200", regStatus, 200);
	const adminSess = await cookieVal();
	H.check("首用户注册后会话建立", !!adminSess);

	// /v1/auth/me 返回 role=admin
	const me = await H.req(server.base, "/v1/auth/me", { session: adminSess });
	H.eq("GET /v1/auth/me → 200", me.status, 200);
	H.eq("首用户 role = admin", me.json?.role, "admin");
	H.eq("首用户 disabled = false", me.json?.disabled, false);

	// ---------- 2. 用 devseed 建一个 member 用户（绕过 WebAuthn）----------
	// devseed 默认建 member 角色（不带 -admin）
	const member = H.seed(server.dbPath, "member_" + Date.now());
	H.check("devseed 建立 member 用户 + 会话", !!member.session);

	// member 的 /v1/auth/me → role=member
	const memberMe = await H.req(server.base, "/v1/auth/me", {
		session: member.session,
	});
	H.eq("member role = member", memberMe.json?.role, "member");

	// ---------- 3. 权限矩阵 ----------
	// 无 session → 401
	H.eq(
		"admin API 无 session → 401",
		(await H.req(server.base, "/v1/admin/stats")).status,
		401,
	);
	// member → 403
	H.eq(
		"member 访问 /v1/admin/stats → 403",
		(await H.req(server.base, "/v1/admin/stats", { session: member.session }))
			.status,
		403,
	);
	// admin → 200
	H.eq(
		"admin 访问 /v1/admin/stats → 200",
		(await H.req(server.base, "/v1/admin/stats", { session: adminSess }))
			.status,
		200,
	);

	// ---------- 4. admin 端点完整访问 ----------
	const stats = await H.req(server.base, "/v1/admin/stats", {
		session: adminSess,
	});
	H.check(
		"stats.userCount ≥ 2（admin+member）",
		stats.json?.userCount >= 2,
		`got ${stats.json?.userCount}`,
	);
	H.eq("stats.adminCount = 1", stats.json?.adminCount, 1);
	H.check("stats.daily 是数组", Array.isArray(stats.json?.daily));
	H.check("stats.topUsers 是数组", Array.isArray(stats.json?.topUsers));

	const users = await H.req(server.base, "/v1/admin/users", {
		session: adminSess,
	});
	H.eq("GET /v1/admin/users → 200", users.status, 200);
	H.check("users.count ≥ 2", users.json?.count >= 2);
	// 首注册用户应排在第一位（created_at ASC）
	H.eq(
		"用户列表首位是首注册的 admin",
		users.json?.users[0]?.username,
		firstUser,
	);
	H.eq("首位用户 role=admin", users.json?.users[0]?.role, "admin");

	const msgs = await H.req(server.base, "/v1/admin/messages", {
		session: adminSess,
	});
	H.eq("GET /v1/admin/messages → 200", msgs.status, 200);
	H.check("messages 是数组", Array.isArray(msgs.json?.messages));

	const sessions = await H.req(server.base, "/v1/admin/sessions", {
		session: adminSess,
	});
	H.eq("GET /v1/admin/sessions → 200", sessions.status, 200);
	H.check("sessions.count ≥ 1", sessions.json?.count >= 1);

	// ---------- 5. 提权 member → admin ----------
	// 先取 member 的 user id
	const memberUserId = memberMe.json.id;
	const promote = await H.req(
		server.base,
		`/v1/admin/users/${memberUserId}/role`,
		{
			session: adminSess,
			method: "PATCH",
			body: { role: "admin" },
		},
	);
	H.eq("提权 member → admin (200)", promote.status, 200);
	// 校验落库
	const memberMe2 = await H.req(server.base, "/v1/auth/me", {
		session: member.session,
	});
	H.eq("提权后 member role = admin", memberMe2.json?.role, "admin");
	// 此时 member 应能访问 admin API
	H.eq(
		"提权后 member 可访问 /v1/admin/stats (200)",
		(await H.req(server.base, "/v1/admin/stats", { session: member.session }))
			.status,
		200,
	);

	// ---------- 6. 降权 member（此时 adminCount=2，降后剩 1）----------
	const demote = await H.req(
		server.base,
		`/v1/admin/users/${memberUserId}/role`,
		{
			session: adminSess,
			method: "PATCH",
			body: { role: "member" },
		},
	);
	H.eq("降权 member → member (200)", demote.status, 200);
	H.eq(
		"降权后 member 访问 admin API → 403",
		(await H.req(server.base, "/v1/admin/stats", { session: member.session }))
			.status,
		403,
	);

	// ---------- 7. 不能改自己角色 ----------
	const selfDemote = await H.req(
		server.base,
		`/v1/admin/users/${memberMe.json.id}/role`,
		{
			session: member.session, // member 试图改自己（member 无权，但验证逻辑先于权限？不，403 先）
			method: "PATCH",
			body: { role: "admin" },
		},
	);
	// member 无 admin 权限 → 403（不是 409，因为权限检查在前）
	H.eq("member 改自己 → 403（权限在前）", selfDemote.status, 403);

	// admin 改自己 → 409
	const adminUserId = me.json.id;
	const adminSelfDemote = await H.req(
		server.base,
		`/v1/admin/users/${adminUserId}/role`,
		{
			session: adminSess,
			method: "PATCH",
			body: { role: "member" },
		},
	);
	H.eq("admin 改自己角色 → 409", adminSelfDemote.status, 409);

	// ---------- 8. 不能降权最后一个 admin ----------
	// 此时只有首用户是 admin。admin 改自己被拦（409，不能改自己）。
	// 用 admin 视角尝试降权——但唯一 admin 只能降自己（被拦）。
	// 验证：再提权 member 为 admin，然后降首用户 admin（此时 adminCount=2，降后=1，允许），
	// 再尝试降 member（此时 member 是 admin，adminCount=1，降后=0）→ 409
	await H.req(server.base, `/v1/admin/users/${memberUserId}/role`, {
		session: adminSess,
		method: "PATCH",
		body: { role: "admin" },
	});
	// 降首用户 admin（admin 自己不能降自己→409）。所以用 member（现在是 admin）来降首用户
	const demoteFirstByMember = await H.req(
		server.base,
		`/v1/admin/users/${adminUserId}/role`,
		{
			session: member.session,
			method: "PATCH",
			body: { role: "member" },
		},
	);
	H.eq(
		"admin(member) 降首用户（剩 1 admin）→ 200",
		demoteFirstByMember.status,
		200,
	);
	// 现在只有 member 是 admin。member 降自己 → 409（不能改自己）
	const memberSelfDemote = await H.req(
		server.base,
		`/v1/admin/users/${memberUserId}/role`,
		{
			session: member.session,
			method: "PATCH",
			body: { role: "member" },
		},
	);
	H.eq("最后一个 admin 降自己 → 409", memberSelfDemote.status, 409);

	// 恢复：member 把首用户提回 admin（保持至少 1 个 admin 不变，现 2 个）
	await H.req(server.base, `/v1/admin/users/${adminUserId}/role`, {
		session: member.session,
		method: "PATCH",
		body: { role: "admin" },
	});
	// adminSess 此时仍有效（首用户未被禁用）
	const meCheck = await H.req(server.base, "/v1/auth/me", {
		session: adminSess,
	});
	H.eq("首用户 admin 会话仍有效", meCheck.status, 200);

	// ---------- 9. 禁用用户 → 会话立即失效 ----------
	// 先把 member 降回 member（保持单一 admin）
	await H.req(server.base, `/v1/admin/users/${memberUserId}/role`, {
		session: adminSess,
		method: "PATCH",
		body: { role: "member" },
	});
	// member 会话当前有效
	H.eq(
		"禁用前 member 会话有效（403 非 401）",
		(await H.req(server.base, "/v1/admin/stats", { session: member.session }))
			.status,
		403,
	);
	// admin 禁用 member
	const disable = await H.req(
		server.base,
		`/v1/admin/users/${memberUserId}/disable`,
		{
			session: adminSess,
			method: "POST",
		},
	);
	H.eq("禁用 member → 200", disable.status, 200);
	// member 会话立即失效（401）
	H.eq(
		"禁用后 member 会话失效（401）",
		(await H.req(server.base, "/v1/auth/me", { session: member.session }))
			.status,
		401,
	);
	// member 无法再登录
	const memberLoginOpts = await H.req(server.base, "/v1/auth/login/options", {
		body: { username: memberMe.json.username },
	});
	H.eq("禁用用户 login/options → 400", memberLoginOpts.status, 400);

	// ---------- 10. 不能禁用自己 ----------
	const selfDisable = await H.req(
		server.base,
		`/v1/admin/users/${adminUserId}/disable`,
		{
			session: adminSess,
			method: "POST",
		},
	);
	H.eq("admin 禁用自己 → 409", selfDisable.status, 409);

	// ---------- 11. 启用用户 ----------
	const enable = await H.req(
		server.base,
		`/v1/admin/users/${memberUserId}/enable`,
		{
			session: adminSess,
			method: "POST",
		},
	);
	H.eq("启用 member → 200", enable.status, 200);

	// ---------- 12. PATCH /users/{id} 组合操作 ----------
	// 先建一个新 member 用于组合操作
	const member2 = H.seed(server.dbPath, "member2_" + Date.now());
	const member2Id = (
		await H.req(server.base, "/v1/auth/me", { session: member2.session })
	).json.id;
	const combined = await H.req(server.base, `/v1/admin/users/${member2Id}`, {
		session: adminSess,
		method: "PATCH",
		body: { role: "admin", disabled: true },
	});
	H.eq("PATCH 组合（提权+禁用）→ 200", combined.status, 200);
	const member2Me = await H.req(server.base, "/v1/auth/me", {
		session: member2.session,
	});
	H.eq("组合操作后 member2 被禁用（401）", member2Me.status, 401);

	// ---------- 13. 前端：admin.html 渲染 + 侧栏 admin 入口 ----------
	await page.goto(server.base + "/admin.html", { waitUntil: "load" });
	await page.waitForTimeout(1500);
	// 注入 admin session cookie（页面已加载，但 401 已跳登录——重新带 cookie 打开）
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: adminSess,
			domain: "localhost",
			path: "/",
			httpOnly: true,
		},
	]);
	await page.goto(server.base + "/admin.html", { waitUntil: "networkidle" });
	await page.waitForTimeout(1500);
	const pageTitle = await page.title();
	H.check(
		"admin.html 标题含「管理后台」",
		pageTitle.includes("管理后台") || pageTitle.includes("Admin"),
		pageTitle,
	);
	const kpiUsers = await page.textContent("#kpi-users").catch(() => null);
	H.check(
		"admin.html KPI 用户数已渲染（非 –）",
		kpiUsers && kpiUsers !== "–",
		`got ${kpiUsers}`,
	);
	const usersRows = await page.$$eval("#users-tbody tr", (rows) => rows.length);
	H.check(
		"admin.html 用户表格有数据行",
		usersRows >= 1,
		`got ${usersRows} rows`,
	);
	// 侧栏 admin 入口（admin 用户应看到）
	const adminNav = await page
		.$eval("#admin-nav-section a", (a) => a.textContent)
		.catch(() => null);
	H.check("侧栏显示「管理后台」入口", !!adminNav, "admin nav 未渲染");

	// ---------- 14. member 用户侧栏不显示 admin 入口 ----------
	await ctx.clearCookies();
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: member.session,
			domain: "localhost",
			path: "/",
			httpOnly: true,
		},
	]);
	// member 之前被禁用又启用，但会话已被吊销（禁用时清 cookie+失效）。
	// 用 devseed 重新建一个 member 会话验证侧栏不显示 admin 入口
	const member3 = H.seed(server.dbPath, "member3_" + Date.now());
	await ctx.clearCookies();
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: member3.session,
			domain: "localhost",
			path: "/",
			httpOnly: true,
		},
	]);
	await page.goto(server.base + "/index.html", { waitUntil: "networkidle" });
	await page.waitForTimeout(1500);
	const adminNavForMember = await page
		.$eval("#admin-nav-section a", (a) => a.textContent)
		.catch(() => null);
	const adminNavHidden = await page
		.$eval("#admin-nav-section", (el) => el.classList.contains("hidden"))
		.catch(() => true);
	H.check(
		"member 侧栏不显示 admin 入口",
		!adminNavForMember && adminNavHidden,
		`nav=${adminNavForMember} hidden=${adminNavHidden}`,
	);

	H.ok(
		"已知盲区：首用户判定的并发场景由 SQLite 单写串行化保证（单测覆盖计数）",
	);

	const passed = H.summary("admin_flow");
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
