# 按快照重装（`…/releases/{release}/restore`）：已卸载实例照 pinned 版本复原（#561）

<!-- snapshot-doc: 2026-11-23 #561 交付当次的契约、断言与门禁数字（定格记录；复算命令见 §5） -->

> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

as_of: 2026-11-23 · area: `api` / `store` / `server` / `web` / `e2e` · 迁移块 `11 playbook_restore_audit`（新增，见 §1.3）
起点 `main@f7c0d1c9`（`#552` 回执 pinned、`#557` 安装历史、`#560` 审计读面已合入）

## 0. 结论

`#552` 让「已卸载实例装了什么」只剩**看**（回执 `created.pinned` + `?as=installed`），要复原还得照着手敲。
本票把「看」变成「照快照装回去」，三个前提先钉死：

| # | 冻结决策 | 落点事实 |
| --- | --- | --- |
| D1 | 复原用 **pinned 版本正文**，不是最新版 | `capabilitySnapshotByVersionOn` 取 `(ns, name, pinned.version)` 那一行的正文（**不过滤 `deleted_at`**：卸载只打标记，正文还在）；那一版被物理回收 → `409 pinned_gone`，**绝不退化成最新版** |
| D2 | 整包原子：冲突 / 缺失 0 写 | 单事务；`restoreCandidatesOn` 一次算完所有判定，`err()` 在第一条 INSERT 之前返回；e2e 用 **6 表指纹**（与 `#535` 同一组表）证明「409 前后逐字节相同」 |
| D3 | 同名对象内容相同 → 复用；不同 → 409 | 能力身份是 `(name, version)`：可见行 `digest` 相等 → `skipped.capabilities`（一行不改、不覆盖）；同名同版本但 digest 不同 → `409 restore_conflict` |
| D4 | 源按名字：有则复用，没有则 409 | 源正文不在回执里，**不假装能重建** → `409 pin_missing_source`（先建源再复原） |
| D5 | 审计一行 `install.restored` | 迁移块 11 的 `playbook_install_audit`（`action` 闭集 `install.restored`），与复原**同一个事务**；读面是详情端点的 `restores[]`（actor + 计数） |
| D6 | 新回执与首次安装**同形**，多 `restoredFrom` | `receipt.restoredFrom` = 被复原的那次 install id；`created` 是重建的、`skipped` 是复用到的，`values` / `valuesDigest` / `playbook` 身份沿用原回执 |

一句话：**照安装时快照装回去，版本不漂移、失败不半写、源没了就明说。**

## 1. 端点契约（真源是 `api/resource.yaml` 的 restore operation）

### 1.1 `POST /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}/restore`

请求体 `{ "valuesDigest": "sha256:…" }`（必填；空 → `400 value_invalid`）。`valuesDigest` 选「复原哪一组参数」：
同一 release 用两套 values 装过是**两组实例**，复原只复原选中那组。

| 状态 | reason | 何时 |
| --- | --- | --- |
| 200 | — | 复原完成：完整回执（同首次安装）+ `restoredFrom` |
| 400 | `value_invalid` | 请求体缺 `valuesDigest` / 为空 |
| 404 | `release_not_found` | 该 release（或该 values 组）从未在此命名空间安装过 |
| 409 | `already_installed` | 该组参数当前仍在生效（回执 `uninstalled_at = 0`），无需复原 |
| 409 | `no_snapshot` | 该组最新回执没有快照（迁移 8 之前装的）；`details.causes[0].field = "release"` |
| 409 | `pinned_gone` | pinned 能力版本的正文已被物理回收；`causes[]` 逐条点名 `{kind,name,version}` |
| 409 | `pin_missing_source` | pinned 源不存在（源正文不在回执里，不重建）；`causes[]` 逐条点名 |
| 409 | `restore_conflict` | 同名同版本的能力已存在且正文不同；`causes[]` 带库里与快照两个 digest |
| 500 | `InternalError` | 未预料的失败 |

三条 409 的判定顺序固定：`pinned_gone` → `pin_missing_source` → `restore_conflict`
（先报「数据没了」，再报「你得先建东西」，最后才是「内容对不上」），**全部在写之前返回**。
`details.causes[].field` 形如 `pinned.capabilities[0]` / `pinned.sources[1]`（与 `requires` 的 field 同一套定位口径）。

### 1.2 五条语义

1. **按 pinned 版本**：能力正文取 `pinned.version`；被硬删 → `409 pinned_gone`
2. **整包 0 写**：冲突 / 缺失时一行不写（与 `#535` 的 requires 同一条纪律，6 表指纹为证）
3. **同名对象相同 → 复用**（`skipped`，不覆盖不报错）；不同 → `409 restore_conflict`
4. **源按名字**：同名存在 → 复用（`skipped.sources`）；不存在 → `409 pin_missing_source`
5. **审计 + 回执**：`install.restored` 一行；新回执带 `restoredFrom`

### 1.3 迁移块（新增一个：`headVersion` 10 → 11）

`playbook_install_audit`：`id` / `namespace_id` / `playbook_id` / `playbook_slug` / `release` /
`install_id` / `action`（闭集 `install.restored`）/ `actor_id` / `detail`（JSON：`restoredFrom` +
`recreated` / `reused` 名单 + `pinnedVersions`）/ `created_at`，索引 `(namespace_id, playbook_slug, release, created_at DESC, id DESC)`。

**为什么必须加块**：`capability_audit` 按能力名组织、`source_audit` 按源 id 组织，塞进去会让「某个实例被谁复原过」
变成一次跨表拼装；而回执行 `playbook_installs` 只记「装了什么」，没有 actor 列可加（回执是用户可见文档，
不该承载操作者身份）。按仓规**不改 baseline**、只追加块，已有本地库会被就地迁移（`schemaLayoutAt` 按记账版本重放）。

## 2. 落点（文件所有权）

| 层 | 文件 | 内容 |
| --- | --- | --- |
| store | `internal/store/playbook_restore.go` | `RestorePlaybookRelease`（单事务：判定 → 写 → 回执 → 审计）、`ListPlaybookInstallAudit`、`PlaybookRestoreError`（分类 + causes） |
| store | `internal/store/schema.sql`（块 11）/ `migrate.go`（`headVersion = 11`） | 审计表 |
| playbook | `internal/playbook/playbook.go` | `Receipt.RestoredFrom`（`omitempty`：不带它的回执逐字节不变） |
| server | `internal/server/resource_playbook_restore.go` | handler + 严格解码 + 409 映射（`causes[]`） |
| server | `internal/server/resource_playbook.go`（路由）/ `…_release_detail.go`（`restores[]`） | 注册 + 审计读面 |
| api | `api/resource.yaml` | restore operation + `PlaybookRestoreRequest` + `PlaybookReceipt.restoredFrom` + `PlaybookRestoreEntry` / `PlaybookRestoreCounts` |
| api parity | `internal/server/openapi_parity_test.go` | 3 个新 pair、`muxPatternCount` 139→140、operation 150→151、`specSchemaFloor` 262→265 |
| web | `web-src/pages/release.html`、`web-src/pages/playbook.html`、四份 `web-src/locales/*.yaml` | 已卸载态「按快照重装」+ 二次确认（列出将重建的能力与版本）+ 回包页高亮 |
| e2e | `scripts/e2e/suites/playbook_release.mjs` §15 | 10 条验收 + 6 表指纹 + 审计行 |
| 单测 | `internal/store/playbook_restore_test.go`、`internal/server/playbook_restore_test.go` | 五条语义 + 0 写 + 审计 + 传输面 |

**UI 行为**：已卸载且 `pinned` 非空的实例才出现 `#rd-restore`（生效中实例的 DOM 不变）；
二次确认列出将重建的能力 `name@vN`（>6 个截断）与源数量，并明说「源不会重建，缺源会 409」；
成功后跳回包页 `?id=<slug>&restored=<release>`，那一行滚到视口中间 + `accent-soft` 淡出高亮，
`replaceState` 抹掉参数（刷新不重复滚动）。409 的五个 reason 各自一句人话，失败时按钮恢复可点。

已知限制：已卸载视图的 `pinned` 是**整份 release 的合并快照**（#553 口径），多参数组时确认弹层会多列别的组的能力；
重装本身仍只复原**选中的那一组**（`params.digest`）。要精确到组需要新的 DTO 字段，不在本票范围。

## 3. 十条验收 → 断言位置

| # | 验收 | store 单测 | e2e（§15） |
| --- | --- | --- | --- |
| 1 | 装 v3 → 升 v4 → 卸载 → restore 重建 **v3** | `TestRestoreUsesPinnedVersionNotLatest`：断言 `created.capabilities = demo-review@1`、`playbook.version` 仍是来源的 1、v1 正文含 `echo v1`、v2 没复活 | `①` 段：同名能力换正文长出 v2 后复原，断言 `created.capabilities=[cap@1]`、按版本读 v1 正文 = `pin-v1`、v2 正文 = `pin-v2`（两版真的不同） |
| 2 | 同组参数已生效 → 409 `already_installed` | `TestRestoreAlreadyInstalled` + 指纹不变 | `②` 段 |
| 3 | 老回执（无 pinned）→ 409 `no_snapshot`，field 指到 release | `TestRestoreNoSnapshot`（落老回执形状 + 指纹不变） | `③` 段（`seedLegacyReceiptRow` + `causes[0].field === "release"`） |
| 4 | pinned 版本硬删 → 409 `pinned_gone` + `{kind,name,version}` + 0 写 | `TestRestorePinnedGone`（`DELETE FROM capability_versions` 模拟 GC + 指纹/审计双证） | `④` 段（`expectZeroWrite` + message 含 `kind=capability`/name/version） |
| 5 | 同名内容相同 → 复用；不同 → 409 + 0 写 | `TestRestoreConflictAndReuse`（**2 能力包**：一个冲突 + 一个本该新建；指纹 + 「demo-b 没被写进去」双证） | `⑤` 段（`insertManualCapabilityRow` 造冲突/同内容）+ `① 对照` 段（2 能力包，同上） |
| 6 | 缺源 → 409 `pin_missing_source` | `TestRestoreMissingSource`（卸载物理删源 + 指纹不变 + 补建源后 200） | `⑥` 段 |
| 7 | 审计有 `install.restored` 一行 | `TestRestoreAuditAndReceipt`（action / actor / installId / detail.restoredFrom / 计数） | `⑦` 段（`restores[]` 全字段 + `playbook_install_audit` 落 1 行、actor_id 与读面同值） |
| 8 | 变异 ① / ② 必红 | 见 §4（两条都在 store 与 e2e 上实测变红） | 见 §4 |
| 9 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | 见 §5 | 见 §5 |
| 10 | 五件套齐；四份 locale 键集一致 | `TestOpenAPIParity*`（151 operations / 140 mux pattern / 3 个新 schema pair） | `make check-i18n-keys`（4 份 locale / 2520 keys 一致） |

## 4. 两条变异（实测）

| # | 变异（临时改产品代码，不入库） | 期望 | 实测 |
| --- | --- | --- | --- |
| ① | `capabilitySnapshotByVersionOn` 改成先取该名字的 `MAX(version)`（复原用最新版） | 验收 1 红 | store `TestRestoreUsesPinnedVersionNotLatest` 红：`created.capabilities = [demo-review@2]`；e2e `①` 红（见 §4.1） |
| ② | 冲突时先把 `createCaps` 写进库再报错（`_ = tx.Commit()`，不整包回滚） | 验收 5 零写入红 | store `TestRestoreConflictAndReuse` 红：`restore_conflict 必须整包 0 写（demo-b 也不许先写进去）`；e2e `① 对照` 段指纹/能力列表红（见 §4.1） |

## 5. 门禁数字

待补（`make ci` / `make e2e-affected` / 变异实测输出见下节）。
