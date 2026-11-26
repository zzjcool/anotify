> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 2026-11-26 #661 交付时的定格数字（真跑输出），复算命令见各节 -->

# 长任务租约韧性（TTL 可配 / 节点硬心跳 / abandoned 可恢复）

> 分支 `wt/leaseres` · 基线 `origin/main` `04c5e22e` · 契约面 `docs/contracts/v4/CONTRACT.md`
> 相关 issue：主案 #661（隧道抖动整单作废、以及「中枢认为连接还在但 lastSeenAt 冻结」）
> 依赖：#660（abandoned 同事务收敛两本账）

一句话：**租约时长从「硬编码 60s」变成「可配、默认 180s、60s 为下限」，节点读循环加了硬心跳
（等回执超时即断开重连），租约失联不再按 `idempotent` 一票否决重试、而是按 `maxAttempts` 预算
重排——同一次执行仍然只会跑一遍（attempt 栅栏 + 退避 ≥ TERM 宽限）。**

---

## 1. 现场事实（为什么必须动）

实测（dev，两台节点，经 Cloudflare 隧道）：

```
13:47:24 hub: 读取帧 failed to read frame header: EOF
13:48:02 runner: task=… 租约失联（最后一次 ACK + TTL 到点）→ TERM/KILL 进程组
          task 被判 abandoned（不吃 maxAttempts=3，不重试）
```

- 一跳 `implement` 真实耗时 10–37 分钟；`LeaseTTL=60s` 只够 3 个续租周期，撞上任意一次 ≥60s 的
  抖动就整单作废。`examples/playbooks/issue-fix/capabilities/implement.yaml` 写的是
  `idempotent: false`，而旧判据是「非幂等 ⇒ 失联一律 abandoned」，于是**声明的 `maxAttempts: 3`
  形同废纸**。
- 第二种形态（#661 正文）：中枢从未 `disconnected`、节点也不报错，但 `lastSeenAt` 冻结、派发
  不再发生，只有重启节点才恢复 —— 节点的读循环永久阻塞在隧道里，自己不知道。

## 2. TTL 取值依据：180s（下限 60s）

`LeaseTTL = 60s` 现在是**协议下限**，`DefaultLeaseTTL = 180s` 是中枢未配置时的默认值
（`internal/protocol/enums.go`），中枢用 `ANOTIFY_LEASE_TTL` 覆盖。

为什么是 180s：

1. **实测抖动时长**：本次事故里隧道从 EOF 到恢复的窗口是 38s（13:47:24 → 13:48:02 之间那次
   重连），同一现场更早的几次 502/TLS 握手重置落在 40–90s 区间；60s 的租约正好落在这个区间的
   中位数上 —— 观测到的「偶发」判死其实是对**常见**抖动的必然反应。
2. **余量按续租周期算**：中枢的判据是「最后一次续租 + TTL」，续租间隔 20s。60s = 3 个周期
   （丢 2 个就到期），180s = 9 个周期。
3. **代价是有界的**：唯一变慢的是「节点真的死掉 → 被摘下来」的时延（≤ TTL + 巡检间隔
   `lease.DefaultInterval`=30s）。这个方向宁可慢：任务被误杀是**不可恢复**的（非幂等副作用 +
   重跑成本），多等 2 分钟只是晚 2 分钟知道。

比例校验（`protocol.ValidateLeaseTTL(ttl, renew)`，中枢 `Config.Validate` 启动即调）：

| 判据 | 拒绝的例子 | 理由 |
| --- | --- | --- |
| `ttl > 0` | `0` / `-30s` / 写错（`3 minutes`） | 零/负租约等于「一 grant 就过期」 |
| `ttl >= LeaseTTL(60s)` | `30s`、`59s` | 一次隧道重建就够判死，属可避免的整单作废 |
| `ttl >= 3×renew` | `ttl=120s, renew=45s` | 续租间隔相对太长，丢一两个 ACK 就到期 |

**非法值启动即拒**（不像保留期那样回落默认）：缩小的租约不会报错，只会静默地把长任务判死，
而手滑写出的值往往正好比默认更小。

节点侧不再自算 TTL：`runner.leaseDeadline` 直接采用中枢在 `lease.grant` / `lease.ack` 里给的
`expiresAt`（时钟偏差过大时退回「now + 一个续租周期」，帧里没带才用配置兜底）。这样
**两侧用同一个数**——否则中枢按 180s 认、节点按 60s 杀，问题只是从「中枢误杀」变成「节点误杀」。

## 3. 两种断连形态的区别（为什么第 2 项是硬心跳而不是加长 TTL）

| 形态 | 观测 | 谁能发现 | 本 PR 的手段 |
| --- | --- | --- | --- |
| **A. 连接真的断了**（FIN/RST/超时） | `Read` 返回错误、`EOF`、ping 失败 | 节点自己（读错误）、中枢（detach） | 原本就有；退避重连 |
| **B. 静默丢包**（隧道把下行帧丢掉、连接不报错） | `Read` 永久阻塞、`exists` 的睡眠式保活也探不到（控制帧可能仍是通的） | **没人** | 节点侧硬心跳 + 中枢侧租约到期 |

关键点：**加长 TTL 只把 B 的窗口推后，不解决「节点不知道自己已经聋了」**——一个被静默切断的
节点会继续持有进程、继续假装在跑，这正是 #661 里「只有重启节点才恢复」。所以需要一条正向证据
链：节点只要发过一个**等回执**的上行帧（`lease.renew→lease.ack`、`task.result→result.ack`、
`task.emit→emit.ack`），超过 `2×LeaseRenewInterval`（默认 40s）没有收到**任何**下行帧，就主动
`CloseNow` + 重连，并在日志里写明「最后收到帧的时间」：

```
hub: 超过 40s 没收到任何下行帧（等回执已 40.1s，最后收到帧于 12.6s 前），主动断开重连
hub: hub: 读取帧: failed to get reader: use of closed network connection；1s 后重连
```

为什么以「有等待回执的帧」为计时起点而不是「任何时刻」：中枢在节点空闲时**一个帧都不发**，
拿「无下行帧」当判据会把每条空闲连接每 40s 掐一次。续租每 20s 一次、每次都有 ACK，所以运行中
的节点天然每 20s 有正向证据。

为什么用 `CloseNow` 而不是优雅 `Close`：`Close` 要等对端的 close 帧（握手期 5s 级），而这里的
判据恰恰是「对端不说话」——等它只会把重连推迟到判据失效之后（实测：优雅关闭让「2 次心跳内
重连」变成 3.4s 才断）。

## 4. abandoned 的可恢复性：选「按 maxAttempts 重试」

两个候选：**(a) 节点重连后重新认领** vs **(b) 按 `maxAttempts` 重试**。

选了 (b)，理由（也是「改动面小」的具体含义）：

1. **(a) 在这个故障下不可能成立**：节点自己的本地 deadline（最后一次 ACK + TTL）到点就会
   TERM→KILL 进程组，`≥TTL` 的抖动之后那次执行**已经死了**，没有「认领回来继续跑」可谈。
   要让 (a) 成立，节点必须放弃「断线不能无限跑」这条不变量（capability.md §8.5），并让中枢
   能逆转终态。
2. **(a) 会撞上「终态不可逆」**：中枢在这条路上已经把任务推成 `abandoned` 并在**同一事务**里
   收敛了两本账（#660：执行账补 `reaped_at` + 槽账释放），复活它等于把 #660 的收敛反向再做
   一遍。新增 claim 帧 / 中枢复活路径 / 账本反向迁移，风险远大于收益。
3. **(b) 只是把一个判断挪了个位置**：`recoverOne` 里 `t.Idempotent && t.Attempt < t.MaxAttempts`
   → `t.Attempt < t.MaxAttempts`，其余事务、CAS、账本收敛全部不动。

**为什么这不会让同一次执行跑两遍**（三段各自可检查）：

- **attempt 账**：重排不改 `attempt`，也不写 `reaped`（旧进程可能还在跑，不伪造事实）；下一次
  `lease.grant` 才把 attempt +1 并生成**新的** `attemptToken`，覆盖 `attempt_token_hash`。
  旧进程此后 emit / 写结果一律被 `EmitAuthorized` / `CompleteTaskAt` 在事务里重做判定后拒绝
  （`VerifyAttemptToken` 的「旧令牌必然对不上」）。所以「同一个 attempt 的第二份事实」不可能存在。
- **物理进程**：旧进程组最迟在自己 deadline + `LeaseTermGrace`(5s) 内被 KILL；重排的退避是
  `RetryBackoff(attempt) + LeaseTermGrace`（≥10s 起），新 attempt 的 spawn 不会与旧进程组重合。
- **预算**：`attempt >= maxAttempts` 仍然直接终态（`max_attempts_exceeded`），所以「死节点」
  最多重排 `maxAttempts-1` 次就被判死 —— 新逻辑不可能把「死」变成「永远不判死」。

**语义变化与正确的「不要重试」写法**：`idempotent` 不再是「失联是否重试」的判据（它仍然作为
派发时钉住的事实投影给 API）。声明「绝不重跑」的正确写法是 `maxAttempts: 1`（那时这里仍然
直接终态，原因码 `lease_expired`）。这是本 PR 唯一的契约语义变化，`docs/contracts/v4/CONTRACT.md`
仍写着「固定默认 TTL 60s」，**需要契约 owner 同步一句**（本 PR 未改 `docs/contracts/`，
见 §8 遗留）。

## 5. 验收证据（真跑）

环境：macOS（Darwin）、Go 1.25、`export PATH=/tmp/venv-yaml/bin:$PATH`。

### ① TTL 配置：非法值启动即拒 / 合法值生效

```
$ go test ./internal/protocol -run 'TestLeaseConstants|TestValidateLeaseTTL' -count=1
ok  	github.com/zzjcool/anotify/internal/protocol	0.395s

$ go test ./internal/server -run 'TestLeaseTTL' -count=1 -v
=== RUN   TestLeaseTTLEnvParsing
=== RUN   TestLeaseTTLEnvParsing/未设置_=_默认_180s
=== RUN   TestLeaseTTLEnvParsing/duration_形式
=== RUN   TestLeaseTTLEnvParsing/分钟形式
=== RUN   TestLeaseTTLEnvParsing/纯数字按秒
=== RUN   TestLeaseTTLEnvParsing/零
=== RUN   TestLeaseTTLEnvParsing/负数
=== RUN   TestLeaseTTLEnvParsing/写错
=== RUN   TestLeaseTTLValidate
=== RUN   TestLeaseTTLValidate/显式_0（哨兵）
=== RUN   TestLeaseTTLValidate/低于协议下限_60s
=== RUN   TestLeaseTTLValidate/负数
=== RUN   TestLeaseTTLValidate/恰好差一点到_3_个续租周期
=== RUN   TestLeaseTTLValidate/未配置_=_默认
=== RUN   TestLeaseTTLValidate/协议下限
=== RUN   TestLeaseTTLValidate/实测默认
=== RUN   TestLeaseTTLValidate/更长也合法
--- PASS: TestLeaseTTLEnvParsing (0.00s)
--- PASS: TestLeaseTTLValidate (0.00s)
PASS

$ go test ./internal/store -run TestLeaseTTLAppliesToGrantAndRenew -count=1 -v
=== RUN   TestLeaseTTLAppliesToGrantAndRenew
--- PASS: TestLeaseTTLAppliesToGrantAndRenew (0.03s)
```

覆盖：`<60s`（59s）、`0`、负数、`与续租间隔不匹配`（`3×renew-1s`，以及 protocol 包里用
`renew=45s` 单独验证「只违反比例、不违反下限」的组合）、`120s`/`180s`/`1h` 合法、
`lease_until` 真的写进配置值（grant 与 renew 同口径）。

### ② 静默断连：2 次心跳内判定并重连

```
$ go test ./cmd/anode/internal/hub -run TestHeartbeatClosesSilentlyDeadLink -count=1 -v
=== RUN   TestHeartbeatClosesSilentlyDeadLink
    client.go:591: hub: 超过 150ms 没收到任何下行帧（等回执已 162ms，本会话还没收到任何帧），主动断开重连
    client.go:591: hub: hub: 读取帧: failed to get reader: use of closed network connection；1s 后重连
--- PASS: TestHeartbeatClosesSilentlyDeadLink (0.19s)
PASS
```

夹具刻意让**控制帧照常工作**：假中枢持续 `Read`（ping 有 pong），但一个下行**业务**帧都不写
—— 这正是 #661 的形态。判据是「2×HeartbeatTimeout 内必须重新握手」。

### ③ 一次真实长任务跨过 ≥2 个 TTL 不被 abandon

```
$ go test ./internal/lease -run TestLongTaskAcrossTwoTTLsIsNotAbandoned -count=1 -v
=== RUN   TestLongTaskAcrossTwoTTLsIsNotAbandoned
--- PASS: TestLongTaskAcrossTwoTTLsIsNotAbandoned (4.57s)
```

口径与生产同构、只缩小量级：`SetLeaseTTL(2s)`、续租 200ms（10 个周期/TTL，生产 9 个）、
`Sweeper.RunOnce` 每 50ms 一轮、**真实定时器**跑 2.25 个 TTL（4.5s）。断言：巡检累计
`abandoned=0 requeued=0`、任务仍在 `running` 且 `attempt=1`、`lease_until` 随续租推进。
（TTL 不能压到 1s：`lease_until` 是秒口径，1s 时续租推不动同一秒内的 deadline —— 这条也顺手
暴露了 `leaseUntil` 的向下截断问题，已改成向上取整。）

### ④ 反例：真死的节点仍在 TTL 后被摘下来

```
$ go test ./internal/lease -run TestKilledNodeIsAbandonedAfterTTL -count=1 -v
=== RUN   TestKilledNodeIsAbandonedAfterTTL
--- PASS: TestKilledNodeIsAbandonedAfterTTL (0.05s)
```

节点进程被 kill（不再续租）后：第 1、2 轮 TTL 后被**重排**（`requeued=1`，摘掉 node、回
pending + retry_backoff），第 3 轮预算用尽 → `abandoned`，`attempt=3`（被 `maxAttempts` 卡死），
且 `reaped_at` 补齐、槽位释放（#660 两本账）。即「死」仍然会被判死，只是按运维声明的预算走完。

配套（store 层，同一套判据）：

```
$ go test ./internal/store -run 'TestRecoverLostLeasesRequeues|TestLeaseSweep|TestConsecutiveLeaseAbandons|TestMaxAttemptsExceeded' -count=1 -v
=== RUN   TestLeaseSweepAbandonSettlesBothLedgers
--- PASS: TestLeaseSweepAbandonSettlesBothLedgers (0.05s)
=== RUN   TestConsecutiveLeaseAbandonsDoNotLeakCapacity
--- PASS: TestConsecutiveLeaseAbandonsDoNotLeakCapacity (0.02s)
=== RUN   TestRecoverLostLeasesRequeues
=== RUN   TestRecoverLostLeasesRequeues/idempotent-requeues
=== RUN   TestRecoverLostLeasesRequeues/non-idempotent-requeues
=== RUN   TestRecoverLostLeasesRequeues/budget-exhausted-abandons
--- PASS: TestRecoverLostLeasesRequeues (0.02s)
=== RUN   TestMaxAttemptsExceededAbandons
--- PASS: TestMaxAttemptsExceededAbandons (0.01s)
PASS
ok  	github.com/zzjcool/anotify/internal/store	0.608s
```

## 6. 变异（每条都必须红）

**变异 ①：去掉硬心跳**（删 `go c.watchHeartbeat(pingCtx, conn)`）

```
$ go test ./cmd/anode/internal/hub -run TestHeartbeatClosesSilentlyDeadLink -count=1
--- FAIL: TestHeartbeatClosesSilentlyDeadLink (0.30s)
    client_test.go:225: 静默断连未被判定：300ms 内没有重连（连接数 1）
FAIL
FAIL	github.com/zzjcool/anotify/cmd/anode/internal/hub	0.398s
```

**变异 ②：去掉 TTL 下限校验**（删 `Config.Validate` 里的 `protocol.ValidateLeaseTTL` 调用，
`gofmt`/`go build` 仍绿——只有测试红）

```
$ go test ./internal/server -run TestLeaseTTLValidate -count=1
        config_test.go:261: LeaseTTL=59s 必须被 Validate 拒绝
    --- FAIL: TestLeaseTTLValidate/负数 (0.00s)
        config_test.go:261: LeaseTTL=-1m0s 必须被 Validate 拒绝
    --- FAIL: TestLeaseTTLValidate/恰好差一点到_3_个续租周期 (0.00s)
        config_test.go:261: LeaseTTL=59s 必须被 Validate 拒绝
FAIL
```

## 7. 门禁

| 门禁 | 结果 |
| --- | --- |
| `make lint-go`（gofmt + vet + staticcheck SA\*/U1000） | ✅ |
| `make check-annotations` | ✅ 全部引用可解析、棘轮未超基线 |
| `make check-test-discipline` | ✅ 五项与基线一致（`time.Sleep` 37/37，未新增） |
| `make check-comment-density` | ✅ 总量 **42893 → 42890（只降）**，dirs/dense 同步收紧 |
| `make contract` / `check-agents` / `check-examples` / `check-secrets` / `check-anode-skill` / `bench-dispatch` | ✅（`make ci` 跑到 `make test` 之前的全部步骤都过） |
| `make ci` 的 `make test`（`go test ./...`） | ⚠️ 本机（Darwin）**不能** exit 0：`cmd/anode` 的 install/TUI/ETXTBSY 一批用例是 Linux-only。**已在基线 `04c5e22e` 上复跑同一套**，失败集合逐条比对：51 条相同 + `TestRework2HubWriteQueueFencesRealRunnerResult`（既有 flake，本机 3/8 概率红，且**回滚本 PR 的 runner 改动后仍是 3/8**） |
| `make test-frontend-v4` | ⚠️ 未设 `E2E_CHROME_PATH` 时 16 条红（`Installed Chrome required`，本机 Chrome 在 `/Applications`，驱动只找 Linux 路径）；设上之后 **213/213 通过**（基线 commit 同样 16 条红） |
| `make e2e` | ❌ 本机 docker 不可用 —— **未跑**（如实说明，不冒充覆盖） |

相关包（本 PR 触碰面）单测：

```
$ go test ./internal/protocol ./internal/lease ./internal/store ./internal/store/v4test ./internal/server ./cmd/anode/internal/hub ./cmd/anode/internal/runner ./cmd/anode/internal/node ./cmd/anode/internal/app -count=1
ok  	github.com/zzjcool/anotify/internal/protocol
ok  	github.com/zzjcool/anotify/internal/lease	5.1s
ok  	github.com/zzjcool/anotify/internal/store
ok  	github.com/zzjcool/anotify/internal/store/v4test
ok  	github.com/zzjcool/anotify/internal/server
ok  	github.com/zzjcool/anotify/cmd/anode/internal/hub	（含既有 flake）
ok  	github.com/zzjcool/anotify/cmd/anode/internal/node
ok  	github.com/zzjcool/anotify/cmd/anode/internal/app
```

（`cmd/anode/internal/runner` 的 3 条红是既有平台差异：`TestSpawnRetriesTextFileBusy` /
`TestSpawnReportsTextFileBusyAfterRetriesExhausted` / `TestUserWorkDirAncestorOfSystemRootRejected`，
基线上同样红。）

## 8. 遗留风险 / 未做

1. **契约文档未同步**：`docs/contracts/v4/CONTRACT.md` 第 86 行仍写「固定默认 TTL 60s」，
   `idempotent` 的语义也从「失联是否重试」变成了「事实投影」。本次改动边界不含 `docs/contracts/`
   与 `api/`，需契约 owner 补一句（改 TTL 默认值 + `maxAttempts: 1` 才是「绝不重跑」）。
2. **schema 未动**：不需要迁移块（`lease_until` / `max_attempts` / `task_attempts` 都是既有列）。
3. **中枢侧没有对应的「死会话」探测器**：#661 的另一半（中枢 `lastSeenAt` 冻结、派发停止）
   现在靠「节点硬心跳重连」间接解决（重连即刷新 lastSeenAt）。中枢自己判定「这条会话 40s 没
   上行帧就 detach」需要动 `internal/ws`，不在本 PR 边界内 —— 建议作为 #661 的后续单独一张卡。
4. **TTL 上限**：`ANOTIFY_LEASE_TTL` 只校验下限与比例，没有上限。配成 1h 会让「节点真死」的
   检测时延变成 1 小时；如需上限，应作为单独决策（而不是塞进本 PR 的校验里）。
5. **压缩时钟的取舍**：验收 ③ 用 2s TTL 而非 180s（跨 2 个 TTL 就是 6 分钟），机制同构但
   不覆盖「真实 20s 续租节奏下的调度抖动」。真 180s 的长任务验证需要两台真节点 + 隧道，
   属 `make e2e` 面的补测。
