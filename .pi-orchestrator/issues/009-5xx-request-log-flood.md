---
id: 009
kind: chore
area: [server]
size: S
severity: P2
state: open
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/server/... -count=1"]
opened: 2026-11-16
---

# 009 · 每个 5xx 都打一条请求日志 —— 背压期间 1.2 GB / 600s

## 现象

perf soak 期间（120–600s 全是 503）：

| 时长 | `server.log` | 速率 |
|---|---|---|
| 180s | **152 MB** | 503 段 ~2.5 MB/s |
| 600s（推算） | **≈1.2–1.3 GB** | 12.4k 行/s × ~200B |

每行是 `slog.Error("request", status:503)`（`internal/server/middleware.go`）。
背压时的「拒绝」恰恰是**最不需要逐条留痕**的一类流量：它本身就是一条计数信号。

## 风险

- 压测/事故现场把磁盘写满（本次已把 `perf-b5` 目录顶到 GB 级，跑满 600s 前必须看余量）；
- 日志轮转在 `pc_start` 里不可行：`>server.log` 由服务进程持有 fd，
  脚本侧 truncate 会在偏移处挖 NUL 洞。

## 建议修法（择一，倾向 A）

| 方案 | 说明 |
|---|---|
| **A. 5xx 采样/降级** | 5xx 请求日志按窗口采样（如首次 + 每 N 条一次，其余只计数），或把背压 503 从 Error 降为 Debug + 一个聚合计数（`backpressure_rejected_total`） |
| B. 聚合信号 | 保留「每类状态码首次出现」一条 + 周期汇总一条（含计数、均值延迟） |
| C. 支持重开日志 | `pc_start` 侧发 `SIGUSR1` 让服务重开日志文件（改动大，且不是所有部署都有 logrotate） |

## 验收

- 背压期间日志体积下降一个数量级（目标：600s 全 503 场景 < 100 MB），
  但**运维仍能看到「发生了多少、什么时候开始」**（计数/汇总必须在）。
- 正常 2xx/4xx 日志行为不变（不要为了省盘把可观测性一起删了）。
- 有单测钉住「采样阈值 + 首条必留」的行为。

## 不做

- 不整体降 `ANOTIFY_LOG_LEVEL`（那是把别的告警一起关掉）。
