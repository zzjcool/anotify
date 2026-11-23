# #554 命名空间凭据 CRUD —— 轮换不必动源，删除前查引用

<!-- snapshot-doc: #554 交付时的定格数字（make ci / e2e / 变异自证的实测输出），复算命令见 §5–§6 -->

分支 `pi-subagent/hw-nj/anotify-554-secrets-a892931b`（base `11ac6ccc`）。issue 的四个端点 / 六条语义 /
9 条验收逐条落地，**老路径（`config.secret` + `secretRef`）行为不变**，值只写不读。

## 1. 端点（`api/resource.yaml`，`{namespace}` 是 slug）

| method | path | 成功 | 失败 |
|---|---|---|---|
| GET | `/v1/namespaces/{namespace}/secrets` | 200 `{items:[{name,updatedAt,usedBy}]}`（非信封，空 `[]`） | 400/401/404 |
| GET | `/v1/namespaces/{namespace}/secrets/{name}` | 200 `{name,updatedAt,usedBy}` | 400/401/404（`secret_not_found`） |
| PUT | `/v1/namespaces/{namespace}/secrets/{name}` | 200 读投影（body `{value}`，只写） | 400（`value_invalid` + `causes[].field=name` / `unknown_field`）、401/404 |
| DELETE | `/v1/namespaces/{namespace}/secrets/{name}` | 204 | **409 `secret_in_use`**（`causes[].field=source`，message 含源名）、400/401/404 |

读投影**没有 `value` 字段**：凭据不是信封资源（没有 spec/status 两半），与 `/edges`、
`/capabilities/{name}/audit` 同类，所以不套四键信封。

## 2. 落点（谁在哪）

| 层 | 文件 | 内容 |
|---|---|---|
| store | `internal/store/namespace_credential.go` | `PutNamespaceSecret` / `DeleteNamespaceSecret` / `ListNamespaceSecrets` / `GetNamespaceSecretMeta` / `ListNamespaceSecretAudit`；`NamespaceSecretFact`（**无 Value**）、`NamespaceSecretInUseError` |
| store | `internal/store/schema.sql` + `migrate.go` | `-- +migration 9 secret_audit`：`namespace_secret_audit`（append-only，`action ∈ created/rotated/deleted`，只记 name/actor），`headVersion 8→9` |
| store | `internal/store/namespace_credential.go`（既有 `upsertNamespaceSecretOn`） | **值写入只有这一条 SQL**：新端点与老路径都调它 |
| spec 名规则 | `internal/sourcespec/poll.go` | 抽出 `ValidateSecretName`；`normalizeSecretRef` 的 name 分支改调它（错误文本逐字保留） |
| server | `internal/server/resource_credential.go`（新）+ `mux.go` 一行 caller | `newSecretAPI(d).register(mux)`；`namespaceSecretDoc/List/Write` 三个 DTO |
| server | `internal/server/api_status.go` | `*NamespaceSecretInUseError → 409 secret_in_use + causes`；`ErrNamespaceSecretNotFound → 404 secret_not_found` |
| parity | `internal/server/openapi_parity_test.go` | 三对 DTO；operation 143→147、mux pattern 132→136 快照同步 |
| e2e | `scripts/e2e/suites/namespace_credentials.mjs`（新）+ 三处套件登记 | 78 条断言 |
| UI | `web-src/pages/source-detail.html` + 4 份 locale | 「拉取」区块的凭据名 + 引用者（`usedBy`），无新页面 |

## 3. 「值不落盘外泄」的证据

值是**只在 `namespace_credentials.value` 一列**存在的字符串；所有出站面（响应/日志/审计/错误）都不含它。

1. **读投影类型上没有那个字段**：`namespaceSecretDoc` 只有 `name/updatedAt/usedBy`，
   `store.NamespaceSecretFact` 也没有 `Value` —— 想泄露得先改类型。
2. **响应/日志/审计三面 grep**：`internal/server/resource_credential_test.go::TestSecretResourcePlaintextNeverLeaks`
   跑完整流程（PUT 创建、PUT 轮换、列表、单条、非法名字 400、空值 400、建 `secretRef` 源、删除被拒 409、
   老路径建源），把**所有响应 body**、`slog` 全量日志（JSON handler，Debug 级）、`namespace_secret_audit`
   全表 dump 拼起来，逐个断言三个互不相同的明文值一次都不出现。
3. **e2e 黑盒再扫一遍**：`namespace_credentials.mjs` 的 G 段把本套件 36 份响应 body 拼起来 grep
   （3 个 `tok-e2e-…` 值）；反假绿断言要求池子里必须含凭据**名字**，所以「搜不到」不是池子为空的假通过。
4. **老路径的 config 也不含值**：`TestSecretResourceOldPathStillCreates` 断言源响应无明文、
   `config` 只留 `secretRef`；store 侧 `internal/store/source_test.go` 既有断言继续守。
5. **审计行本身没有值列**：`namespace_secret_audit` 只有 `name/action/actor_id`，写入函数不接受 value。

## 4. 9 条验收 → 证据

| # | 验收 | 证据（测试） |
|---|---|---|
| 1 | PUT 后 `secretRef` 源能跑出 `Authorization: Bearer …` | `TestSecretResourceRotationTakesEffectOnNextPoll`：**真 `Poller.Sweep`** 打真 `httptest` 上游，断言请求头 `Bearer token-one-31` |
| 2 | 轮换同名 → 下一轮用新值、旧值不再出现 | 同上第二轮：头 = `Bearer token-two-42`，并显式断言头里不含旧值；两轮 `LastPollAt` 必须不同（防「读到上一轮的值当成本轮完成」的假绿） |
| 3 | DELETE 被引用 → 409 + causes 含源名；换掉 ref 再删 → 204 | `TestSecretResourceDeleteGuardListsReferencingSources`（409 + causes + 凭据仍在 → PATCH 换 ref → 204 → GET 404 → 再删 404）；e2e C 段 18 条 |
| 4 | 全流程响应体/日志 grep 不到明文 | 上文 §3 的 2/3；变异 ② 打红 |
| 5 | 老路径 `config.secret`+`secretRef` 行为逐字不变 | `TestSecretResourceOldPathStillCreates`（201、响应无明文、读面可见、usedBy 指向它）+ `internal/store` 的 `TestNamespaceSecretLegacySourcePathUnchanged`（老路径不写资源面审计） |
| 6 | 非法名字 → 400 且 `field` 指到 name | `TestSecretResourceNameRuleSharedWithSecretRef`：同一批非法串在 `PUT …/secrets/{name}`（cause `field=name`）与 `config.secretRef`（cause `config.secretRef`）**都被拒** —— 证明同一份规则；e2e D 段 |
| 7 | 变异 ①DELETE 不查引用 ②GET 单条回显 value 必红 | §5 |
| 8 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | §6 |
| 9 | 五件套 + UI `usedBy` | api/resource.yaml ↔ parity（三对 DTO）↔ DTO（三个 Go 类型）↔ e2e（78 断言）↔ 单测（store 4 例 + server 6 例）；UI 见 §7 |

## 5. 变异自证（两条都打红，跑完立即还原）

**① DELETE 不查引用**（删掉 `secretRef` 使用检查这一段）：

```
--- FAIL: TestNamespaceSecretUsageAndDeleteGuard (0.06s)
    namespace_credential_crud_test.go:96: 被引用必须拒删: <nil>
--- FAIL: TestSecretResourceDeleteGuardListsReferencingSources (0.07s)
    resource_credential_test.go:267: 被引用时 DELETE 期望 409，得到 204：
```

**② GET 单条回显 value**（`get` 里多塞一个 `value` 键）：

```
--- FAIL: TestSecretResourceReadFaceHasNoValue (0.08s)
    resource_credential_test.go:223: 单条读回显了值或状态不对（200）：{"name":"gh-token","updatedAt":"…","usedBy":[],"value":"tok-first-9f1"}
--- FAIL: TestSecretResourcePlaintextNeverLeaks (0.04s)
    resource_credential_test.go:529: 响应体里出现了明文值 "plain-v2-4b8e"
```

两条都是「改被测逻辑 → 断言红 → `git checkout` 还原」，不是改断言。

## 6. 门禁数字

- `make ci`：**exit 0**（contract 静态断言 + OpenAPI exact-five 147 operations / 265 schema、gofmt、
  `make fe`（含 check-classes / check-wire-fields / check-i18n-keys / check-ui-copy / check-contrast）、
  vet、build、全量单测、bench 冒烟、harness 自测、e2e 机器级锁自测）。
- `make e2e-affected`：**exit 0 —— 31/31 套件、断言 3284 通过 / 0 失败、总墙钟 361.6s**
  （改动面含 `api/**` 与 `internal/store/schema.sql`，两者都在 `FULL_SURFACES` ⇒ 按表全量，不是漏跑）。
  新增套件单独跑：**78 通过 / 0 失败，0.5s**。
- known-env：`scripts/e2e/known-env-failures.json` **未改**。
- 注释密度棘轮：`commentLines 43297 → 43296`（**只降**），`internal/server 7765 → 7764`；
  为此回收了几处纯复述函数名的注释（`writeStatus`/`writeJSON`/`writeErr`、`User 是 users 表的一行`），
  并按实况修正了两条失真注释（「写入口只有 poll 的 config.secret」不再成立）。新套件的分段标签走
  `console.log`（不进注释预算），细节留在本报告里。同时修正 `internal/server/AGENTS.md` 里因此漂移的两个
  `path:line` 引用。
- `check-ui-copy` / `check-classes` / `check-i18n-keys`（4 份 locale 键集合一致）：`make fe` 内全绿。

## 7. UI（不加新页面）

源详情页「拉取」区块：`spec.config.secretRef.name` 存在时多一行事实 —— 凭据名（mono）+ 引用者
（`GET …/secrets/{name}` 的 `usedBy`），四态分开说：加载中 / 凭据不存在（404）/ 只有本源引用（`usedBy=[]`，
不是错误）/ 加载失败。名字取自**原始资源**：`model.source` 的 `config` 是剪掉 `secretRef` 的白名单投影
（`web/partials.js` 的 model 层），页面不能靠 `m.config.secretRef` —— 这是实现时发现的既有投影口径，
没有为此改 model 层。

## 8. 明确不做（与 issue 的「不做」一致）

凭据版本历史（轮换只留最新值）、凭据类型（SSH/证书）、`GET` 面回显值、`usedBy` 之外的反向索引、
把审计做成可查端点（审计只有写入 + store 读口，供测试与将来端点用）。

## 8b. 明确的取舍 / 遗留风险

- **`value` 上限 8 KiB**：新面拒超长值（`value_invalid`），老路径（`config.secret`）照旧不设上限 ——
  老路径「行为逐字不变」优先于两条面完全对称。超长值仍可经老路径写入（同一个存储列，读面无差异）。
- **列表不带 keyset 分页**：`GET …/secrets` 与 `/edges`、`…/audit` 同类（非信封派生投影），
  一个命名空间的凭据是个位数量级；等出现分页需求再照 #377 的口径加，不预先造游标。
- **`usedBy` 只在读时算**：没有反向索引（`sources.config` 是 JSON，引用关系不落表）。源数量也是
  个位/十位量级，`secretUsageOn` 一次扫本 ns 的源即可；真要上量再加索引表。
- **UI 的凭据名取自原始资源**（`spec.config.secretRef`），因为 `model.source` 的 config 是剪掉
  `secretRef` 的白名单投影。要在 model 层统一需要改 `web/partials.js` 的 modelSource，本轮没做。

## 9. 运行记录

```
$ timeout 900 ./scripts/e2e/run_all.sh namespace_credentials
  ✅ namespace_credentials 78通过/0失败  0.5s  mode=real
✅ E2E 全绿：1/1 个请求套件均达标（断言 78 通过 / 0 失败）

$ timeout 2400 make e2e-affected
  总耗时: 361.6s
  断言通过: 3284
  断言失败: 0
  套件级重试: 0 个套件
✅ E2E 全绿：31/31 个请求套件均达标（断言 3284 通过 / 0 失败，authority 清单覆盖 31 个）

$ timeout 1500 make ci
EXIT=0
```

`scripts/e2e/known-env-failures.json` 未改（known-env 空）。
