# 本地开发环境（docker compose + Cloudflare 隧道）

本文对应 `docker-compose.dev.yml`，目标是**一条命令起一个可用的开发实例**，
并可选地通过 Cloudflare 命名隧道暴露到 `anotify-dev.openaaas.org`。

与仓库根的 `docker-compose.yml` 的区别：

| | `docker-compose.yml`（部署） | `docker-compose.dev.yml`（开发） |
|---|---|---|
| 镜像来源 | 拉 GHCR 官方镜像 | **从源码构建** |
| 前端 | 二进制内嵌 | 二进制内嵌（构建时 sitegen + 指纹） |
| 播种工具 | 无 | **有**（`build` 阶段产出 `/devseed`，不进运行镜像） |
| 隧道 | 无 | **有**（cloudflared sidecar，可选 profile） |
| 监听 | `0.0.0.0:8080` | **`127.0.0.1:8080`**（默认只回环，避免抢端口） |
| pprof | 关 | **开**（`ANOTIFY_PPROF=1`） |
| 数据卷 | `anotify-data` | `anotify-dev-data`（互不影响） |

> 本机已经有一个 dogfood 实例（容器 `anotify`，端口 **18080**）。开发环境用 8080 + 独立卷，两者不冲突。

## 1. 快速开始

```bash
make dev-env     # 生成 .env.dev（含真实 VAPID 密钥；已 gitignore）
make dev-up      # 源码构建 + 启动（首次构建几分钟）
make dev-seed    # 播种 demo 用户（幂等），打印 SEND_KEY / RECV_KEY / SESSION
```

打开 <http://localhost:8080>，会跳到登录页。**本地用 localhost 时 Passkey 不可用**（RP_ID 配的是隧道域名），
两种进入方式：

- 用 `make dev-seed` 打印的 `SESSION=sess_...`，在浏览器 DevTools 里设 Cookie
  `anotify_session=<session 值>`（Domain: `localhost`），刷新即登录；
- 或者走隧道域名（见第 3 节），Passkey 就能正常注册/登录。

想要一份"真跑过"的示例数据（能力图 + 节点 WS 上线 + 事件派发 + 任务执行）：

```bash
make dev-demo    # 会打印同样的一组 key/session，并跑完 7 条事件 / 14 个任务
```

## 2. 常用命令

```bash
make dev-logs            # 跟日志（S=服务名，默认 anotify）
make dev-sh              # 进容器（alpine，只有 server 二进制，调试用）
make dev-down            # 停止（数据卷保留）
make dev-reset           # 停止并删卷（从零开始）
make dev-tunnel-quick    # 临时隧道（随机 trycloudflare.com 域名，验证隧道链路）
```

改端口/绑定地址：编辑 `.env.dev` 的 `ANOTIFY_DEV_PORT` / `ANOTIFY_DEV_BIND`（后者设 `0.0.0.0` 可局域网直连）。

## 3. 固定域名：`anotify-dev.openaaas.org`

### 本机已配置好（2026-11-01）

| 项 | 值 |
|---|---|
| 隧道名 | `anotify-dev` |
| 隧道 ID | `7e6fdf7b-e8dc-4214-a1f4-6d14f7fbc65a` |
| DNS | `anotify-dev.openaaas.org` CNAME → `<隧道ID>.cfargotunnel.com`（Cloudflare 自动建） |
| 凭据 | `~/.cloudflared/7e6fdf7b-e8dc-4214-a1f4-6d14f7fbc65a.json`（400，勿外传） |
| 本地 ingress | `.dev-tunnel/config.yml`（不入库）：`anotify-dev.openaaas.org` → `http://anotify:8080` |
| 启动 | `make dev-up TUNNEL=1` |

实测（公网）：

```
GET https://anotify-dev.openaaas.org/health  → {"boot":"73de3f9a4a94730d","ok":true}
GET https://anotify-dev.openaaas.org/        → 200, 47,793 B（gzip 12,075 B）
POST /v1/auth/register/options               → rp.id = anotify-dev.openaaas.org  ✅ Passkey 可用
```

### 换机器 / 重建隧道

```bash
cloudflared tunnel login                     # 浏览器授权，产出 ~/.cloudflared/cert.pem
make dev-tunnel-setup                        # 建隧道 + 配 DNS + 写 .dev-tunnel/config.yml
make dev-up TUNNEL=1
```

`make dev-tunnel-setup` 默认 `TUNNEL=anotify-dev DOMAIN=anotify-dev.openaaas.org`，可覆盖。

### 两种隧道模式

| 模式 | 配置文件 | 启动 | ingress 位置 |
|---|---|---|---|
| **A 凭据 + 本地 ingress**（默认，上面这套） | `.dev-tunnel/config.yml` | `make dev-up TUNNEL=1` | 本地文件 |
| B token（Dashboard 建的隧道） | `.env.dev` 的 `CLOUDFLARE_TUNNEL_TOKEN` | `docker compose --env-file .env.dev -f docker-compose.dev.yml --profile tunnel-token up -d` | Dashboard |

A 模式的两个注意点（都是踩过的坑）：

1. **凭据挂载要挂文件，不能把整个目录只读挂进来再往里叠文件**——只读目录里建挂载点会失败
   （`make mountpoint ... read-only file system`）。所以 compose 里是两条独立的文件挂载。
2. **cloudflared 官方镜像以 nonroot 运行，而隧道凭据权限是 400**（属主是宿主机用户）→ 容器里读不了。
   本地开发用 `user: "0:0"` 跑这个容器最省事（它只读挂载、不写任何东西）。

另外 `.env.dev` 里的变量要参与 compose 插值，必须用 `--env-file .env.dev`（`env_file:` 只给容器注入、不参与插值）——
`make` 的 `DEV_COMPOSE` 已经带上了。

### service 必须写 compose 服务名

ingress 的 `service:` 是 `http://anotify:8080`，**不是 `localhost:8080`**——cloudflared 和 anotify
在同一个 docker 网络里，`localhost` 指的是 cloudflared 自己那个容器。写错了就是 502。

### 通过隧道访问时 Passkey 才可用

`.env.dev` 已配：

```bash
ANOTIFY_RP_ID=anotify-dev.openaaas.org
ANOTIFY_RP_ORIGIN=https://anotify-dev.openaaas.org
```

WebAuthn 要求 **RP_ID 与浏览器地址栏域名严格一致**：

- 走 `https://anotify-dev.openaaas.org` → 可注册/登录 Passkey ✅（已用 `register/options` 验证 rp.id 匹配）
- 走 `http://localhost:8080` → RP_ID 不匹配，Passkey 不可用（用 session cookie 进）

**换域名就要同步改这两个值并重启**，否则注册会报 `RP ID 不匹配`。

## 4. 排错

| 症状 | 原因 / 处理 |
|---|---|
| `make dev-up` 卡在 build | 首次要拉 Go/Node 镜像 + 下模块；后续有缓存 |
| 端口 8080 被占 | `ss -ltnp \| grep 8080`；或改 `.env.dev` 的 `ANOTIFY_DEV_PORT` |
| 容器起不来、日志 `permission denied` | 数据卷属主不对：`make dev-reset` 重建卷 |
| `make dev-demo` 报"能力缺失" | 已修（#271）；若仍见，`make dev-demo` 会自动重建工具镜像，确认不是旧镜像 |
| 隧道 502 | Public hostname 的 Service 写了 `localhost:8080`；应写 `http://anotify:8080` |
| 隧道 404 / DNS 不解析 | Public hostname 没配对，或 DNS 记录未生成；回 Dashboard 检查 |
| Passkey 注册报 RP ID 不匹配 | 访问域名与 `ANOTIFY_RP_ID` 不一致（见第 3 节） |
| 改了代码但行为没变 | `docker compose -f docker-compose.dev.yml build --no-cache anotify` |

## 5. 这套环境刻意没做的事

- **不挂源码目录**（不是热重载环境）：改代码要 `make dev-up` 重建镜像。想要热重载用 `make dev-local`
  （宿主机直接跑 `go run`，不走 docker）。
- **不跑 e2e**：e2e 有自己的 harness（`scripts/e2e/`），会自己起进程、管端口，跟这套 compose 无关。
- **不带迁移/运维工具**：只带 `devseed`（播种）。
