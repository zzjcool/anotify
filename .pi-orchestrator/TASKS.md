# Anotify 编排任务板（项目级）

> 协调者（主 pi Agent）在此跟踪 **anotify 项目级**编排任务的进展。
> 通用编排流程（三层架构/7 个专路 agent/协调者纪律）在 workspace `AGENTS.md` §3；专路 Agent 定义在 workspace `.pi/agents/`（pm/designer/scout/worker/frontend/tester/reviewer，去 anotify 前缀）。自我升级机制在 workspace `.pi-orchestrator/EVOLUTION.md`。本文件只跟踪 anotify 自己的任务。

## 编排团队

| Agent | 层 | 模型 | 职责 |
| --- | --- | --- | --- |
| pm | 定义 | kimi-k3 | 需求/价值/边界/验收标准 |
| designer | 定义 | kimi-k3 | 信息架构/视觉方案/交互规格 |
| scout | 侦察 | deepseek-v4-flash | 摸代码现状 → context.md |
| (内置 planner) | 规划 | kimi-k3 | 拆任务 → plan.md |
| worker | 实现 | glm-5.2 | 后端实施 |
| frontend | 实现 | glm-5.2 | 前端照稿实现 |
| tester | 实现 | glm-5.2 | 测试把关/门禁 |
| reviewer | 终审 | kimi-k3 | 对照需求&设计终审 |

> 模型映射在 workspace `.pi/settings.json` 的 agentOverrides。anotify 专属上下文（技术栈/架构地图/门禁/踩坑）在 `AGENTS.md` + `DEVELOPMENT.md`，agent 通过 inheritProjectContext 继承。

## 标准流程

pm 定义 → designer 设计 → scout 侦察 + planner 拆 →
worker(后端) ∥ frontend(前端) 实施 → tester 把关 → reviewer 终审 → 协调者合并并标 ✅

---

## 当前任务

### index-event-stream：首页淘汰「最近任务」，事件流升为主列表（fix/index-event-stream，2026-11-03）

**用户诉求**（原话）：「首页这里进来看到的为什么还是以前的消息，这里都要被淘汰掉，我们现在是事件流」。

**根因**：首页仍用 `/v1/tasks` 渲染「最近任务」列表（来源是任务表，包含历史行），事件流只占右栏 5 行小卡；产品对象已经是事件。

**已做**：

- [x] `web-src/pages/index.html`：删除「最近任务」卡片 / 筛选 tab / 客户端分页 / 任务详情弹层（`renderNotifs`、`normalize`、`openDetail`…）；事件流升为**全宽主列表**（8 行：源 → 类型 / traceId / 相对时间，点整行 → `trace.html`，footer 有「最近 N 条 + 刷新」）
- [x] KPI / 摘要 / 热力图改事件口径（`/v1/stats` 的 total/today/daily 本来就是 events，只是文案写成了「任务」）；「进行中 / 异常任务」两卡只保留计数（`/v1/tasks?limit=50`），取不到显示 – 而非谎报 0
- [x] 旧推送深链 `index.html?msg=<taskId>` → `message.html?id=<taskId>`（历史通知不会点成空页）
- [x] i18n 四语言：删 `index.recent.* / index.notif.* / index.detail.*`，新增 `index.summary / index.events.refresh / index.events.count`；下拉刷新键迁到 `ui.index.pull_*`（`web/partials.js` 同步）
- [x] `web/mock.js`：`/v1/stats` 的 total/today/daily 口径改为事件（对齐真后端，避免「事件 KPI 配任务数字」的静默漂移）
- [x] e2e：frontend / real_backend 的等待锚点与错误态改用 `#index-events`；deeplink 新增「首页 `?msg` 重定向到 message.html」用例
- [x] 门禁：`go test ./...` 通过；`E2E_CONCURRENCY=1 make e2e` → **1836 通过 / 0 失败**；web_verify 桌面 1440 + 移动 390 均 0 问题
- [x] 2026-11-03 与 origin/main 的「动效/加载 wave」合并（`07d5cca`）：保留事件流主列表，吸收 KPI `setStatValue`/spark `is-ready`/热力图骨架/`anim-stagger`；合并后重跑全量 e2e → 1836 通过 / 0 失败，dev 已重建（数据卷保留）

**边界**：`message.html`（任务详情页）保留 —— 它是 trace/执行结果详情，不是首页列表；`.notif-row` 组件样式暂留 `ui.css`（其它页可复用）。

### nav-collapse-integration：集成三 tab 收敛为单一「API 文档」（jic，2026-11-02）

**用户诉求**：侧栏「集成」下的三个 tab（接入 Agent / API 文档 / 技术方案）都去掉，只保留 API 文档（并考虑把 OpenAPI 规范嵌进 API 页）；「接入 Agent」页与「技术方案」入口删除。

**已做**：

- [x] NAV：删「集成」分组；`API 文档` 并入主导航（在 API Keys 之后），`docs.html` active 高亮不变
- [x] `web-src/pages/connect.html` 删除（含 4 语言 `connect.*` 命名空间、`common.nav.{integration,agent,scheme}`）
- [x] 首页两处 CTA `connect.html` → `docs.html#agent`；`keys.html` 的 `docs.html#notify`（死锚点）→ `docs.html#api`
- [x] e2e：frontend 套件删 connect 页用例，新增「集成三 tab 已下线」回归（无分组标签 / 无 connect 入口 / docs active / CTA 不指向已删页）；i18n 与 i18n_coverage 页面清单同步
- [x] 门禁：i18n 299 + frontend 383 + i18n_coverage 61 全绿；`go test ./...` 通过

### api-docs-openapi-embed：API 页嵌入 OpenAPI 端点清单（jic，2026-11-02）

**用户诉求**：把 `api/openapi.yaml` 嵌入 API 页（原话「api 那个可以用 openapi」）。

- [x] `internal/sitegen/openapi.go`：构建期解析 spec → 按 tag 分组（spec tags 声明顺序，未声明 tag 追后、组内按路径+方法排序），deprecated operation 不展示，`/../x` 归一成 `/x`
- [x] `PageData.OpenAPI` + `cmd/sitegen -openapi`（默认 `api/openapi.yaml`，CWD 无关：Makefile 在仓库根跑）
- [x] `docs.html` 新增 `#endpoints`「全部端点」区（93 个端点 / 17 组，静态 HTML，无运行时 JS）+ TOC 入口
- [x] i18n：`docs.endpoints.*` / `docs.toc.endpoints` / `docs.openapi.tag.*`（16 个 tag 标签 × 4 语言）
- [x] 防漂移：`openapi_test.go` 直接解**仓库真实 spec**（≥80 op、含 POST /v1/events / GET /agent-login.sh、deprecated 不泄漏）
- [x] e2e：frontend E-4 量行数（≥80 行 / ≥10 组）+ 真实端点存在 + 弃用端点不展示
- [x] Dockerfile：sitegen 阶段补 `COPY api/`（漏了会直接构建失败——故意的）
- [x] 门禁：frontend 386 + i18n 299 + i18n_coverage 61 = 746 全绿；`make contract` 97 operation 一致；`go test ./...` 通过；web_verify 桌面/移动无溢出
- [ ] 未做：端点表没带 auth 列（现在只有方法 + 路径），需要的话再加 security 解析

---

### connect-agent-page：接入 Agent 独立页（feat/connect-agent-page，2026-08-10 完成）

**用户诉求**：现在侧边栏「接入 Agent」点击后是跳到首页 `#quickstart` 锚点卡片，体验割裂。要为「接入新 Agent」单开一个独立页面，侧边栏导航直达该页；整套交互逻辑重新设计，完整交付。

**编排流程**：pm（需求）→ designer（设计）→ worker/frontend（实施）→ tester（门禁）→ reviewer（终审）。

- [x] 定义层：designer design.md（pm 因模型截断未落盘，协调者亲自拍板 3 个待决策点写进 plan.md）
- [x] 实现层：worker 改 NAV+抽 code-block 样式+4 语言 i18n+首页精简；frontend 造 connect.html（6 区块工作流页）
- [x] 测试层：tester E2E 覆盖新页 + 导航高亮 + 演示态
- [x] 终审：reviewer 首轮 REQUEST_CHANGES（发现演示态 MAJOR bug）→ frontend 修复 → tester 补后端宕机 e2e → reviewer 复审 APPROVE
- [x] 门禁：make e2e 17/17 全绿，1020 断言通过（53.3s）；协调者独立 web_verify 实测真实数据态 7 card + active 高亮 + 无溢出 + 0 JS 错误

**产出**：新建 connect.html（6 区块三步工作流页）、NAV 直达、code-block 样式抽 ui.css 共享、首页 quickstart 降级为 CTA、4 语言 connect.* 命名空间、3 套 e2e 加覆盖

**关键经验**：reviewer 用 Playwright 实证抓出演示态 bug（tester 全在真实后端跑，有后端宕机盲区）——证明 reviewer 独立于 tester 的价值；修复后 tester 补 route.abort() 后端宕机用例堵住盲区

---

## ✅ e2e 测试体系性能与架构重构（refactor/e2e-perf，2026-08-09 合并入 main，commit ab17bcd）

> 分支 `refactor/e2e-perf`（off main 269981a）。worktree 已清理。编排文档：需求 `e2e-perf-requirements.md`、设计 `design.md`（在 subagent artifacts）、侦察 `scout-context.md`。

**问题**：全量 `make e2e` 实测 491 秒（8 分钟），远超 subagent bash 120s 超时，tester subagent 必然超时。用户要求「长期彻底方案，打好基础」。

**根因**（scout 全面侦察）：① 9 套件 55 处 `waitForTimeout` 固定 sleep（62s）；② i18n 28 页遍历 3 遍 + 每页 newContext；③ 无并行（纯串行 for + sleep 1.5）；④ 构建缺口（run_all.sh 不生成 dist，干净仓库编译失败）；⑤ go test 50s 被算进 e2e；⑥ 无结构化结果；⑦ 端口随机分配并行撞车。

**修复**（pm 定义 + designer 设计 + worker/frontend 实现 + tester/reviewer 终审 + 协调者独立验证修复 3 个 worker 遗漏的 bug）：

- **harness 增强**：`waitForAppReady(page, pageType)` 按页面类型给锚点（workspace→#sidebar、login→#lang-switcher-login、data-page→额外 dataAnchor）；结构化 JSON 结果（`.e2e-bin/results/<suite>.json`）；`startTimer/stopTimer`。
- **套件改造**：17 套件全部 sleep→事件等待；i18n 28页遍历3遍→1遍、28ctx→4ctx；i18n_coverage ~29ctx→~9。
- **并行执行器** `parallel_runner.mjs`：HTTP 类全并行 + Chrome 类信号量控制（默认 4，`E2E_CONCURRENCY` 可配）；per-suite 120s 超时（Node 原生，替代 macOS 缺失的 `timeout` 命令）；JSON 结果聚合。
- **run_all.sh 重构**：前置检测 dist/web 不完整则 `git checkout HEAD -- web` 恢复追踪源文件 + `make fe`；go test 拆出独立 `make test`。
- **端口分配**：确定性端口段（每套件 50 端口，SUITE_PORTS 映射）；startServer 直接 spawn + health 失败换端口重试（**不用 findFreePort 预探测**——其 listen-then-close 留下 TIME_WAIT 致 anotify bind 失败，这是 ws_protocol 稳定 crash 的真因）。
- **parallel_runner 误标失败修复**：JSON `failed` 数为权威（非 exit code），断言全过但清理阶段抛错→pass-warning 不算失败。

**协调者独立验证修复的 3 个 worker 遗漏 bug**：

1. passkey_enroll 92.9s 真因是 pageType 用错（passkey-enroll.html 是公开页应用 "login" 锚点非 "workspace"，8 次 10s 超时=80s 浪费）→ 改 "login" 后 14.5s。
2. 7 个 HTTP 套件（api_contract/edge_cases/passkeys/persistence/security/ws_protocol/routing）没传 `suiteName` 仍用随机端口→补上。
3. `findFreePort` 的 listen-then-close 留 TIME_WAIT 致 ws_protocol 稳定 crash→改 startServer 直接 spawn+重试。

**门禁**：全量 `make e2e` **57s**（从 491s 降 88%），968 断言全绿；单套件全部 ≤40s（最慢 lang_hint 40s）；`waitForTimeout` 零残留；干净仓库（删 dist+web/）可跑；**5 次并行连跑零 flaky**；macOS 无 timeout 依赖。

**套件耗时对比**：i18n 174s→22s、i18n_coverage 89s→16s、passkey_enroll 93s→15s、frontend 62s→23s、lang_hint 55s→40s、cli_auth 40s→25s。

**遗留**：

- 无 CI（本次先打牢本地基础，结果格式已为 CI 友好预留 JSON）。后续可加 GitHub Actions。
- Go 单测里 13×`time.Sleep(2200ms)`=28.6s 固定 sleep 未优化（已从 e2e 摘出到 `make test`，单测 sleep 优化另立任务）。
- `findFreePort` 函数保留导出（向后兼容）但 startServer 不再调用它。

---

## ✅ Passkey 登录失败文案修复（fix/passkey-login-error-message，2026-08-08 合并入 main，commit 00e3ff9）

> 分支 `fix/passkey-login-error-message`（off main cb7ae34）。worktree `.pi-orchestrator/worktrees/wt-passkey-errmsg`。
> 编排文档：需求 `passkey-errmsg-requirements.md`、计划 `passkey-errmsg-plan.md`（均在仓库根，随 commit 入库）。

**问题**：可发现登录（discoverable login）按 userHandle（内部 user ID）查用户。用户被删/重建后 username 相同但 user ID 变了，认证器里残留的旧 Passkey 带回旧 userHandle → `GetUserByID` 查不到 → 误报「用户不存在」。文案掩盖了真正失败原因（凭证孤儿），误导用户以为是「没注册过」。

**修复**（worker 实现，reviewer APPROVE）：

- `internal/auth/webauthn.go`：三处登录失败用 `errors.Is(err, store.ErrNotFound)` 区分「用户名从未注册」（保留「用户不存在」真实反馈）vs「其他 DB 错误」（→「登录失败，请稍后重试」）。
- 抽 `lookupUserByHandle` 方法：userHandle 失配时打诊断日志 `event=auth.login.stale_user_handle`（仅前 8 字符 hex，**不泄露**完整 handle / username）+ 返回中性文案「该 Passkey 未关联到任何账户，可能已失效，请尝试其他 Passkey 或重新注册」。
- 新增 `userHandlePrefix` helper（hex 前 8 字符截断）。
- `internal/auth/auth_test.go`：补 `TestBeginLogin_UnknownUser` 文案断言 + 新增 `TestLookupUserByHandle_StaleUserHandle`（双向断言：含中性文案 + 不含「用户不存在」）/ `ValidUser` + `TestUserHandlePrefix`（4 子用例）。

**降级决策**（plan 已说明，需求 §5 预留）：

- 不引入错误码体系——前端纯透传 `data.error` 不判类型（scout 确认），最小改动。
- 不做 begin 阶段防枚举（假 challenge）——scout 证实前端无指定用户名登录 UI，`BeginLogin` 仅 e2e/API 可达，收益低复杂度高，留后续。

**门禁**：`go test ./internal/auth/...` 全绿；e2e 关键三套件 `auth_flow` 28/0、`i18n_coverage` 61/0、`cli_auth` 132/0 全绿；前端零改动，`web_verify` 四语言登录页 8/8 通过。reviewer 独立复核 build/gofmt/vet clean。

**遗留**：

- 防枚举降级（见上）。
- 「认证器里残留过期 Passkey」的清理仍需用户手动在系统删；本次仅在登录失败时给更有用的提示。后续可单独立项做「检测/提示孤儿凭证」功能。
- tester 跑 e2e 时发现 worktree 缺 `vapid.json`（该文件在 `.gitignore` 不随 worktree 带过来），致 api_contract 初跑失败——环境问题非回归，复制 vapid.json 后 48/0 通过。**此为 worktree 编排通病**，后续建 worktree 跑 e2e 需手动拷 vapid.json，或让 run_all.sh 检测并提示。

---

## ✅ i18n 覆盖补全（feat/i18n-coverage，2026-08-07 合并入 main，commit fb61add）

> 分支 `feat/i18n-coverage`（off main 870bcfa）。非中文语言（en/ja/es）下用户可见文案 **0 条中文残留**（演示数据亦翻译）。
>
> 三批顺序执行（规格见 `.pi-orchestrator/archive/i18n-coverage-spec.md`）：
>
> - 批1：partials.js + index.html + message.html 运行时文案 i18n（commit 8f9def9）
> - 批2：keys/receivers/security/login 运行时文案 i18n（commit 6e2734d）
> - 批3：docs.html 整页 `{{t}}` 化 + 4 语言全译，约 120 个 docs.* key（commit e689f31）
> - 收尾：补 login.html 遗漏的 unsupported 文案 t() 化 + 新增 `i18n_coverage` 回归守卫套件（61 断言，防中文残留回归）（commit 28d00d1）
>
> **合并**：feat/i18n-coverage 与 main（含 CLI 设备授权任务）在 870bcfa 后分叉，合并时 4 个 locale yaml + run_all.sh 有冲突（main 加了 `cliauth` 文案 + cli_auth 套件，feat 加了 docs 整页翻译 + i18n_coverage 套件），逐文件合并：保留 main 的 cliauth 区块（移到顶层 key，不嵌进 docs）、feat 的 docs 正文翻译、两边的 e2e 套件都注册。
>
> **门禁**：`make e2e` 14/14 套件全绿（699 断言），含 i18n_coverage 61 通过/0 失败、cli_auth 132 通过/0 失败。sitegen 4 语言×8 页面无 WARN（key 对齐校验通过）。
>
> **遗留**：合并暴露并修复了 1 个 feat 分支批3 的潜在结构问题——cliauth 文案原本会被错误嵌进 docs 下（YAML 缩进），已修正为顶层 key。

---

## ✅ CLI 设备授权登录（feat/cli-device-login，2026-08-06 完成）

> worktree：`.pi-orchestrator/worktrees/wt-cli-auth`，分支 `feat/cli-device-login`（off main 870bcfa），**已合并入 main**（2026-08-07，随 i18n 覆盖补全 merge 一起落地）。

**目标**：替代「手动建 Key + 复制粘贴」，让 Agent 跑一条脚本完成授权取 Key，Key 全程不进对话上下文/剪贴板。

**交付**：

- 服务端：授权会话生命周期 9 端点（建会话/ASCII 二维码/短码 lookup/批准/拒绝/轮询领证/keys-self/脚本分发）、内存限速器、`cli_auth_sessions` 表、go-qrcode 依赖
- 确认页 `cli-auth.html`：双模式（`?s=` 直达 + 无参输码）、五终态+网络错误态、scope 勾选（默认 send）、四语言、focus 布局
- 登录脚本 `web/agent-login.sh`（POSIX sh+curl 零依赖）：三入口同屏输出、0600 原子落盘、stdout 永不打印 Key
- 安全不变量：sessionId/userCode 只有批准权，secret（只在脚本内存）才有领证权；明文 Key 不落库、一次性领取

**编排记录**：pm ∥ designer → worker(T1 store/auth) ∥ frontend(T2 页) → worker(T3 server) → worker(T4 脚本) → tester(T5 e2e) → reviewer(T6 终审 APPROVE) → worker(T7 返工 3 条🟡) → 协调者验收+实机演示

**门禁**：`go test ./...` 全绿；`make e2e` 14/14 套件绿（含 cli_auth 132 断言 + i18n_coverage 61 断言）；web_verify 四语言×双视口无 JS 错误/无溢出；协调者实机演示全链路（起服务→跑脚本→短码批准→领证→Key 发通知成功→幂等重跑）。

**回顾**（对照 EVOLUTION.md 三问）：

1. **哪里顺/哪里卡**：顺——定义层并行产出质量高（designer 逐区块规格可直接照做）；worker/tester 一次过；reviewer 终审 3 条🟡全是真问题（409 假状态/XFF 伪造/短码 TOCTOU）。卡——协调者在定义层完成后又停下等确认，被用户两次催（「为什么又停下来」「继续啊」）；T7 首次派发遇模型端点 unexpected EOF，重派后成功。

2. **根因**：协调者把「阶段完成」误当回合终点（AGENTS.md §0.1 已明令禁止，老毛病复发）；EOF 属外部网络抖动。

3. **怎么改**：L2——AGENTS.md §0.1 已覆盖，无需再改；L1——协调者自查牢记「阶段门=继续信号，不是停止信号」；EOF 类瞬时故障=重派即可。

**遗留风险**（tester/reviewer 已确认可接受）：AC-04 IP 限速无显式 e2e（代码评审覆盖）；AC-27 有浏览器自动打开无 GUI 可测（逻辑已评审）；TTL 10 分钟靠 SetClock 单测覆盖非真实等待；XFF 信任默认关闭（反代部署需显式 `ANOTIFY_TRUST_PROXY=1`）。

---

---

## 历史归档：站点多语言 i18n（已完成 2026-08-05，merge a507cbf）

4 语言（zh-CN/en/ja/es）+ 全站下拉切换器 + hreflang + README 双语 + i18n E2E 套件。
全流程：pm(19 AC) → designer(v1.1 统一下拉) → worker(sitegen Page/Langs+缺key回退) →
frontend(切换器+ja/es+README) → tester(i18n 套件 298 断言) → reviewer(APPROVE，2 偏离修复后复核)。
门禁：e2e 11/11 全绿、go test 全绿、28 页面 web_verify 无错误/溢出。

### 回顾（本次最重要的教训在协调者自身）

**哪里顺**：定义层（pm/designer）一次过、质量高；worker/frontend 照稿实施顺畅；
reviewer 抓出 2 个真实设计偏离（降级列表 hidden、侧栏硬编码语言表），修复后复核通过。
流水线分层本身被验证有效。

**哪里卡（4 次，全在协调者）**：

1. 侦察完停下等确认 → 用户：「你这是找我确认？……你就应该继续完成啊」
2. 排查公网环境时只查不启 → 用户：「为什么你没有继续执行下去？」
3. 只起了隧道没起服务就交付 URL → 用户打开是坏的：「不正常状态」
4. 诊断完 Passkey 域名问题说了方案没动手 → 用户：「你做了吗？」；
   e2e 起后台后不等待就交差 → 用户：「为什么你总是这样空着，没有实际的等待任务」

**根因**：协调者把「汇报进度」误当回合结束条件；后台任务点火就跑、无人等待；
原子步骤（环境=隧道+服务+验证）被切成半成品交付；隐形等待用户确认。
对用户而言「说了」不等于「做了」，只认跑完的结果。

**怎么改**：协调者回合结束三条件写入 AGENTS.md §0（见 L2/L4 改动）；
后台任务必须有等待/检查计划才允许结束回合；环境类任务原子交付。

**子 Agent 侧的小问题**：tester 首次跑全量 e2e 超时（30min 被截断，套件已写好但报告没落盘）——
后续 tester 任务应指示「先写套件+单跑新套件，全量 e2e 放最后且留足预算」。
reviewer 首次终审也被截断（turn 预算）——终审任务要明确「收敛范围、先写报告再深挖」。

---

## 历史归档：前端框架化改造（已完成 2026-08-04）

把 6 个手写 HTML 重构为「构建期 Go template 合成 + 布局复用 + i18n」静态站点。
保持 纯静态 + embed + hash.mjs 指纹 架构。详见 `.pi-orchestrator/archive/ARCHITECTURE.md`。

- [x] T1–T7 全部完成（调研/布局/sitegen/i18n/页面迁移/构建集成/验证），e2e 9/9 全绿

## 历史归档：消息详情页 + 推送深链（已完成 2026-08-05，commit da68540）

- 新增 message.html?id=<id> 详情页 + GET /v1/notifications/{id}
- 推送深链 url → message.html?id=；修复 payload base64 显示不全（messageView）
- 首页弹层字段补全 + 最新通知置顶（seq 降序）；e2e 全绿

## passkey-enroll：新设备授权添加 Passkey（已完成 2026-08-07）

**用户诉求**：换新设备时无法登录就加不了 Passkey（死锁）。方案：旧设备生成授权（二维码/URL/短码三合一），新设备匿名扫码/输码接入 → 旧设备批准 → 新设备本机创建 Passkey 绑定账号。与 CLI 授权同构。

**决策**：复用 cli_auth_sessions 表(加 kind 列) + 独立端点族 /v1/passkey-enroll/sessions/* + requested 敲门中间态(防短码泄露被薅 attestation)。新设备完全匿名。

**编排执行**：

- [x] 定义层：pm(requirements) + designer(design) + scout(context) 三份文档
- [x] 协调者拍板：decisions.md（独立端点族 + requested 态，覆盖 pm/designer 分歧）
- [x] 实现层：worker(后端 11 端点) + frontend(enroll 页 + security modal) + tester(23 Go e2e + 84 Node e2e)
- [x] 修 bug：tester 发现 poll 过早 consume 致 complete 永远 409（严重）+ knock 不存在会话 500（中等）
- [x] 终审：reviewer APPROVE，7 条安全不变量全满足，cli_auth 零回归
- [x] 门禁：make e2e 16/16 全绿

**产出**：5 commits（9ab9011 后端 → 3f2317c 前端 → 33c20e9 修 consume bug → 097d77a kind guard → f8359b8 initiatorName 语义）

**遗留风险**：complete 真实 attestation 成功路径无法在无浏览器认证器的 e2e 覆盖（tester 已标注）。

## 性能评估与五批修复（已完成 2026-10-31）

**起因**：用户要求「对各环境做性能评估 + bug 探查」，最多 3 路 centurion 并行，每个瓶颈开一条带上下文的 issue。

**评估层**：三路并行（A HTTP/存储 · B WS/多跳 · C 资源/泄漏/前端），产出 **25 条 issue + 3 份报告**
（`docs/perf/path-a-http-api-store.md`、`scripts/perf/b/REPORT.md`、`docs/perf/c-report.md`），
配套零依赖压测器 `scripts/perf/loadgen`（本机装不上 hey/wrk/vegeta）+ `ANOTIFY_PPROF` 手动挂载 pprof。

**修复层**：五批，每批独立子树 + 独立审查，协调者**自己复跑前后数字**后才合并。

- [x] 批次 1 正确性（#218 #222 #221）：租约归属到**具体执行**（task+instance+boot id）、`attempts` CAS、`LostAbandon` 宁可 abandoned 也不重复执行
- [x] 批次 2A 鉴权（#227 #245 #241）：argon2 缓存（HMAC 键 + 独立 verifier + generation 全表失效 + 容量上限）、会话缓存、hooks **双维 AND 限流**
- [x] 批次 2B 存储（#240 #238）：连接池 1→8 + `_txlock=immediate`（`:memory:` 恒 1）、迁移 15 索引收口
- [x] 批次 3A 查询（#242 #237）：语句句柄缓存 + 驱动 v1.45.0（消掉每次执行内部 parse）、迁移 16 索引 + `ANALYZE` + `PRAGMA optimize=0x2`
- [x] 批次 3B 响应（#223 #243）：`/v1/nodes` 批量查（N+1 → 常数条）、JSON gzip
- [x] 批次 4A 静态（#231 #232 #233）：进程内一次性 gzip + ETag/304、`classify()` 判真实产物名
- [x] 批次 4B 前端（#234 #235 #236）：图库按页加载、events 游标分页、`limit` 死区消除
- [x] 批次 5 容量（#229 #228）：保留期默认 30 天、背压超阈 503 + fail-open 安全阀

**关键数字**（同场次前后对比，绝对值不可跨场次比较）：

| 项 | 前 | 后 |
|---|---|---|
| `/v1/tasks` p95（100k 行） | 9836.3 ms | **123.0 ms（80×）** |
| Bearer 鉴权热路径 | 46.7 RPS | **19,727 RPS（422×）** |
| 带会话读端点 | 基线 | **+97%**（连接池）、CPU/请求 **−51%**（语句缓存） |
| `/v1/nodes` N=100 | 135 RPS / p50 58ms | **438 RPS / p50 17.1ms** |
| 大响应 `limit=500` | 183,453 B | **20,053 B（−89%）** |
| 首屏 14 子资源 | 402,534 B | **145,436 B（2.77×）** |
| 重复执行（sleep 60 场景） | 2 | **0** |

**门禁**：`make e2e` **1660 断言 / 0 失败**（起点 1548，五批新增 112 条）；`make ci` 连跑全绿。

**修复过程中新发现的 8 条 issue**（都不是评估发现的，而是"真跑起来并核对数字"才暴露的）：

`#249` latency_ms 装的是微秒 · `#252` 测试夹具钉单连接 + 冗余索引 · `#254` 禁用用户不禁 API Key ·
`#259` 收尾竞态（TempDir/db.Close）· `#264` 索引写放大 · `#265` 统计漂移不自动刷新 ·
`#268` `sw.js` 引用未指纹图标 · `#270` pending 无 abandon 路径

**方法学沉淀**（`scripts/perf/README.md`）：
绝对值不可跨场次比较（漂 1.6–2.4×）、`SetMaxOpenConns(1)` 下永远不会 `SQLITE_BUSY`、
**前后对比赛前必须确认跑的是当前代码编出的二进制**（本轮真因旧二进制误判过一次）。

---

## UI v2 前端大翻修（2026-11，分支 `feat/ui-v2`）

**目标**：从「开发者插件面板」到产品级 Dashboard；暗/亮主题、可折叠侧栏、统一组件语言。
**唯一事实源**：`.pi-orchestrator/UI-V2-CONTRACT.md`（v1.2）+ `design/ui-v2/REVIEW{,2,3}.md`。

- [x] Wave 1 外壳 + 首页/事件流：`tokens.css` 双主题（`data-theme` + localStorage `anotify-theme` + FOUC bootstrap）、
      侧栏 264↔76 纯 CSS 折叠 + tooltip、顶栏三态主题按钮、头像菜单、移动抽屉（Esc/焦点归还）、skip link。
- [x] Wave 2 Group A：sources / source / capabilities / edges；修 KPI 空位、`[object Object]`、亮色 login 分段控件、
      热力图 `--hm-empty`、sparkline 折线、折叠栏 footer、错误态一致性。
- [x] Wave 3 B/C/D（14 页）：nodes/node/keys/admin/receivers、playbooks/playbook/trace/docs/message、
      login/cli-auth/passkey-enroll/security。docs 8724→4488px + 右侧 sticky 目录；login 改分屏；
      全部页统一 `.page/.page-head/.page-title(Inter)/.card/.list|.data-table/.empty/.skeleton/Anotify.toast`。
- [x] 最终打磨：mock 形状对齐（recovery/status、POST /v1/keys）、`.tab.active` accent、亮色九态 ≥4.5:1、
      mock 提示条左下角可收起、KPI delta 语义、`.num`/`.st-text`/`.modal-panel` 组件修正。
- [x] 首页两列栅格（热力图∥事件，任务全宽）：2190 → **1585px**（1440×900）。
- [x] 独立验收（REVIEW-3）：**4.1/5，0 P0**；40 页次对比度不达标 0、溢出 0、`dupSub` 0、h1 唯一 20/20。
- [x] 收尾 4 条 P1：KPI 第 2 卡药丸误用（第 3/4 卡同型问题一并修）、语言横幅下侧栏 footer 裁 44px、抽屉可见关闭+焦点进入、admin 无权限态改 `.empty`。

**开页抖动修复（用户反馈）**：4 路 scout 定位——并非侧栏宽度动画，而是 `#scope-switcher` 异步插入 53px（每页 nav 下移）+ 语言提示条首帧后 0→45px 逐帧展开（index/login 整页下滑）+ 慢 JS 时外壳重挂载横跳。修复：`#scope-slot` 骨架占位、语言条首帧即终态（去逐帧 offset 写入）、head 预排版 `#page-main`；首页事件卡骨架同高。实测 nav.y 2 值→1 值、banner 11 帧→1 帧、index CLS 0.104→0.0038、慢 JS 内容 x 恒 296/158。新增 e2e 门禁 `ui_shift`（21 断言）+ frontend 滚动用例补数据落定等待；全量 **1822/0**。

**门禁**：`check-classes` 599 / `check-tailwind` 845 / `sitegen` 4×20 / `make fe` 22 资源；
`make e2e` 全绿（frontend 386/0；contract 曾因并行 SQLite `database is locked` 偶发，单跑 55/0）。

**遗留（非阻塞，按优先级）**
1. Google Fonts CDN（Inter/Fraunces/JetBrains Mono）与「私有化部署」定位冲突 → 自托管 woff2（断网时品牌字会掉回 system-ui）。
2. `/v1/stats.today` 仍是 UTC 口径（daily 已本地化）→ 跨午夜「今日」与热力图可能差 1 天。
3. mock `POST /v1/auth/recovery/regenerate` 仍返回 `{codes}`，真后端 `{ok,recoveryCodes}`；mock 两条种子 Key 缺 `enabled`。
4. 亮色 `--surface-3` 轨道面上 running/abandoned/lease_expired 4.25–4.47（正文面已 4.92+）。
5. 抽屉缺 `role="dialog"`/`aria-modal`/Tab 焦点陷阱（可见关闭与焦点进入已修）；`.st-text.st-<state>` 动态类未进 `check-classes` 白名单；`.toast` 副本仍有 events×2 + security×1。
6. docs 仍有 113 行 10.88px 辅助字；trace `.wf-switch .on` 未同步 `.tab.active` 新视觉。
7. 亮色 shim 只剩 `index.html` 的 `hover:bg-white/5` 与 `partials.js` 的 `ring-white/15` 两处引用，清完可整块删除。

**本地预览**：`NO_TUNNEL=1 MOCK_PORT=7788 ./scripts/mock.sh` → http://127.0.0.1:7788
（主题键 `anotify-theme`、侧栏键 `anotify-sidebar`；截图证据在 `design/ui-v2/shots*`，不入库）

---

## 任务记录 · outcome-v1.1（出口两态化：删「静默」与死配置）

**用户诉求**：取消「出口」的复杂概念，结果只留「成功 / 失败（带原因）+ 出口（可选）」；
借鉴 Rust 的 `Option` / `Result` 设计 —— 不要 null，不要永远清不掉的「配置遗漏」警告。

**最终模型**：`Outcome = { status, out: Option<Port>, message }`

| 字段 | 语义 | 参与路由 |
|---|---|---|
| `status` | succeeded / failed（展示 + 重试策略） | 否 |
| `out` | 选了哪个出口；NULL = 没选（到此为止） | **是（唯一）** |
| `summary` | 唯一 message：成功=做了什么 / 失败=为什么失败（失败必填，空则兜底 `"failed"`） | 否 |

- **I4 重写**：路由只看 `out`，不看 `status` → 失败回报出口即继续（旧的 `node_session.go` status 门槛删掉，失败边从「能配、永不生效」变成真能跑）
- **I5 重写**：`out` 两态（缺省 ≡ `null`）；「没回报」= 没有 result 帧，由租约/巡检表达；失败必须有原因
- **`reported` 退役**（写入恒 1）；**`error` 降为 `summary` 的派生位**（单一来源）
- **节点失败也读 `.anotify-out`**（`cmd/agent/runner.go`）—— 否则「失败也发通知」只有手写客户端能发
- **`edges.html` 的「配置遗漏」警告改成中性「到此为止」**（永远无法消除的警告 = 训练用户忽略警告）

**改动面**：4 个 Go 文件（协议/路由/节点/存储）+ 客户端协议注释；`tasks.reported` 列保留但退役、`error` 派生 —— **零 schema 迁移**；4 语言文案 + 3 个前端页面；`CONTRACT.md` I4/I5/§2.5/§4/A8(+A8b)、`openapi.yaml`、`nats-transport.md` 同步。

**新增断言**：`TestFailedResultWithOutRoutes`（失败+出口 → 下一跳）、`TestRunJobReportsOutOnFailure`（失败也读出口文件）、e2e A8b（失败扇出与成功完全一致）。

**门禁**：`go test ./...` 全绿；`make contract` → 62 路由 / 76 path / 97 op；
`./scripts/e2e/run_all.sh` 全绿（contract 59 · ws_protocol 82 · frontend 386 · i18n 299 · i18n_coverage 61 · 全量 24 套件 0 失败）。

---

## 动效 / 加载态优化（2026-11，分支 `feat/ui-motion`）

**审计**：4 路 scout（动效清单/加载态/逐帧视觉/运行时原型）。结论：全站仅侧栏 width 一个真动画；抽屉的 transform 过渡被 ui.css 覆盖成死代码；弹层/toast/主题/筛选 0ms 硬切；node/playbook/source CLS 0.285/0.178/0.133；admin 错误骨架永闪、security 错误吞成空。

**A · 动效基础层**：`:active` 按压缩放（120ms）、toast 入 180ms/出 160ms、弹层 scale(.98)+opacity 160ms、移动抽屉 240ms+遮罩 180ms、主题翻转旧色遮罩 200ms、KPI 值更新 160ms、列表 stagger 160ms/20ms、skeleton 改合成层 shimmer（`--dur-mid/--dur-toast/--dur-drawer` + `--ease-out`）。

**B · 加载态**：events 5×80px 骨架；admin 错误态 + 重试、停止永久骨架；security 错误/空分离、计数 `—`；node/source/playbook 等高骨架（CLS 0.285/0.133/0.178 → 0/0/0.0007）；trace 图区骨架；全站 `aria-busy`；index 热力图 87px + 最近任务 6 行骨架 + spark 数据前隐藏（CLS 0.0038 → 0.0002）。

**门禁**：e2e 全量 **1832/0**（frontend 386、ui_shift 21、lang_hint 57）；check-classes 629 / tailwind 879。截图在 `.pi-orchestrator/{motion-a,loading-b}-shots/`（不入库）。

**未做/Wave C**：跨文档 View Transitions、docs 目录平滑滚动、login tab 过渡、弹层退场动画（当前只做入场）。

### Wave C（2026-11-03）

- 跨文档 View Transitions：`@view-transition { navigation: auto }` + root 200ms 交叉淡入；reduce 下 `navigation: none`。实测 `pagereveal.viewTransition=true`（normal）/`false`（reduce），导航 298ms/182ms；frontend/ui_shift/lang_hint 全绿。
- docs 目录锚点：`html:has(.doc-toc)` 平滑滚动（reduce 下 auto）；实测 0→632px 约 400ms。
- login tab：`#login-panel/#signup-panel` 显示时 200ms 上浮淡入。
- events 列表加 `.anim-stagger`，筛选/首载 160ms/20ms 入场。
- 全量 e2e **1832/0**（首跑 ws_protocol 并行 SQLite 偶发 2 失败，单跑 82/0，重跑全量绿）。

---

## P0 · 产品可信度（2026-11-03）

- **字体自托管**：Inter（300–700 可变）/ Fraunces（340–420 + italic 400）/ JetBrains Mono（400–500）latin+latin-ext woff2 入库；三个 layout 删除 Google Fonts CDN；README 记录 OFL 来源/子集/体积。实测 0 个外部字体请求，`document.fonts.check` 全 true，首屏只拉 Inter 46.9 KiB。
- **mock 对齐**：`recovery/regenerate → {ok,recoveryCodes}`；种子 Key 全带 `enabled`；task id 改确定性 `tsk_mock_N`，index → message 刷新不再 404；revoke 置 `enabled:false` 且回 `{ok:true}`。
- **stats 本地时区**：`MessageStats` 今日改用 `startOfLocalDayUnix(now)`，热力图日桶加本地 offset；admin `SystemStats` 同步；新增 `stats_tz_test.go`（UTC+8/-5 边界/日桶）。
- **容器生效**：runtime 加 `tzdata`，compose/`.env.compose.example` 支持 `TZ`（默认 Etc/UTC，例 `Asia/Shanghai`）；openapi 描述同步。
- **验证**：`go test ./...` 15 包全绿；全量 e2e **1836/0**（含上游 index 事件流主列表改动）；fonts/ui_shift/lang_hint/contract 全绿；tunnel 200。
- **上游合流**：index 淘汰「最近任务」、事件流升主列表（8bad80e）；跨文档 VT 自动 opt-in 因脚本跳转竞态关闭（89a68b6，保留伪树样式，未来用 nav 助手再开）。

---

## P1 · 抽屉 a11y / 弹层退场 / 主题降并发（2026-11-03）

- **抽屉 dialog**：移动打开时 `role="dialog"` + `aria-modal="true"` + `aria-label`；Tab/Shift+Tab 在面板内循环（含首尾），Esc 关闭，焦点回 `#menu-btn`；桌面/放大后自动剥角色。实测 Tab 6 轮仍在抽屉、Esc 后 `aria-expanded=false`、role=null。
- **主题降并发**：`applyTheme` 换色时加 `html.theme-switching` 220ms，临时关掉元素过渡（遮罩自身除外）；实测 `getAnimations()` 99–181 → **9**，data-theme 仍同步、遮罩正常淡出。
- **弹层退场**：`Anotify.openModal/closeModal` 统一 140ms `.is-closing` 淡出后再 hidden；覆盖 keys/sources/capabilities/edges/admin/receivers/security/login；重开清退场态。实测关闭 30ms 时仍可见、200ms 后 hidden、重开正常。
- **顺带**：trace `.wf-switch .on` 对齐 accent-soft；`.brand-tile` 组件化，亮色白字保留；亮色 3 状态色压深到 4.83–4.87:1；`ring-white/15` shim 归零。
- **验证**：全量 e2e **1836/0**（frontend 387、ui_shift 21、admin_flow 51、security 30、real_backend 127、deeplink 31）。

### P1 与上游 VT 合流（2026-11-03）

- 上游 `31f6801`：把 `@view-transition { navigation:auto }` 内联进三个 layout 的 `<head>`（外部 ui.css 里 Chrome 来不及 opt-in，会中断并抛 InvalidStateError）；ui.css 只留伪树时长/缓动与 reduce 覆盖。
- 本地上游 merge 后实测：`pagereveal.viewTransition=true`、0 pageerror；全量 e2e **1836/0**（frontend 387、deeplink 31、ui_shift 21、i18n_coverage 61）。

---

## P2 · 工程守卫 + v0.2.0（2026-11-03）

- **check-classes JS 扫描**：新增 `class:` / 模板字面量扫描（动态 `${}` 跳过、保留静态片段），覆盖 `web/partials.js` + 23 个页面内联脚本；HTML 625 + JS 154 = **779 类**；`--self-test 11/11`；注入 `side-lnik` 在 HTML 与 partials.js 均红。
- **p2_guards e2e**：30 断言 —— 0 外部字体请求 / `document.fonts.check`（Inter/Fraunces/JetBrains Mono）/ 抽屉 dialog+Esc+焦点回退 / reduce token 0ms / VT 内联 opt-in + `pagereveal`；全量 **1866/0**。
- **shim 审计**：仍有 `text-zinc-500/600/300` 等 5 类活引用，**不整块删除**（会破亮色）；`lang-hint-inner` 补 `min-width:0` 真实定义后从 KNOWN_DEAD 移除。
- **发布**：`CHANGELOG.md` + annotated tag **v0.2.0**（UI v2 / 动效加载 / 字体自托管 / 时区 / P1 a11y）。

---

## P0 · 存量 issue 清偿（2026-11-06，11 个 open → 4）

- **裁决**：6 条「已修未关」关闭（#270 #249 #254 #268 #219 #220，均由 PR #277/`5ba1c81` 修完）；#220 按 v2 已删 `run.concurrency` 判「消解」；#265 证伪后二次修复；#252/#274 收尾；#273 决策 1 判定不做、只剩决策 2；#264 维持搁置（先出干净数字）。
- **代码**（7 bundle / 文件所有权互斥并行，合并 `83219b4`）：
  - `e28beed` #252：`-- +migration 6 drop_edges_lookup`（4 列 = edges 主键严格左前缀）+ `headVersion 5→6` + 9 处过时「单连接」注释；拆掉 `seq_in_trace_test.go` 把 `headVersion` 当迁移 5 的地雷。
  - `ab37f77` **#305 安全**：首用户判定移进 `RegisterUser` 同一事务（`RoleAuto`），原来 `UserCount`+`RegisterUser` 两次池操作在池=8 后可并发双 admin；文件库 + 起跑门 + 20 轮回归（修复前 3/3 必红）。
  - `1bb2921` #306：cancel 释放槽位补 nudge（`consoleHandler.nudge` 窄依赖注入）；`ErrNoCapacity` 不再写 `not_before`（原来对 `attempts≥1` 写 10s，把即时重派退化成定时）。
  - `a6b380e`+`e1ea039` #307：脚本切 v2（能力走「产物+部署」，node.yaml 只剩 bootstrap 四件）；`make demo-chain` 三跳实跑、perf s0–s6 全部 exit=0；devseed hello 改 `applied[]`+`concurrency 1`。
  - `f16c646` #252/#308：契约删索引 DDL、改掉从未实现的 `min(instances concurrency, …)`、§3.3 补 cancel/退避两句；`docs/perf` 的 abandon/连接池/latency 结论对齐现状。
  - `8af2fcc` #265：**证伪** `optimize=0x2`（100→2500 行仍 stat1=100，第一版是安慰剂）→ 改确定性漂移判定（估计 vs `COUNT(*)` 差 ≥10× 才 `ANALYZE "tasks"`），同一用例转绿。
  - `949ca07` #274：删 `api()` 第 3 参 demo 回退（传 null 吞掉所有失败）+ 10 处 `{demo:null}` + 3 段不可达分支 + 四语 59 行死 key + e2e 空操作 `?demo=1`。
- **验证**：`make e2e` 全量 **2273/0**（28 套件）；`make ci` 唯一失败为 `cmd/agent TestHandleUpgradeNeedRootDoesNotExit`（本机 root，`cmd/agent` 零改动），GitHub CI `test` 绿；`make contract` 绿。
- **新开**：#305（已修）/ #306（已修）/ #307（已修）/ #308（已修）/ **#310**（i18n_coverage A 组缺 `injectSession`，受保护页空转）/ **#311**（活文案仍说「演示模式」+ mock.js 伪造 `/v1/test-notify`）。
- **仍 open**：#273（触发 payload，产品已定「做」）、#264（写放大，待干净复测）、#310、#311。
