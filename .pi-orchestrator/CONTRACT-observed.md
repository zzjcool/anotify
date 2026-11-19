# 冻结契约 · 能力 `status.observed`（方案 b）

基线：`main @ b90bf5ef`，分支 `feat/capability-observed`（不合 main、不推 main）。

## 语义（以 `docs/design/v4/capability.md:403-411` 为准，不自创字段）

```json
"observed": {
  "taskId": "tsk_...",                                  // 最近一个逻辑 task；无历史时 ""
  "emits":  [{"to": "analyze-pr", "fields": ["repo"], "ok": true}],
  "pruned": [{"to": "lint-fix", "reason": "param_missing", "detail": "missing required param: diff"}],
  "tasks":  {"total": 12, "failed": 3}
}
```

- 无数据 → 返回 `{}`（字段全空时不写任何键），**保持今天的向后兼容**；绝不报错、绝不 500。
- `tasks.total/failed` 是**近 24h** 窗口内该能力的逻辑任务数与 failed 数（window 常量写进代码注释）。

## 查询与上限（必须在注释与报告里写清）

1. 最近任务：`tasks WHERE namespace_id=? AND capability=? ORDER BY created_at DESC, id DESC LIMIT 1`
   → **不需要新索引**：既有 `idx_tasks_ns_created(namespace_id, created_at, id)` 反向扫即可（实测 EXPLAIN
   零排序；加新索引反而多一个 temp B-tree）。24h 窗口计数才需要
   `idx_tasks_cap_created ON tasks(namespace_id, capability, created_at DESC, id)`。

   **索引落地方式（2026-11-17 裁决：方案 B）**：v4 尚未发布，按 `AGENTS.md §3.2` 的用户批准例外，
   索引直接写进 `schema.sql` 的 **baseline 块**，`headVersion` 保持 1 —— 因为迁移引擎硬校验
   「只有一个 baseline 块」（`migrate.go:506-507`）且 `headVersion=1`（`migrate.go:32-37`），
   追加 `-- +migration 2` 会让 `internal/store` 与 `v4test` 整片红（已实测）。代价：已存在的本地
   dev 库不会补建该索引（需删库重建），CI/e2e 用 fresh 库不受影响；这条写进交付说明。
2. emits：`emits WHERE task_id=? ORDER BY created_at, id LIMIT 50`（既有 `idx_emits_task`）。
   `to` ← `to_ref`，`fields` ← `data` 的**键名**（排序后），`ok` 由 `result` JSON 判定
   （规则写进注释；无 result 视为 ok）。
3. pruned：`events WHERE namespace_id=? AND trace_id=? AND source='route'`（既有 `idx_events_trace`），
   再用 `payload.taskId == 最近任务 id` 过滤，`LIMIT 50`；`to`←`payload.capability`、
   `reason`←`event.type`、`detail`←`payload.detail`。
4. 计数：同一索引窗口扫描（`created_at >= now-24h`），`total = count(*)`、`failed = count(phase='failed')`。
   不做全表扫：必须 EXPLAIN 证明走 `idx_tasks_cap_created`（把 EXPLAIN 输出贴进报告）。

## 代码面

- `internal/store/capability_observed.go`（新）：`ObservedForCapability(ctx, namespaceID, capability string) (CapabilityObserved, error)`，
  上限常量 `ObservedEmitLimit = 50` / `ObservedWindow = 24h` 导出供测试断言。
- `internal/server/resource_capability.go`：`statusFor` 里把 `Observed: map[string]any{}` 换成真实结果
  （空结果仍写空对象）；查询失败 **降级为空对象 + 一条 slog.Warn**，不许让列表/详情 500。
- OpenAPI `api/resource.yaml`：`CapabilityStatus.observed` 从自由 object 改成具名 schema
  （`CapabilityObserved` + `CapabilityObservedEmit` + `CapabilityObservedPrune` + `CapabilityObservedTasks`，
  `additionalProperties:false`），同步 `openapi_parity_test.go` 的 schema/配对棘轮。
- 前端：`web/mock.js` 的 observed 生成保留（本来就有），只对齐字段名；`capability.html` 的 observed 区块
  接真实数据（emits/pruned 两个列表 + tasks 计数），无数据仍是空态文案。

## 测试

- store：`TestObservedForCapabilityEmpty`（无数据 → 零值）、`...WithFacts`（emit/prune/计数往返）、
  `...Limits`（>50 条 emits 只回 50；窗口外任务不计入）。
- server：`TestCapabilityStatusObserved`（有事实时 envelope 里字段齐全；无事实时 `{}`）。
- e2e：`f3-capability.mjs` 增加「详情页 observed 有数据时显示 emits/pruned」的断言（用 e2e 造的真实任务链）。

## 门禁

`make fe && make contract && make ci`，以及 `./scripts/e2e/run_all.sh frontend`。全绿才算完成。
