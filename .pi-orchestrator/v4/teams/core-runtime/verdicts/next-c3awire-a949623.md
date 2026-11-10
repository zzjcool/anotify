## C3a implementationgate：PASS（非 T1 DONE）

已提交：
`a9496238e34a96e1d5ad0087a34e9f02d92f821d`
`fix(store): 对齐真实结果身份授权与总预算判别`

完整报告及原样红绿、四项验证输出：
`/tmp/anotify-core-next-c3awire-report.md`

### 改动文件
- `internal/store/execution_auth.go`
- `internal/store/v4test/{execution_wire,execution_auth,task,task_attempt,retention,stats,emit}_test.go`

仅 8 个授权文件；工作区干净，未 push/merge/MR，未派 agent。

### 实际 Go 签名
```go
func (d *DB) CompleteTaskAuthorized(
    ctx context.Context, auth NodeAuthorization,
    taskID string, attempt int, leaseID string,
    now int64, result TaskResult,
) error
```

移除了不存在的 plaintext result attemptToken 实参及比较。认证凭证、node/task、namespace、current attempt、lease、取消和到期校验仍在写事务内；Renew、Emit/token 校验及 M3 逻辑未放宽。

### 新增测试
- `TestCompleteTaskActualResultWireAuthority`
- `TestCompleteTaskActualResultRefusalsZeroFacts`：9 个身份/绑定/ns/取消/到期反例
- `TestGrantTotalShrinkCountsOldHigherSlotOnly`
- `TestConcurrentTotalShrinkGrantsExactlyOne`

原 perCapability 窄预算、own-exact、M3 whole-facts、D2 测试保留。

### 红绿与 SHA restore
- 真实 tokenless result 旧入口：**exit 1**；迁移后同名测试：**PASS**。
- 仅关闭 total enforcement：单例 B 被错误授予；并发 **success=2 / rejected=0**，两例均 **exit 1**。
- 原样恢复后两例 **PASS / exit 0**。
- 恢复后的 `execution_auth.go` SHA256：
  `e7235e70b4f9865d47dfd3ab0dab32aa99153933e0c34d6e991fe28ecb5da1d5`
- `node.go/task.go/task_attempt.go` SHA 检查全部 OK、最终 **0 diff**。

### final HEAD 四项实际验证
| 门禁 | exit |
|---|---:|
| gofmt store/artifact | 0 |
| go build store/artifact | 0 |
| go vet v4test/artifact | 0 |
| full v4test/artifact/protocol `-count=1` | 0 |

最终 full 输出：
```text
ok  	github.com/zzjcool/anotify/internal/store/v4test	24.723s
ok  	github.com/zzjcool/anotify/internal/artifact	2.379s
ok  	github.com/zzjcool/anotify/internal/protocol	0.045s
exit=0
```
各日志均记录正确 WT 的 pwd、HEAD、命令、exit。全部小于 60 秒，没有遗留长任务。

### actual duplicate ACK：尚无安全重发路径
`execution_auth.go:708–710` 对终态结果直接返回 `ErrInvalidTransition`，没有 same-auth、历史 precise binding、same-facts 的重发确认分类。当前保持 fail-closed，**不能把该错误统一转换为 ACK**；未添加 Get-precheck/无保护写入。报告列出此 Go 接口缺口。

### 完整 remaining
报告附有未改动的冻结卡全文，仍保留：
- **C3b**：typed manifest、上传登记/最终重鉴权、三限额、引用与 GC、7/30 天区分、ParamEnvName 单源。
- **白盒/原测试闭包**：逐名迁移、真实 DDL/ledger/marker 回滚、保留域回归。
- **G2**：真实协议 runtime、结果重发确认缺口、租约/取消/emit/source/deploy/log 链路。
- **G3**：HTTP/CAS/资源、实际 spool、保护域、五份 API 与 checker 对齐。
- **G4**：正式 Node 交接、工具/E2E，以及原全量 `go test ./...`、`make contract`、`make e2e-quick` 和父独立审查。未声称这些通过。

{"ok":true,"reason":"8 allowed files committed at a949623; C3a-WIRE fixed4 actual exits 0/0/0/0, discriminating total red-green and exact SHA restore verified; safe duplicate-result ACK interface gap reported; not T1 DONE"}
