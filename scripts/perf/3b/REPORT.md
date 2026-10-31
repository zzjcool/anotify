# 批次 3B · HTTP 响应成本测量报告（#223 / #243）

> 复现脚本：`scripts/perf/3b/{lib.sh,nodes.sh,events.sh}`（本目录）。
> 结论先放这里：
> - **#223**：`GET /v1/nodes` 每请求 SQL **202 → 4**（节点相关 201 → 3）；N=100 时
>   **RPS 3.2×**（135 → 438）、**p50 3.4× 低**（58 → 17.1ms）、**CPU/千请求 3.7× 低**（26.8 → 7.3s）。
> - **#243**：`limit=500` 响应 **183,453 B → 20,053 B（-89.1%，9.1×）**，代价 **+2% CPU/千请求**；
>   机器入口 `/v1/events` 去掉缩进后明文 **-21.9%**（234,923 → 183,415 B），再 gzip 到 20,048 B（11.7×）。
>   单命名空间的重排已删除（`mergeEvents`/`mergeTasks`），但在 500 行量级收益 <1%（详 §2.4）。
> - 门禁：`make ci` ×2 绿、`make e2e` 绿（1548 断言 / 0 失败）。期间记录到 1 次并行模式
>   `i18n_coverage` 套件超时（详见 §3），单跑与复跑均绿，与本次改动无关。

## 0. 环境与纪律

| 项 | 值 |
|---|---|
| 机器 | 12 核（本机同期**另有两路 perf 在跑**：A 路 0-3 核、C 路 8-11 核） |
| 分核 | server 与 loadgen 都 `taskset -c 4-7` |
| 端口 | 8041（#223）/ 8043（#243），都落在本路允许区间 8040-8059 |
| 库 | 每档 `mktemp -d` 独立 SQLite，跑完删 |
| 客户端 | `scripts/perf/loadgen`（标准库），keep-alive，c=8 / c=16 |
| 二进制的来历 | 基线 `3433eef`（本 PR 的 base）→ `anotify-base`；改后 `e4f9127`（本 PR 头）→ `anotify-new` |
| 二进制新鲜度自查 | `go build ./cmd/server` 重编一次与 `anotify-new` **sha256 完全相同**（`be71f71a…`）——踩过「用旧二进制得新结论」这个坑，所以这条每次都验 |
| 污染记录 | 每档都打 loadavg（见下表）。三路并行时本机 loadavg 在 5–25 之间剧烈波动，**因此每个结论都给了两个独立场次**（低负载场 / 高负载复跑场），并优先用对负载不敏感的 **CPU/千请求** 而不是 RPS 做定量结论 |

`CPU/千请求` 的口径：`/proc/<server pid>/stat` 的 utime+stime 增量 ÷ 该档请求数 × 1000。
它是**服务端**成本，不受客户端是否成为瓶颈影响，所以是本报告的主指标。

## 1. #223 · `GET /v1/nodes` 的 2N+1 条串行查询

### 1.1 改了什么

`viewFor` 对每台节点各跑 2 条 SQL（`ListNodeCapabilities` + `ListInstances`）。N=100 ⇒ 每请求
1（鉴权）+ 1（`ListNodes`）+ 200 = **202 条串行 SQL**（`SetMaxOpenConns(1)`，全部串行）。

改成整批两次查（`ListCapabilitiesByNode` / `InstanceStatsByNode`，`WHERE node_id IN (…)` 一次取回
+ 一条 `GROUP BY node_id` 的计数）：每请求 **4 条**，与 N 无关。JSON 形状、排序
（`capabilities` 按 name,version 升序）、`[]` 而非 `null` 全部保持不变（有新单测逐字节比对
批量版 vs 单台版）。

### 1.2 数字（100 节点、c=8、10s/档、两 rep）

场次：`loadavg 10.0–10.6`（A/C 两路同时在压，绝对数字比空机低，但前后同场次可比）。

| 指标 | 基线 `3433eef` | 改后 identity（=旧客户端行为） | 改后 gzip（浏览器/undici 真实路径） |
|---|---|---|---|
| RPS | **119.7 / 139.9** | **439.4 / 437.5** | **483.6 / 489.5** |
| p50 | 61.2 / 55.6 ms | **17.12 / 17.20 ms** | 15.94 / 15.73 ms |
| p99 | 131.8 / 82.4 ms | 33.9 / 33.5 ms | 29.7 / 28.2 ms |
| server CPU/千请求 | **26.93 / 26.76 s** | **7.34 / 7.30 s** | 7.16 / 7.12 s |
| 响应体（线上字节） | 41,742 B | 41,742 B | **2,310 B（18.1×）** |

> 另一场（`loadavg 6.8–8.4`）给出同样量级：基线 147.2–151.1 RPS / p50 51.9–52.5ms /
> CPU 24.7–24.8s 每千请求 → 改后 429.0–429.5 RPS / p50 17.3ms / CPU 7.27–7.36s 每千请求。
> 两场结论一致，**RPS 3.0–3.7×、CPU/千请求 3.4–3.7× 改善**，不受负载漂移影响。

### 1.3 pprof（同一 15s 窗口，identity 视角，8 并发）

基线（Total samples 36.33s）：

```
cum 82.30%  server.(*consoleHandler).viewFor            ← 每节点 2 条查
cum 50.89%  store.(*DB).ListNodeCapabilities
cum 30.97%  store.(*DB).ListInstances
cum 35.67%  sqlite/lib._sqlite3Parser                   ← 每条短 SQL 都要重新解析
cum 34.13%  sqlite/lib._yy_reduce
```

改后（Total samples 29.58s）：

```
cum 62.85%  server.(*consoleHandler).viewsFor            ← 整批 2 条批量查
cum 42.53%  store.(*DB).ListCapabilitiesByNode
cum 18.97%  store.(*DB).InstanceStatsByNode
cum  9.06%  sqlite.(*conn).prepareV2                     （_sqlite3Parser 落到 9% 量级）
```

- **占比**：`viewFor` **82.30% → `viewsFor` 62.85%**。
- **每请求绝对值**（窗口内请求数从 ~2.3k 涨到 ~6.5k）：`viewFor` 29.90s/2.3k ≈ **13.2ms/请求**
  → `viewsFor` 18.59s/6.5k ≈ **2.9ms/请求**（**-78%**）。
- 顺带把 SQL 文本解析（`_sqlite3Parser`/`_yy_reduce`）从 1/3 的 CPU 打到 9% 量级：
  现代 cgo-free 驱动的 prepare 是**每条 SQL 一次文本解析**，200 条短查询 = 200 次解析。

### 1.4 剩下的 2.9ms/请求 是什么（为什么没继续压）

`viewsFor` 里现在的成本是「把 300 行数据搬出来」本身：`database/sql.(*Rows).Next` 48.8%、
`columnText` 9.4%、`mallocgc` 11.7%。剩下的可压项都落在**本路不能改的文件**上：

| 可压项 | 归属 | 说明 |
|---|---|---|
| `prepare` 无语句缓存（每条 SQL 重解析） | `internal/store/store.go`（禁改） | 每次 `QueryContext(sql, args…)` 都 prepare→query→close |
| `ListNodes` 一次拉全列（含 spec/JSON 列） | `internal/store/node.go` 既有函数（只许新增） | 列表页只需要 5 列 |
| `node_capabilities` 无 `(namespace_id)` 索引 | `schema.sql` / 迁移（禁改） | 现在是按 node_id 走 join，表和索引都不该由本路动 |

## 2. #243 · 大响应（`/v1/console/events?limit=500`）

### 2.1 gzip 中间件（新增 `internal/server/middleware.go: gzipJSON`）

- 只作用于 **GET `/v1/*`**；`Accept-Encoding` 含 `gzip`（`gzip;q=0` 视为不接受）且**累积体 ≥1KB** 才压。
- 头语义：压时**删 `Content-Length`**（改 chunked）、设 `Content-Encoding: gzip`、**追加**
  `Vary: Accept-Encoding`；不压时只要 Content-Type 可压**也带** `Vary`（否则共享缓存会把压缩体
  喂给不支持 gzip 的客户端）。
- **不双重压缩**：响应已带 `Content-Encoding`（br/gzip/…）一律直通。
- **不碰 ETag / Last-Modified / Range**：静态资源（`/`，靠内容指纹 + 条件请求）与 pprof 都不在
  本中间件范围内，这是刻意画的边界。
- 透传 `Hijack` / `Flush` / `Unwrap`：WS（`/v1/node/stream`）升级与 `http.ResponseController` 照常。
- 顺序是 `gzipJSON(accessLog(mux))`（gzip 在**最外侧**）。放内侧会让 handler 收到的 `w` 变成
  `*gzipResponseWriter`，`ratelimit.go` 的 `w.(*statusWriter)` 断言静默失败 → 429 日志丢
  `retry_after`/`rate_limit`（#245 的回归）。已加钉住顺序的用例。
- 大响应（209KB/183KB 是一次 `Write`）走**零拷贝分支**：越阈值先用 `commit` 定局，再把这一批
  字节直接写进 gzip，不把整块 body 先 append 进阈值缓冲。

### 2.2 数字（`/v1/console/events`，每档 5s，3 rep 取中位）

场次 loadavg 5–7（本机三路并行，但相对最干净的一档）。identity = 旧行为（不发
`Accept-Encoding`，Go transport 不会自动加）；gzip = 浏览器/undici 真实路径。

**limit=500（issue 里 209KB 那一档）**

| 指标 | 基线 | 改后 identity | 改后 gzip |
|---|---|---|---|
| RPS | 267.2 (261.6–272.0) | 266.3 (262.2–268.8) | **278.3 (278.1–279.9)** |
| p50 | 28.4 ms | 28.7 ms | **26.5 ms** |
| server CPU/千请求 | **11.76 s** | 11.94 s（+1.5%，噪声内） | 12.11 s（**+3.0%**） |
| **响应体（线上字节）** | **183,453 B** | 183,453 B | **20,053 B（-89.1%，9.1×）** |

**limit=100**

| 指标 | 基线 | 改后 identity | 改后 gzip |
|---|---|---|---|
| RPS | 1097.3 (957.1–1113.6) | 965.3 (852.9–1070.6) | 1046.8 (1029.5–1052.0) |
| server CPU/千请求 | 2.79 s | 2.81 s | 3.02 s（+8%，36KB 文本的 flate 成本） |
| **响应体** | **36,751 B** | 36,751 B | **4,226 B（8.7×）** |

**机器入口 `GET /v1/events?limit=500`（`internal/api` 的 writeJSON）**

| 指标 | 基线（缩进 2 空格） | 改后（紧凑） | 改后 + gzip |
|---|---|---|---|
| 响应体 | 234,923 B | **183,415 B（-21.9%）** | **20,048 B（11.7×）** |

> gzip 自己的 CPU 在 profile 里是 `compress/flate.(*deflateFast).encode` 4.6% + 哈夫曼写入
> 3.97%（`deflateFast` = `gzip.BestSpeed`），对应上表 +0.35s/千请求。
> 高负载复跑场（loadavg 14–25）里 gzip 的**线上字节完全复现**（20,404 B / 4,206 B / 20,398 B），
> 但 CPU/千请求在噪声里上下漂 ±10%，故不作为结论依据。

### 2.3 去掉了哪些「每请求重复的工作」

1. **单命名空间的整表重排**（本次删掉）：`mergeEvents` / `mergeTasks` 无论分组数都
   `make` 新切片 + `sort.SliceStable`。`namespaceId=<某个 ns>` 时 `groups` 只有一个元素，而
   `store.ListEvents` / `ListTasks` 的 SQL 已经是 `ORDER BY created_at DESC, id DESC` ⇒
   每请求把最多 500 个元素再排一遍纯属白付。现在单分组走 `truncateSingleGroup` 快路径
   （透传 + 截断），跨命名空间（`*`）仍归并重排。有专门单测钉住两边（含同秒 `id DESC` 破平局）。
2. **响应缩进**（本次删掉）：机器入口 `writeJSON` 的 `enc.SetIndent("", "  ")` 让大响应凭空多
   29%/22% 体积，对可解析性零收益。
3. **多一次整块拷贝**（本次消掉）：gzip 定局前不把大 body 攒进阈值缓冲（见 §2.1 未条）。

### 2.4 没做到的（如实说明，不拿缓存凑数）

| 项 | 实测占比 | 为什么没做 |
|---|---|---|
| **SQL 侧的 `USE TEMP B-TREE FOR LAST TERM OF ORDER BY`** | 10k 行时 ~0.1ms/请求（issue 原文），**不是当前主因** | 要 `(namespace_id, created_at, id)` 覆盖索引才消，那是 `schema.sql` / 迁移，属本路**禁改**文件（另一路在动 store 写入路径）。本路只能新增只读函数，无法提索引 |
| **payload 每请求重新压缩**（`json.RawMessage` 触发 `appendCompact`） | `appendCompact` cum **7.75%**（基线 7.51%） | 唯一安全的做法是在**写入时**把 payload 存成已校验的紧凑字节（`internal/store` 写入路径，禁改）；读侧绕过它必须手写事件对象的 JSON 编码器 —— 那等于把 `store.Event` 的 wire schema 复制一份到 handler，以后任何字段变更都会静默漏发。**收益 7.75% 换一个长期的漂移隐患，不值**，留作后续单独立项 |
| **prepared statement 缓存** | 每条 SQL 一次文本解析（`prepareV2` 9% 在 N=100，N=3 时 29%） | 需要改 `internal/store/store.go` 的连接/查询封装（禁改） |
| **响应级缓存**（把序列化结果缓存几百 ms） | — | 事件流是「实时」语义，缓存会把「刚发生的事件」藏起来；`Cache-Control: no-store` 也是同一取向。宁可 CPU 平，也不拿正确性换数字 |

## 3. 门禁记录

| 门禁 | 结果 |
|---|---|
| `make ci`（第 1 次） | ✅ 绿（含 `contract` 静态断言、gofmt、`go vet`、`go build`、`go test ./...`、harness 自测） |
| `make ci`（第 2 次，收口前复跑） | ✅ 绿 |
| `make e2e`（并行模式，全量 20 套件） | ✅ **绿：1548 断言 / 0 失败 / 331.3s** |
| 期间记录到的一次红 | 第 1 次全量跑：`i18n_coverage` 0/0（**并行模式下套件 120s 超时被 SIGKILL**，机器 loadavg 8.5）。单跑该套件 **65/0、24.9s**；随后全量并行复跑 **65/0、43.9s**。判定为并行超时 flake，与本次改动无关（该套件只读 `web/` 的多语言文案，不碰响应编码；gzip 只会让页面更快，不会更慢）。**没有为它改任何断言。** |

e2e 里对响应体是 `await res.text()` + `JSON.parse`（undici 自动解压 gzip），仓库内没有任何
`Content-Length` / `Content-Encoding` 断言 —— 这也是敢上 gzip 的前提，并且**一条断言都没改**。

## 4. 顺带发现（未修，不在本路范围）

`GET /v1/nodes` 的 `nodeView.capabilities[].displayName` **恒为空串**：`prefixedCapabilityColumns`
的列清单里没有 `display_name`，`ListNodeCapabilities`（及其批量版）扫不到这一列，`viewFor`
却把 `c.DisplayName` 直接投影出去。属既有行为（改造前后完全一致，未被我改坏），
修它会改变响应内容与详情页展示，需要单独确认口径。
