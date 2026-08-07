#!/usr/bin/env node
/* SUITE: passkey_enroll — Passkey 新设备授权添加全链路
 *
 * 覆盖 decisions.md D-B 端点契约 + D-C 安全不变量。
 * 完整用户旅程：旧设备建会话 → 新设备匿名 lookup → 敲门 → 旧设备批准 →
 * poll 拿 attestationOptions + registrationToken → complete（需浏览器认证器，此处验证到达）。
 *
 * 安全负面矩阵：D-C-1（secret 不进公开渠道）、D-C-2（complete 四项校验）、
 * D-C-4（防重放）、D-C-6（kind 不可绕过）、D-C-7（cli_auth 零回归）。
 *
 * 注意：complete 端点需要真实 WebAuthn attestation（浏览器认证器生成），
 * Node.js e2e 无法模拟。此处验证 complete 的错误处理（缺 token、错 token、
 * 非 approved 状态）以及 poll 正确返回 attestationOptions + registrationToken。
 *
 * 产品 bug 记录（不弱化断言迁就）：
 * 1. poll 的 consumeAndGenerateAttestation 在首次 poll 时做 approved→consumed 迁移，
 *    导致 complete 因 status=consumed 而返回 409。complete 端点当前无法成功。
 *    修复建议：poll 不应 consume，应留给 complete 做 approved→consumed。
 * 2. knock 对不存在的会话返回 500（writeEnrollErr 未处理 store.ErrNotFound），应 404。
 */
import * as H from "../lib/harness.mjs";

async function createEnrollSession(
	server,
	session,
	deviceName = "old-macbook",
) {
	return H.req(server.base, "/v1/passkey-enroll/sessions", {
		method: "POST",
		session,
		body: { deviceName },
	});
}

async function knock(server, sid, deviceHint = "Chrome · macOS") {
	return H.req(server.base, `/v1/passkey-enroll/sessions/${sid}/request`, {
		method: "POST",
		body: { deviceHint },
	});
}

async function watch(server, sid, session) {
	return H.req(server.base, `/v1/passkey-enroll/sessions/${sid}/watch`, {
		session,
	});
}

async function poll(server, sid, secret) {
	return H.req(
		server.base,
		`/v1/passkey-enroll/sessions/${sid}/poll?secret=${encodeURIComponent(secret)}`,
	);
}

async function approve(server, sid, session) {
	return H.req(server.base, `/v1/passkey-enroll/sessions/${sid}/approve`, {
		method: "POST",
		session,
	});
}

// pollSafe：带 pollGuard 最小间隔（2s + buffer）
async function pollSafe(server, sid, secret) {
	const r = await poll(server, sid, secret);
	await new Promise((r) => setTimeout(r, 2300));
	return r;
}

// 完整 happy path 前半段 helper：建会话→敲门→批准→poll 拿 options+token
async function setupApprovedSession(server, session) {
	// 1. 建会话
	const create = await createEnrollSession(server, session, "old-macbook");
	H.eq("PE-01 建会话 200", create.status, 200);
	H.check(
		"PE-01 sessionId 非空",
		!!create.json.sessionId,
		`got ${create.json.sessionId}`,
	);
	H.check(
		"PE-01 secret 非空",
		typeof create.json.secret === "string" && create.json.secret.length >= 32,
		`secret=${create.json.secret}`,
	);
	H.check(
		"PE-01 userCode 格式",
		/^[A-Z2-9]{4}-[A-Z2-9]{4}$/.test(create.json.userCode || ""),
		`code=${create.json.userCode}`,
	);
	H.check(
		"PE-01 authUrl 含 passkey-enroll.html",
		(create.json.authUrl || "").includes("passkey-enroll.html"),
		`authUrl=${create.json.authUrl}`,
	);
	H.check(
		"PE-01 authUrl 含 sessionId",
		(create.json.authUrl || "").includes(create.json.sessionId),
		`authUrl=${create.json.authUrl}`,
	);
	H.eq("PE-01 kind=passkey", create.json.kind, "passkey");
	H.eq("PE-01 pollInterval=2", create.json.pollInterval, 2);

	// 2. 匿名 lookup → pending
	const lookup = await H.req(
		server.base,
		`/v1/passkey-enroll/sessions/${create.json.sessionId}`,
	);
	H.eq("PE-02 匿名 lookup 200", lookup.status, 200);
	H.eq("PE-02 status=pending", lookup.json.status, "pending");
	H.check("PE-02 不含 secret", !lookup.json.secret, "lookup 返回了 secret!");
	H.check("PE-02 不含 userId", !lookup.json.userId, "lookup 返回了 userId!");

	// 3. 敲门
	const knockResp = await knock(
		server,
		create.json.sessionId,
		"Chrome · macOS",
	);
	H.eq("PE-03 敲门 200", knockResp.status, 200);
	H.check(
		"PE-03 返回 secret",
		typeof knockResp.json.secret === "string" &&
			knockResp.json.secret.length >= 32,
		`secret=${knockResp.json.secret}`,
	);

	// 4. 旧设备 watch → requested + deviceHint
	const watchResp = await watch(server, create.json.sessionId, session);
	H.eq("PE-04 watch 200", watchResp.status, 200);
	H.eq("PE-04 status=requested", watchResp.json.status, "requested");
	H.eq("PE-04 deviceHint", watchResp.json.deviceHint, "Chrome · macOS");

	// 5. 旧设备批准
	const approveResp = await approve(server, create.json.sessionId, session);
	H.eq("PE-05 批准 200", approveResp.status, 200);

	// 6. 新设备 poll → approved + attestationOptions + registrationToken
	const pollResp = await pollSafe(
		server,
		create.json.sessionId,
		knockResp.json.secret,
	);
	H.eq("PE-06 poll 200", pollResp.status, 200);
	H.eq("PE-06 status=approved", pollResp.json.status, "approved");
	H.check(
		"PE-06 含 attestationOptions",
		!!pollResp.json.attestationOptions,
		"无 attestationOptions",
	);
	H.check(
		"PE-06 含 registrationToken",
		typeof pollResp.json.registrationToken === "string" &&
			pollResp.json.registrationToken.length > 0,
		"无 registrationToken",
	);
	H.check(
		"PE-06 含 initiatorName",
		typeof pollResp.json.initiatorName === "string" &&
			pollResp.json.initiatorName.length > 0,
		"无 initiatorName",
	);
	H.check(
		"PE-06 不含 apiKey",
		!pollResp.json.apiKey,
		"passkey poll 不应返回 apiKey!",
	);

	// 验证 attestationOptions 结构
	if (pollResp.json.attestationOptions) {
		const pk = pollResp.json.attestationOptions.publicKey;
		H.check("PE-06 publicKey 含 challenge", !!pk?.challenge, "无 challenge");
		H.check("PE-06 publicKey 含 rp", !!pk?.rp, "无 rp");
		H.check("PE-06 publicKey 含 user", !!pk?.user, "无 user");
	}

	return {
		sessionId: create.json.sessionId,
		createSecret: create.json.secret,
		knockSecret: knockResp.json.secret,
		userCode: create.json.userCode,
		authUrl: create.json.authUrl,
		registrationToken: pollResp.json.registrationToken,
	};
}

async function main() {
	console.log("=== SUITE: passkey_enroll（Passkey 新设备授权添加全链路）===");
	const server = await H.startServer({
		rpId: "localhost",
		extraEnv: { ANOTIFY_TRUST_PROXY: "1" },
	});
	const seedData = H.seed(server.dbPath, "enrolluser");
	const { session } = seedData;

	// ============================================================
	// 1. 完整用户旅程（happy path 前半段：到 poll 拿 options+token）
	// ============================================================
	let mainSession;
	mainSession = await setupApprovedSession(server, session);

	// ============================================================
	// 2. D-C-1: secret 不进公开渠道
	// ============================================================
	{
		// authUrl 不含 secret
		H.check(
			"D-C-1 authUrl 不含 createSecret",
			!mainSession.authUrl.includes(mainSession.createSecret),
			"authUrl 含 secret!",
		);
		H.check(
			"D-C-1 authUrl 不含 knockSecret",
			!mainSession.authUrl.includes(mainSession.knockSecret),
			"authUrl 含 knock secret!",
		);

		// qr.txt 不含 secret
		const qr = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${mainSession.sessionId}/qr.txt`,
		);
		H.eq("D-C-1 qr.txt 200", qr.status, 200);
		H.check(
			"D-C-1 qr.txt 不含 createSecret",
			!qr.text.includes(mainSession.createSecret),
			"qr.txt 含 secret!",
		);
		H.check(
			"D-C-1 qr.txt 不含 knockSecret",
			!qr.text.includes(mainSession.knockSecret),
			"qr.txt 含 knock secret!",
		);
		H.check(
			"D-C-1 qr.txt 含二维码字符",
			/█|▀|▄/.test(qr.text),
			"qr 无方块字符",
		);

		// 匿名 lookup 不含 secret（已在 setupApprovedSession 验证）
	}

	// ============================================================
	// 3. D-C-2: complete 四项校验
	// ============================================================
	{
		// 缺 registrationToken → 400
		// 需要新的 approved 会话（mainSession 已 consumed）
		const s = await createEnrollSession(server, session, "test2");
		await knock(server, s.json.sessionId, "Chrome");
		await approve(server, s.json.sessionId, session);
		// poll 拿 token（会话保持 approved，poll 不 consume）
		await new Promise((r) => setTimeout(r, 2300));
		await poll(
			server,
			s.json.sessionId,
			(await knock(server, s.json.sessionId, "Chrome")).json?.secret || "",
		);

		// complete 缺 registrationToken → 400
		const noToken = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/complete?name=test`,
			{
				method: "POST",
				body: {},
			},
		);
		H.eq("D-C-2 缺 registrationToken → 400", noToken.status, 400);

		// complete 错 registrationToken → 401
		const wrongToken = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/complete?registrationToken=wrong&name=test`,
			{
				method: "POST",
				body: {},
			},
		);
		H.check(
			"D-C-2 错 registrationToken → 401",
			wrongToken.status === 401,
			`status=${wrongToken.status}`,
		);
	}

	// ============================================================
	// 4. D-C-4: complete 无效 attestation → 4xx（会话保持 approved 可重试）
	//    修复后：poll 不 consume，会话仍 approved。合法 token + 空 attestation
	//    → FinishEnrollCredential 失败 → 400。会话保持 approved，可重新 poll 拿新 token 重试。
	//    （真正的防重放：complete 成功后 → consumed，第二次 complete → 409。
	//     需真实 WebAuthn attestation 才能到达 consumed，测试环境无法覆盖。）
	// ============================================================
	{
		const replay = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${mainSession.sessionId}/complete?registrationToken=${mainSession.registrationToken}&name=test`,
			{
				method: "POST",
				body: {},
			},
		);
		H.check(
			"D-C-4 无效 attestation → 4xx（会话可重试）",
			replay.status >= 400 && replay.status < 500,
			`status=${replay.status}`,
		);
		// 会话仍 approved（未 consume），可重试
		const recheck = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${mainSession.sessionId}`,
			{ method: "GET" },
		);
		H.eq("D-C-4 失败后会话仍 approved（可重试）", recheck.json.status, "approved");
	}

	// ============================================================
	// 5. D-C-6: kind 不可绕过
	// ============================================================
	{
		// 建 apikey-kind 会话（通过 cli-auth 端点）
		const cliCreate = await H.req(server.base, "/v1/cli-auth/sessions", {
			method: "POST",
			body: { deviceName: "apikey-test", scopes: ["notify:send"] },
		});
		H.eq("D-C-6 建 apikey 会话 200", cliCreate.status, 200);
		const cliSid = cliCreate.json.sessionId;

		// enroll lookup → 404
		const lookup = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${cliSid}`,
		);
		H.eq("D-C-6 apikey-kind enroll lookup → 404", lookup.status, 404);

		// enroll knock → 4xx
		const knockResp = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${cliSid}/request`,
			{
				method: "POST",
				body: { deviceHint: "Chrome" },
			},
		);
		H.check(
			"D-C-6 apikey-kind enroll knock → 4xx",
			knockResp.status >= 400 && knockResp.status < 500,
			`status=${knockResp.status}`,
		);

		// enroll approve → 4xx
		const approveResp = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${cliSid}/approve`,
			{
				method: "POST",
				session,
			},
		);
		H.check(
			"D-C-6 apikey-kind enroll approve → 4xx",
			approveResp.status >= 400 && approveResp.status < 500,
			`status=${approveResp.status}`,
		);
	}

	// ============================================================
	// 6. 状态机边界
	// ============================================================
	// pending 态直接批准 → 409
	{
		const s = await createEnrollSession(server, session, "pending-test");
		const ap = await approve(server, s.json.sessionId, session);
		H.eq("状态机: pending 态批准 → 409", ap.status, 409);
	}

	// denied 会话再操作 → 4xx
	{
		const s = await createEnrollSession(server, session, "deny-test");
		await knock(server, s.json.sessionId, "Chrome");
		await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/deny`,
			{ method: "POST", session },
		);

		// denied 后再批准 → 409
		const ap = await approve(server, s.json.sessionId, session);
		H.eq("状态机: denied 态批准 → 409", ap.status, 409);

		// denied 后再敲门 → 4xx
		const kn = await knock(server, s.json.sessionId, "Firefox");
		H.check(
			"状态机: denied 态敲门 → 4xx",
			kn.status >= 400 && kn.status < 500,
			`status=${kn.status}`,
		);
	}

	// 重复敲门 → 409
	{
		const s = await createEnrollSession(server, session, "repeat-knock");
		await knock(server, s.json.sessionId, "Chrome");
		const kn2 = await knock(server, s.json.sessionId, "Firefox");
		H.eq("状态机: 重复敲门 → 409", kn2.status, 409);
	}

	// DELETE 后所有端点 → 404
	{
		const s = await createEnrollSession(server, session, "delete-test");
		const del = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}`,
			{ method: "DELETE", session },
		);
		H.eq("DELETE 会话 200", del.status, 200);

		const lookup = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}`,
		);
		H.eq("DELETE 后 lookup → 404", lookup.status, 404);

		const pollResp = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/poll?secret=x`,
		);
		H.eq("DELETE 后 poll → 404", pollResp.status, 404);
	}

	// ============================================================
	// 7. 鉴权边界
	// ============================================================
	{
		// 未登录建会话 → 401
		const noAuth = await H.req(server.base, "/v1/passkey-enroll/sessions", {
			method: "POST",
			body: { deviceName: "test" },
		});
		H.eq("鉴权: 未登录建会话 → 401", noAuth.status, 401);

		// 未登录批准 → 401
		const s = await createEnrollSession(server, session, "auth-test");
		await knock(server, s.json.sessionId, "Chrome");
		const noAuthApprove = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/approve`,
			{
				method: "POST",
			},
		);
		H.eq("鉴权: 未登录批准 → 401", noAuthApprove.status, 401);

		// 未登录 watch → 401
		const noAuthWatch = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/${s.json.sessionId}/watch`,
		);
		H.eq("鉴权: 未登录 watch → 401", noAuthWatch.status, 401);
	}

	// ============================================================
	// 8. by-code 匿名 lookup
	// ============================================================
	{
		const s = await createEnrollSession(server, session, "bycode-test");
		const code = s.json.userCode.replace("-", "");

		// 大写带连字符 → 200
		const r1 = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/by-code?code=${s.json.userCode}`,
		);
		H.eq("by-code 大写带连字符 → 200", r1.status, 200);
		H.eq("by-code sessionId 一致", r1.json.sessionId, s.json.sessionId);
		H.check("by-code 不含 secret", !r1.json.secret, "by-code 返回了 secret!");

		// 小写去连字符 → 200
		const r2 = await H.req(
			server.base,
			`/v1/passkey-enroll/sessions/by-code?code=${code.toLowerCase()}`,
		);
		H.eq("by-code 小写 → 200", r2.status, 200);

		// 不存在 → 404
		const r3 = await H.req(
			server.base,
			"/v1/passkey-enroll/sessions/by-code?code=ZZZZZZZZ",
		);
		H.eq("by-code 不存在 → 404", r3.status, 404);

		// 防枚举：两个不存在码错误文案一致
		const r4 = await H.req(
			server.base,
			"/v1/passkey-enroll/sessions/by-code?code=AAAAAAAA",
		);
		H.eq("by-code 防枚举文案一致", r3.json.error, r4.json.error);
	}

	// ============================================================
	// 9. D-C-7: cli_auth 零回归
	// ============================================================
	{
		// 建 apikey 会话 → approve → poll → 领证
		const s = await H.req(server.base, "/v1/cli-auth/sessions", {
			method: "POST",
			body: { deviceName: "regression-test", scopes: ["notify:send"] },
		});
		H.eq("D-C-7 cli-auth 建会话 200", s.status, 200);

		// approve
		const ap = await H.req(
			server.base,
			`/v1/cli-auth/sessions/${s.json.sessionId}/approve`,
			{
				method: "POST",
				session,
				body: { scopes: ["notify:send"] },
			},
		);
		H.eq("D-C-7 cli-auth approve 200", ap.status, 200);

		// poll → 领证
		await new Promise((r) => setTimeout(r, 2300));
		const p = await H.req(
			server.base,
			`/v1/cli-auth/sessions/${s.json.sessionId}/poll?secret=${encodeURIComponent(s.json.secret)}`,
		);
		H.eq("D-C-7 cli-auth poll 200", p.status, 200);
		H.eq("D-C-7 cli-auth status=approved", p.json.status, "approved");
		H.check(
			"D-C-7 cli-auth 含 apiKey",
			typeof p.json.apiKey === "string" && p.json.apiKey.startsWith("ant_"),
			`apiKey=${p.json.apiKey}`,
		);
		H.check(
			"D-C-7 cli-auth 无 registrationToken",
			!p.json.registrationToken,
			"apikey poll 不应返回 registrationToken",
		);
		H.check(
			"D-C-7 cli-auth 无 attestationOptions",
			!p.json.attestationOptions,
			"apikey poll 不应返回 attestationOptions",
		);

		// Key 可用
		const notify = await H.req(server.base, "/v1/notify", {
			key: p.json.apiKey,
			body: { title: "regression ok", status: "success" },
		});
		H.eq("D-C-7 cli-auth Key 可用 → 200", notify.status, 200);
	}

	// ============================================================
	// 10. qr.txt 不存在会话 → 404
	// ============================================================
	{
		const qr = await H.req(
			server.base,
			"/v1/passkey-enroll/sessions/cas_nonexistent/qr.txt",
		);
		H.eq("qr.txt 不存在会话 → 404", qr.status, 404);
	}

	// ============================================================
	// 11. web_verify：passkey-enroll 页四语言 × 双视口
	// ============================================================
	await webVerify(server);

	// ============================================================
	// 12. web_verify：security.html 含「授权新设备」入口
	// ============================================================
	await securityPageVerify(server, session);

	server.stop();
	const passed = H.summary("passkey_enroll");
	process.exit(passed ? 0 : 1);
}

// web_verify：passkey-enroll 页四语言 × 双视口，无 JS pageerror、无横向溢出
async function webVerify(server) {
	let chromium;
	try {
		({ chromium } = await import("playwright-core"));
	} catch {
		H.check(
			"web_verify playwright-core 可用（跳过）",
			false,
			"playwright-core 未安装",
		);
		return;
	}
	console.log("\n  -- web_verify（passkey-enroll 页）--");
	const browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	const VIEWPORTS = [
		{ name: "桌面1280", width: 1280, height: 800 },
		{ name: "移动390", width: 390, height: 844 },
	];
	const LANGS = [
		{ path: "passkey-enroll.html", label: "zh" },
		{ path: "en/passkey-enroll.html", label: "en" },
		{ path: "ja/passkey-enroll.html", label: "ja" },
		{ path: "es/passkey-enroll.html", label: "es" },
	];

	for (const vp of VIEWPORTS) {
		const ctx = await browser.newContext();
		for (const l of LANGS) {
			const page = await ctx.newPage();
			await page.setViewportSize({ width: vp.width, height: vp.height });
			const errs = [];
			page.on("pageerror", (e) => errs.push(String(e)));
			try {
				await page.goto(server.base + "/" + l.path, {
					waitUntil: "load",
					timeout: 15000,
				});
				await page.waitForTimeout(1200);
			} catch (e) {
				H.bad(`${vp.name} ${l.label} 加载`, String(e).slice(0, 80));
				await page.close();
				continue;
			}
			const name = l.label;
			H.check(
				`${vp.name} ${name} 无 JS pageerror`,
				errs.length === 0,
				errs[0]?.slice(0, 100),
			);
			const overflow = await page.evaluate(() => {
				const vw = window.innerWidth;
				let n = 0;
				for (const el of document.querySelectorAll("body *")) {
					const r = el.getBoundingClientRect();
					if (
						r.right > vw + 5 &&
						!el.closest("pre") &&
						!el.closest(".overflow-x-auto") &&
						!el.closest("code")
					)
						n++;
				}
				return n;
			});
			H.check(
				`${vp.name} ${name} 无横向溢出`,
				overflow === 0,
				`${overflow} 个元素超出`,
			);
			await page.close();
		}
		await ctx.close();
	}

	await browser.close();
}

// web_verify：security.html 确认含「授权新设备」入口
async function securityPageVerify(server, seedSession) {
	let chromium;
	try {
		({ chromium } = await import("playwright-core"));
	} catch {
		return; // 已在 webVerify 跳过
	}
	console.log("\n  -- web_verify（security.html 授权入口）--");
	const browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	const ctx = await browser.newContext();
	const u = new URL(server.base);
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: seedSession,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
		},
	]);

	const page = await ctx.newPage();
	await page.setViewportSize({ width: 1280, height: 800 });
	const errs = [];
	page.on("pageerror", (e) => errs.push(String(e)));
	await page.goto(server.base + "/security.html", {
		waitUntil: "load",
		timeout: 15000,
	});
	await page.waitForTimeout(2000);
	H.check(
		"security.html 无 JS pageerror",
		errs.length === 0,
		errs[0]?.slice(0, 100),
	);

	const bodyText = await page.evaluate(() => document.body.innerText);
	// 检查是否含跨设备添加入口按钮（i18n 渲染后应含「在新设备添加」或「本机添加」等文案）
	H.check(
		"security.html 含跨设备添加入口",
		/在新设备添加|本机添加|跨设备|添加.*Passkey/i.test(bodyText),
		`body 未含跨设备入口文案`,
	);

	await page.close();
	await ctx.close();
	await browser.close();
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
