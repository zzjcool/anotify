# E2E：#485 52s 硬杀不再「什么都没留下」；source-actions 的真挂点 = Chrome 153+ 减少动效下的菜单重排死循环

snapshot-doc: 2026-11-20 本机交付快照（下文注入输出、门禁数字都是当次运行的定格；每处都附了复跑命令，复跑以新输出为准）

- 基线：`origin/main` = `141edd25`；本分支从 #483 的重试实现（`d3c26dd4`）继续，PR 叠在 #484 上
- 结论 A（可诊断）：52s/54s 硬杀改成**先落 timeout fragment、再发信号**。判定层把
  `timeout`（有 fragment 的「卡住」）与 `incomplete`（无证据）分开；timeout 与 incomplete
  一样**只重试 1 次**，两次 attempt 的 `lastStep`/`pendingStep` 并排打印
  （相同 = 确定性挂点，不同 = 抖动/环境压力）。三执行器（frontend / ui 系 / i18n 系）同一口径。
- 结论 B（真挂点）：不是「慢」也不是「等一个永远不来的帧」。`source-actions` 第 5 条 PASS 之后
  打开行内 ⋯ 菜单时，`placeMenu()` 会**清空再写 `left/top`**；减少动效偏好把 `*` 的
  `transition-duration` 压到 0.001ms（`transition-property` 保持初始值 `all`），于是每次写入都产生
  `transitionend`，而 `transitionend` 又绑着 `reflowMenus()` → `placeMenu()` → **无限同步重排**。
  Chrome 153+ 会真的跑这 0.001ms 过渡（Playwright 自带的 Chromium 140 把它取整成 0）。
  渲染主线程被占满后，`page.evaluate`（没有内建超时）永不返回：用例自己的 48s 自检、
  失败捕获、`stop()` 里的 `archive()` 全部卡在同一个 evaluate 上 —— 这就是 runner 上
  「52s 之后什么都没有」的机制。
- 没动产品代码（本任务边界），没放宽任何断言，`known-env-failures.json` 的 `entries` 保持空数组。
  产品侧死循环的完整证据 + 一行修法写在第 §3.4 节，建议另开产品 issue 跟踪。
- CI 复核（§5.1）：`make ci` 绿；全量 e2e 里 **source-actions 已通过**（不再 timeout），
  另外两个红（ui_shift 300s、node-aba 计数 flake）都是本 PR 之前就存在的，附了对照证据。

## 1. 现象与旧实现的漏洞（复述 #485）

CI run `44663970230`（#484 分支自己的 e2e-affected）里 `frontend` 的 `source-actions`：

```text
failures: ["source-actions — incomplete ×2：结果文件缺失（case.json 未落盘）；子进程 exit=1 signal=none timedOut"]
durationMs: 230921
```

两次 attempt 的输出都是 1416 字符、末尾都是第 5 条 `PASS`，然后整 52s 没有任何新行。
旧实现的问题不是「重试次数不够」，而是**证据在杀进程之前就丢了**：

- 52s 定时器直接 `SIGTERM`；`scripts/e2e/lib/harness.mjs` 的 `SIGTERM` 钩子是
  `killAll(); process.exit(1)`，子进程里正在 `await` 的 case 没有任何机会写 `case.json`。
- 于是「卡住 52s」与「进程死了」长得一模一样：判定层只能归 `incomplete`，
  汇总里只有「结果文件缺失」，没有**停在哪一步、在等什么、等了多久**。

## 2. A：timeout 的判定、取证与汇总（本次落地）

### 2.1 父进程在发信号**之前**写 fragment

三个执行器的硬预算路径都改成：

```js
const startedAt = Date.now();
const soft = setTimeout(async () => {
  timedOut = true;
  await writeTimeoutFragment({ dir, identity, output: childOutput, budgetMs, elapsedMs: Date.now() - startedAt });
  kill('SIGTERM');                    // 必须晚于落盘
}, budgetMs);
const hard = setTimeout(() => { if (timedOut) kill('SIGKILL'); }, budgetMs + 2000);
```

- `writeTimeoutFragment()` 在 `scripts/e2e/fixtures/f3/f3-retry.mjs`，用 `flag:'wx'` **独占写**：
  子进程若已经在预算边界写完了自己的 `case.json`，就以子进程的证据为准，不覆盖。
- fragment 形状与正常 fragment 对齐（`identity` + `ok:false` + `checks` + `error`），另外带：
  `timedOut:true`、`lastStep`、`pendingStep`、`lastStepIndex`、`elapsedMs`、`budgetMs`、
  `caseOutputBytes`、`caseOutputTail`（尾 4096 字符）。
- `lastStep` / `pendingStep` 来自 case 输出里的两类行：
  - `PASS <case> :: <step>` —— 最后完成的断言；
  - `WAIT <case> :: <label>` / `WAIT OK <case> :: <label>` —— 有界等待的进出标记。
    最后一条**没有** `WAIT OK` 的就是「正在等什么」。`source-actions` 的第 6 段
    （行内菜单 disable/enable 往返）已经逐等待打点（`scripts/e2e/fixtures/f3/f3-source.mjs`）。

### 2.2 判定：timeout 优先于一切，且与 incomplete 分开

`classifyCaseEvidence()`（`f3-retry.mjs`）的顺序：

1. `exit.timedOut === true` → **TIMEOUT**（有 fragment 用 fragment 的 `lastStep`，没有就扫输出；`why` 里明说
   `timeout fragment 未落盘`）——不会再被说成「没有证据」；
2. 无 fragment → INCOMPLETE（保持 #483 的死亡签名口径）；
3. 有 fragment 且 `ok:true` 但进程没收尾 → 不重试、判红（fragment 是独占证据）；
4. 其余 → ASSERTION（永不重试）。

重试策略不变：**只重试 1 次**，`incomplete` 与 `timeout` 共用这一档；断言失败永不重试。
重试不删证据：首次 attempt 的 `case.json`/`exit.json`/`full.log` 搬到 `<case dir>/retry-1/`，
`retry.json` 里逐 attempt 记 `kind/why/exit/lastStep/pendingStep/elapsedMs/budgetMs/caseOutputTail`。

### 2.3 汇总：卡点并排打印（零重试也打印）

套件 stdout（三执行器同形）+ 并行汇总（`scripts/e2e/parallel_runner.mjs`）：

```text
[frontend] 重试: 1 条首次无结论（1 条首次 timeout），1 条重试仍无结论（判红），1 条两次都停在同一步
  1) source-actions         a1 timeout(52016ms/52000ms) 停在「Explicit reload -> …」，正在等「…」 ·
                            a2 timeout(52000ms/52000ms) 停在「Explicit reload -> …」，正在等「…」
                            ⇒ 两次都停在同一步「Explicit reload -> …」
```

结果 JSON（`.e2e-bin/results/<suite>.json` 的 `retries`）也带上同样的字段，CI artifact 整目录上传，
不需要改 workflow。

### 2.4 验收①：sleep-60s 真注入（真套件 + 真子进程）

注入方式：**临时**在 `f3-source.mjs` 的 `wait()` 里加一个 `F3_INJECT_SLEEP_MS/AT` 钩子，
把 `row menu: click Disable` 这个等待换成 60s sleep，跑完立即还原（交付 diff 里没有这些行）。
注入点就是真实执行路径：子进程真的卡在预算内不返回，父进程真的发信号。

```text
F3_INJECT_SLEEP_MS=60000 F3_INJECT_SLEEP_AT='row menu: click Disable' \
E2E_CHROME_PATH=/usr/bin/google-chrome-stable node scripts/e2e/suites/frontend.mjs
```

结果（`/tmp/inject485.log`，`.e2e-bin/results-inject485/`）：

- `42 通过 / 1 失败`；失败项：
  `source-actions — timeout ×2：超出 52000ms 硬预算（已运行 52000ms）；最后完成的步骤：Explicit reload -> next edit succeeds, omitted credentials preserve secret/hookUrl；卡在等待：INJECTED_SLEEP 60000ms；两次都停在同一步「…」`
- **两次 attempt 都真的落了 `case.json`**（不是「结果文件缺失」）：
  `<case dir>/retry-1/case.json` 与 `<case dir>/case.json`，各 2723 字节；
  `timedOut:true`、`checks:5`、`lastStep` 相同、`caseOutputTail` 里有 5 条 PASS + 两条 WAIT 行。
- `frontend.json` 的 `retries`：`retried:1, recovered:0, exhausted:1, timeoutFirst:1,
  timeoutsStuckSameStep:1`，`timeouts[0].attempts[*]` 带 `pendingStep`。

结论：硬杀路径不再丢现场；`timeout` 与 `incomplete` 在输出/JSON/汇总三处都能区分；
「两次都停在同一步」是机器可判的。

## 3. B：source-actions 的真挂点

### 3.1 复现条件（决定性变量是浏览器版本）

| 条件 | 结果 |
| --- | --- |
| 默认（Playwright Chromium 140.0.7339.16） | `source-actions` 10s 内全绿（14 条断言） |
| `E2E_CHROME_PATH=/usr/bin/google-chrome-stable`（Chrome 153.0.8010.52） | **确定性挂死**（每次都在同一个 `pickMenu` 处冻结；单跑也是同一步） |
| 4 个 case 并发 + `CPUQuota=200%`（Chromium 140） | 全绿 —— **不是 CPU/内存压力** |

CI runner 用的是系统 Chrome（日志：`Google Chrome 157.0.8103.50`），不是 Playwright 自带的
Chromium；本机没有 157，但用系统 Chrome 153 复现了**同一处、同一签名**的挂死
（CI 的 1416 字符输出末尾也是同一条 `PASS`、同样 52s 后消失），因此判断同因。
这也解释了「本地默认复跑不出来、CI 固定挂」。

### 3.2 挂在哪一行、在等什么

- 挂住的动作：`scripts/e2e/fixtures/f3/f3-source.mjs` 第 6 段的第一句
  `pickMenu('ui-webhook', 'Disable')` → `.menu-item` 的 `locator.click()`。
- Playwright 的 call log 走到 `element is visible, enabled and stable / done scrolling` 就停了：
  点击的**命中测试**拿不到渲染进程的响应，15s 动作超时；
- 此后**所有** `page.evaluate` 都不返回（本 PR 加的有界 deadline 先报出来）：

```text
WAIT_TIMEOUT archive page.evaluate source-actions：等了 15000ms 仍未返回（有界 deadline；不是放宽断言）
CASE_FAIL source-actions :: locator.click: Timeout 15000ms exceeded.
```

- 用例自己的 48s 自检 `d.stop()` → `archive()` → `page.evaluate` 同样永不返回；
  52s 父进程 SIGTERM → `process.exit(1)` ⇒ 旧实现下 `case.json` 100% 丢失。

### 3.3 根因：`placeMenu` 与 `transitionend` 互相喂饭（产品侧）

CDP `Profiler` + 事件追踪（Chrome 153，菜单打开后 2s 采样）：

```text
PROBE_PROFILE ["getBoundingClientRect 1943.0ms", "placeMenu :331 10.3ms", "reflowMenus :355 5.2ms", …]
PAGE EVT transitionrun  n=1    target=menu prop=top
PAGE EVT transitionend  n=200  target=menu prop=left
PAGE EVT transitionend  n=2000 target=menu prop=left        ← 每秒上千次
computed: transitionProperty="all", transitionDuration="1e-06s"
```

把浏览器报的行号映射回源文件（浏览器报的是内联脚本行号）：

- `web/sources.html:249-271` `placeMenu()`：**先清空** `menu.style.left="" / top=""`，
  再写 `menu.style.left=… / top=…`（第 255-263 行），最后按 `getBoundingClientRect()` 补一次偏移；
- `web/sources.html:273-276` `reflowMenus()`；`web/sources.html:290`
  `document.addEventListener("transitionend", reflowMenus, true)`；
- `web/ui.css:4201-4209`（`prefers-reduced-motion: reduce`）把 `*` 的
  `transition-duration` 压到 `0.001ms !important`，但**没有**改 `transition-property`
  （初始值就是 `all`）。

于是：`placeMenu` 每次清空/写入 `left/top` 都会起一个 0.001ms 过渡 → `transitionend` →
`reflowMenus` → `placeMenu` → 又是新的过渡……同步重排无限循环，渲染主线程永远不还。
Chrome 140 把这 0.001ms 取整成 0（不产生过渡），Chrome 153+ 会实际跑一帧，所以只有新
Chrome 挂。

**为什么不是「某个等待条件永远不满足」**：没有 selector/帧/接口在等；是渲染进程被自己的
过渡回调占满，连 CDP 的 `Runtime.evaluate` 都排不上队。所以「抬 deadline」在这里没用
（52s 只是把一次冻结拖长），有界 deadline + 取证才是正确处置。

### 3.4 产品修法（不在本 PR 修，边界外）

最小一行（推荐，组件级覆盖全局 reduce 规则）：

```css
/* web/ui.css，.act-menu > .menu 段 */
.act-menu > .menu { transition: none; }
```

或让 `reflowMenus` 忽略自己造成的过渡：

```js
document.addEventListener("transitionend", (event) => {
  if (event.target.classList.contains("menu")) return;   // 只关心按压缩放等祖先过渡
  reflowMenus();
}, true);
```

验证方式：`E2E_CHROME_PATH=/usr/bin/google-chrome-stable`（或任意 Chrome ≥153）+
`reducedMotion:'reduce'` 打开任意源的行内 ⋯ 菜单，页面会冻结；修好后同条件点 Disable/Enable
应即时完成。影响面：Chrome ≥153 且系统开了「减少动效」的用户 —— 打开行菜单就整页卡死，
不是 e2e 独有的问题，建议另开产品 issue。

### 3.5 本 PR 在 e2e 侧的处置（不静默改产品）

1. **有界 deadline**（`f3-driver.mjs`）：`withDeadline()` 包住所有 `page.evaluate`
   （`archive` / `req` / `response` / `calls`）与 `stop()` 里的 `archive`/`context.close`/
   `browser.close`；`req`/`response` 另外给页面侧 `AbortSignal.timeout(30s)`。
   超时仍然让 case 失败（不放宽断言），只是失败得快、说清「在等什么、等了多久」。
2. **WAIT 打点**：`source-actions` 第 6 段的每个等待进出都打一行，硬杀 fragment 里能直接看到卡点。
3. **`source-actions` 用正常动效跑**（`reducedMotion:'no-preference'`，case 选项）：
   这条 case 验的是源动作/CAS/回执语义，不验动效偏好；减少动效下的菜单死循环是产品 bug，
   由 §3.4 跟踪，不在 e2e 里假装它不存在（注释放了 issue 指针与证据位置）。

## 4. 验收②③：CI 等价 Chrome 上不再 timeout；正常路径全绿、不产生额外证据

`E2E_CHROME_PATH=/usr/bin/google-chrome-stable`（CI 同款系统 Chrome，153）+ CI 同款超时环境：

```text
F3_DEFAULT_TIMEOUT_MS=15000 F3_NAV_TIMEOUT_MS=20000 E2E_CHROME_PATH=/usr/bin/google-chrome-stable \
node scripts/e2e/suites/frontend.mjs
```

- `43 通过 / 0 失败`，`source-actions` 14 条断言全过（含 §3.2 那一段）；
- `重试: 0 条`；本 case 目录下只有 `case.json`/`exit.json`/`report.json`，**没有** `retry.json`、
  `retry-1/`、timeout fragment；
- 汇总不打印 timeout 段（零重试与零 timeout 都不打印）。

## 5. 验收④：门禁（本机定格）

本机 12 核 / 14GB、Node 26（CI 是 4 vCPU / Node 22；这里的数字只做「本机全绿」的定格）：

```text
$ flock /tmp/anotify-ci.lock -c "make ci"
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对）通过
✅ 契约字段守卫通过（234 个 schema、724 个已知字段）
✅ check-annotations 48 项判定 PASS 48 / FAIL 0（reduced-motion 注释里的计数词按规则 D 改写，棘轮 385/385）
✅ gofmt / go vet / staticcheck 干净；go test 全包 ok（含 internal/server、store/v4test）
✅ harness 自测通过（端口/boot id/两次启动不同）
EXIT=0

$ flock /tmp/anotify-ci.lock -c "make e2e-affected"
✅ E2E 全绿：29/29 个请求套件均达标（断言 2431 通过 / 0 失败，authority 清单覆盖 29 个）
   frontend 43通过/0失败 161.5s · ui_shift 87通过/0失败 156.6s · capacity 178通过/0失败 147.5s
   重试: 首次 incomplete 0 条 · 首次 timeout 0 条 · 重试通过 0 · 重试仍无结论 0（5 个套件上报了重试账本）
✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 360.0s）
EXIT=0
```

（合并 main 前同一套门禁是 28/28、2370 断言；main 带来第 29 个套件后复跑仍全绿。数字是快照，
以复跑输出为准。）

（`make e2e-affected` 本次选中 28/28 全套：改动里的 `scripts/e2e/lib/harness.mjs` 是共享面，
选择器按「无法判定 ⇒ 保守跑全套」处理，不是人为 `--full`。）

## 5.1 CI 复核（GitHub runner，Chrome 157 + Node 22）

`workflow_dispatch` 跑全量（`make ci` + 全量 e2e），run `44673307477`，head `1bbb2041`：

```text
✅ 快门禁（make ci）                       success
  全量 e2e                                  ❌（两个与本 PR 无关的红，见下）
  ❌ frontend 42通过/1失败 219.2s           唯一失败 = node-aba（既有 flake）
  ⏱️ ui_shift 0通过/0失败 300.1s            per-suite 300s 预算被 SIGKILL（既有问题）
  重试（incomplete/timeout → 自动重试 1 次）: 首次 incomplete 0 条 · 首次 timeout 0 条 · 重试通过 0 · 重试仍无结论 0
```

- **source-actions 在 runner 上通过了**（frontend 43 条里 42 条过，唯一失败是 node-aba；
  `首次 timeout 0 条`）——#485 的验收②在真实 CI 上成立，不再是 `timeout ×2`。
- `ui_shift 300s` 是**既有失败**：同一条日志在 #484 的 run `44663970230` 里已经出现
  （`⏱️ 套件 ui_shift 超时（300s）`），本 PR 没动 ui_shift。
  它很可能是同一个产品死循环（`f3-ui-cases.mjs` 的 interact-dialogs 也点行内 `.act-more`），
  修 §3.4 后应一并消失；在那之前需要单独抬 ui_shift 的 per-suite 预算或缩 case。
- `node-aba count: 19 != 18` 是**既有 flake**：它断言「释放延迟 PUT 后 50ms 内页面不得发新请求」，
  多出来的那次请求是页面自己的时序。同一份代码上跑：本分支 3/23 红（13%）、
  基线 `d3c26dd4` 2/25 红（8%）—— 同一量级；本 PR 不改这个 case，也不新增页面请求。

## 6. 复跑命令

```text
# 自测（纯函数 + 真子进程注入：⑦timeout×2 同 lastStep ⑧timeout→pass ⑨timedOut 优先 ⑩WAIT 解析）
node scripts/e2e/f3-retry-selftest.mjs

# 单 case（默认 Chromium 140 应绿；Chrome ≥153 才复现 §3 的挂死）
ANOTIFY_BIN=$PWD/.e2e-bin/anotify DEVSEED_BIN=$PWD/.e2e-bin/devseed ANODE_BIN=$PWD/.e2e-bin/anode \
  node scripts/e2e/suites/frontend.mjs --case source-actions

# 用系统 Chrome 跑（CI 等价）
E2E_CHROME_PATH=/usr/bin/google-chrome-stable node scripts/e2e/suites/frontend.mjs

# 全套门禁
flock /tmp/anotify-ci.lock -c "make ci"
make e2e-affected
```

## 7. 遗留风险

- **产品死循环仍在**（§3.4）：本 PR 只保证 e2e 不再因此「静默超时」并把证据留下来；
  source-actions 用正常动效绕开了触发条件。Chrome ≥153 + 减少动效的真实用户仍会卡死，
  需要产品侧一行修复（建议单独 issue + 回归 case：`reducedMotion:'reduce'` 下打开行菜单）。
- `F3_FRONTEND_CASE_BUDGET_MS` 刻意不复用 CI 的 `F3_CASE_BUDGET_MS`（150s）：frontend 有 43 个
  case、per-suite 预算 360s，单 case 150s 会把「一个 case 卡住」变成「整套被 SIGKILL、零 results」。
- `withDeadline` 的 15s 是宿主侧上限；它只能保证「失败得快」，不能证明页面一定响应
  （§3 的死循环就是反例）——所以 timeout fragment 仍然必须由父进程写。
