# NEXT C3aR2 implementation gate

implementationGate: PASS (scoped self-verification; not T1 DONE / not parent review acceptance)
HEAD: 9e18f59bfd0f405bb66d3692852cb53a88e71e78
branch: pi-subagent/v4-core-runtime-next-59fc6b04
verification source HEAD: ff4859f709a08f8a8bd008d33fc7567572dbbfd6; verified test bytes are committed unchanged.

## 做了什么 / changed file
- ONLY final source change: internal/store/v4test/execution_auth_test.go (145 insertions, 14 deletions).
- Strengthened existing tests (no new test functions):
  - TestAckCancelAuthorizedHistoricalBinding
  - TestAckCancelHistoricalReapIsByteIdempotent
  - TestAckCancelPreservesHistoricalFailureFacts
- Explicit first ACK Now; require non-null ReapedAt exactly equal to that input.
- Preserve entire historical attempt except the permitted first ReapedAt/reclaim reason; preserve complete task and new attempt.
- Reuse instanceLedger via local snapshots of every declared/affected slot. First ACK must clear only the old slot; every other slot stays exact. Replays with distinct Now/reason compare whole task, old/new attempts and every ledger tuple by DeepEqual.
- Historical failure replay rereads the whole task and ledger; current-cancel replay compares complete terminal task/attempt/ledger.
- Kept existing binding/rotation/namespace/expiry/budget/D2 assertions. No Auth/budget interface, wire/API, manager/protocol/schema or final production changes.

## Mutation discrimination / exact restoration
- task_attempt.go single fault: first SQL reaped_at argument now -> now+1 (valid non-null wrong time). Functional red exit=1 in all 3 tests; exact restore then same command green exit=0.
- execution_auth.go single fault: only an already-reaped repeat ACK writes tasks.summary=mutation_repeat_summary. Functional red exit=1 in all 3 tests on whole-task equality; exact restore then same command green exit=0.
- Original/restored SHA256:
  - execution_auth.go: 8472a3d6aeb9ceeb6f8e1fb1131eca3e43fc167b7d3c6e66ae401970e227bc6c
  - task_attempt.go: e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637
- Mutant SHA256:
  - wrong first time: c5e1ee9206a2eeb179079064b1f2d93d51b4f572fdafd81484f80be76d6bd977
  - repeat summary: b0ee3bd000105085574bf9cf4deaea6b3ac9213d42c4b705b20fb66110918b6f
- cmp exact restore exit=0 after each fault; SHA checks OK; final product diff versus 49253553ff5eec5f90e03cb3393a791284b408c0 exit=0.
- Initial timestamp trial exposed that ackReapInput leaves Now=0. That trial is NOT discrimination evidence: explicitly set Now, confirmed restored focused baseline exit=0, then reran valid timestamp fault once. No compile-failure evidence, no third retry, no retained mutant.

## Fixed four verification commands (actual outputs below)
All timeout 60; official direct Go env; exit=0 for gofmt, build, vet, full scoped v4test+artifact+protocol -count=1.
No lens tools exposed in this worker; actual frozen Go vet/build used for mechanical diagnostics.

## 未决问题 / MR
- No discovered product bug; this closes only M3 coverage gaps.
- C3b + G1 whitebox + G2–4 pending; not T1 DONE.
- No push/merge/MR, per this task's NoPushMergeMR restriction.
- Unrelated untracked .pi-orchestrator/v4/teams/core-runtime/task-cards/NEXT-C3b.md appeared during work; not read, modified, or committed.
- Final complete scoped diff reviewed; only the authorized test source committed, no debug or mutation code retained.

## 原样验证输出（包含命令与结果）

### /tmp/anotify-core-next-c3ar2-initial-zero-now-not-evidence.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
fault=task_attempt.go: first reaped_at valid non-null Now+1
baseline_sha256=e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637  /tmp/anotify-core-next-c3ar2-task_attempt.go.original
mutant_sha256=c5e1ee9206a2eeb179079064b1f2d93d51b4f572fdafd81484f80be76d6bd977  internal/store/task_attempt.go
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
--- FAIL: TestAckCancelAuthorizedHistoricalBinding (0.02s)
    execution_auth_test.go:1078: 首历史 ACK 必须写精确 ReapedAt=0：&{TaskID:tsk_BCI6KpMKK75JT5JHbW Attempt:1 NodeID:nd_BCI6KpM5rH7fzG2adn NodeName:node-a InstanceID:nins_BCI6KpMMOVnhWeb6j6 Slot:0 LeaseID:lse_BCI6KpMo8ME1zKGJpV AttemptTokenHash:480ee8b09efbc8a966655f6c7a5acec6d38cb43cfc5045b67ace60ab73590f1c ConfigRevision:1 ConfigHash:sha256:a33ac9df59c596c1ae920e4cbd701ade01f1a0c2938f13dac467142bab77946c MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:2] NamespaceVarsRevision:0 Phase:requeued Error: StartedAt:0xc000314f78 FinishedAt:0xc000314f80 ReapedAt:0xc000314f88 ReclaimReason:cancel_ack} err=<nil>
--- FAIL: TestAckCancelHistoricalReapIsByteIdempotent (0.02s)
    execution_auth_test.go:1236: 第一次历史 ACK 必须写精确 ReapedAt=0：&{TaskID:tsk_BCI6KpUuxHlMlNTfPC Attempt:1 NodeID:nd_BCI6KpUUAu641b2i04 NodeName:node-a InstanceID:nins_BCI6KpUevOGJu6pJSZ Slot:0 LeaseID:lse_BCI6KpVGxbPln9F5Nl AttemptTokenHash:6eb4c76badd5af56229b3744719e0710bbf215d18ddc7c6ae9a4187bb49602fd ConfigRevision:1 ConfigHash:sha256:384fc08b1629e05f327e4de886ca6458d70394ef5f78becb6dc178e5cf9112a4 MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:3] NamespaceVarsRevision:0 Phase:requeued Error: StartedAt:0xc000377bf8 FinishedAt:0xc000377c00 ReapedAt:0xc000377c08 ReclaimReason:cancel_ack} err=<nil>
--- FAIL: TestAckCancelPreservesHistoricalFailureFacts (0.02s)
    execution_auth_test.go:1361: 第一次历史 ACK 必须写精确 ReapedAt=0：&{TaskID:tsk_BCI6Kpd9j3LHdoPbxy Attempt:1 NodeID:nd_BCI6KpccVwHkaX4UKh NodeName:node-a InstanceID:nins_BCI6KpcUp3tJ2C1ZGR Slot:0 LeaseID:lse_BCI6KpcyfFgpiZmdWv AttemptTokenHash:351aac275cbb694fa7135b7e4bb06114360243e1317e4954993fe20721de54af ConfigRevision:1 ConfigHash:sha256:a33ac9df59c596c1ae920e4cbd701ade01f1a0c2938f13dac467142bab77946c MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:2] NamespaceVarsRevision:0 Phase:cancelled Error:cancelled_by_user StartedAt:0xc000453228 FinishedAt:0xc000453230 ReapedAt:0xc000453238 ReclaimReason:cancel_ack}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.074s
FAIL
exit=1
restored_sha256=e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637  internal/store/task_attempt.go
exact_restore_exit=0
```

### /tmp/anotify-core-next-c3ar2-green-focused.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.091s
exit=0
```

### /tmp/anotify-core-next-c3ar2-red-exact-time.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
fault=first historical reaped_at=valid Now+1; only first SQL time argument changed
8472a3d6aeb9ceeb6f8e1fb1131eca3e43fc167b7d3c6e66ae401970e227bc6c  internal/store/execution_auth.go
c5e1ee9206a2eeb179079064b1f2d93d51b4f572fdafd81484f80be76d6bd977  internal/store/task_attempt.go
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
--- FAIL: TestAckCancelAuthorizedHistoricalBinding (0.03s)
    execution_auth_test.go:1079: 首历史 ACK 必须写精确 ReapedAt=1794276895：&{TaskID:tsk_BCI6PYESeS4YyZwDA0 Attempt:1 NodeID:nd_BCI6PYDY6zvQhNJVkO NodeName:node-a InstanceID:nins_BCI6PYDoAqT5G0paoK Slot:0 LeaseID:lse_BCI6PYEp3UNDyabwEK AttemptTokenHash:0693b0ce7d66153400b6afda49fed06d95d85f3b6fa1cb06474e8b7da51e98f2 ConfigRevision:1 ConfigHash:sha256:a33ac9df59c596c1ae920e4cbd701ade01f1a0c2938f13dac467142bab77946c MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:2] NamespaceVarsRevision:0 Phase:requeued Error: StartedAt:0xc00032cec8 FinishedAt:0xc00032ced0 ReapedAt:0xc00032ced8 ReclaimReason:cancel_ack} err=<nil>
--- FAIL: TestAckCancelHistoricalReapIsByteIdempotent (0.02s)
    execution_auth_test.go:1239: 第一次历史 ACK 必须写精确 ReapedAt=1794276895：&{TaskID:tsk_BCI6PYNTzZGK3hdNVh Attempt:1 NodeID:nd_BCI6PYNAhEvxq9TxRK NodeName:node-a InstanceID:nins_BCI6PYN90Fg57rVjOC Slot:0 LeaseID:lse_BCI6PYNvmNzGtLTWGs AttemptTokenHash:80e342914d6d4ea9b34cf085fba05d2c586b735ce593dea82b5f52ee14f9a159 ConfigRevision:1 ConfigHash:sha256:384fc08b1629e05f327e4de886ca6458d70394ef5f78becb6dc178e5cf9112a4 MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:3] NamespaceVarsRevision:0 Phase:requeued Error: StartedAt:0xc00041db58 FinishedAt:0xc00041db60 ReapedAt:0xc00041db68 ReclaimReason:cancel_ack} err=<nil>
--- FAIL: TestAckCancelPreservesHistoricalFailureFacts (0.02s)
    execution_auth_test.go:1365: 第一次历史 ACK 必须写精确 ReapedAt=1794276895：&{TaskID:tsk_BCI6PYVj3piYPELLaU Attempt:1 NodeID:nd_BCI6PYVJbSMVaWF38R NodeName:node-a InstanceID:nins_BCI6PYVWUQZwwQihRH Slot:0 LeaseID:lse_BCI6PYWDYgSzgdWCQM AttemptTokenHash:91a122717c3939f2466bd1f143f7048ce96ce488c876c944b01ccf7294501d07 ConfigRevision:1 ConfigHash:sha256:a33ac9df59c596c1ae920e4cbd701ade01f1a0c2938f13dac467142bab77946c MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:2] NamespaceVarsRevision:0 Phase:cancelled Error:cancelled_by_user StartedAt:0xc000477188 FinishedAt:0xc000477190 ReapedAt:0xc000477198 ReclaimReason:cancel_ack}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.087s
FAIL
exit=1
```

### /tmp/anotify-core-next-c3ar2-restore-time.log
```text
restore=time
internal/store/execution_auth.go: OK
internal/store/task_attempt.go: OK
exact_restore_exit=0
```

### /tmp/anotify-core-next-c3ar2-green-exact-time.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
fault=none; exact byte restore
8472a3d6aeb9ceeb6f8e1fb1131eca3e43fc167b7d3c6e66ae401970e227bc6c  internal/store/execution_auth.go
e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637  internal/store/task_attempt.go
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.085s
exit=0
```

### /tmp/anotify-core-next-c3ar2-red-repeat-summary.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
fault=repeat ACK only: already-reaped row mutates tasks.summary
b0ee3bd000105085574bf9cf4deaea6b3ac9213d42c4b705b20fb66110918b6f  internal/store/execution_auth.go
e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637  internal/store/task_attempt.go
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
--- FAIL: TestAckCancelAuthorizedHistoricalBinding (0.02s)
    execution_auth_test.go:1113: 重复历史 ACK 不得改整任务：before=&{ID:tsk_BCI6PoBzvEDNur7KZB NamespaceID:ns_BCI6PoBmigHI3fP7Be TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_AUYYA8x Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:running NodeID:0xc000383830 NodeName:node-a ConfigRevision:1 Attempt:2 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID:lse_BCI6PoDDPE0hV1s4PC LeaseUntil:0xc000394be0 LeaseInstanceID:nins_BCI6PoBzxgfodBXpwn AttemptTokenHash:112cd9ca403a8a73bfabe7193e5e63a0da2924c09350f2399259ef0400d4a786 WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794276897 StartedAt:0xc000394be8 FinishedAt:<nil>} after=&{ID:tsk_BCI6PoBzvEDNur7KZB NamespaceID:ns_BCI6PoBmigHI3fP7Be TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_AUYYA8x Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:running NodeID:0xc0003a4550 NodeName:node-a ConfigRevision:1 Attempt:2 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID:lse_BCI6PoDDPE0hV1s4PC LeaseUntil:0xc000395a50 LeaseInstanceID:nins_BCI6PoBzxgfodBXpwn AttemptTokenHash:112cd9ca403a8a73bfabe7193e5e63a0da2924c09350f2399259ef0400d4a786 WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary:mutation_repeat_summary Artifacts:[] Error: CreatedAt:1794276897 StartedAt:0xc000395a58 FinishedAt:<nil>}
--- FAIL: TestAckCancelHistoricalReapIsByteIdempotent (0.02s)
    execution_auth_test.go:1286: 重复历史 ACK 不得改整任务/整新 attempt：task before=&{ID:tsk_BCI6PoK8x08ExlW5l5 NamespaceID:ns_BCI6PoJZnjtPwiQ2CL TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_AggNrd5 Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:running NodeID:0xc0004241f0 NodeName:node-a ConfigRevision:1 Attempt:2 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID:lse_BCI6PoL0KVHc9EVMSX LeaseUntil:0xc0004189e0 LeaseInstanceID:nins_BCI6PoJlMKZu3uBOc7 AttemptTokenHash:e7f978f068238accd223a03d23c645f396ae4ec722eabd780f1ad70df66407ff WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794276897 StartedAt:0xc0004189e8 FinishedAt:<nil>} after=&{ID:tsk_BCI6PoK8x08ExlW5l5 NamespaceID:ns_BCI6PoJZnjtPwiQ2CL TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_AggNrd5 Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:running NodeID:0xc000424c30 NodeName:node-a ConfigRevision:1 Attempt:2 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID:lse_BCI6PoL0KVHc9EVMSX LeaseUntil:0xc000419530 LeaseInstanceID:nins_BCI6PoJlMKZu3uBOc7 AttemptTokenHash:e7f978f068238accd223a03d23c645f396ae4ec722eabd780f1ad70df66407ff WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary:mutation_repeat_summary Artifacts:[] Error: CreatedAt:1794276897 StartedAt:0xc000419538 FinishedAt:<nil>} attempt before=&{TaskID:tsk_BCI6PoK8x08ExlW5l5 Attempt:2 NodeID:nd_BCI6PoJyeZuXNuaHXx NodeName:node-a InstanceID:nins_BCI6PoJlMKZu3uBOc7 Slot:1 LeaseID:lse_BCI6PoL0KVHc9EVMSX AttemptTokenHash:e7f978f068238accd223a03d23c645f396ae4ec722eabd780f1ad70df66407ff ConfigRevision:1 ConfigHash:sha256:384fc08b1629e05f327e4de886ca6458d70394ef5f78becb6dc178e5cf9112a4 MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:3] NamespaceVarsRevision:0 Phase:running Error: StartedAt:0xc000418bb8 FinishedAt:<nil> ReapedAt:<nil> ReclaimReason:} after=&{TaskID:tsk_BCI6PoK8x08ExlW5l5 Attempt:2 NodeID:nd_BCI6PoJyeZuXNuaHXx NodeName:node-a InstanceID:nins_BCI6PoJlMKZu3uBOc7 Slot:1 LeaseID:lse_BCI6PoL0KVHc9EVMSX AttemptTokenHash:e7f978f068238accd223a03d23c645f396ae4ec722eabd780f1ad70df66407ff ConfigRevision:1 ConfigHash:sha256:384fc08b1629e05f327e4de886ca6458d70394ef5f78becb6dc178e5cf9112a4 MachineVars:map[] MachineVarSources:map[] MachineLimits:map[total:3] NamespaceVarsRevision:0 Phase:running Error: StartedAt:0xc000419708 FinishedAt:<nil> ReapedAt:<nil> ReclaimReason:}
--- FAIL: TestAckCancelPreservesHistoricalFailureFacts (0.02s)
    execution_auth_test.go:1407: 重复历史失败 ACK 不得改整任务：before=&{ID:tsk_BCI6PoSGWnZPyqNGte NamespaceID:ns_BCI6PoRpuiqTTogXRz TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_Icq7bEW Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:cancelled NodeID:0xc00047abb0 NodeName:node-a ConfigRevision:1 Attempt:1 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID:nins_BCI6PoS6v4WX5k9y3T AttemptTokenHash:8693b36baba2111fc533a93f0d030292118b20448536c0ba1b28d626b1620778 WaitingReason: NextAttemptAt:<nil> CancelReason:cancelled_by_user CancelRequestedAt:0xc000477040 Reported:false Summary: Artifacts:[] Error:cancelled_by_user CreatedAt:1794276897 StartedAt:0xc000477048 FinishedAt:0xc000477050} after=&{ID:tsk_BCI6PoSGWnZPyqNGte NamespaceID:ns_BCI6PoRpuiqTTogXRz TraceID:trace-1 ParentTaskID:<nil> CapabilityID:cap_1794276897_c6079e7f_Icq7bEW Capability:idem Version:1 Digest:sha256:c6079e7faba8fd1c89a9cbb600c013a272adf7fe7f80f2af5d20ac285dfdf873 Params:[123 125] Phase:cancelled NodeID:0xc00047b430 NodeName:node-a ConfigRevision:1 Attempt:1 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID:nins_BCI6PoS6v4WX5k9y3T AttemptTokenHash:8693b36baba2111fc533a93f0d030292118b20448536c0ba1b28d626b1620778 WaitingReason: NextAttemptAt:<nil> CancelReason:cancelled_by_user CancelRequestedAt:0xc0004779b0 Reported:false Summary:mutation_repeat_summary Artifacts:[] Error:cancelled_by_user CreatedAt:1794276897 StartedAt:0xc0004779b8 FinishedAt:0xc0004779c0} err=<nil>
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.073s
FAIL
exit=1
```

### /tmp/anotify-core-next-c3ar2-restore-summary.log
```text
restore=summary
internal/store/execution_auth.go: OK
internal/store/task_attempt.go: OK
exact_restore_exit=0
```

### /tmp/anotify-core-next-c3ar2-green-repeat-summary.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
fault=none; exact byte restore
8472a3d6aeb9ceeb6f8e1fb1131eca3e43fc167b7d3c6e66ae401970e227bc6c  internal/store/execution_auth.go
e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637  internal/store/task_attempt.go
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.076s
exit=0
```

### /tmp/anotify-core-next-c3ar2-check1-gofmt.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
command=test -z "$(gofmt -l internal/store internal/artifact)"
exit=0
```

### /tmp/anotify-core-next-c3ar2-check2-build.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store/ ./internal/artifact/
exit=0
```

### /tmp/anotify-core-next-c3ar2-check3-vet.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test/ ./internal/artifact/
exit=0
```

### /tmp/anotify-core-next-c3ar2-check4-test.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=ff4859f709a08f8a8bd008d33fc7567572dbbfd6
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	24.950s
ok  	github.com/zzjcool/anotify/internal/artifact	2.404s
ok  	github.com/zzjcool/anotify/internal/protocol	0.044s
exit=0
```

### /tmp/anotify-core-next-c3ar2-delivery.log
```text
pwd=/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
head=9e18f59bfd0f405bb66d3692852cb53a88e71e78
branch=pi-subagent/v4-core-runtime-next-59fc6b04
9e18f59 test(store): 补齐历史回收精确时刻与完整事实幂等断言
 internal/store/v4test/execution_auth_test.go | 159 ++++++++++++++++++++++++---
 1 file changed, 145 insertions(+), 14 deletions(-)
internal/store/execution_auth.go: OK
internal/store/task_attempt.go: OK
product_diff_exit=0
test_worktree_diff_exit=0
?? .pi-orchestrator/v4/teams/core-runtime/task-cards/NEXT-C3b.md
```

DONE NEXT-C3aR2 (scoped implementation only; NOT T1 DONE)
