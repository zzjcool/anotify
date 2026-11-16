---
id: 003
kind: bug
area: [task]
size: M
severity: P2
state: open
needs_recon: true
contract: []
depends_on: []
route: [scout, worker]
gates: ["go test ./internal/store/... ./internal/lease/... -count=1", "make contract"]
opened: 2026-11-16
---

# 003 · 能力 tombstone 永远不会被物理回收：`PurgeDeletedCapabilities` 没有生产调用点

## 现象

`internal/store/retention.go:317` 定义了：

```go
func (d *DB) PurgeDeletedCapabilities(ctx context.Context, limit int) (int64, error)
```

但全仓**只有测试在调它**：

```
internal/store/v4test/reference_force_test.go:182,191,200,234
internal/store/v4test/reference_isolation_test.go:55,69,195
```

生产路径一条都没有。`lease.Sweeper` 的巡检（`internal/lease/lease.go:73`）只调
`db.RunRetention(ctx, now)`，而 `RunRetention`（`internal/store/retention.go:73+`）只删
过期 tasks 和 events，**从不调用** `PurgeDeletedCapabilities`。

## 后果

`DELETE …/capabilities/{name}` 是**软删**（所有活行写 `deleted_at`、rv+1），
tombstone 会永久留在 `capability_versions` 里：

- 删除次数越多，表越大，且没有任何回收路径；
- 任何按 name 扫版本行的查询（含未来要加的「含 tombstone 的历史」投影）都会随删除历史线性变慢。

## 需要先侦察确认（`needs_recon: true`）

1. **保留期语义**：tombstone 该保留多久？`PurgeDeletedCapabilities` 的既有约束是
   「只删没有任何 task / attempt / manifest / deployment / receipt 引用的不可变 tombstone」——
   这个判据够不够，要不要加时间窗？
2. **谁负责跑**：挂在 `lease.Sweeper` 的既有一轮巡检里，还是独立循环？挂进去要保证
   `App.WaitBackground` 能收尾（`internal/server/AGENTS.md` 陷阱第 4 条）。
3. **保留域读模型**是否依赖 tombstone（`/v1/namespaces/{ns}/deployments` 等）；
   若有依赖，回收后这些投影必须仍自洽（不能回 500）。
4. 是否需要 `/history` 类的含退役行投影 —— 若要，回收窗口就变成**对外承诺**，
   必须写进 `api-conventions`，不能只写在代码注释里。

## 验收（侦察之后才定稿）

- 生产路径真的会调用（有测试证明「过期 tombstone 在 N 次巡检后被删」）。
- 被引用的 tombstone **不**被删（保护既有引用完整性）。
- 删完后 `make contract` 绿；相关保留域读模型不 500。
- 新增 e2e 或单测覆盖，不接受「只加了调用点」。

## 不做

- 不为了腾空间改成硬删（`deleted_at` 是对外语义的一部分：`/versions` 在删除后恒 404 靠它）。
