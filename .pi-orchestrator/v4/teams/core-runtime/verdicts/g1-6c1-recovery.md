DONE G1-6C1

## 产出 commit
- **`459069f`** `fix(store): 区分 v4 布局格式与 baseline 记账`（branch `pi-subagent/v4-core-runtime-recovery-144eee52`，working tree clean；**未 push / 未 merge / 未开 MR**，按卡执行）

## 实际变更文件（仅卡内路径）
| 文件 | 变更 |
|---|---|
| `internal/store/migrate.go` | 格式探测 + 具名拒绝 + marker 同事务（+564/−62） |
| `internal/store/schema.sql` | 仅 8 行**注释**：布局格式 vs 记账的区别、baseline 必须可整体回滚（无字段改动） |
| `internal/store/v4test/format_test.go` | **新增** 721 行：13 个真实 SQLite 反例/正例测试 |
| `internal/store/v4test/migrate_test.go` | 两条既有拒绝用例加 `errors.Is(ErrUnsupportedFormat)` 断言（只加强，不弱化） |
| `internal/store/v4test/schema_baseline_test.go` | +122 行：布局标记独立性 + 24 表/10 退役列的防漂移拒绝门禁 |

`internal/store/store.go` **未改**：卡里给的是「仅 Open 接线」的许可；标记写入与校验都在 migrate 路径内完成，Open 无需改动（`migrate→stats` 顺序原样）。

## 实现要点（对应父裁决）
1. **布局身份与记账解耦**：`schema_version=1` 只是「baseline 块跑过」；`PRAGMA user_version=4`（`layoutFormatV4`）才是「表结构是 v4」。
2. **只有真正空的新库**（无用户表、无 `schema_version`、marker=0）才建库，且 **marker + `schema_version` DDL + baseline DDL + 记账行在同一事务**（marker 先写，失败随事务回滚）；`headVersion` 仍 = 1，无历史迁移。
3. **已有 baseline1 但无 marker → `ErrUnsupportedFormat`**，不补 marker、不跑任何建表；versionless legacy / 记账 >head / 未知 marker（1/3/5/99）同样具名拒绝。
4. **marker=4 → 只校验不修复**：缺表、缺列、残留 v3 列、缺关键唯一约束、缺记账行、记账版本矛盾 → `ErrSchemaInvalid`，在**任何** `CREATE IF NOT EXISTS` 之前；显式不做自愈、不删数据。
5. 新增具名哨兵：`store.ErrUnsupportedFormat`、`store.ErrSchemaInvalid`（`fmt.Errorf("%w：…")` 包装，中文细则保留）。

## 新增测试（全部真实 SQLite 文件，0 skip）
`format_test.go`（13 顶层 + 子用例）
1. `TestFormatNewDatabaseWritesLayoutMarker`（新库 marker=4 + 记账=1 + reopen + 字段往返）
2. `TestFormatBaselineLedgerWithoutLayoutMarkerRejected`（**核心反例**：v3 + `schema_version=1` + `nodes.namespace_id` / `capability_versions.outputs`）
3. `TestFormatVersionlessLegacyRejected`（无账本旧表）
4. `TestFormatLedgerAheadOfHeadRejected`（记账 1,2,11）
5. `TestFormatUnknownLayoutMarkerRejected`（子用例 marker1/3/5/99）
6. `TestFormatMarkerWithoutLedgerRejected`（marker4 半写）
7. `TestFormatMarkerContradictoryLedgerRejected`（baseline_row_missing / only_higher_version / ledger_ahead_of_head）
8. `TestFormatMarkerMissingRequiredTableRejected`（缺 `emits`）
9. `TestFormatMarkerMissingRequiredColumnRejected`（`nodes.facts` 缺失）
10. `TestFormatMarkerRetiredColumnRejected`（8 子用例：nodes/node_instances.namespace_id、capability_versions.outputs/content_hash、tasks.status/attempts、edges.from_port/priority）
11. `TestFormatMarkerMissingUniqueIndexRejected`（缺 `emits(task_id,emit_id)` 唯一索引）
12. `TestFormatBaselineFailureRollsBack`（baseline DDL 中途失败 → 整库回滚、marker=0）
13. `TestFormatLedgerWriteFailureRollsBackBaseline`（**事务最后一步记账失败** → DDL+marker 全回滚）

`schema_baseline_test.go`（3 顶层）
14. `TestSchemaBaselineLayoutMarkerIsSeparateFromLedger`
15. `TestSchemaBaselineEveryTableRequiredByFormat`（24 子用例，逐表 drop→拒绝，防清单漂移）
16. `TestSchemaBaselineRetiredColumnsRejectedByFormat`（10 子用例）

每条拒绝用例都用 `snapshotDatabase`（marker + `sqlite_master` 全量 DDL + 每表行数）断言**拒绝前后一字不差**，并额外断言未补 marker、未自愈（`emits` 仍缺、`facts` 仍缺、唯一索引仍缺）、旧数据仍在。拒绝原因实测（verbose 日志原文）：
- `v3·schema_version=1 但无布局标记: store: 数据目录不是 v4 新建的库（不支持旧库升级）：数据目录已有 schema_version 记账（最高版本 1，共 1 行）却没有 v4 布局标记（PRAGMA user_version=0）…`
- `baseline 中途失败: migration 1 (baseline): exec sql: SQL logic error: there is already a table named idx_sources_due (1)`
- `记账写入失败: migration 1 (baseline): record version: SQL logic error: cannot modify schema_version because it is a view (1)`

## 机械验证（launch 固定 4 条，HEAD=459069f 上最终运行）
| # | 命令 | exit | 结果 |
|---|---|---|---|
| 1 | `test -z "$(gofmt -l internal/store)"` | **0** | 无输出 |
| 2 | `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store/` | **0** | 生产 store 编译通过 |
| 3 | `env … go vet ./internal/store/v4test/` | **0** | 无诊断 |
| 4 | `env … go test ./internal/store/v4test/ -count=1` | **0** | `ok github.com/zzjcool/anotify/internal/store/v4test 7.705s` |

全量外部 v4 套件（`-count=1 -v`）：**177 top-level PASS / 67 subtest PASS / 0 SKIP / 0 FAIL**（161 既有 + 16 新增，既有 161 全部继续通过）。

完整日志：
- `/tmp/anotify-core-recovery-c1-verify-final.log`（4 条固定命令，post-commit）
- `/tmp/anotify-core-recovery-c1-verify.log`（同 4 条，pre-commit）
- `/tmp/anotify-core-recovery-c1-v4test-verbose.log`（177 PASS 清单 + 拒绝原因原文）
- `/tmp/anotify-core-recovery-c1-v4test-full.log`（首次全量）
- `/tmp/anotify-core-recovery-c1-format.log`（13 新 format 测试 verbose）
- `/tmp/anotify-core-recovery-c1-schema.log`（schema/migration 定向 verbose）

**async_run 说明**：本 session 工具集里没有 `async_run`（`searchTools("async_run")` 无该工具）；最长的全量套件实测 8s，全部命令都是有界 `timeout` 包裹 + 完整 tee 日志，未起任何 Pi 进程、未改 harness。此为工具环境事实，不构成本卡阻塞。

## 未完 / 明确未做（不宣称 G1 或 T1 完成）
- **`internal/store` 包内旧测试仍不编译**（`migrate_test.go`/`capability_links_test.go`/`playbook_test.go`/`reconcile_test.go`/`session_lease_test.go` 等）：本卡按卡**未删、未 skip、未运行**，归 G1-6 旧测试闭包卡。
- 全 Go `go test ./...`、`make contract`、`make e2e-quick` **未跑**（不在本卡 verify 数组内；原 FAIL 证据仍权威）。
- G1-6C2（原子 resourceVersion CAS）、G1-6C3（typed A1 manifest）、G2/G3/G4 未动。
- **刻意的行为变更**：C1 之前（21253ed 代码）建的 dev 库没有 marker → 现在一律 `ErrUnsupportedFormat` 拒绝（不补 marker、不删数据）。按契约「v4 开发期不保留旧数据库迁移兼容层」执行，开发库需重建。
- 空库判据 = 无任何用户表；只含视图/触发器/索引的文件不可能由受支持路径产出，遇到时由建库事务整体回滚（有两个回滚用例覆盖），已在代码注释中写明。

```json
{"ok": true, "reason": "G1-6C1 committed 459069f (5 files: migrate.go/schema.sql/v4test×3); PRAGMA user_version=4 layout marker written atomically with baseline DDL+ledger, baseline1-without-marker/legacy/>1/unknown-marker rejected as ErrUnsupportedFormat, marker4 partial/missing table/column/unique/retired-column rejected as ErrSchemaInvalid without repair, all refusals preserve data+DDL+marker; 4 fixed verify commands exit 0 (gofmt/build/vet/test), external v4test 177 PASS 0 SKIP; no push/MR per card"}
```
