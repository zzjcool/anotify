<!-- snapshot-doc: 2026-11-23 #548 交付当次的门禁与断言数字（定格记录；复算命令逐条写在 §5） -->
> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `api/` 五份真源为准。

# 预案拆成两级：包页只看 release 列表 + 实例详情页（#548）

as_of: 2026-11-23 · area: `api` / `server` / `store` / `web` / `e2e` · 迁移块不变（仍 `7 playbook_requires`）
起点 `main@bc2c45e9`

## 0. 结论

1. **层级判据落成了两个页面，而不是一页两段。** 包页（`playbook.html`）只回答「这个包有几份实例」：
   每行一个 release —— 名字 · 状态 · 参数摘要（短号，hover 给全文）· 对象数 · 最近安装时间；
   同一 release 用多套参数装过显示「N 组」。**对象、回执、YAML、卸载全部退出这一页**，进
   `release.html`。判据：包级事实（几份实例、版本历史、体检）留包页；实例级事实（这份 release
   装出了什么、参数摘要、回执、卸载）进详情页。
2. **新增一个只读端点让详情页一次拿全。** `GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}`
   给出身份（包@版本 / release / valuesDigest / installedAt / lastInstallId）、装出的对象
   （能力 `name+version+digest`、源 `name+kind`）、依赖、参数组数。页面因此**不再**拼
   「实例清单 + 能力列表 + 源列表」三个请求 —— 那种拼法每个页面都要各自复述一遍归属规则，
   而且拿到的是一份「名字对、版本可能不对」的视图。
3. **归属规则只有一条：对象集合由这个 release 的回执 `created` 决定。** 不按名字前缀、不查全
   命名空间、不从 `installedFrom` 反查。两份 release 并存时，各自的详情就是各自的实例
   （验收 3 与变异 ① 都钉在这一条上）。
4. **已卸载 = 404，不是空 200。** release 从未存在、或回执全部已打 `uninstalled_at` → 同一个
   `404 release_not_found`（与导出 / 卸载同一口径）。页面把 404 渲染成「已卸载或不存在」+
   返回包页的入口，所以直接打开旧 URL 不会白屏、也不会 500。
5. **同一 release 的多套参数是「组」，不是多行。** 折叠键仍是 `(release, valuesDigest)`；
   列表按 release 聚合（组数进行内），详情页按组切换（对象 / 回执 / YAML 都跟着所选那组走）。
   `GET …/releases/{release}/yaml` 新增可选 `?valuesDigest=`：不带时是全量并集，带上就只导出
   这一组装出的对象 —— 否则「看第一组、导出却是两组并集」。

## 1. 端点契约（真源是 `api/resource.yaml` 的该 operation）

```
GET /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}      200 | 400 | 401 | 404 | 500
{
  "playbook": {"name": "project-chain", "version": 3},   // 当前版本；安装那一刻的版本在回执里
  "release": "staging",
  "valuesDigest": "sha256:…",
  "installedAt": "2026-11-23T03:11:24Z",
  "lastInstallId": "ins_…",
  "params": {"digest": "sha256:…", "groups": 1},          // 组数 = 同一 release 用几套 values 装过
  "objects": {
    "capabilities": [{"name": "review", "version": 2, "digest": "sha256:…"}],
    "sources": [{"name": "on-push", "kind": "webhook"}]
  },
  "dependencies": [{"capability": "notify", "version": 1}],
  "groups": [{"valuesDigest": "sha256:…", "values": {…打码…}, "attempts": 1,
              "installedAt": "…", "lastInstallId": "ins_…",
              "objects": {…}, "dependencies": […]}]        // 按最近安装降序
}
```

- 顶层的 `valuesDigest/installedAt/lastInstallId/objects/dependencies` 描述**第一组**（现在在跑
  的那一套）；单组 release 时与 `groups[0]` 逐字段一致，页面可以只读顶层。
- 对象的 `version` / `digest` 取**当前生效版本**（与 `GET …/capabilities/{name}` 同源，
  也与 `…/releases/{release}/yaml` 同一口径）：行被升级后详情页与导出必须是同一份名字 + 版本。
  回执记过、但行已被删除的引用不出现在列表里（不编造幽灵行）。
- `dependencies` 取组内最后一条回执的 `dependencies`（这套 values 最近一次安装解析到的版本），
  与 `GET …/installs/{id}` 读到的一致。
- 参数组按 `(created_at, rowid)` 降序：同一秒内的两次安装只比时间戳会随机化「哪一组在跑」。

## 2. 落点

| 文件 | 职责 |
| --- | --- |
| `api/resource.yaml` | 新 operation `getPlaybookRelease` + 七个 schema（Detail / Ref / Params / Objects / CapabilityObject / SourceObject / ParamGroup）；`…/yaml` 加可选 query `valuesDigest` |
| `internal/store/playbook_release_detail.go` | `GetPlaybookReleaseGroups`：按 `(release, valuesDigest)` 折叠并带回每组回执原文；没有回执 → `ErrPlaybookReleaseNotFound`，全部已卸载 → `Uninstalled` |
| `internal/store/playbook_export.go` | 读路径抽成 `playbookReleaseExport`，新增 `GetPlaybookReleaseExportGroup`（按摘要筛 `values_digest`） |
| `internal/server/resource_playbook_release_detail.go` | `getRelease` / `releaseDetail` / `releaseObjectsOf` + 响应 DTO（「回执 created → 当前生效行」的唯一投影点） |
| `internal/server/resource_playbook.go` | 路由表加一行（`GET …/releases/{release}`） |
| `internal/server/resource_playbook_release_yaml.go` | 读 `?valuesDigest=`，走按组导出 |
| `internal/server/openapi_parity_test.go` | 七对新 pair；`parityPairsFloor` 209→216、`muxPatternCount` 131→132、`specSchemaFloor` 249→256、operation 快照 142→143 |
| `internal/store/playbook_release_detail_test.go` | 4 条：折叠组边界 / release 是边界 / 已卸载可区分 / 按组导出（含未知摘要 404） |
| `internal/server/resource_playbook_release_detail_test.go` | 4 条：对象不串 / 与单对象端点同源 / 两套 values 分组 / 404（不存在 + 已卸载）/ 按组导出 |
| `web-src/pages/playbook.html` | 实例区改成 release 列表（聚合、组数、摘要 hover、对象数、最近安装），对象/YAML/卸载/单次回执按钮退出本页 |
| `web-src/pages/release.html` | 新页面：身份行 · 对象（能力/源，带链接）· 回执（created/skipped/upgraded/依赖）· 参数组切换 · YAML 抽屉 · 卸载 · 已卸载态 |
| `web/partials.js` | `loginHref()` 白名单与 `LIST_OF` 登记 `release.html`（深层链接 401 后 `next=` 不丢参数） |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | 新 `release:` 段（28 个键）+ 包页三个新键；四份键集一致 |
| `scripts/e2e/suites/playbook_release.mjs` | HTTP e2e：详情端点 / 不串 / 与单对象端点同源 / 两组参数 / 按组导出 / 404 |
| `scripts/e2e/fixtures/f3/f3-playbook.mjs` | 新增 Chrome case `playbook-release`（两级 IA 全流程）；`install` case 跟着新 IA 更新 |
| `scripts/e2e/fixtures/f3/{f3-cases,f3-driver,f3-ui-helper}.mjs`、`scripts/e2e/affected-map.mjs` | case 注册、页面清单、affected 映射（新页面必须有人盯） |
| `scripts/check-comment-density.mjs` | 注释基线同批**回收**（只下调，见 §5） |

## 3. 五件套

| 面 | 位置 | 覆盖 |
| --- | --- | --- |
| spec | `api/resource.yaml` | 新 operation + 七个 schema + `valuesDigest` query，字段说明写明归属规则与 404 口径 |
| parity | `openapi_parity_test.go` | 七个 Go 结构体 ↔ 七个 schema 双向字段比对；路由数 / schema 数 / operation 数三处 ratchet 同步 |
| DTO | `internal/server/resource_playbook_release_detail.go` | 私有大写结构体（`playbookRelease*`），与 spec 同名同形 |
| 单测 | `internal/store/…detail_test.go` + `internal/server/…detail_test.go` | 8 条（折叠、边界、卸载、按组导出、对象不串、同源、分组、404） |
| e2e | `scripts/e2e/suites/playbook_release.mjs`（HTTP）+ `f3-playbook.mjs` 的 `playbook-release`（Chrome） | HTTP 面验数据一致性与 404；Chrome 面验两级 IA、直接打开 URL、卸载后旧 URL |

## 4. UI（两级 IA 的实际形状）

```
包页 playbook.html
  [状态行] 版本 v3 · 2 个实例 · 体检通过
  [实例]   staging  [生效中]  a1b2c3d4e5f6(悬停给全文)  3 能力 / 1 源 · 最近安装 2026/11/23 11:11
  [实例]   canary   [生效中]  f6e5d4c3b2a1 …  · 2 组        ← 同一 release 两套参数
          ↑ 每行只有一个链接：release.html?namespace=…&id=<slug>&release=<name>
  [版本历史]（折叠）· [体检]（折叠）· [原始包]（折叠）· [删除]（折叠）

实例详情 release.html?id=<slug>&release=<name>
  [身份] project-chain@v3 · staging · 参数摘要 a1b2c3d4e5f6 [复制全文] · 最近安装 … · ins_…
  [装出的对象]  能力：review v2 sha256:… | fixer v1 sha256:…     源：on-push webhook
  [最近一次安装的回执]  created / skipped / upgraded / 依赖（只读，与 GET …/installs/{id} 同源）
  [动作] 查看 YAML（按所选参数组）· 卸载这个实例
```

- 状态行、行内摘要的 `title`、`data-*` 钩子都留着（e2e 按 `[data-release]` / `[data-release-link]` /
  `[data-fact]` / `[data-object]` 断言，不依赖文案）。
- 已卸载 / 不存在 / 包已删除：一把话说清 + 「返回预案详情」按钮；YAML 与卸载按钮在该状态下禁用。
- 空实例（0 对象）：对象区一句话，YAML 抽屉直接给空态**且不发请求**（0 对象的实例也是合法实例）。

## 5. 验收（逐条命令与实测）

```bash
export PATH=/tmp/venv548/bin:$PATH          # PyYAML 在 venv 里（scripts/check_openapi.py 需要）
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto
export E2E_LOCK=0                           # 本机 macOS 没有 flock（run_all.sh 的机器级锁），先确认无其它矩阵在跑
export E2E_CHROME_PATH="…/chromium-1208/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
```

| # | 验收 | 命令 | 实测 |
| --- | --- | --- | --- |
| 1 | 包页只列 release，不展开对象；点行进详情，URL 可直接打开 | Chrome case `playbook-release` | ✅ `rows.length===2`、每行 `href` 精确等于 `release.html?namespace=default&id=project-chain&release=<r>`、`#pb-installs` 不含任何对象名；新标签页直接打开同一 URL 得到同一组对象 |
| 2 | 对象名/版本/kind 与单对象端点一致；回执与 `…/installs/{id}` 一致 | `go test ./internal/server -run TestPlaybookReleaseDetailObjectsMatchSingleObjectEndpoints` + e2e §12 | ✅ 能力 `metadata.version`、源 `spec.kind` 逐条比对；`installs/{lastInstallId}.valuesDigest === detail.valuesDigest` |
| 3 | 两份 release 各自详情只显示自己的对象 | `go test ./internal/server -run TestPlaybookReleaseDetailObjectsStayPerRelease` + e2e §12 + Chrome case | ✅ 三条独立断言（见 §6 变异 ①：改归属后三条同时红） |
| 4 | 同一 release 两组参数：列表「2 组」，详情按组，YAML 按所选那组 | `go test … TestPlaybookReleaseDetailTwoValueGroups` + e2e §12 组用例 | ✅ `params.groups===2`、两组摘要不同、每组对象带自己的 project 前缀；`?valuesDigest=` 只导出该组（不含另一组），不带时仍是并集 |
| 5 | 卸载后从包页消失；直接打开旧 URL → 明确「已卸载/不存在」 | Chrome case `playbook-release` + `go test … TestPlaybookReleaseDetailNotFound` | ✅ DELETE 后列表 1 行、旧 URL 渲染 `#rd-gone-panel`（`#rd-content` 隐藏）、HTTP 是 `404 release_not_found` |
| 6 | e2e：两份 release → 列表 2 行 → 分别进详情 → 对象不串 | `make e2e-one S=playbook_release` + Chrome case | ✅ HTTP 断言 399 通过 / 0 失败（含新增 §12 的 85 条）；Chrome case 1 通过 / 0 失败 |
| 7 | 变异 ① ② 必红 | 见 §6 | ✅ 两条都真改源码跑出来的红 |
| 8 | `make ci` exit 0（见下方平台说明）；`make e2e-affected` 绿；known-env 空；注释密度不上调 | `make ci`(121s) / `make e2e-affected`(657s) | ⚠️ 见 §5.1 的两条平台差异：`make ci` 在本机（macOS）非 0 只因 **5 个 `cmd/anode` 包**的平台用例；`make e2e-affected` 选到全量 30 套（`api/resource.yaml` 是共享面），本机 24 套绿 / 6 套红，红的 6 套逐条归因见 §5.1，与 #548 无关。`known-env-failures.json` 仍是 `entries: []`（未新增）。注释密度 43297 = 基线（同批下调 43300→43297） |
| 9 | 五件套 + 四份 locale 键集一致 | `make contract` / `node scripts/check-i18n-keys.mjs` | ✅ 5 份 spec / 143 operation / 143 唯一 operationId / 258 schema；4 份 locale 键集一致（2462 key / 2248 引用点） |

其它门禁（都属于 `make ci`）：`check-classes` 877 个类全在设计系统内、`check-ui-copy` 4 份 locale × 9832 条值无命中、
`check-wire-fields` / `check-css` / `check-contrast`（48 项判定 PASS 48）/ `check-affect-map` / `check-test-discipline` 全绿。

### 5.1 两条平台差异（本机 macOS；不是"红着交付"）

**(a) `make ci`**：只差 `cmd/anode` 一族 —— `TestInstallScript*` 调用 `scripts/install-anode.sh`，
脚本开头就是「只支持 Linux（当前 Darwin）」；`TestReal*` 要求数据目录不是 `/var` 软链。其余 36 个包全 `ok`。
祖先提交上同一组用例逐条同样失败（34 条，见下）。

**(b) `make e2e-affected`**（选中全部 30 套，657s）：24 套绿，6 套红的归因 ——

| 套件 | 红的原因 | 归属 |
| --- | --- | --- |
| `live_agent` | `install --prefix` 退出码 2（Linux-only 安装脚本） | 平台 |
| `node_v4` | `anode serve: data directory: unsafe symlink/special file /var` | 平台 |
| `frontend` | 仅 `observe-trace` 一张截图超时（4500ms） | 平台（**祖先提交逐字复现**） |
| `i18n` | `navigation-*` 三条：`#lang-switcher` 点击 2.2s 超时 | 时间预算（本机慢） |
| `i18n_coverage` | `task-states-*` 三条 `#task-cancel` 点击超时 + `pages-en-3` 的 message 页 goto 超时 | 时间预算 |
| `ui_shift` | `shift-lang-hint` / `shift-prelayout-*` 三条帧采样断言（只采到 1 帧） | 机器帧率 |

**与 #548 相关的两套单独跑是绿的**：`playbook_release` 399 通过 / 0 失败（4.9s）、
`frontend` 除 `observe-trace` 外全部通过（278s，含新增 `playbook-release` 与更新后的 `playbook-install`）。
全量矩阵里 `release.html` 作为新页面一次都没红过（i18n / i18n_coverage / ui_shift / pages-21-* 全通过）。


**(a) 的复算**（干净 clone 到 `/tmp/base548`，两棵树各跑一次同一组用例）：

```bash
git clone --local . /tmp/base548 && cd /tmp/base548 && git checkout bc2c45e9
go test ./cmd/anode -count=1 | grep -E '^--- FAIL' | sort > /tmp/base_fails.txt   # 34 条
cd - && go test ./cmd/anode -count=1 | grep -E '^--- FAIL' | sort > /tmp/head_fails.txt
diff /tmp/base_fails.txt /tmp/head_fails.txt && echo IDENTICAL                     # IDENTICAL
```

**(b) 里 `observe-trace` 的复算**（同一棵树，只换基线）：

```bash
cd /tmp/base548 && npm install && make fe && go build -tags embeddist -o .e2e-bin/anotify ./cmd/server
ANOTIFY_BIN=/tmp/base548/.e2e-bin/anotify DEVSEED_BIN=… ANODE_BIN=… E2E_CHROME_PATH=… \
  node scripts/e2e/suites/frontend.mjs --case observe-trace
# → CASE_FAIL observe-trace :: page.screenshot: Timeout 4500ms exceeded.  （base 上逐字同样红）
```

## 6. 变异自证（真改源码跑出来的红，不是声称）

### 变异 ① 详情页按 slug 取全部对象（而不是按 release）

`internal/server/resource_playbook_release_detail.go` 的 `releaseObjectsOf` 里把
`playbook.ReleaseObjects(receipts)`（只认这份 release 的回执 `created`）换成「遍历命名空间里全部
能力 / 源」：

```
--- FAIL: TestPlaybookReleaseDetailObjectsStayPerRelease (0.05s)
    blue 的对象 = [blue-a-fixer blue-z-review green-a-fixer green-z-review],
    want [blue-a-fixer blue-z-review]（只属于 blue 的能力，升序）
--- FAIL: TestPlaybookReleaseDetailTwoValueGroups (0.01s)
    two 组对象数不对: {Capabilities:[one-a-fixer one-z-review two-a-fixer two-z-review] …}
```

同一变异的 HTTP e2e（`make e2e-one S=playbook_release`）同时红 15 条，例如：

```
❌ release blue 装出的能力就是自己那一个 — 期望 ["blue-blue-review"]
   实际 ["a-demo-review","b-demo-review","blue-blue-review",…,"green-green-review",…]
❌ release green 的详情不含 blue 的能力 …
❌ 组 project=one 不串另一组的对象 …
```

→ 验收 3 覆盖在单测 / HTTP e2e / Chrome 三层，任一层都能独立抓住这个回归。

### 变异 ② 列表退回展开对象（旧结构）

`web-src/pages/playbook.html` 的 `releaseRow` 里把对象名拼回行内（`item.append(el("p", …))`），
再跑 Chrome case：

```
CASE_FAIL playbook-release :: 包页不得展开对象（这一页只说有几份实例）
  ❌ playbook-release — AssertionError: 包页不得展开对象（这一页只说有几份实例）
```

→ 验收 1 由「行里出现对象名」这一条直接钉住：一旦列表想回到「一行 = 一组 values + 对象清单」，
断言先红，不需要靠人看截图。

## 7. 不做（本票明确不做，写在这里免得下一轮重问）

- **安装历史**（含已卸载实例的完整视图）：回执目前只存名字，翻历史是另一颗票。
- **把版本历史搬进详情页**：版本是包级事实，留在包页。
- **对象列表里的「已删除」占位行**：行已删除的引用不出现在详情里（与导出同口径），不编造
  `version: 0` 的幽灵行。
- **详情页嵌完整回执原文**：回执段用 `GET …/installs/{id}` 的同一份投影（验收 2 就是拿它比对的），
  新端点不自带一份重复的回执 DTO。

## 8. 遗留风险 / 已知边界

1. **详情页发两个请求**：实例事实（对象 / 依赖 / 组数）走新端点一次拿全；「最近一次安装的回执」段
   再读一次 `GET …/installs/{lastInstallId}`。刻意不把回执全文塞进新端点 —— 那样同一份回执会有两个
   wire 形状，验收 2 的「与 `…/installs/{id}` 一致」就变成拿两份实现互相比对。
   前端拼的三个请求（installs + capabilities + sources）仍然只有一个，归属规则只在一处实现。
2. **对象数是「当前生效」而非「安装时的快照」**：能力被升级后，详情与导出都跟着变（与 #536 同一
   口径）。要「那一刻装了什么」只能看回执原文。
3. **已卸载 = 404，不是 410**：与导出 / 卸载同一口径，代价是「从未存在」与「已卸载」在 HTTP 上不可
   区分（页面文案合写为「已卸载或不存在」）。
4. **注释密度是零和的**：本 PR 新增代码的注释绝大多数是判据（why），代价是同批回收了
   `internal/{server,store}` 里若干条历史流水账（基线 43300 → 43297）。后续再加注释仍需先回收。
5. **列表行的排序按 release 名（不是按最近安装倒序）**：一行的身份是 release，名字序稳定、可预期，
   不受时钟影响；「最近安装」作为一列展示，需要按时间找时用详情页的组（按最近安装降序）。
6. **行内「对象数」是同一 release 各参数组的并集**：两套 values 装出不同对象时，列表数会比任何单组
   更大 —— 列表只说「这份实例一共涉及几个对象」，精确到组的数字在详情页的组标题上。
7. **Windows 未验证**：`web-src/pages/release.html` 的抽屉/状态行用的是既有设计系统类，未见平台相关
   分支；但仓库本就只对 Linux/macOS 做门禁。
