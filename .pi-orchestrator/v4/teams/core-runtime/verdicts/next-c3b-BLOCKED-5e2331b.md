**C3b implementation gate：BLOCKED；固定四条 stage 验证均通过。不是 G1/T1 DONE。**

真实接口缺口：
- `internal/protocol/exec_frames.go:201–208` 已有 `ExitCode / StartedAt / FinishedAt`。
- `internal/store/task.go:810–816` 的冻结 `TaskResult` 无法接收这些事实。因此不能完整比较 wire result，也不能同时保留原执行 finishedAt 与使用当前时钟验租约。未擅自扩充接口。

已提交 typed manifest、上传登记/并发配额、当前绑定 duplicate ACK、引用 GC、namespace force 守卫。**18 个授权文件、24 个新增测试**，明细和所有实际 Go 签名见报告。

HEAD：`5e2331b8127f1c0ee748f3df2b102b1b2c47f659`

最终 stage exits：`gofmt=0 / build=0 / vet=0 / full=0`

```text
COMMAND: timeout 60 go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	34.693s
ok  	github.com/zzjcool/anotify/internal/artifact	2.403s
ok  	github.com/zzjcool/anotify/internal/protocol	0.047s
EXIT: 0
```

交付证据：
- 报告：`/tmp/anotify-core-next-c3b-report.md`
- 实际导出：`/tmp/anotify-core-next-c3b-actual-go.txt`
- 红绿、mutation、首轮 full 失败及最终四条日志：`/tmp/anotify-core-next-c3b-*.log`

未 push/merge/MR，未提交外部父进度文档变更。建议父明确批准领域 result 携带现有 exit/time 事实，再续同 writer；G1TEST/G2–4 和原全仓门禁保持 pending。

{"ok":false,"reason":"C3b blocked：冻结 TaskResult 缺既有 wire ExitCode/StartedAt/FinishedAt，无法完整事实比较及保留原 finishedAt；需父批准接口扩充。HEAD 5e2331b，四条 stage 实际 exits 全为0，报告 /tmp/anotify-core-next-c3b-report.md"}
