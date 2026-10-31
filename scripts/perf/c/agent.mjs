// C 路评估用的常驻节点 agent：声明能力、接单、回结果。
//
// 为什么需要它：C1（持续写入）要量的写路径是「webhook 投递 → 派发 → 任务落库 →
// 分配 → 节点回报 → 结果落库」，光造事件只会走到 pruned.no_node，tasks 表一行不涨，
// 量出来的不是同一个东西。所以要有一个真的挂在线上的节点把任务吃掉。
//
// 复用 e2e 的节点工具（scripts/e2e/lib/nodes.mjs 的 bringUpNode）而不是另写一套
// WS 客户端：握手帧格式（hello/welcome）、节点行/槽位的落库方式都在那里，
// 两套实现一定会漂。
//
// 用法：
//   node scripts/perf/c/agent.mjs --base http://localhost:7940 --db /tmp/x/perf.db \
//        --ns ns_xxx --cap-id cap_xxx --cap-name perf.echo --slots 4 [--delay-ms 20]
//
// 输出（每行一个 JSON，便于下游统计）：
//   {"ev":"ready","node":"nd_...","slots":4}
//   {"ev":"tick","acked":123,"resulted":120}
//   {"ev":"error","err":"..."}
import { bringUpNode } from "../../e2e/lib/nodes.mjs";

const args = {};
for (let i = 2; i < process.argv.length; i += 2) {
	const k = process.argv[i].replace(/^--/, "");
	args[k] = process.argv[i + 1];
}
const base = args.base || "http://localhost:7940";
const dbPath = args.db;
const namespaceId = args.ns;
const capId = args["cap-id"];
const capName = args["cap-name"];
const slots = Number(args.slots || 1);
const delayMs = Number(args["delay-ms"] || 0);
// out 越大，result 落库的 payload 越重；1MB 级别用来测大 payload 的写放大。
const outSize = Number(args["out-size"] || 8);

if (!dbPath || !namespaceId || !capId || !capName) {
	console.error("agent.mjs: 需要 --db --ns --cap-id --cap-name");
	process.exit(2);
}

const caps = [{ id: capId, name: capName, version: 1 }];
const node = await bringUpNode(base, dbPath, namespaceId, `perf-c-agent-${Date.now()}`, caps, slots);
console.log(JSON.stringify({ ev: "ready", node: node.id, slots }));

let acked = 0;
let resulted = 0;
let errors = 0;
const pending = [];

node.conn.onFrame((f) => {
	if (!f || f.type !== "assign") return;
	const taskId = f.task_id;
	if (!taskId) return;
	acked++;
	node.conn.send({ type: "ack", task_id: taskId });
	const finish = () => {
		try {
			node.conn.send({
				type: "result",
				task_id: taskId,
				status: "succeeded",
				out: "x".repeat(outSize),
			});
			resulted++;
		} catch (e) {
			errors++;
			console.log(JSON.stringify({ ev: "error", err: String(e && e.message) }));
		}
	};
	if (delayMs > 0) setTimeout(finish, delayMs);
	else finish();
	pending.push(taskId);
});

const ticker = setInterval(() => {
	console.log(JSON.stringify({ ev: "tick", acked, resulted, errors }));
}, Number(args["tick-ms"] || 30000));
ticker.unref?.();

const bye = () => {
	console.log(JSON.stringify({ ev: "bye", acked, resulted, errors }));
	try { node.conn.close(); } catch { /* ignore */ }
	process.exit(0);
};
process.on("SIGTERM", bye);
process.on("SIGINT", bye);
