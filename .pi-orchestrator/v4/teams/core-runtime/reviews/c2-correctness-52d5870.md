# STRICT C2 Gate — `52d58703beac70eecb4dc338f84969cf275c87b1`

**Verdict: PASS within the frozen C2 scope. No confirmed Major/Critical blocker found.** This is a static correctness review, not approval of broader G1 integration or pending C3 work.

Production and test evidence below was read from the fixed commit using `git show`; no tests/builds, writes, services, or child agents were run.

## 1. Facts

All paths below are under `internal/store/`.

1. **Resource tokens bind UID and revision.** `ResourceVersion` produces `rv1:<uid>:<revision>`. Parsing rejects missing/malformed tokens, nonpositive revisions, noncanonical counters, and overflow. Comparison checks both components; conflicts carry the complete current token.
   Evidence: `resource_version.go:10–58`.

2. **Public name-addressed mutations accept string tokens.** Capability, Source, Node, Namespace, and Playbook mutation entrypoints resolve and compare identity/revision inside their mutation transaction. Internal immutable-ID entrypoints remain distinguishable revision-CAS interfaces.
   Evidence: `resource_cas.go:32–64,161–198,262–314,413–486`; `playbook.go:234–280,416–452`; `resource_version.go:61–69`.

3. **Source display names are persisted and scanned independently of configuration.** The new column is included in the shared column list, INSERT arguments, and scanner.
   Evidence: `schema.sql:550–564`; `source.go:61–62,80–95,208–213`.

4. **Source name-path updates distinguish omission from clearing.** A nil `DisplayName` preserves the current value; a pointer to `""` clears it. Omitted configuration preserves the exact stored bytes, while omitted targets/enabled preserve their current values.
   Evidence: `resource_cas.go:153–159,181–192`; `source.go:479–514`.

5. **Capability uniqueness is live-only.** Both exact `(namespace_id,name,version)` and `(namespace_id,digest)` unique indexes use `WHERE deleted_at IS NULL`; tombstones retain their UID primary key.
   Evidence: `schema.sql:246–265`.

6. **Ordinary capability reads hide tombstones.** Latest, explicit version, ordinary UID lookup, lists, digest lookup, fold lookup, and next-version allocation apply the live filter. Playbook installation’s fold lookup and kept-resource listing do likewise.
   Evidence: `ref.go:94–100`; `capability.go:396–422,481–502,545–551`; `playbook_tx.go:143–148,446–454`.

7. **Capability deletion is one transaction covering every visible version.** It checks the current latest UID/revision, conditionally tombstones that row, tombstones remaining live versions, and removes only the capability’s own outgoing projection. There is no incoming-reference rewrite in this operation.
   Evidence: `resource_cas.go:111–150`.

8. **Pinned historical reads remain separate from live resolution.** The private UID reader intentionally omits the live filter; task eligibility uses that reader. Deployment delivery reads through retained deployment-to-capability UID references. Ordinary deployment detail/list reads hide deleted capabilities, and new deployments resolve a live capability inside their write transaction.
   Evidence: `capability.go:535–541`; `node.go:1128–1142`; `deployment.go:213–228,283–291,350–368,561–570`.

9. **Creation reuse cannot bypass mutable-resource CAS.** Same-content creation returns the existing row without changing its display name, routing, or revision. Mutable exceptions require the explicit update path.
   Evidence: `capability.go:290–307`; `v4test/resource_cas_test.go:92–107`.

10. **Node root/config use the same revision fact.** Root reads project `node_configs.resource_version`, defaulting to 1. Root mutation and config mutation share `putNodeConfigOn`; a tags-only root mutation advances the CAS counter without advancing configuration-application revision. Node deletion compares that same fact. Namespace root/config similarly share the namespace row’s counter.
    Evidence: `node.go:211–215`; `resource_cas.go:276–313,349–355`; `config.go:755–790,475–476,543–564`.

11. **Schema validation rejects incompatible capability layouts without repair.** It requires exact live predicates and matching index-column facts, and rejects nonprimary full-table unique constraints covering tombstones. The approved C1 identifier/grouping grammar remains the relevant acceptance boundary.
    Evidence: `migrate.go:426–478,540–545,650–659,706–768`; `v4test/format_test.go:1068–1162`.

12. **Regression evidence is present, not merely a test-count claim.** New assertions cover Source reopen/omit/clear behavior, projection rollback, shared root/config races, ABA rejection with full current tokens, deletion visibility, retained incoming intent, and pinned old-spec reads after recreation.
    Evidence: `v4test/resource_cas_test.go:110–151,176–283,286–319,352–388,412–526,547–583,633–675`.

## 2. Key data flows / public signatures

- **Capability:**
  `UpdateCapability(ctx, namespaceID, name, version, CapabilityPatch, expectedVersion string)`
  → begin transaction → live name/version resolution → UID+revision comparison → guarded row update → latest-only projection rebuild when routing changes → routing event → commit.
  `CapabilityPatch = {DisplayName *string, Next *[]string}`.
  Evidence: `resource_cas.go:25–108`.

- **Capability deletion:**
  `DeleteCapability(ctx, namespaceID, name, expectedVersion string)`
  → latest live token check → all-visible-version tombstoning → own outgoing-edge deletion → commit.
  Evidence: `resource_cas.go:112–150`.

- **Source:**
  `UpdateSourceByName(ctx, namespaceID, name, SourcePatch, expectedVersion string)`
  → transaction-local lookup/check → omission-aware merge → conditional SQL update → projection replacement → commit.
  `SourcePatch = {DisplayName *string, Config []byte, Targets *[]Target, Enabled *bool}`.
  Evidence: `resource_cas.go:153–198`; `source.go:464–535`.

- **Node:**
  `UpdateNode(ctx, name, NodePatch, expectedVersion string)` and
  `PutNodeConfigByName(ctx, name, *NodeConfig, expectedVersion string)`
  → transaction-local root lookup/check → shared configuration fact update → optional tags write → commit.
  Evidence: `resource_cas.go:230–236,262–346`.

- **Namespace/Playbook:** Name-addressed updates/deletes take `expectedVersion string`; Namespace config writes join the root counter, while Playbook replacement/deletion predicates include namespace, slug, UID, and revision.
  Evidence: `resource_cas.go:413–486`; `playbook.go:234–291,416–459`.

## 3. Risks, protections, and boundaries

- **Confirmed protections:** guarded capability/Source mutations, transactional projection rollback, live-only uniqueness, retained incoming declarations, explicit private pinned reads, and full-token ABA conflicts. The recreation test explicitly asserts **same digest, different UID** and rejects the old token against the replacement (`v4test/resource_cas_test.go:241–253`).
- **Transactional dependency:** Node config uses a checked transaction-local UPSERT rather than an independently guarded revision predicate. Namespace deletion also relies on its enclosing checked transaction. These are not the prohibited external `GetUID` followed by an unguarded write. The code documents immediate transaction locking (`node.go:1183–1185`); connection/DSN implementation was not independently audited beyond this scope.
- **C3 remains explicitly pending:** physical FK/ref-safe GC completeness is **not** certified here, and its absence is **not** classified as a new C2 Major. The new tests exercise retained bodies around existing retention operations; that does not establish complete GC safety.
- No task-cancellation, executor, legacy-compatibility, or broader G1 conclusion is made.

## 4. Pending questions / speculation

- Installation constructs a `Source` without assigning `DisplayName` (`playbook_tx.go:390–402`). Whether planned installation inputs can carry that metadata—and whether their propagation belongs to this frozen C2 contract—was not established. **This is not a confirmed Major finding.**
- Runtime race behavior, full four-stage results, and downstream HTTP token serialization were not independently executed or certified.

## Delivery / verification

**DONE STRICT-C2**

- Files modified: **none**.
- Tests added by this reviewer: **none**.
- Fixed-commit static test declarations: **216**, versus **195** in its parent.
- Diff inspection found **21 added test functions and no removed test functions**: 18 resource-CAS tests and 3 format tests.
- Read-only `git show`/filtered declaration-count commands completed successfully.
- Tests/build/lint: **not run**; no files changed. Claimed green stages were not treated as review proof.

{"ok": true, "reason": "C2 static recon complete, 12 facts; no confirmed major or critical blocker; C3 remains explicitly pending"}
