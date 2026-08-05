# 首页语言提示横幅 · 设计规格（lang-hint-design）

> 版本：v1 · 2026-08 · 作者：anotify-designer
> 实现方：anotify-frontend（照稿实现，不擅自改设计）
> 事实源：`web/tokens.css`、`web/partials.js`、`web/i18n.*.js`、`web/index.html` / `web/login.html`

---

## 1. 目标 / 非目标

### 目标

- 当访客的浏览器语言偏好与当前页面语言**不一致**、且站点**已构建该语言版本**时，在 `index.html` 与 `login.html` 两页顶部显示一条**可一键跳转**的细横幅，把访客引到其偏好语言的同一页面。
- 用**目标语言**写提示文案（让目标用户看得懂），而非当前页面语言。
- 纯客户端渐进增强：无 JS / 单语言构建 / i18n 未加载时**完全不出现**，不破坏任何现有功能。
- 尊重无障碍：键盘可达、Esc 关闭、屏幕阅读器用正确语音朗读目标语言文案、`prefers-reduced-motion` 降级。

### 非目标

- **不**做服务器端嗅探 / 不根据 IP 或 `Accept-Language` 在服务端重定向（静态站点，纯客户端判断）。
- **不**持久化「已关闭」状态（每次加载重新判断，见 §10 边界态——有意为之）。
- **不**覆盖 `receivers / keys / security / docs / message` 等需登录的工作台页面（本期只做 `index` + `login`）。
- **不**自动跳转（只提示，不替用户做决定）。
- **不**引入新组件体系 / 新色值 / 构建框架（全部复用 tokens + 现有惯例）。

---

## 2. 触发与检测逻辑

### 2.1 数据来源

- **可切换语言清单**：页面 `<head>` 里的 `<link rel="alternate" hreflang="..." href="...">`（构建期由 sitegen 从 `.Langs` 渲染，是语言列表的唯一事实源）。**排除 `hreflang="x-default"`**（它是回退指向，不是独立语言）。
- **当前页面语言**：`document.documentElement.lang`（如 `zh-CN`、`en`、`ja`、`es`）。
- **用户语言偏好**：`navigator.languages`（有序数组），回退 `navigator.language`。

### 2.2 检测伪代码

```
function detectTargetLang():
    # 1) 构建 可用语言 code→href 映射（排除 x-default）
    alternates = {}
    for link in document.head.querySelectorAll('link[rel="alternate"][hreflang]'):
        code = link.hreflang
        if code.toLowerCase() == 'x-default': continue
        alternates[code] = link.href
    if alternates 为空: return null        # 单语言构建 → 不出横幅

    current = normalize(document.documentElement.lang)   # 小写，如 'zh-cn'

    # 2) 取用户偏好语言列表（小写归一化）
    prefs = (navigator.languages 或 [navigator.language]) map toLowerCase

    # 3) 逐个偏好：先精确匹配，再按主语言前缀匹配
    for pref in prefs:
        # 精确匹配（如 'ja' == 'ja'）
        for code in alternates:
            if normalize(code) == pref and normalize(code) != current:
                return { code, href: alternates[code] }
        # 前缀匹配（如 'zh-tw' → 主语言 'zh' → 命中 'zh-cn'）
        primary = pref.split('-')[0]
        for code in alternates:
            if normalize(code).split('-')[0] == primary and normalize(code) != current:
                return { code, href: alternates[code] }

    return null   # 无匹配 / 只匹配到当前语言 → 不出
```

### 2.3 关键决策（写明，勿改）

- **归一化**：所有比较都先 `toLowerCase()`（`zh-CN` / `zh-cn` 视为同一）。
- **前缀匹配是有意决策**：`zh-TW`（繁中）会命中 `zh-CN`（简中），因为站点只有简中；同理 `en-US`/`en-GB` 命中 `en`、`es-MX` 命中 `es`。宁可提示「有相关语言」也不沉默。
- **匹配到当前语言 → 不出**（用户已在偏好语言页，无需打扰）。
- **首个命中即返回**（尊重 `navigator.languages` 的顺序权重）。
- **检测到目标 ≠ 一定渲染**：渲染前还要过 i18n 文案可用性校验（见 §6.4 防裸 key）。

---

## 3. 形态与位置决策

### 3.1 形态

**页面顶部通栏细条**（full-width thin bar）：单行、高度约 `2.5rem`（展开后 `max-height: 4rem` 容纳折行），横跨整个视口宽度，内容区 `mx-auto max-w-6xl` 居中对齐主内容。

### 3.2 为什么「推挤」而非「覆盖 / sticky」

| 方案 | 取舍 | 结论 |
| --- | --- | --- |
| **文档流内（推挤内容下移）** ✅ | 横幅作为 body 第一个子元素，把 hero / 登录卡片整体往下推；不遮挡任何内容，无 z-index 竞争 | **采用** |
| `position: fixed/sticky` 覆盖 | 会遮挡 hero 顶部 / 登录卡片，需给下方补 padding；与顶栏 `sticky top-0 z-20` 产生层叠竞争；首屏内容被永久压住一截 | 弃用 |
| Toast / 浮层角落弹出 | 语言提示是「导航性」而非「通知性」，角落浮层易被忽略且与 toast 体系语义混淆 | 弃用 |

**核心理由**：语言提示是**进入页面前的路径选择**，应在最顶部、最先被看到、且不遮挡任何内容；推挤式让用户即使不点也能自然阅读下方内容，且无需处理「横幅关闭后布局回填」的跳动之外的复杂度（关闭时内容自然回弹，配合动画平滑）。

### 3.3 层叠与打印

- `z-index: auto`（不参与层叠竞争；文档流内天然在顶栏之上——因为它在 DOM 最前且顶栏是 sticky 内部元素，不会相交）。
- `print: hidden`（打印时不输出横幅，纯页面内容）。

---

## 4. 视觉规格（全部用 tokens，禁止硬编码色值）

### 4.1 tokens 对照表

| 元素 | 属性 | 值（token） |
| --- | --- | --- |
| 横幅底色 | `background` | `var(--bg-raise)` |
| 横幅底线 | `border-bottom` | `1px solid var(--line)` |
| 内容器 | 布局 | `mx-auto max-w-6xl px-5 sm:px-8`（对齐主内容区） |
| globe 图标 | `color` | `var(--accent)` |
| 正文 text | `color` / 字号 | `var(--text)` / `13px`（`text-[13px]`） |
| action pill 底 | `background` | `var(--accent-soft)` |
| action pill 字 | `color` | `var(--accent-strong)` |
| action hover | `background` | `var(--accent-soft)` 加深可用 `filter: brightness(1.15)` 或叠加 `--surface-2`；**不得引入新色值** |
| 关闭 ✕ | `color` | `var(--muted)`，hover → `var(--text)` |
| 关闭 focus | `outline` | `2px solid var(--accent)`（`:focus-visible`） |

> 注：accent pill 沿用现有「徽章浅底 + 亮字」语义（同 `--success-soft`/`--error-soft` 的 `*-soft` 模式），严格取自 tokens，不新增变量。

### 4.2 排版与间距

- 横幅内部 `flex items-center gap-3`，垂直居中，单行高度 ≈ `2.5rem`。
- 顺序：`[globe icon] [正文 text] …弹性空间… [action pill] [close ✕]`。
- 正文与 action 之间用 `ml-auto`（或 flex 弹性）把 action/close 推到右侧。
- action pill：`rounded-full px-3 py-1 text-[13px] font-medium`，`shrink-0`（不压缩）。
- close：`shrink-0` 的 ghost 按钮，触控目标 ≥36px（见 §9）。

---

## 5. DOM / class / 挂载点规格

### 5.1 DOM 结构（自外而内）

```html
<!-- body 的第一个子元素；构建期 HTML 中不存在，纯客户端插入 -->
<div class="lang-hint" role="region" aria-label="{当前页语言的 common.lang.label}">
  <div class="lang-hint-inner mx-auto max-w-6xl px-5 sm:px-8 flex items-center gap-3"
       lang="{目标语言码}">            <!-- 关键：TTS 用正确语音朗读目标文案 -->
    <svg class="lang-hint-icon h-4 w-4 shrink-0" …></svg>          <!-- globe，stroke currentColor -->
    <p class="lang-hint-text text-[13px]">{common.lang_hint.text.{code}}</p>
    <a class="lang-hint-action shrink-0 rounded-full px-3 py-1 text-[13px] font-medium"
       href="{alternate href}{location.search}{location.hash}"
       hreflang="{code}" lang="{code}">{common.lang_hint.action.{code}}</a>
    <button class="lang-hint-close shrink-0" type="button"
            aria-label="{common.lang_hint.dismiss.{code}}">✕</button>
  </div>
</div>
```

### 5.2 class 命名（新增，加入 `ui.css`，不污染现有类）

| class | 作用 |
| --- | --- |
| `.lang-hint` | 根：底色/底线/动画初始态/`print:hidden` |
| `.lang-hint-inner` | 内容器：居中、横向 flex、`lang` 属性载体 |
| `.lang-hint-icon` | globe 图标 |
| `.lang-hint-text` | 正文 |
| `.lang-hint-action` | accent pill 链接 |
| `.lang-hint-close` | ghost 关闭按钮 |
| `.lang-hint--open` | 展开态（动画用，见 §7） |

### 5.3 挂载点与时机

- **位置**：`document.body` 的**第一个子元素**（`body.prepend(banner)`）。
- **时机**：`partials.js` 新增 `mountLangHint()`，在 `DOMContentLoaded` 自动执行。
- **顺序约束（关键）**：**必须在 `mountLayout` 之后**执行。`mountLayout` 用 `body.prepend(root)` 注入侧栏布局；若 lang-hint 先 prepend，会被 mountLayout 的 prepend 顶到下面。**因此 lang-hint 最后 prepend，才能稳占 body 第一位。**
- **构建期**：HTML 模板**不含**横幅（纯客户端渐进增强，利于缓存与 SEO）。

### 5.4 出现范围（协调者已拍板）

- 仅 `login.html` 与 `index.html`。
- 两个 layout 的 `<body>` 加 `data-page="{{.Page}}"`；`mountLangHint()` 仅在 `data-page ∈ { "index.html", "login.html" }` 时继续，其余页面直接 return。

---

## 6. i18n 文案与 key 表

### 6.1 文案方案（关键设计决策）

运行时字典只有**当前页面语言**一份，但横幅要用**目标语言**写文案（否则目标用户看不懂）。因此：**每个 locale 文件都内嵌同一份「目标语言文案目录」**——4 个 i18n 文件里这 12 个 key 的**值完全相同**（不随当前语言变化，因为它描述的是「目标语言」）。

命名规则：`common.lang_hint.{slot}.{code}`，`slot ∈ {text, action, dismiss}`，`code ∈ {zh-CN, en, ja, es}`。

### 6.2 12 个 key 全量文案表（4 语言 × 3 slot，值跨文件一致）

| key | 文案（目标语言书写） |
| --- | --- |
| `common.lang_hint.text.zh-CN` | 本网站提供简体中文版本 |
| `common.lang_hint.text.en` | This site is also available in English |
| `common.lang_hint.text.ja` | このサイトは日本語でもご利用いただけます |
| `common.lang_hint.text.es` | Este sitio también está disponible en español |
| `common.lang_hint.action.zh-CN` | 切换到简体中文 |
| `common.lang_hint.action.en` | Switch to English |
| `common.lang_hint.action.ja` | 日本語に切り替える |
| `common.lang_hint.action.es` | Cambiar a español |
| `common.lang_hint.dismiss.zh-CN` | 关闭语言提示 |
| `common.lang_hint.dismiss.en` | Dismiss language hint |
| `common.lang_hint.dismiss.ja` | 言語のヒントを閉じる |
| `common.lang_hint.dismiss.es` | Cerrar el aviso de idioma |

> 说明：`text.{code}` 是「目标语言写的完整提示句」，`action.{code}` 是「目标语言写的切换到X」，`dismiss.{code}` 是「目标语言写的关闭 aria-label」。三者都服务目标用户，故都用目标语言。

### 6.3 region aria-label

根 `.lang-hint` 的 `aria-label` 用**当前页面语言**的 `common.lang.label`（已有 key：`语言 / Language / 言語 / Idioma`），因为该区域是页面的一部分，报读用当前界面语言即可。

### 6.4 防裸 key（渲染前校验，关键降级）

`file://` 直开或 i18n 未加载时，`t()` 会回退返回 key 本身。渲染前必须校验：

```
text = t('common.lang_hint.text.' + code)
if text == key 本身（即未被解析）: 静默 return，不出横幅
```

对 `text` / `action` / `dismiss` 三个都校验，任一未解析即整体不出（避免出现「裸 key 横幅」）。

---

## 7. 动画与降级

### 7.1 展开动画

- 插入时初始态：`max-height: 0; opacity: 0; overflow: hidden`。
- `requestAnimationFrame` 后加 `.lang-hint--open`：`max-height → 4rem; opacity → 1`，`transition: max-height .3s ease-out, opacity .3s ease-out`。
- 用 `max-height` 而非固定 `height`，以容纳移动端折行（§9）。

### 7.2 关闭动画

- 反向：移除 `.lang-hint--open`，`max-height → 0; opacity → 0`，`transition .2s ease-in`。
- `transitionend`（或 `setTimeout` 200ms 兜底）后 `banner.remove()` 移除 DOM，下方内容自然回弹。

### 7.3 prefers-reduced-motion 降级

```
@media (prefers-reduced-motion: reduce): 不出现过渡
  - 插入：直接带 .lang-hint--open 瞬时出现（无 0.3s 过渡）
  - 关闭：瞬时 remove（无 0.2s 反向动画）
```

JS 侧用 `matchMedia('(prefers-reduced-motion: reduce)').matches` 跳过 rAF 过渡，直接置终态。

---

## 8. 无障碍（a11y）

- **语义**：根用 `role="region"` + `aria-label`（当前页语言的 `common.lang.label`）。**不用 `role="status"`**——status 是 live region，而横幅内含可交互元素（链接+按钮），live region 会干扰交互报读。
- **键盘焦点序**：横幅在 DOM 最前 → Tab 首站是 **action 链接**、其次是 **close 按钮**，然后才进入 header/main。符合「最重要的路径选择最先可达」。
- **Esc 关闭**：焦点在横幅内任意元素时按 `Esc` → 关闭横幅，并把焦点移到 `header`/`main` 内**第一个可聚焦元素**（避免焦点丢失到 body）。
- **TTS 语言**：`.lang-hint-inner` 的 `lang="{目标语言码}"` 让屏幕阅读器用目标语言语音朗读提示与按钮；action `<a>` 同时带 `hreflang` + `lang`。
- **触控目标**：action 与 close 的可点区域 ≥ 36×36px（移动端 §9）。
- **focus 可见**：action / close 的 `:focus-visible` 用 `outline: 2px solid var(--accent)`。

---

## 9. 响应式

| 断口 | 行为 |
| --- | --- |
| **桌面 1280** | 单行；icon + text 左对齐，action + close 右侧；内容器 `max-w-6xl` 与主内容左右缘对齐。 |
| **移动 390** | text 允许**折成两行**（`max-height: 4rem` 刚好容纳）；action / close `shrink-0` 垂直居中不被挤压；icon 可 `shrink-0`；触控目标 ≥36px；横向 padding `px-5`。 |

折叠规则：移动端空间不足时**只折行 text**，绝不压缩/隐藏 action 与 close（它们是核心操作）。

---

## 10. 边界态

| 场景 | 行为 |
| --- | --- |
| 用户偏好语言 == 当前页语言 | 不出横幅 |
| `navigator.languages` 无匹配（如偏好 `fr`，站点无法语） | 不出横幅 |
| 单语言构建（head 无 alternate 链接） | 不出横幅 |
| 无 JS / JS 被禁用 | 横幅本就不在构建期 HTML 中 → 天然不出现，页面正常 |
| i18n 未加载（`file://` 直开 / i18n.*.js 缺失） | t() 回退 key 本身 → 校验拦截 → **静默不出**（§6.4） |
| 非 index/login 页面 | `data-page` 不匹配 → 不出 |
| demo / 后端未连接模式 | 不受影响（纯客户端，不依赖任何 API） |
| 用户点 close | 仅关闭当前这次，**不持久化**；下次加载重新判断（**有意为之**——用户可能只是这次不想切，不该永久剥夺提示；且符合「无状态静态站」定位） |
| 点 action 跳转 | 普通 `<a>` 整页跳转，href = alternate link + `location.search` + `location.hash`（与语言切换器一致，保留深链） |
| 多次检测/重复挂载 | 挂载前先查 `document.querySelector('.lang-hint')`，已存在则不重复插入（幂等） |

---

## 11. 给前端（anotify-frontend）的实现 checklist

按顺序照做，不要改设计：

1. **i18n**：在 `web/i18n.zh-CN.js` / `i18n.en.js` / `i18n.ja.js` / `i18n.es.js` 四个文件中，各加入 §6.2 的 **12 个 key**（4 个文件值完全相同）。放在 `common.lang.*` 附近，保持字母/分组有序。
2. **模板**：给 `index.html` 与 `login.html` 对应 layout 的 `<body>` 加 `data-page="{{.Page}}"`（确认渲染后分别为 `data-page="index.html"` / `"login.html"`）。
3. **CSS**：在 `web/ui.css` 新增 §5.2 的 `.lang-hint*` 系列类（底色/底线/动画/`print:hidden`/`prefers-reduced-motion`），颜色一律用 §4.1 的 tokens 变量，**禁止硬编码色值**。
4. **JS**：`web/partials.js` 新增 `mountLangHint()`：
   - 读 `data-page`，非 index/login 直接 return；
   - 按 §2.2 检测目标语言（含归一化、精确+前缀匹配、排除 x-default 与当前语言）；
   - 按 §6.4 校验三个文案 key 已解析，未解析静默 return；
   - 用 `Anotify.el` 构建 §5.1 DOM（textContent，不用 innerHTML），`body.prepend`；
   - 按 §7 做 rAF 展开、close/Esc 反向关闭 + remove、reduced-motion 降级；
   - 按 §8 绑 Esc 关闭并转移焦点；
   - 幂等：已存在 `.lang-hint` 则不重复插入。
5. **挂载顺序**：在 `DOMContentLoaded` 中，确保 `mountLangHint()` 在 `mountLayout()` **之后**调用（login 页无 mountLayout，则在其初始化逻辑之后）。
6. **跳转**：action href = alternate href + `location.search` + `location.hash`（复用切换器已有拼接逻辑）。

## 12. 验收要点

- [ ] 用 `ja`/`es`/`en` 偏好的浏览器（或 `navigator.languages` mock）访问 `index.html`（zh-CN），顶部出现对应语言横幅；文案为该目标语言。
- [ ] 偏好 `zh-CN` 访问 `zh-CN` 页 → 无横幅；偏好 `fr` → 无横幅。
- [ ] `zh-TW` 偏好 → 命中 `zh-CN` 横幅（前缀匹配生效）。
- [ ] 横幅把 hero / 登录卡片**整体下推**，不遮挡任何内容；顶栏 sticky 正常。
- [ ] Tab 首站是 action、其次 close；Esc 关闭且焦点移到 header/main 首个可聚焦元素。
- [ ] 点 action 整页跳到目标语言同一页，且保留 `?query` 与 `#hash`。
- [ ] close 后刷新 → 横幅再次出现（不持久化）。
- [ ] `file://` 直开 / 移除 i18n.js → 无横幅、无裸 key、无 JS 报错。
- [ ] `receivers/keys/security/docs/message` 页 → 无横幅。
- [ ] 打印预览无横幅；`prefers-reduced-motion` 下瞬时显隐。
- [ ] 390 视口 text 折两行、action/close 不被挤压、触控 ≥36px；1280 单行对齐 `max-w-6xl`。
- [ ] `web_verify` 逐页无 JS 错误/无横向溢出；改完 `make build` 重新指纹。
