/* Anotify E2E 测试底座：服务生命周期 / HTTP 客户端 / 断言 / 播种。
 * 每个 suite 通过 createHarness 获得独立的服务实例与断言收集器。 */
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
export const BIN = process.env.ANOTIFY_BIN || path.join(ROOT, ".e2e-bin/anotify");
export const DEVSEED_BIN = process.env.DEVSEED_BIN || path.join(ROOT, ".e2e-bin/devseed");
export const ROOT_DIR = ROOT;

let passCount = 0;
let failCount = 0;
const failures = [];

export function ok(name) { passCount++; console.log("  ✅ " + name); }
export function bad(name, detail = "") { failCount++; failures.push(name + (detail ? ` — ${detail}` : "")); console.log("  ❌ " + name + (detail ? ` — ${detail}` : "")); }
export function check(name, cond, detail) { cond ? ok(name) : bad(name, detail); }
export function eq(name, actual, expected) {
	const a = typeof actual === "object" ? JSON.stringify(actual) : String(actual);
	const e = typeof expected === "object" ? JSON.stringify(expected) : String(expected);
	check(name, a === e, `期望 ${e} 实际 ${a}`);
}
export function summary(suiteName) {
	console.log(`\n[${suiteName}] ${passCount} 通过 / ${failCount} 失败`);
	if (failures.length) { console.log("失败项:"); failures.forEach((f) => console.log("  - " + f)); }
	return failCount === 0;
}
export function resetCounts() { passCount = 0; failCount = 0; failures.length = 0; }

// ---------- HTTP 客户端 ----------
// req(base, path, {method, key, session, body, raw}) → {status, json, text, headers}
export async function req(base, p, opts = {}) {
	const headers = Object.assign({}, opts.headers);
	if (opts.key) headers["Authorization"] = "Bearer " + opts.key;
	if (opts.session) headers["Cookie"] = "anotify_session=" + opts.session;
	let body;
	if (opts.body !== undefined) {
		if (typeof opts.body === "string") { body = opts.body; }
		else { body = JSON.stringify(opts.body); headers["Content-Type"] = headers["Content-Type"] || "application/json"; }
	}
	const res = await fetch(base + p, { method: opts.method || (body ? "POST" : "GET"), headers, body, redirect: "manual" });
	const text = await res.text();
	let json = null;
	try { json = JSON.parse(text); } catch { /* not json */ }
	const hdrs = {};
	res.headers.forEach((v, k) => { hdrs[k.toLowerCase()] = v; });
	return { status: res.status, json, text, headers: hdrs };
}

// ---------- 服务生命周期 ----------
// startServer({port, rpId, vapid}) → {base, dbPath, stop()}
export async function startServer({ port, rpId, extraEnv = {} } = {}) {
	const p = port || 5900 + Math.floor(Math.random() * 400);
	const tmp = mkdtempSync(path.join(tmpdir(), "anotify-e2e-"));
	const dbPath = path.join(tmp, "test.db");
	const origin = rpId === "localhost" ? `http://localhost:${p}` : `https://${rpId}`;
	const env = Object.assign({}, process.env, {
		ANOTIFY_ADDR: `:${p}`,
		ANOTIFY_DB: dbPath,
		ANOTIFY_STATIC: "",
		ANOTIFY_RP_ID: rpId || "localhost",
		ANOTIFY_RP_ORIGIN: origin,
		ANOTIFY_VAPID_PUBLIC_KEY: process.env.ANOTIFY_VAPID_PUBLIC_KEY || "",
		ANOTIFY_VAPID_PRIVATE_KEY: process.env.ANOTIFY_VAPID_PRIVATE_KEY || "",
	}, extraEnv);
	const proc = spawn(BIN, [], { env, stdio: ["ignore", "pipe", "pipe"] });
	let logBuf = "";
	proc.stdout.on("data", (d) => { logBuf += d; });
	proc.stderr.on("data", (d) => { logBuf += d; });

	const base = `http://localhost:${p}`;
	// 等待健康（最多 10s）
	const deadline = Date.now() + 10000;
	let up = false;
	while (Date.now() < deadline) {
		try {
			const r = await fetch(base + "/health");
			if (r.ok) { up = true; break; }
		} catch { /* not up yet */ }
		await new Promise((r) => setTimeout(r, 200));
	}
	if (!up) {
		proc.kill();
		throw new Error("服务启动失败:\n" + logBuf);
	}
	return {
		base, port: p, dbPath, tmp,
		log: () => logBuf,
		stop: () => { try { proc.kill(); } catch { /* ignore */ } rmSync(tmp, { recursive: true, force: true }); },
	};
}

// ---------- 播种（devseed 后门，仅用于非 auth 套件快速建用户/Key/会话） ----------
export function seed(dbPath, username = "e2e") {
	const out = execFileSync(DEVSEED_BIN, ["-db", dbPath, "-username", username], { encoding: "utf8" });
	const get = (k) => (out.match(new RegExp(`^${k}=(.*)$`, "m")) || [])[1];
	return { uid: get("UID"), sendKey: get("SEND_KEY"), recvKey: get("RECV_KEY"), session: get("SESSION") };
}

// ---------- 常用构造 ----------
export function makeDevice(over = {}) {
	return Object.assign({
		name: "测试设备", platform: "mac", tags: [],
		endpoint: "https://push.example.com/" + Math.random().toString(36).slice(2),
		keys: { p256dh: "BPxTest", auth: "authTest" },
		userAgent: "e2e-test",
	}, over);
}
