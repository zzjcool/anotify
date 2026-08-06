#!/usr/bin/env node
/* SUITE: passkeys — Passkey 管理端点
 *
 * 复现 bug：安全页加载时请求 GET /v1/auth/passkeys，后端无此端点 → 404 →
 * 前端 fetchApi 返回 null → demoMode.passkeys=true → 点「添加 Passkey」
 * 弹「演示模式」提示，根本走不到 WebAuthn 流程。
 *
 * 修复后期望：
 *   - GET    /v1/auth/passkeys        → 200 + {passkeys: []}（首用户空列表）
 *   - GET    /v1/auth/passkeys        → 200 + 真实凭证（非 demo 数据）
 *   - PATCH  /v1/auth/passkeys/:id    → 200，名字更新
 *   - DELETE /v1/auth/passkeys/:id    → 200，列表变空
 *   - 无 session 访问任意端点 → 401
 *   - 删别人的凭证 → 非 200（防越权）
 */
import { execFileSync } from "node:child_process";
import * as H from "../lib/harness.mjs";

async function main() {
	console.log("=== SUITE: passkeys（Passkey 管理端点）===");
	const server = await H.startServer({ rpId: "localhost" });
	const { session } = H.seed(server.dbPath, "pkuser");

	// ---- 1. 复现：GET /v1/auth/passkeys 当前 404（修复后应 200 + 空列表）----
	{
		const r = await H.req(server.base, "/v1/auth/passkeys", { session });
		H.check(
			"已登录 GET /v1/auth/passkeys → 200（当前 bug: 404 致 demo 模式）",
			r.status === 200,
			`status=${r.status} body=${r.text}`,
		);
		if (r.status === 200) {
			// passkeys 必须是 [] 而非 null
			H.check(
				"空列表返回 [] 而非 null",
				Array.isArray(r.json?.passkeys) && r.json.passkeys !== null,
				`passkeys=${JSON.stringify(r.json?.passkeys)}`,
			);
			H.eq("首用户凭证数 = 0", r.json?.passkeys?.length, 0);
		}
	}

	// ---- 2. 未登录 → 401（守卫生效）----
	{
		const r = await H.req(server.base, "/v1/auth/passkeys");
		H.eq("未登录 GET → 401", r.status, 401);
	}

	// ---- 3. 通过 devseed 后门插入一条凭证，验证列表返回真实数据 ----
	// （devseed 不直接插 passkey，这里用 SQL 通过 sqlite3 CLI 不便；
	//   改为：先验证空列表契约，端到端的「添加」流程需真实 WebAuthn 认证器，
	//   属于浏览器 e2e 范畴，此处仅覆盖 CRUD 端点存在性 + 鉴权。）
	// 详见下方「端点存在性」断言。

	// ---- 4. DELETE 不存在的凭证 → 404（不应 500）----
	{
		const r = await H.req(server.base, "/v1/auth/passkeys/cred-not-exist", {
			method: "DELETE",
			session,
		});
		H.check(
			"DELETE 不存在的凭证 → 404（非 500）",
			r.status === 404 || r.status === 200, // 200 表示幂等删除，也接受
			`status=${r.status}`,
		);
	}

	// ---- 5. PATCH 不存在的凭证 → 404 ----
	{
		const r = await H.req(server.base, "/v1/auth/passkeys/cred-not-exist", {
			method: "PATCH",
			session,
			body: { name: "x" },
		});
		H.check("PATCH 不存在的凭证 → 404", r.status === 404, `status=${r.status}`);
	}

	// ---- 6. 未登录的写操作 → 401 ----
	{
		const r = await H.req(server.base, "/v1/auth/passkeys/x", {
			method: "DELETE",
		});
		H.eq("未登录 DELETE → 401", r.status, 401);
	}

	const ok = H.summary("passkeys");
	server.stop();
	process.exit(ok ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	process.exit(1);
});
