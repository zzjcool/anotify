## C2-R M2 — discriminator closed

**Frozen commit:** `e42fe84e705094a14d84188ea935425c87815ac7`
**Verdict:** The new test closes the prior **old revision 3 / replacement revision 1** coverage deficiency. This is a source-only finding, not a runtime test PASS.

### Facts

1. **Genuine captured old token, equal revisions, different UIDs.**
   `internal/store/v4test/resource_cas_test.go:509–515` reads the original Node and captures `oldNodeToken` before deletion. Lines `519–533` recreate the same name directly and require:
   - different old/replacement UIDs;
   - old revision exactly `1`;
   - replacement root revision equal to the old revision;
   - replacement config revision equal to replacement root revision.
   The captured token is not rewritten or fabricated afterward.

2. **All three required entrypoints are covered independently.**
   `internal/store/v4test/resource_cas_test.go:495–508` defines subtests for:
   - `UpdateNode`: stale tags and `Vars`;
   - `PutNodeConfigByName`: namespaces and stale `Vars`;
   - `DeleteNodeByName`.
   Each subtest opens its own database and receives the same captured-old-token scenario.

3. **Every rejection requires the complete current token.**
   `internal/store/v4test/resource_cas_test.go:536` invokes `requireConflictToken` for every table case. Its definition at `:195–200` requires both `ErrResourceVersionConflict` and the typed conflict error, with `CurrentResourceVersion` exactly `ResourceVersion(replacementUID, replacementRevision)`—not merely any error or a raw counter.

4. **Replacement root/config fields and revisions remain unchanged after every rejection.**
   `internal/store/v4test/resource_cas_test.go:520` seeds recognizable root fields: tags, hostname, instance and concurrency limits. Lines `524–531` capture root/config snapshots; `537–543` reread both and require whole-object `reflect.DeepEqual`, including their resource versions.

5. **Production UID discrimination remains intact.**
   `internal/store/resource_version.go:46–58` rejects either UID **or** revision mismatch and returns the complete current token. Frozen and parent source SHA-256 values match:
   `6f8ae20465408882c8fb0a166c0bc2cb9844a8b12ec06c0663dfc344e36def2a`.

### Call chain

The subtest passes `(db, replacementName, capturedOldToken)` to its mutation closure (`resource_cas_test.go:493–504,536`). Each production entrypoint resolves the current Node by name, then calls `checkResourceVersion(expectedVersion, n.ID, n.ResourceVersion)` **before mutation**:

- `UpdateNode`: `internal/store/resource_cas.go:272–292`
- `PutNodeConfigByName`: `:330–339`
- `DeleteNodeByName`: `:368–375`

Because revisions match, the UID comparison is the discriminator this fixture isolates.

### Risks and historical pitfall

The older test performs a config write and a race before capturing its deletion token (`internal/store/v4test/resource_cas_test.go:443–473`). Config writes increment RV (`internal/store/config.go:769`), while fresh Nodes expose default RV `1` (`internal/store/node.go:212–215`). That unequal-revision scenario could reject without UID discrimination.

The new test explicitly avoids the helper-induced increment (`resource_cas_test.go:519`) and guards the fixture numerically (`:532–533`). No confirmed major deficiency remains in this scoped discriminator.

### Verification limits and delivery

- **Changed files / newly authored tests:** none.
- **Frozen new test:** `TestResourceCASNodeEqualRevisionRecreateABA`, with the three named subtests.
- **Commands:** `git show` source/diff inspection and SHA-256 comparison → evidence above.
- **Not executed:** tests, builds, lint/typecheck, mutants, or restoration.
- **待验证:** runtime mutant-red behavior and live-source restoration; optional `/tmp/anotify-core-recovery-c2r-*` logs were not inspected. HTTP wire-level 409 mapping was not rechecked.
- No whole-C2, schema, or C3 review was performed.

DONE C2-R-M2

{"ok": true, "reason": "C2-R M2 discriminator closed at e42fe84: captured old token has equal revision and different UID; all three Node name mutations require complete current-token conflict and unchanged replacement root/config."}
