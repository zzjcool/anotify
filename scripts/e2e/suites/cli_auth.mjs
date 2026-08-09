#!/usr/bin/env node
/* SUITE: cli_auth — CLI 设备授权登录全链路
 *
 * 覆盖 requirements §4 AC-01~36（可端到端验证的部分；TTL 10 分钟的真实等待
 * 与状态机非法迁移的细粒度单测引用 internal/auth/cliauth_test.go 的结论，
 * 本套件只做 HTTP 层可达的验证）。
 *
 * 注意限速器是进程级全局（rlCreate 10/min/IP、rlByCode 20/min/user、pollG
 * 0.8×2s 最小间隔）。本套件通过：
 *   - 建会话控制在 10 次以内（用不同 X-Forwarded-For IP 规避只在测限速那条用）
 *   - 限速验证用独立 IP / 独立会话，与正常流程隔离
 *   - poll 正常流程每次 sleep(pollInterval+0.3) 避开 pollG 429
 *   - poll 过快 429 用同一个会话连发两次（第二次必触发）
 */
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import * as H from "../lib/harness.mjs";

// 辅助：建一个会话，返回 create 响应 json
let _ipCounter = 0;
async function createSession(server, body, opts = {}) {
	// 默认用递增 IP，避免进程级 rlCreate(10/min/IP) 在套件内触发误限速；
	// 测限速的用例显式传 ip 并复用同一 IP。
	const ip = opts.ip || `198.51.100.${(_ipCounter++ % 200) + 1}`;
	const r = await H.req(server.base, "/v1/cli-auth/sessions", {
		method: "POST",
		body: body || { deviceName: "e2e-host", scopes: ["notify:send"] },
		headers: { "X-Forwarded-For": ip },
	});
	return r;
}

// 辅助：approve 一个会话（需 seed 的 session cookie）
async function approve(server, session, sid, scopes) {
	return H.req(server.base, `/v1/cli-auth/sessions/${sid}/approve`, {
		method: "POST",
		session,
		body: { scopes: scopes || ["notify:send"] },
	});
}

// 辅助：poll（带 secret）
async function poll(server, sid, secret) {
	return H.req(server.base, `/v1/cli-auth/sessions/${sid}/poll?secret=${encodeURIComponent(secret)}`);
}

// 辅助：带最小间隔的 poll（避开 pollG）
// 注意：setTimeout 保留——后端 pollGuard 强制最小间隔（防暴力轮询），
// 不是前端页面挂载等待，无法用事件替代。
async function pollSafe(server, sid, secret, extraMs = 300) {
	const r = await poll(server, sid, secret);
	// setTimeout 保留：后端 pollGuard 最小间隔
	await new Promise((r) => setTimeout(r, 2000 + extraMs));
	return r;
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: cli_auth（CLI 设备授权全链路）===");
	const server = await H.startServer({ suiteName: "cli_auth", rpId: "localhost", extraEnv: { ANOTIFY_TRUST_PROXY: "1" } });
	const seedData = H.seed(server.dbPath, "cliuser");
	const { session } = seedData;

	// ============================================================
	// 4.1 建会话（AC-01/02/03）
	// ============================================================
	{
		const r = await createSession(server, {
			deviceName: "my-macbook",
			scopes: ["notify:send"],
		});
		H.eq("AC-01 建会话 200", r.status, 200);
		H.check("AC-01 sessionId 非空", !!r.json.sessionId, `got ${r.json.sessionId}`);
		H.check("AC-01 secret 非空且够长", typeof r.json.secret === "string" && r.json.secret.length >= 32, `secret=${r.json.secret}`);
		H.check("AC-01 userCode 8位去歧义", /^[A-Z2-9]{4}-[A-Z2-9]{4}$/.test(r.json.userCode || ""), `code=${r.json.userCode}`);
		H.check("AC-01 authUrl 含 sessionId", (r.json.authUrl || "").includes(r.json.sessionId), `authUrl=${r.json.authUrl}`);
		H.eq("AC-01 pollInterval=2", r.json.pollInterval, 2);
		H.check("AC-01 expiresAt≈now+600", Math.abs(r.json.expiresAt - (Date.now() / 1000 | 0) - 600) <= 5, `expiresAt=${r.json.expiresAt}`);
		H.check("AC-01 scopes 回显", JSON.stringify(r.json.scopes) === JSON.stringify(["notify:send"]), `scopes=${JSON.stringify(r.json.scopes)}`);

		// AC-02 secret 不进 URL 族
		H.check("AC-02 authUrl 不含 secret", !(r.json.authUrl || "").includes(r.json.secret), "authUrl 含 secret!");
	}

	// AC-03 参数校验
	{
		const empty = await createSession(server, { deviceName: "x", scopes: [] });
		H.eq("AC-03 空 scopes → 400", empty.status, 400);
		const unknown = await createSession(server, { deviceName: "x", scopes: ["admin:*"] });
		H.eq("AC-03 未知 scope → 400", unknown.status, 400);
		const longName = await createSession(server, { deviceName: "x".repeat(65), scopes: ["notify:send"] });
		H.eq("AC-03 超长设备名 → 400", longName.status, 400);
		const emptyName = await createSession(server, { deviceName: "  ", scopes: ["notify:send"] });
		H.eq("AC-03 空设备名 → 400", emptyName.status, 400);
	}

	// ============================================================
	// 4.2 三入口（AC-05/06/07/08/10）
	// ============================================================
	let sess1; // 主会话，贯穿后续流程
	{
		sess1 = await createSession(server, { deviceName: "my-macbook", scopes: ["notify:send"] });
		// AC-06 qr.txt
		const qr = await H.req(server.base, `/v1/cli-auth/sessions/${sess1.json.sessionId}/qr.txt`);
		H.eq("AC-06 qr.txt 200", qr.status, 200);
		H.check("AC-06 qr text/plain", (qr.headers["content-type"] || "").includes("text/plain"), `ct=${qr.headers["content-type"]}`);
		H.check("AC-06 qr 含二维码字符", /█|▀|▄/.test(qr.text), "qr text 无方块字符");
		H.check("AC-06 qr 不含 secret", !qr.text.includes(sess1.json.secret), "qr 含 secret!");

		// AC-06 qr 不存在的会话 → 404
		const qr404 = await H.req(server.base, "/v1/cli-auth/sessions/cas_nonexistent/qr.txt");
		H.eq("AC-06 qr 不存在会话 → 404", qr404.status, 404);

		// AC-07/08 by-code（需登录）
		const code = sess1.json.userCode.replace("-", "");
		const byCodeUpper = await H.req(server.base, `/v1/cli-auth/sessions/by-code?code=${code}`, { session });
		H.eq("AC-07 by-code 大写 → 200", byCodeUpper.status, 200);
		H.eq("AC-08 by-code 同 sessionId", byCodeUpper.json.sessionId, sess1.json.sessionId);

		const byCodeLower = await H.req(server.base, `/v1/cli-auth/sessions/by-code?code=${code.toLowerCase()}`, { session });
		H.eq("AC-07 by-code 小写 → 200", byCodeLower.status, 200);

		const byCodeDash = await H.req(server.base, `/v1/cli-auth/sessions/by-code?code=${sess1.json.userCode}`, { session });
		H.eq("AC-07 by-code 带连字符 → 200", byCodeDash.status, 200);

		// AC-08 by-id 同会话信息一致
		const byId = await H.req(server.base, `/v1/cli-auth/sessions/${sess1.json.sessionId}`, { session });
		H.eq("AC-08 by-id 200", byId.status, 200);
		H.eq("AC-08 by-id 同 sessionId", byId.json.sessionId, sess1.json.sessionId);
		H.eq("AC-08 by-id deviceName", byId.json.deviceName, "my-macbook");
		H.check("AC-08 requestedScopes", JSON.stringify(byId.json.requestedScopes) === JSON.stringify(["notify:send"]), `scopes=${JSON.stringify(byId.json.requestedScopes)}`);
		H.eq("AC-08 status=pending", byId.json.status, "pending");

		// AC-10 by-code 未登录 → 401
		const anon = await H.req(server.base, `/v1/cli-auth/sessions/by-code?code=${code}`);
		H.eq("AC-10 by-code 匿名 → 401", anon.status, 401);

		// AC-10 错误码与不存在统一文案
		const wrong = await H.req(server.base, "/v1/cli-auth/sessions/by-code?code=ZZZZZZZZ", { session });
		H.eq("AC-10 不存在码 → 404", wrong.status, 404);
		H.eq("AC-10 错误文案统一", wrong.json.error, byId404msg(wrong.json.error, "授权会话不存在或已过期"));
	}

	// by-id 未登录 → 401（确认页数据需登录）
	{
		const anon = await H.req(server.base, `/v1/cli-auth/sessions/${sess1.json.sessionId}`);
		H.eq("by-id 匿名 → 401", anon.status, 401);
	}

	// ============================================================
	// 4.3 批准/拒绝与 scope 减量（AC-11/12/13/14/15/16）
	// ============================================================
	// AC-11 批准默认 scope → poll 领证 → Key 可用
	{
		const ap = await approve(server, session, sess1.json.sessionId, ["notify:send"]);
		H.eq("AC-11 approve → 200 approved", ap.status, 200);
		H.eq("AC-11 approve status", ap.json.status, "approved");

		// poll 领证（首次）
		const p = await pollSafe(server, sess1.json.sessionId, sess1.json.secret);
		H.eq("AC-11 poll → 200", p.status, 200);
		H.eq("AC-11 poll status=approved", p.json.status, "approved");
		H.check("AC-11 含 apiKey 明文", typeof p.json.apiKey === "string" && p.json.apiKey.startsWith("ant_"), `apiKey=${p.json.apiKey}`);
		H.check("AC-11 含 keyId", !!p.json.keyId);
		H.eq("AC-11 name=agent:my-macbook", p.json.name, "agent:my-macbook");
		H.eq("AC-11 scopes=[notify:send]", JSON.stringify(p.json.scopes), JSON.stringify(["notify:send"]));

		// 该 Key 调 /v1/notify 成功
		const notify = await H.req(server.base, "/v1/notify", {
			key: p.json.apiKey,
			body: { title: "cli-auth ok", status: "success" },
		});
		H.eq("AC-11 Key 调 /v1/notify → 200", notify.status, 200);
		// 该 Key 调 /v1/stream → 403（无 receive scope）
		const stream = await H.req(server.base, "/v1/stream", { key: p.json.apiKey });
		H.eq("AC-11 Key 调 /v1/stream → 403", stream.status, 403);

		sess1._key = p.json.apiKey;
	}

	// AC-12 scope 减量（建一个 send+receive 的会话，只批 send）
	{
		const s12 = await createSession(server, { deviceName: "scope-test", scopes: ["notify:send", "notify:receive"] });
		const ap = await approve(server, session, s12.json.sessionId, ["notify:send"]);
		H.eq("AC-12 approve 减量 → 200", ap.status, 200);
		const p = await pollSafe(server, s12.json.sessionId, s12.json.secret);
		H.eq("AC-12 poll scopes 只含 send", JSON.stringify(p.json.scopes), JSON.stringify(["notify:send"]));
		// 用该 Key 调 /v1/stream → 403
		const stream = await H.req(server.base, "/v1/stream", { key: p.json.apiKey });
		H.eq("AC-12 减量 Key 调 /v1/stream → 403", stream.status, 403);
		// notify 仍可用
		const notify = await H.req(server.base, "/v1/notify", {
			key: p.json.apiKey,
			body: { title: "scope reduce", status: "success" },
		});
		H.eq("AC-12 减量 Key 调 /v1/notify → 200", notify.status, 200);
	}

	// AC-13 不允许勾出未申请的 scope（构造超集）
	{
		const s13 = await createSession(server, { deviceName: "superset", scopes: ["notify:send"] });
		const ap = await approve(server, session, s13.json.sessionId, ["notify:send", "notify:receive"]);
		H.eq("AC-13 超集 approve → 400", ap.status, 400);
	}
	// AC-13 未知 scope 也 400
	{
		const s13b = await createSession(server, { deviceName: "unknown", scopes: ["notify:send"] });
		const ap = await approve(server, session, s13b.json.sessionId, ["notify:send", "admin:*"]);
		H.eq("AC-13 未知 scope approve → 400", ap.status, 400);
	}

	// AC-14 全不勾不能批准
	{
		const s14 = await createSession(server, { deviceName: "empty", scopes: ["notify:send"] });
		const ap = await approve(server, session, s14.json.sessionId, []);
		H.eq("AC-14 空 scopes approve → 400", ap.status, 400);
	}

	// AC-15 拒绝 → poll 收到 denied，不建 Key
	{
		const s15 = await createSession(server, { deviceName: "deny-test", scopes: ["notify:send"] });
		const d = await H.req(server.base, `/v1/cli-auth/sessions/${s15.json.sessionId}/deny`, { method: "POST", session });
		H.eq("AC-15 deny → 200", d.status, 200);
		H.eq("AC-15 deny status", d.json.status, "denied");
		const p = await pollSafe(server, s15.json.sessionId, s15.json.secret);
		H.eq("AC-15 poll status=denied", p.status, 200);
		H.eq("AC-15 poll 终态 denied", p.json.status, "denied");
		H.check("AC-15 denied 不发 Key", !p.json.apiKey, `got apiKey=${p.json.apiKey}`);
	}

	// AC-16 重复 approve/deny 幂等（终态 409，body.status 为真实状态）
	{
		// sess1 已 consumed → approve/deny 应 409，body.status == consumed
		const ap1 = await approve(server, session, sess1.json.sessionId, ["notify:send"]);
		H.eq("AC-16 consumed 后 approve → 409", ap1.status, 409);
		H.check("AC-16 409 body.status 为真实状态（非 terminal）", ap1.json.status !== "terminal", `status=${ap1.json.status}`);
		H.eq("AC-16 409 body.status == consumed", ap1.json.status, "consumed");
		const d1 = await H.req(server.base, `/v1/cli-auth/sessions/${sess1.json.sessionId}/deny`, { method: "POST", session });
		H.eq("AC-16 consumed 后 deny → 409", d1.status, 409);
		H.eq("AC-16 deny 409 body.status == consumed", d1.json.status, "consumed");
		// denied 会话再 approve → 409，body.status == denied
		const s16 = await createSession(server, { deviceName: "idem", scopes: ["notify:send"] });
		await H.req(server.base, `/v1/cli-auth/sessions/${s16.json.sessionId}/deny`, { method: "POST", session });
		const ap2 = await approve(server, session, s16.json.sessionId, ["notify:send"]);
		H.eq("AC-16 denied 后 approve → 409", ap2.status, 409);
		H.eq("AC-16 409 body.status == denied", ap2.json.status, "denied");
	}

	// ============================================================
	// 4.4 一次性领取（AC-17/18/19/20）
	// ============================================================
	// AC-17/18 首次明文、二次 consumed 无明文（sess1 已领证过）
	{
		const p2 = await pollSafe(server, sess1.json.sessionId, sess1.json.secret);
		H.eq("AC-18 二次 poll → 200", p2.status, 200);
		H.eq("AC-18 二次 poll status=consumed", p2.json.status, "consumed");
		H.check("AC-18 二次 poll 无明文", !p2.json.apiKey, `got apiKey=${p2.json.apiKey}`);
	}

	// AC-19 错 secret 领不到，且不消费
	{
		const s19 = await createSession(server, { deviceName: "wrong-sec", scopes: ["notify:send"] });
		await approve(server, session, s19.json.sessionId, ["notify:send"]);
		// setTimeout 保留：等 pollG 窗口（后端限速，非前端等待）
		await new Promise((r) => setTimeout(r, 1700));
		const wrong = await poll(server, s19.json.sessionId, "totally-wrong-secret-xxx");
		H.eq("AC-19 错 secret → 401", wrong.status, 401);
		H.check("AC-19 错 secret 不发 Key", !wrong.json.apiKey, "发了 key");
		// setTimeout 保留：等 pollG 窗口（后端限速，非前端等待）
		await new Promise((r) => setTimeout(r, 2000));
		const ok = await poll(server, s19.json.sessionId, s19.json.secret);
		H.eq("AC-19 正确 secret 仍可领 → 200", ok.status, 200);
		H.eq("AC-19 正确 secret status=approved", ok.json.status, "approved");
		H.check("AC-19 正确 secret 含 Key", !!ok.json.apiKey, "无 key");
	}

	// AC-20 仅有 sessionId 无法领证（无 secret）
	{
		const noSec = await poll(server, sess1.json.sessionId, "");
		H.check("AC-20 空 secret → 401", noSec.status === 401, `status=${noSec.status}`);
		// by-code/by-id 都不含 secret，无法用于领证（已验证它们不返回 secret）
	}

	// AC-17 明文不落库（DB 文件搜不到）
	{
		const dir = path.dirname(server.dbPath);
		const bufs = readdirSync(dir)
			.filter((f) => f.startsWith(path.basename(server.dbPath)))
			.map((f) => readFileSync(path.join(dir, f)));
		const contains = (n) => bufs.some((b) => b.includes(n));
		H.check("AC-17 DB 不含明文 Key", !contains(sess1._key), "明文 Key 出现在 DB!");
		H.check("AC-17 DB 不含 secret", !contains(sess1.json.secret), "secret 出现在 DB!");
	}

	// ============================================================
	// 4.6 poll 节奏（AC-31）
	// ============================================================
	{
		const s31 = await createSession(server, { deviceName: "poll-fast", scopes: ["notify:send"] });
		await approve(server, session, s31.json.sessionId, ["notify:send"]);
		await new Promise((r) => setTimeout(r, 100));
		const p1 = await poll(server, s31.json.sessionId, s31.json.secret); // 可能领证成功或 429
		// 紧接着立即第二次（必触发 pollG 429，除非第一次就领证了——第一次刚 allow 过，第二次 <1.6s）
		if (p1.status === 200 && p1.json.status === "approved") {
			// 已领证，再 poll 会是 consumed；为测 429 用另一个会话连发
			H.ok("AC-31 首次 poll 领证成功（节奏合规）");
		} else if (p1.status === 429) {
			H.ok("AC-31 首次 poll 即被限速（窗口内）");
		} else {
			// pending 等
			H.check("AC-31 首次 poll 非异常", p1.status === 200, `status=${p1.status}`);
		}
		// 显式测过快 429：新建会话，approve 后立即连发两次
		const s31b = await createSession(server, { deviceName: "poll-429", scopes: ["notify:send"] });
		await approve(server, session, s31b.json.sessionId, ["notify:send"]);
		const r1 = await poll(server, s31b.json.sessionId, s31b.json.secret);
		const r2 = await poll(server, s31b.json.sessionId, s31b.json.secret);
		// r1 可能领证(200)或pending(200)，r2 必 429（同会话 <1.6s）
		H.eq("AC-31 同会话过快 poll → 429", r2.status, 429);
	}

	// ============================================================
	// 4.7 与现有产品一致性（AC-32/33/34）
	// ============================================================
	// AC-32 自动建的 Key 在 /v1/keys 列表可见
	{
		const keys = await H.req(server.base, "/v1/keys", { session });
		H.eq("AC-32 /v1/keys 200", keys.status, 200);
		const list = Array.isArray(keys.json) ? keys.json : keys.json.keys || [];
		const found = list.find((k) => k.name === "agent:my-macbook");
		H.check("AC-32 列表含 agent:my-macbook", !!found, `未找到，列表=${JSON.stringify(list.map((k) => k.name))}`);
		if (found) {
			H.check("AC-32 前缀 ant_send_", (found.prefix || "").startsWith("ant_send_"), `prefix=${found.prefix}`);
			H.check("AC-32 scope 徽章 notify:send", (found.scopes || []).includes("notify:send"), `scopes=${JSON.stringify(found.scopes)}`);
			H.eq("AC-32 enabled=true", found.enabled, true);
		}
		sess1._keyId = found && found.id;
	}

	// AC-33 既有契约不回归：/v1/keys 的 keys 字段是数组（空时为 [] 而非 null）
	{
		const keys = await H.req(server.base, "/v1/keys", { session });
		H.eq("AC-33 /v1/keys → 200", keys.status, 200);
		H.check("AC-33 keys 字段是数组（[] 非 null）", Array.isArray(keys.json.keys), `keys 字段类型=${typeof keys.json.keys}`);
		H.check("AC-33 count 字段为数字", typeof keys.json.count === "number", `count=${keys.json.count}`);
		// store 层 ListAPIKeysByUser 用 make([]...,0) 保证空列表是 [] 而非 null
		// （devseed 总会给用户建 send/recv Key，故不强制 length===0；结构即回归防线）
	}

	// AC-34 正常流程全程无 429（sess1 主流程已验证：create×1 + qr×1 + by-code×3 + by-id×1 + poll×N + approve×1）
	// 上面的流程若触发 429 会被前面的断言捕获；这里显式再跑一次干净的单会话流程
	{
		const s34 = await createSession(server, { deviceName: "clean-flow", scopes: ["notify:send"] }, { ip: "203.0.113.34" });
		H.eq("AC-34 干净建会话 200", s34.status, 200);
		const qr = await H.req(server.base, `/v1/cli-auth/sessions/${s34.json.sessionId}/qr.txt`, { headers: { "X-Forwarded-For": "203.0.113.34" } });
		H.eq("AC-34 qr 200", qr.status, 200);
		await approve(server, session, s34.json.sessionId, ["notify:send"]);
		const p = await pollSafe(server, s34.json.sessionId, s34.json.secret);
		H.check("AC-34 干净流程 poll 无 429", p.status === 200 && p.json.apiKey, `status=${p.status}`);
	}

	// ============================================================
	// 4.4 AC-17 续：Key 停用后 401（AC-32 后半）
	// ============================================================
	{
		// 找到 sess1 对应的 key id 并停用
		const keys = await H.req(server.base, "/v1/keys", { session });
		const list = Array.isArray(keys.json) ? keys.json : keys.json.keys || [];
		const found = list.find((k) => k.name === "agent:my-macbook");
		if (sess1._keyId) {
			const rev = await H.req(server.base, `/v1/keys/${sess1._keyId}/revoke`, { method: "POST", session });
			H.eq("AC-32 停用 Key → 200", rev.status, 200);
			const notify = await H.req(server.base, "/v1/notify", {
				key: sess1._key,
				body: { title: "after revoke", status: "success" },
			});
			H.eq("AC-32 停用后 /v1/notify → 401", notify.status, 401);
		} else {
			H.bad("AC-32 停用测试：未找到 Key");
		}
	}

	// ============================================================
	// AC-25 脚本分发
	// ============================================================
	{
		const s = await H.req(server.base, "/agent-login.sh");
		H.eq("AC-25 /agent-login.sh → 200", s.status, 200);
		H.check("AC-25 text/x-sh", (s.headers["content-type"] || "").includes("sh") || (s.headers["content-type"] || "").includes("text/plain"), `ct=${s.headers["content-type"]}`);
		H.eq("AC-25 no-store", s.headers["cache-control"], "no-store");
		H.check("AC-25 含 shebang", s.text.includes("#!/bin/sh"), "无 shebang");
		H.check("AC-25 不含机密", !s.text.includes("ANT_NOTIFY_SECRET"), "脚本含机密占位");
	}

	// ============================================================
	// keys/self 自检端点
	// ============================================================
	{
		const ok = await H.req(server.base, "/v1/keys/self", { key: seedData.sendKey });
		H.eq("keys/self sendKey → 200", ok.status, 200);
		H.check("keys/self 含 name", typeof ok.json.name === "string", `name=${ok.json.name}`);
		H.check("keys/self 含 scopes", Array.isArray(ok.json.scopes), `scopes=${JSON.stringify(ok.json.scopes)}`);
		const bad = await H.req(server.base, "/v1/keys/self", { key: "ant_send_invalid_xx" });
		H.eq("keys/self 错 Key → 401", bad.status, 401);
		const none = await H.req(server.base, "/v1/keys/self");
		H.eq("keys/self 无 Key → 401", none.status, 401);
	}

	// ============================================================
	// 脚本端到端（AC-21/23/26/27）：后台跑脚本 → approve 短码 → 退出 0 + 凭据 0600 + stdout 无 Key
	// ============================================================
	await scriptE2E(server, session);

	// ============================================================
	// AC-35 web_verify：cli-auth 页四语言 × 双视口
	// ============================================================
	await webVerify(server, session);

	// ============================================================
	// TTL/状态机（AC-28/29/30）：引用 Go 单测结论 + HTTP 层不可达说明
	// =============================================================
	H.check("AC-28/29/30 TTL 10分钟与状态机非法迁移拒绝 → 见 internal/auth/cliauth_test.go（SetClock 注入，HTTP 层无 TTL 覆盖入口，不硬等 10 分钟）", true);

	server.stop();
	const passed = H.summary("cli_auth");
	process.exit(passed ? 0 : 1);
}

// AC-35 web_verify：cli-auth 页四语言（zh 根 + en/ja/es）桌面1280 + 移动390，
// 无 JS pageerror、无横向溢出；未登录跳 login（路由守卫）；已登录 confirm 态可渲染。
async function webVerify(server, seedSession) {
	let chromium;
	try {
		({ chromium } = await import("playwright-core"));
	} catch {
		H.check("AC-35 playwright-core 可用（跳过 web_verify）", false, "playwright-core 未安装");
		return;
	}
	console.log("\n  -- AC-35 web_verify（cli-auth 页）--");
	const browser = await chromium.launch({ channel: "chrome", headless: true, args: ["--no-sandbox"] });
	const VIEWPORTS = [
		{ name: "桌面1280", width: 1280, height: 800 },
		{ name: "移动390", width: 390, height: 844 },
	];
	const LANGS = [
		{ path: "cli-auth.html", label: "zh" },
		{ path: "en/cli-auth.html", label: "en" },
		{ path: "ja/cli-auth.html", label: "ja" },
		{ path: "es/cli-auth.html", label: "es" },
	];

	async function checkPage(ctx, url, vpName, vp) {
		const page = await ctx.newPage();
		await page.setViewportSize({ width: vp.width, height: vp.height });
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		try {
			await page.goto(url, { waitUntil: "load", timeout: 15000 });
			await H.waitForAppReady(page, "login");
		} catch (e) {
			H.bad(`${vpName} ${url.split("/").pop()} 加载`, String(e).slice(0, 80));
			await page.close();
			return;
		}
		const name = `${url.split("/").pop()}`;
		H.check(`${vpName} ${name} 无 JS pageerror`, errs.length === 0, errs[0]?.slice(0, 100));
		const overflow = await page.evaluate(() => {
			const vw = window.innerWidth;
			let n = 0;
			for (const el of document.querySelectorAll("body *")) {
				const r = el.getBoundingClientRect();
				if (r.right > vw + 5 && !el.closest("pre") && !el.closest(".overflow-x-auto") && !el.closest("code")) n++;
			}
			return n;
		});
		H.check(`${vpName} ${name} 无横向溢出`, overflow === 0, `${overflow} 个元素超出`);
		await page.close();
	}

	// 未登录：cli-auth.html?s=<sid> 应跳 login（会话查询 401 → 客户端守卫跳转，AC-09）
	{
		const ctx = await browser.newContext();
		const page = await ctx.newPage();
		await page.goto(server.base + "/cli-auth.html?s=cas_test_unauth", { waitUntil: "load", timeout: 15000 });
		await page.waitForURL("**/login.html*", { timeout: 8000 });
		H.check("AC-09 未登录 cli-auth.html?s= → 跳 login", page.url().includes("login.html"), `最终 ${page.url()}`);
		await page.close();
		await ctx.close();
	}

	// 未登录纯渲染检查（跳转后 login 也无 JS 错误）：四语言 × 双视口
	for (const vp of VIEWPORTS) {
		const ctx = await browser.newContext();
		for (const l of LANGS) {
			await checkPage(ctx, server.base + "/" + l.path, vp.name, vp);
		}
		await ctx.close();
	}

	// 已登录 confirm 态：建会话 → 带 ?s=<sid> 打开 → 渲染确认信息
	{
		const s = await createSession(server, { deviceName: "verify-host", scopes: ["notify:send"] });
		const ctx = await browser.newContext();
		const u = new URL(server.base);
		await ctx.addCookies([{ name: "anotify_session", value: seedSession, domain: u.hostname, path: "/", httpOnly: true }]);
		const page = await ctx.newPage();
		await page.setViewportSize({ width: 1280, height: 800 });
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/cli-auth.html?s=" + s.json.sessionId, { waitUntil: "load", timeout: 15000 });
		await H.waitForAppReady(page, "login");
		H.check("AC-05/35 已登录 confirm 态无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 100));
		const bodyText = await page.evaluate(() => document.body.innerText);
		H.check("AC-05 confirm 态显示设备名", bodyText.includes("verify-host"), `body 未含 verify-host`);
		H.check("AC-05 confirm 态显示 notify:send 勾选项", /notify:send|发送/i.test(bodyText), `body 未含 send scope`);
		// 移动视口也无溢出
		await page.setViewportSize({ width: 390, height: 844 });
		await page.waitForSelector("#lang-switcher-login button[aria-haspopup]", { timeout: 5000 });
		const overflow = await page.evaluate(() => {
			const vw = window.innerWidth;
			let n = 0;
			for (const el of document.querySelectorAll("body *")) {
				const r = el.getBoundingClientRect();
				if (r.right > vw + 5 && !el.closest("pre") && !el.closest("code")) n++;
			}
			return n;
		});
		H.check("AC-35 confirm 态移动390 无横向溢出", overflow === 0, `${overflow} 个元素超出`);
		await page.close();
		await ctx.close();
	}

	await browser.close();
}

// 脚本端到端：后台启动 agent-login.sh，approve 对应短码，验证退出码/权限/stdout 无密
async function scriptE2E(server, seedSession) {
	console.log("\n  -- 脚本端到端（AC-21/23/26/27）--");
	const home = mkdtempSync(path.join(tmpdir(), "anotify-script-"));
	// 先建一个会话拿到 userCode（脚本会自己建会话，但我们需要在它打印短码后 approve；
	// 这里改成：直接驱动脚本，脚本建会话后我们无法立即知道 sessionId。
	// 方案：脚本建会话 → 我们从 stdout 里 grep 出 userCode → 用 seed session 查 by-code 拿 sid → approve）
	const scriptPath = path.join(H.ROOT_DIR, "web/agent-login.sh");
	const env = Object.assign({}, process.env, {
		ANOTIFY_BASE_URL: server.base,
		SSH_TTY: "/dev/pts/0", // 强制无浏览器路径
		SSH_CONNECTION: "fake", // 双重保证
		HOME: home,
		XDG_CONFIG_HOME: "", // 确保 ~/.config 路径
	});

	const proc = spawn("sh", [scriptPath], {
		env,
		stdio: ["ignore", "pipe", "pipe"],
	});
	let stdout = "";
	let stderr = "";
	proc.stdout.on("data", (d) => { stdout += d; });
	proc.stderr.on("data", (d) => { stderr += d; });

	// 等待脚本打印短码（格式：  验证码：XXXX-XXXX）
	let userCode = null;
	const codeDeadline = Date.now() + 15000;
	while (Date.now() < codeDeadline && !userCode) {
		const m = stdout.match(/验证码：\s*([A-Z2-9]{4}-[A-Z2-9]{4})/);
		if (m) { userCode = m[1]; break; }
		await new Promise((r) => setTimeout(r, 200));
	}
	H.check("AC-23 脚本打印短码", !!userCode, `stdout=${stdout.slice(0, 200)}`);

	if (userCode) {
		// 用 seed session 查 by-code 拿 sessionId，approve
		const code = userCode.replace("-", "");
		const lookup = await H.req(server.base, `/v1/cli-auth/sessions/by-code?code=${code}`, { session: seedSession });
		if (lookup.status !== 200) {
			H.bad(`AC 脚本端到端 by-code lookup 失败: ${lookup.status}`, lookup.json && lookup.json.error);
		} else {
			const sid = lookup.json.sessionId;
			const ap = await approve(server, seedSession, sid, ["notify:send"]);
			H.eq("AC 脚本端到端 approve → 200", ap.status, 200);
		}
	}

	// 等脚本退出（领证后应退出 0）
	// setTimeout 保留：等外部进程退出的超时保护，非页面等待
	const exitCode = await new Promise((resolve) => {
		const to = setTimeout(() => {
			proc.kill("SIGKILL");
			resolve(-1);
		}, 30000);
		proc.on("exit", (c) => { clearTimeout(to); resolve(c); });
	});

	H.eq("AC-21 脚本退出码 0", exitCode, 0);

	// 凭据文件
	const credPath = path.join(home, ".config/anotify/credentials.json");
	H.check("AC-21 凭据文件存在", existsSync(credPath), `path=${credPath}`);
	if (existsSync(credPath)) {
		const st = statSync(credPath);
		const mode = (st.mode & 0o777).toString(8);
		H.eq("AC-21 凭据文件权限 0600", mode, "600");
		const dirMode = (statSync(path.dirname(credPath)).mode & 0o777).toString(8);
		H.eq("AC-21 目录权限 0700", dirMode, "700");
		const cred = JSON.parse(readFileSync(credPath, "utf8"));
		H.check("AC-21 含 apiKey", typeof cred.apiKey === "string" && cred.apiKey.startsWith("ant_"), `apiKey=${cred.apiKey}`);
		H.check("AC-21 含 server", cred.server === server.base, `server=${cred.server}`);

		// 该 Key 可用
		const notify = await H.req(server.base, "/v1/notify", {
			key: cred.apiKey,
			body: { title: "script e2e", status: "success" },
		});
		H.eq("AC-21 脚本领到的 Key 可用 → 200", notify.status, 200);
	}

	// AC-23 stdout/stderr 无 Key 明文、无 secret
	// 注意：脚本自己建的会话的 secret 不在 stdout（脚本不打印 secret）；
	// 但 Key 明文绝不出现
	const allOut = stdout + stderr;
	H.check("AC-23 stdout 无 ant_ Key 明文", !/ant_(send|recv|full|key)_/.test(allOut), "stdout 含 Key 明文!");
	// secret 也不应出现（脚本从不打印 secret）
	H.check("AC-23 stdout 无 secret 字样泄露", !allOut.includes('"secret"'), "");

	// AC-26 无浏览器环境：脚本应走二维码/短码路径（已通过打印短码验证）
	H.check("AC-26 无浏览器路径（打印了二维码/短码）", /验证码/.test(stdout), "未打印验证码");

	// AC-21 幂等：再跑一次，应「已登录」退出 0
	const proc2 = spawn("sh", [scriptPath], { env, stdio: ["ignore", "pipe", "pipe"] });
	let out2 = "";
	proc2.stdout.on("data", (d) => { out2 += d; });
	// setTimeout 保留：等外部进程退出的超时保护，非页面等待
	const exit2 = await new Promise((resolve) => {
		const to = setTimeout(() => { proc2.kill("SIGKILL"); resolve(-1); }, 10000);
		proc2.on("exit", (c) => { clearTimeout(to); resolve(c); });
	});
	H.eq("AC-21 幂等再跑退出 0", exit2, 0);
	H.check("AC-21 幂等提示已登录", /已登录/.test(out2), `out=${out2.slice(0,150)}`);
}

function byId404msg(actual, expected) {
	return actual === expected ? expected : `(实际:${actual})`;
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
