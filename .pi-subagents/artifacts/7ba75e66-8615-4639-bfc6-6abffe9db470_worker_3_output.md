# T13 · 前端核心页（login + 总览 + 通知接收）— 完成报告

## 交付概览

在 worktree `wt-fecore`（分支 wt-fecore）实现了 3 个纯静态页面 + 共享层 + Web Push 前提资源，全部落在 `web/` 目录，自包含、无构建框架。commit `b3c4103`（`feat(web): login + 总览 + 通知接收页`，15 文件 +2533 行）。

## 产出文件

| 文件 | 说明 |
| --- | --- |
| `web/login.html` | Passkey 登录/注册页。真实 WebAuthn 流程（`/v1/auth/login | register/options` + `/v1/auth/login | register`，含 base64url↔ArrayBuffer 转换、`NotAllowedError` 等异常分支）；登录/注册 tab；iOS「添加到主屏幕」提示；后端不可达时自动进入演示模式 |
| `web/index.html` | 总览仪表盘。KPI 卡片、GitHub 风格通知活动热力图、快速接入、最近通知（fetch `/v1/notifications`，失败降级演示数据 + 「演示数据」徽章）；状态筛选、通知详情弹层 |
| `web/receivers.html` | 通知接收。双 tab（推送订阅 / WebSocket 长连接）；本机状态卡（按 SW/PushManager/权限实时判定）；设备列表（fetch `/v1/devices`）：接收开关、status 过滤（all/error/success）、标签增删、重命名、解绑；「添加此设备」走完整 Web Push 订阅（权限→SW 注册→取 VAPID 公钥→pushManager.subscribe→POST `/v1/devices`）；WS tab 为说明 + 占位 |
| `web/partials.js` | 共享层：`el()` 安全 DOM 构建（防 XSS）、`Anotify.mountLayout()` 注入统一「侧栏 + 顶栏」布局（IA：工作台/集成/账户）、`api()` fetch 封装、WebAuthn 转换、平台识别、toast、复制 |
| `web/ui.css` | 共享样式（全部颜色来自 tokens.css 变量，不硬编码） |
| `web/tokens.css` | 设计令牌（复制自 design/tokens.css，保持单一色彩来源） |
| `web/sw.js` | Service Worker：push→showNotification、notificationclick→聚焦/打开链接 |
| `web/manifest.webmanifest` | PWA manifest（standalone，iOS 添加主屏幕前提） |
| `web/fonts/` `web/assets/` | 自托管字体与图标（favicon/icon.png/icon-512） |

## 设计/契约遵循

- **tokens.css 唯一色彩来源**：70% 中性 / 20% indigo / 10% 语义色，无硬编码色值
- **信息架构**：左侧栏三组（工作台：总览/通知接收/API Keys · 集成：接入Agent/API文档/技术方案 · 账户：安全与登录/返回首页），与 mockup 一致
- **API 契约**：严格按 `api/openapi.yaml`（`/v1/auth/*`、`/v1/notifications`、`/v1/devices`、`/v1/vapid-public-key`）
- **降级策略**：后端未起时所有页面降级到演示数据并显示「演示数据」徽章，不白屏、不报错

## 自测命令与结果

起 `python3 -m http.server 5712`（root=web/），用自写无头 Chrome（playwright-core，系统 Chrome）脚本逐页检查：

- 桌面 1280×800 + 移动 390×844 双视口
- 断言：无 console 错误 / 无 pageerror / 无意外 404 / 无横向溢出 / 可滚动到底
- **结果：login ✅ index ✅ receivers ✅（全部 PASS）**；移动端三页均无溢出
- 交互复测：WS tab 切换 ✓、添加设备弹层（平台自动识别 macOS）✓、注册 tab 切换 ✓，无 JS 错误
- `/v1/*` 404 为预期（后端未起），已正确降级

截图：`/tmp/t13-shots/{login,index,receivers,index-mobile,receivers-pair,login-signup}.png`

## 遗留风险

1. **WebAuthn 真实联调未做**：登录/注册的 challenge 解码与断言结构按常见后端契约（`{publicKey:{challenge,user.id,allowCredentials[]}}`，base64url）编写；若后端实际返回包裹层级不同，集成期需微调 `opts.publicKey || opts` 这一行。
2. **Web Push 真实订阅未端到端验证**：需要 HTTPS + 后端 VAPID + 真机/浏览器授权，静态 HTTP 环境下只能走到「权限请求」前。代码路径完整，待集成期 HTTPS 验证。
3. **演示数据的 `delivered/total` 字段**：依赖后端 notifications 是否返回投递统计；若无则显示 0/x，集成期按真实字段对齐 `normalize()`。
4. **侧栏「API Keys / 安全与登录 / API 文档」链接**指向 T14 的 `keys.html`/`security.html`/`docs.html`，本期 worktree 内为相对路径占位，集成期由 T14 补齐。

## 给协调者的验证建议

```
cd <repo> && python3 -m http.server <port>   # root = wt-fecore/web
# 或 web_verify 打开 login.html / index.html / receivers.html
```

重点核对：三页渲染无 console 错误、演示降级生效、移动宽度无溢出。