/* 命令行推送工具：
 *   向已保存的订阅（subscriptions.json）发送一条推送
 * 用法：
 *   node send.js "消息内容"
 *   node send.js "标题" "正文"
 *   node send.js --json '{"title":"标题","message":"正文"}'
 */

const fs = require("fs");
const path = require("path");
const webpush = require("web-push");

const VAPID_FILE = path.join(__dirname, "vapid.json");
const SUBS_FILE = path.join(__dirname, "subscriptions.json");

if (!fs.existsSync(VAPID_FILE)) {
	console.error("❌ 缺少 vapid.json，请先运行 node genkeys.js 生成 VAPID 密钥");
	process.exit(1);
}
if (!fs.existsSync(SUBS_FILE)) {
	console.error("❌ 还没有订阅（subscriptions.json），请先在手机上订阅");
	process.exit(1);
}

let vapid, subs;
try {
	vapid = JSON.parse(fs.readFileSync(VAPID_FILE, "utf8"));
	subs = JSON.parse(fs.readFileSync(SUBS_FILE, "utf8"));
} catch (e) {
	console.error("❌ 配置文件解析失败:", e.message);
	process.exit(1);
}
webpush.setVapidDetails(
	"mailto:notify@example.com",
	vapid.publicKey,
	vapid.privateKey,
);

// 解析参数
let title = "iOS 通知原型";
let message = "这是一条来自服务端的测试推送";
if (process.argv[2] === "--json") {
	try {
		const data = JSON.parse(process.argv[3] || "{}");
		if (data.title) title = data.title;
		if (data.message) message = data.message;
	} catch (e) {
		console.error("❌ JSON 解析失败:", e.message);
		process.exit(1);
	}
} else if (process.argv[2]) {
	message = process.argv[2];
	if (process.argv[3]) (title = process.argv[2]), (message = process.argv[3]);
}

const payload = JSON.stringify({
	title,
	body: message,
	tag: "proto-" + Date.now(),
});

(async () => {
	console.log(`📨 向 ${subs.length} 个订阅推送:`);
	console.log(`   title: ${title}`);
	console.log(`   message: ${message}`);
	console.log("");

	let ok = 0,
		fail = 0;
	for (const s of subs) {
		try {
			await webpush.sendNotification(s, payload);
			ok++;
			console.log(`   ✔ ${s.endpoint.slice(-50)}`);
		} catch (e) {
			fail++;
			console.log(`   ✘ ${s.endpoint.slice(-50)}  (${e.statusCode || e})`);
		}
	}
	console.log(`\n📊 成功 ${ok}，失败 ${fail}`);
	process.exit(fail > 0 ? 1 : 0);
})();
