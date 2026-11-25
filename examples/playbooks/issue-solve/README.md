# issue-solve · 手动投一个 issue 号，走完「判断 → 下单」

装好之后得到四行资源：**一个源 + 三个能力**。

```
POST /v1/hooks/<token>  {"repo":"zzjcool/anotify","issue":649}
        │  $(payload.repo / issue / note)
        ▼
  anotify-issue-claim
        ├─ 已关闭 / 是 PR / 已有 open PR / 已有疑似分支 ──► skip（留证据，不动手）
        ├─ 命中需裁决（label 或正文）───────────────► anotify-issue-gate（问题落盘，等人）
        └─ 可动手 ─────────────────────────────────► anotify-issue-dispatch（下单给执行体）
```

**这个包不做执行。** 它只回答"能不能动手、交给谁"，因为执行体需要 git/pi/凭据，
那是另一台机器的事。工单（`work-order.json`）里带的是判决与约束，不是流程。

## 装

```sh
# 1. 上传包（POST 整个目录，同 examples/playbooks/pr-triage-notify 的做法）
# 2. 安装，values 见 values.demo.json
# 3. 部署三个能力到至少一台节点（它们要能跑 curl）
# 4. GET 源，拿 status.hookUrl
```

## 手动触发

```sh
# 载荷要**包一层 "payload"**（与其它 webhook 源一致：外层的 payloadSchema 校验的就是这一层）
curl -fsS -X POST "$HOOK_URL" -H 'content-type: application/json' \
  --data-binary '{"payload":{"repo":"zzjcool/anotify","issue":649,"note":"生成产物只写不删"}}'
```

（这条是实测踩出来的：直接投 `{"repo":...}` 会被剪枝成 `event_schema: 载荷缺 repo, issue`，
因为服务端读的是 `payload` 这一层。见下方"实测记录"。）

触发一次 = 一条 trace + 一条源审计 + 一次能力回执。**这就是"手动触发"的全部**：
没有 GitHub webhook、没有 relay、没有 token 分发。

## 为什么用手动触发

自动化的价值不在"事件自己找上门"，而在"每次走同一套检查"。这个包把检查固化成代码：

| 判定 | 依据 | 对应真事故 |
| --- | --- | --- |
| `already_closed` | `state == closed` | 往已关闭的 issue 上白跑一轮 |
| `is_pull_request` | 载荷里有 `pull_request` | 把 PR 当 issue 处理 |
| `open_pr_exists` | open PR 里出现 `#N` | 同一件事开了两个 PR |
| `branch_exists` | 分支名里含该号 | 孤儿 worktree/分支复用出错，重复实现 |
| `needs_human` | label `needs-human`/`needs-decision` 或正文含"需裁决" | AI 替人选口径导致返工 |

## 实测记录（2026-11-25，dev 环境）

装到 dev（namespace `default`、release `issue-solve`、能力部署到节点 `hw`）后真触发：

| 投递 | 结果 |
| --- | --- |
| `#565`（已关闭） | `claim` succeeded → `verdict.json` = `skip / already_closed`，**没有**创建下游任务 |
| `#512`（OPEN） | `claim@2` succeeded → `emit` → `dispatch@2` succeeded，工单 POST 到接收器（`kind: issue-work-order`） |
| 投错形状（缺 `payload` 层） | `pruned: event_schema，载荷缺 repo, issue`（入口就拒，不进能力） |

过程里修掉两个真问题：

1. **PR 命中过宽**：原先「open PR 里出现 `#N` 就算已在处理」，把"顺带提到"误判成"声称关闭"。
   现在只认关闭关键字（`fixes/closes/resolves #N`）。
2. **`dispatchUrl` 的 pattern 只允许 `https://` 与 `http://localhost`**，导致本地演练填 `127.0.0.1` 被拒
   （部署在节点上时 `localhost` 指向的是节点自己）。已放宽到也允许 `127.0.0.1`。

## 已知边界（写在这里，而不是等踩了再说）

1. **匿名 API 有速率限制**：不配 `ghTokenFile` 时核心 60/小时、搜索 10/分钟。批量用请配 token 文件。
2. **分支判定是启发式**：只扫前 100 个分支，且按"名字含该号"匹配。它的作用是**提醒**，不是权威。
3. **`dispatch` 不自动重试**（`lifecycle.idempotent: false`）：下单失败会红，人重新触发一次。
   自动重试会造出第二张单，这就是重复派发的来源。
4. **gate 不会自己恢复**：人在 issue 上回答后要再触发一次；第二次 claim 会看到新事实。
