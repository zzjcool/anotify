# T12 进度 · /v1/notify 上报 + 标签路由 + WS/WebPush 双派发器

## 状态：进行中

- [x] 读任务卡 t12-notify.md、AGENTS.md、契约（broker.go / store / schema / openapi）
- [ ] 定义 auth 解耦接口（KeyValidator / ScopeChecker）
- [ ] internal/api/notify.go — POST /v1/notify 处理器
- [ ] 路由过滤逻辑（tagMatch / statusMatch / shouldDeliver）
- [ ] internal/push/dispatcher.go + webpush.go — Web Push 派发器（消费者2）
- [ ] internal/ws/handler.go + dispatcher.go — WS 派发器（消费者1）
- [ ] 测试：filter 表驱动 + notify 处理器
- [ ] go build + go test 全绿 + gofmt
- [ ] commit + DONE 上报

## 备注

- context.md / plan.md 在 worktree 中不存在，以任务卡为准。
- devices 表读写、deliveries 写入需要新增 store 辅助方法（任务卡允许改"相关 store"）。
