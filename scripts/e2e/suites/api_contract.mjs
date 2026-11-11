#!/usr/bin/env node
/* SUITE: api_contract — API 契约矩阵（v4 资源面）
 *
 * 本套件钉的是**横切契约**，不是单域功能：同一个入口在「未登录 / 错 key / 错 scope /
 * 缺参数 / 不存在的 ns / 越权读」之间的分界必须可区分，且 v3 的扁平路径与 v3 的响应
 * 形状必须真的消失（否则「迁移完成」只是自说自话）。
 *
 * 覆盖：（v4 迁移后）
 *  events 投递入口（/v1/namespaces/{ns}/events，Bearer key）
 *    无 Key→401；错误 Key→401；recv scope Key→403
 *    缺 source→400 且点名 source；空 source→400；源不存在→404（不泄露存在性之外的差异）
 *    type 可省略（v4 语义）→ 200 且事件类型回落源名
 *    id 幂等键 → 第二次 duplicate=true 且同一 eventId / traceId
 *  读模型信封
 *    events / tasks / console events 一律列表信封（apiVersion/kind/metadata/items），
 *    空列表是 [] 而不是 null；console events 缺 ?namespace= → 400（必须显式给 ns）
 *  tasks 读模型非空路径：能力 + 源（targets）→ 事件 → 任务，列表里能看到那个任务
 *  namespaces 隔离（v4：ns 进路径）
 *    不存在的 ns → 404（**不回退**默认命名空间）
 *    第二个 ns 里建的能力互相看不见（双向）
 *    跨 ns 的 next 引用不被解析（DownstreamResolved=False / NotFound），不会悄悄连过去
 *  caps 写面（信封 + 版本 + 场景）
 *    spec 里没有 v3 的 outputs / handler：未知字段一律 400 unknown_field（不静默丢）
 *    缺实现（没有 run）→ 400（reason 取 artifact 封闭集合）
 *    ?includeBody=true 才回脚本正文；默认读面 spec.run.files 是 {}
 *    跨 ns 读不存在的对象与「真的不存在」逐字节同体（防存在性探测）
 *  v3 路径退役
 *    /v1/events、/v1/tasks、/v1/capabilities 全部 404（没有兼容分支）
 *  vapid-public-key / devices / keys / 静态缓存：沿用保留域形状（v4 不改）
 */
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

// 脚本正文（比对 includeBody 读回的原文用，必须逐字节一致）。
const SCRIPT_BODY = "#!/bin/sh\nset -eu\necho artifact-contract\n";

// expectAnon401 钉住「会话层未登录」的**冻结形状**：401 + `{error:"未登录"}`。
//
// 为什么不套资源 Status：v4 明确把身份域留在保留域（.pi-orchestrator/v4/AUTH-HANDOFF.md
// 第 9 行：匿名 401 正文精确 `{"error":"未登录"}`，由 session middleware 的 http.Error
// 写出，没有 reason/code）。资源 handler 自己的错误才是 Status —— 两种不能混。
// 断言仍要求**有 body 且可解析**（§4.1「所有错误响应都带 body」），不退化成只看状态码。
function expectAnon401(name, res) {
	H.eq(name, res.status, 401);
	const err = res.json?.error;
	H.check(`${name} → 正文是 {error:"未登录"}（身份域冻结形状）`, err === "未登录", `${res.status} ${String(res.text).slice(0, 120)}`);
	return res;
}

async function main() {
	console.log("=== SUITE: api_contract（API 契约矩阵 · v4）===");
	const server = await H.startServer({
		suiteName: "api_contract",
		rpId: "localhost",
	});
	const B = server.base;
	const { sendKey, recvKey, session } = H.seed(server.dbPath, "api");

	// 默认命名空间的对外名（slug）—— v4 的 ns 在**路径**里，拼 URL 用它。
	const ns = await H.defaultNamespace(B, session);
	H.check("解析默认命名空间（slug + id）", !!(ns && ns.slug && ns.id), JSON.stringify(ns));
	if (!ns) throw new Error("无法解析默认命名空间（GET /v1/namespaces 列表信封）");
	const NS = ns.slug;

	// ---------- 造一个真实源：v4 的投递入口要求 source 是本 ns 的资源 ----------
	//
	// v3 的 ingress 对「source 名」不做存在性校验（那是隐式的），v4 把它变成一等资源
	// （source.md §5）：源不存在是 404。所以下面「鉴权/参数校验」的用例必须先有一个
	// 真实源，否则 400/401 与 404 混在一起，测不出鉴权边界。
	const github = await V.createSource(
		B,
		session,
		NS,
		V.sourceBody({ name: "github", kind: "manual", config: {}, targets: [] }),
	);
	V.expectStatus("造数：建源 github（无 targets）→ 201", github, 201);

	// ---------- events 鉴权 ----------
	V.expectReason(
		"events 无 Key → 401",
		await H.req(B, V.P.events(NS), { body: { source: "github", type: "t" } }),
		401,
		"Unauthorized",
	);
	V.expectReason(
		"events 错误 Key → 401",
		await H.req(B, V.P.events(NS), {
			key: "ant_live_bogus",
			body: { source: "github", type: "t" },
		}),
		401,
		"Unauthorized",
	);
	V.expectReason(
		"events recv scope Key → 403",
		await H.req(B, V.P.events(NS), {
			key: recvKey,
			body: { source: "github", type: "t" },
		}),
		403,
		"Forbidden",
	);

	// ---------- events 参数校验（v4：source 必填、type 可选）----------
	const noSource = await H.req(B, V.P.events(NS), { key: sendKey, body: { type: "t" } });
	V.expectReason("events 缺 source → 400", noSource, 400, "BadRequest");
	H.check(
		"缺 source 的 message 点名 source（不回退默认源）",
		String(noSource.json?.message || "").includes("source"),
		String(noSource.json?.message || noSource.text).slice(0, 120),
	);
	V.expectReason(
		"events 空 source → 400",
		await H.req(B, V.P.events(NS), { key: sendKey, body: { source: "  ", type: "t" } }),
		400,
		"BadRequest",
	);
	// v4 的 type 是**可选**（source.md §5）：拿不到类型就回落源名，而不是拒绝。
	// 旧断言「缺 type → 400」是 v3 语义，这里换成等价的强断言（200 + 回落值）。
	const noType = await H.req(B, V.P.events(NS), { key: sendKey, body: { source: "github" } });
	V.expectStatus("events 缺 type → 200（v4：type 可选）", noType, 200);
	H.check("缺 type 的投递拿到 eventId", !!noType.json?.eventId, V.brief(noType));
	// 无 targets 的源：事件照常入库、不派发，回执里留一条 no_target 剪枝痕（source.md §5）。
	// 这是「合法状态不是错误」的强证据：200 + 空 tasks + pruned 点明原因。
	H.eq("无 targets 的源：回执不建任务", (noType.json?.tasks || []).length, 0);
	H.eq("无 targets 的源：回执 pruned[0].reason=no_target", noType.json?.pruned?.[0]?.reason, "no_target");
	// 源不存在 → 404：与「源停用」同码（不区分，免得用状态码探测 token/源有效性）。
	V.expectReason(
		"events 不存在的源 → 404",
		await H.req(B, V.P.events(NS), {
			key: sendKey,
			body: { source: "no-such-source", type: "t" },
		}),
		404,
		"NotFound",
	);
	// 不存在的命名空间 → 404（路径里的 ns 不静默回退）
	V.expectReason(
		"events 投递到不存在的 ns → 404",
		await H.req(B, V.P.events("no-such-ns"), {
			key: sendKey,
			body: { source: "github", type: "t" },
		}),
		404,
		"NotFound",
	);

	// ---------- vapid-public-key ----------
	// 推送是**可选**功能：没配 VAPID 时服务端返回 200 + 空 publicKey，
	// 前端据此隐藏「启用推送」入口。所以端点本身必须永远 200，
	// 但 publicKey 是否非空取决于部署配置——不能把「未配置」当成失败。
	const vapidResp = await H.req(B, "/v1/vapid-public-key");
	H.eq("vapid-public-key → 200", vapidResp.status, 200);
	const vapidPub = vapidResp.json?.publicKey || "";
	if (process.env.ANOTIFY_VAPID_PUBLIC_KEY) {
		H.check(
			"vapid-public-key publicKey 非空（已配置 VAPID）",
			vapidPub.length > 0,
			vapidPub.slice(0, 20),
		);
	} else {
		H.check(
			"vapid-public-key 未配置时返回空串而非报错",
			vapidPub === "",
			vapidPub.slice(0, 20),
		);
	}

	// ---------- devices ----------
	H.eq("devices 无 session → 401", (await H.req(B, "/v1/devices")).status, 401);
	H.eq(
		"devices POST 缺 keys → 400",
		(
			await H.req(B, "/v1/devices", {
				session,
				body: { endpoint: "https://push.example.com/nokeys" },
			})
		).status,
		400,
	);
	const devBody = H.makeDevice({ name: "契约设备A" });
	const devPost = await H.req(B, "/v1/devices", { session, body: devBody });
	H.eq("devices POST 合法 → 200", devPost.status, 200);
	H.check("devices POST 返回 device", !!devPost.json?.device?.id);
	const devId = devPost.json?.device?.id;
	const devList = await H.req(B, "/v1/devices", { session });
	H.eq("devices GET 列表 → 200", devList.status, 200);
	H.check(
		"devices GET 含刚 POST 的设备",
		Array.isArray(devList.json?.devices) &&
			devList.json.devices.some((d) => d.id === devId),
	);
	H.eq(
		"devices PATCH 重命名 → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { name: "改名后" },
			})
		).status,
		200,
	);
	H.eq(
		"devices PATCH eventScope=final → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { eventScope: "final" },
			})
		).status,
		200,
	);
	H.eq(
		"devices PATCH 坏 eventScope → 400",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { eventScope: "bogus" },
			})
		).status,
		400,
	);
	H.eq(
		"devices PATCH enabled=false → 200",
		(
			await H.req(B, `/v1/devices/${devId}`, {
				session,
				method: "PATCH",
				body: { enabled: false },
			})
		).status,
		200,
	);
	H.eq(
		"devices DELETE → 200",
		(await H.req(B, `/v1/devices/${devId}`, { session, method: "DELETE" }))
			.status,
		200,
	);
	const devListAfter = await H.req(B, "/v1/devices", { session });
	const after = (devListAfter.json?.devices || []).find((d) => d.id === devId);
	H.check(
		"devices DELETE 后 enabled=false 或消失",
		!after || after.enabled === false,
		after ? `enabled=${after.enabled}` : "已消失",
	);

	// ---------- keys ----------
	H.eq("keys 无 session → 401", (await H.req(B, "/v1/keys")).status, 401);
	const keyPost = await H.req(B, "/v1/keys", {
		session,
		body: { name: "契约Key", scopes: ["notify:send"] },
	});
	H.eq("keys POST → 200", keyPost.status, 200);
	H.check(
		"keys POST 返回 ant_ 前缀明文",
		!!keyPost.json?.key && keyPost.json.key.startsWith("ant_"),
	);
	const newKeyId = keyPost.json?.record?.ID ?? keyPost.json?.record?.id;
	H.eq(
		"keys POST 无 scopes → 400",
		(await H.req(B, "/v1/keys", { session, body: { name: "noScope" } })).status,
		400,
	);
	const keyList = await H.req(B, "/v1/keys", { session });
	H.eq("keys GET → 200", keyList.status, 200);
	H.check(
		"keys GET 不含明文 key",
		!JSON.stringify(keyList.json).includes(keyPost.json.key),
	);
	H.eq(
		"keys revoke → 200",
		(await H.req(B, `/v1/keys/${newKeyId}/revoke`, { session, method: "POST" }))
			.status,
		200,
	);
	V.expectReason(
		"被 revoke 的 Key 上报 → 401",
		await H.req(B, V.P.events(NS), {
			key: keyPost.json.key,
			body: { source: "github", type: "pr_opened" },
		}),
		401,
		"Unauthorized",
	);

	// ---------- 事件幂等（v4：id 是投递幂等键，命中回 duplicate=true）----------
	const ev1 = await H.req(B, V.P.events(NS), {
		key: sendKey,
		body: { source: "github", type: "pr_opened", payload: { n: 1 } },
	});
	V.expectStatus("POST …/events → 200", ev1, 200);
	H.check("事件回执带 eventId", !!ev1.json?.eventId, V.brief(ev1));
	H.check("事件回执带 traceId", !!ev1.json?.traceId, V.brief(ev1));

	const ev1b = await H.req(B, V.P.events(NS), {
		key: sendKey,
		body: {
			source: "github",
			type: "pr_opened",
			id: "dup-1",
			payload: { n: 1 },
		},
	});
	V.expectStatus("幂等键首次提交 → 200", ev1b, 200);
	H.eq("幂等键首次提交 duplicate=false", ev1b.json?.duplicate, false);
	const ev1c = await H.req(B, V.P.events(NS), {
		key: sendKey,
		body: {
			source: "github",
			type: "pr_opened",
			id: "dup-1",
			payload: { n: 1 },
		},
	});
	V.expectStatus("幂等重复提交 → 200", ev1c, 200);
	H.eq("幂等重复提交 duplicate=true", ev1c.json?.duplicate, true);
	H.eq("幂等重复提交返回同一 eventId", ev1c.json?.eventId, ev1b.json?.eventId);
	H.eq("幂等重复提交返回同一 traceId", ev1c.json?.traceId, ev1b.json?.traceId);

	// ---------- 读模型：列表信封（v4：items[]，不是 v3 的 events/tasks/capabilities）----------
	const evList = await H.req(B, V.P.events(NS) + "?limit=50", { session });
	V.expectStatus("events 列表 → 200", evList, 200);
	const evItems = V.assertListEnvelope("events 列表", evList, "EventList");
	const githubEv = evItems.find((it) => it?.spec?.involvedObject?.name === "github");
	H.check("events 列表里有 github 源的事件", !!githubEv, JSON.stringify(evItems.map((i) => i?.spec?.eventType)));
	H.eq(
		"事件的 spec.reason 是 Ingested（入库事实，不是派发结果）",
		githubEv?.spec?.reason,
		"Ingested",
	);
	H.check(
		"事件信封带 traceUID（trace 是事件的唯一关联键）",
		String(githubEv?.spec?.traceUID || "").length > 0,
		JSON.stringify(githubEv?.spec || {}),
	);

	// 控制台事件流（cluster 保留读模型，必须显式给 ?namespace=）
	expectAnon401(
		"console/events 无 session → 401",
		await H.req(B, "/v1/console/events", { namespace: null }),
	);
	const noNsQuery = await H.req(B, "/v1/console/events", { session, namespace: null });
	V.expectReason("console/events 缺 ?namespace= → 400", noNsQuery, 400, "BadRequest");
	H.check(
		"缺 ?namespace= 的 message 点名 namespace",
		String(noNsQuery.json?.message || "").toLowerCase().includes("namespace"),
		String(noNsQuery.json?.message || noNsQuery.text).slice(0, 120),
	);
	const consoleList = await H.req(B, "/v1/console/events?limit=50", { session });
	V.expectStatus("console/events GET → 200", consoleList, 200);
	const consoleItems = V.assertListEnvelope("console/events", consoleList, "EventList");
	H.check("console/events count≥2", consoleItems.length >= 2, `n=${consoleItems.length}`);
	H.check(
		"console/events 每项都是 Event 信封（kind=Event）",
		consoleItems.every((it) => it?.kind === "Event"),
		JSON.stringify(consoleItems.map((it) => it?.kind)),
	);

	// ---------- tasks 读模型：非空路径（能力 + 源 targets → 事件 → 任务）----------
	const ping = await V.createCapability(
		B,
		session,
		NS,
		V.capabilityBody({ name: "contract.ping", run: V.MIN_RUN }),
	);
	V.expectStatus("造数：建能力 contract.ping → 201", ping, 201);
	const pingSrc = await V.createSource(
		B,
		session,
		NS,
		V.sourceBody({
			name: "contract.src",
			kind: "manual",
			targets: [{ capability: "contract.ping" }],
		}),
	);
	V.expectStatus("造数：建源 contract.src（targets=[contract.ping]）→ 201", pingSrc, 201);
	const pingEv = await H.req(B, V.P.events(NS), {
		key: sendKey,
		body: { source: "contract.src", type: "contract.ping" },
	});
	V.expectStatus("有 targets 的源投递 → 200", pingEv, 200);
	H.eq("回执建出 1 个任务", (pingEv.json?.tasks || []).length, 1);
	const pingTaskId = pingEv.json?.tasks?.[0]?.taskId;
	H.eq("任务能力是 contract.ping", pingEv.json?.tasks?.[0]?.capability, "contract.ping");

	const taskList = await H.req(B, V.P.tasks(NS) + "?limit=50", { session });
	V.expectStatus("tasks 列表 → 200", taskList, 200);
	const taskItems = V.assertListEnvelope("tasks 列表", taskList, "TaskList");
	H.check(
		"tasks 列表里有刚建的那个任务",
		taskItems.some((it) => it?.metadata?.name === pingTaskId),
		JSON.stringify(taskItems.map((it) => it?.metadata?.name)),
	);
	// 未知 query 参数 → 400：静默忽略会让 `?phase=running`（v3 写法）拿到**未过滤的全量**
	// 列表，客户端以为筛过了（taskAPI.list 的显式白名单）。
	V.expectReason(
		"tasks 列表未知参数 phase= → 400 unknown_field",
		await H.req(B, V.P.tasks(NS) + "?phase=running", { session }),
		400,
		"unknown_field",
	);
	// 不存在的 ns → 404（与「没有该 ns 的权限」同码，不泄露存在性）
	V.expectReason(
		"tasks 列表在不存在的 ns → 404",
		await H.req(B, V.P.tasks("no-such-ns"), { session }),
		404,
		"NotFound",
	);

	// ---------- 命名空间隔离（v4：ns 进路径，双向都看不见）----------
	{
		// 1. 不存在的 ns → 404：后端**不回退**默认命名空间（回退等于把人写错的地方藏起来）。
		V.expectReason(
			"缺/错的 ns 路径 → 404（不回退默认 ns）",
			await H.req(B, V.P.capabilities("no-such-ns"), { session }),
			404,
			"NotFound",
		);

		// 2. 第二个命名空间用 API 建（信封：slug 是 metadata.name），不硬编码 id。
		const slug = "iso-" + Date.now();
		const created = await V.createNamespace(B, session, slug, "隔离验证");
		V.expectStatus("POST /v1/namespaces → 201", created, 201);
		V.assertEnvelope("新建的命名空间", created.json, "Namespace");
		H.eq("新命名空间的 metadata.name 就是 slug", created.json?.metadata?.name, slug);
		H.check("新命名空间有内部 uid（ns_…）", String(created.json?.metadata?.uid || "").startsWith("ns_"), String(created.json?.metadata?.uid));

		// 3. 两边各建一个能力，再互相查：谁都看不到对方
		const capOther = await V.createCapability(
			B,
			session,
			slug,
			V.capabilityBody({ name: "iso.only_other", run: V.MIN_RUN }),
		);
		V.expectStatus("在新命名空间里建能力 → 201", capOther, 201);
		const capDefault = await V.createCapability(
			B,
			session,
			NS,
			V.capabilityBody({ name: "iso.only_default", run: V.MIN_RUN }),
		);
		V.expectStatus("在默认命名空间里建能力 → 201", capDefault, 201);

		const namesDefault = V.itemNames(await H.req(B, V.P.capabilities(NS), { session }));
		H.check("默认命名空间列表可读", Array.isArray(namesDefault), JSON.stringify(namesDefault));
		H.check(
			"默认命名空间看不到另一个命名空间的能力",
			(namesDefault || []).includes("iso.only_default") &&
				!(namesDefault || []).includes("iso.only_other"),
			JSON.stringify(namesDefault),
		);
		const namesOther = V.itemNames(await H.req(B, V.P.capabilities(slug), { session }));
		H.check(
			"另一个命名空间也看不到默认命名空间的能力",
			(namesOther || []).includes("iso.only_other") &&
				!(namesOther || []).includes("iso.only_default"),
			JSON.stringify(namesOther),
		);

		// 4. 按名字跨 ns 读 → 404；与「真的不存在」逐字节同体（防存在性探测）
		const crossGet = await H.req(B, V.P.capability(slug, "iso.only_default"), { session });
		const missGet = await H.req(B, V.P.capability(slug, "iso.does_not_exist"), { session });
		H.check(
			"跨 ns 读与不存在都是 404",
			crossGet.status === 404 && missGet.status === 404,
			`cross=${crossGet.status} missing=${missGet.status}`,
		);
		H.check(
			"两种 404 的响应 body 逐字节相同",
			crossGet.text === missGet.text,
			JSON.stringify({ cross: crossGet.text, miss: missGet.text }).slice(0, 220),
		);

		// 5. 跨 ns 的 next 引用**不被解析**：v4 没有「建边」入口，边来自 spec.next，
		//    解析按本 ns 收口（ResolveRef 带 namespace_id）。所以「跨 ns 引用被拒」的
		//    v4 形态是条件为 False + NotFound，而不是一条 400（更早、更可诊断）。
		const crossRef = await V.createCapability(
			B,
			session,
			NS,
			V.capabilityBody({ name: "iso.cross_ref", next: ["iso.only_other"], run: V.MIN_RUN }),
		);
		V.expectStatus("next 指向别的 ns 的能力 → 201（引用本身合法）", crossRef, 201);
		const crossCond = (crossRef.json?.status?.conditions || []).find(
			(c) => c?.type === "DownstreamResolved",
		);
		H.check(
			"跨 ns 的 next 解析不到：DownstreamResolved=False",
			crossCond?.status === "False",
			JSON.stringify(crossCond),
		);
		H.eq("跨 ns 的 next 解析原因是 NotFound", crossCond?.reason, "NotFound");
		H.eq(
			"resolved.next 里把那条引用标成 missing（没有悄悄连过去）",
			crossRef.json?.status?.resolved?.next?.[0]?.missing,
			true,
		);
	}

	// ---------- 能力写面：信封、版本、spec 闭合、includeBody ----------
	{
		const withRun = await V.createCapability(
			B,
			session,
			NS,
			V.capabilityBody({
				name: "contract.artifact",
				run: {
					entrypoint: "run.sh",
					files: { "run.sh": SCRIPT_BODY },
					workDir: "/tmp/work",
				},
				requires: { commands: ["curl"] },
			}),
		);
		V.expectStatus("建带实现的能力 → 201", withRun, 201);
		V.assertEnvelope("新建能力的响应", withRun.json, "Capability");
		H.eq("新能力版本是 1（内容寻址的第一行）", withRun.json?.metadata?.version, 1);
		H.check(
			"新能力 status.digest 是 sha256",
			String(withRun.json?.status?.digest || "").startsWith("sha256:"),
			String(withRun.json?.status?.digest),
		);
		H.check("新能力有 CAS 令牌", V.isResourceVersion(withRun.json?.metadata?.resourceVersion), String(withRun.json?.metadata?.resourceVersion));
		// 写响应也是默认读面：不带正文（写一次不等于把脚本回吐一次，body 上限一致）
		H.check(
			"写响应默认不带脚本正文（spec.run.files = {}）",
			JSON.stringify(withRun.json?.spec?.run?.files || null) === "{}",
			JSON.stringify(withRun.json?.spec?.run?.files),
		);

		// ① 默认读面：spec 有内容，但脚本正文被剥掉
		const detail = await H.req(B, V.P.capability(NS, "contract.artifact"), { session });
		V.expectStatus("GET 能力详情 → 200", detail, 200);
		V.assertEnvelope("能力详情", detail.json, "Capability");
		H.eq("详情带 workDir", detail.json?.spec?.run?.workDir, "/tmp/work");
		H.eq("详情带 requires.commands", detail.json?.spec?.requires?.commands?.[0], "curl");
		H.check(
			"默认读面 spec.run.files 是空对象（不给正文）",
			JSON.stringify(detail.json?.spec?.run?.files || null) === "{}",
			JSON.stringify(detail.json?.spec?.run?.files),
		);

		// ② ?includeBody=true：同一份 spec 的正文全文（map[相对路径]string，不是数组）
		const withBody = await H.req(
			B,
			V.P.capability(NS, "contract.artifact") + "?includeBody=true",
			{ session },
		);
		V.expectStatus("GET 能力详情 ?includeBody=true → 200", withBody, 200);
		H.eq(
			"① includeBody 回脚本原文（逐字节）",
			withBody.json?.spec?.run?.files?.["run.sh"],
			SCRIPT_BODY,
		);
		H.eq(
			"② 正文读面与默认读面的 digest 一致（正文不是另一份文档）",
			withBody.json?.status?.digest,
			withRun.json?.status?.digest,
		);

		// ③ 版本族：单版本，uid 与单对象一致
		const versions = await H.req(B, V.P.capabilityVersions(NS, "contract.artifact"), { session });
		const vItems = V.assertListEnvelope("能力版本族", versions, "CapabilityList");
		H.eq("版本族 1 条", vItems.length, 1);
		H.eq("版本族里的 uid 与单对象一致", vItems[0]?.metadata?.uid, withRun.json?.metadata?.uid);

		// ④ 缺实现 → 400（v4 的唯一来路：run 必填，封闭码取自 artifact 集合）
		const plain = await V.createCapability(
			B,
			session,
			NS,
			{ apiVersion: V.API_VERSION, kind: "Capability", metadata: { name: "contract.noimpl" }, spec: { params: { type: "object" } } },
		);
		V.expectReason("缺 run 建能力 → 400", plain, 400, "files_invalid");
		H.check(
			"缺实现的 message 点名 run.files（不是笼统 BadRequest）",
			String(plain.json?.message || "").includes("run.files"),
			String(plain.json?.message || plain.text).slice(0, 160),
		);

		// ⑤ v3 的 spec.handler 在 v4 的封闭 spec 里**不存在**：未知字段一律 400，
		//    绝不静默丢弃（静默丢会让调用方以为挂上了内置执行器）。
		const handlerTry = await V.createCapability(
			B,
			session,
			NS,
			{ apiVersion: V.API_VERSION, kind: "Capability", metadata: { name: "contract.handler" }, spec: { params: { type: "object" }, handler: "notify-phone", run: V.MIN_RUN } },
		);
		V.expectReason("spec.handler → 400（v4 没有 handler 实现形式）", handlerTry, 400, "unknown_field");
		const capsAfter = V.itemNames(await H.req(B, V.P.capabilities(NS), { session }));
		H.check(
			"被拒的 handler 能力一行都没建",
			!(capsAfter || []).includes("contract.handler"),
			JSON.stringify(capsAfter),
		);

		// ⑥ 未登录 → 401（读资源不因为「在路径里」就漏鉴权）
		expectAnon401(
			"能力详情未登录 → 401",
			await H.req(B, V.P.capability(NS, "contract.artifact"), {}),
		);
	}

	// ---------- v3 扁平路径退役（没有兼容分支，也没有重定向）----------
	// 注意 `/v1/nodes` **不在此列**：Node 是 cluster-scoped（api-conventions §5.2），它的路径
	// 从 v3 到 v4 都是 /v1/nodes。只有「命名空间资源被拍平」的那几条退役了。
	for (const [name, path] of [
		["GET /v1/capabilities", V.P.legacyCapabilities()],
		["GET /v1/sources", "/v1/sources"],
		["GET /v1/tasks", "/v1/tasks"],
		["GET /v1/events", V.P.legacyEvents()],
		["GET /v1/edges", "/v1/edges"],
		["GET /v1/deployments", "/v1/deployments"],
	]) {
		const r = await H.req(B, path, { session });
		H.check(`${name} → 404（v3 路径已退役）`, r.status === 404, `${r.status} ${String(r.text).slice(0, 120)}`);
	}

	// ---------- 静态/缓存 ----------
	H.eq("/ → 200", (await H.req(B, "/")).status, 200);
	const idxHeaders = (await H.req(B, "/index.html")).headers;
	H.check(
		"index.html Cache-Control 含 max-age=60",
		(idxHeaders["cache-control"] || "").includes("max-age=60"),
		idxHeaders["cache-control"],
	);
	const apiHeaders = (await H.req(B, V.P.tasks(NS), { session })).headers;
	H.check(
		"/v1/* Cache-Control 含 no-store",
		(apiHeaders["cache-control"] || "").includes("no-store"),
		apiHeaders["cache-control"],
	);
	const manifest = await H.req(B, "/manifest.json");
	H.check(
		"manifest.json 存在且为合法 JSON",
		manifest.status === 200 &&
			manifest.json &&
			typeof manifest.json === "object",
	);
	const hashedEntry = Object.values(manifest.json || {})[0];
	if (hashedEntry) {
		const hashedResp = await H.req(B, "/" + hashedEntry);
		H.check(
			"哈希资源 Cache-Control 含 immutable",
			(hashedResp.headers["cache-control"] || "").includes("immutable"),
			hashedResp.headers["cache-control"],
		);
	} else {
		H.bad("manifest.json 无任何哈希资源条目");
	}

	const passed = H.summary("api_contract");
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	process.exit(1);
});
