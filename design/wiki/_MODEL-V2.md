# Anotify 模型 v2 · 统一节点模型（唯一事实源）

> 本文件取代之前的「规则引擎」模型。**规则（rules / match 表达式）已删除**，改为显式的出口表 + 查表路由。

## 1. 一句话

**一切参与流转的东西都是节点。节点申报自己的能力，能力带出口，出口指向下一个能力。派发就是查表。**

## 2. 统一抽象：三个要素

| 要素 | 含义 |
| --- | --- |
| **节点 Node** | 参与流转的一个实体。可以是触发器、一台机器上的一个能力、或一个终端。 |
| **能力 Capability** | 节点申报的「我能做什么」。**路由的单位是能力，不是机器。** |
| **出口 Port** | 能力的出口，一个命名的点。跑完之后节点选择走哪个出口。 |

## 3. 三种节点，按端口形状分

| 种类 | 入 | 出 | 例子 | 代码值 |
| --- | --- | --- | --- | --- |
| **源 / 触发器** | ✗ | ✓ | GitHub webhook、cron、watch、外部 API | `source` |
| **工作节点** | ✓ | ✓ | 机器上的 `review` / `implement` / `test` | `worker` |
| **终端 / 叶子** | ✓ | ✗ | 推手机（human sink）、写日志 | `sink` |

**没有第四种。** 任何东西落进这三类之一。

## 4. 出口表（edges）：路由就是查表

```sql
-- 中枢侧唯一的路由依据
CREATE TABLE edges (
  from_capability TEXT NOT NULL,   -- 谁产的
  from_port       TEXT NOT NULL,   -- 走了哪个出口
  to_capability   TEXT NOT NULL,   -- 下一个能力
  PRIMARY KEY (from_capability, from_port, to_capability)
);
```

派发：

```sql
SELECT to_capability FROM edges
 WHERE from_capability = :cap AND from_port = :port;
-- 返回 N 行 → 派 N 个任务（全部下发，不是择一）
```

**没有 match、没有条件表达式、没有优先级、没有求值。** 一次索引查询。

### 4.1 触发器怎么进这张表

触发器没有能力（没有入），它有的只是**出口**。所以它的 `from_capability` 用事件类型充当：

```text
source:github  --(pr_opened)--> [review, notify]
source:cron    --(tick)-------> [healthcheck]
```

节点侧声明（`~/.anotifyd/config.yaml`）：

```yaml
node: gh-webhook
kind: source
emits:
  pr_opened:
    to: [review, notify]
  pr_merged:
    to: [notify]
```

中枢只收到「`source:github` 的 `pr_opened` 通向 review / notify」这些**事实**，不收到任何 webhook 逻辑。

### 4.2 工作节点怎么声明

```yaml
node: mbp-16
kind: worker
capabilities:
  - name: review
    argv: ["git", "diff", "--name-only"]
    outputs: [fix, done]        # 我可能往哪走（跑完才知道选哪个）
  - name: implement
    argv: ["./agent.sh"]
    outputs: [verify, done]
```

**`outputs` 是「我会用到的出口名」，不是「我会去哪」。** 去哪由中枢的 edges 表决定——节点不该知道图长什么样。这是关键的分工。

### 4.3 终端怎么声明

```yaml
node: my-phone
kind: sink
capabilities:
  - name: notify.phone
    outputs: []                 # 没有出口
```

## 5. 运行时：节点选出口

节点跑完后回报：

```jsonc
{
  "status": "succeeded",     // succeeded | failed
  "out": "fix",              // ★ 我选了哪个出口；null = 到此为止
  "summary": "...",          // ≤8KB
  "artifacts": [...]
}
```

**`out` 取代了之前的 `terminal` 布尔字段。** 因为它同时表达了：
- 有没有下游（`null` = 没有）
- 有哪个下游（`"fix"` = 走这条边）

而且 `status` 和 `out` **正交**：

| status | out | 含义 |
| --- | --- | --- |
| `succeeded` | `"fix"` | 干完了，往下走 |
| `succeeded` | `null` | 干完了，到头了 |
| `failed` | `"fix"` | 失败，但触发修复分支 |
| `failed` | `null` | 失败且断在这里 |

**为什么必须是跑完才知道**：命令执行前，没有任何人知道它会走到哪个出口。成功要跑完才知道，失败也要跑完才知道。所以 `out` 只能出现在 `result` 里。

## 6. 收敛判定

```text
trace.settled ⟺
  ① 无 pending / assigned / acked / running
  ② 无未到期任务（expires_at / not_before 全在过去）
  ③ 每个终态任务都已有 out 回报（null 或具体值都算「已回报」）
```

缺失 `out` → 按「未回报」计入（保守，不收敛），直到 `max_duration` 兜底。

**竞态**：不能扫描成立就发通知。要写 `pending_settle` 标记 → 下一轮复核 → 仍成立才发。否则 trigger 事务还没提交，手机已经先响了。

## 7. 环

图是**显式声明**的，所以环在配置期就看得见（可以画出来、可以静态检查）。

**允许环**（`review → fix → review` 是真实场景）。运行期仍靠 `path` 去重剪枝：

- `path` = **从根到本任务这条具体路径**的能力名序列（不是全 trace 集合）
- `path` 里已有该能力 → **静默剪枝**，不报错、不暂停 trace
- 但**必须在 trace 上可见**（记 `pruned` + 原因）

终止性由 path 去重保证（路径最长 = 图里的能力数），`max_hops` 只是外显上界。

## 8. 可解释性（比之前更强）

之前要解释「规则为什么没命中」，需要逐条件求值。现在只需要回答一件事：

```text
能力 review 走了出口 fix
  → edges 表里有没有 (review, fix) 这一行？
    → 有 → 派给 implement
    → 没有 → 这里就是尽头，理由是「表里没有这条边」
```

**「为什么没触发」= 「表里没有这一行」，或者「上游没有回报这个出口」。** 两个都能直接展示，不需要推理。

## 9. 术语表（最终版）

| 术语 | 代码 | 定义 |
| --- | --- | --- |
| 节点 | `node` | 参与流转的实体 |
| 种类 | `kind` | `source` / `worker` / `sink` |
| 源、触发器 | `source` | 只有出口的节点，产生事件 |
| 工作节点 | `worker` | 有进有出的节点，执行能力 |
| 终端、叶子 | `sink` | 只有入口的节点，不产生下游 |
| 能力 | `capability` | 节点申报的「我能做什么」，路由单位 |
| 出口 | `output` / `port` | 能力的命名出口 |
| 边 | `edge` | `(能力, 出口) → 能力`，路由表的一行 |
| 出口表 | `edges` | 全部边的集合，路由的唯一依据 |
| 派发 | `dispatch` | 查表得到 N 个能力，全部建任务 |
| 回报 | `report` | 节点跑完提交 status + out |
| 落定 | `settled` | 一条链再也没有会继续的节点 |
| 剪枝 | `pruned` | 因 path 去重而不建任务 |
| 行程 | `path` | 从根到本任务的能力名序列 |

**已废弃的旧术语**（不要再出现）：

| 废弃 | 原因 |
| --- | --- |
| `rule` / 规则 | 被 edges 表取代，没有 match 求值 |
| `match` / 条件表达式 | 不再需要，路由靠查表 |
| `priority` | 不再需要，全部下发不择一 |
| `action` | 被「能力 + 出口」取代 |
| `terminal`（布尔） | 被 `out`（出口名）取代，一个字段表达两件事 |
| `manifest.commands` | 改名 `manifest.capabilities` |

## 10. 不变的部分（本轮没动）

- 任务九态状态机、租约 30s / 心跳 10s / `max_attempts` 3 / 退避 10s→60s→300s
- `jti` 一次性消费 + `branchBudget` 扇出额度
- 中枢永不下发命令（无 argv 字段、execve、禁 `sh -c`）
- 节点池两级模型、三层并发 min、最少负载优先
- 粘性路由（按能力声明 `sticky` 开启，默认纯均衡）
- 版本钉住三对象（`content_hash`），trace 钉 hash 不钉时间
- 六道闸 G1/G2/G3/G4/G6（G5 废弃）
- 两类 sink 永不混用
- 本期不做：收集/join、batching、拖拽画布、任意代码节点、中枢存密钥

## 11. 本期范围不变

扇出做、收集不做。没有 join 时「等一组都完成」放节点内部实现。
