## C3b IMPLEMENTATIONGATE

**Non-blocking pass: no critical/major findings; one minor finding.** Reviewed fixed HEAD `b46fd2c4d8bd233a056d5313ee40f052ac004d3c`; worktree remained clean. C3a/C1/C2 were not reopened.

### Facts and call chains

1. **Complete result facts:** `TaskResult` contains nullable `ExitCode` and `time.Time` start/finish. JSON normalizes UTC without truncating nanoseconds; missing process times remain nullable projections.  
   Evidence: `internal/store/task.go:826–854`.

2. **First completion:** `CompleteTaskClassifiedAuthorized(auth, taskID, attempt, leaseID, serverNow, result)` checks current node credential, namespace, current execution, cancellation and lease expiry before reference validation and terminal writes in one transaction.  
   Evidence: `internal/store/execution_auth.go:687–747`, `:807–875`.

3. **Clock separation:** complete JSON retains reported process times; task/attempt projections use those times, while first `result_accepted_at` and reap observation use independent `now`.  
   Evidence: `internal/store/execution_auth.go:839–867`.

4. **Duplicate ACK:** requires current task/attempt/node/lease binding, equal complete canonical result JSON in both retained rows, matching accepted-time evidence and exact retained result references. Success returns without DML. Missing facts cannot infer ACK.  
   Evidence: `internal/store/execution_auth.go:759–806`.

5. **Upload flow:** `BeginArtifactUpload(auth, response)` commits an opaque ticket/object before external streaming. Registration takes that ticket plus actual hash/size, reauthorizes, then transactionally applies identity, expiry and quota checks; concurrent duplicates return the actual winning object.  
   Evidence: `internal/store/artifact_manifest.go:25–39`, `:93–126`, `:174–254`.

6. **Reference isolation:** result manifests are validated against registered paths/hashes for the exact task/attempt; `artifact_upload_failed` must preserve all uploaded partial references.  
   Evidence: `internal/store/artifact_manifest.go:307–329`.

7. **GC protection:** task TTL uses trusted acceptance time, with busy, active-ticket and unreaped guards. Byte GC retains manifest expiry metadata and returns retryable opaque purge work. Capability purge checks actual task/attempt/manifest/deployment/receipt UID references.  
   Evidence: `internal/store/retention.go:149–159`, `:307–344`; `internal/store/artifact_manifest.go:399–449`.

8. **Namespace force:** CAS and unreaped/busy guards precede every delete. External ownership references block clearing. Successful deletion explicitly clears local execution/history/configuration rows but does not delete global nodes or content files.  
   Evidence: `internal/store/namespace.go:310–412`.

The shared parameter-name implementation delegates to protocol: `internal/artifact/artifact.go:660`, `:680`.

### Finding / risk

**Minor — same-hash mixed-expiry download lookup can return premature 410.**

`ListArtifactManifest` orders by logical path. `GetTaskArtifactDownload` immediately returns `ErrArtifactExpired` for the first matching result reference, without considering subsequent matching references.

Two paths in the same task/attempt may legitimately share a hash and have different publication/expiry times. Consequently, an expired alphabetically earlier copy can mask a later, still-live registered copy. This is a bounded availability window, not cross-task access or destructive GC.

Evidence: `internal/store/artifact_manifest.go:235`, `:339`, `:383–386`; `internal/store/schema.sql:616`. Same-hash logical paths are expressly permitted by `/root/code/anotify/.pi-orchestrator/v4/ARTIFACT-HANDOFF.md:21`.

### Regression evidence and remaining validation

Static inspection found strong whole-row/no-write assertions, including:

- `TestC3bCompleteResultFullFactsDiscrimination` — `result_facts_test.go:61`
- `TestC3bCompleteResultOldFinishCannotReviveExpiredLease` — `result_facts_test.go:288`
- `TestC3bCompleteResultMissingFactsNeverInferACK` — `result_facts_test.go:430`
- `TestC3bRegisterReauthorizesAfterPrecheckZeroFacts` — `artifact_registry_test.go:49`
- `TestC3bConcurrentSamePathRegistrationActualOwner` — `result_current_binding_test.go:97`
- `TestC3bNamespaceForcePhysicalOccupancyWholeFacts` — `reference_force_test.go:23`

Mixed-expiry same-hash lookup remains **runtime-unverified**; the finding is source-derived. G1 whitebox/G2/G3/G4 remain intentionally pending, not missing C3b implementation findings.

**Delivery:** no changed files; no added tests. `git rev-parse HEAD` matched the fixed SHA; `git status --short` was empty, both exit 0. No mutation, test/build rerun, duplicated full gate or child agent was performed. Previously accepted fixed-command results were not independently re-executed.

{"ok":true,"gate":"implementationgate","findings":[{"path":"internal/store/artifact_manifest.go","line":386,"severity":"minor","evidence":"Download lookup returns artifact_expired on the first path-ordered matching result reference. Same-task/attempt same-hash paths are permitted and expire relative to individual publication times, so an expired earlier path can mask a later still-live registered copy. Source-derived; runtime reproduction not performed."}],"reason":"Fixed b46fd2c4 C3b read-only implementationgate: no critical or major findings; one non-blocking bounded download-availability finding. G1 whitebox/G2/G3/G4 remain intentionally pending."}
