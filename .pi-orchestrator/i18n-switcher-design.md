# Anotify 语言切换器 · 设计规格

> **版本**：v1.1 · **状态**：待前端实现 · **产出人**：anotify-designer
>
> 实现者：anotify-frontend（照本稿实现，视觉/交互不擅自变更；实现细节冲突时以 tokens.css/ui.css 事实源为准并回报）
>
> **修订记录**
>
> | 版本 | 日期 | 变更摘要 |
> | --- | --- | --- |
> | v1.0 | 2025-07-15 | 初版：侧栏下拉 + 登录页紧凑平铺 |
> | v1.1 | 2025-07-15 | **用户实测反馈**：登录页平铺短标在语言多时拥挤不友好，统一改为下拉菜单形态（listbox 模式），与侧栏版形态统一。弹层方向改为向下（顶栏上方无空间），触发按钮宽度 `w-auto`。删除「登录页用平铺是有意差异」的论述，全站统一下拉。 |
>
> **前置事实**（来自侦察，实现时可直接依赖）：
>
> - 站点为纯静态多页，sitegen 构建期生成；模板数据仅有 `{{.Lang}}`（当前语言码）与 `{{.LangPrefix}}`（URL 前缀，默认语言为 `""`，其余为 `/{lang}`）。
> - 语言 URL 方案：默认语言 zh-CN 在根路径（`/index.html`），其余语言在 `/{lang}/`（`/en/index.html`）。
> - 导航链接由 `web/partials.js` 运行时以**相对 href**（`"index.html"`）注入，因此在 `/en/` 下自动解析为 `/en/index.html` —— 语言内导航无需改动。
> - `partials.js` 已导出 `t(key, fallback)` 与 `el()` 安全 DOM 构建器，语言切换器必须复用，禁止 innerHTML。
> - 现有页面**无** hreflang alternate 链接，本稿新增。

---

## 1. 目标与非目标

### 1.1 目标

1. 用户在任意页面可一键切换到 4 种语言：中文（zh-CN，默认）、English（en）、日本語（ja）、Español（es）。
2. 切换后停留在**同一页面**（而非回首页），URL 规则可推导、可分享、可被搜索引擎索引。
3. 无 JS 时切换器仍可用（纯链接跳转，渐进增强）。
4. 视觉与交互完全贴合 Anotify 深色工作台语言，不引入新色相、不硬编码色值。

### 1.2 非目标

- 不做浏览器语言自动重定向（私有化部署场景，用户显式选择优先；避免与 SEO/可分享性冲突）。
- 不做语言偏好持久化（无 cookie/localStorage 写入；URL 即状态，刷新/分享天然保留）。
- 不翻译动态后端数据（通知正文等用户内容保持原文）。

---

## 2. 关键决策：交互形态 = 下拉菜单（全站统一）

### 2.1 结论

**全站统一采用「触发按钮 + 下拉菜单」**（listbox 模式），包括侧栏与登录页。

> **v1.1 变更**：初版设计中登录页使用紧凑平铺（4 短标横排），用户实测后反馈语言一多即拥挤、不友好。统一为下拉菜单形态，与侧栏版一致。

### 2.2 理由

| 维度 | 平铺链接 | 下拉菜单（选用） |
| --- | --- | --- |
| 4 语言占位 | 侧栏底部需占 4 行或拥挤横排，挤压「私有化部署 · 主工作台」标注 | 仅占 1 行触发位，菜单按需展开 |
| 扩展性 | 5+ 语言即失控 | 任意增加语言，UI 不变 |
| 当前语言可见性 | 需在 4 项中高亮，视觉噪音 | 触发按钮直接显示当前语言（如「中文」），一眼可知 |
| 无 JS 降级 | 天然可用 | 触发按钮本身是链接到「当前页其他语言」的回退方案（见 §6.2），仍可用 |
| 移动端（抽屉侧栏） | 横排在窄抽屉中折行 | 下拉在抽屉内正常展开 |
| **登录页实测** | **v1.0 平铺在 4 语言时已拥挤，用户反馈不友好** | **v1.1 统一下拉，触发按钮紧凑，弹层按需展开** |

### 2.3 语言显示名（固定文案，不走 i18n key，各语言版本中显示一致）

| 语言码 | 显示名（原生语） | 短标（触发按钮窄位用） |
| --- | --- | --- |
| zh-CN | 中文 | 中文 |
| en | English | EN |
| ja | 日本語 | 日本語 |
| es | Español | ES |

> 用原生语显示是行业惯例（用户在自己不认识的语言界面里也能找到自己的语言）。

---

## 3. 信息架构与位置

### 3.1 主工作台（base.html · 侧边栏）

**位置：侧边栏最底部，用户卡片区之上。**

现有侧栏底部结构（partials.js 注入）：

```
┌─────────────────────────┐
│ [导航组…]                │  ← nav, flex-1 overflow-y-auto
│                         │
├─────────────────────────┤  ← border-t
│ [头像] 用户名      ●    │  ← 用户卡片（现有）
│      私有化部署·主工作台  │
└─────────────────────────┘
```

**改为：**

```
┌─────────────────────────┐
│ [导航组…]                │
│                         │
├─────────────────────────┤  ← border-t（现有）
│ 🌐 中文              ▾  │  ← 【新增】语言切换器（独立一行）
├─────────────────────────┤  ← border-t（新增，或改用 mt 间距）
│ [头像] 用户名      ●    │
│      私有化部署·主工作台  │
└─────────────────────────┘
```

- 切换器是**独立一行**，与导航项同宽（`px-3` 容器内全宽），视觉上属于「账户/设置」区域而非导航。
- 与下方用户卡片之间用 `border-t border-white/[0.05]` 分隔（与用户卡片现有 `border-t` 一致），保持节奏。
- **不放顶栏**：顶栏右侧已有通知铃 + 头像，且顶栏 `h-16` 紧凑，再加控件显挤；侧栏底部是「设置类」控件的自然归属。

### 3.2 登录页（login.html · 无侧边栏）

**位置：顶栏右侧，tagline 之前。**

现有登录页顶栏：

```
[Logo Anotify]                    Agent 完成即通知
```

**改为：**

```
[Logo Anotify]           🌐 中文 ▾   Agent 完成即通知
                         ↑ 新增        ↑ 现有 tagline（sm 以下隐藏）
```

- 切换器靠右，在 tagline（`{{t "common.brand.tagline"}}`）**左侧**，`gap-3`。
- tagline 在移动端（<640px）`hidden`（现为 `text-sm text-zinc-500`，需加 `hidden sm:block` 让步于切换器——登录页首屏移动端空间宝贵，切换器优先级高于 tagline）。
- 登录页是用户**首次**接触界面、且可能面对不熟悉语言的地方，切换器必须首屏可见、无需滚动。
- **v1.1**：切换器形态与侧栏版统一为「触发按钮 + 下拉菜单」，弹层**向下**弹出（顶栏上方无空间；侧栏版向上）。

---

## 4. 视觉方案（逐区块）

### 4.1 触发按钮（两处共用样式）

**结构语义**：`<button>`（JS 增强）包裹的 `<a>` 回退（无 JS）。实际实现见 §6 —— sitegen 输出 `<a>`，partials.js 增强为 `<button aria-expanded…>`。

```
┌──────────────────────────────┐
│ 🌐  中文                  ▾  │
└──────────────────────────────┘
```

| 属性 | 规格 |
| --- | --- |
| 布局 | `flex w-full items-center gap-2.5 rounded-lg px-3 py-2`（侧栏版）；登录页版 `w-auto` |
| 图标 | 地球图标（globe），`h-4 w-4`，`color: var(--faint)` → hover 时 `var(--muted)` |
| 当前语言名 | `text-sm`，`color: var(--muted)` → hover `var(--text)` |
| 展开箭头 | chevron-down，`h-3.5 w-3.5 ml-auto`（侧栏版把箭头推右；登录页版 `ml-1`），`color: var(--faint)`；菜单展开时旋转 180°（`transition-transform`） |
| 背景/边框 | 默认透明 + `border border-transparent`；hover：`background: var(--surface-2)`；focus-visible：`outline: 2px solid var(--accent)`（`outline-offset: 2px`） |
| 字体 | Inter（继承 body），语言名**不**用 font-display（小字号功能文本） |

> **红线遵守**：地球图标不用 emoji 🌐（渲染随平台漂移），用 SVG（复用 partials.js `icon()` 风格，stroke currentColor）：
>
> ```svg
> <circle cx="12" cy="12" r="10"/>
> <path d="M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/>
> ```

### 4.2 下拉菜单（弹出层）

**定位**：

- **侧栏版**：触发按钮**上方**弹出（侧栏在底部，向上展开避免溢出视口底）。
- **登录页版**：触发按钮**下方**弹出（顶栏上方无空间）。

```
   ┌────────────────────────────┐
   │ 🌐  中文                ▲  │  ← 触发按钮（展开态箭头翻转）
   └────────────────────────────┘
        ┌────────────────────────┐
        │ ✓ 中文          (当前)  │  ← 当前语言：accent 高亮
        │   English              │
        │   日本語               │
        │   Español              │
        └────────────────────────┘
```

> 侧栏版菜单在按钮**上方**弹出；登录页版菜单在按钮**下方**弹出（上图即登录页方向）。

| 属性 | 规格 |
| --- | --- |
| 容器 | `card`（复用现有类：background var(--surface-1), border var(--line), radius 16px）+ `style="background: var(--panel-overlay)"` 覆盖（弹层需实底，复用 tokens 的 `--panel-overlay`，与 Toast 同底）+ `p-1.5` + `shadow-2xl` + `min-w-[160px]` |
| 每项 | `<a>`，`flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm`；默认 `color: var(--muted)`；hover `background: var(--surface-2)` + `color: var(--text)` |
| 当前语言项 | `color: var(--text)` + `background: var(--accent-soft)` + 左侧 `✓`（SVG check，`h-3.5 w-3.5`，`color: var(--accent)`）；非当前项左侧留 `w-3.5` 占位对齐 |
| 每项右侧（可选增强） | 短标 `text-[11px] color: var(--faint)`（如「中文」项右侧显示「zh-CN」）——mono 字体，辅助识别，可省略 |
| 层级 | `z-50`（高于侧栏 z-40、低于 toast z-80） |
| 动画 | `opacity-0 → opacity-100` + `translate-y-1 → 0`（侧栏版，向上弹出）/ `translate-y-[-4px] → 0`（登录页版，向下弹出），150ms ease-out（无 prefers-reduced-motion 时）；reduced-motion 下仅淡入无位移 |
| 溢出保护 | `max-height: 60vh; overflow-y: auto`（矮视口/横屏手机抽屉内菜单过高时可滚动，不顶破视口） |

### 4.3 状态规格汇总

| 状态 | 触发按钮 | 菜单项 |
| --- | --- | --- |
| 默认 | muted 文字，透明底 | muted 文字 |
| hover | `var(--surface-2)` 底，`var(--text)` 文字 | `var(--surface-2)` 底 |
| focus-visible（键盘） | `outline: 2px solid var(--accent)`，offset 2px | 同左 |
| 当前语言 | — | `var(--accent-soft)` 底 + ✓ + `aria-current="true"` |
| 展开 | 箭头旋转 180°，`aria-expanded="true"` | — |
| 禁用/加载 | 无此态（纯链接，无加载过程） | — |

---

## 5. 交互流程

### 5.1 主流程

1. 用户点击触发按钮（或键盘 Enter/Space/↓ 聚焦激活）→ 菜单展开，`aria-expanded=true`，焦点移到**当前语言项**（无 JS 时见 §6.2 降级）。
2. 用户点击目标语言（或方向键导航 + Enter）→ 跳转到目标语言的**同一页** URL。
3. 菜单外点击 / `Esc` / 选择后 → 菜单关闭，焦点归还触发按钮。

### 5.2 键盘操作（listbox 模式，遵循 WAI-ARIA Authoring Practices）

| 键 | 行为 |
| --- | --- |
| `Tab` | 焦点进入/离开触发按钮（菜单关闭时）；菜单展开时 Tab 关闭菜单并移到下一焦点 |
| `Enter` / `Space` / `↓`（按钮上） | 展开菜单 |
| `↑` / `↓`（菜单内） | 在语言项间移动焦点（循环） |
| `Home` / `End` | 跳到首/末项 |
| `Enter`（菜单项上） | 激活（本质是 `<a>`，天然支持） |
| `Esc` | 关闭菜单，焦点归还按钮 |

### 5.3 无障碍标注（必须实现）

- 触发按钮：`aria-haspopup="true"` `aria-expanded="false|true"` `aria-label="{{t "common.lang.switcher_label"}}"`（值如「切换语言，当前：中文」）。
- 菜单容器：`role="menu"`（或 `role="listbox"`）`aria-orientation="vertical"`。
- 菜单项：`role="menuitem"`；当前项加 `aria-current="true"`。
- 每项 `<a hreflang="{目标语言码}" lang="{目标语言码}">`（声明目标语言，助屏幕阅读器正确发音，也是 SEO 信号）。
- 菜单展开状态变化用 `aria-expanded` 表达，无需额外 live region（状态在按钮上，屏幕阅读器自动播报）。

### 5.4 空态 / 降级态

| 场景 | 表现 |
| --- | --- |
| **无 JS**（核心降级） | sitegen 输出的是 `<a>` 列表（见 §6.2），触发位是「跳到下一种语言」的链接，点击可循环切换；或更稳妥：无 JS 时直接平铺显示所有语言链接（用 `<noscript>` 或在 JS 挂载时把平铺替换为下拉——推荐后者：**构建期渲染平铺链接，JS 挂载后增强为下拉**）。 |
| **单语言构建**（langs 只有 zh-CN） | sitegen 不渲染切换器（模板内 `{{if}}` 判断语言数 >1）。 |
| **file:// 直开** | 链接仍为相对路径，同目录/子目录跳转有效；菜单 JS 如未加载则保持平铺态，可用。 |
| **后端未连接/演示模式** | 与切换器无关（纯静态跳转），天然免疫。 |

---

## 6. 落地规格（可直接照做）

### 6.1 URL 推导规则（同一页面跨语言）

当前页 URL 推导目标语言 URL = **替换路径中的语言前缀**：

| 当前页 | 目标 en | 目标 zh-CN（默认） | 目标 ja |
| --- | --- | --- | --- |
| `/index.html` | `/en/index.html` | `/index.html` | `/ja/index.html` |
| `/en/keys.html` | `/en/keys.html`（不变） | `/keys.html` | `/ja/keys.html` |
| `/ja/docs.html#scheme` | `/en/docs.html#scheme` | `/docs.html#scheme` | 不变 |

**规则**（实现时按此）：

1. 取当前 `location.pathname`。
2. 若首段匹配已知语言码（`en`/`ja`/`es`），剥掉该首段 → 得到「页路径」（如 `/keys.html`）；否则 pathname 本身即页路径（当前是默认语言）。
3. 目标 URL = `目标LangPrefix + 页路径 + location.search + location.hash`。
   - 目标为默认语言：`LangPrefix=""` → `/keys.html`
   - 否则：`LangPrefix="/en"` → `/en/keys.html`
4. **保留 query 与 hash**（如 `?msg=ntf_xx`、`#scheme`）——深链不丢。

**构建期 vs 运行时分工**：

- 构建期（sitegen）：`{{.LangPrefix}}` 已知，可直接算出「当前页去掉前缀后的页路径」吗？——sitegen 的 PageData 没有 `PageName`，**建议扩展 PageData 增加 `Page string`**（见 §8 给后端的建议），即可在模板里直接 `href="{{$.LangPrefix}}/{{$.Page}}"`。
- 若不改 sitegen：菜单项 href 用相对路径 + 运行时 JS 按 §6.1 规则推导（partials.js 挂载着生成分支）。**推荐扩展 sitegen**，理由：无 JS 降级链接才能精确指向同页，而非首页。

### 6.2 构建期渲染的 HTML（sitegen 模板片段）

**前提**：PageData 增加 `Page string`（当前页文件名，如 `keys.html`）与 `Langs []LangInfo`（语言列表，每项 `{Code, Prefix, NativeName, ShortName}`）。当前语言项用 `{{if eq .Code $.Lang}}` 判断。

#### (a) 侧栏版（base.html，在 nav 之后、用户卡片之前插入）

```html
{{if gt (len .Langs) 1}}
<!-- 语言切换器：构建期平铺（无 JS 可用），JS 挂载后增强为下拉 -->
<div id="lang-switcher" class="border-t border-white/[0.05] px-3 pt-3">
  <div class="side-label" id="lang-switcher-label">{{t "common.lang.label"}}</div>
  <div class="space-y-0.5" data-lang-list>
    {{range .Langs}}
    <a
      href="{{.Prefix}}/{{$.Page}}"
      hreflang="{{.Code}}"
      lang="{{.Code}}"
      class="lang-link flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm {{if eq .Code $.Lang}}lang-current{{end}}"
      {{if eq .Code $.Lang}}aria-current="true"{{end}}
    >
      <span class="lang-check w-3.5">{{if eq .Code $.Lang}}✓{{end}}</span>
      <span>{{.NativeName}}</span>
      {{if eq .Code $.Lang}}<span class="sr-only">{{t "common.lang.current"}}</span>{{end}}
    </a>
    {{end}}
  </div>
</div>
{{end}}
```

> 说明：✓ 处实际用 SVG check 图标（视觉稿 §4.2），此处简写。`lang-current` 类样式放 ui.css（见 §6.4），不用行内色值。

#### (b) 登录页版（login.html，header 右侧）

**v1.1 变更**：登录页构建期模板从「紧凑平铺短标」改为与侧栏版同构的平铺列表（外层容器 id 为 `lang-switcher-login` 以区分），JS 挂载后增强为下拉菜单。无 JS 降级基础保留。

```html
{{if gt (len .Langs) 1}}
<!-- 语言切换器（登录页）：构建期平铺（无 JS 可用），JS 挂载后增强为下拉 -->
<div id="lang-switcher-login" class="flex items-center">
  <div class="space-y-0.5" data-lang-list>
    {{range .Langs}}
    <a
      href="{{.Prefix}}/{{$.Page}}"
      hreflang="{{.Code}}"
      lang="{{.Code}}"
      class="lang-link flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm {{if eq .Code $.Lang}}lang-current{{end}}"
      {{if eq .Code $.Lang}}aria-current="true"{{end}}
    >
      <span class="lang-check w-3.5">{{if eq .Code $.Lang}}✓{{end}}</span>
      <span>{{.NativeName}}</span>
      {{if eq .Code $.Lang}}<span class="sr-only">{{t "common.lang.current"}}</span>{{end}}
    </a>
    {{end}}
  </div>
</div>
{{end}}
```

> **与侧栏版的差异**：仅外层容器 id 不同（`lang-switcher-login` vs `lang-switcher`），内部 `data-lang-list` 结构完全一致。`partials.js` 的 `mountLangSwitcher` 需同时查找两处并增强（见 §6.3）。

#### (c) head 中的 hreflang alternate（两个 layout 都加）

```html
{{range .Langs}}
<link rel="alternate" hreflang="{{.Code}}" href="{{.Prefix}}/{{$.Page}}" />
{{end}}
<link rel="alternate" hreflang="x-default" href="/{{$.Page}}" />
```

> `x-default` 指向默认语言版。注意：生产环境若部署在域名根，这里用相对路径即可被搜索引擎解析；若有正式域名，建议 sitegen 支持传入 `BaseURL` 生成绝对 URL（SEO 最佳实践要求绝对 URL），列为 §8 的可选增强。

### 6.3 partials.js 运行时增强（JS 把平铺升级为下拉）

**职责**：找到 `#lang-switcher [data-lang-list]`（侧栏版）与 `#lang-switcher-login [data-lang-list]`（登录页版），将其重构为「触发按钮 + 弹层菜单」。

```
函数：Anotify.mountLangSwitcher()
  1. 依次查找两处宿主：
     a. const sidebarHost = document.getElementById("lang-switcher")
     b. const loginHost = document.getElementById("lang-switcher-login")
     两处都不存在则 return（单语言构建）
  2. 对每处宿主执行增强（逻辑相同，仅定位方向不同）：
     - 读取 [data-lang-list] 内所有 <a>，提取 {code, nativeName, href, isCurrent}
     - 构建触发按钮（§4.1 规格）：globe SVG + 当前语言名 + chevron
       · aria-haspopup="true" aria-expanded="false"
       · aria-label = t("common.lang.switcher_label", "切换语言")
       · 侧栏版：class 含 w-full；登录页版：class 含 w-auto
     - 构建菜单容器（§4.2 规格）：role="menu"，复用 <a>（保留 href/hreflang/aria-current），
       加 role="menuitem"
       · 侧栏版定位：absolute bottom-full left-0 right-0 mb-2（向上弹出）
       · 登录页版定位：absolute top-full right-0 mt-2（向下弹出，右对齐）
     - 用菜单替换平铺列表；按钮点击 toggle；外挂 §5.2 键盘逻辑 + 菜单外点击/Esc 关闭
  3. 导出到 window.Anotify.mountLangSwitcher，并在 mountLayout() 末尾自动调用
     （mountLayout 已负责侧栏组装，切换器是其一部分）
  4. 登录页无 mountLayout，需在 login.html 的 <script> 块中显式调用
     Anotify.mountLangSwitcher()（或由 partials.js 自动检测登录页并执行）
```

**菜单定位（两处差异）**：

| 位置 | 触发按钮容器 | 菜单定位 |
| --- | --- | --- |
| 侧栏版（向上弹出） | `relative` | `absolute bottom-full left-0 right-0 mb-2 z-50` |
| 登录页版（向下弹出） | `relative` | `absolute top-full right-0 mt-2 z-50`（右对齐，避免超出视口右侧） |

窄屏抽屉内（侧栏版）同样成立（抽屉即侧栏）。登录页在移动端同样向下弹出。

### 6.4 ui.css 新增样式（只用 tokens 变量，不硬编码色值）

```css
/* 语言切换器（构建期平铺态 + JS 增强后的下拉态共用） */
.lang-link {
 color: var(--muted);
 transition: all 0.2s ease;
}
.lang-link:hover {
 color: var(--text);
 background: var(--surface-2);
}
.lang-link.lang-current {
 color: var(--text);
 background: var(--accent-soft);
}
.lang-check {
 color: var(--accent);
 font-size: 0.75rem;
 text-align: center;
 flex-shrink: 0;
}

/* 触发按钮（JS 增强后） */
.lang-trigger {
 /* 复用 side-link 视觉但不带导航语义；或直接 class="side-link" 复用现有类 */
 color: var(--muted);
 border: 1px solid transparent;
 transition: all 0.2s ease;
 cursor: pointer;
}
.lang-trigger:hover {
 color: var(--text);
 background: var(--surface-2);
}
.lang-trigger:focus-visible {
 outline: 2px solid var(--accent);
 outline-offset: 2px;
}
.lang-trigger[aria-expanded="true"] .lang-chevron {
 transform: rotate(180deg);
}
.lang-chevron {
 color: var(--faint);
 transition: transform 0.2s ease;
}

/* 弹层 */
.lang-menu {
 background: var(--panel-overlay);
 border: 1px solid var(--line);
 border-radius: 12px;
 box-shadow: 0 16px 40px rgba(0, 0, 0, 0.5);
}
.lang-menu-item {
 transition: all 0.15s ease;
}
.lang-menu-item:hover {
 background: var(--surface-2);
 color: var(--text);
}
.lang-menu-item:focus-visible {
 outline: 2px solid var(--accent);
 outline-offset: 2px;
}
```

> **v1.1 删除**：`.lang-link-login` 系列样式不再使用（登录页改为统一下拉，构建期平铺列表复用 `.lang-link` 样式）。若保留旧类名作为遗留兼容，标注 `@deprecated`。

### 6.5 i18n key（按 I18N_KEYS.md 规范，加入 locales/*.yaml 的 common 节）

```yaml
common:
  lang:
    label: "语言"                # zh-CN；en: "Language"；ja: "言語"；es: "Idioma"
    switcher_label: "切换语言"    # aria-label；en: "Switch language"
    switcher_aria: "语言选择"     # 登录页 nav aria-label；en: "Language selection"
    current: "当前语言"           # sr-only；en: "current language"
```

> 语言显示名（中文/English/日本語/Español）**不走 i18n key**（§2.3），由 sitegen 的 Langs 列表自带 NativeName。

---

## 7. 响应式行为

| 视口 | 侧栏版 | 登录页版 |
| --- | --- | --- |
| 桌面 1280 | 侧栏固定展开，切换器在侧栏底部常态可见；菜单向上弹出，宽度 = 侧栏内宽（`left-0 right-0`） | 顶栏右侧触发按钮（`w-auto`），菜单向下弹出，右对齐（`right-0`），`min-w-[160px]` |
| 移动 390 | 侧栏为抽屉（menu-btn 触发），切换器随抽屉底部出现；菜单在抽屉内向上弹出，宽度 = 抽屉内宽；选择/点击遮罩后抽屉关闭（复用现有 closeSidebar 逻辑） | tagline `hidden sm:block` 让位；触发按钮保持可见（`px-3 py-2` 满足触控目标）；菜单向下弹出，右对齐，宽度 `min-w-[160px]`，不超出视口右侧 |

**移动端触控目标**：所有可点项（触发按钮、菜单项）最小触控高度 36px（Tailwind `py-2`+文字行高已满足）。

---

## 8. 给后端/sitegen 的依赖项（非前端职责，需协调）

| 项 | 说明 | 优先级 |
| --- | --- | --- |
| PageData 增加 `Page string` | 当前页文件名，供模板算同页跨语言 URL（无 JS 降级的前提） | **必须** |
| PageData 增加 `Langs []LangInfo`（`{Code, Prefix, NativeName, ShortName}`） | 模板遍历渲染语言列表；NativeName/ShortName 表见 §2.3，可内置在 sitegen | **必须** |
| 模板函数 `len`、`eq` | html/template 内置已有，无需新增 | — |
| `langs` 扩为 `zh-CN,en,ja,es`（Makefile + cmd/sitegen 默认）+ 新增 ja.yaml/es.yaml | 4 语言构建 | 由翻译任务跟进 |
| （可选）PageData 增加 `BaseURL` | hreflang 用绝对 URL，SEO 最佳 | 低 |

---

## 9. 验收要点（供 tester/reviewer 核对）

1. 4 语言构建后，每个页面（含 /en、/ja、/es 前缀页）都有切换器；无 JS（禁用 JS 或用 curl 看 HTML）时语言链接为有效 `<a>` 且指向**同页**目标语言 URL。
2. 切换语言后停留在同页，query/hash 保留（`/en/keys.html?x=1#top` → 切中文 → `/keys.html?x=1#top`）。
3. **侧栏版**在 1280 与 390（抽屉内）均可用；菜单向上弹出不溢出视口。**登录页版**在 1280 与 390 均可用；菜单向下弹出，右对齐不超出视口右侧。
4. 键盘：Tab 到触发按钮 → Enter 展开 → ↑↓ 导航 → Enter 跳转；Esc 关闭且焦点归还。
5. 屏幕阅读器：按钮播报「切换语言」（aria-label）+ 展开状态；当前语言项播报 aria-current。
6. head 中每页有 4 条 hreflang alternate + 1 条 x-default。
7. 全站无新增硬编码色值（grep 新代码无 `#[0-9a-fA-F]`，除 SVG 图标 stroke=currentColor）。
8. 单语言构建（langs=zh-CN）时切换器完全不渲染。
9. **v1.1 新增**：登录页切换器为下拉菜单形态（非平铺），触发按钮显示当前语言名 + chevron，点击后展开菜单，菜单项含 ✓ 标记当前语言。

---

## 10. 已识别的实现陷阱（来自侦察，前端注意）

1. **导航相对 href 已正确**：partials.js 的 NAV 用 `"index.html"` 相对路径，/en/ 下自动正确——**不要**给导航加 `{{.LangPrefix}}` 改绝对路径，否则默认语言页会错（prefix 为 ""）。语言切换器是**唯一**需要感知 LangPrefix 的组件。
2. **挂载顺序**：`mountLangSwitcher` 依赖构建期渲染的 `#lang-switcher` / `#lang-switcher-login` DOM。侧栏版必须在 `mountLayout` 把侧栏插入 body **之后**调用（mountLayout 内部末尾调即可）；登录页版无 mountLayout，需在 login.html 的 `<script>` 块中显式调用 `Anotify.mountLangSwitcher()`（或由 partials.js 自动检测登录页并执行）。
3. **html/template 自动转义**：`hreflang`/`lang` 属性值安全（语言码白名单来自 sitegen，非用户输入），无 XSS 面；但 NativeName 含非 ASCII（日本語），确认 sitegen 输出 UTF-8 且页面 `<meta charset="UTF-8">` 已在（已有）。
4. **不要 innerHTML**：partials.js 用 `el()` 构建，切换器增强逻辑同样用 `el()`，遵守现有 XSS 防线。
5. **v1.1 新增**：登录页版弹层向下弹出时注意右对齐（`right-0`），避免在窄视口下菜单右缘超出屏幕。
