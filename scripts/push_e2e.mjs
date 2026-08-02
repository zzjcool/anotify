#!/usr/bin/env node
/* Anotify 桌面 Chrome Web Push 端到端验证（T36）
 *
 * 流程：
 *   1. headless Chrome 打开 HTTPS 站点（Cloudflare Tunnel 临时域名或本地）
 *   2. 注入脚本：注册 SW → 请求通知权限 → pushManager.subscribe(VAPID) → POST /v1/devices
 *      （需要先有一个登录会话或 API Key 来 POST /v1/devices；此处用预先播种的方式）
 *   3. 服务端 POST /v1/notify 触发推送
 *   4. 断言 SW 收到 push 事件（通过页面轮询 SW 状态/或控制台标记）
 *
 * 说明：真实桌面推送在无头 Chrome 也能收到（FCM endpoint），
 *       但 iOS Safari 的 APNs 链路必须真机验证（T40）。
 *
 * 用法：BASE=https://xxx.trycloudflare.com VAPID_KEY=ant_... node scripts/push_e2e.mjs
 */
import { chromium } from "playwright-core";

const BASE = process.env.BASE || "http://localhost:8080";
const API_KEY = process.env.API_KEY || ""; // 有 devices 写权限的 key 或会话

let passed = 0, failed = 0;
const ok = (m) => { passed++; console.log("  ✅ " + m); };
const bad = (m) => { failed++; console.log("  ❌ " + m); };

async function main() {
	console.log("=== 桌面 Chrome Web Push E2E ===");
	console.log("目标:", BASE);

	const browser = await chromium.launch({
		channel: "chrome",
		headless: true,
		args: ["--no-sandbox"],
	});
	const ctx = await browser.newContext({
		permissions: ["notifications"],
	});
	const page = await ctx.newPage();

	page.on("console", (m) => console.log("  [page]", m.text()));
	page.on("pageerror", (e) => bad("pageerror: " + e));

	await page.goto(BASE + "/", { waitUntil: "load", timeout: 20000 });
	ok("页面加载");

	// 检查 SW / PushManager 支持
	const support = await page.evaluate(() => ({
		sw: "serviceWorker" in navigator,
		push: "PushManager" in window,
		notif: "Notification" in window,
		secure: window.isSecureContext,
	}));
	console.log("  环境:", JSON.stringify(support));
	if (!support.sw || !support.push) bad("当前环境不支持 SW/Push（可能非 HTTPS）");
	else ok("SW + PushManager 可用");

	// 注册 SW + 订阅
	const subResult = await page.evaluate(async (vapidUrl) => {
		try {
			const reg = await navigator.serviceWorker.register("/sw.js");
			await navigator.serviceWorker.ready;
			const perm = await Notification.requestPermission();
			if (perm !== "granted") return { error: "权限被拒: " + perm };
			const { publicKey } = await (await fetch(vapidUrl)).json();
			const b64 = publicKey.replace(/-/g, "+").replace(/_/g, "/");
			const key = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
			const sub = await reg.pushManager.subscribe({
				userVisibleOnly: true,
				applicationServerKey: key,
			});
			const j = sub.toJSON();
			return { endpoint: j.endpoint, keys: j.keys };
		} catch (e) {
			return { error: String(e) };
		}
	}, BASE + "/v1/vapid-public-key");

	if (subResult.error) {
		bad("订阅失败: " + subResult.error);
	} else {
		ok("Push 订阅成功: " + (subResult.endpoint || "").slice(0, 50) + "…");
		console.log("  endpoint:", subResult.endpoint);
	}

	await browser.close();
	console.log(`\n结果：${passed} 通过 / ${failed} 失败`);
	console.log("（注：真实 APNs 推送链路需 iOS 真机 T40 验证；本脚本验证到订阅为止）");
	process.exit(failed === 0 ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
