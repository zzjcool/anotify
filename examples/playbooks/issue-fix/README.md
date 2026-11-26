# issue-fix · PI 当执行者：判断成立 → 修 → review → 有环再修

投一个 issue 号，让 agent 把这件事推到「PR 通过 review」。和 `issue-solve` 的区别：
那条只**判断该不该动手**，这条**真的动手**。

```
POST /v1/hooks/<token>  {"payload":{"issue":649}}
        │  安装时填的 values.repo；仓库目录来自节点变量 REPO_DIR
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

装一次 = 一条仓库的修复线（`repo` 是安装参数），触发时只给 issue 号。

**仓库目录不是安装参数**：它走节点变量 `spec.vars.REPO_DIR`，每台节点配自己的路径。
理由在下一节——同一份 values 对所有节点生效，而各机器的路径本来就不同。

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
| 节点变量 `REPO_DIR` 指到的检出（见下节） | agent 在里面改；脚本 `cd` 进去，不自己 clone |
| 模型凭据（pi 自己的配置） | 否则 agent 起不来，`agent.log` 里会有真原因 |

## 每台节点自己配 `REPO_DIR`

机器路径不能进安装 values，根本原因是**两台机器的路径本来就不同**：服务用户是普通用户时
仓库在 `/home/<user>/…` 下，服务用户是 root 时在 `/root/…` 下。一个安装 values 对所有节点
生效，所以它要么逼你把两台机器的目录抄成一模一样（换用户 / 换 home 就崩），要么就写错一半。
写成节点变量之后，中枢在派发时按 (node, ns) 解析 local > node > namespace，各用各的：

| 节点 | 服务用户 | `spec.vars.REPO_DIR` |
| --- | --- | --- |
| `hw` | 普通用户（home `/home/zzjcool`） | `/home/zzjcool/code/anotify-agent` |
| `host-ws` | `root`（home `/root`） | `/root/code/anotify-agent` |

两行**不同**是常态，不是要抹平的差异。设置方法（`PATCH` 是 merge-patch + CAS，
`resourceVersion` 必填；省略的字段 = 不动）：

```bash
SERVER=https://anotify.example.com   # 换成你的中枢
COOKIE='<控制台会话 cookie>'        # 浏览器里那份，别写成脚本里的明文

# hw：普通用户，路径在它的 home 下
RV=$(curl -fsS -b "$COOKIE" "$SERVER/v1/nodes/hw" | jq -r .metadata.resourceVersion)
curl -fsS -X PATCH "$SERVER/v1/nodes/hw" -b "$COOKIE" \
  -H 'content-type: application/json' \
  -d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Node\",
       \"metadata\":{\"name\":\"hw\",\"resourceVersion\":\"$RV\"},
       \"spec\":{\"vars\":{\"REPO_DIR\":\"/home/zzjcool/code/anotify-agent\"}}}"

# host-ws：服务用户是 root，路径在 /root 下——**换一个值**，不要抄上面那条
RV=$(curl -fsS -b "$COOKIE" "$SERVER/v1/nodes/host-ws" | jq -r .metadata.resourceVersion)
curl -fsS -X PATCH "$SERVER/v1/nodes/host-ws" -b "$COOKIE" \
  -H 'content-type: application/json' \
  -d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Node\",
       \"metadata\":{\"name\":\"host-ws\",\"resourceVersion\":\"$RV\"},
       \"spec\":{\"vars\":{\"REPO_DIR\":\"/root/code/anotify-agent\"}}}"
```

多台一起改用 `POST /v1/nodes/config/batch`（逐台 CAS、逐台给结论，失败只影响那一台）。

**变量名是 `REPO_DIR`，不是 `repoDir`**：`spec.vars` 的键必须匹配 `^[A-Z][A-Z0-9_]{0,63}$`
（`internal/store/config.go` 的 `ValidateVars`），小写键会被 400 `value_invalid` 拒绝。

**缺配不会退到任何默认目录**（没有默认值、没有兜底、没有"两处都试一遍"的探测）：

- 派发期：`spec.requires.values: [vars.REPO_DIR]` 解析不出来 → 该节点对这个能力是
  `ValueMissing`，任务不派给它（`EligibleNodes`）；所有节点都缺 → 任务挂着等人补配置。
- 执行期：`run.env.REPO_DIR` 只来自中枢解析出的节点变量，缺失就不会有 `$REPO_DIR`，
  脚本第一行 `${REPO_DIR:?该节点没配节点变量 REPO_DIR}` 立即失败并说明原因。

**为什么不顺手写成 `run.workDir: $(vars.REPO_DIR)`**：`run.files` 会展开进 workDir，
`run.sh` 就会落进用户的 git 检出、并参与 `git status`——而这条链的 agent 正是在这个仓库里
提 PR。所以保持"默认临时目录 + 脚本自己 `cd "$REPO_DIR"`"，目录来源还是一个值。

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
