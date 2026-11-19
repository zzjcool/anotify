<!-- snapshot-doc: 本报告里的数字是本次修复落地时的定格（复算命令见 §9） -->

# deployments 读模型「malformed_list」修复（#435）

> 用户在节点详情页「已部署能力的实际状态」看到：

```
响应不是 *List 信封：/v1/namespaces/default/deployments
reason: malformed_list
```

修的是**前端**（契约真源不给后端改的理由）。下面每一段都有可重跑的判据。

## 1. 根因（核实结果，不是转述）

| # | 事实 | 证据 |
| --- | --- | --- |
| ① | 契约里这个端点的响应是**裸 `{deployments:[…]}`**，不是 `*List` 信封 | `api/resource.yaml:3205` `DeploymentList`（原文见 §2） |
| ② | 后端发的正是裸投影 | `internal/server/retained_reads.go:228` `writeJSON(…, map[string]any{"deployments": items})` |
| ③ | 后端与契约被双向对账（wire 键 ↔ schema 属性） | `internal/server/openapi_parity_test.go:3207` 把 `retainedReadAPI.deployments` 的响应对 `resource.yaml · DeploymentList` |
| ④ | 前端用**严格列表**读它：`apiList()` 要求 `kind` 以 `List` 结尾**且** `items` 是数组 | `web/partials.js` `apiList()` / `itemsOf()`（`malformed_list`） |
| ⑤ | 页面调的就是 `apiList('/…/deployments')` | 修前 `web-src/pages/node.html` `loadRelated()` |
| ⑥ | **掩盖者**：mock 预览发的是 `DeploymentList` 信封 + v3 风格行（`spec/node/status` + 展平的 `node` 名字） | 修前 `web/mock.js` `GET …/deployments` → `listOf("DeploymentList", deploymentItems(ns))` |

所以 ①③④⑤ 组成一次**契约两侧不一致**：规范说“裸投影”，前端按“`*List` 信封”读。
⑥解释了为什么只有真服务才爆（mock 反过来替错误的前端圆谎）。

同一页还有**第二处**同源缺陷（修完信封后仍然会「有数据渲染成空」）：

- 部署行的身份是 `nodeId`（节点 id，`Resource.yaml · Deployment` 的 required 字段），页面却按
  `x.node === name`（节点**名**）过滤 —— 真服务的行上根本没有 `node` 这个字段。
- 该过滤器还存在语义重复：端点本来就支持 `?nodeId=` 服务端过滤。

## 2. 真源判定（引契约原文）

`api/resource.yaml:1445` 起的端点声明：

```yaml
  /v1/namespaces/{namespace}/deployments:
    get:
      operationId: listDeployments
      ...
      responses:
        '200':
          content:
            application/json:
              schema: {$ref: '#/components/schemas/DeploymentList'}
```

`api/resource.yaml:3205` 的 schema：

```yaml
    DeploymentList:
      type: object
      required: [deployments]
      properties:
        deployments:
          type: array
          items: {$ref: '#/components/schemas/Deployment'}
```

`Deployment` 的 required 是 11 个字段：`id, namespaceId, nodeId, capabilityId, capability,
version, digest, state, reason, createdAt, updatedAt`（`internal/store/deployment.go:74` 的 json tag 与此一致）。
`DeploymentList` **没有** `apiVersion`/`kind`/`metadata`/`items` —— 它不是资源列表信封。

三条独立旁证（都不是“读同一份代码得出的结论”）：

1. `openapi_parity_test.go` 用反射把 handler 的 `writeJSON` 键集合与 schema properties **双向**比对；
   多一个/少一个键都红。它把该 handler 配到 `DeploymentList`。
2. e2e 早就按裸投影写断言：`scripts/e2e/suites/deploy.mjs:121`
   「listDeployments 读部署事实读模型（v4：ns 路径 + 裸 `{deployments:[…]}`，不套信封）」，
   `scripts/e2e/suites/node_v4.mjs:485` 读 `r.json?.deployments`，`scripts/e2e/fixtures/f3/f3-node.mjs`
   的 `deployApplied()` 读 `rows.deployments` + `d.nodeId`/`d.state === 'applied'`。
3. 前端 `apiList()` 自己的语义（`kind` 必须 `*List` + `items[]`）在 capabilities/sources/events/tasks
   上都对得上后端 `writeList()`；只有 deployments 对不上 —— 说明错的是这一处调用点。

**结论：规范是 `{deployments:[…]}` ⇒ 改前端，后端与 `api/*.yaml` 一行不动。**

## 3. 改动

| 文件 | 改动 |
| --- | --- |
| `web-src/pages/node.html` | `A.api('…/deployments?nodeId=<uid>')` + `A.deploymentsOf()` 读 `.deployments`；行过滤交给服务端（删掉 `x.node === name`） |
| `web/partials.js` | 新增 `deploymentsOf(res)`：只认 `deployments` 数组（空列表 `[]` 合法）；非数组抛明确的畸形响应错误，不再把这份投影伪装成“列表信封” |
| `web/mock.js` | `GET …/deployments` → `{deployments:[…]}`，行只留 `Deployment` 的 11 个字段（`nodeId`/`namespaceId`/`capabilityId`/整数 `createdAt` 等）；`state` 改为节点回报语义（`applied`/`pending`，不再是 v3 的 `deployed`/`stale`/`missing`）；支持 `?nodeId=`/`?capabilityId=`；`POST …/deploy` 改成 `CapabilityDeployResponse`（`{deployed,version}`，200，逐节点 `{node(id),state,sent}`） |
| `internal/server/retained_reads_test.go` | 增加**顶层键集合**断言（必须恰好是 `{deployments}`），并把注释里“v3 形状”的说法改成 `Deployment` |
| `scripts/v4-frontend.test.mjs` | mock 单测改成契约形状（裸投影 + 11 个字段 + `?nodeId=` + deploy 回执）；新增 `deploymentsOf` 单测 |
| `scripts/e2e/fixtures/f3/f3-cases.mjs` / `f3-driver.mjs` | 新增 e2e 用例 `node-deployments`（真 Chrome）：真实权威下由场景起真夹具节点 → 真部署 → 等 `applied`，再断言节点页渲染出真行 |

前端页面只改调用方式，DOM 结构与样式未变（`check-classes` 863 个类不变）。

## 4. 全仓同类扫描（`apiList(` 调用点 ↔ 后端 writeJSON 形状）

| 调用点 | 端点 | 后端 | 契约 | 一致？ |
| --- | --- | --- | --- | --- |
| `partials.js` `nsList()` → capabilities / sources | `writeList`（`CapabilityList`/`SourceList`） | `*List` | ✅ |
| `capabilities.html:942`、`sources.html:464`、`source.html:599`（caps） | 同上 | `*List` | ✅ |
| `source.html:735`、`source-detail.html:419`（events） | `resource_task.go:500/674` `writeList("EventList")` | `EventList` | ✅ |
| `node.html:428`、`nodes.html:452`（namespaces） | `writeList("NamespaceList")` | `*List` | ✅ |
| `node.html`（tasks） | `writeList("TaskList")` | `*List` | ✅ |
| **`node.html`（deployments）** | `{deployments}` | `DeploymentList`（裸） | ❌ **本次修复** |

结论：**`apiList` 误用只有这一处**。其它读“非 `*List` 文档”的调用点本来就走 `A.api()`：

- `edges` → `{items}` 文档（`web/mock.js` 里已有同型注释：套信封会让页面误以为能走 apiList）；
- `capability-templates` → `{templates}`（mock 与 Go handler 一致）；
- `capabilities/{name}/deploy` → `CapabilityDeployResponse`（`capability.html` 读 `out.deployed`）。

顺带修掉的 mock 谎言（这两个也是“真实服务一接就错”的形状，只是当时页面恰好先读了正确字段）：
`GET …/deployments` 的信封、`POST …/deploy` 的 `DeploymentList` 信封 + 201。

## 5. 变异证据（每条都是“改回去就红”）

### 5.1 后端：把 handler 改回信封

```bash
# 临时把 retained_reads.go:228 改成 {"apiVersion":..,"kind":"DeploymentList","items":..}
go test ./internal/server -run TestRetainedDeploymentsListAndFilters -count=1
```

```
--- FAIL: TestRetainedDeploymentsListAndFilters
    retained_reads_test.go:337: deployments 必须是裸 {deployments} 投影（不是 *List 信封），
    得到顶层键 [apiVersion items kind]
```

### 5.2 前端：把页面改回 `apiList`（= 用户看到的那个 bug）

```bash
# 临时把 loadRelated 改回 A.apiList(base + '/deployments') 并 make fe + 重编 server
node scripts/e2e/suites/frontend.mjs --case node-deployments
```

```
node-deployments — AssertionError:
  部署区块渲染成了错误态（契约读错）：
+ [ '响应不是 *List 信封：/v1/namespaces/default/deployments' ]
- []
```

**这条报错文本与用户上报的一字不差** —— 即本次修复对应的缺陷被这个用例正面覆盖。

### 5.3 前端：只把“行身份”改回节点名（信封已修好）

```
node-deployments — AssertionError: 有部署事实时必须渲染出真行，不能是空态
```

三条变异都只在工作区内存/临时改动上做，证据在 §5 的日志里，仓库不含变异代码。

## 6. 空结果必须是 `[]` 而不是 `null`

- 后端：`retained_reads.go:225-227` 把 `nil` 换成 `[]*store.Deployment{}`；`TestRetainedDeploymentsListAndFilters`
  断言无匹配时响应体里就是 `"deployments":[]`（该断言在修复前就存在，未改动）。
- mock：`deploymentItems()` 永远返回数组，`{deployments: []}`。
- 前端：`deploymentsOf({deployments: []})` 返回 `[]`（单测覆盖）；`null`/缺失数组才报畸形。

## 7. 门禁

### `make ci`

- 静态面全绿：`make contract`（五份 spec 双向对账）、`check-agents`、`lint-go`（vet）、
  `make fe`（`check-classes` / `check-wire-fields` / `check-css` / `check-i18n-keys` /
  `check-contrast` / `sitegen` / 指纹）、`check-guard-selftest`、`check-test-discipline`、
  `check-annotations`（棘轮 300/300、385/385）、`check-affected-map`、`check-secrets`、
  `check-anode-skill`、`tmp-leak-guard`（残留 0）。
- `go test ./...`：**39 个包 ok**，本批改动的 `internal/server` ok（61.5s）。
- **唯一红**：`cmd/anode` 的 4 个安装脚本测试（`TestInstallScriptPreflightRejected` /
  `TestInstallScriptAutoUserWithPrefixDowngradesToNone` / `TestInstallScriptPreflightFailureNoPhantomStart` /
  `TestInstallScriptRequiresToken`）。**与本次改动无关，且在本机 `origin/main` 纯净树上同样红**：

```bash
git stash push -m baseline && go test ./cmd/anode -run TestInstallScript -count=1; git stash pop
# → FAIL github.com/zzjcool/anotify/cmd/anode （同一组 4 条）
# 注：这组用例用假 sudo/systemctl 走分支，本机（Arch/Omarchy + mise shim + 可用 sudo）落在
#     “非 root + 有 sudo → system”分支，与用例期望的首次安装路径不符；CI runner 上不受影响。
```

### `make e2e-affected`

改到了 `web/*.js`（`affected-map.mjs` 的 `SHARED_CHROME_SURFACES`：所有页面共用脚本），
所以选择器按设计升级为**全量 28 套**（不是漏选）：

```
⚠️ 全量 e2e（28/28 套）：二进制风险面：server←cmd/server
```

结果：**28/28 套件、2288 条断言通过 / 0 失败、总耗时 347.1s**，临时目录残留 0：

```
✅ E2E 全绿：28/28 个请求套件均达标（断言 2288 通过 / 0 失败，authority 清单覆盖 28 个）
  ✅ frontend     40通过/0失败  150.6s  mode=real
  ✅ node_v4      88通过/0失败    5.0s  mode=real
  ✅ deploy       93通过/0失败    4.8s  mode=real
  …
```

新增的 `node-deployments` 用例在 `frontend`（real，真 Chrome + 真 server + 真夹具节点）里跑过；
它的 mock 权威形态也单独跑过一遍（`E2E_MOCK_ONLY=1 … --case node-deployments`），两边都绿 ——
这意味着 mock 与真实服务对这份投影的**评测口径已经一致**，mock 预览不再掩盖契约漂移。

## 8. 发现但本批未修（都在本批范围之外）

1. `scripts/v4-frontend.test.mjs` 是**未接线**的 node 单测（`docs/lint-guards.md:96` 登记了它带基线红）。
   实测基线 `217 个 test / 206 通过 / 11 失败`；本批改动后 `218 个 test / 207 通过 / 11 失败` ——
   失败集合**逐条相同**（新增的 `deploymentsOf` 单测通过；两条 mock 形状单测由红转绿）。这 11 条里包含一条**本批之前就已过期**的字面量：
   `E1b … assert.equal(CASES.length,38)` 而实际注册数在 #435 之前就是 39、本批后是 40。
   本批**没有**把它改成 40：那等于顺手改“已知红台账”（还需同步 `docs/lint-guards.md` 的措辞），
   属于另一次变更；这里只登记事实，不等同于认可它。
2. `web-src/pages/node.html` 部署行的 `x.spec?.capability?.name || x.capability`：前者是 v3 残留
   （`Deployment` 没有 `spec`），后者是契约字段，所以**显示结果正确**；`check-wire-fields` 的可选链
   形态扫不到它。本批不改（与本次缺陷无关），登记在此避免下次又被当成“契约字段”。
3. `web-src/pages/capability.html` 里部署回执的 `out.items` 兜底分支（`out.deployed → out.items`）：
   mock 修正后该分支已成为死代码。保留是因为它不在本次契约不一致的因果链上（`deployed` 先读，
   行为正确），删它会扩大 diff。
4. `web/mock.js` 的 `POST …/deploy` 成功码从 201 改为契约的 200，动机是让 mock 不再说谎；
   如果还有外部脚本依赖 mock 的 201，会在预览里看到状态码变化（真实服务一直是 200）。

## 9. 复现与验证命令

```bash
# 1) 后端形状（含空列表 = []）
go test ./internal/server -run 'TestRetainedDeploymentsListAndFilters|TestOpenAPIParity' -count=1

# 2) mock 与 canonical 层（未接线单测，作为形状文档 + 回归对照）
node --test scripts/v4-frontend.test.mjs

# 3) 真实浏览器 + 真实服务（本 bug 的正面覆盖）
make fe && go build -tags embeddist -o .e2e-bin/anotify ./cmd/server
ANOTIFY_BIN=$PWD/.e2e-bin/anotify DEVSEED_BIN=$PWD/.e2e-bin/devseed ANODE_BIN=$PWD/.e2e-bin/anode \
  node scripts/e2e/suites/frontend.mjs --case node-deployments

# 4) 按改动选择的全量门禁
make ci && make e2e-affected
```

## 10. 交付

- issue：#435（本 bug 的登记）
- PR：#437（`Fixes #435`）→ https://github.com/zzjcool/anotify/pull/437
