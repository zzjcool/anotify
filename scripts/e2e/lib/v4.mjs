/* v4 HTTP 资源面的共享辅助（api_contract / contract / routing / persistence /
 * capacity / orphans 六个套件共用）。
 *
 * ============================ 为什么要单独一个文件 ============================
 *
 * v3 的资源面是「扁平的 /v1/capabilities?namespaceId=…，响应是 {capabilities:[…]}」；
 * v4 把它换成了两件彼此独立的事：
 *
 *   1. **命名空间进路径**：`/v1/namespaces/{ns}/capabilities`。`{ns}` 是对外名字
 *      （slug）优先、id 兼容；集群级资源（nodes / namespaces / capability-templates）
 *      没有 ns 段。query 上再带 namespaceId 不是兼容分支，是**另一个入口**
 *      （`/v1/orphans`、`/v1/stats`、`/v1/console/events` 三个保留读模型要
 *      `?namespace=`），两者不要混。
 *   2. **资源信封**：`apiVersion` / `kind` / `metadata` / `spec` / `status` 五键
 *      （四键 + status），列表是 `{…, items: []}`。v3 的 `{capabilities:[…]}`、
 *      `{tasks:[…]}`、`{events:[…]}` 全部退役。
 *
 * 套件里手写这些字符串会出两类错：路径少一段（403/404 看起来像权限问题）、
 * 读响应还用 v3 的键（`json.capabilities` 是 undefined，断言变成「空数组也算过」）。
 * 所以路径构造与「解信封」只在这里写一次，套件只用语义化的 helper。
 *
 * 纪律：
 *   - 这里**不做**造数以外的任何判断，也不静默兜底（拿不到 items 就返回 null，
 *     由调用方断言 —— 空数组与「读错字段」必须能被区分开）。
 *   - 断言用 harness 的 check/eq，失败信息里带状态码与响应体片段，便于定位。
 */
import { createHash } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

import * as H from "./harness.mjs";
import { nodeHello, wsConnect } from "./nodes.mjs";

export const API_VERSION = "anotify/v1";

// 默认命名空间的对外名（slug）。store 在注册用户时建它（契约 A1），所以套件可以
// 直接用它拼路径 —— 但**不要**把 ns 的 store id（`ns_…`）硬编码进用例。
export const DEFAULT_NS = "default";

// ---------- 路径 ----------
//
// 每个 helper 只做「拼接 + 转义」，不做存在性判断：路径对不对要靠后端的状态码回答，
// 而不是靠本文件猜。
const seg = (v) => encodeURIComponent(String(v));

export function nsPath(ns, suffix = "") {
	return `/v1/namespaces/${seg(ns)}${suffix}`;
}

export const P = {
	namespaces: () => "/v1/namespaces",
	namespace: (ns) => nsPath(ns),
	namespaceConfig: (ns) => nsPath(ns, "/config"),
	namespaceStats: (ns) => nsPath(ns, "/stats"),
	systemConfig: () => "/v1/system/config",

	capabilities: (ns) => nsPath(ns, "/capabilities"),
	capability: (ns, name) => nsPath(ns, `/capabilities/${seg(name)}`),
	capabilityVersions: (ns, name) => nsPath(ns, `/capabilities/${seg(name)}/versions`),
	capabilityVersion: (ns, name, v) => nsPath(ns, `/capabilities/${seg(name)}/versions/${seg(v)}`),
	capabilityDeploy: (ns, name) => nsPath(ns, `/capabilities/${seg(name)}/deploy`),
	capabilityTemplates: () => "/v1/capability-templates",

	edges: (ns) => nsPath(ns, "/edges"),
	deployments: (ns) => nsPath(ns, "/deployments"),

	sources: (ns) => nsPath(ns, "/sources"),
	source: (ns, name) => nsPath(ns, `/sources/${seg(name)}`),
	sourceFire: (ns, name) => nsPath(ns, `/sources/${seg(name)}/fire`),
	sourceDryRun: (ns, name) => nsPath(ns, `/sources/${seg(name)}/dry-run`),
	sourcesDryRun: (ns) => nsPath(ns, "/sources/dry-run"),
	hook: (token) => `/v1/hooks/${seg(token)}`,

	// 集群级资源（没有 ns 段）：nodes 是 v4 仍存在的一等资源（登记/读/配置/令牌/升级）。
	nodes: () => "/v1/nodes",
	node: (name) => `/v1/nodes/${seg(name)}`,
	nodeTokens: (name) => `/v1/nodes/${seg(name)}/tokens`,
	nodeConfig: (name) => `/v1/nodes/${seg(name)}/config`,

	playbooks: (ns) => nsPath(ns, "/playbooks"),
	playbook: (ns, slug) => nsPath(ns, `/playbooks/${seg(slug)}`),

	tasks: (ns) => nsPath(ns, "/tasks"),
	task: (ns, id) => nsPath(ns, `/tasks/${seg(id)}`),
	taskCancel: (ns, id) => nsPath(ns, `/tasks/${seg(id)}/cancel`),
	taskAbandon: (ns, id) => nsPath(ns, `/tasks/${seg(id)}/abandon`),
	taskLogs: (ns, id) => nsPath(ns, `/tasks/${seg(id)}/logs`),
	taskEmissions: (ns, id) => nsPath(ns, `/tasks/${seg(id)}/emissions`),

	events: (ns) => nsPath(ns, "/events"),
	trace: (ns, traceId) => nsPath(ns, `/traces/${seg(traceId)}`),

	// 保留域读模型：**cluster 路径 + 必填 ?namespace=**（不是路径段）。
	stats: (ns) => `/v1/stats?namespace=${seg(ns)}`,
	orphans: (ns) => (ns === undefined ? "/v1/orphans" : `/v1/orphans?namespace=${seg(ns)}`),
	consoleEvents: (ns) => `/v1/console/events${ns === undefined ? "" : `?namespace=${seg(ns)}`}`,

	// v3 已退役、v4 明确不做兼容的路径（用于「它必须 404」这类负向断言）。
	// 注意 /v1/nodes 不在其中：节点是 v4 的**集群级资源**（见上面的 P.nodes）。
	legacyEvents: () => "/v1/events",
	legacyCapabilities: () => "/v1/capabilities",
};

// ---------- 写请求信封 ----------
//
// 只放**必填**的四个键：`status` 在写请求里出现即 400 status_not_writable，所以本
// 文件永不生成 status（服务端自己派生）。`spec` 由各 kind 的 builder 给。
export function envelope(kind, metadata, spec) {
	return { apiVersion: API_VERSION, kind, metadata, spec };
}

// MIN_RUN 是最小合法实现（空脚本，退出码 0）。v4 的能力必须有实现：缺 `spec.run`
// 直接 400 impl_missing（capability.md §6.1），所以「建个能力」一律带它。
export const MIN_RUN = {
	entrypoint: "run.sh",
	files: { "run.sh": "#!/bin/sh\nexit 0\n" },
};

// capabilityBody 造一份 Capability 写请求（信封形状）。
//
// `version` 省略 = 内容寻址（同内容复用同一行，新内容分配 max+1）。这是 v4 的语义，
// 用例要「再造一版」时传新**内容**而不是传递增版本号。
export function capabilityBody({
	name,
	namespace,
	version,
	displayName,
	params,
	next,
	requires,
	lifecycle,
	concurrency,
	run,
} = {}) {
	const metadata = { name };
	if (namespace !== undefined) metadata.namespace = namespace;
	if (version !== undefined) metadata.version = version;
	if (displayName !== undefined) metadata.displayName = displayName;
	const spec = {
		params: params === undefined ? { type: "object" } : params,
		next: next === undefined ? [] : next,
		run: run === undefined ? MIN_RUN : run,
	};
	// requires / lifecycle / concurrency 都是封闭 spec 的成员，缺省即空对象
	// （后端归一化时补，不传也不会 400）。只在用例关心时才显式给。
	if (requires !== undefined) spec.requires = requires;
	if (lifecycle !== undefined) spec.lifecycle = lifecycle;
	if (concurrency !== undefined) spec.concurrency = concurrency;
	return envelope("Capability", metadata, spec);
}

// sourceBody 造一份 Source 写请求（信封形状）。
//
// 写请求里的 `config` 是**可写面**（可含 token / secret）；读响应的 config 会被换成
// 只读投影（hookUrl / secretSet），两者形状不同，用例别拿写入示例当读响应。
export function sourceBody({
	name,
	namespace,
	displayName,
	kind = "manual",
	enabled,
	config = {},
	targets = [],
} = {}) {
	const metadata = { name };
	if (namespace !== undefined) metadata.namespace = namespace;
	if (displayName !== undefined) metadata.displayName = displayName;
	const spec = { kind, config, targets };
	if (enabled !== undefined) spec.enabled = enabled;
	return envelope("Source", metadata, spec);
}

// namespaceBody 造一份 Namespace 写请求：slug 是 `metadata.name`，展示名是 displayName。
export function namespaceBody(name, displayName) {
	const metadata = { name };
	if (displayName !== undefined) metadata.displayName = displayName;
	return envelope("Namespace", metadata, {});
}

// ---------- 读响应 ----------

// items 解列表信封。**不兜底成 []**：拿到 undefined 说明要么路径错了、要么服务端回了
// 一个 v3 形状的响应 —— 静默给空数组会让「列表永远是空的」这类 bug 变成假绿。
export function items(res) {
	const list = res && res.json && res.json.items;
	return Array.isArray(list) ? list : null;
}

// itemNames 取列表里的 metadata.name 序列（隔离类断言最常用）。
export function itemNames(res) {
	const list = items(res);
	if (!list) return null;
	return list.map((it) => (it && it.metadata ? it.metadata.name : undefined));
}

// findItem 在列表信封里按 metadata.name 找一项。
export function findItem(res, name) {
	const list = items(res);
	if (!list) return null;
	return list.find((it) => it && it.metadata && it.metadata.name === name) || null;
}

// reasonOf 取 Status 错误体的机器码（api-conventions §4.1 的 `reason`）。
export function reasonOf(res) {
	return res && res.json ? res.json.reason : undefined;
}

// brief 是断言失败信息里的响应摘要（状态码 + 前 200 字符）。只用于诊断，不参与判断。
export function brief(res) {
	if (!res) return "（无响应）";
	return `${res.status} ${String(res.text || "").slice(0, 200)}`;
}

// ---------- 断言（都带状态码与响应体，失败时不用再去翻日志） ----------

export function expectStatus(name, res, want) {
	H.check(name, res.status === want, `期望 ${want} 实际 ${brief(res)}`);
	return res;
}

// expectOneOf 用于「2xx 家族」这类不想钉死具体码的场合（越少用越好）。
export function expectOneOf(name, res, wants) {
	H.check(name, wants.includes(res.status), `期望 ${wants.join("/")} 实际 ${brief(res)}`);
	return res;
}

// expectReason 断言 Status 错误体（HTTP 码 + 机器码两件都判：只判码会让「换了个错误
// 原因但状态码碰巧一样」的回归漏过去）。
export function expectReason(name, res, wantCode, wantReason) {
	H.eq(`${name} → ${wantCode}`, res.status, wantCode);
	if (wantReason !== undefined) {
		H.eq(`${name} → reason=${wantReason}`, reasonOf(res), wantReason);
	}
	return res;
}

// assertEnvelope 校验一个资源文档的四键 + kind + apiVersion + status 非 null。
//
// 这是 v4 最基础的一条纪律（api-conventions §3.1 E1）；每个读端点都该过一遍，
// 所以做成 helper 而不是每个套件手写。
export function assertEnvelope(name, doc, wantKind) {
	const keys = ["apiVersion", "kind", "metadata", "spec", "status"];
	const missing = keys.filter((k) => !(doc && Object.prototype.hasOwnProperty.call(doc, k)));
	H.check(`${name} 五键齐全（缺 ${missing.join(",") || "无"}）`, missing.length === 0, JSON.stringify(doc && Object.keys(doc)));
	H.eq(`${name} apiVersion`, doc && doc.apiVersion, API_VERSION);
	H.eq(`${name} kind`, doc && doc.kind, wantKind);
	H.check(`${name} status 不是 null（无事实给 {}）`, doc && doc.status !== null && typeof doc.status === "object", JSON.stringify(doc && doc.status));
	H.check(`${name} spec 不是 null`, doc && doc.spec !== null && typeof doc.spec === "object", JSON.stringify(doc && doc.spec));
	return doc;
}

// assertListEnvelope 校验列表信封：四键 + kind 以 List 结尾 + items 是数组。
export function assertListEnvelope(name, res, wantKind) {
	const doc = res && res.json;
	assertEnvelopeWithoutSpec(name, doc, wantKind);
	const list = items(res);
	H.check(`${name} items 是数组（空列表也要 []）`, Array.isArray(list), JSON.stringify(doc && Object.keys(doc || {})));
	return list || [];
}

// 列表信封没有 spec（apiList 是 apiVersion/kind/metadata/items）。
function assertEnvelopeWithoutSpec(name, doc, wantKind) {
	H.check(`${name} apiVersion`, doc && doc.apiVersion, API_VERSION);
	H.eq(`${name} kind`, doc && doc.kind, wantKind);
	H.check(`${name} metadata 是对象`, doc && doc.metadata && typeof doc.metadata === "object", JSON.stringify(doc && doc.metadata));
}

// isResourceVersion 判断 CAS 令牌形状（`rv1:<uid>:<n>`）。
//
// 不解析数值、不做运算（契约 A9：令牌是字符串，客户端不做算术），只确认「有令牌」
// 与「新旧可区分」。
export function isResourceVersion(v) {
	return typeof v === "string" && /^rv1:/.test(v);
}

// ---------- 造数（走真实 HTTP） ----------

// createCapability 建能力并返回响应（调用方自己断言状态码）。
//
// 不在这里 throw：造数失败要看得到**后端**的回答（400 的 reason 比一句「建能力失败」
// 有用得多），所以状态码由用例断言，这里只负责拼路径。
export function createCapability(base, session, ns, doc, opts = {}) {
	return H.req(base, P.capabilities(ns), { session, method: "POST", body: doc, ...opts });
}

// createSource 建源（同上：状态码由调用方断言）。
export function createSource(base, session, ns, doc, opts = {}) {
	return H.req(base, P.sources(ns), { session, method: "POST", body: doc, ...opts });
}

// createNamespace 建命名空间，返回 {res, slug}。slug 由调用方给（要能进 URL）。
export function createNamespace(base, session, slug, displayName) {
	return H.req(base, P.namespaces(), {
		session,
		method: "POST",
		body: namespaceBody(slug, displayName),
	});
}

// deployCapability 触发人工部署（动作 POST，不套 CAS）。
export function deployCapability(base, session, ns, name, body = {}) {
	return H.req(base, P.capabilityDeploy(ns, name), { session, method: "POST", body });
}

// ingestEvent 走 Bearer API Key 投递事件（这是**机器入口**，不是会话）。
export function ingestEvent(base, sendKey, ns, body) {
	return H.req(base, P.events(ns), { key: sendKey, method: "POST", body });
}

// fireSource 手动触发一个源（会话 + ns 路径）。
export function fireSource(base, session, ns, name, body = {}) {
	return H.req(base, P.sourceFire(ns, name), { session, method: "POST", body });
}

// ---------- 轮询 ----------

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// poll 轮询到 fn 返回真值；到点抛出带 last 观察值的错误。
//
// 不返回 null 让调用方自己 if：套件里有十几处等待，漏一处 `if (!v) throw` 就会变成
// 「断言静默跳过」——那是最难发现的一种假绿。
export async function poll(what, fn, { timeoutMs = 8000, intervalMs = 100 } = {}) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	for (;;) {
		last = await fn();
		if (last) return last;
		if (Date.now() > deadline) {
			throw new Error(`等待超时（${timeoutMs}ms）：${what}\n最后一次观察: ${JSON.stringify(last)}`);
		}
		await sleep(intervalMs);
	}
}

// tryPoll 是 poll 的「返回布尔」版本：用于「希望它变成 X，但不变也不该 throw」的场合。
export async function tryPoll(fn, { timeoutMs = 8000, intervalMs = 100 } = {}) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		if (await fn()) return true;
		if (Date.now() > deadline) return false;
		await sleep(intervalMs);
	}
}

// ---------- 直接读库（跨进程核对落盘事实） ----------
//
// 为什么要能直接读库：有些事实**故意**没有 HTTP 写入口（任务只由 ingress/派发器建行、
// 角色的 status 是派生而非落库），要造出「等了 600 秒的 pending」这种状态只能直插。
// 读的时候也一样：API 是派生投影，落盘事实才是「跨重启还在不在」的判据。
export function withDB(dbPath, fn) {
	const db = new DatabaseSync(dbPath);
	try {
		return fn(db);
	} finally {
		db.close();
	}
}

// countRows 数一张表（只接受字面表名，杜绝把用户输入拼进 SQL）。
const COUNTABLE = new Set(["tasks", "events", "event_deliveries", "capability_versions", "sources", "deployments", "nodes"]);
export function countRows(dbPath, table, where = "", args = []) {
	if (!COUNTABLE.has(table)) throw new Error(`countRows: 不支持的表 ${table}`);
	return withDB(dbPath, (db) =>
		db.prepare(`SELECT count(*) AS n FROM ${table} ${where}`).get(...args).n,
	);
}

// readTaskRow 读一条任务的关键列（跨进程核对「还在被推进」「没被删」）。
//
// assignment_id / assignment_expires_at 也在里面：v4 的派发**只记候选**（phase 仍是
// pending、没有租约），所以「派了没」的唯一证据是 assignment 三元组，不是 phase。
export function readTaskRow(dbPath, id) {
	return withDB(dbPath, (db) =>
		db
			.prepare(
				`SELECT id, phase, attempt, node_id, node_name, capability, version, digest,
				        assignment_id, assignment_expires_at, created_at, finished_at, error,
				        summary, cancel_reason
				   FROM tasks WHERE id = ?`,
			)
			.get(id),
	);
}

// seedPendingTask 直插一条 pending 任务并把 created_at 拨到过去。
//
// 为什么直插而不是「造出真实的派不出去」：真实路径需要「节点声明了能力但槽位占满」，
// 那是时序测试（flaky）。要验的是判定与响应，直接把库摆成目标状态最确定。
//
// v4 的 tasks 表变了不少：`status` → `phase`，`cap_hash`/`graph_id`/`path` 都没了，
// digest 非空是「已解析任务」的 CHECK 约束，所以这里必须给真 digest。
export function seedPendingTask(dbPath, { namespaceId, capabilityId, capability, version = 1, digest, ageSec = 0, traceId }) {
	return withDB(dbPath, (db) => {
		const nowSec = Math.floor(Date.now() / 1000);
		const id = `tsk_e2e${Math.random().toString(36).slice(2, 10)}`;
		const trace = traceId || `trace_${id}`;
		db.prepare(
			`INSERT INTO tasks (id, namespace_id, trace_id, capability_id, capability, version, digest,
			                    params, phase, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '{}', 'pending', ?)`,
		).run(id, namespaceId, trace, capabilityId, capability, version, digest || "sha256:" + "0".repeat(64), nowSec - ageSec);
		return id;
	});
}

// seedEventRow 直插一条事件行（保留期清理的判据，不走 ingress 就没有派发副作用）。
export function seedEventRow(dbPath, { namespaceId, source = "e2e", type = "e2e.seed", createdAt }) {
	return withDB(dbPath, (db) => {
		const id = `evt_e2e${Math.random().toString(36).slice(2, 12)}`;
		const trace = `trace_${id}`;
		db.prepare(
			`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, reason, receipt, created_at)
			 VALUES (?, ?, ?, ?, ?, '{}', 'Ingested', '{}', ?)`,
		).run(id, namespaceId, trace, source, type, createdAt);
		return { id, trace };
	});
}

// eventExists 判断一条事件行是否还在（保留期清理的正向证据）。
export function eventExists(dbPath, id) {
	return withDB(dbPath, (db) => !!db.prepare(`SELECT 1 AS x FROM events WHERE id = ?`).get(id));
}

// markTaskDone 把一条 pending 任务直接摆成终态（背压自愈用例要「清掉积压」）。
//
// 为什么不走 POST …/tasks/{id}/abandon：那条动作是**处置孤儿**的产品入口，会写
// cancel/abandon 原因；背压用例要的是「积压凭空消失」这个纯状态变化（模拟积压被
// 消费掉），落在 phase/finished_at 两列上即可。
export function markTaskDone(dbPath, id, phase = "succeeded") {
	return withDB(dbPath, (db) =>
		db
			.prepare(`UPDATE tasks SET phase = ?, finished_at = ? WHERE id = ?`)
			.run(phase, Math.floor(Date.now() / 1000), id).changes,
	);
}

// ============================================================================
// v4 节点面：把一台 mock 节点做到「**真的能被派发**」（capacity / orphans 共用）
// ============================================================================
//
// 为什么必须有这一层：v4 的「节点能跑某任务」不是「节点申报了能力名」这一条可比，
// 派发（以及孤儿视图）用的是同一份 store 判定 `EligibleNodesForTask`，它读三类**真实
// 事实**：
//
//	① nodes.status = online + declared_namespaces（hello 申报 ∩ node_configs 授权）
//	② node_configs.applied_revision == revision（节点回报 config.applied 之后）
//	③ deployments.state = applied（节点回报 capability.deployed 之后）
//
// 这三件事在 v4 里**都有产品入口**（登记 / 签令牌 / WS 帧 / 部署动作），所以这里一律
// 走入口，绝不直接插 nodes / node_configs / deployments：自己写那三张表等于伪造
// 「节点自己报的事实」，断言就变成在测夹具，而这正是 v3 时代 node_capabilities 直接
// 插库留下的教训（v4 已把那张表删了）。

// machineConfigHash 是 protocol.MachineConfigHash 的同一份算法（hash.go）。
//
// config.applied 的 hash 中枢只**记录**不校验（applied_revision 才是派发判据），但造数
// 仍按节点侧真实算法算：错误的 hash 会让「配置漂移」在 Node.status 上凭空出现。
export function machineConfigHash(vars = {}, limits = {}) {
	const total = limits.total || 1; // protocol.DefaultLimitsTotal
	const perCapability = limits.perCapability || {};
	const canonical = JSON.stringify({ vars, limits: { total, perCapability } });
	return "sha256:" + createHash("sha256").update(canonical).digest("hex");
}

// bringUpEligibleNode 走真实产品路径把 mock 节点变成「在线 + 配置已 apply」：
//
//	POST /v1/nodes（登记 + 授权 ns）→ POST /v1/nodes/{name}/tokens →
//	WS hello → welcome → config.applied（applied_revision 落库）
//
// 部署事实（③）由 deployApplied 单独做——它依赖一个已存在的能力，调用节奏也不同。
//
// 返回的 waitFrame/close 直接转发到那条 WS 会话上（套件要观察 task.dispatch 时用）。
export async function bringUpEligibleNode(base, session, { name, namespace = DEFAULT_NS, slots = 1 } = {}) {
	if (!name) throw new Error("bringUpEligibleNode: name 必填");
	const created = await H.req(base, P.nodes(), {
		session,
		body: envelope("Node", { name }, { namespaces: [namespace] }),
	});
	expectStatus(`登记节点 ${name}`, created, 201);
	const nodeId = created.json?.metadata?.uid;
	H.check(`登记节点 ${name} 拿到 uid`, typeof nodeId === "string" && nodeId.length > 0, brief(created));

	const tok = await H.req(base, P.nodeTokens(name), { session, body: {} });
	expectStatus(`签发 ${name} 的令牌`, tok, 200);
	const token = tok.json?.token || "";
	H.check(`${name} 的令牌明文只在签发响应里`, token.length >= 20, `len=${token.length}`);

	const conn = await wsConnect(base, token);
	const frames = [];
	let closed = null;
	conn.onFrame((f) => frames.push(f));
	conn.onClose((info) => {
		closed = info;
	});
	conn.send(nodeHello({ name, token, namespaces: [namespace], slots }));
	const welcome = await poll(`节点 ${name} 的 welcome`, () => frames.find((f) => f.type === "welcome"), { timeoutMs: 5000 });

	// 节点回报「机器级配置已生效」：applied_revision = welcome.configRevision。
	// instances 一起报（槽位事实的 presence 语义：缺字段 = 未知，不能当空闲）。
	conn.send({
		type: "config.applied",
		revision: welcome.configRevision,
		hash: machineConfigHash(welcome.nodeVars || {}, welcome.nodeLimits || {}),
		instances: Array.from({ length: slots }, (_, slot) => ({ slot, busy: false })),
	});

	return {
		name,
		id: nodeId,
		token,
		conn,
		frames,
		configRevision: welcome.configRevision,
		closed: () => closed,
		waitFrame(type, pred = () => true, timeoutMs = 6000) {
			return poll(`收到 ${type} 帧`, () => frames.find((f) => f.type === type && pred(f)), { timeoutMs });
		},
		close() {
			conn.close();
		},
	};
}

// deployApplied 让节点**真的持有**某个能力的正文：部署动作 → capability.deploy 帧 →
// capability.deployed(applied) 回执 → 轮询 deployments 读模型收敛。
//
// 不做「直接 UPDATE deployments SET state='applied'」：那会绕过节点回执这一层，而
// 「pending 不算候选」正是 v4 明确裁定过的语义（G1-4），伪造它会把断言变成自证。
export async function deployApplied(base, session, node, cap, namespace = DEFAULT_NS) {
	const dep = await deployCapability(base, session, namespace, cap.name, { nodes: [node.name] });
	expectStatus(`部署 ${cap.name} → ${node.name}`, dep, 200);
	const frame = await node.waitFrame("capability.deploy", (f) => f.digest === cap.digest);
	node.conn.send({
		type: "capability.deployed",
		requestId: frame.requestId,
		namespace,
		name: cap.name,
		version: cap.version,
		digest: cap.digest,
		status: "applied",
	});
	await poll(`部署行 ${cap.name} → applied`, async () => {
		const r = await H.req(base, P.deployments(namespace), { session });
		// deployments 是**保留的读模型**（`{deployments:[…]}`），不是列表信封 —— 别拿 items() 解。
		const rows = Array.isArray(r.json?.deployments) ? r.json.deployments : [];
		return rows.some((d) => d.capability === cap.name && d.state === "applied" && d.digest === cap.digest);
	}, { timeoutMs: 8000 });
}

// mkCapability 建一个能力并返回造数需要的三件事实（uid / name / version / digest）。
//
// 失败**抛错**而不是返回 null：造数失败时后面的断言会以「undefined.id」崩溃，
// 那种报错看不出是哪一步坏的（capacity 套件在 v3 就是这么挂的）。
export async function mkCapability(base, session, name, { namespace = DEFAULT_NS, body } = {}) {
	const r = await createCapability(base, session, namespace, body || capabilityBody({ name }));
	expectStatus(`建能力 ${name}`, r, 201);
	const doc = r.json || {};
	const out = {
		id: doc.metadata?.uid,
		name: doc.metadata?.name || name,
		version: doc.metadata?.version,
		digest: doc.status?.digest,
	};
	if (!out.id || !Number.isInteger(out.version) || !/^sha256:[0-9a-f]{64}$/.test(String(out.digest))) {
		throw new Error(`建能力 ${name} 的响应缺少 uid/version/digest: ${brief(r)}`);
	}
	return out;
}

// mkSource 建一个已启用的源（v4 ingress 必须有真实源行：源名决定路由模板）。
export async function mkSource(base, session, name, { namespace = DEFAULT_NS, kind = "manual", targets = [], config = {} } = {}) {
	const r = await createSource(base, session, namespace, sourceBody({ name, kind, targets, config, enabled: true }));
	expectStatus(`建源 ${name}`, r, 201);
	return r.json || {};
}
