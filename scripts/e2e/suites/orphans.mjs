#!/usr/bin/env node
/* SUITE: orphans — 孤儿 pending 任务的诊断与处置（#270）
 *
 * 病根：`pending` 原本没有任何 abandon 路径。能力升级后没有节点声明那个版本、
 * 唯一能跑它的节点永久下线、能力被删除……任务就永远停在 pending。
 * 更要命的是背压的判定信号是 `MIN(created_at) WHERE status='pending'`，
 * **一条孤儿任务就能把整个部署的新事件入口钉死在 503 上**。
 *
 * #269 给背压装了 30 分钟 fail-open 泄压阀（正确的取舍），但那是治症状。
 * 本套件验的是治根的那一步：先能看见（GET /v1/orphans）、再能处置（abandon）。
 *
 * 覆盖：
 *  1. 没有任何节点声明 → 列出来，原因 `no_node_declares`
 *  2. 有节点声明但在线 → **不**列（不能误报）
 *  3. 有节点声明但离线 → 列出来，原因 `no_online_node`
 *  4. 时间下限：刚建的任务不算孤儿（派发瞬间"暂时没有在线节点"是正常现象）
 *  5. abandon → 200，状态 abandoned，原因写进 error 列（事后能回答"为什么没跑"）
 *  6. 再 abandon 一次 → 409（CAS，不假装成功）；不存在的任务 → 404
 *  7. 参数校验：olderThan=0 / 乱写 → 400；未登录 → 401
 *  8. `namespaceId=*` 可用（诊断需要一次看全）
 */
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";

function withDB(dbPath, fn) {
	const db = new DatabaseSync(dbPath);
	try {
		return fn(db);
	} finally {
		db.close();
	}
}

async function nsID(server, session) {
	return H.defaultNamespaceId(server.base, session);
}

// mkCustomCap 建一个没人会执行的能力（没有任何节点声明它）。
async function mkCustomCap(server, session, ns, name) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		// P1-b2：建能力必须有实现（缺 run → 400 impl_missing）。这里的空脚本不会被执行
		// （能力没人声明 → 任务派不出去，正是要验的场景）。
		body: {
			namespaceId: ns,
			name,
			// P4：请求体是 spec 形状（Capability Spec v3 §5）。
			spec: {
				params: { type: "object" },
				run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\nexit 0\n" } },
			},
		},
	});
	const cap = r.json?.capability;
	if (!cap?.id) throw new Error(`建能力 ${name} 失败: ${r.status} ${r.text}`);
	return { id: cap.id, name, version: cap.version ?? 1 };
}

// seedPendingTask 直接插一条 pending，并把 created_at 拨到过去。
//
// 为什么不走派发：真实的「派不出去」需要先有节点声明、再把节点弄下线，
// 那会变成时序测试。这里要验的是判定与处置，直接把库摆成目标状态最确定。
function seedPendingTask(dbPath, namespaceId, cap, ageSec) {
	return withDB(dbPath, (db) => {
		const nowSec = Math.floor(Date.now() / 1000);
		const id = `tsk_orph${Math.random().toString(36).slice(2, 10)}`;
		db.prepare(
			`INSERT INTO tasks (id, namespace_id, trace_id, path, capability_id, capability, version,
			                    params, status, cap_hash, graph_id, created_at)
			 VALUES (?, ?, ?, '[]', ?, ?, 1, '{}', 'pending', 'e2e', 'grf_e2e', ?)`,
		).run(id, namespaceId, `trace_${id}`, cap.id, cap.name, nowSec - ageSec);
		return id;
	});
}

// seedNode 插一个节点（可带能力声明）。status 决定「能不能跑」。
function seedNode(dbPath, namespaceId, cap, status) {
	return withDB(dbPath, (db) => {
		const nowSec = Math.floor(Date.now() / 1000);
		const id = `nd_e2e${Math.random().toString(36).slice(2, 10)}`;
		db.prepare(
			`INSERT INTO nodes (id, namespace_id, name, tags, status, token_hash, created_at)
			 VALUES (?, ?, ?, '[]', ?, ?, ?)`,
		).run(id, namespaceId, `node-${id}`, status, `hash-${id}`, nowSec);
		if (cap) {
			// 迁移 18（#276 协议 v2）之后只有三列（run_spec 被产物 digest 取代）
			db.prepare(
				`INSERT INTO node_capabilities (node_id, capability_id, namespace_id)
				 VALUES (?, ?, ?)`,
			).run(id, cap.id, namespaceId);
		}
		return id;
	});
}

function dropNode(dbPath, nodeID) {
	withDB(dbPath, (db) => db.prepare(`DELETE FROM nodes WHERE id = ?`).run(nodeID));
}

function readTask(dbPath, id) {
	return withDB(dbPath, (db) =>
		db.prepare(`SELECT status, error, finished_at FROM tasks WHERE id = ?`).get(id),
	);
}

async function main() {
	const server = await H.startServer({ suiteName: "orphans", rpId: "localhost" });
	try {
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "orphans");
		const ns = await nsID(server, s.session);

		// ---------- 1. 没有任何节点声明 → 列出来 ----------
		console.log("--- 1. 判定：no_node_declares ---");
		const cap = await mkCustomCap(server, s.session, ns, `orphan.cap.${Date.now()}`);
		const oldTask = seedPendingTask(dbPath, ns, cap, 7200);

		const list1 = await H.req(server.base, `/v1/orphans`, { session: s.session });
		H.eq("GET /v1/orphans → 200", list1.status, 200);
		H.check(
			"orphans 是数组（空列表也不能是 null）",
			Array.isArray(list1.json?.orphans),
			JSON.stringify(list1.json).slice(0, 120),
		);
		const hit = (list1.json?.orphans || []).find((o) => o.id === oldTask);
		H.check("列出那条 2 小时前的孤儿任务", !!hit, `count=${list1.json?.count}`);
		H.eq("原因 = no_node_declares", hit?.reason, "no_node_declares");
		H.check(
			"idleSec ≈ 7200",
			(hit?.idleSec || 0) >= 7000,
			`idleSec=${hit?.idleSec}`,
		);
		H.eq("默认 olderThan = 3600 秒", list1.json?.olderThan, 3600);

		// ---------- 2. 有在线节点声明 → 不是孤儿 ----------
		console.log("--- 2. 有在线节点时不能误报 ---");
		const onlineNode = seedNode(dbPath, ns, cap, "online");
		const list2 = await H.req(server.base, `/v1/orphans`, { session: s.session });
		H.check(
			"在线节点能跑 → 不出现在孤儿列表",
			!(list2.json?.orphans || []).some((o) => o.id === oldTask),
			`count=${list2.json?.count}`,
		);

		// ---------- 3. 节点离线 → 原因换成 no_online_node ----------
		console.log("--- 3. 离线节点：no_online_node ---");
		withDB(dbPath, (db) =>
			db.prepare(`UPDATE nodes SET status = 'offline' WHERE id = ?`).run(onlineNode),
		);
		const list3 = await H.req(server.base, `/v1/orphans`, { session: s.session });
		const hit3 = (list3.json?.orphans || []).find((o) => o.id === oldTask);
		H.check("离线节点的任务重新算孤儿", !!hit3);
		H.eq("原因 = no_online_node（与「没人声明」区分开）", hit3?.reason, "no_online_node");
		dropNode(dbPath, onlineNode);

		// ---------- 4. 时间下限：刚建的不算孤儿 ----------
		console.log("--- 4. olderThan 下限防误报 ---");
		const freshTask = seedPendingTask(dbPath, ns, cap, 10);
		const list4 = await H.req(server.base, `/v1/orphans`, { session: s.session });
		H.check(
			"10 秒前的 pending 不算孤儿",
			!(list4.json?.orphans || []).some((o) => o.id === freshTask),
		);

		// ---------- 5. abandon：处置并留下原因 ----------
		console.log("--- 5. abandon ---");
		const ab = await H.req(server.base, `/v1/tasks/${oldTask}/abandon`, {
			session: s.session,
			method: "POST",
			body: { reason: "能力没有节点声明，已确认不再需要" },
		});
		H.eq("POST abandon → 200", ab.status, 200);
		H.eq("返回的任务状态 = abandoned", ab.json?.task?.status, "abandoned");
		const row = readTask(dbPath, oldTask);
		H.eq("库里状态 = abandoned", row?.status, "abandoned");
		H.eq("原因写进 error 列", row?.error, "能力没有节点声明，已确认不再需要");
		H.check("终态写了 finished_at", !!row?.finished_at, String(row?.finished_at));

		const list5 = await H.req(server.base, `/v1/orphans`, { session: s.session });
		H.check(
			"处置后不再出现在孤儿列表",
			!(list5.json?.orphans || []).some((o) => o.id === oldTask),
		);

		// ---------- 6. 幂等性/冲突：CAS 不假装成功 ----------
		console.log("--- 6. 冲突与 404 ---");
		const again = await H.req(server.base, `/v1/tasks/${oldTask}/abandon`, {
			session: s.session,
			method: "POST",
			body: { reason: "second attempt" },
		});
		H.eq("已 abandoned 再 abandon → 409", again.status, 409);
		H.eq("错误码 invalid_transition", again.json?.code, "invalid_transition");
		H.check(
			"第二次拒绝没有改写原因",
			readTask(dbPath, oldTask)?.error === "能力没有节点声明，已确认不再需要",
			String(readTask(dbPath, oldTask)?.error),
		);

		const missing = await H.req(server.base, `/v1/tasks/tsk_nope/abandon`, {
			session: s.session,
			method: "POST",
			body: { reason: "x" },
		});
		H.eq("不存在的任务 → 404", missing.status, 404);

		// ---------- 7. 参数与鉴权 ----------
		console.log("--- 7. 参数校验 / 鉴权 ---");
		const zero = await H.req(server.base, `/v1/orphans?olderThan=0`, { session: s.session });
		H.eq("olderThan=0 → 400（不许把刚派发的当孤儿）", zero.status, 400);
		const bogus = await H.req(server.base, `/v1/orphans?olderThan=abc`, { session: s.session });
		H.eq("olderThan=abc → 400", bogus.status, 400);
		const anon = await H.req(server.base, `/v1/orphans`, {});
		H.eq("未登录 → 401", anon.status, 401);
		// opts.namespaceId === null = 显式声明"就是不带"
		const noNS = await H.req(server.base, `/v1/orphans`, { session: s.session, namespaceId: null });
		H.check(
			"缺 namespaceId → 400（命名空间端点必须显式指定）",
			noNS.status === 400,
			`status=${noNS.status} ${String(noNS.text).slice(0, 80)}`,
		);

		// ---------- 8. 跨命名空间 ----------
		console.log("--- 8. namespaceId=* ---");
		const all = await H.req(server.base, `/v1/orphans?namespaceId=*`, { session: s.session });
		H.eq("namespaceId=* → 200", all.status, 200);
		H.check("跨命名空间视图标记 allScopes", all.json?.allScopes === true, JSON.stringify(all.json?.allScopes));
	} finally {
		server.stop();
	}
}

main()
	.then(() => {
		process.exit(H.summary("orphans") ? 0 : 1);
	})
	.catch((e) => {
		console.error(e);
		process.exit(1);
	});
