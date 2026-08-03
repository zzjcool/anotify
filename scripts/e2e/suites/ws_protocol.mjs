#!/usr/bin/env node
/* SUITE: ws_protocol — WebSocket 帧协议全量验证
 *
 * 端点：GET /v1/stream（Bearer Key，scope=notify:receive）
 * 下行帧：hello / notification / replay_end / pong / subscribed / error / bye
 * 上行帧：subscribe / unsubscribe / ack / ping / resume
 *
 * 覆盖 case：
 *  1. 无 Key 连接 → 拒绝（401，握手失败）
 *  2. send scope Key（无 receive）连接 → 拒绝（403）
 *  3. recv Key 连接 → 收到 hello 帧（含 protocol/conn_id/heartbeat_sec/resume_token）
 *  4. 发 {"type":"ping"} → 收到 {"type":"pong"}
 *  5. POST /v1/notify 后发一条 → WS 收到 notification 帧（event_id/title/seq/tags 字段正确）
 *  6. 收 notification 后发 ack（resume_token 编码的 event_id）→ 无 error 帧
 *  7. 标签过滤：subscribe ["ops"] → subscribed；deviceTags=["ops"]→收到；deviceTags=["other"]→不收；无 tags 广播→收到
 *  8. 断线续传：发 2 条→断开→带 Last-Event-Id=seq1 重连→replay 收到第 2 条 + replay_end
 *  9. 未知帧类型 → error 帧（unknown_type）且连接保持
 */
import * as H from "../lib/harness.mjs";

const T = (ms) => new Promise((r) => setTimeout(r, ms));

// 连接 WS，返回 {ws, frames, errors, closed, waitFrame, send}
function connect(base, { key, headers = {} } = {}) {
	const url = base.replace(/^http/, "ws") + "/v1/stream";
	const hdrs = Object.assign({}, headers);
	if (key) hdrs.Authorization = "Bearer " + key;
	const ws = new WebSocket(url, { headers: hdrs });
	const frames = [];
	const errors = [];
	const state = { ws, frames, errors, closed: null, opened: false };
	ws.onopen = () => {
		state.opened = true;
	};
	ws.onmessage = (ev) => {
		try {
			frames.push(JSON.parse(ev.data));
		} catch {
			/* ignore */
		}
	};
	ws.onerror = (e) => {
		errors.push(e && (e.message || String(e)));
	};
	ws.onclose = (e) => {
		state.closed = { code: e.code, reason: e.reason };
	};
	state.send = (obj) => ws.send(JSON.stringify(obj));
	// 等待一个满足条件的帧（在超时内）
	state.waitFrame = (pred, timeout = 4000) =>
		new Promise((resolve) => {
			const start = Date.now();
			const iv = setInterval(() => {
				const f = frames.find(pred);
				if (f) {
					clearInterval(iv);
					resolve(f);
				} else if (Date.now() - start > timeout) {
					clearInterval(iv);
					resolve(null);
				}
			}, 30);
		});
	return state;
}

// 等待 WS 打开或失败
function waitSettled(state, timeout = 4000) {
	return new Promise((resolve) => {
		const start = Date.now();
		const iv = setInterval(() => {
			if (state.opened) {
				clearInterval(iv);
				resolve("open");
			} else if (state.closed || state.errors.length) {
				clearInterval(iv);
				resolve("closed");
			} else if (Date.now() - start > timeout) {
				clearInterval(iv);
				resolve("timeout");
			}
		}, 30);
	});
}

async function main() {
	console.log("=== SUITE: ws_protocol（WebSocket 帧协议）===");
	const server = await H.startServer({ rpId: "localhost" });
	const { sendKey, recvKey } = H.seed(server.dbPath, "e2e");

	// ---- 1. 无 Key 连接 → 拒绝 ----
	{
		const s = connect(server.base, {});
		const r = await waitSettled(s);
		H.check(
			"无 Key 连接被拒",
			r === "closed" || (s.closed && s.closed.code),
			`结果=${r} closed=${JSON.stringify(s.closed)}`,
		);
		H.check("无 Key 未收到 hello", !s.frames.some((f) => f.type === "hello"));
		s.ws.close();
	}

	// ---- 2. send scope Key（无 receive）→ 拒绝 ----
	{
		const s = connect(server.base, { key: sendKey });
		const r = await waitSettled(s);
		H.check(
			"send scope Key 连 WS 被拒(403)",
			r === "closed" || (s.closed && s.closed.code),
			`结果=${r}`,
		);
		H.check(
			"send scope Key 未收到 hello",
			!s.frames.some((f) => f.type === "hello"),
		);
		s.ws.close();
	}

	// ---- 3-6. recv Key：hello / ping-pong / notification / ack ----
	let seq1;
	{
		const s = connect(server.base, { key: recvKey });
		const r = await waitSettled(s);
		H.eq("recv Key 连接打开", r, "open");

		const hello = await s.waitFrame((f) => f.type === "hello");
		H.check("收到 hello 帧", !!hello);
		if (hello) {
			H.eq("hello.protocol=1", hello.protocol, 1);
			H.check(
				"hello 含 conn_id",
				typeof hello.conn_id === "string" && hello.conn_id.length > 0,
			);
			H.check(
				"hello 含 heartbeat_sec",
				typeof hello.heartbeat_sec === "number" && hello.heartbeat_sec > 0,
			);
			H.check(
				"hello 含 resume_token",
				typeof hello.resume_token === "string" &&
					hello.resume_token.startsWith("evt_"),
			);
		}

		// ping → pong
		s.send({ type: "ping" });
		const pong = await s.waitFrame((f) => f.type === "pong", 2000);
		H.check("ping → 收到 pong", !!pong);

		// notification
		const before = s.frames.length;
		const nr = await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws协议-实时", status: "success", body: "实时帧测试" },
		});
		H.eq("notify 上报 200", nr.status, 200);
		const notif = await s.waitFrame(
			(f) => f.type === "notification" && f.title === "ws协议-实时",
			3000,
		);
		H.check("收到实时 notification 帧", !!notif);
		if (notif) {
			H.check(
				"notification 含 event_id",
				typeof notif.event_id === "string" && notif.event_id.length > 0,
			);
			H.check(
				"notification 含 seq",
				typeof notif.seq === "number" && notif.seq > 0,
			);
			H.eq("notification.title 正确", notif.title, "ws协议-实时");
			H.eq("notification.status 正确", notif.status, "success");
			seq1 = notif.seq;
		}

		// ack（event_id 用 "evt_<seq>" 编码——服务端 parseResumeSeq 解析）
		const errBefore = s.frames.filter((f) => f.type === "error").length;
		s.send({ type: "ack", event_ids: ["evt_" + seq1] });
		await T(600);
		const errAfter = s.frames.filter((f) => f.type === "error").length;
		H.check(
			"ack 后无 error 帧",
			errAfter === errBefore,
			`error ${errBefore}→${errAfter}`,
		);
		H.check("连接保持(未断开)", s.closed === null);
		s.ws.close();
		await T(300);
	}

	// ---- 7. 标签过滤 ----
	{
		const s = connect(server.base, { key: recvKey });
		await waitSettled(s);
		await s.waitFrame((f) => f.type === "hello");

		s.send({ type: "subscribe", tags: ["ops"] });
		const subed = await s.waitFrame((f) => f.type === "subscribed", 2000);
		H.check("subscribe → 收到 subscribed", !!subed);
		if (subed) H.eq("subscribed.tags=[ops]", subed.subscribed_tags, ["ops"]);

		// 匹配标签 → 收到
		await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws-ops", status: "info", deviceTags: ["ops"] },
		});
		const got = await s.waitFrame(
			(f) => f.type === "notification" && f.title === "ws-ops",
			2500,
		);
		H.check("deviceTags=[ops] 匹配订阅 → 收到", !!got);

		// 不匹配标签 → 不收（等超时确认未到）
		await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws-other", status: "info", deviceTags: ["other"] },
		});
		const notGot = await s.waitFrame(
			(f) => f.type === "notification" && f.title === "ws-other",
			1500,
		);
		H.check("deviceTags=[other] 不匹配订阅 → 不收到", !notGot);

		// 广播（无 tags）→ 仍收到（广播发给所有订阅者）
		await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws-bcast", status: "warning" },
		});
		const bcast = await s.waitFrame(
			(f) => f.type === "notification" && f.title === "ws-bcast",
			2500,
		);
		H.check("无 tags 广播 → 仍收到", !!bcast);
		s.ws.close();
		await T(300);
	}

	// ---- 8. 断线续传（Last-Event-Id）----
	{
		// 连一次拿到 hello（拿到当前 seq 基线），发 2 条，断开
		const s1 = connect(server.base, { key: recvKey });
		await waitSettled(s1);
		await s1.waitFrame((f) => f.type === "hello");
		const n1 = await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws-续传-1", status: "success" },
		});
		const n2 = await H.req(server.base, "/v1/notify", {
			key: sendKey,
			body: { title: "ws-续传-2", status: "error" },
		});
		// 等第一条实时到达，取它的 seq 作为 Last-Event-Id
		const first = await s1.waitFrame(
			(f) => f.type === "notification" && f.title === "ws-续传-1",
			3000,
		);
		H.check("续传：第一条实时到达", !!first);
		const lastSeq = first ? first.seq : null;
		await T(500); // 让第二条也进入队列
		s1.ws.close();
		await T(400);

		// 重连带 Last-Event-Id=第一条 seq → 应 replay 出第二条 + replay_end
		const s2 = connect(server.base, {
			key: recvKey,
			headers: { "Last-Event-Id": "evt_" + lastSeq },
		});
		await waitSettled(s2);
		await s2.waitFrame((f) => f.type === "hello");
		const replayed = await s2.waitFrame(
			(f) => f.type === "notification" && f.title === "ws-续传-2",
			3000,
		);
		H.check("断线续传：replay 出漏掉的第 2 条", !!replayed);
		const replayEnd = await s2.waitFrame((f) => f.type === "replay_end", 2000);
		H.check("断线续传：收到 replay_end", !!replayEnd);
		s2.ws.close();
		await T(300);
	}

	// ---- 9. 未知帧类型 → error 帧且连接保持 ----
	{
		const s = connect(server.base, { key: recvKey });
		await waitSettled(s);
		await s.waitFrame((f) => f.type === "hello");
		s.send({ type: "bogus_type_xyz" });
		const err = await s.waitFrame((f) => f.type === "error", 2000);
		H.check("未知帧类型 → 收到 error 帧", !!err);
		if (err) H.eq("error.code=unknown_type", err.code, "unknown_type");
		H.check("未知帧后连接保持", s.closed === null);
		// 再 ping 一次确认还活着
		s.send({ type: "ping" });
		const pong = await s.waitFrame((f) => f.type === "pong", 2000);
		H.check("未知帧后连接仍可 ping/pong", !!pong);
		s.ws.close();
	}

	const passed = H.summary("ws_protocol");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
