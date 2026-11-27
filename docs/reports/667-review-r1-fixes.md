# #667 review 第 1 轮：逐条处理（PR #697 追加提交）

as_of: 2026-11-27 · PR zzjcool/anotify#697 · 分支 `pi-subagent/issue-667-r1` · 本文件＝ r1 的裁决与取证

snapshot-doc: 2026-11-27 的取证快照（数字是当时实测的定格，不是对未来的承诺）

## 结论

3 条意见全部处理，没有静默跳过：1（blocking）改代码把注释压回基线；2（minor）改 PR 正文措辞，
代码经独立复核确认无需要改；3（minor，可选）采纳「抬到 10s」这一支，并把理由写进注释。

| # | 级别 | 裁决 | 落点 |
| --- | --- | --- | --- |
| 1 | blocking | 认，压注释到基线 | `scripts/e2e/suites/capacity.mjs`（新增 3 行 + `hasDeletionLog` 8→5 行） |
| 2 | minor | 认措辞，代码不改 | PR 正文（4 条正向断言逐条核对后改写） |
| 3 | minor·可选 | 认，上界 5s → 10s | `scripts/e2e/suites/capacity.mjs:395`（注释写清 10s 的出处） |

## 逐条

### 1. [blocking][CI 必红] 注释顶破棘轮

复现（PR head `75e94c99` 的干净工作树，`node scripts/check-comment-density.mjs`）：退出码 1，
`[DIR_OVER] scripts/e2e/suites: 2187 > 基线 2179（超 8 行）` + `[TOTAL_OVER] 42881 > 42873`。
与 review 的数一字不差，新增的 8 行注释就是唯一变量。

棘轮是双向的（超与降都算不一致，`docs/lint-guards.md` L12），所以「把 8 行压成 3 行」还不够 ——
净增必须回到 0。处理：

- 现场只留 **3 行**「为什么必须等」，不复述 `tryPoll` 参数、不再列 #630/#617/#628 的流水账；
- 同文件把 `hasDeletionLog` 那 8 行（其中 3 行是空的 `//` 分隔）压成 **5 行**：只留「只在删了东西时打
  ⇒ 第二轮没有这条＝没有重复删」与「两种日志编码都认」，删掉复述 `slog.Info` 调用形状的部分。

净增 0，落点就在基线：

```
$ make check-comment-density
node scripts/check-comment-density.mjs --self-test
✅ 自测全部通过（超限/等于/低于 三态 + 密集五态 + 口径五测 + docs 面八态 + 基线只降不升）
node scripts/check-comment-density.mjs
🔍 注释密度棘轮（docs/lint-guards.md · L12）
   扫描 779 个文件（下限 600）· 注释行 42873 / 基线 42873
   目录：80 个有基线，0 个与基线不一致

✅ 与基线完全一致（超与降都算不一致）
MAKE_EXIT=0
```

没有抬基线（`--update` 也不许抬），没有动 `scripts/e2e/suites/` 之外的文件凑数。

### 2. [minor] PR 正文与代码不符

review 的事实是对的，我的措辞不准确：除 2 条**否定**断言（`capacity.mjs:504` 永久保留没有删除日志、
`:431` 第二轮不再删）外，同文件还有 **4 条一次性读 `server.log()` 的正向断言**。

独立复核（不是采信 review 的结论，逐处看打点时机）：

| 断言 | 打点位置 | 为什么不是同类竞态 |
| --- | --- | --- |
| `capacity.mjs:330` `server.backpressure.engaged` | `internal/server/backpressure.go:319` | 两条 slog 都在 `g.rejecting.Store(...)`（`:374`）**之前**；断言前那次 `tryPoll` 等的是「再投一条事件拿 200」，200 只可能在 `rejecting=false` 之后 ⇒ 日志必已落盘 |
| `capacity.mjs:331` `server.backpressure.released` | `internal/server/backpressure.go:360` | 同上 |
| `capacity.mjs:358-359` `server.retention.configured` | `internal/server/mux.go:691` | 在 listen 之前打的；`startServer` 返回即已 listen |

所以代码这 4 处**不需要**改，改的是 PR 正文：删掉「同文件另外两处日志断言是否定的」这句，
换成上面这张两分类的表。判据差异一句话：`retention sweep deleted rows` 是**异步巡检**在事务提交后打的，
另外 4 条是**同一条路径上先打日志、后置位/后 listen**，断言所等的那个事实成立时日志已经在了。

### 3. [minor][残留风险] 5s 上界与 `busy_timeout(5000)` 同量级

采纳 review 给的第二支（抬到 10s），理由是这两条不是「差不多所以取大点」：

- `RunRetention` 提交之后还要 drain 交货（`internal/store/retention.go:111`）才返回、返回之后才
  `s.log.Info`（`internal/lease/lease.go:84`），这段写锁等待的硬上限就是 `busy_timeout(5000)`
  （`internal/store/store.go:103`）—— 5s 上界正好压在边界上，最坏情况会被吃满；
- 10s 同时等于验证脚本自己直读库用的 `PRAGMA busy_timeout = 10000`（`scripts/e2e/lib/v4.mjs:390`），
  是这套 harness 已有的 DB 访问预算，不是新发明的数。

第一支（与上面 45s 那条合并成一次 `tryPoll`）没选：合并后失败信息只剩「没等到」，分不清是
「行没删」还是「日志没来」，而这两件事的诊断价值完全不同（前者是清理坏了，后者是可观测性坏了）。
上界没有继续放大到无界：变异自证证明它仍在等满上界后判红。

## 验证（本节点实跑，输出留在 `/tmp/667-r1-*.log`）

**1) 非变异（本分支当前代码）→ 绿**

```
$ ./scripts/e2e/run_all.sh capacity
  ✅ capacity           84通过/0失败  147.6s  mode=real
✅ E2E 全绿：1/1 个请求套件均达标（断言 84 通过 / 0 失败）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）
```

**2) 变异（把 `retention sweep deleted rows` 那条 `s.log.Info` 关成 `if false && (…)`）→ 仍必红**

```
$ ./scripts/e2e/run_all.sh capacity
  ❌ capacity           83通过/1失败  157.7s  mode=real
      ↳ 清理日志给了删了多少（retention sweep deleted rows）
  失败套件: capacity
```

唯一红的就是这条，其余 83 条全绿；墙钟 **157.8s vs 147.7s（+10.1s ≈ 10s 上界）** ⇒ 它确实等满
上界才红，抬上界没有把它变成「永远绿」。变异已还原，`git diff` 只剩 `capacity.mjs` 一个文件。

**3) 廉价守卫（`make ci` 里与本改动相邻的那几个）**

```
$ make check-comment-density          → MAKE_EXIT=0（42873 == 42873，0 个目录不一致）
$ node scripts/check-annotations.mjs  → ANNOT_EXIT=0
$ node scripts/check-test-discipline.mjs → DISC_EXIT=0（五项全部与基线一致）
$ node --check scripts/e2e/suites/capacity.mjs → SYNTAX_OK
```

**4) 提交 `0a35dcbf` 的干净树复跑** → 仍绿：

```
$ ./scripts/e2e/run_all.sh capacity
  ✅ capacity           84通过/0失败  147.4s  mode=real
✅ E2E 全绿：1/1 个请求套件均达标（断言 84 通过 / 0 失败）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 147.6s）
```

**5) `make ci`（PR 门禁本体，r0 漏跑的那一步）**

```
$ (umask 022; make ci)          → MAKE_CI_EXIT=0
```

第一次跑（没显式设 umask）红了，但红点与本次改动无关：
`cmd/anode/internal/install` 的 `TestInstallConfigDirectoryIsTraversableByServiceUser`
—— 它建配置目录期望 0755，而本节点 shell 的 `umask=0077` 把 `MkdirAll(0755)` 削成了 0700。
归因自证：单跑该包，`umask=0077` → FAIL、`umask=0022` → ok；同一棵树只变 umask。
CI runner 的默认 umask 是 0022，所以这不是 PR 的问题，也不是 `make ci` 的真红。
除它之外那一次跑的所有目标（contract / lint-go / fe / check-dist-fresh / 各守卫）都是绿的。

## 遗留 / 未覆盖

issue 验收第 2 条「`E2E_CONCURRENCY` 拉满下连跑 5 次不红」仍留给 CI 矩阵：本节点连跑 5 轮单套件
约 12 分钟，且它的负载形态（多套件并发抢写锁）在单套件串行跑里造不出来。本节点给出的方向性证据是
变异/非变异各一轮 + 合入后干净树一轮。
