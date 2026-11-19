> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

# 节点 / 命名空间 / 系统信息架构重构 —— 交付报告

- Issue: [#436](https://github.com/zzjcool/anotify/issues/436)
- 设计: 已随设计文档移除（历史见 `docs/README.md`）；行为以代码与 `api/` 为准
- 分支: `feat/node-namespace-ia`（基线 `origin/main 2c115020`）
- PR: [#441](https://github.com/zzjcool/anotify/pull/441)（①② 同一 PR，提交按 docs / server / web / report 分开）
- 本轮落地: **① 页面 IA + 旧 URL 兼容**、**② Node YAML 查看/编辑**
- 未落地（设计已写死，可独立继续）: ③ Namespace 独立页的 YAML 面、④ System 迁位已完成 —— 见 §6

## 1. 现状事实（只读侦察结论）

| 事实 | 位置 | 结论 |
| --- | --- | --- |
| 命名空间 vars 存在 `namespace_configs.vars`，**没有** limits 列 | `internal/store/schema.sql`（namespace_configs） | 原 `nodes.html` 的「命名空间 vars 配置」是**命名空间级**数据，只是宿主放错了 |
| 节点配置存在 `node_configs.namespaces / vars / limits` | `internal/store/schema.sql`（node_configs） | `namespaces` 缺省 `["*"]`；它是**授权**列表 |
| 系统保留期存在 `system_config` 单行表 | `internal/store/schema.sql` | 系统级配置，与节点无关 |
| `effectiveNamespaces` = `store.AuthorizedNamespaces` = 中枢授权 ∩ 本机申报 | `internal/store/config.go:1359-1417` | 是**硬闸门/授权**（派发 / 产物 / WS 都读它），不是「可见」 |
| limits 只有「本机 + 节点」两层，`ValidateLimits` 只认 `total` / `perCapability` | `internal/store/config.go:194-228` | 命名空间**没有** limits —— 新页面不能编一个 |
| `anode.yaml` 私有字段 `server/token/listen/envPassthrough` 不进 Node spec、不上报 | `api/resource.yaml`、`internal/protocol/config_frames.go:15` | Node YAML 文档里不能出现它们；token 明文只在 `POST /v1/nodes/{name}/tokens` 出现一次 |
| `internal/yamldoc` 是叶子包严格解析器：`Decode(data, Shape) → Result{JSON, LineOf}` | `internal/yamldoc/yamldoc.go` | 行号 1-based、字段路径点号分隔、错误消息不回显标量值（凭据回显红线） |
| 旧页面宿主 | `web-src/pages/nodes.html`（`#namespace-config` / `#system-config`）、`web-src/pages/node.html`（`#node-namespace` / `#node-effective-vars`、`#node-namespaces` / `#node-vars` / `#node-limits`） | 两类配置寄生在错的维度上 |

## 2. 设计要点

1. **页面地图**：`nodes.html` 只列节点；`node.html?id=<name>` 只看一台节点（没有 namespace 参数）；
   新增 `namespaces.html`（列表）与 `namespace.html?name=<slug>`（vars + 逐节点有效值）；
   系统保留期进 `admin.html`。旧链接带 `?namespace=` 时**忽略参数、不重定向**（书签不 404）。
2. **Node YAML = 同一份事实的另一种序列化**：`apiVersion/kind/metadata(name, resourceVersion)/spec(namespaces,vars,limits,tags)`，
   **无** `status`、**无** `metadata.uid`（写端白名单只有 name/resourceVersion，带上 uid 会让「取 → 贴回」被拒）。
3. **解析只存在服务端一份**：前端只做「表单 → 文本」序列化，从不解析；写入走 `yamldoc.Decode` +
   与 `PATCH /v1/nodes/{name}` 同一条 `store.UpdateNode` 路径（同一 CAS、同一 revision 语义）。
4. **授权 ≠ 副本**：节点侧只保留 `spec.namespaces`（授权）与 `status.effectiveNamespaces`（派生生效值）；
   某 ns 的有效变量去 `namespace.html` 选节点看，节点配置不为每个 ns 再存一份。
5. **无数据迁移**：表结构、API 读面、CAS 判据均未变；搬的是「谁渲染它」。
6. **拆分**（设计 §6）：① 页面 IA + 路由兼容 → ② Node YAML → ③ Namespace 页 YAML 面 → ④ System 区块迁位（本轮随 ① 一起完成）。

## 3. 本轮改动

### 服务端（②）

- `internal/server/resource_node_yaml.go`（新）
  - `GET /v1/nodes/{name}/yaml` → 服务端规范化重建 Node 文档（2 空格缩进、键序稳定、两次 GET 字节一致）。
  - `PUT /v1/nodes/{name}/yaml` → `decodeYAMLRequestBody` → `yamldoc.Decode(writeDocShape(...))`
    → 拒 `status` → apiVersion/kind 校验 → `decodeObjectMeta`（name/resourceVersion）→ 路径名一致性
    → `casTokenFromMeta` → `store.UpdateNode`（与 PATCH 同一条路径）→ 回读并返回规范化文档。
  - 失败面：`invalid_yaml` / `unknown_field` / `value_invalid` / `status_not_writable` / `resource_version_conflict`，
    `details.causes[]` 每条带行号与字段路径。
- `internal/server/resource_node.go`：`register` 加两条路由（不新增 mux 写入者）。
- `internal/server/resource_node_yaml_test.go`（新）：
  规范化文档精确断言、禁 `status/token/listen/envPassthrough/server/uid`、两次 GET 字节一致、
  取→贴回幂等（除 rv 前进）、省略字段保持、未知字段带行号、`status` 拒绝、kind/name 不匹配、
  缺 CAS / 旧 CAS（409 带 `details.current.resourceVersion`）。
- `internal/server/resource_node_test.go`：把 `/yaml` 的两条路由并入「所有节点路由都过 session」用例。
- `internal/server/openapi_parity_test.go`：ratchet 129 → 131 operations、118 → 120 mux pattern（注释写明原因）。
- `api/resource.yaml`：`getNodeYAML` / `putNodeYAML` 两条 operation（`make contract` 双向对账通过）。

### 前端（① + ② 的面板）

- `web-src/pages/nodes.html`：删除 `#namespace-config`（vars）与 `#system-config`（保留期）两块及其控制器；
  改留一张「已迁出」说明卡（链接到新宿主），避免旧肌肉记忆落空。
- `web-src/pages/namespaces.html`（新）：`NamespaceList` 只读表（能力/节点/源/任务计数 + 创建时间），
  行锚点 `[data-ns]`，进入 `namespace.html?name=`；不发明批量删除/配额这类没有 API 的动作。
- `web-src/pages/namespace.html`（新）：
  - `select#config-namespace` + `#namespace-vars` / `#namespace-save` / `#namespace-reload` / `#namespace-revisions` / `#namespace-state`
    —— **逐字沿用**原节点页的 CAS/ABA 判据（请求代号 + 编辑器身份 + 保存范围取自加载成功的那次）。
  - 逐节点有效变量：`select#namespace-node` → `GET /v1/nodes/{name}?namespace=<ns>`，
    表格逐键给出 value + source（local > node > namespace），`#namespace-context-revision` 带 `[data-key="namespace"]`。
- `web-src/pages/node.html`：
  - 去掉 `#node-namespace` / `#node-context-revision` / `#node-effective-vars`（不再有 ns 上下文读面）。
  - 新增「授权命名空间 / 生效命名空间」卡（`#node-authorized` / `#node-effective-namespaces`，只读 chip + 链接）。
  - 新增 YAML 面板：`#node-yaml-text`（规范化只读文本 + 复制）、`#node-yaml-edit` 折叠里的
    `#node-yaml-input` / `#node-yaml-save` / `#node-yaml-reset` / `#node-yaml-errors`；
    表单（`#node-namespaces`/`#node-vars`/`#node-limits`）改动在用户没手改 YAML 时同步成同一份文本，
    错误逐条渲染 `字段路径 + 行号消息`。前端**不含** YAML 解析器。
- `web-src/pages/admin.html`：系统保留期整块迁入（`#system-config`，`#result-ttl`/`#event-ttl`/`#task-ttl`
  与会话/文案不变），非管理员不显示、且控制器对「无权限页整块替换 `#page-main`」零 DOM 报错。
- `web/partials.js`：`NAV` 新增「命名空间」（`namespaces.html`）+ 图标；`common.nav.namespaces`。
- 4 份 locale（zh-CN/en/ja/es）同步：`common.unknown`、`common.nav.namespaces`、`nodes.v4.*`（YAML、授权/生效、迁出说明）、
  `namespaces.v4.*`、`namespace.v4.*`。
- `web/mock.js`：`GET /v1/nodes/{name}/yaml`（同一份事实的规范化序列化）；
  `PUT …/yaml` 在预览里**具名拒绝**（`yaml_write_unavailable`，501）——不在 mock 里造第二个宽松解析器。

### e2e（改宿主 + 新增断言）

- `f3-driver.mjs`：`PAGES` 加 `namespace` / `namespaces`；`MATRIX_BATCHES` 末段改用 `PAGES.length`（避免新页静默漏跑）。
- `f3-ui-helper.mjs`：`PAGES` 同步；`ANCHORS` 加两页。
- `f3-i18n-cases.mjs` / `f3-cases.mjs`：`anchors`、`query(...)`（`namespace` → `name=default`）、
  `pageAnchors.node` 改 `#node-machine-vars`、`pageAnchors` 加两页。
- `f3-node-retained.mjs`：
  - 新增 `node-ia` 覆盖（`node-config` case 内）：节点页 **没有** `#node-namespace` / `#node-effective-vars`，
    `#node-yaml-text` 是 Node 文档且不含 `status:`；
  - 有效变量断言迁到 `namespace.html`（选 `anode-1`，切 `prod` 后 `NS_ONLY=prod-visible`）；
  - `namespace-system` case：节点页不再有 `#system-config` / `#namespace-vars`；系统保留期在 `admin.html`，
    命名空间 vars 在 `namespace.html`（同一套 409 草稿保留 / 显式重载 / 保存体形状断言）。
- `f3-extra.mjs`：ABA/CAS 用例宿主改 `namespace.html`（`#namespace-*` 锚点不变）。
- `affected-map.mjs`：`frontend.pages` 登记两页（`check-affected-map` 的「PAGES == web-src/pages」棘轮）。

## 4. 验证证据

### 门禁

```
make contract                                 ✅ 131 operations / 120 mux pattern 双向对账
make check-agents                             ✅
make lint-go                                  ✅
make fe                                       ✅ class 869 / i18n 2111 引用点 / sitegen 23 页 × 4 语言 / 指纹
make check-guard-selftest                     ✅
make check-test-discipline                    ✅
make check-annotations                        ✅（unmarked 385 = 基线，未上调）
make check-affected-map                       ✅（PAGES == web-src/pages 覆盖检查绿）
make check-secrets                            ✅
go build ./...                                ✅
go test ./...（除 cmd/anode）                  ✅
```

> 合并冲突：main 期间合入 #437（节点页部署投影按裸 `{deployments}` 读）。冲突只在
> `web-src/pages/node.html` 的 `loadRelated()` —— 本分支把它随 ns 上下文一起删掉了。
> 处理：**保留 #437 的语义**（`A.api` + `A.deploymentsOf`、`?nodeId=` 过滤、裸投影不套
> `*List` 信封），把这两块「已部署能力 / 最近任务」作为**节点事实**恢复，命名空间改为
> 跟随侧栏作用域（不再读 URL 的 `?namespace=`）；main 新增的 `node-deployments` e2e
> 用例因此继续有效。`api/resource.yaml` 与 `openapi_parity_test.go` 的 ratchet 取并集：
> mux pattern 122+2=124、operation 133+2=135。

> 说明：合并 `origin/main` 之前，`cmd/anode` 的四条 install-script 用例在本机失败
> （`origin/main@2c115020` 的干净 worktree 上同样失败，属环境差异）；main 的 #440
> （`fix/node`: 安装脚本 `--prefix` 下不再探测宿主安装）合并进来后，这四条在本机也随
> `make ci` 一起绿了 —— 本轮最终 `make ci` 退出码 0。

### e2e（改宿主 + 新增断言）

- `f3-driver.mjs`：`PAGES` 加 `namespace` / `namespaces`；`MATRIX_BATCHES` 末段改用 `PAGES.length`（避免新页静默漏跑）。
- `f3-ui-helper.mjs`：`PAGES` 同步；`ANCHORS` 加两页。
- `f3-i18n-cases.mjs` / `f3-cases.mjs`：`anchors`、`query(...)`（`namespace` → `name=default`）、
  `pageAnchors.node` 改 `#node-machine-vars`、`pageAnchors` 加两页。
- `f3-node-retained.mjs`：
  - 新增 `node-ia` 覆盖（`node-config` case 内）：节点页 **没有** `#node-namespace` / `#node-effective-vars`，
    `#node-yaml-text` 是 Node 文档且不含 `status:`；
  - 有效变量断言迁到 `namespace.html`（选 `anode-1`，切 `prod` 后 `NS_ONLY=prod-visible`）；
  - `namespace-system` case：节点页不再有 `#system-config` / `#namespace-vars`；系统保留期在 `admin.html`，
    命名空间 vars 在 `namespace.html`（同一套 409 草稿保留 / 显式重载 / 保存体形状断言）。
- `f3-extra.mjs`：ABA/CAS 用例宿主改 `namespace.html`（`#namespace-*` 锚点不变）。
- `affected-map.mjs`：`frontend.pages` 登记两页（`check-affected-map` 的「PAGES == web-src/pages」棘轮）。

## 4. 验证证据

### 门禁

```
make contract                                 ✅ 131 operations / 120 mux pattern 双向对账
make check-agents                             ✅
make lint-go                                  ✅
make fe                                       ✅ class 869 / i18n 2111 引用点 / sitegen 23 页 × 4 语言 / 指纹
make check-guard-selftest                     ✅
make check-test-discipline                    ✅
make check-annotations                        ✅（unmarked 385 = 基线，未上调）
make check-affected-map                       ✅（PAGES == web-src/pages 覆盖检查绿）
make check-secrets                            ✅
go build ./...                                ✅
go test ./...（除 cmd/anode）                  ✅
```

**`cmd/anode` 的四条 install-script 用例在本机失败，且不是本分支引入的**：

```
git worktree add /tmp/main-check origin/main
cd /tmp/main-check
go test ./cmd/anode -run 'TestInstallScript…' -count=1     # 同样四条 FAIL
```

失败点是本机 systemd/sudo 组合（脚本走了 `sudo -n systemctl start` 分支），与节点配置/YAML
无关；其余包与全部 e2e 均绿。

### e2e

| 套件 | 结果 | authority |
| --- | --- | --- |
| `frontend` | 39 通过 / 0 失败 | real |
| `i18n` | 33 通过 / 0 失败 | explicit-mock |
| `i18n_coverage` | 44 通过 / 0 失败 | explicit-mock |
| `ui_shift` | 87 通过 / 0 失败（368 视图） | explicit-mock |
| `capacity` | 84 通过 / 0 失败 | real |
| `ux_regression_19_23` | 8 通过 / 0 失败 | explicit-mock |
| `make e2e`（全量矩阵） | **28 suites / 2287 passing assertions，0 失败**（`E2E_CONCURRENCY=2`） | real 24 / mock 4 |

本机 load average ~14（共享 VM 上还有别的会话/k8s），默认并行度（4）下全量跑出现过
`database is locked` / `ERR_NETWORK_CHANGED` / 证据片段 ENOENT 这类**负载型抖动**：
同一套件单独重跑均 0 失败（`capacity` 84/0、`ui_shift` 87/0、`i18n` 33/0、`frontend` 39/0 各至少一次全绿）。
把并发降到 2（`E2E_CONCURRENCY=2 make e2e`）后全量 28/28 一次全绿 —— 结论：抖动来自本机负载，
不是本轮改动；要复现绿请用同一条命令。

新增/改写的断言（都在 `frontend` = real authority 上跑）：

1. 节点页没有 `#node-namespace` / `#node-effective-vars`（不再有 ns 上下文）。
2. 节点页的 `#node-yaml-text` 是 Node 文档（`apiVersion` / `kind` / `namespaces:`），且**不含** `status:`。
3. 节点列表页没有 `#namespace-vars`（命名空间 vars 不在这里）与 `#system-config`（系统保留期不在这里）。
4. `namespace.html` 能改 vars：真实 PUT 体只含 `{metadata, vars}`、`namespaceVarsRevision` 前进；
   409 时草稿逐字保留、显式重载后才换成远端值。
5. `admin.html` 有 `#system-config`：实际 GET + CAS 写 0（永久）+ 旧 rv 409 草稿保留。

## 5. 已知取舍 / 遗留风险

1. **Node YAML 不含 `metadata.uid`**：写端白名单（JSON 与 YAML 共用）只接受 `name`/`resourceVersion`，
   带上 uid 会破坏「取 → 贴回」的往返。需要 uid 时用 JSON 读面。
2. **mock 预览不支持 YAML 写入**（501 具名拒绝）：解析器只保留服务端一份；真实写入由 `frontend`（real authority）套件覆盖。
3. **`namespace.html` 的有效变量默认选第一台节点**：选择不写进 URL（避免多一个状态源），
   若节点很多，后续 ③ 可加「按节点名字深链」。
4. **i18n 键名沿用 `nodes.v4.stale/loaded/...`**：命名空间页与节点页共用同一批 CAS 文案（同一套语义、避免译文漂移）；
   键名的 IA 重命名留到 ③ 一并做，避免本轮再动一批 locale 键。
5. ③（Namespace YAML 面）与 ④ 的剩余部分（例如系统 YAML 文档）未实现，设计 §6 已给出可独立合并的边界。

## 6. PR 与顺序

| 顺序 | 提交 | 内容 | 状态 |
| --- | --- | --- | --- |
| 1 | `0661d2ab` docs(design) | 设计文档（页面地图 / YAML 契约 / 拆分） | ✅ |
| 2 | `29069ef6` feat(server) | ② 的读写面 + OpenAPI + 测试 | ✅ |
| 3 | `1312cbc1` feat(web) | ① 的页面/导航/i18n/mock/e2e + ② 的 YAML 面板 | ✅ |
| 4 | `c7e5170f` docs(report) | 本报告 | ✅ |

PR [#441](https://github.com/zzjcool/anotify/pull/441) 覆盖 ①②；③（Namespace YAML 面）与 ④ 的剩余部分
按设计 §6 另开 PR。

> ① 与 ② 在同一个 PR 落地：两者改的是同一张卡片（`node.html` 的配置区）——先删 ns 上下文、
> 再加 YAML 面板如果拆成两个 PR，第二个 PR 会把第一个 PR 刚写的卡片整块重写。提交仍按
> 「server / web」分开，便于 review 与日后再拆。
