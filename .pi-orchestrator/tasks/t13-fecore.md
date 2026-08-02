# 任务 T13 · 前端核心页：login + 总览 + 通知接收(Receivers)

先读根目录 `AGENTS.md` 和 `.pi-orchestrator/TASKS.md` 中 T13 一节。
你在 worktree `wt-fecore`（分支 wt-fecore）工作，**只改 web/ 下你负责的三个页面**。

## 目标

按设计稿实现 3 个纯静态页面（HTML + Tailwind CDN + tokens.css），后端 API 先按 `api/openapi.yaml` 契约 fetch（真实联调在集成期）。

## 设计来源（务必先看，保持一致）

- 设计令牌：`design/tokens.css`（颜色只用 tokens 变量，不硬编码色值；70% 中性/20% indigo/10% 语义）
- 登录页参考：`design/mockup-passkey.html`
- 总览参考：`design/mockup.html`（工作台仪表盘）
- 通知接收参考：`design/mockup-devices.html`
- 信息架构（左侧栏分组）：工作台(总览/通知接收/API Keys) · 集成(接入Agent/API文档/技术方案) · 账户(安全与登录/返回首页)

## 要实现（放在 web/ 目录）

1. `web/login.html`：Passkey 登录/注册页
   - 输入用户名 → 调 `/v1/auth/login/options` + `/v1/auth/login`（WebAuthn navigator.credentials.get）
   - 注册切换：`/v1/auth/register/options` + `/v1/auth/register`（navigator.credentials.create）
   - 无密码、生物识别引导文案；iOS 提示「添加到主屏幕」
2. `web/index.html`：总览仪表盘
   - 左侧栏导航（按上述 IA），主区：通知统计卡片、送达率、趋势图(可用 Chart.js CDN)、最近通知列表（fetch `/v1/notifications`）
3. `web/receivers.html`：通知接收（Receivers）
   - 顶部双 tab：「推送订阅」「WebSocket 长连接」
   - 推送订阅 tab：设备列表（fetch `/v1/devices`），每台设备：名称/平台/最近送达、接收开关(enabled)、status 过滤(all/error/success)、标签编辑、重命名、解绑(DELETE)
   - 「添加此设备」按钮：走 Web Push 订阅流程（navigator.serviceWorker + pushManager.subscribe，VAPID 公钥 fetch `/v1/vapid-public-key`），订阅成功 POST `/v1/devices`
   - WebSocket tab：说明文案 + 连接状态占位（真实连接集成期接）

## 约束

- 纯静态、无构建框架；Tailwind 用 CDN；共用左侧栏可抽成一个 inline 的 `<template>` 或公共 JS 片段（`web/partials.js`）保持一致
- 所有 fetch 失败要有降级（演示数据/错误提示），因为后端可能未起
- 适配桌面与移动宽度
- 完成后 commit：`feat(web): login + 总览 + 通知接收页`

## 自测

- 用系统 Chrome / web_verify 打开每个页面，确认无 console 错误、无元素溢出、能滚动到底

## 上报

`DONE T13` + 产出文件清单 + 自测方式（web_verify 各页结果/截图路径）+ 遗留风险
