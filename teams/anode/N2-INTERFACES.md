# N2 INTERFACES（N2A checkpoint · 实际 Go 签名）

> 任务：V4-ANODE-N2A（配置 / applied / 产物 GET+cache+部署回执 / 基础 WS 配置流）。
> 本文件记录**代码里真实存在**的签名（不是草案），供 N2B runtime/emit/lease/GC 直接使用。
> 只新增 `cmd/anode/**`；protocol/artifact 只读、未改。文件为 N2A checkpoint，A 未完成项见 §5。
>
> 机械门禁（N2 固定三条）：
>
> ```sh
> GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1
> GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/protocol/... ./cmd/anode/...
> git diff --check
> ```

## 0. 包布局（全部在 `cmd/anode/internal/`）

| 包 | 职责 | 依赖 |
|---|---|---|
| `config` | strict `anode.yaml`、机器级合并、机器 hash、ns 交集 | `internal/protocol`、`internal/listen` |
| `applied` | `anode.applied.yaml` 原子读写、strict + hash 校验 | `internal/protocol` |
| `hostenv` | 守护进程启动环境快照（PATH / HOME / 凭证白名单值） | stdlib |
| `listen` | `tcp://` / `unix://` 监听、实际端点、unix 安全清理 | stdlib |
| `capability` | 部署 refs 的 Bearer GET、产物校验、原子 cache、`capability.deployed` | `internal/artifact`、`internal/protocol` |
| `node` | 装配：hello 快照、welcome 应用、ns 授权、部署入口 | 以上全部 |
| `hub` | WS 客户端：连接/重连、配置 5 帧、部署 2 帧接线 | `coder/websocket`、`internal/protocol` |

**没有**新增 `main.go` / 子命令（`serve/install/emit` 属 N3）；没有 import store/ws/server/route/lease/executor/SQLite。

## 1. `config` — strict 本机配置与机器级合并

```go
const (
    DefaultNamespace = "default"
    Wildcard         = "*"
    MaxVarsValueBytes   = 4096
    MaxPassthroughNames = 32
)

type Error struct{ Problems []string }        // 启动期逐项问题的汇总
func (e *Error) Error() string

type Config struct {
    Name string `yaml:"name"`
    Server string `yaml:"server"`
    Token string `yaml:"token"`
    Namespaces []string `yaml:"namespaces"`
    Listen string `yaml:"listen"`
    Vars map[string]string `yaml:"vars"`
    Limits Limits `yaml:"limits"`
    EnvPassthrough []string `yaml:"envPassthrough"`
}

type Limits struct {
    Total *int `yaml:"total"`                 // nil = 未写（继承 node）；显式 0 启动失败
    PerCapability map[string]int `yaml:"perCapability"`
}

func Load(path string) (*Config, error)       // 读文件 + Parse
func Parse(data []byte) (*Config, error)      // KnownFields(true)：顶层/嵌套未知字段、多文档都失败

func (c *Config) DeclaredNamespaces() []string   // 拷贝；缺省 ["default"]
func (c *Config) LocalLimits() protocol.Limits   // 未写 total → 0（protocol 里 0 = 未设置）
func (c *Config) SortedPassthrough() []string    // 仅本机诊断用，禁止上报（完整名单只在本机）

func ValidateNodeDefaults(vars map[string]string, limits protocol.Limits) error

// 机器级合并：vars 逐键 local > node；limits 逐叶（total 未写才继承；perCapability 逐能力键）。
func MergeVars(local, node map[string]string) map[string]string
func MergeLimits(local, node protocol.Limits) protocol.Limits
func MergeMachine(local *Config, nodeVars map[string]string, nodeLimits protocol.Limits) (map[string]string, protocol.Limits)
func MachineHash(vars map[string]string, limits protocol.Limits) (string, error)  // 只调 protocol.MachineConfigHash
func HashMachine(local *Config, nodeVars map[string]string, nodeLimits protocol.Limits) (vars map[string]string, limits protocol.Limits, hash string, err error)

// 生效 ns = 本机申报 ∩ 中枢授权（只收紧，不扩张）。
func EffectiveNamespaces(declared, authorized []string) []string
func AllowsNamespace(effective []string, namespace string) bool
type NamespaceValidator func(namespace string) bool
```

不变量：

- 未知字段（含 `limits.*` / `vars` 形状）→ 启动失败；`namespaces: []` 与重复 → 失败；
- `limits.total: 0` 是**显式非法**，与「未写」在合并上不同；
- `envPassthrough`：名字 ≤32、去重、禁 `PATH`/`HOME`/`ANOTIFY_*`；
- 本机值只经 `hostenv.Baseline` 快照，`Hello` 不带白名单名单/值。

## 2. `applied` — machine applied 文件

```go
var ErrNotExist = errors.New("applied: 文件不存在")

type Store struct{ /* path */ }
func NewStore(dataDir string) *Store                  // dataDir/anode.applied.yaml
func (s *Store) Path() string
func (s *Store) Load() (protocol.AppliedConfig, bool, error)  // Present=false + nil error = 冷启动
func (s *Store) Save(revision string, vars map[string]string, limits protocol.Limits) (protocol.AppliedConfig, error)
```

不变量：

- `Save` 自己算 `protocol.MachineConfigHash`，0600 临时文件 + rename + fsync；**返回 cfg 才算成功**，调用方拿到后才可发 `config.applied`；
- `Load` strict JSON（`DisallowUnknownFields`、拒绝尾随内容）、拒绝符号链接/非普通文件、按内容重算 hash 不一致即 drift；
- 只存 `{revision,hash,vars,limits}`；namespace 默认值没有任何写入入口（结构上不可能落盘）；
- 坏 hash/手改文件 → `Load` error → `node.New` 不信任（Present=false，hello 报空 applied），等 welcome 覆盖。

## 3. `hostenv` — 启动环境快照

```go
type Baseline struct{ /* Path, Home, passthrough */ }
func Capture(passthrough []string) Baseline
func CaptureWith(passthrough []string, lookup func(string) (string, bool)) Baseline
func (b Baseline) PATH() string
func (b Baseline) HOME() string
func (b Baseline) Passthrough(name string) (string, bool)  // 只按名字取值；无「列出全部」入口
func (b Baseline) LookPath(name string) (string, error)    // 用启动基线 PATH

func LookPath(pathEnv, name string) (string, error)        // Unix 语义；返回绝对路径
```

**执行同源**：N2B 的任务环境 `PATH` 必须写 `baseline.PATH()`；`requires.commands` 探测也只用 `baseline.LookPath`，禁止再读 `os.Getenv`。

## 4. `listen` — emit 端点

```go
const DefaultSpec = "tcp://127.0.0.1:0"

type Spec struct{ Network, Address string }
func Parse(spec string) (Spec, error)
func Open(spec string) (*Listener, error)      // tcp 实际端口 / unix socket；unix 只清理确认无人监听的残留 socket
func (l *Listener) Endpoint() string           // tcp: http://host:port；unix: unix://<path>
func (l *Listener) Spec() Spec
func (l *Listener) Close() error               // unix 只删自己创建的那个 inode
```

普通文件 / 目录 / 符号链接占着 unix 路径 → 拒绝并保持原样；活跃 socket → 拒绝；残留 socket（连接被拒）→ 清理后重绑。
**N2B 未决 seam**：unix 形态的 `ANOTIFY_EMIT_ENDPOINT` 注入字符串尚未冻结（tcp 为 `http://host:port`）。

## 5. `capability` — 人工部署闭包

```go
const ArtifactPathPrefix = "/v1/node/capabilities/"
const (
    CodeBadRef, CodeNamespaceDeny, CodeUnauthorized, CodeForbidden,
    CodeNotFound, CodeBadStatus, CodeBadArtifact, CodeDigestMismatch,
    CodeRefMismatch, CodeNetwork = ... // string
)
type Error struct{ Code string; Status int; Message string }
func (e *Error) Error() string
func IsCode(err error, code string) bool

type Fetcher struct{ /* baseURL, token, http, AllowNamespace */ }
func NewFetcher(serverURL, token string) (*Fetcher, error)   // ws:// → http://, wss:// → https://
func HTTPBaseURL(serverURL string) (string, error)
func (f *Fetcher) SetHTTPClient(client *http.Client)
func (f *Fetcher) Fetch(ctx context.Context, ref protocol.DeployRefs) (*artifact.Artifact, string, error)

type Entry struct{ Digest, Name string; Version int; Artifact *artifact.Artifact; Path string }
type Cache struct{ Root string }
func NewCache(root string) *Cache
func (c *Cache) Store(a *artifact.Artifact, digest string) (Entry, error) // 落盘后 Load 复验
func (c *Cache) Load(digest string) (Entry, error)                       // digest/内容/spec 全验
func (c *Cache) Has(digest string) bool
func (c *Cache) Entries() ([]Entry, []error)                             // 重启恢复：坏条目只进 error

type Deployer struct{ Fetcher *Fetcher; Cache *Cache }
func (d *Deployer) Handle(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame
```

不变量：

- 只有**显式** `capability.deploy` 触发 GET；无扫描/自动收敛入口；
- GET 带 `Authorization: Bearer <node token>`；先本地校验 `refs` 形状与 `AllowNamespace`，再请求；
- 响应体按**产物文档本身**解析（`artifact.Parse` → `Validate` → 重算 digest → name/version 必须与 refs 一致）；
- 回执 `requestId` + 四元组 refs 与请求逐字一致；失败 status=failed + 一行 error；
- cache 条目目录/文件拒绝符号链接；损坏/被替换（digest 不符）拒绝；文件 0600、目录 0700。

## 6. `node` — 装配与 hello/welcome/deploy

```go
type Options struct {
    Config *config.Config
    Store  *applied.Store
    Cache  *capability.Cache
    Baseline hostenv.Baseline
    Facts protocol.NodeFacts
    AgentVersion, AgentCommit string
    Fetcher *capability.Fetcher // nil = 用 Config.Server/Token 构造
}
func New(opts Options) (*Node, error)          // 加载 applied；坏文件只记录 loadErr，不伪造事实

func (n *Node) Config() *config.Config
func (n *Node) Name() string
func (n *Node) AppliedPresent() bool
func (n *Node) AppliedError() error
func (n *Node) AppliedState() (revision, hash string)
func (n *Node) EffectiveNamespaces() []string
func (n *Node) AllowsNamespace(namespace string) bool
func (n *Node) HasCapability(digest string) bool
func (n *Node) Hello() protocol.HelloFrame
func (n *Node) ApplyWelcome(welcome protocol.WelcomeFrame) (*protocol.ConfigAppliedFrame, error) // nil = 幂等
func (n *Node) HandleDeploy(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame
func RuntimeFacts(workRoot, homeDir string) protocol.NodeFacts
```

`Hello` 填充共享 DTO 的全部字段（父已确认）：身份、`namespaces`（申报）、`localVars`/`localLimits`、`appliedRevision`/`appliedHash`、`agentVersion`/`agentCommit`、`facts`、`capabilities`（缓存产物 + `requires.commands` 探测，启动 PATH）、`instances`（机器级生效 total 的槽位）。
`ApplyWelcome`：校验节点默认值 → local > node 合并 → `protocol.MachineConfigHash` → Save 成功才更新状态并返回 `config.applied`；同一 revision+hash 返回 nil（不重复写盘/回报）；授权收窄与配置是否变化无关、立即生效。

## 7. `hub` — WS 客户端

```go
var ErrAuthRejected = errors.New("hub: 节点身份被拒绝")
const StreamPath = "/v1/node/stream"
func StreamURL(server string) (string, error)   // 空 path 补 StreamPath；显式 path 原样

type Handler interface {
    Hello() protocol.HelloFrame
    ApplyWelcome(welcome protocol.WelcomeFrame) (*protocol.ConfigAppliedFrame, error)
    AppliedState() (revision, hash string)
    HandleDeploy(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame
}

type DialFunc func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
type SleepFunc func(ctx context.Context, d time.Duration) error

type Client struct {
    ServerURL string
    Token     string
    Handler   Handler
    Dial      DialFunc   // nil = websocket.Dial
    Sleep     SleepFunc  // nil = time.Timer
    Backoff   Backoff
    ReadLimit int64
    Logf      func(format string, args ...any)
}
func New(serverURL, token string, handler Handler) (*Client, error)
func (c *Client) Run(ctx context.Context) error

type Backoff struct{ Base, Max time.Duration; Factor int }
func (b *Backoff) Next() time.Duration
func (b *Backoff) Reset()
```

行为：

- 每次连接（含重连）先发 `hello`；WS 握手同时带 `Authorization: Bearer <token>`（旧中枢的鉴权位置；hello 也有身份字段）；
- `welcome` → `Handler.ApplyWelcome` → 有帧才发 `config.applied`；`config.changed` → 发 `config.get`（带当前 applied revision/hash）；`capability.deploy` → `HandleDeploy` → 原样发回执；
- 401/403 = `ErrAuthRejected` **终态**，不重连；其它断线按有界指数退避重连（默认 1s → 30s 封顶）；
- 未知/未接线帧忽略（一条坏帧不掀连接）；执行/运维帧留给 N2B。

## 8. 已知假设 / 待父裁决（B 前必须收口）

1. **Artifact GET 响应 wire**：本实现取「响应体 = Artifact 文档本身」（`artifact.Parse` 支持 YAML/JSON），单点 seam `capability.decodeArtifactDocument`。父裁决若改为信封，只改该函数 + fake server fixture。GET 路径 `/v1/node/capabilities/{digest}` 已冻结。
2. **WS 端点**：父已裁决 `server` 空 path → `/v1/node/stream`，显式 path 原样；本实现已按此。未使用试拟的 `/v1/node/ws`。
3. **artifact 文档没有 namespace 字段**：客户端能验证的 ns 授权 = 本地「申报 ∩ 授权」预检（`Fetcher.AllowNamespace`）；服务端仍须按 CONTRACT §7 做节点身份 + ns 交集 + 明确部署目标校验。
4. **unix emit 端点**：`ANOTIFY_EMIT_ENDPOINT` 的 unix 字符串未冻结（tcp 已明确 `http://host:port`）。
5. **hello.token + WS Bearer**：共享 DTO 要求 hello.token；握手头沿用旧中枢位置。若 T1 新 handler 只认其中一处，删另一个即可（单点）。

## 9. N2B 剩余 scope（A 未做，不宣称 N2 完成）

- emit HTTP 网关（loopback/unix、Bearer attemptToken 本地预检）+ WS `task.emit`/`task.emit.ack` 接线（等父 E1 批准后的唯一 transport；A 未预占 wire 名）。
- 执行 10 帧：dispatch/accept/reject（仅 stale_config）、lease grant/renew/ack、result/result.ack、cancel/cancel.ack；真实进程组 TERM→5s→KILL、槽位 `processGroupReaped` 回收。
- 任务环境展开（run.env/`$(vars.X)`/params 归一）、workDir/HOME/files_escape 执行前校验、envPassthrough 实际注入与 `envDiagnostics`（只名字+来源）。
- 双流日志、result 上报、ops（upgrade/logs）、GC/保留（7 天、用户 workDir 安全）。
- `cmd/anode` 的 `serve` 装配（dataDir 布局、listener 打开、signal/grace、把 `hub.Client` 与 `node.Node` 接起来）、`install`/`emit` 子命令（N3）。
- 客户端对执行下行帧的分发扩展点（当前 `hub.Client` 对它们不处理）。
