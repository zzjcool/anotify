# T12 进度 · /v1/notify 上报 + 标签路由 + WS/WebPush 双派发器

## 状态：✅ 完成

- [x] 读任务卡 t12-notify.md、AGENTS.md、契约（broker.go / store / schema / openapi）
- [x] internal/authn — KeyValidator 接口（与 T11 解耦）
- [x] internal/api/notify.go — POST /v1/notify 处理器
- [x] internal/route/filter.go — tagMatch / statusMatch / ShouldDeliver / FilterDevices
- [x] internal/push/dispatcher.go + webpush.go — Web Push 派发器（消费者2）
- [x] internal/ws/handler.go + protocol.go — WS 派发器（消费者1）
- [x] internal/store：devices/deliveries/messages/users 数据访问辅助
- [x] 测试：route 表驱动 + notify 处理器 + push 派发 + ws 端到端
- [x] go build 通过 + go test 全绿（api/push/ws）+ gofmt/vet 净
- [x] commit b38e3d6 + DONE 上报

## 测试结果

- go test ./internal/... → api ok / push ok / ws ok（20 个测试函数 + 42 子测试，全 PASS）

## 备注

- context.md / plan.md 在 worktree 中不存在，以任务卡为准。
- 与 T11 解耦：authn.KeyValidator 接口已定义，集成期由协调者把 internal/auth 实现接上。
- deliveries.message_id 外键引用 messages.id，生产上由 broker.Publish 持久化（T10）；
  测试用 store.InsertTestMessage 提供外键父行。
