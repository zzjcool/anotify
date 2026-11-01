> ## ⚠️ 已过期（2026-11-01，console-declares epic）
>
> 本文描述的「节点在 `node.yaml` 里自报 manifest/run、中枢只存不执行」的模型
> **已被取代**。现状：
>
> | 旧（本文） | 新 |
> |---|---|
> | `node.yaml` 声明 `capabilities[].run` | `node.yaml` 只剩 bootstrap 四件（name/server/token/instances） |
> | `hello.capabilities[]` + `hello.manifest[]` | `hello.applied[]`（{capability, version, digest}） |
> | `node_capabilities.run_spec` 存命令原文 | 该列已删除（迁移 18）；`node_capabilities` 只从 `hello.applied[]` 派生 |
> | 中枢「只存不执行」 | 中枢仍是**不执行**，但改为**授权部署**一份内容寻址的产物 |
> | I1「中枢 DB 无命令」 | I1 改写为「执行边界」：内容寻址 + 不可变 + 可审计 + 受节点启动参数约束 |
>
> 权威契约在：`.pi-orchestrator/console-declares/{ISSUE,CONTRACT-artifact,CONTRACT-protocol}.md`
> 与 `docs/contracts/CONTRACT.md`。本文保留只为记录当时的设计推理。

# 预案（playbook）与节点声明

> 状态：**设计已定，本期只做 mock 界面**。设计细节落实现时同步进 `CONTRACT.md`。
>
> 一句话：**预案 = 一份可安装的流程零件**（能力定义 + 它们之间的出口 + 建议的触发器），
> 预案页面**体检就绪度**并**逐项告诉你怎么修**；节点用一份 **YAML** 声明自己如何实现这些能力。

---

## 1. 为什么需要它

现在配一条能跑通的流程，要分别去四个页面各配一遍：

| 步骤 | 页面 | 现状 |
|---|---|---|
| ① 能力定义 | 能力 | 手填 `params_schema` / `outputs` |
| ② 出口 | 出口 | 一条条选 `from` / 出口名 / `to` |
| ③ 触发 | 源 | 另建一个源，再把目标能力一个个加进去 |
| ④ 节点实现 | 节点 | **没有任何地方能声明"这台机器怎么跑这个能力"** |

两个真问题：

- **漏了第 ④ 步没有任何提示。** 能力定义好了、边也连好了，跑起来却永远是
  `pruned/no_node`。用户得自己去节点页猜"是不是没有机器声明这个能力"。
- **配置散在四处，没有"这套流程整体是什么"的视图。** 想交付一整套给人用，
  只能口头描述"你去建这三个能力、连这两条边、注册那台机器"。

预案把 ①②③④ 收进一个对象，并给出**体检结论 + 修复路径**。

---

## 2. 预案是什么（以及不是什么）

**是**：一份 JSON/YAML 清单，描述「这条流程需要哪些能力、它们怎么连、由什么触发、
每个能力在节点上该怎么跑」。

**不是**：

- **不是新的执行概念**。预案不产生任务、不参与派发。安装预案只是**批量写入**
  `capability_versions` + `edges`（+ 可选的 `sources`）。派发逻辑一个字不改。
- **不是模板引擎**。预案里的能力是**具体能力**（`name@version`），
  没有变量、没有循环、没有继承（与 I6 的边界一致：不做递归展开）。
- **不是权限单元**。预案属于命名空间（跟着 I11 走），不做预案级 ACL。

### 2.1 manifest 结构

```json
{
  "capabilities": [
    {
      "name": "review",
      "version": 1,
      "outputs": ["needs-fix", "approved"],
      "paramsSchema": { "type": "object", "required": ["changedLines"] },
      "runner": {
        "runner": "bash",
        "command": "./runners/review.sh",
        "timeoutSec": 900,
        "concurrency": 2,
        "env": { "LOG_LEVEL": "info" }
      }
    }
  ],
  "edges": [{ "from": "review", "fromPort": "approved", "to": "run-tests" }],
  "triggers": [{ "kind": "webhook", "types": ["pull_request.opened"] }]
}
```

**`runner` 是预案里最容易被忽略、但价值最高的一段**：它让"缺节点"这件事
从「你自己去想怎么实现」变成「复制这段 YAML 到 node.yaml，重启 agent」。

---

## 3. 就绪体检（派生量，不落库）

预案页面把四类检查**按依赖顺序**跑一遍。全部是查询时算出来的，不存状态
（与 I3 同源：推导量不落库）。

| # | 检查 | 依赖 | 未就绪时给什么 |
|---|---|---|---|
| 1 | **能力定义** | 无 | 「安装定义」（用 manifest 里的 schema/outputs 建） |
| 2 | **节点实现** | 需要 ① | 生成好的 **node.yaml 片段** + 复制 + 去节点页 |
| 3 | **出口** | 需要 ①（两端能力都要有定义） | 「补齐这条边」+ 缺哪几条的清单 |
| 4 | **触发器** | 需要 ①（要能认出入口能力） | 「创建源」+ 预置的事件类型 |

### 3.1 单项状态

| 状态 | 含义 |
|---|---|
| `ok` | 满足 |
| `missing` | 没有（没定义 / 没这条边 / 没这个源） |
| `no-node` | 定义有了，**没有任何节点声明**它能跑（用户点名的最常见情形） |
| `offline` | 有节点声明了，但**节点全部离线** |

`no-node` 和 `offline` 必须分开：前者要去**写 YAML 装机器**，后者只需要
**把机器连上**。合成一个"未就绪"会让人做错事。

### 3.2 总体状态（优先级从高到低）

```
blocked    —— 有 missing 的能力定义（连契约都不全，什么都跑不了）
needs-node —— 定义齐了，但有 no-node / offline
partial    —— 节点齐了，但边或触发器缺
ready      —— 四类全 ok
```

**优先级就是"最该先做什么"**：卡片上只显示一句，不列全部。

### 3.3 触发器的判定要严格

`kind` 匹配**且**该源的 `targets` 里预案含本预案的**入口能力**（没有入边的能力）。

只匹配 `kind` 会把"你有一个 webhook 源"当成"这条流程有触发器了"——
而那个源可能指向完全无关的能力。

---

## 4. 节点声明（`node.yaml`）

节点与中枢解耦、部署在用户侧，所以它的能力**必须由一份配置声明**，不是由中枢下发。

```yaml
# node.yaml —— 节点侧唯一声明入口
node:
  name: laptop-worker
  max_instances: 20
  max_concurrency: 8
  tags: [local, workstation]
capabilities:
  - name: review
    version: 1
    run: bash                 # 执行器类型：本期只接受 bash
    command: ./runners/review.sh
    out: ok                   # 成功时回报的出口名；缺省 = 不回报 → 分支终止
    timeout_sec: 900
    concurrency: 2
    env:
      LOG_LEVEL: info
```

### 4.1 `run` 是必填的一等字段

**本期只有一种执行器 `bash`**：一份可执行命令 + 超时 + 并发 + 环境变量。

但**字段必须显式写出来**，不能隐式默认成 bash。理由：以后要加
`run: agent`（把能力定义成一段 prompt + 模型，而不是一条命令）时，
不需要改结构、也不需要猜老配置的语义 —— 没写 `run` 就是配置错误。

以后的可能形态（**本期不做**）：

```yaml
    run: agent
    prompt: "审查这个 PR 的改动，找出会被漏掉的回归点"
    model: claude-sonnet
    tools: [read_file, run_tests]
```

抽象上两者同构：**输入 = params，输出 = (出口名, summary, artifacts)**。
bash 用退出码/JSON 表达，agent 用自然语言+工具调用表达。

那 v1 的出口名从哪来？`run.out`（可选）。它是**节点自己声明**的固定字符串，
本期不做插值、不做映射，节点也不预判它在服务端是否合法（合不合法由中枢查边时判）。
中枢拿到它之后**只用来选边**：拿能力 + `out` 去查 `edges.from_port`，**不解释内容**，
更不会把它当命令或参数。只有「`exit 0` 且未超时、未取消」才回报 ——
失败、超时、取消一律不回报出口（结果帧里 `out` 键缺席，不是空串），
不能把失败伪装成走通了某条路；未声明 `out` 时行为与之前完全一致。
**不变量 I1 不变**：中枢仍然不知道也不执行命令，`out` 只是节点回报的一个标签。

### 4.2 与不变量 I1（无 argv/shell）的边界

这是本设计**最容易踩红**的地方，必须写清楚：

| 位置 | 能不能有命令 | 为什么 |
|---|---|---|
| `capability_versions`（中枢侧定义） | ❌ | I1：中枢只知道"做什么"，不知道"怎么执行" |
| `node_capabilities.run_spec`（节点上报） | ✅ | 那是**节点自己的事**，中枢**存下来但从不执行** |

所以 `hello` 帧新增一个**可选**字段 `manifest`：

```text
hello {
  capabilities: ["review@1", ...],        ← 老字段，保留（兼容）
  manifest: [ { name, version, run: {runner, command, ...} }, ... ]   ← 可选
}
```

- **不带 manifest** = 老行为（只声明 `name@version`，没有 run_spec），完全兼容
- 带上 = 中枢把 `run` 存进 `node_capabilities.run_spec`，**只用于展示与诊断**
- 中枢拿到 `command` 也**绝不执行**（I1 的意图没变：中枢不是执行者）

`check_contract.py` 的 A2 只扫 `argv|shell|script` 这几个关键词，`command` 不在其中；
但**能不能放**不是由关键词决定，而是由上表决定的。

---

## 5. 数据模型

```sql
CREATE TABLE IF NOT EXISTS playbooks (
    id           TEXT PRIMARY KEY,              -- pb_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,                 -- pr-review（URL 用，命名空间内唯一）
    name         TEXT NOT NULL,                 -- PR 审查
    descr        TEXT NOT NULL DEFAULT '',      -- 说明文字【注意列名是 descr】
    manifest     TEXT NOT NULL,                 -- manifest JSON 原文
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, slug)
);

-- 节点上报的执行方式（只存不执行）
ALTER TABLE node_capabilities ADD COLUMN run_spec TEXT NOT NULL DEFAULT '{}';
```

**预案不持有自己的图**：`manifest` 是"期望"，真实状态永远从 `capability_versions` /
`edges` / `sources` / `node_capabilities` 里读。这样预案和被安装出来的东西
不会各说各话（安装后手工改了一条边，体检会如实反映）。

---

## 6. API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/playbooks?namespaceId=` | 列表，**带体检结论**（每项一行：`status` + `ready/total`） |
| `GET` | `/v1/playbooks/{id}` | 详情：四类检查的逐项结果 + 每项的修复动作 |
| `POST` | `/v1/playbooks` | 从 manifest 创建预案（不安装） |
| `POST` | `/v1/playbooks/{id}/install` | 安装：批量写能力定义 + 边 +（可选）源，**幂等** |
| `DELETE` | `/v1/playbooks/{id}` | 删预案。**不删已安装的东西**（它们已经独立存在了） |

`install` 幂等的含义：已存在的 `(name, version)` 跳过、已存在的边跳过，
返回 `{created: {capabilities, edges, sources}, skipped: {...}}`。

**删预案不删已安装物**：预案是"清单"，不是"所有权"。要拆就明确去删那些能力/边
（不同语义混在一个删除动作里，迟早误删生产配置）。

---

## 7. UI

### 7.1 预案列表 `playbooks.html`

每张卡：预案名 · 一句话描述 · `能力 3 · 出口 3 · 触发器 1` ·
就绪度 `4/6 就绪` 的细进度条 · 主状态徽标（就绪/缺节点/缺定义/不完整）·
**「最该先做的一件事」**一行（这是全页最有价值的一行）。

### 7.2 预案详情 `playbook.html?id=…`

顶部：名称 + 状态 + 「重扫」（体检是实时的，重扫只是重新取一次）+「安装/补齐」。

四张卡，每张一个小标题 + `n/m` + 逐项结果；不只列问题，**每项都带一条可执行的下一步**：

| 卡 | 一行内容 | 未就绪时 |
|---|---|---|
| 触发器 | `webhook · pull_request.opened` | 「创建源」→ 源页 |
| 能力定义 | `review@1 · needs-fix, approved` | 「安装定义」 |
| 节点实现 | `review@1 → laptop-worker` | **node.yaml 片段 + 复制** |
| 出口 | `review --approved--> run-tests` | 「补齐这条边」 |

节点卡里生成的 YAML **只含缺的那几个能力**，直接可粘 —— 不是把整份文件吐出来让人删。

---

## 8. 明确不做

| 不做 | 为什么 |
|---|---|
| 预案的版本/升级（v1 → v2 迁移边） | 需要"图迁移"这套机制，是独立话题 |
| 预案之间的依赖（预案 A 引用预案 B 的能力） | 依赖解析 + 环检测 + 卸载顺序，收益不抵复杂度 |
| `run: agent` 执行器 | 本期只做 bash；但**字段现在就留好**（§4.1） |
| 从现有图上「反向抽出预案」 | 有价值，但需要先定"什么算一个预案"的判据，容易做出四不像 |
| 预案级 ACL / 配额 | 跟着命名空间走，不额外分层 |

---

## 9. 开放问题

1. **预案能不能只引用已存在的能力定义**（而不是自带定义）？倾向：**能**，
   manifest 里 `capabilities[].runner` 可省略，省略时该能力只能靠已有节点实现。
   本期 mock 里全部自带定义，先不做这个分支。
2. **`install` 遇到"同名能力但 schema 不同"怎么办**？倾向：`409` 并给出差异
   （append-only 意味着同名同版本不能改，只能升版本 —— 这该由人来决定）。
3. **触发器声明要不要支持 `github` 这种"投递格式"字段**？现在 `kind` 混着讲
   （github / webhook / cron / manual）。倾向：预案的 trigger 直接用 `kind` + `types`，
   投递格式由源自己决定，预案不关心。
