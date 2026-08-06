# Anotify 首页语言提示横幅（Language Hint Banner）需求文档

> 状态：待设计/实施 ｜ 优先级：P2（体验增强，不阻塞核心链路）｜ 版本：v1.0
> 作者：anotify-pm ｜ 读者：designer / frontend / tester / reviewer
> 前置文档：`i18n-requirements.md`（N4/N5 原则不可破坏）、`i18n-switcher-design.md`（切换器既有规格）
> 事实基础：PM 亲核 `web/partials.js`、`web/login.html`、`web/index.html`、`web-src/locales/zh-CN.yaml`、`Makefile`（sitegen + hash 指纹链），与代码现状一致。

---

## 1. 背景与痛点

Anotify 已是 4 语言纯静态多页站点（zh-CN 默认根路径，en/ja/es 在 `/{lang}/`，**URL 即语言状态**），且已有全站语言切换器。但存在「最后一厘米」问题：

- 自托管实例的默认落地页是**中文**（根路径）。一个日语/英语/西语用户拿到分享链接（如 `https://someone.example.com/`）打开时，面对的是**非母语页面**。
- 语言切换器存在，但用户**不知道它的存在或位置**——在不认识的文字界面里找一个「切换语言」入口本身就是认知负担。
- i18n-requirements.md 已定死两条原则（N4/N5）：**不做 Accept-Language 自动重定向、不做语言偏好存储（无 Cookie/localStorage）**。因此不能用「记住用户上次选择」或「自动跳转」解决。

**用户拍板的折中方案**：在不破坏「不重定向、不存储」前提下，做一次**客户端软提示**——检测浏览器语言（`navigator.language`），若与当前页面语言不匹配且该语言受支持，在页面顶部弹一个**可关闭的小横幅**，用**目标语言**写「View this page in English?」式文案，点击跳到同页面对应语言版本。

### 1.1 用户价值

| 用户 | 痛点 | 本特性价值 |
| --- | --- | --- |
| 首次访问的国际用户（en/ja/es 母语） | 打开默认中文页，看不懂且不知有切换器 | 一眼看到母语提示，一键切到母语版 |
| 分享链接的推荐者 | 对方打开链接是陌生语言，第一印象差 | 接收方自动被引导到合适的语言，降低流失 |
| 中文用户（基本盘） | 无影响 | 默认中文、浏览器语言匹配时不弹横幅，零打扰 |

### 1.2 为什么不是自动重定向 / 偏好存储

i18n-requirements.md N4/N5 已论证（自动跳转是 SEO 与可用性双输；偏好存储违背纯静态无状态设计）。本特性是**第三路径**：不改变 URL 语义、不写任何状态，只在客户端做一次「建议」，把选择权完整留给用户。这是「提示」与「重定向」的本质区别——**用户不点就什么都不会发生，URL 永远保持原样**。

---

## 2. 不可妥协的约束（红线，实现层必须遵守）

| # | 约束 | 说明 |
| --- | --- | --- |
| C-1 | **绝不自动跳转/重定向** | 横幅只显示提示，页面语言永不因检测而自动改变。用户不点击就留在原页。 |
| C-2 | **绝不写任何存储** | 不读不写 Cookie、localStorage、sessionStorage、IndexedDB。横幅的「已关闭」状态仅存于当次页面的 JS 内存（模块变量），刷新/重进即重置。 |
| C-3 | **不得干扰登录流程** | login.html 涉及 Passkey 安全流程。横幅只做「跳到对应语言 login.html」，不修改 `nextTarget()`、`NEXT_MAP`、Passkey 任何逻辑。**已知现状**：login 的 `next` 参数不携带语言前缀（`/ja/index.html` 401 后跳 `/login.html?next=index.html`，`?next=` 是相对页名），本特性**不要求**修复此现状，横幅跳转也**不依赖** `next` 保留语言（横幅跳到的是 `/{lang}/login.html`，不带 `next`）。修复 `next` 语言丢失是独立的产品 bug，不在本范围。 |
| C-4 | **纯客户端，对 SEO/爬虫零影响** | 横幅**不得**出现在 sitegen 构建期输出的 HTML 里。构建期产物中 grep 不到横幅文案/DOM；横幅完全由运行时 JS 检测后注入。爬虫（无 JS / 禁用 JS）看到的页面与现在**逐字节一致**。 |
| C-5 | **单二进制 / 纯静态约束** | 不引入任何新依赖、新构建步骤、新服务端组件。复用现有 `partials.js`（`t()`、`el()`）+ `web-src/locales/*.yaml` + sitegen 既有管道。CSS 放 `ui.css`（自动进入 `make fe` 指纹链）。 |

---

## 3. 功能范围

### 3.1 本次做（In Scope）

| # | 需求 | 说明 |
| --- | --- | --- |
| S1 | **语言检测** | 读 `navigator.language`（或 `navigator.languages[0]`，取第一个），按 §4 映射表推导目标语言 |
| S2 | **横幅展示** | 检测语言 ≠ 当前页语言 **且** 目标语言受支持（zh-CN/en/ja/es 之一）时，在页面顶部显示可关闭横幅 |
| S3 | **目标语言文案** | 横幅文案用**目标语言**书写（如当前页是中文、浏览器是 en，则显示英文「View this page in English?」），见 §5 文案表 |
| S4 | **点击跳转** | 点击横幅主按钮跳到**同一页面**的目标语言版本，保留 query 与 hash |
| S5 | **可关闭** | 提供关闭按钮（×），点击后关闭，**当次访问内不再出现**（仅 JS 内存，刷新即重置，见 §6） |
| S6 | **适用页面** | **仅 login.html（4 语言版本）**。结论与权衡见 §7 |
| S7 | **文案 i18n 化** | 4 条横幅文案进 `web-src/locales/*.yaml` 的 `common.langHint.*`，4 语言 key 集合对齐（复用现有 key 对齐校验），运行时经 `i18n.{lang}.js` 由 `t()` 取用 |
| S8 | **E2E 覆盖** | 新增 E2E 套件验证 §8 验收标准，纳入 `make e2e` 门禁 |

### 3.2 本次不做（Out of Scope，防范围蔓延）

| # | 不做项 | 理由 |
| --- | --- | --- |
| N1 | **自动跳转/重定向** | C-1 红线；用户已拍板「软提示」而非「自动跳」 |
| N2 | **任何形式的偏好存储**（含「本次会话内不再提示」的 sessionStorage、含 dismissal 标记） | C-2 红线；用户已批准「不存储」。每次访问首页都会重新检测（见 §6 语义确认） |
| N3 | **A/B 测试、文案实验** | 单一文案直出，无实验框架 |
| N4 | **展示统计/埋点/分析** | 不统计横幅展示次数、点击率；纯静态无后端埋点 |
| N5 | **工作台页面（index/receivers/keys/docs/security/message）出横幅** | 见 §7 权衡——这些页面未登录即被 401 导走，登录用户已自主选择语言，横幅价值低 |
| N6 | **`navigator.languages` 全列表遍历** | 只取 `languages[0]`（第一偏好），不做多偏好遍历。简化逻辑，覆盖绝大多数场景 |
| N7 | **服务端 Accept-Language 检测** | 纯静态站点无服务端协商；且 N4 原则禁止服务端介入语言决策 |
| N8 | **横幅动效/轮播/多横幅** | 单条静态横幅，无动画轮播，无多语言同时提示 |
| N9 | **修复 login `?next=` 丢语言前缀** | 已知现状（C-3），独立 bug，不在本范围 |

---

## 4. 语言映射规则（navigator.language → 4 语言）

### 4.1 映射表

取 `navigator.languages[0]`（降级 `navigator.language`），**小写化**后按下表映射：

| 浏览器语言（BCP 47，小写化） | 映射到 | 说明 |
| --- | --- | --- |
| `zh-cn`、`zh-sg`、`zh-my`、`zh-hans`、`zh-hans-*` | **zh-CN** | 简体明确指向 zh-CN |
| `zh-tw`、`zh-hk`、`zh-mo`、`zh-hant`、`zh-hant-*` | **映射 zh-CN**（协调者决议，见 §4.2 修订） | 前缀匹配。繁体用户在 zh-CN 页时目标=当前语言 → 自然不出横幅（无打扰）；在 en/es 等外文页时得到中文提示 → 真实价值。 |
| `zh`（无区域/无文字标签） | **不提示**（返回 null） | 无法判断简繁，保守不提示（默认页本就是中文，zh 用户大概率能读） |
| `en`、`en-us`、`en-gb`、`en-au`、`en-*` | **en** | 任何英语变体 → en |
| `ja`、`ja-jp`、`ja-*` | **ja** | 任何日语变体 → ja |
| `es`、`es-es`、`es-mx`、`es-419`、`es-ar`、`es-*` | **es** | 任何西语变体（含拉美 es-419）→ es |
| 其他（`fr`、`de`、`ko`、`pt`、`ru`…） | **不提示**（返回 null） | 不支持的语言，不提示 |

### 4.2 关键决策说明

- **繁体中文（zh-TW/zh-HK）映射 zh-CN（2026-08-05 协调者决议，修订 PM 原案）**：PM 原案为「繁体一律不提示」，担心繁体用户在中文页收到「切简体」的噪音提示。复核后发现：前缀匹配方案下，繁体用户在 zh-CN 页时映射目标 = 当前语言 → **不会出横幅**（PM 的反打扰目标天然满足）；而繁体用户落在 en/es/ja 外文页时，「本站提供中文版本」提示是真实价值。故采用设计稿的前缀匹配（zh-TW/zh-HK → zh-CN 目标）。本节替代上方原决策，AC-4 相应修订。
- **`zh` 裸码不提示**：`navigator.language = "zh"` 无法判断简繁，且默认页就是中文，提示「切换到中文」无意义。保守不提示。
- **变体前缀匹配**：实现上先精确匹配全码（`es-419`），失败再按主语言子码（`-` 前缀）匹配。`es-419` → 主码 `es` → 命中。**任何我们支持的主语言变体都命中对应语言**；**不支持的主语言一律不提示**。
- **匹配成功但 = 当前页语言**：不提示（如当前页已是 en，浏览器也是 en）——这是 §8 AC-1 的核心。

### 4.3 伪代码（供实现层参考，非强制实现）

```
function detectTargetLang(currentPageLang) {
  const raw = (navigator.languages && navigator.languages[0]) || navigator.language || "";
  const tag = raw.toLowerCase();
  if (!tag) return null;

  // exact match first, then primary subtag
  const candidates = [tag, tag.split("-")[0]];
  for (const c of candidates) {
    if (c === "zh" ) {
      // zh / zh-hans* → zh-CN; zh-hant* / zh-tw / zh-hk / zh-mo → null
      if (c === "zh" && (tag === "zh" || tag.startsWith("zh-hant") || ["zh-tw","zh-hk","zh-mo"].includes(tag))) return null;
      if (c === "zh") return "zh-CN"; // zh, zh-hans, zh-cn, zh-sg, zh-my
    }
    if (c === "en") return "en";
    if (c === "ja") return "ja";
    if (c === "es") return "es";
  }
  return null; // unsupported
}
// 调用方：target = detectTargetLang(); if (!target || target === currentPageLang) 不显示
```

> 注：上述伪代码仅供理解映射规则，实现层可用更简洁的查表法。**行为必须满足 §4.1 映射表的全部 8 行**。

---

## 5. 横幅文案（4 条，用目标语言写）

**原则**：横幅文案用**目标语言**（用户浏览器的语言）写，不是用当前页语言。一个中文页面对英语用户显示英文提示，用户才能看懂并点击。

### 5.1 文案表

| 目标语言 | 横幅文案（主文本 + 按钮） | i18n key |
| --- | --- | --- |
| **zh-CN** | 主文本：「查看此页面的中文版本？」按钮：「切换」 | `common.langHint.text_zh` / `common.langHint.action_zh` |
| **en** | 主文本："View this page in English?" 按钮："Switch" | `common.langHint.text_en` / `common.langHint.action_en` |
| **ja** | 主文本：「このページを日本語で表示しますか？」 按钮：「切り替え」 | `common.langHint.text_ja` / `common.langHint.action_ja` |
| **es** | 主文本："¿Ver esta página en español?" 按钮："Cambiar" | `common.langHint.text_es` / `common.langHint.action_es` |

### 5.2 关闭按钮

- 关闭按钮用 `×`（SVG 图标，非 emoji），`aria-label` 用**目标语言**的「关闭」：
  - zh-CN: `关闭`
  - en: `Dismiss`
  - ja: `閉じる`
  - es: `Cerrar`
- key：`common.langHint.dismiss_zh` / `_en` / `_ja` / `_es`。

### 5.3 key 的组织方式（重要）

由于运行时 `i18n.{lang}.js` 是**每语言一个文件**（`i18n.zh-CN.js` 只含中文字典），而横幅需要在**任何语言页面**上显示**任意目标语言**的文案（如中文页显示英文横幅），**4 条文案必须全部进入每个语言的字典**。

即：`web-src/locales/zh-CN.yaml`、`en.yaml`、`ja.yaml`、`es.yaml` **四个文件都包含全部 8+4=12 个 `common.langHint.*` key**（4 条主文本 + 4 条按钮 + 4 条关闭 aria-label），且值完全相同（因为文案是目标语言的，与所在字典语言无关）。这样无论用户在哪个语言页面，`t("common.langHint.text_en")` 都能取到英文文案。

> **实现层注意**：这会在 4 个 YAML 中产生 12 条重复内容（值相同）。这是**有意为之**——运行时字典按语言拆分，而横幅文案天然跨语言。复用现有 key 对齐校验（4 语言 key 集合一致）即可自然覆盖。不要试图「优化」成单文件共享，那会破坏现有 i18n 架构。

---

## 6. 「一次」的语义（明确界定）

用户原话是「做一次客户端软提示」。在「不存储」红线（C-2）下，语义界定为：

| 场景 | 行为 |
| --- | --- |
| 首次访问 login.html | 检测不匹配 → 显示横幅 |
| 用户点关闭（×） | 横幅关闭，**当次页面内不再出现**（模块级 JS 变量标记） |
| 用户刷新页面 / 重新访问 | **横幅再次出现**（无存储，状态重置） |
| 用户点击「切换」跳转后 | 到达目标语言页，此时浏览器语言 = 页面语言，**不再提示**（匹配） |
| 用户跳转后又手动切回非母语页 | 重新检测，若不匹配**再次提示**（无存储，视为新访问） |

**权衡说明（已按用户「不存储」批准执行）**：

- **每次访问都提示**是「不存储」的必然结果。用户已明确批准不存储，接受「刷新后横幅再出现」的轻微重复。
- 这比「记住用户已关闭」更符合 Anotify 的纯静态无状态哲学，且实现更简单（无存储读写、无隐私合规面）。
- 「当次页面内不重复」由模块级变量保证（防止 SPA 式重复挂载或 JS 重入导致同一页面出现两条横幅），这是**内存态**，不是存储。

> **开放问题 O-1**（见 §9）：若后续用户反馈「每次刷新都弹太烦」，可在**不违背 N5 原则**的前提下评估「sessionStorage 记一次性 dismissal」（会话级、关浏览器即失效、不影响纯静态部署）。但**当前版本严格不存储**。

---

## 7. 适用页面结论：仅 login.html

### 7.1 结论

**横幅只出在 login.html（4 个语言版本）。不出在 index.html 或其他工作台页面。**

### 7.2 推理过程

用户原话是「首页」，但需区分「用户以为的首页」与「首次接触界面」：

| 页面 | 未登录访客能看到吗 | 分析 |
| --- | --- | --- |
| `/`（index.html） | **不能** | `web/index.html` 加载时调用 `api("/v1/notifications?limit=50")`，401 后 `partials.js` 的 `api()` 立即 `location.href = "login.html?next=index.html"`。**未登录访客在 index.html 上停留时间极短（一次 API 往返），随后被导到 login.html。** 在 index.html 出横幅，用户还没看清就被导走了，且横幅会干扰 401 跳转逻辑。 |
| `login.html` | **能** | 这是未登录访客的**真正首次接触界面**。用户在这里决定登录/注册/演示，语言障碍直接影响转化。 |
| 其他工作台页（receivers/keys/…） | **不能** | 同 index.html，401 导走。 |
| 已登录用户的 index.html | 能（已登录） | 已登录用户**已经**在某个语言界面完成了登录，说明他能看懂当前语言或已自主选择。再弹横幅是打扰。 |

### 7.3 结论表述

- **「首页」的产品语义 = 首次接触界面 = login.html**。用户的真实意图是「第一次来的人别被陌生语言挡住」，而不是字面的「index.html」。
- 横幅出在 login.html（4 语言版），覆盖「未登录首次访问」场景，这是唯一有价值且不干扰的场景。
- 已登录用户、工作台页面不出横幅（N5），理由见上表。

> **开放问题 O-2**（见 §9）：若未来 Anotify 增加「公开的落地页/产品介绍页」（无需登录即可浏览），横幅应扩展到该页。当前无此页面。

---

## 8. 验收标准（AC，供 tester 设计 E2E）

### AC-1 匹配时不显示（核心负向断言）

- AC-1.1 在无头浏览器中模拟 `navigator.language = "zh-CN"`（或 `languages: ["zh-CN"]`），访问 `/login.html`（zh-CN 页），**DOM 中不存在横幅**（无 `[data-lang-hint]` 或等效选择器命中的节点）。
- AC-1.2 模拟 `en-US`，访问 `/en/login.html`，**无横幅**。
- AC-1.3 模拟 `ja-JP`，访问 `/ja/login.html`，**无横幅**。
- AC-1.4 模拟 `es-ES`，访问 `/es/login.html`，**无横幅**。

### AC-2 不匹配且支持时显示（核心正向断言）

- AC-2.1 模拟 `en-US`，访问 `/login.html`（zh-CN 页），**显示横幅**，主文本为 `"View this page in English?"`，按钮为 `"Switch"`，且主文本节点 `lang="en"`（或等效语言标注）。
- AC-2.2 模拟 `ja-JP`，访问 `/en/login.html`，**显示横幅**，主文本为日文文案。
- AC-2.3 模拟 `es-419`（拉美西语，前缀匹配），访问 `/login.html`，**显示横幅**，主文本为西语文案。
- AC-2.4 模拟 `zh-Hans-CN`（带文字标签简体），访问 `/en/login.html`，**显示横幅**，主文本为中文文案。

### AC-3 不支持的语言不显示

- AC-3.1 模拟 `fr-FR`，访问 `/login.html`，**无横幅**。
- AC-3.2 模拟 `de-DE`，访问 `/en/login.html`，**无横幅**。
- AC-3.3 模拟 `ko-KR`，访问 `/ja/login.html`，**无横幅**。

### AC-4 繁体中文映射（按 §4.2 修订后的协调者决议）

- AC-4.1 模拟 `zh-TW`，访问 `/en/login.html`，**出中文横幅**（目标 zh-CN ≠ 当前 en，有价值提示）。
- AC-4.2 模拟 `zh-HK`，访问 `/login.html`（zh-CN 页），**无横幅**（映射目标 zh-CN = 当前语言，天然不打扰）。
- AC-4.3 模拟 `zh-Hant`，访问 `/es/login.html`，**出中文横幅**（同 AC-4.1）。
- AC-4.4 模拟裸码 `zh`（无区域），访问 `/en/login.html`，**出中文横幅**（前缀匹配 zh-CN）。

### AC-5 跳转正确且保留 query/hash

- AC-5.1 模拟 `en-US`，访问 `/login.html`，点击横幅「Switch」，最终 URL 为 `/en/login.html`。
- AC-5.2 模拟 `ja-JP`，访问 `/en/login.html`，点击「切り替え」，最终 URL 为 `/ja/login.html`。
- AC-5.3 模拟 `en-US`，访问 `/login.html?foo=bar`，点击「Switch」，最终 URL 为 `/en/login.html?foo=bar`（**query 保留**）。
- AC-5.4 模拟 `es-ES`，访问 `/ja/login.html?x=1#top`，点击「Cambiar」，最终 URL 为 `/es/login.html?x=1#top`（**query + hash 保留**）。

### AC-6 关闭后当次不再出现

- AC-6.1 模拟 `en-US`，访问 `/login.html`，显示横幅，点击关闭（×），**横幅消失**；在同一页面内执行任意 JS 重挂载/检测逻辑（如再次调用检测函数），**横幅不重现**。
- AC-6.2 关闭后**刷新页面**，横幅**重新出现**（验证「不存储」语义，§6）。

### AC-7 爬虫/无 JS 零影响

- AC-7.1 用 `curl`（或无头浏览器禁用 JS）请求 `/login.html`、`/en/login.html`、`/ja/login.html`、`/es/login.html`，响应 HTML 中**不含横幅 DOM/文案**（grep `View this page` / `lang-hint` / 横幅特征 class 均无命中）。
- AC-7.2 对比本特性实施前后的构建产物 HTML，login.html 4 语言版本的 `<body>` 内**无新增静态节点**（除 script 引用外）。

### AC-8 无障碍

- AC-8.1 横幅关闭按钮有 `aria-label`（目标语言的「关闭」，如 en 场景下为 `Dismiss`）。
- AC-8.2 横幅主文本节点有 `lang` 属性标注目标语言（屏幕阅读器正确发音）。
- AC-8.3 键盘 Tab 可达横幅按钮与关闭按钮，Enter 激活。

### AC-9 移动端无溢出

- AC-9.1 在 390×844 视口下，模拟 `en-US` 访问 `/login.html`，横幅显示**无横向滚动溢出**，关闭按钮与「Switch」按钮均可见可点。

### AC-10 不干扰登录流程（回归断言）

- AC-10.1 既有 login E2E 套件（Passkey 登录、demo 模式、`next` 跳转）**全绿零回归**。
- AC-10.2 模拟 `en-US` 访问 `/login.html`，显示横幅后**不点横幅**，直接完成 demo 登录，落点与无横幅时一致（`index.html?demo=1&u=...`）。

### AC-11 质量门禁

- AC-11.1 `make e2e` 全绿（含本特性新增 lang-hint 套件）。
- AC-11.2 `go test ./...` 通过（sitegen 对 4 语言 `common.langHint.*` key 对齐校验）。
- AC-11.3 4 语言 login.html 经无头浏览器验证无 console JS 错误（含横幅显示/不显示两种场景）。

---

## 9. 风险与开放问题

### 9.1 风险

| # | 风险 | 等级 | 缓解 |
| --- | --- | --- | --- |
| R1 | **`navigator.languages` 在部分浏览器/爬虫中为 `["en-US"]` 默认值**：如用户浏览器实际是中文但装了英文版浏览器，会误弹英文横幅 | 低 | 横幅是可关闭的软提示，误弹成本极低（点 × 即关）；不做自动跳转所以无实质伤害 |
| R2 | **每次刷新都弹横幅可能引起烦躁** | 低 | 用户已批准「不存储」；横幅极小且可一键关闭；O-1 留有 sessionStorage 演进路径 |
| R3 | **横幅在 login.html 顶部可能挤压既有布局**（login 页是居中卡片布局） | 中 | designer 设计稿需明确横幅在 login 页的插入位置与布局影响；AC-9 验证移动端无溢出 |
| R4 | **繁体用户期待被提示**：部分繁体用户可能希望有繁体版 | 低 | 当前无繁体语言版本（N2 原则：4 语言覆盖主要市场）；繁体用户读简体普遍可接受；未来若加 zh-TW 版本则自然覆盖 |
| R5 | **E2E 模拟 `navigator.language` 的可靠性**：Playwright 的 `locale` 参数影响 `navigator.language`，需确认测试框架能精确控制 | 中 | tester 用 Playwright `context = browser.new_context(locale="en-US")` 即可控制；AC 断言不依赖具体实现 |

### 9.2 开放问题（不阻塞开工，实施前由 designer/协调者拍板）

- O-1 **sessionStorage dismissal 演进**：当前严格不存储（§6）。若用户反馈「刷新就弹」过烦，评估会话级 dismissal（sessionStorage，关浏览器即失效，不违背 N5 的「无持久偏好」原则——N5 针对的是跨会话偏好）。**当前版本不做**。
- O-2 **未来公开落地页**：若 Anotify 增加无需登录的产品介绍页，横幅应扩展。当前无此页面，仅 login.html。
- O-3 **横幅插入位置与视觉**：横幅在 login.html 顶部（header 之上？header 之下？全宽还是随卡片宽度？）→ **designer 设计稿定**。约束：不挤压既有登录卡片布局，移动端无溢出（R3）。
- O-4 **横幅是否需 `role="banner"` 或 `role="region"` + aria-label**：倾向 `role="region"` + `aria-label`（目标语言的「语言提示」），避免与页面主 landmark 冲突 → designer 定。

---

## 10. 优先级

**P2（体验增强）**。不阻塞核心通知链路，不阻塞登录流程，但直接影响「国际用户首次接触 Anotify 时的第一印象」——这与 i18n 特性（P1）的价值闭环直接相关：i18n 让国际用户「能用」，本特性让国际用户「第一次来就知道能用」。

**拆分交付可接受**：本特性自包含（仅 login.html + 4 条文案 + 1 个检测函数），可独立设计/实施/测试，不依赖其他未完成任务。

---

## 11. 给实现层的关键提示（非强制，供参考）

1. **复用 `partials.js` 的 `el()` 构建 DOM**（禁 innerHTML，防 XSS），复用 `t()` 取文案。
2. **横幅逻辑放 `partials.js`**（或独立小函数挂 `Anotify`），在 `mountLoginLangSwitcher()` 同级的 IIFE 尾部自动执行——login.html 已加载 `partials.js`（第 368 行），无需新增 script 引用。
3. **当前页语言**可从 `<html lang>` 读取（sitegen 已输出，如 `<html lang="zh-CN">`），或从 URL 路径推导。推荐 `<html lang>`（单一事实源）。
4. **目标 URL 推导**复用切换器既有逻辑：`location.pathname` 去语言前缀 + 加目标前缀 + `location.search` + `location.hash`。注意 login.html 在 `/{lang}/login.html` 与 `/login.html` 两种形态。
5. **CSS 放 `ui.css`**（`web/ui.css` → `make fe` 自动指纹），颜色只用 tokens 变量（红线）。
6. **i18n key 对齐**：4 个 YAML 的 `common.langHint.*` key 集合必须完全一致（复用 sitegen 既有校验），值见 §5 文案表。

---

*附：任务来源为用户拍板的折中方案（在不破坏 N4/N5 下做客户端软提示）。本文档覆盖：适用范围（§7）、语言映射（§4）、「一次」语义（§6）、行为细节（§3/§8）、文案表（§5）、验收标准（§8）、非目标（§3.2），并明确 4 条不可妥协约束（§2）。*
