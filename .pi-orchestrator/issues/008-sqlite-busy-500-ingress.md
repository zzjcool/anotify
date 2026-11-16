---
id: 008
kind: bug
area: [server]
size: M
severity: P1
state: open
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

## 需要侦察确认

- 触发点：`store.GetSystemConfig` 走 `BeginTx`（DSN `_txlock=immediate`），**读也要拿写锁**；
  在 ingest 热路径上被调用（`source: 读命名空间输入版本`）。
- 影响面：哪些读路径在热路径上用 `BeginTx`（而不是只读事务/无事务读）；
- 正确修法候选：
  a) 只读路径不用 `IMMEDIATE`（改成默认 deferred 或 `BEGIN` 读事务）；
  b) 把 `SQLITE_BUSY` 映射成 503 + `Retry-After`（与背压同码），并缩短该路径的 busy timeout；
  c) 两者都做：读路径去写锁 + 兜底映射。
- 必须确认：改读事务锁模式不会破坏「读一致性」或与现有 CAS 语义冲突（`internal/store/AGENTS.md` §3.4/§3.6）。

## 验收

- 高并发压测下不再出现 `500 … database is locked`；若仍偶发，必须是 **503 + Retry-After**。
- 有测试能复现（并发写 + 该读路径），不是只靠压测撞。
- `make contract` 绿；不改断言迁就。

## 不做

- 不把 busy timeout 调大（那是把 5s 变成 10s，更糟）。
- 不吞掉错误让对方静默成功。
