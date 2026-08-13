/* Anotify E2E 测试底座：服务生命周期 / HTTP 客户端 / 断言 / 播种。
 * 每个 suite 通过 import * as H 使用，独立进程内天然隔离。
 *
 * 新增能力（v2 重构）:
 * - waitForAppReady(page, pageType, opts): 事件等待替代固定 sleep
 * - SUITE_PORTS + findFreePort: 确定性端口分配，避免并行冲突
 * - startTimer/stopTimer/writeResults: 结构化 JSON 结果输出
 * 断言器 API（ok/bad/check/eq/summary）签名不变，向后兼容。 */
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
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

// ---------- HTTP 客户端 ----------
// req(base, path, {method, key, session, body, raw}) → {status, json, text, headers}
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
	const res = await fetch(base + p, {
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
	auth_flow: 5950,
	ws_protocol: 6000,
	routing: 6050,
	persistence: 6100,
	security: 6150,
	edge_cases: 6200,
	frontend: 6250,
	deeplink: 6300,
	push_e2e: 6350,
	i18n: 6400,
	lang_hint: 6450,
	cli_auth: 6500,
	i18n_coverage: 6550,
	passkeys: 6600,
	passkey_enroll: 6650,
	admin_flow: 6700,
	reply_e2e: 6750,
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
		const proc = spawn(BIN, [], { env, stdio: ["ignore", "pipe", "pipe"] });
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
			return {
				base,
				port: p,
				dbPath,
				tmp,
				log: () => logBuf,
				stop: () => {
					try { proc.kill(); } catch { /* ignore */ }
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
	return {
		uid: get("UID"),
		sendKey: get("SEND_KEY"),
		recvKey: get("RECV_KEY"),
		session: get("SESSION"),
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
