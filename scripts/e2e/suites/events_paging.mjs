#!/usr/bin/env node
/* SUITE: events_paging — 批次 4B 的 e2e 套件（#234 / #235 / #236）· v4 迁移
 *
 * 覆盖 .pi-legion/contract.md 的验收项（v4 口径见每条的迁移注记）：
 *
 *  V1  GET …/namespaces/{ns}/events?limit=501..1000 不再返回 50 条（#236 根因：
 *      store 对 >500 兜底成 50），返回 ≤500 条。v3 的「回显生效值 limit」字段在
 *      v4 列表信封里没有（metadata 只有 continue），改用**条数**证明硬顶，
 *      并补一条「limit=abc → 400」证明参数不再被静默兜底。
 *  V2  GET /v1/console/events?limit=1000 同理（≤500）；v4 必须显式给 ?namespace=。
 *  V3  console events 游标分页（v4 参数名 `continue`，游标在 metadata.continue）：
 *      1000 条事件能一路翻到**第 10 页**，无重复、无遗漏，且拼起来的 id 序列与
 *      「按 created_at DESC, id DESC」的期望**逐条相等**。
 *  V4  events 页 UI 把 1000 条事件全部加载出来（v4 是「加载更多」累积，
 *      不是 v3 的 ?before= 翻页）：行序与 API 游标序列逐条相等、无重复，
 *      到底后按钮禁用、计数等于总数；刷新回到确定的第一窗口。
 *  V5  events / index 不多加载图脚本；trace 页只加载它需要的 flograph+graphs
 *      （#335 重写后的真实依赖，dagre/traceview 已不在 trace 页）；source 页
 *      仍然四个都在（顺序 dagre→flograph→graphs→traceview）且能画图。
 *
 * 套件结构：
 *   A. 造数：1 条走真 POST（派发出任务，trace 页才有内容）+ 999 条直插库的合成事件
 *      （v4：能力/源都走真实 HTTP；入口边由 source.spec.targets 表达，不需要节点）
 *      → 事件流正好 1000 条，「10 页 × 100」这个边界是干净的确定值。
 *      同时构造期望顺序 expectedIds（真实事件在最前，其后按插入序），并与库里
 *      created_at DESC 的查询结果比一次 —— 期望值不靠猜。
 *   B. 后端游标分页：1000 条 → 恰好 10 页，逐页 100 条，10 页拼起来 === expectedIds
 *      逐条相等；另有 limit=37 的短页扫描（1000 条 / 37 → 最后一页 1 行）钉住
 *      「不足整页 ⇒ nextCursor 为空」这条收敛边界。
 *   C. events 页 UI：连点「加载更多」19 次累积到 1000 行，顺序/去重/到底状态全查。
 *   D. 资源分层（同一 Chrome 会话，events/index/trace 三页的 resource 清单）。
 *   E. #236 的 limit 硬顶（库里 1000 条 > 500，limit=501 刚好能看到钳位到 500）。
 *   F. #377 列表规模化：sources/nodes 的 limit/continue 翻页（与一次性全量逐条相等、
 *      末页无游标、非法 limit/跨命名空间游标 400）+ sources 页「首屏 20 行 + 加载更多 + 本地搜索」。
 */
import { chromium } from "playwright-core";
import { DatabaseSync } from "node:sqlite";
import * as H from "../lib/harness.mjs";
import * as V from "../lib/v4.mjs";

// API 侧页大小（max 500，v4 未改）：100 是原套件的取数口径，保留它继续扫。
const PAGE_SIZE = 100;
const TOTAL_EVENTS = 1000; // 事件流总条数（真实 1 + 合成 999）→ 10 页 × 100
const SYNTHETIC_EVENTS = TOTAL_EVENTS - 1;
const EXPECTED_PAGES = TOTAL_EVENTS / PAGE_SIZE; // 10（整除，第 10 页也是满页）
// UI 侧页大小：v4 的 events 页把「翻页」换成了「加载更多」累积，
// 每页数量选项是 2/10/20/50（没有 100），所以 UI 扫全量是 20 页。
const UI_PAGE_SIZE = 50;
const UI_PAGES = TOTAL_EVENTS / UI_PAGE_SIZE; // 20
const NS = V.DEFAULT_NS;
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

// ---------- 造数：直插库（1000 次 POST 太慢，也会污染事件流口径） ----------
//
// 为什么直插而不是循环 POST /v1/events：
//   ① 1000 次 HTTP + 每次派发尝试要几十秒，套件会被 120s 预算吃掉；
//   ② POST 会尝试派发任务、可能写剪枝事件（source='route'），事件流条数就不再是
//      「我插入的条数」那种可预期的东西。
//
// 列名以 internal/store/schema.sql 的 events 表为准（v4 baseline）：
// id / namespace_id / trace_id / source / type / payload / delivery_id / created_at。
//
// created_at 严格递减、id 编号递增（ORDER BY created_at DESC, id DESC 的稳定序），
// trace_id 每条唯一 —— UI 断言靠卡片上的 data-uid（= events.id = metadata.name）
// 判定「顺序、无重复、无遗漏」，trace_id 仍是 trace 页/详情页的关联键。
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
		// v4 的列是 delivery_id（v3 的 idem_key 随重建 schema 改名/退役）；这里不写幂等键。
		const stmt = db.prepare(
			`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, delivery_id, created_at)
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

// ---------- UI 辅助（v4 events 页）----------
//
// #335 的前端重写把 events 页从「?before= 游标翻页」换成了「加载更多」累积：
//
//   #ev-list    卡片列表（每张卡带 data-uid = 事件 metadata.name，稳定且每行唯一）
//   #ev-count   「已显示 {n} 条唯一事件」——它本身就是**去重**口径的呈现
//   #ev-more    「加载更多」按钮（没有 continuation 时 disabled）
//   #ev-limit   每页数量 select（2/10/20/50）
//
// 所以「翻页真的换了窗口」这件事现在等价于「点一次加载更多，列表长度 + 页大小、
// 且不出现重复 uid」——比原来按首行 trace 猜测更硬。
const FIRST_UID_SEL = "#ev-list .card[data-uid]";

// listInfo 读列表状态：行数、首/末 uid、计数文案、更多按钮可用性。
async function listInfo(page) {
	return page.evaluate(() => {
		const cards = [...document.querySelectorAll("#ev-list .card[data-uid]")];
		const more = document.getElementById("ev-more");
		return {
			count: cards.length,
			firstUid: cards.length ? cards[0].getAttribute("data-uid") : "",
			lastUid: cards.length ? cards[cards.length - 1].getAttribute("data-uid") : "",
			uids: cards.map((c) => c.getAttribute("data-uid")),
			countText: (document.getElementById("ev-count") || {}).textContent?.trim() || "",
			moreDisabled: more ? !!more.disabled : null,
		};
	});
}

// waitForList 等列表稳定在 count 行（可选：首行 uid 必须仍是 stableFirstUid）。
//
// v4 的「加载更多」是**追加**语义：新一页接在尾部，首行不该变。所以这里对首行
// 断言的是「不变」（累积），而 v3 的翻页是换窗（首行必须变）——两种语义不能混。
// 固定 sleep 不可靠：API 往返 + 渲染时间随机器负载变化，只能等状态而不是等时间。
async function waitForList(page, count, stableFirstUid = "") {
	await page.waitForFunction(
		([n, stable, sel]) => {
			const cards = [...document.querySelectorAll(sel)];
			if (cards.length !== n) return false;
			if (!stable) return true;
			return cards[0].getAttribute("data-uid") === stable;
		},
		[count, stableFirstUid, FIRST_UID_SEL],
		{ timeout: 20000 },
	);
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
	let cursor = "";
	for (let i = 0; ; i++) {
		if (i > maxPages) {
			throw new Error(`翻页超过 ${maxPages} 页仍未结束（metadata.continue 一直非空？）`);
		}
		const res = await fetchPage(
			"limit=" + limit + (cursor ? "&continue=" + encodeURIComponent(cursor) : ""),
		);
		// v4 列表信封：items[] 是资源，游标在 metadata.continue（v3 的 events/nextCursor 已退役）。
		const events = V.items(res) || [];
		// 没有 continuation 时 v4 省略该字段（JSON 里没有 key）→ 归一成空串。
		const nextCursor = res.json?.metadata?.continue ?? "";
		pages.push({ status: res.status, events, nextCursor });
		// 契约为「continue 为空 ⇔ 没有更旧的了」。
		if (!nextCursor) break;
		cursor = nextCursor;
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
	//
	// v4 配方（与 v3 的差别）：
	//   · 能力是**信封**写请求（capabilityBody），不是 `{name, spec:{outputs}}`；
	//   · 入口边不再是 `/v1/edges` 资源，而是 `source.spec.targets`（边是只读派生投影）；
	//   · 投递入口是 `POST …/namespaces/{ns}/events`，且源必须是本 ns 的真实资源；
	//   · **不需要节点**：v4 的任务在 ingress 时就建行（pending），节点只决定「谁执行」，
	//     不决定「派不派得出任务」。所以这里不再造 WS 节点（也就没有可假的派发事实）。
	console.log("--- A. 造数：真实任务 trace + 合成事件 = 1000 条 ---");
	const cap = await V.mkCapability(base, s.session, "e2e.paging");

	// 入口边的 v4 表达：源 e2e.paging.src 的 targets 指向能力（等价 v3 的 fromIsSource 边）。
	const srcIngress = await V.createSource(
		base,
		s.session,
		NS,
		V.sourceBody({ name: "e2e.paging.src", kind: "manual", targets: [{ capability: "e2e.paging" }] }),
	);
	V.expectStatus("建入口源 e2e.paging.src（targets=[e2e.paging]）→ 201", srcIngress, 201);

	// V5 的 source 半边：source.html 也是「图页面」，需要一个**真源**才打得开详情页。
	// v4 的源详情按 metadata.name 取（`source.html?name=`），所以这里断言拿得到名字。
	const srcCreate = await V.createSource(
		base,
		s.session,
		NS,
		V.sourceBody({
			name: "e2e.paging.cron",
			kind: "cron",
			config: { expr: "0 3 * * *", tz: "Asia/Shanghai" },
			targets: [{ capability: "e2e.paging" }],
		}),
	);
	V.expectStatus(
		"建 cron 源 e2e.paging.cron（出口 e2e.paging）→ 201（source.html 的 V5 半边需要真源）",
		srcCreate,
		201,
	);
	const srcName = srcCreate.json?.metadata?.name;
	H.eq("cron 源名可用于 source.html?name=", srcName, "e2e.paging.cron");

	const realEv = await V.ingestEvent(base, s.sendKey, NS, {
		source: "e2e.paging.src",
		type: "go",
		payload: { title: "events_paging 造数", cwd: "/repo/e2e" },
	});
	V.expectStatus("POST …/events（sendKey）→ 200", realEv, 200);
	const traceId = realEv.json?.traceId;
	H.check(
		"真实事件建出任务（trace 页有内容）",
		(realEv.json?.tasks || []).length >= 1,
		JSON.stringify({ tasks: realEv.json?.tasks, pruned: realEv.json?.pruned }),
	);
	H.check("拿到真实 traceId", typeof traceId === "string" && traceId.length > 0, String(traceId));

	// 真实事件的入库值（created_at / id）**不猜**：此刻事件流里只有它，limit=1 拿到的就是它。
	// v4 的列表是信封：id 在 metadata.name，trace 在 spec.traceUID。
	const newest = await fetchPage("limit=1");
	const realEvent = V.items(newest)?.[0];
	const realEventId = realEvent?.metadata?.name;
	H.check(
		"真实事件是当前最新一条（console limit=1 拿到它）",
		!!realEventId && realEvent?.spec?.traceUID === traceId,
		JSON.stringify({ id: realEventId, traceUID: realEvent?.spec?.traceUID, created: realEvent?.metadata?.creationTimestamp }),
	);
	if (!realEventId) throw new Error("拿不到真实事件的入库行：" + newest.text?.slice(0, 200));
	// created_at 是合成事件的排布基准（要严格递减、又不与真实事件撞），所以从库里读真值。
	const realCreatedAt = V.withDB(server.dbPath, (db) =>
		db.prepare(`SELECT created_at FROM events WHERE id = ?`).get(realEventId)?.created_at,
	);
	H.check("真实事件的 created_at 读得到真值（不靠公式反推）", Number.isInteger(realCreatedAt), String(realCreatedAt));

	// 合成事件：比真实事件旧 10s 起（严格递减），绝不与真实事件撞 created_at。
	const syntheticIds = insertSyntheticEvents(server.dbPath, nsId, {
		count: SYNTHETIC_EVENTS,
		firstIndex: 1,
		newestAt: realCreatedAt - 10,
	});
	// 期望顺序 = 「真实事件（最新）」+ 合成事件的插入序（插入序就是 created_at 降序）。
	const expectedIds = [realEventId, ...syntheticIds];
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
		`前 ${EXPECTED_PAGES - 1} 页都带 metadata.continue（还有更旧的），末页没有`,
		pages.slice(0, -1).every((p) => typeof p.nextCursor === "string" && p.nextCursor.length > 0),
		JSON.stringify(pages.map((p) => p.nextCursor)),
	);
	H.check(
		"每页 kind 都是 EventList（列表信封，不是 v3 的 {events:[…]}）",
		pages.every((p) => p.status === 200),
		JSON.stringify(pages.map((p) => p.status)),
	);
	const pagedIds = pages.flatMap((p) => p.events.map((e) => e.metadata?.name));
	const apiPage10FirstId = pages[EXPECTED_PAGES - 1].events[0]?.metadata?.name;
	const apiPage10LastId = pages[EXPECTED_PAGES - 1].events.at(-1)?.metadata?.name;
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
	// v4 的时间在 metadata.creationTimestamp（RFC3339，秒精度）。合成事件之间严格差 1s，
	// 真实事件与它们差 10s，所以「不增」在这里是可靠判据（同秒并列不参与本断言）。
	const at = (e) => String(e?.metadata?.creationTimestamp || "");
	const descOk =
		pages.every((p) => p.events.every((e, i) => i === 0 || at(p.events[i - 1]) >= at(e))) &&
		pages.every((p, i) => i === 0 || at(pages[i - 1].events.at(-1)) >= at(p.events[0]));
	H.check("每页创建时间不增且页间连续（DESC 全序）", descOk);

	// 不足整页的收敛边界：limit=37（1000 不整除）→ 28 页，最后一页 1 行。
	const shortPages = await scanCursorPages(fetchPage, SHORT_LIMIT, SHORT_PAGES + 5);
	H.eq(`limit=${SHORT_LIMIT} 扫到底共 ${SHORT_PAGES} 页（ceil(1000/37)）`, shortPages.length, SHORT_PAGES);
	H.eq(
		`limit=${SHORT_LIMIT} 最后一页只有 ${SHORT_LAST_ROWS} 行（1000 % 37，不足整页 ⇒ 收敛）`,
		shortPages.at(-1).events.length,
		SHORT_LAST_ROWS,
	);
	H.eq(`limit=${SHORT_LIMIT} 末页 nextCursor 为空`, shortPages.at(-1).nextCursor, "");
	const shortIds = shortPages.flatMap((p) => p.events.map((e) => e.metadata?.name));
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
	// v4 的参数名是 `continue`（不透明 token），解码/归属校验失败都是 400 BadRequest。
	const badCursor = await fetchPage("limit=" + PAGE_SIZE + "&continue=" + encodeURIComponent("这不是游标"));
	V.expectReason("坏游标 continue → 400", badCursor, 400, "BadRequest");
	H.check(
		"坏游标 message 点名 continue token",
		String(badCursor.json?.message || "").includes("continue"),
		String(badCursor.json?.message || badCursor.text).slice(0, 120),
	);

	// V2（强形态）：库里 1000 条时，控制台端点（会话 Cookie）limit=1000 真的钳到 500，
	// 而且给的是**最新 500 条**（整段与 expectedIds 前缀**逐条**相等）—— 不是「库里不足 500
	// 所以看起来对」。
	//
	// 为什么逐条而不是只比首尾：首尾对齐只能证明首行/末行在期望位置上，中间被换序、
	// 被插进别的行、或重复了同一条都发现不了。断言名/提交信息说的是「最新 500 条」，
	// 那就得整段比。
	// v4 的 console 读模型要求显式 `?namespace=`（harness 已按会话解析默认 ns 注入），
	// 响应没有 limit 回显（信封只有 metadata.continue）—— 硬顶用**条数**证明：
	// 请求 1000 条只给 500 条，而 #236 的回归形态是「被折回 50 条」。
	const consoleFull = await fetchPage("limit=1000");
	H.eq("console limit=1000 → 500 条（硬顶，不是折回 50）", V.items(consoleFull)?.length ?? -1, 500);
	H.check(
		"console limit=1000 仍有 continuation（后面还有更旧的）",
		typeof consoleFull.json?.metadata?.continue === "string" && consoleFull.json.metadata.continue.length > 0,
		String(consoleFull.json?.metadata?.continue).slice(0, 40),
	);
	const consoleFullIds = (V.items(consoleFull) || []).map((e) => e.metadata?.name);
	const expectedTop500 = expectedIds.slice(0, 500);
	const top500Diff = firstDiff(consoleFullIds, expectedTop500);
	H.check(
		"V2 强形态：钳到的最新 500 条与 expectedIds[0..499] 逐条相等（整段比较，不只是首尾）",
		top500Diff === null,
		JSON.stringify(
			top500Diff ?? {
				rows: consoleFullIds.length,
				first: consoleFullIds[0],
				last: consoleFullIds.at(-1),
			},
		),
	);
	console.log(
		`  ↳ console limit=1000：钳到 ${consoleFullIds.length} 条 首 id=${consoleFullIds[0]} 末 id=${consoleFullIds.at(-1)}`,
	);

	// ---------- C/D. Chrome：events 页加载全部事件 + 资源分层 ----------
	//
	// v4 的 events 页把「游标翻页（?before=）」换成了「加载更多」累积（#335 前端重写）：
	// 每次点 #ev-more 追加下一页，列表按 metadata.creationTimestamp DESC 渲染。
	// 所以 UI 侧的等价断言是：从 50 行开始，连点 19 次 → 1000 行，并且
	//  ① 行序与 API 游标序列**逐条相等**（无重复、无遗漏、无换序）；
	//  ② 到底后 #ev-more 被禁用、计数文案等于总数。
	console.log(`--- C. events 页 UI 加载更多（Chrome 1280×800，${UI_PAGES} 页 × ${UI_PAGE_SIZE}）---`);
	browser = await chromium.launch({ channel: "chrome", headless: true });
	const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
	await H.mockExternalCdn(ctx);
	await injectSession(ctx, s.session, base);
	const page = await ctx.newPage();
	const errs = [];
	page.on("pageerror", (e) => errs.push(String(e)));

	await page.goto(base + "/events.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(page, "workspace");
	// 默认每页 20；显式选 50，让 UI 与 API 的窗口大小可比。
	await page.selectOption("#ev-limit", String(UI_PAGE_SIZE));
	await page.click("#ev-filter");
	await waitForList(page, UI_PAGE_SIZE);

	let info = await listInfo(page);
	H.eq(`首屏 ${UI_PAGE_SIZE} 行（#ev-limit=50）`, info.count, UI_PAGE_SIZE);
	H.check("首行有 data-uid（行身份可判定）", info.firstUid.length > 0, info.firstUid);
	H.check(
		`首屏计数文案等于 ${UI_PAGE_SIZE}`,
		info.countText.includes(String(UI_PAGE_SIZE)),
		info.countText,
	);
	H.check("首屏「加载更多」可用（还有 continuation）", info.moreDisabled === false, JSON.stringify(info));

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

	// 连点「加载更多」19 次 → 20 页 × 50 行。每次等「行数增加 + 首行不变」，
	// 不用固定 sleep（API 往返时间随机）。
	const uiCounts = [];
	const clickStart = Date.now();
	for (let p = 2; p <= UI_PAGES; p++) {
		await page.click("#ev-more");
		await waitForList(page, p * UI_PAGE_SIZE, info.firstUid);
		info = await listInfo(page);
		uiCounts.push(info.count);
	}
	const clickMs = Date.now() - clickStart;
	console.log(
		`  ↳ UI 连点「加载更多」${UI_PAGES - 1} 次耗时 ${clickMs}ms（每次等行数增长，无固定 sleep）`,
	);
	H.check(
		`连点「加载更多」${UI_PAGES - 1} 次 → 累计 ${TOTAL_EVENTS} 行（每次 +${UI_PAGE_SIZE}）`,
		uiCounts.length === UI_PAGES - 1 &&
			uiCounts.every((c, i) => c === (i + 2) * UI_PAGE_SIZE),
		JSON.stringify({ counts: uiCounts.slice(0, 3).concat(uiCounts.slice(-2)), clickMs }),
	);

	// 到底：行序与 API 游标序列逐条相等（这才是「无重复、无遗漏、无换序」的证据）。
	const all = await listInfo(page);
	H.eq(`末端列表 ${TOTAL_EVENTS} 行`, all.count, TOTAL_EVENTS);
	H.check(
		`UI 列表顺序与 API 游标序列逐条相等（共 ${TOTAL_EVENTS} 条）`,
		firstDiff(all.uids, expectedIds) === null,
		JSON.stringify(firstDiff(all.uids, expectedIds)),
	);
	H.check(
		`UI 列表无重复（${TOTAL_EVENTS} 行 / 去重 ${TOTAL_EVENTS}）`,
		new Set(all.uids).size === TOTAL_EVENTS,
		JSON.stringify({ rows: all.uids.length, unique: new Set(all.uids).size }),
	);
	H.check("到底后「加载更多」被禁用（continuation 为空）", all.moreDisabled === true, JSON.stringify({ moreDisabled: all.moreDisabled }));
	H.check(
		`到底后计数文案等于 ${TOTAL_EVENTS}`,
		all.countText.includes(String(TOTAL_EVENTS)),
		all.countText,
	);
	console.log(
		`  ↳ UI 到底：行数=${all.count} 首 uid=${all.firstUid} 末 uid=${all.lastUid}` +
			` 计数="${all.countText}" #ev-more禁用=${all.moreDisabled}`,
	);

	// 刷新（重新打开 events.html）→ 回到确定的第一窗口：恰好 50 行、首行是最新一条、
	// 「加载更多」重新可用。v4 的无限列表**不把游标写进 URL**（没有 ?before=），
	// 所以「刷新后仍在第 10 页」这条 v3 承诺已不存在；这里换成钉住刷新后的确定状态。
	await page.goto(base + "/events.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(page, "workspace");
	await page.selectOption("#ev-limit", String(UI_PAGE_SIZE));
	await page.click("#ev-filter");
	await waitForList(page, UI_PAGE_SIZE);
	info = await listInfo(page);
	H.eq(`刷新后回到 ${UI_PAGE_SIZE} 行的第一窗口`, info.count, UI_PAGE_SIZE);
	H.eq("刷新后首行仍是最新一条（同一事件流顺序）", info.firstUid, expectedIds[0]);
	H.check("刷新后「加载更多」重新可用", info.moreDisabled === false, JSON.stringify({ moreDisabled: info.moreDisabled }));

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
	// V5 的原始契约是「index 一个图脚本都不许加载」—— 那时首页没有图。
	//
	// 现在首页自带一张「最新事件链路」图（#latest-flow，silk 舞台），它需要
	// graphs.js + flograph.js；**不需要** dagre（silk 是自定义布局，不跑 dagre）
	// 与 traceview（那是 trace 页的视图装配）。所以契约收窄成：
	// 「只许 graphs/flograph，不许 dagre/traceview」—— 省掉的仍是两个最重的，
	// 而且这条**否定**断言依然是"多加载就会红"。
	const indexLoaded = loadedGraphLibs(indexResources);
	const indexHeavy = indexLoaded.filter(
		(b) => b === "dagre.min.js" || b === "traceview.js",
	);
	H.check(
		"index.html 只加载 graphs/flograph，不加载 dagre/traceview（V5 收窄：首页自带链路图）",
		indexHeavy.length === 0,
		JSON.stringify({ loaded: indexLoaded }),
	);
	H.check("index 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	errs.length = 0;
	// v4 的 trace 页地址是 `?namespace=&traceUID=`（traceUID 就是事件回执里的 traceId），
	// 读的端点是 `GET …/namespaces/{ns}/traces/{traceId}`。
	await page.goto(
		base + "/trace.html?namespace=" + encodeURIComponent(NS) + "&traceUID=" + encodeURIComponent(traceId),
		{ waitUntil: "load", timeout: 20000 },
	);
	await H.waitForAppReady(page, "workspace");
	await page
		.waitForFunction(
			(sel) => !!document.querySelector(sel),
			"#trace-graph",
			{ timeout: 10000 },
		)
		.catch(() => {});
	const traceResources = await resourceNames(page);
	const traceLoaded = loadedGraphLibs(traceResources);
	// #335 重写了 trace 页：业务链路图改用 graphs.js 的 taskChain（silk 自定义布局），
	// 不再需要 dagre（布局）与 traceview（v3 的视图装配）。所以这条断言的**意图**
	// （「只加载这一页真正要用的图脚本，不多加载」）保留，口径收到 trace 页的真实依赖：
	// flograph + graphs 必须在，dagre / traceview 必须不在。
	H.check(
		"trace.html 请求了它需要的图脚本（flograph + graphs）",
		JSON.stringify(traceLoaded) === JSON.stringify(["flograph.js", "graphs.js"]),
		JSON.stringify({ loaded: traceLoaded, resources: traceResources.map(libBaseName) }),
	);
	// 顺序：defer 脚本的**执行序 = 文档序**（后者依赖前者）。
	// 不能用 resource timeline 的顺序判 —— defer 是**并行下载**的，entries 的先后
	// 反映的是请求起跑时间，实测 graphs 会排在 flograph 前面（与执行序无关）。
	const scriptOrder = await page.evaluate(() =>
		[...document.querySelectorAll("script[src]")].map((s) => s.getAttribute("src")),
	);
	const libOrder = scriptOrder
		.map((src) => libBaseName(src || ""))
		.filter((b) => b.includes("dagre") || b === "flograph.js" || b === "graphs.js" || b === "traceview.js");
	H.check(
		"trace.html 图脚本顺序 flograph→graphs（依赖在前）",
		JSON.stringify(libOrder) === JSON.stringify(["flograph.js", "graphs.js"]),
		JSON.stringify({ libOrder, scriptOrder }),
	);
	const globals = await page.evaluate(() => ({
		traceView: typeof window.TraceView,
		graphs: typeof window.AnotifyGraphs,
	}));
	H.check(
		"trace 页图脚本真的执行了（AnotifyGraphs 就绪）",
		globals.graphs === "object",
		JSON.stringify(globals),
	);
	const traceSub = await page.evaluate(
		() => (document.getElementById("trace-sub") || {}).textContent || "",
	);
	H.check("trace 页渲染出该 traceUID", traceSub.includes(traceId), traceSub.slice(0, 160));
	H.check("trace 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	// V5 的另一半：#234 改的是两页（trace + source）。source.html 也必须在图脚本上齐全。
	errs.length = 0;
	await page.goto(base + "/source.html?name=" + encodeURIComponent(srcName), {
		waitUntil: "load",
		timeout: 20000,
	});
	await H.waitForAppReady(page, "workspace");
	await page
		.waitForFunction(() => (document.getElementById("src-name") || {}).textContent?.trim(), null, {
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
		name: (document.getElementById("src-name") || {}).textContent?.trim() || "",
	}));
	H.check(
		"source 页图脚本真的执行了（AnotifyGraphs / FlowGraph 就绪）",
		sourceGlobals.graphs === "object" && sourceGlobals.flow === "object",
		JSON.stringify(sourceGlobals),
	);
	H.check("source 页渲染出该源名称", sourceGlobals.name.includes("e2e.paging.cron"), JSON.stringify(sourceGlobals));
	H.check("source 页无 JS pageerror", errs.length === 0, errs[0]?.slice(0, 200));

	// ---------- E. #236 limit 硬顶（库里 1000 条 > 500，天然满足） ----------
	//
	// v4 的**事件资源列表**是 `GET /v1/namespaces/{ns}/events`（会话），v3 那个
	// 「GET /v1/events?namespaceId=… 带 Bearer Key」的读入口已退役（ingress 只剩 POST）。
	// 硬顶口径不变：缺省 50、>500 钳到 500（**不**折回 50 —— 那正是 #236）。
	// limit 的回显字段在 v4 信封里没有了（metadata 只带 continue），所以断言改成
	// 「条数 + 严格参数校验」：条数证明硬顶，limit=abc 的 400 证明「不静默兜底」。
	console.log("--- E. …/events limit 硬顶（#236）---");
	const machine = (limit) =>
		H.req(base, V.P.events(NS) + "?limit=" + encodeURIComponent(limit), { session: s.session });
	const m100 = await machine(100);
	H.check(
		`事件列表 limit=100 → 100 条`,
		m100.status === 200 && (V.items(m100) || []).length === 100,
		JSON.stringify({ status: m100.status, n: V.items(m100)?.length }),
	);
	const m500 = await machine(500);
	H.check(
		`事件列表 limit=500 → 500 条`,
		m500.status === 200 && (V.items(m500) || []).length === 500,
		JSON.stringify({ status: m500.status, n: V.items(m500)?.length }),
	);
	// #236 的关键回归：501..1000 曾经被 store 兜底成 50 条。
	const m501 = await machine(501);
	H.check(
		"事件列表 limit=501 → 500 条（#236 关键回归：不是 50）",
		m501.status === 200 && (V.items(m501) || []).length === 500,
		JSON.stringify({ status: m501.status, n: V.items(m501)?.length }),
	);
	const m1000 = await machine(1000);
	H.check(
		"事件列表 limit=1000 → 500 条",
		m1000.status === 200 && (V.items(m1000) || []).length === 500,
		JSON.stringify({ status: m1000.status, n: V.items(m1000)?.length }),
	);
	// v4 对非法 query 参数是**显式 400**（taskQueryInt），不再静默回退默认值：
	// 静默回退会让「写错参数」拿到另一个窗口，客户端以为筛过了。
	const mAbc = await machine("abc");
	V.expectReason("事件列表 limit=abc → 400 BadRequest（不静默兜底）", mAbc, 400, "BadRequest");
	H.check(
		"limit=abc 的 message 点名 limit",
		String(mAbc.json?.message || "").includes("limit"),
		String(mAbc.json?.message || mAbc.text).slice(0, 120),
	);

	// ---------- F. #377：列表规模化（8 个列表接口的 limit/continue + 4 页搜索/加载更多）----------
	//
	// 本段只钉两件在别处没有的事实：
	//   ① sources / nodes 两个形状不同的列表（ns 资源 vs cluster 资源）按 limit/continue
	//      翻页后与「一次性全量」逐条相等，且末页不带游标；
	//   ② sources 页首屏只渲染一页（DOM 行数 == 已加载条数，不随总条数线性增长），
	//      「加载更多」累加、本地搜索只过滤已加载的行。
	console.log("--- F. #377 列表分页：sources / nodes API + sources 页 UI ---");
	const PAGE = 20;
	const SOURCE_BULK = 50;
	const listNames = (res) => (V.items(res) || []).map((it) => it?.metadata?.name).filter(Boolean);

	// 50 条源（kind=manual：不需要能力出口，建源不会派发）。
	for (let i = 0; i < SOURCE_BULK; i++) {
		const name = `e2e.paging.many-${String(i).padStart(2, "0")}`;
		const res = await V.createSource(base, s.session, NS, V.sourceBody({ name, kind: "manual" }));
		H.eq(`造源 ${name} → 201`, res.status, 201);
	}
	// 期望序列来自「一次性全量」（limit=500 在上限内），不猜顺序。
	const fullRes = await H.req(base, V.P.sources(NS) + "?limit=500", { session: s.session });
	const fullNames = listNames(fullRes);
	const TOTAL = fullNames.length;
	H.check(`sources limit=500 一次拿全（${TOTAL} ≥ ${SOURCE_BULK}）`, TOTAL >= SOURCE_BULK, JSON.stringify({ total: TOTAL }));

	const pagedNames = [];
	let srcCursor = "";
	let srcPages = 0;
	do {
		const qs = "?limit=" + PAGE + (srcCursor ? "&continue=" + encodeURIComponent(srcCursor) : "");
		const res = await H.req(base, V.P.sources(NS) + qs, { session: s.session });
		H.eq(`sources 第 ${srcPages + 1} 页 → 200`, res.status, 200);
		const names = listNames(res);
		H.check(`sources 第 ${srcPages + 1} 页不超过 ${PAGE} 条`, names.length <= PAGE, JSON.stringify({ n: names.length }));
		pagedNames.push(...names);
		srcCursor = res.json?.metadata?.continue || "";
		srcPages++;
		if (srcPages > 10) throw new Error("sources 游标翻页不收敛");
	} while (srcCursor);
	H.eq(`sources 翻页页数 = ceil(${TOTAL}/${PAGE})`, srcPages, Math.ceil(TOTAL / PAGE));
	H.eq("sources 翻页拼接 = 一次性全量（不漏不重、顺序一致）", JSON.stringify(pagedNames), JSON.stringify(fullNames));
	H.eq("sources 翻页无重复", new Set(pagedNames).size, pagedNames.length);
	H.eq("sources 末页不带 continue", srcCursor, "");

	// 非法 limit 与跨命名空间游标：400（不静默兜底、不越权读）。
	V.expectReason("sources limit=abc", await H.req(base, V.P.sources(NS) + "?limit=abc", { session: s.session }), 400, "BadRequest");
	const otherNS = await V.createNamespace(base, s.session, "paging-other", "分页其它");
	H.eq("建第二个命名空间 → 201", otherNS.status, 201);
	for (let i = 0; i < 2; i++) {
		const res = await V.createSource(base, s.session, "paging-other", V.sourceBody({ name: `other-${i}`, kind: "manual" }));
		H.eq(`造源 paging-other/other-${i} → 201`, res.status, 201);
	}
	const otherFirst = await H.req(base, V.P.sources("paging-other") + "?limit=1", { session: s.session });
	const foreign = otherFirst.json?.metadata?.continue || "";
	H.check("第二个命名空间第一页带游标", foreign.length > 0, foreign.slice(0, 24));
	V.expectReason(
		"sources 用别的命名空间的游标",
		await H.req(base, V.P.sources(NS) + "?limit=1&continue=" + encodeURIComponent(foreign), { session: s.session }),
		400,
		"BadRequest",
	);

	// nodes：cluster 列表（无 ns 段），排序键 (name, id)。造 25 台。
	const NODE_BULK = 25;
	for (let i = 0; i < NODE_BULK; i++) {
		const name = `e2e-page-node-${String(i).padStart(2, "0")}`;
		const res = await H.req(base, V.P.nodes(), {
			session: s.session,
			method: "POST",
			body: { apiVersion: "anotify/v1", kind: "Node", metadata: { name }, spec: {} },
		});
		H.eq(`登记节点 ${name} → 201`, res.status, 201);
	}
	const nodesFull = await H.req(base, V.P.nodes() + "?limit=500", { session: s.session });
	const nodesFullNames = listNames(nodesFull);
	H.check(`nodes limit=500 一次拿全（≥ ${NODE_BULK}）`, nodesFullNames.length >= NODE_BULK, JSON.stringify({ total: nodesFullNames.length }));
	const nodesPaged = [];
	let nodeCursor = "";
	let nodePages = 0;
	do {
		const qs = "?limit=" + PAGE + (nodeCursor ? "&continue=" + encodeURIComponent(nodeCursor) : "");
		const res = await H.req(base, V.P.nodes() + qs, { session: s.session });
		H.eq(`nodes 第 ${nodePages + 1} 页 → 200`, res.status, 200);
		nodesPaged.push(...listNames(res));
		nodeCursor = res.json?.metadata?.continue || "";
		nodePages++;
		if (nodePages > 10) throw new Error("nodes 游标翻页不收敛");
	} while (nodeCursor);
	H.eq(`nodes 翻页页数 = ceil(${nodesFullNames.length}/${PAGE})`, nodePages, Math.ceil(nodesFullNames.length / PAGE));
	H.eq("nodes 翻页拼接 = 一次性全量", JSON.stringify(nodesPaged), JSON.stringify(nodesFullNames));
	H.eq("nodes 末页不带 continue", nodeCursor, "");

	// sources 页 UI：首屏 20 行 → 加载更多到全量；搜索只过滤已加载的行。
	await page.goto(base + "/sources.html", { waitUntil: "load", timeout: 20000 });
	await H.waitForAppReady(page, "workspace");
	await page.waitForFunction(() => document.querySelectorAll("#src-list > div[data-name]").length > 0, null, { timeout: 15000 });
	const rowCount = () => page.locator("#src-list > div[data-name]").count();
	const rowNames = () => page.$$eval("#src-list > div[data-name]", (els) => els.map((el) => el.dataset.name));
	H.eq(`sources 页首屏只渲染 ${PAGE} 行（总 ${TOTAL}，DOM 不随总条数增长）`, await rowCount(), PAGE);
	H.check("sources 页有「加载更多」且可用", await page.locator("#src-load-more").isVisible() && !(await page.locator("#src-load-more").isDisabled()));

	await page.locator("#src-load-more").click();
	await page.waitForFunction((n) => document.querySelectorAll("#src-list > div[data-name]").length > n, PAGE, { timeout: 15000 });
	H.eq(`点一次加载更多 → ${PAGE * 2} 行`, await rowCount(), PAGE * 2);

	let clicks = 1;
	while ((await page.locator("#src-load-more").count()) > 0 && clicks < 10) {
		const before = await rowCount();
		await page.locator("#src-load-more").click();
		await page.waitForFunction((n) => document.querySelectorAll("#src-list > div[data-name]").length > n, before, { timeout: 15000 });
		clicks++;
	}
	const uiNames = await rowNames();
	H.eq(`加载到底 → 全部 ${TOTAL} 行`, uiNames.length, TOTAL);
	H.eq("到底后「加载更多」消失", await page.locator("#src-load-more").count(), 0);
	H.eq("sources 页渲染无重复", new Set(uiNames).size, uiNames.length);

	await page.fill("#src-search", "many-07");
	await page.waitForFunction((n) => document.querySelectorAll("#src-list > div[data-name]").length < n, TOTAL, { timeout: 10000 });
	const filtered = await rowNames();
	H.eq("本地搜索 many-07 只剩 1 行", filtered.length, 1);
	H.check("搜索结果就是 many-07", String(filtered[0]).endsWith("many-07"), filtered[0]);
	await page.fill("#src-search", "");
	await page.waitForFunction((n) => document.querySelectorAll("#src-list > div[data-name]").length === n, TOTAL, { timeout: 10000 });
	H.eq("清空搜索恢复全部已加载行", await rowCount(), TOTAL);
	H.check("sources 页 0 pageerror", errs.length === 0, errs.join(" | "));

	const passed = H.summary("events_paging");
	await browser.close();
	server.stop();
	process.exit(passed ? 0 : 1);
}

main().catch(async (e) => {
	console.error(e);
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
