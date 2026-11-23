> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

# 预案页拆两级：包页 release 列表 + release 详情页（#548）—— 交付报告

- Issue: [#548](https://github.com/zzjcool/anotify/issues/548)
- 分支: `pi-subagent/host-workspace/anotify-548-release-page-f1190c8f`（基线 `main bc2c45e9`）
- 改动面: 新只读端点（server + api spec + parity/DTO）+ 包页实例区改造 + 新 `release.html` + 四份 locale + 预览 mock + e2e

## 0. 结论

1. **层级按「包级 / 实例级」切开。** 包页留下的都是包级事实：状态行、安装/升级、**release 列表**、版本历史、体检、模板、删除。
   实例级事实（这份 release 装了什么、参数摘要、回执、导出 YAML、卸载）全部进新的 `release.html`。
2. **一次 GET 拿全。** 新端点 `GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}` 一次返回
   身份四件 + 对象（能力 name/version/digest、源 name/kind）+ 依赖 + 参数分组与本组明细 + **最新一次回执原文**。
   详情页只发这一发请求：不分别拉 `capabilities` / `sources` / `installs`。
3. **对象归属只认回执 id**（`installed_from ∈ 该 release 的回执 id`），不按名字前缀、不看 slug。
   这是「两份 release 不串」的唯一判据，也是变异 ① 的落点。
4. **已卸载 = 404 = 一个明确的页面状态**：直接打开旧 URL 得到「已卸载或从未安装」的空态块（`#rel-gone` + 返回包页），
   不是白屏、不是空壳、也不是 500。
5. 新页进了四语言 × 两主题 × 两视口的 ui/i18n 矩阵；预览 mock 同步实现了该端点（否则 mock 权威套件会撞 501）。

## 1. 新端点

`GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}`（注册在 `internal/server/resource_playbook.go` 的 routes 表，
与既有 `DELETE …/releases/{release}`、`GET …/releases/{release}/yaml` 同一 path item 的第三个 method）。

响应（`internal/server/resource_playbook_release.go`，读模型，不套四键信封）：

| 字段 | 口径 |
| --- | --- |
| `release` | 实例身份（URL 里的 `{release}`） |
| `valuesDigest` / `installedAt` / `lastInstallId` / `receipt` | **最新一条回执**（`GetPlaybookReleaseExport` 按 created_at 升序的最后一条） |
| `objects.capabilities[]` | `{name, version, digest}`，**当前生效版本**（与 `/yaml` 端点同口径），按 (name, version) 升序 |
| `objects.sources[]` | `{name, kind}`，按 name 升序 |
| `dependencies[]` | `{capability, version}`，全部回执去重取**最高 version**，按 capability 升序 |
| `params` | `{digest, count}`：`count` = 不同 `valuesDigest` 的组数（同 digest 装多次算 1 组） |
| `groups[]` | 按参数分组的明细（最近安装的组在前；`groups[0]` 的三件身份与顶层一致），每组 `objects` 只含该组回执名下的行 |

- 404：release **从未存在**或**已卸载** → `404 release_not_found`（复用 `store.ErrPlaybookReleaseNotFound` → `api_status.go` 既有 case，未新增 reason 字符串）。
  包不存在 → `NotFound`；release 形状错 → 400 `invalid_playbook`。
- 只读：读路径 0 写（单测用库指纹前后比对守着）。
- 空列表恒 `[]`（`objects.capabilities` / `objects.sources` / `dependencies` / `groups`）；`groups` 恒 ≥1 条。

契约五件套：`api/resource.yaml` 的 `getPlaybookRelease`（description 里逐条写死上面 6 个口径）+ 6 个 schema
（`PlaybookRelease` / `…Objects` / `…Capability` / `…Source` / `…Params` / `…Group`，`dependencies` 复用 `PlaybookDependency`、
`receipt` 复用 `PlaybookReceipt`）↔ `openapi_parity_test.go` 的 `parityPairs()` 六条双向字段对账（冻结数字同步上移）
↔ `internal/server` 的 DTO ↔ e2e（HTTP 段 + chrome 段）↔ 单测（6 个测试）。

## 2. 页面

### 2.1 包页 `web-src/pages/playbook.html`

实例区从「按 (release, valuesDigest) 折叠的分组 + 每组的成员/回执/YAML/卸载按钮」改成**一行一个 release**：

```
release 名（连到详情） · 生效中徽章 · 参数摘要短号（title 给全文） · 对象数（3 能力 / 1 源） · N 组（多于一组时） · 最近安装时间
```

- 数据源**仍是** `GET …/installs`（不为列表新增请求）：按 `release` 聚合并集 `capabilities`/`sources`，
  「N 组」= 不同 `valuesDigest` 的去重数，`lastAt` 取各组最大。
- 行是 `<a href="release.html?namespace=<ns>&id=<slug>&release=<name>">`，新标签页打开结果一致。
- 每行**零按钮、零 `[data-action]`**（YAML / 回执 / 卸载全部搬走）。空态：「还没有实例。装一次会产生第一份。」
- 卸载回跳带 `?uninstalled=<release>`，包页显示「已卸载」提示并把参数从地址栏抹掉。

### 2.2 详情页 `web-src/pages/release.html`（新）

`/release.html?namespace=<ns>&id=<slug>&release=<name>`：可直接打开、刷新不丢、可分享。

| 区块 | 内容 |
| --- | --- |
| 身份 | `包名@版本`（`receipt.playbook`）· release · 参数摘要**全文可复制** · 安装时间 · installId · 返回包页 |
| 对象 | 能力（name + `@version` + 链到能力详情）、源（name + kind + 链到源详情）；`params.count > 1` 时**按参数分组**渲染，每组自己的对象 + 自己的 YAML 入口 |
| 回执 | `created` / `skipped` / `upgraded` / `dependencies`（只读；来自同一次 GET 的 `receipt`） |
| 动作 | 「查看 YAML」走 `GET …/releases/{release}/yaml`（抽屉交互复用）；「卸载这个实例」走 `DELETE …/releases/{release}`，成功后回包页 |

404（已卸载或从未安装）→ `#rel-state` 里的 `.empty` 空态块（`#rel-gone` + 返回包页），动作按钮置灰；
其它错误走共享的 `loadErrorNode`。`#rel-state` 三态常驻且有几何，正好当矩阵锚点。

### 2.3 四份 locale / 预览 mock / 页面登记

- `web-src/locales/{zh-CN,en,ja,es}.yaml` 各 +29 条（`rel.*` 24 条 + `pb.v4.*` 5 条），键集四份一致；改值 2 条。
- `web/mock.js` 补上该端点的 mock 实现（对象归属同样只认 `installed_from`）；不补则 i18n / i18n_coverage / ui_shift 会撞 501 兜底。
- `web/partials.js` 的 `LIST_OF`（release → playbook）与 `loginHref()` 白名单、`login.html` 的 `NEXT_MAP` 同步登记，深链登录后不丢参数。

## 3. 验收（9 条逐条）

| # | 验收 | 证据 |
| --- | --- | --- |
| 1 | 包页只列 release，点行进详情且 URL 可独立打开 | `release-detail` chrome 用例：`a[data-release]` 恰好 2 行、行内 `button` 计数 0、`[data-action]` 计数 0、href 逐字节等于 `release.html?namespace=default&id=<pkg>&release=a|b`；`page.goto` 同一 URL 结果一致（用例里就是直接跳转后断言的） |
| 2 | 对象名/版本/kind 与 capabilities/sources 端点一致；回执字段与 `…/installs/{id}` 一致 | HTTP 段逐字段比对（能力 version/digest、源 kind 与单对象端点同源，均取当前生效行）；`receipt` 与 `GET …/installs/{lastInstallId}` 做**双向 JSON 深比较**（单测 + HTTP 段各一份），另有一条反向测试证明取的是最新回执不是第一条 |
| 3 | 两份 release 各自只显示自己的对象（不串） | 三层机检：单测 `TestPlaybookReleaseDetailIsolatesReleases`（含同前缀邻居包）、HTTP 段（a 的响应里没有 b-、b 的响应里没有 a-）、chrome 段（a 详情页文本不含 `b-review`/`b-fixer`/`b-push`，反之亦然；YAML 抽屉同样分） |
| 4 | 同 release 两组参数：列表显示「2 组」，详情按参数分组，YAML 按所选那组 | 单测 `…GroupsByValues`（`params.count=2`、`groups` 两条、最近组在前、`attempts`、顶层是并集）；列表行 `pb.v4.release_groups_n`；详情页 `groupBlock` 每组自己的对象与 YAML 入口。（`/yaml` 端点按 release 返回当前生效版本、不接受 digest 参数，所以文案不承诺「按组过滤」——这一点写进了契约） |
| 5 | 卸载后从包页消失；旧 URL 给明确状态 | chrome 段：卸载 b → 包页只剩 1 行 + 「已卸载」提示 → `page.goto` 原 query → `#rel-gone` 可见、`#rel-state` 文案是「已卸载或从未安装」、`data-release` 已摘除、两个动作 disabled、无裸 key、该读请求是 **404（不是 500）** |
| 6 | e2e：两份 release → 列表 2 行 → 分别进详情断言不串 | `release-detail`（chrome，42 条断言）与 `playbook_release` 第 12 段（49 个断言点） |
| 7 | 变异 2 条必红 | 见 §4 |
| 8 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调基线 | 见 §5 |
| 9 | 五件套 + 四份 locale 键集一致 | 见 §1 末段与 §5 的 `check-i18n-keys` / parity 输出 |

## 4. 变异（各一次，跑完即 revert，`git diff` 证明干净）

| # | 变异 | 结果 |
| --- | --- | --- |
| ① | 详情端点把「按 release 的回执 id 过滤」改成「整个命名空间的对象」 | 单测 `TestPlaybookReleaseDetailIsolatesReleases` 红（`blue 的能力集合串了：[blue-demo-review demo-manual-review green-demo-review]`）；HTTP 段 12 条断言红；chrome 段 `release-detail` 红：**`#548 验收 3：a 的详情里不得出现 b 的对象 b-review`** |
| ② | 包页行退回旧结构（行内展开对象 + 每行的 YAML/卸载按钮） | chrome 段 `release-detail` 红：**`#548：列表行内不得有任何按钮（YAML/回执/卸载都搬去详情页）`**，`playbook-install` 同步红 |

两条都是「验收断言直接打红」，不是靠别的套件连带失败。

## 5. 门禁数字

### 5.1 `make ci`

`exit 0`。关键子项：

- `check-i18n-keys`：4 份 locale 键集一致，2242 个引用点全部有词条。
- `check-ui-copy`：4 份 locale / 9812 条值 · 23 个页面 / 242 个文本节点（实现词面 102 · 散文面 78）· 21 条规则。
- `check-classes` / `check-tailwind` / `check-contrast` / `check-css`：全绿（新页只用组件类 + 页内 `<style>` 定义的 `.rel-*`）。
- `check-comment-density`：**注释行 43300 / 基线 43300**，75 个目录 0 个与基线不一致（`web-src/pages` 恰好 2400）。
- `check-annotations`：unbacked **297 / 297**、unmarked **209 / 209**。
- `make contract`：A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对通过；`TestOpenAPIParity` 冻结数字同步
  （mux pattern 131→132、operation 142→143、schema 249→254、parityPairs 209→215）。
- `go test ./...`（shuffle）全绿。

> 注释预算的处理方式：新端点/新测试的说明压到极简（文件头只留「口径真源在 api schema description」的指路），
> 差额按同目录既有散文注释等量压缩，**没有上调任何基线**（棘轮是双向等值的，`--update` 也拒绝抬高）。

### 5.2 `make e2e-affected`

```
✅ E2E 全绿：30/30 个请求套件均达标（断言 3108 通过 / 0 失败，authority 清单覆盖 30 个）
```

逐套件（本次改动把页面、fixture 与 Go 都摸到，选择器直接选中全部 30 套）：

```
playbook_release 369 · frontend 46 · ui_shift 87 · i18n 33 · i18n_coverage 44
routing 425 · contract 185 · events_paging 179 · ws_protocol 134 · cli_auth 133
security 100 · vars_ui 96 · lang_hint 86 · passkey_enroll 85 · capacity 84
persistence 82 · api_key_login 78 · admin_flow 52 · deeplink 50 · auth_flow 49
p2_guards 42 · recovery_next 16 · ux_regression_19_23 8 · …
```

`known-env-failures.json` 保持 `entries: []`；无 case/timeout 重试。

### 5.3 单独跑页面直接对应的 HTTP 面套件

```
./scripts/e2e/run_all.sh playbook_release → 369通过/0失败 · mode=real · 6.3s
```

（#521 时该套件是 225 条；本轮 +144 条，其中第 12 段新增 49 个断言点。）

## 6. 没做（issue 明确排除）

- 安装历史（含已卸载实例的完整视图）—— 回执目前只存名字。
- 版本历史搬进详情页 —— 版本是包级事实，留在包页。
