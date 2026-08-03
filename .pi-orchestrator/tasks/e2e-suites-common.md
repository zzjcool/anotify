# E2E 测试套件编写 · 公共约定（所有套件作者必读）

你在主仓库 `/Users/zheng/code/anotify` 工作。测试底座已验证可用。**只写你负责的 suite 文件，不要改底座和别人的套件。**

## 底座（`scripts/e2e/lib/harness.mjs`，已验证）

```js
import * as H from "../lib/harness.mjs";
// 服务生命周期
const server = await H.startServer({ rpId: "localhost" });  // 返回 {base, port, dbPath, log(), stop()}
//   自动分配端口、起临时 DB、等待健康。rpId="localhost" 时 RP_ORIGIN=http://localhost:PORT
// HTTP 客户端
const r = await H.req(server.base, "/v1/notify", { key, session, body, method, headers });
//   返回 {status, json, text, headers}。key=Bearer API Key, session=会话Cookie值
// 播种（非 auth 套件快速建用户+Key+会话，绕过 WebAuthn）
const { uid, sendKey, recvKey, session } = H.seed(server.dbPath, "e2e");
// 断言
H.eq("描述", actual, expected);  H.check("描述", bool, detail?);  H.ok/bad(name)
const passed = H.summary("套件名");  // 打印统计，返回是否全过
// 构造
H.makeDevice({...})  // 生成测试设备订阅对象
```

运行环境：二进制已构建在 `.e2e-bin/anotify` 和 `.e2e-bin/devseed`（harness 默认从这里取，也可用 ANOTIFY_BIN/DEVSEED_BIN 覆盖）。VAPID 从 env `ANOTIFY_VAPID_PUBLIC_KEY/PRIVATE_KEY` 传入 startServer。

## 套件文件结构（严格遵循）

```js
#!/usr/bin/env node
/* SUITE: <名称> — <一句话>
 * 覆盖 case：逐条列出 */
import * as H from "../lib/harness.mjs";
async function main() {
  console.log("=== SUITE: <名称> ===");
  const server = await H.startServer({ rpId: "localhost" });
  // ... 你的断言 ...
  const passed = H.summary("<名称>");
  server.stop();
  process.exit(passed ? 0 : 1);
}
main().catch(async (e) => { console.error(e); process.exit(1); });
```

若需浏览器：`import { chromium } from "playwright-core"`，launch({channel:"chrome",headless:true,args:["--no-sandbox"]})，用毕 `await browser.close()`。

## 运行与自测（必须真实跑通再上报）

```bash
cd /Users/zheng/code/anotify
ANOTIFY_VAPID_PUBLIC_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['publicKey'])") \
ANOTIFY_VAPID_PRIVATE_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['privateKey'])") \
node scripts/e2e/suites/<你的套件>.mjs
```

**要求 exit 0（全过）**。若发现产品 bug 导致断言失败，**不要改断言迁就**，在上报中明确写出「发现产品 bug：xxx」，由协调者决定修产品还是调测试。

## 现有 API 行为参考（已实测）

- POST /v1/notify：无Key/错Key→401；recv scope→403；缺title→400；坏status→400；成功→200 {id,matched,results:[{device,status}]}；无设备→matched=0
- GET /v1/notifications：无会话→401；有会话→200 {notifications,count}（Replay，支持 limit/sinceSeq）
- /v1/devices：无会话→401；POST {endpoint,keys:{p256dh,auth}}→200；GET→200；PATCH {name,enabled,statusFilter,tags}→200（坏statusFilter→400）；DELETE→200
- /v1/keys：POST {name,scopes}→200 {key(明文一次),record}；无scopes→400；GET→200（不含明文）；POST /v1/keys/:id/revoke→200
- GET /v1/vapid-public-key→200 {publicKey}
- GET /health→200 {ok:true}
- 静态：/→200；哈希资源 Cache-Control:public,max-age=31536000,immutable；index.html max-age=60；/v1/* no-store
- 投递规则：设备 enabled ∧ statusFilter 匹配 ∧ tagMatch（无tag消息=广播；无tag设备=catch-all收一切；双方有tag=交集≥1）

## 上报格式

`DONE <套件名>` + 覆盖 case 清单 + 自测结果（X 通过 / Y 失败）+ 发现的产品 bug（若有）+ 遗留风险
