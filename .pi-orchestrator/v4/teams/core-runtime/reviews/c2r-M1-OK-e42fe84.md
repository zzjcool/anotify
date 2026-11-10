## Verdict: previous M1 closed

Scoped static review of `e42fe84e705094a14d84188ea935425c87815ac7` found **no actual correctness or simplicity blocker**. No broader C2/G1/schema/C3 review was performed.

### Facts and evidence

1. **Ordinary creation is transactional.** `CreateCapability` opens a transaction, passes `txExec{tx: tx}`, rolls back on early return, and commits only after successful creation.
   `internal/store/capability.go:150–163`

2. **An absent positive explicit version reaches insertion**, rather than an update to the highest version.
   `internal/store/capability.go:180–195`

3. **The changed projection block executes after successful row insertion.** It queries latest through the same `ex`, rebuilds edges only when `latest.ID == c.ID`, and otherwise returns success without rebuilding. Thus an ordinary lower-version insertion persists its row while leaving the highest version’s projection untouched.
   `internal/store/capability.go:256–272`

4. **“Latest” implements the frozen ownership rule:** same namespace/name, `deleted_at IS NULL`, ordered by `version DESC LIMIT 1`. This is highest **live version**, not most recently inserted row.
   `internal/store/ref.go:94–100`

5. **The new test exercises ordinary high-3 → low-1 creation.** It creates version 3 with `Next=["keep"]`, then version 1 with `Next=["low"]`; it checks that version 1 reads back equal to the created value and that two rows exist. Its helper calls `db.CreateCapability` directly.
   `internal/store/v4test/resource_cas_test.go:110–127`
   `internal/store/v4test/helpers_test.go:51–56`

6. **The test separately checks both required invariants:** the entire latest record remains deeply equal to its pre-insertion snapshot—including `Spec.Next` and resource version—and persisted edges remain equal to that snapshot’s `Next`. The edge helper reads `db.EdgesFrom`, not the capability spec.
   `internal/store/v4test/resource_cas_test.go:128–133`
   `internal/store/v4test/edges_test.go:16–26`

### Key flow

`CreateCapability(ctx, *CapabilityVersion)` → `createCapabilityTx(ctx, txExec, c)` → explicit-version lookup → `insertCapabilityOn(ctx, same txExec, c, normalized artifact)` → successful INSERT → highest-live lookup → ownership guard → commit.

For version 1 beneath live version 3, the guard skips edge rebuilding; this path contains no update to version 3’s spec or revision.

### Risks, simplicity, and verification limits

- Latest-lookup errors propagate to the transaction’s rollback path; projection ownership is not silently bypassed.
- The fix adds one transaction-local lookup and one ownership branch, without a separate transaction or new abstraction.
- The regression distinguishes the old unconditional projection replacement: `["low"]` cannot satisfy the final `["keep"]` edge assertion.
- Tests/builds/lint were **not run**, as prohibited. Reported red/green and original four-stage exit-zero results were **not treated as proof**.
- The supplied `52d` reference did not resolve. Historical-reference identity remains unverified; the target’s highest-live selector was verified directly, so this does not block the M1 decision.

### Delivery

**DONE C2-R M1 — read-only recheck**

- Files modified: none.
- Inspected changed files: `internal/store/capability.go`, `internal/store/v4test/resource_cas_test.go`.
- Added regression inspected: `TestResourceCASCapabilityLowerVersionCreateKeepsLatestProjection`.
- Verification: scoped `git show` inspection only.

{"ok":true,"reason":"Previous C2-R M1 closed by scoped static review; 6 facts, no actual blocker."}
