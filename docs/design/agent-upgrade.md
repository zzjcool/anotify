# agent 自升级（中枢当升级源）

> **v3 历史文档**（#338）：v4 的节点产品叫 **anode**（`cmd/anode`，CLI 见 `docs/v4/anode-cli.md`），
> 镜像里的升级源二进制是 `/app/anode`（Dockerfile `COPY --from=build /anode /app/anode` +
> `ENV ANOTIFY_AGENT_BIN=/app/anode`）。下文里的 `agent` / `cmd/agent` / `/app/agent` /
> `anotify-agent upgrade` 都是 v3 名词；接口形状（`GET /v1/node/binary`、
> `X-Agent-Sha256`、退出码词表）仍成立，但具体二进制/路径按 anode 读。
>
> 状态：**设计已定**，实现契约见 `docs/contracts/CONTRACT.md` §2.3 / §6.1 / §6.2 / §7。
> 唯一事实源：`.pi-orchestrator/CONTRACT-agent-upgrade.md`（冻结）。
> 相关：`docs/agent-install.md`（首次安装 + 兜底）、`DEVELOPMENT.md` §4（日常怎么升）。
>
> 本文只写**为什么这么切、线格式怎么走、失败怎么查**；字段与退出码以契约逐字为准。

---

## 0. 为什么：中枢当升级源

现状：升级 agent 的唯一方式是**重新跑一遍 `install-agent.sh`**。三处疼：

| 疼点 | 具体表现 |
|---|---|
| 依赖外网 + sudo | 脚本要从 GitHub raw 拉二进制和同名 `.sha256`，装到 `/usr/local/bin` 还要 sudo。离线 / 内网机器直接没这条路 |
| 版本不可观测 | `nodes` 表里**不知道对面跑的是哪一版**。遇到「老 agent 少个帧」这类问题，只能登上机器看 |
| 控制台看不见 | 控制台没有「这台机器的 agent 过期了」这个事实，升级全靠人记 |

目标：**中枢自己带 agent 二进制，并作为升级源**。触发点两个 ——
`anotify-agent upgrade`（机器上主动跑）与「控制台点一下」（WS 下发指令）。

**最关键的取舍：中枢只提供字节流，不执行安装**（不变量 I1）。下载、校验、替换全部由
agent 自己完成 —— 中枢不 `os/exec`、不碰节点文件系统、不知道节点上的路径长什么样。
这样「中枢能装东西到节点上」这件事从来没成立过，权限边界与今天一致。

---

## 1. 版本与二进制来源

### 1.1 `internal/version`

| 变量 | 含义 | 注入 |
|---|---|---|
| `Version` | 语义化版本或 `dev` | `-ldflags -X` |
| `Commit` | git short sha | 同上 |
| `BuiltAt` | 构建时间（RFC3339） | 同上 |

`version.String()` → `1.5.0+3fd5a59` / `dev`。

Makefile 用同一条 `-X` 同时注入 **server 与 agent 两个二进制**（`VERSION` 缺省取
`git describe --tags --always --dirty`）。两边必须是同一套口径：中枢报的「目标版本」和
节点自报的「当前版本」要能直接比，否则「是不是最新」无从判断。

### 1.2 中枢侧：启动时把二进制读进内存

| 项 | 值 |
|---|---|
| 环境变量 | `ANOTIFY_AGENT_BIN` |
| 默认路径 | `/app/anode`（镜像里已有；v3 为 `/app/agent`） |
| 文件不存在 | 该功能**未启用**，**不是错误**（不 panic、不阻止启动） |
| 启动时 | 读进内存并算 `sha256`，打一行 `agent.binary.ready {version, sha256, bytes}` |

`Dockerfile` 在构建阶段 `go build ./cmd/anode`，产物放到镜像 `/app/anode`，
dev 与生产同一份 —— 不会出现「线上跑的节点二进制和仓库里的不是同一版」。

**为什么先读进内存**：升级请求可能同时来（多节点 / 重试），每次读盘既慢又可能读到
半写状态；启动时一次读定 + 一次算哈希，之后 `sha256` 与字节天然一致，
不会出现「`X-Agent-Sha256` 说的是 A、发出去的字节是 B」。

> **同名不同物**：agent 侧早就有一个 `ANOTIFY_AGENT_BIN` —— 它注入给能力进程，
> 指向 **agent 自己的可执行文件**（`DEVELOPMENT.md` §0.5）。这里是**中枢**的配置项，
> 指向**要下发的那个二进制**。两个进程、两处配置，不共享值；改的时候别改错那一边。

### 1.3 为什么二进制不进 WS 帧

二进制约 15 MB。塞进帧会同时坏三件事：

- **心跳被顶住**：`heartbeat` 10s 一次，大帧排在任何小帧前面，判死逻辑会误触发；
- **撞背压上限**：连接从「控制面」变成「文件传输通道」；
- **没有断点续传**：帧是一次性的，断了就整包重来。

所以分工是：**WS 帧只走指令**（`upgrade`，几十字节），**字节走 HTTP**
（`GET /v1/node/binary`）。agent 用自己的 **node token** 下载 —— 和它连 WS 用的是同一份
凭证，不引入新密钥、不引入新概念。

---

## 2. 三条升级路径

| 路径 | 触发 | 需要什么 | 适用 |
|---|---|---|---|
| CLI `anotify-agent upgrade` | 在节点机器上手动跑（`--check` 只看） | 能连上中枢；对目标路径有写权限 | 运维 / 脚本化 / 排查时先看版本 |
| 控制台「节点」页点「升级」 | 会话鉴权的人点一下（二次确认） | 节点在线 + 中枢带了 agent 二进制 | 日常升一台或几台 |
| 兜底：重跑 `install-agent.sh` | 手动 | GitHub（或镜像）可达 + sudo | **首次安装**、镜像不通、自升级不可用时 |

三者**不是替代关系**，是分层：前两条是主路径（中枢当源），第三条保留为兜底 ——
首次安装本来就没有 agent 可以「自升级」，容器只读 FS 的场景也永远走不通自替换。

**控制台只发指令 + 展示结果**：点「升级」只会让中枢推一条 `upgrade` 帧（§4），
下载 / 校验 / 替换**全部在节点侧**。控制台没有「上传二进制」「代我写入路径」这类能力，
将来也不会有（I1）。

---

## 3. CLI：`anotify-agent upgrade`

```bash
anotify-agent upgrade [--check] [--force] [--server URL] [--install-to PATH] [--restart]
```

| 参数 | 作用 |
|---|---|
| `--check` | 只看：打印「当前 X → 目标 Y」，不下载、不替换 |
| `--force` | 目标 sha256 与当前一致时仍然覆盖 |
| `--server URL` | 覆盖 `node.yaml` 里的 `server` |
| `--install-to PATH` | 覆盖目标路径 |
| `--restart` | 替换成功后执行 `systemctl restart anotify-agent` |

**七步流程**：

1. 读 `-f` 指定的 `node.yaml`（与主命令一致），拿 `server` + `token`；
2. `GET {server}/v1/node/binary`，请求头带 `Authorization: Bearer <node token>` →
   拿到字节 + `X-Agent-Version` + `X-Agent-Sha256`；
3. `--check`：只打印「当前 X → 目标 Y（sha 前 12 位）」，到此结束；
4. 目标 sha256 与当前二进制相同 → `already up to date`，退出 0（`--force` 时继续覆盖）；
5. 定目标路径：`--install-to` > 当前运行的可执行（`os.Executable()` 解析 symlink）>
   `~/.anotify/bin/anotify-agent`（最后这个只是没有前两者时的默认落点）。
   **不可写时绝不静默跳到下一个候选** —— 直接报「需要 root」（§5）；
6. 下载到**目标同目录**的临时文件 `.anotify-agent.tmp-*` → 校验 sha256，
   **不符即删临时文件并失败** → `chmod 0755` → `rename` 原子替换。
   同目录是为了 `rename` 不跨设备（`/tmp` 与 `/usr/local/bin` 常常不是同一个挂载，
   跨设备 `rename` 会退化成「复制 + 删源」，中间那一刻的二进制不完整）；
7. 打印「重启后生效」；带 `--restart` 时跑 `systemctl restart anotify-agent`，
   **失败只告警，退出码仍是 0**（二进制已经换好了，重启是另一件事，不该报复性地报失败）。

### 3.1 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功 / 已最新 |
| `1` | 已最新（仅 `--check`） |
| `2` | 服务端未启用（没带 agent 二进制） |
| `3` | 权限不足（EACCES/EPERM，需要 root） |
| `4` | 校验失败（sha256 不符） |
| `5` | 网络 / 其他错误 |

**`--check` 的 0/1/2**：`0` = 有更新（可升）、`1` = 已最新（没得升）、`2` = 服务端没带
二进制（升不了，先解决中枢侧）。三个都是**正常结论**，脚本可以放心 `if` 它们；
只有 `3`/`4`/`5` 是「这条路走不通」。注意真升级时「已最新」是 `0`，不是 `1` ——
「我成功确认了不用升」和「我升了」对调用方是同一个意思。

---

## 4. 控制台下发的帧序列

```text
控制台           中枢                        节点（anotify-agent）
  │  POST /v1/nodes/{id}/upgrade
  │───────────────▶│
  │                │ ① 校验：离线 → 409 / 无二进制 → 501 / 同版本 → 200 up_to_date
  │                │    （通过则写 agentUpgrade.status=sent 并推帧）
  │                │ ② 推 upgrade 帧
  │                │───────────────▶│
  │                │                │ ③ 用 node token GET /v1/node/binary
  │                │◀───────────────│
  │                │ ④ upgrade_ack
  │                │◀───────────────│
  │                │ ⑤ 换二进制 → syscall.Exit(0) → systemd 拉起新版本
  │  ◀── 200 {"status":"sent","targetVersion":"…"}  │ ⑥ 重连 hello 带 agent_version → 落库 → 列表刷新
```

逐条：

1. 控制台 `POST /v1/nodes/{id}/upgrade`（**会话鉴权** + 命名空间隔离；节点属于谁由
   node token 决定，见契约 §6.2）；
2. 中枢先判三种「不发」：

   | 情况 | 响应 |
   |---|---|
   | 节点离线 | `409` `node_offline` |
   | 服务端没带 agent 二进制 | `501` `agent_binary_unavailable` |
   | `agent_version == 目标`（且未 `force`） | `200` `{"status":"up_to_date","targetVersion":"1.5.0+3fd5a59"}` |

3. 否则把该节点的 `agent_upgrade` 置为 `{"status":"sent","targetVersion":"…","error":"","at":<now>}`（落库，§7），
   并**只推一条指令、没有字节**：

   ```json
   { "type": "upgrade",
     "target_version": "1.5.0+3fd5a59",
     "sha256": "…64 hex…",
     "url_path": "/v1/node/binary" }
   ```

   然后回 `200` `{"status":"sent","targetVersion":"1.5.0+3fd5a59"}`；节点照 `url_path`
   用**自己的 node token** 下载（§1.3）。
4. 节点回：

   ```json
   { "type": "upgrade_ack", "ok": true, "version": "1.5.0+3fd5a59", "error": "" }
   ```

   失败时 `ok:false` + `error` ∈ `bad_sha` / `need_root` / `download_failed` /
   `not_supported`（§6 逐条给处理）；
5. 成功替换后：**先回 ack，再 `syscall.Exit(0)`**，systemd 的 `Restart=always` 拉起新版本
   —— 顺序不能反：先退出就没人回 ack，控制台只能看到「节点掉线」。`need_root` 时
   **不退出**（否则节点直接掉线，用户连那条提示都拿不到）；
6. 节点重连 `hello` 带 `agent_version` → 中枢落库 `nodes.agent_version` /
   `nodes.agent_commit`（`+migration 3`）→ 控制台刷新节点列表即看到新版本。
   中枢收到 ack 时也会用 ack 里的 `version` 更新这两列，并把 `agent_upgrade` 覆盖成
   `ok` / `failed`（§7），两条路径收敛到同一个结果。

### 4.1 `GET /v1/node/binary` 的响应契约

| 情况 | 响应 |
|---|---|
| 正常 | `200` `application/octet-stream` + `X-Agent-Version` + `X-Agent-Sha256`，body 就是二进制 |
| 服务端没带二进制 | `501` `{"error":"agent_binary_unavailable"}` |
| 鉴权 | **node token**（`Authorization: Bearer <node token>`），**不是会话** |

鉴权用 node token 而不是会话，理由有两层：一是「能下载二进制」应该与「能连中枢」同权
（下载到的二进制本来就要用这份 token 去连），二是这条路径要在无人值守的机器上跑，
那里没有、也不该有人的会话。

---

## 5. 权限模型：什么时候 `need_root`

agent 以**普通用户**跑（systemd 单元里 `User=<你>`；「装成系统服务」≠「以 root 运行」），
而目标路径默认是 `~/.anotify/bin/anotify-agent`（安装脚本的缺省前缀，**可写**）；
若历史上装到了 `/usr/local/bin`（root 所有），就只能报 `need_root` 让人上去 sudo ——
`--prefix=/usr/local` 的代价就在这里。

| 现象 | 判定 | CLI | WS ack |
|---|---|---|---|
| 写目标目录 EACCES/EPERM | 就是没权限，不是 bug | 退出码 `3` | `error:"need_root"` |

**不静默降级到 `~/.anotify/bin`**：静默换个地方写，等于「升级成功」了，但 systemd 单元
里 `ExecStart` 指的仍是老路径 —— 机器上多了一份新二进制、进程跑的还是老的，
用户完全看不出来（这正是本项目反复要消灭的那类静默失败）。所以：

- CLI 打印可直接粘的命令：`sudo install -m 0755 <下载路径> <目标>`；
- 控制台提示「在节点上执行：sudo anotify-agent upgrade」。

### 5.1 systemd 单元的两种位置

| 单元位置 | 管理方式 | 说明 |
|---|---|---|
| 系统单元 `/etc/systemd/system/anotify-agent.service` | `systemctl` | 需要 root 注册；`--restart` 也可能需要权限（`systemctl restart` 本身要 root） |
| 用户单元 `~/.config/systemd/user/anotify-agent.service` | `systemctl --user` | 不需要 root，但「没登录时还在不在」取决于 `loginctl enable-linger` |

**两者都装会互相打架** —— 同名的两份 agent 抢同一个令牌，表现成两台机器互相踢线
（`docs/agent-install.md` §常见问题已记录：先 `systemctl stop` 再重装）。

### 5.2 `not_supported`

容器内只读 FS、或平台本身不支持自替换（运行中的二进制无法被原子替换的场合）。
这种情况下 agent **照实回 `not_supported`**，而不是尝试了再报一个模糊的 IO 错误 ——
用户要能一眼看出「这条路在我这台机器上走不通，去重跑安装脚本」。

### 5.3 重申

**`need_root` 时 agent 不退出。** 退出等于把「升级失败」变成「节点掉线」：
控制台失去这台机器的所有信息（含那条提示），正在跑的任务被判给别的节点重跑。
失败要说出来，而说出来的前提是连接还在。

---

## 6. 失败分支排查表

| 现象 / 错误码 | 层 | 原因 | 处理 |
|---|---|---|---|
| `need_root`（CLI 退出码 `3`） | 节点写文件 | 目标目录 root 所有，agent 以普通用户跑 | 在节点上 `sudo anotify-agent upgrade`，或 `sudo install -m 0755 <下载路径> <目标>` |
| `bad_sha`（CLI 退出码 `4`） | 节点校验 | 下载到的字节 sha256 ≠ 帧里的 `sha256`（中转缓存/代理改写/服务端换过文件） | 重试一次；仍失败就在节点上看 `X-Agent-Sha256` 与 body 长度，确认中间有没有改写响应的代理 |
| `download_failed`（CLI 退出码 `5`） | 节点网络 | 节点拿不到 `url_path`（TLS 不认、DNS、token 失效、中枢重启） | 先 `curl -I -H "Authorization: Bearer <node token>" <server>/v1/node/binary` 复现；令牌丢就删节点重建 |
| `not_supported` | 节点平台 | 容器只读 FS / 平台不支持自替换 | 走兜底：重跑 `install-agent.sh`（换镜像 / 换单元） |
| `node_offline`（HTTP `409`） | 中枢下发 | 节点不在线（掉线 / 排空中） | 先把节点连上（`systemctl status` / `journalctl -u anotify-agent`）再点升级 |
| `agent_binary_unavailable`（HTTP `501` / CLI 退出码 `2`） | 中枢未启用 | 中枢没带节点二进制（`ANOTIFY_AGENT_BIN` 指向的文件不存在） | 用带 `/app/anode` 的镜像，或给服务端设对 `ANOTIFY_AGENT_BIN`；查启动日志 `agent.binary.ready` 有没有出现 |
| CLI 退出码 `5` | CLI | 连不上 `server` / 读不出 `node.yaml` / 其他非权限非校验错误 | 先 `anotify-agent upgrade --check` 定位是网络还是配置 |
| 回 `up_to_date` 但版本没变 | 中枢判定 | 中枢拿的是**库里的** `agent_version`，它只在 `hello` 时更新；节点重启后还没重连（或老 agent 的 `hello` 不带版本 = 空串） | 等节点重连 / 重启 agent；确认 `hello` 里带 `agent_version`（老 agent 需要先升一次） |
| 控制台显示「未知（老版本）」 | 展示 | `agent_version` 是空串（老 agent 未上报） | 正常状态，不是错误；升一次就自然有了 |
| 「升级中」一直不变（`agentUpgrade.status="sent"`，`at` 超 60s） | 中枢等 ack | 节点在回 ack 前断线 / 被重启，`upgrade_ack` 永远不会来 | 控制台超 60s 后改显示「节点未回复（可能已断线）」（§7.2）；去节点上看 `journalctl -u anotify-agent` |
| 升级按钮不显示 / 「服务端未提供 agent 二进制」 | 控制台读侧 | `GET /v1/nodes` 顶层 `targetAgentVersion` 是空串 | 中枢没带二进制；查 `ANOTIFY_AGENT_BIN` 与启动日志 `agent.binary.ready` |

---

## 7. 控制台读侧（冻结契约 §5.1）

控制台**下发**走 `POST /v1/nodes/{id}/upgrade`；它读什么、从哪儿读，同样已经冻结
（契约 §5.1）。`GET /v1/nodes` 的响应：

```json
{
  "nodes": [ { "id": "nd_x", "agentVersion": "1.5.0+3fd5a59", "agentCommit": "3fd5a59",
               "agentUpgrade": { "status": "ok", "targetVersion": "1.5.0+3fd5a59",
                                 "error": "", "at": 1762329600 } } ],
  "targetAgentVersion": "1.5.0+3fd5a59"
}
```

| 字段 | 位置 | 语义 |
|---|---|---|
| `targetAgentVersion` | 响应**顶层**（string） | 中枢当前带的 agent 二进制版本。**空串 = 服务端没带二进制**（此时不显示升级按钮，或显示「服务端未提供 agent 二进制」） |
| `agentUpgrade` | 每台节点（对象，**恒存在**） | 默认 `{"status":"","targetVersion":"","error":"","at":0}` |
| `agentUpgrade.status` | | `""` 从未尝试 / `sent` 已下发等 ack / `ok` ack 成功 / `failed` ack 失败 |
| `agentUpgrade.error` | | 失败码，与 agent 侧一致：`need_root` / `bad_sha` / `download_failed` / `not_supported` |
| `agentUpgrade.targetVersion` | | 这次尝试的目标版本 |
| `agentUpgrade.at` | | `sent` / `ok` / `failed` 的时刻（unix 秒） |
| `agentVersion` / `agentCommit` | 每台节点 | 当前实跑的版本（来自 `hello`；空 = 老 agent） |

端点响应：`POST /v1/nodes/{id}/upgrade` 成功时回
`{"status":"sent"|"up_to_date", "targetVersion":"…"}`（失败仍走 `409` / `501`）。

### 7.1 `agentUpgrade` 为什么要落库

`nodes.agent_upgrade TEXT NOT NULL DEFAULT '{}'`（JSON），与 `agent_version` /
`agent_commit` **一起在 `+migration 3` 加**。`sent` 由端点写入，`ok` / `failed` 由
`upgrade_ack` 覆盖 —— 所以**刷新页面后上一次的升级结果还在**。

如果它只活在内存或前端状态里，一次 F5 就把「这台机器上次升失败了、原因是 `need_root`」
抹掉，用户会重新点同一个注定失败的按钮；而失败本身（尤其 `need_root`）需要人去做
一件机器做不了的事（sudo），结果必须能活到人去处理为止。

### 7.2 「sent 卡住」的判定

`status == "sent"` 只意味着「帧推出去了」，不代表节点收到了。所以：

> `agentUpgrade.status == "sent"` 且 `at` 距今 **> 60s** 仍是 `sent`（没变成 `ok` / `failed`）
> → 控制台显示「**节点未回复（可能已断线）**」，**不要一直显示「升级中」**。

节点在回 ack 前掉线时，中枢永远等不到那条 ack；而用户看到「升级中」就只会等，
不会去查连接。

---

## 8. 不变式（改这块代码前先看这一节）

| # | 不变式 |
|---|---|
| 1 | **中枢不执行**（I1）：中枢只提供字节流，不跑安装、不碰节点文件系统、不知道节点路径 |
| 2 | **二进制绝不进 WS 帧**：帧里只有指令（版本 + sha256 + `url_path`），字节走 HTTP |
| 3 | **控制台不实现下载/校验/替换**：它只发一条指令，然后展示 `agentVersion` / `agentUpgrade` |
| 4 | **版本可空**：老 agent 不带 `agent_version` = 空串，中枢不报错，控制台显示「未知（老版本）」 |
| 5 | **成功先 ack 再退出**；**`need_root` 不退出** —— 退出码只用来重启，不用来表达失败 |
| 6 | **升级结果落库**（`nodes.agent_upgrade`）：刷新页面不能把上一次的失败原因抹掉 |
