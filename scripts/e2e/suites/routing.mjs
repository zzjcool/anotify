#!/usr/bin/env node
/* SUITE: routing — 投递路由矩阵（标签路由 + eventScope 过滤 + enabled 开关）
 *
 * 验证规则（与设计文档一致）：
 *   投递 ⟺ enabled ∧ scopeMatch(device.eventScope, msg.agentState) ∧ tagMatch
 *   - 消息无 deviceTags → 广播到所有 enabled 设备
 *   - 设备无 tags → catch-all 收一切（含定向消息）
 *   - 双方有 tags → 交集≥1（ANY，非 ALL）
 *   - eventScope=all 全过；=final 仅终态(done/interrupted/error)过；
 *     非终态(working/blocked) 仅 all 时过
 *
 * 设备拓扑：
 *   A: tags=["手机"], eventScope=all,   enabled=true
 *   B: tags=["工作"], eventScope=final, enabled=true
 *   C: tags=[],       eventScope=all,   enabled=true  (catch-all)
 *   D: tags=["手机"], eventScope=all,   enabled=false (禁用)
 *
 * 依赖：PATCH /v1/devices/:id 可正常设置 tags/eventScope/enabled
 *   （若该端点失效则本套件暴露产品 bug，见上报）。
 */
import * as H from "../lib/harness.mjs";

let server;
let sendKey, session;
const dev = {}; // name -> {id, ...}

// 创建一台设备（POST 用唯一 endpoint），返回 device.id
async function createDevice(name, endpointTag) {
	const d = H.makeDevice({
		name,
		endpoint: `https://push.example.com/${endpointTag}-${Math.random().toString(36).slice(2)}`,
	});
	const r = await H.req(server.base, "/v1/devices", { session, body: d });
	if (r.status !== 200)
		throw new Error(`创建设备 ${name} 失败: ${r.status} ${r.text}`);
	return r.json.device;
}

// 用 PATCH 设置设备配置
async function patchDevice(id, patch) {
	const r = await H.req(server.base, `/v1/devices/${id}`, {
		session,
		method: "PATCH",
		body: patch,
	});
	if (r.status !== 200)
		throw new Error(`PATCH 设备 ${id} 失败: ${r.status} ${r.text}`);
	return r.json.device;
}

// 上报一条消息，返回 {matched, hitIds:Set}
async function notify(body) {
	const r = await H.req(server.base, "/v1/notify", { key: sendKey, body });
	if (r.status !== 200) throw new Error(`notify 失败: ${r.status} ${r.text}`);
	const results = r.json.results || [];
	const hitIds = new Set(results.map((x) => x.device));
	return { matched: r.json.matched, hitIds, results };
}

// 断言一次上报的命中集合
function expectHits(caseName, { matched, hitIds }, expectIds) {
	const got = [...hitIds].sort();
	const want = [...expectIds].sort();
	H.eq(`${caseName} · matched 数`, matched, expectIds.length);
	H.eq(`${caseName} · 命中设备集合`, got, want);
}

async function main() {
	console.log("=== SUITE: routing（投递路由矩阵）===");
	server = await H.startServer({ suiteName: "routing", rpId: "localhost" });
	const s = H.seed(server.dbPath, "route_user");
	sendKey = s.sendKey;
	session = s.session;

	// ---- 搭建设备拓扑 ----
	// A: tags=["手机"], scope=all, enabled
	const a = await createDevice("A-手机", "a");
	await patchDevice(a.id, {
		tags: ["手机"],
		eventScope: "all",
		enabled: true,
	});
	dev.A = a.id;

	// B: tags=["工作"], scope=final, enabled
	const b = await createDevice("B-工作", "b");
	await patchDevice(b.id, {
		tags: ["工作"],
		eventScope: "final",
		enabled: true,
	});
	dev.B = b.id;

	// C: tags=[], scope=all, enabled（catch-all）
	const c = await createDevice("C-catchall", "c");
	await patchDevice(c.id, { tags: [], eventScope: "all", enabled: true });
	dev.C = c.id;

	// D: tags=["手机"], scope=all, 禁用
	const d = await createDevice("D-禁用", "d");
	await patchDevice(d.id, {
		tags: ["手机"],
		eventScope: "all",
		enabled: false,
	});
	dev.D = d.id;

	// ---- 先验证设备配置真的生效（PATCH 是否落库）----
	const listR = await H.req(server.base, "/v1/devices", { session });
	const byId = {};
	for (const x of listR.json.devices || []) byId[x.id] = x;
	H.eq(
		"A 配置生效(tags=[手机],scope=all,enabled)",
		[
			byId[dev.A]?.tags?.join(),
			byId[dev.A]?.eventScope,
			byId[dev.A]?.enabled,
		],
		["手机", "all", true],
	);
	H.eq(
		"B 配置生效(tags=[工作],scope=final,enabled)",
		[
			byId[dev.B]?.tags?.join(),
			byId[dev.B]?.eventScope,
			byId[dev.B]?.enabled,
		],
		["工作", "final", true],
	);
	H.eq(
		"C 配置生效(tags=[],scope=all,enabled)",
		[
			(byId[dev.C]?.tags || []).length,
			byId[dev.C]?.eventScope,
			byId[dev.C]?.enabled,
		],
		[0, "all", true],
	);
	H.eq("D 配置生效(enabled=false)", byId[dev.D]?.enabled, false);

	// ---- 路由断言矩阵 ----
	// 1. done 无 deviceTags → 广播；B 的 scope=final 放行(done 是终态)；D 禁用 → 命中 A,B,C
	expectHits(
		"case1 done 广播",
		await notify({ title: "c1", agentState: "done" }),
		[dev.A, dev.B, dev.C],
	);

	// 2. error 无 deviceTags → 广播；B 的 final 放行(error 是终态)；D 禁用 → 命中 A,B,C
	expectHits(
		"case2 error 广播",
		await notify({ title: "c2", agentState: "error" }),
		[dev.A, dev.B, dev.C],
	);

	// 3. error + deviceTags=["手机"] → A(tag+scope=all 过), C(catch-all)；B tag 不符 → 命中 A,C
	expectHits(
		"case3 error 定向[手机]",
		await notify({ title: "c3", agentState: "error", deviceTags: ["手机"] }),
		[dev.A, dev.C],
	);

	// 4. error + deviceTags=["工作"] → B(tag+final 过), C(catch-all)；A 不符 → 命中 B,C
	expectHits(
		"case4 error 定向[工作]",
		await notify({ title: "c4", agentState: "error", deviceTags: ["工作"] }),
		[dev.B, dev.C],
	);

	// 5. done + deviceTags=["手机","工作"] → A(tag+all 过), B(tag+final 过 done), C；D 禁用 → 命中 A,B,C
	expectHits(
		"case5 done 定向[手机,工作]",
		await notify({
			title: "c5",
			agentState: "done",
			deviceTags: ["手机", "工作"],
		}),
		[dev.A, dev.B, dev.C],
	);

	// 6. interrupted 无 deviceTags → A,C,B(final 放行 interrupted 是终态)；D 禁用 → 命中 A,B,C
	expectHits(
		"case6 interrupted 广播",
		await notify({ title: "c6", agentState: "interrupted" }),
		[dev.A, dev.B, dev.C],
	);

	// 7. done + deviceTags=["不存在"] → 仅 C(catch-all) → matched=1
	expectHits(
		"case7 done 定向[不存在]",
		await notify({ title: "c7", agentState: "done", deviceTags: ["不存在"] }),
		[dev.C],
	);

	// 8. 专项：禁用设备 D 在所有定向到其 tag 的情况下也不接收
	{
		const r = await notify({
			title: "c8",
			agentState: "done",
			deviceTags: ["手机"],
		});
		H.check("case8 禁用设备 D 永不命中（定向其 tag）", !r.hitIds.has(dev.D));
	}
	// 8b. working/blocked 广播 → 仅 all 通过（A,C），B=final 不过(非终态)
	expectHits(
		"case8b working 广播",
		await notify({ title: "c8b", agentState: "working" }),
		[dev.A, dev.C],
	);
	expectHits(
		"case8c blocked 广播",
		await notify({ title: "c8c", agentState: "blocked" }),
		[dev.A, dev.C],
	);

	const passed = H.summary("routing");
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error("套件异常:", e);
	try {
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
