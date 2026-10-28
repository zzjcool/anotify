# 节点传输层 · NATS 接入评估与决议

> 状态：**已决议（2026-10-28）：暂不实施。** 维持现有 WSS 帧协议 + SQLite 单副本形态；业务量上来后按 §15.2 的触发条件重估。
>
> 本文保留完整评估，作为：① §14 可扩展性契约的依据；② §15 重估时的起点（不必从零推）；③ §13 已否决方案的记录（避免重复讨论）。
>
> 上游：`docs/contracts/CONTRACT.md`（现状冻结的事实源）、`design/wiki/03-flow.html`（事件旅程）、`design/wiki/06-trace.html`（可观测）。
> 相关不变量：**I1**（无 argv）、**I5**（`out` 三态）、**I7**（图里只有能力）、**I8**（版本钉住）、**I10**（org 隔离）。

---

## 决议

| 项 | 决定 |
|---|---|
| 传输层 | **维持现有 WSS 帧协议**，不引入 NATS |
| 数据库 | **维持 SQLite**，不迁 Postgres |
| 中枢副本 | **单副本** |
| 路由 | 不变（`edges` 查表 + 中枢派发） |
| trace | 不变（`events` + `tasks`） |
| 节点客户端 | 按现有 WS 帧协议实现 |

**理由**：在「单副本 + 第一方节点」形态下，NATS 相对现有实现接近打平（省 ~600 行，多一个进程内依赖）。它的全部实质收益——多副本、节点规模化——都压在已经推迟的业务量假设上。

**本次真正交付两件事**：

1. **§14 可扩展性契约** —— 让未来换传输 / 换库的成本保持在「写一个实现类」的量级，而不是重写。
2. **§15 容量基线与重估触发** —— 把「业务量上来了」翻译成可观测的数字。

**不在本次范围内的已确认问题**：见 **§16**。其中 **16.3（接力空洞）是正在丢任务的缺陷**，不应随本次决议一起推迟。

---

## 0. 一句话

**NATS 只回答「这条派发现在能不能喊到一个节点」。**

`edges` 决定「派给哪个能力」，`tasks` 决定「他答应了没有」。这三件事分别属于三个地方，不重叠。

### 0.1 NATS 在本方案里的角色，以及它不是什么

| NATS 是 | NATS 不是 |
|---|---|
| 节点链路的 wire（替代 §6.1 的 WS 帧） | ❌ 路由器（下一个能力由 `edges` 决定） |
| 同能力节点间的负载均衡器（queue group） | ❌ 队列（队列是 `tasks` 表） |
| 连接、TLS、重连、背压的提供者 | ❌ 去重器 / 限流器（那是主键与 COUNT） |
| 未来多协议入口的预留位置 | ❌ trace 存储（trace = `events` + `tasks`） |
| | ❌ 可靠投递的保证（那是 claim request-reply + 租约） |

> **一句话：NATS 是 wire，不是 brain。**

**方案的正当性依赖一个前提：多租户 ⇒ 多副本。**

| 形态 | NATS 相对现有 WS 协议 | 换不换 |
|---|---|---|
| A · 自托管单副本 | 接近打平（省 ~600 行，多一个进程内依赖） | 理由弱 |
| B · 托管多副本 | 正向（节点亲和性问题消失，副本可水平扩） | **这是方案的全部理由** |

若多租户最终是「一套单机 + 多 org 分区」而非多副本，则本方案应被取消——现有 WS 协议已跑通，换它是净成本。

**以下四句话一出现，就说明定位飘了：**

| 说法 | 回到哪 |
|---|---|
| 「NATS 帮我们路由事件到下游能力」 | §13.4 |
| 「开 JetStream 保证消息不丢」 | §13.2（L2 失守） |
| 「用 NATS 的去重 / 限流」 | §1.1（那个能力不存在） |
| 「把 trace 存进 NATS」 | §7（引出 materialization） |

---

## 1. 边界（三条不可越过的线）

| # | 线 | 越过之后 | 怎么守住 |
|---|---|---|---|
| **L1** | `edges` 是唯一路由依据 | 图不再是数据，`04-edges.html` 的"不跑也能知道"失效 | publish 的 subject 只能来自 `Dispatch` 查表结果；代码里不存在"节点决定下一个能力"的路径 |
| **L2** | `tasks` 是唯一队列与状态机 | 重试策略出现第二个位置 → `cap_hash` 版本钉住失效 | **不开 JetStream**；不依赖任何 broker 级重投 |
| **L3** | 剪枝必须落库 | 「为什么没跑」永久答不出 | `DispatchOutput.Pruned` 每一条都有对应 `events` 行 |

L2 的具体形式：唯一的重试判据是 `sweepOne` 读 `capability_versions.lifecycle`。一旦有人为了"更可靠"打开 JetStream，`max_deliver` 就会成为第二个判据，L2 当场失守。

### 1.1 我们真正需要的抽象：计量共享订阅

把需求压缩成一句话：

> 一个**共享订阅**（消息在消费者之间负载均衡），**外加一个计量闸门**（同一逻辑工作最多被交付 N 次）。

前半句 NATS 给，后半句要在自己的代码里。这不是选型问题：

| 能力 | NATS | 任何 broker |
|---|---|---|
| 共享订阅 / 负载均衡 | ✅ queue group | ✅ |
| 按 key 亲和到唯一消费者 | ❌（需自己做一致性哈希） | ❌ |
| **同一逻辑工作最多交付 N 次** | ❌ | ❌ **没有任何 broker 提供** |

最后一行是**类别边界**，不是缺口：broker 是无状态路由器，而「这份工作已经交付过几次」是**应用状态**。RabbitMQ 的 `x-max-length` 限制的是队列深度，不是累计交付次数；JetStream 的 `max_deliver` 限制的是**单条消息**的重投，不是同一逻辑工作被重复发布。

#### 闸门长什么样

只有三条语句，一个事务：

```sql
BEGIN;
-- ① 去重：确定性主键撞车 = 已经交付过
INSERT INTO tasks (...) VALUES (...) ON CONFLICT(id) DO NOTHING;
-- ② 预算：整条 trace 的任务数上限（wiki 的 G4 max_tasks）
SELECT COUNT(*) FROM tasks WHERE org_id=? AND trace_id=?;
-- ③ 归属 + 租约
UPDATE tasks SET status='assigned', node_id=?, lease_until=?, assigned_at=?
 WHERE id=? AND status='pending';
COMMIT;
```

①②③ 都由主键与 CAS 保证原子；② 是一条走索引的 COUNT。

#### 闸门必须在「能跑」之前，不在「收到」之后

| 理由 | 晚了会怎样 |
|---|---|
| 去重 | 已经跑过一次（副作用不可回滚） |
| 预算 | 已经超了 |
| **拒绝记录** | **没人在「拒绝的那一刻」写 `pruned/max_tasks`** |

第三行最容易被忽略：**拒绝的记录只能由拒绝者写。** 而拒绝者必须是持有共享状态的那个人。

这就是闸门不能分散的最后理由——不是性能，是**痕迹**。分散的闸门拒绝了一条消息，那条消息就安静地消失了，没有任何人能事后说「这里曾经有一件事被挡住」。

---

## 2. 部署形态

| 形态 | broker | 存储 | 副本数 |
|---|---|---|---|
| **A · 单二进制自托管** | 嵌入式 `nats-server`（`nats.InProcessServer`） | SQLite | 1 |
| **B · 托管多租户** | 独立 `nats-server` 集群 | Postgres | N |

**同一套代码，两条路径**：中心侧一律使用 NATS **客户端** API（形态 A 用 `InProcessServer` 连进程内 server），只有 `route.Publisher` 后面的实现不同，业务代码零感知。

> ⚠️ **嵌入与集群互斥。** 形态 A 永远单副本。想要多副本必须切形态 B —— 这必须在文档和 UI 里写明，否则会出现"我们嵌了 NATS，为什么不能 HA"的返工。

---

## 3. Subject 命名

```
anotify.cap.<org_id>.<capability_id>     下行 · 能力派发     （订阅方用 queue group = capability_id）
anotify.node.<node_id>.direct            下行 · 定向         （sticky 派发、cancel）
anotify.up.<node_id>                     上行 · 节点回报     （claim / result 用 request-reply）
```

设计要点：

- **不含协议版本号**。版本在 envelope 里（`v`），subject 保持稳定，便于 ACL 长期生效。
- `org_id` 必须在 subject 里（不是冗余），因为它是 **subject 级 ACL 的唯一依据** —— 这是 I10 从"每处手写 `WHERE org_id`"升级为"连接层强制"的机会。
- 保留前缀：`anotify.cap.` / `anotify.node.` / `anotify.up.` 不得作为节点名或能力名。

---

## 4. 节点通信

### 4.1 声明从握手移到 DB（本方案最大的语义变化）

**改变**：`CONTRACT.md` §6.1 的 `hello` 帧携带能力声明，握手时由中枢校验（`ConfirmCapabilities` → `unknown_capability`）。
**本方案**：声明是 **DB 里的既有事实**，连接时不再申报。

理由：ACL 必须在 `CONNECT` 时就能算出"这个节点能订阅哪些 subject"，而 `hello` 发生在 `CONNECT` 之后 —— 先有鸡还是先有蛋。所以声明只能先落库。

对用户不可见，由 CLI 串起来：

```bash
anotify node up --token <token> --capabilities review@1,fix@1 --concurrency 4
```

CLI 内部：`POST /v1/nodes/self/declare`（带 token）→ 落库 → 建立 NATS 连接。

**代价**：改声明需要断开重连（ACL 在连接期固定）。这个代价可接受，因为改声明本来就很少发生。

> 被否决的替代方案：两阶段连接（先以最小权限连上、申报、断开、以完整 ACL 重连）。可行但节点 SDK 要多一套重连状态机，错误面更大。

### 4.2 时序

```
① 中心派发
   edges 查表 → N 个目标能力
   → 逐个校 params_schema / 判 path / 查 node_capabilities（no_node 前置判定）
   → BEGIN
       INSERT tasks (status=pending, published_at=now, publish_count=0)
     COMMIT
   → publish anotify.cap.<org>.<cap_id>   {v,type:assign, task_id, capability, version,
                                           params, attempt, max_duration_sec}
                                          （sticky：改发 anotify.node.<id>.direct）

② 节点收到 assign
   本地信号量有空位？否 → 丢弃并计数（见 4.5）
   是 → 发出 claim（request-reply）

③ 中心 CAS 授予
   UPDATE tasks SET status='assigned', node_id=?, lease_until=?, assigned_at=?
    WHERE id=? AND status='pending'
   1 行 → reply grant {lease_until}
   0 行 → reply deny  {reason:"taken|expired|canceled"}
         （节点必须丢弃本地任务，不执行）

④ 执行期间
   publish heartbeat  每 10s       → ExtendLease(now+30)
   publish progress   可选         → ExtendLease(now+30)（note 按 §7.2 不落库）

⑤ 回报结果（request-reply）
   request result {status, out, summary, artifacts, error}
   中心：终态写入 + 派下一跳（见 §6.2 的原子性要求）
   reply ok
   （无 reply 或超时 → 节点重发；ReportResult 已按终态幂等）

⑥ 下一跳
   拿 (capability_id, out) 走 ① 的同一条路径
```

### 4.3 envelope v1

**格式必须是 JSON。** 不许换 MessagePack / Protobuf —— `out` 三态依赖「字段缺失」与「显式 null」的区分（I5），JSON 是唯一能干净表达它且各语言一致的选择。

```jsonc
// ── 下行 · 能力派发（publish，fire-and-forget）──────────────
{ "v": 1, "type": "assign",
  "task_id": "tsk_x",
  "capability": "review", "version": 1,
  "params": { "diff": "..." },
  "attempt": 1,                    // fencing token：同一 task 的第几次执行
  "max_duration_sec": 3600 }
  // 没有 argv、没有命令行、没有脚本文本（I1）

// ── 下行 · 定向（publish）────────────────────────────────
{ "v": 1, "type": "cancel", "task_id": "tsk_x", "reason": "superseded" }

// ── 上行 · 领活（request-reply，必须拿到答复才开工）────────
{ "v": 1, "type": "claim", "task_id": "tsk_x", "attempt": 1 }
  → reply  { "v":1, "type":"grant", "task_id":"tsk_x", "lease_until":1730000030 }
  → reply  { "v":1, "type":"deny",  "task_id":"tsk_x", "reason":"taken|expired|canceled" }

// ── 上行 · 存活与进度（publish，可丢）────────────────────
{ "v": 1, "type": "heartbeat", "ts": 1730000000 }
{ "v": 1, "type": "progress",  "task_id": "tsk_x", "note": "step 2/5" }

// ── 上行 · 结果（request-reply，必须拿到答复）─────────────
{ "v": 1, "type": "result", "task_id": "tsk_x",
  "status": "succeeded",        // succeeded | failed
  "out": "fix",                 // 出口名；"out": null = 到头；字段缺失 = 没回报
  "summary": "发现 2 处类型错误",
  "artifacts": ["tsk_8f21a3c0"],
  "error": "" }
  → reply { "v":1, "type":"ok" }
  → reply { "v":1, "type":"err", "code":"invalid_transition|lease_expired|..." }
```

**为什么 `claim` 和 `result` 用 request-reply，另外两个不用**：

| 帧 | 通道 | 理由 |
|---|---|---|
| `claim` | request-reply | 必须先确认所有权再开工，否则节点会干完才发现自己没资格 |
| `result` | request-reply | 丢了 → 任务卡到租约过期才重派。要答案才能重发 |
| `heartbeat` / `progress` | publish | 丢了下次再来，无副作用 |

这样 **不需要 JetStream 也能拿到"可靠投递"**，且不带任何重投策略——重发次数由节点自己的重试逻辑决定，与 broker 无关（L2 守住）。

### 4.4 错误码（替代原 §6.1 的错误表）

| code | 何时 | 谁收到 |
|---|---|---|
| `taken` | claim 时任务已被别人领走 | 节点（必须丢弃本地任务） |
| `expired` | claim 时 `published_at` 已超时 / 任务已被取消 | 节点 |
| `lease_expired` | 收到 `result` 时租约已失效 | 节点（丢弃本地任务） |
| `invalid_transition` | 状态机拒绝 | 节点 |
| `unknown_task` | 任务不存在或不属于该 org | 节点 |
| `deny`（grant 的反面） | 见上三种 `taken/expired/canceled` | 节点 |

`unknown_capability` / `bad_frame` / `unknown_type` **从协议层消失**：前者由 `declare` 时校验（HTTP 422），后者由 envelope 解码错误在中心侧记为 `bad_envelope` 并丢弃（带节点 ID 日志）。

### 4.5 并发自限

- 并发上限由**节点本地信号量**执行（`--concurrency N`），声明值落 `nodes.concurrency`。
- 中心**不再挑选节点、不再占槽位**（`node_instances` 整表删除）。
- 中心的超载观测改为派生查询：

```sql
SELECT node_id, COUNT(*) AS inflight
  FROM tasks
 WHERE status IN ('assigned','acked','running')
 GROUP BY node_id;
```

- 节点满员时丢弃 `assign` 是**正常行为**，不是错误 —— 但必须计数并暴露（`/v1/stats` 的 `assign_dropped`），否则派发失败会静默。

### 4.6 离线检测

删掉 `NodeHub` 与 `nodes.online`。三条路径覆盖：

| 场景 | 检测方式 |
|---|---|
| 节点断线后仍有在途任务 | 租约过期 → `SweepExpiredLeases`（现状逻辑不变） |
| 节点断线、派发无人领 | `published_at` 超时 → 未领取重发（§6.3） |
| sticky 定向派发无人收 | 同第二条 → 重发到能力 subject，并清除失效 pin |

`nodes.last_seen_at` 保留（来自任何上行帧），只用于控制台展示，不参与任何判定。

---

## 5. 能力路由

### 5.1 派发四步（改动标注）

| 步 | 原实现 | 本方案 |
|---|---|---|
| ① 查表 | `edges(from_capability, from_port) → N` | **不变** |
| ② 校参数 | `params_schema` + `path` + `foreign_user` | **不变** |
| ③ 找节点 | `NodesForCapability` → 在候选里按负载挑一台 | **降级为前置判定**：0 行 → `pruned/no_node`，不建任务；>0 行则不挑，直接进 ④ |
| ④ 建任务 + 占槽位 + 推帧 | `LeaseInstance` + `AssignTask` + `PublishAssign` | **改为**：建任务（pending）→ publish |

### 5.2 两侧分工

| 决策 | 谁做 | 依据 |
|---|---|---|
| 派给**哪个能力** | 中心 | `edges` |
| 派给**哪个节点** | NATS | queue group |
| **能同时跑几个** | 节点 | 本地信号量 |
| **跑完了没** | 中心 | `tasks` 九态 + 租约巡检 |

### 5.3 sticky

能力定义里 `sticky: worktree` 时，中心先查 `node_pins`：

- 命中 → publish `anotify.node.<node_id>.direct`
- 未命中 → publish 到能力 subject，并**在 claim 到达时**写 pin（原来在派发时写，现在只有节点确定后才能写）

pin 失效（节点已不在候选集 / 定向派发未领取）→ `rebalance` 清理，退回纯均衡。

### 5.4 删除清单

| 删除 | 位置 | 替代 |
|---|---|---|
| `pickNode` + `ListInstances` 循环 | `route/dispatch.go` | queue group |
| `LeaseInstance` / `ReleaseInstance` / `node_instances` 表 | `store/node.go` + `schema.sql` | 节点本地信号量 |
| `ws.NodeHub`（register/unregister/session） | `internal/ws/node_hub.go` | subject |
| `assign`/`ack`/`hello`/`welcome` 帧 + 握手 | `internal/ws/node_protocol.go`、`node_handshake.go` | envelope v1 |
| `ConfirmCapabilities`（握手期校验） | `store/node.go` | `declare` 时校验（HTTP） |
| `requeueAfterPublishFailure` | `route/dispatch.go` | 不再需要（发布不会失败，只有"没人领"） |

**保留**：`edges`、`tasks` 九态、`sweepOne`、`executor`（中枢能力）、`push`、`graph_versions`、`source`、全部剪枝逻辑。

---

## 6. 事件传递

### 6.1 状态下谁写

| 状态迁移 | 触发 | 写入者 | 通道 |
|---|---|---|---|
| （无）→ `pending` | 派发 | 中心 | SQL 事务 |
| `pending` → `assigned` | `claim` 到达 | 中心 | **CAS**（`WHERE status='pending'`） |
| `assigned` → `acked` | 不再使用 | — | 已合并进 `assigned`（claim 即接单） |
| `assigned` → `running` | 首个 `progress` | 中心 | CAS |
| `assigned`/`running` → `succeeded`/`failed` | `result` | 中心 | CAS |
| 任意非终态 → `lease_expired` → `pending`/`abandoned` | 巡检 | 中心 | 现状逻辑不变 |

> **`acked` 状态保留在表里**（契约 §3.1 的九态不动），但本方案不再产生它：`claim` 同时意味着"接单"和"开始"，两者之间没有可观测的间隔。若将来需要区分"接了但还没开始"，再加一次 `ack` 帧即可，不动表。

### 6.2 接力原子性（硬要求）

今天 `node_session.go:183` 的接力是 **fire-and-forget 后台 goroutine**：`ReportResult` 已提交终态，子任务可能不存在，且没有对账。**这是现在正在丢任务的地方，必须在本次实现中修掉。**

要求：**「终态写入 + 下一跳建任务」要么同事务，要么走 outbox。**

推荐 outbox（因为下一跳需要 publish，事务里不能安全地做网络操作）：

```sql
-- 与终态写入同一事务
INSERT INTO dispatch_outbox (id, org_id, trace_id, parent_task_id, from_capability, out, params, path, created_at)
VALUES (...);
```

```sql
-- 巡检领取（现有 5s ticker 挂一段）
SELECT * FROM dispatch_outbox WHERE created_at <= now - 1 ORDER BY created_at LIMIT 100;
-- 处理成功后 DELETE
```

好处：接力与派发共用同一条 `Dispatch` 路径，剪枝记录也就在这条路径上落库（L3）。

### 6.3 未领取重发（新增）

```sql
-- tasks 新增两列
published_at    INTEGER,                    -- 发布时刻
publish_count   INTEGER NOT NULL DEFAULT 0   -- 已发布次数（不是 attempts）
```

巡检每轮：

```
pending && published_at < now - 10 && publish_count < 3
  → 重新 publish，publish_count++
pending && published_at < now - 10 && publish_count >= 3
  → pruned/no_node（写 events 行），任务保持 pending 等人工/节点上线
```

> 注意 `publish_count` 与 `attempts` 是两个东西：前者是"喊了几次没人应"，后者是"执行了几次没成功"。**不能合并** —— 合并会让 fencing token（`attempt`）在"没人领"时也递增，从一个合法的 fencing 变成错误信号。

### 6.4 剪枝落库（Phase 0，独立于本方案）

剪枝当前**完全不落库**：`PrunedRecord` 只在 HTTP 响应里出现一次（`api/events.go:161`），之后永远查不到。这与 `06-trace.html`「剪枝是静默的，但一定留痕」直接冲突。

实现：复用 `events` 表，不建新表。

```sql
INSERT INTO events (id, org_id, trace_id, source, type, payload, idem_key, created_at)
VALUES (:evt, :org, :trace, 'route', 'pruned.no_edge', :payload, NULL, now());
```

| 字段 | 值 |
|---|---|
| `source` | `'route'` |
| `type` | `pruned.<reason>`，reason ∈ `no_edge` / `param_schema` / `no_node` / `foreign_user` / `path` |
| `payload` | `{fromCapability, port, targetCapability, detail}` |

配套：`ListEvents` 加 `source != 'route'` 过滤（控制台"事件流"页不该显示剪枝），`trace` 查询不回过滤。

注意：§7 的「上游没回报出口」**不是剪枝**，不写这一行（契约 §7 第 ① 行）。

---

## 7. trace 处理

### 7.1 trace = events + tasks，零新表

| 层次 | 表 | 字段 |
|---|---|---|
| 根 | `events` | `source` / `type` / `payload` / `trace_id` / `created_at` |
| 执行过的 | `tasks` | `trace_id`, `parent_task_id`, `path`, `capability@version`, `node_id`, `assigned_at`, `started_at`, `finished_at`, `status`, `out`, `error`, `summary`, `cap_hash`, `graph_id` |
| **没执行的** | `events`（`source='route'`） | 见 §6.4 |

### 7.2 明确不记录的东西

| 不记 | 理由 |
|---|---|
| `progress.note` | 用户已确认：trace 只要"什么时候、多久、谁、为什么失败" |
| `heartbeat` | 心跳是续租，不是执行 |
| `claim` 的时刻 | 与 `assigned_at` 重合 |
| 中间态的时长分布 | 同上 |

> 因此：**不建 `task_progress` 表，不加 `span batcher`，不需要 JetStream 当 trace sink。** 一条 trace 约 12–14 行。

### 7.3 新增两列（唯一的表变更）

```sql
ALTER TABLE tasks ADD COLUMN assigned_at  INTEGER;   -- 授予时刻（拍下 claim CAS 成功时）
ALTER TABLE tasks ADD COLUMN published_at INTEGER;   -- 发布时刻（§6.3）
ALTER TABLE tasks ADD COLUMN publish_count INTEGER NOT NULL DEFAULT 0;
```

`lease/sweeper` 启动时幂等执行（`store.Open` 的现有迁移模式）。

`assigned_at` 的用途只有一个：画"派发 → 授予"之间的等待（此前只能靠 `lease_until - AckWaitSec` 反推，很脆）。

### 7.4 保留策略

**`Lifecycle.ResultTTLSec` 当前定义了但全仓库没有实现**（没有任何 `DELETE`）。补上：

```
每 1 小时（挂在巡检）：
  DELETE FROM tasks  WHERE org_id=? AND status IN (终态) AND finished_at < now - cap.lifecycle.resultTtlSec
  DELETE FROM events WHERE org_id=? AND created_at < now - org.retention_sec（默认 30 天）
```

`tasks` 的 TTL 按能力定义走；`events` 的 TTL 按 org 走（新增 `orgs.retention_sec`，默认 30 天）。

---

## 8. 鉴权

### 8.1 节点身份（不变）

- token 是 **opaque 字符串**（不是 JWT），`nodes.token_hash = sha256(token)`。今天的设计是对的，不动。
- 鉴权失败一律在 `CONNECT` 阶段拒绝，不允许"连上了再发现没权限"。

### 8.2 连接鉴权：嵌入式 server 的认证回调

```go
opts := &server.Options{
    Websocket: server.WebsocketOpts{Host: "0.0.0.0", Port: 443, TLSConfig: tlsCfg},
    CustomClientAuthentication: func(req server.ClientAuthentication) bool {
        node, ok := lookupByTokenHash(ctx, sha256(req.User()))   // req.User() 填 token
        if !ok { return false }
        req.SetPermissions(permissionsFor(node))                  // 见 8.3
        registerConn(node.ID, req.RemoteAddress())
        return true
    },
}
```

> `SetPermissions` 的确切签名以锁定的 nats-server 版本为准；语义要求是"认证回调内为这条连接设定恰好这些 subject 权限"。

### 8.3 Subject 权限生成

节点 `N` 属于组织 `O`，声明了能力集合 `C = {cap_1..cap_n}`：

```
publish:   anotify.up.<N.id>
           anotify.<$reply-inbox>            # request-reply 的回信箱

subscribe: anotify.node.<N.id>.direct
           anotify.cap.<O>.<cap_id>          for each cap_id in C
```

中心自己的连接（形态 A 用 InProcess，形态 B 用独立 creds）：

```
publish:   anotify.cap.>  anotify.node.>
subscribe: anotify.up.>   _INBOX.>          # 中心也会收到 reply
```

**关键性质**：节点的订阅权限是**枚举**的，不是 `anotify.cap.<org>.>` 通配。所以一个节点拿不到同 org 内它没声明过的能力的消息。

### 8.4 三类绕过尝试与防护

| 攻击 | 防护 | 强度 |
|---|---|---|
| 用 token 订阅别的节点的 `anotify.node.<other>.direct` | ACL 只有自己的 | 强（连接层拒绝） |
| 用 token publish 到 `anotify.up.<other>` 伪造回报 | ACL 只能 publish 自己的 | 强 |
| 声明了能力但不用 queue group 订阅，**抢别人的活** | ⚠️ ACL 管不了 queue group | **中** —— 由 §4.2 的 **claim CAS** 兜底：只有第一个 claim 成功，其余收到 `deny`。恶意节点只能造成"抢到后不干"，那时租约过期重派 |
| 节点一次性把同 org 所有能力都声明了 | 声明需管理员/CLI 操作 | 由运维约束，非协议 |

第三行是设计上要写明的取舍：**ACL 保证"看不到不该看的"，`claim` CAS 保证"抢不走不该抢的"。** 两层职责不同，不能只靠一层。

### 8.5 TLS

| 形态 | 终止点 |
|---|---|
| A | `nats-server` 的 `Websocket.TLSConfig`（即今天的 WSS/443，端口不变） |
| B | cluster 监听 + 边缘 WSS 均需 TLS；节点侧不提供明文选项 |

**不提供明文端口。** 今天的节点是纯出站 WSS/443，这个性质必须保留。

---

## 9. 表变更汇总

```sql
-- 新增
ALTER TABLE nodes ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 1;
ALTER TABLE tasks ADD COLUMN assigned_at   INTEGER;
ALTER TABLE tasks ADD COLUMN published_at  INTEGER;
ALTER TABLE tasks ADD COLUMN publish_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orgs  ADD COLUMN retention_sec INTEGER NOT NULL DEFAULT 2592000;

CREATE TABLE IF NOT EXISTS dispatch_outbox (      -- §6.2
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    trace_id       TEXT NOT NULL,
    parent_task_id TEXT,
    from_capability TEXT NOT NULL,
    out            TEXT NOT NULL,
    params         TEXT NOT NULL DEFAULT '{}',
    path           TEXT NOT NULL DEFAULT '[]',
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_due ON dispatch_outbox(created_at);

-- 删除
DROP TABLE node_instances;
ALTER TABLE tasks DROP COLUMN instance_id;
ALTER TABLE tasks DROP COLUMN lease_until;   -- 不删：租约仍在中心，保留
```

> `lease_until` **保留** —— 租约是中心的，不是 broker 的。
> `DROP COLUMN` 在 SQLite 3.35+ 支持；`store.Open` 的迁移里按现有模式处理。若担心兼容性，可先保留列不用，下一版再删。

---

## 10. 若将来实施：分阶段

> ⚠️ 本节描述的是**方案被重新启动时**的实施顺序，不是当前待办。当前待办是 §14（可扩展性契约）与 §16（与传输决策无关的已确认问题）。

| Phase | 内容 | 依赖 | 验收 |
|---|---|---|---|
| **0** | 剪枝落库（§16.5）、`ResultTTLSec` 保留（§16.6）、图版本缓存、事务合并（§16.1）、`pickNode` 聚合（§16.2） | 无 | `go test ./internal/...` 全绿；`events` 里能查到 5 种 `pruned.*`；`dispatch` 不再每次写 `graph_versions` |
| **1** | `internal/wire`（envelope v1 + 三态 `out` 类型）、`route.Publisher` 保留双实现 | Phase 0 | 新旧实现可配置切换；三态单测通过 |
| **2** | 嵌入式 NATS、claim/result request-reply、未领取重发、删 `NodeHub`/`pickNode`/`node_instances` | Phase 1 | `node_test.go` 的语义断言全部在新链路上重跑通过 |
| **3** | 集群形态、Postgres、保留策略按 org 分档 | Phase 2 | 两副本下节点任意连接、派发可达 |

**Phase 0 与传输层完全无关**，它的全部内容已上提到 §16，作为独立于本决议的待办。

---

## 11. 失败模式（滑坡清单）

| 滑坡 | 早期信号 | 后果 |
|---|---|---|
| JetStream 悄悄进来 | 有人开始配 `max_deliver` / `ack_wait` | L2 失守：重试策略有两个位置，`cap_hash` 钉住失效 |
| 中心开始不记账 | "反正 NATS 会重投，`lease_until` 别维护了" | `abandoned` 消失，"节点死了"和"在算"不可区分 |
| 嵌入版与集群版分叉 | 出现两条派发路径 | 回滚能力丧失，长期维护两套 |
| 删协议代码时丢掉语义测试 | `node_test.go`（1040 行）随 `internal/ws` 一起删 | `out` 三态、`lease_expired` 拒收最先丢，且静默 |
| 剪枝落库永远排下一期 | 每次都说"等 NATS 弄完" | 产品最核心的答案一直答不出，而它只要 30 行 |
| 用强一致队列的直觉调 NATS | 想加"消息保证送达"、加去重表 | 把 `tasks` 已有的能力重做一遍，且做出第二份真相 |

**Kill switch**：Phase 2 期间若发现"必须开 JetStream 才能做对"，停下来检查 —— 说明某处把"派发的可靠性"错当成了 broker 的职责。

---

## 12. 待评审的开放项

| # | 问题 | 备选 | 建议 |
|---|---|---|---|
| Q1 | `acked` 状态是否保留 | 保留（不产生）/ 从九态删除 | **保留**。删契约里的状态是大事，而它现在只是不产生 |
| Q2 | `events` 是否接受混入 `pruned.*` | 混入 / 独立 `prunes` 表 | **混入**。零新表，且 trace 要的正是"发生与没发生排在一起的时间轴" |
| Q3 | 包名 | `pkg/wire` / `internal/wire` | **`internal/wire`** 起步；若将来节点仓库要 import，再提为独立 module |
| Q4 | 节点声明放在 `nodes` 还是 `node_capabilities` | 现状后者 | **保持 `node_capabilities`**，只把写入时机从握手改为 `declare` |
| Q5 | Phase 3 的 Postgres 是否现在定型 | 是 / 否 | **否**。先确保 SQL 不写 SQLite 专有语法（`ON CONFLICT` 两边都有） |

---

## 13. 被否决的替代方案

> 这一节存在的意义：同样的提议会反复出现。把否决理由写下来，下次不必从零推。

### 13.1 节点直接投递到能力 subject（「收了 A 的活才有权发 A 的出口」）

**提议**：中心把 `assign` 发给持有能力 A 的节点后，该节点即获得「发布到 A 的 `outputs` 所对应能力 subject」的权限。于是接力不必经中心，中心彻底退出投递路径。以鉴权兜底防滥用，以客户端（用户侧程序）做精确自限。

**否决理由，两层。**

#### （1）NATS 表达不了这个约束

| 路径 | 为什么不通 |
|---|---|
| 静态 ACL | 粒度是「连接 × 能力」，不是「连接 × 任务」。节点一旦连上，**任何时候**都能发布 A 的任一出口，与它是否收到过 A 的活无关 |
| 运行时改权限 | 只有 operator/JWT 模式（`$SYS.REQ.CLAIMS.UPDATE`），且是**连接级重新认证**：在途消息可能被拒、每次改权限几十毫秒、中心的签名私钥成为新的单点 |
| 服务端逐消息授权 | NATS 不对 payload 做任何校验，没有这个钩子 |

#### （2）粒度错配：ACL 挡不住被担心的三件事

ACL 的粒度是**能力**，而威胁的粒度是**任务**。

| 威胁 | 静态 ACL 下的结果 |
|---|---|
| 重复投递 | 同一出口发 1 万次全部通过 → 1 万个下游任务 |
| 路由放大 | `A@1` 的节点可以发 `clean`，即使它实际只回报了 `fix` |
| 越权 | 节点可以为它**从未参与**的 trace 发布 A 的出口，污染别人的链路 |

#### （3）决定性的一条：重放防护需要 nonce 存储

「同一个出口只能发一次」本质上是「这条 `(trace_id, parent_task_id, port)` 见过没有」。

**「见过没有」= 一张共享去重表。** 而这张表只能由中心持有 —— 节点没有、也不该有 DB 访问权。

> **只要需要重放防护，中心就必须在投递路径上。这不是性能取舍，是冲突解决的必需。**

同理，「重复投递要合并成一次下游执行」也需要唯一写者。**去重的唯一写者 = `tasks` 的唯一写者 = 中心。**

#### （4）保留的那一半

提议里有一半是对的：**中心不该是性能瓶颈。** 但这条要靠两件事落实，而不是靠把中心移出路径：

- 图版本按 org 缓存（Phase 0）→ 派发不再每次读全图 + 写一次
- outbox 批量（§6.2）→ 接力不再每跳单独抢写锁

量化：缓存后的派发路径是「查缓存 + 一次 INSERT + 一次 publish」，单机 SQLite 下足以覆盖本产品的量级（§7.2 已算过：一条 trace ≈ 12 行）。

#### （5）将来若真要移出中心

唯一成立的路：把「谁写 `tasks`」和「谁存 nonce」**一起**挪到共享存储（Postgres），中心变成无状态、可水平扩。

那是 **Phase 3**，不是 Phase 2。而且注意：到那一步，收益来自「共享存储」，不是来自「节点直投」—— 节点直投只是结果，不是手段。

### 13.2 JetStream 当工作队列

**否决**：`max_deliver` / `ack_wait` 会构成**第二个重试策略位置**，与 `capability_versions.lifecycle` 冲突；而 `lifecycle` 是 `cap_hash` 版本钉住的一部分，冲突的直接后果是旧 trace 的结论随时间变形（违反 I8）。

**替代**：`claim` / `result` 走 NATS 原生 request-reply（§4.3），无需 broker 级重投即可获得可靠投递。

**例外的合适位置**：JetStream 可以当**只读遥测日志**（追加 + 保留），不参与任何判定。目前不必要。

### 13.3 MQTT（mochi-mqtt）当传输

**协议层面确实更贴**：5.0 的 `$share`（负载均衡）、`Receive Maximum`（协议级并发上限）、LWT（节点异常断开的协议级信号）、session expiry（离线队列）都是 NATS Core 要自己写代码补的东西。

**否决理由在另外两根轴上**：

| 轴 | 判断 |
|---|---|
| Go 客户端生态 | MQTT 5 的 Go 客户端是短板（`paho.mqtt.golang` 支持不全，`eclipse/paho.golang` 用户更少）。**补不了** |
| broker 成熟度 | mochi 干净、纯 Go、MIT，但更年轻、维护者更少。**broker 嵌在你的进程里，它的 bug 就是你的 bug。补不了** |

**判据**：协议能补的优势（`Receive Maximum` ≈ 20 行 inflight 计数；LWT ≈ 10 行断连处理，`MarkInstancesDead` 本来就在）vs 生态/成熟度不能补的劣势。

**不对称**：NATS 自带 MQTT 网关，选 NATS 没关掉 MQTT 的门；选 mochi 拿不到 NATS 的 queue group / request-reply 语义。

**若日后重新评估，必须实测三件事**：`$share` 共享订阅在 QoS1 下是否去重、`Receive Maximum` 是否真的生效、持久会话的存储实现（内存还是落盘）。

### 13.4 节点自己决定下一个能力

**否决**：`edges` 是唯一路由依据（L1）。一旦节点按自己的理解发布到「下一个能力」，图就从数据变成约定，`trace.html` 的 `buildBaseGraph()` 失去底图，「有边没走」的灰虚线画不出来，`04-edges.html`「不跑也能知道」失效。

**保留**：NATS 承担「派给哪个**节点**」是本文档的核心收益（§5.2），这与「派给哪个**能力**」是两件事，不能合并。

---

## 14. 可扩展性契约

> 本节的目标是：**保持「以后能换」这个选项，而不是现在换。**
> 每条都写成「违反后未来要付什么代价」，便于代码评审时判定。

### 14.1 传输层

| # | 约束 | 违反后的代价 | 怎么检查 |
|---|---|---|---|
| **T1** | 所有派发必须经 `route.Publisher`，任何代码不得直接操作 `ws.NodeHub` | 换传输时要改 N 处，而不是写一个实现类 | `grep -rn NodeHub --include=*.go` 应只出现在 `internal/ws` 与 `server/mux.go` |
| **T2** | 线格式帧类型集中一处：`internal/ws/node_protocol.go` 是唯一来源，禁止在 handler 里临时定义承载线格式的结构 | envelope 会分裂成多份，`out` 三态最先走样 | 评审：新增帧类型只能进 `node_protocol.go` |
| **T3** | `out` 三态只由一个类型表达（`ResultFrame.Out` + `OutPresent` + 自定义 `UnmarshalJSON`） | 每个新结果结构重新发明一次「缺失 vs null」，I5 静默失守 | 评审：任何新的「结果」结构必须复用该类型 |
| **T4** | **身份从连接 / 会话取，绝不从 payload 取** | payload 无认证；换成无连接语义的传输后会被伪造 | `grep -rn 'from\"' --include=*.go` 中不应出现从 payload 读发送者的代码 |
| **T5** | 传输层不承载语义：不进状态机、不做重试、不做去重、不依赖顺序 | 换传输时会把旧传输的语义漏洞固化进新实现 | 评审：`internal/ws` 不得决定任务状态；状态只经 `store` 的 CAS |
| **T6** | 不依赖连接存活做正确性判断：节点死亡必须能从 `tasks` 推导（租约），不能只靠断连回调 | 换传输后断连语义不同（NATS 无 LWT），会静默丢任务 | 评审：新增的「节点在线」判断只影响展示，不影响派发 |

**T6 当前已经是对的，保持它**：`MarkInstancesDead` 只在断连时标实例，任务回收靠租约巡检（`lease/sweeper.go`）。这条正是本方案里最值得保留的性质。

### 14.2 存储层

| # | 约束 | 违反后的代价 |
|---|---|---|
| **S1** | 只用 SQLite 与 Postgres 都支持的语法：`ON CONFLICT` 可用，`INSERT OR REPLACE` 不可用 | 迁库要逐条改写 SQL 并重验语义 |
| **S2** | 迁移只走 `store.Open` 的幂等 `ALTER/CREATE`，不写 SQLite 专有 PRAGMA | 同上 |
| **S3** | 所有租户查询以 `org_id` 打头（I10） | 未来按 org 分片时无法切分（而分片键必须是 org，见 §2） |
| **S4** | 控制台 / API 只经 store 方法读，不在 handler 里直接拼 SQL | 加只读副本或分区时影响面不可控 |
| **S5** | 时间一律 unixepoch 整数，不用字符串时间或数据库函数 | 跨库时间语义差异 |

### 14.3 已经成立，继续保持

- **图是数据**：`edges` + `graph_versions` 可查，`trace.html` 的 `buildBaseGraph()` 靠它画底图
- **版本钉住**：`tasks.cap_hash` / `tasks.graph_id` 指向不可变快照（I8）
- **唯一写者**：任务状态只经 `transitionGuarded` 推进
- **剪枝留痕**：`DispatchOutput.Pruned` 每一条都必须能落库（**目前尚未实现**，见 §16.5）

---

## 15. 容量基线与重估触发

### 15.1 基线（估算，待 §16.7 的 benchmark 校准）

主导变量：`modernc` 驱动的单事务提交速率。`WAL` + `synchronous=NORMAL` 已开（`store.go:33`），瓶颈是**单写者串行**，不是 fsync。

```
写事务/trace ≈ 1 + 2·D + 9.5·T      (D = 派发次数, T = 任务数)
```

| 形态 | 写/trace（T=5, D=3） | 小 VPS（500 tx/s 可用） | 云主机 SSD（1–2k tx/s） |
|---|---|---|---|
| 现状 | ~62 | 8 traces/s | 16–32 traces/s |
| 事务合并后（§16.1） | ~22 | 23 | 45–90 |

**node 数**：现状受 `pickNode` 的 O(目标 × 节点) 查询限制 —— **~50 舒适，~200 变疼，~500 不可用**；改成聚合查询（§16.2）后由连接数决定（数千）。

### 15.2 重估触发条件

| 触发 | 阈值 | 先做什么 |
|---|---|---|
| trace 吞吐饱和 | 持续 > 50 traces/s，或写事务开始排队 | §16.1；仍不够 → Postgres |
| 租户互相拖慢 | 任一 org 的突发导致其他 org 派发变慢 | Postgres（SQLite 无租户隔离） |
| 单 org 节点数 | > 50 | §16.2；仍不够 → NATS |
| 需要多副本 | 出现 HA 或容量硬需求 | **NATS + Postgres 一起**（缺一不可，见 §2） |
| 需要非 Go / 非 WS 节点 | 嵌入式设备、浏览器节点 | NATS（MQTT 网关 / 浏览器 NATS 客户端） |

> **「业务量上来了」不是触发条件。上面五行才是。**
> 没有一行成立时，本方案不启动。

---

## 16. 与传输决策无关的已确认问题

> 本节内容均在代码里查实。按收益 / 风险排序。

### 16.1 事务合并（收益最大）· #141

一次任务执行现在写 **11 次**（每处独立 `BEGIN/COMMIT`，SQLite 单写者按事务抢锁）：

| 环节 | 现在 | 合并后 |
|---|---|---|
| `LeaseInstance` + `AssignTask` | 2 tx | 1 |
| `ack` → `acked` | 1 tx | 可省（需同步调整协议） |
| `progress`：`MarkRunning` + `started_at` + `ExtendLease` | 3 | 1 |
| `result`：两次 `Transition` + 两次 `UPDATE` + `ReleaseInstance` | 5 | 1 |
| **合计** | **11** | **3** |

**收益：×3 吞吐**。并且 `result` 合并成单事务后，**接力空洞（§16.3）自动消失**。

### 16.2 `pickNode` 的 O(N) 查询 · #145

`route/dispatch.go` 里对每个候选节点查一次 `ListInstances`。改成一条 `LEFT JOIN ... GROUP BY` 聚合查询：N 次 → 1 次。node 上限从 ~50 抬到受连接数限制。

### 16.3 接力空洞（**正在丢任务**）· #140

`internal/ws/node_session.go:183`：`ReportResult` 提交终态后，下一跳在**后台 goroutine** 里建，30s 超时，失败只记日志。进程在中间退出 → 任务 `succeeded` + `out=fix`，但子任务不存在，且**无任何对账**（`ListRedispatchable` 只看 `status=pending AND node_id IS NULL`）。

修法二选一：**并入 §16.1 的单事务**（推荐，零新增结构），或加 outbox / reaper。

### 16.4 `sweepOne` 忽略 `status` · #144

`store/task.go:609` 把 `status` 传进了 `sweepOne`，但判决只用 `lc.IsIdempotent()` 和 `attempts`。于是 `assigned && started_at IS NULL`（**确定没开始过、重投安全**）的非幂等任务也被直接 `abandoned` —— 过度保守，白丢任务。

改：`assigned && started_at IS NULL` → 无条件 `pending`。

### 16.5 剪枝不落库（产品缺一块）· #142

`route.PrunedRecord` 全仓库只有两处消费：`api/events.go:161`（塞进 HTTP 响应，一次）与 `source.go:295`（日志）。**没有任何 INSERT。**

于是「为什么这条边没走」只在触发的那一刻可见，事后永远查不到 —— 与 `06-trace.html`「剪枝是静默的，但一定留痕」直接冲突。

建议：复用 `events` 表，`source='route'`，`type='pruned.<reason>'`，payload 写 `{fromCapability, port, targetCapability, detail}`。零新表、零新迁移。

### 16.6 `ResultTTLSec` 定义了但从未实现 · #143

`store/capability.go:33` 有字段，全仓库没有任何 `DELETE FROM tasks` / `DELETE FROM events`。**表只进不出** —— 这才是「量大了会出问题」的真实原因（不是同步写慢）。

### 16.7 缺基准测试 · #146

`internal/store` 只有一个 `BenchmarkListEnabledDevices`。补三个（`CreateTasks` / `Transition` / 整链 `Dispatch`）之后，§15 的估算就能换成真数，也是任何优化前后的对比基线。

---

> **§16 里 16.1 / 16.3 / 16.5 / 16.6 四项不依赖任何架构决定，建议优先于任何传输层工作。**
> 其中 16.3（#140）是正在丢任务，16.5（#142）是产品缺一块——两者都不该等业务量。
> 已全部提为 issue：#140–#146。
