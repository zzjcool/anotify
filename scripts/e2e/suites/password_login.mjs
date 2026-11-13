#!/usr/bin/env node
/* SUITE: password_login —— 密码登录（Passkey 之外的第二套登录模型）全流程
 *
 * 契约：.pi-orchestrator/password-login-contract.md（§2 HTTP / §6 DOM / §7 e2e 锚点）
 *
 * 覆盖：
 *  注册（Passkey）→ 密码状态初始 hasPassword=false → 安全页设置密码 → hasPassword=true
 *  /v1/auth/me 不泄露 passwordHash / argon2id（密码哈希永不外泄）
 *  登录页密码登录：错密码留在登录页 + 有错误提示；正确密码进入工作台
 *  两套模型并存：登出后 Passkey 登录仍然 200（Passkey 未被密码功能破坏）
 *  安全页改密码（修改态必须填当前密码）→ 旧密码失效 / 新密码生效
 *  已设置密码 → 允许删除最后一个 Passkey（不是无条件 409）
 *  无 Passkey → 移除密码 409（至少保留一种登录方式），密码没有被清掉
 *
 * 全部走真后端 + 真 Chrome（CDP 虚拟认证器），没有 mock。
 * 密码登录尝试合计 6 次（服务端限速 10/min/IP + 10/min/username，留足余量）。
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

// v4 把命名空间放进了路径：任务列表是 `/v1/namespaces/{ns}/tasks`。
// 会话鉴权边界不变（未登出 200 / 登出 401），只是路径要带 ns。
const NS = V.DEFAULT_NS;

const RP = "localhost";
const SUITE = "password_login";

// 两个密码都由本套件自己设置（不是别人写死的口令），P1 设置 → P2 修改。
const P1 = "Passw0rd!123";
const P2 = "N3wPass!456";

let server, browser, ctx, page, cdp;
const username = "pwd_" + Date.now();
const pageErrors = [];

/* ---------- WebAuthn helpers（与 auth_flow 同一套虚拟认证器写法） ---------- */

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

/* ---------- 小工具 ---------- */

// set-cookie → session（API 直接登录时拿会话，不走浏览器）
function sessionFromSetCookie(raw) {
	const m = /anotify_session=([^;,\s]+)/.exec(String(raw || ""));
	return m ? m[1] : null;
}

// goto 的 ERR_ABORTED 重试。
//
// 为什么需要：登出后页面上还在跑的客户端守卫（partials.js 的 api() 401）会自己
// 跳 login.html?next=…，与套件紧随其后的 page.goto 撞车时，Chrome 会把**前一次**
// 导航 abort 掉（net::ERR_ABORTED）。这不是产品 bug，也不是断言该让步的地方——
// 只对这一个错误重试，其它导航失败（404 / 连接失败）照常抛。
async function goto(url, opts = {}) {
	// 先等当前文档 load 完成：上一次导航（登录成功后的 navigate、受保护页守卫）
	// 还在进行时直接 goto，会把那次导航 abort 掉；Chrome 的跨文档 View Transition
	// 会把被 abort 的过渡报成 pageerror（"Transition was skipped. Navigation aborted"）。
	// 等 load 是幂等的（已 load 立即返回），不改变任何断言。
	await page.waitForLoadState("load").catch(() => {});
	for (let attempt = 0; attempt < 3; attempt++) {
		try {
			await page.goto(url, { waitUntil: "load", ...opts });
			return;
		} catch (e) {
			const msg = String((e && e.message) || e);
			if (!msg.includes("ERR_ABORTED") || attempt === 2) throw e;
			await page.waitForTimeout(300);
		}
	}
}

// fillStable 稳定地填密码弹层：Chrome 的密码管理器会与 fill 抢同一个字段
// （实测：current-password 被自动填充/改写、new-password 被异步清空），
// 单次 fill 之后回读可能拿到的不是我们写的值。这里「填 → 回读 → 不一致再填」，
// 最多 4 轮；最终值仍不一致就返回 false，由断言把它曝光成失败。
//
// 顺序有意义：先 new/confirm、最后 current —— Chrome 在「当前密码」变化时会
// 清空 new-password 类字段，把 current 放最后就不会把刚填的新密码抹掉。
async function fillStable(fields) {
	for (let attempt = 0; attempt < 4; attempt++) {
		for (const [sel, val] of fields) await page.fill(sel, val);
		await page.waitForTimeout(120);
		const got = await page.evaluate(
			(sels) => sels.map((s) => document.querySelector(s)?.value ?? null),
			fields.map((f) => f[0]),
		);
		if (got.every((v, i) => v === fields[i][1])) return true;
	}
	return false;
}

// 轮询元素文本，直到包含 needle（超时返回最后一次读到的文本，交给断言判）
async function waitForText(selector, needle, timeout = 8000) {
	const deadline = Date.now() + timeout;
	let text = "";
	while (Date.now() < deadline) {
		text = await page
			.evaluate(
				(sel) => document.querySelector(sel)?.textContent ?? "",
				selector,
			)
			.catch(() => "");
		if (text.includes(needle)) return text;
		await page.waitForTimeout(80);
	}
	return text;
}

// 轮询元素文本，直到非空（用于「有错误提示」这类断言）
async function waitForNonEmptyText(selector, timeout = 8000) {
	const deadline = Date.now() + timeout;
	let text = "";
	while (Date.now() < deadline) {
		text = await page
			.evaluate(
				(sel) => document.querySelector(sel)?.textContent ?? "",
				selector,
			)
			.catch(() => "");
		if (text.trim()) return text;
		await page.waitForTimeout(80);
	}
	return text;
}

// 导航到工作台：登录成功后的目标是 index.html，但服务端会把
// `/index.html` 规范化成 `/`（internal/server/static.go:277，保留查询串），
// 所以两形都算“进工作台”——断言不该反过来逼产品不重定向。
async function waitForWorkspaceNav(timeout = 15000) {
	const deadline = Date.now() + timeout;
	let url = page.url();
	while (Date.now() < deadline) {
		url = page.url();
		let pathname = "";
		try {
			pathname = new URL(url).pathname;
		} catch {
			/* 过渡态先跳过 */
		}
		if (pathname === "/" || pathname.endsWith("/index.html")) return url;
		await page.waitForTimeout(80);
	}
	return url;
}

// 等元素隐藏（含关闭动画：closeModal 有 --dur-modal-out 的退场时长）
async function waitForHidden(selector, timeout = 8000) {
	const deadline = Date.now() + timeout;
	while (Date.now() < deadline) {
		if (!(await page.isVisible(selector).catch(() => false))) return true;
		await page.waitForTimeout(80);
	}
	return false;
}

async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: password_login（密码登录 · 真后端 + 真 Chrome · 虚拟认证器）===",
	);
	server = await H.startServer({ suiteName: SUITE, rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	// 根路径页面是 zh-CN（<html lang="zh-CN"> + /i18n.zh-CN.js），
	// locale 只是把 navigator.language 也对齐，避免语言提示条干扰交互。
	ctx = await browser.newContext({ locale: "zh-CN" });
	await H.mockExternalCdn(ctx);
	page = await ctx.newPage();
	page.on("pageerror", (e) =>
		pageErrors.push(`${String(e)} @ ${page.url()}`),
	);
	await goto(server.base + "/login.html");
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

	// ---------- 1/2. 注册新用户（注册仍是 Passkey ceremony） ----------
	const regStatus = await webauthnRegister(username);
	H.eq("注册新用户（Passkey）→ 200", regStatus, 200);
	let sess = await cookieVal();
	H.check("注册后会话 Cookie 已建立", !!sess);

	// ---------- 3. 新账号还没有密码，且 me 不泄露哈希 ----------
	const st0 = await H.req(server.base, "/v1/auth/password", {
		session: sess,
	});
	H.eq("GET 密码状态 → 200", st0.status, 200);
	H.eq("新账号 hasPassword === false", st0.json?.hasPassword, false);
	const me0 = await H.req(server.base, "/v1/auth/me", { session: sess });
	H.eq("GET /v1/auth/me → 200", me0.status, 200);
	const meText = JSON.stringify(me0.json || {});
	H.check(
		"me 不泄露 passwordHash",
		!/password[\s_-]*hash/i.test(meText),
		meText.slice(0, 200),
	);
	H.check(
		"me 不泄露 argon2id 哈希",
		!/argon2id/i.test(meText),
		meText.slice(0, 200),
	);

	// ---------- 4. UI：安全页设置密码 ----------
	await goto(server.base + "/security.html");
	await H.waitForAppReady(page, "workspace", { dataAnchor: "#password-status" });
	let statusText = await waitForText("#password-status", "未设置");
	H.check(
		"安全页未设置密码时显示「未设置」",
		statusText.includes("未设置"),
		statusText,
	);
	H.check(
		"未设置密码时 #password-remove-btn 隐藏",
		!(await page.isVisible("#password-remove-btn")),
	);
	const setLabel = (await page.textContent("#password-edit-btn")) || "";
	H.check(
		"未设置时 #password-edit-btn 文案为「设置密码」",
		setLabel.includes("设置密码"),
		setLabel,
	);

	await page.click("#password-edit-btn");
	await page.waitForSelector("#password-modal:not(.hidden)", { timeout: 5000 });
	H.check(
		"点「设置密码」后 #password-modal 可见",
		await page.isVisible("#password-modal"),
	);
	const setTitle = (await page.textContent("#password-modal-title")) || "";
	H.check(
		"设置态弹层标题为「设置密码」",
		setTitle.includes("设置密码"),
		setTitle,
	);
	const filledSet = await fillStable([
		["#pwd-new", P1],
		["#pwd-confirm", P1],
	]);
	H.check("设置密码表单字段值已就位（未被密码管理器改写）", filledSet);
	await page.click("#password-modal-submit");

	const toastText = await waitForText("#toast", "密码已保存", 12000);
	const setErr =
		(await page.textContent("#password-modal-error").catch(() => "")) || "";
	H.check(
		"设置成功有「密码已保存」toast",
		toastText.includes("密码已保存"),
		`toast=${JSON.stringify(toastText)} modalError=${JSON.stringify(setErr)}`,
	);
	H.check("设置成功后 #password-modal 关闭", await waitForHidden("#password-modal"));
	statusText = await waitForText("#password-status", "已设置");
	H.check(
		"设置后安全页显示「已设置」",
		statusText.includes("已设置"),
		statusText,
	);
	H.check(
		"设置后 #password-remove-btn 不再隐藏",
		await page.isVisible("#password-remove-btn"),
	);

	// ---------- 5. API：状态真的是 hasPassword=true ----------
	const st1 = await H.req(server.base, "/v1/auth/password", {
		session: sess,
	});
	H.eq("GET 密码状态 → 200（设置后）", st1.status, 200);
	H.eq("设置后 hasPassword === true", st1.json?.hasPassword, true);
	H.check(
		"设置后 updatedAt > 0",
		Number(st1.json?.updatedAt) > 0,
		`updatedAt=${st1.json?.updatedAt}`,
	);

	// ---------- 6. 登出 → 登录页用密码登录（错 → 对） ----------
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
		"登出后 …/tasks → 401",
		(await H.req(server.base, V.P.tasks(NS), { session: sess })).status,
		401,
	);

	await goto(server.base + "/login.html");
	await H.waitForAppReady(page, "login");
	// #381：首次访问（本浏览器没有本站登录记录）+ 401 时，登录页默认落在「注册」
	// tab（见 web-src/pages/login.html 的 applyFirstRunDefault）。这里要测的是密码
	// **登录**，所以先显式切回登录 tab —— 顺便把切 tab 本身也真实验证一次
	// （断言没删，只是补上了新的必经交互）。
	await page.click("#tab-login");
	await page.waitForSelector("#login-panel:not(.hidden)", { timeout: 5000 });
	H.check(
		"切到登录 tab 后 #login-panel 可见",
		await page.isVisible("#login-panel"),
	);
	await page.click("#password-login-toggle");
	await page.waitForSelector("#password-panel:not(.hidden)", { timeout: 5000 });
	H.check(
		"点「用密码登录」后 #password-panel 可见",
		await page.isVisible("#password-panel"),
	);
	H.eq(
		"展开后 aria-expanded === true",
		await page.getAttribute("#password-login-toggle", "aria-expanded"),
		"true",
	);

	// 错密码（第 1 次密码登录尝试）
	await page.fill("#password-username", username);
	await page.fill("#password-password", "DefinitelyWrong!1");
	await page.click("#password-login-btn");
	const wrongMsg = await waitForNonEmptyText("#status-box");
	H.check(
		"错密码有可见错误提示",
		wrongMsg.trim().length > 0,
		`status-box="${wrongMsg}"`,
	);
	H.check(
		"错密码后仍停留在登录页",
		page.url().includes("login.html"),
		page.url(),
	);
	H.check(
		"错密码提示体现失败原因（失败 / 401 / 错误）",
		/失败|401|错误/.test(wrongMsg),
		`status-box="${wrongMsg}"`,
	);

	// 对密码（第 2 次密码登录尝试）→ 跳工作台
	await page.fill("#password-username", username);
	await page.fill("#password-password", P1);
	await page.click("#password-login-btn");
	const landed = await waitForWorkspaceNav();
	let landedPath = "";
	try {
		landedPath = new URL(landed).pathname;
	} catch {
		/* 保持空串：下面会报失败并带上原始 URL */
	}
	H.check(
		"正确密码登录后进入工作台（index.html，服务端规范化为 /）",
		landedPath === "/" || landedPath.endsWith("/index.html"),
		landed,
	);
	H.check(
		"进入工作台后侧栏已挂载",
		await page
			.waitForSelector("#sidebar", { timeout: 10000 })
			.then(() => true)
			.catch(() => false),
	);
	sess = await cookieVal();
	H.check("密码登录后浏览器会话 Cookie 已建立", !!sess);
	const me1 = await H.req(server.base, "/v1/auth/me", { session: sess });
	H.eq("密码登录后 GET /v1/auth/me → 200", me1.status, 200);
	H.eq("密码登录后 me.username 正确", me1.json?.username, username);

	// ---------- 7. 两套模型并存：Passkey 登录未被破坏 ----------
	H.eq(
		"登出（Passkey 登录前）→ 200",
		(
			await H.req(server.base, "/v1/auth/logout", {
				session: sess,
				method: "POST",
			})
		).status,
		200,
	);
	await goto(server.base + "/login.html");
	H.eq(
		"Passkey 登录仍然 → 200（两套模型并存）",
		await webauthnLogin(username),
		200,
	);
	sess = await cookieVal();
	H.check(
		"Passkey 登录后会话可访问受保护 API",
		!!sess &&
			(await H.req(server.base, V.P.tasks(NS), { session: sess })).status === 200,
	);

	// ---------- 8. UI：安全页改密码（修改态必须填当前密码） ----------
	await goto(server.base + "/security.html");
	await H.waitForAppReady(page, "workspace", { dataAnchor: "#password-status" });
	statusText = await waitForText("#password-status", "已设置");
	H.check(
		"改密码前状态为「已设置」",
		statusText.includes("已设置"),
		statusText,
	);
	const changeLabel =
		(await waitForText("#password-edit-btn", "修改密码")) || "";
	H.check(
		"已设置时 #password-edit-btn 文案为「修改密码」",
		changeLabel.includes("修改密码"),
		changeLabel,
	);

	await page.click("#password-edit-btn");
	await page.waitForSelector("#password-modal:not(.hidden)", { timeout: 5000 });
	H.check(
		"修改态显示当前密码字段 #pwd-current",
		await page.isVisible("#pwd-current"),
	);
	const filledChange = await fillStable([
		["#pwd-new", P2],
		["#pwd-confirm", P2],
		["#pwd-current", P1],
	]);
	H.check(
		"改密码表单三个字段值已就位（未被密码管理器改写）",
		filledChange,
	);
	// 抓这一次 POST 的响应：400/401/500 与响应体是排障的关键证据
	// （否则只剩「toast 没出现」这种没有信息量的失败）。
	const respP = page
		.waitForResponse(
			(r) =>
				r.url().includes("/v1/auth/") &&
				r.url().endsWith("/password") &&
				r.request().method() === "POST",
			{ timeout: 15000 },
		)
		.catch(() => null);
	await page.click("#password-modal-submit");
	const changeResp = await respP;
	// 只要状态码：response.text() 在「body 已被页面 fetch 消费」时会等不到 EOF
	// （实测会把套件挂死），错误文案由 #password-modal-error 提供。
	const respInfo = changeResp ? `status=${changeResp.status()}` : "status=NO_RESPONSE";
	const changeToast = await waitForText("#toast", "密码已保存", 12000);
	// 失败时把弹层里的错误一起带进 detail：否则「toast 没出现」这条断言
	// 只剩一个空 detail，看不出是 400（当前密码错）/ 500 / 请求没发出去。
	const changeErr =
		(await page.textContent("#password-modal-error").catch(() => "")) || "";
	H.check(
		"改密码成功有「密码已保存」toast",
		changeToast.includes("密码已保存"),
		`toast=${JSON.stringify(changeToast)} modalError=${JSON.stringify(changeErr)} ${respInfo}`,
	);
	H.check(
		"改密码成功后 #password-modal 关闭",
		await waitForHidden("#password-modal"),
	);
	statusText = await waitForText("#password-status", "已设置");
	H.check(
		"改密码后状态仍为「已设置」",
		statusText.includes("已设置"),
		statusText,
	);

	// ---------- 9. 旧密码失效 / 新密码生效 ----------
	H.eq(
		"登出（改密码后）→ 200",
		(
			await H.req(server.base, "/v1/auth/logout", {
				session: sess,
				method: "POST",
			})
		).status,
		200,
	);
	// 第 3 次密码登录尝试：旧密码 → 401
	const oldPw = await H.req(server.base, "/v1/auth/password/login", {
		method: "POST",
		body: { username, password: P1 },
	});
	H.eq("旧密码登录 → 401", oldPw.status, 401);
	// 第 4 次密码登录尝试：新密码 → 200
	const newPw = await H.req(server.base, "/v1/auth/password/login", {
		method: "POST",
		body: { username, password: P2 },
	});
	H.eq("新密码登录 → 200", newPw.status, 200);
	sess = sessionFromSetCookie(newPw.headers["set-cookie"]);
	H.check(
		"新密码登录返回会话 Cookie",
		!!sess &&
			(await H.req(server.base, "/v1/auth/me", { session: sess })).status === 200,
	);

	// ---------- 10. 已设置密码时允许删最后一个 Passkey ----------
	const pkList = await H.req(server.base, "/v1/auth/passkeys", { session: sess });
	H.eq("GET /v1/auth/passkeys → 200", pkList.status, 200);
	const passkeys = pkList.json?.passkeys || [];
	H.eq("账号当前只有 1 个 Passkey", passkeys.length, 1);
	const pkId = passkeys[0]?.id;
	H.eq(
		"已设置密码时删除最后一个 Passkey → 200",
		(
			await H.req(
				server.base,
				"/v1/auth/passkeys/" + encodeURIComponent(String(pkId)),
				{ session: sess, method: "DELETE" },
			)
		).status,
		200,
	);

	// ---------- 11. 纯密码账号可登录（Passkey 已删光） ----------
	H.eq(
		"登出（删 Passkey 后）→ 200",
		(
			await H.req(server.base, "/v1/auth/logout", {
				session: sess,
				method: "POST",
			})
		).status,
		200,
	);
	// 第 5 次密码登录尝试
	const pureLogin = await H.req(
		server.base,
		"/v1/auth/password/login",
		{ method: "POST", body: { username, password: P2 } },
	);
	H.eq("纯密码账号（0 个 Passkey）可登录 → 200", pureLogin.status, 200);
	sess = sessionFromSetCookie(pureLogin.headers["set-cookie"]);
	H.check("纯密码登录返回会话 Cookie", !!sess);

	// ---------- 12. 无 Passkey 时移除密码 → 409，且密码没被清掉 ----------
	const removeResp = await H.req(server.base, "/v1/auth/password", {
		session: sess,
		method: "DELETE",
		body: { currentPassword: P2 },
	});
	H.eq("无 Passkey 时移除密码 → 409", removeResp.status, 409);
	H.check(
		"409 文案说明「至少保留一种登录方式」",
		String(removeResp.json?.error || "").includes("至少保留一种登录方式"),
		String(removeResp.json?.error || ""),
	);
	const st2 = await H.req(server.base, "/v1/auth/password", {
		session: sess,
	});
	H.eq("移除失败后 hasPassword 仍 === true", st2.json?.hasPassword, true);
	// 第 6 次密码登录尝试：失败操作没有清掉密码
	const stillLogin = await H.req(
		server.base,
		"/v1/auth/password/login",
		{ method: "POST", body: { username, password: P2 } },
	);
	H.eq("移除失败后密码登录仍 200", stillLogin.status, 200);

	// ---------- 13. 用密码注册（第二种注册方式） ----------
	// 当前会话属于纯密码账号（步骤 12 之后密码仍是 P2），先登出再走注册 UI。
	const signupUser = "pwd_signup_" + Date.now();
	const SIGNUP_PASS = "Signup!Passw0rd";
	await H.req(server.base, "/v1/auth/logout", {
		session: sess,
		method: "POST",
	});

	await goto(server.base + "/login.html");
	await H.waitForAppReady(page, "login");
	await page.click("#tab-signup");
	await page.waitForSelector("#signup-panel:not(.hidden)", { timeout: 5000 });
	H.check(
		"切到注册 tab 后 #signup-panel 可见",
		await page.isVisible("#signup-panel"),
	);
	H.check(
		"密码注册面板默认收起",
		!(await page.isVisible("#password-signup-panel")),
	);

	await page.fill("#new-username", signupUser);
	await page.click("#password-signup-toggle");
	await page.waitForSelector("#password-signup-panel:not(.hidden)", {
		timeout: 5000,
	});
	H.eq(
		"展开密码注册面板后 aria-expanded === true",
		await page.getAttribute("#password-signup-toggle", "aria-expanded"),
		"true",
	);
	const filledSignup = await fillStable([
		["#signup-password", SIGNUP_PASS],
		["#signup-password-confirm", SIGNUP_PASS],
	]);
	H.check("密码注册表单字段值已就位（未被密码管理器改写）", filledSignup);
	// 在用户名框里按 Enter：密码注册面板已展开，回车必须走密码注册，
	// 不能退回 Passkey ceremony（后者会建出 1 个 Passkey，后面的断言会抓到）。
	await page.click("#new-username");
	await page.keyboard.press("Enter");

	// 密码注册成功同样展示一次性恢复码（复用 #codes-modal）
	const codesShown = await page
		.waitForSelector("#codes-modal:not(.hidden)", { timeout: 12000 })
		.then(() => true)
		.catch(() => false);
	H.check("密码注册成功后展示一次性恢复码", codesShown);
	const signupCodeCount = await page.evaluate(
		() => document.querySelectorAll("#codes-grid .code-row").length,
	);
	H.eq("恢复码一次展示 8 个", signupCodeCount, 8);
	// #381：恢复码弹窗加了「我已保存」硬门禁 —— 未勾选时 #codes-continue 真的是 disabled
	// （置灰 + 点不动），所以这里先勾上。断言不删，只补上新的必经交互。
	H.check(
		"恢复码未勾「我已保存」前「继续」禁用",
		await page.isDisabled("#codes-continue"),
	);
	await page.check("#codes-saved-ack");
	H.check(
		"勾选“我已保存”后「继续」可用",
		await page.isEnabled("#codes-continue"),
	);
	await page.click("#codes-continue");
	const signupLanded = await waitForWorkspaceNav();
	let signupPath = "";
	try {
		signupPath = new URL(signupLanded).pathname;
	} catch {
		/* 保持空串：下面会报失败并带上原始 URL */
	}
	H.check(
		"密码注册后进入工作台",
		signupPath === "/" || signupPath.endsWith("/index.html"),
		signupLanded,
	);
	await page.waitForLoadState("load").catch(() => {});

	const signupSess = await cookieVal();
	H.check("密码注册后会话 Cookie 已建立", !!signupSess);
	const me2 = await H.req(server.base, "/v1/auth/me", { session: signupSess });
	H.eq("密码注册后 GET /v1/auth/me → 200", me2.status, 200);
	H.eq("密码注册的 username 正确", me2.json?.username, signupUser);
	const pk2 = await H.req(server.base, "/v1/auth/passkeys", {
		session: signupSess,
	});
	H.eq("密码注册后 GET /v1/auth/passkeys → 200", pk2.status, 200);
	H.check(
		"密码注册账号没有 Passkey（空数组 []）",
		Array.isArray(pk2.json?.passkeys) && pk2.json.passkeys.length === 0,
		JSON.stringify(pk2.json?.passkeys),
	);
	const st3 = await H.req(server.base, "/v1/auth/password", {
		session: signupSess,
	});
	H.eq("密码注册后 hasPassword === true", st3.json?.hasPassword, true);

	// 重复用户名 / 过短密码（API 级；注册端点 IP 限速 10/min，本套件共 3 次）
	const dupSignup = await H.req(server.base, "/v1/auth/password/register", {
		method: "POST",
		body: { username: signupUser, password: SIGNUP_PASS },
	});
	H.eq("重复用户名密码注册 → 400", dupSignup.status, 400);
	H.check(
		"重复用户名文案不含数据库约束细节",
		!/UNIQUE|constraint|insert user/i.test(String(dupSignup.json?.error || "")),
		String(dupSignup.json?.error || ""),
	);
	const weakSignup = await H.req(server.base, "/v1/auth/password/register", {
		method: "POST",
		body: { username: signupUser + "_weak", password: "short" },
	});
	H.eq("过短密码注册 → 400", weakSignup.status, 400);

	// 登出后用密码登录这个新账号
	await H.req(server.base, "/v1/auth/logout", {
		session: signupSess,
		method: "POST",
	});
	const signupLogin = await H.req(server.base, "/v1/auth/password/login", {
		method: "POST",
		body: { username: signupUser, password: SIGNUP_PASS },
	});
	H.eq("密码注册的账号可密码登录 → 200", signupLogin.status, 200);
	H.check(
		"密码注册账号登录返回会话 Cookie",
		!!sessionFromSetCookie(signupLogin.headers["set-cookie"]),
	);

	// ---------- 14. 前端无未捕获异常 ----------
	H.check(
		"UI 流程无未捕获前端异常（pageerror）",
		pageErrors.length === 0,
		pageErrors.slice(0, 3).join(" | "),
	);

	const passed = H.summary(SUITE);
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	try {
		const tail = String(server?.log?.() || "")
			.split("\n")
			.slice(-40)
			.join("\n");
		if (tail) console.error("--- server log (tail) ---\n" + tail);
	} catch {
		/* ignore */
	}
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
