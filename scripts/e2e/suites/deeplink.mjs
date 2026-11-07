#!/usr/bin/env node
/* SUITE: deeplink — 点击通知 → message.html?id=<taskId> 展示任务详情
 *
 * #80 之后旧「通知/消息」模型已下线（/v1/notify、/v1/notifications 删除），
 * 深链的目标从「消息详情」变成「任务详情」。覆盖：
 *
 *  A. 提交事件后建出任务 → message.html?id=<taskId> 展示完整字段
 *     （任务 ID / Trace / 能力 / 状态 / 节点 / 时间）
 *  B. **详情页不再显示 out 字段**（P1.5 退役 + 迁移 28 删列）：路由结果只存在于
 *     emission，任务行上没有这个维度；页面上残留的 out 文案会被这条钉住
 *  C. 任务链区块渲染整条 trace 上的任务
 *  D. /v1/tasks 契约：未登录 401 / traceId 过滤 / 空列表为 []
 *  E. 首页 ?msg=<taskId>（历史推送深链）重定向到 message.html?id=<taskId>
 *  F. 未登录点深链 → 跳登录页时 next 保留 message.html?id=
 *  G. 不存在的 id → 页面降级显示「任务不存在」、无 JS pageerror
 */
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

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

	// ---------- 建能力 + 边 + 节点，让事件能派成任务 ----------
	// review@1 有两个出口（fix / clean）：detail 页曾经按 out 显示「走 fix 出口」
	const cap = await H.req(server.base, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: {
			name: "review",
			outputs: ["fix", "clean"],
			paramsSchema: { type: "object" },
			// P1-b2：建能力必须有实现（缺 run → 400 impl_missing）。
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\nexit 0\n" } },
		},
	});
	H.eq("建 review@1 → 200", cap.status, 200);
	const reviewId = cap.json?.capability?.id;

	// 起点能力
	const src = await H.req(server.base, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: {
			name: "deeplink.src",
			outputs: ["go"],
			paramsSchema: { type: "object" },
			run: { entrypoint: "run.sh", files: { "run.sh": "#!/bin/sh\nexit 0\n" } },
		},
	});
	const srcId = src.json?.capability?.id;

	// 边：deeplink.src --go--> review
	H.check("srcId/reviewId 已拿到", !!srcId && !!reviewId, `${srcId} -> ${reviewId}`);
	const edgeResp = await H.req(server.base, "/v1/edges", {
		session: s.session,
		method: "POST",
		// #110：入口边——来源名 "deeplink.src" 直接当左端
		// P2.2：两端写的是**引用**（`name` / `name@version`），不是能力 id
		body: {
			fromIsSource: true,
			fromRef: "deeplink.src",
			fromPort: "go",
			toRef: "review@1",
		},
	});
	H.eq("建边 deeplink.src→review → 200", edgeResp.status, 200);
	const edgesResp = await H.req(server.base, "/v1/edges", { session: s.session });
	const entryEdge = (edgesResp.json?.edges || []).find(
		(e) => e.fromIsSource === true && e.fromRef === "deeplink.src",
	);
	// #110：入口边的左端是**来源名**（"deeplink.src"），不是能力 id
	// P2.2：断言改成**引用口径**（`toRef`）——`toCapability` 已经不存在了
	H.check(
		"边指向 review@1（引用口径）",
		!!entryEdge && entryEdge.toRef === "review@1",
		JSON.stringify(edgesResp.json?.edges),
	);
	// 引用之外还要能拿到「此刻解析到哪一版」：详情链接靠它，前端不能自己算
	H.check(
		"边带上解析后的能力 id（toCapabilityId）",
		!!entryEdge && entryEdge.toCapabilityId === reviewId,
		JSON.stringify(entryEdge),
	);
	// 入口边的左端是来源名，没有「哪一版」——必须是空串，不能编成某个能力
	H.check(
		"入口边的 fromCapabilityId 是空串",
		!!entryEdge && entryEdge.fromCapabilityId === "",
		JSON.stringify(entryEdge),
	);

	// 节点：声明 review@1 与 deeplink.src@1
	//
	// 能力是建在**默认命名空间**里的，节点必须播到同一个命名空间，否则
	// NodesForCapability 查不到（派发会 pruned/no_node）。
	//
	// #204 / 迁移 14：组织层删除（orgs / org_members 两张表都没了），隔离边界只剩
	// 命名空间。原来那句「拿到 orgId」（按 uid 查 org_members）在这里换成等价强度的
	// **命名空间口径**断言：拿到默认命名空间（slug=default）的 id。
	//
	// #162：节点行直插库，迁移 12 起必须写 namespace_id。
	const nsId = await H.defaultNamespaceId(server.base, s.session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	// 必须**真 WS 握手**：只播 rows 的节点是 offline，
	// PublishAssign 会失败 → 任务被回滚成 pending（attempts=1），
	// 于是响应里 tasks=[]。那是正确行为，但测不到详情页。
	const node = await N.bringUpNode(
		server.base,
		server.dbPath,
		nsId,
		"deeplink-node",
		[
			{ id: reviewId, name: "review", version: 1 },
			{ id: srcId, name: "deeplink.src", version: 1 },
		],
	);
	H.check("节点已上线", !!node.node?.id || !!node.id, JSON.stringify(Object.keys(node)));

	// ---------- 提交事件 → 建出任务 ----------
	const ev = await H.req(server.base, "/v1/events", {
		key: s.sendKey,
		body: {
			source: "deeplink.src",
			type: "go",
			payload: { title: "深链验证-构建完成", cwd: "/repo/prod" },
		},
	});
	H.eq("提交事件 → 200", ev.status, 200);
	const traceId = ev.json?.traceId;
	const tasks0 = ev.json?.tasks || [];
	H.check("事件建出任务", tasks0.length >= 1, JSON.stringify({
		tasks: tasks0,
		pruned: ev.json?.pruned,
		origin: ev.json?.origin,
	}));
	const taskId = tasks0[0]?.id;
	H.check("任务有 ID", typeof taskId === "string" && taskId.length > 0);

	// ---------- D. /v1/tasks 契约 ----------
	console.log("--- /v1/tasks 契约 ---");
	H.eq(
		"tasks 未登录 → 401",
		(await H.req(server.base, "/v1/tasks")).status,
		401,
	);
	const listAll = await H.req(server.base, "/v1/tasks?limit=50", {
		session: s.session,
	});
	H.eq("tasks → 200", listAll.status, 200);
	H.check(
		"tasks 返回数组",
		Array.isArray(listAll.json?.tasks),
		JSON.stringify(listAll.json),
	);
	const byTrace = await H.req(
		server.base,
		"/v1/tasks?traceId=" + encodeURIComponent(traceId),
		{ session: s.session },
	);
	H.eq("tasks?traceId → 200", byTrace.status, 200);
	H.check(
		"traceId 过滤命中",
		(byTrace.json?.tasks || []).some((t) => t.id === taskId),
		JSON.stringify((byTrace.json?.tasks || []).map((t) => t.id)),
	);
	const emptyTrace = await H.req(server.base, "/v1/tasks?traceId=trace_none", {
		session: s.session,
	});
	H.check(
		"不存在的 trace → [] 而非 null",
		Array.isArray(emptyTrace.json?.tasks) &&
			emptyTrace.json.tasks.length === 0,
		JSON.stringify(emptyTrace.json),
	);

	browser = await chromium.launch({ channel: "chrome", headless: true });

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
		const txt = await page.evaluate(() => document.body.textContent || "");

		H.check("详情页标题含能力名", txt.includes("review"), txt.slice(0, 120));
		H.check("详情页含任务 ID", txt.includes(taskId), txt.slice(0, 80));
		H.check("详情页含 Trace", txt.includes(traceId));
		H.check("详情页含 能力 字段", txt.includes("能力"));
		H.check("详情页含 执行节点 字段", txt.includes("执行节点"));
		// P1.5 退役 / 迁移 28 删列之后，详情页**不再有出口这一栏**。
		// 改动前这里断言「含出口字段」+「out 三态文案可见」——那两条覆盖的是已经
		// 删掉的路径（tasks.out 列），现在反过来钉住它的消失。
		//
		// 断言范围是**字段卡**（#msg-fields），不是整个 body：侧边栏的导航项
		// `nav.edges` 也叫「出口」（那是边/出口表的页面名），拿全页文本判「含出口」
		// 会被它污染——旧断言其实正是被这层污染"通过"的，这是个一直没被发现的弱断言。
		//
		// 同时要求字段卡**已渲染且有内容**：否则「没有出口栏」是页面没加载出来的
		// 假阳性（空 dl 当然不含任何词）。
		const fieldsTxt = await page.evaluate(() => {
			const el = document.getElementById("msg-fields");
			return el ? el.textContent || "" : null;
		});
		H.check(
			"详情页字段卡已渲染（下面的「没有出口」才有意义）",
			!!fieldsTxt && fieldsTxt.trim().length > 0,
			String(fieldsTxt).slice(0, 160),
		);
		H.check(
			"详情页字段卡里没有 出口 栏（迁移 28 删列，产出只走 emit）",
			!!fieldsTxt && !fieldsTxt.includes("出口"),
			String(fieldsTxt).slice(0, 200),
		);
		H.check(
			"详情页字段卡里没有 out 两态文案（未回报/到头了）",
			!!fieldsTxt && !/(未回报|到头了)/.test(fieldsTxt),
			String(fieldsTxt).slice(0, 200),
		);
		// 任务链区块
		H.check("详情页含任务链区块", txt.includes("任务链"), txt.slice(0, 200));
		H.check("详情页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 120));
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
		await page
			.waitForFunction(
				() => {
					const el = document.getElementById("msg-title");
					return el && el.textContent && el.textContent.includes("不存在");
				},
				{ timeout: 10000 },
			)
			.catch(() => {});
		const txt = await page.evaluate(() => document.body.textContent || "");
		H.check("未命中显示「任务不存在」", txt.includes("任务不存在"), txt.slice(0, 120));
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
