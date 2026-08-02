# 任务 T12 · /v1/notify 上报 + 标签路由 + 双派发器

先读根目录 `AGENTS.md` 和 `.pi-orchestrator/TASKS.md` 中 T12 一节。
你在 worktree `wt-notify`（分支 wt-notify）工作，**只改 internal/api、internal/ws、internal/push 及相关 store**。

## 目标

实现通知上报与投递：HTTP `POST /v1/notify` 处理器、标签路由（按设计规则）、status 过滤、两个 Broker 消费者（WebSocket 派发器 + Web Push 派发器）。

## 契约

- Broker 接口：`internal/broker/broker.go`（Publish/Subscribe/Ack/Replay，Message 含 DeviceTags/Status/Payload）
- store：`store.Open`、ID 生成器；schema 中 `devices`/`deliveries` 表
- API 契约：`api/openapi.yaml` 的 `/notify` 与 `/stream`
- 依赖：`github.com/SherClockHolmes/webpush-go v1.3.0`、`github.com/coder/websocket v1.8.12`
- auth（由 T11 提供，本期你可先定义接口占位）：`ValidateKey(key)→(userID,scopes)`、`RequireScope("notify:send")`

## 投递规则（必须严格实现，来自设计决策）
>
> 消息投递给设备 ⟺ 设备 enabled AND statusMatch(device.status_filter, msg.status) AND tagMatch

**tagMatch 规则**：

- 消息无 deviceTags → 广播到所有 enabled 设备
- 设备无 tags → 接收一切（catch-all）
- 双方都有 tags → 求交集，≥1 个共同 tag 才投递（ANY，不是 ALL）

**statusMatch**：device.status_filter=all 通过；=error 仅 msg.status=error；=success 仅 msg.status=success。（interrupted/info/warning 仅在 filter=all 时通过）

## 要实现

1. `internal/api/notify.go`：`POST /v1/notify`
   - 校验 Bearer Key（scope=notify:send）→ 定位 userID
   - 解析 NotifyRequest（见 openapi），构造 broker.Message（deviceTags 归一化：trim/去重/≤10个/≤32字符）
   - `broker.Publish` 入队
   - 返回投递结果（matched 数量 + 每设备 status），让 Agent 能看到 "0 设备匹配"
2. `internal/push/dispatcher.go`：Web Push 派发器（消费者2）
   - `Subscribe(userID, nil)` 消费 → 按上述规则过滤设备 → 逐设备 webpush 发送（VAPID 签名，endpoint/p256dh/auth 来自 devices 表）
   - 写 `deliveries` 记录（channel=webpush，status=sent/failed，error 如 410 Gone）
   - 410 Gone 的设备标记失效（enabled=0 或删除）
3. `internal/ws/dispatcher.go`：WebSocket 派发器（消费者1）+ `internal/ws/handler.go`
   - `GET /v1/stream` 升级 WS（Bearer Key，scope=notify:receive）
   - 帧协议（JSON，见 openapi /stream 描述）：hello/notification/replay_end/pong/subscribed/error/bye（下行），subscribe/unsubscribe/ack/ping/resume（上行）
   - 断线重连：Last-Event-Id 头或 resume 帧 → `broker.Replay` 补漏 → replay_end → 实时流
   - ack 帧 → `broker.Ack`
   - 心跳：客户端 ping，服务端 pong，2 次未收到关闭
4. `internal/push/webpush.go`：VAPID 配置加载（从环境变量/配置文件读 VAPID 公私钥）
5. 测试：`internal/api/notify_test.go` + 路由过滤逻辑单测（`internal/push/filter_test.go` 或 `internal/ws/filter_test.go`）
   - 用表驱动覆盖 tagMatch/statusMatch 的所有边界（无tag消息/无tag设备/交集/ALL拒绝/status 过滤）
   - notify 处理器：有效/无效 Key、无设备时返回提示

## 约束

- gofmt；错误包装
- 与 T11 的 auth 解耦：定义小接口（如 `type KeyValidator interface{ ValidateKey(string)(string,[]string,error) }`）便于测试注入，集成时由协调者接线
- 环境：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
- 完成后 commit：`feat(notify): 上报 + 标签路由 + WS/WebPush 双派发`

## 上报

`DONE T12` + 产出清单 + `go test ./internal/...` 结果 + 遗留风险
