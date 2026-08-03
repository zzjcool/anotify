#!/usr/bin/env node
/* SUITE: persistence — 重启持久化
 *
 * 覆盖 case：
 *  1. 上报 3 条通知 + 创建设备 + 创建 Key 后，重启服务（同一 DB 文件）
 *  2. 重启后消息仍在（Replay 到 3 条）
 *  3. 重启后设备仍在
 *  4. 重启后 API Key 仍可用（上报 200）
 *  5. 重启后 seq 连续（新消息 seq=4，不从 1 重来）
 *  6. 重启后会话仍有效（session 可访问受保护 API）
 *
 * 实现要点：harness startServer 的 stop() 会删临时目录，这里自建固定 DB 目录，
 * 两次 startServer 传 extraEnv.ANOTIFY_DB 指向同一文件，进程级 kill 后重启。
 */
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import * as H from "../lib/harness.mjs";

async function main() {
	console.log("=== SUITE: persistence（重启持久化）===");
	// 自建固定 DB 目录（不用 harness 的 stop 删目录行为）
	const tmp = mkdtempSync(path.join(tmpdir(), "anotify-persist-"));
	const dbPath = path.join(tmp, "persist.db");
	const extraEnv = { ANOTIFY_DB: dbPath };

	let passed = false;
	try {
		// ---- 第一次启动：造数据 ----
		let server = await H.startServer({ rpId: "localhost", extraEnv });
		const { sendKey, session } = H.seed(dbPath, "persist");

		// 上报 3 条通知
		for (let i = 1; i <= 3; i++) {
			const r = await H.req(server.base, "/v1/notify", {
				key: sendKey,
				body: { title: `持久化消息${i}`, status: "success", body: `body-${i}` },
			});
			H.eq(`重启前上报第${i}条 → 200`, r.status, 200);
		}

		// 创建设备
		const dev = H.makeDevice({ name: "持久化设备" });
		const devResp = await H.req(server.base, "/v1/devices", {
			session,
			body: dev,
		});
		H.eq("重启前创建设备 → 200", devResp.status, 200);
		const devId = devResp.json?.device?.id;
		H.check("设备返回 id", !!devId);

		// 创建 Key（额外一个，验证 Key 持久化）
		const keyResp = await H.req(server.base, "/v1/keys", {
			session,
			body: { name: "persist-key", scopes: ["notify:send"] },
		});
		H.eq("重启前创建 Key → 200", keyResp.status, 200);
		const extraKey = keyResp.json?.key;

		// 记录重启前 seq（注：broker.Message 契约已统一 camelCase）
		const beforeList = await H.req(server.base, "/v1/notifications?limit=50", {
			session,
		});
		H.eq("重启前消息数=3", beforeList.json?.count, 3);
		const maxSeqBefore = Math.max(
			...beforeList.json.notifications.map((m) => m.seq),
		);

		// 停服（kill 进程，不删 DB）——直接再 startServer 前先 stop 旧进程但保留文件
		// harness 的 stop 会 rm tmp 目录（它自己的 tmp），我们的 DB 在自建 tmp，不受影响
		server.stop();

		// ---- 第二次启动：同一 DB ----
		server = await H.startServer({ rpId: "localhost", extraEnv });

		// 2. 消息仍在
		const afterList = await H.req(server.base, "/v1/notifications?limit=50", {
			session,
		});
		H.eq("重启后消息仍在（count=3）", afterList.json?.count, 3);
		const titles = afterList.json.notifications.map((m) => m.title).sort();
		H.eq("重启后消息内容一致", titles, [
			"持久化消息1",
			"持久化消息2",
			"持久化消息3",
		]);

		// 3. 设备仍在
		const devList = await H.req(server.base, "/v1/devices", { session });
		const found = (devList.json?.devices || []).find((d) => d.id === devId);
		H.check("重启后设备仍在", !!found);
		H.eq("设备名称一致", found?.name, "持久化设备");

		// 4. Key 仍可用
		const reuse = await H.req(server.base, "/v1/notify", {
			key: extraKey,
			body: { title: "重启后上报", status: "success" },
		});
		H.eq("重启后旧 Key 上报 → 200", reuse.status, 200);

		// 5. seq 连续（新消息 seq = maxSeqBefore+1）
		const finalList = await H.req(server.base, "/v1/notifications?limit=50", {
			session,
		});
		const newMsg = finalList.json.notifications.find(
			(m) => m.title === "重启后上报",
		);
		H.check("重启后新消息存在", !!newMsg);
		H.eq("seq 连续（接续不重置）", newMsg?.seq, maxSeqBefore + 1);

		// 6. 会话仍有效
		H.eq(
			"重启后会话仍有效",
			(await H.req(server.base, "/v1/notifications", { session })).status,
			200,
		);

		passed = H.summary("persistence");
		server.stop();
	} finally {
		rmSync(tmp, { recursive: true, force: true });
	}
	process.exit(passed ? 0 : 1);
}
main().catch((e) => {
	console.error(e);
	process.exit(1);
});
