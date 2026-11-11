#!/usr/bin/env node
/* SUITE: security — v4 安全矩阵（鉴权 / 隔离 / 凭据卫生 / 限流与背压）
 *
 * v4 把「谁在说话」拆成了**三类主体**，每一类有各自的凭据入口，混用必须失败：
 *
 *   会话 Cookie  → 控制台资源域（/v1/namespaces/{ns}/…、/v1/nodes、/v1/stats）
 *   API Key      → 机器入口（POST /v1/namespaces/{ns}/events，scope notify:send）
 *   节点 Token   → 节点通道（WS /v1/node/stream、GET /v1/node/capabilities/{digest}）
 *
 * v3 的扁平路径（/v1/events、/v1/tasks）在 v4 已退役：那些请求现在的答案是 404，
 * 而 404 是「路径不存在」，**不是**「权限不够」——拿它当鉴权断言会让「鉴权整体
 * 失效」也照样变绿。所以本套件的每条负向用例都断言**具体状态码 + Status.reason**：
 *   401 Unauthorized  没带凭据 / 凭据无效（该换一个）
 *   403 Forbidden     凭据有效但 scope 不够（该加权限）
 *   404 NotFound      不存在，或不属于这个命名空间（不泄露存在性）
 *
 * 覆盖：
 *   A. 会话端点：未登录 401、伪造会话 401、节点令牌不能当会话用
 *   B. 事件入口鉴权矩阵：send 200 / recv 403 / 无凭据 401 / 篡改 401 / 换前缀 401 / Basic 401
 *   C. 错误体卫生：任何拒绝都不回显呈上的凭据（token / key / session 明文）
 *   D. 命名空间隔离：资源按 ns 作用域，跨 ns 不可见（404，不泄露存在性）
 *   E. 内容通道（节点令牌）：401 无凭据 / 404 未知 digest（含形状不对的也算 404）
 *   F. 凭据落盘不可逆：DB（含 -wal/-shm）里搜不到明文 Key，且是 argon2id PHC
 *   G. 会话 Cookie：HttpOnly + 正确名字（logout 时下发清理）
 *   H. 注入与穿越：SQL 注入串不越权、不 5xx；路径穿越读不到站外文件
 *   I. 背压：超阈时**写入口** 503 + Retry-After，而**读模型全部照常 200**
 *   J. 钩子入口限流：超出额度 429 + Retry-After（不是 5xx、不是静默放行）
 *
 * 纪律：
 *   - 断言用状态码 + reason；只断状态码会让「换了个错误原因但码碰巧一样」漏过去。
 *   - 每个 server 独立起停（背压/限流要专属 env），退出前都 stop（残留 server 会
 *     把后续套件挤到别的端口，看起来像随机失败）。
 */
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";

import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

const SUITE = "security";
const NS = V.DEFAULT_NS;

// JSON 里的错误体不该出现任何呈上的凭据。判断用「原文是否出现在响应体里」——
// 比逐字段检查更硬：任何新增字段把 token 带出来都会被抓住。
function echoCheck(label, res, secrets) {
	const text = String(res.text || "");
	const leaked = secrets.filter((s) => s && text.includes(s));
	H.check(
		`${label} 的错误体不回显凭据`,
		leaked.length === 0,
		`响应里出现了 ${leaked.length} 个呈上的凭据明文：${text.slice(0, 240)}`,
	);
}

// 造一个至少能收事件的源（v4 ingress 必须有已启用的源：源名决定路由模板）。
async function mkSource(base, session, name, { kind = "manual", targets = [] } = {}) {
	const r = await V.createSource(
		base,
		session,
		NS,
		V.sourceBody({ name, kind, targets, enabled: true }),
	);
	V.expectStatus(`建源 ${name}`, r, 201);
	return r.json;
}

// 投递一条事件（机器入口）。
function postEvent(base, key, body = {}) {
	return V.ingestEvent(base, key, NS, {
		source: body.source || "sec-ingress",
		type: body.type || "sec.probe",
		payload: body.payload || { n: 1 },
	});
}

// ---------- A. 会话端点 ----------
async function partSessionAuth(base, seeded) {
	console.log("--- A. 会话端点：未登录 / 伪造会话 / 节点令牌不能当会话 ---");
	// 会话中间件（auth 层）与资源域用的是**两种**错误体：资源域的拒绝是 Status
	// （`{apiVersion,kind,status,code,reason,message}`），会话中间件的拒绝是更早的一层
	// `{"error": "…"}`（v3 遗留形状，v4 没有改）。所以这一类只钉状态码 + 「有可读错误
	// 文案 + 不回显凭据」，reason 只在资源域/ingress 的拒绝上断言。
	const expectSessionReject = (label, res) => {
		V.expectStatus(label, res, 401);
		H.check(`${label}：带可读错误体（不是空 body）`, typeof (res.json?.error || res.json?.reason) === "string" && (res.json?.error || res.json?.reason).length > 0, JSON.stringify(res.json).slice(0, 200));
	};
	for (const p of [V.P.tasks(NS), V.P.capabilities(NS), V.P.namespaces(), "/v1/nodes"]) {
		expectSessionReject(`未登录 GET ${p}`, await H.req(base, p));
	}
	{
		const r = await H.req(base, V.P.tasks(NS), { session: "sess_fake_does_not_exist" });
		expectSessionReject("伪造会话 → 401（会话不是客户端可自证的）", r);
		echoCheck("伪造会话", r, ["sess_fake_does_not_exist"]);
	}
	// 会话 Cookie 无效值（乱码 / SQL 注入串）都只能得到 401，不能落成 500。
	{
		const r = await H.req(base, V.P.tasks(NS), { session: "sess_fake.' OR '1'='1" });
		H.check("会话里的 SQL 注入串 → 401（不是 500）", r.status === 401, `${r.status} ${String(r.text).slice(0, 160)}`);
	}

	// 节点令牌是**机器身份**，不是会话：拿它打资源域必须 401（不能因为「长得像 Bearer」放行）。
	const nodeName = "sec-session-node";
	const created = await H.req(base, "/v1/nodes", {
		session: seeded.session,
		body: V.envelope("Node", { name: nodeName }, { namespaces: [NS] }),
	});
	V.expectStatus("登记节点", created, 201);
	const tok = await H.req(base, `/v1/nodes/${encodeURIComponent(nodeName)}/tokens`, {
		session: seeded.session,
		body: {},
	});
	V.expectStatus("签发节点令牌", tok, 200);
	const nodeToken = tok.json?.token || "";
	H.check("节点令牌明文只出现在签发响应里", nodeToken.length >= 20, `len=${nodeToken.length}`);
	{
		const r = await H.req(base, V.P.tasks(NS), { key: nodeToken });
		expectSessionReject("节点令牌不能当会话用（GET tasks → 401）", r);
		H.check(
			"节点令牌被拒时错误体不回显令牌",
			!String(r.text || "").includes(nodeToken),
			String(r.text).slice(0, 200),
		);
	}
	{
		const r = await postEvent(base, nodeToken);
		V.expectReason("节点令牌不能当 API Key 投递（→ 401）", r, 401, "Unauthorized");
	}
	return { nodeName, nodeToken };
}

// ---------- B. 事件入口鉴权矩阵 ----------
async function partIngressMatrix(base, seeded) {
	console.log("--- B. 事件入口鉴权矩阵 ---");
	await mkSource(base, seeded.session, "sec-ingress");

	const ok = await postEvent(base, seeded.sendKey);
	V.expectReason("send Key 投递 → 200", ok, 200);
	H.check("投递回执带真 eventId", typeof ok.json?.eventId === "string" && ok.json.eventId.length > 0, JSON.stringify(ok.json).slice(0, 200));

	const forbidden = await postEvent(base, seeded.recvKey);
	V.expectReason("recv Key 投递 → 403（scope 不够，不是 401）", forbidden, 403, "Forbidden");
	echoCheck("recv Key 403", forbidden, [seeded.recvKey]);

	const anon = await postEvent(base, undefined);
	V.expectReason("无 Authorization 头 → 401", anon, 401, "Unauthorized");

	const tampered = seeded.sendKey.slice(0, -4) + (seeded.sendKey.endsWith("aaaa") ? "bbbb" : "aaaa");
	const tamperedRes = await postEvent(base, tampered);
	V.expectReason("篡改尾部 → 401", tamperedRes, 401, "Unauthorized");
	echoCheck("篡改 Key 401", tamperedRes, [tampered]);

	// 前缀是 key 的**类型**标记：把 send 改成别的种类不是「权限不足」，是无效凭据。
	const wrongPrefix = seeded.sendKey.replace(/^ant_send_/, "ant_live_");
	V.expectReason(
		"改前缀 → 401（凭据类型标记属于凭据本身）",
		await postEvent(base, wrongPrefix),
		401,
		"Unauthorized",
	);

	const basic = await H.req(base, V.P.events(NS), {
		headers: { Authorization: "Basic " + seeded.sendKey },
		body: { source: "sec-ingress", type: "sec.probe", payload: {} },
	});
	V.expectReason("Basic scheme → 401（只认 Bearer）", basic, 401, "Unauthorized");
	echoCheck("Basic 401", basic, [seeded.sendKey]);

	// 停用的源不接收投递：404（与「源不存在」同码不同文案，不泄露别的 ns 的源）
	{
		const r = await H.req(base, `/v1/hooks/${"x".repeat(16)}`, { body: { a: 1 } });
		V.expectReason("未知 hook token → 404（不是 200）", r, 404, "NotFound");
	}
}

// ---------- C/D. 命名空间隔离 ----------
async function partNamespaceIsolation(base, seeded) {
	console.log("--- D. 命名空间隔离（跨 ns 不可见 → 404） ---");
	const cap = await V.createCapability(
		base,
		seeded.session,
		NS,
		V.capabilityBody({ name: "sec-iso-cap" }),
	);
	V.expectStatus("建能力（default ns）", cap, 201);
	await mkSource(base, seeded.session, "sec-iso-src");

	// 第二个命名空间：slug 是路径段（不是 ns_ id）。
	const ns2 = await V.createNamespace(base, seeded.session, "sec-iso-second", "隔离用");
	V.expectStatus("建第二个命名空间", ns2, 201);
	const slug2 = ns2.json?.metadata?.name || "sec-iso-second";
	H.eq("第二个命名空间的对外名就是请求的 slug", slug2, "sec-iso-second");

	// 同一份资源在另一个 ns 里不存在：**404**（与「不存在」同码，不泄露它在别处存在）。
	V.expectReason(
		"能力跨 ns 查询 → 404（不泄露存在性）",
		await H.req(base, V.P.capability(slug2, "sec-iso-cap"), { session: seeded.session }),
		404,
		"NotFound",
	);
	V.expectReason(
		"源跨 ns 查询 → 404",
		await H.req(base, V.P.source(slug2, "sec-iso-src"), { session: seeded.session }),
		404,
		"NotFound",
	);
	{
		const list = await H.req(base, V.P.capabilities(slug2), { session: seeded.session });
		V.expectStatus("新 ns 的能力列表 → 200", list, 200);
		const names = V.itemNames(list);
		H.check("新 ns 的能力列表是 []（不是 null、也不带别 ns 的行）", Array.isArray(names) && names.length === 0, JSON.stringify(names));
	}
	{
		const ev = await H.req(base, V.P.events(slug2), { session: seeded.session });
		V.expectStatus("新 ns 的事件列表 → 200", ev, 200);
		H.check("新 ns 的事件列表是 []（事件按 ns 隔离）", (V.items(ev) || []).length === 0, JSON.stringify(V.items(ev)));
	}
	V.expectReason(
		"不存在的命名空间 → 404",
		await H.req(base, V.P.capabilities("no-such-namespace"), { session: seeded.session }),
		404,
		"NotFound",
	);
	// v3 的扁平路径已退役：它现在是 404（路径不存在），**不是** 200 也不是 401。
	// 这条断言是「迁移没做完」的机械守卫：谁把扁平别名加回来，这里立刻红。
	for (const legacy of [V.P.legacyEvents(), V.P.legacyCapabilities()]) {
		const r = await H.req(base, legacy, { session: seeded.session });
		H.check(`v3 扁平路径 ${legacy} 已退役（404，不做兼容分支）`, r.status === 404, `${r.status} ${String(r.text).slice(0, 160)}`);
	}
	return { slug2 };
}

// ---------- E. 内容通道 ----------
async function partContentChannel(base, nodeToken, seeded) {
	console.log("--- E. 内容通道（节点令牌）：401 / 404 ---");
	const digest = "sha256:" + "0".repeat(64);
	V.expectReason(
		"无令牌取能力包体 → 401",
		await H.req(base, `/v1/node/capabilities/${digest}`),
		401,
		"Unauthorized",
	);
	V.expectReason(
		"未知 digest → 404（内容通道不泄露全局存在性）",
		await H.req(base, `/v1/node/capabilities/${digest}`, { key: nodeToken }),
		404,
		"NotFound",
	);
	//
	// 形状不对的 digest 也是 404：形状校验在授权之后，错误体不能变成「这是个坏请求」
	// 那种把内部判据泄露出去的形状。
	V.expectReason(
		"形状不对的 digest → 404（不是 400）",
		await H.req(base, "/v1/node/capabilities/not-a-digest", { key: nodeToken }),
		404,
		"NotFound",
	);
	// 会话 Cookie 不是节点身份：资源域之外没有会话中间件，所以这里是 401 而不是 200。
	V.expectReason(
		"会话 Cookie 取能力包体 → 401（内容通道只认节点令牌）",
		await H.req(base, `/v1/node/capabilities/${digest}`, { session: seeded.session }),
		401,
		"Unauthorized",
	);
}

// ---------- F. 凭据落盘 ----------
function partCredentialAtRest(server, seeded) {
	console.log("--- F. 凭据落盘不可逆 ---");
	// SQLite WAL 模式下新写的数据可能还在 -wal 里，所以搜整个库目录下的同名前缀文件。
	const dir = path.dirname(server.dbPath);
	const base = path.basename(server.dbPath);
	const bufs = readdirSync(dir)
		.filter((f) => f.startsWith(base))
		.map((f) => readFileSync(path.join(dir, f)));
	const contains = (needle) => bufs.some((b) => b.includes(needle));
	H.check("DB（含 -wal/-shm）不含明文 send Key", !contains(seeded.sendKey), "明文 send Key 出现在库里");
	H.check("DB 不含明文 recv Key", !contains(seeded.recvKey), "明文 recv Key 出现在库里");
	// 注：会话 id 是**服务端会话表**的主键（明文存），它是 v3 起的既有设计，不是
	// v4 迁移的一部分；本套件只钉「API Key 这类长期凭据必须不可逆」。
	H.check("DB 含 argon2id PHC 标记（key 用慢哈希存）", contains("$argon2id$"), "未找到 $argon2id$");
}

// ---------- G. 会话 Cookie 属性 ----------
async function partCookieAttributes(base, seeded) {
	console.log("--- G. 会话 Cookie 属性 ---");
	// logout 会吊销这个会话，所以放最后（本套件后面不再用这个 session）。
	const res = await fetch(base + "/v1/auth/logout", {
		method: "POST",
		headers: { Cookie: "anotify_session=" + seeded.session },
		redirect: "manual",
	});
	const setCookie = res.headers.get("set-cookie") || "";
	H.check("logout 下发 Set-Cookie", setCookie.length > 0, `set-cookie: ${setCookie}`);
	H.check("Set-Cookie 含 HttpOnly", /httponly/i.test(setCookie), `set-cookie: ${setCookie}`);
	H.check("Set-Cookie 指定 anotify_session", setCookie.includes("anotify_session"), `set-cookie: ${setCookie}`);
}

// ---------- H. 注入 / 穿越 ----------
async function partInjection(base, seeded) {
	console.log("--- H. SQL 注入 / 路径穿越 ---");
	{
		const r = await H.req(base, "/v1/auth/register/options", {
			body: { username: "admin' OR '1'='1", displayName: "x" },
		});
		H.check("注入串当用户名 → 不 5xx", r.status !== 500, `got ${r.status}`);
	}
	{
		const r = await postEvent(base, seeded.sendKey, {
			payload: { title: "'; DROP TABLE users;--" },
		});
		V.expectReason("注入串当 payload 投递 → 200（按数据处理）", r, 200);
		const after = await H.req(base, V.P.tasks(NS), { session: seeded.session });
		V.expectReason("注入之后服务仍正常（会话可用）", after, 200);
	}
	for (const p of ["/../etc/passwd", "/%2e%2e/%2e%2e/etc/passwd", "/..%2f..%2fetc%2fpasswd", "/static/../../etc/passwd"]) {
		const r = await H.req(base, p);
		const leaked = String(r.text || "").includes("root:") || String(r.text || "").includes("/bin/bash");
		H.check(`路径穿越 ${p} 读不到站外文件（${r.status}）`, !leaked, `status=${r.status} body=${String(r.text).slice(0, 120)}`);
	}
}

// ---------- I. 背压 ----------
// 背压的判据是「最老 pending 任务的等待时长」（#228），所以要直插一条老 pending 行：
// 真路径需要「节点声明了能力但槽位吃满」的时序，那是不稳定的。
// 断言两件事：
//   ① 写入口（events / sources fire）变成 503 + Retry-After，body 是 backpressure；
//   ② **读模型全部照常 200**（tasks / capabilities / stats / console events）——
//      背压只拒新写入，不许把「我现在看不了自己的队列」当保护。
async function partBackpressure() {
	console.log("--- I. 背压：写入口 503，读模型不受影响 ---");
	const server = await H.startServer({
		suiteName: SUITE,
		portOffset: 1,
		rpId: "localhost",
		extraEnv: { ANOTIFY_PENDING_MAX_AGE_SEC: "1" },
	});
	try {
		const seeded = H.seed(server.dbPath, "sec-bp");
		const nsId = await H.defaultNamespaceId(server.base, seeded.session);
		H.check("拿到默认命名空间 id", !!nsId, String(nsId));

		const cap = await V.createCapability(
			server.base,
			seeded.session,
			NS,
			V.capabilityBody({ name: "sec-bp-cap" }),
		);
		V.expectStatus("背压组：建能力", cap, 201);
		const capId = cap.json?.metadata?.uid;
		const digest = cap.json?.status?.digest;
		H.check("能力带 uid + digest（造 pending 行需要）", !!capId && /^sha256:/.test(String(digest)), `${capId} ${digest}`);

		await mkSource(server.base, seeded.session, "sec-bp-src", {
			targets: [{ capability: "sec-bp-cap" }],
		});
		// 一条等了 1 小时的 pending：阈值是 1s，所以采样最迟 5s 后必然超阈。
		V.seedPendingTask(server.dbPath, {
			namespaceId: nsId,
			capabilityId: capId,
			capability: "sec-bp-cap",
			version: cap.json?.metadata?.version || 1,
			digest,
			ageSec: 3600,
		});

		// 轮询等背压生效（采样是后台的：idle 5s / fast 1s）。
		const rejected = await V.poll(
			"背压生效（写入口返回 503 + backpressure）",
			async () => {
				const r = await V.ingestEvent(server.base, seeded.sendKey, NS, {
					source: "sec-bp-src",
					type: "sec.bp",
					payload: {},
				});
				return r.status === 503 ? r : null;
			},
			{ timeoutMs: 15000, intervalMs: 250 },
		);
		H.eq("背压拒绝的 body.error 是 backpressure", rejected.json?.error, "backpressure");
		H.check(
			"背压拒绝带 retry-after 头（可执行的退避提示）",
			Number(rejected.headers["retry-after"]) >= 1,
			JSON.stringify(rejected.headers),
		);
		H.check(
			"背压 body 给出 pending 年龄与阈值（可观测）",
			typeof rejected.json?.pending_age_sec === "number" && typeof rejected.json?.max_pending_age_sec === "number",
			JSON.stringify(rejected.json),
		);
		H.eq("背压响应禁止缓存（no-store）", rejected.headers["cache-control"], "no-store");

		// sources fire 也是写入口：同样被拒（保护覆盖全部写路径，不只是 events）。
		const fired = await V.fireSource(server.base, seeded.session, NS, "sec-bp-src", {});
		H.eq("背压下 POST …/sources/{name}/fire → 503", fired.status, 503);

		// 读模型：全部照常 200（这是本条用例的核心断言）。
		for (const [label, p] of [
			["tasks", V.P.tasks(NS)],
			["capabilities", V.P.capabilities(NS)],
			["sources", V.P.sources(NS)],
			["events", V.P.events(NS)],
			["deployments", V.P.deployments(NS)],
			["stats", V.P.stats(NS)],
			["console events", V.P.consoleEvents(NS)],
		]) {
			const r = await H.req(server.base, p, { session: seeded.session });
			V.expectStatus(`背压下读模型 ${label} 仍 200`, r, 200);
		}
		// 背压不该篡改已有的读模型：那条 pending 任务仍然看得见（孤儿视图是处置入口）。
		const orphans = await H.req(server.base, V.P.orphans(NS), { session: seeded.session });
		V.expectStatus("背压下孤儿视图仍 200", orphans, 200);
	} finally {
		server.stop();
	}
}

// ---------- J. 钩子入口限流 ----------
async function partHookRateLimit() {
	console.log("--- J. 钩子入口限流：429 + Retry-After ---");
	const server = await H.startServer({
		suiteName: SUITE,
		portOffset: 2,
		rpId: "localhost",
		// 每 token 每分钟 1 次、每 IP 每分钟 1 次：第二个请求必然撞限流之一。
		extraEnv: { ANOTIFY_HOOK_RATE_PER_MIN: "1", ANOTIFY_HOOK_RATE_PER_IP_PER_MIN: "1" },
	});
	try {
		const seeded = H.seed(server.dbPath, "sec-hook");
		const src = await V.createSource(
			server.base,
			seeded.session,
			NS,
			V.sourceBody({ name: "sec-hook-src", kind: "webhook", enabled: true }),
		);
		V.expectStatus("建 webhook 源", src, 201);
		const hookUrl = src.json?.spec?.config?.hookUrl || "";
		H.check("webhook 源的读投影带 hookUrl（授权读路径）", /^\/v1\/hooks\//.test(hookUrl), hookUrl);
		// 写请求不能回写只读投影：hookUrl 是服务端派生，客户端写它必须 400（不是静默忽略）。
		const writeBack = await V.createSource(
			server.base,
			seeded.session,
			NS,
			V.sourceBody({ name: "sec-hook-write", kind: "webhook", config: { hookUrl } }),
		);
		V.expectReason("写请求里出现只读投影 hookUrl → 400 unknown_field", writeBack, 400, "unknown_field");

		const first = await H.req(server.base, hookUrl, { body: { hello: 1 } });
		V.expectStatus("第一次 hook 投递 → 200", first, 200);
		const second = await H.req(server.base, hookUrl, { body: { hello: 2 } });
		H.eq("超出额度 → 429（限流生效）", second.status, 429);
		H.check(
			"429 带 retry-after 头",
			Number(second.headers["retry-after"]) >= 1,
			JSON.stringify(second.headers),
		);
		echoCheck("429", second, [hookUrl.split("/").pop()]);
	} finally {
		server.stop();
	}
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log("=== SUITE: security（v4 安全矩阵）===");
	const server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	const seeded = H.seed(server.dbPath, "sectest");

	const { nodeToken } = await partSessionAuth(server.base, seeded);
	await partIngressMatrix(server.base, seeded);
	await partNamespaceIsolation(server.base, seeded);
	await partContentChannel(server.base, nodeToken, seeded);
	partCredentialAtRest(server, seeded);
	await partInjection(server.base, seeded);
	await partCookieAttributes(server.base, seeded);

	server.stop();

	await partBackpressure();
	await partHookRateLimit();

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
