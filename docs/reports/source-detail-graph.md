# 源详情页链路图（source-detail.html）

状态：实现完成，验收数字见 §5~§7（`make ci` / `make e2e-affected` 的数字在本 PR 里实测填入）。

## 1. 为什么图丢了（考古，一行结论）

链路图原本挂在 `web-src/pages/source.html` 上（`.source-graph` 容器 + `render()` 里
`await A.graphLib()` + `{{define "graph-libs"}}`），该页在 **`85de629c`**
（"批量清审计债（A-1…F-2 共 37 条）+ 实现词守卫 + 下线重复源页"）的 **B-4** 项被整页删除、
playbook 的引用入口改指 `source-detail.html`；而 `source-detail.html` 历史上从未有过图 ——
所以「图没了」不是渲染坏了，是**承载它的那一页被删了，图没有搬过去**。

## 2. 图落在哪（容器 / id / class）

| 项 | 值 | 为什么 |
| --- | --- | --- |
| 容器 class | `.source-graph` | 沿用旧源页的类语义：`min-height:320px; width:100%; min-width:0`（只定尺寸，外观全归 flograph） |
| 容器 id | `#src-detail-graph` | **新增**的 id。既有 DOM id 一个都没动（`#src-detail` / `#src-detail-name` / `#src-detail-body` / `#src-detail-state` / `#src-detail-recent` … 全保持原样） |
| 区块 | `[data-sec="graph"]` | 与页内其它区块同一套 `section()` 构件，新值不覆盖任何既有 `data-sec` |
| 位置 | 「目标能力」之后、「近期回执」之前 | 图讲的是 targets 往下的连接，紧挨 targets 读得通 |
| 标题 / 提示 | `sources.v4.graph` / `sources.v4.graph_hint` | **复用** locale 里已有的词条（旧源页的 copy，本次没有新增任何 key） |

## 3. 数据来源和执行路径

- 取数：`A.loadSourceTopology(name, { sources: [res] })` —— 三页（首页/链路页/源详情页）共用的唯一入口；
  本页手上已有源资源，所以只省掉那一次 sources 请求，能力表仍由它自己拉（`?limit=500` 整份）。
- 推导：`AnotifyGraphs.sourceTopology` —— 入口边来自 `source.spec.targets`，下游来自
  `capability.spec.next` 的展开（`graph.js` 里唯一一份推导，页面不自算）。
- 渲染：`G.capabilityFlow(host, …)` —— 与链路页同一个 silk 渲染器（条/丝带/回放/图例/全屏）。
  预览语义（`preview: true`）：只说配置引用，不说执行成功。
- 取库：`await A.graphLib(...)` —— 图库是 defer 加载的，**不许**同步探 `window.AnotifyGraphs`
  （那就是 TRACE-STARTUP / SOURCE-STARTUP 的老事故）。解析期补了 `A.expectGraphics()`，
  页面重启 `{{define "graph-libs"}}` 槽位（`/flograph.js` + `/graphs.js`，顺序不能反）。

## 4. 降级行为

图库缺失（或拓扑取不到）时，**只把图这一块**换成一行说明：

- 文案：`sources.flow.nolib`（locale 里已有，四份语言齐全）—— 不新造句子；
- 实现：`host.replaceChildren(<p class="field-hint">…)`，`catch` 覆盖 `graphLib()` 抛出的
  `graph_unavailable` 与 `loadSourceTopology` 的 null/异常；
- 其余区块不受影响：页头、目标能力、近期回执、原始 YAML 照常渲染（e2e 有断言）。

## 5. e2e：锁住「图存在且有节点」

新增 f3 case `source-detail-graph`（`scripts/e2e/fixtures/f3/f3-source-graph.mjs`，
注册在 `f3-cases.mjs`，跑在 `frontend` 套件里）：

夹具用真实 API 造一条 3 能力链（先叶子后上游）：`graph-claim.spec.next = [graph-dispatch, graph-gate]`，
外加一个 `manual` 源 `graph-chain`，`spec.targets = [graph-claim]`。

断言（`frontend` 套件里 real 权威跑过；同一 case 在 `E2E_MOCK_ONLY=1` 的 explicit-mock 权威下也单独跑过一轮，两条都 PASS）：

```
PASS source-detail-graph :: SOURCE-GRAPH 图上有源 + 3 个能力（4 节点 / 3 条边：
     源→graph-claim、graph-claim→graph-dispatch/gate）
PASS source-detail-graph :: SOURCE-GRAPH-NOLIB 图库缺失 → 图上只剩 locale 已有的 nolib
     一行说明（0 节点），页头/目标/近期回执照常
```

节点集合锁死为 `{__source__, graph-claim, graph-dispatch, graph-gate}`；
边集合锁死为 `{__source__→graph-claim, graph-claim→graph-dispatch, graph-claim→graph-gate}`
（同时读 DOM 的 `[data-node-id]` 与 `host._graphSpec`，前者是用户看得见的，后者是拓扑事实）。

降级那一半把 `/graphs*.js` 换成一份空脚本（等价「库没加载」，不制造网络失败以免污染
pageerror 断言），断言：图上 0 个节点、宿主文字恰为 `sources.flow.nolib`、页头仍是这个源、
`[data-sec="targets"]` 与 `#src-detail-recent` 仍在。

## 6. 变异（两条必红，实测）

| 变异 | 做法 | 结果 | 现场 |
| --- | --- | --- | --- |
| ① 删掉图的渲染调用 | `void drawGraph(...)` → `void 0` | **红** | `图必须真画出来（4 节点），实际 {"waited":12017,"dom":[],"spec":null,"mounted":false,"text":""}` |
| ② 把 targets 解析成空 | `graphs.js` 的 `targetsOf()` → `return []` | **红** | `{"waited":12020,"dom":["__source__"],"spec":{"nodes":["__source__"],"edges":[]},"mounted":true}` —— 只剩源节点，边 0 条 |

两条都在改完后 `make fe` + 重跑同一 case 得到；改回后同一 case 恢复两条 PASS。

## 7. 门禁数字（本轮实测）

- `make ci`：exit 0（含 `check-classes` / `check-graph-access` / `check-i18n-keys` / `check-ui-copy` /
  `check-comment-density` / `check-affected-map` / `test-frontend*` / `go build` / `go test`）。
- `check-classes`：828 个类（HTML 595 + JS 233）全部落在设计系统 / Tailwind 工具类内。
- `check-i18n-keys`：4 份 locale 键集合一致，2163 个引用点全部有词条（本次**没有新增 key**）。
- `check-ui-copy`：4 份 locale / 9312 条值 · 22 个页面 / 180 个文本节点，全部通过。
- 注释密度棘轮：**不上调** —— 全仓 42929 行注释（733 个文件）与基线完全一致；
  本次新增的解释全部落在代码行尾注释上，另外删掉三条只复述类名/函数名的注释保持持平。
- `make e2e-affected`：exit 0，选中 5 套（frontend / i18n / i18n_coverage / ui_shift / ux_regression_19_23），
  **断言 216 通过 / 0 失败**，总耗时 250.4s；逐套：frontend 47/0（194.1s，real）、ui_shift 86/0（164.4s）、
  i18n_coverage 44/0（70.3s）、i18n 33/0（55.4s）、ux_regression_19_23 6/0（8.1s）；case 级重试 0 条、套件级重试 0 个。

## 8. 已知取舍

- 图库缺失的文案用了 `sources.flow.nolib`，因此 `sources.v4.graph_error`（旧源页的另一条同义文案）
  仍然没有被引用 —— 未引用 key 只告警不拦构建，清理留给专门的一轮，不在本 PR 扩大面。
- 降级时图那块高度仍是 320px（`.source-graph` 的 min-height）——一行说明放在里面，
  不做高度自适应，避免为降级态引入第二套尺寸规则。
