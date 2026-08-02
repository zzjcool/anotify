# DONE T10

## 产出文件

- `internal/broker/sqlite.go` — SQLiteBroker 实现（Broker 接口）
- `internal/broker/sqlite_test.go` — 单测（7 个测试全过）

## 实现要点（对照任务卡）

1. **Publish**：事务内 `SELECT COALESCE(MAX(seq),0)+1 ... WHERE user_id=?` 生成 seq → 插入 messages（payload JSON、`expires_at=created_at+ttl`）→ 提交后**进程内广播**给该用户活跃订阅。ID/Status/Priority/TTL/CreatedAt 缺省自动补全，CreatedAt+TTL 推导出 ExpiresAt。
2. **Subscribe**：返回带缓冲 channel(64) 的 `sqliteSub`，broker 维护 `userID→subs` 映射；ctx 取消自动退订；Close 幂等。
3. **Ack**：`INSERT ... ON CONFLICT(consumer_id,user_id) DO UPDATE SET last_seq=MAX(last_seq, excluded.last_seq)`（high-water 只前进），并刷新 updated_at。
4. **Replay**：`SELECT ... WHERE user_id=? AND seq>? ORDER BY seq ASC LIMIT ?`（默认 limit=100）。
5. **DeleteExpired(ctx, retentionSeconds)**：删除 `created_at < now-retention` 的历史（deliveries 级联删），返回删除行数；默认 90 天。
6. 快/稳路径分离：实时走内存广播，回放/历史/崩溃恢复走 DB。

### 标签路由（matchTags）

- 订阅 tags 为空 = 全订阅（catch-all，接收一切）
- 消息 DeviceTags 为空 = 广播
- 双方都有 = 求交集，≥1 命中即投递（ANY，非 ALL）

## 自测命令与结果

- `go build ./...` → 通过
- `go vet ./internal/...` → 通过
- `gofmt -l internal/` → 空（clean）
- `go test ./internal/broker/... -v -count=1` → **PASS (7 tests)**
  - TestPublishReplaySeqMonotonic（读回 + seq 单调递增 + 多用户独立 + sinceSeq）
  - TestSubscribeReceivesPublished（实时收到）
  - TestSubscribeTagFilter（7 个子用例覆盖路由边界）
  - TestAckHighWaterForwardOnly（只前进）
  - TestDeleteExpired（清理 + deliveries 级联删）
  - TestPublishRoundTripFields（payload/tags/expires_at 往返）
  - TestSubscribeCloseStopsDelivery（Close 后不再收）

## git

- worktree `wt-store`，分支 wt-store，commit `ed071a2`
  `feat(broker): SQLiteBroker 实现（Publish/Subscribe/Ack/Replay + 进程内广播 + 过期清理）`
- 工作区干净，无未提交/暂存文件

## 设计决策（不影响接口契约，供 reviewer 知悉）

- **订阅 channel 不在 Close 时 close**：避免 broadcast 在持有读锁遍历时向已关闭 channel 发送导致 panic。Close 通过 `done` + 从映射移除来停止投递；channel 缓冲由 GC 回收。这是典型的「生产者不关闭由消费者持有的 channel」模式。
- **broadcast 非阻塞**：channel 满则跳过实时推送，慢消费者靠 Replay(sinceSeq) 补漏，与"至少一次投递 + 消费方按 Seq 去重"契约一致。
- **未改 `broker.go` 接口签名**（按任务卡约束）。

## 遗留风险

- 单连接 SQLite（store 已 SetMaxOpenConns(1)），高并发下 Publish 串行化——Phase 1 个人/小团队量级足够，符合"数据库先行"设计。
- `DeleteExpired` 是"历史保留"清理（按 created_at），与"可投递性"（expires_at）解耦，未提供"仅按 expires_at 标记不可投递"的辅助——投递器在发送前需自行判断 expires_at（任务卡范围内未要求该辅助方法）。