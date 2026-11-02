# 接入 Agent：原生安装与「能力工作目录」

本文回答两件事：

1. **原生装**怎么装（对比 Docker 的取舍）
2. **能力工作目录**（`workDir` / `workDirs`）是什么，为什么有白名单

---

## 1. 两条路，本质区别是「能看见什么」

| | Docker | 原生（本文） |
|---|---|---|
| 一行命令 | ✅ `docker run ...` | ✅ `curl ... \| sh -s -- ...` |
| 能看见宿主机文件 | ❌ 只有容器内（除非 `-v`） | ✅ 运行它的那个用户能看见什么，它就能看见什么 |
| 能在你的项目目录里跑 `make test` | 需要额外 `-v` 挂载 | 直接配 `workDirs` 即可 |
| 升级 | 拉新镜像重启 | 重跑安装脚本 |
| 隔离 | 容器边界（更强） | 进程边界（= 你的用户权限） |

**选型建议**：能力只做网络/API 类的事（发通知、调 webhook、跑测试镜像）→ Docker 更省心；
能力要改本地仓库、跑本地工具链 → 原生装。

---

## 2. 原生装

在控制台「节点」页点**注册节点**，会同时给你三条东西：Docker 命令、`node.yaml`、以及一条原生安装命令。第三条形如：

```bash
curl -fsSL https://raw.githubusercontent.com/zzjcool/anotify/main/scripts/install-agent.sh \
  | sh -s -- --token=nt_xxx --server=https://anotify.example.com \
             --work-dirs=/home/me/project
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
workDirs:
  - /home/me/project
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
- **配置可以进 git**（除了 `token`）。它是可审计的：谁改了 `workDirs` 一目了然。

---

## 3. 能力工作目录

### 它解决什么

v2 之后节点只跑**能力产物**，`cwd` 永远是 `<data-dir>/work/<task_id>`（一次任务一个临时目录），
替你把「产物文件」和「用户文件」隔开。但有些能力必须在**你自己的目录**里跑：

- `make test`、`npm run lint` —— 工具链的配置在仓库根
- `git status` / `git diff` —— 要读工作区
- 改完文件就地验证 —— 产物在临时目录里改，等于没改

### 怎么配

**控制台侧**（「能力」页 → 部署 → 工作目录）：

```
/home/me/project
```

留空 = 用默认的每任务临时目录（与加这个功能之前完全一致）。

**节点侧**（`~/.anotify/node.yaml`）—— 这是**授权**，不是配置：

```yaml
workDirs:
  - /home/me/project
  - /srv/apps
```

或环境变量（Docker）：`ANOTIFY_WORK_DIRS=/home/me/project:/srv/apps`。

### 语义与安全

| 事实 | 说明 |
|---|---|
| 匹配方式 | **前缀**，按路径分段：`/a/b` 放行 `/a/b` 与 `/a/b/c`，不放行 `/a/bc` |
| 符号链接 | 比对前**解链**（`ln -s /etc <allowed>/etc` 会被拒） |
| 越界 | 任务 **failed**，`error` 写明「不在本机的 workDirs 白名单内（允许：…）」——**不会**悄悄退回默认目录 |
| 空白名单 | 一个目录都不许：控制台指定 workDir 的能力在这台机器上必然失败 |
| 产物文件 | 仍然展开在 `<data-dir>/work/<task_id>/`，**不会**写进你的项目目录 |
| 环境变量 | `$ANOTIFY_WORKDIR` = 进程 cwd（历史不变式）；`$ANOTIFY_TASKDIR` = 产物所在的任务目录 |

设计立场（**别改**）：

> 中枢提出请求（`deployments.work_dir`），节点说了算（`node.yaml` 的 `workDirs`）。

中枢看不到、也不该看到节点文件系统。如果让中枢直接指定任意路径并生效，
那就等于「谁改了中枢，谁就能让本机以 agent 身份在任意目录读写」——原生装下这是整机沦陷。

越界时**失败而不是降级**也是刻意的：用户要的是「在这个项目里跑」，退回临时目录只会
产出一个看起来莫名其妙的错误（工具链找不到配置文件），而真正的原因被掩盖。

---

## 4. 常见问题

**Q：能力报了 `work_dir "…" 不在本机的 workDirs 白名单内`**
A：两种可能。① 节点没配 `workDirs`（或没重启加载）；② 配了但路径不完全一致（注意结尾斜杠、
软链解链后的真实路径）。用 `anotify-agent -f ...` 前台跑一次能看到启动日志里的配置。

**Q：Docker 里能用 workDir 吗？**
A：能，但必须**同时挂载**：`-e ANOTIFY_WORK_DIRS=/proj -v /proj:/proj`。
控制台生成的 Docker 命令在你填了工作目录时会自动带上这两段。只给 env 不给 `-v` 的症状是
「目录不存在」。

**Q：两个任务同时用同一个 workDir 会互相踩吗？**
A：会——它们共享同一个目录。给这类能力把节点的 `instances` 设成 1，或在能力脚本里自己做锁。

**Q：`script.test` 这类「执行任意命令」的能力还能用吗？**
A：能，这是预期用法（你信任的是**已部署的产物**）。但要记住：在原生装 + 指定目录的情况下，
它是以**你的用户身份**在**你授权的目录**里跑任意命令。别把 `workDirs` 配成 `/`。
