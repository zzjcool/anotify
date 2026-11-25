# store · 模块约定

as_of: 2026-11-16 · area: store · 门禁: `go test ./internal/store/... -count=1`

## 1. 边界

- **负责**：SQLite 访问（事实的唯一落地）、`schema.sql` + 迁移、ID 铸造、keyset 分页、统计/保留期查询。
- **不负责**：HTTP/WS/装配（`internal/server`、`internal/ws`）、执行（`cmd/anode`）。
- **依赖白名单**：只允许 import 叶子纯逻辑包 —— `protocol` / `artifact` / `sourcespec` / `playbook` /
  `route` / `source`。**不得** import `server` / `ws` / `lease` / `auth` / `api`
  （分层倒挂 + import 循环）。

## 2. 改之前必读

1. `internal/store/schema.sql` 顶部 20 行注释（PRAGMA、迁移块格式、A2 纪律）
2. `internal/store/migrate.go` 的三个常量注释：`baselineVersion` / `headVersion` / `layoutFormatV4`
3. `internal/store/txexec.go` 文件头（为什么事务内的读必须走 `tx`）

## 3. 硬红线

1. **`schema.sql` 全文（含注释）不得出现 `argv` / `shell` / `script`** —— `make contract` 的 A2 机检，出现即红。
2. **schema 变更 = 追加一个迁移块，不改 baseline**：`schema.sql` 是分块迁移，`headVersion`
   （`migrate.go:37`）必须等于块数——追加块忘了 +1，新块**永远不会被应用**。
   参照结构按「已记账到的版本」重放（`schemaLayoutAt`，`migrate.go:516`）：还没跑增量块的库
   按旧一块校验，跑完再按 head 复核，所以**已有本地库会被就地迁移**，不需要删库重建。
   - 加/改列 = `-- +migration N <word>` 块（`ALTER TABLE … ADD COLUMN` + 回填 + 索引）+
     `headVersion` + 本文件 §3.7 的三处。
   - 原地改 baseline 是**禁止**的：它让「已记账到 1」的库突然缺列/多列 → `ErrSchemaInvalid`
     具名拒绝，而 CI/e2e 全用 fresh 库，**抓不到这个回归**。
   - v4 发布（有 tag）后规则不变：baseline 冻结，只追加块。
3. 新增/修改字段必须加「往返一致性」单测：存进去什么，读出来就是什么。
4. **事务内的读必须走 `tx`**（`queryExec`），不许用 `d.QueryContext`：否则读到事务外的快照，
   校验与提交之间会出现「检查时成立、提交后不成立」的窗口（`txexec.go` 头部有完整原因）。
5. 空列表返回 `[]` 不返 `null`；资源 `status` 无事实时返 `{}` 不返 `null`。
6. 写路径用 `BEGIN IMMEDIATE`（DSN `_txlock=immediate` + `busy_timeout=5000`），别靠重试兜锁。
   连接池是 8（`store.go:38`）——**不要**假设单连接串行化。
7. **改表结构至少改三处**：`schema.sql`（列 + 注释）+ 本模块单测（往返 + 结构）+ 结构验收
   （`schema_baseline_test.go` / `v4test/schema_baseline_test.go` 的必需列清单）。
   `migrate.go` 只在引入新迁移块或改引擎时动。

## 4. 门禁

```bash
go test ./internal/store/... -count=1
make contract                                                        # 改了 schema.sql 必跑
go test ./internal/store/ -run 'Baseline|Migrate|Schema' -count=1    # 改了迁移块必跑
```

## 5. 陷阱（带锚点）

- **布局身份 ≠ 记账**：`schema_version=1` 不代表 v4 布局（v3 的库同样记着 1）；布局由
  `PRAGMA user_version=4` 单独标记（`migrate.go` 的 `layoutFormatV4`）。**绝不补 marker 把老库当新库用**。
- 老库会**明确报错**（`ErrUnsupportedFormat`），`Open` 不会自动删用户数据 ——
  `TestLegacyDatabaseRejected` 守着这条（连「数据还在」都断言了）。
- 迁移块不幂等 → 重开库直接失败：`TestBaselineReopenIsIdempotent` 逐块比对 `schema_version` 行数。
- SQLite 没有 `ADD COLUMN IF NOT EXISTS`：一个块只跑一次靠 `schema_version` 记账，重开幂等由
  `TestBaselineReopenIsIdempotent` / `v4test.TestMigrationIdempotent` 守着。
- 改 baseline 的表/列/索引清单要同步 `schema_baseline_test.go`（`foreign_key_check` + `integrity_check` + 重开比对）。
- 池放到 8 之后，历史注释里「单连接使 A 与 B 串行化」这类断言**已失效**（真实事故：并发首注册双 admin，
  见 `docs/history/issue-n1-body.md`）。看到这类注释不要信，按事务重新论证。

## 6. 上下游契约

- `schema.sql` 是**跨 area 共享文件**：任何 area 加表/列都由 contract owner 合并，不由本模块自决。
- 语义变化（不只是加列）→ 同步 `api/*.yaml` 与 `docs/contracts/`（字段级真源是 `schema.sql` + `api/`；设计文档已移除）。
- 向 `server` 交付的是**事实**不是视图：store 不做展示裁剪（分页/信封在 server 侧）。
