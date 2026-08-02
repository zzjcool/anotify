# DONE T14 · 前端管理页（API Keys + 安全与登录 + 接入文档）

任务卡：`.pi-orchestrator/tasks/t14-feadmin.md` ｜ worktree：`wt-feadmin` ｜ commit：`c4104fa`

## 产出文件

- `web/keys.html` —— API Keys 管理
- `web/security.html` —— 安全与登录
- `web/docs.html` —— 接入文档
- `web/layout.js` —— 工作台共享布局（侧边栏/顶栏/移动抽屉/DOM 工具），三页复用

## 功能清单

### keys.html

- 列表 `GET /v1/keys`：名称/前缀/scope 徽章/最近使用/状态/操作
- 新建 `POST /v1/keys`：scope 复选（notify:send / notify:receive / devices:read）→ 明文**仅展示一次**弹窗（复制按钮 + 「请立即保存」警示）
- 停用 `POST /v1/keys/:id/revoke`
- 后端不可达时静默降级到演示数据（表格右上角标「演示数据」），不抛 console 错误

### security.html

- Passkey 凭证列表 `GET /v1/auth/passkeys`：重命名(PATCH)/删除(DELETE)/添加(WebAuthn create)，本机凭证禁删
- 登录会话列表 `GET /v1/auth/sessions`：设备/浏览器/位置/最近活跃/吊销(DELETE)，当前会话禁吊销
- 恢复码区：占位（8 个掩码 + 「重新生成」按钮 + 即将上线说明）
- 认证轴 vs 投递轴概念说明卡

### docs.html

- 接入 Agent：两步指引 + 最小 curl + 通知旅程时序图
- API 概览：Base URL / 版本 + 接口地图（11 条端点）
- 认证方式：API Key(Bearer,argon2id) vs 会话 Cookie(Passkey)
- `POST /v1/notify`：参数表 + 完整 curl + 200/400 响应示例 + 投递规则（标签路由 + 级别过滤）
- Web Push 订阅：4 步流程 + 前端订阅核心代码 + iOS/Android/E2E 提示卡
- WebSocket `/v1/stream`：握手 + 双向帧协议（hello/notification/replay_end/pong/subscribed/error + subscribe/unsubscribe/ack/ping/resume）+ 重连续传
- 右侧目录（xl 起）滚动跟随高亮

## 设计一致性

- 严格用 design/ tokens（--bg/--accent/--muted 等变量），暗色主题、Inter + Fraunces + Sacramento 字体、jetBrains Mono 代码
- 侧边栏 IA 与设计决策一致：工作台(总览/通知接收/API Keys) · 集成(接入Agent/API文档) · 账户(安全与登录/返回首页)
- 卡片/徽章/按钮/方法徽章(m-get/m-post/m-patch/m-delete/m-ws)/代码块(tk-key/tk-str/tk-num/tk-punct/tk-comment) 均复刻 mockup 视觉

## 自测命令与结果

工具：playwright-core + 系统 Chrome（headless），本地 `python3 -m http.server` 起服

最终验收（桌面 1280×800 + 移动 390×844）：

```
keys     | JS错误:0 | 滚动到底:OK | 桌面溢出:0px | 移动溢出:0px
security | JS错误:0 | 滚动到底:OK | 桌面溢出:0px | 移动溢出:0px
docs     | JS错误:0 | 滚动到底:OK | 桌面溢出:0px | 移动溢出:0px
```

截图：`.pi-orchestrator/.pi/web-verify-shots/t14-{keys,security,docs}-final.png`

修复过程（开发中发现并解决）：

1. 后端 404 时 keys/security 抛 console 错误 → 改为静默降级（fetchApi 返回 null）
2. docs 接口地图表格移动端撑破视口（docOverflow 17px）→ 根因是长内容子元素经 grid/flex `min-width:auto` 把卡片撑开 → 加 `.grid>*,.card,.code-block{min-width:0}` + 移动端 `.overflow-x-auto{max-width+contain:inline-size}` → docOverflow=0

## 遗留风险

- **API 契约为占位对接**：`GET /v1/auth/passkeys`、Passkey PATCH/DELETE、会话 DELETE、`POST /v1/keys` 返回字段名（`key`/`plaintext`/`id`/`prefix`）是按 openapi.yaml 合理假设，**后端(T11)实现时若字段不同需在集成期对齐**；keys.html 已做双键兜底（`data.key || data.plaintext`）。
- 恢复码为纯占位 UI（任务卡允许「占位即可」），无实际生成逻辑。
- WebAuthn「添加 Passkey」需 HTTPS + 真实后端，file/http 演示模式下点击给友好提示，不会真实调起。
- 侧边栏「接入 Agent/总览/通知接收」链接指向 T13 的页面文件（index.html/receivers.html），本 worktree 未实现，集成期由 T13 提供。
EOF