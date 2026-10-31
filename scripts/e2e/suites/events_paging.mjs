#!/usr/bin/env node
/* SUITE: events_paging — 批次 4B 的 e2e 套件（#234 / #235 / #236）
 *
 * 覆盖 .pi-legion/contract.md 的验收项：
 *
 *  V1  GET /v1/events?limit=501..1000 不再返回 50 条（#236 根因：store 对 >500 兜底成 50），
 *      返回 ≤500 条且**回显生效值** limit。
 *  V2  GET /v1/console/events?limit=1000 同理（≤500，回显）。
 *  V3  console events 支持 before=<cursor> 游标分页：一路翻到最旧，无重复、无遗漏。
 *  V4  events 页有分页导航，URL 带 ?before=，刷新/分享后回到同一窗口。
 *  V5  events / index 不加载 dagre/flograph/graphs/traceview（-150.7KB / -4 请求），
 *      trace 页仍然四个都在（顺序 dagre→flograph→graphs→traceview）且能画图。
 *
 * 套件结构（顺序有讲究，见下面 §造数）：
 *   A. 造一条**真实任务**的 trace（V5 的 trace 页要有内容）+ 249 条直插库的合成事件
 *      → 事件流恰好 250 条，于是 V1/V3 的页边界是确定值（100/100/50）。
 *   B. 后端游标分页（HTTP，会话 Cookie，250 条）。
 *   C. events 页 UI 分页（Chrome，250 条 → 3 页）。
 *   D. 资源分层（同一 Chrome 会话，events/index/trace 三页的 resource 清单）。
 *   E. 再把库撑到 550 条，验 #236 的 limit 硬顶（limit=501/1000 要能看到 500 条：
 *      库里只有 250 条时「返回 500 条」根本无从判定，所以这一段必须排在 B/C 之后）。
 */
import { chromium } from "playwright-core";
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const PAGE_SIZE = 100; // 契约 §3.4：events 页页大小
const TOTAL_EVENTS = 250; // B/C 的数据规模 → 100/100/50，第 3 页就是最后一页
const BOOST_EVENTS = 300; // E 段把库撑到 550 条，才能验 limit=501 → 500
// 契约 §4：四个图脚本只在 trace / source 两页加载。
//
// 注意**指纹**：静态资源经 `make fe` 改名为 `<stem>.<hash8>.js`
// （`/vendor/dagre.min.54a503e3.js`、`/traceview.642fc03d.js`）。所以这里比的是
// 「去掉指纹后的 basename」，而不是 URL 里的字面子串 —— 用子串匹配的话
// 「不含 dagre.min.js」这种**否定**断言会永远为真（指纹对不上就当成没加载），
// 是个假绿陷阱。
const GRAPH_LIBS = ["dagre.min.js", "flograph.js", "graphs.js", "traceview.js"];

// libBaseName 把 URL 还原成「无指纹的 basename」：/x/y.flograph.20c5c69e.js → flograph.js
function libBaseName(url) {
	let pathname = "";
	try {
		pathname = new URL(url).pathname;
	} catch {
		pathname = String(url).split("?")[0];
	}
	const base = pathname.split("/").pop() || "";
	// 指纹是 .js 前的一段 hex（长度不定，实测 8 位）。只在这个位置剥一次。
	return base.replace(/\.[0-9a-f]{6,}(?=\.js$)/i, "");
}

let server, browser, nodeConn;

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

// ---------- 造数：直插库（250 次 POST 太慢，也会污染事件流口径） ----------
//
// 为什么直插而不是循环 POST /v1/events：
//   ① 250 次 HTTP + 每次派发尝试要好几秒，套件会被 120s 预算吃掉；
//   ② POST 会尝试派发任务、可能写剪枝事件（source='route'），事件流条数就不再是
//      「我插入的条数」那种可预期的东西。
//
// 列名以 internal/store/schema.sql 的 events 表为准（迁移 12 之后是 namespace_id，
// 不再是 org_id）：id / namespace_id / trace_id / source / type / payload / idem_key / created_at。
//
// created_at 严格递减、id 编号递增（ORDER BY created_at DESC, id DESC 的稳定序），
// trace_id 每条唯一 —— UI 断言要靠「首行 trace_id」来判定翻页真的换了窗口
// （卡片上不渲染事件 id，只渲染 source → type 和 trace_id）。
function insertSyntheticEvents(dbPath, namespaceId, { count, firstIndex, newestAt }) {
	const db = new DatabaseSync(dbPath);
	try {
		// 服务在跑（WAL），写事务会和服务端抢写锁：等不到就报 SQLITE_BUSY 而不是立刻失败。
		db.exec("PRAGMA busy_timeout=5000");
		// 一条事务插完：250 次独立事务会写 250 次 WAL，慢且无必要。
		db.exec("BEGIN IMMEDIATE");
		const stmt = db.prepare(
			`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
			 VALUES (?, ?, ?, 'e2e_page', 'e2e.tick', '{}', NULL, ?)`,
		);
		for (let i = 0; i < count; i++) {
			const idx = firstIndex + i;
			const suffix = String(idx).padStart(4, "0");
			stmt.run(
				"evt_e2e_" + suffix,
				namespaceId,
				"tr_e2e_" + suffix,
				newestAt - i,
			);
		}
		db.exec("COMMIT");
	} catch (e) {
		try {
			db.exec("ROLLBACK");
		} catch {
			/* 已经回滚/没开事务 */
		}
		throw e;
	} finally {
		db.close();
	}
}

// streamRows 读「事件流口径」的行（不含 source='route' 剪枝行，与 store.ListEvents
// 不带 traceId 时的剪枝规则一致）。期望值从库里查，而不是把 250 写死在断言里 ——
// 写死的话任何造数偏差都会变成「断言看起来是对的，其实比的是错的数」。
function streamRows(dbPath, namespaceId) {
	const db = new DatabaseSync(dbPath);
	try {
		db.exec("PRAGMA busy_timeout=5000");
		const rows = db
			.prepare(
				`SELECT id, created_at FROM events
				 WHERE namespace_id = ? AND source != 'route'
				 ORDER BY created_at DESC, id DESC`,
			)
			.all(namespaceId);
		return rows.map((r) => ({ id: r.id, createdAt: Number(r.created_at) }));
	} finally {
		db.close();
	}
}

// ---------- UI 辅助 ----------
// 首行身份：卡片上唯一每行不同的是 trace_id（唯一），用它判定「翻页真的换了窗口」。
const FIRST_TRACE_SEL = "#ev-list .card div.mono.truncate";

async function listInfo(page) {
	return page.evaluate(() => {
		const cards = document.querySelectorAll("#ev-list .card");
		const first = document.querySelector("#ev-list .card div.mono.truncate");
		return {
			count: cards.length,
			firstTrace: first ? first.textContent.trim() : "",
			summary:
				(document.getElementById("ev-summary") || {}).textContent?.trim() || "",
			hasOlder: !!document.getElementById("ev-older"),
			hasNewer: !!document.getElementById("ev-newer"),
		};
	});
}

// waitForList 等列表稳定在 count 行（可选：排除某条首行 trace，用于「点了更旧」）。
// 固定 sleep 不可靠：API 往返 + 渲染时间随机器负载变化，只能等状态而不是等时间。
async function waitForList(page, count, prevFirstTrace = "") {
	await page.waitForFunction(
		([n, prev, sel]) => {
			const cards = document.querySelectorAll("#ev-list .card");
			if (cards.length !== n) return false;
			if (!prev) return true;
			const first = document.querySelector(sel);
			return !!first && first.textContent.trim() !== prev;
		},
		[count, prevFirstTrace, FIRST_TRACE_SEL],
		{ timeout: 15000 },
	);
}

function beforeOf(url) {
	try {
		return new URL(url).searchParams.get("before") || "";
	} catch {
		return "";
	}
}

function resourceNames(page) {
	return page.evaluate(() =>
		performance.getEntriesByType("resource").map((r) => r.name),
	);
}

// loadedGraphLibs 返回实际被请求过的图脚本（按 GRAPH_LIBS 的声明顺序）。
function loadedGraphLibs(names) {
	const seen = new Set(names.map(libBaseName));
	return GRAPH_LIBS.filter((lib) => seen.has(lib));
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: events_paging（#234 / #235 / #236）===");
	server = await H.startServer({ suiteName: "events_paging", rpId: "localhost" });
	const s = H.seed(server.dbPath, "events_paging");
	const base = server.base;

	const nsId = await H.defaultNamespaceId(base, s.session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	// ---------- A. 造一条有真实任务的 trace（V5 的 trace 页要有内容） ----------
	// 配方同 deeplink.mjs：能力 + 入口边 + 真 WS 节点，事件才会派发出任务。
	// 事件本身 source='e2e.paging.src'（不是 'route'），因此它也会出现在事件流里
	// —— 下面合成事件插 249 条，加起来正好 250 条。
	console.log("--- 造数：真实任务 trace + 合成事件 ---");
	const cap = await H.req(base, "/v1/capabilities", {
		session: s.session,
		method: "POST",
		body: {
			name: "e2e.paging",
			outputs: ["ok"],
			paramsSchema: { type: "object" },
		},
	});
	H.eq("建能力 e2e.paging → 200", cap.status, 200);
	const capId = cap.json?.capability?.id;

	const edge = await H.req(base, "/v1/edges", {
		session: s.session,
		method: "POST",
		body: {
			fromIsSource: true,
			fromCapability: "e2e.paging.src",
			fromPort: "go",
			toCapability: capId,
		},
	});
	H.eq("建入口边 e2e.paging.src --go--> e2e.paging → 200", edge.status, 200);

	const node = await N.bringUpNode(
		base,
		server.dbPath,
		nsId,
		"events-paging-node",
		[{ id: capId, name: "e2e.paging", version: 1 }],
	);
	nodeConn = node.conn;

	const nowSec = Math.floor(Date.now() / 1000);
	const realEv = await H.req(base, "/v1/events", {
		key: s.sendKey, // 机器入口要 notify:send（recvKey 一律 403，不是端点坏了）
		body: {
			source: "e2e.paging.src",
			type: "go",
			payload: { title: "events_paging 造数", cwd: "/repo/e2e" },
		},
	});
	H.eq("POST /v1/events（sendKey）→ 200", realEv.status, 200);
	const traceId = realEv.json?.traceId;
	H.check("真实事件建出任务（trace 页有内容）", (realEv.json?.tasks || []).length >= 1, JSON.stringify({
		tasks: realEv.json?.tasks,
		pruned: realEv.json?.pruned,
		origin: realEv.json?.origin,
	}));
	H.check("拿到真实 traceId", typeof traceId === "string" && traceId.length > 0, String(traceId));

	// 合成事件：比真实事件旧 10s 起，严格递减，绝不与真实事件撞 created_at。
	insertSyntheticEvents(server.dbPath, nsId, {
		count: TOTAL_EVENTS - 1,
		firstIndex: 1,
		newestAt: nowSec - 10,
	});
	const rows = streamRows(server.dbPath, nsId);
	const total = rows.length;
	H.eq(`事件流恰好 ${TOTAL_EVENTS} 条（真实 1 + 合成 ${TOTAL_EVENTS - 1}）`, total, TOTAL_EVENTS);
	if (total <= PAGE_SIZE) throw new Error(`事件流只有 ${total} 条，分页断言无意义`);

	// ---------- B. #235 游标分页（HTTP，会话 Cookie） ----------
	console.log("--- B. /v1/console/events 游标分页 ---");
	const fetchPage = (qs) =>
		H.req(base, "/v1/console/events?" + qs, { session: s.session });

	const p1 = await fetchPage("limit=" + PAGE_SIZE);
	H.eq("第 1 页 status 200", p1.status, 200);
	H.eq("第 1 页 100 条", p1.json?.events?.length ?? -1, PAGE_SIZE);
	H.eq("第 1 页回显 limit=100", p1.json?.limit, PAGE_SIZE);
	const cursor1 = p1.json?.nextCursor || "";
	H.check("第 1 页 nextCursor 非空", cursor1.length > 0, JSON.stringify({ nextCursor: cursor1 }));

	const p2 = await fetchPage(
		"limit=" + PAGE_SIZE + "&before=" + encodeURIComponent(cursor1),
	);
	H.eq("第 2 页 100 条", p2.json?.events?.length ?? -1, PAGE_SIZE);
	const cursor2 = p2.json?.nextCursor || "";
	H.check("第 2 页 nextCursor 非空", cursor2.length > 0, JSON.stringify({ nextCursor: cursor2 }));

	const p3 = await fetchPage(
		"limit=" + PAGE_SIZE + "&before=" + encodeURIComponent(cursor2),
	);
	const lastPageExpected = total - 2 * PAGE_SIZE; // 250 → 50
	H.eq(`第 3 页 ${lastPageExpected} 条（最后一页）`, p3.json?.events?.length ?? -1, lastPageExpected);
	H.eq("第 3 页 nextCursor 为空（已到最旧）", p3.json?.nextCursor, "");

	// 无重复、无遗漏：三页并集必须正好等于库里事件流的全部 id。
	const pageIds = [p1, p2, p3].flatMap((p) => (p.json?.events || []).map((e) => e.id));
	const union = new Set(pageIds);
	H.check("三页 id 无重复", union.size === pageIds.length, `三页共 ${pageIds.length} 行，去重后 ${union.size}`);
	const allIds = new Set(rows.map((r) => r.id));
	H.check(
		`三页并集 == 库里全部 ${total} 个 id（无遗漏）`,
		union.size === allIds.size && [...allIds].every((id) => union.has(id)),
		JSON.stringify({ union: union.size, all: allIds.size }),
	);

	// 每页内部 DESC（created_at 严格不增），且页间不断档。
	const pages = [p1, p2, p3].map((p) => p.json?.events || []);
	const descOk =
		pages.every((evs) => evs.every((e, i) => i === 0 || evs[i - 1].createdAt >= e.createdAt)) &&
		pages.every((evs, i) => i === 0 || (pages[i - 1].at(-1).createdAt >= evs[0].createdAt));
	H.check("每页 createdAt 不增且页间连续（DESC 全序）", descOk, JSON.stringify(
		pages.map((evs) => evs.slice(0, 1).concat(evs.slice(-1)).map((e) => e.createdAt)),
	));

	// 非法游标一律 400 —— 不静默降级回第一页（那正是 #236 那类「悄悄换窗口」的病根）。
	const badCursor = await fetchPage("limit=" + PAGE_SIZE + "&before=" + encodeURIComponent("这不是游标"));
	H.eq("坏游标 before → 400", badCursor.status, 400);
	H.eq("坏游标 error 文案", badCursor.json?.error, "invalid before cursor");

	// V2：limit=1000 收到硬顶 500，回显生效值。
	const big = await fetchPage("limit=1000");
	H.eq("console limit=1000 → 库里全部行数（≤500 硬顶）", big.json?.events?.length ?? -1, total);
	H.eq("console limit=1000 回显生效值", big.json?.limit, 500);

	// ---------- C/D. Chrome：events 页分页 + 资源分层 ----------
	console.log("--- C. events 页 UI 分页（Chrome 1280×800）---");
	browser = await chromium.launch({ channel: "chrome", headless: true });
	const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
	await H.mockExternalCdn(ctx);
	await injectSession(ctx, s.session, base);
	const page = await ctx.newPage();
	const errs = [];
	page.on("pageerror", (e) => errs.push(String(e)));

	await page.goto(base + "/events.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(page, "workspace");
	await waitForList(page, PAGE_SIZE);

	let info = await listInfo(page);
	H.eq("首屏 100 行", info.count, PAGE_SIZE);
	H.check("首页 URL 不带 before", beforeOf(page.url()) === "", page.url());
	const firstTrace1 = info.firstTrace;
	H.check("首屏首行有 trace_id（行身份可判定）", firstTrace1.length > 0, firstTrace1);

	// V5（events 页）：非图页面不加载四个图脚本。
	const eventsResources = await resourceNames(page);
	const eventsLoaded = loadedGraphLibs(eventsResources);
	H.check(
		"events.html 不加载 dagre/flograph/graphs/traceview（V5）",
		eventsLoaded.length === 0,
		JSON.stringify({ loaded: eventsLoaded, resources: eventsResources.map(libBaseName) }),
	);

	// 第 2 页
	await page.click("#ev-older");
	await waitForList(page, PAGE_SIZE, firstTrace1);
	info = await listInfo(page);
	H.eq("第 2 页 100 行", info.count, PAGE_SIZE);
	const before2 = beforeOf(page.url());
	H.check("第 2 页 URL 带 before=", before2.length > 0, page.url());
	H.check("第 2 页有「较新」按钮", info.hasNewer, JSON.stringify(info));
	const firstTrace2 = info.firstTrace;

	// 第 3 页（250 条 → 最后 50 条，已到最旧）
	await page.click("#ev-older");
	await waitForList(page, lastPageExpected, firstTrace2);
	info = await listInfo(page);
	H.eq(`第 3 页 ${lastPageExpected} 行`, info.count, lastPageExpected);
	H.check("第 3 页「更旧」按钮消失（已到最旧）", !info.hasOlder, JSON.stringify(info));
	H.check("摘要有「已到最旧」", info.summary.includes("已到最旧"), info.summary);
	H.check("第 3 页有「较新」按钮", info.hasNewer, JSON.stringify(info));
	const before3 = beforeOf(page.url());
	H.check("第 3 页 URL 带 before=", before3.length > 0, page.url());
	const firstTrace3 = info.firstTrace;

	// 「较新」回退一页：行数回到 100，URL 的 before 与第 2 页一致
	await page.click("#ev-newer");
	await waitForList(page, PAGE_SIZE, firstTrace3);
	info = await listInfo(page);
	H.eq("点「较新」→ 回到 100 行", info.count, PAGE_SIZE);
	H.eq("点「较新」→ URL before 与第 2 页一致", beforeOf(page.url()), before2);

	// 再往前翻回第 3 页，然后把该 URL 直接打开（V4：刷新/分享后位置不丢）
	await page.click("#ev-older");
	await waitForList(page, lastPageExpected, firstTrace2);
	await page.goto(base + "/events.html?before=" + encodeURIComponent(before3), {
		waitUntil: "load",
		timeout: 20000,
	});
	await H.waitForAppReady(page, "workspace");
	await waitForList(page, lastPageExpected);
	info = await listInfo(page);
	H.eq("直接打开 ?before= 的第 3 页 URL → 50 行", info.count, lastPageExpected);
	H.eq("刷新后首行 trace_id 不变（同一窗口，V4）", info.firstTrace, firstTrace3);
	H.check("刷新后「较新」按钮隐藏（游标栈为空，契约 §3.4）", !info.hasNewer, JSON.stringify(info));
	H.check("刷新后仍显示「已到最旧」", info.summary.includes("已到最旧"), info.summary);

	const layout = await page.evaluate(() => ({
		overflow: document.documentElement.scrollWidth <= window.innerWidth + 1,
		detail: `scrollWidth=${document.documentElement.scrollWidth} innerWidth=${window.innerWidth}`,
	}));
	H.check("events 页无横向溢出（1280×800）", layout.overflow, layout.detail);
	H.check("events 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	// ---------- D. 资源分层：index 不含、trace 含（同一 Chrome 会话） ----------
	console.log("--- D. 资源分层（V5）---");
	errs.length = 0; // 每个页面单独判 pageerror，避免上一页的噪声串到下一页
	await page.goto(base + "/index.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(page, "workspace");
	const indexResources = await resourceNames(page);
	const indexLoaded = loadedGraphLibs(indexResources);
	H.check(
		"index.html 不加载四个图脚本（V5：-150.7KB / -4 请求）",
		indexLoaded.length === 0,
		JSON.stringify({ loaded: indexLoaded }),
	);
	H.check("index 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	errs.length = 0;
	await page.goto(base + "/trace.html?traceId=" + encodeURIComponent(traceId), {
		waitUntil: "load",
		timeout: 20000,
	});
	await H.waitForAppReady(page, "workspace");
	await page
		.waitForFunction(
			(sel) => !!document.querySelector(sel),
			"#trace-body .trace-id",
			{ timeout: 10000 },
		)
		.catch(() => {});
	const traceResources = await resourceNames(page);
	const traceLoaded = loadedGraphLibs(traceResources);
	H.check(
		"trace.html 四个图脚本都被请求了",
		traceLoaded.length === GRAPH_LIBS.length,
		JSON.stringify({ loaded: traceLoaded, resources: traceResources.map(libBaseName) }),
	);
	// 顺序：defer 脚本的**执行序 = 文档序**（后者依赖前者，见 base.html 的图脚本槽位）。
	// 不能用 resource timeline 的顺序判 —— defer 是**并行下载**的，entries 的先后
	// 反映的是请求起跑时间，实测 graphs 会排在 flograph 前面（与执行序无关）。
	const scriptOrder = await page.evaluate(() =>
		[...document.querySelectorAll("script[src]")].map((s) => s.getAttribute("src")),
	);
	const libOrder = scriptOrder
		.map((src) => libBaseName(src || ""))
		.filter((b) => b.includes("dagre") || b === "flograph.js" || b === "graphs.js" || b === "traceview.js");
	H.check(
		"trace.html 图脚本顺序 dagre→flograph→graphs→traceview",
		JSON.stringify(libOrder) === JSON.stringify(GRAPH_LIBS),
		JSON.stringify({ libOrder, scriptOrder }),
	);
	const globals = await page.evaluate(() => ({
		traceView: typeof window.TraceView,
		graphs: typeof window.AnotifyGraphs,
	}));
	H.check(
		"trace 页图脚本真的执行了（TraceView / AnotifyGraphs 就绪）",
		globals.traceView === "object" && globals.graphs === "object",
		JSON.stringify(globals),
	);
	const traceBody = await page.evaluate(() => document.body.textContent || "");
	H.check("trace 页渲染出该 traceId", traceBody.includes(traceId), traceBody.slice(0, 160));
	H.check("trace 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	// ---------- E. #236 limit 硬顶（需要 ≥500 行，所以排在 B/C 之后） ----------
	console.log("--- E. /v1/events limit 硬顶与生效值回显 ---");
	insertSyntheticEvents(server.dbPath, nsId, {
		count: BOOST_EVENTS,
		firstIndex: TOTAL_EVENTS, // 编号继续递增
		newestAt: nowSec - 1000, // 比现有最旧的那条还旧，保持全局严格递减
	});
	const boosted = streamRows(server.dbPath, nsId).length;
	H.eq(`撑到 ${TOTAL_EVENTS + BOOST_EVENTS} 条（limit=501 需要库里 >500）`, boosted, TOTAL_EVENTS + BOOST_EVENTS);

	// 机器入口：Bearer API Key（notify:send），namespaceId 显式带上。
	const machine = (limit) =>
		H.req(
			base,
			`/v1/events?namespaceId=${encodeURIComponent(nsId)}&limit=${encodeURIComponent(limit)}`,
			{ key: s.sendKey },
		);
	const m100 = await machine(100);
	H.check(
		"机器入口 limit=100 → 100 条且回显 100",
		m100.status === 200 && m100.json?.events?.length === 100 && m100.json?.limit === 100,
		JSON.stringify({ status: m100.status, n: m100.json?.events?.length, limit: m100.json?.limit }),
	);
	const m500 = await machine(500);
	H.check(
		"机器入口 limit=500 → 500 条且回显 500",
		m500.json?.events?.length === 500 && m500.json?.limit === 500,
		JSON.stringify({ status: m500.status, n: m500.json?.events?.length, limit: m500.json?.limit }),
	);
	// #236 的关键回归：501..1000 曾经被 store 兜底成 50 条。
	const m501 = await machine(501);
	H.check(
		"机器入口 limit=501 → 500 条（#236 关键回归：不是 50）且回显 500",
		m501.json?.events?.length === 500 && m501.json?.limit === 500,
		JSON.stringify({ status: m501.status, n: m501.json?.events?.length, limit: m501.json?.limit }),
	);
	const m1000 = await machine(1000);
	H.check(
		"机器入口 limit=1000 → 500 条且回显 500",
		m1000.json?.events?.length === 500 && m1000.json?.limit === 500,
		JSON.stringify({ status: m1000.status, n: m1000.json?.events?.length, limit: m1000.json?.limit }),
	);
	const mAbc = await machine("abc");
	H.check(
		"机器入口 limit=abc → 回退默认 100 条且回显 100（不新增 400）",
		mAbc.status === 200 && mAbc.json?.events?.length === 100 && mAbc.json?.limit === 100,
		JSON.stringify({ status: mAbc.status, n: mAbc.json?.events?.length, limit: mAbc.json?.limit }),
	);

	const passed = H.summary("events_paging");
	await browser.close();
	server.stop();
	try {
		nodeConn?.close();
	} catch {
		/* 已断开 */
	}
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
	try {
		nodeConn?.close();
	} catch {
		/* ignore */
	}
	try {
		await browser?.close();
	} catch {
		/* ignore */
	}
	try {
		server?.stop();
	} catch {
		/* ignore */
	}
	process.exit(1);
});
