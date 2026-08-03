# Anotify · Agent 完成即通知平台

Agent 完成任务后，把通知自动推送到你的所有设备（iOS / Mac / PC / Android）。
Passkey 无密码登录、无需轮询、单二进制易自托管。

> 本仓库由原型（iOS Web Push 机制验证）演进为完整实现。
> 设计稿见 `design/`，技术方案见 `design/tech-scheme.html`。

## 架构

```
Agent ─POST /v1/notify─▶ Go 后端 ─▶ Broker(SQLite) ─┬─▶ WS 派发器(消费者1) ─▶ WebSocket 客户端
 (ant_send Key)        单二进制      队列+历史       └─▶ Push 派发器(消费者2) ─▶ Web Push(APNs/FCM)
```

- **单二进制**：Go + `go:embed` 内嵌前端 + SQLite（纯 Go，无 CGO）
- **Broker 抽象**：消息队列接口（Publish/Subscribe/Ack/Replay），前期 SQLite 实现，未来可换 NATS/Redis/Kafka 零业务改动
- **两条接收通道 = 两个消费者**：WebSocket 长连接 + Web Push
- **路径分离**：`/v1/*` 动态 API（no-store），静态资源走 CDN 缓存分级（哈希文件 immutable）

## 快速开始

```bash
# 1. 生成 VAPID 密钥（首次）
make keys        # 输出 ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY

# 2. 构建单二进制（含前端指纹 + embed）
make build

# 3. 配置环境并运行
export ANOTIFY_VAPID_PUBLIC_KEY=... ANOTIFY_VAPID_PRIVATE_KEY=...
./anotify        # 默认 :8080，embed 前端

# 开发模式（前端改不动二进制）
make dev         # 用 web/ 本地目录，不指纹
```

## Docker

```bash
make docker                              # 构建镜像（~20MB）
docker run -p 8080:8080 \
  -e ANOTIFY_VAPID_PUBLIC_KEY=$ANOTIFY_VAPID_PUBLIC_KEY \
  -e ANOTIFY_VAPID_PRIVATE_KEY=$ANOTIFY_VAPID_PRIVATE_KEY \
  -e ANOTIFY_RP_ID=你的域名 \
  -e ANOTIFY_RP_ORIGIN=https://你的域名 \
  anotify
```

## 公网暴露（Cloudflare 临时隧道，供 iOS/真机验证）

```bash
# 服务在 :8080 运行后，用隧道域名作为 RP_ID 重启（Passkey 需要域名匹配）
cloudflared tunnel --url http://localhost:8080
# → 得到 https://xxx.trycloudflare.com，用它作为 ANOTIFY_RP_ID / ANOTIFY_RP_ORIGIN 重启服务
```

## 测试

```bash
make test                # 全部单元测试
make integration         # 集成测试（健康/缓存分级/鉴权矩阵）
node scripts/ws_test.mjs # WS 接收端（需 RECV_KEY/SEND_KEY）
node scripts/push_e2e.mjs# 桌面 Chrome 推送 E2E（需 SESSION/API_KEY）
go run ./cmd/devseed     # 播种测试用户 + Key + 会话
```

## API（/v1，详见 api/openapi.yaml）

| 端点 | 说明 |
| --- | --- |
| `POST /v1/auth/register[/options]` | Passkey 注册 |
| `POST /v1/auth/login[/options]` | Passkey 登录 |
| `POST /v1/notify` | **Agent 上报**（Bearer Key，scope=notify:send） |
| `GET /v1/stream` | WebSocket 接收（Bearer Key，scope=notify:receive） |
| `GET/POST /v1/devices` | 推送订阅设备管理 |
| `GET/POST /v1/keys` | API Key 管理 |
| `GET /v1/notifications` | 通知历史 |
| `GET /v1/vapid-public-key` | VAPID 公钥（前端订阅用） |

### Agent 上报示例

```bash
curl -X POST https://你的域名/v1/notify \
  -H "Authorization: Bearer ant_send_..." \
  -H "Content-Type: application/json" \
  -d '{"title":"部署完成","status":"success","body":"构建成功","deviceTags":["运维"]}'
```

投递规则：设备 enabled ∧ status 过滤通过 ∧ 标签匹配（无 tag 消息=广播；无 tag 设备=catch-all；否则取交集）。

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
  auth/             Passkey(WebAuthn) + API Key(argon2id) + 会话
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
- Passkey(WebAuthn) 无密码；会话 httpOnly Cookie
- 推送载荷端到端加密（p256dh/auth），VAPID 私钥只在服务端
- `vapid.json` / `*.db` / `.env.local` 已 gitignore，勿提交
