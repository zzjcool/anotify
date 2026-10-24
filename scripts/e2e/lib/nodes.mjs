/* 节点播种 + 上线的共享辅助（routing / ws_protocol 共用）。
 *
 * 为什么节点要直接写库：`CreateNode` 是 store 层 API，**没有 HTTP 端点**——
 * 节点注册是运维动作（`anotify node create`），不是自助服务。
 * 所以 e2e 必须有一个后门来造节点。
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

// seedNodeRows 往库里插一行节点（token 已知），可选声明能力与实例。
//
// 返回 { id, token }。token 是明文，DB 里只存 sha256。
export function seedNodeRows(dbPath, orgId, name, capabilityIds = [], slots = 1) {
	const token = "{{SECRET_dg3rvnxb}}" + randomUUID().replace(/-/g, "");
	const nodeId = "nd_e2e_" + randomUUID().replace(/-/g, "").slice(0, 20);
	const now = Math.floor(Date.now() / 1000);

	const db = new DatabaseSync(dbPath);
	try {
		db.prepare(
			`INSERT INTO nodes (id, org_id, name, tags, status, token_hash, max_instances, max_concurrency, created_at)
			 VALUES (?, ?, ?, '[]', 'offline', ?, 20, 8, ?)`,
		).run(nodeId, orgId, name, sha256(token), now);
		for (const capId of capabilityIds) {
			db.prepare(
				`INSERT INTO node_capabilities (node_id, capability_id) VALUES (?, ?)`,
			).run(nodeId, capId);
		}
		for (let i = 0; i < slots; i++) {
			db.prepare(
				`INSERT INTO node_instances (id, node_id, slot, concurrency, alive, updated_at)
				 VALUES (?, ?, ?, 1, 1, ?)`,
			).run(`nins_e2e_${nodeId}_${i}`, nodeId, i, now);
		}
	} finally {
		db.close();
	}
	return { id: nodeId, token };
}

// nodeHello 构造 hello 帧。
export function nodeHello(nodeId, caps = [], slots = 1, protocol = 1) {
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
// 返回 { id, token, conn, frames }。frames 是已收到的所有帧（含 welcome）。
// 握手失败时抛出，带上服务端下发的 error 帧便于定位。
export async function bringUpNode(serverBase, dbPath, orgId, name, caps = [], slots = 1) {
	const node = seedNodeRows(dbPath, orgId, name, caps.map((c) => c.id), slots);
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
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		const hit = frames.find(pred);
		if (hit) return hit;
		await new Promise((r) => setTimeout(r, 20));
	}
	return null;
}

export { wsConnect, waitFrame };
