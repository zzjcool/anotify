# C 路 · 资源、泄漏与前端加载（性能评估报告）

> 上游 #217《性能评估与瓶颈探查（三路并行）》· 分工：C 路 = 资源与泄漏：10 分钟持续写入下的
> RSS/goroutine/fd/DB+WAL 增长、事件保留未开的无界增长、静态资源缓存、页面加载。
> 本报告只给结论与数字；**每个问题一条 issue**，不含任何代码改动。

## 0. 结论速览

| 指标 | 实测 | 属于 |
|---|---|---|
| 10 分钟持续写入 | 32,253 事件 / 18,619 任务，`200×18,324` 零失败，RPS 17.1→**35.3** | — |
| RSS | 95MB → 33s 内阶跃到 1.04GB → 610~1062MB 锯齿（**平台化，非泄漏**） | #227 |
| goroutine | 9 → 中位 37 / 峰值 107（压测中）→ **9**（停压；pprof 快照 t≈330 为 27） | 无问题 |
| fd | 11 → 20 → **11**（停压） | 无问题 |
| WAL | 1.72 → **4.01MB 恒定**（autocheckpoint 压住） | 无问题 |
| 主库文件 | 0 → 25.08MB（含 events + 18,619 行 tasks，合 **815 B/事件**；独立单事件场景实测 655.4 B/事件） | #229 / #230 |
| 首屏（index 冷启动） | TTFB 45.7ms / **DCL 565.5ms** / load 706.7ms；**14 个子资源 402,534B（含入口 HTML 450,791B，浏览器侧 477.9KB）** 未压缩 | #231 / #232 / #234 |
| 大列表（1000 条事件） | DOM 11,203 节点、renderList 26ms（**不是瓶颈**）；但页面只显示 100 条 | #235 |
| 10 万事件读路径 | events 侧纯 SQL **1.69~1.83ms 平线**（走 `idx_events_retention`）；tasks 列表 **1.06 → 11.91 → 36.12ms（p95）随任务数增长** | #237 |

**核心判据（#217 要的那条）**：只有 **3 个指标随时间/数据量单调增长** ——

1. **主库文件**（655.4 B/事件，无保留期、auto_vacuum=0、无 VACUUM）→ #229
2. **`tasks` 表积压**（无背压，写入 > 消费就以 ~8 行/秒累积）→ #228
3. **`GET /v1/tasks` 延迟**（无 `(namespace_id, created_at)` 索引，全表扫描 + 临时 B 树排序）→ #237

其余（RSS / goroutine / fd / WAL）都**一次性涨到位后平台化**，停压即回落——**没有资源泄漏**。

## 1. 最重要的 3 个发现

### ① API Key 认证每请求跑一遍 argon2id（m=64MB, p=4）→ 写路径 ~35 RPS/4 核封顶，RSS ≈ 并发×128MB（#227）

`internal/auth/apikey.go:20-24` 的 `m=64MB, t=1, p=4`，每次校验都重新派生（无缓存）：

```
# 10 分钟 soak：alloc_space 累计 1,173,845 MB = 18,341 次 × 64MB（与 Bearer 请求数逐一对上）
1173845.02MB 99.90% 99.90%    golang.org/x/crypto/argon2.initBlocks
# 30s CPU profile（4 核跑满 387.6%）
42.42s 36.32%  argon2.mixBlocksSSE2
30.54s 26.15%  argon2.blamkaSSE4
22.23s 19.03%  runtime.memclrNoHeapPointers      ← 清零那 64MB
 1.58s  1.35%  modernc.org/sqlite/lib._yy_reduce  ← 数据库只占 1.6%
```

| 并发 | RSS | 吞吐（4 核） | p50 |
|---|---|---|---|
| `-c 8`（soak） | **1.04GB** | 35.3 RPS | 222ms |
| `-c 8`（C5 单请求口径） | — | — | `GET /v1/events?limit=1` 也要 **38ms**（health 0.5ms） |

结论：SQLite/HTTP/JSON 都不是瓶颈；**上限在认证**。不会随时间累积，但随并发线性上涨（`-c 32` ≈ 4GB）。

### ② 静态资源既无 gzip 也无 ETag/Last-Modified → 首屏 402KB、304 命中率 0%、每次导航全量重传（#231 / #232 / #234）

```
首屏 14 个子资源合计 402,534 B → gzip -9 估算 141,344 B（2.85x）；
   含入口 HTML 文档（48,257B）后 450,791 B → 153,523 B（2.94x）；HTML 入口 3.96x、CSS 3.32x、JS 3.12x
   浏览器侧（s4，21 个子资源 + 文档）477.9KB —— 三者是不同口径，引用时要写明
带 Accept-Encoding: gzip,br 的 size_download 与 plain 逐字节相同  → 服务端没有压缩中间件
15/15 对象既无 ETag 也无 Last-Modified；If-None-Match / If-Modified-Since 一律 200 + 全量 → 304 = 0%
index.html 冷启动 DCL 565.5ms / load 706.7ms（TTFB 只有 45.7ms），6 个同步阻塞脚本里 150KB 图库全站加载
长任务：index [{245,254ms},{560,101ms}] / events [{150,93ms}] / playbooks [{159,58ms}]
```

两者叠加是 4 倍成本：入口 HTML 48KB 未压缩 × 没有 304 = **每次导航 48KB**，而不是「304 空响应」或「12KB gzip」。

### ③ 「容量三件套」：无保留期 + 无背压 + 列表全表扫描（#229 / #228 / #237）

| 项 | 数字 | 形状 |
|---|---|---|
| 事件写入磁盘 | **655.4 B/事件**（空 payload，含剪枝留痕 2 行）、**1937.4 B**（1KB payload）；38 ev/s → 一月 **60.1 GiB** / **177.7 GiB**，10GiB 盘撑 **5.0 天** / **1.7 天** | 严格线性（m2000→m4000 翻倍条数=精确翻倍字节） |
| 任务积压 | 600s 内 **26.8% 停在 pending**（4,988/18,619），入流 31.0/s − 成功 22.7/s = 8.3/s | 线性累积 |
| 任务列表 | `SCAN tasks` + `USE TEMP B-TREE FOR ORDER BY`；0→2千→2万任务：**1.06 → 11.91 → 36.12ms(p95)**（早一轮 1.08/11.66/25.47，n=20 抖动已披露）；纯 SQL 1000 行 2.6ms → 20000 行 11.1ms | 随任务数单调增长 |

三者叠在一起：写进来的东西既删不掉（#229）、消费不完就积压（#228）、积压表还被在线列表全表扫（#237）。

## 2. Issue 清单

| # | 标题（简） | 严重度 | 类型 | 关键数字 |
|---|---|---|---|---|
| [#227](https://github.com/zzjcool/anotify/issues/227) | API Key 每请求 argon2id(64MB,p=4) | **major** | perf | 1.17TB/10min 分配、CPU 75%、~35 RPS/4核、RSS≈并发×128MB |
| [#228](https://github.com/zzjcool/anotify/issues/228) | 派发无背压，任务无限积压 | **major** | perf | 26.8% pending、8.3 行/秒 |
| [#229](https://github.com/zzjcool/anotify/issues/229) | 事件保留期关闭且无上限 | **major** | perf | 655.4 B/事件、一月 60.1 GiB、10GiB 撑 5 天 |
| [#230](https://github.com/zzjcool/anotify/issues/230) | `idx_events_retention` 的写放大（**复审后更正**：读路径在用它，不可删） | minor | perf | 占 events 增量 11.88%（≈7.1 GiB/月，需计入容量预算） |
| [#231](https://github.com/zzjcool/anotify/issues/231) | 静态资源无 gzip | **major** | perf | 14 子资源 402KB→141KB（2.85x）/ 含 HTML 450KB→153KB（2.94x），逐字节未压 |
| [#232](https://github.com/zzjcool/anotify/issues/232) | 静态资源无 ETag/Last-Modified，304=0% | **major** | perf | 0/15 有校验器、每次导航全量 48KB |
| [#233](https://github.com/zzjcool/anotify/issues/233) | `classify()` 判 `manifest.json`，产物是 `.webmanifest` | minor | bug | 靠兜底碰巧对，改兜底就变「缓存一年」 |
| [#234](https://github.com/zzjcool/anotify/issues/234) | 首屏被同步阻塞脚本 + 未压缩资源吃掉 | **major** | perf | DCL 565.5ms / load 706.7ms、longtask 254ms |
| [#235](https://github.com/zzjcool/anotify/issues/235) | events 页只显示最新 100 条、无分页 | **major** | bug | 1000 条里 900 条 UI 不可达 |
| [#236](https://github.com/zzjcool/anotify/issues/236) | `?limit=501..1000` 静默返回 50 条 | **major** | bug | 请求 600 → 得 50（比默认 100 还少） |
| [#237](https://github.com/zzjcool/anotify/issues/237) | `tasks` 缺 `(namespace_id, created_at)` 索引 | **major** | perf | 2 万任务 36.12ms(p95)，SCAN + 临时 B 树；纯 SQL 2.6ms→11.1ms |
| [#238](https://github.com/zzjcool/anotify/issues/238) | `schema.sql` 里 `idx_edges_lookup` 重复声明（第二条引用已删列） | minor | bug | 2 处同名声明、库里只有 1 条、`from_capability` 已不存在 |

统计：**major 9 条 / minor 3 条**；perf 8 条 / bug 4 条。
其中「随时间或数据量单调增长」的：**#227（随并发）、#228、#229、#237** 四条。

## 3. 正面结论（同样重要，别再重复查）

这些是**排除项**，数字都实测过：

1. **没有 goroutine 泄漏**：9（空载）→ 压测段中位 **37** / 峰值 **107** → 9（停压）；`pprof` 快照在 t≈330 是 27（单点快照与 30s 采样峰值口径不同，别混读）。峰值与 argon2 的 `p=4` 并发派生相关，**未单独验证**归因。
2. **没有 fd 泄漏**：11 → 20 → 11。
3. **没有 WAL 无界增长**：1.72 → 4.01MB 后恒定，与 `wal_autocheckpoint=1000 × 4096` 吻合。
4. **RSS 不是泄漏**：一次阶跃后锯齿振荡，停压回落到 688MB；heap in-use 恒定 320MB（全是 argon2 的在飞缓冲）。
5. **events 读路径没有慢查询**：`idx_events_retention` 生效（`EXPLAIN` 为证，见 #230 的更正），10 万行下 `ListEvents(limit=500)` 纯 SQL **1.69~1.83ms 平线**；
   `GET /v1/events` 的 38~50ms 里 95% 是 argon2（#227），不是 SQL。
6. **大列表渲染不是瓶颈**：1000 行 renderList **26ms**、DOM 11 节点/行、无 longtask；
   50ms 长任务门槛 ≈ 2000 行、1s ≈ 40000 行，均超出当前 API 硬顶 500 行。
7. **10 分钟窗口内没有「越写越慢」**：DB 从 0.9MB 涨到 25MB，p95 反而从 748ms 降到 320ms（冷启动效应）。

## 4. 测量方法与可复现性

| 项 | 做法 |
|---|---|
| 分核 | A/B/C 三路并行，C 用 `taskset -c 8-11`；C4 收窄 8-9、C5 收窄 10-11（避免两路互扰） |
| 端口 | 只用 7940-7959（`common.sh` 的 `pc_port_ok` 硬拒绝段外端口）；实测 7940 被别的实例占用 → 顺延 7943 |
| 库 | 每场景各自 `pc_start` 建的独立目录，停服务后再量体积（先 `PRAGMA wal_checkpoint(TRUNCATE)`，否则量到 4 倍噪声） |
| profile | 服务一律 `ANOTIFY_PPROF=1`；CPU 30s、heap/goroutine 在首尾各一次。**mutex profile 抓了但是空的**（454B / 0 样本）——本进程没有开 `runtime.SetMutexProfileFraction`，见 §5 |
| 写路径真实性 | `agent.mjs` 起真实 WS 节点接单回报，所以量到的是「webhook 派发 → 任务落库 → 分配 → 结果落库」全链 |
| 造大量数据 | C5 的 10 万事件/2 万任务是**停服务后 sqlite3 直插**（接口 35 ev/s 要 48 分钟）；已注明是合成数据 |
| 前端 | `playwright-core` + 系统 Chrome（`channel:"chrome"` headless）；每 run 新 context（冷缓存），同 context 再 reload 记「热」 |

五个场景各自的脚本（都能独立重跑）：`scripts/perf/c/{s1-soak.sh,s2-retention.sh,s3-static.sh,s4-page.mjs,s5-slowquery.sh}`，
公共底座 `scripts/perf/c/{common.sh,agent.mjs}`。原始产物在 `/tmp/perf-c/s{1..5}/`（未入库）。

## 5. 未确认 / 缺口

1. **gzip 能省多少毫秒**没做 A/B（只证了字节数 2.85x，没证时间）。按本机 2 核实测占比估计 150~250ms，属外推。
2. **C4 的 1000 行档是合成响应**（API 硬顶 500，`limit>500` 还会被打回 50 见 #236），测的是渲染成本不是端到端能力；
   2000/40000 行的长任务阈值是线性外推。
3. **C5 的 tasks 1s 临界点是外推**（2k→20k 的纯 SQL 斜率），要精确值需百万级任务（本轮墙钟不允许）。
4. **C1 的 hook 支路**只有 30/min（受 `60/min/IP` 限速所限），所以「webhook 派发」这一环的压力样本偏轻；
   满压走的是 `/v1/events`。
5. **C3 的 `/assets/` 目录列表**（autoindex 200）只记录未定级——指纹资源本身是公开的，影响待判。
6. **mutex / 锁竞争 = 完全没测到**：`grep -rn SetMutexProfileFraction internal/` 零命中，Go 默认 fraction=0，
   所以 `profiles/*.mutex.pprof` 是 0 样本空壳（报告任何地方都没有用它当证据，但也不能声称"采过"）。
   要补：`ANOTIFY_PPROF=1` 时同时开 `SetMutexProfileFraction(1)` 再跑 S1。
7. **heap 在 soak 内只有 2 个时间点**（t≈330 / t≈600，都是 320MB），少于契约 §5-4 要求的 3 点；
   其余指标（RSS/goroutine/fd/WAL/DB）都是 21 点。所以「heap in-use 不涨」这条证据比别的弱一档。
8. **p95 的单点方差**：s5 两次运行给出 tasks 列表 25.47 vs 36.12ms（n=20），已在 #237 里披露；
   结论靠「p1→p2」与「纯 SQL 五档曲线」两条独立证据，不靠单点绝对值。
9. **#227 的 `-c 32 ≈ 4GB` 是外推**：本轮只测了 `-c 8`；要坐实「RSS ≈ 并发×128MB」需要 `-c 2/4/8` 三点扫描。
