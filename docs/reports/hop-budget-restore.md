# 派发预算恢复（max_hops / budget）+ playbook 包内成环放开

Fixes #655 · 基线 `origin/main` @ `6c3ac370`

## 1. 缺陷

`docs/contracts/CONTRACT.md` A7（「环合法 + hop 预算」）是空头承诺：

- v3 的 `route.Dispatcher`（`e552bd34^:internal/route/dispatch.go`）在**建任何东西之前**判两个预算：
  `len(in.Path) >= MaxHops` → `pruned/max_hops`；`CountTasksByTrace >= TraceTaskBudget` → `pruned/budget`。
- `e552bd34` 用 `route.Router` 取代 `Dispatcher`，两个预算没有搬过去：
  `ANOTIFY_MAX_HOPS` / `ANOTIFY_TRACE_TASK_BUDGET` 只在 `internal/server/config.go` 读进 config、
  `mux.go` 打一行「不再消费」的日志；`pruneReasons` 词表里也没有 `max_hops` / `budget`。
- 后果：环能建（两个能力互指仍然 201），但**没有刹车**——A7 承诺的兜底不存在。

同一 PR 里要放开的打包规则：`internal/playbook/render.go` 的 `checkCycle` 拒绝「包内 next 成环」，
与平台语义冲突（平台允许环，由预算兜底）。

## 2. 本报告的待填项

（实施中）
