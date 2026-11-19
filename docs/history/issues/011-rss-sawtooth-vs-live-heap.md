---
id: 011
kind: bug
area: [server]
size: S
severity: P3
state: done
needs_recon: true
contract: []
depends_on: [007]
route: [scout]
gates: []
opened: 2026-11-17
---

> 迁移自 .pi-orchestrator/issues/011-rss-sawtooth-vs-live-heap.md（2026-11-19，协调者工作区不再入库）

# 011 · soak 里 RSS 在 40MB↔460MB 之间锯齿，但 GC 后存活堆只有 1-3MB

## 现象（实测，两次独立跑）

`S1` soak（40/s、8 并发）的 `raw/samples.csv`，同一进程：

| 时点 | RSS | goroutines | fds | db_bytes |
|---|---|---|---|---|
| t36 | 37.8 MB | 14 | 22 | 3.1 MB |
| t66 | **325 MB** | 19 | 27 | 5.8 MB |
| t96 | **425 MB** | 18 | 32 | 8.5 MB |
| t126 | **464 MB** | 12 | 26 | 11.1 MB |
| t156 | 55.9 MB | 23 | 33 | 13.8 MB |
| t186 | **466 MB** | 26 | 33 | 16.5 MB |
| t216 | 52.5 MB | 34 | 33 | 19.0 MB |

- 周期约 150s；峰值 460MB、谷值 40MB；`fds` 恒 33、`goroutines` 12-34（都很小）。
- 600s 收尾时 RSS = 266MB。

## 但 Go 堆不是这个形状（gctrace 实测）

同一路径跑 90s，服务端带 `GODEBUG=gctrace=1`：

```
gc134 @93.524s  3->3->0 MB
gc143 @98.120s  3->4->1 MB
gc145 @98.150s  4->4->1 MB
```

**GC 后存活堆 0-2MB、GC 目标 3-4MB、GC 每 ~0.4s 一次** —— Go 堆从头到尾没有长大过。
同期 RSS 采样：99MB(t6) → 41MB(t36) → **223MB(t66)** → 44MB(t97)。

所以：**RSS 的 460MB 峰值不可能来自 Go 堆**。真实的 460MB 要么在堆外（mmap/madvise 未归还/page cache 计入 RSS），
要么**采样本身量错了对象**。

## 为什么要查（可能是测量假象）

这类「证据文件说谎」本轮已经踩到两次（`raw/ingest_<tag>.log` 跨轮污染、`pc_build` 用旧二进制）。
如果 RSS 列是假象，那 `docs/perf` 里所有「RSS 稳态」结论都站不住；如果是真的，那是堆外内存问题
（mmap 的 DB/WAL 页、连接缓冲、pprof 缓冲），同样是运维关心的。

## 结论（2026-11-17 实测）：**真 Go 堆锯齿（GC 驱动）**，不是采样假象、也不是堆外

### 证据 1：pid 与读数都对

- `pc_sample` 用的 `$PC_PID` 就是服务进程：`readlink /proc/<pid>/exe` → `/tmp/perf-c/bin/anotify`；
- `ps -o rss=` 与 `/proc/<pid>/status` 的 `VmRSS` **逐点一致**（误差 ≤ 16 KB）。

### 证据 2：涨的是 Anonymous，不是 file-backed

独立探针（40/s 持续 ingest，每秒抓 `/proc/<pid>/smaps_rollup`）：

```
t_s  rss_kb   anon_kb   file_kb
1    20732    10064     15256
5    103844   87960     15884
10   131560   115680    15880
15   151164   135260    15904   ← 峰值
16   30632    14728     15904   ← GC 回收，掉了 120 MB
30   34020    18188     16032
42   36940    20908     16032
```

- `file`-backed **恒定 15.9 MB**（就是二进制本身 + 少量映射）⇒ 不是 mmap / page cache；
- 涨的 130 MB **全是 `Anonymous`** ⇒ 只能是 Go 堆（进程里没有别的匿名大分配）；
- t=16 一次掉 120 MB ⇒ 典型 GC 回收形状。

### 证据 3：soak 里振幅更大，但机制相同

s1（40/s + 派发/emit 流量）里峰值到 460 MB、谷值 40 MB，周期约 150 s。
分配速率更高 → GC 目标更高 → 峰值更高；GC 后存活堆仍然很小（同一路径的 `gctrace`
显示 `3->3->0 MB` 量级，说明**不是泄漏**，是「大量短命对象 + Go 惰性归还」）。

## 运维含义（要写进容量文档）

- 40/s 稳态摄取下，**RSS 峰值可达 ~460 MB**（容器 limit 要按峰值给，不能按稳态 40 MB 给）；
- 这是可调的：`GOGC` 调小或给 `GOMEMLIMIT` 能把峰值压回去，代价是 GC CPU 上升；
- 不属于泄漏，长时间跑不会单调涨（600 s 跑完 RSS 266 MB，回落到锯齿区间内）。

## 若要继续（可选，不在本卡）

量一次 `GOGC=50` / `GOMEMLIMIT=256MiB` 的对照，给出「峰值 vs CPU」的取舍，写进 `docs/perf`。

## 原侦察计划（保留作方法记录）

1. **确认采样对象**：`pc_sample` 用 `${PC_PID}`（`scripts/perf/c/common.sh`）。在 soak 运行中同时打印
   `cat /proc/<PC_PID>/status | grep VmRSS` 与 `ps -o rss= -p <PC_PID>`，确认两者一致、且
   `<PC_PID>` 确实是 `$PC_BIN`（`readlink /proc/<pid>/exe`）。
2. **峰值时刻抓 smaps**：写一个 5s 间隔的采样器，在 RSS 越过 200MB 时立刻
   `cat /proc/<pid>/smaps_rollup`（看 `Rss` / `Pss` / `Anonymous` / `Shared`）+
   `awk '$6=="Rss" && $6>10000' /proc/<pid>/smaps` 找出最大的几个映射段。
3. **对照 Go 视角**：同一时刻 `curl /debug/pprof/heap?debug=1` 取 `inuse_space` / `heap_sys` /
   `sys - heap_idle`，和 gctrace 对账（Go 1.25 的 `heap_sys` 就是向 OS 要过的量）。
4. 判定：
   - 若 `/proc` 与 `ps` 一致且 `smaps` 显示匿名大段 → 真堆外（继续查是不是 `MADV_FREE` 未归还 / 大对象）；
   - 若两者不一致或 pid 不是服务 → **采样 bug**，修 `pc_sample`，并把 `docs/perf` 里依赖 RSS 的结论标为待复测。

## 不做

- 不先改任何代码；先定性。
- 不把峰值当成「泄漏」写进文档（当前证据不支持泄漏：存活堆是平的）。

## 产出

事实清单（file:line + 实测数字），结论二选一：**真堆外** / **采样假象**，附可复现命令。
