# EXPORTED-INTERFACES · recovery checkpoint

**As of product commit `21253ed92ff2782ceca602982d966338a26a1c67`**（后续仅父授权文档同步）。这是已实现Go表面，不是T1最终冻结/完整v4闭环。不得按旧G1中间签名实现G2；精确全量导出见 `api-snapshots/*.txt`（本级 `go doc -all` 实际生成，无测试skip）。

## 来源与所有权

- `api-snapshots/store-21253ed.txt`：实际类型/所有导出方法，包括下列S/R2变更。
- `api-snapshots/{artifact,sourcespec,playbook}-21253ed.txt`：纯包实际类型/函数。
- `api-snapshots/protocol-21253ed.txt`：仅T3授权N1+E1/A1快照（本队提交19827b3/1b420e6，T3源caecd9d/63570df）。**尚未包含A2/A3最终Go delta**。T3唯一作者，T1不得加镜像。
- 逐阶段原始verdict见 `verdicts/`；fresh审查见 `reviews/`。不能把有major的旧review文件当最终通过。

## G2必须使用的S新接口（旧签名已过时）

```go
func (d *DB) AssignTask(ctx context.Context, taskID, nodeID, assignmentID string, proposedAttempt int) error
func (d *DB) AssignTaskAt(ctx context.Context, taskID, nodeID, assignmentID string, proposedAttempt int, now int64) error
func (d *DB) GrantLease(ctx context.Context, taskID, assignmentID, nodeID string, slot int, now int64) (leaseID, attemptToken string, attempt int, expiresAt int64, err error)
func (d *DB) RejectAssignment(ctx context.Context, taskID, assignmentID, reason string, now int64) error
func (d *DB) TaskInputs(ctx context.Context, taskID string, names []string) (map[string]EffectiveVar, error)
const AssignmentWindowSec int64 = 30
const TaskRejectSlotUnavailable = "slot_unavailable"

func (d *DB) GetTaskAttempt(ctx context.Context, taskID string, attempt int) (*TaskAttempt, error)
func (d *DB) ListTaskAttempts(ctx context.Context, taskID string) ([]*TaskAttempt, error)
func (d *DB) PreviousTaskAttempt(ctx context.Context, taskID string) (*TaskAttempt, error)
func (d *DB) ReapTaskAttempt(ctx context.Context, in ReapedInputs) (bool, error)
type ReapedInputs struct {
    TaskID string; Attempt int
    NodeID, NodeTokenHash, LeaseID string
    Slot int; InstanceID string; Now int64; Reason string
}
func (d *DB) MarkNodeOffline(ctx context.Context, nodeID string, now int64) error
func (d *DB) ReclaimStaleSlots(ctx context.Context, nodeID string, observations []SlotObservation, now int64) (int, error)
func (d *DB) LeaseSlot(ctx context.Context, nodeID, taskID string, attempt int, leaseID string, slot int) (string, error)
func (d *DB) ReleaseSlot(ctx context.Context, nodeID, instanceID, taskID string, attempt int) error
// ReleaseSlotByTask已删除，禁止按logicaltask一把释放重叠attempt。
type SlotObservation struct {
    Slot int; Busy *bool; TaskID string; Attempt int; LeaseID string
    AssignmentID string; ProposedAttempt int; ObservedAt int64
}
type InstanceSpec struct { Slot int }
func TypedMachineLimits(raw map[string]any) (protocol.Limits, error)
func MachineConfigHash(vars map[string]string, limits map[string]any) (string, error)
```

- Task `NamespaceVars`/`LeaseInstanceID` 内部`json:"-"`；`NamespaceVarsRevision`与`AssignmentExpiresAt`可诊断。
- Grant同事务使用accepted slot/30s window，计attempt/授lease，写`task_attempts`机器vars/limits/source/revision/hash快照。TaskInputs只该attempt快照+逻辑task首次namespace pin，不现读后续配置；无snapshot报ErrNoAttemptSnapshot。
- ReapTaskAttempt用当前Node身份与**历史**task/attempt/node/lease/slot绑定；旧授权无需active即可仅释放对应旧槽，不能改变新phase/token/槽。旧result/emit依旧拒。
- MarkNodeOffline只停派发/alive=0、保busy；Reclaim仅显式SlotObservation（Busy三态、匹配历史refs），不能DBphase/rehello推断reaped。
- CompleteTaskAt当前attempt/lease/expiry/cancel同事务校验；旧AckCancel不可当完整A3历史入口，G2应映射ReapTaskAttempt。

## Emit权威入口（E1）

```go
type EmitAuth struct {
    TaskID, NodeID, NodeTokenHash string
    Attempt int; LeaseID, AttemptToken string; Now int64
}
func (d *DB) EmitAuthorized(ctx context.Context, auth EmitAuth, rec *EmitRecord, tasks []*Task) (json.RawMessage, bool, error)
```

事务内验证身份/ns交集/current running/node/attempt/lease/token/未过期/cancel，**再**判(taskId,id)/冲突/写emit+子任务。已有id不绕授权；无id每次新，不用requestId业务去重。调用方不能先验权再独立接受。完整类型见store快照。

## 已实现包安装（8497d7f）

```go
func (d *DB) CreatePlaybook(ctx context.Context, namespaceID, slug string, files map[string]string, source string) (*PlaybookRow, error)
func (d *DB) GetPlaybook(ctx context.Context, namespaceID, slug string) (*PlaybookRow, error)
func (d *DB) ListPlaybooks(ctx context.Context, namespaceID string) ([]*PlaybookRow, error)
func (d *DB) ReplacePlaybook(ctx context.Context, namespaceID, slug string, expectedRV int64, files map[string]string, source string) (*PlaybookRow, error)
func (d *DB) RenderPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any) (*playbook.RenderResult, error)
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, slug string, values map[string]any, dryRun bool) (*playbook.Receipt, error)
func (d *DB) DeletePlaybook(ctx context.Context, namespaceID, slug string, expectedRV int64) (*PlaybookDeleteResult, error)
func (d *DB) ListPlaybookInstalls(ctx context.Context, namespaceID, currentPlaybookID string) (*PlaybookInstallList, error)
func (d *DB) GetPlaybookInstall(ctx context.Context, namespaceID, slug, installID string) (*playbook.Receipt, error)
```

Existing适配器在当前transaction取快照并捕获error；只Action=create写对象/边，reuse不改next/targets/displayName；普通每次新receipt、dryRun同路径Rollback；历史snapshot原slug/id/ns、删包保receipt、同slug新id不混实例。created/skipped为对象，values masked、digest打码前。HTTP必须按W3映射，不直接JSON dump Package内部类型成第二根清单。

## Recovery updates / explicitly pending authenticated entry (not implemented)

Recovery C1 `459069f` added layout marker and named rejection; fresh gate found structural validator gaps, precise C1-R is active. S3 `bd69177` adds transaction ns recheck, stale assignment preservation, non-destructive empty/shrink observations, expiry renewal rejection and discriminating history/JSON/GC tests. Original report and independent fixed four mechanical close are on disk; fresh strict review still `ok:false` for credential/budget/coverage gaps. Final parent T3 support63570df->33e517a is imported at `a3bbd15`, shared protocol no longer waiting.

**PENDING, required before WS consumption; these are proposed internal signatures, NOT actual exported methods:**

```go
// Authenticated from current NodeSession; never accept caller-selected body node UID.
type NodeAuthorization struct { NodeID, NodeTokenHash string }
func (d *DB) GrantLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
    taskID, assignmentID string, proposedAttempt, slot int, now int64) (
    leaseID, attemptToken string, attempt int, expiresAt int64, err error)
func (d *DB) RenewLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
    taskID string, attempt int, leaseID string, now int64) (int64, error)
// CompleteTaskAuthorized likewise must validate current Node credential/ns and
// current attempt/lease/token/expiry/cancel in the same result+manifest transaction.
```

A Node UID alone cannot reject a rotated identity token. G2 MUST use implemented authenticated entries with current `nodes.token_hash` equality and same-node assignment/attempt binding in the write transaction; precheck then ordinary GrantLease/RenewLease is forbidden TOCTOU. Renew wire has no attemptToken field, so do not invent one; current authenticated Node+attempt+lease are its authority. Grant also must enforce name/wildcard perCapability and pinned maxPerNode against all unreaped relevant occupancy, not merely `limits.total` or previous eligibility. Existing trusted internal primitives may remain but must not be exposed as WS bypass. Actual final signatures will replace the pending section after code delivery; no false implemented API freeze.

Fresh coverage still pending discriminating task-create->ns-config-change->first-grant namespace pin and same-attempt changed machine limits assertions; deterministic snapshot concurrency hook not required (no production torn snapshot found). Integer internal slot mapping is safe only after `protocol.ValidateCancelAck`/accept presence validation at G2; do not mirror wire pointers or skip validator.

## 明确仍未实现/未最终审查

1. 非配置资源原子resourceVersion写CAS，Node根/config共用底层令牌（W1/W5）；Playbook PUT/DELETE已有txn CAS，不能据此宣称其余资源完成。
2. Open格式marker4/必需schema校验（旧schema_version=1结构拒绝）；当前marker缺失边界未实现。
3. A1 typed TaskResult.artifacts manifest/upload登记/引用/GC/下载spool；当前store Task.Artifacts/TaskResult为旧字符串表面，必须后续升级，不能据此改HTTP。
4. G2 route/ws/source/trigger/lease全运行闭环与T3A3finalfields映射；NodeStreamPath现已有，但A2 EnvArtifactDir/A3slotpresence/reject仍待纯Go支持delta。
5. artifact仍有私有ParamEnvName副本，后续切共享函数；其校验ANOTIFY_保留不放宽。
6. 旧in-package tests仍编译失败；S的新外部161PASS不等全store/fullrepo通过。S三个major只是writer修复+独立机械测试，尚需fresh代码复审。

## ACTUAL C2 superseding signatures · 52d58703beac70eecb4dc338f84969cf275c87b1

C2 writer collected accepted(verified) fourfixedstage commands at52d5870; old195tests retained+21new=216, full23.299s. Freshcorrectness/coverage/simplicity fixed52d review running; actualmethods do not imply fullG1/T1 approval. Older21253ed signature snapshot remains historical; use these C2 signatures for changedresourcewrites.

```go
func ResourceVersion(uid string, revision int64) string
func (d *DB) UpdateCapability(ctx context.Context, namespaceID, name string, version int, patch CapabilityPatch, expectedVersion string) (*CapabilityVersion, error)
func (d *DB) UpdateNext(ctx context.Context, namespaceID, name string, version int, next []string, expectedVersion string) (*CapabilityVersion, error)
func (d *DB) DeleteCapability(ctx context.Context, namespaceID, name, expectedVersion string) error
func (d *DB) UpdateSourceByName(ctx context.Context, namespaceID, name string, patch SourcePatch, expectedVersion string) (*Source, error)
func (d *DB) DeleteSourceByName(ctx context.Context, namespaceID, name, expectedVersion string) error
func (d *DB) UpdateNode(ctx context.Context, name string, patch NodePatch, expectedVersion string) (*Node, error)
func (d *DB) PutNodeConfigByName(ctx context.Context, name string, cfg *NodeConfig, expectedVersion string) error
func (d *DB) DeleteNodeByName(ctx context.Context, name, expectedVersion string) error
func (d *DB) UpdateNamespace(ctx context.Context, slug, displayName, expectedVersion string) (*Namespace, error)
func (d *DB) PutNamespaceConfigByName(ctx context.Context, slug string, vars map[string]string, expectedVersion string) (*NamespaceConfig, error)
func (d *DB) DeleteNamespaceByName(ctx context.Context, slug string, force bool, expectedVersion string) error
func (d *DB) ReplacePlaybook(ctx context.Context, namespaceID, slug string, expectedVersion string, files map[string]string, source string) (*PlaybookRow, error)
func (d *DB) DeletePlaybook(ctx context.Context, namespaceID, slug string, expectedVersion string) (*PlaybookDeleteResult, error)
```

Row.ResourceVersion remains internal int64; API/nodeconfig wrappers emit the singleUID+revhelper's string and use only safe-name entries, noGetUIDprecheckthenunguardednamewrite or rawnamecountercompat. NodeRoot/config share node_configs revision; Namespace/config namespaces revision. Source.DisplayName persistedcolumn. Capability.deleted_at internalhidden; livequeries/fold exclude, private getPinnedCapabilityByIDOn(ctx,ex,ns,id) retainsoriginalbody, EligibleNodesForTask consumesTaskUID. DeploymentsForNode retains realdeploy/pinnedbody references; ordinarydeployment reads filterdeletedtargets. CreateCapability reuse doesnotmutatenext/displaywithoutCAS; invalidCreateCapabilityForce removed. C3mustprovideactualpublicauthorizedexecution/task-pinned body/content path+manifest/FK/physicalGC closure, neverresolveoldTaskbyrecreatedname. Fullreport+alltype/patchdetails inverdicts/g1-6c2-52d5870.md andverify/c2-full-report.md.gz.

## ACTUAL C3a superseding signatures · a9496238e34a96e1d5ad0087a34e9f02d92f821d

C3a/R1/R2/WIRE actualfour fixedchecks correctWT passed; finalfresh3 strictimplementationgate majorclear. Supersedes older pendingAuth section and oldGrant/Renew/Complete/Ack signatures above. This is scoped C3a, NOT wholeG1/T1.

```go
type NodeAuthorization struct { NodeID, NodeTokenHash string }
func (d *DB) GrantLeaseAuthorized(ctx context.Context, auth NodeAuthorization, taskID, assignmentID string, proposedAttempt, slot int, now int64) (leaseID, attemptToken string, attempt int, expiresAt int64, err error)
func (d *DB) RenewLeaseAuthorized(ctx context.Context, auth NodeAuthorization, taskID string, attempt int, leaseID string, now int64) (int64, error)
func (d *DB) CompleteTaskAuthorized(ctx context.Context, auth NodeAuthorization, taskID string, attempt int, leaseID string, now int64, result TaskResult) error
func (d *DB) AckCancelAuthorized(ctx context.Context, auth NodeAuthorization, in ReapedInputs, processGroupReaped bool) error
```

OldpublicGrantLease/RenewLease/CompleteTask/CompleteTaskAt/AckCancel deleted; no uppercompat. NodeAuthorization hash MUST come from actual connection-authenticated credential, not fetched-currentDBhash self-authorizing. RESULT-WIRE-AUTH removes plaintext resulttoken not in protocol33e; result checks actualcredential+currentlease/task/node/ns/attempt/expiry/cancel txn. Emit/upload tokens still required on provided-token channels. Namespace pin firsttask input and eachattempt machine snapshot retained.

Budgets inside granttxn: mergedlocal/node total, samecapabilityname crossNS perCapability/nameelse'*', pinned maxPerNode sameNS+name acrossversions; allunreaped/actualreservations, exactownnode/task/assignment/proposed/slot excludedonce. node_instances busy_assignment_id/busy_expires_at internalfacts (no wirefields). Expiryclear only recorded neverexecutedlease-free reservation, never trueexecution. HistoricalAck firstexactReapedAt/phase/error/finishedfacts preserved; repeatscompletefact idempotent.

KNOWN pending actual interface gap: CompleteTaskAuthorized terminal result returns ErrInvalidTransition; no safe exactrecorded-resultreplayACK classification yet. C3btypedmanifest/currentauthhistorysamefacttxn assigned before G2. Never genericerr->ACK or Getprecheck+unguardedmutate. Typedoutputs/uploadregistry/refphysicalGC not yet implemented. Parent approved namespaceforce preciseguard in NAMESPACE-FORCE-HANDOFF, addedC3bowner. G1originalwhitebox/G2/G3/G4 and finaloriginal3 gates still pending.

## ACTUAL C3b superseding interfaces · b46fd2c4d8bd233a056d5313ee40f052ac004d3c

C3b5e typedregistry/ref/force source plus parent-approved fullfacts b46 collectedaccepted(verified) exact4; finalfresh3 strictimplementationgate no majorcritical. OldC3b fieldblocker report retainedhistorical, nowresolved; NOT originalwhitebox/G1/T1 DONE.

Authoritative currentfullGo export api-snapshots/store-c3b-b46fd2c.txt. Upload/manifest/GC methods in prior5e snapshot nowactual with these finalresultfacts; no privatewirecopy.

```go
type TaskResult struct {
    Phase string
    Reason string
    Summary string
    Artifacts []protocol.TaskArtifact
    Error string
    ExitCode *int
    StartedAt, FinishedAt time.Time
}
// All JSONcamelCase, typed emptyartifacts[]; nullableExit!=0; UTCmonotonic-free nanos preserved.
func (d *DB) CompleteTaskClassifiedAuthorized(ctx context.Context, auth NodeAuthorization, taskID string, attempt int, leaseID string, now int64, result TaskResult) (TaskResultDisposition, error)
// TaskResultRecorded vs TaskResultDuplicate; samecurrentauth/currentbinding/allretainedfacts only.
```

BeginArtifactUpload(EmitAuth, protocol.ArtifactUploadResponse) yields servermintedopaque ticket; networkoutsideDBtxn; RegisterArtifactUploadClassified checks actualhash/size and freshcurrentcredential/ns/currentlease/attempt/providedtoken/expiry/cancel then quota/idempotence inonewrite txn, returns actualPublishedObjectID/status. Abort/ExpireArtifactObjects/AckArtifactObjectPurged supply exactprivate-ID compensation/expiry workflow, no user/nodepath fallback. Limits16MiB/64MiB/32 registereditems, repeatno doublecharge. GetTaskArtifactDownload requires actualcurrentTaskresultmanifest refs; 7daybytes expiredmetadata410, 30dayfacts trustedserveracceptedtime, activeupload/unreaped protections.

Complete no plaintext resulttoken (wire33e neverprovidedone); independentlyCURRENTserverNow auth, fullresult JSON exactprecision, originalNode processstart/finish/exit projectedwithoutfakebeforeSpawnstart. Hiddenresult_accepted_at firsttrustedaccepttime drivesTTL; Node reportedold/future time cannotextendlease/revive/GCearly. Duplicatebranch samecurrentNodeAuth/task/attempt/lease and entirePhase/Reason/Summary/Error/typedmanifest/nullableexit/exactnormalizedtimes & retainedrefs/acceptedfacts agrees -> no writes. Missingfacts/oldlease/newcurrentbinding/differentfacts refuses, genericerror is NOT ACK.

Physicalcapability purge onlytombstone UID with actualtask/attempt/manifest/deploy/receipt guards/FKlateproducer safety. Namespaceforce sameCASdelete txn first rejects granted/unreaped/realbusy/unknownNSledger0writes; aftertrustedreap originalsyncownedclear, globalNodes/externalNS/sharedcontent untouched. OrdinaryC2capDelete continuesimmediatehide+pinretention distinctfromNamespaceforce.

Knownacceptedminor fromscout10: firstpath-ordered expired samehash reference canmaskanotherstilllivecopy inGetTaskArtifactDownload; source-derivednotruntimeverified. G3downloadclosure must resolve/test this exactavailabilityedge before finalcomplete; no fullHTTPspool/GC/NodeE2Eclaim. G1originalwhitebox/G2runtime/G3API5/G4/finaloriginal3 remainmandatory.
