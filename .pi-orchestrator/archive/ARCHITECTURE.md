# Anotify 前端框架化架构蓝图（协调者定稿 · 所有子 Agent 必读）

> 参考 rssyes（Go template 布局 / i18n / 模板缓存 / staticURL）与 Hugo（静态站点生成）
> 思想，为 Anotify 定制的**轻量构建期静态站点生成**方案。
> **不引入 Gin / 运行时模板引擎 / 运行时 i18n 服务**，保持「纯静态 + embed + 指纹」架构。

## 0. 核心原则（不可违背）

1. **产物仍是纯静态 HTML**：所有合成在构建期完成，`make build` 输出静态文件供 embed。
2. **`make dev` 与 `make build` 行为一致**：dev 也跑 sitegen，只是 go run 直读 web/。
3. **不破坏现有功能**：partials.js 的客户端布局（sidebar/topbar/footer）继续工作；SPA 交互逻辑不动。
4. **视觉零回归**：重构后页面渲染必须与现状一致（用 web_verify 对比）。
5. **store 不依赖 broker、空列表返回 []、VAPID subject TrimPrefix** 等既有铁律继续有效。

## 1. 目录结构（新建 web-src/ 为源，web/ 为生成产物）

```
web-src/                        ← 【新建】唯一的页面源（人工编辑处）
  layouts/
    base.html                   ← 布局：<!doctype>+<head>公共块+<body>骨架+{{template "content"}}+脚本引入
    login.html                  ← login 独立布局（无 sidebar，flex 布局）
  pages/
    index.html                  ← 每页：{{define "content"}}页内容{{end}} + {{define "title"}} + {{define "style"}}(可选)
    receivers.html keys.html security.html docs.html login.html
  locales/
    zh-CN.yaml                  ← 中文翻译（默认）
    en.yaml                     ← 英文翻译
  static/                       ← 【软链或复制】指向 web/ 现有静态资源（css/fonts/assets/js）

web/                            ← 【生成产物】sitegen 输出，人工不再直接编辑 .html
  index.html login.html ...     ← 生成（zh-CN 默认，根路径）
  en/index.html en/login.html   ← 生成（英文，/en/ 前缀）
  (css/fonts/assets/js 等静态资源保持不变，仍由 hash.mjs 指纹)

cmd/sitegen/main.go             ← 【新建】Go 构建器
internal/sitegen/               ← 【新建】构建器核心（可测试）
  sitegen.go  template.go  i18n.go
```

**关键**：`web/*.html` 从"人工源"变成"生成产物"。人工只编辑 `web-src/`。`web/` 下静态资源（ui.css/tokens.css/partials.js/fonts/assets/sw.js/manifest.webmanifest）保持现状，**不移入 web-src**（它们是真正的静态资源，sitegen 只生成 .html）。

## 2. 布局契约（layouts/base.html）

借鉴 rssyes layout + content 模式。base.html 定义骨架，pages 定义 `{{define "content"}}` 等块。

base.html 负责：

- `<head>`：meta/图标块(manifest+theme-color+apple-touch-icon+favicon)/tailwind CDN/fonts/css
  - `<title>` 用 `{{template "title" .}}`，格式 `Anotify · {{页标题}}`
  - fonts 链接：默认 Inter+Fraunces；docs 页通过 `{{template "fonts-extra" .}}` 追加 JetBrains Mono
  - 页专属 `<style>` 用 `{{template "style" .}}`（无则空）
- `<body class="min-h-screen antialiased overflow-x-hidden">`
  - `<div id="page-main">{{template "content" .}}</div>`
  - `<script src="partials.js"></script>`
  - 页内联脚本 `{{template "script" .}}`
- i18n：所有可见文本用 `{{t .Lang "key"}}` 函数，构建期替换

login.html 布局：独立 `<body class="...flex flex-col">`，含 grid-overlay/header/main/footer 骨架，不用 page-main。

## 3. 模板数据（sitegen 注入）

```go
type PageData struct {
    Lang     string            // "zh-CN" / "en"
    LangPrefix string          // "" / "/en"（资源/链接前缀）
    T        func(key string) string  // i18n 翻译（也可注册为模板函数 t）
    // 页级元数据由 pages 通过 define 提供，无需字段
}
```

模板函数（借鉴 rssyes getTemplateFuncMap，精简）：

- `t key` → 当前语言翻译（缺 key 回退中文，再回退 key 本身）
- 不需要 staticURL（产物再由 hash.mjs 指纹，与现流程一致）

## 4. i18n 设计（构建期 + 轻量运行时）

**两类文案分别处理**：

### A. HTML 静态文本（构建期）

- 存于 `web-src/locales/{lang}.yaml`，按页面分组（`index.xxx` / `login.xxx` / `common.xxx`）
- sitegen 用 `{{t .Lang "..."}}` 在生成时替换 → 每语言一套静态 HTML
- zh-CN 为默认（根路径），en 生成到 `/en/` 子目录

### B. JS 字符串文案（轻量运行时）

- 现状：大量中文在 partials.js 导航、docs 的 innerHTML、各页状态/toast/confirm
- 方案：sitegen 额外从 locales 生成 `web/i18n.{lang}.js`（`window.AnotifyI18n = {...}`）
- partials.js 和各页 JS 改为读 `Anotify.t('key')`（Anotify.t 查 AnotifyI18n，缺则回退传入默认中文字符串）
- base.html 在 partials.js 之前引入 `<script src="i18n.{lang}.js">`（按当前语言）
- **首期范围**：i18n.js 机制 + 导航/页脚（partials.js 共享文案）先行；各页 JS 内文案逐步迁移（可作为后续增量）

## 5. sitegen 构建器（cmd/sitegen + internal/sitegen）

借鉴 rssyes TemplateCache（模板缓存 + devMode 旁路）：

- 解析 layouts + 所有 pages 为 template.Template（Funcs: t）
- 遍历 languages × pages，渲染 content/title/style/script 块合成完整 HTML
- 输出到 web/{lang-prefix}/page.html
- 生成 web/i18n.{lang}.js
- 模板缓存：布局解析一次，各页复用（构建期缓存，非运行时）
- 单元测试：internal/sitegen/*_test.go（渲染含预期文本、i18n 替换正确、缺 key 回退）

CLI：`go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en`
默认 lang 第一个（zh-CN）输出到根，其余到 `/{lang}/`。

## 6. 构建集成（Makefile）

```
make fe = sitegen + hash        # 先生成 HTML，再指纹
  node scripts/gen-icons.mjs    # (可选)图标
  go run ./cmd/sitegen -src web-src -out web
  node scripts/hash.mjs web internal/server/dist
make dev = sitegen + ANOTIFY_STATIC=./web go run ./cmd/server
```

## 7. 子 Agent 分工与验收

| 任务 | Agent | 模型 | 产出 | 验收 |
| --- | --- | --- | --- | --- |
| S1 sitegen 核心 | worker | glm-5.2 | cmd/sitegen + internal/sitegen + 单测 | go test 过，能把样例页合成 |
| S2 布局+页面迁移 | worker | glm-5.2 | web-src/layouts + 6 页 pages | sitegen 生成与现状视觉一致 |
| S3 i18n 抽取+机制 | worker | glm-5.2 | locales/*.yaml + i18n.js + partials.js 改造 | 中英文切换正确 |
| S4 构建集成+文档 | worker | deepseek-v4-flash | Makefile + DEVELOPMENT.md | make build/dev 通 |
| 验证 | 协调者 | kimi-k3 | make build + e2e + web_verify 对比 | 全绿 + 视觉一致 |

**依赖序**：S1（核心）先行 → S2/S3 并行（都依赖 S1 的模板契约）→ S4 最后。协调者全程验证。

## 8. 明确不做（避免范围蔓延）

- 不引入 Gin / 运行时 SSR / 运行时模板服务器
- 不做 rssyes 的 SEO（hreflang/OG/sitemap）——Anotify 是私有工具，无需 SEO
- 不重写 SPA 交互逻辑（partials.js 布局、各页 JS 保持）
- 不做 docs 页 JS 内 innerHTML 文档正文的完整 i18n（量太大，首期只做 UI 框架文案，docs 正文标记为后续）
- 不改 store/broker/push 等后端
