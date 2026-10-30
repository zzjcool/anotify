/* Anotify E2E 测试底座：服务生命周期 / HTTP 客户端 / 断言 / 播种。
 * 每个 suite 通过 import * as H 使用，独立进程内天然隔离。
 *
 * 新增能力（v2 重构）:
 * - waitForAppReady(page, pageType, opts): 事件等待替代固定 sleep
 * - SUITE_PORTS + findFreePort: 确定性端口分配，避免并行冲突
 * - startTimer/stopTimer/writeResults: 结构化 JSON 结果输出
 * 断言器 API（ok/bad/check/eq/summary）签名不变，向后兼容。 */
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { DatabaseSync } from "node:sqlite";
import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	"../../..",
);
export const BIN =
	process.env.ANOTIFY_BIN || path.join(ROOT, ".e2e-bin/anotify");
export const DEVSEED_BIN =
	process.env.DEVSEED_BIN || path.join(ROOT, ".e2e-bin/devseed");
export const ROOT_DIR = ROOT;

let passCount = 0;
let failCount = 0;
const failures = [];
const assertions = [];
let timerStart = 0;

export function ok(name) {
	passCount++;
	assertions.push({ name, pass: true });
	console.log("  ✅ " + name);
}
export function bad(name, detail = "") {
	failCount++;
	failures.push(name + (detail ? ` — ${detail}` : ""));
	assertions.push({ name, pass: false, detail: detail || undefined });
	console.log("  ❌ " + name + (detail ? ` — ${detail}` : ""));
}
export function check(name, cond, detail) {
	cond ? ok(name) : bad(name, detail);
}
export function eq(name, actual, expected) {
	const a =
		typeof actual === "object" ? JSON.stringify(actual) : String(actual);
	const e =
		typeof expected === "object" ? JSON.stringify(expected) : String(expected);
	check(name, a === e, `期望 ${e} 实际 ${a}`);
}
export function summary(suiteName) {
	console.log(`\n[${suiteName}] ${passCount} 通过 / ${failCount} 失败`);
	if (failures.length) {
		console.log("失败项:");
		failures.forEach((f) => console.log("  - " + f));
	}
	// 自动写 JSON 结果（向后兼容：现有套件不调 writeResults 也能有结果文件）
	writeResults(suiteName, stopTimer());
	return failCount === 0;
}
export function resetCounts() {
	passCount = 0;
	failCount = 0;
	failures.length = 0;
	assertions.length = 0;
}

// ---------- 结构化结果（JSON） ----------
// startTimer/stopTimer 在套件入口/出口调用，writeResults 写 JSON 结果文件。
export function startTimer() {
	timerStart = Date.now();
}
export function stopTimer() {
	return timerStart ? Date.now() - timerStart : 0;
}
export function writeResults(suiteName, durationMs) {
	const dir = process.env.E2E_RESULTS_DIR ||
		path.join(ROOT, ".e2e-bin", "results");
	mkdirSync(dir, { recursive: true });
	const result = {
		suite: suiteName,
		passed: passCount,
		failed: failCount,
		failures: [...failures],
		durationMs,
		assertions: [...assertions],
	};
	writeFileSync(path.join(dir, `${suiteName}.json`), JSON.stringify(result, null, 2));
}

// ---------- 事件等待（替代固定 sleep） ----------
// waitForAppReady 按 pageType 等待 JS 挂载完成的稳定锚点。
// 超时后 console.warn 但不抛（降级，让后续断言自己判）。

// ---------- 外部 CDN mock（离线门禁保障） ----------
// 线上页面已改用构建期 web/tailwind.css，不再请求 Play CDN。
// 这里仍拦截外域，避免回归（有人把 cdn.tailwindcss.com 加回来）或 Google Fonts
// 在外网抖动时把 page.goto(load) 拖到超时：
//   - tailwind Play CDN → 本地 fixtures/tailwind-cdn.js（npm 拉取的官方
//     @tailwindcss/browser v4，类名兼容 v3 语法，无内联 config 依赖）
//   - 其余外域（fonts 等）→ 200 空响应（不阻塞 load，不影响断言）
// 固件更新：npm pack @tailwindcss/browser 后取 package/dist/index.global.js。
const _TW_FIXTURE = (() => {
	try {
		return readFileSync(
			path.join(ROOT_DIR, "scripts/e2e/fixtures/tailwind-cdn.js"),
			"utf8",
		);
	} catch {
		return ""; // 固件缺失时退化为空 mock（样式断言会失败，提示补固件）
	}
})();
export async function mockExternalCdn(ctx) {
	await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/, (route) => {
		if (route.request().url().includes("cdn.tailwindcss.com")) {
			return route.fulfill({
				status: 200,
				contentType: "text/javascript",
				body: _TW_FIXTURE,
			});
		}
		return route.fulfill({ status: 200, contentType: "text/plain", body: "" });
	});
}

// waitForUrl 等页面导航到指定 URL，**轮询最终状态**而不是监听单次导航事件。
//
// 为什么需要它（真实踩过，多个套件偶发失败）：
//
//   未登录页可能同时发出好几个请求（/v1/auth/me、/v1/tasks、/v1/stats），
//   每个 401 都会执行一次 location.href = "login.html?next=..."。
//   第二次赋值会 **中止** 前一次导航，于是 page.waitForURL() 抛出
//   net::ERR_ABORTED（"maybe frame was detached"）。调用方一旦写
//   .catch(() => {}) 然后立即读 page.url()，拿到的还是被打断前的旧地址，
//   断言就假失败 —— 而且频率随机器负载变化，看起来像是随机 flaky。
//
//   产品行为本身是对的（最终一定会停在 login.html），错的只是测试用
//   “等一次导航事件”去断言“最终落在哪”。所以这里改成轮询 page.url()，
//   中途的 abort 与我们无关。
//
export async function waitForUrl(page, pattern, timeout = 8000) {
	const re =
		pattern instanceof RegExp
			? pattern
			: new RegExp(
					"^" +
						String(pattern)
							.split("*") // 把 Playwright 风格的 **/x* 变成正则
							.map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
							.join(".*") +
						"$",
				);
	const deadline = Date.now() + timeout;
	let last = "";
	while (Date.now() < deadline) {
		try {
			last = page.url();
		} catch {
			/* 页面正在换文档时 url() 可能拿到过渡态，下一轮再看 */
		}
		if (re.test(last)) return last;
		await new Promise((r) => setTimeout(r, 100));
	}
	return last;
}

export async function waitForAppReady(page, pageType, opts = {}) {
	const timeout = 10000;
	try {
		if (pageType === "login") {
			await page.waitForSelector(
				"#lang-switcher-login button[aria-haspopup]",
				{ timeout },
			);
		} else {
			// workspace 或 data-page：先等布局挂载
			await page.waitForSelector("#sidebar", { timeout });
			// data-page 额外等数据锚点
			if (opts.dataAnchor) {
				await page.waitForSelector(opts.dataAnchor, { timeout });
			}
		}
	} catch (e) {
		console.warn(
			`  ⚠️ waitForAppReady(${pageType}) 超时: ${e.message}`,
		);
	}
}

// ---------- 命名空间（命名空间资源端点必须显式带 namespaceId） ----------
//
// 契约 §6.2：/v1/capabilities、/v1/nodes、/v1/edges、/v1/sources、/v1/events、
// /v1/tasks、/v1/traces/*、/v1/stats 的 namespaceId 是**必填**参数，缺了就 400
// （后端**绝不**回退默认命名空间——回退等于把人写错的地方藏起来）。
// 组织级端点（/v1/orgs、/v1/admin/*、/v1/auth/* 等）反过来不需要也不该带。
const NS_REQUIRED_PREFIXES = [
	"/v1/capabilities",
	"/v1/nodes",
	"/v1/edges",
	"/v1/sources",
	"/v1/events",
	// 控制台的事件流是 events 的另一个入口（同一张表、同一套作用域），
	// 前缀匹配不会覆盖到它，必须单列。
	"/v1/console/events",
	"/v1/tasks",
	"/v1/traces",
	"/v1/stats",
];

// isNamespaceEndpoint 判断路径是否属于「必填 namespaceId」的命名空间资源端点。
// 按整段前缀匹配：/v1/nodes 是命名空间资源，而 /v1/node/stream 是 WS 端点
// （命名空间由节点令牌决定，见契约 §2.1），两者不能混。
export function isNamespaceEndpoint(p) {
	const pathname = String(p || "").split("?")[0];
	return NS_REQUIRED_PREFIXES.some(
		(prefix) => pathname === prefix || pathname.startsWith(prefix + "/"),
	);
}

// 默认命名空间解析结果按 base 进程内缓存：一次 suite 只真正取一次。
// 只缓存**成功**结果——后端还没上线 /v1/namespaces 时下次请求要能重试。
const nsCache = new Map(); // base → 默认命名空间 id
const nsInflight = new Map(); // base → 解析中的 Promise（并发请求不重复打）
const nsSessionByBase = new Map(); // base → 最近一次见到的会话（给只有 API Key 的请求兜底）
const nsSessionByDb = new Map(); // dbPath → 会话（seed() 登记；服务重启后同一个库还能用同一会话解析）
const nsDbByBase = new Map(); // base → SQLite 路径（startServer 注册；无会话时按库查）
const nsWarned = new Set(); // base → 已经提示过「解析不到默认命名空间」

// defaultNamespaceId(base, session) → 本组织默认命名空间（slug=default）的 id。
//
// 用例里**不许**硬编码 ns_xxx：命名空间 id 是建库时生成的，抄进用例就等着换库全红。
// 需要第二个命名空间做隔离断言时用 POST /v1/namespaces 建，拿响应里的 id。
export async function defaultNamespaceId(base, session) {
	if (session) nsSessionByBase.set(base, session);
	const hit = nsCache.get(base);
	if (hit) return hit;
	let inflight = nsInflight.get(base);
	if (!inflight) {
		// 会话来源三选一：本次调用的 → 这个 base 见过的 → seed() 登记过的同一个库的。
		// 最后一条是为「只有 API Key 的请求」准备的：/v1/events 是机器入口，
		// 没有会话 Cookie，但它要的命名空间跟这个库的会话看到的是同一个。
		const sess =
			session ||
			nsSessionByBase.get(base) ||
			nsSessionByDb.get(nsDbByBase.get(base) || "");
		inflight = resolveDefaultNamespaceId(base, sess).finally(() =>
			nsInflight.delete(base),
		);
		nsInflight.set(base, inflight);
	}
	const id = await inflight;
	if (id) nsCache.set(base, id);
	return id;
}

// resolveDefaultNamespaceId 是 defaultNamespaceId 的实际解析，两条路：
//
//   ① GET /v1/namespaces（契约 §6.1）——唯一事实源，默认命名空间是 slug=default。
//   ② 兜底：/v1/events 是**机器入口**（Bearer API Key，没有会话 Cookie），拿不到
//      /v1/namespaces。这时按 startServer 注册的库路径查 namespaces 表。仍然只是
//      「查出来的 id」，不是硬编码；用例侧看不出差别。
async function resolveDefaultNamespaceId(base, session) {
	if (session) {
		try {
			const res = await fetch(base + "/v1/namespaces", {
				headers: { Cookie: "anotify_session=" + session },
				redirect: "manual",
			});
			if (res.ok) {
				const j = await res.json().catch(() => null);
				const list = j?.namespaces || (Array.isArray(j) ? j : []);
				const def = list.find((n) => n?.slug === "default") || list[0];
				if (def?.id) return def.id;
			}
		} catch {
			/* 端点还没上线 / 服务刚重启：落到下面的兜底 */
		}
	}
	const dbPath = nsDbByBase.get(base);
	if (dbPath) {
		try {
			const db = new DatabaseSync(dbPath);
			try {
				const row = db
					.prepare(
						`SELECT id FROM namespaces WHERE slug = 'default' ORDER BY created_at LIMIT 1`,
					)
					.get();
				if (row?.id) return row.id;
			} finally {
				db.close();
			}
		} catch {
			/* namespaces 表还不存在（后端未升级）：视为解析不到 */
		}
	}
	return null;
}

// withNamespaceQuery 给命名空间资源端点补上 namespaceId（契约 §6.2 的必填参数）。
//
// 为什么在**出口**统一注入，而不是逐个用例手写：套件里这类调用上百处，靠人记得
// 加参数必然漏，而漏掉的表现是「某天 400 了」，排查成本极高。
//
// 调用方显式表态时**不覆盖**：
//   1) 路径里已经带 namespaceId（含 namespaceId=*：跨命名空间汇总用）；
//   2) body 对象里自带 namespaceId（后端取值顺序 query → body，意图明确就别抢）；
//   3) opts.namespaceId === null（显式声明「就是不带」，用于断言缺参 400）。
// 要指向非默认命名空间：opts.namespaceId = "<ns_id>"（id 来自 POST /v1/namespaces）。
async function withNamespaceQuery(base, p, opts) {
	if (!isNamespaceEndpoint(p)) return p;
	if (opts.namespaceId === null) return p;
	if (/[?&]namespaceId=/.test(p)) return p;
	if (
		opts.body &&
		typeof opts.body === "object" &&
		Object.prototype.hasOwnProperty.call(opts.body, "namespaceId")
	) {
		return p;
	}
	const explicit =
		typeof opts.namespaceId === "string" && opts.namespaceId
			? opts.namespaceId
			: null;
	const id = explicit || (await defaultNamespaceId(base, opts.session));
	if (!id) {
		// 解析不到就保持原样（后端还没升级 /v1/namespaces 时不至于把套件提前打红），
		// 但要说一次，否则严格后端下只会看到一片 400 而不知道原因。
		if (!nsWarned.has(base)) {
			nsWarned.add(base);
			console.warn(
				`  ⚠️ ${base}：解析不到默认命名空间，命名空间资源请求将不带 namespaceId（后端严格后会 400）`,
			);
		}
		return p;
	}
	return (
		p + (p.includes("?") ? "&" : "?") + "namespaceId=" + encodeURIComponent(id)
	);
}

// ---------- HTTP 客户端 ----------
// req(base, path, {method, key, session, body, namespaceId}) → {status, json, text, headers}
// namespaceId 一般不用传：命名空间资源端点会自动补默认命名空间（见 withNamespaceQuery）。
export async function req(base, p, opts = {}) {
	const headers = Object.assign({}, opts.headers);
	if (opts.key) headers["Authorization"] = "Bearer " + opts.key;
	if (opts.session) headers["Cookie"] = "anotify_session=" + opts.session;
	let body;
	if (opts.body !== undefined) {
		if (typeof opts.body === "string") {
			body = opts.body;
		} else {
			body = JSON.stringify(opts.body);
			headers["Content-Type"] = headers["Content-Type"] || "application/json";
		}
	}
	// 契约 §6.2 的必填参数在这里统一注入（namespaceId 走 query，后端以 query 为准）。
	const target = await withNamespaceQuery(base, p, opts);
	const res = await fetch(base + target, {
		method: opts.method || (body ? "POST" : "GET"),
		headers,
		body,
		redirect: "manual",
	});
	const text = await res.text();
	let json = null;
	try {
		json = JSON.parse(text);
	} catch {
		/* not json */
	}
	const hdrs = {};
	res.headers.forEach((v, k) => {
		hdrs[k.toLowerCase()] = v;
	});
	return { status: res.status, json, text, headers: hdrs };
}

// ---------- 确定性端口分配 ----------
// 每套件固定 50 个端口空间，并行时不同套件永不撞端口。
// 冲突时在段内 +1 重试（最多 50 次）。
export const SUITE_PORTS = {
	api_contract: 5900,
	contract: 5850,
	auth_flow: 5950,
	ws_protocol: 6000,
	routing: 6050,
	persistence: 6100,
	security: 6150,
	edge_cases: 6200,
	frontend: 6250,
	deeplink: 6300,
	i18n: 6400,
	lang_hint: 6450,
	cli_auth: 6500,
	i18n_coverage: 6550,
	passkeys: 6600,
	passkey_enroll: 6650,
	admin_flow: 6700,
	reply_e2e: 6750,
	ux_regression_19_23: 6800,
	recovery_next: 6850,
};

// findFreePort(base): 用 net.listen 探测端口是否可用，冲突 +1 重试（最多 50 次）。
// 绑定 0.0.0.0 与 anotify server 的 ":port"（绑所有接口）一致，避免地址族不匹配误判。
// close 后加短延迟，确保 OS 释放端口给后续 server 进程。
export function findFreePort(base) {
	return new Promise((resolve, reject) => {
		let attempts = 0;
		const tryPort = (port) => {
			const server = net.createServer();
			server.unref();
			server.on("error", () => {
				attempts++;
				if (attempts >= 50) {
					reject(new Error(`无可用端口（段起始 ${base}，尝试 ${attempts} 次）`));
					return;
				}
				tryPort(port + 1);
			});
			server.listen(port, "0.0.0.0", () => {
				server.close(() => {
					// close 回调后端口可能仍在 TIME_WAIT，短暂延迟确保 OS 释放
					setTimeout(() => resolve(port), 50);
				});
			});
		};
		tryPort(base);
	});
}

// LIVE_SERVERS 记录本进程启动过的 server 子进程。
//
// 为什么需要：套件正常结束时自己会调 stop()，但**并行运行器超时用的是
// SIGKILL**——那时 stop() 永远执行不到，server 进程会一直活着占着端口和
// 内存。曾经因此累积了 138 个孤儿进程、1.9GB 常驻内存，把后续
// `make e2e` 逼到随机失败（表现是多个套件"0通过/0失败"，看起来像
// 代码问题，其实是资源被自己人吃光了）。
const LIVE_SERVERS = new Set();
let exitHookInstalled = false;
function installExitHook() {
	if (exitHookInstalled) return;
	exitHookInstalled = true;
	const killAll = () => {
		for (const proc of LIVE_SERVERS) {
			try { proc.kill("SIGKILL"); } catch { /* ignore */ }
		}
		LIVE_SERVERS.clear();
	};
	// exit 只能做同步操作，正好 kill 是同步的
	process.on("exit", killAll);
	process.on("SIGTERM", () => { killAll(); process.exit(1); });
	process.on("SIGINT", () => { killAll(); process.exit(1); });
}

// ---------- 服务生命周期 ----------
// startServer({port, suiteName, portOffset, rpId, extraEnv}) → {base, dbPath, stop()}
// port: 显式指定端口（向后兼容，优先级最高）
// suiteName: 套件名 → 从 SUITE_PORTS 查端口段起始
// portOffset: 段内偏移（同套件多次启动递增，默认 0）
//
// 端口分配策略：不用 findFreePort 预探测（listen-then-close 会在端口上留下
// TIME_WAIT，导致随后 spawn 的 anotify bind 失败）。改为直接 spawn anotify
// 尝试绑定，health 轮询失败（bind 冲突或启动慢）就段内 +1 重试，最多 5 次。
export async function startServer({
	port,
	suiteName,
	portOffset = 0,
	rpId,
	extraEnv = {},
} = {}) {
	let basePort;
	if (port) {
		basePort = port;
	} else if (suiteName && SUITE_PORTS[suiteName]) {
		basePort = SUITE_PORTS[suiteName] + portOffset;
	} else {
		basePort = 5900 + Math.floor(Math.random() * 400);
	}

	// 尝试启动（段内最多 5 次，避免 TIME_WAIT/端口冲突）
	let lastErr = "";
	for (let attempt = 0; attempt < 5; attempt++) {
		const p = basePort + attempt;
		const tmp = mkdtempSync(path.join(tmpdir(), "anotify-e2e-"));
		const dbPath = path.join(tmp, "test.db");
		const origin =
			rpId === "localhost" ? `http://localhost:${p}` : `https://${rpId}`;
		const env = Object.assign(
			{},
			process.env,
			{
				ANOTIFY_ADDR: `:${p}`,
				ANOTIFY_DB: dbPath,
				ANOTIFY_STATIC: "",
				ANOTIFY_RP_ID: rpId || "localhost",
				ANOTIFY_RP_ORIGIN: origin,
				ANOTIFY_VAPID_PUBLIC_KEY: process.env.ANOTIFY_VAPID_PUBLIC_KEY || "",
				ANOTIFY_VAPID_PRIVATE_KEY: process.env.ANOTIFY_VAPID_PRIVATE_KEY || "",
			},
			extraEnv,
		);
		installExitHook();
		const proc = spawn(BIN, [], { env, stdio: ["ignore", "pipe", "pipe"] });
		LIVE_SERVERS.add(proc);
		proc.on("exit", () => LIVE_SERVERS.delete(proc));
		let logBuf = "";
		proc.stdout.on("data", (d) => { logBuf += d; });
		proc.stderr.on("data", (d) => { logBuf += d; });

		const base = `http://localhost:${p}`;
		// 等待健康（最多 6s）
		const deadline = Date.now() + 6000;
		let up = false;
		while (Date.now() < deadline) {
			// 进程意外退出（bind 失败等）→ 立即换端口重试
			if (proc.exitCode !== null) { break; }
			try {
				const r = await fetch(base + "/health");
				if (r.ok) { up = true; break; }
			} catch {
				/* not up yet */
			}
			await new Promise((r) => setTimeout(r, 200));
		}
		if (up) {
			// 注册库路径：无会话的请求（/v1/events 走 Bearer API Key）靠它解析默认命名空间。
			// 同一个 base 重新拉起服务（重试/重启）时旧缓存必须作废：库换了，ns id 也换了。
			// 服务真正用的库：调用方可能用 extraEnv.ANOTIFY_DB 指回老库
			// （persistence 套件的"重启后数据还在"就是这条路）。
			// 记成上面那个新 mkdtemp 的话，兜底会去查一个服务从未打开过的空库，
			// 于是 namespaces 查不到 → 不带 namespaceId → 机器入口 400。
			nsDbByBase.set(base, extraEnv.ANOTIFY_DB || dbPath);
			nsCache.delete(base);
			nsSessionByBase.delete(base);
			return {
				base,
				port: p,
				dbPath,
				tmp,
				log: () => logBuf,
				stop: () => {
					try { proc.kill(); } catch { /* ignore */ }
					LIVE_SERVERS.delete(proc);
					// 等进程真的退出再返回。
					//
					// 为什么必须等：不等的话下一个套件用相邻端口启动时，
					// 可能碰上还没退出的旧进程 —— 表现为 ECONNREFUSED 或
					// bind 失败。这种"随机失败"极难查（曾经被误判成机器慢）。
					//
					// 用 process.kill(pid, 0) 探测而不是看 proc.exitCode：
					// stop() 是同步的，同步忙等会阻塞事件循环，exitCode
					// 永远不会更新。而 kill(pid,0) 是直接的系统调用，
					// 进程不在了就抛 ESRCH。
					const deadline = Date.now() + 3000;
					for (;;) {
						try {
							process.kill(proc.pid, 0);
						} catch {
							break; // 已退出
						}
						if (Date.now() > deadline) {
							try { proc.kill("SIGKILL"); } catch { /* ignore */ }
							break;
						}
						const until = Date.now() + 10;
						while (Date.now() < until) { /* 短忙等 10ms */ }
					}
					rmSync(tmp, { recursive: true, force: true });
				},
			};
		}
		// 健康/启动失败：kill 进程，清理，记日志，换端口重试
		lastErr = logBuf;
		try { proc.kill(); } catch { /* ignore */ }
		rmSync(tmp, { recursive: true, force: true });
	}
	throw new Error(`服务启动失败（试 ${5} 个端口均不可用）:\n${lastErr}`);
}

// ---------- 播种（devseed 后门，仅用于非 auth 套件快速建用户/Key/会话） ----------
export function seed(dbPath, username = "e2e") {
	const out = execFileSync(
		DEVSEED_BIN,
		["-db", dbPath, "-username", username],
		{ encoding: "utf8" },
	);
	const get = (k) => (out.match(new RegExp(`^${k}=(.*)$`, "m")) || [])[1];
	const session = get("SESSION");
	// 登记这个库的会话：#162 之后资源请求要带 namespaceId，而 /v1/events 这类
	// 机器入口只带 API Key（没有 Cookie）。留着会话，解析默认命名空间时就能走
	// 契约端点 GET /v1/namespaces，而不是退化成查库。
	if (session) nsSessionByDb.set(dbPath, session);
	return {
		uid: get("UID"),
		sendKey: get("SEND_KEY"),
		recvKey: get("RECV_KEY"),
		session,
	};
}

// ---------- 常用构造 ----------
export function makeDevice(over = {}) {
	return Object.assign(
		{
			name: "测试设备",
			platform: "mac",
			tags: [],
			endpoint:
				"https://push.example.com/" + Math.random().toString(36).slice(2),
			keys: { p256dh: "BPxTest", auth: "authTest" },
			userAgent: "e2e-test",
		},
		over,
	);
}
