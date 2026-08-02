#!/usr/bin/env node
/* Anotify WebSocket 接收端集成测试
 *
 * 流程：
 *   1. 用 API Key（notify:receive scope）连 wss://.../v1/stream
 *   2. 收到 hello 帧
 *   3. 用另一个 Key（notify:send scope）POST /v1/notify 发一条通知
 *   4. WS 应收到 notification 帧，校验字段
 *   5. 发送 ack 帧
 *
 * 用法：BASE=http://localhost:8080 RECV_KEY=ant_... SEND_KEY=ant_... node scripts/ws_test.mjs
 */
const BASE = process.env.BASE || "http://localhost:8080";
const RECV_KEY = process.env.RECV_KEY || "";
const SEND_KEY = process.env.SEND_KEY || "";
if (!RECV_KEY || !SEND_KEY) {
	console.error("需要 RECV_KEY 与 SEND_KEY 环境变量");
	process.exit(2);
}

const wsUrl = BASE.replace(/^http/, "ws") + "/v1/stream";
let passed = 0, failed = 0;
const ok = (m) => { passed++; console.log("  ✅ " + m); };
const bad = (m) => { failed++; console.log("  ❌ " + m); };

async function main() {
	console.log("=== WS 接收端集成测试 ===");
	const ws = new WebSocket(wsUrl, { headers: { Authorization: `Bearer ${RECV_KEY}` } });

	const frames = [];
	let helloSeen = false, notifSeen = false;

	ws.onmessage = (ev) => {
		let f;
		try { f = JSON.parse(ev.data); } catch { return; }
		frames.push(f);
		if (f.type === "hello") { helloSeen = true; ok(`hello 帧 (protocol=${f.protocol})`); }
		if (f.type === "notification") {
			notifSeen = true;
			ok(`notification 帧 (title=${f.title})`);
			if (f.title === "ws-集成测试") ok("通知标题正确"); else bad(`标题不符: ${f.title}`);
			// 回 ack
			ws.send(JSON.stringify({ type: "ack", event_ids: [f.event_id] }));
		}
		if (f.type === "error") bad(`收到 error 帧: ${f.message}`);
	};

	ws.onerror = (e) => bad("WS 错误: " + (e.message || e));
	ws.onclose = (e) => { if (!notifSeen) bad(`连接提前关闭 code=${e.code}`); };

	await new Promise((res, rej) => {
		ws.onopen = res;
		setTimeout(() => rej(new Error("WS 连接超时")), 8000);
	}).catch((e) => { bad(e.message); });

	await new Promise((r) => setTimeout(r, 800)); // 等 hello

	// 发一条通知
	console.log("=== POST /v1/notify ===");
	const resp = await fetch(BASE + "/v1/notify", {
		method: "POST",
		headers: { "Content-Type": "application/json", Authorization: `Bearer ${SEND_KEY}` },
		body: JSON.stringify({ status: "success", title: "ws-集成测试", body: "来自 WS 集成测试" }),
	});
	if (resp.ok) ok(`notify 上报 ${resp.status}`); else bad(`notify 上报 ${resp.status}`);

	// 等 notification 帧
	await new Promise((r) => setTimeout(r, 2500));
	if (!notifSeen) bad("2.5s 内未收到 notification 帧");

	ws.close();
	console.log(`\n结果：${passed} 通过 / ${failed} 失败`);
	process.exit(failed === 0 && notifSeen ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
