#!/usr/bin/env node
/* SUITE: contract — CONTRACT.md §9 的十二断言（A1–A12）的运行时部分
 *
 * A2（无 argv）与 A4（kind 不落库）是**静态**断言，已由 `make contract`
 * 的 `scripts/check_contract.py` 在 `make ci` 里守住，这里不重复。
 *
 * 本套件覆盖：
 *   A1  注册即得 default 命名空间（组织层已在迁移 14 删除）
 *   A3  能力必须先定义（未定义能力不能被声明/派发）
 *   A5  出口封闭（from_port ∈ outputs）
 *   A5b 能力 + 下游边**一次提交**（POST /capabilities 带 links）：请求/响应形状 + 原子性
 *       （坏边 → 400 且能力根本没建出来；自连自契约 §2 起合法）
 *   A6  扇出（一个出口多条边 → 多个任务）
 *   A7  成环（环合法：第二圈真的能跑；兜底是 hop 预算 → pruned/max_hops）
 *   A8  out 退役（P1.5 + 迁移 28）：result 帧里的 out 不再路由；产出只走 emit；
 *       **API 响应里没有 out 字段**（列已删）
 *   A9  租约接管（节点掉线后任务被回收重派）
 *   A10 命名空间隔离（同名能力互不可见 / 事件与任务不串 / 跨命名空间引用 400 /
 *       未知命名空间 404）
 *   A11 版本并存（同名多版本可共存，各自 hash 不同）
 *   A12 空列表返回 []（不是 null）
 *
 * 每条断言都直接对应契约里的不变量，失败时会指出是哪一条。
 */
import * as H from "../lib/harness.mjs";
import { spawnSync } from "node:child_process";
import { DatabaseSync } from "node:sqlite";
import { bringUpNode } from "../lib/nodes.mjs";

let server, session, sendKey;
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

async function addEdge(from, port, to, priority) {
	// priority 可选：契约 §1 起一个 (能力,端口) 是**候选集合**，运行期按 priority
	// 选一条（同级轮盘）。不传就是 0（最高优先）。
	const body = { fromCapability: from.id, fromPort: port, toCapability: to.id };
	if (priority !== undefined) body.priority = priority;
	return H.req(server.base, "/v1/edges", { session, body });
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

// withNSIdQ 给路径显式加 namespaceId（覆盖 harness 自动补的默认命名空间）。
function withNSIdQ(path, nid) {
	return path + (path.includes("?") ? "&" : "?") + "namespaceId=" + nid;
}

function sqlQuery(q, ...args) {
	const db = new DatabaseSync(server.dbPath);
	try {
		return db.prepare(q).all(...args);
	} finally {
		db.close();
	}
}

// fetchTask 走**控制台 HTTP 接口**读一条任务（GET /v1/tasks/{id}）。
//
// out 退役的断言必须落在响应字节上：库是服务端事实，接口才是客户端事实。
// `tasks.out` 列已在迁移 28 删掉，所以「没有 out」的正确判据是**键不存在**
// （`!("out" in task)`），而不是「值为 null」——后者仍然意味着有人会去读它。
async function fetchTask(nsId, taskId) {
	const r = await H.req(
		server.base,
		`/v1/tasks/${encodeURIComponent(taskId)}?namespaceId=${encodeURIComponent(nsId)}`,
		{ session },
	);
	if (r.status !== 200) throw new Error(`GET /v1/tasks/${taskId}: ${r.status} ${r.text}`);
	return r.json?.task || {};
}

// noOutKey 读出任务对象并断言它没有 out 键（返回断言用的 {ok, detail}）。
async function noOutKey(nsId, taskId) {
	const task = await fetchTask(nsId, taskId);
	return { ok: !("out" in task), detail: JSON.stringify(Object.keys(task)) };
}

// emitPort 发一帧 emit 并等**这一帧**的 emit_ack（P1.5 起这是唯一的产出通道）。
//
// 为什么必须等 ack：节点是假 WS 客户端，脚本语义是「emit 之后再 result」。
// 若 result 先被处理，任务进终态，随后那帧 emit 会被判「任务已非运行态」——
// 这是真实能力里同样存在的竞态，e2e 里不该靠 sleep 赌它。
//
// 参数形状直接对应 `anotify-agent emit --port <port> --data <json>`：seq 是任务内
// 单调递增的计数（emit-protocol §4），data 就是下游的 params。
async function emitPort(node, taskId, seq, port, data = {}) {
	const from = node.frames.length;
	node.conn.send({ type: "emit", task_id: taskId, seq, port, data });
	const deadline = Date.now() + 5000;
	while (Date.now() < deadline) {
		const hit = node.frames
			.slice(from)
			.find((f) => f.type === "emit_ack" && f.seq === seq);
		if (hit) return hit;
		await new Promise((r) => setTimeout(r, 40));
	}
	return null;
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

	// #162：命名空间资源（能力/节点/边/任务……）必须带 namespaceId。
	// 节点是直插库的（见 lib/nodes.mjs），也要写 namespace_id。这里统一取
	// 默认命名空间（slug=default）的 id —— **不硬编码 ns_xxx**。
	const nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));

	// A1 随 #204 改写：原断言是「注册即得**组织**（orgs 1 行 + org_members owner）」。
	// 组织层已在迁移 14 删除，注册保证的是「用户 + 一个 default 命名空间 + 内置能力播种」
	// 同事务原子完成（store.RegisterUser）。这里断言同一件事的新形态，且**更强**：
	// 不只看行数，还看内置能力确实被播种了（否则新命名空间里连 notify.phone 都没有）。
	const nss = sqlQuery(`SELECT id, slug FROM namespaces ORDER BY rowid`);
	const builtins = sqlQuery(
		`SELECT name FROM capability_versions WHERE namespace_id = ? AND source = 'builtin'`,
		nsId || "",
	);
	A(
		1,
		"注册即得 default 命名空间",
		nss.length === 1 && nss[0].slug === "default" && nss[0].id === nsId,
		JSON.stringify({ nss, nsId }),
	);
	A(
		1,
		"default 命名空间已播种内置能力",
		builtins.some((b) => b.name === "notify.phone"),
		JSON.stringify(builtins.map((b) => b.name)),
	);

	// ================= A3 能力必须先定义 =================
	// 未定义的能力无法被声明（节点 hello 会被 unknown_capability 拒绝），
	// 也无法成为出口目标（建边被拒）。
	const ghostEdge = await H.req(server.base, "/v1/edges", {
		session,
		body: { fromCapability: "cap_does_not_exist", fromPort: "x", toCapability: "cap_nope" },
	});
	// CONTRACT-downstream-edit §3 起，能力边只能走 PUT /v1/capabilities/{name}/downstreams，
	// 而那个端点只改**已有**能力 → 左端不存在时是 404 not_found（以前 POST /v1/edges
	// 报的是 400 unknown_capability）。不变量没变：未定义的能力**建不出边**。
	A(
		3,
		"未定义能力不能建边",
		ghostEdge.status === 404 && String(ghostEdge.text || "").includes("not_found"),
		`${ghostEdge.status} ${ghostEdge.text?.slice(0, 100)}`,
	);

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
	// 机器码变了（edge_rejected → unknown_port），不变量没变：端口必须属于这个能力
	// 声明过的集合（I2）。新码更具体 —— 它直接点名是哪个端口不在集合里。
	A(
		5,
		"拒绝码是 unknown_port",
		String(closedPort.text || "").includes("unknown_port"),
		String(closedPort.text).slice(0, 120),
	);

	// ================ A5b 能力 + 下游边**一次提交**（POST /capabilities 带 links）================
	//
	// 这条接口把「先建能力、再逐条建边」的两步流程收成**一个事务**：要么全成，要么全不成。
	// 断言分两半：① 形状（请求字段 / 响应的 links 口径 / 边落库）② 原子性（坏边 → 能力也没建）。
	const atomicName = "c.atomic";
	const atomicRes = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name: atomicName,
			outputs: ["done", "failed"],
			paramsSchema: { type: "object" },
			links: [
				{ fromPort: "done", toRef: "c.fin" },
				{ fromPort: "failed", toRef: "c.work" },
			],
		},
	});
	A(
		5,
		"POST /v1/capabilities 带 links → 200",
		atomicRes.status === 200,
		`${atomicRes.status} ${String(atomicRes.text || "").slice(0, 160)}`,
	);
	const atomicBody = atomicRes.json || {};
	const atomicLinks = Array.isArray(atomicBody.links) ? atomicBody.links : [];
	A(
		5,
		"响应的 links 是建成的边（edge 视图：左端裸名字=跟随，带解析到的能力 id）",
		atomicLinks.length === 2 &&
			atomicLinks.every(
				(l) => l.fromRef === atomicName && l.fromIsSource === false && typeof l.toCapabilityId === "string",
			),
		JSON.stringify(atomicLinks).slice(0, 240),
	);
	A(
		5,
		"links 的字段口径与 GET /v1/edges 一致（fromPort/toRef/literals 都在）",
		atomicLinks.length === 2 &&
			atomicLinks.every((l) => "fromPort" in l && "toRef" in l && "literals" in l),
		JSON.stringify(atomicLinks[0] || null).slice(0, 240),
	);
	const atomicEdgesRes = await H.req(server.base, "/v1/edges", { session });
	const atomicEdges = (atomicEdgesRes.json?.edges || []).filter((e) => e.fromRef === atomicName);
	A(
		5,
		"两条边都落库了（一次提交，不是逐条）",
		atomicEdges.length === 2 &&
			atomicEdges.some((e) => e.fromPort === "done" && e.toRef === "c.fin") &&
			atomicEdges.some((e) => e.fromPort === "failed" && e.toRef === "c.work"),
		JSON.stringify(atomicEdges.map((e) => `${e.fromPort}->${e.toRef}`)),
	);

	// 坏边（目标不存在）→ 400 指出 links[i]，且**能力根本没建出来**
	const badName = "c.atomic.bad";
	const badCap = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name: badName,
			outputs: ["done"],
			paramsSchema: { type: "object" },
			links: [{ fromPort: "done", toRef: "c.nope.ghost" }],
		},
	});
	A(
		5,
		"坏边（目标不存在）→ 400，body 带 links[0] + edge_rejected",
		badCap.status === 400 &&
			/links\[0\]/.test(String(badCap.text || "")) &&
			String(badCap.text || "").includes("edge_rejected"),
		`${badCap.status} ${String(badCap.text || "").slice(0, 200)}`,
	);
	const afterBad = await H.req(server.base, "/v1/capabilities", { session });
	A(
		5,
		"坏边让整个提交回滚：能力不在列表里（不是「能力在、边缺」）",
		!((afterBad.json?.capabilities || []).some((c) => c.name === badName)),
		JSON.stringify((afterBad.json?.capabilities || []).map((c) => c.name).filter((n) => n === badName)),
	);
	const afterBadEdges = await H.req(server.base, "/v1/edges", { session });
	A(
		5,
		"坏边让整个提交回滚：一条边都没留下",
		(afterBadEdges.json?.edges || []).filter((e) => e.fromRef === badName).length === 0,
		JSON.stringify((afterBadEdges.json?.edges || []).filter((e) => e.fromRef === badName)),
	);

	// 自连（出口接自己）：契约 §2 起**合法**（环靠 hop 预算兜底，不做静态环检测）。
	// 这里反过来钉住两件事：① 真的存下来了（以前是 400 self loop）；
	// ② 建边的失败仍然整批回滚 —— 用一个真的非法 links（端口不在 outputs）来证明，
	//    否则「回滚」这条会被自连这个现在合法的形状带成假绿。
	const selfName = "c.atomic.self";
	const selfCap = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name: selfName,
			outputs: ["done"],
			paramsSchema: { type: "object" },
			links: [{ fromPort: "done", toRef: selfName }],
		},
	});
	A(
		5,
		"自连现在**允许**（环合法）：能力与那条边一起落库",
		selfCap.status === 200,
		`${selfCap.status} ${String(selfCap.text || "").slice(0, 200)}`,
	);
	const afterSelf = await H.req(server.base, "/v1/edges", { session });
	A(
		5,
		"自连的边真的存在（fromRef = toRef = 自身）",
		(afterSelf.json?.edges || []).some(
			(e) => e.fromRef === selfName && e.toRef === selfName && e.fromPort === "done",
		),
		JSON.stringify((afterSelf.json?.edges || []).filter((e) => e.fromRef === selfName)),
	);

	// 回滚仍然成立：端口不在 outputs 里的 links → 400 且能力没建出来
	const badPortName = "c.atomic.badport";
	const badPort = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name: badPortName,
			outputs: ["done"],
			paramsSchema: { type: "object" },
			links: [{ fromPort: "not-a-port", toRef: selfName }],
		},
	});
	const afterBadPort = await H.req(server.base, "/v1/capabilities", { session });
	A(
		5,
		"坏端口整体回滚：400（links[0]）且能力不在列表里",
		badPort.status === 400 &&
			/links\[0\]/.test(String(badPort.text || "")) &&
			!((afterBadPort.json?.capabilities || []).some((c) => c.name === badPortName)),
		`${badPort.status} ${String(badPort.text || "").slice(0, 200)}`,
	);

	// ================= A6 扇出 =================
	// 一个 (cap, port) 多条边 → 多个任务
	const fanA = (await defineCapability("c.fan.a", [])).json.capability;
	const fanB = (await defineCapability("c.fan.b", [])).json.capability;
	// 候选集合 + 优先级（契约 §3）：fin 是 p=0 的唯一候选，所以下面「选中 fin」
	// 是确定结论，而不是「碰巧轮到谁」。
	await addEdge(work, "done", fanA, 10);
	await addEdge(work, "done", fanB, 20);
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
		// P2.2：过滤参数叫 fromRef（引用口径）；旧名 fromCapability 仍兼容一个版本期，
		// 两个名字各测一遍 —— 先升级的那端不会因为名字换了就查不到。
		["/v1/edges?fromRef=cap_nonexistent", "edges"],
		["/v1/edges?fromCapability=cap_nonexistent", "edges（旧参数名）"],
	]) {
		const r = await H.req(server.base, path, { session });
		const raw = r.text || "";
		const isArray = /"\[|\s*\[/.test(raw) || raw.includes("[]");
		A(12, `${key} 空列表是 [] 不是 null`, r.status === 200 && isArray, raw.slice(0, 100));
	}
	{
		// capabilities 一定有数据（至少内置能力 + 上面建的那些），改用 nodes 验空列表
		// （新库无节点）。组织层删除后也没有「按不存在的组织查」这条路了。
		const r = await H.req(server.base, "/v1/nodes", { session });
		A(12, "nodes 空列表是 [] 不是 null", r.status === 200 && (r.text || "").includes("[]"), (r.text || "").slice(0, 100));
	}

	// ================= A10 命名空间隔离 =================
	//
	// **改写说明（原两条断言不得静默削弱，逐条对照见 PR 正文）**：
	// 原断言：`GET /v1/capabilities?orgId=org_not_mine_xxx` → 403，且响应不含 `c.origin`。
	// 它证明「读别人的组织被拒 + 不泄漏」。`?orgId=` 随组织层删除（迁移 14）而消失，
	// 「组织间隔离」也不再是语义（#204：本部署内任何已登录用户都能访问任何存在的命名空间）。
	// 所以这两条**不能直接删**，改写为一组更强的东西 —— 原用例只有负向断言，
	// 下面补上了正向隔离证明（证明隔离真的生效，而不只是"拒绝了某个参数"）：
	//   ① 两个命名空间各建**同名**能力 same.name → 各自列表里只有自己那条
	//   ② A 空间产生的事件/任务，在 B 空间查不到（B 里查 A → 空；A 里查 A → 有，正向对照）
	//   ③ B 空间的边指向 A 空间的 cap → 400（I11 跨命名空间不引用）
	//   ④ 未知命名空间 → 404（不再用 403/404 区分「别人的」与「不存在」）
	let isoNS;
	let isoTrace = null;
	{
		const created = await H.req(server.base, "/v1/namespaces", {
			session,
			body: { slug: "iso" + Date.now().toString(36), name: "隔离验证" },
		});
		isoNS = created.json?.namespace;
		A(
			10,
			"建第二个命名空间成功",
			created.status === 201 && !!isoNS?.id,
			`${created.status} ${String(created.text).slice(0, 120)}`,
		);
	}
	if (isoNS?.id) {
		const nsB = isoNS.id;
		const nsA = nsId;
		const withNSId = withNSIdQ;
		const sameNamed = (r) =>
			(r.json?.capabilities || []).filter((c) => c.name === "same.name").map((c) => c.id);

		// ---- ① 两个命名空间各建同名能力 ----
		const sameA = (
			await H.req(server.base, withNSId("/v1/capabilities", nsA), {
				session,
				body: { name: "same.name", paramsSchema: { type: "object" } },
			})
		).json?.capability;
		const sameB = (
			await H.req(server.base, withNSId("/v1/capabilities", nsB), {
				session,
				body: { name: "same.name", paramsSchema: { type: "object" } },
			})
		).json?.capability;
		A(
			10,
			"两个命名空间的 same.name 是两个不同定义",
			!!sameA?.id && !!sameB?.id && sameA.id !== sameB.id,
			`${sameA?.id} vs ${sameB?.id}`,
		);

		const listA = await H.req(server.base, withNSId("/v1/capabilities", nsA), { session });
		const listB = await H.req(server.base, withNSId("/v1/capabilities", nsB), { session });
		A(
			10,
			"A 空间列表里 same.name 只有自己那条",
			sameA && JSON.stringify(sameNamed(listA)) === JSON.stringify([sameA.id]),
			JSON.stringify(sameNamed(listA)),
		);
		A(
			10,
			"B 空间列表里 same.name 只有自己那条",
			sameB && JSON.stringify(sameNamed(listB)) === JSON.stringify([sameB.id]),
			JSON.stringify(sameNamed(listB)),
		);
		// 负向（原「不泄漏」意图）：A 的列表不能出现只在 B 里存在的定义 id
		A(
			10,
			"A 空间列表不含 B 空间的能力定义",
			!sameB || !(listA.json?.capabilities || []).some((c) => c.id === sameB.id),
			String(listA.text).slice(0, 160),
		);

		// ---- ② 事件不跨命名空间（任务那半段放在节点上线之后，见下方 A6 段）----
		//
		// 这里只验**事件**：此刻节点还没上线，A 空间这条事件会 pruned/no_node
		// （不建任务），所以拿「产生了任务」当正向对照会假红。事件的落库与
		// namespace_id 无关，可以现在就验；任务的隔离放在 A6 建出真任务之后再验。
		isoTrace = "trace_iso_" + Date.now();
		const evA = await postEvent({
			source: "c.origin",
			type: "go",
			traceId: isoTrace,
			namespaceId: nsA,
			payload: {},
		});
		A(10, "A 空间的事件已落库（正向对照）", evA.json?.inserted === true, String(evA.text).slice(0, 160));
		const eventsInA = await H.req(
			server.base,
			withNSId("/v1/console/events?traceId=" + isoTrace, nsA),
			{ session },
		);
		// 断言 >= 1 而不是 === 1：这条 trace 下除了摄入的那条事件，还可能有一条
		// 剪枝留痕（此刻节点没上线 → pruned/no_node，落进 events 表 source='route'）。
		// 隔离要证的是「B 一条都看不到」，不是「A 恰好一条」。
		A(
			10,
			"A 空间能查到自己的事件",
			eventsInA.status === 200 && (eventsInA.json?.events || []).length >= 1,
			String(eventsInA.text).slice(0, 160),
		);
		const eventsInB = await H.req(
			server.base,
			withNSId("/v1/console/events?traceId=" + isoTrace, nsB),
			{ session },
		);
		A(
			10,
			"B 空间查不到 A 空间的事件",
			eventsInB.status === 200 && (eventsInB.json?.events || []).length === 0,
			String(eventsInB.text).slice(0, 160),
		);

		// ---- ③ 跨命名空间引用 → 400（I11）----
		// B 空间里建一条边，右端指向 A 空间的 same.name
		const crossEdge = await H.req(server.base, "/v1/edges", {
			session,
			body: {
				namespaceId: nsB,
				fromCapability: sameB.id,
				fromPort: "go",
				toCapability: sameA.id,
			},
		});
		A(
			10,
			"跨命名空间建边 → 400",
			crossEdge.status === 400,
			`${crossEdge.status} ${String(crossEdge.text).slice(0, 120)}`,
		);

		// ---- ④ 未知命名空间 → 404，且不泄漏 ----
		const ghost = await H.req(
			server.base,
			withNSId("/v1/capabilities", "ns_not_mine_" + Date.now()),
			{ session },
		);
		A(10, "未知命名空间读 → 404", ghost.status === 404, String(ghost.status));
		A(
			10,
			"404 不泄漏能力名",
			!(ghost.text || "").includes("c.origin"),
			String(ghost.text).slice(0, 120),
		);
	}

	// ================= A8 out 三态 + A7 环剪枝 + A9 租约接管 =================
	// 这三条需要真实节点跑完整链路。
	const node = await bringUpNode(server.base, server.dbPath, nsId, "contract-node", [origin, work, fin, fanA, fanB], 4);
	A(6, "节点上线", !!node.id);

	// A6：从 origin 出发 → work（1 个任务）；work 在 done 出口 emit → 扇出到 fanA+fanB
	//
	// P1.5 起产出只走 emit（result.out 已退役）：扇出的触发点因此是那帧 emit，
	// 而不是结果帧里的一个字符串。判定与建行仍然由中枢按库里的边算（emit 只能报 port）。
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
		// 产出：一帧 emit —— 这就是「能力脚本」唯一能做的事。
		const ackFan = await emitPort(node, tWork, 1, "done", {});
		A(
			6,
			"work 在 done 出口 emit 成功，routed=1（候选集合按优先级选一个，不再全派）",
			!!ackFan && !ackFan.error && ackFan.routed === 1,
			JSON.stringify(ackFan),
		);
		// 结果帧只落终态，不带（也不再支持）出口。
		node.conn.send({ type: "result", task_id: tWork, status: "succeeded", summary: "work 完成" });
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
		// work 的 done 出口有 3 个**候选**：fin(p=0) / fanA(p=10) / fanB(p=20)。
		// 契约 §3 起派发是「过滤不可达 → 取最小 priority 组 → 组内轮盘」，
		// 所以这里必须恰好 1 个任务，而且是 p=0 的 fin —— 全派是以后的 balance:all。
		const fanCaps = fanTasks.map((t) => t.capability);
		A(
			6,
			"第二跳只选 1 个候选（p=0 的 c.fin），不是按边数全派",
			fanTasks.length === 1 && fanCaps[0] === "c.fin",
			JSON.stringify(fanCaps),
		);
	}

	// A10（续）任务隔离：此时节点已上线、A 空间真的有任务了 —— 拿这条真任务验
	// 「B 空间查不到」，同时用「A 空间查得到」做正向对照（避免空转的假绿）。
	if (isoNS?.id) {
		const realTrace = ev1.json.traceId;
		const tInA = await H.req(server.base, "/v1/tasks?traceId=" + realTrace, { session });
		A(
			10,
			"A 空间有自己的任务（正向对照）",
			tInA.status === 200 && (tInA.json?.tasks || []).length >= 1,
			String(tInA.text).slice(0, 160),
		);
		const tInB = await H.req(
			server.base,
			withNSIdQ("/v1/tasks?traceId=" + realTrace, isoNS.id),
			{ session },
		);
		A(
			10,
			"B 空间查不到 A 空间的任务",
			tInB.status === 200 && (tInB.json?.tasks || []).length === 0,
			String(tInB.text).slice(0, 160),
		);
	}

	// A8 out 退役 + A8b「失败也能继续」（P1.5 硬切）
	//
	// 改动前：A8 验 out 的三种写法（缺失 / null / 字符串），A8b 验「失败 + out 也派
	// 下一跳」。out 退役之后，出口的唯一载体是 emit，所以这一节改成：
	//
	//   A8  结果帧里带什么都不再路由：API 响应里没有 out 字段、不建任何下游；
	//   A8b 想派下一跳 → emit；**失败不撤回已经发生的 emit**（I4 的形式保留）。
	//
	// 断言没有放宽：A8 新增了「老写法 out='done' 也一条下游都不建」这条更强的负向
	// 断言，A8b 仍然逐条核对扇出集合（fin + fanA + fanB）。迁移 28 之后，「没有 out」
	// 也从「值恒为 NULL」升级成「响应里没有这个键」（`!("out" in task)`）——更强的那条。
	{
		// 清场：A6 扇出的 3 个任务还占着实例槽位（节点 4 个实例），不释放的话
		// 后面的任务会一直停在 pending、拿不到 assign（emit 会被判「任务不存在」）。
		for (const busy of sqlQuery(
			`SELECT id FROM tasks WHERE node_id = ? AND status IN ('assigned','acked','running')`,
			node.id,
		)) {
			node.conn.send({ type: "result", task_id: busy.id, status: "succeeded" });
		}
		await new Promise((r) => setTimeout(r, 600));

		// 等任务真的落到节点上（status 离开 pending）再操作——
		// 这是节点的硬前提，不是可以靠 sleep 赌的时序。
		const waitDispatched = async (id) => {
			for (let i = 0; i < 60; i++) {
				const row = sqlQuery(`SELECT status FROM tasks WHERE id = ?`, id)[0];
				if (row && row.status !== "pending") return true;
				await new Promise((r) => setTimeout(r, 50));
			}
			return false;
		};

		// 造独立 trace，用「来源 → work」这条入口；work --done--> fin/fanA/fanB 已建好。
		const mk = async (label) => {
			const ev = await postEvent({
				source: "c.origin",
				type: "go",
				traceId: "trace_outcome_" + label + "_" + Date.now(),
				payload: {},
			});
			const id = ev.json?.tasks?.[0]?.id;
			await waitDispatched(id);
			return { id, traceId: ev.json?.traceId };
		};
		const childrenOf = (taskId) =>
			sqlQuery(
				`SELECT capability FROM tasks WHERE trace_id IS NOT NULL AND parent_task_id = ?`,
				taskId,
			);

		// A8-态1：out 缺失 → 响应里没有 out 键，且不派生下一跳
		const tMissing = await mk("missing");
		node.conn.sendRaw(
			`{"type":"result","task_id":"${tMissing.id}","status":"succeeded","summary":"no out"}`,
		);
		await new Promise((r) => setTimeout(r, 400));
		{
			const r = await noOutKey(nsId, tMissing.id);
			A(8, "out 缺失 → 响应里没有 out 字段", r.ok, r.detail);
		}
		A(
			8,
			"out 缺失 → 不建下游（0 次 emit 合法）",
			childrenOf(tMissing.id).length === 0,
			JSON.stringify(childrenOf(tMissing.id)),
		);

		// A8-态2：out:null 与缺失等价（老帧照收，语义相同）
		const tNull = await mk("null");
		node.conn.sendRaw(`{"type":"result","task_id":"${tNull.id}","status":"succeeded","out":null}`);
		await new Promise((r) => setTimeout(r, 400));
		{
			const r = await noOutKey(nsId, tNull.id);
			A(8, "out:null → 响应里没有 out 字段（与缺失等价）", r.ok, r.detail);
		}

		// A8-态3（核心）：out 指向一条**真有边**的出口（done），照样不落库、不建下游。
		// 老实现里这一帧会建出 3 个任务；现在它必须什么都不做——这就是「这条腿断了」。
		const tStr = await mk("str");
		node.conn.send({ type: "result", task_id: tStr.id, status: "succeeded", out: "done" });
		await new Promise((r) => setTimeout(r, 600));
		{
			const r = await noOutKey(nsId, tStr.id);
			A(8, "out='done' → 响应里没有 out 字段（列已删，没有写入方）", r.ok, r.detail);
		}
		A(
			8,
			"out='done' → 一条下游都不建（老通道已退役，不是并存）",
			childrenOf(tStr.id).length === 0,
			JSON.stringify(childrenOf(tStr.id)),
		);

		// A8b：**失败**之前 emit 过的下游照样在（I4 的形式保留：路由只看 emit，不看 status）
		const tFail = await mk("failedroute");
		const ackFail = await emitPort(node, tFail.id, 1, "done", {});
		A(
			8,
			"A8b 失败路径：emit(done) 成功 routed=1（与成功路径同一条候选）",
			!!ackFail && !ackFail.error && ackFail.routed === 1,
			JSON.stringify(ackFail),
		);
		node.conn.send({
			type: "result",
			task_id: tFail.id,
			status: "failed",
			summary: "exit status 7: 编译失败",
		});
		await new Promise((r) => setTimeout(r, 700));
		const failParent = sqlQuery(`SELECT status, summary FROM tasks WHERE id = ?`, tFail.id)[0];
		A(8, "A8b 失败 + 已 emit → 父任务 status=failed", failParent?.status === "failed", JSON.stringify(failParent));
		{
			const r = await noOutKey(nsId, tFail.id);
			A(8, "A8b 失败 → 响应里同样没有 out 字段（出口只在 emission 里）", r.ok, r.detail);
		}
		A(8, "A8b 失败必须带原因", !!failParent?.summary, JSON.stringify(failParent));
		// work --done--> 有 3 个候选，选中规则与成功时**完全一样**（p=0 的 c.fin）：
		// 失败不该改变「派给谁」，只该改变父任务的终态。
		const failCaps = childrenOf(tFail.id).map((c) => c.capability);
		A(
			8,
			"A8b 失败前 emit 的选中结果与成功时一致（恰好 c.fin）",
			failCaps.length === 1 && failCaps[0] === "c.fin",
			JSON.stringify(failCaps),
		);
	}

	// A7 成环：**契约 §2 起环是合法的**（review → fix → review 是正常编排），
	// 兜底从「静态环检测 + 运行期 path 判重」换成 **hop 预算**（ANOTIFY_MAX_HOPS，
	// 默认 16 → 剪枝 max_hops）。这条用例因此反转：
	//
	//   ① 建环边 `b --back--> a` → **200**（以前是 400）；
	//   ② 第一跳 a → b 正常派发；
	//   ③ b 在 back 出口 emit → **又建出一个 a 任务**（环真的跑起来了，
	//      这在 path 判重时代是不可能的：第二次进 a 会被剪成 pruned/path）；
	//   ④ hop 预算本身由 internal/route 的单测覆盖（prune_test 的 max_hops 用例），
	//      这里不再跑满 16 圈——e2e 跑 16 次往返只会更慢、更容易 flaky。
	{
		const a = (await defineCapability("c.cycle.a", ["go"])).json.capability;
		const b = (await defineCapability("c.cycle.b", ["back"])).json.capability;
		// #110：事件入口用**入口边**（左端是来源名），不再需要"起点能力"。
		await addEntryEdge("c.cycle.src", "start", a);
		const ab = await addEdge(a, "go", b);
		A(7, "环：a --go--> b 建边成功", ab.status === 200, `${ab.status} ${String(ab.text).slice(0, 120)}`);

		const ba = await addEdge(b, "back", a);
		A(
			7,
			"环：b --back--> a 现在**允许保存**（环是合法编排，靠 hop 预算兜底）",
			ba.status === 200,
			`${ba.status} ${String(ba.text).slice(0, 160)}`,
		);

		const cycNode = await bringUpNode(server.base, server.dbPath, nsId, "contract-cycle", [a, b], 2);
		const ev = await postEvent({ source: "c.cycle.src", type: "start", payload: {} });
		const tA = ev.json?.tasks?.[0]?.id;
		A(7, "环：第一跳建 1 个任务（a）", ev.json?.tasks?.length === 1);
		A(7, "环：第一跳任务是 c.cycle.a", ev.json?.tasks?.[0]?.capability, "c.cycle.a");

		await new Promise((r) => setTimeout(r, 400));
		cycNode.conn.send({ type: "ack", task_id: tA });
		await new Promise((r) => setTimeout(r, 200));
		cycNode.conn.send({ type: "progress", task_id: tA, note: "环：第一跳" });
		await new Promise((r) => setTimeout(r, 200));
		const ackA = await emitPort(cycNode, tA, 1, "go", {});
		A(
			7,
			"环：a 在 go 出口 emit 成功并建出 b（routed=1）",
			!!ackA && !ackA.error && ackA.routed === 1,
			JSON.stringify(ackA),
		);
		cycNode.conn.send({ type: "result", task_id: tA, status: "succeeded", summary: "a 完成" });

		const tB = sqlQuery(
			`SELECT id FROM tasks WHERE parent_task_id = ? AND capability = 'c.cycle.b'`,
			tA,
		)[0]?.id;
		A(7, "环：第二跳任务真的建出来了（b）", !!tB, String(tB));

		// ③ 回到 a：环真的能绕第二圈。
		await new Promise((r) => setTimeout(r, 400));
		cycNode.conn.send({ type: "ack", task_id: tB });
		await new Promise((r) => setTimeout(r, 200));
		cycNode.conn.send({ type: "progress", task_id: tB });
		await new Promise((r) => setTimeout(r, 200));
		const ackB = await emitPort(cycNode, tB, 1, "back", {});
		A(
			7,
			"环：b 在 back 出口 emit → 又建出一个 a 任务（环跑起来了，不再被 path 判重剪掉）",
			!!ackB && !ackB.error && ackB.routed === 1,
			JSON.stringify(ackB),
		);
		cycNode.conn.send({ type: "result", task_id: tB, status: "succeeded", summary: "b 完成" });
		await new Promise((r) => setTimeout(r, 300));
		const tA2 = sqlQuery(
			`SELECT id, path FROM tasks WHERE parent_task_id = ? AND capability = 'c.cycle.a'`,
			tB,
		)[0];
		A(7, "环：第二圈的 a 任务带完整 path（a,b,a）", !!tA2, String(tA2?.id));
		A(
			7,
			"环：path 里能看到重复能力名（说明 hop 预算才是唯一闸门）",
			!!tA2 && String(tA2.path).includes("c.cycle.a") && String(tA2.path).split("c.cycle.a").length > 2,
			String(tA2?.path),
		);
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
				st = sqlQuery(`SELECT status, reported FROM tasks WHERE id = ?`, phoneTaskId)[0];
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
	// 仍是同一条不变量，只是状态码随入口变了：PUT downstreams 对不存在的左端是 404。
	A(3, "未定义能力无法建边（运行时侧）", ghostEdge.status === 404);

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
