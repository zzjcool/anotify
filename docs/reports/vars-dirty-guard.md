# 节点 vars 表格的后台刷新不再静默吃掉未保存的编辑（#546）

snapshot-doc: 本报告是某台 8 核开发机上一轮实测的定格快照（`make ci` / `make e2e-affected` / 变异三组数字对着 `/tmp/ci.out`、`/tmp/e2e-affected.out`、`/tmp/e2e-mut.out` 可复算）；复跑以新输出为准。

> **结论：脏标记被清掉不是「刷新太快」，是刷新路径**没有**守卫。** `refreshVars()` 在重建表格前
> 无条件 `varsEditing = null; varsDirty = false`，于是任何一次重读都把「用户刚改、还没保存」的那一行
> 换成服务端事实、把保存按钮变回 `disabled` —— 界面上看起来像**从来没改过**。同页的 YAML 编辑器早
> 就有这条守卫（`if (!yamlDirty) …`），vars 表格没有。
>
> 修法是把「刷新」拆成三种口径：后台的遇草稿**整轮跳过**（脏标记与输入都不动，表格上方留一行说明）、
> 用户点的先过 `confirmDanger`（与「放弃改动」同一套口径）、确认过/刚写完的（force）照旧重建。
> 不脏时行为逐字不变。

## 1. 现象与判定

用户正在表里加一行变量、还没点保存，页面一刷新（切标签回来 / 手动点刷新）—— 输入没了，且保存按钮
变回禁用。三条症状必须**一起**看，缺一条就会被「看起来正常」骗过去：

| 症状 | 代码落点（改前） | 可观察断言 |
| --- | --- | --- |
| 行里的草稿被服务端值覆盖 | `refreshVars` 里 `varsRows = VT.buildRows(...)` | `tr[data-key=…] td.vars-value` 回到服务端值 |
| 保存按钮变回禁用 | 同一函数里的 `$('node-vars-save').disabled = !varsDirty` | `#node-vars-save.disabled === true` |
| 脏标记被清、没有任何提示 | `varsEditing = null; varsDirty = false` | 页面上没有一行说明「暂停刷新」 |

## 2. 落点

`web-src/pages/node.html` 的 `refreshVars()`：重建表格前加守卫，并把「口径」显式化。

| 口径 | 调用者 | 遇草稿的行为 |
| --- | --- | --- |
| 后台（默认） | 切回标签页的重读 | 整轮跳过：不发请求、脏标记与输入都不动、表格上方给一行说明 |
| 用户点的 | 页头 `#node-refresh` | 先 `A.confirmDanger`（文案与「放弃改动」同一套口径）：取消 → 整次刷新不发生；确认 → 按服务端事实重建 |
| force | 保存后 / 「放弃改动」确认后 / 出错重试 | 无条件重建（这些路径上草稿已经被用户处理掉了） |

**一处按代码实况对齐题面的偏差（已按更严的那侧实现）**：题面写的是「遇 `varsDirty`」，
但 #476 的表格里 `varsDirty` 只在 **apply 之后**才置位 —— 编辑框里刚打好、还没确定的那一行
（`varsEditing` 非空，`varsDirty` 仍为假）同样会被重建静默收掉，而它同样是「未保存的编辑」（issue 标题）。
所以守卫用的谓词是 `varsDirty || varsEditing`：**没有未保存的编辑时行为逐字不变**（§4 ⑤ 对照组钉住），
有未保存的编辑时一律不静默丢。e2e ② 专门钉住「apply 之前那一行也活过后台刷新」。

## 3. 后台刷新从哪来（本仓此前没有触发点）

改前全仓 grep：`web-src/pages/*.html` 与 `web/*.js` 里 `setInterval` / `visibilitychange` / `pageshow`
在页面数据重读上**一处都没有**（只有 passkey 倒计时用 `setInterval`）。也就是说，题面点名的
「后台定时 / 切标签回来」在节点页当时都不存在 —— 守卫若只加不接触发点，就是一段永远进不去的死代码。

本 PR 因此把题面现象里点名的**「切标签回来」**接上，并且只做这一件事：

```js
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && n) refreshVars();
});
```

- 只重读 vars 表格的上游读面（`GET /v1/nodes/{name}?namespace=` + 该 ns 的能力读面），不动其它卡片；
- 页面还没加载成功时（`n` 未就绪）不触发；
- **不做定时轮询**：轮询会在每个长跑用例里持续发读请求，等于给全量矩阵加一层与本次修复无关的噪声，
  且 `vars_ui` 的「保存收尾等 GET」判据会被它误满足（假绿）。切标签回来是这条丢输入路径里最常见的那一个，
  足够把守卫生效变成可观测事实。

## 4. 行为矩阵（e2e 逐条钉住）

| # | 状态 | 动作 | 期望 | 断言数 |
| --- | --- | --- | --- | --- |
| ① | 改一行、apply、未保存 | 后台刷新 | 行里仍是草稿、保存按钮仍 enabled、提示可见、**一个读请求都不发** | 4 |
| ② | 编辑框开着（还没 apply） | 后台刷新 | 编辑行与刚打的字都还在 | 1 |
| ③ | 同上（脏） | 点刷新 → **取消** | 出现在确认框（文案 = `nodes.v4.vars_refresh_confirm`）、取消后草稿与按钮都不变、整次刷新没发生（零读请求） | 4 |
| ④ | 同上（脏） | 点刷新 → **确认** | 表格回到服务器当前值、暂停提示自己收掉 | 2 |
| ⑤ | 表格没改动 | 后台刷新（对照组） | 表格内容跟着服务器更新、不弹确认框、没有暂停提示 | 5 |

## 5. 验收数字（本轮实测）

| 门禁 | 命令 | 结果 |
| --- | --- | --- |
| 单套件 | `./scripts/e2e/run_all.sh vars_ui` | **91 通过 / 0 失败**，套件 4.9s（合计 8.6s），`mode=real` |
| 改动自适应 | `make e2e-affected` | **16/16 套件通过，断言 1078 通过 / 0 失败**，430.3s；authority real 12 / explicit-mock 4；预算水位 frontend 212s/360s |
| PR 门禁 | `make ci` | **exit 0**（157s，最终树；含 `check-i18n-keys` / `check-ui-copy` / `check-classes` / `check-annotations` / `check-comment-density` / `check-doc-links`） |
| i18n 键 | `node scripts/check-i18n-keys.mjs` | 4 份 locale 键集合一致，2428 key / 2218 引用点全有词条 |
| 死类 | `node scripts/check-classes.mjs` | 869 个类全部有定义（新增 `.vars-paused` 在页面内联 `<style>` 里） |
| 文案 | `node scripts/check-ui-copy.mjs` | 通过（新增两条例外豁免不动） |
| 注释密度棘轮 | `node scripts/check-comment-density.mjs` | `web-src/pages` 2400 = 2400、`scripts/e2e/suites` 2182 = 2182、仓库总量 43300 = 43300 —— **基线一个字都没上调**，改动的净注释行数为 0 |
| known-env | `scripts/e2e/known-env-failures.json` | `entries: []`（本轮未新增登记，原本就是空的） |

## 6. 变异：去掉守卫必红

把 `refreshVars()` 里的守卫整段删掉（= 恢复「直接清空重建」），只重跑 `vars_ui`：

```
❌ ① 后台刷新后行里还是未保存的草稿        — 期望 draft-546 实际 node-value-v2
❌ ① 后台刷新后保存按钮仍 enabled          — 期望 false 实际 true
❌ ① 跳过时表格上方给一行说明              — 期望 [true,"有未保存改动，暂停刷新"] 实际 [false,…]
❌ ① 整轮跳过：没有发出任何节点读请求      — 期望 1 实际 2
E2E EXIT=1（套件判 fail）
```

四条断言同时红，正好对应 §1 的三条症状 + 「整轮跳过」这条实现判据；把守卫生回去，同一套件 91/0 绿。

## 7. 没做的（边界）

- **不改后端、不改 PUT 语义**：写路径仍是唯一的 `PUT /v1/nodes/{name}/yaml`，本轮没有新增字段、
  没有新增端点，表格重建也只读既有读面。
- **命名空间页没动**：`web-src/pages/namespace.html` 有一份同形状的表格（`loadEffective` 末尾同样
  `varsEditing = null; varsDirty = false`），但 #546 的范围是节点页，且那一页同样没有后台刷新触发点。
  这是**尚未修的同类落点**，不在本轮偷偷扩大，留给单独一张单子。
- **不做定时轮询**（理由见 §3）：等真有「表格必须自己变」的需求再单独上，并同批想清楚它与 e2e 收尾判据的
  可分辨性。
