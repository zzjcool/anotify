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
| 370 | 容器层次 | c-vis | [#387](https://github.com/zzjcool/anotify/pull/387) | rev-387 进行中 | — |
| 371 | 对比度 | c-vis | #387（同上） | rev-387 进行中 | — |
| 372 | 设计系统收编 | c-vis | — | — | — |
| 373 | 强调密度 | c-vis | — | — | — |
| 374 | 内部字段上屏 | c-fecontent | [#386](https://github.com/zzjcool/anotify/pull/386) | rev-386 进行中 | — |
| 375 | 裸 JSON | c-fecontent | [#388](https://github.com/zzjcool/anotify/pull/388)（stacked #386） | rev-388 进行中 | — |
| 376 | 动作收敛 | c-feactions | 进行中（c-feactions） | — | — |
| 377 | 列表分页 | c-paging | — | — | — |
| 378 | 错误态分级 | c-fecontent | — | — | — |
| 379 | 文案与未接能力 | c-copy | — | — | — |
| 380 | 预案徽章失败态 | c-feactions | 分支 ui/369-playbook-badge @ f9f0757（PR 待建） | — | — |
| 381 | 通知接收 + 首跑 | c-ia | — | — | — |
| 382 | 触控目标 | c-mobile | 进行中（c-mobile，stacked 在 #383 之上） | — | — |
| 383 | iOS 聚焦缩放 | c-mobile | 进行中（c-mobile） | — | — |
| 384 | 主内容横滚 | c-mobile | — | — | — |
| 385 | 手机首屏/弹层/安全区 | c-mobile | — | — | — |
