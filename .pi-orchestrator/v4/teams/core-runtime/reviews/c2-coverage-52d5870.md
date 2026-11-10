# STRICT C2 coverage gate — FAIL

**Fixed revision:** `52d58703beac70eecb4dc338f84969cf275c87b1`.
Review used fixed-commit `git show` content and necessary CAS references. No tests, builds, writes, other worktrees, or nested agents.

## Confirmed major coverage hole

### C2-1 — Node “ABA” assertions do not discriminate UID checking from revision-only checking

**Evidence:**

- `internal/store/v4test/resource_cas_test.go:416` initializes node configuration. Its helper performs one configuration write (`internal/store/v4test/config_test.go:38–50`), advancing the default resource revision from **1 to 2** (`internal/store/config.go:624–630,769–790`).
- The root/config race requires another increment, making the surviving revision **3** (`internal/store/v4test/resource_cas_test.go:431–440`).
- The deletion token consequently contains the old UID and revision **3** (`internal/store/v4test/resource_cas_test.go:446–447`).
- The same-name replacement starts at revision **1** (`internal/store/v4test/resource_cas_test.go:450`; `internal/store/node.go:297–299`).
- Both purported ABA checks reuse that revision-3 token against the revision-1 replacement (`internal/store/v4test/resource_cas_test.go:454–458`).

**Assertion loophole:** A Node-specific regression that checks only the token’s revision—and ignores its UID—would still reject both operations because **3 ≠ 1**. It could also return the complete replacement token and satisfy `requireConflictToken` (`internal/store/v4test/resource_cas_test.go:168–173`). These assertions therefore do not establish Node UID anti-ABA.

`PutNodeConfigByName` is exercised in the race at line 431, but not against the recreated node. Thus the configuration name path has no replacement-UID challenge either.

**Minimum coverage correction:** Exercise a genuinely captured old token whose revision equals the replacement’s revision but whose UID differs. Require complete-current-token conflicts for `UpdateNode`, `PutNodeConfigByName`, and `DeleteNodeByName`; reread the replacement root/config after each rejection and assert unchanged fields and revisions.

This is a **confirmed major coverage deficiency**, not a confirmed production defect. Production currently checks both identity components (`internal/store/resource_version.go:46–54`; Node call sites at `internal/store/resource_cas.go:276,334,372`).

## Facts established by the scoped assertions

- **Capability deletion, visibility and incoming intent:** tests delete with a task and deployment present, reject deleted public references/ID reads, remove outgoing projection, and preserve incoming capability/source declarations.
  Evidence: `internal/store/v4test/resource_cas_test.go:188–225`.

- **Pins, recreation, shared digest and retention safety:** assertions preserve private deployment identity, pending task identity/state, retained spec bytes, the replacement’s live resolution and outgoing edges, and both shared-digest owners.
  Evidence: `internal/store/v4test/resource_cas_test.go:236–282`.

- **Discriminating pinned-body read:** old and replacement capabilities have different execution requirements/digests; eligibility must continue using the old task’s requirement.
  Evidence: `internal/store/v4test/resource_cas_test.go:547–582`.

- **Source display semantics:** reopen/JSON roundtrip preserves display and config; display-only patches preserve identity, kind, credentials, targets and enabled state; omission retains display; explicit empty clears display while retaining config. Cron scheduling facts also remain unchanged.
  Evidence: `internal/store/v4test/resource_cas_test.go:110–150,352–387,655–674`.

- **Concurrency and update rollback:** the race helper demands exactly one success and one conflict. Source/capability assertions compare persisted declarations with projection; injected edge failures require complete row/projection rollback.
  Evidence: `internal/store/v4test/resource_cas_test.go:17–41,54–66,293–318,378–387`.

- **Other UID challenges and token errors:** capability, Source and Playbook recreation checks require complete replacement conflict tokens. Malformed/missing capability tokens and absent-resource updates have explicit error assertions.
  Evidence: `internal/store/v4test/resource_cas_test.go:241–253,391–409,501–544`.

- **Layout rejection:** wrong live-index predicates, full uniqueness, extra-key uniqueness and missing tombstone/display columns must be rejected without database repair.
  Evidence: `internal/store/v4test/format_test.go:1037–1163`.

## Relevant call chain and safeguards

Node root/config name operations resolve the live node by name, check `expectedVersion` against `(node.ID, node.ResourceVersion)`, and update the shared configuration counter transactionally. `NodePatch` carries tags/namespaces/vars/limits; the configuration path accepts a `NodeConfig` plus the same opaque token.

Evidence: `internal/store/resource_cas.go:230–235,261–345`; shared counter logic at `internal/store/config.go:719–790`.

The existing root/config race also checks that hostname, token hash, local variables and applied hash survive, and that both competing spec changes did not commit (`internal/store/v4test/resource_cas_test.go:440–444`). Those protections do **not** close the equal-revision UID loophole.

## Quantification, limitations and delivery

Static function-name comparison of fixed and parent `internal/store/v4test/*_test.go` independently confirmed:

- **195 old names retained; zero removed**
- **216 total**
- **21 additions:** 18 resource-CAS tests and three format tests
  Addition locations: `internal/store/v4test/resource_cas_test.go:44–676`; `internal/store/v4test/format_test.go:1037,1068,1118`.

The reported four-stage green result remains **unverified execution evidence**. Physical GC/FK behavior, typed manifests and auth-entry completion remain outside this C2 decision; no whole-G1 audit was performed.

**DONE C2-scoped-review — gate rejected**
Modified files: none. Newly authored tests: none. Validation: fixed/parent `git show` reads and static assertion/name analysis succeeded; tests/build/lint were not run, as requested.

{"ok":false,"reason":"Confirmed major C2 coverage hole: Node recreation tests compare old revision 3 against replacement revision 1, so revision-only checks pass; equal-revision UID anti-ABA coverage is required for Node root/config/delete name paths."}
