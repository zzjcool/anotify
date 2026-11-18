# 节点 / 命名空间 / 系统的信息架构（node-namespace-ia）

> 状态：**设计 + 分阶段实施**（本轮落地 ① 与 ②，③④ 见 §6）
> 契约真源：`docs/design/v4/decisions.md` > `internal/store/AGENTS.md` / `internal/server/AGENTS.md` > 本文。
> 本文写的是「配置归谁、页面在哪、YAML 长什么样」，不改 `Node` / `Namespace` 的资源定义本身。
>
> 起源：四条用户反馈（§0.1）。它们指向同一个错位——**命名空间级的配置被放在了节点页上**。

---

## 0. 现状事实（先写清「配置存在哪、被谁读」）

### 0.0 四条用户反馈（需求源，逐条回应见 §1 / §3）

1. `nodes.html` 上有「命名空间 vars 配置（无 limits）」和「系统保留配置（管理员）」——那不是命名空间级
   / 系统级的配置吗？为什么在节点下面？
2. 命名空间应该单独有管理页面，这类配置放到那个页面。
3. `node.html?id=hw&namespace=default` 应该是「某个节点的具体配置」；节点是**节点维度**管理。
   唯一跟命名空间有关的是「这个节点能让哪些命名空间看到/使用」。节点配置不需要为每个命名空间再加一份。
4. 这一切应抽象成 YAML：节点配置 + 允许的命名空间都在 Node YAML 上声明；控制台能直接看到 YAML；
   UI 主要对 YAML 做可视化编辑，也能直接查看。

### 0.1 配置实际落在哪张表（数据源唯一，不允许 UI 自造第二份）

| 配置 | 表 / 列 | 读它的 API | 谁是权威读者 |
| --- | --- | --- | --- |
| 节点授权命名空间 | `node_configs.namespaces`（缺省 `["*"]`，`internal/store/schema.sql:202-204`） | `GET/PUT /v1/nodes/{name}/config`、`GET/PATCH /v1/nodes/{name}`（`spec.namespaces`） | **派发闸门**：`internal/store/execution_auth.go:75-95`、`internal/store/emission.go:139,404-410`；**节点会话与产物**：`internal/ws/hub.go:372,642`、`internal/server/node_capability.go:233`；`POST /v1/nodes/config/batch` |
| 节点默认 vars / limits | `node_configs.vars` / `.limits`（同上） | 同上 + `GET /v1/nodes/{name}` 的 `status.nodeVars` | 配置帧下发（`internal/ws/hub.go:368`）、`machineVars` 投影 |
| 节点本机快照（只读） | `node_configs.local_vars` / `.local_limits`、`applied_*`、`nodes.declared_namespaces` | `GET /v1/nodes/{name}` 的 `status.localVars` / `machineVars` / `effectiveNamespaces` | 只做匹配与展示，不参与 apply |
| 命名空间默认 vars | `namespace_configs.vars`（`schema.sql:149-156`） | `GET/PUT /v1/namespaces/{name}/config` | 变量解析的第三层（`local > node > namespace`） |
| 系统保留期 | `system_config.retention_*`（`schema.sql:158-165`） | `GET/PUT /v1/system/config` | 保留期后台循环（`internal/server/mux.go:601-692`） |

**命名空间没有 limits**：`namespace_configs` 只有 `vars` / `revision` / `resource_version`，
schema 里不存在 ns 级配额；`limits` 只有「本机 + 节点」两层（`internal/store/config.go:194-228`
明确「它没有 ns 级配额」）。所以命名空间页本轮只有 **vars + revisions + 计数**，
「命名空间 limits」是**未批准的功能**，不在本文伪造一个空编辑器。

### 0.2 `effectiveNamespaces` 是「授权」还是「可见」——是授权

`store.AuthorizedNamespaces` = `node_configs.namespaces`（中枢授权，缺省 `["*"]`）
**∩** `nodes.declared_namespaces`（本机申报），见 `internal/store/config.go:1359-1417`。

它不是「界面上能看到什么」，而是**硬闸门**：

- 派发：任务的 ns 必须在这台节点的生效列表里，否则不派（`store/execution_auth.go`、`store/emission.go`）；
- 产物：节点取能力包时按生效 ns 逐个查（`internal/server/node_capability.go:220-240`）；
- 会话：`hello` / 配置帧按生效 ns 过滤（`internal/ws/hub.go:372,642`）。

所以在页面上：`spec.namespaces` 是**可写的授权列表（唯一来源）**，`status.effectiveNamespaces`
是**只读的生效列表**（= 授权 ∩ 本机申报）。两者必须同时出现，否则运维会以为「我授权了却没生效」。

### 0.3 页面现状（错位在哪一行）

| 页面 | 现状 | 问题 |
| --- | --- | --- |
| `web-src/pages/nodes.html:142-151` | `#namespace-config`：`select#config-namespace` + `#namespace-vars` + `#namespace-save` | **命名空间级**配置挂在节点列表页下 |
| `web-src/pages/nodes.html:153-166` | `#system-config`：保留期三个 TTL 输入 | **系统级**配置挂在节点页下，且只有 admin 能用 |
| `web-src/pages/nodes.html:14` | 注册面板产出 `anode.yaml` 片段（`#reg-yaml`，`nodes.html:161-163`） | 唯一「YAML」痕迹，且只在注册那一刻出现 |
| `web-src/pages/node.html:135-136` | `select#node-namespace` + `#node-effective-vars` + `#node-context-revision` | 节点页带 **ns 上下文**（`?namespace=`），把「某 ns 的有效值」混进节点维度 |
| `web-src/pages/node.html:139-140` | `#node-deployments` / `#node-tasks`（按 ns 查的列表） | 同样是 ns 维度视图（`/v1/namespaces/{ns}/deployments`） |
| `web-src/pages/node.html:118-124` | `#node-namespaces` / `#node-vars` / `#node-limits`（三个原始 JSON textarea，藏在 `#node-config-advanced`） | 可写，但**没有 YAML**，也没有把「授权」讲清楚 |

### 0.4 i18n 与 e2e 现状（改页面必须同步的地方）

- i18n：`nodes.v4.*`（`web-src/locales/zh-CN.yaml:1678-1747`，四份语言）、`uxaudit.res.nodes.*`
  （`:2355-2370`）、`uxaudit.dev.*`（`:2462-2482`）。四份 locale 的 key 集合必须完全相等
  （`scripts/check-i18n-keys.mjs:493`），缺 key 只 WARN（`internal/sitegen/sitegen.go:347-372`），
  所以门禁靠 `make check-i18n-keys`。
- e2e 断言**不在** `suites/*.mjs`，全在 `scripts/e2e/fixtures/f3/`：
  - `f3-node-retained.mjs`：`node-create`（`#reg-*`）、`node-config`（`#node-config-*` / `#node-vars` /
    `#node-effective-vars` / `#node-namespace`）、`node-namespace-system`（`#namespace-*` + `#system-*`）；
  - `f3-extra.mjs`：`node-aba`（`#namespace-vars` / `#namespace-save` / `#config-namespace` 的 ABA/CAS 语义）；
  - 页面结构锚点三处副本：`f3-cases.mjs:299`（`node: '#node-effective-vars [data-key]'`）、
    `f3-i18n-cases.mjs:11-14`、`f3-ui-helper.mjs:26-37`；
  - 页面矩阵 `PAGES`：`f3-driver.mjs:20`（新页面必须登记，否则 i18n / ui_shift 覆盖不到）。

---

## 1. 页面地图（改完之后的 IA）

```
nodes.html                     节点列表（cluster）—— 只有节点
  ├─ 注册节点（#register-panel / #reg-token / #reg-yaml）
  └─ 行 → node.html?id=<name>

node.html?id=<name>            节点维度（无 namespace 参数）—— 只有这台机器
  ├─ 身份 / 连接（hostname / os / arch / capacity / online / draining / agentVersion）
  ├─ 配置（YAML 主视图 + 表单可视化编辑）
  │    ├─ spec.namespaces      授权：这个节点能被哪些命名空间使用（唯一来源）
  │    ├─ spec.vars            节点默认 vars（机器级，跨 ns 共用）
  │    ├─ spec.limits          total / perCapability
  │    └─ spec.tags
  ├─ 生效命名空间（只读）= 授权 ∩ 本机申报（用于排查「授权了却没生效」）
  ├─ 机器实际值（只读）：localVars / nodeVars / machineVars / localLimits / machineLimits
  ├─ 槽位与动作（drain / upgrade / rotate）
  └─ 指向：namespace.html?name=<ns>（某 ns 的变量）、capabilities.html

namespaces.html                命名空间列表（cluster）
  └─ 行 → namespace.html?name=<slug>

namespace.html?name=<slug>     命名空间维度
  ├─ 身份（metadata.name / displayName / uid / creationTimestamp）
  ├─ 计数（status.counts：capabilities / nodes / sources / tasks）
  ├─ vars（#namespace-vars，CAS 保存）+ revisions（#namespace-revisions）
  ├─ 逐节点有效变量：选一台节点 → status.namespaceVars / status.effectiveVars（local > node > namespace）
  ├─ 作用域切换（select#config-namespace，改 URL 的 name，不重新导航；ABA 判据不变）
  └─ 指向：该 ns 的能力 / 源 / 事件

admin.html（系统区块）          系统保留配置（管理员）
  └─ #system-config：retention.{event,task,result}TtlSec（CAS 保存）
```

规则：

1. **节点页不带 `namespace` 参数**。旧的 `node.html?id=hw&namespace=default` 仍可打开，
   `namespace` 参数被**忽略**（不 302、不报错：书签和旧链接必须继续工作）。
2. 节点页**不出现**命名空间 vars 编辑器，也不出现系统保留期——这两块各自归位（见上）。
3. 节点页与命名空间页之间用**链接**表达关系，不复用同一个编辑面板：
   「这台机器在 ns X 下的有效变量」= `namespace.html?name=X` 里选这台节点（本轮的 ① 已落地；
   ③ 再补 Namespace YAML 与更细的上下文视图）。
4. 侧栏新增 `namespaces` 一项（`web/partials.js` 的 `NAV`），`active: 'namespaces'`。
5. `nodes.html` 保留 `#reg-yaml`（注册时给出 `anode.yaml` 片段）：那是**本机文件**，
   与中枢的 Node 文档是两份不同用途的 YAML（§2.4）。

---

## 2. YAML 契约

### 2.1 Node 文档（中枢权威，`GET /v1/nodes/{name}/yaml`）

```yaml
apiVersion: anotify/v1
kind: Node
metadata:
  name: hw
  resourceVersion: rv1:node_01H...:7   # 只读；但它是写回时的 CAS 令牌
spec:
  namespaces: [default]         # 可写：授权（唯一来源；缺省 ["*"]）
  vars:                         # 可写：节点默认值（机器级，跨 ns 共用）
    TOOLCHAIN: /opt/go1.25
  limits:                       # 可写：total 未写 = 不压住下层；perCapability 逐能力
    total: 4
    perCapability:
      "*": 2
      fixer: 1
  tags: [build, prod]           # 可写
```

- 这就是 `docs/design/v4/api-conventions.md §7.4` 的 `Node` 文档去掉 `status` 与 `metadata.uid`。
  **不新增字段、不改语义**：YAML 只是同一份事实的另一种序列化。
- 为什么连 `uid` 也不进 YAML：写入口的 `metadata` 白名单只有 `name` / `resourceVersion`
  （JSON 与 YAML 共用同一份 `writeMetaAllowList`）。带上 `uid` 会让「取 → 贴回」在写入口
  撞上只读判定，那样这份文档就**不是**可贴回的文档。uid 在不透明的内部身份里，编辑用不到；
  需要它时用 JSON 读面（`GET /v1/nodes/{name}`）。
- `status` **不进 YAML**：它是派生只读面（online / appliedRevision / machineVars / instances…），
  放进可编辑文档只会让人以为能改。想看状态事实用 JSON 读面（页面上就是那一圈只读卡片）。
- `metadata.resourceVersion` 是写回的 CAS 令牌（`PUT` 时必填，缺失 400、旧值 409）。

### 2.2 Namespace 文档（`GET /v1/namespaces/{name}/yaml`，③ 落地）

```yaml
apiVersion: anotify/v1
kind: Namespace
metadata:
  name: default
  displayName: 默认命名空间
  uid: ns_01H...                # 只读
  resourceVersion: rv1:ns_01H...:3
spec: {}                        # 无写开关（未批准功能）
status:                         # 只读（counts / stats）
  counts: {capabilities: 12, nodes: 3, sources: 4, tasks: 91}
```

命名空间 vars 不是 `spec` 字段而是配置子资源（`api-conventions.md §7.5` 冻结的 wrapper 形状）。
③ 的 Namespace YAML **只读**：写 vars 仍走 `PUT /v1/namespaces/{name}/config`（CAS + revision 语义
与现有 UI 一致），YAML 里以注释标明「vars 见配置子资源」——不发明第二种写入口。

### 2.3 与 `/etc/anode/anode.yaml` 的字段对齐（重要边界）

`anode.yaml` 是本机文件（`cmd/anode/internal/config/config.go:52-70`），**不是**中枢资源：

| anode.yaml 字段 | 中枢 Node 文档 | 理由 |
| --- | --- | --- |
| `name` | `metadata.name` | 同一个身份（集群内唯一） |
| `namespaces` | `spec.namespaces` | 本机**申报**；中枢侧是**授权**，生效 = 交集 |
| `vars` / `limits` | `spec.vars` / `spec.limits` | 中枢是默认值，本机同键优先（`local > node`） |
| `server` | **不进 spec** | `api-conventions.md §7.4`：`server/token/listen/envPassthrough` 是 anode.yaml 私有字段 |
| `token` | **永不出现** | 明文只在 `POST /v1/nodes/{name}/tokens` 出现一次；库里只有 sha256 |
| `listen` / `envPassthrough` | **不进 spec** | 名字与值都不上报（`internal/protocol/config_frames.go:15`） |

所以本任务的示例 YAML 里那三个字段（`server` / `listen` / `envPassthrough`）在**中枢 Node 文档里
刻意不存在**：中枢拿不到它们（协议规定不上报），写进去就只能编造。用户要的「一切抽象成 YAML」
仍然成立——YAML 是中枢侧配置的单一文档；本机文件继续保持 `anode.yaml` 的既有形状，
两者的公共字段（name / namespaces / vars / limits）逐字同名。

### 2.4 三种 YAML 各是什么（别混）

| YAML | 谁写 | 位置 | 用途 |
| --- | --- | --- | --- |
| Node 文档 | 中枢（配置 + spec） | `GET/PUT /v1/nodes/{name}/yaml` | 控制台查看 / 可视化编辑 / 贴回 |
| `anode.yaml` | 运维（本机） | `/etc/anode/anode.yaml` | 节点启动配置（含 token） |
| 注册片段 | 控制台生成（`#reg-yaml`） | `nodes.html` 注册结果 | 首次安装时粘进 `anode.yaml` |

---

## 3. UI 与 YAML

### 3.1 查看 = 服务端渲染的规范化 YAML（只读）

`GET /v1/nodes/{name}/yaml` 返回 `{"yaml": "<全文>"}`（复用 `yamlDoc` / `ResourceYAML`，
与 `GET …/capabilities/{name}/yaml` 同 shape）。服务端从 store 事实重建，**不是**用户当初贴的原始字节：
键序与缺省由序列化固定，所以「取 → 改 → 贴回」是一次 schema 复用的往返。
页面把它放进 `.code-block`（等宽 + 一键复制），并在同一张卡上给出可编辑 textarea。

### 3.2 编辑 = 表单 ↔ YAML 双向同步，解析只在服务端

- **form → YAML**：前端只做**序列化**（把当前草稿结构写成 YAML 文本），不做解析。
- **YAML → form**：不做前端解析。`PUT /v1/nodes/{name}/yaml` 由服务端用
  `internal/yamldoc.Decode`（严格解析器：未知字段 / 类型 / 必填 / 重复键 / 多文档 / 锚点别名
  全部拒绝）转成 canonical JSON，再走**与 `PATCH /v1/nodes/{name}` 完全同一条** store 路径
  （`UpdateNode`，同一 CAS、同一 revision 语义），成功后返回重新规范化的 `{yaml}`；页面拿返回值
  回填表单与 YAML 视图。**前端不实现任何宽松解析器**（这是 #capability-yaml 已经立下的规矩）。
- **错误**：400 + `details.causes[]`，`reason` ∈ `invalid_yaml | unknown_field | value_invalid`，
  `message` 带「第 N 行：…」，`field` 是点号路径（`spec.limits.total` / `spec.vars.FOO`）。
  页面逐条渲染成「行号 + 字段路径 + 原因」，并把行号定位到 textarea（不是只弹一句 toast）。
- **只读字段**：`status`、`metadata.uid`、`metadata.creationTimestamp` 写了 → 400
  `status_not_writable` / `unknown_field`（复用既有写入口的判定），不静默忽略。
  `metadata.resourceVersion` 必须与库里一致，否则 409 `resource_version_conflict`
  （`details.current.resourceVersion` 给当前令牌）。
- 现有 `#node-config-advanced` 里的三块 JSON textarea **保留**（它们是同一份 spec 的紧凑编辑面，
  也是 e2e 的稳定锚点），YAML 面板是新增的**主视图**；两者改动同一份草稿并同步。

### 3.3 为什么不做 `dryRun`

写路径与 `PATCH` 完全一致，校验失败在 400 里已经带行号与字段路径；再加一个
`?dryRun=true` 只是同一份判据的第二次入口（多一条要同步维护的路由与契约），本阶段不做。
需要的话按 `POST …/playbooks/{slug}/install` 的既有 `dryRun` 惯例补，不改本文的判据。

---

## 4. API 变化

### 4.1 新增（②）

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| `GET` | `/v1/nodes/{name}/yaml` | 规范化 Node 文档文本 `{"yaml": "…"}`；401/404/500 |
| `PUT` | `/v1/nodes/{name}/yaml` | 严格解析 → 与 `PATCH` 同一条 store 路径；400/401/404/409/500 |

- 路由挂在 `internal/server/resource_node.go` 的 `register`（Node 资源域，cluster 级，只包 `session`）。
- 请求体：`{"yaml": "…"}`（`ResourceYAML`），上限 2 MiB（`yamlDocTextLimit`），与能力/源同口径。
- `spec` 的键集与 `NodeSpec` 一致（`namespaces` / `vars` / `limits` / `tags`）；
  `metadata` 只允许 `name` / `resourceVersion`。

### 4.2 不变 / 明确不改

- **节点读写不以 namespace 为路径参数**（现状已经如此：`/v1/nodes/{name}`）。
  `GET /v1/nodes/{name}?namespace=X` 的「ns 上下文读」保留在 API 上（`api-conventions.md §7.4`
  的冻结能力，任务/调试仍需要），但**控制台不再用**：节点页不传 `namespace`。
- `/v1/nodes/{name}/config`、`/v1/nodes/config/batch`、`/v1/namespaces/{name}/config`、
  `/v1/system/config` 形状全部不动（只是前端的**归属**变了）。
- 旧 URL 兼容：`node.html?id=x&namespace=y` 忽略 `namespace`（页面不再读它）；
  `nodes.html?namespace=y` 忽略（列表页本来就是 cluster 级）。不新增重定向中间件。
- 不做 `?namespace` 的服务端弃用：它是 k8s 风格的上下文读，不是 IA 错误的载体。

### 4.3 数据迁移：**没有**

配置本来就是一份（`node_configs` / `namespace_configs` / `system_config`），
这次只改「谁在哪个页面编它」和「Node 文档的序列化出口」。不动 schema、不搬行、不双写。
`make ci` 的 `check_contract`（schema 不变量）因此不受影响。

---

## 5. 验收判据（每步都要能机械检查）

1. `nodes.html` 渲染后：`#namespace-config`、`#system-config` **不存在**（DOM 查询为空）；
   节点列表、注册面板、`#reg-yaml` 仍在。
2. `node.html?id=hw`：URL 不带 `namespace` 时也正常渲染；`#node-yaml`（只读文本）非空且含
   `kind: Node` 与 `namespaces:`；`#node-namespace` / `#node-effective-vars` 不存在。
3. `namespace.html?name=default`：`#namespace-vars` 可编辑 + 保存（CAS）；锚点与旧用例同名，
   所以「能改 vars」这件事有真实断言（含 409 冲突保留草稿）。
4. `admin.html`：`#system-config` 存在，非 admin 仍显示「仅管理员」。
5. `make ci` 绿；`node scripts/e2e/affected.mjs --run-e2e`（或 `make e2e-affected`）绿。
6. i18n：四份 locale 键集合相等，页面无裸 key（`make check-i18n-keys` + f3 `i18n` 套件）。

---

## 6. 拆分（三到四个可独立合并的 PR）

### ① `refactor(web): 节点页只留节点，命名空间与系统配置各自归位`

- `nodes.html`：删 `#namespace-config` / `#system-config`（含 JS），加一行「已迁移」说明 + 链接。
- 新增 `namespaces.html`（列表）与 `namespace.html?name=`（vars + revisions），
  `#namespace-*` 的 id 与 CAS/ABA 逻辑原样搬过去（**不重写逻辑，只换宿主**）。
- `node.html`：删 `namespace` 参数依赖、`#node-namespace`、`#node-effective-vars`、
  `#node-context-revision`、`#node-deployments`、`#node-tasks`；`effectiveNamespaces` 以只读
  「生效命名空间」呈现（授权 ∩ 申报）；加指向 `namespace.html` 的链接。
- `admin.html`：新增系统保留期区块（`#system-config` 及其全部 id 原样迁入）。
- 侧栏 + locales ×4 + `web/mock.js`（按需）+ f3 fixtures（锚点三处副本、`PAGES`、
  `node-namespace-system` / `node-aba` 改宿主）+ 新增 `node-ia` 用例。
- 验收：§5 的 1 / 3 / 4 + 门禁全绿。

### ② `feat(server): Node YAML 查看/编辑（GET/PUT /v1/nodes/{name}/yaml）`

- `internal/server/resource_node_yaml.go`（新）+ `resource_node.go` 注册两行 + 单测
  （往返、未知字段 400 带行号、只读字段拒绝、CAS 409、name 不匹配）。
- `api/resource.yaml`：两条 operation（复用 `ResourceYAML` / `Status400|401|404|409|500`）。
- `node.html`：YAML 主视图（只读规范化文本 + 可编辑 textarea + `details.causes[]` 逐条错误 +
  表单 ↔ YAML 同步），mock 补 `GET/PUT /v1/nodes/{name}/yaml`。
- 验收：§5 的 2 + `TestOpenAPIParity*` + `go test ./internal/server/...` 绿。

### ③ `feat(web): 命名空间详情页补齐（上下文与 YAML 只读）`

- `Namespace` YAML（只读，vars 以注释指路）+ `GET /v1/namespaces/{name}/yaml`（若做）。
- 从节点页迁来的「某 ns 的有效变量（逐节点）」视图：`status.effectiveVars` 的调用落在 ns 页，
  选节点而不是选 ns。
- 验收：namespace 页能看 revisions / counts / 每节点有效值；e2e 覆盖。

### ④ `refactor(web): 系统配置成区块（或 system.html），并给 System YAML`

- 若 ① 已把 `#system-config` 放进 admin，则 ④ 只做「系统 YAML + 权限态文案 + 保留期校验提示」
  与 `admin.html` 的分组结构；否则 ④ 是整块迁位。
- 验收：admin 页系统区块、非 admin 403 提示、YAML 只读视图。

> 详细 PR 顺序、编号与证据见 `docs/reports/node-namespace-ia.md`。
