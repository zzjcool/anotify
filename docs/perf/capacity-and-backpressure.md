# 容量与背压：保留期、磁盘稳态测算、背压配置与运维

> 对应 issue：#229（事件保留期关闭且无上限）· #228（派发无背压）。
> 测量脚本：`scripts/perf/c/s6-capacity.sh`（6 段，可单独跑：`bash scripts/perf/c/s6-capacity.sh a b`）。
> 端口段 8140-8159，`taskset -c 8-11`，产物落 `/home/<user>/perf-b5`（**不要落 /tmp**：
> 那里是 7.5G tmpfs，本实验的库能到几百 MB）。
>
> 本文所有数字都是**实测**的，并且每个数字都注明来自哪一段、哪一行输出
> （产物目录 `~/perf-b5/out/<段>/`，每段另有一份 `out/report-<段>.txt`）。
> 外推（7 天 / 30 天）不是估的，是「实测 B/事件 × 实测速率」。换机型 / 换 payload 大小要重跑脚本。

---

## 0. 一句话结论

| 问题 | 结论 |
|---|---|
| 一条事件在磁盘上值多少 | **679.9 B**（空 payload）/ **1908.7 B**（1KB payload）；一条 ingest 落 **2 行** events |
| 30 天保留窗口的稳态大小 | `679.9 B × 速率 × 2,592,000 s`。38 ev/s → **62.4 GiB**；10 ev/s → **16.4 GiB** |
| **10 GiB 盘 + 30 天保留能撑多久** | 分水岭在 **6.09 ev/s**。≤6.09 ev/s → 先在 30 天处**平台化**，磁盘永不满（保留期是瓶颈）；>6.09 ev/s → **磁盘先满**（磁盘是瓶颈），10 GiB 在 38 ev/s 下只能撑 **4.8 天** |
| 清理跟得上写入吗 | 单轮 20,000 行 / 242ms ≈ **8.3 万行/秒**；追赶态 5,760 万行/天/范围，对 518 万行/天有 **11 倍余量** |
| 清理伤读吗 | **测不出**：读 p95 **1.838 ms → 1.837 ms**（删除 20,000 行/350ms 完全落在窗口内） |
| 背压开着的额外开销 | 请求路径 **0 次 DB 往返**（10000 请求 → 采样调用 1 次）；每请求 CPU 1788.8 → 1776.9 µs、读 syscall 28.75 → 28.16（**无增量**） |
| 超阈时 | 99.6% 请求被 503，**p95 932 µs**（正常写 p95 ≈ 35 ms，**快 37 倍**）；被收下的 2,182 条一条任务都没丢 |

---

## 1. 一条事件在磁盘上值多少字节

口径（照 #229 的复现要求）：每量一个点之前先 `PRAGMA wal_checkpoint(TRUNCATE)`，否则量到的是
「主库 + 一堆未回写 WAL」（差 4 倍以上）。产物：`out/a/points.csv`、`out/a/deltas.txt`。

| 档 | 发出事件 | events 行数增量 | 磁盘增量 | **B/事件** | 行/事件 |
|---|---|---|---|---|---|
| 空 payload 第 1 批 | 1000 | 2000 | 647,168 B | **647.2** | 2.00 |
| 空 payload 第 2 批 | 1000 | 2000 | 712,704 B | **712.7** | 2.00 |
| 1KB payload | 500 | 1000 | 954,368 B | **1908.7** | 2.00 |

`del` 原文（`out/a/deltas.txt`）：

```
m0→m1000       events_sent=1000 delta_rows=2000 delta_bytes=647168 rows/event=2.00 B/event=647.17
m1000→m2000    events_sent=1000 delta_rows=2000 delta_bytes=712704 rows/event=2.00 B/event=712.70
m2000→m2000_1k events_sent=500  delta_rows=1000 delta_bytes=954368 rows/event=2.00 B/event=1908.74
```

- **空 payload 档 = 两批均值 679.9 B/事件**（与 #229 的 655.4 同量级；批间本身差 10%，
  来自页分配/索引分裂的抖动，所以用均值而不是单批）。
- **一条 ingest 落 2 行 events**：事件本体 + `route|pruned.*` 剪枝留痕（`internal/route/dispatch.go`）。
  所以「按事件数估容量」必须把剪枝行算进去；**按行数估则要把条数乘 2**。
  这两处漏一个都刚好差一倍 —— 第一版脚本就是这么错的（把 B/行 当成了 B/事件，
  得到的 323.6 B 恰好是 647.2 的一半）。
- payload 每多 1KB ≈ **多 1230 B/事件**（1908.7 − 679.9）。

## 2. 30 天稳态：算式与结论

**算式（可复算）**：

```
稳态大小 = B/事件 × 速率(ev/s) × 保留窗口(秒)
         = 679.9 B × R × 2,592,000 s            (30 天)
1 GiB ≈ 1.0737e9 B
```

空 payload 档（**679.9 B/事件**，产物 `out/a/`）：

| 写入速率 R | 1 天 | 1 周 | **30 天（保留窗口）** | 10 GiB 盘撑多久 |
|---|---|---|---|---|
| 1 ev/s | 0.05 GiB | 0.38 GiB | **1.64 GiB** | 182.8 天 |
| 10 ev/s | 0.55 GiB | 3.83 GiB | **16.41 GiB** | 18.3 天 |
| 30.6 ev/s（#228 soak 实测） | 1.67 GiB | 11.72 GiB | **50.22 GiB** | 6.0 天 |
| 38 ev/s（#229 基准档） | 2.08 GiB | 14.55 GiB | **62.37 GiB** | 4.8 天 |
| 100 ev/s | 5.47 GiB | 38.30 GiB | **164.13 GiB** | 1.8 天 |
| 876 ev/s（本机 ingest 上限，conc=8） | 47.93 GiB | 335.5 GiB | **1437.8 GiB** | 0.2 天 |

1KB payload 档（**1908.7 B/事件**）：

| 写入速率 R | 30 天 | 10 GiB 盘撑多久 |
|---|---|---|
| 30.6 ev/s | **140.9 GiB** | **2.13 天** |
| 38 ev/s | **175.1 GiB** | **1.71 天** |
| 100 ev/s | **460.7 GiB** | **0.65 天** |

### 「10 GiB 盘在 30 天保留下能撑多久」的准确回答

这不是一个数，而是一个**分水岭**：

```
保留窗口需要的空间   S30 = 679.9 B × R × 2,592,000
磁盘能撑的天数       D   = 10 GiB / (679.9 B × R × 86,400)

D ≥ 30  ⇔  R ≤ 10 GiB / (679.9 B × 2,592,000) = 6.09 ev/s
```

- **R ≤ 6.09 ev/s**：`S30 ≤ 10 GiB`。磁盘在 30 天**之前**就到达平台
  （文件大小 ≈ `S30` 不再增长），**保留期是瓶颈** —— 结论是「永远不会满」，不需要扩容。
- **R > 6.09 ev/s**：磁盘先满，**磁盘才是瓶颈**。10 GiB 的寿命 = `10 GiB / (679.9 B × R × 86,400)`：
  38 ev/s → **4.8 天**，100 ev/s → **1.8 天**。此时保留期救不了容量，必须扩容或调小窗口。

**所以要盯的告警线是两条，而不是一条**：

1. `R > 6.09 ev/s` 时立刻按 `S30` 规划磁盘（先算 `679.9 × R × 2,592,000` 再乘 1.3 余量）。
2. `server.retention.permanent` 一出现 → 上面所有算式都不成立（永不清），
   磁盘按 `679.9 B × R × 86,400 × 天数` 线性增长。

### 三条必须写进容量规划的结论

1. **保留期封住的是增长上限，不是省磁盘**：它把「从开机到现在所有事件之和」换成
   「保留窗口内的写入量」这个可预测的数。
2. **`auto_vacuum=0` ⇒ 删了不还盘**。文件大小 = 历史最高水位；要真缩回去只能 `VACUUM`
   （本仓没有，且需要长空闲窗口）。所以**磁盘阈值要按「峰值积压 + 保留窗口」定，不能按当前行数定**。
3. **一条 ingest 落 2 行**：按行数估容量会把结论算错一半。

## 3. 实测：清理真的把文件「平台化」了吗

`s6-capacity.sh b` 段：造 26,000 条「40 天前」的 events → 起服务（保留期 1 天）→
首个 5s tick 触发第一轮 → 删空后再灌同样多行，比较文件/页数。产物 `out/b/sizes.txt`。

| 时点 | 文件字节 | page_count | freelist | events 行 |
|---|---|---|---|---|
| before（满积压） | 5,185,536 | 1266 | 0 | 26,000 |
| after（删空） | 5,185,536 | 1266 | **1178** | 0 |
| refilled（重灌 26,000 行） | 5,267,456 | 1286 | **0** | 26,000 |

- **删空后文件一字节没变**（100.0%），93% 的页进 freelist —— 印证 `auto_vacuum=0`。
- **重灌同样多的行只多花 1.6%**（+81,920 B），freelist 被复用 → **稳态是「页数到顶不再涨」，
  不是「文件变小」**。这条不写清，运维开了保留期看到磁盘没变小会以为功能没生效。

### 清理吞吐与轮次（`out/b/sweep-round1.log`）

```json
{"event":"lease.sweeper.retention","events_deleted":20000,"truncated":true, "elapsed_ms":242}
{"event":"lease.sweeper.retention","events_deleted":6000, "truncated":false,"elapsed_ms":67}
```

- 单轮 **20,000 行 / 242 ms ≈ 82,600 行/秒**；小轮 **6,000 行 / 67 ms ≈ 89,600 行/秒**。
- **从满积压到删空 35 s**（首个 5s tick + 一次 30s 追赶门）。
- **幂等 / 可重入**：同一份库再起一次进程（已无过期数据）→ **没有再删任何行**
  （`lease.sweeper.retention` 只在「删了东西或被截断」时才打，所以第二轮一条都不该有）、
  `retention_failed` **0 条**、文件**没有被再次撑大**。
  这一条的**门禁**在 `make e2e` 的 capacity 套件里（真进程、真重启、逐项断言，见 §8）；
  perf 侧的旁证是 `out/b/second-round.log` 里该进程一条 retention 轮次都没有、也没有 failed
  ——注意「没删到就不打日志」本身是设计，所以这个文件里**不会**出现 `events_deleted: 0`。
- **inflow 对账**：30 ev/s × 2 行/事件 = **518 万行/天**。
  - 旧预算（5 批 × 1000 × 每小时一轮）= **12 万行/天** → 差 **43 倍，永远删不完**（A5 要修的坑）。
  - 新预算（20 批 × 1000 × 常态 1 小时 / 截断后 30s 追赶）= 追赶态 **5,760 万行/天** → **11 倍余量**。

## 4. 清理对读路径 p95 的影响

口径（**两命名空间**）：读压测固定打命名空间 A（24,000 行，全程不变），清理删的是命名空间 B
的 26,000 行过期事件。两次测量各先跑 4s 预热（丢弃）再量 20s，`-c 4`。
产物 `out/c/read-baseline.txt`、`out/c/read-under-sweep.txt`、`out/c/sweep.log`。

> 为什么不能用「同一个命名空间里造过期行再删掉」：那会产生一个与被测目标无关的巨大混淆项
> ——**表变小了，读变快了**。第一版脚本就是这么做的，结果 p95 从 111ms「降到」64ms，
> 证明的是「数据少了读得快」，不是「清理不伤读」。

| 场景 | 总请求 | RPS | p50 | p95 | p99 |
|---|---|---|---|---|---|
| baseline（保留期 0，不跑清理） | 69,907 | 3495.3 | 943 µs | **1.838 ms** | 2.730 ms |
| under sweep（保留期 1，删除落在窗口内） | 70,771 | 3538.5 | 929 µs | **1.837 ms** | 2.790 ms |

- 这一轮删了 **20,000 行 / 350 ms**，完全落在压测窗口内（`out/c/sweep.log`）。
- **p95 差 0.001 ms**，p99 差 0.06 ms。
- 机制：WAL 的锁语义是「读者不阻塞写者、写者不阻塞读者」，两侧没有共享锁。
  所以预期就是「无可测退化」，上面的数字是这句话的实测。
- 分批 `LIMIT` 控制的是**单次写锁持有量**（每批一次 1000 行的独立短事务），
  不是「让读更快」——读本来就不等写。

## 5. 背压（#228）

### 5.1 判定信号

```
pending_age = now - MIN(created_at) FROM tasks WHERE status = 'pending'
```

**不用 pending 行数**：行数随命名空间数 / 能力数 / 流量口径漂移，跨部署不可比；
「最老的 pending 等了多久」与部署规模无关。

**不加 `not_before` 过滤**：`SetNotBefore` 是「重派退避」，而写它的正是
「派不出去（`ErrNoCapacity`）」那条路径（`internal/server/mux.go` 的 `Redispatch`）。
按 `not_before` 过滤等于把 #228 的实测场景整个漏掉。

**成本**：迁移 17 建了部分索引
`idx_tasks_pending_created_at ON tasks(created_at) WHERE status='pending'`，
把这条查询退化成一次「索引最左端 seek」，成本与积压行数无关（实测 10,000 行 0.13ms /
100,000 行 0.13ms）。**一个边界**：SQLite 的 min/max 优化要看到该索引自己的 `sqlite_stat1`
条目才会选它；「全新库灌满数据、尚未重开进程」的那个窗口里规划器仍会走 `idx_tasks_lease`
（回表扫 pending 行，10,000 行 4.77ms / 100,000 行 51ms）。重开一次进程
（`Open` 末尾的 `PRAGMA optimize=0x2`）即补上；老库升级没有这个窗口。
降级期间**判定值仍然正确**，慢的只是后台采样 goroutine，请求路径只读 atomic。

### 5.2 未超阈时的额外开销：为什么是 0，怎么验

**设计**：请求路径**完全不碰数据库**，也不维护内存计数。判定由后台采样推进，
放进一个 `atomic.Bool`；`Wrap` 每请求只做**一次** `atomic.Bool.Load()`。

- 谁维护：`backpressureGuard.Run` 这个 goroutine（`NewApp` 里 `go bp.Run(ctx)` 起一份）。
- 何时更新：自适应降采样 —— 本次年龄 `age*2 < MaxAge` 时下一轮 **5s** 后，否则 **1s** 后。
  即稳态 **0.2 次/秒**，接近/超过阈值才 **1 次/秒**。
- 超阈的判定也是它算好写进 atomic 的（`rejecting = over && !stuck`），
  请求路径不做任何比较以外的逻辑。

**四条证据，从机械到端到端**：

| # | 口径 | 结果 | 产物 |
|---|---|---|---|
| 1 | **单测**：10000 条请求打 `Wrap`，数 `pendingAgeSource` 被调几次；且 fake 一被调用就 `t.Fatalf` | 调用 **1 次**（就是那一次显式采样）→ **每请求 0 次 DB 往返** | `TestBackpressureUnderThresholdNoDBRoundTripOnRequestPath` |
| 2 | **调用点**：`grep -rn OldestPendingCreatedAt internal/` | 生产代码只有 **1 处**调用（`sample`），`sample` 只被 `Run` 与测试调用 | — |
| 3 | **benchmark**：裸 handler vs 包一层 | 125.9/129.2/147.4 → 140.9/143.6/153.0 ns/op，**分配完全相同**（32 B/op，2 allocs） | `BenchmarkBackpressureWrapUnderThreshold` |
| 4 | **进程级**（`f` 段，25s 同负载，阈值 0 vs 86400） | 每请求 CPU **1788.8 → 1776.9 µs**；每请求读 syscall **28.75 → 28.16** | `out/f/cpu.csv` |
| 5 | **CPU profile**（启用档，`backpressureGuard.Wrap.func1`） | **flat 0.02s / Total 25.70s = 0.078%**（且这 0.02s 是最小采样粒度 0.01s 的两倍，等价于「采样分辨不出来」） | `out/f/cpu-huge.pprof` |

`out/f/cpu.csv` 摘录（只取两列，原行还含 `server_cpu_ms` / `read_syscalls`；
**开关两档没有可测差异，连符号都是「开着更快」**）：

```
off, requests=23015, rps=920.6, cpu_us_per_request=1788.83, read_syscalls_per_request=28.7506
huge,requests=23097, rps=923.8, cpu_us_per_request=1776.85, read_syscalls_per_request=28.1589
```

**如实说明后台采样的代价**（不是零）：固定 **0.2 ~ 1 次/秒**，与请求速率无关。
30 ev/s 的写入下，稳态是「每 150 条事件一次索引 seek」；最坏（贴阈值）是「每秒一次 0.13ms 的 seek」
= 单核的 **0.013%**。

> `d` 段的端到端 RPS/p95 A/B **不作为结论**：这台机是共享的（批次 5 期间 load average 到过 21，
> 12 核），同一轮里两对的符号就能反过来 —— 实测第 1 对 `889.5 → 869.3 rps`（−2.3%）、
> 第 2 对 `811.0 → 911.7 rps`（**+12.4%**），另一轮则是 −42% / −27%。
> 这种量级的漂移读的是别人的负载。判断「有没有额外开销」请看上面 1~5 条。

### 5.3 超阈时的行为（`e` 段实测）

口径：阈值 5s、**1 个槽位 × 每单 3s** 的极慢消费者、`-c 8` 压 40s。产物 `out/e/`。

| 指标 | 数值 |
|---|---|
| 200（收下） | **2,182** |
| **503（拒绝）** | **616,398（99.65%）** |
| 503 延迟 p50 / p95 / p99 | **364 µs / 932 µs / 1.802 ms** |
| 对照：正常 200 的 p95 | ~35 ms（同机 `d` 段实测） |
| 超阈触发时刻 | 服务启动后 ~5s（首个采样立刻，之后 1s 一档） |
| **任务不丢** | `tasks_after = 2,182` **== 被收下的事件数**；`pending_after = 2,173`；最老 pending 40s |

读法：**拒绝比接受便宜 37 倍**（p95 0.93ms vs 35ms）—— 正是背压要的效果：积压时把最贵的
路径（写事件 + 派发 + 落任务）挡在门外，只留读路径与**已入库任务的派发**。

### 5.4 安全阀：超阈过久必须 fail-open（否则是永久停机）

**为什么必须有**：判定信号是 `MIN(created_at) WHERE status='pending'`，而库里一旦存在
**永远派不出去**的 pending，这个值只增不减：

- 能力升级后没有节点再声明那个版本；
- 节点下线后没人再声明该能力（`RedispatchTask` 见 `len(nodes)==0` 就返回 false，保持 pending）；
- 任务建出来后集群里再没有能跑它的节点。

全仓**没有任何 abandon 路径**，所以这些行永远停在 pending。于是一条孤儿任务就能把
**整个部署**的新事件入口钉在 503 上，而且永不恢复 —— 这比「队列继续涨」糟得多。

**做法**：连续超阈超过 `StuckAfter`（默认 **30 分钟**，下限 1s）就**停止拒绝**（fail-open），
并打一条 `ERROR server.backpressure.stuck` 说明最可能的原因。信号回落后自动复位，
**重新具备**拒绝能力（不是一次性烧毁的开关）。

这是刻意的取舍：真正的短时过载（分钟级）在 30 分钟内早自愈了；只有结构性坏掉才会走到这里，
那时必须有人去看日志。日志节流：`still_engaged` 60s 一条，`sample_failed` 60s 一条
（超阈时采样档是 1s，不节流就是 1 条 WARN/秒）。

### 5.5 配置

| 环境变量 | 默认 | 语义 |
|---|---|---|
| `ANOTIFY_PENDING_MAX_AGE_SEC` | **120** | 最老 pending 等待秒数**超过**它 → 新事件入口 503。`0` = 不启用；负数同 `0`；**非数字**回落 120；**`> 7 天` 钳到 7 天**（防 `time.Duration` 回绕） |
| `ANOTIFY_EVENT_RETENTION_DAYS` | **30** | events 保留天数。`0` = **永久保留**（启动日志 WARN：磁盘会持续增长）；负数同 `0`；**非数字**回落 30；**`> 100 年` 钳到 100 年**（防 `days*86400` 回绕成「删全库」） |

> 两个上界都是**防溢出**，不是产品策略：`strconv.Atoi` 能吃下 9.2e18，乘完会回绕，
> 而回绕方向极其危险（保留期回绕成几分钟 = 一次删掉几乎全库；背压阈值回绕成 290ms = 全站立刻 503）。
> 手滑多打几个 0 是最常见的事故来源。
>
> **超上限的处理是「钳到上限」，不是「回落默认」**，因为回落的方向是反的：
> 填一个天文数字的人想的是「几乎永不删」/「几乎不触发」，回落成 30 天等于**加速删**、
> 回落成 120 秒等于**变严**。钳到上限（100 年 / 7 天）保住他想要的方向，也保证乘法不回绕。
> 只有**看不懂的输入**（非数字）才回落默认。
> 两条路径（env 解析 / 直接写 `Config`）语义一致，有单测钉住：
> `TestRetentionAndBackpressureOverflowClamp`。

120 的来路：#228 的 soak 里积压以 ~8.3 行/秒线性增长、p99 排队 89s。取 120s ≈ 比实测 p99
再留一档余量：能容忍一次分钟级洪峰自愈，又不会让用户等到分钟级延迟。
**为什么默认不是更激进**：这个阈值是「拒绝用户请求」的开关，误触代价高于晚触；
单机 / 无节点自动扩容的小部署尤其容易长期贴着阈值 —— 给它们一个 `0` 的出口。

### 5.6 入口与返回码

被拒的是**三个新事件入口**（读端点完全不受影响，否则运维看不到发生了什么）：

| 入口 | 路径 | 包装位置 |
|---|---|---|
| 机器入口 | `POST /v1/events` | `noStore(bp.Wrap(eventH))` |
| webhook | `POST /v1/hooks/{token}` | `noStore(bp.Wrap(hook))` |
| 手动触发 | `POST /v1/sources/{id}/fire` | `noStore(sessMW(bp.Wrap(fire)))` |

第三行的顺序是刻意的：`bp` 在**会话鉴权内层**，未登录先拿到 **401**，
不许让人从 503/200 的差别里读出「中枢现在积压了吗」。这条有 e2e 钉住。

503 响应：

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json; charset=utf-8
Cache-Control: no-store
Retry-After: 15
```
```json
{"error":"backpressure","pending_age_sec":213,"max_pending_age_sec":120,"retry_after_sec":15}
```

- `Retry-After: 15`：一次退避后重试足够（积压消化是秒级推进的），又不至于把重试打成脉冲。
- `Cache-Control: no-store`：这是「此刻不该重试」的即时结论，任何层都不许缓存。
- `pending_age_sec` 向下截断、判定用全精度，所以可能出现
  `pending_age_sec == max_pending_age_sec` 却仍被拒（例如 120.5s）。
  调用方决定退避多久请用 `retry_after_sec`，不要用两者相减。

## 6. 运维视角：日志与告警

启动时（务必进日志看）：

| event | 级别 | 含义 |
|---|---|---|
| `server.retention.configured` | INFO | 这台机器**会**删超过保留期的事件：`days` / `events_retention_sec` / `est_bytes_per_event` |
| `server.retention.permanent` | **WARN** | `days=0`：**永不删，磁盘随写入线性增长**。`hint` 里明写「未设置时默认 30；显式 0 或负数都落到这里」 |
| `server.backpressure.configured` | INFO | `pending_max_age_sec` / `enabled` |
| `lease.sweeper.retention_enabled` | INFO | 一轮都没跑过时打一次：`interval` / `catch_up_interval` / `batch` / `batches` / `event_retention`（`enabled（30d）` 或 `permanent（不清理…）`） |

每个清理轮次（**成功 / 失败 / 取消三条路径都带** `events_retention_sec` + `elapsed_ms`）：

| event | 级别 | 何时打 |
|---|---|---|
| `lease.sweeper.retention` | INFO | 真删了东西或被截断（「本轮没有可删的」不打日志） |
| `lease.sweeper.retention_failed` | ERROR | 失败，但**同样带 partial report 的计数**与 `elapsed_ms` —— 不许「删了一部分但日志里看不见」 |
| `lease.sweeper.retention_cancelled` | INFO | ctx 取消（优雅退出） |

背压期间：

| event | 级别 | 说明 |
|---|---|---|
| `server.backpressure.engaged` | **WARN** | 开始拒绝（`pending_age_sec` / `max_pending_age_sec` / `stuck_after`） |
| `server.backpressure.still_engaged` | WARN | 每 60s 一条（节流）：`engaged_secs` / `rejected_total` / `rejecting` |
| `server.backpressure.stuck` | **ERROR** | 超阈超过安全阀 → **已停止拒绝**（fail-open）。最可能是孤儿 pending，见 §5.4 |
| `server.backpressure.released` | INFO | 恢复收事件：`rejected_total`（进程级累计）/ `engaged_secs`（本轮）/ `was_stuck` |
| `server.backpressure.sample_failed` | WARN | 采样失败，**保持上一次判定**；每 60s 至多一条 |

告警线：

- `server.retention.permanent` 出现 → 直接告警（这是「磁盘无上限」的唯一信号）。
- `server.backpressure.engaged` 持续 > 10 分钟 → 告警（消费能力长期不足：加节点 / 加槽位）。
- `server.backpressure.stuck` 出现 → **告警并人工介入**，见下面的 runbook。

### 6.1 Runbook：`engaged` 一直不 released / 出现 `stuck`

先分清是**过载**还是**孤儿任务**，两者的处置**相反**：

```sql
-- ① 最老的 pending 是谁？它指向哪个能力？
SELECT id, namespace_id, capability_id, capability, version, created_at,
       CAST(strftime('%s','now') AS INTEGER) - created_at AS age_sec
FROM tasks WHERE status = 'pending' ORDER BY created_at LIMIT 5;

-- ② 这个能力现在有节点声明吗？（空 = 永远派不出去）
SELECT n.id, n.name, n.status FROM nodes n
JOIN node_capabilities nc ON nc.node_id = n.id
WHERE nc.capability_id = '<上一步的 capability_id>';
```

- **② 有行、且节点都忙** → 真过载：加节点 / 加槽位，等它自愈。
- **② 空** → **孤儿任务**，这就是会一路走到 `stuck` 的那条路径。
  **处置不是加槽位**（加多少都没用，没有节点声明它）：
  - 短期恢复：把这条任务取消/置终态，或临时 `ANOTIFY_PENDING_MAX_AGE_SEC=0` 重启止血；
  - 根治：让该能力重新有节点声明（回滚能力版本，或让节点声明新版本并把待办任务迁过去）。
  - 之后信号自然回落，`released` 会带 `was_stuck=true`。

**为什么会有孤儿**：`RedispatchTask` 见「没有节点声明该能力」只返回 false（保持 pending），
全仓**没有 abandon 路径**，所以这些行永远停在 pending 且年龄单调增长。
安全阀（§5.4）保证它不会把整站永久钉死，但**不会替你清掉那条任务** —— 每次重启后仍会再撞一次。
- `lease.sweeper.retention` 的 `truncated=true` 连续出现 > 1 小时 → 清理追不上写入
  （调大预算或核对保留期配置）。
- 磁盘用量 > `679.9 B × R × 2,592,000 × 1.3` → 见 §2。

## 7. 复现

```bash
bash scripts/perf/c/s6-capacity.sh        # 全部区段 a-f（约 10 分钟）
bash scripts/perf/c/s6-capacity.sh a      # B/事件 + 30 天外推
bash scripts/perf/c/s6-capacity.sh b      # 平台化 + 清理吞吐 + 可重入
bash scripts/perf/c/s6-capacity.sh c      # 清理 vs 读 p95（双命名空间）
bash scripts/perf/c/s6-capacity.sh d      # 未超阈 A/B（受机器负载影响，仅辅助）
bash scripts/perf/c/s6-capacity.sh e      # 超阈 503 比例与延迟
bash scripts/perf/c/s6-capacity.sh f      # 进程内开销（CPU µs/请求 + syscall + profile）
```

纪律：`taskset -c 8-11`、端口 8140-8159、产物落 `/home/<user>/perf-b5`。
每段都会写 `out/report-<段>.txt`（跨运行保留），本次运行的全部区段另汇总进 `out/report.txt`。

**踩过的两个坑**（都写进了脚本注释）：

1. 报告原来是每次运行 `: >report.txt` 重置 → 分段跑完最后一段，前面的数字就从汇总里消失了。
2. `cp` 复用库前没清残留 `perf.db-wal` → 旧 WAL 帧被「重放」到新主库上，服务端
   报 `database disk image is malformed (11)`，整个读压测 100% 500。只在重复跑同一段时出现，
   极易被误读成「产品把库写坏了」。

## 8. 端到端门禁

`scripts/e2e/suites/capacity.mjs`（`make e2e` 的一部分，端口段 6950，46 条断言）：

**背压启用（`ANOTIFY_PENDING_MAX_AGE_SEC=1`）**

- 无积压 → 200；造一条 600s 的 pending → **轮询**到 503（不 sleep 固定值，避免偶发红）
- 503 的四个 body 字段、`Retry-After` 头、`no-store`、Content-Type
- 503 期间**任务行数不变**
- **webhook 入口** 503、**`/sources/{id}/fire` 入口** 503、**未登录 `/fire` 是 401 而不是 503**
- **读端点全通**：`GET /v1/events`、`GET /v1/console/events`、`GET /v1/tasks` 都 200
- **不丢任务的正向证据**：503 当口造一条 600s 的 pending 且**有真节点声明该能力** →
  观察到 `status` 离开 `pending` 且 `node_id` 被写上（证明中枢仍在派发），
  同时刻入口**仍然 503**（两件事并存，不是先后）；不可派发的那条仍在 `pending`；任务行一条没少
- 清掉积压后 12s 内自动恢复 200；日志有 `engaged` 与 `released`
- 阈值 0 = 不启用：同样的老 pending 存在也仍 200

**保留期 1 天**

- 40 天前的 events 被删、刚写的留着
- 日志有 `events_deleted` / `elapsed_ms` / `events_retention_sec`，无 `retention_failed`
- **幂等 / 可重入**：把库搬到临时目录外，**用同一个库再起一个进程** →
  第二轮没有失败、没有再删任何行（`events_deleted > 0` 的轮次一条都没有）、
  过期事件仍不存在、窗口内事件仍在、事件行数不变、**文件没有被再次撑大**（±64KB）

**保留期 0**：40 天前的事件**不**被删，启动日志有 `server.retention.permanent`。
