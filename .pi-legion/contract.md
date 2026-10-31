# 契约 · 批次 2A：鉴权热路径（#227 / #245 / #241）

冻结日期：本轮。任何改动此文件必须经 `legion_phase` 走修复流程。

## 0. 文件所有权（唯一 owner，互不重叠）

| owner | 可写文件 | 禁止 |
|---|---|---|
| **A-track**（#227） | `internal/auth/apikey.go`、`internal/auth/apikey_cache.go`(新)、`internal/auth/apikey_cache_test.go`(新)、`internal/auth/apikey_bench_test.go` | 其它一切 |
| **B-track**（#245） | `internal/server/ratelimit.go`、`internal/server/middleware.go`、`internal/server/source_handlers.go`（**仅** `hook()` 里限流那 3 行）、`internal/server/hook_ratelimit_test.go`、`internal/server/config.go`（**仅**新增 hook 限流两个字段） | 其它一切 |
| **C-track**（#241） | `internal/auth/session.go`、`internal/auth/session_cache.go`(新)、`internal/auth/session_cache_test.go`(新)、`internal/server/auth_handlers.go`（**仅** logout/sessions revoke 失效钩子） | 其它一切 |
| **centurion**（本人） | `make ci` 修桥、`internal/server/mux.go`（env 接线）、`internal/auth/webauthn.go`（Config + NewService 转发 TTL）、`internal/server/handlers.go`+`admin_handlers.go`（各 1 行缓存失效钩子）、perf 脚本/报告 `scripts/perf/2a-auth/**` | — |

**红线**：`internal/store/**` 已有签名一个字符都不许改；`internal/ws/**`、`schema.sql` 只读禁写。

## 1. #227 · API Key 验证结果缓存

### 1.1 现状
`internal/auth/apikey.go:ValidateKey` 每请求：`GetAPIKeyByPrefix`(DB) + `argon2.IDKey(m=64MB,p=4)`(≈30ms/64MB) + `TouchAPIKey`(写事务)。

### 1.2 目标设计
- `KeyManager` 增加**已验证结果缓存**：`map[[32]byte]*keyEntry`，键 = `sha256(plaintext)`。
- `keyEntry{userID string, scopes []string, keyID string, expiresAt time.Time, lastTouched atomic.Int64}`。
- 命中路径顺序（**必须**是这个顺序，任何一步不通过都回落到完整校验）：
  1. `strings.HasPrefix(k, "ant_")` 不满足 → 立即拒绝（**不进缓存**）。
  2. `sha256(k)` → 查表；命中且 `now < expiresAt` → **直接返回**（0 次 DB、0 次 argon2）。
  3. 未命中 → 原来那条路：`GetAPIKeyByPrefix` → `!Enabled` 拒绝 → `verifyKey`(argon2) → 成功后写缓存。
- **负结果绝不缓存**（防 key 喷洒把内存打爆）：查不到前缀 / 校验失败 / `!Enabled` 直接返回错误。
- 缓存容量硬上限 `maxEntries = 4096`；超限时惰性清理过期项，仍超限 → **不写入**（不驱逐有效条目，避免被喷洒挤掉热 key）。
- TTL 由 `KeyManager.cacheTTL` 控制，默认 `60s`，注入点 `NewKeyManager(db)` 保持签名 → 新增 `NewKeyManagerTuned(db, opts)`；默认构造器给默认值。env 接线在 mux.go：`ANOTIFY_APIKEY_CACHE_TTL`（`0`/空 = 用默认；`-1`/`off` = 关缓存）。

### 1.3 写路径失效（安全边界，必须全部做到）
- `KeyManager.RevokeKey(id)`：`db.RevokeAPIKey(id)` **成功之后**删掉缓存里所有 `keyID == id` 的条目。
- `KeyManager.CreateKey` 不写缓存（无必要）。
- `KeyManager.InvalidateAll()`：清空（测试/运维用）。
- `keysHandler.revoke`（`internal/server/handlers.go`）改调 `h.keys.RevokeKey(id)`（1 行）。
- **失效必须是同步的、在 DB 写成功之后**：DB 失败则不清缓存（此时 key 仍然有效，清了只是白丢性能）。

### 1.4 安全不降级（逐条写进测试）
| 场景 | 期望 | 测试 |
|---|---|---|
| 错 key（同前缀、不同随机段） | 401 | `TestAPIKeyCacheRejectsWrongKey` |
| 过期缓存条目 | 回落完整校验，仍正确 | `TestAPIKeyCacheExpiredEntryFallsBack` |
| 已 revoke key（缓存热时 revoke） | **立即** 401，不等 TTL | `TestAPIKeyRevokeInvalidatesCache` |
| Key 前缀不对（无 `ant_`） | 401 且不进缓存 | `TestAPIKeyRejectsNonAntPrefixUncached` |
| 缓存内容不含明文/半明文 | 结构体只有 sha256 摘要 + userID/scopes/keyID | `TestAPIKeyCacheHoldsNoPlaintext` |
| 并发下缓存与 DB 一致 | `-race` 通过 | `TestAPIKeyCacheConcurrent` |

**明确不做（会降级安全）**：不缓存明文 key、不缓存 argon2 salt/hash、不跳过 `verifyKey` 的常量时间比对、不做"前缀命中即放行"。

### 1.5 必须交付的数字
- `go test -bench 'BenchmarkValidateKey' -benchmem ./internal/auth/`：缓存命中单次 ns/op、B/op、allocs/op；对照未命中（argon2 路径）。
- `runtime.MemStats`/RSS：命中路径**不产生 64MB 分配**（用 `testing.AllocsPerRun` 或 bench 的 B/op 直接给）。

## 2. #245 · hooks 限流

### 2.1 现状
`internal/server/ratelimit.go:rlHookPerMin=60`（IP 单维、固定窗口）；`rateLimited()` 写一条 WARN，accessLog 再写一条 WARN（429 双写）。

### 2.2 目标
- **双维度**：`rlHookToken`（按 `{token}` 路径参数）+ `rlHookIP`（按 `clientIP`）。**两维都必须放行才算放行**，任一维 429 → 429。
- **可配置**（`internal/server/config.go` 新增两个字段 + `FromEnv` 读取）：
  - `ANOTIFY_HOOK_RATE_PER_MIN` → 每 token 每分钟，默认 **600**（10 RPS）
  - `ANOTIFY_HOOK_RATE_PER_IP_PER_MIN` → 每 IP 每分钟，默认 **1200**（20 RPS，留出多源同 IP 的余量）
  - 值 `0` = 用默认；`<0`（如 `-1`/`off`）= 关闭该维度限流（显式运维选择，需打一条 WARN）。
- `NewApp` 里按配置重建两个限速器（包级变量，测试可替换）。
- **429 只写一份日志**：删掉 `rateLimited()` 里的 `slog.Warn("rate limited", …)`；改由最外层 `accessLog` 的 429 分支写**唯一一行**，并带上 `retry_after` 与 `rate_limit`（维度名）。传递方式：`rateLimited` 通过类型断言往 `*statusWriter` 上塞字段（`sw.retryAfter`、`sw.rlScope`），不改 `http.ResponseWriter` 签名。
- **Retry-After 真实**：固定窗口剩余秒数（`ceil(window - (now - bucketAt))`），不是硬编码 60。
- 保留 `rateLimited(w, rl, key)` 旧签名（enroll/recovery 等调用点不动），新增 `rateLimitedHook(w, r, token)` 之类内部实现。

### 2.3 测试
- 既有 `hook_ratelimit_test.go` 三条断言（额度内不拒 / 超限 429 / 换 token 不能绕过 IP 维度）**一条都不许放松**，只允许把测试 helper 里额外重置 `rlHookToken`。
- 新增：token 维度独立生效（同 IP 不同 token，各有自己的 token 额度被耗尽）、Retry-After ∈ (0,60]、429 只产生 1 条日志（用 `slog` 的 `slog.SetDefault` + 计数 handler 断言）。

## 3. #241 · 会话鉴权链

### 3.1 现状（每请求 4 次 DB 往返）
`session.go:Middleware`：`GetSession` + `GetUserByID`(只为 `Disabled`) + `TouchSession`(**同步写事务**) + 下游 `AuthorizeNamespace`。

### 3.2 目标
1. **会话缓存**（`SessionManager`）：`map[string]*sessEntry{sessID → userID, expiresAt, disabled, cachedAt}`，TTL 默认 **10s**（env `ANOTIFY_SESSION_CACHE_TTL`，同 1.2 的 0/-1 语义）。
   - 命中且 `now < cachedAt+TTL` 且 `now < expiresAt` → **跳过 GetSession + GetUserByID 两次往返**。
   - 未命中 → `GetSession` → 过期检查 → `GetUserByID`（`err != nil` 时**显式失败**而不是静默通过 —— 修 #241 §5 的顺带发现）→ 写缓存。
   - 容量上限 8192，超限不写（同 1.2 的理由）。
2. **TouchSession 节流 + 异步**：
   - 缓存条目上带 `lastTouch atomic.Int64`（unix 秒）。
   - 只有 `now - lastTouch >= touchInterval`（默认 60s）**并且** CAS 成功的那**一个** goroutine 才 `go func(){ _ = db.TouchSession(...) }()`。
   - **不丢更新**的证明：`TestTouchSessionNoLostUpdate` —— N 个并发请求，断言 (a) DB 写次数 ≤ ceil(N/…) 有界且 ≥1，(b) 最终 `last_seen` 单调不减且等于某个 ≥ 起始值的时间戳，(c) 用 `-race` 跑。节流窗口内多次请求只会写一次，且写的是**最新**时间。
3. ~~**可选合并**：新增只读函数 `store.GetSessionAuth(sessionID)`~~ → **【冻结修订 1】放弃**。理由：本轮有 3 个 centurion 并行改 `internal/store/**`，为本条 0.02ms/次（且只在 10s 一次的缓存未命中时发生）的收益去碰 store 文件不值得。记录为「未做项」，不影响任何验收数字（命中路径本来就是 0 次往返）。
4. 写路径失效：`SessionManager.Revoke` / `DeleteByUser` 删缓存条目；`adminHandler.applyDisabledChange` 成功后调 `h.svc.Sessions().InvalidateUser(id)`（1 行）；`auth_handler.logout` 已走 `Revoke` 自动失效。

### 3.3 测试
- `TestSessionCacheHitSkipsDB`：缓存热后把 DB 关掉/换一个空 DB，中间件仍放行到 TTL 边界。
- `TestSessionRevokeInvalidatesCache`：热缓存后 Revoke → 立刻 401。
- `TestSessionDisabledUserRejectsAndInvalidates`。
- `TestTouchSessionNoLostUpdate`（见上）。
- `TestSessionUserLookupErrorFailsClosed`：`GetUserByID` 失败必须 401，不能静默放行。

## 4. 验收（机器门禁）

```
legion-verify:
  - make ci
  - go test ./internal/auth/... ./internal/server/... -count=1 -race
```

- `make ci` 全绿（含 gofmt / vet / build / 单测）。
- 性能对比（centurion 本人在 `taskset -c 4-7`、端口 7980-7999 上跑，前后同一命令）：
  - Bearer 写路径 RPS（`POST /v1/events`，`-c 8`）
  - `-c 8` 时 RSS
  - 读路径 RPS（`GET /v1/tasks` 带 Cookie，`-c 32`）
  - hooks 路径 RPS + 日志增量（限流前）
- 数字落盘 `scripts/perf/2a-auth/REPORT.md`（新增目录，centurion owner）。

## 5. 交付格式

每个 track 结束必须给：

```
DONE <track>
产出文件: …
自测命令与结果: …
安全边界（逐条对应 §1.4/§2.3/§3.3）: …
发现的产品 bug（不许改断言迁就）: …
遗留风险: …
```

## 6. 未做项（预先声明）
- 不加 Redis/外部缓存（单进程 SQLite，进程内缓存足够）。
- 不改 `AuthorizeNamespace`（属 #240/#241 的另一半，跨 store 边界）。
- 不做滑动窗口/令牌桶（固定窗口 + 真实 Retry-After 已满足 issue 诉求；滑动窗口是策略变更，不是测量结论）。
