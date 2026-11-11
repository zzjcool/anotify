#!/usr/bin/env node
/* SUITE: ws_protocol — v4 节点协议（帧级，真后端 + 真 WS 会话，执行由 mock 节点驱动）
 *
 * 为什么要有「帧级」这一层：node_v4 验的是**真 anode 二进制**把整条链跑通（那是最有
 * 说服力的证据，但它只会走绿路径）。协议本身的**边界**——坏帧、方向错误、未知帧、
 * 未 accept 就 result、旧 attempt 的 result、取消与结果的竞态——只有能自己造帧的
 * 客户端才测得出来。所以本套件用 mock 节点：它的每一帧都是手写的，断言的是**中枢**
 * 的判据（而不是 anode 的容错）。
 *
 * 帧契约的唯一来源是 internal/protocol；本文件不发明第二种形状。
 *
 * 覆盖（每条都对应 capability.md / config.md 的一句话）：
 *   A. 握手：v2 hello 被拒（1008，且不登记节点）/ 未知 token / name 与 token 不一致 /
 *      正确 hello → welcome 是 v4 形状（configRevision/nodeVars/nodeLimits/namespaces）
 *   B. 配置：config.get 复用 welcome（D-I2）/ config.applied 落 appliedRevision+hash /
 *      坏 revision 是协议违例（1008）/ hash 漂移可见
 *   C. 部署帧：capability.deploy 只带 refs（不带包体）/ 回执四元组配对 → applied
 *   D. 派发：task.dispatch **不授执行权**（没有 leaseId/attemptToken）/ proposedAttempt=1 /
 *      configRevision 与 welcome 同源 / assignment 有窗口
 *   E. 授权：accept 用对名字+真实 slot → lease.grant（attempt 才计数）/ lease.renew → lease.ack /
 *      错 lease 的 renew 不回 ack / accept 错节点名或缺失 slot 是协议违例（1008）
 *   F. 结果绑定：错 attempt / 错 lease / 取消在飞 → **不回 result.ack** 且任务不动 /
 *      正确绑定 → accepted=true，任务终态、槽位释放
 *   G. 日志：GET logs → 中枢发 logs_request → 回 logs_response → 行序/流分开；
 *      没有档案（pending 任务 / 节点回 no_local_archive）→ **410 no_local_archive**
 *   H. 取消：task.cancel 带完整历史绑定（taskId+attempt+leaseId+slot+reason+grace）/
 *      错绑定的 cancel.ack 不清槽 / 正确绑定 → cancelled + 槽位真释放 /
 *      pending 取消直接终态（attempt=0）/ 重复取消 → 409
 *   I. 帧边界：未知帧不断连（后续 config.get 仍工作）/ 坏 JSON → 1003 /
 *      方向错误（节点发下行帧）→ 1008 / 超大帧 → 连接被断（1 MiB 软上限）
 *
 * 纪律：所有「等一帧」都有上限；「不该来」的断言用有界窗口（并明确写出窗口），
 * 不用 sleep 猜状态。
 */
import { createHash, randomUUID } from "node:crypto";

import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "ws_protocol";
const NS = V.DEFAULT_NS;

// 机器配置内容哈希（protocol.MachineConfigHash 的同一份算法）。
// 正确 hash 才能让 config.applied 被接受为「配置已生效」；错误 hash 会在 grant 时
// 以 config_not_applied 拒绝（本套件用这条边界钉「配置未生效不算执行失败」）。
function machineConfigHash(vars = {}, total = 1, perCapability = {}) {
	return "sha256:" + createHash("sha256").update(JSON.stringify({ vars, limits: { total, perCapability } })).digest("hex");
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// pollUntil：等一个异步结论，到点抛错（带最后一次观察值）。
async function pollUntil(what, fn, { timeoutMs = 8000, intervalMs = 50 } = {}) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	for (;;) {
		last = await fn();
		if (last) return last;
		if (Date.now() > deadline) {
			throw new Error(`等待超时（${timeoutMs}ms）：${what}\n最后一次观察: ${JSON.stringify(last)}`);
		}
		await sleep(intervalMs);
	}
}

// ---------- mock 节点 ----------
//
// 一个 mock 节点 = 一条真 WS 会话 + 已收帧数组 + 关闭原因。所有断言都读这两样东西，
// 没有任何「假装收到」的辅助。
async function mkMock(base, name, token) {
	const conn = await N.wsConnect(base, token);
	const frames = [];
	let closed = null;
	conn.onFrame((f) => frames.push(f));
	conn.onClose((info) => {
		closed = info;
	});
	return {
		name,
		conn,
		frames,
		closed: () => closed,
		// waitFrame 等某类帧出现（可带谓词）；超时抛错。
		async waitFrame(type, pred = () => true, timeoutMs = 6000) {
			return pollUntil(`收到 ${type} 帧`, async () => frames.find((f) => f.type === type && pred(f)), { timeoutMs });
		},
		// framesOf 取某类帧的全部实例（诊断用）。
		framesOf(type) {
			return frames.filter((f) => f.type === type);
		},
		reset() {
			frames.length = 0;
		},
		close() {
			conn.close();
		},
	};
}

// mkNode 走真实产品路径登记节点 + 签令牌（然后才连），返回节点事实与 mock 句柄。
async function mkNode(base, session, name) {
	const created = await H.req(base, "/v1/nodes", {
		session,
		body: V.envelope("Node", { name }, { namespaces: [NS] }),
	});
	V.expectStatus(`登记节点 ${name}`, created, 201);
	const id = created.json?.metadata?.uid;
	const tok = await H.req(base, `/v1/nodes/${encodeURIComponent(name)}/tokens`, { session, body: {} });
	V.expectStatus(`签发 ${name} 的令牌`, tok, 200);
	return { id, name, token: tok.json?.token || "" };
}

// helloAndWelcome 发 v4 hello 并等 welcome。
async function helloAndWelcome(base, node, { slots = 1, namespaces = [NS] } = {}) {
	const mock = await mkMock(base, node.name, node.token);
	mock.conn.send(N.nodeHello({ name: node.name, token: node.token, namespaces, slots }));
	const welcome = await mock.waitFrame("welcome", () => true, 5000);
	return { mock, welcome };
}

// closeExpect 连一条会话、发一帧、等连接被服务端关掉（返回关闭原因）。
async function closeExpect(base, node, send) {
	const mock = await mkMock(base, node.name, node.token);
	if (send) send(mock);
	const deadline = Date.now() + 5000;
	while (!mock.closed() && Date.now() < deadline) await sleep(30);
	// 收尾：无论是否已被服务端关掉，都主动断一次（避免残留会话影响后续派发判定）。
	mock.close();
	return mock.closed();
}

// ---------- 数据构造 ----------
async function mkCapability(base, session, name, script = "#!/bin/sh\nexit 0\n") {
	const r = await V.createCapability(
		base,
		session,
		NS,
		V.capabilityBody({
			name,
			run: { entrypoint: "run.sh", files: { "run.sh": script } },
			concurrency: { maxPerNode: 1 },
		}),
	);
	V.expectStatus(`建能力 ${name}`, r, 201);
	const doc = r.json || {};
	const digest = doc.status?.digest;
	H.check(`能力 ${name} 带 digest`, /^sha256:[0-9a-f]{64}$/.test(String(digest)), String(digest));
	return { name, version: doc.metadata?.version, digest };
}

async function mkSource(base, session, name, capability) {
	const r = await V.createSource(
		base,
		session,
		NS,
		V.sourceBody({ name, kind: "manual", enabled: true, targets: [{ capability }] }),
	);
	V.expectStatus(`建源 ${name}`, r, 201);
	// 返回**源名**（投递入口的 source 是 ns 内的源名，不是 id）。
	return r.json?.metadata?.name;
}

// emitTask 投递事件并返回任务 id（回执里的 tasks[0].taskId 一定是真任务）。
async function emitTask(base, sendKey, source, type) {
	const r = await V.ingestEvent(base, sendKey, NS, { source, type, id: randomUUID(), payload: {} });
	V.expectStatus(`投递事件 ${type}`, r, 200);
	const t = (r.json?.tasks || [])[0];
	H.check(`事件 ${type} 建出真任务`, !!t?.taskId, JSON.stringify(r.json).slice(0, 300));
	return t?.taskId;
}

async function getTask(base, session, id) {
	const r = await H.req(base, V.P.task(NS, id), { session });
	if (r.status !== 200) throw new Error(`GET task ${id}: ${r.status} ${String(r.text).slice(0, 300)}`);
	return r.json || {};
}

// deployAndAck 走 HTTP 部署动作 + 等帧 + 回 applied 回执（部署事实变 applied）。
async function deployAndAck(base, session, cap, mock) {
	const dep = await V.deployCapability(base, session, NS, cap.name, { nodes: [mock.name] });
	V.expectStatus(`部署 ${cap.name}`, dep, 200);
	const frame = await mock.waitFrame("capability.deploy", (f) => f.digest === cap.digest);
	mock.conn.send({
		type: "capability.deployed",
		requestId: frame.requestId,
		namespace: NS,
		name: cap.name,
		version: cap.version,
		digest: cap.digest,
		status: "applied",
	});
	await pollUntil("部署行 applied", async () => {
		const r = await H.req(base, V.P.deployments(NS), { session });
		const rows = r.json?.deployments || [];
		return rows.some((d) => d.capability === cap.name && d.state === "applied" && d.digest === cap.digest);
	});
	return frame;
}

// dispatchAcceptGrant 是「拿到派发 → accept → 拿 grant」的公共三步。
async function dispatchAcceptGrant(mock, slot = 0) {
	const dispatch = await mock.waitFrame("task.dispatch");
	mock.conn.send({
		type: "task.accept",
		taskId: dispatch.taskId,
		assignmentId: dispatch.assignmentId,
		proposedAttempt: dispatch.proposedAttempt,
		node: mock.name,
		slot,
	});
	const grant = await mock.waitFrame("lease.grant", (f) => f.taskId === dispatch.taskId);
	return { dispatch, grant };
}

// ---------- A. 握手 ----------
async function partHandshake(base, session, nsId) {
	console.log("--- A. 握手：v2 被拒 / 未知 token / 名字不符 / v4 welcome ---");
	// 1) 纯 v2 hello：v4 只认 hello.token，v2 的 node_id 不是凭据。
	{
		const conn = await N.wsConnect(base, null);
		const frames = [];
		let closed = null;
		conn.onFrame((f) => frames.push(f));
		conn.onClose((i) => (closed = i));
		conn.send({
			type: "hello",
			protocol: 2,
			node_id: "nd_e2e_v2_shape",
			name: "ws-v2-probe",
			applied: [],
			instances: [{ slot: 0, concurrency: 1 }],
		});
		const deadline = Date.now() + 5000;
		while (!closed && Date.now() < deadline) await sleep(30);
		conn.close();
		H.check("v2 hello 换不到 welcome（v4 没有兼容层）", !frames.some((f) => f.type === "welcome"), JSON.stringify(frames).slice(0, 200));
		H.eq("v2 hello 被关连接（1008 策略违规）", closed?.code, 1008);
		const list = await H.req(base, "/v1/nodes", { session });
		H.check(
			"v2 hello 没有登记出节点（不落地任何身份）",
			!(V.itemNames(list) || []).includes("ws-v2-probe"),
			JSON.stringify(V.itemNames(list)),
		);
	}
	// 2) 未知 token / 3) name 与 token 不一致：都必须在握手期被拒（1008）。
	{
		const node = await mkNode(base, session, "ws-auth-node");
		{
			const conn = await N.wsConnect(base, "e2eUnknownToken" + randomUUID().replace(/-/g, ""));
			let closed = null;
			conn.onClose((i) => (closed = i));
			conn.send({ type: "hello", name: "ws-auth-node", token: "e2eUnknownToken", namespaces: [NS] });
			const deadline = Date.now() + 5000;
			while (!closed && Date.now() < deadline) await sleep(30);
			conn.close();
			H.eq("未知 token 的 hello → 1008", closed?.code, 1008);
		}
		{
			// 名字与 token 不一致：token 是真的，但声称自己是另一台机器 → 1008。
			// （身份来自 token，不能由帧里的 name 选择。）
			const closed = await closeExpect(base, node, (mock) => {
				mock.conn.send({ ...N.nodeHello({ name: "ws-someone-else", token: node.token, namespaces: [NS] }), name: "ws-someone-else" });
			});
			H.eq("name 与 token 所属节点不一致 → 1008", closed?.code, 1008);
		}
		// 4) 正确 hello → v4 welcome
		const { mock, welcome } = await helloAndWelcome(base, node);
		H.check("v4 hello 换来 welcome", !!welcome, JSON.stringify(mock.frames).slice(0, 200));
		const keys = Object.keys(welcome || {});
		H.check(
			"welcome 是 v4 形状（configRevision/nodeVars/nodeLimits/namespaces）",
			["configRevision", "nodeVars", "nodeLimits", "namespaces"].every((k) => keys.includes(k)),
			JSON.stringify(keys),
		);
		H.check(
			"welcome 没有 v2 的退役键（protocol / vars / limits / applied）",
			!["protocol", "vars", "limits", "applied"].some((k) => keys.includes(k)),
			JSON.stringify(keys),
		);
		H.check("welcome.namespaces 是授权交集（含 default）", Array.isArray(welcome.namespaces) && welcome.namespaces.includes(NS), JSON.stringify(welcome.namespaces));
		H.check("configRevision 是十进制字符串（不是 revision 对象）", /^\d+$/.test(String(welcome.configRevision)), String(welcome.configRevision));
		mock.close();
	}
}

// ---------- B. 配置子协议 ----------
async function partConfig(base, session, node) {
	console.log("--- B. 配置：config.get / config.applied / 漂移可见 ---");
	const { mock, welcome } = await helloAndWelcome(base, node);

	// config.get 复用 welcome 同形（D-I2）：节点主动拉默认值只有这一条路。
	mock.conn.send({ type: "config.get" });
	const second = await pollUntil("config.get 的回应（第二条 welcome）", async () => mock.framesOf("welcome")[1], { timeoutMs: 4000 });
	H.eq("config.get 的回应也是 welcome", second.type, "welcome");
	H.eq("两条 welcome 的 configRevision 一致（同一份配置）", second.configRevision, welcome.configRevision);

	// config.applied 带正确 hash → applied 事实落库（Node.status 可读）。
	const goodHash = machineConfigHash();
	mock.conn.send({ type: "config.applied", revision: welcome.configRevision, hash: goodHash, instances: [{ slot: 0, busy: false }] });
	// 注意：Node.status 的这两个 revision 是**字符串**（wire 上是十进制文本，与 welcome 同形），
	// 不要拿 Number() 去比 —— 类型混用会让「看起来没生效」变成假红。
	const applied = await pollUntil("Node.status.appliedRevision 落库", async () => {
		const r = await H.req(base, `/v1/nodes/${encodeURIComponent(node.name)}`, { session });
		return r.json?.status?.appliedRevision === String(welcome.configRevision) ? r.json : null;
	});
	H.eq("appliedRevision == configRevision（配置已对齐）", applied.status.appliedRevision, String(welcome.configRevision));
	H.eq("appliedHash 就是节点算出来的内容哈希", applied.status.appliedHash, goodHash);
	H.check("槽位事实落库（instances 非空）", Array.isArray(applied.status.instances) && applied.status.instances.length === 1, JSON.stringify(applied.status.instances));

	// hash 漂移：节点报了另一个内容 hash → applied 事实可见地不一致（不是静默忽略）。
	const driftHash = "sha256:" + "1".repeat(64);
	mock.conn.send({ type: "config.applied", revision: welcome.configRevision, hash: driftHash });
	const drift = await pollUntil("漂移后的 appliedHash 可见", async () => {
		const r = await H.req(base, `/v1/nodes/${encodeURIComponent(node.name)}`, { session });
		return r.json?.status?.appliedHash === driftHash ? r.json : null;
	});
	H.eq("漂移：appliedHash 变成节点报的值（中枢不覆盖节点事实）", drift.status.appliedHash, driftHash);
	mock.conn.send({ type: "config.applied", revision: welcome.configRevision, hash: goodHash });

	// 坏 revision 是**协议违例**：帧结构对了但取值不可能成立 → 关连接（1008）。
	const closed = await closeExpect(base, node, (m) => {
		m.conn.send({ type: "hello", name: node.name, token: node.token, namespaces: [NS], localLimits: { total: 1 }, instances: [{ slot: 0, busy: false }] });
		m.conn.send({ type: "config.applied", revision: "not-a-number", hash: goodHash });
	});
	H.eq("config.applied.revision 不是非负整数 → 1008", closed?.code, 1008);
	mock.close();
}

// ---------- C/D/E/F. 部署 / 派发 / 授权 / 结果绑定 ----------
async function partExecution(base, session, seeded, node, cap, capSource) {
	console.log("--- C. 部署帧：只带 refs ---");
	const { mock, welcome } = await helloAndWelcome(base, node);
	mock.conn.send({ type: "config.applied", revision: welcome.configRevision, hash: machineConfigHash() });
	await sleep(100);

	const deployFrame = await deployAndAck(base, session, cap, mock);
	H.eq("capability.deploy 的 ns 是 slug", deployFrame.namespace, NS);
	H.eq("capability.deploy 的版本", deployFrame.version, cap.version);
	H.eq("capability.deploy 的 digest", deployFrame.digest, cap.digest);
	H.check(
		"capability.deploy **不带包体**（1 MiB 软上限的通道不搬脚本）",
		!Object.keys(deployFrame).some((k) => ["artifact", "files", "run", "spec"].includes(k)),
		JSON.stringify(Object.keys(deployFrame)),
	);
	H.check("回执按 requestId 配对（dep_ 前缀）", /^dep_/.test(String(deployFrame.requestId)), String(deployFrame.requestId));

	console.log("--- D. 派发：task.dispatch 不授执行权 ---");
	mock.reset();
	const taskId = await emitTask(base, seeded.sendKey, capSource, "ws.dispatch");
	const dispatch = await mock.waitFrame("task.dispatch", (f) => f.taskId === taskId);
	H.check("dispatch 带 assignmentId（关联 id，不是执行授权）", typeof dispatch.assignmentId === "string" && dispatch.assignmentId.length > 0, JSON.stringify(dispatch));
	H.eq("proposedAttempt 从 1 起（grant 生效才算数）", dispatch.proposedAttempt, 1);
	H.check(
		"dispatch **不含** leaseId / attemptToken（派发不是授权）",
		dispatch.leaseId === undefined && dispatch.attemptToken === undefined,
		JSON.stringify(Object.keys(dispatch)),
	);
	H.eq("dispatch 的 configRevision 与 welcome 同源", dispatch.configRevision, String(welcome.configRevision));
	H.eq("dispatch 钉住能力名", dispatch.capability?.name, cap.name);
	H.eq("dispatch 钉住 digest（部署身份）", dispatch.capability?.digest, cap.digest);
	H.eq("dispatch 的 ns 是 slug", dispatch.namespace, NS);
	H.check(
		"dispatch 带 assignment 窗口（未来时刻）",
		!!dispatch.assignmentExpiresAt && Date.parse(dispatch.assignmentExpiresAt) > Date.now(),
		String(dispatch.assignmentExpiresAt),
	);
	// 派发时任务还是 pending、attempt=0：授权还没有发生。
	{
		const t = await getTask(base, session, taskId);
		H.eq("刚派发的任务是 pending（accept 之前不授权）", t.status?.phase, "pending");
		H.eq("刚派发的任务 attempt=0", t.status?.attempt, 0);
	}

	console.log("--- E. accept → lease.grant → renew → lease.ack ---");
	mock.conn.send({
		type: "task.accept",
		taskId,
		assignmentId: dispatch.assignmentId,
		proposedAttempt: dispatch.proposedAttempt,
		node: node.name,
		slot: 0,
	});
	const grant = await mock.waitFrame("lease.grant", (f) => f.taskId === taskId);
	H.eq("grant 的 attempt=1（accept + grant 生效才计数）", grant.attempt, 1);
	H.check("grant 带 leaseId", typeof grant.leaseId === "string" && grant.leaseId.length > 0, String(grant.leaseId));
	H.check("grant 带 attemptToken（脚本里的 ANOTIFY_TASK_TOKEN）", typeof grant.attemptToken === "string" && grant.attemptToken.length >= 16, `len=${grant.attemptToken?.length}`);
	H.eq("grant 回带 assignmentId（节点据此配对 reservation）", grant.assignmentId, dispatch.assignmentId);
	H.eq("grant 回带节点在 accept 时预留的 slot", grant.slot, 0);
	H.check("grant 带过期时刻", !!grant.expiresAt && Date.parse(grant.expiresAt) > Date.now(), String(grant.expiresAt));
	{
		const t = await getTask(base, session, taskId);
		H.eq("grant 之后任务进入 running", t.status?.phase, "running");
		H.eq("running 的任务 attempt=1", t.status?.attempt, 1);
		H.eq("running 的任务绑在这台节点上（wire 名字）", t.status?.nodeName, node.name);
		H.check("running 的任务有租约到期时刻", !!t.status?.leaseExpiresAt, String(t.status?.leaseExpiresAt));
	}

	mock.conn.send({ type: "lease.renew", taskId, attempt: grant.attempt, leaseId: grant.leaseId });
	const leaseAck = await mock.waitFrame("lease.ack", (f) => f.taskId === taskId);
	H.eq("lease.ack 的 attempt 与 grant 一致", leaseAck.attempt, 1);
	H.eq("lease.ack 的 leaseId 与 grant 一致", leaseAck.leaseId, grant.leaseId);

	// 错 lease 的续租：不回 ack（节点按最后一次 ACK + 本地 deadline 自行停止）。
	mock.reset();
	mock.conn.send({ type: "lease.renew", taskId, attempt: grant.attempt, leaseId: "lse_bogus" });
	await sleep(400);
	H.eq("错 lease 的 renew 不回 lease.ack（不伪造续租成功）", mock.framesOf("lease.ack").length, 0);
	H.check("错 lease 的 renew 也不断连（业务拒绝 ≠ 协议违例）", mock.closed() === null, JSON.stringify(mock.closed()));

	console.log("--- F. 结果绑定：错 attempt / 错 lease / 取消在飞都不 ACK ---");
	const resultBase = {
		type: "task.result",
		taskId,
		node: node.name,
		leaseId: grant.leaseId,
		phase: "succeeded",
		exitCode: 0,
		result: { summary: "ws_protocol ok", artifacts: [] },
		startedAt: new Date().toISOString(),
		finishedAt: new Date().toISOString(),
	};
	mock.reset();
	mock.conn.send({ ...resultBase, attempt: 99 });
	mock.conn.send({ ...resultBase, attempt: grant.attempt, leaseId: "lse_bogus" });
	await sleep(500);
	H.eq("错 attempt / 错 lease 的 result 一条 ack 都不回", mock.framesOf("result.ack").length, 0);
	{
		const t = await getTask(base, session, taskId);
		H.eq("被拒的 result 没有改动任务相位（仍 running）", t.status?.phase, "running");
		H.check("被拒的 result 没有写 summary", (t.status?.result?.summary || "") === "", String(t.status?.result?.summary));
	}

	mock.conn.send({ ...resultBase, attempt: grant.attempt });
	const ack = await mock.waitFrame("result.ack", (f) => f.taskId === taskId);
	H.eq("正确绑定的 result → accepted=true", ack.accepted, true);
	H.eq("ack 回带 attempt", ack.attempt, grant.attempt);
	const done = await getTask(base, session, taskId);
	H.eq("任务终态 succeeded", done.status?.phase, "succeeded");
	H.eq("summary 是节点显式上报的那句", done.status?.result?.summary, "ws_protocol ok");
	H.eq("成功结果没有失败原因", done.status?.result?.error?.reason ?? "", "");
	H.check("任务有 startedAt/finishedAt", !!done.status?.startedAt && !!done.status?.finishedAt, `${done.status?.startedAt} → ${done.status?.finishedAt}`);
	return { mock, taskId, attempt: grant.attempt };
}

// ---------- E2. task.reject（pre-grant 的两个合法理由） ----------
//
// reject 只允许两个理由（A3 封闭集合）：stale_config / slot_unavailable。两者的语义
// 都是「本机这次接不了，但任务没失败」——不计 attempt、不授 lease、不占槽，回 pending
// 并带上有界退避。把它们当成「失败」会让一次配置抖动烧掉一整条 trace。
async function partReject(base, session, seeded, mock, capSource) {
	console.log("--- E2. task.reject：pre-grant 的两个合法理由都不算失败 ---");
	for (const [reason, wantWaiting] of [
		["stale_config", "waiting_for_config"],
		["slot_unavailable", "waiting_for_slot"],
	]) {
		mock.reset();
		const taskId = await emitTask(base, seeded.sendKey, capSource, `ws.reject.${reason}`);
		const dispatch = await mock.waitFrame("task.dispatch", (f) => f.taskId === taskId);
		mock.conn.send({
			type: "task.reject",
			taskId,
			assignmentId: dispatch.assignmentId,
			proposedAttempt: dispatch.proposedAttempt,
			node: mock.name,
			reason,
		});
		// 回 pending 是异步的（同一事务里改相位），轮询到目标状态。
		const back = await pollUntil(`${reason} 之后任务回 pending`, async () => {
			const t = await getTask(base, session, taskId);
			return t.status?.phase === "pending" && t.status?.waitingReason === wantWaiting ? t : null;
		});
		H.eq(`${reason} → 回 pending（不是 failed）`, back.status?.phase, "pending");
		H.eq(`${reason} → waitingReason=${wantWaiting}`, back.status?.waitingReason, wantWaiting);
		H.eq(`${reason} → 不计 attempt（reject 不是执行）`, back.status?.attempt, 0);
		H.check(`${reason} → 任务没有绑定节点（没被授权）`, !back.status?.nodeName, String(back.status?.nodeName));
		H.eq(`${reason} → 中枢没有发 lease.grant`, mock.framesOf("lease.grant").length, 0);
	}
}

// ---------- G. 日志 ----------
async function partLogs(base, session, seeded, mock, taskId, attempt, pendingTaskId) {
	console.log("--- G. 日志：logs_request/logs_response 与 410 语义 ---");
	mock.reset();
	// 中枢向节点要日志：帧里只有「哪条任务的哪次请求」，不搬 params。
	const logsP = H.req(base, `${V.P.taskLogs(NS, taskId)}?tailLines=50`, { session });
	const req = await mock.waitFrame("logs_request", (f) => f.taskId === taskId);
	H.check("logs_request 带 requestId（同一连接可能多条在飞，配对不能靠猜）", typeof req.requestId === "string" && req.requestId.length > 0, JSON.stringify(req));
	H.eq("logs_request 的 attempt 是真实执行的那一次", req.attempt, attempt);
	H.eq("logs_request 透传 tailLines", req.tailLines, 50);
	H.check("logs_request 不带任务 params（内部输入快照不外传）", req.params === undefined, JSON.stringify(Object.keys(req)));

	mock.conn.send({
		type: "logs_response",
		taskId,
		requestId: req.requestId,
		attempt,
		stdout: "ws stdout",
		stderr: "ws stderr",
		truncated: false,
		bytes: 18,
		lines: [
			{ sequence: 1, stream: "stdout", text: "ws stdout", at: new Date().toISOString() },
			{ sequence: 2, stream: "stderr", text: "ws stderr", at: new Date().toISOString() },
		],
	});
	const logs = await logsP;
	V.expectStatus("GET …/tasks/{id}/logs → 200", logs, 200);
	H.eq("日志 attempt 就是那次执行", logs.json?.attempt, attempt);
	H.check("stdout / stderr 分到不同 stream", (logs.json?.lines || []).some((l) => l.stream === "stdout" && l.text === "ws stdout") && (logs.json?.lines || []).some((l) => l.stream === "stderr" && l.text === "ws stderr"), JSON.stringify(logs.json?.lines));
	H.check("每行带节点采集序号", (logs.json?.lines || []).every((l) => Number.isInteger(l.sequence)), JSON.stringify(logs.json?.lines?.map((l) => l.sequence)));

	// 节点说「这台机器上没有那次执行的档案」→ 410（不能伪装成「这次什么都没打印」）。
	{
		const p = H.req(base, V.P.taskLogs(NS, taskId), { session });
		const req2 = await pollUntil("第二条 logs_request", async () => mock.framesOf("logs_request").find((f) => f.requestId !== req.requestId), { timeoutMs: 4000 });
		mock.conn.send({
			type: "logs_response",
			taskId,
			requestId: req2.requestId,
			stdout: "",
			stderr: "",
			truncated: false,
			bytes: 0,
			error: "no_local_archive",
		});
		const r = await p;
		V.expectReason("节点回 no_local_archive → 410（缺档案不是空成功）", r, 410, "no_local_archive");
	}

	// 还没有任何 attempt 的任务（pending）：中枢没有档案可问 → 410，不发帧。
	mock.reset();
	{
		const r = await H.req(base, V.P.taskLogs(NS, pendingTaskId), { session });
		V.expectReason("pending 任务（没有 attempt）读日志 → 410 no_local_archive", r, 410, "no_local_archive");
		H.eq("没有 attempt 时中枢不会向节点发 logs_request", mock.framesOf("logs_request").length, 0);
	}
}

// ---------- H. 取消 ----------
async function partCancel(base, session, seeded, mock, capSource, pendingSource) {
	console.log("--- H. 取消：历史绑定 / 错绑定不清槽 / 槽位真释放 ---");
	mock.reset();
	const taskId = await emitTask(base, seeded.sendKey, capSource, "ws.cancel");
	const { grant } = await dispatchAcceptGrant(mock);

	const cancelResp = await H.req(base, V.P.taskCancel(NS, taskId), { session, body: {} });
	V.expectStatus("POST …/tasks/{id}/cancel → 200", cancelResp, 200);
	const cancel = await mock.waitFrame("task.cancel", (f) => f.taskId === taskId);
	H.eq("task.cancel 带 attempt（历史绑定，不能按 taskId 猜）", cancel.attempt, grant.attempt);
	H.eq("task.cancel 带 leaseId（同一代执行的 lease）", cancel.leaseId, grant.leaseId);
	H.eq("task.cancel 带 slot（历史绑定）", cancel.slot, 0);
	H.eq("缺省取消原因是 cancelled_by_user", cancel.reason, "cancelled_by_user");
	H.check("task.cancel 带 graceSec（TERM → KILL 的宽限）", Number.isInteger(cancel.graceSec) && cancel.graceSec > 0, String(cancel.graceSec));
	{
		const t = await getTask(base, session, taskId);
		H.eq("取消意图落库但任务仍在跑（终态由 cancel.ack 裁定）", t.status?.phase, "running");
	}

	// 取消在飞时到了 result：不得 ACK（结果由 cancel.ack 裁定，不能被一次 result 抢先定终态）。
	mock.reset();
	mock.conn.send({
		type: "task.result",
		taskId,
		attempt: grant.attempt,
		node: mock.name,
		leaseId: grant.leaseId,
		phase: "succeeded",
		exitCode: 0,
		result: { summary: "raced with cancel", artifacts: [] },
		startedAt: new Date().toISOString(),
		finishedAt: new Date().toISOString(),
	});
	await sleep(400);
	H.eq("取消在飞时的 result 不回 ack", mock.framesOf("result.ack").length, 0);

	// 错绑定的 cancel.ack：不清槽、不改相位（fail closed）。
	const bogus = { ...{ type: "cancel.ack", taskId, attempt: grant.attempt, slot: 0, processGroupReaped: true } };
	mock.conn.send({ ...bogus, leaseId: "lse_bogus" });
	await sleep(400);
	{
		const t = await getTask(base, session, taskId);
		H.eq("错 lease 的 cancel.ack 不改相位（仍 running）", t.status?.phase, "running");
	}
	// 正确绑定：进程组真回收 → 任务终态 + 槽位释放。
	mock.conn.send({ ...bogus, leaseId: grant.leaseId });
	const cancelled = await pollUntil("任务进入 cancelled", async () => {
		const t = await getTask(base, session, taskId);
		return t.status?.phase === "cancelled" ? t : null;
	});
	H.eq("取消原因落在终态结果里", cancelled.status?.result?.error?.reason, "cancelled_by_user");
	const freed = await pollUntil("槽位回到空闲（cancel.ack 是唯一依据）", async () => {
		const r = await H.req(base, `/v1/nodes/${encodeURIComponent(mock.name)}`, { session });
		const instances = r.json?.status?.instances || [];
		return instances.length > 0 && instances.every((i) => i.busy === false) ? instances : null;
	});
	H.check("cancel.ack 之后没有槽位仍在占用", freed.every((i) => i.busy === false), JSON.stringify(freed));

	// pending 任务取消：没有执行者，直接终态；不计 attempt；重复取消 409。
	{
		const pendingId = await emitTask(base, seeded.sendKey, pendingSource, "ws.cancel.pending");
		const t0 = await getTask(base, session, pendingId);
		H.eq("没有部署记录的任务停在 pending", t0.status?.phase, "pending");
		H.eq("pending 任务 attempt=0", t0.status?.attempt, 0);
		const r = await H.req(base, V.P.taskCancel(NS, pendingId), { session, body: {} });
		V.expectStatus("取消 pending 任务 → 200", r, 200);
		H.eq("pending 取消直接终态（无需等节点）", r.json?.status?.phase, "cancelled");
		H.eq("pending 取消不计 attempt", r.json?.status?.attempt, 0);
		V.expectReason(
			"对已终态任务再取消 → 409（不是 5xx）",
			await H.req(base, V.P.taskCancel(NS, pendingId), { session, body: {} }),
			409,
			"Conflict",
		);
	}
}

// ---------- I. 帧边界 ----------
async function partFrameBoundaries(base, session, node) {
	console.log("--- I. 帧边界：未知帧不断连 / 坏 JSON 1003 / 方向错误 1008 ---");
	// 未知帧：Decode 的既定语义是「一条老/新帧不该打断整条连接」。
	{
		const { mock } = await helloAndWelcome(base, node);
		mock.reset();
		mock.conn.send({ type: "totally.unknown.frame", foo: 1 });
		await sleep(300);
		H.check("未知帧没有断连", mock.closed() === null, JSON.stringify(mock.closed()));
		mock.conn.send({ type: "config.get" });
		const w = await pollUntil("未知帧之后 config.get 仍工作", async () => mock.framesOf("welcome")[0], { timeoutMs: 4000 }).catch(() => null);
		H.check("未知帧之后连接仍可用（config.get 换到 welcome）", !!w, JSON.stringify(mock.frames.map((f) => f.type)));
		mock.close();
	}
	// 坏 JSON 分两种时机，答案不同（都是真事实，各自钉住）：
	//   ① 握手期（首帧就不是 JSON）：连 hello 都没解出来 → 1008 handshake failed；
	//   ② 已认证会话里：读循环判 bad_frame → 1003（UnsupportedData）。
	{
		const closed = await closeExpect(base, node, (m) => m.conn.sendRaw("{not-json"));
		H.eq("首帧坏 JSON（握手期）→ 1008 handshake failed", closed?.code, 1008);
	}
	{
		const closed = await closeExpect(base, node, (m) => {
			m.conn.send(N.nodeHello({ name: node.name, token: node.token, namespaces: [NS] }));
			m.conn.sendRaw("{not-json");
		});
		H.eq("已认证会话里的坏 JSON → 1003 bad_frame", closed?.code, 1003);
	}
	// 方向错误：节点发了只该由中枢下发的帧（task.dispatch）→ 1008。
	{
		const closed = await closeExpect(base, node, (m) => m.conn.send({ type: "task.dispatch", taskId: "x" }));
		H.eq("节点发下行帧 → 1008（方向错误是协议违例）", closed?.code, 1008);
	}
	// accept 形状不合法（缺 slot / slot 负数 / node 不是自己）→ 1008。
	for (const [label, frame] of [
		["accept 缺 slot", { type: "task.accept", taskId: "t", assignmentId: "a", proposedAttempt: 1, node: node.name }],
		["accept slot 为负", { type: "task.accept", taskId: "t", assignmentId: "a", proposedAttempt: 1, node: node.name, slot: -1 }],
		["accept node 不是本会话节点", { type: "task.accept", taskId: "t", assignmentId: "a", proposedAttempt: 1, node: "someone-else", slot: 0 }],
		["reject reason 不在封闭集合", { type: "task.reject", taskId: "t", assignmentId: "a", proposedAttempt: 1, node: node.name, reason: "because" }],
	]) {
		// 先发 hello（服务端按序处理：先认证、再谈形状），紧跟着发违规帧。
		const closed = await closeExpect(base, node, (m) => {
			m.conn.send(N.nodeHello({ name: node.name, token: node.token, namespaces: [NS] }));
			m.conn.send(frame);
		});
		H.eq(`${label} → 1008`, closed?.code, 1008);
	}
	// 超大帧：1 MiB 软上限（FrameSoftLimit）——超过时连接必须被断开，而不是被读进内存。
	{
		const closed = await closeExpect(base, node, (m) => {
			m.conn.send(N.nodeHello({ name: node.name, token: node.token, namespaces: [NS] }));
			m.conn.send({ type: "unknown.big.frame", blob: "x".repeat(1200 * 1024) });
		});
		H.check(
			"超大帧让连接被断开（1 MiB 软上限生效）",
			!!closed,
			`close=${JSON.stringify(closed)}`,
		);
	}
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log("=== SUITE: ws_protocol（v4 帧级协议）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	const mocks = [];
	try {
		const seeded = H.seed(server.dbPath, "wsproto");
		const session = seeded.session;
		const nsId = await H.defaultNamespaceId(server.base, session);
		H.check("拿到默认命名空间 id", !!nsId, String(nsId));

		// A. 握手（自建节点，独立于后续用例）
		await partHandshake(server.base, session, nsId);

		// B. 配置（同一台节点：config.applied 的 applied 事实要落在它身上）
		const cfgNode = await mkNode(server.base, session, "ws-config-node");
		await partConfig(server.base, session, cfgNode);

		// C~F. 部署 → 派发 → 授权 → 结果（一台节点、一份能力、一份源）
		const execNode = await mkNode(server.base, session, "ws-exec-node");
		const cap = await mkCapability(server.base, session, "ws.exec-cap");
		const capSource = await mkSource(server.base, session, "ws-exec-src", cap.name);
		// 未部署的能力 + 源：用于 pending 取消与「没有 attempt 的日志」。
		const pendingCap = await mkCapability(server.base, session, "ws.pending-cap");
		const pendingSource = await mkSource(server.base, session, "ws-pending-src", pendingCap.name);

		const { mock, taskId, attempt } = await partExecution(server.base, session, seeded, execNode, cap, capSource);
		mocks.push(mock);

		// 一个 pending 任务（不部署 → 不会派发）供日志 410 断言用。
		const pendingTaskId = await emitTask(server.base, seeded.sendKey, pendingSource, "ws.logs.pending");

		await partLogs(server.base, session, seeded, mock, taskId, attempt, pendingTaskId);
		await partReject(server.base, session, seeded, mock, capSource);
		await partCancel(server.base, session, seeded, mock, capSource, pendingSource);

		// I. 帧边界（用独立节点，避免影响前面的会话）
		const edgeNode = await mkNode(server.base, session, "ws-edge-node");
		await partFrameBoundaries(server.base, session, edgeNode);
	} finally {
		for (const m of mocks) {
			try {
				m.close();
			} catch {
				/* 已关闭 */
			}
		}
		server.stop();
	}

	const passed = H.summary(SUITE);
	process.exitCode = passed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e && e.stack ? e.stack : e).slice(0, 800));
	H.summary(SUITE);
	process.exit(1);
});
