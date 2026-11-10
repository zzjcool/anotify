# STRICT S3 coverage review — FAIL

**Snapshot:** `bd69177b467cfe08458a632fc60300def6285e3b`
**Method:** `git show` only; four scoped test files, their parent versions, and `task.go` / `node.go` / `task_attempt.go`. No tests, builds, writes, log reads, or children.

## Confirmed major coverage gaps

### P1 — D2 does not discriminate acceptance-time namespace pinning from first-grant pinning

**Evidence:** `internal/store/v4test/task_attempt_test.go:80-82` sets namespace value `ns-A`, creates the task, then immediately grants it. The namespace changes only **after** that first grant (`:118-121`). Assertions at `:130-131` and `:180-181` establish persistence across execution/retry, but cannot distinguish **acceptance-time pinning** from **first-grant-only pinning**.

Production explicitly requires acceptance-time pinning: `internal/store/task.go:304-318`, persisted at `:330-338`.

**Discriminating counterexample:** pinning namespace inputs only on the first grant, then retaining them across retries, produces the same values checked by this scoped test. This is a static coverage counterexample, not an executed mutation.

**Minimum missing verification:** change namespace configuration between task creation and first grant; assert that the original accepted value and revision survive that grant and the subsequent retry. No concurrency hook is needed.

### P1 — D2 same-attempt immutability is not discriminated for machine limits

**Evidence:** `internal/store/v4test/task_attempt_test.go:73` uses `total=2`; the later configuration update also uses `total=2` (`:119`). Limits are checked before mutation (`:97-99`), while the post-mutation historical snapshot check verifies only `ConfigHash` (`:133-135`).

Production treats limits as part of the frozen attempt snapshot: `internal/store/task.go:978-983`; snapshot persistence includes `MachineLimits` at `internal/store/task_attempt.go:328-341`.

**Discriminating counterexample:** returning current/live limits for an old attempt while retaining its stored variables/hash would not be rejected by these immutability assertions: the limits never change and are not rechecked afterward.

**Minimum missing verification:** mutate a limits value during the running attempt; verify the old attempt retains its original limits and the next attempt receives the updated limits.

These are **coverage gaps**, not confirmed defects in the fixed production implementation.

## Facts: coverage that does discriminate

- **Real grant authorization:** revoked authorized/declared namespaces, offline/wrong/missing nodes, and exhausted-budget ordering are exercised through `GrantLease`, with empty authorization outputs, unchanged task facts, and no attempt creation: `internal/store/v4test/task_test.go:1272-1319`.
- **Grant/reject identity:** stale and empty identities require `ErrNoAssignment`, preserve the complete task, and leave the replacement assignment grantable: `internal/store/v4test/task_test.go:1325-1374`. These assertions are stronger than rejection alone.
- **Expiry boundaries:** assignment refresh and renewal test both equality and one second beyond expiry; renewal must return zero and leave authorization unchanged: `internal/store/v4test/task_test.go:1380-1433`.
- **No empty/shrink fake ledger clearing:** nil/empty declarations compare full instance rows; reintroduction checks instance/task/attempt/lease identity and unreaped history. Occupied-slot grant rejection is followed by exact reap and successful grant: `internal/store/v4test/cluster_node_test.go:744-813`.
- **All-history GC:** the fixture explicitly has an old unreaped attempt and a completed/reaped latest attempt; both retention entry points preserve it, and exact old reap subsequently permits deletion: `internal/store/v4test/retention_test.go:439-476`.
- **Historical reap authorization preservation:** rejected inputs preserve the old attempt; valid old reap preserves complete current task facts. Actual current-token verification, authorized replay, fresh emit, old emit rejection, and current completion are exercised: `internal/store/v4test/task_attempt_test.go:436-545`. This is not merely a busy-count or token-precheck oracle.
- **Decoded JSON:** exact public-key membership catches uppercase/tag-removal leaks, alongside secret/token substring checks and retained internal-input verification: `internal/store/v4test/task_attempt_test.go:831-864`.

## Key call chains

- `GrantLease(taskID, assignmentID, nodeID, slot, now)` → transactional identity/namespace checks → machine snapshot load → exact slot binding → attempt insertion → current-task authorization update → commit: `internal/store/task.go:761-1010`.
- `ReapTaskAttempt(ReapedInputs{task, attempt, node identity, lease, slot, optional instance})` → current node-token validation → historical binding validation → attempt reap → exact binding release: `internal/store/task_attempt.go:469-537`.
- `TaskInputs(taskID, names)` reads the selected attempt’s machine values and the logical task’s namespace snapshot: `internal/store/task.go:392-434`.

## Risks and pending verification

**Snapshot concurrency is not an additional confirmed major gap.** Machine revision/applied facts/values/limits are read in one SQL query (`internal/store/task_attempt.go:265-269`), and grant reads and persists them through the same transaction. No torn-snapshot production path was confirmed. The absence of a deterministic concurrent-config interleaving test remains a limitation—not grounds for demanding a test-hook framework.

The rejection race explicitly does not claim observation of both orders (`internal/store/v4test/task_test.go:1439-1476`); deterministic stale-identity checks provide separate coverage.

Scoped parent/fixed comparison retains all **51** existing top-level tests and adds **7**. The stated whole-suite **177** and red/green logs were not independently verified.

## Delivery

Review modifications: **none**. Seven added tests:

- `TestGrantRechecksNodeAuthorization`
- `TestStaleGrantAndRejectPreserveNewAssignment`
- `TestExpiredAssignmentCannotRefreshDeadline`
- `TestRenewLeaseCannotReviveExpiredAuthorization`
- `TestRejectAssignmentRacePreservesNewAssignment`
- `TestUpsertInstancesPreservesOccupiedHistory`
- `TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped`

Verification: scoped `git show` reads/comparisons completed; tests/build/lint/typecheck intentionally not run.

**DONE S3-review — strict gate not satisfied.**

{"ok": false, "reason": "STRICT S3 coverage gate failed: scoped D2 tests do not discriminate acceptance-time namespace pinning or same-attempt machine-limits immutability; no additional concurrency-related major defect confirmed."}
