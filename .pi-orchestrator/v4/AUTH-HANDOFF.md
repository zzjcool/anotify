# 保留身份域 L1 精确响应（父定向核对）

身份域形状不变；不能强行套资源 `Status`/metadata/spec/status。证据来自现 `internal/server/auth_handlers.go`、`internal/auth/session.go` 与路由，不把缺失 mock handler 当真实后台新 API。

## GET /v1/auth/me

- 会话 Cookie `anotify_session` 认证；没有 Cookie 即使带 Bearer API key 仍401。证据 `mux.go:220,249`、`session.go:188–198`。
- 200顶层用户对象恰为 `{id,username,displayName,role,disabled,createdAt}`，不是 `{user:...}`，无 `authenticated` 字段。字段类型沿现handler/store DTO，不改既有时间表达。证据 `auth_handlers.go:196–215`。
- 匿名401正文精确 `{"error":"未登录"}\n`（session middleware的http.Error，text/plain但正文可JSON解析）；没有reason/code或资源Status。handler直接writeErr也是`{error:message}`，不套新信封。

## 密码登录

- 现有 `POST /v1/auth/password/login`，匿名可访问。请求只含username和现有密码字符串字段；客户端trim username，但密码保持原样，不能trim/改大小写或把它作为用户名别名。
- 成功200精确 `{ "ok": true }`，会话通过Set-Cookie建立，不回user/sessiontoken/authenticated新字段。
- 错JSON/缺字段400、登录服务错误401：既有JSON `{error:<人读文案>}`，无resource reason/code。
- 限流429 `{error:"请求过于频繁，请稍后再试"}`，Retry-After；IP与小写username各10/min。mock可精准模拟现有测试要求，不据此引新公共字段。
- Cookie为anotify_session，Path=/、HttpOnly、SameSite=Lax、Secure按配置、Expires约7天。证据 `auth_handlers.go:499–542`、`ratelimit.go:25–26,311–318`、`session.go:162–171`。

## 前端最小 L1

- 既有login无启动me探测；新增仅在页面加载检查会话：me200且真实顶层用户形状有效→复用现 `nextTarget` 白名单与 `navigate` 二次校验去安全next；401留登录，不经资源API的自动401跳转制造循环。
- me网络/5xx或200坏shape应诚实错误/重试，不伪匿名/假已登录；登录成功仍用现成功导航，不要求passwordlogin返回用户对象。
- Mock GETme/private登录状态需与测试guest/已登录样例一致，公共响应不得加wrapper/authenticated。密码handler缺失501是mock缺口，应按以上现有endpoint/shape补，不更改后台API。
- 复用原安全next，禁止跨origin/javascript等新开放重定向；不把ResourceVersion/namespace资源信封附在身份域。
- 已知密码登录 HTTP429 固定限流语义使用现有本地化 `login.rate_limited` 提示，不强制在英语页面重复显示后台的固定中文 error。验收须同时证明真实请求为429、原响应 `error` 非空、Retry-After 有效、当前语言精确提示可见；不能只检查通用警告、忽略网络abort或将身份错误套为Resource Status。此例外不允许删除其它动态服务诊断。

源码指针：`web-src/pages/login.html:1205–1254,1418–1442`；`internal/server/auth_handlers.go:196–215,508–542`；`internal/server/mux.go:249,258`。对应密码登录测试验证Cookie→me200顶层username，匿名session测试验证401。
