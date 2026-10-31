# 性能评估 · A 路：HTTP API 与存储层（#217）

> 范围：HTTP API（读路径 / 写路径 / 大响应）+ SQLite 存储层（连接、往返、排序、锁）。
> 环境：`taskset -c 0-3`，端口 7900-7919，DB 各自 `mktemp -d`；**不改产品代码**（本文只记录测量与归因）。
> 原始数据：每个数字都能在文末「复现附录」的命令下重新跑出来；测量当时的原始输出落盘在
> worker 的 `/tmp/perf-a/logs/`（不入库，命令可重跑）。

## 0. 一句话结论

在 4 核（`taskset -c 0-3`）口径下，**无 DB 的 `/health` 能到 24k–28.8k RPS，而任何一个带会话的读端点只有它的 0.3%–11%，
机器入口更低到 0.11%（`GET /v1/events` 走 Bearer），写入口（`POST /v1/events` 走 Bearer）0.13%–0.14%**。三层原因按影响从大到小：

1. **`store.Open` 把 SQLite 连接池钉成 1 条**（`internal/store/store.go:46 SetMaxOpenConns(1)`）：
   所有读请求在同一连接上排队，并发 1→64 吞吐只放大 1.24–1.55 倍（`/health` 是 4.23 倍）；
   混合读写时**写被读饿死（-93%）**，且这类实现里根本没有 `SQLITE_BUSY`，只有队列。
2. **API Key 鉴权每请求重算一次 argon2id（m=64MB, t=1, p=4）**（`internal/auth/apikey.go:132`）：
   机器入口被钉在 ~32–41 RPS、CPU 4 核打满、每请求分配 64MB（c=16 时 inuse 768–960MB）。
3. **列表端点的 N+1 查询**：`/v1/nodes` 每节点 2 次额外查询（`console_handlers.go:1016 viewFor`）→
   100 节点 79–85 RPS（`/health` 的 0.30%），p50 91–385ms。

另外两个固定开销：每个带会话请求**固定 4 次鉴权 DB 往返**（含一次**同步 UPDATE**），以及**同一条 SQL 每请求重新 parse**（26% 读路径 CPU）。

## 0.1 本轮开的 issue（每个瓶颈一条）

| issue | 标题（简） | 严重度 | 关键数字 |
|---|---|---|---|
| [#240](https://github.com/zzjcool/anotify/issues/240) | `SetMaxOpenConns(1)` 让只读查询也串行排队 | **高** | 读路径 2.5%–11% of `/health`；c=1→64 吞吐 ×1.24–1.55（`/health` ×4.23）；30–32 goroutine 阻塞在 `sql.DB.conn` |
| [#241](https://github.com/zzjcool/anotify/issues/241) | 每带会话请求固定 4 次 DB 往返（含同步 TouchSession 写） | 中 | auth 链占读端点 CPU 12.7%–34.0% |
| [#242](https://github.com/zzjcool/anotify/issues/242) | 每条 SQL 每请求重新 prepare/parse | 中 | `prepareV2` 占 CPU 26.1%–26.4% ≈39–43µs/条；20s 解析十几万次 |
| [#243](https://github.com/zzjcool/anotify/issues/243) | 大响应无 gzip + 全量排序 + payload 重复压缩 | 中 | `limit=500` = 209KB / 7.66 CPU 秒每千请求 / 209 RPS；可压 10.3× |
| [#245](https://github.com/zzjcool/anotify/issues/245) | `/v1/hooks/{token}` 是 1 RPS/IP 天花板 + 429 双份日志 | 中 | 成功数恒为 60/10s，99.9% 是 429；429 双份日志 75MB/10s |
| [#239](https://github.com/zzjcool/anotify/issues/239) | `/v1/nodes` 每请求 2N+1 次串行查询（**已 CLOSED，并入 #223**） | **高** | 20→100 节点 p50 ×4.87（理论 ×4.90）；100 节点 79–85 RPS |

与别路重复、**只在对应 issue 上补证据、不另开**的：

| 已有 issue | A 路补充的证据 |
|---|---|
| [#227](https://github.com/zzjcool/anotify/issues/227) / [#224](https://github.com/zzjcool/anotify/issues/224) argon2 | Bearer 31.6–39.0 RPS vs 同数据同 SQL 的 Cookie 917.7/895.9 RPS（慢 23–29×）；CPU/请求 102–126ms（60–76×） |
| [#223](https://github.com/zzjcool/anotify/issues/223) nodes 2N+1 | 节点数斜率 + profile（`viewFor` 65% = `ListNodeCapabilities` 41% + `ListInstances` 24%） |
| [#230](https://github.com/zzjcool/anotify/issues/230) / [#237](https://github.com/zzjcool/anotify/issues/237) 索引与排序 | `idx_events_retention` **仍被用作 namespace 过滤**，只是覆盖不了 `id`；扩成 `(namespace_id, created_at, id)` 后 1.28M 行 LIMIT 1 从 2.79ms → 0.57ms |
| [#229](https://github.com/zzjcool/anotify/issues/229) 事件无保留期 | 每次写落 **2 行**（50.0% 是 `source='route'` 剪枝留痕）⇒ 磁盘账要 ×2 |

## 1. 测量条件（读这些数字前必须先读这一节）

| 项 | 值 |
|---|---|
| 机器 | AMD Ryzen 5 4600H，12 逻辑核；本轮三路 perf 并行（A:0-3 / B:4-7 / C:8-11） |
| 被测二进制 | `go build -trimpath -tags embeddist -o /tmp/perf-a/anotify ./cmd/server`（go1.25.0，未 strip，供 pprof 归因） |
| 压测器 | `go build -o /tmp/perf-a/loadgen ./scripts/perf/loadgen`（仓库自带零依赖压测器） |
| 分核 | server 与 loadgen **都在 `taskset -c 0-3`** |
| DB | `go run ./cmd/devseed -db $(mktemp -d)/a.db -username <name>`；`/tmp` 是 **tmpfs**（无磁盘 I/O 成本） |
| pprof | `ANOTIFY_PPROF=1`，压测进行中 `curl /debug/pprof/profile?seconds=15~20` |
| 采样 | 每个数据点 **3 次取中位数**，记 min–max；每块前后夹一次 `/health -c 32 -d 3s` 作同场次参照 |

### 1.1 为什么结论一律用「比值」而不是绝对值

- #217 的基线（46,691.9 RPS）是**12 核不 pin、单跑**量的；本轮 server + loadgen 挤在 4 核里，
  同一条命令实测 **14.9k–28.8k RPS**（四条不同时段的夹逼参照，最低 14,975.7、最高 28,841.7），且随另外两路的负载漂移（采样期 `loadavg(1m)` 在 **2.9–18.4** 之间）。
- 三个 worker 的参照带：`/health -c 32` = **14.9k–28.8k**（同一二进制、同一命令行！）。
- 所以：**绝对值只作量级参考，所有结论用「目标端点 ÷ 同块 `/health` 参照」的比值**，比值在 ±9% 内可复现。
- 另外：`/health` 本身是**客户端受限**的（服务端只占 1.64–1.69/4 核），它不是「服务端天花板」，
  只是「同一批 4 核下 loadgen 能把服务端打多高」。真正的服务端单位成本用「CPU 秒/千请求」看。

## 2. 基线校准（S1）：`GET /health`

| 并发 | RPS 中位 (min–max) | p50 | p99 | 服务端 CPU 核占用 | CPU 秒/千请求 |
|---|---|---|---|---|---|
| 8 | 23,637.9 (22,983.0–23,892.5) | 0.256ms | 1.244ms | 1.64 | 0.0705 |
| 16 | 24,720.1 (24,662.6–25,290.9) | 0.508ms | 2.248ms | 1.69 | 0.0677 |
| 32 | 24,057.0 (23,718.2–24,620.6) | 1.012ms | 4.542ms | 1.67 | 0.0695 |
| 64 | 23,281.2 (23,269.3–23,721.8) | 1.978ms | 8.762ms | 1.67 | 0.0719 |

- **基线在不同时段能跑到 24.0k–28.8k**（机器安静时 28.8k），与 #217 的 46.7k 差距来自「4 核 pin + 三路并行」，不是回归。
- 服务端 CPU 恒定 1.64–1.69 核 ⇒ **吞吐被 loadgen 卡住**，`/health` 的真实服务端能力高于此值。
- `/health` 路径无 DB、无锁（`mux.go:395` 预 marshaled 固定字节）——**它仍是访问日志中间件 + HTTP 栈的成本**（实测访问日志对它开销 ≈0%，见 §5.2）。

## 3. 读路径（S2）：带会话的三个列表端点 + 机器入口

### 3.1 总表（中位数；比值 = 同块 `/health -c 32` 参照）

| 端点 | 鉴权 | c=16 | c=32 | c=64 | 相对 `/health` | CPU 秒/千请求 | p50@c32 |
|---|---|---|---|---|---|---|---|
| `/v1/tasks?limit=100` | Cookie | 1,847 | 1,851 | 1,819 | **11.1–11.3%** | 0.74–0.78 | 15.6ms |
| `/v1/capabilities` | Cookie | 1,421 | 1,477 | 1,353 | **7.4–8.1%** | 0.92–0.96 | 20.0ms |
| `/v1/console/events?limit=100` | Cookie | 507 | 441 | 489 | **2.5–2.9%** | 2.80–3.13 | 65.5ms |
| `/v1/console/events?limit=100` (`namespaceId=*`) | Cookie | 512 | — | — | 2.8% | 2.85 | 28.8ms |
| `/v1/events?limit=100`（机器入口） | **Bearer** | 31.6–39.0 | — | — | **0.11%** | 102–126 | 69.6ms+ |
| `/v1/nodes`（20 节点） | Cookie | 398 | 395 | — | 1.37% | 3.67 | 39.4ms |
| `/v1/nodes`（100 节点） | Cookie | 81.4 | 79.2 | — | **0.30–0.32%** | 17.1–18.3 | 190ms |

（`/health` 同场次参照 14.9k–28.8k；`/v1/console/events?limit=100` 与 `/v1/events?limit=100` 是**同一条 SQL、同一份数据**，
差的 23–29 倍主要来自鉴权（argon2 占该路径 ≈94% 的 CPU 样本；机器入口还多 2 空格缩进 JSON，字节 +29.4%）。）

### 3.2 串行瓶颈的取证（`SetMaxOpenConns(1)` 成立）

| 证据 | 数值 |
|---|---|
| c=32 压测中阻塞在 `database/sql.(*DB).conn` 的 goroutine | **30–32 个**（`/health` 对照 1 个） |
| 并发 1 → 64 的吞吐放大 | events **1.24×**、capabilities **1.46×**、tasks **1.55×**，而 `/health` **4.23×** |
| 同区间 p50 涨幅 | events 2.36ms→115.36ms（**49×**），`/health` 0.14ms→1.01ms（7.2×） |
| 服务端 CPU | 恒定 1.31–1.47/4 核 ⇒ **不是 CPU 受限，是排队** |
| c=1 的 p50 = 单请求串行服务时间 | capabilities 0.95ms / tasks 0.71ms / events 2.36ms（= 各自 RPS 的理论上限 928 / 1172 / 394） |

### 3.3 每个带会话请求固定 4 次鉴权 DB 往返 + 1 次目标查询

`auth/session.go:87 GetSession` → `auth/session.go:152 GetUserByID` →
**`auth/session.go:163 TouchSession`（是同步 UPDATE，注释写「异步」与代码不符）** →
`store/authz.go:27 AuthorizeNamespace`（EXISTS）→ 目标查询。
auth 链占读端点 CPU：**12.7%（events）～34.0%（tasks）**。

### 3.4 第二条 CPU 热点：同一条 SQL 每请求重新 parse

`modernc.org/sqlite.(*conn).prepareV2` → `_sqlite3Parser` → `_yy_reduce`：占 CPU **26.1%–26.4%**（capabilities / tasks），
折算 **≈39–43µs / 条 SQL**；同一批 SQL 文本（`SELECT ... FROM sessions WHERE id = ?` 等）在 20 秒里被解析约 18 万次。

### 3.5 `/v1/nodes` 的 N+1（20→100 节点斜率）

| 节点数 | c=32 RPS | c=32 p50 | CPU 秒/千请求 | 理论查询数 |
|---|---|---|---|---|
| 20 | 395 | 79.12ms | 3.67 | 41 |
| 100 | 79.2 | 385.30ms | 17.1–18.3 | 201 |
| **×5 节点** | **÷4.99** | **×4.87** | **×4.65–4.94** | **×4.90** |

实测/理论 = **0.96–0.99** ⇒ 成本几乎 100% 由「每节点 2 次额外查询」解释
（`console_handlers.go:1016 viewFor` → `ListNodeCapabilities` 41% + `ListInstances` 24%，`ListNodes` 本身只占 3.6%）。

### 3.6 机器入口 Bearer：每请求一次 64MB argon2id

| 项 | Cookie（控制台） | Bearer（机器入口） | 倍数 |
|---|---|---|---|
| RPS（c=16 / c=8） | 917.7 / 895.9 | **31.6 / 39.0** | **慢 23–29×** |
| CPU 秒/千请求 | 1.66–1.69 | **102–126** | 60–76× |
| 服务端 CPU | 1.4–1.5 核 | **4.00 核（打满）** | CPU 受限 |
| 每请求分配 | ~813KB（大响应档） | **64MB**（`argon2.initBlocks`） | c=16 时 inuse **768–960MB** |

`internal/auth/apikey.go:131 ValidateKey` → `verifyKey` → `argon2.IDKey(..., argonMemory=64MB, argonTime=1, argonThreads=4, ...)`，
而且成功路径还会 `TouchAPIKey` 写一次库。inline 常量在 `internal/auth/apikey.go:18-24`。

## 4. 大响应（S5）：`limit=500`

| 事件行（表行 = 2×） | limit | c | RPS | p50 | p99 | 响应字节 | CPU 秒/千请求 |
|---|---|---|---|---|---|---|---|
| 1 000 (2 000) | 100 | 16 | 918 | 16.44ms | 37.9ms | 41,693 | 1.70 |
| 10 000 (20 000) | 100 | 16 | 838 | 17.95ms | 42.5ms | 41,893 | 1.84 |
| 1 000 (2 000) | 500 | 16 | 212 | 69.72ms | 159.9ms | **208,260** | 7.66 |
| 10 000 (20 000) | 500 | 16 | 209 | 71.49ms | 175.4ms | **209,254** | 7.66 |

- **「10× 数据 → p50 暴涨」不成立**：p50 只 +2.5%~+9.2%；排序分量约 0.1ms/请求（≈6%）。
- 真正大的是**单请求体量**：209KB/请求、7.7ms CPU/请求、每请求累计分配 ≈813KB（**3.9× 放大**）。
- 归因：`ListEvents` cum **60.6%** + `writeJSON` cum **20.9%**，其中 `json.appendCompact` **11.4%** 全花在
  `json.RawMessage` payload 的重新压缩上；**无 gzip、无 ETag**（209KB 明文可压到 20.2KB，10.3×）。
- `ORDER BY created_at DESC, id DESC` 只被 `idx_events_retention(namespace_id, created_at)` 覆盖第一项，
  `EXPLAIN QUERY PLAN` 逐字出现 **`USE TEMP B-TREE FOR LAST TERM OF ORDER BY`**；实测 LIMIT 无法提前截断
  （170k 行时 LIMIT 1 ≈ LIMIT 100 ≈ 0.72ms；覆盖索引 `(namespace_id, created_at, id)` 后 1.28M 行 LIMIT 1 从 2.79ms → 0.57ms）。
  ⇒ **数据量再涨一个量级时它会从 6% 变成主导**，当前 1 万行还看不出来。

## 5. 写路径与混合读写（S3 / S4 / S6）

写路径必须**分三档看**，否则会把鉴权成本误当成派发成本：

| 档 | 端点 | 鉴权 | 并发 | RPS（中位） | p50 | 服务端 CPU/请求 | 核占用 | RSS 峰值 |
|---|---|---|---|---|---|---|---|---|
| 公开 webhook | `POST /v1/hooks/{token}` | token（**+60/min/IP 限流**） | 1 / 4 / 16 | 成功**恒为 60**；99.90%–99.97% 是 **429** | 141–574µs | 0.11–0.14ms | — | — |
| 机器入口 | `POST /v1/events` | **Bearer（argon2id 64MB）** | 1 / 4 | **37.6 / 41.2** | 26.2 / 95.8ms | 76.6 / 96.1ms | 2.88 → **3.96（打满）** | 161 → **561MB** |
| **纯净派发** | `POST /v1/sources/{id}/fire` | Cookie | 1 / 4 / 16 | **1,036 / 1,136 / 1,107** | 0.891 / 3.41 / 14.0ms | **1.25–1.30ms** | 1.25–1.39 | 41–42MB |

- **`/v1/hooks` 的天花板是 1 RPS/IP，不是性能问题而是限额**：每个 10s 窗口成功数恒等于 60，第 61 个开始 429（`Retry-After: 60`），
  固定窗口从首个请求起 60.08s 重置；5 请求/秒的突发 **11.96s 就吃光配额**（不是令牌桶）。限流键是 **IP**（反代/NAT 后所有调用方共享）。
- **429 会写两条 WARN 日志**（`ratelimit.hit` + accessLog `request` 429）⇒ c=16 时 10s 写 **75.1MB** 日志；
  把级别调到 `error` 后同样 429 负载的吞吐 22,540 → 25,878 RPS（**+14.8%**）。见 issue 07。
- 派发本身（会话鉴权）其实**很快**：1.25ms CPU/请求、1,036 RPS @c=1。c=1→16 吞吐只涨 6.9% 而 p50 涨 15.8× ⇒ 又是那条单连接。
- 每次写落 **2 行**：真事件 + `source='route'` 的剪枝留痕（10 万次 fire 后 `events_total=203,149`，其中 `route` 行 **50.0%**）。

### 5.1 混合读写（S4）：**写被读饿死，读几乎不受影响**

| 场景（同一 DB 状态，20s 窗口） | 读 `events?limit=100` (c=16) | 写 `fire` (c=4) |
|---|---|---|
| 孤立跑 | **828.6 RPS** / p50 17.97ms | **1,139.1 RPS** / p50 3.40ms |
| 同时跑 | **809.4 RPS（-2.3%）** / 18.43ms | **79.5 RPS（-93.0%，÷14.3）** / **48.57ms（×14.3）** |

- 小库对照（行数 ÷46）跌幅相同（写 1,168 → 62–85 RPS）⇒ **跌幅与数据量无关**，就是单连接被读占满。
- goroutine 栈（47 个）：**20 个阻塞在 `database/sql.(*DB).conn`**，其中 17 个能归到具体调用（GetSession 6 / GetUserByID 4 / AuthorizeNamespace 4 / TouchSession 1 / GetSource 1 / EnsureGraphVersion 1），
另 3 个停在进出 conn 的间隔里（栈上没有 store 调用帧）；同一瞬间只有 **1** 个在跑 SQLite。
- **`database is locked` = 0，`SQLITE_BUSY` = 0**（3.08M 行服务端日志）；271 条 5xx 全部落在压测收尾窗（`context canceled`），**稳态窗口 0 条**。
  ⇒ `SetMaxOpenConns(1)` 把「锁冲突」换成了「应用层排队」：**这类实现里不要去找锁，要找队列**。

### 5.2 访问日志成本（S6 A/B）：对 `/health` 是 0%，对会落盘的路径约 8%

| 档 | info / warn RPS 比 | CPU/千请求 | 每请求日志字节 |
|---|---|---|---|
| `/health` | **1.0064（0%）** | — | **0 B**（`middleware.go:84-86` 把 /health 降级成 DEBUG，info 级也不落盘） |
| 会落盘的 INFO 路径（404） | **1.0864（info 吞吐 -8.0%）** | 0.0779 vs 0.0667（+16.8%） | ~187 B |

- profile 佐证：`middleware.go:95 slog.Info` cum **3.23s（11.4%）** vs warn 档 **0.07s（0.26%）**。
- **429 路径单独采了一份 profile**（`/tmp/perf-a/logs/fix-hook429.cpu.pprof`，`ANOTIFY_LOG_LEVEL=error`、c=16、15s、窗口内 ≈334k 请求）：
  单请求 ≈**78µs CPU**，`time.Now` 23.84%（分散在 net/http 内部、`accessLog` 14.68% 份额、`fixedWindow.allow` 11.61% 份额）、写响应 `syscall.Write` ≈20.6%、
  `accessLog` 自身增量 ≈7.6%（19.18% cum 减它包住的 `ServeMux` 11.59%）、`rateLimited` 8.71%。**没有 DB 参与**（被限流提前拒绝）。
  这份 profile 是为了补 #245 的「profile 摘要」而采的（初次提交时该节是占位符，见 §8）。
- 判定：**不单独开 issue**。依据是同一批实测里它远小于其它固定开销：真实端点 CPU/请求 1.25–4.6ms，日志 ~187B/请求 折算下来只占 0.2%–0.7%；
  它只在「除了 HTTP 栈什么都不做」的 `/health` 上比例才显眼，而 `/health` 恰好被降级成 DEBUG 了。
  真正的问题是**被限流放大后的 429 双份日志**（见 issue 07）。

## 6. 未确认项

- **同一端点、同一数据量，读 RPS 在 218–271 ↔ 828–913 之间摆动 3.3–4.2×（p50 68ms ↔ 16ms）**，与采集顺序相关；
  慢态里 `ListEvents` 占 CPU 77%、`_sqlite3BtreeTableMoveto`（按 rowid 回表）23.3%（快态 61% / 7.0%）。
  怀疑 SQLite page cache 默认 2MB < 工作集（1.28M 行时更明显），**无 pager 级证据** ⇒ 未确认，未开 issue。
- `namespaceId=*` 在多命名空间下的 fan-out 斜率（本轮库里只有 1 个命名空间，测不出斜率）。
- `/v1/tasks` 的**有量**表现（无 edge / 无节点声明时 `POST /v1/events` 只落 `pruned.no_edge`/`no_node`，tasks 表恒 0 行
  ⇒ 现有数字是「空表下限」，**task 插入成本本轮没量到**）。
- `POST /v1/hooks/{token}` 的**去限流**吞吐（60/min/IP 是设计上限，不改代码无法越过；已用 `/v1/sources/{id}/fire` 量到纯派发上限）。
- 连接池放开后 `database/sql` 对 `*sql.Stmt` 的绑定行为与净收益（需改代码才能测，属后续调优）。
- mutex/block profile 不可得：Go 1.25 里 `blockprofilerate`/mutex fraction 默认 0，`/debug/pprof/{block,mutex}` 不读 `?rate=`，
  开它需要进程内插桩（违反「不改产品代码」），已用 goroutine 栈 + c=1 对比 + CPU cum 替代。
- 顺手发现的**观测性 bug（非性能，未开 issue）**：`internal/server/middleware.go:67` 取的是 `…Microseconds()`、`:76` 却把它写进名为 `latency_ms` 的字段，
  日志里 0.256ms 的请求记成 `latency_ms: 256`。建议另开一条 bug。

## 7. 复现附录

见每条 issue 的「复现命令」一节（原样可跑）。前提：Go 1.25、`curl`、`jq`（取 capability/hookUrl）、`sqlite3`（只看索引/计划时）、`ANOTIFY_PPROF=1`（取 profile 时）。总体骨架：

```bash
WT=<worktree 根>; mkdir -p /tmp/perf-a/logs; cd $WT
make fe
go build -trimpath -tags embeddist -o /tmp/perf-a/anotify ./cmd/server
go build -o /tmp/perf-a/loadgen ./scripts/perf/loadgen
DB=$(mktemp -d)/a.db
go run ./cmd/devseed -db "$DB" -username perf 2>&1 | tee /tmp/perf-a/logs/devseed.log  # 必须 2>&1：ns_ 走 stderr
SESSION=$(grep -o 'SESSION=[^ ]*' /tmp/perf-a/logs/devseed.log | cut -d= -f2)
SEND=$(grep -o 'SEND_KEY=[^ ]*' /tmp/perf-a/logs/devseed.log | cut -d= -f2)
NS=$(grep -o 'ns_[A-Za-z0-9]*' /tmp/perf-a/logs/devseed.log | head -1)
taskset -c 0-3 env ANOTIFY_ADDR=:7900 ANOTIFY_DB="$DB" ANOTIFY_PPROF=1 ANOTIFY_LOG_FORMAT=json \
  /tmp/perf-a/anotify > /tmp/perf-a/logs/server.log 2>&1 &
echo $! > /tmp/perf-a/server.pid
taskset -c 0-3 /tmp/perf-a/loadgen -url "http://127.0.0.1:7900/v1/console/events?namespaceId=$NS&limit=100" \
  -c 32 -d 5s -header "Cookie: anotify_session=$SESSION"
# 收尾：kill $(cat /tmp/perf-a/server.pid)   ← 不要用 pkill -f
```

## 8. 独立审查记录（对抗式）

本报告与 6 条 issue 在发布**前**由一个**未参与测量**的独立子节点做过一次对抗式审查（只读，不跑压测），
判定 **FAIL**，7 条必改 + 16 条建议。已全部处理：

| 必改 | 问题 | 处理 |
|---|---|---|
| 1 | #245 的 profile 摘要是占位命令 + 无法追溯的百分比 | 重新采了 429 路径的 15s profile，原文贴进 #245 §3 与本报告 §5.2 |
| 2 | #245 的 5 处 `文件:行` 有 4 处错（含一处越过文件末尾） | 逐条改成 `ratelimit.go:45/32/78/170/174`、`source_handlers.go:732` |
| 3 | 6 条 issue + 报告 §7 的复现命令 `$NS` 恒为空（`devseed` 的 `ns_…` 走 **stderr**，`tee` 接不到） | 全部改成 `2>&1 | tee`；并用改好的 #240 脚本**原样跑了一遍**验证 |
| 4 | #245 标题 15 MB/10s 与正文 75.1 矛盾 | 标题改「75 MB/10s @c=16」，并保留 20.6/53.4/75.1 三档 |
| 5 | #241 标题 27%–34% 把下限抬高 2 倍（events 只有 12.7%） | 标题改「12.7%–34.0%」 |
| 6 | #224 评论「811GB 在 26s 内」时间口径错（`alloc_space` 是进程累计） | 改成「进程累计 811GB ≈ 12.7k 次 argon2 ≈350–400s」 |
| 7 | #245 复现步骤 ③ 是省略号 + 缺 `mkdir -p` | 补成完整的两端口 `ANOTIFY_LOG_LEVEL` A/B 命令 |

建议项也一并修了（行号 131→132、71→67/76、`:16/32` 标签错、`1.7–1.8`→`1.66–1.69`、
§1.1 参照带自相矛盾、去掉无出处的「15% 门槛」、goroutine 分解与总数口径、`event.go:257`→`:261`、
#237 评论补上 `EXPLAIN` 的出处与「migration 12 之后索引是 `(namespace_id, parent_task_id)`」的说明、
#241 补原始 loadgen 输出、#240 混入 `S1ref` 的说明、`jq`/`sqlite3` 前提）。

审查自身的复算：**20 个抽样数字全部可追溯**（含 3 个中位数、1 个 CPU 秒/千请求、4 个百分比比值、1 个 µs/条 SQL），
其中 6 个做了独立复算一致；`go build ./...` 与 `go vet ./internal/store/ ./internal/auth/ ./internal/server/` 均 exit 0。

## 9. 复跑验证（同一脚本、更安静的机器）

审阅后按 issue 里**改好的复现命令原样跑了一遍**（新库、新端口、`make fe` 重新构建，脚本退出码 0），
时间落在别路压测已结束的窗口（loadavg ≈2）：

| 端点（c=32，5s） | 首轮（loadavg 9.4–18.4） | 复跑（loadavg ≈2） | 首轮比值 | 复跑比值 |
|---|---|---|---|---|
| `/health` | 24,057 | **28,798 / 27,462** | 100% | 100% |
| `/v1/tasks?limit=100` | 1,851 | **3,104** | 11.3% | **10.8%** |
| `/v1/capabilities` | 1,477 | **2,584** | 8.1% | **9.0%** |
| `/v1/console/events?limit=100` | 441 | **1,060** | 2.5% | **3.7%** |
| 阻塞在 `database/sql.(*DB).conn` 的 goroutine（grep 计数） | 30–32 | **38**（其间还叠着 1 个 loadgen 在跑） | — | — |
| `database is locked` / 真 5xx（稳态窗） | 0 / 0 | **0 / 0** | — | — |

**结论**：绝对值随机器负载漂 1.6–2.4 倍，**比值稳定在 ±1.5 个百分点内** —— 再次印证 §1.1 的口径：
这套数字只能用比值读，不能用绝对值读；同时证明 issue 里的复现命令确实「原样可跑」。
