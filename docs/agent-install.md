# 接入 Agent：原生安装

本文回答两件事：

1. **原生装**怎么装（对比 Docker 的取舍）
2. **能力工作目录**归谁管（一句话：归能力定义，见 §3）

---

## 1. 两条路，本质区别是「能看见什么」

| | Docker | 原生（本文） |
|---|---|---|
| 一行命令 | ✅ `docker run ...` | ✅ `curl ... \| sh -s -- ...` |
| 能看见宿主机文件 | ❌ 只有容器内（除非 `-v`） | ✅ 运行它的那个用户能看见什么，它就能看见什么 |
| 能在你的项目目录里跑 `make test` | 需要额外 `-v` 挂载 | 能力里声明 `run.workDir` 即可 |
| 升级 | 拉新镜像重启 | `anotify-agent upgrade` 或控制台点一下（兜底：重跑安装脚本） |
| 隔离 | 容器边界（更强） | 进程边界（= 你的用户权限） |

**选型建议**：能力只做网络/API 类的事（发通知、调 webhook、跑测试镜像）→ Docker 更省心；
能力要改本地仓库、跑本地工具链 → 原生装。

---

## 2. 原生装

在控制台「节点」页点**注册节点**，会同时给你三条东西：Docker 命令、`node.yaml`、以及一条原生安装命令。第三条形如：

```bash
curl -fsSL https://raw.githubusercontent.com/zzjcool/anotify/main/scripts/install-agent.sh \
  | sh -s -- --token=nt_xxx --server=https://anotify.example.com
```

它做**七**件事，中间不问你任何问题：

1. 从 GitHub Release 下 `anotify-agent-<os>-<arch>`（先试 `latest`，可用 `ANOTIFY_VERSION=vX.Y.Z` 钉版本），
   并核对同名 `.sha256`——**校验不通过就中止**，不会装上一个来源不明的二进制
2. 装到 `/usr/local/bin`（需要 root 就脚本自己申请 sudo；不想给 sudo 就加 `--user`）
3. 写 `~/.anotify/node.yaml`（**权限 600**，里面有令牌）
4. **预检**：先以前台方式连一次中枢，`handshake.ok` 才继续。令牌错、中枢不通、TLS 不认
   都在这一步停下并说清原因——不会给你留下一个开机就崩溃重启的服务
5. 装成 systemd **系统**服务并以你的身份运行：开机自启、**没登录也活着**、挂了 3 秒重启
6. 起来后核对 `is-active`，打印节点/中枢/日志/升级/卸载
7. 清理上一版装法的残留（`~/.config/systemd/user/` 里的用户服务单元、`~/.local/bin` 里的旧副本）
   —— 同名的两份 agent 会抢同一个令牌，表现成两台机器互相踢线

跑完你只会看到一段结果，**没有"下一步"**：

```
anotify-agent 已装好并已启动
  节点      box-1
  中枢      https://anotify.example.com
  服务      anotify-agent：systemd 系统服务（开机自启；没登录也活着；挂了 3 秒后重启）
  状态      active (running)
```

预检是这里最要紧的一步：它让"装完即上线"变成一条**可验证**的断言，而不是祈祷。
跳过它没有任何好处，所以只在明确知道自己在干什么时才用 `--no-preflight`。

### 升级 / 卸载

```bash
# 升级（推荐）：只换二进制、保留 node.yaml 与 systemd 单元
anotify-agent upgrade --check     # 只看：当前 → 目标（退出码 0 有更新 / 1 已最新 / 2 服务端未启用）
anotify-agent upgrade --restart   # 换二进制并重启服务
# 或者：控制台「节点」页点「升级」（会替换二进制并重启 agent；在跑的任务等完，grace 20s）

# 兜底（首次安装 / 内网镜像 / 平台不支持自替换）：重跑同一条安装命令
curl -fsSL https://raw.githubusercontent.com/zzjcool/anotify/main/scripts/install-agent.sh | sh
```

自升级的前提是**服务端带了 agent 二进制**（镜像里的 `/app/agent`，可用 `ANOTIFY_AGENT_BIN` 改）。
没带时 CLI 退出码 `2`、接口回 `501` `agent_binary_unavailable`。

兜底路径保留的理由：首次安装本来就没有 agent 可以「自升级」，镜像不通的机器拿不到
中枢的二进制，容器只读 FS 的平台永远走不通自替换（`not_supported`）。
设计、退出码与排查表见 `docs/design/agent-upgrade.md`。

```bash
# 卸载（保留 ~/.anotify 数据目录）
sh install-agent.sh --uninstall
# 连产物缓存/工作目录一起删
sh install-agent.sh --uninstall --purge
```

### 没有 sudo 的机器

脚本会退化成 systemd **用户**服务，并尝试 `loginctl enable-linger`（这一步决定"没登录时还在不在"）。
两条都没成时它会明确告诉你缺什么，而不是假装装好了。容器里就更直接：

```bash
export ANOTIFY_RELEASE_BASE=https://your-mirror/anotify
curl -fsSL https://your-mirror/install-agent.sh \
  | sh -s -- --token=… --server=… --no-systemd   # 只装二进制+配置，前台跑
```

### 内网 / 镜像

下载走的是 GitHub。机器上不了外网时，把 release 资产（二进制 + `.sha256`）放到自己的
文件服务器上，然后：

```bash
export ANOTIFY_RELEASE_BASE=https://your-mirror/anotify    # 该目录下直接有 anotify-agent-linux-amd64 等文件
curl -fsSL https://your-mirror/install-agent.sh | sh -s -- --token=… --server=…
```

### 不用脚本也能装

```bash
# 1. 二进制
curl -fsSL -o /usr/local/bin/anotify-agent \
  https://github.com/zzjcool/anotify/releases/latest/download/anotify-agent-linux-amd64
chmod +x /usr/local/bin/anotify-agent

# 2. 配置
mkdir -p ~/.anotify && chmod 700 ~/.anotify
cat > ~/.anotify/node.yaml <<'YAML'
server: https://anotify.example.com
token: nt_xxx
instances: 2
YAML
chmod 600 ~/.anotify/node.yaml

# 3. 跑
anotify-agent -f ~/.anotify/node.yaml
```

### 三件必须知道的事

- **别用 root 跑**。agent 的执行权限 = 它的用户权限；`install-agent.sh` 默认拒绝 root，
  要强行也得显式 `--allow-root`。注意"装成系统服务"不等于"以 root 运行"：单元里写的是
  `User=<你>`，root 只用来注册服务（`/etc/systemd/system` + `systemctl enable`）。
- **令牌只显示一次**，且它会出现在命令里（所以会进 shell 历史）。介意的话把命令
  写进临时脚本执行，或执行完 `history -d`。丢了令牌就删节点重新注册。
- **配置可以进 git**（除了 `token`）。它是可审计的：谁改了什么一目了然。

---

## 3. 能力工作目录

工作目录由能力定义（`run.workDir`）决定，空 = 每次任务独立目录；节点侧不再有任何目录配置。

---

## 4. 常见问题

**Q：安装时的预检报 `unknown_capability`（本地缓存里有中枢不认识的产物）**
A：中枢上已经删除/改名过的能力还留在 `~/.anotify/index` 里，而 `hello` 会把本地缓存原样申报，
中枢对不认识的能力是**整条连接拒绝**。安装脚本会自己把 `index` 挪成 `index.stale-<时间戳>`
再重试一次（只挪不删，备份还在）；确认无碍后把备份删掉即可。手工装的场合自己执行
`rm -rf ~/.anotify/index` 也行。

**Q：预检报「这个令牌正被另一条连接用着」（superseded）**
A：同一份 `node.yaml` 被两个进程或两台机器用了。中枢的策略是新连接顶掉旧连接，于是两边对跳、
双方都跑不稳。先停掉旧的：`systemctl stop anotify-agent`（或 `systemctl --user stop anotify-agent`），再重跑安装。

**Q：升级报 `need_root` / `anotify-agent upgrade` 退出码 3，要不要换到 `~/.anotify/bin`？**
A：不要。agent 以普通用户跑，而 `/usr/local/bin/anotify-agent` 是 root 所有，写不进去就是没权限。
CLI 会打印可直接粘的 `sudo install -m 0755 <下载路径> <目标>`，或者直接 `sudo anotify-agent upgrade`。
**不静默降级到 `~/.anotify/bin`**：那会让 systemd 单元里的 `ExecStart` 继续指老路径，
机器上多了一份新二进制、进程跑的还是老的。

**Q：任务报了「能力 X@Y 声明的工作目录 /path 不存在」**
A：那个目录是**能力定义**里的 `run.workDir`，节点上没有这个目录。去控制台把该能力的
工作目录改掉，或者先在机器上把它建好。节点**不会**退回默认目录——静默退回只会让脚本
在一个空目录里报一个跟真实原因无关的错。

**Q：两个任务同时用同一个 workDir 会互相踩吗？**
A：会——它们共享同一个目录。给这类能力把节点的 `instances` 设成 1，或在能力脚本里自己做锁。

**Q：`script.test` 这类「执行任意命令」的能力还能用吗？**
A：能，这是预期用法（你信任的是**已部署的产物**）。但要记住：在原生装 + 声明了 `run.workDir`
的情况下，它是以**你的用户身份**在**那个目录**里跑任意命令。别把 `run.workDir` 配成 `/`。
