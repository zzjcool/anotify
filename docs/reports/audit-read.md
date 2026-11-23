<!-- snapshot-doc: 2026-11-23 #559 交付时的定格数字；随代码变更请重测，不要把本文件的数字当现值 -->
# 把 source/secret 审计读出来（#559）交付报告

> **历史快照**。本文件记录 #559 交付当时的事实与实测数字（基线 `391a0c39`，迁移号 10）。
> 后续改动请重跑本文档里的命令再引用。

## 0. 结论

两张审计表（`source_audit` 迁移 10 / `namespace_secret_audit` 迁移 9）此前只写不读。本次补齐两个读端点
（`GET …/sources/{name}/audit`、`GET …/secrets/{name}/audit`）、源详情页的「最近操作」折叠块与「凭据历史」弹层，
并把「五件套」（api 真源 ↔ parity ↔ DTO ↔ e2e ↔ 单测）一次拉通。

**没有新增迁移号**：两张表与它们的索引都已存在，读端点只是多一条 `SELECT`。

## 1. 端点契约

```
GET /v1/namespaces/{ns}/sources/{name}/audit?limit=N&before=<RFC3339>
  → 200 {items:[{id,action,actor,at,detail}]}
GET /v1/namespaces/{ns}/secrets/{name}/audit?limit=N&before=<RFC3339>
  → 200 {items:[{id,action,actor,at}]}
```

- 形状与能力审计一致：非信封、`items` 最新在前、空为 `[]`（不是 `null`）。
- `at` = RFC3339 UTC；`actor` 空串 = 非会话主体（CLI / 系统），不做伪造。
- source 的 `action` 闭集 `source.polled_manually`；secret 的闭集 `created|rotated|deleted`。
- `limit`：缺省 50、上限 200；**显式** `limit=0` / `limit=201` / 负数 → 400（不夹取）。
- `before`：RFC3339 时间戳，取严格早于它的一页；精度 = 秒（同一秒的记录不重复返回，可能一并略过）。
- 未知 name 或跨命名空间同名 → **404**，与「不存在」同码（不泄露存在性）。
  这是与能力审计（未知 name → `200 {"items":[]}`）的**刻意差异**，理由写在 spec 的 description 里。

## 2. 落点

| 层 | 文件 |
| --- | --- |
| store 读 | `internal/store/source_audit.go`（`ListSourceAudit` + `AuditDefaultLimit`/`AuditMaxLimit`）、`internal/store/namespace_credential.go`（`ListNamespaceSecretAudit` 加 `beforeAt`） |
| server | `internal/server/resource_audit_query.go`（`auditLimitQuery`/`auditBeforeQuery`）、`internal/server/resource_source.go`（`(*sourceAPI).audit`）、`internal/server/resource_credential.go`（`(*secretAPI).audit`） |
| 契约 | `api/resource.yaml`（两条 path + 四个 schema） |
| parity | `internal/server/openapi_parity_test.go`（op 数、mux pattern 数、parityPairs、schema 数四个棘轮） |
| UI | `web-src/pages/source-detail.html`、`web-src/locales/{zh-CN,en,ja,es}.yaml`、`web/mock.js` |
| e2e | `scripts/e2e/suites/namespace_credentials.mjs`、`scripts/e2e/suites/routing.mjs` |

## 3. 五件套

| 件 | 落点 |
| --- | --- |
| api 真源 | `api/resource.yaml`：`listSourceAudit` / `listNamespaceSecretAudit` |
| parity | 四条 DTO ↔ 四个 schema 的 `parityPairs()` 配对；op 总数与 mux pattern 数的冻结快照同步 |
| DTO | `sourceAuditEntryDoc`/`sourceAuditListDoc`、`secretAuditEntryDoc`/`secretAuditListDoc` |
| e2e | 凭据侧（created/rotated/deleted 倒序、limit 边界、跨 ns 404、值不泄露）+ 源侧（polled_manually、跨 ns、limit 边界、before） |
| 单测 | store 读（游标/过滤/边界）+ server handler（形状/边界/404/401/值不泄露） |

## 4. 「值不泄露」的证据

1. `namespace_secret_audit` 表结构本来就没有值列（`internal/store/schema.sql`，迁移 9），审计写入只接受 `(namespace, name, action, actor)`。
2. 读端点的 `SELECT` 只取 `id, name, action, actor_id, created_at`；secret 审计 DTO **没有** `detail` 键。
3. 单测断言响应正文里 grep 不到写入的凭据值。
4. e2e 的 G7 类断言把本次全部响应 body 拼起来，逐个写入值必须搜不到。

## 5. 8 条验收 → 证据

| # | 验收 | 证据 |
| --- | --- | --- |
| 1 | 手动拉一次 → 审计有 `polled_manually`，actor 与会话一致 | （待填：单测 + e2e 断言名） |
| 2 | 创建/轮换/删除 → 三条按时间倒序；值不出现在任何响应 | （待填） |
| 3 | `limit=1` 只回 1 条；`limit=0`/`limit=201` → 400 | （待填） |
| 4 | 跨命名空间访问 → 404 | （待填） |
| 5 | UI：展开「最近操作」能看到记录；空态文案；凭据历史弹层能开 | （待填） |
| 6 | 变异 ①（忽略 limit）/ ②（不按 namespace 过滤）都必红 | （待填） |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | （待填） |
| 8 | 五件套 + 四份 locale 键集一致 | （待填） |

## 6. 变异自证（两条必红）

（待填：变异 diff 片段 + 实测 `--- FAIL` 输出 + 还原方式）

## 7. 门禁数字

（待填：`make ci` 的 exit code、`make e2e-affected` 的套件/断言数、known-env `entries`、注释密度总量）

## 8. UI

（待填：折叠块的懒加载证据、空态截图要点、弹层字段）

## 9. 不做 / 遗留风险

- 全局审计流（跨资源）与审计导出/归档：issue 明确不做，本次也不做。
- `config.secret` 老路径（安装/编辑源的副产物）**不写**审计，与 #554 语义一致；本次未改这条语义。
- `source_audit.source_id` 对 `sources` 是 `ON DELETE CASCADE`：删源会连它的审计一起消失。读端点因此只服务「活着的源」；若要保留墓碑历史需要另立外键策略（本次不做）。
