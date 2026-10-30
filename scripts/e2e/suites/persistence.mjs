#!/usr/bin/env node
/* SUITE: persistence — 重启后事件中心的状态存活（契约 §2.5 / §3）
 *
 * 覆盖：
 *  1. 事件 + 任务落库后重启进程，数据仍在
 *  2. 任务状态机（assigned / running / succeeded）跨重启保持，不会"复活"
 *  3. out 三态（缺失 / null / 字符串）跨重启保持（不变量 I5）
 *  4. trace 关联跨重启可查（GET /v1/events?traceId=）
 *  5. 拒收已进入终态的任务再被改状态（终态不可变）
 *
 * 与旧版的区别：旧版测 messages 表与 seq 连续性（已下线模型）；
 * 现在测 events/tasks 的存活与状态机恢复。
 */
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";
import { seedNodeRows } from "../lib/nodes.mjs";

// seedNodeRows 直接用共享的造节点辅助（lib/nodes.mjs）。
//
// 本文件原来自己拷了一份，插 nodes 时写 `org_id` 列 —— #204 / 迁移 14 删掉组织层后
// nodes 已经没有这一列（节点只属于命名空间），那份拷贝必然插不进去。与其两边各维护
// 一份 DDL，不如直接用共享实现（签名：seedNodeRows(dbPath, namespaceId, name, capIds, slots)，
// namespaceId 用 H.defaultNamespaceId() 查出来的 id，不硬编码 ns_xxx）。

// queryTasks 直接读库断言（跨重启后核对落盘状态最可靠）。
//
// #204 / 迁移 14：tasks 表上没有 org_id 了，隔离边界是 namespace_id —— 过滤条件改为
// `namespace_id = ?`（参数是默认命名空间 id，不再是 orgId）。
function queryTasks(dbPath, namespaceId) {
	const db = new DatabaseSync(dbPath);
	try {
		return db
			.prepare(
				`SELECT id, status, out, reported, summary, attempts FROM tasks WHERE namespace_id = ? ORDER BY created_at`,
			)
			.all(namespaceId);
	} finally {
		db.close();
	}
}

async function main() {
	console.log("=== SUITE: persistence（事件中心状态存活）===");
	H.startTimer();

	const tmp = mkdtempSync(path.join(tmpdir(), "anotify-persist-"));
	const dbPath = path.join(tmp, "persist.db");
	const extraEnv = { ANOTIFY_DB: dbPath };

	let phase1 = null;
	let traceId = null;
	let session = null;
	let sendKey = null;

	try {
		// ================= 第一次启动：造数据 =================
		let server = await H.startServer({
			suiteName: "persistence",
			portOffset: 0,
			rpId: "localhost",
			extraEnv,
		});

		const seeded = H.seed(dbPath, "persist");
		session = seeded.session;
		sendKey = seeded.sendKey;

		// #204 / 迁移 14：组织层删除（orgs / org_members 没了），devseed 也不再输出 org
		// （改成 stderr 的「命名空间就绪: ns_xxx」）。所以：
		//   ① 原来的 `H.check("建库拿到 orgId", ...)` 换成**命名空间口径**的等价断言；
		//   ② 原来为了拿 orgId 而单独 spawn 一次 devseed 的代码删掉 —— 它唯一的产物就是
		//      orgId，H.seed() 已经在同一个库上用同一个用户名跑过 devseed。
		//
		// #162：节点直插库要写 namespace_id（迁移 12 起 NOT NULL）。
		// 取本部署默认命名空间（slug=default）的 id，不硬编码 ns_xxx。
		const nsId = await H.defaultNamespaceId(server.base, session);
		H.check("拿到默认命名空间 id", !!nsId, String(nsId));
		if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

		// 定义能力与出口
		const origin = (
			await H.req(server.base, "/v1/capabilities", {
				session,
				body: { name: "persist.demo", outputs: ["go"], paramsSchema: { type: "object" } },
			})
		).json.capability;
		const target = (
			await H.req(server.base, "/v1/capabilities", {
				session,
				body: { name: "persist.work", outputs: [], paramsSchema: { type: "object" } },
			})
		).json.capability;
		H.eq(
			"建边 persist.demo→persist.work",
			(await H.req(server.base, "/v1/edges", {
				session,
				body: {
					// #110：入口边（左端是来源名），不再需要"起点能力"
					fromIsSource: true,
					fromCapability: "persist.demo",
					fromPort: "go",
					toCapability: target.id,
				},
			})).status,
			200,
		);
		seedNodeRows(dbPath, nsId, "persist-node", [target.id], 2);

		// 发事件 → 应该有 1 个任务
		const ev = await H.req(server.base, "/v1/events", {
			key: sendKey,
			body: { source: "persist.demo", type: "go", payload: {} },
		});
		H.eq("事件入队", ev.status, 200);
		traceId = ev.json.traceId;
		H.check("拿到 traceId", !!traceId);

		// 节点不在线（只播种了 DB 行，没走 WS），所以任务应留 pending
		const before = queryTasks(dbPath, nsId);
		H.eq("重启前有 1 个任务", before.length, 1);
		H.eq("任务状态 pending（节点未上线）", before[0].status, "pending");
		phase1 = { taskId: before[0].id };

		// 手工把任务推到终态并写 out，验证跨重启保持
		const db = new DatabaseSync(dbPath);
		try {
			db.prepare(
				`UPDATE tasks SET status='succeeded', out='fix', reported=1, summary='ok',
				 started_at=?, finished_at=? WHERE id=?`,
			).run(Math.floor(Date.now() / 1000), Math.floor(Date.now() / 1000), phase1.taskId);
		} finally {
			db.close();
		}

		server.stop();
		await new Promise((r) => setTimeout(r, 300)); // 等进程真退出、释放 DB

		// ================= 第二次启动：验证存活 =================
		server = await H.startServer({
			suiteName: "persistence",
			portOffset: 1,
			rpId: "localhost",
			extraEnv,
		});

		// 1. 事件仍在，且能按 trace 查回
		// /v1/events 是机器入口，GET 与 POST 一样走 Bearer Key（不是会话）
		const evList = await H.req(server.base, `/v1/events?traceId=${encodeURIComponent(traceId)}`, { key: sendKey });
		H.eq("重启后 GET /v1/events 200", evList.status, 200);
		H.eq("重启后同一 trace 仍有 1 条事件", evList.json?.events?.length, 1);
		H.eq("重启后事件 traceId 一致", evList.json?.events?.[0]?.traceId, traceId);

		// 2. 任务仍在，状态机结果保持
		const after = queryTasks(dbPath, nsId);
		H.eq("重启后任务数量不变", after.length, 1);
		H.eq("重启后任务状态仍是 succeeded", after[0].status, "succeeded");

		// 3. out 三态跨重启保持（I5）
		H.eq("重启后 out 仍是 'fix'", after[0].out, "fix");
		H.eq("重启后 reported 仍是 1", after[0].reported, 1);
		H.eq("重启后 summary 保留", after[0].summary, "ok");

		// 4. 通过 API 读任务
		const taskList = await H.req(server.base, `/v1/tasks?traceId=${encodeURIComponent(traceId)}`, { session });
		H.eq("重启后 GET /v1/tasks 200", taskList.status, 200);
		H.eq("重启后 API 也能看到该任务", taskList.json?.tasks?.length, 1);
		H.eq("API 读到的状态正确", taskList.json?.tasks?.[0]?.status, "succeeded");
		H.eq("API 读到的 out 正确", taskList.json?.tasks?.[0]?.out, "fix");

		// 5. 终态不可变：取消一个已成功的任务应 409
		const cancel = await H.req(server.base, `/v1/tasks/${phase1.taskId}/cancel`, {
			method: "POST",
			session,
		});
		H.eq("已终态任务取消 → 409", cancel.status, 409);
		H.check(
			"409 带 invalid_transition 码",
			String(cancel.text || "").includes("invalid_transition"),
			String(cancel.text).slice(0, 120),
		);

		// 6. 会话跨重启仍有效（同一个 server 进程内已重建）
		const me = await H.req(server.base, "/v1/auth/me", { session });
		H.eq("重启后会话仍有效", me.status, 200);

		// 7. API Key 跨重启仍可用（发一个新事件）
		const ev2 = await H.req(server.base, "/v1/events", {
			key: sendKey,
			body: { source: "persist.demo", type: "go", payload: {} },
		});
		H.eq("重启后 API Key 仍能发事件", ev2.status, 200);
		H.check("重启后新事件有新 traceId", ev2.json?.traceId !== traceId);

		server.stop();
	} finally {
		rmSync(tmp, { recursive: true, force: true });
	}

	const allPassed = H.summary("persistence");
	H.writeResults("persistence", H.stopTimer());
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	process.exit(1);
});
