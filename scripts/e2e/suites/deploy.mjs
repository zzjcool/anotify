#!/usr/bin/env node
/* SUITE: deploy — 部署模型的控制台端点（console-declares / 契约 §11 + §7）
 *
 * 纯 HTTP（不起 Chrome）：这一层验的是**中枢自己的承诺**——
 *   能力定义（+ run 块）→ 编译成内容寻址的产物（digest）→ 部署到选中的机器 →
 *   部署状态机（pending / applied / failed / local）与撤销。
 *
 * 为什么这些必须进 e2e 而不是只留单测：
 *   单测各自钉住一个函数（校验规则、SQL、handler 分支），但「建能力顺手编译产物」
 *   到「部署行建成 pending」到「hello 对账把行改成 applied / 发现本地变体」是**跨
 *   进程**的三段，任何一段没接上都会表现为「控制台上看起来推过了，节点其实没跑」。
 *
 * 覆盖（每条都对应契约里的一句话）：
 *   1. GET /v1/capability-templates → 9 个模板，每个都有 fields/script/requires
 *   2. POST /v1/capabilities 带 run → 响应带 digest；GET /v1/artifacts 查得到
 *   3. 同一份内容重复建 → digest 相同（内容寻址的幂等语义）
 *   4. 非法 run（入口无 shebang）→ 400 invalid_artifact，且**能力没有被建出来**
 *   5. 部署到离线节点 → 200 且 pushed:false，行仍然建出来（state=pending）
 *   6. 部署没有产物的能力 → 404 artifact_not_found
 *   7. GET /v1/deployments?nodeId= 过滤生效
 *   8. DELETE /v1/deployments?id= → 行消失
 *   9. 本地变体（§7）：hello 报不同 digest → state=local，reason 含两个 digest
 *
 * 纪律：不睡固定时长，一律「轮询 + 超时」（部署结果来自节点帧，天生异步）。
 */
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const SUITE = "deploy";

// 契约 CONTRACT-artifact §8 冻结的 9 个模板 id。写死是有意的：
// 模板是**代码生成器的输入**，少一个/多一个都意味着控制台表单与后端脱节。
const TEMPLATE_IDS = [
	"script.blank",
	"script.http",
	"script.git",
	"script.test",
	"script.notify",
	"agent.claude",
	"agent.codex",
	"agent.pi",
	"agent.custom",
];

// 一份合法产物用的脚本：直接 exec（不经 shell），所以入口首行必须是 shebang。
const OK_SCRIPT = "#!/bin/bash\nset -euo pipefail\necho hello\n";

let server, session, nsId;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// pollUntil 轮询到 fn 返回真值；到点返回 null 由调用方断言（不静默吞错）。
async function pollUntil(fn, { timeoutMs = 6000, intervalMs = 50 } = {}) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		const v = await fn();
		if (v) return v;
		if (Date.now() > deadline) return null;
		await sleep(intervalMs);
	}
}

// mkCapability 建一个带 run 的能力，返回 {id,name,version,digest}。
async function mkCapability(name, { script = OK_SCRIPT, outputs = [], requires = [], version } = {}) {
	const body = {
		name,
		outputs,
		requires,
		run: { entrypoint: "run.sh", files: { "run.sh": script } },
	};
	if (version) body.version = version;
	const r = await H.req(server.base, "/v1/capabilities", { session, body });
	if (r.status !== 200) throw new Error(`建能力 ${name} 失败: ${r.status} ${r.text}`);
	return { ...r.json.capability, digest: r.json.digest };
}

// listDeployments 读部署行（可带 nodeId / capabilityId 过滤）。
async function listDeployments(filter = {}) {
	const q = Object.entries(filter)
		.map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
		.join("&");
	const r = await H.req(server.base, "/v1/deployments" + (q ? "?" + q : ""), { session });
	if (r.status !== 200) throw new Error(`GET /v1/deployments: ${r.status} ${r.text}`);
	return r.json?.deployments || [];
}

// findDeployment 取某条部署行（id 不存在时返回 null）。
async function findDeployment(id, filter = {}) {
	const rows = await listDeployments(filter);
	return rows.find((d) => d.id === id) || null;
}

// waitDeployment 轮询等某条行满足条件（部署状态只由节点帧驱动）。
async function waitDeployment(id, pred, timeoutMs = 6000) {
	let last = null;
	await pollUntil(async () => {
		last = await findDeployment(id);
		return last && pred(last);
	}, { timeoutMs });
	return last;
}

async function main() {
	H.startTimer();
	server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	const seeded = H.seed(server.dbPath, "deploy");
	session = seeded.session;
	nsId = await H.defaultNamespaceId(server.base, session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	// ================= 1. 内置模板（契约 §8） =================
	console.log("--- 1. GET /v1/capability-templates ---");
	const tpl = await H.req(server.base, "/v1/capability-templates", { session });
	H.eq("GET /v1/capability-templates → 200", tpl.status, 200);
	const templates = tpl.json?.templates || [];
	H.eq("内置模板正好 9 个（契约 §8 冻结）", templates.length, 9);
	H.eq("模板 id 集合与契约一致", templates.map((t) => t.id).sort(), [...TEMPLATE_IDS].sort());
	{
		// 每个模板必须自带「表单字段 / 脚本模板 / 预装依赖」三样：
		// 少了 fields，控制台表单无从渲染；少了 script，渲染不出产物；
		// 少了 requires，部署期的依赖预检就永远显示“没问题”。
		const missing = templates.filter(
			(t) =>
				!Object.prototype.hasOwnProperty.call(t, "fields") ||
				!Object.prototype.hasOwnProperty.call(t, "script") ||
				!Object.prototype.hasOwnProperty.call(t, "requires"),
		);
		H.eq("每个模板都有 fields / script / requires 三个键", missing.map((t) => t.id), []);
		const badScript = templates.filter(
			(t) => typeof t.script !== "string" || !t.script.trim().startsWith("#!"),
		);
		H.eq("每个模板的 script 都是带 shebang 的脚本文本", badScript.map((t) => t.id), []);
		const badFields = templates.filter((t) => t.fields !== null && !Array.isArray(t.fields));
		H.eq("fields 要么是数组、要么是 null（不返回别的形状）", badFields.map((t) => t.id), []);
		// agent 类模板必须声明 requires：它们的脚本是 `claude --print ...`，
		// 机器上没装就应该在**部署期**失败，而不是等第一个任务跑挂。
		const agentTpls = templates.filter((t) => t.category === "agent" && t.id !== "agent.custom");
		const agentNoReq = agentTpls.filter((t) => !(t.requires || []).length);
		H.eq("agent 类模板都声明了 requires（部署期预检）", agentNoReq.map((t) => t.id), []);
	}

	// ================= 2. 建能力即编译产物 =================
	console.log("--- 2. POST /v1/capabilities 带 run → 产物 ---");
	const cap = await mkCapability("dep.hello", { outputs: ["go"] });
	H.check(
		"建带 run 的能力：响应带 digest（sha256:<64 hex>）",
		/^sha256:[0-9a-f]{64}$/.test(cap.digest || ""),
		String(cap.digest),
	);
	H.eq("建带 run 的能力：kind 仍是 capability 的推导量", cap.name, "dep.hello");
	const arts = await H.req(server.base, `/v1/artifacts?capabilityId=${encodeURIComponent(cap.id)}`, {
		session,
	});
	H.eq("GET /v1/artifacts?capabilityId= → 200", arts.status, 200);
	const artRows = arts.json?.artifacts || [];
	H.eq("产物列表里有刚建的那份 digest", artRows.map((a) => a.digest), [cap.digest]);
	{
		// spec 是 canonical JSON（base64 进 JSON）：它必须能独立解析成一份完整产物，
		// 否则「拷到别的机器直接跑」这句话就不成立。
		let parsed = null;
		try {
			parsed = JSON.parse(Buffer.from(artRows[0]?.spec || "", "base64").toString("utf8"));
		} catch {
			parsed = null;
		}
		H.eq("产物 spec 是可解析的 canonical JSON", parsed?.kind, "Capability");
		H.eq("产物 spec 带 metadata.name/version", [parsed?.metadata?.name, parsed?.metadata?.version], ["dep.hello", cap.version]);
		H.check(
			"产物 spec 内联了入口文件内容（自包含）",
			typeof parsed?.spec?.run?.files?.["run.sh"] === "string",
			JSON.stringify(parsed?.spec?.run || {}).slice(0, 160),
		);
	}

	// ================= 3. 内容寻址的幂等语义 =================
	console.log("--- 3. 同一份内容重复建 → digest 稳定 ---");
	{
		// 3a. 同内容、同版本：append-only 的幂等返回（不该 409，也不该生成第二份产物）
		const again = await H.req(server.base, "/v1/capabilities", {
			session,
			body: {
				name: "dep.hello",
				version: cap.version,
				outputs: ["go"],
				run: { entrypoint: "run.sh", files: { "run.sh": OK_SCRIPT } },
			},
		});
		H.eq("同内容同版本重复建 → 200（幂等，不是 409）", again.status, 200);
		H.eq("同内容同版本重复建 → digest 相同", again.json?.digest, cap.digest);
		// 3b. 同内容、新版本：内容寻址意味着「同一份内容」永远对应同一个 digest。
		// 中枢把请求归一到已有版本（append-only 不重复插行），响应里的 digest 必须
		// 与首次一致 —— 否则同一个产物会有两个身份，节点上的缓存索引就对不上了。
		const nextVer = cap.version + 1;
		const reup = await H.req(server.base, "/v1/capabilities", {
			session,
			body: {
				name: "dep.hello",
				version: nextVer,
				outputs: ["go"],
				run: { entrypoint: "run.sh", files: { "run.sh": OK_SCRIPT } },
			},
		});
		H.eq(`同内容换版本（v${nextVer}）重复建 → 200`, reup.status, 200);
		H.eq(
			`同内容换版本（v${nextVer}）→ digest 与首次相同（内容寻址）`,
			reup.json?.digest,
			cap.digest,
		);
		const artRows2 = (
			await H.req(server.base, `/v1/artifacts?capabilityId=${encodeURIComponent(cap.id)}`, {
				session,
			})
		).json?.artifacts || [];
		H.eq("重复建没有造出第二份产物（仍然只有 1 条 digest）", artRows2.length, 1);
	}

	// ================= 4. 非法产物：400 且不留半个能力 =================
	console.log("--- 4. 非法 run（入口无 shebang）---");
	{
		// 执行方式是**直接 exec**（不经 shell），所以入口没有 shebang 就根本跑不起来。
		// 关键不只是 400，而是「能力不能被建出来」：append-only 的能力表里，
		// 半建出来的定义永远改不掉、也永远跑不起来。
		const bad = await H.req(server.base, "/v1/capabilities", {
			session,
			body: {
				name: "dep.badshebang",
				run: { entrypoint: "run.sh", files: { "run.sh": "echo no-shebang\n" } },
			},
		});
		H.eq("入口无 shebang → 400", bad.status, 400);
		H.eq("非法产物 → code=invalid_artifact", bad.json?.code, "invalid_artifact");
		H.check(
			"错误信息点名 entrypoint_no_shebang（不是泛泛的 400）",
			/entrypoint_no_shebang/.test(bad.json?.error || ""),
			String(bad.json?.error).slice(0, 200),
		);
		const capsNow = await H.req(server.base, "/v1/capabilities", { session });
		const names = (capsNow.json?.capabilities || []).map((c) => c.name);
		H.check(
			"非法产物的能力**没有被建出来**（append-only 里不留半成品）",
			!names.includes("dep.badshebang"),
			JSON.stringify(names),
		);
	}

	// ================= 5. 部署到离线节点 =================
	// 离线不是错误：行必须先建好（pending），等节点上线时补推。
	// 如果这里返回 4xx 或干脆不建行，运维就会看到「部署失败」，而节点其实只是没开。
	console.log("--- 5. 部署到离线节点 ---");
	const offline = N.seedNodeRows(server.dbPath, nsId, "dep-offline-node", [], 1);
	const online = N.seedNodeRows(server.dbPath, nsId, "dep-online-node", [], 1);
	const depOff = await H.req(server.base, "/v1/deployments", {
		session,
		body: { capabilityId: cap.id, nodeIds: [offline.id] },
	});
	H.eq("部署到离线节点 → 200（不是错误）", depOff.status, 200);
	const offItem = (depOff.json?.deployments || [])[0] || {};
	H.eq("离线节点：pushed=false", offItem.pushed, false);
	H.check(
		"离线节点：error 说明原因（node_offline）",
		/node_offline|offline/i.test(offItem.error || ""),
		String(offItem.error),
	);
	H.check("离线节点：deployId 仍然发出来了（行已建）", !!offItem.deployId, JSON.stringify(offItem));
	const offRow = await findDeployment(offItem.deployId);
	H.eq("离线节点的行 state=pending（等上线补推）", offRow?.state, "pending");
	H.eq("离线节点的行 digest 正确", offRow?.digest, cap.digest);

	// ================= 6. 没有产物的能力 → 404 =================
	console.log("--- 6. 部署没有产物的能力 ---");
	{
		const plain = await H.req(server.base, "/v1/capabilities", {
			session,
			body: { name: "dep.noartifact", outputs: [] },
		});
		H.eq("建一个纯定义能力（不带 run）→ 200", plain.status, 200);
		const dep = await H.req(server.base, "/v1/deployments", {
			session,
			body: { capabilityId: plain.json?.capability?.id, nodeIds: [offline.id] },
		});
		H.eq("部署没有产物的能力 → 404", dep.status, 404);
		H.eq("→ code=artifact_not_found", dep.json?.code, "artifact_not_found");
	}

	// ================= 7. 按 nodeId 过滤 =================
	console.log("--- 7. GET /v1/deployments?nodeId= ---");
	{
		const all = await listDeployments();
		const filtered = await listDeployments({ nodeId: offline.id });
		H.check("部署列表非空（前置）", all.length >= 1, JSON.stringify(all.map((d) => d.id)));
		H.eq("按 nodeId 过滤只留下这台机器的行", filtered.every((d) => d.nodeId === offline.id), true);
		H.eq("按 nodeId 过滤条数 = 该机器行数", filtered.length, all.filter((d) => d.nodeId === offline.id).length);
		H.check(
			"过滤不会把别的机器的行带出来（另一台此刻 0 行）",
			(await listDeployments({ nodeId: online.id })).length === 0,
			JSON.stringify((await listDeployments({ nodeId: online.id })).map((d) => d.id)),
		);
	}

	// ================= 8. 撤销部署 =================
	console.log("--- 8. DELETE /v1/deployments?id= ---");
	{
		const del = await H.req(server.base, `/v1/deployments?id=${encodeURIComponent(offItem.deployId)}`, {
			session,
			method: "DELETE",
		});
		H.eq("DELETE /v1/deployments?id= → 200", del.status, 200);
		H.eq("响应回被删的 id", del.json?.deleted, offItem.deployId);
		H.eq("行真的消失了", await findDeployment(offItem.deployId), null);
	}

	// ================= 9. 本地变体（契约 §7） =================
	//
	// 这条是整个模型的核心机制：**每个「节点 × 能力」永远有唯一归属**。
	//   · 节点缓存的 digest == 中枢期望 → applied（托管态，中枢可覆盖）
	//   · 节点缓存的 digest != 中枢期望 → local（本地变体，中枢永不覆盖，UI 标黄）
	// 判定只发生在节点 hello 的对账里（节点在部署后 / 重连时如实上报缓存），
	// 所以这里用真 WS 握手模拟节点的两次 hello。
	console.log("--- 9. 本地变体（hello 报不同 digest）---");
	{
		const node = await N.bringUpNode(server.base, server.dbPath, nsId, "dep-var-node", [], 1);
		H.check(
			"节点上线（hello → welcome）",
			node.frames.some((f) => f.type === "welcome"),
			JSON.stringify(node.frames).slice(0, 200),
		);
		const dep = await H.req(server.base, "/v1/deployments", {
			session,
			body: { capabilityId: cap.id, nodeIds: [node.id] },
		});
		const item = (dep.json?.deployments || [])[0] || {};
		H.eq("在线节点：pushed=true", item.pushed, true);

		// ① 节点如实上报「我缓存的就是这份」→ applied
		node.conn.send(N.nodeHello(node.id, [{ name: cap.name, version: cap.version, digest: cap.digest }], 1));
		const appliedRow = await waitDeployment(item.deployId, (r) => r.state === "applied");
		H.eq("hello 报的 digest 与期望一致 → state=applied", appliedRow?.state, "applied");
		H.eq("applied 的 reason 为空（正常态不留噪音）", appliedRow?.reason, "");

		// ② 节点报一个**不同**的 digest（= 用户手改过产物）→ local
		const localDigest = "sha256:" + "b".repeat(64);
		node.conn.send(
			N.nodeHello(node.id, [{ name: cap.name, version: cap.version, digest: localDigest }], 1),
		);
		const localRow = await waitDeployment(item.deployId, (r) => r.state === "local");
		H.eq("hello 报的 digest 不一致 → state=local（本地变体）", localRow?.state, "local");
		H.check(
			"local 的 reason 同时包含两个 digest（节点上的 + 中枢期望的）",
			(localRow?.reason || "").includes(localDigest) && (localRow?.reason || "").includes(cap.digest),
			String(localRow?.reason),
		);
		// 本地变体不能被「再部署一次」覆盖（中枢永不覆盖）：
		// 重复部署只更新期望值，节点仍报旧 digest → 仍然 local。
		const dep2 = await H.req(server.base, "/v1/deployments", {
			session,
			body: { capabilityId: cap.id, nodeIds: [node.id] },
		});
		H.eq("对同一 (节点, 产物) 重复部署 → 200（幂等：仍是一行）", dep2.status, 200);
		H.eq(
			"重复部署没有建出第二行（唯一键在 node×artifact 上）",
			(await listDeployments({ nodeId: node.id })).length,
			1,
		);
		node.conn.close();
	}

	server.stop();
	// 定时器兜底：套件本体已经跑完，exitCode 让进程自然退出（避免截断 stdout）。
	const allPassed = H.summary(SUITE);
	process.exitCode = allPassed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch((e) => {
	console.error(e);
	H.bad("套件异常终止", String(e).slice(0, 300));
	if (server) server.stop();
	process.exit(1);
});
