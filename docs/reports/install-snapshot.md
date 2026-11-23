# 安装时快照：回执记 `{name,version,digest}`，导出支持 `as=installed`（#551）

<!-- snapshot-doc: 2026-11-24 #551 的冻结契约与逐条验收；数字为交付当次实测（复算命令见 §5） -->

> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

as_of: 2026-11-24 · area: `store` / `playbook` / `server` / `api` / `web` / `e2e` · 迁移块 `7 playbook_requires` → `8 install_pinned`
起点 `main@996212a1`

## 0. 结论（冻结决策）

| # | 决策 | 为什么 |
| --- | --- | --- |
| D1 | 回执 `created.pinned` 是 **`[{kind,name,kindOf?,version?,digest?}]`**：能力 `{kind:"capability",name,version,digest}`，源 `{kind:"source",name,kindOf}`（源**没有** `version`/`digest`） | issue 给的形状里源那行带了 version，但「不做」段写明「源的历史版本（源没有版本概念）」；源能记的只有它是哪种源 → `kindOf` |
| D2 | `pinned` **不进 `required`**、读路径 `nil → []`（与 #534 `dependencies` 同一惯例）；`skipped.pinned` 恒 `[]` | 老回执必须原样可读，且不得改写回执原文（审计快照） |
| D3 | 迁移 8 = 新表 **`playbook_install_pinned`**（install 级索引），与回执 JSON 在**同一事务**、同一份切片写入；`headVersion` 7 → 8 | issue 明写「迁移只加列/表」；SQL 侧对账需要它（issue 动机 ③「当初依赖哪一版」），形状照抄 #534 在迁移 7 的做法（回执 JSON + 索引表双写） |
| D4 | `as` ∈ {空, `current`, `installed`}；其它值 → **400 `value_invalid`**；空与 `current` **逐字节等价**（含已卸载 → 404） | 「默认行为逐字不变」 |
| D5 | `as=installed`：不判 `uninstalled`；按选中的回执取快照，按 `(kind,name)` 合并、**后一条回执覆盖前一条**；输出能力按名字升序在前、源按名字升序在后 | 同一 release 多组 values / 多次安装时，「实例装成什么样」= 每个对象最后一次被装的版本 |
| D6 | 能力按 `(name,version)` 取**含软删行**（卸载只 `SET deleted_at`，正文仍在）；源按名字取**当前**（卸载会物理删源行）→ 源缺失记为「快照缺失」 | 「已卸载实例 as=installed 仍可导出」；源无版本，只能取当前 |
| D7 | 缺快照（选中回执没有任何 pinned 行）→ **409 `no_snapshot`**，`details.causes[0].field = "release"` | issue 验收 3 |
| D8 | 头注释：`as=current` 恒 3 行（逐字不变）；`as=installed` 恒 4 行 + 有缺失时第 5 行 | 头注释是唯一能写清「这份是快照」的地方（响应是裸 YAML，无可加字段的槽） |
| D9 | UI：YAML 抽屉里加 `.tabs`/`.tab` 二选一（**当前生效 / 安装时**，默认当前）；`no_snapshot` 时把「安装时」置灰并给原因 | issue 第 3 块 |
| D10 | 单对象 `/yaml` 端点**不动**；导出里每个文档仍与它逐字节相同（同一份读投影与脱敏） | #536 的既有契约；`as=installed` 只换「取哪一行」 |

## 1. wire 形状（真源 = `api/resource.yaml` 的该 operation / `PlaybookCreated`）

```json
"created": {
  "capabilities": ["review@3", "gate@1"],
  "sources": ["hook"],
  "pinned": [
    {"kind": "capability", "name": "gate", "version": 1, "digest": "sha256:…"},
    {"kind": "capability", "name": "review", "version": 3, "digest": "sha256:…"},
    {"kind": "source", "name": "hook", "kindOf": "webhook"}
  ]
}
```

- Go：`playbook.PinnedRef{Kind,Name,KindOf,Version,Digest}` + `Created.Pinned []PinnedRef`。
- `digest` 复用能力版本行已有的摘要（`capability_versions.digest`，含 version 的部署身份），不新算。
- 排序：能力按 `name` 升序在前，源按 `name` 升序在后（与 `created.capabilities` 的排序口径一致）。
- 旧字段 `created.capabilities` / `created.sources` **原样保留**（不改成对象）。

## 2. 迁移 8（`-- +migration 8 install_pinned`）

```sql
CREATE TABLE IF NOT EXISTS playbook_install_pinned (
    install_id   TEXT NOT NULL REFERENCES playbook_installs(id) ON DELETE CASCADE,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    ref_kind     TEXT NOT NULL,
    name         TEXT NOT NULL,
    obj_kind     TEXT NOT NULL DEFAULT '',
    version      INTEGER NOT NULL DEFAULT 0,
    digest       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (install_id, ref_kind, name)
);
CREATE INDEX IF NOT EXISTS idx_playbook_pinned_lookup
    ON playbook_install_pinned(namespace_id, ref_kind, name, version);
```

- 列名不用 `kind`：`internal/store/migrate_test.go` 的 I3 不变量只放行 `sources` / `cli_auth_sessions` 的 `kind` 列。
- `ref_kind` ∈ `capability` / `source`；`obj_kind` 只对源有意义（webhook/cron/poll…）；`seq` 保留回执里的顺序。
- **存量回执不写任何行** → 读出来 `pinned: []`（不假装知道）。
- 同一事务：`insertPlaybookInstallOn` 写回执 JSON 的同时写这些行，任一步失败整包回滚（不允许「装了但没记」）。
- 必须同步的版本号断言点：`migrate.go:35`（`headVersion`）、`internal/store/v4test/format_test.go`（2 处 `!= 7` + `ledger_ahead_of_head` 的 `1..8` → `1..9`）、`internal/store/v4test/migrate_test.go`（3 处 `want 7`）、`internal/store/v4test/schema_baseline_test.go`（`v != 7`、`schema_version` 行数、表/列/索引 golden）。

## 3. 导出端点（`GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}/yaml`）

| 请求 | 行为 |
| --- | --- |
| 无 `as` / `as=current` | **逐字节不变**：按名字取当前生效版本；已卸载 → 404 `release_not_found`；头注释 3 行 |
| `as=installed` | 按回执 pinned 渲染能力（该版本文正，即使当前已升级）；源按名字取当前正文；**已卸载仍 200**；头注释固定 4 行（+ 缺失时第 5 行） |
| `as=installed` 且无 pinned | 409 `no_snapshot`，`details.causes[0].field = "release"`，message 说明「该实例安装于未记录快照的版本」 |
| `as=其他` | 400 `value_invalid` |
| `valuesDigest` | 两种 `as` 都生效（先按参数组选回执，再取快照/当前） |

`as=installed` 的头注释（逐字）：

```
# playbook: <slug>@<version>  release: <release>  values-digest: <sha256:…>
# install: <installId>  installed-at: <RFC3339>  objects: <N>
# 版本说明：以下对象为「安装时快照」（安装那一刻记录的版本，不是当前生效版本）
# 源无版本：源按回执记录的名字取当前正文（源没有版本概念）
# 快照缺失: <M> 个对象（安装时记录的对象已不存在，未包含在本导出里）
```

第 5 行只在 M > 0 时出现；`objects: N` 只数真正写进导出的文档。

## 4. store / playbook 层新增 API（冻结签名，供并行实现）

```go
// internal/playbook/playbook.go
type PinnedRef struct {
    Kind    string `json:"kind"`              // "capability" | "source"
    Name    string `json:"name"`
    KindOf  string `json:"kindOf,omitempty"`  // 仅 source：源的 kind
    Version int    `json:"version,omitempty"` // 仅 capability
    Digest  string `json:"digest,omitempty"`  // 仅 capability
}
type Created struct {
    Capabilities []string    `json:"capabilities"`
    Sources      []string    `json:"sources"`
    Pinned       []PinnedRef `json:"pinned"`  // #551；不进 required，nil 归一成 []
}

// internal/store
func (d *DB) GetCapabilityForSnapshot(ctx context.Context, namespaceID, name string, version int) (*CapabilityVersion, error)
func (d *DB) PlaybookInstallPinned(ctx context.Context, namespaceID string, installIDs []string) (map[string][]playbook.PinnedRef, error)
var ErrPlaybookSnapshotMissing = errors.New("playbook install snapshot missing")
```

- `GetCapabilityForSnapshot` 与 `GetCapability` 的唯一差别：**不**过滤 `deleted_at`（卸载后仍能取到正文）。
- `PlaybookInstallPinned` 按 `install_id, seq` 读，返回每个 install 的切片（能力在前、源在后）。
- 错误映射：`ErrPlaybookSnapshotMissing` → 409 `no_snapshot`（`internal/server/api_status.go`）。

## 5. 验收 → 判据（每条都有可复算命令）

| # | 验收（issue） | 判据（测试名） |
| --- | --- | --- |
| 1 | 装 `review@3` → 升级 v4 → `as=installed` 是 v3 正文、`as=current` 是 v4 | `TestPlaybookReleaseYAMLInstalledSnapshot`（server） |
| 2 | 装 → 卸载 → `as=installed` 200 且头注释含 installId；`as=current` 404 | `TestPlaybookReleaseYAMLInstalledAfterUninstall`（server） |
| 3 | 老回执（无 `pinned`）→ 409 `no_snapshot`，field 指到 release | `TestPlaybookReleaseYAMLNoSnapshot`（server） |
| 4 | 老库升级后旧回执 `pinned: []`；新装 `pinned` 非空且 digest 与版本行一致 | `TestMigrationInstallPinnedTable`（store）+ `TestInstallReceiptPinned`（store） |
| 5 | e2e：升级 → 两种视图版本号不同；卸载后 `as=installed` 仍可下载 | `scripts/e2e/suites/playbook_release.mjs` 新段 `--- 13. 安装时快照（#551）---` |
| 6 | 变异 ①/② 必红 | 见 §6 |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | 见 §5 实测表 |

## 6. 变异（真改源码跑出来的红）

| 变异 | 期望红 |
| --- | --- |
| ① 安装事务里不写 `pinned`（跳过 `created.pinned` 与索引表） | 验收 1 / 4 的测试红 |
| ② `as=installed` 复用 `as=current` 的取法（按名字取最新版） | 验收 1 / 2 的测试红 |

## 7. 文件所有权（并行实现的边界）

| 面 | 文件 |
| --- | --- |
| store + 迁移 + 回执 | `internal/store/{schema.sql,migrate.go,playbook.go,playbook_tx.go,capability.go,playbook_export.go,playbook_release_detail.go}` + `internal/store/**/*_test.go` + `internal/playbook/{playbook.go,plan.go}` |
| server + api | `internal/server/{resource_playbook_release_yaml.go,resource_playbook_release_yaml_test.go,api_status.go,openapi_parity_test.go}` + `api/resource.yaml` |
| web | `web-src/pages/release.html`、`web-src/locales/*.yaml`、`web/**`（生成物） |
| e2e | `scripts/e2e/suites/playbook_release.mjs` |

## 8. 不做（本票明确不做）

- 源的历史版本（源没有版本概念）：只记名字与 kind，正文取当前。
- 回执存整份对象正文（体积；只存指针，正文去 `capability_versions` 取）。
- 单对象 `/yaml` 端点的任何行为变化。
- v3 兼容层 / 回执原文改写。
