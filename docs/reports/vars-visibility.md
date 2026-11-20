# 可视化 vars（节点页 / 命名空间页）实施报告（#476）

<!-- snapshot-doc: 本文件是 #476 的实测证据：断言数 / 注册表计数 / 命令输出是当次运行的快照，会随代码演进，不是长期承诺。 -->

> 目标：把「这台机器 / 这个名字空间里，每个变量**是什么值、从哪一层来、被哪些能力要**」
> 变成一眼可读的一张表；阶段 2 允许在表里增 / 删 / 改，但**写进同一份 YAML 文档**，
> 不引入第二种写路径。PR：`Fixes #476`。

## 0. 一页结论

| 项 | 结论 |
|---|---|
| 阶段 1（只读） | 两页各一张五列表（名字 / 值 / 来源 / 被哪些能力用到 / 操作）；节点页 `#node-vars-table`、命名空间页 `#namespace-effective-vars` 都是 `web/vars-table.js` 的同一套纯逻辑渲染 |
| 阶段 2（可编辑） | 增删改只写 `spec.vars`，序列化成**整份文档** → `PUT /v1/{nodes\|namespaces}/{name}/yaml`；页面里对资源级 `/config` 的**写**调用 = 0（新守卫静态锁死） |
| 数据真源 | 值/来源 = `GET /v1/nodes/{name}?namespace=<侧栏作用域>` 的 `status.effectiveVars`（服务端逐键解析）；需求侧 = 当前 ns 能力读面的 `spec.requires.values`（元素是 `vars.<NAME>`，页面前端剥前缀） |
| 缺但被要求 | `data-missing="true"` + `⚠ 必填` + 行底色（`--error-soft`）；只认「**有效值不存在**」，不是「本层没有」 |
| 不混入 | `envPassthrough` 不在 vars 表里出现（它是机器私有字段，YAML 读面也不下发）；表格只碰 `spec.vars` |
| 新端点 | `PUT /v1/namespaces/{name}/yaml`（新增；形状照 `nodeAPI.yamlPut`，CAS = `metadata.resourceVersion`，即 `GET …/config` 返回的同一个令牌） |
| 守卫 | `scripts/check-config-writes.mjs`（真解析内联 `<script>`，`--self-test` 49 项），挂 `make fe` 与 `make check-guard-selftest` |
| 前端单测 | `scripts/vars-table.test.mjs` 11 个（含变异自证） |
| e2e | `scripts/e2e/suites/vars_ui.mjs`（真 server + 真 Chrome，61 断言；登记进 `affected-map` 与注册表 29 个） |

## 1. 为什么要「YAML 是唯一编辑面」

`GET …/config` 只是**读投影**：它把 `node_configs` / `namespace_configs` 的一部分字段照出来，
缺了 `tags` 之类的字段（#454 的教训：用 `/config` 回写把 tags 清空）。所以：

- 编辑只能走 YAML（服务端从当前事实重建的完整文档），页面把表格改动**序列化进这份文档**；
- 页面里再出现 `PUT …/config` 就是同一份事实的第二条写路径 —— 两条路各自 200，
  只有用户看到的值对不上，且没有任何运行期报错。这条缝隙只能静态锁死（守卫）。

**守卫的边界**（写在脚本头，也写进 `docs/lint-guards.md`）：只拦**资源级**配置投影
（`/nodes/<x>/config`、`/namespaces/<x>/config`）；系统保留期 `PUT /v1/system/config` 是
**合法例外**——它的 System YAML 只读，那个 PUT 是保留期的唯一写入口（收掉等于删功能）。
识别成写调用并豁免 1 处，输出里单列「豁免 1 处」，不是漏扫。

## 2. 表格语义（`web/vars-table.js`，纯逻辑、不碰 DOM/网络）

| 概念 | 判据 | 为什么 |
|---|---|---|
| `source` | 直接取 `effectiveVars[key].source`（`local` / `node` / `namespace`） | 来源是服务端逐键解析的权威事实，页面不拿三层映射自己猜 |
| `missing` | `usedBy.length > 0 && value === null` | 「被能力要求」且「没有任何生效值」才是缺口；本层没有但上层有 = 正常覆盖 |
| `owned` | `source === layer`（或用户显式新增/编辑该行） | 只把**本层文档**该管的键写回 YAML。否则在节点页保存会把 namespace 默认值抄成 node 默认值，悄悄多出一层覆盖 |
| `locked` | 节点页 + `source === 'local'` | 本机快照（`anode.yaml`）由节点自己提供，中枢改不了 → 不给编辑按钮，只给说明 |
| `shadowed` | 命名空间页 + `source` 是 `local`/`node` | 可以写 ns 默认值，但当前有效值仍来自上层；编辑时明确提示「压不住」 |
| 名字两层校验 | 引用形状 `[A-Za-z_][A-Za-z0-9_]*`（与 `$(vars.X)` 同一套）+ 服务端权威形状 `^[A-Z][A-Z0-9_]{0,63}$` | 前者是能力引用/`workDir` 的语法，后者是 store 的写入校验；两层都过才允许提交，错误文案分开 |
| 建议名 | 现有名字 ∪ **能力要求但缺失**的名字 | 覆盖「补一个被要求却没有的变量」这条主路径；`<datalist>` 只做建议，不限制输入 |

`rowsToVars` 只收 `owned && !deleted && value != null`；`varsToYAMLLines` 排序后输出，
空映射显式 `{}`（不写 `vars:` 后跟空白）。`diffVars` 只用于「没有改动」判定与提示。

## 3. 阶段 1 验收（只读）

e2e 套件 `vars_ui` 的前两段就是阶段 1 验收（真服务 + 真 Chrome）：

- 节点页 4 行：`SOURCE_ROOT`（node，2 个能力要）、`NODE_ONLY`（node，无需求）、
  `NS_ONLY`（namespace）、`JAVA_HOME`（缺失，**红标**，1 个能力要）；
  每个有值键的「值 / 来源」逐键对照 `status.effectiveVars`（不是页面自证）。
- 命名空间页（选中节点）同一批行，来源列分别为 `node` / `namespace`。
- **加载零写请求**（`page.on('request')` 里没有任何非 GET）；无 JS pageerror。
- 既有 f3 锚点保留：`#namespace-vars` / `#namespace-save` / `#namespace-effective-vars [data-key] td`
  列序（1=值、2=来源）/ `#node-vars` / `#node-config-save`；节点页仍然**没有** `#node-effective-vars`。

## 4. 阶段 2 验收（可编辑，真写）

### 4.1 真服务 + 真浏览器：改一个 var，YAML 只有该键变

转录命令（脚本在 `.e2e-bin/transcript.mjs`，是一次性工具、不入库；服务用 `anotify` 真二进制，
浏览器用系统 Chrome 点真页面）：

```
POST /v1/namespaces/default/capabilities   → 201   (build requires vars.SOURCE_ROOT + vars.JAVA_HOME)
POST /v1/nodes                             → 201   (spec.vars = {SOURCE_ROOT:/srv/repos, NODE_ONLY:node-value})
PUT  /v1/namespaces/default/config         → 200   (vars = {NS_ONLY:ns-value})

### 节点页：改一个 var（NODE_ONLY: node-value → node-value-v2）
--- GET yaml (before) 摘录 ---
apiVersion: anotify/v1
kind: Node
  name: anode-demo
  resourceVersion: rv1:nd_BCc0XJBtxi5tDSs4ln:2
  namespaces:
  vars:
    NODE_ONLY: node-value
    SOURCE_ROOT: /srv/repos
--- diff -u 口径（只列变化行） ---
-   resourceVersion: rv1:nd_BCc0XJBtxi5tDSs4ln:2
-     NODE_ONLY: node-value
+   resourceVersion: rv1:nd_BCc0XJBtxi5tDSs4ln:3
+     NODE_ONLY: node-value-v2

PUT http://localhost:7911/v1/nodes/anode-demo/yaml
body = {"yaml":"apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: anode-demo\n  resourceVersion: rv1:nd_BCc0XJBtxi5tDSs4ln:2\nspec:\n  namespaces:\n    - default\n  vars:\n    NODE_ONLY: node-value-v2\n    SOURCE_ROOT: \"/srv/repos\"\n  limits:\n    perCapability: {}\n  tags: []\n"}

### 命名空间页：改一个 var（NS_ONLY: ns-value → ns-value-v2）
--- GET yaml (before) 摘录 ---
apiVersion: anotify/v1
kind: Namespace
  name: default
  resourceVersion: rv1:ns_BCc0XIcDjNf7Lb4kjd:2
  vars:
    NS_ONLY: ns-value
--- diff -u 口径（只列变化行） ---
-   resourceVersion: rv1:ns_BCc0XIcDjNf7Lb4kjd:2
-     NS_ONLY: ns-value
+   resourceVersion: rv1:ns_BCc0XIcDjNf7Lb4kjd:3
+     NS_ONLY: ns-value-v2

PUT http://localhost:7911/v1/namespaces/default/yaml
body = {"yaml":"apiVersion: anotify/v1\nkind: Namespace\nmetadata:\n  name: default\n  resourceVersion: rv1:ns_BCc0XIcDjNf7Lb4kjd:2\nspec:\n  vars:\n    NS_ONLY: ns-value-v2\n"}

写请求清单： ["/v1/nodes/anode-demo/yaml","/v1/namespaces/default/yaml"]
```

读法：两次保存都只有 `resourceVersion` 与**那一个键**变；`SOURCE_ROOT`（节点层）、
`namespaces` / `tags` / `limits` 结构逐字不变。写请求清单里没有任何 `/config`。

### 4.2 e2e 覆盖（`vars_ui`，61 断言）

- 改键：YAML diff = 2 行换 2 行（`resourceVersion` + 该键），其余键逐字相同；
- 删键：YAML 只少该行（+`resourceVersion`），同文档其它键不动；删掉后该名字**重新变成红标**
  （仍被能力要求），不是从表里消失；
- 增键：`JAVA_HOME` 用「填入」进入编辑态，保存后写进 ns 文档，红标消失；
- 名字校验：`1FOO` / `FOO-BAR` 前端即时禁用「确定」并给形状说明；绕过页面直接 PUT 含
  `"1FOO"` 的文档 → 后端 `400 value_invalid` 且消息点名该键；
- 写路径：整段测试的写请求**全部**打到 `…/yaml`（正则断言，不是采样）；
- 真实 f3 用例同步：#476 之后 `#node-config-save` / `#namespace-save` 的保存都走 YAML，
  f3 的 `node-config`（stale 409）、`namespace-system`（旧 rv 409）、`namespace-detail`（YAML 读面）
  的 `expectHttpStatus` 声明一并改到 `…/yaml`，仍然是「真网络上必须发生过」。

### 4.3 两条变异（反假绿）

**变异 ①：把保存改回 `PUT …/config` → 守卫红**

```
✗ 配置写路径守卫：1 处**写** /config 的调用（YAML 才是唯一编辑面，#476）
   web-src/pages/node.html:733 → write(path + '/config', { metadata: { resourceVersion: config.metadata.resourceVersion }, vars: doc.spec.vars }) → config-literal-write：第一个实参含以 /config 结尾的字面量「/config」
GUARD_EXIT=1
```

**变异 ②：打掉「缺但被要求」的红标判据 → e2e 红**

```
❌ JAVA_HOME 有红标（data-missing=true） — 期望 true 实际 false
❌ 红标汇总文案说明有 1 个缺口
❌ 命名空间页 JAVA_HOME 红标 — 期望 true 实际 false
断言失败: 4（vars_ui 42通过/4失败）
```

两处变异都已还原（守卫全扫 EXIT=0；`vars-table.test.mjs` 11/11 绿）。

## 5. 改了什么

| 文件 | 作用 |
|---|---|
| `internal/server/resource_namespace_yaml.go`（+`_test.go`） | 新增 `PUT /v1/namespaces/{name}/yaml`：`decodeYAMLRequestBody` → `yamldoc.Decode` → 拒 `status` → 严格 JSON → apiVersion/kind/name → `casTokenFromMeta` → `PutNamespaceConfigByName` → 回读；`namespaceYAMLText` 的 `resourceVersion` 改用**配置行**令牌（与 `GET …/config` 同一个值） |
| `internal/server/resource_namespace.go` | 注册 PUT（GET 保留） |
| `api/resource.yaml` | 新 operation + `ResourceYAML` 请求/响应；parity 登记（138 ops） |
| `web/vars-table.js` | 表格纯逻辑：`checkVarName` / `requiredNames` / `buildRows` / `rowsToVars` / `suggestNames` / `renameImpact` / `yamlScalar` / `varsToYAMLLines` / `diffVars` |
| `web-src/pages/node.html` | vars 卡（五列表 + 增删改 + 缺口语义）；`#node-config-save` 与表格保存共用一个 `saveNodeDoc()`（序列化整份 Node 文档 → `PUT …/yaml`）；`total` 未设置时**省略**该行（见 §6） |
| `web-src/pages/namespace.html` | 右侧卡改成五列表（按所选节点解析值/来源）；左卡原始 JSON 保留为「高级」入口，保存同样走 `PUT …/yaml`；无节点时退回 ns 默认值视图（不编造缺口） |
| `web/ui.css` | `.vars-table` 组件 + 行状态（`data-missing` / `data-deleted`）+ 横向滚动关在容器内（f3 每页断言 document 不横向溢出） |
| `web/mock.js` | 预览的 Namespace 文档 prelude 改为指向 `PUT …/yaml`；PUT 仍 501（预览不解析 YAML，与 Node 文档一致） |
| `web-src/locales/*.yaml`（4 份） | 表格/编辑/校验/提示文案；4 份键集合一致 |
| `scripts/check-config-writes.mjs` + Makefile + `docs/lint-guards.md` | 新守卫：真解析内联脚本、剥注释、按调用点判写、`--self-test` 49 项、反假绿计数下限；挂 `make fe` / `check-guard-selftest` |
| `scripts/vars-table.test.mjs` | 11 个前端单测（含变异自证） |
| `scripts/e2e/suites/vars_ui.mjs` + `affected-map.mjs` + `lib/harness.mjs` + `parallel_runner.mjs` | 新套件（真 server + 真 Chrome）、端口段、注册表 29、映射行 |
| `scripts/e2e/fixtures/f3/*` | 保存目标/请求形状/409 声明/prelude 断言同步到 YAML 写入口 |
| `docs/v4/e2e-inventory.md` | 逐套件台账 + 注册表计数 29 |

## 6. 发现的产品 bug（未在本 PR 修，按边界上报）

1. **`GET /v1/nodes/{name}/yaml` 的文本贴不回去（无 limits 的节点）**：服务端为表达
   「不压住下层」把 `total` 序列化成显式 `total: null`，但 `store.ValidateLimits` 要求
   `total` 是 ≥1 整数 → 原样 PUT 回 400 `value_invalid: limits.total 必须是 ≥1 的整数，实际 <nil>`。
   复现：对任意未设 limits 的节点 `GET …/yaml` 再原样 `PUT`（本报告转录里 `原样贴回 → HTTP 400` 即此）。
   影响：节点页的 YAML 文本框（只读展示 + 保存按钮）在这种节点上仍然会 400；
   **表格保存不受影响**——它从 `GET …/config` 构造文档，`total` 未设置就省略该行（省略 = 不动）。
   旧报告已把这条登记为 `#454` 的相邻缺陷；要修需要决定 `total: null` 的写入语义（清空？不动？），
   属于 store 契约变更，不在本 issue 的范围。
2. 当时的 IA 设计文档（§0.3 / §2.1；已随设计文档整套移除，见 `docs/README.md`）与历史报告
   `docs/reports/ns-detail-and-system.md` 仍把 Namespace YAML 描述成「没有 PUT」。
   历史报告是当时证据，保留原文；设计文档已删除，权威口径见
   `internal/server/resource_namespace_yaml.go` 文件头（#476 之后写入口是 `PUT …/yaml`）。

## 7. 门禁

- `flock /tmp/anotify-ci.lock -c "make ci"` → 结果见 §7.1
- `make e2e-affected` → 结果见 §7.2
- `node --test scripts/vars-table.test.mjs` → 11/11
- `node scripts/check-config-writes.mjs --self-test` → 49/49；全扫 EXIT=0
- `make check-annotations` → 棘轮同步下调（300 → 299，见 §7.1 说明）
- `node --test scripts/e2e/affected.test.mjs` → 全绿（含「套件源码里出现的页面必须登记」）

### 7.1 `make ci`

```
$ flock /tmp/anotify-ci.lock -c "make ci"
…
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
✅ 模块约定与代码一致
✅ class 校验通过：877 个类（…）全部落在设计系统 / Tailwind 工具类内
✅ 契约字段守卫通过（234 个 schema、724 个已知字段）
✅ self-test 49/49 通过
✅ 配置写路径守卫：页面对 /config 只有读、没有写（扫描面：页面 23 · 内联 script 块 25 · 写调用点 32）
✅ i18n 键守卫：4 份 locale 键集合一致
✅ 文案守卫通过：4 份 locale / 23 个页面
✅ test-discipline / annotations / doc-links / doc-endpoints / affected-map / test-frontend / secrets 全绿
ok  github.com/zzjcool/anotify/internal/server   (含 TestNamespaceYAMLPutRoundTrip)
go build ./...  /  embeddist 构建通过
✅ harness 自测通过
FINAL_EXIT=0
```

（`make ci` 跑在**代码终态**的提交树上；报告文字是之后补的，只影响文档，补齐后
`check-doc-links` / `check-doc-endpoints` / `check-annotations` / `check-secrets` 已重跑通过。）

### 7.2 `make e2e-affected`

改动落在 `api/resource.yaml`（五份 OpenAPI 真源）→ 选择器按 fail-closed 选中**全量**：

```
$ E2E_CONCURRENCY=3 make e2e-affected
  ⚠️ 全量 e2e（29/29 套）：api/resource.yaml 落在共享面（五份 OpenAPI 真源）
…
  ✅ vars_ui            61通过/0失败
  ✅ frontend           43通过/0失败  203.7s  mode=real
  ✅ ui_shift           87通过/0失败  166.8s  mode=explicit-mock
  ✅ i18n_coverage      44通过/0失败   99.4s  mode=explicit-mock
  ✅ i18n               33通过/0失败
  ✅ capacity           84通过/0失败  147.5s
  …（29/29 全部达标）
✅ E2E 全绿：29/29 个请求套件均达标（断言 2431 通过 / 0 失败，authority 清单覆盖 29 个）
✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 325.9s）
FINAL_EXIT=0
```
