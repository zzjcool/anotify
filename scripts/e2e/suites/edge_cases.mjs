#!/usr/bin/env node
/* SUITE: edge_cases — v4 边界 / 并发 / 编码 / CAS / 分页（事件中心模型）
 *
 * v3 的「通知/消息」模型早已下线，v4 又把资源面换成了
 * `/v1/namespaces/{ns}/…` + 信封（apiVersion/kind/metadata/spec/status）。本套件钉的是
 * **这些新形状在边界上的行为**，每条都对应契约里的一句话：
 *
 *   1. 并发：10 条事件同时投递 → 全 200、eventId 唯一（事务/去重不是串行的假象）
 *   2. 编码：Unicode/Emoji/换行/引号/尖括号 → 原样存回（按文本存，不转义、不截断）
 *   3. 尺寸：超长字段 200；> 4 MiB 请求体 400（**不是 500**：调用方的错不是服务器坏了）
 *   4. 幂等：同一投递 id 并发 5 次 → 只产生一条事件（重复回 duplicate=true）
 *   5. 源不存在 → 404（绝不静默收下一个「不知道谁发的」事件）
 *   6. 空列表 → `items: []`（不是 null：前端把 null 当「未连接」）
 *   7. limit 边界：极大 200（钳到 500）、负数 200（落回默认）、非整数 400
 *   8. 未知 query 参数 → 400 unknown_field（静默忽略会让「筛过了」变成假象）
 *   9. 分页游标：绑 (ns, 筛选条件)，跨 ns 复用 → 400；坏游标 → 400
 *  10. CAS：缺令牌 400 / 坏令牌 400 / 过期令牌 409（带 current）/ 正常一次 200
 *  11. 写请求的封闭字段面：未知字段 400 unknown_field、`status` 400 status_not_writable
 *  12. 超长资源名 → 400（不是 500、也不是静默截断）
 *
 * 纪律：并发用例必须真的并发（Promise.all），串行跑看不出竞态；
 * 负向用例断言**具体状态码 + reason**，只断「不是 200」会让 500 也算过。
 */
import { randomUUID } from "node:crypto";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "edge_cases";
const NS = V.DEFAULT_NS;

// 建一个可投递的源（v4 ingress 要求源已存在且启用）。
async function mkSource(base, session, name, targets = []) {
	const r = await V.createSource(
		base,
		session,
		NS,
		V.sourceBody({ name, kind: "manual", enabled: true, targets }),
	);
	V.expectStatus(`建源 ${name}`, r, 201);
	return r.json;
}

// 投递一条事件（Bearer API key 的机器入口）。
function emit(base, key, body) {
	return V.ingestEvent(base, key, NS, {
		source: body.source,
		type: body.type || "edge.tick",
		...(body.id ? { id: body.id } : {}),
		payload: body.payload === undefined ? {} : body.payload,
	});
}

// ---------- 1. 并发 ----------
async function partConcurrent(base, seeded) {
	console.log("--- 1. 并发投递 ---");
	const N = 10;
	const results = await Promise.all(
		Array.from({ length: N }, (_, i) =>
			emit(base, seeded.sendKey, { source: "edge-src", payload: { seq: i } }),
		),
	);
	H.check(
		`并发 ${N} 条事件全部 200`,
		results.every((r) => r.status === 200),
		results.map((r) => `${r.status}:${String(r.text).slice(0, 80)}`).join(" | "),
	);
	const ids = results.map((r) => r.json?.eventId).filter(Boolean);
	H.eq("并发后事件 ID 数 = N（每条都有回执 id）", ids.length, N);
	H.eq("并发后事件 ID 无重复", new Set(ids).size, N);
	H.check(
		"每条回执都带 traceId（同一条事件的所有派生共享它）",
		results.every((r) => typeof r.json?.traceId === "string" && r.json.traceId.length > 0),
		JSON.stringify(results.map((r) => r.json?.traceId)).slice(0, 300),
	);
	// 落库核对：事件流里的条数与回执一致（不留「回了 200 但没写进去」）。
	const list = await H.req(base, `${V.P.events(NS)}?limit=100`, { session: seeded.session });
	V.expectStatus("事件列表 → 200", list, 200);
	const stored = new Set((V.items(list) || []).map((e) => e.metadata?.name));
	const missing = ids.filter((id) => !stored.has(id));
	H.eq("并发投递的每条事件都能在列表里查到", missing, []);
	return { ids };
}

// ---------- 2. 编码 ----------
async function partUnicode(base, seeded) {
	console.log("--- 2. Unicode / Emoji / 特殊字符 ---");
	const title = "部署完成 🎉 中文「引号」<script>alert(1)</script>\n换行\t制表";
	const body = "正文：émojis 🚀🔥、特殊字符 <>\"'&、多行\n第二行";
	const r = await emit(base, seeded.sendKey, { source: "edge-src", payload: { title, body } });
	V.expectStatus("Unicode/Emoji/特殊字符事件 → 200", r, 200);

	const list = await H.req(base, `${V.P.events(NS)}?limit=100`, { session: seeded.session });
	const found = (V.items(list) || []).find((e) => e.metadata?.name === r.json?.eventId);
	H.check("Unicode 事件能在事件流里查回", !!found, r.json?.eventId);
	if (found) {
		V.assertEnvelope("Event", found, "Event");
		// v4 的 payload 在 spec.payload（不是 v3 的顶层 payload 列）。
		H.eq("Unicode title 原样存回（不转义）", found.spec?.payload?.title, title);
		H.eq("Unicode body 原样存回（不截断）", found.spec?.payload?.body, body);
		H.eq("事件类型原样存回", found.spec?.eventType, "edge.tick");
		H.eq("事件归属来源名原样存回", found.spec?.involvedObject?.name, "edge-src");
	}
}

// ---------- 3. 尺寸 ----------
async function partSizeLimits(base, seeded) {
	console.log("--- 3. 超长字段与超大请求体 ---");
	const long = await emit(base, seeded.sendKey, {
		source: "edge-src",
		payload: { title: "x".repeat(5000), body: "y".repeat(50000) },
	});
	V.expectStatus("超长 payload 字段（50 KB）→ 200", long, 200);

	// 4 MiB 是 ingress 的投递上限（source.md §5）：超限必须是**调用方的错**（400），
	// 不能落成 500 —— 5xx 会让运维去查服务器，而真相是「body 太大了」。
	const huge = await emit(base, seeded.sendKey, {
		source: "edge-src",
		payload: { body: "z".repeat(5 << 20) },
	});
	H.eq("超 4 MiB 请求体 → 400（不是 500）", huge.status, 400);
	H.eq("超限错误体带 Status.reason", huge.json?.reason, "BadRequest");

	// 边界正例：payload 里有 3 MiB 字符串仍然收下（上限判断是 4 MiB 而不是 1 MB）。
	const below = await emit(base, seeded.sendKey, {
		source: "edge-src",
		payload: { body: "w".repeat(3 << 20) },
	});
	V.expectStatus("3 MiB payload 仍被收下（上限是 4 MiB）", below, 200);
}

// ---------- 4. 幂等 ----------
async function partIdempotency(base, seeded) {
	console.log("--- 4. 同一投递 id 的幂等（并发） ---");
	const id = "edge-idem-" + randomUUID();
	const results = await Promise.all(
		Array.from({ length: 5 }, () => emit(base, seeded.sendKey, { source: "edge-src", id, payload: { n: 1 } })),
	);
	H.check(
		"幂等并发全部 200",
		results.every((r) => r.status === 200),
		results.map((r) => `${r.status}:${String(r.text).slice(0, 80)}`).join(" | "),
	);
	const ids = results.map((r) => r.json?.eventId).filter(Boolean);
	H.eq("同一投递 id 只产生一个事件", new Set(ids).size, 1);
	const dupes = results.filter((r) => r.json?.duplicate === true).length;
	H.check("重复投递被显式标记 duplicate=true（而不是静默再造一条）", dupes >= 1, JSON.stringify(results.map((r) => r.json?.duplicate)));

	// 串行重放同样幂等（并发去重不能只在单线程里成立）。
	const again = await emit(base, seeded.sendKey, { source: "edge-src", id, payload: { n: 2 } });
	V.expectStatus("同 id 再投一次 → 200", again, 200);
	H.eq("串行重放仍是同一条事件", again.json?.eventId, ids[0]);
	H.eq("串行重放被标记 duplicate", again.json?.duplicate, true);
}

// ---------- 5/6. 源不存在 / 空列表 ----------
async function partMissingSourceAndEmptyList(base, seeded) {
	console.log("--- 5/6. 源不存在 → 404；空列表 → [] ---");
	const missing = await emit(base, seeded.sendKey, { source: "edge-no-such-source", payload: {} });
	V.expectReason("未知 source → 404（不静默收下）", missing, 404, "NotFound");

	// 空 trace 过滤：`items: []` 而不是 null（I12 空列表纪律）。
	const empty = await H.req(base, `${V.P.events(NS)}?trace=trace_does_not_exist`, { session: seeded.session });
	V.expectStatus("空 trace 过滤 → 200", empty, 200);
	const items = V.items(empty);
	H.check("空结果仍是数组（不是 null）", Array.isArray(items), JSON.stringify(empty.json).slice(0, 200));
	H.eq("空结果长度 0", items?.length, 0);
	H.check(
		"列表信封的五键纪律（apiVersion/kind/metadata/items）",
		empty.json?.apiVersion === V.API_VERSION && empty.json?.kind === "EventList" && typeof empty.json?.metadata === "object",
		JSON.stringify(empty.json && Object.keys(empty.json)),
	);
}

// ---------- 7/8. limit 与未知参数 ----------
async function partQueryBoundaries(base, seeded) {
	console.log("--- 7/8. limit 边界 / 未知 query 参数 ---");
	{
		const big = await H.req(base, `${V.P.events(NS)}?limit=99999`, { session: seeded.session });
		V.expectStatus("极大 limit → 200（钳到上限，不 5xx）", big, 200);
		H.check("极大 limit 被钳到 500 条以内", (V.items(big) || []).length <= 500, String((V.items(big) || []).length));
	}
	{
		const neg = await H.req(base, `${V.P.events(NS)}?limit=-5`, { session: seeded.session });
		V.expectStatus("负数 limit → 200（落回默认窗口）", neg, 200);
	}
	{
		const bad = await H.req(base, `${V.P.events(NS)}?limit=abc`, { session: seeded.session });
		V.expectReason("非整数 limit → 400（不静默落回默认）", bad, 400, "BadRequest");
	}
	{
		// tasks 列表只接受 status/traceId/nodeId/limit：旧名字（phase/trace/node）必须 400，
		// 否则调用方以为筛过了，其实拿到的是全量。
		const unknown = await H.req(base, `${V.P.tasks(NS)}?phase=running`, { session: seeded.session });
		V.expectReason("tasks 列表未知 query 参数 → 400 unknown_field", unknown, 400, "unknown_field");
	}
	{
		const ok = await H.req(base, `${V.P.tasks(NS)}?status=running&limit=5`, { session: seeded.session });
		V.expectStatus("tasks 列表合法筛选 → 200", ok, 200);
	}
}

// ---------- 9. 游标绑定 ----------
// 游标是 (ns id + 筛选条件) 的不透明绑定：拿 A 空间的游标读 B 空间的记录必须 400。
// 这不是「分页细节」，是越权探针的入口。
async function partCursorBinding(base, seeded) {
	console.log("--- 9. 分页游标绑定 ns ---");
	const ns2 = await V.createNamespace(base, seeded.session, "edge-second", "游标用");
	V.expectStatus("建第二个命名空间", ns2, 201);
	const slug2 = ns2.json?.metadata?.name || "edge-second";

	// 至少两条事件才能有第二页。
	for (let i = 0; i < 2; i++) {
		await emit(base, seeded.sendKey, { source: "edge-src", payload: { page: i } });
	}
	const page1 = await H.req(base, `${V.P.consoleEvents(NS)}&limit=1`, { session: seeded.session });
	V.expectStatus("console events 第一页 → 200", page1, 200);
	H.eq("limit=1 只回一条", (V.items(page1) || []).length, 1);
	const cursor = page1.json?.metadata?.continue;
	H.check("还有下一页时给出不透明 continue", typeof cursor === "string" && cursor.length > 0, JSON.stringify(page1.json?.metadata));

	if (cursor) {
		const page2 = await H.req(base, `${V.P.consoleEvents(NS)}&limit=1&continue=${encodeURIComponent(cursor)}`, { session: seeded.session });
		V.expectStatus("同 ns 用游标取第二页 → 200", page2, 200);
		const a = (V.items(page1) || [])[0]?.metadata?.name;
		const b = (V.items(page2) || [])[0]?.metadata?.name;
		H.check("两页不重叠（keyset 游标，不是 OFFSET）", !!a && !!b && a !== b, `${a} vs ${b}`);

		const crossed = await H.req(
			base,
			`/v1/console/events?namespace=${encodeURIComponent(slug2)}&limit=1&continue=${encodeURIComponent(cursor)}`,
			{ session: seeded.session },
		);
		V.expectReason("跨 ns 复用游标 → 400（游标绑 ns，不能当越权探针）", crossed, 400, "BadRequest");
	}
	const malformed = await H.req(base, `${V.P.consoleEvents(NS)}&continue=not-a-cursor`, { session: seeded.session });
	V.expectReason("坏游标 → 400（不静默当第一页）", malformed, 400, "BadRequest");
}

// ---------- 10/11/12. CAS / 封闭字段面 / 超长名 ----------
async function partCASAndWriteSurface(base, seeded) {
	console.log("--- 10. CAS（resourceVersion）ABA ---");
	const created = await V.createCapability(base, seeded.session, NS, V.capabilityBody({ name: "edge-cas-cap" }));
	V.expectStatus("建能力", created, 201);
	const rv1 = created.json?.metadata?.resourceVersion;
	H.check("建出来的能力带 CAS 令牌（rv1:…）", V.isResourceVersion(rv1), String(rv1));

	// ① 缺令牌 → 400 resource_version_required（不猜一个「最新」替客户端做决定）
	{
		const r = await H.req(base, V.P.capability(NS, "edge-cas-cap"), {
			session: seeded.session,
			method: "PATCH",
			body: { apiVersion: V.API_VERSION, kind: "Capability", metadata: {}, spec: { next: [] } },
		});
		V.expectReason("PATCH 缺 resourceVersion → 400", r, 400, "resource_version_required");
	}
	// ② 坏令牌 → 400 resource_version_invalid
	{
		const r = await H.req(base, V.P.capability(NS, "edge-cas-cap"), {
			session: seeded.session,
			method: "PATCH",
			body: { apiVersion: V.API_VERSION, kind: "Capability", metadata: { resourceVersion: "not-a-token" }, spec: { next: [] } },
		});
		V.expectReason("PATCH 坏 resourceVersion → 400", r, 400, "resource_version_invalid");
	}
	// ③ 正常一次：令牌换代
	const patched = await H.req(base, V.P.capability(NS, "edge-cas-cap"), {
		session: seeded.session,
		method: "PATCH",
		body: {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { resourceVersion: rv1, displayName: "边界能力" },
			spec: { next: [] },
		},
	});
	V.expectStatus("PATCH 带当前令牌 → 200", patched, 200);
	H.eq("displayName 生效", patched.json?.metadata?.displayName, "边界能力");
	const rv2 = patched.json?.metadata?.resourceVersion;
	H.check("令牌换代（新 != 旧）", V.isResourceVersion(rv2) && rv2 !== rv1, `${rv1} → ${rv2}`);

	// ④ ABA：拿着**旧**令牌再写一次 → 409，并给出当前令牌（客户端据此重建，不必猜）
	{
		const r = await H.req(base, V.P.capability(NS, "edge-cas-cap"), {
			session: seeded.session,
			method: "PATCH",
			body: {
				apiVersion: V.API_VERSION,
				kind: "Capability",
				metadata: { resourceVersion: rv1, displayName: "重放" },
				spec: { next: [] },
			},
		});
		V.expectReason("旧令牌重放（ABA）→ 409", r, 409, "resource_version_conflict");
		H.eq("409 带 details.current.resourceVersion = 当前令牌", r.json?.details?.current?.resourceVersion, rv2);
		H.eq("被拒的重放没有改到 displayName", (await H.req(base, V.P.capability(NS, "edge-cas-cap"), { session: seeded.session })).json?.metadata?.displayName, "边界能力");
	}
	// ⑤ DELETE 的 CAS 走 query（唯一承载方式）；缺失 → 400
	{
		const r = await H.req(base, V.P.capability(NS, "edge-cas-cap"), { session: seeded.session, method: "DELETE" });
		V.expectReason("DELETE 缺 ?resourceVersion= → 400", r, 400, "resource_version_required");
	}

	console.log("--- 11. 写请求的封闭字段面 ---");
	{
		const r = await V.createCapability(base, seeded.session, NS, {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: "edge-unknown-field", bogusKey: 1 },
			spec: { params: { type: "object" }, next: [], run: V.MIN_RUN },
		});
		V.expectReason("metadata 里的未知字段 → 400 unknown_field", r, 400, "unknown_field");
	}
	{
		const doc = V.capabilityBody({ name: "edge-status-write" });
		doc.status = { conditions: [] };
		const r = await V.createCapability(base, seeded.session, NS, doc);
		V.expectReason("写请求带 status → 400 status_not_writable", r, 400, "status_not_writable");
	}
	{
		const doc = V.capabilityBody({ name: "edge-kind-mismatch" });
		doc.kind = "Source";
		const r = await V.createCapability(base, seeded.session, NS, doc);
		V.expectReason("kind 与端点不符 → 400 kind_mismatch", r, 400, "kind_mismatch");
	}
	{
		// metadata.namespace 与路径 ns 冲突：两条入口给了两个不同的作用域，必须拒绝
		// （静默用其中一个会让资源落到调用方没预期的命名空间）。
		const doc = V.capabilityBody({ name: "edge-ns-mismatch", namespace: "edge-other" });
		const r = await V.createCapability(base, seeded.session, NS, doc);
		V.expectReason("metadata.namespace 与路径不一致 → 400 namespace_mismatch", r, 400, "namespace_mismatch");
	}

	console.log("--- 12. 超长资源名 ---");
	{
		const r = await V.createCapability(base, seeded.session, NS, V.capabilityBody({ name: "n".repeat(300) }));
		V.expectReason("超长能力名 → 400（不 500、不截断）", r, 400, "BadRequest");
	}
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log("=== SUITE: edge_cases（v4 边界 / 并发 / 编码 / CAS）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	try {
		const seeded = H.seed(server.dbPath, "edge");
		await mkSource(server.base, seeded.session, "edge-src");

		await partConcurrent(server.base, seeded);
		await partUnicode(server.base, seeded);
		await partSizeLimits(server.base, seeded);
		await partIdempotency(server.base, seeded);
		await partMissingSourceAndEmptyList(server.base, seeded);
		await partQueryBoundaries(server.base, seeded);
		await partCursorBinding(server.base, seeded);
		await partCASAndWriteSurface(server.base, seeded);
	} finally {
		server.stop();
	}

	const passed = H.summary(SUITE);
	process.exitCode = passed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e && e.stack ? e.stack : e).slice(0, 800));
	H.summary(SUITE);
	process.exit(1);
});
