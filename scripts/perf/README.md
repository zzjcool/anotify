# 性能评估工具与场景

本目录只放**评估用**的工具，不参与构建产物。目标：任何人在任何机器上
`go run` 就能复现同一个压测，不需要装 wrk / hey / vegeta。

## 工具

### 1. 压测：`scripts/perf/loadgen`

```bash
go run ./scripts/perf/loadgen -url http://127.0.0.1:7900/health -c 32 -d 10s
go run ./scripts/perf/loadgen -url .../v1/hooks/<token> -method POST \
    -body-file /tmp/evt.json -header "content-type: application/json" -c 16 -d 30s
```

输出：总请求 / RPS / 状态码分布 / 失败按类别归并 / 延迟 p50-p90-p95-p99-max。
零依赖（只用标准库），所以数字可复现、可对比。

### 2. 采样：`/debug/pprof`（`ANOTIFY_PPROF=1` 打开）

```bash
ANOTIFY_PPROF=1 ANOTIFY_ADDR=:7900 ANOTIFY_DB=/tmp/perf.db ./anotify &

# CPU 30s
curl -s -o /tmp/cpu.pprof "http://127.0.0.1:7900/debug/pprof/profile?seconds=30"
go tool pprof -top -nodecount=25 /tmp/cpu.pprof
go tool pprof -list='<函数名>' /tmp/cpu.pprof

# 堆 / goroutine / 锁
curl -s -o /tmp/heap.pprof http://127.0.0.1:7900/debug/pprof/heap
curl -s "http://127.0.0.1:7900/debug/pprof/goroutine?debug=1" | head -40
curl -s -o /tmp/mutex.pprof http://127.0.0.1:7900/debug/pprof/mutex
```

默认关闭（pprof 会泄露栈与堆分布，且 CPU 采样本身有 ~5% 开销）。

## 三路并行时的测量纪律

12 核机器上跑三路压测会**互相污染**。约定：

```bash
taskset -c 0-3   ...   # A：HTTP / SQLite
taskset -c 4-7   ...   # B：WS 节点 / 多跳链路
taskset -c 8-11  ...   # C：资源与泄漏 / 前端
```

采样时也要 `taskset`，否则 profile 里混进别人的 CPU。

## 场景清单

### A. HTTP API 与存储层

| 场景 | 命令要点 | 关注 |
|---|---|---|
| `/health` 上限 | `-c 32 -d 10s` | 基线（无 DB 访问） |
| 读：能力/事件/任务列表 | 登录拿 cookie，`-c 16/32/64` | 分页与 `namespaceId` 过滤的代价 |
| 写：webhook 派发 | `POST /v1/hooks/<token>` 包装体 | SQLite 写锁、trace/task 插入 |
| 混合读写 | 两路 loadgen 同时跑 | 读会不会被写饿死（SQLITE_BUSY / 5xx） |
| 大响应 | `/v1/events?limit=500` | 序列化与内存放大 |

### B. WS 节点与多跳链路

| 场景 | 关注 |
|---|---|
| N 个 agent 并发接单 | 握手成本、hub 锁、assign 扇出 |
| assign → result 往返 | 端到端延迟 vs 任务数 |
| 槽位饱和（`instances`/`concurrency`） | 是否排队而不是丢帧 |
| 大 payload（params 1MB） | 帧大小限制、内存峰值 |
| 慢节点（sleep 命令） | 租约到期是否**重复执行** |
| agent 断连重连 | 是否泄漏 goroutine / 会话 |

### C. 资源、泄漏与前端

| 场景 | 关注 |
|---|---|
| 持续 10 分钟写入 | RSS、goroutine 数、fd 数、DB 文件与 WAL 大小 |
| 事件保留未开 | 无界增长（这是已知设计取舍，量出来写进 issue） |
| 静态资源 | `Cache-Control` / ETag / 304 命中率 |
| 页面加载 | playwright 抓 `performance.timing` 与大列表渲染 |

## 两条反直觉的结论（实测得出，先看这两条再开工）

### 1. 绝对值不可跨场次比较，只能用**同场次比值**

`#217` 里记录的 46,691.9 RPS（`/health`）是 **12 核、不 pin、单跑** 的结果；
三路并行 + `taskset -c 0-3` 下**同一条命令**只有 **14.9k–28.8k**。

复跑实证：绝对 RPS 在两次场次之间漂 **1.6–2.4×**，而**比值稳定在 ±1.5 个百分点**。

所以：

- 报告「A 是 B 的 N 倍」**可以**；报告「A 有 X RPS」**不行**（除非同场次、同分核、同负载）
- 对比修复前后时，必须在**同一次会话里跑对照组**，而不是拿上周的数字比自己

### 2. `SetMaxOpenConns(1)` 之下**永远不会出现 `SQLITE_BUSY`**

连接池只有 1 条连接时，并发请求在**应用层排队**，根本到不了 SQLite 的锁竞争。
于是：

- 看到「吞吐只有 `/health` 的 x%」+「`database is locked` / `SQLITE_BUSY` = 0」，
  **不要去找锁**，去看 `sql.DB.conn` 的等待（`/debug/pprof/goroutine` 里大量阻塞在 `sql.DB.conn`）
- 反过来，把池放开之后再出现 `SQLITE_BUSY` 才是真的锁问题（那时才需要 `_txlock` / `busy_timeout`）

### 3. 已知未确认项

- 同数据量下读 RPS 摆动 **3.3–4.2×**，怀疑 SQLite page cache（默认 2MB）小于工作集，
  但**缺 pager 命中证据**（未验证）。若后续要压这个数，先量 `PRAGMA cache_size` 与命中率。

## 测量纪律：先确认你跑的是**这棵代码**编出的二进制

这不是理论风险——本轮真的发生过：验证 #255 的「重复执行 2→0」时，
直接跑 `scripts/perf/b/s5-lease-dup.sh` 得到「仍然重复执行 2 次」，差点据此判定修复无效。
真相是 `scripts/perf/b/lib.sh` **默认不重建**（只要 `$PB_BIN` 里已有可执行文件就直接用），
而 `$PB_BIN` 里躺着的是**修复前**编的二进制。

```bash
# 对比修复前后时，必须显式要求重建（或清掉缓存目录）
PB_REBUILD=1 bash scripts/perf/b/s5-lease-dup.sh
rm -rf /tmp/perfB/bin && bash scripts/perf/b/s5-lease-dup.sh
```

**自查三件事**（做任何"前后对比"之前）：

1. 二进制 mtime 是否晚于你要验证的那个 commit
2. `git rev-parse --short HEAD` 与你要验证的分支是否一致（worktree 之间很容易串）
3. 数字如果"没变化"，**先怀疑二进制没换**，再怀疑修复无效

同理适用于 `-race` 与 profile：**旧二进制 + 新结论 = 最危险的组合**。
