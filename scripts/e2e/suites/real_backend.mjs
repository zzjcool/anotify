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
 * 两次都是靠人工 playwright 才发现的。本套件只做两件 mock 做不到的事：
 *
 *  A. 真后端页面健康：起真 server（临时库）+ 真会话 cookie，逐页打开
 *     index / playbooks / playbook?id= / nodes / capabilities / sources / events，
 *     每页断言
 *       ① 整页生命周期内 `/v1/*` 没有 4xx（白名单**只有** `/v1/orgs` 的 404：
 *          侧栏组织切换器的能力探测；别的任何 4xx 都算失败）
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
 * 纪律：
 *   - 不用 mock、不改 `frontend.mjs`；A 部分的数据全部走真 API + 真 WS 握手。
 *   - 不睡等时间：一律等条件（waitForFunction / 轮询到状态变化）。
 *   - 端口走 `H.SUITE_PORTS.real_backend`（段内独占，不与其它套件重叠）。
 */
import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { DatabaseSync } from "node:sqlite";
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

// 真 agent 的那台节点（B 部分从 UI 注册出来的）名。
const UI_NODE_NAME = "ui-agent";

// ---------- 反例白名单 ----------
// **只有** `/v1/orgs` 的 404：`partials.js` 的侧栏组织切换器探测该端点，
// 真后端还没有这套端点（docs/design/namespaces.md），拿不到就隐藏切换器。
// 除它以外任何 4xx 都是"某个页面用错了端点/漏了参数"，一律算失败。
const FOUR_XX_WHITELIST = [{ path: "/v1/orgs", status: 404 }];

// 命名空间资源端点（请求必须带 namespaceId）。
//
// `H.isNamespaceEndpoint` 是各 HTTP 套件共用的口径，但它**不含** `/v1/playbooks`
// —— #193 新增的命名空间资源。本套件的价值就在页面级请求的**全量**体检，
// 所以在这里补齐；漏项的症状正是 #196（预案页永远"载入失败"）。
const EXTRA_NS_PATHS = ["/v1/playbooks"];
function isNamespaceScoped(pathname) {
	if (H.isNamespaceEndpoint(pathname)) return true;
	return EXTRA_NS_PATHS.some(
		(p) => pathname === p || pathname.startsWith(p + "/"),
	);
}

let server;
let browser;
let wsNode = null; // A 部分的真 WS 握手节点（套件结束要 close）
let agentProc = null;
let tmpDir = null;

// ---------- 小工具 ----------
function sleep(ms) {
	return new Promise((r) => setTimeout(r, ms));
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

// primaryOrgOf 按 uid 查主组织（devseed 只把 org 打到 stderr，H.seed 不返回它）。
function primaryOrgOf(dbPath, uid) {
	const db = new DatabaseSync(dbPath);
	try {
		const row = db
			.prepare(
				`SELECT org_id FROM org_members WHERE user_id = ? ORDER BY rowid LIMIT 1`,
			)
			.get(uid);
		return row ? row.org_id : null;
	} finally {
		db.close();
	}
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
//   reqs  — 所有 /v1/* 请求（path / search / namespaceId）
//   resps — 所有 /v1/* 响应（path / status）
//   errs  — pageerror
function watchV1(page) {
	const v1 = { reqs: [], resps: [], errs: [] };
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
	page.on("pageerror", (e) => v1.errs.push(String(e)));
	return v1;
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
	// ① /v1/* 无未白名单的 4xx
	const fourXx = v1.resps.filter((r) => r.status >= 400 && r.status < 500);
	const offenders = fourXx.filter(
		(r) =>
			!FOUR_XX_WHITELIST.some((w) => w.path === r.path && w.status === r.status),
	);
	H.check(
		`${label}：/v1/* 没有未白名单的 4xx（白名单只有 /v1/orgs 的 404）`,
		offenders.length === 0,
		`未白名单 4xx：${fmtList(offenders)}；全部 /v1 响应：${fmtList(v1.resps, 12)}`,
	);

	// ② 没有 pageerror
	H.check(
		`${label}：无 JS pageerror`,
		v1.errs.length === 0,
		v1.errs[0]?.slice(0, 200),
	);

	// ③ 每个命名空间资源请求都带 namespaceId
	const nsReqs = v1.reqs.filter((r) => isNamespaceScoped(r.path));
	const missing = nsReqs.filter((r) => !r.namespaceId);
	H.check(
		`${label}：${nsReqs.length} 个命名空间资源请求全部带 namespaceId`,
		nsReqs.length > 0 && missing.length === 0,
		nsReqs.length === 0
			? "一个命名空间资源请求都没发出来（页面没走真数据）"
			: `缺 namespaceId：${fmtList(missing)}`,
	);

	// ④ namespaceId 指向**默认**命名空间（不是别的、不是 `*`）
	const wrong = nsReqs.filter((r) => r.namespaceId && r.namespaceId !== nsId);
	H.check(
		`${label}：namespaceId 指向默认命名空间`,
		wrong.length === 0,
		`实际：${fmtList(wrong)}；期望 ${nsId}`,
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
				"首页：没有退化成演示态（真后端活着，#demo-badge 隐藏）",
				await page.evaluate(
					() =>
						!!document.getElementById("demo-badge") &&
						document.getElementById("demo-badge").classList.contains("hidden"),
				),
				txt.slice(0, 200),
			);
			H.check(
				"首页：事件流是真后端的空态（还没有事件）",
				/还没有事件|No events yet/.test(txt),
				txt.slice(0, 200),
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
			H.check("节点页：含节点名", txt.includes(ctxWSNName()), txt.slice(0, 300));
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
				nodeOpts.some((o) => String(o).includes(ctxWSNName())),
				JSON.stringify(nodeOpts),
			);
			H.check(
				"事件页：没有事件时给出空态提示",
				txt.length > 0,
				txt.slice(0, 120),
			);
		},
	},
];

// A 部分的 WS 节点名（只在节点类断言里用，写成函数是为了让 PAGES 定义保持纯数据）
let WS_NODE_NAME = "rb-ws-node";
function ctxWSNName() {
	return WS_NODE_NAME;
}

async function runPageChecks(ctxData, nsId) {
	for (const spec of PAGES) {
		const ctx = await browser.newContext();
		await H.mockExternalCdn(ctx);
		await injectSession(ctx, ctxData.session, server.base);
		const page = await ctx.newPage();
		const v1 = watchV1(page);
		try {
			await page.goto(server.base + spec.url(ctxData), {
				waitUntil: "load",
				timeout: 20000,
			});
			await H.waitForAppReady(page, "workspace");
			await page
				.waitForFunction(spec.ready, null, { timeout: 15000 })
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
		// 列表项首键换成真能力名（页面示例里是 analyze-pr；这里不依赖它叫什么）
		if (!renamed && /^\s*- [A-Za-z_][\w-]*:/.test(body)) {
			body = body.replace(
				/^(\s*)- [A-Za-z_][\w-]*:.*$/,
				`$1- capability: ${capName}`,
			);
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

function ensureAgentBin() {
	if (existsSync(AGENT_BIN)) return AGENT_BIN;
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
	let yamlPath = "";

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

		// ③ 解开 capabilities 示例 → 真能力 → 写盘（DONE 之前由 agent 判对错）
		const patched = uncommentCapabilitySample(pageYaml, ENTRY_CAP);
		H.check(
			"node.yaml：capabilities 示例块能解出一条能力声明",
			patched.items >= 1,
			`解出 ${patched.items} 条；解开后：\n${patched.text}`,
		);
		yamlPath = path.join(tmpDir, "node.yaml");
		writeFileSync(yamlPath, patched.text);

		const agentBin = ensureAgentBin();
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
			const early = await waitChildExit(agentProc, 5000);
			await sleep(150);
			H.bad(
				"真 cmd/agent 用页面给的 YAML 起来，节点变 online",
				`agent 退出码=${early.code} 信号=${early.signal}；agent 日志：\n${agentLog.slice(-1500)}\n喂进去的 node.yaml：\n${patched.text}`,
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

		// ⑥ SIGTERM → 优雅退出，退出码 0
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

		// ⑦ 断开后节点回到 offline（服务端清理）
		const offline = await waitNodeStatus(ctxData.session, UI_NODE_NAME, "offline", {
			timeoutMs: 15000,
			intervalMs: 100,
		});
		H.check(
			"agent 退出后节点回到 offline",
			!!offline,
			offline ? "" : "15s 内没变 offline",
		);

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
	const orgId = primaryOrgOf(server.dbPath, s.uid);
	H.check("拿到 orgId", !!orgId, String(orgId));
	wsNode = await N.bringUpNode(
		server.base,
		server.dbPath,
		orgId,
		nsId,
		WS_NODE_NAME,
		capList,
	);
	H.check(
		"真 WS 节点已上线（hello → welcome）",
		wsNode.frames.some((f) => f.type === "welcome"),
		JSON.stringify(wsNode.frames).slice(0, 300),
	);

	browser = await chromium.launch({ channel: "chrome", headless: true });

	// ---------- A. 逐页健康 ----------
	console.log("--- A. 真后端页面健康 ---");
	await runPageChecks(s, nsId);

	// ---------- B. UI 产物 → 真 agent ----------
	await runUiArtifactChecks(s, nsId);

	const passed = H.summary(SUITE);
	await browser.close();
	if (wsNode?.conn) wsNode.conn.close();
	server.stop();
	rmSync(tmpDir, { recursive: true, force: true });
	process.exit(passed ? 0 : 1);
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
