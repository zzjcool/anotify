# vars_ui「删一行再保存」的假红：一条没被等住的收尾重载（#544）

snapshot-doc: 本报告是某台 8 核机器上一轮实测的定格快照（改前/变异/改后三组数字对着 `/tmp/544-*.log` 与 `.e2e-bin/r-*` 可复算）；造压脚本与判据见 §5，复跑以新输出为准。

> **结论：修的是测试的等待，不是产品代码。** `vars_ui` 在「删一行再保存」处缺的不是时间，是**顺序**：
> `PUT …/yaml` 回执之后前端还有一段**串行收尾**，最后一跳会拿服务端快照重建表格，把还没落库的
> `row.deleted` 冲掉、把 `varsDirty` 归零（保存按钮重新 `disabled`），也会把没提交的编辑器收掉。
> 原测试「等单元格文本变成新值」被 **apply 时的本地重画**满足，所以那一步什么也没等到。
>
> 四处等待全部改成等**可观察状态**：不调大 15s、不加 `waitForTimeout`、不 skip。
> 造压下：改前 **2/10 红**，改后 **10/10 绿**（同一台机器、同一套造压脚本）；全量并发矩阵 **3/3 绿**。

## 1. 现象与复现

#542 的 16 次全量矩阵里 `vars_ui` 红 6 次（37.5%），模式是两条：5 次「delete → save 没等到
`PUT …/yaml`」、1 次 `locator.fill` 30s；空闲单跑 8/8 绿。本轮在本机（8 核）复现同一形态：

| 阶段 | 条件 | 结果 | 失败签名 |
| --- | --- | --- | --- |
| 改前 | 空闲单跑 | 74/0 绿，套件 3.1s | — |
| 改前 | 造压（配方见 §5）× 10 | **8 绿 / 2 红**（RUN4 28.6s、RUN9 29.0s） | `page.waitForResponse: Timeout 15000ms exceeded while waiting for event "response"`，两次都停在 **51 条断言**（正好是删行后那次保存） |
| 改后 | 造压（同一配方）× 10 | **10 绿 / 0 红**，套件 5.0–5.2s | — |
| 改后 | 空闲单跑 | 74/0 绿，套件 3.1s（改前 3.1s，绿路无增量） | — |

两次红的断言数完全一致（51），说明它们停在同一步；这也解释了为什么 `capacity` / `routing` 在同一批
实验里 0 失败 —— 不是「机器不够」，是这条时序。

## 2. 根因

### 2.1 页面侧：PUT 回执之后还有 4 段串行重载

`web-src/pages/node.html` 的表格保存（`:332`）在拿到 `PUT …/yaml` 回执后并不立刻结束：

```
saveAndWait 拿到回执
  └ saveNodeDoc()            :469  → await loadFacts() :479 → await loadConfig() :479
     └ 回到 save 收尾        :349  → await loadYAML() → await refreshVars()
        └ refreshVars()      :206…:229  用服务端快照重建 varsRows
                             :217  varsRows = VT.buildRows(...)
                             :218  varsEditing = null; varsDirty = false      ← 编辑器被收掉、脏标记被清
                             :224  $('node-vars-save').disabled = !varsDirty  ← 保存按钮重新禁用
```

命名空间页是同一形状：`:354-357` `loadNamespace → loadNamespaceYAML → loadEffective`，而
`loadEffective`（`:425-448`）同样在末尾 `varsEditing = null; varsDirty = false; renderVarsTable()`，
且它**没有** seq 守卫（只有 scope 校验），比节点页更不设防。

`row.deleted` 是纯前端字段（`:254-257`），从不提交给服务端；所以重建一定会把它抹掉 —— 被删的行
**复活**，保存按钮**变灰**。测试接着点保存：Playwright 的 `click` 等元素可用，
`waitForResponse` 的 15s 先到，于是报「等不到 PUT」。**不是请求失败，是点击根本没落到可点状态上。**

### 2.2 测试侧：那一步等待是假的

```js
await page.locator('#node-vars-table tr[data-key="NODE_ONLY"] [data-act="vars-edit"]').click();
await page.locator('#node-vars-table tr[data-editing="true"] input[data-field="value"]').fill("node-value-v2");
await page.locator('#node-vars-table [data-act="vars-apply"]').click();
await saveAndWait(page, "#node-vars-save", "/yaml");
await page.waitForFunction(() => document.querySelector('#node-vars-table tr[data-key="NODE_ONLY"] td.vars-value').textContent.trim() === "node-value-v2");
```

`varsApplyEdit`（`:317-320`）在 apply 时就改了 `row.value` 并**同步重画**，所以最后那句
`waitForFunction` 在 `PUT` 之前就已经成立 —— 它证明的是「本地改过了」，不是「服务端回来了」。
于是测试的下一步（删行 / 开编辑器）可以落在 §2.1 那段收尾还没走完的时候，被重建冲掉。

这也正是 #542 第二条红的来源：`refreshVars` 的 `varsEditing = null` 会让
`tr[data-editing="true"]` 消失，后面的 `locator.fill` 只能干等 30s。**同一根因，两种签名。**

### 2.3 与 issue 原文的一处偏差（按代码为准）

issue 提议的判据是「目标行从表格消失（`toHaveCount(0)`）」。实际代码里删掉的行**不消失**：
`varsPlainRow`（`:236-239`）给它 `data-deleted="true"` 并把操作区换成「撤销」。所以确定性判据是
「该行被标删」，不是「count 为 0」—— 按 issue 原话写会让断言永远等不到。

## 3. 修法：把「下一步」等在这段收尾之后

四处等待（`scripts/e2e/suites/vars_ui.mjs`），全部等可观察状态：

| helper | 等什么 | 用在哪 |
| --- | --- | --- |
| `waitVarsRerender` | 收尾最后一跳的**两个 GET 回执**（`GET /v1/nodes/<node>?namespace=<ns>` + `GET /v1/namespaces/<ns>/capabilities`）→ 页面**空网** → 一帧 | `saveAndWait` 的 `settle`（4 处保存） |
| `deleteVarsRow` | 点删除**前**先空网；点完等「该行 `data-deleted="true"` **且**保存按钮可用」——同一次原子检查 | 节点页删 `SOURCE_ROOT` |
| `openVarsEditor` | 点开编辑器（`vars-edit` / `vars-fill` / `vars-add`）后先等输入框真的在，再 `fill` | 4 处（节点 1、命名空间 3） |
| `watchInflight` | 页面里还有几个请求在飞（事件驱动计数，不睡觉、不猜 URL） | 上面三个的公共底座 + 命名空间页切节点后的一跳 |

关键设计取舍：

1. **为什么等这两个 GET 就是等收尾？** 两个页面的收尾都以它们结尾（节点页 `refreshVars`、命名空间页
   `loadEffective`），而且它们只在收尾里出现：节点页 `loadFacts` 读同一路径但**不带** `?namespace=`，
   `loadConfig` / `loadYAML` 是另一个路径。
2. **锚点在点击之前注册，用 `gate.put` 挡早于这次保存的回执。** 这一条是审查改出来的：Playwright 的
   `waitForResponse` 只看**注册之后**派发的事件，而一次 socket read 里的多条消息可能在同一个 macrotask
   里连发 —— 如果等 `PUT` 回执后再注册，主循环一卡（正是被造压压住的形态），`PUT` 与收尾的回执可能被
   同一批派发完，两个锚点一起错过，于是「PUT 明明 200 却报 15s 超时」。改成点击前注册 + `gate.put`
   之后才算数：既不漏事件，也不会把上一次保存的重建回执误当成本次的。
3. **为什么还要空网 + 一帧？** 响应到 ≠ DOM 已重建：`A.api` 要读完 body，`renderVarsTable` 在
   `Promise.all` 之后同步跑。空网排除「别的请求还在飞」，一帧让同步重绘落进 DOM。这两步都是事件/帧，
   不是睡眠。
4. **等状态的上界取 `STATE_WAIT = 30s`**，与 Playwright 动作自动等待同量级。刻意**不**取更小的值：
   把「慢」换成「红」是把假红搬家，不是修。
5. **不许的三件事都有机械守卫**：`waitForTimeout` 的仓库基线是 21（`scripts/check-test-discipline.mjs`
   的 `BASELINE`），加一处就红；15s 的 `waitForResponse` 一字未动；没有任何 skip。

## 4. 验收

### 4.1 稳定性

| 项 | 数字 | 耗时 |
| --- | --- | --- |
| 单跑 ×10（造压，见 §5） | **10/10 绿**，每次 74 断言 | 套件 5.0–5.2s/次，整体 14.8–16.4s/次（含 dist 校验与二进制检查） |
| 单跑（空闲，多次） | 绿 | 套件 3.1s，整体 6.7–6.8s |
| 全量并发矩阵 `make e2e` ×3 | **3/3 绿**（`vars_ui` 三次均 74/0） | 见 §4.5 |

### 4.2 变异：把新增的等待删掉就会红

做法：在工作树上把三处结构（删行 → `deleteVarsRow`、开编辑器 → `openVarsEditor`、4 处 `settle`）
换回改前的裸点击，**其它一律不动**（诊断保留），再用同一套造压跑 10 次；跑完 `git checkout --` 复原。

| 版本 | 造压 ×10 | 失败签名 |
| --- | --- | --- |
| 改前（无等待、无诊断） | **8 绿 / 2 红** | `waitForResponse: Timeout 15000ms`（51 断言处） |
| 变异（无等待、有诊断） | **5 绿 / 5 红**（RUN 2 / 6 / 7 / 8 / 9） | 见下 |
| 改后（有等待） | **10 绿 / 0 红** | — |

变异跑的失败签名全部落在 51 断言处，而且诊断把**两种**「没有请求」的形态都抓了出来：

| 诊断里的 `按钮(点击前)` | 次数 | 含义 |
| --- | --- | --- |
| `{"found":true,"disabled":true}` | 4 | 点击落在**禁用**按钮上：重建把 `varsDirty` 清掉 → 按钮变灰 → 没有请求 |
| `{"found":true,"disabled":false}` | 1 | 按钮可点但 handler **提前 return**：重建复活了被删的行 → `diff` 为空 → 走 `vars_no_changes` 分支，同样没有请求 |

两次都是 `点击后写请求=[]`、`表格=[…,"SOURCE_ROOT"]`（`SOURCE_ROOT` **没有** `:deleted`，即删除被冲掉）。
改前的 2/10 与变异的 5/10 是同一族概率波动（#542 记的是 6/16），重点不是比例而是**签名一致、
判据可复现**。

### 4.3 诊断：超时消息里能看到表格 key 与按钮状态

把「删行后保存」的选择器故意改成不存在的 `#node-vars-save-typo`，跑一次单套：

把「删行后保存」的 `saveSelector` 改成不存在的 `#node-vars-save-typo`，跑一次单套。抛出去的那两行：

```
等不到写 /yaml：page.waitForResponse: Timeout 15000ms exceeded while waiting for event "response" ⏎
按钮(点击前)={"found":false} 按钮(现在)={"disabled":false,"label":"保存这张表"} 点击后写请求=[] 表格=["JAVA_HOME:missing","NODE_ONLY","NS_ONLY","SOURCE_ROOT:deleted"] 状态="已保存：表格改动已写进同一份 YAML 文档。"
```

同一时刻落在 stdout 的全量现场（`runner` 只透传 stdout，所以现场必须写 stdout，见第 3 个提交）：

```
--- 保存超时现场（/yaml · #node-vars-save-typo）---
{
 "nodeRows": ["JAVA_HOME:missing", "NODE_ONLY", "NS_ONLY", "SOURCE_ROOT:deleted"],
 "namespaceRows": null,
 "nodeSave": { "disabled": false, "label": "保存这张表" },
 "namespaceSave": null,
 "nodeState": "已保存：表格改动已写进同一份 YAML 文档。",
 "namespaceState": ""
}
```

读法：`按钮(点击前)={"found":false}` 一眼说明**选择器写错了**（不是等待不够）；`表格` 里
`SOURCE_ROOT:deleted` 说明删除**已经生效**（所以判据确实该等标删）；`点击后写请求=[]` 把「请求没发出去」
与「请求发了但失败」分开 —— 这三条正是 #542 那 6 次红缺少的信息。

全量现场（含两张表、按钮、状态文案）另落 stderr，形如 `--- 保存超时现场（/yaml · #node-vars-save）---`
+ 缩进 JSON；抛出去的那两行受 `harness.mjs:209` 的 300 字截断限制，所以只放结论。

### 4.4 门禁

| 项 | 结果 |
| --- | --- |
| `make ci` | **exit 0**（含 `check-annotations` / `check-comment-density` / `check-test-discipline` / `check-affected-map` / e2e 锁自测） |
| `make e2e-affected` | **exit 0**：按改动只选中 `vars_ui`（“`scripts/e2e/suites/vars_ui.mjs` 是它自己的套件源码”），74 通过 / 0 失败，3.2s |
| `scripts/e2e/known-env-failures.json` | 未改，`entries: []`（保持空） |
| 注释密度棘轮（双向冻结） | 与基线**逐项相等**：总量 43300/43300、`scripts/e2e/suites` 2182/2182 |
| `check-test-discipline` 五项 | 21/21、38/38、3/3、37/37、3/3 全等 |
| `check-annotations` | unbacked 297/297、unmarked 209/209 全等 |
| 断言数 | 74（改前 74）—— 没有削弱任何断言 |

### 4.5 全量矩阵

| 轮次 | `vars_ui` | 套件墙钟 | 矩阵墙钟 | 矩阵结论 |
| --- | --- | --- | --- | --- |
| 1 | 74/0 绿 | 10.2s | 377.1s | rc=1：**只有 `frontend` 红**（见下注） |
| 2 | 74/0 绿 | 8.2s | 376.9s | rc=0 全绿 |
| 3 | 74/0 绿 | 8.8s | 377.5s | rc=0 全绿 |

`vars_ui` **3/3 绿**；矩阵墙钟 377.1 / 376.9 / 377.5s，与 #542 记录的基线（374–387s）同档，没有墙钟回归。
单跑 3.1s → 全量并发下 8.2–10.2s：多出来的是收尾那几次真实往返（外加并发下的服务端争用），仍在
120s 套件预算内。

> 注（第 1 轮 `frontend` 的红与本改动无关，不算本次回归）：签名是
> `retained-security-permissions — TypeError: Cannot convert undefined or null to object`
> （`f3-node-retained.mjs:442`），与 `docs/reports/e2e-parallel-experiment.md:105/163-166/234`
> 记录的**同一个** f3 聚合层诊断问题（单 case 单跑是 45/0 绿）。本 PR 不触碰 `frontend`、`f3`、`web-src/`。

## 5. 造压配方（可复现）

空闲机器上这条竞态大概率不出（窗口只有几段 GET），所以造压刻意**只压 CPU**、不动网络、不改系统配置：

```bash
# nproc=8：每核一个忙循环，把浏览器/服务端的调度挤到「收尾比下一步慢」
NP=$(nproc); PIDS=(); for _ in $(seq "$NP"); do ( while :; do :; done ) & PIDS+=("$!"); done
for i in $(seq 10); do
  E2E_RESULTS_DIR="$PWD/.e2e-bin/r-load-$i" ./scripts/e2e/run_all.sh vars_ui >"/tmp/run-$i.log" 2>&1
  echo "RUN $i rc=$?"
done
kill "${PIDS[@]}"
```

判据：改前 2/10 红（约 20%，与 #542 的 37.5% 同量级），改后 0/10。忙循环退出即恢复，不留系统状态。
**没有**用 `tc netem`、没有调 `E2E_PARALLEL`、没有改任何超时。

## 6. 已知耦合与遗留风险

1. **helper 里写死了收尾那两个 GET 的形状**。页面以后改重载路径时，症状会是
   `等不到表格重建（保存收尾里必出现 GET /v1/nodes/<node>?namespace=<ns> 与 …/capabilities）`，
   而不是一个没有信息量的 15s —— 但这条耦合是真的，改动那两个页面的读面时要一起改这里。
2. **`watchInflight` 依赖 Playwright 把 `request` 与 `requestfinished`/`requestfailed` 配成同一实例**。
   若哪天不成立，症状是 `还有 N 个请求在飞：<方法 + URL 列表>`（不是静默通过）。
3. **产品侧问题仍然开着（本 issue 刻意不修）**：vars 表格里「后台重载会静默丢掉未保存的编辑」。
   对照证据：YAML 输入框有 `if (!yamlDirty)` 守卫（`node.html:454`），表格没有。本轮只按 issue
   的范围把测试的等待写对；建议另开 issue 给 `refreshVars`/`loadEffective` 补上「有脏改动就不覆盖」
   的守卫（和 YAML 输入框对齐）。
4. **最坏情况仍受套件预算约束**：`vars_ui` 没有预算 override（默认 120s），绿路只多了几次 GET 与一帧；
   失败路上每个等待各自有界（15s / `STATE_WAIT`），且任一等待超时立即抛错，不会累加到「慢而成功」。

## 7. 本 PR 不做的事（范围）

- 不改产品代码：`web-src/**`、`web/**`、Go 侧零改动。
- 不调大 15s、不加 `waitForTimeout`、不 skip、不加重试。
- 不改断言强度与数量，不动其它套件，不动 `known-env-failures.json`。
