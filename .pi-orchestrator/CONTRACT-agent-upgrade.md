# 冻结契约 · agent 自升级（CLI + 控制台下发）

> 唯一事实源。三组实现必须与此逐字一致；不一致先停下问协调者。日期 2026-11-05。

## 0. 为什么

现在升级 agent 的方式是「重新跑一遍 install-agent.sh」（要 GitHub raw + sudo）。问题：
- 离线/内网机器不行；
- 二进制版本没有任何可观测性（`nodes` 表里不知道对面跑的是哪一版）；
- 控制台看不到「这台机器的 agent 过期了」。

目标：**中枢自己带 agent 二进制并作为升级源**，两个触发点——
`anotify-agent upgrade`（CLI）与「控制台点一下」（WS 下发）。

## 1. 版本

新增包 `internal/version`：

```go
var (
	Version = "dev"      // 语义化版本或 dev；-ldflags 注入
	Commit  = ""         // git short sha
	BuiltAt = ""         // RFC3339
)
func String() string   // "1.5.0+3fd5a59" / "dev"
```

Makefile 给 **server 与 agent 两个二进制**都注入：

```
-X github.com/zzjcool/anotify/internal/version.Version=$(VERSION) -X ...Commit=$(GIT_SHA) -X ...BuiltAt=...
```

`VERSION` 缺省取 `git describe --tags --always --dirty`（现在 Makefile 已有 VERSION 变量，沿用）。

## 2. 中枢提供二进制

- 服务端启动时读 `ANOTIFY_AGENT_BIN`（默认 `/app/agent`；文件不存在 = 该功能**未启用**，不是错误）。
- 启动时把文件读进内存，算 `sha256`，日志打 `agent.binary.ready {version, sha256, bytes}`。
- `GET /v1/node/binary`（**node token 鉴权**，不是会话）→ 200 返回二进制原文：
  - `Content-Type: application/octet-stream`
  - `X-Agent-Version: <version.String()>`
  - `X-Agent-Sha256: <hex>`
  - 未启用 → **501** `{"error":"agent_binary_unavailable"}`（明确说「服务端没带 agent 二进制」）
- Dockerfile 在构建阶段 `go build ./cmd/agent`，把产物放到镜像 `/app/agent`（dev 与生产同一份）。

## 3. hello 报版本

`hello` 帧新增 `agent_version`（string，可空）。中枢落库到 `nodes.agent_version`（+`nodes.agent_commit`，都 `TEXT NOT NULL DEFAULT ''`，**`+migration 3`**）。控制台 `GET /v1/nodes` 每台带 `agentVersion` / `agentCommit`（camelCase）。老 agent 不带 = 空串，不报错。

## 4. CLI

```
anotify-agent upgrade [--check] [--force] [--server URL] [--install-to PATH] [--restart]
```

流程：
1. 读 `-f` 指定的 node.yaml（与主命令一致），拿 `server` + `token`。
2. `GET {server}/v1/node/binary`（`Authorization: Bearer <node token>`）→ 拿字节 + `X-Agent-Version` + `X-Agent-Sha256`。
3. `--check`：只打印「当前 X → 目标 Y（sha 前 12 位）」，退出码 0（有更新）/ 1（已最新）/ 2（服务端未启用）。
4. 目标已同 sha256 → `already up to date`，退出 0（`--force` 时继续覆盖）。
5. 目标路径：`--install-to` > 当前运行的可执行（`os.Executable()` 解析 symlink）> `~/.anotify/bin/anotify-agent`（前者不可写时的回落，打印提示）。
6. 下载到**目标同目录**的临时文件（`.anotify-agent.tmp-*`）→ 校验 sha256 **不符即删临时文件并失败** → `chmod 0755` → `rename` 原子替换。
   - 同目录是为了 rename 不跨设备（`/tmp` 与 `/usr/local/bin` 常常不是同一挂载）。
7. 不可写（EACCES/EPERM）：不静默降级，报清楚「需要 root」，并打印可直接粘的
   `sudo install -m 0755 <临时或下载路径> <目标>`。退出码 3。
8. 替换成功后要**让它立刻生效**（换文件 ≠ 换进程），三种情形：
   - 默认：扫描 `/proc/*/exe` 找到「exe 指向目标路径 + 由 systemd 服务单元管 + 不是自己或祖先」
     的 agent，给它发 **SIGTERM**（它自己会优雅收尾），systemd `Restart=always` 拉起新版本。
     **不需要 root** —— 让升级这件日常事每次都要 sudo 是把设计问题转嫁给用户。
   - `--restart`：`systemctl restart anotify-agent`（system 级单元需要 root；失败只告警）
   - `--no-restart`：只换文件，下次启动生效
   - 三种情形的失败都**不改退出码**（二进制已经换好了）。

退出码：`0` 成功/已最新 · `1` 已最新（仅 `--check`）· `2` 服务端未启用 · `3` 权限不足 · `4` 校验失败 · `5` 网络/其他错误。

## 5. 控制台下发

端点 `POST /v1/nodes/{id}/upgrade`（会话鉴权，命名空间隔离）：
- 节点离线 → 409 `node_offline`
- 服务端没有 agent 二进制 → 501 `agent_binary_unavailable`
- 节点当前 `agent_version == 目标` 且未 `force` → 200 `{"status":"up_to_date"}`
- 否则向该节点连接推：

```json
{ "type": "upgrade",
  "target_version": "1.5.0+3fd5a59",
  "sha256": "…64 hex…",
  "url_path": "/v1/node/binary" }
```

- 节点回：

```json
{ "type": "upgrade_ack", "ok": true, "version": "1.5.0+3fd5a59", "error": "" }
```

失败时 `ok:false` + `error`，且错误码可判别：
`bad_sha`（校验失败）/ `need_root`（无权限写目标）/ `download_failed` / `not_supported`（本平台不支持自替换，如容器内只读 FS）。

- agent 成功替换后：先回 `upgrade_ack`，再 **`syscall.Exit(0)`**（systemd `Restart=always` 会拉起新版本）。`need_root` 时**不退出**（否则节点直接掉线，且用户拿不到那条提示）。
- 中枢收到 ack：更新 `nodes.agent_version/agent_commit`（成功时用 ack 里的 version），并把结果写进响应（`{"status":"sent"}` → 后续靠 ack 更新；控制台轮询节点列表即可看到新版本）。
- 下载时 agent 用**自己的** node token 请求 `url_path`（不是把二进制塞进 WS 帧——15 MB 的帧会撑爆心跳与背压）。

## 5.1 控制台读侧（**冻结**：C 组提的缺口，B 组必须实现）

`GET /v1/nodes` 的响应：

```json
{
  "nodes": [ { "id": "nd_x", "agentVersion": "1.5.0+3fd5a59", "agentCommit": "3fd5a59",
               "agentUpgrade": { "status": "ok", "targetVersion": "1.5.0+3fd5a59",
                                 "error": "", "at": 1762329600 } } ],
  "targetAgentVersion": "1.5.0+3fd5a59"
}
```

- `targetAgentVersion`（**顶层**，string）：中枢当前带的 agent 二进制版本；空串 = 服务端没带二进制（此时 UI 不显示升级按钮，或显示「服务端未提供」）。
- 每台节点 `agentUpgrade`（**对象**，恒存在，默认 `{"status":"","targetVersion":"","error":"","at":0}`）：
  - `status`：`""`（从未尝试）/ `sent`（已下发，等 ack）/ `ok`（ack 成功）/ `failed`（ack 失败）
  - `error`：失败码，与 §5 的 agent 侧一致（`need_root` / `bad_sha` / `download_failed` / `not_supported`）
  - `targetVersion`：这次尝试的目标版本；`at`：`sent`/`ok`/`failed` 的时刻（unix 秒）
- 落库：`nodes.agent_upgrade TEXT NOT NULL DEFAULT '{}'`（JSON），**在 `+migration 3` 里和 agent_version/agent_commit 一起加**。`sent` 由端点写入，`ok`/`failed` 由 `upgrade_ack` 覆盖。
- `POST /v1/nodes/{id}/upgrade` 的响应：`{"status":"sent"|"up_to_date", "targetVersion":"…"}`（失败仍走 409/501）。

这组字段是**冻结契约的一部分**，C 组的别名降级（`agentTargetVersion` / `hubAgentVersion` 等）只作兼容兜底，主路径必须读上面这两个。

## 6. 控制台 UI

节点列表/节点卡片：
- 显示 `agentVersion`（空 = 「未知（老版本）」）；
- 「升级」按钮（二次确认，提示「会替换节点上的二进制并重启 agent；正在跑的任务会被等完（grace 20s）」）；
- 失败弹具体原因：`need_root` 给出「在节点上执行：sudo anotify-agent upgrade」，`bad_sha` 提示「服务端二进制可能被改动，已拒绝」；
- 目标版本 = 中枢当前版本；同版本时按钮显示「已是最新」。

## 7. 分组与文件归属（三组并行）

| 组 | 范围 | 独占文件 |
|---|---|---|
| **A agent 侧** | §1（version 包 + agent 的 ldflags）、§4（upgrade 子命令）、hello 带 `agent_version` 的**发送**侧 | `internal/version/**`、`cmd/agent/upgrade*.go`、`cmd/agent/main.go`、`cmd/agent/client.go`（只加 hello 字段）、`cmd/agent/*_test.go`、`Makefile` |
| **B 中枢侧** | §1（server 的 ldflags 由 A 统一改 Makefile，B 不改）、§2（二进制端点）、§3（列 + 迁移 3 + hello 解析）、§5 + **§5.1 控制台读侧**（WS 帧 / 端点 / ack 处理 / `targetAgentVersion` / `agentUpgrade`） | `internal/store/schema.sql`、`internal/store/node.go`、`internal/ws/*.go`、`internal/server/*.go`、`api/openapi.yaml`、`Dockerfile`、`docker-compose*.yml` |
| **C 控制台与文档** | §6 UI、§5/§4 的文案、docs | `web-src/**`、`web/**`、`docs/contracts/CONTRACT.md`、`docs/design/agent-upgrade.md`、`DEVELOPMENT.md`、`scripts/e2e/suites/frontend.mjs` |

`internal/ws/node_protocol.go` 的 `HelloFrame.AgentVersion` 字段**由 A 加**（一条 `json:"agent_version,omitempty"`），A 同时负责 agent 发送侧；B 只负责**解析 + 落库 + 视图**，不得再改这个 struct。这样两边的分支都能独立编译，不需要 SEAM 注释。

`internal/store/schema.sql` 的 `+migration 3` 只有 B 动（A 不碰 schema）。

## 8. 门禁

各自：`gofmt`/`go vet ./...`/`go build ./...`/`go test ./... -count=1`；B 另加 `make contract`；C 另加 `check-classes`/`check-tailwind-css`/`make fe`/`e2e frontend`。
