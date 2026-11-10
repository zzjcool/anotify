## checkpoint 审查（只读）· G1-1..G1-4 + N1 19827b3

范围：`internal/store`（task/emission/event/retention/config/namespace/source/deployment）+ E1 接受事务；未审 `internal/playbook`（并行 writer）；未跑 build/test/baseline。G1-5/G2/G3 未实现项不算 missing。

### 已核对合规（非问题）
1. **E1 同事务验权顺序正确**：节点身份→任务事实（phase/node/attempt/lease/token/`lease_until`）→ns 交集→才判幂等，已有 id 也不绕过。`internal/store/emission.go:216-233`（身份）、`240-286`（attempt/lease/token/过期）、`300-330`（申报∩授权覆盖任务 ns）、`154-201`（authorize 在 idempotency 之前）。并发唯一冲突回读旧结果：`emission.go:205-216`。
2. **delivery 独立窗**：`event_deliveries` 独立表，窗判定按 `expires_at` 现算，不靠 sweeper 也不靠删除实体：`internal/store/schema.sql:300-312`、`event.go:152-176`；过窗重收刷新记录并得新 eventId：`event.go:177-213`；旧事件被 task 引用不延长业务窗（两套判据分离）：`event.go:60-70`、`retention.go:171-186`。
3. **回执不被 eventTTL/删包级联**：`playbook_installs.playbook_id` 无 live FK（`schema.sql:429-451`），sweeper 不删回执（`retention.go:110-127`），独立 primitive 带引用保护（`retention.go:236-262`）；`namespace` 显式删除才删（`namespace.go:341-348`）。
4. **config 三层/CAS**：local>node>ns 逐键、缺失不注入、双 revision 不混：`config.go:919-954`、`976-1000`；CAS 缺 rv/旧 rv 哨兵：`config.go:377-437`、`481-545`、`664-756`。

### findings（可确认 bug 与风险分列）

**F1 · major（可确认 bug）· 取消在飞的 running 任务被租约巡检重排后可再次派发，取消意图丢失**
证据：`internal/store/task.go:918-924`（running 取消只写 `cancel_reason/cancel_requested_at`）；`task.go:1083-1097`（`recoverOne` 重排 CAS 只看 `phase+attempt`，不看 `cancel_requested_at`）；`task.go:455`（`ListDispatchableTasks` 不过滤取消）；`task.go:672-683`（grant 的 UPDATE 无 `cancel_requested_at IS NULL`）。
复现：幂等能力、attempt<max 的任务 grant 后 `RequestCancel`，再令 `lease_until=now-1`/节点转 offline → `RecoverLostLeases` 回 pending（`cancel_requested_at` 仍在）→ `AssignTask`+`GrantLease` 成功、attempt=2 running，用户已取消的任务继续执行（非幂等能力同路径被 abandoned，但原因码写成 `lease_expired` 而非取消）。违反 D4「取消/放弃为终态，不自动重试」。`v4test/task_test.go:304` 只测未取消任务，无覆盖。
最小建议：`recoverOne` 重排分支先判 `CancelRequestedAt != nil` → 直接终态（`cancelPhase(CancelReason)`），并在 grant 的 CAS 上加 `AND cancel_requested_at IS NULL`。

**F2 · major（未证实，取决于 draining 产品语义）· `draining` 被当作「失联」回收 running 任务**
证据：`internal/store/task.go:1020-1026`（`node_id NOT IN (SELECT id FROM nodes WHERE status='online')`）；`internal/store/node.go:34-35` 保留 `draining`；`internal/ws/node_handshake.go:153-154` 拒绝 draining 新连接，同时 `node.go:1066` 只把 draining 排除出新派发。
后果组合：把在跑任务的节点置 draining → 下一轮巡检 abandon/重排（非幂等能力 work 可能仍在跑，结果被 `CompleteTask` 拒），且该节点已不能再连回报。若 drain 语义是「不再接新任务、跑完存量」，这就是误杀；若是「立即驱逐」，则应显式写清并让结果路径可接受。建议父裁决后二者取一（sweep 只看 `offline`，或 drain 时先 cancel）。

**F3 · minor（可确认 bug）· grant 预算用尽分支不释放已占槽位**
证据：`task.go:637-649`（`finishTx` 后 `committed=true` 返回，跳过 L601-605 的 `ReleaseSlot`）；`node.go:775-776`（`LeaseSlot` 已写 `busy_task`）；兜底只在 hello 时跑（`node.go:719-741`）。
复现：任务 attempt=1/max=3 重排为 pending 后把能力 maxAttempts 降到 1 → grant → 任务 abandoned 且 `busy_task` 仍指向它（`busySlots=1`），直到该节点下次 hello。`v4test/task_test.go:90` 未覆盖该分支。建议：该分支同时 `node_instances SET busy_task=NULL WHERE busy_task=taskID`。

**F4 · major（未证实风险，API 形状问题）· 资源写没有原子的 `resourceVersion` CAS 入口**
证据：`source.go:462-466`（UPDATE 只 bump 不校验）、`capability.go:430-434`/`633-637`、`node.go:397` 等同形；对照 `docs/design/v4/api-conventions.md:173-179`、A9/A20（写必带 RV，旧值 409）。
后果：G3 只能 Get→比较→Update（TOCTOU），无法满足「旧 RV → 409」的原子语义；需要 store 接受期望 RV（或条件 UPDATE）。若父已决定 CAS 在 G3 另加参数，请确认签名变更归属。

**F5 · minor（可确认边界 + 未证实风险）· delivery 窗依赖调用方传入，且永久记录不可回收**
证据：`event.go:77`（`windowSec` 由调用方给，store 不从 `system_config` 派生）；`event.go:161-165`（upsert 只在 `expires_at>0` 时回收）；`retention.go:205`（只删 `expires_at>0`）。后果：`eventTtlSec` 从 0 改回 N 后，老 `expires_at=0` 记录永久吞同 id 投递（「窗口过期后可重收」不成立）；调用方若传 0 会把 30 天窗变永久（G2/G3 未接，故只标风险）。

**F6 · minor（可确认不一致）· `VerifyAttemptToken` 不校验租约未过期**
证据：`task.go:1129-1155`（token/phase/取消三项）vs `emission.go:283-285`（emit 路径有 `lease_until`）。E1 父规则只要求 emit 接受校验租约（已满足）；但同一函数被声明为 task.result 的权威校验（`task.go:1119-1128` 注释），过期租约到巡检前仍可写终态。建议在 result 路径（G2）复用 emit 的过期判定或在注释里写明例外。

### 待验证
- E1 两帧 `task.emit`/`task.emit.ack` 不在本 worktree `internal/protocol`（`grep task.emit` 无命中）；按批准文件「N1 已提交则新增第二个纯 protocol commit」属 T3 待交付，非 T1 finding。
- `internal/lease`、`internal/ws`、`internal/server` 仍是 v3 签名（`SweepRetention/SweepExpiredLeases/ListRedispatchable` 已不存在），整体不可编译属阶段预期。

```json
{"ok": false, "reason": "G1 checkpoint review: 1 confirmed major (cancel-in-flight task requeued by lease sweeper then re-granted: internal/store/task.go:1083 + :455 + :921), 1 minor slot leak (task.go:637), 4 risks (draining-as-lost task.go:1020; missing atomic resourceVersion CAS source.go:462; permanent delivery window event.go:161; VerifyAttemptToken no lease-expiry check task.go:1129). E1 same-tx auth, delivery window, receipt/reference GC verified compliant; 6 findings, 0 critical"}
```
