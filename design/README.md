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
| [00 总览](./wiki/index.html) | 六层结构、和 workflow 引擎的差别 |
| [01 为什么](./wiki/01-why.html) | 和单机 agent / dify·n8n 的对比、要解决的三件事、我们不是什么 |
| [02 核心概念](./wiki/02-concepts.html) | Capability / Output / Node / Task / Trace / Edge 六个名词、三种节点 |
| [03 一条事件的旅程](./wiki/03-flow.html) | 十步时序、四步派发、租约心跳、失败分支 |
| [04 能力与出口](./wiki/04-edges.html) | 三个要素、出口即下一能力名、kind 推导、edges 查表、版本并存、成环 |
| [05 节点与执行](./wiki/05-nodes.html) | 能力声明与校验、状态机、节点池、粘性路由、下线与生命周期 |
| [06 可观测与版本](./wiki/06-trace.html) | OTel 对齐、trace 与 span、没触发的四种原因、四对象版本钉住 |
| [07 安全边界](./wiki/07-safety.html) | 中枢永不下发命令、六道闸、中枢不解释任何用户逻辑 |
| [08 本期范围](./wiki/08-scope.html) | 组织模型、做/不做、预留扩展点 |
| [09 术语表](./wiki/09-glossary.html) | 94 条名词速查 |

**这一套是设计终态。** 它描述的模型（能力原子 + 出口即能力名 + kind 推导）已经定下来，
后续按它做实现重构。**没有历史包袱：库清空重建，不迁移、不双写、不兼容。**

改动 wiki 前先读 [wiki/_meta/README.md](./wiki/_meta/README.md) 的硬约束与写作原则
（只写结论不写推导过程、同一规则只讲一次、不留「旧模型 vs 新模型」这类痕迹）。

## 历史稿

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
