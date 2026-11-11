#!/usr/bin/env node
/* SUITE: deploy — v4 部署模型（能力产物 / 人工部署动作 / 内容通道 / 部署事实读模型）
 *
 * 这一层验的是**中枢自己的承诺**：
 *   能力定义（+ run 块）→ 编译成内容寻址的产物（digest）→「人工部署」把部署意图
 *   写成事实并把 refs 推给在线节点 → 节点用自己的节点令牌来取**这份 digest 的正文**
 *   → 回执后才算 applied。
 *
 * v4 与 v3 的三处根本差别（本套件的断言都按 v4 口径）：
 *   1. **部署是动作不是资源**：`POST /v1/namespaces/{ns}/capabilities/{name}/deploy`
 *      （v3 的 `POST /v1/deployments` 已退役 → 现在只有 GET，写方法 405）。
 *   2. **目标节点写名字**：`{nodes: ["node-a"]}` 是 `metadata.name`（契约
 *      CapabilityDeployRequest 的口径；#348 修好了旧实现只认 id 的分叉），id 也兼容。
 *   3. **内容通道是节点令牌**：`GET /v1/node/capabilities/{digest}`（不是会话），
 *      只有「已部署到这台节点」的内容可读：未部署 → 403，不存在 → 404。
 *
 * 覆盖：
 *   1. 内置模板 10 个（id 冻结、每个都带 fields/script/requires、script 带 shebang）
 *   2. 建能力带 run → 201 + digest；默认读面**不含**脚本正文，?includeBody=true 才有
 *   3. 内容寻址：同内容省略版本 → 复用同一行/同一 digest；显式版本 → 新行、新 digest
 *   4. 非法 run（入口无 shebang）→ 400 invalid_artifact，且能力**没有被建出来**
 *   5. 缺实现（spec 里没有 run）→ 400 impl_missing
 *   6. 部署到离线节点（**按名字**）→ 200、sent=false、行仍是 pending；按 **id** 也认
 *   7. 未知节点名 / 未知能力 / 未登录 / body 未知字段 → 404 / 404 / 401 / 400
 *   8. 在线节点：sent=true 且节点真收到 capability.deploy 帧；回执 applied 后行变 applied
 *   9. 幂等：同一 (节点, 能力) 重复部署不建第二行
 *  10. 内容通道：本节点已部署 → 200 + canonical JSON；**别的**在线节点 → 403；
 *      未知 digest → 404；会话 Cookie → 401；非 GET → 405
 *  11. 部署事实读模型：GET 只有读；POST/DELETE 显式 405（写入口已随 v3 退役）
 *
 * 纪律：不睡固定时长，异步结论一律「轮询 + 上限」；负向用例断言具体状态码 + reason。
 */
import { createHash } from "node:crypto";

import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "deploy";
const NS = V.DEFAULT_NS;

// 契约 CONTRACT-artifact §8 冻结的 10 个模板 id：写死是有意的 —— 少一个/多一个都
// 意味着控制台表单与后端脱节。
const TEMPLATE_IDS = [
	"script.blank",
	"script.http",
	"script.git",
	"script.test",
	"script.notify",
	"agent.tui",
	"agent.claude",
	"agent.codex",
	"agent.pi",
	"agent.custom",
];

// 一份合法产物：执行方式是**直接 exec**（不经 shell），所以入口首行必须是 shebang。
const OK_SCRIPT = "#!/bin/bash\nset -euo pipefail\necho hello\n";

// machineConfigHash 复算中枢的机器配置内容哈希（protocol.MachineConfigHash 的同一份算法）。
// 空 vars + 缺省 limits（total=1, perCapability={}）→ 归一成同一串 canonical JSON。
// 本套件用它让 mock 节点回一个**正确**的 config.applied（错误 hash 会被 grant 以
// config_not_applied 拒绝，那条边界在 ws_protocol 里钉）。
function machineConfigHash(vars = {}, total = 1, perCapability = {}) {
	const canonical = JSON.stringify({ vars, limits: { total, perCapability } });
	return "sha256:" + createHash("sha256").update(canonical).digest("hex");
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// pollUntil 是「等异步结论」的统一入口；超时抛错（返回 null 会让调用方漏断言）。
async function pollUntil(what, fn, { timeoutMs = 8000, intervalMs = 75 } = {}) {
	const deadline = Date.now() + timeoutMs;
	let last = null;
	for (;;) {
		last = await fn();
		if (last) return last;
		if (Date.now() > deadline) {
			throw new Error(`等待超时（${timeoutMs}ms）：${what}\n最后一次观察: ${JSON.stringify(last)}`);
		}
		await sleep(intervalMs);
	}
}

// mkMockNode 走真实产品路径登记节点 + 签令牌，再起一条真 WS 会话（v4 hello）。
// 返回 { name, id, token, conn, frames, welcome }。
async function mkMockNode(base, session, name) {
	const created = await H.req(base, "/v1/nodes", {
		session,
		body: V.envelope("Node", { name }, { namespaces: [NS] }),
	});
	V.expectStatus(`登记节点 ${name}`, created, 201);
	const id = created.json?.metadata?.uid;
	H.check(`节点 ${name} 有 uid`, !!id, String(id));

	const tok = await H.req(base, `/v1/nodes/${encodeURIComponent(name)}/tokens`, { session, body: {} });
	V.expectStatus(`签发 ${name} 的令牌`, tok, 200);
	const token = tok.json?.token || "";

	const conn = await N.wsConnect(base, token);
	const frames = [];
	conn.onFrame((f) => frames.push(f));
	conn.send(N.nodeHello({ name, token, namespaces: [NS], slots: 1 }));
	const welcome = await pollUntil(
		`节点 ${name} 收到 welcome`,
		async () => frames.find((f) => f.type === "welcome"),
		{ timeoutMs: 5000 },
	);
	return { name, id, token, conn, frames, welcome };
}

// deployTo 触发部署动作（nodes 是**节点名**数组，除非显式传 id）。
function deployTo(base, session, capName, nodes, extra = {}) {
	return H.req(base, V.P.capabilityDeploy(NS, capName), {
		session,
		method: "POST",
		body: { nodes, ...extra },
	});
}

// listDeployments 读部署事实读模型（v4：ns 路径 + 裸 {deployments:[…]}，不套信封）。
async function listDeployments(base, session, filter = {}) {
	const q = Object.entries(filter)
		.map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
		.join("&");
	const r = await H.req(base, V.P.deployments(NS) + (q ? `?${q}` : ""), { session });
	if (r.status !== 200) throw new Error(`GET deployments: ${r.status} ${String(r.text).slice(0, 300)}`);
	return r.json?.deployments || [];
}

// ---------- 1. 内置模板 ----------
async function partTemplates(base, session) {
	console.log("--- 1. GET /v1/capability-templates ---");
	const tpl = await H.req(base, V.P.capabilityTemplates(), { session });
	V.expectStatus("GET /v1/capability-templates → 200", tpl, 200);
	const templates = tpl.json?.templates || [];
	H.eq("内置模板正好 10 个（契约 §8 冻结）", templates.length, 10);
	H.eq("模板 id 集合与契约一致", templates.map((t) => t.id).sort(), [...TEMPLATE_IDS].sort());

	const missing = templates.filter(
		(t) =>
			!Object.prototype.hasOwnProperty.call(t, "fields") ||
			!Object.prototype.hasOwnProperty.call(t, "script") ||
			!Object.prototype.hasOwnProperty.call(t, "requires"),
	);
	H.eq("每个模板都有 fields / script / requires 三个键", missing.map((t) => t.id), []);
	const badScript = templates.filter((t) => typeof t.script !== "string" || !t.script.trim().startsWith("#!"));
	H.eq("每个模板的 script 都是带 shebang 的脚本文本", badScript.map((t) => t.id), []);
	const badFields = templates.filter((t) => t.fields !== null && !Array.isArray(t.fields));
	H.eq("fields 要么是数组、要么是 null", badFields.map((t) => t.id), []);
	const agentNoReq = templates.filter((t) => t.category === "agent" && t.id !== "agent.custom" && !(t.requires || []).length);
	H.eq("agent 类模板都声明了 requires（部署期预检）", agentNoReq.map((t) => t.id), []);
}

// ---------- 2/3. 建能力 + 内容寻址 ----------
async function partCreateAndAddressing(base, session) {
	console.log("--- 2/3. 建能力 → 产物 digest / 内容寻址 ---");
	const created = await V.createCapability(
		base,
		session,
		NS,
		V.capabilityBody({
			name: "dep.hello",
			displayName: "部署样例",
			run: { entrypoint: "run.sh", files: { "run.sh": OK_SCRIPT } },
		}),
	);
	V.expectStatus("POST …/capabilities 带 run → 201", created, 201);
	V.assertEnvelope("能力", created.json, "Capability");
	const cap = created.json || {};
	const digest = cap.status?.digest;
	const version = cap.metadata?.version;
	H.check("响应带内容寻址 digest（sha256:<64 hex>）", /^sha256:[0-9a-f]{64}$/.test(String(digest)), String(digest));
	H.check("metadata.version 是正整数", Number.isInteger(version) && version >= 1, String(version));
	H.eq("displayName 落库", cap.metadata?.displayName, "部署样例");
	H.check(
		"status.conditions 四类齐全（Implemented/NodesAvailable/DownstreamResolved/Deployed）",
		["Implemented", "NodesAvailable", "DownstreamResolved", "Deployed"].every((t) =>
			(cap.status?.conditions || []).some((c) => c.type === t),
		),
		JSON.stringify((cap.status?.conditions || []).map((c) => c.type)),
	);

	// 默认读面**不含**脚本正文：一次列表几十条会把页面拖成几百 KB。
	H.check(
		"默认读面剥离脚本正文（spec.run.files 是空对象，不是缺键）",
		cap.spec?.run?.files !== null &&
			typeof cap.spec?.run?.files === "object" &&
			Object.keys(cap.spec.run.files).length === 0,
		JSON.stringify(cap.spec?.run || {}).slice(0, 200),
	);
	const withBody = await H.req(base, `${V.P.capability(NS, "dep.hello")}?includeBody=true`, { session });
	V.expectStatus("?includeBody=true → 200", withBody, 200);
	H.eq(
		"?includeBody=true 时带回脚本正文（自包含产物）",
		withBody.json?.spec?.run?.files?.["run.sh"],
		OK_SCRIPT,
	);

	// 内容寻址：省略版本重复建 = 复用同一行（重试/双击不该在 append-only 表里长出一串行）
	const reup = await V.createCapability(
		base,
		session,
		NS,
		V.capabilityBody({
			name: "dep.hello",
			displayName: "部署样例",
			run: { entrypoint: "run.sh", files: { "run.sh": OK_SCRIPT } },
		}),
	);
	V.expectStatus("同内容省略版本重复建 → 201", reup, 201);
	H.eq("→ 复用同一行（uid 相同）", reup.json?.metadata?.uid, cap.metadata?.uid);
	H.eq("→ digest 相同（内容寻址）", reup.json?.status?.digest, digest);

	// 显式版本永不折叠，且 version 进 digest：同一份正文给 v2 是**新行 + 新 digest**。
	const nextVersion = version + 1;
	const reupV2 = await V.createCapability(
		base,
		session,
		NS,
		V.capabilityBody({
			name: "dep.hello",
			version: nextVersion,
			displayName: "部署样例",
			run: { entrypoint: "run.sh", files: { "run.sh": OK_SCRIPT } },
		}),
	);
	V.expectStatus(`同内容显式 v${nextVersion} → 201`, reupV2, 201);
	H.eq(`→ 新行是 v${nextVersion}`, reupV2.json?.metadata?.version, nextVersion);
	H.check(
		`→ version 进 digest：v${nextVersion} 与 v${version} 的 digest 不同`,
		typeof reupV2.json?.status?.digest === "string" && reupV2.json.status.digest !== digest,
		`${reupV2.json?.status?.digest} vs ${digest}`,
	);

	// 版本族：新版在前（降序），两条都在。
	const versions = await H.req(base, V.P.capabilityVersions(NS, "dep.hello"), { session });
	V.expectStatus("GET …/capabilities/{name}/versions → 200", versions, 200);
	const vs = (V.items(versions) || []).map((it) => it.metadata?.version);
	H.eq("版本族按整数降序", vs, [nextVersion, version]);
	const one = await H.req(base, V.P.capabilityVersion(NS, "dep.hello", version), { session });
	V.expectStatus("按版本取到那一行 → 200", one, 200);
	H.eq("按版本取到的 digest 就是 v1 的", one.json?.status?.digest, digest);

	// 部署走**裸名字**（= 该 name 的最新版）：这里把最新版的 version/digest 一起带出去，
	// 免得部署断言拿 v1 的 digest 去配 v2 的帧（那会看起来像「帧没发」）。
	const latestDigest = reupV2.json?.status?.digest || digest;
	H.check("最新版的 digest 也是 sha256 形状", /^sha256:[0-9a-f]{64}$/.test(String(latestDigest)), String(latestDigest));
	return { name: "dep.hello", version: nextVersion, digest: latestDigest, v1: { version, digest }, uid: cap.metadata?.uid };
}

// ---------- 4/5. 非法 / 缺实现 ----------
async function partRejectedCreates(base, session) {
	console.log("--- 4/5. 非法产物 / 缺实现 ---");
	{
		const bad = await V.createCapability(base, session, NS, {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: "dep.badshebang" },
			spec: { params: { type: "object" }, next: [], run: { entrypoint: "run.sh", files: { "run.sh": "echo no-shebang\n" } } },
		});
		// v4 的写期拒绝用的是 artifact 的封闭短码（capability.md §6.1），reason 直接是它。
		V.expectReason("入口无 shebang → 400 entrypoint_no_shebang", bad, 400, "entrypoint_no_shebang");
		H.check(
			"文案点名入口文件（运维能直接定位是哪份脚本）",
			/run\.sh/.test(String(bad.json?.message || "")) && /#!/.test(String(bad.json?.message || "")),
			String(bad.json?.message).slice(0, 200),
		);
		const list = await H.req(base, V.P.capabilities(NS), { session });
		const names = V.itemNames(list) || [];
		H.check("非法产物的能力**没有被建出来**（append-only 里不留半成品）", !names.includes("dep.badshebang"), JSON.stringify(names));
	}
	{
		const plain = await V.createCapability(base, session, NS, {
			apiVersion: V.API_VERSION,
			kind: "Capability",
			metadata: { name: "dep.noimpl" },
			spec: { params: { type: "object" }, next: [] },
		});
		// 注意：这里是**实现与契约措辞的分叉**。capability.md §6.1 的表把「没有 run」
		// 记成 impl_missing，但写路径先走 artifact.Normalize（run.files 至少一个文件），
		// 所以线上答案是 files_invalid。产品代码不在本卡范围内（只改 scripts/e2e/**），
		// 断言按**真实行为**钉死 400 + files_invalid，并已作为契约差异上报。
		V.expectReason("缺 run（没有实现）→ 400 files_invalid（契约写 impl_missing，实现没有该分支）", plain, 400, "files_invalid");
		H.check(
			"缺实现错误指向 run.files（可定位）",
			/run\.files/.test(String(plain.json?.message || "")),
			String(plain.json?.message).slice(0, 200),
		);
	}
}

// ---------- 6/7. 部署动作：授权面 / 按名字 / 按 id / 负例 ----------
//
// v4 的部署目标是「**生效命名空间**里有这台节点」才允许写部署意图：生效 =
// hello 申报 ∩ node_configs 授权。一台**从没连过**的节点两半都是空 → 403
// namespace_unauthorized。这不是权限 bug，而是「给一台还没见过面的机器推能力」
// 本来就不该成功（否则部署行会指向一台根本不存在的执行面）。
async function partDeployAuthorization(base, session, cap, neverSeenNode) {
	console.log("--- 6. 部署授权面：从没上线的节点 → 403 ---");
	const r = await deployTo(base, session, cap.name, [neverSeenNode.name]);
	V.expectReason("部署到从未上线的节点 → 403（生效 ns 为空）", r, 403, "Forbidden");
	H.check(
		"403 文案点名 namespace_unauthorized（可定位到「去授权/去上线」）",
		/namespace_unauthorized/.test(String(r.json?.message || "")),
		String(r.json?.message).slice(0, 240),
	);
	H.eq(
		"被拒的部署一行都没建（不留下一条永远推不出去的意图）",
		(await listDeployments(base, session)).length,
		0,
	);
}

// ---------- 7/8/9/10. 在线节点：真发帧 / 回执 / 内容通道 / 幂等 / 离线 ----------
async function partOnlineNodes(base, session, cap, onlineNode, otherNode) {
	console.log("--- 7. 在线节点：sent=true 且节点真收到帧 ---");
	// 节点先对齐机器配置：这是「能不能执行」的前置事实（ws_protocol 里钉错误 hash 的边界）。
	for (const n of [onlineNode, otherNode]) {
		n.conn.send({ type: "config.applied", revision: n.welcome?.configRevision, hash: machineConfigHash() });
	}
	await sleep(150);

	const dep = await deployTo(base, session, cap.name, [onlineNode.name]);
	V.expectStatus("部署到在线节点（按名字）→ 200", dep, 200);
	const outcome = (dep.json?.deployed || [])[0] || {};
	H.eq("部署响应回带解析出的节点 id", outcome.node, onlineNode.id);
	H.check("在线节点：sent=true（中枢真的把帧发出去了）", outcome.sent === true, JSON.stringify(outcome));
	H.eq("响应里的版本就是钉住的那一版", dep.json?.version, cap.version);

	// 节点侧证据：帧里的 refs 与部署响应/读模型是同一组事实。
	const frame = await pollUntil("节点收到 capability.deploy 帧", async () =>
		onlineNode.frames.find((f) => f.type === "capability.deploy" && f.digest === cap.digest),
	);
	H.eq("帧里的 ns 是 slug（不是 ns_ id）", frame.namespace, NS);
	H.eq("帧里的能力名", frame.name, cap.name);
	H.eq("帧里的版本", frame.version, cap.version);
	H.eq("帧里的 digest", frame.digest, cap.digest);
	H.check("帧带 requestId（回执按它配对，同一次重放保持同一个）", typeof frame.requestId === "string" && frame.requestId.length > 0, JSON.stringify(frame));

	const row = await pollUntil("部署行出现在读模型里", async () =>
		(await listDeployments(base, session, { nodeId: onlineNode.id })).find((d) => d.capability === cap.name) || null,
	);
	H.eq("刚推出去的部署行 state=pending（回执未到）", row.state, "pending");
	H.eq("行钉住的 digest 就是产物 digest", row.digest, cap.digest);
	H.check("行 id 与 requestId 是同一份事实", String(outcome.requestId || "").includes(row.id), `${outcome.requestId} vs ${row.id}`);

	console.log("--- 8. 内容通道（节点令牌）：200 / 403 / 404 / 405 ---");
	const content = await H.req(base, `/v1/node/capabilities/${cap.digest}`, { key: onlineNode.token });
	V.expectStatus("已部署节点取产物 → 200", content, 200);
	H.check(
		"产物是 canonical JSON（含 metadata.name/version）",
		content.json?.metadata?.name === cap.name && content.json?.metadata?.version === cap.version,
		JSON.stringify(content.json?.metadata),
	);
	H.eq("产物内联脚本正文（自包含，节点重算 digest 用同一份字节）", content.json?.spec?.run?.files?.["run.sh"], OK_SCRIPT);
	H.eq("产物禁止被缓存（no-store）", content.headers["cache-control"], "no-store");
	H.eq("产物禁止嗅探（nosniff）", content.headers["x-content-type-options"], "nosniff");

	// 另一台在线节点：内容在它的生效 ns 里可见，但**没有**部署给它 → 403。
	// 为什么不是 404：内容确实存在，缺的是给这台机器的部署意图（运维据此去点部署）。
	const other = await H.req(base, `/v1/node/capabilities/${cap.digest}`, { key: otherNode.token });
	V.expectReason("内容可见但未部署到这台节点 → 403", other, 403, "Forbidden");
	H.eq("403 文案指向「未部署」而不是「不存在」", other.json?.message, "capability is not deployed to this node");

	V.expectReason(
		"未知 digest → 404",
		await H.req(base, `/v1/node/capabilities/sha256:${"0".repeat(64)}`, { key: onlineNode.token }),
		404,
		"NotFound",
	);
	{
		const r = await H.req(base, `/v1/node/capabilities/${cap.digest}`, { key: onlineNode.token, method: "POST", body: {} });
		V.expectReason("内容通道非 GET → 405（不是 404：路径存在、方法不对）", r, 405, "MethodNotAllowed");
	}
	V.expectReason(
		"会话 Cookie 取产物 → 401（只认节点令牌）",
		await H.req(base, `/v1/node/capabilities/${cap.digest}`, { session }),
		401,
		"Unauthorized",
	);

	console.log("--- 9. 回执 applied → 行收敛 / status 派生一致 ---");
	onlineNode.conn.send({
		type: "capability.deployed",
		requestId: frame.requestId,
		namespace: NS,
		name: cap.name,
		version: cap.version,
		digest: cap.digest,
		status: "applied",
	});
	const applied = await pollUntil("部署行变成 applied（节点回报 capability.deployed）", async () => {
		const rows = await listDeployments(base, session, { nodeId: onlineNode.id });
		const hit = rows.find((d) => d.capability === cap.name);
		return hit && hit.state === "applied" ? hit : null;
	});
	H.eq("applied 行的 digest 就是产物 digest", applied.digest, cap.digest);

	const capDoc = await H.req(base, V.P.capability(NS, cap.name), { session });
	const deployedCond = (capDoc.json?.status?.conditions || []).find((c) => c.type === "Deployed");
	H.eq("能力 status.Deployed = True（同一份部署事实的另一个投影）", deployedCond?.status, "True");
	H.eq("Deployed 的 reason 是 Valid", deployedCond?.reason, "Valid");
	H.check(
		"能力 status.nodes[] 列出这台节点（deployed）",
		(capDoc.json?.status?.nodes || []).some((n) => n.name === onlineNode.name && n.state === "deployed"),
		JSON.stringify(capDoc.json?.status?.nodes),
	);

	console.log("--- 10. 幂等 / 失败回执 / 离线不再伪造发送 ---");
	// 按 **id** 再部署一次：同一 (节点, 能力) 只有一条期望，且会重推帧（#348 支持 id）。
	const byId = await deployTo(base, session, cap.name, [onlineNode.id]);
	V.expectStatus("对同一 (节点, 能力) 按 id 重复部署 → 200（幂等）", byId, 200);
	H.eq("按 id 解析出的还是这台节点", (byId.json?.deployed || [])[0]?.node, onlineNode.id);
	H.eq(
		"重复部署没有建出第二行（一个能力对一台节点只有一条期望）",
		(await listDeployments(base, session, { nodeId: onlineNode.id })).filter((d) => d.capability === cap.name).length,
		1,
	);

	// 失败回执必须让行**离开** applied：把「取不到产物」伪装成成功是这一层最坏的错。
	const otherDep = await deployTo(base, session, cap.name, [otherNode.name]);
	V.expectStatus("部署到第二台节点 → 200", otherDep, 200);
	const otherFrame = await pollUntil("第二台节点收到帧", async () =>
		otherNode.frames.find((f) => f.type === "capability.deploy" && f.digest === cap.digest),
	);
	otherNode.conn.send({
		type: "capability.deployed",
		requestId: otherFrame.requestId,
		namespace: NS,
		name: cap.name,
		version: cap.version,
		digest: cap.digest,
		status: "failed",
		error: "e2e: simulated fetch failure",
	});
	const failed = await pollUntil("失败回执让行落到 failed", async () => {
		const rows = await listDeployments(base, session, { nodeId: otherNode.id });
		const hit = rows.find((d) => d.capability === cap.name);
		return hit && hit.state === "failed" ? hit : null;
	});
	H.eq("失败回执 → 行 state=failed（不伪装成 applied）", failed.state, "failed");

	// 节点断连 = 会话消失。中枢必须承认「发不出去」：sent=false，且**不改写**最后一次
	// 可信回报（离线不等于「它没持有过这份产物」）。
	onlineNode.conn.close();
	await pollUntil(
		"中枢观察到节点离线（session 表里没了）",
		async () => {
			const r = await deployTo(base, session, cap.name, [onlineNode.name]);
			const o = (r.json?.deployed || [])[0];
			return o && o.sent === false ? o : null;
		},
		{ timeoutMs: 8000 },
	).then(
		(o) => H.check("节点离线后部署结论 sent=false（离线排队，不是失败）", o?.sent === false, JSON.stringify(o)),
		() => H.bad("节点离线后部署结论 sent=false（离线排队，不是失败）", "8s 内没有变成 sent=false"),
	);
	const rowAfterOffline = (await listDeployments(base, session, { nodeId: onlineNode.id })).find((d) => d.capability === cap.name);
	H.eq("离线不抹掉最后一次可信回报（state 仍是 applied）", rowAfterOffline?.state, "applied");

	console.log("--- 11. 部署事实读模型：只有读 ---");
	for (const method of ["POST", "DELETE", "PUT"]) {
		const r = await H.req(base, V.P.deployments(NS), { session, method, body: method === "DELETE" ? undefined : {} });
		V.expectReason(`${method} …/deployments → 405（写入口随 v3 退役）`, r, 405, "MethodNotAllowed");
	}
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log("=== SUITE: deploy（v4 部署模型）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	let nodes = [];
	try {
		const seeded = H.seed(server.dbPath, "deploy");
		const session = seeded.session;
		const nsId = await H.defaultNamespaceId(server.base, session);
		H.check("拿到默认命名空间 id", !!nsId, String(nsId));
		if (!nsId) throw new Error("解析不到默认命名空间 id（GET /v1/namespaces）");


		await partTemplates(server.base, session);
		const cap = await partCreateAndAddressing(server.base, session);
		await partRejectedCreates(server.base, session);

		// 从没上线的节点（只登记）：部署到它必须被授权面拒掉。
		const neverSeen = await H.req(server.base, "/v1/nodes", {
			session,
			body: V.envelope("Node", { name: "dep-never-seen" }, { namespaces: [NS] }),
		});
		V.expectStatus("登记（但从不连接的）节点", neverSeen, 201);
		await partDeployAuthorization(server.base, session, cap, {
			name: "dep-never-seen",
			id: neverSeen.json?.metadata?.uid,
		});

		// 两台在线节点：一台收部署（200 + 回执），一台做「内容可见但未部署」（403）
		// 与「失败回执」的对照组。
		const online = await mkMockNode(server.base, session, "dep-online-node");
		const other = await mkMockNode(server.base, session, "dep-other-node");
		nodes = [online, other];
		await partOnlineNodes(server.base, session, cap, online, other);
	} finally {
		for (const n of nodes) n.conn.close();
		server.stop();
	}

	const passed = H.summary(SUITE);
	process.exitCode = passed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e && e.stack ? e.stack : e).slice(0, 800));
	H.summary(SUITE);
	process.exit(1);
});
