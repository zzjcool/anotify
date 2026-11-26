<!-- snapshot-doc: 2026-11-23 #559 交付时的定格数字；随代码变更请重测，不要把本文件的数字当现值 -->
# 把 source/secret 审计读出来（#559）交付报告

> **历史快照**。本文件记录 #559 交付当时的事实与实测数字（基线 `391a0c39`，`headVersion = 10`）。
> 后续改动请重跑本文档里的命令再引用。

## 0. 结论

`source_audit`（迁移 10）与 `namespace_secret_audit`（迁移 9）此前只写不读。本次补齐两个读端点
（`GET …/sources/{name}/audit`、`GET …/secrets/{name}/audit`）、源详情页的「最近操作」折叠块与
「凭据历史」弹层，并把五件套（api 真源 ↔ parity ↔ DTO ↔ e2e ↔ 单测）一次拉通。

**没有新增迁移号**：两张表与索引都已存在，读端点只是多一条 `SELECT`（`headVersion` 仍是 10）。
**没有改写入语义**：`rotated` 早在 #554 就记了，`config.secret` 老路径仍不写审计。

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
- `limit`：缺省 50、范围 1..200；**显式** `limit=0` / `201` / `-1` → 400（不夹取，边界由 handler 判）。
- `before`：RFC3339 时间戳（就是上一条的 `at`），取**严格早于**它的一页；精度 = 秒。
- 未知 name 或跨命名空间同名 → **404**（与「不存在」同码，不泄露存在性）。
  这是与能力审计（未知 name → `200 {"items":[]}`）的**刻意差异**：审计要挂在实体上，
  而实体解析本身就是命名空间隔离的那条边。

两处实现细节（值得知道，别当成笔误）：

1. `before` 的缺省哨兵是 `0`，而 `1970-01-01T00:00:00Z` 的 Unix 秒**正好是 0**。
   所以 handler 把「纪元及更早」归一到 `-1`，让 `before=<epoch>` 真的回空页而不是回全部。
2. 审计读端点的 `before` 与 v4 退役的信封游标词撞名。`scripts/check_envelope.py` 的 E7 原来
   对**任意**参数名禁 `before`（v4 只有 `continue`）。本次给守卫加了**按路径收窄**的例外
   （`AUDIT_TIME_CURSOR_WHERE`，只放行 `…/audit` 的 operation），守卫自测里那条
   「往 `GET …/sources` 塞 `?before=` 必须红」的变异用例原样保留并通过。
   理由：退役的是**信封分页游标**（`continue` 取代 `nextCursor`/`before`/`sinceId`），而审计读端点
   不是信封列表，issue #559 冻结了 `?before=<RFC3339>` 这个名字。

## 2. 落点

| 层 | 文件 |
| --- | --- |
| store 读 | `internal/store/source_audit.go`（`ListSourceAudit` + `AuditDefaultLimit`/`AuditMaxLimit`）、`internal/store/namespace_credential.go`（`ListNamespaceSecretAudit` 加 `beforeAt`） |
| server | `internal/server/resource_audit_query.go`（`auditLimitQuery`/`auditBeforeQuery`）、`internal/server/resource_source.go`（`(*sourceAPI).audit`）、`internal/server/resource_credential.go`（`(*secretAPI).audit`） |
| 契约 | `api/resource.yaml`（两条 path + 四个 schema） |
| parity | `internal/server/openapi_parity_test.go`（op 数 148→150、mux pattern 137→139、parityPairs 218→222、schema 258→262） |
| UI | `web-src/pages/source-detail.html`、`web-src/locales/{zh-CN,en,ja,es}.yaml`、`web/mock.js` |
| e2e | `scripts/e2e/suites/namespace_credentials.mjs`、`scripts/e2e/suites/routing.mjs` |
| 守卫 | `scripts/check_envelope.py`（E7 的按路径例外，见 §1 第 2 条） |

## 3. 五件套

| 件 | 落点 |
| --- | --- |
| api 真源 | `api/resource.yaml`：`listSourceAudit` / `listNamespaceSecretAudit`（各带 `limit` 与 `before`） |
| parity | 四个 DTO ↔ 四个 schema 进 `parityPairs()`；op 总数与 mux pattern 快照同步 |
| DTO | `sourceAuditEntryDoc`/`sourceAuditListDoc`、`secretAuditEntryDoc`/`secretAuditListDoc` |
| e2e | 凭据侧 AU 组 50 条 + 源侧 §16 组 49 条（见 §5/§7） |
| 单测 | store 8 个 `TestList*Audit*` + server 4 个 `Test*AuditRead*` |

四份 locale 键集一致（各新增 18 键），`check-i18n-keys` / `check-classes` / `check-ui-copy` 全绿。

## 4. 「值不泄露」的证据

1. 表本身没有值列：`namespace_secret_audit` 只有 `(id, namespace_id, name, action, actor_id, created_at)`。
2. 写入侧只接受 `(namespace, name, action, actor)`，审计与业务写入**同事务**（`appendNamespaceSecretAuditOn`）。
3. 读端点的 `SELECT` 只取 `id, name, action, actor_id, created_at`；`secretAuditEntryDoc` **没有**
   `detail`、更没有 `value` 字段（parity 会把「DTO 有而 spec 没有」也判红，两边都不许加）。
4. 断言层：e2e 把本次 **58 份**响应正文拼起来 grep，逐个写入值必须搜不到（AU11）。
5. `?limit=0` / 跨 ns 这类错误响应也不回显值（错误体只有 reason + message）。

## 5. 8 条验收 → 证据

| # | 验收 | 证据 |
| --- | --- | --- |
| 1 | 手动拉一次 → 审计有 `polled_manually`，actor 与会话一致 | `TestSourceAuditReadEndpoint`；e2e routing §16.1（`actor` 与 `GET /v1/auth/me` 的 id **逐字**相等、`detail` 恰好四键） |
| 2 | 创建/轮换/删除 → 三条按时间倒序；值不出现在任何响应 | 端点侧：建 → 轮换 → 读 2 条 `[rotated, created]`；删除后实体消失 → 端点按契约 404，故 `deleted` 用 `V.withDB` 直读表证 `[created, rotated, deleted]` 齐全（e2e AU9）。值不泄露见 §4 |
| 3 | `limit=1` 只回 1 条；`limit=0`/`201` → 400 | 单测：`TestSourceAuditReadEndpoint` / `TestSecretAuditReadEndpoint` / `TestListSourceAuditLimitBounds` / `TestListNamespaceSecretAuditLimitBounds`；e2e AU5/AU6 与 §16.3/16.4（含 `-1` 与 `abc`，并逐字钉住三条 message） |
| 4 | 跨命名空间访问 → 404 | 单测：`TestSourceAuditReadIsolatesNamespaces`、`TestSecretAuditReadIsolatesNamespaces`；e2e AU8 / §16.6（跨 ns 与本 ns 不存在的 404 正文**逐字节相同**） |
| 5 | UI：展开能看到记录；空态文案；凭据历史弹层能开 | 见 §8（真浏览器冒烟：默认收起不请求、展开恰好一次请求、空态一句、弹层三列） |
| 6 | 变异 ①/② 必红 | 见 §6 |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | 见 §7 |
| 8 | 五件套 + 四份 locale 键集一致 | 见 §3 |

## 6. 变异自证（两条必红）

两次变异都只改一处，跑完立即 `cp` 还原，`sha256sum` 与改前逐字节一致，还原后再跑一次为绿。

### ① 端点忽略 `limit`

变异：`auditLimitQuery` 保留边界校验，但返回 `store.AuditDefaultLimit`（不再回 `limit`）。

```
$ go test ./internal/store/ ./internal/server/ -run 'Audit' -count=1
--- FAIL: TestSourceAuditReadEndpoint (0.11s)
    resource_audit_read_test.go:231: limit=1 只回 1 条，实际 2
--- FAIL: TestSecretAuditReadEndpoint (0.05s)
    resource_audit_read_test.go:338: limit=1 只回 1 条，实际 2
FAIL	github.com/zzjcool/anotify/internal/server	0.452s

$ ./scripts/e2e/run_all.sh --serial namespace_credentials routing
    ❌ AU5 limit=1 → 恰好 1 条 — 期望 1 实际 2
    ❌ 16.3 limit=1 → 恰好 1 条 — 期望 1 实际 2
  断言通过: 634   断言失败: 2
❌ E2E 未全绿
```

### ② 审计查询不按 namespace 过滤

变异：两个 store 读函数的 `WHERE namespace_id = ?` 换成恒真谓词（`? != ''`），arg 位置不动。

```
$ go test ./internal/store/ ./internal/server/ -run 'Audit' -count=1
--- FAIL: TestListNamespaceSecretAuditNamespaceAndNameScope (0.05s)
--- FAIL: TestListSourceAuditNamespaceAndSourceScope (0.05s)
--- FAIL: TestSecretAuditReadIsolatesNamespaces (0.06s)

$ ./scripts/e2e/run_all.sh --serial namespace_credentials
    ❌ AU8 nsB 读到的是空的 {"items":[]}（同名不串号：变异②的 HTTP 探测器） —
      {"items":[{"id":"saud_BCkEGJKlmQrrH3sjjy","action":"rotated","actor":"usr_BCkEGICyhCx1E582Pi",…},
                {"id":"saud_BCkEGJKIL1RCaNtjgB","action":"created","actor":"usr_BCkEGICyhCx1E582Pi",…}]}
  断言通过: 127   断言失败: 1
```

**注意源侧的观测边界**（诚实记录）：`ListSourceAudit` 的 WHERE 是 `(namespace_id, source_id)`，而
`source_id` 跨命名空间全局唯一 —— 去掉 ns 过滤对源侧 HTTP 响应**没有可观测差异**。所以源侧的变异 ②
探测器放在 store 级（`ListSourceAudit(nsB, nsA 的 sourceID)` 必须为空，上面第一条 FAIL 就是它）；
凭据侧才是 HTTP 级同名跨 ns 探测器（`(namespace_id, name)` 会串号，上面第二段输出即证据）。

## 7. 门禁数字

```
$ export PATH=/usr/local/go/bin:$PATH; timeout 1500 make ci          → CI_EXIT=0
$ E2E_CONCURRENCY=1 timeout 2400 make e2e-affected                   → AFFECTED_EXIT=0
  全量 e2e（31/31 套）：api/resource.yaml 落在共享面（五份 OpenAPI 真源）
  ✅ E2E 全绿：31/31 个请求套件均达标（断言 3454 通过 / 0 失败，authority 清单覆盖 31 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 642.2s）
$ ./scripts/e2e/run_all.sh --serial namespace_credentials routing    → E2E_EXIT=0
  断言通过: 636（namespace_credentials 128 / routing 508）· 断言失败: 0 · 总耗时 11.0s
$ jq '.entries' scripts/e2e/known-env-failures.json                  → []
$ node scripts/check-comment-density.mjs
   注释行 43296 / 基线 43296 · 目录：75 个有基线，0 个与基线不一致   → exit 0（不上调）
$ node scripts/check-test-discipline.mjs
   e2e/waitForTimeout 21/21 ✅ · e2e/swallow-catch 38/38 ✅ · go/t.Skip 3/3 ✅
   go/time.Sleep 37/37 ✅ · e2e/bare-tag-first-nth 3/3 ✅              → exit 0
```

`make ci` 覆盖到的本次相关项：`make contract`（五份 spec 150 operation、信封字段级、`TestOpenAPIParity`）、
`check-comment-density`（零余量棘轮）、`check-annotations`、`check-doc-links`、`check-doc-endpoints`、
`test-frontend`、`go test ./...`（含本次新增单测）、`check-e2e-lock`。

## 8. UI

- 源详情页（`kind: poll` 才有）：`「拉取」区块下方「最近操作」`，原生 `<details>` **默认收起**；
  首次展开才 `GET …/sources/{name}/audit?limit=20`（状态机 idle→loading→ok/error，`AUDIT_SEQ` 防旧响应覆盖）。
  换源/刷新时重置，不拿上一个源的记录骗人。
- 空态：`还没有审计记录：手动拉取会记在这里。`；错误态有原文 + 重试；**不**用本地操作记录冒充。
- 凭据：「引用者」旁边「凭据历史」按钮 → 弹层（操作/时间/操作者三列 + 空态）；弹层挂在 `#page-main`
  之外（`.reveal` 的 transform 会破坏 `fixed` 定位），显隐靠 `hidden` ↔ `flex`（容器类不声明 `display`）。
- 真浏览器冒烟（Chromium headless + `cmd/fe-preview` + `web/mock.js`）：zh/en 两页 **0 JS 错误**；
  展开前 0 请求、展开后恰好 1 请求；错误态与重试、`{items:[]}` 空态各验一次；弹层开合正常。
- mock 侧补了两条只读路由（`/sources/{name}/audit`、`/secrets/{name}/audit`）。mock 没有凭据表也没有
  `kind=poll` 的种子源，所以 mock 下只能验证组件本身，真实数据要在真后端上看。

## 9. 不做 / 遗留风险

- 全局审计流（跨资源）与审计导出/归档：issue 明确不做。
- `config.secret` 老路径（安装/编辑源的副产物）**不写**审计，与 #554 语义一致；本次未改这条语义。
- `source_audit.source_id` 对 `sources` 是 `ON DELETE CASCADE`：删源会连它的审计一起消失。
  读端点因此只服务「活着的源」；要保留墓碑历史需要另立外键策略（本次不做）。
- `deleted` 审计行**按契约不可经端点观测**：handler 先解析实体再读审计，删除后实体已不存在 ⇒ 必然 404。
  这是 D4（不泄露存在性）的必然结果，e2e 用直读表补证三动作齐全；若将来要在 UI 上展示「已删除」，
  得先改隔离模型（例如按 id 而非 name 定位），不要在端点上开后门。
- 同一秒内落两行时 `ORDER BY created_at DESC, id DESC` 的 `id` 是随机 `sau_*`，谁在前不确定；
  断言刻意不依赖这个顺序（按 `detail` 集合比，只钉 `at` 单调）。
- `before` 的精度是秒：同一秒的记录不会被重复返回，但可能一并略过。要让游标精确到行需要
  把游标升级成 `(created_at, id)` 的合成令牌——那会改变 wire 形状，本次不做。
