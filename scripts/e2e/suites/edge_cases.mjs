#!/usr/bin/env node
/* SUITE: edge_cases — 边界 / 并发 / Unicode
 *
 * 覆盖 case：
 *  1. 并发上报 10 条 → 全 200；Replay 回来 seq 为 1..10 无重复无缺口（broker 并发 seq 事务正确）
 *  2. Unicode/Emoji/换行/引号/<script> → 200 且 Replay 内容一致（按文本存储）
 *  3. 超长 title(5000) / 超长 body → 行为明确（200 或 400，绝不 500）
 *  4. 空 deviceTags [] vs 省略 → 都是广播（matched 一致）
 *  5. deviceTags 含空串/纯空格 → 归一化剔除，不报错
 *  6. 重复 endpoint 设备 → Upsert（列表只一个）
 *  7. 极大 limit=99999 → 钳制，不 500
 *  8. 负数/非法 sinceSeq → 不 500，按 0 处理
 */
import * as H from "../lib/harness.mjs";

let server;
let sendKey, session;

async function notify(body, key = sendKey) {
	return H.req(server.base, "/v1/notify", { key, body });
}

async function main() {
	console.log("=== SUITE: edge_cases（边界/并发/Unicode）===");
	server = await H.startServer({ rpId: "localhost" });
	const s = H.seed(server.dbPath, "edge");
	sendKey = s.sendKey;
	session = s.session;

	// ---- 1. 并发上报 seq 单调无缺口 ----
	console.log("--- 并发上报 ---");
	const N = 10;
	const results = await Promise.all(
		Array.from({ length: N }, (_, i) =>
			notify({ title: `并发-${i}`, status: "success" }),
		),
	);
	const allOk = results.every((r) => r.status === 200);
	H.check(`并发 ${N} 条上报全部 200`, allOk, results.map((r) => r.status).join(","));

	const list = await H.req(server.base, "/v1/notifications?limit=50", { session });
	// 注意：/v1/notifications 直接序列化 broker.Message（Go 默认 PascalCase 字段名，无 json tag）
	const seqs = (list.json?.notifications || []).map((m) => m.Seq).sort((a, b) => a - b);
	const expected = Array.from({ length: N }, (_, i) => i + 1);
	H.eq("并发后 seq 为 1..N 无重复无缺口", seqs, expected);

	// ---- 2. Unicode / Emoji / 特殊字符 ----
	console.log("--- Unicode/特殊字符 ---");
	const unicodeTitle = "部署完成 🎉 中文「引号」<script>alert(1)</script>\n换行\t制表";
	const unicodeBody = "正文：émojis 🚀🔥、特殊字符 <>\"'&、多行\n第二行";
	const u = await notify({ title: unicodeTitle, body: unicodeBody, status: "success" });
	H.eq("Unicode/Emoji/特殊字符上报 → 200", u.status, 200);
	const u2 = await H.req(server.base, "/v1/notifications?limit=50", { session });
	const found = (u2.json?.notifications || []).find((m) => m.ID === u.json?.id);
	H.check("Unicode 内容 Replay 一致", !!found && found.Title === unicodeTitle && found.Body === unicodeBody,
		found ? `title=${JSON.stringify(found.Title).slice(0, 60)}` : "未找到该消息");

	// ---- 3. 超长 title / body ----
	console.log("--- 超长字段 ---");
	const longTitle = await notify({ title: "T".repeat(5000), status: "success" });
	H.check("超长 title(5000) 不 500", longTitle.status !== 500, `got ${longTitle.status}`);
	const longBody = await notify({ title: "正常", body: "B".repeat(50000), status: "success" });
	H.check("超长 body(50000) 不 500", longBody.status !== 500, `got ${longBody.status}`);

	// ---- 4/5. deviceTags 边界（需要一台 catch-all 设备来观测 matched）----
	console.log("--- deviceTags 边界 ---");
	// 建一台无 tag 设备（catch-all），用于观测 matched
	const dev = await H.req(server.base, "/v1/devices", {
		session, body: H.makeDevice({ name: "catch-all", tags: [] }),
	});
	H.eq("创建 catch-all 设备 → 200", dev.status, 200);

	const noTags = await notify({ title: "省略tags", status: "success" });
	const emptyTags = await notify({ title: "空tags", status: "success", deviceTags: [] });
	H.eq("省略 deviceTags 是广播", noTags.json?.matched, 1);
	H.eq("空 deviceTags [] 也是广播", emptyTags.json?.matched, noTags.json?.matched);

	const blankTags = await notify({ title: "空白tags", status: "success", deviceTags: ["", "   ", "\t"] });
	H.check("deviceTags 含空串/纯空格被剔除不报错", blankTags.status === 200, `got ${blankTags.status}`);
	H.eq("全空白 tags 归一化后按广播", blankTags.json?.matched, noTags.json?.matched);

	// ---- 6. 重复 endpoint 设备 Upsert ----
	console.log("--- 重复 endpoint Upsert ---");
	const sameEndpoint = "https://push.example.com/dup-" + Date.now();
	const d1 = await H.req(server.base, "/v1/devices", { session, body: H.makeDevice({ name: "设备A", endpoint: sameEndpoint }) });
	const d2 = await H.req(server.base, "/v1/devices", { session, body: H.makeDevice({ name: "设备B", endpoint: sameEndpoint }) });
	H.check("相同 endpoint 两次 POST 均 200", d1.status === 200 && d2.status === 200, `${d1.status},${d2.status}`);
	const devList = await H.req(server.base, "/v1/devices", { session });
	const dupCount = (devList.json?.devices || []).filter((d) => d.endpoint === sameEndpoint).length;
	H.eq("相同 endpoint 列表只保留一个(Upsert)", dupCount, 1);

	// ---- 7. 极大 limit 钳制 ----
	console.log("--- 极大 limit ---");
	const bigLimit = await H.req(server.base, "/v1/notifications?limit=99999", { session });
	H.check("limit=99999 不 500", bigLimit.status !== 500, `got ${bigLimit.status}`);
	H.check("limit=99999 返回数组", Array.isArray(bigLimit.json?.notifications));

	// ---- 8. 非法 sinceSeq ----
	console.log("--- 非法 sinceSeq ---");
	const negSeq = await H.req(server.base, "/v1/notifications?sinceSeq=-5", { session });
	H.check("sinceSeq=-5 不 500", negSeq.status !== 500, `got ${negSeq.status}`);
	const badSeq = await H.req(server.base, "/v1/notifications?sinceSeq=abc", { session });
	H.check("sinceSeq=abc 不 500", badSeq.status !== 500, `got ${badSeq.status}`);
	const negSeqCount = negSeq.json?.notifications?.length ?? -1;
	H.check("负数 sinceSeq 按 0 处理(返回全部)", negSeqCount === (await H.req(server.base, "/v1/notifications?limit=500", { session })).json?.notifications?.length,
		`negSeq=${negSeqCount}`);

	const passed = H.summary("edge_cases");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => { console.error(e); try { server?.stop(); } catch { /* ignore */ } process.exit(1); });
