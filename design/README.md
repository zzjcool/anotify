# design/

Anotify 视觉事实源：mockup HTML + `tokens.css`。

## 约定

- **颜色**：只改 `tokens.css`；页面用 `var(--…)`，不要内联 `:root` 调色板。
- **与产品**：`design/tokens.css` 为事实源；同步方向 design → `web/tokens.css`（同套自定义属性，diff 应为 0）。产品页在 `web-src/` / `web/`。
- **构建 CSS**：产物页走 issue #9 / `web/tailwind.css`（勿用 Play CDN）。
- **CDN**：本目录原型可用 Tailwind Play CDN；生产页去 CDN 见 issue #9。
- **登录**：结构对齐产品（Passkey discoverable）；UX 态见 `mockup-login-states.html`。产品文案走 i18n。

## 入口

### 产品设计文档（事件中心 · 当前）

**[wiki/index.html](./wiki/index.html)** — 10 页设计文档，从「为什么」到「术语表」。

| 页 | 内容 |
| --- | --- |
| [00 总览](./wiki/index.html) | 五层结构、为什么不是 workflow 引擎 |
| [01 为什么](./wiki/01-why.html) | 现有断裂点、要解决的三件事、我们不是什么 |
| [02 核心概念](./wiki/02-concepts.html) | Capability / Output / Node / Task / Trace / Edge 六个名词、kind 推导、一次旅程 |
| [03 一条事件的旅程](./wiki/03-flow.html) | 十步时序、四步派发、租约心跳、失败分支 |
| [04 能力与出口](./wiki/04-edges.html) | 三个要素、kind 推导、edges 表查表派发、版本并存、成环与剪枝 |
| [05 节点与执行](./wiki/05-nodes.html) | 能力声明与校验、状态机、节点池、粘性路由、下线分级 |
| [06 可观测与版本](./wiki/06-trace.html) | OTel 对齐、trace 与 span、没触发的四种原因、四对象版本钉住 |
| [07 安全边界](./wiki/07-safety.html) | 中枢永不下发命令、六道闸、删掉规则引擎的安全收益 |
| [08 本期范围](./wiki/08-scope.html) | 做/不做、预留扩展点 |
| [09 术语表](./wiki/09-glossary.html) | 全部名词速查 |

样式：`wiki/assets/wiki.css`（依赖 `tokens.css`，无 Tailwind、无内联色值）。
维护约定：`wiki/_meta/README.md`（硬约束、已废弃术语、交付前自查清单）。

### 历史稿

早期讨论稿（hub-blueprint / hub-architecture / adr-001 / positioning）**已删除**。
它们是旧模型（规则引擎、两类 sink、节点=机器）的过程稿，与现行 wiki 冲突太多，
留着只会被误读。需要查历史时用 git：

```bash
git log --all --diff-filter=D -- design/archive/
git show <commit>:design/archive/hub-blueprint.md
```

### v1 原型（通知终端）

- [mockup-index.html](./mockup-index.html) — 导航
- [mockup-passkey.html](./mockup-passkey.html) — 登录
- [mockup-login-states.html](./mockup-login-states.html) — 登录 UX 态
- [mockup-style.html](./mockup-style.html) — 配色系统
