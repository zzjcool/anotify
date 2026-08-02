# 任务 T14 · 前端管理页：API Keys + 安全与登录(Security) + 接入文档

先读根目录 `AGENTS.md` 和 `.pi-orchestrator/TASKS.md` 中 T14 一节。
你在 worktree `wt-feadmin`（分支 wt-feadmin）工作，**只改 web/ 下你负责的页面**。

## 目标

按设计稿实现 3 个纯静态页面（HTML + Tailwind CDN + tokens.css），API 先按 `api/openapi.yaml` 契约 fetch。

## 设计来源

- 设计令牌：`design/tokens.css`（只用 tokens 变量配色）
- API Keys 参考：`design/mockup-keys.html`
- 安全与登录参考：`design/mockup-security.html`
- 接入文档参考：`design/mockup-api.html`、`design/tech-scheme.html`
- 信息架构（左侧栏分组）：工作台(总览/通知接收/API Keys) · 集成(接入Agent/API文档/技术方案) · 账户(安全与登录/返回首页)

## 要实现（放在 web/ 目录）

1. `web/keys.html`：API Keys 管理
   - 列表（fetch `/v1/keys`）：名称、前缀、scopes、状态、最近使用、操作
   - 新建（POST `/v1/keys`）：选 scope（notify:send / notify:receive / devices:read），**创建成功后明文只显示一次**（弹窗 + 复制按钮 + 「请立即保存」提示）
   - 停用（POST `/v1/keys/:id/revoke`）
2. `web/security.html`：安全与登录
   - Passkey 列表（已注册的凭证，重命名/删除）——数据可来自 `/v1/auth/sessions` 或专门端点
   - 登录会话列表（fetch `/v1/auth/sessions`）：设备/最近活跃/吊销
   - 恢复码区（占位即可，说明文案）
3. `web/docs.html`：接入文档
   - Agent 接入指引：pi 扩展监听 `agent_settled` 事件 → `POST /v1/notify`（Bearer Key）
   - curl 示例、Web Push 订阅流程、WebSocket 接收协议简介
   - 可参考 `design/tech-scheme.html` 的 API 契约与时序图内容

## 约束

- 纯静态、无构建框架；Tailwind CDN；左侧栏与 T13 保持一致（可各自内联相同样式，集成期统一）
- fetch 失败要降级（演示数据/错误提示）
- 适配桌面与移动宽度
- 完成后 commit：`feat(web): keys + security + docs 页`

## 自测

- web_verify 打开每个页面：无 console 错误、无溢出、能滚动到底

## 上报

`DONE T14` + 产出清单 + 自测方式 + 遗留风险
