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
| 升级 | 拉新镜像重启 | 重跑安装脚本 |
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

它做四件事：

1. 从 GitHub Release 下 `anotify-agent-<os>-<arch>`（先试 `latest`，可用 `ANOTIFY_VERSION=vX.Y.Z` 钉版本），
   并核对同名 `.sha256`——**校验不通过就中止**，不会装上一个来源不明的二进制
2. 装到 `/usr/local/bin`（不可写则退到 `~/.local/bin` 并提示 PATH）
3. 写 `~/.anotify/node.yaml`（**权限 600**，里面有令牌）
4. 生成 systemd **用户**服务 `~/.config/systemd/user/anotify-agent.service`

随后：

```bash
systemctl --user enable --now anotify-agent
journalctl --user -u anotify-agent -f
# 没登录也要活着（可选）
sudo loginctl enable-linger "$USER"
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
  要强行也得显式 `--allow-root`。用普通用户 + systemd 用户服务就够了。
- **令牌只显示一次**，且它会出现在命令里（所以会进 shell 历史）。介意的话把命令
  写进临时脚本执行，或执行完 `history -d`。丢了令牌就删节点重新注册。
- **配置可以进 git**（除了 `token`）。它是可审计的：谁改了什么一目了然。

---

## 3. 能力工作目录

工作目录由能力定义（`run.workDir`）决定，空 = 每次任务独立目录；节点侧不再有任何目录配置。

---

## 4. 常见问题

**Q：任务报了「能力 X@Y 声明的工作目录 /path 不存在」**
A：那个目录是**能力定义**里的 `run.workDir`，节点上没有这个目录。去控制台把该能力的
工作目录改掉，或者先在机器上把它建好。节点**不会**退回默认目录——静默退回只会让脚本
在一个空目录里报一个跟真实原因无关的错。

**Q：两个任务同时用同一个 workDir 会互相踩吗？**
A：会——它们共享同一个目录。给这类能力把节点的 `instances` 设成 1，或在能力脚本里自己做锁。

**Q：`script.test` 这类「执行任意命令」的能力还能用吗？**
A：能，这是预期用法（你信任的是**已部署的产物**）。但要记住：在原生装 + 声明了 `run.workDir`
的情况下，它是以**你的用户身份**在**那个目录**里跑任意命令。别把 `run.workDir` 配成 `/`。
