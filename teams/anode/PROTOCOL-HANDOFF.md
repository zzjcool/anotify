# PROTOCOL-HANDOFF · V4-ANODE N1 共享协议冻结

- 任务：V4-ANODE-N1（团队契约 `.pi-legion/contract.md` rev1/rev1a；父权威 `.pi-orchestrator/v4/CONTRACT.md` rev2 §7）
- 状态：**已冻结，交父转 T1**（按 CONTRACT rev2 §7：N1 交快照后 T1 再接 G2）
- 唯一作者：T3/N1 worker；`internal/protocol/**` 从此冻结，后续缺口先报父协调，不得各自改字段
- 支持快照：父授权纯支持 commit **f7e9a6c**（来源 T1 `ba21a89` 的 protocol/artifact，非 T3 原创）；`internal/artifact/**` 本任务只读、未改
- 产品提交：**caecd9d** `feat(protocol): 冻结 N1 共享协议（env 常量/机器配置 hash/部署两帧）`
- 纯 protocol 补丁：**`teams/anode/protocol-only.patch`**（`git diff f7e9a6c caecd9d -- internal/protocol`；可在 f7e9a6c 上 `git apply`）

改动文件（全部在 `internal/protocol/`）：

| 文件 | 变更 |
|---|---|
| `env.go` | 新增：4 个环境常量 + `ParamEnvName` |
| `env_test.go` | 新增：大写/数字/符号/camel/碰撞具名测试 |
| `hash.go` | 新增：`MachineConfigHash` + canonical JSON |
| `hash_test.go` | 新增：golden/归一/排序/排除/负值/格式测试 |
| `deploy_frames.go` | 新增：`capability.deploy`/`capability.deployed` 两帧 |
| `deploy_frames_test.go` | 新增：帧名/方向/wire 键/状态/refs 一致测试 |
| `codec.go` | Decode 两个 case + 上行/下行方向表各 +1 |
| `doc.go` | 帧集合说明 19 → 21 |
| `protocol_test.go` | 帧名快照 19→21；round-trip fixtures 增两帧 |

## 1. 任务环境契约变量与参数归一（env.go）

```go
const (
    EnvEmitEndpoint = "ANOTIFY_EMIT_ENDPOINT" // 协议+地址+端口，脚本拼 /emit
    EnvTaskToken    = "ANOTIFY_TASK_TOKEN"    // Authorization: Bearer <token>
    EnvTaskID       = "ANOTIFY_TASK_ID"       // 只读，日志关联
    EnvParamPrefix  = "ANOTIFY_PARAM_"        // 参数变量前缀
)

func ParamEnvName(name string) string
```

`ParamEnvName` 规则（与 artifact 私有 `paramEnvName` **逐字一致**，见 `internal/artifact/artifact.go`）：

1. `a`-`z` 抬成 `A`-`Z`；`A`-`Z` / `0`-`9` 原样；
2. 其余**每个 rune** 换成一个 `_`（非 ASCII 多字节 rune 仍只算一个 `_`）；
3. **不插 camel 大小写边界**：`sha→SHA`、`repo-url→REPO_URL`、`repoUrl→REPOURL`、`参数→ANOTIFY_PARAM___`（前缀自带 `_` + 两个 rune 各一个 `_`）；
4. 输出 = `EnvParamPrefix + 归一结果`，不返回 error。

撞名（`sha`/`SHA`/`Sha`、`repo-url`/`repo.url`/`repo_url`）是**预期**：本函数不消歧、不报错；写入期由 artifact 用**同一个函数**检测并拒绝 `param_env_collision`（capability.md §2.3）。T1 后续把 artifact 的私有函数替换为本函数即可（artifact 当前只读，由 T1 在其闭包改）。

## 2. 机器配置 hash（hash.go）

```go
func MachineConfigHash(vars map[string]string, limits Limits) (string, error)
```

canonical 文档（**只有** `vars` / `limits` 两个顶层键；两级 map 键由 `encoding/json` 按字节序排序）：

```json
{"vars":{"A":"1","B":"2"},"limits":{"total":8,"perCapability":{"*":1,"review":2}}}
```

归一与形状：

- `limits.Total == 0` → 缺省 `DefaultLimitsTotal`（=1）；`total < 0` → error（非法值不静默归一）；
- nil / 空 `vars` → `{}`；nil / 空 `perCapability` → `{}`（`{}` 不是 null、不是省略）；
- 输出 `sha256:<64 hex>`（与 artifact digest 同前缀）；
- **不含** revision / namespace 默认 / token / 宿主凭证 / envPassthrough（这些不进机器配置内容）；
- 两侧（中枢算期望值、节点算 appliedHash）只调用这一份实现，禁止第二份 hash 算法。

golden 值（可用来交叉验证 T1 实现；同时由 `TestMachineConfigHashGolden` 用手写 canonical 字符串独立复算）：

| 输入 | canonical | hash |
|---|---|---|
| `vars=nil, limits=Limits{}` | `{"vars":{},"limits":{"total":1,"perCapability":{}}}` | `sha256:7f8ef08104bf2220506e2a2f35f735dff34bd66ee19637c637465be8a38785bd` |
| `vars={"A":"1"}, limits={Total:2}` | `{"vars":{"A":"1"},"limits":{"total":2,"perCapability":{}}}` | `sha256:04a4070924fad973f3813be4317b0bc268d107b7fb5f0494f71a9b68581bc8f1` |
| `vars={"A":"1","B":"2"}, limits={Total:8, perCapability{"*":1,"review":2}}` | `{"vars":{"A":"1","B":"2"},"limits":{"total":8,"perCapability":{"*":1,"review":2}}}` | `sha256:80fa17cfdaa7c73612f7d349c7231d5acbe40b5a367fdb39cb91d23d248bee68` |

注：`encoding/json` 默认 HTML 转义（`<`→`\u003c`、`&`→`\u0026`）参与 canonical 字节，已用 golden 固定；两侧同实现即可，不要求跨语言。

## 3. 人工部署两帧（deploy_frames.go）

来源：父 CONTRACT rev2 §7（设计文档 `capability.md §8.5` **尚未**收录这两帧，见 §6 缺口）。

```go
const (
    FrameCapabilityDeploy   = "capability.deploy"   // 中枢 → 节点（下行）
    FrameCapabilityDeployed = "capability.deployed" // 节点 → 中枢（上行）
)

type DeployRefs struct {              // 两帧匿名嵌入，wire 上平铺
    Namespace string `json:"namespace"`
    Name      string `json:"name"`
    Version   int    `json:"version"`
    Digest    string `json:"digest"`
}

type CapabilityDeployFrame struct {
    Type      string `json:"type"`      // "capability.deploy"
    RequestID string `json:"requestId"` // 中枢生成；重放同一意图保持同一个
    DeployRefs                          // → namespace/name/version/digest 平铺
}

type DeployStatus string
const (
    DeployStatusApplied DeployStatus = "applied"
    DeployStatusFailed  DeployStatus = "failed"
)
func (s DeployStatus) Valid() bool    // 封闭集合

type CapabilityDeployedFrame struct {
    Type      string       `json:"type"`      // "capability.deployed"
    RequestID string       `json:"requestId"`
    DeployRefs                                // 与请求完全相同的四元组
    Status    DeployStatus `json:"status"`
    Error     string       `json:"error,omitempty"` // 人话原因，仅 failed 时出现
}
```

wire 键快照（`TestCapabilityDeployFrameWire` / `TestCapabilityDeployedFrameWire`）：

- deploy：`digest,name,namespace,requestId,type,version`
- deployed（failed）：`digest,error,name,namespace,requestId,status,type,version`；applied 时**省略** `error`

方向 / codec：

- `IsDownstream(FrameCapabilityDeploy)=true`、`IsUpstream(FrameCapabilityDeployed)=true`，已进 `downstreamFrames` / `upstreamFrames`；
- `Decode` 分别返回 `*CapabilityDeployFrame` / `*CapabilityDeployedFrame`；
- 既有的配置 5 帧、执行 10 帧、运维 4 帧语义/字段**未改**；`Limits`、`CapabilityRef`、emit HTTP DTO 也未改。

语义边界（T1/T3 共同遵守）：

- WS 帧**只传 refs**，不传 Artifact 整份文档（帧有 1 MiB 软上限）。节点用自身节点身份 Bearer `GET /v1/node/capabilities/{digest}` 取不可变 Artifact，客户端验证 spec/digest/路径后原子缓存；T1 handler 负责节点身份、ns 交集、明确 deployment target 验证。
- 人工部署不是自收敛：离线节点重连后由中枢重放**已明确请求**的未完成部署，不新增帧、不做通用 reconciler。
- `status` 封闭 `applied|failed`；`error` 是给人看的原因，不参与任何判断；`RequestID` 用于回执配对，refs 用于校验部署目标未被篡改。
- `Version` 是 `int`（与 `CapabilityRef.Version` 一致）；若父要求改 string，必须在 T1 消费前统一修订。

## 4. 新增具名测试（全部在 `internal/protocol/`）

`env_test.go`：

- `TestEnvContractVariables`（4 个常量字面值 + ANOTIFY_ 前缀）
- `TestParamEnvNameUppercase` / `TestParamEnvNameDigits` / `TestParamEnvNameSymbols`
- `TestParamEnvNameNoCamelBoundary`（repoUrl→REPOURL，反证 repoUrl≠repo-url）
- `TestParamEnvNameCollisions`（三组撞名 + 三组不撞）

`hash_test.go`：

- `TestMachineConfigHashGolden`（4 个 golden canonical + 手算 sha256；含 nil/空归一、键排序、HTML 转义）
- `TestMachineConfigHashDefaults`（total 未设置 == 显式 1）
- `TestMachineConfigHashCanonicalKeys`（顶层只有 vars/limits，limits 只有 total/perCapability；不含 revision/namespace/secret/token/envPassthrough/credentials）
- `TestMachineConfigHashOrderInsensitive`（32 次重建 map 稳定）
- `TestMachineConfigHashContentSensitive`（值/名/增键/total/perCapability 变则 hash 变）
- `TestMachineConfigHashNegativeTotal`（负 total 报错）
- `TestMachineConfigHashFormat`（`sha256:` + 64 hex）

`deploy_frames_test.go`：

- `TestDeployFrameNamesAndDirection`
- `TestCapabilityDeployFrameWire`（键快照 + 无 files/script/artifacts + 往返）
- `TestCapabilityDeployedFrameWire`（failed 带 error / applied 省略 error + 往返）
- `TestDeployStatus`（applied/failed 合法，其余非法）
- `TestDeployFramesShareRefs`（请求/回执 requestId + refs 一致）

既有测试更新：`TestFrameNamesAreWireValues`（21 帧全集 = 上行 ∪ 下行、互不相交）、`TestFrameRoundTripAndKeys`（新增两帧 fixture）。

## 5. 机械验证（本 handoff 提交前实跑）

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/protocol/... ./internal/artifact/... -count=1
# ok github.com/zzjcool/anotify/internal/protocol  0.058s
# ok github.com/zzjcool/anotify/internal/artifact  2.492s

GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/protocol/...
# （无输出，退出 0）

git diff --check
# （无输出，退出 0）

gofmt -l internal/protocol/   # 空
```

注意：补丁文件 `protocol-only.patch` 内含 diff 上下文行（`空格 + tab`），若对**提交区间**跑
`git diff f7e9a6c..HEAD --check` 会把补丁正文当成新增文本报 space-before-tab；固定验证门禁是不带
range 的 `git diff --check`（工作树），本分支工作树 0 退出。补丁本身已用
`git apply --check` 在 f7e9a6c 快照上验证通过，应用后 `internal/protocol` 与 caecd9d 逐字节相同。

## 6. 缺口 / 待父协调（N1 未实现，不私造中枢镜像）

1. **WS emit request/reply 缺失（事实）**：`internal/protocol/emit.go` 只有 HTTP DTO（`EmitRequest{to,id?,data}`、`EmitResponse{taskId,eventId?,to,version,pruned?}`、`EmitPruned`、`EmitError`）与退出码/HTTP 状态映射；`Decode` 的 21 帧里**没有**节点→中枢的 emit 转发帧。但设计要求节点网关本地预检后把请求**转发中枢**（capability.md §4.0），该 WS 传输未定义。
   - 父会话已向 root 提交 `task.emit` / `task.emit.ack` 共享 transport 提案；**N1 未实现任何未批准帧**，也未预占 wire 名。
   - 字段建议（仅供批准时参考）：`task.emit` node→hub `{requestId, taskId, attempt, node, to, id?, data?}`（WS 已认证节点身份，中枢可按 `(taskId,attempt)` 对该节点当前 lease 做权威校验；若需要 token 级权威，再加 `attemptToken`）；`task.emit.ack` hub→node `{requestId, taskId, attempt, status?, error?, taskId?/eventId?/to/version/pruned?}`（复用 HTTP 结果语义，剪枝回 `taskId:null`）。批准后按父分配串行增补 Decode/方向表/具名测试。
2. **设计文档缺部署帧章节**：`docs/design/v4/capability.md §8.5` 只有配置 5 帧 + 执行 10 帧，`capability.deploy/deployed` 仅见于父 CONTRACT rev2 §7。冻结设计不可由 T3 改；建议父/T1 在文档侧补一节（或声明 CONTRACT §7 即权威），wire 以本 handoff 为准。
3. **hash 的 `total<0` 语义**：归一时负值报 error（不静默变 1）。正常配置在写入/启动校验期就应被拒（anode.md §2：`limits.total ≥ 1`），N2 配置解析负责；若父认为 hash 应无条件成功，需修订本函数（冻结前）。
4. **`perCapability` 条目值不做 hash 侧校验**：hash 只归一缺省，`<1` 由 N2 配置校验拒绝；不在 hash 里再造一套校验规则。
5. artifact 仍用私有 `paramEnvName`：T1 后续闭包改为调用 `protocol.ParamEnvName`（同一实现），本任务因 artifact 只读未动。`MachineConfigHash` 的期望值计算（T1 中枢侧）也须直接 import protocol。

## 7. 未决 scope

- 本 handoff 只覆盖 N1（协议冻结）；N2/N3 的节点实现、artifact GET 客户端、部署缓存、emit 网关等均未开始，不属于本提交。
- 未改 Makefile / go.mod / api / store / ws / server / artifact / 冻结设计 / settings/env / 其他 worktree。
