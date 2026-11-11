#!/usr/bin/env node
/* SUITE: contract — v4 冻结契约（decisions V4-D1…D6）的运行时断言
 *
 * ======================= 这一版为什么整篇重写 =======================
 *
 * v3 的 contract 套件验的是 v3 CONTRACT.md §9 的 A1–A12，其中一半的不变量在 v4 里
 * **载体变了**（边表 → 资源 spec、port → 单一 emit 通道、handler 能力 → 不存在、
 * `tasks.status` 列 → `phase`、`outputs` → `spec.next`）。v4 的权威是
 * `docs/design/v4/decisions.md`（V4-D1…D6）与四份主文档。
 *
 * 所以本套件改成**逐条钉 v4 决策**，并保留 v3 十二断言的编号做对照表 —— 每一条
 * v3 断言要么迁到 v4 的等价物，要么明确写出「为什么它在 v4 里不存在了」。没有静默删除：
 *
 *   v3 A1  注册即得 default 命名空间
 *     → v4 A1：注册（devseed 同一条 store 规则）建出**唯一** default ns，且 v4 **不播种
 *       任何内置能力**（store/identity.go:57 明确写了「v4 不再播种内置能力」，
 *       所以 v3 的「notify.phone 已播种」是设计退役，不是回归）。顺带守卫表结构：
 *       capability_versions 已经没有 v3 的 `source` / `content_hash` 列。
 *   v3 A2  无 argv / 无 shell
 *     → 静态断言（scripts/check_contract.py，`make ci`）；本套件补一条运行时断言：
 *       派发帧里不出现 argv/shell/command（见 A6 段）。
 *   v3 A3  能力必须先定义
 *     → v4：能力 `spec.next` 的引用是**延迟解析**（DownstreamResolved=False/NotFound，
 *       不阻止创建）；「未定义能力不能当出口目标」的硬边界落到**源**上（建源被拒）。
 *       两条都钉（FINDING：源侧拒绝码当前是 500，见 routing.mjs 文件头，这里不固化 5xx）。
 *   v3 A5  出口封闭（from_port ∈ outputs）
 *     → v4 没有 port：闭合性 = `spec.next` 只接受合法引用 + PATCH 只接受 `next`
 *       （其它 spec 键 400 immutable_field）+ 内容不可变（同版本换内容 400）。
 *   v3 A5b 能力+下游边一次提交（links）
 *     → v4：`next` 就在**同一份 spec** 里（天然一次提交）；坏引用 → 400 且 0 行 0 边。
 *   v3 A6  扇出（一个出口多条边）
 *     → v4：源的一行 targets 多条 → 多个任务（routing 覆盖）；能力侧的下游是**显式**
 *       emit（一次一个 `to`）。本套件钉：emit 到 next 外 → 400 target_not_declared；
 *       emit 带 `id` 幂等（D4）；不带 id 每次都派发；同 id 换 to → 409 emit_id_conflict。
 *   v3 A7  成环
 *     → v4：环合法，且没有 hop 预算/`path` 列（那些是 v3 router 的遗留）。钉法：a→b→a
 *       第二圈真的建出新任务（emit 是显式的，环不会自己转）。
 *   v3 A8  out 退役
 *     → v4：Task 信封里**没有任何 out 键**（顶层/spec/status 都查），且不 emit 就没有下游。
 *   v3 A9  租约接管（掉线回收重派）
 *     → v4 没有「租约过期巡检」：恢复语义是「节点离线 → 新任务留 pending」（node_v4 §11）
 *       + 显式 cancel/abandon。这里迁成 D4 的**不自动重试**：非幂等任务失败后
 *       任务行只有一行、attempt 不增长（默认 `idempotent:false`，V4-D4 主动改默认）。
 *   v3 A10 命名空间隔离 → v4（ns 进路径）：同名不同定义、跨 ns 读 404 同体、trace 不串、
 *       未知 ns 404。
 *   v3 A11 版本并存 → v4：同内容 fold（同 uid 同 version）、新内容 version+1、
 *       同版本换内容 400 version_content_immutable。
 *   v3 A12 空列表 [] → v4 列表信封 `items: []`（不是 null，也不是缺键）。
 *
 * 另外补上 v4 新增、v3 没有的运行时契约：
 *   D1 信封与 CAS（status 只读 / kind / apiVersion / namespace_mismatch / resourceVersion
 *      缺失 400、过期 409 + details.current / DELETE 走 ?resourceVersion= / name 不可变）
 *   D2 三层 vars：`configRevision`（机器）与 `namespaceVarsRevision`（ns）是两个字段，
 *      派发帧只带**被引用的** ns 默认值
 *   D6 保留期默认值：事件/任务 30 天、结果 7 天（显式 0 = 永久）
 *
 * 做法：能力/源/配置/任务全走真实 HTTP；节点用共享 MockNode（v4 帧协议镜像，
 * accept → lease.grant → emit → result）。**真执行器**（脚本、产物、取消）在 node_v4。
 */
import { spawnSync } from "node:child_process";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";
import * as N from "../lib/nodes.mjs";

let server, session, sendKey, nsId, NS, node = null;
const results = [];

// A(name, cond, detail) 记录一条契约断言（带 v4 决策编号，便于对照 decisions.md）。
function A(id, name, cond, detail) {
	results.push({ id, ok: !!cond });
	H.check(`[${id}] ${name}`, cond, detail);
}

// AEq 是比较版的 A：actual/want 逐字（对象/数组按 JSON 比较）。
//
// 为什么单独一个：`A(..., actual === want, ...)` 一行行写最容易写反或漏等号，而
// 「把期望值当 detail 传进去」看起来像断言、实际恒为真 —— 这是 e2e 里最典型的假绿。
function AEq(id, name, actual, want) {
	const norm = (v) => (v && typeof v === "object" ? JSON.stringify(v) : String(v));
	const ok = norm(actual) === norm(want);
	results.push({ id, ok });
	H.check(`[${id}] ${name}`, ok, `want=${norm(want)} got=${norm(actual)}`);
}

// defineCap 造能力（失败直接抛：造数失败看后端 body，比一句「建能力失败」有用）。
async function defineCap(name, opts = {}) {
	const r = await V.createCapability(server.base, session, NS, V.capabilityBody({ name, ...opts }));
	if (r.status !== 201) throw new Error(`定义能力 ${name} 失败: ${r.status} ${String(r.text).slice(0, 300)}`);
	return r.json;
}

// deployToNode 走真实人工部署（POST …/deploy）→ 节点回执 applied → 行收敛。
// v4 的候选判定要求 `state=applied`，pending 不算候选（store/node.go 的注释）。
async function deployToNode(capName) {
	const res = await H.req(server.base, V.P.capabilityDeploy(NS, capName), {
		session,
		method: "POST",
		body: { nodes: [node.id] },
	});
	H.eq(`部署 ${capName} → 200`, res.status, 200);
	const frame = await node.deployFrame(capName);
	H.check(`节点收到 ${capName} 的 capability.deploy 帧`, !!frame, JSON.stringify(node.frames.map((f) => f.type)));
	const applied = await V.tryPoll(
		async () => {
			const rows = (await H.req(server.base, V.P.deployments(NS), { session })).json?.deployments || [];
			const row = rows.find((d) => d.capability === capName && d.nodeId === node.id);
			return row && row.state === "applied" ? row : null;
		},
		{ timeoutMs: 5000 },
	);
	H.check(`deployment ${capName} 收敛成 applied`, !!applied, JSON.stringify(frame).slice(0, 200));
	return applied;
}

// postEvent 走机器入口（Bearer key + ns 路径）。
function postEvent(body) {
	return H.req(server.base, V.P.events(NS), { key: sendKey, body });
}

async function traceOf(traceId) {
	const r = await H.req(server.base, V.P.trace(NS, traceId), { session });
	if (r.status !== 200) throw new Error(`GET trace ${traceId}: ${r.status} ${r.text}`);
	return r.json;
}

// startTask 投递一条事件并要求恰好建出 1 个任务，等它被派发到 mock 节点。
async function startTask(label) {
	const ev = await postEvent({ source: "c.origin", type: "go", payload: {} });
	AEq("A6", `${label}：事件建出 1 个任务`, (ev.json?.tasks || []).length, 1);
	const taskId = ev.json?.tasks?.[0]?.taskId;
	const dispatch = await node.waitDispatch(taskId, 8000);
	H.check(`[A6] ${label}：任务被派发到节点（task.dispatch）`, !!dispatch, JSON.stringify(node.frames.map((f) => f.type)));
	if (!dispatch) throw new Error(`${label}: 没收到 task.dispatch`);
	return { ev, traceId: ev.json.traceId, taskId, dispatch };
}

// runTask 按 v4 执行子协议跑完一条任务：accept → 等 lease.grant →（可选）若干 emit → result。
//
// 顺序是契约的一部分：**emit 必须发生在 result 之前**（result 之后 attempt 不再是
// running，emit 会被判 task_not_running）。所以 emits 在这里统一发出，再落终态。
async function runTask(dispatch, { emits = [], phase = "succeeded", summary = "mock 完成", error } = {}) {
	node.accept(dispatch, 0);
	const grant = await node.waitGrant(dispatch.taskId, 8000);
	H.check(`[A6] 任务 ${dispatch.taskId} 拿到 lease.grant（执行权只来自这里）`, !!grant, JSON.stringify(node.frames.map((f) => f.type)));
	if (!grant) throw new Error(`没收到 ${dispatch.taskId} 的 lease.grant`);
	const acks = [];
	for (const e of emits) acks.push(await node.emit(grant, e));
	node.result(grant, { phase, summary, error });
	return { grant, acks };
}

// waitPhase 轮询任务相位（不 sleep 猜时序），返回**任务文档**（不是 boolean）。
//
// 为什么不用 V.tryPoll：它是「返回 bool」的版本，套件要拿文档里的 attempt/result 一起断言，
// 拿到 true 之后再 GET 一次是另一种竞态（中间可能又变了）。
async function waitPhase(taskId, want, timeoutMs = 8000) {
	try {
		return await V.poll(
			`任务 ${taskId} 进入 ${want}`,
			async () => {
				const r = await H.req(server.base, V.P.task(NS, taskId), { session });
				return r.json?.status?.phase === want ? r.json : null;
			},
			{ timeoutMs },
		);
	} catch {
		return null;
	}
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: contract（v4 冻结契约 V4-D1…D6 的运行时断言）===");
	server = await H.startServer({ suiteName: "contract" });
	const seeded = H.seed(server.dbPath, "contract");
	session = seeded.session;
	sendKey = seeded.sendKey;

	const ns = await H.defaultNamespace(server.base, session);
	H.check("拿到默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
	if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
	NS = ns.slug;
	nsId = ns.id;

	// devseed 的 stdout 只有 KEY=VALUE，log.Printf 走 stderr —— 两个流都要读（沿用 v3 口径，
	// 因为 A1 要证明「devseed 与真实注册走同一条 store 规则」，而不是另建一套夹具）。
	const seedRun = spawnSync(H.DEVSEED_BIN, ["-db", server.dbPath, "-username", "contract"], {
		encoding: "utf8",
	});
	const seedOut = (seedRun.stdout || "") + (seedRun.stderr || "");
	const uid = (seedOut.match(/^UID=(.*)$/m) || [])[1];
	H.check("devseed 输出 UID（v4 仍输出）", !!uid, seedOut.slice(0, 200));

	// ================= A1 注册即得 default 命名空间（同事务） =================
	// v4 的注册只保证「用户 + 一个 default ns」原子完成（store/identity.go registerUser）。
	// **不播种内置能力**：v4 没有中枢内置执行器，链尾就是 `next: []` 的普通脚本能力。
	{
		const nss = V.withDB(server.dbPath, (db) =>
			db.prepare(`SELECT id, slug FROM namespaces ORDER BY rowid`).all(),
		);
		A(
			"A1",
			"注册即得 default 命名空间（唯一一行）",
			nss.length === 1 && nss[0].slug === "default" && nss[0].id === nsId,
			JSON.stringify({ nss, nsId }),
		);
		// v4 表结构：capability_versions 没有 v3 的 `source` 列（v3 用它区分 builtin）。
		// 这条断言同时是「别再拿 v3 列写断言」的守卫。
		const cols = V.withDB(server.dbPath, (db) =>
			db.prepare(`PRAGMA table_info(capability_versions)`).all().map((c) => c.name),
		);
		A(
			"A1",
			"capability_versions 没有 v3 的 source / content_hash 列（v4 baseline 重建）",
			!cols.includes("source") && !cols.includes("content_hash") && cols.includes("digest"),
			JSON.stringify(cols),
		);
		const capCount = V.withDB(server.dbPath, (db) =>
			db.prepare(`SELECT count(*) AS n FROM capability_versions`).get().n,
		);
		A(
			"A1",
			"v4 不播种任何内置能力（设计退役，不是回归）",
			capCount === 0,
			`capability_versions 行数 = ${capCount}`,
		);
		const nsCap = V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session }));
		A("A1", "默认命名空间的能力列表是空的 items: []", Array.isArray(nsCap) && nsCap.length === 0, JSON.stringify(nsCap));
	}

	// ================= A3 未定义能力：不能当出口目标 / 引用只解析到 NotFound =================
	{
		// ① 源把事件指向不存在的能力 → 建源就被拒（客户端可修正，不是运行期静默丢事件）。
		//    FINDING：当前回 500（见 routing.mjs 文件头），所以这里只钉「被拒 + 点名」。
		const broken = await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({ name: "c.broken", kind: "manual", targets: [{ capability: "c.ghost" }] }),
		);
		A("A3", "源 targets 引用不存在的能力 → 建源被拒", broken.status >= 400, `${broken.status} ${String(broken.text).slice(0, 160)}`);
		A(
			"A3",
			"拒绝消息点名那个能力（不是一句 500）",
			String(broken.text || "").includes("c.ghost"),
			String(broken.text).slice(0, 160),
		);
		const srcList = V.itemNames(await H.req(server.base, V.P.sources(NS), { session }));
		A("A3", "被拒的源一行都没建", !(srcList || []).includes("c.broken"), JSON.stringify(srcList));

		// ② 能力 next 引用不存在的能力：**允许创建**（延迟解析），但状态必须说「解析不到」。
		//    这是 v4 与 v3 的语义差异：v3 建边时硬拒；v4 的 next 是声明，解析是读时派生。
		const dangling = await defineCap("c.dangling", { next: ["c.ghost"] });
		const cond = (dangling.status?.conditions || []).find((c) => c?.type === "DownstreamResolved");
		A("A3", "next 引用不存在 → DownstreamResolved=False", cond?.status === "False", JSON.stringify(cond));
		AEq("A3", "原因码是 NotFound（封闭集合）", cond?.reason, "NotFound");
		A(
			"A3",
			"resolved.next 把引用标成 missing（没有悄悄连过去）",
			dangling.status?.resolved?.next?.[0]?.missing === true,
			JSON.stringify(dangling.status?.resolved?.next || []),
		);
	}

	// ================= A11 版本 / 内容寻址（capability.md §3） =================
	{
		const v1 = await defineCap("c.versioned");
		await V.expectReason(
			"A11 同版本换内容 → 400 version_content_immutable",
			await V.createCapability(server.base, session, NS, {
				apiVersion: V.API_VERSION,
				kind: "Capability",
				metadata: { name: "c.versioned", version: 1 },
				spec: { run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho different\n" } } },
			}),
			400,
			"version_content_immutable",
		);
		// 同内容（省略 version）= 内容寻址 → 复用同一行（同 uid、同 version、同 digest）
		const fold = await defineCap("c.versioned");
		A("A11", "同内容复用同一行（fold：同 uid）", fold.metadata?.uid === v1.metadata?.uid, `${fold.metadata?.uid} vs ${v1.metadata?.uid}`);
		A("A11", "同内容不产生新版本", fold.metadata?.version === v1.metadata?.version, `${fold.metadata?.version} vs ${v1.metadata?.version}`);
		A("A11", "同内容 digest 相同", fold.status?.digest === v1.status?.digest, `${fold.status?.digest} vs ${v1.status?.digest}`);
		// 新内容 → 版本 +1、uid 新、digest 不同
		const v2 = await defineCap("c.versioned", {
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\necho v2\n" } },
		});
		AEq("A11", "新内容 → version=2", v2.metadata?.version, 2);
		A("A11", "新内容 → 新 uid（版本 = 独立的行）", v2.metadata?.uid !== v1.metadata?.uid, `${v2.metadata?.uid} vs ${v1.metadata?.uid}`);
		A("A11", "新内容 → digest 不同", v2.status?.digest !== v1.status?.digest, `${v2.status?.digest} vs ${v1.status?.digest}`);
		// 裸 name 读 = 最新版；versions 子资源 = 版本族（降序）
		const latest = (await H.req(server.base, V.P.capability(NS, "c.versioned"), { session })).json;
		AEq("A11", "GET 裸 name 回最大版本", latest?.metadata?.version, 2);
		const versions = V.assertListEnvelope(
			"A11 版本族",
			await H.req(server.base, V.P.capabilityVersions(NS, "c.versioned"), { session }),
			"CapabilityList",
		);
		AEq("A11", "版本族有 2 行", versions.length, 2);
		AEq("A11", "版本族按 version 降序", versions.map((v) => v.metadata?.version), [2, 1]);
	}

	// ================= A5 / D1 写面：信封纪律 + CAS + 只接受 next =================
	{
		const cap = await defineCap("c.cas", { next: [] });
		const path = V.P.capability(NS, "c.cas");
		const base = { apiVersion: V.API_VERSION, kind: "Capability" };

		// status 只读（D1：写请求带 status → 400；空对象 {} 视为「没写」）
		V.expectReason(
			"A5 写请求带非空 status → 400 status_not_writable",
			await H.req(server.base, path, {
				session,
				method: "PATCH",
				body: { ...base, metadata: { resourceVersion: cap.metadata?.resourceVersion }, spec: { next: [] }, status: { phase: "x" } },
			}),
			400,
			"status_not_writable",
		);
		// kind / apiVersion / 未知顶层键 / namespace 三条形状错误各有机器码
		V.expectReason(
			"A5 kind 不符 → 400 kind_mismatch",
			await H.req(server.base, V.P.capabilities(NS), { session, method: "POST", body: { apiVersion: V.API_VERSION, kind: "Source", metadata: { name: "c.badkind" }, spec: { run: V.MIN_RUN } } }),
			400,
			"kind_mismatch",
		);
		const badApiVersion = await H.req(server.base, V.P.capabilities(NS), {
			session,
			method: "POST",
			body: { apiVersion: "v9", kind: "Capability", metadata: { name: "c.badapi" }, spec: { run: V.MIN_RUN } },
		});
		AEq("A5", "apiVersion 不受支持 → 400", badApiVersion.status, 400);
		A("A5", "message 点名 apiVersion", /apiVersion/i.test(String(badApiVersion.json?.message || "")), String(badApiVersion.json?.message || "").slice(0, 120));
		V.expectReason(
			"A5 未知顶层字段 → 400 unknown_field（严格解码）",
			await H.req(server.base, V.P.capabilities(NS), {
				session,
				method: "POST",
				body: { apiVersion: V.API_VERSION, kind: "Capability", metadata: { name: "c.unknown" }, spec: { run: V.MIN_RUN }, bogus: 1 },
			}),
			400,
			"unknown_field",
		);
		V.expectReason(
			"A5 metadata.namespace 与路径不符 → 400 namespace_mismatch",
			await H.req(server.base, V.P.capabilities(NS), {
				session,
				method: "POST",
				body: { apiVersion: V.API_VERSION, kind: "Capability", metadata: { name: "c.nsmismatch", namespace: "other" }, spec: { run: V.MIN_RUN } },
			}),
			400,
			"namespace_mismatch",
		);
		// 创建不套 CAS：POST 带 resourceVersion 是协议误解，必须 400（不是静默丢）
		const postRv = await H.req(server.base, V.P.capabilities(NS), {
			session,
			method: "POST",
			body: { apiVersion: V.API_VERSION, kind: "Capability", metadata: { name: "c.postrv", resourceVersion: "rv1:x:1" }, spec: { run: V.MIN_RUN } },
		});
		AEq("A5", "POST 带 metadata.resourceVersion → 400", postRv.status, 400);
		A("A5", "拒绝理由点名 resourceVersion", /resourceVersion/i.test(String(postRv.text || "")), String(postRv.text).slice(0, 140));

		// PATCH：缺令牌 400；令牌过期 409 + details.current（客户端据它能立刻重试）
		V.expectReason(
			"A5 PATCH 缺 resourceVersion → 400 resource_version_required",
			await H.req(server.base, path, { session, method: "PATCH", body: { ...base, metadata: {}, spec: { next: ["c.ghost"] } } }),
			400,
			"resource_version_required",
		);
		const stale = await H.req(server.base, path, {
			session,
			method: "PATCH",
			body: { ...base, metadata: { resourceVersion: "rv1:cap_stale:1" }, spec: { next: ["c.ghost"] } },
		});
		V.expectReason("A5 PATCH 令牌过期 → 409 resource_version_conflict", stale, 409, "resource_version_conflict");
		A(
			"A5 409 带 details.current.resourceVersion（不用猜）",
			V.isResourceVersion(stale.json?.details?.current?.resourceVersion),
			JSON.stringify(stale.json?.details || {}),
		);
		// name 不可变（E3）
		V.expectReason(
			"A5 PATCH 改 metadata.name → 400 name_immutable",
			await H.req(server.base, path, {
				session,
				method: "PATCH",
				body: { ...base, metadata: { resourceVersion: cap.metadata?.resourceVersion, name: "c.renamed" }, spec: { next: [] } },
			}),
			400,
			"name_immutable",
		);
		// spec 只接受 next：内容不可变（v4 的「改内容请提交新版本」）
		V.expectReason(
			"A5 PATCH spec.run → 400 immutable_field",
			await H.req(server.base, path, {
				session,
				method: "PATCH",
				body: { ...base, metadata: { resourceVersion: cap.metadata?.resourceVersion }, spec: { run: V.MIN_RUN } },
			}),
			400,
			"immutable_field",
		);
		// 正常 PATCH：next 变了、version/uid 不变、resourceVersion 递增
		const patched = await H.req(server.base, path, {
			session,
			method: "PATCH",
			body: { ...base, metadata: { resourceVersion: cap.metadata?.resourceVersion }, spec: { next: ["c.versioned"] } },
		});
		V.expectStatus("A5 PATCH spec.next → 200", patched, 200);
		AEq("A5", "next 改成功", patched.json?.spec?.next, ["c.versioned"]);
		AEq("A5", "next 原地更新不产生内容版本", patched.json?.metadata?.version, cap.metadata?.version);
		A(
			"A5 CAS 令牌递增（旧令牌失效）",
			V.isResourceVersion(patched.json?.metadata?.resourceVersion) &&
				patched.json.metadata.resourceVersion !== cap.metadata.resourceVersion,
			`${cap.metadata?.resourceVersion} → ${patched.json?.metadata?.resourceVersion}`,
		);

		// DELETE：?resourceVersion= 必填（A9 的唯一承载方式）、过期 409、成功后 404
		V.expectReason(
			"A5 DELETE 缺 resourceVersion → 400 resource_version_required",
			await H.req(server.base, path, { session, method: "DELETE" }),
			400,
			"resource_version_required",
		);
		V.expectReason(
			"A5 DELETE 令牌过期 → 409 resource_version_conflict",
			await H.req(server.base, `${path}?resourceVersion=${encodeURIComponent(cap.metadata?.resourceVersion)}`, { session, method: "DELETE" }),
			409,
			"resource_version_conflict",
		);
		const del = await H.req(server.base, `${path}?resourceVersion=${encodeURIComponent(patched.json?.metadata?.resourceVersion)}`, { session, method: "DELETE" });
		AEq("A5", "DELETE 带当前令牌 → 204", del.status, 204);
		V.expectReason("A5 删除后 GET → 404", await H.req(server.base, path, { session }), 404, "NotFound");
		V.expectReason(
			"A5 重复删除（旧令牌）→ 404（对象已不存在，不泄露旧令牌有效性）",
			await H.req(server.base, `${path}?resourceVersion=${encodeURIComponent(patched.json?.metadata?.resourceVersion)}`, { session, method: "DELETE" }),
			404,
			"NotFound",
		);
	}

	// ================= A5b 一次提交的原子性（v4：next 就在同一份 spec 里） =================
	{
		const name = "c.atomic";
		const ok = await V.createCapability(server.base, session, NS, V.capabilityBody({ name, next: ["c.versioned"] }));
		V.expectStatus("A5b 带 next 创建能力 → 201", ok, 201);
		const edges = (await H.req(server.base, V.P.edges(NS), { session })).json?.items || [];
		A(
			"A5b 能力与 next 一起落库（派生投影立即可见）",
			edges.some((e) => e.from === name && e.to === "c.versioned" && e.kind === "capability"),
			JSON.stringify(edges.filter((e) => e.from === name)),
		);
		// 坏 next（非法引用）→ 400 且能力/边都不留（单事务）
		const badName = "c.atomic.bad";
		V.expectReason(
			"A5b next 里有非法引用 → 400",
			await V.createCapability(server.base, session, NS, {
				apiVersion: V.API_VERSION,
				kind: "Capability",
				metadata: { name: badName },
				spec: { next: ["has space"], run: V.MIN_RUN },
			}),
			400,
			"BadRequest",
		);
		A(
			"A5b 坏 next 让整个提交回滚：能力不在列表里",
			!(V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session })) || []).includes(badName),
			badName,
		);
		const edgesAfter = (await H.req(server.base, V.P.edges(NS), { session })).json?.items || [];
		A(
			"A5b 坏 next 让整个提交回滚：一条边都没留下",
			edgesAfter.filter((e) => e.from === badName).length === 0,
			JSON.stringify(edgesAfter.filter((e) => e.from === badName)),
		);
	}

	// ================= A12 空列表是 []（列表信封的 items，不是 null） =================
	{
		// 用一个**全新**的命名空间验空列表，避免「碰巧有数据」的假绿。
		const emptyNs = "empty" + Date.now().toString(36);
		V.expectStatus("A12 建空命名空间 → 201", await V.createNamespace(server.base, session, emptyNs, "空"), 201);
		for (const [label, path, kind] of [
			["capabilities", V.P.capabilities(emptyNs), "CapabilityList"],
			["sources", V.P.sources(emptyNs), "SourceList"],
			["tasks", V.P.tasks(emptyNs), "TaskList"],
			["events", V.P.events(emptyNs), "EventList"],
			["playbooks", V.P.playbooks(emptyNs), "PlaybookList"],
		]) {
			const r = await H.req(server.base, path, { session });
			const items = V.items(r);
			A("A12", `${label} 空列表是 items: []`, Array.isArray(items) && items.length === 0, String(r.text).slice(0, 120));
		}
		// edges / deployments 是保留域投影（不是资源信封）
		const edgesEmpty = await H.req(server.base, V.P.edges(emptyNs), { session });
		A("A12", "edges 空列表是 items: []", Array.isArray(edgesEmpty.json?.items) && edgesEmpty.json.items.length === 0, String(edgesEmpty.text).slice(0, 120));
		const depsEmpty = await H.req(server.base, V.P.deployments(emptyNs), { session });
		A("A12", "deployments 空列表是 []", Array.isArray(depsEmpty.json?.deployments) && depsEmpty.json.deployments.length === 0, String(depsEmpty.text).slice(0, 120));
		// nodes 是 cluster 级：此刻还没有节点
		const nodesEmpty = await H.req(server.base, "/v1/nodes", { session });
		A("A12", "nodes 空列表是 items: []", Array.isArray(nodesEmpty.json?.items) && nodesEmpty.json.items.length === 0, String(nodesEmpty.text).slice(0, 120));
	}

	// ================= A10 命名空间隔离（v4：ns 进路径） =================
	let isoNS;
	{
		const slug = "iso" + Date.now().toString(36);
		const created = await V.createNamespace(server.base, session, slug, "隔离验证");
		V.expectStatus("A10 建第二个命名空间 → 201", created, 201);
		isoNS = created.json?.metadata?.name;

		// ① 同名能力在两个 ns 是两个不同定义
		const sameA = await defineCap("same.name");
		const sameB = await V.createCapability(server.base, session, slug, V.capabilityBody({ name: "same.name" }));
		V.expectStatus("A10 在 B 空间建同名能力 → 201", sameB, 201);
		A("A10 同名能力是两个不同定义（uid 不同）", sameA.metadata?.uid !== sameB.json?.metadata?.uid, `${sameA.metadata?.uid} vs ${sameB.json?.metadata?.uid}`);
		const listA = V.itemNames(await H.req(server.base, V.P.capabilities(NS), { session }));
		const listB = V.itemNames(await H.req(server.base, V.P.capabilities(slug), { session }));
		A("A10 A 空间有 same.name", (listA || []).includes("same.name"), JSON.stringify(listA));
		A("A10 B 空间有 same.name", (listB || []).includes("same.name"), JSON.stringify(listB));
		A("A10 A 空间看不到 B 独有定义", !(listA || []).includes("c.origin"), JSON.stringify(listA));

		// ② 跨 ns 读 = 不存在（同码同体，防存在性探测）
		const cross = await H.req(server.base, V.P.capability(slug, "c.versioned"), { session });
		const ghost = await H.req(server.base, V.P.capability(slug, "nope.nope"), { session });
		A("A10 跨 ns 读与不存在都是 404", cross.status === 404 && ghost.status === 404, `cross=${cross.status} ghost=${ghost.status}`);
		A("A10 两种 404 逐字节相同", cross.text === ghost.text, JSON.stringify({ cross: cross.text, ghost: ghost.text }).slice(0, 200));

		// ③ 未知 ns → 404，不泄漏能力名
		const unknown = await H.req(server.base, V.P.capabilities("ns_bogus_xxxxxxxx"), { session });
		V.expectReason("A10 未知命名空间读 → 404", unknown, 404, "NotFound");
		A("A10 404 不泄漏能力名", !(unknown.text || "").includes("c.versioned"), String(unknown.text).slice(0, 120));
	}

	// ================= D6 保留期默认值（decisions 主动改默认） =================
	{
		const sys = await H.req(server.base, "/v1/system/config", { session });
		V.expectStatus("D6 GET /v1/system/config → 200", sys, 200);
		AEq("D6", "事件/trace 保留 30 天", sys.json?.retention?.eventTtlSec, 2592000);
		AEq("D6", "任务行保留 30 天", sys.json?.retention?.taskTtlSec, 2592000);
		AEq("D6", "结果（日志/产物）保留 7 天", sys.json?.retention?.resultTtlSec, 604800);
		A("D6 系统配置带 CAS 令牌", !!sys.json?.metadata?.resourceVersion, JSON.stringify(sys.json?.metadata || {}));
		// 三个保留期必须显式给（缺字段 ≠ 0 = 永久）：静默补默认会让「永久保留」被误开
		V.expectReason(
			"D6 PUT 缺一个保留期 → 400 value_invalid（0 也必须显式写）",
			await H.req(server.base, "/v1/system/config", {
				session,
				method: "PUT",
				body: { metadata: { resourceVersion: sys.json?.metadata?.resourceVersion }, retention: { eventTtlSec: 1, taskTtlSec: 2 } },
			}),
			400,
			"value_invalid",
		);
		V.expectReason(
			"D6 PUT 缺 CAS 令牌 → 400 resource_version_required",
			await H.req(server.base, "/v1/system/config", {
				session,
				method: "PUT",
				body: { metadata: {}, retention: { eventTtlSec: 1, taskTtlSec: 2, resultTtlSec: 3 } },
			}),
			400,
			"resource_version_required",
		);
		const put = await H.req(server.base, "/v1/system/config", {
			session,
			method: "PUT",
			body: {
				metadata: { resourceVersion: sys.json?.metadata?.resourceVersion },
				retention: { eventTtlSec: 111, taskTtlSec: 222, resultTtlSec: 0 },
			},
		});
		V.expectStatus("D6 PUT 三个保留期齐全 → 200", put, 200);
		AEq("D6", "显式 0 表示永久（原样存回）", put.json?.retention?.resultTtlSec, 0);
		AEq("D6", "新值原样读回", put.json?.retention?.eventTtlSec, 111);
	}

	// ================= 执行链（A6/A7/A8/A9 + D2）=================
	//
	// 这一段用共享 MockNode 扮演节点：握手 → 部署回执 → accept → lease.grant →
	// emit → result。真执行器（子进程、产物、取消、日志）在 node_v4（真 anode 二进制）。
	console.log("--- 执行链：emit 路由 / 环 / out 退役 / 不自动重试 / D2 两版配置 ---");
	const fin = await defineCap("c.fin");
	const fanA = await defineCap("c.fan.a");
	const work = await defineCap("c.work", { next: ["c.fin", "c.fan.a"] });
	const origin = await V.createSource(server.base, session, NS, V.sourceBody({ name: "c.origin", kind: "manual", targets: [{ capability: "c.work" }] }));
	V.expectStatus("建入口源 c.origin → 201", origin, 201);

	node = await N.MockNode.connect({
		base: server.base,
		dbPath: server.dbPath,
		namespaceId: nsId,
		namespaceSlug: NS,
		name: "contract-node",
		slots: 3,
	});
	A("A6", "mock 节点完成 v4 两段式握手（configRevision 对齐）", typeof node.configRevision === "string", String(node.configRevision));
	for (const c of [work, fin, fanA]) await deployToNode(c.metadata.name);

	// ---- A6 emit 路由：只能发给 next；带 id 幂等；不带 id 每次派发 ----
	{
		const { traceId, taskId, dispatch } = await startTask("A6 首跳");
		AEq("A6", "派发帧不授执行权（proposedAttempt 只是提案）", dispatch.proposedAttempt, 1);
		A("A6 派发帧不带 argv/shell/command（无 argv 不变量）", !/argv|shell|command/i.test(JSON.stringify(dispatch)), JSON.stringify(dispatch).slice(0, 160));
		const { acks } = await runTask(dispatch, {
			emits: [
				{ to: "c.fin", data: {}, id: "e1" },
				{ to: "c.fin", data: {}, id: "e1" }, // 同 id 同 to → 幂等
				{ to: "c.fan.a", data: {}, id: "e1" }, // 同 id 换 to → 409
				{ to: "c.fan.a", data: {} }, // 不带 id → 新派发
				{ to: "c.not.next", data: {} }, // 不在 next → 400
			],
		});
		A("A6 emit → next 里的目标 → 200 且回 taskId", acks[0]?.httpStatus === 200 && !!acks[0]?.response?.taskId, JSON.stringify(acks[0]));
		A("A6 同 id 同 to 重复 emit → 200 且同一个下游任务（D4 幂等）", acks[1]?.httpStatus === 200 && acks[1]?.response?.taskId === acks[0]?.response?.taskId, JSON.stringify(acks[1]));
		A("A6 同 id 换 to → 409 emit_id_conflict", acks[2]?.httpStatus === 409 && acks[2]?.error?.error === "emit_id_conflict", JSON.stringify(acks[2]));
		A("A6 不带 id 每次 emit 都是新派发（行为不变）", acks[3]?.httpStatus === 200 && acks[3]?.response?.taskId !== acks[0]?.response?.taskId, JSON.stringify(acks[3]));
		A("A6 emit 到 next 外 → 400 target_not_declared", acks[4]?.httpStatus === 400 && acks[4]?.error?.error === "target_not_declared", JSON.stringify(acks[4]));

		// 终态 + 下游：两次成功 emit → 恰好 2 个子任务（同一父、不同能力）
		const done = await waitPhase(taskId, "succeeded");
		A("A6 任务进 succeeded", !!done, String(done?.status?.phase));
		const tr = await traceOf(traceId);
		const children = (tr.tasks || []).filter((t) => t?.spec?.parentTaskId === taskId);
		AEq("A6", "两次成功 emit → 2 个子任务", children.length, 2);
		AEq(
			"A6",
			"子任务能力就是 emit 的 to",
			children.map((t) => t.spec.capability.name).sort(),
			["c.fan.a", "c.fin"],
		);
		AEq("A6", "父任务的 attempt 计在 grant 上（=1）", done?.status?.attempt, 1);
		// A8：任务信封里没有任何 out 键（v3 的 tasks.out 列与出口语义都已退役）
		const keys = [Object.keys(done || {}), Object.keys(done?.spec || {}), Object.keys(done?.status || {})].flat();
		A("A8 Task 信封（含 spec/status）没有 out 键", !keys.includes("out"), JSON.stringify(keys));
	}

	// ---- A8 不 emit → 没有下游（出口只在 emission 里）----
	{
		const { traceId, taskId, dispatch } = await startTask("A8 无 emit");
		await runTask(dispatch, { emits: [] });
		const done = await waitPhase(taskId, "succeeded");
		A("A8 不 emit 也能正常结束", !!done, String(done?.status?.phase));
		const tr = await traceOf(traceId);
		AEq("A8", "不 emit → 0 个子任务", (tr.tasks || []).filter((t) => t?.spec?.parentTaskId === taskId).length, 0);
	}

	// ---- A9 / D4 不自动重试（默认 idempotent:false）----
	{
		const { traceId, taskId, dispatch } = await startTask("A9 失败");
		await runTask(dispatch, { phase: "failed", summary: "exit status 7", error: { reason: "script_failed", message: "exit 7" } });
		const failed = await waitPhase(taskId, "failed");
		A("A9 失败任务进 failed", !!failed, String(failed?.status?.phase));
		AEq("A9", "失败原因来自节点上报（封闭码）", failed?.status?.result?.error?.reason, "script_failed");
		AEq("A9", "失败计入 1 次 attempt（grant 过就算）", failed?.status?.attempt, 1);
		// 给中枢一个「如果它会自动重试，一定会重试」的窗口：非幂等任务默认不重试（V4-D4）
		await new Promise((r) => setTimeout(r, 1500));
		const again = (await H.req(server.base, V.P.task(NS, taskId), { session })).json;
		AEq("A9", "非幂等任务失败后不自动重试（相位仍 failed）", again?.status?.phase, "failed");
		AEq("A9", "不自动重试（attempt 不增长）", again?.status?.attempt, 1);
		const tr = await traceOf(traceId);
		AEq("A9", "重试不新建 task 行（trace 里只有 1 个任务）", (tr.tasks || []).length, 1);
	}

	// ---- A7 环是合法的编排（v4 没有 path 判重 / hop 预算）----
	{
		const a = await defineCap("c.cycle.a", { next: ["c.cycle.b"] });
		const b = await defineCap("c.cycle.b", { next: ["c.cycle.a"] });
		const src = await V.createSource(server.base, session, NS, V.sourceBody({ name: "c.cycle.src", kind: "manual", targets: [{ capability: "c.cycle.a" }] }));
		V.expectStatus("A7 建环的入口源 → 201", src, 201);
		for (const c of [a, b]) await deployToNode(c.metadata.name);

		const ev = await postEvent({ source: "c.cycle.src", type: "start", payload: {} });
		AEq("A7", "第一跳建出 a 任务", (ev.json?.tasks || []).map((t) => t.capability), ["c.cycle.a"]);
		const taskA = ev.json?.tasks?.[0]?.taskId;
		const dA = await node.waitDispatch(taskA, 8000);
		A("A7 第一跳被派发", !!dA, JSON.stringify(node.frames.map((f) => f.type)));
		const cycA = await runTask(dA, { emits: [{ to: "c.cycle.b", data: {} }] });
		AEq("A7", "a emit 到 b → 200（环是合法编排，建边/声明都不拦）", cycA.acks[0]?.httpStatus, 200);
		const taskB = cycA.acks[0]?.response?.taskId;
		A("A7 第二跳任务真的建出来了（b）", !!taskB, String(taskB));
		await waitPhase(taskA, "succeeded");

		// 第二圈：b emit 回 a → 又建出一个 a 任务（v3 的 path 判重会在这里剪掉）
		const dB = await node.waitDispatch(taskB, 8000);
		A("A7 第二跳也被派发", !!dB, JSON.stringify(node.frames.map((f) => f.type)));
		const cycB = await runTask(dB, { emits: [{ to: "c.cycle.a", data: {} }] });
		AEq("A7", "b emit 回 a → 200（第二圈真的跑起来了）", cycB.acks[0]?.httpStatus, 200);
		const taskA2 = cycB.acks[0]?.response?.taskId;
		const a2 = (await H.req(server.base, V.P.task(NS, taskA2), { session })).json;
		AEq("A7", "第二圈的 a 任务带父链（parentTaskId = b 任务）", a2?.spec?.parentTaskId, taskB);
		AEq("A7", "第二圈任务能力仍是 c.cycle.a", a2?.spec?.capability?.name, "c.cycle.a");
	}

	// ---- D2 三层 vars：configRevision（机器）与 namespaceVarsRevision（ns）是两个字段 ----
	{
		// 能力声明它要 vars.SOURCE_ROOT；执行输入只该含**被引用的**名字。
		const envCap = await V.createCapability(server.base, session, NS, {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: "c.env" },
			spec: {
				params: { type: "object" },
				next: [],
				run: {
					entrypoint: "run.sh",
					files: { "run.sh": "#!/bin/sh\necho \"$SOURCE_ROOT\"\n" },
					env: { SOURCE_ROOT: { valueFrom: { fieldRef: { fieldPath: "vars.SOURCE_ROOT" } } } },
				},
				requires: { commands: [], values: ["vars.SOURCE_ROOT"] },
			},
		});
		V.expectStatus("D2 建声明 vars.SOURCE_ROOT 的能力 → 201", envCap, 201);
		await V.createSource(server.base, session, NS, V.sourceBody({ name: "c.env.src", kind: "manual", targets: [{ capability: "c.env" }] }));

		// ns 配置：缺令牌 400、写成功、读回
		const cfg0 = await H.req(server.base, V.P.namespaceConfig(NS), { session });
		V.expectStatus("D2 GET ns config → 200", cfg0, 200);
		A("D2 ns config 带 CAS 令牌 + namespaceVarsRevision", !!cfg0.json?.metadata?.resourceVersion && cfg0.json?.namespaceVarsRevision !== undefined, JSON.stringify(cfg0.json?.metadata || {}));
		V.expectReason(
			"D2 PUT ns config 缺令牌 → 400 resource_version_required",
			await H.req(server.base, V.P.namespaceConfig(NS), { session, method: "PUT", body: { metadata: {}, vars: { SOURCE_ROOT: "/x" } } }),
			400,
			"resource_version_required",
		);
		const put = await H.req(server.base, V.P.namespaceConfig(NS), {
			session,
			method: "PUT",
			body: {
				metadata: { resourceVersion: cfg0.json?.metadata?.resourceVersion },
				vars: { SOURCE_ROOT: "/tmp/src", UNUSED_BY_ANY_CAPABILITY: "zzz" },
			},
		});
		V.expectStatus("D2 PUT ns config → 200", put, 200);
		AEq("D2", "vars 原样读回（往返一致）", put.json?.vars, { SOURCE_ROOT: "/tmp/src", UNUSED_BY_ANY_CAPABILITY: "zzz" });
		A(
			"D2 namespaceVarsRevision 跟着写递增",
			put.json?.namespaceVarsRevision !== cfg0.json?.namespaceVarsRevision,
			`${cfg0.json?.namespaceVarsRevision} → ${put.json?.namespaceVarsRevision}`,
		);

		await deployToNode("c.env");
		const ev = await postEvent({ source: "c.env.src", type: "go", payload: {} });
		AEq("D2", "环境能力事件建出 1 个任务", (ev.json?.tasks || []).length, 1);
		const d = await node.waitDispatch(ev.json?.tasks?.[0]?.taskId, 8000);
		A("D2 任务被派发", !!d, JSON.stringify(node.frames.map((f) => f.type)));
		AEq(
			"D2",
			"派发帧只带被引用的 ns 默认值（UNUSED 不出现）",
			d?.namespaceVars,
			{ SOURCE_ROOT: "/tmp/src" },
		);
		AEq("D2", "configRevision 是机器 revision（与 ns revision 不同字段）", d?.configRevision, node.configRevision);
		AEq("D2", "namespaceVarsRevision 就是 ns 配置的 revision", d?.namespaceVarsRevision, put.json?.namespaceVarsRevision);
		await runTask(d, { emits: [] });
		const done = await waitPhase(ev.json?.tasks?.[0]?.taskId, "succeeded");
		A("D2 带 ns 默认值的任务正常跑完", !!done, String(done?.status?.phase));
		AEq("D2", "任务 status 记录本次 ns 默认值的版本", done?.status?.namespaceVarsRevision, put.json?.namespaceVarsRevision);
	}

	// ================= A10（续）事件/任务不跨命名空间 =================
	{
		const ev = await postEvent({ source: "c.origin", type: "go", payload: {} });
		const traceId = ev.json?.traceId;
		const inA = await H.req(server.base, V.P.trace(NS, traceId), { session });
		V.expectStatus("A10 A 空间查得到自己的 trace", inA, 200);
		A(
			"A10 trace 里既有事件也有任务",
			(inA.json?.events || []).length >= 1 && (inA.json?.tasks || []).length >= 1,
			JSON.stringify({ e: inA.json?.events?.length, t: inA.json?.tasks?.length }),
		);
		V.expectReason("A10 B 空间查不到 A 空间的 trace", await H.req(server.base, V.P.trace(isoNS, traceId), { session }), 404, "NotFound");
		const eventsB = await H.req(server.base, `/v1/console/events?namespace=${encodeURIComponent(isoNS)}&traceId=${encodeURIComponent(traceId)}`, { session });
		V.expectStatus("A10 B 空间 console events → 200", eventsB, 200);
		AEq("A10", "B 空间 console events 是空的 items: []", V.items(eventsB)?.length, 0);
	}

	if (node) node.close();
	server.stop();
	const allPassed = H.summary("contract");
	H.writeResults("contract", H.stopTimer());

	// 汇总：按决策编号报告哪些没通过（v4 用 A/D 混排）
	const failed = results.filter((r) => !r.ok).map((r) => r.id);
	if (failed.length) {
		console.log("\n未通过的契约断言: " + [...new Set(failed)].join(", "));
	}
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	if (node) {
		try {
			node.close();
		} catch {
			/* 已关闭 */
		}
	}
	if (server) server.stop();
	process.exit(1);
});
