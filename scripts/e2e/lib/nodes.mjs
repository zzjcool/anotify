/* 节点播种 + 上线的共享辅助（routing / ws_protocol 共用）。
 *
 * 为什么节点要直接写库：直接写库才能**指定明文 token**（`CreateNode` 只在
 * 201/返回时给一次明文，库里只有 sha256），而这里需要在握手时用同一个 token。
 * 注：`POST /v1/nodes` 自 #185 起已存在（节点页 #198 起也能从 UI 注册），
 * 但那条路拿到的 token 要跨进程传，不适合当作 e2e 的播种手段。
 *
 * 但**上线**必须走真实 WS 握手（hello → welcome）：只有握手成功，
 * 服务端的 NodeHub 才会注册这条连接，派发器才推得到 assign 帧。
 */
import { DatabaseSync } from "node:sqlite";
import { createHash, randomUUID } from "node:crypto";
import { wsConnect, waitFrame } from "./wsclient.mjs";

export function sha256(s) {
	return createHash("sha256").update(s).digest("hex");
}

// ---------- 入口参数校验（#210） ----------
//
// 为什么这些校验必须存在：本文件的辅助把参数**直接绑进 sqlite**。绑定 undefined
// 时 node:sqlite 抛的是
//
//   TypeError: Provided value cannot be bound to SQLite parameter 2.
//
// 位置在造数深处（#210 那次就是 node_capabilities 的 INSERT），而套件崩在 await
// 里不会写 JSON 结果 → 汇总显示「0通过/0失败」。这个信号和超时 / 崩溃 / 环境问题
// 完全没法区分（本轮已经为它误判过一次），排查成本极高。
//
// 所以在入口把「调用方拿了个 undefined id」变成一句能直接定位的话，例如：
//
//   seedNodeRows: capabilityIds[2] 是 undefined（上游建能力失败？）
//
// 铁律：**不跳过**无效项、也不做静默兜底。自动跳过会把调用方的 bug 变成静默丢
// 数据 —— 用例照样绿，但测的已经不是它声称的东西了。
const HINT_UPSTREAM = "上游建能力失败？";

// show 把值渲染成短且可区分的形式：undefined / null / "" / "undefined" 必须一眼
// 分得开（"undefined" 这种字符串是最坑的一种，它不是 bug）。
function show(v) {
	if (v === undefined) return "undefined";
	if (v === null) return "null";
	if (typeof v === "string") return JSON.stringify(v);
	if (typeof v === "bigint") return `${v}n`;
	try {
		const s = JSON.stringify(v);
		return s === undefined ? String(v) : s;
	} catch {
		return String(v);
	}
}

function fail(fn, msg, hint) {
	throw new Error(`${fn}: ${msg}${hint ? `（${hint}）` : ""}`);
}

// needText 校验「非空字符串」并原样返回，便于链式使用。
function needText(fn, label, value, hint) {
	if (typeof value !== "string" || value === "") {
		fail(fn, `${label} 必须是非空字符串，实际是 ${show(value)}`, hint);
	}
	return value;
}

// needCount 校验「非负整数」（slots / protocol 这类计数与版本号）。
// 不校验的话，slots=undefined 会让 node_instances 一行都不插 —— 静默少造数据。
function needCount(fn, label, value, hint, { min = 0 } = {}) {
	if (!Number.isInteger(value) || value < min) {
		fail(fn, `${label} 必须是 >= ${min} 的整数，实际是 ${show(value)}`, hint);
	}
	return value;
}

// needIdList 校验「元素全是非空字符串的数组」，报错**带下标与实际值**。
// 这是 #210 的元凶入口：capabilityIds 里混进 undefined → 绑库崩在深处。
function needIdList(fn, label, value, hint) {
	if (!Array.isArray(value)) {
		fail(fn, `${label} 必须是数组，实际是 ${show(value)}`, "应传入能力 id 数组，不要传单个 id");
	}
	for (let i = 0; i < value.length; i++) {
		const item = value[i];
		if (typeof item !== "string" || item === "") {
			fail(fn, `${label}[${i}] 是 ${show(item)}`, hint);
		}
	}
	return value;
}

// needCapList 校验能力声明数组（hello 帧与 bringUpNode 的 caps）。
// requireId：bringUpNode 要把 id 落进 node_capabilities，缺 id 就是 #210 那种
// 「undefined 绑库」；nodeHello 只发 name@version，不需要 id。
function needCapList(fn, label, value, { requireId, hint = HINT_UPSTREAM }) {
	if (!Array.isArray(value)) {
		fail(fn, `${label} 必须是数组，实际是 ${show(value)}`, "应传入 [{id?, name, version}] 数组");
	}
	for (let i = 0; i < value.length; i++) {
		const cap = value[i];
		if (cap === null || typeof cap !== "object") {
			fail(fn, `${label}[${i}] 必须是 {id?, name, version} 对象，实际是 ${show(cap)}`, hint);
		}
		if (requireId && (typeof cap.id !== "string" || cap.id === "")) {
			fail(fn, `${label}[${i}].id 是 ${show(cap.id)}`, hint);
		}
		if (typeof cap.name !== "string" || cap.name === "") {
			fail(fn, `${label}[${i}].name 是 ${show(cap.name)}`, "上游建能力失败？能力名应为非空字符串");
		}
		if (!Number.isInteger(cap.version) || cap.version < 1) {
			fail(fn, `${label}[${i}].version 是 ${show(cap.version)}`, "能力版本应是正整数（契约 §6.1）");
		}
	}
	return value;
}

// seedNodeRows 往库里插一行节点（token 已知），可选声明能力与实例。
//
// namespaceId 必传：nodes / node_capabilities / node_instances 都是命名空间资源
// （namespace_id TEXT NOT NULL，唯一键 UNIQUE(namespace_id, name)）。节点属于哪个
// 命名空间必须由调用方给**查出来的** id（`await H.defaultNamespaceId(base, session)`）
// ——用例里写死 ns_xxx 等换库全红。
//
// **#204 / 迁移 14**：组织层删除，nodes 表不再有 org_id 列，所以这里没有 orgId 参数。
//
// 返回 { id, token }。token 是明文，DB 里只存 sha256。
export function seedNodeRows(
	dbPath,
	namespaceId,
	name,
	capabilityIds = [],
	slots = 1,
) {
	// #210：入口校验。参数不对就在**这里**炸，别让它变成「0通过/0失败」。
	needText("seedNodeRows", "dbPath", dbPath);
	needText(
		"seedNodeRows",
		"namespaceId",
		namespaceId,
		"用 H.defaultNamespaceId(base, session) 取，不要硬编码",
	);
	needText("seedNodeRows", "name", name);
	needIdList("seedNodeRows", "capabilityIds", capabilityIds, HINT_UPSTREAM);
	needCount("seedNodeRows", "slots", slots);
	const token = "{{SECRET_dg3rvnxb}}" + randomUUID().replace(/-/g, "");
	const nodeId = "nd_e2e_" + randomUUID().replace(/-/g, "").slice(0, 20);
	const now = Math.floor(Date.now() / 1000);

	const db = new DatabaseSync(dbPath);
	try {
		db.prepare(
			`INSERT INTO nodes (id, namespace_id, name, tags, status, token_hash, max_instances, max_concurrency, created_at)
			 VALUES (?, ?, ?, '[]', 'offline', ?, 20, 8, ?)`,
		).run(nodeId, namespaceId, name, sha256(token), now);
		for (const capId of capabilityIds) {
			// 上面 needIdList 已经把 undefined/"" 挡在入口了，这里再断言一次成本为零，
			// 但能挡住「以后有人绕过入口改直插」——#210 崩的就是这一行。
			if (typeof capId !== "string" || capId === "") {
				throw new Error(
					`seedNodeRows: capability_id 准备落库时是 ${show(capId)}（上游建能力失败？）`,
				);
			}
			db.prepare(
				`INSERT INTO node_capabilities (node_id, capability_id, namespace_id) VALUES (?, ?, ?)`,
			).run(nodeId, capId, namespaceId);
		}
		for (let i = 0; i < slots; i++) {
			db.prepare(
				`INSERT INTO node_instances (id, node_id, slot, concurrency, alive, updated_at, namespace_id)
				 VALUES (?, ?, ?, 1, 1, ?, ?)`,
			).run(`nins_e2e_${nodeId}_${i}`, nodeId, i, now, namespaceId);
		}
	} finally {
		db.close();
	}
	return { id: nodeId, token };
}

// nodeHello 构造 hello 帧。
// #210：同样在入口校验 —— hello 里 cap.name/cap.version 是 undefined 时服务端只会
// 回一个语义模糊的 error 帧，看起来像「能力没注册」，实际是造数时就没拿到 id/name。
export function nodeHello(nodeId, caps = [], slots = 1, protocol = 1) {
	needText("nodeHello", "nodeId", nodeId);
	needCapList("nodeHello", "caps", caps, { requireId: false });
	needCount("nodeHello", "slots", slots);
	needCount("nodeHello", "protocol", protocol, "hello 的协议版本", { min: 1 });
	return {
		type: "hello",
		node_id: nodeId,
		protocol,
		capabilities: caps.map((c) => ({ capability: c.name, version: c.version })),
		instances: Array.from({ length: slots }, (_, i) => ({ slot: i, concurrency: 1 })),
	};
}

// bringUpNode 播种节点 + 真实 WS 握手 + 等 welcome。
//
// namespaceId 决定节点归属于哪个命名空间（hello 帧里的能力名只在这个命名空间内解析，
// 见契约 §2.1）；必须与建能力/边时用的命名空间一致，否则派发会找不到节点。
//
// 签名里**没有 orgId**（#204 / 迁移 14：组织层删除，令牌只绑命名空间）。
//
// 返回 { id, token, conn, frames }。frames 是已收到的所有帧（含 welcome）。
// 握手失败时抛出，带上服务端下发的 error 帧便于定位。
export async function bringUpNode(
	serverBase,
	dbPath,
	namespaceId,
	name,
	caps = [],
	slots = 1,
) {
	// #210：caps 里的 {id,name,version} 都来自「上一步建能力」。建能力失败时
	// cap.id 就是 undefined —— 先在这里报出来（带下标），再谈握手。
	needText("bringUpNode", "serverBase", serverBase);
	needText(
		"bringUpNode",
		"namespaceId",
		namespaceId,
		"用 H.defaultNamespaceId(base, session) 取，不要硬编码",
	);
	needText("bringUpNode", "name", name);
	needCapList("bringUpNode", "caps", caps, { requireId: true });
	needCount("bringUpNode", "slots", slots);

	const node = seedNodeRows(
		dbPath,
		namespaceId,
		name,
		caps.map((c) => c.id),
		slots,
	);
	const conn = await wsConnect(serverBase, node.token);

	const frames = [];
	conn.onFrame((f) => frames.push(f));

	conn.send(nodeHello(node.id, caps, slots));

	// 等 welcome（最多 3s）
	const deadline = Date.now() + 3000;
	while (Date.now() < deadline) {
		if (frames.some((f) => f.type === "welcome")) break;
		await new Promise((r) => setTimeout(r, 25));
	}
	if (!frames.some((f) => f.type === "welcome")) {
		const err = frames.find((f) => f.type === "error");
		conn.close();
		throw new Error(
			`节点 ${name} 握手失败: ${JSON.stringify(err || frames)}`,
		);
	}
	return { ...node, conn, frames };
}

// waitForFrame 等一帧满足条件的消息（轮询已收集的 frames）。
export async function waitForFrame(frames, pred, timeoutMs = 3000) {
	// 传错 frames / pred 时，这里会静默轮询到超时再返回 null，用例只看到「没等到」。
	if (!Array.isArray(frames)) {
		fail("waitForFrame", `frames 必须是数组，实际是 ${show(frames)}`);
	}
	if (typeof pred !== "function") {
		fail("waitForFrame", `pred 必须是函数，实际是 ${show(pred)}`);
	}
	needCount("waitForFrame", "timeoutMs", timeoutMs, "等待时长应是正整数毫秒", { min: 1 });
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		const hit = frames.find(pred);
		if (hit) return hit;
		await new Promise((r) => setTimeout(r, 20));
	}
	return null;
}

export { wsConnect, waitFrame };
