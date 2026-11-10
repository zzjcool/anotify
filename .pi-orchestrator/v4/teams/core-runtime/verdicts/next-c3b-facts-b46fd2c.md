已完成 C3b **store 结果事实闭包**，保留 5e2331b 成果，没有改协议、manager 或其他团队源码。

### Implementation gate

- **HEAD**：`b46fd2c4d8bd233a056d5313ee40f052ac004d3c`
- **提交**：`fix(store): 保存完整执行结果与独立接收时钟`
- **实现闭包**：已完成
- **固定四项 self 验证**：全部 exit 0
- **extension / fresh review / 白盒 G2G3G4 / 原最终 Go-contract-quick**：pending，未减门禁、未冒称完成
- **Push / MR / Merge**：未执行，符合任务要求

### 实际改动文件（11）

源码：
- `internal/store/task.go`
- `internal/store/task_attempt.go`
- `internal/store/execution_auth.go`
- `internal/store/schema.sql`
- `internal/store/retention.go`

测试：
- `internal/store/v4test/result_facts_test.go`
- `internal/store/v4test/schema_baseline_test.go`
- `internal/store/v4test/retention_test.go`
- `internal/store/v4test/execution_wire_test.go`
- `internal/store/v4test/result_current_binding_test.go`
- `internal/store/v4test/task_attempt_test.go`

### 字段与时钟

`TaskResult` 新增 `ExitCode *int`、`StartedAt/FinishedAt time.Time`，camelCase JSON。完整 JSON 保存纳秒；UTC 规范化剥离 monotonic，nil exit 不等于 0。Task/attempt 秒列保存实际 Node 起止投影，zero start 不伪造进程开始。

新增隐藏 `result_accepted_at` 保存首接收服务器时钟，用于可信 TTL；duplicate 零写且不覆盖它。凭证、命名空间、当前 binding、lease expiry 等仍独立用 serverNow。

### 新增测试（11）

`TestC3bCompleteResult` 前缀：
- `FullJSONNanoseconds`
- `FullFactsDiscrimination`
- `ReopenNullableExitUTC`
- `MonotonicEquivalentDuplicate`
- `CurrentClockAfterSlowUpload`
- `OldFinishCannotReviveExpiredLease`
- `BasicTimeOrderZeroWrite`
- `OldFutureClocksTrustedRetention`
- `TrustedRetentionActiveUnreapedProtected`
- `MissingFactsNeverInferACK`

另新增：
- `TestC3bResultFactsSchemaRejectsMissingColumnsZeroWrites`

### 红绿证明

- 基线 `cc26a2e`：功能红，exit/time 丢失，nil/0、exit 变化及 start/finish 1ns 差异错误 ACK；修复后同测试绿。
- 单故障：仅把租约过期比较改用 `result.FinishedAt.Unix()`，expired equal/past 两项功能红。
- 恢复 `cmp` exit 0，同测试绿。恢复前后源码 SHA256 均为：
  `797477390ec9a9d8dc0aab3fe4f9ae7d228ebd52fc4621d614868e953dc7b75f`
- 首次 full 仅发现精确公共键测试缺新增 `exitCode`；补键及 null 断言后重试一次通过，无弱化或 skip。

### 最终固定四项

gofmt 空输出、store/artifact build、v4test/artifact vet：均 **exit 0**。完整测试原样输出：

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
b46fd2c4d8bd233a056d5313ee40f052ac004d3c
CMD: timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test ./internal/artifact ./internal/protocol -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	29.931s
ok  	github.com/zzjcool/anotify/internal/artifact	2.386s
ok  	github.com/zzjcool/anotify/internal/protocol	0.050s
EXIT: 0
```

完整字段/方法、逐项原始红绿与四项日志：
- `/tmp/anotify-core-next-c3b-facts-report.md`
- `/tmp/anotify-core-next-c3b-facts-implementation-gate.json`

工作区 clean，无新增产品 bug 或越界阻塞。

DONE V4-CORE-RUNTIME-NEXT/C3b

{"ok":true,"reason":"C3b store结果事实闭包完成；11 files、11新增测试，功能红绿及SHA原样恢复，固定4项self真实exit0；HEAD b46fd2c；extension/review/白盒G2G3G4与原Go-contract-quick pending；按任务未push/MR/merge"}
