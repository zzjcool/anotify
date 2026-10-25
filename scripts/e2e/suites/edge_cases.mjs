#!/usr/bin/env node
/* SUITE: edge_cases — 边界 / 并发 / Unicode（事件中心模型）
 *
 * #80 之后旧「通知/消息」模型已下线（/v1/notify、/v1/notifications、broker 删除）。
 * 原先基于 seq 单调、title/body 透传、deviceTags 广播的断言都失去意义了。
 * 这里改为验证**事件与任务**的等价边界：
 *
 *  1. 并发提交 10 条事件 → 全 200，事件全部落库、ID 唯一
 *  2. Unicode/Emoji/换行/引号/尖括号 → 200 且 payload 原样存回（按文本存，不做转义）
 *  3. 超长 payload 字段 → 明确 200 或 400，绝不 500
 *  4. 同一 idemKey 并发提交 → 只产生一个事件（幂等约束在并发下成立）
 *  5. 不存在的 source → 422（不是静默 200），错误里带 available 列表
 *  6. 极大 limit=99999 → 钳制，不 500
 *  7. 负数/非法 limit → 不 500，按默认处理
 */
import * as H from "../lib/harness.mjs";

let server;
let sendKey, session;

// notify 提交一条事件。payload 任意，用于验证原样存取。
async function notify(body, key = sendKey) {
	return H.req(server.base, "/v1/events", { key, body });
}

async function main() {
	console.log("=== SUITE: edge_cases（边界/并发/Unicode）===");
	server = await H.startServer({ suiteName: "edge_cases", rpId: "localhost" });
	const s = H.seed(server.dbPath, "edge");
	sendKey = s.sendKey;
	session = s.session;

	// ---- 1. 并发提交事件 ----
	console.log("--- 并发提交 ---");
	const N = 10;
	const results = await Promise.all(
		Array.from({ length: N }, (_, i) =>
			notify({
				source: "github",
				type: "edge.tick",
				payload: { seq: i },
			}),
		),
	);
	H.check(
		`并发 ${N} 条事件全部 200`,
		results.every((r) => r.status === 200),
		results.map((r) => r.status).join(","),
	);
	const evIDs = results.map((r) => r.json?.eventId).filter(Boolean);
	H.eq("并发后事件 ID 数 = N", evIDs.length, N);
	H.eq("并发后事件 ID 无重复", new Set(evIDs).size, N);

	// ---- 2. Unicode / Emoji / 特殊字符 ----
	console.log("--- Unicode/特殊字符 ---");
	const unicodeTitle =
		"部署完成 🎉 中文「引号」<script>alert(1)</script>\n换行\t制表";
	const unicodeBody = "正文：émojis 🚀🔥、特殊字符 <>\"'&、多行\n第二行";
	const u = await notify({
		source: "github",
		type: "edge.tick",
		payload: { title: unicodeTitle, body: unicodeBody },
	});
	H.eq("Unicode/Emoji/特殊字符事件 → 200", u.status, 200);

	// 事件列表里能找到，且 payload 原样存回（不转义、不截断）
	const list = await H.req(server.base, "/v1/console/events?limit=100", {
		session,
	});
	H.eq("console/events → 200", list.status, 200);
	const events = list.json?.events || [];
	const found = events.find((e) => e.id === u.json?.eventId);
	H.check("Unicode 事件可查回", !!found, u.json?.eventId);
	if (found) {
		// payload 是 json.RawMessage → 序列化后已是 JSON 对象，
		// 不再需要二次 JSON.parse（那是字符串形态才需要的）。
		const payload =
			typeof found.payload === "string"
				? JSON.parse(found.payload)
				: found.payload || {};
		H.eq("Unicode title 原样存回", payload.title, unicodeTitle);
		H.eq("Unicode body 原样存回", payload.body, unicodeBody);
	}

	// ---- 3. 超长字段 ----
	console.log("--- 超长字段 ---");
	const longTitle = "x".repeat(5000);
	const longBody = "y".repeat(50000);
	const big = await notify({
		source: "github",
		type: "edge.tick",
		payload: { title: longTitle, body: longBody },
	});
	H.check(
		"超长 payload 不 500（200 收下或 400 拒绝）",
		big.status === 200 || big.status === 400,
		`got ${big.status}`,
	);

	// 超大 body（>1MB，超过 MaxBytesReader）必须 400 而不是 500
	const huge = await notify({
		source: "github",
		type: "edge.tick",
		payload: { body: "z".repeat(2 << 20) },
	});
	H.check(
		">1MB 请求体被拒（400，不 500）",
		huge.status === 400 || huge.status === 413,
		`got ${huge.status}`,
	);

	// ---- 4. 同一 idemKey 并发提交 ----
	console.log("--- 幂等并发 ---");
	const sameKey = "edge-idem-" + Date.now();
	const idem = await Promise.all(
		Array.from({ length: 5 }, () =>
			notify({
				source: "github",
				type: "edge.tick",
				idemKey: sameKey,
				payload: { n: 1 },
			}),
		),
	);
	H.check(
		"幂等并发全部 200",
		idem.every((r) => r.status === 200),
		idem.map((r) => r.status).join(","),
	);
	const idemIDs = idem.map((r) => r.json?.eventId).filter(Boolean);
	H.eq("同一 idemKey 只产生一个事件", new Set(idemIDs).size, 1);

	// ---- 5. 未定义的 source → 422 ----
	//
	// 早先是 200 + origin:""（"事件收下但派不出去"）。那会让调用方以为成功，
	// 实际什么都没发生。现在显式 422 并列出可用来源。
	console.log("--- 未定义的 source ---");
	const noOrigin = await notify({
		source: "no.such.source." + Date.now(),
		type: "whatever",
		payload: {},
	});
	H.eq("未定义 source → 422（非静默 200）", noOrigin.status, 422);
	H.check(
		"422 带 available 列表",
		Array.isArray(noOrigin.json?.available) && noOrigin.json.available.length > 0,
		JSON.stringify(noOrigin.json),
	);
	H.check(
		"available 提示含内置起点 github",
		(noOrigin.json?.available || []).includes("github"),
		JSON.stringify(noOrigin.json?.available),
	);
	H.check(
		"422 如实回传 eventId（事件确实已入库）",
		typeof noOrigin.json?.eventId === "string" && noOrigin.json.eventId.length > 0,
		JSON.stringify(noOrigin.json),
	);

	// ---- 6/7. limit 边界 ----
	console.log("--- limit 边界 ---");
	const bigLimit = await H.req(
		server.base,
		"/v1/console/events?limit=99999",
		{ session },
	);
	H.check("极大 limit 不 500", bigLimit.status !== 500, `got ${bigLimit.status}`);
	H.eq("极大 limit → 200", bigLimit.status, 200);

	const negLimit = await H.req(
		server.base,
		"/v1/console/events?limit=-5",
		{ session },
	);
	H.check("负数 limit 不 500", negLimit.status !== 500, `got ${negLimit.status}`);

	const badLimit = await H.req(
		server.base,
		"/v1/console/events?limit=abc",
		{ session },
	);
	H.check("非法 limit 不 500", badLimit.status !== 500, `got ${badLimit.status}`);

	// 空 trace 过滤 → [] 而非 null（不变量 I9）
	const emptyTrace = await H.req(
		server.base,
		"/v1/console/events?traceId=trace_does_not_exist",
		{ session },
	);
	H.eq("空 trace 过滤 → 200", emptyTrace.status, 200);
	H.check(
		"空 trace 过滤返回 [] 而非 null",
		Array.isArray(emptyTrace.json?.events) &&
			emptyTrace.json.events.length === 0,
		JSON.stringify(emptyTrace.json),
	);

	const passed = H.summary("edge_cases");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	try {
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
