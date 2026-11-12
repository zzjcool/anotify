# Anotify · Agent 完成即通知平台

**[中文](README.md)** | [English](README.en.md)

Agent 完成任务后，把通知自动推送到你的所有设备（iOS / Mac / PC / Android）。
Passkey 无密码登录（也可选用密码登录）、无需轮询、单二进制易自托管。

> 本仓库由原型（iOS Web Push 机制验证）演进为完整实现。
> 设计稿见 `design/`，技术方案见 `design/tech-scheme.html`。

## 📚 文档导航

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| **[DEVELOPMENT.md](DEVELOPMENT.md)** | 开发者 / Agent | 架构决策、开发流程、测试体系、踩坑记录（上手必读） |
| **[AGENTS.md](AGENTS.md)** | 子 Agent | 工作约定、规范、陷阱 |
| **[E2E_TESTING.md](E2E_TESTING.md)** | 开发者 | 端到端测试体系（`make e2e` 固化门禁） |
| **[IOS_TESTING.md](IOS_TESTING.md)** | 验证者 | iOS 真机推送验证清单（唯一人工环节） |
| `.pi-orchestrator/TASKS.md` | 协调者 | 实施任务台账（历史记录） |

## 架构

```
外部系统 ─HTTP/WS─▶ 中枢 anotify（单二进制）─┬─▶ WS 派发 ─▶ 节点 anode ─▶ 执行能力脚本
                             SQLite         └─▶ Web Push ─▶ 设备
```

- **两个产品**：中枢 `anotify`（`cmd/server`：资源面 + 派发 + 推送，**不执行能力**）
  与节点 `anode`（`cmd/anode`：`serve` / `install` / `emit`，真正跑能力的机器）
- **单二进制**：Go + `go:embed` 内嵌前端 + SQLite（纯 Go，无 CGO）
- **中枢不执行**：执行只发生在节点（契约机检：中枢侧不得 `os/exec`）
- **路径分离**：`/v1/*` 动态 API（no-store），静态资源走 CDN 缓存分级（哈希文件 immutable）

> v4 的设计与 API 以 `docs/design/v4/` 与 `api/` 下五份 spec 为准；v3 的 `cmd/agent`
> 与 Broker 抽象已退役（不保留兼容层）。

## 快速开始

### 首次准备（每台机器一次）

```bash
# 1. 安装依赖（Go ≥1.25、Node 22、cloudflared、Chrome）
brew install go node@22 cloudflared

# 2. 克隆
git clone git@github.com:zzjcool/anotify.git && cd anotify

# 3. 配置本地环境（.env.local 不入库，含密钥）
cp .env.example .env.local
make keys                                   # 生成 VAPID 密钥对
# 把输出的 public/private key 填进 .env.local 的 ANOTIFY_VAPID_PUBLIC_KEY / _PRIVATE_KEY
```

`.env.local` 关键字段（已预配 dev.openaaas.org）：

```
ANOTIFY_ADDR=:8080
ANOTIFY_STATIC=./web
ANOTIFY_RP_ID=dev.openaaas.org
ANOTIFY_RP_ORIGIN=https://dev.openaaas.org
ANOTIFY_VAPID_PUBLIC_KEY=<make keys 生成>
ANOTIFY_VAPID_PRIVATE_KEY=<make keys 生成>
```

### Cloudflare 命名隧道（dev.openaaas.org，可选但推荐）

用固定域名（而非临时 trycloudflare URL）作 WebAuthn RP_ID，Passkey 可重复使用。

```bash
cloudflared tunnel login                       # 浏览器授权（生成 ~/.cloudflared/cert.pem）
cloudflared tunnel create anotify              # 记住返回的 UUID
```

创建 `~/.cloudflared/config.yml`（替换 `<UUID>`）：

```yaml
tunnel: <UUID>
credentials-file: /Users/<你>/.cloudflared/<UUID>.json

ingress:
  - hostname: dev.openaaas.org
    service: http://localhost:8080
  - service: http_status:404
```

绑定 DNS：

```bash
cloudflared tunnel route dns anotify dev.openaaas.org
```

> 不用隧道？跳过本节，`make dev-local` 只起本地 server（RP_ID 默认 localhost）。

### 日常启动

```bash
make dev          # 起 server + cloudflared tunnel，Ctrl-C 一起停
```

这一条命令做了：读 `.env.local` → 确保 `web/*.html` → 检查端口 → 后台起 tunnel → 前台起 `go run ./cmd/server`（用 `web/` 源文件，改前端即时生效）。

启动后：

- **公网**：<https://dev.openaaas.org>
- **本地**：<http://localhost:8080>
- 首页跳登录页 → 注册首个账号：可用 Passkey，也可选「用户名 + 密码」（首个用户自动成管理员）
- 想用密码登录？注册时直接选密码注册，或登录后在「安全与登录」页设置（Passkey 与密码两套模型并存）
- server 日志：`tail -f /tmp/anotify-dev.log`；tunnel 日志：`tail -f /tmp/anotify-tunnel.log`

| 命令 | 用途 |
| --- | --- |
| `make dev` | 开发：server + tunnel（dev.openaaas.org） |
| `make dev-local` | 开发：只起本地，不起 tunnel |
| `make build` | 构建两个产品二进制：`anotify`（内嵌指纹前端）+ `anode`（节点） |
| `make test` | Go 单元测试 |
| `make ci` | PR 门禁：gofmt / vet / build / 单元测试（与 GitHub Actions 相同，不含 e2e） |
| `make e2e` | 全量端到端测试（~57s，968 断言） |
| `make e2e-one S=auth_flow` | 只跑某个套件 |
| `make keys` | 生成 VAPID 密钥对 |

## Docker

方式一：docker compose（推荐，首次配置好后后续一条命令部署）：

```bash
cp .env.compose.example .env   # 填入 make keys 生成的 VAPID 密钥 + 访问域名
make up                        # = docker compose up -d（从 GHCR 拉官方镜像）
make down                      # 停止并移除容器（SQLite 数据卷保留）
```

镜像托管在 GHCR：`ghcr.io/zzjcool/anotify`。推送 main 自动构建 `latest` + `sha-<短hash>`，打 `v*` tag 额外出版本号 tag（CI 见 `.github/workflows/docker.yml`）。本地源码构建部署可用 `make push`（多架构推 GHCR）或把 compose 里的 `image:` 换回 `build: .`。

方式二：直接 docker run：

```bash
make docker                              # 构建中枢镜像（自带节点升级源 /app/anode）
docker run -p 8080:8080 \
  -e ANOTIFY_VAPID_PUBLIC_KEY=$ANOTIFY_VAPID_PUBLIC_KEY \
  -e ANOTIFY_VAPID_PRIVATE_KEY=$ANOTIFY_VAPID_PRIVATE_KEY \
  -e ANOTIFY_RP_ID=你的域名 \
  -e ANOTIFY_RP_ORIGIN=https://你的域名 \
  anotify
```

## 节点（anode）

中枢只负责派发，**真正执行能力的是节点** `anode`（独立二进制，跑在你自己的机器上；
它连中枢、拉能力、执行脚本、回报结果/emit）。默认走**一条命令**——中枢「节点」页注册节点后
给出的就是它：

```bash
curl -fsSL https://<中枢域名>/install-anode.sh | sh -s -- \
  --name node-a --server https://<中枢域名> --token-file /root/anode.token
```

脚本会下载并校验 sha256、用 `anode serve --check` 预检令牌、`anode install` 落盘、
装 systemd 服务（`system` / `user` / `none`）、轮询到 `active`，最后打印摘要；
跑完去控制台「节点」页应该看到它在线。**命令里不用写 `sudo`**（脚本自己从终端提权）。

- 升级 = **重跑同一条命令**（沿用现有 `anode.yaml`，只换二进制 + restart）
- 卸载 = `sh install-anode.sh --uninstall`（默认连配置一起删；`--keep-config` 保留，`--purge` 连数据目录）
- 完整走查（下载优先级、服务模式与提权、预检诊断、安全）见 `docs/anode-install.md`

不用脚本时（自己控制二进制/unit/服务）：

```bash
make anode                                  # 构建 ./anode（带版本信息）
./anode install --name node-a --server wss://<中枢域名> --token-file /root/anode.token
./anode serve --config /etc/anode/anode.yaml --data /var/lib/anode
./anode --version                           # {"version":"<version>+<commit>","commit":"<sha>"}

make docker-anode                           # 节点镜像（容器即一台节点）
```

token 在中枢登记节点后签发（`POST /v1/nodes` → `POST /v1/nodes/{name}/tokens`，明文只返回一次）。
`anode install` 只落盘（不 `sudo`、不 `systemctl`）；完整的三个子命令与 flag 见 `docs/v4/anode-cli.md`。

## 公网暴露

见上方「快速开始 → Cloudflare 命名隧道」一节：`make dev` 已内置起 `cloudflared tunnel run anotify`（固定 dev.openaaas.org → localhost:8080）。临时隧道（`cloudflared tunnel --url`）仅用于一次性真机验证，不建议作日常开发（URL 每次变，Passkey 失效）。

## 测试

```bash
make ci                  # 与 CI 相同：gofmt + 前端 embed 产物 + vet + build + 单元测试
make test                # Go 单元测试（独立于 e2e；默认不嵌入 dist）
make e2e                 # 全量端到端测试（~57s，并行，968 断言）
make e2e-one S=auth_flow # 只跑某个套件
make integration         # 集成测试（健康/缓存分级/鉴权矩阵）
node scripts/ws_test.mjs # WS 接收端（需 RECV_KEY/SEND_KEY）
node scripts/push_e2e.mjs# 桌面 Chrome 推送 E2E（需 SESSION/API_KEY）
go run ./cmd/devseed     # 播种测试用户 + Key + 会话
```

> e2e 体系详见 `E2E_TESTING.md`（17 套件、并行执行器、结构化 JSON 结果）。

## API（/v1）

> v4 的权威定义是 `api/` 下五份 spec（agent / auth / events / ingress / resource），
> 文档页由 `make sitegen` 生成（访问 `/docs`）。下表是 v3 的扁平端点，部分已随 v4 资源面
> 退役（`/v1/notify`、`/v1/stream`、`/v1/notifications`），仅作历史对照。

| 端点 | 说明 |
| --- | --- |
| `POST /v1/auth/register[/options]` | Passkey 注册 |
| `POST /v1/auth/password/register` | 密码注册（用户名 + 密码） |
| `POST /v1/auth/login[/options]` | Passkey 登录 |
| `POST /v1/auth/password/login` | 密码登录（用户名 + 密码） |
| `GET/POST/DELETE /v1/auth/password` | 密码状态 / 设置 / 修改 / 移除（需会话） |
| `POST /v1/notify` | **Agent 上报**（Bearer Key，scope=notify:send） |
| `GET /v1/stream` | WebSocket 接收（Bearer Key，scope=notify:receive） |
| `GET/POST /v1/devices` | 推送订阅设备管理 |
| `GET/POST /v1/keys` | API Key 管理 |
| `GET /v1/notifications` | 通知历史 |
| `GET /v1/vapid-public-key` | VAPID 公钥（前端订阅用） |

### Agent 上报示例（v3 历史：v4 用 `/v1/namespaces/{ns}/events` + API Key）

```bash
curl -X POST https://你的域名/v1/notify \
  -H "Authorization: Bearer ant_send_..." \
  -H "Content-Type: application/json" \
  -d '{"title":"部署完成","agentState":"done","body":"构建成功","deviceTags":["运维"]}'
```

投递规则：设备 enabled ∧ event_scope 过滤通过（final=仅终态 done/interrupted/error；all=全生命周期）∧ 标签匹配（无 tag 消息=广播；无 tag 设备=catch-all；否则取交集）。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ANOTIFY_ADDR` | `:8080` | 监听地址 |
| `ANOTIFY_DB` | `./anotify.db` | SQLite 路径 |
| `ANOTIFY_STATIC` | 空(embed) | 本地静态目录（开发用 `./web`） |
| `ANOTIFY_VAPID_PUBLIC_KEY/PRIVATE_KEY` | — | VAPID 密钥（推送必需） |
| `ANOTIFY_RP_ID` | `localhost` | WebAuthn RP 域名（=访问域名） |
| `ANOTIFY_RP_ORIGIN` | `http://localhost:8080` | WebAuthn Origin（含协议） |
| `ANOTIFY_CDN_PREFIX` | 空 | CDN 前缀（生产静态加速） |

## 文件结构

```
cmd/server/         单二进制入口
cmd/devseed/        测试播种工具
internal/
  store/            SQLite 数据访问 + schema
  broker/           消息队列抽象 + SQLiteBroker
  auth/             Passkey(WebAuthn)/密码注册 + 密码(argon2id) + API Key(argon2id) + 会话
  api/              /v1/notify 上报
  ws/               WebSocket 派发器
  push/             Web Push 派发器(VAPID)
  route/            标签/status 投递过滤（共享纯逻辑）
  server/           路由装配 + 静态资源(CDN 缓存分级) + embed
web/                前端（纯静态 HTML+Tailwind CDN+tokens.css）
scripts/hash.mjs    指纹脚本（content-hash + 引用改写 + manifest）
scripts/integration.sh / ws_test.mjs / push_e2e.mjs   测试脚本
design/             设计稿 + 技术方案
```

## 安全

- API Key **只存 argon2id 哈希**，明文仅创建时显示一次，带 scope（notify:send / notify:receive / devices:read）
- Passkey(WebAuthn) 无密码；也可注册时选密码，或登录后在安全页设置密码作为第二套登录方式；会话 httpOnly Cookie
- **每个账号至少保留一种登录方式**：删除最后一个 Passkey 需先设置密码，移除密码需先有 Passkey
- 推送载荷端到端加密（p256dh/auth），VAPID 私钥只在服务端
- `vapid.json` / `*.db` / `.env.local` 已 gitignore，勿提交
