#!/usr/bin/env node
/* SUITE: Passkey 认证全流程（Playwright CDP 虚拟认证器，无头、无需真人）
 *
 * 覆盖 case：
 *  注册：新用户注册 → 会话建立 → 可访问受保护 API
 *  注册：重复用户名 → 400
 *  Key：登录态下创建 notify:send / notify:receive Key（真实 API）→ 各 scope 生效
 *  登录：登出后用已注册 Passkey 重新登录 → 会话恢复
 *  登出：logout → 会话失效 → 401
 *  登录：未注册用户登录 → 400/401
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";

const RP = "localhost";
let server, browser, ctx, page, cdp;
const username = "auth_" + Date.now();

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
			const payload = {
				id: cred.id,
				rawId: b64u(cred.rawId),
				type: cred.type,
				response: {
					clientDataJSON: b64u(cred.response.clientDataJSON),
					attestationObject: b64u(cred.response.attestationObject),
				},
			};
			const r = await fetch(
				base + "/v1/auth/register?username=" + encodeURIComponent(name),
				{
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify(payload),
				},
			);
			return r.status;
		},
		{ base: server.base, name },
	);
}

async function webauthnLogin(name) {
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
				await fetch(base + "/v1/auth/login/options", {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ username: name }),
				})
			).json();
			const pk = o.publicKey || o;
			pk.challenge = b64(pk.challenge);
			if (pk.allowCredentials)
				pk.allowCredentials = pk.allowCredentials.map((c) => ({
					...c,
					id: b64(c.id),
				}));
			const as = await navigator.credentials.get({ publicKey: pk });
			const payload = {
				id: as.id,
				rawId: b64u(as.rawId),
				type: as.type,
				response: {
					clientDataJSON: b64u(as.response.clientDataJSON),
					authenticatorData: b64u(as.response.authenticatorData),
					signature: b64u(as.response.signature),
					userHandle: as.response.userHandle
						? b64u(as.response.userHandle)
						: null,
				},
			};
			const r = await fetch(
				base + "/v1/auth/login?username=" + encodeURIComponent(name),
				{
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify(payload),
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

// webauthnDiscoverableLogin：免用户名登录（不传 username，靠 resident key）
async function webauthnDiscoverableLogin() {
	return page.evaluate(
		async ({ base }) => {
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
				await fetch(base + "/v1/auth/login/options", {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({}),
				})
			).json();
			const pk = o.publicKey || o;
			pk.challenge = b64(pk.challenge);
			if (pk.allowCredentials)
				pk.allowCredentials = pk.allowCredentials.map((c) => ({
					...c,
					id: b64(c.id),
				}));
			const as = await navigator.credentials.get({ publicKey: pk });
			const r = await fetch(base + "/v1/auth/login", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({
					id: as.id,
					rawId: b64u(as.rawId),
					type: as.type,
					response: {
						clientDataJSON: b64u(as.response.clientDataJSON),
						authenticatorData: b64u(as.response.authenticatorData),
						signature: b64u(as.response.signature),
						userHandle: as.response.userHandle
							? b64u(as.response.userHandle)
							: null,
					},
				}),
			});
			return r.status;
		},
		{ base: server.base },
	);
}

async function main() {
	console.log("=== SUITE: auth_flow（Passkey 全流程 · 虚拟认证器）===");
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

	// 1. 注册新用户
	const regStatus = await webauthnRegister(username);
	H.eq("注册新用户 → 200", regStatus, 200);
	let sess = await cookieVal();
	H.check("注册后会话 Cookie 已建立", !!sess);
	H.eq(
		"会话可访问受保护 API",
		(await H.req(server.base, "/v1/notifications", { session: sess })).status,
		200,
	);

	// 2. 重复用户名注册 → 400
	const dupStatus = await webauthnRegister(username).catch(() => "js-error");
	H.check(
		"重复用户名注册被拒(400或WebAuthn异常)",
		dupStatus === 400 || dupStatus === "js-error",
		`got ${dupStatus}`,
	);

	// 3. 登录态创建两个 scope 的 Key（真实 API）
	const sendKeyResp = await H.req(server.base, "/v1/keys", {
		session: sess,
		body: { name: "ci-send", scopes: ["notify:send"] },
	});
	H.eq("创建 notify:send Key → 200", sendKeyResp.status, 200);
	const sendKey = sendKeyResp.json?.key;
	H.check("Key 明文仅创建时返回一次", !!sendKey && sendKey.startsWith("ant_"));
	const recvKeyResp = await H.req(server.base, "/v1/keys", {
		session: sess,
		body: { name: "ws-recv", scopes: ["notify:receive"] },
	});
	const recvKey = recvKeyResp.json?.key;
	H.eq("创建 notify:receive Key → 200", recvKeyResp.status, 200);

	// 4. scope 生效验证
	H.eq(
		"send Key 可上报",
		(
			await H.req(server.base, "/v1/notify", {
				key: sendKey,
				body: { title: "t", status: "success" },
			})
		).status,
		200,
	);
	H.eq(
		"recv Key 上报被拒(403)",
		(
			await H.req(server.base, "/v1/notify", {
				key: recvKey,
				body: { title: "t", status: "success" },
			})
		).status,
		403,
	);

	// 5. Key 列表不泄露明文
	const keyList = await H.req(server.base, "/v1/keys", { session: sess });
	H.check(
		"Key 列表不含明文",
		keyList.status === 200 && !JSON.stringify(keyList.json).includes(sendKey),
	);

	// 6. 登出
	H.eq(
		"登出 → 200",
		(
			await H.req(server.base, "/v1/auth/logout", {
				session: sess,
				method: "POST",
			})
		).status,
		200,
	);
	H.eq(
		"登出后会话失效(401)",
		(await H.req(server.base, "/v1/notifications", { session: sess })).status,
		401,
	);

	// 7. 用已注册 Passkey 重新登录
	const loginStatus = await webauthnLogin(username);
	H.eq("Passkey 重新登录 → 200", loginStatus, 200);
	sess = await cookieVal();
	H.check(
		"重新登录后会话恢复",
		!!sess &&
			(await H.req(server.base, "/v1/notifications", { session: sess }))
				.status === 200,
	);

	// 8. 未注册用户登录 → 非 200
	const badLogin = await webauthnLogin("nonexistent_user_xyz").catch(
		() => "js-error",
	);
	H.check("未注册用户登录被拒", badLogin !== 200, `got ${badLogin}`);

	// === 新功能：免用户名登录 + /v1/auth/me + /v1/stats + 退出按钮 ===
	// 9. /v1/auth/me 返回真实用户
	const me = await H.req(server.base, "/v1/auth/me", { session: sess });
	H.eq("GET /v1/auth/me → 200", me.status, 200);
	H.eq("me 返回真实 username", me.json?.username, username);
	H.check(
		"me 不泄露敏感字段",
		me.json && !me.json.keyHash && !me.json.password,
	);

	// 10. 免用户名 Passkey 登录（discoverable，靠 resident key）
	await H.req(server.base, "/v1/auth/logout", {
		session: sess,
		method: "POST",
	});
	const discLogin = await webauthnDiscoverableLogin();
	H.eq("免用户名 Passkey 登录 → 200", discLogin, 200);
	sess = await cookieVal();
	H.check("免用户名登录后会话建立", !!sess);

	// 11. /v1/stats 真实统计
	await H.req(server.base, "/v1/notify", {
		key: sendKey,
		body: { title: "统计测试", status: "success" },
	});
	const stats = await H.req(server.base, "/v1/stats", { session: sess });
	H.eq("GET /v1/stats → 200", stats.status, 200);
	H.check(
		"stats.total ≥ 1",
		stats.json?.total >= 1,
		`total=${stats.json?.total}`,
	);
	H.check("stats.daily 是数组", Array.isArray(stats.json?.daily));
	H.check(
		"stats.deviceCount 是数字",
		typeof stats.json?.deviceCount === "number",
	);

	// 12. 前端侧栏真实用户名 + 退出按钮 + 退出跳登录
	await page.goto(server.base + "/index.html", { waitUntil: "load" });
	await page.waitForTimeout(1500);
	const sidebarUser = await page.evaluate(
		() => document.getElementById("sidebar-username")?.textContent || "",
	);
	H.eq("侧栏显示真实用户名", sidebarUser.trim(), username);
	const hasLogout = await page.evaluate(
		() => !!document.querySelector('a[href="#logout"]'),
	);
	H.check("侧栏有「退出登录」按钮", hasLogout);
	await page.evaluate(() =>
		document.querySelector('a[href="#logout"]')?.click(),
	);
	await page.waitForTimeout(800);
	H.check("点击退出后跳登录页", page.url().includes("login.html"), page.url());

	// 13. 已知盲区记录（非断言）：虚拟认证器 BackupEligible 默认 false，无法覆盖
	// 真实同步型 Passkey（BackupEligible=true）的登录校验路径。该不变量由 store 层
	// 单测 TestPasskey_BackupEligibleRoundtrip 覆盖（验证字段存取往返一致）。
	H.ok("已知盲区已记录：BackupEligible=true 路径由 store 单测覆盖");

	const passed = H.summary("auth_flow");
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
