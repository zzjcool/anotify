# Anotify 前端框架化改造 · 任务板

## 目标

借鉴 rssyes（Go template 布局/i18n/模板缓存）与 Hugo（静态站点生成）思想，
把 6 个手写 HTML 页面重构为「构建期 Go template 合成 + 布局复用 + i18n」的静态站点。
保持现有 纯静态 + embed + hash.mjs 指纹 架构，不引入 Gin/运行时模板引擎。

## 架构决策（协调者定）

- 构建期合成：`cmd/sitegen` 用 html/template 把 layouts + pages + locales → web/ 静态 HTML
- i18n：构建期每语言一套（zh-CN 默认根路径，en 在 /en/），翻译存 locales/*.yaml
- 布局：layouts/base.html（含 <head>/header/sidebar/footer 外壳）+ pages/*.html（{{define "content"}}）
- 图标/资源：沿用 gen-icons.mjs + hash.mjs 指纹
- 兼容：`make dev`（直读 web/）与 `make build`（embed dist）行为一致

## 任务拆解

- [ ] T1 调研产物固化：rssyes 可借鉴点 + 本项目页面结构分析 → docs
- [ ] T2 公共布局抽取：分析 6 页公共 <head>/结构 → layouts/base.html 设计稿
- [ ] T3 sitegen 构建器：cmd/sitegen（template 解析+缓存+i18n 合成+静态输出）
- [ ] T4 i18n 抽取：6 页文案 → locales/zh-CN + en
- [ ] T5 页面迁移：6 页 → pages/*.html（{{define "content"}}），去重公共部分
- [ ] T6 构建集成：Makefile（sitegen→hash→build）、make dev 兼容
- [ ] T7 验证：make build + e2e 全绿 + web_verify 各页面/各语言
- [ ] T8 文档：DEVELOPMENT.md / AGENTS.md 更新新流程

## 状态

进行中：T1
