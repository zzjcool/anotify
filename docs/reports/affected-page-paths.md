> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 本报告里的数字是本次交付落地时的定格（复算命令见 §7） -->

# affected 选择器：页面文件按 `pages ∪ paths` 的并集选套件（#524）

> 症状：只改 `web-src/pages/playbook.html` 时，覆盖该页 release/版本/卸载交互的
> `playbook_release` 套件**不会被 affected 选择器选中**。#521 交付时实测 `make e2e-affected`
> 选了 16 个套件（含 frontend / ui_shift / i18n），**不含 playbook_release**，只能手工补跑 225 条断言。
>
> 这是 #502 同族的**漏跑**：选择器算错不是"多跑"，是"真功能套件默认不跑"。

## 1. 根因（代码事实）

两处各自都"对"，合起来就是漏跑：

| 位置 | 事实 |
| --- | --- |
| `scripts/e2e/affected.mjs` 的「③ 页面」分支 | owner 只由 `row.pages` 算出，随后 `continue` —— `row.paths` 在这条分支上**永远不参与** |
| `scripts/e2e/affected-map.mjs` 的 `playbook_release` 行 | 该页登记在 **`paths`**（`"web-src/pages/playbook.html"`），`pages` 为空 |

于是「用 `paths` 登记页面」这件事在数据里成立、在判定里不成立。#533 之前的实测：
改 `web-src/pages/playbook.html` → `frontend, i18n, i18n_coverage, ui_shift`（4 个），
`playbook_release` 缺位。同源问题还有一个：页面没有任何 `pages` 登记、只被 `paths` 覆盖时，
页面分支会把它判进 `unknown`（误报「没人覆盖」）。

## 2. 修法

**A（根因修复）：页面分支取并集，不再提前按 `pages` 定死 owner。**

`scripts/e2e/affected.mjs` 的 ③ 分支现在对每个套件行同时看两处，命中任一即入选：

- `row.pages === "ALL"` 或 `row.pages.includes(page)` → 理由「是它访问的页面（矩阵遍历全部页面）」；
- `row.paths` 里有一条 glob 匹配改动文件 → 理由「落在它登记的页面路径里」。

同一处还处理了一个对称性问题：`web-src/pages/<p>.html` 与生成物 `web/<p>.html` 是**同一个页面**，
两种写法必须算出同一批套件，所以 `paths` 除了匹配改动文件本身，也匹配该页的源路径
`web-src/pages/<p>.html`。否则"改源"与"改产物"会给出两个答案，漏跑就只是换了个入口。

`unknown` 的判据没有放宽：页面名不在 `web-src/pages` 清单里（新增页/改名/打错）照旧 fail-closed。

**B（数据补登记）：`playbook_release` 加 `pages: ["playbook"]`。**

让"这个套件盯着这个页面"的意图在数据里也显式（`why` 里写明了双登记的由来）。
B 是冗余表达，不是修复 —— 判断"修没修"的断言刻意做成**去掉 B 也必须命中**（见 §3.1）。

**影响面（为什么这不会把"改任意页面"扩成全量）**：整张表里只有 `playbook_release` 一个套件把页面
html 登记在 `paths`（`git grep -n "web-src/pages" scripts/e2e/affected-map.mjs` 只有这一行）。
并集带来的增量命中面 = 这一行的显式 glob，`paths` 是逐套件写死的模式，
不是"所有页面都算命中"的通配。

## 3. 三条反向断言的实测

复算：`node --test scripts/e2e/affected.test.mjs`（37 pass）。

### 3.1 只改 `web-src/pages/playbook.html` → `playbook_release` 在结果里，且**不靠 B** 也能命中

| 场景 | 选中 | `playbook_release` 的命中理由 |
| --- | --- | --- |
| 现状（A + B） | `frontend, i18n, i18n_coverage, playbook_release, ui_shift`（5） | 「`web-src/pages/playbook.html` 是它访问的页面」 |
| 把该行的 `pages` 抹掉，只留 A | 同上（5） | 「`web-src/pages/playbook.html` 落在它登记的页面路径里」 |
| 改生成物 `web/playbook.html` | 同上（5） | 与改源一致 |

第二行就是"证明走的是并集而不是只补数据"的那条断言：它只可能因为 A 命中。

### 3.2 只改一个没有任何套件登记的页面 → 仍然是 `unknown`（不许变成静默通过）

两个入口都测：

| 输入 | 结果 |
| --- | --- |
| `web-src/pages/does-not-exist.html`（不在 `web-src/pages` 清单里 = 新增页的形态） | `unknown` 非空 ⇒ **全量 30/30**（既有用例，修完仍绿） |
| 站点清单里存在、但没有任何套件 `pages`/`paths` 登记它（合成用例 `no-suite-page`） | `unknown` 为空**且**选中 = 恰好 3 个 `pages: ALL` 矩阵套件（`i18n`/`i18n_coverage`/`ui_shift`）；既不是空集（静默通过），也不是全量（扩面） |

`paths` 匹配不可能让后者变成"有人覆盖"：它是逐套件白名单，没写就没有。

### 3.3 只改 `capabilities.html` → 套件数不暴涨

上限取「现有 `pages: ALL` 类数 + 该页的 `pages` 登记数」= 3 + 2 = 5；实测选中
`auth_flow, frontend, i18n, i18n_coverage, ui_shift`（5），与修复前一致，`full` 为空，
且不含 `playbook_release`（别的页面不会被 playbook 的 `paths` 带走）。

## 4. 变异测试（把并集改回"只查 pages"）

删掉 ③ 分支里的 `else if (… row.paths …)` 那一支（= 回到 issue 描述的形态）后：

```
✖ #524：页面走 pages ∪ paths 的并集 —— playbook_release 不登记 pages 也必须命中（证的是并集，不是补数据）
  AssertionError: 只靠 paths 也必须命中 —— 否则根因（页面分支不看 paths）没修，只是把数据补了一遍
ℹ pass 36 / fail 1
```

恢复后 `ℹ pass 37 / fail 0`。

一条值得记下来的观察：变异时 COUPLED 里那条「改预案页必须选中 playbook_release」**仍然绿**
—— 因为 B（`pages: ["playbook"]`）单独就能满足它。所以能杀死"只补数据"这种假修的，
只有 3.1 第二行那条"抹掉 `pages` 再断言"的用例；只写一条耦合断言等于没测根因。

## 5. 门禁

| 命令 | 结果 |
| --- | --- |
| `make ci` | **exit 0**（222s，含报告入库后复跑一次；更早一次 162s：contract 静态断言 + gofmt + vet + build + 单测 + 守卫自测 + 注释密度 + 文档链接/端点 + harness 自测 + bench 冒烟） |
| `make e2e-affected` | **30/30 套件 · 断言 2809 通过 / 0 失败** · 总耗时 475.6s（`E2E_CONCURRENCY=2`）。其中 `playbook_release` 225 通过 / 0 失败 —— 正是 issue 里「只能手工补跑」的那 225 条。本次改动落在 `FULL_SURFACES`（改的是选择器本体），所以自己触发全量，这是设计意图 |
| `scripts/e2e/known-env-failures.json` | `entries: []` 保持空（未新增任何登记；本次 0 失败，清单里没有可删项） |
| `make check-comment-density` | 注释行 43335 = 基线 43335；`scripts/e2e` 634 = 基线 634；**基线文件未改**（新增注释由同目录内压缩等量注释抵扣） |
| `node --test scripts/e2e/shard.test.mjs` | 13 pass / 0 fail |
| `node --test scripts/e2e/affected.test.mjs` | 37 pass / 0 fail |

## 6. 遗留（不在本 issue 范围）

`scripts/e2e/affected.test.mjs` 本身在映射表里没有分类，改动它会走「无法判定 ⇒ 全量」。
本次保持原样：改选择器**自己的测试**时跑全量是保守方向（多跑，不漏跑），
要收窄得先给 `NO_E2E_IMPACT` 加「`scripts/e2e/*.test.mjs` 不被任何套件执行」这条
—— 那是独立的一次判定变更，不该混进这次修复。

## 7. 复算命令

```bash
node --test scripts/e2e/affected.test.mjs            # 三条反向断言 + 变异守卫
node --test scripts/e2e/shard.test.mjs               # 计划分片自测
node scripts/e2e/affected.mjs                        # 只打印选择计划（不执行）
make ci                                              # PR 门禁
make e2e-affected                                    # 按改动选套件并执行（本次=全量 30）
node scripts/check-comment-density.mjs               # 注释密度棘轮
node scripts/e2e/affected.mjs --files web-src/pages/playbook.html --json   # 单页命中的机器可读结果
```
