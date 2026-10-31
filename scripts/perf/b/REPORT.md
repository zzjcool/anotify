# B 路 · WS 节点与多跳链路 · 性能评估报告

隶属 issue #217（三路并行性能评估）。本文件只汇总**测量结果与已开的 issue**，
修复方案不在本轮范围（每条 issue 里只写初步归因与影响判断）。

- 代码版本：`01a3f1f`（本 worktree 分支 `pi-subagent/centurion-20-e3891f68`）
- 纪律：全部命令 `taskset -c 4-7`；端口只用 7920-7939；库 `mktemp -d`；
  **不改产品代码**：`selfcheck.sh` 断言「相对评估地基 `01a3f1f` 的 `internal/ cmd/ api/ web/ web-src/`
  无 diff」；相对 `origin/main` 会有 2 个文件差异（`internal/server/config.go`、`mux.go`），
  那是地基提交加的 `ANOTIFY_PROF` 开关，不是本路的改动（selfcheck 会把这两个文件打出来）。
- 复现：`scripts/perf/b/` 下每个场景一个脚本，`bash scripts/perf/b/s0-smoke.sh` 先自检环境

> **测量污染声明（必读）**：本轮 A/C 两路在同一台 12 核机上并行压测，
> 本路（4-7 核）测到的 1 分钟 loadavg 在 **3.9~7.8** 之间波动。
> 因此**绝对值偏悲观**（尤其 p99 与吞吐），但**同一次会话内的相对比较**
> （S2 的 a/b/c/d 四配置、S1 的 N 曲线）是同一时段内测的，可以互相对照。
> 需要"干净数字"的人请在没有其它两路时重跑同一脚本。

## 1. 最重要 3 个发现

### ① 节点重启后，在跑任务永久卡死、槽位永久占用（#218，P0）
`kill -9` agent 后用同一 token 重启，崩溃瞬间在跑的 2 个任务**永久停在 `acked`**：
新连接的心跳把它们的 `lease_until` 一直续到 now+30，巡检永远不回收；
`node_instances.busy_task` 永不释放 ⇒ `FreeSlotsByNode` 恒 0 ⇒ **58 条 pending 任务 100 秒零进展**。
`node.ws.result=0 / lease.sweeper.reclaimed=0 / goroutine 9→9 / fd 11→11`（不是泄漏，是立即死锁）。
控制台仍显示 `online`，**静默瘫痪**。

### ② 积压只靠 5 秒巡检排空，单槽有效吞吐 1.2 task/s（#219 + #220，P1）
槽位满后的 pending **没有"槽一空就取下一条"的触发器**，只能等 `internal/lease` 的 5s tick。
- 32 个任务打 1 个槽：**p50 排队 24.5s、p99 89.3s**，而单任务端到端只有 **15.7ms**（差 1500×）
- 100 个任务打 1 个槽：30s 只排空 36 个 = **1.20 task/s**，同期 server CPU 仅 0.9%（瓶颈不是 CPU）
- 每 5s 一轮的巡检里 `dispatched=3~9`、`skipped=39~66`（几十次注定失败的尝试）
- 同时 `run.concurrency` **完全不参与调度**：1×8（p50 9.3s）vs 8×1（p50 52.6ms）=
  **177×（p50）/ 238×（吞吐）**；而 1×8 相对 1×1 的差异（0.36→0.54 task/s）落在噪声内

### ③ 租约重派会重复执行同一任务（#222，P1，历史坑已复现）
`kill -STOP` agent（心跳饥饿，孙进程继续跑）→ 34.4s 后租约被回收 → 10.1s 后新 agent 上线 →
9.8s 后第二次执行：**同一 task_id 在 marker 里出现 2 行，`pgrep '^sleep 60$'` 同时有 2 个进程**，
`attempts=2`，第一次执行的结果从未上报。窗口 = 判死 20s + 租约 30s + 巡检 ≤5s + 退避 10s ≈ 45~65s。
对照组（`timeoutSec:5` < 租约 30s）不重复——说明"不重复"靠的是**使用者恰好把本地超时设短**这条隐式约定。

## 2. issue 清单

| issue | 严重度 | 一句话 |
|---|---|---|
| #218 | **P0** | agent 重启后 in-flight 任务永久卡 `acked`，槽位钉死，节点假在线 |
| #219 | P1 | pending 只靠 5s 巡检排空 → 单槽 1.2 task/s、p99 排队 89s |
| #220 | P1 | `run.concurrency` 不参与调度，只有 `instances` 计槽（p50 差 177×、吞吐差 238×） |
| #221 | P1 | params 无大小闸门：≥128KiB 必失败（ENV 上限）、≥1MiB 打死连接、hello >32KiB 连不上 |
| #222 | P1 | 租约重派在旧进程仍活着时重复执行同一任务（已复现） |
| #223 | P2 | 派发热路径 7.74ms/task（8 槽闭环达成 129 task/s）、`GET /v1/nodes` 是 2N+ 条 SQL |
| #224 | P1 | API Key 每请求重算 argon2id(64MB) → 16 并发 RSS 1.4GB、延迟 790ms（可能与 A 路重复） |

## 3. 六个场景的原始数字

### S1 · N 个 agent 并发接单（`s1-nodes-scaling.sh`，N=1/4/16/64）
| N | 握手到全 online | `GET /v1/nodes` RPS / p50 / 响应体 | 波 A（N 任务铺满槽） | 波 B（4N 任务，超槽） | goroutine | RSS |
|---|---|---|---|---|---|---|
| 1 | 17ms（17ms/节点） | 1863 / 4.09ms / 579B | 27ms（37 task/s） | 4 任务 9.67s | 7→9 | 24.5→35MB |
| 4 | 140ms（35ms/节点） | 960 / 8.07ms / 2,166B | 41ms（98 task/s） | 16 任务 4.56s | 7→15 | 24.8→35MB |
| 16 | 202ms（12.6ms/节点） | 361 / 21.8ms / 8,521B | 4.11s（p90 4.08s） | 64 任务 5.02s | 7→39 | 27→35MB |
| 64 | 347ms（5.4ms/节点） | **104 / 76.0ms / 33,961B** | 2.85s（p90 2.70s） | 256 任务 5.24s | 7→135 | 31→38MB |

读法：**握手很便宜**（N=64 只用 347ms，5.4ms/节点，hub 无锁竞争）；
**昂贵的是 `GET /v1/nodes`（2N+ 条 SQL 的 N+1）与每次派发**（见 #223）。
波 A 的墙钟**不是单调的**（N=16 是 4.11s、N=64 是 2.85s），因为 N=16 那次有 2 个任务掉进了
5s 巡检尾巴（p90 = 4.08s）。所以**不能**用这两行推出"加节点更快/更慢"；
能确定的只有：单次派发成本 ~7.7ms（#223），与节点数无关。

### S2 · assign → result 往返（`s2-roundtrip.sh`）
见上表 ③ 引用；完整四配置：

| CONFIG | instances×concurrency | 串行 e2e p50 | 32 并发 p50 | 32 并发 p99 | 墙钟 | 吞吐 |
|---|---|---|---|---|---|---|
| a | 1 × 1 | 15.7ms | 24,483ms | 89,344ms | 89.55s | 0.36 task/s |
| b | 1 × 8 | 18.0ms | 9,322ms | 59,241ms | 59.50s | 0.54 task/s |
| c | 8 × 1 | 15.8ms | 52.6ms | 79.4ms | 0.25s | 128.4 task/s |
| d | 8 × 8 | 15.9ms | 50.2ms | 76.8ms | 0.23s | 138.7 task/s |

端到端拆解（串行、8 槽）：**派发 fire→开始执行 14.1ms（90%）**、执行+回报 1.6ms（10%）。
⇒ 延迟几乎全在"中枢把任务交给节点"这段，不在执行也不在结果回传。

### S3 · 槽位饱和（`s3-slot-saturation.sh`）
```
基线：goroutine=9 RSS=26,668KB cpu=0.05s db=4,096B
阶段1 触发 100 个任务（P=10）耗时 0.43s；1s 后：pending=75 succeeded=25
排空速率：30s 完成 36 个 → 1.20 task/s（单槽理想值 1/15.7ms ≈ 64 task/s）；同期 server CPU 0.26s（0.9%）
阶段2 再打 900 个（P=20）耗时 3.08s；pending≥500；DB 1.34MB；goroutine=9
积压期 CPU：30s / 0.44s（1.47%）
队列上限：没有上限 —— 1000 条事件全部 200，无 429/5xx，abandoned=0 failed=0
内存：RSS 26.7→37.4MB（1000 条任务 ≈ +10MB，1.3MB DB）
```
读法：**服务端不堆内存、不堆 goroutine**（积压在 SQLite 里，agent 侧也看不到它们），
但**排空速度被 5s 节拍钉死**，积压可以无限增长（#219）。

补充（勘误）：这 36 个不是"每 5s 一个"，而是每轮 tick 里**顺序尝试最多 100 条 pending、
槽位中途释放就继续派**（`dispatched` 6 轮合计 `9+5+8+4+7+3 = 36`，与完成数完全吻合）。
所以真实吞吐 ≈ `每轮成功派发数 / 5s`，限制来自"5s 才开一次窗口"+"每次尝试 ~40ms"。
500 条积压期实测 server CPU 仅 1.47%、fire 请求 p50 39.3ms / p99 74.4ms（由 server.log 重算），
**没有**观察到连接饱和——"会把 SQLite 连接占满"只是推断，未证实。

### S4 · 大 payload（`s4-payload.sh`）
| params | HTTP | 任务终态 | 关键错误 |
|---|---|---|---|
| 1KB | 200 | succeeded | — |
| 100KB（body 102,447B） | 200 | succeeded | — |
| 131,047B | 200 | succeeded | — （边界内最后一档） |
| **131,119B（131,072 起）** | 200 | **failed** | `fork/exec /usr/bin/sh: argument list too long` |
| 512KB / 900KB | 200 | failed | 同上 |
| **1,048,623B** | 200 | assigned | **连接被打死**：`StatusMessageTooBig … read limited at 1048577 bytes`，节点掉线 |
| 2MB | 200 | pending（attempts=0） | **不是**节点离线（`online=1`）：上一个 1MiB 任务把唯一槽位钉死了（= #218 机制） |
| 4b：command 40KB 的 node.yaml | — | 节点从未 online | `read limited at 32769 bytes`（服务端默认上行帧上限 32KiB） |

内存：RSS 26.6→46.8MB；goroutine 9 恒定。⇒ 这里不是内存问题，是**没有闸门导致的确定性失败**（#221）。

**1MiB 那一档顺带复现了 #218**：连接被 1009 关掉、节点重连后，那条 1MiB 任务仍停在 `assigned`
且 `node_instances.busy_task` 被它占着（查保留库确认），于是 2MB 那条永远排不出去。
这是一条**不用 kill agent** 的 #218 复现路径（一颗 1 槽节点 + 一次 1MiB payload 即可）。

### S5 · 慢节点与租约重派（`s5-lease-dup.sh`）
- **5a（`sleep 60` + `timeoutSec:5`）**：`status=failed`、`error=timed out after 5s`、
  `node.ws.result=1`、`attempts=1`、无残留 `sleep 60`、**不重复**。fire→终态 5.20s。
- **5b（`sleep 60` + `timeoutSec:200` + SIGSTOP 心跳饥饿）**：marker 2 行（同一 task_id）、
  `sleep 60` 峰值并发 **2**、`attempts=2`、`SIGSTOP→回收 34.4s`、`回收→agent2 在线 10.1s`、
  `agent2 在线→二次执行 9.8s`、fire→终态 114.3s ⇒ **重复执行**（#222）。

### S6 · 断连重连 ×10（`s6-reconnect.sh`）
```
round  goroutine  RSS(KB)  fd  online
0      9          24484     11  1
1..10  9          26748→31204  11  1      ← goroutine 与 fd 完全不动
泄漏判读：goroutine 9→9（Δ0）；fd 11→11（Δ0）；RSS 24.5→34.2MB（Δ9.7MB，Go 未归还 OS）
任务状态分布：acked=2 pending=58（见 #218：2 个孤儿任务把 2 个槽钉死，58 条永不派发）
node.ws.hello 11 次 / node.ws.closed 10 次 / node.ws.result 0 次 / lease.sweeper.reclaimed 0 次
```
读法：**没有 goroutine / fd / 会话泄漏**（这点是干净的）；
真正的问题是 #218 的死锁 —— **第 1 轮就把 2 个槽位钉死**（marker 只有 2 条执行记录，
即那 2 条 `acked` 任务；后续 9 轮全部空转，什么也没派出去）。

### 补充 · 派发热路径 profile（`s1b-profile.sh`）
```
churn 101 轮 × 32 任务 = 3232 次派发；完成 2584；窗口 CPU 16.15s / 20s
吞吐 129.2 task/s = 7.74ms/task（闭环负载下的**达成值**，非开环饱和值）
flat:  Syscall6 11.29% | runtimeNow 5.12% | nanotime 4.99% | _sqlite3VdbeExec 2.43%(cum 22.89%)
cum :  database/sql.withLock 57.95% | nodeSession.readLoop 31.00% | Dispatcher.Dispatch 30.19%
```
对照基线（#217 给的 `/health -c 32`）：46,691 RPS / p50 250µs。⇒ 真实派发路径比无 DB 上限低
**约 361×（2.6 个数量级）**。

## 4. 未完成 / 未确认

1. **时钟读取占 10% CPU**（`time.runtimeNow` + `runtime.nanotime`）—— 未用 `-list` 定位到具体调用点，
   猜测是 slog 每条日志打时间戳 + `store.Now()`，**未确认**。
2. **mutex / block profile 采集不到**：服务端没有 `SetMutexProfileFraction` / `SetBlockProfileRate`
   （`grep` 全仓为空），所以「hub 锁」这一项只能用 goroutine 数与吞吐间接判断（hub 是 RWMutex，
   N=64 握手 347ms 说明它**不是**瓶颈）。要精确量化需要临时插桩，本轮按纪律未改代码。
3. **loadavg 3.9~7.8 的污染**（A/C 两路并行）已在前言声明；建议在无其它两路时重跑 S1/S2 复验绝对值。
4. **64 节点以上的规模**未测（64 个 agent 进程已占 ~1.3GB；再往上要换机器）。
5. **重复执行**只在"心跳饥饿 + 同 token 重连"这条路径上复现；「网络半开（RST 丢包）」路径未单独构造。
6. #224（API Key argon2）可能与 A 路重复，等上级合并。

## 5. 独立审查与勘误记录

本报告与 7 条 issue 都经过一个**未参与实现**的只读审查者逐项核对（数字回原始输出、归因回源码行号）。
审查结论：**无 major finding**（7 条 issue 的核心结论全部成立），但抓出以下数值/表述缺陷，**已全部订正**：

| # | 原稿 | 订正后 | 位置 |
|---|---|---|---|
| 1 | #224「argon2 每次分配两块 64MB = 128MB/请求」 | **一块 64MiB**（`initBlocks` 单次 `make`；heap 448MB/64MiB = 7 个在飞）；"50 并发 OOM" → ~100 并发量级 | #224、本报告 §2 |
| 2 | #219「稳态吞吐 = 槽位/5s = 0.2 task/s 理论下限，实测 1.2 靠突发重叠」 | 一轮 tick 内顺序尝试最多 100 条、槽位中途释放就继续派（`dispatched` 合计 36 = 完成数） | #219、§3 S3 |
| 3 | #221「2MB 那条：节点不在线」 | **`online=1`**，是 1MiB 那条把槽位钉死（= #218），并作为 #218 的独立复现路径 | #221、§3 S4 |
| 4 | #220「128× 差距」「1×8 的改善是本地信号量的间接效应」 | **177×（p50）/ 238×（吞吐）**；1×8 相对 1×1 的差异落在噪声内（同形态实测 0.36 vs 1.20 task/s） | #220、§1②、§2 |
| 5 | 本报告「`git diff origin/main` 为空」 | 相对 `01a3f1f` 为空；相对 origin/main 仅差地基提交那 2 个文件 | 本报告前言 |
| 6 | #223「129 task/s 天花板 / 低 3 个数量级」 | 「闭环达成值」/ **361×** | #223、§3 补充 |
| 7 | #219/#224 引用「fire p50 37.7ms / p99 73.8ms」 | 由 `s3/out/server.log` 重算：**p50 39.3ms / p99 74.4ms** | #219、#224 |
| 8 | #224「/v1/events p50 790ms（服务端视角）」 | 两次独立运行的 server 侧数字：**580ms / 790ms**（客户端原始输出未保留） | #224 |

审查同时确认（记为"应记功"的地方）：测量污染披露（loadavg 3.92~7.83 与报告前言一致）、
`time.runtimeNow` 明确标注"未确认"、mutex/block profile 采不到也如实写"未确认"、
没有把"时序常量可调"当缺陷。

审查指出的**未修**项（诚实登记）：
1. `pb_build` **默认**仍按"二进制存在"决定是否重建（`lib.sh:71-77`）→ 换 commit 复现会静默用旧二进制；
   现在有 `PB_REBUILD=1` 可强制重建（已实测走通从零构建），但默认行为不构成机制性保证。
2. #224 没有独立脚本（手工 loadgen + `watch /proc/<pid>/status` + `curl heap`），
   不满足 #217 的"原样可跑"标准。
3. S6 的判据是"等全部任务终态"，超时只打 `!!` 不返回非零 → 作为回归用例不会自动变红
   （这也是 #218 缺少自动化回归的现状）。
4. 记录在案的运行里有些用了 `PORT=` 覆盖（S2 的 b/c/d 跑在 7920，s1 的 N=1/4/16 跑在 7921/7924/7936），
   所以"issue 里的端口"与"日志里的端口"可能不一致；脚本默认值本身是正确的。
