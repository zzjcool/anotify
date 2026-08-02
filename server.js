/* Node 服务端：
 *   - 服务 public/ 下的静态文件（index.html, sw.js 等）
 *   - POST /subscribe    保存订阅到 subscriptions.json
 *   - GET  /subscriptions 查看已保存的订阅
 *   - DELETE /subscriptions/:endpoint 删除某个订阅（可选）
 *   - POST /send         向所有已保存订阅推送一条消息
 *
 * 启动：node server.js [PORT]   （默认 5699）
 */

const http = require("http");
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const webpush = require("web-push");

const PORT = Number(process.argv[2]) || 5699;
const PUBLIC_DIR = path.join(__dirname, "public");
const VAPID_FILE = path.join(__dirname, "vapid.json");
const SUBS_FILE = path.join(__dirname, "subscriptions.json");

// ---------- VAPID ----------
let vapid = { publicKey: "", privateKey: "" };
if (fs.existsSync(VAPID_FILE)) {
	try {
		vapid = JSON.parse(fs.readFileSync(VAPID_FILE, "utf8"));
		webpush.setVapidDetails(
			"mailto:notify@example.com",
			vapid.publicKey,
			vapid.privateKey,
		);
	} catch (e) {
		console.error("[server] VAPID 读取失败:", e.message);
		vapid = { publicKey: "", privateKey: "" };
	}
}

// ---------- 订阅存储 ----------
function loadSubs() {
	if (!fs.existsSync(SUBS_FILE)) return [];
	try {
		return JSON.parse(fs.readFileSync(SUBS_FILE, "utf8"));
	} catch {
		return [];
	}
}
function saveSubs(subs) {
	fs.writeFileSync(SUBS_FILE, JSON.stringify(subs, null, 2));
}

const MIME = {
	".html": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".png": "image/png",
	".svg": "image/svg+xml",
	".webmanifest": "application/manifest+json",
	".ico": "image/x-icon",
	".woff2": "font/woff2",
	".woff": "font/woff",
	".ttf": "font/ttf",
};

function sendJson(res, code, obj) {
	const body = JSON.stringify(obj, null, 2);
	res.writeHead(code, { "Content-Type": "application/json; charset=utf-8" });
	res.end(body);
}

function readBody(req) {
	return new Promise((resolve, reject) => {
		let data = "";
		req.on("data", (c) => {
			data += c;
			if (data.length > 1e6) req.destroy();
		});
		req.on("end", () => {
			try {
				resolve(data ? JSON.parse(data) : {});
			} catch (e) {
				reject(e);
			}
		});
		req.on("error", reject);
	});
}

// ---------- 静态文件 ----------
function serveStatic(req, res, pathname) {
	const file = pathname === "/" ? "/index.html" : pathname;
	const full = path.join(PUBLIC_DIR, file);
	// 防目录穿越：规范化后必须仍位于 PUBLIC_DIR 内
	const rel = path.relative(PUBLIC_DIR, full);
	if (rel.startsWith("..") || path.isAbsolute(rel)) {
		res.writeHead(403);
		res.end("Forbidden");
		return;
	}
	fs.readFile(full, (err, buf) => {
		if (err) {
			res.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" });
			res.end("Not Found: " + file);
			return;
		}
		const ext = path.extname(full).toLowerCase();
		res.writeHead(200, {
			"Content-Type": MIME[ext] || "application/octet-stream",
		});
		res.end(buf);
	});
}

// ---------- 路由 ----------
const server = http.createServer(async (req, res) => {
	const url = new URL(req.url, "http://localhost");
	const pathname = decodeURIComponent(url.pathname);
	const method = req.method;

	try {
		// 订阅相关
		if (pathname === "/subscribe" && method === "POST") {
			const sub = await readBody(req);
			if (
				!sub ||
				!sub.endpoint ||
				!sub.keys ||
				!sub.keys.p256dh ||
				!sub.keys.auth
			) {
				sendJson(res, 400, { error: "订阅数据不完整" });
				return;
			}
			const subs = loadSubs();
			const existing = subs.findIndex((s) => s.endpoint === sub.endpoint);
			const record = {
				endpoint: sub.endpoint,
				keys: sub.keys,
				userAgent: sub.userAgent || "",
				time: new Date().toISOString(),
			};
			if (existing >= 0) subs[existing] = record;
			else subs.push(record);
			saveSubs(subs);
			console.log(`[subscribe] 保存订阅 (${subs.length} 个)`);
			sendJson(res, 200, { ok: true, count: subs.length });
			return;
		}

		if (pathname === "/subscriptions" && method === "GET") {
			sendJson(res, 200, {
				count: loadSubs().length,
				subscriptions: loadSubs(),
			});
			return;
		}

		if (pathname === "/vapid-public-key" && method === "GET") {
			sendJson(res, 200, { publicKey: vapid.publicKey });
			return;
		}

		// 发送推送
		if (pathname === "/send" && method === "POST") {
			const body = await readBody(req);
			const title = body.title || "iOS 通知原型";
			const message = body.message || "这是一条来自服务端的测试推送";
			const subs = loadSubs();
			if (subs.length === 0) {
				sendJson(res, 400, { error: "还没有订阅，请先在手机上订阅" });
				return;
			}
			const payload = JSON.stringify({
				title,
				body: message,
				tag: "proto-" + Date.now(),
			});
			const results = [];
			for (const s of subs) {
				try {
					await webpush.sendNotification(s, payload);
					results.push({ endpoint: s.endpoint.slice(-40), status: "sent" });
				} catch (e) {
					results.push({
						endpoint: s.endpoint.slice(-40),
						status: "failed",
						reason: String(e.statusCode || e),
					});
				}
			}
			console.log(`[send] 已尝试推送 ${subs.length} 个订阅`);
			sendJson(res, 200, { ok: true, results });
			return;
		}

		// 健康检查
		if (pathname === "/health") {
			res.writeHead(200, { "Content-Type": "application/json" });
			res.end(
				JSON.stringify({
					ok: true,
					subs: loadSubs().length,
					vapid: !!vapid.publicKey,
				}),
			);
			return;
		}

		// 其它都当作静态文件
		serveStatic(req, res, pathname);
	} catch (e) {
		console.error("[server]", e);
		sendJson(res, 500, { error: String((e && e.message) || e) });
	}
});

server.listen(PORT, () => {
	console.log(`✅ 服务端已启动 http://localhost:${PORT}`);
	console.log(`   静态目录: ${PUBLIC_DIR}`);
	console.log(`   当前订阅: ${loadSubs().length} 个`);
	console.log(`   VAPID: ${vapid.publicKey ? "已配置 ✔" : "未配置 ✘"}`);
});
