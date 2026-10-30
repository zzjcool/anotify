#!/usr/bin/env node
// harness 自测：/health 身份校验（issue #215）。
//
// 为什么需要这条自测：这个缺陷的**症状**是「套件随机 0通过/0失败」——
// 和超时、环境、代码 bug 完全没法区分，本轮已经为它误判过两次。
// 而它的**修复**（boot id 校验）本身是"看不见的"：正常跑的时候什么都不打印。
// 所以必须有一条用例把两个方向都钉住：
//
//   ① 端口上跑着**别人**的服务（返回 200 但没有匹配的 boot）→ 必须拒绝，且报错要点名
//   ② 端口空闲 → 正常起来，且 /health 回显的 boot 就是本进程的
//
// 运行：ANOTIFY_BIN=./anotify node scripts/e2e/harness_selftest.mjs
// 由 `make ci` 调用（`make e2e` 里也会覆盖到，但那是十几分钟的反馈循环，
// 这条是秒级，放在 PR 门禁里）。

import http from "node:http";
import { startServer } from "./lib/harness.mjs";

const PORT = 6100; // 与 SUITE_PORTS 不重叠；连续 5 个端口会被假服务占满
const ATTEMPTS = 5;

function assert(cond, msg) {
	if (!cond) {
		console.error(`❌ ${msg}`);
		process.exit(1);
	}
	console.log(`  ✅ ${msg}`);
}

const fakes = [];
function startFake(port) {
	return new Promise((resolve) => {
		const s = http.createServer((_req, res) => {
			// 关键：故意只回 {"ok":true}，**没有** boot 字段 —— 这正是
			// "别人的服务/上一次残留的 server" 的样子。
			res.writeHead(200, { "content-type": "application/json" });
			res.end('{"ok":true}');
		});
		s.listen(port, () => resolve(s));
	});
}

async function closeAll() {
	await Promise.all(fakes.map((s) => new Promise((r) => s.close(r))));
	fakes.length = 0;
}

let servers = [];
try {
	// ---- ① 占满整个重试窗口：必须失败，并且点名"跑着别的服务" ----
	for (let i = 0; i < ATTEMPTS; i++) fakes.push(await startFake(PORT + i));
	console.log(`▶ ① ${ATTEMPTS} 个端口都被"别人的服务"占着`);

	let msg = "";
	try {
		servers.push(await startServer({ port: PORT, suiteName: "selftest" }));
	} catch (e) {
		msg = String(e && e.message ? e.message : e);
	}
	assert(servers.length === 0, "没有把别人的服务当成自己的（startServer 抛错而不是返回 base）");
	assert(
		msg.includes("跑着别的服务"),
		`报错点明了根因（实际：${msg.split("\n").filter(Boolean)[1] || msg})`,
	);
	assert(
		msg.includes("没退干净") || msg.includes("worktree"),
		"报错给出了人的下一步（残留进程 / 并行 worktree）",
	);

	// ---- ② 端口放开后必须能起来，且 boot 是本进程的 ----
	await closeAll();
	console.log("▶ ② 端口空闲");
	const srv = await startServer({ port: PORT, suiteName: "selftest" });
	servers.push(srv);
	const body = await (await fetch(srv.base + "/health")).json();
	assert(body.ok === true, "/health 正常");
	assert(
		typeof body.boot === "string" && body.boot.length > 0,
		`/health 回显 boot（${body.boot}）`,
	);

	// ---- ③ 两次启动的 boot 必须不同 ----
	// 这是整套机制的前提：如果 boot 是固定的（或退化值），
	// 那么「同端口上的旧进程」和「新进程」就区分不开，一切又回到原样。
	srv.stop();
	const srv2 = await startServer({ port: PORT, suiteName: "selftest" });
	servers.push(srv2);
	const body2 = await (await fetch(srv2.base + "/health")).json();
	assert(
		body2.boot !== body.boot,
		`两次启动的 boot 不同（${body.boot} ≠ ${body2.boot}）`,
	);

	console.log("✅ harness 自测通过");
} finally {
	for (const s of servers) {
		try {
			s.stop();
		} catch {
			/* 已停 */
		}
	}
	await closeAll();
}
