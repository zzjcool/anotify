# Original completed assistant report before late steer (NOT final collect acceptance)

## DONE G1-6S3

**HEAD:** `bd69177b467cfe08458a632fc60300def6285e3b`
**提交:** `fix(store): 闭合授权与实例历史安全缺口`

### 产出文件（本提交仅以下 7 个路径）
- `internal/store/task.go`
- `internal/store/task_attempt.go`
- `internal/store/node.go`
- `internal/store/v4test/task_test.go`
- `internal/store/v4test/task_attempt_test.go`
- `internal/store/v4test/cluster_node_test.go`
- `internal/store/v4test/retention_test.go`

### 实现与覆盖
- Grant 事务内重查 Node 存在/状态、任务归属及授权 namespace ∩ declared namespace；授权拒绝不写 attempt、快照或占槽。
- 旧/空 grant 不清新 assignment；过期同身份不能刷新 deadline。Reject 改为事务内精确 assignment CAS。
- nil/空实例清单不构成断言；缩容保留 occupied 历史，重报不覆盖 busy 引用。
- Renew 拒绝 `now >= leaseUntil`，保留取消/current-attempt 条件。
- 保留 Reap 的非空 lease、slot、当前 Node 身份和历史精确匹配，显式无条件比较 lease。
- JSON 检查真实 decoded key map，拒绝资本化 Go key 泄漏。
- 历史 reap 后证明当前 token、真实 EmitAuthorized 写入/回放仍有效，旧 emit 即使使用既有业务 id 也拒绝。
- GC 新用例覆盖 attempt1 未回收、当前 attempt2 已终态/reaped。

**新增测试：**
- `TestGrantRechecksNodeAuthorization`
- `TestStaleGrantAndRejectPreserveNewAssignment`
- `TestExpiredAssignmentCannotRefreshDeadline`
- `TestRenewLeaseCannotReviveExpiredAuthorization`
- `TestRejectAssignmentRacePreservesNewAssignment`
- `TestUpsertInstancesPreservesOccupiedHistory`
- `TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped`

强化既有 `TestHistoricalReapRequiresExactBinding`、`TestTaskAttemptJSONShape`；未删、skip 或弱化现有测试。

### 红测原始输出
日志：`/tmp/anotify-core-recovery-s3-red.log`

```text
$ timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -count=1 -run "Test(GrantRechecksNodeAuthorization|StaleGrantAndRejectPreserveNewAssignment|ExpiredAssignmentCannotRefreshDeadline|RenewLeaseCannotReviveExpiredAuthorization|UpsertInstancesPreservesOccupiedHistory|HistoricalReapRequiresExactBinding|TaskAttemptJSONShape|RetentionProtectsOldUnreapedAttemptAfterCurrentReaped)$"
--- FAIL: TestUpsertInstancesPreservesOccupiedHistory (0.06s)
    --- FAIL: TestUpsertInstancesPreservesOccupiedHistory/nil (0.02s)
        cluster_node_test.go:777: nil erased ledger: rows=0 busy=0 err=<nil>
    --- FAIL: TestUpsertInstancesPreservesOccupiedHistory/empty (0.02s)
        cluster_node_test.go:777: empty erased ledger: rows=0 busy=0 err=<nil>
    --- FAIL: TestUpsertInstancesPreservesOccupiedHistory/shrink (0.02s)
        cluster_node_test.go:777: shrink erased ledger: rows=1 busy=0 err=<nil>
--- FAIL: TestGrantRechecksNodeAuthorization (0.10s)
    --- FAIL: TestGrantRechecksNodeAuthorization/authorized (0.02s)
        task_test.go:1305: revoked grant must fail without authorization: lease="lse_BCHh7iubGyVWPWXKYy" attempt=1 until=1794259954 err=<nil>
    --- FAIL: TestGrantRechecksNodeAuthorization/declared (0.02s)
        task_test.go:1305: revoked grant must fail without authorization: lease="lse_BCHh7j1QaFjsxVlfdD" attempt=1 until=1794259954 err=<nil>
--- FAIL: TestStaleGrantAndRejectPreserveNewAssignment (0.08s)
    --- FAIL: TestStaleGrantAndRejectPreserveNewAssignment/grant-old (0.02s)
        task_test.go:1353: stale request changed newer assignment: before=&{ID:tsk_BCHh7jSJOuaRrfCegF NamespaceID:ns_BCHh7jRxuDHr6Fjt7h TraceID:trace-cas ParentTaskID:<nil> CapabilityID:cap_1794259894_38997019_ADcGcYg Capability:idem Version:1 Digest:sha256:3899701901c8abdbb3a4560e70deada9728ded6a39988dd76b5f6b129ecc9976 Params:[123 125] Phase:pending NodeID:0xc000001210 NodeName: ConfigRevision:0 Attempt:0 MaxAttempts:3 AssignmentID:asg-new ProposedAttempt:2 AssignmentExpiresAt:0xc0001950e0 Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID: AttemptTokenHash: WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794259894 StartedAt:<nil> FinishedAt:<nil>} after=&{ID:tsk_BCHh7jSJOuaRrfCegF NamespaceID:ns_BCHh7jRxuDHr6Fjt7h TraceID:trace-cas ParentTaskID:<nil> CapabilityID:cap_1794259894_38997019_ADcGcYg Capability:idem Version:1 Digest:sha256:3899701901c8abdbb3a4560e70deada9728ded6a39988dd76b5f6b129ecc9976 Params:[123 125] Phase:pending NodeID:<nil> NodeName: ConfigRevision:0 Attempt:0 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID: AttemptTokenHash: WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794259894 StartedAt:<nil> FinishedAt:<nil>} err=<nil>
    --- FAIL: TestStaleGrantAndRejectPreserveNewAssignment/grant-empty (0.02s)
        task_test.go:1353: stale request changed newer assignment: before=&{ID:tsk_BCHh7jYbuaUofxyMSn NamespaceID:ns_BCHh7jY9N3rlUNjJyi TraceID:trace-cas ParentTaskID:<nil> CapabilityID:cap_1794259894_38997019_9ECboc4 Capability:idem Version:1 Digest:sha256:3899701901c8abdbb3a4560e70deada9728ded6a39988dd76b5f6b129ecc9976 Params:[123 125] Phase:pending NodeID:0xc00013b0d0 NodeName: ConfigRevision:0 Attempt:0 MaxAttempts:3 AssignmentID:asg-new ProposedAttempt:2 AssignmentExpiresAt:0xc000250600 Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID: AttemptTokenHash: WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794259894 StartedAt:<nil> FinishedAt:<nil>} after=&{ID:tsk_BCHh7jYbuaUofxyMSn NamespaceID:ns_BCHh7jY9N3rlUNjJyi TraceID:trace-cas ParentTaskID:<nil> CapabilityID:cap_1794259894_38997019_9ECboc4 Capability:idem Version:1 Digest:sha256:3899701901c8abdbb3a4560e70deada9728ded6a39988dd76b5f6b129ecc9976 Params:[123 125] Phase:pending NodeID:<nil> NodeName: ConfigRevision:0 Attempt:0 MaxAttempts:3 AssignmentID: ProposedAttempt:0 AssignmentExpiresAt:<nil> Idempotent:true NamespaceVars:map[] NamespaceVarsRevision:0 LeaseID: LeaseUntil:<nil> LeaseInstanceID: AttemptTokenHash: WaitingReason: NextAttemptAt:<nil> CancelReason: CancelRequestedAt:<nil> Reported:false Summary: Artifacts:[] Error: CreatedAt:1794259894 StartedAt:<nil> FinishedAt:<nil>} err=<nil>
    --- FAIL: TestStaleGrantAndRejectPreserveNewAssignment/reject-empty (0.02s)
        task_test.go:1349: stale/empty identity must fail closed: <nil>
--- FAIL: TestExpiredAssignmentCannotRefreshDeadline (0.04s)
    --- FAIL: TestExpiredAssignmentCannotRefreshDeadline/expiry+0 (0.02s)
        task_test.go:1380: expired assignment must not be refreshed
    --- FAIL: TestExpiredAssignmentCannotRefreshDeadline/expiry+1 (0.02s)
        task_test.go:1380: expired assignment must not be refreshed
--- FAIL: TestRenewLeaseCannotReviveExpiredAuthorization (0.05s)
    --- FAIL: TestRenewLeaseCannotReviveExpiredAuthorization/expiry+0 (0.02s)
        task_test.go:1411: expired renewal must reject: until=1794260014 err=<nil>
    --- FAIL: TestRenewLeaseCannotReviveExpiredAuthorization/expiry+1 (0.02s)
        task_test.go:1411: expired renewal must reject: until=1794260015 err=<nil>
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.391s
FAIL
EXIT=1
```

### 同组绿测原始输出
日志：`/tmp/anotify-core-recovery-s3-green.log`

```text
$ timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -count=1 -run "Test(GrantRechecksNodeAuthorization|StaleGrantAndRejectPreserveNewAssignment|ExpiredAssignmentCannotRefreshDeadline|RenewLeaseCannotReviveExpiredAuthorization|UpsertInstancesPreservesOccupiedHistory|HistoricalReapRequiresExactBinding|TaskAttemptJSONShape|RetentionProtectsOldUnreapedAttemptAfterCurrentReaped)$"
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.389s
EXIT=0
```

相关回归首次 `EXIT=1`：`TestTaskDrainingNotLost` 原本要求 `ErrInvalidTransition`，新检查顺序返回了 `ErrNoAssignment`。已恢复 Node 状态检查优先级，**不改断言**，同一相关回归集第二次 `EXIT=0`。完整日志：
- `/tmp/anotify-core-recovery-s3-relevant.log`
- `/tmp/anotify-core-recovery-s3-relevant-green.log`

### 最终固定机械验证原始输出
```text
$ timeout 60 test -z "$(gofmt -l internal/store)"
EXIT=0
```
```text
$ timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store/
EXIT=0
```
```text
$ timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test/
EXIT=0
```
```text
$ timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	7.755s
EXIT=0
```

完整日志：
- `/tmp/anotify-core-recovery-s3-format-final.log`
- `/tmp/anotify-core-recovery-s3-build-final.log`
- `/tmp/anotify-core-recovery-s3-vet-final.log`
- `/tmp/anotify-core-recovery-s3-full.log`

### 遗留限制与交接
- **机器快照确定性并发判别未新增**：公开 DB/SQLite 可以控制整笔事务顺序，但未构造出无需 hook/sleep、能确定卡在机器读取与 grant 写入之间的观测点。既有顺序快照测试仍通过；新增 reject 两写者竞态测试不是 snapshot 原子性证明。
- pi-lens 本会话不可用；使用完整源码读取及固定 build/vet/test。所有实际命令均在 60 秒以内，无未收集长命令。
- 逐文件重读完整 diff，`git diff --check` 通过。本提交只含授权 7 文件；没有 push/MR/merge。
- 父协调文档提交期间进入分支；当前另见未跟踪 `G1-6C2.md`，未创建、编辑或暂存它。
- **仅本卡机械门禁 PASS，不代表 T1 DONE。** 旧 inpackage、原始 all-Go/contract/e2e 门禁未在本卡执行；固定 commit fresh review 仍由 centurion 完成。

{"ok":true,"reason":"G1-6S3 only: 7 authorized files committed at bd69177b467cfe08458a632fc60300def6285e3b; red-to-green reproduced, gofmt/store build/external v4 vet/full test exit 0; deterministic snapshot concurrency limitation reported; no push/MR/merge, fresh review pending"}
