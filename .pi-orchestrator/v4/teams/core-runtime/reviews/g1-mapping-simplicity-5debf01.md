## Final G1 gate — **NOT APPROVED**

HEAD remained `5debf01fcd06d6e8171e478143c60c253b7bcf3b`. Canonical cwd/gitroot verified. Read-only review; no compile, tests, services, or file changes.

Below, **M** denotes `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md`.

### 1. Inventory facts

- **No omitted original names.** Original inventory at `M:129–873` contains **460 unique file/name pairs**, matching both `0d4b975` and `b5adeda` and the original inventory at `5d44685`.
- Outcomes: **219 kept, 109 migrated, 127 retired, 3 narrow-retired, 2 restored**. Outcome-only generic pending: **0**.
- All **78 deleted-file names** now have per-name dispositions. The previous “78 generic pending” finding is resolved **mechanically**, not necessarily semantically.
- Restoration is intact: `b99db8a → 5debf01f` changes exactly five existing locations plus the appended source-name report; no inventory deletion. Evidence: `M:582,1022,1042,1054,1076,1100`.
- Exact unmapped original names: **`[]`**. Neither the “486 rows” claim nor package-green output establishes assertion equivalence.

### 2. Remaining findings

| Severity | Evidence | Finding |
|---|---|---|
| **Major — assertion mapping** | `M:209,219`; `internal/store/v4test/edges_test.go:131–149`; `internal/store/capability_test.go:276–307` | `TestCreateCapabilitiesBatchRollsBackOnBadLink` maps to **ResolveRef reads**, not a failing capability write/rollback. `TestCreateCapabilityWithLinksRollsBackOnBadLink` maps to **invalid script/params validation**, not bad-link rollback. The claimed invariants are not asserted by those targets. |
| **Major — concurrency mapping** | `M:223`; `internal/store/capability_spec_test.go:177–210` | `TestCreateCapabilityWithLinksConcurrent` maps to three **sequential** CreateCapability calls. Actual concurrent-create assertions exist separately at `internal/store/v4test/resource_cas_test.go:673–690`, but this original row does not cite them. |
| **Major — mutation-path mapping** | `M:713`; `internal/store/v4test/edges_test.go:72–115`; `internal/store/source_test.go:144–214` | `TestUpdateSourceRebuildsEntryEdges` cites direct projection replacement and CRUD without a source update. Neither proves UpdateSource rebuilds edges. The real update/round-trip assertions exist at `internal/store/v4test/source_test.go:235–253`, under `TestSourceTargetsRebuildEdges`. |
| **Minor — anchor drift** | `M:570–596`; `internal/store/playbook_test.go:171,204,266,283,322,387,433,469,505,549,585,666` | **24 original-row references remain one line early**, affecting 12 distinct playbook tests. The earlier node/txn/deployment/source/namespace anchor problems are resolved; current playbook anchors are not. |
| **Minor — previous outcome branch remains unconfirmed** | `M:189,1078–1081`; `internal/store/task_test.go:885–908` | The new failure-projection test submits nonempty Reason and Summary and proves **Reason fallback**. It does not exercise the previously reported “failure with no reason → `failed`” branch. Repository-wide assertion loss is **not established**, but this exact prior branch is not reconciled by the named mapping. |

These are **mapping/coverage-proof failures**, not evidence that all corresponding production invariants are broken.

### 3. Verified coverage and simplicity

- **Invalid-result auth minor resolved:** `TestCompleteRejectsInvalidResultPhaseWithZeroWrites` uses valid authorization/attempt/lease, checks the phase-specific error, whole task/attempt equality, attempt count and unchanged busy slots (`internal/store/task_test.go:818–867`).
- **Oversized batch resolved:** mixed good/bad input leaves zero task rows; exact limit round-trips; typed error details asserted (`internal/store/task_test.go:755–812`).
- **Lease/txn safety remains real:** budget refusal and zero busy slots (`internal/store/v4test/task_test.go:1030–1055`); foreign-slot preservation and old-attempt refusal (`internal/store/tx_merge_test.go:66–164`).
- **Source-name chain is discriminating:** Render → Plan/create/reuse → stored Render/dryRun/install; final writer defense has prior capability writes followed by rollback. Evidence: `internal/playbook/render_test.go:737`, `internal/playbook/plan_test.go:574`, `internal/store/v4test/source_name_test.go:70–127`, `internal/store/playbook_test.go:801–861`.
- CAP’s four formerly reserved names are accepted (`internal/store/playbook_test.go:767–798`). Explicit same-Spec v2 uses a fresh object/new UID, retains two rows, equal FoldKey and different Digest (`internal/store/capability_test.go:430–479`). Empty targets and persisted `no_target` remain asserted (`internal/store/source_test.go:224–247`).
- Actual baseline DDL/ledger/marker transaction rollback exists, distinct from preOpen refusal (`internal/store/v4_whitebox_test.go:119–198`).
- No unnecessary new duplicate coverage demonstrated. Several rejected mappings have **existing stronger tests elsewhere**; the layered source-name tests exercise distinct boundaries.
- Historical checkpoint prose remains stale/unqualified at `M:8,23,27`; unlike the former appendix, it does not create a second pending inventory.

### 4. Limits / delivery

- **Confirmed repository-wide true assertion losses:** none established within this bounded review.
- **Not approved:** the four exact original names above still point to nondiscriminating assertion paths.
- Changed files: **none**. New tests: **none**.
- Validation: original-name reconciliation, complete chunked mapping read, declaration-anchor checks, targeted assertion reads and `git diff --check` → completed. Compilation/lint/test execution intentionally not performed.

{"ok": false, "reason": "Original 460 and deleted 78 inventory reconciliation is complete with no omitted names, but four protected original-case mappings cite nondiscriminating tests; 24 playbook anchors drift and the prior no-reason failure fallback branch remains unconfirmed."}
