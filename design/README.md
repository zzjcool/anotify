# design/

Anotify 视觉事实源：mockup HTML + `tokens.css`。

## 约定

- **颜色**：只改 `tokens.css`；页面用 `var(--…)`，不要内联 `:root` 调色板。
- **与产品**：`web/tokens.css` 应与本目录 tokens 保持同套自定义属性；产品页在 `web-src/` / `web/`。
- **CDN**：本目录原型可用 Tailwind Play CDN；生产页去 CDN 见 issue #9。
- **登录**：结构对齐产品（Passkey discoverable）；UX 态见 `mockup-login-states.html`。产品文案走 i18n。

## 入口

- [mockup-index.html](./mockup-index.html) — 导航
- [mockup-passkey.html](./mockup-passkey.html) — 登录
- [mockup-login-states.html](./mockup-login-states.html) — 登录 UX 态
- [mockup-style.html](./mockup-style.html) — 配色系统
