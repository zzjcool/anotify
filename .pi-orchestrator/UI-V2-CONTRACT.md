# Anotify 前端大翻修 · 冻结契约 v1（2026-11）

> 协调者冻结。所有参与改版的 agent（design / impl / reviewer）以本文为唯一接口事实源。
> 与本文冲突的直觉一律让步；需要改契约必须由协调者修订并注明原因。

## 0. 目标与质量判据

现状（已截图取证）：单一暗色、白色 alpha 表面模型、侧栏固定 240px 不可折叠、
页面头/卡片/列表靠 Tailwind 手搓、首页长 4176px、7 份 `.toast`、9 态配色三处定义。
观感评价：像开发者临时拼的插件面板，缺少产品级视觉层次。

目标：**产品级现代化 Dashboard**，具体可验收判据：

1. 亮/暗双主题（跟随系统 + 手动切换 + 持久化），首屏无闪白/闪黑（pre-paint）。
2. 桌面侧栏可折叠为图标轨（持久化），移动端抽屉保留且可键盘关闭。
3. 首页在 1440×900 下总高 ≤ 2600px（当前 4176px），信息层次：问候/摘要 → 4 KPI → 活动 → 最近任务。
4. 所有共享组件颜色只来自 `web/tokens.css`（允许的例外：图表状态色走新增 `--st-*` token）。
5. 20 个页面统一页头/卡片/列表/表格/空态语言；`en/es` 页面零 CJK 残留。
6. 所有既有 e2e UI 契约保持绿色（见 §2）。

## 1. 主题架构（硬性）

- `<html data-theme="dark|light">`；`tokens.css` 里 `:root` = dark，`:root[data-theme="light"]` = light。
- 三套 layout 的 `<head>` 里加**同步**内联脚本（在 CSS link 之前）：
  1. 读 `localStorage['anotify-theme']`（值 `dark|light|auto`）
  2. `auto`/缺失 → `matchMedia('(prefers-color-scheme: light)')`
  3. 写 `document.documentElement.dataset.theme`、`style.colorScheme`
  4. 更新 `meta[name="theme-color"]`（dark `#08080e` / light `#f7f8fc`）
  - 注释里写明「防 FOUC，必须在首帧前执行，不能 defer」。
- 切换 API（`web/partials.js` 暴露）：`Anotify.theme = { get(), set(mode), cycle() }`，
  并在 `document.dispatchEvent(new CustomEvent('anotify:theme', {detail:{theme}}))`。
- 订阅：`graphs.js` / `traceview.js` 重新取色后重绘或更新样式；首页热力图用 CSS 变量自然生效。
- `tokens.css` 里必须声明 `color-scheme`（dark/light），滚动条、表单控件跟随。
- **不允许**用 Tailwind `dark:` 变体（`tailwind.config.js` 未配 darkMode，且改配置会牵动全站生成物）。

## 2. 不能破坏的既有契约（e2e 硬断言）

改任何 DOM 前先 `grep` `scripts/e2e/suites/`。必须保留：

| 锚点 | 位置 | 约束 |
|---|---|---|
| `#page-main` | 17 个页面 | 每页恰好一个；`mountLayout` 必需 |
| `#sidebar` `#sidebar-overlay` `#menu-btn` | `partials.js` | 移动端开合用 `-translate-x-full`/`hidden` 状态类（可增强，不可改名） |
| `.side-link` + `.active`、`.side-label` | `partials.js` / ui.css | 文本节点仍是链接最后一个文本子节点（e2e 取 label） |
| `#sidebar-username` `#sidebar-avatar` `#topbar-avatar` | `partials.js` | 登录态锚点 |
| `#scope-switcher` 全套 `.scope-*` + `data-ns-id` + `aria-checked` + role | `partials.js` | 结构/属性不得删改；初始 `hidden` |
| `#lang-switcher` `#lang-switcher-login [data-lang-list]` `#lang-switcher-data` | layout/partials | 消费后 remove；`[aria-current="true"]` 保留 |
| `#admin-nav-section`（`dataset.mounted` 幂等） | `partials.js` | admin 入口 |
| `#anotify-toast-host` | `partials.js` | toast 宿主 |
| `header h1` 全站唯一，含 `.scope-badge`（`scopeLevel==="deployment"`） | `partials.js` | 页面内容里禁止新增 `<h1>` |
| `#notif-list` + `.notif-row` | `index.html` | 错误态 0 行 + 列表内有重试 button |
| `#ev-list` 事件卡：`<a class="card … cursor-pointer">` + `div.mono.truncate` + 首个 `<code>`=源名 | `events.html` | e2e 用 `document.querySelector('.cursor-pointer')` 点击事件卡，并断言跳转 `trace.html?traceId=`、`.wf-node>0`、`.wf-switch button` ×3、默认图视图 |
| `#ev-list`、`#ev-list .card div.mono.truncate` | `events.html` | 事件流结构断言 |
| `#greeting` `#kpi-users` `#current-device-card` `#msg-title` `#recovery-*` | 各页 | 不得改名 |
| hreflang 链接 | layouts | 属性顺序 + 自闭合 `/>` 被正则锁死，不要动 |
| 文案 | 全部 | 新文案必须进 4 份 locale（zh-CN/en/ja/es），`en/es` 零 CJK |

## 3. Token 规范（v2）

### 3.1 新增语义层级（旧名必须继续存在且可用）

暗色（`:root`）：

```
--bg:#08080e; --bg-raise:#0c0c13; --surface-1:#101018; --surface-2:#15151f; --surface-3:#1b1b27;
--line:rgba(255,255,255,.07); --line-strong:rgba(255,255,255,.14);
--text:#edeef5; --muted:#a2a6b6; --faint:#6e7284;
--accent:#8b8bfd; --accent-strong:#a6a6ff; --accent-soft:rgba(139,139,253,.14);
--success:#34d399; --error:#fb7185; --warn:#fbbf24; --info:var(--accent);
--shadow-1:0 1px 2px rgba(0,0,0,.4); --shadow-2:0 8px 24px -8px rgba(0,0,0,.55); --shadow-3:0 24px 64px -16px rgba(0,0,0,.7);
--panel-overlay:#14141e; --panel-modal:#0d0d15; --code-bg:#0b0b13; --code-text:#d7d9e6; --on-solid:#0a0a10;
```

亮色（`:root[data-theme="light"]`）：

```
--bg:#f7f8fc; --bg-raise:#ffffff; --surface-1:#ffffff; --surface-2:#f4f5fa; --surface-3:#eceef6;
--line:rgba(17,19,28,.08); --line-strong:rgba(17,19,28,.15);
--text:#11131c; --muted:#5b6070; --faint:#8b90a1;
--accent:#5b5bd6; --accent-strong:#4a4ac4; --accent-soft:rgba(91,91,214,.10);
--success:#059669; --error:#e11d48; --warn:#b45309; --info:var(--accent);
--shadow-1:0 1px 2px rgba(16,24,40,.06); --shadow-2:0 8px 24px -10px rgba(16,24,40,.18); --shadow-3:0 24px 64px -20px rgba(16,24,40,.24);
--panel-overlay:#ffffff; --panel-modal:#ffffff; --code-bg:#f2f3f8; --code-text:#2a2e3d; --on-solid:#ffffff;
```

图表状态（两套主题各给值，供 graphs.js / ui.css `.wf-bar` / `.st-*` 使用）：

```
--st-pending/assigned/acked/running/succeeded/failed/canceled/abandoned/lease_expired
```

暗色沿用现有 9 色（`#71717a #3b82f6 #06b6d4 #22c55e #10b981 #ef4444 #a1a1aa #f97316 #eab308`），
亮色压深一档（如 `#52525b #2563eb #0891b2 #16a34a #059669 #dc2626 #71717a #ea580c #ca8a04`）。

### 3.2 尺寸/动效 token（新）

```
--r-xs:6px; --r-sm:8px; --r-md:12px; --r-lg:16px; --r-xl:20px; --r-full:999px;
--dur-fast:120ms; --dur:200ms; --dur-slow:320ms; --ease:cubic-bezier(.2,.8,.2,1);
--sidebar-w:264px; --sidebar-w-collapsed:76px; --topbar-h:64px;
--z-header:20; --z-overlay:30; --z-sidebar:40; --z-menu:60; --z-toast:80; --z-modal:90;
--font-sans:"Inter",system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;
--font-mono:"JetBrains Mono",ui-monospace,SFMono-Regular,Menlo,monospace;
```

> `ui.css` 里现有 26 种字号、22 种圆角不强制一次收敛；但**新增/重写**的组件必须用 token。
> 修掉引用不存在 token 的声明（`--mono`/`--fg`/`--danger`/`--ok`/`--text-muted`/`--panel-hover`）。

## 4. 外壳规范（`partials.js` + `ui.css` + layouts）

### 4.1 侧栏
- 宽度 `var(--sidebar-w)`，折叠时 `var(--sidebar-w-collapsed)`；折叠状态 `data-collapsed="true"` 挂在外壳根 `div`（`#app-shell`）上，CSS 驱动，**不重建 DOM、不删文字节点**（e2e 取 label）。
- 折叠按钮：侧栏品牌行右侧 `#sidebar-collapse`（桌面 `lg:` 显示，icon chevron）。`aria-expanded`、`title` 走 i18n。
- 折叠态：图标居中、隐藏 `.side-label`、隐藏文字与 scope 文本（只留头像/首字母）、hover 显示 tooltip（CSS `::after` 或 title 属性）。
- 导航分组：Workspace（overview/events/sources/capabilities/edges/playbooks/nodes）、Resources（keys/docs/security/admin）。改 `NAV` 常量即可，保留所有 id/href。
- active：左侧 2px accent 条 + accent-soft 背景 + `aria-current="page"`。
- footer：语言切换 + 用户块（紧凑，折叠态只留头像）。
- 移动端：维持抽屉 + overlay；新增 Esc 关闭、打开时 `body.overflow=hidden`（关闭恢复）、`aria-expanded`/`aria-controls`、关闭后焦点回到 `#menu-btn`；点击 `.side-link` 自动关闭。

### 4.2 顶栏
- 高 `var(--topbar-h)`，`backdrop-filter` + `color-mix(bg 82%)`；左侧：`#menu-btn`（移动）、页面标题 h1 + subtitle（现状保留，subtitle 在 1280 以上显示）。
- 右侧：主题切换 `#theme-toggle`（三态 auto/dark/light：点击循环；`aria-label` + `title` 走 i18n；icon sun/moon/monitor）、通知铃、头像按钮 `#topbar-avatar`（改为 `<button>`，点击开菜单：账户安全/管理后台(admin)/退出登录；Esc 关闭，点击外部关闭）。
- 保留 `#topbar-avatar` 这个 id 与 `#scope-switcher` 的层级关系（z-index 见 token）。

### 4.3 页头/容器
- 新增组件类（ui.css）：`.page`（`max-width:1200px; margin-inline:auto; display:grid; gap:24px`）、`.page-head`、`.page-title`、`.page-sub`、`.page-actions`。
- 保留各页现有 `mx-auto max-w-6xl space-y-8` 结构可正常工作；新页头类先在 index/events 用，wave 2 推广。

## 5. 组件规范（ui.css，新类 + 旧类保留）

旧类**只允许重写样式，不允许删除/改名**（页面与 JS 都依赖）。新增：

- 卡片：`.card`（surface-1 + line + r-lg + shadow-1）；`.card-hover`；`.card-pad`（20/24）；`.card-head`/`.card-title`/`.card-sub`/`.card-actions`。
- 统计卡：`.stat`、`.stat-icon`、`.stat-label`、`.stat-value`、`.stat-delta.up/.down`、`.stat-spark`。
- 按钮：`.btn`（基类）+ `.btn-primary`（accent 填充 + `--on-solid` 文字）、`.btn-ghost`、`.btn-danger`、`.btn-sm`、`.btn-icon`；全局 `:focus-visible` 环。
  - 兼容：`a.btn-primary` 现有样式用途（e2e 取 `a.btn-primary` href），保持 `display:inline-flex`。
- 徽章：`.badge` + `.badge-neutral/.badge-accent/.badge-success/.badge-warn/.badge-error`；保留 `.status-badge/.status-*`、`.tag-chip`、`.src-badge`、`.target-pill`。
- 表格：`.table-wrap`、`.data-table`（th 粘顶、行 hover、数字右对齐、`.table-empty`）。admin/keys/docs 逐步替换，wave 1 只提供样式。
- 表单：`.input`/`.select`/`.textarea` 统一 36px 高、focus ring、placeholder 用 `--faint`；`.field`/`.field-label`/`.field-hint`/`.field-error`。
- 导航/tab：`.tabs`、`.tab`、`.tab.active`；保留 `.filter-tab`、`.seg-switch`、`.channel-tab`。
- 浮层：`.modal-backdrop`、`.modal-panel`、`.modal-head`、`.modal-body`、`.modal-foot`；`.toast`（统一，wave 2 再删各页副本）；`.menu`/`.menu-item`（顶栏头像菜单、下拉复用一个样式族）。
- 状态：`.empty`（icon + 标题 + 说明 + action）、`.skeleton`、`.kbd`（复活）。
- 列表行：`.list`/`.list-row`/`.list-main`/`.list-meta`（替代散落的 `divide-y` 拼装）；`.notif-row` 保留并重写为紧凑 56px 行。

## 6. 页面波次与文件归属

### Wave 1（本轮，impl centurion）
`web/tokens.css`、`web/ui.css`、`web/partials.js`、`web/graphs.js`、`web/traceview.js`、
`web-src/layouts/{base,login,focus}.html`、`web-src/pages/index.html`、`web-src/pages/events.html`、
`web-src/locales/{zh-CN,en,ja,es}.yaml`（仅新增 key）。

### Wave 2（下一轮，按页归属，共享文件仍归 impl 主 owner）
- A：sources / source / capabilities / edges
- B：nodes / node / keys / admin / receivers
- C：playbooks / playbook / trace / docs / message
- D：login / cli-auth / passkey-enroll / security

### 不动的
`internal/**`（除生成的 dist，由 `make fe` 重写）、`scripts/e2e/**`、`scripts/check-classes.mjs`
（除非新增合法 Tailwind 工具类被误判，需先向协调者报）、`api/**`、`design/` 既有文件（design 只新增 `design/ui-v2/`）。

## 7. 门禁（每轮必跑）

```bash
node scripts/check-classes.mjs      # 新增组件类必须在 ui.css 定义
node scripts/check-tailwind-css.mjs # 新增 Tailwind 工具类 → 先 node scripts/build-tailwind.mjs
make sitegen                        # web-src → web/（会删除 stale 页）
make fe                             # 指纹 → internal/server/dist（e2e 用）
```

- 新增 Tailwind 工具类（含新变体/任意值）必须 `node scripts/build-tailwind.mjs`（会改写已提交的 `web/tailwind.css`，属预期）。
- 不得手改 `web/*.html`（生成物，gitignore）。
- 不得用硬编码色值（除 token 文件内的主题定义本身）。
- 改 DOM 前 `grep` 对应 e2e 断言；改完自测 `node scripts/e2e` 相关套件（可选），`make e2e` 由协调者跑。

## 8. 视觉语言

- 圆角 12/16；卡片 1px `--line` + `--shadow-1`；hover 提升到 `--shadow-2` + `--line-strong`。
- 留白：区块间 24px，卡片内 20–24px；列表行 52–60px；页面左右 24/32px。
- 字号：页标题 28/34 semibold（`font-display` 保留为品牌衬线，但只用于 h2 主标题与欢迎语）；正文 14；辅助 12–13。
- 颜色比例 70/20/10 不变；accent 只出现在主按钮、active、focus、关键数据、图标底。
- 动效：`--dur` + `--ease`；hover 位移 ≤2px；`prefers-reduced-motion` 全部停。
- 图表配色统一走 `--st-*`；不要第三套色板。

## 9. 失败处理
- 若发现契约与代码现实矛盾（例如某锚点必须改才能实现折叠），**停下来在产出里写明「契约矛盾 + 建议」**，不要自行解释。
- 若某页 e2e 锚点会因视觉改造失效，保留旧 DOM 作为视觉隐藏层，而不是删除锚点。

---

## 10. 契约修订 v1.1（协调者裁决，设计稿交付后）

依据 `design/ui-v2/REVIEW.md` §3/§6，裁决如下（后续 agent 以此为准）：

- **C1 采纳 D2**：顶栏 `header h1` 是**上下文标题**（15px/600），内容区页面大标题用
  `.page-title`（`<h2>`，28px Inter 600 `-0.02em`）。`header h1` 唯一性 + `.scope-badge` 位置不变。
- **C2 例外**：`login/cli-auth/passkey-enroll`（login/focus 布局、无 `#page-main`/mountLayout）**允许**多个 `<h1>`；
  `base` 布局的 17 个工作台页仍然禁止页面内容里出现 `<h1>`。
- **C3 采纳新增类**：`.page-narrow`（760px，node/source/trace/message）、`.card-fill`、`.card-foot`、
  `.side-text`、`.side-count`、`.st-text.<state>`、`.btn-quiet`、`.hm-scroll`、`.table-foot`、`.field*`、`.menu*`。
- **C4 更正事实**：`.toast` 页内副本实为 11 份（不是 7）；wave 2 统一到 `partials.js` 的 `Anotify.toast`。
- **字体**：Fraunces 只用于 ≥22px 的三类位置（登录大标题、欢迎语、章节主标题）；≤20px 一律 Inter。
- **KPI**：首页不允许出现空网格位。优先 4 张有真实数据的卡（可用已请求的 stats/nodes 数据，**不新增 API**）；
  推送类指标被隐藏时让网格自适应为 2 列，而不是留两个空位。
- **light 兼容 shim**：wave 1 允许在 `ui.css` 用 shim（把旧页的 `text-zinc-*`/`bg-white/*` 映射到 token）；
  wave 2 迁移页面后 shim 只能收缩，不能扩张。shim 必须放在 `ui.css` 末尾并带注释说明「临时兼容层」。
- **默认主题**：跟随系统；无法判定时用 dark。`theme-toggle` 顺序 dark → light → auto，`title`/`aria-label` 走 i18n。
- **设计稿位置**：`design/ui-v2/`（截图不入库；需要时 `node design/ui-v2/verify.mjs` 重新生成）。
