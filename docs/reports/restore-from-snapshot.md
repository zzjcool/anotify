# 按快照重装（`…/releases/{release}/restore`）：已卸载实例照 pinned 版本复原（#561）

<!-- snapshot-doc: 2026-11-23 #561 交付当次的契约与断言（本章先冻结契约，数字在 §5 落地后补齐） -->

> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

as_of: 2026-11-23 · area: `api` / `store` / `server` / `web` / `e2e` · 迁移块 `11 playbook_restore_audit`（新增，见 §1.3）
起点 `main@f7c0d1c9`（`#552` 回执 pinned、`#557` 安装历史、`#560` 审计读面已合入）

## 0. 结论（契约先冻结，落地事实见 §2）

`#552` 让「已卸载实例装了什么」只剩**看**：回执 `created.pinned` + `?as=installed` 能导出当时那一版，
但要复原还得照着手敲。本票把「看」变成「照快照装回去」，且**三个前提先钉死**：

| # | 冻结决策 | 落点 |
| --- | --- | --- |
| D1 | 复原用 **pinned 版本正文**，不是当前最新版 | 能力取 `capability_versions` 里 `(ns, name, pinned.version)` 那一行的正文（含软删行：卸载只 SET `deleted_at`，正文还在）；pinned 版本被物理回收 → `409 pinned_gone`，**绝不退化成最新版** |
| D2 | 整包原子：冲突 / 缺失时 0 写 | 单事务；所有判定在第一条 INSERT 之前完成；e2e 用 6 张表指纹证明「409 前后逐字节不变」 |
| D3 | 同名对象内容相同 → 复用；不同 → 409 | 能力身份是 `(name, version)`：可见行 digest 相等 → 记 `reused.capabilities`，一行不改；同名同版本但正文不同 → `409 restore_conflict` |
| D4 | 源按名字：有则复用，没有则 409 | 源正文不在回执里，**不假装能重建** → `409 pin_missing_source`，让用户先把源建起来 |
| D5 | 审计一行 `install.restored` | 迁移块 11 的 `playbook_install_audit`（action 闭集 `install.restored`），与复原同一个事务；`GET …/releases/{release}` 的 `restores[]` 是它的读面（actor + 计数） |
| D6 | 新回执与首次安装**同形**，多一个 `restoredFrom` | `receipt.restoredFrom` = 被复原的那次 install id；`created` 是本次重建的、`skipped` 是复用到的，`values`/`valuesDigest` 沿用原回执（打码后形态） |

一句话：**照着安装时快照装回去，版本不漂移、失败不半写、源没了就明说。**

## 1. 端点契约（真源是 `api/resource.yaml` 的 restore operation）

### 1.1 `POST /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}/restore`

请求体 `{ "valuesDigest": "d7d234f759ec" }`（必填，空 → `400 value_invalid`）。
`valuesDigest` 选的是「哪一组参数」：同一 release 用两套 values 装过是两组实例，复原只复原选中那组。

| 状态 | reason | 何时 |
| --- | --- | --- |
| 200 | — | 复原完成，回执同首次安装形状 + `restoredFrom` |
| 400 | `value_invalid` | 请求体缺 `valuesDigest` / 为空 |
| 404 | `release_not_found` | 该 release（或该 values 组）从未在此命名空间安装过 |
| 409 | `already_installed` | 该组参数当前仍在生效（回执 `uninstalled_at = 0`），无需复原 |
| 409 | `no_snapshot` | 该组最新回执没有 pinned（迁移 8 之前装的）；`details.causes[0].field = "release"` |
| 409 | `pinned_gone` | 某个 pinned 能力版本的正文已被物理回收；`causes[]` 逐条列出 `kind` / `name` / `version` |
| 409 | `pin_missing_source` | 某个 pinned 源在命名空间里不存在（源正文不在回执里，不重建）；`causes[]` 逐条列出 `kind` / `name` |
| 409 | `restore_conflict` | 同名同版本的能力已存在且**正文不同**；`causes[]` 逐条列出冲突对象与两边 digest |
| 500 | `InternalError` | 未预料的失败 |

三种 409 的判定顺序固定：`pinned_gone` → `pin_missing_source` → `restore_conflict`
（先报「数据没了」再报「你得先建东西」最后才是「内容对不上」），全部在写之前返回。

### 1.2 语义（五条，逐条对应验收）

1. **按 pinned 版本**：能力正文取 `pinned.version`；被硬删 → 409 `pinned_gone`
2. **整包 0 写**：冲突 / 缺失时一行不写（与 `#535` 的 requires 同一条纪律）
3. **同名对象相同 → 复用**（`reused`，不覆盖不报错）；不同 → 409 `restore_conflict`
4. **源按名字**：同名存在 → 复用（`reused.sources`）；不存在 → 409 `pin_missing_source`
5. **审计 + 回执**：`install.restored` 一行；新回执带 `restoredFrom`

### 1.3 迁移块（新增一个，headVersion 10 → 11）

`playbook_install_audit`：`id` / `namespace_id` / `playbook_id` / `playbook_slug` / `release` /
`install_id` / `action`（闭集 `install.restored`）/ `actor_id` / `detail` / `created_at`。
为什么加块而不是塞进既有表：`capability_audit` 按能力名组织、`source_audit` 按源 id 组织，
塞进去会让「某个实例被谁复原过」变成一次跨表拼装；而回执行 `playbook_installs` 只记「装了什么」，
没有 actor 列可加（回执是用户可见文档，不该承载操作者身份）。

## 2. 落点（文件所有权）

| 层 | 文件 | 内容 |
| --- | --- | --- |
| store | `internal/store/playbook_restore.go` | `RestorePlaybookRelease`（单事务：判定 → 写 → 回执 → 审计）、`ListPlaybookInstallAudit` |
| store | `internal/store/schema.sql` / `migrate.go` | 迁移块 11 + `headVersion = 11` |
| playbook | `internal/playbook/playbook.go` | `Receipt.RestoredFrom` |
| server | `internal/server/resource_playbook_restore.go` | handler + 409 映射（`causes[]`） |
| server | `internal/server/resource_playbook_release_detail.go` | `restores[]`（审计读面） |
| api | `api/resource.yaml` | restore operation + `PlaybookRestoreRequest` + `PlaybookReceipt.restoredFrom` |
| api parity | `internal/server/openapi_parity_test.go` | 新 pair + `restores[]` 字段 |
| web | `web-src/pages/release.html` + `web-src/locales/*.yaml` ×4 | 已卸载态的「按快照重装」+ 二次确认 |
| e2e | `scripts/e2e/suites/playbook_release.mjs` | 10 条验收 + 6 表指纹 |
| 单测 | `internal/store/playbook_restore_test.go`、`internal/server/playbook_restore_test.go` | 五条语义 + 0 写 + 审计 |

## 3. 验收（10 条）与变异（2 条）

见 §4（落地后逐条填「怎么断言 / 怎么红」）。

## 4. 待补

## 5. 门禁数字

待补。
