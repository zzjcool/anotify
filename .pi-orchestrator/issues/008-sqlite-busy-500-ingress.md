---
id: 008
kind: bug
area: [server]
size: M
severity: P1
state: done
needs_recon: true
contract: []
depends_on: []
route: [scout, worker]
gates: ["go test ./internal/server/... ./internal/store/... -count=1"]
opened: 2026-11-16
---

# 008 · 写锁竞争时事件入口返回 **500**（`SQLITE_BUSY`），应当是 503 / 可重试

## 现象（perf soak 实测，2026-11-16）

压测中出现极低频（2/35245 ≈ 0.006%）但**语义错**的响应：

```
500  延迟 5.006s
{"error":{"code":"internal","message":"source: 读命名空间输入版本: get namespace config: begin: database is locked (5) (SQLITE_BUSY)"}}
```

## 为什么是 bug 而不是「意料之中的锁竞争」

`SQLITE_BUSY` 是**暂时性**失败：调用方重试就会成功。而现在：

1. 事件入口把它翻成 **500**（服务端错误）→ 客户端按语义不会重试，事件直接丢；
2. 延迟 **5.006s** 说明它在锁上等满了 busy timeout 才失败 —— 这 5 秒已经把连接和请求
   一起占住，属于「慢失败」；
3. 背压的语义是「超阈时回 503 + `Retry-After`」（现有实现见 `backpressure.go`），
   同一个入口对「暂时不可用」有两种互斥答案（503 会重试、500 不会），调用方无从区分。

## 侦察结论（2026-11-16，48 条 file:line 事实）

**根因是 DSN 全局开关，不是读语义**：`internal/store/store.go:104` 给 DSN 加了 `_txlock=immediate`，
驱动把它应用到该 DSN 的**每一条**连接 → 任何 `BeginTx` 都是 `BEGIN IMMEDIATE`，**没有按调用点降级的开关**。
busy_timeout = `store.go:102` 的 5000ms（现象 5.006s 逐字吻合）。

ingest 一次会拿 **3 次写锁**（2 读 + 1 写）：

| 步骤 | 位置 |
|---|---|
| 热点读 #1 | `internal/source/source.go:150` → `GetNamespaceConfig`（`store/config.go:495`，tx 在 `:499`） |
| 热点读 #2 | `source.go:363` `eventWindow` → `GetSystemConfig`（`config.go:349`，tx 在 `:350`） |
| 写 | `store/source_event.go:148`（AcceptSourceEvent） |

全仓 64 处 `BeginTx`，**只有 6 处是纯读**：`GetSystemConfig` / `GetNamespaceConfig` / `GetNodeConfig` /
`loadVarsLayers` / `EffectiveLimits` / `AuthorizedNamespaces`（后两个在派发/握手热路径）。

**已有的「无事务读」先例**（本次直接照抄形状，`queryExec` seam 见 `internal/store/txexec.go:36-47`）：
`node.go:1409` `nodeEligibility`（注释明写「读事务会先拿写锁，把并发派发串行化」）、
`task.go:1472` `OldestPendingCreatedAt`、`event.go:395` `GetEvent`、`source.go:407` `GetSourceByName`。

**禁改 `_txlock=immediate`**：三处测试 + 两条业务语义依赖它 —— 首个 admin 唯一性
（`identity.go:12-20`）、取消/放弃的线性化点（`task.go:906-912`）、全部 CAS 写。

**顺手发现的第二个 500（同形状）**：`internal/api/events.go` 的 `writeAPIError`（`:251`）
**没有**映射 `store.ErrNamespaceInputsChanged` 与 `*ResourceVersionConflictError`，
落到 `:275-277` 的默认 500 —— 而 `internal/server/api_status.go` 那边是同口径映射了的。
所以 **CAS 冲突 / ns 输入变更在 ingress 上也是 500**（应当 409）。

**这条决定了施工顺序**：如果只去掉 `GetNamespaceConfig` 的读事务而不补映射，
就会把「偶发 BUSY 500」换成「偶发 revision 500」——症状一模一样。

## 施工范围（冻结）

**Phase 1（本卡）**

1. `store`：`GetNamespaceConfig` 与 `GetSystemConfig` 改成**无事务读**（走 `d`，即 `queryExec`），
   照 `nodeEligibility` 的写法与注释风格；说明为什么安全：
   - `GetSystemConfig`：调用方 `eventWindow` 出错即回落默认值（`source.go:364-367`）；
   - `GetNamespaceConfig`：`Revision` 在 `AcceptSourceEvent` 的**写事务内**被再校验
     （`source_event.go:187-196` → `ErrNamespaceInputsChanged`），读侧不做 check-then-act。
   - 注意 `config.go:504` 的 `namespaceExistsOn(...)` 必须一起换成 `ex = d`（**忌只改一半**）。
2. `api/events.go`：`writeAPIError` 补两条映射，与 `server/api_status.go` 对齐：
   - `ErrNamespaceInputsChanged` → 409
   - `ErrResourceVersionConflict`（含 `*ResourceVersionConflictError`）→ 409
3. 单测：把 `internal/store/pool_test.go:170` 那条「写持锁期间读照常」的直读断言换成
   `GetNamespaceConfig`（改前必红、改后绿），并补 `GetSystemConfig` 同款。

**Phase 2（另开卡，不混进来）**

- 写事务本身撞 BUSY 时的语义（`500` → `503 + Retry-After`）需要动契约：
  `api/ingress.yaml:353` 的 503 只声明了 `Backpressure503`，body 被 `resource.yaml:1508-1523`
  冻结为 snake_case `BackpressureError`，`scripts/e2e/suites/capacity.mjs:197` 硬断言
  `error === "backpressure"`；`api-conventions.md:247-260` 的 reason 是**封闭集合**（无可用性类词）。
  这不是「加个 case」能了的事，单独裁决。

## 原始需要侦察确认

- 触发点：`store.GetSystemConfig` 走 `BeginTx`（DSN `_txlock=immediate`），**读也要拿写锁**；
  在 ingest 热路径上被调用（`source: 读命名空间输入版本`）。
- 影响面：哪些读路径在热路径上用 `BeginTx`（而不是只读事务/无事务读）；
- 正确修法候选：
  a) 只读路径不用 `IMMEDIATE`（改成默认 deferred 或 `BEGIN` 读事务）；
  b) 把 `SQLITE_BUSY` 映射成 503 + `Retry-After`（与背压同码），并缩短该路径的 busy timeout；
  c) 两者都做：读路径去写锁 + 兜底映射。
- 必须确认：改读事务锁模式不会破坏「读一致性」或与现有 CAS 语义冲突（`internal/store/AGENTS.md` §3.4/§3.6）。

## Phase 1 已交付（`61934c2` + 报告 `8e3edaa`）

| 改动 | 内容 |
|---|---|
| `internal/store/config.go` | `GetSystemConfig` / `GetNamespaceConfig` 改无事务读（`ex := d`，三条内部读共用同一 ex，防撕裂）；注释写明为什么仍安全 |
| `internal/api/events.go` | 补 `ErrNamespaceInputsChanged` → 409、`ErrResourceVersionConflict` → 409（含 typed），与 `server/api_status.go` 对齐 |
| 测试 | `pool_test` 的「写持锁期间读照常」换成两条**真热路径读**：改前 4.62s 红（`database is locked (5) (SQLITE_BUSY)`，就是线上那条文本），改后 0.44s 绿；`TestWriteAPIErrorConflictMapping` 4 子用例（含「未知错误仍 500」反向对照） |

**ingest 一次请求的写锁次数：3 → 1**（只剩 `AcceptSourceEvent` 那次真正的写）。

门禁（我复核过）：`go test ./internal/store/... ./internal/api/... ./internal/server/...` 全绿、`make contract` 通过、`gofmt -l` 无输出。

**未做**：写事务自身撞 BUSY 仍是 500（→ 见 [010](010-busy-on-write-path-503.md)）；
读侧从「单事务快照」降级为「三条独立快照」（写侧无影响，两处**展示**投影在并发 PUT 时可能早一拍，CAS 仍安全）。

## 验收

- 高并发压测下不再出现 `500 … database is locked`；若仍偶发，必须是 **503 + Retry-After**。
- 有测试能复现（并发写 + 该读路径），不是只靠压测撞。
- `make contract` 绿；不改断言迁就。

## 不做

- 不把 busy timeout 调大（那是把 5s 变成 10s，更糟）。
- 不吞掉错误让对方静默成功。
