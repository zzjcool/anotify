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
 *                                            引用解析不到的出口 → 建源被拒（见 FINDING）
 *   ③ param_schema                           拆成 v4 的两条：`param_missing`（缺必填）与
 *                                            `param_type`（类型不符）——v4 把这两种情况分开报
 *   ④ no_node                                能力+源都在，但没有节点持有该 digest →
 *                                            任务照建、pending、不剪枝、无人被挑
 *   ⑤ foreign_user                           v4 没有「按用户路由」（用户维度是 ns 授权）；
 *                                            同一条意图改成「源不属于本 ns → fire 404，不跨 ns 泄漏」
 *   ⑥ path（环剪枝）                          v4 没有 path 概念（v3 router 的遗留）。派生边的
 *                                            可解析性由 `status.resolved.next`（digest）与
 *                                            `GET …/edges` 投影证明；下游非法目标由 emit 侧
 *                                            `target_not_declared` 拒（归 node_v4/emit 覆盖）
 *   A5 出口封闭（port 不在 outputs）          v4 没有 port；闭合性 = `spec.next` 逐条解析出
 *                                            digest + PATCH 只接受 `next`（其它 spec 键 400）
 *   P0.3 边字面量（literals 优先）            v4 = `source.spec.targets[].params` 模板：
 *                                            常量字面量 + `$(payload.…)` 引用，常量优先
 *   P0.2 源 resultSchema                      v4 = `source.spec.config.payloadSchema`
 *                                            → prune `event_schema`（入库+留痕，不派发）
 *
 * 做法（真实 HTTP + 真实协议）：
 *   - 配置（能力 / 源）走真实 HTTP API；
 *   - 节点用共享 MockNode（lib/nodes.mjs）：**两段式 v4 握手**（hello → welcome →
 *     带 appliedRevision 重连）+ 部署回执（capability.deploy → capability.deployed）；
 *     真产物的拉取/真脚本执行由 node_v4 覆盖（那边是零 mock 的真 anode）。
 *   - 不断言 sleep 出来的时序：一律「轮询 + 上限」，超时把最后一帧原样带出来。
 *
 * ======================== FINDING（产品 bug，未在本轮修）========================
 * 建源时 target 指向不存在的出口能力 → 500 InternalError（应为 400）。
 *
 *   位置：`internal/store/source.go:266-272`（checkSourceTargetsOn）返回的是**未包装的
 *     普通 error**：`fmt.Errorf("出口能力 %s 不存在", ref)`（:270），既不是
 *     `ErrNotFound` 也不是 `ErrInvalidSourceConfig`；`internal/server/api_status.go:99`
 *     的 writeAPIError 因此走不到任何已知映射，落到末尾的 500 InternalError。
 *   复现：POST /v1/namespaces/default/sources，body 的 spec.targets 写
 *     `[{ "capability": "does.not.exist" }]` → 500
 *     `{"kind":"Status","code":500,"reason":"InternalError","message":"create source: 出口能力 does.not.exist 不存在"}`。
 *   为什么是 bug：这是**客户端可修正**的校验失败（引用必须在本 ns 内可解析），按
 *     api-conventions §4.1 属于 400；`unknown_target` 已在 §4.2 的封闭集合里。
 *   本轮纪律：只钉「被拒（非 2xx）+ 消息点名出口能力」，**不把 5xx 写成契约**。
 *     产品修好后这两条应升级为 400 + reason。
 * ==============================================================================
 */
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";
import * as N from "../lib/nodes.mjs";

let server, session, sendKey, nsId, NS;
const nodes = []; // 本套件起的 mock 节点（退出前统一关连接）

// ---------- 节点：共享 MockNode（两段式握手 + 部署回执）----------
//
// 帧形状的唯一镜像在 lib/nodes.mjs（MockNode）；这里只做套件侧的编排：
// 起节点 → 触发部署 → 等 deployment 收敛成 applied。
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

// deployToNode 触发**真实**人工部署，并让 mock 节点按 v4 帧协议回执 applied。
//
// 这条路径不是「造一个 deployment 行」：中枢发 capability.deploy 帧、节点回
// capability.deployed，deployment 行才从 pending 收敛成 applied（store 侧唯一入口）。
// 真节点还会先 GET /v1/node/capabilities/{digest} 校验字节 —— 那段由 node_v4 覆盖。
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

// defineCap 造一个能力并返回信封（失败直接抛，带 body）。
async function defineCap(name, opts = {}) {
	const r = await V.createCapability(server.base, session, NS, V.capabilityBody({ name, ...opts }));
	if (r.status !== 201) throw new Error(`定义能力 ${name} 失败: ${r.status} ${String(r.text).slice(0, 300)}`);
	return r.json;
}

// capRef 把能力信封折成 hello.capabilities[] 的一条（name+version+digest）。
//
// v4 的 hello 申报的是「这台机器持有哪一版」（带 digest），不是整份信封；把信封
// 直接塞进去会在 handshake 校验里报 caps[0].name undefined —— 那正是这里的类型边界。
function capRef(doc) {
	return { name: doc?.metadata?.name, version: doc?.metadata?.version, digest: doc?.status?.digest };
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

	// 节点应收到 task.dispatch 帧（第 4 步真的挑到了它）
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

	// 4.4 出口能力解析不到：v4 在**建源**时就拒（引用必须在本 ns 内可解析）
	//
	// FINDING：当前回 500（见文件头）。这里只钉「被拒 + 消息点名」，不把 5xx 固化成契约。
	const brokenSrc = await V.createSource(
		server.base,
		session,
		NS,
		V.sourceBody({ name: "broken.demo", kind: "manual", targets: [{ capability: "does.not.exist" }] }),
	);
	H.check(
		"引用了不存在的出口能力 → 建源被拒",
		brokenSrc.status >= 400,
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

	// ③ 不可用的 payloadSchema：fail-closed（拒绝派发，但仍然是「事件照收」）
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
	V.expectStatus("③ 不可用 schema 建源 → 201（归一化不深判）", unusableSrc, 201);
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
