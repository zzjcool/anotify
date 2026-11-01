# 冻结契约 · 能力产物格式（Artifact v1）

> 本文件是**冻结接口**。任何 worker 不得自行解释或扩展；发现矛盾必须上报协调者。
> 实现位置：`internal/artifact/`

## 1. 本质

一个能力产物 = **一个自包含的 YAML/JSON 文件**，包含：
- 能力的参数契约（schema）
- 它怎么跑（入口文件 + 文件内容 + 超时 + 环境变量）
- 它的出口名
- 在哪些机器上需要预装什么命令

**核心纪律**：

1. **产物是编译产物，不是手写源文件。** 唯一写者是控制台（表单 → 编译）。用户可以手改，
   但改了就成了「本地变体」，中枢永不覆盖（见 §7）。
2. **产物必须自包含**：脚本内容内联在文件里，不引用外部 blob/URL。
   这是「拷到别的机器直接能跑」的前提。
3. **执行方式是直接 exec，不经 shell**。入口文件必须有 shebang。
   （这是对旧 `sh -c` 的替换：没有 shell 就没有插值/拼接/引号问题。）

## 2. 线上形状（canonical JSON）

```json
{
  "apiVersion": "anotify/v1",
  "kind": "Capability",
  "metadata": { "name": "review.pr-diff", "version": 3 },
  "spec": {
    "displayName": "PR 代码审查",
    "params": { "type": "object", "required": ["repo","pr"], "properties": { ... } },
    "outputs": ["clean", "needs-fix"],
    "requires": ["gh"],
    "run": {
      "entrypoint": "run.sh",
      "files": { "run.sh": "#!/bin/bash\nset -euo pipefail\n..." },
      "timeoutSec": 900,
      "env": { "LOG_LEVEL": "info" }
    }
  }
}
```

Go 结构（`internal/artifact`，字段顺序即声明顺序）：

```go
type Artifact struct {
    APIVersion string   `json:"apiVersion"`
    Kind       string   `json:"kind"`
    Metadata   Metadata `json:"metadata"`
    Spec       Spec     `json:"spec"`
}
type Metadata struct {
    Name    string `json:"name"`
    Version int    `json:"version"`
}
type Spec struct {
    DisplayName string            `json:"displayName,omitempty"`
    Params      json.RawMessage   `json:"params,omitempty"`
    Outputs     []string          `json:"outputs"`
    Requires    []string          `json:"requires,omitempty"`
    Run         Run               `json:"run"`
}
type Run struct {
    Entrypoint string            `json:"entrypoint"`
    Files      map[string]string `json:"files"`
    TimeoutSec int               `json:"timeoutSec,omitempty"`
    Env        map[string]string `json:"env,omitempty"`
}
```

`Outputs` 为空时序列化成 `[]`（不能是 `null`，遵循 AGENTS.md 的空列表纪律）。

## 3. Digest

```
digest = "sha256:" + hex(sha256(canonicalJSON(Artifact)))
```

- `canonicalJSON` = `json.Marshal` 的字节（Go 的 map key 有序、struct 字段有序 → 确定性）
- digest **不包含在文件里**，始终由内容算出来。文件里没有 `digest` 字段。
- 因此：**手改内容 = digest 变了 = 本地变体**（自动检测，无需额外元数据）

## 4. 校验规则（`Parse` 必须全部通过，否则整份拒绝）

| 字段 | 规则 | 拒绝原因 |
|---|---|---|
| `apiVersion` | 必须等于 `anotify/v1` | `unsupported_api_version` |
| `kind` | 必须等于 `Capability` | `unsupported_kind` |
| `metadata.name` | `^[a-z0-9][a-z0-9._-]{0,62}$` | `invalid_name` |
| `metadata.version` | `>= 1` | `invalid_version` |
| `spec.run.entrypoint` | 非空，且必须是 `files` 的一个 key | `entrypoint_missing` |
| `spec.run.files` | 至少 1 个文件；每个 key 是相对路径，不得以 `/` 开头、不得含 `..` 段、不得含空段、长度 ≤ 255 | `invalid_file_path` |
| `spec.run.files` 内容 | 单文件 ≤ 256 KiB；全部文件合计 ≤ 1 MiB | `payload_too_large` |
| 入口文件内容 | 必须以 `#!` 开头（要有 shebang，因为直接 exec） | `entrypoint_no_shebang` |
| `spec.run.timeoutSec` | `0..86400`（0 = 不设本地超时，交给中枢租约） | `invalid_timeout` |
| `spec.run.env` key | 不得以 `ANOTIFY_` 开头（契约变量独占） | `reserved_env` |
| `spec.outputs` | 每项 `^[a-zA-Z0-9._-]{1,32}$`，去重后 ≤ 32 个 | `invalid_output` |
| `spec.requires` | 每项 `^[a-zA-Z0-9._+-]{1,64}$` | `invalid_requirement` |
| `spec.params` | 若存在必须是合法 JSON 对象（**只校验是 object**；子集 Schema 校验仍在派发期做，归 `internal/route`） | `invalid_params_schema` |

错误必须**全部列出**（`;` 分隔），不得只报第一条。

## 5. 执行契约（节点侧，冻结）

节点应用一份产物时：

```
~/.anotify/artifacts/<digest>/capability.yaml    ← 产物原文（YAML）
~/.anotify/artifacts/<digest>/files/<rel-path>   ← 展开的文件
```

跑一个任务时：

```
<data-dir>/work/<task_id>/                       ← 每次任务独立工作目录（默认 data-dir = ~/.anotify）
```

| 项 | 契约 |
|---|---|
| 工作目录 | 每个任务**独立**建目录，脚本 cwd 就是它，通过 `ANOTIFY_WORKDIR` 暴露 |
| 文件落盘 | `files` 按相对路径展开进 `ANOTIFY_WORKDIR`；entrypoint 及所有文件 chmod `0755` |
| 执行方式 | `exec <ANOTIFY_WORKDIR>/<entrypoint>`，**不经 shell** |
| 环境变量 | 父进程 env（剔除 `ANOTIFY_*`）+ `run.env`（剔除 `ANOTIFY_*`）+ 契约变量（见 §6） |
| 超时 | `run.timeoutSec` 优先，其次中枢 `max_duration_sec`，都没有则不设本地超时 |
| 资源 | 由节点启动参数约束（并发/CPU/内存/网络），**中枢推不动** |
| 清理 | 成功后删除工作目录；失败保留（默认 24h，可查） |
| 退出码 | `0` → 可回报 `out`；非 0 或超时 → 不回报 `out` |

## 6. 环境变量契约（冻结）

| 变量 | 内容 | 备注 |
|---|---|---|
| `ANOTIFY_TASK_ID` | 任务 ID | 已有 |
| `ANOTIFY_CAPABILITY` | `name@version` | 已有 |
| `ANOTIFY_PARAMS` | 完整参数 JSON | 已有，复杂结构用它 + `jq` |
| `ANOTIFY_PARAM_<NAME>` | 每个顶层参数一份 | 已有；非字符串值 JSON 序列化 |
| `ANOTIFY_WORKDIR` | 本次任务的独立工作目录 | **新增** |
| `ANOTIFY_TRACE_ID` | 链路 ID | **新增**（旧协议 assign 帧没这个字段，本期补） |
| `ANOTIFY_DIGEST` | 本次跑的产物 digest | **新增**（排障用） |
| `ANOTIFY_OUT_FILE` | 出口回报文件路径（`<workdir>/.anotify-out`） | **新增**，见 §6.1 |

**参数命名规范**：能力参数 key **一律 snake_case**。
理由：`paramEnvName` 只做「大写 + 非字母数字换下划线」，`prNumber` 会变成 `$ANOTIFY_PARAM_PRNUMBER`（不是 `PR_NUMBER`），很难猜。
控制台表单层强制 snake_case 校验；服务端 `POST /v1/capabilities` 对参数 key 做同样校验。

### 6.1 出口名（out）的回报通道

v1 的 `run.out` 是 node.yaml 里一个**固定**字符串，任务成功就回报它。这在 v2 里不够：
产物只有 `spec.outputs[]`（**集合**），而「这一次跑的是哪一个」是**运行期**才知道的。

约定（这是唯一的回报通道）：

| 脚本行为 | 节点行为 | result 帧 |
|---|---|---|
| 不写 `$ANOTIFY_OUT_FILE` | 不回报 | `out` 键缺席（分支终止） |
| 写一行、trim 后在 `spec.outputs` 里 | 回报它 | `"out": "<name>"` |
| 写一行、但不在 `spec.outputs` 里 | **整次执行判 failed** | `status=failed`，`error` 说明原因 |
| 文件读不出来（权限等） | 判 failed | 同上 |

为什么「写了错的」要判失败而不是静默忽略：出口是**封闭集合**（不变量 I2）。
静默吃掉一个拼错的出口名，症状会表现为「链路莫名断在这里」，而且没有任何痕迹。

示例：

```bash
#!/bin/bash
set -euo pipefail
cd "$ANOTIFY_WORKDIR"
if grep -q FIXME src/*; then
  printf 'needs-fix\n' > "$ANOTIFY_OUT_FILE"
else
  printf 'clean\n'    > "$ANOTIFY_OUT_FILE"
fi
```

## 7. 本地变体（Local Variant）

| 场景 | 行为 |
|---|---|
| 节点上缓存的 digest == 中枢desired digest | 托管态，中枢可覆盖 |
| 节点上缓存的 digest != 中枢desired digest | **本地变体**：状态 `local`，中枢永不覆盖，UI 标黄「已被本地修改」 |
| 节点上有中枢不知道的 digest | 同上，`local`，不可被部署覆盖 |
| 手动 `anotify-agent run ./x.yaml` | 不进中枢、不入缓存，纯本地一次性执行 |

这条规则是「不产生两个真相源」的全部机制：**每个「节点 × 能力」永远有唯一归属**。

## 8. 内置模板（`GET /v1/capability-templates`）

模板 = **代码生成器的输入**，不是执行器类型。v1 只有一种执行器（bash 脚本）。

```go
type Template struct {
    ID          string      `json:"id"`          // agent.claude
    DisplayName string      `json:"displayName"`
    Category    string      `json:"category"`    // script | agent
    Description string      `json:"description"`
    Fields      []Field     `json:"fields"`      // 表单字段
    Script      string      `json:"script"`      // Go 模板，渲染出 entrypoint 内容
    Requires    []string    `json:"requires"`    // 预检：机器上必须有
    Params      string      `json:"params"`      // 渲染进 params 的 JSON Schema 模板
    Outputs     []string    `json:"outputs"`
}
type Field struct {
    Key      string   `json:"key"`       // 需要是 snake_case
    Label    string   `json:"label"`
    Type     string   `json:"type"`      // string | text | select | number
    Required bool     `json:"required"`
    Default  string   `json:"default"`
    Options  []string `json:"options"`   // type=select 时
    Help     string   `json:"help"`
}
```

v1 必须有这 9 个模板：

| ID | Category | Requires | 说明 |
|---|---|---|---|
| `script.blank` | script | — | 空白 `#!/bin/bash` |
| `script.http` | script | `curl` | 按参数发 HTTP 请求 |
| `script.git` | script | `git` | clone / 操作仓库 |
| `script.test` | script | — | 跑测试命令 |
| `script.notify` | script | `curl` | 回调一个 webhook |
| `agent.claude` | agent | `claude` | `claude --print "$ANOTIFY_PARAM_PROMPT"` |
| `agent.codex` | agent | `codex` | `codex exec "$ANOTIFY_PARAM_PROMPT"` |
| `agent.pi` | agent | `pi` | `pi --print -p "$ANOTIFY_PARAM_PROMPT"` |
| `agent.custom` | agent | — | 用户填命令名 |

Agent 模板的表单字段固定为：`prompt`(text, required)、`model`(string)、`tools`(string)。
模板渲染出的脚本**只读展示**在控制台，用户可切「脚本模式」自由改。

**Agent 密钥不归中枢**：`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` 等留在**机器上**
（agent 启动时的 env），中枢只传 prompt。控制台不存任何 LLM 密钥。

## 9. 本期不做（明确）

- `runner: agent` 作为一等执行器类型（模板已够）
- 镜像构建 / 推 registry
- 控制台存密钥
- Monaco/CodeMirror 编辑器（`<textarea>` + 等宽 + 行号即可）
- 可视化拖拽编排（edges 表已经是可解释的编排）
- 产物签名（v1 靠 digest + 节点启动参数约束；签名留扩展点）
