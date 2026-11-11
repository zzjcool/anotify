# anode CLI 手册（v4 节点）

> 状态：**被机检**（`make check-anode-skill` / `scripts/check_anode_skill.py`）。
> 本文的 ```bash 代码块是**操作面契约**：块里出现的每个 `anode <sub>` 与 `--flag`
> 都必须能在真二进制的 `-h` 里找到；改 flag 忘了改这里 → CI 红。
> 这与 v3 的 `cmd/agent/skill.md` 是同一个防漂移思路（手册 ↔ 真二进制），
> 只是 v4 的节点产品换成 `anode`，手册从「内嵌进二进制」改为「随仓库发布」。

`anode` 是**节点**（执行者）唯一的产品二进制，只有三个子命令：

| 子命令 | 作用 | 谁在用 |
| --- | --- | --- |
| `install` | 把二进制/配置/服务单元**装到本机**（不做注册、不签 token、不 sudo） | 管理员 / 安装脚本 |
| `serve` | 常驻：连中枢、跑能力、开 emit 端点 | systemd / 容器 |
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

# 只轮换令牌：保留本机配置与数据
anode install --name node-a --server wss://anotify.example.com --token-file /root/anode.token --update-token
```

- `--token` 与 `--token-file` **二选一**（前者会落进 shell history，只适合临时排查）。
- 已存在的配置不会被默认覆盖；换 token 必须显式 `--update-token`。
- 不做：自动注册节点、自动签 token、`sudo`、联网下载（下载/校验在
  `scripts/install-anode.sh`，它只验 sha256 与 `--version`，然后把参数交给 `anode install`）。

## 2. 起节点守护：`anode serve`

```bash
anode serve --config /etc/anode/anode.yaml --data /var/lib/anode
```

- `--config` 是**严格**的 `anode.yaml`：未知字段启动失败，不热更（改配置就重启）。
- `--data` 是私有数据目录（缓存的能力产物、执行档案、emit 端点信息）。
- 启动是**全有或全无**：身份被拒 / 配置不合法就退出，不存在「半在线」。

## 3. 能力脚本的出口：`anode emit`

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

## 4. 版本与帮助

```bash
anode -h          # 三个子命令
anode serve -h    # 子命令的 flag（install / emit 同理）
anode --version   # 单行 JSON：{"version":"<label>","commit":"<sha>"}
```

`--version` 的 `<label>` 与中枢的 `internal/version.String()` 是**同一个串**
（`<version>+<commit>`）：中枢拿它当升级目标版本，节点在 hello 里上报同一个值；
两处不等 → 「已是最新」永远判不出来，节点会被反复升级。`scripts/install-anode.sh`
因此按**前缀**核对发布版本（允许 `+<commit>` 后缀）。

## 5. 边界（踩了就错的几条）

- **不写中枢数据库**：`cmd/anode` 只依赖 `internal/protocol` 与 `internal/artifact`
  两个中枢包（`make check-anode-skill` 会拦），不 import `internal/server` /
  `internal/store`，也不直接开 sqlite。
- **不注册自己**：节点是集群级对象，登记与授权都在中枢（`POST /v1/nodes`）。
- **不猜宿主环境**：`envPassthrough` 只写变量名，值留在本机；`run.env` 里禁止覆盖 `PATH`。
- **不做插件系统**：三个子命令之外没有第四个入口；新增能力走中枢的能力资源。
