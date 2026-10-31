#!/usr/bin/env node
/* SUITE: capacity — 容量与背压（#229 事件保留期 / #228 派发背压）
 *
 * 这两条是同一个问题的两个面：
 *   - 写入速率长期高于消费速率 → 任务积压无上限（#228）→ 靠**拒绝新事件**兜底
 *   - 事件只进不出 → 磁盘线性增长（#229）→ 靠**保留期**兜底
 *
 * 本套件只验「入口行为与库内事实」，不量性能（性能在 scripts/perf/c 里量）。
 *
 * 覆盖：
 *  1. 背压默认值下、库里没有积压 → 新事件照常 200（默认值不能误伤正常部署）
 *  2. `ANOTIFY_PENDING_MAX_AGE_SEC=1` + 一条很老的 pending → POST /v1/events = 503
 *     · body 是机器可读的 {error,pending_age_sec,max_pending_age_sec,retry_after_sec}
 *     · 带 `Retry-After` 头
 *     · `Cache-Control: no-store`（503 不能被缓存成常态）
 *  3. 503 期间**已在库里的任务一条都不少**（拒绝的是新事件，不是丢任务）
 *  4. 503 期间**读端点仍然可用**（GET /v1/events 200）——否则运维看不见发生了什么
 *  5. **webhook 入口**同样被拒（503），不是只有 /v1/events 挡
 *  6. 积压清掉后自动恢复 200（保护是自愈的，不是单向开关）
 *  7. `ANOTIFY_PENDING_MAX_AGE_SEC=0` → 不启用：同样一条老 pending，POST 仍 200
 *  8. `ANOTIFY_EVENT_RETENTION_DAYS=1` → 40 天前的 events 被清掉、刚写的留着
 *  9. `ANOTIFY_EVENT_RETENTION_DAYS=0` → 永久保留：同样 40 天前的事件**不**被删，
 *     且启动日志明确写了「磁盘会持续增长」
 */
import { copyFileSync, existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";
import { bringUpNode } from "../lib/nodes.mjs";

// 库路径一律用 server.dbPath（harness 自己建的临时库，stop() 会连目录一起收掉）。
// **不要**另外指定 ANOTIFY_DB：那样 server.dbPath 指向的是一个服务从未打开过的空库，
// 直插的行会写进另一份文件，症状是「造了数据但服务看不见」。

// ---------- 直接读写库的小工具（跨进程核对落盘事实最可靠） ----------
function withDB(dbPath, fn) {
	const db = new DatabaseSync(dbPath);
	try {
		return fn(db);
	} finally {
		db.close();
	}
}

// 取默认命名空间的 id。走契约端点（不猜库结构），需要 session。
async function nsID(server, session) {
	return H.defaultNamespaceId(server.base, session);
}

// 建一个**自定义**能力，返回 (id, name)。
//
// 为什么不能用内置能力（如 notify.phone）造 pending：内置能力是「中枢执行」的，
// `store.ClaimCentralTasks` 会把它们从 pending 领走并推进状态机（实测 30s 内就变成
// running/failed），于是「造一条等了 600 秒的 pending」根本留不住。自定义能力没有执行者，
// 又没有任何节点声明它 → 派发只会失败后继续留在 pending（这才是真实的「派不出去」）。
async function mkCustomCap(server, session, ns, name) {
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: { namespaceId: ns, name, paramsSchema: { type: "object" } },
	});
	const id = r.json?.capability?.id;
	if (!id) throw new Error(`建能力 ${name} 失败: ${r.status} ${r.text}`);
	// version 也要带出来：bringUpNode 的 needCapList 会校它是正整数（契约 §6.1），
	// hello 帧里缺 version 会被后端拒握手 —— 症状是「节点起来但永远不被派发」。
	return { id, name, version: r.json?.capability?.version ?? 1 };
}

// 在库里凭空造一条「很老的 pending 任务」。
//
// 为什么要直插而不是走 API：真实的 pending 积压需要「有节点声明了该能力但槽位占满」，
// 那要起真 node + 卡住它，套件会变成 flaky 的时序测试。这里要验的是**背压的判定与
// 响应**，不是派发本身——直接造出「库里有一条等了 N 秒的 pending」这个状态最确定。
function seedOldPendingTask(dbPath, namespaceId, ageSec, cap) {
	return withDB(dbPath, (db) => {
		const nowSec = Math.floor(Date.now() / 1000);
		const id = `tsk_e2e${Math.random().toString(36).slice(2, 10)}`;
		db.prepare(
			`INSERT INTO tasks (id, namespace_id, trace_id, path, capability_id, capability, version,
			                    params, status, cap_hash, graph_id, created_at)
			 VALUES (?, ?, ?, '[]', ?, ?, 1, '{}', 'pending', 'e2e', 'grf_e2e', ?)`,
		).run(id, namespaceId, `trace_${id}`, cap.id, cap.name, nowSec - ageSec);
		return id;
	});
}

function countTasks(dbPath) {
	return withDB(dbPath, (db) => db.prepare(`SELECT count(*) AS n FROM tasks`).get().n);
}

// 读一条任务的关键列（跨进程核对「还在被推进」）。
function readTask(dbPath, id) {
	return withDB(dbPath, (db) =>
		db.prepare(`SELECT id, status, node_id, not_before FROM tasks WHERE id = ?`).get(id),
	);
}

// 数库里 events 的行数（跨进程核对清理与幂等）。
function countEvents(dbPath) {
	return withDB(dbPath, (db) => db.prepare(`SELECT count(*) AS n FROM events`).get().n);
}

// 主库文件的字节数。**先 checkpoint 再读**：auto_vacuum=0 + WAL，不回写的话
// 读到的是「主库 + 未回写 WAL」的组合，同一个逻辑状态每次都能差出几倍。
function dbFileBytes(dbPath) {
	withDB(dbPath, (db) => db.prepare(`PRAGMA wal_checkpoint(TRUNCATE)`).get());
	return statSync(dbPath).size;
}

// 日志里是否出现过「真的删了行」的清理轮次（events_deleted > 0）。
//
// 用来断言「第二轮清理没有重复删」——注意 `lease.sweeper.retention` 只在
// 「删了东西或被截断」时才打，所以同一份库的第二轮**一条都不该有**。
function hasPositiveDeletion(logText) {
	for (const line of logText.split("\n")) {
		if (!line.includes("lease.sweeper.retention")) continue;
		const m = /"events_deleted":(\d+)/.exec(line);
		if (m && Number(m[1]) > 0) return true;
	}
	return false;
}

// 塞 events：一条「40 天前」的 + 一条「刚刚」的，返回两者的 id。
function seedOldEvents(dbPath, namespaceId, count = 3) {
	return withDB(dbPath, (db) => {
		const nowSec = Math.floor(Date.now() / 1000);
		const fortyDays = 40 * 86400;
		const stmt = db.prepare(
			`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
			 VALUES (?, ?, ?, 'e2e', 'capacity.old', '{}', NULL, ?)`,
		);
		const oldIds = [];
		for (let i = 0; i < count; i++) {
			const id = `evt_old${i}_${Math.random().toString(36).slice(2, 8)}`;
			stmt.run(id, namespaceId, `trace_${id}`, nowSec - fortyDays);
			oldIds.push(id);
		}
		const freshId = `evt_fresh_${Math.random().toString(36).slice(2, 8)}`;
		stmt.run(freshId, namespaceId, `trace_${freshId}`, nowSec);
		return { oldIds, freshId };
	});
}

function eventExists(dbPath, id) {
	return withDB(dbPath, (db) => !!db.prepare(`SELECT 1 AS x FROM events WHERE id = ?`).get(id));
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 等某个条件成立（默认最多 8s）。返回是否成功。
async function waitFor(fn, { timeoutMs = 8000, everyMs = 100 } = {}) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		if (await fn()) return true;
		if (Date.now() > deadline) return false;
		await sleep(everyMs);
	}
}

// ---------- 场景 1-7：背压 ----------

// runBackpressure 用给定的 ANOTIFY_PENDING_MAX_AGE_SEC 起一套环境，跑完整条
// 「积压 → 拒绝 → 恢复」链路。maxAgeSec=0 时跳过拒绝相关断言（那条是「不启用」）。
async function runBackpressure(enabled) {
	const maxAgeSec = enabled ? 1 : 0;
	const tag = enabled ? "启用(1s)" : "关闭(0)";
	console.log(`--- 背压 ${tag} ---`);

	let server;
	try {
		server = await H.startServer({
			suiteName: "capacity",
			rpId: "localhost",
			extraEnv: {
				ANOTIFY_PENDING_MAX_AGE_SEC: String(maxAgeSec),
			},
		});
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "capbp");
		const ns = await nsID(server, s.session);

		// 建一个 webhook 源：用来验「webhook 入口同样被拒」。
		// 出口指向内置能力（存在即可），参数留空。
		const cap = withDB(dbPath, (db) =>
			db
				.prepare(`SELECT id FROM capability_versions WHERE namespace_id = ? ORDER BY name LIMIT 1`)
				.get(ns),
		);
		const srcRes = await H.req(server.base, "/v1/sources", {
			session: s.session,
			body: {
				namespaceId: ns,
				name: "cap-hook",
				kind: "webhook",
				config: {},
				targets: [{ capabilityId: cap.id }],
			},
		});
		const hookURL = srcRes.json?.source?.hookUrl || "";
		const srcID = srcRes.json?.source?.id || "";
		H.check("建 webhook 源", srcRes.status === 201 && hookURL.startsWith("/v1/hooks/"), JSON.stringify(srcRes.json));

		// 基线：库里没有积压 → 新事件必须 200（默认值不许误伤正常部署）
		const baseline = await H.req(server.base, "/v1/events", {
			key: s.sendKey,
			body: { source: "github", type: "cap.baseline", payload: { n: 0 } },
		});
		H.check("无积压时 POST /v1/events = 200", baseline.status === 200, `status=${baseline.status} ${baseline.text}`);

		// 造一条「等了 600 秒」的 pending（必须是**自定义**能力，理由见 mkCustomCap）
		const pendingCap = await mkCustomCap(server, s.session, ns, `cap.pending.${maxAgeSec}`);
		const pendingID = seedOldPendingTask(dbPath, ns, 600, pendingCap);
		const tasksBefore = countTasks(dbPath);

		const rejected = await H.req(server.base, "/v1/events", {
			key: s.sendKey,
			body: { source: "github", type: "cap.backpressure", payload: { n: 1 } },
		});

		if (!enabled) {
			// 阈值 0 = 不启用：无论积压多老都放行
			H.check("阈值 0 时不启用：老积压存在也仍 200", rejected.status === 200, `status=${rejected.status} ${rejected.text}`);
			H.eq("阈值 0 时任务数不变", countTasks(dbPath), tasksBefore);
			return;
		}

		// 采样是后台跑的，且「远离阈值」时是 5s 一档（自适应降采样，见契约 §1.2 第 8 条）：
		// 造出积压后最多要等一个空闲采样周期才会切到超阈。所以这里**轮询到超阈**，
		// 而不是 sleep 一个拍脑袋的数——前者在任何机器速度下都稳，后者必然变成偶发红。
		let over = rejected;
		if (over.status !== 503) {
			await waitFor(async () => {
				over = await H.req(server.base, "/v1/events", {
					key: s.sendKey,
					body: { source: "github", type: "cap.backpressure.poll", payload: { n: 2 } },
				});
				return over.status === 503;
			}, { timeoutMs: 12000, everyMs: 300 });
		}

		H.eq("超阈时 POST /v1/events = 503", over.status, 503);
		H.eq("503 body.error = backpressure", over.json?.error, "backpressure");
		H.eq("503 body.max_pending_age_sec", over.json?.max_pending_age_sec, maxAgeSec);
		H.check(
			"503 body.pending_age_sec ≥ 600（就是那条老 pending 的年龄）",
			typeof over.json?.pending_age_sec === "number" && over.json.pending_age_sec >= 600,
			String(over.json?.pending_age_sec),
		);
		H.check(
			"503 body.retry_after_sec 是正数",
			typeof over.json?.retry_after_sec === "number" && over.json.retry_after_sec > 0,
			String(over.json?.retry_after_sec),
		);
		H.check("503 带 Retry-After 头", !!over.headers["retry-after"], JSON.stringify(over.headers));
		H.eq("503 的 Content-Type 是 JSON", (over.headers["content-type"] || "").split(";")[0], "application/json");
		H.eq("503 不可缓存", over.headers["cache-control"], "no-store");

		// 已在库里的任务一条都不许少（拒绝的是新事件，不是丢任务）
		H.eq("503 期间任务行数不变", countTasks(dbPath), tasksBefore);

		// webhook 入口同样被拒
		const hookRes = await H.req(server.base, hookURL, {
			body: { type: "cap.hook", payload: { n: 2 } },
		});
		H.eq("超阈时 webhook 入口 = 503", hookRes.status, 503);

		// 手动触发入口同样被拒（契约 §1.4 冻结的**第三个**新事件入口）。
		//
		// 为什么这条必须单独验：单测覆盖的是 `Wrap` 本身，证明不了「这条路由挂上了它」；
		// 而 fire 是三条里唯一带顺序约束的（sessMW 在外、bp 在内，见 mux.go 注释）——
		// 把 bp 挪到 sessMW 外面会让**未登录**请求收到 503，而当时全套测试仍然全绿。
		const fireRes = await H.req(server.base, `/v1/sources/${srcID}/fire`, {
			session: s.session,
			body: { namespaceId: ns, type: "cap.fire", payload: { n: 9 } },
		});
		H.eq("超阈时 POST /v1/sources/{id}/fire = 503", fireRes.status, 503);

		// 未登录先 401：不许从 503/200 的差别里读出「中枢现在积压了吗」。
		const fireAnon = await H.req(server.base, `/v1/sources/${srcID}/fire`, {
			body: { namespaceId: ns, type: "cap.fire" },
		});
		H.eq("未登录时 /fire 是 401（不是 503）", fireAnon.status, 401);

		// ---- 「不丢任务」的**正向**证据：503 当口，库里已有的 pending 仍在被派发 ----
		//
		// 只断言「行数没变」是不够的：那只证明**没被删**，不证明**还在被推进**。
		// 这里造两条 600s 的 pending：
		//   · backlogCap（没有节点声明）→ 永远派不出去，正是它把信号顶在阈值之上；
		//   · liveCap（有真节点声明）    → 应当照常被重派/占用，即使入口在 503。
		// 节点不做 ack：只要能观察到 status 离开 pending 且 node_id 被写上，就证明
		// 「中枢仍在派发」，与节点后面 ack 与否无关（租约到期会自己回收）。
		const liveCap = await mkCustomCap(server, s.session, ns, `cap.live.${maxAgeSec}`);
		const liveNode = await bringUpNode(server.base, dbPath, ns, "cap-live-node", [liveCap], 1);
		const liveTaskID = seedOldPendingTask(dbPath, ns, 600, liveCap);
		const tasksWithLive = countTasks(dbPath);

		const dispatched = await waitFor(() => {
			const row = readTask(dbPath, liveTaskID);
			return !!row && row.status !== "pending" && !!row.node_id;
		}, { timeoutMs: 20000, everyMs: 200 });

		// 同一时刻入口必须还在 503（证明「派发在跑」与「入口被拒」共存，不是顺序巧合）
		const stillRejecting = await H.req(server.base, "/v1/events", {
			key: s.sendKey,
			body: { source: "github", type: "cap.still-rejecting", payload: { n: 4 } },
		});

		H.check(
			"503 期间已有 pending 仍被派发（status 离开 pending 且写上 node_id）",
			dispatched,
			JSON.stringify(readTask(dbPath, liveTaskID)),
		);
		H.eq("派发发生时入口仍然 503（两件事并存，不是先后）", stillRejecting.status, 503);

		const liveRow = readTask(dbPath, liveTaskID);
		H.check("派发后任务行还在（没被丢弃）", !!liveRow, "行消失了");
		H.eq("派发后任务没有被删，仍在库", countTasks(dbPath), tasksWithLive);
		// 不可派发的那条必须原地不动（它没被「清理」也没有被误删）
		const backlogRow = readTask(dbPath, pendingID);
		H.eq("不可派发的那条仍在 pending（背压原因仍然成立）", backlogRow?.status, "pending");
		liveNode?.conn?.close();

		// 读端点不受影响：背压只挡写入。三个入口都拒了，读必须全通 ——
		// 否则运维在事故现场连「发生了什么」都查不了。
		const readChecks = [
			["GET /v1/events（机器入口）", "/v1/events", { key: s.sendKey }],
			["GET /v1/console/events（控制台事件流）", "/v1/console/events", { session: s.session }],
			["GET /v1/tasks（任务列表）", "/v1/tasks", { session: s.session }],
		];
		for (const [label, path, opts] of readChecks) {
			const r = await H.req(server.base, path, opts);
			H.check(`503 期间 ${label} 仍 200`, r.status === 200, `status=${r.status} ${r.text.slice(0, 200)}`);
		}

		// 积压清掉 → 自动恢复（保护必须自愈，否则一次积压就是永久停机）。
		// 清的是**这一轮造出来的所有**老 pending：除了不可派发的 pendingID，
		// 还有上面那条被派发过、但节点没 ack 又回到 pending 的 liveTaskID
		// （它的 created_at 也是 600s 前，不清掉信号永远不会回落）。
		withDB(dbPath, (db) => {
			const stmt = db.prepare(`UPDATE tasks SET status = 'succeeded', finished_at = ? WHERE id = ?`);
			for (const id of [pendingID, liveTaskID]) stmt.run(Math.floor(Date.now() / 1000), id);
		});
		const recovered = await waitFor(async () => {
			const r = await H.req(server.base, "/v1/events", {
				key: s.sendKey,
				body: { source: "github", type: "cap.recovered", payload: { n: 3 } },
			});
			return r.status === 200;
		}, { timeoutMs: 12000, everyMs: 250 });
		H.check("积压清掉后 12s 内自动恢复 200", recovered, "仍返回 503");

		// 日志可观测性：至少出现过一次 engaged（#228 的「积压不可观测」）
		H.check(
			"服务日志里有 server.backpressure.engaged",
			server.log().includes("server.backpressure.engaged"),
			server.log().slice(-800),
		);
		H.check(
			"服务日志里有 server.backpressure.released",
			server.log().includes("server.backpressure.released"),
			server.log().slice(-800),
		);
	} finally {
		server?.stop();
	}
}

// ---------- 场景 8-9：保留期 ----------

// runRetention 用给定的 ANOTIFY_EVENT_RETENTION_DAYS 起一套环境。
//
// 清理挂在 5s 的租约巡检 tick 上（首轮 Tick 就真的执行），所以这里等 ~8s。
// 不用更短的等待：读 server.log 断言「确实跑过一轮」比抢时间更稳。
async function runRetention(days) {
	const tag = days > 0 ? `${days} 天` : "0（永久保留）";
	console.log(`--- 保留期 ${tag} ---`);

	let server;
	try {
		server = await H.startServer({
			suiteName: "capacity",
			rpId: "localhost",
			extraEnv: {
				ANOTIFY_EVENT_RETENTION_DAYS: String(days),
			},
		});
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "capret");
		const ns = await nsID(server, s.session);
		const { oldIds, freshId } = seedOldEvents(dbPath, ns, 3);

		const swept = await waitFor(() => !eventExists(dbPath, oldIds[0]), { timeoutMs: 12000, everyMs: 250 });

		if (days > 0) {
			H.check("过期事件被清理（40 天前）", swept, "12s 内仍存在");
			for (let i = 0; i < oldIds.length; i++) {
				H.check(`过期事件 #${i} 已删除`, !eventExists(dbPath, oldIds[i]));
			}
			H.check("窗口内的事件（刚刚写入）保留", eventExists(dbPath, freshId));
			H.check(
				"服务日志有 lease.sweeper.retention（删了多少）",
				server.log().includes("lease.sweeper.retention"),
				server.log().slice(-1200),
			);
			H.check(
				"清理日志带 events_deleted / elapsed_ms / events_retention_sec",
				/events_deleted/.test(server.log()) &&
					/elapsed_ms/.test(server.log()) &&
					/events_retention_sec/.test(server.log()),
				server.log().slice(-1200),
			);
			H.check("没有 retention_failed", !server.log().includes("lease.sweeper.retention_failed"), server.log().slice(-1200));

			// ---- 幂等 / 可重入：同一份库再起一次服务，第二轮必须「删 0 条、不报错」 ----
			//
			// 为什么这条必须是 e2e：单测只能证明「store 层对空集返回 0」，证明不了
			// 「真实进程重启后第一轮 tick 不会因为上次删到一半而重复删/报错」。
			// 分批 LIMIT 的截止条件、`truncated` 的交接、以及「本轮没有可删的就不打日志」
			// 这三件事只在真实进程里连起来。
			// 先把库搬到 harness 临时目录**之外**再重启。两个必须踩对的点：
			//  1. `server.stop()` 会把 mkdtemp 出来的整个目录 rmSync 掉 —— 直接拿原
			//     dbPath 重启会得到 `unable to open database file: out of memory (14)`
			//     （库根本不存在了）。所以先搬出去。
			//  0. 先证明库**静默**再拷：拷贝理论上会和「后台正在落页」撞上（撕裂读）。
			//     这里把假设变成断言 —— 隔 300ms 读两次事件行数必须相同。
			//     （此时保留期的下一轮在 1 小时之后，且没有任何客户端流量打这个库。）
			const rowsT0 = countEvents(dbPath);
			await new Promise((r) => setTimeout(r, 300));
			H.eq("幂等段前置：拷贝前库是静默的（300ms 内无写入）", countEvents(dbPath), rowsT0);
			//  2. 但**不能等 stop 之后再拷**：stop 会把整个目录删掉（ENOENT）。所以是
			//     「先拷、后 stop」。拷贝必须在 stop 之前完成，因此主库与 -wal 一起拷
			//     （-shm 是内存索引，新进程会自己重建，不拷）：WAL 自带校验和，
			//     即使拷到一帧写到一半的尾部，SQLite 也会丢掉它而不是读出错数据。
			//     拷完再 stop，然后对新库做一次 checkpoint 把 WAL 回写进主库。
			const beforeRows = countEvents(dbPath);
			const reuseDir = mkdtempSync(join(tmpdir(), "anotify-cap-reuse-"));
			const reuseDB = join(reuseDir, "test.db");
			copyFileSync(dbPath, reuseDB);
			if (existsSync(`${dbPath}-wal`)) copyFileSync(`${dbPath}-wal`, `${reuseDB}-wal`);
			server.stop();
			const beforeBytes = dbFileBytes(reuseDB);
			const again = await H.startServer({
				suiteName: "capacity",
				rpId: "localhost",
				extraEnv: {
					ANOTIFY_EVENT_RETENTION_DAYS: String(days),
					ANOTIFY_DB: reuseDB,
				},
			});
			try {
				// 给第二轮留足一个 5s tick + 多删一次的余量
				await new Promise((r) => setTimeout(r, 8000));
				const round2 = again.log();
				H.check(
					"第二轮清理：没有失败",
					!round2.includes("lease.sweeper.retention_failed"),
					round2.slice(-1200),
				);
				H.check(
					"第二轮清理：没有再删任何行（应该是「无需清理」而不是重复删）",
					!hasPositiveDeletion(round2),
					round2.slice(-1200),
				);
				H.eq("第二轮清理：过期事件仍然不存在（幂等）", eventExists(reuseDB, oldIds[0]), false);
				H.eq("第二轮清理：窗口内事件仍在", eventExists(reuseDB, freshId), true);
				H.eq("第二轮清理：事件行数不变", countEvents(reuseDB), beforeRows);
				H.check(
					"第二轮清理：文件没有被再次撑大（auto_vacuum=0 下不应变大）",
					dbFileBytes(reuseDB) <= beforeBytes + 65536,
					`before=${beforeBytes} after=${dbFileBytes(reuseDB)}`,
				);
			} finally {
				again.stop();
				rmSync(reuseDir, { recursive: true, force: true });
			}
		} else {
			H.check("永久保留：40 天前的事件不被删", !swept, "被清理了（0 应该是永久保留）");
			H.check("永久保留：窗口内的事件也在", eventExists(dbPath, freshId));
			// 0 = 永久保留必须**明确告知磁盘会持续增长**（不能静默）
			H.check(
				"启动日志有 server.retention.permanent 告警",
				server.log().includes("server.retention.permanent"),
				server.log().slice(-1200),
			);
		}
	} finally {
		server?.stop();
	}
}

async function main() {
	console.log("=== SUITE: capacity（#229 保留期 / #228 背压）===");
	H.startTimer();

	await runBackpressure(true);
	await runBackpressure(false);
	await runRetention(1);
	await runRetention(0);

	const okAll = H.summary("capacity");
	if (!okAll) process.exitCode = 1;
}

main().catch((e) => {
	console.error("SUITE: capacity 崩溃：", e);
	H.bad("套件崩溃", String(e && e.stack ? e.stack : e));
	H.summary("capacity");
	process.exitCode = 1;
});
