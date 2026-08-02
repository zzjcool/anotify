# 任务 T14 返工 · 修复字体引用 + 设计令牌

你在 worktree `wt-feadmin`（分支 wt-feadmin）工作。协调者独立验证发现了 2 个必须修复的问题，请修正后重新上报。

## 问题 1：字体引用 404（功能缺陷）

三个页面都引用了 `../public/fonts/fonts.css` 和 `../public/favicon.svg`，在 `web/` 作为静态根时这些是 404。

- **修复**：参考 T13（`wt-fecore/web/`），把字体与图标资源**复制进 web/**（`web/fonts/`、`web/assets/favicon.svg`），并把引用改为相对路径 `fonts/fonts.css`、`assets/favicon.svg`。favicon 也可用根级 `favicon.svg`。

## 问题 2：违反设计令牌规则（一致性缺陷）

`keys.html`/`security.html`/`docs.html` 硬编码了 hex 色值（#050508/#8b8bfd/#34d399/#6ee7b7/#fcd34d 等），且未引入 `tokens.css`。设计规定**颜色只能来自 tokens.css 变量**（--bg/--accent/--success 等），70% 中性/20% indigo/10% 语义。

- **修复**：
  1. 复制 `design/tokens.css`（或直接复用 T13 的 `web/tokens.css`）到 `web/tokens.css`，在三个页面 `<link rel="stylesheet" href="tokens.css">`
  2. 把所有硬编码 hex 色值替换为对应 tokens 变量：
     - #050508→var(--bg)、#ececf1→var(--text)、#8b8bfd→var(--accent)、#34d399→var(--success)、#fb7185/#fda4af→var(--error)、#fbbf24/#fcd34d→var(--warn)、rgba 中性色→var(--surface-1)/var(--line)/var(--muted) 等
  3. 徽章/状态色的浅底用 --success-soft/--error-soft/--warn-soft/--accent-soft
- **目标**：三页 grep 不到任何裸 hex 色值（除 tokens.css 自身），视觉与原来一致。

## 不要改动

- 不要改页面结构/文案/交互，只做「引用修复 + 色值令牌化」
- 保持与 T13 侧栏 IA 一致

## 自测（必须真实执行）

```
cd <worktree> && python3 -m http.server 5715 --directory web &
# web_verify 或 playwright 打开 keys/security/docs：
#  - 无 404 失败请求（尤其 fonts.css / favicon）
#  - 无 JS 错误、无溢出
grep -rno "#[0-9a-fA-F]\{6\}" web/keys.html web/security.html web/docs.html  # 应为空
```

## 上报

`DONE T14-返工` + 修复清单 + 自测结果（404 已消除截图/grep 为空证明）+ 遗留风险
完成后 commit：`fix(web): T14 字体引用修复 + 色值令牌化`
