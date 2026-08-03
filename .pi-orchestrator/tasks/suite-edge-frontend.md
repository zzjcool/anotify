# 任务：编写两个 E2E 套件 `edge_cases.mjs` 和 `frontend.mjs`

先读 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md`（公共约定）。

## 套件 1：`scripts/e2e/suites/edge_cases.mjs`（边界/并发/Unicode）

覆盖 case：

1. 并发上报：同一用户（seed）同时并发 POST 10 条 /v1/notify（Promise.all）→ 全 200；然后 GET /v1/notifications?limit=50 → seq 是 1..10 无重复无缺口（验证 broker 并发 seq 事务正确）
2. Unicode/Emoji：title/body 含中文、emoji、换行、引号、<script> → 200 且 Replay 回来内容一致（XSS 内容按文本存储，渲染安全由前端负责）
3. 超长 title（如 5000 字符）→ 行为明确（200 或 400，不 500）；超长 body 同理
4. 空 deviceTags 数组 [] vs 省略 deviceTags → 都是广播（matched 一致）
5. deviceTags 含空字符串/纯空格 tag → 被归一化剔除，不报错
6. 重复 endpoint 设备：POST 两台相同 endpoint 的设备 → Upsert 语义（后覆盖先，列表只一个）
7. 极大 limit（如 limit=99999）→ 被钳制到合理值，不 500
8. 负数/非法 sinceSeq → 不 500，按 0 处理

## 套件 2：`scripts/e2e/suites/frontend.mjs`（前端渲染 + 路由守卫）

用 Playwright（chromium，channel:"chrome"）。对每个页面 index/login/receivers/keys/security/docs.html：

- 桌面 1280 + 移动 390 两视口：无 JS pageerror、无横向溢出、能滚动到底（/v1/* 的 401/404 是预期降级，不算失败，但要区分）
特殊断言：
- **未登录访问 index/receivers/keys/security** → 应自动跳转到 login.html（这是最近修的路由守卫，验证它生效）。断言最终 URL 含 login.html
- **已登录**（先用虚拟认证器注册建会话，见 auth_flow.mjs 的做法，或用 seed 的 SESSION cookie 注入 ctx.addCookies）访问 index → **不显示**「演示数据」徽章，显示真实数据（哪怕空）
- login.html 未登录可正常渲染（它是公开页，不跳）
虚拟认证器注入方法参考 `scripts/e2e/suites/auth_flow.mjs`（CDP WebAuthn.addVirtualAuthenticator）。SESSION 注入：H.seed 拿 session，ctx.addCookies({name:"anotify_session",value:session,domain:"localhost",path:"/"})。

两个套件都自测跑通（exit 0）后上报。发现前端 bug（路由守卫失效、某页 JS 错误、真实数据显示异常）要明确上报。
