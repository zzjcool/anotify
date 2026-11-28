#!/usr/bin/env node
/* SUITE: deeplink — 点击通知 → message.html?id=<taskId> 展示任务详情
 *
 * #80 之后旧「通知/消息」模型已下线（/v1/notify、/v1/notifications 删除），
 * 深链的目标从「消息详情」变成「任务详情」。v4 迁移后覆盖（锚点随 #335 前端重写换到
 * 新 DOM，断言强度不降反升：改成判精确值 / 精确锚点，而不是全页 textContent 找词）：
 *
 *  A. 提交事件后建出任务 → message.html?id=<taskId> 展示完整事实
 *     （#msg-title = 能力@版本、#msg-sub = ns · 任务 ID、#task-facts 里的 nodeName/digest）
 *  B. **详情页不再显示 out 字段**（P1.5 退役 + 迁移 28 删列）：路由结果只存在于
 *     emission，任务行上没有这个维度；#msg-content 里残留的 out 文案会被这条钉住
 *  C. 任务链入口：详情页的「查看链路」指向 trace.html?...&traceUID=<该 trace>
 *  D. 任务列表契约（v4：ns 进路径 + 列表信封 items[]）：未登录 401 / traceId 过滤 /
 *     空列表为 []
 *  E. 首页 ?msg=<taskId>（历史推送深链）重定向到 message.html?id=<taskId>
 *  F. 未登录点深链 → 跳登录页时 next 保留 message.html?id=
 *  G. 不存在的 id → 页面降级显示「任务不存在」、无 JS pageerror
 *
 * 造数走 v4 的真实产品路径：能力（信封）→ 源 targets（入口边）→ 登记节点 + 签发令牌
 * + WS hello + config.applied + 部署回执 → 投递事件。派发证据用 tasks 行的
 * node_id/assignment_id/node_name（v4 的 assign 只记候选，phase 仍是 pending）。
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

let server, browser;

async function injectSession(ctx, sessionValue, base) {
	let u;
	try {
		u = new URL(base);
	} catch {
		throw new Error("server.base 非法: " + base);
	}
	await ctx.addCookies([
		{
			name: "anotify_session",
			value: sessionValue,
			domain: u.hostname,
			path: "/",
			httpOnly: true,
			secure: u.protocol === "https:",
		},
	]);
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: deeplink（任务深链 message.html?id=<taskId>）===");
	server = await H.startServer({ suiteName: "deeplink", rpId: "localhost" });
	const s = H.seed(server.dbPath, "deeplink");

	// ---------- 建能力 + 入口边（源 targets）+ 真节点，让事件能派成任务 ----------
	//
	// v4 没有 `/v1/edges` 写入口：边是**只读派生投影**（能力边来自 `spec.next`，
	// 入口边来自 `source.spec.targets`，api-conventions §9 / A5）。所以这里改用语
	// 资源本身表达同一条拓扑：`deeplink.src --go--> review`。
	const NS = V.DEFAULT_NS;
	const cap = await V.mkCapability(server.base, s.session, "review", {
		body: V.capabilityBody({ name: "review" }),
	});
	await V.mkSource(server.base, s.session, "deeplink.src", {
		namespace: NS,
		targets: [{ capability: "review" }],
	});

	// 边投影：入口边的左端是**源名**（不是能力名），右端是**引用**（name / name@version）。
	const edgesResp = await H.req(server.base, V.P.edges(NS), { session: s.session });
	V.expectStatus("读边投影 …/edges → 200", edgesResp, 200);
	const edgeItems = Array.isArray(edgesResp.json?.items) ? edgesResp.json.items : null;
	H.check("边投影是 {items:[…]} 列表（v4：没有 Edge 资源）", !!edgeItems, V.brief(edgesResp));
	const entryEdge = (edgeItems || []).find((e) => e.kind === "source" && e.from === "deeplink.src");
	H.check(
		"入口边 deeplink.src → review（引用口径，kind=source）",
		!!entryEdge && entryEdge.to === "review",
		JSON.stringify(edgeItems),
	);
	// v3 的 Edge 视图带 `toCapabilityId` / `fromCapabilityId` 两个字段；v4 的投影**刻意
	// 只给引用**（前端图按 name@version 连，id 由请求方解析）。等价的强断言是：
	// 引用能在这个 ns 里解析到一个真实能力，且入口边的左端不会被当成能力名。
	const resolved = await H.req(server.base, V.P.capability(NS, entryEdge?.to || "review"), { session: s.session });
	V.expectStatus("边上的引用解析到真实能力 → 200", resolved, 200);
	H.eq("解析到的正是刚建的 review", resolved.json?.metadata?.uid, cap.id);
	H.check(
		"入口边的左端是源名，不是能力（没有 capability 类边从 deeplink.src 出发）",
		!(edgeItems || []).some((e) => e.kind === "capability" && e.from === "deeplink.src"),
		JSON.stringify(edgeItems),
	);

	// 节点：v4 的「节点能跑某任务」= 登记 → 签发令牌 → WS hello → config.applied →
	// 部署回执（deployments.state=applied），判定在 store.EligibleNodesForTask。
	// 这里走真实产品路径造这台节点（不直插 nodes：v4 已删 node_capabilities 那张表）。
	const node = await V.bringUpEligibleNode(server.base, s.session, {
		name: "deeplink-node",
		namespace: NS,
		slots: 1,
	});
	await V.deployApplied(server.base, s.session, node, cap, NS);

	// ---------- 提交事件 → 建出任务 ----------
	const ev = await V.ingestEvent(server.base, s.sendKey, NS, {
		source: "deeplink.src",
		type: "go",
		payload: { title: "深链验证-构建完成", cwd: "/repo/prod" },
	});
	H.eq("提交事件 → 200", ev.status, 200);
	const traceId = ev.json?.traceId;
	const tasks0 = ev.json?.tasks || [];
	H.check("事件建出任务", tasks0.length >= 1, JSON.stringify({
		tasks: tasks0,
		pruned: ev.json?.pruned,
	}));
	// v4 的回执元素是 `{taskId, capability}`（v3 的 `id` 已退役）。
	const taskId = tasks0[0]?.taskId;
	H.check("任务有 ID", typeof taskId === "string" && taskId.length > 0, JSON.stringify(tasks0[0]));
	H.eq("任务能力是 review", tasks0[0]?.capability, "review");

	// 派发证据：v4 的 assign 只记**候选**（phase 仍可能是 pending、没有租约），
	// 所以「派到了这台节点」的证据是 node_id + assignment_id + node_name 写上，
	// 而不是「离开 pending」。这条替换 v3 的「节点已上线」：后者只是造数前置，
	// 而这里直接钉住「任务真的落到了那台节点」。
	const dispatchedRow = await V.poll(
		`任务 ${taskId} 被派到节点 ${node.name}`,
		() => {
			const row = V.readTaskRow(server.dbPath, taskId);
			return row && row.node_id && row.assignment_id ? row : null;
		},
		{ timeoutMs: 20000, intervalMs: 200 },
	);
	H.eq("任务派到真节点（node_name）", dispatchedRow.node_name, node.name);

	// ---------- D. 任务列表契约（v4：ns 进路径 + 列表信封 items[]）----------
	console.log("--- 任务列表契约 ---");
	H.eq(
		"tasks 未登录 → 401",
		(await H.req(server.base, V.P.tasks(NS))).status,
		401,
	);
	const listAll = await H.req(server.base, V.P.tasks(NS) + "?limit=50", {
		session: s.session,
	});
	const listItems = V.assertListEnvelope("tasks 列表", listAll, "TaskList");
	H.check(
		"tasks 列表里有刚建的任务",
		listItems.some((it) => it?.metadata?.name === taskId),
		JSON.stringify(listItems.map((it) => it?.metadata?.name)),
	);
	const byTrace = await H.req(
		server.base,
		V.P.tasks(NS) + "?traceId=" + encodeURIComponent(traceId),
		{ session: s.session },
	);
	const traceItems = V.assertListEnvelope("tasks?traceId 列表", byTrace, "TaskList");
	H.check(
		"traceId 过滤命中",
		traceItems.some((it) => it?.metadata?.name === taskId),
		JSON.stringify(traceItems.map((it) => it?.metadata?.name)),
	);
	const emptyTrace = await H.req(server.base, V.P.tasks(NS) + "?traceId=trace_none", {
		session: s.session,
	});
	V.expectStatus("不存在的 trace → 200", emptyTrace, 200);
	H.check(
		"不存在的 trace → [] 而非 null",
		Array.isArray(V.items(emptyTrace)) && V.items(emptyTrace).length === 0,
		JSON.stringify(emptyTrace.json),
	);

	console.log("--- 重跑：用原任务的输入再跑一次 ---");
	const origBefore = await H.req(server.base, V.P.tasks(NS) + "/" + taskId, {
		session: s.session,
	});
	V.expectStatus("读原任务 → 200", origBefore, 200);
	V.assertEnvelope("原任务是 Task 信封", origBefore.json, "Task");
	const rerun = await H.req(server.base, V.P.tasks(NS) + "/" + taskId + "/rerun", {
		method: "POST",
		session: s.session,
		body: {},
	});
	V.expectStatus("重跑 → 200", rerun, 200);
	V.assertEnvelope("重跑回执是**新任务**信封", rerun.json, "Task");
	const rerunId = rerun.json?.metadata?.name;
	H.check(
		"重跑建的是新任务（新 taskId）",
		typeof rerunId === "string" && rerunId.length > 0 && rerunId !== taskId,
		String(rerunId),
	);
	H.eq("新任务 spec.rerunOf 指回原任务", rerun.json?.spec?.rerunOf, taskId);
	H.check(
		"新任务 params 与原任务深度相等",
		JSON.stringify(rerun.json?.spec?.params) === JSON.stringify(origBefore.json?.spec?.params),
		`new=${JSON.stringify(rerun.json?.spec?.params)} orig=${JSON.stringify(origBefore.json?.spec?.params)}`,
	);
	H.eq(
		"重跑默认钉住当前 live 版本",
		rerun.json?.spec?.capability?.version,
		origBefore.json?.spec?.capability?.version,
	);
	const origAfter = await H.req(server.base, V.P.tasks(NS) + "/" + taskId, {
		session: s.session,
	});
	V.expectStatus("重跑后读原任务 → 200", origAfter, 200);
	H.eq("原任务 phase 不变", origAfter.json?.status?.phase, origBefore.json?.status?.phase);
	H.check(
		"原任务 params 不变",
		JSON.stringify(origAfter.json?.spec?.params) === JSON.stringify(origBefore.json?.spec?.params),
		JSON.stringify(origAfter.json?.spec?.params),
	);
	H.check("原任务没有 rerunOf（重跑不改原任务）", !origAfter.json?.spec?.rerunOf, JSON.stringify(origAfter.json?.spec));
	V.expectReason(
		"重跑未知字段 → 400 unknown_field（参数域封闭）",
		await H.req(server.base, V.P.tasks(NS) + "/" + taskId + "/rerun", {
			method: "POST",
			session: s.session,
			body: { verison: 1 },
		}),
		400,
		"unknown_field",
	);
	V.expectStatus(
		"重跑不存在的任务 → 404",
		await H.req(server.base, V.P.tasks(NS) + "/tsk_missing/rerun", {
			method: "POST",
			session: s.session,
			body: {},
		}),
		404,
	);
	const cancelled = await H.req(server.base, V.P.tasks(NS) + "/" + rerunId + "/cancel", {
		method: "POST",
		session: s.session,
		body: {},
	});
	V.expectStatus("取消重跑出来的任务 → 200", cancelled, 200);
	H.eq("取消后是终态 cancelled", cancelled.json?.status?.phase, "cancelled");

	browser = await chromium.launch({ channel: "chrome", headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"] });

	// ---------- A/B/C. 详情页完整展示 ----------
	console.log("--- 任务详情页 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/message.html?id=" + taskId, {
			waitUntil: "load",
			timeout: 15000,
		});
		// 等布局挂载 + 数据渲染（固定 sleep 不可靠：API 往返时间不定）
		await H.waitForAppReady(page, "workspace");
		await page
			.waitForFunction(
				() => {
					const el = document.getElementById("msg-title");
					return el && el.textContent && el.textContent.trim().length > 0 &&
						el.textContent.indexOf("@") >= 0;
				},
				{ timeout: 10000 },
			)
			.catch(() => {});

		// v4 的 message.html 换了 DOM（#335 前端重写）：
		//   #msg-title = `能力@版本`， #msg-sub = `ns · taskId`， #msg-trace = 链路链接，
		//   字段卡是 #task-facts（诊断区），不再是 v3 的 #msg-fields。
		// 断言全部**盯 DOM 锚点与精确值**，不用全页 textContent 判词（侧栏导航会把
		// 「能力」「节点」这类词污染成永远为真）。
		const title = await page.evaluate(
			() => (document.getElementById("msg-title") || {}).textContent || "",
		);
		const sub = await page.evaluate(
			() => (document.getElementById("msg-sub") || {}).textContent || "",
		);
		const traceHref = await page.evaluate(
			() => (document.getElementById("msg-trace") || {}).getAttribute?.("href") || "",
		);
		const factsTxt = await page.evaluate(() => {
			const el = document.getElementById("task-facts");
			return el ? el.textContent || "" : null;
		});

		H.eq("详情页标题是 能力@版本", title.trim(), `${cap.name}@${cap.version}`);
		H.check("详情页含任务 ID", sub.includes(taskId), sub);
		H.check(
			"详情页「查看链路」链接带该 traceUID",
			traceHref.includes("trace.html") && traceHref.includes(traceId),
			traceHref,
		);
		H.check(
			"详情页诊断区已渲染（下面的「没有出口」才有意义）",
			!!factsTxt && factsTxt.trim().length > 0,
			String(factsTxt).slice(0, 160),
		);
		H.check("详情页诊断区含 nodeName 字段", !!factsTxt && factsTxt.includes("nodeName"), String(factsTxt).slice(0, 200));
		H.check(
			"详情页诊断区显示真实节点名",
			!!factsTxt && factsTxt.includes(node.name),
			String(factsTxt).slice(0, 300),
		);
		H.check(
			"详情页诊断区显示 digest（能力身份，不是编的版本号）",
			!!factsTxt && factsTxt.includes(cap.digest),
			String(factsTxt).slice(0, 300),
		);
		// P1.5 退役 / 迁移 28 删列之后，详情页**不再有出口这一栏**。
		// 改动前这里断言「含出口字段」+「out 三态文案可见」——那两条覆盖的是已经
		// 删掉的路径（tasks.out 列），现在反过来钉住它的消失。
		//
		// 范围仍是**字段区**（v4 的 #task-facts + #task-result 所在的 #msg-content），
		// 不是整个 body：侧栏的导航项叫「下游」之类，拿全页文本判「含出口」会被污染。
		const contentTxt = await page.evaluate(() => {
			const el = document.getElementById("msg-content");
			return el ? el.textContent || "" : null;
		});
		H.check(
			"详情页字段区已渲染（「没有出口」的前置守卫）",
			!!contentTxt && contentTxt.trim().length > 0,
			String(contentTxt).slice(0, 160),
		);
		H.check(
			"详情页字段区里没有 出口 栏（迁移 28 删列，产出只走 emit）",
			!!contentTxt && !contentTxt.includes("出口"),
			String(contentTxt).slice(0, 200),
		);
		H.check(
			"详情页字段区里没有 out 两态文案（未回报/到头了）",
			!!contentTxt && !/(未回报|到头了)/.test(contentTxt),
			String(contentTxt).slice(0, 200),
		);
		// 任务链区块：v4 用「查看链路」链接承接（不再内嵌区块）。
		H.check(
			"详情页有任务链入口（#msg-trace 可见）",
			await page.evaluate(() => {
				const el = document.getElementById("msg-trace");
				return !!el && !el.hidden;
			}),
			traceHref,
		);
		H.check("详情页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 120));
		await page.close();
		await ctx.close();
	}

	console.log("--- 重跑入口（终态页） ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/message.html?id=" + rerunId, {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		await page.waitForFunction(() => {
			const el = document.getElementById("task-phase");
			return !!el && el.getAttribute("data-phase") === "cancelled";
		}, { timeout: 10000 });
		const ui = await page.evaluate(() => ({
			banner: (document.getElementById("task-rerun-of") || {}).textContent || "",
			rerun: !!document.getElementById("task-rerun"),
			rerunEdit: !!document.getElementById("task-rerun-edit"),
			noRetry: (document.getElementById("task-no-retry") || {}).textContent || "",
			facts: (document.getElementById("task-facts") || {}).textContent || "",
		}));
		H.check("重跑出来的任务首屏注明「是谁的重跑」", ui.banner.includes(taskId), ui.banner);
		H.check("终态任务有「重跑」按钮", ui.rerun, JSON.stringify(ui));
		H.check("终态任务有「修改参数后重跑」按钮", ui.rerunEdit, JSON.stringify(ui));
		H.check("非幂等终态任务说明「不会自动重试」", ui.noRetry.includes("不会自动重试"), ui.noRetry);
		H.check(
			"非幂等终态任务不再显示「N/M」的剩余次数",
			!/[0-9]+\/[0-9]+/.test(ui.facts),
			ui.facts.slice(0, 240),
		);
		await page.click("#task-rerun");
		await page.waitForFunction(() => {
			const box = document.getElementById("rerun-modal");
			return !!box && !box.classList.contains("hidden") &&
				document.querySelectorAll("#rerun-versions .msg-rerun-choice").length >= 1;
		}, { timeout: 10000 });
		const modal = await page.evaluate(() => {
			const box = document.getElementById("rerun-modal");
			const versions = Array.from(document.querySelectorAll("#rerun-versions .msg-rerun-choice")).map((n) => n.textContent.trim());
			return {
				open: !!box && !box.classList.contains("hidden"),
				versions,
				submitDisabled: document.getElementById("rerun-submit").disabled,
			};
		});
		H.check("点「重跑」打开版本选择弹层", modal.open, JSON.stringify(modal));
		H.check("弹层里有一个可选的版本", modal.versions.length >= 1 && modal.versions[0].includes(`v${cap.version}`), JSON.stringify(modal.versions));
		H.check("弹层可以提交", modal.submitDisabled === false, JSON.stringify(modal));
		H.check("重跑入口无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 120));
		await page.close();
		await ctx.close();
	}

	// ---------- E. 首页 ?msg=<taskId> 深链 → 任务详情页 ----------
	//
	// 首页的「最近任务」列表与任务弹层已下线（产品对象是事件流），
	// 但历史推送载荷里还留着 index.html?msg=<taskId> —— 不能点成空页，
	// 所以首页脚本把它转到 message.html?id=<taskId>（见 index.html 的
	// redirectLegacyMsg）。这条用例钉住「旧深链仍然到得了那条任务」。
	console.log("--- 首页 ?msg 深链 → 任务详情页 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/index.html?msg=" + taskId, {
			waitUntil: "load",
			timeout: 15000,
		});
		await page
			.waitForURL(/message\.html\?id=/, { timeout: 10000 })
			.catch(() => {});
		H.check(
			"首页 ?msg=<taskId> 重定向到 message.html?id=",
			page.url().includes("message.html") && page.url().includes("id=" + taskId),
			page.url(),
		);
		await H.waitForAppReady(page, "workspace");
		await page
			.waitForFunction(
				() => {
					const el = document.getElementById("msg-title");
					return el && el.textContent && el.textContent.includes("@");
				},
				{ timeout: 10000 },
			)
			.catch(() => {});
		const txt = await page.evaluate(() => document.body.textContent || "");
		H.check("重定向后详情页渲染该任务", txt.includes(taskId), txt.slice(0, 120));
		H.check(
			"首页 ?msg 深链无 JS pageerror",
			errs.length === 0,
			errs[0]?.slice(0, 120),
		);
		await page.close();
		await ctx.close();
	}

	// ---------- G. 不存在的 id → 降级 ----------
	console.log("--- 不存在的 id ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, s.session, server.base);
		const page = await ctx.newPage();
		const errs = [];
		page.on("pageerror", (e) => errs.push(String(e)));
		await page.goto(server.base + "/message.html?id=tsk_not_exist", {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForAppReady(page, "workspace");
		// v4 的 404 分支把文案放进 #msg-error（标题保持「任务详情」不变）。
		await page
			.waitForFunction(
				() => {
					const el = document.getElementById("msg-error");
					return el && (el.textContent || "").includes("任务不存在");
				},
				{ timeout: 10000 },
			)
			.catch(() => {});
		const errTxt = await page.evaluate(
			() => (document.getElementById("msg-error") || {}).textContent || "",
		);
		H.check("未命中显示「任务不存在」", errTxt.includes("任务不存在"), errTxt.slice(0, 200));
		H.check(
			"不存在的 id 无 JS pageerror",
			errs.length === 0,
			errs[0]?.slice(0, 120),
		);
		await page.close();
		await ctx.close();
	}

	// ---------- F. 未登录点深链 → next 保留 ----------
	console.log("--- 未登录深链 → next 保留 ---");
	{
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		const page = await ctx.newPage();
		await page.goto(server.base + "/message.html?id=" + taskId, {
			waitUntil: "load",
			timeout: 15000,
		});
		await H.waitForUrl(page, /login\.html/, 8000);
		const u = page.url();
		H.check("未登录点深链 → 跳登录页", u.includes("login.html"), u);
		let next = "";
		try {
			next = new URL(u).searchParams.get("next") || "";
		} catch {
			/* next 为空时下面的断言会失败 */
		}
		H.check(
			"next 保留 message.html?id= 深链",
			next.includes("message.html") && next.includes("id=" + taskId),
			`next=${next}`,
		);
		await page.close();
		await ctx.close();
	}

	const passed = H.summary("deeplink");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}
main().catch(async (e) => {
	console.error(e);
	try {
		await browser?.close();
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
