# Anotify · Agent 完成即通知平台

**[中文](README.md)** | [English](README.en.md)

Agent 完成任务后上报一条事件，中枢按你定义的链路在节点上执行后续能力
（分析、留证、告警脚本……），全过程在控制台可查：事件 → 任务链 → 日志。
Passkey 无密码登录（也可选用密码登录）、单二进制易自托管。

> 本仓库由原型演进为完整实现。**Web Push 推送域已整体退役**（决策 V4-D7，见
> [`docs/contracts/v4/CONTRACT.md`](docs/contracts/v4/CONTRACT.md)）——本仓库不再把
> 通知推送到手机；「通知」的落点是事件流与任务链，接哪里由你自己的能力脚本决定。
> 行为的唯一权威是**代码 + 测试 + `docs/contracts/`**；文档入口见 [`docs/README.md`](docs/README.md)。

## 📚 文档导航

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| **[DEVELOPMENT.md](DEVELOPMENT.md)** | 开发者 / Agent | 架构决策、开发流程、测试体系、踩坑记录（上手必读） |
| **[AGENTS.md](AGENTS.md)** | 子 Agent | 工作约定、规范、陷阱 |
| **[docs/README.md](docs/README.md)** | 所有人 | 文档入口：权威在哪、哪些文档是快照、改行为的规矩 |
| **[E2E_TESTING.md](E2E_TESTING.md)** | 开发者 | 端到端测试体系（`make e2e` 固化门禁） |
| **[examples/playbooks/](examples/playbooks/)** | 使用者 | 可直接安装的 playbook 示例（事件 → 判定 → 告警/留证），含空环境落地步骤 |

## 架构

```
外部系统 ─HTTP/WS─▶ 中枢 anotify（单二进制）─▶ WS 派发 ─▶ 节点 anode ─▶ 执行能力脚本
                             SQLite
```

- **两个产品**：中枢 `anotify`（`cmd/server`：资源面 + 派发，**不执行能力**）
  与节点 `anode`（`cmd/anode`：`serve` / `install` / `emit`，真正跑能力的机器）
- **单二进制**：Go + `go:embed` 内嵌前端 + SQLite（纯 Go，无 CGO）
- **中枢不执行**：执行只发生在节点（契约机检：中枢侧不得 `os/exec`）
- **路径分离**：`/v1/*` 动态 API（no-store），静态资源走 CDN 缓存分级（哈希文件 immutable）

> v4 的行为与 API 以代码与 `api/` 下五份 spec 为准；v3 的 `cmd/agent` 与 Broker 抽象已退役（不保留兼容层）。

## 快速开始

### 首次准备（每台机器一次）

```bash
# 1. 安装依赖（Go ≥1.25、Node 22、cloudflared、Chrome）
brew install go node@22 cloudflared

# 2. 克隆
git clone git@github.com:zzjcool/anotify.git && cd anotify

# 3. 配置本地环境（.env.local 不入库）
cp .env.example .env.local
# 按需改 ANOTIFY_RP_ID / ANOTIFY_RP_ORIGIN（Passkey 的 RP 必须=访问域名）
```

`.env.local` 关键字段（已预配 dev.openaaas.org）：

```
ANOTIFY_ADDR=:8080
ANOTIFY_STATIC=./web
ANOTIFY_RP_ID=dev.openaaas.org
ANOTIFY_RP_ORIGIN=https://dev.openaaas.org
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

## Docker

方式一：docker compose（推荐，首次配置好后后续一条命令部署）：

```bash
cp .env.compose.example .env   # 填入访问域名（RP_ID / RP_ORIGIN）
make up                        # = docker compose up -d（从 GHCR 拉官方镜像）
make down                      # 停止并移除容器（SQLite 数据卷保留）
```

镜像托管在 GHCR：`ghcr.io/zzjcool/anotify`。推送 main 自动构建 `latest` + `sha-<短hash>`，打 `v*` tag 额外出版本号 tag（CI 见 `.github/workflows/docker.yml`）。本地源码构建部署可用 `make push`（多架构推 GHCR）或把 compose 里的 `image:` 换回 `build: .`。

方式二：直接 docker run：

```bash
make docker                              # 构建中枢镜像（自带节点升级源 /app/anode）
docker run -p 8080:8080 \
  -e ANOTIFY_RP_ID=你的域名 \
  -e ANOTIFY_RP_ORIGIN=https://你的域名 \
  anotify
```

## 节点（anode）

中枢只负责派发，**真正执行能力的是节点** `anode`（独立二进制，跑在你自己的机器上；
它连中枢、拉能力、执行脚本、回报结果/emit）。默认走**一条命令**——中枢「节点」页注册节点后
给出的就是它（页面给的是 `--token <令牌>`，令牌明文只显示一次）：

```bash
curl -fsSL https://<中枢域名>/install-anode.sh | sh -s -- \
  --name node-a --server https://<中枢域名> --token <节点令牌>
```

`--token` 会留在 shell 历史里，长期使用建议换成 0600 文件：`--token-file /root/anode.token`
（两者只能给一个；升级/轮换令牌时同样会自动生效）。

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
`anode install` 只落盘（不 `sudo`、不 `systemctl`）；完整的三个子命令与 flag 见 `docs/anode-cli.md`。

## 公网暴露

见上方「快速开始 → Cloudflare 命名隧道」一节：`make dev` 已内置起 `cloudflared tunnel run anotify`（固定 dev.openaaas.org → localhost:8080）。临时隧道（`cloudflared tunnel --url`）仅用于一次性真机验证，不建议作日常开发（URL 每次变，Passkey 失效）。

## 测试

```bash
make ci                  # 与 CI 相同：gofmt + 前端 embed 产物 + vet + build + 单元测试
make test                # Go 单元测试（独立于 e2e；默认不嵌入 dist）
make e2e                 # 全量端到端测试（~57s，并行，968 断言）
make e2e-one S=auth_flow # 只跑某个套件
make integration         # 集成测试（健康/缓存分级/鉴权矩阵）
node scripts/ws_test.mjs # WS 接收端（需 SEND_KEY）
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
| `POST /v1/notify` | **已删除** —— v4 用 `POST /v1/namespaces/{namespace}/events`（Bearer Key，scope=notify:send） |
| `GET /v1/stream` | **已删除** —— v4 长连接见 `api/agent.yaml` 的节点流；控制台读事件流走 `GET /v1/console/events` |
| `GET/POST /v1/keys` | API Key 管理 |
| `GET /v1/notifications` | **已删除** —— v4 的事件流就是通知历史：`GET /v1/console/events` |

### Agent 上报示例（v4 事件入口）

```bash
curl -X POST https://你的域名/v1/namespaces/{namespace}/events \
  -H "Authorization: Bearer ant_live_..." \
  -H "Content-Type: application/json" \
  -d '{"source":"github","type":"pull_request.opened","idemKey":"pr-1234-opened","payload":{"pr":1234,"repo":"acme/api"}}'
```

> v3 的 `/v1/notify`（`title` / `agentState` / `deviceTags` 载荷）**已删除**：打过去是 404，
> 不是「旧行为还在」。上表里标了「已删除」的扁平端点同理；v3 的标签/级别投递规则只是历史说明。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ANOTIFY_ADDR` | `:8080` | 监听地址 |
| `ANOTIFY_DB` | `./anotify.db` | SQLite 路径 |
| `ANOTIFY_STATIC` | 空(embed) | 本地静态目录（开发用 `./web`） |
| `ANOTIFY_RP_ID` | `localhost` | WebAuthn RP 域名（=访问域名） |
| `ANOTIFY_RP_ORIGIN` | `http://localhost:8080` | WebAuthn Origin（含协议） |
| `ANOTIFY_CDN_PREFIX` | 空 | CDN 前缀（生产静态加速） |

## 文件结构

```
cmd/server/         单二进制入口
cmd/devseed/        测试播种工具
internal/
  store/            SQLite 数据访问 + schema
  auth/             Passkey(WebAuthn)/密码注册 + 密码(argon2id) + API Key(argon2id) + 会话
  api/              事件入口 + 资源路由（见 api/*.yaml）
  ws/               节点长连接与派发
  route/            事件 → 任务的路由与剪枝（共享纯逻辑）
  protocol/         中枢与节点之间的 wire 协议
  playbook/         预案解析/渲染/安装
  yamldoc/          YAML 文档层（单文档约束）
  server/           路由装配 + 静态资源(CDN 缓存分级) + embed
web/                前端（纯静态 HTML+Tailwind CDN+tokens.css）
scripts/hash.mjs    指纹脚本（content-hash + 引用改写 + manifest）
scripts/integration.sh / ws_test.mjs                 测试脚本
```

## 安全

- API Key **只存 argon2id 哈希**，明文仅创建时显示一次，scope 目前只有 `notify:send`（它护的是事件入口 `POST /v1/namespaces/{ns}/events`）
- Passkey(WebAuthn) 无密码；也可注册时选密码，或登录后在安全页设置密码作为第二套登录方式；会话 httpOnly Cookie
- **每个账号至少保留一种登录方式**：删除最后一个 Passkey 需先设置密码，移除密码需先有 Passkey
  - `*.db` / `.env.local` / `.env.dev` 已 gitignore，勿提交
