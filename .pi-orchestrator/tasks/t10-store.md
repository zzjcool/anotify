# 任务 T10 · SQLiteBroker 实现

先读根目录 `AGENTS.md` 和 `.pi-orchestrator/TASKS.md` 中 T10 一节，遵守统一约定。
你在 worktree `wt-store`（分支 wt-store）工作，**只改 store/broker 相关文件**。

## 目标

实现 `internal/broker` 的 SQLiteBroker（`Broker` 接口，见 `internal/broker/broker.go`），以及配套的 store 数据访问。

## 契约（已实现，直接复用）

- `internal/store/store.go`：`store.Open(path)` 返回 `*store.DB`（已设 WAL/busy_timeout/foreign_keys，单连接）
- `internal/store/ids.go`：`NewMessageID()` 等 ID 生成器
- `internal/broker/broker.go`：`Broker` 接口 + `Message` 结构体 + `Subscription` 接口
- schema：`internal/store/schema.sql`（messages/consumer_offsets/deliveries 等表已定义）

## 要实现

1. `internal/broker/sqlite.go`：SQLiteBroker 实现 `Broker` 接口
   - `Publish`：事务内 `SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE user_id=?` 生成 seq，插入 messages（含 payload JSON、expires_at=created_at+ttl）；提交后**进程内广播**给该用户的所有活跃订阅
   - `Subscribe`：返回一个 `Subscription`（带缓冲 channel），按 `tags` 过滤（tags 为空=全订阅；消息 DeviceTags 与其有交集即投递）；broker 维护 userID→subscriptions 映射
   - `Ack`：`INSERT ... ON CONFLICT(consumer_id,user_id) DO UPDATE SET last_seq=MAX(last_seq, excluded.last_seq)`（high-water 只前进）
   - `Replay`：`SELECT * FROM messages WHERE user_id=? AND seq>? ORDER BY seq ASC LIMIT ?`
   - 进程内广播 + DB 回放结合（Phase 1 单进程，快路径走内存、稳路径走 DB）
   - 过期清理：`DeleteExpired(ctx, retentionSeconds)` 删除 `created_at < now-retention` 的历史（deliveries 级联删）
2. `internal/broker/sqlite_test.go`：单测
   - Publish 后 Replay 能读回、seq 单调递增
   - Subscribe 实时收到 Publish 的消息；tags 过滤正确
   - Ack high-water 只前进
   - 过期清理删除旧消息
   - 用 `:memory:` 或 t.TempDir()

## 约束

- gofmt；错误用 `fmt.Errorf("...: %w", err)`
- 环境：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
- 不要改 `broker.go` 接口签名（如需调整在上报里说明理由）
- 完成后 `git -C <worktree> add -A && git commit -m "feat(broker): SQLiteBroker 实现"`

## 上报格式

按 AGENTS.md：`DONE T10` + 产出文件清单 + 自测命令与结果（`go test ./internal/broker/...`）+ 遗留风险
