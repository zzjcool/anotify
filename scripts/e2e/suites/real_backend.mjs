#!/usr/bin/env node
/* SUITE: real_backend — 真后端 + 真浏览器：挡住「只在真后端才暴露」的回归（#201）
 *
 * 为什么需要它（和 `frontend.mjs` 互补，不是把 mock 再跑一遍）：
 * mock（`web/mock.js`）有两个"宽容"特性 —— ① 命名空间参数缺失时回退默认值、
 * ② 不校验产物形状。于是两类 bug 在 mock 上永远是绿的：
 *
 *   - `/v1/playbooks` 没进 `partials.js` 的 `NS_PATHS` → 真后端 400 →「载入失败」（#196）
 *   - 「复制 YAML」给出的 node.yaml 与 `cmd/agent` 格式不符 → 贴进去必解析失败（#197）
 *
 * 两次都是靠人工 playwright 才发现的。本套件只做三件 mock 做不到的事：
 *
 *  A. 真后端页面健康：起真 server（临时库）+ 真会话 cookie，逐页打开
 *     index / playbooks / playbook?id= / nodes / capabilities / sources / events，
 *     每页断言
 *       ① 整页生命周期内 `/v1/*` 没有 4xx/5xx —— **没有任何白名单**。
 *          组织幽灵 UI 删掉后（#203）没有任何页面会请求 `/v1/orgs`，
 *          留一条死白名单只会让人误以为"真后端还没有 /v1/orgs"
 *       ② 没有 `pageerror`
 *       ③ 预期内容真的渲染出来了（数据是这套件自己用真 API 造的）
 *       ④ **每个命名空间资源请求都带 `namespaceId`** —— 这条直接把 `NS_PATHS`
 *          漏项锁死（漏项的症状就是 400 或"切了命名空间列表不变"，两者都不报错）
 *
 *  B. UI 产物能被真实消费者解析：`nodes.html` 走「注册节点」→ 从 DOM 取一次性令牌
 *     与生成的 node.yaml → 把 capabilities 段的示例解开成一条**真能力** → 写临时
 *     文件 → 真起 `cmd/agent`（不是 mock，也不是文本正则）→ 节点在 `GET /v1/nodes`
 *     变 `online` 且声明的 run 被中枢接受 → SIGTERM 后退出码 0、节点回到 offline。
 *     真实消费者解析不了 = 直接红（#197 那一类）。
 *
 *  C. 命名空间切换器真的能切（#203）：两个命名空间各建一个**不同名字**的能力，
 *     在真浏览器里点一下切过去 → 页面请求真的带上新 `namespaceId`、能力列表真的
 *     换成新命名空间的那条、整个过程（含 reload）没有一条 `/v1/*` 4xx/5xx。
 *     这条在 mock 上永远是绿的：mock 缺参时回退默认命名空间，切错了也看不出来。
 *
 * 纪律：
 *   - 不用 mock、不改 `frontend.mjs`；A 部分的数据全部走真 API + 真 WS 握手。
 *   - 不睡等时间：一律等条件（waitForFunction / 轮询到状态变化）。
 *   - 端口走 `H.SUITE_PORTS.real_backend`（段内独占，不与其它套件重叠）。
 */
import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { chromium } from "playwright-core";
import * as H from "../lib/harness.mjs";
import * as N from "../lib/nodes.mjs";

const SUITE = "real_backend";
// agent 二进制：run_all.sh 会预构建 .e2e-bin/agent 并导出 AGENT_BIN；
// 单独跑本套件时（没走 run_all.sh）现场 go build 一份，不依赖外部步骤。
const AGENT_BIN = process.env.AGENT_BIN || path.join(H.ROOT_DIR, ".e2e-bin", "agent");

// ---------- 造数据用的预案（3 能力 + 2 边 + 1 webhook 触发） ----------
// 形状与 internal/server/playbook_handlers_test.go 的 pbFixtureManifest 一致：
// 入口能力 = analyze-pr（不作为任何边的 to）→ install 会为它建 webhook 源。
const PB_SLUG = "pr-review";
const PB_NAME = "PR 审查链";
const ENTRY_CAP = "analyze-pr";
// install 建的源名是 `<slug>-<kind>-<idx>`（internal/store/playbook.go）。
const WEBHOOK_SOURCE = PB_SLUG + "-webhook-0";
const INSTALLED_CAPS = ["analyze-pr@1", "review-pr@1", "notify-pr@1"];
const INSTALLED_CAP_NAMES = ["analyze-pr", "review-pr", "notify-pr"];
const MANIFEST = {
	capabilities: [
		{ name: "analyze-pr", version: 1, outputs: ["analyzed"] },
		{ name: "review-pr", version: 1, outputs: ["approved"] },
		{ name: "notify-pr", version: 1 },
	],
	edges: [
		{ from: "analyze-pr", fromPort: "analyzed", to: "review-pr" },
		{ from: "review-pr", fromPort: "approved", to: "notify-pr" },
	],
	triggers: [{ kind: "webhook", types: ["pull_request.opened"] }],
};

// A 部分那台节点（真 WS 握手；lib/nodes.mjs 播种 + hello/welcome）与
// B 部分从 UI 注册出来、由真 cmd/agent 连上的那台节点，名字分开，断言不串。
const WS_NODE_NAME = "rb-ws-node";
const UI_NODE_NAME = "ui-agent";

// ---------- 反例白名单：没有 ----------
// 这里**曾经**有一条 `/v1/orgs` 的 404 白名单（侧栏组织切换器探测该端点，真后端
// 没有这套路由）。#203 把组织幽灵 UI 删掉之后，没有任何页面会请求 `/v1/orgs`，
// 死条目留着只会让人误以为"真后端还没有 /v1/orgs"，所以整条摘掉、也不留等价物
// —— 现在**任何** 4xx/5xx 都算失败（要收紧，不要放松）。

// ---------- C 部分：命名空间切换（两个命名空间各一个不同名字的能力） ----------
//
// 两个命名空间里各建一个**名字不同**的能力，靠数据本身区分"到底切过去没有"：
// 只断言"列表渲染出来了"是不够的（切失败时旧列表还在，照样有内容）。
const NS2_SLUG = "rb-alt";
const NS2_NAME = "备用命名空间（切换验证）";
const CAP_MAIN = "rb-main-cap";
const CAP_ALT = "rb-alt-cap";

// 命名空间资源端点的判定直接复用 `H.isNamespaceEndpoint`（各套件共用口径）。
//
// 这条口径本轮**补过一次**：`/v1/playbooks` 是 #193 起的命名空间资源，但
// harness 的 `NS_REQUIRED_PREFIXES` 一直漏着它 —— 后果是 `H.req()` 静默少带
// `namespaceId`，真后端一律 400（#196 的同类问题）。改动在 lib 层，所以它
// 影响所有套件（`make e2e` 全量验证过没波及其它 19 个）。

let server;
let browser;
let wsNode = null; // A 部分的真 WS 握手节点（套件结束要 close）
let agentBin = ""; // 真 cmd/agent 二进制（main 里提前备好，B 部分直接用）
let agentProc = null;
let tmpDir = null;

// ---------- 小工具 ----------
function sleep(ms) {
	return new Promise((r) => setTimeout(r, ms));
}

// maskToken 把节点令牌从"要进 CI 日志"的文本里抹掉。
//
// 产品侧的纪律是"令牌绝不进日志"（中枢只存 sha256，见 console_handlers.go 的
// CreateNode 注释），e2e 的失败详情同样不该破例：临时库跑完即删，但 CI 日志会
// 留存/转发。失败诊断只需要知道"令牌存在且被 agent 读到了"，不需要它的值。
function maskToken(text) {
	return String(text == null ? "" : text)
		.replace(/^(\s*token:\s*)\S+/gm, "$1<masked>")
		.replace(/ant_node_[\w-]+/g, "ant_node_<masked>")
		.replace(/anotify_session=[\w.~-]+/g, "anotify_session=<masked>");
}

// pollUntil 轮询到 fn 返回真值。**不是**"睡固定时间"：等的是条件，
// intervalMs 只是轮询间隔；到点没成返回 null，由调用方断言。
async function pollUntil(fn, { timeoutMs = 10000, intervalMs = 100, stopWhen } = {}) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		const v = await fn();
		if (v) return v;
		if (stopWhen && stopWhen()) return null;
		if (Date.now() > deadline) return null;
		await sleep(intervalMs);
	}
}

// waitChildExit 等子进程退出（带超时；退出码/信号都回，不抛）。
function waitChildExit(child, timeoutMs = 15000) {
	if (child.exitCode !== null || child.signalCode !== null) {
		return Promise.resolve({ code: child.exitCode, signal: child.signalCode });
	}
	return new Promise((resolve) => {
		const timer = setTimeout(
			() => resolve({ code: null, signal: "TIMEOUT" }),
			timeoutMs,
		);
		child.once("exit", (code, signal) => {
			clearTimeout(timer);
			resolve({ code, signal });
		});
	});
}

// injectSession 把真会话 cookie 注入浏览器上下文（与 deeplink 套件同一写法）。
async function injectSession(ctx, sessionValue, base) {
	const u = new URL(base);
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

// ---------- 页面级请求观测 ----------
//
// 每个页面单独注册监听器（一页一个 context，互不串味）：
//   reqs   — 所有 /v1/* 请求（path / search / namespaceId）
//   resps  — 所有 /v1/* 响应（path / status）
//   fails  — 网络层失败的 /v1 请求（连不上、被 abort）
//   errs   — pageerror
//
// 为什么要收 fails：`page.on("response")` **不会**为失败请求触发。后端在中途挂掉
// 时，页面里的 /v1 请求全部 requestfailed → resps 为空 → "没有 4xx" 恒真。
// 那种页面必须红，所以失败请求要单独收。
function watchV1(page) {
	const v1 = { reqs: [], resps: [], fails: [], errs: [] };
	const parse = (u) => {
		try {
			return new URL(u);
		} catch {
			return null;
		}
	};
	page.on("request", (req) => {
		const u = parse(req.url());
		if (!u || !u.pathname.startsWith("/v1")) return;
		v1.reqs.push({
			method: req.method(),
			path: u.pathname,
			search: u.search,
			namespaceId: u.searchParams.get("namespaceId"),
		});
	});
	page.on("response", (res) => {
		const u = parse(res.url());
		if (!u || !u.pathname.startsWith("/v1")) return;
		v1.resps.push({
			method: res.request().method(),
			path: u.pathname,
			status: res.status(),
		});
	});
	page.on("requestfailed", (req) => {
		const u = parse(req.url());
		if (!u || !u.pathname.startsWith("/v1")) return;
		v1.fails.push({
			method: req.method(),
			path: u.pathname,
			error: (req.failure() || {}).errorText || "request failed",
		});
	});
	page.on("pageerror", (e) => v1.errs.push(String(e)));
	return v1;
}

// v1Settled 判断「已经发出的 /v1 请求是不是都落地了」（拿到响应，或明确失败）。
//
// 为什么需要：切命名空间靠 `location.reload()`，而 reload 会**中止**那些还在途的
// 请求 —— 它们变成 requestfailed。那不是产品 bug，是我们自己制造的噪音，
// 却正好会被「切换全过程没有 requestfailed」那条断言抓住（假红）。
// 所以起跳前先等到没有在途请求：等的是条件，不是固定时长。
function v1Settled(v1) {
	const tally = (list) => {
		const m = new Map();
		for (const r of list) {
			const k = r.method + " " + r.path;
			m.set(k, (m.get(k) || 0) + 1);
		}
		return m;
	};
	const want = tally(v1.reqs);
	const got = tally([...v1.resps, ...v1.fails]);
	for (const [k, n] of want) if ((got.get(k) || 0) < n) return false;
	return true;
}

const fmtList = (list, n = 6) => {
	const shown = list.slice(0, n).map((x) => JSON.stringify(x));
	return shown.length < list.length
		? shown.join(", ") + ` …(+${list.length - shown.length})`
		: shown.join(", ") || "—";
};

// checkPageHealth 是 A 部分每页共用的四条硬断言。
//
// 第③条是本套件的核心：真后端对命名空间资源端点**严格要求** namespaceId
// （缺 → 400，绝不回退默认值）。页面漏加的症状，在 mock 上是"什么都没发生"。
function checkPageHealth(label, v1, nsId) {
	// ① /v1/* 没有任何 4xx/**5xx**，也没有网络层失败的请求
	//
	// 5xx 一起算：500（DB 报错、命名空间解析 panic）同样意味着"页面这条链路是
	// 坏的"，放它过去等于给自己留个后门。
	//
	// 白名单是**空**的（#203 摘掉了最后一条 `/v1/orgs`）：组织幽灵 UI 删掉后没有
	// 任何页面会请求那个端点，留着只会让下一个人以为"真后端还没有 /v1/orgs"。
	const bad = v1.resps.filter((r) => r.status >= 400);
	const offenders = [...bad, ...v1.fails];
	H.check(
		`${label}：/v1/* 没有 4xx/5xx（没有任何白名单）`,
		// 非空前提：一个响应都没收到（页面根本没走真数据 / 请求全失败）不能算通过
		v1.resps.length > 0 && offenders.length === 0,
		v1.resps.length === 0
			? "一个 /v1 响应都没收到（请求全失败或页面没走真数据）"
			: `失败请求：${fmtList(offenders)}；全部 /v1 响应：${fmtList(v1.resps, 12)}`,
	);

	// ② 没有 pageerror
	H.check(
		`${label}：无 JS pageerror`,
		v1.errs.length === 0,
		v1.errs[0]?.slice(0, 200),
	);

	// ③ 每个命名空间资源请求都带 namespaceId
	const nsReqs = v1.reqs.filter((r) => H.isNamespaceEndpoint(r.path));
	const missing = nsReqs.filter((r) => !r.namespaceId);
	H.check(
		`${label}：${nsReqs.length} 个命名空间资源请求全部带 namespaceId`,
		nsReqs.length > 0 && missing.length === 0,
		nsReqs.length === 0
			? "一个命名空间资源请求都没发出来（页面没走真数据）"
			: `缺 namespaceId：${fmtList(missing)}`,
	);

	// ④ namespaceId 指向**默认**命名空间（不是别的、不是 `*`）
	// 同样带非空前提：空集合上"全都对"是废话（③ 一旦被改动，这条就成装饰）
	const wrong = nsReqs.filter((r) => r.namespaceId && r.namespaceId !== nsId);
	H.check(
		`${label}：namespaceId 指向默认命名空间`,
		nsReqs.length > 0 && wrong.length === 0,
		nsReqs.length === 0
			? "没有命名空间资源请求可判"
			: `实际：${fmtList(wrong)}；期望 ${nsId}`,
	);
}

// ---------- A 部分：逐页打开 + 断言 ----------
//
// 每页一个全新 context（localStorage 空的"全新浏览器"）：#196 正是死在
// "首次打开、scope 里还没有 namespaceId"这条路径上，复用 context 会把它藏起来。
const PAGES = [
	{
		name: "index.html",
		url: () => "/index.html",
		// 事件流小卡片是 init() 的最后一件事：它有内容 = 首页数据链路跑完了
		ready: () => {
			const box = document.getElementById("index-events");
			return !!box && box.children.length > 0;
		},
		expect: async (page, txt) => {
			H.check(
				"首页：没有演示模式残留（#demo-badge 已移除，#273）",
				await page.evaluate(() => !document.getElementById("demo-badge")),
				txt.slice(0, 200),
			);
			H.check(
				"首页：渲染的是真实数据（不是「载入失败」错误态）",
				await page.evaluate(() => {
					const t = document.getElementById("notif-list")?.textContent || "";
					return !/载入失败|Failed to load|読み込みに失敗|Error al cargar/.test(t);
				}),
				txt.slice(0, 200),
			);
			// 内容断言用**结构**而不是文案：页面文案随 4 种语言构建变化（zh/en/ja/es），
			// 拿中文/英文正则去锁，只会在别的语言构建下变成误红或恒真。
			const eventBox = await page.evaluate(() => {
				const b = document.getElementById("index-events");
				return b ? { children: b.children.length, text: b.innerText || "" } : null;
			});
			H.check(
				"首页：事件流区块有内容（真后端返回后渲染过）",
				!!eventBox && eventBox.children > 0 && eventBox.text.trim().length > 0,
				JSON.stringify(eventBox),
			);
		},
	},
	{
		name: "playbooks.html",
		url: () => "/playbooks.html",
		ready: () => {
			const g = document.getElementById("pb-grid");
			return !!g && g.children.length > 0;
		},
		expect: async (page, txt) => {
			H.check("预案列表页：含预案名", txt.includes(PB_NAME), txt.slice(0, 300));
			H.check("预案列表页：含预案 slug", txt.includes(PB_SLUG), txt.slice(0, 300));
			H.check(
				"预案列表页：渲染出 1 张预案卡片",
				(await page.evaluate(
					() => document.querySelectorAll("#pb-grid .pb-card").length,
				)) === 1,
				txt.slice(0, 200),
			);
			// #196 的直接症状：载入失败时把 #pb-empty 显示出来（并写上「载入失败」）。
			// 断言"空态块仍然藏着"而不是搜文本 —— 文本里搜中文会被内联 JS 干扰。
			const emptyState = await page.evaluate(() => {
				const e = document.getElementById("pb-empty");
				return e
					? { hidden: e.classList.contains("hidden"), text: e.textContent || "" }
					: null;
			});
			H.check(
				"预案列表页：没有落到「载入失败」（#196 的直接症状）",
				!!emptyState && emptyState.hidden === true,
				JSON.stringify(emptyState),
			);
		},
	},
	{
		name: "playbook.html?id=<预案>",
		url: (ctx) => "/playbook.html?id=" + encodeURIComponent(ctx.pbId),
		ready: () => {
			const box = document.getElementById("cap-list");
			return !!box && box.children.length > 0;
		},
		expect: async (page, txt) => {
			H.check("预案详情页：含预案名", txt.includes(PB_NAME), txt.slice(0, 300));
			for (const cap of INSTALLED_CAPS) {
				H.check(
					`预案详情页：items 里的能力 ${cap} 可见`,
					txt.includes(cap),
					txt.slice(0, 400),
				);
			}
			// 详情页载入失败时，pb-summary 会被写成「载入失败，请刷新重试。」
			H.check(
				"预案详情页：没有落到「载入失败」",
				!/载入失败|Load failed/.test(txt),
				txt.slice(0, 300),
			);
		},
	},
	{
		name: "nodes.html",
		url: () => "/nodes.html",
		ready: () => {
			const box = document.getElementById("nodes-list");
			return !!box && box.children.length > 0;
		},
		expect: async (page, txt) => {
			H.check("节点页：含节点名", txt.includes(WS_NODE_NAME), txt.slice(0, 300));
			H.check(
				"节点页：这台节点显示为在线（状态点 st-online）",
				(await page.evaluate(
					() => document.querySelectorAll("#nodes-list .st-online").length,
				)) >= 1,
				txt.slice(0, 300),
			);
			H.check(
				"节点页：显示它声明的能力",
				txt.includes(ENTRY_CAP),
				txt.slice(0, 400),
			);
		},
	},
	{
		name: "capabilities.html",
		url: () => "/capabilities.html",
		ready: () => {
			const box = document.getElementById("caps-list");
			return !!box && box.children.length > 0;
		},
		expect: async (page, txt) => {
			for (const name of INSTALLED_CAP_NAMES) {
				H.check(
					`能力页：含 ${name}`,
					txt.includes(name),
					txt.slice(0, 400),
				);
			}
			const capRows = await page.evaluate(
				() => document.querySelectorAll("#caps-list .cap-row").length,
			);
			H.check(
				"能力页：渲染出 3 行能力（不是空列表）",
				capRows >= 3,
				`cap-row 行数=${capRows}`,
			);
		},
	},
	{
		name: "sources.html",
		url: () => "/sources.html",
		ready: () => {
			const box = document.getElementById("src-list");
			return !!box && box.children.length > 0;
		},
		expect: async (page, txt) => {
			H.check(
				"源页：含 install 建出来的 webhook 源",
				txt.includes(WEBHOOK_SOURCE),
				txt.slice(0, 400),
			);
		},
	},
	{
		name: "events.html",
		url: () => "/events.html",
		ready: () => {
			const sel = document.getElementById("ev-source");
			return !!sel && sel.options.length > 1;
		},
		expect: async (page, txt) => {
			const opts = await page.evaluate(() => {
				const sel = document.getElementById("ev-source");
				return sel ? Array.from(sel.options).map((o) => o.textContent) : [];
			});
			H.check(
				"事件页：源筛选下拉里有这个源",
				opts.some((o) => String(o).includes(WEBHOOK_SOURCE)),
				JSON.stringify(opts),
			);
			const nodeOpts = await page.evaluate(() => {
				const sel = document.getElementById("ev-node");
				return sel ? Array.from(sel.options).map((o) => o.textContent) : [];
			});
			H.check(
				"事件页：节点筛选下拉里有这台节点",
				nodeOpts.some((o) => String(o).includes(WS_NODE_NAME)),
				JSON.stringify(nodeOpts),
			);
			// 这套件不产生事件（不 fire 源），所以真后端下列表应该只有**一条**空态卡。
			// 同 index：断结构不断文案（文案随语言构建变）。
			const listShape = await page.evaluate(() => {
				const box = document.getElementById("ev-list");
				return box
					? {
							children: box.children.length,
							text: (box.innerText || "").trim().slice(0, 80),
						}
					: null;
			});
			H.check(
				"事件页：无事件时只渲染一条空态（不是空白、不是演示数据）",
				!!listShape && listShape.children === 1 && listShape.text.length > 0,
				JSON.stringify(listShape),
			);
		},
	},
];

async function runPageChecks(ctxData, nsId) {
	for (const spec of PAGES) {
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, ctxData.session, server.base);
		const page = await ctx.newPage();
		const v1 = watchV1(page);
		try {
			// 超时值是为了"真坏时快速红"而不是"等足够久"：正常渲染 <2s，
			// 7 个页面的失败路径串行累加不能把套件预算（240s）吃光。
			await page.goto(server.base + spec.url(ctxData), {
				waitUntil: "load",
				timeout: 15000,
			});
			await H.waitForAppReady(page, "workspace");
			await page
				.waitForFunction(spec.ready, null, { timeout: 10000 })
				.catch((e) =>
					H.bad(
						`${spec.name}：等待页面数据就绪`,
						"超时：" + String(e.message || e).slice(0, 160),
					),
				);
			// innerText（不是 textContent）：只含**渲染出来**的文字。
			// 页面里的内联 <script> 也是 body 的子节点，textContent 会把 JS 源码一起拼进来
			// （于是 `t("pb.load_failed", "载入失败…")` 这种 fallback 字面量会把内容断言假打红）。
			const txt = await page.evaluate(() => document.body.innerText || "");
			await spec.expect(page, txt);
			checkPageHealth(spec.name, v1, nsId);
		} finally {
			await page.close();
			await ctx.close();
		}
	}
}

// ---------- B 部分：UI 产物交给真实消费者（cmd/agent） ----------
//
// 关键设计：**不自己写 capabilities 段**。页面生成的那份 node.yaml 里，
// capabilities 下面是一条**注释示例**（用户照着改）。这里把这条示例原样
// "解开"成真 YAML，只把能力名换成命名空间里真有的 analyze-pr，其它一字不改
// —— 于是这段 YAML 就是"用户照抄页面示例"的真实产物。
// 如果页面把 run 写成标量 / 键名不是 capability / timeout_sec 拼错，
// 严格解码的 agent 立刻失败（#197 那类），套件必红。
//
// 两条实现纪律（都是踩过的坑）：
//   ① 注释前缀 `"  # "` 要换成 **两个空格**，不是删掉：页面里 `  # - capability`
//      想表达的就是 `  - capability`（缩进 2）。直接删 4 个字符会把整条列表项
//      顶到第 0 列，YAML 立刻变成另一个意思。
//   ② 不能"凡是不像 YAML 的行就丢"：块标量（`command: |`）的内容行整天不像 YAML
//      （`echo replace-me`）。所以要记着"当前是否在块标量里"，在里面的行原样保留；
//      只有块标量之外的纯说明文字（`# 一条能力声明长这样…`）才丢。
function uncommentCapabilitySample(yamlText, capName) {
	const lines = String(yamlText || "").split("\n");
	const out = [];
	let inCaps = false;
	let renamed = false;
	let blockIndent = -1; // >=0 = 正在块标量里，内容行的缩进必须大于它
	const indentOf = (s) => (s.match(/^ */) || [""])[0].length;
	const looksLikeYaml = (s) => /^\s*(-\s|[A-Za-z_][\w-]*:)/.test(s);

	for (const line of lines) {
		if (/^capabilities:\s*$/.test(line)) {
			inCaps = true;
			out.push(line);
			continue;
		}
		if (!inCaps) {
			out.push(line);
			continue;
		}
		// capabilities 块内：`  # xxx` → `  xxx`（见纪律①）
		const commented = line.startsWith("  # ") || line.trim() === "#";
		let body = commented ? "  " + (line.trim() === "#" ? "" : line.slice(4)) : line;

		// 块标量内容：缩进比 `key: |` 深，原样保留（哪怕它看着不像 YAML）
		if (blockIndent >= 0 && body.trim() !== "") {
			if (indentOf(body) > blockIndent) {
				out.push(body);
				continue;
			}
			blockIndent = -1;
		}
		if (body.trim() === "") {
			if (!commented) out.push(body);
			continue;
		}
		// 块标量之外的纯说明注释：解开会把文件弄坏，直接丢（它本来就是注释）
		if (commented && !looksLikeYaml(body)) continue;
		// **只换能力名**，键名与缩进一律不动：页面示例里写的就是 analyze-pr，
		// 这里换名字只是为了"名字变了也不脆"。
		// 顺手把键名/缩进改对是不可接受的 —— 那会把 #197 那类"产物键名/缩进写错"
		// 的 bug 一起改好，套件永远绿（上面 ③ 的断言就是为这件事先钉一层）。
		if (!renamed && /^\s*- capability: /.test(body)) {
			body = body.replace(/^(\s*- capability: )\S+/, `$1${capName}`);
			renamed = true;
		}
		// 进入块标量（`command: |` / `>`；带 chomping 修饰符也认）
		if (/^\s*[A-Za-z_][\w-]*:\s*[|>][+-]?\s*$/.test(body)) {
			blockIndent = indentOf(body);
		}
		out.push(body);
	}

	const text = out.join("\n").replace(/\n+$/, "\n");
	const items = text
		.split("\n")
		.filter((l) => /^\s*- capability:/.test(l)).length;
	return { text, items };
}

// AGENT_SOURCES 是"这个二进制代表哪份代码"的判据（判新旧用）。
const AGENT_SOURCES = [
	"cmd/agent/main.go",
	"cmd/agent/config.go",
	"cmd/agent/client.go",
	"cmd/agent/runner.go",
];

// ensureAgentBin 返回一个**和当前源码一致**的 agent 二进制。
//
// run_all.sh 每次都会重新构建并导出 AGENT_BIN，所以 `make e2e` 这条路上永远新鲜。
// 但单独跑本套件时 `.e2e-bin/agent` 可能是上一次留下的旧二进制 —— 改了 cmd/agent
// 还拿它去跑就是假绿。所以：不存在、或比任一 agent 源文件旧，就现场构建一份。
function ensureAgentBin() {
	if (existsSync(AGENT_BIN)) {
		const binMs = statSync(AGENT_BIN).mtimeMs;
		const stale = AGENT_SOURCES.some((rel) => {
			try {
				return statSync(path.join(H.ROOT_DIR, rel)).mtimeMs > binMs;
			} catch {
				return false;
			}
		});
		if (!stale) return AGENT_BIN;
		console.log(`  · .e2e-bin/agent 比源码旧，现场重建`);
	}
	const out = path.join(tmpDir, "agent");
	execFileSync("go", ["build", "-o", out, "./cmd/agent"], {
		cwd: H.ROOT_DIR,
		env: process.env,
		stdio: ["ignore", "pipe", "pipe"],
	});
	return out;
}

async function waitNodeStatus(session, name, want, opts = {}) {
	return pollUntil(
		async () => {
			const r = await H.req(server.base, "/v1/nodes", { session });
			if (r.status !== 200) return null;
			const n = (r.json?.nodes || []).find((x) => x.name === name);
			return n && n.status === want ? n : null;
		},
		opts,
	);
}

async function runUiArtifactChecks(ctxData, nsId) {
	console.log("--- B. UI 产物 → 真 cmd/agent ---");
	const ctx = await browser.newContext();
	await H.mockExternalCdn(ctx);
	await injectSession(ctx, ctxData.session, server.base);
	const page = await ctx.newPage();
	const v1 = watchV1(page);

	try {
		await page.goto(server.base + "/nodes.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");
		await page
			.waitForFunction(
				() => {
					const box = document.getElementById("nodes-list");
					return !!box && box.children.length > 0;
				},
				null,
				{ timeout: 15000 },
			)
			.catch(() => {});

		// ① 走真 UI：点「注册节点」→ 填名字 → 提交
		await page.click("#node-register-btn");
		await page.fill("#reg-name", UI_NODE_NAME);
		await page.click("#reg-submit");
		const shown = await page
			.waitForFunction(
				() => {
					const res = document.getElementById("reg-result");
					const tok = document.getElementById("reg-token");
					const yaml = document.getElementById("reg-yaml");
					if (!res || !tok || !yaml) return null;
					if (res.classList.contains("hidden")) return null;
					const token = tok.textContent.trim();
					return token.length > 8 ? { token, yaml: yaml.textContent } : null;
				},
				null,
				{ timeout: 15000 },
			)
			.then((h) => h.jsonValue())
			.catch(() => null);

		const token = (shown && shown.token) || "";
		const pageYaml = (shown && shown.yaml) || "";
		H.check(
			"注册节点：一次性令牌出现在 DOM 里",
			token.length > 8,
			`令牌长度=${token.length} 页面错误行=${await page
				.evaluate(
					() => (document.getElementById("reg-error") || {}).textContent || "",
				)
				.catch(() => "")}`,
		);
		H.check(
			"注册节点：生成了 node.yaml（不是空块）",
			pageYaml.trim().length > 0,
			pageYaml.slice(0, 120),
		);

		// ② 页面的产物逐项对齐 cmd/agent/config.go 的顶层字段
		const origin = await page.evaluate(() => location.origin);
		H.check(
			"node.yaml：name 行与提交的名字一致",
			new RegExp("^name: " + UI_NODE_NAME + "\\s*$", "m").test(pageYaml),
			pageYaml.slice(0, 300),
		);
		H.check(
			"node.yaml：server 行是当前 origin（用户从哪个地址打开就回连哪个中枢）",
			new RegExp("^server: " + origin.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "\\s*$", "m").test(
				pageYaml,
			),
			pageYaml.slice(0, 300),
		);
		H.check(
			"node.yaml：token 行就是页面上那份一次性令牌",
			new RegExp("^token: " + token + "\\s*$", "m").test(pageYaml),
			"令牌=" + (token ? token.slice(0, 6) + "…" : "(空)"),
		);
		H.check(
			"node.yaml：有 instances 行",
			/^instances: \d+\s*$/m.test(pageYaml),
			pageYaml.slice(0, 300),
		);

		// ③ 先钉"页面示例本身的形状"：键名必须叫 capability、缩进必须是 2。
		//
		// 这一步**不能**省给后面的解注释代劳：#197 的五处不符里就有「键名写成 name」
		// 和「列表项顶到第 0 列」—— 如果解注释时顺手把键名/缩进改对了，这两类
		// bug 就永远不会让套件红（假绿）。所以：形状先断言，解注释只换能力名。
		const sampleItem = (pageYaml.match(/^  # - (\S+): (\S+)\s*$/m) || [])[1] || "";
		H.check(
			"node.yaml 示例：capabilities 列表项的键名是 capability（不是 name）",
			sampleItem === "capability",
			`实际键名 ${JSON.stringify(sampleItem)}；示例块：\n${pageYaml.split("capabilities:")[1] || ""}`,
		);
		H.check(
			"node.yaml 示例：列表项缩进是 2 个空格（顶到第 0 列是 #197 的旧形状）",
			/^  # - capability: \S+$/m.test(pageYaml),
			`示例块：\n${pageYaml.split("capabilities:")[1] || ""}`,
		);

		// ④ 解开 capabilities 示例 → 真能力 → 写盘（对错由真 agent 判）
		const patched = uncommentCapabilitySample(pageYaml, ENTRY_CAP);
		H.check(
			"node.yaml：解开示例后得到一条真能力声明（缩进 2）",
			patched.items >= 1 && /^  - capability: \S+$/m.test(patched.text),
			`解出 ${patched.items} 条；解开后：\n${patched.text}`,
		);
		const yamlPath = path.join(tmpDir, "node.yaml");
		writeFileSync(yamlPath, patched.text);

		agentProc = spawn(agentBin, ["-f", yamlPath, "-grace", "2"], {
			stdio: ["ignore", "pipe", "pipe"],
		});
		let agentLog = "";
		agentProc.stdout.on("data", (d) => {
			agentLog += d;
		});
		agentProc.stderr.on("data", (d) => {
			agentLog += d;
		});

		// ④ 真 agent 连上来 → 节点 online
		//    stopWhen：agent 已经退出（配置解析失败等）就不可能再 online，别等到超时
		const online = await waitNodeStatus(ctxData.session, UI_NODE_NAME, "online", {
			timeoutMs: 20000,
			intervalMs: 100,
			stopWhen: () => agentProc.exitCode !== null,
		});
		if (!online) {
			// 失败时把 agent 的日志等回来再断言：否则 stderr 可能还在管道里，
			// 失败详情只剩"退出码 1"三个字，等于把排查成本转嫁给下一个人。
			// 这里的 sleep 只等管道 flush（150ms），不承担任何断言语义。
			const early = await waitChildExit(agentProc, 5000);
			await sleep(150);
			H.bad(
				"真 cmd/agent 用页面给的 YAML 起来，节点变 online",
				maskToken(
					`agent 退出码=${early.code} 信号=${early.signal}；agent 日志：\n${agentLog.slice(-1500)}\n喂进去的 node.yaml：\n${patched.text}`,
				),
			);
		} else {
			H.ok("真 cmd/agent 用页面给的 YAML 起来，节点变 online");
		}

		// ⑤ 中枢接受了它的 run 声明（manifest 真的解析成功，不是"连上就算"）
		if (online) {
			const caps = online.capabilities || [];
			const cap = caps.find((c) => c.name === ENTRY_CAP && c.version === 1);
			H.check(
				"节点声明的真能力被中枢接受（含 run 声明）",
				!!cap,
				JSON.stringify(caps),
			);
			H.check(
				"run 声明往返一致（runner=bash）",
				!!cap && cap.runSpec && cap.runSpec.runner === "bash",
				JSON.stringify(cap && cap.runSpec),
			);
			H.check(
				"节点的 instances 与 YAML 一致",
				online.instances === 1,
				`instances=${online.instances}`,
			);
		}

		// ⑥ SIGTERM → 优雅退出，退出码 0；⑦ 断开后节点回 offline
		//
		// 只在"agent 真的起来了"之后才断言这两步：agent 本来就崩了（配置解析失败）
		// 的情况下，再报"没在运行"/"退出码不是 0"只是三条同因噪音，
		// 会把根因（上面那条红）淹掉。
		if (online) {
			H.check(
				"agent 收到 SIGTERM 前仍在运行（不是早就崩了）",
				agentProc.exitCode === null,
				`exitCode=${agentProc.exitCode}`,
			);
			agentProc.kill("SIGTERM");
			const exited = await waitChildExit(agentProc, 15000);
			H.eq("agent SIGTERM 退出码 = 0", exited.code, 0);
			H.check(
				"agent 是以正常信号收尾（不是被 SIGKILL）",
				exited.signal === null || exited.signal === undefined,
				`signal=${exited.signal}`,
			);

			const offline = await waitNodeStatus(ctxData.session, UI_NODE_NAME, "offline", {
				timeoutMs: 15000,
				intervalMs: 100,
			});
			H.check(
				"agent 退出后节点回到 offline",
				!!offline,
				offline ? "" : "15s 内没变 offline",
			);
		}

		// 这一页的请求同样过一遍 A 部分的四条硬断言（注册节点 = 真 POST /v1/nodes）
		checkPageHealth("nodes.html（注册链路）", v1, nsId);
	} finally {
		if (agentProc && agentProc.exitCode === null && agentProc.signalCode === null) {
			agentProc.kill("SIGKILL");
		}
		await page.close();
		await ctx.close();
	}
}

// ---------- C 部分：真浏览器切命名空间 ----------
//
// issue #203 的直接验收：侧栏切换器必须**真的**把页面的数据范围换掉。
// mock 套件看不到这条 —— mock 在 namespaceId 缺失/切错时会回退到默认命名空间，
// 于是"切了没生效"在 mock 上是绿的。这里用真后端（缺参严格 400）+ 真浏览器，
// 并在断言层面钉三层：
//   ① 数据侧先证明两边**本来就不一样**（否则"页面变了"可能是恒真）；
//   ② 切换后页面的每一个命名空间资源请求都带新 id（漏写 scope 的直接症状）；
//   ③ 整个切换过程（含 reload）没有一条 `/v1/*` 4xx/5xx、没有 requestfailed、
//      没有 pageerror —— 白名单已经摘掉，这一条现在是实话。
async function runNamespaceSwitchChecks(ctxData, nsId) {
	console.log("--- C. 真浏览器切命名空间（能力归属 + 全程无 4xx） ---");

	// ① 默认命名空间里建 CAP_MAIN（namespaceId 放 body，与契约 §6.2 一致）
	const capMain = await H.req(server.base, "/v1/capabilities", {
		session: ctxData.session,
		method: "POST",
		body: { namespaceId: nsId, name: CAP_MAIN, outputs: ["ok"] },
	});
	H.check(
		`默认命名空间建出能力 ${CAP_MAIN}（200/201）`,
		capMain.status === 200 || capMain.status === 201,
		`${capMain.status} ${String(capMain.text).slice(0, 160)}`,
	);

	// ② 建第二个命名空间：id 由后端生成，绝不硬编码（命名空间 id 是建库时生成的）
	const createdNs = await H.req(server.base, "/v1/namespaces", {
		session: ctxData.session,
		method: "POST",
		body: { slug: NS2_SLUG, name: NS2_NAME },
	});
	H.eq("POST /v1/namespaces（第二个命名空间）→ 201", createdNs.status, 201);
	const ns2 = createdNs.json?.namespace?.id || "";
	H.check(
		"拿到第二个命名空间 id（下面所有切换断言都靠它）",
		!!ns2,
		JSON.stringify(createdNs.json).slice(0, 200),
	);
	if (!ns2) return; // 没 id 无从切换：上面的红已经说清了原因，别再刷噪音

	// ③ 第二个命名空间里建 CAP_ALT
	const capAlt = await H.req(server.base, "/v1/capabilities", {
		session: ctxData.session,
		method: "POST",
		body: { namespaceId: ns2, name: CAP_ALT, outputs: ["ok"] },
	});
	H.check(
		`第二个命名空间建出能力 ${CAP_ALT}（200/201）`,
		capAlt.status === 200 || capAlt.status === 201,
		`${capAlt.status} ${String(capAlt.text).slice(0, 160)}`,
	);

	// ④ API 侧基线：两边数据**本来就不同**（否则下面的切换断言是恒真的废话）
	const listMain = await H.req(server.base, "/v1/capabilities", {
		session: ctxData.session,
		namespaceId: nsId,
	});
	const namesMain = (listMain.json?.capabilities || []).map((c) => c.name);
	const listAlt = await H.req(server.base, "/v1/capabilities", {
		session: ctxData.session,
		namespaceId: ns2,
	});
	const namesAlt = (listAlt.json?.capabilities || []).map((c) => c.name);
	H.check(
		`API 基线：默认命名空间含 ${CAP_MAIN}、不含 ${CAP_ALT}`,
		namesMain.includes(CAP_MAIN) && !namesMain.includes(CAP_ALT),
		JSON.stringify(namesMain),
	);
	H.check(
		`API 基线：第二个命名空间含 ${CAP_ALT}、不含 ${CAP_MAIN}`,
		namesAlt.includes(CAP_ALT) && !namesAlt.includes(CAP_MAIN),
		JSON.stringify(namesAlt),
	);

	// ⑤ 真浏览器：全新 context（localStorage 空 → 走 ensureScope 的首次路径）
	const ctx = await browser.newContext();
	await H.mockExternalCdn(ctx);
	await injectSession(ctx, ctxData.session, server.base);
	const page = await ctx.newPage();
	const v1 = watchV1(page);

	// 页内读状态（切换器形状 + 能力列表文本）。
	// 每次 evaluate 都可能撞上 reload（执行上下文销毁）→ 由调用处 catch 后
	// 当成"还没准备好"交给 pollUntil 下一轮重试，不在这里睡时间。
	const readState = () =>
		page.evaluate(() => {
			const ns = document.querySelector(".scope-ns");
			const list = document.getElementById("caps-list");
			return {
				subs: Array.from(document.querySelectorAll(".scope-item-sub")).map(
					(e) => (e.textContent || "").trim(),
				),
				ids: Array.from(document.querySelectorAll(".scope-item")).map(
					(e) => e.getAttribute("data-ns-id") || "",
				),
				checkedIds: Array.from(
					document.querySelectorAll('.scope-item[aria-checked="true"]'),
				).map((e) => e.getAttribute("data-ns-id") || ""),
				groupTitles: Array.from(
					document.querySelectorAll(".scope-menu .scope-group span"),
				).map((e) => (e.textContent || "").trim()),
				avatars: document.querySelectorAll(".scope-menu .scope-avatar").length,
				nsText: ns ? (ns.textContent || "").trim() : "",
				capsText: list ? list.innerText || "" : "",
			};
		});

	// openMenuAndPick：点开触发器 → 点某条命名空间（点击即 saveScope + reload）
	//
	// 两次点击都可能因为「页面正在导航」而抛（reload 是点击的副作用），所以**不**
	// 在这里判失败 —— 交给后面「页面是不是真的切过去了」的轮询断言：
	// 那种红能一眼看出发生了什么；这里只把错误文案带出去当详情。
	const openMenuAndPick = async (id) => {
		let err = "";
		await page.click(".scope-trigger").catch((e) => {
			err = String(e.message || e).slice(0, 120);
		});
		await page
			.waitForFunction(
				() => {
					const m = document.querySelector(".scope-menu");
					return !!m && !m.classList.contains("hidden");
				},
				null,
				{ timeout: 5000 },
			)
			.catch(() => {});
		await page.click(`.scope-item[data-ns-id="${id}"]`).catch((e) => {
			err = err || String(e.message || e).slice(0, 120);
		});
		return err;
	};

	try {
		await page.goto(server.base + "/capabilities.html", {
			waitUntil: "load",
			timeout: 20000,
		});
		await H.waitForAppReady(page, "workspace");

		// 等切换器挂载（.scope-item 渲染）+ 能力列表有行（真数据真的回来了）
		const mounted = await page
			.waitForFunction(
				() => {
					const list = document.getElementById("caps-list");
					return (
						!!list &&
						list.children.length > 0 &&
						document.querySelectorAll(".scope-item").length > 0
					);
				},
				null,
				{ timeout: 15000 },
			)
			.then(() => true)
			.catch(() => false);
		H.check(
			"能力页：切换器挂载出来（.scope-item 与能力列表都已渲染）",
			mounted,
			"15s 内没等到 .scope-item 与 #caps-list 同时就绪",
		);
		// 挂载都没成，后面的点击/切换断言全是噪音（同因刷屏），直接收手：
		// 上面那条红已经把根本原因说清了。
		if (!mounted) return;

		// ⑥ 切换器列出了两个命名空间、且只有命名空间一个维度（组织维度已消失）
		const before = await readState().catch(() => null);
		H.check(
			`切换器列出 2 个命名空间（default + ${NS2_SLUG}）`,
			!!before &&
				before.subs.length === 2 &&
				before.subs.includes("default") &&
				before.subs.includes(NS2_SLUG),
			JSON.stringify(before && before.subs),
		);
		// e2e 靠 data-ns-id 定位条目（下面的点击就是），没它就无法按 id 切
		H.check(
			"每条命名空间条目都带 data-ns-id（切换靠它定位，不靠中文名）",
			!!before && before.ids.length === 2 && before.ids.includes(ns2),
			JSON.stringify(before && before.ids),
		);
		H.check(
			"切换器只有命名空间一个维度（1 个分节、0 个组织头像）",
			!!before && before.groupTitles.length === 1 && before.avatars === 0,
			JSON.stringify(
				before && { groups: before.groupTitles, avatars: before.avatars },
			),
		);
		H.check(
			"切换前当前项是默认命名空间（.scope-ns 含 default）",
			!!before && before.nsText.includes("default"),
			before && before.nsText,
		);

		// ⑦ 起跳：先等已发出的 /v1 请求都落地（见 v1Settled 的注释），
		//    再记住 reqs 分界 —— 分界之后就是"切换之后"的全部请求。
		await pollUntil(() => v1Settled(v1), { timeoutMs: 8000, intervalMs: 50 });
		const boundary = v1.reqs.length;
		const pickErr = await openMenuAndPick(ns2);

		// reload 落地 + 新命名空间的数据渲染出来（reload 期间 evaluate 抛错 → 下一轮）
		const after = await pollUntil(
			async () => {
				try {
					const st = await readState();
					if (!st.nsText.includes(NS2_SLUG)) return null;
					if (!st.capsText.includes(CAP_ALT)) return null;
					return st;
				} catch {
					return null; // 执行上下文已被销毁（导航中）
				}
			},
			{ timeoutMs: 15000, intervalMs: 100 },
		);
		H.check(
			`点一下切到 ${NS2_SLUG}：页面重新加载并渲染新命名空间的能力`,
			!!after,
			`超时 15s：.scope-ns 含 ${NS2_SLUG} 且 #caps-list 含 ${CAP_ALT} 没同时成立；点击报错：${pickErr || "无"}`,
		);

		// ⑧ 切换后 GET /v1/capabilities 返回的**是**新命名空间的：页面文本 + API 复核
		const capsText = after ? after.capsText : "";
		H.check(
			`切换后能力列表含 ${CAP_ALT}、不含 ${CAP_MAIN}（数据真的换了）`,
			capsText.includes(CAP_ALT) && !capsText.includes(CAP_MAIN),
			capsText.slice(0, 400),
		);
		H.check(
			"切换后当前项标记落在新命名空间上（aria-checked=true 只有它一条）",
			!!after && after.checkedIds.length === 1 && after.checkedIds[0] === ns2,
			JSON.stringify(after && after.checkedIds),
		);
		const recheck = await H.req(server.base, "/v1/capabilities", {
			session: ctxData.session,
			namespaceId: ns2,
		});
		const namesRecheck = (recheck.json?.capabilities || []).map((c) => c.name);
		H.check(
			`复核 API：${NS2_SLUG} 的能力名集合含 ${CAP_ALT}、不含 ${CAP_MAIN}`,
			namesRecheck.includes(CAP_ALT) && !namesRecheck.includes(CAP_MAIN),
			JSON.stringify(namesRecheck),
		);

		// 分界之后：每个命名空间资源请求都必须带**新** id（漏写 scope 的直接症状）
		const nsAfter = v1.reqs
			.slice(boundary)
			.filter((r) => H.isNamespaceEndpoint(r.path));
		const wrongNs = nsAfter.filter((r) => r.namespaceId !== ns2);
		H.check(
			`切换后 ${nsAfter.length} 个命名空间资源请求全部带新 namespaceId（${NS2_SLUG}）`,
			nsAfter.length > 0 && wrongNs.length === 0,
			nsAfter.length === 0
				? "切换后一条命名空间资源请求都没发出来（页面没重新取数）"
				: `不符：${fmtList(wrongNs)}`,
		);
		const capAfter = nsAfter.filter((r) => r.path === "/v1/capabilities");
		H.check(
			"切换后页面确实用新 namespaceId 重新取了能力（GET /v1/capabilities）",
			capAfter.some((r) => r.method === "GET" && r.namespaceId === ns2),
			capAfter.length === 0 ? "切换后没有能力请求" : fmtList(capAfter),
		);

		// 整个切换过程（含 reload）：没有 4xx/5xx、没有网络层失败、没有 pageerror
		const badResps = v1.resps.filter((r) => r.status >= 400);
		H.check(
			`切换全过程（含 reload）${v1.resps.length} 个 /v1 响应没有 4xx/5xx`,
			v1.resps.length > 0 && badResps.length === 0,
			badResps.length === 0
				? "一个 /v1 响应都没收到（请求全失败或页面没走真数据）"
				: `失败响应：${fmtList(badResps)}`,
		);
		H.check(
			"切换全过程没有 /v1 请求在网络层失败（requestfailed）",
			v1.fails.length === 0,
			fmtList(v1.fails),
		);
		H.check(
			"切换全过程没有 JS pageerror",
			v1.errs.length === 0,
			String(v1.errs[0] || "").slice(0, 200),
		);

		// 选择必须落盘（刷页面/下一个页面靠它；只改内存等于切了个寂寞）
		const stored = await page
			.evaluate(() => {
				try {
					return localStorage.getItem("anotify.scope") || "";
				} catch {
					return "";
				}
			})
			.catch(() => "");
		H.check(
			"切换后的选择落盘（localStorage.anotify.scope 含新命名空间 id）",
			stored.includes(ns2),
			stored.slice(0, 200),
		);

		// ⑨ 反向切回默认命名空间：切换器不是单向的。
		// 正向都没切过去时这条必然也是红的（同因刷屏）—— 收手，让根因留在前面那条。
		if (!after) return;
		await pollUntil(() => v1Settled(v1), { timeoutMs: 8000, intervalMs: 50 });
		const boundaryBack = v1.reqs.length;
		const pickErrBack = await openMenuAndPick(nsId);
		const back = await pollUntil(
			async () => {
				try {
					const st = await readState();
					if (!st.nsText.includes("default")) return null;
					if (!st.capsText.includes(CAP_MAIN)) return null;
					return st;
				} catch {
					return null;
				}
			},
			{ timeoutMs: 15000, intervalMs: 100 },
		);
		H.check(
			`反向切回默认命名空间：页面重新出现 ${CAP_MAIN} 且不再有 ${CAP_ALT}`,
			!!back && !back.capsText.includes(CAP_ALT),
			back
				? back.capsText.slice(0, 400)
				: `超时 15s：.scope-ns 含 default 且 #caps-list 含 ${CAP_MAIN} 没同时成立；点击报错：${pickErrBack || "无"}`,
		);
		const nsBack = v1.reqs
			.slice(boundaryBack)
			.filter((r) => H.isNamespaceEndpoint(r.path));
		const wrongBack = nsBack.filter((r) => r.namespaceId !== nsId);
		H.check(
			`反向切换后 ${nsBack.length} 个命名空间资源请求全部指回默认命名空间`,
			nsBack.length > 0 && wrongBack.length === 0,
			nsBack.length === 0
				? "反向切换后一条命名空间资源请求都没有"
				: `不符：${fmtList(wrongBack)}`,
		);
	} finally {
		await page.close();
		await ctx.close();
	}
}

// ---------- 主流程 ----------
async function main() {
	H.startTimer();
	console.log(
		"=== SUITE: real_backend（真后端页面健康 + UI 产物交给真 cmd/agent）===",
	);
	server = await H.startServer({ suiteName: SUITE, rpId: "localhost" });
	tmpDir = mkdtempSync(path.join(tmpdir(), "anotify-e2e-real-backend-"));
	const s = H.seed(server.dbPath, "real_backend");
	H.check("建出真会话（devseed）", !!s.session, String(s.session || "").slice(0, 8));

	// ---------- 造数据：真 API 建预案并 install ----------
	console.log("--- 造数据（真 API，全部带 namespaceId）---");
	const nsId = await H.defaultNamespaceId(server.base, s.session);
	H.check("拿到默认命名空间 id", !!nsId, String(nsId));
	if (!nsId) throw new Error("无法解析默认命名空间 id（GET /v1/namespaces）");

	const created = await H.req(server.base, "/v1/playbooks", {
		session: s.session,
		method: "POST",
		body: {
			slug: PB_SLUG,
			name: PB_NAME,
			description: "real_backend 套件造的预案",
			manifest: MANIFEST,
		},
	});
	H.eq("POST /v1/playbooks → 201", created.status, 201);
	const pbId = created.json?.playbook?.id;
	// 详情页的 URL 参数（页面用 ?id= 定位预案）
	s.pbId = pbId || "";
	H.check(
		"预案详情带 items（创建即回详情形态）",
		Array.isArray(created.json?.playbook?.items),
		JSON.stringify(created.json)?.slice(0, 300),
	);

	const installed = await H.req(
		server.base,
		"/v1/playbooks/" + encodeURIComponent(pbId) + "/install",
		{ session: s.session, method: "POST" },
	);
	H.eq("POST install → 200", installed.status, 200);
	const made = installed.json?.created || {};
	H.eq("install 建出 3 个能力定义", (made.capabilities || []).length, 3);
	H.eq("install 建出 2 条边", (made.edges || []).length, 2);
	H.check(
		"install 建出 1 个 webhook 源",
		(made.sources || []).includes(WEBHOOK_SOURCE),
		JSON.stringify(made.sources),
	);

	// 真 WS 节点：让能力有节点声明、节点页有在线节点（A 部分的"预期内容"靠它）
	const capsResp = await H.req(server.base, "/v1/capabilities", {
		session: s.session,
	});
	const allCaps = (capsResp.json?.capabilities || []).map((c) => ({
		id: c.id,
		name: c.name,
		version: c.version,
	}));
	// 命名空间建出来时自带一个内置起点能力，所以这里只断言"install 的 3 个都在"，
	// 不写死总数（写死总数等于把内置能力的数量钉进套件）。
	const capList = allCaps.filter((c) => INSTALLED_CAP_NAMES.includes(c.name));
	H.eq("GET /v1/capabilities 含 install 的 3 个定义", capList.length, 3);
	// #204 / 迁移 14：组织层删除（orgs / org_members 两张表都没了）。原来这里那句
	// 「拿到 orgId」（primaryOrgOf 按 uid 查 org_members）换成**命名空间口径**：
	// main 开头已经断言过「拿到默认命名空间 id」（nsId），节点就播进那个命名空间。
	// 不需要重复取一次，也不再查任何 org 表。

	// 先把 agent 二进制准备好：`ensureAgentBin()` 走现场 `go build` 时是**同步阻塞**
	// 的，压在心跳定时器期间会把 rb-ws-node 的心跳一起卡住（看门狗 20s 就关连接）。
	agentBin = ensureAgentBin();

	wsNode = await N.bringUpNode(
		server.base,
		server.dbPath,
		nsId,
		WS_NODE_NAME,
		capList,
	);
	H.check(
		"真 WS 节点已上线（hello → welcome）",
		wsNode.frames.some((f) => f.type === "welcome"),
		JSON.stringify(wsNode.frames).slice(0, 300),
	);
	// 节点**必须**持续发上行帧：服务端看门狗超过 2×心跳（2×10s＝20s）没有上行帧
	// 就直接关连接（internal/ws/node_handler.go watchdog）→ 节点掉成 offline。
	// lib/nodes.mjs 的裸 WS 客户端不会自己发心跳，而本套件在并行/变异场景下会跑
	// 30s 以上 —— 不补心跳的话，失败原因会变成"跑得慢"，而不是真的有问题。
	const heartbeat = setInterval(() => {
		try {
			wsNode.conn.send({
				type: "heartbeat",
				ts: Math.floor(Date.now() / 1000),
			});
		} catch {
			/* 连接已关：由断言负责报错，这里不抛 */
		}
	}, 5000);

	browser = await chromium.launch({ channel: "chrome", headless: true });

	// ---------- A. 逐页健康 ----------
	console.log("--- A. 真后端页面健康 ---");
	await runPageChecks(s, nsId);

	// ---------- B. UI 产物 → 真 agent ----------
	await runUiArtifactChecks(s, nsId);

	// ---------- C. 真浏览器切命名空间（#203） ----------
	await runNamespaceSwitchChecks(s, nsId);

	const passed = H.summary(SUITE);
	clearInterval(heartbeat);
	await browser.close();
	if (wsNode?.conn) wsNode.conn.close();
	server.stop();
	rmSync(tmpDir, { recursive: true, force: true });
	// 不用 process.exit()：它可能截断 stdout（失败详情/失败项清单正好在尾部，
	// 那些恰恰是排障要看的）。设 exitCode 让进程自然退出；再挂一个 unref 的兜底
	// 定时器，万一还有句柄没释放也不会挂死。
	process.exitCode = passed ? 0 : 1;
	setTimeout(() => process.exit(process.exitCode), 3000).unref();
}

main().catch(async (e) => {
	console.error(e);
	try {
		if (agentProc && agentProc.exitCode === null) agentProc.kill("SIGKILL");
		await browser?.close();
		if (wsNode?.conn) wsNode.conn.close();
		server?.stop();
		if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
	} catch {
		/* ignore */
	}
	process.exit(1);
});
