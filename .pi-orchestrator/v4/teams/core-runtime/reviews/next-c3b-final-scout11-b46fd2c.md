## Implementation gate — C3b at `b46fd2c4d8bd233a056d5313ee40f052ac004d3c`

### Facts

- **Registration reauthorizes before mutation**, including the supplied attempt token; duplicate detection follows authorization. Quotas are checked inside the registration transaction. Evidence: `internal/store/artifact_manifest.go:81`, `:191`, `:194`, `:219`; discriminants: `internal/store/v4test/artifact_registry_test.go:49`, `:165`, `internal/store/v4test/reference_isolation_test.go:98`.
- **Results require registered references belonging to the exact task/attempt.** Unuploaded, cross-task, cross-namespace and previous-attempt borrowing are rejected. Evidence: `internal/store/artifact_manifest.go:307`; tests: `internal/store/v4test/artifact_registry_test.go:254`, `internal/store/v4test/reference_isolation_test.go:79`.
- **Duplicate ACK requires current credential/namespace/current binding and complete retained facts**, including matching task/attempt acceptance timestamps and recorded manifest references. Its branch performs no DML. Evidence: `internal/store/execution_auth.go:717`, `:749`, `:766`, `:779`, `:786`, `:802`.
- **Full-facts tests discriminate every result field**, nullable exit versus zero, absent times and nanosecond differences; equal instants with different offsets and monotonic representation are accepted without changing whole-row snapshots. Evidence: `internal/store/v4test/result_facts_test.go:61`, `:131`, `:232`, `:430`. The JSON fixture has explicit value assertions and full decoded equality, not merely key-presence checks (`:52`, `:181`).
- **Authorization and retention use trusted observation time independently of Node finish.** The tests cover simulated 75-second completion delay, lease expiry equality/past, old/future process clocks, seven-day bytes and thirty-day facts. Evidence: `internal/store/execution_auth.go:823`, `:840`; `internal/store/retention.go:152`; `internal/store/v4test/result_facts_test.go:252`, `:288`, `:329`, `:377`.
- **Capability GC independently protects task, attempt and manifest pins**, plus deployment and retained receipt references. Late task production after physical purge is denied; recreation/shared digest remain intact. Evidence: `internal/store/retention.go:326`; `internal/store/v4test/reference_isolation_test.go:15`, `:187`; `internal/store/v4test/reference_force_test.go:161`, `:212`.
- **Force-deletion discriminants cover whole-fact refusal** for granted/unspawned, terminal/unreaped, offline/shrink, busy-unknown and reservation states; successful post-reap deletion preserves the global Node and outside shared content, rejects late grants and old-UID CAS. Evidence: `internal/store/v4test/reference_force_test.go:23`, `:77`, `:138`.

### Data flow

`BeginArtifactUpload(EmitAuth, typed response)` commits an opaque ticket without holding a transaction across network I/O. `RegisterArtifactUpload(auth, ticket, actualSHA256, actualSize)` reauthorizes and enforces transactional quotas. `CompleteTaskClassifiedAuthorized(NodeAuthorization, taskID, attempt, leaseID, serverNow, TaskResult)` checks execution/reference authority, records complete facts atomically, or returns a read-only duplicate disposition. Evidence: `internal/store/artifact_manifest.go:91`, `:167`; `internal/store/execution_auth.go:675`.

### Discrimination evidence and boundaries

Historical functional reds demonstrate typed-manifest/replay/force failures and removed-guard failures for final authorization, quotas and current binding. Evidence: `/tmp/anotify-core-next-c3b-red.log:4`, `/tmp/anotify-core-next-c3b-fault-mutation-red.log:5`.

Facts baseline reds demonstrate discarded fields and false ACKs; the clock mutation fails both expiry boundaries. Restoration SHA matches both the saved baseline and current source: `797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f`. Evidence: `/tmp/anotify-core-next-c3b-facts-baseline-red.log:4`, `/tmp/anotify-core-next-c3b-facts-mutation-red.log:5`, `/tmp/anotify-core-next-c3b-facts-mutation-restored-green.log:4`.

Corruption/isolation fixtures explicitly disclose their purpose (`internal/store/v4test/reference_isolation_test.go:41`, `internal/store/v4test/result_current_binding_test.go:53`). The 75-second test uses controlled server timestamps, not an HTTP streaming test. Original G1 whitebox and G2–G4 remain deliberately pending, **not findings**.

### Delivery and verification

- Changed files: **none**. Added tests: **none**.
- Reviewed **24 existing C3b tests plus 11 result-facts/schema tests**; no skip directives found in the scoped files.
- HEAD and worktree cleanliness independently checked; no tests or mutations rerun.
- Existing fixed-four logs identify the correct worktree and fixed HEAD: gofmt, build and vet exit **0**; scoped `go test ./internal/store/v4test ./internal/artifact ./internal/protocol -count=1` exits **0**, with v4test **29.931s**. This is **not full-repository verification**. Evidence: `/tmp/anotify-core-next-c3b-facts-final-verify1-gofmt.log:1–4`, `verify2-build.log:1–4`, `verify3-vet.log:1–4`, `verify4-full.log:1–7` under the same prefix.

{"ok":true,"gate":"implementationgate","findings":[],"reason":"C3b implementation gate passes at b46fd2c4d8bd233a056d5313ee40f052ac004d3c: scoped discriminants, fixture honesty, functional reds, exact SHA restoration and fixed-four logs verified; no confirmed major defect identified. Original G1 whitebox and G2–G4 remain pending, not findings. No writes, agents, test reruns or mutations performed."}
