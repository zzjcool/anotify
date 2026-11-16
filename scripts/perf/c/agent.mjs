// C 路评估用的常驻节点 agent（v4 帧）：两段式握手 → 部署回执 → 接单 → 回结果。
//
// 为什么需要它：C1（持续写入）要量的写路径是「webhook 投递 → 派发 → 任务落库 →
// 分配 → 节点回报 → 结果落库」。光造事件只写 events/tasks 行：v4 的 tasks 在**接收
// 事务里**就落库，没人吃掉它们 pending 就只增不减 → 最老 pending 越过
// `defaultPendingMaxAgeSec`(120s) → 派发背压把两个投递入口一起打成 503。
// issue 007 的现场就是这个：前 120s 有写入，后 8 分钟 100% 被拒。
//
// ============================ v4 帧（与 v3 的差别） ============================
//
// v3（旧实现）：`assign` → 回 `ack` → 回 `result`。v4 把这三个帧全退役了，
// 旧实现收到的 `assign` 永远不会来、发出去的 `ack`/`result` 被当未知 type 丢弃，
// 于是 acked 恒 0 —— 症状看起来像「中枢没派发」（issue 007 §3 实测）。
//
// v4 的完整链路（唯一真源：internal/protocol；e2e 镜像：scripts/e2e/lib/nodes.mjs）：
//
//   握手   hello{name,token,namespaces,capabilities,instances,appliedRevision}
//          → welcome{configRevision}            ← 首段只为拿这个 revision
//          hello{..., appliedRevision=configRevision} → welcome   ← 第二段重连
//   部署   capability.deploy{requestId,namespace,name,version,digest}
//          → capability.deployed{requestId,同四元组,status:"applied"}
//   执行   task.dispatch{taskId,assignmentId,proposedAttempt,capability,params,...}
//          → task.accept{taskId,assignmentId,proposedAttempt,node,slot}
//          → lease.grant{taskId,attempt,node,leaseId,attemptToken,assignmentId,slot}
//             （**执行权只来自这一帧**：dispatch 不是授权）
//          → task.result{taskId,attempt,node,leaseId,phase,result,startedAt,finishedAt}
//          → result.ack{taskId,attempt,accepted:true}（服务端在这一笔事务里回收槽位）
//
// **两段式握手不能省**：少了第二段，node_configs.applied_revision != revision ⇒
// 节点永远 config_pending ⇒ 永远不是派发候选（症状同样像「中枢没派发」）。
//
// 帧形状**不在这里手写第二份**（那正是两次实现各自漂移的原因）：nodeHello /
// MockNode / wsConnect 全部来自 scripts/e2e/lib/nodes.mjs，accept 与 result 的字段
// 直接走 MockNode 的方法。本文件只负责压测节点需要的三件事：
//
//   1. **推送式**处理帧（同步 switch，不轮询、不做断言式等待）——MockNode 的
//      wait*/waitDispatch 是为 e2e 断言设计的（每帧 push 数组 + 20ms 轮询），
//      压测节点每帧都跑那套会把 CPU 吃在数组增长与轮询上；
//   2. **本地槽位池**：最多 slots 个在飞任务，同一槽不并发复用（服务端
//      leaseSlot 是 CAS，同槽二次 accept 会被拒），有空槽才 accept，否则排队；
//   3. 轻量计数输出（每行一个 JSON，供 s1-soak 采样判「节点到底有没有在吃任务」）。
//
// 刻意不做（与 MockNode 的边界一致）：不跑脚本、不传 artifact、不做 emit、
// 不续租、不回 cancel.ack。压测节点的产出只到「结果行落库」这一段。
//
// 用法：
//   node scripts/perf/c/agent.mjs --base http://localhost:7940 --db /tmp/x/perf.db \
//        --ns ns_xxx --cap-name perf.echo --slots 8 [--tick-ms 5000] [--delay-ms 0] [--out-size 8]
//
//   --cap-id 在 v4 已无用处（「这台机器装着哪个能力」由 deploy 动作产生，不是申报），
//   保留只为命令行兼容。
//
// 输出（每行一个 JSON，便于下游统计）：
//   {"ev":"ready","node":"nd_...","slots":8}
//   {"ev":"tick","acked":123,"resulted":120,"dispatched":124,"confirmed":119,"errors":0,"inflight":4,"queued":0}
//   {"ev":"error","err":"..."}
//   {"ev":"bye","acked":...,"resulted":...,...}
import { DatabaseSync } from "node:sqlite";
import { MockNode, nodeHello, wsConnect } from "../../e2e/lib/nodes.mjs";

const args = {};
for (let i = 2; i < process.argv.length; i += 2) {
	const k = process.argv[i].replace(/^--/, "");
	args[k] = process.argv[i + 1];
}
const base = args.base || "http://localhost:7940";
const dbPath = args.db;
const namespaceId = args.ns;
const capName = args["cap-name"];
const slots = Number(args.slots || 1);
const delayMs = Number(args["delay-ms"] || 0);
// out 越大，result 正文越重：v4 的 result 会落进 tasks.result_json /
// task_attempts.result_json（v3 的 out 字段已随端口路由退役），所以这里把它折成
// `summary` 的长度，量的仍是「大 result 的写放大」。
const outSize = Number(args["out-size"] || 8);
const tickMs = Number(args["tick-ms"] || 30000);

if (!dbPath || !namespaceId || !capName) {
	console.error("agent.mjs: 需要 --db --ns --cap-name");
	process.exit(2);
}

// hello.namespaces 要的是 ns 的 **slug**（人读名），不是 `ns_…` id：
// 「申报 ∩ 授权」为空时节点在线但永远不接活。从库里按 id 反查，避免假设 default。
function namespaceSlugOf(dbFile, id) {
	const db = new DatabaseSync(dbFile);
	try {
		const row = db.prepare("SELECT slug FROM namespaces WHERE id = ?").get(id);
		return row && typeof row.slug === "string" ? row.slug : "";
	} finally {
		db.close();
	}
}

const namespaceSlug = namespaceSlugOf(dbPath, namespaceId);
if (!namespaceSlug) {
	console.error(JSON.stringify({ ev: "error", err: `库里找不到 ns ${namespaceId} 的 slug（hello.namespaces 无法申报）` }));
	process.exit(2);
}

// v4 的 caps 只是 hello **申报**（中枢据此回答 CommandMissing）；「装着哪个能力」
// 由 POST …/capabilities/{name}/deploy + 本文件回的 capability.deployed 产生。
const caps = [{ name: capName, version: 1 }];
const nodeName = `perf-c-agent-${Date.now()}`;
const summaryText = "x".repeat(Math.max(1, outSize));

// ---------- 本地状态：槽位池 + 在飞表 + 排队 ----------
const freeSlots = [];
for (let s = slots - 1; s >= 0; s--) freeSlots.push(s);
const inFlight = new Map(); // taskId → { slot, state, acceptedAt, resultedAt }
const queued = new Set(); // 已排队未 accept 的 taskId（sweep 重复下发时去重）
// 已真正回收（result.ack accepted / task.cancel）的 taskId 短期记住：
// 中枢的 sweep 会在任务已 running/succeeded 之后还重发一次旧 assignment（实测：60s 内 2 次），
// 再 accept 一次只会换来 grant rejected（task is succeeded）并占着槽位等超时。
const doneRecent = new Set();
const doneOrder = [];
function markDone(taskId) {
	if (doneRecent.has(taskId)) return;
	doneRecent.add(taskId);
	doneOrder.push(taskId);
	if (doneOrder.length > 4096) doneRecent.delete(doneOrder.shift());
}
const queue = [];
// 排队的派发窗口只有 30s（task.dispatch.assignmentExpiresAt），且每帧带 params；
// 队列无界增长既是内存风险也说明节点跟不上。上限压在这里，超出的直接丢（服务端
// 会在窗口过期后重新提案），并计数——丢帧是**事实**，不静默。
const QUEUE_MAX = Math.max(8, slots * 4);
let stopping = false;

const counts = { dispatched: 0, acked: 0, resulted: 0, confirmed: 0, cancelled: 0, dropped: 0, duplicate: 0, errors: 0 };
// 往返延迟（accept→grant / grant→ack）：压测节点慢在哪一层要能一眼看出来——
// 是本地槽用完了（queued 堆着），还是中枢每个 grant/result 都要等写锁（lat 大）。
const lat = { grantSum: 0, grantN: 0, ackSum: 0, ackN: 0, max: 0 };

function note(event, extra) {
	counts.errors++;
	console.log(JSON.stringify({ ev: "error", err: event, ...extra }));
}

// ---------- 帧处理（推送式） ----------
function onFrame(f) {
	if (!f || typeof f.type !== "string") return;
	switch (f.type) {
		case "task.dispatch":
			onDispatch(f);
			break;
		case "lease.grant":
			onGrant(f);
			break;
		case "result.ack":
			onAck(f);
			break;
		case "task.cancel":
			onCancel(f);
			break;
		default:
			// capability.deploy 由 MockNode 构造器无条件回 applied（mock 没有真缓存，
			// 只能声明「拿到了」）；welcome / lease.ack / 运维帧在这里不需要动作。
			break;
	}
	// MockNode.frames 是给 e2e 断言用的数组；压测节点不消费它，长期运行只会白吃内存。
	if (node.frames.length > 512) node.frames.length = 0;
}

function onDispatch(f) {
	counts.dispatched++;
	if (inFlight.has(f.taskId) || queued.has(f.taskId) || doneRecent.has(f.taskId)) {
		counts.duplicate++;
		return;
	}
	if (queue.length >= QUEUE_MAX) {
		counts.dropped++;
		return;
	}
	queued.add(f.taskId);
	queue.push(f);
	drain();
}

// drain 把排队里的派发按「有空槽就接」的节奏吃掉。
function drain() {
	while (queue.length > 0 && freeSlots.length > 0) {
		const d = queue.shift();
		queued.delete(d.taskId);
		const slot = freeSlots.pop();
		const now = Date.now();
		inFlight.set(d.taskId, { slot, state: "accepted", acceptedAt: now, t0: now });
		counts.acked++;
		// task.accept 的字段形状（assignmentId/proposedAttempt/node/slot）走 MockNode：
		// 它是 internal/protocol 在脚本侧的唯一镜像。
		node.accept(d, slot);
	}
}

function onGrant(f) {
	const rec = inFlight.get(f.taskId);
	if (!rec) {
		// 没接过的 grant：只可能是本地丢了状态（例如断线重连之前的在飞任务）。
		note("grant_without_accept", { taskId: f.taskId });
		return;
	}
	if (rec.state !== "accepted") return; // 重复 grant：已经回过 result
	rec.state = "granted";
	const now = Date.now();
	lat.grantSum += now - rec.t0;
	lat.grantN++;
	rec.t1 = now;
	finish(f, rec);
}

// finish 上报终态。压测节点「跑完」是零成本事实，delayMs 用来模拟执行耗时。
function finish(grant, rec) {
	const send = () => {
		try {
			node.result(grant, { summary: summaryText });
			counts.resulted++;
			rec.state = "resulted";
			rec.resultedAt = Date.now();
		} catch (e) {
			note("result_send_failed", { taskId: grant.taskId, err: String(e && e.message) });
			release(grant.taskId, rec.slot);
		}
	};
	if (delayMs > 0) setTimeout(send, delayMs);
	else send();
}

function onAck(f) {
	const rec = inFlight.get(f.taskId);
	if (!rec) return;
	if (rec.t1) {
		const ms = Date.now() - rec.t1;
		lat.ackSum += ms;
		lat.ackN++;
		if (ms > lat.max) lat.max = ms;
	}
	if (f.accepted) {
		counts.confirmed++;
		// 只有真被接受才记「这条任务结束了」：accepted=false 时任务可能被重新派发
		// （新 attempt），记成 done 会把重派当重复丢掉。
		markDone(f.taskId);
	} else {
		note("result_rejected", { taskId: f.taskId, attempt: f.attempt });
	}
	// accepted=false 时服务端没回收槽位（attempt 未 reaped）：本地仍要还槽，
	// 否则 8 个槽会被一次错帧永久吃掉，节点静默停摆——那正是本卡要防的形态。
	release(f.taskId, rec.slot);
}

function onCancel(f) {
	counts.cancelled++;
	markDone(f.taskId);
	const rec = inFlight.get(f.taskId);
	// 没有真子进程可等，所以不回 cancel.ack（取消的真实行为归 node_v4 那个真二进制）。
	if (rec) release(f.taskId, rec.slot);
}

function release(taskId, slot) {
	if (!inFlight.delete(taskId)) return;
	freeSlots.push(slot);
	drain();
}

// ---------- 断线重连 ----------
// 一次 WS 断掉不该让 10 分钟的曲线作废。重连**不重新种子**节点行（唯一键会冲突）：
// 用同一个 token 直接重走 hello（带最新 configRevision），deploy 由中枢在 welcome
// 之后重放，MockNode 构造器会自动回执。
async function reconnectLoop() {
	for (let attempt = 1; !stopping; attempt++) {
		await new Promise((r) => setTimeout(r, Math.min(200 * attempt, 2000)));
		try {
			const conn = await wsConnect(base, node.token);
			const next = new MockNode({ conn, name: node.name, id: node.id, token: node.token, slots, namespaceSlug });
			next.configRevision = node.configRevision;
			conn.send(
				nodeHello({
					name: node.name,
					token: node.token,
					namespaces: [namespaceSlug],
					capabilities: caps,
					slots,
					appliedRevision: node.configRevision,
				}),
			);
			const deadline = Date.now() + 3000;
			let welcome = null;
			while (Date.now() < deadline && !stopping) {
				welcome = next.frames.find((x) => x.type === "welcome");
				if (welcome) break;
				await new Promise((r) => setTimeout(r, 25));
			}
			if (!welcome) {
				next.close();
				continue;
			}
			node = next;
			node.configRevision = welcome.configRevision || node.configRevision;
			node.conn.onFrame(onFrame);
			node.conn.onClose(onClose);
			console.log(JSON.stringify({ ev: "reconnected", attempt, node: node.id }));
			return;
		} catch {
			// 中枢还没恢复：下一轮继续（退避上限 2s）。
		}
	}
}

let reconnecting = false;
function onClose(info) {
	if (stopping || reconnecting) return;
	reconnecting = true;
	console.log(JSON.stringify({ ev: "closed", code: info && info.code, reason: (info && info.reason) || "" }));
	reconnectLoop().finally(() => {
		reconnecting = false;
	});
}

// ---------- 启动：seed 节点行 + 两段式握手 ----------
let node = await MockNode.connect({ base, dbPath, namespaceId, name: nodeName, slots, caps, namespaceSlug });
console.log(JSON.stringify({ ev: "ready", node: node.id, name: node.name, namespace: namespaceSlug, slots, applied: node.configRevision }));
node.conn.onFrame(onFrame);
node.conn.onClose(onClose);

// ---------- 计数输出 + 在飞超时看门狗 ----------
const ticker = setInterval(() => {
	const now = Date.now();
	// 看门狗：拿不到 grant / 拿不到 result.ack 的在飞任务会把槽位永久占住。
	// 这里按「多久没进展」还槽（服务端的租约巡检 30s 另有一条独立的回收路径）。
	for (const [taskId, rec] of inFlight) {
		if (rec.state === "accepted" && now - rec.acceptedAt > 10000) {
			note("grant_timeout", { taskId });
			release(taskId, rec.slot);
		} else if (rec.state === "resulted" && now - rec.resultedAt > 5000) {
			note("result_not_acked", { taskId });
			release(taskId, rec.slot);
		}
	}
	console.log(
		JSON.stringify({
			ev: "tick",
			acked: counts.acked,
			resulted: counts.resulted,
			dispatched: counts.dispatched,
			confirmed: counts.confirmed,
			cancelled: counts.cancelled,
			dropped: counts.dropped,
			duplicate: counts.duplicate,
			errors: counts.errors,
			inflight: inFlight.size,
			queued: queue.length,
			grant_ms: lat.grantN > 0 ? +(lat.grantSum / lat.grantN).toFixed(2) : 0,
			ack_ms: lat.ackN > 0 ? +(lat.ackSum / lat.ackN).toFixed(2) : 0,
			max_ms: lat.max,
		}),
	);
	lat.grantSum = 0;
	lat.grantN = 0;
	lat.ackSum = 0;
	lat.ackN = 0;
	lat.max = 0;
}, tickMs);
ticker.unref?.();

const bye = () => {
	stopping = true;
	console.log(JSON.stringify({ ev: "bye", ...counts, inflight: inFlight.size, queued: queue.length }));
	try {
		node.conn.close();
	} catch {
		/* ignore */
	}
	process.exit(0);
};
process.on("SIGTERM", bye);
process.on("SIGINT", bye);
