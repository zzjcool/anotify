# Anotify · 开发指南

> 新 Agent / 贡献者上手必读。本文档把架构决策、开发流程、测试方法、踩过的坑全部固化，确保任何人（或 AI Agent）接手都能快速进入开发状态。

## 1. 项目是什么

Anotify 是「Agent 完成即通知」平台：Agent 任务结束后通过事件 Hook 上报，后端用 Web Push 把结果推送到用户所有设备（iOS/Mac/PC/Android）。Passkey 无密码登录，单 Go 二进制 + SQLite，易自托管。

完整设计见 `design/tech-scheme.html`，产品定位见 `README.md`。

## 2. 技术栈与关键决策

| 层 | 选型 | 决策理由 |
| --- | --- | --- |
| 后端 | **Go 单二进制** + `go:embed` 内嵌前端 | 自托管首要价值是「单文件分发」，不引入运行时依赖 |
| 存储 | **SQLite**（纯 Go `modernc.org/sqlite`，无 CGO） | 数据库先行；Broker 接口抽象，未来可换 NATS/Redis/Kafka 零业务改动 |
| 认证 | **Passkey (WebAuthn)** + API Key (argon2id) | 无密码；Key 仅存哈希带 scope |
| 推送 | **Web Push (VAPID)**，FCM + APNs 双链路 | 标准 Web Push，按 endpoint 自动路由 |
| 前端 | **纯静态 HTML + Tailwind CDN + tokens.css** | 无构建框架；用指纹脚本实现 content-hash（不引入 Vite 等重型链） |
| 测试 | Go 单测 + Node E2E（Playwright 虚拟认证器） | Passkey 全流程无头自动化；除 iOS 真机外全自动 |

### 核心架构（必须理解）

```
Agent ─POST /v1/notify─▶ Go 后端 ─▶ Broker(SQLite) ─┬─▶ WS 派发器(消费者1) ─▶ WebSocket 客户端
 (Bearer Key)        单二进制      队列+历史       └─▶ Push 派发器(消费者2) ─▶ Web Push(APNs/FCM)
```

**两条接收通道 = Broker 的两个消费者**（WS + WebPush）。Broker 接口（`internal/broker/broker.go`）是核心抽象边界，划在 Publish/Subscribe/Ack/Replay 上，不在 HTTP/WS 处理器上——换 MQ 时业务代码零改动。

### 投递规则（投递给设备 ⟺ 三条件全满足）

1. `device.enabled = true`
2. `scopeMatch(device.event_scope, msg.agentState)`：all 全过；final 仅终态（done/interrupted/error）
3. `tagMatch`：消息无 deviceTags → 广播到所有 enabled 设备；设备无 tags → catch-all 收一切；双方都有 tags → 交集 ≥1（ANY，非 ALL）

## 3. 环境准备

```bash
# Go（自动补 go1.25 工具链）
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

# VAPID 密钥（首次，推送功能必需）
make keys  # 输出 ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY

# 依赖（前端测试用）
npm install  # playwright-core + web-push

# Docker（可选，构建镜像用）
docker info  # 确认 daemon 在跑
```

## 4. 开发流程

### 前端站点生成（sitegen · 构建期）

前端已框架化为「构建期 Go template 静态站点生成」（借鉴 rssyes 布局/i18n 与 Hugo 思想），**产物仍是纯静态 + embed + 指纹**，未引入任何运行时模板引擎。

- **唯一的页面源**：`web-src/`（人工只编辑这里）
  - `layouts/base.html`：公共外壳（`<head>` 图标/字体/css + `#page-main` 骨架 + partials.js/i18n.js 引入）
  - `layouts/login.html`：login 独立布局（无 sidebar）
  - `pages/*.html`：每页 `{{define "content"}}` / `{{define "title"}}` / 可选 `style`/`script`/`fonts-extra`。首行 `<!-- layout: login -->` 指定非默认布局
  - `locales/{zh-CN,en}.yaml`：i18n 翻译（嵌套 YAML，扁平化为 `page.key`）
- **生成产物（勿手改，已 gitignore）**：`web/*.html`（zh 根路径）、`web/en/*.html`、`web/i18n.{lang}.js`
- **静态资源（保持跟踪）**：`web/ui.css` `tokens.css` `tailwind.css` `partials.js` `fonts/` `assets/` `sw.js` `manifest.webmanifest`

### Tailwind（构建期 CSS，不用 Play CDN）

线上和 embed 的页面（`web-src/layouts/`）引用 `/tailwind.css`，即提交在仓库里的 `web/tailwind.css`。**不要**在这些页面加载 `cdn.tailwindcss.com`：Play CDN 会在控制台打出 “should not be used in production”。

`design/*.html` 设计稿仍引用 Play CDN，只给本地对照，不进 sitegen，也不进 embed。

改了 `web-src/` 或 `web/partials.js` 里的 Tailwind 工具类之后：

```bash
npm install          # 首次需要 devDependency tailwindcss
npm run tailwind     # 或 node scripts/build-tailwind.mjs，重写 web/tailwind.css
```

`make fe` / `make ci` **不**安装 npm 依赖、也不跑 Tailwind CLI。它们只跑 `scripts/check-tailwind-css.mjs`：源码里的工具类必须已经出现在提交的 `web/tailwind.css` 里。

```bash
make sitegen  # go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en
make fe       # sitegen + hash（指纹化到 internal/server/dist）
```

**改页面/i18n 后**：编辑 `web-src/`，跑 `make build`（自动 sitegen+hash+go build）。

**i18n 两类文案**：

- HTML 静态文本 → 构建期 `{{t "page.key"}}`（在 pages/layouts 里用，locales 填值）
- JS 字符串 → 运行时 `Anotify.t('key', '默认中文')`（locales 生成 `i18n.{lang}.js`，partials.js 导航/页脚已接入）

### 本地开发

```bash
make mock   # 纯前端预览（mock 数据，不连后端），:7788
make dev    # 先跑 sitegen 生成 web/*.html，再用 web/ 本地目录起服务（改完即生效），:8080
make build  # 产出单二进制 anotify（sitegen + 指纹 + embed）
```

#### 只想看前端？`make mock`

调 UI 时不需要把整条后端链路拉起来（server + tunnel + devseed + WS worker）。
任何一步出问题，看到的都是"页面空着"，很难判断是前端坏了还是后端没起来。

```
make mock                  # 本地 + trycloudflare 公网地址，→ :7788
make mock-local            # 只本机，不起隧道
make mock MOCK_PORT=7789   # 换端口
make mock MOCK_BIND=0.0.0.0  # 同时暴露到局域网（手机/同 WiFi 的人能开）
```

和 `make dev` 一样默认带 **trycloudflare 隧道**，所以可以直接把这个地址发给别人看
（不需要数据库、后端、节点，对方打开就能点）。

#### 内网地址为什么默认不可用（`MOCK_BIND`）

mock 服务器默认**只绑 `127.0.0.1`**，不暴露到局域网。这是刻意的：

> `cmd/fe-preview` 的 `-addr` 默认值就是 `localhost:7788`，而 `-port`
> 只替换端口的数字部分、**保留 host**。所以光传 `-port` 得到的监听地址
> 还是 `127.0.0.1:7788`。这时局域网主机收到的 SYN 会被内核**直接 RST
> （Connection refused）**，数据包根本没进 ufw 的 INPUT 链——
> **防火墙怎么配都不会生效**。
>
> 所以脚本显式传 `-addr "$MOCK_BIND:$MOCK_PORT"`，把「host」和「port」
> 当成同一件事处理。

要给同一 WiFi 的手机或同事看：

```bash
make mock MOCK_BIND=0.0.0.0 MOCK_PORT=7788
# 如果提示「被 ufw 拦着」，放行一次即可（一直有效）：
#   sudo ufw allow 7788/tcp
```

脚本会自己分辨三种状态，不会误导人：

| 输出 | 含义 | 怎么办 |
|---|---|---|
| `✖ 不可用（当前只绑 127.0.0.1）` | 没绑 0.0.0.0，内核回 RST | 加 `MOCK_BIND=0.0.0.0` |
| `⚠️ 被 ufw 拦着（连上去要等超时）` | 绑对了但 ufw 默认 DROP 且未放行端口 | `sudo ufw allow <端口>/tcp` |
| `（手机连同一 WiFi 可用）` | 已绑非回环 + ufw 已放行或未启用 | 直接用 |

对比 `make dev`：`dev.sh` 用的是 `ANOTIFY_ADDR=:8080`（空 host = 全部接口），
所以它天然对内网可见；mock 的 `fe-preview` 默认不是，两边不要混为一谈。

`make fe-preview` 是 `make mock` 的同义词（旧写法仍可用）。

隧道域名每次启动都会变；拿到域名后脚本会真探测一次可达性再宣布可用。
quick tunnel 的新域名要下发到边缘节点，实测 60~90 秒才稳定，所以探测窗口
给到约 120 秒（先静等 15 秒再开始探）。如果本机始终探不通但隧道已注册成功，
脚本仍然把地址打印出来——那种情况通常只是本机代理拦了 `*.trycloudflare.com`，
换手机或换网络就能访问。

它只做一件事：起一个静态服务器托管 `web/`，并给每个 HTML 注入 `web/mock.js`。
于是**所有 `/v1/*` 请求都在浏览器里被拦下**，由 mock 直接应答，一个请求都不会
真正发出。页面底部有个黄色提示条，避免误以为是真数据。

- 数据是内存里的可变状态：新建源、立即运行、删边都会真的改变它，能点着试完整流程
- 刷新页面回到初始状态
- 任意接口加 `?mockFail=1` 可以看错误态长什么样
- `window.MockApi.state` 可以在控制台里改数据
- 换端口：`make mock MOCK_PORT=7789`

**为什么 mock 在浏览器侧而不是 Go 侧假装后端**：Go 侧等于维护第二套后端，
会跟真后端漂移，看到的"效果"就不可信；而且 mock 里要做状态（新建/触发/任务
跑完），Go 侧等于重写一遍。浏览器侧改一行就生效，不用重新编译。

`web/mock.js` **不会**进 `internal/server/dist/`（指纹脚本只处理
`FINGERPRINT_EXT` 里列出的扩展名），所以生产二进制里没有它。

#### 免 passkey 登录（换域名时很省事）

`make dev` 会顺手播种一个开发账号，并在 `web/__dev_login.html` 生成一个
**自动登录页**：

```
▶ 播种开发账号 demo…
  ✅ 自动登录页：http://localhost:8080/__dev_login.html
     公网版本：https://dev.openaaas.org/__dev_login.html
```

打开它 → 写入会话 cookie → 跳工作台。为什么需要这个：**WebAuthn 的 RP_ID
必须与访问域名严格一致**，换成 trycloudflare 随机域名 / 局域网 IP / 手机访问时
就注册不了 passkey，否则每次都要手工造会话、手工往浏览器粘 cookie。

#### 示例数据（默认造，`NO_DEMO_DATA=1` 起空实例）

`make dev` 默认把一份示例数据摆好，打开就能看到完整效果：

```
▶ 造示例数据（能力图 + 节点 + 事件链）…
     节点已上线（3 个能力，真实 WS 握手）
     已投递 7 条事件，等待任务链执行…
     节点执行了 4 个任务，链路已全部跑完，断开
```

**它不是往库里插假状态**。`devseed -demo` 做的是真事：

1. 建能力图（`analyze-pr` / `apply-fix` / `deploy-preview` + 8 条边）
2. 注册一个节点并**用真实 WS 握手上线**
3. 通过 `POST /v1/events` 投 7 条事件（等同于外部系统发 webhook）
4. 节点接单、`ack`、回报 `result`，链路自己长出来

所以界面上的事件流、任务链、`out` 三态都是**真跑出来的**：

```
github -opened------> analyze-pr ─needs-fix─> apply-fix ─ok─> deploy-preview
cron   -nightly----->               └─approved──────────────────────┘
http   -webhook-----> deploy-preview
manual -manual.run--> deploy-preview
```

`analyze-pr` 按 `payload.changedLines` 分叉（>200 走 `needs-fix`），
所以同一个图能演「小改直接过」和「大改要返工」两种走向。

起一个**完全空**的实例：

```bash
NO_DEMO_DATA=1 make dev      # 只有账号和内置起点能力，0 事件 0 边 0 任务
```

| 变量 | 作用 |
|---|---|
| `NO_DEMO_DATA=1` | 不造示例数据（要自己从零配图时用） |
| `ANOTIFY_DEMO_SEC=60` | 示例节点保持在线的秒数上限（默认 30；跑完会提前退出） |

> 实现细节：`-demo` 必须**在 server 起来之后**跑（它要 POST 事件、还要 WS
> 上线节点）。`dev.sh` 第 5 步只造账号和登录页，示例数据在第 8 步。
> 同名节点会被复用并轮换 token，重复跑不会攒出一堆离线死节点。

#### tunnel 模式

`make dev` 默认起 **quick tunnel**（trycloudflare 随机域名），零配置，
**不会碰你已登录的命名 tunnel**：

```
▶ 启动 quick tunnel（trycloudflare 随机域名 → localhost:8080）…
  ✅ tunnel 就绪（https://<随机>.trycloudflare.com）
```

| 变量 | 行为 |
|---|---|
| （默认） | quick tunnel，随机域名每次变；域名从 cloudflared 日志解析 |
| `ANOTIFY_TUNNEL=named` | 用命名 tunnel（固定域名）；需事先 `cloudflared login` + `tunnel create` + 配 DNS。名字由 `ANOTIFY_TUNNEL_NAME`（默认 `anotify`）指定 |
| `NO_TUNNEL=1` | 只起本地，不起 tunnel |

quick tunnel 的域名**要等 Cloudflare 边缘传播**（实测几秒到几十秒），
所以 `make dev` 会在 server 起来之后才探测可达性，并打印**已验证可用**的地址：

```
▶ 检查公网可达性…
  ✅ 公网可达：https://<随机>.trycloudflare.com
     免 passkey 登录：https://<随机>.trycloudflare.com/__dev_login.html
     （此地址随进程退出失效，下次启动会变）
```

探测失败会明确说「tunnel 进程注册成功但域名不可达」，而不是给一个打不开的链接。

#### 三种访问地址

`make dev` 最后会打印一份地址汇总，三条路各有用途：

| 地址 | 用途 | 说明 |
|---|---|---|
| `http://localhost:8080` | 本机 | cookie 在这里最稳定 |
| `http://<内网IP>:8080` | 手机 / 同网段机器 | 同一 WiFi 下直接打开 |
| `https://<随机>.trycloudflare.com` | 外网 / 分享 | 每次启动都变，退出即失效 |

内网地址那块长这样：

```
▶ 内网访问（手机/同网段机器，同一 WiFi 下直接打开）：
   http://192.168.8.204:8080
   免 passkey 登录：http://192.168.8.204:8080/__dev_login.html
```

内网 IP 取的是**默认路由的出口 IP**（`ip route get 1.1.1.1`），不是
`hostname -I | awk '{print $1}'`——后者在有 docker0 / `br-*` 网桥的机器上
经常先返回 `172.17.0.1`，那个地址手机根本连不上。探测到非私有网段
（10/8、192.168/16、172.16/12）会直接丢弃，不拿出来误导人。

**如果内网地址打不开，先看是不是防火墙。** `dev.sh` 会检测 ufw 并直接说出来：

```
▶ 内网地址被 ufw 拦住了
   这台机器开了 ufw（默认策略 DROP），但没有放行 8080 端口。
   本机 curl 能通只是走了回环，手机/别的机器连不上，符合预期。

   放行（需要 sudo，执行一次即可，一直有效）：
     sudo ufw allow 8080/tcp
```

这点很容易误判：**本机 `curl http://<内网IP>:8080` 恒通**，因为回环流量不过
INPUT 链，ufw 的 `DEFAULT_INPUT_POLICY=DROP` 拦不到它。所以那个测试方法
没有诊断价值——只能用另一台设备试，或者直接看 ufw 规则。

> 手机访问时同样绕不开 WebAuthn 的 RP_ID 限制（默认绑 `localhost`），
> 所以走自动登录页那条路。要让手机用 passkey，把 `ANOTIFY_RP_ID`
> 设成内网 IP、`ANOTIFY_RP_ORIGIN` 设成 `http://<内网IP>:8080` 再启动。

> 排查提示：如果 `getent hosts <任意域名>` 返回 `198.18.x.x`，那是 clash
> 之类 **fake-ip 模式**的正常表现，不代表被劫持——域名仍然能通。
> 所以脚本只认真实 HTTP 请求，不看 DNS 结果。

可选环境变量：

| 变量 | 作用 |
|---|---|
| `NO_AUTOLOGIN=1` | 不播种开发账号、不生成登录页（只用 passkey） |
| `ANOTIFY_DEV_USER=alice` | 开发账号用户名（默认 `demo`） |
| `NO_TUNNEL=1` | 只起 server，不起 cloudflared |

两点注意：

- 页面里**内嵌着有效会话**，所以 `dev.sh` 退出时会删掉它；文件权限 0600，
  且 `web/*.html` 已被 `.gitignore` 覆盖，不会入库
- 它写的是普通 cookie（JS 无法设 `HttpOnly`），比正常登录的会话更容易被页面
  脚本读到。**仅限开发**，别把这个机制带到生产

### 指纹脚本（content-hash + CDN 缓存）

`scripts/hash.mjs` 在构建期把 `web/` → `internal/server/dist/`：

- JS/CSS/字体/图片加内容哈希（`app.a1b2c3.js`）→ CDN 可 immutable 长缓存
- HTML 引用改写为哈希文件名
- `sw.js` / `manifest.webmanifest` 不指纹（路径必须固定）
- **CSS 内 url() 引用也会改写**（fonts.css 引用的 .woff2）
- 生成 `manifest.json`（原始名 → 哈希名映射）

缓存分级（`internal/server/static.go` classify）：

- 哈希文件 → `Cache-Control: public, max-age=31536000, immutable`
- index.html / sw.js → `public, max-age=60`
- `/v1/*` → `no-store`

### 改前端后必须重新指纹 + 重建

```bash
make build  # 自动跑指纹 + go build
```

## 5. 测试体系（固化门禁）

> **规则：每次开发完成后，必须跑 `make e2e` 全绿才算完成。** 见 `E2E_TESTING.md`。

```bash
make e2e              # 全量：构建 + Go 单测 + 9 个 E2E 套件
make e2e-one S=auth_flow  # 只跑某个套件
make ci               # PR 门禁（与 GitHub Actions 相同；不含 e2e / integration）
```

### PR CI（`.github/workflows/ci.yml`）

指向 `main` 的 pull request，以及推送到 `main`，跑 `make ci`。本地同一条命令：

```bash
make ci
```

门禁内容：

1. `gofmt -l`：有未格式化的 `.go` 文件即失败（跳过 `vendor/`）
2. `make fe`：sitegen + 指纹，供下一步 `-tags embeddist` 内嵌。默认 `go build` / `go test` / `go run` 不嵌入 dist（开发期设 `ANOTIFY_STATIC` 即可编译）
3. `go vet ./...`
4. `go build ./...`，以及 `go build -tags embeddist`（生产单二进制；dist 由第 2 步生成）
5. `go test ./... -count=1`（与 `make test` 相同）

不跑 `make e2e`、`make integration`、`scripts/e2e/`（要浏览器或已启动的服务）。`make e2e` 仍是合并前的固化门禁；这条 CI 只守构建、格式和包内单测。名为 `*_e2e_test.go` 的 Go 测试走 `httptest`，不连外部服务，留在 `go test` 里。

### 测试分层

| 层 | 位置 | 覆盖 |
| --- | --- | --- |
| Go 单测 | `internal/*_test.go` | broker/auth/api/push/ws/store/server 包级逻辑 |
| API 契约 | `scripts/e2e/suites/api_contract.mjs` | 全端点状态码/鉴权/校验 |
| Passkey 认证 | `scripts/e2e/suites/auth_flow.mjs` | **虚拟认证器无头跑注册/登录/登出/会话** |
| 节点协议 | `scripts/e2e/suites/ws_protocol.mjs` | 九帧（hello/heartbeat/ack/progress/result + welcome/assign/cancel/error）、握手鉴权、out 三态、心跳判死 |
| 派发路由 | `scripts/e2e/suites/routing.mjs` | 派发四步 + 六种未触发原因（no_edge/param_schema/no_node/foreign_user/path）、扇出、容量不足留 pending |
| 持久化 | `scripts/e2e/suites/persistence.mjs` | 重启后 events/tasks 存活、状态机与 out 三态保持、终态不可变 |
| 安全 | `scripts/e2e/suites/security.mjs` | scope 越权/Key 篡改/哈希不可逆/SQL 注入/路径穿越 |
| 前端 | `scripts/e2e/suites/frontend.mjs` | 6 页渲染 + 路由守卫 + 真实数据 |
| 边界 | `scripts/e2e/suites/edge_cases.mjs` | 并发 seq/Unicode/超大体/畸形输入 |
| 容量与背压 | `scripts/e2e/suites/capacity.mjs` | #229 保留期（默认 30 天 / 0=永久）与 #228 背压：超阈 503 + `Retry-After` + 机器可读 body、任务不丢、读端点不受影响、积压清掉后自愈；过期事件被清、窗口内保留 |

### 测试底座

`scripts/e2e/lib/harness.mjs`：服务生命周期（自动分配端口/临时 DB/等健康）、HTTP 客户端（key/session 注入）、断言收集、devseed 播种。每个套件独立起服务，互不干扰。`run_all.sh` 套件间加 1.5s 间隔避免端口释放时序问题。

### 关键技术：Passkey 自动化

Playwright CDP 虚拟认证器（`WebAuthn.addVirtualAuthenticator`）让 WebAuthn 注册/登录无头完成，**无需真人生物识别**。这是把 auth 全流程自动化的关键。注意：虚拟认证器 BackupEligible 默认 false，覆盖不到真实同步型 Passkey（true）的路径——该不变量由 store 单测 `TestPasskey_BackupEligibleRoundtrip` 覆盖。

### 唯一人工环节

iOS 真机 APNs 推送（需真实 iPhone，见 `IOS_TESTING.md`）。其余全自动。

## 6. 目录结构

```
cmd/server/        单二进制入口
cmd/devseed/       测试播种工具（建用户+Key+会话，绕过 WebAuthn）
cmd/sitegen/       前端站点生成器 CLI（web-src → web 静态 HTML）
internal/
  sitegen/         站点生成核心（模板缓存/layout+content/i18n 合成）
  store/           SQLite 数据访问 + schema.sql + 幂等列迁移
  broker/          消息队列抽象 + SQLiteBroker
  auth/            Passkey(WebAuthn) + API Key(argon2id) + 会话
  authn/           Key 校验接口（解耦 auth 与 api/ws）
  api/             /v1/notify 上报处理器
  ws/              WebSocket 派发器（消费者1）
  push/            Web Push 派发器（消费者2，VAPID）
  route/           标签/status 投递过滤（共享纯逻辑）
  server/          路由装配 + 静态资源(CDN缓存) + embed
web-src/           前端页面源（layouts/pages/locales，人工编辑处）
web/               前端产物（*.html 由 sitegen 生成已 gitignore；css/js/fonts/assets 静态资源保留）
scripts/
  hash.mjs         指纹脚本（支持根绝对路径指纹化）
  gen-icons.mjs    图标生成器（Sacramento 字体 → 各尺寸 PNG）
  genkeys.go       VAPID 密钥生成
  e2e/             E2E 测试体系（run_all.sh + lib/ + suites/）
design/            设计稿 + 技术方案（参考用）
```

## 7. 贡献规范

1. **改代码前**：读 `AGENTS.md`（子 Agent 约定）+ 相关包的 `*_test.go`
2. **改完必须**：`make e2e` 全绿才算完成
3. **提交信息**：`type(scope): 中文描述`，如 `feat(broker): 实现 Replay` / `fix(push): 修双重 mailto 前缀`
4. **发现 bug**：先加能复现的测试，再修产品代码（测试驱动回归防护）
5. **新增 API**：同步更新 `api/openapi.yaml` + 给 `broker.Message`/`store.*` 加 json tag（统一 camelCase）

## 8. 踩过的坑（避免重蹈）

### VAPID subject 双重 mailto: 前缀

`webpush-go` 会自动给非 https 的 subscriber 加 `mailto:`，若配置已带则变 `mailto:mailto:` → Apple APNs 返回 `BadJwtToken`。修复：`options()` 里 `TrimPrefix(sub, "mailto:")`。**教训**：库差异要逐字段 diff 实际请求，别过早下"库不兼容"结论。

### BackupEligible flag 未持久化

go-webauthn 登录校验"注册存的 BackupEligible"与"登录返回的"必须一致。saveCredential 漏存这个 flag → 读回恒 false → 真实同步型 Passkey（true）登录报错。**教训**：store 层必测"字段往返一致性"（存什么读什么），不只测 CRUD；虚拟认证器默认行为覆盖不到真实设备的 flag 组合。

### PATCH /v1/devices 不落库

`UpsertDevice` 的 `ON CONFLICT(endpoint)` 只更新订阅密钥，不更新 name/enabled/status_filter/tags → 路由功能生产失效。修复：新增 `UpdateDevice`（按 id 全字段 UPDATE）。**教训**：Upsert 与 Update 语义要分离——Upsert 是"订阅刷新"（只更新密钥），Update 是"用户改配置"。

### 空列表返回 null 被误判为未连接

Go nil slice 序列化成 `null`，前端 `Array.isArray(null)` 为 false → 误判"后端未连接"显示演示数据。修复：空列表返回 `[]`。**教训**：API 契约统一返回数组（哪怕空），别让前端据类型判断连接状态。

### store import 循环

`store/messages.go` 引用 `broker.Message`，而 `broker/sqlite.go` 引用 `store.DB` → 循环。修复：store 定义本地 `MessageRow`，broker 做适配。**教训**：store 是底层，不应依赖上层 broker 的类型。

### 幂等列迁移

`CREATE TABLE IF NOT EXISTS` 不给已存在的表加新列。新增字段（如 `passkeys.backup_eligible`）要在 `store.Open` 里显式 `ALTER TABLE ADD COLUMN`（幂等，重复执行不报错）。

### 改了 web/ 源码后跑 make e2e 用的是旧产物（run_all.sh 脏检测）

`scripts/e2e/run_all.sh` 只在 `internal/server/dist` 不存在或 `web/partials.js` 缺失时才跑 `make fe`，否则直接用现有 dist 编译二进制。改了 `web/`（partials.js/sw.js/tokens.css 等）或 `web-src/`（layouts/pages/locales，sitegen 会重新渲染进 `web/*.html` + i18n JS）后直接 `make e2e`，跑的是陈旧 embed，表现为“改的逻辑像没生效、新断言莫名挂、旧行为还在”。修复：跑门禁前先 `rm -rf internal/server/dist`（让 run_all.sh 强制重建），或先 `make fe` 再 `make e2e`。验证产物链路是否新：`grep -c <新特征字符串> internal/server/dist/*` 应 >0。**教训**：改任何会被 embed 的前端源后，跑门禁前必须确认 dist 产物是新的，别信 run_all.sh 的“已存在就跳过”；这是源→指纹→embed 三段链路里最易断的一环。

### `auto_vacuum=0`：删了不等于变小，且「开保留期」不等于「跟得上写入」

#229 两条一起踩过：

1. **删了不还盘**：`PRAGMA auto_vacuum = 0`（本仓 DSN 没设），DELETEd 的页只进 freelist。
   实测 26,000 行全删掉，文件字节一个不变（5,185,536 → 5,185,536），`freelist` 从 0 涨到 1178；
   再灌同样多的行只多花 1.6%（freelist 被复用）。**所以「稳态」是页数到顶不再涨，不是文件变小。**
   运维看「磁盘没变小」会以为保留期没生效，文档/告警里必须写清。
2. **单轮预算 × 轮次必须 ≥ 写入量**：旧配置 5 批×1000 行、每小时一轮 = 12 万行/天，
   而 30 ev/s 的写入（一条 ingest 落 **2 行** events：本体 + `pruned.*` 剪枝留痕）是 518 万行/天
   —— **差 43 倍，保留期开着也永远删不完**。修法：`DefaultRetentionBatches` 5→20 + 上一轮
   `truncated` 时把下一轮周期从 1 小时切成 30s（`lease.DefaultRetentionCatchUpInterval`）。
   注意每批仍是独立的 1000 行短事务，写锁持有量不变；放大的是「单轮几批」。
   **记住**：`LIMIT` 只限删除行数、不限扫描/排序行数，单批成本仍 ∝ 该范围已过期行数。
   实测单轮 20,000 行 / 242ms。测量与容量测算见 `docs/perf/capacity-and-backpressure.md`。

### 背压信号：不要用 pending 行数，也不要过滤 `not_before`

#228：pending 行数随命名空间数/能力数/流量口径漂移，跨部署不可比 → 用
`now - MIN(created_at) WHERE status='pending'`。**但别顺手加 `not_before` 过滤**：
`SetNotBefore` 是「重派退避」，而写它的正是 `ErrNoCapacity`（派不出去）那条路径
（`internal/server/mux.go` 的 `Redispatch`）—— 加了等于把要治的场景整个漏掉。
另：这条查询每采样周期都跑，必须配部分索引
`idx_tasks_pending_created_at ON tasks(created_at) WHERE status='pending'`（迁移 17），
否则积压越多它越贵 —— 正好在最需要它的时候最贵。
再另：判定要在**请求路径之外**（后台降采样 0.2 次/秒 + `atomic` 读），否则等于把 #242
刚优化掉的 per-request 开销又加回来。

### 保护必须有出口：孤儿 pending 会让背压永久拒绝

#228 复审才发现的一条：信号是 `MIN(created_at) WHERE status='pending'`，而库里一旦有
**永远派不出去**的 pending（能力升级后无人声明该版本 / 节点下线后无人声明 / 任务建出来后
再没有能跑它的节点），`RedispatchTask` 只会一直返回 false 且**全仓没有 abandon 路径** ——
于是一条孤儿任务就能把整个部署的新事件入口钉死在 503 上，永不恢复。
修法：连续超阈超过安全阀窗口（默认 30 分钟）就 **fail-open**（停止拒绝 + `ERROR
server.backpressure.stuck`），信号回落自动复位且**重新具备**拒绝能力。
**教训**：任何「长期生效的保护」都要有「保护本身坏掉时怎么退出去」的设计，
且退出方向要选**保持可用**那一侧（整站写不进来比队列继续涨更糟）。

## 9. 常用命令速查

```bash
make build           # 构建单二进制（指纹 + embed）
make dev             # 开发模式（web/ 本地目录）
make test            # Go 单测（go test ./...；默认不嵌入 dist）
make ci              # PR 门禁：gofmt + fe + vet + build + 单测（不含 e2e）
make e2e             # 全量端到端（固化门禁）
make e2e-one S=routing  # 单套件
make docker          # 构建 Docker 镜像
make up              # compose 部署（先 cp .env.compose.example .env 填好密钥/域名）
make down            # compose 停止（数据卷保留）
make keys            # 生成 VAPID 密钥
make tunnel          # Cloudflare 临时隧道（公网暴露，iOS 验证用）

go run ./cmd/devseed -db ./anotify.db -username demo  # 播种测试用户
```

## 10. 环境变量

见 `README.md` 的环境变量表。关键：`ANOTIFY_VAPID_*_KEY`（推送必需）、`ANOTIFY_RP_ID`/`ANOTIFY_RP_ORIGIN`（Passkey，必须=访问域名）、`ANOTIFY_STATIC`（空=embed，`./web`=开发）。

容量相关（#229 / #228，详见 `docs/perf/capacity-and-backpressure.md`）：

| 变量 | 默认 | 语义 |
| --- | --- | --- |
| `ANOTIFY_EVENT_RETENTION_DAYS` | `30` | events 保留天数；**`0` = 永久保留**（启动日志 `server.retention.permanent` 会 WARN：磁盘随写入线性增长） |
| `ANOTIFY_PENDING_MAX_AGE_SEC` | `120` | 最老 pending 任务等待秒数**超过**它 → 三个新事件入口（`POST /v1/events`、`POST /v1/hooks/{token}`、`POST /v1/sources/{id}/fire`）返回 `503` + `Retry-After`。**`0` = 不启用**（小部署别被自己的保护坑到）。超阈持续 30 分钟会 fail-open（孤儿 pending，见 §8） |
