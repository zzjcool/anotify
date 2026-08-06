# iOS Safari 真机验证清单（T40）

> 这是唯一无法自动化的环节（需真实 iPhone + APNs 通道）。
> 我已完成其余全部验证，请你按以下步骤做最后确认。

## 当前状态（我已备好）

- ✅ 服务已在公网运行（Cloudflare **命名隧道**，固定域名 `dev.openaaas.org`，RP_ID 已配为该域名）
- ✅ 真实 VAPID 已加载，push 派发器已启动
- ✅ 桌面 Chrome 推送全链路已实测通过（真实 FCM sent）

**公网地址（固定）**：`https://dev.openaaas.org`

> 这是命名隧道，域名不再变化，Passkey 一次注册长期可用。

## 步骤

### 1. 打开 + 添加到主屏幕

1. iPhone **Safari** 打开公网地址（手机流量）
2. 底部分享 → **添加到主屏幕** → 从主屏幕图标打开（standalone 模式，这是 iOS 通知的硬性前提）

### 2. 注册 / 登录（Passkey）

1. 进入登录页 → 切到「注册」→ 输入用户名（如 `zheng`）
2. 按提示用 Face ID / 触控创建 Passkey → 自动进入工作台
   - 若已有账号：直接「登录」→ 识别已有 Passkey

### 3. 订阅推送

1. 进入「通知接收」→ 推送订阅 tab → 点「添加此设备」
2. 允许通知权限 → 自动注册 SW + PushManager 订阅 → 设备出现在列表（平台 iOS）

### 4. 触发服务端推送（我来做，或你执行）

1. 我用你的 `ant_send_...` Key 触发：

   ```bash
   curl -X POST https://dev.openaaas.org/v1/notify \
     -H "Authorization: Bearer ant_send_..." \
     -H "Content-Type: application/json" \
     -d '{"title":"iOS 真机测试","status":"success","body":"来自 Anotify 的真机推送"}'
   ```

### 5. 确认送达

1. **前台**：页面内应看到通知
2. **后台/锁屏**：把 App 切后台或锁屏，再次触发 → 系统通知横幅应弹出（这是 Web Push 核心价值）

## 需要你反馈的结果

| 检查点 | 期望 |
| --- | --- |
| 添加到主屏幕后能打开 | ✅ standalone 模式 |
| Passkey 注册/登录 | ✅ 免密成功 |
| 订阅后设备出现在列表 | ✅ 平台显示 iOS |
| 前台通知 | ✅ 页面弹出 |
| **后台/锁屏通知** | ✅ 系统横幅（关键） |

## 常见问题

- **收不到推送**：确认是 standalone 模式（从主屏幕图标打开，不是 Safari 标签页）；确认已允许通知权限
- **Passkey 报错**：确认 RP_ID = `dev.openaaas.org`（固定域名，不再变化）
- **隧道断开**：命名隧道不会过期。重跑 `make tunnel` 即可恢复（需本地 `./anotify` 服务在 8080 端口运行）

完成后告诉我结果，我把 T40 标记 ✅ 并收尾。
