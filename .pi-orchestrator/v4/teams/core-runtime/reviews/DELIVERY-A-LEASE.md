# A-continue Security-Mapping Review (READ-ONLY)

Canonical cwd `/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery` (pwd/gitroot verified, HEAD `55e3e46` + WIP). Old side read via `git show b5adeda8…:internal/store/{session_lease_test.go,session_lease_safety_test.go,tx_merge_test.go}` (no oldWT/session/root files read). Mapping source: `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md:984+`. No compile/vet/test/write performed.

## 1. Mandated "must have exact real named replacement" categories — all PASS

| category | exact v4 named test (path:line) | meaningful assertions verified |
|---|---|---|
| wrongbinding | `internal/store/task_test.go:379 TestRenewLeaseAuthorized` (`:399` wrong attempt, `:402` wrong lease → `ErrLeaseMismatch`); `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry`; `tx_merge_test.go:115` cross-attempt → `ErrLeaseMismatch` | real `NodeAuthorization`; phase/binding unchanged; slot kept (`v4test:1153 busySlots==1`) |
| expiry | `internal/store/v4test/task_test.go:1423 TestRenewLeaseCannotReviveExpiredAuthorization` (expiry+0/+1; `reflect.DeepEqual` before/after); `v4test/task_test.go:1124` (expired lease result + expired attempt token rejected) | zero-write on expired renewal; expired token → `ErrLeaseMismatch`; valid window later succeeds and releases slot |
| cancel | `v4test/task_test.go:234 TestTaskCancelWaitsForReap` (not-reaped→no slot release `:258`; old-attempt ack `ErrLeaseMismatch` `:268`; reaped→slot released `:280`); `:875 TestTaskCancelInFlightNeverRegranted`; `:287` pending-cancel | exact; store-level `task_test.go:526 TestCancelRunningAwaitsAck` additionally red-pending-B (not an A defect) |
| current-newslot preservation | `v4test/task_test.go:763 TestTaskOldAckCannotReleaseNewAttemptSlot` | wrong-lease reap fail-closed, `busySlots==2` kept (`:833`), old reap frees only old slot (`:847`), new attempt phase/lease untouched (`:850`), repeat idempotent (`:858`), current cancel frees own slot (`:868`) |
| auth grant | `internal/store/node_test.go:829 TestNodeAuthorizationGrant` (bad token hash → no slot `:852`; real grant occupies; second task same slot `ErrSlotUnavailable` `:884`); `task_test.go:314 TestGrantMovesToRunning` (token hash only, verify round-trip); `v4test/task_test.go:19` | no fake auth: helpers `v4Auth`/`v4Grant` use real `GrantLeaseAuthorized` (`v4_fixture_test.go:192,259`), not SQL |
| transaction rollback / foreignslot | **restored** `internal/store/tx_merge_test.go:66 TestReportResultDoesNotReleaseForeignSlot` (real 2-slot fixture; A released, B slot kept `:106`, B lease/attempt unchanged `:109`); `tx_merge_test.go:115` slot + busy lease/attempt unchanged `:169-181`; `task_test.go:357` rejected grant → `v4BusySlots==0` (`:374`) | production release is exact-key single SQL point (`node.go:1070` instanceID+task+attempt; completion `execution_auth.go:866`), so assertions are non-vacuous |

Conclusion: the six mandated safety categories are genuinely restored/covered by real named tests with non-trivial DB-fact assertions. `productionC3` not reopened.

## 2. Findings (production untouched)

- **minor** — `TestCreateTasksRejectsOversizedParams` (old `internal/store/session_lease_test.go:697`) marked "migrated" but narrowed: `v4test/task_test.go:555` only `errors.Is(ErrParamsTooLarge)` (test comment at `:547` claims "一条不合格一条都不建" with a **single-task** batch). True (narrow) assertion loss: old "整批原子回滚" (good+oversized→0 rows), "正好到上限通过并往返", `*ParamsTooLargeError{Size,Limit,TaskID}` diagnostics are unasserted. Batch-rollback mechanism itself is protected by uncited `internal/store/task_test.go:103 TestCreateTasksAtomic` (mixed batch, FK failure→0 rows), so not classified major.
- **minor** — mapping anchor line drift: `tx_merge_test.go:111` → actual decl `:115`; `node_test.go:821 TestNodeAuthorizationGrant` → actual `:829` (`.pi-orchestrator/.../TEST-MAPPING.md:1035,1039`).
- **minor** — attempts-exhausted row cites `v4test/task_test.go:93 TestTaskMaxAttemptsTerminal` as the budget-exhaustion replacement, but that test abandons via non-idempotent lease expiry; the exact budget assertion is `v4test/task_test.go:1030 TestTaskGrantBudgetExhaustedReleasesSlot` (`ErrMaxAttemptsExceeded`, abandoned, reason `max_attempts_exceeded`, `busySlots==0`), uncited (`TEST-MAPPING.md:1014`). Coverage exists; citation wrong.
- **minor** — restored-name prose overstates: `TEST-MAPPING.md:1035` claims "旧任务终态事实都不变", but `tx_merge_test.go:115` force-resets terminal→pending (`:139` raw UPDATE) and asserts only current binding; terminal-replay zero-write lives in `v4test/result_current_binding_test.go:18` (cited in same row). No coverage loss, mapping text inaccurate.
- **minor** — `TestReclaimStaleSlots` row (`TEST-MAPPING.md:1028`) omits deliberate semantic change: phase-inferred stale-slot clearing is gone; positive reclaim now requires explicit node idle observation with matching task/attempt/lease (`v4test/cluster_node_test.go:518-549`); live/unknown/wrong-lease fail-closed directions preserved. Not an invariant loss, but not "same invariant".
- **minor** — lost-executor recovery rows (`TEST-MAPPING.md:1020-1026`) imply old slot-release/immediate-redispatch equivalence; v4 deliberately keeps `busy_task` until trusted facts (`internal/store/task.go:1046-1051` recoverOne comment; `v4test/task_test.go:307` asserts busy retained, retry_backoff/next_attempt_at, then explicit `ReclaimStaleSlots`+re-grant). Replacement is meaningful; semantic change unstated.

## 3. B-runtime-pending vs A compile-only (not flagged as A missing)

- Cited store replacements among the mapping's own 31 reds: `task_test.go:408/462/498/526/608/654` (`TestCompleteRecordsResultAndFinishedAt`, `TestCompleteRequiresGrant`, `TestTerminalPhaseIsImmutable`, `TestCancelRunningAwaitsAck`, `TestMaxAttemptsExceededAbandons`, `TestTaskAttemptSnapshotRoundTrip`). Assertions inspected are real (not weak), but never executed green in this tree — B must run them (`TEST-MAPPING.md:1190-1208`).
- All `v4test` replacements inspected are in the package that previously ran `ok`; A claims only compile closure, consistent with `TEST-MAPPING.md:986-988`.

## 4. Bounded checklist (missing old names / replacements only)

1. Old `session_lease_test.go:697 TestCreateTasksRejectsOversizedParams`: add mixed-batch oversized→0-rows, exact-limit accepted+roundtrip, and `ParamsTooLargeError` field assertions (or explicitly mark row "narrowed" and cite `task_test.go:103`).
2. Fix anchors: `TEST-MAPPING.md:1035` `:111`→`:115`; `:1039` `:821`→`:829`.
3. Replace `:1014` secondary anchor `v4test:93` with `v4test:1030 TestTaskGrantBudgetExhaustedReleasesSlot` (or state both).
4. Amend rows `1028` and `1020-1026` to state ReclaimStaleSlots observation-based narrowing and recovery-keeps-busy semantics.
No new replacement test is required for wrongbinding/expiry/cancel/current-slot/auth-grant/foreignslot.

## 5. 待验证

- Whether an "acked"-phase variant of old `TestResolveLostTasksAbandon` has any v4 analogue (v4 phase set not exhaustively checked; mapping did not claim one).
- `TestC3b*` helper `registryFacts` breadth (assumed row-level Task/attempt/artifact facts) — not expanded per scope bound.

```json
{"ok": true, "reason": "A security mapping sufficient: 6 mandated categories have exact real named v4 replacements (task_test.go:379/357, v4test/task_test.go:1124/1423/234/763/1030, node_test.go:829, restored tx_merge_test.go:66/115) with meaningful real-auth assertions; only minor narrowing (oversized-params sub-assertions) and mapping anchor/prose drift; store reds are B-runtime-pending, not A loss"}
```
