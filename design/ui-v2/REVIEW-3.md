# Anotify UI v2 · 第三轮评审（最终验收）

> 事实源：`.pi-orchestrator/UI-V2-CONTRACT.md`（v1.2 + §10/§11/§12 裁决）· `design/ui-v2/REVIEW.md`（D1–D20）· `design/ui-v2/REVIEW-2.md`（wave 1，3.2 分）。
> 只读评审：`web/`、`web-src/`、`scripts/`、`internal/`、`design/ui-v2/{REVIEW,REVIEW-2,gallery,dashboard,login}.html` 均未改动。
> 全部数据由 `design/ui-v2/shots-review3/shoot.mjs` 自采（playwright-core 1.62 + chromium-1208，preview = 主 checkout 的 `127.0.0.1:7788` fe-preview + `web/mock.js`）：
> **20 页 × 2 主题 × 1440×900**（40 张）+ 移动 390（6 页 ×2 主题 + 抽屉 12 张）+ 折叠侧栏 2 + 键盘焦点 2 页 ×26 tab stop + 断网错误态 11 页 + hover 2 + 慢网首屏 3。
> 原始数值：`shots-review3/measure.json`（每页 6KB 结构体）、`summary.json`。截图 `shots-review3/*.png`。
> 行号指当前实现（`web-src/pages/*`、`web/ui.css`、`web/tokens.css`、`web-src/layouts/*`）。

---

## 0. 一页结论

**wave 2 + wave 3 是成功的。这一版可以叫产品级。**

REVIEW-2 的 3 条 P0 全部真修好（逐一实测，不是看 diff）：KPI 4 格被真实数据填满（36/36/2/5，`index-dark-1440.png`）、
断网时事件流显示「载入失败：连不上服务端，请重试」且页面内 0 处 `[object Object]`（`err2-events-dark.png`）、
亮色登录分段控件恢复三级层次（`login-light-1440.png`：容器 `--surface-3` / active `--surface-1`+阴影）。
REVIEW-2 列的 P1 也基本清完：docs 裸标签消失（「Anotify 的核心场景：Agent 完成任务后…」已是正常文本）、
热力图空格在深浅两色下都可辨、折叠侧栏 76px 且标签全隐（`visibleLabelCount=0`、`overflowX=0`）、
跳过导航已存在（首个 tab stop 前有「跳到主内容」）。

**机械判据全绿**：20 页 × 2 主题 —— `header h1` 唯一 20/20、内容区 h1 = 0（三个 focus 页 = 1，符合 C-2/C-3）、
`#page-main` 唯一 17/17、`dupSub=false` 20/20、横向溢出 0（40/40）、
**对比度不达标 0 处**（正文 0/226、辅助 0/993，深浅各扫一遍；详见 §4.3）、焦点环 26/26、`overflowX` 移动端 12/12 为 0。

**离「精细产品」还差 4 个 P1**（无 P0）：
① 旗舰首屏第 2 张 KPI 卡把**药丸组件当说明文字**用，渲染成 224×22 的空描边盒（像坏掉的输入框）；
② 语言建议横幅把侧栏顶下去 45px，**侧栏 footer 被裁 44px**（`demo` 用户块只剩半行）；
③ 移动抽屉**仍无可见关闭按钮**，焦点也不进面板（12/12 实测）；
④ `admin` 的「无权访问」态**整页绕开共享语言**（自造 `.ad-forbidden*`，无 `.page-head`，不用 `.empty`）。

**结论：`ok: true`**（无 P0）。上面 4 条是 P1，建议合并前修 ① ② ④（都是小改），③ 可进下一轮。

---

## 1. 最终评分（1–5）

| 维度 | 分 | 证据（一句话） | vs REVIEW-2 |
|---|---|---|---|
| **层次** | **4.5** | 首页 1630px（契约 ≤2600 ✅ / wave1 2190 / 设计稿 1545）：问候 28px → 4 张 `.stat`（30px/600 数值 + 12.5px 标签 + delta 药丸 + spark）→ 热力图 → 最近事件 → 最近任务，四级层次在 900px 首屏内成立（`index-dark-1440.png`）。扣分只在 KPI 第 2 卡的胶囊错用。 | 4 → 4.5 |
| **密度** | **4** | 首页 1630px 承载 4 KPI + ≈376 格热力图 + 5 事件 + 10 任务；节点页 3 行 `.list` 每行塞下 名字/状态/标签/5 个能力 chip/占位条/心跳/两个动作（`nodes-dark-1440.png`），是标准的「产品级紧凑」。扣分：events 每行 84px（`.card` 承载，契约锁定），docs 4488px 单页。 | 3 → 4 |
| **一致性** | **4** | 页头宽度 17/17 = 1112px（`.page`）、`.page-narrow` 3/3 = 760px（source/node/message）、半径 12/16、`.data-table` 真被用（admin/docs/keys 6 张表 + `.table-wrap`/`.table-foot`）。扣分：列表 3 套形态并存、admin 自造空态、`.st-text` 与 `.toast` 有未落地/副本残渣（§5）。 | 3 → 4 |
| **亮色** | **5** | **0 处不达标**（40 页次全量扫描，见 §4.3）：正文最小 5.02:1（light）/ 6.78:1（dark），辅助最小 3.16:1（light，docs 的 12px 徽章，阈值 3.0）/ 3.80:1（dark）。QR 深底按契约 §12 保留 `--qr-bg/--qr-fg` 兜底（`security.html:510-511`），浅色下不反转。 | 2 → 5 |
| **可访问性** | **4** | 26/26 tab stop 均有 2px accent 焦点环（`focus-index`/`focus-events`，noRing=0）；抽屉 Esc 关闭 + 焦点归还 `#menu-btn`（12/12）；跳过导航存在；`aria-expanded/controls` 保留。扣分：抽屉无关闭按钮且焦点不进入；移动端无焦点陷阱。 | 3 → 4 |
| **移动端** | **3.5** | 390 宽 12/12 `overflowX=0`、抽屉 264px 不挤压内容、`.side-link` 无裁切（clipped=[]）、点击/遮罩/Esc 三路关闭齐备。扣分项同③。 | 4 → 3.5 |
| **性能预算** | **4** | 本地 load 383–505ms、DCL 267–355ms、10–11 请求、JS 288–323KB、CSS 111KB、DOM 482–988 节点（index/docs/edges 实测），无横向重排、无构建框架。扣分：**字体走 Google Fonts CDN**（3 layout × preconnect + css2 URL），内网/离线部署首屏会掉到 system-ui。 | 未评 → 4 |

**加权 4.1 / 5**（REVIEW-2 为 3.2）。判据是「能不能给客户看」：能。剩下的 4 条 P1 是**局部**问题而不是**语言**问题——
它们不会像 wave 1 的 KPI 死区那样被复制 18 遍，因为组件层（`.card/.list/.data-table/.empty/.stat/.page`）已经在 17 个页面上真正跑起来了。

---

## 2. P0 / P1

**P0：无。** 

### P1（4 条，均已实测取证）

| # | 路径:行号 / 截图 | 证据（可复现的数字） | 修法 |
|---|---|---|---|
| **P1-1** | `web-src/pages/index.html:132` · `web/ui.css` `.stat-delta` · `index-dark-1440.png` 坐标 x=599,y=364 | 第 2 张 KPI（今日任务）用 `<div class="stat-delta">{{t "index.kpi.today_sub"}}</div>` 承载说明「上报 → 完成」。`.stat-delta` 是**药丸**（`index.html:839` 注释自己写着「是药丸，不是描述词」），实测渲染为 **224×22px 全宽描边胶囊**（`bg #15151f / border rgba(255,255,255,.07)`，深色；`#f4f5fa / rgba(17,19,28,.08)` 亮色），而兄弟三卡同位置是 12.5px 无底说明文字（「近 30 天」「近 50 条任务」）。首屏第一行就出现一个没有人会认为是设计的东西——读起来像一个空输入框。 | 换成普通说明 span（`class="stat-caption"` 或直接 `<div class="text-faint text-xs">`），把漏斗 `上报 → 完成` 放到 spark 上方作 caption；不要再复用 `.stat-delta`。 |
| **P1-2** | `web/partials.js` 侧栏高度 · `index-dark-1440.png` / `index-light-1440.png` | 语言建议横幅占 45px 后，`#sidebar` 仍是 `100vh`：实测 rect `y=45, h=899, bottom=944`（viewport 900），**溢出 44px**；`#lang-switcher` 落在 y=841–879，其下的用户块被切掉，截图里 `demo` 只剩半行字（y≈897）。无横幅的页面（events/nodes/keys）footer 正常（y=836–884），说明是「横幅占流 + 侧栏按 100vh」的交互，不是全局 bug。 | 侧栏高度改 `height: calc(100vh - var(--banner-h, 0px))`（横幅出现时写 `--banner-h:45px`），或让横幅 `position:sticky` 不占 sidebar 的视口高度；契约 C-1「footer 始终可见」的同一精神。 |
| **P1-3** | `web/partials.js` 抽屉（`#sidebar`/`#sidebar-overlay`/`#menu-btn`） · `m390-*-drawer.png`（12 张） | 390 宽 6 页 × 2 主题实测：`closeBtn=false` **12/12**（`#sidebar` 内无任何 `[id*=close]/.drawer-close`），打开后 `document.activeElement` 仍是 `body`/`#menu-btn`（焦点未进入面板），且无 `role=dialog`/焦点陷阱。关闭路径只有 overlay 点击与 Esc（Esc 后 `sidebarHidden=true, focus=menu-btn` ✅）。触屏用户唯一的关闭手段是点遮罩，没有可见的「✕」。 | 抽屉头部加可见关闭按钮（`#sidebar-close`，`aria-label` 走 i18n），打开时 focus 第一个链接/关闭键，加 `role="dialog" aria-modal="true"`，关闭后回 `#menu-btn`（已有）。REVIEW-2 已提过，本轮仍在。 |
| **P1-4** | `web-src/pages/admin.html:152-173`（`.ad-forbidden*`）· `:1126`（`.page` 在 admin 分支内） · `admin-dark-1440.png` | mock 用户非管理员，20 页里唯一一个**可见的非迁移状态**：`/admin.html` 实测 `.page` 容器 class 为空、无 `.page-head`/`.page-title`/`.page-sub`，无 `.empty`，标题是自造 `.ad-forbidden-title`（20px/600）+ `.ad-forbidden-body`（13px）+ `.ad-forbidden-cta`。视觉上它是全站唯一一页「没有页头大标题」的工作台页——恰好是权限错配时用户最需要「我在哪、怎么回去」的那一页。 | 把 `.ad-forbidden` 换成共享 `.page-head`（`.page-title`「无权访问管理后台」）+ `.empty`（icon + 说明 + `返回总览` 作 `.btn`）。删掉页级 `.ad-forbidden*` 四条规则（约 22 行）。 |

### P2（5 条，上限用满）

| # | 路径:行号 | 证据 | 建议 |
|---|---|---|---|
| P2-1 | `web-src/pages/index.html:753` | 唯一残留的 `hover:bg-white/5`；`ui.css:3228` 的亮色 shim **明确不含 `hover:` 变体**（注释：「hover:/focus: 变体不在此列」）→ 亮色下「最近事件/最近任务」行 hover 无任何反馈（白色 5% 叠白底）。 | 改成 `hover:bg-[var(--surface-2)]` 或页级 `tr:hover`/`.list-row:hover`（ui.css 已有）。 |
| P2-2 | `web-src/pages/index.html:861` | `text-zinc-500`（热力图图例的 `%`），靠 shim 才在亮色下可读；`grep text-zinc-\|bg-white/` 在 17 页里**只剩 2 处真残留**（本条 + P2-1，其余 7 处命中都在注释里）。 | 改 `var(--muted)`，然后下轮把 shim 中该选择器删掉。 |
| P2-3 | `web-src/pages/docs.html`（全页）· `docs-*-1440-full.jpg` | 4488px 单页（20 页最长，是第 2 名 edges 2163 的 2 倍）；字号分布 `10.88px ×113`、`11.5px ×98`、`11px ×20` → 113 处 10.88px 正文级文字，长文档阅读疲劳。 | 目录 sticky 化 + 端点表折叠（`<details>`）是超出本轮的小改；至少把 10.88px 提到 12px（`.code-body` 已可 12px）。 |
| P2-4 | `web-src/pages/login.html`（品牌卡）· `login-light-1440.png` | 左侧品牌卡 3 条卖点结束于 y≈485，卡片底 y≈760，`无第三方账号` 贴在 y≈725 —— **中间 220px 纯空**（占卡片高 36%），是本轮唯一一处「留白大到像没做完」的地方。 | 加一组数据（如「已保护告警 N 条 / 设备 M 台」）或把 3 条卖点垂直居中（`justify-content:center`）。 |
| P2-5 | 慢网首屏 `slow-{index,events,keys}-dark.png` · 12 页模板里的 `.skeleton` | 把 `window.fetch` 延迟 1.5s 后实测可见 `.skeleton` = **0**（index/events/keys 三页）：index 用「正在加载你的任务统计…」+ `–` 占位（可接受），events/keys 在数据回来前是**空壳**（keys 只剩表头）。模板里 `skeleton` 出现 12 次但都不在首屏渲染路径上（多为 JS 模板串，且被 mock 的 0 延迟掩盖）。 | 至少给 keys 表体、events 列表、index KPI 值加真 skeleton（`.skeleton` 类已在 `ui.css:2946` 有 shimmer 动画，只是没人用）。 |

---

## 3. 跨页一致性回归（「同一组件两种实现」）

实测口径：主列表容器类型 + 空态实现 + 加载态 + 页头宽度。

| 页 | 主列表实现 | 行高形态 | 空态 | 加载态 | 容器 |
|---|---|---|---|---|---|
| index | `.notif-list`（10 行 `div`） | 57px 紧凑行 | 无（错误态走文案） | 文案占位 | `.page` 1112 |
| events | 每行一张 `a.card` ×9（**契约 §2 锁定**） | 84px | `.empty`=0，错误态文案 | 无 | `.page` 1112 |
| sources | `.list` + 4 `.list-row` | 紧凑行 | 0（mock 有数据） | inline「载入中」 | `.page` 1112 |
| capabilities | `.list` + 10 `.list-row` | 紧凑行 | 0 | inline | `.page` 1112 |
| nodes | `.list` + 3 `.list-row` | 紧凑行（chip 行） | 0 | inline | `.page` 1112 |
| edges | `.list` 11 行 + 13 `.card`（混合） | 中 | 0 | inline | `.page` 1112 |
| playbooks | **卡片网格**（4 `.card`） | 大卡 | 0 | inline | `.page` 1112 |
| node / source / message | `.list`（`#list` 4/15 行） | 紧凑行 | message 有 `.empty` | message 有 skeleton（首屏不显示） | `.page-narrow` 760 |
| keys | `.data-table` + `.table-wrap` + `.table-foot` | 表格 40px 行 | `.empty`=1 | 模板 skeleton | `.page` 1112 |
| security | `.list` + `.tabs/.tab` | 紧凑行 | `.empty`=1 | 模板 skeleton | `.page` 1112 |
| receivers | `.list` + `.empty`×2 | 紧凑行 | `.empty`×2 ✅ | 模板 skeleton | `.page` 1112 |
| docs | `.data-table`×2 + `.card`×24 | 文档流 | — | — | `.page` 1112 |
| trace | `.empty`=1 | 空态 | ✅ | 模板 skeleton | `.page` 1112 |
| admin | **`div.card` + 自造 `.ad-forbidden*`** | — | ✗（不用 `.empty`） | 模板 skeleton | **无 `.page`** |
| login / cli-auth / passkey-enroll | 单卡 400–408px | — | — | — | focus 布局（无 `#page-main`，合 C-2） |

**结论（3 条，按值得修的顺序）**

1. **admin 是全站唯一的例外**（P1-4）：它没有 `.page/.page-head/.page-title/.empty`，自造 4 条页级规则。这是唯一「组件没用上」而不是「组件选得不同」的页。
2. **列表 3 套形态并存但不都是缺陷**：events 一行一卡被 e2e 锁定（`a.card` 的选择器）、index `.notif-row` 被契约 §5 保留（56px 目标，实测 57px 正中）——这两条属于「刻意」。真正不一致的是 **playbooks 用大卡网格、nodes/sources/capabilities 用 `.list` 紧凑行**：两者都是「一组同类对象」，4 个预案用大卡而 3 个节点用行，切换页面时密度跳变明显。建议 playbooks 改 `.list`+2 列 grid（保留卡内信息量）。**不建议**动 events/index。
3. **空态与加载态的落地率仍低**：`.empty` 仅 5 页用（keys/security/receivers/trace/message），`.skeleton` 首屏实测 0（P2-5）；其余页面的空数据分支是「空白区域」而不是「空态」。错误态反而做得很好：11 页断网实测里 6 页给出统一句式的「载入失败：… 重试」（index/events/sources/nodes/playbooks/edges），文案与 `.btn` 一致。

字号/圆角：主字号分布 `12px/12.5px/11.2px/13px/14px` 为主（index 62×12px、capabilities 35×12px、node 65×11px），与契约「正文 14 / 辅助 12–13」一致；圆角分布集中在 `16px`（卡）、`8px`（控件）、`999px`（药丸）、`6px`（chip），未见第三种卡片圆角。**唯一越界**是 docs 的 `10.88px ×113`（P2-3）。

---

## 4. 机械判据（逐页实测，1440×900 与 390×844）

### 4.1 结构锚点（20 页 × 2 主题 = 40 页次，全绿）

| 判据 | 结果 |
|---|---|
| `header h1` 唯一 | **17/17 工作台页 = 1**；3 个 focus 页无 `header`（`h1Count=1`，符合 C-2/C-3） |
| `.scope-badge` | 只在 `scopeLevel==="deployment"` 的 3 页出现（keys/admin/security）✅；其余 14 页无 badge 属正确条件渲染，未误挂 |
| 内容区 `h1` = 0 | **17/17 = 0**；focus 3 页 = 1（C-3 允许） |
| `#page-main` 唯一 | **17/17 = 1**；focus 3 页 = 0（无 mountLayout） |
| `dupSub`（顶栏副标题 ≠ `.page-sub`） | **false 40/40**（含 focus 页无 `.page-sub` 的情形） |
| 横向溢出 | **0，40/40**；移动端 390：**0，12/12** |
| `header h1` 字号一致 | 15px/600 全站（抽查 index/events/nodes/keys/docs 均同） |
| `.page-title` | `28px / 600 / Inter` **36/36**（所有渲染了 `.page-title` 的页次，且 tag=`H2`）；focus 页 `H1` 同样 28px Inter |
| 首屏总高 | index 1630 / events 1196 / sources 900 / capabilities 1394 / edges 2163 / playbooks 900 / nodes 1459 / keys 900 / security 934 / docs 4488 / trace 900 / receivers 900 / message 900 / source 1349 / node 1863 / playbook 1282 |

> `admin` 是唯一 `.page-title` 缺失的页（P1-4，权限态分支）；`login` 用品牌 h1（36px Fraunces，契约允许），`cli-auth/passkey-enroll` 的 `.page-title` 是 `h1`。

### 4.2 硬编码色残留（`web-src/pages/*.html`）

`grep -nE '#[0-9a-fA-F]{3,8}\b|rgba\(255,\s*255,\s*255'` 全量 10 个文件命中 25 处，**逐条判定后真实色值只剩 2 处（1 组）**：

| 文件:行 | 内容 | 判定 |
|---|---|---|
| `security.html:510-511` | `background: var(--qr-bg, #0a0a11); color: var(--qr-fg, #e6e6f0)` | **允许**：契约 §12「QR 深底例外需注释」，且 `:505-509` 注释写明缘由与退休条件（tokens.css 补 `--qr-bg/--qr-fg` 后自动退休） |
| `sources.html:12` / `edges.html:75` / `admin.html:12` / `security.html:49` / `source.html:48` / `sources.html:54` / `index.html:1115` | 命中在注释里（「不再出现 rgba(255,255,255,\*)」「旧实现把 "#fbbf24" 写死三处」） | 非色值 |
| 其余 `#110/#198/#235/#273/#274/#277/#281/#283` | 注释中的 issue/行引用 | 非色值 |

**结论：`rgba(255,255,255,\*)` 归零，hex 只剩 1 组带注释的 QR 例外。** 契约 §0.4「颜色只来自 tokens」达标。

Tailwind 色板类残留（shim 的真实服务对象）只剩 **2 处真引用**：`index.html:753`（`hover:bg-white/5`，P2-1）、`index.html:861`（`text-zinc-500`，P2-2）；其余 7 处命中均为注释。

### 4.3 对比度（dark/light 各 20 页，正文阈值 4.5 / 辅助 3.0）

方法：遍历 `main` 内全部可见文本节点，向上合成有效背景（含 alpha），大字号按 3.0 计，`<13px` 或 alpha 色/`.page-sub` 归入辅助（3.0）。

| 页 | dark 正文 不达标/总数 | dark 辅助 | light 正文 | light 辅助 |
|---|---|---|---|---|
| index | 0/18 | 0/101 | 0/18 | 0/101 |
| events | 0/12 | 0/34 | 0/12 | 0/34 |
| sources | 0/7 | 0/36 | 0/7 | 0/36 |
| source | 0/6 | 0/56 | 0/6 | 0/56 |
| capabilities | 0/15 | 0/99 | 0/15 | 0/99 |
| edges | 0/14 | 0/100 | 0/14 | 0/100 |
| playbooks | 0/6 | 0/25 | 0/6 | 0/25 |
| playbook | 0/19 | 0/20 | 0/19 | 0/20 |
| nodes | 0/9 | 0/51 | 0/9 | 0/51 |
| node | 0/30 | 0/89 | 0/30 | 0/89 |
| keys | 0/8 | 0/32 | 0/8 | 0/32 |
| admin | 0/3 | 0/0 | 0/3 | 0/0 |
| security | 0/13 | 0/17 | 0/13 | 0/17 |
| docs | 0/43 | 0/307 | 0/43 | 0/307 |
| trace | 0/4 | 0/8 | 0/4 | 0/8 |
| receivers | 0/4 | 0/2 | 0/4 | 0/2 |
| message | 0/2 | 0/3 | 0/2 | 0/3 |
| login | 0/9 | 0/4 | 0/9 | 0/4 |
| cli-auth | 0/2 | 0/5 | 0/2 | 0/5 |
| passkey-enroll | 0/2 | 0/4 | 0/2 | 0/4 |
| **合计** | **0/226** | **0/993** | **0/226** | **0/993** |

最低实测值（独立复核，避免自检脚本自证）：docs light `rgb(5,150,105)@12px = 3.16`、`@11px = 3.34`，capabilities light `rgb(161,98,7)@11.2px = 4.30`、`rgb(107,112,128)@10.88 = 4.53`，docs dark `rgb(110,114,132)@11px = 3.80`。全部落在各自阈值之上（辅助 3.0 / 正文 4.5），**不是勉强踩线**：正文最小 **5.02（light）/ 6.78（dark）**，辅助最小 **3.16（light）/ 3.80（dark）**。

### 4.4 空态 / 错误态 / 加载态

| 场景 | 实测 | 判定 |
|---|---|---|
| 空列表 | `.empty` 出现在 keys(1)/security(1)/receivers(2)/trace(1)/message(1)；sources/capabilities/nodes/edges/playbooks 因 mock 有数据未触发 | 有 `.empty` 的 5 页**都带动作**（「+ 本机添加」「返回事件流」等）；未触发的 5 页空态不可证 → 不下结论 |
| 断网（mock 500，11 页） | index「暂时取不到任务数据，请重试」+3 个重试；events/sources/nodes/playbooks/edges「载入失败：服务端错误 HTTP 500」+ 重试按钮；**0 处 `[object Object]`、0 处 `undefined`**（`/\[object\|\bundefined\b/` 全 11 页 false） | ✅ REVIEW-2 F2 已修；句式统一但要留意 events 同时出现「连不上服务端，请重试」与「服务端错误 HTTP 500」两种措辞（同一页两个接口） |
| keys 错误态 | mock 无「刷新」按钮，reload 后注入失效 → **未取到可靠样本（不下结论）** | 未验证 |
| 慢网首屏 | index/events/keys 可见 `.skeleton` = 0（P2-5） | ✗ |
| 键盘 | index 26 stops + events 26 stops，**noRing=0**（全部 2px outline）；首个 stop 是「Switch to English」（横幅），第 2 个是横幅 ✕，第 3 个是品牌链接；跳过导航 `base.html:46 a.skip-link` 存在 | ✅ |
| 折叠侧栏 | `data-collapsed=true`、`sidebarW=76`、`mainLeft=76`、`visibleLabelCount=0`、`overflowX=0`（深浅各一次） | ✅ |
| 移动抽屉 | 264px、`overflowX=0`、`clipped=[]`（0 个导航项越界）、Esc 关闭 + 焦点回 `#menu-btn`；**无关闭按钮 / 焦点不进入**（P1-3） | 部分 |
| hover | `hover-nodes-*.png`：行 hover 抬到 `--surface-2`/`--line-strong`；亮色下 index 行 hover 无变化（P2-1） | 部分 |

---

## 5. 与设计稿的剩余差距（只列值得修的）

对照 `design/ui-v2/dashboard.html` / `login.html`：

1. **dashboard**：设计稿的信息块是**两列交错**（顶部 4 KPI 通栏 → 左 2/3 活动 + 右 1/3 事件），实现为 4 KPI 通栏 → 左(任务活动 617px) + 右(最近事件 463px) → 通栏「接入你的 Agent」→ 通栏「最近任务」，**结构已对齐**。差距只剩 KPI 卡内层次：设计的 delta 是纯文本 `↑ 12% 较上周`，实现把 delta 做成药丸（本身没问题），但第 2 卡把**说明文字**塞进药丸（P1-1）。
2. **login**：设计稿左品牌卡有「网格背景 + 渐变光晕 + 3 条卖点 + 底部状态行」，实现全有；差在**垂直分布**——设计的 3 条卖点跟随内容居中，实现顶对齐后留 220px 空白（P2-4）。
3. **不修的部分（明确记录）**：设计稿首页总高 1545 vs 实现 1630（+85px，来自 KPI 卡加了 spark 与 delta），在三层结构一致的前提下这 85px 是合理代价，**不建议为对齐数字去压 padding**。

---

## 6. 契约矛盾 / 未兑现承诺

| 承诺 | 机器事实 | 判定 |
|---|---|---|
| `.data-table` 是否真被使用 | `web-src/pages` 里 `class="data-table"` **6 处 / 3 页**（admin、docs×2、keys），配 `.table-wrap` + `.table-foot`（keys 页脚「2 个 Key · 2 启用」），th 粘顶/行 hover 生效 | ✅ 兑现（不再是 0 使用的死类） |
| `--st-*` 文字对比度 | `tokens.css:66-74`（dark）/`:191+`（light）双份定义齐全；`.st-text.st-*` 在 `ui.css:3394-3398` 定义，但 **`web-src/pages` 里 `.st-text` 使用数 = 0**；`--st-*` 另有 5 处作 `color:` 用于状态字（如 `--st-assigned`），实测 20 页扫描未出现不达标 | ⚠️ 未兑现（C-3 新增的 `.st-text` 是死类）；对比度本身达标，无需改色 |
| shim 是否该删 | `ui.css:3228` 起有界注释块「亮色主题兼容层（wave 1 止血，wave 2 逐页清理后整块删除）」，注释明确写了「hover:/focus: 变体不在此列」；服务对象从 wave1 的 18 页缩到现在 **2 处真引用**（index.html:753/861） | ⚠️ **现在还不能整块删**：删掉会让这 2 处亮色失真。修掉 P2-1/P2-2 后即可整块删除（契约的「只能收缩」已满足） |
| `.toast` 副本 | `web-src/pages` 里 `^\s*\.toast\s*{` 仍在 **events.html ×2、security.html ×1**（wave1 是 11 份，已收缩但未归零） | ⚠️ 部分兑现；建议下轮删这 3 条页级规则，统一走 `Anotify.toast` |
| 三 focus 页「品牌行 + 单 h1」 | login h1 36px Fraunces、cli-auth/passkey h1 28px Inter 且分别是「输入授权码」「输入添加短码」；三页卡片宽 400/408/408、标题轴心与卡片轴心对齐（cli-auth 720/720、passkey 718/720） | ✅ 兑现 |
| 字体：Fraunces 只用于 ≥22px 三类位置 | 全站 `.page-title` 36/36 = Inter；Fraunces 只出现在 login 标题（36px）与 index `#greeting`（28px） | ✅ 兑现 |
| 契约未覆盖的新问题 | **字体走 Google Fonts CDN**（`web-src/layouts/{base,focus,login}.html:29-31`，`preconnect` + `fonts.googleapis.com/css2?family=Inter…&family=Fraunces…`）：与 §0「私有化部署 / 数据仅存于你的服务器」的定位冲突，内网部署时首屏回落到 system-ui，Fraunces 品牌标题直接消失。契约 §3.1 只定义了 `--font-sans/--font-mono`，**没有规定字体来源**——这是契约缺口，不是实现违背。 | ⚠️ 记为契约缺口（P1 级建议，不阻塞本轮） |

---

## 7. 交付与复跑

- 截图与原始数据：`design/ui-v2/shots-review3/`（79 张 PNG/JPG + `measure.json` 全量结构体 + `summary.json`），采集脚本 `shots-review3/shoot.mjs` 可复跑：
  `node design/ui-v2/shots-review3/shoot.mjs`（`--only=<page>` 单页，`--no-shots` 只测不截图）。
- 本轮**未改任何产品文件**；评审文件仅本文件 + `shots-review3/`。
- 已知限制（避免把「没测到」说成「没问题」）：① 数据全部来自 `web/mock.js`；`trace.html?traceId=…` 与 `message.html?id=…` 的 mock 里没有对应实体，本次只覆盖了它们的 **空/错误态**（两页空态都带 `.empty` + 返回动作，表现正常）；② admin 的管理员分支因 mock 用户非管理员**未渲染**，只评审了可达的「无权访问」态；③ keys 错误态未能注入（无刷新按钮），未验证；④ 未做像素级像素 diff，颜色/间距结论来自 computed style 取样。

---

```json
{"ok": true, "reason": "final acceptance passed: 0 P0 across 20 pages x 2 themes (h1 unique 20/20, content h1=0 on 17 workspace pages, #page-main=1 17/17, dupSub=false 40/40, overflowX=0 40/40 and 12/12 at 390w, contrast failures 0/226 body + 0/993 aux, focus rings 26/26, collapsed sidebar 76px with 0 visible labels, no [object Object]/undefined in 11 offline pages, hardcoded hex reduced to the single contract-approved QR fallback, .data-table now really used on 6 tables/3 pages); score 4.1/5 vs REVIEW-2 3.2 - product-grade reached; 4 P1 remain: KPI #2 misuses .stat-delta as caption (index.html:132, 224x22 outlined pill), sidebar footer clipped 44px when the lang banner occupies 45px (#sidebar bottom 944 > vh 900), mobile drawer still has no visible close button and does not move focus (12/12 measured), admin forbidden state bypasses .page/.page-head/.empty with hand-rolled .ad-forbidden*; plus 5 P2 (index.html:753 hover:bg-white/5 invisible in light because the shim excludes hover variants, index.html:861 text-zinc-500, docs 4488px with 113 lines at 10.88px, login brand card 220px void, .skeleton never visible on first paint). Contract gaps: .st-text has 0 usages, .toast copies remain in events x2 + security x1, shim cannot be deleted yet (2 real references left), and Google Fonts CDN in 3 layouts conflicts with the self-hosted positioning."}
```
