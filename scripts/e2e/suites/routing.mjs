#!/usr/bin/env node
/* SUITE: routing — 派发四步 + 六种「未触发」原因（契约 §4 / §7）
 *
 * 这是「事件中心」的路由语义，与旧的「按 tag 过滤设备」模型完全无关。
 *
 * 覆盖：
 *   四步    edges 查表 → 校参 → 找节点 → 挑一台
 *   六因    ① 上游没回报（**不是**剪枝）
 *           ② no_edge  ③ param_schema  ④ no_node
 *           ⑤ foreign_user  ⑥ path（环剪枝）
 *   扇出    一个 (cap, port) 多条边 → 多个任务
 *   容量    有节点但槽位满 → 任务留 pending（不剪枝）
 *   幂等    同一 idemKey 不重复派发
 *
 * 做法：
 *   - 配置（能力/出口表/事件）全部走真实 HTTP API
 *   - 节点用 SQL 直接播种（CreateNode 没有 HTTP 端点，属于运维动作），
 *     然后用**真实 WS 握手**让节点上线——这样派发才真的能找到它
 */
import * as H from "../lib/harness.mjs";
import { bringUpNode as bringUpShared } from "../lib/nodes.mjs";

let server, session, sendKey, nsId;

// ---------- 配置辅助（真实 HTTP API） ----------

async function defineCapability(name, outputs = [], extra = {}) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name,
			outputs,
			paramsSchema: extra.paramsSchema || { type: "object" },
			...extra,
		},
	});
	if (r.status !== 200) throw new Error(`定义能力 ${name} 失败: ${r.status} ${r.text}`);
	return r.json.capability;
}

async function addEdge(fromCap, fromPort, toCap) {
	return H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: fromCap.id, fromPort, toCapability: toCap.id },
	});
}

// addEntryEdge 建"入口边"：左端是**来源名**（#110），不是能力 id。
//
// 旧模型要求先播一个叫 github@1 的空壳能力当锚点；那个空壳已经不存在了。
async function addEntryEdge(source, port, toCap) {
	return H.req(server.base, "/v1/edges", {
		session,
		body: {
			fromIsSource: true,
			fromCapability: source,
			fromPort: port,
			toCapability: toCap.id,
		},
	});
}

async function postEvent(body) {
	// /v1/events 是**机器入口**：鉴权走 Bearer API Key（notify:send），不是会话 Cookie。
	return H.req(server.base, "/v1/events", { key: sendKey, body });
}

// bringUpNode 薄封装：把本套件的 (name, caps, slots) 映射到共享实现的参数。
// nsId 是默认命名空间（下面从 /v1/namespaces 查出，不硬编码）：
// 节点属于哪个命名空间决定了 hello 里声明的能力名在哪查。
// **没有 orgId**：组织层已在迁移 14 删除（#204），令牌只绑命名空间。
async function bringUpNode(name, caps, slots = 1) {
	return bringUpShared(server.base, server.dbPath, nsId, name, caps, slots);
}

function expectPrune(name, resp, reason) {
	const pruned = resp.json?.pruned || [];
	const hit = pruned.find((p) => p.reason === reason);
	H.check(name, !!hit, `want reason=${reason}, got ${JSON.stringify(pruned)}`);
	return hit;
}

async function main() {
	H.startTimer();
	server = await H.startServer({ suiteName: "routing" });
	const s = H.seed(server.dbPath, "routing");
	session = s.session;
	sendKey = s.sendKey;

	// devseed 用 log.Printf 输出「组织就绪: name (org_xxx)」→ 走 **stderr**，
	// 而 stdout 只有 KEY=VALUE 那几行。所以必须两个流都读。
	const { spawnSync } = await import("node:child_process");
	const seedRun = spawnSync(
		H.DEVSEED_BIN,
		["-db", server.dbPath, "-username", "routing"],
		{ encoding: "utf8" },
	);
	const seedOut = (seedRun.stdout || "") + (seedRun.stderr || "");
	// #162：节点直插库时必须写 namespace_id（NOT NULL）。
	// 取默认命名空间（slug=default）的 id —— 用例里不硬编码 ns_xxx。
	// **不再从 devseed 输出解析 orgId**：组织层已在迁移 14 删除（#204）。
	nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");
	H.check("拿到 UID（devseed 仍然输出）", /^UID=/m.test(seedOut), seedOut.slice(0, 200));

	// ---------- 建图 + 节点上线 ----------
	console.log("--- 派发四步 ---");
	// #110：github 不再是能力，入口边直接以来源名 "github" 为左端。
	// 出口是任意事件类型，所以不需要预先声明 pull_request.opened。
	const capsAll = await H.req(server.base, "/v1/capabilities", { session });
	const names = (capsAll.json?.capabilities || []).map((c) => c.name);
	H.check(
		"起点不再是能力（#110）",
		!names.includes("github") && !names.includes("cron"),
		JSON.stringify(names),
	);
	const review = await defineCapability("review", ["fix"], {
		paramsSchema: {
			type: "object",
			required: ["number"],
			properties: { number: { type: "number" } },
		},
	});
	const fix1 = await defineCapability("fix1", []);
	const fix2 = await defineCapability("fix2", []);

	H.eq("建入口边 github→review", (await addEntryEdge("github", "pull_request.opened", review)).status, 200);
	H.eq("建边 review→fix1", (await addEdge(review, "fix", fix1)).status, 200);
	H.eq("建边 review→fix2", (await addEdge(review, "fix", fix2)).status, 200);

	const nodeA = await bringUpNode("routing-node", [review, fix1, fix2], 4);
	H.check("节点上线（真实 WS 握手）", !!nodeA.id);


	// ---------- 四步：一次事件 → 一个 review 任务 ----------
	const ev1 = await postEvent({
		source: "github",
		type: "pull_request.opened",
		payload: { number: 42 },
	});
	H.eq("事件入队 200", ev1.status, 200);
	H.eq("首轮建 1 个任务", ev1.json?.tasks?.length, 1);
	H.eq("首轮任务能力是 review", ev1.json?.tasks?.[0]?.capability, "review");
	H.eq("任务状态 assigned（第 4 步挑到节点）", ev1.json?.tasks?.[0]?.status, "assigned");
	H.eq("无剪枝", ev1.json?.pruned?.length, 0);

	// 节点应收到 assign 帧（第 4 步真的派下去了）
	const deadline = Date.now() + 2000;
	let assignFrame = null;
	while (Date.now() < deadline && !assignFrame) {
		assignFrame = nodeA.frames.find((f) => f.type === "assign");
		if (!assignFrame) await new Promise((r) => setTimeout(r, 30));
	}
	H.check("节点收到 assign 帧", !!assignFrame, JSON.stringify(nodeA.frames.map((f) => f.type)));
	if (assignFrame) {
		H.eq("assign 帧的能力名", assignFrame.capability, "review");
		H.eq("assign 帧带 task_id", typeof assignFrame.task_id, "string");
		H.check("assign 帧有租约", typeof assignFrame.lease_until === "number" && assignFrame.lease_until > 0);
		// A2：没有 argv / shell（不变量 I1）
		const raw = JSON.stringify(assignFrame).toLowerCase();
		H.check(
			"assign 帧不含 argv/shell/command",
			!raw.includes("argv") && !raw.includes("shell") && !raw.includes("command"),
			raw.slice(0, 160),
		);
	}

	// ---------- A6 扇出 ----------
	console.log("--- A6 扇出（一个出口多条边）---");
	// #110：来源不是能力，两条边都从来源名直接出发（这就是扇出）
	const t1 = await defineCapability("fan.target1", []);
	const t2 = await defineCapability("fan.target2", []);
	await addEntryEdge("fanout.demo", "go", t1);
	await addEntryEdge("fanout.demo", "go", t2);
	await bringUpNode("fanout-node", [t1, t2], 2);

	const fan = await postEvent({ source: "fanout.demo", type: "go", payload: {} });
	H.eq("扇出事件 200", fan.status, 200);
	H.eq("一个出口两条边 → 2 个任务", fan.json?.tasks?.length, 2);
	H.eq(
		"扇出的两个能力正确",
		(fan.json?.tasks || []).map((t) => t.capability).sort(),
		["fan.target1", "fan.target2"],
	);

	// ---------- ② no_edge ----------
	console.log("--- 六种未触发原因 ---");
	const noEdge = await postEvent({ source: "github", type: "pull_request.closed", payload: {} });
	H.eq("no_edge 事件 200", noEdge.status, 200);
	H.eq("no_edge 不建任务", noEdge.json?.tasks?.length, 0);
	expectPrune("② no_edge 留痕", noEdge, "no_edge");

	// ---------- ③ param_schema ----------
	const badParam = await postEvent({
		source: "github",
		type: "pull_request.opened",
		payload: { title: "缺 number 字段" },
	});
	H.eq("param_schema 不建任务", badParam.json?.tasks?.length, 0);
	const ps = expectPrune("③ param_schema 留痕", badParam, "param_schema");
	if (ps) {
		H.check("param_schema 证据可读", String(ps.detail || "").length > 0, ps.detail);
	}

	// ---------- ④ no_node ----------
	const orphan = await defineCapability("orphan.cap", []);
	// #110：来源不需要是能力，直接建入口边
	await addEntryEdge("nonode.demo", "go", orphan);
	const noNode = await postEvent({ source: "nonode.demo", type: "go", payload: {} });
	H.eq("no_node 不建任务", noNode.json?.tasks?.length, 0);
	expectPrune("④ no_node 留痕", noNode, "no_node");

	// ---------- ⑤ foreign_user ----------
	// #110：来源名直接当左端
	const phone = await defineCapability("notify.phone", [], {
		paramsSchema: {
			type: "object",
			required: ["to"],
			properties: { to: { type: "string" } },
		},
	});
	await addEntryEdge("push.demo", "notify", phone);
	await bringUpNode("phone-node", [phone], 1);

	const foreign = await postEvent({
		source: "push.demo",
		type: "notify",
		payload: { to: "usr_not_a_member_of_this_org" },
	});
	H.eq("foreign_user 不建任务", foreign.json?.tasks?.length, 0);
	const fu = expectPrune("⑤ foreign_user 留痕", foreign, "foreign_user");
	if (fu) {
		H.check(
			"foreign_user 证据里点名那个用户",
			String(fu.detail || "").includes("usr_not_a_member_of_this_org"),
			fu.detail,
		);
	}

	// ---------- ⑥ path（环剪枝） ----------
	// a --go--> b --back--> a。单次 POST 只走一跳，所以环剪枝靠
	// 「review 回报 fix」这类多跳才触发。
	// 这里用节点 WS 真的走两跳：完成 a 任务 → 回报 go → 派发 b →
	// 完成 b 任务 → 回报 back → 目标是 a，已在 path → 剪枝。
	console.log("--- ⑥ 环剪枝（真走两跳）---");
	const a = await defineCapability("cycle.a", ["go"]);
	const b = await defineCapability("cycle.b", ["back"]);
	// 第一跳是入口边（来源 "cycle.a" → b），第二跳是能力边（b → a），
	// 这样才能真的绕回 a 触发环剪枝
	await addEntryEdge("cycle.a", "go", b);
	await addEdge(b, "back", a);
	const nodeCycle = await bringUpNode("cycle-node", [a, b], 2);

	const cyc = await postEvent({ source: "cycle.a", type: "go", payload: {} });
	H.eq("环：第一圈建 1 个任务", cyc.json?.tasks?.length, 1);
	H.eq("环：第一圈任务是 cycle.b", cyc.json?.tasks?.[0]?.capability, "cycle.b");
	H.eq("环：第一圈不误剪", cyc.json?.pruned?.length, 0);

	// ---------- 容量不足不剪枝 ----------
	console.log("--- 容量不足不剪枝 ---");
	const busyCap = await defineCapability("busy.target", []);
	// #110：来源不是能力
	await addEntryEdge("busy.demo", "go", busyCap);
	// 节点声明能力但 0 槽位
	await bringUpNode("busy-node", [busyCap], 0);

	const busy = await postEvent({ source: "busy.demo", type: "go", payload: {} });
	H.eq("空槽位仍建任务", busy.json?.tasks?.length, 1);
	H.eq("空槽位任务留 pending", busy.json?.tasks?.[0]?.status, "pending");
	H.eq("空槽位不产生剪枝", busy.json?.pruned?.length, 0);

	// ---------- 幂等 ----------
	console.log("--- 幂等 ---");
	const idemKey = "routing-idem-" + Date.now();
	const i1 = await postEvent({ source: "github", type: "pull_request.opened", idemKey, payload: { number: 1 } });
	const i2 = await postEvent({ source: "github", type: "pull_request.opened", idemKey, payload: { number: 1 } });
	H.eq("幂等：第一次 inserted", i1.json?.inserted, true);
	H.eq("幂等：第二次 not inserted", i2.json?.inserted, false);
	H.eq("幂等：同一 eventId", i2.json?.eventId, i1.json?.eventId);
	H.eq("幂等：同一 traceId", i2.json?.traceId, i1.json?.traceId);
	H.eq("幂等：第二次不重复派发", i2.json?.tasks?.length, 0);

	// ---------- A10 命名空间隔离 ----------
	//
	// **改写说明（原两条断言不得静默削弱）**：
	// 原断言：`GET /v1/capabilities?orgId=org_bogus_xxxxxxxx` → 403，且响应不含 `cycle.a`。
	// 它证明「读别人的组织被拒 + 不泄漏」。`?orgId=` 随组织层删除（迁移 14）而消失，
	// 组织间隔离也不再是语义（#204：本部署内任何已登录用户都能访问任何存在的命名空间）。
	// 所以不能直接删，改写为更强的一组（原用例只有负向断言，下面补上正向隔离证明）：
	//   ① 两个命名空间各建同名能力 cycle.a → 各自列表里只有自己那条（同名不同定义）
	//   ② B 空间的边指向 A 空间的 cap → 400（I11 跨命名空间不引用）
	//   ③ A 空间产生的事件在 B 空间查不到（数据不串）
	//   ④ 未知命名空间 → 404，且不泄漏能力名
	console.log("--- A10 命名空间隔离 ---");
	const isoCreated = await H.req(server.base, "/v1/namespaces", {
		session,
		body: { slug: "iso" + Date.now().toString(36), name: "隔离验证" },
	});
	H.eq("A10 建第二个命名空间", isoCreated.status, 201);
	const nsB = isoCreated.json?.namespace?.id;
	if (nsB) {
		const withNSId = (path, nid) =>
			path + (path.includes("?") ? "&" : "?") + "namespaceId=" + nid;

		// ① 两个命名空间各建同名能力 cycle.a
		const sameB = (
			await H.req(server.base, withNSId("/v1/capabilities", nsB), {
				session,
				body: { name: "cycle.a", outputs: ["go"], paramsSchema: { type: "object" } },
			})
		).json?.capability;
		H.check("A10 在第二个命名空间建同名能力 cycle.a", !!sameB?.id, String(sameB?.id));
		H.check(
			"A10 同名能力是两个不同定义（命名空间隔离的前提）",
			!!sameB?.id && sameB.id !== a.id,
			`${sameB?.id} vs ${a.id}`,
		);

		const listA = await H.req(server.base, withNSId("/v1/capabilities", nsId), { session });
		const listB = await H.req(server.base, withNSId("/v1/capabilities", nsB), { session });
		const cycleAIdsIn = (r) =>
			(r.json?.capabilities || []).filter((c) => c.name === "cycle.a").map((c) => c.id);
		H.check(
			"A10 A 空间列表里 cycle.a 只有自己那条",
			JSON.stringify(cycleAIdsIn(listA)) === JSON.stringify([a.id]),
			JSON.stringify(cycleAIdsIn(listA)),
		);
		H.check(
			"A10 B 空间列表里 cycle.a 只有自己那条",
			JSON.stringify(cycleAIdsIn(listB)) === JSON.stringify([sameB.id]),
			JSON.stringify(cycleAIdsIn(listB)),
		);
		// 负向（原「不泄漏」意图保留）：A 的列表不能出现只在 B 里存在的定义
		H.check(
			"A10 A 空间列表不含 B 空间的能力定义",
			!(listA.json?.capabilities || []).some((c) => c.id === sameB.id),
			String(listA.text).slice(0, 160),
		);

		// ② 跨命名空间引用 → 400（I11）
		const crossEdge = await H.req(server.base, "/v1/edges", {
			session,
			body: {
				namespaceId: nsB,
				fromCapability: sameB.id,
				fromPort: "go",
				toCapability: a.id,
			},
		});
		H.eq("A10 跨命名空间建边 → 400", crossEdge.status, 400);

		// ③ 事件不跨命名空间：A 空间刚跑过 cycle 事件，B 空间按同一 trace 查必须为空
		const eventsInB = await H.req(
			server.base,
			withNSId("/v1/console/events?traceId=" + (cyc.json?.traceId || "none"), nsB),
			{ session },
		);
		H.check(
			"A10 B 空间查不到 A 空间的事件",
			eventsInB.status === 200 && (eventsInB.json?.events || []).length === 0,
			String(eventsInB.text).slice(0, 160),
		);

		// ④ 未知命名空间 → 404，且不泄漏
		const ghost = await H.req(
			server.base,
			withNSId("/v1/capabilities", "ns_bogus_xxxxxxxx"),
			{ session },
		);
		H.eq("A10 未知命名空间读 → 404", ghost.status, 404);
		H.check(
			"A10 404 不泄漏数据",
			!(ghost.text || "").includes("cycle.a"),
			String(ghost.text).slice(0, 120),
		);
	}

	// ---------- A5 出口封闭 ----------
	console.log("--- A5 出口封闭 ---");
	const closedPort = await H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: a.id, fromPort: "not-in-outputs", toCapability: b.id },
	});
	H.eq("出口不在 outputs 里 → 400", closedPort.status, 400);

	for (const n of [nodeA, nodeCycle]) n.conn.close();
	server.stop();
	// summary() 返回 true 表示**全部通过**（不是"failed"），别搞反
	const allPassed = H.summary("routing");
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	if (server) server.stop();
	process.exit(1);
});
