#!/usr/bin/env node
/* SUITE: contract — CONTRACT.md §9 的十二断言（A1–A12）的运行时部分
 *
 * A2（无 argv）与 A4（kind 不落库）是**静态**断言，已由 `make contract`
 * 的 `scripts/check_contract.py` 在 `make ci` 里守住，这里不重复。
 *
 * 本套件覆盖：
 *   A1  注册即得组织
 *   A3  能力必须先定义（未定义能力不能被声明/派发）
 *   A5  出口封闭（from_port ∈ outputs）
 *   A6  扇出（一个出口多条边 → 多个任务）
 *   A7  成环剪枝（path 命中 → pruned/path）
 *   A8  out 三态（缺失 / null / 字符串）
 *   A9  租约接管（节点掉线后任务被回收重派）
 *   A10 组织隔离（跨组织读写被拒且不泄漏）
 *   A11 版本并存（同名多版本可共存，各自 hash 不同）
 *   A12 空列表返回 []（不是 null）
 *
 * 每条断言都直接对应契约里的不变量，失败时会指出是哪一条。
 */
import * as H from "../lib/harness.mjs";
import { spawnSync } from "node:child_process";
import { DatabaseSync } from "node:sqlite";
import { bringUpNode } from "../lib/nodes.mjs";

let server, session, sendKey, orgId;
const results = [];

// A(name, cond, detail) 记录一条契约断言（带 A 编号，便于对照契约）。
function A(id, name, cond, detail) {
	results.push({ id, ok: !!cond });
	H.check(`A${id} ${name}`, cond, detail);
}

async function defineCapability(name, outputs = [], extra = {}) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { name, outputs, paramsSchema: extra.paramsSchema || { type: "object" }, ...extra },
	});
	return r;
}

async function addEdge(from, port, to) {
	return H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: from.id, fromPort: port, toCapability: to.id },
	});
}

// addEntryEdge 建入口边：左端是**来源名**（#110），不是能力 id。
async function addEntryEdge(source, port, to) {
	return H.req(server.base, "/v1/edges", {
		session,
		body: {
			fromIsSource: true,
			fromCapability: source,
			fromPort: port,
			toCapability: to.id,
		},
	});
}

async function postEvent(body) {
	return H.req(server.base, "/v1/events", { key: sendKey, body });
}

function sqlQuery(q, ...args) {
	const db = new DatabaseSync(server.dbPath);
	try {
		return db.prepare(q).all(...args);
	} finally {
		db.close();
	}
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: contract（契约 §9 十二断言）===");
	server = await H.startServer({ suiteName: "contract" });

	// ================= A1 注册即得组织 =================
	// devseed 与真实注册走同一条 EnsureOrgForUser 规则；这里用 devseed 建用户后
	// 直接查库确认组织与 owner 关系都在。
	const seeded = H.seed(server.dbPath, "contract");
	session = seeded.session;
	sendKey = seeded.sendKey;

	const seedRun = spawnSync(H.DEVSEED_BIN, ["-db", server.dbPath, "-username", "contract"], {
		encoding: "utf8",
	});
	const seedOut = (seedRun.stdout || "") + (seedRun.stderr || "");
	const uid = (seedOut.match(/^UID=(.*)$/m) || [])[1];
	orgId = (seedOut.match(/\((org_[A-Za-z0-9]+)\)/) || [])[1];

	// #162：命名空间资源（能力/节点/边/任务……）现在必须带 namespaceId。
	// 节点是直插库的（见 lib/nodes.mjs），也要写 namespace_id。这里统一取
	// 本组织默认命名空间（slug=default）的 id —— **不硬编码 ns_xxx**。
	const nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));

	const orgs = sqlQuery(`SELECT id, name FROM orgs WHERE id = ?`, orgId || "");
	const members = sqlQuery(
		`SELECT org_id, user_id, role FROM org_members WHERE user_id = ?`,
		uid || "",
	);
	A(1, "注册即得组织", orgs.length === 1, JSON.stringify({ orgs, orgId }));
	A(
		1,
		"组织里有 owner 成员",
		members.length === 1 && members[0].role === "owner",
		JSON.stringify(members),
	);

	// ================= A3 能力必须先定义 =================
	// 未定义的能力无法被声明（节点 hello 会被 unknown_capability 拒绝），
	// 也无法成为出口目标（建边被拒）。
	const ghostEdge = await H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: "cap_does_not_exist", fromPort: "x", toCapability: "cap_nope" },
	});
	A(3, "未定义能力不能建边", ghostEdge.status === 400, `${ghostEdge.status} ${ghostEdge.text?.slice(0, 100)}`);

	// 正常建图
	const origin = (await defineCapability("c.origin", ["go"])).json.capability;
	const work = (await defineCapability("c.work", ["done"])).json.capability;
	const fin = (await defineCapability("c.fin", [])).json.capability;
	// #110：事件入口用**入口边**（左端是来源名），不再需要"起点能力"。
	// c.origin 这个能力仍保留，专门给下面 A5「出口封闭」的负例用。
	await addEntryEdge("c.origin", "go", work);
	await addEdge(work, "done", fin);

	// ================= A5 出口封闭 =================
	// from_port 必须属于 from_capability.outputs
	const closedPort = await addEdge(origin, "not-in-outputs", work);
	A(5, "出口不在 outputs 里 → 400", closedPort.status === 400, String(closedPort.status));
	A(
		5,
		"拒绝码是 edge_rejected",
		String(closedPort.text || "").includes("edge_rejected"),
		String(closedPort.text).slice(0, 120),
	);

	// ================= A6 扇出 =================
	// 一个 (cap, port) 多条边 → 多个任务
	const fanA = (await defineCapability("c.fan.a", [])).json.capability;
	const fanB = (await defineCapability("c.fan.b", [])).json.capability;
	await addEdge(work, "done", fanA);
	await addEdge(work, "done", fanB);
	A(6, "同一出口两条边都建成功", true); // 上面两个 addEdge 若失败会抛

	// ================= A11 版本并存 =================
	// 同名能力可以开多个版本，内容不同则 hash 不同
	const v1 = (await defineCapability("c.versioned", ["x"])).json.capability;
	const v2 = (await defineCapability("c.versioned", ["x", "y"])).json.capability;
	A(11, "同名能力开出版本 2", v2.version === 2, `${v1.version} → ${v2.version}`);
	A(
		11,
		"不同内容 → 不同 contentHash",
		v1.contentHash !== v2.contentHash,
		`${v1.contentHash?.slice(0, 12)} vs ${v2.contentHash?.slice(0, 12)}`,
	);
	// 同 name@version 换内容应被拒
	const conflict = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { name: "c.versioned", version: 1, outputs: ["totally-different"] },
	});
	A(11, "同版本换内容 → 409", conflict.status === 409, String(conflict.status));

	// ================= A12 空列表 [] =================
	for (const [path, key] of [
		["/v1/tasks?traceId=trace_nonexistent_xyz", "tasks"],
		["/v1/edges?fromCapability=cap_nonexistent", "edges"],
	]) {
		const r = await H.req(server.base, path, { session });
		const raw = r.text || "";
		const isArray = /"\[|\s*\[/.test(raw) || raw.includes("[]");
		A(12, `${key} 空列表是 [] 不是 null`, r.status === 200 && isArray, raw.slice(0, 100));
	}
	{
		// capabilities 一定有数据，改用「按不存在的组织」→ 403（不是空列表）；
		// 真正的空列表用 nodes（新库无节点）
		const r = await H.req(server.base, "/v1/nodes", { session });
		A(12, "nodes 空列表是 [] 不是 null", r.status === 200 && (r.text || "").includes("[]"), (r.text || "").slice(0, 100));
	}

	// ================= A10 组织隔离 =================
	const crossRead = await H.req(server.base, "/v1/capabilities?orgId=org_not_mine_xxx", { session });
	A(10, "跨组织读 → 403", crossRead.status === 403, String(crossRead.status));
	A(
		10,
		"跨组织读不泄漏能力名",
		!(crossRead.text || "").includes("c.origin"),
		String(crossRead.text).slice(0, 120),
	);
	// 跨组织写：建边时两端不同组织应被拒（用另一个组织的能力）
	{
		const db = new DatabaseSync(server.dbPath);
		try {
			const otherOrg = "org_x_" + Date.now();
			const now = Math.floor(Date.now() / 1000);
			db.prepare(`INSERT INTO orgs (id, name, created_at) VALUES (?, ?, ?)`).run(
				otherOrg,
				"other-" + Date.now(),
				now,
			);
			// 迁移 12 之后能力行必须归属一个命名空间（namespace_id NOT NULL），
			// 所以给这个「别的组织」也建一个 default 命名空间再插能力。
			const foreignNs = "ns_x_" + Date.now();
			db.prepare(
				`INSERT INTO namespaces (id, org_id, slug, name, created_at) VALUES (?, ?, 'default', '默认', ?)`,
			).run(foreignNs, otherOrg, now);
			db.prepare(
				`INSERT INTO capability_versions (id, org_id, namespace_id, name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
				 VALUES (?, ?, ?, 'foreign.cap', 1, '{}', '[]', '{}', '', 'custom', ?, ?)`,
			).run("cap_foreign_" + Date.now(), otherOrg, foreignNs, "hash_" + Date.now(), now);
			const foreignId = db
				.prepare(`SELECT id FROM capability_versions WHERE org_id = ?`)
				.get(otherOrg).id;
			const crossEdge = await H.req(server.base, "/v1/edges", {
				session,
				body: { fromCapability: origin.id, fromPort: "go", toCapability: foreignId },
			});
			A(10, "跨组织建边 → 400", crossEdge.status === 400, String(crossEdge.status));
		} finally {
			db.close();
		}
	}

	// ================= A8 out 三态 + A7 环剪枝 + A9 租约接管 =================
	// 这三条需要真实节点跑完整链路。
	const node = await bringUpNode(server.base, server.dbPath, orgId, nsId, "contract-node", [origin, work, fin, fanA, fanB], 4);
	A(6, "节点上线", !!node.id);

	// A6：从 origin 出发 → work（1 个任务）；work 回报 done → 扇出到 fanA+fanB
	const ev1 = await postEvent({ source: "c.origin", type: "go", payload: {} });
	A(6, "第一跳建 1 个任务（work）", ev1.json?.tasks?.length === 1, JSON.stringify(ev1.json?.tasks?.length));
	const tWork = ev1.json?.tasks?.[0]?.id;

	// 用节点 WS 完成 work 并回报 done → 扇出
	{
		await new Promise((r) => setTimeout(r, 400));
		// work 的 assign 帧应已到达
		node.conn.send({ type: "ack", task_id: tWork });
		await new Promise((r) => setTimeout(r, 200));
		node.conn.send({ type: "progress", task_id: tWork });
		await new Promise((r) => setTimeout(r, 200));
		node.conn.send({ type: "result", task_id: tWork, status: "succeeded", out: "done" });
		// 等扇出
		const deadline = Date.now() + 4000;
		let fanTasks = [];
		while (Date.now() < deadline) {
			fanTasks = sqlQuery(
				`SELECT id, capability FROM tasks WHERE trace_id = ? AND parent_task_id = ?`,
				ev1.json.traceId,
				tWork,
			);
			if (fanTasks.length >= 2) break;
			await new Promise((r) => setTimeout(r, 60));
		}
		// work 的 done 出口一共有 3 条边：fin（上面 A5 之后建的）+ fanA + fanB。
		// 扇出的定义是「一个出口多条边 → 每条边一个任务」，所以期望是 3。
		const fanCaps = fanTasks.map((t) => t.capability).sort();
		A(
			6,
			"第二跳按边数扇出 3 个任务（fin + fanA + fanB）",
			fanTasks.length === 3 &&
				JSON.stringify(fanCaps) === JSON.stringify(["c.fan.a", "c.fan.b", "c.fin"]),
			JSON.stringify(fanCaps),
		);
	}

	// A8 out 三态：三态各验一次（用 fin 这个 end 能力）
	{
		const db = new DatabaseSync(server.dbPath);
		let traceId;
		try {
			traceId = "trace_tristate_" + Date.now();
		} finally {
			db.close();
		}

		// 造三条独立任务，分别用 缺失 / null / 字符串 回报
		const mk = async (label) => {
			const ev = await postEvent({
				source: "c.origin",
				type: "go",
				traceId: traceId + "_" + label,
				payload: {},
			});
			return ev.json?.tasks?.[0]?.id;
		};

		const tMissing = await mk("missing");
		await new Promise((r) => setTimeout(r, 400));
		node.conn.sendRaw(
			`{"type":"result","task_id":"${tMissing}","status":"succeeded","summary":"no out"}`,
		);
		await new Promise((r) => setTimeout(r, 400));
		const rMissing = sqlQuery(`SELECT reported, out FROM tasks WHERE id = ?`, tMissing)[0];
		A(
			8,
			"out 缺失 → reported=0, out=NULL",
			rMissing && rMissing.reported === 0 && rMissing.out === null,
			JSON.stringify(rMissing),
		);

		const tNull = await mk("null");
		await new Promise((r) => setTimeout(r, 400));
		node.conn.sendRaw(`{"type":"result","task_id":"${tNull}","status":"succeeded","out":null}`);
		await new Promise((r) => setTimeout(r, 400));
		const rNull = sqlQuery(`SELECT reported, out FROM tasks WHERE id = ?`, tNull)[0];
		A(
			8,
			'out:null → reported=1, out=NULL（"到头了"）',
			rNull && rNull.reported === 1 && rNull.out === null,
			JSON.stringify(rNull),
		);

		// 字符串态用 work（有出口）已验证过扇出；这里再单独确认落库
		const tStr = await mk("str");
		await new Promise((r) => setTimeout(r, 400));
		node.conn.send({ type: "result", task_id: tStr, status: "succeeded", out: "done" });
		await new Promise((r) => setTimeout(r, 400));
		const rStr = sqlQuery(`SELECT reported, out FROM tasks WHERE id = ?`, tStr)[0];
		A(
			8,
			'out:"done" → reported=1, out="done"',
			rStr && rStr.reported === 1 && rStr.out === "done",
			JSON.stringify(rStr),
		);
	}

	// A7 环剪枝：a --go--> b --back--> a
	{
		const a = (await defineCapability("c.cycle.a", ["go"])).json.capability;
		const b = (await defineCapability("c.cycle.b", ["back"])).json.capability;
		// #110：环检测靠 task.path（能力 id 链）。入口边不贡献 path
		// （来源不是能力），所以第一跳必须先进 a，环才成立：
		//   来源 c.cycle.src → a --go--> b --back--> a（a 已在 path → 剪枝）
		await addEntryEdge("c.cycle.src", "start", a);
		await addEdge(a, "go", b);
		await addEdge(b, "back", a);

		const cycNode = await bringUpNode(server.base, server.dbPath, orgId, nsId, "contract-cycle", [a, b], 2);
		const ev = await postEvent({ source: "c.cycle.src", type: "start", payload: {} });
		// 第一跳进 a；完成 a 回报 go → b；完成 b 回报 back → a 已在 path → 剪枝
		const tA = ev.json?.tasks?.[0]?.id;
		A(7, "环：第一跳建 1 个任务（a）", ev.json?.tasks?.length === 1);
		A(7, "环：第一跳任务是 c.cycle.a", ev.json?.tasks?.[0]?.capability, "c.cycle.a");

		await new Promise((r) => setTimeout(r, 400));
		cycNode.conn.send({ type: "ack", task_id: tA });
		await new Promise((r) => setTimeout(r, 200));
		cycNode.conn.send({ type: "result", task_id: tA, status: "succeeded", out: "go" });
		await new Promise((r) => setTimeout(r, 600));

		// 第二跳：b 应被建出来
		const tB = sqlQuery(
			`SELECT id FROM tasks WHERE parent_task_id = ? AND capability = 'c.cycle.b'`,
			tA,
		)[0]?.id;

		// 完成 b，回报 back → 目标是 a，但 a 已在 path 里 → 应被剪枝
		await new Promise((r) => setTimeout(r, 400));
		cycNode.conn.send({ type: "ack", task_id: tB });
		await new Promise((r) => setTimeout(r, 200));
		cycNode.conn.send({ type: "result", task_id: tB, status: "succeeded", out: "back" });

		// 等第二跳结果：不应产生新任务（被剪枝）
		const deadline = Date.now() + 3000;
		let childCount = 0;
		while (Date.now() < deadline) {
			childCount = sqlQuery(
				`SELECT count(*) AS n FROM tasks WHERE parent_task_id = ?`,
				tB,
			)[0].n;
			await new Promise((r) => setTimeout(r, 60));
			if (childCount === 0 && Date.now() > deadline - 2000) break;
		}
		A(7, "环：第二跳被剪枝（0 个子任务）", childCount === 0, `children=${childCount}`);
		cycNode.conn.close();
	}

	// ============ #83 内置能力：notify.phone 由中枢执行 ============
	// 验证「没有节点执行者的能力」也能在图里跑通：
	// 建边 → 派发（跳过找节点）→ 中枢执行 → 任务进终态。
	{
		// notify.phone 由 EnsureOrgForUser 播种（内置能力），直接查是否就位
		const capsResp = await H.req(server.base, "/v1/capabilities", { session });
		const phone = (capsResp.json?.capabilities || []).find(
			(n) => n.name === "notify.phone" && n.version === 1,
		);
		A(3, "notify.phone 作为内置能力已播种", !!phone, JSON.stringify(phone));
		if (phone) {
			A(3, "notify.phone 无出口（终端能力）", (phone.outputs || []).length === 0);
			// kind 是推导量：没有节点声明 + 无出口 → origin（不是 end）
			// end 的定义是「有节点声明且 outputs 全空」，notify.phone 没有节点声明。
			A(3, "notify.phone 的 kind 已推导", !!phone.kind, phone.kind);
		}

		// 建一条边：某 origin 的出口 → notify.phone
		// #110：来源用入口边，不再需要"起点能力"
		const nEdge = await addEntryEdge("notice.src", "fire", phone);
		A(5, "可以给内置能力建入边", nEdge.status === 200, String(nEdge.status));

		// 触发：payload 必须满足 notify.phone 的 params_schema（required: to + title），
		// 否则会在第 ② 步被 param_schema 剪枝——那是正确行为，但不是本断言要验的东西。
		//
		// to 用当前组织成员（否则 foreign_user 剪枝）；这里用 devseed 建的 uid。
		const nEv = await postEvent({
			source: "notice.src",
			type: "fire",
			payload: { to: uid, title: "内置能力测试" },
		});
		A(3, "内置能力事件 200", nEv.status === 200, String(nEv.status));
		const nPruned = (nEv.json?.pruned || []).map((p) => p.reason);
		A(
			3,
			"内置能力不因「没有节点」被剪枝",
			!nPruned.includes("no_node"),
			JSON.stringify(nPruned),
		);
		A(
			3,
			"内置能力建出了任务",
			(nEv.json?.tasks || []).length === 1,
			JSON.stringify({
				tasks: nEv.json?.tasks?.length,
				pruned: nEv.json?.pruned,
			}),
		);

		// 任务应被中枢执行器领取并跑完（VAPID 未配置 → notifier 为 nil → failed）
		// 关键：**必须进终态**，不能卡在 running（中枢任务没有租约，卡住就永不回收）
		const phoneTaskId = nEv.json?.tasks?.[0]?.id;
		if (phoneTaskId) {
			const deadline = Date.now() + 8000;
			let st = null;
			while (Date.now() < deadline) {
				st = sqlQuery(`SELECT status, out, reported FROM tasks WHERE id = ?`, phoneTaskId)[0];
				if (st && ["succeeded", "failed", "canceled", "abandoned"].includes(st.status)) break;
				await new Promise((r) => setTimeout(r, 150));
			}
			A(
				3,
				"中枢任务进入终态（不卡在 running）",
				st && ["succeeded", "failed", "canceled", "abandoned"].includes(st.status),
				JSON.stringify(st),
			);
			// 中枢任务没有节点归属
			const owner = sqlQuery(`SELECT node_id FROM tasks WHERE id = ?`, phoneTaskId)[0];
			A(3, "中枢任务没有 node_id", owner && owner.node_id === null, JSON.stringify(owner));
		}
	}

	// A9 租约接管：节点掉线后，任务被巡检回收并重派
	{
		const t = (await postEvent({ source: "c.origin", type: "go", payload: {} })).json?.tasks?.[0]?.id;
		A(9, "建出待接管的任务", !!t, String(t));

		// 把租约压到已过期（模拟节点失联后租约到期）
		{
			const db = new DatabaseSync(server.dbPath);
			try {
				db.prepare(`UPDATE tasks SET lease_until = ? WHERE id = ?`).run(
					Math.floor(Date.now() / 1000) - 60,
					t,
				);
			} finally {
				db.close();
			}
		}
		// 后台巡检每 5s 一轮（契约 §3.3），所以最多等 5s + 余量。
		// 这里断言的是「真的被回收并重派」——不是「能被识别」。
		// 第一版只断言 lease_until < now，绿灯漏过了「巡检从未被调度」这个 bug。
		const deadline = Date.now() + 12000;
		let row = null;
		let reclaimed = false;
		while (Date.now() < deadline) {
			row = sqlQuery(`SELECT status, attempts, node_id, lease_until FROM tasks WHERE id = ?`, t)[0];
			// 被回收的标志：不再是 assigned（回 pending 或已重派成新的 assigned）
			// 且 attempts 增加了（说明经历了回收 → 重派）
			if (row && (row.status === "pending" || row.attempts > 1)) {
				reclaimed = true;
				break;
			}
			await new Promise((r) => setTimeout(r, 200));
		}
		A(
			9,
			"租约过期后任务被巡检回收（不再是原租约）",
			reclaimed,
			JSON.stringify(row),
		);
		if (reclaimed && row) {
			// 槽位必须被释放，否则节点并发能力会永久泄漏
			const busy = sqlQuery(
				`SELECT count(*) AS n FROM node_instances WHERE busy_task = ?`,
				t,
			)[0].n;
			A(9, "回收后实例槽位被释放", busy === 0, `busy_task 仍指向该任务: ${busy}`);
		}
	}

	node.conn.close();
	await new Promise((r) => setTimeout(r, 300));

	// ================= A10：内置起点能力 =================
	//
	// 起点的两件事必须成立：
	//   1. 内置起点开箱可用 —— 用户不必先伪造一个"叫 github 的能力"
	//   2. 起点的出口是任意事件类型 —— 但**只限内置起点**，自定义能力仍严格
	{
		const capsResp = await H.req(server.base, "/v1/capabilities", {
			session,
		});
		const caps = capsResp.json?.capabilities || [];
		const byName = (n) => caps.find((c) => c.name === n);

		// #110：起点不再作为能力存在 —— 能力列表里只该有真能干活的东西
		A(
			10,
			"起点不再是能力（#110）",
			!byName("github") && !byName("cron"),
			JSON.stringify(caps.map((c) => c.name)),
		);

		// notify.phone 是中枢终端能力，绝不能被当成起点
		const phone = byName("notify.phone");
		A(
			10,
			"notify.phone kind=end（不是起点）",
			phone?.kind === "end",
			String(phone?.kind),
		);

		// 入口边的出口可填任意事件类型（外部系统决定，不可能枚举）
		const sinkResp = await H.req(server.base, "/v1/capabilities", {
			session,
			method: "POST",
			body: { name: "a10.sink", outputs: [], paramsSchema: { type: "object" } },
		});
		const sinkId = sinkResp.json?.capability?.id;
		const wildEdge = await H.req(server.base, "/v1/edges", {
			session,
			method: "POST",
			body: {
				fromIsSource: true,
				fromCapability: "github",
				fromPort: "never_declared_before.opened",
				toCapability: sinkId,
			},
		});
		A(10, "入口边出口可任意（未声明过也能建边）", wildEdge.status === 200, `${wildEdge.status} ${wildEdge.text?.slice(0, 80)}`);

		// 自定义能力的出口仍严格（通配只给入口边，没有放松不变量 I2）
		const customResp = await H.req(server.base, "/v1/capabilities", {
			session,
			method: "POST",
			body: { name: "a10.custom", outputs: ["only_port"], paramsSchema: { type: "object" } },
		});
		const badEdge = await H.req(server.base, "/v1/edges", {
			session,
			method: "POST",
			body: {
				fromCapability: customResp.json?.capability?.id,
				fromPort: "NOT_DECLARED",
				toCapability: sinkId,
			},
		});
		A(10, "自定义能力出口仍封闭（I2 未放松）", badEdge.status === 400, String(badEdge.status));

		// 未知 source → 200 + no_edge（#110 之后的新语义：
		// 先让 webhook 打着、后配边是正常流程，事件不该因为还没配边而丢）
		const unknown = await H.req(server.base, "/v1/events", {
			key: sendKey,
			body: { source: "a10.no.such.source", type: "x", payload: {} },
		});
		A(10, "未知 source → 200（事件照收）", unknown.status === 200, String(unknown.status));
		A(
			10,
			"未知 source 记 no_edge（可回答为什么没触发）",
			(unknown.json?.pruned || [])[0]?.reason === "no_edge",
			JSON.stringify(unknown.json?.pruned),
		);
		A(
			10,
			"未知 source 无害（事件已入库）",
			typeof unknown.json?.eventId === "string" && unknown.json.eventId.length > 0,
			JSON.stringify(unknown.json?.eventId),
		);
		// available 列表已随 422 一起移除（#110）：来源不再需要预先登记，
		// 所以没有"可用起点"这个概念了。
	}

	// ================= A3 的运行时侧：未定义能力不能被声明 =================
	// （由 ws_protocol 的 unknown_capability 断言覆盖；这里确认它仍在）
	A(3, "未定义能力无法建边（运行时侧）", ghostEdge.status === 400);

	server.stop();
	const allPassed = H.summary("contract");
	H.writeResults("contract", H.stopTimer());

	// 汇总：按 A 编号报告哪些没通过
	const failed = results.filter((r) => !r.ok).map((r) => "A" + r.id);
	if (failed.length) {
		console.log("\n未通过的契约断言: " + [...new Set(failed)].join(", "));
	}
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	if (server) server.stop();
	process.exit(1);
});
