#!/usr/bin/env node
/* SUITE: routing — 派发四步 + 未触发原因（v4：源 targets → 校参 → 候选节点 → 派发）
 *
 * v4 把 v3 的「边表」换成了**资源本身**（api-conventions §6 / A5）：
 *
 *   入口边 = `source.spec.targets`（左端是源名，右端是能力）
 *   能力边 = `capability.spec.next`（下游引用）
 *   `GET …/edges` 只是这两者的**派生只读投影**，没有写入口。
 *
 * 所以本套件不再「建边」，而是写资源；每条 v3 用例的 v4 对应物如下（**没有静默删除**）：
 *
 *   v3 用例                                 v4 用例
 *   ───────────────────────────────────────  ────────────────────────────────────────────
 *   ① 上游没回报（不是剪枝）                  任务留 pending 且回执 pruned=[]（容量/无人两处都钉）
 *   ② no_edge                                源没有 targets → prune `no_target`；
 *                                            引用解析不到的出口 → 建源被拒（精确 400）
 *   ③ param_schema          v4 拆成 `param_missing`（缺必填）与 `param_type`（类型不符）两条
 *   ④ no_node               能力+源都在但没有节点持有该 digest → 任务照建、pending、不剪枝
 *   ⑤ foreign_user          v4 没有「按用户路由」：同一条意图 = 「源不属于本 ns → fire 404」
 *   ⑥ path（环剪枝）         v4 没有 path；下游非法目标由 emit 侧 `target_not_declared` 拒
 *   A5 出口封闭（port）       v4 没有 port；闭合性 = `spec.next` 逐条解析出 digest
 *   P0.3 边字面量            v4 = `source.spec.targets[].params` 模板（常量 + `$(payload.…)`）
 *   P0.2 源 resultSchema     v4 = `source.spec.config.payloadSchema` → prune `event_schema`
 *
 * 做法：配置（能力/源）走真实 HTTP API；节点用共享 MockNode（lib/nodes.mjs，两段式 v4 握手
 * + 部署回执；真产物/真脚本由 node_v4 覆盖）；不断言 sleep 时序（一律轮询 + 上限）。
 *
 * 已修复 #352：未包装的普通 error 改为 `ErrSourceTargetInvalid` 哨兵 → 400
 * `invalid_source_config`；本套件因此把断言收紧为 **精确 400 + reason**（不接受 5xx 蒙混）。
 * ==============================================================================
 */
import { createHmac } from "node:crypto";
import { createServer } from "node:http";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";
import * as N from "../lib/nodes.mjs";

let server, session, sendKey, nsId, NS;
const nodes = []; // 本套件起的 mock 节点（退出前统一关连接）

// ---------- 节点：共享 MockNode（两段式握手 + 部署回执）----------
// 帧形状的唯一镜像在 lib/nodes.mjs（MockNode）；这里只做套件侧的编排：起节点 → 触发部署 → 等 deployment 收敛成 applied。
async function connectNode(name, { slots = 2, declared = [] } = {}) {
	const node = await N.MockNode.connect({
		base: server.base,
		dbPath: server.dbPath,
		namespaceId: nsId,
		namespaceSlug: NS,
		name,
		caps: declared,
		slots,
	});
	nodes.push(node);
	return node;
}

// deployToNode 触发**真实**人工部署，并让 mock 节点按 v4 帧协议回执 applied（不是造
// deployment 行：中枢发 capability.deploy、节点回 capability.deployed 才收敛；真节点还会先
// GET /v1/node/capabilities/{digest} 校验字节 —— 那段由 node_v4 覆盖）。
async function deployToNode(node, capName) {
	const res = await H.req(server.base, V.P.capabilityDeploy(NS, capName), {
		session,
		method: "POST",
		body: { nodes: [node.id] },
	});
	H.eq(`部署 ${capName} → 200`, res.status, 200);
	const frame = await node.deployFrame(capName);
	H.check(
		`节点收到 ${capName} 的 capability.deploy 帧`,
		!!frame,
		JSON.stringify(node.frames.map((f) => f.type)),
	);
	const applied = await V.tryPoll(
		async () => {
			const rows = (await H.req(server.base, V.P.deployments(NS), { session })).json?.deployments || [];
			const row = rows.find((d) => d.capability === capName && d.nodeId === node.id);
			return row && row.state === "applied" ? row : null;
		},
		{ timeoutMs: 5000 },
	);
	H.check(
		`deployment ${capName} 收敛成 applied（节点回执驱动）`,
		!!applied,
		JSON.stringify(frame).slice(0, 200),
	);
	return { res, applied };
}

// dispatchFrames 取某条任务收到的派发帧（节点不 accept，所以会随 sweep 重复下发）。
function dispatchFrames(node, taskId) {
	return node.frames.filter((f) => f.type === "task.dispatch" && f.taskId === taskId);
}

async function waitDispatch(node, taskId, timeoutMs = 5000) {
	return N.waitForFrame(node.frames, (f) => f.type === "task.dispatch" && f.taskId === taskId, timeoutMs);
}

// expectPrune 断言回执里的剪枝原因（v4 的 pruned[] 与 v3 同形，但原因码是封闭集合）。
function expectPrune(name, receipt, reason) {
	const pruned = receipt.json?.pruned || [];
	const hit = pruned.find((p) => p.reason === reason);
	H.check(name, !!hit, `want reason=${reason}, got ${JSON.stringify(pruned)}`);
	return hit;
}

// postEvent 走**机器入口**：Bearer API Key（notify:send）+ ns 路径。
function postEvent(body) {
	return H.req(server.base, V.P.events(NS), { key: sendKey, body });
}

// fireSource 走人工触发（会话 + ns 路径），回执与投递入口同一份形状（source.md §5.2）。
function fireSource(name, body = {}) {
	return H.req(server.base, V.P.sourceFire(NS, name), { session, method: "POST", body });
}

async function defineCap(name, opts = {}) {
	const r = await V.createCapability(server.base, session, NS, V.capabilityBody({ name, ...opts }));
	if (r.status !== 201) throw new Error(`定义能力 ${name} 失败: ${r.status} ${String(r.text).slice(0, 300)}`);
	return r.json;
}

// capRef 把能力信封折成 hello.capabilities[] 的一条（name+version+digest）。v4 申报的是
// 「这台机器持有哪一版」，不是整份信封（直接塞信封会报 caps[0].name undefined）。
function capRef(doc) {
	return { name: doc?.metadata?.name, version: doc?.metadata?.version, digest: doc?.status?.digest };
}

// ---------- #513 webhook 信封：投递侧 helper（mkHookSource 返回含 token 的 hookUrl）----------
async function mkHookSource(name, { config = {}, targets = [] } = {}) {
	const r = await V.createSource(server.base, session, NS, V.sourceBody({ name, kind: "webhook", config, targets }));
	V.expectStatus(`建 webhook 源 ${name} → 201`, r, 201);
	const hookUrl = r.json?.spec?.config?.hookUrl || "";
	H.check(`webhook 源 ${name} 的读投影带 hookUrl`, /^\/v1\/hooks\//.test(hookUrl), hookUrl);
	return { doc: r.json, hookUrl };
}

// deliver 投递一次 webhook（body 是 string 时按**原始字节**发出，签名对着它算）。
function deliver(hookUrl, headers, body) {
	return H.req(server.base, hookUrl, { method: "POST", headers, body });
}

// hmacHex = 契约 §4 的口径：HMAC-SHA256(secret, 原始字节) 的小写 hex。
function hmacHex(secret, raw) {
	return createHmac("sha256", secret).update(raw).digest("hex");
}

async function taskParamsOf(taskId) {
	return (await H.req(server.base, V.P.task(NS, taskId), { session })).json?.spec?.params;
}

// eventOf 按 eventId 在 trace 里找事件行（入站事件的 type 只在这里看得见）。
async function eventOf(traceId, eventId) {
	const trace = (await H.req(server.base, V.P.trace(NS, traceId), { session })).json;
	return (trace?.events || []).find((e) => e?.metadata?.uid === eventId) || null;
}

async function main() {
	H.startTimer();
	server = await H.startServer({ suiteName: "routing" });
	const s = H.seed(server.dbPath, "routing");
	session = s.session;
	sendKey = s.sendKey;

	const ns = await H.defaultNamespace(server.base, session);
	H.check("拿到默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
	if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
	NS = ns.slug;
	nsId = ns.id;

	// ---------- 0. 起点是源而不是能力（#110）----------
	console.log("--- 0. 起点是源 / 能力边来自 next ---");
	const initialCaps = V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session }));
	H.check(
		"起点不再是能力（#110）",
		!(initialCaps || []).includes("github") && !(initialCaps || []).includes("cron"),
		JSON.stringify(initialCaps),
	);

	// ---------- 1. 建图：能力 next（派生能力边）+ 源 targets（派生入口边）----------
	const review = await defineCap("review", {
		params: {
			type: "object",
			required: ["number"],
			properties: { number: { type: "number" } },
		},
		next: ["fix1", "fix2"],
	});
	const fix1 = await defineCap("fix1");
	const fix2 = await defineCap("fix2");

	const github = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "github",
			kind: "manual",
			targets: [{ capability: "review", params: { number: "$(payload.number)" } }],
		}),
	);
	V.expectStatus("建源 github（targets=[review]）→ 201", github, 201);

	// 派生投影：两类边都在，且没有写入口（写入口 405 由 resource_capability 显式注册）
	const edges = await H.req(server.base, V.P.edges(NS), { session });
	V.expectStatus("GET …/edges → 200", edges, 200);
	const edgeList = Array.isArray(edges.json?.items) ? edges.json.items : null;
	H.check("edges 是 {items:[…]} 的只读投影", !!edgeList, JSON.stringify(edges.json).slice(0, 200));
	const hasEdge = (from, to, kind) =>
		(edgeList || []).some((e) => e.from === from && e.to === to && e.kind === kind);
	H.check("能力边 review→fix1 来自 spec.next", hasEdge("review", "fix1", "capability"), JSON.stringify(edgeList));
	H.check("能力边 review→fix2 来自 spec.next", hasEdge("review", "fix2", "capability"), JSON.stringify(edgeList));
	H.check("入口边 github→review 来自 source.targets", hasEdge("github", "review", "source"), JSON.stringify(edgeList));
	V.expectReason(
		"POST …/edges → 405（v4 没有边写入口）",
		await H.req(server.base, V.P.edges(NS), {
			session,
			method: "POST",
			body: { from: "a", to: "b" },
		}),
		405,
		"MethodNotAllowed",
	);

	// ---------- 2. 派发四步 ----------
	console.log("--- 2. 派发四步：targets → 校参 → 候选节点 → 派发帧 ---");
	const routingNode = await connectNode("routing-node", { slots: 4, declared: [capRef(review)] });
	await deployToNode(routingNode, "review");

	const ev1 = await postEvent({
		source: "github",
		type: "pull_request.opened",
		payload: { number: 42 },
	});
	V.expectStatus("事件入队 200", ev1, 200);
	H.eq("首轮建 1 个任务", (ev1.json?.tasks || []).length, 1);
	H.eq("首轮任务能力是 review", ev1.json?.tasks?.[0]?.capability, "review");
	H.eq("任务钉住能力版本 1", ev1.json?.tasks?.[0]?.version, 1);
	H.eq("无剪枝", (ev1.json?.pruned || []).length, 0);
	const task1 = ev1.json?.tasks?.[0]?.taskId;

	const disp = await waitDispatch(routingNode, task1);
	H.check(
		"节点收到 task.dispatch 帧",
		!!disp,
		JSON.stringify(routingNode.frames.map((f) => f.type)),
	);
	if (disp) {
		H.eq("派发帧的能力名", disp.capability?.name, "review");
		H.eq("派发帧钉住能力版本", disp.capability?.version, 1);
		H.check(
			"派发帧带 digest（部署身份，不是名字猜的）",
			String(disp.capability?.digest || "").startsWith("sha256:"),
			JSON.stringify(disp.capability),
		);
		H.eq("派发帧带渲染后的 params", disp.params?.number, 42);
		H.eq("派发帧的命名空间就是任务的 ns", disp.namespace, NS);
		H.eq("派发帧提议第 1 次执行", disp.proposedAttempt, 1);
		H.check("派发帧带 assignmentId", String(disp.assignmentId || "").length > 0, JSON.stringify(disp).slice(0, 200));
		H.check("派发帧带 configRevision", String(disp.configRevision || "").length > 0, String(disp.configRevision));
		H.check("派发帧带 assignmentExpiresAt", !!disp.assignmentExpiresAt, String(disp.assignmentExpiresAt));
		// A2：没有 argv / shell（不变量 I1）
		const raw = JSON.stringify(disp).toLowerCase();
		H.check(
			"派发帧不含 argv/shell/command",
			!raw.includes("argv") && !raw.includes("shell") && !raw.includes("command"),
			raw.slice(0, 160),
		);
	}

	// 任务信封：挑到了哪台 + 还没授执行权（v4：派发 ≠ lease）
	await V.tryPoll(async () => {
		const r = await H.req(server.base, V.P.task(NS, task1), { session });
		return r.json?.status?.nodeName ? r : null;
	}, { timeoutMs: 4000 });
	const task1Doc = (await H.req(server.base, V.P.task(NS, task1), { session })).json;
	V.assertEnvelope("任务信封", task1Doc, "Task");
	H.eq("任务仍是 pending（等节点 accept）", task1Doc?.status?.phase, "pending");
	H.eq("任务记录被挑中的节点", task1Doc?.status?.nodeName, "routing-node");
	H.eq("派发不授执行权（attempt=0，等 lease.grant）", task1Doc?.status?.attempt, 0);
	H.eq("任务 spec 钉住能力名", task1Doc?.spec?.capability?.name, "review");
	H.eq("任务 spec 钉住版本", task1Doc?.spec?.capability?.version, 1);
	H.eq("任务 spec.paramsn 是渲染后的值", task1Doc?.spec?.params?.number, 42);
	H.eq("任务绑定的 trace 就是事件的 trace", task1Doc?.spec?.traceUID, ev1.json?.traceId);
	// 稳定提案（R1-C2）：重复 sweep 不 churn —— 同一任务的所有派发帧共用同一个 assignmentId。
	const laterFrames = dispatchFrames(routingNode, task1);
	H.check("派发会重复 sweep（同一任务多次下发）", laterFrames.length >= 1, `n=${laterFrames.length}`);
	H.check(
		"重复 sweep 不换提案（assignmentId 稳定）",
		laterFrames.every((f) => f.assignmentId === laterFrames[0].assignmentId),
		JSON.stringify(laterFrames.map((f) => f.assignmentId)),
	);

	// ---------- 3. A6 扇出（源的一行 targets 多条 → 多个任务）----------
	console.log("--- 3. 扇出（source.spec.targets 多条）---");
	const t1 = await defineCap("fan.target1");
	const t2 = await defineCap("fan.target2");
	const fanSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "fanout.demo",
			kind: "manual",
			targets: [{ capability: "fan.target1" }, { capability: "fan.target2" }],
		}),
	);
	V.expectStatus("建扇出源 fanout.demo（2 个 targets）→ 201", fanSrc, 201);
	const fanNode = await connectNode("fanout-node", { slots: 2, declared: [capRef(t1), capRef(t2)] });
	await deployToNode(fanNode, "fan.target1");
	await deployToNode(fanNode, "fan.target2");

	const fan = await fireSource("fanout.demo");
	V.expectStatus("扇出事件 200", fan, 200);
	H.eq("一个源两条 targets → 2 个任务", (fan.json?.tasks || []).length, 2);
	H.eq(
		"扇出的两个能力正确",
		(fan.json?.tasks || []).map((t) => t.capability).sort(),
		["fan.target1", "fan.target2"],
	);
	H.eq("扇出不产生剪枝", (fan.json?.pruned || []).length, 0);
	for (const t of fan.json?.tasks || []) {
		const f = await waitDispatch(fanNode, t.taskId);
		H.check(`扇出任务 ${t.capability} 也真的派下去了`, !!f, JSON.stringify(fanNode.frames.map((x) => x.type)));
	}

	// ---------- 4. 未触发原因（封闭集合逐个钉）----------
	console.log("--- 4. 未触发原因 ---");
	// 4.1 param_missing：目标声明了能力参数却没写映射（v4：模板取不到 → param_missing）
	const noMap = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "s.nomap", kind: "manual", targets: [{ capability: "review" }] }),
	);
	V.expectStatus("建源 s.nomap（target 不映射 number）→ 201", noMap, 201);
	const missing = await fireSource("s.nomap", { payload: { number: 5 } });
	H.eq("param_missing 不建任务", (missing.json?.tasks || []).length, 0);
	const pm = expectPrune("param_missing 留痕", missing, "param_missing");
	H.check("param_missing 证据点名缺哪个参数", String(pm?.detail || "").includes("number"), String(pm?.detail));
	H.eq("param_missing 剪枝行写明是哪个能力", pm?.capability, "review");

	// 4.2 param_type：类型不符是**另一个**原因码（v4 不把两种问题混成一个）
	const badType = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "s.badtype",
			kind: "manual",
			targets: [{ capability: "review", params: { number: "$(payload.number)" } }],
		}),
	);
	V.expectStatus("建源 s.badtype → 201", badType, 201);
	const typed = await fireSource("s.badtype", { payload: { number: "not-a-number" } });
	H.eq("param_type 不建任务", (typed.json?.tasks || []).length, 0);
	const pt = expectPrune("param_type 留痕", typed, "param_type");
	H.check("param_type 证据点名参数与两边的类型", /number/.test(String(pt?.detail || "")) && /string/.test(String(pt?.detail || "")), String(pt?.detail));

	// 4.3 no_target：源没有 targets 是**合法配置**（先把入口建起来），事件照收
	const noTarget = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "s.notarget", kind: "manual", targets: [] }),
	);
	V.expectStatus("建无 targets 的源 → 201", noTarget, 201);
	const nt = await fireSource("s.notarget");
	V.expectStatus("无 targets 的源 fire → 200（事件照收）", nt, 200);
	H.check("no_target 仍返回合法 eventId", !!nt.json?.eventId, V.brief(nt));
	H.eq("no_target 不建任务", (nt.json?.tasks || []).length, 0);
	expectPrune("no_target 留痕", nt, "no_target");

	const brokenSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "broken.demo", kind: "manual", targets: [{ capability: "does.not.exist" }] }),
	);
	H.check(
		"引用了不存在的出口能力 → 建源 400 invalid_source_config",
		brokenSrc.status === 400 && brokenSrc.json?.reason === "invalid_source_config",
		`${brokenSrc.status} ${String(brokenSrc.text).slice(0, 160)}`,
	);
	H.check(
		"拒绝消息点名那个出口能力",
		String(brokenSrc.text || "").includes("does.not.exist"),
		String(brokenSrc.text).slice(0, 160),
	);

	// 4.5 no_node：能力与源都在，但没有节点持有这份 digest（未部署）→ 任务照建、不剪枝
	const orphanCap = await defineCap("orphan.cap");
	const orphanSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "nonode.demo", kind: "manual", targets: [{ capability: "orphan.cap" }] }),
	);
	V.expectStatus("建源 nonode.demo → 201", orphanSrc, 201);
	const orphanEv = await fireSource("nonode.demo");
	V.expectStatus("无人可跑的源 fire → 200", orphanEv, 200);
	H.eq("no_node 仍然建任务（任务先排队，不是丢弃）", (orphanEv.json?.tasks || []).length, 1);
	H.eq("no_node 不产生剪枝", (orphanEv.json?.pruned || []).length, 0);
	const orphanTask = (await H.req(server.base, V.P.task(NS, orphanEv.json?.tasks?.[0]?.taskId), { session })).json;
	H.eq("no_node 的任务停在 pending", orphanTask?.status?.phase, "pending");
	H.eq("no_node 的任务没有被挑中任何节点", orphanTask?.status?.nodeName, null);
	H.eq("no_node 的任务没有被授权（attempt=0）", orphanTask?.status?.attempt, 0);

	// 4.6 源不存在 / 跨 ns 的 fire → 404（授权边界，不区分「不存在」与「不属于你」）
	V.expectReason(
		"fire 不存在的源 → 404",
		await H.req(server.base, V.P.sourceFire(NS, "s.does_not_exist"), { session, method: "POST", body: {} }),
		404,
		"NotFound",
	);

	// ---------- 5. P0.2 入口校验：config.payloadSchema → prune(event_schema) ----------
	//
	// 与派发期的 param_* 是**两个层级**（source.md §6）：
	//   param_*        逐条 target 判，其他 target 照常派发
	//   event_schema   整个事件判，一条 target 都不走（但**事件照常落库**）
	// 所以它只能挂在**源**上，且必须用「真源 + fire」测（投递入口不经过源配置）。
	console.log("--- 5. 入口校验：payloadSchema → event_schema ---");
	const schemaCap = await defineCap("schema.target");
	const schemaNode = await connectNode("schema-node", { slots: 2, declared: [capRef(schemaCap)] });
	await deployToNode(schemaNode, "schema.target");
	const RESULT_SCHEMA = {
		type: "object",
		required: ["title"],
		properties: { title: { type: "string" } },
	};
	const schemaSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "schema.src",
			kind: "manual",
			config: { payloadSchema: RESULT_SCHEMA },
			targets: [{ capability: "schema.target" }],
		}),
	);
	V.expectStatus("建带 payloadSchema 的源 → 201", schemaSrc, 201);
	H.eq(
		"payloadSchema 原样存下（required 读得回来）",
		schemaSrc.json?.spec?.config?.payloadSchema?.required,
		["title"],
	);

	// ① 不合规 payload：零任务 + prune(event_schema)，但**事件仍然落库**
	const badFire = await fireSource("schema.src", { payload: { nope: 1 } });
	H.eq("① 不合规 payload：事件照收 200（400 会让外部系统把事件丢在地上）", badFire.status, 200);
	H.eq("① 不合规 payload → 零任务", (badFire.json?.tasks || []).length, 0);
	const esPrune = expectPrune("① 不合规 payload → prune(event_schema)", badFire, "event_schema");
	H.check(
		"① 剪枝证据点名缺哪个字段（不是一句「不合规」）",
		/title/.test(String(esPrune?.detail || "")),
		String(esPrune?.detail || ""),
	);
	// 事件必须查得到：入口校验只决定「派不派」，不能把外部世界给的事实弄丢
	const badTrace = badFire.json?.traceId;
	const badTraceDoc = await H.req(server.base, V.P.trace(NS, badTrace), { session });
	V.expectStatus("① 事件仍然落库（trace 里查得到）", badTraceDoc, 200);
	const badEvents = badTraceDoc.json?.events || [];
	H.check(
		"① trace 里就是那条事件（id 对得上）",
		badEvents.some((e) => e?.metadata?.uid === badFire.json?.eventId),
		JSON.stringify(badEvents.map((e) => e?.metadata?.uid)),
	);
	H.check(
		"① 同一条 trace 里有 event_schema 的剪枝事件行（留痕可查）",
		badEvents.some((e) => e?.spec?.eventType === "event_schema" && e?.spec?.involvedObject?.name === "route"),
		JSON.stringify(badEvents.map((e) => [e?.spec?.eventType, e?.spec?.involvedObject?.name])),
	);
	H.eq("① 剪枝不建任务（trace 里没有任务）", (badTraceDoc.json?.tasks || []).length, 0);

	// ② 合规 payload：正常派发（证明上面那条零任务真的是「不合规」而不是「源坏了」）
	const okFire = await fireSource("schema.src", { payload: { title: "空指针" } });
	V.expectStatus("② 合规 payload：200", okFire, 200);
	H.eq("② 合规 payload → 正常建 1 个任务", (okFire.json?.tasks || []).length, 1);
	H.eq("② 任务落在源声明的出口上", okFire.json?.tasks?.[0]?.capability, "schema.target");
	H.eq("② 合规 payload 不产生任何剪枝", (okFire.json?.pruned || []).length, 0);

	// ③ 不可用的 payloadSchema：#612 起写入期就 400（与运行期同一份判据）；存量坏行仍 fail-closed（下面改库造）。
	const unusableSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "s.unusable",
			kind: "manual",
			config: { payloadSchema: { type: "nonsense" } },
			targets: [{ capability: "schema.target" }],
		}),
	);
	V.expectStatus("③ 不可用 schema 建源 → 400（#612：写入期与运行期同一判据）", unusableSrc, 400);
	H.eq("③ reason 是 invalid_source_config", V.reasonOf(unusableSrc), "invalid_source_config");
	const unusableCauses = ((unusableSrc.json || {}).details || {}).causes || [];
	H.eq(
		"③ causes 精确指到 spec.config.payloadSchema.type",
		unusableCauses.map((c) => c?.field).join(","),
		"spec.config.payloadSchema.type",
	);
	V.expectStatus( // 升级前写入的存量行（写入期不再收）：改库造，验运行期仍旧 fail-closed 而不是放行
		"③ 先建一份合法 schema 的源 → 201（用来造存量坏行）",
		await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({
				name: "s.unusable",
				kind: "manual",
				config: { payloadSchema: { type: "object", required: ["a"] } },
				targets: [{ capability: "schema.target" }],
			}),
		),
		201,
	);
	V.withDB(server.dbPath, (db) => {
		const row = db
			.prepare(
				`SELECT s.id AS id, s.config AS config FROM sources s JOIN namespaces n ON n.id = s.namespace_id
				WHERE n.slug = ? AND s.name = ?`,
			)
			.get(NS, "s.unusable");
		if (!row) throw new Error("③ 存量坏行造数失败：s.unusable 不存在");
		const config = JSON.parse(row.config);
		config.payloadSchema = { type: "nonsense" };
		db.prepare("UPDATE sources SET config = ? WHERE id = ?").run(JSON.stringify(config), row.id);
	});
	const unusableFire = await fireSource("s.unusable", { payload: { a: 1 } });
	H.eq("③ 不可用 schema 时事件照收 200", unusableFire.status, 200);
	H.eq("③ fail-closed：零任务", (unusableFire.json?.tasks || []).length, 0);
	const unusablePrune = expectPrune("③ 不可用 schema → event_schema", unusableFire, "event_schema");
	H.check(
		"③ 证据说明 schema 不可用（不是「不合规」）",
		/schema/.test(String(unusablePrune?.detail || "")),
		String(unusablePrune?.detail || ""),
	);

	// ④ 未声明 payloadSchema 的源：行为与加这个字段之前完全一致（不做入口校验）
	const legacySrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "schema.legacy", kind: "manual", targets: [{ capability: "schema.target" }] }),
	);
	V.expectStatus("④ 建未声明 payloadSchema 的源 → 201", legacySrc, 201);
	const legacyFire = await fireSource("schema.legacy", { payload: { nope: 1 } });
	H.eq("④ 老源：同一份 payload 照常派发（未声明 = 不校验）", (legacyFire.json?.tasks || []).length, 1);
	H.eq("④ 老源：不产生 event_schema 剪枝", (legacyFire.json?.pruned || []).length, 0);

	// ⑤ 写期硬拒非法配置（v4 的封闭配置面）：坏模板根名 / 未知 config 键
	const badRoot = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "s.badroot",
			kind: "manual",
			targets: [{ capability: "schema.target", params: { title: "$(vars.x)" } }],
		}),
	);
	V.expectReason(
		"参数模板引用 $(vars.…) → 400（那个根名只在能力 workDir 里有意义）",
		badRoot,
		400,
		"invalid_source_config",
	);
	V.expectReason(
		"未知 config 键（payloadScheme）→ 400（拼错的键不能静默保留）",
		await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({ name: "s.badfield", kind: "manual", config: { payloadScheme: {} } }),
		),
		400,
		"invalid_source_config",
	);

	// ---------- 6. 容量不足不剪枝（有节点、有部署，但槽位全满）----------
	console.log("--- 6. 容量不足不剪枝 ---");
	const busyCap = await defineCap("busy.target");
	await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "busy.demo", kind: "manual", targets: [{ capability: "busy.target" }] }),
	);
	// 节点声明了能力、也部署了，但 physical slots = 0（本机没有空槽）
	const busyNode = await connectNode("busy-node", { slots: 0, declared: [capRef(busyCap)] });
	await deployToNode(busyNode, "busy.target");
	const busy = await fireSource("busy.demo");
	V.expectStatus("空槽位事件 200", busy, 200);
	H.eq("空槽位仍建任务", (busy.json?.tasks || []).length, 1);
	H.eq("空槽位不产生剪枝", (busy.json?.pruned || []).length, 0);
	const busyTask = (await H.req(server.base, V.P.task(NS, busy.json?.tasks?.[0]?.taskId), { session })).json;
	H.eq("空槽位任务留 pending", busyTask?.status?.phase, "pending");
	H.eq("空槽位任务没有被挑中节点", busyTask?.status?.nodeName, null);
	H.eq("空槽位任务没有被授权（attempt=0）", busyTask?.status?.attempt, 0);
	H.eq(
		"空槽位时没有派发帧（不是「发了但没人接」）",
		dispatchFrames(busyNode, busy.json?.tasks?.[0]?.taskId).length,
		0,
	);

	// ---------- 7. 幂等（同一投递 id 不重复派发）----------
	console.log("--- 7. 幂等 ---");
	const idemId = "routing-idem-" + Date.now();
	const i1 = await postEvent({ source: "github", type: "pull_request.opened", id: idemId, payload: { number: 1 } });
	const i2 = await postEvent({ source: "github", type: "pull_request.opened", id: idemId, payload: { number: 1 } });
	H.eq("幂等：第一次 duplicate=false", i1.json?.duplicate, false);
	H.eq("幂等：第二次 duplicate=true", i2.json?.duplicate, true);
	H.eq("幂等：同一 eventId", i2.json?.eventId, i1.json?.eventId);
	H.eq("幂等：同一 traceId", i2.json?.traceId, i1.json?.traceId);
	// 幂等回执回的是**原事件**的派发结果（duplicate=true 时客户端仍要能看到任务），
	// 所以「不重复派发」的判据不是 tasks=[]，而是**同一批 taskId**（同一行，不是新建一行）。
	H.eq("幂等：第二次回的是同一批任务（没有新建任务行）", (i2.json?.tasks || []).length, 1);
	H.eq(
		"幂等：taskId 与第一次逐字相同（没有重复派发）",
		i2.json?.tasks?.[0]?.taskId,
		i1.json?.tasks?.[0]?.taskId,
	);
	const idemTrace = await H.req(server.base, V.P.trace(NS, i1.json?.traceId), { session });
	H.eq("幂等：trace 里只有 1 个任务（事件没有派发两次）", (idemTrace.json?.tasks || []).length, 1);
	H.eq("幂等：trace 里只有 1 条事件", (idemTrace.json?.events || []).length, 1);

	// ---------- 8. A10 命名空间隔离（v4：ns 进路径）----------
	console.log("--- 8. A10 命名空间隔离 ---");
	// ① 两个命名空间各建同名能力 → 各自列表里只有自己那条（同名不同定义）
	const nsB = "iso" + Date.now().toString(36);
	const created = await V.createNamespace(server.base, session, nsB, "隔离验证");
	V.expectStatus("A10 建第二个命名空间 → 201", created, 201);
	const sameB = await V.createCapability(
		server.base,
		session,
		nsB,
		V.capabilityBody({ name: "review", params: review.spec?.params }),
	);
	V.expectStatus("A10 在第二个命名空间建同名能力 review → 201", sameB, 201);
	H.check(
		"A10 同名能力是两个不同定义（uid 不同）",
		sameB.json?.metadata?.uid !== review.metadata?.uid,
		`${sameB.json?.metadata?.uid} vs ${review.metadata?.uid}`,
	);
	const listA = V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session }));
	const listB = V.itemNames(await H.req(server.base, V.P.capabilities(nsB), { session }));
	H.check("A10 A 空间列表可读", Array.isArray(listA), JSON.stringify(listA));
	H.check("A10 A 空间有 review", (listA || []).includes("review"), JSON.stringify(listA));
	H.check(
		"A10 A 空间看不到只在 B 里的能力",
		!(listA || []).includes("iso.only.b"),
		JSON.stringify(listA),
	);
	const bOnly = await V.createCapability(
		server.base,
		session,
		nsB,
		V.capabilityBody({ name: "iso.only.b" }),
	);
	V.expectStatus("A10 在 B 空间建 iso.only.b → 201", bOnly, 201);
	H.check(
		"A10 A 空间仍然看不到 B 空间的能力",
		!(V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session })) || []).includes("iso.only.b"),
		JSON.stringify(V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session }))),
	);
	H.check(
		"A10 B 空间看不到 A 空间独有的能力",
		!(listB || []).includes("fan.target1"),
		JSON.stringify(listB),
	);

	// ② 跨 ns 读 → 404（与「不存在」同形，不泄漏存在性）
	const crossRead = await H.req(server.base, V.P.capability(nsB, "fan.target1"), { session });
	const ghostCap = await H.req(server.base, V.P.capability(nsB, "nope.nope"), { session });
	H.check(
		"A10 跨 ns 读与不存在都是 404",
		crossRead.status === 404 && ghostCap.status === 404,
		`cross=${crossRead.status} ghost=${ghostCap.status}`,
	);
	H.eq("A10 两种 404 的 body 逐字节相同（防存在性探测）", crossRead.text, ghostCap.text);

	// ③ 事件不跨命名空间：A 的 trace 在 B 里查不到（404），在 A 里能查全
	const traceB = await H.req(server.base, V.P.trace(nsB, ev1.json?.traceId), { session });
	V.expectReason("A10 B 空间查不到 A 空间的事件", traceB, 404, "NotFound");
	const traceA = await H.req(server.base, V.P.trace(NS, ev1.json?.traceId), { session });
	V.expectStatus("A10 A 空间查得到自己的 trace", traceA, 200);
	H.check(
		"A10 trace 里既有事件也有任务（同一条链）",
		(traceA.json?.events || []).length >= 1 && (traceA.json?.tasks || []).length >= 1,
		JSON.stringify({ e: (traceA.json?.events || []).length, t: (traceA.json?.tasks || []).length }),
	);

	// ④ 未知命名空间 → 404，且不泄漏任何能力名
	const ghostNs = await H.req(server.base, V.P.capabilities("ns_bogus_xxxxxxxx"), { session });
	V.expectReason("A10 未知命名空间读 → 404", ghostNs, 404, "NotFound");
	H.check("A10 404 不泄漏数据", !(ghostNs.text || "").includes("review"), String(ghostNs.text).slice(0, 120));

	// ---------- 9. A5 出口闭合（v4：spec.next 逐条解析出 digest + 内容不可变）----------
	console.log("--- 9. A5 出口闭合 / next 解析 ---");
	const reviewDoc = (await H.req(server.base, V.P.capability(NS, "review"), { session })).json;
	const resolved = reviewDoc?.status?.resolved?.next || [];
	H.eq("next 两条都被解析（扇出的副作用面）", resolved.length, 2);
	H.check(
		"每条解析结果都带 digest（不是只有名字）",
		resolved.every((r) => String(r?.digest || "").startsWith("sha256:")),
		JSON.stringify(resolved),
	);
	H.check(
		"解析结果没有一条 missing",
		resolved.every((r) => !r?.missing),
		JSON.stringify(resolved),
	);
	const downstream = (reviewDoc?.status?.conditions || []).find((c) => c?.type === "DownstreamResolved");
	H.eq("DownstreamResolved=True", downstream?.status, "True");
	H.eq("原因是 Valid（不是 NotFound）", downstream?.reason, "Valid");

	// PATCH 只接受 spec.next（v4：内容不可变，改内容 = 提交新版本）
	const fix3 = await defineCap("fix3");
	const patched = await H.req(server.base, V.P.capability(NS, "review"), {
		session,
		method: "PATCH",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { resourceVersion: reviewDoc?.metadata?.resourceVersion },
			spec: { next: ["fix1", "fix2", "fix3"] },
		},
	});
	V.expectStatus("PATCH spec.next → 200（图配置可变）", patched, 200);
	H.eq("PATCH 后 next 是 3 条", (patched.json?.spec?.next || []).length, 3);
	H.eq("PATCH 不改内容 identity（version 不变）", patched.json?.metadata?.version, 1);
	const edgesAfter = (await H.req(server.base, V.P.edges(NS), { session })).json?.items || [];
	H.check(
		"派生投影跟着 spec.next 变（fix3 出现）",
		edgesAfter.some((e) => e.from === "review" && e.to === "fix3" && e.kind === "capability"),
		JSON.stringify(edgesAfter.filter((e) => e.from === "review")),
	);
	H.eq("新增下游不影响能力身份（uid 不变）", patched.json?.metadata?.uid, reviewDoc?.metadata?.uid);
	// 其它 spec 键不可变：静默忽略会让人以为脚本改了、digest 却没变
	const immutable = await H.req(server.base, V.P.capability(NS, "review"), {
		session,
		method: "PATCH",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { resourceVersion: patched.json?.metadata?.resourceVersion },
			spec: { run: V.MIN_RUN },
		},
	});
	V.expectReason("PATCH spec.run → 400 immutable_field", immutable, 400, "immutable_field");
	H.check(
		"400 消息点名被拒的键（spec.run）",
		String(immutable.json?.message || "").includes("run"),
		String(immutable.json?.message || immutable.text).slice(0, 160),
	);

	// ---------- 10. P0.3 参数模板：字面量优先（v4 的「边字面量」）----------
	console.log("--- 10. 参数模板：常量字面量 ⊎ payload 引用 ---");
	const litCap = await defineCap("lit.target", {
		params: {
			type: "object",
			properties: { env: { type: "string" }, to: { type: "string" } },
		},
	});
	const litNode = await connectNode("lit-node", { slots: 2, declared: [capRef(litCap)] });
	await deployToNode(litNode, "lit.target");
	const litSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "lit.demo",
			kind: "manual",
			// env 是**常量**（配置里写死），to 来自载荷
			targets: [{ capability: "lit.target", params: { env: "prod", to: "$(payload.to)" } }],
		}),
	);
	V.expectStatus("建带常量字面量的源 → 201", litSrc, 201);
	const litEv = await postEvent({
		source: "lit.demo",
		type: "go",
		payload: { env: "dev", to: "u_1" },
	});
	V.expectStatus("字面量用例：事件 200", litEv, 200);
	H.eq("字面量用例：建出 1 个任务", (litEv.json?.tasks || []).length, 1);
	H.eq("字面量用例：无剪枝", (litEv.json?.pruned || []).length, 0);
	const litTask = (
		await H.req(server.base, V.P.task(NS, litEv.json?.tasks?.[0]?.taskId), { session })
	).json;
	H.eq("常量赢：payload 里 env=dev，任务 params 里是 prod", litTask?.spec?.params?.env, "prod");
	H.eq("引用取到值：to 来自 payload", litTask?.spec?.params?.to, "u_1");

	// ---------- 11. #513 A1：webhook 信封 / 签名 scheme / match.types ----------
	console.log("--- 11. webhook 信封 / 签名 scheme / match.types ---");

	// 11.1 老契约对照（§8）：不写 envelope/signature/match 的源行为逐字不变
	await defineCap("wh.legacy", { params: { type: "object", properties: { env: { type: "string" } } } });
	const legacyHook = await mkHookSource("wh.legacy", {
		targets: [{ capability: "wh.legacy", params: { env: "$(payload.env)" } }],
	});
	const legacy = await deliver(legacyHook.hookUrl, {}, { type: "t-old", id: "old-1", payload: { env: "prod" } });
	V.expectStatus("老契约：无 envelope / 无签名头的投递 → 200", legacy, 200);
	H.eq("老契约：回执 duplicate=false（首次投递）", legacy.json?.duplicate, false);
	H.eq("老契约：建 1 个任务", (legacy.json?.tasks || []).length, 1);
	H.eq(
		"老契约：任务参数 env=prod（body.payload 仍是载荷）",
		(await taskParamsOf(legacy.json?.tasks?.[0]?.taskId))?.env,
		"prod",
	);
	H.eq(
		"老契约：事件行 type 恰为 t-old（body.type 仍是类型来源）",
		(await eventOf(legacy.json?.traceId, legacy.json?.eventId))?.spec?.eventType,
		"t-old",
	);

	// 11.2 GitHub issues.opened：type = 头值 + joinFrom，载荷 = 整个投递体
	await defineCap("wh.issue", {
		params: { type: "object", properties: { event: { type: "string" }, issueTitle: { type: "string" } } },
	});
	const ghHook = await mkHookSource("wh.github", {
		config: {
			envelope: {
				type: { header: "x-github-event", joinFrom: "$.action" },
				deliveryId: { header: "x-github-delivery" },
				payload: { whole: true },
			},
		},
		targets: [
			{
				capability: "wh.issue",
				params: { event: "$(payload.action)", issueTitle: "$(payload.issue.title)" },
				match: { types: ["issues.opened"] },
			},
		],
	});
	const deliveryId = "gh-delivery-" + Date.now();
	const ghHeaders = { "X-GitHub-Event": "issues", "X-GitHub-Delivery": deliveryId };
	const ghBody = { action: "opened", issue: { title: "hello" } };
	const gh1 = await deliver(ghHook.hookUrl, ghHeaders, ghBody);
	V.expectStatus("GitHub issues.opened → 200", gh1, 200);
	H.eq("GitHub issues.opened 建 1 个任务", (gh1.json?.tasks || []).length, 1);
	const ghParams = await taskParamsOf(gh1.json?.tasks?.[0]?.taskId);
	H.eq("issues.opened：参数 event 来自 payload.action（whole 载荷）", ghParams?.event, "opened");
	H.eq("issues.opened：参数 issueTitle 来自 payload.issue.title（两层指针）", ghParams?.issueTitle, "hello");
	H.eq(
		"事件行 type 恰为 issues.opened（头值拼 joinFrom）",
		(await eventOf(gh1.json?.traceId, gh1.json?.eventId))?.spec?.eventType,
		"issues.opened",
	);

	// 11.3 不命中 match.types → no_match（issue.title 取不到也证明「先 match 后 params」）
	const ghPr = await deliver(
		ghHook.hookUrl,
		{ "X-GitHub-Event": "pull_request", "X-GitHub-Delivery": deliveryId + "-pr" },
		{ action: "opened" },
	);
	V.expectStatus("GitHub pull_request 不被命中 → 200（不是错误码）", ghPr, 200);
	H.eq("pull_request：0 任务（match 不命中不建任务）", (ghPr.json?.tasks || []).length, 0);
	H.eq("pull_request：回执 pruned[0].reason 恰为 no_match", ghPr.json?.pruned?.[0]?.reason, "no_match");
	H.check(
		"pull_request：剪枝证据带事件 type 与声明的 types（§5）",
		/pull_request\.opened/.test(String(ghPr.json?.pruned?.[0]?.detail || "")),
		JSON.stringify(ghPr.json?.pruned),
	);

	// 11.4 ping：payloadSchema 不合规也不该变成 400（否则外部系统会丢掉事件）
	const pingHook = await mkHookSource("wh.github-ping", {
		config: {
			envelope: {
				type: { header: "x-github-event", joinFrom: "$.action" },
				deliveryId: { header: "x-github-delivery" },
				payload: { whole: true },
			},
			payloadSchema: { type: "object", required: ["issue"], properties: { issue: { type: "object" } } },
		},
		targets: [{ capability: "wh.issue", match: { types: ["issues.opened"] } }],
	});
	const ping = await deliver(
		pingHook.hookUrl,
		{ "X-GitHub-Event": "ping", "X-GitHub-Delivery": "gh-ping-" + Date.now() },
		{ zen: "Keep it simple." },
	);
	V.expectStatus("GitHub ping → 200（不是 400：schema 不合规只剪枝）", ping, 200);
	H.eq("ping：0 任务", (ping.json?.tasks || []).length, 0);
	H.check(
		"ping：pruned 非空（event_schema 或 no_match 都算）",
		(ping.json?.pruned || []).length > 0,
		JSON.stringify(ping.json?.pruned),
	);

	// 11.5 幂等：同一个 X-GitHub-Delivery 再投一次（回执回原事件，不新建任务行）
	const gh2 = await deliver(ghHook.hookUrl, ghHeaders, ghBody);
	V.expectStatus("幂等：同一个 X-GitHub-Delivery 再投一次 → 200", gh2, 200);
	H.eq("幂等：第二次 duplicate=true", gh2.json?.duplicate, true);
	H.eq(
		"幂等：taskId 与首次逐字相同（没有重复派发）",
		gh2.json?.tasks?.[0]?.taskId,
		gh1.json?.tasks?.[0]?.taskId,
	);
	const ghTraceAfter = (await H.req(server.base, V.P.trace(NS, gh1.json?.traceId), { session })).json;
	H.eq("幂等：该 trace 的任务总数不变（仍是 1）", (ghTraceAfter?.tasks || []).length, 1);
	H.eq("幂等：该 trace 的事件总数不变（仍是 1）", (ghTraceAfter?.events || []).length, 1);

	// 11.6 github-sha256：X-Hub-Signature-256，对**实际发出的原始字节**算 HMAC
	const sigHook = await mkHookSource("wh.github-sig", {
		config: { secret: "s3cr3t", signature: { scheme: "github-sha256" } },
		targets: [{ capability: "wh.legacy", params: { env: "signed" } }],
	});
	const sigRaw = JSON.stringify({ type: "sig.test", id: "sig-1", payload: { x: 1 } });
	const sigHeader = { "X-Hub-Signature-256": "sha256=" + hmacHex("s3cr3t", sigRaw) };
	const sigOK = await deliver(sigHook.hookUrl, sigHeader, sigRaw);
	V.expectStatus("github-sha256：对原始字节签名 → 200", sigOK, 200);
	H.eq("github-sha256：签名正确时建 1 个任务", (sigOK.json?.tasks || []).length, 1);
	const tamperedRaw = sigRaw.replace('"x":1', '"x":2'); // 只改一个字节，签名头不变
	const sigBad = await deliver(sigHook.hookUrl, sigHeader, tamperedRaw);
	V.expectStatus("github-sha256：body 改一个字节（签名不变）→ 401", sigBad, 401);

	// 11.7 不填 scheme：旧头 X-Anotify-Signature 逐字仍是今天的口径；无头 → 401
	const oldSigHook = await mkHookSource("wh.anotify-sig", {
		config: { secret: "s3cr3t" },
		targets: [{ capability: "wh.legacy", params: { env: "legacy" } }],
	});
	const oldRaw = JSON.stringify({ type: "sig.old", id: "sig-old", payload: { y: 1 } });
	const oldOK = await deliver(oldSigHook.hookUrl, { "X-Anotify-Signature": "sha256=" + hmacHex("s3cr3t", oldRaw) }, oldRaw);
	V.expectStatus("不填 scheme：旧头 X-Anotify-Signature → 200", oldOK, 200);
	const oldMissing = await deliver(oldSigHook.hookUrl, {}, oldRaw);
	V.expectStatus("不填 scheme：不带签名头 → 401（secret 非空就必须签）", oldMissing, 401);

	// 11.8 config.payload 对 webhook 真生效：静态打底 + 入站覆盖（冲突时入站赢）
	await defineCap("wh.pay", {
		params: { type: "object", properties: { tenant: { type: "string" }, env: { type: "string" } } },
	});
	const payHook = await mkHookSource("wh.payload", {
		config: { payload: { tenant: "acme", env: "static" } },
		targets: [
			{ capability: "wh.pay", params: { tenant: "$(payload.tenant)", env: "$(payload.env)" } },
		],
	});
	const pay = await deliver(payHook.hookUrl, {}, { payload: { env: "inbound" } });
	V.expectStatus("webhook config.payload：投递 → 200", pay, 200);
	const payParams = await taskParamsOf(pay.json?.tasks?.[0]?.taskId);
	H.eq("静态打底：tenant 来自 config.payload", payParams?.tenant, "acme");
	H.eq("入站覆盖静态：env=inbound（不是 static）", payParams?.env, "inbound");

	// 11.9 payloadSchema 仍在：入口校验整条事件，0 任务 + event_schema 留痕
	const schHook = await mkHookSource("wh.schema", {
		config: {
			payloadSchema: { type: "object", required: ["environment"], properties: { environment: { type: "string" } } },
		},
		targets: [{ capability: "wh.pay", params: { env: "x" } }],
	});
	const sch = await deliver(schHook.hookUrl, {}, { payload: { x: 1 } });
	V.expectStatus("webhook payloadSchema 不合规 → 200（事件照收）", sch, 200);
	H.eq("payloadSchema 不合规：0 任务", (sch.json?.tasks || []).length, 0);
	H.eq("payloadSchema 不合规：pruned[0].reason 恰为 event_schema", sch.json?.pruned?.[0]?.reason, "event_schema");

	// 11.10 写入期 400：坏信封 / 坏签名 / 坏 match 一律当场拒（不静默保留）
	for (const [i, [label, config]] of [
		["envelope 出现未知键 typo", { envelope: { typo: 1 } }],
		["envelope.type 同时给 header 与 pointer", { envelope: { type: { header: "x-a", pointer: "$.b" } } }],
		["envelope.payload 同时给 whole 与 pointer", { envelope: { payload: { whole: true, pointer: "$.b" } } }],
		["signature.scheme 非法值 md5", { signature: { scheme: "md5" } }],
	].entries()) {
		V.expectReason(
			`${label} → 400`,
			await V.createSource(
				server.base,
				session,
				NS,
				V.sourceBody({ name: `wh.bad-config-${i}`, kind: "webhook", config, targets: [{ capability: "wh.legacy" }] }),
			),
			400,
			"invalid_source_config",
		);
	}
	for (const [i, [label, match, reason]] of [
		// 未知键在**写请求解码**层被拒（unknown_field），不是归一化层（invalid_source_config）
		["targets[0].match 出现未知键 type", { type: ["issues.opened"] }, "unknown_field"],
		["targets[0].match.types 是空数组", { types: [] }, "invalid_source_config"],
	].entries()) {
		V.expectReason(
			`${label} → 400`,
			await V.createSource(
				server.base,
				session,
				NS,
				V.sourceBody({ name: `wh.bad-match-${i}`, kind: "webhook", targets: [{ capability: "wh.legacy", match }] }),
			),
			400,
			reason,
		);
	}

	// 11.11 读投影不回退：凭据仍只写不读，但 envelope / signature / match 原样可见（§7）
	const sigCfg = (await H.req(server.base, V.P.source(NS, "wh.github-sig"), { session })).json?.spec?.config || {};
	H.check(
		"读投影没有 token / secret 键（凭据只写不读）",
		!("token" in sigCfg) && !("secret" in sigCfg),
		JSON.stringify(sigCfg),
	);
	H.eq("读投影 secretSet=true（掩码事实可见）", sigCfg.secretSet, true);
	H.check("读投影字节里没有 secret 原文", !JSON.stringify(sigCfg).includes("s3cr3t"), JSON.stringify(sigCfg));
	H.eq("读投影原样回显 signature.scheme", sigCfg.signature?.scheme, "github-sha256");
	const ghCfg = (await H.req(server.base, V.P.source(NS, "wh.github"), { session })).json;
	H.eq("读投影原样回显 envelope.type.header", ghCfg?.spec?.config?.envelope?.type?.header, "x-github-event");
	H.eq(
		"读投影原样回显 envelope.type.joinFrom（指针 $.action 与 action 等价）",
		String(ghCfg?.spec?.config?.envelope?.type?.joinFrom || "").replace(/^\$\./, ""),
		"action",
	);
	H.eq("读投影原样回显 match.types", ghCfg?.spec?.targets?.[0]?.match?.types, ["issues.opened"]);

	console.log("--- 12. #519 match 字段级组合子（all / any / negate / in / 数值 / 正则）---");
	const ghEnv = {
		type: { header: "x-github-event", joinFrom: "$.action" },
		deliveryId: { header: "x-github-delivery" },
		payload: { whole: true },
	};
	let hookSeq = 0;
	const ghHead = (event, tag) => ({
		"X-GitHub-Event": event,
		"X-GitHub-Delivery": `gh519-${tag}-${Date.now()}-${++hookSeq}`,
	});
	await defineCap("mc.issue", { params: { type: "object", properties: { tag: { type: "string" } } } });

	const prGate = await mkHookSource("mc.pr-gate", {
		config: { envelope: ghEnv },
		targets: [
			{
				capability: "mc.issue",
				params: { tag: "$(payload.action)" },
				match: {
					types: ["issues.labeled"],
					all: [{ field: "issue.pull_request", op: "exists", negate: true }],
				},
			},
		],
	});
	const prBody = {
		action: "labeled",
		issue: {
			number: 7,
			title: "t",
			pull_request: { url: "https://api.github.com/repos/o/r/pulls/7" },
			labels: [{ name: "bug" }],
		},
	};
	const asIssue = await deliver(prGate.hookUrl, ghHead("issues", "pr"), prBody);
	V.expectStatus("12.1 PR 当作 issue 投递 → 200（字段不符不是 400/500）", asIssue, 200);
	H.eq("12.1 PR-as-issue：0 任务", (asIssue.json?.tasks || []).length, 0);
	H.eq("12.1 PR-as-issue：pruned[0].reason 恰为 no_match", asIssue.json?.pruned?.[0]?.reason, "no_match");
	H.check(
		"12.1 PR-as-issue：detail 点名 match.all[0]",
		/match\.all\[0\]/.test(String(asIssue.json?.pruned?.[0]?.detail || "")),
		JSON.stringify(asIssue.json?.pruned),
	);
	const realIssue = await deliver(prGate.hookUrl, ghHead("issues", "issue"), {
		action: "labeled",
		issue: { number: 7, title: "t", labels: [{ name: "bug" }] },
	});
	V.expectStatus("12.1 同一 body 去掉 pull_request 再投 → 200", realIssue, 200);
	H.eq("12.1 真 issue（无 pull_request）：1 任务", (realIssue.json?.tasks || []).length, 1);

	const labelHook = await mkHookSource("mc.labels", {
		config: { envelope: ghEnv },
		targets: [
			{
				capability: "mc.issue",
				params: { tag: "$(payload.action)" },
				match: {
					types: ["issues.labeled"],
					all: [{ field: "issue.labels.0.name", op: "in", value: ["bug", "regression"] }],
				},
			},
		],
	});
	const labeled = (names) =>
		deliver(labelHook.hookUrl, ghHead("issues", "lbl"), {
			action: "labeled",
			issue: { number: 1, title: "t", labels: names.map((name) => ({ name })) },
		});
	const bugLbl = await labeled(["bug"]);
	V.expectStatus("12.2 labels[0].name=bug → 200", bugLbl, 200);
	H.eq("12.2 in 命中：1 任务", (bugLbl.json?.tasks || []).length, 1);
	const docsLbl = await labeled(["docs"]);
	H.eq("12.2 labels[0].name=docs：0 任务", (docsLbl.json?.tasks || []).length, 0);
	H.eq("12.2 labels[0].name=docs：no_match", docsLbl.json?.pruned?.[0]?.reason, "no_match");
	const laterLbl = await labeled(["p2", "regression", "bug"]);
	H.eq("12.2 regression 在第 2 位但下标 0 是 p2：0 任务", (laterLbl.json?.tasks || []).length, 0);
	H.eq("12.2 下标 0 不命中：no_match", laterLbl.json?.pruned?.[0]?.reason, "no_match");

	const assigneeHook = await mkHookSource("mc.assignee", {
		config: { envelope: ghEnv },
		targets: [
			{
				capability: "mc.issue",
				params: { tag: "$(payload.action)" },
				match: { all: [{ field: "issue.assignee.login", op: "==", value: "alice" }] },
			},
		],
	});
	const noAssignee = await deliver(assigneeHook.hookUrl, ghHead("issues", "asg"), {
		action: "opened",
		issue: { number: 1, title: "t" },
	});
	V.expectStatus("12.3 没有 assignee 字段 → 200（不是 500）", noAssignee, 200);
	H.eq("12.3 没有 assignee：0 任务", (noAssignee.json?.tasks || []).length, 0);
	H.eq("12.3 没有 assignee：no_match", noAssignee.json?.pruned?.[0]?.reason, "no_match");
	const withAssignee = await deliver(assigneeHook.hookUrl, ghHead("issues", "asg2"), {
		action: "opened",
		issue: { number: 1, title: "t", assignee: { login: "alice" } },
	});
	V.expectStatus("12.3 assignee.login=alice → 200", withAssignee, 200);
	H.eq("12.3 assignee 命中：1 任务", (withAssignee.json?.tasks || []).length, 1);

	const numericHook = await mkHookSource("mc.numeric", {
		config: { envelope: ghEnv },
		targets: [
			{
				capability: "mc.issue",
				params: { tag: "$(payload.issue.title)" },
				match: {
					all: [
						{ field: "issue.number", op: ">", value: 3 },
						{ field: "issue.title", op: "matches", value: "^URGENT" },
					],
				},
			},
		],
	});
	const num = (number, title) =>
		deliver(numericHook.hookUrl, ghHead("issues", "num"), { action: "opened", issue: { number, title } });
	const numHit = await num(5, "URGENT: disk full");
	V.expectStatus("12.4 number=5 且 title 以 URGENT 开头 → 200", numHit, 200);
	H.eq("12.4 数值与正则都命中：1 任务", (numHit.json?.tasks || []).length, 1);
	const numLow = await num(2, "URGENT: disk full");
	H.eq("12.4 number=2 不满足 >3：0 任务", (numLow.json?.tasks || []).length, 0);
	const numWip = await num(5, "WIP: disk full");
	H.eq("12.4 title 不匹配 ^URGENT：0 任务", (numWip.json?.tasks || []).length, 0);
	const badRe = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "mc.bad-regex",
			kind: "webhook",
			targets: [
				{ capability: "mc.issue", match: { all: [{ field: "issue.title", op: "matches", value: "(unclosed" }] } },
			],
		}),
	);
	V.expectStatus("12.4 matches 的正则编不过 → 400", badRe, 400);
	H.eq("12.4 坏正则：reason=invalid_source_config", badRe.json?.reason, "invalid_source_config");
	H.eq(
		"12.4 坏正则：causes[0].field 精确到 all[0].value",
		badRe.json?.details?.causes?.[0]?.field,
		"spec.targets[0].match.all[0].value",
	);

	const andHook = await mkHookSource("mc.and", {
		config: { envelope: ghEnv },
		targets: [
			{
				capability: "mc.issue",
				params: { tag: "$(payload.action)" },
				match: {
					types: ["issues.labeled"],
					all: [{ field: "issue.pull_request", op: "exists", negate: true }],
					any: [{ field: "issue.labels.0.name", op: "in", value: ["bug"] }],
				},
			},
		],
	});
	const anyOnly = await deliver(andHook.hookUrl, ghHead("issues", "and1"), {
		action: "labeled",
		issue: { number: 1, title: "t", pull_request: { url: "u" }, labels: [{ name: "bug" }] },
	});
	H.eq("12.5 只有 any 命中（all 不成立）：0 任务", (anyOnly.json?.tasks || []).length, 0);
	H.eq("12.5 只有 any 命中：no_match", anyOnly.json?.pruned?.[0]?.reason, "no_match");
	const typesNo = await deliver(andHook.hookUrl, ghHead("pull_request", "and2"), {
		action: "opened",
		issue: { number: 1, title: "t", labels: [{ name: "bug" }] },
	});
	H.eq("12.5 类型不符（pull_request.opened）但组合子成立：0 任务", (typesNo.json?.tasks || []).length, 0);
	H.eq("12.5 types 与组合子是 AND：no_match", typesNo.json?.pruned?.[0]?.reason, "no_match");
	const andHit = await deliver(andHook.hookUrl, ghHead("issues", "and3"), {
		action: "labeled",
		issue: { number: 1, title: "t", labels: [{ name: "bug" }] },
	});
	V.expectStatus("12.5 types + all + any 三者都成立 → 200", andHit, 200);
	H.eq("12.5 三者都成立：1 任务", (andHit.json?.tasks || []).length, 1);

	const rows33 = Array.from({ length: 33 }, () => ({ field: "issue.title", op: "exists" }));
	const tooMany = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "mc.rows-33",
			kind: "webhook",
			targets: [{ capability: "mc.issue", match: { all: rows33 } }],
		}),
	);
	V.expectStatus("12.6 all 写 33 行 → 400", tooMany, 400);
	H.check(
		"12.6 33 行：message 含上限 32",
		String(tooMany.json?.details?.causes?.[0]?.message || "").includes("32"),
		JSON.stringify(tooMany.json?.details),
	);
	H.eq("12.6 33 行：causes[0].field 指向 match.all", tooMany.json?.details?.causes?.[0]?.field, "spec.targets[0].match.all");
	const just32 = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "mc.rows-32",
			kind: "webhook",
			targets: [{ capability: "mc.issue", match: { all: rows33.slice(0, 32) } }],
		}),
	);
	V.expectStatus("12.6 all 写 32 行 → 201（边界是 >32 不是 ≥32）", just32, 201);

	for (const [i, [label, match, field, reason]] of [
		["match 出现未知键 type", { type: ["issues.opened"] }, "spec.targets[0].match.type", "unknown_field"],
		["all 是空组合子", { all: [] }, "spec.targets[0].match.all", "invalid_source_config"],
		[
			"op 写错成 =~",
			{ any: [{ field: "a", op: "=~", value: "x" }] },
			"spec.targets[0].match.any[0].op",
			"invalid_source_config",
		],
		[
			"field 路径坏成 issue.labels.0-name",
			{ all: [{ field: "issue.labels.0-name", op: "exists" }] },
			"spec.targets[0].match.all[0].field",
			"invalid_source_config",
		],
	].entries()) {
		const res = await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({ name: `mc.bad-match-${i}`, kind: "webhook", targets: [{ capability: "mc.issue", match }] }),
		);
		V.expectStatus(`12.7 ${label} → 400`, res, 400);
		H.eq(`12.7 ${label}：顶层 reason=${reason}`, res.json?.reason, reason);
		H.eq(`12.7 ${label}：causes[0].field 精确`, res.json?.details?.causes?.[0]?.field, field);
	}

	const proj = await H.req(server.base, V.P.source(NS, "mc.and"), { session });
	V.expectStatus("12.8 GET mc.and → 200", proj, 200);
	const projMatch = proj.json?.spec?.targets?.[0]?.match;
	H.eq("12.8 回显 types 原样", projMatch?.types, ["issues.labeled"]);
	H.eq("12.8 回显 all[0].negate === true", projMatch?.all?.[0]?.negate, true);
	H.eq("12.8 回显 all[0].field + op", `${projMatch?.all?.[0]?.field} ${projMatch?.all?.[0]?.op}`, "issue.pull_request exists");
	H.eq("12.8 回显 any[0].value 深等于 [\"bug\"]", projMatch?.any?.[0]?.value, ["bug"]);

	// ---------- 13. #522 router 决策通道 + outcomes 表（断言落在 trace 上：判决执行在 result 之后）----------
	console.log("--- 13. #522 router 决策通道 / outcomes 表 ---");

		const decisionPath = (taskId) => `/v1/tasks/${encodeURIComponent(taskId)}/decision`;
	const postDecision = (taskId, token, body) =>
		H.req(server.base, decisionPath(taskId), {
			method: "POST",
			...(token === undefined ? {} : { key: token }),
			body,
		});

	const routeTypes = (doc) =>
		(doc?.events || [])
			.filter((e) => e?.spec?.involvedObject?.name === "route")
			.map((e) => e?.spec?.eventType);
	const hasRouteType = (doc, want) => routeTypes(doc).includes(want);
	const childrenOf = (doc, parentTaskId) =>
		(doc?.tasks || []).filter((t) => t?.spec?.parentTaskId === parentTaskId);
	const waitTrace = async (traceId, pred) => {
		// 上限 24s（并行跑满时抢 CPU；判决执行跟在 task.result 之后，8s 会假红）。
		let doc = {};
		for (let i = 0; i < 240; i++) {
			doc = (await H.req(server.base, V.P.trace(NS, traceId), { session })).json || {};
			if (pred(doc)) return doc;
			await new Promise((r) => setTimeout(r, 100));
		}
		return doc;
	};

	const closeCap = await defineCap("issue-close", {
		params: {
			type: "object",
			properties: { reason: { type: "string" }, title: { type: "string" } },
		},
	});
	const notifyCap = await defineCap("issue-notify", {
		params: { type: "object", properties: { channel: { type: "string" } } },
	});
	const triageCap = await defineCap("issue-triage", {
		params: { type: "object", properties: { title: { type: "string" } } },
	});
	const routerCap = await defineCap("issue-router", {
		params: { type: "object", properties: { event: { type: "object" } } },
		next: ["issue-close", "issue-notify", "issue-triage"],
	});
	const routerNode = await connectNode("router-node", {
		slots: 4,
		declared: [capRef(routerCap), capRef(closeCap), capRef(notifyCap), capRef(triageCap)],
	});
	await deployToNode(routerNode, "issue-router");

	const routerSrc = await mkHookSource("r522.router", {
		targets: [
			{
				capability: "issue-router",
				params: { event: "$(payload)" },
				outcomes: {
					close: [
						{
							capability: "issue-close",
							params: { reason: "$(decision.reason)", title: "$(payload.issue.title)" },
						},
						{ capability: "issue-notify", params: { channel: "#mod" } },
					],
					triage: [{ capability: "issue-triage", params: { title: "$(payload.issue.title)" } }],
					drop: [],
				},
			},
		],
	});
	const echoTarget = routerSrc.doc?.spec?.targets?.[0] || {};
	H.eq(
		"13.0 读投影回显结局名（close / drop / triage）",
		Object.keys(echoTarget.outcomes || {}).sort(),
		["close", "drop", "triage"],
	);
	H.eq("13.0 读投影回显 drop: []（空动作结局合法）", (echoTarget.outcomes?.drop || []).length, 0);
	H.eq(
		"13.0 读投影回显动作的参数模板（$(decision.reason) 原样保留，不在此刻求值）",
		echoTarget.outcomes?.close?.[0]?.params?.reason,
		"$(decision.reason)",
	);

	let r522Seq = 0;
	const deliverIssue = (tag, payload) =>
		deliver(routerSrc.hookUrl, {}, {
			type: "issue.opened",
			id: `r522-${tag}-${Date.now()}-${++r522Seq}`,
			payload,
		});

	const startRouterTask = async (label, payload) => {
		const res = await deliverIssue(label, payload);
		V.expectStatus(`${label}：投递 → 200`, res, 200);
		H.eq(`${label}：回执恰好 1 个任务（router 自己）`, (res.json?.tasks || []).length, 1);
		const taskId = res.json?.tasks?.[0]?.taskId;
		const traceId = res.json?.traceId;
		const dis = await routerNode.waitDispatch(taskId);
		H.check(
			`${label}：router 任务收到 task.dispatch`,
			!!dis,
			JSON.stringify(routerNode.frames.map((f) => f.type)),
		);
		if (dis) routerNode.accept(dis, 0);
		const grant = dis ? await routerNode.waitGrant(taskId) : null;
		H.check(
			`${label}：拿到 lease.grant（执行权只来自这里）`,
			!!grant,
			JSON.stringify(routerNode.frames.map((f) => f.type)),
		);
		return { taskId, traceId, grant };
	};

	const one = await startRouterTask("13.1", { issue: { title: "RAM 飙升" } });
	H.check(
		"13.1 grant.decisionToken 非空（只有声明了 outcomes 的 router 任务才有）",
		typeof one.grant?.decisionToken === "string" && one.grant.decisionToken.length >= 16,
		JSON.stringify(one.grant && Object.keys(one.grant)),
	);
	const routerDoc1 = (await H.req(server.base, V.P.task(NS, one.taskId), { session })).json;
	H.eq(
		"13.1 router 任务的 params 取到 $(payload) 整根载荷（建任务时求值）",
		routerDoc1?.spec?.params?.event?.issue?.title,
		"RAM 飙升",
	);
	const d1 = await postDecision(one.taskId, one.grant?.decisionToken, {
		outcome: "close",
		params: { reason: "spam" },
	});
	V.expectStatus("13.1 POST decision close → 200", d1, 200);
	H.eq("13.1 回执 actions=2（close 挂了两个动作）", d1.json?.actions, 2);
	H.eq(
		"13.1 回执的 taskId/outcome 与提交逐字相同",
		`${d1.json?.taskId}/${d1.json?.outcome}`,
		`${one.taskId}/close`,
	);
	routerNode.result(one.grant, { phase: "succeeded" });
	const doc1 = await waitTrace(one.traceId, (d) => childrenOf(d, one.taskId).length >= 2);
	const kids1 = childrenOf(doc1, one.taskId);
	H.eq("13.1 结局 close → 恰好 2 个子任务", kids1.length, 2);
	H.eq(
		"13.1 子任务能力集合 = {issue-close, issue-notify}",
		kids1.map((t) => t?.spec?.capability?.name).sort(),
		["issue-close", "issue-notify"],
	);
	H.check(
		"13.1 没有派 issue-triage（未选中的结局不执行）",
		!kids1.some((t) => t?.spec?.capability?.name === "issue-triage"),
		JSON.stringify(kids1.map((t) => t?.spec?.capability?.name)),
	);
	H.check(
		"13.1 每个子任务的 parentTaskId 都指向 router 任务",
		kids1.every((t) => t?.spec?.parentTaskId === one.taskId),
		JSON.stringify(kids1.map((t) => t?.spec?.parentTaskId)),
	);
	const closeKid = kids1.find((t) => t?.spec?.capability?.name === "issue-close") || {};
	H.eq(
		"13.1 issue-close 的 reason 来自判决 params（$(decision.reason) 判决时求值）",
		closeKid?.spec?.params?.reason,
		"spam",
	);
	H.eq(
		"13.1 issue-close 的 title 来自载荷（$(payload.issue.title) 建任务时求值）",
		closeKid?.spec?.params?.title,
		"RAM 飙升",
	);

	const two = await startRouterTask("13.2", { issue: { title: "需要人工判断" } });
	H.check("13.2 新投递是新 trace（不是幂等重放）", two.traceId && two.traceId !== one.traceId, `${two.traceId} vs ${one.traceId}`);
	const d2 = await postDecision(two.taskId, two.grant?.decisionToken, { outcome: "triage" });
	V.expectStatus("13.2 POST decision triage → 200", d2, 200);
	H.eq("13.2 回执 actions=1", d2.json?.actions, 1);
	routerNode.result(two.grant, { phase: "succeeded" });
	const doc2 = await waitTrace(two.traceId, (d) => childrenOf(d, two.taskId).length >= 1);
	const kids2 = childrenOf(doc2, two.taskId);
	H.eq("13.2 结局 triage → 恰好 1 个子任务", kids2.length, 1);
	H.eq("13.2 那一条就是 issue-triage", kids2[0]?.spec?.capability?.name, "issue-triage");
	H.eq("13.2 triage 的参数取自载荷", kids2[0]?.spec?.params?.title, "需要人工判断");
	H.eq("13.2 子任务与 router 同一条 trace", kids2[0]?.spec?.traceUID, two.traceId);

	const three = await startRouterTask("13.3", { issue: { title: "噪音" } });
	const d3 = await postDecision(three.taskId, three.grant?.decisionToken, { outcome: "drop" });
	V.expectStatus("13.3 POST decision drop（drop: []）→ 200", d3, 200);
	H.eq("13.3 空动作结局的 actions=0", d3.json?.actions, 0);
	routerNode.result(three.grant, { phase: "succeeded" });
	const doc3 = await waitTrace(three.traceId, (d) => hasRouteType(d, "outcome_dropped"));
	H.eq("13.3 drop → 0 子任务", childrenOf(doc3, three.taskId).length, 0);
	H.check(
		"13.3 trace 里有 outcome_dropped 观察行",
		hasRouteType(doc3, "outcome_dropped"),
		JSON.stringify(routeTypes(doc3)),
	);

	const four = await startRouterTask("13.4", { issue: { title: "没人问" } });
	routerNode.result(four.grant, { phase: "succeeded" });
	const doc4 = await waitTrace(four.traceId, (d) => hasRouteType(d, "no_decision"));
	H.eq("13.4 没判决 → 0 子任务", childrenOf(doc4, four.taskId).length, 0);
	H.check("13.4 trace 里有 no_decision 观察行", hasRouteType(doc4, "no_decision"), JSON.stringify(routeTypes(doc4)));

	const five = await startRouterTask("13.5", { issue: { title: "越界" } });
	V.expectReason(
		"13.5 无 Bearer 凭据 → 401",
		await postDecision(five.taskId, undefined, { outcome: "close" }),
		401,
		"token_invalid",
	);
	V.expectReason(
		"13.5 结局名不在声明表里 → 403 outcome_not_declared",
		await postDecision(five.taskId, five.grant?.decisionToken, { outcome: "bogus" }),
		403,
		"outcome_not_declared",
	);
	routerNode.result(five.grant, { phase: "succeeded" });
	const doc5 = await waitTrace(five.traceId, (d) => hasRouteType(d, "no_decision"));
	H.eq("13.5 越界判决不派任何任务", childrenOf(doc5, five.taskId).length, 0);
	H.check(
		"13.5 trace 里有 outcome_not_declared 审计行（越界必须留痕）",
		hasRouteType(doc5, "outcome_not_declared"),
		JSON.stringify(routeTypes(doc5)),
	);

	const six = await startRouterTask("13.6", { issue: { title: "重复提交" } });
	const d6 = await postDecision(six.taskId, six.grant?.decisionToken, {
		outcome: "close",
		params: { reason: "dup" },
	});
	V.expectStatus("13.6 首次 POST close → 200", d6, 200);
	H.eq("13.6 首次 actions=2", d6.json?.actions, 2);
	V.expectReason(
		"13.6 第二次 POST close → 409 decision_already_taken",
		await postDecision(six.taskId, six.grant?.decisionToken, { outcome: "close", params: { reason: "dup" } }),
		409,
		"decision_already_taken",
	);
	routerNode.result(six.grant, { phase: "succeeded" });
	const doc6 = await waitTrace(six.traceId, (d) => childrenOf(d, six.taskId).length >= 2);
	H.eq("13.6 判决只执行一次：仍然恰好 2 个子任务（不是 4）", childrenOf(doc6, six.taskId).length, 2);

	const badNext = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "r522.bad-next",
			kind: "webhook",
			targets: [{ capability: "issue-router", outcomes: { close: [{ capability: "not-in-next" }] } }],
		}),
	);
	V.expectStatus("13.7 ① 动作能力不在 spec.next → 400", badNext, 400);
	H.eq("13.7 ① reason = invalid_source_config", badNext.json?.reason, "invalid_source_config");
	H.eq(
		"13.7 ① causes[0].field 精确到 outcomes.close[0].capability",
		badNext.json?.details?.causes?.[0]?.field,
		"spec.targets[0].outcomes.close[0].capability",
	);
	for (const [i, badName] of ["Close", "has space"].entries()) {
		const badNameRes = await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({
				name: `r522.bad-name-${i}`,
				kind: "webhook",
				targets: [{ capability: "issue-router", outcomes: { [badName]: [] } }],
			}),
		);
		V.expectStatus(`13.7 ② 结局名 ${JSON.stringify(badName)} 非法 → 400`, badNameRes, 400);
		H.eq(
			`13.7 ② 名字非法时 causes[0].field 只指到 outcomes（名字进不了路径）`,
			badNameRes.json?.details?.causes?.[0]?.field,
			"spec.targets[0].outcomes",
		);
	}
	const badParams = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({
			name: "r522.bad-params",
			kind: "webhook",
			targets: [{ capability: "issue-router", params: { event: "$(payload)", why: "$(decision.reason)" } }],
		}),
	);
	V.expectStatus("13.7 ③ target 参数写 $(decision.…) → 400", badParams, 400);
	H.eq("13.7 ③ reason = invalid_source_config", badParams.json?.reason, "invalid_source_config");

	const eight = await startRouterTask("13.8", { issue: { title: "脚本炸了" } });
	routerNode.result(eight.grant, {
		phase: "failed",
		error: { reason: "script_failed", message: "boom" },
	});
	const doc8 = await waitTrace(eight.traceId, (d) => hasRouteType(d, "router_failed"));
	H.eq("13.8 失败终态 → 0 子任务（结局不执行）", childrenOf(doc8, eight.taskId).length, 0);
	H.check(
		"13.8 trace 里有 router_failed 观察行",
		hasRouteType(doc8, "router_failed"),
		JSON.stringify(routeTypes(doc8)),
	);
	// ---------- 14. kind: poll（#532）：SSRF 默认拒 / 凭据只给引用 / 轮询状态面 ----------
	console.log("--- 14. kind: poll（#532）：SSRF 默认拒 / 凭据只给引用 / 轮询状态面 ---");
	const POLL_SCHEDULE = { expr: "*/5 * * * *" };
	const POLL_QUIET_SCHEDULE = { expr: "0 0 1 1 *" };
	const POLL_SECRET = "poll-fixture-cred";
	let pollBad = 0;
	const pollBody = (name, config) => V.sourceBody({ name, kind: "poll", config });
	const expectPollRejected = async (label, config, field) => {
		const res = await V.createSource(server.base, session, NS, pollBody(`poll.bad-${++pollBad}`, config));
		V.expectReason(`14.${label}`, res, 400, "invalid_source_config");
		H.eq(`14.${label}：causes[0].field`, res.json?.details?.causes?.[0]?.field, field);
		return res;
	};

	for (const [label, config] of [
		["1 http 环回 http://127.0.0.1:9/feed（没写 allowInsecure）", { url: "http://127.0.0.1:9/feed", schedule: POLL_SCHEDULE }],
		["1 云元数据 https://169.254.169.254/latest/meta-data", { url: "https://169.254.169.254/latest/meta-data", schedule: POLL_SCHEDULE }],
		["1 私网 https://10.0.0.5/feed", { url: "https://10.0.0.5/feed", schedule: POLL_SCHEDULE }],
		["1 localhost 名字 https://localhost/feed", { url: "https://localhost/feed", schedule: POLL_SCHEDULE }],
		["1 内部域名 https://api.internal/feed", { url: "https://api.internal/feed", schedule: POLL_SCHEDULE }],
		[
			"1 公网 http 配 allowInsecure 也不放开 http://example.com/feed",
			{ url: "http://example.com/feed", allowInsecure: true, schedule: POLL_SCHEDULE },
		],
	]) {
		await expectPollRejected(label, config, "spec.config.url");
	}

	await expectPollRejected(
		"2 maxItems 写 501（上限 500）",
		{ url: "https://example.com/feed", schedule: POLL_SCHEDULE, maxItems: 501 },
		"spec.config.maxItems",
	);
	await expectPollRejected(
		"2 schedule.expr 分钟越界 61 * * * *",
		{ url: "https://example.com/feed", schedule: { expr: "61 * * * *" } },
		"spec.config.schedule",
	);

	const pollRef = { name: "e2e-poll-cred", header: "Authorization", prefix: "Bearer " };
	const pollConfig = { url: "https://example.com/feed", schedule: POLL_QUIET_SCHEDULE, secretRef: pollRef };
	const refOnly = await V.createSource(server.base, session, NS, pollBody("poll.ref-only", pollConfig));
	V.expectReason("14.3 只给 secretRef 而命名空间里还没有这条凭据", refOnly, 400, "invalid_source_config");
	H.eq(
		"14.3 只给引用：causes[0].field 精确到 spec.config.secretRef",
		refOnly.json?.details?.causes?.[0]?.field,
		"spec.config.secretRef",
	);
	const withSecret = await V.createSource(
		server.base,
		session,
		NS,
		pollBody("poll.with-secret", { ...pollConfig, secret: POLL_SECRET }),
	);
	V.expectStatus("14.3 同一份配置补上 write-only secret → 201", withSecret, 201);
	H.check(
		"14.3 创建响应里没有 config.secret 键（凭据只写不读）",
		!(withSecret.json?.spec?.config && "secret" in withSecret.json.spec.config),
		JSON.stringify(withSecret.json?.spec?.config),
	);
	H.check(
		"14.3 创建响应里没有凭据原文",
		!String(withSecret.text).includes(POLL_SECRET),
		String(withSecret.text).slice(0, 200),
	);
	const reused = await V.createSource(server.base, session, NS, pollBody("poll.reuse", pollConfig));
	V.expectStatus("14.3 第二个源只给同名 secretRef、不带 secret → 201（复用命名空间凭据）", reused, 201);

	const pollProj = await H.req(server.base, V.P.source(NS, "poll.with-secret"), { session });
	V.expectStatus("14.4 GET poll.with-secret → 200", pollProj, 200);
	V.assertEnvelope("14.4 poll 源", pollProj.json, "Source");
	H.eq("14.4 spec.kind 是 poll", pollProj.json?.spec?.kind, "poll");
	H.eq("14.4 spec.config.url 原样回显", pollProj.json?.spec?.config?.url, pollConfig.url);
	H.eq("14.4 spec.config.secretRef.name 原样回显", pollProj.json?.spec?.config?.secretRef?.name, "e2e-poll-cred");
	H.check(
		"14.4 读投影里没有 config.secret 键",
		!(pollProj.json?.spec?.config && "secret" in pollProj.json.spec.config),
		JSON.stringify(pollProj.json?.spec?.config),
	);
	H.check(
		"14.4 整个读响应里不出现凭据原文",
		!String(pollProj.text).includes(POLL_SECRET),
		String(pollProj.text).slice(0, 200),
	);
	H.eq("14.4 status.consecutiveFailures 恒存在且为 0", pollProj.json?.status?.consecutiveFailures, 0);
	H.eq("14.4 status.skippedOverlaps 恒存在且为 0", pollProj.json?.status?.skippedOverlaps, 0);
	H.eq("14.4 status.lastError 恒存在、未失败时是 null", pollProj.json?.status?.lastError, null);
	H.check(
		"14.4 status.nextFireAt 按 schedule 算出来了（RFC3339）",
		typeof pollProj.json?.status?.nextFireAt === "string" && !Number.isNaN(Date.parse(pollProj.json.status.nextFireAt)),
		JSON.stringify(pollProj.json?.status),
	);

	const kindChange = await H.req(server.base, V.P.source(NS, "poll.with-secret"), {
		session,
		method: "PATCH",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Source",
			metadata: { resourceVersion: pollProj.json?.metadata?.resourceVersion },
			spec: { kind: "manual" },
		},
	});
	V.expectReason("14.5 PATCH 把 poll 改成 manual（kind 不可变）", kindChange, 400, "source_kind_immutable");

	// ---------- 15. 「立即拉取一次」（#555）：同一把 in-flight 锁 / 失败不动游标 / 只有 poll 源能点 ----------
	console.log("--- 15. 「立即拉取一次」（#555）：同一把锁 / 失败不动游标 / 只有 poll 源能点 ---");
	// 本机 feed：listen(0) 随机端口（不占 SUITE_PORTS 段），响应体由 state 决定；15.2 的并发由
	// 「feed 里发信号 + 等信号」造出来（不靠 sleep 计时序）。
	const feeds = [];
	const mkFeed = async () => {
		const state = { body: { items: [] }, etag: '"e1"', gate: null, arrived: null };
		const srv = createServer(async (_req, res) => {
			if (state.arrived) {
				const hit = state.arrived;
				state.arrived = null;
				hit();
			}
			if (state.gate) await state.gate;
			res.writeHead(200, { "Content-Type": "application/json", ETag: state.etag });
			res.end(JSON.stringify(state.body));
		});
		await new Promise((resolve) => srv.listen(0, "127.0.0.1", resolve));
		feeds.push(srv);
		return { state, url: `http://127.0.0.1:${srv.address().port}/feed` };
	};
	const pollFeedConfig = (url) => ({
		url,
		allowInsecure: true,
		schedule: POLL_QUIET_SCHEDULE,
		envelope: { items: "items", type: { pointer: "type" }, deliveryId: { pointer: "id" } },
	});
	const pollNow = (name) => H.req(server.base, V.P.sourcePoll(NS, name), { session, method: "POST" });
	const pollStatus = async (name) =>
		(await H.req(server.base, V.P.source(NS, name), { session })).json?.status;
	const pollSource = (name, url) =>
		V.createSource(server.base, session, NS, V.sourceBody({ name, kind: "poll", config: pollFeedConfig(url) }));

	const okFeed = await mkFeed();
	okFeed.state.body = { items: [{ id: "a", type: "advisory" }, { id: "b", type: "advisory" }] };
	V.expectStatus(
		"15.1 建 poll 源（环回 http + allowInsecure，schedule 不自己触发）→ 201",
		await pollSource("poll.now-ok", okFeed.url),
		201,
	);
	const okFirst = await pollNow("poll.now-ok");
	V.expectStatus("15.1 立即拉取一次 → 200", okFirst, 200);
	H.eq("15.1 items=2", okFirst.json?.items, 2);
	H.eq("15.1 created=2", okFirst.json?.created, 2);
	H.eq("15.1 duplicates=0", okFirst.json?.duplicates, 0);
	H.eq("15.1 cursorAdvanced=true", okFirst.json?.cursorAdvanced, true);
	H.check(
		"15.1 durationMs 是 number ≥ 0",
		typeof okFirst.json?.durationMs === "number" && okFirst.json.durationMs >= 0,
		JSON.stringify(okFirst.json),
	);
	const okEvents = V.items(
		await H.req(server.base, V.P.events(NS) + "?source=poll.now-ok&limit=50", { session }),
	) || [];
	H.eq("15.1 事件页里这批事件 2 条", okEvents.length, 2);
	H.eq(
		"15.1 事件的 deliveryId 就是 feed item 的 id",
		JSON.stringify(okEvents.map((e) => e?.spec?.deliveryId).sort()),
		JSON.stringify(["a", "b"]),
	);
	H.check(
		"15.1 事件类型取自 feed item 的 type",
		okEvents.every((e) => e?.spec?.eventType === "advisory"),
		JSON.stringify(okEvents.map((e) => e?.spec?.eventType)),
	);
	const okAgain = await pollNow("poll.now-ok");
	V.expectStatus("15.1 再点一次 → 200", okAgain, 200);
	H.eq("15.1 再点一次 created=0（同一份 feed 不新建事件）", okAgain.json?.created, 0);
	H.eq("15.1 再点一次 duplicates=2（按 deliveryId 命中去重）", okAgain.json?.duplicates, 2);

	const slowFeed = await mkFeed();
	slowFeed.state.body = { items: [{ id: "s1", type: "advisory" }] };
	V.expectStatus("15.2 建慢 feed 的 poll 源 → 201", await pollSource("poll.now-slow", slowFeed.url), 201);
	let releaseFirst;
	slowFeed.state.gate = new Promise((resolve) => {
		releaseFirst = resolve;
	});
	const firstArrived = new Promise((resolve) => {
		slowFeed.state.arrived = resolve;
	});
	const inFlight = pollNow("poll.now-slow");
	await firstArrived;
	V.expectReason("15.2 第一次还在飞时再点一次", await pollNow("poll.now-slow"), 409, "poll_in_progress");
	releaseFirst();
	const firstSlow = await inFlight;
	V.expectStatus("15.2 放行后第一次 → 200", firstSlow, 200);
	H.eq("15.2 放行后第一次拿到了 feed 的 1 条", firstSlow.json?.items, 1);
	slowFeed.state.gate = null;
	V.expectStatus("15.2 锁释放后再点一次 → 200（不排队、也不残留）", await pollNow("poll.now-slow"), 200);

	const failFeed = await mkFeed();
	failFeed.state.body = { items: [{ id: "f1", type: "advisory" }] };
	V.expectStatus("15.3 建 poll 源 → 201", await pollSource("poll.now-fail", failFeed.url), 201);
	V.expectStatus("15.3 先跑一轮成功 → 200", await pollNow("poll.now-fail"), 200);
	const cursorBefore = await pollStatus("poll.now-fail");
	H.eq("15.3 成功一轮后 status.cursor 落在 feed 的 ETag 上", cursorBefore?.cursor, '"e1"');
	failFeed.state.etag = '"e2"';
	failFeed.state.body = { feed: { items: [] } };
	const failed = await pollNow("poll.now-fail");
	V.expectReason("15.3 envelope.items 取不到顶层 items 数组", failed, 502, "poll_failed");
	H.check(
		"15.3 502 回的是错误原文（message 非空）",
		typeof failed.json?.message === "string" && failed.json.message.length > 0,
		JSON.stringify(failed.json),
	);
	const cursorAfter = await pollStatus("poll.now-fail");
	// 变异②的哨兵：手动触发失败若也前进游标，cursorAfter 就会与 cursorBefore 不同 → 红。
	H.eq("15.3 失败不动游标（与失败前逐字相同）", cursorAfter?.cursor, cursorBefore?.cursor);
	H.eq("15.3 失败计数 = 1", cursorAfter?.consecutiveFailures, 1);

	V.expectStatus(
		"15.4 建 kind=manual 的源 → 201",
		await V.createSource(server.base, session, NS, V.sourceBody({ name: "poll.now-manual" })),
		201,
	);
	V.expectReason("15.4 对非 poll 源点立即拉取", await pollNow("poll.now-manual"), 409, "not_poll_source");

	V.expectStatus(
		"15.5 未登录点立即拉取 → 401",
		await H.req(server.base, V.P.sourcePoll(NS, "poll.now-slow"), { method: "POST" }),
		401,
	);
	const okProj = await H.req(server.base, V.P.source(NS, "poll.now-ok"), { session });
	V.expectStatus(
		"15.6 停用 poll 源 → 200",
		await H.req(server.base, V.P.source(NS, "poll.now-ok"), {
			session,
			method: "PATCH",
			body: {
				apiVersion: V.API_VERSION,
				kind: "Source",
				metadata: { resourceVersion: okProj.json?.metadata?.resourceVersion },
				spec: { enabled: false },
			},
		}),
		200,
	);
	V.expectReason("15.6 停用的 poll 源点立即拉取", await pollNow("poll.now-ok"), 409, "source_disabled");

	console.log("--- 16. 源审计读端点（#559）：polled_manually 一条 / limit 边界 / before 游标 / 跨 ns ---");
	const AUDIT_SRC = "poll.audit";
	const auditFeed = await mkFeed();
	auditFeed.state.body = { items: [{ id: "au1", type: "advisory" }, { id: "au2", type: "advisory" }] };
	V.expectStatus("16.1 建 poll 源（本地 feed）→ 201", await pollSource(AUDIT_SRC, auditFeed.url), 201);
	const auditPath = (ns, name, qs = "") => V.P.source(ns, name) + "/audit" + qs;
	const sourceAudit = (qs = "") => H.req(server.base, auditPath(NS, AUDIT_SRC, qs), { session });
	const sessionUser = (await H.req(server.base, "/v1/auth/me", { session })).json || {};
	const who = sessionUser.id;
	H.check(
		"16.1 会话用户 id 非空（actor 的逐字基准）",
		typeof who === "string" && who.length > 0,
		JSON.stringify(sessionUser),
	);

	V.expectStatus("16.1 立即拉取一次 → 200", await pollNow(AUDIT_SRC), 200);
	const auditOne = await sourceAudit();
	V.expectStatus("16.1 GET …/sources/poll.audit/audit → 200", auditOne, 200);
	const auditOneRows = V.items(auditOne) || [];
	H.eq("16.1 审计恰好 1 条（一次人工拉取一行）", auditOneRows.length, 1);
	const auditFirst = auditOneRows[0] || {};
	H.eq("16.1 action = source.polled_manually", auditFirst.action, "source.polled_manually");
	H.eq("16.1 actor = 会话用户 id（逐字相等）", auditFirst.actor, who);
	H.check(
		"16.1 at 是 RFC3339 且能被 Date.parse",
		typeof auditFirst.at === "string" && !Number.isNaN(Date.parse(auditFirst.at)),
		String(auditFirst.at),
	);
	H.check(
		"16.1 审计项恰好 5 键（id/action/actor/at/detail）",
		JSON.stringify(Object.keys(auditFirst).sort()) === JSON.stringify(["action", "actor", "at", "detail", "id"]),
		JSON.stringify(Object.keys(auditFirst)),
	);
	H.check(
		"16.1 detail 是对象且恰好 ok/items/created/duplicates 四键",
		!!auditFirst.detail &&
			typeof auditFirst.detail === "object" &&
			JSON.stringify(Object.keys(auditFirst.detail).sort()) ===
				JSON.stringify(["created", "duplicates", "items", "ok"]),
		JSON.stringify(auditFirst.detail),
	);
	H.eq("16.1 detail.ok = true（这一轮成功）", auditFirst.detail?.ok, true);
	H.eq("16.1 detail.items = 2（feed 的条数）", auditFirst.detail?.items, 2);
	H.eq("16.1 detail.created = 2", auditFirst.detail?.created, 2);
	H.eq("16.1 detail.duplicates = 0", auditFirst.detail?.duplicates, 0);
	H.check(
		"16.1 detail 只存计数、不存响应正文（feed 里的 type 值不出现）",
		!String(auditOne.text).includes("advisory"),
		String(auditOne.text).slice(0, 200),
	);

	V.expectStatus("16.2 再点一次 → 200", await pollNow(AUDIT_SRC), 200);
	const auditTwoRows = V.items(await sourceAudit()) || [];
	H.eq("16.2 审计 2 条（每次拉取一行）", auditTwoRows.length, 2);
	H.check(
		"16.2 最新在前（items[0].at >= items[1].at，同秒时也成立）",
		Date.parse(auditTwoRows[0]?.at) >= Date.parse(auditTwoRows[1]?.at),
		`${auditTwoRows[0]?.at} vs ${auditTwoRows[1]?.at}`,
	);
	H.check(
		"16.2 两行 id 不同（每次拉取一行）",
		!!auditTwoRows[0]?.id && auditTwoRows[0].id !== auditTwoRows[1]?.id,
		`${auditTwoRows[0]?.id} vs ${auditTwoRows[1]?.id}`,
	);
	H.check(
		"16.2 两行 actor 都是会话用户",
		auditTwoRows.every((r) => r?.actor === who),
		JSON.stringify(auditTwoRows.map((r) => r?.actor)),
	);
	H.eq(
		"16.2 两行 detail 各记一轮（created=2/dups=0 与 created=0/dups=2，同秒并列时按集合比）",
		JSON.stringify(auditTwoRows.map((r) => `${r?.detail?.created}/${r?.detail?.duplicates}`).sort()),
		JSON.stringify(["0/2", "2/0"]),
	);

	const auditLimit1 = V.items(await sourceAudit("?limit=1")) || [];
	H.eq("16.3 limit=1 → 恰好 1 条", auditLimit1.length, 1);
	H.eq("16.3 limit=1 回的是最新那条（与全量第一条同 id）", auditLimit1[0]?.id, auditTwoRows[0]?.id);

	const auditBad = {};
	for (const qs of ["limit=0", "limit=201", "limit=-1", "limit=abc", "before=abc"]) {
		auditBad[qs] = await sourceAudit("?" + qs);
		V.expectReason(`16.4 ?${qs} → 400`, auditBad[qs], 400, "BadRequest");
	}
	const auditMsg = (qs) => String(auditBad[qs]?.json?.message || "");
	H.eq("16.4 limit=0 的 message 点名 1..200 区间", auditMsg("limit=0"), "limit must be between 1 and 200");
	H.eq("16.4 limit=201 的 message 与 limit=0 逐字相同", auditMsg("limit=201"), auditMsg("limit=0"));
	H.eq("16.4 limit=abc 的 message 是「limit must be an integer」", auditMsg("limit=abc"), "limit must be an integer");
	H.eq(
		"16.4 before=abc 的 message 是「before must be an RFC3339 timestamp」",
		auditMsg("before=abc"),
		"before must be an RFC3339 timestamp",
	);

	const auditEpoch = (await sourceAudit("?before=1970-01-01T00:00:00Z")).json;
	H.check(
		"16.5 before=1970-01-01T00:00:00Z → 正文恰好 {\"items\":[]}",
		JSON.stringify(auditEpoch) === '{"items":[]}',
		JSON.stringify(auditEpoch),
	);
	const auditFuture = V.items(await sourceAudit("?before=" + encodeURIComponent("2999-01-01T00:00:00Z"))) || [];
	H.eq("16.5 before=<未来> 条数不变（严格早于口径不裁未来行）", auditFuture.length, auditTwoRows.length);

	const nsAudit = "aud" + Date.now().toString(36);
	V.expectStatus("16.6 建第二个命名空间 → 201", await V.createNamespace(server.base, session, nsAudit, "审计隔离"), 201);
	V.expectStatus(
		"16.6 第二个 ns 建同名 poll 源（从未 poll）→ 201",
		await V.createSource(
			server.base,
			session,
			nsAudit,
			V.sourceBody({ name: AUDIT_SRC, kind: "poll", config: pollFeedConfig(auditFeed.url) }),
		),
		201,
	);
	const auditCrossEmpty = await H.req(server.base, auditPath(nsAudit, AUDIT_SRC), { session });
	V.expectStatus("16.6 同名源在第二个 ns 读到 200", auditCrossEmpty, 200);
	H.check(
		'16.6 同名未 poll 源的审计是空的 {"items":[]}（隔离：读不到 nsA 的行）',
		JSON.stringify(auditCrossEmpty.json) === '{"items":[]}',
		JSON.stringify(auditCrossEmpty.json),
	);
	const auditGhostLocal = await H.req(server.base, auditPath(NS, "poll.ghost"), { session });
	V.expectReason("16.6 本 ns 里不存在的源 → 404", auditGhostLocal, 404, "NotFound");
	const auditGhostCross = await H.req(server.base, auditPath(nsAudit, "poll.ghost"), { session });
	V.expectReason("16.6 第二个 ns 里不存在的源 → 404", auditGhostCross, 404, "NotFound");
	H.eq(
		"16.6 两种 404 正文逐字节相同（跨 ns 与不存在同形，不泄露存在性）",
		auditGhostCross.text,
		auditGhostLocal.text,
	);
	V.expectStatus("16.7 匿名 GET 源审计 → 401", await H.req(server.base, auditPath(NS, AUDIT_SRC), {}), 401);

	// ---------- 17. 多节点派发分散（#681）：四单不许全堆在字典序小的那台（现场：另一台一次提议都收不到）----------
	console.log("--- 17. 多节点派发分散（#681）---");
	const spreadCap = "spread.spread681";
	const spreadDoc = await defineCap(spreadCap, { concurrency: { maxPerNode: 1 } });
	V.expectStatus(
		"17.1 建 targets=[spread.spread681] 的 manual 源 → 201",
		await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({ name: "spread.src", kind: "manual", targets: [{ capability: spreadCap }] }),
		),
		201,
	);
	const spreadA = await connectNode("spread-node-a", { slots: 2, declared: [capRef(spreadDoc)] });
	const spreadB = await connectNode("spread-node-b", { slots: 2, declared: [capRef(spreadDoc)] });
	await deployToNode(spreadA, spreadCap);
	await deployToNode(spreadB, spreadCap);

	const dispatchedOn = (node) =>
		new Set(node.frames.filter((f) => f.type === "task.dispatch").map((f) => f.taskId));
	for (let i = 0; i < 4; i++) {
		const res = await postEvent({ source: "spread.src", type: "spread.go", payload: { i } });
		V.expectStatus(`17.2 第 ${i + 1} 单入队 → 200`, res, 200);
		H.eq(`17.2 第 ${i + 1} 单恰好 1 个任务`, (res.json?.tasks || []).length, 1);
	}
	let spreadSeen = null;
	const spreadDone = await V.tryPoll(() => {
		const a = dispatchedOn(spreadA).size;
		const b = dispatchedOn(spreadB).size;
		if (a === 2 && b === 2) {
			spreadSeen = [a, b];
			return true;
		}
		return false;
	}, { timeoutMs: 10000 });
	H.check("17.3 两台候选各 2 条提案在 10s 内到齐", spreadDone, "超时仍是中间态");
	H.eq("17.3 四单在两台候选上各 2 条提案（不是 4:0）", spreadSeen, [2, 2]);

	const accepted = [];
	for (const node of [spreadA, spreadB]) {
		const frame = node.frames.find((f) => f.type === "task.dispatch");
		H.check(`17.4 ${node.name} 有可接受的提案`, !!frame, JSON.stringify(node.frames.map((f) => f.type)).slice(0, 200));
		if (!frame) continue;
		node.accept(frame, 0);
		const grant = await node.waitGrant(frame.taskId);
		H.check(
			`17.5 ${node.name} 的提案拿到 lease.grant（第 1 次执行）`,
			!!grant && grant.attempt === 1,
			JSON.stringify(grant).slice(0, 200),
		);
		accepted.push({ taskId: frame.taskId });
	}
	H.eq("17.5 两台各接受 1 条（maxPerNode=1 时一台最多执行 1 条）", accepted.length, 2);
	H.eq("17.5 接受的是两条不同任务", new Set(accepted.map((a) => a.taskId)).size, 2);

	const taskStatus = async (taskId) =>
		(await H.req(server.base, V.P.task(NS, taskId), { session })).json?.status || {};
	const runningOn = [];
	for (const { taskId } of accepted) {
		await V.tryPoll(async () => (await taskStatus(taskId)).phase === "running", { timeoutMs: 5000 });
		const status = await taskStatus(taskId);
		H.eq(`17.6 ${taskId} 真的在跑（attempt=1）`, [status.phase, status.attempt], ["running", 1]);
		runningOn.push(status.nodeName);
	}
	H.eq("17.6 两条执行分别落在两台机器上", runningOn, ["spread-node-a", "spread-node-b"]);

	const others = [...dispatchedOn(spreadA), ...dispatchedOn(spreadB)].filter(
		(id) => !accepted.some((a) => a.taskId === id),
	);
	for (const taskId of others) {
		const status = await taskStatus(taskId);
		H.check(
			`17.7 ${taskId} 还在等槽（两条同能力执行不许越过 maxPerNode=1）`,
			status.phase !== "running",
			JSON.stringify(status),
		);
	}

	for (const srv of feeds.splice(0)) {
		srv.closeAllConnections();
		await new Promise((resolve) => srv.close(resolve));
	}

	for (const n of nodes) n.conn.close();
	server.stop();
	// summary() 返回 true 表示**全部通过**（不是 "failed"），别搞反
	const allPassed = H.summary("routing");
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	for (const n of nodes) {
		try {
			n.conn.close();
		} catch {
			/* 已关闭 */
		}
	}
	if (server) server.stop();
	process.exit(1);
});
