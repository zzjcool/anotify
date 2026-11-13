# anode CLI 手册（v4 节点）

> 状态：**被机检**（`make check-anode-skill` / `scripts/check_anode_skill.py`）。
> 本文的 ```bash 代码块是**操作面契约**：块里出现的每个 `anode <sub>` 与 `--flag`
> 都必须能在真二进制的 `-h` 里找到；改 flag 忘了改这里 → CI 红。
> 这与 v3 的 `cmd/agent/skill.md` 是同一个防漂移思路（手册 ↔ 真二进制），
> 只是 v4 的节点产品换成 `anode`，手册从「内嵌进二进制」改为「随仓库发布」。

`anode` 是**节点**（执行者）唯一的产品二进制，只有四个子命令：

| 子命令 | 作用 | 谁在用 |
| --- | --- | --- |
| `install` | 把二进制/配置/服务单元**装到本机**（不做注册、不签 token、不 sudo） | 管理员 / 安装脚本 |
| `serve` | 常驻：连中枢、跑能力、开 emit 端点 | systemd / 容器 |
| `path` | 看/调**任务环境的 PATH 基线**（`Environment=PATH=` + `install.json`） | 管理员 |
| `emit` | 能力脚本的**唯一出口**（把产物交给下游能力） | 能力脚本 |

内置事实（配置、身份、变量）见 `docs/design/v4/anode.md`；本文件只讲命令行。

---

## 1. 装一台节点：`anode install`

token 由中枢签发（`POST /v1/nodes` 建 Node → `POST /v1/nodes/{name}/tokens` 签 token，
明文只返回一次）。`install` 只**落盘**，不替你注册节点。

```bash
# 标准路径（/usr/local/bin + /etc/anode/anode.yaml + systemd unit）
anode install --name node-a --server wss://anotify.example.com --token-file /root/anode.token

# 全部落到一个非 root 前缀下（沙箱 / 打包 / 容器）
anode install --prefix /opt/anode --name node-a --server ws://127.0.0.1:8080 --token nt_xxxxx

# 逐项指定路径（覆盖 --prefix 推导出来的默认值）
anode install --name node-a --server wss://anotify.example.com --token-file /root/anode.token \
  --config /etc/anode/anode.yaml --bin /usr/local/bin/anode --unit /etc/systemd/system/anode.service --data /var/lib/anode

# 系统服务：unit 里写 User=<安装者>，并显式给出 HOME/PATH
anode install --name node-a --server wss://anotify.example.com --token-file /root/anode.token \
  --unit-user anotify --unit-home /var/lib/anotify --unit-path /usr/local/bin --unit-path /usr/bin

# 只轮换令牌：保留本机配置与数据
anode install --name node-a --server wss://anotify.example.com --token-file /root/anode.token --update-token
```

- `--token` 与 `--token-file` **二选一**（前者会落进 shell history，只适合临时排查）。
- 已存在的配置不会被默认覆盖；换 token 必须显式 `--update-token`。
- `--unit-user` / `--unit-home` / `--unit-path` 只影响**写出来的 unit**：
  - `--unit-user` → `User=`（服务以谁的身份跑；不写就是启动者，root 装就是 root 跑，
    能力脚本的权限等于整台机器）；
  - `--unit-home` → `Environment=HOME=`（必须是绝对路径；这是能力脚本的 `HOME` 基线）；
  - `--unit-path` → `Environment=PATH=`，**可重复**，按给出顺序用 `:` 拼成一条。
  三者留空就不输出那一行（unit 继承 systemd 的默认），形状不合法（相对路径/越界/非法用户名）直接拒绝。
- `--path-source` / `--path-shell` 只进 `install.json`（记录这条 PATH 是 `login-shell` /
  `flags` / `fallback` 里的哪一种、由哪个 shell 抓的）：事后回答「为什么任务里没有某个命令」
  全靠它。`install.json` 与 `anode.yaml` 同目录，`0644`（PATH 不是秘密）。
- 给了 `--unit-path` 就**写记录**；不给且记录已存在就**沿用记录**（同一套安装重跑 → unit
  逐字节一致，`existing unit is unknown/modified` 不会挡住升级）；`--refresh-path`
  （安装脚本的参数）走「重抓登录 shell」。
- **不做**：自动注册节点、自动签 token、`sudo`、`systemctl`、联网下载。
  这些属于完整安装器 `scripts/install-anode.sh`（下载 → 校 sha256 → 停旧实例 →
  `anode serve --check` 预检 → 调 `anode install` 落盘 → 装服务 → 轮询 `is-active` → 打摘要）；
  `anode install` 自己永远只落盘，不碰服务生命周期。

## 2. 任务环境的 PATH：`anode path`

任务的环境是**白名单**，`PATH` 固定等于守护进程启动基线（`execenv` 里是 builtin：
`run.env` / `envPassthrough` 都不允许覆盖 PATH），基线就是 unit 的 `Environment=PATH=`。
守护进程没有 shell 启动过程，所以基线来自**安装那一刻服务用户的登录 shell**（安装器抓一次，
写进 `install.json`），之后重跑一律复用。

```bash
# 看这台节点的任务能看见哪些目录（--raw 只打 PATH 一行，给脚本用）
anode path show --config /etc/anode/anode.yaml
anode path show --config /etc/anode/anode.yaml --raw          # 只打 PATH 一行（给脚本用）
anode path show --config /etc/anode/anode.yaml --raw-source   # 只打来源（login-shell|flags|fallback）

# 存量节点补一个工具链目录（只改 unit 里的 PATH 行 + 记录，不动其余任何一行）
anode path add --config /etc/anode/anode.yaml --unit /etc/systemd/system/anode.service /home/node/.local/share/mise/shims
systemctl daemon-reload && systemctl restart anode.service
```

- `show` 只读；`--raw` / `--raw-source` 是给安装脚本用的两行机读输出（脚本据此判断记录是不是
  "抓自登录 shell"，`flags`/`fallback` 的记录会被重抓而不是沿用）。
- `add` 的守卫：unit 里的 PATH 必须与 `install.json` **完全一致**，不一致（手改过）就拒绝，
  不会把别人的改动静默覆盖掉。
- 目录只做形状校验（绝对 / clean / 非 `/` / 不含破坏 unit 行的字符），**不要求此刻存在**。
- 按目录切换的版本（direnv / `.nvmrc` / `mise.toml`）不会在任务里生效：要锁版本就在能力脚本里
  写绝对路径或 `mise exec`。

## 3. 起节点守护：`anode serve`

```bash
# 常驻（systemd unit 跑的就是这一行）
anode serve --config /etc/anode/anode.yaml --data /var/lib/anode

# 预检：只连一次中枢验证身份与可达，然后退出（不写盘、不监听、不跑能力）
anode serve --check --config /etc/anode/anode.yaml --data /var/lib/anode
```

- `--config` 是**严格**的 `anode.yaml`：未知字段启动失败，不热更（改配置就重启）。
- `--data` 是私有数据目录（缓存的能力产物、执行档案、emit 端点信息）。
- 启动是**全有或全无**：身份被拒 / 配置不合法就退出，不存在「半在线」。
- `--check` 是**一次性自检**：握手走到 `welcome` 就退出。成功时 stdout 只有一行
  `check ok: node=<name>`（不含 token）→ exit 0；失败时 stderr 打
  `anode check: <unauthorized|timeout|config>: …` → **exit 2**。上限**固定 10s**，
  不写盘、不监听、不启动 runner/emit，也不改配置——所以可以在落正式配置**之前**用它验令牌。
- `serve` 本体也认这条判据：`welcome` 之前的 close 1008（或 401）是**身份拒绝**，直接退出，
  不再无限重连——错误令牌重连只会把中枢日志刷成一片认证失败。

## 4. 能力脚本的出口：`anode emit`

节点在执行能力前注入 `ANOTIFY_EMIT_ENDPOINT` 与 `ANOTIFY_TASK_TOKEN`；
脚本用 `emit` 把本次产物交给下游能力（`--data` 就是下游的 params）。

```bash
# 显式 --to（推荐：可读）
anode emit --to deploy-preview --data '{"repo":"acme/x"}'

# 位置参数写法（与 --to 等价，二选一）
anode emit deploy-preview --data '{"repo":"acme/x"}'

# 幂等键：同一次任务里重复 emit 用同一个 id
anode emit --to deploy-preview --id apply-fix --data '{"ok":true}'
```

- 0 次 emit = 到此为止（合法）；失败**之前** emit 过就继续（路由只看出口，不看 status）。
- 不在任务里（没有 `ANOTIFY_EMIT_ENDPOINT` / `ANOTIFY_TASK_TOKEN`）会明确报错退出，
  不会静默成功。

## 5. 版本与帮助

```bash
anode -h          # 三个子命令
anode serve -h    # 子命令的 flag（install / emit 同理）
anode --version   # 单行 JSON：{"version":"<label>","commit":"<sha>"}
```

`--version` 的 `<label>` 与中枢的 `internal/version.String()` 是**同一个串**
（`<version>+<commit>`）：中枢拿它当升级目标版本，节点在 hello 里上报同一个值；
两处不等 → 「已是最新」永远判不出来，节点会被反复升级。`scripts/install-anode.sh`
因此按**前缀**核对发布版本（允许 `+<commit>` 后缀）。

## 6. 边界（踩了就错的几条）

- **不写中枢数据库**：`cmd/anode` 只依赖 `internal/protocol` 与 `internal/artifact`
  两个中枢包（`make check-anode-skill` 会拦），不 import `internal/server` /
  `internal/store`，也不直接开 sqlite。
- **不注册自己**：节点是集群级对象，登记与授权都在中枢（`POST /v1/nodes`）。
- **不猜宿主环境**：`envPassthrough` 只写变量名，值留在本机；`run.env` 里禁止覆盖 `PATH`。
- **不做插件系统**：三个子命令之外没有第四个入口；新增能力走中枢的能力资源。

## 7. 一条命令装节点（终端用户的入口）

终端用户**不需要**自己拼 `anode install` + `systemctl`：中枢「节点」页注册完给出**一条命令**，
脚本自己完成下载、校验、预检、落盘、装服务、启停与校验：

`curl -fsSL <中枢 origin>/install-anode.sh | sh -s -- --name <节点名> --server <中枢 origin> --token-file <token 文件>`

- 命令里**不写 `sudo`**：脚本自己探测提权（root 直跑 / `sudo -n` / 从 `/dev/tty` 读一次密码）；
  `curl|sh` 下 stdin 是管道，`sudo` 只能从终端读密码，所以把 `sudo` 写在管道前面反而会卡住。
- 下发的 `install-anode.sh` 就是 `scripts/install-anode.sh` 的逐字节副本（`make check-anode-skill` 有 `cmp` 守卫）。
- 重跑同一条命令 = **升级**（沿用现有 `anode.yaml`，只换二进制并 restart）；
  `--uninstall` 卸载（默认删配置，`--keep-config` 保留，`--purge` 连数据目录）。
- 完整走查（下载优先级、服务模式与提权、预检诊断、卸载/升级、安全）见 `docs/anode-install.md`。
