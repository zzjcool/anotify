# issue-solve · 手动投一个 issue 号，走完「判断 → 下单」

装一次 = **一条仓库的收单线**：安装时填仓库名，之后每次触发只给 issue 号。

```
POST /v1/hooks/<token>  {"payload":{"issue":649}}
        │  安装时的 values.repo + 触发时的 $(payload.issue)
        ▼
  issue-claim
        ├─ 已关闭 / 是 PR / 已有 open PR / 已有疑似分支 ──► skip（留证据，不动手）
        ├─ 命中需裁决（label 或正文）───────────────► issue-gate（问题落盘，等人）
        └─ 可动手 ─────────────────────────────────► issue-dispatch（工单 POST 给执行体）
```

装好之后是**四行资源**：一个源 + 三个能力（名字都由 `values.project` 决定，装 N 次互不影响）。

**这个包不做执行。** 它回答的是"这个 issue 现在该不该动手、交给谁"；真正的干活需要 git/pi/凭据，
属于执行体那台机器。工单（`work-order.json`）里带的是判决与约束，不装流程。

## 两边参数怎么分

| 参数 | 在哪填 | 为什么 |
| --- | --- | --- |
| `values.repo`（`owner/name`） | **安装时** | 一条收单线服务一个仓库；换仓库就再装一次 |
| `values.dispatchUrl` | 安装时 | 工单往哪送（执行体入口） |
| `payload.issue` | **每次触发** | 这才是手动触发要说的事：今天做哪个 |
| `payload.note` | 可选 | 触发人的一句话意图，会进工单 |

## 装

```sh
# 1. 上传包（POST 整个目录：playbook.yaml + capabilities/ + sources/）
# 2. 安装，values 形如 values.demo.json（repo 就是仓库名）
# 3. 把三个能力部署到至少一台节点（它们要能跑 curl）
# 4. GET 这个源，status.hookUrl 就是触发地址
```

## 手动触发

```sh
# 载荷要**包一层 payload**：外层 payloadSchema 校验的就是这一层
curl -fsS -X POST "$HOOK_URL" -H 'content-type: application/json' \
  --data-binary '{"payload":{"issue":649,"note":"生成产物只写不删"}}'
```

（实测踩出来的：直接投 `{"issue":649}` 会被剪枝成 `event_schema: 载荷缺 issue`，
因为服务端读的是 `payload` 这一层。）

## 为什么用手动触发而不是 GitHub webhook

自动化的价值不在"事件自己找上门"，而在"每次走同一套检查"。这个包把检查固化成代码：

| 判定 | 依据 | 对应真事故 |
| --- | --- | --- |
| `already_closed` | `state == closed` | 往已关闭的 issue 上白跑一轮 |
| `is_pull_request` | 载荷里有 `pull_request` | 把 PR 当 issue 处理 |
| `open_pr_exists` | open PR **声称**关闭它（`fixes/closes/resolves #N`） | 同一件事开了两个 PR |
| `branch_exists` | 分支名里含该号 | 孤儿 worktree/分支复用出错，重复实现 |
| `needs_human` | label `needs-human`/`needs-decision` 或正文含"需裁决" | AI 替人选口径导致返工 |

`open_pr_exists` 只认关闭关键字，不认"顺带提到" —— 提到 `#N` 的 PR 满地都是（讨论、引用、依赖），
把它们算成"已经在处理"会误拦掉真正该做的活（这条是实测发现的，详见下）。

## 实测记录（2026-11-25，dev 环境）

装到 dev（namespace `default`、release `issue-solve`、能力部署到节点 `hw`）后真触发：

| 投递 | 结果 |
| --- | --- |
| `#565`（已关闭） | `claim` succeeded → `verdict.json` = `skip / already_closed`，**没有**创建下游任务 |
| `#512`（OPEN） | `claim` succeeded → `emit` → `dispatch` succeeded，工单 POST 到接收器（`kind: issue-work-order`） |
| 投错形状（缺 `payload` 层） | `pruned: event_schema，载荷缺 issue`（入口就拒，不进能力） |

过程里修掉三个真问题：

1. **PR 命中过宽**（见上）：从"出现 `#N`"收紧到"声称关闭 `#N`"。
2. **`dispatchUrl` 的 pattern 只允许 `https://` 与 `http://localhost`**：节点上跑时 `localhost` 指的是
   节点自己，本地演练填 `127.0.0.1` 会被 400 拒。已放宽到也允许 `127.0.0.1`。
3. **`repo` 曾经要从载荷传**：现在挪到安装参数——一个安装一条收单线，触发时只说 issue 号。

## 已知边界（写在这里，而不是等踩了再说）

1. **匿名 API 有速率限制**：不配 `ghTokenFile` 时核心 60/小时、搜索 10/分钟。批量用请配 token 文件。
2. **分支判定是启发式**：只扫前 100 个分支，按"名字含该号"匹配。作用是**提醒**，不是权威。
3. **`dispatch` 不自动重试**（`lifecycle.idempotent: false`）：下单失败会红，人重新触发一次。
   自动重试会造出第二张单，那正是重复派发的来源。
4. **gate 不会自己恢复**：人在 issue 上回答后要再触发一次；第二次 claim 会看到新事实。
