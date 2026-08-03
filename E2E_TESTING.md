# Anotify 端到端测试体系（固化方案）

> **规则：每次开发完成后，必须跑 `make e2e` 全绿才算完成。** 这是固化的质量门禁。
> 除「iOS 真机 APNs 推送」外，**所有环节全自动，无需人工**。

## 技术选型

| 层 | 技术 | 覆盖 |
| --- | --- | --- |
| 单元 | `go test ./...` | broker/auth/api/push/ws/store/server 包级逻辑 |
| API 契约 | Node fetch（`scripts/e2e/suites/api_contract.mjs`） | 全端点的状态码/鉴权/参数校验 |
| **Passkey 认证** | **Playwright CDP 虚拟认证器** | 真实 WebAuthn 注册/登录/登出/会话（无头，无需真人） |
| WS 协议 | Node WebSocket 客户端 | hello/subscribe/ack/resume/heartbeat/标签过滤 |
| 路由过滤 | Node fetch | 标签路由 + status 过滤全矩阵（notify.matched 断言） |
| 桌面推送 | Playwright 持久化 Chrome | 真实 FCM 订阅 → 投递记录 |
| 持久化 | 重启服务验证 | 消息/设备/Key 重启后仍在 |
| 安全 | Node fetch + DB 检查 | scope 越权/Key 篡改/哈希格式/会话属性 |
| 前端 | Playwright | 6 页渲染 + 未登录跳登录 + 已登录真实数据 |
| 边界 | Node fetch | 畸形输入/超大体/并发/Unicode |
| Docker | docker build/run | 单二进制镜像端到端 |

## 唯一人工环节

- **iOS Safari 真机 APNs 推送**：需真实 iPhone，无法自动化（见 `IOS_TESTING.md`）。
  其余全部自动。

## 目录结构

```
scripts/e2e/
  run_all.sh            # 总编排：构建→逐套件运行→汇总报告
  lib/harness.mjs       # 服务生命周期 / HTTP 客户端 / 断言 / 播种
  suites/
    api_contract.mjs    # API 契约矩阵
    auth_flow.mjs       # Passkey 全流程（虚拟认证器）
    ws_protocol.mjs     # WS 帧协议
    routing.mjs         # 标签/status 投递矩阵
    persistence.mjs     # 重启持久化
    security.mjs        # 安全矩阵
    edge_cases.mjs      # 边界/并发/Unicode
    frontend.mjs        # 前端渲染 + 路由守卫
    push_e2e.mjs        # 桌面推送（真实 FCM）
```

## 运行

```bash
make e2e          # 全量端到端（构建 + 所有套件）
make e2e-one S=auth_flow   # 只跑某个套件
```

## 测试矩阵（全 case 清单）

见各 suite 文件顶部注释。总计 **100+ 断言**，覆盖：

- 每个 API 端点的 成功/401/403/400/404 路径
- Passkey 注册→登录→登出→会话失效 全生命周期
- WS 全部帧类型 + 断线续传
- 投递规则全部边界（无tag消息/无tag设备/交集/ALL拒绝/status 5 种 × filter 3 种）
- 并发 seq 单调性、畸形输入、SQL 注入安全性、Key 哈希不可逆
