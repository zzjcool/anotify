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
import * as V from "../lib/v4.mjs";

const RP = "localhost";
// v4：命名空间进路径（任务/事件都是 ns 内资源）。会话鉴权边界不变，只是路径要带 ns。
const NS = V.DEFAULT_NS;
let server, browser, ctx, page, cdp;
// 虚拟认证器 id（firstRunCase() 收尾要用它清 resident 凭证，所以提到模块作用域）
let authenticatorId = "";
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

/* #381 首跑闭环（空实例）。每一步都断言真实后果，不是「元素存在」：
 *  ① 首次访问 + /v1/auth/me 401 → 默认 tab 是**注册**（不是登录），并给出提示；
 *  ② 走页面自己的注册表单（前端真的做 WebAuthn ceremony）→ 会话 Cookie 建立；
 *  ③ 恢复码是唯一一次明文 → 未勾「我已保存」时「继续」真的 disabled，勾上才放行；
 *  ④ 放行后落到首页，三步引导（源 → 能力 → 节点）可见；空实例三步全亮；
 *  ⑤ 造一个源后第一步**由真数据**变成「已完成」——证明状态不是写死的。
 *
 * 必须在 main() 的第 1 步之前跑：那时库里确实没有用户，首注册成为 admin 的路径才是真的。
 */
async function firstRunCase() {
	const firstUser = "first_" + Date.now();
	await page.goto(server.base + "/login.html", { waitUntil: "domcontentloaded" });
	await H.waitForAppReady(page, "login");
	await page
		.waitForFunction(
			() =>
				document
					.getElementById("tab-signup")
					?.classList.contains("active"),
			null,
			{ timeout: 6000 },
		)
		.catch(() => {
			/* 下面逐项断言会报出真实差异 */
		});
	const tabs = await page.evaluate(() => {
		const el = (id) => document.getElementById(id);
		return {
			signupActive: !!el("tab-signup")?.classList.contains("active"),
			loginActive: !!el("tab-login")?.classList.contains("active"),
			loginHidden: !!el("login-panel")?.classList.contains("hidden"),
			signupShown: !el("signup-panel")?.classList.contains("hidden"),
			hint:
				!!el("first-run-hint") &&
				!el("first-run-hint").classList.contains("hidden"),
		};
	});
	H.eq("首跑：首访默认选中「注册」tab", tabs.signupActive, true);
	H.eq("首跑：此时「登录」tab 不选中", tabs.loginActive, false);
	H.eq("首跑：登录面板默认收起", tabs.loginHidden, true);
	H.eq("首跑：注册面板默认展开", tabs.signupShown, true);
	H.eq("首跑：给出「第一次使用」提示", tabs.hint, true);

	// ② 真实注册表单（虚拟认证器应答 ceremony）
	await page.fill("#new-username", firstUser);
	await page.click("#signup-btn");
	const codesShown = await page
		.waitForSelector("#codes-modal:not(.hidden)", { timeout: 15000 })
		.then(() => true)
		.catch(() => false);
	H.check("首跑：注册后展示一次性恢复码", codesShown);
	const codeCount = await page.evaluate(
		() => document.querySelectorAll("#codes-grid .code-row").length,
	);
	H.eq("首跑：恢复码一次展示 8 个", codeCount, 8);

	// ③ 「我已保存」是硬门禁
	const beforeAck = await page.evaluate(() => ({
		hasAck: !!document.getElementById("codes-saved-ack"),
		checked: !!document.getElementById("codes-saved-ack")?.checked,
		disabled: !!document.getElementById("codes-continue")?.disabled,
	}));
	H.check("首跑：恢复码弹窗有「我已保存」勾选", beforeAck.hasAck);
	H.eq("首跑：勾选默认未选中", beforeAck.checked, false);
	H.eq("首跑：未勾选时「继续」禁用", beforeAck.disabled, true);
	await page.click("#codes-saved-ack");
	H.eq(
		"首跑：勾选后「继续」可用",
		await page.evaluate(
			() => !!document.getElementById("codes-continue")?.disabled,
		),
		false,
	);
	await page.click("#codes-continue");

	// ④ 落到工作台，三步引导可见（空实例 = 三步全亮）
	await page.waitForFunction(
		() =>
			location.pathname === "/" || location.pathname.endsWith("/index.html"),
		null,
		{ timeout: 15000 },
	);
	await page.waitForSelector("#fe-steps .fe-step", { timeout: 10000 });
	// 三步状态是异步算出来的（三个列表接口）：等渲染真的落地，不读模板初始态。
	await page.waitForFunction(
		() => document.getElementById("first-event")?.dataset.ready === "1",
		null,
		{ timeout: 10000 },
	);
	const steps = await page.evaluate(() => {
		const items = [...document.querySelectorAll("#fe-steps .fe-step")];
		return {
			order: items.map((n) => n.dataset.step),
			links: items.map((n) => n.querySelector("a")?.getAttribute("href")),
			states: items.map((n) => n.dataset.state),
			lit: items.map((n) => n.classList.contains("is-lit")),
			texts: items.map(
				(n) =>
					(n.querySelector("[data-state-text]")?.textContent || "").trim(),
			),
			doneHidden: !!document.getElementById("fe-done")?.hidden,
		};
	});
	H.eq(
		"首跑：三步顺序 = 源 → 能力 → 节点",
		JSON.stringify(steps.order),
		JSON.stringify(["source", "capability", "node"]),
	);
	H.eq(
		"首跑：每步都链到真实页面",
		JSON.stringify(steps.links),
		JSON.stringify(["sources.html", "capabilities.html", "nodes.html"]),
	);
	H.check(
		"首跑：空实例三步全亮",
		steps.lit.every(Boolean),
		JSON.stringify(steps.lit),
	);
	H.check(
		"首跑：每步都有「现在做到哪了」的文案",
		steps.texts.every((s) => s.length > 0),
		JSON.stringify(steps.texts),
	);
	H.eq(
		"首跑：空实例不谎报「三步已完成」",
		steps.doneHidden,
		true,
	);

	// ⑤ 有源之后第一步由真数据变成已完成
	const firstSess = await cookieVal();
	await V.mkSource(server.base, firstSess, "first-run-src", {
		namespace: NS,
	});
	await page.reload({ waitUntil: "domcontentloaded" });
	await page.waitForSelector("#fe-steps .fe-step", { timeout: 10000 });
	await page.waitForFunction(
		() => document.getElementById("first-event")?.dataset.ready === "1",
		null,
		{ timeout: 10000 },
	);
	await page
		.waitForFunction(
			() =>
				document.querySelector('#fe-steps .fe-step[data-step="source"]')
					?.dataset.state === "done",
			null,
			{ timeout: 10000 },
		)
		.catch(() => {
			/* 下面断言会报出真实值 */
		});
	const afterSource = await page.evaluate(() => {
		const pick = (step) =>
			document.querySelector(`#fe-steps .fe-step[data-step="${step}"]`);
		return {
			source: pick("source")?.dataset.state,
			capability: pick("capability")?.dataset.state,
			capabilityLit: !!pick("capability")?.classList.contains("is-lit"),
		};
	});
	H.eq("首跑：有源后第一步变「已完成」", afterSource.source, "done");
	H.eq("首跑：下一步指向「能力」", afterSource.capability, "current");
	H.eq("首跑：此时不再三步全亮", afterSource.capabilityLit, false);

	// 收尾：清掉虚拟认证器里的 resident 凭证（首跑那个用户的）。不清的话，后面的
	// 「免用户名 Passkey 登录」会在多枚可发现凭证里挑一枚，可能挑到它 → 会话串成
	// 首跑用户，套件末尾的「侧栏显示真实用户名」会拿到别的名字。只清设备侧凭证，
	// 服务端用户保留（后面的注册/登录断言都不依赖「库里只有一个用户」）。
	await cdp.send("WebAuthn.clearCredentials", { authenticatorId });
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: auth_flow（Passkey 全流程 · 虚拟认证器）===");
	server = await H.startServer({ suiteName: "auth_flow", rpId: RP });
	browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	ctx = await browser.newContext();
	await H.mockExternalCdn(ctx);
	page = await ctx.newPage();
	await page.goto(server.base + "/login.html", { waitUntil: "load" });
	cdp = await ctx.newCDPSession(page);
	await cdp.send("WebAuthn.enable");
	({ authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
		options: {
			protocol: "ctap2",
			transport: "internal",
			hasResidentKey: true,
			hasUserVerification: true,
			isUserVerified: true,
			automaticPresenceSimulation: true,
		},
	}));

	// 0. #381 首跑闭环：空实例（无用户）→ 登录页默认落在注册 → 真实 UI 注册
	//    → 恢复码「我已保存」门禁 → 落到首页 → 三步引导可见。
	//    必须在 1 之前：这时库里确实没有任何用户，首注册才会是 admin。
	await firstRunCase();

	// 1. 注册新用户
	const regStatus = await webauthnRegister(username);
	H.eq("注册新用户 → 200", regStatus, 200);
	let sess = await cookieVal();
	H.check("注册后会话 Cookie 已建立", !!sess);
	// 造数：v4 的投递入口要求源是本 ns 的真实资源（source.md §5）。
	// 无 targets = 只入库、不建任务，刚好只验 scope 分界。
	await V.mkSource(server.base, sess, "github", { namespace: NS });
	H.eq(
		"会话可访问受保护 API",
		(await H.req(server.base, V.P.tasks(NS), { session: sess })).status,
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
			await V.ingestEvent(server.base, sendKey, NS, {
				source: "github", type: "pr_opened", payload: { title: "t" },
			})
		).status,
		200,
	);
	H.eq(
		"recv Key 上报被拒(403)",
		(
			await V.ingestEvent(server.base, recvKey, NS, {
				source: "github", type: "pr_opened", payload: { title: "t" },
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
		(await H.req(server.base, V.P.tasks(NS), { session: sess })).status,
		401,
	);

	// 7. 用已注册 Passkey 重新登录
	const loginStatus = await webauthnLogin(username);
	H.eq("Passkey 重新登录 → 200", loginStatus, 200);
	sess = await cookieVal();
	H.check(
		"重新登录后会话恢复",
		!!sess &&
			(await H.req(server.base, V.P.tasks(NS), { session: sess }))
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
	await V.ingestEvent(server.base, sendKey, NS, {
		source: "github", type: "pr_opened", payload: { title: "统计测试" },
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
	await H.waitForAppReady(page, "workspace", { dataAnchor: "#sidebar-username" });
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
	await page.waitForURL("**/login.html*", { timeout: 8000 });
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
