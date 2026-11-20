# pr-triage-notify · 一个可安装的 playbook 示例

webhook 事件进来 → 按改动量判定 → 高危走告警，常规只留证。装一次得到四行资源：
三个能力 + 一个源，互相连通。

```
POST /v1/hooks/{token}   (源 checkout-push，webhook)
        │  $(payload.repo / commit / changedLines)
        ▼
  checkout-triage ──► checkout-page   （改动 ≥ 阈值：POST 到 values.alertUrl）
        └──────────► checkout-audit  （否则：只写证据文件）
```

## 这个目录就是一个 playbook 包

| 文件 | 作用 |
| --- | --- |
| `playbook.yaml` | 唯一入口：包身份 + `spec.values`（参数声明）+ 引用清单 |
| `capabilities/triage.yaml` | 链头，判定并把 `next` 指向两个下游 |
| `capabilities/page.yaml` | 高危终端：`curl` 打告警端点 + 落 `verdict.json` |
| `capabilities/audit.yaml` | 常规终端：把判定写进 `audit.log` |
| `sources/push.yaml` | 入口：webhook 源，`targets` 把载荷映射成 triage 的参数 |
| `values.demo.json` | 本次安装用的 values（下面的命令和测试都读它，只有一份） |

放在仓库根 `examples/` 下、而不是 `docs/` 下：`docs/` 里全是给人读的散文（示例一律是
markdown 围栏块，目录下没有一个 `.yaml`），这里的文件是要被 `POST` 上去、被服务端解析的
**可执行产物**。两者混在一起，"文档里的示例"和"装得进去的包"就分不清了。

包内只有 `.yaml` 必须被 `spec.capabilities` / `spec.sources` 引用到；`README.md` 与
`values.demo.json` 随包上传也无害（解析时忽略），所以下面的打包命令把整个目录打进去就行。

## 每个字段的依据（都不是自创的）

| 写法 | 依据 |
| --- | --- |
| 包 = 一个目录 + `playbook.yaml` 入口 + 能力/源各一个文件 | `internal/playbook/playbook.go:37` |
| 入口只允许 `apiVersion/kind/metadata/spec`，`metadata` 白名单和 `spec` 白名单 | `internal/playbook/parse.go:101`、`internal/playbook/parse.go:115`、`internal/playbook/parse.go:169` |
| `spec.values` 是 JSON Schema，支持 `type/required/properties/default/enum/pattern/minimum/...` | `internal/schema/schema.go:18`、`internal/playbook/values.go:165` |
| `{{values.点路径}}` 是唯一模板形态，且只替换值不替换键 | `internal/playbook/render.go:285` |
| 整个值恰好一个引用 → 保留原类型；掺常量 → 字符串拼接 | `internal/playbook/render.go:335` |
| `spec.params/next/requires/run/lifecycle/concurrency`（六段，多一个就拒） | `internal/artifact/artifact.go:183` |
| `run.files` 正文以 `#!` 开头、`entrypoint` 必须在 `files` 里 | `internal/artifact/artifact.go:774` |
| `next` 是纯名字列表（`name` 或 `name@version`），没有端口 | `api/resource.yaml:1719` |
| 源 `spec.kind/enabled/config/targets`，webhook 的 config 只认 `token/secret/payloadSchema/sample` | `internal/sourcespec/sourcespec.go:75`、`internal/sourcespec/config.go:17` |
| `targets.params` 里的 `$(payload.x)` 在触发期求值（与 `{{values.*}}` 是两套定界符） | `internal/sourcespec/template.go:212` |
| 脚本里的参数是 `ANOTIFY_PARAM_<大写>`、契约变量 `ANOTIFY_ARTIFACT_DIR` 等 | `internal/protocol/env.go:13`、`cmd/anode/internal/execenv/execenv.go:351` |
| `anode emit <下游> --data '<json 对象>'` | `cmd/anode/emit.go:34` |
| 创建/安装的 HTTP 形状 | `api/resource.yaml:458`、`api/resource.yaml:608` |

## 前置条件

1. **一个跑着的服务端**（`make dev`，或你自己的部署）。
2. **一个命名空间**：空库注册的第一个用户自带 `default`，不用单独建。
3. **一台节点**（可选但要看效果就必须有）：`cmd/anode`，节点上要有 `anode`（emit 用）
   和 `curl`（page 能力用）。没有节点时安装照样成功、事件照样接收，但任务会停在
   `pending`，能力的 `NodesAvailable` 条件不是 `Valid`（`NoMatchingNode`）。

## 方式一：控制台（最快，先走一遍）

不写命令也能装 —— 控制台走的是同一套端点：

1. `playbooks.html` → 「导入」→ 选**目录**上传 `examples/playbooks/pr-triage-notify`。
   顶层目录名会被剥掉，路径按包根算；包名会从 `playbook.yaml` 的 `metadata.name` 自动填好。
   提交后出现「打开」链接（同名再传会 409 `slug_taken`，并给出去详情页的入口）。
2. 打开详情页：`spec.values` 已经被渲染成表单，填 `project` 与 `alertUrl`，其余留空（= 用 schema 默认值）。
3. 依次点「渲染」→「试运行」→「安装」：第一个只回渲染后的文档，第二个是 `dryRun: true`（0 写入），
   最后一个才落库。回执按 `created / skipped / upgraded / external / pending / notes` 分区展示。
4. 把 `project` 改成另一个值再点一次「安装」，就是第二套并存的链。

下面的 curl 是同一件事的可脚本版本，无人值守环境用它。

## 方式二：curl 五步

下面的 `NS=default`；换成别的命名空间就改这个变量。

### ① 拿一个会话 Cookie

playbook 的全部端点只认会话 Cookie（不是 Bearer API key）。

```bash
B=http://localhost:8080
NS=default

curl -sc /tmp/anotify.jar -X POST "$B/v1/auth/password/register" \
  -H 'content-type: application/json' \
  -d '{"username":"demo","password":"demo-only-change-me"}'
```

已有账号就换成 `.../password/login`（body 相同）。成功是 200 + `Set-Cookie`。

### ② 把目录打成请求体

`spec.files` 就是「相对路径 → 正文」的映射，与磁盘上的目录一一对应：

```bash
python3 - <<'PY' > /tmp/playbook-body.json
import json, pathlib
root = pathlib.Path('examples/playbooks/pr-triage-notify')
files = {p.relative_to(root).as_posix(): p.read_text()
         for p in root.rglob('*') if p.is_file()}
print(json.dumps({
    "apiVersion": "anotify/v1",
    "kind": "Playbook",
    # 这里的 name 必须等于包内 playbook.yaml 的 metadata.name，也是 URL 里的 {slug}
    "metadata": {"name": "pr-triage-notify"},
    "spec": {"files": files},
}, ensure_ascii=False))
PY
```

### ③ 先创建包，再 dryRun 预览

创建只落清单、不安装；dryRun 走与真装完全相同的渲染与计划，但整包 0 写入。

```bash
curl -sb /tmp/anotify.jar -X POST "$B/v1/namespaces/$NS/playbooks" \
  -H 'content-type: application/json' --data @/tmp/playbook-body.json

curl -sb /tmp/anotify.jar -X POST "$B/v1/namespaces/$NS/playbooks/pr-triage-notify/install" \
  -H 'content-type: application/json' \
  -d "{\"dryRun\": true, \"values\": $(cat examples/playbooks/pr-triage-notify/values.demo.json)}"
```

dryRun 的预期回执（关键字段）：

```json
{
  "kind": "InstallReceipt",
  "playbook": { "name": "pr-triage-notify", "version": 1 },
  "install": { "id": "ins_…", "at": "2026-…", "dryRun": true },
  "namespace": "default",
  "values": { "project": "checkout", "env": "staging",
              "alertUrl": "https://alerts.example.com/anotify-demo", "threshold": 200 },
  "valuesDigest": "sha256:…",
  "created": { "capabilities": ["checkout-triage@1", "checkout-page@1", "checkout-audit@1"],
               "sources": ["checkout-push"] },
  "skipped": { "capabilities": [], "sources": [] },
  "upgraded": [], "unapplied": [], "external": [], "pending": [], "notes": []
}
```

`values` 里 `env: staging` 是**没传过**的值 —— 它来自 `spec.values` 的 `default`。
`notes` 是空的：说明包内每个名字都解析得到、也没有装上去接不上链的能力。

### ④ 真装

```bash
curl -sb /tmp/anotify.jar -X POST "$B/v1/namespaces/$NS/playbooks/pr-triage-notify/install" \
  -H 'content-type: application/json' \
  -d "{\"values\": $(cat examples/playbooks/pr-triage-notify/values.demo.json)}"
```

回执与 dryRun 相同，只有 `install.dryRun` 变 `false`、`install.id` 换一个新的。
**再装一次**（同 values）仍然 200，但 `created` 全空、`skipped` 是上次那四个名字 ——
安装是幂等的、只做加法。

### ⑤ 触发一次，看链跑起来

```bash
HOOK=$(curl -sb /tmp/anotify.jar "$B/v1/namespaces/$NS/sources/checkout-push" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["hookUrl"])')

curl -s -X POST "$HOOK" -H 'content-type: application/json' \
  -d '{"repo":"checkout","commit":"9f2c1ab","changedLines":240}'
```

投递体就是 payload 本身（没配 `secret` 时不校验签名）。回执是 `SourceReceipt`：
`{eventId, traceId, duplicate, tasks:[{taskId, capability, version}], pruned:[]}`。

`changedLines: 240 ≥ threshold: 200`，所以：

- `tasks[]` 里出现 `checkout-triage`；
- triage 执行后 emit `checkout-page`（而不是 audit），page 把告警 POST 到
  `values.alertUrl` —— 想立刻看到效果，就把它换成一个你能收的地址（例如
  `https://webhook.site/…`）后重新装；
- `page` 落了 `verdict.json`，任务详情里能看到 `artifacts: [{path: "artifacts/verdict.json", …}]`。

把 `changedLines` 改成 `12` 再来一次：这次走 `checkout-audit`，只有 `audit.log`，
没有告警。

### 观察点

| 想看什么 | 怎么看 |
| --- | --- |
| 一条链的全景 | `trace.html`（或 `GET /v1/namespaces/{ns}/traces/{traceId}`） |
| 单个任务的状态/日志/产物 | `message.html?id=<taskId>`（或 `GET …/tasks/{taskId}`） |
| 这次到底派给了谁 | `GET …/tasks/{taskId}/emissions`（`taskId: null` = 被剪枝，带 `pruned.reason`） |
| 能力的就绪度 | `GET …/capabilities/checkout-triage` → `status.conditions` |

## 换参数 = 第二套链

`values.project` 决定全部名字，所以把它改成 `payments` 再装一次，就并存出
`payments-triage / payments-page / payments-audit / payments-push`，两套互不影响：

```bash
curl -sb /tmp/anotify.jar -X POST "$B/v1/namespaces/$NS/playbooks/pr-triage-notify/install" \
  -H 'content-type: application/json' \
  -d '{"values": {"project": "payments", "alertUrl": "https://alerts.example.com/payments"}}'
```

注意这次只给了两个必填项（`env` 用默认 `staging`、`threshold` 用默认 `200`）。
`values` 里少给必填项会 400 `values_missing`，拼错键名会 400 `values_invalid`。

## 几条必须知道的边界

- **包体可以换，装出来的东西不会跟着变**：PUT 是整体替换包体（要带 `metadata.resourceVersion`），
  但安装只做加法 —— 已经建出来的能力/源不会被改写，要让改动生效就在 PUT 之后再装一次
  （正文变了会作为新版本落库），或者换个 slug 建新包。同名重复 POST 是 409 `slug_taken`。
- **没有端口**：v4 的分支只能写在脚本里（本示例用一个 `if` 决定 emit 谁）。下游也拿不到
  "从哪个口来的"，只有一个 `parentTaskId`。
- **没有 CLI**：创建 / 渲染 / 安装只有 HTTP 面，仓库里没有 `anotify playbook` 之类的子命令。
- **改这里要连带跑测试**：`go test ./internal/playbook` 里有
  `TestExamplePlaybooksInstallable`，它读的就是这个目录（`Parse → Render → Plan`）。
  字段拼错、`next` 悬空、`values.demo.json` 少给必填项，它都会失败 —— 不用手点一遍。
- 示例里的 `alertUrl` 指向 `alerts.example.com`，只是个占位；真正发告警前换成你自己的端点。
