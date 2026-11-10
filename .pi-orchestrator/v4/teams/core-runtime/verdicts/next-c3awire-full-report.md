# V4-CORE-RUNTIME-NEXT · C3a wire / exact-total implementation gate

## 做了什么 / scope

- Implementation gate: **PASS for this frozen C3a-WIRE implementation only**. Fresh scoped review/major-clear is still the parent's gate. This is not T1 DONE, not full Go / contract / E2E success.
- WT: /root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
- Base: 8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
- HEAD: a9496238e34a96e1d5ad0087a34e9f02d92f821d
- Commit: fix(store): 对齐真实结果身份授权与总预算判别
- 8 allowed source/test files only, 380 insertions / 50 deletions. No product final diff outside execution_auth.go; no protocol/schema/auth/G2/API/manager changes. No nested agent, push, merge or MR.
- Result removed only the absent attemptToken argument/validation/hash comparison. Current DB node credential / task-node binding / namespace intersection / current attempt and exact lease / non-expiry / no-cancel remain in the same BEGIN IMMEDIATE result-writing transaction. No stored task token hash is substituted for caller evidence.
- Result test callers use issued fixture node identity snapshots or explicit captured minted credentials, not a fresh GetNode hash at completion. The finishedTask test wrapper now receives its creation fixture Node so its result caller does not synthesize authority from the current DB; stats_test has only the two corresponding wrapper caller adaptations.
- RenewLeaseAuthorized and AckCancelAuthorized/M3 body are byte-exact unchanged against the supplied base. EmitAuthorized / provided upload token checks were not weakened.

## 改动文件清单

1. internal/store/execution_auth.go — actual result signature and removal of nonexistent result token comparison only.
2. internal/store/v4test/execution_wire_test.go — new actual result authority / total-only discriminators.
3. internal/store/v4test/execution_auth_test.go — six result callers plus captured credential / no-longer-used grant token variables. M1/percap, own reservation exactness, M3, D2 tests retained.
4. internal/store/v4test/task_test.go — nine result callers; unused grant secrets removed only where no other token test needs them. Existing VerifyAttemptToken security assertions unchanged.
5. internal/store/v4test/task_attempt_test.go — four result callers / unused grant variables. Whole-history/M3 and emit token assertions retained.
6. internal/store/v4test/retention_test.go — two result callers, issued identity passed through finishedTask wrapper.
7. internal/store/v4test/stats_test.go — only two finishedTask wrapper callers.
8. internal/store/v4test/emit_test.go — only result caller uses captured EmitAuth.NodeTokenHash; EmitAuth.AttemptToken checks untouched.

## 实际 Go 签名

```go
func (d *DB) CompleteTaskAuthorized(
    ctx context.Context, auth NodeAuthorization,
    taskID string, attempt int, leaseID string, now int64, result TaskResult,
) error
```

No plaintext result attemptToken. TaskResult remains the existing store shape in task.go:807-813 (Artifacts []string); typed protocol artifacts/registration are the still-pending C3b task, not implemented or faked here.

## 测试覆盖 / 新增测试名

- TestCompleteTaskActualResultWireAuthority: grant secret deliberately not returned to result caller; captured old minted session is rotated and rejected, current captured session succeeds; exact result/reap times and pinned D2 snapshot checked.
- TestCompleteTaskActualResultRefusalsZeroFacts: wrongCredential, wrongNode, wrongLease, oldAttempt (old unreaped + new running in another slot), authorizedNamespace, declaredNamespace, cancel, expiryEqual, expiryPast. All task / complete historical attempt / node slot columns are compared before/after each refusal, not selected facts only.
- TestGrantTotalShrinkCountsOldHigherSlotOnly: total2 -> old live A slot1 -> shrink total1 -> B legal empty slot0 refused only by total; perCapability/maxPerNode=10. Rejection has attempt0/no attempt rows/free unchanged slot0; old task/attempt/slot rows exact.
- TestConcurrentTotalShrinkGrantsExactlyOne: total3 -> old live slot2 -> shrink total2 -> two new tasks simultaneously request legal empty slots0/1, perCapability/maxPerNode=10. Exactly one grant, loser attempt0/no attempt row/slotfree, old rows unchanged and unreaped count + physical busy count remain 2.
- Existing TestConcurrentNarrowBudgetGrantsExactlyOne remains separately perCapability-only. No slot-range weakening or fake busy release was used. Full v4test includes the existing M3 whole-fact/exact-time and D2 cases.

## actual duplicate result ACK / 精确 Go 接口 gap

**No safe duplicate-result ACK path in this entry; fail-closed rejection is preserved.**

- execution_auth.go:657-658: CompleteTaskAuthorized returns only error. First valid completion returns nil; a terminal result replay reaches execution_auth.go:708-710 and returns ErrInvalidTransition unconditionally.
- That terminal branch precedes task-node/attempt/lease/namespace binding and same-facts comparison (binding checks start at 714 / 718; ns check at 729). Therefore upper G2 must NOT convert any ErrInvalidTransition into a duplicate ACK or classify any terminal GetTask precheck as sufficient authority.
- task.go:1194-1196 finishTxGuarded clears current task lease_id/lease_until; precise historical attempt lease must be used for a future replay classification. Current entry does not compare recorded Summary/Artifacts/Error/Reported/history facts for replay.
- No new replay bypass/Get-precheck+unguarded write was added. Existing same-terminal replay test still expects ErrInvalidTransition. Historical reaped ACK idempotence remains the separately authorized M3/AckCancel path and was byte-exact preserved.
- Next step: parent serially specifies safe same-current-auth + task namespace + precise historical task/attempt/lease + exact recorded typed result facts replay classification in C3b/G2. Do not blanket-ACK terminal errors, change wire, or borrow DB current hash/stored attempt hash as caller identity. This planned reportable gap is not hidden by the four green scope checks.

## 红绿 / exact source restoration

1. Initial result wire red used the old CompleteTaskAuthorized signature with an empty absent wire token (no grant secret fabricated). It failed at the old argument gate with lease_mismatch rather than the expected current credential rejection; the subsequent current-credential assertion was not reached in that red run. Only the result call shape was migrated to the approved new signature, then the same named test and all refusal cases passed.
2. Total mutation modified exactly one expression to false && totalBusy+1 > effectiveTotal; no slot range, perCapability, maxPerNode or reservation code changed. Single test accepted B (err nil); concurrent test admitted both requests (success2/rejected0). Both are discriminating reds, not range/percap confounds.
3. Restored execution_auth.go byte-exact from its pre-mutation new-signature source: SHA256 e7235e70b4f9865d47dfd3ab0dab32aa99153933e0c34d6e991fe28ecb5da1d5. sha256sum -c and cmp both pass. The three temporary-owner files were never changed and compare exactly to base:
   - node.go 5a7ddc839b13b6d3a5964df0ba73d9762dcb5e9a402b9c143e9f7a60223d0eb6
   - task.go a16efe434591f9972a49cce4e0d54d452b00214f03d99f9976780bd6d66b1d9c
   - task_attempt.go e7baf9798572934bf088c1a9dbd04e3a15dd5f62da99a3885b240ee979768637

## fixed4 实际结果 / 验证方式

All four ran at final HEAD a9496238e34a96e1d5ad0087a34e9f02d92f821d with absolute cd to the correct WT and pwd/HEAD/cmd/exit in each full log. Actual exits **0 / 0 / 0 / 0**. Full stage test ran once after all changes: v4test24.723s / artifact2.379s / protocol0.045s.

Every verification command used the fixed project stage entry and timeout60. No command exceeded 60 seconds, so no long-command process was launched or left uncollected. Available child tools did not include pi-lens or pi-async (actual ALL_TOOLS enumerated); code was read in bounded parallel batches and the required Go vet/build checks supplied mechanical diagnostics. No extension/settings/env-file/harness workaround.

Raw output is reproduced below, including initial expected red and compile/vet migration failures. Lightweight unused-token migration feedback was fixed, including restoration of the accidentally altered earlier VerifyAttemptToken declaration. No security assertion was deleted/weakend; final full suite passed. No failed full-suite retry.

## MR / PR

N/A — explicit NoPushMergeMR task instruction; local source commit only. Final source diff saved at /tmp/anotify-core-next-c3awire-final-diff.log, scoped to supplied base8fd44b9...HEAD (not unrelated earlier team commits), reviewed file by file.

## 完整 remaining closure (all still pending, not reduced by C3a PASS)

- Parent fresh independent C3a review and major-clear remains required before serial C3b admission.
- C3b: typed shared result/attempt manifest persistence; current-attempt upload registration and final reauthorization; 16MiB/64MiB/32-count transactional quotas/idempotence; cross-task/ns/attempt/path/hash reference refusals; exact first/repeat/conflict; partial manifest errors; actual byte7day vs fact30day expiry and active upload/GC protection; true physical capability UID/task/attempt/manifest/deploy/receipt reference guards; same-name recreation/shared-digest safety; shared protocol.ParamEnvName call only. Result wire credential rules above supersede old text mentioning plaintext result token; upload/emit token constraints do NOT change.
- G1-6TEST: named original in-package test migration (not bulk retirement), injected actual baseline DDL/ledger/marker transaction rollback whitebox, pool/CAS/IDs/references/retention/field roundtrips, retained auth/admin/password/passkey BackupEligible/device/push tests; migrate only obsolete builtin-seed assertion, full exact test mapping.
- G2: real shared-protocol runtime chain / authenticated NodeSession identity snapshots and atomic entries; dispatch accepted real slot / budget/backoff / stale binding / configuration; lease/cancel exact history and physical reap; safe result replay classification/correlation gap above; emit business identity; unified source ingest/preview/cron; explicit deploy/content authorization; true historical structured log routing; only retired executor/old-node paths retired.
- G3a/b/c: namespaced resource envelopes/status/CAS/action wiring, full Playbook12-cell transactions and sensitive historical receipts, real upload spool bounded stream/publish/compensation/final authorization and 7day410/30day facts, retained protected domains/notifications, five actual API truth files + full path/method/operation/ref/handler/auth checker parity.
- G4: approved final T3 real Node source handoff/build/install/binary distribution; tooling/devseed/nonfrontend E2E/harness adaptation without fake phases or frontend changes; actual Node dispatch/cancel/reap/emit/deploy/contract/auth quick suites; final unchanged **go test ./... -count=1 + make contract + make e2e-quick**, plus parent independent full CI/build/E2E/web and fresh full review. These original all-system gates were NOT run or claimed green here.

The following five precise frozen remaining task cards are copied verbatim as completeness reference (read-only originals unchanged). Any old result attemptToken wording is superseded only for task.result by parent RESULT-WIRE-AUTH.md, never for upload/emit.

### Remaining frozen card: NEXT-C3b.md

# NEXT C3b actual typed outputs / upload registry / references

Serial only after C3a collected/freshmajorclear, source checkpoint and actualentryinterfaces must be included atdispatch. Full remainingstore/whitebox/G2/G3/G4 retained. No whole-repo recon/C1C2reimplementation. D4/A1/A2/A3/D6/RESOURCE-DELETION/RESULT-AUTHORITY authoritative.

## Owner upper bound (exact launch list before assignment)
Original G1-6C3: internal/store/task.go,task_attempt.go,emission.go,node.go,retention.go,schema.sql,migrate.go; artifact_manifest.go(new),execution_auth.go; v4test/*_test.go onlytypedmanifest/calleradaptation/referencecases; internal/artifact/artifact.go,artifact_test.go ONLY protocol.ParamEnvName single-sourcecall. No protocol/upperHTTP/resultspool/userIO yet. If actual physical capabilitypurge resides outside originalpaths(ref.go/orphan.go/etc), reportexactfile/function first; manager must record explicit serialreassignment withinT1bounds before writes, don't silentlyexpand.

## Actual interfaces/behavior
- Task/attempt artifactmanifest is actualsharedprotocol []{path,sha256}, persistedroundtrip/camelCase/empty[]. No legacy []string DTO or fake[] success. Result currentauth/execution+registeredmanifest references and terminal mutation samewrite txn, no borrow knownhashfromotherTask/attempt/ns.
- NodeAuthorization sessionNodeID/TokenHash, currentattempt/lease/token/ns/expiry/cancel checks inside finalwrite transaction. Beginprecheck doesnot holdDBwriter acrossnetwork. Registration carries task+attempt+logicalnormalizedrelativepath+64lowerhex+actualsize+publishedsystemidentity+time. Finalregistration reauth currentnodehash/ns/execution afternetwork; first201 vs exactrepeat200, samepathdifferenthash409, crossTask/attempt refdenied. Use protocol types/constants no privatewiremirror. No fakeupstream success.
- 16MiB/file/64MiB/attempt/32logicalitems actualtransactional enforcement/idempotentrepeatnotdoublecharge; malformedhash/path/duplicates rejected; currentNode rotate/cancel/leaseexpiry betweenprecheck/finalregister reject with0facts. Path neveruserhostpath, systemidentitynotuncheckeduserinput. HTTP spoolstreamhandler afterG3, not claimendtoendstoretests.
- Manifest/taskfacts30day vs systembytes7day. Expiredcopy metadata retainedto410 later, no nodefallback/userfiledelete. Actualuploadinprogress/referenceprotection and registrationGC races: no longDBwriter. Expose factualexpiredmetadata/get/downloadref+GCoperations forG3, notdummyregistry.
- Capabilitytombstone physicalGC protected by actualallTask/attempt/manifest/deploy/receiptrefs viaFK or strictsame-transactionguards. Taskpin UID immutable after delete/recreate, never bynamepurge/shared digest collateral deletion. Do not use C2 tombstone alone as physicalprotection proof. Facts allregistered refs and userdelete semantics unchanged.
- artifact private ParamEnvName implementation removesonlyduplicate, callsprotocol.ParamEnvName; preserveANOTIFY_ restrictions/collisions.

## Required tests
TypedmanifestJSON/reopen/roundtrip; currentauthorization/crossTask/ns/attempt/resultunuploadedref; uploadpath/hash/size/idempotence/conflict, all3limitsincludingconcurrentquota/ownrepeat; precheck->rotate/cancel/expiry finalregister0write; registeredpartialmanifesterror preservedno fakeempty; 7daybyteexpiry/30dayfacts+activeuploadprotection; realcapUIDTask/attempt/manifest/receipt/deployGC guards and same-name recreation/shareddigest safety; no userworkdirIO. Actualredgreenfornewmajor behavior, full logs/realexits, no skip/weaken.

## Fixed4 stage verify
Each pwd then absolute cd thisWT; gofmtstore/artifact empty; go build store/artifact; go vet v4test/artifact; go test v4test/artifact/protocol-count1, officialdirectGoenv. Exact originalfullGo/contract/quick finalunchanged. Shorttimeout60full logs; longnamedasync+collect/frozenreadonlyclose ifregistryclosed nohack. Sameissuemax2retries stopparentwithfiles/errors/hypothesis/nextstep. ReportactualnewGo signatures/changedfiles/newtestnames/logs4exits/sourceHEAD/refguardsproof/pendingHTTPwhiteboxG2-4. NoPushMergeMR/nestedwriter/settings/env/node_modules/otherteamwrites.


### Remaining frozen card: G1-6TEST.md

# G1-6TEST · original inpackage test closure + retained-domain regressions

Serial after domain C2/C3 actual interface updates; no concurrentwriter. Known original inpackage compile failures are NOT reason to delete tests wholesale. No broadrepo baseline/recon/oldsession. Facts TEST-MAPPING.md and original allGoFAILlog established. Current actual api snapshots authoritative after C3.

## Exact files owner
All tracked internal/store/*_test.go and internal/store/v4test/*_test.go (tests only), new internal/store/v4_whitebox_test.go; internal/auth/password_test.go ONLY oldbuiltinseeding assertion in TestRegisterWithPassword_Success; .pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md temporarily serial-owned by this member (centurion won't concurrently write). No productionGo/schema/migrate/protocol/source/route/server files, no otherauth/passkey/device production or tests outsidelisted. If genuine productbug stops regression, report named test/actualerror/productline and don't weaken assertion; parent/centurion then assigns separateproductrepair.

## Closure / migration discipline
- Enumerate each original test name (base0e0ff0f/7ab4ccf alreadylocal Git objects), map kept/currentrewrite/newV4case or narrowly retired pure v3 ports/priority/sticky/handler/centralexecution/historicalmigrationchain semantic. Preserve whiteboxtransactions/pools/CAS/ids/stats/references/retention/schema/rollback, fieldroundtrips and full standalone auth/admin/passkey/password/deviceregessions. No bulkdelete/skip/legacyDTOstubs/emptytests.
- Current public tests v4test augment not replace unexamined originals. Migrate fixturefields to actualcurrent v4 constructors/pins/attempts/sources, no forgedLeaseSlot shortcut when testing executionauthority. Edge now projection not directmutable user entity; currentplaybook receipts/12cells/D3dryrun transaction facts tested, namedmapping explicitly.
- Original migrate parse/gap/failure hook/transactionrollback whitebox reproduced with actual internal applyMigrationTx/set/hook (adapt to signature). C1-R view-only collision fixtures now pre-refusal are not midtxnrollback; add real injectedbaselineDDLfail/ledgerfail/markertransactionrollback via whitebox local migration call, not exposing testhooks in product or pretending prerefusal proves rollback. Newempty baseline/reopen/unsupportedformat/badmarkerlayout preserved.
- Auth password_register oldseed assertion only migrates expected retiredbuiltin meaning; keep user/passworddefaultnamespace/recoverycodes/session/adminroles/errorrollback/security assertions; all passkeyBackupEligible and devicetags/update/pushdomain untouched.
- TEST-MAPPING becomes exactcase-to-case outcome, including retired builtin_test names recoveredfrombase, cannot say fileonly/lowvaluedeleted; include actual commands PASSFAIL and remaining upperG2/G3 blockers. No unrelated design/parentSTATUS changes.

## Fixed verification
1 test -z "$(gofmt -l internal/store internal/auth/password_test.go)"
2 envGOPROXYdirect/GOSUMDBsum.golang.org/GOTOOLCHAINauto go vet ./internal/store/... ./internal/auth/... ./internal/push/...
3 same env go test ./internal/store/... ./internal/auth/... ./internal/push/... -count=1

No allGofullrepo until G2G3closed; these actual packages mustpass before closure. No bypass. Shortindependenttimeout60full /tmp/anotify-core-recovery-oldtests-*.log; true long command via frozen mechanicalclose extension exactcdWT no sourceactive, parentescalateifunavailable. Twofailedretriesperissuemax, actualblockingbug reportwithpath/output/hypothesis/recommendation. NO PUSH/MERGE/MR/nestedwriters/settings/env/userdata. Commit test(store): 闭合 v4 原测试与保留域回归. Exactfiles/testnames/mappings/3commandsactualexit/reportnotT1DONE.


### Remaining frozen card: G2.md

# G2 · actual v4 runtime chain (serial after complete G1 store closure)

Frozen fullT1 G2scope no shortcuts. Read current real EXPORTED/API snapshots, CONTRACT/D4/A1/A3/W1W6 and actualsharedprotocolfinal33e definitions; no rediscover oldrepo/baselinefail. Single writer only.

## Exact ownership
internal/route/**; internal/ws/**; internal/lease/**; internal/source/**; internal/trigger/**; internal/logging/** (only protocol/log-routingadaptation, retainindependentlogger); internal/executor/** delete retiredcentralcapabilityexecutor only; cmd/agent/** delete oldnodeproduct only (T3anodeuniqueowner). .pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md temporarilyserialowner for G2 namedold->newtests; allstore/protocol/artifact/server/api/tools/T2/T3files read-only. Store atomicentry missing/contradictory -> report exactsignaturegap, do not broadenstorewrites/precheckfakegreen.

## Runtime and source behavior
- route pure currentcapability/source next/targets projection, params payload $(payload...) mapping with actualartifact specvalidation; deleteport/priority/sticky/legacyhandler paths, no oldAPI/WSaliases. Taskversion/digest/params/pinnedns stable; prune receipt real pertargetreason; nothingrunscentralexec. Source manual/webhook/cron same normalized kind/config/targets authority. Dryrun0writes/in-memorypreview, omittedpayloaddefaults valid.
- Dispatch onlyassignmentId/proposedAttempt/assignmentExpiresAt30sec; producer maps DBUID to protocol node=name, sessionbody cannotchooseidentity. Candidatefacts hints; GrantLeaseAuthorized actualcurrenthash/ns+assignment/proposed+slot+budgettxn authority. Acceptslotpresent valid, staleconfig|slotunavailable noattempt/boundedbackoff/nohotloops, sameassignmentidempotence, expiredoldrequest no newreservationdamage.
- NodeSession snapshot authenticatedNodeID+tokenhash, every inbound authorized write currentDBcredential validation transaction; oldtoken/sessionrotate cannotgrant/renew/result/emit/upload. hello/configapplied namespaces+vars/limits+revision/hash/instancespureprotocol, missing/emptybusy unknown, exactslotobservations only, no phase/offline fakebusyrelease. appliedmachineonly, tasknamespaceinputs memory. Upsertbudgets preserveoldbusy.
- Leasegrant sameacceptedslot+assignment only; renew20s/TTL60 with ACK, expiry==now reject. CompleteTaskAuthorized actualauth+manifest/lease/token/cancel, resultack correlation; oldresult/emit neverrestore. Cancel producer exacttask/attempt/lease/slot, receiver protocol.ValidateCancelAck before sessionidentity and historicalCAS viaAckCancelAuthorized; currentcancel finalizes atomically, oldack onlyhistoricalreap, false/missing/negative/wrongbinding no release, repeatexactidempotent. Do not wire bareAckCancel/ReleaseSlot asauthority.
- task.emit/ack directlysharedDTO+Decode/direction; RPCrequestIdonlycorrelation, authoritativeEmitAuthorized txn thenbusinessid, no seqautoidempotence; sameidpayloadconflict409, noideverycallnew, staleauthorizationcannotreplayoldid. Output exactHTTPemitDTO, no independentmirror.
- Source ingest unifiedreceipt eventId/traceId/duplicate/tasks/pruned, realatomicaccept/tasks/deliverywindow; cronfired/effects errorhandling no swallow. Lease/retention/stats actualstoreinterfacesnolegacyaliases; oldcmdagent/executor retirement retainsindependentauth/broker/push/device/notify.
- Manualdeploy references namespace/name/version/digest, onlineProducer/offlinependingintent replayonlyexplicitdeploy, receiptfact authorizedcurrentNode. No fullfilesinWS/autoreconciler/installautodeploy.
- Logs(requestactualattempt) send exacthistoryattemptNode, validate taskId/attempt/requestId reply; expose structured actualsequence/stream/text/at withoutinventtimestamps/crossstreamorder, previoushistorynoprev[] true; ifprotocolonlyrawstringscannotmeetW6 reportexactgapparent notfabricate. Keep upgradeframes bysharedprotocol/T3existingcontract only.

## Tests / gates / delivery
Namedoriginaltests migrate onlyv3retiredsemantics, preserve brokerWS/device notifications unrelatednodeprotocol. Test truehttptestWS shareddecoderframes/grantnoearlyexec/currentidentityrotate/nsrevocation/cancelwrongmissingoldlease+slot/expires/rehello/disconnectbusy/noid+idemit/historylogs/offlinedeploy/params-sourcepreview. Socketsettimeouts/contextdeadlines/probestimeout60, no nakedreceive/pollloop.
Fixed launchverify: gofmt internal/route internal/ws internal/lease internal/source internal/trigger internal/logging; envGOPROXYdirect/GOSUMDBsum.golang.org/GOTOOLCHAINauto go vet ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/...; same go test those packages -count=1; same go test ./internal/store/... ./internal/artifact/... ./internal/protocol/... -count=1. Originalgoall/contract/e2equick finalstillrequired, upperoldserver compileblockednotfakedbyDTOcompat. Outputexactpublicruntime signatures/interfacecardactual, changedfiles/newtests/namedretirements/eachgateexit/logs, don'tclaimT1DONE. NO PUSH/MERGE/MR/nestedwriters/settings/env/userdata. True>60cmdonlymanagedextensionverifyfixednoflivewriter, shorttimeout60logs/tmp/anotify-core-recovery-g2-*.log,2failedretriesmaxthenblockreport.


### Remaining frozen card: G3.md

# G3 · full HTTP namespace resources / protected domains / artifact spool / five specs

Serial after G2 actual interface closure. Frozen fullparentCONTRACT/W1-W6/ARTIFACT/SLOT/API-SPEC-HANDOFF; no oldcompat endpoints or shape fallbacks. Parent API1 read from /root/code/anotify/.pi-orchestrator/v4/API-SPEC-HANDOFF.md (authoritative read-only; don't recreate root authority). Same solewriter slot may divide G3a(resources+actions), G3b(spool/system/protectedregressions), G3c(API5checkers) as exact permitted files below in serial rounds, no parallelwrites. All StoreRuntime methods use actual finalEXPORTED signatures.

## Ownership upper bound for exact G3 round launches
internal/server/**; internal/api/**; internal/auth/** only assembly/retiredbuiltin expectation migration and preserveauthfeatures; internal/authn/** only newnode/usernamespace authwiring; new internal/resultspool/**; api/resource.yaml,api/events.yaml,api/agent.yaml,api/ingress.yaml,api/auth.yaml; api/openapi.yaml retirement or explicitlygeneratednon-authoritative aggregate ONLY; scripts/check_contract.py,scripts/check_openapi.py,scripts/check_envelope.py and any actualexistingcheckerbasename require explicitlaunchlistfirst; docs/contracts/CONTRACT.md. Store/route/ws/lease/protocol/nodeproduct/T2sitegen/UI read-only; if missingactualmethodreportgap not GetCheckCAS/productfake DTO.

## G3a resource/status/action closure
- Only private /v1/namespaces/{namespace}/capabilities|sources|playbooks|tasks|events|traces|edges|deployments; globalnamespace/node resources; /v1/hooks/{token}, nodeprotectedcontentstream. RemoveoldflatresourceAPI aliases. Users/Bearer scopes perns; explicitbody/pathnamespace mismatch400, directstatuswrites400, metadata.namevalid, version/name@ semantics.
- Exactkind envelopes + KindLists; spec/status separate, computedresolved/observed arrays/detailsfromfactsnotcounts/persistedstatus/reconciler. camelCase, realempty[] notnull. CapabilityversionlistdescendingCapabilityList/noextrakind, includeBodyfilesmap; digest/nextunchangedidentity. SourcehookUrl+secretSet authorizedread no token/secret echo; PATCHtargets+CASprojectiontransactionactualstore. UnifiedStatuscode/reason/details.
- W1 PUT/PATCHmetadata.resourceVersion, DELETEqueryonlystring; missing400required/stale409conflict details.current.resourceVersion/404missing; no If-Match/bodyalias or GetCheckunconditionalupdate. Node root/config sametoken/fact; Namespaceconfig/systemwrappers, batchpernodeatomicpartialsuccess no Playbookatomicconfusion.
- W3 Playbook spec.files xor spec.source envelope, playbook.yamluniqueroot; originalrawincludeBody/provenancestatus; pureRender/noDBconflictread; install12cellatomic/dryrun0writes/ordinaryfreshreceipt/historyfolding/lastInstall/secretmask/unapplied, conflict400fulldetailsvaluesDigest/conflicts/hint nottrim. No automaticdeploy/uninstallcascade.
- Node offlineclusterPOST thenexplicit usertokensaction NodeToken{node:{name},token} with longidentity/onceplaintext/rotate invalidatesoldsession; missingnode404/noimplicitregister; no querycredential. Manualdeploy persistentexplicitintent + onlineproducer/offline replay, directArtifact NodeGETauthorized by currenthash+nsintersect+explicitthisnode target only, no scriptsWS/no genericreconciler.
- W6 sourcefire/dryrun exactpayload defaults/output, dryrun0persistentwrites, unifiedingressreceiptdeliverydupidentity/tasks/pruned. Consoleevents explicitns+limit/opaquecontinuefilters bound, EventList metadata.continue; logs actualhistoricalattemptnode+replycorrelation+structuredlines/no fabricatedtimestamps, previousnone[]; emissions exactlatestfirsttypeditems/pagination/id notlegacyport.

## G3b A1 spool and retained protected system
- NodePUT exactprotocolpath/headerconstants; BearerNodeidentitypluscurrentattempt/lease/token/ns, before networkprecheck then boundedstreamcopy systemtemp16MiBfile/64MiBattempt/32entries/hash/path. No DBwrite transactionheldduring network; finalatomicNodecredential+attemptauthorization and registry/resultmanifest. Safe systemroot no userpath reads/deletes; temp/publish/DB failure compensation and orphanTTL/activeupload exclusion. Nodehumanbodyselected identitynevertrusted.
- Node upload201first/200exactrepeat/409pathdiffhash/400pathhash/413limits real. UsernamespaceTaskdownload actualresultmanifestreferenceonly rawbytes200 safebasenameDisposition; unknownref404, expiredcopymetadata4107day/taskmanifest30day; nodedown/userfile neverfallbacksource. Duplicatehash elsewhere noauthbypass, interruptedcancel/expiry/rotate preventsregistration, logsheaders no credentials leak.
- Retaincompleteauth/password/passkey/recovery/CLIkey/session/admin/device/VAPID/broker/independentnotify/push assembly/shape/security. Removeonlycentralcapabilityexecutor/notifyphone builtinseeding, not independentpushproduct. No authregressiontest deletion; productbugsreportedfixnotweakenassertions.
- Serverruntimeconsumespureprotocolhub and actualrouting/source/lease APIs; shutdowngoroutines bounded/readtimeouts/GC protected. Health/statics etc preserved but frontendexclusivefiles nevermodified.

## G3c five actual APIs/checkers (no singleopenapi fallback)
API1 exact5 names only:
- resource.yaml: sixresourcesCRUD/status,versions/deploy/Playbookactions/config/Nodeusertokens/actions/Taskactions/logs/emissions/userartifactdownload/edge-deployprojections/publictemplates.
- events.yaml: readonlyEvent/Trace/console/stats.
- agent.yaml: NodeidentityGETbinary/stream/capabilitydigest and artifactPUT; T3protocolframe/DTOschema paritysingleGoauthority. Nodenodecontent/upload stream exceptions nofourkeyrequirement.
- ingress.yaml: exactly POSTns/events, POSThooks/token, POSTns/source/name/fire, POSTns/sources/dry-run, POSTns/source/name/dry-run. No new/ingress prefix or publictaskemitHTTP.
- auth.yaml: allretainedauth/passkey/password/recovery/keys/CLI/session/admin/device/VAPID/notificationlegalshape.
(method,path) globalunique; operationIdglobalunique; samepathGETvsPOSTmergebymethodnotlastfilepathoverwrite; real/full/v1pathsserversnoextraversions; valid5filecrossrefsresolve/no sixthtruthfile/oldfallback/skipfixtures. Realhandler+DTO/auth parity, accurateexplicitnonresourceexceptions. Updatecheck_contract/A6digest/schema/projection/newnoexecutor invariants, notdisablingchecks togreens. CheckOpenAPI and envelopecheckers validatefullpaths/authnamespace/resourcefields/Status/cursors/actionexceptions, honesterrors.

## Gates and evidence (fixed per exact round)
HTTPsource rounds: gofmt applicableinternalpackages; envGOPROXYdirect/GOSUMDBsum.golang.org/GOTOOLCHAINauto go vet ./internal/server/... ./internal/api/... ./internal/auth/... ./internal/authn/... ./internal/resultspool/...; same go test those packages -count=1; endG3whole go test ./... -count=1 + make contract fixed true targets. Spec round include makecontract + Pythonchecker syntaxlint and complete5file ref/operationparity checks. Actualredgreenhttptestcounterexamples: namespaceisolation/statusreadonly/CASrace wrappers/Nodeidentityrotation/contenttargetauth/12cellinstalls/source0dryrun/attemptlogs/emissions/uploads(cancel/expiry/rotatehashpathlimits)/crosstaskdownload7day41030daymanifest/userworkDirsafe/authpasswordpasskeydevice. No skip/stubs/legacylayer; finalG4E2Equick+parentfullCI/build/E2E/webunchanged. NO PUSH/MERGE/MR/nestedwriters/settings/env/node_modules/userdata. True>60commandsfrozenmechanicalcloseextensionverifyexactcdWT/noactivesourcewriter; shorttimeout60full/tmp/anotify-core-recovery-g3-*.log,2failedretriesmaxthenconcreteblockreport. Returnexactpublictypes/API5paths+HEAD/paritylogs/newtests/changedfiles/commands/exits/risks, notteamDONEbeforefixedall3+freshreview.

## Parent later receipt-sensitivity checkpoint (not confirmed Go bug / not C2 scope)
T2 static review raised writeOnly propagation through unapplied declared/actual and upgraded.from/to/reused identity diagnostics; historical receipts must preserve their own sensitivity/provenance, not inherit current template. Go BuildReceipt/history/G3 diagnostics need corresponding real counterexamples. Mask rendered locations by provenance, never global substring-replace entire Status/receipt (secret value `edit` must not corrupt static action `edit_capability`). Raw render and valuesDigest use real values; Source persistent credential GET masking remains its separate domain. Do not persist raw receipt/values JSON backups. Parent/T2 cases still partial/static; do not claim Go exploit reproduced or rewrite C2 now.


### Remaining frozen card: G4.md

# G4 · actual tooling/seed/nonfrontend E2E and final original gates

Serial after G1/G2/G3actualclosure, fixedderivedinterfaces/API5/W1W6/A1A2A3. T3realNodeproductdependency stillPENDING untilparent finalREWORK2+freshreview+original3gates/clean-sourcecapture. Parentwillhandoff onlycmd/anode/**,Dockerfile.anode,scripts/install-anode.sh,docs/anode-install.md+necessarydepswithaccurateSHA; ourprotocolfinal33ealreadyimportednotreapply. No unapproved temporaryNodeproduct/cherrypickwholeteam/binaryfakegreen. T1 owns consumers/tooling, T3sources readonlysupport; Nodefinding parentnotprivatefix.

## Exact launch files upper bound (onewriterserial)
Makefile,Dockerfile,Dockerfile.agent(retire),docker-compose.yml,docker-compose.dev.yml; cmd/server/**,cmd/devseed/**; scripts/e2e/lib/{harness,nodes,wsclient}.mjs,scripts/e2e/run_all.sh, allscripts/e2e/suites/*.mjs EXCEPT T2exclusivefrontend/i18n/i18n_coverage/ui_shift/ux_regression_19_23.mjs; scripts/install-agent.sh(oldretire),scripts/check_agent_skill.py(oldretire/migratecallsactualT3anode),scripts/{dev.sh,devseed?}(onlyactualnamesexplicitlaunchrequired),existinginstall/buildtest helpersnotT3/t2; README.md,DEVELOPMENT.md,E2E_TESTING.md,docs/agent-install.md(retire),docs/contracts/CONTRACT.md,.gitignore(newbinaryignoreonly). Go dependencyfilesonlyexactnecessarydeclaredauthorised. No T2web/web-src/sitegen/fe-preview or T3sourcewrites, parentauthoritydesignsettings/env/node_modulesforbidden.

## Tooling + actual tests
- Buildrealanode/server, binarydistributionNodeGETnode/binarycurrentidentity, nooldanotify-agentaliases/agentservice/confignames; install onlyT3canonicalanodeinstallsemantics, Dockercomposecorrectnames/environmentpreserved withoutediting.env*. Oldcentralexec/agentretired but independentlyretainedpush/auth/notificationstilllive.
- Devseed currentv4constructors/API/envelopes, scripts/sessionauthanddefaultns legitimate. Demo realWS/realNodewhenintended, no forgedgreenTaskphase/no centralexecutor. Paths/UIAPIconsume5resourcecontractsactual, no legacyfallback.
- Migrate nonfrontendharness/WSfixtures to sharedactual10frames +A3/config/emit/deploy/logs, correctnamespaces/resourceCAS/tokenactions/camelCase/status. Keep originalauth/password/passkey/devices/push/security suites and assert realproduct behavior. Namedold->newmap, retireonlyports/priority/sticky/handler/builtinlegacyexclusive, no skip/weakassertions/emptynewsuite.
- E2Equick must exercise realNode dispatchacceptgrant/expiry/cancelphysicalreap/sourceemit/deploy/digestcontents andcontract/auth gates asoriginalselection; cannot replacewithsmallstoretest/fixtureWSonly. FullparentE2E/UIcrosschecks stilloutsideT1final3, no scope reduction. Tests/probeseachdeadline/sockettimeouts, gracefultermination/contextcollect no nakedrecv/pollforever.
- NoT2source/generatedfilechanges justtoforceharnessgreen. If makee2equick requires missingapprovedT2generatedassets or finalNode dependency, askparentexactsupportartifact/SHA andrecordtruepending—notskipbuildsteps orfakeweb. RealNodeproduct frozenimportsafeonlynoactivewriter.

## Mechanical final original3 (exact)
1 GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./... -count=1
2 make contract
3 make e2e-quick
plus gofmt/vettypecheckrelevantallGo rounds. Use frozenread-onlymechanicalcloseworker exactverifyarraywithcdthisWT and extensionreceiptforlonggates; no sourceactivewriter, generatedbuildoutputsnotconcurrent. It is NOT async restored. Supervisorverifying also exactlegion-verify three commands; allgreen+machineDONEthenfinalreport. If mechanicalclose/longserviceadmission genuinelyblocked provideexactcommit/cwd/cmd/expectedbudget/rootmanagedrequest, no unboundedbash orsplitcoverage. Max2failedretriesthenreport/rollbackcheckpoint.

Fresh independent correct/regression/coverage/simplicity fullT1scope, strict okfalseifanymajorcritical, no recon_completedfakepass. Fixmajorthroughmachineimplementing/serialre-review, allworkerscollectedfirst. Localbranchcommitonly noMR/pushunlessfinalparentpermission, no merge. Finalfiles/commits/mappings/verdicts/all3actualreceipts/risksandHEAD,parentcrossrealCI/build/fullE2E/webneeded. Only actualDONEverified or truly noncontinuablesafelysettledBLOCKEDfinal; don'tfinal 'waitingwriter'.


## 验证输出原样 / full raw evidence

### /tmp/anotify-core-next-c3awire-red-result-wire.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run ^TestCompleteTaskActualResultWireAuthority$ -count=1 -v
=== RUN   TestCompleteTaskActualResultWireAuthority
    execution_wire_test.go:113: rotated captured session must reject: lease_mismatch: result 必须带 task/attempt/lease/attemptToken
--- FAIL: TestCompleteTaskActualResultWireAuthority (0.04s)
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.043s
FAIL
exit=1
```

### /tmp/anotify-core-next-c3awire-green-focused-final.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^(TestCompleteTaskActual|TestGrantTotalShrinkCountsOldHigherSlotOnly|TestConcurrentTotalShrinkGrantsExactlyOne)" -count=1 -v
=== RUN   TestCompleteTaskActualResultWireAuthority
--- PASS: TestCompleteTaskActualResultWireAuthority (0.03s)
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/wrongCredential
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/wrongNode
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/wrongLease
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/oldAttempt
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/authorizedNamespace
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/declaredNamespace
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/cancel
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/expiryEqual
=== RUN   TestCompleteTaskActualResultRefusalsZeroFacts/expiryPast
--- PASS: TestCompleteTaskActualResultRefusalsZeroFacts (0.19s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/wrongCredential (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/wrongNode (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/wrongLease (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/oldAttempt (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/authorizedNamespace (0.03s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/declaredNamespace (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/cancel (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/expiryEqual (0.02s)
    --- PASS: TestCompleteTaskActualResultRefusalsZeroFacts/expiryPast (0.02s)
=== RUN   TestGrantTotalShrinkCountsOldHigherSlotOnly
--- PASS: TestGrantTotalShrinkCountsOldHigherSlotOnly (0.02s)
=== RUN   TestConcurrentTotalShrinkGrantsExactlyOne
--- PASS: TestConcurrentTotalShrinkGrantsExactlyOne (0.03s)
PASS
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.274s
exit=0
```

### /tmp/anotify-core-next-c3awire-red-total-only.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
mutation=total enforcement only disabled; slot range/perCapability/maxPerNode unchanged
--- /tmp/anotify-core-next-c3awire-execution_auth.pre-total.go	2026-11-10 02:28:38.876821880 +0000
+++ internal/store/execution_auth.go	2026-11-10 02:28:49.132821831 +0000
@@ -207,7 +207,7 @@
 	if err != nil {
 		return err
 	}
-	if totalBusy+1 > effectiveTotal {
+	if false && totalBusy+1 > effectiveTotal {
 		return fmt.Errorf("%w: node %s 机器总量预算已满（%d/%d）",
 			ErrSlotUnavailable, nodeID, totalBusy, effectiveTotal)
 	}
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^(TestGrantTotalShrinkCountsOldHigherSlotOnly|TestConcurrentTotalShrinkGrantsExactlyOne)$" -count=1 -v
=== RUN   TestGrantTotalShrinkCountsOldHigherSlotOnly
    execution_wire_test.go:244: legal empty slot must be rejected by total occupancy alone: <nil>
--- FAIL: TestGrantTotalShrinkCountsOldHigherSlotOnly (0.02s)
=== RUN   TestConcurrentTotalShrinkGrantsExactlyOne
    execution_wire_test.go:305: total-only transaction must admit exactly one legal new grant: success=2 rejected=0 results=[<nil> <nil>]
--- FAIL: TestConcurrentTotalShrinkGrantsExactlyOne (0.03s)
FAIL
FAIL	github.com/zzjcool/anotify/internal/store/v4test	0.060s
FAIL
exit=1
```

### /tmp/anotify-core-next-c3awire-sha-restore.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=restore exact saved execution_auth.go; sha256sum -c; cmp; git diff mutation owners
internal/store/execution_auth.go: OK
internal/store/node.go: OK
internal/store/task.go: OK
internal/store/task_attempt.go: OK
exit=0
```

### /tmp/anotify-core-next-c3awire-green-total-only.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^(TestGrantTotalShrinkCountsOldHigherSlotOnly|TestConcurrentTotalShrinkGrantsExactlyOne)$" -count=1 -v
=== RUN   TestGrantTotalShrinkCountsOldHigherSlotOnly
--- PASS: TestGrantTotalShrinkCountsOldHigherSlotOnly (0.03s)
=== RUN   TestConcurrentTotalShrinkGrantsExactlyOne
--- PASS: TestConcurrentTotalShrinkGrantsExactlyOne (0.03s)
PASS
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.057s
exit=0
```

### /tmp/anotify-core-next-c3awire-check1-gofmt.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=a9496238e34a96e1d5ad0087a34e9f02d92f821d
cmd=timeout 60 bash -c 'test -z "$(gofmt -l internal/store internal/artifact)"'
exit=0
```

### /tmp/anotify-core-next-c3awire-check2-build.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=a9496238e34a96e1d5ad0087a34e9f02d92f821d
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go build ./internal/store/ ./internal/artifact/
exit=0
```

### /tmp/anotify-core-next-c3awire-check3-vet.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=a9496238e34a96e1d5ad0087a34e9f02d92f821d
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test/ ./internal/artifact/
exit=0
```

### /tmp/anotify-core-next-c3awire-check4-test.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=a9496238e34a96e1d5ad0087a34e9f02d92f821d
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	24.723s
ok  	github.com/zzjcool/anotify/internal/artifact	2.379s
ok  	github.com/zzjcool/anotify/internal/protocol	0.045s
exit=0
```

### /tmp/anotify-core-next-c3awire-final-audit.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=a9496238e34a96e1d5ad0087a34e9f02d92f821d
cmd=final scoped-source audit, exact restoration, unchanged Renew/AckCancel proof
internal/store/execution_auth.go: OK
internal/store/node.go: OK
internal/store/task.go: OK
internal/store/task_attempt.go: OK
RenewLeaseAuthorized: byte-exact unchanged
AckCancelAuthorized/M3: byte-exact unchanged
internal/store/execution_auth.go:657: func (d *DB) CompleteTaskAuthorized(ctx context.Context, auth NodeAuthorization,
internal/store/execution_auth.go:708: if IsTerminalPhase(phase) {
exit=0
```

### /tmp/anotify-core-next-c3awire-iteration1-vet.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 gofmt -w internal/store/execution_auth.go internal/store/v4test/{execution_auth,execution_wire,task,task_attempt,retention,emit,stats}_test.go; timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test/ ./internal/artifact/
# github.com/zzjcool/anotify/internal/store/v4test
# [github.com/zzjcool/anotify/internal/store/v4test]
vet: internal/store/v4test/execution_auth_test.go:935:11: declared and not used: token
exit=1
```

### /tmp/anotify-core-next-c3awire-migration-compile.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run ^TestCompleteTaskActual -count=1 -v
# github.com/zzjcool/anotify/internal/store/v4test [github.com/zzjcool/anotify/internal/store/v4test.test]
internal/store/v4test/execution_auth_test.go:935:11: declared and not used: token
internal/store/v4test/execution_auth_test.go:967:10: declared and not used: token2
internal/store/v4test/retention_test.go:441:10: declared and not used: token2
internal/store/v4test/task_attempt_test.go:879:10: declared and not used: tokenA
internal/store/v4test/task_attempt_test.go:941:9: declared and not used: token
internal/store/v4test/task_test.go:393:11: declared and not used: token
internal/store/v4test/task_test.go:1063:11: declared and not used: token
FAIL	github.com/zzjcool/anotify/internal/store/v4test [build failed]
FAIL
exit=1
```

### /tmp/anotify-core-next-c3awire-iteration1-vet-green.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/v4test/ ./internal/artifact/
# github.com/zzjcool/anotify/internal/store/v4test
# [github.com/zzjcool/anotify/internal/store/v4test]
vet: internal/store/v4test/task_test.go:212:50: undefined: token
exit=1
```

### /tmp/anotify-core-next-c3awire-green-focused.log

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
HEAD=8fd44b9dc7eb2db0baf715c2f251cae9d0a7103e
cmd=timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/v4test/ -run "^(TestCompleteTaskActual|TestGrantTotalShrinkCountsOldHigherSlotOnly|TestConcurrentTotalShrinkGrantsExactlyOne)" -count=1 -v
# github.com/zzjcool/anotify/internal/store/v4test [github.com/zzjcool/anotify/internal/store/v4test.test]
internal/store/v4test/task_test.go:212:50: undefined: token
internal/store/v4test/task_test.go:219:48: undefined: token
internal/store/v4test/task_test.go:228:51: undefined: token
internal/store/v4test/task_test.go:393:11: declared and not used: token
FAIL	github.com/zzjcool/anotify/internal/store/v4test [build failed]
FAIL
exit=1
```

