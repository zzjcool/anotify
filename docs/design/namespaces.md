# 命名空间（namespace）

> 状态：**设计已定，等待实现**。本文是 `docs/contracts/CONTRACT.md` 的补充章节，
> 冲突时以 CONTRACT 为准（CONTRACT 已同步收录本文的结论）。
>
> 一句话：**组织 → 命名空间 → 配置与运行时数据。**
> 命名空间是「配置面 + 运行面」的隔离单位；**能力和节点仍是组织级共享资源池**。

---

## 1. 为什么需要它

现在只有一层边界：`org_id`（不变量 I10）。一个组织里所有人共用一张 `edges` 表、
一条事件流、一个 trace 列表。这在一人一套流程时没问题，一旦组织里存在**多套互不相干的流程**就塌了：

| 场景 | 只有 org 时的后果 |
|---|---|
| 生产 / 预发两套流程 | 在预发配一条边，生产也会派发。**没有地方安放「不同的图」** |
| 给某个客户单独一套 webhook | 只能新建一个 org，然后把节点重新注册一遍（节点是 org 级的） |
| 重放一次测试事件 | 污染生产的 trace 列表与统计 |
| 做一次危险改动（删一堆边） | 影响面是整个组织，没有「只在这个环境里试」的粒度 |

根因是同一个：**`edges` / `sources` / `events` / `tasks` 缺少一个「这套东西属于哪套流程」的维度**。

命名空间就是那个维度。

---

## 2. 边界：隔离什么、不隔离什么

这一节是本文最重要的部分。命名空间**不是**「第二层租户」，它只隔离「配置 + 运行数据」，
不隔离「定义」和「机器」。

| 对象 | 是否隔离 | 理由 |
|---|---|---|
| `edges`（路由表） | ✅ | 路由必须封闭在命名空间内，否则预发的图能把任务派到生产的拓扑上 |
| `sources` / `source_targets` | ✅ | 入口天然按环境分（不同 webhook、不同 cron） |
| `events` / `tasks` | ✅ | 数据面隔离：trace、统计、审计、配额都要能按环境切 |
| `graph_versions`（图快照） | ✅ | 快照回答「这套图当时长什么样」，图是命名空间内的 |
| `capability_versions` | ❌ 组织级 | 见 §2.1 |
| `nodes` / `node_instances` | ❌ 组织级 | 见 §2.2 |
| `users` / `org_members` | ❌ 组织级 | 身份不属于任何命名空间 |
| API key / CLI token | ⭕ 可选绑定 | 见 §2.3 |

### 2.1 能力定义为什么保持组织级

三个理由，按重要性排序：

1. **节点协议不用改。** `hello` 帧里节点声明 `review@1`。如果能力按命名空间划分，
   同一个 `review@1` 会在 N 个命名空间里指向 N 个不同定义 —— 节点要么必须带上
   命名空间（等于改协议 + 所有已部署 agent 都要升级），要么中枢只能猜。**歧义就是漏洞。**
2. **`I2`（能力必须先定义、出口是封闭集合）依赖名字在组织内唯一。** 一旦同名多定义，
   `edges.from_port` 的封闭性要按命名空间再乘一层，校验路径翻倍而收益不明。
3. **真有「两套同名能力」的需求时，命名约定更便宜。** `team-a-lint@1` + append-only
   版本已经能表达，代价远低于改节点协议。

**扩展点（明确留了，但本期不做）**：给 `capability_versions` 加 `namespace_id`，
把 `UNIQUE (org_id, name, version)` 改成 `UNIQUE (org_id, namespace_id, name, version)`，
同时 `hello` 帧带命名空间列表。这是一次**破坏性升级**，必须先有节点侧配合。

### 2.2 节点为什么保持组织级

节点是**机器池**。一台机器装了一堆 runner，声明自己能跑哪些能力，价值恰恰是
**跨项目共享**——同一个池子既跑生产的部署、也跑预发的测试。按命名空间切节点，
等于每台机器要为每个命名空间重新注册一次，池子就没了。

**已知限制（写下来，别假装不存在）**：今天**无法**把某个命名空间的活固定到某批机器上。
派发不读 `nodes.tags`（`tags` 目前只是展示/筛选用的元数据）。

将来要做「环境隔离到机器」，最短路径**不是**命名空间级绑定，而是
**能力定义加 `selector`**（例如 `"env=prod"`），派发时按 `nodes.tags` 过滤。
这是能力级选择器，粒度比命名空间更细，且不需要动节点协议。

### 2.3 密钥可以绑定命名空间

API key / CLI token 仍属组织，但可以携带 `namespace_id`：

- **不带** `namespace_id` = 全组织（老行为，管理员用）
- **带** = 只能读写该命名空间的资源；碰别的命名空间一律 403

这样「给 CI 一把只能动预发的 key」可以做到，不需要给它建一个 org。

---

## 3. 数据模型

```sql
CREATE TABLE IF NOT EXISTS namespaces (
    id         TEXT PRIMARY KEY,              -- ns_<base62>
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,                 -- prod / staging / team-a（URL 里用，稳定）
    name       TEXT NOT NULL,                 -- 展示名（可改）
    created_at INTEGER NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_namespaces_org ON namespaces(org_id, created_at);
```

**所有被隔离的表加一列**，并把唯一约束/索引的主键前缀加上它。

```sql
-- edges：主键从 (org_id, ...) 变成 (namespace_id, ...)
ALTER TABLE edges ADD COLUMN namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE;
-- PRIMARY KEY (namespace_id, from_capability, from_port, to_capability)
CREATE INDEX idx_edges_lookup ON edges(namespace_id, from_capability, from_port);

-- sources / events / tasks / graph_versions 同样加 namespace_id
```

`org_id` **仍然保留**（不变量 I10 的粗过滤，也避免每次都要 join `namespaces`）。
两个维度是包含关系：`namespace.org_id` 就是这一行的 `org_id`，写入时由中枢校验，
不允许两者不一致。

### 3.1 默认命名空间

**首次注册自动建组织时，同一个事务里建 `default` 命名空间**（slug `default`，名「默认」）。
和 `org` 一样：**代码里不存在「没有命名空间」的分支。**

不允许删除最后一个命名空间（`409`）；删 `default` 可以，但必须先有别的命名空间。

### 3.2 删除语义

`DELETE /v1/namespaces/{id}`：

- 命名空间里**还有** sources / edges / tasks 时 → `409`，列出阻塞对象
- `?force=true` → 级联删除（走 `ON DELETE CASCADE`），并写一条审计事件
- 不允许删最后一个命名空间

---

## 4. 不变量修订与新增

| # | 内容 | 变更 |
|---|---|---|
| **I10** | 一切租户数据先过 `org_id` | **修订**：命名空间资源还必须过 `namespace_id`，且 `namespace_id` 必须属于同一个 `org_id` |
| **I11**（新） | **跨命名空间不引用** | 新增 |

**I11 的含义**（三处必须同时成立，缺一处就是洞）：

1. `edges` 两端的能力虽然组织级，但**边本身属于一个命名空间**；
   `(from_capability, from_port)` 的查表**只在创建任务所在的命名空间内进行**。
2. `source_targets` 的目标能力可以在任何命名空间定义，但**源只能派给同命名空间的任务**。
3. **子任务继承父任务的 `namespace_id`**，不接受调用方指定。

违反 I11 的请求一律 `400`（不是静默忽略 —— 静默忽略会让人以为配置生效了）。

---

## 5. 派发四步的改动（最小）

`route.Dispatch` 的四步不变，只是查表的输入多一个 `namespace_id`：

```
输入: { orgID, namespaceID, fromCapability, fromPort, params, parentTaskID }
 1. EdgesFrom(namespaceID, fromCapability, fromPort)   ← 唯一改动：调用带命名空间
 2. 校验 params（对着目标能力定义的 params_schema）
 3. 找节点（保持不变：声明了该能力的节点里按最少负载选）
 4. 建任务，namespace_id = 入参（根任务）或父任务的 namespace_id（子任务）
```

源触发的路径同理：`source.namespace_id` 就是这次派发的命名空间。

**剪枝/路径（`tasks.path`）不用改**：`path` 里是能力 id，能力是组织级的，
不会跨命名空间混淆；命名空间已经由 `namespace_id` 独立表达了。

---

## 6. HTTP API

### 6.1 命名空间自身

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/namespaces` | 列出本组织的命名空间 |
| `POST` | `/v1/namespaces` | `{slug, name}` |
| `PATCH` | `/v1/namespaces/{id}` | 只允许改 `name`（`slug` 是稳定标识，不可改） |
| `DELETE` | `/v1/namespaces/{id}` | 见 §3.2 |

### 6.2 资源端点怎么带命名空间

**显式，或者显式全部。永远没有隐式默认。**

| 端点 | `namespaceId` |
|---|---|
| `/v1/edges`、`/v1/sources`、`/v1/events`、`/v1/tasks`、`/v1/traces/*`、`/v1/stats` | **必填** |
| 同上，但要跨命名空间汇总（管理员视图） | `namespaceId=*`（显式通配） |
| `/v1/capabilities`、`/v1/nodes`、`/v1/nodes/{id}`、`/v1/admin/*`、`/v1/auth/*` | **不需要**（组织级） |

错误码：

- 缺 `namespaceId` → `400`（**不**回退到默认命名空间：回退等于把人写错的地方藏起来）
- `namespaceId` 不存在 → `404`
- `namespaceId` 属于别的 org → `403`
- 写操作跨命名空间引用 → `400`（I11）

响应里所有命名空间资源带 `namespaceId` 字段；`/v1/namespaces` 之外的对象
（能力、节点）**不带**，也别加 —— 它们是组织级的，加了会诱导前端按命名空间过滤。

### 6.3 与 openapi 的关系

`scripts/check_openapi.py` 要求 `api/openapi.yaml` 与 `internal/server/mux.go`
**双向一致**。所以本轮（只有设计 + mock）**不动 openapi**：
先加 spec 而没有 handler 会直接让 `make contract` 失败。

实现落地时按此顺序：mux → handler → openapi → `make contract`。

---

## 7. UI

- **侧栏顶部**（logo 下）放两个切换器：**组织** → **命名空间**。
  状态存 `localStorage`，切换后整页重取。
- 命名空间切换器右侧一个 `+`，直接建新命名空间（`slug` 必填，`name` 默认同 `slug`）。
- **组织级页面要标出来**：能力页、节点页顶部显示「组织级 · 不随命名空间切换」。
  否则用户切了命名空间发现能力列表没变，会以为功能坏了。
- 事件页 / 链路页 / 源页 / 边页：标题旁边显示当前命名空间 slug。

---

## 8. 明确不做（本期）

| 不做 | 为什么 |
|---|---|
| 命名空间级 RBAC（谁能看哪个 ns） | 现在是组织级角色；做之前先把 org 级角色补齐更划算 |
| 命名空间级配额（max_tasks 等） | 六道闸 G1~G6 目前是任务级/图级；配额是另一套机制 |
| 按命名空间绑定机器 | 见 §2.2，正确解法是能力选择器 |
| 跨命名空间复制图（「把预发的图 promote 到生产」） | 有价值，但需要先有「图导出/导入」；不属于隔离机制本身 |
| 命名空间重命名 slug | `slug` 出现在 URL 和 key 作用域里，改了会让旧链接失效；只改 `name` |

---

## 9. 开放问题

1. **`namespaceId=*` 的鉴权**：管理员视图要 `admin`/`owner` 角色吗？（倾向：要）
2. **事件中心的默认视图**：登录后落在 `default` 还是「上次用的」？（倾向：上次用的，存 localStorage；服务端不记）
3. **`graph_versions` 的 hash 是否要含 `namespace_id`**：倾向**不含** —— 快照内容就是 edges 列表，命名空间已经由 `namespace_id` 列表达，混进 hash 会让「同一张图在两个命名空间」变成两条不同快照，浪费且语义混乱。
