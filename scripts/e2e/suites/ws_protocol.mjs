#!/usr/bin/env node
/* SUITE: ws_protocol — 节点协议十一帧（契约 §2 帧清单 / §6.1）
 *
 * 端点：GET /v1/node/stream（Bearer <node_token>，opaque token）
 *
 * console-declares（协议 v2）之后是**十一帧**：
 *   上行 6    hello / heartbeat / ack / progress / result / **apply**（新）
 *   下行 4    welcome / assign / cancel / error（assign 加 digest/trace_id）
 *   下行 1    **deploy**（新：下发 / 撤销产物）
 *
 * 覆盖：
 *   握手     无 token → 401 且**不升级**；假 token → 403
 *   hello v2 applied[] 取代 capabilities[]（能力归属由部署派生，D4）
 *   deploy → apply 往返（§4/§5）：推产物 → 节点回报 → deployments.state=applied
 *   apply 失败路径：requires 探不到 → apply{failed} + missing_requirement
 *   协议版本  hello{protocol:1} → error 帧且连接关闭（v1 客户端必须升级）
 *   错误帧    unknown_type / bad_frame / unknown_capability / invalid_transition
 *   三态      result 的 out 缺失 / null / 字符串（不变量 I5）
 *   心跳      >2×心跳无上行帧 → 判死关连接；心跳续租
 *   不变量    assign / deploy 帧里没有 argv/shell/command（I1）
 *
 * 与旧版的区别：旧版测 notification/ack/subscribe（浏览器端旧模型），
 * 现在测节点协议。
 */
import * as H from "../lib/harness.mjs";
import { DatabaseSync } from "node:sqlite";
import {
	bringUpNode,
	nodeHello,
	seedNodeRows,
	wsConnect,
	waitForFrame,
} from "../lib/nodes.mjs";

let server, session, sendKey, nsId;

async function defineCapability(name, outputs = [], extra = {}) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { name, outputs, paramsSchema: extra.paramsSchema || { type: "object" }, ...extra },
	});
	if (r.status !== 200) throw new Error(`define ${name}: ${r.status} ${r.text}`);
	return r.json.capability;
}

// defineCapabilityWithRun 建一个**有可部署产物**的能力（契约 §11 的 run 块）。
//
// v2 的关键分工：能力定义在中枢（含 params/outputs），实现方式（入口文件 + 文件内容）
// 编译成产物、按 digest 下发。没有 run 的能力是「纯定义」，无法部署（部署时 404
// artifact_not_found，见 deploy 套件）。这里返回 digest，供下面的 apply 帧与对账断言用。
async function defineCapabilityWithRun(name, outputs, script, requires = []) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			name,
			outputs,
			requires,
			run: { entrypoint: "run.sh", files: { "run.sh": script } },
		},
	});
	if (r.status !== 200) throw new Error(`define ${name}: ${r.status} ${r.text}`);
	return { ...r.json.capability, digest: r.json.digest };
}

// listDeployments 读部署行（契约 §11 GET /v1/deployments）。
async function listDeployments(filter = {}) {
	const q = Object.entries(filter)
		.map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
		.join("&");
	const r = await H.req(server.base, "/v1/deployments" + (q ? "?" + q : ""), { session });
	if (r.status !== 200) throw new Error(`GET /v1/deployments: ${r.status} ${r.text}`);
	return r.json?.deployments || [];
}

// waitDeployment 轮询到某条部署行满足条件（部署结果来自节点帧，天生异步）。
//
// 纪律：等的是**条件**，不是固定 sleep（套件里不允许 waitForTimeout）。
async function waitDeployment(deployId, pred, timeoutMs = 5000) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	while (Date.now() < deadline) {
		const row = (await listDeployments()).find((d) => d.id === deployId);
		last = row || null;
		if (row && pred(row)) return row;
		await new Promise((r) => setTimeout(r, 40));
	}
	return last;
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

// dbRows 直接读库拿多行（与 taskRow 同一手法）。
//
// 为什么 emit 的用例必须看表：它要断言的恰好是「**行没变**」（重复 seq 不重复派发、
// 被拒的那次不落 emission）。走 HTTP 只能看到聚合后的页面视角，而「没有多出一行」
// 在接口上表现为「还是 2 个任务」——看起来一样，却可能是 3 个里删了 1 个。
function dbRows(sql, ...args) {
	const db = new DatabaseSync(server.dbPath);
	try {
		return db.prepare(sql).all(...args);
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

	// #204 / 迁移 14：组织层删除（orgs / org_members 没了，devseed 也不再输出 org，
	// 改成 stderr 的「命名空间就绪: ns_xxx」）。所以：
	//   ① 原来那句 `H.check("拿到 orgId", ...)` 换成**命名空间口径**的等价断言；
	//   ② 原来为了拿 orgId 而单独 spawn 一次 devseed 的代码整段删掉 —— 它唯一的
	//      产物就是 orgId，而 H.seed() 已经在同一个库上用同一个用户名跑过 devseed。
	//
	// #162：节点直插库时必须写 namespace_id（迁移 12 起 NOT NULL）。
	// 取本部署默认命名空间（slug=default）的 id —— 用例里不硬编码 ns_xxx。
	nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	// ================= 造数入口校验（#210） =================
	// 背景：造数辅助把参数**直插 sqlite**。capabilityIds 里混进 undefined 时，
	// 过去会一路绑进 node_capabilities，崩在造数深处：
	//   TypeError: Provided value cannot be bound to SQLite parameter 2.
	// 套件崩在 await 里不写 JSON 结果 → 汇总显示「0通过/0失败」，和超时/环境
	// 问题没法区分，排查成本极高。
	// 这里钉死：① 入口立刻抛；② 消息里带**下标与值**；③ 不静默跳过无效项、
	// 校验失败时也不留半行脏数据。
	console.log("--- 造数入口校验（#210）---");
	{
		const grab = async (fn) => {
			try {
				await fn();
				return null;
			} catch (e) {
				return e.message;
			}
		};
		// capabilityIds 里的 undefined：必须抛在入口，不能等到绑库
		const badCap = await grab(() =>
			seedNodeRows(server.dbPath, nsId, "ws-badseed", ["cap_placeholder", undefined], 1),
		);
		H.check(
			"#210 capabilityIds[1] 是 undefined → 入口立刻抛且带下标",
			/capabilityIds\[1\] 是 undefined/.test(badCap || ""),
			String(badCap),
		);
		// 空串同样不该被当成「有效 id」静默放过
		const emptyCap = await grab(() =>
			seedNodeRows(server.dbPath, nsId, "ws-badseed", [""], 1),
		);
		H.check(
			'#210 capabilityIds[0] 是 "" → 入口立刻抛（不静默跳过）',
			/capabilityIds\[0\] 是 ""/.test(emptyCap || ""),
			String(emptyCap),
		);
		// bringUpNode 的 caps[].id 就是 #210 那次真正的源头（上游建能力失败）
		const badCaps = await grab(() =>
			bringUpNode(server.base, server.dbPath, nsId, "ws-badnode2", [
				{ id: undefined, name: "ws.demo", version: 1 },
			]),
		);
		H.check(
			"#210 caps[0].id 是 undefined → 入口立刻抛且带下标",
			/caps\[0\]\.id 是 undefined/.test(badCaps || ""),
			String(badCaps),
		);
		// 入口校验必须在**开库写库之前**：失败不能留下半行节点
		const dbChk = new DatabaseSync(server.dbPath);
		try {
			const dirty = dbChk.prepare("SELECT id FROM nodes WHERE name = ?").get("ws-badseed");
			H.check("#210 校验失败不留半行脏数据", !dirty, JSON.stringify(dirty));
		} finally {
			dbChk.close();
		}
	}

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

	// ================= 协议版本 1 被拒（契约 §1） =================
	//
	// 版本升到 2 之后，**老客户端必须升级**：一个 v1 节点发来的 hello 里没有 applied[]，
	// 中枢若默默接受，它就永远拿不到产物、也永远会被当成「能跑」却跑不了
	// （那正是本 epic 要消灭的两个真相源）。所以 hello 的 protocol != 2 直接判死：
	// 回 error 帧并关连接，不让它留在半握手状态里。
	console.log("--- 协议版本（v1 被拒）---");
	{
		const oldNode = seedNodeRows(server.dbPath, nsId, "ws-protocol1", [], 1);
		const oldConn = await wsConnect(server.base, oldNode.token);
		const oldFrames = [];
		oldConn.onFrame((f) => oldFrames.push(f));
		oldConn.send(nodeHello(oldNode.id, [], 1, 1)); // protocol = 1
		const verErr = await waitForFrame(oldFrames, (f) => f.type === "error");
		H.check("hello{protocol:1} → 收到 error 帧", !!verErr, JSON.stringify(oldFrames));
		H.check(
			"错误信息指明协议不匹配（而不是默默接受）",
			/protocol/i.test(verErr?.message || ""),
			JSON.stringify(verErr),
		);
		H.check("协议不符则不收 welcome", !oldFrames.some((f) => f.type === "welcome"));
		// 关闭也是契约的一部分：留着半死连接只会让节点侧以为“连上了”
		const deadline = Date.now() + 3000;
		while (Date.now() < deadline && !oldConn.closed) {
			await new Promise((r) => setTimeout(r, 25));
		}
		H.check("协议不匹配后连接被关闭", oldConn.closed);
		oldConn.close();
	}

	// ================= 正常握手 =================
	console.log("--- 握手成功 ---");
	const cap = await defineCapability("ws.demo", ["go"]);
	const cap2 = await defineCapability("ws.work", []);

	const node = await bringUpNode(server.base, server.dbPath, nsId, "ws-node", [cap, cap2], 2);
	const welcome = node.frames.find((f) => f.type === "welcome");
	H.check("收到 welcome", !!welcome);
	H.eq("welcome.node_id 正确", welcome?.node_id, node.id);
	H.eq("welcome 带协议版本（v2）", welcome?.protocol, 2);
	H.check("welcome 带心跳间隔", typeof welcome?.heartbeat_sec === "number" && welcome.heartbeat_sec > 0);
	H.check("welcome 带租约 TTL", typeof welcome?.lease_ttl_sec === "number" && welcome.lease_ttl_sec > 0);

	// 节点应被标记 online
	const nodesView = await H.req(server.base, "/v1/nodes", { session });
	const self = (nodesView.json?.nodes || []).find((n) => n.id === node.id);
	H.eq("节点状态 online", self?.status, "online");

	// ================= hello v2 形状（契约 §3） =================
	// 这条钉的是**帧形状本身**：v2 的 hello 只陈述「我缓存了哪些产物」（applied[]），
	// 不再声明「我会跑什么」（capabilities[]）。中枢据此重建派生索引 node_capabilities，
	// 而「能力归谁」的唯一写者仍是控制台的部署（契约 D4）。
	console.log("--- hello v2 形状（applied 取代 capabilities）---");
	{
		const sample = nodeHello(node.id, [{ name: "ws.demo", version: 1 }], 2);
		H.eq("hello.protocol = 2", sample.protocol, 2);
		H.check(
			"hello 不再有 capabilities 字段（v1 形状已删）",
			!("capabilities" in sample),
			Object.keys(sample).join(","),
		);
		H.eq("hello.applied 条数 = 缓存产物数", (sample.applied || []).length, 1);
		H.eq("hello.applied[0].capability", sample.applied?.[0]?.capability, "ws.demo");
		H.eq("hello.applied[0].version", sample.applied?.[0]?.version, 1);
		H.check(
			"hello.applied[0].digest 是 sha256:<64 hex>（内容寻址）",
			/^sha256:[0-9a-f]{64}$/.test(sample.applied?.[0]?.digest || ""),
			JSON.stringify(sample.applied?.[0]),
		);
		// 服务端确实按 applied 重建了归属：握手时带了 2 条 → 节点上能看到 2 条
		const capsAfter = (self?.capabilities || []).map((c) => `${c.name}@${c.version}`).sort();
		H.eq(
			"中枢按 hello.applied 重建能力归属（ws.demo@1 + ws.work@1）",
			capsAfter,
			["ws.demo@1", "ws.work@1"],
		);
	}

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

	// ================= applied 里放未定义能力 → unknown_capability =================
	//
	// v1 时这条测的是「节点声明了自己不会跑的能力」；v2 的语义变了（能力归属由部署决定），
	// 但**同一条护栏必须保留**：applied[] 里的能力必须本来就是中枢定义过的
	// —— 产物只可能是从中枢部署过去的，出现陌生名字说明数据被写坏了。
	// 契约要求「整帧拒绝」：不收 welcome，且**一行不写**（node_capabilities 保持空）。
	console.log("--- 能力白名单（A3 / applied 全帧拒绝）---");
	const badNode = seedNodeRows(server.dbPath, nsId, "ws-badnode", [], 1);
	const badConn = await wsConnect(server.base, badNode.token);
	const badFrames = [];
	badConn.onFrame((f) => badFrames.push(f));
	badConn.send(nodeHello(badNode.id, [{ name: "does.not.exist", version: 9 }], 1));
	const capErr = await waitForFrame(badFrames, (f) => f.type === "error");
	H.eq("applied 里放未定义能力 → unknown_capability", capErr?.code, "unknown_capability");
	H.check("未注册成功则不收 welcome", !badFrames.some((f) => f.type === "welcome"));
	{
		// 整帧拒绝的另一半：库里不能留下半行归属（否则页面会显示「这台能跑」）
		const badView = await H.req(server.base, "/v1/nodes", { session });
		const badSelf = (badView.json?.nodes || []).find((n) => n.id === badNode.id);
		H.eq("整帧拒绝：node_capabilities 一行都不写", (badSelf?.capabilities || []).length, 0);
	}
	badConn.close();

	// ================= deploy → apply 往返（契约 §4 / §5） =================
	//
	// 这条钉的是 v2 的**主链路**：控制台 POST /v1/deployments → 中枢推 deploy 帧 →
	// 节点落盘后回 apply → 中枢把『这台机器到底装上没有』记在 deployments.state 上。
	// 少了 apply 这一跳，部署就永远停在 pending（控制台上看起来“推过了”），
	// 失败部署更是永远不可见——运维会以为已经生效。
	console.log("--- deploy → apply 往返 ---");
	const artCap = await defineCapabilityWithRun(
		"ws.deploy",
		["go"],
		"#!/bin/bash\nset -euo pipefail\nprintf 'go\\n' > \"$ANOTIFY_OUT_FILE\"\n",
	);
	H.check("建出带产物（run）的能力，响应带 digest", /^sha256:[0-9a-f]{64}$/.test(artCap.digest || ""), String(artCap.digest));
	const depRes = await H.req(server.base, "/v1/deployments", {
		session,
		body: { capabilityId: artCap.id, nodeIds: [node.id] },
	});
	H.eq("POST /v1/deployments → 200", depRes.status, 200);
	const depItem = (depRes.json?.deployments || [])[0] || {};
	H.check(
		"逐台返回 {nodeId, deployId, pushed:true}（在线节点）",
		!!depItem.deployId && depItem.pushed === true && depItem.nodeId === node.id,
		JSON.stringify(depItem),
	);
	const depFrame = await waitForFrame(
		node.frames,
		(f) => f.type === "deploy" && f.deploy_id === depItem.deployId,
	);
	H.check("节点收到 deploy 帧", !!depFrame, JSON.stringify(node.frames.map((f) => f.type)));
	H.eq("deploy.action = apply", depFrame?.action, "apply");
	{
		// 产物必须**自包含**（可拷到别的机器直接跑）且不含可执行命令文本（I1）
		const art = depFrame?.artifact || {};
		H.eq("deploy.artifact.metadata.name", art?.metadata?.name, "ws.deploy");
		H.eq("deploy.artifact.metadata.version", art?.metadata?.version, artCap.version);
		H.check(
			"deploy.artifact 内联了脚本文本（自包含）",
			typeof art?.spec?.run?.files?.["run.sh"] === "string" && art.spec.run.files["run.sh"].startsWith("#!"),
			JSON.stringify(art?.spec?.run || {}).slice(0, 160),
		);
		const rawDeploy = JSON.stringify(depFrame || {}).toLowerCase();
		H.check(
			"deploy 帧不含 argv/shell/command（I1：中枢不描述执行）",
			!rawDeploy.includes("argv") && !rawDeploy.includes("shell") && !rawDeploy.includes("command"),
			rawDeploy.slice(0, 200),
		);
	}
	// 节点回报 applied（真实节点在 cache.put 成功之后发这一帧）
	node.conn.send({
		type: "apply",
		deploy_id: depItem.deployId,
		capability: artCap.name,
		version: artCap.version,
		digest: artCap.digest,
		state: "applied",
	});
	const appliedRow = await waitDeployment(depItem.deployId, (r) => r.state === "applied");
	{
		// 上行的 apply 帧如果被中枢当成「未知帧」，节点会收到一帧 error{unknown_type}：
		// 那是这条断言最可能的真实死因，所以把它拼进失败详情。
		const applyErr = node.frames.find(
			(f) => f.type === "error" && /unexpected frame/i.test(f.message || ""),
		);
		H.check(
			"apply{applied} 后该行 state 变 applied",
			appliedRow?.state === "applied",
			`实际 state=${appliedRow?.state}；中枢回给节点的 error 帧：${JSON.stringify(applyErr || null)}`,
		);
	}
	H.eq("apply 成功后 reason 为空", appliedRow?.reason, "");
	H.eq("该行 digest 就是部署的产物 digest", appliedRow?.digest, artCap.digest);

	// ================= apply 失败路径（缺依赖） =================
	//
	// 「这台机器没装 claude」是**正常的部署结论**，不是协议错误：必须走 apply.reason，
	// 不能掀连接，而且逐台显示在控制台上。推一个 requires 不可能满足的产物，
	// 断言中枢把 failed + missing_requirement 真的落库。
	console.log("--- apply 失败路径（missing_requirement）---");
	const reqCap = await defineCapabilityWithRun(
		"ws.req",
		[],
		"#!/bin/bash\necho ok\n",
		["definitely-not-installed-cmd"],
	);
	const depFail = await H.req(server.base, "/v1/deployments", {
		session,
		body: { capabilityId: reqCap.id, nodeIds: [node.id] },
	});
	H.eq("POST /v1/deployments（requires 缺失的能力）→ 200", depFail.status, 200);
	const failItem = (depFail.json?.deployments || [])[0] || {};
	const depFrame2 = await waitForFrame(
		node.frames,
		(f) => f.type === "deploy" && f.deploy_id === failItem.deployId,
	);
	H.check("节点收到第二帧 deploy", !!depFrame2);
	H.check(
		"产物里带 requires（节点据此探依赖）",
		(depFrame2?.artifact?.spec?.requires || []).includes("definitely-not-installed-cmd"),
		JSON.stringify(depFrame2?.artifact?.spec?.requires || null),
	);
	node.conn.send({
		type: "apply",
		deploy_id: failItem.deployId,
		capability: reqCap.name,
		version: reqCap.version,
		digest: reqCap.digest,
		state: "failed",
		reason: "missing_requirement:definitely-not-installed-cmd",
	});
	const failedRow = await waitDeployment(failItem.deployId, (r) => r.state === "failed");
	H.check(
		"apply{failed} 后该行 state 变 failed",
		failedRow?.state === "failed",
		`实际 state=${failedRow?.state}`,
	);
	H.check(
		"failed 行的 reason 含 missing_requirement（机器可读短码）",
		/missing_requirement/.test(failedRow?.reason || ""),
		String(failedRow?.reason),
	);

	// ================= 完整任务流：assign → ack → progress → result =================
	console.log("--- 任务流（十一帧里的一条完整链）---");
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
		// 工作目录**不在协议里**（#289）：它属于能力定义（产物 run.workDir），
		// 节点执行时从缓存产物读。帧里再出现 work_dir 就是又开了一个来源。
		H.check(
			"assign 帧不带 work_dir（目录归能力定义）",
			!Object.prototype.hasOwnProperty.call(assign || {}, "work_dir"),
			JSON.stringify(Object.keys(assign || {})),
		);

		// 字段集合是协议契约：多一个少一个都要在这里显式改，不能悄悄漂移。
		//
		// **这里从「数个数」改成「点名」**（P1 交付时发现的一次漂移）：
		// 断言原本是 `fields.length === 7`，而 P1 给 assign 加了 `trace_id`（v2）
		// 与 `token`（emit socket 鉴权），帧已经变成 9 个字段 —— 但 e2e 里钉的是
		// 数字 7，于是这条断言从 P1 合入那一刻起就是红的。数个数还天生说不出
		// 「换掉了哪个」：删掉 digest、加个 foo 也是 7。所以改成**逐个点名**
		// （更强，不是放宽）：新增字段必须在这里写下来，删掉/改名会直接红。
		//
		// ws.work 没有 outputs，所以 ports/expects 整帧不出现（omitempty）；
		// 没有产物，digest 也不出现。
		const fields = Object.keys(assign).sort();
		H.eq(
			"assign 帧字段集合固定（增删字段必须在这里显式登记）",
			fields,
			[
				"capability",
				"lease_until",
				"max_duration_sec",
				"params",
				"task_id",
				"token",
				"trace_id",
				"type",
				"version",
			].sort(),
		);
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

	// ================= A8：out 两态（outcome-v1.1）=================
	console.log("--- A8 out 两态 ---");

	// 态1：out 缺失 = 没选出口 → out=NULL（结果帧到了，所以 reported=1）
	const evMissing = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tMissing = evMissing.json?.tasks?.[0]?.id;
	const aMissing = await waitForFrame(
		node.frames,
		(f) => f.type === "assign" && f.task_id === tMissing,
	);
	H.check("态1 收到 assign", !!aMissing);
	node.conn.sendRaw(
		`{"type":"result","task_id":"${tMissing}","status":"succeeded","summary":"no out"}`,
	);
	await waitStatus(tMissing, "succeeded");
	const rMissing = taskRow(tMissing);
	H.eq("态1 out 缺失 → reported=1（帧到了）", rMissing.reported, 1);
	H.eq("态1 out 缺失 → out=NULL", rMissing.out, null);

	// 态2：out:null 与缺失完全等价（协议层不再区分）
	const evNull = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tNull = evNull.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tNull);
	node.conn.sendRaw(`{"type":"result","task_id":"${tNull}","status":"succeeded","out":null}`);
	await waitStatus(tNull, "succeeded");
	const rNull = taskRow(tNull);
	H.eq("态2 out:null → reported=1", rNull.reported, 1);
	H.eq("态2 out:null → out=NULL（与缺失等价）", rNull.out, null);

	// 态3：out:"x" → 选了出口
	const evStr = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tStr = evStr.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tStr);
	node.conn.send({ type: "result", task_id: tStr, status: "succeeded", out: "again" });
	await waitStatus(tStr, "succeeded");
	const rStr = taskRow(tStr);
	H.eq("态3 out:'again' → reported=1", rStr.reported, 1);
	H.eq("态3 out:'again' → out='again'", rStr.out, "again");

	// 态4：失败 + 选了出口 → 路由照走（I4：路由只看 out，不看 status）。
	// ws.demo 的出口没有接边，所以「继续」的表现是：不静默——库里 out 落了值。
	const evFail = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tFail = evFail.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tFail);
	node.conn.send({
		type: "result",
		task_id: tFail,
		status: "failed",
		out: "again",
		summary: "exit status 3: 编译失败",
	});
	await waitStatus(tFail, "failed");
	const rFail = taskRow(tFail);
	H.eq("态4 失败 + out='again' → out 落库", rFail.out, "again");
	H.eq("态4 失败带原因 → summary 非空", rFail.summary === "exit status 3: 编译失败" ? 1 : 0, 1);

	// 态5：失败且没说原因 → 库里兜底成 "failed"（不允许「失败但没有说明」）
	const evFail2 = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tFail2 = evFail2.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tFail2);
	node.conn.sendRaw(`{"type":"result","task_id":"${tFail2}","status":"failed"}`);
	await waitStatus(tFail2, "failed");
	const rFail2 = taskRow(tFail2);
	H.eq("态5 失败无说明 → summary 兜底", rFail2.summary, "failed");

	// ================= emit 帧（P1：唯一的产出通道） =================
	//
	// 设计：docs/design/emit-protocol.md（§3 帧 / §4 seq 与幂等 / §7 事务边界 / §8 边界）。
	//
	// 为什么这条链路必须在 e2e 里、不能只靠单测：一次 emit 要 ①读真库里的任务状态
	// ②在同一事务里写 emission + 建下游任务行 ③在**一条活着的节点连接上**把结果写成
	// emit_ack。三件事分开看都对、连起来才错的那类 bug（比如拒收顺手把连接关了 →
	// 节点重连、重发、再被拒、再重连）只有真连接 + 真库能抓到。
	//
	// 一条硬纪律（ws/node_emit.go）：**拒收一律走 emit_ack.error**，不回 error 帧、
	// 更不断连接。下面每一条拒收用例都顺带钉住它。
	console.log("--- emit 帧（正常 / 幂等 / 冲突 / 缺口 / 终态 / 部分扇出）---");
	{
		// 出口是**封闭集合**（能力的 outputs），三个出口各测一条语义：
		//   fix     → 两条边，都有人接        → routed=2
		//   partial → 一条有人接、一条没人接  → routed=1 / pruned=1（§8 部分扇出）
		//   solo    → 没有边                  → routed=0 / pruned=0（到此为止，不是失败）
		const emitCap = await defineCapability("ws.emit", ["fix", "partial", "solo"]);
		const emitA = await defineCapability("ws.emit.a", []);
		const emitB = await defineCapability("ws.emit.b", []);
		// 能力定义了但**没有任何节点**接它：no_node 剪枝的靶子
		const emitGhost = await defineCapability("ws.emit.ghost", []);

		H.eq("建边 ws.emit.fix → ws.emit.a", (await addEdge(emitCap, "fix", emitA)).status, 200);
		H.eq("建边 ws.emit.fix → ws.emit.b", (await addEdge(emitCap, "fix", emitB)).status, 200);
		H.eq("建边 ws.emit.partial → ws.emit.a", (await addEdge(emitCap, "partial", emitA)).status, 200);
		H.eq(
			"建边 ws.emit.partial → ws.emit.ghost（有边、无人接）",
			(await addEdge(emitCap, "partial", emitGhost)).status,
			200,
		);
		H.eq(
			"建入口边 ws.emit.src → ws.emit",
			(await addEntryEdge("ws.emit.src", "go", emitCap)).status,
			200,
		);

		// 用一条**干净的**连接：后面的断言是「拒收之后没有 error 帧、连接没关」，
		// 复用主节点会把上面 unknown_type / bad_frame 的历史帧混进来，变成恒假。
		const emitNode = await bringUpNode(
			server.base,
			server.dbPath,
			nsId,
			"ws-emit",
			[emitCap, emitA, emitB],
			4,
		);

		// emitAck 发一帧 emit 并等**这一帧**对应的 emit_ack。
		//
		// 必须按「发送前的帧数」切片：同一个 seq 会被重复发（幂等用例），只按 f.seq
		// 找会立刻命中上一条旧 ack，把「重复 seq 得到同一份结果」变成恒真的废话。
		const emitAck = async (body, timeoutMs = 4000) => {
			const from = emitNode.frames.length;
			emitNode.conn.send({ type: "emit", ...body });
			const deadline = Date.now() + timeoutMs;
			while (Date.now() < deadline) {
				const hit = emitNode.frames
					.slice(from)
					.find((f) => f.type === "emit_ack" && f.seq === body.seq);
				if (hit) return hit;
				await new Promise((r) => setTimeout(r, 20));
			}
			return null;
		};
		// children 读某条任务的直接下游（emit 建出来的那些行）。
		const children = (parentId) =>
			dbRows(
				`SELECT id, capability, status, params FROM tasks WHERE parent_task_id = ?`,
				parentId,
			);
		// emissions 读某条任务已落库的 emit 行（幂等 = 行不变；缺口 = 行不增）。
		const emissions = (taskId) =>
			dbRows(
				`SELECT seq, port, data, routed, pruned FROM emissions WHERE task_id = ? ORDER BY seq`,
				taskId,
			);

		// ---- 父任务：一次事件 → 一个 ws.emit 任务，派到这台节点 ----
		const parentEv = await postEvent({ source: "ws.emit.src", type: "go", payload: {} });
		H.eq("emit 用例：事件建出 1 个父任务", parentEv.json?.tasks?.length, 1);
		const parentTask = parentEv.json?.tasks?.[0]?.id;
		const parentAssign = await waitForFrame(
			emitNode.frames,
			(f) => f.type === "assign" && f.task_id === parentTask,
		);
		H.check(
			"父任务派到 emit 节点（下面所有 emit 都以它上报）",
			!!parentAssign,
			JSON.stringify(emitNode.frames.map((f) => f.type)),
		);

		// assign 的三个 P1 增补字段（都是**推导量**，不落库；§3.4）
		H.eq(
			"assign 带 ports（出口封闭集合的快照）",
			(parentAssign?.ports || []).slice().sort(),
			["fix", "partial", "solo"],
		);
		H.eq(
			"assign 带 expects.fix.targets（这个出口能去哪）",
			(parentAssign?.expects?.fix?.targets || []).map((t) => t.capability).sort(),
			["ws.emit.a", "ws.emit.b"],
		);
		H.check(
			"assign 带一次性任务令牌（本地 emit socket 鉴权）",
			(parentAssign?.token || "").length >= 16,
			// 令牌绝不进日志：只说长度
			`token 长度=${(parentAssign?.token || "").length}`,
		);

		// ---- ① 正常 emit：routed 正确 + 下游任务真的建出来 ----
		const ack1 = await emitAck({
			task_id: parentTask,
			seq: 1,
			port: "fix",
			data: { issue: "#42" },
			note: "第一次产出",
		});
		H.check("emit → 收到 emit_ack", !!ack1, JSON.stringify(emitNode.frames.map((f) => f.type)));
		H.eq("emit_ack.error 为空（成功）", ack1?.error || "", "");
		H.eq("一个端口两条边 → routed=2", ack1?.routed, 2);
		H.eq("成功路径 pruned=0", ack1?.pruned || 0, 0);
		H.eq("emit_ack.seq 回显上报的那个 seq", ack1?.seq, 1);
		H.eq("emit_ack.task_id 是上报的那条任务", ack1?.task_id, parentTask);

		const kids1 = children(parentTask);
		H.eq("① 下游任务真的建了两条", kids1.length, 2);
		H.eq(
			"① 两条下游任务落在两个目标上",
			kids1.map((k) => k.capability).sort(),
			["ws.emit.a", "ws.emit.b"],
		);
		// 事务边界（§7）：emission 与子任务行同一次提交，routed 存**首次**结果
		const em1 = emissions(parentTask);
		H.eq("① emissions 落了一行", em1.length, 1);
		H.eq("① emissions.port 是上报的出口", em1[0]?.port, "fix");
		H.eq("① emissions.routed 与 ack 一致", em1[0]?.routed, 2);

		// ---- ② 重复 seq（内容相同）→ 幂等：不重复派发 ----
		const ack1b = await emitAck({
			task_id: parentTask,
			seq: 1,
			port: "fix",
			data: { issue: "#42" },
			note: "重发（内容相同）",
		});
		H.eq("② 重复 seq：ack.error 仍为空（幂等成功）", ack1b?.error || "", "");
		H.eq("② 重复 seq：routed 取首次结果", ack1b?.routed, 2);
		H.eq("② 重复 seq：下游任务不多建", children(parentTask).length, 2);
		H.eq("② 重复 seq：emissions 仍然只有 1 行", emissions(parentTask).length, 1);

		// ---- ③ 同 seq 不同 port/data → 拒收（节点 bug，不能静默覆盖）----
		const ackReuse = await emitAck({
			task_id: parentTask,
			seq: 1,
			port: "solo",
			data: { issue: "#43" },
		});
		H.check(
			"③ 同 seq 不同内容 → ack 带 error（不静默覆盖）",
			/reused/i.test(ackReuse?.error || ""),
			JSON.stringify(ackReuse),
		);
		H.eq("③ 被拒的那次不建任务", children(parentTask).length, 2);
		H.eq("③ 被拒的那次不改 emissions", emissions(parentTask).length, 1);

		// ---- ④ 部分扇出：一条边有人接、一条没人接（§8）----
		const ack2 = await emitAck({ task_id: parentTask, seq: 2, port: "partial", data: {} });
		H.eq("④ 部分扇出：ack.error 为空（剪枝不是失败）", ack2?.error || "", "");
		H.eq("④ 一条边无节点 → routed=1", ack2?.routed, 1);
		H.eq("④ 一条边无节点 → pruned=1", ack2?.pruned, 1);
		const kids2 = children(parentTask);
		H.eq("④ 下游总行数 = 3（2 + 被派出去的 1）", kids2.length, 3);
		H.eq(
			"④ 被剪的那条（no_node）不建任务行",
			kids2.filter((k) => k.capability === "ws.emit.ghost").length,
			0,
		);

		// ---- solo：出口没接下游 → 0/0，不是错误（§8）----
		const ack3 = await emitAck({ task_id: parentTask, seq: 3, port: "solo", data: {} });
		H.eq("出口没接下游：ack.error 为空", ack3?.error || "", "");
		H.eq("出口没接下游：routed=0", ack3?.routed || 0, 0);
		H.eq("出口没接下游：pruned=0", ack3?.pruned || 0, 0);
		H.eq("出口没接下游：不建任务", children(parentTask).length, 3);

		// ---- ⑤ 缺口：1..seq-1 必须齐全（seq=5 而 4 缺失）----
		const ackGap = await emitAck({ task_id: parentTask, seq: 5, port: "fix", data: { issue: "#44" } });
		H.check(
			"⑤ 缺口 → ack 带 error（丢消息必须可见，不猜不跳过）",
			/gap/i.test(ackGap?.error || ""),
			JSON.stringify(ackGap),
		);
		H.eq("⑤ 缺口：不建任务", children(parentTask).length, 3);
		H.eq("⑤ 缺口：不落 emission（emissions 仍是 3 行）", emissions(parentTask).length, 3);
		// 拒收一条都没关连接、也没回 error 帧——这正是「不制造重连风暴」的判据
		H.check("⑤ 拒收后连接仍活着", !emitNode.conn.closed);
		H.check(
			"⑤ 拒收走 emit_ack.error，不夹带 error 帧",
			!emitNode.frames.some((f) => f.type === "error"),
			JSON.stringify(emitNode.frames.filter((f) => f.type === "error")),
		);

		// ---- ⑥ 已终结任务 emit → 拒收（§8：晚到的 emit 不许改已终结的任务）----
		emitNode.conn.send({ type: "ack", task_id: parentTask });
		await waitStatus(parentTask, "acked");
		emitNode.conn.send({ type: "progress", task_id: parentTask, note: "emit 用例收尾" });
		await waitStatus(parentTask, "running");
		emitNode.conn.send({
			type: "result",
			task_id: parentTask,
			status: "succeeded",
			summary: "emit 用例结束",
		});
		await waitStatus(parentTask, "succeeded");
		const ackClosed = await emitAck({ task_id: parentTask, seq: 4, port: "fix", data: {} });
		H.check(
			"⑥ 已终结任务 emit → ack 带 error",
			/not running/i.test(ackClosed?.error || ""),
			JSON.stringify(ackClosed),
		);
		H.eq("⑥ 终态被拒：不建任务", children(parentTask).length, 3);
		H.eq("⑥ 终态被拒：不落 emission", emissions(parentTask).length, 3);

		emitNode.conn.close();
	}

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
	const otherNode = await bringUpNode(server.base, server.dbPath, nsId, "ws-other", [capOther], 1);
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
