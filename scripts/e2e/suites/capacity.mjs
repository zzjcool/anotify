#!/usr/bin/env node
/* SUITE: capacity — 容量与背压（#229 保留期 / #228 派发背压）· v4 重写
 *
 * 这两条是同一个问题的两个面：
 *   - 写入速率长期高于消费速率 → 任务积压无上限（#228）→ 靠**拒绝新事件**兜底
 *   - 事件只进不出 → 磁盘线性增长（#229）→ 靠**保留期**兜底
 *
 * 本套件只验「入口行为与库内事实」，不量性能（性能在 scripts/perf/c 里量）。
 *
 * ============================ v4 口径（与 v3 的差别） ============================
 *
 * 1. 入口是 `POST /v1/namespaces/{ns}/events`（Bearer Key）+ `…/sources/{name}/fire`
 *    （会话）+ `/v1/hooks/{token}`（token）。v3 的 `/v1/events` 已退役 —— 它现在只会
 *    落到静态兜底（404）。三条入口共用一份回执，**都被同一个背压中间件包住**。
 * 2. 判定信号仍是「最老 pending 任务的等待时长」（`MIN(created_at) WHERE phase='pending'`，
 *    v4 把 `status` 改名 `phase`），503 body/头与 v3 逐字相同（§1.2 冻结）。
 * 3. 读模型是 v4 的**资源域 + 保留域**：`GET …/events`、`…/tasks`、`…/sources`、
 *    `…/capabilities`、`/v1/console/events?namespace=`、`/v1/orphans?namespace=`、
 *    `/v1/stats?namespace=`。背压只挡写入，这些必须全绿 —— 否则运维在事故现场
 *    连「发生了什么」都查不了。
 * 4. 派发证据变了：v4 的 assign 只记**候选**（phase 仍是 pending、没有租约），
 *    所以「还在派发」的证据是 `assignment_id` 非空 + `node_id` 写上，不是「离开 pending」。
 * 5. 保留期的**权威是系统配置** `retention.*`（config.md §3.6、CONTRACT R7），不是
 *    `ANOTIFY_EVENT_RETENTION_DAYS`（那个 env 在 v4 只剩启动日志）。清理挂在 30s 的
 *    租约巡检 tick 上（`lease.DefaultInterval`），所以每条保留期用例要等一整轮。
 *
 * 套件里**没有任何 skip / 放宽**：v4 语义变了的地方，断言换成 v4 的对应事实（见上）。
 */
import { copyFileSync, existsSync, statSync } from "node:fs";
import { join } from "node:path";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "capacity";
const NS = V.DEFAULT_NS;

// 库路径一律用 server.dbPath（harness 自己建的临时库，stop() 会连目录一起收掉）。
// **不要**另外指定 ANOTIFY_DB：那样 server.dbPath 指向的是一个服务从未打开过的空库，
// 直插的行会写进另一份文件，症状是「造了数据但服务看不见」。

const countTasks = (dbPath) => V.countRows(dbPath, "tasks");
const countEvents = (dbPath) => V.countRows(dbPath, "events");

// 主库文件的字节数。**先 checkpoint 再读**：auto_vacuum=0 + WAL，不回写的话
// 读到的是「主库 + 未回写 WAL」的组合，同一个逻辑状态每次都能差出几倍。
function dbFileBytes(dbPath) {
	V.withDB(dbPath, (db) => db.prepare(`PRAGMA wal_checkpoint(TRUNCATE)`).get());
	return statSync(dbPath).size;
}

// hasDeletionLog 判断日志里是否有「真的删了事件」的那一轮清理。
//
// v4 的清理日志是 `slog.Info("retention sweep deleted rows", "events", n, …)`，只在**删了
// 东西**时打（EventsDeleted/TasksDeleted/DeliveriesDeleted 任一 > 0），所以「第二轮
// 没有这条」就是「没有重复删」的正向证据。
//
// 两种编码都接受：cmd/server 用 JSON handler（`"events":3`），而纯 text handler 是
// `events=3`。只认一种会让断言在「日志格式换了」时变成假红/假绿。
function hasDeletionLog(logText, field = "events") {
	const re = new RegExp(`(?:"${field}"\\s*:\\s*(\\d+)|\\b${field}=(\\d+))`);
	for (const line of String(logText).split("\n")) {
		if (!line.includes("retention sweep deleted rows")) continue;
		const m = re.exec(line);
		if (m && Number(m[1] || m[2]) > 0) return true;
	}
	return false;
}

// deadPendingAgeSec 是「造一条很老的 pending」用的年龄。
//
// 600s > 任何一种阈值（1s），也远大于采样周期，所以「超阈」不依赖机器的快慢。
const deadPendingAgeSec = 600;

// ---------- 系统配置（保留期的唯一权威） ----------
async function setRetention(base, session, patch) {
	const cur = await H.req(base, V.P.systemConfig(), { session });
	V.expectStatus("读系统配置", cur, 200);
	const rv = cur.json?.metadata?.resourceVersion;
	// 系统配置是 **config wrapper**（§7.5），不是资源：它的 CAS 令牌是纯正整数，
	// 没有资源那套 `rv1:<uid>:<n>` 前缀。两种形状都接受，不猜格式。
	H.check(
		"系统配置带 resourceVersion（CAS 令牌）",
		/^\d+$/.test(String(rv)) || V.isResourceVersion(rv),
		String(rv),
	);
	const retention = {
		eventTtlSec: patch.eventTtlSec ?? cur.json?.retention?.eventTtlSec,
		taskTtlSec: patch.taskTtlSec ?? cur.json?.retention?.taskTtlSec,
		resultTtlSec: patch.resultTtlSec ?? cur.json?.retention?.resultTtlSec,
	};
	const put = await H.req(base, V.P.systemConfig(), {
		session,
		method: "PUT",
		body: { metadata: { resourceVersion: rv }, retention },
	});
	V.expectStatus(`写系统配置 retention=${JSON.stringify(retention)}`, put, 200);
	return put.json;
}

// ---------- 场景 1-7：背压 ----------

// runBackpressure 用给定的 ANOTIFY_PENDING_MAX_AGE_SEC 起一套环境，跑完整条
// 「积压 → 拒绝 → 恢复」链路。enabled=false 时只验「阈值 0 = 不启用」。
async function runBackpressure(enabled) {
	const maxAgeSec = enabled ? 1 : 0;
	const tag = enabled ? "启用(1s)" : "关闭(0)";
	console.log(`--- 背压 ${tag} ---`);

	let server;
	let node;
	try {
		server = await H.startServer({
			suiteName: SUITE,
			portOffset: enabled ? 0 : 1,
			rpId: "localhost",
			extraEnv: { ANOTIFY_PENDING_MAX_AGE_SEC: String(maxAgeSec) },
		});
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, enabled ? "capbp" : "capbp0");
		const nsId = await H.defaultNamespaceId(server.base, s.session);

		// 背压的积压任务钉住一个**自定义**能力：内置能力在 v4 已随中央执行域退役，
		// 而「有执行者的能力」会把 pending 领走（派发/租约），留不住 600s 的积压。
		const backlogCap = await V.mkCapability(server.base, s.session, `cap.pending.${maxAgeSec}`);

		// 一条 webhook 源：用来验「webhook 入口同样被拒」，同时给 ingress 一个**真实
		// 的**源行（v4 ingress 必须按名字查到已启用的源）。
		// targets 留空是刻意的：空 targets 的源只留一条 no_target 剪枝痕、**不建任务**，
		// 于是「积压了几条 pending」这件事完全由本套件直插的那几行决定，不受轮询影响。
		const srcDoc = await V.mkSource(server.base, s.session, "cap-hook", { kind: "webhook" });
		const hookURL = srcDoc?.status?.hookUrl || srcDoc?.spec?.config?.hookUrl || "";
		H.check(
			"webhook 源给出授权读 hookUrl（/v1/hooks/…）",
			typeof hookURL === "string" && hookURL.startsWith("/v1/hooks/"),
			JSON.stringify(srcDoc?.status || {}).slice(0, 160),
		);
		const srcName = srcDoc?.metadata?.name || "cap-hook";

		// 基线：库里没有积压 → 新事件必须 200（默认值不许误伤正常部署）
		const baseline = await V.ingestEvent(server.base, s.sendKey, NS, {
			source: srcName,
			type: "cap.baseline",
			id: "cap-baseline",
			payload: { n: 0 },
		});
		H.eq("无积压时 POST …/events = 200", baseline.status, 200);
		H.check("投递回执带 eventId", typeof baseline.json?.eventId === "string" && baseline.json.eventId.length > 0, JSON.stringify(baseline.json).slice(0, 200));

		// 造一条「等了 600 秒」的 pending（直插：真实路径需要「节点声明了能力但槽位吃满」
		// 的时序，那会变成 flaky 测试；这里要验的是判定与响应，不是派发本身）。
		const pendingID = V.seedPendingTask(dbPath, {
			namespaceId: nsId,
			capabilityId: backlogCap.id,
			capability: backlogCap.name,
			version: backlogCap.version,
			digest: backlogCap.digest,
			ageSec: deadPendingAgeSec,
		});
		const tasksBefore = countTasks(dbPath);

		const rejected = await V.ingestEvent(server.base, s.sendKey, NS, {
			source: srcName,
			type: "cap.backpressure",
			id: "cap-bp-probe",
			payload: { n: 1 },
		});

		if (!enabled) {
			// 阈值 0 = 不启用：无论积压多老都放行（运维的显式选择，不是配置错误）。
			H.eq("阈值 0 时不启用：老积压存在也仍 200", rejected.status, 200);
			H.eq("阈值 0 时任务行数不变（ingress 没有副作用）", countTasks(dbPath), tasksBefore);
			return;
		}

		// 采样在后台跑，且「远离阈值」时是 5s 一档（自适应降采样）：造出积压后最多要等
		// 一个空闲采样周期才会切到超阈。所以这里**轮询到超阈**，而不是 sleep 一个拍脑袋
		// 的数 —— 前者在任何机器速度下都稳，后者必然变成偶发红。
		const over =
			rejected.status === 503
				? rejected
				: await V.poll(
						"背压生效（ingress 返回 503）",
						async () => {
							const r = await V.ingestEvent(server.base, s.sendKey, NS, {
								source: srcName,
								type: "cap.backpressure.poll",
								payload: { n: 2 },
							});
							return r.status === 503 ? r : null;
						},
						{ timeoutMs: 15000, intervalMs: 250 },
					);

		H.eq("超阈时 POST …/events = 503", over.status, 503);
		H.eq("503 body.error = backpressure", over.json?.error, "backpressure");
		H.eq("503 body.max_pending_age_sec 就是当前阈值", over.json?.max_pending_age_sec, maxAgeSec);
		H.check(
			"503 body.pending_age_sec ≥ 600（就是那条老 pending 的年龄）",
			typeof over.json?.pending_age_sec === "number" && over.json.pending_age_sec >= deadPendingAgeSec,
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

		// webhook 入口同样被拒：保护覆盖**全部**新事件入口，不是只有 ingress。
		const hookRes = await H.req(server.base, hookURL, { body: { type: "cap.hook", payload: { n: 2 } } });
		H.eq("超阈时 webhook 入口 = 503", hookRes.status, 503);

		// 手动触发入口同样被拒（契约 §1.4 冻结的第三个新事件入口）。
		//
		// 为什么这条必须单独验：单测覆盖的是 `Wrap` 本身，证明不了「这条路由挂上了它」；
		// 而 fire 是三条里唯一带顺序约束的（session 在外、bp 在内，见 resource_source.go
		// 的 register 注释）—— 把 bp 挪到 session 外面会让**未登录**请求收到 503，而当时
		// 全套测试仍然全绿。
		const fireRes = await V.fireSource(server.base, s.session, NS, srcName, { payload: { n: 9 } });
		H.eq("超阈时 POST …/sources/{name}/fire = 503", fireRes.status, 503);

		// 未登录先 401：不许从 503/200 的差别里读出「中枢现在积压了吗」。
		const fireAnon = await V.fireSource(server.base, null, NS, srcName, { payload: {} });
		H.eq("未登录时 /fire 是 401（不是 503）", fireAnon.status, 401);

		// ---- 「不丢任务」的**正向**证据：503 当口，库里已有的 pending 仍在被派发 ----
		//
		// 只断言「行数没变」不够：那只证明没被删，不证明还在被推进。这里造第二条 600s 的
		// pending，钉住一个**真节点持有正文**的能力（部署 applied + 配置 applied）：
		//   · backlogCap（没人持有）  → 永远派不出去，正是它把信号顶在阈值之上；
		//   · liveCap（真节点持有）   → 应当照常被派发候选，即使入口在 503。
		// 节点不做 accept：v4 的 assign 只记候选（phase 仍 pending），只要能观察到
		// node_id + assignment_id 被写上，就证明「中枢仍在派发」，与节点后面 accept 与否无关。
		const liveCap = await V.mkCapability(server.base, s.session, `cap.live.${maxAgeSec}`);
		node = await V.bringUpEligibleNode(server.base, s.session, { name: `cap-live-node-${maxAgeSec}`, namespace: NS });
		await V.deployApplied(server.base, s.session, node, liveCap, NS);
		const liveTaskID = V.seedPendingTask(dbPath, {
			namespaceId: nsId,
			capabilityId: liveCap.id,
			capability: liveCap.name,
			version: liveCap.version,
			digest: liveCap.digest,
			ageSec: deadPendingAgeSec,
		});
		const tasksWithLive = countTasks(dbPath);

		const dispatched = await V.tryPoll(
			() => {
				const row = V.readTaskRow(dbPath, liveTaskID);
				return !!row && !!row.node_id && !!row.assignment_id;
			},
			{ timeoutMs: 20000, intervalMs: 200 },
		);

		// 同一时刻入口必须还在 503（证明「派发在跑」与「入口被拒」共存，不是顺序巧合）
		const stillRejecting = await V.ingestEvent(server.base, s.sendKey, NS, {
			source: srcName,
			type: "cap.still-rejecting",
			id: "cap-bp-still",
			payload: { n: 4 },
		});

		H.check(
			"503 期间已有 pending 仍被派发（node_id + assignment_id 写上）",
			dispatched,
			JSON.stringify(V.readTaskRow(dbPath, liveTaskID)),
		);
		H.eq("派发发生时入口仍然 503（两件事并存，不是先后）", stillRejecting.status, 503);

		const liveRow = V.readTaskRow(dbPath, liveTaskID);
		H.check("派发后任务行还在（没被丢弃）", !!liveRow, "行消失了");
		H.eq("assign 只记候选：phase 仍是 pending（没有租约就没有执行权）", liveRow?.phase, "pending");
		H.eq("派发后任务没有被删，仍在库", countTasks(dbPath), tasksWithLive);
		// 不可派发的那条必须原地不动（它没被「清理」也没有被误删）
		H.eq("不可派发的那条仍在 pending（背压原因仍然成立）", V.readTaskRow(dbPath, pendingID)?.phase, "pending");

		// 读端点不受影响：背压只挡写入。三个写入口都拒了，读必须全通 ——
		// 否则运维在事故现场连「发生了什么」都查不了。
		const readChecks = [
			["GET …/events（机器入口的事件流）", V.P.events(NS)],
			["GET /v1/console/events（控制台事件流）", V.P.consoleEvents(NS)],
			["GET …/tasks（任务列表）", V.P.tasks(NS)],
			["GET …/capabilities（能力列表）", V.P.capabilities(NS)],
			["GET …/sources（源列表）", V.P.sources(NS)],
			["GET /v1/orphans（孤儿诊断：背压的可观测出口）", V.P.orphans(NS)],
			["GET /v1/stats（工作台统计）", V.P.stats(NS)],
		];
		for (const [label, path] of readChecks) {
			const r = await H.req(server.base, path, { session: s.session });
			V.expectStatus(`503 期间 ${label} 仍 200`, r, 200);
		}
		// 读模型不是空的：那条排不出去的任务在孤儿视图里看得见（背压的唯一出口）。
		// olderThan=5m：那条 pending 等了 600s（10m），而默认诊断窗口是 1h——
		// 用默认窗口会让「刚好 10 分钟」的积压落在窗口外，断言就变成假红/假绿。
		const orphans = await H.req(server.base, `${V.P.orphans(NS)}&olderThan=5m`, { session: s.session });
		V.expectStatus("背压期间孤儿视图仍 200", orphans, 200);
		H.check(
			"背压期间孤儿视图能看见那条积压任务",
			(orphans.json?.orphans || []).some((o) => o.id === pendingID),
			JSON.stringify(orphans.json).slice(0, 200),
		);

		node.close();
		node = null;

		// 积压清掉 → 自动恢复（保护必须自愈，否则一次积压就是永久停机）。
		// 清的是**这一轮造出来的全部**老 pending：backlogCap 那条，以及被派发过候选
		// （节点没 accept、仍留在 pending）的 live 那条。
		V.markTaskDone(dbPath, pendingID);
		V.markTaskDone(dbPath, liveTaskID);
		const recovered = await V.tryPoll(
			async () => {
				const r = await V.ingestEvent(server.base, s.sendKey, NS, {
					source: srcName,
					type: "cap.recovered",
					id: "cap-bp-recovered",
					payload: { n: 3 },
				});
				return r.status === 200;
			},
			{ timeoutMs: 15000, intervalMs: 250 },
		);
		H.check("积压清掉后 15s 内自动恢复 200", recovered, "仍返回 503");

		// 日志可观测性：至少出现过一次 engaged / released（#228 的「积压不可观测」）
		H.check("服务日志里有 server.backpressure.engaged", server.log().includes("server.backpressure.engaged"), server.log().slice(-800));
		H.check("服务日志里有 server.backpressure.released", server.log().includes("server.backpressure.released"), server.log().slice(-800));
	} finally {
		node?.close?.();
		server?.stop();
	}
}

// ---------- 场景 8-9：保留期 ----------

// runRetentionEnabled 验「保留期生效」这一侧：
//   · 默认系统配置是 30 天（eventTtlSec=2592000）——保留期的权威是系统配置
//   · 40 天前的事件被一轮巡检删掉、窗口内的留着
//   · 清理日志给了删了多少（可观测）
//   · **幂等/可重入**：同一份库再起一次服务，第二轮必须「删 0 条、不报错」
async function runRetentionEnabled() {
	console.log("--- 保留期 30 天（默认系统配置）---");

	let server;
	try {
		server = await H.startServer({ suiteName: SUITE, portOffset: 2, rpId: "localhost" });
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "capret");
		const nsId = await H.defaultNamespaceId(server.base, s.session);

		// 启动日志必须先说清「会删」：保留期不可逆，运维必须能从日志读到这台机器的口径。
		H.check(
			"启动日志有 server.retention.configured",
			server.log().includes("server.retention.configured"),
			server.log().slice(0, 800),
		);
		const cfg = await H.req(server.base, V.P.systemConfig(), { session: s.session });
		V.expectStatus("GET /v1/system/config", cfg, 200);
		H.eq("默认 events 保留期 = 30 天（2592000s）", cfg.json?.retention?.eventTtlSec, 2592000);
		H.eq("默认 task 保留期 = 30 天（2592000s）", cfg.json?.retention?.taskTtlSec, 2592000);

		// 三条 40 天前的 + 一条刚刚的（直插：保留期判定只看 created_at，不需要派发副作用）
		const oldIds = [];
		const nowSec = Math.floor(Date.now() / 1000);
		for (let i = 0; i < 3; i++) {
			oldIds.push(
				V.seedEventRow(dbPath, {
					namespaceId: nsId,
					type: "capacity.old",
					createdAt: nowSec - 40 * 86400,
				}).id,
			);
		}
		const fresh = V.seedEventRow(dbPath, { namespaceId: nsId, type: "capacity.fresh", createdAt: nowSec });
		const beforeRows = countEvents(dbPath);

		// 清理挂在 30s 的租约巡检 tick 上 → 上限给 45s（一轮 tick + 处理时间）。
		const swept = await V.tryPoll(() => !V.eventExists(dbPath, oldIds[0]), { timeoutMs: 45000, intervalMs: 250 });
		H.check("过期事件被清理（40 天前）", swept, "45s 内仍存在");
		for (let i = 0; i < oldIds.length; i++) {
			H.eq(`过期事件 #${i} 已删除`, V.eventExists(dbPath, oldIds[i]), false);
		}
		H.eq("窗口内的事件（刚刚写入）保留", V.eventExists(dbPath, fresh.id), true);
		H.eq("事件行数 = 清理后剩下的条数", countEvents(dbPath), beforeRows - oldIds.length);
		H.check("清理日志给了删了多少（retention sweep deleted rows）", hasDeletionLog(server.log(), "events"), server.log().slice(-1200));

		// ---- 幂等 / 可重入：同一份库再起一次服务，第二轮必须「删 0 条、不报错」 ----
		//
		// 为什么这条必须是 e2e：单测只能证明「store 对空集返回 0」，证明不了「真实进程
		// 重启后第一轮 tick 不会因为上次删到一半而重复删/报错」。
		// 先把库搬到 harness 临时目录**之外**再重启（stop() 会把整个 mkdtemp 目录删掉）：
		//   · 拷贝前先证明库是**静默**的（间隔 300ms 两次计数相同），否则是撕裂读；
		//   · 主库与 -wal 一起拷（-shm 是内存索引，新进程自己重建）。
		const rowsT0 = countEvents(dbPath);
		await V.sleep(300);
		H.eq("幂等段前置：拷贝前库是静默的（300ms 内无写入）", countEvents(dbPath), rowsT0);
		const reuseDir = H.mkdtempTmp("anotify-cap-reuse-");
		const reuseDB = join(reuseDir, "test.db");
		copyFileSync(dbPath, reuseDB);
		if (existsSync(`${dbPath}-wal`)) copyFileSync(`${dbPath}-wal`, `${reuseDB}-wal`);
		server.stop();
		server = null;

		const beforeBytes = dbFileBytes(reuseDB);
		const again = await H.startServer({
			suiteName: SUITE,
			portOffset: 3,
			rpId: "localhost",
			extraEnv: { ANOTIFY_DB: reuseDB },
		});
		try {
			// 一整轮 30s tick + 余量（小库的一轮清理是毫秒级，5s 足够观察）
			await V.sleep(35000);
			const round2 = again.log();
			H.check("第二轮清理：没有失败（lease sweep failed）", !round2.includes("lease sweep failed"), round2.slice(-1200));
			H.check(
				"第二轮清理：没有再删任何事件（应该是「无需清理」而不是重复删）",
				!hasDeletionLog(round2, "events"),
				round2.slice(-1200),
			);
			H.eq("第二轮清理：过期事件仍然不存在（幂等）", V.eventExists(reuseDB, oldIds[0]), false);
			H.eq("第二轮清理：窗口内事件仍在", V.eventExists(reuseDB, fresh.id), true);
			H.eq("第二轮清理：事件行数不变", countEvents(reuseDB), beforeRows - oldIds.length);
			H.check(
				"第二轮清理：文件没有被再次撑大（auto_vacuum=0 下不应变大）",
				dbFileBytes(reuseDB) <= beforeBytes + 65536,
				`before=${beforeBytes} after=${dbFileBytes(reuseDB)}`,
			);
		} finally {
			again.stop();
			H.removeTmpDir(reuseDir);
		}
	} finally {
		server?.stop();
	}
}

// runRetentionPermanent 验永久保留（0）这一侧：
//   · env `ANOTIFY_EVENT_RETENTION_DAYS=0` 的启动告警必须在（磁盘会持续增长，不能静默）
//   · 但**真正生效的是系统配置**：PUT retention.eventTtlSec=0 之后 40 天前的事件不被删
//   · 0 的语义是「永久」，不是「立刻删全部」
async function runRetentionPermanent() {
	console.log("--- 保留期 0（永久保留，系统配置权威）---");

	let server;
	try {
		server = await H.startServer({
			suiteName: SUITE,
			portOffset: 4,
			rpId: "localhost",
			extraEnv: { ANOTIFY_EVENT_RETENTION_DAYS: "0" },
		});
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "capret0");
		const nsId = await H.defaultNamespaceId(server.base, s.session);

		H.check(
			"启动日志有 server.retention.permanent 告警（磁盘会持续增长）",
			server.log().includes("server.retention.permanent"),
			server.log().slice(0, 900),
		);

		// v4 的权威是系统配置：env 只影响**启动日志**，不改 system_config。
		// （DEVELOPMENT.md 里「ANOTIFY_EVENT_RETENTION_DAYS 决定保留天数」的表述在 v4
		//   已过期；本套件按 CONTRACT R7 / config.md §3.6 的事实断言，并已在报告里上报。）
		const cfg0 = await H.req(server.base, V.P.systemConfig(), { session: s.session });
		V.expectStatus("GET /v1/system/config（env=0 时）", cfg0, 200);
		H.eq("env=0 不改变 system_config：默认仍是 30 天", cfg0.json?.retention?.eventTtlSec, 2592000);

		// 显式把 events 保留期写成 0（三个字段都要写：漏一个就是 400 value_invalid）
		const put = await setRetention(server.base, s.session, { eventTtlSec: 0 });
		H.eq("PUT 后 events 保留期 = 0", put?.retention?.eventTtlSec, 0);

		const nowSec = Math.floor(Date.now() / 1000);
		const old = V.seedEventRow(dbPath, { namespaceId: nsId, type: "capacity.old", createdAt: nowSec - 40 * 86400 });
		const fresh = V.seedEventRow(dbPath, { namespaceId: nsId, type: "capacity.fresh", createdAt: nowSec });
		const rows = countEvents(dbPath);

		// 等**一整轮** tick（30s + 余量）——「0 = 永久」不能靠「服务刚起还没跑到」蒙混过去。
		// 清理只在删了东西时打日志，所以这一段是「等一整轮后什么都没变」。
		await V.sleep(35000);
		H.eq("永久保留：40 天前的事件不被删（一整轮 tick 之后仍在）", V.eventExists(dbPath, old.id), true);
		H.eq("永久保留：窗口内的事件也在", V.eventExists(dbPath, fresh.id), true);
		H.eq("永久保留：事件行数不变", countEvents(dbPath), rows);
		H.check("永久保留：没有删除日志", !hasDeletionLog(server.log(), "events"), server.log().slice(-1200));

		// ---- 正向对照：把同一个 0 改成 1s，**同一条** 40 天前的事件必须被删 ----
		//
		// 这一步是在证明上面那段的「没删」不是「巡检根本没跑」：同一条行、同一个进程，
		// 唯一的变量是系统配置，而它下一轮就删了。少了这一步，「永久保留」的断言会变成
		// 假绿（巡检完全停摆时也成立）。
		await setRetention(server.base, s.session, { eventTtlSec: 1 });
		const deletedAfterFlip = await V.tryPoll(() => !V.eventExists(dbPath, old.id), { timeoutMs: 45000, intervalMs: 250 });
		H.check("对照：把保留期从 0 改成 1s 后，同一条 40 天前事件被删（巡检确实在跑且读系统配置）", deletedAfterFlip, "45s 内仍存在");
		// TTL 是**绝对年龄**而不是「只删很久以前的」：1s 的窗口连本轮刚写的那条也过期。
		// 这条同时证明「上面那段的没删」不是 0 被误读成 1（否则两条会被一起删）。
		H.eq("对照：1s TTL 下刚写的事件也已过期（TTL 是绝对年龄）", V.eventExists(dbPath, fresh.id), false);
	} finally {
		server?.stop();
	}
}

async function main() {
	console.log("=== SUITE: capacity（#229 保留期 / #228 背压）· v4 ===");
	H.startTimer();

	await runBackpressure(true);
	await runBackpressure(false);
	await runRetentionEnabled();
	await runRetentionPermanent();

	const okAll = H.summary(SUITE);
	if (!okAll) process.exitCode = 1;
}

main().catch((e) => {
	console.error("SUITE: capacity 崩溃：", e);
	H.bad("套件崩溃", String(e && e.stack ? e.stack : e));
	H.summary(SUITE);
	process.exitCode = 1;
});
