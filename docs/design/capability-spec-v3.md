# Capability Spec v3 · 冻结契约

> 状态：**冻结**（2026-11-07）。实施中若要改本文，先改本文再改代码，禁止「代码这么写所以文档也这么写」。
> 权威性：本文取代 `docs/design/playbooks.md` §2（定义/产物分离）与 `api/openapi.yaml` 的 `CapabilityInput`。
> `docs/contracts/CONTRACT.md` §2.2/§2.8 是陈旧副本（写着 `-- +migration 13` + `org_id`），与 `schema.sql` 不符，**以本文和 `schema.sql` 为准**。

---

## 0. 一句话

**能力 = 一份不可卖的、自包含的规格。规格里既有契约，也有实现。**

不存在「定义」与「实现」两个物体。过去的 `capability_versions`（壳） + `artifacts`（实现）是本契约要消灭的形态。

## 1. 七个名词

| 名词 | 是什么 | 不是什么 |
|---|---|---|
| **Capability@version** | 一份不可变规格：入口参数、出口、脚本文件、超时、env、依赖命令 | 不是一个「壳」，实现不是它的一半 |
| **Source** | 事件从哪来（cron / webhook / github / manual） | 不是能力（它不执行） |
| **Edge** | 端口 → 能力 + 参数映射 | 不描述能力内容 |
| **Node** | 执行者（`agent` 或 `builtin`） | 不「拥有」能力 |
| **Deployment** | 期望：某节点该持有哪个 digest | 不是「安装实现」 |
| **Event** | 发生了一件事 | — |
| **Task** | 一次执行（谁、什么参数、哪份 digest） | — |

## 2. 六条无例外规则（可机检的才是规则）

- **R1** 实现永远跑在节点上。中枢只是一台 `builtin` 节点，它只跑编译进去的白名单 handler。
- **R2** 一个能力永远有实现。最简实现是 `handler: "noop"`（合法、可部署、无副作用、不发 emit）。
- **R3** 版本正文不可变。**digest 是内容的身份，`name@version` 是指向它的指针。**
- **R4** 中枢不得 `os/exec`（I1 不变）。
- **R5** 端口是封闭集合：`spec.outputs` 是唯一真相，边只能引用已声明的端口。
  > 由此删除 #383 的并集妥协（「有效端口 = outputs ∪ 边上出现过的 port」）。
- **R6** 一个字段必须能回答「这描述的是这个能力是什么吗」，否则它不属于能力。

## 3. 规格形状（wire，YAML / JSON 等价）

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review                 # ^[a-z0-9][a-z0-9._-]{0,62}$
  version: 3                   # >= 1
  displayName: 代码审查         # 纯展示；**不参与 digest**（改它不产生新版本、不漂移）
spec:
  params:    { type: object, properties: { repo: { type: string } } }   # JSON Schema
  outputs:   [needs-fix, approved]     # 封闭集合；空必须序列化成 []
  requires:  [git, jq]                  # 节点必须预装（节点侧 LookPath 探测）
  lifecycle: { retryable: true, maxAttempts: 2, idempotent: true, resultTtlSec: 86400, expiresInSec: 900 }
  sticky:    worktree                   # 粘性路由键类型（'' | worktree | …）

  # ── 实现：下面两组**恰好给一组**，由 Validate 强制 ──
  run:                                  # 脚本形式：任意 agent 节点执行
    entrypoint: run.sh                  # 必须是 files 的 key，内容以 #! 开头
    files:
      run.sh: |
        #!/usr/bin/env bash
        ...
    timeoutSec: 900                     # 节点本地超时；0 = 只受中枢租约约束
    env:       { GH_TOKEN: "" }         # 键不得以 ANOTIFY_ 开头
    workDir:   ~                        # 绝对路径或 ~/…；~ 只在节点展开
  # 或者（handler 形式**不得**带 run 的任何字段）
  handler: notify-phone                 # 内置执行器白名单；只有 builtin 节点能认领
```

> 字段序（决定 canonical JSON / digest）：`params, outputs, requires, lifecycle, sticky, run, handler`。
> `run` **不拍平**——它保持嵌套结构，上面的「拍平顺序」只是给人读的语义顺序。

### 3.1 实现的两种形式（唯一允许的「种类」维度）

| 形式 | 谁来跑 | 用于 |
|---|---|---|
| `files` + `entrypoint` | 节点（任意 `agent` 节点） | 99% 的能力 |
| `handler` | 中枢进程内的 `builtin` 节点（白名单，编译进去） | 平台自带动作（`notify-phone`）、迁移占位（`noop`） |

**校验规则（`artifact.Validate`）**：
1. `files`+`entrypoint` 与 `handler` **恰好给一组**，两组都给 / 都不给 → `impl_ambiguous` / `impl_missing`。
2. `handler` 不在白名单 → `unknown_handler`（白名单只有 `notify-phone`、`noop`）。
3. `handler` 形式不得带 `files` / `entrypoint` / `entrypoint` 相关字段。

### 3.2 digest

```
digest = "sha256:" + hex(sha256(canonical{metadata.name, metadata.version, spec}))
```

- **`metadata.displayName` 不进 digest**（这是当前漂移 bug 的根：PATCH 改表不改产物）。
- **`version` 进 digest**（试行过「只由内容决定」，已回退）：若同内容的不同版本折叠成一行，边里写的 `to_ref = x@2` 会悬空。显式版本必须能落地。
- **`name` 进 digest**：跨名字共享实现本轮刻意不做（§9）。
- **本条会改变所有既有 digest** → 升级后所有部署变 stale，需重推一次（P1-b2 负责，一次性）。
- 已知代价：不带 version 重复 POST 原本会各自长出一个版本（能力**没有 DELETE 端点**）——**已按 §10-B 修掉**：省略版本时按 `(ns, name, spec)` 内容寻址复用已有行，显式版本永不折叠。

## 4. 存储：一张表

```sql
-- +migration 8 capability_spec
ALTER TABLE capability_versions ADD COLUMN spec TEXT NOT NULL DEFAULT '';
```

改后 `capability_versions` 的语义：

| 列 | 语义 |
|---|---|
| `id / namespace_id / name / version / created_at` | 身份与归属 |
| `display_name` | 展示名（真相在这一列，不在 spec 里） |
| `params_schema` / `outputs` / `lifecycle` / `sticky` | **派生只读列**：写入时由 `spec` 算出，供路由校验与列表查询，禁止单独修改 |
| `balance` | P1-b 起恒写 `'{}'`（死字段，P3 删除） |
| `content_hash` | **重定义为 ≡ digest**（吃 name + version + 完整 spec）。`capHashInput` 改为整份 spec |
| `spec` | 规格正文 canonical JSON（**不权威，见下**） |

> `spec` 列与派生列的关系是「一个写者」：`spec` 是输入，派生列是输出。任何直接改派生列的路径都是 bug。
>
> 为什么不把 `spec` 当唯一真相：路由校验（`fromPort ∈ outputs`、`literals` 键 ∈ `params.properties`）是热路径，不该每次解 1 MiB JSON。**派生列是缓存，不是第二真相**——两者由同一个事务写入，不允许不同步。
>
> 包依赖约束：`internal/store` 已经 import `internal/artifact`（`builtin.go`），所以 `artifact` 不能反向 import store。因此 `artifact.Spec.Lifecycle` 的类型是 **`json.RawMessage`**（不透明透传），其内容校验留在 store 层。

`artifacts` 表在 P1 保留为**写穿镜像**（同事务由 `capability_versions.spec` 派生，保证 `deployments.artifact_id` 外键继续工作），**P2 删除**。

## 5. 端点

> **请求形状的切换推迟到 P4**。P1-b 只改内部（服务端把扁平字段组装成 spec），HTTP 请求体仍是旧的扁平形状——
> 这样 P1-b 的每一跳都能保持 `make ci` + e2e 绿。一次性切形状等于把前端与 4 个 e2e 套件一起拖进来，那不是 P1 的职责。

| 方法 | 路径 | P1-b | P4 |
|---|---|---|---|
| POST | `/v1/capabilities` | 请求体**不变**（`paramsSchema`/`outputs`/`run`/`requires`/`sticky`/`lifecycle`）。服务端组装成 spec。**缺实现 → 400 `impl_missing`**（这是唯一的行为收紧） | 请求体改 `{namespaceId?, name, version?, displayName?, spec, force?, links?}`，删扁平字段 |
| GET | `/v1/capabilities` | 列表项新增 `spec`（**剔除 `files` 正文**，换成 `files: {count, bytes}`）；`run` 旧投影保留 | 删旧投影 |
| PATCH | `/v1/capabilities` | 仍只改 `displayName`（现在它不在 spec 里，天然不漂移） | 同 |
| GET | `/v1/capabilities/{id}/artifact` | **它同时是详情端点**：返回整份产物（metadata + 完整 `spec`，含 `files` 与 `env`）。仓库里从来没有 `GET /v1/capabilities/{id}`，**不新增**（`check_openapi.py` 机检 spec 与 mux 一致，凭空加 path 会红；也没必要——`/artifact` 已经是详情页展示脚本用的那个） | 可加 `/spec` 别名 |
| POST | `/v1/deployments` | **handler 形式的能力不可部署** → 400 `builtin_not_deployable`（否则会给节点推一份没有 files 的产物） | 同 |
| — | `/v1/artifacts` 系列 | 保留（写穿镜像还在） | P2 删除 |
| — | openapi | **补齐声明**：`run`(5 字段) / `requires` / `spec` 进 `CapabilityInput` 与响应 schema（这是长期缺口的修复，纯增量） | 改形状时再动 |

## 6. 迁移与回填（`migration 8` 的 hooks）

| 现有状态 | 新 `spec` |
|---|---|
| 有 artifact 的能力 | `spec` = 该 artifact 的 `spec` 字段（去掉 `displayName`） |
| `notify.phone@1` | `{handler: "notify-phone", params: <原 schema>, outputs: [], lifecycle: {}}` |
| `script.echo@1` | 已有 artifact，原样迁 |
| **其它「有能力行、零 artifact」**（预案装的、devseed 的、手建的） | `{handler: "noop", params: <原 schema>, outputs: <原 outputs>, lifecycle: <原>}`，并在日志与 `make contract` 输出里列出清单 |
| `devseed -demo` 的 3 个演示能力 | 改用真实脚本（`demo.go` 的假执行器删除） |

**禁止**：伪造用户脚本、给空壳编造 `exit 0` 正文。占位用 `handler: noop`（平台认领、不假装是用户代码、列表页可标「未实现」）。

### 6.1 两个必须绕开的獠坑

1. **`artifacts` 有 `UNIQUE(namespace_id, digest)`，且它是「可部署」的唯一凭据**。所以回填 `notify.phone` 时**不能**给它造 artifacts 行——那会让它出现在部署面板、推送一份没有 files 的产物给节点。正确做法：`BuiltinCapability` 新增 `Handler string` 字段，播种时只写 `capability_versions.spec`（`handler: notify-phone`），`Artifact` 保持 nil，artifacts 写穿逻辑跳过 handler 形式。
2. **中枢执行的运行时认领仍然是 `CentralCapabilityNames()`（一张硬编码名字表，`internal/store/builtin.go:435`）**，不是 spec。P1-b1 **不要**改派发逻辑，但要加一条一致性测试：`BuiltinCapabilities` 里每个 `Central: true` 的条目，其 spec 必须有 `handler` 且在 `artifact.KnownHandlers()` 里。运行时的切换是 P2。

### 6.2 回填的幂等要求

`migrate.go` 的 hook 会被重跑（旧库升级、新库建表）。要求：
- 只处理 `spec = ''` 的行，已填的不动；
- 同一输入重跑得到同一 `content_hash`；
- 回填后必须能通过 `artifact.Validate`（否则下次部署/派发时才发现，太晚）；
- 写入一份「回填清单」到日志（哪些能力拿到了 noop），供人工补实现。

## 7. 分期（每期独立可验收）

| 期 | 内容 | 依赖 | 协议影响 |
|---|---|---|---|
| **P1-a** | `internal/artifact`：Spec 形状（handler / displayName 移到 Metadata / retry 入 spec）、digest 规则、Validate 三条新规则、JSON Schema、单测 | 无 | 无（包内） |
| **P1-b1** | `migration 8` + store（`spec` 列、`content_hash`≡digest、派生列由 spec 算、`artifacts` 写穿、builtin/playbook/devseed 造 spec）+ 服务端构造点的机械适配 | P1-a | 无 |
| **P1-b2** | 服务端行为（缺实现 400、handler 不可部署、响应加 `spec`、列表剥 `files`）+ openapi 补 `run`/`requires`/`spec` + e2e 夹具补 run | P1-b1 | 无 |
| **P2** | 删 `artifacts` 表；`deployments` 改 `(capability_id, digest)` 引用；运行时从 `spec.handler` 派生中枢执行能力（取代 `CentralCapabilityNames()` 的硬编码名字表） | P1-b2 | 无 |
| **P3** | 删 `balance`（死字段：只放行 `priority`，语义上只有一个合法值） | P1-b1 | 无 |
| **P4** | 前端 + 请求形状切到 `spec`：能力页多文件 spec 编辑器、env 可编辑、删「还没有产物」分支、「部署」按钮按实现形式分叉 | P1-b2 | 请求体变更 |
| **P5** | 预案：manifest 里每个能力 = 完整 spec 或 `ref: name@ver` | P1-b1 | 无 |
| **P6** | 契约门禁：`check_openapi.py` 增加「handler struct 的 json tag ⊆ schema properties」机检 | P1-b2 | 无 |

## 8. 验收（每期都跑）

```bash
make fe                                    # ⚠️ 必做：internal/server/dist 是 gitignore 产物，
                                           #    run_all.sh 只在缺失/为空时才重建。dist 陈旧会让前端
                                           #    套件出现**假红**（断言数也会变少，例如 309 而不是 639）。
gofmt -l $(git ls-files '*.go')
go test ./internal/... ./cmd/agent/...     # 单元
make contract                              # A2/A4/A5/A6/A7 + openapi↔mux
make ci                                    # PR 门禁（含 build / vet / 基准冒烟）
make e2e-one S=deploy                      # 部署语义
make e2e-one S=api_contract                # 端点契约
make e2e-one S=real_backend                # 真后端全链
```

**新功能的门禁要求**：任何一次「能力创建 → 部署 → 派发」的行为变化，必须在 `scripts/e2e/suites/deploy.mjs` 或 `api_contract.mjs` 里有一条断言，不能只靠单测。

## 9. 本文刻意不做的事

- 跨名字共享实现（digest 含 name）—— 无真实需求。
- 「能力市场 / 第三方实现包签名」—— 与 I1 正面冲突，无需求。
- 中枢执行用户任意代码 —— 永不。
- `impl.kind` 作为独立枚举字段 —— 用「files/handler 二选一」表达，新增 0 个概念。

## 10. 已决：同内容重放（§10-B）

**采纳 B**。`version` 省略（`<= 0`）= 「内容寻址的创建」：

1. 用占位版本 1 归一正文（version 只进 digest、不进正文）；
2. 按 `(ns, name, spec)` 字节相等查已有行 → 命中就**复用那一行**（只额外更新 `display_name`）；
3. 未命中 → 在**事务内**算 `MAX(version)+1`（不再由服务端预猜：预猜与插入之间有个窗口，并发写者会撞同一个 next）。

**显式给了 version 就永不折叠**：`x@1` 与 `x@2` 各占一行，哪怕内容相同。这条不能让步——边里写的是 `name@version`，折叠会让 `to_ref` 悬空。

落地位置：`internal/store/capability.go` 的 `createCapabilityTx`（② 版本分配）、`getCapabilityBySpecOn`、`nextCapabilityVersionOn`、`foldIntoExistingOn`；测试 `TestCapabilityCreateContentAddressed`。

被排掉的选项：
- **A 保持现状**（省略版本 = 永远开新版本）：重试/双击会留下重复行，而能力不可删。
- **C digest 去掉 version**：显式 `x@2` 会被折成 `x@1`，边的 `to_ref` 悬空。已试过并回退，playbook 四个用例当场变红。

## 11. P2 实施细化（删 artifacts 表 + 运行时切到 spec.handler）

P1 期间 `artifacts` 是**写穿镜像**：真相在 `capability_versions.spec`，镜像只为保住
`deployments.artifact_id` 外键。P2 把镜像与双重依赖一起拆掉。

### 11.1 表

```sql
-- +migration 10 deployments_reference_capability
-- SQLite 改外键只能重建表：新建 → 拷 → 删旧 → 改名（migrate.go 的 hook，必须幂等）。
CREATE TABLE deployments_new (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    digest        TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'pending',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (node_id, capability_id, digest)
);
-- 拷贝：artifact_id → artifacts.capability_id（旧行一条都不能丢）
-- 拷完 DROP TABLE deployments; ALTER TABLE deployments_new RENAME TO deployments;
-- 再建索引 idx_deploy_node(node_id) / idx_deploy_cap(capability_id)
DROP TABLE artifacts;
```

### 11.2 读路径（全部改读 `capability_versions`）

| 旧 | 新 |
|---|---|
| `LatestArtifact(ns, capID)` | `GetCapabilityByID` → `(digest, spec)`；产物 = `Artifact()` |
| `GetArtifactByDigest(ns, digest)` | 查 `capability_versions WHERE namespace_id=? AND content_hash = ?`（digest 去掉 `sha256:` 前缀） |
| `ListArtifacts` | 按 `capability_id` 查版本行；或直接删端点 |
| `CreateDeployment(artifactID)` | `CreateDeployment(capabilityID, digest)` |
| `resolveArtifact` | 从能力行构建 `artifact.Artifact`（帧形状**不变**：`WsDeployFrame.Artifact` 仍是整份 canonical JSON） |

**协议零改动**：deploy 帧内容不变，只是它的来源从 artifacts 行变成能力行。
`GET /v1/artifacts` 与 `GET /v1/capabilities/{id}/artifact`：前者的 `spec` 语义变成
「该能力的当前产物」，后者保持；若实现成本高，`/v1/artifacts` 可以**只保留 GET 一版并标注 deprecated**。

### 11.3 运行时：中枢执行由 `spec.handler` 认领

现在 `CentralCapabilityNames()` 是一张硬编码名字表（`builtin.go:435`），派发与
`ClaimCentralTasks` 都读它。P2 换成**数据驱动**：

```
SELECT DISTINCT name FROM capability_versions WHERE json_extract(spec,'$.handler') IS NOT NULL
```

- 允许进程内缓存 + 写路径失效；缓存失效的 bug 比每次查库慢更难查，但**不要**让它成为第二真相。
- `BuiltinCapabilities` 里的 `Central` 布尔**删掉**：它的唯一作用是喂那张名字表。
  `Handler` 字段是唯一来源。
- 白名单仍然只有 `artifact.KnownHandlers()`；播种时校验（已有测试
  `TestBuiltinHandlersAreKnownHandlers`）。
- 派发对 handler 能力的「跳过找节点」逻辑不变，只是判据从名字表变成 `spec.handler != ""`。

### 11.4 验收（除 §8 那套之外）

- `go test ./internal/store/... ./internal/route/... ./internal/server/... ./internal/ws/...`
- `make e2e-one S=deploy` / `S=ws_protocol` / `S=real_backend` / `S=live_agent`
- 手工确认：`notify.phone` 的推送链路仍能跑（`make demo-chain` 或 `live_agent` 套件）
- 确认 `artifacts` 表已不存在（`schema_baseline_test.go` 这类断言要跟着更新）

## 12. 预案 manifest v2（P5）

预案就是**一份可导入的配置**：一组能力 + 它们之间的边 + 触发器。P5 把它从「名字清单」
升级成能表达完整 spec 的清单。

### 12.1 形状

```jsonc
{
  "capabilities": [
    // ① 引用已有定义（含内置）：不新建任何行
    { "ref": "review@2" },
    { "ref": "notify.phone" },            // 裸名字 = 跟随 latest

    // ② 内联完整产物：装出来就是一份真能跑的能力
    { "spec": {
        "apiVersion": "anotify/v1",
        "kind": "Capability",
        "metadata": { "name": "review", "version": 1, "displayName": "审查" },
        "spec": { "params": {...}, "outputs": ["needs-fix"], "run": { "entrypoint": "run.sh",
                  "files": { "run.sh": "#!/usr/bin/env bash\n…" }, "timeoutSec": 900 } }
    } }
  ],

  "edges": [
    { "from": "review", "fromPort": "needs-fix", "to": "apply-fix@1",
      "literals": { "mode": "safe" },   // 常量覆盖（语义同 edges.literals）
      "priority": 0 }                    // 同端口候选的优先级
  ],

  "triggers": [
    { "name": "pr-review-github",        // 可选；省略时沿用 <slug>-<kind>-<idx>
      "kind": "github",
      "config": { "event": "pull_request", "resultSchema": {...} },
      "enabled": true,
      "targets": [                       // 入口边；mapping 只在入口边允许
        { "to": "review", "mapping": { "repo": "{{repository.full_name}}" } }
      ] }
  ]
}
```

### 12.2 三条硬规则

1. **`ref` 与 `spec` 恰好给一个**。两个都给 / 都不给 → 400。
2. **内联 `spec` 只允许两种实现形式**：脚本形式（`run.files`+`entrypoint`），或 `handler: "noop"`。
   **禁止 manifest 携带其他 handler** —— 否则一份配置文件就能把任意能力挂到中枢执行，
   I1 直接破掉。要「链尾发推送」请 `ref: notify.phone`。
3. **装出来的能力必须能通过 `artifact.Validate`**：install 在落库前逐条校验，失败带下标 400，
   引用不到的名字（`ref` / 边的端点）带下标 400 —— 不允许「装了一半」。

### 12.3 兼容与幂等

- 旧 manifest（`{name, version, outputs, paramsSchema, runner}`）**继续接受**，
  等价于一条 `handler: noop` 的内联 spec（与 P1-b1 的回填口径一致）。
- 已存在的 `name@version`：内容相同 → 归入 `skipped`；内容不同 → **skip + note**（append-only，
  绝不覆盖用户改过的实现）。
- install 仍然只做「能力 → 边 → 源」，单事务，冲突只 skip。
