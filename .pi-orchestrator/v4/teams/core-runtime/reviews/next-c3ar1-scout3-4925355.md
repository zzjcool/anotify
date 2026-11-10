## Implementation gate: PASS

Reviewed cumulative `133fd03` + `4925355` at **HEAD `49253553ff5eec5f90e03cb3393a791284b408c0`**. Working tree is clean. No confirmed major/critical correctness or regression findings within the requested scope.

### Facts and key call chains

1. **Current credentials are checked inside each execution transaction.** Grant, renew, complete, and ACK call `authorizeNodeOn`, which compares against the current persisted node token hash. Grant/renew/complete also recheck the namespace intersection.  
   Evidence: `internal/store/execution_auth.go:50–70`, `:296`, `:375`, `:578`, `:624`, `:677`, `:730`, `:809`.

2. **Stale grants cannot clear the current assignment.** Assignment/node/proposed identity checks precede the expiry-clearing mutation. A matching expired candidate is rejected without granting execution.  
   Evidence: `internal/store/execution_auth.go:353–373`.

3. **All unreaped attempts remain budget occupants, regardless of task phase or lease expiry.** Reservation counting excludes only the requested node/task/assignment/proposed/slot tuple—not every reservation belonging to that task. Total, cross-namespace capability, and namespace/capability pinned limits use these counts.  
   Evidence: `internal/store/execution_auth.go:169–241`.

4. **The reservation ledger carries internal assignment identity and bounded expiry.** Recording requires a matching live pending assignment. Idle release and grant binding require matching reservation identity; an old assignment with the same proposed number cannot clear or consume the newer reservation.  
   Evidence: `internal/store/schema.sql:233–245`; `internal/store/node.go:824–830`, `:960–976`, `:1037–1044`.

5. **Expiry replacement cannot reap an actual execution.** Its predicate requires an empty lease and a positive recorded expiry already reached. Grant binding clears reservation assignment/expiry fields while installing the execution lease. No attempt-reaping mutation occurs in reservation replacement.  
   Evidence: `internal/store/node.go:847–856`, `:1038–1044`. Regression coverage preserves the execution binding and unreaped attempt after lease recovery: `internal/store/v4test/execution_auth_test.go:747–817`.

6. **Grant snapshots remain pinned and transactional.**  
   `GrantLeaseAuthorized(auth, taskID, assignmentID, proposedAttempt, slot, now)` → budget check → exact slot binding → attempt snapshot insertion → task execution update. The snapshot includes machine values, sources, limits, revision/hash, and the task’s pinned namespace revision.  
   Evidence: `internal/store/execution_auth.go:445`, `:482`, `:505–544`; snapshot regressions: `internal/store/v4test/execution_auth_test.go:1285–1381`.

7. **Historical ACK preserves failure facts and is repeat-idempotent.** After current-node authentication and historical binding validation, ACK calls `reapAttemptOn`; it preserves phase/error and existing finish time, writes the supplied first-reap time, and releases only that attempt’s binding. Already-reaped attempts return without rewriting.  
   Evidence: `internal/store/execution_auth.go:809–840`, `:898–905`; `internal/store/task_attempt.go:399–429`; current-attempt guard: `internal/store/task.go:1155–1164`.

8. **The repaired concurrency test discriminates budget enforcement rather than slot range.** It uses total=2, per-capability=1, legal slots 0/1, and asserts exactly one grant plus unchanged losing-slot occupancy. ACK regressions check whole-attempt equality across repeated ACKs and successful budget reuse after historical reap.  
   Evidence: `internal/store/v4test/execution_auth_test.go:498–579`, `:1092–1114`, `:1120–1200`, `:1204–1280`.

### Risks, boundaries, and verification

- Lease expiry/requeue remains distinct from physical reclamation; no new expiry-based release of actual executions was found.
- Supplied exact-worktree four-check acceptance was retained, **not rerun**. This is **not full T1 acceptance**.
- C3b typed references, original whitebox work, G2–4, and the pending result/wire issue remain outside this gate and are not findings.
- Review changes: **none**. Tests added by this review: **none**.
- Read-only verification: `git rev-parse HEAD`, `git status --short`, and scoped commit/diff inspection → exit 0; fixed HEAD confirmed, clean tree.
- No additional test, lint, or build commands were run.

{"ok":true,"findings":[],"reason":"Implementation gate passes for cumulative 133fd03 + 4925355 at fixed HEAD 49253553ff5eec5f90e03cb3393a791284b408c0; no confirmed major or critical findings in scope. Supplied four-check acceptance retained; not full T1 acceptance."}
