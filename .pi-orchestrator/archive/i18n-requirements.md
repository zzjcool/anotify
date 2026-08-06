# Anotify 多语言国际化（i18n）需求文档

> 状态：待实施 ｜ 优先级：P1（重要，不阻塞核心通知链路）｜ 版本：v1.0
> 作者：anotify-pm ｜ 读者：designer / worker / frontend / tester / reviewer
> 事实基础：协调者侦察结论 + PM 抽查核对（sitegen、locales、login 跳转逻辑），与代码现状一致。

---

## 1. 背景与痛点

Anotify 当前为双语静态站点（zh-CN 默认 → 根路径，en → `/en/`），由 `make sitegen` 构建期生成。存在三个真实痛点：

1. **国际自托管用户进不来**：Anotify 的首要价值是「单二进制易自托管」，目标用户是全球跑 AI Agent 的开发者。只有中英双语、且 UI 无切换入口，非中英用户（日语、西班牙语两大开发者群体）实际上被挡在门外。
2. **已有 en 版本「建成了但没有门」**：`web/en/*.html` 已生成，但全站没有任何语言切换器，用户不知道、也到不了英文版——已投入的翻译成本在浪费。
3. **项目门面单语**：README 仅中文。GitHub 是国际用户发现自托管软件的第一入口，单语 README 直接压低 star/试装转化。

## 2. 用户价值与目标用户

| 用户 | 痛点 | 本特性的价值 |
| --- | --- | --- |
| 国际自托管开发者（ja/es/en 母语） | 看不懂中文 UI，无法评估和部署 | 母语界面 + 英文 README，降低采纳门槛 |
| 中国开发者（现有基本盘） | 无影响 | 默认中文、根路径不变，零回归 |
| 向团队/社区推荐 Anotify 的用户 | 分享链接对方打开是中文 | 按 URL 分语言，分享 `/en/`、`/ja/` 链接对方直接看到对应语言 |

**价值排序**：易自托管 ≠ 只服务中文用户。多语言是「让更多人能自托管」的直接延伸，符合产品第一价值。

## 3. 目标与非目标（功能边界）

### 3.1 本次做（In Scope）

| # | 需求 | 说明 |
| --- | --- | --- |
| S1 | 站点从 2 语言升级为 **4 语言**：`zh-CN`（默认，根路径）、`en`（/en/）、`ja`（/ja/）、`es`（/es/） | 新增 `web-src/locales/ja.yaml`、`es.yaml`，与现有 zh-CN/en **key 集合完全对齐**（扁平 dotted-key YAML）；`Makefile` 的 `sitegen` 目标 `-langs` 改为 `zh-CN,en,ja,es` |
| S2 | **语言切换器**：所有页面（含 login）可见，点击后落在**同一页面的对应语言版本** | 行为规格见 §5 |
| S3 | **SEO/分享语义**：每个 HTML `<head>` 输出 `hreflang` 互链标签（含 `x-default`） | 结论与规格见 §4 |
| S4 | **README 双语**：`README.md`（中文主）+ `README.en.md`（英文），两文件顶部互链 | 翻译范围为 README 全文（快速开始/配置/架构等），保持结构一致 |
| S5 | **新代码注释用英文**：本特性新增/修改的代码，注释一律英文 | 仅约束**新增代码**，不返工存量中文注释（见非目标） |
| S6 | **全流程可过**：`make e2e` 全绿；4 语言 × 全部页面经无头浏览器验证无 JS 错误、无布局溢出 | 验收见 §6 |

### 3.2 本次不做（Out of Scope，防范围蔓延）

| # | 不做项 | 理由 |
| --- | --- | --- |
| N1 | **后端 API 错误消息 i18n** | API 消费者是 Agent/脚本（机器），不是人；错误码 + 英文消息足够。引入 Accept-Language 协商会污染 API 契约 |
| N2 | **更多语言**（ko/fr/de/pt…） | 4 语言覆盖主要开发者市场；新增语言 = 加一个 YAML + `-langs` 加一项，架构上已留路，但本次不翻 |
| N3 | **运行时语言切换（不跳转、局部刷新文案）** | 违背纯静态多页架构；按 URL 分语言才有分享/SEO 价值 |
| N4 | **Accept-Language 自动重定向 / 语言探测** | 无状态、无服务端内容协商；用户访问哪个 URL 就看哪个语言。自动跳转是 SEO 与可用性双输（搜索引擎抓取被干扰、用户分享链接被改写） |
| N5 | **localStorage / Cookie 记住语言偏好** | 与纯静态无状态设计冲突；URL 即状态（详见 §5.4 结论） |
| N6 | **内部文档完整翻译**（DEVELOPMENT.md / AGENTS.md / E2E_TESTING.md / IOS_TESTING.md / design/） | 面向实施者与 Agent 的内部文档，维护双语同步成本高、收益低；README 是门面必须双语，内部文档不是 |
| N7 | **存量中文注释返工为英文** | 纯成本无用户价值；只约束新增代码（S5） |
| N8 | **weblate/ Crowdin 等翻译管理平台、重型 i18n 框架** | 违反单二进制/纯静态约束；YAML 文件 + git 足够 |
| N9 | **日期/数字/时区本地化格式化** | 现有 UI 无强依赖；出现「相对时间」等文案时按各语言 YAML 翻译即可，不引入 Intl 格式化层 |

## 4. URL 结构与 SEO/分享语义

### 4.1 URL 结构（维持现状，仅扩展语言数）

```
/            → zh-CN（默认，根路径，不变）
/en/**       → English
/ja/**       → 日本語（新增）
/es/**       → Español（新增）
```

- 由 sitegen `-langs zh-CN,en,ja,es` 生成：**第一个为默认语言输出到根路径**，其余输出到 `web/{lang}/`。
- 运行时 JS 文案文件同步扩展：`web/i18n.zh-CN.js`、`i18n.en.js`、`i18n.ja.js`、`i18n.es.js`。
- **同一页面各语言版本 URL 一一对应**：`/receivers.html` ↔ `/en/receivers.html` ↔ `/ja/receivers.html` ↔ `/es/receivers.html`；查询串（如 `?msg=ntf_xxx`、`?demo=1`）切换时**原样保留**。

### 4.2 hreflang 结论：**需要做**（S3）

每个页面的 `<head>` 输出一组互链标签，指向自身及其他 3 个语言版本 + `x-default`：

```html
<link rel="alternate" hreflang="zh-CN" href="{absBase}/receivers.html" />
<link rel="alternate" hreflang="en" href="{absBase}/en/receivers.html" />
<link rel="alternate" hreflang="ja" href="{absBase}/ja/receivers.html" />
<link rel="alternate" hreflang="es" href="{absBase}/es/receivers.html" />
<link rel="alternate" hreflang="x-default" href="{absBase}/receivers.html" />
```

**规格约束（实现层注意）**：

- `x-default` 必须指向默认语言（zh-CN，根路径）版本。
- 4 个语言版本的同一页面，输出的 hreflang 集合**完全一致**（自引用 + 互指），这是 Google 的硬性要求。
- `<html lang="{{.Lang}}">` 已存在，保持由 sitegen 按语言输出。
- **href 的绝对/相对**：自托管部署的 base URL 不可知，hreflang 允许相对路径时按相对输出（与站内现有相对链接风格一致）；若实现上需要绝对 URL，必须在 sitegen 提供可配置 base（环境变量/flag），缺省退化为相对路径——**绝不允许硬编码任何域名**。

## 5. 语言切换器行为规格

### 5.1 形态与位置

- **形态**：极简下拉/弹出菜单，触发器显示当前语言（如 `简体中文 ▾`）或 🌐 图标 + 当前语言。桌面与移动端均可用；移动端可收进现有菜单抽屉，但**必须可达**。
- **位置**：
  - 工作台页面（layouts/base.html 体系）：顶部导航栏，全局一致位置。
  - **login 页（layouts/login.html）：同样必须有**（用户原话明确要求），位置自定但首屏可见。
- **视觉**：遵循 designer 设计稿；颜色只用 tokens 变量，不硬编码。

### 5.2 切换目标 URL 推导规则（核心逻辑，无状态）

设当前 `location.pathname` 为 P，目标语言为 L（L ∈ {zh-CN, en, ja, es}，zh-CN 为默认）：

1. **去语言前缀**：若 P 以 `/en/`、`/ja/`、`/es/` 开头，剥掉该前缀，得到页面路径 rest；否则 P 本身就是 rest（当前为默认语言）。
2. **加目标前缀**：
   - 若 L 是默认语言（zh-CN）→ 目标 = `/` + rest
   - 否则 → 目标 = `/` + L + `/` + rest
3. **保留查询串与 hash**：最终跳转 URL = 目标 + `location.search` + `location.hash`。
4. 用 `<a href>` 直出各语言链接（构建期可由模板推导，或运行时用上述规则推导），**整页跳转**（纯静态多页，无需 JS 局部替换文案）。

**示例**：

| 当前 URL | 点「English」 | 点「日本語」 | 点「简体中文」 |
| --- | --- | --- | --- |
| `/receivers.html` | `/en/receivers.html` | `/ja/receivers.html` | 当前页（高亮态） |
| `/en/keys.html?msg=ntf_1` | 当前页（高亮态） | `/ja/keys.html?msg=ntf_1` | `/keys.html?msg=ntf_1` |
| `/ja/login.html` | `/en/login.html` | 当前页（高亮态） | `/login.html` |

### 5.3 当前语言高亮

- 菜单中当前语言项有明确选中态（高亮/勾选），点击当前语言不跳转或跳转自身（行为一致即可，由 designer 定夺细节）。

### 5.4 状态结论：**不需要 localStorage / Cookie**

- 现有设计是纯静态多页、按 URL 分语言，**URL 即语言状态**：刷新、分享、书签、前进后退全部天然正确。
- login 页登录成功后 `location.href = nextTarget()` 是相对路径跳转（现状），从 `/ja/login.html` 登录会落回 `/ja/index.html`——**语言在登录流程中自动保持，无需任何偏好存储**。这是维持无状态方案的关键证据，实现层不得引入破坏该相对跳转语义的改动（登录后强制回根路径 = 产品 bug，需上报）。
- 「下次访问自动进上次语言」属于 N4/N5 明确不做项。

## 6. 验收标准（AC，供 tester 设计 E2E）

### AC-1 站点生成与产物完整性

- AC-1.1 执行 `make sitegen` 后，`web/` 下存在：根路径 7 个页面（zh-CN）、`/en/`、`/ja/`、`/es/` 各 7 个页面，共 28 个 HTML；且存在 `i18n.zh-CN.js`、`i18n.en.js`、`i18n.ja.js`、`i18n.es.js` 四个文件，内容均为 `window.AnotifyI18n = {...}` 且可被 JS 解析。
- AC-1.2 四个语言的翻译文件 key 集合完全一致：以 zh-CN 为基准，en/ja/es 无缺失 key、无多余 key（应有 sitegen 校验或测试断言；构建时发现缺 key 必须显式失败或警告——具体 fail/warn 策略由实现层定，但产物中**不得出现把 dotted-key 原样渲染到页面**的情况）。
- AC-1.3 每个 HTML 的 `<html lang>` 与目录语言一致：根路径 `zh-CN`，`/en/` 为 `en`，以此类推。

### AC-2 hreflang 与 head 语义

- AC-2.1 任取 3 个不同页面 × 4 个语言版本，每个 HTML 的 `<head>` 含且仅含 5 条 `rel="alternate"` hreflang 标签：`zh-CN`、`en`、`ja`、`es`、`x-default`，且 4 个版本的标签集合相同（含自引用）。
- AC-2.2 hreflang 链接指向真实存在的页面：抽样点击/请求全部返回 200（无 404）。
- AC-2.3 产物全文搜索**不存在**硬编码的外部域名（如 github.io、example.com 等）出现在 hreflang href 中。

### AC-3 语言切换器

- AC-3.1 **每一个页面**（index/receivers/keys/docs/security/message/login × 4 语言 = 28 个）渲染后 DOM 中存在语言切换器，且列出全部 4 种语言选项。
- AC-3.2 切换器内当前语言项有可见选中态（断言带选中态 class/aria-current 的项文本对应当前语言）。
- AC-3.3 在 `/en/keys.html` 点击「日本語」，无头浏览器最终 URL 为 `/ja/keys.html`；在 `/ja/login.html` 点击「简体中文」最终 URL 为 `/login.html`（覆盖 §5.2 示例矩阵，含默认语言往返）。
- AC-3.4 带查询串切换：`/en/receivers.html?msg=ntf_test1` 切到 es 后 URL 为 `/es/receivers.html?msg=ntf_test1`（查询串保留）。
- AC-3.5 切换后页面文案确实变化：如切换到 en 后导航「通知接收」变为对应英文文案，切换到 ja 后变为日文文案（抽查 nav 与页面 title 各一处）。
- AC-3.6 切换器在移动端视口（如 390×844）可用：能打开菜单并完成一次切换，无横向溢出。

### AC-4 登录流程语言保持

- AC-4.1 从 `/en/login.html`（或 `/ja/login.html`）完成演示模式进入（demoEnter）或登录跳转后，落点是**同语言**的 `/{lang}/index.html`，而非被弹回中文根路径。（用 demo 模式做 E2E 即可，Passkey 真登录不在本特性验证范围。）

### AC-5 README

- AC-5.1 仓库根存在 `README.md`（中文为主）与 `README.en.md`（英文），两文件顶部（首个 H1 之后 5 行内）均有指向对方的链接。
- AC-5.2 `README.en.md` 主体为英文，章节结构与中文 README 一一对应（标题数量一致），内部相对链接（如 `DEVELOPMENT.md`）仍然有效。

### AC-6 代码注释规范

- AC-6.1 本特性新增的 `.go`、`.mjs`、模板文件中的注释为英文（reviewer 人工把关 + PR diff 抽查，不写自动化语言检测）。

### AC-7 质量门禁（全绿）

- AC-7.1 `make e2e` 全部套件通过（含既有套件零回归 + 本特性新增 i18n E2E 套件）。
- AC-7.2 `go test ./...` 通过（sitegen 相关单测更新/新增：4 语言生成、key 对齐校验）。
- AC-7.3 无头浏览器遍历 28 个页面：无 console JS 错误、无横向滚动溢出（沿用现有 web_verify 流程，逐页跑）。

## 7. 风险与开放问题

### 7.1 风险（实现层需正视）

| # | 风险 | 等级 | 缓解 |
| --- | --- | --- | --- |
| R1 | **翻译质量**：ja/es 为机器翻译起步，母语者可能觉得生硬 | 中 | YAML 结构利于社区 PR 修正；README.en 由英文母语级润色（它是门面）；接受 v1 不完美 |
| R2 | **key 漂移**：4 语言 × ~200 key，后续加文案时容易漏翻某语言 | 高 | AC-1.2 的 key 对齐校验必须落地为构建/测试断言，缺 key 不得静默渲染成原始 key 字符串 |
| R3 | **sitegen 改动回归**：`-langs` 扩为 4 语言触及生成逻辑，可能影响现有 zh-CN/en 产物 | 中 | AC-7 全量门禁；sitegen 单测覆盖 4 语言矩阵 |
| R4 | **日文/西文文案长度溢出**：日文更短通常安全，西语普遍比中文长 30%+，按钮/导航可能撑破布局 | 中 | AC-3.6/AC-7.3 逐页无溢出验证；designer 在稿件中为切换器与导航预留弹性 |
| R5 | **login 页改动风险**：login 涉及 Passkey 安全流程，加切换器不得干扰现有 JS | 高 | 切换器只做静态链接，不改 login 业务 JS；E2E 登录套件回归（AC-4.1 + 既有套件） |
| R6 | **指纹/embed 链路**：`make fe` 指纹与 `go:embed` 对新增 `/ja/`、`/es/` 目录与 i18n js 的引用改写 | 中 | `make build` 后抽查 embed 产物；e2e 覆盖 |

### 7.2 开放问题（不阻塞开工，实现前由 designer/协调者拍板）

- Q1 切换器视觉形态（🌐 图标 + 当前语言名 vs 纯文字下拉）→ **designer 设计稿定**。
- Q2 语言选项的显示名：建议各语言用**母语名**（简体中文 / English / 日本語 / Español），不随界面语言变化——业界惯例，倾向直接采用，designer 无异议即定稿。
- Q3 `?demo=1` 演示模式横幅等运行时 JS 文案（partials.js 的 `t(key)` 路径）在 ja/es 下的 key 是否已覆盖全 → worker 实施时核对，缺 key 按 R2 处理。
- Q4 sitegen 对缺 key 的策略（构建失败 vs 警告 + fallback 到默认语言文案）→ 倾向 **警告 + fallback 到 zh-CN 文案**，既不阻塞开发又不出现裸 key；最终由协调者定。

## 8. 优先级

**P1（重要）**。不阻塞核心通知链路，但直接关系产品第一价值（易自托管触达国际用户）与已投入翻译成本的回收。建议在下一个迭代完整交付，不接受「只做切换器不加语言」或「只加语言不做 README」的半吊子拆分——三者加起来才构成「国际用户能发现、能用、能部署」的完整闭环。

---

*附：任务来源为用户原话 5 点诉求（4 语言 / 切换器 / README 双语 / 英文注释 / e2e 全绿），本文档全部覆盖并细化为可验收条款。*
