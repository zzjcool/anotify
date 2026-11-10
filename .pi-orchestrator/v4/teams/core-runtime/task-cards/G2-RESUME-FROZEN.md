# G2 resume frozen task / ownership / acceptance · 2026-11-17

This is a G2-only local execution card, not replacement authority. Original task-cards/G2.md and task-cards/NEXT-G2-NOTES.md remain unchanged. Parent RESUME-01A1C041.md and current direct parent instruction govern recovery. G1 source epoch 32ec26b / clean delivery HEAD 50fb9ac7c79d07c9a6c439c8d685898ea17483d8 are admitted facts; no rerunning old baseline/recon, no wait for T1/Node/C3. Node5a3dae3/protocol33e already frozen. Current branch pi-subagent/core-g2-resume-52fc54d4, canonical cwd/root/GOMOD=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume[/go.mod]. No new WT, branch copying/integration, model or preset overrides.

## Authority and deliberate decisions

Read AGENTS/DEVELOPMENT; frozen .pi-orchestrator/v4/CONTRACT.md; docs/design/v4/decisions.md D1-D6 and relevant four semantic documents/config; WIRE-HANDOFF.md W1-W6; ARTIFACT-HANDOFF.md A1/A2; SLOT-HANDOFF.md A3; RESULT-FACTS-HANDOFF.md; G1-CLOSURE-HANDOFF.md; EXPORTED-INTERFACES.md ACTUAL C2/C3a/C3b FINAL sections (older PENDING/historical signatures superseded); HTTP-HANDOFF-CHECKLIST.md; G2 and NEXT-G2-NOTES. Shared protocol source, store source and their related named tests are read-only current definition sources.

- D1 lightweight spec/status, facts-derived status, no reconciler/generation/finalizer/aliases/new kinds.
- D2 local > node > task namespace vars; local/node limits only. Applied machine only; same logical task pins namespace inputs; each attempt pins its own machine snapshot.
- D3 installation is insert-only, no route mutation/autodeploy; true conflict atomic 0 writes. Manual deployment explicit only.
- D4 default non-idempotent, infra structured facts only for eligible retry, one logical task, grant counts attempt, 30s proposal / TTL60 / renew20 / reap5; revoke authorization is not physical reap. Explicit business emit id only, authorization precedes replay.
- D5 clean env and name-only diagnostics; no fabricated environment/log secrets. Node execution remains T3-owned.
- D6 finite trusted-clock retention, active/referenced protection, no user path/data deletion. No v3 migration/compatibility; retire central capability executor/old cmd/agent deliberately, preserve independent auth/broker/push/device/notify and non-Node WS.
- W1/W5 CAS and current credential/namespace authority are store-transaction responsibilities. W2 immutable task body/version/digest. W3 install does not deploy. W4 capture actual authentication token hash at connection, not fetch-current-hash to self-authorize. W6 ingress/dryRun/log/emission shapes unchanged.

## Exclusive ownership and sequence

At most ONE source writer, dispatched worktree:false in current canonical cwd; no nested writers. No centurion writes while that writer is live. Centurion owns .pi-legion/contract.md and the exact new G2-RESUME-FROZEN.md/G2-RESUME-PROGRESS.md/G2-RESUME-VERDICT.md plus G2-only review/verify evidence directory teams/core-runtime/g2-resume-evidence/**. Writer owns exclusively:

1. internal/route/**
2. internal/ws/** (retain non-Node WS and notification behavior)
3. internal/lease/**
4. internal/source/**
5. internal/trigger/**
6. internal/logging/** ONLY protocol/log-routing adaptation; retain independent logger
7. internal/executor/** ONLY deletion of retired central capability executor
8. cmd/agent/** ONLY deletion of old Node product; not cmd/anode
9. .pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md ONLY G2 named runtime rows; preserve all G1 rows/evidence.
10. Exact new .pi-orchestrator/v4/teams/core-runtime/g2-resume-evidence/G2-WRITER-INTERFACES.md and G2-WRITER-VERDICT.md (writer's public signatures, test inventory/mapping, checkpoints, gate exit/log report).

All store/protocol/artifact/sourcespec/playbook/server/api/tools/Go deps/Makefile/Node/T2/T3/frozen design/parent status are READ-ONLY. No settings/env/node_modules/.git writes, no deletion of old evidence/user data, no push/MR/merge/rebase/reset/squash/WT/session cleanup. Ownership changes require precise parent ruling first. Local owner-only commit allowed, never broad git add.

Sequence: freeze contract -> phase implementing -> single source worker -> collect actual worker session/verdict and fixed four gates -> phase reviewing -> three fresh-context independent scout reviews (correctness/regression; coverage + named old→new; simplicity), G2 changes only -> precise authorized same-owner rework + fresh re-review for major/critical -> phase verifying only if independent strict ok:true and all conditions real. Supervisor executes legion-verify; actual exits/logs, not phase/self-report, determine acceptance. Final G3/G4 and full go-all/contract/e2e-quick still required upstream, never replaced by these four G2 gates. If unresolved protected gap remains, SAFE_BLOCKED G2, not DONE.

## Frozen public consumer / wire / authority seams

Single shared protocol DTO source, protocol.Decode + direction checks on actual WS ingress/egress, presence validation before integer use. Public runtime internal adapters may change inside owned modules but MUST expose their final real Go signatures in G2-WRITER-INTERFACES.md for G3; no second shared wire type or legacy export shim. The exact store consumer signatures are already implemented (ACTUAL final):

```go
type store.NodeAuthorization struct { NodeID, NodeTokenHash string }
func (*store.DB) GrantLeaseAuthorized(ctx context.Context, auth store.NodeAuthorization, taskID, assignmentID string, proposedAttempt, slot int, now int64) (leaseID, attemptToken string, attempt int, expiresAt int64, err error)
func (*store.DB) RenewLeaseAuthorized(ctx context.Context, auth store.NodeAuthorization, taskID string, attempt int, leaseID string, now int64) (int64, error)
func (*store.DB) CompleteTaskClassifiedAuthorized(ctx context.Context, auth store.NodeAuthorization, taskID string, attempt int, leaseID string, now int64, result store.TaskResult) (store.TaskResultDisposition, error)
func (*store.DB) AckCancelAuthorized(ctx context.Context, auth store.NodeAuthorization, in store.ReapedInputs, processGroupReaped bool) error
func (*store.DB) EmitAuthorized(ctx context.Context, auth store.EmitAuth, rec *store.EmitRecord, tasks []*store.Task) (json.RawMessage, bool, error)
```

- Route: current Capability spec.next / Source normalized targets projection, payload $(payload...) mapping + actual artifact validation. Pins namespace/version/digest/params, exact per-target prune receipt; no ports/priority/sticky/legacy handlers or central exec.
- Source: manual/webhook/cron normalized kind/config/targets authority; unified {eventId,traceId,duplicate,tasks:[{taskId,capability,version}],pruned:[{capability?,reason,detail}]} via actual atomic accept/tasks/delivery window. Omitted payload uses source config/{}. DryRun {targets:[{capability,version?,params,state:ready|pruned|missing,reason?,detail?}],pruned:[...]} entirely in memory; 0 event/task/delivery/secret/receipt writes. Cron effect/errors not swallowed.
- Dispatch only assignmentId/proposedAttempt/assignmentExpiresAt 30s, DB UID -> wire registered Node name, body never selects auth. Accept slot required/present, 0 legal, exact accepted slot. Current hash/ns/assignment/proposed/slot/all budgets checked by GrantLeaseAuthorized transaction. Same-binding duplicate idempotent; old expired requests cannot damage new reservation; stale_config/slot_unavailable no attempt, bounded backoff.
- NodeSession immutable authenticated NodeID + credential hash captured from actual connection. Every inbound authorized write must use current DB identity authority, never a stale snapshot or Get-precheck + unguarded execution mutation. Hello/applied machine vars/limits/revision/hash and explicit observations only; missing/empty/unknown busy, terminal phase/rehello/disconnect never reap; preserve busy above shrunken budget.
- Grant/renew/result/emit/cancel direct shared Decode/direction. Renewal ACK and expiry==now rejection. Complete uses full typed manifest/nullable exit/nanosecond original process times with separate serverNow; ACK only Recorded/Duplicate full facts, arbitrary errors not ACK. Wrong binding / expired / revoked never writes or revives.
- Cancel producer exact task/attempt/lease/slot. ValidateCancelAck before identity/history CAS; only AckCancelAuthorized with processGroupReaped=true matching history, no bare AckCancel/ReleaseSlot authority. Wrong/missing/false/negative binding no release, old ack only old occupancy, exact repeats idempotent.
- Emit requestId is correlation ONLY; independent accepted event business identity, explicit id replay after EmitAuthorized current credential/ns/lease/token/cancel authority, no-id new each call / same-id same body one fact / changed body409. Exact shared HTTP emit response/error DTO, no mirrors.
- Manualdeploy namespace/name/version/digest only, actual current target/auth receipt. Online notify; offline pending intents replay ONLY explicit requested deploy; no install/auto reconciler/full files in WS.
- Logs request actual history attempt's Node, validate reply requestId/taskId/actual attempt; previous without history returns true empty array, offline/archive-expired truthful error. Keep upgrade frames solely existing shared/T3 contract. Independent logger retained.
- Lease/retention/stats consume real current store, no old aliases.

### Parent-confirmed structured log gap / explicit pending

Parent recovery supplement confirms current shared LogsResponseFrame provides stdout/stderr/truncated/bytes/attempt but no per-record sequence/stream/text/at; W6/A1 require actual structured historical records. Parent is doing limited seam adjudication. This specific gap is PENDING, not a reason to rediscover/rewrite G1 or stall unrelated G2. Writer may implement exact historical request routing/correlation using current shared facts, but MUST NOT invent timestamps, cross-stream order, empty success, private DTO wire, or modify protocol/Node. Record exact path:line/signature gap and leave structured output explicitly pending until parent supplies approved shared/Node snapshot. No claim that partial/raw logs meet W6. Other missing/contradictory atomic entry -> stop precise affected path and report to parent; do not fake store authority/precheck or expand ownership.

## Test acceptance / preservation

Read original related *_test.go before changes. Inventory each original G2 file/name in TEST-MAPPING individually: retained, migrated with exact new name, deliberately retired ONLY old ports/priority/sticky/handler/centralexec/old Node product. Do not delete unrelated notification/broker/device/auth coverage or weaken assertions for green. Add actual httptest WS + shared decoder/direction tests: dispatch no early exec, slot0/missing/negative, duplicate/wrong/expired assignment, renew==expiry, credential rotate/ns revoke for every execution channel, wrong/new-history cancel lease/slot/missing/false/idempotence, rehello/disconnect/unknown busy, budget shrink preservation, classified result duplicates/full facts, emit no-id/id/conflict/auth-before-replay, cross-node historical logs/correlation/offline and the structured pending negative boundary, offline explicit deployment, Source mapping/normal receipt/delivery window/omitted payload/0-write preview; real lease/retention/stats interfaces. No stand-alone DTO/compile-only substitutes.

All sockets have settimeout/context deadlines, read/write bounded, diagnostic probes timeout60. Commands that may exceed60s use async_run(name, full saved log) and async_wait collected before verdict, never naked background/recv/poll loop. Same root cause at most 2 failed meaningful retries (renamed tasks do not reset); then SAFE_BLOCKED with exact checkpoint, attempted methods/raw outputs/current hypothesis/affected paths/recommended next step. No repeated unchanged old baselines.

## Frozen four mechanical gates

Canonical cd/root/GOMOD guard applies to each command, GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto. Fixed launch verify array retains ALL four gates, no reduction:

1. `gofmt -w internal/route internal/ws internal/lease internal/source internal/trigger internal/logging` plus `git diff --check`.
2. `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/...`
3. `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... -count=1`
4. `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/artifact/... ./internal/protocol/... -count=1`

Store/artifact/protocol regression gate is read-only, not invitation to fix protected files. Extension runs fixed four after writer freezes; self tests/vet and all exits/full logs still mandatory. Any failure report honestly, no gate skip/compat shim/settings hack. Upper old server failures may remain for G3/G4, but these exact G2 gates must not be faked.

## Required verdict

G2-WRITER-INTERFACES.md + G2-WRITER-VERDICT.md: actual HEAD/branch/source epoch; exact changed files; public runtime signatures; all newly named tests and individually named old→new/retired mappings with deliberate-decision reasons; four actual gate command/exit/log paths; all managed tasks collected; product bugs and precise protected gap; pending G3/G4/final three not claimed. End DONE G2 only full actual scope+review+real gate acceptance, otherwise SAFE_BLOCKED G2 with nextgap. Final JSON own line {"ok":true|false,"reason":"branch ... at SHA ...; actual result / blocking evidence"}. No MR/push/merge: local owner-only commit then parent collection/integration only.
