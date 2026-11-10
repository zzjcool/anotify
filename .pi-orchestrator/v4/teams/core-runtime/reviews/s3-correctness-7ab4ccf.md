# Interim adversarial review · G1-6S @ `7ab4ccf35dc335caedd33d2182c8c5e4f6dddc19`

**Scope/evidence**: `internal/store/{task.go,task_attempt.go,node.go,config.go,retention.go}` + `internal/store/v4test/*`. `git diff 7ab4ccf -- <these five>` is empty, so WT contents equal the fixed commit for scoped files. Static review only; no tests/build/schema run (per constraint). Emission authorization (`emission.go`) is outside scope and not independently revalidated.

## Frozen summaries: what actually holds

**D2 — namespace first pin; per-attempt machine snapshot immutable; retry on another node keeps ns input**
- `createTasksTx` pins `namespace_configs` inside the insert transaction: `task.go:307-335`; no code updates `tasks.namespace_vars`.
- `GrantLease` loads node config only inside the grant tx (`task_attempt.go:258-317`) and inserts the snapshot in that tx (`task.go:954-970`); only INSERTs exist for `task_attempts`; later UPDATEs touch phase/error/finished/reaped only (`task_attempt.go:351-455`).
- `TaskInputs` reads only `task_attempts` + pinned `Task.NamespaceVars`; no fallback to current `node_configs` (`task.go:392-441`). Tests: `task_inputs_test.go:23,72,185`, `task_attempt_test.go:64`.
- Verified: same-attempt config/hello change does not drift; cross-node retry re-resolves machine values while ns stays pinned.

**A3 — accepted slot / 30s assignment / historical exact reap / no phase inference**
- `GrantLease(...,slot,...)` rejects negative slot and binds exactly that node+slot via CAS (`task.go:742-757,947-953`; `node.go:901-933`). Tests: `task_attempt_test.go:200,281`.
- Historical reap checks node token hash + task/attempt/node/lease/slot (+optional instance) and releases only that binding; idempotent: `task_attempt.go:469-540`, `node.go:945-973`; tests `task_attempt_test.go:395`, `task_test.go:760`.
- Old result/token remain rejected by current-attempt+lease+expiry checks (`task.go:1111-1190,1601-1633`); test `task_test.go:1117`.
- `ReclaimStaleSlots` ignores nil/busy=true, requires Busy=false + exact refs, never reads task phase (`node.go:799-879`); `MarkNodeOffline` keeps busy ledger (`node.go:757-793`); `RecoverLostLeases` never frees physical slots (`task.go:1438-1595`). Tests: `task_attempt_test.go:499,572,616`, `task_test.go:639,683`.
- Budget shrink: grant counts all busy slots and rejects while occupied (`task.go:896-945`); test `task_attempt_test.go:616`.

## Findings

### F1 · major · validated contract gap — GrantLease does not recheck Node/namespace authorization
`GrantLease` reads only phase/assignment/attempt/capability (`task.go:775-783`), checks node **status** only (`task.go:830-845`), and never loads `node_configs.namespaces` or `nodes.declared_namespaces`. The ns check exists only in eligibility (`node.go:1299-1306`; `config.go:1174`). Parent A3 §1/§5 explicitly requires grant txn to recheck “Node/ns权限”. Consequence: revoke the node’s namespace (PutNodeConfig or hello-declared namespaces) after assign, apply config, then grant — `appliedMatches` passes and the task still runs in a now-unauthorized namespace. A G2 pre-check outside this transaction is TOCTOU and not a substitute. No v4test covers revoke-between-assign-and-grant.

### F2 · major · validated logic bug — stale grant clears the *current valid* assignment
Branch `task.go:846-864`: if `rowAssign != assignmentID`, it still executes `UPDATE ... assignment_id = rowAssign`, i.e. clears the current assignment, then returns `ErrNoAssignment`. Reproduction: assign `asg-new`, call `GrantLease(..., "asg-old",...)` → pending task loses `asg-new`; subsequent valid `asg-new` grant fails. Existing `TestTaskGrantRejectsStaleAssignment` (`task_test.go:69-89`) sets up exactly this but only asserts pending/busy, never `AssignmentID` or a follow-up valid grant, so it masks the bug.

### F3 · major · provisional — expired assignment identity can be resurrected with a new 30s window
`AssignTaskAt` is idempotent only while unexpired (`task.go:653-667`); otherwise it rewrites `assignment_expires_at = now+30` for the **same** assignment ID (`task.go:672-681`). The node-side reservation was keyed to the original `assignmentExpiresAt` and may already be released. A later grant then passes central expiry (`task.go:848`) although the shared 30s deadline is no longer shared. Existing test only re-dispatches with a new `asg-2` (`task_attempt_test.go:281-342`). Bug if G2/scheduler reuses an ID for re-dispatch; store does not reject it.

### F4 · major · provisional — UpsertInstances can delete live busy-slot ledger
Empty `specs` deletes **all** rows (`node.go:639-643`); shrink deletes `slot NOT IN (...)` (`node.go:650-656`) with no `busy_task IS NULL` guard. ON CONFLICT preserves busy only for retained slots (`node.go:664-673`). Thus an empty/omitted hello instances list, or slot shrink while an old process still runs, erases occupancy; the attempt/slot binding is lost and the slot can be rebound. This contradicts A3 “empty/absent instances is not no-process” and “shrink keeps stock until reaped”. Tests shrink only an unleased slot (`cluster_node_test.go:470-483`). Store behavior is validated; severity depends on G2 mapping, so provisional.

### F5 · major · provisional — presence of `slot` is not type-enforced at store boundary
A3 wire uses present `*int`; store APIs use plain `int`: `GrantLease(slot int)` only rejects `<0` (`task.go:742-750`), `ReapedInputs.Slot int` (`task_attempt.go:454-466`), `SlotObservation.Slot int` (`node.go:157-175`). A G2 mapping that decodes an absent wire slot to zero produces a valid-looking slot 0 and can grant/reap/clear slot 0. Tests simulate missing with `-1` (`task_attempt_test.go:343,431`), proving the store relies on caller conversion. Provisional until T3/G2 mapping lands, but it is a sharp seam.

### F6 · minor · validated race — RejectAssignment can clear a newer assignment
Read (`task.go:1053-1066`) and update (`task.go:1070-1075`) are separate statements; update guards only `id+phase`, not `assignment_id`. A stale reject interleaving after a new assign clears the newer reservation. Also `assignmentID == ""` skips the mismatch check (`task.go:1062`) and clears the current assignment. Impact is liveness/churn, not unauthorized execution.

### F7 · minor · validated — AckCancel accepts attempt<=0 no-op on pending
Pending branch (`task.go:1379-1392`) calls `reapAttemptForTaskTx`; a missing attempt row is swallowed (`task.go:1646-1660`) and returns success. Malformed `cancel.ack` attempt=0 on pending is accepted as no-op. Fail-open validation gap.

### F8 · minor · provisional — pinned lifecycle can be overwritten at grant
`GrantLease` reads `max_attempts` from the task row, then overwrites it and `idempotent` from `lifecycleOn` (`task.go:791-800,971-988`). `Task`/schema say these are pinned at dispatch. If the capability version row is deleted between assign/grant, `lifecycleOn` returns defaults (`task.go:1717-1736`), changing retry semantics/budget. Normally capability-version immutability masks this.

### F9 · minor/provisional — remaining edge holes
- `recoverOne` aborts the sweep if a task attempt row is missing (`task.go:1535-1540,1573`), wedging an expired task; v4 grant always inserts, so only corrupt/legacy data.
- `DeleteExpiredTasks` only checks `reaped_at IS NULL` (`retention.go:151-164`); no artifact/manifest/receipt reference check. Known deferred A1/G1-6C3 territory, reported only as a gap against task-card §9, not a new S regression.
- `DeleteNode` cascades `node_instances` without a busy guard (`node.go:601-611`); admin delete can drop occupancy ledger while tasks remain.
- `ReclaimStaleSlots` swallowing `ErrNotFound` then releasing (`node.go:866-874`) appears intentional orphan-slot healing, not a finding.
- Prior delivery-window residual (`retention.go:205`, `expires_at>0` only) remains unchanged; outside S focus.

## Bottom line
The three previous S majors are materially addressed in store logic (snapshot no-drift; historical exact reap; no phase-based reclaim). I do **not** accept the checkpoint as correct yet because of F1/F2 (validated contract/logic gaps) and F3–F5 (provisional but high-impact A3/D2 seams). The `161 PASS` count is not evidence for these uncovered paths.

```json
{"ok": false, "reason": "G1-6S @7ab4ccf: D2 snapshot/ns pin and historical reap/reclaim mostly hold; validated gaps: GrantLease omits mandatory Node/ns authorization recheck (task.go:742/775-845) and stale grant clears current valid assignment (task.go:848; test task_test.go:69 misses assertion); provisional: expired assignment resurrection (task.go:653-681), UpsertInstances deletes busy ledger on empty/shrink (node.go:639-656), store slot presence not type-enforced"}
```
