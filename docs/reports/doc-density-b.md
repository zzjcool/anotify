# #495 B 阶段：文档面删到 0（B6）

<!-- snapshot-doc: 本 PR 的定格数字（删了什么、基线前后、门禁输出），复算命令见 §5；不是「现在是多少」的承诺 -->

> 一句话：`docs/v4`、`docs/research`、`docs/perf` 与仓库根的 plan/requirements 类 md 全删到 0；两处前置
> （守卫的绝对下限、`@decision` 的落点）**先解再删**；每批同批下调基线；`make ci` 与全量 e2e 全绿。
> 判据与分批计划来自 [comment-and-doc-ratchet.md](comment-and-doc-ratchet.md)（A 阶段）；本文件只记 B6。

## 1. 两个前置（顺序不能反）

### 1.1 守卫下限：从「仓库存量快照」改成「扫描面覆盖 + 解析器存活」

`check-annotations` 原来钉着四个 2026-11-16 的实测快照。B 阶段每批都在删注释与文档，这四条会变成
「删到一半必须下调下限」的墙 —— 而降下限正是任务边界禁止的动作。

| 项 | 前 | 后 |
| --- | --- | --- |
| 扫描文件 | `SCAN_FILES_MIN = 520` | 删掉，改成**扫描面覆盖**：`git ls-files` 里落在扫描面的文件漏读一个即 `SCAN_MISSING_CODE` / `SCAN_MISSING_DOCS` 红（比 520 严：当前 597/597 代码 · 86/86 文档） |
| 文档数 | `SCAN_DOCS_MIN = 40` | 删掉，并入上一条 |
| 注释行 | `COMMENT_LINES_MIN = 24000` | `ANNOTATION_MIN.commentLines = 1`（只拦「解析结果为空」） |
| 引用 | `REFS_MIN = 550` | `ANNOTATION_MIN.refs = 1` |
| 锚点 | `ANCHOR_MIN = 4` | 不动（4 = 本守卫头部的自锚点） |

总量不再由这里管：`check-comment-density` 对每个目录、仓库总量、单文件密度都是**等值棘轮**（超与降都算
不一致），它才是「删了多少」的事实判据；两套比例型下限叠在一起只会让人用「改下限」解决「删内容」。
密度守卫同批删掉 `MIN_SCANNED.commentLines = 30000` 这条同类地板（`files` / `codeLines` 保留：只在代码真被
删时下降）。新增自测三组：小仓库（覆盖完整）绿 / 漏读被跟踪文件红 / 解析结果为空红。

### 1.2 `@decision` 落点：先搬锚点，再删文档

`DECISION_DOC_CANDIDATES` 原来只认两份 `docs/contracts` 文件 —— 契约文档哪天搬完，所有 `@decision` 会一起
`ANCHOR_UNRESOLVED`（外键指向被删的文件）。本批新增**代码侧索引** `internal/decisions/decisions.go`
（`V4-D1`～`V4-D6` → 执行者文件，`decisions_test.go` 逐个 `os.Stat` 核对存在），并把它加为第三落点。
三条锚点从「文档示例」搬进真实代码与测试：

```
cmd/anode/internal/runner/runner.go:4            // 硬不变量（capability.md §8.5 / @decision V4-D4 / SLOT-HANDOFF A3）：
internal/server/api_envelope.go:16               // 轻量资源信封（api-conventions §3.1–§3.3 / @decision V4-D1）。
internal/server/resource_namespace_test.go:625   // TestNamespaceResourceSpecSwitchRejected @decision V4-D1：…
```

`git grep @decision V4-` 仍有落点（上面三行 + 守卫头部与 `docs/lint-guards.md` 里的语法示例）；锚点数 29 → 32。

## 2. 删了什么（每批一次 `make ci`）

| 批 | 处理 | 提交 |
| --- | --- | --- |
| B1 | `docs/v4` 五份：四份清单快照（e2e/测试/spec inventory + 构建清单）删掉；`anode-cli.md` 是**被机检的操作手册**（`scripts/check_anode_skill.py` 拿真二进制对账），迁到 `docs/anode-cli.md` 并改完全部仓内引用 | `09a29c50` |
| B2 | `docs/research` 两份（可行性研究 = 计划）、`docs/perf` 两份（一次性测量快照）；`capacity-and-backpressure.md` 是运维口径，按判据「要留就并进 reports/」搬进 `docs/reports/` | `913ef576` |
| B3 | 仓库根 `e2e-perf-requirements.md`、`passkey-errmsg-plan.md`、`passkey-errmsg-requirements.md`（计划进 issue/PR） | `730fd039` |

留下：`docs/reports`（31 份）、`docs/history`（17 份）、`docs/*.md` 手册（6 份）、`docs/contracts`（16 份）。

## 3. 两个守卫的预算：前 → 后

| 守卫 | 项 | 前 | 后 | 同批下调 |
| --- | --- | --- | --- | --- |
| 密度（L12） | `docs/v4` | 5 份 / 960 行 | 逐出基线 + **禁止重建** | B1 |
| | `docs/research` | 2 份 / 422 行 | 逐出基线 + 禁止重建 | B2 |
| | `docs/perf` | 3 份 / 706 行 | 逐出基线 + 禁止重建 | B2 |
| | 仓库根 plan 类 md | 3 份 / 193 行 | 0 份 / 0 行 | B3 |
| | 注释行总量 | 43373 | 43372（守卫脚本自身净 -1；md 不计注释行） | 前置 |
| | `docs/` 合计 | 77 份 / 14537 行 | 69 份 / 12942 行（删完时；加上本报告为 70 份 / 13009 行） | B2 |
| 注解（L5） | `COUNT_UNMARKED_BASELINE` | 312 | 248 → 210 | B1 / B2 |
| | `CLAIM_UNBACKED_BASELINE` | 299 | 298（删掉的下限注释里有一行带 `guard`，不是判据放宽） | 前置 |
| | 绝对下限 | 520 / 40 / 24000 / 550 | 扫描面覆盖 + 存活下限 1 | 前置 |

根目录 md 不在规则 D 的扫描面（只认 Makefile / `scripts` / `web-src` / `docs` / 三份手册），所以 B3 不动 L5 的预算。

## 4. 证据

- `node scripts/check-comment-density.mjs --self-test`：全绿（新增「`docs/v4` 重建 → `DOC_DIR_FORBIDDEN`」）。
- `node scripts/check-annotations.mjs --self-test`：全绿（新增「契约文档不在时 `@decision` 由代码侧索引提供」
  「不存在的号仍红」；覆盖判据的红由 ㉑'' 钉住：漏读被跟踪文件 → `SCAN_MISSING_*`）。
- 收紧方向没被放宽：`--update` 抬高直接拒；删文档后 `DOC_DIR_UNDER` / `DOC_ROOT_UNDER` 强制同批下调（三次都在同一提交里降）。

## 5. 门禁（真实退出码，不走管道）

| 项 | 结果 |
| --- | --- |
| `make ci`（前置 / B1 / B2 / B3） | 各一次，`EXIT=0` |
| `make e2e-affected`（`PI_E2E_FULL=1`，29 套件） | `EXIT=0`：断言 **2468 通过 / 0 失败**、29/29 达标、authority 覆盖 29、临时目录零残留 |
| `scripts/e2e/ci-e2e-verdict.mjs` | `✅ 清单内 0 条（已容忍），清单外 0 条` |
| `check-doc-links` / `check-doc-endpoints` / `check-agents` / `check-anode-skill` | 全绿（手册搬家后仍拿真二进制对账） |

## 6. 已知边界

- `docs/contracts/**`（16 份）本 PR 不删：B7 的阻塞项（决策号落点）已就位，但它还承担 wire/不变量契约，搬进代码与测试得单独一批。
- `docs/history/**`、`docs/reports/**` 里对被删文件的引用是**定格记录**，不回写（口径见 `docs/README.md` §3）。
- 这一批删的是文档，不是代码注释：注释行总量几乎不动（B1～B5 的桶才是它）。
