# DONE T12 · /v1/notify 上报 + 标签路由 + WS/WebPush 双派发器

工作区：`/Users/zheng/code/anotify/.pi-orchestrator/worktrees/wt-notify`（分支 wt-notify）
提交：`a807f98 feat(notify): 上报 + 标签路由 + WS/WebPush 双派发`

## 实现概要

按任务卡 `t12-notify.md` 完成全部 5 项要求。投递规则严格落地设计决策：
**消息投递给设备 ⟺ enabled AND statusMatch(device.status_filter, msg.status) AND tagMatch**。

### 产出文件

**路由（共享纯逻辑）**

- `internal/route/filter.go` — `StatusMatch` / `TagMatch` / `ShouldDeliver` / `FilterDevices`
  - 无 tag 消息=广播；无 tag 设备=catch-all；双方有 tag=ANY 交集（非 ALL）；大小写不敏感
  - status 过滤：all 全放行；error 仅 error；success 仅 success；interrupted/info/warning 仅在 all 时通过

**HTTP 上报**

- `internal/api/notify.go` — `POST /v1/notify`：Bearer Key 校验（scope=notify:send）→ 解析 NotifyRequest →
  deviceTags 归一化（trim/去空/大小写不敏感去重/≤10 个/≤32 字符）→ `broker.Publish` →
  返回 `{id, matched, results[]}`（含每设备投递预览，让 Agent 看到 "0 设备匹配"）
- `internal/api/notify_test.go` — 有效/无效/缺失 Key、scope 不足(403)、各类 400、0 设备(matched=0)、归一化边界

**Web Push 派发器（消费者2）**

- `internal/push/dispatcher.go` — `Run`(常驻订阅消费) + `Dispatch`(可同步/测试调用)；
  过期消息跳过；`Sender` 抽象便于注入；410/404 → 禁用设备；写 `deliveries`(webpush, sent/failed, error)
- `internal/push/webpush.go` — VAPID 配置加载（env `ANOTIFY_VAPID_*` 或 `ANOTIFY_VAPID_FILE`，兼容原型 vapid.json）
- `internal/push/filter_test.go` — 表驱动覆盖 statusMatch/tagMatch/ShouldDeliver/FilterDevices 全边界
- `internal/push/dispatcher_test.go` — 命中设备发送、过期跳过、410 禁用 + deliveries 落库

**WebSocket 派发器（消费者1）**

- `internal/ws/protocol.go` — 帧协议：下行 hello/notification/replay_end/pong/subscribed/error/bye；上行 subscribe/unsubscribe/ack/ping/resume
- `internal/ws/handler.go` — `GET /v1/stream` 升级（Bearer scope=notify:receive）；
  hello → (Last-Event-Id/resume 断线续传 → broker.Replay 补漏 → replay_end) → 实时流；
  ack→broker.Ack；心跳=仅客户端活动续命（2×heartbeat 无 ping 则 bye+关闭）；订阅标签过滤
- `internal/ws/handler_test.go` — 端到端（hello→实时 notification→ping/pong）、replay 补漏、无效 Key 拒绝、订阅标签过滤

**鉴权解耦 & store 辅助**

- `internal/authn/authn.go` — `KeyValidator` 接口 + `Authenticate` + scope 常量（与 T11 `internal/auth` 解耦，集成期由协调者接线）
- `internal/store/devices.go` `deliveries.go` `messages.go` `users.go` — 设备 CRUD/Upsert/Disable/TouchDelivered、deliveries 写入、消息插入（测试外键父行）、用户插入

## 自测命令与结果

```
go build ./...            → 通过
go vet ./...              → 通过
gofmt -l internal/        → 空（净）
go test ./internal/...    → ok api / ok push / ok ws（20 个测试函数 + 42 子测试，全 PASS）
```

## 遗留风险

1. **auth 接线在集成期**：`authn.KeyValidator` 是接口占位；T11 的 `internal/auth` 真实实现（argon2id 校验）需协调者在 main 里接到 notify/ws 处理器。当前测试用 stub。
2. **deliveries 外键依赖 broker 持久化**：`deliveries.message_id` 引用 `messages.id`，生产上由 T10 `SQLiteBroker.Publish` 落库后父行才存在。push dispatcher 单测用 `store.InsertTestMessage` 模拟。集成时需确认 T10 Publish 先于 dispatcher Dispatch。
3. **WS 真实 Push 链路未端到端验真机**：webpush-go 发送已对 `Sender` 抽象打桩测试；真实 VAPID 投递（Apple/FCM）需阶段3 桌面 Chrome / iOS 真机验证。
4. **Push dispatcher 编排入口未定**：`Run(ctx, userID)` 是常驻消费者，「何时为哪个用户启动」由上层（集成期 main）编排，本期未含主程序。