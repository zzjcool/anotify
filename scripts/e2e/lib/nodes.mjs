/* 节点播种 + 上线的共享辅助（routing / ws_protocol / node_v4 共用）。
 *
 * ============================ v4 口径（必读） ============================
 *
 * 帧契约唯一来源是 `internal/protocol`（Go 侧）。本文件是它在 e2e 里的**唯一**镜像，
 * 严禁在套件里手写第二种 hello 形状。
 *
 * v4 hello（domain：config.md §4.2 / anode.md §3.3）：
 *
 *   {type:"hello", name, token, namespaces[], localVars{}, localLimits{},
 *    appliedRevision, appliedHash, agentVersion, agentCommit, facts{}, capabilities[], instances[]}
 *
 * 与已退役的 v2 hello（`{type:"hello", node_id, protocol:2, applied[], instances[{slot,concurrency}]}`）
 * 的三处根本差别，也是本文件被改写的原因：
 *
 *   1. **身份在帧里**：`token` 是节点身份（库里只有 sha256），`name` 必须与 token 所属
 *      节点一致。v2 的 `node_id` 不是凭据 —— 服务端 v4 见到它只会当成未知字段忽略，
 *      然后在 `authenticate` 里因为「hello 缺少 token」把连接关掉。
 *   2. **ns 是申报不是参数**：`namespaces` 是本机申报列表（生效 = 申报 ∩ 中枢授权），
 *      v4 不再有 `?namespace=` 那种握手参数。
 *   3. **能力是探测结果不是缓存清单**：`capabilities[]` 带每个能力的
 *      `requires.commands` 探测结果；`instances[]` 是**物理槽位事实**（`busy` 有
 *      presence：缺字段 = 未知，不能当空闲）。
 *
 * 没有 v2 fallback：留一个「形状不对就退回老帧」的分支，正是让迁移永远做不完的原因
 * （两边都通过，谁都发现不了中枢其实只认一边）。
 *
 * ---------------------------- 造数与真实性的边界 ----------------------------
 *
 * `seedNodeRows` 直接写库，只为**指定明文 token**（`POST /v1/nodes/{name}/tokens` 只在
 * 响应里给一次明文，跨进程传不了）。它**不伪造执行事实**：不写 tasks、不写 result、
 * 不写 deployments。
 *
 * **v4 的 nodes 是 cluster 对象**：表里没有 namespace_id，能力归属由 `deployments`
 * 派生（v3 的 `node_capabilities` 表已随 v4 重建删除）。所以本文件的 seed 只写
 * `nodes` 一行 + 申报的 `node_instances` 槽位——「这台机器装着哪个能力」是部署动作，
 * 由 `POST …/capabilities/{name}/deploy` 产生，e2e 不能替产品记这条账。
 *
 * 需要**真节点**的套件（node_v4）走真实产品路径：
 * `POST /v1/nodes` → `POST /v1/nodes/{name}/tokens` → 起 `cmd/anode` 二进制。
 * 本文件的 mock 节点只用于「中枢侧的握手/派发语义」这类不需要执行器的断言。
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
//   bringUpNode: namespaces[1] 是 undefined
//
// 铁律：**不跳过**无效项、也不做静默兜底。自动跳过会把调用方的 bug 变成静默丢
// 数据 —— 用例照样绿，但测的已经不是它声称的东西了。

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

// needCount 校验「非负整数」（slots 这类计数）。
// 不校验的话，slots=undefined 会让 node_instances 一行都不插 —— 静默少造数据。
function needCount(fn, label, value, hint, { min = 0 } = {}) {
	if (!Number.isInteger(value) || value < min) {
		fail(fn, `${label} 必须是 >= ${min} 的整数，实际是 ${show(value)}`, hint);
	}
	return value;
}

// needNameList 校验「元素全是非空字符串的数组」（hello.namespaces）。
//
// v4 的 namespaces 是**申报**列表：元素是 ns 的 slug（人读名），不是 `ns_…` id。
// 传 id 进来不会报错，只会让「申报 ∩ 授权」永远为空 —— 症状是节点在线但不接活，
// 所以这里顺手把明显是 id 的形状点名出来。
function needNameList(fn, label, value, hint) {
	if (!Array.isArray(value)) {
		fail(fn, `${label} 必须是数组，实际是 ${show(value)}`, hint || "应传入 ns 的 slug 数组，如 [\"default\"]");
	}
	for (let i = 0; i < value.length; i++) {
		const item = value[i];
		if (typeof item !== "string" || item === "") {
			fail(fn, `${label}[${i}] 是 ${show(item)}`, hint);
		}
		if (item.startsWith("ns_")) {
			fail(fn, `${label}[${i}] 是 ${show(item)}，看起来是命名空间 id`, "hello.namespaces 要的是 slug（如 default），id 会让「申报 ∩ 授权」为空");
		}
	}
	return value;
}

// needCapList 校验能力声明数组（hello.capabilities 的来源）。
//
// v4 的一条是 `{name, version, digest?, commands?[]}`：中枢用它在
// `requires.commands` 匹配里回答 CommandMissing。不再校验 `id`：v4 没有
// node_capabilities 表，「这台机器装着哪个能力」由 deployments 表达（部署动作）。
function needCapList(fn, label, value, { hint } = {}) {
	if (!Array.isArray(value)) {
		fail(fn, `${label} 必须是数组，实际是 ${show(value)}`, "应传入 [{name, version}] 数组");
	}
	for (let i = 0; i < value.length; i++) {
		const cap = value[i];
		if (cap === null || typeof cap !== "object") {
			fail(fn, `${label}[${i}] 必须是 {name, version} 对象，实际是 ${show(cap)}`, hint);
		}
		if (typeof cap.name !== "string" || cap.name === "") {
			fail(fn, `${label}[${i}].name 是 ${show(cap.name)}`, "能力名应为非空字符串（同 spec.metadata.name 正则）");
		}
		if (!Number.isInteger(cap.version) || cap.version < 1) {
			fail(fn, `${label}[${i}].version 是 ${show(cap.version)}`, "能力版本应是正整数（capability.md §2.2）");
		}
	}
	return value;
}

// seedNodeRows 往库里插一行节点（token 已知），可选申报实例槽位。
//
// **v4 形状**：`nodes` 是 cluster 对象（无 namespace_id；唯一键是 name），
// `node_instances` 是物理槽位（slot 0..N-1，每槽最多一个能力进程）。
// v3 的 `node_capabilities` 表已删除 —— 能力归属是 `deployments` 的事实，不在这里造。
//
// 形参 `namespaceId` / `capabilityIds` **保留只为调用点兼容**：
//   - namespaceId：仍要求传真值（`H.defaultNamespaceId(...)` 的结果）。v4 里 ns 归属
//     由 hello.namespaces（申报）与 node_configs.namespaces（授权）表达，不在 nodes 行上；
//     保留形参是为了让「还按 v3 调用的套件」在这里**当场**报错，而不是静默少造数据。
//   - capabilityIds：v4 已无对应表。非空会被显式拒绝（见下），不做静默丢弃。
//
// 返回 { id, token }。token 是明文，DB 里只存 sha256。
export function seedNodeRows(
	dbPath,
	namespaceId,
	name,
	capabilityIds = [],
	slots = 1,
) {
	const FN = "seedNodeRows";
	// #210：入口校验。参数不对就在**这里**炸，别让它变成「0通过/0失败」。
	needText(FN, "dbPath", dbPath);
	needText(FN, "namespaceId", namespaceId, "用 H.defaultNamespaceId(base, session) 取，不要硬编码");
	needText(FN, "name", name);
	needCount(FN, "slots", slots);
	if (!Array.isArray(capabilityIds)) {
		fail(FN, `capabilityIds 必须是数组，实际是 ${show(capabilityIds)}`);
	}
	if (capabilityIds.length > 0) {
		// 静默忽略 = 套件以为「这台节点装着某能力」，实际库上什么都没有，断言随后
		// 在派发处失败，看起来像产品 bug。v4 的答案是部署动作，不是插表。
		fail(
			FN,
			`capabilityIds 非空（${capabilityIds.length} 项），但 v4 没有 node_capabilities 表`,
			"能力归属由 deployments 派生：要声明「这台机器装着某能力」，请走 POST /v1/namespaces/{ns}/capabilities/{name}/deploy 并让节点回 capability.deployed",
		);
	}
	const token = "e2eNodeToken" + randomUUID().replace(/-/g, "");
	const nodeId = "nd_e2e_" + randomUUID().replace(/-/g, "").slice(0, 20);
	const now = Math.floor(Date.now() / 1000);

	const db = new DatabaseSync(dbPath);
	try {
		db.prepare(
			`INSERT INTO nodes (id, name, tags, status, token_hash, created_at)
			 VALUES (?, ?, '[]', 'offline', ?, ?)`,
		).run(nodeId, name, sha256(token), now);
		for (let slot = 0; slot < slots; slot++) {
			db.prepare(
				`INSERT INTO node_instances (id, node_id, slot, alive, updated_at)
				 VALUES (?, ?, ?, 1, ?)`,
			).run(`nins_e2e_${nodeId}_${slot}`, nodeId, slot, now);
		}
	} finally {
		db.close();
	}
	return { id: nodeId, token };
}

// PLACEHOLDER_DIGEST 是 e2e 用的占位 digest。
//
// 中枢对 hello.capabilities[].digest 的对账只对**已请求的部署行**生效（deployments
// 表里没有这个 digest 的行时，它只是「节点缓存了什么」的陈述）。所以不关心产物内容
// 的套件给一个固定的占位 digest 就够。
export const PLACEHOLDER_DIGEST = "sha256:" + "0".repeat(64);

// nodeHello 构造 v4 hello 帧（**对象参数**）。
//
//   nodeHello({ name, token, namespaces?, capabilities?, slots?, ... })
//
// 为什么改成对象参数：v4 hello 有 12 个字段，位置参数已经读不懂谁是谁，而
// `(nodeId, caps, slots, protocol)` 这种形状本身就是 v2 的（nodeId 不是凭据、
// protocol 字段 v4 根本没有）。传旧形状会得到一句明确的迁移提示，而不是「服务端
// 静默关了连接，理由是 hello 缺 token」这种要读服务端日志才知道的失败。
export function nodeHello(opts) {
	const FN = "nodeHello";
	if (typeof opts === "string") {
		// 旧调用点（(nodeId, caps, slots, protocol)）：直接点名，不做形状猜测。
		fail(
			FN,
			"收到位置参数，v4 要求对象参数 nodeHello({ name, token, namespaces, slots, ... })",
			"旧形状 (nodeId, caps, slots, protocol) 是协议 v2 的；v4 身份是 hello.token，没有 protocol 字段",
		);
	}
	if (opts === null || typeof opts !== "object" || Array.isArray(opts)) {
		fail(FN, `参数必须是对象，实际是 ${show(opts)}`);
	}
	needText(FN, "name", opts.name);
	needText(FN, "token", opts.token, "token 是节点身份（明文只在签发响应里出现一次）");
	const namespaces = needNameList(
		FN,
		"namespaces",
		opts.namespaces === undefined ? ["default"] : opts.namespaces,
	);
	const capabilities = needCapList(
		FN,
		"capabilities",
		opts.capabilities === undefined ? [] : opts.capabilities,
	);
	const slots = needCount(FN, "slots", opts.slots === undefined ? 1 : opts.slots);
	const localVars = opts.localVars === undefined ? {} : opts.localVars;
	if (localVars === null || typeof localVars !== "object" || Array.isArray(localVars)) {
		fail(FN, `localVars 必须是对象，实际是 ${show(localVars)}`);
	}
	// localLimits 是**本机**预算（逐叶压住中枢节点默认值）。total 缺省按 slots 报，
	// 否则中枢算出来的机器级预算会是 0，派发永远批不下来（症状：任务一直 pending）。
	const localLimits = {
		total: slots,
		...(opts.localLimits || {}),
	};

	return {
		type: "hello",
		name: opts.name,
		token: opts.token,
		namespaces: [...namespaces],
		localVars: { ...localVars },
		localLimits,
		appliedRevision: opts.appliedRevision || "",
		appliedHash: opts.appliedHash || "",
		agentVersion: opts.agentVersion === undefined ? "e2e-mock" : opts.agentVersion,
		agentCommit: opts.agentCommit === undefined ? "e2e-mock-commit" : opts.agentCommit,
		// 本机内置事实（anode.md §3.6）。mock 没有真探测能力，但字段必须**在**：
		// 「节点没报」与「节点报了空」在中枢是两个不同的事实。
		facts:
			opts.facts ||
			{
				hostIP: "127.0.0.1",
				hostname: "e2e-mock",
				os: process.platform,
				arch: process.arch,
				cpu: 1,
				workRoot: "/tmp",
				homeDir: "/tmp",
			},
		// 能力条目的 commands 缺省是空数组（= 没有 requires.commands 的探测结果）。
		capabilities: capabilities.map((c) => ({
			name: c.name,
			version: c.version,
			digest: c.digest || PLACEHOLDER_DIGEST,
			commands: c.commands || [],
		})),
		// 槽位：slot 用数字（presence 由「有没有这个字段」表达），busy=false 是**明确**
		// 的「本机空闲」断言。缺 busy 是「未知」，中枢不能据此把槽当空闲。
		instances: Array.from({ length: slots }, (_, i) => ({ slot: i, busy: false })),
	};
}

// bringUpNode 播种节点 + 真实 WS 握手 + 等 welcome。
//
// 位置参数保持与既有调用点一致（(serverBase, dbPath, namespaceId, name, caps, slots)），
// 尾部可选的 opts 用于 v4 的新事实：
//
//   opts.namespaceSlug  hello.namespaces 里申报的 slug（默认 "default"）
//   opts.agentVersion   hello.agentVersion（默认 "e2e-mock"）
//   opts.localVars      hello.localVars
//
// 注意 `caps` 在 v4 只是 **hello 申报**（中枢据此回答 CommandMissing），不再落
// node_capabilities（表已删除）；因此 mock 节点不会被 `deployments` 认作「已部署」。
// 需要「真的能接活」的套件必须走真实部署（见 suites/node_v4.mjs）。
//
// 返回 { id, token, conn, frames }。frames 是已收到的所有帧（含 welcome）。
// 握手失败时抛出，带上服务端下发的帧与关闭原因便于定位。
export async function bringUpNode(
	serverBase,
	dbPath,
	namespaceId,
	name,
	caps = [],
	slots = 1,
	opts = {},
) {
	const FN = "bringUpNode";
	// #210：caps 里的 {name,version} 都来自「上一步建能力」。建能力失败时
	// cap.name 就是 undefined —— 先在这里报出来（带下标），再谈握手。
	needText(FN, "serverBase", serverBase);
	needText(FN, "namespaceId", namespaceId, "用 H.defaultNamespaceId(base, session) 取，不要硬编码");
	needText(FN, "name", name);
	needCapList(FN, "caps", caps);
	needCount(FN, "slots", slots);
	const namespaceSlug = opts.namespaceSlug === undefined ? "default" : opts.namespaceSlug;
	needText(FN, "opts.namespaceSlug", namespaceSlug);

	// caps 不落库（v4 没有那张表），seed 只写节点行 + 槽位。
	const node = seedNodeRows(dbPath, namespaceId, name, [], slots);
	const conn = await wsConnect(serverBase, node.token);

	const frames = [];
	let closed = null;
	conn.onFrame((f) => frames.push(f));
	conn.onClose((info) => {
		closed = info;
	});

	conn.send(
		nodeHello({
			name,
			token: node.token,
			namespaces: [namespaceSlug],
			capabilities: caps,
			slots,
			agentVersion: opts.agentVersion,
			localVars: opts.localVars,
		}),
	);

	// 等 welcome（最多 3s）。到点还没来就把「收到的帧 + 关闭原因」一起报出去：
	// 只有 "握手失败" 四个字的报错在 v4 里没有任何定位价值（可能是 token 错、
	// name 与 token 不匹配、或者首帧不是 hello）。
	const deadline = Date.now() + 3000;
	while (Date.now() < deadline) {
		if (frames.some((f) => f.type === "welcome")) break;
		if (closed) break;
		await new Promise((r) => setTimeout(r, 25));
	}
	if (!frames.some((f) => f.type === "welcome")) {
		conn.close();
		throw new Error(
			`节点 ${name} 握手失败（v4 hello → welcome）: frames=${JSON.stringify(frames).slice(0, 400)} close=${JSON.stringify(closed)}`,
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
