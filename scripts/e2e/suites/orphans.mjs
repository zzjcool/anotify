#!/usr/bin/env node
/* SUITE: orphans — 孤儿 pending 任务的诊断与处置（#270）· v4 重写
 *
 * 病根：`pending` 原本没有任何 abandon 路径。能力升级后没有节点声明那个版本、
 * 唯一能跑它的节点永久下线、能力被删除……任务就永远停在 pending。
 * 更要命的是背压的判定信号是 `MIN(created_at) WHERE phase='pending'`，
 * **一条孤儿任务就能把整个部署的新事件入口钉死在 503 上**。
 *
 * ============================ v4 口径（与 v3 的根本差别） ============================
 *
 * v3 的判据是「有一张 nodes 申报能力的表 + 能力 id 还在不在」。v4 把那张表删了：
 * 「能不能跑」= `EligibleNodesForTask` 的**派发判定**，孤儿视图与调度器共用同一份实现
 * （internal/store/orphan.go 文件头）。所以一条 pending 成为孤儿的真实原因是：
 *
 *   在线 + ns 申报∩授权 + 机器级配置已 apply + 持有该 digest(applied)
 *   + requires.commands 全部 found + requires.values 都能解析 —— 任一不满足。
 *
 * 于是 reason 词表变成 `no_capability`（能力行已不存在）/ `no_online_node`，
 * 后者再带 protocol 的封闭 `nodeReason`：NoMatchingNode / NodeOffline /
 * NamespaceUnauthorized / ConfigPending / NotDeployed / CommandMissing / ValueMissing。
 * 本套件逐个造出这七种**真实**状态（全部走产品入口：登记节点 / WS hello / 部署动作 /
 * 回执 / 配置 PATCH），因为自己插 nodes/deployments 等于伪造「节点自己报的事实」。
 *
 * 覆盖：
 *  1. 空列表是 `[]` 不是 null；信封字段（count / olderThan / namespace / allScopes / hint）
 *  2. 一个节点都没有 → no_online_node / NoMatchingNode
 *  3. 在线 + 持有 digest → **不**列（不能误报）
 *  4. 能力要命令但节点没探到 → CommandMissing
 *  5. 能力要值但节点/ns 都没有 → ValueMissing
 *  6. 节点没持有该 digest → NotDeployed
 *  7. 节点没申报该 ns → NamespaceUnauthorized
 *  8. 配置 revision 前进、节点还没 apply → ConfigPending（config.applied 之后自愈）
 *  9. 时间下限：刚建的 pending 不算孤儿
 * 10. 跨命名空间：默认 ns / 指定 ns / `namespace=*` 的隔离与 allScopes
 * 11. abandon：200 + 状态/原因落库；再 abandon → 409；不存在 → 404；跨 ns → 404
 * 12. 参数校验 / 鉴权 / 只读（POST 无写入口，列表不受影响）
 * 13. 节点断开（真 WS 关闭）→ NodeOffline（优先级最高）
 */
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "orphans";
const NS = V.DEFAULT_NS;

// mkPending 直插一条 pending 并返回 id。
//
// 为什么不走派发：真实的「派不出去」需要「节点持有正文但槽位/配置恰好不满足」的时序，
// 那会变成 flaky 的时序测试。这里要验的是**判定与处置**，把库摆成目标状态最确定，
// 而「节点侧事实」（在线/配置/部署/命令/值）全部走产品入口造。
function mkPending(dbPath, nsId, cap, ageSec = 7200) {
	return V.seedPendingTask(dbPath, {
		namespaceId: nsId,
		capabilityId: cap.id,
		capability: cap.name,
		version: cap.version,
		digest: cap.digest,
		ageSec,
	});
}

async function listOrphans(base, session, ns) {
	const r = await H.req(base, V.P.orphans(ns), { session });
	V.expectStatus(`GET /v1/orphans?namespace=${ns}`, r, 200);
	return r;
}

// findOrphan 在列表里按任务 id 找一条；找不到返回 null（不静默当空数组）。
function findOrphan(res, taskId) {
	const list = res.json?.orphans;
	if (!Array.isArray(list)) throw new Error(`orphans 不是数组: ${JSON.stringify(res.json)?.slice(0, 200)}`);
	return list.find((o) => o.id === taskId) || null;
}

// patchNodeConfig 推进节点配置 revision（PATCH /v1/nodes/{name}，CAS 在 body metadata）。
async function patchNodeConfig(base, session, name, vars) {
	const cur = await H.req(base, V.P.node(name), { session });
	V.expectStatus(`读节点 ${name}`, cur, 200);
	const rv = cur.json?.metadata?.resourceVersion;
	H.check("节点带 resourceVersion（CAS 令牌）", V.isResourceVersion(rv), String(rv));
	const r = await H.req(base, V.P.node(name), {
		session,
		method: "PATCH",
		body: V.envelope("Node", { name, resourceVersion: rv }, { vars }),
	});
	V.expectStatus(`PATCH 节点 ${name} 的 vars`, r, 200);
	return r.json;
}

// nodeConfig 读节点配置 wrapper（configRevision / appliedRevision 是两个不同的事实）。
async function nodeConfig(base, session, name) {
	const r = await H.req(base, V.P.nodeConfig(name), { session });
	V.expectStatus(`读节点 ${name} 的配置`, r, 200);
	return r.json || {};
}

async function main() {
	console.log("=== SUITE: orphans（孤儿 pending：诊断与处置）· v4 ===");
	H.startTimer();

	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	let node = null;
	try {
		const dbPath = server.dbPath;
		const s = H.seed(dbPath, "orphans");
		const nsId = await H.defaultNamespaceId(server.base, s.session);
		H.check("拿到默认命名空间 id", !!nsId, String(nsId));

		// ---------- 1. 空列表：`[]` 而不是 null ----------
		console.log("--- 1. 空列表与信封形状 ---");
		const empty = await listOrphans(server.base, s.session, NS);
		H.check("空列表是数组（不是 null）", Array.isArray(empty.json?.orphans), JSON.stringify(empty.json).slice(0, 160));
		H.eq("空列表长度 0", empty.json?.orphans?.length, 0);
		H.eq("count = 0", empty.json?.count, 0);
		H.eq("默认 olderThan = 3600 秒", empty.json?.olderThan, 3600);
		H.eq("namespace 回显请求的 ns id", empty.json?.namespace, nsId);
		H.eq("单 ns 视图 allScopes=false", empty.json?.allScopes, false);
		H.check(
			"hint 说明「不会自动清理、用 abandon 显式处置」",
			typeof empty.json?.hint === "string" && empty.json.hint.includes("abandon"),
			String(empty.json?.hint),
		);

		// ---------- 2. 一个节点都没有 → NoMatchingNode ----------
		console.log("--- 2. 判定：没有任何节点 → NoMatchingNode ---");
		const capPlain = await V.mkCapability(server.base, s.session, "orphan.cap.plain");
		const noNodeTask = mkPending(dbPath, nsId, capPlain);
		const list2 = await listOrphans(server.base, s.session, NS);
		const hit2 = findOrphan(list2, noNodeTask);
		H.check("列出那条 2 小时前的孤儿任务", !!hit2, JSON.stringify(list2.json).slice(0, 240));
		H.eq("reason = no_online_node", hit2?.reason, "no_online_node");
		H.eq("nodeReason = NoMatchingNode（一台节点都没有）", hit2?.nodeReason, "NoMatchingNode");
		H.eq("类别：任务仍是 pending", hit2?.phase, "pending");
		H.eq("诊断带 namespaceId", hit2?.namespaceId, nsId);
		H.eq("诊断带能力名", hit2?.capability, "orphan.cap.plain");
		H.eq("诊断带钉住的 digest", hit2?.digest, capPlain.digest);
		H.check("idleSec ≈ 7200", (hit2?.idleSec || 0) >= 7000, `idleSec=${hit2?.idleSec}`);

		// ---------- 3-8. 节点侧事实逐项不满足 ----------
		console.log("--- 3. 起一台真节点：在线 + 配置 applied + 持有 digest ---");
		node = await V.bringUpEligibleNode(server.base, s.session, { name: "orphan-node", namespace: NS });

		// 三个能力：普通 / 要命令 / 要值；另一个能力**故意不部署**（NotDeployed）。
		const capCmd = await V.mkCapability(
			server.base,
			s.session,
			"orphan.cap.needs-command",
			{ body: V.capabilityBody({ name: "orphan.cap.needs-command", requires: { commands: ["git"] } }) },
		);
		const capVal = await V.mkCapability(
			server.base,
			s.session,
			"orphan.cap.needs-value",
			{ body: V.capabilityBody({ name: "orphan.cap.needs-value", requires: { values: ["vars.SOURCE_ROOT"] } }) },
		);
		const capNotDeployed = await V.mkCapability(server.base, s.session, "orphan.cap.not-deployed");

		// 节点**真的持有**三份正文（部署动作 + capability.deployed(applied) 回执）。
		// 不部署 capNotDeployed：节点在线且配置已 apply，但「没持有这份正文」= NotDeployed。
		await V.deployApplied(server.base, s.session, node, capPlain, NS);
		await V.deployApplied(server.base, s.session, node, capCmd, NS);
		await V.deployApplied(server.base, s.session, node, capVal, NS);

		const cmdTask = mkPending(dbPath, nsId, capCmd);
		const valTask = mkPending(dbPath, nsId, capVal);
		const notDeployedTask = mkPending(dbPath, nsId, capNotDeployed);

		const list3 = await listOrphans(server.base, s.session, NS);
		H.eq("在线且持有 digest → 普通能力的任务**不**出现在孤儿列表", findOrphan(list3, noNodeTask), null);
		H.eq(
			"要命令但节点没探到 → nodeReason CommandMissing",
			findOrphan(list3, cmdTask)?.nodeReason,
			"CommandMissing",
		);
		H.eq(
			"要值但三层都没有 SOURCE_ROOT → nodeReason ValueMissing",
			findOrphan(list3, valTask)?.nodeReason,
			"ValueMissing",
		);
		H.eq(
			"节点没持有该 digest → nodeReason NotDeployed",
			findOrphan(list3, notDeployedTask)?.nodeReason,
			"NotDeployed",
		);
		for (const [label, id] of [
			["CommandMissing", cmdTask],
			["ValueMissing", valTask],
			["NotDeployed", notDeployedTask],
		]) {
			H.eq(`${label} 的类别都是 no_online_node`, findOrphan(list3, id)?.reason, "no_online_node");
		}

		// ---------- 7. NamespaceUnauthorized：节点没申报那个 ns ----------
		console.log("--- 4. 跨命名空间：节点没申报 ns → NamespaceUnauthorized ---");
		const ns2Slug = "orphans-two";
		const ns2 = await V.createNamespace(server.base, s.session, ns2Slug, "孤儿套件第二命名空间");
		V.expectStatus("建第二个命名空间", ns2, 201);
		const ns2Id = ns2.json?.metadata?.uid;
		H.check("第二个命名空间拿到 id", typeof ns2Id === "string" && ns2Id.length > 0, String(ns2Id));
		const capNs2 = await V.mkCapability(server.base, s.session, "orphan.cap.ns2", { namespace: ns2Slug });
		const ns2Task = mkPending(dbPath, ns2Id, capNs2);

		const listNs2 = await listOrphans(server.base, s.session, ns2Slug);
		const hitNs2 = findOrphan(listNs2, ns2Task);
		H.check("第二个命名空间自己的视图里看得见", !!hitNs2, JSON.stringify(listNs2.json).slice(0, 200));
		H.eq(
			"节点没申报 ns2 → nodeReason NamespaceUnauthorized",
			hitNs2?.nodeReason,
			"NamespaceUnauthorized",
		);
		H.eq("默认 ns 的视图**不**含 ns2 的任务", findOrphan(await listOrphans(server.base, s.session, NS), ns2Task), null);

		// `namespace=*` = 跨 ns 诊断汇总（逐 ns 过授权收口）
		const all = await listOrphans(server.base, s.session, "*");
		H.eq("namespace=* → allScopes=true", all.json?.allScopes, true);
		H.eq("namespace=* 回显 *", all.json?.namespace, "*");
		H.check("namespace=* 同时含两个 ns 的孤儿", !!findOrphan(all, ns2Task) && !!findOrphan(all, cmdTask), JSON.stringify(all.json).slice(0, 200));
		H.eq("count 与 orphans.length 一致", all.json?.count, all.json?.orphans?.length);

		// ---------- 8. ConfigPending（配置 revision 前进，节点还没 apply） ----------
		console.log("--- 5. 配置 revision 前进 → ConfigPending，config.applied 之后自愈 ---");
		await patchNodeConfig(server.base, s.session, node.name, { E2E_BUMP: "1" });
		const cfgPending = await nodeConfig(server.base, s.session, node.name);
		H.check(
			"配置 revision 已前进、applied 还停在旧值",
			cfgPending.configRevision !== cfgPending.appliedRevision,
			`revision=${cfgPending.configRevision} applied=${cfgPending.appliedRevision}`,
		);
		const listPending = await listOrphans(server.base, s.session, NS);
		H.eq(
			"配置未 apply → nodeReason ConfigPending（比 NotDeployed/缺命令更具体）",
			findOrphan(listPending, noNodeTask)?.nodeReason,
			"ConfigPending",
		);

		// 节点回报 config.applied（新 revision）→ 这条任务回到「能跑」（不再列出）
		node.conn.send({
			type: "config.applied",
			revision: cfgPending.configRevision,
			hash: V.machineConfigHash(cfgPending.vars || {}, cfgPending.limits || {}),
			instances: [{ slot: 0, busy: false }],
		});
		const healed = await V.tryPoll(async () => {
			const r = await listOrphans(server.base, s.session, NS);
			return findOrphan(r, noNodeTask) === null;
		}, { timeoutMs: 8000, intervalMs: 200 });
		H.check("config.applied 之后普通能力的任务不再是孤儿（自愈）", healed, "仍在列表里");
		const listHealed = await listOrphans(server.base, s.session, NS);
		H.eq("自愈后 CommandMissing 仍然成立（配置不是它唯一的问题）", findOrphan(listHealed, cmdTask)?.nodeReason, "CommandMissing");

		// ---------- 9. 时间下限：刚建的 pending 不算孤儿 ----------
		console.log("--- 6. olderThan 下限防误报 ---");
		const freshTask = mkPending(dbPath, nsId, capNotDeployed, 10);
		H.eq("10 秒前的 pending 不算孤儿", findOrphan(await listOrphans(server.base, s.session, NS), freshTask), null);

		// olderThan 是**窗口**而不是开关：3 小时前的任务在 2h 窗口外、在 4h 窗口内。
		const threeHourTask = mkPending(dbPath, nsId, capNotDeployed, 3 * 3600);
		const w2h = await H.req(server.base, `${V.P.orphans(NS)}&olderThan=2h`, { session: s.session });
		V.expectStatus("olderThan=2h → 200", w2h, 200);
		H.eq("olderThan=2h：含 3 小时前的任务（2h < 3h）", findOrphan(w2h, threeHourTask)?.id, threeHourTask);
		const window2h = await H.req(server.base, `${V.P.orphans(NS)}&olderThan=4h`, { session: s.session });
		V.expectStatus("olderThan=4h → 200", window2h, 200);
		H.eq("olderThan=4h：3 小时前的任务在窗口外（4h > 3h）", findOrphan(window2h, threeHourTask), null);
		H.eq("olderThan 回显请求的窗口（7200）", w2h.json?.olderThan, 7200);

		// ---------- 10. abandon：处置并留下原因 ----------
		console.log("--- 7. abandon：处置 + 冲突 + 404 ---");
		const reason = "能力没有节点声明，已确认不再需要";
		const ab = await H.req(server.base, V.P.taskAbandon(NS, noNodeTask), {
			session: s.session,
			method: "POST",
			body: { reason },
		});
		V.expectStatus("POST …/tasks/{id}/abandon → 200", ab, 200);
		H.eq("返回的 Task 信封 kind", ab.json?.kind, "Task");
		H.eq("返回的任务相位 = abandoned", ab.json?.status?.phase, "abandoned");
		const row = V.readTaskRow(dbPath, noNodeTask);
		H.eq("库里 phase = abandoned", row?.phase, "abandoned");
		H.eq("原因写进 error 列（事后能回答「为什么没跑」）", row?.error, reason);
		H.check("终态写了 finished_at", !!row?.finished_at, String(row?.finished_at));
		H.eq("处置后不再出现在孤儿列表", findOrphan(await listOrphans(server.base, s.session, NS), noNodeTask), null);

		const again = await H.req(server.base, V.P.taskAbandon(NS, noNodeTask), {
			session: s.session,
			method: "POST",
			body: { reason: "second attempt" },
		});
		V.expectReason("已 abandoned 再 abandon → 409（CAS 不假装成功）", again, 409, "Conflict");
		H.eq("第二次拒绝没有改写原因", V.readTaskRow(dbPath, noNodeTask)?.error, reason);

		const missing = await H.req(server.base, V.P.taskAbandon(NS, "tsk_nope"), {
			session: s.session,
			method: "POST",
			body: { reason: "x" },
		});
		V.expectStatus("不存在的任务 → 404", missing, 404);
		V.expectStatus(
			"跨 ns 处置别人的任务 → 404（不泄露存在性）",
			await H.req(server.base, V.P.taskAbandon(NS, ns2Task), { session: s.session, method: "POST", body: { reason: "x" } }),
			404,
		);

		// ---------- 11. 参数校验 / 鉴权 / 只读 ----------
		console.log("--- 8. 参数校验 / 鉴权 / 只读 ---");
		V.expectStatus("olderThan=0 → 400（不许把刚派发的当孤儿）", await H.req(server.base, `${V.P.orphans(NS)}&olderThan=0`, { session: s.session }), 400);
		V.expectStatus("olderThan=abc → 400", await H.req(server.base, `${V.P.orphans(NS)}&olderThan=abc`, { session: s.session }), 400);
		V.expectStatus("olderThan=30s → 400（下限 1 分钟）", await H.req(server.base, `${V.P.orphans(NS)}&olderThan=30s`, { session: s.session }), 400);
		V.expectStatus("limit=0 → 400", await H.req(server.base, `${V.P.orphans(NS)}&limit=0`, { session: s.session }), 400);
		V.expectStatus("未登录 → 401", await H.req(server.base, V.P.orphans(NS)), 401);
		V.expectStatus(
			"缺 namespace → 400（绝不回退默认 / 不做全量）",
			await H.req(server.base, V.P.orphans(), { session: s.session, namespaceId: null }),
			400,
		);
		V.expectStatus("不存在的命名空间 → 404", await H.req(server.base, V.P.orphans("no-such-ns"), { session: s.session }), 404);

		// 只读：这条端点没有写入口（未注册的方法落到静态兜底 → 404；显式 405 也没写语义）。
		// 断言「非 2xx + 列表事实不变」：只钉状态码会让「哪天有人加了个 POST 写入口」漏过去。
		// 比较的是 id 集合而不是整份 JSON：`idleSec` 每秒都在变，整份比会假红。
		const idsOf = (res) => (res.json?.orphans || []).map((o) => o.id).sort();
		const before = idsOf(await listOrphans(server.base, s.session, NS));
		const postOrphans = await H.req(server.base, V.P.orphans(NS), { session: s.session, method: "POST", body: {} });
		H.check(
			"POST /v1/orphans 没有写入口（404/405）",
			postOrphans.status === 404 || postOrphans.status === 405,
			`status=${postOrphans.status} ${String(postOrphans.text).slice(0, 120)}`,
		);
		const after = await listOrphans(server.base, s.session, NS);
		H.eq("POST /v1/orphans 不改事实（孤儿集合与 count 都不变）", JSON.stringify(idsOf(after)), JSON.stringify(before));
		H.eq("POST /v1/orphans 后 count 不变", after.json?.count, before.length);

		// 显式 reject 提示：hint 指到 v4 的 abandon 路径（ns 进路径）
		H.check(
			"hint 指到 v4 的 abandon 路径 /v1/namespaces/{ns}/tasks/{id}/abandon",
			String(after.json?.hint || "").includes("/v1/namespaces/{ns}/tasks/{id}/abandon"),
			String(after.json?.hint),
		);

		// ---------- 12. 节点断开 → NodeOffline（优先级最高） ----------
		console.log("--- 9. 节点断开 → NodeOffline ---");
		const offlineTask = mkPending(dbPath, nsId, capPlain);
		node.close();
		node = null;
		// 断开是异步落库的（hub 的会话收尾），轮询到原因变成 NodeOffline。
		const offlineSeen = await V.tryPoll(async () => {
			const r = await listOrphans(server.base, s.session, NS);
			return findOrphan(r, offlineTask)?.nodeReason === "NodeOffline";
		}, { timeoutMs: 15000, intervalMs: 200 });
		const listOffline = await listOrphans(server.base, s.session, NS);
		H.check("节点断开后任务变成孤儿，且原因 = NodeOffline", offlineSeen, JSON.stringify(findOrphan(listOffline, offlineTask)));
		H.eq("NodeOffline 的类别仍是 no_online_node", findOrphan(listOffline, offlineTask)?.reason, "no_online_node");
	} finally {
		node?.close?.();
		server.stop();
	}

	const okAll = H.summary(SUITE);
	if (!okAll) process.exitCode = 1;
}

main().catch((e) => {
	console.error("SUITE: orphans 崩溃：", e);
	H.bad("套件崩溃", String(e && e.stack ? e.stack : e));
	H.summary(SUITE);
	process.exitCode = 1;
});
