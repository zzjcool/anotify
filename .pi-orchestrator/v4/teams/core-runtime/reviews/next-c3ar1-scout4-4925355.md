## Scope and facts

Reviewed fixed HEAD `49253553ff5eec5f90e03cb3393a791284b408c0` in the requested WT; final `git status --short` and scoped diff were empty. **Coverage gate: FAIL — M3 remains incompletely discriminated, not a confirmed product defect.**

1. **M1’s budget-only discriminator is real.** Both slots 0/1 are legal with total=2; exactly-one success, `ErrSlotUnavailable`, loser attempt counter=0, and empty loser slot are asserted (`internal/store/v4test/execution_auth_test.go:502–576`). Removing only perCapability enforcement produced `success=2 rejected=0`, exit=1 (`/tmp/anotify-core-next-c3ar1-red-m1-concurrent.log:4–11`).

2. **M2 catches task-wide reservation exclusion.** Two same-task reservations on other slots remain counted and preserved after rejection (`internal/store/v4test/execution_auth_test.go:637–664`). The corresponding red log reports an unexpected successful grant, not a compilation failure (`/tmp/anotify-core-next-c3ar1-red-m2-count.log:4–11`).

3. **Assignment identity has meaningful counterexamples.** Old/new assignments share proposed=1; stale busy/idle cannot overwrite/clear newer occupancy, and a newer grant cannot consume the old reservation (`internal/store/v4test/execution_auth_test.go:681–740`). Removing ledger assignment checks causes stale idle to reclaim one slot, exit=1 (`/tmp/anotify-core-next-c3ar1-red-m2-assignment.log:4–11`). Binding implementation checks task/proposed/assignment (`internal/store/node.go:1041–1044`).

4. **The added budget dimensions are isolated.** Reservation perCapability rejection uses total/pinned=3 versus perCapability=2; pinned rejection uses total/perCapability=3 versus pinned=2 (`internal/store/v4test/execution_auth_test.go:836–846`). Local-over-node total has a real grant discriminator: node=3, local=2, pinned=3, requested slot=1, with two existing occupancies (`internal/store/v4test/execution_auth_test.go:884–920`).

5. **Cumulative coverage retains meaning:** rotation has rejected old/current successful credentials (`internal/store/v4test/execution_auth_test.go:98–145`); namespace revocation covers authorized and declared branches (`:152–188`); current lease expiry includes boundary refusal plus before-expiry success, and cancellation refuses renew/result without clearing occupancy (`:941–985`). D2 checks first-grant namespace value/revision pinning (`:1292–1316`) and old/new attempt limits (`:1335–1379`).

6. **Schema assertions include the new reservation identity/window columns**, exact column counts, and reopen stability (`internal/store/v4test/schema_baseline_test.go:150–189`, `:394–407`).

## Call chain and existing safeguards

`GrantLeaseAuthorized(auth, taskID, assignmentID, proposed, slot, now)` loads applied configuration, checks budget, binds the requested slot, then inserts the attempt snapshot (`internal/store/execution_auth.go:411–505`).

Historical `AckCancelAuthorized(auth, ReapedInputs, true)` calls `reapAttemptOn` rather than `finishAttemptOn` (`internal/store/execution_auth.go:899–904`). The latter core returns unchanged when already reaped; first reap uses `reaped_at IS NULL` and preserves existing finished time (`internal/store/task_attempt.go:411–427`). These are implementation safeguards, **not substitutes for missing assertions**.

## Findings

```json
{
  "ok": false,
  "findings": [
    {
      "path": "internal/store/v4test/execution_auth_test.go",
      "line": 1160,
      "severity": "major",
      "evidence": "M3 first historical ACK checks only non-null ReapedAt, not equality to the supplied ACK Now; the failure-facts case at line 1251 does the same. A wrong non-null initial timestamp subsequently preserved survives these assertions. Budget release is genuinely tested at lines 1149–1151 and 1194–1196, but exact first-reap time is not."
    },
    {
      "path": "internal/store/v4test/execution_auth_test.go",
      "line": 1175,
      "severity": "major",
      "evidence": "Replay DeepEqual covers only the old attempt. Lines 1178–1187 check aggregate busy count and selected task/new-attempt fields, without whole-task or new-slot-ledger before/after equality. The failure-facts replay at 1274–1279 does not reread the task; current-cancel replay at 1109 compares only task Error/FinishedAt. Replay-only changes to task Summary or the new slot busy_lease_id can escape the scoped assertions. The functional red-m3 log proves old error clearing/time rewriting is detected, not full task/new-occupancy immutability."
    }
  ],
  "reason": "M1/M2 discriminants and isolated budget paths are supported; two major M3 coverage requirements remain unclosed."
}
```

## Validation and limits

- Changed files: **none**. New tests: **none**.
- Ran only read/search and git-state commands. No mutations, test reruns, writes, commits, or agents.
- Existing four gate logs record correct WT/HEAD and exit=0; scoped store tests record 24.357s (`/tmp/anotify-core-next-c3ar1-check4-test.log:1–7`). Red/green evidence was reviewed, not reproduced.
- C3b, G1 whitebox, G2–4, old reviews/checkpoints/sessions were excluded—not missing-scope findings.
- `DONE NEXT-C3aR1-readonly-review` denotes this review only, **not T1 DONE or implementation acceptance**.

{"ok":false,"reason":"Coverage gate fails: major M3 gaps in exact first ReapedAt and replay task/new-slot immutability assertions."}
