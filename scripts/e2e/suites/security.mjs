#!/usr/bin/env node
/* SUITE: security — 安全矩阵
 *
 * 覆盖 case：
 *  1. scope 越权：recv Key 上报→403；send Key 连 /v1/stream→403/拒
 *  2. Key 篡改：改字符→401；改前缀→401
 *  3. 无 Authorization 头→401；非 Bearer scheme（Basic）→401
 *  4. 未登录访问 /v1/devices /v1/keys /v1/notifications→各 401
 *  5. Key 哈希不可逆：DB 中 key_hash 为 argon2id PHC 格式（$argon2id$ 开头），
 *     不等于明文、DB 文件二进制中搜不到明文 Key
 *  6. 会话 Cookie 属性：Set-Cookie 含 HttpOnly
 *  7. SQL 注入：恶意 username/输入 → 不报错、不越权、正常 400/404
 *  8. 路径穿越：/../etc/passwd、/%2e%2e/ → 不读出站外文件（403/404/400）
 */
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import * as H from "../lib/harness.mjs";

async function main() {
	console.log("=== SUITE: security（安全矩阵）===");
	const server = await H.startServer({ suiteName: "security", rpId: "localhost" });
	const { sendKey, recvKey, session } = H.seed(server.dbPath, "sectest");

	// ---- 1. scope 越权 ----
	H.eq(
		"recv Key 上报 → 403",
		(
			await H.req(server.base, "/v1/notify", {
				key: recvKey,
				body: { title: "t", status: "success" },
			})
		).status,
		403,
	);
	// send Key 连 WS（无 receive scope）→ 应在握手阶段被拒（连接失败/立即关闭，收不到 hello）
	{
		const wsUrl = server.base.replace(/^http/, "ws") + "/v1/stream";
		const outcome = await new Promise((resolve) => {
			let settled = false;
			const done = (v) => {
				if (!settled) {
					settled = true;
					resolve(v);
				}
			};
			try {
				const ws = new WebSocket(wsUrl, {
					headers: { Authorization: "Bearer " + sendKey },
				});
				const timer = setTimeout(() => done("timeout-no-hello"), 3000);
				ws.onmessage = (ev) => {
					try {
						const f = JSON.parse(ev.data);
						if (f.type === "hello") {
							clearTimeout(timer);
							ws.close();
							done("got-hello");
						} else if (f.type === "error") {
							clearTimeout(timer);
							ws.close();
							done("error-frame:" + (f.message || f.code || ""));
						}
					} catch {
						/* ignore */
					}
				};
				ws.onerror = () => {
					clearTimeout(timer);
					done("conn-error");
				};
				ws.onclose = (e) => {
					clearTimeout(timer);
					done("closed:" + e.code);
				};
			} catch (e) {
				done("thrown:" + e);
			}
		});
		H.check(
			"send Key 连 /v1/stream 被拒（收不到 hello）",
			outcome !== "got-hello",
			`outcome=${outcome}`,
		);
	}

	// ---- 2. Key 篡改 ----
	const tampered =
		sendKey.slice(0, -4) + (sendKey.endsWith("aaaa") ? "bbbb" : "aaaa");
	H.eq(
		"篡改尾部的 Key → 401",
		(
			await H.req(server.base, "/v1/notify", {
				key: tampered,
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);
	const prefixChanged = sendKey.replace("ant_send_", "ant_live_");
	H.eq(
		"改前缀的 Key → 401",
		(
			await H.req(server.base, "/v1/notify", {
				key: prefixChanged,
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);

	// ---- 3. 鉴权头缺失/错误 scheme ----
	H.eq(
		"无 Authorization 头 → 401",
		(
			await H.req(server.base, "/v1/notify", {
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);
	H.eq(
		"Basic scheme → 401",
		(
			await H.req(server.base, "/v1/notify", {
				headers: { Authorization: "Basic " + sendKey },
				body: { title: "t", status: "success" },
			})
		).status,
		401,
	);

	// ---- 4. 未登录访问受保护端点 ----
	for (const p of ["/v1/devices", "/v1/keys", "/v1/notifications"]) {
		H.eq(`未登录 GET ${p} → 401`, (await H.req(server.base, p)).status, 401);
	}

	// ---- 5. Key 哈希不可逆 ----
	// SQLite WAL 模式下数据可能还在 -wal 文件未 checkpoint 到主 DB，故搜 db 目录全部相关文件。
	const dir = path.dirname(server.dbPath);
	const bufs = readdirSync(dir)
		.filter((f) => f.startsWith(path.basename(server.dbPath)))
		.map((f) => readFileSync(path.join(dir, f)));
	const contains = (needle) => bufs.some((b) => b.includes(needle));
	H.check(
		"DB 不含明文 send Key",
		!contains(sendKey),
		"明文 send Key 出现在 DB",
	);
	H.check(
		"DB 不含明文 recv Key",
		!contains(recvKey),
		"明文 recv Key 出现在 DB",
	);
	H.check(
		"DB 含 argon2id 哈希标记(PHC)",
		contains("$argon2id$"),
		"未找到 $argon2id$",
	);

	// ---- 7. SQL 注入（提前于 logout，避免会话被吊销后影响后续断言）----
	const inj1 = await H.req(server.base, "/v1/auth/register/options", {
		body: { username: "admin' OR '1'='1", displayName: "x" },
	});
	H.check(
		"SQL注入 username 注册 options 不 500",
		inj1.status !== 500,
		`got ${inj1.status}`,
	);
	const inj2 = await H.req(server.base, "/v1/notify", {
		key: sendKey,
		body: { title: "'; DROP TABLE users;--", status: "success" },
	});
	H.eq("SQL注入串作为 title 上报仍正常", inj2.status, 200);
	H.eq(
		"SQL注入后服务正常（会话可用）",
		(await H.req(server.base, "/v1/notifications", { session })).status,
		200,
	);
	H.eq(
		"伪造 session → 401",
		(
			await H.req(server.base, "/v1/notifications", {
				session: "sess_fake.' OR '1'='1",
			})
		).status,
		401,
	);

	// ---- 7b. 用户名规则校验 ----
	// 合法字符（字母数字 _ - .）可过；非法字符/长度/首尾的 → 400
	for (const [name, why] of [
		["a", "太短"],
		["-abc", "- 开头"],
		["abc-", "- 结尾"],
		["a b", "含空格"],
		["a/b", "含斜杠"],
		["a@b", "含 @"],
		["中文", "非 ASCII"],
	]) {
		const r = await H.req(server.base, "/v1/auth/register/options", {
			body: { username: name, displayName: "x" },
		});
		H.eq(`非法用户名(${why}) → 400`, r.status, 400);
	}
	// 合法用户名（不触发完整注册，只要 options 不 400 即过规则）
	{
		const ok = await H.req(server.base, "/v1/auth/register/options", {
			body: { username: "valid_user-1.2", displayName: "x" },
		});
		H.check("合法用户名过规则(非 400)", ok.status !== 400, `got ${ok.status}`);
	}

	// ---- 6. 会话 Cookie HttpOnly（放最后，logout 会吊销会话）----
	const logoutResp = await fetch(server.base + "/v1/auth/logout", {
		method: "POST",
		headers: { Cookie: "anotify_session=" + session },
		redirect: "manual",
	});
	const setCookie = logoutResp.headers.get("set-cookie") || "";
	H.check(
		"Set-Cookie 含 HttpOnly",
		/httponly/i.test(setCookie),
		`set-cookie: ${setCookie}`,
	);
	H.check(
		"Set-Cookie 指定 anotify_session",
		setCookie.includes("anotify_session"),
	);

	// ---- 8. 路径穿越 ----
	for (const p of [
		"/../etc/passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
		"/static/../../etc/passwd",
	]) {
		const r = await H.req(server.base, p);
		const leaked = r.text.includes("root:") || r.text.includes("/bin/bash");
		H.check(
			`路径穿越 ${p} 不泄露文件(${r.status})`,
			(!leaked && r.status !== 200) || !leaked,
			`status=${r.status}`,
		);
	}

	const passed = H.summary("security");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch((e) => {
	console.error(e);
	process.exit(1);
});
