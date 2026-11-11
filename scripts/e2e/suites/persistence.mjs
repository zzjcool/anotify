#!/usr/bin/env node
/* SUITE: persistence — 重启后 v4 资源与执行事实仍然存活
 *
 * v4 的持久化面比 v3 宽：除了事件/任务，还有**资源文档**（Capability / Source /
 * Namespace）与它们的 CAS 令牌。重启一次会同时验证三件互不相同的事：
 *
 *   ① **资源身份稳定**：metadata.uid / version / resourceVersion 都是落库事实。
 *      重启后 uid 变了 = "同一个能力被当成两个"（引用它的源、任务、部署全断）；
 *      resourceVersion 被重置 = 所有客户端手里的 CAS 令牌永久失效（467 冲突风暴）。
 *   ② **执行事实不复活**：终态任务重启后仍是终态（不能因为内存里的状态机丢了就
 *      回到 pending 再跑一遍），pending 任务仍是 pending（不伪造执行）。
 *   ③ **凭据不丢**：会话与 API Key 跨重启仍可用（否则所有机器入口都要重新发 key）。
 *
 * 与 v3 版的区别：v3 测 messages 表与 seq 连续性（模型已下线）、`tasks.out` 列
 * （v4 没有出端口的概念，出口只存在于 emissions）；现在测的是信封身份、CAS 令牌、
 * 任务相位与事件 trace 的存活。
 *
 * 断言的「强度」约定：每条都要求**具体值相等**（uid / version / resourceVersion /
 * phase / reason），不用「字段存在」这种弱判据替代。
 */
import path from "node:path";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

// 能力脚本内容（重启前后比 digest 时要一模一样，所以抽成常量）。
const RUN = {
	entrypoint: "run.sh",
	files: { "run.sh": "#!/bin/sh\nset -eu\necho persist\n" },
};

async function main() {
	console.log("=== SUITE: persistence（v4 资源与执行事实跨重启存活）===");
	H.startTimer();

	// 临时库目录走 H.mkdtempTmp 登记（#326）：/tmp 是 tmpfs，泄漏吃内存。
	// 正常路径 finally 里 removeTmpDir；崩溃/提前退出时 harness 的 exit hook 兜底。
	const tmp = H.mkdtempTmp("anotify-persist-");
	const dbPath = path.join(tmp, "persist.db");
	const extraEnv = { ANOTIFY_DB: dbPath };

	// 跨阶段要记住的事实（第二阶段逐条比回来）。
	let phase1 = null;
	let session = "";
	let sendKey = "";

	try {
		// ================= 第一次启动：造数据 =================
		let server = await H.startServer({
			suiteName: "persistence",
			portOffset: 0,
			rpId: "localhost",
			extraEnv,
		});
		const seeded = H.seed(dbPath, "persist");
		session = seeded.session;
		sendKey = seeded.sendKey;

		// 默认命名空间的对外名（拼路径）与内部 id（直查库）都要。
		const ns = await H.defaultNamespace(server.base, session);
		H.check("解析默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
		if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
		const NS = ns.slug;

		// ---------- 造资源：两个能力 + 一个源 ----------
		const target = await V.createCapability(
			server.base,
			session,
			NS,
			V.capabilityBody({ name: "persist.work", params: { type: "object" }, run: RUN }),
		);
		V.expectStatus("建目标能力 persist.work → 201", target, 201);
		const targetDoc = target.json;

		const demo = await V.createCapability(
			server.base,
			session,
			NS,
			V.capabilityBody({ name: "persist.demo", params: { type: "object" }, next: ["persist.work"], run: RUN }),
		);
		V.expectStatus("建上游能力 persist.demo → 201", demo, 201);

		const src = await V.createSource(
			server.base,
			session,
			NS,
			V.sourceBody({
				name: "persist.src",
				kind: "manual",
				config: {},
				targets: [{ capability: "persist.work" }],
			}),
		);
		V.expectStatus("建源 persist.src → 201", src, 201);
		const srcDoc = src.json;

		// ---------- 事件 → 任务（源 targets + 无节点 = 留 pending）----------
		const ev = await V.ingestEvent(server.base, sendKey, NS, {
			source: "persist.src",
			type: "persist.run",
			id: "persist-one",
			payload: {},
		});
		V.expectStatus("事件入队 → 200（Bearer API Key）", ev, 200);
		H.check("回执带 eventId", !!ev.json?.eventId, V.brief(ev));
		H.check("回执带 traceId", !!ev.json?.traceId, V.brief(ev));
		H.eq("回执建出 1 个任务", (ev.json?.tasks || []).length, 1);
		H.eq("任务能力是 persist.work", ev.json?.tasks?.[0]?.capability, "persist.work");
		H.eq("回执的能力版本是 1", ev.json?.tasks?.[0]?.version, 1);
		H.eq("没有剪枝", (ev.json?.pruned || []).length, 0);
		const pendingId = ev.json?.tasks?.[0]?.taskId;
		H.check("拿到任务 id", !!pendingId, V.brief(ev));

		// 第二个任务：用一个新 deliveryId 造，随后用 API 取消 → 终态。
		const ev2 = await V.ingestEvent(server.base, sendKey, NS, {
			source: "persist.src",
			type: "persist.run",
			id: "persist-two",
			payload: {},
		});
		V.expectStatus("第二个事件入队 → 200", ev2, 200);
		const cancelledId = ev2.json?.tasks?.[0]?.taskId;
		H.check("拿到第二个任务 id", !!cancelledId, V.brief(ev2));

		const cancel = await H.req(server.base, V.P.taskCancel(NS, cancelledId), {
			session,
			method: "POST",
		});
		V.expectStatus("pending 任务取消 → 200", cancel, 200);
		H.eq("取消后相位 = cancelled", cancel.json?.status?.phase, "cancelled");
		H.check("终态写了 finishedAt", !!cancel.json?.status?.finishedAt, JSON.stringify(cancel.json?.status));
		H.eq("取消原因 = cancelled_by_user", cancel.json?.status?.result?.error?.reason, "cancelled_by_user");

		// 直查库核对落盘（API 是派生投影，落盘才是「跨进程」的判据）。
		const rowBefore = V.readTaskRow(dbPath, cancelledId);
		H.eq("库里相位 = cancelled", rowBefore?.phase, "cancelled");
		H.check("库里 finished_at 非空", !!rowBefore?.finished_at, String(rowBefore?.finished_at));

		phase1 = {
			nsId: ns.id,
			NS,
			targetUid: targetDoc?.metadata?.uid,
			targetRv: targetDoc?.metadata?.resourceVersion,
			targetDigest: targetDoc?.status?.digest,
			srcUid: srcDoc?.metadata?.uid,
			srcRv: srcDoc?.metadata?.resourceVersion,
			evTrace: ev.json?.traceId,
			evId: ev.json?.eventId,
			pendingId,
			cancelledId,
		};
		H.check("阶段一：能力 uid 是 rv1 之外的真身份", !!phase1.targetUid, String(phase1.targetUid));
		H.check("阶段一：能力有 CAS 令牌", V.isResourceVersion(phase1.targetRv), String(phase1.targetRv));
		H.check("阶段一：能力 status.digest 是 sha256", String(phase1.targetDigest).startsWith("sha256:"), String(phase1.targetDigest));

		server.stop();
		await new Promise((r) => setTimeout(r, 300)); // 等进程真退出、释放 DB

		// ================= 第二次启动：逐条验存活 =================
		server = await H.startServer({
			suiteName: "persistence",
			portOffset: 1,
			rpId: "localhost",
			extraEnv,
		});

		// 1. 资源身份：uid / resourceVersion / version / digest 全部逐字相等
		const capAfter = await H.req(server.base, V.P.capability(NS, "persist.work"), { session });
		V.expectStatus("重启后 GET 能力 → 200", capAfter, 200);
		V.assertEnvelope("重启后的能力", capAfter.json, "Capability");
		H.eq("重启后 metadata.uid 不变（资源身份稳定）", capAfter.json?.metadata?.uid, phase1.targetUid);
		H.eq("重启后 resourceVersion 不变（旧 CAS 令牌仍然有效）", capAfter.json?.metadata?.resourceVersion, phase1.targetRv);
		H.eq("重启后 metadata.version 不变", capAfter.json?.metadata?.version, 1);
		H.eq("重启后 status.digest 不变（内容没被重算）", capAfter.json?.status?.digest, phase1.targetDigest);

		// 2. 列表与版本族仍在
		const capsAfter = await H.req(server.base, V.P.capabilities(NS), { session });
		const capNames = V.itemNames(capsAfter);
		H.check("重启后能力列表可读", Array.isArray(capNames), V.brief(capsAfter));
		H.check(
			"重启后两个能力都还在",
			(capNames || []).includes("persist.work") && (capNames || []).includes("persist.demo"),
			JSON.stringify(capNames),
		);
		const versions = await H.req(server.base, V.P.capabilityVersions(NS, "persist.work"), { session });
		const vItems = V.assertListEnvelope("重启后的版本族", versions, "CapabilityList");
		H.eq("重启后版本族仍有 1 条", vItems.length, 1);
		H.eq("版本族里的 uid 与单对象一致", vItems[0]?.metadata?.uid, phase1.targetUid);

		// 3. 源：uid / resourceVersion / targets 保留
		const srcAfter = await H.req(server.base, V.P.source(NS, "persist.src"), { session });
		V.expectStatus("重启后 GET 源 → 200", srcAfter, 200);
		H.eq("重启后源 uid 不变", srcAfter.json?.metadata?.uid, phase1.srcUid);
		H.eq("重启后源 resourceVersion 不变", srcAfter.json?.metadata?.resourceVersion, phase1.srcRv);
		H.eq("重启后源的 targets 保留", srcAfter.json?.spec?.targets?.[0]?.capability, "persist.work");

		// 4. 任务：终态不复活、pending 仍是 pending
		const cancelledAfter = await H.req(server.base, V.P.task(NS, phase1.cancelledId), { session });
		V.expectStatus("重启后 GET 终态任务 → 200", cancelledAfter, 200);
		V.assertEnvelope("重启后的任务", cancelledAfter.json, "Task");
		H.eq("重启后终态仍是 cancelled（状态机不复活）", cancelledAfter.json?.status?.phase, "cancelled");
		H.eq("重启后终态原因仍在", cancelledAfter.json?.status?.result?.error?.reason, "cancelled_by_user");
		H.check("重启后终态 finishedAt 仍在", !!cancelledAfter.json?.status?.finishedAt, JSON.stringify(cancelledAfter.json?.status));

		const pendingAfter = await H.req(server.base, V.P.task(NS, phase1.pendingId), { session });
		V.expectStatus("重启后 GET pending 任务 → 200", pendingAfter, 200);
		H.eq("重启后 pending 仍 pending（不伪造执行）", pendingAfter.json?.status?.phase, "pending");
		H.eq("重启后 attempt 仍是 0（没有授予过执行权）", pendingAfter.json?.status?.attempt, 0);

		// 5. 任务 spec 形状：钉住能力版本 + digest，且**没有** v3 的 out 字段
		H.eq("任务 spec 钉住能力名", pendingAfter.json?.spec?.capability?.name, "persist.work");
		H.eq("任务 spec 钉住版本", pendingAfter.json?.spec?.capability?.version, 1);
		H.check(
			"任务 spec 钉住 digest（double pin）",
			String(pendingAfter.json?.spec?.capability?.digest || "").startsWith("sha256:"),
			JSON.stringify(pendingAfter.json?.spec?.capability),
		);
		H.check(
			"任务 spec 没有 out 字段（v4 出口只在 emissions）",
			!!pendingAfter.json?.spec && !("out" in pendingAfter.json.spec),
			JSON.stringify(Object.keys(pendingAfter.json?.spec || {})),
		);

		// 6. 事件跨重启可按 trace 查回（资源列表 + 会话 + ns 路径）
		const evList = await H.req(server.base, V.P.events(NS) + `?trace=${encodeURIComponent(phase1.evTrace)}`, {
			session,
		});
		const evItems = V.assertListEnvelope("重启后的事件列表", evList, "EventList");
		H.eq("重启后同一 trace 仍有 1 条事件", evItems.length, 1);
		H.eq("重启后事件 id 一致", evItems[0]?.metadata?.uid, phase1.evId);
		H.eq("重启后事件 traceUID 一致", evItems[0]?.spec?.traceUID, phase1.evTrace);

		// 7. 任务列表按 trace 过滤仍然工作（不是「全量返回碰巧包含」）
		const taskList = await H.req(server.base, V.P.tasks(NS) + `?traceId=${encodeURIComponent(phase1.evTrace)}`, {
			session,
		});
		const taskItems = V.assertListEnvelope("重启后的任务列表", taskList, "TaskList");
		H.eq("重启后按 trace 过滤任务 → 1 条", taskItems.length, 1);
		H.eq("过滤到的是那条 pending 任务", taskItems[0]?.metadata?.name, phase1.pendingId);

		// 8. 凭据跨重启：会话与 API Key 都还在
		const me = await H.req(server.base, "/v1/auth/me", { session });
		V.expectStatus("重启后会话仍有效", me, 200);
		const ev3 = await V.ingestEvent(server.base, sendKey, NS, {
			source: "persist.src",
			type: "persist.after-restart",
			id: "persist-three",
			payload: {},
		});
		V.expectStatus("重启后 API Key 仍能投递事件", ev3, 200);
		H.check("重启后的新事件有新 traceId", ev3.json?.traceId !== phase1.evTrace, String(ev3.json?.traceId));
		H.eq("重启后新事件同样建出 1 个任务", (ev3.json?.tasks || []).length, 1);

		// 9. 终态不可再停：409（不是 5xx，也不假装成功）
		const recancel = await H.req(server.base, V.P.taskCancel(NS, phase1.cancelledId), {
			session,
			method: "POST",
		});
		V.expectReason("重启后取消已终态任务 → 409", recancel, 409, "Conflict");
		H.check(
			"409 的 message 点名当前相位",
			String(recancel.json?.message || "").includes("cancelled"),
			String(recancel.json?.message || recancel.text).slice(0, 160),
		);

		// 10. 不存在的任务 → 404（与「不属于本 ns」同一个回答）
		const missing = await H.req(server.base, V.P.task(NS, "tsk_does_not_exist"), { session });
		V.expectStatus("不存在的任务 → 404", missing, 404);

		server.stop();
	} finally {
		H.removeTmpDir(tmp);
	}

	const allPassed = H.summary("persistence");
	H.writeResults("persistence", H.stopTimer());
	process.exit(allPassed ? 0 : 1);
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	process.exit(1);
});
