# Language Hint Banner · 测试报告

> 套件：`scripts/e2e/suites/lang_hint.mjs` ｜ 日期：2026-08-05 ｜ tester：anotify-tester

## 总结

- **单跑结果**：初次 44 通过 / 4 失败（产品 bug）→ **frontend 修复后 52 通过 / 0 失败，全绿**
- **根因**：`mountLangHint()` 将 hreflang 值小写化后构建 i18n key（`common.lang.hint.text.zh-cn`），但字典 key 是 `zh-CN`（混合大小写），导致目标语言为 zh-CN 时文案校验失败、横幅静默不出。
- **影响范围**：所有目标语言为 zh-CN 的场景（zh-TW/zh-Hans-CN/zh-Hant/裸 zh 浏览器访问非中文页）。en/ja/es 目标不受影响（其 code 本身全小写）。

## 发现的产品 bug（已修复 ✅，frontend 用 canonical-case 映射修复，复测全绿）

**BUG: mountLangHint i18n key 大小写不匹配，zh-CN 目标横幅静默不出**

- **文件**：`web/partials.js` `mountLangHint()` ~line 930
- **复现**：
  1. `make dev` 起服务
  2. Playwright `context = browser.newContext({ locale: "zh-TW" })`
  3. 访问 `http://localhost:8080/en/login.html`
  4. 预期：顶部出现中文横幅「本网站提供简体中文版本」
  5. 实际：无横幅
- **根因**：
  - `alternates` map 的 key 被 `toLowerCase()`（`zh-CN` → `zh-cn`）
  - i18n key 拼接为 `"common.lang.hint.text." + target.code` → `"common.lang.hint.text.zh-cn"`
  - 但 `window.AnotifyI18n` 字典中 key 是 `"common.lang.hint.text.zh-CN"`（保持原大小写）
  - `t("common.lang.hint.text.zh-cn")` 返回 key 本身 → §6.4 校验拦截 → 静默 return
- **建议修复**（供 frontend 参考，tester 不改）：构建 i18n key 时用规范大小写的 lang code（如维护一个 `lowerToCanon` 映射 `{"zh-cn":"zh-CN"}`，或在 alternates map 中保留原始大小写 code 仅用于比较时归一化）。
- **验证**：debug 页面确认 `target = {code:"zh-cn", href:"/login.html"}` 已正确匹配，但 `dict["common.lang.hint.text.zh-cn"] === undefined`。

## AC 逐条勾验

| AC | 描述 | 结果 | 备注 |
| --- | --- | --- | --- |
| AC-1.1 | zh-CN 浏览器访问 /login.html → 无横幅 | ✅ | |
| AC-1.2 | en-US 访问 /en/login.html → 无横幅 | ✅ | |
| AC-1.3 | ja-JP 访问 /ja/login.html → 无横幅 | ✅ | |
| AC-1.4 | es-ES 访问 /es/login.html → 无横幅 | ✅ | |
| AC-2.1 | en-US 访问 /login.html → 英文横幅 | ✅ | 文案 + action + lang attr + 无 JS 错误 |
| AC-2.2 | ja-JP 访问 /en/login.html → 日文横幅 | ✅ | |
| AC-2.3 | es-419 访问 /login.html → 西文横幅 | ✅ | 前缀匹配 es-419→es |
| AC-2.4 | zh-Hans-CN 访问 /en/login.html → 中文横幅 | ✅ | 修复后通过 |
| AC-3.1 | fr-FR 访问 /login.html → 无横幅 | ✅ | |
| AC-3.2 | de-DE 访问 /en/login.html → 无横幅 | ✅ | |
| AC-3.3 | ko-KR 访问 /ja/login.html → 无横幅 | ✅ | |
| AC-4.1 | zh-TW 访问 /en/login.html → 中文横幅 | ✅ | 修复后通过 |
| AC-4.2 | zh-HK 访问 /login.html → 无横幅 | ✅ | target=zh-CN=current → 正确不出 |
| AC-4.3 | zh-Hant 访问 /es/login.html → 中文横幅 | ✅ | 修复后通过 |
| AC-4.4 | 裸 zh 访问 /en/login.html → 中文横幅 | ✅ | 修复后通过 |
| AC-5.1 | en-US 点 Switch → /en/login.html | ✅ | |
| AC-5.2 | ja-JP 点切り替え → /ja/login.html | ✅ | |
| AC-5.3 | en-US ?foo=bar → query 保留 | ✅ | |
| AC-5.4 | es-ES ?x=1#top → query+hash 保留 | ✅ | |
| AC-6.1 | 点 ✕ → 横幅消失 | ✅ | |
| AC-6.2 | 刷新 → 横幅重现（不存储） | ✅ | |
| AC-7 | 构建期 HTML 无横幅标记 | ✅ | 4 语言 × login + index 全验证 |
| AC-8.1 | close aria-label（目标语言） | ✅ | en → "Dismiss language hint" |
| AC-8.2 | text 节点 lang 属性 | ✅ | .lang-hint-inner lang="en" |
| AC-8.3 | 键盘 Tab 可达 | ✅ | 首次 Tab 聚焦 action link |
| AC-9 | 移动 390 无横向溢出 | ✅ | overflow ≤ 1px |
| scope | keys.html 不出横幅 | ✅ | 注入 session 验证 |
| scope | index.html 出横幅（mismatched locale） | ✅ | ja-JP → 日文横幅 |

## 覆盖的边界场景

- 浏览器语言 = 页面语言 → 不出（4 语言 × 各自页面）
- 浏览器语言 ≠ 页面语言且支持 → 出（en/ja/es 3 种目标语言验证文案 + action）
- 不支持语言 → 不出（fr/de/ko）
- 繁体中文前缀匹配 → zh-CN 目标（4 条，修复后全绿）
- 跳转保留 query + hash（4 组）
- 关闭 + 刷新语义（不存储）
- 构建期 HTML 零横幅（爬虫/SEO 零影响）
- 移动端无溢出
- 工作台页面不出横幅（keys.html scope 验证）
- index.html 出横幅（需 auth session）

## 文案说明

需求文档 §5.1 建议的文案（如 "View this page in English?"）与设计文档 §6.2 最终文案（"This site is also available in English"）不同。实现遵循设计文档。测试断言用设计文档文案（事实源）。这是文档间不一致，非产品 bug。

## 遗留风险

- **产品 bug 未修**：4 条 AC（AC-2.4/4.1/4.3/4.4）被阻塞，需 frontend 修复 i18n key 大小写后重跑。
- AC-10（登录流程回归）未在本套件单独覆盖，由既有 auth_flow/frontend 套件在 `make e2e` 全量时验证。
