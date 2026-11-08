# Source Spec v4 · 源（触发器）

> 状态：**讨论中**（2026-11-08）。标记 ⚠️ 的地方是待定项，尚未定稿。
> 配套：`capability-spec-v4.md`（能力）、`playbook-*`（安装包）。

---

## 0. 定位

**源 = 触发器 = 事件的入口。** 它回答「什么时候开始干」，能力回答「能干什么」。

三条硬性质：

1. **源不是能力**：它不执行代码、不产生任务行、不需要节点，因此也不能出现在 `next` 里。
2. **源不属于命名空间之外的任何层**：一个源属于一个命名空间（和能力的隔离维度相同）。
3. **源与能力只通过「事件」相连**：源触发 → 产生一条事件 → 事件按入口边交给目标能力的 params。中间没有任何直接调用。

用户词（`capability-spec-v3.md §13`）：**源**。英文 Source。界面不叫「触发器」「入口能力」。

---

## 1. 四种 kind

| kind | 什么时候触发 | 谁触发 |
|---|---|---|
| `cron` | 按五段 cron 表达式到点 | 中枢调度器（`nextFireAt`） |
| `webhook` | 外部系统 POST 到唯一 URL | 外部系统 |
| `github` | GitHub webhook 投递 | GitHub（`webhook` 的「知道载荷长什么样」版本） |
| `manual` | 用户在页面上点一下 | 人 |

`webhook` 与 `github` 后端校验完全同一条路径，差别只有两处：**前端给哪份字段清单**、**用哪个事件名**（GitHub 放在 `X-GitHub-Event` 头里）。

合法值就这四个（`store.SourceKinds`），别的 kind → `invalid source kind`。

---

## 2. YAML 形状

```yaml
apiVersion: anotify/v1
kind: Source
metadata:
  name: on-pull-request            # 命名空间内唯一（重名 → source name already used）
spec:
  kind: github                     # cron | webhook | github | manual
  enabled: true                    # 关掉 = 不触发（webhook 也不收）

  config:                          # 见 §3，按 kind 解释
    event: [pull_request]

  targets:                         # 见 §4
    - capability: review
      params:
        repo: "{{payload.repository.full_name}}"
```

---

## 3. `config` 字段

### 3.1 kind 专属

#### `cron`

| 字段 | 类型 | 必填 | 默认 | 约束 |
|---|---|---|---|---|
| `expr` | string | ✅ | — | 五段 cron 表达式，**创建时真的解析一遍**（`61 * * * *` 当场拒，不等到触发才发现永不运行） |
| `tz` | string | 可选 | `Local` | 时区名 |

#### `webhook` / `github`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `token` | string | 可选 | 决定唯一 URL：`/v1/hooks/<token>`。**空 = 创建时服务端随机生成**。它本身是凭据：谁拿到 URL 谁就能往里发事件，所以支持轮换 |
| `event` | []string | 仅 github | 订阅哪些 GitHub 事件（`push` / `pull_request` / `issues` / `release` / `workflow_run` …）。决定前端给哪份字段清单，也决定本次投递用哪个事件名 |

#### `manual`

没有 kind 专属字段。

### 3.2 四种 kind 通用

| 字段 | 类型 | 说明 |
|---|---|---|
| `vars` | array | 这个触发方式**能提供哪些字段**的声明（`{name, from}`）。前端据此做变量提示，后端逐项校验（出错会指出是哪一项） |
| `resultSchema` | JSON Schema | 事件 payload 的契约。**每次投递都要在派发前过它**，不合法 → 剪枝（不是收下来再爆）。缺省与显式 `null` 都算「不声明」；数组/标量/字符串一律拒 |
| `payload` | object | **静态打底数据**，只有 `cron` / `manual` 保留。webhook / github 的 payload 是投递进来的请求体，存到 config 里没有意义 |

---

## 4. `targets` —— 事件送给哪些能力

| 字段 | 类型 | 说明 |
|---|---|---|
| `capability` | string | 目标能力名（多行 = 扇出） |
| `params` | object | 参数模板。可写常量，也可写 `{{payload 路径}}`；空对象 = 不额外传参 |

⚠️ **待定 D1**：v4 里能力→能力已改成「同名直取、无 mapping」，但**源→能力仍保留模板**，因为事件 payload 的形状**不由我们决定**（GitHub 的 `payload.repository.full_name` 这种嵌套没法靠改字段名解决）。三个选项待定：

1. 保留模板（现在这样）
2. 只允许 `params` 里写 `{{字段}}`，不允许任意表达式（收紧）
3. 也强制同名直取，能力侧自己适配外部形状（最狠，会逼所有能力写得跟 GitHub 一样）

---

## 5. 运行期行为

```yaml
# 一次触发发生了什么
源触发（cron 到点 / webhook 被 POST / GitHub 投递 / 人工点击）
  → events 插一行：{source: <源名>, type: <事件类型>, payload: <请求体>}
  → 按入口边找到目标能力（from_is_source = 1, from_ref = <源名>）
  → 对每个 target 求值 params（模板 + payload）
  → 目标能力入参校验 → 建任务（或剪枝留痕）
```

两个细节：

- **事件类型**：写进 `events.type`。传空时用**源自己的名字**。
- **入口边匹配**：`from_port IN (事件类型, '*')`。控制台建的 target 恒为 `'*'`（收全部事件类型），所以现在**不按事件类型分流**。

⚠️ **待定 D2**：要不要让 target 声明「只响应某个事件类型」。现在恒收全部，能力只能从事件里自己判断。

---

## 6. 校验与错误

| 情况 | 结果 |
|---|---|
| kind 不在四种里 | `invalid source kind` |
| config 不合法（cron 表达式、vars 项、resultSchema 形状） | `invalid source config`，消息点到具体项 |
| 重名 | `source name already used` |
| 命名空间不存在 | `source not found` 系列 |

投递期：

| 情况 | 结果 |
|---|---|
| `enabled: false` | 不收 |
| payload 不满足 `resultSchema` | 剪枝，留痕（`resultSchema` 相关 reason） |
| 目标能力参数不满足 | 剪枝 `param_missing` / 参数校验类 reason |

---

## 7. 待定清单

| 编号 | 问题 | 影响 |
|---|---|---|
| **D1** | 源→能力的 `params` 模板保留到什么程度 | 决定 v4 会不会留下唯一的表达式语法 |
| **D2** | target 能不能按事件类型分流 | 决定「一个源接多个事件、各自去找不同能力」能不能表达 |
| **D3** | 安装包（Playbook）里的源是内联还是引用 | 已倾向内联（自包含），待与 Playbook 契约一起定 |
| **D4** | 独立 `kind: Source` 文档能不能单独 apply | 决定源是不是「一等文档」 |
| **D5** | `vars` 与 `resultSchema` 是否合并成一份（前者说「能提供什么」，后者说「必须长什么样」，现在是两份） | 可能是一份 schema 的两个方向 |
