# #381 · 通知接收去留 + 首跑闭环 · 车道 REPORT（c-ia）

分支 `ui/369-firstrun`（worktree `c-ia`）· base `epic/369-ui-review`
决策：**B2** —— 决策 B 的「删文件」部分按 e2e 承重事实推迟为独立事项；本 PR 落地「不再无高亮、无入口（死键已除 + 页面自带返回路径）」+ 首跑闭环 5 项。

---

## 1. 逐条对应 issue 验收

| # | 验收原文 | 结果 | 证据 |
|---|---|---|---|
| 1 | 决策落地（A 或 B），且 `receivers.html` 不再「无高亮、无入口」 | ✅（B2 口径） | `active:"receivers"` 死键删除 → `mountLayout({active:null})`；侧栏高亮项 0（本来就没它，现在不再传一个不存在的 id）；顶栏不再尝试取不存在的分组名；页头补 `index.html` + `docs.html#agent` 两个真实出口。运行时实测：`page-actions a[href] = ["index.html","docs.html#agent"]`、`activeNav=0`、`topbarGroup=null` |
| 2 | 首页给出「第一条事件」三步路径（源 → 能力 → 节点），替代单点「接入新 Agent」 | ✅ | `index.html` 新增 `#first-event` 三步卡（每步真实链接 + 「现在做到哪了」）；页头原 `btn-primary 接入新 Agent` 降为 `btn-ghost 查看文档`（docs 不再是唯一路径） |
| 3 | 节点登记面板示例命令引用**当前实例真实存在**的能力（或给创建入口） | ✅ | `anode emit fixer --id review-a.go …` → `anode emit <真能力> --data '{}'`；无能力时整行 + 复制按钮隐藏，改为「去建一个能力 →」链接到 `capabilities.html` |
| 4 | 恢复码弹窗加「我已保存」确认（与 API Key 页一致） | ✅ | 新增 `#codes-saved-ack` 勾选；未勾选 `#codes-continue` 真 disabled；每次打开重置 |
| 5 | 实例无用户时登录页默认落在「注册」 | ⚠️ 见 §5（无服务端判定位） | 无会话 + 本浏览器首次来访 → 注册 tab + 提示条；回访（本机有登录记录）→ 登录 tab |
| 6 | e2e：新增首跑路径用例（无用户 → 注册 → 首页 → 三步引导可见） | ✅ | `auth_flow` 新增 `firstRunCase()`（含 14 条断言，见 §4） |

## 2. 三步路径的三态判定逻辑

`index.html` 的 `loadFirstEvent()` → `renderFirstEvent()`：

1. 三个列表接口各取 `limit=1`（只要「有没有」，不把全量拉进首屏）：
   `/v1/namespaces/{ns}/sources`、`/v1/namespaces/{ns}/capabilities`、`/v1/nodes`。
2. 每步记 `true`（有）/ `false`（没有）/ `null`（取不到 = 未知）。**未知不算已完成、也不算没有**。
3. `current` = 第一个 `false` 的步骤；`done` = `true` 的步骤；其余 `todo`。
4. `empty` = 三步都已知且都为 `false` → 三步都加 `.is-lit`（**空实例三步全亮**），第 1 步额外 `aria-current="step"`。
5. 三步都 `true` → `#fe-done` 显示（「去节点页复制 emit 命令」），三步卡收成一行，不占首屏。
6. 渲染完成才置 `#first-event[data-ready="1"]`——e2e 等这个标记，不靠 sleep，也不会把模板初始态误判成「还没做」。

实测三态（真实服务，`.pi-orchestrator/out/issue-381/*-state.json`）：

| 实例状态 | states | is-lit | 状态文案 | `#fe-done` |
|---|---|---|---|---|
| 空实例（0/0/0） | `current, todo, todo` | `true, true, true` | 现在做这一步 / 等待上一步 / 等待上一步 | hidden |
| 有源+有能+有节点 | `done, done, done` | `false, false, false` | 已完成 ×3 | 显示 |
| 有源、无能（e2e 实测） | `done, current, todo` | `false, false, false` | 已完成 / 现在做这一步 / 等待上一步 | hidden |

## 3. 示例命令取到哪个能力

- 端点：`GET /v1/namespaces/default/capabilities?limit=500`，取 `items[0].metadata.name`。
- 为什么先看 `default`：上方生成的 `anode.yaml` 写死 `namespaces: [default]`，emit 必须用节点实际运行的命名空间；`default` 没有再看用户当前在看的 namespace。
- 渲染：`anode emit <name> --data '{}'`（`cmd/anode/emit.go`：`--data` 默认 `{}`，位置参数是 target → 可直接复制运行）。
- 复制：`#reg-emit-copy`（复用页面既有的 `A.copyText` 管线）。
- 实测：种子能力 `shot-cap` → `emit = "anode emit shot-cap --data '{}'"`，`hint = "这一行用的是本实例已存在的「shot-cap」，可直接复制运行。"`；空实例 → `emitHidden=true, copyHidden=true, createShown=true`。

## 4. e2e

新增用例（`scripts/e2e/suites/auth_flow.mjs`，`firstRunCase()`，套件第 0 步，跑在「库里还没有任何用户」时）：

```
首跑：首访默认选中「注册」tab / 此时「登录」tab 不选中 / 登录面板默认收起 / 注册面板默认展开
首跑：给出「第一次使用」提示
首跑：注册后展示一次性恢复码（真实 UI 表单 + 虚拟认证器）
首跑：恢复码一次展示 8 个
首跑：恢复码弹窗有「我已保存」勾选 / 勾选默认未选中 / 未勾选时「继续」禁用 / 勾选后「继续」可用
首跑：三步顺序 = 源 → 能力 → 节点
首跑：每步都链到真实页面
首跑：空实例三步全亮
首跑：每步都有「现在做到哪了」的文案
首跑：空实例不谎报「三步已完成」
首跑：有源后第一步变「已完成」/ 下一步指向「能力」/ 此时不再三步全亮
```

结果：`auth_flow` **49 通过 / 0 失败**（改前 35 条，新增 14 条）。

改断言（不删断言）的地方，逐条说明：

| 文件 | 改动 | 为什么 |
|---|---|---|
| `suites/password_login.mjs` | 恢复码处补 `isDisabled → check(#codes-saved-ack) → isEnabled → click` | 新增的「我已保存」是必经交互；顺手把门禁本身断言两次（未勾选禁用/勾选可用） |
| `suites/password_login.mjs` | 切到登录页后先 `click(#tab-login)` + 断言面板可见 | 默认 tab 现在是数据相关的；原来直接点 `#password-login-toggle` 在新的默认态下不可见 |
| `fixtures/f3/f3-ui-helper.mjs` | `ANCHORS.login`：`#login-panel` → `.auth-tabs` | 登录页结构锚点不能是「登录面板可见」——默认 tab 现在跟状态走；`.auth-tabs` 两种默认态都可见，仍是「这页真渲染了」的真事实 |
| `fixtures/f3/f3-i18n-cases.mjs` | `anchors.login`：同上 | 同上（i18n / i18n_coverage 逐页 `waitFor(visible)`） |
| `fixtures/f3/f3-cases.mjs` | `auth-route-guards`：`waitLogin` 后补 `click(#tab-login)` | 该用例验的是「401 后能拿到公开的密码登录表单」，不是「登录 tab 是默认」 |
| `fixtures/f3/f3-node-retained.mjs` | `goto('login')` 后补 `click(#tab-login)` + `#login-panel:not(.hidden)` 可见 | 同上；额外把 tab 切换也验了一次 |

**没有**改动：`PAGES` 仍是 19 页、`MATRIX_VIEWS` 仍是 304、`KEY_PAGES`/`DATA_PAGES`/批次不变、`scripts/v4-frontend.test.mjs` 一行未动、retained 用例 id 与数量不变。

## 5. 自测（命令 + 结果）

```
make fe                                     → PASS（sitegen 4 langs × 19 pages + 指纹）
node scripts/check-classes.mjs              → PASS（827 个类全部落在设计系统/Tailwind 内）
./scripts/e2e/run_all.sh --serial auth_flow     → PASS 49/0（含新首跑用例 14 条断言）
./scripts/e2e/run_all.sh --serial password_login → PASS 78/0
./scripts/e2e/run_all.sh --serial deeplink       → PASS 50/0
./scripts/e2e/run_all.sh --serial frontend       → PASS 36/0
./scripts/e2e/run_all.sh ui_shift ux_regression_19_23 i18n i18n_coverage
  → ui_shift           PASS 83/0（304 视图全计数）
  → ux_regression_19_23 PASS 8/0（retained 用例 id/数量不变）
  → i18n               27/2（**剩 2 条 = 基线红，见 §6.5**；改前 6 条，我的 anchor 修好 4 条、新增 0 条）
  → i18n_coverage      38/2（同上，message 批次）
node --test scripts/v4-frontend.test.mjs    → F3b2b 三个注册表用例 3/3 PASS（304 视图 / retained id / needles 全部守住）；
                                              其余 17 条基线红见 §6.4
```

视觉自检（真实服务，非 mock：`/tmp/anotify381` + 空库注册出的用户）：
`1440×900` 与 `390×844` × `light/dark`，每页断言 **0 console error、0 pageerror、`scrollWidth ≤ innerWidth + 2`**：

```
index-empty      4/4 ✅（lit=[true,true,true]）      index-all-done  4/4 ✅（done×3 + #fe-done 显示）
nodes-no-cap     4/4 ✅（emit 隐藏 + 建能力入口）    nodes-with-cap  4/4 ✅（anode emit shot-cap --data '{}'）
login-first-run  4/4 ✅（注册 tab + 提示条）         login-returning 4/4 ✅（仍是登录 tab）
login-codes-modal ✅（continue disabled=true，勾选文案「我已保存这些恢复码」）
receivers        4/4 ✅（activeNav=0、两个真实出口、两句未开放文案）
docs             4/4 ✅（off_note 可见、push 章节 hidden）
合计 32 张 PNG + 状态 JSON → .pi-orchestrator/out/issue-381/（gitignored，不入库）
```

## 6. 发现的产品 bug / 契约事实

1. **PLAN.md §5「已冻结决策（#381）：e2e 无套件引用该页，风险低」不成立**（已上报，据此改成 B2）。
   `receivers.html` 是 4 个套件 + 1 个硬门禁的承重面：`f3-driver` PAGES → `f3-cases` `pages-19-*`（4 语言断言 `All 19 pages must pass`）、
   `f3-ui-helper` 的 `MATRIX_VIEWS=304`/批次 `[15,19]`/`KEY_PAGES`/`ANCHORS`/`DATA_PAGES`、`f3-i18n-cases` 3 个 receivers 专属用例、
   `v4-frontend.test.mjs:5392/5401/5402/5407` 三处常量 + `:5424` 逐 id 钉住两个保留用例。
2. **删页时必须一起改的 6 个落点**（留作独立事项，别当机械删除）：
   ① `f3-driver.mjs` `PAGES`/`GUARDED`；② `f3-ui-helper.mjs` `MATRIX_VIEWS`(304→288)/`MATRIX_BATCHES`(`[15,19]`→`[15,18]`)/`KEY_PAGES`/`ANCHORS`/`DATA_PAGES`；
   ③ `f3-cases.mjs` `pageAnchors` + `pages-19-*` 命名与「All 19 pages」文案；④ `f3-i18n-cases.mjs` 的 anchors + 3 个 receivers 用例；
   ⑤ **两个反-theatre 保留用例**：`ux20-notification-denied`（拒绝权限绝不注册 SW / 零 device 写入）、`retained-device-patch`（设备行 toggle → PATCH `/v1/devices/{id}` 且 body 只带 `enabled`）——后者随 UI 一起退役前要先决定它的替代面；
   ⑥ `scripts/v4-frontend.test.mjs` 三处常量 + `:5424` 的 id 清单；另加 `docs/dev-docker.md:182` 一行活文档（把我漏了）。
3. **产品发现（删页的连带后果）**：`receivers.html` 是 `GET/PATCH /v1/devices`、`/v1/push/*` 在 Web UI 里的**唯一**消费方。删页后这些接口没有 UI 消费方（CLI/客户端仍可用），`retained-device-patch` 也就没有可测面了——这是删页真正的代价，不是文件数。
4. **基线红（不是我引入的，已实测确认）**：`node --test scripts/v4-frontend.test.mjs` 在本 worktree 的 **HEAD（未含我的改动）** 上就红 17 条：
   `F2b2 trace/index controllers`（`trace-graph._mountSpec.nodes.length >= 7`）、`TRACE-STARTUP *`、`SOURCE-STARTUP *`、`R1 graph clicks`、`R1 sources template copy`、`E1b all 35 … registered IDs`、`TRACE-ARCHIVE …`。
   与 #381 改动面无关（trace/source 图库、图点击）。**CONTRACT §2 的基线红清单里没有这一条**，建议补进去，否则后续车道会把它当成自己引入的回归。
5. **i18n / i18n_coverage 各剩 2 条也是基线红（已取证）**：两条都在 `message` 页，
   `#task-params` 的 `DD` 节点上是 mock 任务参数值 `部署完成`，被「no API substring exemption」的严格扫描判成
   「en/es 页面出现中文 UI 残留」。来源：#375 把裸 JSON 改成结构化视图后，参数**值**第一次成为可见文本节点
   （`223c1d2` 已在 `epic/369-ui-review` 上、已在我的 base 里）；`部署完成` 本身是 `web/mock.js` 里 #160 起的 mock 数据。
   **取证方式**：把我对 `f3-i18n-cases.mjs` 的唯一改动（login anchor）临时回退后重跑 → 6 条失败（4 条 login + 同样这 2 条 message）；
   再把改动放回 → 2 条。即：**这 2 条与 #381 无关，且我没新增任何失败**。
   修法不在本车道：要么给 `#task-params` 的真数据加「data 不是 UI chrome」豁免，要么把 mock 演示值换成非中文；属 #375 后续或 i18n 扫描 owner。

## 7. 遗留风险

1. **«实例无用户» 没有服务端判定位**：全仓 grep `needsSetup|setupRequired|UserCount` → HTTP 层没有任何匿名可读的「实例是否已初始化」接口（`UserCount` 只在 `store` 内部用于「首用户成为 admin」）。
   按「别发明接口」，实现只用两条真事实：`/v1/auth/me` 401 +「本浏览器在本站从未成功登录/注册」（`localStorage["anotify.login.seen"]`，由登录页唯一跳转出口 `navigate()` 写入）。
   - 于是**换浏览器 / 清 storage 的回访用户会先看到注册 tab**（一次点击可切回，界面不会声称"实例是空的"）。
   - 隐私模式（`localStorage` 不可用）→ 保持登录 tab，不猜。
   - **正确的根治**：`partials.js` 在拿 `/v1/auth/me` 200 的挂载路径上写这个标记（一行），或后端加 `GET /v1/auth/instance`。前者属 `web/partials.js` 的 `loadMe` 区段（#378 车道在写），后者是服务端改动——都不在本车道的可写范围，建议单开。
2. **`?next=` 不能当回访信号**：`partials.js` 的 `redirectToLogin()` 对所有 401 都拼 `login.html?next=<page>`，首跑用户从 `/` 进来一定带 next（`next=index.html`）。实现里已显式避开这个陷阱并写了注释，避免后人「优化」时踩回去。
3. **i18n 键未删**：`receivers.*` 全套（含 `unavailableTitle`/`unavailable`/`device.*`/`pair.*`/`checklist.*`）保留未删（决策 B 推迟），本 PR 只**新增** `receivers.back_home`/`docs_link`/`unavailable_next`。
4. **未动 `web/partials.js`**：`receivers.html` 仍在 `redirectToLogin()` 白名单（页面还在，深链 `?next=receivers.html` 仍需保留），`login.html` 的 `NEXT_MAP` 同理保留——两者都随删页事项一起处理。
5. **`index.quickstart.cta`**（「接入新 Agent」）现在只剩 quickstart 横条在用，页头不再用——没有删除键盘，避免无谓 diff；#373（c-vis 强调密度）若继续收「接入」口径可一并清理。
6. **三态判定用的是 `limit=1`**：只知道「有没有」，不显示数量。若产品要「已完成 · 3 个源」这类计数，需把 `limit` 调大或加计数接口（本 issue 未要求）。
