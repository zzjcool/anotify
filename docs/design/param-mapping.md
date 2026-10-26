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
```

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
```

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
```

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
```

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
```

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
