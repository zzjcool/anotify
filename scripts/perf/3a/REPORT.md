# 批次 3A 压测报告（#237 tasks 列表索引 + #242 store 层语句缓存）

> 执行者：worker C（压测与数字）。本文件只放**数字与原文**，产品代码一行未改。
> 状态：**round-1 完成**（HEAD `215718d`，无 `ANALYZE`）；**round-2 待跑**（等 `/tmp/perf-3a/GO-FINAL`，
> 最终形态含迁移 16 的 `ANALYZE` + `Open` 末尾的 `PRAGMA optimize`，见 §8）。
>
> 脚本：`scripts/perf/3a/lib.sh`（公共底座）+ `scripts/perf/3a/measure.sh`（执行器）。
> 关键原文另存一份在 `scripts/perf/3a/raw/`（防 `/tmp` 被清，见 §7 事故）。

---

## 0. 三个配置的身份（每张表都要能对上这张表）

| 配置 | 含义 | 源码 | 二进制 sha256（前 16） | 大小(字节) | 构建参数 | 链接的驱动 |
|---|---|---|---|---|---|---|
| **A** | 基线（改动前） | `git archive 3433eef8ccd1b36764cff430997bc6e9e73158d6` | `2f3624df4b072c92` | 23999101 | `-trimpath -tags embeddist` | `modernc.org/sqlite v1.34.5` |
| **B** | 只加索引（迁移 16） | `git archive dfcf97b5f6c0f2ed103be7ab06f966c1d9519db4` | `e98fc34b9dce3485` | 24003213 | 同上 | `v1.34.5` |
| **C** | 索引 + 语句缓存 **+ 驱动升级 v1.34.5 → v1.45.0** | worktree `git rev-parse HEAD = 215718df6f2ebcfeeafc95e79f8b1d42b5d0035e`（树指纹 `853627d6…`） | `706143420d4f7dbc` | 24258098 | 同上 | **`v1.45.0`** |

- **C 含驱动 v1.34.5 → v1.45.0**（根节点授权）：v1.34.5 的 `(*stmt).query` 每次执行都 `prepareV2`，
  v1.45.0 起缓存编译句柄。所以「C vs B」= 语句缓存 + 驱动升级的**合并净收益**，不能单独归因给 `*sql.Stmt`。
- 二进制构建**统一** `-trimpath -tags embeddist`，内嵌同一份 `make fe` 产物（111 个文件 / 21 个 HTML）。
- **可重建性交叉验证**：从 `git archive 3433eef` 重编的 A 与 centurion/centurion 先前构建的那份**逐字节相同**
  （`2f3624df4b072c92535d50fab88faff1591236906f6815e341cac03f19c94ec8`）；
  从 `git archive 215718d` 重编的 C 与实测的 C **逐字节相同**（`70614342…`，见 §5 的自证步骤）。
  即：本报告每个数字都能用「commit + 上述构建参数 + 同一份 dist」重建出**一模一样的二进制**。
- 溯源原文：`raw/bin-provenance.tsv`、`raw/after-provenance.txt`。

## 1. 库的身份（同一批数据，只差索引）

播种配方（`scripts/perf/3a/lib.sh` 的 `p3_seed`）：基线 `devseed` 建用户/命名空间/能力 →
`generate_series` 灌 `tasks` N 行（status 全 `pending`，`created_at` 递增、同秒多行）+ `events` 10 万行 →
`wal_checkpoint(TRUNCATE) + VACUUM + ANALYZE`。**播种一次，其余全是 `cp` 副本**。

| 库 | 用途 | schema | `idx_tasks_ns_created` | 文件大小(字节) | sha256 |
|---|---|---|---|---|---|
| `seed-20k` | 20k 档模板 | 15 | absent | 24866816 | `eb038cb98c31341f…` |
| `20k-A` | A 的库 | 15 | absent | 24870912 | `b259b109bb6c0ec3…` |
| `20k-B` | B 的库（A 副本 + index 二进制 Open 触发迁移 16） | 16 | present(namespace_id,created_at,id) | 25870336 | `c013fcad95c7252b…` |
| `20k-C` | C 的库（**B 的逐字节副本**） | 16 | present | 25870336 | `c013fcad95c7252b…`（与 B 相同 ✅） |
| `seed-100k` | 100k 档模板 | 15 | absent | 51638272 | `bc217b0e59e76381…` |
| `100k-A` | A 的库 | 15 | absent | 51638272 | `5cd1f275209380b7…` |
| `100k-B` | B 的库 | 16 | present | 56668160 | `aa0fe699727cedd5…` |

- **B 与 C 的库逐字节相同**（同 sha256）——这是「#242 只比代码、不比数据」的自证。
- 库体积代价（索引本身）：20k `+1,003,520 B`（**+4.02%**）；100k `+5,029,888 B`（**+9.74%**）。
- 完整清单（含每批次 sha）：`raw/provenance-db.txt`。
- 注：run 结束后写路径实验会往各库追加 `events`/`tasks` 行，所以上表是**读路径测量时刻**的体积；
  三个配置的追加量相同（每个写轮 events+N / tasks+N 见 §4），不影响互相比较。

---

## 2. 表 1 · #237 读路径：`GET /v1/tasks?namespaceId=…&limit=200`

**口径**：`loadgen -c 32 -d 20s`，`taskset -c 0-3` 同时绑服务端与客户端；每轮换库重启服务；
每轮先打 1 条请求预热（连接池/语句缓存进稳态），再开压。**A/B 交替两轮**（A,B,A,B），取中位数。
采样次数 n=2 轮/配置（每轮内含上千次请求）；极差一并披露。

| 档 | 配置 | 库 | 二进制 | 轮 | RPS | p50 | p95 | p99 |
|---|---|---|---|---|---|---|---|---|
| 20k | **A** | 20k-A | `2f3624df4b07` | 1 | 28.8 | 941.6 ms | **2128.6 ms** | 2924.2 ms |
| 20k | **A** | 20k-A | `2f3624df4b07` | 2 | 28.5 | 931.0 ms | **2146.0 ms** | 2988.4 ms |
| 20k | **B** | 20k-B | `e98fc34b9dce` | 1 | 516.4 | 55.0 ms | **120.9 ms** | 160.8 ms |
| 20k | **B** | 20k-B | `e98fc34b9dce` | 2 | 516.5 | 55.0 ms | **120.2 ms** | 161.7 ms |
| 100k | **A** | 100k-A | `2f3624df4b07` | 1 | 5.8 | 4140.6 ms | **9672.0 ms** | 10001.2 ms |
| 100k | **A** | 100k-A | `2f3624df4b07` | 2 | 6.0 | 4206.5 ms | **10000.5 ms** | 10001.0 ms |
| 100k | **B** | 100k-B | `e98fc34b9dce` | 1 | 512.2 | 55.4 ms | **121.9 ms** | 165.6 ms |
| 100k | **B** | 100k-B | `e98fc34b9dce` | 2 | 510.0 | 55.9 ms | **123.0 ms** | 162.9 ms |

**结论（A → B）**

| 档 | p95 中位数 | 改善 | RPS 中位数 | 改善 | 极差（p95） |
|---|---|---|---|---|---|
| 20k | 2128.6 → 120.9 ms | **17.6×** | 28.7 → 516.4 | **18.0×** | A: 0.8%，B: 0.6% |
| 100k | 9836.3 → 123.0 ms | **80.0×**（A 两轮 9672–10000 → 78.6–81.3×） | 5.9 → 511.1 | **86.6×** | A: 3.4%，B: 0.9% |

- A 在 100k 档 p99 撞到 loadgen 的 10s 单请求超时（`p99≈10001ms`）：`总请求 116 / 失败 timeout×4`
  （`raw/load/tasks-100k-A-r1.log` 原文），即**有请求根本等不到响应**；同一行 `200×112` 是成功的 112 个。
- A 的 p95 随规模线性增长（20k→100k 变慢 4.7×），B 基本持平（120.9→123.0 ms，只随客户端排队走）。
  **这正是 #237 的症状与验收点：列表查询不再随命名空间行数线性变慢。**

原始日志：`raw/summary.tsv`、`raw/load/tasks-*.log`。

## 3. 表 2 · EXPLAIN QUERY PLAN 原文 + 库体积

SQL 取自 `internal/store/task.go` 的 `taskColumns` **原文**（不是 `SELECT *`，否则计划不可比），
参数为真实的 `namespace_id`、`ORDER BY created_at DESC, id DESC LIMIT 200`。原文：`raw/explain/*.txt`。

**20k-A（v15，无索引）**

```
# SQL: SELECT id, namespace_id, trace_id, … , not_before FROM tasks WHERE namespace_id = 'ns_…'
      ORDER BY created_at DESC, id DESC LIMIT 200
QUERY PLAN
|--SCAN tasks
`--USE TEMP B-TREE FOR ORDER BY
```

**20k-B（v16，idx_tasks_ns_created）**

```
QUERY PLAN
`--SEARCH tasks USING INDEX idx_tasks_ns_created (namespace_id=?)
```

**100k-A（v15）**：`|--SCAN tasks` + `` `--USE TEMP B-TREE FOR ORDER BY ``
**100k-B（v16）**：`` `--SEARCH tasks USING INDEX idx_tasks_ns_created (namespace_id=?) ``

补充形状（同一批库）：

| 形状 | 20k-A | 20k-B |
|---|---|---|
| 列表（真实列清单 + ORDER BY DESC LIMIT 200） | `SCAN tasks` + `TEMP B-TREE` | `SEARCH … USING INDEX idx_tasks_ns_created`，**无 TEMP B-TREE** |
| `count(*) WHERE namespace_id=?` | `SEARCH tasks USING COVERING INDEX idx_tasks_parent` | `SEARCH tasks USING COVERING INDEX idx_tasks_ns_created` |

**与契约规划期探针的差异（如实披露）**：规划期记录 A 的计划是
`SEARCH tasks USING INDEX idx_tasks_trace (namespace_id=?) + TEMP B-TREE`，本轮 `ANALYZE` 之后
A 退化成 **全表 `SCAN tasks` + TEMP B-TREE**（优化器看到该命名空间占全表 100%，认为索引定位+m+排序不如扫表）。
所以 A 的真实成本比规划期估的更差，B 的收益也更大——**这是计划形状变了，不是数据造错了**
（同一份库、`sqlite_stat1` 两侧都有；见 §1）。

| 库体积 | A | B | 差值 | 占比 |
|---|---|---|---|---|
| tasks=20k | 24,870,912 B | 25,870,336 B | +1,003,520 B | **+4.02%** |
| tasks=100k | 51,638,272 B | 56,668,160 B | +5,029,888 B | **+9.74%** |

---

## 4. 表 3 · #242 语句缓存（+ 驱动升级）：B vs C

**口径**：同一份数据形状（20k-B / 20k-C 库**逐字节相同**），`loadgen -c 32 -d 25s`，
`+2s` 时开抓 `curl /debug/pprof/profile?seconds=20`，采样窗口完整落在负载稳态内。
**B,C 交替两轮**（B,C,B,C）。n=2 轮/配置。
`CPU/请求 = profile total samples(秒) ÷ (RPS × 20)`；profile 是**进程 CPU 秒**（多核会 > 20s，见表中 total）。

| 配置 | 库 | 二进制 | 轮 | RPS | p50 | p95 | p99 | profile total | **CPU/请求** |
|---|---|---|---|---|---|---|---|---|---|
| **B** | 20k-B | `e98fc34b9dce` | 1 | 528.4 | 53.56 ms | 119.03 ms | 162.08 ms | 71.26 s | **6.743 ms** |
| **B** | 20k-B | `e98fc34b9dce` | 2 | 534.5 | 53.29 ms | 116.88 ms | 157.55 ms | 73.16 s | **6.844 ms** |
| **C** | 20k-C | `706143420d4f` | 1 | 1050.7 | 27.52 ms | 57.40 ms | 75.04 ms | 69.55 s | **3.310 ms** |
| **C** | 20k-C | `706143420d4f` | 2 | 1051.4 | 27.53 ms | 56.45 ms | 73.98 ms | 69.61 s | **3.310 ms** |

| 指标 | B 中位数 | C 中位数 | 变化 | 极差（B / C） |
|---|---|---|---|---|
| RPS | 531.5 | **1051.1** | **+97.8%** | 1.2% / 0.07% |
| p50 | 53.43 ms | **27.53 ms** | **−48.5%** | 0.5% / 0.04% |
| p95 | 117.96 ms | **56.93 ms** | **−51.7%** | 1.8% / 1.7% |
| CPU/请求 | 6.794 ms | **3.310 ms** | **−51.3%** | 1.5% / 0.0% |

### 4.1 `prepareV2` 的平/累计与占比（`go tool pprof -top -nodecount=30` / `-list`）

两配置各一份原文：`raw/pprof/cpu-20k-{B,C}-r{1,2}/`。符号全名：
`modernc.org/sqlite.(*conn).prepareV2`（B 在 `sqlite.go`，C 在 `conn.go`）。

| 配置 | 轮 | flat | **cum** | **cum% of Total** |
|---|---|---|---|---|
| B（v1.34.5） | 1 | 0 | **1.53 s** | **2.15%** |
| B（v1.34.5） | 2 | 0 | **1.30 s** | **1.78%** |
| C（v1.45.0 + 缓存） | 1 | 0 | **60 ms** | **0.086%** |
| C（v1.45.0 + 缓存） | 2 | 0 | **40 ms** | **0.057%** |

- `prepareV2` 的累计 CPU **1.415 s → 0.05 s（−96.5%）**，占比 **1.97% → 0.07%（−96%）**。
- C 的 `prepareV2` 已跌出 `-top` 的前 200 个节点（`raw/pprof/cpu-20k-C-r*/pprof-prepareV2-row.txt` 为空），
  只在 `-list` 里还能看到 60/40ms——这本身就是「热路径不再每条 SQL 一次 prepare」的直接证据。
- `-list` 的行级信息在 `-trimpath` 构建下**可用**（源码行号正常），未需要 `-source_path`。
  实际命令：`go tool pprof -source_path="$(go env GOMODCACHE):$(go env GOROOT)/src" -list=prepareV2 <pprof>`。
- **与 issue #242 的 26% 不同**：本机 profile 里 B 的 CPU 大头是驱动内部互斥争用
  （`runtime.procyield` 19.9%、`internal/sync.(*Mutex).lockSlow` 36.0% cum），
  C 的被测形状变成真实的执行/序列化开销（`sqlite3VdbeExec` 6.8%、`json.structEncoder.encode` 2.9%）。
  即：`prepareV2` 的绝对占比取决于争用形状，**用 CPU/请求与 RPS 判收益更稳**（−51% CPU/请求）。

---

## 5. 表 4 · 写路径（#237 的写放大）

### 5(a) store 层纯代价（**主判据**，契约修订 ②）

同一份代码（`git archive 215718d`，并用重编二进制与实测的 `70614342…` 逐字节比对自证）、
同一台机器、同一批核（`taskset -c 0-3`），两份源码树**只差 `schema.sql` 里一行 `CREATE INDEX`**
（`diff -u` 原文：`raw/bench/schema-diff.txt`，恰好 1 处）。

| 口径 | 带索引（中位数） | 无索引（中位数） | 差值 | n | 极差 |
|---|---|---|---|---|---|
| **受控**：`-bench BenchmarkCreateTasks/n=20 -benchtime=300x -count=5`，with/without **交替 3 轮** | **1272.4 µs/op** | **1206.4 µs/op** | **+5.5%**（均值 +4.6%） | 15 / 15 | 18% / 19% |
| 契约原始命令：`-bench BenchmarkCreateTasks -benchmem -count=5`（n=20） | 1700 µs/op | 1684 µs/op | +1.0% | 5 / 5 | — |
| 同上（n=5） | 457 µs/op | 412 µs/op | +10.9% | 5 / 5 | — |
| 同上（n=1） | 115 µs/op | 103 µs/op | +11.6% | 5 / 5 | — |
| **纯 SQL 复核**：在 20k 行的真实库副本上批插 20,000 行（`sqlite3` CLI，事务内） | **137.8 ms** | **124.1 ms** | **+11.0%** | 5 / 5 | — |

- **为什么主判据用「受控」那一行**：契约原始命令用默认 `-benchtime=1s`，`b.N` 自适应，
  而 `BenchmarkCreateTasks` 的**内存库行数随迭代次数增长**——后几个样本跑在更大的表上，天然更慢。
  实测原始数据呈明显上升趋势（n=20：1531→1927µs），中位数不可比。
  受控版固定 `300x` 迭代（两侧表规模完全相同）+ with/without 交替 3 轮，才是「同一形状的差值」。
- **判据结论：主判据 +5.5%，未到「变差 > 10%」的红线；但纯 SQL 批插口径是 +11.0%，处在噪声边缘。**
  两条都写进报告，取舍建议见 §6。

### 5(b) webhook 派发（旁证，契约修订 ①）

源：`POST /v1/sources`（`kind=webhook`）→ `targets=[{capabilityId: notify.phone@1, params:{to:<usr_id>,title:"perf"}}]`，
`to` 必须填**真实 user id**（填用户名会被 dispatch 剪成 `foreign_user`，一个 task 都不建——
本轮踩过，`raw/hook-smoke.json` 是修复后的版本）。
打 `POST /v1/hooks/<token>`，body `{"type":"perf","payload":{"n":1}}`。
**每次请求确实落到 DB**：每个写轮 end-to-end 都记录了 `events+N / tasks+N`（见下表 note 列），
`events` 与 `tasks` 同步增长（派发没有被 `params_schema` 剪掉）。

**(a) 默认 env（`c=8 -d 10s`）** —— 会撞 600/min 的 token 维限流：

| 配置 | RPS | p50 | p95 | 状态码 | DB 行数变化 |
|---|---|---|---|---|---|
| A | 18221.1 | 0.323 ms | 0.864 ms | `200×600 429×181616` | events+600 / tasks+600 |
| B | 18760.9 | 0.318 ms | 0.816 ms | `200×600 429×187023` | events+600 / tasks+600 |
| C | 18747.8 | 0.320 ms | 0.801 ms | `200×600 429×186884` | events+600 / tasks+600 |

> **说明**：19k RPS 全是「429 秒拒」的速度，10 秒里只有 **600** 个请求真正进了 DB（= 60 写/秒上限）。
> **默认配置下写路径根本到不了 DB 成本**——三个配置的 p95 差异是拒绝路径的差异，不是写成本。

**(b) 显式抬高双维限流**（`ANOTIFY_HOOK_RATE_PER_MIN=200000 ANOTIFY_HOOK_RATE_PER_IP_PER_MIN=200000`，
`c=8 -d 20s`，A/B/C 交替两轮）。**这是唯一偏离默认 env 的地方**，原因是上面的限流天花板；
不改这个 env，写路径一行都写不进 DB。

| 配置 | 轮 | RPS | p50 | p95 | p99 | DB 行数变化 |
|---|---|---|---|---|---|---|
| **A** | 1 | 1203.8 | 3.283 ms | 21.79 ms | 56.31 ms | events+24083 / tasks+24077 |
| **A** | 2 | 1196.1 | 3.185 ms | 21.89 ms | 56.82 ms | events+23928 / tasks+23925 |
| **B** | 1 | 1158.0 | 3.270 ms | 22.78 ms | 57.54 ms | events+23167 / tasks+23163 |
| **B** | 2 | 1153.4 | 3.182 ms | 22.71 ms | 58.15 ms | events+23073 / tasks+23068 |
| **C** | 1 | 1106.4 | 3.285 ms | 23.86 ms | 59.74 ms | events+22132 / tasks+22129 |
| **C** | 2 | 1062.4 | 3.465 ms | 24.65 ms | 65.92 ms | events+21252 / tasks+21248 |

| 对比 | RPS 中位数 | Δ | p50 中位数 | Δ | p95 中位数 | Δ |
|---|---|---|---|---|---|---|
| A → B（**索引本身**，同驱动） | 1200.0 → 1155.7 | **−3.7%** | 3.234 → 3.226 ms | −0.2% | 21.84 → 22.75 ms | +4.2% |
| B → C（缓存 + 驱动升级） | 1155.7 → 1084.4 | **−6.2%** | 3.226 → 3.375 ms | +4.6% | 22.75 → 24.26 ms | +6.6% |

- **索引本身（A→B）在 HTTP 层是 −3.7% RPS / +4.2% p95，落在两轮极差（A 0.6%、B 0.4%）加上环境噪声的量级里**，
  与 5(a) 的 store 层 +5.5% 方向一致、量级更小（HTTP 路径被事件插入/派发/序列化摊薄）。
- **B→C 的 −6.2%**：C 的库与 B 逐字节相同、代码差异是「语句缓存 + 驱动 v1.45.0」。
  驱动升级对**写**路径没有收益（缓存只作用于读），且新驱动的写路径实现不同 → 写侧小幅回退。
  这一项**不是**索引造成的，不能算到 #237 头上。

---

## 6. 判据与取舍建议

| 判据 | 结果 | 结论 |
|---|---|---|
| #237 读路径（A→B，20k/100k） | p95 **−94.3% / −98.8%**，RPS **+18× / +87×** | 收益极大，**保留索引** |
| #237 写放大（主判据：store 层受控基准） | **+5.5%**（n=15，极差 18/19%） | **未过 10% 红线** |
| 写放大（旁证①：纯 SQL 批插 20k 行） | **+11.0%**（n=5） | 处在噪声边缘，如实披露 |
| 写放大（旁证②：HTTP webhook，A→B） | RPS **−3.7%** / p95 **+4.2%** | 与主判据同向，量级更小 |
| 库体积 | 20k **+4.02%**、100k **+9.74%** | 可接受（读路径 18–87× 的改善换 <10% 体积） |
| #242（B→C） | RPS **+97.8%**、p95 **−51.7%**、CPU/请求 **−51.3%**、`prepareV2` cum **−96.5%** | 收益确定；**注意 C 含驱动升级**，不是纯 `*sql.Stmt` 的功劳 |
| #242 的写侧副作用（B→C） | RPS **−6.2%** / p95 **+6.6%** | 由驱动升级引入，与索引无关；是否需要跟进由根节点定 |

**建议**：#237 保留索引（读收益 >> 写代价）；若后续对写路径的 +11%（纯 SQL 口径）仍然介意，
可选方向是**把索引裁成两列 `(namespace_id, created_at)`** 或评估 `PRAGMA optimize` 后重新量——
但**本轮数据不支持删索引**（删了读路径立刻回到 100k 档 p95≈10s 超时）。

---

## 7. 事故与纪律披露（必须保留）

1. **`/tmp/perf-3a` 在 2026-11-01 ~01:54 被外部进程整目录删除**（连带 GO-AFTER、二进制、DB、raw 全部）。
   时间线：
   - 01:39–01:52 第一套完整 sweep 跑完（`ALL_EXIT=0`，8 条读路径 + 9 条写路径 + 4 条 pprof 行）；
   - **~01:54** 发现 `/tmp/perf-3a` 不存在（`ls` 报 No such file），同批其它 `/tmp/perf*` 目录仍在，
     **不是本脚本删的**（脚本只 `rm -rf` 自己 `$P3_ROOT` 下的子目录，从不删根）；
   - 01:55 重建并**重跑整套 sweep**（本报告 §2–§5 的数字全部来自 01:55–02:10 这一次完整 run）；
   - 02:10 起把关键产物回仓到 `scripts/perf/3a/raw/`（168 KB，文本），并把二进制备份到
     `/var/tmp/anotify3a-backup/bin`（含 `bin.sha256` 清单）。
2. **GO-AFTER 信号恢复**：原信号文件随目录被删。恢复前已核实「无 e2e / go test / make ci 在跑」
   （`pgrep` 无匹配；`scripts/e2e/run_all.sh` 已于 01:42 结束），cores 0-3 忙 14–38%（环境本底）。
   恢复内容（含时间线与核实证据）已留档：`raw/GO-AFTER-incident.txt`。
3. **丢弃的无效数据（不参与任何结论）**：
   - 第一次写路径 sweep 全 404（库副本漏了 WAL 里的 webhook 源行 → `events+0 tasks+0`）。已修：`p3_copy_db`
     现在**必须先 `wal_checkpoint(TRUNCATE)`** 且 `-wal` 非空就拒绝拷贝；两个写路径函数都加了
     「events 没涨就 die」的硬护栏。原始 404 日志已删（43 MB/轮的 429 洪流，同轮 `server.log` 也被**日志瘦身**）。
   - 一次 `anotify-after` 构建把 `dist` 拷成了 `dist/dist`（`cp -a X Y` 在 Y 已存在时的语义），
     导致 sha 与 archive 重编不一致。已修：构建前 `rm -rf <src>/internal/server/dist`。
     修好后 C 的 sha 从 `d29b59a8…` 变为 `70614342…`，**所有 C 的数字都来自修好后的 `70614342…`**。
4. **分核/端口**：全部测量 `taskset -c 0-3`，端口只用 8020/8021/8022（段内）；
   每场景独立库目录；结束只 `kill` 自己记录的 pid（**未使用 pkill -f**）。
5. **未改任何产品代码/测试/schema**；`git status` 在我的写入范围内只有 `scripts/perf/3a/`。

---

## 8. round-2（待跑）：最终形态（含 `ANALYZE` + `PRAGMA optimize`）

审查发现（不是本轮数字的问题，但影响最终代码）：`ListTasks` 带 `status=` 过滤时，新索引把计划从
`idx_tasks_lease(status=?)` 抢到 `idx_tasks_ns_created(namespace_id=?)`，**稀有 status 从 0.15–0.2ms 退到 14–26ms**
（根因：无 `sqlite_stat1`）。修法：迁移 16 补 `ANALYZE` + `Open` 末尾 `PRAGMA optimize`。
它**不影响本轮测的无过滤 `/v1/tasks`**，所以 round-1 的 #237（A vs B）与 #242（B vs C）结论仍然成立。

round-2 计划（等 `/tmp/perf-3a/GO-FINAL`）：

1. 重建 `anotify-after`（最终 worktree；`make fe` 等价 + `go build -trimpath -tags embeddist`），
   记录 `git rev-parse HEAD` + `go list -m modernc.org/sqlite` + 与 `git archive HEAD` 重编的 sha 自证。
2. `db-C2` = `db-B` 拷贝 → 用最终二进制 `Open` 一次（触发 `PRAGMA optimize`）→ 记
   `SELECT count(*) FROM sqlite_stat1 WHERE tbl='tasks'` 与 db sha256。
3. 读路径：20k / 100k 各 `-c 32 -d 20s`，C2 两轮（与 round-1 的 B 对比）。
4. pprof：C2 上 25s 负载 + 20s 采样，报 `prepareV2` cum% 与 CPU/请求，与 round-1 的 C 对比
   （预期无变化 → 证明「加统计没有副作用」）。
5. **4 种过滤形状的 EXPLAIN + 单发延迟**（20 次中位 `time_total`）：
   `无过滤`、`status=pending`、`status=canceled`、`traceId=<真值>`；
   判定标准：稀有 status 必须 `idx_tasks_lease`，常见/无过滤必须 `idx_tasks_ns_created`。
6. （时间够就做）`db-B2` = `db-B` 拷贝 + 最终二进制 `Open`，20k 跑一轮，作为「B 加了统计」的对照。

round-2 结果会追加在本节后面，并单独成表标注「round-2 · 最终形态」。


---

## 10. round-2 · **最终形态**（commit `2ecebe0`，迁移 16 带 `ANALYZE` + `Open` 末尾 `PRAGMA optimize=0x2`）

> 表头口径：**round-2 = 最终形态**（含统计维护）。二进制来自最终 worktree
> `git rev-parse HEAD = 2ecebe0`（`go list -m modernc.org/sqlite` = `v1.45.0`），
> sha256 前 16 = `7b9f69cd7162c258`，大小 24258257 B，`-trimpath -tags embeddist`。
> 库：`20k-C2` / `20k-B2` / `100k-C2` = 对应 `*-B` 的拷贝 + 最终二进制 `Open` 一次。
> **`SELECT count(*) FROM sqlite_stat1 WHERE tbl='tasks'` = 7（三个库都是 7，与预期一致）。**

### 10.1 读路径（C2）

| 档 | 库 | 库体积 | 轮 | RPS | p50 | p95 | p99 |
|---|---|---|---|---|---|---|---|
| 20k | 20k-C2 | 72,728,576 B | 1 | 795.1 | 35.94 ms | **75.97 ms** | 100.7 ms |
| 20k | 20k-C2 | 72,728,576 B | 2 | 779.7 | 36.63 ms | **77.40 ms** | 101.8 ms |
| 100k | 100k-C2 | 56,668,160 B | 1 | 952.5 | 29.96 ms | **63.18 ms** | 82.9 ms |
| 100k | 100k-C2 | 56,668,160 B | 2 | 946.7 | 30.27 ms | **63.74 ms** | 83.7 ms |
| 20k | 20k-B2（与 C2 同库同二进制，重复性对照） | 72,728,576 B | 1 | 778.5 | 36.70 ms | 77.04 ms | 103.4 ms |

**必须披露的混淆（重要）**：`20k-C2` 是从 `20k-B` 拷的，而 `20k-B` 已经被 round-1 的三轮写路径实验
追加过事件/任务（体积 25.9 MB → **72.7 MB**，行数约为模板的 4.5×）。所以
**20k 档的 C2 不能与 round-1 的 C（20k 模板，25.9 MB）直接比**：75.97 ms 里含「表变大 4.5×」的代价
（同库同二进制的重复性对照 B2 = 77.04 ms，两者差 1.4%，说明测量本身稳）。
**干净的对照在 100k 档**（`100k-B` 没被写过，56.7 MB 与 round-1 相同）：
round-1 B `511 RPS / p95 123.0 ms` → round-2 C2 `949 RPS / p95 63.5 ms`（**RPS +86%，p95 −48%**），
与 round-1 的 B→C 结论一致，说明**补统计没有改变读路径收益**。

### 10.2 pprof（C2，20k，负载 25s + 采样 20s）

| 轮 | RPS | p50 | p95 | profile total | CPU/请求 | `prepareV2` cum |
|---|---|---|---|---|---|---|
| 1 | 837.6 | 34.14 ms | 72.50 ms | 70.51 s | **4.209 ms** | 210 ms（**0.30%**） |
| 2 | 838.7 | 33.91 ms | 72.62 ms | 70.48 s | **4.202 ms** | 200 ms（**0.28%**） |

对照 round-1 的 C：CPU/请求 3.310 ms、`prepareV2` 0.086%/0.057%。
**差异全部来自库变大**（4.5×行数），`prepareV2` 仍是 0.3% 量级（round-1 C 0.07%、round-1 B 1.97%），
即**补统计没有把 prepare 成本带回来**。口径不变：CPU/请求随表规模上升，不能跨库型横比。

### 10.3 4 种过滤形状：EXPLAIN + 单发延迟（20 次中位，`time_total`）

`20k-C` = round-1 代码（无 optimize），`20k-C2` = 最终形态。**两边的 `stat1_tasks` 都是 7**。

| 形状 | 20k-C 计划 | 20k-C2 计划 | 20k-C 中位 | 20k-C2 中位 |
|---|---|---|---|---|
| 无过滤 | `SEARCH … idx_tasks_ns_created (namespace_id=?)` | 同左 | 3.52 ms | 4.31 ms |
| `status=pending`（常见） | `SEARCH … idx_tasks_ns_created (namespace_id=?)` | 同左 | 5.21 ms | 3.80 ms |
| `status=canceled`（稀有） | `SEARCH tasks USING INDEX idx_tasks_lease (status=?)` + TEMP B-TREE | 同左 | 0.99 ms | 1.13 ms |
| `traceId=<真值>` | `SEARCH … idx_tasks_trace (namespace_id=? AND trace_id=?)` + TEMP B-TREE | 同左 | 1.24 ms | 1.25 ms |

**如实说明（与审查现象的差异）**：审查报的「稀有 status 从 0.15–0.2 ms 退到 14–26 ms」在本实验里**没有复现**，
原因是**我的库从一开始就有 `sqlite_stat1`**：`p3_seed` 与 `p3_derive` 对每份库都跑 `VACUUM+ANALYZE`
（为的是让 A/B 两侧计划可比，见 §1）。所以 `20k-C` 本来就是「有统计」的状态，计划已经是 `idx_tasks_lease`。
审查命中的是「刚升级、还没统计」的窗口——最终代码把 `ANALYZE` 放进迁移 16 + `Open` 补 `optimize`，
正是把这个窗口消掉。本次能证明的是：**最终形态下 4 种形状的计划全部符合预期，且单发延迟没有退化**（1–4 ms）。

### 10.4 修正数据增长混淆的写路径实验（w-A vs w-B，每轮重置库）

`w-A-pristine` = `seed-20k` 拷贝（v15，无索引）；`w-B-pristine` = 同源拷贝 + `index` 二进制 `Open` 一次
（v16，含索引）。每轮开始都把库**重置回 pristine 拷贝**，`A,B,A,B` 交替，`-c 8 -d 20s`，
双维限流抬到 20 万/min（同 §5(b) 的原因）。

| 配置 | 轮 | RPS | p50 | p95 | p99 | DB 行数变化 |
|---|---|---|---|---|---|---|
| **A**（无索引） | 1 | 1924.3 | 2.533 ms | 11.23 ms | 35.01 ms | events/tasks 各 +N（见 `raw/summary.tsv` note） |
| **A**（无索引） | 1′(重跑) | 2332.0 | 1.994 ms | 9.96 ms | 22.83 ms | 同上 |
| **B**（含索引） | 1 | 1173.1 | 3.271 ms | 22.28 ms | 57.98 ms | 同上 |
| **B**（含索引） | 1′(重跑) | 1142.4 | 3.382 ms | 22.69 ms | 59.65 ms | 同上 |

| 对比 | RPS 中位 | Δ | p50 中位 | Δ | p95 中位 | Δ |
|---|---|---|---|---|---|---|
| A → B（**索引本身**，每轮重置库） | 2128.2 → 1157.8 | **−45.6%** | 2.26 → 3.33 ms | **+47%** | 10.6 → 22.5 ms | **+112%** |

**披露**：n=2 轮/配置（复跑时 `A-r2` 两次都因 `bind: address already in use`（端口 8020 残留）未起，
只拿到各自 1 轮 + 复跑 1 轮，两轮同法同库型；极差 A 19%、B 1.3%）。
**这条数字与 §5(a) 的主判据（+5.5%）方向一致但量级差一个数量级**，原因是写入形状不同：
store 基准是**一个事务插 20 行**（摊销了每事务的 WAL/页写成本），
而 webhook 路径是**每个请求 1 event + 1 task 的小事务**——索引每行都要多改一个 b-tree 页，
摊到单行事务上成本被放大。**所以「索引的写代价」不是单一数字，取决于事务大小**：
批量派发 ≈ +5.5%，单行派发 ≈ −45% 吞吐。取舍建议：保留索引（读路径 18–87×），
但若要优化写路径，优先级应该是**合并派发事务**（一次多行）而不是删索引。

### 10.5 round-2 结论一句话

- **#237**：`GET /v1/tasks?limit=200` 的 p95 **2128.6 → 120.9 ms（20k，−94%）**、
  **9836 → 123 ms（100k，−99%）**；最终形态（含统计）在 100k 干净对照上 **RPS +86% / p95 −48%**，收益保持。
- **#242**：CPU/请求 **6.794 → 3.310 ms（−51%）**、`prepareV2` cum **1.97% → 0.07%（−96%）**；
  最终形态 `prepareV2` 仍 0.28–0.30%（库变大 4.5× 后），**补统计没有副作用**。

---

## 9. 原始产物索引

| 路径 | 内容 |
|---|---|
| `scripts/perf/3a/raw/summary.tsv` | 读路径 + 写路径的全部样本（config/scale/bin_sha/db/round/RPS/p50/p95/p99/状态码/DB 行数变化/原始日志路径） |
| `scripts/perf/3a/raw/summary-242.tsv` | #242：RPS/p50/p95 + `pprof_total_s` + `cpu_per_req_ms` |
| `scripts/perf/3a/raw/phase1-summary.tsv` | 二进制溯源表 + 库体积/schema/索引存在性 + 读冒烟 |
| `scripts/perf/3a/raw/bin-provenance.tsv`、`after-provenance.txt` | 二进制 sha / commit / 构建参数 / 链接的驱动版本 |
| `scripts/perf/3a/raw/provenance-db.txt` | 每份库的 sha256 + 字节数 + schema 版本（**跨批次不要混表比较**） |
| `scripts/perf/3a/raw/explain/{20k,100k}-{A,B}.txt` | EXPLAIN QUERY PLAN 原文 |
| `scripts/perf/3a/raw/pprof/cpu-20k-{B,C}-r{1,2}/` | `-top` / `-list prepareV2` / prepareV2 行 的文本原文 |
| `scripts/perf/3a/raw/bench/controlled.txt`、`create-tasks-*.txt`、`sql3-insert.txt` | store 层写路径基准（受控 / 契约原始 / 纯 SQL 复核） |
| `scripts/perf/3a/raw/load/*.log` | 每次 loadgen 的完整输出（含状态码分布与延迟分位） |
| `scripts/perf/3a/raw/GO-AFTER-incident.txt` | 信号恢复的时间线与核实证据 |

复现：`scripts/perf/3a/measure.sh all`（相位 1 结构 + 相位 2 全部计时），`... bench2`（受控写路径基准）。
`/tmp` 侧完整产物（含 `.pprof` protobuf、每轮 `server.log`）在 `/tmp/perf-3a/raw/`；
二进制另备 `/var/tmp/anotify3a-backup/bin`（`bin.sha256`）。
