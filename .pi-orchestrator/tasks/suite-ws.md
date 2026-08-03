# 任务：编写 E2E 套件 `scripts/e2e/suites/ws_protocol.mjs`

先读 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md`（公共约定）。

写 WebSocket 帧协议套件。WS 端点：`GET /v1/stream`（Bearer Key，scope=notify:receive）。用 H.seed 拿 sendKey/recvKey。Node 22 有全局 WebSocket（或用 `ws` 风格）。注意 WS URL = base 的 http→ws 替换 + /v1/stream，鉴权用子协议或 Authorization 头（Node WebSocket 支持 `new WebSocket(url, {headers:{Authorization:"Bearer "+key}})`）。

帧协议（见 api/openapi.yaml /v1/stream 与 internal/ws/protocol.go）：

- 下行：hello / notification / replay_end / pong / subscribed / error / bye
- 上行：subscribe / unsubscribe / ack / ping / resume

覆盖 case（逐条实现并断言）：

1. 无 Key 连接 → 连接被拒（error/close/401）
2. send scope Key（无 receive）连接 → 403/拒
3. recv Key 连接 → 收到 hello 帧（含 protocol 字段）
4. 发 {"type":"ping"} → 收到 {"type":"pong"}
5. 连接后用 sendKey POST /v1/notify 发一条 → WS 收到 notification 帧（含 event_id/title/seq），字段正确
6. 收到 notification 后发 {"type":"ack","event_ids":[event_id]} → 无 error 帧
7. 标签过滤：连接后 subscribe {"type":"subscribe","tags":["ops"]} → 收到 subscribed；发一条 deviceTags=["ops"] 的通知 → 收到；发一条 deviceTags=["other"] 的通知 → 不收到（等超时确认未到）；发一条无 deviceTags 的广播 → 收到
8. 断线续传：先发 2 条通知记下 seq；断开；重连时带 Last-Event-Id 头=第一条的 seq（或 resume 帧）→ 收到 replay 的后续消息 + replay_end
9. 未知帧类型 → error 帧或忽略（按实现，不断连即可）

参考现有脚本 `scripts/ws_test.mjs`（已验证 hello/notification/ack 流程）。每步给足超时（如 2-3s）。自测跑通（exit 0）后上报。若发现协议与文档不符，记录为产品 bug 上报。
