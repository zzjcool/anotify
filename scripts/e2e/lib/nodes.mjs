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
	if (!namespaceId) {
		throw new Error(
			"seedNodeRows: 需要 namespaceId（用 H.defaultNamespaceId(base, session) 取，不要硬编码）",
		);
	}
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
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		const hit = frames.find(pred);
		if (hit) return hit;
		await new Promise((r) => setTimeout(r, 20));
	}
	return null;
}

export { wsConnect, waitFrame };
