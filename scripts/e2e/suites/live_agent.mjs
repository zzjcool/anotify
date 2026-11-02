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
