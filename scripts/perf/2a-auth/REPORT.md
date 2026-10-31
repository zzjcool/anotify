# 批次 2A · 鉴权热路径修复前后对比 (#227 / #245 / #241)

测量环境与纪律（与 issue #217 的评估口径一致，保证可复现）：

- 分核 `taskset -c 4-7`（服务与压测器同核组），端口 `7980`，库为每次新建的临时 SQLite
- 压测器 `scripts/perf/loadgen`，服务二进制 `-trimpath -tags embeddist`
- **before** = 本批改动前的 `HEAD`（a39ca89）构建的二进制，冻结在 `/tmp/perf-2a/bin/anotify-before`
- **after** = 本分支构建，`/tmp/perf-2a/bin/anotify-after`
- 脚本：`scripts/perf/2a-auth/{ab.sh,rss-vs-concurrency.sh}`（同一命令跑两个二进制）
- 机器上同时有别的子树在跑 CI，load average 在 2.7–19 之间波动。**下面的数字都标了当时的 load**；
  为了抗噪，关键项做了两轮交替（before/after/before/after），并对同一并发下的**每请求**成本做归一化。

---

## 1. #227 · Bearer 写路径 RPS（`POST /v1/events`，`-c 8`，10s）

| 轮次 | load | before | after | 倍数 |
|---|---|---|---|---|
| 低载那轮（c=8, 5s，`rss-vs-concurrency.sh`） | 2.7 / 3.7 | **41.4 RPS** | **2,220.5 RPS** | **53.7×** |
| ab.sh 第 1 轮（c=8, 10s） | 16.5 / 16.5 | 32.3 RPS | 1,203.1 RPS | 37.2× |
| ab.sh 第 2 轮（c=8, 10s） | 15.5 / 11.3 | 37.2 RPS | 2,124.2 RPS | 57.1× |
| 修复前较早两次（低载） | 3.6 / 4.5 | 40.8 / 40.5 RPS | — | — |

全部状态码 `200`（不是半途 400）：`200×323 / 200×12031` 等，即真的走完「鉴权 → 落库 → 派发」。
p50 从 **203–233 ms** 降到 **3.4–5.0 ms**。

> 对照 issue #227 的原始结论「4 核 ~35 RPS」：after 在 **load 更高**的窗口里仍然两个数量级
> 以上。上限已经不在认证上（剩余成本是 SQLite 单写者 + 事件派发）。

## 2. #227 · RSS：`RSS ≈ 并发 × 128MB` 这条曲线没了

峰值 RSS 用内核维护的 `VmHWM`（比定时采样更抓得住 argon2 那种秒级脉冲），固定 5s、递增并发：

| 并发 | before base / **peak** | after base / **peak** | before 请求数 | after 请求数 |
|---|---|---|---|---|
| 1 | 14.0 / **146.9 MB** | 18.1 / **160.9 MB** | 175 | 7,774 |
| 4 | 14.1 / **531.3 MB** | 18.1 / **438.9 MB** | 210 | 11,144 |
| 8 | 14.0 / **1,043.4 MB** | 18.0 / **695.6 MB** | 207 | 11,103 |

**before 的斜率 ≈ 128 MB / 并发**（147 → 531 → 1043，正好是 `64MB × GOGC 2×`），与 issue 的描述吻合。

**但「同一时长」的绝对 RSS 不是公平对比**：after 在同一个 5s 里跑了 **53 倍**的请求，
绝对内存自然被工作负载推高。归一化后（峰值 / 请求数）：

| | before | after | 倍数 |
|---|---|---|---|
| c=8 峰值 / 请求数 | 1043.4 / 207 = **5.04 MB/请求** | 695.6 / 11103 = **0.063 MB/请求** | **80×** |

### 决定性对照：同一个 after 二进制，只切缓存开关

| after 二进制 | 峰值 RSS | 请求数 | RPS |
|---|---|---|---|
| `ANOTIFY_APIKEY_CACHE_TTL=-1`（**缓存关**） | **1,048.0 MB** | 172 | 34.4 |
| 默认（**缓存开**） | **547.9 MB** | 6,572 | 1,314.1 |

缓存关掉后 after 精确复现了 before 的 ~1 GB / ~34 RPS —— 证明那 64 MB/请求的分配确实是这个
缓存消掉的，剩下的 RSS 是「38 倍请求量」的正常工作内存，不是认证。

### 命中缓存的单请求成本（`internal/auth` bench，`-benchtime 20x -benchmem`）

| 路径 | ns/op | B/op | allocs/op |
|---|---|---|---|
| **缓存命中** | **3,391** | **64** | **2** |
| 缓存未命中（argon2id 全量） | 31,407,779 | 67,120,790 | 122 |

**加速 9,263×；内存 64 B vs 67.1 MB（约 10⁶ 倍）。**
命中路径恒定为 0 次 DB 往返、0 次 argon2；64 B / 2 allocs = `[]byte(plaintext)` 48B + scopes 副本 16B。

> 本机 `time.Now()` 实测约 **2.6 µs/次**（无 vDSO），命中路径唯一一次取时钟就占了 3.4µs 里的大头；
> 缓存逻辑本身约 250–300 ns。生产机（`time.Now` ~50ns）预期命中在 **~300 ns** 量级。

## 3. #241 · 读路径 RPS（`GET /v1/tasks?namespaceId=…&limit=100`，带 Cookie，`-c 32`，15s）

两轮交替（同场次）：

| 轮次 | load | before | after | 倍数 | p50 before → after |
|---|---|---|---|---|---|
| 1 | 18.96 → 16.63 | 1,284.9 RPS | 4,134.5 RPS | 3.22× | 20.6 ms → 6.5 ms |
| 2 | 13.24 → 12.04 | 2,656.5 RPS | 4,423.4 RPS | 1.67× | 11.2 ms → 6.1 ms |
| 中位 | — | 1,971 RPS | 4,279 RPS | **2.17×** | — |

p50 从 11–21 ms 降到 6.1–6.5 ms，且**跨轮次稳定得多**（before 在 1285–2657 之间抖，after 在 4135–4423）。

DB 往返账（每请求）：

| 路径 | before | after |
|---|---|---|
| 会话缓存命中（10s 窗口内） | GetSession + GetUserByID + 同步 TouchSession = **3 次** | **0 次** |
| 会话缓存未命中 | 3 次 | **2 次**（随后入缓存） |
| `TouchSession` | **每请求 1 次同步写事务** | **每会话每 60s 1 次异步写**（实测 320 请求 / 2.02s → 写 2 次） |
| 下游 `AuthorizeNamespace` | 1 次 | 1 次（属 #240，本批未动） |

## 4. #245 · `/v1/hooks/{token}`

### 4.1 天花板（同一个端点，各自默认配置）

| | 10s 内成功数 | 429 数 | 429 占比 |
|---|---|---|---|
| before（60/min/IP） | **60** | 184,363 | 99.97 % |
| after（600/min/token + 1200/min/IP） | **600** | 187,351 | 99.68 % |

成功额度 **10×**（60 → 600/10s），与默认阈值改动一致。

### 4.2 429 的日志成本（把两边阈值都钉成 60/60，只比「每个 429 打几行」）

| | 429 数 | 日志字节 / 10s | 每 429 字节 | `ratelimit.hit` 行数 | accessLog 429 行数 |
|---|---|---|---|---|---|
| before-r2 | 181,189 | 60,689,725 | **335 B** | 181,204 | 181,204 |
| after-r2（阈值也钉 60/60） | 196,634 | 49,049,982 | **249 B** | **0** | 196,647 |

- **行数从 2 行/429 降到 1 行/429**（`ratelimit_warn_lines` 181,204 → **0**）。
- 每 429 的字节只降 26% 而不是 50%，因为剩下的那一行**变长了**（多了 `retry_after` 与
  `rate_limit` 两个字段）——这是有意的取舍：用一行的信息量换掉一行的重复。
- 同阈值下的吞吐 181,249 → 196,694 请求/10s（**+8.5%**），就是少写一半日志换来的。

### 4.3 `Retry-After`

从硬编码 `60` 改为**真实窗口剩余秒数**（向上取整，夹到 `[1,60]`）。

## 5. 安全回归（必须仍然被拒）

全部由测试锁住，`-race` 绿：

| 场景 | 测试 |
|---|---|
| 错 key（同前缀、不同随机段） | `internal/auth`: `TestAPIKeyCacheRejectsWrongKey` |
| 过期 key / 过期缓存条目 | `TestAPIKeyCacheExpiredEntryFallsBack` |
| 已 revoke key（缓存热时 revoke，**立即**拒绝） | `TestAPIKeyRevokeInvalidatesCache`、`TestAPIKeyRevokeKeyBumpsGeneration` |
| generation bump 后**全表**旧条目失效 | `TestAPIKeyGenerationBumpInvalidatesAllEntries`、`TestAPIKeyCacheGenerationBumpInvalidates` |
| 错 key 喷洒 230 次 → 缓存条目数恒为 0（不能变成内存 DoS） | `TestAPIKeyNegativeResultNeverCachedNoDoS` |
| 命中后仍做常量时间比对（verifier 改坏一字节必须不放行） | `TestAPIKeyCacheHitStillConstantTimeCompares` |
| 会话过期 / revoke / 用户被禁用 | `internal/auth`: `TestSessionExpiredRejected`、`TestSessionRevokeInvalidatesCache`、`TestSessionDisabledUserRejected` |
| `GetUserByID` 出错 **fail-closed**（旧实现静默放行） | `TestSessionUserLookupErrorFailsClosed` |
| 换 token 不能绕过限流（既有断言原样保留） | `internal/server`: `TestHookRateLimitedNotByToken` |
| HTTP 吊销 key 后缓存立即失效 | `internal/server`: `TestKeyRevokeViaHTTPInvalidatesCache` |
| 超管禁用用户后会话立即失效 | `internal/server`: `TestAdminDisableInvalidatesSessionCache` |

## 6. 复现

```bash
go build -trimpath -tags embeddist -o /tmp/perf-2a/bin/anotify-after ./cmd/server
go build -o /tmp/perf-2a/bin/loadgen ./scripts/perf/loadgen
go build -o /tmp/perf-2a/bin/devseed ./cmd/devseed

bash scripts/perf/2a-auth/rss-vs-concurrency.sh /tmp/perf-2a/bin/anotify-before before
bash scripts/perf/2a-auth/rss-vs-concurrency.sh /tmp/perf-2a/bin/anotify-after  after
bash scripts/perf/2a-auth/ab.sh /tmp/perf-2a/bin/anotify-before before
bash scripts/perf/2a-auth/ab.sh /tmp/perf-2a/bin/anotify-after  after
```
