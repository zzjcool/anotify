# 冻结契约 · 协议与部署状态机

> 冻结接口。实现位置：`internal/ws/`、`internal/store/`、`internal/server/`。

## 1. 协议版本

`ProtocolVersion = 2`（从 1 升）。协议不匹配的 hello 一律 `protocol_mismatch`，老客户端必须升级。

## 2. 帧清单（11 种）

| 方向 | 帧 | 变化 |
|---|---|---|
| ↑ 节点→中枢 | `hello` | **改**：删 `capabilities[]`、删 `manifest[]`；加 `applied[]` |
| ↑ | `heartbeat` | 不变（只有 `ts`） |
| ↑ | `ack` | 不变 |
| ↑ | `progress` | 不变 |
| ↑ | `result` | 不变 |
| ↑ | **`apply`** | **新增**：回报部署结果 |
| ↓ 中枢→节点 | `welcome` | 不变 |
| ↓ | `assign` | **改**：加 `digest`、`trace_id`（都可选） |
| ↓ | **`deploy`** | **新增**：下发/撤销产物 |
| ↓ | `cancel` | 不变 |
| ↓ | `error` | 不变（错误码表见 §6） |

## 3. hello（改）

```go
type HelloFrame struct {
    Type      string         `json:"type"`      // "hello"
    NodeID    string         `json:"node_id"`
    Protocol  int            `json:"protocol"`  // 必须 == 2
    Instances []InstanceDecl `json:"instances"` // 不变：{slot, concurrency}[]
    Applied   []AppliedDecl  `json:"applied"`   // 新增：本机已缓存的产物
    Labels    map[string]any `json:"labels,omitempty"`
}

// AppliedDecl 是节点对「我缓存了什么」的陈述，不是「我能跑什么」的声明。
// 能力归属完全由中枢的部署决定（D4）。
type AppliedDecl struct {
    Capability string `json:"capability"`
    Version    int    `json:"version"`
    Digest     string `json:"digest"` // "sha256:<64 hex>"
}
```

**删除**：`CapabilityDecl`、`ManifestDecl` 两个类型整个删掉。

**服务端处理顺序（必须保持）**：
1. 校验 `protocol == 2`
2. `UpsertInstances`（超 `nodes.max_instances` → `too_many_instances`）
3. **`ReconcileApplied`**：把 `applied[]` 落成 `node_capabilities`（全部删除后按 applied 重建 —— 保持「全量替换」语义，但数据源从「节点声明的能力」换成「节点缓存的产物」）
4. `ReclaimStaleSlots` / `UpdateNodeStatus(online)` / `hub.Register`

**`applied[]` 里的能力必须已在中枢定义**（否则 `unknown_capability`）。这条保留：
产物本身就是从中枢部署过去的，不可能出现中枢不认识的能力。

## 4. deploy（新增，下行）

`action=remove` **也必须带 artifact**：帧里没有 capability/version 字段，节点靠产物里的
`metadata.{name,version}` 才知道要摘掉哪一条缓存索引。缺 artifact 时节点回
`apply{failed, reason="invalid_artifact"}`（不猜名字去删）。


```go
type DeployFrame struct {
    Type     string          `json:"type"`     // "deploy"
    DeployID string          `json:"deploy_id"`
    Action   string          `json:"action"`   // "apply" | "remove"
    Artifact json.RawMessage `json:"artifact"` // **两种 action 都必填**，格式见 CONTRACT-artifact §2
}
```

## 5. apply（新增，上行）

```go
type ApplyFrame struct {
    Type       string `json:"type"`     // "apply"
    DeployID   string `json:"deploy_id"`
    Capability string `json:"capability"`
    Version    int    `json:"version"`
    Digest     string `json:"digest"`
    State      string `json:"state"`    // "applied" | "failed"
    Reason     string `json:"reason"`   // failed 时必填，机器可读短码 + 人可读说明
}
```

`reason` 短码（`;` 连接多个）：`invalid_artifact`、`missing_requirement:claude`、
`write_failed`、`disk_full`、`no_such_capability`、`unsupported_action`。

## 6. assign（改）

```go
type AssignFrame struct {
    Type           string          `json:"type"`
    TaskID         string          `json:"task_id"`
    Capability     string          `json:"capability"`
    Version        int             `json:"version"`
    Params         json.RawMessage `json:"params,omitempty"`
    LeaseUntil     int64           `json:"lease_until"`
    MaxDurationSec int             `json:"max_duration_sec,omitempty"`
    Digest         string          `json:"digest,omitempty"`   // 新增：本次应跑的产物 digest
    TraceID        string          `json:"trace_id,omitempty"` // 新增：链路 ID
}
```

节点侧：优先用 `digest` 定位缓存；缺省则用 `capability@version` 查索引。

## 7. 部署状态机（`deployments.state`）

```
                 控制台 POST /v1/deployments
                            │ INSERT (state=pending)
                            ↓
                     ┌─────────────┐
              ┌──────│   pending   │──────┐
              │      └─────────────┘      │
    apply.applied│                    apply.failed
              ↓                            ↓
      ┌─────────────┐              ┌─────────────┐
      │   applied   │              │   failed    │
      └─────────────┘              └─────────────┘
              │
   hello.applied[].digest != desired.digest
              ↓
      ┌─────────────┐
      │    local    │   本地变体：中枢永不覆盖
      └─────────────┘
```

控制台 `DELETE /v1/deployments` → 推 `deploy{action:remove}` 后**直接删行**（不留 `removed` 态）。

**写者纪律（关键，防两个真相源）**：

| 表 | 列 | 谁写 |
|---|---|---|
| `deployments` | `id, namespace_id, node_id, artifact_id, digest, created_at` | **只有控制台 API** |
| `deployments` | `state, reason, updated_at` | **只有节点帧**（apply）与 hello 对账 |

两边不得碰对方的列。任何跨列写入视为契约违反。

## 8. `node_capabilities` 的语义变更（D4）

从「节点自报的能力事实」降级为 **「已部署且节点确认的产物的派生索引」**。

```sql
-- 派生规则：每次 reconcile 都全量重建
DELETE FROM node_capabilities WHERE node_id = ?;
INSERT INTO node_capabilities (node_id, capability_id, namespace_id)
  SELECT d.node_id, a.capability_id, d.namespace_id
    FROM deployments d JOIN artifacts a ON a.id = d.artifact_id
   WHERE d.node_id = ? AND d.state IN ('applied', 'local');
```

- `run_spec` 列**删除**（migration 15 重建表）
- `NodesForCapability` SQL **一行不改** → 派发链路零改动（这是 D4 的全部价值）
- 语义边界写进 schema 注释：**这张表不得手写，只从 `deployments` 派生**

## 9. 新增表

```sql
CREATE TABLE IF NOT EXISTS artifacts (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    digest        TEXT NOT NULL,
    spec          TEXT NOT NULL,            -- canonical JSON（CONTRACT-artifact §2）
    created_at    INTEGER NOT NULL,
    UNIQUE (namespace_id, digest)
);

CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL,
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    artifact_id  TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    digest       TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'pending',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (node_id, artifact_id)
);
CREATE INDEX IF NOT EXISTS idx_deploy_node ON deployments(node_id);
CREATE INDEX IF NOT EXISTS idx_deploy_artifact ON deployments(artifact_id);
```

## 10. I1 改写（必须一起落）

**旧**：中枢 DB 无 `argv`、无 shell 字符串、无脚本文本。
→ **已是假命题**（`node_capabilities.run_spec` 存着 command）。

**新 I1**：

> **中枢自身不得执行任何部署产物中的代码。**
> 节点执行的一切必须是内容寻址（digest）、不可变、可审计的产物，
> 并受**节点启动参数**设定的资源/网络/并发上限约束。
> 中枢可以授权部署，但不得扩大节点自身允许的范围。

对应的静态断言（`make contract`，全部机检）：

| # | 断言 |
|---|---|
| A2 | `schema.sql` 里不出现 `argv\|shell\|script`（保留） |
| A5 | `internal/{ws,route,store,api,trigger,source,lease,executor}/**/*.go` 不 import `os/exec`（中枢不执行） |
| A6 | `artifacts`、`deployments` 两张表必须有 `digest` 列（内容寻址） |
| A7 | `node_capabilities` 不得出现在任何 `cmd/agent/**` 的写路径里（节点不能自报能力） |

## 11. 服务端 API

| Method + Path | 说明 |
|---|---|
| `GET /v1/capability-templates` | 9 个内置模板（CONTRACT-artifact §8） |
| `POST /v1/capabilities` | **改**：body 新增可选 `run: {entrypoint, files, timeoutSec, env}` + `requires[]`；有 `run` 时服务端编译产物、算 digest、落 `artifacts`，响应带 `digest` |
| `GET /v1/artifacts?capabilityId=` | 某能力的产物列表（digest + 时间） |
| `POST /v1/deployments` | body `{namespaceId, capabilityId, digest?, nodeIds[]? , tags[]?}`；`digest` 缺省取该能力最新产物；按选择器展开成 node 列表，INSERT pending，逐台推 `deploy` frame |
| `GET /v1/deployments?nodeId=&capabilityId=` | 部署状态列表（含 state/reason/本地变体标记） |
| `DELETE /v1/deployments?id=` | 推 `remove` 后删行 |

`POST /v1/deployments` 的返回值必须逐台给出 `{nodeId, deployId, pushed: bool, error?}`；
节点不在线 → `pushed=false, error="node_offline"`，**行仍然创建**（等它上线时由重连对账补推）。
