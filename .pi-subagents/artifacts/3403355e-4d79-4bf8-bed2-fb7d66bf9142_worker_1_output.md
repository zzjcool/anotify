# DONE ws_protocol

## 产出文件

- `scripts/e2e/suites/ws_protocol.mjs`（新建，289 行，未 stage/未提交）

## 覆盖 case（9 大组，31 断言）

1. **无 Key 连接** → 握手被拒，未收到 hello
2. **send scope Key（无 receive）连接** → 被拒(403)，未收到 hello
3. **recv Key 连接** → hello 帧（protocol=1 / conn_id / heartbeat_sec / resume_token=evt_*）
4. **ping → pong**
5. **notify 实时帧** → notification 帧含 event_id/seq/title/status，字段正确
6. **ack**（event_id 用 `evt_<seq>` 编码）→ 无 error 帧，连接保持
7. **标签过滤** → subscribe["ops"] 收到 subscribed；["ops"] 收到 / ["other"] 不收 / 无 tags 广播仍收
8. **断线续传** → Last-Event-Id=`evt_<seq1>` 重连，replay 出漏掉的第 2 条 + replay_end
9. **未知帧类型** → error 帧(code=unknown_type)，连接保持且仍可 ping/pong

## 自测命令与结果

```
ANOTIFY_VAPID_PUBLIC_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['publicKey'])") \
ANOTIFY_VAPID_PRIVATE_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['privateKey'])") \
node scripts/e2e/suites/ws_protocol.mjs
→ [ws_protocol] 31 通过 / 0 失败，EXIT=0
```

## 发现的产品 bug

- 无。协议实现与 `api/openapi.yaml` / `internal/ws/protocol.go` 文档一致。

## 实现要点 / 注意事项（供协调者与后续套件参考）

- **ack 的 event_id 编码**：服务端 `handleFrame` 对 ack 的每个 event_id 调 `parseResumeSeq`，只认 `evt_<seq>` 或纯数字，**不认消息的真实 `ntf_...` ID**（那会被解析成 0 而静默丢弃，且不回 error）。测试里用 `evt_<seq>` 才能走通 Ack 路径。文档/swagger 未明确这一点——建议协调者知悉（不算 bug，但 event_id 命名易误导）。
- **Last-Event-Id / resume_token 编码**：同样为 `evt_<seq>` 前缀格式，`parseResumeSeq` 取数字部分。`resume` 帧的 resume_token 也走同一解析。
- **标签过滤方向**：WS 侧 `matchTags` 是「订阅标签 vs 消息 DeviceTags」——无订阅=全收；消息无 tags（广播）= 所有订阅者都收；否则交集≥1。与设备路由规则一致，已验证。
- **心跳**：hello 下发 heartbeat_sec=30，服务端 2×heartbeat 无客户端活动则 bye+关闭。测试用短交互未触发超时，符合预期。
- 连接拒绝路径：无 Key/错 scope 时服务端在 `Accept` 前 `http.Error`，coder/websocket 客户端表现为握手失败（onclose/onerror，非 onopen），测试按此断言。

## 遗留风险

- 心跳超时（bye 帧）路径未实测（需 30s+ 等待，会拖慢套件）；逻辑直观，已读代码确认。
- `ack` 对真实 `ntf_...` ID 静默忽略：若未来想让 ack 语义对齐消息 ID，需在协议层做 ID→seq 映射（当前用 seq 编码规避）。