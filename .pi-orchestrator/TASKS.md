# Anotify 编排任务板

> 协调者（主 pi Agent）在此跟踪编排任务的进展。专路 Agent 定义见 `.pi/agents/anotify-*.md`，
> 编排架构与分层原则见 `AGENTS.md` 第 0 节。

## 编排团队（6 专路 + 内置）

| Agent | 层 | 模型 | 职责 |
| --- | --- | --- | --- |
| anotify.anotify-pm | 定义 | kimi-k3 | 需求/价值/边界/验收标准 |
| anotify.anotify-designer | 定义 | kimi-k3 | 信息架构/视觉方案/交互规格 |
| anotify.anotify-scout | 侦察 | deepseek-v4-flash | 摸代码现状 → context.md |
| (内置 planner) | 规划 | kimi-k3 | 拆任务 → plan.md |
| anotify.anotify-worker | 实现 | glm-5.2 | 后端实施 |
| anotify.anotify-frontend | 实现 | glm-5.2 | 前端照稿实现 |
| anotify.anotify-tester | 实现 | glm-5.2 | 测试把关/门禁 |
| anotify.anotify-reviewer | 终审 | kimi-k3 | 对照需求&设计终审 |

## 标准流程

pm 定义 → designer 设计 → scout 侦察 + planner 拆 →
worker(后端) ∥ frontend(前端) 实施 → tester 把关 → reviewer 终审 → 协调者合并并标 ✅

---

## 当前任务

（空 —— 等待协调者下达下一个编排任务）

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
