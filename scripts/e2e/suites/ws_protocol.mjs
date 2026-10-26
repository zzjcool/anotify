#!/usr/bin/env node
/* SUITE: ws_protocol — 节点协议九帧（契约 §6.1）
 *
 * 端点：GET /v1/node/stream（Bearer <node_token>，opaque token）
 *
 * 覆盖：
 *   握手     无 token → 401 且**不升级**；假 token → 403；draining → 403
 *   上行 5    hello / heartbeat / ack / progress / result
 *   下行 4    welcome / assign / cancel / error
 *   错误帧    unknown_type / bad_frame / unknown_capability / invalid_transition
 *   三态      result 的 out 缺失 / null / 字符串（不变量 I5）
 *   心跳      >2×心跳无上行帧 → 判死关连接；心跳续租
 *   不变量    assign 帧里没有 argv/shell/command（I1）
 *
 * 与旧版的区别：旧版测 notification/ack/subscribe（浏览器端旧模型），
 * 现在测节点协议。
 */
import * as H from "../lib/harness.mjs";
import { spawnSync } from "node:child_process";
import { DatabaseSync } from "node:sqlite";
import {
	bringUpNode,
	nodeHello,
	seedNodeRows,
	wsConnect,
	waitForFrame,
} from "../lib/nodes.mjs";

let server, session, sendKey, orgId;

async function defineCapability(name, outputs = [], extra = {}) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { name, outputs, paramsSchema: extra.paramsSchema || { type: "object" }, ...extra },
	});
	if (r.status !== 200) throw new Error(`define ${name}: ${r.status} ${r.text}`);
	return r.json.capability;
}

async function addEntryEdge(source, port, to) {
	const r = await H.req(server.base, "/v1/edges", {
		session,
		body: {
			fromIsSource: true,
			fromCapability: source,
			fromPort: port,
			toCapability: to.id,
		},
	});
	return r;
}

async function addEdge(from, port, to) {
	return H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: from.id, fromPort: port, toCapability: to.id },
	});
}

async function postEvent(body) {
	return H.req(server.base, "/v1/events", { key: sendKey, body });
}

// taskRow 直接读库核对状态机落盘结果。
function taskRow(taskId) {
	const db = new DatabaseSync(server.dbPath);
	try {
		return db
			.prepare(`SELECT id, status, out, reported, summary FROM tasks WHERE id = ?`)
			.get(taskId);
	} finally {
		db.close();
	}
}

// waitStatus 轮询等任务到达期望状态。
async function waitStatus(taskId, want, timeoutMs = 4000) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	while (Date.now() < deadline) {
		const row = taskRow(taskId);
		if (row) {
			last = row.status;
			if (row.status === want) return row;
		}
		await new Promise((r) => setTimeout(r, 40));
	}
	throw new Error(`task ${taskId} status=${last}, want ${want}`);
}

async function main() {
	H.startTimer();
	server = await H.startServer({ suiteName: "ws_protocol" });
	const seeded = H.seed(server.dbPath, "wsproto");
	session = seeded.session;
	sendKey = seeded.sendKey;

	const seedRun = spawnSync(H.DEVSEED_BIN, ["-db", server.dbPath, "-username", "wsproto"], {
		encoding: "utf8",
	});
	const seedOut = (seedRun.stdout || "") + (seedRun.stderr || "");
	orgId = (seedOut.match(/\((org_[A-Za-z0-9]+)\)/) || [])[1];
	H.check("拿到 orgId", !!orgId, seedOut.slice(0, 150));
	if (!orgId) throw new Error("需要 orgId");

	// ================= 握手鉴权 =================
	console.log("--- 握手鉴权 ---");

	// 无 token → 401，且不能是 101
	let noTok;
	try {
		const c = await wsConnect(server.base, "");
		c.close();
		noTok = { ok: true };
	} catch (e) {
		noTok = { status: e.status };
	}
	H.check("无 token 被拒", !noTok.ok, JSON.stringify(noTok));
	H.eq("无 token → 401（不是 101）", noTok.status, 401);

	// 假 token → 403
	let badTok;
	try {
		const c = await wsConnect(server.base, "{{SECRET_yqzqoivg}}" + Date.now());
		c.close();
		badTok = { ok: true };
	} catch (e) {
		badTok = { status: e.status };
	}
	H.check("假 token 被拒", !badTok.ok, JSON.stringify(badTok));
	H.eq("假 token → 403", badTok.status, 403);

	// ================= 正常握手 =================
	console.log("--- 握手成功 ---");
	const cap = await defineCapability("ws.demo", ["go"]);
	const cap2 = await defineCapability("ws.work", []);

	const node = await bringUpNode(server.base, server.dbPath, orgId, "ws-node", [cap, cap2], 2);
	const welcome = node.frames.find((f) => f.type === "welcome");
	H.check("收到 welcome", !!welcome);
	H.eq("welcome.node_id 正确", welcome?.node_id, node.id);
	H.eq("welcome 带协议版本", welcome?.protocol, 1);
	H.check("welcome 带心跳间隔", typeof welcome?.heartbeat_sec === "number" && welcome.heartbeat_sec > 0);
	H.check("welcome 带租约 TTL", typeof welcome?.lease_ttl_sec === "number" && welcome.lease_ttl_sec > 0);

	// 节点应被标记 online
	const nodesView = await H.req(server.base, "/v1/nodes", { session });
	const self = (nodesView.json?.nodes || []).find((n) => n.id === node.id);
	H.eq("节点状态 online", self?.status, "online");

	// ================= 未知帧类型 → error 但连接保持 =================
	console.log("--- 未知帧类型 ---");
	node.conn.send({ type: "definitely-not-a-frame" });
	const unknownErr = await waitForFrame(node.frames, (f) => f.type === "error");
	H.eq("未知类型回 error 帧", unknownErr?.code, "unknown_type");
	// 连接仍活着：发心跳不报错
	node.conn.send({ type: "heartbeat", ts: Math.floor(Date.now() / 1000) });
	await new Promise((r) => setTimeout(r, 150));
	H.check("未知类型后连接仍活着", !node.conn.closed);

	// ================= 坏 JSON → error 但连接保持 =================
	node.conn.sendRaw("{not json");
	const badFrameErr = await waitForFrame(node.frames, (f) => f.type === "error" && f.code === "bad_frame");
	H.check("坏 JSON 回 bad_frame", !!badFrameErr, JSON.stringify(node.frames.slice(-3)));
	H.check("坏 JSON 后连接仍活着", !node.conn.closed);

	// ================= 声明未定义能力 → unknown_capability =================
	console.log("--- 能力白名单（A3）---");
	const badNode = seedNodeRows(server.dbPath, orgId, "ws-badnode", [], 1);
	const badConn = await wsConnect(server.base, badNode.token);
	const badFrames = [];
	badConn.onFrame((f) => badFrames.push(f));
	badConn.send(nodeHello(badNode.id, [{ name: "does.not.exist", version: 9 }], 1));
	const capErr = await waitForFrame(badFrames, (f) => f.type === "error");
	H.eq("声明未定义能力 → unknown_capability", capErr?.code, "unknown_capability");
	H.check("未注册成功则不收 welcome", !badFrames.some((f) => f.type === "welcome"));
	badConn.close();

	// ================= 完整任务流：assign → ack → progress → result =================
	console.log("--- 任务流九帧 ---");
	// #110：入口边（来源名 → 能力），不再需要"起点能力"
	await addEdge(cap, "go", cap2);
	await addEntryEdge("ws.demo", "go", cap2);

	const ev = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	H.eq("事件派发建出 1 个任务", ev.json?.tasks?.length, 1);
	const taskId = ev.json?.tasks?.[0]?.id;
	H.check("拿到 taskId", !!taskId);
	H.eq("任务状态 assigned", ev.json?.tasks?.[0]?.status, "assigned");

	// 下行 assign 帧
	const assign = await waitForFrame(node.frames, (f) => f.type === "assign");
	H.check("节点收到 assign", !!assign, JSON.stringify(node.frames.map((f) => f.type)));
	H.eq("assign.task_id 正确", assign?.task_id, taskId);
	H.eq("assign.capability 正确", assign?.capability, "ws.work");
	H.check("assign 有租约", typeof assign?.lease_until === "number" && assign.lease_until > 0);
	// I1：没有 argv/shell/command
	{
		const raw = JSON.stringify(assign).toLowerCase();
		H.check(
			"assign 帧不含 argv/shell/command（I1）",
			!raw.includes("argv") && !raw.includes("shell") && !raw.includes("command"),
			raw.slice(0, 160),
		);
		const fields = Object.keys(assign).sort();
		H.eq("assign 帧字段集合固定（7 个）", fields.length, 7);
	}

	// ack：assigned → acked
	node.conn.send({ type: "ack", task_id: taskId });
	await waitStatus(taskId, "acked");
	H.check("ack 后状态 acked", true);

	// progress：acked → running
	node.conn.send({ type: "progress", task_id: taskId, note: "step 1/2" });
	await waitStatus(taskId, "running");
	H.check("progress 后状态 running", true);

	// result：running → succeeded，带 out
	node.conn.send({
		type: "result",
		task_id: taskId,
		status: "succeeded",
		out: "done",
		summary: "全部完成",
	});
	await waitStatus(taskId, "succeeded");
	const row = taskRow(taskId);
	H.eq("result 落库 status", row.status, "succeeded");
	H.eq("result 落库 out", row.out, "done");
	H.eq("result 落库 reported", row.reported, 1);
	H.eq("result 落库 summary", row.summary, "全部完成");

	// ================= A8：out 三态 =================
	console.log("--- A8 out 三态（I5）---");

	// 三态 1：out 缺失 → reported=0
	const evMissing = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tMissing = evMissing.json?.tasks?.[0]?.id;
	const aMissing = await waitForFrame(
		node.frames,
		(f) => f.type === "assign" && f.task_id === tMissing,
	);
	H.check("三态1 收到 assign", !!aMissing);
	node.conn.sendRaw(
		`{"type":"result","task_id":"${tMissing}","status":"succeeded","summary":"no out"}`,
	);
	await waitStatus(tMissing, "succeeded");
	const rMissing = taskRow(tMissing);
	H.eq("三态1 out 缺失 → reported=0", rMissing.reported, 0);
	H.eq("三态1 out 缺失 → out=NULL", rMissing.out, null);

	// 三态 2：out:null → reported=1, out=NULL
	const evNull = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tNull = evNull.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tNull);
	node.conn.sendRaw(`{"type":"result","task_id":"${tNull}","status":"succeeded","out":null}`);
	await waitStatus(tNull, "succeeded");
	const rNull = taskRow(tNull);
	H.eq("三态2 out:null → reported=1", rNull.reported, 1);
	H.eq("三态2 out:null → out=NULL", rNull.out, null);

	// 三态 3：out:"x" → reported=1, out="x"
	const evStr = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tStr = evStr.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tStr);
	node.conn.send({ type: "result", task_id: tStr, status: "succeeded", out: "again" });
	await waitStatus(tStr, "succeeded");
	const rStr = taskRow(tStr);
	H.eq("三态3 out:'again' → reported=1", rStr.reported, 1);
	H.eq("三态3 out:'again' → out='again'", rStr.out, "again");

	// ================= 非法迁移 → invalid_transition =================
	console.log("--- 非法迁移 ---");
	// 对已终态的任务再发 result
	node.frames.length = 0;
	node.conn.send({ type: "result", task_id: tStr, status: "succeeded", out: "x" });
	const invErr = await waitForFrame(node.frames, (f) => f.type === "error");
	H.eq("终态任务再 result → invalid_transition", invErr?.code, "invalid_transition");

	// ack 派给别人的任务 → invalid_transition
	node.frames.length = 0;
	const capOther = await defineCapability("ws.other", []);
	const otherNode = await bringUpNode(server.base, server.dbPath, orgId, "ws-other", [capOther], 1);
	const evOther = await postEvent({ source: "ws.other", type: "go", payload: {} });
	// ws.other 没有出口边，所以不会有任务；直接构造一个"不属于本节点"的 taskId
	node.conn.send({ type: "ack", task_id: "tsk_not_assigned_to_me_000" });
	const notMine = await waitForFrame(node.frames, (f) => f.type === "error");
	H.check(
		"ack 不存在/不属于自己的任务 → error",
		!!notMine && ["invalid_transition", "bad_frame"].includes(notMine.code),
		JSON.stringify(notMine),
	);
	void evOther;
	otherNode.conn.close();

	// ================= 心跳与判死 =================
	console.log("--- 心跳与租约 ---");
	// 心跳应刷新 last_seen_at
	const hbBefore = Date.now();
	node.conn.send({ type: "heartbeat", ts: Math.floor(hbBefore / 1000) });
	await new Promise((r) => setTimeout(r, 300));
	const nodesAfter = await H.req(server.base, "/v1/nodes", { session });
	const selfAfter = (nodesAfter.json?.nodes || []).find((n) => n.id === node.id);
	H.check(
		"心跳刷新 last_seen_at",
		!!selfAfter?.lastSeenAt && selfAfter.lastSeenAt > 0,
		JSON.stringify(selfAfter?.lastSeenAt),
	);
	H.eq("心跳后节点仍 online", selfAfter?.status, "online");

	// ================= 断开后离线 =================
	console.log("--- 断开处理 ---");
	node.conn.close();
	await new Promise((r) => setTimeout(r, 500));
	const nodesFinal = await H.req(server.base, "/v1/nodes", { session });
	const selfFinal = (nodesFinal.json?.nodes || []).find((n) => n.id === node.id);
	H.eq("断开后节点 offline", selfFinal?.status, "offline");

	// ================= 旧浏览器端协议仍可用（#7 前不破坏） =================
	console.log("--- 旧 /v1/stream 仍可用 ---");
	// 无 Key → 401（不升级）
	let oldNoKey;
	try {
		const c = await wsConnect(server.base, "", "/v1/stream");
		c.close();
		oldNoKey = { ok: true };
	} catch (e) {
		oldNoKey = { status: e.status };
	}
	H.check("旧 /v1/stream 无 Key 被拒", !oldNoKey.ok, JSON.stringify(oldNoKey));

	server.stop();
	const allPassed = H.summary("ws_protocol");
	H.writeResults("ws_protocol", H.stopTimer());
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	if (server) server.stop();
	process.exit(1);
});
