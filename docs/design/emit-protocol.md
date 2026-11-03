# emit 协议（节点 ↔ 中枢）

> 状态：**冻结**（P1 的实现规范）
> 上游设计：`docs/design/typed-pipeline.md` §2（emit 是唯一产出通道）、§3.4（catalog）、§5（校验矩阵）
> 本文只写**线格式、时序、事务边界、错误码**。模型为什么这样，看上游设计。

---

## 0. 进程树

```text
中枢（control plane + 唯一写者）
   │  WS: assign / emit / emit_ack / result
   ▼
anotify-agent（每台边缘节点一个进程）
   │  unix socket（父子进程，同机）
   ▼
anotify-agent emit  ← 能力进程（脚本 / LLM agent）调用
```

**能力进程不持有中枢凭证**，也不直连网络。它只能通过本地 socket 说服 agent 代发。

---

## 1. 本地 socket

| 项 | 值 |
|---|---|
| 路径 | `<data-dir>/emit/<task_id>.sock`（`data-dir` 默认 `~/.anotify`） |
| 协议 | 每连接一行 JSON 请求 → 一行 JSON 响应（换行分隔，不设长度前缀） |
| 注入 | agent 起能力进程前写环境变量：`ANOTIFY_EMIT_SOCK`、`ANOTIFY_TASK_ID`、`ANOTIFY_TASK_TOKEN` |
| 鉴权 | 请求里必须带 `token`，与 `ANOTIFY_TASK_TOKEN` 一致（每任务随机，任务结束即销毁） |
| 生命周期 | socket 与该任务同生命周期：进程退出 / 任务终结 / agent 关闭 → 关闭并删除 |

请求：

```json
{"op":"emit","token":"…","seq":3,"port":"fix","data":{"issue":"#42"},"note":"optional"}
```

响应：

```json
{"ok":true,"seq":3,"acked":"durable"}
{"ok":false,"seq":3,"acked":"","error":"…","rejected":"port|schema|size|rate|task|token"}
```

`acked` 取值：`durable`（已本地落盘，默认）| `routed`（已由中枢派发完，仅 `--wait-routed`）。

---

## 2. CLI（复用 `anotify-agent`）

```bash
anotify-agent emit --port fix --data '{"issue":"#42","title":"空指针"}'
anotify-agent emit --port fix --data @- < payload.json     # 从 stdin 读
anotify-agent emit --port fix --data '…' --wait-routed     # 等中枢派发完
anotify-agent emit --port fix --data '…' --note '第 2 项'   # 人类可读备注
```

退出码（**实现应当据此决定重试还是停止**）：

| 码 | 含义 | 调用方该做什么 |
|---|---|---|
| 0 | 成功 | 继续 |
| 2 | 参数/请求形状错（不是 JSON 对象、--port 为空、超体积） | 修，别重试 |
| 3 | 被拒：`port` 不在 `ports[]` | 修（换端口），别重试 |
| 4 | 被拒：数据不满足 `expects[port].schema` | 补齐字段后重发（同一次运行可以重发） |
| 5 | 任务已非运行态（cancel / 租约过期 / 已终结） | 停止执行 |
| 6 | 与 agent 通信失败 / token 无效 / socket 不存在 | 可重试一次，仍失败则停止 |
| 7 | ack 超时（`--wait-routed` 或默认路径的本地落盘失败） | 可重试（中枢按 `(task_id, seq)` 幂等） |

> `--async` 不存在。fire-and-forget 会让「emit 成功但中枢没收到」变成静默丢事件；
> 需要异步语义时的正确做法是**本地 outbox（agent 内建）**，而不是让调用方放弃等待。

---

## 3. 帧

### 3.1 emit（节点 → 中枢）

```jsonc
{
  "type":    "emit",
  "task_id": "tsk_8f21a3c0",
  "seq":     3,              // 该任务内单调递增，从 1 开始
  "port":    "fix",
  "data":    { "issue": "#42" },   // 必须是 JSON 对象；可缺省 = {}
  "note":    ""                    // 可选，人类可读；不参与路由
}
```

### 3.2 emit_ack（中枢 → 节点）

```jsonc
{
  "type":    "emit_ack",
  "task_id": "tsk_8f21a3c0",
  "seq":     3,
  "routed":  2,       // 派发出去几条下游
  "pruned":  0,       // 剪枝条数（no_node / foreign_user / path）
  "error":   ""       // 非空 = 拒收；节点把等价错误回给 CLI
}
```

`emit_ack` 的 `error` 是**幂等**的：重复 `seq` 一律回 `ok`（`routed`/`pruned` 取首次结果），
不因为重发而重复派发。

---

## 4. seq 与幂等

- 每个任务一个计数器，agent 侧持有并持久化（随 outbox 一起落盘）。
- 中枢侧唯一约束：`UNIQUE(task_id, seq)`。
- **缺口策略**：中枢收到 `seq = n` 而库里没有 `1..n-1` → 回 `error`（`seq gap`），
  节点据此重发缺失项。**不猜测、不跳过、不等待超时**——缺口是丢消息的证据，必须可见。
- 重复 `seq`（内容相同）→ 幂等成功。
- 重复 `seq`（内容不同）→ `error`（`seq reused with different payload`）。这是节点 bug，
  不能静默覆盖。

---

## 5. 两级 ack

| 模式 | CLI 何时返回 | 语义 | 代价 |
|---|---|---|---|
| 默认 | agent 已把 emit 追加进本地 outbox（fsync） | **已持久收下**，agent 崩溃重启后会重发 | 毫秒级 |
| `--wait-routed` | 收到中枢 `emit_ack` 且 `error` 为空 | **已派发**（下游任务已建行） | 一次 RTT |

为什么默认不是「已派发」：能力进程（尤其 LLM）会在一次运行里 emit 很多次，每次都等 RTT 会把
长任务拖慢一个数量级。而「不丢」由 outbox + `(task_id, seq)` 幂等保证，不靠同步等待。

---

## 6. 校验顺序（agent 侧，投递时）

1. `token` 匹配当前任务 → 否则 `task`（exit 6）
2. 任务仍在运行态（未 cancel / 租约未过期）→ 否则 `task`（exit 5）
3. `port` ∈ 本次 `assign.ports` → 否则 `port`（exit 3）
4. `data` 是 JSON 对象 → 否则 `schema`（exit 2）
5. `data` 满足 `assign.expects[port].schema`（若该 port 有 expects）→ 否则 `schema`（exit 4）
   - `expects[port].schema` = 该 port 所有下游 `params_schema` **减去各自 literals** 的并集
   - 没有下游（0 条 link）= 没有 expects → 只做结构校验
6. 体积 ≤ 64 KiB → 否则 `size`（exit 2）
7. 本任务累计 emit 次数 < 1000 → 否则 `rate`（exit 2）

> 中枢侧**仍会**在派发时对每条 link 再校验一次下游 `params_schema`（第二道，防拓扑在
> emit 与派发之间变化）。但正常情况下不该再触发——参数问题应该在 emit 时就被挡住。

---

## 7. 中枢侧事务边界

一次 `emit` 的处理（**与 #140 的接力原子性同一套**）：

```text
BEGIN
  INSERT INTO emissions(task_id, seq, port, data, status, created_at)
      ON CONFLICT(task_id, seq) DO NOTHING        -- 幂等
  校验 seq 连续性（缺口 → 回 error，事务不写别的）
  查 link(from=capability, port) → 目标列表
  为孩子分别解析 params（emit.data ⊎ literals）并校验下游 params_schema
  INSERT 子任务行（petding）+ 父任务/子任务终态记账
COMMIT
再 publish（推 assign 帧）；publish 失败 → 任务留在 pending，由巡检重派
```

- **落库与派发必须在同一事务里**（否则进程崩溃 = 静默丢一条 emit）。
- publish 在事务外（网络不可回滚），失败不影响已落库的事实。
- outbox 批量：agent 断开时 emit 留在本地 outbox；重连后按 seq 顺序补发。

---

## 8. 边界情况

| 情况 | 行为 |
|---|---|
| 任务已 cancel / 租约过期 | `emit` 被拒（exit 5）。已落库的 emissions 保留（审计） |
| 能力进程退出后才到的 emit（socket 已关） | 拒绝（socket 不存在 → exit 6）。不允许晚到的 emit 改动已终结的任务 |
| `anotify-agent run`（本地模式，不连中枢） | 没有 socket：`emit` 直接失败并提示「本地模式无中枢，emit 不可用」 |
| emit 的目标能力没有任何在线节点 | 归入 `pruned: no_node`（不是 emit 失败）；`emit_ack.routed=0` |
| 环 / 深度剪枝 | 归入 `pruned: path` |
| 跨组织用户校验失败 | 归入 `pruned: foreign_user` |
| 同一 port 的 3 条 link 中 1 条无节点 | 该条剪枝、其余派发（扇出可部分成功——这条和参数无关，保留） |
| agent 在 emit 与 ack 之间崩溃 | outbox 里有记录；重启后重发同一 seq → 中枢幂等，不重复派发 |

---

## 9. 明确不做

| 不做 | 理由 |
|---|---|
| fire-and-forget / `--async` | 静默丢事件 |
| 能力进程直连中枢 | 凭证下发到沙箱里，等于把路由权交给不可信代码 |
| emit 携带「调用哪个能力」 | 路由权在中枢；端口名是唯一的路由输入（I2） |
| 跨跳上下文（`steps[]`） | 窄接口：下游只看直接上游（`data`） |
| 节点自己投递 | `nats-transport.md` §13.1/§13.4 已否决；去重需要唯一写者 |
