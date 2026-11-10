# V4-CORE-RUNTIME-NEXT/C3b 结果事实实施报告

## 做了什么 / implementation gate

```json
{
  "task": "V4-CORE-RUNTIME-NEXT/C3b exact parent-approved result facts",
  "head": "b46fd2c4d8bd233a056d5313ee40f052ac004d3c",
  "base": "cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8",
  "preservedProduction": "5e2331b",
  "implementationGate": {
    "storeC3bClosureImplemented": true,
    "newWireOrToken": false,
    "selfFixed4": "PASS",
    "extensionVerified": false,
    "reviewAndWhiteboxG2G3G4": "PENDING",
    "finalGoContractQuick": "UNCHANGED/PENDING"
  },
  "changedFiles": [
    "internal/store/task.go",
    "internal/store/task_attempt.go",
    "internal/store/execution_auth.go",
    "internal/store/schema.sql",
    "internal/store/retention.go",
    "internal/store/v4test/result_facts_test.go",
    "internal/store/v4test/schema_baseline_test.go",
    "internal/store/v4test/retention_test.go",
    "internal/store/v4test/execution_wire_test.go",
    "internal/store/v4test/result_current_binding_test.go",
    "internal/store/v4test/task_attempt_test.go"
  ],
  "newTests": [
    "TestC3bCompleteResultFullJSONNanoseconds",
    "TestC3bCompleteResultFullFactsDiscrimination",
    "TestC3bCompleteResultReopenNullableExitUTC",
    "TestC3bCompleteResultMonotonicEquivalentDuplicate",
    "TestC3bCompleteResultCurrentClockAfterSlowUpload",
    "TestC3bCompleteResultOldFinishCannotReviveExpiredLease",
    "TestC3bCompleteResultBasicTimeOrderZeroWrite",
    "TestC3bCompleteResultOldFutureClocksTrustedRetention",
    "TestC3bCompleteResultTrustedRetentionActiveUnreapedProtected",
    "TestC3bCompleteResultMissingFactsNeverInferACK",
    "TestC3bResultFactsSchemaRejectsMissingColumnsZeroWrites"
  ],
  "redGreen": {
    "baselineRedExit": 1,
    "baselineGreenExit": 0,
    "baselineHead": "cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8",
    "mutation": "only lease expiry compares result.FinishedAt.Unix() instead of server now",
    "mutationRedExit": 1,
    "restoredGreenExit": 0,
    "exactRestoreCmpExit": 0,
    "executionAuthSha256": "797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f",
    "faultSha256": "f085d348660b9a3840fc28e166c4fc739cce83d4f07727caf1eee1ae8f4ef31f"
  },
  "selfVerify": [
    {
      "command": "sh -c 'unformatted=$(gofmt -l internal/store internal/artifact) && printf \"%s\" \"$unformatted\" && test -z \"$unformatted\"'",
      "exit": 0,
      "log": "/tmp/anotify-core-next-c3b-facts-final-verify1-gofmt.log"
    },
    {
      "command": "go build ./internal/store ./internal/artifact",
      "exit": 0,
      "log": "/tmp/anotify-core-next-c3b-facts-final-verify2-build.log"
    },
    {
      "command": "go vet ./internal/store/v4test ./internal/artifact",
      "exit": 0,
      "log": "/tmp/anotify-core-next-c3b-facts-final-verify3-vet.log"
    },
    {
      "command": "go test ./internal/store/v4test ./internal/artifact ./internal/protocol -count=1",
      "exit": 0,
      "log": "/tmp/anotify-core-next-c3b-facts-final-verify4-full.log"
    }
  ],
  "fullFirstRun": {
    "exit": 1,
    "reason": "approved exitCode projection absent from strict TestTaskAttemptJSONShape expected keys; added key and null assertion, no weakening",
    "log": "/tmp/anotify-core-next-c3b-facts-verify4-full.log",
    "retries": 1
  },
  "pushMergeMR": "NOT DONE per task",
  "productBugs": [],
  "report": "/tmp/anotify-core-next-c3b-facts-report.md"
}
```

## Go 字段、方法与时间投影

- store.TaskResult: Phase/Reason/Summary/Error string、Artifacts []protocol.TaskArtifact 不变；新增 ExitCode *int (json:"exitCode")、StartedAt/FinishedAt time.Time (json:"startedAt"/"finishedAt")，无 omitempty，nil exit 与 0 区别。TaskResult.MarshalJSON() 将时间 UTC 化并剥 monotonic，不截断纳秒；空 artifacts 为 []。
- CompleteTaskAuthorized(ctx context.Context, auth NodeAuthorization, taskID string, attempt int, leaseID string, now int64, result TaskResult) error 签名不变；CompleteTaskClassifiedAuthorized 同参数、返回 (TaskResultDisposition, error) 不变。
- 现 safe-classified 事务入口保留：当前 Node credential/ns/task/attempt/lease/cancel/leaseexpiry 用独立 serverNow；Node start/finish 仅两者都有时做 start<=finish 校验。新完整 JSON 写入任务与 attempt；同完整事实且当前绑定及 retained typed refs 齐全才 duplicate，读事务 rollback 0 DML。任一 exit/time/其他结果事实变更冲突 0 写。
- Task 与 TaskAttempt 新增 ExitCode *int JSON 投影、隐藏 ResultJSON json.RawMessage 及 ResultAcceptedAt *int64 (json:"-")。Task 的 result_json 新增；attempt 的既有 result_json 扩为完整事实。两表 result_accepted_at 是内部首接收秒时钟，首次 COALESCE 写入、duplicate 不重写，非 Node finish。
- 既有 StartedAt/FinishedAt *int64 公共投影/索引继续秒精度，但保存实际 Node 时间（zero -> NULL），不写上传收完 now。完整纳秒保在 result_json。finishAttemptOn/reapAttemptOn 对已有 result 不用 now 补造缺失进程 finish；可信 reaped_at/槽释放仍用 serverNow。
- DeleteExpiredTasks 按可信首接收时间；未上报服务器终态才 fallback 到服务器 finished_at。reported 行若可信接收事实缺失不 fallback 到 Node 时间。活动 ticket/槽/所有 unreaped 历史保护不变；已有 artifact published_at/expiry 是服务器可信时钟，7-day bytes GC 未改。
- schema.sql v4 baseline 增补这些内部列。migrate.go 原有 schema-derived 严格 layout validator 自动读嵌入 baseline，无需改 hooks；四种缺列测试证明拒绝且数据库原样不写。无旧库迁移兼容/自删数据。
- 5e2331b typed registry/三 quotas/refGC/namespace force/M3/current-binding 实现原样保留。未改 artifact_manifest.go、protocol、manager、namespace/capability/node/emission/artifact/API。

## 测试覆盖

11 个新增测试函数列在结构化 gate。覆盖完整 JSON 重开/nanos/nullable exit、同瞬 offset 与 monotonic、全部结果事实 discrimination、whole task/attempt/manifest/upload/object/slot ledger 对比、首接收时间不变、before-spawn 无 start/exit、真实续租后 +75s 接收保存原 finish、很旧 Node finish 不能绕已过期服务器 lease、30-day facts/7-day bytes、活动/未reaped/运行中保护、证据缺失不推断 ACK、四类 schema 缺列拒绝 0 写。

既有测试只同步授权投影/真实结果 caller/可信 TTL 历史夹具。TestTaskAttemptJSONShape 仍严格精确键集合并增加 exitCode null 断言；空 artifacts 仍严格 []，不再把合法 nullable exitCode 的 null 误报为空数组错误。没有 skip/减少用例/放松拒绝断言。

## 红绿与 SHA 恢复（以下原样日志）

### baseline-red

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run "^TestC3bCompleteResultFull(JSONNanoseconds|FactsDiscrimination)$" -count=1
--- FAIL: TestC3bCompleteResultFullJSONNanoseconds (0.02s)
    result_facts_test.go:53: complete result discarded exit/time facts: {"phase":"succeeded","reason":"","summary":"actual process","artifacts":[{"path":"full-facts","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"error":""}
--- FAIL: TestC3bCompleteResultFullFactsDiscrimination (0.02s)
    --- FAIL: TestC3bCompleteResultFullFactsDiscrimination/nilExit (0.00s)
        result_facts_test.go:89: different complete nilExit facts ACKed: disposition=duplicate err=<nil>
    --- FAIL: TestC3bCompleteResultFullFactsDiscrimination/differentExit (0.00s)
        result_facts_test.go:89: different complete differentExit facts ACKed: disposition=duplicate err=<nil>
    --- FAIL: TestC3bCompleteResultFullFactsDiscrimination/startedNanos (0.00s)
        result_facts_test.go:89: different complete startedNanos facts ACKed: disposition=duplicate err=<nil>
    --- FAIL: TestC3bCompleteResultFullFactsDiscrimination/finishedNanos (0.00s)
        result_facts_test.go:89: different complete finishedNanos facts ACKed: disposition=duplicate err=<nil>
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.053s
FAIL
EXIT: 1
```

### baseline-green

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run "^TestC3bCompleteResultFull(JSONNanoseconds|FactsDiscrimination)$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.080s
EXIT: 0
```

### mutation-baseline-sha

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: sha256sum internal/store/execution_auth.go; cp internal/store/execution_auth.go /tmp/anotify-core-next-c3b-facts-execution_auth.baseline.go
797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f  internal/store/execution_auth.go
EXIT: 0
```

### mutation-red

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
f085d348660b9a3840fc28e166c4fc739cce83d4f07727caf1eee1ae8f4ef31f  internal/store/execution_auth.go
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run "^TestC3bCompleteResultOldFinishCannotReviveExpiredLease$" -count=1
--- FAIL: TestC3bCompleteResultOldFinishCannotReviveExpiredLease (0.04s)
    --- FAIL: TestC3bCompleteResultOldFinishCannotReviveExpiredLease/equal (0.02s)
        result_facts_test.go:307: old Node finish bypassed CURRENT expired server lease: <nil>
    --- FAIL: TestC3bCompleteResultOldFinishCannotReviveExpiredLease/past (0.02s)
        result_facts_test.go:307: old Node finish bypassed CURRENT expired server lease: <nil>
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.052s
FAIL
EXIT: 1
```

### mutation-restored-green

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: cp /tmp/anotify-core-next-c3b-facts-execution_auth.baseline.go internal/store/execution_auth.go; cmp -s /tmp/anotify-core-next-c3b-facts-execution_auth.baseline.go internal/store/execution_auth.go; sha256sum both; focused green
RESTORE_EXIT: 0
797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f  /tmp/anotify-core-next-c3b-facts-execution_auth.baseline.go
797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f  internal/store/execution_auth.go
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run "^TestC3bCompleteResultOldFinishCannotReviveExpiredLease$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.057s
EXIT: 0
```

## 每轮轻量验证（原样）

### round1-build

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store ./internal/artifact
EXIT: 0
```

### round1-vet

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test ./internal/artifact
EXIT: 0
```

### round2-focused

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run 'TestC3bCompleteResult|TestC3bResultFactsSchema|TestCompleteTaskActualResultWireAuthority|TestC3bTypedEmptyArrays|TestRetention' -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	1.042s
EXIT: 0
```

### round2-build

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store ./internal/artifact
EXIT: 0
```

### round2-vet

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
cc26a2e820c08c18c09bf43e80c97ebc40a8ebc8
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test ./internal/artifact
EXIT: 0
```

### round3-projection-green

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b86457525d4e5145856478cd8b91a0f59b986148
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test -run "^TestTaskAttemptJSONShape$" -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.028s
EXIT: 0
```

### round3-build

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b86457525d4e5145856478cd8b91a0f59b986148
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store ./internal/artifact
EXIT: 0
```

### round3-vet

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b86457525d4e5145856478cd8b91a0f59b986148
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test ./internal/artifact
EXIT: 0
```

## 首次 full 与精确修订（原样）

首次 full 失败仅为新增 exitCode 的精确公共键断言未同步，修订授权 test projection 后重试 1 次；不是产品 bug，也不弱化断言。

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b86457525d4e5145856478cd8b91a0f59b986148
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test ./internal/artifact ./internal/protocol -count=1
2026/11/10 03:48:53 INFO refreshed sqlite statistics event=lease.stats.refreshed trigger=open table=tasks prev_rows=0 cur_rows=1 statement="ANALYZE \"tasks\""
2026/11/10 03:49:18 INFO refreshed sqlite statistics event=lease.stats.refreshed trigger=open table=tasks prev_rows=0 cur_rows=1 statement="ANALYZE \"tasks\""
2026/11/10 03:49:18 INFO refreshed sqlite statistics event=lease.stats.refreshed trigger=open table=tasks prev_rows=0 cur_rows=1 statement="ANALYZE \"tasks\""
--- FAIL: TestTaskAttemptJSONShape (0.02s)
    task_attempt_test.go:850: attempt JSON keys differ from exact public map: {"artifacts":[],"exitCode":null,"taskId":"tsk_BCIEq4kuUobWkj439v","attempt":1,"nodeId":"nd_BCIEq4kErYAgl1wivR","nodeName":"node-a","slot":0,"leaseId":"lse_BCIEq4lEoJtbj4abvE","configRevision":2,"configHash":"sha256:0e25b9125aec61252bd515d59c26f4fa002440b20b821b24ef3bf4f4799b3c99","machineVarSources":{"SECRET_TOKEN":"node"},"namespaceVarsRevision":0,"phase":"running","error":"","startedAt":1794282561,"finishedAt":null,"reapedAt":null,"reclaimReason":""}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	29.495s
ok  	github.com/zzjcool/anotify/internal/artifact	2.457s
ok  	github.com/zzjcool/anotify/internal/protocol	0.042s
FAIL
EXIT: 1
```

## 最终固定四项 self 验证输出（原样，非冒称 extension verified）

### verify1-gofmt

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b46fd2c4d8bd233a056d5313ee40f052ac004d3c
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto sh -c 'unformatted=$(gofmt -l internal/store internal/artifact) && printf "%s" "$unformatted" && test -z "$unformatted"'
EXIT: 0
```

### verify2-build

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b46fd2c4d8bd233a056d5313ee40f052ac004d3c
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store ./internal/artifact
EXIT: 0
```

### verify3-vet

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b46fd2c4d8bd233a056d5313ee40f052ac004d3c
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test ./internal/artifact
EXIT: 0
```

### verify4-full

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b46fd2c4d8bd233a056d5313ee40f052ac004d3c
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test ./internal/artifact ./internal/protocol -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	29.931s
ok  	github.com/zzjcool/anotify/internal/artifact	2.386s
ok  	github.com/zzjcool/anotify/internal/protocol	0.050s
EXIT: 0
```

## Diff 与未决

- 逐文件完整检查本卡 cc26a2e...HEAD 的授权 diff（含新测试），无 debug/print、注释掉旧逻辑或越界变更；git diff --check PASS，工作区 clean。
- 没有 push/MR/merge，符合本卡 NoPushMergeMR。报告与证据只写 /tmp/anotify-core-next-c3b-facts-*。
- 当前 C3b **store 实现小闭包已经实现**，固定四项 self 全绿。父会话 extension 自动核验、fresh review、白盒 G2/G3/G4 及原最终 Go-contract-quick **仍 pending，未减门禁、未宣称整个 runtime 验收完成**。
- 没有新增需扩大路径的阻塞/产品 bug；manager/协议/API 等只读模块的集成属于后续父会话 gates。
- 本会话 tool roster 未提供 pi-lens / async_run，未改 settings/registry/harness 获工具；全部已知短命令 timeout60，full 约30s真实完成。Go build/vet 为真实机械诊断，不冒称 lens_diagnostics。
