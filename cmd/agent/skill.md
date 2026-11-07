# anotify-agent manual ({{VERSION}})

你是 `anotify-agent`：Anotify 的**节点执行器**，跑在用户自己的机器上，替中枢
（control plane）在本机执行能力、并把产出上报回去。中枢决定「做什么」，本二进制
决定「在本机怎么跑」。

## 先分清三个东西

| 名字 | 是什么 | 你怎么用 |
|---|---|---|
| `anotify-agent` | **本二进制**：节点执行器，有子命令 | 就是你手里这个，`-h` 看命令表 |
| `anotify` | **服务端**（中枢/控制台），纯 HTTP，**没有子命令** | 浏览器或 curl 访问，别在它身上找 CLI |
| herdr | 另一个工具（终端复用 / agent 编排），与 Anotify 无关 | 参数不要混用 |

三者只共享「把结果告诉人」这件事，接口互不兼容。

## 1. 起一个节点（运维）

节点配置只有四个键：`name` / `server` / `token` / `instances`。多写一个键 agent 直接
拒绝启动（未知字段），这比「悄悄忽略」安全。

```bash
# 前台跑（排障时用；Ctrl-C 优雅退出）
anotify-agent -f ~/.anotify/node.yaml

# 不给 -f 时的回落顺序：显式 -f > ANOTIFY_TOKEN 等环境变量 > ./node.yaml
anotify-agent -f ./node.yaml -grace 20 --data-dir ~/.anotify
```

`--data-dir`（缺省 `~/.anotify`）下面放着这个节点的全部本地状态：
产物缓存与索引、`work/<task_id>/`（每次执行的工作目录）、`runs/`（执行档案）、
`emit/`（本机 socket）。它是**绝对路径**语义，别传相对路径（会随 cwd 漂移）。

装成服务以后就别再手动起了（两个进程会抢同一个节点令牌，表现成互相踢线）：

```bash
systemctl status anotify-agent          # 安装脚本装的是系统服务，开机自启
anotify-agent upgrade --check           # 有更新？0 有 / 1 已最新 / 2 服务端没带二进制
anotify-agent upgrade --restart         # 换二进制并重启服务（需要 root 改系统目录）
```

日志一律写 stderr —— 装成服务后就在 journal 里（用 journalctl 跟这个单元），stdout
永远留给脚本的产出。

## 2. 写能力与上报（能力作者）

能力是**控制台编译出来的产物**（YAML，内容寻址）。写完先**校验**、再**本地跑**——
顺序别反：`run` 会真的执行入口文件，`check` 不会。

```bash
anotify-agent check ./capability.yaml          # 只解析 + 校验：一次列出全部问题；0 全过 / 1 有问题
anotify-agent check ./capability.yaml --json   # {"ok":false,"problems":[{"code","message"}]}
anotify-agent schema capability                  # 能力 yaml 的 JSON Schema（check 校验的就是它）
```

`check` 的三条边界就是它与 `run` 的区别：**不执行、不连中枢、不落盘**。`--json` 的
`code` 是 `invalid_name` / `entrypoint_missing` / `invalid_output` 这类短码（与 `run`
报错时同一套词表），所以「哪里错了」可以只读 JSON、不必正则中文。`schema` 另有两份
形状：`anotify-agent schema node`（node.yaml 的四个键）与 `anotify-agent schema emit`
（出口契约 `$ANOTIFY_EMIT_PORTS` 的形状）。

然后才是真正跑一遍（本机、临时工作目录、退出码透传产物自己）：

```bash
anotify-agent run ./capability.yaml -p repo=acme/x -p pr=42 --timeout 600
```

`run` 不连中枢、不占节点连接、不读写 `~/.anotify` 的缓存：它把产物展开到一个临时
工作目录里、直接 exec 入口文件（**不经 shell**，参数只走环境变量）。
`anotify-agent run` 的 `-p` 值一律当字符串（`-p code=007` 就是 `"007"`），退出码原样透传产物自己。

脚本里拿得到的东西：

| 环境变量 | 含义 |
|---|---|
| `ANOTIFY_PARAMS` | 全部参数的 JSON（`anotify-agent run -p k=v` 给的，或中枢下发） |
| `ANOTIFY_PARAM_<NAME>` | 单个参数，名字大写（`repo` → `ANOTIFY_PARAM_REPO`） |
| `ANOTIFY_CAPABILITY` / `ANOTIFY_DIGEST` | 我是谁、跑的是哪一版（内容寻址） |
| `ANOTIFY_WORKDIR` / `ANOTIFY_TASKDIR` | 工作目录 / 任务目录（产物文件、`emit.json`） |
| `ANOTIFY_EMIT_SOCK` | 上报用的本地 socket；**没有它 = 本地 standalone 跑**，跳过上报 |
| `ANOTIFY_TASK_TOKEN` | 本次任务的一次性令牌（socket 鉴权用，不是中枢凭证） |
| `ANOTIFY_AGENT_BIN` | agent 自己的绝对路径，用它调 emit，不必假设它在 PATH 上 |

中途产出只有一个通道：**emit**（没有 `.anotify-out`、没有 `$ANOTIFY_OUT_FILE`，
那些已退役，写了没人读）。

```bash
# 交给哪个下游：anotify-agent emit --to <能力名> 或 <能力名>@<版本>；单出口且候选唯一的可以省略
anotify-agent emit --to notify.phone --data '{"issue":"#42","title":"空指针"}'
anotify-agent emit --to notify.phone --data @- < payload.json   # 从 stdin 读
anotify-agent emit --to notify.phone --data '…' --note '第 2 项' # 人类可读备注
anotify-agent emit --to notify.phone --data '…' --wait-routed    # 等中枢派发完再返回
```

出口长什么样由 agent 交给脚本：`$ANOTIFY_EMIT_PORTS`（环境变量）与
`$ANOTIFY_TASKDIR/emit.json`（同一份字节，0600）。里面是 `port` / `pick` / `schema` /
`targets[].needs` —— 「我不点名时 data 要满足什么」和「我点名这一个时它还要什么」
是**两个**门槛，按它拼字段就不会被拒。一次都不 emit 是合法的（终端能力）。

## 3. 排障（runs / show）

每次执行都落在**跑它的那台机器上**（params 是用户的真实输入，不传中枢）：

```
<data-dir>/runs/<trace_id>/<capability>@<version>/<seqInTrace>-a<attempts>/
    input.json    traceId / taskId / 能力 / 版本 / params / workdir
    stdout.log    完整 stdout（单流上限 8 MiB）
    stderr.log    完整 stderr
    meta.json     起止时间、exitCode、status、emit 次数
```

```bash
anotify-agent runs                        # 最近 20 条，时间倒序
anotify-agent runs --trace trc_0af7651916cd43dd8448eb211c80319c   # 一条 trace 的全部执行
anotify-agent runs --trace trc_0af76519                            # 唯一前缀也行（最少 8 位）
anotify-agent runs --capability notify.phone@2                      # 这个能力的历次执行
anotify-agent runs --limit 0                                       # 不限条数
anotify-agent show trc_0af7651916cd43dd8448eb211c80319c/notify.phone@2/3-a1
anotify-agent show trc_0af76519/notify.phone@2/3-a1 --tail 50           # 只看最后 50 行
anotify-agent show trc_0af76519/notify.phone@2/3-a1 --no-stdout         # 只看元信息与输入
```

表格里 **TRACE 只打前 8 位**（完整 id 在 `--json` 里）：agent 的痛感来自 token，
不来自屏幕。`anotify-agent runs --trace` 与 show 的 ref 第一段都接受**唯一前缀**
（最少 8 字符）；撞车时**不猜**：退 1、`kind` 是 `ambiguous_ref`、stderr 列候选，
且**不输出任何日志正文**。

`show` 的 ref 直接从 `runs` 表里抄（TRACE / CAPABILITY / SEQ / ATT 四列）。找到 = 0，
空结果或 ref 不存在 = 1 —— 「没找到」不是崩溃，但通常意味着目录或 id 写错了。
单流上限 8 MiB，所以窗口粒度是必需品：`anotify-agent show --tail N --no-stdout --no-stderr`
只给你要看的那一段，元信息不受影响。

### 机器可读输出（给 agent 与脚本）

stdout **不是终端**（管道 / 重定向）时自动输出 JSON，TTY 上保持上面的表格；
`--json` 可以显式要。五种形状：

| 命令 | `--json` 形状 |
|---|---|
| `runs` | `{"runs":[{ref + meta.json 全字段}]}`（空结果也是 `{"runs":[]}`，退出码仍 1） |
| `show` | `{"ref","meta":{…},"input":{…},"stdout","stderr"}` |
| `upgrade` | `{"current","latest","updated","restarted","reason"}` |
| `emit` | `{"ok":true,"port","routed","note"}`（`routed` = 中枢已确认派发；默认只等到本地落盘） |
| `check` | `{"ok":bool,"problems":[{"code","message"}]}`（没问题时 `problems` 是 `[]`，退出码 0） |

```bash
anotify-agent runs --json | jq '.runs | type == "array"'
anotify-agent runs --json | jq '.runs[0].ref'
anotify-agent show <ref> --json --tail 50
anotify-agent upgrade --check --json | jq '.updated, .restarted'
```

失败时 stderr 是**单行** `{"error":{"kind","message","hint"}}`：`kind` 与退出码对应
（见 §4 的 kind 列），`hint` 是下一步（可重试 / 别重试 / 去要 root）。TTY 上仍然是那句
中文，不牺牲人的体验。**空结果与出错要分着看**：`runs` 空是 stdout 上 `{"runs":[]}` +
退出码 1，真正出错时 stdout 什么都没有。
执行失败或超时留下的人肉现场在 `<data-dir>/work/<task_id>/`（agent 会打一行
「工作目录已保留」）。保留策略：`ANOTIFY_RUNS_RETENTION_DAYS`（默认 7 天）与
`ANOTIFY_RUNS_MAX_COUNT`（默认 2000）。

## 4. 退出码表

各子命令的退出码**不是同一张表**（同一个数字含义不同），别混着读。
`kind` 是管道 / `--json` 下 `{"error":{"kind",…}}` 里那个短码 —— 脚本只该 switch 它，
不要去正则中文。

| 命令 | 码 | 含义 | `kind` |
|---|---|---|---|
| 节点模式（不带子命令） | 0 / 1 | 正常退出 / 参数、配置、握手、连接失败（重启交给进程管理器） | — |
| `run` | 产物自己的码 / 1 | 脚本怎么退就怎么退 / 只有 agent 侧故障（产物不合法、起不来）才用 1 | artifact 短码：`invalid_yaml` / `unknown_field` / `invalid_name` / `unsupported_api_version` … |
| `emit` | 0 | 成功（已本地落盘；`--wait-routed` 时已确认派发） | — |
| `emit` | 2 | 参数/请求形状错（不是 JSON 对象、超体积）→ 修，别重试 | `bad_request` |
| `emit` | 3 | 被拒：`--to` 或端口不在本能力的出口里 → 修，别重试 | `port_not_allowed` |
| `emit` | 4 | 被拒：data 不满足要去那个下游的要求 → 补齐字段后**可以**重发 | `schema_mismatch` |
| `emit` | 5 | 任务已非运行态（cancel / 租约过期 / 已终结）→ **停止执行** | `task_gone` |
| `emit` | 6 | 通信失败 / token 无效 / socket 不存在 → 可重试一次，仍失败就停 | `comm_failed` |
| `emit` | 7 | ack 超时 → 可重试（中枢按 (task_id, seq) 幂等） | `ack_timeout` |
| `upgrade` | 0 | 成功（`--check` 时表示有更新） | — |
| `upgrade` | 1 | 已是最新（仅 `--check`；**不是错误**，stderr 是空的） | — |
| `upgrade` | 2 | 服务端没带 agent 二进制（501）→ 别重试，去重跑安装脚本 | `unavailable` |
| `upgrade` | 3 | 权限不足 → 用 root 再来（message 里有能直接粘的一行） | `need_root` |
| `upgrade` | 4 | sha256 不符 → 别原样重试（传输或服务端文件有问题） | `bad_sha` |
| `upgrade` | 5 | 网络/其他错误（含参数、配置）→ 可以原样重跑 | `failed` / `config_missing` / `config_invalid` |
| `runs` / `show` | 0 / 1 | 找到 / 没找到或参数非法（**不新增第三档**：歧义也走 1） | `not_found` / `ambiguous_ref` / `usage` / `config_missing` / `config_invalid` |
| `check` | 0 / 1 | 全过 / 产物有问题（stdout 一次列出全部问题，**不执行**）；命令自身出错也退 1 | 产物有问题这一档没有 kind；文件读不到是 `config_missing` / `config_invalid`，参数错是 `usage` |
| `schema` | 0 / 1 | 已输出一份 JSON Schema / schema 名不认识或多余参数 | `usage` |

## 5. 不要做

1. **别把 `run` 当校验器**：它会**真的执行**入口文件。只想看 yaml 对不对，用 `check`
   （只 Parse + Validate，不执行、不连中枢、不落盘）。写完先 `check`，再 `schema`
   对字段，最后才 `run`。
2. **别把「不带子命令」当查询**：那是常驻进程，会连中枢、占掉一个节点连接。
   查本机状态请用 `runs` / `show`。
3. **别指望 `-grace` 关连接**：它只算在跑任务的收尾，不含关闭连接本身；总耗时可能比
   grace 略长。它应当小于进程管理器的 stop 超时（`docker stop` 默认只有 10s），
   否则来不及收尾就被 SIGKILL。
4. **`emit` 退出码 5 时停止重试**：任务已非运行态，重发只会一直拿到 5。退出码 4 才是
   「补齐字段后可以重发」。
5. **别把中枢凭证交给能力脚本**：脚本只能通过 `ANOTIFY_EMIT_SOCK` 说服本机 agent
   代发一帧，路由权永远在中枢。同理，`ANOTIFY_TASK_TOKEN` 是单次任务的，别存下来复用。
6. **别无脑重试 `emit` 的 2 / 3**：参数错、出口不在契约里，重试一万次也一样。
7. **别在已经装成服务的机器上手工起第二个节点**：两个进程抢同一个令牌，表现是互相踢线。
8. **别把表里的 8 位前缀当完整 id 存起来**：前缀只是给人省 token 的；要用就先看 `runs --json`
   的完整 id，否则换台机器就可能撞上前缀（`ambiguous_ref`，退 1）。
