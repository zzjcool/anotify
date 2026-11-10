# G1 公开接口与简洁性审查（只读，未运行 build/test）

范围：worktree HEAD=`19827b3`，重点审查 `52b17a6`（store/sourcespec）与 `19827b3`（protocol N1）；artifact 为 G1-1 既有交付，一并抽取签名。G1-5/G2/G3 未开始，不判 missing。

## A. 接口事实（G2/G3 消费面，精确签名 + path:line）

### artifact（`internal/artifact/artifact.go`）
```
type Artifact{APIVersion,Kind string; Metadata Metadata; Spec Spec} @161
type Metadata{Name string; Version int; DisplayName string} @172
type Spec{Params json.RawMessage; Next []string; Run Run; Requires Requires; Lifecycle Lifecycle; Concurrency Concurrency} @182
type Run{Entrypoint string; Files map[string]string; Env map[string]EnvValue; WorkDir string} @199
type EnvValue{Value,FieldPath string}; func (v EnvValue) IsValueFrom() bool @216,224
type Requires{Commands,Values []string}; type Lifecycle{Idempotent bool; MaxAttempts,ResultTTLSec *int; TimeoutSec int} @286,328
func (l Lifecycle) EffectiveMaxAttempts() int | EffectiveResultTTLSec() int | EffectiveTimeoutSec() int @340,348,356
type Concurrency{MaxPerNode *int}; func (c Concurrency) EffectiveMaxPerNode() int @359,390
type Error{Problems []string}; func Code(err error) string; ProblemCode(string) string; ProblemMessage(string) string @409,424,433,441
func Parse(data []byte) (*Artifact, error) @454
func Normalize(a *Artifact) (*Artifact, error) @534
func Validate(a *Artifact) error @595
func CanonicalJSON(a *Artifact) ([]byte, error) @1109
func FoldKey(a *Artifact) (string, error) @1126
func Digest(a *Artifact) (string, error) @1145   // version<1 直接报错
func SameFold(a, b *Artifact) (bool, error) @1179
func SameContent(a, b *Artifact) (bool, error) @1196
func MarshalYAML(a *Artifact) ([]byte, error) @1213; func Decode(data []byte) (*Artifact,string,error) @1230
const APIVersion="anotify/v1"; KindCapability="Capability"; DefaultMaxAttempts=3; DefaultResultTTLSec=604800 @54,86,97
```

### sourcespec（`internal/sourcespec`）
```
type Target{Capability string; Params map[string]any} @sourcespec.go:66
type Spec{Kind string; Enabled bool; Config json.RawMessage; Targets []Target} @sourcespec.go:75
type Config{Expr,TZ,Token,Secret string; Payload map[string]any; PayloadSchema json.RawMessage; Sample map[string]any} @sourcespec.go:86
func Kinds() []string @46; func IsKind(kind string) bool @53; func ParseConfig(kind string, raw json.RawMessage) Config @105; func SortedKeys(map[string]any) []string @174
// 注意：不存在 Validate；真实入口是 ValidateSpec
func NormalizeConfig(kind string, raw json.RawMessage) (json.RawMessage, error) @config.go:55
func MergeConfig(kind string, existing, patch json.RawMessage) (json.RawMessage, error) @150
func ValidateSpec(spec Spec, forWrite bool) error @191
func ValidateTargets(targets []Target) error @210
func MaskConfig(kind string, raw json.RawMessage) json.RawMessage @230
func SameConfig(kind string, a, b json.RawMessage) bool @270
func SameTargets(a, b []Target) bool @286
func ValidateParamsTemplate(params map[string]any) error @template.go:40
func RenderParams(target Target, payload map[string]any) (map[string]any, []string) @212
func ParseCron(expr string) (*CronSchedule, error) @cron.go:48; func (s *CronSchedule) Next(after time.Time) time.Time @147
func NextCronTime(config json.RawMessage, after time.Time) (time.Time, error) @196
var ErrInvalidKind, ErrInvalidConfig error @27,29（store 以别名转发）
```

### protocol N1（`internal/protocol`，T3 快照 `19827b3`）
```
func Encode(f any) ([]byte, error) @codec.go:35; func Decode(data []byte) (any, error) @48; func FrameTypeOf([]byte) (string,error) @24
func IsUpstream(frameType string) bool @140; func UpstreamFrames() []string @150; func DownstreamFrames() []string @157
func MachineConfigHash(vars map[string]string, limits Limits) (string, error) @hash.go:30
func ParamEnvName(name string) string  // 返回含 ANOTIFY_PARAM_ 前缀 @env.go:34
const EnvEmitEndpoint="ANOTIFY_EMIT_ENDPOINT"; EnvTaskToken="ANOTIFY_TASK_TOKEN"; EnvTaskID="ANOTIFY_TASK_ID"; EnvParamPrefix="ANOTIFY_PARAM_" @11-20
type Limits{Total int; PerCapability map[string]int} @limits.go:12; const DefaultLimitsTotal=1; LimitsWildcard="*" @20,22
type DeployStatus string; func (s DeployStatus) Valid() bool; const DeployStatusApplied/Failed @deploy_frames.go:26,36
type DeployRefs{Namespace,Name string; Version int; Digest string} @49
type CapabilityDeployFrame{Type,RequestID string; DeployRefs} @65
type CapabilityDeployedFrame{Type,RequestID string; DeployRefs; Status DeployStatus; Error string} @77
const FrameCapabilityDeploy="capability.deploy"; FrameCapabilityDeployed="capability.deployed" @21,23
// Decode 注册：codec.go:96-101；deploy=downstream、deployed=upstream：codec.go:119,133
```

### store 关键类型（`internal/store`）
```
type CapabilityVersion{ID,NamespaceID,Name,DisplayName string; Version int; Digest,FoldKey,InstalledFrom string; ResourceVersion,CreatedAt int64; Spec artifact.Spec `json:"-"`} @capability.go:69; Artifact() artifact.Artifact @92
type Node{ID,Name string; Tags []string; Status,TokenHash string; MaxInstances,MaxConcurrency int; AgentVersion,AgentCommit string; AgentUpgrade; LastSeenAt *int64; CreatedAt,ResourceVersion int64; Facts NodeFacts; DeclaredCapabilities []DeclaredCapability; DeclaredNamespaces []string} @node.go:52
type NodeInstance{ID,NodeID string; Slot,Concurrency int; BusyTask *string; Alive bool; UpdatedAt int64} @106; type InstanceSpec{Slot,Concurrency int} @129
type NodeConfig{NodeID string; Namespaces []string; Vars map[string]string; Limits map[string]any; Revision,ResourceVersion,AppliedRevision int64; AppliedHash string; AppliedAt *int64; LocalVars map[string]string; LocalLimits map[string]any} @config.go:115
type NamespaceConfig{NamespaceID string; Vars map[string]string; Revision,ResourceVersion int64} @101
type SystemConfig{RetentionEventTTLSec,RetentionTaskTTLSec,RetentionResultTTLSec,ResourceVersion int64} @90
type Task{ID,NamespaceID,TraceID string; ParentTaskID *string; CapabilityID,Capability string; Version int; Digest string; Params json.RawMessage; Phase string; NodeID *string; NodeName string; ConfigRevision int64; Attempt,MaxAttempts int; AssignmentID string; ProposedAttempt int; Idempotent bool; LeaseID string; LeaseUntil *int64; AttemptTokenHash string `json:"-"`; WaitingReason string; NextAttemptAt *int64; CancelReason string; CancelRequestedAt *int64; Reported bool; Summary string; Artifacts []string; Error string; CreatedAt int64; StartedAt,FinishedAt *int64} @task.go:123
type TaskResult{Phase,Reason,Summary string; Artifacts []string; Error string} @778; type TaskFilter @346; type ParamsTooLargeError @90
type Source{ID,NamespaceID,Name,Kind string; Config json.RawMessage; Targets []sourcespec.Target; Enabled bool; LastFiredAt,NextFireAt *int64; InstalledFrom string; ResourceVersion,CreatedAt int64} @source.go:57
type Event{ID,NamespaceID,TraceID,Source,Type string; Payload json.RawMessage; DeliveryID *string; Reason string; CreatedAt int64} @event.go:45
type EmitAuth{TaskID,NodeID,NodeTokenHash string; Attempt int; LeaseID,AttemptToken string; Now int64} @emission.go:60
type EmitRecord{ID,TaskID string; EmitID *string; ToRef string; Data,Result json.RawMessage; CreatedAt int64} @41
type Deployment{ID,NamespaceID,NodeID,CapabilityID,Capability string; Version int; Digest,State,Reason string; CreatedAt,UpdatedAt int64} @deployment.go:74
type Artifact{NamespaceID,CapabilityID,Digest string; Spec []byte}（派生值，非 artifact.Artifact）@59
type DeploymentDelivery{DeploymentID,CapabilityID,Capability string; Version int; Digest string; Spec []byte} @547; type AppliedCapability @422; ReconcileStats @430
type RetentionStats{EventTTLSec,TaskTTLSec,ResultTTLSec,EventsDeleted,TasksDeleted,DeliveriesDeleted int64; EventsSkipped,TasksSkipped,Truncated bool} @retention.go:38
type Edge{NamespaceID string; FromIsSource bool; FromRef,ToRef string; CreatedAt int64} @graph.go:20; EdgeTarget @36
type OrphanPendingTask{Task; IdleSec int64; Reason string} @orphan.go:32
type EffectiveVar{Value,Source string} @config.go:84; type Stats/TaskStats @stats.go:17,44
```

### store 方法（G2/G3 直接消费）
```
// 开关
func Open(path string) (*DB,error) @store.go:56 | OpenWithConns(path string,maxConns int)(*DB,error) @63 | Now() int64 @214 | MaintainStats(ctx)([]string,error) @236 | (d *DB) SchemaVersion(ctx)(int,error) @migrate.go:286
// 能力
(d *DB) CreateCapability(ctx,c *CapabilityVersion) error @capability.go:150 | CreateCapabilityForce(ctx,c *CapabilityVersion,_ bool) error @170
UpdateNext(ctx,namespaceID,name string,version int,next []string)(*CapabilityVersion,error) @359 | GetCapability(ctx,namespaceID,name string,version int)(*CapabilityVersion,error) @589
GetCapabilityByID(ctx,namespaceID,id string)(*CapabilityVersion,error) @605 | SetCapabilityDisplayName(ctx,namespaceID,id,displayName string) error @622
ListCapabilities(ctx,namespaceID)([]*CapabilityVersion,error) @650 | NextCapabilityVersion(ctx,namespaceID,name)(int,error) @676 | CapabilityNamespace(ctx,capabilityID)(string,error) @684
GetCapabilityLatest(ctx,namespaceID,name)(*CapabilityVersion,error) @ref.go:89 | ResolveRef(ctx,namespaceID,ref)(*CapabilityVersion,error) @109
// 集群节点/槽位
CreateNode(ctx,n *Node)(token string,err error) @node.go:232 | GetNode(ctx,nodeID)(*Node,error) @311 | GetNodeByName @331 | GetNodeByTokenHash(ctx,tokenHash)(*Node,error) @351
ListNodes(ctx)([]*Node,error) @371 | UpdateNodeStatus(ctx,nodeID,status) error @390 | TouchNode(ctx,nodeID,at int64) error @420
RecordNodeFacts(ctx,nodeID,facts NodeFacts) error @447 | UpsertInstances(ctx,nodeID,specs []InstanceSpec)([]*NodeInstance,error) @574
ListInstances(ctx,nodeID)([]*NodeInstance,error) @643 | FreeSlotsByNode(ctx,nodeIDs []string)(map[string]int,error) @668
MarkInstancesDead(ctx,nodeID) error @708 | ReclaimStaleSlots(ctx,nodeID,now int64)(int,error) @725
LeaseSlot(ctx,nodeID,taskID)(string,error) @755 | ReleaseSlot(ctx,nodeID,instanceID) error @793 | ReleaseSlotByTask(ctx,nodeID,taskID) error @803
InstanceStatsByNode(ctx,nodeIDs []string)(map[string]InstanceStat,error) @841
EligibleNodes(ctx,namespaceID string,spec artifact.Spec,digest string)([]*Node,map[string]string,error) @901 | NodeAvailabilityReason(map[string]string) string @1133
NewNodeToken()(token,hash string,err error) @136 | HashNodeToken(token) string @146 | ValidateNodeName(name) string @158
// 三层配置
GetSystemConfig(ctx)(*SystemConfig,error) @config.go:349 | PutSystemConfig(ctx,cfg *SystemConfig,rv int64) error @381
GetNamespaceConfig(ctx,namespaceID)(*NamespaceConfig,error) @443 | PutNamespaceConfig(ctx,namespaceID string,vars map[string]string,rv int64)(*NamespaceConfig,error) @485
GetNodeConfig(ctx,nodeID)(*NodeConfig,error) @570 | PutNodeConfig(ctx,cfg *NodeConfig) error @669 | PutNodeConfigBatch(ctx,cfgs []*NodeConfig) []NodeConfigFailure @763
RecordApplied(ctx,nodeID string,revision int64,hash string) error @782
RecordHello(ctx,nodeID string,facts NodeFacts,capabilities NodeCapabilityReport,declaredNamespaces []string,localVars map[string]string,localLimits map[string]any,agentVersion,agentCommit string) error @827
EffectiveVars(ctx,nodeID,namespaceID string,names []string)(map[string]EffectiveVar,error) @923
ResolveRequiredValues(ctx,nodeID,namespaceID string,values []string)(bool,[]string,error) @1023
EffectiveLimits(ctx,nodeID)(map[string]any,error) @1062 | AuthorizedNamespaces(ctx,nodeID)([]string,error) @1121
ValidateVars/ValidateConfigNamespaces/ValidateLimits @151,181,198
// 任务/租约
CreateTasks(ctx,ts []*Task) error @task.go:214 | CheckTaskParamsSize(taskID string,params json.RawMessage) error @109
GetTask(ctx,namespaceID,id)(*Task,error) @324 | ListTasks(ctx,namespaceID string,f TaskFilter)([]*Task,error) @356
ListTasksByTrace(ctx,namespaceID,traceID)([]*Task,error) @405 | CountTasksByTrace @432
ListDispatchableTasks(ctx,now int64,limit int)([]*Task,error) @449
AssignTask(ctx,taskID,nodeID,assignmentID string,proposedAttempt int) error @481
GrantLease(ctx,taskID,assignmentID,nodeID string,now int64)(leaseID,attemptToken string,attempt int,expiresAt int64,err error) @552
RenewLease(ctx,taskID string,attempt int,leaseID string,now int64)(int64,error) @707
RejectAssignment(ctx,taskID,assignmentID,reason string,now int64) error @735
CompleteTask(ctx,taskID string,attempt int,leaseID string,result TaskResult) error @794
RequestCancel(ctx,taskID,reason string,now int64) error @883 | AckCancel(ctx,taskID string,attempt int,processGroupReaped bool,now int64) error @941
RecoverLostLeases(ctx,now int64)(int,int,error) @1015 | VerifyAttemptToken(ctx,taskID,token string)(*Task,error) @1129
CountTasksByPhase(ctx,namespaceID,traceID)(map[string]int,error) @1263 | CountOpenTasks/Since @1292,1297 | OldestPendingCreatedAt @1312
RetryBackoff(attempt int) int64 @1231 | StaleConfigBackoff @1237 | TaskPhases/IsTaskPhase/IsTerminalPhase @34,39,49
// 源
SourceKinds() []string @source.go:27 | IsValidSourceKind @30 | NewSourceToken()(string,error) @124
CreateSource(ctx,s *Source) error @141 | ListSources @306 | GetSource @329 | GetSourceByName @351 | LookupHook(ctx,token)(*Source,error) @374
UpdateSource(ctx,s *Source) error @404 | DeleteSource @489 | ListDueSources(ctx,now int64,limit int)([]*Source,error) @520 | MarkSourceFired(ctx,id string,firedAt,nextFire int64) error @550
// 事件/emit
InsertEvent(ctx,e *Event,now int64,windowSec int64)(inserted bool,err error) @event.go:77 | InsertPruneEvent(ctx,e *Event) error @211
GetEvent @307 | GetEventByTrace @327 | ListEvents(ctx,namespaceID,traceID,source string,limit int)([]*Event,error) @355 | ListEventsBefore(...beforeCreatedAt int64,beforeID string,limit int) @event_page.go:15
LookupEmit(ctx,taskID,emitID)(*EmitRecord,error) @emission.go:78 | EmitAuthorized(ctx,auth EmitAuth,rec *EmitRecord,tasks []*Task)(json.RawMessage,bool,error) @122 | ListEmits(ctx,taskID)([]*EmitRecord,error) @351
// 部署/保留/边/孤儿/命名空间
CurrentArtifact(ctx,namespaceID,capabilityID)(*Artifact,error) @deployment.go:93 | GetArtifactByDigest @148 | ListArtifacts @173
CreateDeployments(ctx,namespaceID,capabilityID,digest string,nodeIDs []string)([]*Deployment,error) @195
GetDeploymentByNodeCapability @283 | GetDeployment @290 | ListDeployments(ctx,namespaceID string,f DeploymentFilter) @350
DeleteDeployment(ctx,namespaceID,id)(*Deployment,error) @386 | MarkDeployResult(ctx,namespaceID,deployID,state,reason string) error @399
ReconcileNode(ctx,nodeID string,applied []AppliedCapability,now int64)(ReconcileStats,error) @458 | DeploymentsForNode(ctx,namespaceID,nodeID)([]*DeploymentDelivery,error) @561
RunRetention(ctx,now int64)(RetentionStats,error) @retention.go:66 | DeleteExpiredTasks @135 | DeleteExpiredEvents @171 | DeleteExpiredEventDeliveries @200 | DeleteExpiredPlaybookInstalls @233
EdgesFrom(ctx,namespaceID string,fromIsSource bool,fromRef string)([]EdgeTarget,error) @graph.go:45 | ListEdges @73 | ReplaceCapabilityEdges(ctx,c *CapabilityVersion) error @108 | ReplaceSourceEdges(ctx,s *Source) error @136
ListOrphanPendingTasks(ctx,namespaceID string,olderThanSec,limit int)([]OrphanPendingTask,error) @orphan.go:54 | AbandonPendingTask(ctx,namespaceID,taskID,reason string) error @149
DefaultNamespaceID/GetNamespace/ListNamespaces/CreateNamespace(ctx,slug,name)/RenameNamespace/DeleteNamespace(ctx,id,force bool) @namespace.go:112,126,145,242,274,308
AuthorizeNamespace(ctx,userID,namespaceID) error @authz.go:27 | AuthorizeNamespaceForNode(ctx,namespaceID) error @69
```
错误哨兵：`ErrNotFound`(auth.go:12)、`ErrNamespaceRequired`(namespace.go:39)、`ErrInvalidTransition/ErrLeaseMismatch/ErrAttemptTokenInvalid/ErrTaskNotRunning/ErrMaxAttemptsExceeded/ErrNoAssignment`(task.go:63-76)、`ErrEmitIDConflict/ErrEmitNamespaceDenied/ErrNodeIdentityMismatch`(emission.go:17,23,29)、`ErrSourceNotFound/NameTaken/KindImmutable/NameImmutable`(source.go:43-49)、`ErrArtifactNotFound`(deployment.go:43)、`ErrCapabilityInvalid/ErrVersionConflict`(capability.go:30,36)、`ErrConfigInvalid/ErrResourceVersionRequired/Conflict`(config.go:60-64)、`ErrNoCapacity/ErrTooManyInstances`(node.go:119-125)。

## B. 简洁性 findings（已读证，severity / path:line / 证据）

**major**
1. **RequestCancel pending 分支非 CAS**：`task.go:890-918` 先在事务外读 phase，随后 `finishTx`（`task.go:1184-1187` 仅 `WHERE id=?`）无条件改终态。与 GrantLease 并发时会把刚 running 的任务改成 cancelled/abandoned，槽位仍绑在节点上；对照 `AbandonPendingTask` 正确使用 `WHERE phase='pending'`（`orphan.go:168-175`）。
2. **GrantLease 预算用尽泄漏槽位**：`task.go:637-647` 提交 abandoned 后置 `committed=true`，跳过 `task.go:581-586` defer 的 `ReleaseSlot`；`finishTx` 不清 `node_instances.busy_task`。该槽位要等下一次 hello `ReclaimStaleSlots`（`node.go:725`）才回收。
3. **参数环境名三份实现（重复权威）**：`protocol/env.go:23-34` 注释声称“只有一份规则，禁止各写一份”，但 `artifact/artifact.go:680` 仍私有复制（含相同前缀），`cmd/agent/runner.go:990` 是第三份且返回**无前缀片段**（runner.go:803 自己拼前缀）。任一处改动会造成“写入期撞名校验”与“节点注入”漂移。
4. **孤儿列表判据与 EligibleNodes 不一致**：注释自称“同一组判据”（`orphan.go:17-18,44-48`），实际 SQL 只查 online+deployment applied（`orphan.go:102-113`），缺 ns 授权/申报、`applied_revision==revision`、commands、requires.values（对比 `node.go:901-1046`）。后果：因 `ConfigPending`/缺命令卡死的 pending 任务不会出现在 `/orphans`。
5. **读路径使用 BEGIN IMMEDIATE 事务**：DSN `_txlock=immediate`（`store.go:104`）使每个 `BeginTx` 先拿写锁；`EligibleNodes` 明确避免（`node.go:909`），但 `GetSystemConfig`(config.go:349)、`GetNamespaceConfig`(443)、`loadVarsLayers`(956)、`EffectiveLimits`(1062)、`AuthorizedNamespaces`(1121) 仍开事务读。G2 派发热路径（EffectiveVars）会与写事务互斥、可能触发 5s busy timeout。
6. **force 参数被静默忽略（假 success）**：`CreateCapabilityForce(ctx,c,_ bool)` 与 CreateCapability 完全等价（`capability.go:166-172`），而 v3 server 仍传 `req.Force`（`internal/server/console_handlers.go:1406`）；调用方以为 force 生效，实际走不可变冲突错误。

**minor**
7. `MarkDeployResult` 仍接受 `DeploymentLocal`（`deployment.go:398-403`），但 v4 拒旧库、自身注释称该态“不再产生”（`deployment.go:27-33`）——唯一写部署状态的入口保留 v3 旁路。
8. `MarkSourceFired` 不检查 `RowsAffected`（`source.go:550-556`），不存在的源也返回成功；`CreateSource` 忽略调用方 `s.LastFiredAt`（`source.go:194-198` 固定传 nil）。
9. `deploymentRetryAfter` 死常量靠 `var _ =` 续命（`deployment.go:609-612`）；`ErrDeploymentExists`（`deployment.go:46`）、`NewArtifactID`（`ids.go:99`）、`NewMessageID`（`ids.go:68`）、`ErrNotImplemented`（`store.go:26`）均无生产调用方。
10. `AckCancel(processGroupReaped=false)` 返回 nil 纯 no-op（`task.go:973-982`），调用方无法区分“已记录待回收”与“已生效”。
11. `RejectAssignment` 无 node 身份参数，且 `assignmentID==""` 时跳过匹配（`task.go:735-757`）：知道 taskID 的调用方可重置任意 pending 任务的候选派发。
12. 事件列表 SQL 双份：`eventListQuery`（`event.go:384-398`）与 `eventPageQuery`（`event_page.go:49-72`），仅靠等价性测试防漂移；JSON 语义比较亦有两份（`store/jsonutil.go:19`、`sourcespec/json.go:34`）。
13. `store.Artifact`（`Spec []byte`）与 `artifact.Artifact`（结构体）并存（`deployment.go:59-71`），部署/路由消费方需在两套表示间转换，schema 变化时易分叉。
14. `PutSystemConfig(ctx,cfg,rv)` 同时接受 cfg.ResourceVersion 与独立 rv（`config.go:381` 只用 rv），与 `PutNodeConfig` 只用 cfg.ResourceVersion（`config.go:669`）不一致，属多余输入面。

## C. 待验证
- `protocol.MachineConfigHash` 需要 `protocol.Limits`，而 `store.EffectiveLimits` 返回 `map[string]any`（json.Number）；G1 未提供适配器，G2 需自己转换（数值 `1 vs 1.0` 的归一风险待定）。
- 本 worktree HEAD 的 route/ws/server 仍是 v3（`store.NodePin`、`store.SourceKindGitHub` 等已不存在），无法静态确认 G2/G3 对新签名的实际调用形状，须待 G2/G3 落地后复核。

```json
{"ok": true, "reason": "recon complete: artifact/sourcespec/protocol/store G1 public signatures extracted (~120 facts), 14 simplicity findings (6 major, 8 minor), no writes/tests"}
```
