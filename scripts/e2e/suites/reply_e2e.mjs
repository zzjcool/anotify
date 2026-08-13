#!/usr/bin/env node
/* SUITE: reply_e2e — 双向回复端到端验证
 *
 * 覆盖 case：
 *  1. 上报一条带 agentId 的 task 消息 → 拿 msgId
 *  2. 用 session Cookie 调 POST /v1/reply（replyTo=msgId, body="回复内容"）→ 200
 *  3. WS 客户端订阅 agent 路由键 → 收到 reply 消息帧（kind=reply, replyTo=msgId）
 *  4. GET /v1/notifications/{replyMsgId} 验证 payload 含原 agentId/sessionId
 *  5. 越权：回复别人的消息 → 404（当前实现不区分不存在/越权，安全合理）
 *  6. body 超长 → 400
 *  7. reply 消息 agentState=working（final 设备收不到，all 设备收到）
 */
import * as H from "../lib/harness.mjs";

const T = (ms) => new Promise((r) => setTimeout(r, ms));

function connect(base, { key, headers = {} } = {}) {
	const url = base.replace(/^http/, "ws") + "/v1/stream";
	const hdrs = Object.assign({}, headers);
	if (key) hdrs.Authorization = "Bearer " + key;
	const ws = new WebSocket(url, { headers: hdrs });
	const frames = [];
	const errors = [];
	const state = { ws, frames, errors, closed: null, opened: false };
	ws.onopen = () => { state.opened = true; };
	ws.onmessage = (ev) => {
		try { frames.push(JSON.parse(ev.data)); } catch { /* ignore */ }
	};
	ws.onerror = (e) => { errors.push(e && (e.message || String(e))); };
	ws.onclose = (e) => { state.closed = { code: e.code, reason: e.reason }; };
	state.send = (obj) => ws.send(JSON.stringify(obj));
	state.waitFrame = (pred, timeout = 4000) =>
		new Promise((resolve) => {
			const start = Date.now();
			const iv = setInterval(() => {
				const f = frames.find(pred);
				if (f) { clearInterval(iv); resolve(f); }
				else if (Date.now() - start > timeout) { clearInterval(iv); resolve(null); }
			}, 30);
		});
	return state;
}

function waitSettled(state, timeout = 4000) {
	return new Promise((resolve) => {
		const start = Date.now();
		const iv = setInterval(() => {
			if (state.opened) { clearInterval(iv); resolve("open"); }
			else if (state.closed || state.errors.length) { clearInterval(iv); resolve("closed"); }
			else if (Date.now() - start > timeout) { clearInterval(iv); resolve("timeout"); }
		}, 30);
	});
}

async function main() {
	console.log("=== SUITE: reply_e2e（双向回复端到端）===");
	const server = await H.startServer({ suiteName: "reply_e2e", rpId: "localhost" });
	const { sendKey, recvKey, session } = H.seed(server.dbPath, "reply");

	// ---- 1. 上报带 agentId 的 task 消息 ----
	const agentId = "pi@replye2e:r2d2";
	const sessionId = "sess_reply_e2e_001";
	const taskResp = await H.req(server.base, "/v1/notify", {
		key: sendKey,
		body: {
			title: "reply-e2e-task",
			agentState: "done",
			body: "任务完成",
			agentId,
			sessionId,
			cwd: "/tmp/test-project",
		},
	});
	H.eq("上报 task → 200", taskResp.status, 200);
	const taskId = taskResp.json?.id;
	H.check("task 返回 id", !!taskId, `json=${JSON.stringify(taskResp.json)}`);

	if (!taskId) {
		H.bad("无法继续：缺少 taskId");
		H.summary("reply_e2e");
		server.stop();
		process.exit(1);
	}

	// ---- 2. POST /v1/reply（Cookie 鉴权）----
	const replyResp = await H.req(server.base, "/v1/reply", {
		session,
		body: { replyTo: taskId, body: "继续改下样式" },
	});
	H.eq("reply 合法 → 200", replyResp.status, 200);
	H.check("reply 返回 id", !!replyResp.json?.id);
	H.eq("reply routed=true", replyResp.json?.routed, true);
	H.eq(
		"reply agentRoute 正确（用 sessionId 路由）",
		replyResp.json?.agentRoute,
		"agent:" + sessionId,
	);

	const replyMsgId = replyResp.json?.id;
	H.check("reply 返回 msgId", !!replyMsgId);

	// ---- 3. WS 收到 reply 消息帧 ----
	// 先连 WS（recv Key），订阅 agent 路由键
	// 注：reply 消息的 deviceTags=[agent:sess_reply_e2e_001]（后端从原消息 sessionId 构造路由键），
	// WS 客户端需要 subscribe(tags=[agent:sess_reply_e2e_001]) 才能收到。
	{
		const s = connect(server.base, { key: recvKey });
		await waitSettled(s);
		await s.waitFrame((f) => f.type === "hello");

		// 订阅 agent 路由键（用 sessionId，与后端构造逻辑一致）
		const agentRoute = "agent:" + sessionId;
		s.send({ type: "subscribe", tags: [agentRoute] });
		const subed = await s.waitFrame((f) => f.type === "subscribed", 2000);
		H.check("WS subscribe agent 路由键 → subscribed", !!subed);

		// 再发一条 reply（上一条已在连接前发布，靠 replay 补）
		const reply2 = await H.req(server.base, "/v1/reply", {
			session,
			body: { replyTo: taskId, body: "第二条回复" },
		});
		H.eq("第二条 reply → 200", reply2.status, 200);

		const replyFrame = await s.waitFrame(
			(f) => f.type === "notification" && f.kind === "reply",
			4000,
		);
		H.check("WS 收到 kind=reply 的 notification 帧", !!replyFrame);
		if (replyFrame) {
			H.eq("reply 帧 kind=reply", replyFrame.kind, "reply");
			H.eq("reply 帧 replyTo 正确", replyFrame.replyTo, taskId);
			H.eq("reply 帧 agentState=working", replyFrame.agentState, "working");
			H.check("reply 帧 body 非空", !!replyFrame.body);
		}
		s.ws.close();
		await T(300);
	}

	// ---- 4. GET /v1/notifications/{replyMsgId} 验证 payload ----
	if (replyMsgId) {
		const detailResp = await H.req(server.base, `/v1/notifications/${replyMsgId}`, { session });
		H.eq("GET reply 消息详情 → 200", detailResp.status, 200);
		const detail = detailResp.json;
		H.check("详情含 id", !!detail?.id);
		if (detail?.id) {
			H.eq("消息 kind=reply", detail.kind, "reply");
			H.eq("消息 replyTo 正确", detail.replyTo, taskId);
			H.eq("消息 agentState=working", detail.agentState, "working");
			// payload 是原始 JSON，解析后应含 agentId/sessionId
			let payload = null;
			try {
				payload = typeof detail.payload === "string"
					? JSON.parse(detail.payload)
					: detail.payload;
			} catch { /* ignore */ }
			H.check("payload 含原 agentId", payload?.agentId === agentId);
			H.check("payload 含原 sessionId", payload?.sessionId === sessionId);
			H.check("payload source=reply", payload?.source === "reply");
		}
	}

	// ---- 5. 越权：回复别人的消息 ----
	// 用另一个用户播种，该用户的消息对当前 session 不可见
	{
		const otherSeed = H.seed(server.dbPath, "other-reply-user");
		// 用 other 用户的 sendKey 上报消息
		const otherTask = await H.req(server.base, "/v1/notify", {
			key: otherSeed.sendKey,
			body: { title: "other-user-task", agentState: "done", agentId: "pi@other:x1" },
		});
		H.eq("越权: other 用户上报 → 200", otherTask.status, 200);
		const otherTaskId = otherTask.json?.id;
		if (otherTaskId) {
			// 当前 session 用户尝试回复 other 的消息 → 404（GetMessage 按 userID 查，查不到）
			const crossResp = await H.req(server.base, "/v1/reply", {
				session,
				body: { replyTo: otherTaskId, body: "越权回复" },
			});
			H.check(
				"越权回复 → 404（不区分不存在/越权，安全合理）",
				crossResp.status === 404,
				`got ${crossResp.status}`,
			);
		}
	}

	// ---- 6. body 超长 → 400 ----
	{
		const longBody = "x".repeat(2001);
		const longResp = await H.req(server.base, "/v1/reply", {
			session,
			body: { replyTo: taskId, body: longBody },
		});
		H.eq("reply body 超长(2001) → 400", longResp.status, 400);
	}

	// ---- 7. reply 消息 agentState=working：final 设备收不到 ----
	// 已在 case 3 验证 agentState=working；这里验证 WS eventScope=final 不收到 reply
	{
		const s = connect(server.base, { key: recvKey });
		await waitSettled(s);
		await s.waitFrame((f) => f.type === "hello");

		s.send({ type: "subscribe", event_scope: "final" });
		await s.waitFrame((f) => f.type === "subscribed", 2000);

		// 发一条 reply（agentState=working，非终态）
		await H.req(server.base, "/v1/reply", {
			session,
			body: { replyTo: taskId, body: "final 过滤测试" },
		});
		const replyFrame = await s.waitFrame(
			(f) => f.type === "notification" && f.kind === "reply",
			1500,
		);
		H.check(
			"eventScope=final 过滤 reply（agentState=working 非终态）→ 不收到",
			!replyFrame,
		);
		s.ws.close();
		await T(300);
	}

	const passed = H.summary("reply_e2e");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
