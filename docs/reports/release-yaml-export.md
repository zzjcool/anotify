<!-- snapshot-doc: 2026-11-22 #536 交付当次的门禁与断言数字（定格记录；复算命令逐条写在 §5） -->
> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

# 按 release 导出实例装出的对象 YAML + 详情页查看（#536）

as_of: 2026-11-22 · area: `server` / `store` / `playbook` / `web` / `e2e` · 迁移块不变（仍 `7 playbook_requires`）
起点 `main@ab0a4fc9`（含 #534 / PR #535）

## 0. 结论

1. **端点按 release 导出，集合只由回执决定。** 新增
   `GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}/yaml`，返回**多文档 YAML 正文**
   （`Content-Type: application/yaml; charset=utf-8`）。对象名单来自该 release 全部安装回执的
   `created.capabilities` / `created.sources` —— **不做名字前缀匹配**，也不从 `installedFrom` 反查：
   前者会把别人的对象算进「这个实例装出了什么」，后者回答不了「回执记过什么」。
2. **每份文档 = 单对象端点的同一份投影与脱敏。** 能力走 `artifact.MarshalYAML(capabilityArtifact(row))`
   （与 `GET …/capabilities/{name}/yaml` 同一函数），源走 `srcYAMLDocument`（与 `GET …/sources/{name}/yaml`
   同一函数）。没有第二套渲染、也没有第二套脱敏口径，所以「导出 vs 单对象」可以逐字节断言。
3. **顺序与错误面都是契约。** 能力按名字升序在前、源按名字升序在后（`playbook.ReleaseObjects` 唯一实现）；
   release 不存在 / 已卸载 → `404 release_not_found`（与 `DELETE …/releases/{release}` 同一错误面）；
   0 对象 → `200` 且 `objects: 0`（只有头注释）；只读、不写库、不产生回执。

## 1. 端点契约（写死在 `api/resource.yaml` 与头注释里）

```yaml
# playbook: project-chain@3  release: staging  values-digest: sha256:1a2b…
# install: ins_01H…  installed-at: 2026-11-22T09:11:24Z  objects: 3
# 版本说明：以下对象为「当前生效版本」（不是安装那一刻的快照）
---
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review
…
```

- 头注释取**最新一条回执**的安装身份（重装/升级后 install id 与时间会变，release 名不变）；
  `playbook@version` 取该次安装真正用到的版本（钉住安装时是那一版）。
- 文档顺序：能力升序 → 源升序；`---` 单独一行，正文以恰好一个换行收尾。
- 已卸载 = 对象已物理删除 → 与「release 不存在」同一个 `404 release_not_found`，而不是 200 的空导出。
- 回执记过、但对象已被删除的能力/源**不出现在导出里**（导出给的是当前生效版本，不编造），
  这一点写进了 spec 的 description。

## 2. 落点

| 文件 | 职责 |
| --- | --- |
| `api/resource.yaml` | 新 path item（`application/yaml` 的 200 + 400/401/404/500），文档里写明「响应体是 YAML 不是 JSON」 |
| `internal/playbook/export.go` | `ReleaseObjects`：只认回执 `created`、剥 `@version`、去重、两条升序（纯函数，可单测） |
| `internal/store/playbook_export.go` | `GetPlaybookReleaseExport`：按 (ns, slug, release) 读全部回执 + `Uninstalled`；release 归一化复用 `playbook.NormalizeRelease` |
| `internal/server/resource_playbook_release_yaml.go` | handler + `releaseYAMLBody` + `releaseYAMLText`（头注释与 `---` 拼接） |
| `internal/server/resource_playbook.go` | 路由注册一行（`GET …/releases/{release}/yaml`） |
| `internal/server/openapi_parity_test.go` | 快照 130→131 / 141→142；新增「非 JSON 成功响应」显式豁免表与守卫 |
| `internal/server/resource_playbook_release_yaml_test.go` | 6 条传输面验收（一致/确定性/不越界/脱敏/404/空态+只读） |
| `internal/playbook/export_test.go` | 集合纯函数（去重、剥版本、升序、空集合是 `[]`） |
| `web-src/pages/playbook.html` | 实例分组每条「查看 YAML」+ 抽屉（`pre` + 复制 + 空态/失败态） |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | `pb.v4.view_yaml` / `yaml_title` / `yaml_empty` / `yaml_load_failed` |
| `scripts/e2e/suites/playbook_release.mjs` | HTTP e2e：一致/确定性/不越界/脱敏/404/空态/只读 |
| `scripts/e2e/fixtures/f3/f3-playbook.mjs` | UI e2e：抽屉 3 段文档、secret 只以 `secretSet` 出现、0 对象空态 |

## 3. 五件套

- **api**：`api/resource.yaml` 新增 `exportPlaybookReleaseYAML`（`application/yaml` + inline `{type: string}`）。
  仓库里「非 JSON 成功响应」原本只有 5 条（节点二进制 / 产物下载 / 登录脚本 / 两个 qr.txt），本端点第 6 条。
- **parity**：YAML 正文没有可反射的 Go DTO，**DTO 字段 parity 无法覆盖它** —— 所以在
  `openapi_parity_test.go` 里新增 `nonJSONResponseOps` 表（Reason + Witness）与
  `TestOpenAPIParityNonJSONResponses`：① spec 里任何非 JSON 的 2xx **必须**登记（不登记即红）；
  ② 登记项必须在 spec 里仍然存在、仍然是非 JSON（改回 JSON 就报「已过期」）；③ 证人符号必须还在。
  这不是静默跳过：把本端点的 200 改回 `application/json` 或删掉这条登记，守卫都会红。
- **单测**：见 §2 两个测试文件（handler 6 条 + 纯函数 2 条）。
- **e2e**：`playbook_release`（HTTP）+ `frontend`（真 Chrome）。
- **门禁**：`make ci` / `make e2e-affected`（§5）。

## 4. UI

- 实例分组（按 release 折叠的 `.pb-group`）每条加「查看 YAML」按钮（`data-action="release-yaml"`）：
  打开右侧抽屉 `#pb-yaml-drawer`（`#page-main` 之外，复用 `openModal/closeModal` 的 Esc/背板语义），
  `.code-block > .code-head + pre.code-body` 显示全文，`#pb-yaml-copy` 复制（`copyText` 闭包取文本）。
- 裸 YAML **不能走 `api()`**（它只吃 JSON）：这一处用 `fetch(..., credentials:"same-origin")` + `res.text()`；
  401 与 `api()` 同一个落点（`Anotify.loginHref()`），失败读 JSON Status 显示 `reason · message`。
- 0 对象：分组里 `capabilities`/`sources` 都空 → 直接空态文案（`pb.v4.yaml_empty`），**不发请求**、
  不留空 `<pre>`；服务端返回 `objects: 0`（正文没有任何 `---`）时也落到同一空态。
- 文案四条 × 四语言（`check-i18n-keys` / `check-ui-copy` / `check-classes` 全绿）。

## 5. 验收（逐条命令与实测）

单测（Go）：

```text
$ PATH=/usr/local/go/bin:$PATH go test ./internal/server -run 'TestPlaybookReleaseYAML' -count=1
ok  	github.com/zzjcool/anotify/internal/server
$ PATH=/usr/local/go/bin:$PATH go test ./internal/playbook -run 'TestReleaseObjects' -count=1
ok  	github.com/zzjcool/anotify/internal/playbook
$ PATH=/usr/local/go/bin:$PATH go test ./internal/playbook ./internal/store ./internal/server -count=1
ok  	github.com/zzjcool/anotify/internal/playbook	0.5s
ok  	github.com/zzjcool/anotify/internal/store	25.2s
ok  	github.com/zzjcool/anotify/internal/server	75.5s
```

e2e（HTTP + 浏览器）：

```text
$ ./scripts/e2e/run_all.sh --serial playbook_release
  [playbook_release] 314 通过 / 0 失败
✅ E2E 全绿：1/1 个请求套件均达标（断言 314 通过 / 0 失败）
$ ./scripts/e2e/run_all.sh --serial frontend
  [frontend] 43 通过 / 0 失败
✅ E2E 全绿：1/1 个请求套件均达标（断言 43 通过 / 0 失败）
```

1. **一致**：`TestPlaybookReleaseYAMLMatchesSingleObjectEndpoints` —— 装一个 2 能力 + 1 源的包，
   导出恰好 3 段；每段与 `GET …/capabilities/{name}/yaml` / `GET …/sources/{name}/yaml` 的
   `yaml` 字段**逐字节相同**（同一份读投影）。e2e 同断言（`playbook_release` 的「能力文档与单对象端点逐字节相同」×3）。
2. **确定性**：`TestPlaybookReleaseYAMLDeterministicSorted` —— 同一 release 连续两次导出 sha256 相同；
   断言 `a-fixer < z-review < m-push`（包里能力列表顺序刻意是 z→a，与升序相反）。e2e：
   「同一 release 两次导出逐字节相同」+「顺序固定：能力升序再源」。
3. **不越界**：`TestPlaybookReleaseYAMLOnlyReceiptRefs` —— 先装另一个包，造出**同前缀**（`demo-`）的
   `demo-manual-review` / `demo-manual-push`，再装目标包；导出仍恰好 3 段，两个 foreign 名字一个字都不出现。
   e2e 同形（`pb-release-yaml-other`）。变异 ① 见 §6。
4. **脱敏**：`TestPlaybookReleaseYAMLMasksCredentials` —— webhook 源的 `config.secret` 原文不出现；
   只有 `hookUrl` / `secretSet: true`；`token` 只允许出现在 `hookUrl` 的路径段里（剥掉 `/v1/hooks/<token>`
   之后正文里不应再有 token）；不存在 `token:` / `secret:` 键。UI e2e 在抽屉 DOM 文本上重跑了同一批否定断言。
5. **404**：`TestPlaybookReleaseYAMLErrors` —— release 从未存在 → `404 release_not_found`；已卸载 → 同一 404；
   release 形状错 → `400 invalid_playbook`；包不存在 → `404 NotFound`。e2e 复算其中两条 + 卸载后 404。
6. **UI**：`frontend` 的 `#536 instance YAML drawer`：`---` 段数 = 3、`name: a-review` / `a-fixer` / `a-push`
   可见、能力整体在源之前、`secretSet: true`、不含 `config.secret` / `token:` / credential 原文、关闭后不可见；
   `#536 zero-object instance`：`#pb-yaml-empty` 可见且文案非空、没有 `<pre>`、没发 `/yaml` 请求。
7. **变异**：见 §6。
8. **门禁**：

```text
$ make ci                       # exit 0（contract + 全部静态守卫 + go build + make test + bench-dispatch + harness 自测）
$ make e2e-affected             # exit 0
  ✅ E2E 全绿：30/30 个请求套件均达标（断言 3028 通过 / 0 失败，authority 清单覆盖 30 个）
  最慢套件: frontend 200.2s · ui_shift 186.7s · capacity 148.4s
  注释密度：扫描 717 文件 · 注释行 43300 / 基线 43300（基线只下调：43330 → 43300）
  check-annotations：unbacked 297 / 基线 297，unmarked 209 / 基线 209
  known-env：scripts/e2e/known-env-failures.json 仍是 {"entries": []}
```

`make e2e-affected` 本次是**全量**（30 套件）：`api/**` 在 `FULL_SURFACES` 里，且 `internal/server`
属于 `cmd/server` 的二进制风险面 —— 选择器按 fail-closed 判全量，不是「只挑了几个套件」。

  注释密度：本 PR 新增了实现/测试注释，同批**回收**了历史流水账与重复说明（`openapi_parity_test.go`
  的递增史、`store.go` 的实测日志、`resource_capability.go` 的重复边界说明等），基线 **43330 → 43300 只降**。
9. **五件套**：见 §3。

## 6. 变异自证（真改源码跑出来的红，不是声称）

变异都只在工作区做，跑完立刻 `git checkout --` 还原（`git status` 干净）：

| 变异 | 改法 | 结果 |
| --- | --- | --- |
| ① 回执 refs → 名字前缀匹配 | `resource_playbook_release_yaml.go` 里把 `playbook.ReleaseObjects(exp.Receipts)` 换成「取首个引用名的第一段当前缀，扫全命名空间」 | `TestPlaybookReleaseYAMLOnlyReceiptRefs` **FAIL**：`导出集合只由回执决定，应恰好 3 个文档，got 5`（把 foreign 的 review/push 带了进来） |
| ② 去掉固定排序 | `internal/playbook/export.go` 的 `sort.Strings(out)` 换成降序 | `TestPlaybookReleaseYAMLDeterministicSorted` **FAIL**：`顺序必须是能力升序（a-fixer → z-review）再源：a=849 z=363 src=1330` |

两条都是先红后绿：还原后同一命令 PASS。①证明「不越界」不是靠数据巧合，②证明顺序是断言出来的契约。

## 7. 不做（本票明确不做，写在这里免得下一轮重问）

- **安装时快照**：回执目前只记名字（`created.capabilities` 是 `name@version`，但**没有对象正文/摘要**），
  所以「安装那一刻长什么样」在对象被升级/改过之后**无法还原**。本端点给的是**当前生效版本**，头注释
  第三行就是这句实话。要做真快照，得先给回执加 `{name, version, digest}`（甚至对象正文），属于另一颗票。
- 已卸载实例的历史导出：对象已物理删除；能力版本虽保留 pinned body，但回执没有摘要可指（同上一条的依赖）。
- npm/tar 之类打包形态的导出。
- 回执记过、但对象已被删除的引用**不进导出**（不编造；spec 里写明）。

## 8. 遗留风险 / 已知边界

1. **head 注释里的 `values-digest` 取最新回执**：同一 release 多次安装且 values 不同时，它是最后一次的摘要，
   不代表「全部对象都按这组 values 装」。回执里的 `values` 已打码，导出不带 values。
2. **能力按名字导出当前最高版本**：回执记的是 `name@version`，但单对象端点的读投影就是「最高可见版本」；
   导出沿用同一口径（否则「与单对象端点逐字节相同」不成立）。历史版本仍可走
   `GET …/capabilities/{name}/versions`。
3. **UI 的空态来自实例分组的 `capabilities`/`sources`（`installed_from` 反查）**：对象被删掉但回执仍记着时，
   界面会直接给空态而服务端会给「缺对象的导出」——两者都是「没有可展示的对象」，但口径来源不同。
4. **`loginHref()` 的页面白名单不含 `playbook`**：401 跳登录会落到 `login.html?next=index.html…`（与
   `api()` 在别的页面的行为一致，不是本 PR 引入）；要精确回跳需改 `web/partials.js` 的白名单，不在本票范围。
5. **非 JSON 成功响应的守卫是新增的**：它把既有 5 条也登记了；将来新增裸响应端点必须登记，否则红。
