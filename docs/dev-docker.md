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

### 为什么必须是"命名隧道"

`cloudflared tunnel --url http://localhost:8080` 是**临时隧道**：域名随机（`xxx.trycloudflare.com`）、
重启就变，而且**不能绑自定义域名**。要固定域名必须用**命名隧道**（有 ID + token，域名路由存在 Cloudflare 侧）。

### 步骤（一次性）

1. **建隧道**
   Cloudflare Dashboard → Zero Trust → Networks → Tunnels → **Create a tunnel**（类型 Cloudflared）→ 起名如 `anotify-dev`。

2. **配 Public hostname**（在同一页的 Public Hostname 标签）
   - Subdomain: `anotify-dev`
   - Domain: `openaaas.org`
   - Path: 留空
   - Service: **Type `HTTP`，URL `anotify:8080`**

   > 这里写的是 **compose 里的服务名 `anotify`**，不是 `localhost`——cloudflared 和 anotify 在同一个
   > docker 网络里，`localhost` 指的是 cloudflared 自己那个容器。
   >
   > DNS 记录由 Cloudflare 自动创建（`anotify-dev.openaaas.org` CNAME 到 `<tunnel-id>.cfargotunnel.com`），
   > 不需要手工加。

3. **把 token 写进 `.env.dev`**

   ```bash
   CLOUDFLARE_TUNNEL_TOKEN=eyJhIjoi...
   ```

4. **带隧道启动**

   ```bash
   make dev-up TUNNEL=1
   ```

   起来后 `https://anotify-dev.openaaas.org/health` 应返回 `{"boot":"...","ok":true}`。

### 通过隧道访问时 Passkey 才可用

`.env.dev` 里已经配好：

```bash
ANOTIFY_RP_ID=anotify-dev.openaaas.org
ANOTIFY_RP_ORIGIN=https://anotify-dev.openaaas.org
```

WebAuthn 要求 **RP_ID 与浏览器地址栏域名严格一致**，所以：

- 走 `https://anotify-dev.openaaas.org` → 可注册/登录 Passkey ✅
- 走 `http://localhost:8080` → RP_ID 不匹配，Passkey 不可用（用 session cookie 进）

**改了域名就要同步改这两个值并重启**，否则注册会报 `RP ID 不匹配`。

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
