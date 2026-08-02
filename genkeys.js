/* 生成 VAPID 密钥对，保存到 vapid.json */

const fs = require("fs");
const path = require("path");
const webpush = require("web-push");

const keys = webpush.generateVAPIDKeys();
fs.writeFileSync(
	path.join(__dirname, "vapid.json"),
	JSON.stringify(keys, null, 2),
);
console.log("✅ 已生成 VAPID 密钥对并写入 vapid.json");
console.log("publicKey :", keys.publicKey);
console.log("privateKey:", keys.privateKey);
