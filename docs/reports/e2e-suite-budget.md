# E2E 套件预算：#488 — 超时先留现场、预算按实测 ×2、套件级重试 1 次

snapshot-doc: 本报告的数字是**某次运行**的定格（每条都附复跑命令）；复跑以新输出为准。

- 基线：`origin/main` = `b85c6d48`（分支从 `141edd25` 起，已合入 main 的最新提交）
- 范围：只改 `scripts/e2e/**` + 本报告；没动产品代码、没动 `.github/workflows/`、没放宽任何断言
- 结论：`ui_shift` 的 300s 墙钟预算在 CI 上**从来没跑完过**（连续 5 次 run 全部
  `0通过/0失败 300.1s`）。根因是「固定墙钟 + 慢环境」+「超时不留现场」+「套件级没有重试」，
  三件事一起修；`known-env-failures.json` 的 `entries` 保持空数组（没有把 ui_shift 加回去）。

## 1. 事实（issue 的判据复述 + 本地基线）

| 位置 | 现象 |
| --- | --- |
| CI run `44674292518` 等 5 次 | `⏱️ ui_shift 0通过/0失败 300.1s`；判定「超过 300s 预算被 SIGKILL；没有 results/<suite>.json」 |
| 同一 run 的其他 28 个套件 | 全绿（`frontend 182.2s` / `capacity 147.8s` / `total 994.7s`，串行模式） |
| 本地同代码（本报告基线，`make fe` 后） | `ui_shift 87通过/0失败 187.2s`（issue 里是 145.4s；本机负载不同，机器差异本来就该由预算吸收） |

结论：`ui_shift` 不是「偶发红」，是每次都在同一个墙钟上被砍；而「没有 results」让 5 次失败
长得一模一样 —— 既分不清「慢」和「卡死」，也没法回答「跑了多少条、停在哪」。

## 2. A：超时必须留住现场（对称于 #485 对 case 的做法）

### 2.1 顺序：先落盘，再发信号

`scripts/e2e/lib/suite-budget.mjs` 的 `runSuiteAttempt()`：

```text
预算到点（不含宽限）
  ① 读进度快照 → 写 results/<suite>.json（timedOut/completedCases/lastCase/partial…）
  ② 才发 SIGTERM（套件有机会杀子进程、补写自己的 summary；f3 系会补一份完整计数）
  ③ 宽限 20s 还没退 → SIGKILL（整进程组；套件是 detached spawn，不会只杀 leader）
settle 之后
  ④ 若套件补写了 summary：保留它的 passed/failed（更全），把 timeout 元数据合并回去
  ⑤ 归档 attempts、写重试账本（见 §4）
```

为什么是 SIGTERM → 宽限 → SIGKILL 而不是直接 SIGKILL：`SIGKILL` 不可捕获，套件里那套
「写 results」的收尾（`H.summary` / crash hook）没有机会跑；先落 fragment 保证**信号发出时
现场已在盘上**，SIGTERM 只是让现场更完整，SIGKILL 是兜底（卡死的套件收不了尾）。

**顺序是可验证的**：`scripts/e2e/suite-budget-selftest.mjs` 的假套件在 SIGTERM handler 里
读回 `results/<suite>.json` 并断言 `timedOut === true` —— 落盘确实早于信号。

### 2.2 现场字段（`results/<suite>.json`）

```json
{
  "timedOut": true, "partial": true,
  "elapsedMs": 45241, "budgetMs": 45000,
  "completedCases": 10, "totalCases": 87,
  "lastCase": "grid-zh-CN-light-desktop-1", "lastCaseKind": "in-flight",
  "inFlight": [{ "case": "...", "elapsedMs": 220 }],
  "progressSource": "f3-evidence",
  "attempt": 2, "retried": true, "stuckSameCase": true,
  "attempts": [ { "attempt": 1, "kind": "timeout", "lastCase": "..." }, { "attempt": 2, ... } ]
}
```

- `completedCases` / `totalCases` / `lastCase` 来自**套件既有的逐 case 证据目录**（f3 的五个套件
  本来就把每个 case 的 attempt 记录与 `case.json` 写进 `fixtures/f3/screenshots/*/runs/<runId>/…`）。
  执行器只**读**它们（`lib/suite-progress.mjs`），不改任何套件逻辑；`F3_RUN_ID` 由执行器下发，
  所以能精确定位到本次 attempt 的运行目录。
- `lastCase` 的定义是「被杀时仍在跑、且**开始最早**的那条（跑得最久的）」——并发 2 时「最后一条
  开始」会随其它 case 完成而漂移，两次重试就永远比不出「是否停同一条」。
- 非 f3 套件拿不到 case 级证据时**不猜**：`completedCases: null`，退化到 stdout 的最后一条断言，
  并在 `progressSource` 里写明 `stdout-assertions`（汇总不会把它冒充成「N/M 条 case」）。

### 2.3 判定层：`timeout` 与 `incomplete` 分开，且不可用白名单容忍

- 执行器判分：`timeout`（有现场）≠ `incomplete`（整套没有 results）≠ `assertion`（断言不符）。
- CI 判定器 `scripts/e2e/ci-e2e-verdict.mjs` 新增一档：读 `timedOut:true` → 归 `timeout`，
  打印「已完成 N/M 条，停在 <case>；两次是否停同一条」，**`case: "*"` 条目不再能放过它**
  （`"*"` 只覆盖「整套没有 results」）。这正是 issue 里那条「白名单掩盖整套从来没跑完」的口子：
  修完之后 ui_shift 的超时会变成一条**可诊断的硬红**，而不是一条被容忍的 `0/0`。
- 对已知清单的口径：timeout 不属于「环境性失败」，只能靠预算/卡点修，所以它**不进**白名单
  （`known-env-failures.json` 的 `entries` 保持 `[]`）。

### 2.4 汇总打印

```text
  ⏱️ ui_shift           已完成 10/87 条  92.7s  mode=explicit-mock（timeout ×2：两次都停在同一条「grid-zh-CN-light-desktop-1」）
      ↳ a1 timeout 46.3s 预算 45s：已完成 13/87 条，停在「grid-zh-CN-light-desktop-1」
      ↳ a2 timeout 46.3s 预算 45s：已完成 10/87 条，停在「grid-zh-CN-light-desktop-1」

  套件级重试: 1 个（账本 .e2e-bin/results/_suite-retries/）：ui_shift timeout→timeout
```

判定行（`判分失败项`）里是同样的信息，供 `ci-e2e-verdict` 复核。

## 3. B：预算是数据驱动的

### 3.1 旧值为什么错

`ui_shift: 300_000` 是「本地 148-169s × 2」定的；但 CI runner 上没有 GPU、系统 Chrome 渲染矩阵
页面更慢，同一份代码在 runner 上的实际耗时 **大于 300s**（5 次 run 都恰好停在 300.1s，说明它
不是卡死，而是被墙钟砍在「还在跑」的路上 —— A 的现场会证明这一点）。

### 3.2 定值方法（先能跑完，再按实测收）

1. 先把 `ui_shift` 抬到 **900s**，跑一次 CI 拿真实耗时（这是本 PR 的第一轮 CI）。
2. 按「实测 ×2」定最终值，并把实测数字写进 `SUITE_TIMEOUT_OVERRIDE` 的注释与本报告。
3. `E2E_SUITE_BUDGET_MS` / `E2E_SUITE_BUDGET_<SUITE>` 只用于**诊断/注入**（例如把预算压到 45s
   逼出超时路径）；一旦使用，执行器启动横幅会显式打印「被 env 覆盖」，抬预算不许留静默。

**实测（CI）**：待本 PR 的第一轮 `ci / e2e（affected）` 结果回填 —— 见 §6 的 run 链接与
`ui_shift` 行。最终预算 = 实测 ×2（写死在 `SUITE_TIMEOUT_OVERRIDE.ui_shift`，并在注释里带 run id）。

### 3.3 预算水位可见

汇总新增一行「预算水位（>50% 预算）」，用**单次 attempt** 的耗时与预算比较（重试过的套件不会
拿两次累加去比一次预算）。下次预算再撞线时，日志里能直接看到「离撞线还有多远」。

## 4. C：套件级重试（与 case 级同策略）

| 层 | 判据 | 处置 |
| --- | --- | --- |
| `timeout` | 超墙钟预算（已落部分现场） | 自动重试 1 次 |
| `incomplete` | 整套没有 `results/<suite>.json`（崩溃 / 被 OOM / 输出为空） | 自动重试 1 次 |
| `assertion` | 有 results 且 `failed > 0` | **永不**重试 |
| `fail` | exit≠0 但断言全过（#338 口径：收尾/清理异常） | 不重试 |

- 每个 attempt 用独立的 `F3_RUN_ID`（`<suite>-<stamp>-a<n>`），重试不会污染上一次的证据；
  首次的 `results` 与 stdout/stderr 搬到 `_suite-retries/<suite>-a1.{json,log}`，
  账本 `_suite-retries/<suite>.json` 并排记两次的 `kind/elapsed/lastCase` 与 `stuckSameCase`。
- **两次都停同一条** = 确定性挂点（修卡点）；**停在不同条** = 抖动/预算太紧（修预算/环境）。
  两者都是机器可判的，不靠人读日志猜。
- 墙钟保护：重试会把整条 job 再拖一个预算。CI 的 `e2e` job 有 40min 上限，撞上它连 artifact 都
  传不上去（现场全丢）——所以有 `E2E_RETRY_WALL_LIMIT_MS`（默认 25min）：再重试一次就会撞上限时
  **不重试**，保留现场直接红，并在汇总里写明「未重试的理由」。这比赌整份证据丢掉更好。

## 5. 验收

### ① 注入「某 case sleep 到超预算」→ 现场 + timeout + 两次同一条

注入方式：**临时**在 `f3-ui-cases.mjs` 的 `runUiCase()` 入口加
`F3_INJECT_SLEEP_CASE=<case>:sleep(<ms>)` 钩子，跑完立即 `git checkout` 还原（交付 diff 里没有它）。

```text
$ E2E_SUITE_BUDGET_MS=45000 F3_INJECT_SLEEP_CASE=grid-zh-CN-light-desktop-1 \
  F3_INJECT_SLEEP_MS=600000 ./scripts/e2e/run_all.sh --serial ui_shift
套件墙钟预算（#488：实测×2）: ui_shift=45s（env E2E_SUITE_BUDGET_MS）
  ↻ ui_shift: 第 1 次 timeout（已完成 13/87 条，停在「grid-zh-CN-light-desktop-1」（仍在跑，最久的一条））→ 自动重试 1 次（证据搬到 .e2e-bin/results/_suite-retries/ui_shift-a1.json）
  ⏱️ 套件 ui_shift timeout（预算 45s，attempts=2）：已完成 10/87 条，停在「grid-zh-CN-light-desktop-1」（仍在跑，最久的一条）（timeout ×2：两次都停在同一条「grid-zh-CN-light-desktop-1」）
```

`results/ui_shift.json`（实测）：`timedOut:true`、`partial:true`、`elapsedMs:45241`、
`budgetMs:45000`、`completedCases:10`、`totalCases:87`、
`lastCase:"grid-zh-CN-light-desktop-1"`、`lastCaseKind:"in-flight"`、`retried:true`、
`stuckSameCase:true`、两次 attempt 的 `lastCase` 相同。

### ② `ui_shift` 在 CI 上真跑完并全绿（87 条）+ 新预算 + 实测耗时

见 §6（CI 回填）。

### ③ 其它套件时长/产物不膨胀

- 只有 `timeout` / 重试过的套件才多出产物：`_suite-retries/` 与带 `timedOut` 的结果文件；
  正常套件的 `results/<suite>.json` 形状与旧版本逐字一致（`retries` 账本只在重试/超时时落）。
- 正常路径只多了「一次 progress 快照的 stat 扫描」（超时时才做），与套件时长无关。
- 全部 29 个套件的预算除 `ui_shift` 外未改（本报告 §3 的定值只动 ui_shift）。

### ④ 门禁

- `flock /tmp/anotify-ci.lock -c "make ci"` → EXIT=0（含新增的 `#488 套件级自测`）
- `make e2e-affected` → 全绿（29/29，见 §6）

## 6. CI 回填

（本轮交付时回填：PR 号、两轮 `ci / e2e（affected）` 的 run id、`ui_shift` 行、最终预算。）

## 7. 边界与后续

- 本 PR 只做**套件级**：单个 case 在 runner 上死亡的重试（`incomplete` → 先取证再重试）属于
  #483/#484 的 case 层实现（PR #484，未合入时本分支的 known-env 也会清空；合并顺序由协调者定，
  两者语义不冲突：套件级重试吸收「整套死」，case 级重试吸收「单条死」）。
- `E2E_RETRY_WALL_LIMIT_MS` 的取舍：宁可「直接红 + 现场完整」，也不赌 job 超时丢 artifact。
  如果以后 CI job 的 `timeout-minutes` 上调，这个上限可以跟着放宽。
- 超时现场依赖 f3 系的证据目录布局（`lib/suite-progress.mjs` 里显式列出，并由合成树单测钉住）。
  新增 f3 套件时要同步这张表；非 f3 套件退化为「case 级进度不可得」，**不会**编造数字。
