#!/usr/bin/env node
/* C4 · 页面加载时序 + 大列表渲染（issue #217 / C 路）
 *
 * 量什么：
 *   1) 首屏时序（index.html / events.html / playbooks.html，各 3 次冷缓存 + 1 次热缓存）
 *      - navigation timing：ttfb(responseStart)/DCL/load/transferSize/encoded/decoded
 *      - resource timing：逐条 + 汇总（条数/字节/总时长）
 *      - longtask（PerformanceObserver，buffered）+ DOM 节点数 + JS 堆
 *   2) 1000 条事件后的 events.html：导航变化、渲染行数、首屏→列表可见耗时、longtask、内存
 *      - 页面默认不传 limit（服务端默认 100），所以另做 limit=500（API 硬上限）与
 *        1000 行（拼接响应，**合成**，见 syntheticRows）两档，用来量渲染随条数的增长曲线
 *   3) 归因：页面实际请求 /v1/console/events（无 limit）+ 渲染是 replaceChildren 全量重建
 *
 * 纪律（契约 §1）：
 *   - 整个进程树绑核：脚本先把自己 re-exec 到 `taskset -c $PC_CPUS`（默认 8-9），
 *     服务与 Chrome 都是子进程，天然继承同一批核。
 *   - 端口只用 7944（撞了按 7948 顺延），库在 $PC_ROOT/runs/s4/ 下，互不共用。
 *   - 产物全部落 $PC_ROOT/s4/：raw.json / runs.csv / table.md / server.log / chrome.log。
 *   - 不改产品代码；只走 HTTP + sqlite3 造数。
 *
 * 用法：
 *   export PC_CPUS=8-9
 *   node scripts/perf/c/s4-page.mjs            # 全流程（含造 1000 条）
 *   PC_SKIP_1K=1 node scripts/perf/c/s4-page.mjs
 */

import { spawn, execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync, existsSync, readFileSync } from "node:fs";
import path from "node:path";
import net from "node:net";
import { fileURLToPath, pathToFileURL } from "node:url";

const SELF = fileURLToPath(import.meta.url);
const REPO = path.resolve(path.dirname(SELF), "../../..");
const CPUS = process.env.PC_CPUS || "8-9";
const PC_ROOT = process.env.PC_ROOT || "/tmp/perf-c";
const OUT = path.join(PC_ROOT, "s4");
const RUN_DIR = path.join(PC_ROOT, "runs", "s4");
const BIN = path.join(PC_ROOT, "bin", "anotify");
const DEVSEED = path.join(PC_ROOT, "bin", "devseed");
const CANDIDATE_PORTS = [7944, 7948, 7949, 7950];
const PAGES = ["index.html", "events.html", "playbooks.html"];
const N_EVENTS = Number(process.env.PC_N_EVENTS || 1000);

// playwright-core 是 CJS 包，且本脚本可能在 git worktree 里跑（worktree 没有
// node_modules）。ESM 不认 NODE_PATH，所以从脚本目录往上找第一个装了的
// node_modules，再按绝对路径 import。找不到就明确报错，不要静默退回 headless-shell。
async function loadChromium() {
	const tried = [];
	let dir = REPO;
	for (;;) {
		const cand = path.join(dir, "node_modules", "playwright-core", "index.js");
		tried.push(cand);
		if (existsSync(cand)) {
			const pw = await import(pathToFileURL(cand).href);
			const chromium = pw.chromium || (pw.default && pw.default.chromium);
			if (!chromium) throw new Error(`playwright-core 里取不到 chromium：${cand}`);
			return { chromium, from: cand };
		}
		const up = path.dirname(dir);
		if (up === dir) break;
		dir = up;
	}
	throw new Error(`找不到 playwright-core（试过：\n${tried.join("\n")}）`);
}

// ---------- 分核：把自己 re-exec 到 taskset 下 ----------
// 只靠 spawn 时加 taskset 不够：Chrome 由 Playwright 内部 fork，我们没法逐层加。
// 整棵进程树继承父进程的 CPU affinity，所以最稳的是脚本自己先绑好。
if (!process.env.PC_REEXEC) {
	const child = spawn(
		"taskset",
		["-c", CPUS, process.execPath, SELF, ...process.argv.slice(2)],
		{ stdio: "inherit", env: { ...process.env, PC_REEXEC: "1", PC_CPUS: CPUS } },
	);
	child.on("exit", (code) => process.exit(code ?? 1));
} else {
	main().catch((e) => {
		console.error("s4-page 失败：", e && e.stack ? e.stack : e);
		process.exit(1);
	});
}

// ---------- 小工具 ----------
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function cpusAllowed() {
	try {
		const st = readFileSync("/proc/self/status", "utf8");
		return (st.match(/^Cpus_allowed_list:\s*(.*)$/m) || [])[1] || "?";
	} catch {
		return "?";
	}
}

function median(xs) {
	const a = xs.filter((x) => typeof x === "number" && Number.isFinite(x)).sort((x, y) => x - y);
	if (!a.length) return null;
	const m = Math.floor(a.length / 2);
	return a.length % 2 ? a[m] : Math.round(((a[m - 1] + a[m]) / 2) * 10) / 10;
}

async function httpJson(url, opts = {}) {
	const res = await fetch(url, { redirect: "manual", ...opts });
	const text = await res.text();
	let json = null;
	try {
		json = JSON.parse(text);
	} catch {
		/* 非 JSON */
	}
	return { status: res.status, json, text, headers: res.headers };
}

function portFree(port) {
	return new Promise((resolve) => {
		const s = net.createServer();
		s.unref();
		s.on("error", () => resolve(false));
		s.listen(port, "0.0.0.0", () => s.close(() => resolve(true)));
	});
}

// ---------- 构建（走 common.sh 的冻结口径） ----------
function ensureBuilt() {
	if (process.env.PC_FORCE_BUILD || !existsSync(BIN) || !existsSync(DEVSEED)) {
		console.error("[s4] 二进制缺失或 PC_FORCE_BUILD，走 common.sh pc_build");
		execFileSync(
			"bash",
			["-c", `set -e; cd '${REPO}'; source scripts/perf/c/common.sh; pc_require_env; pc_build`],
			{ stdio: "inherit", env: { ...process.env, PC_CPUS: CPUS, PC_ROOT } },
		);
	}
}

// ---------- 服务 ----------
async function startServer() {
	mkdirSync(RUN_DIR, { recursive: true });
	const dbPath = path.join(RUN_DIR, "perf.db");
	// 独立库：每次从空库开始（契约 §1「每个场景各自 pc_start 建的目录」）
	for (const f of [dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"]) {
		try {
			execFileSync("rm", ["-f", f]);
		} catch {
			/* ignore */
		}
	}
	const boot = `perf-c-s4-${Date.now()}`;
	for (const port of CANDIDATE_PORTS) {
		if (!(await portFree(port))) {
			console.error(`[s4] 端口 ${port} 已被占用，顺延`);
			continue;
		}
		const logPath = path.join(OUT, "server.log");
		const log = [];
		const proc = spawn("taskset", ["-c", CPUS, BIN], {
			env: {
				...process.env,
				ANOTIFY_ADDR: `:${port}`,
				ANOTIFY_DB: dbPath,
				ANOTIFY_PPROF: "1",
				ANOTIFY_LOG_LEVEL: "warn",
				ANOTIFY_RP_ID: "localhost",
				ANOTIFY_RP_ORIGIN: `http://localhost:${port}`,
				ANOTIFY_VAPID_PUBLIC_KEY: "dummy",
				ANOTIFY_VAPID_PRIVATE_KEY: "dummy",
				ANOTIFY_BOOT_ID: boot,
			},
			stdio: ["ignore", "pipe", "pipe"],
		});
		proc.stdout.on("data", (d) => log.push(String(d)));
		proc.stderr.on("data", (d) => log.push(String(d)));
		const base = `http://localhost:${port}`;
		const deadline = Date.now() + 15000;
		while (Date.now() < deadline) {
			if (proc.exitCode !== null) break;
			try {
				const r = await httpJson(base + "/health");
				// boot 身份校验：同一个端口上别人的服务也返回 200（#215 的教训）
				if (r.status === 200 && (r.json || {}).boot === boot) {
					writeFileSync(logPath, log.join(""));
					return { proc, base, port, dbPath, boot };
				}
			} catch {
				/* 还没起来 */
			}
			await sleep(150);
		}
		try {
			proc.kill("SIGKILL");
		} catch {
			/* ignore */
		}
		writeFileSync(path.join(OUT, `server-fail-${port}.log`), log.join(""));
		console.error(`[s4] 端口 ${port} 启动失败`);
	}
	throw new Error("服务启动失败（7944/7948/7949/7950 均不可用）");
}

function seed(dbPath) {
	const out = execFileSync(DEVSEED, ["-db", dbPath, "-username", "c4"], { encoding: "utf8" });
	const get = (k) => ((out.match(new RegExp(`^${k}=(.*)$`, "m")) || [])[1] || "").trim();
	return { session: get("SESSION"), sendKey: get("SEND_KEY"), uid: get("UID") };
}

// ---------- 造数（直插 sqlite，绕开 hook 的 60/min 限速，见契约 §6） ----------
function insertEvents(dbPath, ns, n) {
	if (!/^[A-Za-z0-9_]+$/.test(ns)) throw new Error(`命名空间 id 形状异常：${ns}`);
	const now = Math.floor(Date.now() / 1000);
	const rows = [];
	for (let i = 0; i < n; i++) {
		const idx = String(i).padStart(5, "0");
		const src = i % 3 === 0 ? "github" : i % 3 === 1 ? "cron" : "watch";
		const type = i % 3 === 0 ? "pull_request.opened" : i % 3 === 1 ? "schedule.tick" : "file.changed";
		const payload = JSON.stringify({ n: i, title: `perf event ${i}`, body: "x".repeat(40) });
		// source 不能是 'route'（store.ListEvents 在无 source 过滤时会排除它 → 列表里看不见）
		rows.push(
			`INSERT INTO events (id,namespace_id,trace_id,source,type,payload,created_at) VALUES ('evt_c4_${idx}','${ns}','trace_c4_${idx}','${src}','${type}','${payload}',${now - i});`,
		);
	}
	const sql = `PRAGMA busy_timeout=10000;\nBEGIN;\n${rows.join("\n")}\nCOMMIT;\n`;
	execFileSync("sqlite3", [dbPath], { input: sql, encoding: "utf8" });
	const cnt = execFileSync("sqlite3", [dbPath, "SELECT count(*) FROM events;"], { encoding: "utf8" }).trim();
	return Number(cnt);
}

// ---------- 浏览器侧测量 ----------
// 采样口径（写进 finding，避免数字不可比）：
//   * 每次测量一个**全新 context**（全新 HTTP 缓存、无 SW 残留）→ 记作 cold；
//     同一 context 里再 reload 一次 → 记作 warm（缓存命中）。
//     页面本身不注册 service worker（只有 receivers.html 注册），所以 SW 不参与。
//   * 外域请求（fonts.googleapis.com）一律 abort：本环境无外网，不 abort 会把
//     load 事件拖到超时，数字全是网络等待 —— 这是测量控制项，不是产品改进。
//   * 等 load 后再等 networkidle（≤5s）+ 300ms，让首屏 XHR 落地，
//     然后一次性读 navigation/resource/longtask/DOM。
async function measureOnce(browser, { label, url, warm = false, limitOverride = null, synthetic = null, session, base }) {
	const ctxOpts = { viewport: { width: 1280, height: 800 } };
	const ctx = await browser.newContext(ctxOpts);
	await ctx.addCookies([{ name: "anotify_session", value: session, url: base }]);
	await ctx.route(/^https?:\/\/(?!localhost[:/]|127\.0\.0\.1[:/])/, (r) => r.abort());
	await ctx.addInitScript(() => {
		// 长任务：buffered 拿不到「observer 注册前」的条目，所以同步注册 buffered:true。
		window.__lt = [];
		window.__ltErr = null;
		try {
			new PerformanceObserver((list) => {
				for (const e of list.getEntries()) {
					window.__lt.push({ start: Math.round(e.startTime), dur: Math.round(e.duration) });
				}
			}).observe({ type: "longtask", buffered: true });
		} catch (e) {
			window.__ltErr = String(e);
		}
		// 首屏 → 事件列表**数据渲染完成**的时刻。
		// 注意：#ev-list 里静态就有一个「加载中」占位卡，所以不能拿
		// childElementCount>0 当信号（那会在解析阶段就命中，量到的是占位符）。
		// 正确做法是把页面自己的 renderList 包一层：函数声明是全局绑定，
		// 页面内联脚本里的 `renderList()` 会解析到这个包装版。
		window.__renderAt = null;
		window.__renderStart = null;
		window.__renderDur = null;
		window.__renderedRows = null;
		(function wrap() {
			try {
				if (typeof window.renderList === "function" && !window.renderList.__wrapped) {
					const orig = window.renderList;
					const wrapped = function () {
						const t = performance.now();
						const ret = orig.apply(this, arguments);
						window.__renderStart = Math.round(t);
						window.__renderAt = Math.round(performance.now());
						window.__renderDur = Math.round(performance.now() - t);
						window.__renderedRows = document.querySelectorAll("#ev-list .ev-row").length;
						return ret;
					};
					wrapped.__wrapped = true;
					window.renderList = wrapped;
					return;
				}
			} catch (e) {
				/* 包不上就算了，字段留 null，不编数字 */
			}
			requestAnimationFrame(wrap);
		})();
	});
	if (limitOverride != null) {
		await ctx.route(/\/v1\/console\/events/, async (route) => {
			const u = new URL(route.request().url());
			u.searchParams.set("limit", String(limitOverride));
			const resp = await route.fetch({ url: u.toString() });
			await route.fulfill({ response: resp });
		});
	}
	if (synthetic) {
		await ctx.route(/\/v1\/console\/events/, (route) =>
			route.fulfill({
				status: 200,
				contentType: "application/json",
				body: JSON.stringify({ events: synthetic, namespaceId: synthetic.length ? "" : "" }),
			}),
		);
	}
	const page = await ctx.newPage();
	const pageErrors = [];
	page.on("pageerror", (e) => pageErrors.push(String(e.message || e)));
	// warm：同一 context 先访问一次再 reload，缓存命中
	if (warm) {
		await page.goto(url, { waitUntil: "load", timeout: 30000 }).catch(() => {});
	}
	let navErr = null;
	const t0 = Date.now();
	try {
		await page.goto(url, { waitUntil: "load", timeout: 30000 });
	} catch (e) {
		navErr = String(e.message || e);
	}
	const wallMs = Date.now() - t0;
	await page.waitForLoadState("networkidle", { timeout: 5000 }).catch(() => {});
	await sleep(300);
	const m = await page.evaluate(() => {
		const n = performance.getEntriesByType("navigation")[0];
		const pick = (o) => (o ? Math.round(o * 10) / 10 : null);
		const res = performance.getEntriesByType("resource").map((r) => ({
			name: r.name.replace(location.origin, ""),
			init: r.initiatorType,
			dur: Math.round(r.duration * 10) / 10,
			transfer: r.transferSize,
			enc: r.encodedBodySize,
			dec: r.decodedBodySize,
		}));
		const lt = (window.__lt || []).slice();
		return {
			nav: n
				? {
						type: n.type,
						startTime: Math.round(n.startTime),
						ttfb: pick(n.responseStart),
						requestStart: pick(n.requestStart),
						responseEnd: pick(n.responseEnd),
						domInteractive: pick(n.domInteractive),
						dcl: pick(n.domContentLoadedEventEnd),
						load: pick(n.loadEventEnd),
						transfer: n.transferSize,
						enc: n.encodedBodySize,
						dec: n.decodedBodySize,
					}
				: null,
			resources: res,
			longtasks: lt,
			longtaskErr: window.__ltErr,
			domNodes: document.querySelectorAll("*").length,
			evRows: document.querySelectorAll("#ev-list .ev-row").length,
			evSummary: (document.getElementById("ev-summary") || {}).textContent || "",
			yieldedAt: window.__renderAt,
			renderStart: window.__renderStart,
			renderDur: window.__renderDur,
			renderedRows: window.__renderedRows,
			heap: performance.memory
				? { used: performance.memory.usedJSHeapSize, total: performance.memory.totalJSHeapSize }
				: null,
			finalUrl: location.href,
			bodyNodes: document.body ? document.body.querySelectorAll("*").length : 0,
		};
	});
	await ctx.close();
	const sum = (k) => m.resources.reduce((a, r) => a + (typeof r[k] === "number" ? r[k] : 0), 0);
	return {
		label,
		url,
		warm,
		limitOverride,
		syntheticRows: synthetic ? synthetic.length : null,
		wallMs,
		navErr,
		pageErrors,
		nav: m.nav,
		resourceCount: m.resources.length,
		resourceTransfer: sum("transfer"),
		resourceEnc: sum("enc"),
		resourceDec: sum("dec"),
		resourceDurationSum: Math.round(sum("dur") * 10) / 10,
		resources: m.resources,
		longtaskCount: m.longtasks.length,
		longtaskSum: m.longtasks.reduce((a, x) => a + x.dur, 0),
		longtasks: m.longtasks,
		longtaskErr: m.longtaskErr,
		domNodes: m.domNodes,
		bodyNodes: m.bodyNodes,
		evRows: m.evRows,
		evSummary: m.evSummary,
		listVisibleAt: m.yieldedAt,
		renderStart: m.renderStart,
		renderDur: m.renderDur,
		renderedRows: m.renderedRows,
		heap: m.heap,
		finalUrl: m.finalUrl,
	};
}

// 3 次冷缓存 + 1 次热缓存，返回中位数汇总
async function measure3(browser, ctxArgs, runs = 3) {
	const list = [];
	for (let i = 0; i < runs; i++) list.push(await measureOnce(browser, { ...ctxArgs, warm: false }));
	const warm = await measureOnce(browser, { ...ctxArgs, warm: true });
	return { cold: list, warm, med: medianOf(list) };
}

function medianOf(list) {
	const med = (f) => median(list.map(f));
	return {
		ttfb: med((x) => x.nav && x.nav.ttfb),
		dcl: med((x) => x.nav && x.nav.dcl),
		load: med((x) => x.nav && x.nav.load),
		transfer: med((x) => x.nav && x.nav.transfer),
		enc: med((x) => x.nav && x.nav.enc),
		dec: med((x) => x.nav && x.nav.dec),
		resourceCount: med((x) => x.resourceCount),
		resourceTransfer: med((x) => x.resourceTransfer),
		resourceEnc: med((x) => x.resourceEnc),
		resourceDurationSum: med((x) => x.resourceDurationSum),
		longtaskCount: med((x) => x.longtaskCount),
		longtaskSum: med((x) => x.longtaskSum),
		domNodes: med((x) => x.domNodes),
		evRows: med((x) => x.evRows),
		listVisibleAt: med((x) => x.listVisibleAt),
		renderDur: med((x) => x.renderDur),
		heapUsed: med((x) => x.heap && x.heap.used),
		wallMs: med((x) => x.wallMs),
	};
}

// ---------- 主流程 ----------
async function main() {
	mkdirSync(OUT, { recursive: true });
	console.error(`[s4] 亲和性 Cpus_allowed_list=${cpusAllowed()}（期望 ${CPUS}）`);
	ensureBuilt();
	const sv = await startServer();
	console.error(`[s4] 服务 ${sv.base} pid=${sv.proc.pid} db=${sv.dbPath}`);
	const stopServer = () => {
		try {
			sv.proc.kill("SIGTERM");
		} catch {
			/* ignore */
		}
	};
	process.on("exit", stopServer);
	process.on("SIGINT", () => {
		stopServer();
		process.exit(1);
	});

	const { session, sendKey } = seed(sv.dbPath);
	const nsRes = await httpJson(sv.base + "/v1/namespaces", {
		headers: { Cookie: `anotify_session=${session}` },
	});
	const NS = ((nsRes.json || {}).namespaces || [])[0]?.id;
	if (!NS) throw new Error(`取不到默认命名空间：${nsRes.status} ${nsRes.text.slice(0, 200)}`);

	const { chromium, from } = await loadChromium();
	console.error(`[s4] playwright-core @ ${from}`);
	const browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox", "--disable-dev-shm-usage", "--enable-precise-memory-info"],
	});
	console.error(`[s4] chrome ${browser.version()}`);

	const result = {
		meta: {
			at: new Date().toISOString(),
			commit: execFileSync("git", ["rev-parse", "--short", "HEAD"], { cwd: REPO, encoding: "utf8" }).trim(),
			bin: BIN,
			binMtime: execFileSync("stat", ["-c", "%y", BIN], { encoding: "utf8" }).trim(),
			playwrightCore: from,
			cpus: CPUS,
			cpusAllowedSelf: cpusAllowed(),
			base: sv.base,
			db: sv.dbPath,
			ns: NS,
			chrome: browser.version(),
			nEvents: N_EVENTS,
		},
		empty: {},
		loaded: {},
		api: {},
	};

	try {
		// ===== 阶段 1：空库的三个页 =====
		for (const p of PAGES) {
			const r = await measure3(browser, { label: `empty:${p}`, url: `${sv.base}/${p}`, session, base: sv.base });
			result.empty[p] = r;
			console.error(`[s4] empty ${p} med: ttfb=${r.med.ttfb} dcl=${r.med.dcl} load=${r.med.load} res=${r.med.resourceCount} lt=${r.med.longtaskCount} dom=${r.med.domNodes}`);
		}

		// 空库时页面实际打的接口（归因证据）
		result.api.emptyConsoleEvents = await httpJson(
			`${sv.base}/v1/console/events?namespaceId=${NS}`,
			{ headers: { Cookie: `anotify_session=${session}` } },
		).then((r) => ({ status: r.status, count: ((r.json || {}).events || []).length }));

		// ===== 阶段 2：造 N 条事件 =====
		if (!process.env.PC_SKIP_1K) {
			const total = insertEvents(sv.dbPath, NS, N_EVENTS);
			// 接口核对（不信自己的 SQL，走产品读路径确认）
			const [c100, cDef, c1000] = await Promise.all([
				httpJson(`${sv.base}/v1/console/events?namespaceId=${NS}&limit=100`, { headers: { Cookie: `anotify_session=${session}` } }),
				httpJson(`${sv.base}/v1/console/events?namespaceId=${NS}`, { headers: { Cookie: `anotify_session=${session}` } }),
				httpJson(`${sv.base}/v1/console/events?namespaceId=${NS}&limit=1000`, { headers: { Cookie: `anotify_session=${session}` } }),
			]);
			result.api.afterInsert = {
				dbTotal: total,
				limit100: ((c100.json || {}).events || []).length,
				noLimit: ((cDef.json || {}).events || []).length,
				limit1000: ((c1000.json || {}).events || []).length,
			};
			console.error(`[s4] 造数完成 db=${total} api(no limit)=${result.api.afterInsert.noLimit} api(limit=1000)=${result.api.afterInsert.limit1000}`);

			// ===== 阶段 3：1000 条后重测 events.html（页面原样，不传 limit）=====
			const rDef = await measure3(browser, { label: `1k:events.html`, url: `${sv.base}/events.html`, session, base: sv.base });
			result.loaded["events.html(default)"] = rDef;
			console.error(`[s4] 1k events(default limit) med: ttfb=${rDef.med.ttfb} dcl=${rDef.med.dcl} load=${rDef.med.load} rows=${rDef.med.evRows} dom=${rDef.med.domNodes} listVisible=${rDef.med.listVisibleAt} lt=${rDef.med.longtaskCount}`);

			// ===== 阶段 4：渲染随条数增长 —— limit=500（服务端硬上限）======
			const r500 = await measure3(browser, {
				label: `1k:events.html(limit=500)`,
				url: `${sv.base}/events.html`,
				session,
				base: sv.base,
				limitOverride: 500,
			});
			result.loaded["events.html(limit=500, API 上限)"] = r500;
			console.error(`[s4] limit=500 med: rows=${r500.med.evRows} dom=${r500.med.domNodes} listVisible=${r500.med.listVisibleAt} lt=${r500.med.longtaskCount}`);

			// ===== 阶段 5：1000 行（合成响应：接口按 500 封顶，拿不到真 1000 行）=====
			// 用真接口的 500 行按序复制成 1000 行，形状与后端 JSON 完全一致，
			// 只改 id/traceId 防重复；用来回答「如果一次渲染 1000 条要多久」。
			const real = await httpJson(`${sv.base}/v1/console/events?namespaceId=${NS}&limit=500`, {
				headers: { Cookie: `anotify_session=${session}` },
			});
			const base500 = ((real.json || {}).events || []);
			const synth = [];
			for (let i = 0; i < N_EVENTS; i++) {
				const e = base500[i % base500.length] || {};
				synth.push({ ...e, id: `evt_syn_${i}`, traceId: `trace_syn_${i}` });
			}
			const r1k = await measure3(browser, {
				label: `1k:events.html(limit=1000, 合成)`,
				url: `${sv.base}/events.html`,
				session,
				base: sv.base,
				synthetic: synth,
			});
			result.loaded["events.html(1000 行, 合成)"] = r1k;
			result.api.synthBase = base500.length;
			console.error(`[s4] 1000 行(合成) med: rows=${r1k.med.evRows} dom=${r1k.med.domNodes} listVisible=${r1k.med.listVisibleAt} lt=${r1k.med.longtaskCount}`);

			// 1000 条后的 index.html（跨页对照：别的页不受事件数影响？）
			const rIdx = await measure3(browser, { label: `1k:index.html`, url: `${sv.base}/index.html`, session, base: sv.base });
			result.loaded["index.html(1k)"] = rIdx;
			console.error(`[s4] 1k index med: ttfb=${rIdx.med.ttfb} dcl=${rIdx.med.dcl} load=${rIdx.med.load} dom=${rIdx.med.domNodes}`);
		}
	} finally {
		await browser.close().catch(() => {});
		stopServer();
		await sleep(500);
		try {
			sv.proc.kill("SIGKILL");
		} catch {
			/* attach 已退出 */
		}
	}

	// ---------- 落盘 ----------
	writeFileSync(path.join(OUT, "raw.json"), JSON.stringify(result, null, 2));
	writeFileSync(path.join(OUT, "runs.csv"), toCsv(result));
	writeFileSync(path.join(OUT, "table.md"), toMarkdown(result));
	console.log(toMarkdown(result));
	console.log(`\n[s4] 原始产物：${OUT}/raw.json ${OUT}/runs.csv ${OUT}/table.md ${OUT}/server.log`);
}

// ---------- 输出 ----------
function rowsOf(result) {
	const out = [];
	const push = (grp, label, r) => {
		for (const [i, x] of [...r.cold.map((c, i) => [`cold${i + 1}`, c]), ["warm", r.warm]]) {
			out.push({
				group: grp,
				page: x.url.replace(/^https?:\/\/[^/]+\//, ""),
				phase: label,
				run: i,
				ttfb: x.nav && x.nav.ttfb,
				dcl: x.nav && x.nav.dcl,
				load: x.nav && x.nav.load,
				resCount: x.resourceCount,
				resBytes: x.resourceTransfer,
				lt: x.longtaskCount,
				ltSum: x.longtaskSum,
				dom: x.domNodes,
				rows: x.evRows,
				listVisible: x.listVisibleAt,
				renderDur: x.renderDur,
				heapMB: x.heap ? Math.round((x.heap.used / 1048576) * 10) / 10 : null,
				navTransfer: x.nav && x.nav.transfer,
			});
		}
	};
	for (const [p, r] of Object.entries(result.empty)) push("empty", `empty:${p}`, r);
	for (const [p, r] of Object.entries(result.loaded)) push("loaded", p, r);
	return out;
}

function toCsv(result) {
	const rows = rowsOf(result);
	const head = ["group", "page", "phase", "run", "ttfb_ms", "dcl_ms", "load_ms", "res_count", "res_bytes", "longtask_count", "longtask_ms_sum", "dom_nodes", "event_rows", "list_visible_ms", "render_ms", "heap_used_mb", "nav_transfer_bytes"];
	const lines = [head.join(",")];
	for (const r of rows) {
		lines.push([r.group, r.page, r.phase, r.run, r.ttfb, r.dcl, r.load, r.resCount, r.resBytes, r.lt, r.ltSum, r.dom, r.rows, r.listVisible, r.renderDur, r.heapMB, r.navTransfer].join(","));
	}
	return lines.join("\n") + "\n";
}

function toMarkdown(result) {
	const rows = rowsOf(result);
	const L = [];
	L.push(`# C4 原始数据（${result.meta.commit} · ${result.meta.base} · cpus=${result.meta.cpus}）`);
	L.push("");
	L.push("| 页 / 阶段 | run | TTFB(ms) | DCL(ms) | load(ms) | 子资源数 | 子资源字节 | longtask 数 | DOM 节点数 | 事件行数 | 列表可见(ms) | 渲染(ms) | heap(MB) |");
	L.push("|---|---|---|---|---|---|---|---|---|---|---|---|---|");
	for (const r of rows) {
		L.push(`| ${r.phase} | ${r.run} | ${r.ttfb ?? "-"} | ${r.dcl ?? "-"} | ${r.load ?? "-"} | ${r.resCount} | ${r.resBytes} | ${r.lt} | ${r.dom} | ${r.rows} | ${r.listVisible ?? "-"} | ${r.renderDur ?? "-"} | ${r.heapMB ?? "-"} |`);
	}
	return L.join("\n") + "\n";
}
