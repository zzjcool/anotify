# 命名空间（namespace）

> 状态：**已实现**（命名空间隔离自 #162 起；组织层已于 #204 / 迁移 14 **删除**）。
> 本文是 `docs/contracts/CONTRACT.md` 的补充章节，冲突时以 CONTRACT 为准。
>
> 一句话：**命名空间 → 全部资源。**
> 命名空间是**唯一的硬隔离边界**：能力、节点、源、边、事件、任务、图快照全部属于某个命名空间。
> 这之外只剩「身份」（`users`）和「密钥」（`api_keys`），它们是**整个部署**级的。
>
> 本文早期版本描述的是「组织 → 命名空间」两级模型，那一层的删除理由见 §0。

---

## 0. 为什么没有「组织」层

早期模型是两级：`orgs` → `namespaces` → 资源，每个用户注册时自动建一个组织。
**迁移 14（#204）把它删掉了**，理由是可验证的：

| 事实 | 后果 |
|---|---|
| `/v1/orgs` 端点**从未存在** | 用户看不到、也选不了组织 |
| `org_members.role`（owner/admin/member）**从未参与鉴权** | 角色矩阵是空的 |
| 每个用户注册即建一个组织，且**没有加成员的端点** | 一个组织永远只有一个人 |
| `org_id` 出现在 13 张表、445 处代码里 | 纯粹是「每查一次多加一个恒等条件」 |

结论：组织层只是**把所有查询锁在同一个人身上**的栅栏，而命名空间已经足够表达
「同一个部署内的多套流程」。于是：

```
删除：orgs、org_members 两张表 + 13 张表的 org_id + ?orgId= / X-Org-Id / resolveOrg
保留：users（全局身份）、api_keys（部署级密钥）、namespaces（唯一隔离边界）
```

### 安全语义（明确，别再猜）

| | 说明 |
|---|---|
| 数据隔离 | **命名空间一级**：A 空间的能力/事件/任务在图里、在列表里、在统计里都看不到 B 空间的 |
| 谁能访问一个命名空间 | **本部署任何已登录用户**（一套部署 = 一个团队） |
| 密钥作用范围 | 个人 API key，覆盖**整个部署的命名空间** |

### 如果将来真要按命名空间分权限

唯一入口是 `internal/store` 的 **`AuthorizeNamespace(ctx, userID, namespaceID)`**。
今天它等价于「已登录 + 命名空间存在」。改成按成员表判定时：

1. 新增 `namespace_members(namespace_id, user_id, role)`
2. 只改 `AuthorizeNamespace` 一个函数
3. 数据模型、端点形状、I11 都不动

这条收口是刻意留的：**没有它，将来加成员表就要在每个 handler 里散着改。**

---

## 1. 为什么需要它

一个部署里天然存在多套互不相干的流程：生产 / 预发、某个客户的专用链、某个团队的试验田。
如果只有一层资源空间，它们共用一张 `edges` 表、一个能力定义空间、一个节点池、一条事件流：

| 场景 | 没有命名空间时的后果 |
|---|---|
| 生产 / 预发两套流程 | 在预发配一条边，生产也会派发 —— **没有地方安放「不同的图」** |
| 两个团队都要 `lint@1` | 定义全局唯一，只能改名（`team-a-lint@1`）绕开 |
| 给某客户单独一套 webhook 和专用机器 | 只能新建 org，然后把身份体系也复制一遍 |
| 重放一次测试事件 | 污染生产的 trace 列表与统计 |
| 机器被某个实验占满 | 没有粒度把「这套流程的活」限制在某批机器上 |

根因相同：**能力 / 边 / 源 / 事件 / 任务 / 节点都缺少一个「这套东西属于哪套流程」的维度。**

---

## 2. 边界：全部隔离，只留身份和密钥

| 对象 | 归属 | 说明 |
|---|---|---|
| `capability_versions` | **命名空间** | `review@1` 在 prod 和 staging 是**两个不同的定义** |
| `nodes` / `node_instances` / `node_capabilities` / `node_pins` | **命名空间** | 一台机器属于一个命名空间 |
| `edges` | **命名空间** | 路由表 |
| `sources` / `source_targets` | **命名空间** | 事件入口 |
| `events` / `tasks` | **命名空间** | 运行数据（trace / 统计 / 审计） |
| `graph_versions` | **命名空间** | 图快照 |
| `users` / `api_keys` / `cli_auth_sessions` | **整个部署** | 身份与密钥，不随命名空间切换（见 §2.3） |

### 2.1 能力隔离后，节点协议怎么解决（本次最关键的一处）

节点在 `hello` 里声明的是 `capabilities: ["review@1"]` —— 一个**名字**。
一旦能力按命名空间划分，同名就会指向 N 个不同定义：节点到底能跑哪个？

**解法：把命名空间放进节点令牌，而不是放进协议帧。**

```text
节点令牌  ant_<label>_<base64url(24B)>  →  DB 里那一行带 node_id → node_id 带 namespace_id
hello 帧  { capabilities: ["review@1"] }   ← 形状一个字都不用改
中枢解析  "review@1" 只在**这个节点所在命名空间**里查定义
```

收益：

- **已部署的 agent 不需要任何代码改动**，只需要换一个令牌（`hello` 帧完全不变）
- 歧义从根上消失：一个节点只属于一个命名空间，它能声明的能力集合天然封闭
- `I2`（能力必须先定义、出口是封闭集合）继续成立，只是闭合范围从「组织」缩小到「命名空间」

**一台机器要同时服务两个命名空间怎么办**：注册两次，两个节点行、两个令牌。
这看起来"多此一举"，但它是**唯一无歧义**的表达 —— 同一个进程同时挂两个命名空间时，
模型上它就是两个节点（各自的并发槽位、各自的声明、各自的排空状态）。
反过来（一个节点跨命名空间）会让 `node_capabilities` 带命名空间、`hello` 必须
按命名空间分组上报、槽位算谁的也说不清 —— 复杂度全砸在协议和调度上，收益为零。

### 2.2 内置能力要按命名空间播种

`notify.phone` 这类内置能力（中枢执行、`kind=origin/end`）是**随项目发布**的，
不属于任何用户操作。能力既然按命名空间隔离，就必须：

- **新建命名空间时，同事务把内置能力播种进去**（否则新命名空间里连 `notify.phone` 都没有，
  `end` 节点无法声明，推送直接失效）
- **升级内置能力版本时，遍历所有命名空间各插一行**（append-only，不改旧行）

这条容易被漏掉：播种只在「新命名空间」发生，升级只在「发布时」发生，两条路径都要写。

### 2.3 密钥属于部署，不属于命名空间

API key / CLI token 是「谁的凭据」，作用范围是**整个部署**：一个 key 能读写所有命名空间。

为什么不做成命名空间绑定：

- 一套部署 = 一个团队，密钥的威胁模型是「泄露后谁能用」，不是「防内部越权」
- 绑定会让 `api_keys` 多一份 `namespace_id`，每次校验多一次连接查询，收益为零

页面上的对应表达：`keys` / `security` / `admin` 三页**不随命名空间切换变化**，
标题旁的徽标写「全部署通用」（曾经写「组织级」—— 组织层删除后那是错的，见 #209）。

**节点令牌是另一回事**（§2.1）：它**必须**绑定命名空间，因为节点天然属于一个命名空间。

---

## 3. 数据模型

```sql
CREATE TABLE IF NOT EXISTS namespaces (
    id         TEXT PRIMARY KEY,              -- ns_<base62>
    slug       TEXT NOT NULL UNIQUE,          -- prod / staging / team-a（稳定，出现在 URL 里）
    name       TEXT NOT NULL,                 -- 展示名（可改）
    created_at INTEGER NOT NULL
);
```

**所有资源表加 `namespace_id`，并把唯一约束/索引的主键前缀换成它。**

```sql
-- 能力定义：名字的唯一性从「组织」缩小到「命名空间」
ALTER TABLE capability_versions ADD COLUMN namespace_id TEXT NOT NULL
    REFERENCES namespaces(id) ON DELETE CASCADE;
-- UNIQUE (namespace_id, name, version)
-- UNIQUE (namespace_id, content_hash)

-- 节点：一台机器属于一个命名空间
ALTER TABLE nodes ADD COLUMN namespace_id TEXT NOT NULL
    REFERENCES namespaces(id) ON DELETE CASCADE;

-- 边 / 源 / 事件 / 任务 / 图快照 / 节点能力 / 粘性钉 同理
CREATE INDEX idx_edges_lookup ON edges(namespace_id, from_capability, from_port);
CREATE INDEX idx_events_trace ON events(namespace_id, trace_id);
CREATE INDEX idx_tasks_trace  ON tasks(namespace_id, trace_id);
CREATE UNIQUE INDEX ux_graph_ns_hash ON graph_versions(namespace_id, content_hash);
```

**没有 `org_id`。** 迁移 14 已把所有 `org_id` 列与 `orgs` / `org_members` 删掉；
隔离的唯一依据就是 `namespace_id`。

> 迁移 14 的一处历史包袱：`namespaces` 原本是 `UNIQUE(org_id, slug)`，改成 `UNIQUE(slug)` 时
> 老库里多个组织各有一个 `default` 会撞车 —— 迁移按 `(created_at, rowid)` 保留最早一行，
> 其余顺序改名为 `default-2`、`default-3`…，并有专门用例守着。

### 3.1 默认命名空间

首次注册时，**同事务**建 `default` 命名空间（slug `default`，名「默认」），
并播种内置能力。**代码里不存在「没有命名空间」的分支。**

### 3.2 删除命名空间（比之前危险得多）

删命名空间现在会级联删掉：能力定义、节点、源、边、事件、任务、图快照。

- 里面还有 sources / edges / tasks 时 → `409`，列出阻塞对象
- `?force=true` → 级联删除，并写一条审计事件
- **不允许删最后一个命名空间**

**运维后果必须写在 UI 上**：删命名空间 = 里面的机器**全部掉线**（令牌失效），
agent 重连会一直失败。这不是"清理配置"，是"拆掉一个环境"。
建议 UI 在确认框里明确列出「N 台机器将失去连接」。

---

## 4. 不变量修订与新增

| # | 内容 | 变更 |
|---|---|---|
| **I10** | 一切租户数据先过 `namespace_id` | **修订（#204）**：不再有 `org_id`；写路径必须校验「命名空间存在」且调用方已通过 `AuthorizeNamespace` |
| **I11**（新） | **跨命名空间不引用** | 新增 |

**I11 的四处必须同时成立**（能力隔离后，前三处已经「结构上几乎不可能」，
但**仍要显式校验** —— 结构能防的是"写不出来"，防不了"写错了"）：

1. `edges` 两端的能力必须与边属于同一 `namespace_id`（能力 id 全局唯一，
   但把别处的 id 填进来是合法的 SQL）
2. `source_targets` 的目标能力必须是同命名空间的
3. `node_capabilities` 只能声明**本命名空间**定义的能力
4. **子任务继承父任务的 `namespace_id`**，不接受调用方指定

违反一律 `400`（不是静默忽略 —— 静默忽略会让人以为配置生效了）。

---

## 5. 派发四步的改动

```text
输入: { orgID, namespaceID, fromCapability, fromPort, params, traceID, path, parentTaskID }

① 查表   SELECT to_capability FROM edges
          WHERE namespace_id=? AND from_capability=? AND from_port=?
② 校参数  完全不改（params_schema 是能力自己的东西）
③ 找节点  SELECT node_id FROM node_capabilities WHERE capability_id=?
          + 只在这个命名空间的在线节点里挑
④ 挑一台  完全不改（粘性/负载逻辑与命名空间无关）
⑤ 建任务  namespace_id = 父任务的（子任务）或入参（根任务）
```

**②④ 一个字都不用改**，①②③⑤ 各多一个维度。

能力 id 全局唯一，所以 ③ 的 `capability_id=?` 天然只能匹配到一个命名空间的声明；
但仍然用 `namespace_id` 过滤一遍——防的是**数据被写坏**（见 I11 的说明）。

---

## 6. HTTP API

### 6.1 命名空间自身

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/namespaces` | 列出本组织的命名空间（带各自的资源计数，UI 直接用） |
| `POST` | `/v1/namespaces` | `{slug, name}`；**同事务播种内置能力** |
| `PATCH` | `/v1/namespaces/{id}` | 只允许改 `name`（`slug` 稳定标识，不可改） |
| `DELETE` | `/v1/namespaces/{id}` | 见 §3.2 |

### 6.2 资源端点怎么带命名空间

**显式，或者显式全部。永远没有隐式默认。**

| 端点 | `namespaceId` |
|---|---|
| `/v1/capabilities`、`/v1/nodes`、`/v1/nodes/{id}`、`/v1/edges`、`/v1/sources`、`/v1/events`、`/v1/tasks`、`/v1/traces/*`、`/v1/stats` | **必填** |
| 同上但要跨命名空间汇总（管理员视图） | `namespaceId=*` |
| `/v1/admin/*`、`/v1/auth/*`、`/v1/keys/*`（密钥） | **不需要**（整个部署级） |

- 缺 `namespaceId` → `400`（**不回退到默认命名空间**：回退等于把人写错的地方藏起来）
- 不存在 → `404`；跨命名空间引用 → `400`（I11）
- **不再有 `403「别人的命名空间」`**：组织层删除后没有「别人」这个概念，
  只有「存在」与「不存在」；`?orgId=` / `X-Org-Id` 也不再是任何端点的参数
- 所有资源在响应里带 `namespaceId`；`/v1/namespaces` 自身不带

**节点注册**：`POST /v1/nodes` 的 `namespaceId` 决定这台机器属于哪个命名空间，
**令牌就绑在这个命名空间上**。创建后 `namespaceId` 不可改（要换就删了重建）。

### 6.3 与 openapi 的关系

`scripts/check_openapi.py` 要求 `api/openapi.yaml` 与 `internal/server/mux.go`
**双向一致**。所以本轮（设计 + mock）**不动 openapi**：
先加 spec 而没有 handler 会直接让 `make contract` 失败。

实现落地时按此顺序：mux → handler → openapi → `make contract`。

---

## 7. UI

- **侧栏顶部**一个触发器 + 分节菜单：**命名空间（单一维度）**。
  已实现（`web/partials.js` 的 `mountNamespaceSwitcher`），状态存 localStorage。
- **所有页面都随命名空间切换**（能力和节点也是）。切换后整页重取。
- 新建命名空间入口在菜单「命名空间」小节标题右侧的 `+`；列表为空时该菜单显示空态（提示文本 + 新建按钮），
  不渲染空下拉，两个新建入口都可用。
- 因为**没有页面是组织级的**了，页面标题不再需要「组织级」徽标
  （之前能力页 / 节点页有，能力与节点隔离之后就必须去掉，否则是自相矛盾的信息）。

---

## 8. 明确不做（本期）

| 不做 | 为什么 |
|---|---|
| 命名空间级 RBAC（谁能看哪个 ns） | 一套部署 = 一个团队，所有登录用户共享全部命名空间。真要分权时的唯一入口见 §0（`AuthorizeNamespace` + `namespace_members`） |
| 命名空间级配额（max_tasks 等） | 六道闸 G1~G6 是任务级/图级；配额是另一套机制 |
| 一个节点跨多个命名空间 | 见 §2.1：注册两次即可。跨命名空间会让协议与调度复杂度暴涨 |
| 跨命名空间 promote 图（预发 → 生产） | 有价值，但需要先有图的导出/导入，不属于隔离机制本身 |
| 命名空间重命名 slug | `slug` 出现在 URL 和令牌作用域里，改了会让旧链接失效；只改 `name` |

---

## 9. 开放问题

1. **`namespaceId=*` 的鉴权**：现在是「已登录即可」。是否要让 `users.role=admin` 才允许跨命名空间汇总？（倾向：要，但当前单团队场景下优先级低）
2. **事件中心的默认视图**：登录后落在 `default` 还是「上次用的」？
   （倾向：上次用的，存 localStorage；服务端不记）
3. **`graph_versions.content_hash` 是否含 `namespace_id`**：倾向**不含** ——
   快照内容就是 edges 列表，命名空间已经由列表达；混进 hash 会让「同一张图在两个
   命名空间」变成两条内容不同、实际相同的快照。
4. **删命名空间时，在线节点怎么处理**：直接断 WS，还是先发一条
   `draining` / `namespace_removed` 帧让 agent 打日志？（倾向：先发帧再断，
   否则机器侧只能看到"连接被拒"，排查成本高）
