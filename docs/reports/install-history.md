# 安装历史（含已卸载）：列表 / 详情带 `?include=uninstalled`，已卸载仍可看安装时快照（#553）

<!-- snapshot-doc: 2026-11-23 #553 交付当次的门禁与断言数字（定格记录；复算命令逐条写在 §5） -->

> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

as_of: 2026-11-23 · area: `api` / `store` / `server` / `web` / `e2e` · 迁移块不变（仍 `8 install_pinned`）
起点 `main@11ac6ccc`（#552 的安装时快照已合入）

## 0. 结论

`#551` 之后卸载过的实例**数据上查得回来**（回执 `created.pinned` + 软删的能力行 + `?as=installed`），
但界面上看不见：列表 `WHERE uninstalled_at = 0`、详情 404、包页只列生效中的 —— 审计链断在最后一步。
本票把最后一步接上，且**默认行为一个字节都没改**：

| # | 冻结决策 | 落地事实 |
| --- | --- | --- |
| D1 | 列表 `?include=uninstalled` 连带已卸载；折叠键不变 | `store.ListPlaybookInstallsWithUninstalled`（默认仍走 `ListPlaybookInstalls`，8 处既有调用点一行未改）；整组都已卸载的组带 `uninstalledAt` |
| D2 | 已卸载行多 `uninstalledAt` + `lastInstallId` | `lastInstallId` 本来就有（组内最新回执）；`uninstalledAt` 取组内最大 `uninstalled_at`，**只有整组已卸载**才出现（卸载后又重装同一套 values 的组是「现在在跑」的） |
| D3 | 已卸载组的对象引用如实为空 | 不再反查 `installed_from`：软删的能力行不该算作「这份实例现在装出了什么」 |
| D4 | 详情 `?include=uninstalled` → 200，带 `uninstalledAt` / `objectsGone: true` / `pinned` | `getRelease` 只在显式参数下放行；`objects` 与每组 `objects` 强制为空（同名对象被别的实例重装也不漏进来） |
| D5 | `pinned` 与 `?as=installed` **同口径** | `releasePinnedRefs`：同一 `(kind,name)` 后者覆盖前者，能力按 name 升序在前、源在后；老回执 → 空切片（只读视图**不** 409） |
| D6 | `include` 只认一个值 | 其它值 → `400 value_invalid`（与 `as` 同口径，不做大小写模糊匹配） |
| D7 | 生效中实例的列表 / 详情 / YAML 逐字节不变 | 三个新键都 `omitempty` 且只在已卸载视图填值；测试与 e2e 都做了「带参 vs 缺省」的逐字节对照 |

一句话：**装过又卸载的实例现在「看得见、说得清」**——列表能翻出来，详情能读到卸载时刻与安装时快照，
YAML 能导出当时那一版；生效中的实例连响应字节都没变。

## 1. 端点契约（真源是 `api/resource.yaml` 的两个 operation）

### 1.1 `GET /v1/namespaces/{namespace}/playbooks/{slug}/installs`

| 请求 | 行为 |
| --- | --- |
| 缺省 | **逐字节不变**：只列生效中的回执，组里没有 `uninstalledAt` 键 |
| `?include=uninstalled` | 连带已卸载的回执；整组已卸载的组多 `uninstalledAt`（RFC3339），`capabilities`/`sources` 为 `[]` |
| `?include=其他` | `400 value_invalid` |

### 1.2 `GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}`

| 请求 | 行为 |
| --- | --- |
| 缺省 | **不变**：已卸载 → `404 release_not_found`；生效中 → 今天的响应（与带参逐字相同） |
| `?include=uninstalled` + 已卸载 | `200`，多三个键：`uninstalledAt`、`objectsGone: true`、`pinned: [{kind,name,version,digest} \| {kind,name,kindOf}]`；`objects` 与每组 `objects` 都是 `[]` |
| `?include=uninstalled` + 生效中 | 与缺省响应**逐字节相同**（三个新键都不出现） |
| `?include=其他` | `400 value_invalid` |
| release 从未存在 / 包已删除 | 仍 `404` |

### 1.3 YAML（行为不变，只是 UI 默认切过去了）

`…/yaml?as=installed` 对已卸载仍 `200` 且头注释带 installId（#551 已实现）；缺省与 `as=current` 仍 `404`。

## 2. 落点

| 面 | 文件 | 改动 |
| --- | --- | --- |
| 读数 | `internal/store/playbook.go` | `PlaybookInstallGroup.uninstalledAt`；`ListPlaybookInstallsWithUninstalled` + 共用的 `listPlaybookInstalls(include bool)`；已卸载组不反查对象 |
| 读数 | `internal/store/playbook_release_detail.go` | `PlaybookReleaseGroups.UninstalledAt`（release 级卸载时刻）+ `Receipts`（扁平回执，供合并快照）+ 回执行的 `uninstalledAt` |
| 端点 | `internal/server/resource_playbook.go` | `includeUninstalledQuery`（`include` 唯一合法值 `uninstalled`）+ `listInstalls` 选读数 |
| 端点 | `internal/server/resource_playbook_release_detail.go` | `getRelease` 的放行口径；`releaseDetail` 的已卸载分支（强制空对象 + `objectsGone` + `pinned`）；`releasePinnedRefs` |
| 真源 | `api/resource.yaml` | 两个 operation 的 `include` 参数 + `PlaybookInstallGroup.uninstalledAt` + `PlaybookReleaseDetail` 三个新属性 + 描述 |
| UI | `web-src/pages/playbook.html` | `#pb-installs-head` 里的 `#pb-show-uninstalled`（`.switch`）驱动 query；行聚合出 `uninstalled`/`uninstalledAt`；已卸载行只有「查看 YAML」；空态两种文案 |
| UI | `web-src/pages/release.html` | 详情请求恒带 `?include=uninstalled`；`objectsGone` 视图（`#rd-notice` 说明 + 对象空态 + 卸载按钮 hidden + YAML 默认「安装时」/「当前生效」禁用）；`&yaml=` 深链 |
| i18n | `web-src/locales/{zh-CN,en,ja,es}.yaml` | 各 +7 键（键集一致），`release.gone_*` 文案不再提「已卸载」 |
| 单测 | `internal/server/playbook_install_history_test.go` | 传输面 2 条（列表两态 + 详情可读/对象不泄漏/对照组逐字相等） |
| 单测 | `internal/store/v4test/playbook_install_history_test.go` | store 读数 1 条（默认不含 / 历史含 / 对象为空 / 分组读数带卸载时刻） |
| e2e | `scripts/e2e/suites/playbook_release.mjs` | 第 14 段 `partInstallHistory`（48 条断言） |
| e2e | `scripts/e2e/fixtures/f3/f3-playbook.mjs` + `f3-cases.mjs` | `playbook-release` case：已卸载视图 + 开关两态 + 两种空态；404 声明改口径 |

## 3. 五件套

| 件 | 落点 | 证据 |
| --- | --- | --- |
| API 真源 | `api/resource.yaml`（2 个 operation + 3 个 schema 属性 + 1 个组属性） | `make contract` exit 0（5 份 spec / 143 operation / 259 schema） |
| parity | `internal/server/openapi_parity_test.go`：两个 DTO 早已登记（`store.PlaybookInstallGroup` / `playbookReleaseDetail`），新字段双向自动对账；`include` 是字面量读法，query 双向同样自动覆盖 | `ok internal/server`（`TestOpenAPIParity*` 全绿，floor 未动：217 对 / 257 schema） |
| DTO | `store.PlaybookInstallGroup.UninstalledAt`、`store.PlaybookReleaseGroups.{UninstalledAt,Receipts}`、`playbookReleaseDetail.{UninstalledAt,Pinned,ObjectsGone}` | 单测逐字段断言（含「键不存在」而不是空串） |
| 单测 | server 2 条 + store 1 条（见 §2） | `go test ./internal/server/ ./internal/store/... ./internal/playbook/ -count=1` 全绿 |
| e2e | `partInstallHistory` + `playbook-release` case | `[playbook_release] 504 通过 / 0 失败`（基线 456）；`[frontend] 1 通过 / 0 失败` |

## 4. UI

包页（`playbook.html`）实例卡片头多一个开关，`.switch` 是 `web/ui.css` 里既有组件（此前零引用，本轮首次使用，没有新造类）：

- 关（默认）→ `GET …/installs`（**不带** query）；开 → `GET …/installs?include=uninstalled`。
- 已卸载行：`badge badge-warn` +「已卸载 · 时间」、`data-uninstalled="1"`、**没有** `[data-release-uninstall]`、
  保留 `[data-release-yaml]`（深链到 `&yaml=installed`）；对象数与「最近安装」不再显示（对象已删）。
- 生效中行：`badge badge-success` +「生效中」、对象数 / 最近安装照旧，另有「卸载」按钮（confirm 后 DELETE 并重拉）。
- 状态行的实例数只数**生效中**的 release。
- 空态分两种：从来没有实例 → 原文案；只剩已卸载 → 「只剩已卸载的实例；打开「显示已卸载」可以查看它们。」
  （默认视图 0 行时补一次 `?include=uninstalled` 探测，只读、不写库）。

详情页（`release.html`）拿到 `objectsGone: true` 时是「已卸载视图」，不是死局面板：

- 顶部 `#rd-notice[data-uninstalled-notice]`：「这份实例已于 … 卸载，对象已删除；下面是安装时的快照。」
- 对象区空态文案 `release.objects_gone`（与「这份实例还没有装出任何对象」区分开）；回执区照常可读。
- `#rd-uninstall` 直接 **hidden**（不是 disabled）：已卸载的实例没有「再卸载一次」这个动作。
- YAML 抽屉：默认「安装时」，「当前生效」`disabled` + `aria-disabled` 并给出原因；页内卸载成功后自动落到这个视图。
- 列表里的「查看 YAML」用 `release.html?…&yaml=installed` 深链，落地即开抽屉并选中「安装时」。
- `gone()` 只剩三个真实用途：缺参数、从没存在过、包已删除（文案不再说「已卸载」）。

## 5. 验收（逐条命令与实测）

| # | 验收（issue 原文） | 命令 | 实测 |
| --- | --- | --- | --- |
| 1 | 装 → 卸载 → 列表默认**不含**它；`?include=uninstalled` 含它且 `uninstalledAt` 有值 | `go test ./internal/server/ -run TestPlaybookInstallHistoryIncludesUninstalled -v`；`./scripts/e2e/run_all.sh playbook_release` | PASS（卸载前两组都没有 `uninstalledAt` **键**；卸载后默认 `total=1` 只剩 live；带参 `total=2`，gone 组 `uninstalledAt` 可 `time.Parse(time.RFC3339)`、`lastInstallId` = 那次卸载的 `installIds[0]`、`capabilities`/`sources` 都是 `[]`） |
| 2 | 已卸载详情：默认 404；`?include=uninstalled` 200 且 `objectsGone: true`、`pinned` 非空 | `go test ./internal/server/ -run TestPlaybookReleaseDetailUninstalledReadable -v` | PASS（默认 404 `release_not_found`；带参 200：`objectsGone=true`、`uninstalledAt` RFC3339、`pinned` = 2 能力（带 version/digest）+ 1 源（只带 kindOf）、`objects` 与 `groups[].objects` 全空） |
| 3 | 从「查看 YAML（安装时）」能下到 `?as=installed`（200，含 installId） | 同上 + Chrome case | PASS（`…/yaml?as=installed` 200 且含 `install: ins_…`；缺省视图仍 404。Chrome case 里抽屉自动停在「安装时」，`#rd-yaml-text` 匹配 `/install: ins_/`） |
| 4 | UI：开关切到「显示已卸载」出现该行；进详情看到说明与安装时 YAML；「卸载」按钮不存在 | `node scripts/e2e/suites/frontend.mjs --case playbook-release` | PASS（`[frontend] 1 通过 / 0 失败`：开关 `aria-checked` 两态 + `role=switch`、blue 行 `data-uninstalled=1` 且 `[data-release-uninstall]` 计数 0、green 行计数 1；`#rd-uninstall` `isVisible()===false`；`#rd-gone-panel` 计数 0；两种空态文案互不相同且与运行时字典逐字一致） |
| 5 | 生效中实例的行为逐字不变（列表、详情、YAML 默认视图都有对照组） | 上面两条 Go 测试 + e2e | PASS（详情：`?include=uninstalled` 与缺省响应 `JSON.stringify` 全等；列表：生效中组没有 `uninstalledAt` 键；YAML：默认视图仍不带 `as=installed`（Chrome case 断言 `calls()` 里没有该参数）） |
| 6 | 变异 ①②③ 必红 | 见 §6 | 三条都真红（贴了原文） |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | `timeout 1500 make ci` / `E2E_CONCURRENCY=1 timeout 2400 make e2e-affected` / `jq '.entries' scripts/e2e/known-env-failures.json` | `make ci` **exit 0**（`go test ./...` 全包 ok；契约/parity/fe/各项守卫全绿；注释密度 43297 = 基线 43297、75 目录 0 不一致）；`make e2e-affected` 见 §5.1；known-env `entries: []` 未动 |

### 5.1 `make e2e-affected` 实测

```
$ E2E_CONCURRENCY=1 timeout 2400 make e2e-affected
  ✅ playbook_release   504通过/0失败  6.4s   mode=real
  ✅ frontend           46通过/0失败  192.0s  mode=real
  …
✅ E2E 全绿：30/30 个请求套件均达标（断言 3243 通过 / 0 失败，authority 清单覆盖 30 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 652.6s）
AFFECTED_EXIT=0        （总耗时 652.6s）
```

## 6. 变异自证（真改源码跑出来的红，不是声称）

| 变异 | 改法 | 变红 |
| --- | --- | --- |
| ① 列表忽略 `include` | `internal/server/resource_playbook.go`：`if withGone {` → `if withGone && false {` | 验收 1：`TestPlaybookInstallHistoryIncludesUninstalled` → `历史清单应是 live + gone 两组：[{Release:live …}]`；同一变异的 HTTP e2e 红 6 条，逐条贴了 `❌ include=uninstalled 时已卸载的 gone 与生效中的 live 都在 — 期望 ["gone","live"] 实际 ["live"]`、`❌ 已卸载的组带 uninstalledAt，且是能 Date.parse 的 RFC3339 — undefined` → `[playbook_release] 498 通过 / 6 失败` |
| ② 已卸载详情不带 `objectsGone` | `internal/server/resource_playbook_release_detail.go`：`doc.ObjectsGone = true` → `= false` | 验收 2：`TestPlaybookReleaseDetailUninstalledReadable` → `已卸载视图必须带 objectsGone: true：{…"uninstalledAt":"2026-11-23T12:38:33Z","pinned":[…]}`（对象确实空、但没有那句话）；e2e → `❌ 已卸载详情带 objectsGone: true` → `[playbook_release] 502 通过 / 2 失败` |
| ③ UI 给已卸载行渲染卸载按钮 | `web-src/pages/playbook.html`：`if (!row.uninstalled) {` → `{`（无条件渲染） | 验收 4：`CASE_FAIL playbook-release :: #553 已卸载行**不得**渲染卸载按钮（否则点了就是个 404）` → `[frontend] 0 通过 / 1 失败` |

三次都是**真改文件 → `make fe` → 重建二进制 → 跑 → 贴输出 → `git checkout` 复原 → 复绿**。
（③ 的复算注意：f3 case 走 `.e2e-bin` 里的二进制，改前端后必须 `make fe` **并且** `go build -tags embeddist`
重出二进制，否则跑的是上一版页面 —— 我第一次没重建，看到的是「通知区等不到」的假现场。）

## 7. 不做（本票明确不做，写在这里免得下一轮重问）

- **恢复 / 重放已卸载实例**：那是「从快照重装」，另一颗。
- **把已卸载实例计入配额 / 健康统计**：本票只把它们从读面放出来，`status` 的计数口径没动（UI 也只数生效中的）。
- **给已卸载行保留对象列表**：对象已删，`objects` 如实为空 + `objectsGone` 明说「不是没装过」；要「当时装了什么」看 `pinned` 或 YAML 的安装时视图。
- **`include` 的其它取值**（`all` / `deleted`）：只有一个合法值，别的 400，不猜。

## 8. 遗留风险 / 已知边界

1. **`pinned` 可能不出现**：安装发生在 #551 之前（回执没有快照行）时，已卸载详情不给 `pinned`（`omitempty`）——
   `objectsGone: true` 仍然在，调用方据此知道「装过、对象没了、但当时没记版本」。导出侧同样场景是 `409 no_snapshot`；
   详情是只读视图，选择「不编造」而不是报错。
2. **同名对象被别的实例重装**：已卸载视图的 `objects` 强制为空，不按名字前缀反查 —— 这是刻意的（否则 blue 卸载后
   green 装出同名对象，blue 的详情会「复活」出对象）。`pinned` 与 YAML 的安装时视图按回执取，也不受影响。
3. **组粒度 vs release 粒度**：列表的 `uninstalledAt` 是**折叠组**的（同一 release 用两套 values 装过是两组）；
   「卸载后又重装同一套 values」的组算生效中（`uninstalled_at` 是逐回执的，整组都已卸载才给时刻）。
4. **`include` 大小写敏感**：`include=Uninstalled` → `400 value_invalid`（不走模糊匹配）。
5. **注释密度买预算**：新增说明靠压缩同批既有冗长注释换行数，总量回到 43297 = 基线（`check-comment-density`
   双向棘轮，`--update` 一次都没跑）。
6. **e2e 前必须重建前端产物与二进制**：`run_all.sh` 会先比 `dist` stamp（陈旧直接红）；f3 case 单独跑时
   必须自己 `make fe` + `go build -tags embeddist -o .e2e-bin/anotify ./cmd/server`（§6 第 ③ 条踩过）。
7. **矩阵类套件在负载下会 false red**：本机 `E2E_CONCURRENCY=1` 跑完整矩阵（见 §5.1）；known-env 保持 `[]` ——
   那是「GitHub runner 的环境性失败」登记表，本地负载抖动不是它的适用对象。
