---
id: 002
kind: docs
area: [server]
size: S
severity: P3
state: open
needs_recon: false
contract: []
depends_on: [001]
route: [worker]
gates: ["go test ./internal/server/ -run TestOpenAPIParity -count=1", "gofmt -l internal/server/resource_playbook.go"]
opened: 2026-11-16
---

# 002 · `resource_playbook.go` 有和 001 一样的错误注释

## 现象

`internal/server/resource_playbook.go:44-46`：

```go
// 方法不匹配（例如 `PATCH …/playbooks/{slug}`）由 Go 1.22 ServeMux 自己回 405，
// 这里不重复实现方法分派。
```

与 001 完全同一处错误结论：生产 mux 有 `mux.Handle("/", resolveStatic(cfg))` 兜底，方法不匹配
落到静态兜底 → 404/503，不是 405。

## 已确认事实

- 001 已把 `resource_capability.go` 的同一句改对（含取证结论：裸 mux 405 / 生产 mux 404）。
- 本卡位置由 001 的 worker 在报告里点名，**未改动**（超出它的授权范围）。

## 修法

照 001 改后的措辞改写，不重复整段：这句只需要说清「本函数不兜底方法分派；生产 mux 落到静态兜底」，
再指回 001 对应的 `resource_capability.go` 注释（同仓同结论的地方只解释一次）。

## 验收

- 只改注释：`git diff -U0` 里非注释变更行 = 0。
- `go test ./internal/server/ -run TestOpenAPIParity -count=1` 绿。
- 全仓不再有「由 Go 1.22 ServeMux 自己回 405」的错误断言：
  `grep -rn 'ServeMux 自己回 405' internal/` → 0 命中。

## 不做

- 不改行为、不补 405 pattern（同 001「不做」）。
