# Anotify UI v2 · 设计批评与决策（design 侧）

> 事实源：`/root/code/anotify/.pi-orchestrator/UI-V2-CONTRACT.md`（冻结契约，下称「契约」）。
> 本文只做**批评 + 裁决 + 类名/尺寸规范**，不改产品代码；所有行号指 `web-src/` 与 `web/` 的**当前**实现。
> 交付物：
> `design/ui-v2/tokens.css`（§3 逐条实现）· `ui-v2.css`（组件层，供实现侧并入 `web/ui.css`）·
> `gallery.html` · `dashboard.html` · `login.html` · `verify.mjs`（自检脚本）· `shots/`（30 张截图）
>
> 附加文件说明：任务书列了 5 个产出，我多给了一个 `ui-v2.css` 和一个 `_verify` 脚本。
> 理由：三个 HTML 若各自内联组件 CSS，会出现三份互不相同的实现，实现侧反而无法判断哪份是事实源；
> 抽出一份组件层可以让 `web/ui.css` 直接按段落搬运。两者都在允许写入的 `design/ui-v2/` 内。

---

## 0. 一页结论

现在的界面**不是丑，是没有层次**：所有东西都是同一种 1px 白描边浮在纯黑上、同一档 14px 灰字、
同一条 60px 列表行——它像 IDE 里临时拼出来的调试面板，而不是一个有产品主张的工作台。

v2 不靠加装饰解决，靠三件事：

1. **材料**：卡片从「白 2% alpha 描边」换成实底 + 单侧阴影 + 一行顶部高光，层级才可读（D1）。
2. **密度与节奏**：首屏从「3 张卡 + 两个空格」变成「问候 → 4×KPI → 活动 → 任务」，
   1440×900 首屏承载 4 个信息块，整页高度 4176 → **1545px**（D6/D7/D17）。
3. **一套组件语言**：11 份 toast、22 种圆角、70 个无 padding 的卡片收敛成一份 `.card/.btn/.badge/.data-table…`（D8–D16）。

---

## 1. 诊断：为什么现在看起来像插件面板（5 条根因）

> 截图证据：`.pi/web-verify-shots/1793644624350.png`（首页 1440×4176，下文坐标均为该图**原图**坐标，
> 显示值 ×2.09）。可对照本轮 `design/ui-v2/shots/dashboard-dark-1440-full.jpg` 看改后效果。

**R1 · 表面模型是「白 alpha 描边」，卡片等于没画。**
`web/tokens.css:21-24`：`--surface-1: rgba(255,255,255,.02)`、`--line: .08`。
在 `--bg:#050508` 上，卡片与背景的亮度差只有约 1.02:1 —— 低于可辨阈值。
证据：首页 y=160–260 的三张 KPI 卡与 y=940 的纯背景区**在显示器上几乎同色**，只有边框线可见；
侧栏 y=60–1930 整片无分层。这不是配色问题，是「没有材料」的问题。

**R2 · 首屏只有一个半信息块，其余全是纵向堆叠。**
`web-src/pages/index.html:26` 是 4 列 KPI 网格，但 `:48`/`:78` 两张卡带 `data-push-tile`，
在 `web/partials.js:1458` 的 `PUSH_UI_ENABLED=false` 下被隐藏 → 桌面右侧**空出两格**（x=440–670, y=160–260）。
再往下 `#notif-list`（`index.html:225`，limit 50）30 行 × 60px 独占 y=1320–3690，
首屏 900px 只装下 12% 的内容。**信息量不缺，缺的是横向组织。**

**R3 · 排版只有两档，页标题靠衬线硬撑。**
全站实测 **28 种字号**（`ui.css` 26 种 + 页面工具类 14 种，交集仅 5 档；其中 11px 用了 174 次、
12px 144 次，`text-[11px]` 162 次）。真正读得出来的是 11/12/13/14 四档，页标题是孤立的 30px 衬线。
顶栏 h1 只有 **16px**（`partials.js:837` `text-base`），副标题 **11px**（`:853`）——
「总览」这两个字在截图 y=65 处比列表里的 UUID 还弱，层级是反的。

**R4 · 组件语言各写各的，同一屏里三套规矩。**
- `.toast` 在 **11 个页面**各自定义（`capabilities.html:155`、`events.html:59`、`keys.html:9`、`nodes.html:26`、
  `security.html:14`、`source.html:103`、`sources.html:383` 定义；`edges.html:140`、`node.html:181`、
  `playbook.html:74`、`playbooks.html:27` 用了但**根本没定义**）——契约 §0 写的「7 份」实测是 11 份。
- 圆角 **22 种**（`ui.css`）+ 7 个 `rounded-*` 类 + 14 个内联值；`.card` 是 16px（`ui.css:43`），
  但页面里 102 次 `rounded-xl`(12px) 直接盖在手写卡片 div 上 → 同一屏卡片圆角不一致。
- `.card` 自身**没有 padding**（`ui.css:40-45`），70 个 `.card` 元素里有 12 个完全没有 padding、
  其余分散在 5 种 padding 组合 → 同类卡片内容不对齐。

**R5 · 色彩纪律失效：70/20/10 已不可测量，状态色散在四处。**
`text-white` **115 处**（`index.html` 15 / `security.html` 14 / `docs.html` 13 …）、`bg-white/*` 49 处、
`border|ring-white` 74 处、`rgba(255,255,255,*)` 48 处。这些不是「设计选择」，是绕过 token 的硬编码。
同时 9 态色有 **4 份拷贝**：`ui.css:1584-1596`（`.wf-bar`）、`graphs.js:67-75`（`STATUS_COLOR`）、
`node.html:84-93`（`.np-st-*`）、`capabilities.html:150`；另有一整套 300 级语义色被硬编码
（`ui.css:399/403/407/411`）。最直接的后果：热力图空格用 `--surface-2`（`ui.css:715`），
在纯黑底上**几乎看不见**（截图 y=650–780 是一整片黑，只剩几十个亮点），数据可视化变成了空场。

> 一句话：R1 让层级不可读、R2 让首屏无内容、R3 让标题不可信、R4 让同类组件长得不一样、
> R5 让品牌色与状态色互相稀释。视觉稿的每个决策都对着其中一条打。

---

## 2. 20 页问题清单（P0 影响第一印象 / P1 影响一致性 / P2 打磨）

| # | 页面 | P0 | P1 | P2 |
|---|---|---|---|---|
| 1 | `admin.html` | 唯一用 `max-w-7xl`（:3），与其他 16 页 6xl 不一致 → 宽屏下侧栏与内容列对齐错位 | 3 张表各写一份 `overflow-x-auto` 包裹（:97/:134/:163）+ 手写 `th`，无粘顶表头、无 `.table-foot`；`#users-tbody` 8 列（:125）在 1024 宽仍需横向滚 | `.admin-tab` 定义在 `{{define "script"}}`（:232），与其他页 `style` 块不同源 |
| 2 | `capabilities.html` | 卡片用 `<a class="card">`（:391）与 div 卡片 hover/焦点行为不一致 | 345 行内联样式（:4-348）；`.kind-*` 三色硬编码（:41/:45/:49） | 两个弹层嵌在 `#page-main` 内（:410/:532），与 sources/security 相反 |
| 3 | `cli-auth.html` | 无 `#page-main`；**2 个 `<h1>`**（:85/:164）；8 个 state section 预渲染约 620 行 | `.grid-overlay` 是白 alpha 网格，亮色下变白雾（:13/:16） | 输入内联字号（:112 `font-size:1.5rem`） |
| 4 | `docs.html` | 首节 h2 是 `text-3xl`（:131），其余同级 h2 是 `text-2xl`（:207/:368/:408/:550）→ 同级两种字号 | 16 处 `bg-white/[.02~.1]` 代码/表格底（:244-342） | `#events`(:451)/`#node`(:636) 用 `<code>` 当标题 |
| 5 | `edges.html` | `.toast` 用了但**无定义**（:140）→ 裸文本浮层 | 弹层内联 `rgba(0,0,0,.62)`（:144）且挂在 `#page-main` 内（:143） | `.edge-missing` 三处硬编码黄（:78-81） |
| 6 | `events.html` | 100 行/页（:198）× 每行一张 `.card`（:156）→ 单页高度失控 | 4 个筛选控件固定 `w-48`（:111/:118/:125/:134）不随容器伸缩 | `#ev-active` 初始缺 `flex`（:150），靠 JS 补类才成型 |
| 7 | `index.html` | ① 4 列 KPI 只有 2 张真卡（:26/:48/:78 + `partials.js:1458`）② 30 行单列堆叠致 4176px（y=1320–3690） | 热力图空格 `--surface-2` 不可见（`ui.css:715`） | quickstart 两列 flex 右侧留白（:163 `flex:1 1 16rem`） |
| 8 | `keys.html` | 表格初始为空且无 loading（:130）→ 首屏是「一张白纸」 | 弹层在 `#page-main` 内（:183）+ `bg-black/50`（:97/:347） | JS 内联 `rgba(255,255,255,.06)`（:590） |
| 9 | `login.html` | 单卡居中 + 大片空场；主面是 `.glass rounded-3xl`（:72）而非 `.card` | 同元素两个 max-w 并存（:304） | 状态色 map 硬编码（:412-415） |
| 10 | `message.html` | 3 张同权卡片（:3 起），无页头层级 → 不知道先看哪 | 字段网格 `grid-cols-2 md:grid-cols-3`（:48）遇不定长字段留洞 | `.md-body` 四行硬编码（`ui.css:886/895/896/902`） |
| 11 | `node.html` | 正文 100% JS 生成（:173-179），首屏只有一张 loading 卡 | `.np-st-*` 9 态硬编码（:84-93） | toast 借 `.card rounded-xl`（:181）而 `.toast` 无定义 |
| 12 | `nodes.html` | 注册面板/结果/错误三重 `hidden`（:100/:155/:138），改版极易只删一个 | `.slot.busy` 硬编码 `#6366f1`（:24）；5 个代码块无统一头 | 全站 0 个 `<form>`（:811 注释），回车提交全靠手写 |
| 13 | `passkey-enroll.html` | **4 个 `<h1>`**（:92/:171/:279/:356）+ 2 个 h2（:431/:499）混用 | 6 个 state section 结构重复（:65 起） | 内联 `font-size:1.5rem`（:112） |
| 14 | `playbook.html` | 首屏 4 张同构空卡（:37/:45/:62/:70）= 「标题 + 0」，像没写完 | `<title>` 与 `playbooks.html` 同 key（:1）→ 两页浏览器标题相同 | `.yaml-box` 硬编码（`ui.css:1927`） |
| 15 | `playbooks.html` | `.pb-grid` 最小 320px（`ui.css:1749`）+ body `overflow-x-hidden`（`base.html:28`）→ 窄屏卡片被裁且不能滚 | `.toast` 无定义（:27） | `.pb-card` 14px 圆角与 `.card` 16px 不一致 |
| 16 | `receivers.html` | **唯一没有页头**的页（:7 第一个元素就是 tab 条） | push 关闭时 `replaceChildren` 整页（:989-1000），WS 区块一起消失 | 状态 map 硬编码（:862-865） |
| 17 | `security.html` | Passkey 列表无卡（:114）而会话列表有卡（:134）→ 同页两种列表语言 | 硬编码 `#fcd34d`(:150/:153)、`#0a0a11`(:425)、`rgba(251,191,36,.2)`(:145/:555) | 恢复码 4 列（:219）窄屏换行后对齐散 |
| 18 | `source.html` | 正文全 JS（:144-148），首屏 loading 卡 | toast 组合 `toast card rounded-xl`（:152）；`.param-*` 硬编码（:56/:261/:264） | 与 node/trace 三页壳雷同却各写一套 |
| 19 | `sources.html` | `#src-list` 初始全空且无 loading（:434） | 394 行内联样式（:4-397）+ 弹层内联尺寸与遮罩（:451/:470/:442） | 引用不存在的 `--fg` 6 处（:70/:136/:215/:293/:340/:356，无兜底） |
| 20 | `trace.html` | 正文全 JS（:83-87），首屏 loading 卡 | 容器 `max-w-6xl`（:55）而 node/source 是 `max-w-3xl`（`node.html:157`）→ 同类详情页宽度不同 | 错误直接写进 `#trace-body`（:232），无 toast |

> 统计口径补充：`--fg`/`--danger`/`--ok`/`--panel-hover`/`--text-muted`/`--mono` 是**引用了但 tokens.css 没定义**的
> 变量（7 处有兜底、6 处无兜底），契约 §3.2 要求修掉；`--on-solid` 则相反——定义了但全仓 0 引用，
> `ui.css:334` 用硬编码 `#0a0a10` 顶替。

---

## 3. 标题字体 A/B 结论

视觉证据：`shots/gallery-light-1440-full.jpg` 末尾「标题字体 A/B」区块（同一行文案、两套字体）。

| | A · Fraunces 600 / 30px / -0.015em | B · Inter 600 / 28px / -0.03em |
|---|---|---|
| 观感 | 有温度、有品牌记忆点；与 `Anotify` 手写体 logo 同源 | 更「工具」，和 14px 列表同族，整页只剩一种气质 |
| 中文 | CJK 回落到 Source Han Sans（黑体）+ 英文衬线，混排成立（Stripe/Figma 同款做法） | 中英完全同族，最稳但最平 |
| 风险 | ≤20px 时英文衬线笔画过细，与 13px 中文正文并列会「发虚」 | 无风险，但也无记忆点 |

**推荐：A，但收紧边界（已落到 tokens.css 与视觉稿）。**

- Fraunces（`--font-display`）只允许出现在 **3 个位置**：
  ① 登录大标题 40px　② 首页/详情页欢迎语 28–34px　③ 空态或章节主标题 ≥22px。
- **≤20px 一律 Inter**，页面主标题 `.page-title` 是 28px Inter 600 `-0.02em`（视觉稿里的"功能标题"用 B）。
- 理由：品牌需要一处「不是系统字体」的记忆点，但衬线的可用下限是 22px；再往下它既不能表达层级，
  又会让中文卡在黑体与衬线之间。**A 在 ≥22px 胜出，B 在 ≤20px 胜出** —— 视觉稿按这条线切分，不是二选一。

---

## 4. 决策 D1…D20（实现侧照此执行）

> 每条给：**做什么 / 类名与关键尺寸 / 对应契约**。类名已与契约 §5 对齐；
> 标 `[新增]` 的是契约 §5 未列、但实现侧一定会用到的容器类（建议补进契约，见 §6）。

### 材料与外壳

**D1 · 表面模型换成实底（对应契约 §3.1、§8）**
`.card { background: var(--surface-1); border:1px solid var(--line); border-radius: var(--r-lg); box-shadow: var(--shadow-1), var(--elevate); }`
dark `--surface-1:#101018` 在 `--bg:#08080e` 上是明确可读的一层；`.card-hover:hover` 抬 `--shadow-2` + `--line-strong` + `translateY(-2px)`。
`--elevate`（`inset 0 1px 0 rgba(255,255,255,.04)`）替代旧代码里散落的 inset 白 alpha；亮色下换成 `rgba(255,255,255,.9)`。
**禁止**再出现 `--surface-*: rgba(255,255,255,.0x)`。

**D2 · 顶栏 h1 = 上下文标题 15px / 页面大标题 28px（对应 §4.2 与 §8，见 §6 契约矛盾 1）**
`.topbar h1 { font-size:15px; font-weight:600 }` + `.topbar-sub { 11px; color:var(--faint) }`；
内容区 `.page-title` 是 `<h2>` 28px（Fraunces 或 Inter，见 §3）+ `.page-sub` 13px/45% 行高。
理由：64px 高的顶栏塞不进 28px 标题 + 副标题；现状本来也是「顶栏 h1 16px + 页面 h2 30px」（`index.html:8`）。

**D3 · 侧栏 264 / 76 可折叠（对应 §4.1）**
`#app-shell[data-collapsed="true"]` 驱动，`grid-template-columns: var(--sidebar-w) minmax(0,1fr)` 过渡 200ms。
折叠时 **不重建 DOM、不删文字节点**：`.side-text` 用 `max-width:0 + opacity:0` 收窄（e2e 取 label 文本仍成立），
`.side-count`/`.side-user-main`/`.brand-word` 同理；`.side-brand` 居中，折叠按钮浮到品牌下方 24×24。
tooltip = `.side-link::after { content: attr(data-tip) }`，`left: calc(100% + 8px)`。
**坑**：`.side-nav` 一旦 `overflow-y:auto` 会强制另一轴也 auto，tooltip 被裁 —— 折叠态必须 `.side-nav{overflow:visible}`，
只在 `max-height:620px` 才恢复滚动（此时 tooltip 退化为 `title`）。

**D4 · 导航分组与行高（对应 §4.1）**
`.side-label`（Workspace / Resources）：11px / 600 / `letter-spacing:.1em` / uppercase / `--faint`，`margin:18px 0 6px`。
`.side-link`：高 **38px**、`padding:0 10px`、`border-radius:8px`、13px/500、图标 **18px stroke 1.75**、gap 10px。
现状是 `ui.css:51-61` 的 14px / `0.55rem .8rem` / 10px 圆角，且图标 16px（`partials.js:722` 走默认 `h-4 w-4`）
与顶栏 20px（`:820`）不一致 —— v2 统一成 18 / 16 / 14 三档（侧栏 18、按钮 16、小号 14）。

**D5 · active 态（对应 §4.1）**
`background: var(--accent-soft)` + 左侧 **2px** accent 条（`::before`，`left:-12px`（折叠态 `-8px`）贴侧栏边缘，`top/bottom:8px`，
圆角 0 2px 2px 0）+ `aria-current="page"`。图标同步 `color: var(--accent)`。

**D6 · 页头与容器（对应 §4.3）**
`.page { max-width: var(--w-page=1200px); margin-inline:auto; padding:28px 32px 40px; display:grid; grid-template-columns: minmax(0,1fr); gap:24px }`
`.page-narrow { max-width:760px }`（node/source/trace/message 四个详情页统一用这个，替代现在的 3xl/6xl 混用）。
**`grid-template-columns: minmax(0,1fr)` 是硬要求** —— 默认 `min-content` 会让宽表格把整页顶宽（390 宽实测 +44px 溢出，已修）。

**D7 · 统计卡 `.stat`（对应 §5）**
`.stat` grid gap 14、padding 20、`.stat-icon` **30×30** / 8px 圆角 / accent-soft 底 + accent 18px 图标（`.is-success/.is-warn/.is-error` 三变体）；
`.stat-value` **30px / 600 / `-0.03em` / tabular-nums**；`.stat-delta` 22px 药丸 + `↑/↓` 12px 图标（`.up` 绿 / `.down` 红）；
`.stat-spark` 高 34px、`preserveAspectRatio="none"` + `vector-effect="non-scaling-stroke"`（路径线宽不随拉伸变粗）。
`.stat-foot` 11px 双端对齐。**4 列**；`KPI 数量永远是 4**（现状 push tile 被隐藏后只剩 2 张 → 见 D18 的空位处理）。

**D8 · 按钮 `.btn`（对应 §5）**
基类 **36px 高 / 12px 圆角 / 13px 字 / gap 7**；`.btn-primary` accent 填充 + `--on-solid` 文字（dark 深字、light 白字）；
`.btn-ghost` 透明 + `--line` 边；`.btn-quiet`［新增变体］无边框纯文字（表格行内动作用）；
`.btn-danger` error-soft 底 + error 字；`.btn-sm` **30px**；`.btn-lg` **42px**；`.btn-icon` 36×36；`.btn-block`。
`:focus-visible { box-shadow: var(--ring) }` 全站唯一入口，组件不要各写 outline。
`a.btn-primary` 必须保持 `display:inline-flex`（e2e 取 href，契约 §5）。

**D9 · 徽章与 9 态（对应 §5）**
`.badge` 22px 药丸 + 5 变体；`.status-badge` 保留（前面 5px 圆点用 `currentColor`）；`.tag-chip`/`.src-badge`/`.target-pill` 保留重写。
9 态**只允许** `--st-pending/assigned/acked/running/succeeded/failed/canceled/abandoned/lease_expired`；
`.dot` 7px，`.dot-<state>` 一一映射；`.st-text.<state>` 供「只染文字」的场景（替代 `node.html:84-93`）。
`graphs.js` 的 `STATUS_COLOR` 改成读 `getComputedStyle(document.documentElement).getPropertyValue('--st-…')`，
并在 `anotify:theme` 事件里重取（契约 §1）。

**D10 · 表格 `.table-wrap / .data-table`（对应 §5）**
`.table-wrap { overflow:auto; max-height:560px; border:1px solid var(--line); border-radius:12px }`
`th { position:sticky; top:0; z-index:2; background: var(--surface-2); 11px / 600 / uppercase / .06em / --faint; padding:9px 14px }`
`td { padding:11px 14px; border-bottom:1px solid var(--line) }`，`.num { text-align:right; tabular-nums }`，`.shrink { width:1% }`，
`.table-empty`（40px 高居中）、`.table-foot`（分页条，底部分隔线 + 11px 字）。

**D11 · 表单（对应 §5）**
`.input/.select/.textarea` 统一 **36px** 高 / 8px 圆角 / `--surface-2` 底 / 12px 内距 / 13px 字；
`:focus` → accent 边 + `--ring`；placeholder 用 `--faint`；`.is-invalid` → error 边 + `--ring-error`。
`.field`/`.field-label`（12px/500/--muted）/`.field-hint`（11px/--faint）/`.field-error`（11px/--error）。
`.switch` 38×22、`.check` 16×16、`.seg-switch`+`.seg-item`（26px 高，保留旧名）。

**D12 · Tabs（对应 §5）**
`.tabs` 底部 1px 线 + 横向可滚；`.tab` 38px 高 / 13px；`.tab.active::after` 2px accent 下划线（左右内缩 6px）+ `.count` 药丸。
`.filter-tab` 保留为 28px 药丸型（筛选场景），`.active` 用 accent-soft。

**D13 · 列表行 `.list / .list-row`（对应 §5）**
`.list-row { grid-template-columns: auto minmax(0,1fr) auto; min-height:56px; gap:12px; padding:10px 4px }`，
相邻行 `border-top`；`.list-main`（标题 + meta 两行）、`.list-title` 13px/500、`.list-meta` 11px/--faint + 2px 圆点分隔、
`.list-side`（时间 + `chev`，chev 仅 hover 显示）、`.list-lead` 28×28 图标底。
`.notif-row` 保留但重写成 56px 紧凑行（`min-height:56px; padding:8px 12px`）。

**D14 · 卡片脚与撑满［新增 `.card-foot` / `.card-fill`］**
`.card-fill { display:flex; flex-direction:column; height:100% }` + `.card-foot { margin-top:auto; padding-top:12px; border-top:1px solid var(--line) }`。
用途：双列布局里左卡比右卡矮时，「加载更多」贴在卡片底部而不是浮在半空（视觉稿 `dashboard-dark-1440-full.jpg` y≈1345 处）。

**D15 · 浮层（对应 §5）**
`.modal-backdrop`（`color-mix(bg 66%)` + blur 6px）+ `.modal-panel`（`max-width:520px`、`--r-xl`、`--panel-modal`）；
`.modal-head/.modal-body/.modal-foot`；`.toast-host` 固定在右下 `20px`、`max-width:min(380px, 100vw-40px)`，
`.toast` 三列网格（icon/文字/关闭）+ `.success/.error/.warn`；
`.menu`（`--panel-overlay`、`--shadow-3`、6px 内距、`.menu-head/.menu-item/.menu-sep`）同时服务头像菜单、语言菜单、行内动作。
z-index 只用 `--z-header:20 / overlay:30 / sidebar:40 / menu:60 / toast:80 / modal:90`。

**D16 · 空态 / 骨架 / kbd（对应 §5）**
`.empty`（44×44 图标底 + 15px 标题 + 13px 说明 + 动作按钮，`padding:44px 24px`）；空态**必须给下一步动作**。
`.skeleton`（`--surface-2 → --surface-3 → --surface-2` 1.5s shimmer，`.is-circle/.is-line-sm/.is-line-lg/.is-block`）。
`.kbd` 复活（18px 高、mono 11px、2px 下边）。

**D17 · 热力图（对应 §5 + §8 图表配色）**
格 **11px / gap 3px / 圆角 2px**；空格 `--hm-empty`（dark `#171722`，**不是** `--surface-2`，否则在实底卡上看不见）；
4 级 `--accent-1..4`；`grid-auto-flow: column; grid-template-rows: repeat(7, …)` 按周列填充。
53 周 × 7 = 371 格，1440 下宽 739px；月份标签按**真实日期**在月切换列上绝对定位（`left: w*14px`），
移动端外层 `.hm-scroll { overflow-x:auto }`。今天格 `.hm-today` 用 1px accent 描边。
右栏 3 个统计（全年/最长连续/最高单日）+ 一句「深浅只表示数量」。

**D18 · 空位与降级（对应 §0-3：4 KPI）**
`data-push-tile` 仍可隐藏（业务正确），但 KPI 网格改成 `repeat(4, minmax(0,1fr))` + 隐藏后由**新的第 4 张真卡**占位
（视觉稿用「待处理告警」/「平均时延」这类不需要 push 的数据源），而不是留空两格。短内容卡不许撑高整行。

**D19 · 移动端（对应 §4.1、§0-2）**
`<1024px`：侧栏 `position:fixed` + `translateX(-100%)`，`.shell[data-drawer="true"]` 打开，
`#sidebar-overlay` 出现（`color-mix(bg 62%)` + blur 3px），`body.overflow=hidden`，Esc 关闭并归还焦点到 `#menu-btn`，
点 `.side-link` 自动关闭（全部已实现并截图验证）。
`<640px`：KPI 改 **2 列**、`.stat` padding 14、值 24px、**隐藏 sparkline 与 foot**（否则 4 张卡吃掉 800px 首屏）。

**D20 · 动效（对应 §8）**
只用 `--dur-fast:120 / --dur:200 / --dur-slow:320` + `--ease`；hover 位移 **≤2px**；只用 `opacity/transform/box-shadow`；
`prefers-reduced-motion: reduce` 下 `animation-duration/transition-duration` 归 0.001ms 且 `.card-hover:hover` 取消位移。

---

## 5. 亮色模式的坑：现在哪些地方会因为 alpha-white 表面模型变脏

> 结论先说：**不是「把 tokens 加一套 light 值」就能亮**。当前 UI 有 4 类硬编码会一起炸。
> 下面每条都给了「症状 → 文件:行 → 修法」，可以直接当 checklist 用。

### 5.1 四类会炸的硬编码

| 类 | 数量 | 亮色下的症状 | 修法 |
|---|---|---|---|
| `text-white` | **115** 处（`index.html:8/36/56/66/89/105…`、`security.html` 14、`docs.html` 13） | 白字画在白卡上 → **文字消失**（这是最致命的一条） | → `var(--text)`；hover 态 → `var(--text)` |
| `bg-white/{5,[0.02],[0.03],[0.04],10}` | **49** 处（`index.html:85/208/484/489/823`、`docs.html` 16） | 白底叠白底 = 无边界；输入框/代码块/表头全部「熔化」 | → `var(--surface-1/2/3)`；hover → `--surface-2` |
| `border|ring|divide-white/*` | **74** 处（`index.html:197/227/823/862`、`security.html` 7） | 边框染成更亮的白 → 卡片轮廓消失 | → `var(--line)` / `var(--line-strong)` |
| `rgba(255,255,255,*)` 直接写在 CSS 里 | **26**（`ui.css`）+ 48（全仓） | 同上；`.code-head`(:590)/`.seg-item-active`(:932)/`.target-pill`(:1731)/`.out-chip`(:1682) 的底直接变白 | → `--surface-2`/`--surface-3`；边框 → `--line` |

关键行号（`web/ui.css`，白 alpha 26 处）：
`28`(.glass 边) `337/338/344`(.btn-primary 白边+白雾) `414` `462` `581`(.code-block 边) `590`(.code-head 下边)
`595` `605` `725`(.hm-cell hover) `895/896/902`(.md-body 的 code/pre/th 底) `932`(.seg-item-active) `967/969/999/1000`(flograph 点阵)
`1130/1138`(.wf-node) `1540` `1638`(.wf-switch on) `1682/1694`(.out-chip) `1731`(.target-pill) `1800`(.pb-bar)

### 5.2 暗色实底与阴影（亮色下变成黑块/灰雾）

| 现状 | 位置 | 修法 |
|---|---|---|
| `.hm-tip` `background:#16161f` + `rgba(0,0,0,.5)` | `ui.css:736-752` | `--panel-overlay` + `--shadow-2` |
| `.wf-node` 渐变 `#17171d→#121216`、`.is-trigger` `#1b1b2a→#14141f` | `ui.css:1127/1229` | `--surface-1` / `--panel-overlay` |
| `.flograph-legend` `rgba(18,18,24,.82)`、`:1444` `rgba(20,20,25,.9)` | `ui.css:1389/1444` | `color-mix(in srgb, var(--panel-overlay) 88%, transparent)` |
| 14 条 `box-shadow` 里的 `rgba(0,0,0,.x)` | `ui.css:751/782/1131/1139/1302/1336/1662…` | `--shadow-1/2/3` |
| QR 码 `background:#0a0a11` + `color:var(--text)` | `security.html:425` | **保持深底**（QR 是深色模块，亮色下必须仍然深底白边），并把容器注释写明「不跟主题反转」；`--code-text` 在亮色是深色，直接反转会让 QR 扫不出来 |
| `.md-body` `#fff` 标题 / `rgba(255,255,255,.06)` code 底 / `rgba(0,0,0,.35)` pre 底 | `ui.css:886/895/896/902` | `--text` / `--surface-2` / `--code-bg` |
| `.btn-primary` `#fff` 底 + `#0a0a10` 字 | `ui.css:333-334` | `--accent` + `--on-solid`（`--on-solid` 早已定义，0 引用） |

### 5.3 语义色 300 级（亮色下对比不足）

`#6ee7b7` / `#fda4af` / `#fcd34d` 被硬编码在 `ui.css:399/403/407/411`（`.status-*` 文字色）、
`node.html:54/63`、`security.html:554/1840`、`login.html:169/223/412-415`、`receivers.html:862-865`、`passkey-enroll.html:387/637`。
在 `#f7f8fc` 上这些 300 级几乎不可读 → tokens.css 已给亮色深一档：`--success-300:#047857` / `--error-300:#be123c` / `--warn-300:#b45309`。
**实现侧必须把硬编码全部换成这三个变量**；`--panel-hover` 的兜底是 `rgba(255,255,255,.08)`（`security.html:65`），亮色下 hover 会「变亮」，
必须改成 `--surface-2`。

### 5.4 主题基础设施缺口（不修则亮色根本出不来）

1. 全仓 `prefers-color-scheme|data-theme|color-scheme` **0 命中** —— 目前没有任何主题机制。
2. `theme-color` 三处硬编码 `#050508`（`base.html:8`、`focus.html:8`、`login.html:8`）→ 亮色下移动端地址栏仍是黑的，
   要按契约 §1 由 pre-paint 脚本改成 `#f7f8fc`。
3. `tailwind.config.js` 未配 `darkMode`，且 `content` 不含 `ui.css/tokens.css` → **不得**用 `dark:` 变体；
   主题切换只能靠 tokens.css 的 `:root[data-theme="light"]` 覆写（视觉稿已验证这条路径可行）。
4. `--on-solid`/`--code-text`/`--panel-*` 已在 tokens.css 里，但产品侧几乎没被引用 → 换主题时这些区域不会跟着变。

### 5.5 已验证的亮色结果

`shots/{gallery,dashboard,login}-light-1440-first.png`、`-light-390-first.png`：
亮色下卡片、表格、热力图、徽章、代码块、toast、菜单、模态全部可读，无「脏灰」「白雾」「文字消失」。
**唯一必须在实现侧盯住的**：凡是我在视觉稿里写成 `var(--…)`、而产品侧还写着 Tailwind 色板类的地方，都要按 5.1 替换完，
否则亮色只亮一半（半张页白、半张页黑）。

---

## 6. 契约矛盾 + 建议（不改契约文件，交协调者裁决）

**C1 · §8「页标题 28/34 semibold」 vs §4.2「顶栏左侧：页面标题 h1」。**
现状是「顶栏 h1 = 16px」（`partials.js:837`）+「页面 h2 = 30px 衬线」（`index.html:8`）。
若严格照 §8 把 h1 做 28px，64px 高的顶栏放不下标题 + 副标题，且会与内容区标题打两次层级。
**建议**：§8 的「页标题」明确指内容区 `.page-title`（h2，28px），顶栏 h1 定位为 15px 上下文标签。
视觉稿按这个解释实现（D2）。若协调者不同意，则 §4.2 的顶栏高度需从 64 提到 88，属于契约级变更。

**C2 · §2「`header h1` 全站唯一」 vs 现状 focus/login 布局页有多个 h1。**
`cli-auth.html` 有 2 个 `<h1>`（:85/:164），`passkey-enroll.html` 有 4 个（:92/:171/:279/:356），
它们**不走** `mountLayout`（无 `#page-main`），因此没有顶栏 h1 —— 现状「唯一」只在 17 个 workspace 页成立。
**建议**：把这条约束改写为「workspace 布局内 h1 唯一」，并在 wave 2 的 D 组（login/cli-auth/passkey-enroll/security）
把这些 h1 降级为 h2 + `.page-title` 类，同时给 focus/login 布局加一个最小页头（只有品牌行，不加 h1）。
需要协调者确认后再动，否则会与现有 e2e 的 h1 断言冲突（改前必须 `grep scripts/e2e/suites/`）。

**C3 · §5 组件清单缺 3 个实现侧必然用到的容器类。**
本轮视觉稿引入了 `.card-foot`、`.card-fill`、`.page-narrow`（还有 `--fs-*`/`--gap-*`/`--w-page` 这批排版 token）。
不写进契约，wave 2 五个分组会各自发明同类名字（正是 R4 的复因）。
**建议**：在契约 §5 补一行「容器：`.card-fill`（纵向撑满 + `.card-foot` 贴底）、`.page-narrow`（760px 详情页）、
`.card-foot`（卡片底栏：加载更多/分页）」；在 §3.2 补「字号阶梯 `--fs-display..--fs-micro`」。

**C4 · §0 的「7 份 `.toast`」数字过期。**
实测 11 个页面文件（定义 7 处 + 用了没定义 4 处，见 R4）。删除计划应以 11 份为准，且 `edges/node/playbook/playbooks`
四页现在是**裸文本浮层**，属于功能性缺陷，不是纯视觉问题 —— 建议在 wave 2 的 A/B/C 组各修一次。

---

## 7. 自检：方法、结果、截图索引

### 7.1 方法（web_verify 不在子会话工具集内）

本会话的工具集没有宿主扩展 `web_verify`（子 agent 只继承角色声明的工具），因此用等价脚本自检：
`design/ui-v2/verify.mjs` —— playwright-core 1.62 + 本机 chromium（`~/.cache/ms-playwright/chromium-1208`），
静态起服务后对 **3 页 × 2 主题 × 2 视口** 逐项断言：

```bash
node design/ui-v2/verify.mjs            # 全量（12 组合，约 60s）
node design/ui-v2/verify.mjs --only=dashboard
```

断言：① `console.error` / `pageerror` **必须为 0**（`console.warn` 只记录，Tailwind CDN 的生产提示属预期）
② `documentElement.scrollWidth - clientWidth ≤ 1px`（横向溢出即失败，并打印溢出元素）
③ 记录 `scrollHeight`（页面高度）　④ 截图落盘 `shots/`。

### 7.2 结果（最后一次全量）

| 页面 | 主题 | 视口 | 整页高度 | 横向溢出 | console.error |
|---|---|---|---|---|---|
| gallery | dark / light | 1440×900 | 4987 | 0 | 0 |
| gallery | dark / light | 390×844 | 7446 | 0 | 0 |
| dashboard | dark / light | 1440×900 | **1545** | 0 | 0 |
| dashboard | dark / light | 390×844 | 2105 | 0 | 0 |
| login | dark / light | 1440×900 | 900 | 0 | 0 |
| login | dark / light | 390×844 | 844 | 0 | 0 |

`RESULT: PASS`（12/12）。首页达标：契约要求 1440×900 下 **≤2600px**，视觉稿 **1545px**（现状 4176px，−63%）。

### 7.3 截图索引（`design/ui-v2/shots/`，共 30 张）

| 文件 | 看什么 |
|---|---|
| `dashboard-dark-1440-first.png` / `-light-1440-first.png` | **首屏第一印象**：问候 + 4 KPI + 热力图上沿 |
| `dashboard-dark-1440-full.jpg` / `-light-1440-full.jpg` | 整页（1545px）：KPI / 热力图 / 最近任务 / 最近事件 / 接入 Agent |
| `dashboard-dark-390-first.png` / `-light-390-first.png` | 移动端单列、KPI 两列紧凑、页头动作撑满 |
| `dashboard-dark-390-drawer.png` | **抽屉打开态**（264px + overlay 模糊，内容不被挤压） |
| `dashboard-dark-1440-collapsed.png` | **侧栏折叠 76px 图标轨**（active 条仍在） |
| `dashboard-dark-1440-collapsed-tooltip.png` | 折叠态 hover tooltip（`事件流`）裁切图 |
| `gallery-*`（同 6 类） | 组件画廊：外壳 / KPI / 卡片 / 按钮 / 徽章 9 态 / 表格粘顶 / 表单 / tabs / 列表 / 空态骨架 / toast menu modal / 代码块 / 图表色板 / 字体 A/B |
| `gallery-dark-1440-collapsed-tooltip.png` | 同上，画廊版 |
| `login-*`（同 4 类） | 品牌区渐变+网格+流程示意 / Passkey 主按钮 / 移动端单列 |

对照基线：`.pi/web-verify-shots/1793644624350.png`（首页 4176px）、`1793644623941.png`（登录）。

---

## 8. 未决问题与风险

1. **字体来源**：视觉稿用 Google Fonts CDN 加载 Inter / Fraunces / JetBrains Mono（design 目录允许）。
   产品页当前 `web/fonts/` 只有手写体（sacramento/greatvibes/caveat），**Inter 与 Fraunces 都没有自托管文件**，
   本机也没装。实现侧要么自托管 woff2（推荐，含 `font-display:swap` 与 CJK 回退），要么接受系统字体回退——
   后者会让「品牌衬线」策略直接失效。**这是 wave 1 的前置依赖，建议协调者先定。**
2. **热力图数据形状**：视觉稿按 `daily[52~53 周]` 假设。若 `/v1/stats` 只回近 30 天，D17 的 53 周视图需要改数据契约或降级为 26 周。
3. **移动端 390 下热力图**：靠卡片内横向滚动（`.hm-scroll`）查看 53 周；若产品不接受横向滚动，需改成「近 13 周 + 展开」。
4. **deep link / mock 提示条**：`partials.js:1293` 的 mock 黄条与 `mock.js` 自带暗色样式（`:3710-3718`）不在本轮范围，
   亮色切换后它们会是两块孤岛，wave 2 需一并处理。
5. **登录页 e2e**：视觉稿把「登录/注册」从 `tab-btn` 改成 `.tabs .tab`。类名变了，若 `scripts/e2e/suites/` 里有 `.tab-btn` 断言，
   wave 2 的 D 组必须先 `grep`，必要时保留 `.tab-btn` 作为附加类（契约 §2 的兼容原则）。
6. **未做**：真实 20 页的逐页视觉稿（本轮只给 3 页 + 组件库）；`--st-*` 在 flograph 全屏模式下的实际观感未截图验证。
