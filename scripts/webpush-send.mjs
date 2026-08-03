#!/usr/bin/env node
/* webpush-send.mjs — Apple endpoint 专用 Web Push 发送器（webpush-go 库对 Apple 兼容性 bug 的绕行）。
 *
 * 协议：从 stdin 读 JSON {endpoint, p256dh, auth, payload, vapid:{publicKey,privateKey,subject}}，
 *       发送后向 stdout 输出 JSON {status, error}。
 *
 * 依赖：web-push（与原型 send.js 同库，已验证 iOS 全链路可用）。 */
import webpush from "web-push";
import { readFileSync } from "node:fs";

async function main() {
	let req;
	try {
		const raw = readFileSync(0, "utf8");
		req = JSON.parse(raw);
	} catch (e) {
		process.stdout.write(JSON.stringify({ status: 0, error: "invalid stdin JSON: " + String(e).slice(0, 120) }));
		return;
	}
	const { endpoint, p256dh, auth, payload, vapid } = req;
	const subject = vapid.subject || "mailto:notify@example.com";
	webpush.setVapidDetails(subject, vapid.publicKey, vapid.privateKey);
	try {
		const r = await webpush.sendNotification(
			{ endpoint, keys: { p256dh, auth } },
			payload,
		);
		process.stdout.write(JSON.stringify({ status: r.statusCode }));
	} catch (e) {
		process.stdout.write(
			JSON.stringify({ status: e.statusCode || 0, error: String(e.body || e.message).slice(0, 200) }),
		);
	}
}
main().catch((e) => {
	process.stdout.write(JSON.stringify({ status: 0, error: String(e).slice(0, 200) }));
});
