# i18n 特性终审报告

APPROVE

> 终审人：anotify-reviewer ｜ 对象：feat/i18n 分支未提交改动（工作树 diff）
> 依据：`.pi-orchestrator/i18n-requirements.md`（19 条 AC）、`.pi-orchestrator/i18n-switcher-design.md` v1.1
> 说明：本次为截断评审的收尾。上轮发现 2 个设计偏离（已修复），本轮复核修复 + 完成剩余检查项。

---

## 1. 上轮两个设计偏离的修复复核 —— ✅ 均正确

### 修复 1：登录页无 JS 降级列表（原 hidden → 已可见）

- `web-src/layouts/login.html:60`：`data-lang-list` 容器已无 `hidden` 类，构建期渲染的平铺语言链接在无 JS 时可见可用。
- `web/partials.js:411` `mountLoginLangSwitcher()`：JS 加载后读取平铺 `<a>`、追加 query/hash、用 `createLangDropdown` 构建下拉并**替换**平铺列表（`removeChild` 循环 + `append`，无 innerHTML）。
- IIFE 末尾（partials.js:859）自动调用，login 页无需显式挂载。✓

### 修复 2：侧栏切换器硬编码 KNOWN_LANGS → 构建期数据块

- `web-src/layouts/base.html:33-40`：新增 `#lang-switcher-data`（`hidden aria-hidden`）数据块，由 `{{range .Langs}}` 构建期渲染语言链接，**单一事实源 = sitegen .Langs**。
- `web/partials.js:366` `buildLangSwitcher()`：从数据块读取链接（无 `KNOWN_LANGS` 硬编码——已 grep 确认全文件无残留），追加 query/hash 后构建下拉，消费完 `dataEl.remove()`。数据块缺失（单语言构建）时返回 null 不渲染切换器。✓
- 实现取舍说明：设计稿原方案是侧栏也渲染**可见**平铺列表；实现改为隐藏数据块。可接受——整个侧栏本就由 mountLayout JS 注入，无 JS 时侧栏不存在，可见平铺列表无处安放。核心问题（硬编码列表漂移）已消除。已记录为残余风险（见 §5）。

### 修复未引入新问题

- query/hash 保留逻辑两处均在（partials.js:373-374、419-420：`qs && !href.includes("?")` / `hash && !href.includes("#")`），且构建期 href 不带 query（AC-3.4 依赖 JS 追加，e2e 已覆盖通过）。
- 两处都对 `linkEls.length <= 1` 短路（单语言构建不渲染），`hreflang` 值来自 sitegen 白名单（非用户输入），无 XSS 面。

## 2. 翻译质量抽查 —— ✅ 合格

ja/es 各抽 12 个 key 对照 zh 语义（tagline / quickstart / keys.save.warning / login.hint / signup.username_hint / receivers.pair.hint / ws.desc / passkey.subtitle / recovery 等）：

- **技术术语保留英文**：Passkey / Face ID / API Key / Web Push / WebSocket / WebAuthn / Agent / `notify:receive` 在两种语言中均正确保留。✓
- **日语**：自然、语体统一；`上报`→`送信` 略宽泛（scope 语境下可接受）；`2〜32文字（記号で始まらない・終わらない）` 语义忠实。
- **西语**：tú 语态全文一致（Toca / Guárdala / pierdes）；`Al cerrar, la Key completa no podrá volver a verse` 等长句准确。
- 结论：机翻起步 + 人工润色质量，符合需求文档 R1 的可接受标准（社区 PR 可继续打磨）。

## 3. 硬编码色值扫描 —— ✅ 无违规

`grep -rnE "#[0-9a-fA-F]{3,8}" web/partials.js web/ui.css web-src/layouts/`：

- partials.js：0 处。
- layouts：仅 `<meta name="theme-color" content="#050508">`（存量代码，非本次新增；meta 非样式）。
- ui.css 新增 lang 段（485-544 行）：全部使用 tokens 变量（`var(--muted)` / `var(--surface-2)` / `var(--accent)` / `var(--panel-overlay)` / `var(--line)`），唯一 rgba 是 `rgba(0,0,0,0.5)` 阴影——与设计稿 §6.4 逐字一致，可接受。

## 4. README 双语 —— ✅ 达标（AC-5.1/5.2）

- 互链：README.md 第 3 行 `**[中文](README.md)** | [English](README.en.md)`；README.en.md 第 3 行镜像。均在 H1 后 5 行内。✓
- 结构：剔除代码块内 `#` 注释后，两文件均为 **12 个真实标题、层级序列完全一致**（`#,##×7,###,##×3`），章节一一对应。✓
- 内部链接：README.en.md 引用的 DEVELOPMENT.md / AGENTS.md / E2E_TESTING.md / IOS_TESTING.md 均存在于仓库根。✓

## 5. YAML key 对齐 —— ✅ 完美对齐（AC-1.2）

用 YAML 解析器（非正则）扁平化比对 4 个 locale：**各 199 个 key，missing=[] extra=[]**（zh-CN 基准，en/ja/es 无缺失无多余）。

## 6. 生成产物抽验 —— ✅

- `web/ja/login.html`：`<html lang="ja">`；5 条 head hreflang alternate + 4 条列表 hreflang；切换器容器在。
- `web/es/keys.html`：恰好 5 条 alternate（zh-CN/en/ja/es/x-default），x-default 指向默认语言根路径，全部为相对路径——无硬编码域名（AC-2.3）。

## 7. 门禁状态（引用 tester/协调者已跑结果，本轮未重跑）

- `make e2e`：11/11 套件全绿（既有 10 套件零回归 + i18n 套件 298 断言，含 28 页面无 JS 错误/无横向溢出）。
- `go test ./...`：全绿（sitegen 4 语言矩阵 + key 对齐校验有单测覆盖）。

---

## 残余风险 / 非阻塞建议（🟢 可选，不阻塞合并）

1. **侧栏无 JS 降级为隐藏数据块**（非可见平铺）：与设计稿字面有出入但功能等价（无 JS 时侧栏整体不存在）。若未来侧栏改为 SSR 可见，需把 `#lang-switcher-data` 改回可见平铺。已在代码注释说明。
2. **ja/es 翻译为机翻起步**：母语者可能觉得个别表达生硬（如 ja `送信` vs zh `上报`）。建议后续社区 PR 打磨；YAML 结构已利于贡献。
3. **hreflang 用相对路径**：自托管 base 不可知下的正确选择；若将来有固定公开域名，可在 sitegen 加 `BaseURL` 生成绝对 URL（设计稿 §8 已列为低优先可选增强）。
4. **DEVELOPMENT.md / AGENTS.md / E2E_TESTING.md / IOS_TESTING.md 未双语**：需求文档 N6 明确不做（内部文档），仅提示后续若要国际化社区贡献可考虑补 README 之外的入口文档。

## 阻塞项

无。

---

VERDICT: **APPROVE**（可合并）
