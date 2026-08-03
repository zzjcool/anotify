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
2. `statusMatch(device.statusFilter, msg.status)`：all 全过；error 仅 error；success 仅 success；interrupted/info/warning 仅 all 时过
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

### 本地开发

```bash
make dev    # 前端用 web/ 本地目录（改完即生效），服务在 :8080
make build  # 产出单二进制 anotify（含指纹后的前端 embed）
```

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
```

### 测试分层

| 层 | 位置 | 覆盖 |
| --- | --- | --- |
| Go 单测 | `internal/*_test.go` | broker/auth/api/push/ws/store/server 包级逻辑 |
| API 契约 | `scripts/e2e/suites/api_contract.mjs` | 全端点状态码/鉴权/校验 |
| Passkey 认证 | `scripts/e2e/suites/auth_flow.mjs` | **虚拟认证器无头跑注册/登录/登出/会话** |
| WS 协议 | `scripts/e2e/suites/ws_protocol.mjs` | hello/subscribe/ack/replay/心跳/标签过滤 |
| 路由矩阵 | `scripts/e2e/suites/routing.mjs` | 标签 + status 投递全边界 |
| 持久化 | `scripts/e2e/suites/persistence.mjs` | 重启后数据/seq 连续 |
| 安全 | `scripts/e2e/suites/security.mjs` | scope 越权/Key 篡改/哈希不可逆/SQL 注入/路径穿越 |
| 前端 | `scripts/e2e/suites/frontend.mjs` | 6 页渲染 + 路由守卫 + 真实数据 |
| 桌面推送 | `scripts/e2e/suites/push_e2e.mjs` | 真实 FCM 订阅 → 投递 |
| 边界 | `scripts/e2e/suites/edge_cases.mjs` | 并发 seq/Unicode/超大体/畸形输入 |

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
internal/
  store/           SQLite 数据访问 + schema.sql + 幂等列迁移
  broker/          消息队列抽象 + SQLiteBroker
  auth/            Passkey(WebAuthn) + API Key(argon2id) + 会话
  authn/           Key 校验接口（解耦 auth 与 api/ws）
  api/             /v1/notify 上报处理器
  ws/              WebSocket 派发器（消费者1）
  push/            Web Push 派发器（消费者2，VAPID）
  route/           标签/status 投递过滤（共享纯逻辑）
  server/          路由装配 + 静态资源(CDN缓存) + embed
web/               前端（纯静态，无构建框架）
scripts/
  hash.mjs         指纹脚本
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

## 9. 常用命令速查

```bash
make build           # 构建单二进制（指纹 + embed）
make dev             # 开发模式（web/ 本地目录）
make test            # Go 单测
make e2e             # 全量端到端（固化门禁）
make e2e-one S=routing  # 单套件
make docker          # 构建 Docker 镜像
make keys            # 生成 VAPID 密钥
make tunnel          # Cloudflare 临时隧道（公网暴露，iOS 验证用）

go run ./cmd/devseed -db ./anotify.db -username demo  # 播种测试用户
```

## 10. 环境变量

见 `README.md` 的环境变量表。关键：`ANOTIFY_VAPID_*_KEY`（推送必需）、`ANOTIFY_RP_ID`/`ANOTIFY_RP_ORIGIN`（Passkey，必须=访问域名）、`ANOTIFY_STATIC`（空=embed，`./web`=开发）。
