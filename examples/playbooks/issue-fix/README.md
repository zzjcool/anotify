# issue-fix · PI 当执行者：判断成立 → 修 → review → 有环再修

投一个 issue 号，让 agent 把这件事推到「PR 通过 review」。和 `issue-solve` 的区别：
那条只**判断该不该动手**，这条**真的动手**。

```
POST /v1/hooks/<token>  {"payload":{"issue":649}}
        │  安装时填的 values.repo / values.repoDir
        ▼
   triage ──不成立──► gh issue close + record(closed)   ← 结束
        │成立
        ▼
   implement ──做不成──► record(stuck)                   ← 结束
        │ 提 PR
        ▼
   review ──通过──► record(approved)                     ← 结束
        │ 有问题
        ▼
     fix ──做不成──► record(stuck)                       ← 结束
        │ 改完推送
        ▼
   review ──第二轮通过──► record(approved)                    ← 结束
        │ 仍不通过
        ▼
     record(escalate)                                    ← 交回人
```

装一次 = 一条仓库的修复线（`repo` + `repoDir` 是安装参数），触发时只给 issue 号。

## 环是真的，就在图上

`review → fix → review` 是一条**真的回边**，平台允许（环是合法编排，由派发预算兜底：
深度或同 trace 任务数超限就 `pruned/max_hops` / `pruned/budget`，不是失败）。

但平台预算是最后一道闸（默认可跑很深），所以这条链自己带业务上限：`review` 看到
`round >= 2` 就不再回 `fix`，直接 `record(escalate)` 交回人。两轮的依据和我们对人的
要求一致（同一问题重试不超过 2 次）；**第三圈不该由平台来拦，而该由流程决定不该有**。

## agent 的契约：只认文件，不解析屏幕

每个能力都做三件事：把 prompt 交给 agent（`values.agentCmd` + `values.agentArgs`，默认 `pi -p`）、
等它退出、读它写的裁决文件。

```
$ANOTIFY_ARTIFACT_DIR/agent-verdict.json
triage     {"valid": true|false, "reason": "..."}
implement  {"ok": true|false, "pr": "...", "branch": "...", "reason": "..."}
review     {"approved": true|false, "findings": "逐条；通过写空串"}
fix        {"ok": true|false, "reason": "..."}
```

三条硬规则：

1. **没有裁决文件 = 任务失败**。脚本不猜、不把屏幕上的话当结论（`no parser on screen`）。
2. **agent 的完整输出留在 `agent.log`**，作为证据，不参与判断。
3. **不自动合并**：`review` 通过只是"这一环过了"，合并要人看（这条链到 `record(approved)` 就停）。

## 装之前，节点上要有什么

| 依赖 | 为什么 |
| --- | --- |
| `pi`（或你填进 `agentCmd` 的任何 agent） | 干活的是它；`values.agentArgs` 默认 `-p` 走非交互 |
| `git` + 已登录的 `gh` | 建分支、开 PR、`gh issue close/comment` |
| `values.repoDir` 里这个仓库的检出 | agent 在里面改；脚本 `cd` 进去，不自己 clone |
| 模型凭据（pi 自己的配置） | 否则 agent 起不来，`agent.log` 里会有真原因 |

## 失败语义（重要）

```
agent 没写裁决文件          → 任务失败（不猜结果）
gh issue close 失败         → triage 仍是成功、但 record 里记下（不吞）
emit 到不存在的下游         → 包校验期就会红（渲染后查 unreachable）
同一 issue 重复投递         → 会真的再跑一遍：这是"重投"不是"重试"，由人决定要不要投
```

## 已知边界

1. **`timeoutSec` 是整任务上限**：triage 1800s、implement/review/fix 3600s。agent 跑太久会被杀，
   现场留在 `agent.log` —— 这时应当改小任务范围，而不是调大超时。
2. **同一分支上追加提交**：`fix` 不 force push；如果 agent 硬推了共享分支，那是它违约，
   review 里要抓（这也是 review 能力存在的理由之一）。
3. **不做并发保护**：同一 issue 同时投两次会并行改同一个仓库。要防就串行触发，或者等
   `values` 里加一把锁（当前没有）。
4. **`roundLimit` 是保留字段**：当前固定两轮。要三轮就加一个 `review3` 能力并改 `fix` 的
   emit —— 环展开的代价是显式的，写在包结构里而不是藏在一个参数后面。
