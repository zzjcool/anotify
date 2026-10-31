#!/usr/bin/env node
/* SUITE: events_paging — 批次 4B 的 e2e 套件（#234 / #235 / #236）
 *
 * 覆盖 .pi-legion/contract.md 的验收项：
 *
 *  V1  GET /v1/events?limit=501..1000 不再返回 50 条（#236 根因：store 对 >500 兜底成 50），
 *      返回 ≤500 条且**回显生效值** limit。
 *  V2  GET /v1/console/events?limit=1000 同理（≤500，回显）。
 *  V3  console events 支持 before=<cursor> 游标分页：1000 条事件能一路翻到**第 10 页**，
 *      无重复、无遗漏，且拼起来的 id 序列与「按 created_at DESC, id DESC」的期望**逐条相等**。
 *  V4  events 页有分页导航，URL 带 ?before=，刷新/分享后回到同一窗口（UI 也走满 10 页）。
 *  V5  events / index 不加载 dagre/flograph/graphs/traceview（-150.7KB / -4 请求），
 *      trace 页仍然四个都在（顺序 dagre→flograph→graphs→traceview）且能画图。
 *
 * 套件结构：
 *   A. 造数：1 条走真 POST（派发出任务，trace 页才有内容）+ 999 条直插库的合成事件
 *      → 事件流正好 1000 条，「10 页 × 100」这个边界是干净的确定值。
 *      同时构造期望顺序 expectedIds（真实事件在最前，其后按插入序），并与库里
 *      created_at DESC 的查询结果比一次 —— 期望值不靠猜。
 *   B. 后端游标分页：1000 条 → 恰好 10 页，逐页 100 条，10 页拼起来 === expectedIds
 *      逐条相等；另有 limit=37 的短页扫描（1000 条 / 37 → 最后一页 1 行）钉住
 *      「不足整页 ⇒ nextCursor 为空」这条收敛边界。
 *   C. events 页 UI：连点「更旧」9 次走满 10 页，末页行数/按钮/摘要/URL 换窗口全查。
 *   D. 资源分层（同一 Chrome 会话，events/index/trace 三页的 resource 清单）。
 *   E. #236 的 limit 硬顶（库里 1000 条 > 500，limit=501 刚好能看到夹取）。
 */
import { chromium } from "playwright-core";
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const PAGE_SIZE = 100; // 契约 §3.4：events 页页大小
const TOTAL_EVENTS = 1000; // 事件流总条数（真实 1 + 合成 999）→ 10 页 × 100
const SYNTHETIC_EVENTS = TOTAL_EVENTS - 1;
const EXPECTED_PAGES = TOTAL_EVENTS / PAGE_SIZE; // 10（整除，第 10 页也是满页）
const SHORT_LIMIT = 37; // 短页扫描：故意不整除，钉住「不足整页 ⇒ 末页收敛」
const SHORT_PAGES = Math.ceil(TOTAL_EVENTS / SHORT_LIMIT); // 28
const SHORT_LAST_ROWS = TOTAL_EVENTS % SHORT_LIMIT; // 1
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

// ---------- 造数：直插库（1000 次 POST 太慢，也会污染事件流口径） ----------
//
// 为什么直插而不是循环 POST /v1/events：
//   ① 1000 次 HTTP + 每次派发尝试要几十秒，套件会被 120s 预算吃掉；
//   ② POST 会尝试派发任务、可能写剪枝事件（source='route'），事件流条数就不再是
//      「我插入的条数」那种可预期的东西。
//
// 列名以 internal/store/schema.sql 的 events 表为准（迁移 12 之后是 namespace_id，
// 不再是 org_id）：id / namespace_id / trace_id / source / type / payload / idem_key / created_at。
//
// created_at 严格递减、id 编号递增（ORDER BY created_at DESC, id DESC 的稳定序），
// trace_id 每条唯一 —— UI 断言要靠「首行/末行 trace_id」来判定翻页真的换了窗口
// （卡片上不渲染事件 id，只渲染 source → type 和 trace_id）。
//
// 返回**插入顺序**（新 → 旧）的 id 数组：调用方用它拼期望顺序，不靠公式反推。
function insertSyntheticEvents(dbPath, namespaceId, { count, firstIndex, newestAt }) {
	const ids = [];
	const db = new DatabaseSync(dbPath);
	try {
		// 服务在跑（WAL），写事务会和服务端抢写锁：等不到就报 SQLITE_BUSY 而不是立刻失败。
		db.exec("PRAGMA busy_timeout=5000");
		// 一条事务插完：1000 次独立事务会写 1000 次 WAL，慢且无必要。
		db.exec("BEGIN IMMEDIATE");
		const stmt = db.prepare(
			`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
			 VALUES (?, ?, ?, 'e2e_page', 'e2e.tick', '{}', NULL, ?)`,
		);
		for (let i = 0; i < count; i++) {
			const idx = firstIndex + i;
			const suffix = String(idx).padStart(4, "0");
			const id = "evt_e2e_" + suffix;
			stmt.run(id, namespaceId, "tr_e2e_" + suffix, newestAt - i);
			ids.push(id);
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
	return ids;
}

// streamRows 读「事件流口径」的行（不含 source='route' 剪枝行，与 store.ListEvents
// 不带 traceId 时的剪枝规则一致），顺序就是契约里的事件序。
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

// 首行/末行的 trace_id（卡片按 DESC 渲染，首行 = 本页最新一条）。
async function listEdgeTraces(page) {
	return page.evaluate(() => {
		const all = [...document.querySelectorAll("#ev-list .card div.mono.truncate")];
		return {
			first: all.length ? all[0].textContent.trim() : "",
			last: all.length ? all[all.length - 1].textContent.trim() : "",
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

// firstDiff 给出两条 id 序列第一处不一致的位置（逐条相等的断言要能直接定位）。
function firstDiff(actual, expected) {
	const n = Math.min(actual.length, expected.length);
	for (let i = 0; i < n; i++) {
		if (actual[i] !== expected[i]) {
			return { index: i, actual: actual[i], expected: expected[i] };
		}
	}
	if (actual.length === expected.length) return null;
	return { index: n, actual: actual[n], expected: expected[n], lengthOnly: true };
}

// scanCursorPages 从第一页开始用响应里的 nextCursor 一路翻到底。
// 返回每页 { events, nextCursor, limit }；maxPages 是死循环护栏（不是断言值）。
async function scanCursorPages(fetchPage, limit, maxPages) {
	const pages = [];
	let before = "";
	for (let i = 0; ; i++) {
		if (i > maxPages) {
			throw new Error(`翻页超过 ${maxPages} 页仍未结束（nextCursor 一直非空？）`);
		}
		const res = await fetchPage(
			"limit=" + limit + (before ? "&before=" + encodeURIComponent(before) : ""),
		);
		const events = res.json?.events || [];
		const nextCursor = res.json?.nextCursor ?? null;
		pages.push({ status: res.status, events, nextCursor, limit: res.json?.limit });
		// 契约 §3.2：nextCursor 为 "" ⇔ 没有更旧的了。
		if (!nextCursor) break;
		before = nextCursor;
	}
	return pages;
}

async function main() {
	H.startTimer();
	console.log("=== SUITE: events_paging（#234 / #235 / #236）===");
	server = await H.startServer({ suiteName: "events_paging", rpId: "localhost" });
	const s = H.seed(server.dbPath, "events_paging");
	const base = server.base;
	// 控制台事件流入口（会话 Cookie）。
	const fetchPage = (qs) => H.req(base, "/v1/console/events?" + qs, { session: s.session });

	const nsId = await H.defaultNamespaceId(base, s.session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	// ---------- A. 造数：真实任务 trace + 999 条合成事件 = 1000 条 ----------
	// 配方同 deeplink.mjs：能力 + 入口边 + 真 WS 节点，事件才会派发出任务。
	// 事件本身 source='e2e.paging.src'（不是 'route'），因此它也会出现在事件流里。
	console.log("--- A. 造数：真实任务 trace + 合成事件 = 1000 条 ---");
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

	// V5 的 source 半边：source.html 也是「图页面」（#234 改了两页），需要一个**真源**
	// 才打得开详情页。建一个 cron 源，出口就是上面那个能力。
	const srcCreate = await H.req(base, "/v1/sources", {
		session: s.session,
		method: "POST",
		body: {
			name: "e2e.paging.cron",
			kind: "cron",
			config: { expr: "0 3 * * *", tz: "Asia/Shanghai" },
			targets: [capId],
		},
	});
	const sourceId = srcCreate.json?.source?.id;
	H.check(
		"建 cron 源 e2e.paging.cron（出口 e2e.paging）→ 201（source.html 的 V5 半边需要真源）",
		srcCreate.status === 201 && typeof sourceId === "string" && sourceId.length > 0,
		JSON.stringify({ status: srcCreate.status, sourceId, body: srcCreate.text?.slice(0, 200) }),
	);

	const node = await N.bringUpNode(
		base,
		server.dbPath,
		nsId,
		"events-paging-node",
		[{ id: capId, name: "e2e.paging", version: 1 }],
	);
	nodeConn = node.conn;

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
	H.check(
		"真实事件建出任务（trace 页有内容）",
		(realEv.json?.tasks || []).length >= 1,
		JSON.stringify({
			tasks: realEv.json?.tasks,
			pruned: realEv.json?.pruned,
			origin: realEv.json?.origin,
		}),
	);
	H.check("拿到真实 traceId", typeof traceId === "string" && traceId.length > 0, String(traceId));

	// 真实事件的入库值（created_at / id）**不猜**：此刻事件流里只有它，limit=1 拿到的就是它。
	const newest = await fetchPage("limit=1");
	const realEvent = newest.json?.events?.[0];
	H.check(
		"真实事件是当前最新一条（console limit=1 拿到它）",
		!!realEvent?.id && realEvent?.traceId === traceId,
		JSON.stringify({ id: realEvent?.id, traceId: realEvent?.traceId, createdAt: realEvent?.createdAt }),
	);
	if (!realEvent?.id) throw new Error("拿不到真实事件的入库行：" + newest.text?.slice(0, 200));

	// 合成事件：比真实事件旧 10s 起（严格递减），绝不与真实事件撞 created_at。
	const syntheticIds = insertSyntheticEvents(server.dbPath, nsId, {
		count: SYNTHETIC_EVENTS,
		firstIndex: 1,
		newestAt: realEvent.createdAt - 10,
	});
	// 期望顺序 = 「真实事件（最新）」+ 合成事件的插入序（插入序就是 created_at 降序）。
	const expectedIds = [realEvent.id, ...syntheticIds];
	const rows = streamRows(server.dbPath, nsId);
	const total = rows.length;
	H.eq(`事件流恰好 ${TOTAL_EVENTS} 条（真实 1 + 合成 ${SYNTHETIC_EVENTS}）`, total, TOTAL_EVENTS);
	H.check(
		"期望顺序（真实事件在前 + 插入序）与库里 created_at DESC 序一致",
		JSON.stringify(rows.map((r) => r.id)) === JSON.stringify(expectedIds),
		JSON.stringify(firstDiff(rows.map((r) => r.id), expectedIds)),
	);
	if (total !== TOTAL_EVENTS) throw new Error(`事件流 ${total} 条，与预期的 ${TOTAL_EVENTS} 不一致`);

	// ---------- B. #235 游标分页：1000 条 → 恰好 10 页 ----------
	console.log("--- B. /v1/console/events 游标分页（1000 条 → 10 页）---");
	const pages = await scanCursorPages(fetchPage, PAGE_SIZE, EXPECTED_PAGES + 5);
	H.eq(`游标翻页恰好 ${EXPECTED_PAGES} 页（第 10 页之后 nextCursor 为空）`, pages.length, EXPECTED_PAGES);
	H.check(
		`逐页 ${PAGE_SIZE} 条（第 1..10 页都是满页）`,
		pages.every((p) => p.events.length === PAGE_SIZE),
		JSON.stringify(pages.map((p) => p.events.length)),
	);
	H.check(
		`每页都回显生效值 limit=${PAGE_SIZE}`,
		pages.every((p) => p.limit === PAGE_SIZE),
		JSON.stringify(pages.map((p) => p.limit)),
	);
	const pagedIds = pages.flatMap((p) => p.events.map((e) => e.id));
	const apiPage10FirstId = pages[EXPECTED_PAGES - 1].events[0]?.id;
	const apiPage10LastId = pages[EXPECTED_PAGES - 1].events.at(-1)?.id;
	H.check(
		`10 页拼起来与 expectedIds 逐条相等（共 ${TOTAL_EVENTS} 条，created_at 排序期望一致）`,
		firstDiff(pagedIds, expectedIds) === null,
		JSON.stringify(firstDiff(pagedIds, expectedIds)),
	);
	H.check(
		`10 页之间无重复（${TOTAL_EVENTS} 行 / 去重后 ${TOTAL_EVENTS}）`,
		pagedIds.length === TOTAL_EVENTS && new Set(pagedIds).size === TOTAL_EVENTS,
		JSON.stringify({ rows: pagedIds.length, unique: new Set(pagedIds).size }),
	);
	// 第 10 页证据（直接写进日志，收队时不用重跑）：
	console.log(
		`  ↳ API 第 ${EXPECTED_PAGES} 页：行数=${pages[EXPECTED_PAGES - 1].events.length}` +
			` 首 id=${apiPage10FirstId} 末 id=${apiPage10LastId}` +
			` nextCursor=${JSON.stringify(pages[EXPECTED_PAGES - 1].nextCursor)}`,
	);
	H.eq(
		"第 10 页 nextCursor 为空（peek 补丁生效：不再多给一个空页）",
		pages[EXPECTED_PAGES - 1].nextCursor,
		"",
	);
	// 每页内部 created_at 不增，且页间不断档（DESC 全序）。
	const descOk =
		pages.every((p) => p.events.every((e, i) => i === 0 || p.events[i - 1].createdAt >= e.createdAt)) &&
		pages.every((p, i) => i === 0 || pages[i - 1].events.at(-1).createdAt >= p.events[0].createdAt);
	H.check("每页 createdAt 不增且页间连续（DESC 全序）", descOk);

	// 不足整页的收敛边界：limit=37（1000 不整除）→ 28 页，最后一页 1 行。
	const shortPages = await scanCursorPages(fetchPage, SHORT_LIMIT, SHORT_PAGES + 5);
	H.eq(`limit=${SHORT_LIMIT} 扫到底共 ${SHORT_PAGES} 页（ceil(1000/37)）`, shortPages.length, SHORT_PAGES);
	H.eq(
		`limit=${SHORT_LIMIT} 最后一页只有 ${SHORT_LAST_ROWS} 行（1000 % 37，不足整页 ⇒ 收敛）`,
		shortPages.at(-1).events.length,
		SHORT_LAST_ROWS,
	);
	H.eq(`limit=${SHORT_LIMIT} 末页 nextCursor 为空`, shortPages.at(-1).nextCursor, "");
	const shortIds = shortPages.flatMap((p) => p.events.map((e) => e.id));
	H.check(
		`limit=${SHORT_LIMIT} 扫到底无重复无遗漏（逐条一致 + 去重 ${TOTAL_EVENTS}）`,
		firstDiff(shortIds, expectedIds) === null && new Set(shortIds).size === TOTAL_EVENTS,
		JSON.stringify({
			diff: firstDiff(shortIds, expectedIds),
			rows: shortIds.length,
			unique: new Set(shortIds).size,
		}),
	);

	// 非法游标一律 400 —— 不静默降级回第一页（那正是 #236 那类「悄悄换窗口」的病根）。
	const badCursor = await fetchPage("limit=" + PAGE_SIZE + "&before=" + encodeURIComponent("这不是游标"));
	H.eq("坏游标 before → 400", badCursor.status, 400);
	H.eq("坏游标 error 文案", badCursor.json?.error, "invalid before cursor");

	// V2（强形态）：库里 1000 条时，控制台端点（会话 Cookie）limit=1000 真的钳到 500，
	// 而且给的是**最新 500 条**（首尾 id 对齐 expectedIds 前缀）—— 不是「库里不足 500
	// 所以看起来对」。
	const consoleFull = await fetchPage("limit=1000&namespaceId=" + encodeURIComponent(nsId));
	H.eq("console limit=1000 → 500 条（硬顶）", consoleFull.json?.events?.length ?? -1, 500);
	H.eq("console limit=1000 回显生效值", consoleFull.json?.limit, 500);
	H.check(
		"V2 强形态：钳到的最新 500 条 == expectedIds[0..499]（首尾 id 逐条对齐）",
		consoleFull.json?.events?.[0]?.id === expectedIds[0] &&
			consoleFull.json?.events?.at(-1)?.id === expectedIds[499],
		JSON.stringify({
			first: consoleFull.json?.events?.[0]?.id,
			last: consoleFull.json?.events?.at(-1)?.id,
			expectedFirst: expectedIds[0],
			expectedLast: expectedIds[499],
		}),
	);

	// ---------- C/D. Chrome：events 页走满 10 页 + 资源分层 ----------
	console.log("--- C. events 页 UI 分页（Chrome 1280×800，连点 9 次到第 10 页）---");
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
	// 先守住「清单真的是非空的」：清单为空时，下面的**否定**断言会真空为真。
	H.check(
		"events.html resource 清单非空（否定断言的前置守卫，防真空通过）",
		eventsResources.length > 5,
		JSON.stringify({ n: eventsResources.length, resources: eventsResources.map(libBaseName) }),
	);
	const eventsLoaded = loadedGraphLibs(eventsResources);
	H.check(
		"events.html 不加载 dagre/flograph/graphs/traceview（V5）",
		eventsLoaded.length === 0,
		JSON.stringify({ loaded: eventsLoaded, resources: eventsResources.map(libBaseName) }),
	);

	// 连点「更旧」9 次 → 第 2..10 页，每次等首行 trace_id 变化（禁止固定 sleep）。
	const uiCounts = [];
	let prevTrace = firstTrace1;
	let hasNewerOnPage2 = false;
	const clickStart = Date.now();
	for (let p = 2; p <= EXPECTED_PAGES; p++) {
		await page.click("#ev-older");
		await waitForList(page, PAGE_SIZE, prevTrace);
		info = await listInfo(page);
		uiCounts.push(info.count);
		if (p === 2) hasNewerOnPage2 = info.hasNewer;
		prevTrace = info.firstTrace;
	}
	const clickMs = Date.now() - clickStart;
	console.log(
		`  ↳ UI 连点「更旧」${EXPECTED_PAGES - 1} 次耗时 ${clickMs}ms（每页等首行 trace_id 变化，无固定 sleep）`,
	);
	H.check(
		`连点「更旧」${EXPECTED_PAGES - 1} 次 → 第 2..10 页每页 ${PAGE_SIZE} 行`,
		uiCounts.length === EXPECTED_PAGES - 1 && uiCounts.every((c) => c === PAGE_SIZE),
		JSON.stringify({ counts: uiCounts, clickMs }),
	);
	H.check("第 2 页有「较新」按钮", hasNewerOnPage2, JSON.stringify({ hasNewerOnPage2 }));

	// 第 10 页：满页、已到最旧、与 API 第 10 页同一窗口。
	const page10 = await listInfo(page);
	const edges10 = await listEdgeTraces(page);
	const apiPage10 = pages[EXPECTED_PAGES - 1].events;
	H.eq(`第 ${EXPECTED_PAGES} 页 ${PAGE_SIZE} 行`, page10.count, PAGE_SIZE);
	H.check(`第 ${EXPECTED_PAGES} 页「更旧」按钮消失（已到最旧）`, !page10.hasOlder, JSON.stringify(page10));
	H.check(`第 ${EXPECTED_PAGES} 页摘要有「已到最旧」`, page10.summary.includes("已到最旧"), page10.summary);
	H.check(`第 ${EXPECTED_PAGES} 页有「较新」按钮`, page10.hasNewer, JSON.stringify(page10));
	H.eq("第 10 页首行 trace_id 与 API 第 10 页首条一致", edges10.first, apiPage10[0]?.traceId);
	H.eq("第 10 页末行 trace_id 与 API 第 10 页末条一致", edges10.last, apiPage10.at(-1)?.traceId);
	const before10 = beforeOf(page.url());
	H.check("第 10 页 URL 带 before=", before10.length > 0, page.url());
	console.log(
		`  ↳ UI 第 ${EXPECTED_PAGES} 页：行数=${page10.count} #ev-older存在=${page10.hasOlder}` +
			` 首行 trace=${edges10.first} 末行 trace=${edges10.last} 摘要="${page10.summary}"`,
	);

	// V4：把第 10 页的 URL 直接打开（刷新/分享）→ 仍在同一窗口。
	await page.goto(base + "/events.html?before=" + encodeURIComponent(before10), {
		waitUntil: "load",
		timeout: 20000,
	});
	await H.waitForAppReady(page, "workspace");
	await waitForList(page, PAGE_SIZE);
	info = await listInfo(page);
	H.eq("直接打开第 10 页 URL → 100 行", info.count, PAGE_SIZE);
	H.eq("刷新后首行 trace_id 不变（第 10 页同一窗口，V4）", info.firstTrace, edges10.first);
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
	H.check(
		"index.html resource 清单非空（否定断言的前置守卫，防真空通过）",
		indexResources.length > 5,
		JSON.stringify({ n: indexResources.length }),
	);
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

	// V5 的另一半：#234 改的是两页（trace + source）。source.html 也必须在图脚本上齐全。
	errs.length = 0;
	await page.goto(base + "/source.html?id=" + encodeURIComponent(sourceId), {
		waitUntil: "load",
		timeout: 20000,
	});
	await H.waitForAppReady(page, "workspace");
	await page
		.waitForFunction(() => (document.getElementById("head-name") || {}).textContent?.trim(), null, {
			timeout: 10000,
		})
		.catch(() => {});
	const sourceResources = await resourceNames(page);
	H.check(
		"source.html resource 清单非空（否定/正向断言的前置守卫）",
		sourceResources.length > 5,
		JSON.stringify({ n: sourceResources.length }),
	);
	const sourceLoaded = loadedGraphLibs(sourceResources);
	H.check(
		"source.html 四个图脚本都被请求了",
		sourceLoaded.length === GRAPH_LIBS.length,
		JSON.stringify({ loaded: sourceLoaded, resources: sourceResources.map(libBaseName) }),
	);
	const sourceLibOrder = (
		await page.evaluate(() =>
			[...document.querySelectorAll("script[src]")].map((s) => s.getAttribute("src")),
		)
	)
		.map((src) => libBaseName(src || ""))
		.filter((b) => b.includes("dagre") || b === "flograph.js" || b === "graphs.js" || b === "traceview.js");
	H.check(
		"source.html 图脚本顺序 dagre→flograph→graphs→traceview",
		JSON.stringify(sourceLibOrder) === JSON.stringify(GRAPH_LIBS),
		JSON.stringify({ sourceLibOrder }),
	);
	const sourceGlobals = await page.evaluate(() => ({
		graphs: typeof window.AnotifyGraphs,
		flow: typeof window.FlowGraph,
		name: (document.getElementById("head-name") || {}).textContent?.trim() || "",
	}));
	H.check(
		"source 页图脚本真的执行了（AnotifyGraphs / FlowGraph 就绪）",
		sourceGlobals.graphs === "object" && sourceGlobals.flow === "object",
		JSON.stringify(sourceGlobals),
	);
	H.check("source 页渲染出该源名称", sourceGlobals.name.includes("e2e.paging.cron"), JSON.stringify(sourceGlobals));
	H.check("source 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	// ---------- E. #236 limit 硬顶（库里 1000 条 > 500，天然满足） ----------
	console.log("--- E. /v1/events limit 硬顶与生效值回显 ---");
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
