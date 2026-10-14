# design/

Anotify 视觉事实源：mockup HTML + `tokens.css`。

## 约定

- **颜色**：只改 `tokens.css`；页面用 `var(--…)`，不要内联 `:root` 调色板。
- **与产品**：`design/tokens.css` 为事实源；同步方向 design → `web/tokens.css`（同套自定义属性，diff 应为 0）。产品页在 `web-src/` / `web/`。
- **构建 CSS**：产物页走 issue #9 / `web/tailwind.css`（勿用 Play CDN）。
- **CDN**：本目录原型可用 Tailwind Play CDN；生产页去 CDN 见 issue #9。
- **登录**：结构对齐产品（Passkey discoverable）；UX 态见 `mockup-login-states.html`。产品文案走 i18n。

## 入口

- [mockup-index.html](./mockup-index.html) — 导航
- [mockup-passkey.html](./mockup-passkey.html) — 登录
- [mockup-login-states.html](./mockup-login-states.html) — 登录 UX 态
- [mockup-style.html](./mockup-style.html) — 配色系统
