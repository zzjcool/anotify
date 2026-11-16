> 总任务：https://github.com/zzjcool/anotify/issues/330
> 任务标识：V4_10；发现于 #338 的 `go test ./... -count=1` 门禁（不是 flaky 免责：可稳定复现）。
> 一个工作树一个源码 writer；base = `origin/v4/integration` @ 66d926a。

# 现象

`go test ./... -count=1` 在满负载并行包时失败（`cmd/anode/internal/runner`，15s 超时）：

```
--- FAIL: TestOldLeaseACKDoesNotExtendNewAttempt (15.02s)
    runner: task=t1 attempt=2 复制 stdout.log 日志失败：read |0: file already closed
    runner: task=t1 attempt=2 复制 stderr.log 日志失败：read |0: file already closed
    runner: task=t1 attempt=2 未确认进程组回收，保留物理槽位
    slots_test.go:236: 推进时钟等待帧超时；已收帧：[... *protocol.CancelAckFrame]
```

- 单包/单独跑 3 次都过（0.05–3.2s）；**加 CPU 负载 + `-count=60` 必现**：
  `cmd/anode/internal/runner` 的 `TestOldLeaseACKDoesNotExtendNewAttempt`。
- 复现命令（在 worktree 内）：
  ```bash
  cd /root/code/wt-338
  for i in $(seq 1 12); do (timeout 60 sh -c 'while :; do :; done' &) ; done
  go test ./cmd/anode/internal/runner -run TestOldLeaseACKDoesNotExtendNewAttempt -count=60
  ```

# 根因（已确认，不要重新侦察）

1. `Attempt.waitProcess()`（`cmd/anode/internal/runner/attempt.go:570-612`）在 leader 退出后：
   - 用**注入时钟**等 `copiesDone`（TermGrace）；到点 → SIGKILL 进程组 + 关读端 → `copyLog` 读到 closed file，`noteCopyErr` 记成真实复制错误（日志出现“复制失败”）。
   - 再 `groupAlive` → TERM → `waitGone(pgid, TermGrace)` → SIGKILL → `waitGone(...)`，最后 `a.groupReaped = pgid<=0 || !groupAlive(pgid)`。
2. `waitGone`（`attempt.go:876-899`）的**截止时间由注入时钟驱动**，但进程组消失是内核/真实时间事实。测试用 `waitFrameAdvancing` 每次把假时钟推进 5s、只 sleep 2ms real（`runner_test.go:335-347`）。SIGTERM/SIGKILL 后 2ms 内 `kill(-pgid,0)` 仍可能活着（受调度负载影响）→ `waitGone` 返回 false → `groupReaped=false`。
3. `HandleCancel`（`runner.go:641-656`）在 `<-att.done` 后只发一次 `CancelAck{ProcessGroupReaped: att.groupReapedNow()}`；`finishAttemptLocked`（`runner.go:939-955`）遇 false 只“保留物理槽位”——**之后没有任何复核/补发**。若进程组在 1ms 后确实消失，中枢永远认为槽位被占（物理槽位泄漏到重启），且测试永远等不到 reaped=true 的 ack。

# 要求

1. **SIGKILL 后的回收确认改成真实时间有界轮询**（SIGKILL 不可捕获，必然死亡；这不是放宽 grace，而是确认内核事实）：
   - TERM 宽限仍由注入时钟驱动（测试语义不变）；
   - SIGKILL 之后用一个小的**真实时钟**有界确认（例如 ≤2s，5–20ms 轮询，`kill(-pgid,0)==ESRCH` 即 reaped），而不是再吃一次假时钟 timer；
   - `groupReaped` 必须反映这个确认结果。
2. **不允许 slot 永久假占**：若最终仍未确认回收，保留现有“保留槽位”语义，但必须有后台复核（有界）→ 一旦确认 reaped，补发一个 `CancelAck{ProcessGroupReaped:true, 同 attempt/lease/slot}` 并按现有规则释放槽位/写历史；不要重新执行任务、不要改 attempt。
3. **修正误报的日志复制错误**：读端是回收路径自己关闭的（intentional close），`copyLog` 不应把它记成 `copyErr` / 让成功任务翻成 failed。用显式状态（如 `pipesClosed` flag under mutex + `errors.Is(err, os.ErrClosed)`）区分“自己关的”与真错误；保留真实复制错误的失败语义。
4. 禁止改测试断言/放宽 `waitFrameAdvancing` 的 15s real 上限来迁就；`TestOldLeaseACKDoesNotExtendNewAttempt` 必须原样通过（可加新回归，不删旧断言）。

# 验收

- [ ] 负载复现命令（12 个 CPU burner + `-count=60`）修复后全绿；同命令在 `66d926a` 上是 RED（先证 RED 再修）。
- [ ] `go test ./cmd/anode/internal/runner -count=1` 连跑 5 次全绿；`go test ./cmd/anode/... -count=1` 全绿。
- [ ] 新增具名回归：① SIGKILL 后真实收敛确认（不依赖假时钟推进）；② intentional close 不触发 `copyErr`/不把成功任务置 failed；③ 未确认回收时后台复核补发 reaped=true 的 ack（或有界失败后明确记录，不静默）。
- [ ] `gofmt -l cmd/anode` 空、`go vet ./cmd/anode/...`、`go build ./...` 真实 exit 0。

# 不做

- 不改 `internal/protocol` 帧形状/字段；不改中枢 store/schema；不动前端；不加全局 sleep。
- 不通过放宽测试超时/删除断言/`t.Skip` 过关。
