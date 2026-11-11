#!/usr/bin/env node
/* SUITE: node_v4 — 真实节点 v4 核心链（真后端 + 真 `cmd/anode` 二进制，零 mock）
 *
 * 为什么这个套件必须存在：v4 把「谁来执行」从「中枢内的执行器」换成了**独立节点二进制**
 * （capability.md §0/§7：能力只在装了 anode 的机器上跑）。这条链的每一环都在不同进程里：
 *
 *   中枢（cmd/server）──WS──▶ anode（cmd/anode serve）
 *                          ├─ 真 hello（token 身份 + 申报 ns + 槽位）
 *                          ├─ welcome → 真落盘 applied → config.applied
 *                          ├─ capability.deploy → GET /v1/node/capabilities/{digest} → 真缓存 → deployed
 *                          ├─ task.dispatch → 真 accept（真空闲槽）→ lease.grant → 真起子进程
 *                          └─ task.result（真 summary + 真托管产物）→ 真上传 PUT /v1/node/tasks/…
 *
 * 单测各自钉住一侧：store 的事务、hub 的帧校验、anode 的 runner。**没有**一层能回答
 * 「两端合起来还通吗」——而这正是 v3 时代反复踩的坑（每层都对、连起来才错）。
 *
 * 覆盖（每条都对应契约里的一句话）：
 *   1. 纯 v2 hello 被拒（v4 只认 `hello.token`；v2 的 `node_id` + `protocol:2` 不是凭据）
 *   2. 共享辅助 lib/nodes.mjs 的 v4 hello → welcome 是 v4 形状（configRevision/nodeVars/
 *      nodeLimits/namespaces，且没有 `protocol` / `applied` 这些退役键）
 *   3. POST /v1/nodes → POST /v1/nodes/{name}/tokens（明文只出现一次）→ 起真 anode
 *      → GET /v1/nodes 里出现真实上报（agentVersion / appliedRevision / 物理槽位）
 *   4. POST …/capabilities/{name}/deploy → 节点真拉产物（磁盘上的缓存是证据）
 *      → deployments 行从 pending 变 applied
 *   5. 投递事件 → 真任务：attempt ≥ 1（**只有 lease.grant 生效才递增**）→
 *      task.result 的 summary / artifacts → 终态 succeeded
 *   6. GET …/tasks/{id}/logs 有真实 stdout + stderr 原文
 *   7. run.files 产物上行 + 私有下载字节一致（GET …/artifacts/{sha256}）；未引用的
 *      hash → 404（授权只认这份 manifest）
 *   8. 取消：运行中取消（cancel 帧 → cancel.ack processGroupReaped → 槽位真的放回）
 *      与 pending 取消（直接终态，不需要帧）
 *   9. 节点进程退出后中枢不再认为它在线（deploy 的 sent=false，不伪造发送）
 *
 * 纪律：
 *   - **不用 sleep 猜状态**：一律「轮询 + 上限」，超时把最后一帧/最后一份响应原样带出来。
 *   - 整条套件有界：每步都有独立上限，末尾断言总时长 < 5 分钟。
 *   - 失败时打印 anode 的 stdout/stderr 尾部（真二进制的问题只在那里面）。
 */
import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { closeSync, existsSync, openSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";

import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const SUITE = "node_v4";
const NS = "default"; // 路径用 slug（v4 把命名空间放进路径；slug 优先，id 兼容）

// 全局上限：任何一步都不能把套件拖成「看起来在跑，其实卡死」。
const SUITE_BUDGET_MS = 300_000;

// 每步上限（毫秒）。都是「绿路径下只要几十毫秒」的同一数量级，上限只是防挂。
const T = {
	handshake: 30_000,
	deploy: 30_000,
	run: 60_000,
	cancel: 45_000,
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// poll 轮询到 fn 返回真值；到点抛错（带 what + 最后一次观察结果）。
//
// 为什么不返回 null 让调用方自己断言：套件里有十几处等待，漏一处 `if (!v) throw`
// 就会变成「断言静默跳过」。这里把「等不到」直接变成一条带上下文的失败。
async function poll(what, fn, { timeoutMs, intervalMs = 100 } = {}) {
	const limit = timeoutMs || T.run;
	const deadline = Date.now() + limit;
	let last = null;
	for (;;) {
		last = await fn();
		if (last) return last;
		if (Date.now() > deadline) {
			throw new Error(`等待超时（${limit}ms）：${what}\n最后一次观察: ${JSON.stringify(last)}`);
		}
		await sleep(intervalMs);
	}
}

// reqJ 是 H.req 的「要求 2xx」版本：失败时把状态码与响应体一起抛出去。
// 造数/断言混在一个函数里最容易丢诊断信息（只有 status 没有 body）。
async function reqJ(what, p, opts = {}, expect = null) {
	const r = await H.req(server.base, p, opts);
	if (expect !== null && r.status !== expect) {
		throw new Error(`${what}: 期望 HTTP ${expect}，实际 ${r.status}；body=${String(r.text).slice(0, 600)}`);
	}
	return r;
}

// fetchRaw 取原始字节（产物下载必须是字节比较，不能过 text()）。
async function fetchRaw(p, opts = {}) {
	const headers = { ...(opts.headers || {}) };
	if (opts.session) headers["Cookie"] = "anotify_session=" + opts.session;
	const res = await fetch(server.base + p, { headers, redirect: "manual" });
	const buf = Buffer.from(await res.arrayBuffer());
	const hdrs = {};
	res.headers.forEach((v, k) => {
		hdrs[k.toLowerCase()] = v;
	});
	return { status: res.status, buf, headers: hdrs, text: buf.toString("utf8") };
}

// sha256hex 是「文件字节的 sha256」（64 位小写 hex，不带 sha256: 前缀）——
// 产物传输的 {sha256} 就是这个，不是 spec digest（A1 §语义边界）。
const sha256hex = (buf) => createHash("sha256").update(buf).digest("hex");

// ---------- 套件状态 ----------
let server = null;
let session = "";
let sendKey = "";
let nsId = "";
let nodeName = "";
let nodeId = "";
let anode = null; // { proc, logPath, dataDir, configPath, stop() }
let suiteStart = Date.now();

// anodeLog 读真二进制的输出尾部（失败诊断里最有用的一份证据）。
function anodeLog(tail = 4000) {
	if (!anode) return "（anode 未启动）";
	try {
		const raw = readFileSync(anode.logPath, "utf8");
		return raw.length > tail ? "…" + raw.slice(-tail) : raw;
	} catch (e) {
		return `（读不到 anode 日志 ${anode.logPath}: ${e && e.message}）`;
	}
}

// requireAnodeBin 在起进程**之前**确认二进制存在。
// 缺文件时必须是红（不是 skip）：run_all.sh 会构建它，单独跑请自带 ANODE_BIN。
function requireAnodeBin() {
	const bin = H.ANODE_BIN;
	if (!existsSync(bin)) {
		throw new Error(
			`缺少 anode 二进制 ${bin}。run_all.sh 会构建它；单独跑请先 ` +
				`go build -o .e2e-bin/anode ./cmd/anode（或用 ANODE_BIN 指过去）`,
		);
	}
	return bin;
}

// startAnode 写 anode.yaml 并起真 serve 进程。
//
// listen 用 unix socket：套件不需要额外 TCP 端口段，也不会与并行套件抢端口。
// PATH/HOME 必须由我们自己给：anode 的任务环境是**启动基线快照**（execenv 的白名单），
// 没 PATH 时 runner 会直接判 workdir/env 不可用 —— 那是真实约束，不是测试噪声。
async function startAnode({ bin, token, dataDir, name }) {
	const configPath = path.join(dataDir, "anode.yaml");
	const yaml = [
		`name: ${name}`,
		`server: ws://127.0.0.1:${server.port}`,
		`token: ${token}`,
		"namespaces:",
		`  - ${NS}`,
		`listen: unix://${path.join(dataDir, "emit.sock")}`,
		"",
	].join("\n");
	writeFileSync(configPath, yaml, { mode: 0o600 });

	const logPath = path.join(dataDir, "anode.log");
	const logFd = openSync(logPath, "w");
	const proc = spawn(bin, ["serve", "--config", configPath, "--data", dataDir], {
		cwd: dataDir,
		env: {
			...process.env,
			PATH: "/usr/local/bin:/usr/bin:/bin",
			HOME: dataDir,
		},
		stdio: ["ignore", logFd, logFd],
	});
	let exited = null;
	proc.on("exit", (code, sig) => {
		exited = { code, sig };
	});
	const state = {
		proc,
		configPath,
		dataDir,
		logPath,
		exited: () => exited,
		// stop 幂等：SIGTERM → 最多等 8s → SIGKILL（anode 自己会收 TERM 退干净）。
		stop: async () => {
			if (exited) return exited;
			try {
				proc.kill("SIGTERM");
			} catch {
				/* 已退出 */
			}
			const deadline = Date.now() + 8000;
			while (!exited && Date.now() < deadline) await sleep(50);
			if (!exited) {
				try {
					proc.kill("SIGKILL");
				} catch {
					/* ignore */
				}
				await sleep(200);
			}
			try {
				closeSync(logFd);
			} catch {
				/* ignore */
			}
			return exited;
		},
	};
	return state;
}

// ---------- 资源构造 ----------

// mkCapability 用 v4 写请求建一个能力版本，返回 {name, version, digest, uid}。
//
// 故意声明空的 requires.commands：这个套件验的是**核心链**，不是命令探测
// （CommandMissing 的匹配另有其测）。入口脚本用 /bin/sh，节点上必然存在。
async function mkCapability({ name, script, timeoutSec = 30, next = [] }) {
	const body = {
		apiVersion: "anotify/v1",
		kind: "Capability",
		metadata: { name },
		spec: {
			params: {},
			next,
			run: { entrypoint: "run.sh", files: { "run.sh": script } },
			requires: { commands: [], values: [] },
			lifecycle: { idempotent: false, timeoutSec },
			concurrency: { maxPerNode: 1 },
		},
	};
	const r = await reqJ(`建能力 ${name}`, `/v1/namespaces/${NS}/capabilities`, { session, body }, 201);
	const doc = r.json || {};
	const digest = doc.status?.digest || "";
	const version = doc.metadata?.version;
	H.check(`能力 ${name} 建出来带真实 digest`, /^sha256:[0-9a-f]{64}$/.test(digest), String(digest));
	H.check(`能力 ${name} 的版本号是正整数`, Number.isInteger(version) && version >= 1, String(version));
	return { name, version, digest, uid: doc.metadata?.uid || "" };
}

// mkSource 建一个 webhook 源，targets 指向给定能力。
async function mkSource({ name, capability }) {
	const body = {
		apiVersion: "anotify/v1",
		kind: "Source",
		metadata: { name },
		spec: {
			kind: "webhook",
			enabled: true,
			config: {},
			targets: [{ capability }],
		},
	};
	const r = await reqJ(`建源 ${name}`, `/v1/namespaces/${NS}/sources`, { session, body }, 201);
	H.eq(`源 ${name} 的目标就是 ${capability}`, r.json?.spec?.targets?.[0]?.capability, capability);
	return r.json;
}

// emitEvent 走真实 ingress（Bearer API key，scope notify:send）投递一条事件。
//
// 回执里的 tasks[].taskId 一定是**已建任务**的真 id（E1）——这里当场断言，
// 免得后面拿 undefined 去请求任务详情，报出一个与被测行为无关的 404。
async function emitEvent({ source, type, payload = {} }) {
	const r = await reqJ(
		`投递事件 ${type}`,
		`/v1/namespaces/${NS}/events`,
		{ key: sendKey, body: { source, type, id: randomUUID(), payload } },
		200,
	);
	const receipt = r.json || {};
	H.check(
		`事件 ${type} 建出真任务（receipt.tasks 非空）`,
		Array.isArray(receipt.tasks) && receipt.tasks.length === 1 && !!receipt.tasks[0].taskId,
		JSON.stringify(receipt).slice(0, 400),
	);
	H.eq(`事件 ${type} 没有剪枝`, (receipt.pruned || []).length, 0);
	return receipt.tasks[0].taskId;
}

// getTask 读任务信封（v4 只读事实）。
async function getTask(taskId) {
	const r = await H.req(server.base, `/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}`, {
		session,
	});
	if (r.status !== 200) {
		throw new Error(`GET task ${taskId}: ${r.status} ${String(r.text).slice(0, 400)}`);
	}
	return r.json || {};
}

// waitPhase 轮询任务到某个相位（终态才返回）。
async function waitPhase(taskId, want, timeoutMs, what) {
	let last = null;
	return poll(
		`${what}（任务 ${taskId} → ${want}）`,
		async () => {
			const doc = await getTask(taskId);
			last = doc.status || {};
			return last.phase === want ? doc : null;
		},
		{ timeoutMs },
	).catch((e) => {
		throw new Error(`${e.message}\nphases 观察: ${JSON.stringify(last)}`);
	});
}

// ---------- 各阶段 ----------

// partV2Refused：纯 v2 hello 必须被拒。
//
// v4 只认 `hello.token`。v2（`{type:"hello", node_id, protocol:2, applied[], instances[]}`）
// 里没有凭据 → 中枢 `authenticate` 报「hello 缺少 token」并**关连接**（1008），
// 一个 welcome 帧都不会发。这条就是「协议版本没有兼容层」的机械证据。
async function partV2Refused() {
	console.log("--- 1. 纯 v2 hello 被拒（v4 只认 hello.token） ---");
	const conn = await N.wsConnect(server.base, null);
	const frames = [];
	let closed = null;
	conn.onFrame((f) => frames.push(f));
	conn.onClose((info) => {
		closed = info;
	});
	conn.send({
		type: "hello",
		protocol: 2,
		node_id: "nd_e2e_v2_shape",
		name: "node-v4-v2-probe",
		applied: [],
		instances: [{ slot: 0, concurrency: 1 }],
	});
	const deadline = Date.now() + 5000;
	while (!closed && Date.now() < deadline) await sleep(30);
	conn.close();
	H.check("v2 hello 没有换来 welcome", !frames.some((f) => f.type === "welcome"), JSON.stringify(frames).slice(0, 300));
	H.check(
		"v2 hello 被关连接（策略违规 1008）",
		!!closed && closed.code === 1008,
		JSON.stringify(closed),
	);
	// 落地检查：这帧没有建立任何节点（库里不该出现这个名字）。
	const nodes = await reqJ("GET /v1/nodes", "/v1/nodes", { session }, 200);
	const names = (nodes.json?.items || []).map((n) => n.metadata?.name);
	H.check("v2 hello 没有登记出节点", !names.includes("node-v4-v2-probe"), JSON.stringify(names));
}

// partMockHello：共享辅助 lib/nodes.mjs 的 v4 hello / welcome 形状。
//
// 这不是「再测一遍中枢」：它是给 12 个还在用 mock 节点的套件（routing / capacity / …）
// 钉住共享底座的**唯一**入口。migration 之后 welcome 必须是 v4 形状（configRevision 等），
// 不是 v2 的 `{protocol, vars, limits}`。
async function partMockHello() {
	console.log("--- 2. 共享辅助的 v4 hello → v4 welcome ---");
	const mock = await N.bringUpNode(server.base, server.dbPath, nsId, "node-v4-mock", [], 1, {
		namespaceSlug: NS,
	});
	try {
		const welcome = mock.frames.find((f) => f.type === "welcome");
		H.check("mock 节点收到 welcome", !!welcome, JSON.stringify(mock.frames).slice(0, 300));
		const keys = Object.keys(welcome || {});
		H.check(
			"welcome 是 v4 形状（configRevision/nodeVars/nodeLimits/namespaces）",
			["configRevision", "nodeVars", "nodeLimits", "namespaces"].every((k) => keys.includes(k)),
			JSON.stringify(keys),
		);
		H.check(
			"welcome 没有 v2 的退役键（protocol / vars / limits）",
			!keys.includes("protocol") && !keys.includes("vars") && !keys.includes("limits"),
			JSON.stringify(keys),
		);
		H.check(
			"welcome.namespaces 是授权交集（包含 default）",
			Array.isArray(welcome.namespaces) && welcome.namespaces.includes(NS),
			JSON.stringify(welcome.namespaces),
		);
		const list = await reqJ("GET /v1/nodes", "/v1/nodes", { session }, 200);
		const row = (list.json?.items || []).find((n) => n.metadata?.name === "node-v4-mock");
		H.check("mock 节点出现在 GET /v1/nodes", !!row, JSON.stringify(list.json?.items || []).slice(0, 300));
		H.check(
			"mock 节点申报的槽位被中枢记账（instances 非空）",
			Array.isArray(row?.status?.instances) && row.status.instances.length === 1,
			JSON.stringify(row?.status?.instances),
		);
	} finally {
		mock.conn.close();
	}
}

// partCreateNode：走真实产品路径登记节点 + 签发令牌。
async function partCreateNode() {
	console.log("--- 3. POST /v1/nodes → POST /v1/nodes/{name}/tokens（明文只出现一次） ---");
	const create = await reqJ(
		"登记节点",
		"/v1/nodes",
		{
			session,
			body: {
				apiVersion: "anotify/v1",
				kind: "Node",
				metadata: { name: nodeName },
				spec: { namespaces: [NS] },
			},
		},
		201,
	);
	const created = create.json || {};
	nodeId = created.metadata?.uid || "";
	H.eq("登记出的节点名就是请求的名字", created.metadata?.name, nodeName);
	H.check("登记响应不带任何凭据材料", !("token" in created) && !JSON.stringify(created).includes("token"), JSON.stringify(created).slice(0, 200));

	const tok = await reqJ("签发节点令牌", `/v1/nodes/${encodeURIComponent(nodeName)}/tokens`, { session, body: {} }, 200);
	const token = tok.json?.token || "";
	H.check("令牌明文在签发响应里出现", typeof token === "string" && token.length >= 20, `len=${token.length}`);
	H.eq("令牌文档的 kind 是 NodeToken", tok.json?.kind, "NodeToken");
	return token;
}

// partAnodeOnline：起真 anode，等中枢侧出现真实的 v4 上报。
async function partAnodeOnline(bin, token) {
	console.log("--- 4. 起真 anode，等 GET /v1/nodes 出现真实上报 ---");
	const dataDir = H.mkdtempTmp("anotify-node-v4-");
	anode = await startAnode({ bin, token, dataDir, name: nodeName });

	const read = async () => {
		const r = await H.req(server.base, `/v1/nodes/${encodeURIComponent(nodeName)}`, { session });
		if (r.status !== 200) return null;
		return r.json || {};
	};

	// 在线 = 中枢收到了这台机器的 hello。三个字段只有真 hello 才写得进去：
	//   agentVersion（D-I13 自报版本）、appliedRevision（config.applied 回路）、instances（物理槽）。
	const doc = await poll(
		"anode 上线并完成配置对齐（agentVersion + appliedRevision + 槽位）",
		async () => {
			const d = await read();
			const st = d?.status || {};
			const ok =
				!!st.agentVersion &&
				!!st.appliedRevision &&
				st.appliedRevision === st.configRevision &&
				Array.isArray(st.instances) &&
				st.instances.length >= 1;
			return ok ? d : null;
		},
		{ timeoutMs: T.handshake },
	).catch((e) => {
		throw new Error(`${e.message}\nanode 日志:\n${anodeLog(3000)}`);
	});

	const st = doc.status || {};
	H.check("中枢记录了节点自报的版本（hello.agentVersion）", !!st.agentVersion, String(st.agentVersion));
	H.eq("配置已对齐：appliedRevision == configRevision", st.appliedRevision, st.configRevision);
	H.check("中枢记录了机器事实（facts.hostname 落库）", !!st.hostname, String(st.hostname));
	H.check(
		"中枢记录了真实物理槽位（instances >= 1 且此刻空闲）",
		st.instances.length >= 1 && st.instances.every((i) => i.busy === false),
		JSON.stringify(st.instances),
	);
	H.check("有效命名空间是真交集（含 default）", Array.isArray(st.effectiveNamespaces) && st.effectiveNamespaces.includes(NS), JSON.stringify(st.effectiveNamespaces));
	return doc;
}

// partDeploy：人工部署 + 真拉取 + 真 apply。
async function partDeploy(cap) {
	console.log(`--- 5. 部署 ${cap.name} → anode 真拉产物 → deployments=applied ---`);
	// `nodes` 按契约传**节点名**；#348 的 resolveDeployTargets 先名后 id，
	// 响应里的 node 仍回带 id（部署事实钉的是 id）。按 id 的兼容路径由 deploy 套件覆盖。
	const dep = await reqJ(
		`部署 ${cap.name}`,
		`/v1/namespaces/${NS}/capabilities/${encodeURIComponent(cap.name)}/deploy`,
		{ session, body: { nodes: [nodeName] } },
		200,
	);
	const outcomes = dep.json?.deployed || [];
	H.eq("部署响应逐节点给结论（1 台）", outcomes.length, 1);
	const outcome = outcomes[0] || {};
	H.eq("部署目标就是这台节点（按 id 回带）", outcome.node, nodeId);
	// sent=true 是「中枢此刻有这台节点的活跃会话」的直接证据（sess.Send 成功），
	// 比任何「看起来在线」的投影都硬。
	H.check("中枢真的把 capability.deploy 帧发出去了（sent=true）", outcome.sent === true, JSON.stringify(outcome));
	H.eq("部署响应里的版本就是钉住的那一版", dep.json?.version, cap.version);

	// 部署行由节点帧驱动：pending → applied 只能靠 capability.deployed 回执。
	const row = await poll(
		"deployments 行变成 applied（节点回报 capability.deployed）",
		async () => {
			const r = await H.req(server.base, `/v1/namespaces/${NS}/deployments`, { session });
			const rows = r.json?.deployments || [];
			const hit = rows.find((d) => d.nodeId === nodeId && d.capability === cap.name && d.version === cap.version);
			return hit && hit.state === "applied" ? hit : null;
		},
		{ timeoutMs: T.deploy },
	).catch((e) => {
		throw new Error(`${e.message}\nanode 日志:\n${anodeLog(3000)}`);
	});
	H.eq("部署行钉住的 digest 就是能力产物的 digest", row.digest, cap.digest);

	// 磁盘证据：真 GET /v1/node/capabilities/{digest} + 真落盘。
	// 只有「下载 → 校验 → 写缓存」都成功，这个目录才会存在（capability.Store）。
	const hex = cap.digest.replace(/^sha256:/, "");
	const cached = path.join(anode.dataDir, "capabilities", hex, "artifact.yaml");
	H.check(
		`anode 缓存里真的有这份产物（${path.relative(anode.dataDir, cached)}）`,
		existsSync(cached) && statSync(cached).size > 0,
		existsSync(cached) ? "存在" : `目录里: ${(() => {
			try {
				return JSON.stringify(readdirSync(path.join(anode.dataDir, "capabilities")));
			} catch (e) {
				return String(e && e.message);
			}
		})()}`,
	);
	return row;
}

// partRunAndResult：事件 → 任务 → grant → 执行 → result。
async function partRunAndResult(cap, { marker, artifactBody }) {
	console.log(`--- 6. 投递事件跑 ${cap.name}：grant → 执行 → task.result ---`);
	const taskId = await emitEvent({ source: `node-v4-src-${cap.name}`, type: `node.v4.${cap.name}`, payload: { marker } });

	// 这个能力的脚本是「echo 完就退」，从 dispatch 到终态只有几十毫秒 —— 观察
	// `running` 会变成一场与调度的赛跑（窗口根本抓不到）。所以这里**等终态**，
	// 执行权的证据用契约自己的口径：attempt 只有 lease.grant 生效才递增
	// （capability.md §8.5「派发不是授权」），加上 nodeName / startedAt / finishedAt。
	// 「running 且 leaseExpiresAt 非空」这类中间态事实在下一条长任务用例里观察。
	const done = await poll(
		"任务到终态（lease.grant → 真执行 → task.result 被接受）",
		async () => {
			const d = await getTask(taskId);
			const s = d.status || {};
			return ["succeeded", "failed", "cancelled", "abandoned"].includes(s.phase) ? d : null;
		},
		{ timeoutMs: T.run, intervalMs: 50 },
	).catch(async (e) => {
		const d = await getTask(taskId).catch(() => ({}));
		throw new Error(`${e.message}\n当前任务: ${JSON.stringify(d.status || {})}\nanode 日志:\n${anodeLog(3000)}`);
	});
	const ds = done.status || {};
	H.eq("任务终态成功（不是 failed/cancelled）", ds.phase, "succeeded");
	// attempt ≥ 1 = lease.grant 真的生效过（派发本身不计 attempt）。
	const attempt = ds.attempt;
	H.check("lease.grant 生效过（attempt ≥ 1，派发不计 attempt）", attempt >= 1, String(attempt));
	H.check("任务绑定的节点名就是这台（wire 上的名字）", ds.nodeName === nodeName, String(ds.nodeName));
	H.check("执行真的发生过（startedAt / finishedAt 都不是 null）", !!ds.startedAt && !!ds.finishedAt, `${ds.startedAt} → ${ds.finishedAt}`);
	H.check("结果 summary 非空（节点显式上报）", !!ds.result?.summary, JSON.stringify(ds.result).slice(0, 300));
	H.eq("成功结果没有失败原因", ds.result?.error?.reason ?? "", "");
	H.eq("运行产物条目数（run.files 显式导出了 1 个文件）", (ds.result?.artifacts || []).length, 1);
	const art = (ds.result?.artifacts || [])[0] || {};
	// 托管副本的逻辑路径带 ANOTIFY_ARTIFACT_DIR 这一层（导出目录相对路径是 `artifacts/`）。
	H.eq("产物逻辑路径就是脚本写在导出目录里的那个", art.path, "artifacts/report.txt");
	H.eq("产物 sha256 等于字节的 sha256（不是 spec digest）", art.sha256, sha256hex(artifactBody));
	H.check("finishedAt 非空", !!ds.finishedAt, String(ds.finishedAt));
	return { taskId, attempt };
}

// partLogs：日志子资源返回真实原文。
async function partLogs(taskId, attempt) {
	console.log("--- 7. GET …/tasks/{id}/logs：真实 stdout / stderr ---");
	const r = await reqJ("读任务日志", `/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/logs`, { session }, 200);
	const doc = r.json || {};
	H.eq("日志 attempt 就是真实执行的那一次", doc.attempt, attempt);
	H.eq("日志没有被截断（1 次执行的小输出）", doc.truncated, false);
	const lines = doc.lines || [];
	const text = lines.map((l) => l.text).join("\n");
	H.check("日志里有真实 stdout 原文", text.includes("node-v4 stdout"), text.slice(0, 400));
	H.check("日志里有真实 stderr 原文（两个流都采）", text.includes("node-v4 stderr"), text.slice(0, 400));
	H.check(
		"stdout / stderr 被分到不同 stream（不是混成一个）",
		lines.some((l) => l.stream === "stdout" && l.text.includes("node-v4 stdout")) &&
			lines.some((l) => l.stream === "stderr" && l.text.includes("node-v4 stderr")),
		JSON.stringify(lines.map((l) => l.stream)),
	);
	H.check("每行都带节点采集序号", lines.every((l) => Number.isInteger(l.sequence)), JSON.stringify(lines.map((l) => l.sequence)));
	return doc;
}

// partArtifacts：产物上传已发生（result manifest）+ 私有下载字节一致。
async function partArtifacts(taskId, artifactBody) {
	console.log("--- 8. 产物私有下载（GET …/artifacts/{sha256}）字节一致 ---");
	const sha = sha256hex(artifactBody);
	const got = await fetchRaw(
		`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/artifacts/${sha}`,
		{ session },
	);
	H.eq("下载被引用产物 → 200", got.status, 200);
	H.check("下载字节与脚本写进 ANOTIFY_ARTIFACT_DIR 的字节一致", got.buf.equals(artifactBody), `got=${got.buf.length}B want=${artifactBody.length}B`);
	H.eq("响应是原始字节流", got.headers["content-type"], "application/octet-stream");
	H.eq("产物禁止在中枢域渲染（nosniff）", got.headers["x-content-type-options"], "nosniff");

	// 阴性：未被这份 manifest 引用的 hash 一律 404（授权只认当前 result 的实际引用）。
	const unknown = "0".repeat(64);
	const miss = await fetchRaw(`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/artifacts/${unknown}`, { session });
	H.eq("未在 manifest 引用的 hash → 404（不泄露存在性）", miss.status, 404);

	// 未登录 / 别的会话拿不到：私有产物的第一道门是会话。
	const anon = await fetchRaw(`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/artifacts/${sha}`, {});
	H.eq("未登录下载 → 401", anon.status, 401);
}

// partCancelRunning：运行中取消 → cancel 帧 → cancel.ack（进程组真回收）→ 槽位放回。
async function partCancelRunning(sleeperCap) {
	console.log("--- 9. 运行中取消：cancel 帧 → cancel.ack → 槽位真的放回 ---");
	const taskId = await emitEvent({ source: `node-v4-src-${sleeperCap.name}`, type: `node.v4.${sleeperCap.name}`, payload: {} });
	const running = await poll(
		"长任务进入 running（占住唯一槽位）",
		async () => {
			const d = await getTask(taskId);
			return d.status?.phase === "running" && d.status?.attempt >= 1 ? d : null;
		},
		{ timeoutMs: T.cancel, intervalMs: 50 },
	).catch(async (e) => {
		const d = await getTask(taskId).catch(() => ({}));
		throw new Error(`${e.message}\n当前任务: ${JSON.stringify(d.status || {})}\nanode 日志:\n${anodeLog(3000)}`);
	});
	const occupied = running.status?.nodeName;
	H.eq("正在跑的任务绑在这台节点上", occupied, nodeName);

	// 取消前先确认槽位真的被占着（busy 事实来自 node_instances）：
	// 这是「槽位回收」断言的对照组，否则 cancel 之后看到空闲无法归因。
	const before = await peakBusySlot();
	H.check("取消前该槽位处于占用状态", before.busyCount >= 1, JSON.stringify(before.instances));

	// body 故意留空（→ 缺省原因 cancelled_by_user）。
	// #348 已把「用户取消」与「放弃」由端点意图区分：cancel 恒 cancelled（人话 reason
	// 也落 cancelled），abandon 才 abandoned。人话 reason 的回归由 store 单测覆盖；
	// 这里保留无 reason 的主路径，避免把两条语义混在同一个 e2e 断言里。
	const cancel = await reqJ(
		"请求取消",
		`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/cancel`,
		{ session, body: {} },
		200,
	);
	H.check("取消立刻写入了意图（任务仍在跑，相位不是终态）", ["running", "cancelled"].includes(cancel.json?.status?.phase), String(cancel.json?.status?.phase));

	const done = await waitPhase(taskId, "cancelled", T.cancel, "取消后任务进入终态 cancelled");
	H.eq("取消原因落在终态结果里", done.status?.result?.error?.reason, "cancelled_by_user");

	// cancel.ack 的 processGroupReaped=true 是槽位释放的唯一依据（hub 不伪造回收）。
	// 所以「槽位回到空闲」就是 ack 真的到了的机械证据。
	const after = await poll(
		"cancel.ack 之后槽位回到空闲（进程组已回收）",
		async () => {
			const snap = await peakBusySlot();
			return snap.busyCount === 0 ? snap : null;
		},
		{ timeoutMs: T.cancel },
	).catch(async (e) => {
		throw new Error(`${e.message}\nanode 日志:\n${anodeLog(3000)}`);
	});
	H.eq("取消后没有槽位仍在占用", after.busyCount, 0);
	return taskId;
}

// peakBusySlot 读节点当前的槽位占用快照。
async function peakBusySlot() {
	const r = await reqJ("GET /v1/nodes/{name}", `/v1/nodes/${encodeURIComponent(nodeName)}`, { session }, 200);
	const instances = r.json?.status?.instances || [];
	return { instances, busyCount: instances.filter((i) => i.busy).length };
}

// partCancelPending：还没被派发的任务取消 = 直接终态（不需要帧、不计 attempt）。
async function partCancelPending(pendingCap) {
	console.log("--- 10. pending 取消：没有节点能跑 → 直接终态，不计 attempt ---");
	const taskId = await emitEvent({ source: `node-v4-src-${pendingCap.name}`, type: `node.v4.${pendingCap.name}`, payload: {} });
	const pending = await getTask(taskId);
	H.eq("没有部署记录的待跑任务停在 pending", pending.status?.phase, "pending");
	H.eq("pending 的任务没有被授权（attempt=0）", pending.status?.attempt, 0);

	const cancel = await reqJ(
		"取消 pending 任务",
		`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/cancel`,
		{ session, body: {} },
		200,
	);
	H.eq("pending 取消直接进终态（无需等节点）", cancel.json?.status?.phase, "cancelled");
	H.eq("pending 取消仍然不计 attempt", cancel.json?.status?.attempt, 0);
	const again = await reqJ(
		"对已终态任务再取消",
		`/v1/namespaces/${NS}/tasks/${encodeURIComponent(taskId)}/cancel`,
		{ session, body: {} },
		409,
	);
	H.eq("重复取消 → 409（终态不可再停，不是 5xx）", again.status, 409);
}

// partNodeExit：节点进程退出后中枢不再认为它在线（不伪造发送）。
async function partNodeExit(cap) {
	console.log("--- 11. 节点进程退出 → 中枢不再把它当在线（deploy.sent=false 而不是假装发送） ---");
	const exited = await anode.stop();
	H.check("anode 进程收到 SIGTERM 后自己退了（不是被 KILL）", !!exited && exited.sig !== "SIGKILL", JSON.stringify(exited));

	// 断连 = 会话消失。此时中枢必须承认「发不出去」：deployed[].sent=false。
	// 这条同时钉住「offline 是事实而不是猜测」：判据是 hub 的会话表，不是超时。
	const dep = await waitSentFalse(cap);
	H.check("节点离线后部署结论 sent=false（离线排队，不是失败）", dep?.sent === false, JSON.stringify(dep));
	// 行不会被抹掉：部署意图是中枢的事实，节点离线不等于「它没持有过这份产物」。
	// state 保持 applied（最后一次可信回报），不因为离线被改写成别的态。
	const rows = await reqJ("GET deployments", `/v1/namespaces/${NS}/deployments`, { session }, 200);
	const row = (rows.json?.deployments || []).find((d) => d.nodeId === nodeId && d.capability === cap.name);
	H.check("离线后部署行仍在（不因离线删行）", !!row, JSON.stringify(rows.json?.deployments || []).slice(0, 300));
	H.eq("离线不抹掉最后一次可信回报（state 仍是 applied）", row?.state, "applied");

	// 离线节点不能跑任务：新事件的任务停在 pending（EligibleNodes 报 NodeOffline）。
	const taskId = await emitEvent({ source: `node-v4-src-${cap.name}`, type: "node.v4.offline", payload: {} });
	const t = await getTask(taskId);
	H.eq("节点离线后新任务停在 pending（不伪造执行）", t.status?.phase, "pending");
	H.eq("离线任务没有被授权（attempt=0）", t.status?.attempt, 0);
}

// waitSentFalse 轮询「部署结论变成 sent=false」。
async function waitSentFalse(cap) {
	return poll(
		"节点离线后 POST …/deploy 返回 sent=false",
		async () => {
			const r = await H.req(
				server.base,
				`/v1/namespaces/${NS}/capabilities/${encodeURIComponent(cap.name)}/deploy`,
				{ session, body: { nodes: [nodeId] } },
			);
			if (r.status !== 200) return null;
			const outcome = (r.json?.deployed || [])[0];
			return outcome && outcome.sent === false ? outcome : null;
		},
		{ timeoutMs: T.deploy },
	);
}

// ---------- 主流程 ----------
async function main() {
	suiteStart = Date.now();
	H.startTimer();
	const bin = requireAnodeBin();

	server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	const seeded = H.seed(server.dbPath, "node_v4");
	session = seeded.session;
	sendKey = seeded.sendKey;
	nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("解析不到默认命名空间 id（GET /v1/namespaces）");
	H.check("anode 二进制存在（run_all.sh 负责构建）", existsSync(bin), bin);

	// 1. 协议版本：v2 hello 必须被拒。
	await partV2Refused();

	// 2. 共享底座：lib/nodes.mjs 的 v4 hello / welcome。
	await partMockHello();

	// 3/4. 真节点：产品路径登记 + 真二进制上线。
	nodeName = "node-v4-real";
	const token = await partCreateNode();
	await partAnodeOnline(bin, token);

	// 5. 三份能力 + 三份源（核心链 / 长任务取消 / 未部署的 pending）。
	const probeScript = [
		"#!/bin/sh",
		"set -eu",
		'echo "node-v4 stdout: running task ${ANOTIFY_TASK_ID}"',
		'echo "node-v4 stderr: real anode" >&2',
		'mkdir -p "$ANOTIFY_ARTIFACT_DIR"',
		"printf 'node-v4 artifact payload\\nsecond line\\n' > \"$ANOTIFY_ARTIFACT_DIR/report.txt\"",
		'echo "node-v4 stdout: done"',
		"",
	].join("\n");
	const artifactBody = Buffer.from("node-v4 artifact payload\nsecond line\n", "utf8");

	const probeCap = await mkCapability({ name: "node-v4-probe", script: probeScript });
	const sleeperCap = await mkCapability({
		name: "node-v4-sleeper",
		timeoutSec: 90,
		script: ["#!/bin/sh", "set -eu", 'echo "node-v4 cancel: started"', "sleep 120", 'echo "node-v4 cancel: not reached"', ""].join("\n"),
	});
	const pendingCap = await mkCapability({
		name: "node-v4-pending",
		script: ["#!/bin/sh", "set -eu", 'echo "node-v4 pending: should never run"', ""].join("\n"),
	});
	await mkSource({ name: `node-v4-src-${probeCap.name}`, capability: probeCap.name });
	await mkSource({ name: `node-v4-src-${sleeperCap.name}`, capability: sleeperCap.name });
	// pendingCap 故意**不部署**：没有 deployments 行 = 没有节点能跑它。
	await mkSource({ name: `node-v4-src-${pendingCap.name}`, capability: pendingCap.name });

	// 6. 真部署（probe 与 sleeper 都要真跑，两份都部署）。
	await partDeploy(probeCap);
	await partDeploy(sleeperCap);

	// 7/8. 真执行 + 真日志 + 真产物。
	const { taskId, attempt } = await partRunAndResult(probeCap, { marker: "probe", artifactBody });
	await partLogs(taskId, attempt);
	await partArtifacts(taskId, artifactBody);

	// 9/10. 两种取消。
	await partCancelRunning(sleeperCap);
	await partCancelPending(pendingCap);

	// 11. 节点退出后的中枢口径。
	await partNodeExit(probeCap);

	const elapsed = Date.now() - suiteStart;
	H.check(`套件整体有界（${(elapsed / 1000).toFixed(1)}s < ${SUITE_BUDGET_MS / 1000}s）`, elapsed < SUITE_BUDGET_MS, `${elapsed}ms`);
}

main()
	.then(() => {
		if (anode) anode.stop();
		if (server) server.stop();
		const allPassed = H.summary(SUITE);
		process.exitCode = allPassed ? 0 : 1;
		// 兜底退出：套件本体已完成，别让残留 timer 把进程留成"卡住"。
		setTimeout(() => process.exit(process.exitCode), 3000).unref();
	})
	.catch((e) => {
		console.error(`\n[node_v4] 套件异常终止: ${e && e.stack ? e.stack : e}`);
		if (anode) {
			console.error("--- anode 输出（尾部） ---");
			console.error(anodeLog(8000));
			anode.stop();
		}
		if (server) {
			console.error("--- server 输出（尾部） ---");
			console.error(String(server.log() || "").slice(-4000));
			server.stop();
		}
		H.bad("套件异常终止", String(e && e.message ? e.message : e).slice(0, 600));
		H.summary(SUITE);
		process.exit(1);
	});
