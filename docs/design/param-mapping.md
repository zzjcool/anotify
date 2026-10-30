# 参数映射（字段取值）· 后端实现需求

> 状态：**前端 + mock 已实现，后端未实现**
> 触发：用户提问「源是不是还需要支持变量赋值？比如 GitHub 的 PR 过来，
> 用户可能需要不同的东西组装成一个事件；还有 cron 的用户可能需要自己定义字段」
> 相关不变量：**I6**（中枢不解释任何用户提供的逻辑，无表达式求值）

## 1. 现状（为什么要改）

源的出口参数目前只支持**整份字面量覆盖**：

```go
// internal/route/dispatch.go:241
targetParams := in.Params
if tp, ok := in.TargetParams[targetID]; ok && tp != nil {
    targetParams = tp      // 配了参数 → payload 被整个丢掉
}
```text

后果：

| 场景 | 现在的结果 |
|---|---|
| 源不配 params | payload 原样传给下游（`{"number":42}` 能用） |
| 源配了 params | **payload 完全丢弃**，只用配置的字面量 |

于是用户想要「把 PR 标题传给能力」和「给能力一个固定 env」时二选一，做不到。

cron 更没有自定义字段的能力——payload 是硬编码的：

```go
// internal/source/source.go:242
payload, _ := json.Marshal(map[string]any{
    "source": src.Name, "kind": store.SourceKindCron, "firedAt": fireAt,
})
```text

## 2. 设计（已在前端/mock 落地，后端照此实现）

### 2.1 参数值有两种形态

`source_targets.params` 的**每个值**可以是：

```jsonc
// ① 字面量（与现在完全一致，向后兼容）
{ "env": "prod" }

// ② 映射声明
{
  "title":  { "from": "pull_request.title", "default": "(无标题)" },
  "number": { "from": "pull_request.number" },
  "lines":  { "from": "pull_request.additions", "as": "integer" }
}
```text

混合也行（一部分字面量、一部分映射）。

### 2.2 判定规则（**必须与前端 `isMappingDecl` 一字不差**）

只有**恰好**包含 `from`（外加可选的 `default` / `as`）的对象才算映射：

```go
func isMapping(v any) bool {
    m, ok := v.(map[string]any)
    if !ok { return false }
    if _, has := m["from"]; !has { return false }
    for k := range m {
        switch k {
        case "from", "default", "as":
        default: return false
        }
    }
    return true
}
```text

**多一个字段就退回字面量**。理由：用户真想传 `{"from": 1}` 这种字面量对象时不该被误认为映射。
行为可预期比"聪明"重要。

### 2.3 取值规则（I6 边界）

路径语法只有两条：

- 点号分段：`a.b.c`
- 数组用数字下标：`items.0.name`

取值逻辑：逐段下钻，任何一段取不到就返回「未命中」。**不抛错**——payload 来自外部，形状不受控。

**明确不做**（这些都会越过 I6）：

| 不做 | 原因 |
|---|---|
| 字符串拼接 `"PR: " + x` | 求值 |
| 表达式 / 模板语法 `{{x}}`、`${x}` | 求值器攻击面 |
| 条件 / 默认值表达式 / 三元 | 求值 |
| 函数调用（`upper()`、`split()`） | 求值 |
| 正则 / 通配 / JSONPath 过滤器 | 求值 |

I6 原文：**中枢不解释任何用户提供的逻辑：无表达式求值、无条件匹配、无优先级**。
引入求值器就多出模板注入、无限递归、超大表达式 DoS 三类攻击面，
而收益只是"少一次节点侧拼装"——节点本来就知道数据形状，它才是该做这件事的一方。

### 2.4 未命中与类型错误

三种结果，**都必须可观测**（静默失败是最难查的）：

| 情况 | 行为 |
|---|---|
| 命中 | 用取到的值 |
| 未命中 + 有 `default` | 用 default |
| 未命中 + 无 `default` | 该目标整个剪枝，`reason=param_missing`，detail 写明「参数 x 要从 payload.y 取值，但 payload 里没有」 |
| 类型转不了 | 该目标整个剪枝，`reason=param_type`，detail 写明「参数 x 期望 integer，但 payload.y 的值 "abc" 转不了」 |

**为什么「未命中无默认值」要剪枝而不是跳过该字段**：
跳过会让 `param_schema` 的必填校验报 "x is required"，用户看着配置里明明写了 x
却说必填——真实原因（payload 里没这个字段）完全没体现。

### 2.5 类型转换

`as` 优先，否则**按目标能力的 `params_schema` 自动推断**：

```jsonc
{ "params_schema": { "properties": { "n": { "type": "integer" } } } }
// 配置 { "n": { "from": "pr.number" } }，payload 是 {"pr":{"number":"42"}}
// → 自动转成 42（不需要用户写 as）
```text

支持：`string` / `number` / `integer` / `boolean` / `json`。

自动推断是刻意设计：用户不该为了「把 GitHub 的字符串 title 塞进 integer 字段」
再去学一个 `as`。`as` 留给「schema 说 string 但我想存数字」这种少数情况。

**转换失败必须报 `param_type`**，不能悄悄退回原值——那会让用户看到
"expected integer, got string" 而不知道是 payload 里那个字段本身不是数字。

## 3. 后端改动清单

### 3.1 `internal/store`（无 schema 变更）

映射声明存在现有的 `source_targets.params`（TEXT/JSON）里，**不需要新列**。
但需要：

- [ ] 配置期校验 `validateMappingPaths`（形式校验）：
  - `from` 必须是非空字符串
  - 不允许 `..`、不以 `.` 开头或结尾
  - `as` ∈ {string, number, boolean, json}
  - **配置期不做 schema 校验**（映射的值触发时才存在，无从校验）
- [ ] 字面量部分仍按 `params_schema` 校验（与现在一致）

### 3.2 `internal/route`

新增 `resolveParams(template map[string]any, payload map[string]any) (params map[string]any, missing []MappingMiss, typeErr *MappingTypeErr)`：

- [ ] 实现 §2.2 判定 + §2.3 取值 + §2.5 转换
- [ ] `DispatchInput.TargetParams` 的消费点（`dispatch.go:241`）改为先 resolve 再校验
- [ ] 新增两个剪枝原因：`ReasonParamMissing = "param_missing"`、`ReasonParamType = "param_type"`
- [ ] 需要在 `dispatch.go` 里拿到目标能力的 `params_schema` 做类型推断
      （现有代码在这个位置已经有 `capDef`，可直接用）

**注意**：`DispatchNext` 的递归派发路径（能力 → 能力）**不用**映射——
那里 `Params` 是上游节点的产物，原样传递。映射只作用于**源的首次派发**。

### 3.3 `internal/source`

- [ ] `Trigger.Fire` 把 `src.Targets[i].Params` 交给 route 时，同时传 payload
      供 resolve 使用（现在 `TargetParamsMap()` 只返回模板，需要带上 payload 或
      在 route 侧 resolve）
- [ ] cron：`sources.config.payload` 字面量合并进事件 payload
      （现有硬编码的 `source`/`kind`/`firedAt` 保留，用户字段覆盖同名键）
- [ ] manual fire：同样用 `config.payload` 打底，请求体里的 `payload` 覆盖它

### 3.4 `internal/store`（sources.config）

- [ ] cron 的 `config` 增加 `payload` 字段（无 schema 变更，它是 JSON 列）
      `{"expr":"0 3 * * *","tz":"Local","payload":{"env":"prod"}}`
- [ ] 校验：`payload` 必须是 JSON object（不是数组、不是标量）

### 3.5 API 契约

- [ ] `api/openapi.yaml`：`sourceTargetParams` 的值类型从 `object` 放宽为
      「字面量 | 映射声明」的 oneOf，并写明判定规则
- [ ] `docs/contracts/CONTRACT.md` 补一节 §5.5「参数映射」，明确 I6 边界

## 4. 验收（后端实现时必须有的测试）

单测：

- [ ] `isMapping` 边界：`{"from":"x"}` ✅ / `{"from":1,"y":2}` ❌ 字面量 /
      `{"form":"x"}` ❌ 字面量 / 数组 ❌
- [ ] `getPath`：嵌套对象 / 数组下标 / 段不存在 / 中间是标量 / 空 payload
- [ ] `resolveParams` 四种结果各一例（命中 / default / missing / type 转不了）
- [ ] 混合模板：一个字面量 + 一个映射
- [ ] **自动类型推断**：payload `{"n":"42"}` + schema `integer` → 42
- [ ] `as` 覆盖推断
- [ ] 递归派发（能力→能力）不受映射影响

e2e（`routing` 套件加一节）：

- [ ] webhook 源：payload 里的字段经映射进入任务 params
- [ ] 未命中 + default → 用 default，任务建出来
- [ ] 未命中 + 无 default → `pruned reason=param_missing`，detail 含字段名
- [ ] 类型转不了 → `pruned reason=param_type`
- [ ] cron 源自定义字段 → 出现在事件 payload 与任务 params 里

## 5. 已实现的部分（本次提交）

**前端 `web-src/pages/sources.html`**：

- 每个出口参数有「固定值 / 从事件取值」两态切换（`.param-mode-btn`）
- 映射态：payload 路径输入框 + 默认值输入框 + 说明文字
- 收集时按当前可见态判断，不能靠"哪个有值"（映射态 from 为空但 default 有值时会误判）
- cron 弹窗新增「事件字段（可选）」JSON 输入框（`#f-payload`）
- 默认值按参数声明类型解析（integer 字段拿到数字而不是 `"3"`）

**mock `web/mock.js`**：

- `isMapping` / `getPath` / `resolveParams` / `validateMappingPaths` / `coerceValue` / `schemaPropType`
- fire 端点接入 resolve，剪枝新增 `param_missing` / `param_type`
- cron/manual 的 `config.payload` 作为事件 payload 基础（请求 body 覆盖它）
- 配置期只校验映射形式 + 字面量 schema

**实测（mock）**：

| 用例 | 结果 |
|---|---|
| 映射命中（payload `{pr:{n:42}}` → integer 字段） | ✅ 任务 params = 42 |
| 未命中 + default 7 | ✅ 任务 params = 7 |
| 字面量（不映射） | ✅ 任务 params = 99（向后兼容） |
| 类型转不了（`"不是数字"` → integer） | ✅ `pruned param_type` |
| cron `config.payload={env:prod,lines:123}` + 映射 `lines` | ✅ 任务 params = 123 |

i18n：4 语言各加 8 个 key（`pm_literal` / `pm_map` / `from_hint` / `default_ph` /
`need_from` / `payload` / `payload_hint` / `bad_payload`）。

## 6. 为什么不直接上模板语法

如果用户将来真的需要「拼接 / 条件 / 运算」，正确做法是**在节点侧做**，不是在中枢：

- 节点是执行者，它本来就跑用户代码、本来就知道数据形状
- 中枢只搬运数据，它的可信边界才成立（契约 §1、I6）
- 一旦中枢求值，它就从"路由器"变成"解释器"，安全模型完全不同

所以中枢的能力上限就是**声明式取值**，这已经覆盖了绝大多数真实用法
（GitHub PR 的 title / number / author，cron 的自定义常量）。

---

# 附：源变量（source variables）· 后端实现需求（v2）

> 状态：**前端 + mock 已实现，后端未实现**
> 触发：用户提问「源的默认变量，能力配置时引用 `{{$A}}` 这样」
> 前置：本文档 §1–§6 的参数映射已在此基础上演进；**本节是推荐形态**

## A.1 为什么从「payload 路径」升级成「变量」

§2 的形态是 `{"title": {"from": "pull_request.title"}}` —— 能用，但：

- 用户要记 `pull_request.head.ref`、`pull_request.user.login` 这种深层路径
- cron 源**没有 payload**，只能写死 `config.payload` 里的一坨 JSON，和 webhook 是两套写法
- 同一个路径在多个出口参数里重复出现，改一处要翻遍所有出口

变量把「源对外暴露什么」显式声明出来：

```jsonc
// sources.config.vars —— 这个源提供哪些数据
[
  { "name": "title",  "from": "pull_request.title" },   // webhook：从请求体取
  { "name": "env",    "value": "prod" },                // cron：常量
  { "name": "lines",  "from": "pull_request.additions" }
]
```text

```jsonc
// source_targets.params —— 参数引用变量
{ "title": { "var": "title" },
  "env":   { "var": "env" },
  "n":     { "var": "lines" } }
```text

**两种引用形态并存**（向后兼容）：`{from: "payload.path"}` 与 `{var: "name"}`。
同时写两个 → 配置期拒绝（"到底取哪个"没有合理默认）。

## A.2 I6 边界：为什么 `{{$A}}` 不能做，而 `{var:"A"}` 可以

用户原话是想要 `{{$A}}` 这种模板语法。**这不做**，原因是它和变量引用是两件事：

| 形态 | 中枢要做什么 | I6 |
|---|---|---|
| `{"title": {"var": "A"}}` | 查表取值，**原样传** | ✅ 不解释逻辑 |
| `{"env": "prod"}` | 原样传 | ✅ |
| `{"title": "PR: {{$A}} by {{$B}}"}` | **字符串解析 + 替换 + 拼接** | ❌ 求值器 |

引入模板语法就多出三类攻击面：模板注入（`{{` 嵌套/递归）、表达式 DoS（超长/深嵌套）、
以及"用户以为支持条件/运算"的语义漂移。

**变量名不是表达式**：`$title` 里没有运算符、没有函数、没有嵌套。它就是一个键。
所以 `{var:"title"}` 在法律上等价于"从字典里读一个键"，和 `{from:"a.b.c"}` 是同一类操作。

如果用户真的要拼接/条件，正确做法是**在节点侧做**（节点是执行者，它跑用户代码）。

## A.3 后端改动清单

### A.3.1 `internal/store`（无 schema 变更）

`sources.config` 是自由 JSON 列，`vars` 存在里面即可。

- [ ] `validateVarDefs(vars) error`：
  - `name` 非空且匹配 `^[A-Za-z_][A-Za-z0-9_]*$`
  - 名字不重复
  - `from` 与 `value` **恰好有一个**
  - `from` 路径形式合法（非空、不含 `..`、不以 `.` 开头/结尾）
- [ ] `Source` 增加便捷方法 `VarDefs()`、`VarValue(name)`（可选，也可在 trigger 里直接读）

### A.3.2 `internal/route`

- [ ] `isMapping` 扩展到接受 `var`（与 `from` 互斥）
- [ ] 新增 `resolveVars(varDefs, payload) (values map[string]any, missing []string)`
- [ ] `resolveParams(template, payload, varValues)`：
  - `{var}` → 从 `varValues` 取
  - `{from}` → 从 `payload` 取（保持不变）
  - 未命中 + 无 default → `param_missing`，detail 要写 `$var` 或 `payload.x` 区分来源
- [ ] 配置期校验：参数引用的变量**必须在源的 vars 里声明过**
      （不校验的话拼错 `$titel` 要等触发时才在剪枝里看到）

### A.3.3 `internal/source`

- [ ] `Trigger.Fire` 里先 `resolveVars(src.VarDefs(), payload)`，再传给 route
- [ ] cron 的 `config.payload` **保留**（向后兼容老数据），但新语义下它等价于
      "所有 `value` 型变量" —— 可以把 payload 由 vars 推导出来：
      `payload = {v.name: v.value for v where has(value)}`
- [ ] manual fire 同理

### A.3.4 API 契约

- [ ] `api/openapi.yaml`：`sourceTargetParams` 的 oneOf 增加 `{var,default,as}` 形态
- [ ] `sources.config` 增加 `vars` 的 schema 描述（数组，元素 oneOf from/value）
- [ ] `docs/contracts/CONTRACT.md` 新增 §5.5「源变量与参数引用」，
      明确写清 **不做模板语法** 及其理由（I6）

## A.4 验收（后端实现时必须有的测试）

单测：

- [ ] `validateVarDefs`：非法名 / 重名 / from+value 并存 / 两者都没有
- [ ] `isMapping`：`{var:"x"}` ✅ / `{var:"x",from:"y"}` ❌ / `{var:1}` ❌
- [ ] `resolveVars`：from 命中 / from 未命中 / value 常量 / 空 vars
- [ ] `resolveParams`：`{var}` 命中 / 未命中 + default / 未命中无 default（`param_missing`）
- [ ] 变量引用 + 类型推断：`{"var":"n"}` 且 schema 是 integer，payload 里是 `"88"` → 88
- [ ] 配置期拒绝未声明变量

e2e（`routing` 套件加一节）：

- [ ] webhook 源声明 `vars: [{name:"n", from:"pr.count"}]`，
      出口参数 `{var:"n"}` → 任务 params 拿到值
- [ ] cron 源 `vars: [{name:"env", value:"prod"}]` → 同一套写法可用
- [ ] 引用未声明变量 → 创建/更新源时 400，错误信息含变量名
- [ ] 变量 from 未命中且无 default → `pruned param_missing`，detail 含 `$var`

## A.5 已实现（本次提交，前端 + mock）

**mock `web/mock.js`**：

- `isMapping` 支持 `var`（与 `from` 互斥）
- 新增 `isVarRef` / `resolveVars` / `validateVarDefs`
- `resolveParams(template, payload, varValues)` 支持两种引用
- `validateMappingPaths(template, declaredVars)`：变量必须声明过
- fire 端点：先 `resolveVars` 再 `resolveParams`
- POST/PATCH `/v1/sources`：校验并持久化 `config.vars`
  （PATCH 的 webhook/manual 分支要把 `vars` 一起带上，否则"只改 token"会清空变量）

**前端 `web-src/pages/sources.html`**：

- 源弹窗新增「变量」区：`名字 = 从事件取/固定值 + 值`，可增删
- 出口参数的映射态新增「选变量」下拉；选了变量时路径框禁用
- 变量改名后已渲染的下拉实时更新（不重建整个参数区，避免清掉用户正在填的输入）
- 配置期本地先校验未声明变量（不用等后端 400）
- **删除了上一轮的「事件字段」JSON 框**：它和变量的「固定值」是同一件事，
  两个入口会让人不知道写哪个。现在统一走变量，
  保存时把 `value` 型变量折算进 `config.payload`（兼容老数据）

**实测（mock）**：

| 用例 | 结果 |
|---|---|
| 变量引用 `{var:"n"}`，payload `{pr:{count:42}}` | 任务 params = **42** |
| 变量字面量 `{name:"n",value:7}` | 任务 params = **7** |
| 引用未声明变量 `titel` | **400**，错误含变量名 |
| 非法变量名 `9bad` | **400** |
| UI 全流程（填变量 → 选变量 → 保存 → 触发） | `vars.lines="88"` → 任务 `changedLines: **88**`（integer 自动推断） |

i18n：4 语言各加 15 个 key（`vars`/`add_var`/`var_from`/`var_value`/`var_none`/
`bad_var_name`/`dup_var`/`unknown_var` 等）。

## A.6 两个实现时容易踩的坑（我踩过）

**① `buildVarChooser` 必须在输入框进 DOM 之后调**

它要 `pathIn.parentNode.insertBefore(...)`。放在 `appendChild` 之前会拿不到
`parentNode` 抛 TypeError —— 而 `openModal` 是 `async` 且调用处没有 `await`，
异常变成**静默的 unhandled rejection**，表现为"点新建源没反应"，
`pageerror` 也收不到。定位方法是往页面里注入 `unhandledrejection` 监听。

**② 值输入框不能用闭包捕获 `isVal`**

用户切换「从事件取 / 固定值」之后，那是个常量不会变，输入会一直写进旧字段
（表现为"填了固定值但保存后是空的"）。每次输入都要从下拉的**当前值**判断。

---

# 附二：简化后的形态（`{{path}}` 直接引用）

> 状态：**前端 + mock 已实现**
> 触发：用户指出「为什么还要让人选固定值/从事件取值」「为什么变量还要单独赋一次值，直接暴露出来引用不可以吗」

这两条批评都成立，前一版（A 章）设计过度了。

## B.1 错误一：把「引用」和「模板求值」混为一谈

A 章写「不做 `{{x}}`，因为那是求值器」。这个判断**只在 `{{x}}` 参与拼接时成立**：

| 写法 | 中枢要做什么 | I6 |
|---|---|---|
| `"{{pull_request.title}}"` | 解析出路径 → 查表 → 原样返回 | ✅ **不解释逻辑** |
| `"PR: {{pull_request.title}}"` | 解析 + 替换 + 拼接 + 转义 | ❌ 求值器 |

第一种只是一次查表，和 `{"from": "pull_request.title"}` 完全等价，`{{}}` 只是更短的写法。
**而且它是「整个值占满」的**，不存在嵌套、运算、函数调用——没有求值面。

没有理由为此强加一个 `{from: ...}` 的对象语法。

## B.2 错误二：变量表变成了纯冗余

A 章的 `{name:"title", from:"pull_request.title"}` 要用户：

1. 先去变量表声明 `title = pull_request.title`
2. 再去参数里选 `$title`

如果参数里能直接写 `{{pull_request.title}}`，**第 1 步完全没有必要**——
用户想引用哪个字段，看到什么就写什么。

变量表只在两种情况下才有价值：

- **改名/复用**：同一个路径在 8 个参数里出现，改路径时只改一处
- **cron/manual**：没有 payload，只能给固定值

所以它现在是**可选的便利功能**，不是必经步骤。

## B.3 现在的形态

一个输入框，按内容自动识别：

```text
120                          ← 字面量，按 schema 转成 integer
prod                         ← 字面量
{{pull_request.additions}}   ← 引用 payload 路径
{{$env}}                     ← 引用源变量（固定值或它的 from）
```text

界面不再有「固定值 / 从事件取值」的按钮切换——用户脑子里想的是
「这个值从哪来」，不是「我这次算哪种填法」。有内容就以内容为准。

## B.4 仍然拒绝的写法

```text
"PR: {{pull_request.title}}"        ← 前后缀拼接 = 求值
"{{a}}{{b}}"                        ← 多个引用拼接
"{{toUpper(x)}}"                    ← 函数调用
"{{#if x}}...{{/if}}"               ← 条件
```text

配置期就报错并说明原因（`sources.form.no_interp`），不是等触发时静默失败。

## B.5 mock 实现要点

- `parseTemplate(v)`：正则 `^\{\{\s*([^{}]+?)\s*\}\}$` —— **锚定首尾**，
  所以 `"PR: {{x}}"` 自然不匹配，落到字面量分支
- `resolveParams`：先看字符串形态的 `{{x}}`，再看对象形态的 `{from}`/`{var}`
  （后者保留，供旧配置和需要 `default`/`as` 的场景）
- `{{$name}}` → 源变量表；`{{a.b.c}}` → 直接按 payload 路径取，**不需要先声明**
- `validateTargets` 配置期校验时**跳过**模板引用：它的实际值要等触发才知道，
  不能拿字符串去校验 "expected integer"（这是个真踩过的坑）

## B.6 后端改动清单（更新 A.3.2）

- [ ] `route.parseTemplate` + `resolveParams` 支持字符串形态的 `{{x}}`
- [ ] 正则必须锚定首尾（`^...$`），否则混写会被误当成引用
- [ ] `{{$name}}` 查变量表，`{{path}}` 直接查 payload
- [ ] **配置期 schema 校验必须跳过模板引用**，否则合法的引用配置会被拒
- [ ] 未命中且无 default → `param_missing`，detail 里带上原始引用文本
      （用户写的是 `{{pull_request.additions}}`，报错就该原样回显这个，而不是拆成两半）

## B.7 顺带修掉的 UI 债

- 删掉 `.param-mode` / `.param-body` / `.param-map` / `buildVarChooser` /
  `refreshVarChoosers` / `hintText`（约 120 行死代码）
- `type=number` 的输入框**静默拒绝**非数字内容 —— 写 `{{x}}` 进去会被清空。
  现在引用时切成 `text`，输入以 `{` 开头时也切，清空后切回 `number`
- Go template 会解析 `web-src/pages/*.html` 里的 `{{...}}`，脚本里直接写
  双大括号会报 `function "pull_request" not defined`。改用 `TPL_OPEN` /
  `TPL_CLOSE` 常量拼接，注释里也避开完整的花括号对

---

# 附三：支持模板拼接（修正 B 章的过度限制）

> 状态：**前端 + mock 已实现**
> 触发：用户「PR: {{pull_request.title}} 为什么不支持呢，我其实就是想要这种，有时候通知类能力就是需要自己拼接模板」

B 章把「整值引用」和「混写」划了一条线，认为混写是求值器。**这个划线仍然过严了。**

## C.1 关键区分不是「有没有前后缀」，而是「替换后还解不解析」

真正危险的是**替换结果被再次当作模板扫描**（模板引擎的递归展开）：

| 能力 | 单趟替换 | 递归展开 |
|---|---|---|
| 无限展开（`{{a}}` 展开出 `{{b}}`…） | 不会 | 会 |
| 函数调用 `{{toUpper(x)}}` | 解析不了 | 可以 |
| 访问外部资源 | 不会 | 可能 |
| 本质 | `strings.Replace` | 解释器 |

单趟替换里，用户提供的是「一段文本 + 若干取值点」，不是「一段要被执行的程序」。**`"PR: {{title}}"` 和一个 HTML 模板里写 `<b>{{title}}</b>` 是同一类东西**——所有服务端渲染框架都在做这件事，没有人认为那是引入了求值器。

## C.2 现在支持的写法

```text
120                                     字面量（数字）
staging                                 字面量（字符串）
{{pull_request.additions}}              整值引用 → 保留原类型（数字还是数字）
PR #{{pull_request.number}}: {{pull_request.title}} by {{pull_request.user.login}}
                                        模板拼接 → 结果一定是字符串
[{{$env}}] {{$author}}                 变量 + 拼接
```text

**整值引用保留类型**这点很重要：

```text
"{{pull_request.additions}}"        →  42         (number)
"PR has {{pull_request.additions}}" →  "PR has 42" (string)
```text

同一份数据，整值引用能进 `integer` 字段，拼接就只能进 `string` 字段。这是语义决定的，不是实现限制。

## C.3 仍然不做

```text
{{#if x}}...{{/if}}      条件
{{#each xs}}...{{/each}} 循环
{{toUpper(x)}}           函数调用
{{a + b}}                运算
```text

**占位符正则只接受标识符 + 点 / $ / 数字下标**：

```js
/\{\{\s*([$A-Za-z_][$A-Za-z0-9_.]*)\s*\}\}/g
```text

所以 `{{a + b}}` 不会被当成表达式求值，而是被当成「查一个叫 `a + b` 的路径」（查不到 → `param_missing`）。这是有意的：**语法上就没有运算的位置**。

## C.4 类型处理（实现要点）

| 情况 | 类型推断 |
|---|---|
| 对象形态 `{from}` / `{var}` | `as` 优先，否则 `params_schema` |
| 整值引用 `{{x}}` | 保留原类型（payload 里是数字就是数字） |
| 混写 | `params_schema`（结果必是字符串） |
| 纯字面量 | 不转换，尊重用户写的 |

混写对 `integer` 字段：`"PR #42"` 转不成数字 → `param_type` 剪枝，detail 说清楚。这是正确的——用户确实把一段文字塞进了数字字段。

## C.5 后端改动清单（替代 B.6）

- [ ] `renderTemplate(s, lookup)`：**一次性遍历 + 拼接**，绝不对替换结果再扫描
- [ ] 占位符正则严格限定为标识符路径（不允许括号/运算符/空格表达式）
- [ ] 整值引用走「保留原类型」分支，混写走「一律字符串」分支
- [ ] 未命中时**原样保留 `{{x}}` 文本**并记 `param_missing`（用户要能看见自己写了什么，而不是只被告知"某字段取不到"）
- [ ] 配置期 `validateTargets` 必须跳过**所有含占位符的值**（不只整值引用——混写同样是运行时才知道类型）
- [ ] `{{$name}}` 未声明 → 配置期 400

## C.6 三个实现坑（都真踩过）

**① `type=number` 的输入框会静默吞掉 `{{x}}`**

浏览器对 `type="number"` 输入非数字内容会清空 value，表现为"填了但保存后没有"。现在引用时切成 `text`，输入以 `{` 开头时也切，清空后切回 `number`。

**② 配置期 schema 校验把混写当字面量**

`"PR #{{x}}"` 拿去校验 `expected integer` → 合法配置被拒。校验必须跳过所有含占位符的值。

**③ Go template 会把脚本里的双花括号当语法**

`web-src/pages/*.html` 走 Go template 渲染，JS 里写 `{{...}}` 会报 `function "pull_request" not defined`。解法：

- 值用 `TPL(ref)` 常量拼接
- **正则在字符类里拼花括号**：`"[" + "{" + "]"` 得到 `[{]`，效果等价于字面 `\{`，但源码里不出现双花括号

---

# 附四：去掉 `$`，变量表降级为纯别名（D 章）

> 状态：**前端 + mock 已实现**
> 触发：用户「{{$env}} 和 {{pull_request.additions}} 这两个有什么区别，为什么一个带 $」
> + 「我不明白为什么有 payload 和变量表之分」

这两个疑问指向的是同一个问题：**模型里多了一层没有理由的区分。**

## D.1 `$` 是纯实现细节的语法

```text
{{$env}}                  查变量表
{{pull_request.title}}    查 payload
```text

用户想的是「这个值从哪来」，却要先记住「我该不该加 `$`」。而 `$` 本身**不携带任何语义**——它只是我在实现里区分两个 Map 的开关。

更糟的是：

- 想用固定值 `prod`，得先去变量表声明 `env = prod`，再写 `{{$env}}` —— 绕一圈
- 而直接写字面量 `prod` 就行

**那 `{{$env}}` 比 `prod` 多了什么？** 除了「改一处生效多处」，什么都没有。为了这个让用户学一个符号，不划算。

## D.2 payload 和变量表为什么会分成两个

这是一段**历史叠加**，不是设计：

| 阶段 | 发生了什么 |
|---|---|
| 最早 | 只有 payload（事件请求体），参数写 `{from: "路径"}` |
| 加 cron | cron 没有请求体，于是塞了个 `config.payload` 让它也能带数据 |
| 加变量 | 为了让 cron 也能给字段起名，做了 `vars` |
| 结果 | `vars` 的 `value` 项又被折算回 `payload` —— **同一份数据存两遍** |

证据就是这行代码（改动前）：

```js
const payload = {};
for (const v of vars) {
  if (v.value !== undefined) payload[v.name] = v.value;
}
config = { expr, payload, vars };   // value 型变量同时存在于 payload 和 vars
```json

## D.3 现在的模型

只有两个真实概念：

```text
事件（payload，由外部系统产生）
    ↓
参数取值：字面量 / {{路径}} / {{别名}}
```text

**别名表不是第三样东西**，它只是「短名字 → payload 路径」的映射，不承载数据：

```json
"config": { "vars": [{ "name": "author", "from": "pull_request.user.login" }] }
```text

```text
{{author}}  等价于  {{pull_request.user.login}}
{{pull_request.title}}         直接用，不必先声明
"staging"                      字面量，不需要变量
```text

解析顺序：**先查别名表（未命中）→ 再当 payload 路径**。名字冲突时别名优先——别名本来就是为了改写/缩短而存在。

## D.4 废弃项

| 项 | 处置 |
|---|---|
| `{{$name}}` 语法 | 不再生成。老配置仍认（去掉 `$` 后按别名查） |
| `config.vars[].value` | 不再生成。老配置仍认（当常量处理） |
| `config.payload`（前端写入） | **不再写入**。常量直接写在参数里 |
| `config.payload`（读取） | 保留兼容 |

## D.5 验证

```text
别名 {{lines}} → {{pull_request.additions}}  →  changedLines: 42   ✅
直接 {{pull_request.additions}}               →  changedLines: 9    ✅
保存后的 config: vars=4 项别名，payload 不写入                        ✅
cron 常量直写参数（零别名）                    →  changedLines: 5    ✅
页面正文不出现 `$`                                                  ✅
```text

## D.6 后端改动清单

- [ ] `resolveParams` 的 lookup：先查别名表 → 再查 payload
- [ ] 废弃 `$` 前缀（保留兼容读取）
- [ ] `validateVarDefs`：只要求 `from`；`value` 兼容但不再接受新建
- [ ] 前端保存时不再写 `config.payload`
- [ ] 配置期不再强制「引用的名字必须声明过」——`{{pull_request.title}}` 无需声明；
      拼错会在触发时以 `param_missing` 报出，并**原样回显用户的引用文本**

---

# 附五：后端落地（E 章）

> 状态：**后端已实现**（issue #114）。本章不重复论证，只写「落在哪、按哪条规则跑、
> 与冻结契约怎么对应」。
>
> **规则权威是 `docs/contracts/CONTRACT.md` §5.5**；本章与 A~D 章有出入的地方以 §5.5 为准
> （差异点集中在 E.6）。**A~D 章保留为历史记录，不回改**——它们是当时的设计推理，
> 改掉就看不到模型为什么演化成现在这样。

## E.1 落地位置

| 文件 | 职责 |
|---|---|
| `internal/route/params.go` | 取值与替换的全部纯函数：`HasPlaceholder` / `WholeRef` / `RenderTemplate` / `GetPath` / `IsMapping` / `IsVarRef` / `CoerceValue` / `SchemaPropType` / `ResolveVars` / `ValidateParamTemplates` / `ValidateLiteralParams` / `ResolveParams` |
| `internal/route/prune.go` | 两个剪枝原因：`ReasonParamMissing = "param_missing"`、`ReasonParamType = "param_type"`；`PruneEventType` 自动产出 `pruned.param_missing` / `pruned.param_type` |
| `internal/route/dispatch.go` | `DispatchInput` 增 `Payload` / `VarValues`；步骤 ② 里 `TargetParams[targetID]` 有值时走 `ResolveParams`（失败 → 整条剪枝），没有条目时回退公共 `Params`（行为不变） |
| `internal/store/source.go` | `config` 的解析与校验：`VarDefs()` / `VarNames()` / `ConfigPayload()` / `ValidateVarDefs()`；`ValidateSourceConfig` **保留** `config.vars` 与 cron/manual 的 `config.payload`（过去会静默抹掉 = 丢数据） |
| `internal/source/source.go` | `Fire` / `DryRun` / `Scheduler.Tick`：先 `VarDefs()` + `ResolveVars()`，再把 `Payload` / `VarValues` 传进 `DispatchInput`；**cron/manual** 用 `config.payload` 打底（webhook/github 不合并：投递体即 payload） |
| `internal/server/source_handlers.go` | 配置期 400 链：`ValidateVarDefs` → `ValidateParamTemplates`（形式）→ `ValidateLiteralParams`（**只对能完全解析的字面量逐字段**）；错误消息含能力名 / 字段名 / 变量名；PATCH 的 `config` 合并（A6/A18） |

单测落在 `internal/route/params_test.go`、`internal/route/dispatch_test.go`、
`internal/store/source_test.go`、`internal/source/source_test.go`、
`internal/server/source_handlers_test.go`。

## E.2 取值规则

- 形态：`{{点号路径}}`（含混写）与旧对象形态 `{from}` / `{var,default,as}`；`from` 与 `var` **互斥**。
- 顺序：`{{name}}` **先查 `config.vars` 别名表**，未命中再当 payload 点号路径；`{var:"name"}` **只查别名表**。
- 路径：点号分段 + 数组十进制下标（`items.0.name`）；只查 map 自有键；`"0.0"` / `"-1"` / `"1e2"` / 越界一律 miss，不抛错。
- `{{$name}}` **已废弃且不再识别**：占位符正则以 `[A-Za-z_]` 起始、**不含 `$`**，所以老写法 `{{$name}}`
  在配置期被拒（`400`，提示「占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）」）；
  改成 `{{name}}` 即可（别名语义不变）。存量配置里已经存下的 `{{$name}}` 在**派发期按字面量原样透传**。
- **旧 `$` 剥除兼容仍保留，但只对映射声明生效**：`{"from":"$name"}` 会把前导 `$` 剥掉再查（老文档把变量写成 `$name`），
  这与 `{{$x}}` 不被识别不矛盾：前者是对象形态的 `from` 路径写法，后者是占位符语法。

**对象形态的判定顺序（A15）**：先把 `from` + `var` **并存**判成互斥错 → 再用 `IsMapping` 判是不是
映射声明 → **不是映射声明就按字面量放过**。所以 `{"from": 1}`（值不是字符串）、
`{"from":"a","x":2}`（多一个键）都能存，它们在派发期同样按字面量原样传。
配置期只有三种会 400：`from`+`var` 并存、`{var}` 引用了未声明的别名、
**写全了 `default`/`as` 却没有 `from`/`var`**（漏写主体的高概率形态）。

## E.3 类型

| 情况 | 依据 |
|---|---|
| 整值引用 `"{{x}}"` | 保留原类型，再按目标 `params_schema` 转换（`"42"` + `integer` → `42`） |
| 混写 `"PR #{{n}}"` | 结果必字符串，按 `params_schema` 转换 |
| 对象形态 `{from}` / `{var}` | `as` 优先，否则 `params_schema` |
| 纯字面量 | **不转换**，尊重用户写的（类型不对由 `ValidateParams` 报 `param_schema`） |

`as` 在**配置期**只允许 `string` / `number` / `boolean` / `json`（其余 400）；**触发期**
`CoerceValue` 认的集合更宽（`string` / `number` / `integer` / `boolean` / `array` / `object`）——
这是刻意的：配置期收敛是为了不让用户写没意义的东西，触发期放宽是为了不让历史配置
在改严之后突然失效。

转换失败必须报 `param_type`，**绝不静默退回原值**（退回会让用户看到 "expected integer,
got string" 却不知道是 payload 里那个字段本身不是数字）。

和 `web/mock.js` 的 `coerceValue` **结果一致、拦截阶段不同**：`integer` 遇到 `"4.5"` 时，
mock 的 `coerceValue` 不拒（只做数字化，给出 `4.5`），靠它自己的 `validateParams` 以
`expected integer`（`param_schema` 口径）挡下；后端在 `CoerceValue` 直接返回 `(nil,false)` → `param_type`。
两边都不会把非法值派出去，只是 reason / 阶段不同（拿 mock 当参考实现时注意）。

## E.4 未命中与剪枝

| 情况 | 行为 |
|---|---|
| 命中 | 用取到的值 |
| 未命中 + 有 `default`（只有对象形态有） | 用 `default` |
| 未命中 + 无 `default` | 该目标整条剪枝，`reason=param_missing` |
| 类型转不了 | 该目标整条剪枝，`reason=param_type` |

**`null` 一律算未命中（A16）**：无论 `nil` 来自**别名表的值**（`{name,value}` 常量声明成 `null`，
或 `from` 路径指到的字段是 `null`），还是 payload 里的**显式 `null`**，都按「取不到」处理——
有 `default` 用 `default`，否则 `param_missing`。**混写里未命中不得静默拼成空串**：
`"PR #{{n}}"` 在 `n` 为 `null` 时不会产出 `"PR #"`，而是整条目标按 `param_missing` 剪枝。
`{var}` 与 `{{x}}` 在这一条上完全一致（A7）。

整条剪枝而不是跳过字段：跳过会让 `param_schema` 报 "x is required"，而用户配置里
明明写了 x——真实原因（payload 里没这个字段）完全没体现。

`detail` 的形态是「**能力前缀 `能力名@版本 ` + 冻结正文**」（多条 miss 用 `；` 连接），
正文逐字见 CONTRACT §5.5，测试按子串与等值各钉一条；关键是**回显用户写的原文**
并区分来源：`{{pull_request.title}}` / `payload.pull_request.title` / `$title`。
前缀不是装饰：扇出到一个源上多个能力时，只有它能回答「是哪个目标的参数出了问题」。

## E.5 配置期 vs 触发期

| 时机 | 做什么 | 不做什么 |
|---|---|---|
| 建源 / 改源（400） | ① `ValidateVarDefs`：`config.vars` 形式校验（名字合法 / 不重名 / `from` 与 `value` 恰有其一 / `from` 路径形态）；② `ValidateParamTemplates`：参数模板**形式**校验（**占位符闭合检查先于**「是否含占位符」——分两档、两类消息，见 E.6；只含 `}}` 的字面量放行）、路径形态、`as` 枚举、`from`+`var` 互斥、`{var}` 必须已声明、不是映射声明的对象按字面量放过（A15）；③ `ValidateLiteralParams`：**只对能完全解析的字面量逐字段**校验（见下） | 不对含占位符 / 映射声明的**子树**跑 schema 校验（类型要触发时才知道）；不判对象级 `min/maxProperties` |
| 触发派发（剪枝） | `ResolveParams` → 类型转换 → **同一个** `ValidateParams`（全量） | 任何静默跳过 |

**配置期为什么不能用 `ValidateParams` 直接跑整份模板（A14）**：`required` 会因为「值还没求」而误拒
「字面量 + 占位符混用」的合法模板。所以 `route.ValidateLiteralParams(schema, template)` 的四条口径是：

1. **顶层 `required` 按模板键集合判**：键写了就算满足（值是占位符 / 映射声明都算），**不看值**；
2. 只对**完全解析得出**的字面量**逐字段**校验（路径 `params.<key>`，用与派发期同一个校验器）——
   `{"n":"abc"}` 对 `integer` 这种当场能判的错依旧当场报；
3. 含**未解析**内容（完整占位符 / `{from}`·`{var}` 映射声明，含**嵌套**）的整棵子树跳过，派发期兜底；
4. **对象级**约束（`minProperties` / `maxProperties`）不判；`additionalProperties: false` 仍判（键名不依赖求值）。

自测口径的例子：schema `required: [to, title]` + `{"to": "{{pull_request.user.login}}", "title": "hi"}`
**必须能保存**（`to` 的键在 → 必填满足；它的值含占位符 → 跳过类型校验；`title` 是字面量 → 照常校验）。
过去「剔掉占位符的子集再跑 `ValidateParams`」会把 `to` 当缺失 → 误报 `missing required field "to"`。

- 事件 payload 的打底只对 **cron / manual** 生效；webhook / github 用投递体（历史行里遗留的
  `payload` 键不被复活）。因此 **`config.payload` 的「必须是 JSON 对象」也只对 cron/manual 校验**（A17）：
  webhook / github **直接丢弃该键**，残留的坏值不会因这次保存而 400。
- **PATCH 的 `config` 合并（A6 / A18）**：`"config": null` 视为「本次不改 config」（no-op）；
  `config` 里**未带** `vars` / `payload` 键时从当前配置**继承**（显式传 `"vars": []` / `"payload": {}` 才清空）。
  不这么做的话，控制台「只改 cron 表达式」会把变量表抹掉，紧接着既有的 `{var:"x"}` 全变未声明 → 400。
- 占位符的两类消息：`{{`/`}}` **数量不等** →「占位符没有闭合（{{ 与 }} 数量不一致）」；
  **数量相等但抹掉合法占位符后仍有残留**（`{{x + 1}}` / `{{f(x)}}` / `{{#if x}}` / `{{.a}}` / `{{$x}}`）→
  「占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）」。两种原因不能给同一句话。
- `{{路径}}` **不要求声明**——它可能就是 payload 路径；拼错在触发时报 `param_missing` 并回显原文。
- `{var:"name"}` 是显式引用，**配置期必须已声明**（否则 400，消息含变量名）。
- `DryRun` 与 `Fire` 走同一套解析（dry-run 也要能看到 `param_missing` / `param_type`）。
- 解析只作用于**源的首次派发**；能力 → 能力的递归派发原样传递，不解析。

## E.6 兼容与差异（相对 D 章）

| 项 | D 章写法 | 落地口径（§5.5） |
|---|---|---|
| `config.vars` | 已弃用，读取兼容 | **deprecated 但读取兼容**：仍用作**纯别名表**（`{{name}}` → 它的 `from` 路径）；`{name,value}` 常量仍当别名值读 |
| `config.payload` | 前端不再写入、读取兼容 | 保留读取：**只对 cron/manual** 作事件 payload 打底，**用户字段覆盖同名键**（包括 `source`/`kind`/`firedAt`）；webhook/github 不合并（投递体即 payload） |
| `{{$name}}` | D.4 说「老配置仍认」 | **不认**：正则不含 `$` → 配置期 400（文案落在「数量相等但残留」那档：`占位符不合法（…或 $）`）；存量值派发期按字面量透传。`{from:"$name"}` 的前导 `$` 仍会剥掉 |
| 占位符闭合检查 | A~D 章只提到「半截占位符」 | **分两档、两类消息分开**：① 只含 `}}`（如 `{"a":{"b":1}}` 当字符串参数）→ **放行**；② `{{`/`}}` 数量不等 → `占位符没有闭合（{{ 与 }} 数量不一致）`；③ 数量相等但残留（`{{x + 1}}`/`{{f(x)}}`/`{{#if x}}`/`{{.a}}`/`{{$x}}`）→ `占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）` |
| `{from}` / `{var}` 对象形态 | 支持 | 作为**向后兼容**保留；`as` 配置期只允许 `string`/`number`/`boolean`/`json` |
| `validateVarDefs` | 「只要求 `from`」 | `from` 与 `value` **恰有其一**（两者并存 / 都没有 = 400） |
| 配置期变量声明要求 | 「不再强制引用的名字必须声明过」 | 对 `{{路径}}` 不强制；对 `{var:"name"}` **强制**（显式引用拼错必须当场 400） |
| 配置期的 schema 校验 | A~D 章没区分函数 | 用 `ValidateLiteralParams`（A14）：`required` 按**模板键集合**判、只对能完全解析的字面量逐字段校验、未解析子树跳过、对象级 `min/maxProperties` 不判（详见 E.5） |
| 对象形态的「多一个键」 | D 章说按字面量 | 口径相同，但把**判定顺序**写死（A15）：先判 `from`+`var` 并存 → 再 `IsMapping` → 不是映射就放过；`{"from": 1}` 是字面量；只有 `from`+`var` 并存 / `{var}` 未声明 / 只写 `default`+`as` 三种 400 |
| `null` 取值 | A~D 章只说「未命中」 | **`nil` 一律视为未命中**（A16）：别名表的值与 payload 里的**显式 `null`** 同等对待 → 有 `default` 用 `default`、否则 `param_missing`；混写里不得拼成空串 |
| `config.payload` 的对象性校验 | 未区分 kind | 只对**会保留它的 kind**（cron/manual）校验；webhook/github 丢弃该键、不 400（A17） |
| `PATCH {"config": null}` | 未写 | 视为本次不改 config（no-op），与「未带 `vars`/`payload` 从当前配置继承」配套（A18） |

## E.7 单趟替换 / 禁止递归的判定方式

判定「有没有引入模板引擎」只问一句：**替换结果还会被再扫描一次吗？**

- 实现：`RenderTemplate` 用 `FindAllStringSubmatchIndex` 扫**原始字符串**，边扫边拼到
  `strings.Builder`，替换进去的值**不再进入扫描**（也就不存在无限展开）。
- 语法层保证：`placeholderPattern = \{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}` 里没有运算符、
  括号、空格表达式的落脚点，`{{a + b}}` / `{{f(x)}}` / `{{#if x}}` 在语法上就不是占位符。
- 因此这些写法在**配置期**就会被闭合检查拦下，分两类消息：`{{`/`}}` 数量不等 →「占位符没有闭合
  （{{ 与 }} 数量不一致）」；数量相等但抹掉合法占位符后仍有 `{{`/`}}` 残留（`{{x + 1}}`、
  `{{f(x)}}`、`{{#if x}}`、`{{$x}}`）→「占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）」。
  只含 `}}` 而不含 `{{` 的字面量（如 `{"a":{"b":1}}`）**放行**——否则用户存不了普通字符串。
- 判定用例：payload `{"a": "{{b}}", "b": "B"}` 解析 `"{{a}}"` → 得到 **`"{{b}}"`**（不是 `"B"`）；
  混写 `"x={{a}}"` → `"x={{b}}"`。这条断言就是「不是模板引擎」的证据。

## E.8 不做模板引擎（一句话）

求值器一开就多出**模板注入**（`{{` 嵌套 / 递归展开）、**表达式 DoS**（超长 / 深嵌套）、
**语义漂移**（用户以为支持条件 / 运算 / 函数）三类攻击面，而收益只是省掉一次节点侧拼装——
节点本来就知道数据形状，拼接是它的活（I6）。
