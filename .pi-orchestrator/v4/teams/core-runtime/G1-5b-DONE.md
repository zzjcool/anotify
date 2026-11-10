# G1-5b DONE · Store 目录包 / 单事务安装 / dryRun / 回执实例历史

- 任务卡：`.pi-legion/tasks/G1-5b.md`
- 分支：`pi-subagent/v4-core-runtime-775b3657`（已 push 到 origin，同名分支）
- 提交：`8497d7f feat(store): v4 原子包安装与回执实例历史`（仅本卡 paths）
- 前置：纯包 c654058（未改）；G1-4R 8b1b95c；protocol 1b420e6 只读
- 未开 PR/MR：origin 只有 `main`（v4 集成分支未 push），按 CONTRACT §6 由父会话决定最终合入；
  分支已 push，父可本地/远程按需开 MR。

## 改动文件（7）

- `internal/store/playbook.go`（新增）— PlaybookRow CRUD / render / 单事务 install / DELETE / 实例清单 / 单回执
- `internal/store/playbook_tx.go`（新增）— 事务内 `playbook.Existing` 适配器（error 账本）、严格 INSERT、回执落库、kept/values 反查
- `internal/store/source.go`（仅抽取）— 从 CreateSource 抽出 `insertSourceOn`（INSERT + 入口边投影），直写语义不变
- `internal/store/schema.sql`（必需 storage 列）— `playbooks.display_name`（v4 baseline 未发布，属本轮批准的全量重建）
- `internal/store/v4test/playbook_test.go`（新增）
- `internal/store/v4test/playbook_history_test.go`（新增）
- `internal/store/v4test/schema_baseline_test.go`（列断言 + 回执/installed_from 无 live-parent FK 断言）

未改：`capability.go`（已有 `insertCapabilityOn`；本卡安装路径为「只加不改」另写严格 INSERT，不再走折叠复用）、
`retention.go`（G1-4 已实现回执引用保护，本卡只验证集成）、`internal/playbook/**`（只读纯包）、旧 in-package 测试。

## 实际导出签名（G3 消费面）

```go
// internal/store/playbook.go
var ErrSlugTaken, ErrPlaybookSlugMismatch error
const DeletePlaybookNote = "预案只是清单，不是所有权；要拆链请直接删能力 / 删源"

type PlaybookRow struct {
    ID, NamespaceID, Slug, Name, DisplayName, Description string
    Version int
    Files map[string]string   // 原始未渲染包体（含 playbook.yaml）
    Source string             // OCI provenance
    ResourceVersion, CreatedAt, UpdatedAt int64
}
type PlaybookKept struct{ Capabilities, Sources []string }
type PlaybookDeleteResult struct {
    Deleted bool
    Kept PlaybookKept
    Values []map[string]any   // 打码后的有效 values，按 valuesDigest 去重
    Note string
}
type PlaybookInstallGroup struct {
    ValuesDigest string
    Values map[string]any     // 打码后
    FirstAt, LastAt string    // RFC3339 UTC
    Attempts int
    LastInstallID string
    Capabilities, Sources []string
}
type PlaybookInstallList struct{ Total int; Installs []PlaybookInstallGroup }

func (d *DB) CreatePlaybook(ctx context.Context, namespaceID, slug string, files map[string]string, source string) (*PlaybookRow, error)
func (d *DB) GetPlaybook(ctx context.Context, namespaceID, slug string) (*PlaybookRow, error)
func (d *DB) ListPlaybooks(ctx context.Context, namespaceID string) ([]*PlaybookRow, error)
func (d *DB) ReplacePlaybook(ctx context.Context, namespaceID, slug string, expectedRV int64, files map[string]string, source string) (*PlaybookRow, error)
func (d *DB) RenderPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any) (*playbook.RenderResult, error)
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any, dryRun bool) (*playbook.Receipt, error)
func (d *DB) DeletePlaybook(ctx context.Context, namespaceID, slug string, expectedRV int64) (*PlaybookDeleteResult, error)
func (d *DB) ListPlaybookInstalls(ctx context.Context, namespaceID, currentPlaybookID string) (*PlaybookInstallList, error)
func (d *DB) GetPlaybookInstall(ctx context.Context, namespaceID, slug, installID string) (*playbook.Receipt, error)
```

错误映射：`ErrSlugTaken`（409 slug_taken）、`ErrPlaybookSlugMismatch`（路径 slug ≠ 包内 metadata.name）、
`ErrNotFound`（404）、`ErrResourceVersionRequired`（400）、`ErrResourceVersionConflict`（409）、
`playbook.Code(err)` 透传纯包领域码（values_* / version_content_immutable / playbook_conflict / cycle …）。

## 关键实现边界

- 安装是 `BEGIN IMMEDIATE` 单事务：事务内读包体 → `installSnapshot`（6 个 query 全走 tx + error 账本）→
  `playbook.Plan` → **snapshot.err 优先于 conflict 裁决**（读错不当空库）→ 只写 `Action=create` 条目 →
  新 `ins_` 回执（masked values + 真 valuesDigest + ns/原 slug/原包 id/provenance 快照）→ COMMIT；
  dryRun 走完同一条写路径后 Rollback（0 持久写）。
- `reuse` 条目一行不写：绝不调用 CreateCapability / UpdateNext / SetCapabilityDisplayName / UpdateSource；
  安装侧新增严格 INSERT（唯一冲突即错误），显式低版本新建（已有 max=3 再装 v1）**不重建**该名字的边投影。
- `Existing.CapabilityByFoldKey` 用 `ORDER BY version DESC LIMIT 1`：同 foldKey 多行（@1/@3）时确定性复用最高版本，
  skipped 版本号与 next 比较对象不会漂移。
- token：省略保留已有（`inheritServerGenerated`）、显式参与比较、差异只报字段名；新 token 只对新源生成（dryRun 随回滚消失）。
- 边：能力边随「成为 latest」的新行重建；复用/unapplied 不动已有边；源入口边只对新源写。
- 回执/历史：每次普通 install 新回执；首建对象 `installed_from` 只记首次；删包只删清单，
  `kept` 由 installed_from 反查、`values` 由回执历史按 digest 去重；
  `ListPlaybookInstalls` 按 `(ns, 原 playbook id, valuesDigest)` 折叠、lastAt 降序、≤200 组、Total=全组数；
  `GetPlaybookInstall` 只按 `(ns, 历史 slug, installId)` 读，不要求当前清单存在。
- GC：自动 `RunRetention` 不删回执（G1-4 已实现）；`DeleteExpiredPlaybookInstalls` 的
  installed_from / trace 引用检查与 DELETE 同事务，limit≤0 no-op。

## 具名测试（v4test，真实 SQLite，38 个 RUN = 22 顶层 + 16 子测试）

安装/写路径（playbook_test.go）：
- `TestPlaybookCreateGetReplace`（Parse/同 slug/原始 files 往返/slug 一致/PUT CAS/只换模板/回执快照与 provenance）
- `TestRenderPlaybookReadOnlyAndConflictFree`（0 写、不读冲突库）
- `TestInstallMatrixDBRows`，12 格各一条真实 DB 断言：
  `row01_capability_created`、`row02_capability_fold_skipped`、`row03_capability_new_version_upgraded`、
  `row04_explicit_version_created`、`row05_explicit_version_same_content_skipped`、
  `row06_explicit_version_conflict_zero_write`、`row07_next_change_unapplied`、
  `row08_source_created_with_token`、`row09_source_equivalent_reused`、
  `row10_source_config_conflict_zero_write`、`row11_source_target_pending`、`row12_source_targets_unapplied`
- `TestInstallConflictRollsBackWholePackage`（多资源：已安排 create + 冲突 → 全表 0 写）
- `TestInstallConcurrentSamePackage`（并发 2 次：1 份对象 + 2 份回执）
- `TestInstallDryRunWritesNothing`（0 写；冲突错误与真实安装同码）
- `TestInstallReuseNeverUpdatesExistingRow`（skipped 不偷改 displayName/rv）
- `TestInstallSnapshotReadErrorFails`（corrupt source targets / corrupt capability spec 都失败且 0 写）
- `TestInstallValuesWriteOnlyMaskedDigestOnRaw`（writeOnly 打码、digest 打码前、库无明文、created/skipped 空数组形状）
- `TestInstallTokenOmittedKeepsExisting`（省略保留 / 显式相同复用 / 显式不同冲突且错误不泄露 token）
- `TestInstallMissingPlaybookAndNamespace`
- `TestInstallFoldKeyDeterministicHighestVersion`（@1/@3 同正文 → 复用 @3、unapplied 锚 @3）
- `TestInstallExplicitLowerVersionKeepsBareRoute`（已有 v3 装 v1：裸名/边仍属 v3）
- `TestInstallWriteOnlyCompositeMasked`（整数组/整对象 writeOnly → "***"，digest 真实）
- `TestInstallCombinedConflictsKeepDetails`（capability+source 两条 conflict 明细完整，0 写）

历史/GC（playbook_history_test.go）：
- `TestPlaybookInstallsFoldAttemptsAndUnion`（折叠 attempts/lastInstallId/installed_from 并集）
- `TestPlaybookInstallsOrderAndLimit`（lastAt 降序、≤200、Total=205）
- `TestPlaybookInstallsEmpty`（`installs: []`、未知回执 404）
- `TestDeletePlaybookKeepsReceiptsAndResources`（kept/values 汇总脱敏、对象/回执保留、单回执仍读）
- `TestPlaybookRecreateSameSlugDoesNotInheritHistory`（新 id 不继承旧 group、旧回执仍可读）
- `TestPlaybookReceiptGCReferenceSafety`（自动 sweeper 不清回执；limit=0 no-op；引用保护；未引用过期才删）
- `TestPlaybookGCConcurrentWithInstall`（并发 GC × install 后无悬空 installed_from）

schema：`TestSchemaBaselineCapabilityColumns`（playbooks 新列）、`TestSchemaBaselinePlaybookNoLiveParentFK`（回执无 live-parent FK）

## 验证（真实输出，提交 8497d7f 后）

```
$ test -z "$(gofmt -l internal/store)"
$ go build ./internal/store/
$ go vet ./internal/store/v4test/
$ go test ./internal/store/v4test/ ./internal/playbook/... ./internal/sourcespec/... -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	3.018s
ok  	github.com/zzjcool/anotify/internal/playbook	0.062s
ok  	github.com/zzjcool/anotify/internal/sourcespec	0.004s
verify-exit:0
```

附加：`go test -race ./internal/store/v4test/ -run 'TestInstallConcurrentSamePackage|TestPlaybookGCConcurrentWithInstall'` → ok。

## 未决 / 已知边界

1. **未开 MR**：origin 仅有 `main`；v4 集成分支只在本地。分支已 push，最终合入由父会话裁决。
2. `retention.go` 未改：回执引用保护是 G1-4 已交付实现，本卡用真实 install 产物验证集成；
   若父要求把该 primitive 移出 retention.go，属另一张卡。
3. G1-6 债务不变：旧 in-package `internal/store/*_test.go` 仍不编译（未 skip/删/弱化）；
   本轮 verify 只跑 v4test + 纯包，符合任务卡。
4. 全仓 `go build ./...` / `go test ./...` / contract / e2e 未跑（G4 最终门禁）；不宣称 G1/G2 全量完成。
5. G3 注意：`GetPlaybookInstall` 按**历史** `(namespace_id, playbook_slug, id)` 读取（删包/重建后旧回执仍可读）；
   若产品要求「新包路径下不可见旧回执」，handler 需自行核对 installId 属于当前 playbook id。
6. `ListPlaybookInstalls.Total` = 折叠组总数（可能 > 返回的 200 条）；`Installs` 按 lastAt 降序截断。
