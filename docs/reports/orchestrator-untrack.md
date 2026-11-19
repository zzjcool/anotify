<!-- snapshot-doc: 2026-11-19 把 .pi-orchestrator/ 摘出版本时的一次性台账（数字是那天的定格，复算命令见各节） -->

# 把 `.pi-orchestrator/` 摘出版本（PR #445）· 台账

> **本表的路径写法**：所有「原路径」列都**省略** `.pi-orchestrator/` 前缀 —— 那个目录已不入库，
> 在仓库文件里带前缀写它现在就是死链，`scripts/check-doc-links.mjs` 会判红（§5）。
> 迁移后的文件头保留了完整出处行（`> 迁移自 …`），那行由守卫单独按「来历」核验。

## 0. 结论

| 项 | 结果 |
| --- | --- |
| 迁移文件 | **32 份**（契约 15 / 历史证据 17） |
| 死链 | **REF_DEAD 0**（另有 REF_MIGRATED 32、REF_LOCAL 48、REF_HIST 1，逐条列在 §5） |
| `git ls-files .pi-orchestrator` | **322 → 0** |
| 磁盘文件 | **322 份原样保留**（`git rm -r --cached`，工作区一个字节没动） |
| `make ci` | 基线 **EXIT=0** → 本 PR **EXIT=0**（棘轮 300/300、385/385 未动，见 §7） |

## 1. 为什么不能只 `.gitignore`

`.pi-orchestrator/` 被 40 多处当「唯一事实源 / 权威契约」引用。直接 untrack 不会让任何测试变红
（这些路径在本地磁盘上仍然存在），但 **fresh clone / 新 worktree 里一个都找不到** ——
这正是最贵的一类假绿：读者顺着「唯一事实源」找过去，扑空且没有任何红灯。

## 2. 引用清单（改动前）

扫描命令（任务书给的那条）：

```
grep -rn "\.pi-orchestrator" --include="*.md" --include="*.go" --include="*.mjs" \
  --include="*.sh" --include="Makefile*" --include="*.yml" --include="*.yaml" \
  --include="*.json" . | grep -v "^./.pi-orchestrator/"
```

命中 **50 处**。其中**文件级引用 40 处**（必须迁或改写），其余 10 处是目录/通配常量
（排除清单、截图落盘目录、gitignore 规则 —— 见 §5 的 REF_LOCAL）。

> **这条命令漏了两种文件**：它只按给定扩展名扫，漏掉 `*.py` 与 `*.js`。
> 后来写守卫按 `git ls-files` 全量扫（§5），又抓出 2 处真死链：
> `scripts/check_openapi.py:10`（`v4/API-SPEC-HANDOFF.md`）与 `web/partials.js:3448`
> （`CONTRACT-agent-upgrade.md`）。所以最终判据以守卫为准，不以 grep 的 include 列表为准。

### 2.1 文件级引用 → 处理

| 引用方（改动前） | 被引用（原路径，省略前缀） | 处理 |
| --- | --- | --- |
| `AGENTS.md:63` | `v4/CONTRACT.md` | 改指 `docs/contracts/v4/CONTRACT.md` |
| `AGENTS.md:89` | `TASKS.md` | 改写：协调者本地台账（不入库），不再给路径 |
| `DEVELOPMENT.md:673` | `v4/CONTRACT.md` | 改指迁移后路径 |
| `DEVELOPMENT.md:759` | `UI-EPIC-369/CONTRACT.md` | 改指 `docs/history/UI-EPIC-369/CONTRACT.md` |
| `README.md:20` / `README.en.md:22` | `TASKS.md` | 「仓库文档导航」表里删掉该行（它已不是仓库文档） |
| `design/ui-v2/REVIEW{,-2,-3}.md:3` | `UI-V2-CONTRACT.md` | 改指 `docs/contracts/UI-V2-CONTRACT.md` |
| `docs/design/agent-upgrade.md:12` | `CONTRACT-agent-upgrade.md` | 改指 `docs/contracts/CONTRACT-agent-upgrade.md` |
| `docs/design/playbooks.md:14` | `console-declares/{ISSUE,CONTRACT-artifact,CONTRACT-protocol}.md` | 改指 `docs/contracts/console-declares/…` |
| `docs/design/v4/decisions.md:160` | `v4/SLOT-HANDOFF.md` | 改指 `docs/contracts/v4/SLOT-HANDOFF.md` |
| `docs/lint-guards.md:6` | `UI-EPIC-369/*`、`UI-REVIEW-2026-11-13.md`、`UI-AUDIT-v3.md` | 三者一并迁移，改指 `docs/history/…` |
| `docs/perf/c-report.md:10` | `issues/007-perf-agent-v3-frames.md` | 迁移 → `docs/history/issues/007-…` |
| `docs/perf/capacity-and-backpressure.md:14,173` | `issues/006-…`、`issues/011-…` | 迁移 → `docs/history/issues/…` |
| `docs/reports/capability-lifecycle.md:6` | `cap-lifecycle/WIRE.md` | 迁移 → `docs/contracts/cap-lifecycle/WIRE.md` |
| `docs/reports/l4-guard.md:102` | `UI-EPIC-369/CONTRACT.md:45` | 迁移，`:45` → `:48`（插入出处行后行号下移） |
| `docs/research/mygo-desktop-client-feasibility.md:5` | `.pi-orchestrator/`（整目录当事实源） | 从事实源清单里去掉（已不是可核验事实源） |
| `docs/research/…:132` | `receiver-capability-design.md:216-218` | 迁移，`:216-218` → `:218-220` |
| `docs/v4/e2e-inventory.md:94,162` | `v4/issues/dispatch/TRACKER.md`（:58） | 迁移 → `docs/history/v4-dispatch/TRACKER.md`（:58 → :61） |
| `e2e-perf-requirements.md:4` | `worktrees/wt-e2e-perf` | 改写：本地 worktree 名 + 分支 + base，不给路径 |
| `passkey-errmsg-{plan,requirements}.md:4` | `worktrees/wt-passkey-errmsg` | 同上 |
| `g2-resume-evidence/E1-EMIT-RECEIPT.md:3` | `v4/EMIT-FAILED-TASK-RECEIPT-HANDOFF.md` | 迁移 → `docs/contracts/v4/…` |
| `g2-resume-evidence/G2-WRITER-VERDICT.md:79` | `v4/teams/core-runtime/TEST-MAPPING.md` | 迁移 → `docs/history/v4-core-runtime/…` |
| `internal/server/version_seam.go:6` | `CONTRACT-agent-upgrade.md §1` | 改指迁移后路径 |
| `internal/store/AGENTS.md:60` | `issue-n1-body.md` | 迁移 → `docs/history/issue-n1-body.md` |
| `scripts/e2e/suites/api_contract.mjs:39` | `v4/AUTH-HANDOFF.md` | 迁移 → `docs/contracts/v4/AUTH-HANDOFF.md` |
| `scripts/e2e/suites/<login>_login.mjs:4` | `<login>-login-contract.md` | 迁移 → `docs/contracts/<login>-login-contract.md` |
| `scripts/e2e/parallel_runner.mjs:105,173` | `v4/issues/dispatch/TRACKER.md` | 改指 `docs/history/v4-dispatch/TRACKER.md` |
| `scripts/perf/b/lib.sh:171,258`、`s2-roundtrip.sh:29` | `issue-n3-body.md` | 迁移 → `docs/history/issue-n3-body.md` |
| `scripts/check-annotations.mjs:59,204,1924,1928` | `v4/CONTRACT.md` | 决策文档候选改为迁移后路径（含自测 fixture） |
| `scripts/check-doc-links.mjs`（新增） | — | 本 PR 的守卫，§5 |
| `scripts/check_openapi.py:10` | `v4/API-SPEC-HANDOFF.md` | 迁移 → `docs/contracts/v4/API-SPEC-HANDOFF.md` |
| `web/partials.js:3448` | `CONTRACT-agent-upgrade.md` | 改指 `docs/contracts/CONTRACT-agent-upgrade.md` |
| `scripts/v4-frontend.test.mjs:6376` | `v4/teams/frontend/f3-ui-cases.mjs` | **不改**：`git show 9022863:…` 读的是历史 commit 里的 blob（`cat-file -e` 实测存在），与本目录是否入库无关 |

### 2.2 目录/通配引用（不是产物链接，保留但登记）

| 引用方 | 形态 | 为什么不算死链 |
| --- | --- | --- |
| `.gitignore:45-57`、`.dockerignore:2` | `.pi-orchestrator/`、`tasks/`、`out/`、`**/*.gz`、`v4/teams/` | 忽略规则本身 |
| `scripts/check-annotations.mjs:113,187,218` | `.pi-orchestrator/` | 扫描排除清单（它还顺带解释了文档份数为什么小于文件数） |
| `scripts/check-secrets.mjs:35,248,303` | 目录名 / `**` | 注释里的扫描面说明 |
| `scripts/e2e/affected-map.mjs:29` | `.pi-orchestrator/**` | 改动选择表：该目录**不是运行输入**（本就标了 `unversioned: true`，现在名副其实） |
| `docs/*/…`（迁移件内的 `out/`） | `out/issue-<N>/` 等 | 截图落盘目录，gitignored，PR body 里引用路径 |
| `docs/contracts/<login>-login-contract.md:269` | `.pi-orchestrator/**` | 权限清单（谁拥有哪些路径），不是文档链接 |

## 3. 迁移映射（32 份，只迁被引用的）

统一在文件头插入出处行：`> 迁移自 .pi-orchestrator/<原路径>（2026-11-19，协调者工作区不再入库）`；
带定格数字的原件再加一行 `> snapshot-doc: …`（只豁免计数棘轮，见 §7）。

### 3.1 `docs/contracts/`（契约，15 份）

| 原路径 | 新路径 |
| --- | --- |
| `v4/CONTRACT.md` | `docs/contracts/v4/CONTRACT.md` |
| `v4/SLOT-HANDOFF.md` | `docs/contracts/v4/SLOT-HANDOFF.md` |
| `v4/AUTH-HANDOFF.md` | `docs/contracts/v4/AUTH-HANDOFF.md` |
| `v4/API-SPEC-HANDOFF.md` | `docs/contracts/v4/API-SPEC-HANDOFF.md` |
| `v4/EMIT-FAILED-TASK-RECEIPT-HANDOFF.md` | `docs/contracts/v4/EMIT-FAILED-TASK-RECEIPT-HANDOFF.md` |
| `v4/RESULT-ERROR-PROJECTION.md` | `docs/contracts/v4/RESULT-ERROR-PROJECTION.md` |
| `v4/SOURCE-NAME-CLOSURE.md` | `docs/contracts/v4/SOURCE-NAME-CLOSURE.md` |
| `CONTRACT-agent-upgrade.md` | `docs/contracts/CONTRACT-agent-upgrade.md` |
| `UI-V2-CONTRACT.md` | `docs/contracts/UI-V2-CONTRACT.md` |
| `console-declares/ISSUE.md` | `docs/contracts/console-declares/ISSUE.md` |
| `console-declares/CONTRACT-artifact.md` | `docs/contracts/console-declares/CONTRACT-artifact.md` |
| `console-declares/CONTRACT-protocol.md` | `docs/contracts/console-declares/CONTRACT-protocol.md` |
| `cap-lifecycle/WIRE.md` | `docs/contracts/cap-lifecycle/WIRE.md` |
| `receiver-capability-design.md` | `docs/contracts/receiver-capability-design.md` |
| `<login>-login-contract.md` | `docs/contracts/<login>-login-contract.md` |

### 3.2 `docs/history/`（issue / perf / Epic 证据，17 份）

| 原路径 | 新路径 |
| --- | --- |
| `issues/006-perf-harness-stale-v4.md` | `docs/history/issues/006-perf-harness-stale-v4.md` |
| `issues/007-perf-agent-v3-frames.md` | `docs/history/issues/007-perf-agent-v3-frames.md` |
| `issues/011-rss-sawtooth-vs-live-heap.md` | `docs/history/issues/011-rss-sawtooth-vs-live-heap.md` |
| `issue-n1-body.md` | `docs/history/issue-n1-body.md` |
| `issue-n3-body.md` | `docs/history/issue-n3-body.md` |
| `v4/issues/dispatch/TRACKER.md` | `docs/history/v4-dispatch/TRACKER.md` |
| `v4/issues/dispatch/338-evidence.md` | `docs/history/v4-dispatch/338-evidence.md` |
| `v4/teams/core-runtime/TEST-MAPPING.md` | `docs/history/v4-core-runtime/TEST-MAPPING.md` |
| `v4/teams/core-runtime/DELIVERY-PROGRESS.md` | `docs/history/v4-core-runtime/DELIVERY-PROGRESS.md` |
| `v4/teams/core-runtime/task-cards/DELIVERY-B-ROUTE-REWORK.md` | `docs/history/v4-core-runtime/task-cards/DELIVERY-B-ROUTE-REWORK.md` |
| `UI-EPIC-369/{CONTRACT,LANES,PLAN,REVIEWER}.md`、`report-381.md` | `docs/history/UI-EPIC-369/`（整目录一套证据，含 CONTRACT） |
| `UI-REVIEW-2026-11-13.md` | `docs/history/UI-REVIEW-2026-11-13.md` |
| `UI-AUDIT-v3.md` | `docs/history/UI-AUDIT-v3.md` |

**为什么 `UI-EPIC-369/CONTRACT.md` 进了 `docs/history/` 而不是 `docs/contracts/`**：
`docs/lint-guards.md` 引用的是 `UI-EPIC-369/*` 整个目录（一批 Epic 证据），把其中一份拆到别处
会让「这个目录是那轮 Epic 的完整台账」这句话半真半假。目录整体保留，`CONTRACT.md` 仍是这份台账的冻结契约。

**只迁被引用的**：`.pi-orchestrator/` 另有 290 份没被任何仓库文件引用（`STATUS.md`、`PLAN.md`、
`verdicts/`、`archive/` 等）—— 它们仍留在协调者本地，不进版本。

## 4. untrack 前后

```
$ git ls-files .pi-orchestrator | wc -l
322                    # 改动前
0                      # git rm -r --cached .pi-orchestrator 之后

$ find .pi-orchestrator -type f | wc -l
322                    # 磁盘未删（--cached 只动索引）

$ ls .pi-orchestrator | head -3
CONTRACT-agent-upgrade.md
CONTRACT-cycles-priority-batch.md
CONTRACT-downstream-edit.md
```

`.gitignore` 里的 `.pi-orchestrator/` 由 #443 提供，本 PR 未改（`git check-ignore` 确认命中第 45 行）。

## 5. 死链检查（可复跑）

```bash
$ node scripts/check-doc-links.mjs --self-test   # 判据自测：红/绿双侧 13 组
$ node scripts/check-doc-links.mjs               # 全仓（make ci 里的就是这条）
```

判据面是 **`git ls-files`**，不是工作区 —— 本地那 322 份文件一个不少，拿文件系统判会得到
「全员绿」的假绿。每一条引用按四种情形定性：

| 判定 | 含义 |
| --- | --- |
| `REF_MIGRATED` | 文件头的 `> 迁移自 …` 出处行，且同名文件确实落在 `docs/contracts/` 或 `docs/history/` 下（**来历也要能核验**） |
| `REF_HIST` | `<rev>:<path>` 形态（`git show 9022863:…`），只要求该 rev 下真有这个 blob |
| `REF_LOCAL` | 目录 / 通配 / 占位符（`.pi-orchestrator/**`、`out/`、`{CONTRACT,PLAN}.md`）——本地目录约定，不算产物链接，逐条打印不静默 |
| `REF_DEAD` | 其余：指向 fresh clone 里不存在的具体文件 → 非零退出 |

输出（本 PR 的树）：

```
协调者工作区引用检查 · `.pi-orchestrator/` 不再是仓库内容
  扫描：848 个被跟踪文件（判据是 git ls-files，不是工作区）
  REF_OK  0 / REF_HIST  1 / REF_MIGRATED 32 / REF_LOCAL 48 / REF_DEAD  0
  ✅ REF_DEAD 0：仓库里没有指向协调者工作区的悬空引用
```

`REF_OK 0` 是预期的：目录已不入库，**没有任何** `.pi-orchestrator/<文件>` 还能解析成被跟踪路径。
`REF_MIGRATED 32` = 32 份迁移件的出处行全部落到了真实文件上；`REF_LOCAL 48` 就是 §2.2 的登记清单，再加上本文件里为了讲清这件事而写的那些目录名。

守卫自己的红侧也自证了（否则它只是个恒绿的装饰）：
`--self-test` 里「指向不存在文件」判 `REF_DEAD`、「迁移声明指向不存在的落点」也判 `REF_DEAD`。

已接进 `make ci`（`check-doc-links`，紧跟 `check-annotations`）。

## 6. 渲染

- `make fe`（sitegen + 指纹）在改动后重跑：`✅ sitegen done: 4 languages × 21 pages`，产物无 diff。
- 文档页构建的判据测试：`go test ./internal/sitegen -run TestDocsPageRendersFiveSpecFixturesFourLanguages -v`
  → `--- PASS (0.52s)`。（docs 页的输入是 `api/` 五份 spec，不是 `docs/*.md`；迁移的 md 不参与渲染，
  但仍跑一遍证明站点构建面没被带坏。）

## 7. `make ci`

| 阶段 | 结果 |
| --- | --- |
| 基线 `origin/chore/ignore-orchestrator-dir`（3295dc5c） | **EXIT=0** |
| 本 PR | **EXIT=0** |

`check-annotations` 的两个棘轮（只许降）在迁移后**一动没动**：

| 项 | 基线 | 迁移后 |
| --- | --- | --- |
| 扫描文件（代码/文档） | 624（568/56） | 658（569/89） |
| 引用（已解析/未解析/占位符） | 818（789/3/25） | 1105（1057/13/37） |
| unbacked | 300 / 基线 **300** | 300 / 基线 **300** |
| unmarked | 385 / 基线 **385** | 385 / 基线 **385** |

多出来的 32 份历史原件里带大量计数断言（`13 个套件`、`177 项`…），如果让它们进 D 面，
`unmarked` 会直接冲到 413。处理方式是仓库已有的机制 —— 文件头 `snapshot-doc:`（**只**豁免计数棘轮，
引用/锚点照判），共 25 份；没有计数断言的文件不标（否则 `SNAPSHOT_STALE` 会红，本 PR 就撞过一次）。
**没有上调任何基线，也没有放宽任何判据。**

带历史 v3 引用的 7 份原件用 `frozen-doc:` 标注（同样是仓库既有机制）—— 那些引用指向 `cmd/agent` 与 `make demo-chain` 这类**已随 v3 节点产品退役**的东西，原文不重写，只标冻结，
`check-annotations` 因此把 13 条未解析引用计入 `冻结标记文档 7（其中抑制引用 13 条）` —— 是「计入并具名列出」，
不是静默放过；只要某份文件的引用全部解析成功，`FROZEN_STALE` 会要求撤掉标记。

### 7.1 唯一一处「下调」（不是放宽）

`scripts/check-secrets.mjs` 的反假绿下限 `FILES_MIN` 从 900 降到 **735**：该守卫要求「扫描文件数 > 下限」，
而这次仓库真的缩了 322 份（被跟踪文件 992 → 814，实测扫描 977 → 799）。这是该常量注释里写明的处置
（「真遇到仓库大幅缩水要同步下调时，在 PR 里写清原因」），按同一个 92% 比例重算；跳过规则与检测器
（17 条）一条没动，检测器下限仍是 15。改后实测扫描 833 文件 > 735。

## 8. 遗留与风险

1. **`.pi-orchestrator/` 仍然存在但不入库**：本地 322 份文件照旧，协调者工作流不变；危险动作是
   「用 `git add -A` 之外的显式路径提交本地产物」——`.gitignore` 第 45 行会拦住整目录。
2. **`scripts/v4-frontend.test.mjs:6376` 依赖历史 commit `9022863`**：只读 blob，不受 untrack 影响；
   但若将来对该仓库做历史重写（filter-repo 之类），这条会失效 —— 它本来就是「可选基线对比」模式。
3. **`docs/history/` 是只读档案**：里面的行号/链接是当时的定格，不要当现行契约读；
   现行契约是 `docs/contracts/`、`docs/design/`、`api/`、`internal/store/schema.sql`。
4. **两个 `path:line` 引用改了行号**（`UI-EPIC-369/CONTRACT.md:45→48`、`TRACKER.md:58→61`）：
   因为文件头插入了出处行。`check-agents` 只校验 `AGENTS.md` 里的 `path:line`，这两处在散文里，
   由本次人工核对 + `check-doc-links` 的 basename 核验兜底。
