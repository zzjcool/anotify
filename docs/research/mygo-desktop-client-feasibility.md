# 把 Anotify 做成 MyGo 桌面客户端 · 可行性调研

> 调研日期：2026-11-06
> 「mygo」口径：`github.com/egoist/mygo`（v0.7.4 / MIT / Go 1.27+ / 系统 WebView 或自绘原生 UI）
> 本地事实源：`internal/`、`web-src/`、`api/openapi.yaml`、`IOS_TESTING.md`、`.pi-orchestrator/`
> 外部事实源：mygo.egoist.dev 文档、passkeys.dev（macOS 2025-10）、WebKit bug 205350 / PR 70116

## 0. 结论先行

1. **前端能复用，而且复用度很高**：20 页 × 4 语言的纯静态站点，无框架、无打包器、响应式已过 390px 门禁，
   用 `mygo.FileServer` 直接托管 `web/` 产物即可。要改的地方只有 4 处（API base、登录、推送订阅、窗口外壳）。
2. **Passkey 不能在 MyGo 的 WebView 里跑通**——不是 MyGo 的锅，是平台 WebView 的硬约束
   （WKWebView 只认关联域 RP、WebKitGTK 至今没真正支持 WebAuthn）。**换成 Tauri / Wails 结论一样。**
   可行解是「浏览器里过 passkey → 客户端拿 token」，Anotify 已有 device-code 流程可复用。
3. **API 有，但不是给客户端用的 API**：102 个 operation、契约检查已进 `make contract`，
   但 `Authorization: Bearer` 只被 3 个端点接受，控制面全部只认 cookie，且没有 CORS、没有用户级实时通道。
   要让客户端能用，缺口是「token 鉴权 + 设备注册 + 事件流」，改动量属于中等偏小。

**推荐路线**：先做 1~2 天的 MyGo 壳 spike（远端 URL + 托盘 + 通知），
再用约 8~12 人日做「浏览器登录 + token 客户端」垂直切片。**不要**把 passkey 搬进 WebView。

## 1. 三条路径对比

| | A. 壳 + 远端 URL（`LoadURL("https://hub")`） | B. 壳 + 内嵌前端（`mygo.FileServer(web/)`） | C. 内嵌前端 + token 鉴权（推荐） |
|---|---|---|---|
| 前端改动 | 0 | 4 处（见 §2） | 4 处（同 B） |
| 服务端改动 | 0 | 0（用 mygo Go-side fetch 绕 CORS） | token 端点 + Bearer 接受 + 设备注册 |
| Passkey | Windows 可用（WebView2），macOS/Linux 不可用 | 不可用（origin `mygo://localhost` ≠ RP） | 浏览器里过，客户端拿 token ✅ |
| 能否调 Go 能力（托盘/通知/全局快捷键） | ❌ 远端页面不受信，不能调 Go | ✅ | ✅ |
| 桌面通知 | ❌ WebView 无 Web Push | ✅ Go 轮询 + 原生通知 | ✅ |
| 工作量 | 1~2 天 | 3~5 天 | 8~12 天（含服务端） |
| 定位 | 桌面快捷方式，**不是产品客户端** | 有壳无身份，登录上不去 | 真正的桌面客户端 |

## 2. 前端复用：能复用到什么程度

复用面（已验证的事实）：

| 事实 | 位置 |
|---|---|
| 20 个页面 × 4 语言 = 80 个 HTML，Go 模板构建期渲染 | `Makefile:34` `sitegen`、`internal/sitegen/sitegen.go:139-192` |
| 页面零框架、零打包器：Tailwind 是**预生成 CSS**（非 CDN），JS 是手写全局 `partials.js` | `package.json:11-17`、`web-src/layouts/base.html:35-52` |
| 资源全走根绝对路径（`/ui.css`、`/partials.js`），mygo 的 `mygo://localhost/` 根下天然成立 | `web-src/layouts/base.html:37,40,41,52,106,107` |
| 已响应式：抽屉侧栏 + `dvh` + 390×844 视口 E2E 门禁 | `web/ui.css:3415-3445`、`scripts/e2e/suites/frontend.mjs:61-64` |
| i18n 运行时字典、主题防 FOUC、死类守卫 `check-classes.mjs` 都能原样保留 | `internal/sitegen/i18n.go:81-113`、`web-src/layouts/base.html:11-31` |

必须改的 4 处：

1. **API base**：`partials.js` 的 `api()` 用相对路径 `/v1/...`；在 `mygo://localhost` 下相对 URL 会被 webview 处理
   （mygo Fetch 插件文档：「非 http(s) 的 URL 交给 webview 的 fetch」），必须改成
   `window.__ANOTIFY_API_BASE__ + path`（一处拼接，服务端网页仍留空 = 行为不变）。
   改点：`web/partials.js:1671`（`api()`）、`web/partials.js:1690`（`withScope()`）。
2. **登录页**：passkey 在 WebView 里不可用（§3），要换成「在浏览器中登录 → 深链回跳 / 短码」。
   改点：`web-src/pages/login.html:1181-1214`（`navigator.credentials.get`）与整段 ceremony。
3. **通知接收页**：WebView 里没有 `serviceWorker` / `PushManager`，`register("sw.js")` 必然失败。
   改点：`web-src/pages/receivers.html:1252`、能力探测 `:843,:1290`；客户端设备改由 Go 侧登记与接收。
4. **窗口外壳**：安全区（`safe-area-inset` 全仓 0 命中）、自定义标题栏、外链统一丢给系统浏览器
   （远端页面在 `LoadURL` 模式下不受信，`window.open` 需 `SetWindowOpenHandler` 兜）。

不需要改的：cookie 语义。mygo 的 Fetch 插件由 Go 发起请求并持有 `http.Client.Jar`，
`anotify_session`（HttpOnly / SameSite=Lax）在 Go 侧照常保存与回放，**前端 `credentials: "include"` 不用删**。
注意：Fetch 插件拿不到 `Set-Cookie` 响应头，但 jar 会自动存——所以「登录成功与否」要靠响应体判断，不能读头。

## 3. Passkey：现在支持到什么程度 · 为什么壳里不行 · 怎么绕

### 3.1 服务端 + 网页端已经完整支持

| 能力 | 现状 | 证据 |
|---|---|---|
| 库 | `go-webauthn/webauthn v0.17.4` | `go.mod:8` |
| 注册 / 登录（含 discoverable） | ✅ 三步 ceremony | `internal/auth/webauthn.go:174-199,338-401` |
| 多把 passkey、改名、删除、删最后一把 409 | ✅ | `internal/server/auth_handlers.go:306-466` |
| 跨设备补建（另一台已登录设备批准） | ✅ 与 CLI 授权同构 | `internal/auth/enroll.go`、`mux.go:520-548` |
| 恢复码兜底 | ✅ | `auth_handlers.go:468-507` |
| 会话 | Cookie `anotify_session`，HttpOnly / SameSite=Lax / 7 天硬编码 | `internal/auth/session.go:19,162-172`、`mux.go:92` |
| 前端 | 能力探测、错误归因（SecurityError/NotAllowedError）、NEXT 白名单 | `web-src/pages/login.html:906-938,1052-1125` |

缺口（与客户端直接相关）：

- **RP 只允许一个 origin**：`RPOrigins: []string{cfg.RPOrigin}`，env 是单值，`RPAllowCrossOrigin=false`
  （`internal/server/mux.go:91`、`internal/server/config.go:389`）；
- **没有任何「passkey 登录 → 换 token」的端点**：唯一产物是 `Set-Cookie`（`auth_handlers.go:155`）；
- 会话校验只读 Cookie，`Authorization: Bearer <sessionID>` 不被识别（`session.go:184-198`）。

### 3.2 平台事实：WebView 里的 passkey

| 平台 | 结论 | 依据 |
|---|---|---|
| macOS `WKWebView` | 只能对**App 关联域（`webcredentials`）绑定的 RP** 用 passkey；第三方/任意域名不行，官方推荐 `ASWebAuthenticationSession`（= 系统浏览器） | passkeys.dev macOS · WebViews（2025-10-31 更新）、Apple 论坛 726943/734513 |
| Linux `WebKitGTK` | WebAuthn 上游长期「不支持」；GTK/WPE 支持的 PR 到 **2026-07-24** 才合入，且后续仍有 stub 返回 `NotSupportedError` | WebKit bug 205350、PR 70116、PR 72235 |
| Windows `WebView2` | WebAuthn 可用（走 Edge + Windows Hello）——但 origin 必须等于 RP origin | WebView2 既有能力 |

再叠加 MyGo 的 origin 事实：**构建态前端 origin 是 `mygo://localhost`（macOS/Linux）、`http://mygo.localhost`（Windows）**
（mygo.egoist.dev/docs/frontend「How pages load」）。这与 hub 的真实域名（`dev.openaaas.org` 或用户自建域名）不一致，
所以**即使平台支持 WebAuthn，内嵌前端也过不了 RP 校验**；而 `LoadURL("https://hub")` 时 origin 对了、页面却不受信、调不到 Go。

MyGo 仓库里 `webauthn` / `passkey` / `navigator.credentials` 的代码命中数为 **0**——框架层面没有 WebAuthn 桥接。

> 结论：passkey 必须留在系统浏览器里。这不是换框架能解决的（Tauri 用同一个 WKWebView/WebKitGTK，结论相同）。

### 3.3 三种绕法

| 方案 | 流程 | 服务端改动 | 评价 |
|---|---|---|---|
| **S1 深链回跳（推荐）** | App 打开系统浏览器到 `hub/cli-login?redirect=anotify://auth&state=…` → 浏览器里 passkey 登录 → 服务端发一次性 code → 深链回跳 `anotify://auth?code=…` → App 换 token | 新增 1 个一次性 code 端点 + 1 个换 token 端点（可复用 `cli_auth_sessions` 表） | 体验最好，mygo 原生支持 `urlSchemes` + `App.OnOpenURL` |
| **S2 复用现有 device-code** | App 调 `POST /v1/cli-auth/sessions` → 显示短码/二维码 → 用户在已登录浏览器批准 → App poll 拿 `ant_` Key | 0 新协议；只需扩 scope + 让控制面接受 Bearer | 最省事，`web/agent-login.sh` 与 `cli-auth.html` 已验证 |
| **S3 硬上 WebView passkey** | — | 要多 origin + cross-origin，仍只在 Windows 可能成立 | 不推荐 |

## 4. API：能弄，但缺三块

现状（全部已核对）：

| 事实 | 位置 |
|---|---|
| 102 个 operation / 67 条路由，`make contract` 有机器校验 | `api/openapi.yaml`、`scripts/check_openapi.py`、`Makefile:82` |
| `/v1/*` 一律 `Cache-Control: no-store` | `internal/server/static.go:577-583` |
| **Bearer 仅 3 个端点**：`POST/GET /v1/events`（scope `notify:send`）、`GET /v1/keys/self` | `mux.go:382-383,514`、`internal/api/events.go:89` |
| 控制面（tasks/sources/capabilities/nodes/playbooks/keys/devices/admin）**只认 cookie** | `internal/auth/session.go:184-198` + `mux.go` 的 `sessMW` |
| **无 CORS、无 OPTIONS 预检**（Go 源码 0 命中） | 全仓 grep |
| `/v1/devices` 要 cookie（原生端无法只凭 Key 注册推送设备） | `mux.go:267-268` |
| 用户级实时通道**不存在**：唯一 WS 是节点通道 `GET /v1/node/stream` | `mux.go:378`、`internal/ws/node_*` |
| openapi 有 18 个 200 响应无 schema（devices/keys/auth 全组） | `openapi.yaml:284,295,306,2491,2515,2801…` |
| `notify:receive` / `devices:read` 两个 scope 是装饰，从不校验 | `internal/auth/apikey.go:29-31` |

要补的三块（按推荐顺序）：

1. **token 鉴权**：新增「session token」或扩 `api_keys` scope（`console:read` / `console:write`），
   把 `sessMW` 换成 `authMW`（cookie **或** Bearer）。改动集中：`internal/auth/session.go`、`internal/server/mux.go`、
   `internal/auth/apikey.go`（scope 常量）、`api/openapi.yaml`（securitySchemes 已三套齐全，补值即可）。
2. **设备注册带 token**：`POST /v1/devices` 接受 Bearer（桌面端直接登记自己，不必走 Web Push）。
3. **事件流**：先 Go 侧轮询 `GET /v1/console/events?before=`（`internal/server/console_handlers.go:338+`，游标分页）；
   中期加用户级 WS（mygo 有 Go 侧 WebSocket 插件，带握手头；服务端新增 `/v1/stream` 侧 user 通道，
   `.pi-orchestrator/receiver-capability-design.md:216-218` 里「桌面 Pet（WS + replay）」「CLI stream」正是这个方向）。

不做也不会死人的：CORS（Go-side fetch 绕过）、PWA manifest（壳里没用）。

## 5. 工作量与里程碑（单人估算）

| 阶段 | 内容 | 估时 | 验收 |
|---|---|---|---|
| P0 spike | MyGo 壳：`LoadURL` 远端 + 托盘 + 原生通知 + 外链丢浏览器 | 1~2 天 | macOS/Linux/Windows 三端能开窗、能收一条轮询来的通知 |
| P1 身份 | S1 或 S2；服务端一次性 code/换 token；`authMW`；`/v1/devices` Bearer | 5~8 天 | 客户端冷启动 → 浏览器 passkey → 回跳 → 控制台全绿 |
| P2 复用前端 | `api()` base + login 换页 + receivers 改写 + 窗口外壳 | 3~4 天 | `web/` 产物在 `mygo://localhost` 下 20 页无 JS 错误、无溢出 |
| P3 实时与通知 | Go 轮询/WS + 托盘未读 + 点击深链跳 `message.html?id=` | 3~5 天 | 后台 5s 内到达；点击直达详情 |
| P4 分发 | 打包、macOS notarize、Windows 签名、自动更新、多 hub 切换 | 3~5 天 | `mygo build` 三端产物 + 自动更新链路 |

总计 15~24 人日（P0+P1 就能给出「值不值得做」的判断）。

## 6. 风险清单

1. **框架年轻**：MyGo v0.7.4（未 1.0），2026-09-26 建仓、4.7k star、125 个 open issue、日更。
   它的 API 面很大（原生 UI、菜单、更新、i18n 都自己做），升级会破。**建议只吃「webview 壳 + 托盘 + 通知 + 深链」这一小块**。
2. **Go 1.27**：anotify 是 `go 1.25.0`；客户端另起 module，`GOTOOLCHAIN=auto` 可解，但别把 `internal/` 代码 import 进客户端
   （尤其 `internal/ws` 帧协议，会连带工具链与依赖）。
3. **passkey 只在浏览器**：这是产品级约束，要在文案上写清楚（客户端首启必然跳出一次浏览器）。
4. **自托管多域名**：RP ID 随用户域名变化 → 任何「App 内置关联域」的方案都不成立，S1/S2 才是通用解。
5. **通知通道**：WebView 里没有 Web Push；桌面通知只能靠 Go 轮询/WS + 原生通知，与现有 `web/sw.js` 是两套实现，
   别指望复用 service worker。
6. **文档漂移**（会误导实现者）：`IOS_TESTING.md:60-70` 仍写已删除的 `POST /v1/notify`；
   `openapi.yaml:24-30` 说旧端点「仍在服务」；`/devices` 被标成 `deprecated` 但它是唯一在用入口。
7. **PWA 已经覆盖了 60~80% 的收益**：macOS「添加到程序坞」/ Chrome 安装 + Web Push 现在就能用、且 passkey 可用。
   桌面壳的**增量价值**只有：常驻托盘/Pet 窗口、全局快捷键、不依赖浏览器、多 hub 切换、系统级通知点击路由。
   —— 这一条决定「要不要做」，建议先想清楚再做 P1 之后的事。

## 7. 补充：passkey 退为可选之后，能不能上原生界面（`ui` 包）

**能，而且和「passkey 外包给浏览器」是同一个决定的两面**：原生界面里没有浏览器上下文，
Go 直接发 HTTP（token 或 cookie jar 自己管），§3 的 RP origin / CORS / cookie 三个坑一次性消失。

### 7.1 不是二选一

| 组合方式 | 依据 |
|---|---|
| 一个 App 同时开 native UI 窗口和 webview 窗口 | 文档「One app can have windows of both kinds」 |
| native UI 窗口里**嵌** `ui.WebView`（作为元素，原生 UI 绘制在其上） | `/docs/ui/web-view` |
| 混合的 web 页仍是 App 自有页，**可以调 Go 方法** | `/docs/ui/web-view#its-page` |

### 7.2 组件覆盖度（对 anotify 意外地合适）

原生控件齐全：`Table`/`List`/`Outline`/`Sidebar`/`Split`/`Toolbar`/`Segmented`/`Tabs`、
`TextInput`/`SearchField`/`Combobox`/`TokenField`、`Dialog`/`Popover`/`Toast`、
`Progress`/`Spinner`/`Meter`/`Badge`、**`Charts`（bars/lines/areas/stacks/donut + scales/axes + 键盘可选区）**、
`Markdown`、`CodeEditor`、`DiffViewer`、`Canvas`+`Painter`（自绘/`Path`/`Transform`/动画）、`Image`/`Icon`/`Avatar`。

页面映射（仓库实际 20 页）：

| 页 | 原生实现 | 难度 |
|---|---|---|
| `index` / `events` / `message` / 任务详情 | Table + List + Toast + Badge | 低 |
| `trace` / `source`（flot 图） | `ui.Charts` 或 `Canvas` 自绘 | 中（图是主要工作量） |
| `receivers` / `keys` / `security` / `admin` / `nodes` / `node` | Form/Table/Dialog | 低 |
| `docs`（openapi 渲染 + marked.js） | `ui.Markdown` 或嵌 `ui.WebView` | 中 |
| `playbooks` / `playbook` / `sources` / `edges` / `capabilities` | Table + `CodeEditor`（YAML/JSON） | 中 |
| `login` / `passkey-enroll` / `cli-auth` | **不搬**：本来就在浏览器里跑，客户端只需「开浏览器 + 深链回跳」 | — |

### 7.3 要重做的「设计资产」（不是代码）

- `web/tokens.css` 的灯/暗双套 token → `ui.Theme()` + 自定义 theme 结构（1:1 搬变量，小）；
- 4 语言 YAML（`web-src/locales/*.yaml`）→ mygo i18n 的 ICU JSON catalog（`locales/<tag>.json`，`source: en`），
  `{{t "key"}}` 改为 Go 侧 i18n 调用；
- `web/ui.css`（~101KB 组件类）与 `check-classes.mjs` 死类守卫 → 全部失效；`make fe` 只服务网页版；
- 网页版与桌面版从此是**两套 UI 实现**，双维护成本要预算。

### 7.4 工作量（到「可用」）

| 做法 | 内容 | 估时 |
|---|---|---|
| 纯原生 | 16 个可搬视图 + 托盘/通知/快捷键/深链 | 20~33 人日 |
| **混合（推荐）** | native 外壳 + 4 个 native 视图（收件箱/详情/设备/设置）+ 其余用 `ui.WebView` 嵌现有 HTML，之后逐个替换 | 10~14 人日 |
| 纯 WebView（§1 路径 C） | 现有 20 页全复用 | 8~12 人日 |

### 7.5 风险

1. **押注面变大**：走原生等于把产品押在 v0.7.4（6 周龄、125 open issue）的**自绘渲染栈**上（表格/图表/代码编辑器都是新写的），
   而 WebView 路线只吃它的窗口/托盘那一层。
2. **Linux 落地**：原生渲染走 OpenGL + GTK3，Wayland 下窗口位置由 compositor 决定；CJK 字体回退依赖系统文本引擎，
   我们有 zh/ja 两套语言，必须实测。
3. **无障碍**：原生 UI 有 accessibility 文档，但要比 web 版重新验证一遍（web 版已过 E2E）。
4. **后期想「原生做 passkey」仍然不行**：mygo 仓库 `webauthn` 命中 0，原生 WebAuthn 需要各平台原生 API
   （macOS `ASAuthorizationController` + 关联域），且只对自有域名成立——**passkey 永久留在浏览器**。

### 7.6 trace 页逐项评估（拓扑图 + 瀑布图）

trace 页有两张图，难度差很多：

| 图 | 实现 | native 难度 |
|---|---|---|
| 瀑布图 `traceview.js:118` `renderWaterfall` | 纯百分比布局：左侧缩进 + 状态点 + 能力名，右侧 `left%/width%` 放条，4 个刻度，悬停用 `title` | **0.5~1 天**（行 + 绝对定位矩形，`ui.Draw` 或 `Box().Absolute().Left(pct)`） |
| 拓扑图 `flograph.js`（1479 行）+ `graphs.js`（1056 行） | dagre 布局 + HTML 卡片节点 + SVG 贝塞尔连线 + pan/zoom/fit + 边标签避让 + 环路虚线 | **6~10 天**，而且是最难追平网页版的一块 |

能力对照（逐项已核实）：

| flograph 现在做的事 | native 对应 |
|---|---|
| dagre 分层布局 | **无 Go 移植**，要自己写 Sugiyama + `findBackEdges`（`graphs.js:982`） ≈ 3~5 天 |
| SVG `M/L/Q/C` 贝塞尔（`flograph.js:110-343`） | `ui.Path` 直线 + 曲线，`StrokePath`/`FillPath` |
| 虚线（cycle/muted） | `Path.Dash` |
| 箭头 marker | `FillPath` 画三角 |
| 卡片节点（图标+标题+副标题+徽章） | `Box().Radius().Background().Border()` + `Text`/`Icon`/`Badge`，放进 `Transform` 容器（指针/文本/无障碍跟随） |
| pan（mousedown/move，`flograph.js:848-881`） | `Element.Dragged()` + `PointerPosition` |
| 滚轮缩放 / 触控板捏合（`flograph.js:885-930`） | ✅ `HandleInput` → `ui.InputScroll{DX,DY,Precise}` / `ui.InputZoom{Scale,X,Y}`，文档就是为图表写的 |
| fit / zoom 按钮 / 全屏 | `Transform` + `ui.Shortcut`；全屏用窗口 `FullScreen` |
| 点击节点跳详情 | `Clicked()` |
| tooltip | `ui.Tooltip` |
| 边端口吸附 + 标签避让（`pickLabelSpot`/`snapEnd`，~250 行几何） | 要照搬重写 ≈ 2~3 天 |

规模事实：`graphs.js:523,636` 的 `limit` 默认 120 个节点，实际 trace 通常 3~15 个任务 → **性能不是问题**，
瓶颈只在布局算法与边缘几何。

**结论：trace 页整页用 `ui.WebView` 嵌现有 `trace.html`**（它本来就是 1200px 主视图 + 一张卡，
混排反而不统一）。真值得 native 的是 events 流 / 任务详情 / 托盘 / 通知。

## 8. 建议的下一步

1. **P0 spike**（1~2 天）：新 module `client/`（不进 `internal/`），先跑**两个窗口**：
   一个 `mygo.NewWindow{URL: "https://dev.openaaas.org"}`（壳路线），一个 `Content: ui.View(...)`（原生路线，Table + Tabs 列 `GET /v1/console/events`），
   都加 `SetWindowOpenHandler` + 托盘 + 一条 Go 轮询的原生通知（用 devseed 的会话 cookie 手塞 jar 验证）。
   这一步就足以判断「原生界面值不值 20~33 人日」。
2. **决策点**：若结论是「要常驻、要托盘、要脱离浏览器」，再进 P1；否则把力气花在 PWA 上（成本低一个数量级）。
3. **P1 前必须先定的契约**：token 形态（`sess_*` 不透明 token vs 扩 scope 的 `ant_*` Key）、
   scope 名（`console:read` / `console:write`）、换票端点路径、以及 `authMW` 是否同时接受 cookie 与 Bearer（建议接受，兼容网页）。
