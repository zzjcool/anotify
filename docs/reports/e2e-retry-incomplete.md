# E2E：#483 case 子进程死亡不再算「断言失败」——incomplete 先取证、再重试 1 次；断言永不重试

snapshot-doc: 2026-11-20 本机交付快照（下文注入输出、门禁数字都是当次运行的定格；每处都附了复跑命令，复跑以新输出为准）

- 基线：`origin/main` = `141edd25`；分支 `pi-subagent/hw-nj/anotify-e2e-retry-91d4ffa4`（独立 worktree）
- 结论：把「用例没跑完」与「断言不符」拆成两层；前者**先取证、再自动重试 1 次**，后者**永不**重试。
  `known-env-failures.json` 的两条环境性登记随之删除（`entries` 现在是空数组）。
- 没动产品代码，没放宽任何断言，没改 `make ci` 的其它门。
- 重试是治标，所以同时把它做成**取证器**：每个 incomplete 都留 dmesg / 内存 / 磁盘 /
  chromium 足迹 / 该 case 自己的输出 / case 序号（见 §4），下一次失败自带根因线索。

## 1. 现象与根因分层（复述 #483）

runner 上 case 子进程会偶发死亡：`completed`/`case.json` 没落盘、子进程输出为空、渲染进程
整批被关。旧判定只看「结果文件在不在 + 断言过没过」，于是同一个环境故障换一个 case 长出来
就多一条红 —— 按 case 登记 known-env 永远追不上（`source-actions` 登记之后又长出了
`observe-events` / `node-create` / `interact-dialogs`）。

分层之后：

| 层 | 判据 | 处置 |
| --- | --- | --- |
| `incomplete` | 结果文件缺失 / 子进程输出为空 / 子进程非 0（含信号、超预算）且没有 fragment | 先取证 → 自动重试 1 次 |
| `assertion` | 拿到了 fragment，且失败是断言不符（含 `AssertionError`） | 永不重试 |

## 2. 判据的唯一实现

新增 `scripts/e2e/fixtures/f3/f3-retry.mjs`，三个 f3 执行器（`frontend` / ui 系 / i18n 系）
共用同一套口径，不在调用点另发明判据：

- `classifyCaseEvidence({fragment, fragmentError, exit, output})` →
  `pass` / `incomplete` / `assertion`（纯函数）。
- **死亡签名优先于 AssertionError**：用例常把 `Target page…closed` 包进
  `assert(report.errors 为空)` 里，堆栈同时带 `AssertionError`；那是执行层死亡，不是断言不符。
  所以顺序是：无 fragment → incomplete；有 fragment 且 `ok=false` → 先匹配
  浏览器/传输/进程死亡签名（incomplete），否则才是 assertion。
- `fragment.ok=true` 但子进程没收尾（exit≠0 / 信号）**不重试**：fragment 是独占证据，
  重跑既不能覆盖它，也不能把「进程没正常收尾」算成通过 —— 判红并保留证据。

## 3. 重试怎么发生（重试不删证据）

`executeWithRetry()` 在**同一个 attempt 身份**下再跑一次（epoch 清单不变：每条 case 仍然
只有一个已登记 attempt，重试不能覆盖任何已落盘的结论）。首次 incomplete 的
`case.json` / `exit.json` / `full.log` 被搬到 `<case dir>/retry-1/`，判定记录写
`<case dir>/retry.json`（两条 attempt 的 kind/why/exit 事实）。所以「重试过」本身是可复核的
证据，不是把失败抹掉。

## 4. 首次 incomplete：**先取证，再重试**（治标同时做取证器）

每个 incomplete 在重试前同步采一份现场（单条命令 3s 上限，采集失败不影响判定，只记
`ok:false` + 原因）：

1. `dmesg` 尾部 40 行（内核 OOM kill 会在这里留痕；普通 CI 日志里看不到）。
   dmesg 在受限 runner 上对非 root 关闭（`kernel.dmesg_restrict=1`），所以同时记一份
   **不需要权限**的 `/proc/vmstat` 的 `oom_kill` 累计计数器 —— 同一台 runner 上失败前后
   从 0 变成非 0，就是一次真 OOM 的内核侧证据。
2. `free -m` 与 `df -h /tmp`（tmpfs 满了会以各种奇怪方式表现为「进程没了」）。
3. chromium/chrome 进程数与总 RSS（`ps -eo rss,comm`，按 `comm` 含 `chrom` 统计）。
4. 该 case **自己的 stdout+stderr**（头 4KB + 尾 4KB；**为空也要写「为空」这个事实**）。
5. 它是本套件第几个 case（`#N/M`）与第几次 attempt —— 序号能判断是否「越跑越死」。

落盘（JSON 给机器读、TXT 给人读）：

```text
<results>/_incomplete-forensics/<suite>-<case>-a<attempt>.{json,txt}
例：.e2e-bin/results/_incomplete-forensics/frontend-source-actions-a1.txt
```

`<results>` 就是 `E2E_RESULTS_DIR`（默认 `.e2e-bin/results`），而 CI 的 e2e artifact
已经整目录上传 `.e2e-bin/results`（`.github/workflows/ci.yml` 的 `Upload e2e evidence`），
所以取证文件**天然随 artifact 走**，不需要改 workflow。

汇总里打印（套件 stdout 与 `parallel_runner` 汇总同形，路径 + 逐条摘要，零重试不打印取证段）：

```text
[frontend] 重试: 1 条首次 incomplete，1 条重试通过，现场取证: .e2e-bin/results/_incomplete-forensics/
  1) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用（dmesg: 读取内核缓冲区失败: 不允许的操作）)  oom_kill=0  取证: .e2e-bin/results/_incomplete-forensics/frontend-source-actions-a1.txt

  ✅ frontend           1通过/0失败（其中 1 条首次 incomplete、1 条重试通过；现场取证: .e2e-bin/results/_incomplete-forensics/）  11.4s  mode=real
      1) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用…)  oom_kill=0  取证: …/frontend-source-actions-a1.txt
```

（本机非 root，`dmesg` 读不到内核缓冲区 —— 这个「不可用」本身也是要留下的事实；
在 GitHub runner 上 `dmesg` 可读，OOM 会以 `Out of memory: Killed process …` 出现。）

## 5. 重试计数怎么打印（不许静默）

三处，零重试也打印：

1. 套件 stdout：`[frontend] 重试: 1 条首次 incomplete，1 条重试通过`
   （零重试：`[frontend] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）`）。
2. 结果 JSON：`retries` 字段（`H.setRetryStats` 写；`scripts/e2e/lib/harness.mjs`），
   含 `forensics[]` 摘要；只有 f3 系五个套件会写，其余套件结果形状不变。
3. 并行汇总（`scripts/e2e/parallel_runner.mjs`）：
   `✅ frontend 1通过/0失败（其中 1 条首次 incomplete、1 条重试通过；现场取证: …）` +
   逐条取证摘要 + 全局一行 `重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete N 条 · 重试通过 M · 重试仍 incomplete K（…；现场取证: …）`。

## 6. known-env：删掉两条（entries 已空）

删除：

- `frontend` / `source-actions`
- `ui_shift` / `["*", "interact-dialogs"]`

两条都是「case 子进程在 runner 上死亡」（fragment 未落盘 / 渲染进程被关），即 #483 定义的
incomplete，现在由执行层重试吸收。口径写进文件头：删掉之后若仍偶发 incomplete，应由那次重试
吸收；**连续两次 incomplete 才是硬红**（汇总/失败项里写 `incomplete ×2`），这时该查真死因，
不要再往这张表加一行。断言失败永不重试，所以这张表也不该再登记「断言不符」。

## 7. 验收：四组注入（真套件 + 真子进程，不是只跑正常路径）

注入方式：**临时**给 `frontend` 套件的 fragment 加一个 `F3_INJECT_CASE=<case>:<mode>` 钩子
（`die-once` / `assert` / `die-always`），并把 `CASES` 收窄到 `source-actions` 一条，
跑完立即还原（交付 diff 里没有这些行）。注入点就是真实执行路径：子进程里 `process.exit(1)`
且不写 `case.json` = 真实的「case 子进程死亡」。

复跑（`ANOTIFY_BIN` / `DEVSEED_BIN` / `ANODE_BIN` 由 `run_all.sh` 或手工导出）：

```text
F3_ONLY_CASE=source-actions node scripts/e2e/suites/frontend.mjs
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-once node scripts/e2e/parallel_runner.mjs frontend
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:assert node scripts/e2e/suites/frontend.mjs
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-always node scripts/e2e/suites/frontend.mjs
```

### ① 注入死亡：首次 `process.exit(1)` 且不写 fragment → 先取证、重试一次通过

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-once \
    node scripts/e2e/parallel_runner.mjs frontend
  ✅ source-actions (14 behavior assertions) [首次 incomplete → 重试通过]
  [frontend] 重试: 1 条首次 incomplete，1 条重试通过，现场取证: .e2e-bin/results/_incomplete-forensics/
    1) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用（dmesg: 读取内核缓冲区失败: 不允许的操作）)  oom_kill=0  取证: .e2e-bin/results/_incomplete-forensics/frontend-source-actions-a1.txt

  [frontend] 1 通过 / 0 失败
...
  重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete 1 条 · 重试通过 1 · 重试仍 incomplete 0（1 个套件上报了重试账本；现场取证: .e2e-bin/results/_incomplete-forensics/）
...
  ✅ frontend           1通过/0失败（其中 1 条首次 incomplete、1 条重试通过；现场取证: .e2e-bin/results/_incomplete-forensics/）  11.4s  mode=real
      1) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用（dmesg: 读取内核缓冲区失败: 不允许的操作）)  oom_kill=0  取证: .e2e-bin/results/_incomplete-forensics/frontend-source-actions-a1.txt
==========================================
✅ E2E 全绿：1/1 个请求套件均达标（断言 1 通过 / 0 失败，authority 清单覆盖 1 个）
```

首次死亡的 `retry.json`（节选）与取证文件（`…/frontend-source-actions-a1.txt`，节选）：

```json
{ "kind": "pass", "retried": true, "recovered": true, "exhausted": false,
  "attempts": [
    { "attempt": 1, "kind": "incomplete", "emptyOutput": true,
      "why": "结果文件缺失（case.json 未落盘）；子进程输出为空；子进程 exit=1 signal=none",
      "forensics": { "caseIndex": 1, "chromProcs": 0, "chromMb": 0, "dmesg": "不可用（…）", "file": "…-a1.txt" } },
    { "attempt": 2, "kind": "pass", "why": "" } ] }
```

```text
#483 incomplete 现场取证
suite=frontend case=source-actions #1/1 attempt=1 at=2026-11-20T02:2x:xx.xxxZ
子进程: exit=1 signal=none timedOut=false
case 输出: 为空（0 字符）
chromium: 0 procs, 0 MB RSS
kernel dmesg(tail 40): 不可用（dmesg: 读取内核缓冲区失败: 不允许的操作）
oom_kill(自启动累计, /proc/vmstat): 0

--- dmesg ---
(不可用: …)
--- /proc/vmstat (oom_kill，自机器启动累计) ---
oom_kill 0
--- free -m ---
              总计        已用        空闲  …
--- df -h /tmp ---
tmpfs           7.5G  2.8G  4.8G   37% /tmp
--- case stdout/stderr（head） ---
(为空：子进程没有任何输出)
```

### ② 注入断言失败：不得重试（该 case 只跑一次）

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:assert \
    node scripts/e2e/suites/frontend.mjs
[frontend] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）

[frontend] 0 通过 / 1 失败
失败项:
  - source-actions — AssertionError [ERR_ASSERTION]: injected assertion failure (#483 acceptance): deterministic
```

`retry.json` 证明只跑了一次：`"attempts": [{ "attempt": 1, "kind": "assertion" }]`，
`"retried": false`，**没有任何取证文件**（断言失败不是 incomplete，不采现场、不重试）。

### ③ 连续死两次：仍然红，reason = `incomplete ×2`，两份取证

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-always \
    node scripts/e2e/suites/frontend.mjs
  ❌ source-actions — incomplete ×2：结果文件缺失（case.json 未落盘）；子进程输出为空；子进程 exit=1 signal=none
[frontend] 重试: 1 条首次 incomplete，1 条重试仍 incomplete（判红），现场取证: .e2e-bin/results/_incomplete-forensics/
  1) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用…)  oom_kill=0  取证: …/frontend-source-actions-a1.txt
  2) source-actions         #1   exit=1 signal=none  chrom=0 procs/0MB  dmesg: (不可用…)  oom_kill=0  取证: …/frontend-source-actions-a2.txt

[frontend] 0 通过 / 1 失败
EXIT=1
```

### ④ 正常全绿：重试计数 = 0（证明不是无条件重跑）

```text
$ F3_ONLY_CASE=source-actions node scripts/e2e/suites/frontend.mjs
  ✅ source-actions (14 behavior assertions)
[frontend] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）

[frontend] 1 通过 / 0 失败
EXIT=0
```

全量并行跑的同一结论见 §9（28 个套件全绿，`首次 incomplete 0 条`）。

## 8. 自测（不需要真浏览器，秒级，接在 make ci 路径上）

`scripts/e2e/f3-retry-selftest.mjs`：真起子进程，覆盖 ①die-once（含取证文件内容与摘要行）
②assertion（不重试、无取证）③die-always（两份取证）④pass（零重试、无 retry.json）
⑤浏览器死亡签名（`Target page…closed` + fragment 仍算 incomplete）⑥`fragment.ok=true` 但
exit≠0（不重试）⑦纯函数边界（无 fragment + 空输出必须点名）。

接线方式：`scripts/e2e/ci-e2e-verdict.mjs --self-test` 会跑它，而那条已经在
`make check-affected-map`（属于 `make ci`）里 —— 判「什么算红」的机制自己也被判了一次。
单独跑：`node scripts/e2e/f3-retry-selftest.mjs`。

## 9. 疑似第二根因：4500ms 类硬等待（只查不改）

`known-env` 里 `source-actions` 那条 reason 写的是「locator 4500ms 撞线」。核实结果：

- **默认值**在 `scripts/e2e/fixtures/f3/f3-driver.mjs:783`：
  `page.setDefaultTimeout(Number(process.env.F3_DEFAULT_TIMEOUT_MS || 4500))`。这一条 CI 已经
  抬过：`.github/workflows/ci.yml:142` 设 `F3_DEFAULT_TIMEOUT_MS: "15000"`
  （`F3_NAV_TIMEOUT_MS: "20000"`、`F3_CASE_BUDGET_MS: "150000"`）。所以「4500ms 撞线」的措辞
  对 driver 默认值已**过期**。
- 但还有**活的、env 抬不动的**硬等待（这才是「慢 runner 被判成没有」的候选）：
  - `scripts/e2e/fixtures/f3/f3-i18n-cases.mjs:330`：
    `d.page.setDefaultTimeout(2200); d.page.setDefaultNavigationTimeout(5000);`
    —— i18n / i18n_coverage 两个套件把 action 超时**又压回 2.2s**，env 抬到 15s 也救不了它。
  - `scripts/e2e/fixtures/f3/f3-ui-helper.mjs:148/149/151`：写死 7000 / 8000 / 5000 的
    挂载与锚点等待（都带 `.catch()`，等待本身不抛，后果由后续断言判红）。
  - `scripts/e2e/fixtures/f3/f3-trace-startup.mjs:84/148/177/180/203`：写死 4500，但**没有
    注册套件 import 它**（只在 epoch 指纹里，属死路径）。
  - 反例（已经是对的做法）：`scripts/e2e/fixtures/f3/f3-ui-cases.mjs:652` 把撤销条的等待从
    4500 抬到 15000，并写明「15s 是等它出现的上界，不是断言」。

结论：这类失败会产出 fragment（`AssertionError`）→ 被判成 `assertion` → **不重试**，所以
本 PR 的重试**不会掩盖它**；真正的修法是 #478 式「放 deadline、不放断言」：把
`f3-i18n-cases.mjs:330` 的 2200/5000 改成读 env（或直接抬到与 driver 同档），把
`f3-ui-helper.mjs` 的三个上界也做成可配置。这条单独立项，不在本 PR 范围。

## 10. 顺手修掉的一个执行层竞态（跑全量时撞出来的）

第一次全量验证里 `frontend` 红了，失败项是 `yaml-create`：

```text
yaml-create — AssertionError [ERR_ASSERTION]: 坏 YAML 的 reason 必须是 invalid_yaml
+ actual - expected
+ undefined
- 'invalid_yaml'
```

按 #483 的判据这属于 `assertion`（有 fragment、失败是断言不符）⇒ **没有重试**，判红是对的。
查证据发现不是产品问题：那条 POST 在 `report.requests` 里明明有
`httpStatus:400, reason:"invalid_yaml"`，用例读到的却是「有状态、没有 body」的半条记录。

根因（执行层竞态，`scripts/e2e/fixtures/f3/f3-driver.mjs:758` 附近）：fetch 包装器先写
`call.httpStatus=response.status`，再 `call.result=await response.clone().json()`；而
`waitPost()` 只等状态码（`scripts/e2e/fixtures/f3/f3-yaml-create.mjs:93`），机器慢时正好卡在
两者之间。

修法（**不放宽断言**，断言一个字没改，只是等它要断言的 body 真的到齐）：

- 包装器在 body 处理完之后置 `call.settled=true`（异常路径也置）；
- `waitPost()` 的条件加上 `c.settled === true`。

这类「读到半条调用记录」的红本身就是 #483 想消灭的那类假红（用例没跑完/证据不全），只是它恰好
长成了 `AssertionError` 的形状，所以归到执行层修，而不是给它开重试口子。

## 11. 门禁结果（本机）

```text
$ flock /tmp/anotify-ci.lock -c "make ci"
EXIT=0
  ✅ #483 重试自测：incomplete 重试 1 次 / 断言不重试 / 连续两次 incomplete 仍红

$ make e2e-affected
  ✅ E2E 全绿：28/28 个请求套件均达标（断言 2370 通过 / 0 失败，authority 清单覆盖 28 个）
  重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete 0 条 · 重试通过 0 · 重试仍 incomplete 0（5 个套件上报了重试账本）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 352.7s）
  总耗时: 352.6s
```

`make e2e-affected` 因为改了 `scripts/e2e/lib/harness.mjs`（共享面）而**保守跑全量**
28/28（不是只跑 f3 系）。正常全量下重试计数 = 0，与注入实验（§7）互为对照。

> 验证过程中撞到两个**与本改动无关**的既有抖动，都已留档、最终全量通过：
> ①第一次全量跑 `orphans` 挂 `database is locked`（`lib/v4.mjs` 的 `seedPendingTask`
> 直插 sqlite 与自己的 server 抢写锁），重跑与后续全量均通过（`orphans 98通过/0失败`）；
> ②第二次全量跑 `frontend/yaml-create` 挂在「读到半条调用记录」的竞态上（§10），
> 已按执行层修掉，随后 `frontend` 单跑 43/0、全量 43/0。

## 12. 边界与已知取舍

- **超预算（timedOut）也按 incomplete 先取证、重试一次**：它的现场与「没有 fragment」相同，
  按同一档处理；代价是慢机上一个卡死的 case 会花两次单 case 预算（52s→约 108s），套件层的
  per-suite 预算（如 frontend 360s）仍独立判红。
- **`--case` 单跑模式不经过重试**：那条路径是 fragment 本身（无父进程可重试），与旧行为一致。
- 重试与取证只发生在**执行层**；判定层（`ci-e2e-verdict.mjs`）与 known-env 语义没变，只是清单空了。
- 取证采集是同步的（父进程在重试前阻塞几十毫秒），每条命令 3s 上限；采集失败只记原因，不影响判定。
- 注入钩子是临时的（已还原），没有把故障注入开关留在交付代码里。
