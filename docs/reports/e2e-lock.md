# e2e 机器级互斥（#529）—— 交付与验收

<!-- snapshot-doc: 2026-11-22 #529 交付取证快照。下文数字都是这台机器（12 核，常驻 kind/k8s）这次运行的真实输出；复算命令：`make ci`、`make e2e-affected`、`make check-e2e-lock`、`bash scripts/e2e/lock_selftest.sh`，复跑以新输出为准。 -->

> 结论：`scripts/e2e/run_all.sh` 入口加了 **flock 机器级互斥**，粒度是**矩阵**而不是套件；
> 等锁上限 `E2E_LOCK_WAIT`（默认 900s），超时打印持锁者后 **exit 75**；`E2E_LOCK=0` 是显式逃生口；
> `kill -9` 后 **22ms** 就能重新拿到锁。锁只落在矩阵入口，`--parallel` 的套件照旧并发（实测峰值
> 20 个套件进程同时在跑）。`make ci` exit 0，`make e2e-affected` 30/30 套件 · 2820 断言全绿。

## 1. 落点与机制

唯一落点：`scripts/e2e/run_all.sh` 的入口（`make e2e` / `e2e-parallel` / `e2e-serial` / `e2e-one` /
`e2e-quick` / `e2e-affected` / `test-affected` 的 e2e 段 / CI 的每个分片 job 全部经过它，
`affected.mjs --run-e2e` 也只是 spawn 它）。没有第二个地方拿锁。

| 项 | 口径 |
| --- | --- |
| 锁文件 | `${TMPDIR:-/tmp}/anotify-e2e-$(id -u).lock`（按 uid 隔离；**常驻不删** —— 删掉会让「已打开的等待者」与「新建者」指向不同 inode，互斥静默失效） |
| 打开方式 | `exec 9>>"$LOCK_FILE"`（**追加**而不是截断：等待者不许把持锁者写下的诊断行抹掉，只有拿到锁才覆盖） |
| 等锁上限 | `E2E_LOCK_WAIT`，默认 **900s**；非整秒数直接 `exit 2` |
| 等不到 | 打印 `holder: pid=<pid> start=<ISO> cwd=<repo> suites=<请求>` 后 **exit 75**（EX_TEMPFAIL）；提示等它结束/换机器/显式 `E2E_LOCK=0` |
| 逃生口 | `E2E_LOCK=0`（**必须显式**；跳过锁 + 打一句风险提示）。默认值永远是「上锁」 |
| 缺 `flock` | **硬失败** `exit 1`（不静默降级成并发）：装 util-linux，或显式 `E2E_LOCK=0` |
| 释放 | 内核在进程退出/崩溃（含 `kill -9`）时释放；**所有子命令都带 `9>&-`**，锁的生命周期严格等于矩阵进程，残留子进程不许替它延长锁 |
| 粒度 | **矩阵**：矩阵内 `--parallel` 的 N 个套件并发；`shard.mjs` 的每个分片各是一个矩阵（CI 上不同机器 ⇒ 互不等待；同一台机器上跑两个分片会被串行化 —— 那正是想要的） |

为什么锁在矩阵层而不是套件层：套件之间本来就该并发（`--parallel`、分片、`E2E_CONCURRENCY`），
被碰撞的是**矩阵之间**（`lib/harness.mjs` 的 `findFreePort` 是「探测 → close → 稍后使用」，
两个矩阵同时探测同一空闲端口会**都通过**；再叠加共享的 `/tmp`、浏览器 profile 与 CPU）。
`make check-e2e-lock` 的 ⑧ 会在有人把锁加到 `parallel_runner.mjs` / `lib/` / `suites/` 时判红。

诊断字段：`pid` + `start`（`date -Is`）+ `cwd` + `suites`。破坏性残留（上一次被 kill -9 的
pid 文本还在文件里）会被标注 —— `pid N 已不在：锁已由内核释放，这行只是残留文本` —— 不会被
误读成「有人正在跑」。

## 2. 验收（6 条，逐条实测）

环境：本机 12 核，常驻 kind/k8s（load ≈ 4–6）；除下述并发实验外，没有别的 e2e 在跑。

### ① 两份矩阵不许同时跑（两终端）

终端 A `./scripts/e2e/run_all.sh --serial capacity`（持锁 147.6s），随后：

- 终端 B `E2E_LOCK_WAIT=600 ./scripts/e2e/run_all.sh --serial frontend`
  → 先等锁、A 结束后开跑、**绿**：
  ```
  ▶ 本机已有 e2e 矩阵在跑（holder: pid=3756021 start=2026-11-22T04:10:20+08:00 cwd=…/anotify-529-e2e-lock suites=--serial capacity ）；等锁最多 600s…
    ✅ 等到锁（本进程已跑 151s）
  🔒 机器级 e2e 锁：/tmp/anotify-e2e-1000.lock（pid=3756131）
  …
  ✅ E2E 全绿：1/1 个请求套件均达标（断言 43 通过 / 0 失败）      B 总墙钟 309s（含 151s 等待）
  ```
- 终端 C `E2E_LOCK_WAIT=3 ./scripts/e2e/run_all.sh --serial vars_ui` → **exit 75**（4s 内返回，
  一条套件都没启动）：
  ```
  ▶ 本机已有 e2e 矩阵在跑（holder: pid=3756021 start=2026-11-22T04:10:20+08:00 cwd=… suites=--serial capacity ）；等锁最多 3s…
  ❌ 等锁 3s 超时，另一个 e2e 矩阵仍占着 /tmp/anotify-e2e-1000.lock（holder: pid=3756021 start=… cwd=… suites=--serial capacity ）；等它结束或换台机器，确认无共享资源冲突时才可显式 E2E_LOCK=0 强行并发
  C_EXIT=75
  ```

### ② 单跑不受影响（贴数字）

同一台机器、同一份产物、`--serial frontend` 各跑一次（前后相隔 5 分钟）：

| 运行 | 总墙钟 | 套件墙钟 | 断言 |
| --- | --- | --- | --- |
| 锁开（`E2E_LOCK=1`） | **158s** | 156.3s | 43 通过 / 0 失败 |
| 锁关（`E2E_LOCK=0`） | **157s** | 156.3s | 43 通过 / 0 失败 |

锁本身的开销（只拿锁不干活：`E2E_LOCK_PROBE=0 bash scripts/e2e/run_all.sh`，n=7）：
median **11.4ms**、min 8.9ms（对照 `bash -c exit`：median 5.7ms）。对 150s 量级的矩阵是噪声级。

### ③ 崩溃不留锁（`kill -9`，贴实测）

真矩阵 `./scripts/e2e/run_all.sh --parallel live_agent ws_protocol deploy contract`，
等它持锁并在跑套件后 `kill -9` **锁文件里记的那个 pid**（就是 `run_all.sh` 本体，pid=3946114）：

```
锁里记的持锁进程 holder pid=3946114
3946114 3946113 bash ./scripts/e2e/run_all.sh --parallel live_agent ws_protocol deploy contract
此刻套件进程：4 个
kill -9 3946114 之后立刻再取锁：exit=0 用时=22ms
    🔒 机器级 e2e 锁：/tmp/anotify-e2e-1000.lock（pid=3946592）
    lock-probe: lock=/tmp/anotify-e2e-1000.lock holder=pid=3946592 start=2026-11-22T04:50:19+08:00 …
新持有者写的锁内容：pid=3946592 start=2026-11-22T04:50:19+08:00 cwd=… suites=（全量）
残留的矩阵子进程（证明不是靠清理释放的）：4 个套件 / 3 个执行器
孤儿矩阵自己收尾：✅ E2E 全绿：4/4 个请求套件均达标（断言 474 通过 / 0 失败）
```

关键点：**释放来自内核**，不是来自我们的清理 —— 被杀矩阵的 4 个套件进程与 3 个执行器当时还活着
（这正是「子命令一律 `9>&-`」换来的性质：锁不跟着子进程走）。`make check-e2e-lock` 的 ④ 把这条
行为钉成秒级用例（并顺带断言残留 pid 文本被新持有者覆盖）。

### ④ 分片模式仍并发（锁在矩阵层，不在套件层）

- 单个矩阵内部：并行全量跑时每 0.5s 采样 `suites/*.mjs` 进程数，
  峰值 **20 个套件进程同时在跑**（直方图各值：2×6、3×194、4×132、5×144、6×49、7×96、8×6、9×13、10–13 各 2、16×3、17×1、19×1、20×4）。
- 分片：`node scripts/e2e/shard.mjs --shards 4 …` 给出 4 片计划，**每片一个矩阵**；
  CI 的分片 job 就是 `./scripts/e2e/run_all.sh --parallel $SHARD_SUITES`（4 台机器 ⇒ 4 个独立
  `/tmp` ⇒ 谁都不等谁）。`node --test scripts/e2e/shard.test.mjs` 绿（fail 0）。
- 自测 ⑧：`parallel_runner.mjs` / `lib/` / `suites/` 里 grep 不到 `flock` ⇒ 锁没有下沉到执行器层。

### ⑤ `make ci` 与 `make e2e-affected`

- `make ci`：**exit 0**（141s）。契约静态断言 / gofmt / 前端产物 / vet+staticcheck / `go test ./...`
  全 ok（`internal/server` 62.4s、`internal/store/v4test` 40.9s、`internal/store` 12.8s…）；
  基准冒烟 `BenchmarkDispatchChain 2099265 ns/op  123264 B/op  2281 allocs/op`；
  harness 自测通过；**新增的 `make check-e2e-lock` 8 项通过**；注释密度「与基线完全一致」。
- `make e2e-affected`（本次改动命中 `scripts/e2e/run_all.sh` ⇒ 全量 30 套）：**exit 0，355s**
  ```
  总耗时: 353.6s
  断言通过: 2820
  断言失败: 0
  验收 authority: real 26 / explicit-mock 4
  ✅ E2E 全绿：30/30 个请求套件均达标（断言 2820 通过 / 0 失败，authority 清单覆盖 30 个）
  最慢套件: frontend 166.2s · ui_shift 149.9s · capacity 147.8s · i18n_coverage 73.8s · i18n 56.8s
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 354.4s）
  ```
- `scripts/e2e/known-env-failures.json` 的 `entries` 仍为 `[]`（**未动**）。
- 注释密度棘轮**零改动**：`scripts/e2e` 目录 634 行、`scripts/e2e/run_all.sh` 单文件 39 行、
  仓库总量 43334 行，全部与基线逐字相等（`make check-comment-density` 通过，基线文件无 diff）。

### ⑥ 文档

- `AGENTS.md §7`：一条（锁的位置、`E2E_LOCK_WAIT`、exit 75、`E2E_LOCK=0` 的语义与风险、粒度）。
- `E2E_TESTING.md`：新增「机器级互斥（#529）」整节（机制表 / 诊断 / 分片与 CI 的关系 / 自测覆盖）。
- `DEVELOPMENT.md`：`make e2e` 段落加三行指针。
- 本报告（`docs/reports/e2e-lock.md`）。

## 3. 自测（`make check-e2e-lock`，已进 `make ci`）

八个方向，约 6s，用**私有 TMPDIR**（不碰本机真实 e2e 锁，因此 `make ci` 与正在跑的真 e2e 可共存）：

| # | 断言 |
| --- | --- |
| ① | 无争用：立刻拿锁，锁就在 `$TMPDIR` 里 |
| ② | 争用：等待后成功（不是静默并发），并打印 holder |
| ③ | 超时：`E2E_LOCK_WAIT=1` → exit 75，且打印 `holder: pid=<pid> start=<ISO>`、上限、逃生口 |
| ④ | 崩溃：`kill -9` 持锁进程后立刻可再取，残留身份被新持有者覆盖 |
| ⑤ | 残留文本被标注「pid 已不在」，且非 `run_all.sh` 的 `flock` 持锁者也认 |
| ⑥ | `E2E_LOCK=0` 在有人持锁时也能跑，但必须打风险提示 |
| ⑦ | 缺 `flock` → 硬失败（不静默降级） |
| ⑧ | 锁没落到执行器/套件层（`--parallel` 仍并发） |

为什么这个脚本没有注释（连 shebang 也没有）：注释密度是**零和棘轮** —— `scripts/e2e` 目录 634 行、
`run_all.sh` 单文件 39 行都逐字冻结，「不上调基线」意味着新文件的每一个注释行都要从别处删掉同样多。
所以它的规格写在 `E2E_TESTING.md` 与本报告里，脚本本体只留可执行断言与自解释输出
（由 `make check-e2e-lock` 驱动，`bash scripts/e2e/lock_selftest.sh` 亦可直接跑）。

## 4. 附带发现（与本改动无关，未改断言、未登记 known-env）

本机全量并行（30 套）跑 `make e2e-affected` 时观测到 3 次**负载型**单套件假红，每次都单跑复验绿：

| 次数 | 套件 | 失败形态 | 单跑复验 |
| --- | --- | --- | --- |
| 1 | capacity | 1 条日志断言（`retention sweep deleted rows` 未及时出现在日志尾） | `--serial capacity` → 84 通过 / 0 失败 |
| 2 | routing | `Error: database is locked`（busy_timeout 5s 被耗尽） | `--serial routing` → 295 通过 / 0 失败 |
| 3 | vars_ui | `page.waitForResponse: Timeout 15000ms exceeded` | `--serial vars_ui` → 74 通过 / 0 失败 |

形状与 #529 开头贴的两次实测**数字完全一致**（routing 单跑 295/0、vars_ui 单跑 74/0），可印证一件事：
锁挡掉的是**矩阵之间**的碰撞；同一矩阵内部 30 套并发 + 机器上别的重活（kind/k8s、别的 worktree）
造成的负载抖动是另一个面（per-suite 预算 / busy_timeout，属 #488 那一层），不在本 issue 的处理范围。
按仓库纪律没有把这类抖动写进 `known-env-failures.json`（继续只许变小）。

## 5. 遗留风险

- **旧代码的矩阵看不见锁**：别的 worktree 若还跑着未合入本改动的 `run_all.sh`，它不参与互斥 ——
  本锁只能协调「已经带锁的矩阵」。合入后自然收敛。
- **同机多分片会被串行化**：有人在一台机器上并发跑多个分片时，第 2 个会等锁（最多 900s 后 exit 75）。
  CI 是 4 台 GitHub runner，不受影响；本地手动跑分片请分机器或设 `E2E_LOCK=0`（并接受碰撞风险）。
- **`E2E_LOCK_PROBE` 是自测钩子**：它只拿锁、不跑任何套件，因此不构成「绕过并发保护」的口子；
  真实运行没有人设它。
- **`flock` 依赖 util-linux**：macOS 默认没有（缺了就硬失败并提示逃生口，不会静默并发）。

## 6. 改动文件

| 文件 | 作用 |
| --- | --- |
| `scripts/e2e/run_all.sh` | 入口加锁（`flock` + 等待/超时/逃生口/诊断/`9>&-`），注释预算内重排 |
| `scripts/e2e/lock_selftest.sh` | 新增：八个方向的自测（私有 TMPDIR，约 6s） |
| `Makefile` | 新增 `check-e2e-lock`，并接进 `make ci` |
| `E2E_TESTING.md` / `AGENTS.md` / `DEVELOPMENT.md` | 锁与逃生口的语义、风险、诊断 |
| `scripts/check-secrets.mjs` | 顺带修一处指向 `run_all.sh` 的过期行号引用（VAPID 段位置变了） |
| `docs/reports/e2e-lock.md` | 本报告 |
