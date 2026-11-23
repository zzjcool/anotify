# 安装时快照：回执记 `{name,version,digest}`，导出支持 `as=installed`（#551）

<!-- snapshot-doc: 2026-11-24 #551 交付当次的门禁与断言数字（定格记录；复算命令逐条写在 §5） -->

> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

as_of: 2026-11-24 · area: `store` / `playbook` / `server` / `api` / `web` / `e2e` · 迁移块 `7 playbook_requires` → `8 install_pinned`
起点 `main@996212a1`

## 0. 结论

| # | 冻结决策 | 落地事实 |
| --- | --- | --- |
| D1 | 回执 `created.pinned` = `[{kind,name,kindOf?,version?,digest?}]`：能力 `{kind:"capability",name,version,digest}`，源 `{kind:"source",name,kindOf}`（源**没有** `version`/`digest`） | `playbook.PinnedRef`（`internal/playbook/playbook.go`）+ `PlaybookPinnedRef`（`api/resource.yaml`）；issue 例子里源那行写了 `version`，但同一份 issue 的「不做」段写明源没有版本概念 —— 以「不做」为准，源只记 `kindOf` |
| D2 | `pinned` **不进 `required`**、读路径 `nil → []`（与 #534 `dependencies` 同惯例）；`created.pinned` 记新建、`skipped.pinned` 记**复用到的版本**（同一个安装解析出的对象都在） | 4 处反序列化点全部归一，回执原文一字不改（有测试盯原文）。skipped 也记的理由见 §8 第 5 条 |
| D3 | 迁移 8 = 新表 `playbook_install_pinned`，与回执 JSON 同一事务、同一份切片双写；`headVersion` 7 → 8 | `internal/store/schema.sql` 块 8 + `internal/store/playbook_tx.go` 的 `insertPlaybookInstallOn` |
| D4 | `as` ∈ {空, `current`, `installed`}；其它值 → 400 `value_invalid`；空与 `current` **逐字节等价** | `resource_playbook_release_yaml.go` 的三态分支；`TestPlaybookReleaseYAMLAsParam` 逐字节比对 |
| D5 | `as=installed` 按 `(kind,name)` 合并选中的回执（后一条覆盖前一条）；输出能力 name 升序在前、源 name 升序在后 | `mergedInstallPinned` |
| D6 | 能力按 `(name,version)` 取**含软删行**；源按名字取**当前**（卸载物理删源行）→ 缺失计入头注释 | `store.GetCapabilityForSnapshot`；缺失 → 第 5 行 `# 快照缺失: N 个对象` |
| D7 | 选中回执没有任何 pinned → **409 `no_snapshot`**，`details.causes[0].field = "release"` | `store.ErrPlaybookSnapshotMissing` → `api_status.go` 映射 |
| D8 | 头注释：`as=current` 恒 3 行（逐字不变）；`as=installed` 恒 4 行 + 有缺失时第 5 行 | `releaseYAMLText` / `releaseYAMLInstalledText` |
| D9 | UI：YAML 抽屉里 `.tabs`/`.tab` 二选一（默认当前生效）；`no_snapshot` 时置灰并给原因 | `web-src/pages/release.html` + 四份 locale 各 3 条键 |
| D10 | 单对象 `/yaml` 端点不动；导出里每个文档仍与它逐字节相同 | 既有 7 条导出测试一条未改，全绿 |

一句话：**安装那一刻解析出的每个对象（能力版本 + 摘要 / 源的种类）被记进回执与索引表，导出端点因此能在对象被升级、甚至实例被卸载之后，还原「当初装的是哪一版」。** 默认行为一个字节都没变。

## 1. 端点契约（真源是 `api/resource.yaml` 的该 operation）

`GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}/yaml`

| 请求 | 行为 |
| --- | --- |
| 无 `as` / `as=current` | **逐字节不变**：按名字取当前生效版本；已卸载 → 404 `release_not_found`；头注释 3 行 |
| `as=installed` | 按回执 `created.pinned` 渲染：能力给**安装那一刻那一版**的正文（即使已升级），源按回执记的名字取当前正文；**已卸载仍 200**；头注释固定 4 行（+ 缺失时第 5 行） |
| `as=installed` 且无 pinned | 409 `no_snapshot`，`details.causes[0].field = "release"` |
| `as=其他` | 400 `value_invalid` |
| `valuesDigest` | 两种 `as` 都生效（先按参数组选回执，再取快照 / 当前） |

`as=installed` 头注释（实测输出，已卸载实例，源被物理删 → 第 5 行）：

```
# playbook: pb-sample@1  release: default  values-digest: sha256:9973b735911f805ca3c39c385d27aa0a2e1d23c222b2512b869c1f767b5b70fc
# install: ins_BCjbVhwI2YqLdadCK5  installed-at: 2026-11-23T08:40:35Z  objects: 2
# 版本说明：以下对象为「安装时快照」（安装那一刻记录的版本，不是当前生效版本）
# 源无版本：源按回执记录的名字取当前正文（源没有版本概念）
# 快照缺失: 1 个对象（安装时记录的对象已不存在，未包含在本导出里）
```

409 形状（实测）：

```json
{"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":409,"reason":"no_snapshot",
 "message":"该实例安装于未记录快照的版本，没有安装时快照可导出（可改用 as=current 导出当前生效版本）",
 "details":{"causes":[{"reason":"no_snapshot",
   "message":"该实例的安装回执没有记录对象版本（安装发生在记录安装时快照之前）","field":"release"}]}}
```

## 2. 迁移块与落点

### 2.1 迁移 8（`-- +migration 8 install_pinned`）

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
- `ref_kind` ∈ `capability` / `source`；`obj_kind` 只对源有意义；`seq` 保住回执顺序。
- **存量回执不写任何行** → 读出来 `pinned: []`（不假装知道）。
- 为什么既有回执 JSON 又要一张表：JSON 是审计快照（回执原文），表是 SQL 侧的对账依据（「这个包当初依赖的是哪一版能力」必须能查）—— 与迁移 7（`playbook_install_requires`）同形，也是 #534 立下的做法。

### 2.2 落点表

| 面 | 文件 | 改动 |
| --- | --- | --- |
| 迁移 | `internal/store/schema.sql` | 追加块 8（表 + 索引） |
| 迁移 | `internal/store/migrate.go` | `headVersion = 8` |
| 迁移断言 | `internal/store/v4test/{migrate,format,schema_baseline}_test.go` | 版本号 7→8、`ledger_ahead_of_head` 反例 `1..8`→`1..9`、新表进列/索引 golden |
| wire | `internal/playbook/playbook.go` | `PinnedRef`；`Created.Pinned`；`emptyCreated` 带 `Pinned: []` |
| wire | `internal/playbook/plan.go` | `filledCreated` 透传 `Pinned`（漏这行会把快照抹成 `null`） |
| 写入 | `internal/store/playbook.go` | `InstallPlaybookVersion`：`BuildReceipt` **之前**组装 pinned（created + skipped 两条都组） |
| 写入 | `internal/store/playbook_tx.go` | `pinnedRefsOn`（tx 作用域回读 digest / 源的 kind）；`insertPlaybookInstallOn` 同事务写索引表 |
| 读取 | `internal/store/capability.go` | `GetCapabilityForSnapshot`（**不**过滤 `deleted_at`） |
| 读取 | `internal/store/playbook_export.go` | `PlaybookInstallPinned`（按 `install_id, seq`）+ 读路径归一 |
| 读取 | `internal/store/{playbook,playbook_release_detail,playbook_uninstall}.go` | 读路径 `pinned` 归一成 `[]`；哨兵 `ErrPlaybookSnapshotMissing` |
| 端点 | `internal/server/resource_playbook_release_yaml.go` | `as` 三态；`releaseYAMLInstalledBody` / `mergedInstallPinned` / `releaseYAMLInstalledText` |
| 错误 | `internal/server/api_status.go` | `ErrPlaybookSnapshotMissing` → 409 `no_snapshot` + `field: release` |
| 真源 | `api/resource.yaml` | 该 operation 加 `as` + `409` + 重写 description；`PlaybookCreated.pinned`；新 component `PlaybookPinnedRef`；`Status409` prose 补 `no_snapshot` |
| 一致性 | `internal/server/openapi_parity_test.go` | 登记 `playbook.PinnedRef` ↔ `PlaybookPinnedRef` 一对；`parityPairsFloor 216→217`、`specSchemaFloor 256→257` |
| UI | `web-src/pages/release.html` + 四份 locale | 视图切换（§4） |
| e2e | `scripts/e2e/suites/playbook_release.mjs` | 第 13 段 `partInstalledSnapshot`（57 条断言） |

关键实现点：pinned 的 `digest` 是**在事务内回读刚写入的版本行**得到的（`getCapabilityOn` → `.Digest`），不是另算一份 —— 所以「pinned 的 digest 与版本行一致」是构造出来的事实，不靠巧合。

## 3. 五件套

| 件 | 落点 | 证据 |
| --- | --- | --- |
| API 真源 | `api/resource.yaml`（operation + `PlaybookPinnedRef` + `Status409`） | `make contract` exit 0（5 份 spec / 143 operation / 259 schema） |
| parity | `internal/server/openapi_parity_test.go`：DTO 双向字段集（`pinned` 两个方向都过）+ query 参数双向（`"as"` 字面量真在实现里） | `TestOpenAPIParity*` 全绿（含 `DTOFields` / `ResponsesAndParams` / `Mutations`） |
| DTO | `playbook.Created.Pinned []PinnedRef` | `TestBuildReceiptKeepsCreatedPinned`（JSON 形状 + `skipped.pinned` 是 `[]`） |
| 单测 | store 7 条 + playbook 1 条 + server 4 条（见 §5） | `go test ./internal/store/... ./internal/playbook/... ./internal/server/...` 全绿 |
| e2e | `playbook_release.mjs` 第 13 段 57 条断言 | `[playbook_release] 456 通过 / 0 失败` |

## 4. UI

release 详情页的 YAML 抽屉里加一条二选一（复用既有 `.tabs` / `.tab`，不自造类）：

- 默认选中「当前生效」→ 请求**不带** `as`（默认行为因此逐字节不变）。
- 切「安装时」→ `…&as=installed`。
- 收到 409 `reason === "no_snapshot"` → 「安装时」tab `disabled` + `aria-disabled`、原因行显示 `release.as_no_snapshot`、自动切回「当前生效」并重新取一次。
- 抽屉每次打开重置为「当前生效」；`0 对象 → 空态` 的既有短路只对「当前生效」生效（快照里可能有当前已看不见的对象）。
- 四份 locale 各加 3 条键（`release.as_current` / `release.as_installed` / `release.as_no_snapshot`），键集一致。

已卸载实例在页面上走不到抽屉（既有行为：`gone()` 会禁用「查看 YAML」按钮），所以「卸载后仍可下载」由端点级 e2e 覆盖，不在 UI 层重复。

## 5. 验收（逐条命令与实测）

| # | 验收（issue 原文） | 命令 | 实测 |
| --- | --- | --- | --- |
| 1 | 装 `review@3` → 升级 → `as=installed` 是安装那一版、`as=current` 是新版 | `go test ./internal/server/ -run TestPlaybookReleaseYAMLInstalledSnapshot -v`；e2e 同一形状 | PASS（单测：安装那一版 = `version: 1` + `echo ok`，默认 = `version: 2` + `echo v2`，两份正文不同。e2e 走的就是 issue 的字面形状：先把 `snap-review` 预置到 v2 → **安装建出 v3** → 再升 **v4**，`as=installed` = `version: 3` + `snap-v3`、默认 = `version: 4` + `snap-v4`） |
| 2 | 装 → 卸载 → `as=installed` 仍 200（头注释含 installId）；`as=current` 404 | `go test ./internal/server/ -run TestPlaybookReleaseYAMLInstalledAfterUninstall -v` | PASS（`as=installed` 200 + `install: <id>` + 5 行头注释；默认与 `as=current` 都 404 `release_not_found`） |
| 3 | 老回执（无 `pinned`）→ 409 `no_snapshot`，`field` 指到 release | `go test ./internal/server/ -run TestPlaybookReleaseYAMLNoSnapshot -v` | PASS（409 + `reason=no_snapshot` + `causes[0].field="release"`；同一 release 的默认视图仍 200） |
| 4 | 老库升级后旧回执 `pinned: []`；新装 `pinned` 非空且 digest 与版本行一致 | `go test ./internal/store/ -run 'TestMigrationInstallPinnedTable\|TestInstallReceiptPinned\|TestPlaybookInstallPinnedRows\|TestInstallReceiptPinnedLegacyEmpty\|TestInstallReceiptPinnedSkipped' -v` | PASS（5 条全绿；索引表与回执逐项相等；旧回执 `[]` 且原文逐字不变；同参数重装的 `skipped.pinned` 与首次 `created.pinned` 逐项相同） |
| 5 | e2e：升级 → 两视图版本号不同；卸载后 `as=installed` 仍可下载 | `./scripts/e2e/run_all.sh playbook_release` | `[playbook_release] 456 通过 / 0 失败`（新增 57 条；基线 399）。同一数字出现在 `make e2e-affected` 的全量矩阵里 |
| 6 | 变异 ①/② 必红 | 见 §6 | 两条都真红（贴了 FAIL 原文） |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | `timeout 1500 make ci` / `E2E_CONCURRENCY=1 timeout 2400 make e2e-affected` / `jq '.entries' scripts/e2e/known-env-failures.json` | `make ci` **exit 0**（`go test ./...` 全包 ok；契约/parity/fe/各项守卫全绿；注释密度 43297 = 基线 43297，75 个目录 0 不一致）；`make e2e-affected` **30/30 套件、3195 断言通过 / 0 失败**（642.6s）；known-env `entries: []` 未动 |

### 5.1 `make e2e-affected` 实测

```
$ E2E_CONCURRENCY=1 timeout 2400 make e2e-affected
  ✅ playbook_release   456通过/0失败  6.5s  mode=real
  ✅ frontend           46通过/0失败 188.6s  mode=real
  ...
✅ E2E 全绿：30/30 个请求套件均达标（断言 3195 通过 / 0 失败，authority 清单覆盖 30 个）
AFFECTED_EXIT=0        （总耗时 642.6s）
```

判定口径：改动面含 `api/**` + `internal/**` + `web-src/**` + `scripts/**`，affected 选择器按 fail-closed 规则选中**全部 30 个套件**（不是只挑 4 个）—— 这一条等价于一次全量矩阵。`E2E_CONCURRENCY=1` 是执行器文档化的覆盖项（默认 2）：默认并发下这次矩阵里有 4 个页面在 2.2s 的 `waitForFunction` 上超时（`admin` / `capabilities` / `capability` / `cli-auth`，与本期改动无关的四个页面），单跑 `i18n` 是 33 通过 / 0 失败 —— 属于负载导致的 false red，不是断言不符；降到 1 之后矩阵稳定全绿（§8 第 7 条记录全过程）。

新增的 Chrome 覆盖：`frontend` 的 `playbook-release` case 里钉住「YAML 抽屉默认看当前生效（请求不带 `as`）→ 切「安装时」真的带 `as=installed` → 头注释写明是快照」。

## 6. 变异自证（真改源码跑出来的红，不是声称）

| 变异 | 改法 | 变红 |
| --- | --- | --- |
| ① 安装时不写 `pinned` | `internal/store/playbook.go`：把 `plan.Created.Pinned, err = pinnedRefsOn(...)` 包进 `if false { … }`（等价于「装了但没记」） | 验收 4：`TestInstallReceiptPinned` → `created.pinned = [], want 3 项`；`TestPlaybookInstallPinnedRows` → `索引表与回执不一致: table [] / rcpt []`。验收 1/2：`TestPlaybookReleaseYAMLInstalledSnapshot` → `as=installed 应 200, got 409 … no_snapshot`；`TestPlaybookReleaseYAMLInstalledAfterUninstall` → 同样退化成 409 |
| ② `as=installed` 复用 `as=current` 的取法 | `internal/server/resource_playbook_release_yaml.go`：`if installed {` → `if installed && false {`（快照分支永不进入，一律走当前生效） | 验收 1：`TestPlaybookReleaseYAMLInstalledSnapshot` → `as=installed 头注释要 4 行，got 3` + 头注释写的是「当前生效版本」；验收 2：`TestPlaybookReleaseYAMLInstalledAfterUninstall` → 同上红（卸载后被 404 口径吃掉） |

两次变异都**真改文件 → 跑 → 贴输出 → `git checkout` 复原 → 复绿**（复原后 `go test ./internal/server -run TestPlaybookReleaseYAML` → `ok`）。

## 7. 不做（本票明确不做，写清楚免得下一轮重问）

- 源的历史版本：源没有版本概念，只记名字 + `kindOf`，导出时取当前正文（头注释注明「源无版本」）。
- 回执存整份对象正文：只存指针（`kind/name/version/digest`），正文去 `capability_versions` 取。
- 单对象 `/yaml` 端点行为变化：一个字节没动。
- v3 兼容层 / 回执原文改写：老回执原样保留，只在**读**的时候把缺的键归一成 `[]`。

## 8. 遗留风险 / 已知边界

1. **能力版本行被物理回收后，快照只能报缺失**：`PurgeDeletedCapabilities` 会删掉不再被引用的软删行；被安装钉住的版本行靠 `installed_from` 保护（回执行还在就不回收），若真被回收，`as=installed` 会把它计入「快照缺失」而不是编造一个新版本。这是刻意的降级方向。
2. **D5「后一条回执覆盖前一条」没有专门的 HTTP 单测**：同一 `(kind,name)` 在两条回执里被钉成不同版本，需要构造「卸载后重装并升级同一能力」的时序，而 store 复用版本行让第二回执仍是同一版本（重装的对象进 `skipped`，`pinned` 只由 `created` 生成）。覆盖循环本身是确定的，`valuesDigest` 选组与并集行为已被 `TestPlaybookReleaseYAMLAsParam` 覆盖。
3. **`as` 大小写敏感**：`as=Installed` → 400 `value_invalid`（不走模糊匹配）。
4. **注释密度买预算**：新增注释把 `internal/server` / `internal/store` / `scripts/e2e/fixtures/f3` 顶到基线上方，同批压缩了 `api_status.go` / `openapi_parity_test.go` / `internal/playbook` / 新增测试文件里的若干既有冗长注释（纯注释文本，语义未变），总量回到 43297 = 基线、75 个目录 0 不一致。
5. **为什么 `skipped.pinned` 也记**：只复用、没新建的安装同样解析出了版本。若不记，「同一 release 的第二套参数组（对象全是复用）」按组导出 `as=installed` 会拿到 409 —— 那个实例明明有安装时快照。所以两个列表都记，索引表也两批都写（同名不会同时在两边）。
6. **e2e 前必须重建前端产物**：改了 `api/*.yaml` 或 `web-src/**` 之后 `run_all.sh` 会以 `dist 陈旧` 硬失败（本期在改 `api/resource.yaml` 的 `skipped.pinned` 描述时踩过一次），先 `make fe`。
7. **矩阵类套件在负载下会 false red**：第一次全量矩阵（默认 `E2E_CONCURRENCY=2`）里 `i18n` 有 4 个页面在 2.2s 的 `waitForFunction` 上超时，同一分支单跑 `i18n` 33 通过 / 0 失败；第二次矩阵（`E2E_CONCURRENCY=1`）30/30 全绿。没有写进 known-env（`entries` 保持 `[]`）：那是「GitHub runner 的环境性失败」登记表，本地负载抖动不是它的适用对象。
