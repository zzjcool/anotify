## Implementation gate：PASS（仅 C3aR2，非 T1 DONE）

已提交：`9e18f59bfd0f405bb66d3692852cb53a88e71e78`  
分支：`pi-subagent/v4-core-runtime-next-59fc6b04`

**唯一最终改动文件**
- `internal/store/v4test/execution_auth_test.go`

**强化三个既有测试，无新增测试函数**
- `TestAckCancelAuthorizedHistoricalBinding`
- `TestAckCancelHistoricalReapIsByteIdempotent`
- `TestAckCancelPreservesHistoricalFailureFacts`

首历史 ACK 断言 `ReapedAt != nil && *ReapedAt == in.Now`；保留旧失败/结果事实。不同 Now/reason 的重复 ACK 比较完整 task、old/new attempt 及全部 affected slots 的 ledger。首 ACK 仅允许清旧槽，新槽完整保持。

### 实际变异红绿

共同命令：
```bash
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
go test ./internal/store/v4test/ \
-run '^TestAckCancel(AuthorizedHistoricalBinding|HistoricalReapIsByteIdempotent|PreservesHistoricalFailureFacts)$' -count=1
```

| 单故障 | 红测 exit | 精确恢复后绿测 exit |
|---|---:|---:|
| 首次 `reaped_at` 写有效 `Now+1` | 1，三个测试失败 | 0 |
| 已回收的重复 ACK 改 `tasks.summary` | 1，三个测试失败 | 0 |

首次试验发现 helper 默认 `Now=0`，该输出**不计判别证据**；显式赋 Now、确认基线通过后，重新完成有效红绿。

两文件 `cmp` 恢复 exit=0，恢复 SHA256：
```text
execution_auth.go  8472a3d6aeb9ceeb6f8e1fb1131eca3e43fc167b7d3c6e66ae401970e227bc6c
task_attempt.go    e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637
```
与 `4925355` 产品源码最终 diff：空，exit=0。

### 固定四条检查

日志均位于 `/tmp/anotify-core-next-c3ar2-`，包含 pwd、HEAD、command、exit；均使用 timeout 60。

| 检查 | exit | 日志后缀 |
|---|---:|---|
| `test -z "$(gofmt -l internal/store internal/artifact)"` | 0 | `check1-gofmt.log` |
| `go build ./internal/store/ ./internal/artifact/` | 0 | `check2-build.log` |
| `go vet ./internal/store/v4test/ ./internal/artifact/` | 0 | `check3-vet.log` |
| 全量 scoped tests，`-count=1` | 0 | `check4-test.log` |

最终全量输出原文：
```text
command=GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	24.950s
ok  	github.com/zzjcool/anotify/internal/artifact	2.404s
ok  	github.com/zzjcool/anotify/internal/protocol	0.044s
exit=0
```

完整报告（含全部红绿原始输出、SHA、恢复及四条验证）：  
`/tmp/anotify-core-next-c3ar2-report.md`

未 push/merge/MR；未动其他模块。工作期间出现的未跟踪 `NEXT-C3b.md` 未读取、修改或提交。**C3b、G1 whitebox、G2–4 仍 pending**；未发现产品缺陷。

DONE NEXT-C3aR2（仅本次断言返修）

{"ok":true,"reason":"C3aR2：仅授权测试文件已提交 9e18f59；两项有效变异红绿完成，产品 byte-exact 恢复，固定四检查 exit=0；C3b/whitebox/G2–4 pending，非 T1 DONE。"}
