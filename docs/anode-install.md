# anode：v4 节点安装与运行

anode 只有 `serve`、`install`、`emit` 三个公开子命令。没有 `run`、旧节点配置或旧命令别名；不写子命令也不会隐式启动。`anode --version` 输出单行 `{"version":"…","commit":"…"}`，hello 和升级 ACK 使用同一构建事实。

安装面分成两层，边界是**冻结**的：

| 谁 | 负责 | 明确不做 |
|---|---|---|
| `scripts/install-anode.sh`（唯一安装入口，静态站以 `GET <origin>/install-anode.sh` 匿名下发同一份字节） | 下载、校验 sha256、停旧实例、`anode serve --check` 预检、调 `anode install` 落盘、装/启/停 systemd 服务、卸载、升级 | 不重写 YAML/unit（参数原样交给 `anode install`），不跳过校验，不接收 stdin |
| `anode install`（二进制子命令） | 只**落盘**：二进制 + `anode.yaml` + unit + 数据目录 | 不注册节点、不签 token、不 `sudo`、不调 `systemctl`、不联网下载 |

终端用户正常情况下只跑**一条命令**（§1）。`anode install` 的直接用法见 §8，服务运行与配置见 §10 起。

## 1. 一条命令装好一台节点

在控制台「节点」页注册节点后，页面给出可直接复制的命令：

```sh
curl -fsSL https://hub.example.com/install-anode.sh | sh -s -- \
  --name node-a --server https://hub.example.com --token <节点令牌>
```

- 命令里**不写 `sudo`**。脚本自己探测提权（见 §5）：`curl|sh` 下 stdin 是管道，`sudo` 只能从终端 `/dev/tty` 读密码，所以把 `sudo` 放在管道前面反而会卡住。
- 控制台给的是 `--token <令牌>`（明文只显示一次，见 §2）。它会把令牌留在 shell 历史里，日常更推荐换成 `--token-file`（两者只能给一个；升级/轮换令牌时语义完全相同，都会自动带 `--update-token`，见 §7）：

  ```sh
  curl -fsSL https://hub.example.com/install-anode.sh | sh -s -- \
    --name node-a --server https://hub.example.com --token-file /secure/node-token.txt
  ```
- 下发的脚本就是仓库里 `scripts/install-anode.sh` 的逐字节副本（`make check-anode-skill` 有 `cmp` 守卫）。想先审再跑就下载它、读完再执行，两种方式语义完全一致：

```sh
curl -fsSL https://hub.example.com/install-anode.sh -o /tmp/install-anode.sh
sh /tmp/install-anode.sh --name node-a --server https://hub.example.com \
  --token-file /secure/node-token.txt
```

结束时脚本打印一段摘要（这几行就是"装完了"的判据）：

```text
== 结论
anode 已装好并已启动
  节点      node-a
  中枢      wss://hub.example.com
  运行身份  nodeadmin（能力的权限 = 这个用户）
  二进制    /usr/local/bin/anode
  配置      /etc/anode/anode.yaml（0600：里面有令牌）
  数据      /var/lib/anode
  服务      anode.service（system）
  状态      active (running)
  日志      journalctl -u anode -f
  升级      重跑同一条安装命令（沿用现有 anode.yaml，只换二进制 + restart）
  卸载      sh install-anode.sh --uninstall

控制台「节点」页应该看到 node-a 在线；没上线先看日志：journalctl -u anode -f
```

看到 `状态 active/running` 之后去控制台「节点」页确认节点在线（没上线先看上面那行日志命令，不要反复重跑安装）。脚本先启服务再**轮询**，最多 10 秒；但判据不是 `is-active`——`Type=simple` 的 unit 在「进程刚 fork 出来、还没读到配置就退出」的那一瞬间也算 active（`SubState=start`），而 `systemctl restart` 正是等到“started”就返回，紧跟它的第一次轮询几乎必然命中这一刻（真实事故：配置目录 0700 → 读不到 anode.yaml → 422 次重启，摘要却写 `active (running)`）。所以判定用 `ActiveState=active && SubState=running`，并要求 **2 秒后仍然如此**（稳定窗口）；稳定不了就是安装失败（非 0），摘要会把 systemd 的原始 `ActiveState/SubState` 和重启次数如实打出来。

## 2. 先创建长期节点身份

管理员先在中枢调用 **POST Node → POST token**（v4 管理 API），保存返回的 Node 名称、server 地址和长期 Node identity token。明文 token **只返回一次**；后续管理响应不能取回它。身份属于节点，不是某次任务的 attempt token。具体管理资源路径/权限以中枢 API 文档为准。

`install` 与安装脚本都必须显式拿到这个已有身份：不注册、不生成 token、不安装第二个注册服务、不自动部署能力。**不要把长期凭证放进脚本、镜像或公开日志**。推荐临时保存为仅安装用户可读的 0600 token 文件，安装后由管理员按自己的凭证管理策略处理；程序不自动删除用户文件。

## 3. 这条命令做了什么（七步）

| # | 步骤 | 关键行为 | 失败时 |
|---|---|---|---|
| 1 | 解析身份 | `--name`（缺省用主机名，规范成小写 `[a-z0-9._-]`）、`--server` 归一化（`http(s)://` ↔ `ws(s)://`）、校验令牌形状 | 非 0，指出缺哪个参数 |
| 2 | 取二进制 | 三优先级下载（§4） | 非 0 + 下一步建议（`--version` / `--download-url`） |
| 3 | 校 sha256 | 摘要必须恰好 64 位十六进制；`sha256sum --check` 必须成功；再核对下载二进制的 `--version` | **一律中止**，不落盘、不做"未校验放行"降级 |
| 4 | 停旧实例 | 同一个令牌不能有两条连接：先停旧服务（含从 user 服务迁到 system 服务时的旧 user unit） | 警告并继续 |
| 5 | 预检 | 用**临时**配置（0600，只含 name/server/token）跑 `anode serve --check`，正式配置一个字没动（§6） | 非 0，打印三分诊断；正式配置未改动，升级前停掉的旧实例会被重新启动（原本没在跑就不动，§6） |
| 6 | 落盘 | `anode install`：把令牌写成临时 0600 文件传给 `--token-file`，安装结束即删除（§9） | 非 0 原文透传（权限/已有配置冲突见 §8） |
| 7 | 装服务 + 摘要 | `system` / `user` / `none` 三选一（§5），`daemon-reload` → `enable` → `restart`，再轮询「`active/running` 且 2 秒不掉」，最后打 §1 那段摘要 | 服务起不来 → 非 0，摘要里给出日志命令与 systemd 原始状态 |

退出码：`0` 成功（含 `--help` / 卸载 / `--dry-run`），`2` 用法/下载/校验/预检/服务失败。脚本在失败处直接打印下一步，不需要照着文档猜。

## 4. 取二进制：三个优先级，校验失败即停

优先级从上到下，前一个给了就完全覆盖后面的：

| 优先级 | 参数 | 从哪来 | 校验依据 |
|---|---|---|---|
| 1 | `--download-url URL`（或 `ANODE_DOWNLOAD_URL`） | 内网 / 镜像 / 测试 | `<URL>.sha256`，或用 `--checksum-url` 覆盖 |
| 2 | `--version vX.Y.Z`（或 `ANODE_VERSION`） | GitHub Release，资产 `anode_linux_<arch>` | 同目录 `<asset>.sha256` |
| 3 | 都不给 | 中枢 `GET <server>/v1/node/binary`（Bearer 节点令牌） | 响应头 `X-Agent-Sha256`；版本取 `X-Agent-Version` |

**校验和缺失、或校验工具缺失，一律失败**（不做 v3 那种"假通过"降级）：

- 没有 `.sha256` / 中枢没给 `X-Agent-Sha256` → 中止；
- 摘要不是单行/不是十六进制/长度不是 64 → 中止（远端 checksum 里的**文件名/路径一律不执行**，只取那一个 digest）；
- 机器上没有 `curl` 或 `sha256sum` → 中止（脚本会说要装什么）；
- `sha256sum` 不匹配 → 中止，提示"重试一次，仍不一致就去 release 页手工核对"；
- 下载的二进制 `--version` 跑不起来或版本不匹配 → 中止（架构不对 / 代理缓存了旧包 / 被换过）。

第三种（中枢）的两种常见拒绝：

- `501`：**这台中枢没带二进制**（`ANOTIFY_AGENT_BIN` 未配置）——改用 `--version vX.Y.Z`；
- `401`：令牌无效（节点被删了、或令牌属于另一台中枢/命名空间）——回控制台重新注册。

### 4.1 版本发现：v4 未发版 vs 发版后

- **v4 还没发版**（当前状态）：用中枢下载（优先级 3）——中枢自己就是二进制的权威来源，节点二进制随中枢一起构建。这也是控制台一条命令默认走的路。
- **发版后**：钉版本更可复现，`--version v4.1.0` 走 GitHub Release 的 `anode_linux_amd64` / `anode_linux_arm64` + 同名 `.sha256`（`.github/workflows/release.yml` 的资产名，脚本严格按它拼 URL）。
- 版本核对是**前缀**匹配：构建标签形状是 `<release>+<commit>`，所以 `--version v4.1.0` 接受 `"version":"v4.1.0"` 与 `"version":"v4.1.0+<sha>"`，不相关的版本仍然拒装。
- 离线/内网：`--download-url` + `--checksum-url` 兜底；Release 基础地址可用环境变量 `ANODE_DOWNLOAD_BASE` 替换。下载有 10s 连接 / 300s 总超时。

## 5. 服务模式与提权

```sh
sh install-anode.sh --service auto      # 默认：自动探测
sh install-anode.sh --service system    # 等价 --system
sh install-anode.sh --service user      # 等价 --user
sh install-anode.sh --service none      # 等价 --no-systemd
sh install-anode.sh --no-sudo           # 禁止提权：需要 root 的步骤直接失败
```

| 模式 | 什么情况 | 行为 |
|---|---|---|
| `auto`（默认） | 有 `systemctl` 且**实测**可提权 → `system`；有 `systemctl` 但提不了权 → `user`；没有 `systemctl`（容器/精简系统）→ `none` | 按探测结果落到下面的具体分支，摘要里写明实际用了哪种 |
| `system` | 开机自启、没登录也活着 | 装 `/etc/systemd/system/anode.service`（默认落点）；`User=<安装者>`（root 安装时取 `SUDO_USER`），并在 `restart` **之前**把 config/data `chown` 给该用户、把**配置父目录 `chmod 0755`**（否则 `0600` 配置读不到，服务起来就退：`/etc/anode` 若被建成 `0700 root`，非 root 服务连目录都进不去）；顺手停用并删除同名旧 **user** unit（两份实例会抢同一个令牌） |
| `user` | 单用户机器、不给 sudo | `systemctl --user` + `$HOME/.config/systemd/user/anode.service`；尽量 `loginctl enable-linger`，失败只 warn（登录态结束后服务会停，脚本会打印手动命令）；不能与 `--prefix` 一起用（unit 会落到 prefix 下，`systemctl --user` 发现不到） |
| `none` | 容器 / 没有 systemd | 只落盘，打印手工启动与装服务的命令（**仍然预检**，见 §6） |

`--prefix DIR` 是 **staging 布局**：`anode install` 把 unit 写进 `<DIR>/etc/systemd/system/`，而 systemd 不在那里找 unit。所以**点名**要 `--service system` / `--service user` 又同时给 `--prefix` 会被直接拒绝（报错给出能走通的出路）；`auto` 推导成 `system` 时只 warn（`ANODE_SYSTEMCTL` 覆盖的“自带 systemd 语义”除外），推导成 `user` 时**降级为 `none` 并 warn**——不静默装一个起不来的服务。要真装服务就别加 `--prefix`（用户服务用 `$HOME/.anode`）；只想落到自定义目录就用 `--service none`。

提权方式（只对真正需要 root 的那几条命令提权，不整段 `sudo sh` 重跑脚本）：

| 情况 | 判定 |
|---|---|
| 当前是 root | 直接跑 |
| `sudo -n true` 成功 | 用 `sudo -n` |
| 有终端（`/dev/tty` 可打开） | `sudo -v` 读一次密码，后续复用凭据 |
| 以上都不行 / 显式 `--no-sudo` | 不提权：`system` 模式需要的步骤直接失败并说明下一步；`--user` / `--service none` 仍可完成 |

启停之后脚本**轮询**「真的在跑」（`ActiveState=active && SubState=running`，最多 10 次、每次 1s，命中后再观察 2 秒），而不是“发完命令立刻判定”、也不是只问 `is-active`（`active(start)`／`activating(auto-restart)` 都会骗过它）；轮询失败或 `anode install` 失败时，脚本会把升级前 stop 的旧服务重新拉起来再退出（不把节点留在停止态）。`--dry-run` 只打印将执行的命令，不碰网络、不写盘、不提权、**不调 `systemctl` / `loginctl`**（也**不实测** sudo，只按“假设能提权”打印，摘要里会注明）——包括 `--service user` 的 `loginctl enable-linger` 也只是一行打印。

### 任务环境的 PATH 从哪来（“和终端对齐”）

能力的环境是白名单，`PATH` 固定等于守护进程启动基线（`execenv` 里是 builtin —— `run.env` 与
`envPassthrough` 都不允许覆盖 PATH），基线就是 unit 的 `Environment=PATH=`。守护进程没有
shell 启动过程，所以**能对齐、也应该对齐的是安装那一刻服务用户的登录 shell**：

1. 脚本用 `env -i HOME=<服务用户> … "$SHELL" -lc 'printf …"$PATH"'` 抓一次（5s 超时，只认
   最后一条标记行），逐项去重、丢掉非绝对项，再把 `$HOME/.local/bin` 钉在最前；
2. 抓到的值作为 `--unit-path` 交给 `anode install`，同时落盘 `install.json`
   （`{"source":"login-shell","shell":"/bin/bash","path":"…"}`，与 anode.yaml 同目录，0644）；
3. **重跑一律复用记录**（不再看调用者 `$PATH`，`--refresh-path` 才重抓）→ unit 逐字节一致，
   “重跑=升级”不会被 `existing unit is unknown/modified` 打断，也不会把节点停在已 stop 的状态。

为什么不是“照抄安装时的 `$PATH`”：`curl|sh` 的父进程（CI、另一个 ssh 会话、systemd-run）不一定
有登录 shell 的 PATH，换个终端重跑就变成“另一个 unit”。记录是这两件事的交点：**抓取只发生一次**。

存量节点补目录不用重装：`anode path add <dir>` 只改 unit 里的 PATH 行 + 记录（守卫要求两者一致，
手改过的 unit 会被拒绝），`anode path show` 看当前值。运维侧的两个开关：

| flag | 作用 |
|---|---|
| `--refresh-path` | 重新抓一次登录 shell 的 PATH，重写 unit + `install.json` |
| `--no-capture-path` | 不抓，退回老的固定集合（`$HOME/.local/bin` + 系统目录） |

**边界**：按目录切换的版本（direnv / `.nvmrc` / `mise.toml`）不会在任务里生效 —— 任务没有 cwd
那个“终端上下文”。要锁版本就在能力脚本里写绝对路径或用 `mise exec`。

## 6. 预检：`anode serve --check`

脚本在停掉旧实例之后、落正式配置之前，用临时配置（0600，只含 name/server/token）跑一次：

```sh
anode serve --check --config <tmp>/preflight.yaml --data <tmp>/data   # 脚本内部构造
```

语义（`anode` 自己的契约，见 `docs/v4/anode-cli.md` §3）：

- **一次性**：握手走到 `welcome` 就退出；不写盘、不监听、不启动 runner/emit。
- 成功：stdout 一行 `check ok: node=<name>`（不含 token）→ exit 0。
- 失败：stderr 一行 `anode check: <kind>: …` → **exit 2**。`kind` 三类，诊断直接对应下一步：
  - `unauthorized`（`welcome` 之前的 close 1008 / 401）→ 令牌不对 / 节点被删 / 属于另一台中枢，回控制台重新注册；
  - `timeout` → 网络或 TLS：核对 `--server` 从这台机器能不能通、证书链是否受信；
  - `config` → `anode.yaml` 的 name/server 形状不对。
- 上限**固定 10s**（不新增 flag）；脚本外层还有 `--preflight-timeout SEC`（默认 20）兜底，超时就收掉进程并提示。

预检失败时**正式配置没有被改动**；升级路径上，预检前为“让出令牌”而 stop 的旧实例会被重新启动再退出（原本没在跑就不会凭空 start）——一次失败的升级不把节点留在停止态。修好重跑同一条命令即可。想跳过它硬装：`--no-preflight`（不推荐——这正是把“装完了但在线不了”提前一步暴露的地方）。**这是预检的唯一例外：`--service none` 也预检**（否则错令牌会以 exit 0 “装成功”却永远上不了线）。

`--service none` 不停旧服务（停了没人再拉起来），所以如果那台机器上真有一台旧实例在跑，这一次预检会把它顶掉一次：同一令牌已经有一条 live 连接，新的 hello 会把旧的以 1008 `superseded` 踢掉，旧进程 1 秒内自愈重连（`--check` 的 hello 还会把 `declared_capabilities` 整份覆盖成空，下一次重连的 hello 会写回）。要避开这段瞬断，就先用 `systemctl stop anode.service` 或用 `--no-preflight`。

排障时单独跑 `--check` 同理：**不要对一台正在工作的节点跑**（会瞬时顶掉它的连接，随后自愈）；只想验令牌形状可以对着临时配置跑。

## 7. 卸载与升级

**升级 = 重跑同一条安装命令。** 没有单独的 upgrade 子命令：

- 脚本从已有 `anode.yaml` 读回 name / server / token，命令行可以只写 `--name` 或什么都不写；
- 配置里用户的 namespaces、vars、limits、listen、envPassthrough **原样保留**，只换二进制并 `restart`；摘要会写"升级完成"；
- 换令牌是显式动作：命令行给了 `--token` / `--token-file` 且与已保存的不同 → 脚本自动带上 `--update-token`（`anode install` 默认拒绝覆盖不同 token）。只想轮换令牌也可以直接 `anode install … --update-token`（§8）。

```sh
# 升级：重跑（令牌沿用配置里的）
curl -fsSL https://hub.example.com/install-anode.sh | sh -s

# 轮换令牌：带新令牌重跑
curl -fsSL https://hub.example.com/install-anode.sh | sh -s -- --token-file /secure/rotated-node-token.txt
```

**卸载**不需要 token/server（按已知落点逐个清，清不到就跳过；没权限的项如实报告而不是假装删掉）：

| 命令 | 停服务/删 unit | 二进制 | `anode.yaml` | 数据目录 |
|---|---|---|---|---|
| `--uninstall` | 是 | 删 | **删**（令牌不该留在机器上） | 保留 |
| `--uninstall --keep-config` | 是 | 删 | 保留（脚本会提示"令牌还在里面"） | 保留 |
| `--uninstall --purge` | 是 | 删 | 删 | **删**（产物缓存/工作区） |

`anode.yaml` 那一列包含同目录的 `install.json`（PATH 记录）：跟配置同生共死 —— 留着它再装一次
会复用一个旧 PATH，`--keep-config` 时两者一起保留。

```sh
sh install-anode.sh --uninstall                # 连配置一起删
sh install-anode.sh --uninstall --keep-config  # 保留配置（想留令牌备份）
sh install-anode.sh --uninstall --purge        # 连数据目录一起删
```

卸载结束会提示：节点在控制台里会显示离线；真正不需要了，去「节点」页把节点删掉。

## 8. 不用脚本时：直接 `anode install`

脚本只是把下面这条命令包起来。没有脚本、或想自己控制每一步时：

```sh
./anode --version
./anode install --name node-a --server wss://hub.example.com \
  --token-file /secure/node-token.txt
```

`anode install` 的边界**没变**：无 stdin、无 sudo、无任何 `systemctl` 调用。写文件权限不足会明确失败，需管理员以合适身份重跑原命令。成功输出的是管理员后续步骤，而不是擅自操作宿主服务：

```sh
systemctl daemon-reload
systemctl enable --now anode.service
```

默认安装到：

| 文件 | 默认路径 / 权限 |
|---|---|
| 二进制 | `/usr/local/bin/anode`，0755 |
| strict 配置 | `/etc/anode/anode.yaml`，0600 |
| systemd unit | `/etc/systemd/system/anode.service`，0644 |
| 私有数据目录 | `/var/lib/anode`，新建目录 0700 |

配置的**父目录**（默认 `/etc/anode`）新建为 `0755`：秘密边界是文件的 `0600`，而系统的服务以**安装者**（非 root）身份运行，目录不可穿越就等于读不到配置。历史上这里被建成 `0700 root`，导致服务起来就 `exit 2`、`Restart=always` 崩溃重启；重跑安装脚本会把它 `chmod 0755` 修好（幂等）。

unit 用 `serve --config … --data …`，`UMask=0077`，`Restart=always`。它不会导入一个全宿主环境文件。若需要凭证变量，管理员通过自己的受保护 systemd 配置显式注入，并同时在 anode.yaml 白名单声明名字。

装成**系统**服务时必须给 unit 三个字段（不给就等于以启动者身份跑，root 装就是 root 跑，能力脚本的权限等于整台机器）：

| flag | 写进 unit 的 | 说明 |
|---|---|---|
| `--unit-user NAME` | `User=` | 服务以谁的身份跑；脚本装 system 服务时自动填安装者（root 安装取 `SUDO_USER`） |
| `--unit-home DIR` | `Environment=HOME=` | 必须是存在的绝对路径；能力脚本的 `HOME` 基线 |
| `--unit-path DIR` | `Environment=PATH=` | **可重复**，按给出顺序用 `:` 拼成一条；不写就沿用 `install.json` 记录（都没有才继承 systemd 默认） |
| `--path-source S` | `install.json` 的 `source` | 记录这条 PATH 的来源（`login-shell`/`flags`/`fallback`），只用于事后判断 |
| `--path-shell P` | `install.json` 的 `shell` | 抓 PATH 用的登录 shell（例 `/bin/bash`），只用于事后判断 |

```sh
./anode install --name node-a --server wss://hub.example.com --token-file /secure/node-token.txt \
  --unit-user nodeadmin --unit-home /home/nodeadmin --unit-path /usr/local/bin --unit-path /usr/bin
```

`anode install` 只写 unit，**不创建用户**：`--unit-user` 指定的账号必须已存在于本机，否则服务启不来（脚本装 system 服务时自动用的是安装者自己，不存在这个问题）。路径参数同理不会被创建，HOME 必须已经存在。

三者留空就不输出那一行；非绝对路径、`..` 越界、非法用户名由 `anode install` 直接拒绝。

### 重装与凭证更新

同名/server/token 重装保留用户现有 namespaces、vars、limits、listen、envPassthrough，甚至不改原配置字节。不同 token 默认拒绝；只有显式 `--update-token` 才更新保存的 token，保持所有本机配置值。换 name/server 视为不同身份，拒绝覆盖。已有未知/非法配置、修改过的 unit、非本安装的已有二进制会明确拒绝，不删用户数据目录。单文件采用同目录临时写、fsync、rename，失败不会留下半个 YAML/二进制；安装涉及多个文件，不声称跨文件事务，权限问题处理完可以安全重跑。

```sh
./anode install --name node-a --server wss://hub.example.com \
  --token-file /secure/rotated-node-token.txt --update-token
# 随后管理员重启 service，使新 token/本机配置生效。
```

`--prefix /absolute/staging` 把默认布局置于 staging 根内；`--config`、`--bin`、`--unit`、`--data` 可显式覆盖路径。prefix 非根、路径必须绝对规范，无 `..`、符号链接、特殊文件或 prefix escape，三个文件名仍必须是 anode.yaml、anode、anode.service。staging 不启动/改动宿主服务；输出如需复制 unit 的管理员步骤。目录应由管理员控制，这是安装安全边界，不是对恶意本机并发 writer 的 sandbox。

## 9. 安全

- **令牌不进日志/报告**：脚本把令牌写进自己的临时 0600 文件再以 `--token-file` 交给 `anode install`（不进程 argv、不 echo、不进摘要），安装结束即 `rm`。从中枢下载二进制时也**不走 argv**：`curl --config <0600 文件>` 读 `Authorization: Bearer …`（同机其他用户从 `/proc/<pid>/cmdline` 看不到令牌），文件随临时目录删除。摘要与预检诊断都只出现节点名与中枢地址。
- **优先 `--token-file`**：`--token nt_…` 会留在 shell 历史（以及安装脚本自己的 argv 里，`ps` 可见），只适合临时排查；控制台给的命令用的是 `--token`（它只显示一次明文），要长期使用就换成 `--token-file`（§1）。
- **不静默跳过校验**：缺校验和、缺 `sha256sum`/`curl`、摘要形状不对、不匹配——一律失败。校验和保证完整性，不是签名；下载源与摘要源仍必须可信。
- **不执行远端内容**：checksum 文件里的文件名/路径一概不用（只取那一个 digest），版本号只允许 `[A-Za-z0-9._-]`。
- **落盘权限**：`anode.yaml` 0600（里面有令牌），数据目录 0700，unit 0644。
- **最小权限运行**：能力脚本以 service 用户的宿主权限执行，不是 sandbox。给 `--unit-user` 一个专用低权用户，而不是让系统服务跟着 root 跑。
- **控制台侧**：命令块里的令牌只进 `textContent`，关闭面板即清空；不进 localStorage / console / URL，也不出现在截图里。

## 10. serve 与严格配置

```sh
anode serve --config /etc/anode/anode.yaml --data /var/lib/anode
```

`serve` 只支持这两个路径 flags（以及 `-h` / `--check`，见 §6）；listen/env 行为来自 **strict anode.yaml**，没有旧 `node.yaml` 接口。未知字段/多文档/重复或非法白名单/空 namespaces/非正 limits 启动失败，不半在线。身份被拒（`welcome` 之前的 close 1008 / 401）现在**直接退出**，不再无限重连。SIGINT/SIGTERM 触发 drain，终止授权、TERM→grace→KILL 真进程组后关闭网关和连接。缺应用配置时不会假装在线。

server 空 path 补共享 `/v1/node/stream`；显式反代 path 原样使用。合法示例（不要把真实 token 提交到版本库）：

```yaml
name: node-a
server: wss://hub.example.com
token: "REPLACE_WITH_EXISTING_NODE_IDENTITY_TOKEN"
namespaces: [default]
listen: tcp://127.0.0.1:0            # 默认仅本机、随机实际端口；可用 unix:///run/anode/emit.sock
vars:
  SOURCE_ROOT: /srv/project
limits:
  total: 2
  perCapability:
    review: 1
envPassthrough:
  - API_KEY
```

namespaces 是申报，与中枢授权取交集。机器值 local > node，下游 ns defaults 只在任务内存中合并，不落 applied。修改本机 YAML 需重启。能力仅由管理员 **manual deploy** 请求：Node Bearer GET `/v1/node/capabilities/{digest}` 返回直接 Artifact JSON（无 wrapper），验证 refs/spec/digest 后原子缓存；install 与重连都不是自动部署。

### 环境和宿主权限（不是 sandbox）

任务只拿基础 ANOTIFY_ 变量、params、明确 run.env 和宿主 **名字白名单**。PATH 固定为节点进程启动基线，命令探测同源；禁止 run.env/passthrough 覆盖 PATH。HOME 来自节点基线/明确 run.env，必须是存在的绝对目录。白名单 ≤32 名，拒绝重复/非法/PATH/HOME/ANOTIFY_；run.env 优先于同名白名单值。凭证值不进入 hello/applied/hash/诊断；诊断只报告实际名字与来源。

凭证不是沙箱：脚本以 service 用户的宿主权限执行，仍可访问该用户可读的文件、网络、工具和挂载。选择最小权限服务用户，并确保它对指定二进制目录具有升级所需权限；程序不会运行 sudo 或创建新的权限隔离产品。

run.files 普通文件写入 0600，entrypoint 0700，直接按 shebang exec，不包额外 shell。用户 workDir 必须事先存在、绝对规范，禁止越界/符号链接逃逸；节点不创建它。**同名文件会被覆盖且权限改变**，包括用户原有同名文件；明确选择专用 workDir 或接受这个副作用。节点绝不删除用户 workDir/其祖先，TTL 不构成用户目录清理许可。

## 11. 脚本 emit

```sh
anode emit review --id logical-report --data '{"summary":"ready"}'
anode emit --to review --data '{}'
```

目标可以位置参数或 `--to`（矛盾目标明确失败）；只有 `--id`、`--data`，data 必须是 JSON object。token 只从 `protocol.EnvTaskToken`（`ANOTIFY_TASK_TOKEN`）、端点只从 `protocol.EnvEmitEndpoint`（`ANOTIFY_EMIT_ENDPOINT`）读取；token 只放 Authorization header，**不进 argv/query/payload**。TCP 使用节点实际端点；`unix://path` 自定义 HTTP transport 请求 `/emit`。网关经唯一 E1 task.emit/task.emit.ack WS 向中枢请求，只有中枢授权/幂等/建下游是权威。

| 退出码 | 判据 |
|---|---|
| 0 | HTTP 200 已接受，包括 taskId:null + eventId/pruned 的剪枝 |
| 2 | 缺目标/非法参数/HTTP 400 |
| 3 | 缺/无效 attempt token、HTTP 401 |
| 4 | HTTP 409 task_not_running |
| 6 | HTTP 409 emit_id_conflict（同逻辑 id 异 body） |
| 5 | 超时、断线、未知结果，**可能已派发** |

结果 stdout 一行 JSON；错误另 stderr 一行人话。不自动重发请求，尤其不无 id 重发。相同 logical task+显式 id+同 body 可跨 attempt 返回原 task 或原剪枝 event；异 body 409。旧 token 必须先被拒，不能靠已知 id 取回旧权威授权。无 id 两次是两个不同投递。`emit` 退出 5 是脚本结果未知，**不得**当 whole-task infra 自动重跑的依据；想安全重发，由脚本显式使用同一个 id/body。

## 12. 每 attempt 导出与 A1 上行

A2 为每 `(taskId, attempt)` 新建系统自有唯一 0700 导出根，注入 `ANOTIFY_ARTIFACT_DIR`。任务重试/并发共享 workDir 不共享导出根；不要依赖固定工作目录或 stdout 魔法：

```sh
#!/bin/sh
set -eu
printf '%s\n' 'report bytes' > "$ANOTIFY_ARTIFACT_DIR/report.txt"
mkdir "$ANOTIFY_ARTIFACT_DIR/details"
printf '%s\n' 'detail' > "$ANOTIFY_ARTIFACT_DIR/details/output.txt"
anode emit review --id report --data '{"summary":"ready"}'
```

空导出目录合法 `[]`；创建/注入/枚举失败、symlink/特殊文件是明确 artifact_export_invalid failed，不假空成功、不自动重试。用户 workDir 内旧目录不创建、不删除、不因存在拒绝。

完成后安全枚举→托管副本→sha256→A1 出站 PUT raw bytes `/v1/node/tasks/{taskId}/artifacts/{sha256}`，携 Node Bearer 与 attempt/lease/task-token/编码逻辑 path headers。201 首次、200 同 path+hash 幂等，异 hash 409；最多 32 项/单 16MiB/attempt 64MiB。上传期间继续续租；只有授权仍有效才对同 path/hash 有界重试，不 whole-task replay。确定上传失败为 artifact_upload_failed，保留已经成功上传的清单，不能把未知上传伪成功/空数组。用户下载授权/过期 410 由中枢负责，节点不开放用户文件回源。

GC 默认 7 天，grant/冻结 artifact 的 resultTtlSec 可覆盖，**0 永不按时间删**。只清系统创建的 attempt/work/export/日志/托管副本；活动/未确认 reaped 槽位不清；未知 meta 不删；用户原目录、原文件和祖先永不删。

## 13. 自升级与容器

升级是 drain→确认物理组 reaped（历史 lease/slot ACK）→同源 Node Bearer 下载 raw binary→sha 校验→下载二进制实际 `--version` 自检→同目录 atomic replace→ACK **实际 version+commit**→真实 exec 同 argv/service env 重启→新 hello。自检输出有界/10s 超时，环境只有 PATH/HOME，不继承任务 run.env 或宿主凭证。bad_sha/自检失败不替换、不重启；目标目录权限不足明确 `need_root`，不把 Linux 一概伪装成 not_supported。不确认 reaped 不释放真实物理槽位、不升级；缩预算也保留活存量。升级失败时已经进入 drain，不自动恢复派发或重跑旧任务；管理员修复下载摘要/权限后重启 serve 恢复接任务。

这条自升级是**在线**路径；离线机器用 §7 的"重跑安装命令"升级（脚本负责停旧实例/预检/重启），两者不重复执行同一次替换。

```sh
docker build -f Dockerfile.anode --build-arg VERSION=v4.0.0 \
  --build-arg COMMIT=RELEASE_COMMIT -t anode:v4.0.0 .
docker run --name anode --restart unless-stopped \
  -v /secure/anode-config:/etc/anode:ro \
  -v anode-data:/var/lib/anode \
  -e API_KEY=EXPLICIT_VALUE anode:v4.0.0
```

Dockerfile 独立 CGO=0 Go 二进制，无 SQLite dependency closure，默认 serve，仅配置默认 local emit，不发布端口、不导入宿主环境。在容器里装节点时（没有 systemd）用 `--service none`，只落盘再让容器进程直接跑 `serve`：配置/数据卷显式提供；`-e API_KEY` 和 config 白名单需同时具备。镜像默认进程用户权限不是 sandbox，必要时用管理员选择的 `--user` 和正确卷权限。容器内二进制目录只读/无写权限时 self-upgrade 明确失败，应由管理员更新镜像重启，不能伪成功。构建需外部 Go/Alpine/CA 网络；网络失败不是镜像构建 PASS。

## 14. 验证映射

具名测试和原始命令/结果见 `teams/anode/N3-TESTS.md`。安装测试全 temp/offline，无宿主 service/binary 修改；脚本测试 fake curl/假 systemctl/假 sudo/假 loginctl + 真 `anode` 二进制，断言下载、sha256、预检、install、`daemon-reload`/`enable`/`restart` 顺序、`is-active` 轮询，并断言 token 不出现在 stdout；另覆盖篡改 sha256 中止（精确 exit 2）、`--token`/`--token-file` 缺失、`--no-preflight`、预检失败不装服务且**恢复升级前停掉的旧服务**（没停过就不 start）、`--service none` 不调 systemctl 但**照样预检**（错令牌 → exit 2）、`--token-file` 轮换自动 `--update-token`、系统服务的 **chown 早于 restart**（假 `systemctl is-active` 只在「config 属主 == unit 的 `User=`」时为 active）、起不来/装不上时**恢复旧服务**并把子进程失败码带进诊断、PATH 抓一次落 `install.json`、重跑复用记录（换 caller PATH / 换登录 shell 都不换 unit，`--refresh-path` 才更新）、`--prefix` + `--service system` / `--service user` 明确拒绝、`auto` 推导出的 `user` 在 `--prefix` 下降级为 `none`（不调 systemctl）、`--dry-run --service user` 不调 `loginctl`、中枢下载路径的令牌只走 `curl --config`（不进 argv）、重跑=升级、`--uninstall`/`--keep-config`/`--purge`/`--dry-run`，以及下发副本与仓库脚本字节一致（`cmp`）。能力脚本测试执行实际构建 anode emit，含跨 attempt 回原 task/pruned、异 body conflict、旧 token 先拒、无 id 不同投递；升级测试复制真实旧 binary，在 fake hub 下替换当前进程的文件、真实重启并上报新 hello，不用空 Restart 冒充。

`make check-anode-skill` 是文档侧的门：`docs/v4/anode-cli.md` 代码块里的每个子命令/flag 都要在真二进制的 `-h` 里存在，`web/install-anode.sh` 必须与 `scripts/install-anode.sh` 字节相同。
