# worker 报告 · 能力详情「近期调用记录（observed）」接真实数据（前端 + e2e + mock）

分支：`feat/capability-observed`（按任务要求：**未 commit / 未 push / 未切分支 / 未开 MR**，改动留在工作树）
基线：`main @ b90bf5ef`；本报告只涉及前端面（Go / api 未动，工作树里已有的 Go 改动是服务端 worker 的既有产出）

## 1. 改动文件清单（我的 9 个文件）

| 文件 | 改了什么 |
| --- | --- |
| `web-src/pages/capability.html` | `[data-sec="observed"]` 区块接真实数据：`{}` 空态 / `taskId`+`tasks.total`+`tasks.failed` / `emits[]`（to+fields+✓✗）/ `pruned[]`（to+reason+detail）/ 50 条上限截断提示 / 24h 窗口说明 |
| `web-src/locales/zh-CN.yaml` | 新增 `caps.observed.truncated`、`caps.observed.window`；重写 `caps.observed.unavailable` 文案（见 §2.4） |
| `web-src/locales/en.yaml` | 同上 |
| `web-src/locales/ja.yaml` | 同上 |
| `web-src/locales/es.yaml` | 同上 |
| `web/mock.js` | `capabilityStatus.observed` 对齐真后端：无最近任务 → `{}`；`emits[].ok` 由回执判定（镜像 `store.observedEmitOK`）；`tasks.total/failed` 只统计近 24h。**数据路由未改** |
| `scripts/e2e/fixtures/f3/f3-capability.mjs` | 新增「真实投递 → 等 `status.observed.taskId` → 详情页 DOM」断言 + 空态反面对照；把两条「真实权威 observed 恒 {}」的**过期断言**改成对账读模型（更强，不是放宽） |
| `scripts/v4-frontend.test.mjs` | 新增 1 条 mock 用例：`taskId`/24h 计数/emits(ok ✓✗)/pruned(read-only detail 走 `span[data-raw-content]`)/50 条上限/`{}` 空态 |

未动：`internal/`、`api/`、`Makefile`、`web/ui.css`、`web/partials.js`（`model.capability` 本来就原样投影 `status.observed`，无需改）。

## 2. 语义与取舍

### 2.1 四种情况（页面）

| 服务端 `status.observed` | 页面 |
| --- | --- |
| `{}`（这个能力从没跑过） | 空态 `caps.observed.unavailable`；**不渲染 0 计数**（0 是编造的事实） |
| `{taskId,tasks:{total,failed}}` | 「最近任务：tsk_…」「任务总数：N」「失败：M」+ 窗口说明 |
| `emits[]` 非空 | 每行 `to` + `fields`（`caps.observed.fields` 模板）+ `ok` 标记（`ok===false` → `✗`+`cap-missing`） |
| `pruned[]` 非空 | 每行 `to` + `reason` + `detail`（detail 是服务端原文，套 `span[data-raw-content]`） |
| `emits/pruned` 到 50 条 | 追加一行 `caps.observed.truncated`（`{n}` = `OBSERVED_LIST_LIMIT` 常量），不假装是全部 |

### 2.2 为什么加 `caps.observed.window`（小新增，理由是「不误导」）

`tasks.total/failed` 是**近 24h** 窗口（`store.ObservedWindow`）。原页面把它标成「任务总数」，等于把窗口数字说成全部历史——与仓库「不杜撰事实」的底线冲突。所以加了一行说明词条（4 语言），并用测试钉住（e2e + mock 单测都断言它必须出现）。不想保留的话是一行回退（页面 1 行 + 4 份 locale 各 1 条 + 两处断言）。

### 2.3 `caps.observed.unavailable` 文案重写（关键取舍，请 review）

旧文案「服务端暂未提供能力维度的运行记录」在服务端已实现 observed 之后就是**假话**（服务端确实提供了，只是这个能力没跑过）。改成「这个能力还没有运行记录（跑过一次就会出现）」/ en / ja / es 同步。
- 只有 `f3-capability.mjs` 断言过旧文案（已随之改成断言运行时字典里的新词条，不硬编码中文）；
- 词条 key 未变，`check-i18n-keys` 4 份键集合仍一致。

### 2.4 `web/mock.js` 的三处对齐（都是「预览要说和真后端同样的话」）

1. **无最近任务 → `{}`**：原来给 `{taskId:null, tasks:{total:0,failed:0}}`。页面 `hasObserved` 判断 `total != null` ⇒ 0 也算「有数据」，预览会把「没数据」画成 0 计数，与真后端（`capabilityObservedStatus` 直接返回 `{}`）不一致；
2. **`ok` 由回执判定**：原来写死 `ok:true`。现在 `ok: !e.pruned && Boolean(e.taskId)`（镜像 `store.observedEmitOK`）——mock 种子里那条 `evt_mock_3`（`taskId:null` + `pruned`）现在正确显示 `✗`；
3. **24h 窗口**：`tasks.total/failed` 只统计 `createdAt >= now-24h` 的任务行（种子里全部在窗口内，所以既有断言值不变）。

数据路由 / 种子事实一律未改（`in/out` 形状与 mock 的 `deliver`/`planDispatch` 无关）。

### 2.5 e2e 为什么这样造数据

- 造事实走**产品入口**：`POST /v1/namespaces/default/sources/deploy-hook/fire`（真实权威是同一个 handler，mock 权威是同一条 `MockApi` 路由）→ 源的 target `fixer` 建出真任务；
- 期望值**只来自读模型**：`GET …/capabilities/fixer?includeBody=true` 的 `status.observed`，轮询 `taskId` 非空（15s 超时，不靠固定 sleep）；
- DOM 断言与读模型逐字对账：`taskId` 出现在 `[data-sec="observed"]`；计数必须和**它自己的标签**连在一起（`任务总数：2`，不是「页面里碰巧有个 2」）；窗口说明必须在；空态文案必须不在；
- 反面对照：本用例刚建、**从没跑过**的 `f2a-demo` → 读模型必须是 `{}`（`assert.deepEqual(…, {})`），页面必须是空态且**不得**出现 `任务总数：`/`最近任务：`。
- 真实权威这 52s 窗口里 fixer 有真任务但没有 `emits/pruned`（那需要 grant→emit→剪枝链：在线节点 + 部署 digest）。按任务给的兜底条款，emits/pruned/50 条上限的数据态由 `v4-frontend.test.mjs` 的 mock 用例覆盖（该套件与真后端同形状、同 `ok` 判定）。

## 3. 新增/改写的测试

| 类型 | 名称 |
| --- | --- |
| e2e（real 权威） | `F3-CAP-10 observed：真实投递后详情页显示最近任务与 24h 计数（与读模型逐字一致），没跑过的能力保持诚实空态` |
| e2e（改写，变强） | `F3-CAP-09`（fixer：原「真实权威恒 {} 空态」→ 对账读模型）、`F3-CAP-12`（prod/report.send：同上 + mock 分支仍钉 `param_missing`） |
| 单测（mock） | `R1 A2d capability detail observed: taskId/24h 计数/emits(ok)/pruned/50 条上限/{} 空态` |
| 单测（硬编码断言） | 既有 `mock: capability 列表…observed 明细` 的 `tasks={total:1,failed:0}`、`prod-deploy.observed.pruned[0].reason=param_missing` 未改，仍绿 |

### 3.1 非空断言自证（mutation，已回滚）

1. 把 mock 的 `ok` 改回写死 `ok: true` → 单测红：
   `AssertionError: 剪枝回执（没建出任务）必须标 ✗： run-tests 字段：suite ✓`
2. 把页面的截断阈值改成 `>= 999` → 单测红：
   `AssertionError: 到服务端 50 条上限必须给出截断提示（新增词条 caps.observed.truncated）`
3. 两处还原后同一条用例绿（见 §4 第 6 项）。

e2e 的真任务证据（real 权威截图 `…/capability-actions/…/10-observed-real-task.png`）：
`最近任务：tsk_8CXZgrrEcXxetHf6t 任务总数：2 失败：0 / 计数是近 24 小时的窗口`（2 = base fixture 投递 1 + 本用例投递 1）。

## 4. 门禁输出（原始）

```
### 1) make fe
✅ tailwind.css 覆盖 931 个源码类名（组件类已排除）
✅ 图库访问守卫：23 个页面模板都只走 Anotify.graphLib()
✅ sitegen done: 4 languages × 20 pages → web (version v0.2.0-554-gb90bf5ef-dirty)
✅ 指纹完成：30 个资源，80 个 HTML → /root/code/anotify/internal/server/dist
✅ 已写入 internal/server/dist/.source-stamp：输入面 66 个文件：文本 51 / 二进制 15（已计入 sha256）/ 超大跳过 0

### 2) node scripts/check-classes.mjs
✅ class 校验通过：847 个类（HTML class 属性 619 + JS class 串新增 228）全部落在设计系统 / Tailwind 工具类内

### 3) node scripts/check-i18n-keys.mjs
解析 29 个源文件 / 4 份 locale / 2165 个 key / 1965 个 key 引用点（覆盖 1397 个不同 key；另有 56 处动态引用、0 处非扁平字面量）
✅ i18n 键守卫：4 份 locale 键集合一致，1965 个引用点全部有词条（768 个未被引用的 key 仅告警）

### 4) ./scripts/e2e/run_all.sh --serial frontend
    ✅ capability-actions (13 behavior assertions)
  [frontend] 36 通过 / 0 失败
  ✅ 套件 frontend 通过
  断言通过: 36
  断言失败: 0
  验收 authority: real 1 / explicit-mock 0
  ✅ frontend           36通过/0失败  136.8s  mode=real
✅ E2E 全绿：1/1 个请求套件均达标（断言 36 通过 / 0 失败，authority 清单覆盖 1 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 138.4s）

### 5) ./scripts/e2e/run_all.sh i18n_coverage
  AGGREGATE {…"passed":40,"failed":0,"unrun":0,"pageAttempted":80,"pagePassed":80,"pageFailed":0,"mode":"explicit-mock"…}
  [i18n_coverage] 40 通过 / 0 失败
  ✅ 套件 i18n_coverage 通过
  ✅ i18n_coverage      40通过/0失败  59.2s  mode=explicit-mock
✅ E2E 全绿：1/1 个请求套件均达标（断言 40 通过 / 0 失败）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 60.9s）

### 6) node --test scripts/v4-frontend.test.mjs
ℹ tests 217
ℹ pass 208
ℹ fail 9
（9 条 = 基线那 9 条，逐名一致：F2b2 controllers / F2b2 trace-index / F2c2 / R1 graph clicks /
  TRACE-ARCHIVE / TRACE-STARTUP ×4；新增用例 R1 A2d 绿，计数从基线 216 → 217）
```

## 5. 发现的问题（不算我的改动范围，交协调者裁决）

1. **既有 e2e 断言随服务端实现过期（已在本任务内修）**：`f3-capability.mjs` 的 check 9/11 原本断言「真实权威 observed 恒 `{}` → 页面必须显示『服务端暂未提供』」。服务端实现后这条变成**反断言**（真实权威现在有 taskId），全量 e2e 会红。我把它改成「对账读模型」（更强），没有删除覆盖。
2. **`caps.observed.empty` 是一条死词条**：locale 里有 `caps.observed.empty: '没有保留历史（清理后 observed 为空，不杜撰时间）'`，页面从未引用（`--allow-unreferenced` 之外只在 unreferenced 告警里）。这是历史遗留，未清理（不在授权范围，删词条要同步四份 + 计数下限）。
3. **`scripts/v4-frontend.test.mjs:954` 的模型单测仍用旧 `tasks: {"task-1": 2}` 形状**：它断言的是 `model.capability` 的**原样投影**（`observedOf` 只做 `obj()` 透传），所以照旧绿；但它不代表 wire 形状（wire 是 `{total,failed}`）。未改（避免把模型契约测试改成另一种断言），仅记录。

## 6. 遗留风险

- **50 条上限只在 mock 单测里覆盖**：真实权威要造 50 条 emits 需要一条真 grant→emit 循环，超出用例窗口。页面阈值常量 `OBSERVED_LIST_LIMIT = 50` 与 `store.ObservedEmitLimit/ObservedPruneLimit` 是**两份手抄常量**：服务端改上限时页面不会自动跟随（注释已互相点名）。真正的解法是把上限放进 wire（服务端已回 `emits` 长度，页面只能按 `>= 50` 判），或在 read model 里给 `truncated: true`——那是契约变更，不在本任务授权内。
- **空态文案改了**（§2.3）：如果有其它文档/测试引用旧中文（仓内已 grep 过，仅 `f3-capability.mjs` 与旧 `.reports/*.md` 提到，后者是历史报告，未动）。
- **mock 的 24h 窗口用 `Date.now()`**：如果有人在 mock 种子里放 >24h 前的任务，计数会小于总任务数（这正是真后端行为）；当前种子全部在窗口内，既有断言值不变。
- 未 commit / 未 push：按任务要求交付在工作树，MR 未创建。
