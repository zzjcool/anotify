# Anotify 编排任务板

> 协调者（主 pi Agent）在此跟踪编排任务的进展。专路 Agent 定义见 `.pi/agents/anotify-*.md`，
> 编排架构与分层原则见 `AGENTS.md` 第 0 节。

## 编排团队（6 专路 + 内置）

| Agent | 层 | 模型 | 职责 |
| --- | --- | --- | --- |
| anotify.anotify-pm | 定义 | kimi-k3 | 需求/价值/边界/验收标准 |
| anotify.anotify-designer | 定义 | kimi-k3 | 信息架构/视觉方案/交互规格 |
| anotify.anotify-scout | 侦察 | deepseek-v4-flash | 摸代码现状 → context.md |
| (内置 planner) | 规划 | kimi-k3 | 拆任务 → plan.md |
| anotify.anotify-worker | 实现 | glm-5.2 | 后端实施 |
| anotify.anotify-frontend | 实现 | glm-5.2 | 前端照稿实现 |
| anotify.anotify-tester | 实现 | glm-5.2 | 测试把关/门禁 |
| anotify.anotify-reviewer | 终审 | kimi-k3 | 对照需求&设计终审 |

## 标准流程

pm 定义 → designer 设计 → scout 侦察 + planner 拆 →
worker(后端) ∥ frontend(前端) 实施 → tester 把关 → reviewer 终审 → 协调者合并并标 ✅

---

## 当前任务

（空 —— 等待协调者下达下一个编排任务）

---

## 历史归档：前端框架化改造（已完成 2026-08-04）

把 6 个手写 HTML 重构为「构建期 Go template 合成 + 布局复用 + i18n」静态站点。
保持 纯静态 + embed + hash.mjs 指纹 架构。详见 `.pi-orchestrator/ARCHITECTURE.md`。

- [x] T1–T7 全部完成（调研/布局/sitegen/i18n/页面迁移/构建集成/验证），e2e 9/9 全绿

## 历史归档：消息详情页 + 推送深链（已完成 2026-08-05，commit da68540）

- 新增 message.html?id=<id> 详情页 + GET /v1/notifications/{id}
- 推送深链 url → message.html?id=；修复 payload base64 显示不全（messageView）
- 首页弹层字段补全 + 最新通知置顶（seq 降序）；e2e 全绿
