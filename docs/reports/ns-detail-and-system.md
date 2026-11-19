# 命名空间详情页补齐 + 系统配置区块（#436 ③④）—— 交付报告

- Issue: [#436](https://github.com/zzjcool/anotify/issues/436)
- 设计: [`docs/design/node-namespace-ia.md`](../design/node-namespace-ia.md)（§2.2 已按实现校准）
- 基线: `origin/feat/node-namespace-ia` @ `7cd93dbb`（PR [#441](https://github.com/zzjcool/anotify/pull/441)，①② 在那边；
  该分支已把 `origin/main` 合进来，本 PR 直接 rebase 到它）
- 本分支: `feat/ns-detail-yaml`
- 本轮: **③ 命名空间详情页补齐**（只读 Namespace YAML + 逐节点有效值表）、
  **④ 系统配置成区块**（只读 System YAML + 权限态文案 + 危险操作分区）
- 依赖：**#441 先合**（本 PR 以它的分支为基线，不重复它已交付的内容）

## 0. 用户原话逐字对照

| 用户要求 | 落点 | 机械证据 |
| --- | --- | --- |
| 「namespace 页进去应该是一个当前有什么 ns 的列表」 | `web-src/pages/namespaces.html`（① 已建） | e2e：导航点进去至少一行 `#ns-list [data-ns]`；行 href `^namespace.html\?name=` |
| 「点击其中一个进入 ns 具体信息页面查看」 | `web-src/pages/namespace.html`（① 已建） | e2e：点 `[data-ns="default"]` → URL `namespace.html?name=default`，`#ns-name` = default，页头有返回列表 |
| 「ns 的 yaml 我觉得也是没有问题的，我同意」 | ③ 只读 `GET /v1/namespaces/{name}/yaml` + 详情页 YAML 卡 | e2e：`#namespace-yaml-text` 含 `kind: Namespace` / `spec:` / `vars:`；curl 原文见 §2.1 |
| ④ 系统配置成区块 + System YAML | admin 页 `#system-config` 三区分组 + 只读 `GET /v1/system/yaml` | e2e：`#system-yaml-text` 含 `kind: SystemConfig`；member → `#admin-forbidden` 含 `403` |

## 1. 先验证「列表 → 详情」这条路

1. **导航入口只在共享侧栏，不在页面里复制**：`web/partials.js` 的 `NAV`
   （WORKSPACE 组，`id: "namespaces"` / `labelKey: "common.nav.namespaces"` /
   `fallback: "命名空间"` / `href: "namespaces.html"`）。`mountLayout()` 用这一份数据给每一页
   渲染侧栏，所以全站只有**一处**定义；本轮没有新增第二份导航。
2. **列表每行可点**：`web-src/pages/namespaces.html` 的 `row()` 产出
   `<a class="nsl-row" data-ns="<ns>" href="namespace.html?name=<ns>">`。
3. **详情能回列表**：`web-src/pages/namespace.html` 页头 `a[href="namespaces.html"]`
   （文案 `namespace.v4.back`）。切换 ns 走 `select#config-namespace`，URL `name` 参数同步更新。
4. **e2e 断言（real authority，`namespace-detail` case）**：

```
PASS namespace-detail :: namespace detail: list → detail → yaml / per-node table / member 403:
     actual geometry and unlocked scroll
PASS namespace-detail :: Sidebar nav (partials.js NAV) → namespaces list → row → namespace.html;
     read-only Namespace YAML (kind: Namespace, no editor); per-node table rows + effective values
     with server-provided source; node pages carry none of it; member sees real 403
     (page + /v1/system/yaml).
```

截图（文字内容即上面两条断言现场，1440×900、无横向溢出）：
`scripts/e2e/fixtures/f3/screenshots/f3/runs/current/real/namespace-detail/<attempt>/namespace-detail-list-to-yaml-and-member-403.png`
（截图里可见：左侧共享侧栏 `Namespaces` 项；admin 页 member 态是 `Admin access denied` +
`HTTP 403 · Forbidden` + 返回按钮，不是空白页。）

## 2. ③ 命名空间详情页补齐

### 2.1 只读 Namespace YAML

- 端点：`GET /v1/namespaces/{name}/yaml` → `{"yaml": "<全文>"}`（`ResourceYAML`，与
  `GET /v1/nodes/{name}/yaml` 同 shape / 同缩进 / 键序稳定、两次 GET 字节一致）。
- 实现：`internal/server/resource_namespace_yaml.go`。读面在返回前把**自己生成的文本**过一遍
  `internal/yamldoc.Decode`（同一个严格形状）：文本坏 → 500 具名失败，而不是把客户端贴不回去的
  文档发出去。
- 文档形状（实测 curl 原文，见下）：`apiVersion` / `kind: Namespace` /
  `metadata.name` / `metadata.resourceVersion` / `spec.vars`。
  **没有** `status`（派生计数）、`metadata.uid`（不透明内部身份）、`displayName`（用户数据，
  理由见 §7）。
- **只读**：没有 PUT 路由。写 vars 的唯一入口是 `PUT /v1/namespaces/{name}/config`
  （CAS + `namespaceVarsRevision`）；文档头部注释与页面文案都写明这一点。
  生产 mux 对 `PUT …/yaml` 落 404（静态兜底，与 `production_mux_test` 记录的「退役写 = 404」
  同因），单测只钉「不是 2xx」。
- 未知 ns → 404 `NotFound`（与 JSON 读面同一 `resolve()` 判据）。

实测 curl（本机临时 server + devseed 会话；member 也可读该 ns 文档）：

```console
$ curl -s -b "anotify_session=$SESSION" http://localhost:6231/v1/namespaces/default/yaml
{"yaml":"# Namespace document (read-only). spec.vars are namespace defaults; a node may\n# override them (merge order: local > node > namespace). Writes go through\n# PUT /v1/namespaces/{name}/config (CAS) - this endpoint deliberately has no PUT.\napiVersion: anotify/v1\nkind: Namespace\nmetadata:\n  name: default\n  resourceVersion: rv1:ns_BCaoAU04Kl5TPomU6I:2\nspec:\n  vars:\n    # namespace defaults; a node may override them (see the per-node effective values table)\n    FEATURE_X: \"on\"\n    LOG_LEVEL: info\n"}

$ curl -s -w '\nHTTP %{http_code}\n' -b "anotify_session=$SESSION" \
    http://localhost:6231/v1/namespaces/nope/yaml
{"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":404,"reason":"NotFound","message":"namespace not found"}
HTTP 404

$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' -X PUT -b "anotify_session=$SESSION" \
    -H 'Content-Type: application/json' -d '{"yaml":"kind: Namespace"}' \
    http://localhost:6231/v1/namespaces/default/yaml
HTTP 404
```

页面：`namespace.html` 新增「命名空间 YAML（只读）」卡（`#namespace-yaml-text` +
`#namespace-yaml-copy`，`.code-block` 等宽 + 一键复制；`#namespace-yaml-state` 报读取状态）。
**没有编辑框**（e2e 断言 `#namespace-yaml-input` 不存在），文案
`namespace.v4.yaml_hint` 说明为什么只读（第二条写入口会让「谁赢了」无法判定）。

### 2.2 「逐节点有效值」从节点页搬到 ns 页

**先确认了实际接口名与形状（不猜）**：

- `GET /v1/nodes/{name}?namespace=<ns>` 的响应在**顶层**多一个 `context.namespace`，
  `status.namespaceVars` + `status.effectiveVars` 才出现（`internal/server/resource_node.go`
  的 `nodeReadContext` / `nodeStatus.EffectiveVars`）。
- `status.effectiveVars` 是 `map[key]store.EffectiveVar`，每项 `{"value","source"}`
  （`internal/store/config.go`）。`source` 的封闭集合是 **`local` / `node` / `namespace`**
  —— 服务端逐键解析（local > node > namespace），页面不自己拿三层变量表猜。
- **「能力级」覆盖来源在 vars 上不存在**：`perCapability` 只出现在 `limits`
  （`spec.limits.perCapability`）。所以表格的「覆盖来源」列给的是 `local` / `node` /
  `namespace` 三档，并在 `namespace.v4.effective_source_hint` 里写明这一点（本轮不新增能力层，
  也不改合并优先级）。
- 节点身份 / 状态来自 `GET /v1/nodes` 的 `status.online` / `status.draining` /
  `status.configRevision` / `status.appliedRevision`（`api/resource.yaml` · `NodeStatus`）。

页面形态（维度与节点页相反：在 ns 页选节点）：

| 元素 | 内容 | 说明 |
| --- | --- | --- |
| `#namespace-nodes` | 表格：**节点 / 状态** | 每行 `tr[data-node]`，点击 = 选中该节点（与 `select` 同一个选中态） |
| `select#namespace-node` | 节点选择 | 保留既有锚点（旧 e2e 与深链习惯不变） |
| `#namespace-node-status` | `状态 · configRevision · appliedRevision` | 选中节点的只读状态行 |
| `#namespace-context-revision` | `命名空间上下文 = <ns>` | 保留 `[data-key="namespace"]` 锚点 |
| `#namespace-effective-vars` | 表格：**变量 / 有效值 / 覆盖来源** | 每行 `tr[data-key]`，来源是服务端给的 `source` |

revisions 与 counts 保持原处并被 e2e 继续断言：`#namespace-revisions`
（`resourceVersion` + `namespaceVarsRevision`）与 `#ns-counts`（`counts.capabilities/nodes/
sources/tasks`）。

### 2.3 空态

- 没有节点：`#namespace-nodes` 显示 `namespace.v4.no_nodes`（不是一张空白表），
  `select#namespace-node` 禁用，有效值区显示同一句空态。
  e2e 的空态分支在 **explicit-mock** 权威下造（清空 `MockApi.state.nodes`）；真实权威的
  节点是 fixtures，删掉会污染同批其它用例 —— 这一点写在用例注释里，不假装覆盖。
- 没有有效变量：表格显示 `nodes.v4.none`（「无」）。

## 3. ④ 系统配置成区块 + 只读 System YAML

### 3.1 只读 System YAML

- 端点：`GET /v1/system/yaml` → `{"yaml": "..."}`，`kind: SystemConfig`，
  `spec.retention.{eventTtlSec,taskTtlSec,resultTtlSec}`，字段名与 `GET /v1/system/config`
  的 JSON wrapper 逐字相同（同一份事实的另一种序列化）。只读、无 PUT。
- **管理员面**：非 admin → 403 `Forbidden`（未登录仍是 401），与页面/mock 的文案一致。

实测 curl：

```console
$ curl -s -b "anotify_session=$ADMIN" http://localhost:6231/v1/system/yaml
{"yaml":"# SystemConfig document (read-only). retention holds three TTLs in seconds; 0 means\n# keep forever. Writes go through PUT /v1/system/config (CAS) - this endpoint\n# deliberately has no PUT.\napiVersion: anotify/v1\nkind: SystemConfig\nmetadata:\n  resourceVersion: \"1\"\nspec:\n  retention:\n    eventTtlSec: 2592000\n    taskTtlSec: 2592000\n    resultTtlSec: 604800\n"}

$ curl -s -w '\nHTTP %{http_code}\n' -b "anotify_session=$MEMBER" http://localhost:6231/v1/system/yaml
{"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":403,"reason":"Forbidden","message":"administrator required"}
HTTP 403
```

### 3.2 页面分组结构

`#system-config` 拆成三个 `.sys-group`（标题 + 说明 + 分隔线）：

1. `#system-group-retention` —— 保留期（可改）：三个 TTL 输入 + 「秒 ≈ 天 / 0 = 永久」提示
   （`#result-ttl-hint` 等，ND-4 既有行为保留）。
2. `#system-group-yaml` —— System YAML（只读）：`#system-yaml-text` + `#system-yaml-copy` +
   `#system-yaml-state`；保存保留期后**同步**刷新（同一份事实两个视图，不允许一个是旧的）。
3. `#system-group-danger` —— 危险操作：显式的保存动作 + 「超出新窗口的事件/任务/产物会被清理且
   不可恢复、0 = 永久保留」的说明（`.sys-danger` 视觉分区）。

### 3.3 权限态文案

- 页面级：member 打开 `admin.html` → `#admin-forbidden` 卡（原有 `showForbidden`）新增
  `HTTP 403 · Forbidden` 一行（`admin.forbidden.status`），不再是只有一句「无权访问」。
- 区块级：若页面级替换没有发生（例如 me 与后续调用之间角色变化），
  `#system-forbidden` 会显示「仅管理员… · HTTP 403」，`#system-fields` 保持隐藏。
- 服务端：`/v1/system/yaml` 与 `/v1/system/config`（GET/PUT）统一走 `requireAdminUser`
  （401 未登录 / 403 非 admin）。**这是本轮的行为收紧**，见 §7-1。

## 4. 后端改动清单

| 文件 | 改动 |
| --- | --- |
| `internal/server/resource_namespace_yaml.go`（新） | 两条只读 YAML 读面 + 文档形状 + `requireAdminUser` + 严格自校验 |
| `internal/server/resource_namespace.go` | 注册两条新路由；`/v1/system/config` GET/PUT 改走管理员判据 |
| `internal/server/resource_namespace_yaml_test.go`（新） | 文档往返、空 vars `{}`、无 `status/uid`、两次 GET 一致、未知 ns 404、无写路径、严格解析行号、System YAML + member 403 |
| `internal/server/resource_namespace_test.go` | 测试用户改 admin；新增 member 会话 helper；「所有路由都要登录」表补两条新路由 |
| `api/resource.yaml` | `getNamespaceYAML` / `getSystemYAML` 两条 operation；system config/YAML 的 403 响应 |
| `internal/server/openapi_parity_test.go` | ratchet：mux pattern 120 → 122、operation 131 → 133（注释写原因） |
| `scripts/check_openapi.py` | `NAMESPACE_OWN_SUBRESOURCES` 加 `yaml`（ns 自身子资源沿用 `{name}`） |

## 5. e2e 覆盖（六条要求 + 防回潮）

| # | 断言 | 用例 | authority |
| --- | --- | --- | --- |
| 1 | 共享侧栏有且只有一个 `#sidebar a[href="namespaces.html"]`；点进去 URL 变 `/namespaces.html`，`#ns-list [data-ns]` 至少一行，行 href 形如 `namespace.html?name=` | `namespace-detail` | real + explicit-mock |
| 2 | 点 `[data-ns="default"]` → URL/页头都是 default；页头有返回列表入口 | 同上 | real + explicit-mock |
| 3 | `#namespace-yaml-text` 含 `apiVersion` / `kind: Namespace` / `spec:` / `vars:` / 真实 var；不含 `status:`；含 `PUT /v1/namespaces/{name}/config` 只读声明；没有 `#namespace-yaml-input`；真实 `PUT …/yaml` 不是 2xx | 同上 | real + explicit-mock |
| 4 | `#namespace-nodes [data-node]` 至少一行、列 = 节点/状态、状态非空；选/点同一节点后 `#namespace-effective-vars [data-key]` 给出值 + 来源 `namespace`；空态：无节点时是明确空态 + 选择器禁用 | 同上 | 数据 real + mock；空态 explicit-mock |
| 5 | 节点页没有 `#node-effective-vars` / `#node-namespace` / `#namespace-nodes` / `#namespace-effective-vars` / `#namespace-yaml-text`；节点列表页也没有 ns 逐节点表 | 同上（+ 既有 `node-config` 用例） | real + explicit-mock |
| 6 | member 打开 admin：`#admin-forbidden` 含 `403`、没有可写 `#system-save`；真实 `GET /v1/system/yaml` → 403 `Forbidden` | 同上 | real + explicit-mock |
| 附 | `#system-yaml-text` 含 `kind: SystemConfig` / `retention:`；保存 `resultTtlSec=0` 后文本同步出现 `resultTtlSec: 0` | `namespace-system` | real + explicit-mock |

「改回去即红」的落点：去掉侧栏项 → #1 的 `count()==1` / click 超时；删返回链接 → #2；
删 YAML 卡或加编辑框 → #3；删逐节点表 / 改坏来源列 → #4；把这些搬回节点页 → #5；
把 403 改成空白或去掉 admin 闸门 → #6（真实 API 断言会拿到 200）。

## 6. 门禁数字

```
make contract        ✅ 133 operations / 133 unique operationId（131 + 两条只读 YAML）
cli e2e contract     ✅ 185 通过 / 0 失败（含新增的 member 读/写 403 各一条）
cli e2e capacity     ✅ 84 通过 / 0 失败
make check-agents    ✅ 6 area；server 58 lines / store 66 lines，引用全部可解析
make lint-go         ✅
make fe              ✅ class 875 全部有定义；i18n 引用点 2133；sitegen 4 语言 × 23 页；指纹 31 资源 / 92 HTML
make check-guard-selftest   ✅ 判定 PASS 48 / FAIL 0
make check-test-discipline  ✅ 五项与基线一致（bare-tag-first-nth 3/3）
make check-annotations      ✅ unbacked 300/300，unmarked 385/385（基线）
make check-affected-map     ✅（映射表自测 + PAGES 覆盖）
make check-secrets   ✅
go build ./...       ✅
go test ./...        ✅ 全绿（含 cmd/anode：四条 install-script 用例随基线合入 main 的 #440 修复转绿）
```

e2e（affected）：`api/resource.yaml` 属共享面 ⇒ 选择器按 fail-closed 判「全量」，
`E2E_CONCURRENCY=2 make e2e-affected`：

```
✅ E2E 全绿：请求的套件全部达标（28/28，passed 2358 / failed 0），authority real 24 / explicit-mock 4
   frontend 42 · contract 185 · capacity 84 · api_contract 220 · events_paging 178
   routing 168 · ws_protocol 134 · cli_auth 133 · security 100 · ui_shift 87
   i18n_coverage 44 · i18n 33 · 其余套件全部 0 失败；总耗时 354.2s
   ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）
```

定向复跑（新增/改写的用例，real + explicit-mock 双权威）：

```
node scripts/e2e/suites/frontend.mjs --case namespace-detail     → 1 通过 / 0 失败
E2E_MOCK_ONLY=1 … --case namespace-detail                       → 1 通过 / 0 失败
node scripts/e2e/suites/frontend.mjs --case namespace-system     → 1 通过 / 0 失败
E2E_MOCK_ONLY=1 … --case namespace-system                       → 1 通过 / 0 失败
```

## 7. 发现 / 取舍 / 遗留风险

1. **发现产品 bug（已修）**：`GET/PUT /v1/system/config` 在真实服务上**只要登录就能读写**，
   与页面注释（「对其它角色 403」）、mock（`ME.role !== "admin" → 403`）都不一致。
   本轮把 `/v1/system/config` 与新的 `/v1/system/yaml` 统一到 `requireAdminUser`。
   影响面已核对并同步修改：Go 侧仅 `newNamespaceTestEnv`（改 admin）+ 新 member 用例；
   e2e 侧 `capacity` 的两段保留期场景改用 `H.seed(..., { admin: true })`（新增的 harness
   选项，底层是 devseed 的 `-admin`）；`contract` 的 D6 段新增真实 admin 会话，
   并**加了两条 member 403 的读/写断言**。这不是「改断言迁就」：断言强度
   （200 + 精确值）没变，口径从「登录即可读写」收紧成「只有 admin」——那条被删掉的
   假设本身就是 §7-1 的产品 bug。另有两条口径写进报告：
   - 写信封的语法校验在 mux 外层，**先于**鉴权：畸形体 → 400（对所有路由一致）。
     所以 D6 的 member 写用**合法形状**的请求体，钉的是「鉴权先于 CAS 令牌解析」。
   - `contract` 的 admin 会话在 D6 段才创建：额外用户会多一个 `default-N` 命名空间，
     而 A1 的断言是「这次注册只有一行 default」，提前建会让它测到夹具而不是产品。
2. **`PUT …/yaml` 在生产 mux 是 404 而不是 405**：末尾静态兜底会接管未注册方法
   （`production_mux_test` 对退役写已记录同一行为）。所以单测/用例钉的是「不是 2xx」，
   不把 405 写死（那会在整机装配下假红/假绿）。
3. **服务端生成的 YAML 注释用英文**：文本与 locale 无关（页面不翻译它），而 en/es 的 i18n
   chrome residue 扫描会把 `<pre>` 里的中文当成漏翻。中文解释放在页面文案
   （`namespace.v4.yaml_hint` / `effective_source_hint`）。
4. **Namespace YAML 不含 `displayName`**：它是用户数据（可以是任意语言），`<pre>` 只含文本时
   没有 `data-raw-content` 那样的豁免机制；需要展示名请读 JSON 读面。
5. **空态只在 explicit-mock 造**（清空节点）：真实权威的节点来自 fixtures，删除会污染同批
   其它用例。空态代码路径仍在真实页面上（同一个渲染函数），只是真实权威下没有可制造的入口。
6. **`cmd/anode` 四条 install-script 用例**：在旧的基线 `7cca144a` 上会失败（脚本在
   「无令牌」用例里仍然读了已存在的系统配置）；基线和 `origin/main` 合并后（`7cd93dbb`，
   main 侧 #440 的修复）已转绿。本分支 rebase 到新基线后 `make ci` 全绿 —— 这条记录保留，
   是为了说明「基线变了要 rebase」而不是「本分支带着已知红」。
7. **`make ci` 现在全绿**（含 `cmd/anode`），上一轮报告的四条例外已消失。
8. 新 worktree 必须先 `npm ci`（否则 `check-wire-fields` 缺 `yaml` 依赖），再 `make fe`，
   否则 `dist-stamp --check` 会让 e2e 直接拒跑（这是故意的）。

## 8. 提交与 PR

<!-- COMMITS -->

PR 基于 `feat/node-namespace-ia`，PR 描述里注明**依赖 #441 先合**；不 force push、不直推 main。
