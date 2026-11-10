# STRICT gate review — BLOCKED

**Fixed commit:** `bd69177b467cfe08458a632fc60300def6285e3b`. All evidence below is from that commit via `git show`, not the live worktree. Read G1-6S3 and relevant frozen D2/D4/A3 requirements. No tests, builds, writes, other-worktree access, or children.

**Conclusion:** The listed S3 repairs are substantially present, but **two confirmed Major store-invariant gaps remain**. These are remaining gaps, not newly introduced regressions.

## Blocking findings

### F1 — Major — Grant/renew cannot atomically reject a rotated Node credential

**Fixed locations:** `internal/store/task.go:744`, `internal/store/task.go:803–811`, `internal/store/task.go:1022–1034`.

**Evidence:** `GrantLease(ctx, taskID, assignmentID, nodeID, slot, now)` receives no authenticated/expected Node token hash. Its transaction reads Node name, status and declared namespaces, but **not `token_hash`**. Node UID/existence/status validation is not credential validation.

`RenewLease(ctx, taskID, attempt, leaseID, now)` receives neither Node identity nor its authenticated credential. Its UPDATE checks current attempt/lease, running, cancellation and expiry, but no current Node credential.

A concrete ordering remains possible:

1. An old session authenticates with hash H0.
2. Credential rotation commits H1. The in-scope setter changes `nodes.token_hash` without changing task leases (`internal/store/node.go:587–593`).
3. An already admitted request invokes grant or renew.
4. Both mutations can succeed despite H0 being revoked.

This violates immediate old-token/old-session invalidation (`docs/design/v4/anode.md:31`). A G2 precheck outside the mutation boundary leaves precisely this TOCTOU window.

**Minimal repair requirement:** Grant/renew acceptance must depend atomically on the caller’s authenticated Node credential still matching the current credential and the execution belonging to that Node. Mapping an unchecked Node UID into today’s signatures cannot establish that invariant.

**Attempt-token nuance:** Renew also lacks an `attemptToken` parameter, but frozen `lease.renew` supplies only taskId/attempt/leaseId (`docs/design/v4/capability.md:841`), and renew does match the current attempt/lease. Therefore, that omission alone is **not a separate confirmed defect**. The missing current **Node credential** predicate is sufficient to block.

**Boundary:** This confirms a missing store invariant; deployed G2 exploitability and any caller synchronization remain unreviewed—not assumed safe.

### F2 — Major — Grant enforces total capacity, not perCapability/maxPerNode

**Fixed locations:** `internal/store/task.go:856–861`, `internal/store/task.go:913–947`; supporting helper at `internal/store/task.go:1755–1770`.

**Evidence:** Grant derives only `typedLimits.Total`, counts all busy instance rows, and checks slot range/total occupancy. It never evaluates `PerCapability`, capability-name occupancy, or `spec.concurrency.maxPerNode`. The capability helper returns only lifecycle data, discarding concurrency.

Source-level counterexample: with applied `total=2`, two free slots, and one capability configured with `perCapability[name]=1` or `maxPerNode=1`, two valid assignments can both grant. The second sees `busyCount=1 < total=2`; neither narrower ceiling participates in its decision.

Frozen semantics explicitly retain all three ceilings and require grant rechecking, including name-based perCapability sharing across namespaces (`docs/design/v4/anode.md:35–36,93–102`; `docs/design/v4/decisions.md:166`). Candidate-side checks cannot replace the atomic grant budget predicate.

**Minimal repair requirement:** The grant acceptance boundary must enforce the applicable name/wildcard perCapability ceiling and pinned capability maxPerNode ceiling against relevant unreaped occupancy, while preserving existing busy/history facts and rejecting without consuming an attempt.

## Verified facts and regression protections

1. **Namespace revocation is now rechecked inside grant.** Current authorized∩declared namespaces are checked against the pinned task namespace before budget terminalization, snapshot or occupancy writes (`internal/store/task.go:833–863`). Tests include revoked authorization with matching applied configuration and spent attempts (`internal/store/v4test/task_test.go:1272–1319`).

2. **Stale/mismatched grant preserves newer assignment.** Identity mismatch returns before clearing anything; only an expired matching Node/assignment candidate is cleared (`internal/store/task.go:814–831`). Same expired assignment cannot refresh its deadline (`internal/store/task.go:662–682`).

3. **Reject is exact and transactional.** Empty identity fails closed; task read and assignment-guarded UPDATE share a transaction (`internal/store/task.go:1063–1114`). The new race assertion checks newer identity, proposed attempt and deadline (`internal/store/v4test/task_test.go:1442–1477`).

4. **Nil/empty/shrink preserve occupied history.** Empty input performs no instance update; shrink deletes only unoccupied omitted slots; re-report changes liveness/time, not busy references (`internal/store/node.go:640–663`). Offline marks instances unavailable without releasing them (`internal/store/node.go:775–777`). New coverage checks retained instance identity and exact-reap-only reuse (`internal/store/v4test/cluster_node_test.go:743–817`).

5. **Expired renewal cannot resurrect authorization for the supplied clock.** The mutation requires `lease_until > now` (`internal/store/task.go:1031–1034`). Equality and expiry+1 are asserted without task mutation (`internal/store/v4test/task_test.go:1414–1437`).

6. **Historical reap is separated from current execution.** It checks current Node hash plus historical task/attempt/node/lease/slot and optional instance, then updates only attempt history and its bound instance (`internal/store/task_attempt.go:469–539,415–427`; `internal/store/node.go:942–955`). Repeat exact reap returns before release. The strengthened test checks unchanged current task facts, old-result rejection, actual old/current `EmitAuthorized` calls and current-token validity—not merely prechecks (`internal/store/v4test/task_attempt_test.go:486–545`).

7. **Snapshot semantics remain pinned.** Grant loads, validates and persists the snapshot through its transaction (`internal/store/task.go:889–910,969–1008`). TaskInputs reads historical attempt vars before pinned namespace vars, without fallback to current Node configuration (`internal/store/task.go:409–434`). JSON assertions now inspect exact decoded keys (`internal/store/v4test/task_attempt_test.go:836–858`).

## Minor/interface observations and pending verification

- **Crop misleading comments:** assignment still says the slot was previously seized (`internal/store/task.go:649`); reject says Node ownership is unchanged although it clears `node_id/node_name` (`internal/store/task.go:1057,1101`). These are documentation issues, not blocking behavior.
- **Snapshot concurrency evidence remains limited:** the observed pinning test changes configuration sequentially (`internal/store/v4test/task_attempt_test.go:118–143`). The new concurrent test targets rejection, not snapshot ordering. No snapshot invariant failure was found.
- **Caller mapping pending:** authenticated-session→Node/hash mapping and wire slot-presence enforcement were not inspected. Store’s integer slot need not mirror wire pointers.
- **Retention coverage improved, not independently proven:** the new test distinguishes old-unreaped/current-reaped history, checking both retention entries before exact old reap (`internal/store/v4test/retention_test.go:427–475`). Retention implementation was outside this review scope.

## Review delivery

- Reviewer-modified files: **none**.
- Reviewer-added tests: **none**; seven newly named S3 tests and strengthened historical-reap/JSON assertions inspected.
- Verification: scoped `git show` metadata, diffs and fixed file contents → **exit 0**.
- Tests/build/lint/typecheck: **not run, explicitly prohibited**. Previous red/green results were not treated as correctness proof.

{"ok":false,"reason":"STRICT gate blocked: two confirmed Major store-invariant gaps—rotated Node credential TOCTOU in grant/renew and incomplete perCapability/maxPerNode grant budgets."}
