# 接口层：冻结契约（本轮唯一事实源）

> 已在 2026-10 由用户确认四条决策。所有页面按这份契约改。

## 1. 为什么加这一层

旧模型里出口名由执行器自己编：

```yaml
# 机器 A
capabilities:
  - name: review
    outputs: [fix, clean]

# 机器 B（同一个名字）
capabilities:
  - name: review
    outputs: [ok, bad]
```

两个 `review` 出口不同 → `edges` 表里 `(review, fix)` 和 `(review, ok)` 并存 → `from_port` 失去唯一性，查表无解。

**根因：出口命名权在执行器。** 执行器只该说「我实现了什么」，不该编出口名。

## 2. 四层

```text
接口 interface     ← 契约。中枢侧定义，版本化
   ↑ implements
能力 capability    ← 执行器对某接口的实现（runner_id + interface_id）
   ↑ declares
执行器 runner      ← 一台机器
   ↑ 组成
图 graph           ← edges 表，纯接口层的边，与机器无关
```

**关键：`edges` 是接口到接口的边，不引用任何机器。** 换机器不影响图。

## 3. 接口定义

```yaml
# 中枢侧 · interfaces/ （进 git，走 review）
- name: review
  version: 1
  kind: pipe                      # 该接口的角色：origin / pipe / end
  params_schema:                  # JSON Schema，派发前校验
    type: object
    required: [pr]
    properties:
      pr: { type: integer }
  outputs: [fix, clean]           # ★ 出口名在这里定死

- name: notify.phone
  version: 1
  kind: end
  params_schema:
    type: object
    required: [title]
  outputs: []                     # 终点，没有出口

- name: github
  version: 1
  kind: origin                    # origin 也走接口
  outputs: [pr_opened, pr_merged, issue_opened]
```

### 3.1 内置 + 用户自定义

| 来源 | 位置 | 谁维护 |
| --- | --- | --- |
| **内置接口** | `interfaces/builtin/`（随项目发布） | 项目 |
| **自定义接口** | 中枢配置目录，用户自己加 | 用户 |

两者同一套格式、同一套版本化，加载后无区别。

### 3.2 内置清单（本期）

| 接口 | kind | outputs |
| --- | --- | --- |
| `github` | origin | `pr_opened` / `pr_merged` / `issue_opened` |
| `cron` | origin | `tick` |
| `watch` | origin | `changed` |
| `http` | origin | `received` |
| `review` | pipe | `fix` / `clean` |
| `implement` | pipe | `verify` / `give_up` |
| `test` | pipe | `pass` / `fail` |
| `notify.phone` | end | — |
| `notify.webhook` | end | — |
| `log` | end | — |

## 4. 执行器怎么声明

```yaml
# 执行器本地 ~/.anotifyd/config.yaml
runner: mbp-16
kind: pipe
capabilities:
  - implements: review@1        # ★ 只写实现了哪个接口
    argv: ["git", "diff", "--name-only"]
  - implements: implement@1
    argv: ["./agent.sh"]
```

**执行器不再写 `outputs`。** 出口集合由接口给定。

`argv` 永不上传。上报到中枢的只有：

```json
{ "kind": "pipe", "implements": [{"interface": "review", "version": 1}] }
```

## 5. 版本：两个版本 = 两个接口

**决策**：`review@1` 和 `review@2` 是**两个不同的接口**，可以并存。

- 旧机器继续实现 `review@1`
- 新机器实现 `review@2`
- `edges` 里分别有指向两者的边
- 迁移期两条并存，不强制全量升级

好处：升级不中断。代价：图里同名接口可能出现两个版本，控制台必须**显式显示版本**，不能只显示 `review`。

## 6. 不允许实现子集

**决策**：实现 `review@1` 就必须支持它的**全部** `outputs`（`fix` 和 `clean`）。

理由：允许子集会让 `edges` 里出现「这台机器没这个出口」的分叉，查表命中却派不出去。要单独能力就定义新接口。

## 7. 校验

注册时（执行器上报 manifest）：

| 检查 | 不通过 |
| --- | --- |
| 接口存在 | `unknown_interface`，拒绝该能力 |
| 版本受支持 | `unsupported_interface_version` |
| `kind` 与接口声明一致 | `kind_mismatch`（`notify.phone` 只能被 `end` 实现） |

派发时：

| 检查 | 不通过 |
| --- | --- |
| `params` 符合 `params_schema` | 记 `pruned / reason=param_schema`，**不建任务** |
| 有执行器实现了目标接口 | 任务保持 `pending`，按租约排队（不是错误） |

静态检查（配置期）：

| 检查 | 不通过 |
| --- | --- |
| 每条边的 `from_port` 属于 `from_interface.outputs` | 拒绝加载，指出哪一行 |
| 每条边的 `to_interface` 存在 | 拒绝加载 |
| 每条边的两个端点 `kind` 相容 | `origin` 只能作为起点，`end` 只能作为终点，不允许 `end → x` 或 `x → origin` |

## 8. 表结构

```sql
-- 接口版本（append-only）
CREATE TABLE interface_versions (
  id            TEXT PRIMARY KEY,   -- ifc_<UTC>_<hash8>
  name          TEXT NOT NULL,      -- review
  version       INTEGER NOT NULL,   -- 1
  kind          TEXT NOT NULL,      -- origin | pipe | end
  params_schema TEXT NOT NULL,      -- JSON
  outputs       TEXT NOT NULL,      -- JSON 数组
  source        TEXT NOT NULL,      -- builtin | custom
  content_hash  TEXT NOT NULL UNIQUE,
  created_at    TEXT NOT NULL,
  UNIQUE (name, version)
);

-- 执行器实现了哪些接口（argv 不在中枢）
CREATE TABLE capabilities (
  runner_id     TEXT NOT NULL,
  interface_id  TEXT NOT NULL,      -- → interface_versions.id
  PRIMARY KEY (runner_id, interface_id)
);

-- 边：纯接口层，不引用机器
CREATE TABLE edges (
  from_interface TEXT NOT NULL,     -- ifc_...（review@1 的 id）
  from_port      TEXT NOT NULL,     -- fix
  to_interface   TEXT NOT NULL,     -- ifc_...（implement@1 的 id）
  PRIMARY KEY (from_interface, from_port, to_interface)
);
```

**注意**：`edges` 存的是 `interface_id`，不是 `name@version`。展示时 join 出名字。
这样接口改名、加版本都不影响已有的边。

## 9. 派发流程（改了）

旧：查 edges → 得到 to_capability → 挑机器
新：**四步**

```text
1. 查 edges(from_interface, from_port) → to_interface（N 行就是 N 个任务）
2. 用 to_interface 的 params_schema 校验 params   ← 新增，前置校验
3. 查 capabilities WHERE interface_id = to_interface → 候选执行器
4. 按负载挑一台（或按 sticky 查 runner_pins）→ 建任务
```

## 10. 命名

| 概念 | 名字 | 代码 |
| --- | --- | --- |
| 契约 | **接口** interface | `interface` / `interface_id` |
| 接口的具名引用 | `name@version` | 如 `review@1` |
| 执行器对接口的实现 | **能力** capability | `capabilities` 表 |
| 角色 | origin / pipe / end | `kind` |

**「能力」不再等于「接口」。** 能力 = 某台机器实现了某个接口。路由的单位是**接口**，不是能力。

## 11. 各页要改什么

| 页 | 改动 |
| --- | --- |
| 02 核心概念 | 名词从 5 个变 6 个：Interface / Capability / Output / Task / Trace / Role；关系图加接口层 |
| 03 一条事件的旅程 | 第 3 步「查表」前加参数校验；第 5 步「派发」改成「按接口找执行器」 |
| 04 能力与出口 | 三要素 → 四要素（接口/能力/出口/边）；三方声明 → 四方；edges 改 interface_id |
| 05 执行器与执行 | 「能力清单」节改为 implements；新增接口注册与校验 |
| 06 可观测与版本 | 版本钉住从三对象 → 四对象（加 interface_versions） |
| 07 安全边界 | 参数校验前移；未定义接口 → unknown_interface |
| 08 本期范围 | 内置接口清单；自定义接口；版本并存策略 |
| 09 术语表 | 加 Interface / name@version / 内置接口 / kind_mismatch 等 |
| index | 五层结构图加接口层 |
