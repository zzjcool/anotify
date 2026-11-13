# Epic #369 · UI/UX 评审总账 · 编排计划

> 状态文件（协调者维护）：波次 / 车道 / PR / 审查 / 合并。
> 契约（冻结）：[`CONTRACT.md`](CONTRACT.md)；评审事实源：[`../UI-REVIEW-2026-11-13.md`](../UI-REVIEW-2026-11-13.md)。
> 集成分支：`epic/369-ui-review`（所有 PR 的 base），全部通过后一次性 `→ main`。

## 1. 分解原则

- **按文件所有权分车道，不按 issue 主题分**：16 个 issue 有 60% 的改动落在同一批文件（`ui.css` / 19 个页面 / `partials.js`），
  并行车道的唯一安全维度是"谁能写哪些路径"。
- 每个车道串行交付多个 PR；**同一文件的写入者同一时刻只有一个**。
- 全局收口类 issue（#373 强调密度、#378 错误态、#379 文案、#381 IA）放在**页面结构稳定之后**，
  避免"改两遍"。
- 每个 PR 独立成 review 单元，独立可回滚（#372 明确要求分批判）。

## 2. 波次与车道

| 波 | 车道 | issue | 交付物 | 状态 |
|---|---|---|---|---|
| W1 | `c-vis` 视觉基座 | #370 #371 | PR：容器层次 + 对比度（tokens.css / ui.css） | 待派发 |
| W1 | `c-fecontent` 产品表现 | #374 #375 | PR：内部字段折叠 + JSON 结构化（6 个页面） | 待派发 |
| W1 | `c-feactions` 动作收敛 | #380 #376 | PR：bug + 动作收敛（6 个页面） | 待派发 |
| W2 | `c-vis` | #372 | PR：设计系统收编（分 3 批 = 3 个 PR） | 待 W1 |
| W2 | `c-mobile` | #383 #382 | PR：iOS 缩放 + 触控热区（ui.css 移动断点 + 度量脚本） | 待 W1 |
| W2 | `c-fecontent` | #378 | PR：错误态分级（partials.js + 全站 confirm 收敛） | 待 W1 |
| W3 | `c-mobile` | #384 | PR：主内容不再必须横滚（keys/admin/docs/图/热力图 + e2e） | 待 W2 |
| W3 | `c-ia` | #381 | PR：通知接收去留 + 首跑闭环 | 待 W2 |
| W3 | `c-paging` | #377 | PR：8 个列表接口 limit/continue + 4 页搜索/加载更多 | 待 W2 |
| W4 | `c-mobile` | #385 | PR：手机首屏优先级 + 弹层 + 安全区 | 待 W3 |
| W4 | `c-copy` | #379 | PR：旁白/双语徽章/cron 预设/能力模板 | 待 W3 |
| W5 | `c-vis` | #373 | PR：全站 btn-primary 收敛到每页 ≤1 | 待 W4 |
| W6 | 协调者 | — | 全量 e2e + web_verify 19 页 × 桌面/移动 → `epic/369-ui-review → main` | — |

## 3. 每车道交付纪律

1. 在自己的 worktree 里做；**只写契约里属于本车道的路径**。
2. 机械门禁：`make ci`（Go 车道）/ `make fe`（前端车道）+ `node scripts/check-classes.mjs` + i18n 四语言键一致。
3. 视觉证据：`make mock-local MOCK_PORT=<专属端口>` → 无头截图（改前/改后）落 `.pi-orchestrator/out/issue-<N>/`（gitignored，不入库）。
4. 一个 PR 一个主题；PR body 逐条勾 issue 验收项并附证据（grep 计数 / 对比度矩阵 / 截图路径）。
5. 不合并、不 squash、不动 `main`；合并由协调者在 review 通过后执行。
6. 发现产品 bug 不改断言迁就，写进 REPORT。

## 4. 审查与合并

- 每个 PR 由**独立 worker（fresh context，review-only）**审查：读 diff、对照 issue 验收、独立跑机械门禁 + 视觉抽查，
  给出 `gh pr review --approve` 或 `--request-changes`（逐条编号）。
- 不通过 → `continue` 原车道返工（同一 worktree，保留上下文）。
- 通过 → 协调者本地 `make ci` + 目标 e2e 套件 + web_verify 抽查 → `gh pr merge --merge` 到 `epic/369-ui-review`。
- 每波结束跑一次**全量** `make e2e`（唯一 push 门禁），红则当波内修完再进下一波。

## 5. 进度台账

### 基线事实（2026-11-13，`epic/369-ui-review` @ 5882659）

- `./scripts/e2e/run_all.sh`（全量）→ **绿**：28/28 套件、2136 断言通过 / 0 失败、287s。→ 每波结束的推门禁就是它。
- `make ci` → **红**（5 + 1 pre-existing 失败）：`internal/sitegen` docs test（确定性）、`cmd/anode` 5 个 bootstrap（本机无 sudo）、`internal/ws`+`runner`（负载 flaky）。→ 车道门禁不用 `make ci`，用"改哪个包跑哪个包 + make fe"。
- 已冻结决策（#381）：**选 B —— 推送未开放期间下线「通知接收」页**（删 `receivers.html` + 清 `partials.js`/`login.html` 白名单，docs 注明推送未开放；e2e 无套件引用该页，风险低）。理由：`PUSH_UI_ENABLED=false`，给一个"暂未开放"页加导航入口是更差的 UX。

| # | issue | 车道 | PR | 审查 | 合并 |
|---|---|---|---|---|---|
| 370 | 容器层次 | c-vis | [#387](https://github.com/zzjcool/anotify/pull/387) | rev-387 CHANGES → 返工 → rev-387b **APPROVE** | ✅ `fb4547c` |
| 371 | 对比度 | c-vis | #387（同上） | rev-387b APPROVE（门禁承重 6/6；药丸实拍已补） | ✅ `fb4547c` |
| 372 | 设计系统收编 | c-design | [#395](https://github.com/zzjcool/anotify/pull/395)（组件）+ [#396](https://github.com/zzjcool/anotify/pull/396)（刻度，stacked） | rev-395 / rev-396 进行中 | — |
| 373 | 强调密度 | c-vis | — | — | — |
| 374 | 内部字段上屏 | c-fecontent | [#386](https://github.com/zzjcool/anotify/pull/386) | rev-386 CHANGES → 返工 → rev-386b **APPROVE**（message 5→0、合计 45→6） | ✅ `a07e69d` |
| 375 | 裸 JSON | c-fecontent | [#388](https://github.com/zzjcool/anotify/pull/388) | rev-388 CHANGES → 返工 → rev-388b CHANGES(1行) → 修 → rev-388c **APPROVE** | ✅ `1747ce9` |
| 376 | 动作收敛 | c-feactions | [#390](https://github.com/zzjcool/anotify/pull/390) | rev-390 CHANGES → c-actfix 返工 → rev-390b **APPROVE**（32/32 组 0 越界） | ✅ `5f0a75f` |
| 377 | 列表分页 | c-paging | [#393](https://github.com/zzjcool/anotify/pull/393)（api）+ [#394](https://github.com/zzjcool/anotify/pull/394)（fe） | rev-393 CHANGES（users 破平局随机红）→ rowid 修 → rev-393b 只差 1 行文档 → 修；rev-394 **APPROVE** | ✅ `a74f516` + 前端 merge |
| 378 | 错误态分级 | c-err | [#398](https://github.com/zzjcool/anotify/pull/398) | rev-398 **APPROVE**（裸码 12/12 归零、真撤销 GET 404→200） | ✅ `ab6d269` |
| 379 | 文案与未接能力 | c-copy | — | — | — |
| 380 | 预案徽章失败态 | c-feactions | [#389](https://github.com/zzjcool/anotify/pull/389) | rev-389 **APPROVE**（新 e2e 回滚后确定性变红） | ✅ `535fa22` |
| 381 | 通知接收 + 首跑 | c-ia | [#397](https://github.com/zzjcool/anotify/pull/397) | rev-397 CHANGES（emit 命令跑不通/缓存不过期）→ 修 → rev-397b **APPROVE** | ✅ `2a4697c` |
| 382 | 触控目标 | c-mobile | [#392](https://github.com/zzjcool/anotify/pull/392) | rev-392 CHANGES → 返工 → rev-392b **APPROVE** | ✅ `4e96c45` |
| 383 | iOS 聚焦缩放 | c-mobile | [#391](https://github.com/zzjcool/anotify/pull/391) | rev-391 CHANGES → 返工 → rev-391b **APPROVE**（5 项承重测试） | ✅ `52e7c10` |
| 384 | 主内容横滚 | c-mobile | — | — | — |
| 385 | 手机首屏/弹层/安全区 | c-mobile | — | — | — |

## 波末门禁（2026-11-14 03:30 实测，epic tip `e2e-driver-fix`）

| 门禁 | 结果 |
|---|---|
| `make fe` | PASS（class 841 / tailwind 923 / sitegen 4×19 / 指纹 30+76） |
| `./scripts/e2e/run_all.sh`（全量 28 套件） | **2274 断言全绿 / 0 失败**（基线 2136；新增 ~138 条来自 #377/#378/#381/#382/#383/#384/#385） |
| 已合入子 issue | #370 #371 #372 #374 #375 #376 #377 #378 #380 #381 #382 #383 #384 #385（14/16） |
| 在审 | 无 |
| 关键坑（本轮踩到） | **`run_all.sh` 不重建前端**：Go 二进制 embed 的是 `internal/server/dist`，`make fe` 没跑 → 二进制发的是旧页面，全量 e2e 会大面积假红（实测 7 套件红 → 跑完 make fe 后 1 条真红）。波末门禁必须 `make fe && ./scripts/e2e/run_all.sh` |

## 收尾（2026-11-14 04:00）

16 个子 issue 全部合入 `epic/369-ui-review`：**#370 #371 #372 #373 #374 #375 #376 #377 #378 #379 #380 #381 #382 #383 #384 #385**（PR #386–#402，其中 #389/#387/#386/#388/#390/#391/#393/#394/#392/#395/#396/#397/#398/#399/#400/#401/#402）。

终局门禁：

| 项 | 结果 |
|---|---|
| `make fe` | PASS（class 842 / sitegen 4×19 / 指纹 30 资源 76 HTML） |
| `./scripts/e2e/run_all.sh` 全量 | **28/28 套件，2274 断言通过 / 0 失败** |
| 19 页 × 1440/390 无头巡检 | **38 次加载，0 console error / 0 pageerror / 0 横向溢出** |
| `node scripts/check-contrast.mjs` | 48/48（含 #370 新增 D370-4/5/6） |
| `node --test scripts/v4-frontend.test.mjs` | 198 pass / 16 fail，失败集合与 base 逐条相同（基线红，非本 epic 引入） |

## 终局审查返工（rev-403 CHANGES-REQUESTED → 修完）

| 阻塞 | 修法 | 复核 |
|---|---|---|
| 分页缺省 50 在第 51 项静默截断（`nsList` / `admin users` / `security sessions·passkeys`） | 调用点显式 `?limit=500`（恢复 #377 之前「一次取全」的行为；分页仍留给列表页） | 全量 e2e 2274/0 |
| 单测相对 `main` 新增 5 红（`source.html` 的 `window.addEventListener` 在 vm 沙箱缺失、`CASES.length` 35 vs 36） | 沙箱补 `addEventListener/removeEventListener/dispatchEvent`；pin 35→36 | `node --test v4-frontend` **205 pass / 9 fail，相对 main 新增红 0 条**（main 自身 31 红） |
| e2e 精确选择器（`#task-logs button` 因 #378 加「复制」变 2 个；`scope-bootstrap` 与 `R1` 的 URL 断言） | 精确到 `#logs-form button[type=submit]`、按路径比对（忽略查询串） | 全量 e2e 2274/0 |

最终门禁（epic tip `d910474`）：`make fe` PASS ｜ 全量 e2e **28/28 套件、2274 断言 / 0 失败** ｜ 19 页 × 1440/390 巡检 **38 次加载 0 问题** ｜ contrast 48/48 ｜ v4-frontend 相对 main 新增红 **0**。
