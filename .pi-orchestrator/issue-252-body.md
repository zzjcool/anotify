## 背景

#240（连接池 1→8）+ #238（`idx_edges_lookup` 收敛）+ **迁移链 squash**（`000a633`）之后，原文有两处已失效，这里按当前代码重写。**行号与列名以本 issue 更新时为准。**

原第 1 点的字面调用已全部消失，原第 2 点的验证 SQL 用了已删除的列（`from_key` → 现为 `from_ref`）。

## 1. `idx_edges_lookup` 是严格左前缀冗余（已实测，待 DROP）

```sql
-- internal/store/schema.sql:305
PRIMARY KEY (namespace_id, from_is_source, from_ref, from_port, to_ref)
-- internal/store/schema.sql:310-311
CREATE INDEX IF NOT EXISTS idx_edges_lookup
   ON edges(namespace_id, from_is_source, from_ref, from_port);
```

4 列 = 主键前 4 列，**严格左前缀**，数学上即可由 `sqlite_autoindex_edges_1` 完全替代。

实测（20k 行 + ANALYZE，逐个查询形状对比 DROP 前后）：派发 4 列等值**本来就走 autoindex**；3 列前缀（`ns,is_source,ref`）与 4 列 DELETE 在有索引时用 `idx_edges_lookup`，DROP 后仍由 autoindex 以**同一 search shape** 承担，无全表扫、无 TEMP B-TREE。

⇒ 它不是「不一定被选中」，而是**在任何形状下都没提供 PK 给不了的东西**，只多付一份写维护成本。

修法：追加 `-- +migration 6 drop_edges_lookup`（`DROP INDEX IF EXISTS`），同步 `headVersion`、`schema_baseline_test.go` 的索引表项、`CONTRACT.md` 的 DDL。
**地雷**：`internal/store/seq_in_trace_test.go:415` 把 `headVersion` 当「migration 5」用，`headVersion` 一变就红（`no such column: seq_in_trace`），必须改成字面量 5。

## 2. 过时「单连接」注释（9 处）

`SetMaxOpenConns(1)` 在 issue 原文列出的 6 个文件里已全部消失（其中 `migrate14_test.go`、`migrate_test.go:222` 指向的代码已被 squash 删除）。剩下的是**注释仍在说谎**（保留的理由应是「函数已在事务里，必须用同一事务句柄」，不是「只有一根连接」）：

`internal/store/task.go:866,1083-1084,1187-1188`、`internal/store/node.go:1238,1246-1247,1319-1320`、`internal/store/authz_test.go:54-55`、`internal/lease/sweeper.go:58`、`internal/lease/sweeper_test.go:274`、`internal/server/admin_handlers.go:294`（API Key 缓存 60s，不是 10s）。

其中 `node.go:1246` 不是措辞问题：`LeaseInstance` 的查空槽与 CAS 占位都走池、**没有事务包裹**，安全性来自 CAS 重试，不来自池大小。

## 3. 从本 issue 拆出去的真问题

`internal/auth/webauthn.go` 曾声称「单连接使 `UserCount` 与 `RegisterUser` 串行化」——池放开到 8 后该不变量已假，并发首注册可双 admin。**已单独开条（安全级），不在此 issue 内修。**

## 验收

- `go test ./internal/store/... ./internal/ws/... ./internal/lease/...` 全绿
- `make contract` 通过
- 删索引后：`EXPLAIN QUERY PLAN` 对派发 4 列查询仍用 `sqlite_autoindex_edges_1`；`TestBaselineIndexes` 的显式索引集合与 schema 严格相等
