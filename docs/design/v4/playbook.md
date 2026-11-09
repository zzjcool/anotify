# Playbook Spec v4 · 安装包（第四版 / 多文件 + OCI 分发）

> 状态：**已定（语义冻结；实现未开始）**（第四版，2026-11-09；同日按 `decisions.md` V4-D3/D6 补安装冲突、PUT 更新与回执保留/引用完整性）。**取代第三版**（单文件内联版）。
> 纪律同 `capability.md`：实施中若要改本文，先改本文再改代码。
> 配套：`capability.md`（能力，冻结）、`source.md`（源，冻结）、`anode.md`（节点）。
> **本文只引用它们、不重新定义**；凡本文与它们冲突，以它们为准，本文的句子就是那句要改的。
>
> 开发期，**不背任何历史包袱**：不迁移旧 manifest、不留 v3/v2 兼容层、不为现有实现形状让步。
>
> **本版相对第二版的实质变化**
>
> | # | 第二版 | 第三版 |
> |---|---|---|
> | 核心定位 | 一份**静态清单**（内联能力 + 内联源） | 一个**参数化生成器**：`values` + 模板 → 渲染 → 装（§1.4 / §2） |
> | 拿掉的东西 | 「不是模板引擎，无变量」 | **反过来**：变量是第一位的能力；仍然不是"模板语言"（无函数/无循环/无条件，§13） |
> | 名字 | 包里写死 | 能力名 / 源名 / `next` / `targets` **全部可变量化**（`{{values.project}}-review`） |
> | 多项目 | 装 N 个包 | **一个包装 N 次**，每次一组 values（§4.8）——这是本版的主要用法 |
> | 校验 | 创建期纯函数 | 分三段：创建期只判"与 values 无关"的东西（形状 + 命名空间禁止 + 计数）；**渲染后走单建校验**；写库前才判与库冲突（§9.1） |
> | 回执 | 只报"建了什么" | **必存 `values` + `valuesDigest`**——参数化之后 values 就是这次安装的身份（§4.5） |
> | 包形状 | 一个 YAML 里内联所有能力与源 | **一个目录**：`playbook.yaml` 只放 values + 引用清单，能力/源各占一个文件（§1.3） |
> | 分发 | 只能用 POST body 传 | **OCI artifact**：`oci://…:tag`，中枢拉一次、按 digest 落库（§11） |

---

## 0. 定位

**预案 = 一个可参数化导入的安装包**：里面是一组 `values` 声明 + 一组模板化的**能力** + 一组模板化的**源**。

装一次 = 给一组 values → 渲染成一组普通文档 → 按"只做加法"写进库。

六条性质。前三条沿用，后三条是本版的地基：

1. **自描述**：一个包是**一个目录**——`playbook.yaml` 只声明 values 与引用清单，能力/源各占一个文件；被引用的文件全在包内（相对路径），装出来就是完整的图。
2. **不是运行时对象**：装完之后用户面对的是「能力」「源」；预案留下的是**清单 + 回执**，不是所有权。
3. **不含边**：v4 的边就是能力的 `next`，跟着能力一起进来。
4. **参数化**：包里任何字符串叶子都可以引用 `values`，**包括名字**。渲染出来的东西和手工建的**逐字节等价**。
5. **只做加法（I-P1）**：一次 install 只 `INSERT`，**不 UPDATE / DELETE 任何已有的行**。
   任何"想改已有东西"的差异一律进回执，不进数据库。
6. **拒绝优先于静默（I-P5）**：凡是"装上去了但没生效"的情况，必须出现在回执里。
   宁可 400，也不要一个装成功的 200 配一个坏掉的链。

### 0.1 不是四样东西

| 不是 | 为什么 |
|---|---|
| **不是模板语言** | 包层模板只有 `{{values.点路径}}` 一种形态：没有函数、没有管道、没有 `if`、没有 `range`、没有 include、没有子包。要循环就**装多次**（§4.8）——循环该发生在安装层，那里本来就有回执和条目级幂等。源的触发期引用 `$(payload…)` 属 `source.md §4.1`，本文既不扫也不定义 |
| **不是 release 管理器** | 没有 release / instance 对象、没有 rollback、没有库存清单（§13）。名字由 values 决定，升级就是重装，回滚没有语义 |
| **不是 desired-state** | 一次性写入 + 一次回执。要"对账"就读 `status`（§6），没有 reconcile 循环 |
| **不是所有权** | 删清单不删物件（§7）。`installedFrom` 只是**记录**，不是 GC 依据 |

---

## 1. 形状：一个目录

一个包 = **一个目录**，一个入口 `playbook.yaml`，其余每个能力/源各占一个文件：

```text
project-chain/
├── playbook.yaml            # 唯一入口：身份 + values + 引用清单
├── capabilities/
│   ├── review.yaml          # 一份 Capability 文档（`capability.md §2` 的原样）
│   └── fixer.yaml
└── sources/
    └── on-push.yaml         # 一份 Source 文档（`source.md §2` 的原样）
```


一个真实形态：线上已经有一个"项目 A 审查链"，把 A 抽成变量，就成了这个包。

```yaml
# playbook.yaml
apiVersion: anotify/v1
kind: Playbook

metadata:
  name: project-chain          # 包名 = slug（命名空间内唯一，也是 URL 里那段）。**不参与渲染**
  displayName: 单项目审查链
  version: 3                   # 只管展示与溯源，不参与任何身份（§5）
  description: 给一个项目装一条「审查 → 修 → 通知」的链

spec:
  values:                      # JSON Schema：默认值、校验、前端表单都从这一份出（§1.2）
    type: object
    additionalProperties: false
    required: [project]
    properties:
      project:
        type: string
        pattern: '^[a-z0-9][a-z0-9-]{0,20}$'   # 会拼进名字，所以先收紧（§2.3）
        description: 项目 slug，例如 a / checkout / payments
      env:  { type: string, enum: [dev, staging, prod], default: prod }
      repo: { type: string, default: '' }
      slackToken: { type: string, writeOnly: true }   # 进回执时打码（§4.5）

  capabilities:                # 相对路径，从包根算（§1.3）
    - capabilities/review.yaml
    - capabilities/fixer.yaml
  sources:
    - sources/on-push.yaml
```

被引用的文件就是**普通文档原文**（带不带 `apiVersion` / `kind` 都行，不看）：

```yaml
# capabilities/review.yaml
metadata:
  name: '{{values.project}}-review'
  displayName: '{{values.project}} 审查'
spec:
  params:
    type: object
    required: [repo]
    properties: { repo: { type: string } }
  next: ['{{values.project}}-fixer']
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        set -euo pipefail
        echo "project={{values.project}} env={{values.env}}"
        anode emit '{{values.project}}-fixer' --data '{"file":"a.go"}'
```

`capabilities/fixer.yaml` / `sources/on-push.yaml` 同形：前者 `next: []`，后者 `kind: webhook`、
`config: {}`、`targets: [{capability: '{{values.project}}-review', params: {repo: '{{values.repo}}'}}]`。

装一次（`values: {project: a}`）得到的就是**手写也能写出来的** `a-review` / `a-fixer` / `a-on-push`；
再装一次（`values: {project: b}`）得到一套并存的 `b-*`，互不影响。

**一个文件一件事。** 用户按"一个能力一个文件"写，diff / review / 复用都跟改普通 YAML 一样；
`playbook.yaml` 只回答"这个包要什么参数、由哪些文件组成"。

### 1.1 `metadata`（包身份）

| 字段 | 类型 | 必填 | 取值 / 约束 | 说明 |
|---|---|---|---|---|
| `name` | string | ✅ | `^[a-z0-9]+(-[a-z0-9]+)*$`，≤64 | 包名。**不渲染**：它是 URL 里的一段，身份不能依赖输入 |
| `displayName` | string | 可选 | 任意 | 纯展示 |
| `version` | int | 可选 | ≥1 | 只管展示 + 进回执，不参与 digest / 幂等 / 任何解析 |
| `description` | string | 可选 | 任意 | 纯展示 |

> **为什么包名不参数化**：`/v1/namespaces/{namespace}/playbooks/<name>` 是寻址用的。若包名要渲染，那么"引用这个包"
> 就得先知道 values——寻址与参数化互相打架。要区分用途就建两个包（`project-chain` / `repo-chain`），
> 不是给包名塞变量。

### 1.2 `spec.values`（参数声明）

**就是一份 JSON Schema 对象**，与 `capability.spec.params`、`source.spec.config.payloadSchema` 用同一个校验器。
它同时承担三件事：**校验输入**、**提供默认值**、**生成前端表单**。

| 规则 | 内容 |
|---|---|
| 默认值 | 安装时按 schema 的 `properties` 递归填 `default`（数组元素与 `additionalProperties` 不填） |
| 必填 | `required` 里的键必须出现在**有效 values**（= 传入 + default）中，否则 400 `values_missing` |
| 未知键 | 建议 `additionalProperties: false`；写了就整份拒绝（400 `values_invalid`） |
| 打码 | `writeOnly: true` 的字段照常参与渲染，但**进回执时替换成 `"***"`** |
| 不渲染 | `spec.values` 本身是**纯数据，不参与模板**（不许 values 引用 values） |

**给名字用的变量要在 schema 里收紧。** 把 `{{values.project}}` 拼进能力名，就得保证它渲染后仍然是合法名字——
`pattern` / `maxLength` 写在这里，比在模板里补救便宜得多（§2.3 的报错会直接指向这条建议）。

### 1.3 `spec.capabilities[]` / `spec.sources[]`（引用清单）

这两项是**相对路径列表**，不是内联文档。文件内容就是 `capability.md §2` / `source.md §2` 的原样文档。

| 规则 | 内容 |
|---|---|
| 路径形态 | 相对包根，`/` 分隔；不以 `/` 开头、不含 `..` 段、≤ 255 字符、不含 NUL → 否则 400 `file_path_invalid` |
| 必须存在 | 引用了包里没有的文件 → 400 `file_missing` |
| 必须被引用 | 包内 `.yaml` / `.yml` 未被任何引用列出 → 400 `file_unreferenced`（`.md` / `.txt` 等非 YAML 忽略，写 README 不必登记） |
| 不重复 | 同一个文件被列两次 → 400 `file_duplicate` |
| 入口 | `playbook.yaml` 是唯一入口，缺了就什么都不是（400 `entry_missing`） |
| 类型由槽位定 | `capabilities[]` 列的文件按 Capability 文档校验，`sources[]` 列的按 Source 文档校验——同一个文件放错槽位 = 400 `invalid_playbook`（消息指向路径） |
| 裁剪同第三版 | `metadata.namespace` **禁止**（安装上下文决定，400 `namespace_in_package`）；`status` 在导入时剥离（不提交 API；API 直接写 status 400）；`metadata.name` 必填、**可渲染**；`spec.*` 完全沿用两份冻结文档 |

**路径本身不渲染。** 路径是寻址（同 D8 包名）：先能列出"这个包有哪些文件"，才谈得上"渲染成什么"；
给路径塞变量会让"装了什么"取决于 values，审计就断了。

**文件顺序不参与语义。** 渲染后的同名检查（§3.3）与文件先后无关，只与内容有关。

**包不另立源的语义。** `kind` / `config` / `targets` 全部随 `source.md` 定；
`targets[].params` 里的触发期引用 `$(payload.x)` 随 `source.md §4.1`——**本文的渲染器只替换 `{{values.*}}`，`$(…)` 与其它 `{{…}}` 原样透传**，不新增、不收紧、不翻译它。

### 1.4 模板：只有 `{{values.点号路径}}` 一种，单相求值

这是全篇最要紧的一节。

**包层模板只有一种形态**：`{{values.点号路径}}`（数字段 = 数组下标，无函数/无条件/无默认值）。
被**替换**的根名只有 `values` 一个：渲染器只认 `{{values.点号路径}}`，其余 `{{…}}`
（如 `{{payload.x}}` / `{{env.x}}`）是**字面量**——原样留下，不报错。触发期的载荷引用是
**另一套定界符** `$(payload…)`（`source.md §4.1`），本文既不扫、也不定义它：

| 根 | 谁求值 | 什么时候 | 求值不到 |
|---|---|---|---|
| `values.*` | **渲染器**（安装期） | `POST …/install`、`…/render` | 400 `values_missing`（不静默给空） |

**两种记号，互不扫对方**：

- 渲染器**只**替换 `{{values.点号路径}}`；`{{…}}` 的其它写法、`$(…)`（字段里的触发期引用）、
  `$NAME` / `${…}`（脚本正文里的 shell）都是普通文本，原样透传；
- 运行期只由派发器解 `$(payload.*)`（`source.md §4`）；`{{…}}` 在它眼里就是字符串；
- **不检查残留、不报错**：包层模板只在安装包时组装一次，装完的文档里再出现 `{{…}}` 只可能是
  作者写错了根名（`{{payload.x}}`），这种笔误由 `render` 预览兜（§2.4），不用错误码兜。

**渲染范围**：所有**被引用文件**的字符串叶子（`capabilities/*.yaml` / `sources/*.yaml`）。
**`playbook.yaml` 自己不渲染**——它是身份与引用清单：包名不参数化（D8），路径也不参数化（§1.3）。
键名（`properties.<name>` 这类）不渲染——只渲染值，不渲染键。

**类型规则**（沿用 `source.md §4.1`，不另立）：

- 整个值恰好只有一个引用 → **保留原类型**（`replicas: '{{values.n}}'` 渲染出来是数字）；
- 掺了任何常量 → **字符串拼接**（`'{{values.project}}-review'` → `a-review`），非字符串按 JSON 文本拼入；
- 拼接后的字段若要求字符串而拿到对象/数组 → 400 `render_type_mismatch`。

**没有转义，也不需要转义。** 只有恰好匹配 `{{values.<path>}}` 的片段会被替换，其余文本一律原样
保留：脚本里写字面 `{{` 不受影响，也不做任何清洗或拒绝。

> **为什么这样是安全的**：`{{…}}` 是安装期的常量，只由渲染器解析；`$(…)` 只在**字段**里由
> 派发器解析（写进脚本正文才是 shell 的命令替换）。各扫各的记号，谁都不碰对方的语法——
> 定界符本身就是隔离，比靠约定去禁止根名重叠更硬。

### 1.5 体积与数量上界

| 项 | 上界 | 超了怎么办 |
|---|---|---|
| 包内单个文件 | 2 MiB（= 单能力 `run.files` 1 MiB 的 YAML 余量） | 拆能力 |
| 包内所有 YAML 合计（渲染前） | 8 MiB | 拆包（§13） |
| 文件数 | 64（能力 ≤32 + 源 ≤32，外加 `playbook.yaml`） | 400 `too_many_items` |
| 创建 / 安装请求体、OCI layer 解压后 | 8 MiB（= 上一行，不造第二个数字） | 拆包 |
| 包内单个能力的 `run.files` | 沿用 `capability.md`：合计 ≤ 1 MiB、单文件 ≤ 256 KiB | 拆能力。**按渲染后计**（values 可能变大） |
| 包内能力数 / 源数 | 32 / 32（与 `next` 上限同量级） | 400 `too_many_items` |

> 为什么 32：一个包超过 32 个能力，基本等于"整条业务流水线"——那不是安装包而是部署清单，
> 真到那一步需要的是分组（多个包 + 包外引用），而不是加大上限。

---

## 2. 渲染（render）

```
render(files, values) → { capabilities[], sources[] }      ← 纯函数，只读写包体（files/values）
```

输入是**文件集合**，产物仍是**一组文档**（§2.2 之后的事和单文件时代一样）。

三步，顺序固定：

```
① 填默认：按 spec.values（JSON Schema）递归补 default
② 校验 values：schema 校验（类型 / required / pattern / additionalProperties）→ 400 values_invalid / values_missing
③ 结构内插值：遍历解析后的文档树，对每个字符串叶子求值 values 模板
```

产物是**一组普通文档**——`capability.md` / `source.md` 眼里的东西。之后的校验它们说了算。

### 2.1 为什么是"结构内插值"而不是"文本替换"

| 做法 | 后果 |
|---|---|
| 文本替换（先渲染 YAML 字符串再解析） | values 里一个 `:` 或换行就能把 YAML 结构改掉；引号要不要转义变成玄学；错误定位只能给行号 |
| **结构内插值（解析后走树）** | 类型保真（字符串→数字不会变成 `"3"`）；不可能注入结构；报错能精确到 `capabilities/fixer.yaml: spec.run.files["run.sh"]` |

代价是不能把 values 用在**键名**上——这正是我们要的（§1.4「只渲染值不渲染键」）。

### 2.2 渲染后的校验 = 单建校验

**渲染产物走与"直接创建"完全相同的那一条校验路径**（`capability.md §6.1` / `source.md §6`），不设第二套：

| 校验 | 依据 |
|---|---|
| 能力 / 源的形状与内容 | `capability.md §2`、`source.md §2` |
| 名字正则（能力 `^[a-z0-9][a-z0-9._-]{0,62}$`；源按源侧规则） | 同上 |
| 参数 schema、`run.files` 体积、entrypoint shebang | `capability.md §2` |

在这之上，包**额外**加三条（都是"包"这个视角才有的）：

| 检查 | 时机 | 结果 |
|---|---|---|
| 渲染后同名（能力 / 源各一套命名空间） | 渲染期 | 400 `duplicate_capability` / `duplicate_source` |
| 包内闭合子图上成环 | 渲染期 | 400 `cycle`（指向包外的名字不参与，我们不知道外面长什么样） |
| 能力数 / 源数 ≤ 32 | 创建期 + 渲染期 | 400 `too_many_items` |

### 2.3 渲染期错误码

| 码 | 触发 | 消息要点 |
|---|---|---|
| `values_missing` | `required` 缺 / 模板引用的路径取不到 | **点名路径 + 建议**："`values.project` 没有默认值也没传；要么 install 时给，要么在 `spec.values` 里写 `default`" |
| `values_invalid` | 不满足 `pattern` / `enum` / `type` / `additionalProperties: false` | 点名键与实际值 |
| `render_name_invalid` | 渲染后的能力 / 源名不合法 | **三个都给出**：哪条模板、渲染成什么、为什么不合规，并建议在 `spec.values` 加 `pattern` |
| `render_type_mismatch` | 类型规则冲突（对象拼进字符串字段等） | 点名字段与期望类型 |

`render_name_invalid` 值得单独说：它是"名字变量化"必须付的代价。**永远不要自动清洗**
（把 `My Project` 悄悄变成 `my-project`）——那会让"包里写的名字"和"库里存的名字"永久分叉，
且用户看不出是哪一步改的。拒绝 + 三个信息，一次说清。

### 2.4 `render` 端点

```
POST /v1/namespaces/{namespace}/playbooks/{slug}/render   { "values": {...} }
→ 200 { values, valuesDigest, files: {路径: 正文}, documents: { capabilities: [...], sources: [...] } }
```

纯函数：只读写包体（files/values），**不读安装冲突库、不做冲突判定、不写库、不产生回执**。它是 `helm template`——**写包时的主要调试入口**，
也是前端"看看这个名字会渲染成什么"的唯一实现。

---

## 3. 引用与闭合

`next` 里的名字有三种落点，三种待遇（**全部在渲染后判定**）：

| 落点 | 安装行为 | 回执 | 体检 |
|---|---|---|---|
| **包内**（本次渲染出的能力名） | 同一事务里一起建，**顺序无关** | `created` / `skipped` | 参与聚合（§6） |
| **包外已有**（库里已有、包内没有） | **允许**，不建任何行（`capability.md §6`） | `external[]` | 照常解析，`DownstreamResolved` 正常为 true |
| **都没有** | **允许**，装得下（先装上后装下） | `pending[]` | `DownstreamResolved = False`，`resolved.next[].missing = true` |

`source.targets[].capability` 同一套三种落点。落 `pending` 时不阻止安装，但源体检标 `blocked`
（§6.2）——**事件会真的进来，派发会被剪枝留痕**（`source.md §6`）。

### 3.1 为什么"都没有"也允许

`capability.md §6` 已经把这条定死了：**单建**能力时 `next` 解析不到 → 400 `unknown_target`；
**预案 install** 允许顺序自由，装完由 `DownstreamResolved` 与体检报告缺哪个。

本版不另立第三套语义。差异只体现在**回执**上：`pending[]` 是"我知道缺什么"，`unknown_target` 是"我拒绝"。

### 3.2 环

**渲染后的包内闭合子图上做环检测 → 400 `cycle`。** 指向包外的名字不参与（外面长什么样我们不知道）。

跨包成环（A 包尾 → B 包头 → A 包头）不在本包责任范围，但 `store` 侧的边环检测仍兜底。

> 这条只能在渲染后做：模板化的 `next` 在渲染前连"指向谁"都还不知道。

### 3.3 渲染后同名只能出现一次

同一份包里，**渲染后**同一个能力名（同 `name`，不论省略版本还是显式版本）出现两次 → 400 `duplicate_capability`；
源同理 → `duplicate_source`。

理由：包是"一份能自圆其说的清单"。同名两次意味着**这次渲染的结果自己带着版本歧义**
（哪一个是本包的 `a-review`？体检的哪一行算它？）。需要同名多版本共存 = 两个包（§13）。

**这带来一条使用纪律**：`{{values.project}}-review` 与 `{{values.project}}-review` 是同一个名字，
但 `{{values.a}}-review` 与 `{{values.b}}-review` 只有在 a≠b 时才是两个名字——
**变量化把"两个能力"变成了"一组值的一元函数"**，重名只可能来自"两组值撞了"。

---

## 4. 安装语义

### 4.1 铁律

> **I-P1 安装只做加法**：只 `INSERT` 能力行 / 源行 / 派生的边行 / 回执行。
> 不 `UPDATE` 任何已有行，不 `DELETE` 任何已有行。

这条铁律把"要不要覆盖"这个问题变成"怎么报告"。它不是新发明——
现有实现（`store/playbook.go` 的 `createCapabilityTx` + `edgeExists` + `sourceNameExists`）
已经是"只加不改"的形状，只是**差异被静默吞掉了**（`literals` 变、`config` 变都没有 note）。
本版不改行为，改的是**可见性**。

### 4.2 冲突矩阵（逐格定死）

包里的每一项（**= 渲染后的那一项**）落到库里只有下面这些组合。
凡是标"拒绝"的，**整包拒绝，一行不写**（单事务）。

| # | 包里的条目 | 库里状态 | 动作 | 回执 |
|---|---|---|---|---|
| 1 | 能力（省略 v） | 无同名 | INSERT，版本 = 1 | `created` |
| 2 | 能力（省略 v） | 同名 + 正文相同（折叠键相同） | 复用那一行 | `skipped`（**不写 note**，早装过了） |
| 3 | 能力（省略 v） | 同名 + 正文不同 | INSERT 新版本（max+1） | `created` + `upgraded` |
| 4 | 能力（显式 v） | 无 `name@v` | INSERT | `created` |
| 5 | 能力（显式 v） | 有 `name@v`，正文相同 | 复用 | `skipped` |
| 6 | 能力（显式 v） | 有 `name@v`，正文不同 | **拒绝** 400 `version_content_immutable`（换 version） | `conflicts[]` |
| 7 | 能力（任意） | 该 `name@v` **已存在** 且包里 `next` 与库里不同 | **不改路由** | `unapplied[]`（`reason: routing_unapplied`） |
| 8 | 源 | 无同名 | INSERT（`config.token` 省略则服务端生成）；**入口边只对本次新建源 INSERT** | `created` |
| 9 | 源 | 同名 + `kind`/`config` **归一化后**等价 | 复用；**不写 targets、不写入口边** | `skipped` |
| 10 | 源 | 同名 + `kind`/`config` 真正不同 | **拒绝** 400 `playbook_conflict` | `conflicts[]` |
| 11 | 源 `targets` | 目标既不在包内也不在库里 | 建源 | `pending[]` + 源体检 `blocked` |
| 12 | 源 `targets` | 同名 + `kind`/`config` 等价，但 `targets`（含 `params` 映射）不同 | **不改 targets** | `unapplied[]`（`reason: targets_unapplied`；体检不得全绿） |

五条要点：

- **第 3 行是升级的主路径，不是冲突。** 内容变了就是另一个东西，v4 的 `version` 本来就是一串追加的行。
- **第 7 行是最关键的一条。** 它是"用户以为装上了、实际没接上"的唯一残余形态。
  我们不偷偷跳过（v3 就是偷偷跳过），而是**装完照样成功，但回执里点名**：
  `a-review@3 的路由没有改：包里要 [a-fixer]，库里是 [other]`。
- **第 12 行跟第 7 行同口径。** 源的 `targets`（包括每行的 `params` 映射）也是路由；同名源已存在且 kind/config 等价时，
  只复用源本身，**不修改 targets**，在回执 `unapplied[]` 里写明 `declared` / `actual`，体检必须显示。
- **第 10 行跟第 6 行同口径。** 能力怎么判，源就怎么判。轮换 webhook token 是源的独立操作
  （`PATCH /v1/namespaces/{namespace}/sources/{name}`），不是安装的职责——把它藏进安装里，用户就永远不知道自己的 URL 换了。
- **入口边只属于新建源。** `INSERT 源行 + 入口边` 只对第 8 行的新建源执行；第 9/12 行复用已有源时，
  不写 `targets`、不重写入边，差异只进 `unapplied[]`（第 12 行）。

**`config` 等价比较（归一化规则）**：

| 情况 | 算不算冲突 | 说明 |
|---|---|---|
| 包里省略服务端生成字段（如 `config.token`），库里已有值 | **不算** | 视为“保留已有值”；回执不报差异 |
| 包里显式写 token，值与库里不同 | **算**（走第 10 行拒绝，或按源的独立 PATCH 轮换） | 参与比较；但回执/差异报告**脱敏**（只给后 4 位或 `***`） |
| 两边都有、只是默认值未写全（如 `enabled` 缺省 = `true`） | **不算** | 先归一化默认值再比较 |
| 其它省略字段 | **不能**一律当等价 | 只有契约明确“服务端生成 / 可省略”的字段才走保留规则；其余按真实差异处理 |

### 4.3 为什么是"拒绝"而不是"跳过"

| | 跳过 | 覆盖 | **拒绝** |
|---|---|---|---|
| 显式版本同版异内容 | 用户以为装上了 | 违背"显式版本不可变" | 一次说清，改版本号即可 |
| 同名源 config 不同 | 用户以为 token 换了 | 悄悄换掉 URL，外部系统全挂 | 一次说清，走源的 PATCH |

显式版本冲突用 Skip 的代价是**不可诊断**：库里那一行看起来是对的，只有跑起来才发现脚本不是这份。
用 Reject 的代价只有一次 400——而且**内容寻址让这个 400 几乎不会出现**（改内容 = 新版本，不冲突）。

### 4.4 顺序与事务

```
① render(files, values)                  ← 纯函数；values 错就地 400（§2）
② 读一次库快照（已存在的能力 / 源 / 当前路由）
③ 纯函数算出 4.2 的动作矩阵              ← 有 conflict 就 400，一行不写
④ BEGIN
     INSERT 能力行（内容寻址或显式版本）
     INSERT 源行 + 入口边（from_is_source = 1；仅本次新建源，复用源不写）
     INSERT 包内 next 派生的边（from_is_source = 0）——只对"本次新建"的上游
     INSERT 回执行（playbook_installs，含 values 与 digest）
   COMMIT
⑤ 返回回执
```

**回执不报 edges**：能力边是 `next` 的派生行、源入口边是 `targets` 的派生行；报了反而暗示用户能单独操作它们。

### 4.5 回执契约（D15）

```yaml
apiVersion: anotify/v1
kind: InstallReceipt
playbook:     { name: project-chain, version: 3 }   # 包的身份
source:       oci://ghcr.io/acme/playbooks/project-chain@sha256:4c11…   # 从 registry 拉来的才有（§11）
install:      { id: ins_7f3a, at: 2026-11-08T13:20:00Z, dryRun: false }
namespace:    prod

values:       { project: a, env: prod, repo: '', slackToken: '***' }   # 有效 values（writeOnly 打码）
valuesDigest: sha256:9c1f…                                             # 对**未打码**的有效 values 取哈希

created:  { capabilities: [a-review@1, a-fixer@1], sources: [a-on-push] }
skipped:  { capabilities: [], sources: [] }

upgraded:                       # 同名能力长出新版本：会改变所有"按裸名解析"的落点
  - { capability: a-review, from: 2, to: 3, affects: { edges: 5, sources: 1 } }

unapplied:                      # 装了，但库里已有行不许改 → 这条差异没生效
  - { kind: capability, ref: a-review@3, field: next,
      declared: [a-fixer], actual: [other], reason: routing_unapplied }
  - { kind: source, ref: a-on-push, field: targets,
      declared: [{ capability: a-fixer, params: {...} }],
      actual:   [{ capability: a-linter }], reason: targets_unapplied }

external: [notify@2]            # 引用包外（合法，不用管）
pending:  [a-linter]            # 解析不到（装得下，但链是断的）

notes:                          # 稳定机器码 + 人话；不是一串自由文本
  - { code: unreachable, ref: a-fixer,
      message: "a-fixer 装上了，但没有上游指向它：a-review 的路由未改" }
```

五条规则：

| 规则 | 内容 |
|---|---|
| **回执落库** | 每次 install 写一行 `playbook_installs`（含 `values` JSON、`values_digest`、`source`）；`dryRun` **不写行**（§4.6 的 0 写入）；`GET /v1/namespaces/{namespace}/playbooks/{slug}` 带 `lastInstallId` |
| **values 必须留痕** | 回执存**有效** values（含补上的默认值）+ digest。没有它，"这次装的是哪一组值"事后无从回答——而 values 现在就是那个身份（§0.1） |
| **secret 打码** | `writeOnly: true` 是合法 JSON Schema 关键字（不是非法扩展）；这类字段在回执里是 `"***"`，digest 对**打码前**的值算，这样"同一组值"仍可比较。显式的 `config.token` 差异同样只显示后 4 位 / `***`；源读响应只给 `secretSet`，不回显 `secret` / `token` |
| **note 有 `code`** | 每个 note 是 `{code, ref?, message}`。`code` 是稳定标识（e2e 断言它），`message` 只给人读 |
| **跳过必有原因** | 只有"早装过了（第 2/5/9 行）"不写 note；其余跳过 / 未生效 / 断链都必须有一条 |

#### `notes[].code` 全表

**就这六个码，一期不加。** 新增一个码要改本文：`code` 是 e2e 断言的对象，不是随便拼的字符串。

| code | 什么时候出现 | `ref` 指向 | 性质 |
|---|---|---|---|
| `routing_unapplied` | 已存在的能力，包里 `next` 与库里不同（§4.2 第 7 行） | `capability@version` | 装了，但这条路由没生效 |
| `targets_unapplied` | 已存在的源，包里 `targets`（含 `params`）与库里不同（§4.2 第 12 行） | 源名 | 源复用了，但事件去向没改 |
| `unreachable` | 装完自检：包内能力既无上游、也无源指向 | `capability@version` | 装上了，但接不进链 |
| `pending_ref` | `next` / `targets` 指向的名字**库里也没有**（§3 第三行） | 那个名字 | 链是断的，等补 |
| `source_disabled` | 包内源渲染后 `enabled: false` | 源名 | 只是提示 |
| `no_node` | 包内**没有任何**能力能在当前节点池上跑（`NodesAvailable` 会卡住） | 空 | 汇总一条，不逐能力重复 |

`unreachable` 这条自检正好覆盖第 7 行（路由未生效）导致的"孤儿能力"——
两个字段从两个角度说同一件事，用户第一次装就看得见。

### 4.6 试装（`dryRun: true`）

`POST /v1/namespaces/{namespace}/playbooks/{slug}/install` 带 `"dryRun": true` 走**同一条代码路径**，事务最后 `Rollback`，
返回同一份回执（`dryRun: true`），**0 持久写入、不落回执行**（不写 `playbook_installs`，不派任务、不写 deployments）。

为什么值得做（成本约 10 行）：选了"拒绝"，拒绝就必须能提前看——
否则用户只能靠"装失败了、看 400、改 values、再装"来试探。有了 dryRun，第一次就能看到
`conflicts` / `unapplied` / `pending` 全貌。

`render` 返回**渲染后的**结果，不是原始模板读取；编辑 / 导出原始 `files` 用 `GET …?includeBody=true`。
`render`（§2.4）与 `dryRun` 的分工：`render` 只回答"渲染成什么"，`dryRun` 回答"装下去会怎样"
（要读库、有冲突判定）。写包时用 `render`，上线前用 `dryRun`。

### 4.7 幂等粒度（D18）

**条目级，不是整包级。** 包内容哈希只用于"这个包上次装的是不是同一份"这种快信息，
不用于"要不要执行安装"。

理由：包的一半装成功、另一半因为环境缺失进了 `pending`，是正常状态；用整包哈希判幂等
会让"补一次安装"变成"再来一遍全量动作"。条目级幂等 = 每次 install 都是**补齐**，
补齐的代价只有一次读快照。

**同一组 `values` 再装一次不是 409**：仍走完整比较 + 补齐，`created` 可能为空、`skipped` 全绿，
但每次 install 都产生一份**新回执**（`playbook_installs` 多一行）。回执是“这次尝试”的记录，
不是“这个 values 只能装一次”的锁；也不返回旧回执来假装没发生过。

### 4.8 一个包，N 个项目（本版的主要用法）

```
POST /v1/namespaces/{namespace}/playbooks/project-chain/install  { values: { project: a } }   → a-review@1, a-fixer@1, a-on-push
POST /v1/namespaces/{namespace}/playbooks/project-chain/install  { values: { project: b } }   → b-review@1, b-fixer@1, b-on-push
```

两次都 `created`，两套图**并存**。这不需要任何"实例 / release"对象——**名字由 values 决定，
没有名字冲突就没有实例管理的必要**（§0.1）。

三个必须提前想清楚的点：

| 点 | 结论 |
|---|---|
| 忘了把源名变量化 | 第二个项目装的时候**撞 `playbook_conflict`**（同名源、config 不同）——这正是我们要的：宁可 400，也不要静默复用第一个项目的 webhook token |
| 实现要改 | 每个项目一套行，改一次实现 = 用新包体重装 N 次（`upgraded[]` 每次都会报），或用 shared 能力把实现抽走 |
| 值该进名字还是进参数 | 见下表 |

**值放哪里，决定了复用的是"图"还是"行"**：

| 用法 | 结果 | 适合 |
|---|---|---|
| 拼进名字（`{{values.project}}-review`） | 每个项目一套独立能力，互不影响 | 需要按项目隔离（不同脚本、不同节点要求、不同并发） |
| 写进正文（`run.files` 里的 `{{values.project}}`） | 同上，且 digest 不同（每个项目一条独立版本） | 项目名必须出现在脚本里 |
| 只进 `params.default` / 源 `config` | 一套能力服务 N 个项目 | 逻辑完全一样、只是数据不同——**但名字相同，所以只能装一次** |

### 4.9 多次实例化：没有"实例对象"，只有名字与 values

同一个包可以装 N 次，每次一组 values（§4.8 上表就是它的展开）。**"实例"不是一个对象**——库里没有位置存它，
它只是（包, values）渲染出来的那批行的集合。

| 问题 | 答案 |
|---|---|
| 怎么区分两个实例 | 靠**渲染后的名字**（`a-review` / `b-review`）与 `valuesDigest`；不靠实例名 |
| 同一组 values 再装一次 | 条目级幂等（§4.7）：什么都不会新建，`skipped` 全绿；但 `playbook_installs` **多一行**，返回**新回执**（不 409、不复用旧回执） |
| 只改包体、不重装 | **不影响已装的行**：`PUT /v1/namespaces/{namespace}/playbooks/{slug}` 只换模板（§10），库里那批行要等下一次 install 才变 |
| 想升级全部实例 | 逐个 values 重装 N 次；没有 `upgrade --all`——那需要一份实例清单，见下 |
| 换 namespace | 包是命名空间内的实体：同一个 OCI artifact 在另一个 ns 要**再建一个包**（`slug` 在 ns 内唯一） |

### 4.10 实例清单（读模型）

"这个包装了哪几套"是**纯派生**的：把 `playbook_installs` 按 `valuesDigest` 折叠，不建实例表。

```
GET /v1/namespaces/{namespace}/playbooks/{slug}/installs        # 折叠读模型（按 valuesDigest）
→ 200 {
    total: 2,
    installs: [
      { valuesDigest: "sha256:9c1f…",
        values: { project: a, env: prod, repo: "", slackToken: "***" },   # 沿用 D16 打码
        firstAt: 2026-11-01T09:00:00Z, lastAt: 2026-11-08T13:20:00Z,
        attempts: 3,                    # 同一组 values 装了几次（每次 install 一行）
        lastInstallId: "ins_7f3a",
        capabilities: [a-review@1, a-fixer@1],   # 该组所有 install id 反查（installed_from）
        sources: [a-on-push] },
      { valuesDigest: "sha256:be07…", values: { project: b, … }, attempts: 1, … }
    ] }
```

| 规则 | 内容 |
|---|---|
| 折叠键 | `(namespace_id, playbook_id, values_digest)` —— 一个折叠组 = 一套实例；**列表不是原始回执列表** |
| 单次回执 | `GET /v1/namespaces/{namespace}/playbooks/{slug}/installs/{installId}` 返回那一次 install 的完整回执（§4.5）；列表里的 `lastInstallId` 可直达 |
| `attempts` | 组内行数。不是"版本号"，只是"试过几次" |
| `capabilities` / `sources` | 组内**所有** install id 反查 `installed_from` 的行（并集去重）；某次 install 全 `skipped` 时它仍指向第一次建它的那条 |
| 排序 | `lastAt` 降序；`total` = 组数 |
| 上限 | 一次最多 200 组（超出按 `lastAt` 截断）。不另立分页参数：实例数是十位量级 |
| 空 | `installs: []` 不是 `null` |
| dryRun | **不进这个视图**：dryRun 0 写入，不落 `playbook_installs`（§4.6） |
| 不是所有权 | 只读：删这个视图不影响任何能力或源（§7）；删一条回执行只影响 `attempts` |

**与体检（§6）的关系**：体检按**名字**切（`items[]`），实例视图按 **valuesDigest** 切——
同一批行的两个切法，不是两套真相。

---

## 5. 升级

**升级 = 同名包再装一次**（换包体、换 values，或两者都换）。包体就是 §1 那组文件：改包用 **`PUT /v1/namespaces/{namespace}/playbooks/{slug}` 整体替换**（§10），不要求“删除重建”；已存在同 slug 再 `POST` 则 409 `slug_taken`。没有 `upgrade` 动词，没有版本区间，没有迁移脚本。

**install 本身不部署。** 安装只写能力/源/回执；要让节点持有新 digest，走人工部署动作
`POST /v1/namespaces/{namespace}/capabilities/{name}/deploy`（可选 version / 目标节点，写 `deployments` 事实）。

> 注意：`POST` 换包体**只换模板**，库里已装的行一行不动——真正生效还是下一次 install（且只对那组 values）。

| 变化 | 结果 |
|---|---|
| `run.files` / `params` / `requires` / values 渲染结果变了 | 内容寻址 → 新版本 `created`；`upgraded[]` 报告影响面（边 + 源两个计数） |
| `next` 变了（指向**本次新建**的能力） | 照写 |
| `next` 变了（指向**已存在**的能力） | `unapplied[]`：那条路由不动（§4.2 第 7 行） |
| 新增能力 / 新增源 | `created` |
| 删掉的能力 / 源 | **不做任何事**（I-P1 不删）。仍被引用的旧版本继续存在，`next: [x@2]` 的下游照旧跑 v2 |
| `metadata.version` 变了 | 只进回执 |

两条必须说清的后果：

1. **钉死的下游不跟着升级。** 包里写 `next: ['{{values.project}}-fixer@2']`，把带 v3 的新包装上去，
   那个下游仍是 v2。这是刻意的（pin 就是 pin）。日常写裸名。
2. **裸名下游会自动换实现。** 装了 `a-review@4` 之后，任何 `next: [a-review]` 的上游下一次派发就走 v4。
   `upgraded[].affects` 就是告诉用户「5 条边 + 1 个源会改落点」。这是 v4 把 `next` 做成裸名的固有代价，
   回执是唯一的告知渠道。

   **`affects` 两个计数都要算**：`edges`（裸名 `next` 边）与 `sources`（`targets` 里写裸名的源）。
   只报边会漏掉一半事实——事件入口同样按名字解析到 latest，"源指向的能力换了实现"
   和"边指向的能力换了实现"对用户是同一件事。

装完之后要生效还差两步（不属于本包的职责，但回执要指出来）：
**节点部署**（`Deployed` condition）与 **节点装依赖**（`NodesAvailable`）。

---

## 6. 体检（D22）

### 6.1 不再有预案自己的状态词

v3 有 `ready / blocked / needs-node / partial` 四态 + 4 类 item（能力 / 边 / 触发器 / 节点）。
v4 删掉端口、边、handler、noop 之后，这套词失去了对应的东西：

- `partial` 的存在理由是"边或源缺了但定义在"，而 v4 里边不再是用户实体、安装是单事务；
- 4 类 item 收缩成 **2 类**：`capability`（含节点可用性）与 `source`。

**v4 的包级 status 不自定义枚举，直接聚合成员能力已有的四条 condition**（`capability.md §2.4`）。

```yaml
status:
  ready: false                    # 四条 condition 全 True 且 unapplied 为空
  unapplied: []                   # 非空 → ready=false：路由 / 事件去向没有全部生效
  total: 2
  conditions:                     # 四条全部来自成员能力，按 type 聚合
    - { type: Implemented,        status: "True" }
    - { type: NodesAvailable,     status: "False", reason: NoMatchingNode,
        members: [a-fixer], message: "没有节点声明 git" }
    - { type: DownstreamResolved, status: "True" }
    - { type: Deployed,           status: "False", reason: NotDeployed,
        members: [a-review, a-fixer], message: "2 台节点持有旧 digest" }
  blocker:                        # 最该先做的一件事（不再叫 next，见 §6.3）
    { type: NodesAvailable, capability: a-fixer, action: open_node_page }

  items:                          # 只有两类；名字是**渲染后**的名字
    - { kind: capability, name: a-fixer, version: 1, state: no_node }
    - { kind: source,     name: a-on-push, state: ok, targets: [a-review] }
  sources: { total: 1, enabled: 1, blocked: 0, disabled: 0, unapplied: 0 }
  lastInstallId: ins_7f3a
```

**体检看的是"装过什么"，不是"模板长什么样"。** 它的输入是包在历史上装出来的那些行
（按 `installed_from` 反查，§7.2）——不同 values 装出来的两套能力会同时出现在 items 里，
因为它们在库里就是两套独立的东西。

**`items[].state` 的词表**：能力取 `ok` / `no_node`（`no_node` = 当前节点池里没有能跑它的节点，
与 `notes[].code` 同一个词）；源取 `ok` / `blocked` / `disabled` / `unapplied`（§6.2）。不另立第三套词。

### 6.2 三条判定细则

| 项 | 规则 |
|---|---|
| `ready` | 四条 condition 全 True（与 `capability.md` 同口径，不另立"包级 ready"），**且 `unapplied[]` 为空** |
| 源 | **不参与四条 condition**，单列 `sources{}`。理由：源可以手动触发 / 用 `emit` 验链，源的 config 在创建期已校验过；把源算进 ready 会让"没配触发器"看起来像"能力坏了"。但源的 `targets` 差异会进 `unapplied[]`，**仍使 `ready=false`**（这是安装结论，不是节点健康） |
| 源 status | `ok` / `blocked`（`targets` 里的能力解析不到）/ `disabled`（`enabled: false`，只是提示，不算错）/ `unapplied`（targets 与库里不同、本次未生效）。`status.sources` 按这四态计数，供前端直接渲染 |

### 6.3 `next` 改名 `blocker`

v3 的 `status.next` 是"最该先做的一件事"。v4 里 **`next` 已经是"下游"**（`capability.md §2.3`），
一个词不能兼两份职，所以改名 `blocker`，值是 `{type, capability, action}`。

`action` 是稳定枚举（`edit_capability` / `edit_source` / `open_node_page` / `deploy` / `create_capability`），
前端据此给按钮——不再靠猜文案。

---

## 7. 生命周期（D20）

**不支持卸载。** `DELETE /v1/namespaces/{namespace}/playbooks/{slug}` 只删清单，且回执要告诉用户留下了什么：

```yaml
# DELETE → 200
{ deleted: true,
  kept: { capabilities: [a-review@1, a-fixer@1], sources: [a-on-push, b-on-push] },
  values: [ { project: a }, { project: b } ],          # 从安装记录里汇总
  note: "预案只是清单，不是所有权；要拆链请直接删能力 / 删源" }
```

注意 `kept` 的两个来源：**能力行靠 `installed_from` 反查**（精确），
`values` 靠回执历史汇总（说明"这些行是哪几组值装出来的"）。
**不需要再用 values 渲染一次**——这正是 §7.2 那两处记录的价值。

### 7.1 为什么不做卸载

| 理由 | 展开 |
|---|---|
| **与"不是所有权"矛盾** | 若删清单能级联删能力，那清单就是所有权了；紧接着的问题就是"别的包也引用了它，删谁"、"用户手改过还算不算我装的"——每一个都需要一套归属协议 |
| **能力是图的一部分** | 删一个能力会断好几条链，而这个动作的正确性取决于**整张图**，不是这个包 |
| **正确的信号已经存在** | 真想拆链的人去删能力/删源；删完之后**别的包的体检会报 `DownstreamResolved = False`**——这正是我们想要的信号，比悄悄级联安全得多 |

### 7.2 记录 ≠ 所有权（D21）

为了回答"谁装了这个能力 / 这次装的是哪一组 values"，引入两处记录：

| 记录 | 位置 | 用途 |
|---|---|---|
| `installedFrom` | `capability_versions.installed_from`、`sources.installed_from`（空 = 手建） | 反向溯源：能力页显示"由 project-chain 装的（project=a）"，`DELETE … kept` 靠它 |
| 回执行 | `playbook_installs`（`receipt` JSON 原文 + `values` + `values_digest` + `source`） | 正向溯源：预案详情页显示"上次安装时间 + 回执"；`GET …/installs` 在它上面折叠（§4.10） |

**这两处都是记录，不是所有权**：不用于 GC、不阻止手工编辑、不阻止手删。

`installed_from` 存**安装记录 id**（`playbook_installs.id`），指向"哪一次装"。两条已知边界：

- 同一个能力行被两个包先后创建 → 记**首次**那一次（内容寻址下这一行本来就只被创建一次）；
- 复用已有能力/源时**不覆盖** `installed_from`：它只表示初次来源；后续 install 的 `skipped` 不改变它；
- 手工 `PATCH` 过能力不影响它（记录的是来源，不是守卫）。

### 7.3 回执保留与引用完整性（V4-D6）

| 项 | 规则 |
|---|---|
| 安装回执 | **不跟事件 / trace 的 30 天 TTL 一起删**；每次 install 一行，直到显式清理或所属数据生命周期结束 |
| `installed_from` | 逻辑外键指向 `playbook_installs.id`；被引用的回执**不得删除**，否则就是悬空外键 |
| 删包 | 只删清单；**回执保留**（DELETE 响应的 `kept` / `values` 靠它汇总）；不级联删能力 / 源 |
| GC 顺序 | 先清关联执行数据（日志 / 产物）→ 再事件 / 去重记录 → 回执最后；任何仍被引用的行延长保留 |
| 回收条件 | 只有当**没有任何** `installed_from` 指向某条回执时，它才可以被回收；不允许“先删回执再补引用” |
| 事件过期 | 事件 30 天过期不会让回执 / `installed_from` 失效；不能把 `installed_from` 当事件元数据删 |

> 回执与 `installed_from` 仍然**不是所有权**：它们不阻止手删能力、不参与级联；只是引用完整性要求。

---

## 8. 与旧形态的关系

**不迁移、不兼容、不识别。** 开发期没有历史包袱，旧 manifest 在新代码下校验不过：

| 旧东西 | 现在 |
|---|---|
| `capabilities[]` 三种写法（`ref` / 内联 `spec` / v1 旧形态） | **一种**：`spec.capabilities[]` 是相对路径引用，正文在各自文件里（§1.3） |
| `edges[]`（`fromPort` / `literals` / `priority` / `mapping`） | **整段删除**：边就是能力的 `next` |
| `triggers[]` | 改名 `sources[]`，形状 = Source 文档 |
| 内联 spec 的 `handler: noop` 占位 | 删除（随 `capability.md §10` 一起消失） |
| 回执 `created.{capabilities, edges, sources}` | `created.{capabilities, sources}` + `unapplied` / `upgraded` / `external` / `pending` / `notes` |
| 体检 4 态 + 4 类 item | 聚合 4 条 condition + 2 类 item |
| "同 kind 且指向同入口的源"启发式跳过 | 删除。只认 `name` 身份（同名才跳过，config 不同就拒绝） |
| 创建期读库校验 | 删除。创建期是"值无关"的（§9.1） |

开发库删掉重建；devseed / e2e 夹具 / 单测夹具同步改成 v4 形状。

> **外部句子已同步**：`README.md` 的文档分工表已写成本版的「一个目录 + 引用清单 + values + OCI 分发」；
> `capability.md` 未定义预案 manifest 形状，只引用本文。

---

## 9. 校验与错误码

### 9.1 校验分三段（本版重写）

旧版按"对象种类"切（ref 安装期 400、边端点安装期 skip、触发器 targets 创建期 400）。
参数化把这条切法彻底废掉：**创建期根本不知道名字长什么样**——`{{values.project}}-review`
在 values 落地之前就只是个字符串。

所以按**"要不要 values 才能判"**切：

| 段 | 判什么 | 不需要 values | 结果 |
|---|---|---|---|
| **创建期**（包入库） | **文件集合**：入口存在、引用闭合、路径合法、无未引用 YAML、无重复引用；每个被引用文件的**形状**；`metadata.namespace` 禁止；计数与上界 | ✅ | 400 `invalid_playbook` 家族 / `entry_missing` / `file_*` |
| **渲染期**（render / install） | **全部语义校验**：名字正则、参数 schema、体积、同名、包内环 | ❌ 需要 | 400 `values_*` / `render_*` / `duplicate_*` / `cycle` |
| **安装期**（写库） | 与库里已有行是否冲突 | — 需要库快照 | 400 `playbook_conflict` + `conflicts[]` |

推论 **I-P2（改写）：创建期是值无关的纯函数**——同一个包在任何时候提交都同结果，
不会"今天能存明天不能存"。**渲染期是值相关的纯函数**——同一组 (包, values) 永远渲染出同样的东西。

这是本版对第二版的修正：第二版说"创建期连包内环都要判"，参数化之后那句不成立了。

> **一条实现硬约束（极易写错）**：创建期**不能**拿包内能力去跑“单建校验”——名字正则、`next` 解析、
> 参数 schema 展开这些都要在渲染后才判得了（此刻能力名还是模板，`{{values.project}}-review` 不是
> 合法名字），于是**没有任何参数化的包能存进去**。创建期跑的是“包的形状”；单建校验只在渲染后跑（§2.2）。
> 两处共用代码时必须显式传“现在是在校验模板还是在校验文档”。

### 9.2 错误码

**创建期**

| 码 | 语义 |
|---|---|
| `invalid_playbook` | 包的形状/内容不合法（消息点到 `capabilities[i]` / `sources[i]`） |
| `slug_taken` | 同命名空间已有同名包（409） |
| `slug_invalid` | 包名不合 `^[a-z0-9]+(-[a-z0-9]+)*$` / >64 |
| `namespace_in_package` | 包内某项写了 `metadata.namespace` |
| `too_many_items` | 能力 / 源超过 32，或文件数超过 64 |
| `entry_missing` | 文件集合里没有 `playbook.yaml` |
| `file_path_invalid` | 引用路径是绝对路径 / 含 `..` / 超 255 字符 / 含 NUL |
| `file_missing` | 引用的路径在文件集合里不存在 |
| `file_unreferenced` | 包内 `.yaml` / `.yml` 没被任何引用列出 |
| `file_duplicate` | 同一个文件在 `capabilities[]` / `sources[]` 里出现两次 |

**渲染期**

| 码 | 语义 |
|---|---|
| `values_missing` | `required` 缺 / 模板引用的路径取不到 |
| `values_invalid` | schema 不满足（类型 / pattern / enum / 未知键） |
| `render_name_invalid` | 渲染后的能力名 / 源名不合法 |
| `render_type_mismatch` | 类型规则冲突 |
| `duplicate_capability` / `duplicate_source` | 渲染后同名 |
| `cycle` | 渲染后的包内 `next` 图成环 |

**安装期**

| 码 | 语义 |
|---|---|
| `playbook_conflict` | 与库里源 config 冲突（§4.2 第 10 行），响应带 `conflicts[]` |
| `version_content_immutable` | 显式 `name@v` 已存在且执行正文不同；必须换 version（`next` / `displayName` 例外） |
| `not_found` | 包 / 命名空间不存在 |

**分发期**（`source: oci://…`，§11）

| 码 | 语义 |
|---|---|
| `oci_unsupported` | 媒体类型不是 `application/vnd.anotify.playbook.v1.tar+gzip` |
| `oci_pull_failed` | registry 认证 / 404 / 网络 / 超时；message 区分"没权限"与"没这个 tag" |
| `oci_digest_mismatch` | 拉下来的字节与声明的 digest 不符（**不写库**，一个字节都不落） |
| `oci_too_large` | 解压后超过 §1.5 的上界（按**解压后**判，防 tar 炸弹） |

`playbook_conflict` 的响应形状：

```yaml
error: "预案冲突：2 条需要覆盖已有定义，安装被拒绝"
code: playbook_conflict
valuesDigest: sha256:9c1f…
conflicts:
  - { kind: capability, ref: a-review@3, reason: version_conflict,
      declaredDigest: sha256:ab12…, actualDigest: sha256:cd34… }
  - { kind: source, ref: a-on-push, reason: config_differs, fields: [config.token] }
hint: "给该能力改一个 metadata.version 再装；或删掉库里那一行重建"
```

> `hint` 是必须的：一个 400 如果不告诉人下一步做什么，它跟 500 对用户是一样的。
> `valuesDigest` 也带上——参数化之后，"用哪组值装出来的冲突"是第一条要问的信息。

---

## 10. API

| 端点 | 作用 |
|---|---|
| `POST /v1/namespaces/{namespace}/playbooks` | 存包（创建）：`{slug, files}` 或 `{slug, source: "oci://…"}` **二选一**（§1.3 / §11）。校验只看形状，值无关；同 slug 已存在 → 409 `slug_taken` |
| `PUT /v1/namespaces/{namespace}/playbooks/{slug}` | **整体替换包体**（升级模板）；body 同上。不要求删除重建；不影响已装的行 |
| `GET /v1/namespaces/{namespace}/playbooks` / `GET /v1/namespaces/{namespace}/playbooks/{slug}` | 列表 / 详情；详情默认摘要 + `lastInstallId`（回执摘要入口）+ 体检（§6） |
| `GET /v1/namespaces/{namespace}/playbooks/{slug}?includeBody=true` | 返回**原始 `files` 集合（未渲染）**，供编辑 / 导出；`render` 返回渲染结果，不替代原始模板读取 |
| `POST /v1/namespaces/{namespace}/playbooks/{slug}/render` | `{values}` → `{values, valuesDigest, documents, files}`（`files` = 渲染后的 `{路径: 正文}`），纯函数（§2.4） |
| `POST /v1/namespaces/{namespace}/playbooks/{slug}/install` | `{values, dryRun?}` → 回执（§4.5）；`dryRun: true` 0 写入（§4.6） |
| `DELETE /v1/namespaces/{namespace}/playbooks/{slug}` | 只删清单 + `kept` 报告（§7） |
| `GET /v1/namespaces/{namespace}/playbooks/{slug}/installs` | 实例清单（**读模型**，§4.10）：按 `valuesDigest` 折叠回执行 |

约定：

- **JSON 字段全 camelCase**；空列表返回 `[]` 不是 `null`。
- `install` 的 body 是 `{values, dryRun}`；**不要** query 参数版本（同一件事只有一种写法）。
- 每次 install 都产生一行 `playbook_installs`，`GET` 详情里给最近一次 + 次数。
- `files` 是 `{路径: 正文}` 的 JSON map，键 = §1.3 的相对路径；`source` 与 `files` 同时给 → 400 `invalid_playbook`。
- 同 slug 再 `POST` → 409 `slug_taken`；改包用 **`PUT /v1/namespaces/{namespace}/playbooks/{slug}` 整体替换**（升级就靠它，§5）；不做 `PATCH`、不做部分修改。

---

## 11. 分发：OCI 仓库

包是一个目录，用 **OCI artifact** 发出去——不新造分发协议，推用标准工具，拉由中枢做一次。

```text
project-chain/  ──oras push──▶  ghcr.io/acme/playbooks/project-chain:1.4.2
                                          │
                                          │ 中枢 pull（一次，然后就不碰 registry）
                                          ▼
              POST /v1/namespaces/{namespace}/playbooks {slug, source: "oci://…:1.4.2"}
```

| 项 | 定死的事 |
|---|---|
| 媒体类型 | manifest = `application/vnd.oci.image.manifest.v1+json`；**单层** `application/vnd.anotify.playbook.v1.tar+gzip`（tar 里就是 §1 那组文件，路径相对包根）；config = 空对象 `application/vnd.oci.empty.v1+json` |
| 引用形态 | `oci://<registry>/<repository>:<tag>` 或 `…@sha256:<hex>`。**tag 只在拉取那一刻有意义** |
| 谁拉 | **中枢拉**。节点永远不碰 registry（它的出网面保持只有中枢一条，`anode.md`） |
| 拉完就存 | **copy-on-pull**：文件集合按 digest 落库，之后 render / install 完全不碰 registry。registry 挂了不影响已存的包——"外面变了怎么办"的答案是"它进不来第二次" |
| 身份 | 库里记 `source: oci://…@sha256:…`，回执带同一个 digest。**tag 不参与身份**：同 tag 换 digest = 另一份包体，按“`PUT` 替换包体”处理（§5） |
| 校验 | ① 媒体类型不认识 → 400 `oci_unsupported`；② 自己算 digest 再比，不信 registry → 不符 400 `oci_digest_mismatch`；③ 解压后超 §1.5 上界 → 400 `oci_too_large`；④ 文件集合按 §1.3 判（与手传 body 完全同一条路径） |
| 凭据 | **不进配置模型、不进 values**（`config.md` 没有 `secrets`）：中枢进程读标准 `$DOCKER_CONFIG/config.json`（默认 `~/.docker/config.json`）。私有 registry 就照 docker 的办法配一次 |
| 发布 | **不做 push 端点**：发就是 `oras push` / `crane` / CI，我们只定 mediaType。CLI 面保持三个子命令（`README.md`） |
| 签名 | 本期不做。digest 落库，将来挂 verifier 有落点 |

**这是本篇唯一的出网面。** 约束三条：

1. **只认 `oci://`**，不接裸 `http(s)://` URL 导入（那会变成任意 URL 代理）；
2. 只对 registry 的 distribution API v2 发 GET（manifest / blob），不转发任意请求；
3. 超时 / 重试写死一份（与 `cmd/agent/upgrade.go` 同款：单次上限 + 流式 sha256），不做用户可配。

离线 / 内网同理：指向内网 registry 就行，不用另造"离线包"格式。

---

## 12. 分期

| 期 | 内容 | 门禁 |
|---|---|---|
| **P9-f1** | 形状：`playbook.yaml`（values + 引用清单）+ 文件集合（入口 / 闭合 / 路径 / 未引用 / 重复）；删 `edges` / `ref` / 旧形态 / `noop`；创建期值无关校验 | `go test ./internal/store`（形状矩阵单测，每个拒绝码一条） |
| **P9-f2** | 渲染器：JSON Schema 默认值填充 + `values` 校验 + 结构内插值 + 定界符隔离 | `TestRender*`：类型保真、拼接、`$(…)` 原样透传、非 `values` 根的 `{{…}}` 原样透传 |
| **P9-f3** | 安装语义：渲染 → 冲突矩阵 12 格 → 单事务 → 只做加法 | `TestInstallMatrix`：12 格每格一条断言（现有 `playbook_test.go` 27 个测试按 v4 重写） |
| **P9-f4** | 回执：`created/skipped/unapplied/upgraded/external/pending/notes` + `values`/`valuesDigest` 落库 + 打码 | 回执往返单测；`writeOnly` 打码断言；`code` 封闭性断言 |
| **P9-f5** | `render` + `dryRun` | 单测：render **只读包体、不读安装冲突库、不写库**；dryRun 后**逐表行数不变**（`rowCounts` 对比） |
| **P9-f6** | 体检：聚合四条 condition + 2 类 item + `blocker`（删四态） | `TestPlaybookChecks*` 按 v4 重写；`blocker.action` 枚举断言 |
| **P9-f7** | DELETE 带 `kept` + `installed_from` 反查 | 单测：删清单后 caps/sources 行数不变 + `kept` 内容正确 |
| **P9-f8** | API + openapi + 前端（`playbooks.html` / `playbook.html`：values 表单从 `spec.values` 生成；文件列表来自 `files`）+ e2e 夹具改 v4 形状 | `make contract`、`make fe`（死类守卫）、`real_backend` / `frontend` 套件 |
| **P9-f10** | 实例视图 `GET …/installs`（按 `valuesDigest` 折叠）+ 前端"实例"页签 | 单测：同 values 装 3 次 → `attempts=3`、`names` 不重复；两套 values → 2 组；`dryRun` 不产生组；删回执行后 `attempts` 紧上 |
| **P9-f9** | OCI 分发：distribution v2 拉取（manifest + blob）+ 流式 digest + 解压上界 + `$DOCKER_CONFIG` 凭据 | 单测：`httptest` 假 registry（push 一份 → pull → 文件集合相等）；digest 不符 / 媒体类型错 / 解压超限 各一条；**不引外网** |

**schema 变更：不迁移。** v4 是 breaking change，没有存量库要升级——`schema.sql` 的 baseline 直接写成
v4 形状：新增 `playbook_installs` 表（列：`id / namespace_id / playbook_id / values / values_digest / receipt / source`），`capability_versions` / `sources` 各加一列
`installed_from TEXT NOT NULL DEFAULT ''`。**不追加 `-- +migration N` 块**；旧库由 `store.Open`
明确报错（与 squash 前的策略一致），不给升级路径。

（baseline 改写是一次性动作，与 `DEVELOPMENT.md`「schema 变更」的「追加迁移块」常规路径不同——
本版没有需要升级的库，所以是例外，不是新规矩。）

合并纪律（`AGENTS.md §7`）：纯形状/校验可以先走 `make fe` + 目标页截图迭代；
**push 前必须 `make e2e` 全绿**。

---

## 13. 非目标

| 不做 | 理由 |
|---|---|
| **模板逻辑**（`if` / `range` / 函数 / 管道 / include / 子包） | 语法只有一份且是"点路径"（`source.md §4.1`）。一旦有逻辑，"装上去了但渲染成了空"就变成新的静默失败形态。要循环 = 装多次（§4.8） |
| **release / instance 对象** | 名字由 values 决定，"同一次安装"不需要名字。没有它就没有 rollback、没有库存清单、没有"这个 release 装了啥"这些衍生问题 |
| **`--set` 语法**（`a.b=c`） | values 就是一个 JSON 对象。CLI 传文件 / 传 `-f`，不做第二个解析器 |
| **desired-state 对账 / 自动纠正漂移** | 那是控制器模式，要状态历史与所有权；预案是一次性写入 |
| **卸载 / GC / rollback** | 与"不是所有权"互斥（§7.1）；"回滚"在只做加法的库里没有语义（旧版本行还在，裸名下游跟 latest——换回去就是再装一次旧包体） |
| **版本范围 / 依赖求解**（`semver: ^2.0`、`requires: other-package`） | 包之间只通过"名字在库里"耦合；求解器是另一个产品 |
| **一个包内同名能力多版本共存** | 包自己带版本歧义（§3.3）；需要就拆成两个包 |
| **跨命名空间模板**（一份包同时装进多个 ns 并保持联动） | 包是命名空间的输入，不是跨空间的实体 |
| **裸 URL / git 拉包（非 OCI）** | 分发只认 OCI artifact：一份 mediaType、一个 digest、一套标准凭据。支持任意 URL 等于给中枢开一个通用代理，多一个信任面 |
| **模板里的默认值语法**（占位符后面再挂 `default`） | 默认值只写在 `spec.values` 的 JSON Schema 里。两个地方写默认值 = 两份真相 |

---

## 14. 决策索引

一行一条，正文有展开。这份表就是本文的目录：想反驳哪一条，直接看对应小节。

| 编号 | 结论 | 一句话理由 |
|---|---|---|
| **D1** | 包 = `values` + 模板 + **一组文件**（每个能力/源一个）；**参数化是第一位的能力** | 现实里"同一套流程跑 N 个项目"是常态，静态清单逼人复制 N 份包 |
| **D2** | 模板语法只有一份（`source.md §4.1`），本文只加根 `values` 与求值时机 | 两套语法 = 同一份源文档在包里和包外行为不同，这类分叉最难查 |
| **D3** | 包层只用 `{{values}}`，字段引用用 `$(…)`，脚本正文归 shell，三方互不扫描 | 记号一旦重叠就永远说不清"谁来填"；分开之后这一点在语法上就成立，不靠约定 |
| **D4** | 渲染是**结构内插值**，不是文本替换 | 类型保真、不可能注入 YAML 结构、错误能精确到字段 |
| **D5** | 只渲染值，不渲染键 | 键影响派生（如 `ANOTIFY_PARAM_*` 名字），渲染键会把契约搅进模板 |
| **D6** | 渲染后 = 普通文档，走**单建校验**，不设第二套 | 一套校验才能保证"包里装的"和"手建的"逐字节等价 |
| **D7** | 校验分三段：创建期（值无关）/ 渲染期（全部语义）/ 安装期（与库冲突） | 参数化之后创建期拿不到名字，按对象种类切的那套分界失效了 |
| **D8** | 包名（slug）不渲染 | 它要出现在 URL 里；寻址不能依赖输入 |
| **D9** | 没有 release / instance 对象 | 名字由 values 决定，没有名字冲突就不需要实例管理 |
| **D10** | 安装只做加法（I-P1） | 冲突/覆盖之争都假设了"安装会改已有对象"，禁掉这个前提问题就消失了 |
| **D11** | 显式版本同版异内容 / 同名源异 config → **拒绝** | 跳过的代价是不可诊断：库里那行看起来是对的，跑起来才发现不是这份 |
| **D12** | 路由差异不改库，报 `unapplied[]` | 这是"以为装上了、其实没接上"的唯一残余形态，必须点名 |
| **D13** | 包 `metadata.version` 不参与任何东西；升级 = 重装 | 内容寻址已让"改内容 → 新版本 → 裸名下游跟上"成立，再套一层包版本就是两套版本号 |
| **D14** | 折叠键 = `(name, 正文不含 next、不含 version)` | 否则"只改路由"会凭空长出新版本、让所有节点重推 |
| **D15** | 回执落库，且必须存 `values` + `valuesDigest` | 参数化之后 values 就是身份，"装的是哪组值"必须事后可答 |
| **D16** | `writeOnly: true` 的 values 在回执里打码，digest 对打码前算 | 凭证不该进回执，但"同一组值"仍要能比较 |
| **D17** | `notes[].code` 封闭五码 | 稳定机器码才能当 e2e 断言锚点 |
| **D18** | 幂等粒度 = 条目级，不是整包哈希 | "补一次安装"不该变成"再来一遍全量动作" |
| **D19** | `render` 与 `dryRun` 是两个动作：render 只读写包体、不读安装冲突库；dryRun 读库做冲突判定且 0 持久写入 | 写包时要的是"渲染成什么"，上线前要的是"装下去会怎样" |
| **D20** | 不支持卸载；DELETE 回 `kept`，靠 `installed_from` 反查 | "不是所有权"与"级联删除"互斥；反查比"再用 values 渲染一遍"更准 |
| **D21** | 记录（`installed_from` + 回执行）≠ 所有权 | 不然"谁装的"是空话，`kept` 也答不出来 |
| **D22** | 删掉预案自己的四态，聚合成员能力的四条 condition；`next` 改名 `blocker` | v4 删了端口/边/noop，`partial` 已无对应物；`next` 一词语义已被路由占用 |
| **D23** | `upgraded[].affects` = `{edges, sources}` 两个计数 | 事件入口也按名字解析 latest，只报边会漏一半事实 |
| **D24** | 渲染后同名 = 400，不自动去重 | 包是"一份能自圆其说的清单"，同名两次意味着这次渲染自己带版本歧义 |
| **D25** | 名字非法就拒绝，**不自动清洗**（不清洗大小写/空格） | 悄悄改名字会让"包里写的"和"库里存的"永久分叉，且看不出哪一步改的 |
| **D26** | 包是**文件集合**：`playbook.yaml` 只放 values + 引用，一个能力/源一个文件 | 内联成一个大 YAML 让 diff / review / 复用都退化成"改一个 800 行的文件"；拆开后每份都是普通 Capability / Source 文档，能和手建的对照 |
| **D27** | 引用是**相对路径**，路径本身不渲染 | 路径是寻址（同 D8）：能先列出"包里有哪几个文件"，审计才成立 |
| **D28** | 分发走 **OCI artifact**，中枢拉一次、copy-on-pull | 复用现成 registry 与凭据；"外面变了怎么办"用"只拉一次、拉完定死"回答 |
| **D29** | **只拉不推**，不为分发新增 CLI 子命令 | 推是 `oras push` 的事；我们的二进制保持 `serve / install / emit` 三个 |
| **D30** | 包内 `.yaml` 必须全被引用，未引用 = 400 | 静默漏装是本文最怕的失败形态（I-P5）；README 之类非 YAML 不强制登记 |
| **D31** | 实例清单是**读模型**：按 `(namespace, 包, valuesDigest)` 折叠回执行，不建实例表、不新增所有权 | "这个包装了哪几套"必须可答，但答它不需要一个可命名的实体——回执行早就有这份信息 |
| **D32** | `POST` 创建：同 slug 已存在 → 409 `slug_taken`；改包体用 `PUT /v1/namespaces/{namespace}/playbooks/{slug}` 整体替换 | 创建与更新是两个语义，不能用“同 slug POST”兼任覆盖；也不要求删除重建 |
| **D33** | `config` 等价比较先归一化默认值：省略的服务端生成 token 保留已有值、不算冲突；显式 token 参与比较但回执脱敏；其它省略字段不得一律当等价 | 让重装幂等可判定，又不把真实差异吞掉 |
| **D34** | 源同名但 `targets`（含 `params`）不同 → 不改 targets，回 `unapplied[]`（`targets_unapplied`），体检不得全绿 | 与能力 `next` 差异同口径；“装上了但事件去了旧地方”必须可见 |
| **D35** | 安装回执 / `installed_from` 不跟事件 30 天一起删；被引用时不得删回执 | 有限保留不能删出悬空外键（V4-D6） |
