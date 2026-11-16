---
id: 001
kind: docs
area: [server]
size: S
severity: P3
state: done
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/server/ -run TestOpenAPIParity -count=1", "gofmt -l internal/server/resource_capability.go"]
opened: 2026-11-16
closed: 2026-11-16
---

# 001 · `register()` 注释谎称「ServeMux 自动 405」

## 现象

`internal/server/resource_capability.go` 的 `register()` 上方注释写：

> 方法不匹配（例如 `PUT …/capabilities/{name}`）由 Go 1.22 ServeMux 自己回 405。

生产 mux **不是**这样。`mux.go` 末尾有：

```go
mux.Handle("/", resolveStatic(cfg))
```

这条兜底 pattern **不带方法**，接受任意 method，所以任何未注册的 method+path 都会退到它 ——
真实二进制回 **404**、未内嵌前端的测试构建回 **503**，自动 405 根本不会发生。

## 根因

`mux.Handle("/", …)` 是「路径最长匹配 + 方法不匹配时继续找更短的 pattern」的结果：
`GET /v1/…/{name}` 匹配不上 `PUT` 时，mux 会落到 `/`。Go 只在**没有**其他 pattern 接住时才自动 405。

## 取证（实测，不进仓库）

最小 mux 探针（go1.25.0，pattern 与生产同形）：

| 场景 | `PUT …/capabilities/{name}` |
|---|---|
| 无 `/` 兜底（测试夹具的裸 mux） | `405 Method Not Allowed` |
| 有 `/` 兜底（生产 mux） | `404 static-fallback` |

## 已修

注释改写为「现在是什么 + 为什么」，分三层：本 register 只挂带方法的 pattern / 裸 mux 与生产 mux
的两种答案 / 为什么只为完全只读的 `edges` 补显式 405。

**零行为改动**：`git diff -U0 | grep '^[+-]' | grep -v '^[+-]\s*//'` 计非注释变更行 = **0**。

## 验证

```
$ go test ./internal/server/ -run TestOpenAPIParity -count=1
ok  github.com/zzjcool/anotify/internal/server  1.873s
$ gofmt -l internal/server/resource_capability.go   （无输出）
```

外加独立核验：session JSONL 显示 worker 实际跑了上述门禁（不是自述），且未触碰
`mux.go` / `*_test.go` / 断言。

## 不做（防止重开）

- 不给能力写端点补逐路径 405 兜底：v4 能力没有 `PUT`（内容变化 = 新版本走 `POST`），
  非要 405 需先改 `api-conventions` 的方法分派语义 + parity 裸挂载表，属另开卡。
- 不动 `edges` 那两条 `methodNotAllowedAllowGet`（那是刻意的只读例外）。

## 遗留给下一张卡

`internal/server/resource_playbook.go:44-46` 有**同一处错误结论**（见 002）。
