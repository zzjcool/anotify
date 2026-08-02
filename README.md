# iOS Safari 通知机制原型实验

用于在**公网（HTTPS）**环境下验证 iOS Safari 网页端通知机制，并测试**服务端主动推送**。
通过 Cloudflare Tunnel（见根目录 `Makefile` / `tunnel.sh`）把本地 Node 服务暴露到公网。

## 验证的核心机制

| 机制 | iOS Safari 关键限制 |
| ------ | --------------------- |
| **Notification API** | iOS 16.4+ 才支持；**必须先把站点「添加到主屏幕」以 standalone 方式运行**，普通标签页无法请求权限 |
| **Web Push (PushManager)** | 依赖 Service Worker + HTTPS + 已授予权限；同样要求 standalone 环境 |
| **Service Worker** | 要求 HTTPS（Secure Context）；注册于 `sw.js` |
| **服务端主动推送** | 用 VAPID 私钥向订阅 endpoint 发送加密消息 → SW 触发 `showNotification` |
| **Vibration API** | iOS Safari **不支持** `navigator.vibrate` |

## 页面功能

- 环境检测：运行模式、iOS 版本、Secure Context、各 API 可用性
- 通知权限 `Notification.requestPermission()`
- Service Worker 注册
- 本地测试通知
- Web Push 订阅（自动使用服务端 VAPID 公钥，订阅后自动上报服务端）
- 后台可见性 / 定时器节流观察

## 测试「服务端推送」的完整流程

```bash
# 1. 安装依赖并生成 VAPID 密钥（首次）
npm install
node genkeys.js          # 生成 vapid.json（含公私钥）

# 2. 启动本地 Node 服务 + Cloudflare 隧道（公网）
make start PORT=5699 DIR=./public   # 或分别启动
make serve PORT=5699
make tunnel PORT=5699
make url                             # 查看公网地址
```

```text
# 3. 手机上操作（关键）
a. iOS Safari 打开公网地址
b. 「共享 → 添加到主屏幕」→ 从主屏幕以独立 App 打开
c. 授权通知权限
d. 点击「请求 Push 订阅」
e. 订阅成功后，页面日志显示「✅ 订阅已上报服务端」
```

```bash
# 4. 服务端推送（任意一种方式）
node send.js "你好，这是一条测试推送"                  # 命令行
curl -X POST https://<公网域名>/send \
     -H "Content-Type: application/json" \
     -d '{"title":"测试","message":"来自服务端的推送"}'
```

手机屏幕应弹出通知横幅（即使 App 在后台/关闭也能收到）。

## 常用命令

| 命令 | 作用 |
| ------ | ------ |
| `node genkeys.js` | 生成 VAPID 密钥对 |
| `node server.js [PORT]` | 启动 Node 服务端（静态 + 订阅 + 推送 API） |
| `node send.js "消息"` | 向所有已订阅设备推送 |
| `make start/serve/tunnel/url` | 管理本地服务与公网隧道 |
| `make stop` | 停止服务 |
| `make clean` | 停止并清理 |

## 服务端 API

- `GET /vapid-public-key` — 返回 VAPID 公钥（前端订阅用）
- `POST /subscribe` — 接收并保存订阅（前端订阅后自动调用）
- `GET /subscriptions` — 查看已保存订阅
- `POST /send` — 向所有订阅推送（body: `{title, message}`）
- `GET /health` — 健康检查

## 文件结构

```text
server.js             # Node 服务端（静态 + 订阅 + 推送 API）
send.js               # 命令行推送工具
genkeys.js            # 生成 VAPID 密钥
vapid.json            # VAPID 公私钥（已 gitignore，勿提交）
subscriptions.json    # 已订阅设备（已 gitignore，运行时生成）
public/
  index.html          # 实验页面
  app.js              # 前端检测与订阅逻辑
  sw.js               # Service Worker（处理 push / 通知点击）
  manifest.webmanifest
  icon.png / icon-512.png
```

## 注意事项

- **VAPID 私钥在 `vapid.json`，切勿提交或泄露**；更换密钥后需重新在手机上订阅。
- 手机与开发机需能访问同一公网地址；本机 DNS 可能被公司网络过滤（返回 `198.20.0.x`），属开发环境问题，不影响真机。
- 内置占位 VAPID key 已废弃，现在全部使用服务端真实密钥。
