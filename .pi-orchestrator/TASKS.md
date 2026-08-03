# Anotify 前端框架化改造 · 任务板

## 目标（已达成）
借鉴 rssyes（Go template 布局/i18n/模板缓存）与 Hugo（静态站点生成）思想，
把 6 个手写 HTML 页面重构为「构建期 Go template 合成 + 布局复用 + i18n」的静态站点。
保持现有 纯静态 + embed + hash.mjs 指纹 架构，未引入 Gin/运行时模板引擎。

## 任务拆解
- [x] T1 调研固化：rssyes 可借鉴点 + 页面结构分析（scout）→ .pi-orchestrator/ARCHITECTURE.md
- [x] T2 公共布局抽取：layouts/base.html + login.html（S2）
- [x] T3 sitegen 构建器：cmd/sitegen + internal/sitegen + 12 单测（S1）
- [x] T4 i18n：locales zh-CN/en + 运行时 Anotify.t + content/title {{t}} 化 151 处（S3+S2b）
- [x] T5 页面迁移：6 页 → web-src/pages（S2）
- [x] T6 构建集成：Makefile fe/dev 集成 sitegen + hash 根绝对路径指纹 + gitignore（S4）
- [x] T7 验证：make build + e2e 9/9 全绿 + web_verify 视觉零回归（协调者）
- [ ] T8 文档：DEVELOPMENT.md / AGENTS.md 更新新流程（进行中）

## 子 Agent 分工与模型
- S1 sitegen 核心 / S2 布局+页面 / S3 i18n 机制 / S2b 文案 {{t}} 化：glm-5.2
- S4 构建集成：deepseek-v4-flash
- scout 侦察：deepseek-v4-flash
- 协调者（架构/验证/合并）：kimi-k3

## 遗留（后续增量）
- 各页 script 块 JS 字符串的运行时 i18n（docs innerHTML 283 组 / 各页 toast/confirm/演示数据）
- mountLayout title/subtitle 的运行时 i18n（Anotify.t）
