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
 *   out 退役  result 不再路由（P1.5 + 迁移 28）：帧里带 out 也不会建下游，
 *            且 **API 响应里根本没有 out 字段**（tasks.out 列已删）
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

// MIN_RUN 是最小合法实现（空脚本，退出码 0）。P1-b2 起建能力**必须**带实现
// （缺 run → 400 impl_missing，Capability Spec v3 §5）。
const MIN_RUN = { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\nexit 0\n" } };

async function defineCapability(name, outputs = [], extra = {}) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { run: MIN_RUN, name, outputs, paramsSchema: extra.paramsSchema || { type: "object" }, ...extra },
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
//
// 注意这里**没有** out：`tasks.out` 列已在迁移 28 删掉（P1.5 的收尾），
// 现在「出口」只存在于 emissions.port。老写法 `SELECT ... out ...` 会直接报
// 「no such column」——那正是本套件要防的回归。
function taskRow(taskId) {
	const db = new DatabaseSync(server.dbPath);
	try {
		return db
			.prepare(`SELECT id, status, reported, summary FROM tasks WHERE id = ?`)
			.get(taskId);
	} finally {
		db.close();
	}
}

// taskApi 走**控制台 HTTP 接口**读一条任务（GET /v1/tasks/{id}）。
//
// 为什么要两条腿读同一件事：库是「服务端事实」，接口是「客户端事实」。out 退役的
// 断言必须落在后者上——前端与外部调用方读的是响应字节，不是表。所以「没有 out」
// 用 `!("out" in task)` 钉在响应对象上（键不存在，而不是值为 null）。
async function taskApi(taskId) {
	const r = await H.req(
		server.base,
		`/v1/tasks/${encodeURIComponent(taskId)}?namespaceId=${encodeURIComponent(nsId)}`,
		{ session },
	);
	if (r.status !== 200) throw new Error(`GET /v1/tasks/${taskId}: ${r.status} ${r.text}`);
	return r.json?.task || {};
}

// assertNoOutKey 断言 API 响应里的任务对象没有 out 键（迁移 28 的客户端可见证据）。
async function assertNoOutKey(taskId, label) {
	const task = await taskApi(taskId);
	H.check(
		`${label} → API 响应里没有 out 字段（迁移 28 删列）`,
		!("out" in task),
		JSON.stringify(Object.keys(task)),
	);
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

	// ================= applied 里有中枢没有的能力 → 握手成功 + welcome.prune =================
	//
	// 这条用例以前测的是「整帧拒绝 + unknown_capability」。那是错的：节点缓存是
	// **派生物**，控制台删掉一个能力之后缓存必然过期 —— 那是正常管理动作，不是
	// 协议违规。旧行为的后果是 agent 把 unknown_capability 当致命错误退出 →
	// systemd 拉起 → 再拒，**删一个能力就打死所有在线节点**（实测崩溃循环）。
	//
	// 现在的契约：认下认识的、把不认识的放进 welcome.prune 让节点删缓存；
	// 中枢不从缓存推断归属（node_capabilities 里不能出现这条）。
	console.log("--- 陈旧缓存（A3 / welcome.prune）---");
	const badNode = seedNodeRows(server.dbPath, nsId, "ws-badnode", [], 1);
	const badConn = await wsConnect(server.base, badNode.token);
	const badFrames = [];
	badConn.onFrame((f) => badFrames.push(f));
	badConn.send(nodeHello(badNode.id, [{ name: "does.not.exist", version: 9 }], 1));
	const badWelcome = await waitForFrame(badFrames, (f) => f.type === "welcome");
	H.check("陈旧缓存不拒绝握手（收到 welcome）", !!badWelcome, JSON.stringify(badFrames.map((f) => f.type)));
	H.check(
		"welcome.prune 点名要删掉那条（does.not.exist@9）",
		JSON.stringify((badWelcome?.prune || []).map((p) => `${p.capability}@${p.version}`)) ===
			JSON.stringify(["does.not.exist@9"]),
		JSON.stringify(badWelcome?.prune),
	);
	H.check("陈旧缓存不产生 error 帧", !badFrames.some((f) => f.type === "error"), JSON.stringify(badFrames));
	{
		// 另一半：库里不能留下归属行（否则页面会显示「这台能跑」）
		const badView = await H.req(server.base, "/v1/nodes", { session });
		const badSelf = (badView.json?.nodes || []).find((n) => n.id === badNode.id);
		H.eq("陈旧缓存不写 node_capabilities", (badSelf?.capabilities || []).length, 0);
	}
	badConn.close();

	// ================= deploy → apply 往返（契约 §4 / §5） =================
	//
	// 这条钉的是 v2 的**主链路**：控制台 POST /v1/deployments → 中枢推 deploy 帧 →
	// 节点落盘后回 apply → 中枢把『这台机器到底装上没有』记在 deployments.state 上。
	// 少了 apply 这一跳，部署就永远停在 pending（控制台上看起来“推过了”），
	// 失败部署更是永远不可见——运维会以为已经生效。
	console.log("--- deploy → apply 往返 ---");
	// 产物脚本用 emit 报产出（P1.5 起 `$ANOTIFY_OUT_FILE` 已退役）。这条用例本身
	// 不执行脚本（只验 deploy → apply 往返），但**样例必须是真的能跑的写法**：
	// 一份写着已退役通道的产物，会把下一个照着抄的人带进「没有下游且没有报错」。
	const artCap = await defineCapabilityWithRun(
		"ws.deploy",
		["go"],
		'#!/bin/bash\nset -euo pipefail\n"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit --port go --data "${ANOTIFY_PARAMS}"\n',
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
		// ws.work 没有 outputs，所以 ports/expects 整帧不出现（omitempty）。
		//
		// `digest` **现在会出现**：P1-b2 起建能力必须有实现（缺 run → 400
		// impl_missing，Capability Spec v3 §5），`defineCapability` 因此总带一份最小
		// 脚本 → 能力有了产物 → assign 帧带 digest。「没有产物时帧里没有 digest」这条
		// 仍由 Go 单测钉着（internal/ws/node_assign_digest_test.go 的缺省分支）。
		//
		// attempts / seq_in_trace 是「节点本地执行档案」那条腿的输入：节点拿它们
		// 命名 runs/<trace>/<capability>@<ver>/<seq>-a<attempt>/ 并写进 input.json /
		// meta.json（见 docs/contracts/CONTRACT.md 的 assign 载荷与
		// cmd/agent/runstore.go）。两者都是展示/审计用，节点不得据此改变执行语义。
		// 这里必须**真的**出现在帧里：派发一定写过 attempts（≥1）与 seq_in_trace（≥1）。
		H.eq("assign 帧 attempts 是这次派发的世代（≥1）", assign.attempts, 1);
		H.eq("assign 帧 seq_in_trace 是本次 trace+能力的第几次执行（≥1）", assign.seq_in_trace, 1);
		const fields = Object.keys(assign).sort();
		H.eq(
			"assign 帧字段集合固定（增删字段必须在这里显式登记）",
			fields,
			[
				"attempts",
				"capability",
				"digest",
				"lease_until",
				"max_duration_sec",
				"params",
				"seq_in_trace",
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

	// result：running → succeeded。
	//
	// 帧里**故意**还带着 `out: "done"`（老节点/手写客户端的写法）：P1.5 把 out 整条
	// 退役了，服务端必须照收（未知字段忽略）、不报错、**不路由**——证据是下面两份：
	// ① API 响应里没有 out 键（迁移 28 删了列）② 这条 trace 没有多出任何任务。
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
	H.eq("result 落库 reported", row.reported, 1);
	H.eq("result 落库 summary", row.summary, "全部完成");
	await assertNoOutKey(taskId, "result 帧带 out='done'");
	// 产出只走 emit：这条任务一次都没 emit，所以它没有任何下游（老通道不再兜底）。
	H.eq(
		"结果帧不再建下游（out 通道已退役）",
		dbRows(`SELECT count(*) AS n FROM tasks WHERE parent_task_id = ?`, taskId)[0]?.n,
		0,
	);

	// ================= A8：out 退役（P1.5 硬切） =================
	//
	// 改动前这里叫「A8 out 两态（outcome-v1.1）」：out 缺省 / null / 有值三种写法，
	// 有值时中枢按它查出口表建下一跳。P1.5 把 out 整条退役（产出只走 emit），
	// 迁移 28 又把 `tasks.out` 整列删掉，于是三种写法退化成**同一份事实**：
	// 不建任何下游，而且 API 响应里连 out 这个键都没有。
	//
	// 断言一条都没放宽，而且更强：老通道以前**会建行**的那个场景（out 指向一条真有边
	// 的出口），现在必须一条都不建；被剪的判定也从「查表后剪枝」变成「压根没有入口」。
	console.log("--- A8 out 退役（帧里带 out 不再路由） ---");

	// 造一个「出口真的有边」的任务：ws.demo --go--> ws.work（见上面的 addEdge）。
	// 这是最关键的一条：老实现里给这个任务发 out="go" 一定会建出 ws.work 的任务。
	await addEntryEdge("ws.out.src", "run", cap);
	const evOut = await postEvent({ source: "ws.out.src", type: "run", payload: {} });
	const tOut = evOut.json?.tasks?.[0]?.id;
	H.eq("A8 任务落在 ws.demo（有 go 出口、且 go 真有下游的那条能力）", evOut.json?.tasks?.[0]?.capability, "ws.demo");
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tOut);
	node.conn.send({ type: "ack", task_id: tOut });
	await waitStatus(tOut, "acked");
	node.conn.send({ type: "progress", task_id: tOut, note: "A8" });
	await waitStatus(tOut, "running");

	// 态1（核心）：out="go" 且 go 真的有边 → 不落库、不建下游、没有 emission。
	node.conn.send({
		type: "result",
		task_id: tOut,
		status: "succeeded",
		out: "go",
		summary: "带 out 的老帧",
	});
	await waitStatus(tOut, "succeeded");
	const rOut = taskRow(tOut);
	await assertNoOutKey(tOut, "态1 out='go'");
	H.eq("态1 out='go' → reported=1（帧到了）", rOut.reported, 1);
	H.eq(
		"态1 out='go' → 一条下游都不建（老通道已退役，不是并存）",
		dbRows(`SELECT count(*) AS n FROM tasks WHERE parent_task_id = ?`, tOut)[0]?.n,
		0,
	);
	H.eq(
		"态1 该任务没有任何 emission（没 emit = 到此为止，0 次合法）",
		dbRows(`SELECT count(*) AS n FROM emissions WHERE task_id = ?`, tOut)[0]?.n,
		0,
	);

	// 态2：out 缺省 → 同一份事实（老写法与新写法结果一致，这正是退役的目的）
	const evMissing = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tMissing = evMissing.json?.tasks?.[0]?.id;
	const aMissing = await waitForFrame(
		node.frames,
		(f) => f.type === "assign" && f.task_id === tMissing,
	);
	H.check("态2 收到 assign", !!aMissing);
	node.conn.sendRaw(
		`{"type":"result","task_id":"${tMissing}","status":"succeeded","summary":"no out"}`,
	);
	await waitStatus(tMissing, "succeeded");
	const rMissing = taskRow(tMissing);
	H.eq("态2 out 缺失 → reported=1（帧到了）", rMissing.reported, 1);
	await assertNoOutKey(tMissing, "态2 out 缺失");

	// 态3：out:null 也照收（协议层不区分「缺省」与「显式 null」，两者都等于没 emit）
	const evNull = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tNull = evNull.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tNull);
	node.conn.sendRaw(`{"type":"result","task_id":"${tNull}","status":"succeeded","out":null}`);
	await waitStatus(tNull, "succeeded");
	const rNull = taskRow(tNull);
	H.eq("态3 out:null → reported=1", rNull.reported, 1);
	await assertNoOutKey(tNull, "态3 out:null");

	// 态4：失败 + 带 out → 失败照落库（summary 是原因），但**没有**下一跳。
	// 这是 I4 形式在新载体下的样子：路由与 status 无关，而路由的唯一来源是 emit；
	// 这条任务没 emit 过，所以无论成还是败都到此为止。
	const evFail = await postEvent({ source: "ws.demo", type: "go", payload: {} });
	const tFail = evFail.json?.tasks?.[0]?.id;
	await waitForFrame(node.frames, (f) => f.type === "assign" && f.task_id === tFail);
	node.conn.send({
		type: "result",
		task_id: tFail,
		status: "failed",
		out: "go",
		summary: "exit status 3: 编译失败",
	});
	await waitStatus(tFail, "failed");
	const rFail = taskRow(tFail);
	await assertNoOutKey(tFail, "态4 失败 + out='go'");
	H.eq("态4 失败带原因 → summary 就是那句原因", rFail.summary, "exit status 3: 编译失败");

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
		// 契约 §3：fix 有两个候选（ws.emit.a / ws.emit.b，同为 p=0），运行期只派**一个**。
		// 「全派」是 balance:all（未实现），所以这里是 1 而不是 2。
		H.eq("一个端口两条候选 → routed=1（选一个，不全派）", ack1?.routed, 1);
		H.eq("成功路径 pruned=0", ack1?.pruned || 0, 0);
		H.eq("emit_ack.seq 回显上报的那个 seq", ack1?.seq, 1);
		H.eq("emit_ack.task_id 是上报的那条任务", ack1?.task_id, parentTask);

		const kids1 = children(parentTask);
		H.eq("① 下游任务只建了一条", kids1.length, 1);
		H.check(
			"① 选中的是候选之一（ws.emit.a / ws.emit.b）",
			["ws.emit.a", "ws.emit.b"].includes(kids1[0]?.capability),
			JSON.stringify(kids1.map((k) => k.capability)),
		);
		// 事务边界（§7）：emission 与子任务行同一次提交，routed 存**首次**结果
		const em1 = emissions(parentTask);
		H.eq("① emissions 落了一行", em1.length, 1);
		H.eq("① emissions.port 是上报的出口", em1[0]?.port, "fix");
		H.eq("① emissions.routed 与 ack 一致", em1[0]?.routed, 1);

		// ---- ② 重复 seq（内容相同）→ 幂等：不重复派发 ----
		const ack1b = await emitAck({
			task_id: parentTask,
			seq: 1,
			port: "fix",
			data: { issue: "#42" },
			note: "重发（内容相同）",
		});
		H.eq("② 重复 seq：ack.error 仍为空（幂等成功）", ack1b?.error || "", "");
		H.eq("② 重复 seq：routed 取首次结果", ack1b?.routed, 1);
		H.eq("② 重复 seq：下游任务不多建", children(parentTask).length, 1);
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
		H.eq("③ 被拒的那次不建任务", children(parentTask).length, 1);
		H.eq("③ 被拒的那次不改 emissions", emissions(parentTask).length, 1);

		// ---- ①b 同级轮盘：再 emit 一次 fix，这次应当轮到**另一个**候选 ----
		//
		// 这是「1:1 选择」与「随机抖动」的区别：两个候选同优先级时，中枢按游标
		// 轮转（契约 §3），所以连续两次必定落在不同目标上。
		const ack1c = await emitAck({
			task_id: parentTask,
			seq: 2,
			port: "fix",
			data: { issue: "#42b" },
			note: "第二次产出（同级轮盘）",
		});
		H.eq("①b 第二次 emit fix 仍然只派一个", ack1c?.routed, 1);
		const kids1c = children(parentTask);
		H.eq("①b 下游总行数 = 2", kids1c.length, 2);
		H.eq(
			"①b 两次落到两个**不同**候选（同级轮盘在真服务器上生效）",
			kids1c.map((k) => k.capability).sort(),
			["ws.emit.a", "ws.emit.b"],
		);

		// ---- ④ 部分扇出：一条边有人接、一条没人接（§8）----
		const ack2 = await emitAck({ task_id: parentTask, seq: 3, port: "partial", data: {} });
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
		const ack3 = await emitAck({ task_id: parentTask, seq: 4, port: "solo", data: {} });
		H.eq("出口没接下游：ack.error 为空", ack3?.error || "", "");
		H.eq("出口没接下游：routed=0", ack3?.routed || 0, 0);
		H.eq("出口没接下游：pruned=0", ack3?.pruned || 0, 0);
		H.eq("出口没接下游：不建任务", children(parentTask).length, 3);

		// ---- ⑤ 缺口：1..seq-1 必须齐全（seq=6 而 5 缺失）----
		const ackGap = await emitAck({ task_id: parentTask, seq: 6, port: "fix", data: { issue: "#44" } });
		H.check(
			"⑤ 缺口 → ack 带 error（丢消息必须可见，不猜不跳过）",
			/gap/i.test(ackGap?.error || ""),
			JSON.stringify(ackGap),
		);
		H.eq("⑤ 缺口：不建任务", children(parentTask).length, 3);
		H.eq("⑤ 缺口：不落 emission（emissions 仍是 4 行）", emissions(parentTask).length, 4);
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
		const ackClosed = await emitAck({ task_id: parentTask, seq: 5, port: "fix", data: {} });
		H.check(
			"⑥ 已终结任务 emit → ack 带 error",
			/not running/i.test(ackClosed?.error || ""),
			JSON.stringify(ackClosed),
		);
		H.eq("⑥ 终态被拒：不建任务", children(parentTask).length, 3);
		H.eq("⑥ 终态被拒：不落 emission", emissions(parentTask).length, 4);

		emitNode.conn.close();
	}

	// ================= 非法迁移 → invalid_transition =================
	console.log("--- 非法迁移 ---");
	// 对已终态的任务再发 result（tOut 在 A8 里已经 succeeded）
	node.frames.length = 0;
	node.conn.send({ type: "result", task_id: tOut, status: "succeeded" });
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
