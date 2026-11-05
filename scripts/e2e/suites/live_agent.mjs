/**
 * live_agent —— 「真 agent 跑真任务」的端到端冒烟（#291）。
 *
 * ## 为什么需要它
 *
 * 有两类 bug 用别的套件都抓不到，而且**门禁全绿**：
 *
 *   1. 中枢算的产物 digest 与节点重算的不一致（内容寻址失效）——
 *      症状是每次部署都被判成 `local`（"节点上是本地变体"），而 `ws_protocol`
 *      那些套件用的是自己构造的假 digest，两边一起错也发现不了。
 *   2. 能力声明的工作目录没被真正用上 —— 症状是任务在默认临时目录里跑，
 *      文件不出现在用户以为的地方，日志里一个错都没有。
 *
 * 这类问题的共同点：**分开看每层都对，连起来才错**。所以这里必须：
 * 真后端 + 真 cmd/agent 进程 + 真部署 + 真任务，最后去**文件系统**上验收。
 *
 * 不碰浏览器，跑完约 10-15s，适合每次合并前跑。
 */
import { spawn } from "node:child_process";
import {
	existsSync,
	mkdtempSync,
	readdirSync,
	readFileSync,
	realpathSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const SUITE = "live_agent";
// run_all.sh 会预构建 .e2e-bin/agent 并导出 AGENT_BIN。
const AGENT_BIN = process.env.AGENT_BIN || path.join(H.ROOT_DIR, ".e2e-bin", "agent");

let server;
let session;
let nsId;
let agentProc;
let agentLog = "";
let tmpDir;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** 轮询直到 fn 返回真值；超时返回 null（不抛）。 */
async function until(fn, { timeoutMs = 15000, intervalMs = 100 } = {}) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		const v = await fn();
		if (v) return v;
		if (Date.now() >= deadline) return null;
		await sleep(intervalMs);
	}
}

async function getJSON(p) {
	const r = await H.req(server.base, p, { session });
	return r.json || {};
}

async function listDeployments() {
	return (await getJSON("/v1/deployments?namespaceId=" + encodeURIComponent(nsId))).deployments || [];
}

async function listNodes() {
	return (await getJSON("/v1/nodes?namespaceId=" + encodeURIComponent(nsId))).nodes || [];
}

async function listTasks() {
	return (await getJSON("/v1/tasks?namespaceId=" + encodeURIComponent(nsId))).tasks || [];
}

/** 建能力（带 run；workDir 为空表示不声明，走默认每任务目录）。 */
async function makeCapability(name, script, workDir) {
	const run = { entrypoint: "run.sh", files: { "run.sh": script }, timeoutSec: 60 };
	if (workDir) run.workDir = workDir;
	const r = await H.req(server.base, "/v1/capabilities", {
		session,
		body: {
			namespaceId: nsId,
			name,
			outputs: [],
			// 必须带 params，而且 key 顺序要和**页面**发的一样（type 在前）。
			// 内容寻址的坑就藏在这里：Parse 会把 params 归一成 key 有序，
			// 而 Digest 当年没归一 → 中枢存一个 digest、节点重算得另一个，
			// 部署永远判成 local。没有 params 的能力复现不出来，所以不能省。
			paramsSchema: { type: "object", properties: { note: { type: "string" } } },
			run,
		},
	});
	H.eq(`建能力 ${name} → 200`, r.status, 200);
	H.check(
		`${name} 编译出产物 digest（内容寻址）`,
		/^sha256:[0-9a-f]{64}$/.test(String(r.json?.digest || "")),
		String(r.json?.digest),
	);
	return { id: r.json?.capability?.id, name, version: r.json?.capability?.version };
}

/** 部署一个能力到那台真节点。 */
async function deploy(cap) {
	const r = await H.req(server.base, "/v1/deployments", {
		session,
		body: { namespaceId: nsId, capabilityId: cap.id, nodeIds: [nodeId] },
	});
	H.eq(`部署 ${cap.name} → 200`, r.status, 200);
	return (r.json?.deployments || [])[0]?.deployId;
}

let nodeId;

async function main() {
	H.startTimer();

	if (!existsSync(AGENT_BIN)) {
		H.check(
			"需要先构建 .e2e-bin/agent（run_all.sh 会做；单独跑请 go build -o .e2e-bin/agent ./cmd/agent）",
			false,
			AGENT_BIN,
		);
		return;
	}

	server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	const seeded = H.seed(server.dbPath, SUITE);
	session = seeded.session;
	nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间", !!nsId, String(nsId));
	if (!nsId) return;

	tmpDir = mkdtempSync(path.join(tmpdir(), "anotify-live-"));
	// 用例声明的执行目录（真实存在）—— 这就是「用户的项目目录」。
	const projDir = realpathSync(mkdtempSync(path.join(tmpDir, "proj-")));

	// ================= 1. 真 agent 连上来 =================
	console.log("--- 1. 真 cmd/agent 起进程并上线 ---");
	// 注意 seedNodeRows 只返回 { id, token }：名字是我们自己起的，别从返回值里取。
	const nodeName = "live-node";
	const node = N.seedNodeRows(server.dbPath, nsId, nodeName, [], 1);
	nodeId = node.id;
	const yamlPath = path.join(tmpDir, "node.yaml");
	// v2/v3 的 node.yaml 只有 bootstrap 四件：目录不再出现在节点配置里（#290）。
	// token 必须加引号：播种用的 token 形如 `{{…}}<hex>`，裸写进 YAML 会被当成
	// **流式映射**的开头（`{` 是语法），报 "line 1: did not find expected key"。
	// 页面上生成的那份（服务端 hex token）裸写没事，但这里不能依赖 token 的形态。
	const yaml = [
		"server: " + JSON.stringify(server.base),
		"token: " + JSON.stringify(node.token),
		"name: " + JSON.stringify(nodeName),
		"instances: 1",
		"",
	].join("\n");
	writeFileSync(yamlPath, yaml);

	agentProc = spawn(
		AGENT_BIN,
		["-f", yamlPath, "-data-dir", path.join(tmpDir, "agent-data"), "-grace", "2"],
		{ stdio: ["ignore", "pipe", "pipe"] },
	);
	agentProc.stdout.on("data", (d) => {
		agentLog += d;
	});
	agentProc.stderr.on("data", (d) => {
		agentLog += d;
	});

	const online = await until(
		async () => (await listNodes()).some((n) => n.id === node.id && n.status === "online"),
		{ timeoutMs: 20000, intervalMs: 150 },
	);
	H.check(
		"真 cmd/agent 用 bootstrap 四件起来，节点变 online",
		!!online,
		online
			? ""
			: `agent 日志：\n${agentLog.slice(-1200)}\n喂进去的 node.yaml（JSON 转义，看真实字节）：\n${JSON.stringify(yaml)}`,
	);
	if (!online) return;

	// ================= 2. 部署 → applied（digest 必须能对账） =================
	console.log("--- 2. 部署带 run.workDir 的能力 ---");
	// 脚本把 pwd 写进 whereami.txt：文件出现在 projDir 里才说明目录真的生效了。
	const cap = await makeCapability(
		"live.whereami",
		'#!/bin/bash\nset -euo pipefail\npwd > whereami.txt\necho "cwd=$(pwd)"\n',
		projDir,
	);

	const depId = await deploy(cap);
	const dep = await until(
		async () => {
			const row = (await listDeployments()).find((d) => d.id === depId);
			return row && row.state === "applied" ? row : null;
		},
		{ timeoutMs: 15000 },
	);
	// 这条就是 #286 的护栏：中枢与节点各自算 digest，只要有一边没归一，
	// 状态会停在 `local`（reason 写着 digest mismatch），这里立刻红。
	H.check(
		"部署状态变 applied（不是 local：两端 digest 必须对得上账）",
		!!dep,
		dep
			? ""
			: JSON.stringify((await listDeployments()).find((d) => d.id === depId) || { state: "缺失" }),
	);

	// ================= 3. 真跑一个任务 =================
	console.log("--- 3. 触发任务：真 agent 在声明的目录里执行 ---");
	// 源必须声明 targets（至少一个出口能力）：源 = 触发点，targets 就是它的第一跳。
	// 少了这一段服务端返回 400，而"触发一个不存在的源"会 404 —— 两种错都很远。
	const srcRes = await H.req(server.base, "/v1/sources", {
		session,
		body: {
			namespaceId: nsId,
			name: "live-src",
			kind: "manual",
			config: {},
			targets: [{ capabilityId: cap.id }],
		},
	});
	H.check("建手动源（带 targets）→ 201", srcRes.status === 201, `${srcRes.status} ${srcRes.text}`);
	const srcId = srcRes.json?.source?.id;

	const fired = await H.req(server.base, `/v1/sources/${srcId}/fire`, {
		session,
		body: { namespaceId: nsId, type: "live.run", payload: { note: "live" } },
	});
	H.check("触发源 → 200 且建出任务", fired.status === 200, `${fired.status} ${fired.text}`);

	const done = await until(
		async () => (await listTasks()).find((t) => t.capability === "live.whereami" && t.status === "succeeded"),
		{ timeoutMs: 20000, intervalMs: 150 },
	);
	H.check(
		"任务成功（真 agent 真的执行了产物）",
		!!done,
		done ? "" : JSON.stringify((await listTasks()).map((t) => [t.capability, t.status, (t.error || "").slice(0, 80)])),
	);

	// ================= 4. 去文件系统验收 =================
	console.log("--- 4. 文件系统验收（这才是用户看到的结果） ---");
	const whereami = path.join(projDir, "whereami.txt");
	H.check("脚本把文件写进了能力声明的目录", existsSync(whereami), projDir);
	if (existsSync(whereami)) {
		H.eq("文件内容就是那个目录（cwd 真的切过去了）", readFileSync(whereami, "utf8").trim(), projDir);
	}

	// 产物文件（run.sh）不应该落进用户目录：它们属于任务目录。
	const leftovers = existsSync(projDir)
		? readdirSync(projDir).filter((n) => n !== "whereami.txt")
		: [];
	H.check("用户目录里没有残留的产物文件（run.sh 不该出现在项目里）", leftovers.length === 0, leftovers.join(","));

	// 任务行的 stdout 落在 summary 里（output 是产物出口，不是控制台输出）。
	H.check(
		"任务 summary 里带着 cwd（排障时最直接的证据）",
		/cwd=/.test(String(done?.summary || "")),
		String(done?.summary || "").slice(0, 160),
	);

	// ================= 5. 没声明 workDir 的能力走默认目录 =================
	console.log("--- 5. 未声明 workDir 的能力：仍在每次任务的独立目录 ---");
	const capDefault = await makeCapability(
		"live.default",
		'#!/bin/bash\nset -euo pipefail\necho "cwd=$(pwd)"\n',
		"",
	);
	await deploy(capDefault);
	const depOk = await until(
		async () =>
			(await listDeployments()).some((d) => d.capabilityId === capDefault.id && d.state === "applied"),
		{ timeoutMs: 15000 },
	);
	H.check("默认目录能力的部署也是 applied", !!depOk);

	const src2 = await H.req(server.base, "/v1/sources", {
		session,
		body: {
			namespaceId: nsId,
			name: "live-src-default",
			kind: "manual",
			config: {},
			targets: [{ capabilityId: capDefault.id }],
		},
	});
	H.eq("建第二个源 → 201", src2.status, 201);
	await H.req(server.base, `/v1/sources/${src2.json.source.id}/fire`, {
		session,
		body: { namespaceId: nsId, type: "live.default.run", payload: {} },
	});
	const done2 = await until(
		async () => (await listTasks()).find((t) => t.capability === "live.default" && t.status === "succeeded"),
		{ timeoutMs: 20000, intervalMs: 150 },
	);
	H.check("第二个任务也成功", !!done2, JSON.stringify((await listTasks()).map((t) => [t.capability, t.status])));
	if (done2) {
		const m = /cwd=(\S+)/.exec(String(done2.summary || ""));
		H.check("没声明就不该进用户目录", !!m && m[1] !== projDir, String(m && m[1]));
		H.check("默认目录在 data-dir 的 work 下", !!m && m[1].includes("work"), String(m && m[1]));
	}

	// ================= 6. 真进程 emit：能力脚本 → 本机 socket → 中枢派发 =================
	//
	// 为什么这一节必须用**真进程**（docs/design/emit-protocol.md §0 的进程树）：
	//
	//   中枢 ──WS── agent ──unix socket── anotify-agent emit ← 能力进程
	//
	// 这条链上任何一层用假节点 / 假 socket 替掉，下面三件事就都没被验过：
	//   ① 环境变量真的被注进了**能力进程**（ANOTIFY_EMIT_SOCK 指向一个活的 unix socket）；
	//   ② 脚本里的 `anotify-agent emit` 真的被 agent 中转、被中枢 ack（acked=routed）；
	//   ③ socket 与任务同生命周期（§1 / §8）：任务终结后文件必须消失——「socket 不存在」
	//      正是中枢/agent 用来拒绝「晚到的 emit 改动已终结的任务」的信号。
	//
	// 断言分两半：**进程里看到的**（探针文件，脚本自己写）+ **中枢里落下的**
	// （任务行：父任务 1 条、下游 2 条）。只看一半就能绿的那种实现是错的。
	console.log("--- 6. 真进程 emit：能力脚本 → 本机 socket → 中枢 ---");

	// 本节自己的建能力辅助（不能复用上面的 makeCapability）：emit 需要一个**声明出口**的
	// 能力（outputs 即 ports），而 makeCapability 把 paramsSchema 写死成 {note}。
	const makeEmitCap = async (name, script, outputs, paramsSchema) => {
		const r = await H.req(server.base, "/v1/capabilities", {
			session,
			body: {
				namespaceId: nsId,
				name,
				outputs,
				paramsSchema: paramsSchema || { type: "object" },
				run: { entrypoint: "run.sh", files: { "run.sh": script }, timeoutSec: 60 },
			},
		});
		H.eq(`建能力 ${name} → 200`, r.status, 200);
		H.check(
			`${name} 编译出产物 digest（内容寻址）`,
			/^sha256:[0-9a-f]{64}$/.test(String(r.json?.digest || "")),
			String(r.json?.digest),
		);
		return {
			id: r.json?.capability?.id,
			name,
			version: r.json?.capability?.version,
			digest: r.json?.digest,
		};
	};

	const probe = path.join(tmpDir, "emit-probe.txt");
	// 两个下游都要 issue：这样 emit 时的 expects（下游要什么的并集）真的非空，
	// 「emit 参数被挡在节点侧」这条链也顺带被覆盖。
	const targetParams = {
		type: "object",
		required: ["issue"],
		properties: { issue: { type: "string" } },
	};
	const t1 = await makeEmitCap(
		"live.emit.target1",
		'#!/bin/bash\nset -euo pipefail\necho "t1 got ${ANOTIFY_PARAM_ISSUE:-} "\n',
		[],
		targetParams,
	);
	const t2 = await makeEmitCap(
		"live.emit.target2",
		'#!/bin/bash\nset -euo pipefail\necho "t2 got ${ANOTIFY_PARAM_ISSUE:-} "\n',
		[],
		targetParams,
	);
	// 能力进程里的 emit：脚本自己把「它看到了什么」写进探针文件（不靠日志格式猜）。
	// 探针路径是绝对值（tmpDir 下的固定文件），所以不依赖 cwd / workDir。
	const emitterScript = [
		"#!/bin/bash",
		"set -uo pipefail",
		`PROBE=${JSON.stringify(probe)}`,
		'sock="${ANOTIFY_EMIT_SOCK:-}"',
		'if [ -S "$sock" ]; then is=yes; else is=no; fi',
		'printf \'sock=%s\\n\' "$sock" > "$PROBE"',
		'printf \'sock_is_socket=%s\\n\' "$is" >> "$PROBE"',
		'printf \'task_id=%s\\n\' "${ANOTIFY_TASK_ID:-}" >> "$PROBE"',
		'printf \'token_len=%s\\n\' "${#ANOTIFY_TASK_TOKEN}" >> "$PROBE"',
		// 真正的调用：--wait-routed = 等中枢 ack 再返回（默认只等本地 outbox 落盘，
		// 那证明不了「已经派发出去」——而这正是本节要断言的那一半）。
		`rc=0`,
		`${JSON.stringify(AGENT_BIN)} emit --port emit.fix --data '{"issue":"#42","title":"空指针"}' --wait-routed --note '第 1 项' >> "$PROBE" 2>&1 || rc=$?`,
		'printf \'exit=%s\\n\' "$rc" >> "$PROBE"',
		// 第二次独立产出（契约 §3）：一次 emit 只派**一个**候选，所以要看全两个目标
		// 就得 emit 两次 —— 同级轮盘各落一个。这也是「两次 emit = 两个独立任务」的
		// 真进程证据。
		`rc2=0`,
		`${JSON.stringify(AGENT_BIN)} emit --port emit.fix --data '{"issue":"#43","title":"竞态"}' --wait-routed --note '第 2 项' >> "$PROBE" 2>&1 || rc2=$?`,
		'printf \'exit2=%s\\n\' "$rc2" >> "$PROBE"',
		// 脚本自己总是成功退：这节的断言看探针里的 exit=，而不是把任务先弄成 failed
		"exit 0",
		"",
	].join("\n");
	const emitter = await makeEmitCap("live.emit", emitterScript, ["emit.fix"]);

	// 三个能力都部署到同一台真节点（下游也要有节点接，否则 no_node 剪枝 → routed=0）
	for (const c of [emitter, t1, t2]) await deploy(c);
	const emitApplied = await until(
		async () => {
			const rows = await listDeployments();
			const want = [emitter.id, t1.id, t2.id];
			const hit = rows.filter((d) => want.includes(d.capabilityId));
			return hit.length === 3 && hit.every((d) => d.state === "applied") ? hit : null;
		},
		{ timeoutMs: 20000 },
	);
	H.check(
		"emit 用例的三个能力都 applied",
		!!emitApplied,
		JSON.stringify(
			(await listDeployments()).map((d) => [d.capability, d.state, (d.reason || "").slice(0, 60)]),
		),
	);
	// 归属只由部署派生（D4）：三个名字必须都在这台节点上，否则 emit 的下游会被
	// no_node 剪掉，症状是「脚本 exit 0、但下游一条任务都没有」——很难从日志上看出来。
	const emitDerived = await until(
		async () => {
			const n = (await listNodes()).find((x) => x.id === nodeId);
			const names = (n?.capabilities || []).map((c) => c.name);
			return [emitter.name, t1.name, t2.name].every((x) => names.includes(x)) ? names : null;
		},
		{ timeoutMs: 20000 },
	);
	H.check(
		"三个能力都从部署派生到这台节点（否则下游会被 no_node 剪掉）",
		!!emitDerived,
		JSON.stringify((await listNodes()).map((n) => (n.capabilities || []).map((c) => c.name))),
	);

	// 一个出口两条边（fan-out）：routed 应当 = 2
	const mkEdge = (fromPort, to) =>
		H.req(server.base, "/v1/edges", {
			session,
			body: {
				namespaceId: nsId,
				fromCapability: emitter.id,
				fromPort,
				toCapability: to.id,
			},
		});
	H.eq("建边 emit.fix → target1", (await mkEdge("emit.fix", t1)).status, 200);
	H.eq("建边 emit.fix → target2", (await mkEdge("emit.fix", t2)).status, 200);

	const emitSrc = await H.req(server.base, "/v1/sources", {
		session,
		body: {
			namespaceId: nsId,
			name: "live-emit-src",
			kind: "manual",
			config: {},
			targets: [{ capabilityId: emitter.id }],
		},
	});
	H.eq("建 emit 用例的源 → 201", emitSrc.status, 201);
	const emitFired = await H.req(
		server.base,
		`/v1/sources/${emitSrc.json?.source?.id}/fire`,
		{ session, body: { namespaceId: nsId, type: "live.emit.run", payload: {} } },
	);
	H.eq("触发 emit 用例的源 → 200", emitFired.status, 200);
	const parentId = (emitFired.json?.dispatch?.tasks || [])[0]?.id;
	H.check("拿到 emit 用例的父任务", !!parentId, JSON.stringify(emitFired.json).slice(0, 300));

	// 父任务跑到终态：脚本退出 → agent 先收 socket 再回 result（顺序有语义，见 client.go）
	const parentDone = await until(
		async () => {
			const t = (await listTasks()).find((x) => x.id === parentId);
			return t && (t.status === "succeeded" || t.status === "failed") ? t : null;
		},
		{ timeoutMs: 30000 },
	);
	H.eq("父任务（跑 emit 脚本的那个）succeeded", parentDone?.status, "succeeded");

	// ---------- 真实进程里看到的（探针文件）----------
	H.check("能力进程写出了探针文件", existsSync(probe), probe);
	const probeText = existsSync(probe) ? readFileSync(probe, "utf8") : "";
	const probeField = (k) =>
		(new RegExp("^" + k + "=(.*)$", "m").exec(probeText) || [])[1] || "";
	const emitSock = probeField("sock");
	H.check(
		"ANOTIFY_EMIT_SOCK 已注入能力进程（是个绝对路径）",
		!!emitSock && path.isAbsolute(emitSock),
		`probe:\n${probeText}`,
	);
	H.eq(
		"注入的路径确实是一个活的 unix socket（agent 在起进程之前就监听好了）",
		probeField("sock_is_socket"),
		"yes",
	);
	H.eq("ANOTIFY_TASK_ID 就是这条任务（身份不串台）", probeField("task_id"), parentId);
	H.check(
		"ANOTIFY_TASK_TOKEN 也注入了（本地 socket 的鉴权凭据）",
		Number(probeField("token_len")) >= 16,
		`token_len=${probeField("token_len")}`,
	);
	H.eq("脚本里 `anotify-agent emit` 的退出码 = 0", probeField("exit"), "0");
	H.check(
		"CLI 报 acked=routed（中枢确认已派发，不是只落了本地 outbox）",
		/acked=routed/.test(probeText),
		probeText,
	);

	// ---------- 中枢里落下的（任务行才是 routed 的真凭据）----------
	const emitKids = (await listTasks()).filter((t) => t.parentTaskId === parentId);
	// 契约 §3：emit.fix 有 2 个候选，**每次 emit 只派一个**；脚本 emit 了两次，
	// 同级轮盘把两次落到不同的候选上 —— 所以这里恰好 2 条，且覆盖两个目标。
	// （以前是「一次 emit 全派 2 条」，那是 fanout，现在是 1:1。）
	H.eq("两次 emit（各派一个候选）→ 共 2 条下游任务", emitKids.length, 2);
	H.eq(
		"两条下游任务分别落在那两个候选上（同级轮盘）",
		emitKids.map((t) => t.capability).sort(),
		[t1.name, t2.name].sort(),
	);
	// P0：本次执行钉在一个确定的产物上（assign.digest / tasks.digest）
	H.eq("父任务钉住的 digest 就是中枢编译出来的那份", parentDone?.digest, emitter.digest);
	H.eq(
		"下游任务的 digest 也各自钉住了自己的产物",
		emitKids.map((t) => t.digest).sort(),
		[t1.digest, t2.digest].sort(),
	);

	// ---------- socket 与任务同生命周期（§1 / §8）----------
	H.check(
		"能力进程退出后 emit socket 已被清理（晚到的 emit 只会得到 exit 6）",
		!!emitSock && !existsSync(emitSock),
		`sock=${emitSock} exists=${emitSock ? existsSync(emitSock) : "?"}`,
	);
}

async function cleanup() {
	if (agentProc && agentProc.exitCode === null) {
		agentProc.kill("SIGKILL");
		await sleep(100);
	}
	if (tmpDir) {
		try {
			rmSync(tmpDir, { recursive: true, force: true });
		} catch {
			/* 清理失败不影响结论 */
		}
	}
	if (server) await server.stop();
}

main()
	.catch((e) => {
		console.error(e);
		H.bad("套件异常终止", String((e && e.message) || e).slice(0, 300));
	})
	.finally(async () => {
		await cleanup();
		const allPassed = H.summary(SUITE);
		process.exitCode = allPassed ? 0 : 1;
		setTimeout(() => process.exit(process.exitCode), 2000).unref();
	});
