# Anotify UI v2 · 第二轮设计批评（wave 1 验收）

> 事实源：`.pi-orchestrator/UI-V2-CONTRACT.md`（v1.1）· `design/ui-v2/REVIEW.md`（D1–D20）· 实现分支 `feat/ui-v2`。
> 本文只读产品代码；所有截图由 `design/ui-v2/shots-review2/shoot.mjs` 自采（playwright-core 1.62 + chromium-1208），
> 覆盖 9 页 × 2 主题 × 1440×900、index/events 整页、折叠态、390 抽屉、键盘焦点、hover、错误/空态。
> 行号指**当前实现**（`web/`、`web-src/`）；`design/ui-v2/{gallery,dashboard,login}.html`、`REVIEW.md` 未改动。

---

## 0. 一页结论

wave 1 把 REVIEW 诊断的 R1（没有材料）、R3（标题不可信）、R4（组件各写各的）**治好了大约六成**：
卡片有了实底与阴影、`Inter` 正文 + `Fraunces` 只在 28px 以上、12 个组件类被真正用起来、
首屏第一次有了「问候 → 4 KPI 位 → 活动 → 任务」的骨架，整页 4176 → **2190px**（契约 ≤2600 ✅）。

但**还不能叫产品级**，三条硬伤各自独立且都在首屏可见：

1. **KPI 网格右半边空着**（564×220px 死区）——这正是契约 §10 明文禁止的「空网格位」，实现用的是固定 4 列。
2. **事件流的错误态把对象打成字符串**：断网时列表显示 `载入失败：[object Object]`（已截图）。
3. **亮色下登录页的「登录/注册」分段控件整体消失**——容器、active 项、卡片底三层在亮色下被 shim 压成同一个 `--surface-2`。

再加上：热力图空格在深色下几乎不可见（D17 要求 `--hm-empty`，实现退回 `--surface-2`）、
折叠侧栏底部语言切换被压成竖排并溢出导轨、`docs` 页把 locale 里的 HTML 标签当文本显示（14 处）——
这些都不是打磨项，是**用户会当成"坏了"**的东西。

**wave-1 结论：骨架达标，产品级未达标。建议做一轮 P0 修复（预计 ≤1 天）再开 wave 2。**
理由：wave 2 要把这套语言推广到 18 个页面，而上面 3 条里有 2 条（KPI 空位、三层同色）是**组件层/AI 默认值**层面的错误，
不修就等于把错误复制 18 遍。

---

## 1. 打分（1–5，每项一句证据）

| 维度 | 分 | 证据 |
|---|---|---|
| **层次** | **4** | 首页 y=120–160 的问候 28px Fraunces + 13px sub，与 y=200 的 `.stat`（30px/600 数值 / 12.5px 标签）形成三级；顶栏 h1 15–16px 稳定退为上下文标签（`index-dark-1440.png`）。扣分：KPI 死区把「4 卡并排」的横向节奏砍成 2 卡，右半边没有任何东西引导视线。 |
| **密度** | **3** | 2190px（契约 ✅ / 设计稿 1545px），但 6 个信息块**单列纵向堆叠**，KPI 死区 564×220px + events 行 80px/行（index 的 `.notif-row` 实测 57px，**正中** D13 的 56px——所以问题不在“列表做不紧凑”，在 events 用了另一种列表）。首屏（900px）只装了「问候+2 KPI+半个事件卡」；信息不缺，缺横向组织——与 R2 同因。 |
| **一致性** | **3** | 迁移过的两页自己就不一致：index 的「最近任务」是一张卡 + 15 行 `.notif-row`，events 的「最近事件」是**一行一张 `.card`**（`events.html:876`）。我抽查契约 §5 的 27 个新类，**10 个 0 使用**（`.card-fill/.card-foot/.btn-quiet/.table-wrap/.data-table/.skeleton/.kbd/.page-narrow/.hm-scroll/.st-text`），`.empty` 作为组件类全仓 0 引用。 |
| **亮色** | **2** | index/events 亮色干净（自检仅 15 处 <4.5，全部是 9–11px 的 `--faint`）；但 login 亮色分段控件不可见、capabilities 亮色 81 处不达标（最差 `kind-end` **1.92:1**）、`out-chip` 2.92:1。shim 只覆盖 Tailwind 工具类，页面内联 `<style>` 的硬编码 hex 完全不在射程内。 |
| **可访问性** | **3** | 22 个 tab stop 全部有 2px accent 焦点环（实测），抽屉 Esc ✓ / 焦点归还 `#menu-btn` ✓ / `aria-expanded`+`aria-controls` ✓，头像菜单 Esc + 点击外部关闭 ✓，`prefers-reduced-motion` 生效（1e-06s）✓。扣分：无跳过导航；主题按钮只有图标且 `aria-label` 只述状态不述动作；热力图月份 9px/3.18:1；7 个页面深色下 `text-zinc-600` 2.45:1。 |
| **移动端** | **4** | 390：抽屉 264px + overlay blur、内容不被挤压、KPI 2 列填满、`scrollWidth-clientWidth=0`（9 页全 0 溢出）、侧栏链接点击自动关闭。扣分：抽屉无可见关闭按钮、焦点不进入面板；固定 mock 药丸在 390 下遮住抽屉里的用户块与列表行。 |

**加权印象分 3.2 / 5**。作为"这一轮能不能开始复制到 18 页"的门槛：**不能，先修 P0**。

---

## 2. Findings

严重度口径：**P0** = 用户会判定为"坏了"/违反契约裁决；**P1** = 明显降低产品感或可读性，wave 2 前必须修；**P2** = 打磨。

### 2.1 P0

| # | 路径:行号 | 证据（一句话 + 截图） | 建议修法 |
|---|---|---|---|
| **F1** | `web-src/pages/index.html:26` · `web/partials.js:1836` · `:58/:120` | KPI 网格写死 `grid-cols-2 lg:grid-cols-4`；两张 `[data-push-tile]` 因 `PUSH_UI_ENABLED=false` 被 `hidden`，1440 下实测量出 `grid-template-columns: 266px ×4`、第 3/4 列整列空（x=860–1408，564×220px）。**直接违反契约 §10「首页不允许出现空网格位…让网格自适应为 2 列」**。截图 `index-dark-1440.png`（右侧大片黑）。 | ① 补齐 2 张真卡（用已请求的 `/v1/stats`+`/v1/nodes`：待处理告警 / 平均处理时长，§10 明确允许不新增 API）；② 保底：JS 按可见卡数设 `grid-template-columns`（2 卡 → `repeat(2,1fr)`，卡宽 ~548px）。两者取一，**不允许留空格**。 |
| **F2** | `web-src/pages/events.html:709` · `web/partials.js:1862-1872` | 断网时事件列表渲染 `载入失败：[object Object]`。原因：页面只保留 `loadErrorStatus` 并传 `{status}`，`loadErrorNode()` 里 `String(err.message \|\| err)` → `"[object Object]"`，且 `/Failed to fetch/` 不匹配该串，最终 `reason` 落在这个字符串上。截图 `errnet-events-dark-1440.png`。 | 传真实 Error（`catch (e)` 收着）或 `{status, message: t("common.backend_offline")}`；并在 `loadErrorNode` 里把 `"[object Object]"` 兜底成默认文案。§7 门禁加一条断网截图断言。 |
| **F3** | `web-src/pages/login.html:96` · `:41` · `web/ui.css:46` | 亮色下「登录/注册」分段控件**三层塌成同一个颜色**：`.glass` = `--surface-2`(#f4f5fa)、容器 `bg-white/[0.04]` 被 shim 映射到 `--surface-2`、`.tab-btn.active` 也是 `--surface-2`。截图 `login-light-1440.png`：容器与 active 项都看不见，只剩「登录」字重略粗。 | 三级取值：卡片 `--surface-1`、容器 `--surface-3`、active `--surface-1`+`--shadow-1`（或 active 用 `--accent-soft`+accent 字）。同时把 `.tab-btn` 换成 `.tabs/.tab`，按契约 §2 保留 `.tab-btn` 作附加类。 |
| **F4** | `web-src/locales/zh-CN.yaml:731,734,737,765,780,807-811,830,851`（14 处 `class="`）· `web-src/pages/docs.html:136,154` | `docs` 页把 locale 里的 HTML **当文本显示**：首屏直接看到 `Anotify 的核心场景：<b class="text-zinc-200">Agent 完成任务后…</b>` 与 `请到 <a href="keys.html" class="text-indigo-300…">API Keys</a> 页新建…`。截图 `docs-light-1440.png` / `docs-dark-1440.png`。`{{t}}` 走 html/template 转义，全仓没有 `safeHTML` 通道。 | 二选一：① 词条拆成「纯文本 + 结构化片段」（推荐，`{link}`/`{code}` 占位符由模板拼 `<a>`/`<code>`）；② 加一个受控 `{{thtml ...}}` 只允许 `b/a/code` 白名单。**同时**把词条里的 `text-zinc-200/bg-white/5` 换成 token —— 否则修好转义那天就是 14 处暗色写死复活那天。 |
| **F5** | `web-src/pages/index.html:1039-1045` · `web/ui.css:752-760` · `web/tokens.css`（无 `--hm-empty`） | 热力图空格 `LEVELS[0] = var(--surface-2)`，在 `--surface-1` 的卡上是 **#15151f on #101018 = 1.12:1**，实测 371 个格里除了 accent 亮块几乎看不出网格；图例 5 个色块里有 4 个不可见（`index-dark-1440-heatmap.png`）。D17 明文要求 `--hm-empty: #171722`「**不是** `--surface-2`，否则在实底卡上看不见」——该 token 从未定义。 | 在 `tokens.css` 补 `--hm-empty`（dark `#1c1c29`、light `#e8eaf3`，需 ≥1.3:1 以可辨）并让 `.hm-cell` 默认底改用它；`LEVELS[0]` 同步。图例改为「空 → accent-1..4」6 格。 |

### 2.2 P1

| # | 路径:行号 | 证据 | 建议修法 |
|---|---|---|---|
| **F6** | `web-src/pages/events.html:99` vs `:194` | 顶栏 h1「事件流」+ 副标题与 hero「事件流」+ 副标题**逐字重复**（脚本断言 `dupTitle=true, dupSub=true`）。`sources.html:408/:660`、`nodes.html:78/:293` 同样 `dupSub=true`（截图 `events-dark-1440.png` y=45 与 y=140 两段一模一样的 40 字长句）。`capabilities.html:579` 已有正确做法（顶栏用短 key `caps.sidebar`）并在注释里写明了理由——另外三页没跟上。 | 约定：**顶栏 subtitle = 版块上下文（≤12 字）**，hero `.page-sub` = 本页解释性长句，两者不得同 key。给 events/sources/nodes 各补一个 `*.topbar` 短 key（4 locale）。验收：`dupSub=false 且 dupTitle=false`。 |
| **F7** | `web/ui.css:2209` + `web/partials.js:396-400` | 折叠态（76px）语言切换**溢出导轨且竖排**：`#lang-switcher` 实测 90×58 @ x=-7（父容器右侧溢出 18px），内部「中文」被压成上下两行；同一时刻 `#sidebar-avatar` 位于 y=893（h=36 → 底 929 > 视口 900），用户块被裁掉约 30px。截图 `index-dark-1440-collapsed.png`、`index-dark-390-drawer.png`。 | 折叠态给 `.lang-trigger` 去 `w-full`、`justify-content:center`、只留地球图标（`title`/`aria-label` 保中文名），宽度锁 40px；用户块折叠态只留头像且 `padding` 收到 0（已有规则，被 lang 的溢出顶开）。 |
| **F8** | `web/ui.css:2032-2047`（`position:sticky; top:0; height:100dvh`）· `web/partials.js:1926-1930` | `.app-sidebar` 高 `100dvh` 但它位于 45px 高的语言提示条**之后**，实测 `#sidebar` 的 `bottom - innerHeight = +45px`（900/820/768/700 四个高度全部 +35~45px）。结果是**首页/登录页**（只有这两页注入 banner）在任何视口下侧栏底部都被裁。另：`height ≤ 700` 时最后一个 `.side-link`（bottom 653）与 footer（top 633）**重叠 20px**（`.app-nav{overflow:visible}` 为 tooltip 让路）。 | ① `height: calc(100dvh - var(--shell-top-offset, 0px))`，由 banner 开合写 offset；或把 banner 改成 overlay。② 按 D3 的预案：`@media (max-height:760px){ .app-nav{overflow-y:auto} }` 并让折叠 tooltip 退化为 `title`。 |
| **F9** | `web-src/pages/index.html:26-160` vs `design/ui-v2/dashboard.html:400-455` | KPI 卡丢了设计稿的**三个签名元素**：① 无 `.stat-delta`——该位置被当普通标签用，实测 `#kpi-total-trend` 是 **12px/18px 高的纯文本「实时」**（无药丸底、无圆角、无 ↑/↓、无数字），D7 要的是 22px 药丸 + 趋势；② `.stat-foot` 全页 **0 个**；③ sparkline 是 **7 根宽色块柱**（`index.html:355-370` 生成 `<rect>`，`preserveAspectRatio="none"` 拉伸后每根 21px）而非设计稿的 `path.line`+`path.area`（`dashboard.html:409-412`）。数值也没有 `.unit`（设计稿 `328` / `98.6%` / `6/7` / `1.8s`）。 | 折线：`<path d="M…L…" vector-effect="non-scaling-stroke">` + 13% alpha area；delta 药丸必须有 ↑/↓ + 数字（无数据时显示 `–` 而不是用「实时」占位）；`.stat-foot` 放「近 30 天 · 更新于 2 分钟前」。 |
| **F10** | `web-src/pages/events.html:876` vs `web-src/pages/index.html:270-300` | 同一产品里两种列表语言：events 每行一张 `.card card-hover p-4`（9 张卡、80px/行、9 份阴影），index「最近任务」一张卡 + `.notif-row` 行。截图 `events-dark-1440.png` 与 `index-dark-1440-full.jpg` y=1030 对比。 | events 改成一个 `.card` + `.list/.list-row`（56px，行间 `border-top`），整卡 hover 取消；`chev` 只在行 hover 出现。100 行/页时这个差别会放大 10 倍。 |
| **F11** | `web-src/pages/events.html:896` | 每行右侧一个 `.badge badge-accent`「查看链路 →」，9 行重复 9 次；同一行左侧的 source 也是 accent 药丸 → 一屏 18 个 accent 元素，70/20/10 的 10 被拉平。而且整行本身就是 `<a href="trace.html">`（`:876`），这个 badge 既不可点也不增加信息。 | 删掉 badge，改为行尾 `chev`（hover 显现）；或保留但降级为 `.card-sub`「查看链路 →」。 |
| **F12** | `web/ui.css:3086-3200`（light shim）· `web-src/pages/capabilities.html:41-49` | shim 只映射 Tailwind 工具类；页面内联 `<style>` 的硬编码 hex 全部漏网。capabilities 亮色实测 81 处 <4.5，最差 `.kind-end`(#34d399 on #fff)=**1.92:1**、`.kind-pipe`(#60a5fa)=2.54、`.kind-origin`(#a78bfa)=2.72；`.out-chip`(=`--faint` on `--surface-2`)=2.92；`.src-badge/src-manual`=3.77；`.status-warn`=4.3（截图 `capabilities-light-1440.png`）。**根因是 shim 把多种语义压到同一个 `--surface-2`**（F3 同因）。 | shim 只能收缩不能扩张（契约 §10）→ wave 2 每页迁移时：① 删掉页内硬编码语义色，改用 `--success/--error/--warn/--info` 或新增 `--st-*`；② 表面按层级取 `--surface-1/2/3` 三档，不要一律 `--surface-2`。 |
| **F13** | `web-src/pages/{docs,capabilities,nodes}.html` 中 `text-zinc-600`；`web/ui.css` 无 dark 侧 shim | **深色下** 也是 2.45:1：`#52525b on #101018`。不达标节点数 capabilities 59 / docs 44 / nodes 30，其中 `text-zinc-600` 是主因（capabilities 前 14 名全部是它，含 `hash1c514c` 这类 10px mono 哈希）；另有 `.out-chip` 3.8:1（nodes 10 处）。不是亮色专属问题。 | 深色侧同样需要一层「`text-zinc-600/700` → `--muted`」的语义收敛，或直接删掉这些 10px 辅助信息（哈希/声明节点在列表页本就不该出现）。 |
| **F14** | `web-src/pages/index.html:586-587,644,725` | API 全挂时首页**自相矛盾**：`#notif-list` 正确显示「载入失败 / 重试」，但 `.page-sub` 断言「最近收录 **0** 条任务」、`.stat-value` 断言 `0`、最近事件卡显示成功态文案「还没有事件。接上 webhook 或定时触发器后…」。截图 `errnet-index-dark-1440.png`。空态与错误态不可区分 = 用户会以为数据丢了。 | 三处统一：KPI 失败显示 `–` 且 `.stat` 加 `.is-error` 角标；`.page-sub` 失败时改成「载入失败」；最近事件卡复用 `Anotify.loadErrorNode()`（组件已存在，`index.html:725` 已在用）。 |
| **F15** | `web/partials.js:208-235` | 品牌 logo 上是 `bg-red-500 + animate-ping` 的**红色通知点**，位置 `-right-1 -top-1` 半出框。它没有语义（不是未读数），却永续脉动；折叠态（76px）下 logo 瓦片 `bg-black` 与侧栏 `--bg-raise`(#0c0c13) 几乎同色，**视觉上只剩那颗红点** —— 任务书里「品牌 logo 只剩红点」的现象得到证实，但根因是「红点在、logo 不在」而不是 logo 被裁。 | 去掉 ping（或改成 1 次性入场动画）；红点语义化（真有未读才出现，并给 `aria-label`）或换成 accent 描边；折叠态把品牌字降级为 `A` 字标（首字母），保证导轨内仍有可识别品牌。 |

### 2.3 P2

| # | 路径:行号 | 证据 | 建议修法 |
|---|---|---|---|
| **F16** | `web/ui.css:57-67` | `.side-link` 仍是旧规格：`border-radius:10px`、`font-size:14px`、`padding:8.8px 12.8px`、高 41px；D4 定的是 8px / 13px / 38px / `0 10px`。active 的 2px accent 条实际渲染为药丸**内**左侧 2×18px 小竖条（`::before left:0`），不是 D5 的「贴侧栏边缘」。 | 按 D4/D5 改数值；accent 条改 `left:-10px`（折叠态 `-8px`）并给 `.side-link` 加 `position:relative`（已有）。 |
| **F17** | `web/partials.js:1189` | 顶栏 h1 `text-base` = **16px**，契约 §10 C1 裁决是 15px/600。 | 改 `app-topbar-title` 组件类（15px/600/`-0.01em`），别再让页面用工具类定字号。 |
| **F18** | `web/ui.css:2250-2255`（`.app-topbar-sub` 12px）· 三处 9px 热力图标签 | 字号阶梯没收敛：`.app-topbar-sub` 12px（D2 要 11px），`.hm-month/.hm-day` 9px（全站最小，`3.18:1` 亮色），`.side-label` 10.88px。9px 在任何主题下都不该出现在「信息」位置。 | 热力图标签 10px 起、只保留「1月/4月/7月/10月」四个月份 + 周标签只留「一/四」；`.side-label` 提到 11px/600 并升到 `--muted`。 |
| **F19** | `web/ui.css:2819`（`.empty`）· `:2831`（`.skeleton`）· 全仓 `class="empty` 命中 0 | 契约 §5/D16 造的 `.empty`（icon+标题+说明+动作）和 `.skeleton` **一次都没用**：index 的空态是一行 13px 灰字（`index.html:586`），加载态是「载入中…」文字。组件层造了不用 = 18 页仍会各写一套。 | wave 2 第一页（sources）先落地 `.empty` + `.skeleton` 并截图，再推广；本轮也可以先把 index 的「最近事件」空态换成 `.empty`（有 CTA「接入新源」）。 |
| **F20** | `web/partials.js:1205` + 三态按钮 | 主题按钮 `aria-label`/`title` 都是**状态**「当前主题：深色」，不是动作；只有图标（moon/sun/monitor）。三态循环本身可理解性实测 OK（dark→light→auto→dark，icon 与 `theme-color` 都跟着变），但「再点一下会变成什么」不可预期。 | `aria-label` = 「切换主题（当前：深色，下一档：浅色）」；`title` 同；可选：菜单式三选项（`.menu` 已存在），比循环更省点击。 |
| **F21** | `web-src/layouts/base.html`（无跳过导航） | Tab 顺序第 1 站是语言提示条、第 3 站是品牌链接，到 `#page-main` 要按 17 次 Tab（10 个 side-link + 语言 + 退出 + 顶栏 3 个）。 | 布局首个可聚焦元素加 `.sr-only:focus-visible` 的「跳到主内容」→ `#page-main`。 |
| **F22** | `web/ui.css:59`（`.side-link{transition:all .2s}`） | 焦点环通过 `transition: all` 从 0 → 2px **淡入 200ms**；快按 Tab 时环看起来"追不上"。 | 焦点环不参与过渡：`.side-link{transition: color,background,border-color}`（去掉 `all`），`:focus-visible{transition:none}`。 |
| **F23** | `web/partials.js:1293` 附近的 mock 药丸 + `web/mock.js:3700-3720` | mock 提示条：① 亮色页面里仍是**深底 + 黄边**（`index-light-1440.png` 底部一块黑）；② 390 下固定居中，遮住抽屉的用户块与列表第 4–5 行（`index-dark-390-drawer.png`）。REVIEW §8.4 已挂账。 | 改走 token（`--panel-overlay` + `--warn`），移动端贴底 + 可点击收起，z-index 让位于 `--z-overlay` 之下的抽屉。 |
| **F24** | `web/partials.js:1205`（`.app-topbar-sub` 1290px 以下？） | 顶栏副标题在 1440 显示（契约 §4.2 要求 ≥1280 显示），但实测 390/768 也在 DOM 中（只是被 `page` 断点隐藏？未验证到规则）。 | 明确加 `@media (max-width:1279px){.app-topbar-sub{display:none}}`，避免和 F6 一起造成窄屏挤行。 |
| **F25** | `web-src/pages/login.html:72` | 登录主面仍是 `.glass w-full rounded-3xl p-8`（24px 圆角 + `--surface-2` 底），与全站 `.card`（16px + `--surface-1`）不同源；REVIEW #9 提过。 | wave 2 D 组换 `.card` + `--r-xl`，保留密码/恢复码区在 `--surface-2` 上做内嵌层。 |
| **F26** | `web-src/pages/index.html:184-205` | 热力图图例 6 格里有 4 格与卡底同色（见 F5），变相"两个端点在说话、中间沉默"。 | 与 F5 同修；另加一句 `aria-label`（"颜色越深任务越多"已在文案里，够了）。 |

### 2.4 任务书疑点的真伪核对（逐条）

| 疑点 | 结论 | 依据 |
|---|---|---|
| 首页 KPI 只有 2 张卡、右侧 2 个空位（1440）；390 正常 | **真，且违反契约 §10** | F1；1440 实测 4 列 266px、390 实测 2 列 167px 且填满 |
| 顶栏 subtitle 与 hero subtitle 重复 | **真，比报告更严重**：events 连**标题**都重复；另有 sources/nodes 两页 dupSub=true | F6；`events:99/194`、`sources:408/660`、`nodes:78/293` |
| 折叠侧栏底部语言切换竖排/被裁、logo 只剩红点、用户块溢出 | **真（三件事是三个不同的 bug）** | F7（lang 90×58 溢出 18px + 竖排）、F8（用户块底 929 > 900）、F15（红点在/logo 不在） |
| 深色热力图空格几乎不可见、月份标签密度 | **空格不可见=真**；**密度不是问题**（13 个标签 / 1053px ≈ 80px 间距，按真实月份定位，页脚不挤）；问题是 **9px + 3.97:1** | F5、F18；`index.html:1106` 按周列定位 |
| 首页 sparkline 是柱状色块而非曲线 | **真**（7 个 `<rect>`，非 `path`） | F9；`index.html:355-370` vs `dashboard.html:409-412` |
| 事件流行内重复「查看链路」是否过重 | **真，但"过重"不是主要问题**——主要问题是它是**不可点的 badge**，而整行已经是链接 | F11；`events.html:876`（`<a>`）+ `:896`（badge） |
| light 下未迁移页的对比度与白 alpha 表面 | **真**：capabilities 81 处、nodes 30、docs 44、security 16、keys 15、sources 10；白 alpha 表面**已被 shim 兜住**（不再白上加白），新问题是 shim 把三层压成一层 | F12；对比度扫描见 §6 |
| 键盘可达性（抽屉 Esc / 焦点环 / 跳过导航 / 菜单 Esc / 外点关闭） | **Esc ✗→✓（已实现且正确）**、**焦点环 ✓（22/22 有环）**、**跳过导航 ✗（缺）**、**菜单 Esc+外点 ✓**；另有「焦点环淡入」与「抽屉不动焦点」两个小问题 | §1 可访问性行；F21、F22 |
| 主题切换按钮三态是否可理解（无文字） | **基本可理解（图标区分 + 状态正确循环），但动作不可预期**（label 只述状态） | F20；实测 `dark→light→auto→dark`，`theme-color` 同步 |

---

## 3. 未迁移页逐页 P0 清单（18 页，非 16 页）

> **计数更正**：`web-src/pages/` 共 **20 页**，wave 1 迁移 index + events 两页 → 未迁移 **18 页**，
> 不是任务书/REVIEW.md 里写的 16。login/cli-auth/passkey-enroll 虽然用的是 login/focus 布局（无 `#page-main`），
> 但它们同样需要迁移（只是页头规则按契约 §10 C2 例外），所以计入 wave 2 的 D 组。
> 「改版时必须先解决的 1–3 条」= 优先级排序后的必办项，不含纯打磨。
> **取证标注**：页头/宽度/`.toast` 定义/`.pb-grid`/`max-w-*`/`security` 双列表/`docs` 字号/`receivers` 无页头 均已本轮 grep 实测；
> `message/node/source/trace/playbook/edges/receivers` 的“正文全 JS”“同构空卡”“硬编码色值”等**沿用 REVIEW.md §2 的首轮发现**（本轮只改了行号与优先级，未逐页重测）。

| 页 | 页头/布局基线 | P0-1 | P0-2 | P0-3 |
|---|---|---|---|---|
| `admin.html` | `max-w-7xl`（唯一） | 宽度改 1200（`.page`）与全站对齐 | 3 张表（:97/:134/:163）换 `.table-wrap`+`.data-table`（粘顶表头） | `#users-tbody` 8 列 1024 宽横滚，先定列优先级（次要列 1280 才出现） |
| `capabilities.html` | 无 `.page-title` | `.kind-origin/.kind-pipe/.kind-end` 硬编码 300 级色（亮色 1.92–2.72:1）→ 改 `--info/--success/--accent` | 345 行内联样式（:4-348）拆进 `ui.css` 组件类 | `<a class="card">`（:391）与 div 卡片行为不一（focus/hover/圆角） |
| `cli-auth.html` | **无 `#page-main`** | 2 个 `<h1>`（:85/:164）→ 按 §10 C2 降级为 `.page-title`(h2) 或保留 h1 但补最小页头（需先 grep e2e） | 8 个 state section 预渲染约 620 行 → 只渲染当前态 | `.grid-overlay` 白 alpha 网格在亮色下变白雾（:13/:16） |
| `docs.html` | 首节 h2 `text-3xl` vs 其余 `text-2xl` | **locale 里的 HTML 被转义成文本（14 处）** → 见 F4 | 16 处 `bg-white/*` 代码/表底 → token；`text-zinc-600` 2.45:1 | 8877px 长页缺 `.tabs`/右侧目录（当前有 docs nav？需确认），先定"长页导航"再改视觉 |
| `edges.html` | 有页头 | **`.toast` 用了没定义（:140）→ 裸文本浮层**（功能缺陷） | 弹层挂在 `#page-main` 内（:143）+ 内联 `rgba(0,0,0,.62)` 遮罩 | `.edge-missing` 三处硬编码黄（:78-81） |
| `keys.html` | 有页头 | 表格 8 列表头非粘顶、无 `.table-wrap`、失败态挤在 `<td>` 里 | 弹层在 `#page-main` 内（:183）+ `bg-black/50`（:97/:347） | `{demo}` 占位符在非 demo 路径泄漏成字面量（locale `keys.summary`） |
| `login.html` | login 布局 | **亮色分段控件三层同色（不可见）** → F3 | `.glass rounded-3xl` → `.card`（F25）；`.tab-btn` → `.tabs/.tab` 并保留旧类 | 品牌行红点 + 手写体词标（F15） |
| `message.html` | 3 张同权卡无层级 | 补 `.page-head`（哪张先看） | 字段网格 `grid-cols-2 md:grid-cols-3` 遇不定长字段留洞 → `auto-fit minmax` | `.md-body` 四处硬编码（`ui.css:886-902`） |
| `node.html` | `max-w-3xl` | 正文 100% JS（:173-179），首屏只有 loading 卡 → 先渲染骨架/静态壳 | `.np-st-*` 9 态硬编码（:84-93）→ `--st-*` | toast 借 `.card rounded-xl`（:181）而 `.toast` 无定义 |
| `nodes.html` | 有页头 | 注册面板/结果/错误三重 `hidden`（:100/:155/:138）→ 状态机化（一次只留一个） | `.slot.busy` 硬编码 `#6366f1`（:24）→ `--accent` | `text-zinc-600` 30 处 2.45:1 |
| `passkey-enroll.html` | focus 布局 | **4 个 `<h1>`（:92/:171/:279/:356）+ 2 个 h2** → §10 C2 例外但要收成 1 个 | 6 个 state section 结构重复（:65 起） | 内联 `font-size:1.5rem`（:112）→ `--fs-*` |
| `playbook.html` | `max-w-3xl`? | 首屏 4 张同构空卡（:37/:45/:62/:70）=「标题 + 0」→ 空态给动作 | `<title>` 与 playbooks 同 key（:1） | `.yaml-box` 硬编码（`ui.css:1927`） |
| `playbooks.html` | 有页头 | **`.toast` 无定义（:27）** | `.pb-grid` minmax(320px)（`ui.css:1793`）+ body `overflow-x-hidden` → 窄屏裁切不可滚 | `.pb-card` 14px 圆角 vs `.card` 16px |
| `receivers.html` | **唯一没有页头** | 补 `.page-head`（:7 第一个元素就是 tab 条） | push 关闭时 `replaceChildren` 整页（:989-1000）把 WS 区块一起删掉 | 状态 map 硬编码（:862-865） |
| `security.html` | 有页头 | Passkey 列表无卡（:114）而会话列表有卡（:134）→ 同页两种列表语言 | 硬编码 `#fcd34d`(:150/:153)、`#0a0a11`(:425)、`rgba(251,191,36,.2)`(:145/:555) | **QR 容器必须保持深底**（亮色下不反转，注释写明） |
| `source.html` | `max-w-3xl` | 正文全 JS（:144-148），首屏 loading 卡 | toast `toast card rounded-xl`（:152）；`.param-*` 硬编码（:56/:261/:264） | 与 node/trace 三页壳雷同却各写一套 → 抽成一套「详情页壳」 |
| `sources.html` | 有页头 | **顶栏 subtitle 与 hero 副标题逐字重复（:408/:660）** → F6 | 394 行内联样式（:4-397）+ 弹层内联尺寸/遮罩（:451/:470/:442） | 引用不存在的 `--fg` 6 处无兜底（:70/:136/:215/:293/:340/:356）→ `--text` |
| `trace.html` | `max-w-6xl`（唯一） | 正文全 JS（:83-87），首屏 loading 卡 | 同类详情页宽度不一致（node/source 3xl vs trace 6xl）→ `.page-narrow`/`.page` 二选一并写进契约 | 错误直接写进 `#trace-body`（:232），无 toast/无重试 |

**横向必办（跨所有 18 页，先做一次再逐页）**：
1. `.toast` 页内定义 11 份 → 0 份，全走 `Anotify.toast`（C4 已裁决）；
2. 硬编码语义色 → `--st-*` / `--success|--warn|--error`（4 处 9 态拷贝：`ui.css:1584`、`graphs.js:67`、`node.html:84`、`capabilities.html:150`）；
3. `max-w-3xl|6xl|7xl` → `.page` / `.page-narrow` 两档；
4. 空态统一 `.empty`、加载统一 `.skeleton`（当前 0 引用）。

---

## 4. 与设计稿 `dashboard.html` 的偏差（只列值得修的）

| # | 设计稿 | 实现 | 影响 | 建议 |
|---|---|---|---|---|
| **D-a** | **两列网格**：`热力图 + 最近任务`（`dashboard.html:459/:513`）、`最近事件 + 接入 Agent`（`:600/:655`）并排 | 6 个块**单列全宽**堆叠（index.html:20→1194） | 页面 2190px（设计 1545px）+ KPI 死区；"dashboard 密度"没做出来。这是与设计稿最大的一处偏差。 | wave 1 补一步：`grid-template-columns: minmax(0,1.35fr) minmax(0,1fr)` 两列，热力图/最近任务并排；events/接入 Agent 并排。可省 ~600px 高度。 |
| **D-b** | 4 张 KPI 卡，含 `delta 药丸 + ↑/↓`、`spark path.line+area`、`.stat-foot`、数值带 `.unit` | 2 张（+2 空位）、delta 位置贴「实时」标签、7 根 `rect`、无 foot/unit | 首屏最贵的 4 个格子有一半在浪费；"趋势"这个语义完全丢失 | F1 + F9 |
| **D-c** | `.list-row`（56px，`is-clickable`，hover 出 `chev`）+ `.card-foot`「加载更多」 | index 用 `.notif-row`（可）、events 用每行一卡（不可）、`.card-fill/.card-foot` 0 使用 | 同类列表两种语言；「加载更多」浮在半空 | F10 + 把「加载更多」放进 `.card-foot` |
| **D-d** | 热力图 11px 格 / 5 级图例（空+accent1-4）/ `--hm-empty` | 17px 格（`aspect-ratio:1` + 53 列均分）/ 6 格图例但 4 格不可见 / 空格 `--surface-2` | 空格不可见、图例失效、卡片高度被 17px 格撑到 255px | F5；格宽改回 `--hm-cell:11px` 并把 `.hm` 收进 `.hm-scroll`（宽屏也不需要铺满） |
| **D-e** | 欢迎语 28–34px + 摘要 13px + 动作按钮同一行 | 实现一致 ✅（`page-title` 28px Fraunces） | — | 但见契约矛盾 C-2（`.page-title` 字体归属未定） |
| **D-f** | `.stat-icon` **30×30 / 8px 圆角** | 实测 **36×36 / 12px 圆角**（`.stat-icon` 走的是页面工具的 `h-9 w-9 rounded-xl` 而不是 D7 的 token） | 图标比规格大 20%，4 卡并排时视觉重心偏向图标而非数值 | 改用 30×30 + `--r-sm`，并把尺寸写进 `.stat-icon` 组件类（别让页面工具类覆盖） |
| **D-g** | 「接入你的 Agent」卡在第二列（与最近事件并排），且展开区有 3 步代码 | 全宽独占一行、默认折叠 | 占位大而信息少 | 随 D-a 一并改 |
| **D-h** | 无 mock 提示条、无语言提示条 | 两者都在首屏（前者遮内容，后者顶 45px 并引发 F8） | 首屏观感 | F23 + F8 |

---

## 5. 下两轮验收建议（机械可检查）

### Round 3（wave 2 前半：A/B 组 = sources/source/capabilities/edges/nodes/node/keys/admin/receivers）

1. **KPI 空位 = 0**：1440 下 `.stat` 可见数 == 网格列数，且 `grid-template-columns` 的列数 == 可见卡数（`design/ui-v2/shots-review2/kpi-*.json` 同款断言）。
2. **每页 `dupSub=false 且 dupTitle=false`**：`.app-topbar-sub` 文本 ≠ 该页任一 `.page-sub` 文本；`header h1` 文本 ≠ `.page-title` 文本。
3. **light 对比度 <3.0 的文本节点 = 0，<4.5 的 ≤5**（除 9–11px 的纯装饰标签，需白名单注释）。扫描器已在 `shoot.mjs` 的 `contrast-*` 分支里（含 alpha 叠加与 effective bg）。
4. **每页 `text-white|bg-white/|border-white/` 命中数 = 0**（shim 只能收缩，清零后才允许删 shim；`grep -c` 即可）。
5. **组件落地**：每个迁移页至少用到 `.page-head/.page-title/.card/.btn` 四件，且 `.empty` 或 `.skeleton` 至少被一页真实使用（`grep -c 'class="empty'` ≥1）。

### Round 4（wave 2 后半：C/D 组 + 收尾）

1. **`.toast` 页内定义数 = 0**：`grep -rE '\.toast\s*\{' web-src/pages/` 命中 0；且 `edges/node/playbook/playbooks` 四页不再出现裸文本浮层（断网点击一次抓 `getComputedStyle` 的 `position==='fixed'`）。
2. **`[object Object]` = 0**：断网脚本（`route abort **/v1/**`）跑 20 页，断言 `innerText` 不含 `[object Object]`、且每页至少出现 1 次「载入失败」+ 1 个可点重试按钮；KPI 值不得为 `0`（应为 `–`）。
3. **每页 `#page-main` 内 `h1` 计数 = 0**（login/cli-auth/passkey-enroll 的 `h1` 计数 ≤1）。
4. **1440×900 首屏有效内容 ≥3 个信息块**：`scrollHeight ≤ 1800`（index，含 D-a 两列改造后应 ≈1550）；`docs ≤ 4500`（分节折叠/锚点导航后）。
5. **hardcoded-hex 归零**：`grep -rE '#[0-9a-fA-F]{6}' web-src/pages/` 只允许出现在 `web/tokens.css` 与明确注释的「QR 深底」例外处（现网 40 处 → 目标 ≤2）。

---

## 6. 契约矛盾 / 事实更正（不改契约，交协调者裁决）

| # | 矛盾点 | 事实 | 建议裁决 |
|---|---|---|---|
| **C-1** | §4.1「折叠态 hover 显示 tooltip」+ D3「`.side-nav` 折叠态必须 `overflow:visible`」 **vs** 「footer 语言/用户块始终可见」 | 视口高 ≤700px 时，`overflow:visible` 让最后一个 `.side-link`(b=653) 与 footer(top=633) **重叠 20px**（实测 1440×700）。两个要求在中低视口不可同时满足。 | 采纳 D3 的预留方案：`@media (max-height:760px){.app-nav{overflow-y:auto}}`，此时折叠 tooltip 退化为 `title`（D3 已写明这个代价）。请确认「tooltip 在短视口降级」可接受。 |
| **C-2** | §3/§10「Fraunces 只用于 ≥22px 的三类位置（登录大标题、欢迎语、章节主标题）；`.page-title` 是 28px **Inter 600**」 | 实现 `.page-title` = **Fraunces 28px**（`ui.css:2301` 起 + 所有迁移页 `font-display`）。events 的「事件流」属于「章节主标题」还是「页面标题」，两处条文给出相反答案（D2 原话也是「Fraunces 或 Inter，见 §3」）。 | 请明确：**工作台页的 `.page-title` 用 Inter 600**（与顶栏 h1 同族，只有欢迎语/登录用 Fraunces），否则 18 页会各判一次。 |
| **C-3** | §0-5「20 个页面统一页头/卡片/列表/表格/空态语言」 vs §10 C2（focus/login 布局允许无页头） | login/cli-auth/passkey-enroll **没有** `#page-main`、不走 `mountLayout`，实测 h1 数 0/2/4。 | 把 §0-5 的口径改成「**17 个工作台页**统一页头；3 个 focus 页另有最小页头规范（品牌行 + 单 h1）」。 |
| **C-4** | 任务书与本轮计数：未迁移 **16 页** | `web-src/pages/` = 20 页，迁移 2 页 → **18 页**。REVIEW.md §2 的「20 页清单」与契约 §6 的 wave 2 分组（A 4 + B 5 + C 5 + D 4 = 18）其实自洽，只是「16」这个数在任务书里写错了。 | wave 2 按 18 页排期（A/B/C/D 四组 = 18）。 |
| **C-5** | §2「`header h1` 全站唯一，含 `.scope-badge`」 | 实测 keys/security 页 `header h1` 的 `textContent` 是「API Keys全部署通用」——`.scope-badge` 在 h1 **内部**，导致 e2e/脚本取 h1 文本时得到粘连串（本轮 dupTitle 断言差点误判）。 | 保留 h1 唯一性，但请裁决 `.scope-badge` 是否应移出 h1（或给 e2e 一个 `h1 > span:not(.scope-badge)` 取值约定）。 |

---

## 7. 截图索引（`design/ui-v2/shots-review2/`，自采 60+ 张）

| 文件 | 看什么 |
|---|---|
| `index-dark-1440.png` / `index-light-1440.png` | 首屏：问候 / **KPI 死区(F1)** / 最近事件 / 热力图首行 |
| `index-dark-1440-full.jpg`（2190px）/ `-light-` | 整页六块单列堆叠（D-a）、15 行 `.notif-row`、页脚 |
| `index-dark-1440-collapsed.png` / `-light-` / `-collapsed-tooltip.png` | 76px 导轨：**语言竖排溢出(F7)**、用户块被裁(F8)、tooltip 正常 |
| `index-dark-390-first.png` / `-light-` / `-dark-390-drawer.png` | 移动端两列 KPI、抽屉 + overlay、**mock 药丸遮内容(F23)** |
| `events-dark-1440.png` / `-light-` | **标题+副标题双份重复(F6)**、每行一卡(F10)、每行 badge(F11) |
| `events-dark-1440-bottom.png` | 9 行到底 + 无分页 |
| `errnet-events-dark-1440.png` / `errnet-index-*-1440.png` | **`载入失败：[object Object]`(F2)** / 首页错误态自相矛盾(F14) |
| `sources-{dark,light}-1440.png` | 未迁移页在亮色下的整体观感（其实相当好）+ 副标题重复 |
| `capabilities-light-1440.png` | **kind-badge 1.92–2.72:1(F12)**、out-chip 2.92 |
| `keys-dark-1440.png` / `err-keys-*-1440.png` | 表格形态、`{demo}` 泄漏、失败态挤在 td |
| `security-{dark,light}-1440.png` | 同页两种列表语言、硬编码黄 |
| `docs-light-1440.png` / `-dark-` | **locale HTML 被转义(F4)**、代码块亮色可读 |
| `login-dark-1440.png` / `login-light-1440.png` / `login-dark-390.png` | **亮色分段控件消失(F3)**、品牌红点(F15)、网格背景 |
| `focus2-dark-1440-sidelink.png` / `-themetoggle.png` / `-btnprimary.png` | 焦点环证据（22/22 有 2px accent 环） |
| `hover-dark-1440-sidelink.png` / `-stat.png` / `-themetoggle.png` | hover 态 |
| `zoom-heatmap-dark.png` | 热力图空格与图例的近距离取证 |
| `admin-light-1440.png` / `cli-auth-light-1440.png` / `passkey-enroll-light-1440.png` / `nodes-light-1440.png` | 未迁移页亮色基线 |
| `shoot.mjs` | 采集脚本（可复跑：`node design/ui-v2/shots-review2/shoot.mjs`） |

---

## 8. 最高优先级建议（3 条）

1. **先补 P0 三件：KPI 空位（F1）/ events 错误文案（F2）/ login 亮色分段控件（F3）**，并在 `ui.css` 的 light shim 旁写死一条规则：
   *「shim 只能把 N 种语义映射到 N 个 token，禁止一律 `--surface-2`」* —— 这三条里两条同源，是同一次设计决策失误。
2. **首页按设计稿恢复两列网格（D-a）**：热力图∥最近任务、最近事件∥接入 Agent。这一步能把 2190 → ≈1550px，同时消掉 KPI 死区带来的"空场感"，比任何视觉微调都值钱。
3. **在开 wave 2 之前先做一页「样板页」（建议 `sources.html`）**，把 `.empty`/`.skeleton`/`.table-*`/`.toast` 一次性落地并截图，作为 18 页的复制基线 —— 当前契约 §5 有 10 个类 0 使用，18 个 worker 并行时必然各自发明一套（R4 复因）。

---

## 9. 自检

```bash
node design/ui-v2/shots-review2/shoot.mjs        # 9 页 × 2 主题 + 折叠/抽屉/错误/焦点/hover + 对比度扫描
node /tmp/batch3.mjs                             # 逐页 dupTitle/dupSub/langHint 断言
node /tmp/err3.mjs                               # route abort **/v1/** → 错误态取证
node /tmp/batch2.mjs                             # 主题三态循环 / 菜单外点 / 侧栏四个视口高度 / reduced-motion
```

未改任何产品文件（`git status` 仅新增 `design/ui-v2/REVIEW-2.md` 与 `design/ui-v2/shots-review2/{shoot.mjs,README.md}`，截图按契约 §10「不入库」留在工作区）；
未重启 7788；未在 `feat/ui-v2` 上提交（本轮只提交到子分支 `pi-subagent/ui-critic-2-e2ee56b7`，内容提交 `aaa239e`，HEAD 以分支最新为准）。

```json
{"ok": false, "reason": "wave 1 未达产品级：P0 F1 KPI 空网格位（1440 下第 3/4 列整列空，直接违反契约 §10「不允许空网格位」裁决）、P0 F2 events 断网时列表渲染'载入失败：[object Object]'（events.html:709 传 {status} 给 loadErrorNode）、P0 F3 亮色下 login「登录/注册」分段控件消失（.glass / bg-white/[0.04] / .tab-btn.active 三层被 shim 压成同一个 --surface-2），另有 F4 docs 页 14 处 locale HTML 被转义成裸文本、F5 热力图空格 1.12:1 且 --hm-empty 从未定义（D17 明文要求）。方向正确：首屏 2190px ≤ 契约 2600、22/22 tab stop 有 2px focus ring、抽屉 Esc+焦点归还 / 菜单 Esc+外点均正确、9 页横向溢出 0、reduced-motion 生效。建议先做 ≤1 天 P0 修复 + 首页恢复设计稿两列网格（2190→≈1550）再开 wave 2；未迁移页实为 18 页（20−2，非 16）。交付：design/ui-v2/REVIEW-2.md + shots-review2/（60+ 截图留工作区 + 可复跑 shoot.mjs）。branch pi-subagent/ui-critic-2-e2ee56b7（内容提交 aaa239e，HEAD 见本轮 verdict）；remote=origin(github.com:zzjcool/anotify.git) 存在但未 push、未开 MR（任务书要求不 commit 到产品分支，分支本身即交付物）。"}
```

**3 条最高优先级建议（复述）**：① 补 F1/F2/F3 三个 P0，并把 shim 的「一律 `--surface-2`」立为禁止项；② 首页恢复设计稿的两列网格，先降高度再做视觉微调；③ 开 wave 2 前先做一页样板页（sources），把 `.empty/.skeleton/.table-*/.toast` 落地成复制基线。

---

### 备注：本轮未能验证的两件事

1. **热力图 hover tooltip** 在深色卡上的可读性只做了 CSS 取值核对（`--panel-overlay` + `--line-strong`），没有逐格截到 tooltip 的像素图 —— 元素定位是 `position:fixed` + `translate(-50%,calc(-100% - 8px))`，贴近视口顶部时可能被裁（未实测到该位置）。
2. **`prefers-color-scheme` 首帧无闪** 只验证了 `data-theme` 在首帧前落盘（内联脚本在 `<link rel=stylesheet>` 之前），没有用视频帧对比验证「无闪白」——需要真实浏览器录屏。
