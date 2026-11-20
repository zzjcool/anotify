# 注释与文档棘轮（#495）

> 一句话：`check-annotations`（L5）保证留下的注释**不说谎**；本棘轮（L12）保证**它们先得存在**。
> 注释与文档只在「代码无法自述」处保留，其余一律删；删除量由基线锁定，只许往下。
>
> 本 PR 交付 **A**：棘轮 + 基线（= 当前实测值）+ 逐目录判据 + 红/绿自证。**B** 是后续按桶删（§5）。

## 1. 判据

**注释**只允许三类：非显然的取舍、外部约束（内核行为/浏览器差异/协议怪癖）、危险操作警告。
复述函数名、解释显而易见的 `if`、历史流水账、TODO 式吐槽 —— 删。

**文档**只留操作手册（怎么跑/怎么部署/怎么排障）；设计与契约定居代码与测试里。逐目录：

| 目录 | 现状（份 / 行） | 判据 | 结果 |
| --- | --- | --- | --- |
| `docs/design/**` | 0 / 0（#477 已删） | 设计文档是实施前的计划，落地后只能变错，且没有执行者 | **保持删除**；重建即判红（`DOC_DIR_FORBIDDEN`） |
| `docs/v4/**` | 5 / 960 | 实施期清单快照（inventory / manifest）：计数只会腐烂（`e2e-inventory.md` 至今指向已删套件） | B 阶段删（→0） |
| `docs/research/**` | 2 / 422 | 可行性研究 = 计划 | B 阶段删（→0） |
| `docs/perf/**` | 3 / 706 | 一次性测量快照 | B 阶段删（→0）；真要用就并进 `reports/` |
| `docs/contracts/**` | 16 / 2966 | 冻结契约（不变量 / wire 字节 / 决策索引）：**仍被执行者引用**（`@decision` 只解析到这里） | 冻结在实测值，只许下调 |
| `docs/reports/**` | 27 / 5068 | 排障取证，按 PR 增长 | 留（不定上限） |
| `docs/history/**` | 17 / 3269 | 更早的档案与已迁移台账 | 留 |
| `docs/*.md`（根） | 5 / 837 | 操作手册 | 留 |
| 仓库根的 plan/requirements 类 `*.md` | 3 / 193 | 计划不进仓库（进 issue/PR） | B 阶段删（→0）；`ROOT_DOC_ALLOW` 之外的一律进 `rootPlans` 棘轮 |

判据的**执行者**是脚本里的两张表：`DOC_POLICY`（verdict：manual / evidence / shrink / forbidden）
与 `ROOT_DOC_ALLOW`。`shrink` 目录同时钉**文件数与行数** —— 否则 16 份并成 1 份巨型文件就能冒充通过。

## 2. 与 `check-annotations`（L5）的收敛方案（不搞两套打架）

| | `check-annotations`（L5） | 本棘轮（L12） |
| --- | --- | --- |
| 管什么 | **留下的**注释不许说谎：引用可解析、`@guard`/`@decision` 锚点可解析 | **总量**只许降：目录 / 总量 / 单文件密度 / docs 面 |
| 方向 | 逐条追溯（外键完整性） | 单向棘轮（数量） |
| 交集 | L5 的 `CLAIM_UNBACKED` / `COUNT_UNMARKED` 是**分子**，本棘轮的基线是**分母**：删注释会把两边一起往下带 | |

**B 阶段的两处前置条件（本轮不动，但必须先解决，否则删到一半会被 L5 挡住）**：

1. **L5 的硬下限**：`COMMENT_LINES_MIN = 24000`（实测 33986）与 `SCAN_FILES_MIN` / `REFS_MIN`。
   注释量掉到 L5 的下限以下时 L5 自己会红 —— 那时只能显式下调这三个下限（review 可见），不能装作没发生。
2. **`DECISION_DOC_CANDIDATES` 写死了两份 `docs/contracts` 文件**：
   `docs/contracts/**` 要删，必须先把三条 `@decision` 锚点（`V4-D1`，目前全在文档示例里）
   搬进代码或测试，并让 L5 能解析新落点。这是 B7 的阻塞项。

## 3. 基线（= 当前实测值，2026-11-20）

扫描面：**所有被跟踪文件**里 `.go/.mjs/.js/.py/.sh/.html/.css/.sql/.yml/.yaml`，
排除 `web/vendor/`、`internal/server/dist/`、`node_modules/`、`.pi-subagents/`。
HTML 里 `<script>` / `<style>` 的内容按 JS / CSS 计（否则内联脚本的注释被当成代码）。

- 文件 658 / 注释行 43411 / 代码行 ≈263.5k（守卫自己也在扫描面里，改它会让代码行小幅浮动；
  `commentLines` 是判据，`codeLines` 只作参考）
- Go 451 文件 / 注释行 24944 / 代码行 145194（与 #495 里的 24947 基本一致；差 3，来自这次开始解析字符串 —— `"$X"/*)` 那类不再误判）
- 单文件密度超阈值（注释行 > max(12, 代码行 × 0.5)）：**58 文件**，合计 4828 注释行
- 分布：注释行 ≥200 的 35 文件（12479）；≥100 的 125 文件（24524）；≥50 的 274 文件（35187）；≥20 的 467 文件（41672）

按 area 汇总（注释行 / 代码行 / 文件）：

| area | 注释行 | 代码行 | 文件 |
| --- | --- | --- | --- |
| internal | 22753 | 122838 | 382 |
| scripts | 10471 | 54743 | 141 |
| web（手写 js + css + 两个安装脚本） | 4705 | 19766 | 14 |
| web-src（页面 + 内联 script/style） | 2550 | 34032 | 30 |
| cmd | 2367 | 22799 | 73 |
| other（`api/`、`.github/`、根 yml/yaml） | 565 | 9313 | 18 |

逐目录（共 73，数字即基线；基线就写在 `scripts/check-comment-density.mjs` 的 `BASELINE` 块里，跟着判据走）：

| dir | 注释行 | dir | 注释行 |
| --- | --- | --- | --- |
| internal/server | 7765 | . | 89 |
| internal/store | 6821 | cmd/anode/internal/capability | 84 |
| web | 4674 | cmd/anode/internal/config | 77 |
| scripts | 3655 | internal/schema | 73 |
| web-src/pages | 2400 | cmd/anode/internal/node | 70 |
| scripts/e2e/suites | 2182 | scripts/perf/3b | 66 |
| scripts/e2e/fixtures/f3 | 1479 | cmd/anode/internal/app | 61 |
| internal/playbook | 1347 | examples/capabilities | 53 |
| internal/auth | 1259 | web-src/locales | 49 |
| internal/store/v4test | 1208 | internal/version | 48 |
| internal/protocol | 1061 | cmd/anode/internal/emitgw | 43 |
| scripts/perf/c | 801 | scripts/perf/2a-auth | 41 |
| scripts/e2e/lib | 766 | internal/sitegen/testdata/specs | 40 |
| internal/ws | 669 | cmd/anode/internal/clock | 37 |
| scripts/e2e | 634 | internal/lease | 36 |
| internal/sitegen | 471 | cmd/anode/internal/upload | 35 |
| cmd/anode/internal/runner | 432 | cmd/anode/internal/hostenv | 34 |
| internal/artifact | 424 | cmd/fe-preview | 32 |
| scripts/perf/b | 405 | scripts/perf/loadgen | 31 |
| cmd/anode/internal/resultstore | 394 | web/fonts | 31 |
| scripts/perf/3a | 351 | cmd/anode/internal/listen | 29 |
| internal/route | 299 | cmd/anode/internal/applied | 27 |
| internal/sourcespec | 294 | internal/trigger | 25 |
| cmd/anode | 282 | scripts/testdata/fakebroker | 24 |
| api | 271 | internal/logging | 22 |
| internal/yamldoc | 262 | cmd/sitegen | 21 |
| cmd/devseed | 249 | examples/playbooks/pr-triage-notify/capabilities | 21 |
| internal/resultspool | 184 | scripts/migrations | 20 |
| internal/api | 168 | cmd/server | 18 |
| internal/source | 151 | internal/authn | 16 |
| cmd/anode/internal/hub | 134 | scripts/demo | 16 |
| cmd/anode/internal/install | 112 | examples/playbooks/pr-triage-notify | 15 |
| .github/workflows | 108 | examples/playbooks/pr-triage-notify/sources | 8 |
| internal/push | 108 | web-src | 3 |
| cmd/anode/internal/execenv | 99 | internal/server/testdata | 2 |
| web-src/layouts | 98 | scripts/e2e/fixtures | 0 |
| cmd/anode/internal/ops | 97 |  |  |
| **合计** | **43411** | | |

## 4. 红/绿自证

`make check-comment-density` 先跑 `--self-test`（39 组 check，内存 fixture），再判真仓库。三态齐全：

- 超限 → 红；等于基线 → 绿；低于基线 → 红并提示「把基线降到 N」（`DIR_UNDER`/`TOTAL_UNDER`/`DENSE_UNDER`/`DOC_*_UNDER`）
- 反假绿：扫描文件/注释/代码三条下限；`--update` **抬高直接拒**、新目录与密集文件**不许自动入表**；
  登记表过期即红；`--init` 只在**未初始化**时可用（已初始化即拒，堵住「删基线重建 = 全额赦免」）

真实仓库上的变异（本 PR 实测，可逐条复跑）：

1. `--init`（基线已初始化）→ **拒**：`❌ BASELINE 已初始化 —— 只许用 --update 收紧，不许重建`
2. 造 `internal/densityprobe/probe.go`（注释行 60 / 代码行 8）→ **红（3 findings）**：`DIR_NEW`（新目录 60 > 10）、`TOTAL_OVER`（43471 > 43411）、`DENSE_UNPINNED`（该文件注释行 60 / 代码行 8，超阈值 12）
3. 同一文件上跑 `--update` → **拒（3 findings）**：新目录不许自动入表 + 密集文件不许自动入表 + 总量不许抬
4. 删掉该文件 → **绿**（与基线完全一致）
5. 从 `internal/protocol/limits.go` 删两行注释 → **红（3 findings）**：`DIR_UNDER` / `TOTAL_UNDER`（43409 < 43411）/ `DENSE_UNDER`，每条都写着「把基线降到实测值」
6. 跑 `--update` → 总量 43411→43409、`internal/protocol` 1061→1059、`limits.go` 的登记预算（13）作废 → **绿**

## 5. B 阶段（后续 PR，按桶推进，每批同批下调基线）

| 批次 | 目标 | 文件数 | 注释行 |
| --- | --- | --- | --- |
| B1 | 注释行 ≥200 的文件 | 35 | 12479 |
| B2 | 100–199 | 90 | 12045 |
| B3 | 50–99 | 149 | 10663 |
| B4 | 20–49 | 193 | 6485 |
| B5 | <20（收尾到接近零） | 191 | 1739 |
| B6 | 删 `docs/v4` + `docs/research` + `docs/perf` + 根部 3 份 plan | 13 份 | 2281 |
| B7 | `docs/contracts` 16 份 → 0（阻塞：见 §2 的第二条前置条件） | 16 份 | 2966 |

顺序理由：B1 的 35 文件占全部注释的 29%，收益/风险比最高；它们几乎都在密集登记表（58）里，
删一批就能同时让登记表缩短一行。每批必须同批跑 `--update`，否则棘轮会以 `*_UNDER` 提醒「基线过期」。

## 6. 已知边界（刻意不做的部分）

- **行尾注释算代码行**（`x := 1 // 说明`）。要堵它得改口径为「注释字符数」，属 B 阶段之后的事。
- **docs 面的「份 / 行」是代理指标**：内容层面（契约是否真的搬进了代码与测试）由 review 兜，棘轮只挡体积。
- 字符串/注释的解析只做了一层（字符串里的注释符号不算注释）；`<script>` 之外的 HTML 属性里的
  `<!--` 之类极端形态不特殊处理。

## 7. 验收

| 项 | 结果 |
| --- | --- |
| `make check-comment-density` | ✅ 与基线完全一致（自测 39 组全绿） |
| `make check-annotations` / `check-doc-links` / `check-doc-endpoints` | ✅ 全绿（旧规则一条没放宽，三个脚本零改动） |
| `make ci` | ✅ exit 0（契约静态断言 / 守卫 / gofmt / vet / build / 单测 / 基准冒烟全过） |
| `make e2e-affected` | ✅ 全量 29/29 达标，断言 2455 通过 / 0 失败，authority 清单全覆盖，临时目录零残留 |
