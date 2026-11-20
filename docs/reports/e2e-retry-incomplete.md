# E2E：#483 case 子进程死亡不再算「断言失败」（incomplete 重试 1 次 / 断言永不重试）

snapshot-doc: 2026-11-20 本机交付快照（下文注入输出、门禁数字都是当次运行的定格；每处都附了复跑命令，复跑以新输出为准）

- 基线：`origin/main` = `141edd25`；分支 `pi-subagent/hw-nj/anotify-e2e-retry-91d4ffa4`（独立 worktree）
- 结论：把「用例没跑完」与「断言不符」拆成两层；前者自动重试 1 次，后者**永不**重试。
  `known-env-failures.json` 的两条环境性登记随之删除（`entries` 现在是空数组）。
- 没动产品代码，没放宽任何断言，没改 `make ci` 的其它门。

## 1. 现象与根因分层（复述 #483）

runner 上 case 子进程会偶发死亡：`completed`/`case.json` 没落盘、子进程输出为空、渲染进程
整批被关。旧判定只看「结果文件在不在 + 断言过没过」，于是同一个环境故障换一个 case 长出来
就多一条红 —— 按 case 登记 known-env 永远追不上（`source-actions` 登记之后又长出了
`observe-events` / `node-create` / `interact-dialogs`）。

分层之后：

| 层 | 判据 | 处置 |
| --- | --- | --- |
| `incomplete` | 结果文件缺失 / 子进程输出为空 / 子进程非 0（含信号、超预算）且没有 fragment | 自动重试 1 次 |
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
`<case dir>/retry.json`（两条 attempt 的 kind/why/exit 事实）。所以「重试过」本身是可复核的证据，
不是把失败抹掉。

## 4. 重试计数怎么打印（不许静默）

三处，零重试也打印：

1. 套件 stdout：`[frontend] 重试: 1 条首次 incomplete，1 条重试通过`
   （零重试：`[frontend] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）`）。
2. 结果 JSON：`retries` 字段（`H.setRetryStats` 写；`scripts/e2e/lib/harness.mjs`），
   只有 f3 系五个套件会写，其余套件结果形状不变。
3. 并行汇总（`scripts/e2e/parallel_runner.mjs`）：
   `✅ frontend 1通过/0失败（其中 1 条首次 incomplete、1 条重试通过）` +
   全局一行 `重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete N 条 · 重试通过 M · 重试仍 incomplete K`。

## 5. known-env：删掉两条（entries 已空）

`scripts/e2e/known-env-failures.json` 删除：

- `frontend` / `source-actions`
- `ui_shift` / `["*", "interact-dialogs"]`

两条都是「case 子进程在 runner 上死亡」（fragment 未落盘 / 渲染进程被关），即 #483 定义的
incomplete，现在由执行层重试吸收。口径写进文件头：删掉之后若仍偶发 incomplete，应由那次重试
吸收；**连续两次 incomplete 才是硬红**（汇总/失败项里写 `incomplete ×2`），这时该查真死因，
不要再往这张表加一行。断言失败永不重试，所以这张表也不该再登记「断言不符」。

## 6. 验收：四组注入（真套件 + 真子进程，不是只跑正常路径）

注入方式：**临时**给 `frontend` 套件的 fragment 加一个 `F3_INJECT_CASE=<case>:<mode>` 钩子
（`die-once` / `assert` / `die-always`），并把 `CASES` 收窄到 `source-actions` 一条，
跑完立即还原（`git diff` 里没有这些行）。注入点就是真实执行路径：子进程里 `process.exit(1)`
且不写 `case.json` = 真实的「case 子进程死亡」。

复跑：

```text
F3_ONLY_CASE=source-actions node scripts/e2e/suites/frontend.mjs
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-once node scripts/e2e/parallel_runner.mjs frontend
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:assert node scripts/e2e/suites/frontend.mjs
F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-always node scripts/e2e/suites/frontend.mjs
```

（`F3_ONLY_CASE` 是临时收窄用的；`ANOTIFY_BIN` / `DEVSEED_BIN` 由 `run_all.sh` 或手工导出。）

### ① 注入死亡：首次 `process.exit(1)` 且不写 fragment → 重试一次通过，汇总出现「重试」

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-once \
    node scripts/e2e/parallel_runner.mjs frontend
  ✅ source-actions (14 behavior assertions) [首次 incomplete → 重试通过]
  [frontend] 重试: 1 条首次 incomplete，1 条重试通过

  [frontend] 1 通过 / 0 失败
...
  重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete 1 条 · 重试通过 1 · 重试仍 incomplete 0（1 个套件上报了重试账本）
...
  ✅ frontend           1通过/0失败（其中 1 条首次 incomplete、1 条重试通过）  11.3s  mode=real
==========================================
✅ E2E 全绿：1/1 个请求套件均达标（断言 1 通过 / 0 失败，authority 清单覆盖 1 个）
```

首次死亡的证据（`retry.json`，节选）：

```json
{ "kind": "pass", "retried": true, "recovered": true, "exhausted": false,
  "attempts": [
    { "attempt": 1, "kind": "incomplete", "emptyOutput": true,
      "why": "结果文件缺失（case.json 未落盘）；子进程输出为空；子进程 exit=1 signal=none" },
    { "attempt": 2, "kind": "pass", "why": "" } ] }
```

### ② 注入断言失败：不得重试（该 case 只跑一次）

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:assert \
    node scripts/e2e/suites/frontend.mjs
  ❌ source-actions — AssertionError [ERR_ASSERTION]: injected assertion failure (#483 acceptance): deterministic
[frontend] 重试: 0 条（没有 case 需要重试：incomplete 才重试 1 次，断言失败不重试）

[frontend] 0 通过 / 1 失败
```

`retry.json` 证明只跑了一次：`"attempts": [{ "attempt": 1, "kind": "assertion" }]`,
`"retried": false`。日志里该 case 的 `FRAGMENT_IDENTITY` / `PASS` 行也只出现一次。

### ③ 连续死两次：仍然红，reason = `incomplete ×2`

```text
$ F3_ONLY_CASE=source-actions F3_INJECT_CASE=source-actions:die-always \
    node scripts/e2e/suites/frontend.mjs
  ❌ source-actions — incomplete ×2：结果文件缺失（case.json 未落盘）；子进程输出为空；子进程 exit=1 signal=none
[frontend] 重试: 1 条首次 incomplete，1 条重试仍 incomplete（判红）

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

全量并行跑的同一结论见 §8（28 个套件全绿，`首次 incomplete 0 条`）。

## 7. 自测（不需要真浏览器，秒级，接在 make ci 路径上）

`scripts/e2e/f3-retry-selftest.mjs`：真起子进程，覆盖 ①die-once ②assertion ③die-always
④pass ⑤浏览器死亡签名（`Target page…closed` + fragment 仍算 incomplete）⑥`fragment.ok=true`
但 exit≠0（不重试）⑦纯函数边界（无 fragment + 空输出必须点名）。

接线方式：`scripts/e2e/ci-e2e-verdict.mjs --self-test` 会跑它，而那条已经在
`make check-affected-map`（属于 `make ci`）里 —— 判「什么算红」的机制自己也被判了一次。
单独跑：`node scripts/e2e/f3-retry-selftest.mjs`。

## 8. 门禁结果（本机）

```text
$ flock /tmp/anotify-ci.lock -c "make ci"
EXIT=0

$ make e2e-affected
  ✅ E2E 全绿：28/28 个请求套件均达标（断言 2370 通过 / 0 失败，authority 清单覆盖 28 个）
  重试（incomplete → 自动重试 1 次；断言失败永不重试）: 首次 incomplete 0 条 · 重试通过 0 · 重试仍 incomplete 0（5 个套件上报了重试账本）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 353.4s）
  总耗时: 353.3s
```

`make e2e-affected` 因为改了 `scripts/e2e/lib/harness.mjs`（共享面）而**保守跑全量**
28/28（不是只跑 f3 系）。正常全量下重试计数 = 0，与注入实验（§6）互为对照。

> 第一次全量跑时 `orphans` 挂过一次 `database is locked`（`lib/v4.mjs` 的
> `seedPendingTask` 直插 sqlite 与自己的 server 抢写锁，与本改动无关的既有抖动）；
> 重跑那套件与第二次全量均通过（`orphans 98通过/0失败`）。

## 9. 边界与已知取舍

- **超预算（timedOut）也按 incomplete 重试一次**：它的现场与「没有 fragment」相同，按同一档
  处理；代价是慢机上一个卡死的 case 会花两次单 case 预算（52s→约 108s），套件层的
  per-suite 预算（如 frontend 360s）仍独立判红。
- **`--case` 单跑模式不经过重试**：那条路径是 fragment 本身（无父进程可重试），与旧行为一致。
- 重试只发生在**执行层**；判定层（`ci-e2e-verdict.mjs`）与 known-env 语义没变，只是清单空了。
- 注入钩子是临时的（已还原），没有把故障注入开关留在交付代码里。
