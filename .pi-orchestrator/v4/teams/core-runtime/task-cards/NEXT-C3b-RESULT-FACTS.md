# NEXT C3b result facts — parent exact approved rework

Parent RESULT-FACTS-HANDOFF.md read-only authoritative at /root/code/anotify/.pi-orchestrator/v4/RESULT-FACTS-HANDOFF.md. Former5e2331b refusal is preserved, not completion. Current evidenceHEAD15ed5bf; oldwriter toolstatealreadyretired (03:24:09), no activewriter. Newfresh c3b-result-facts reworks this exactblocker, noC3a/C3bregistry/GC reimplementation.

## Exactsoleowner
internal/store/task.go,task_attempt.go,execution_auth.go,schema.sql,migrate.go,retention.go; internal/store/artifact_manifest.go ONLY existing result-fact/accepted-time/retention safety integration if required; internal/store/v4test/*_test.go ONLY resultfieldcaller/schema/projection/TTL and newfullfactsdiscriminants. No namespace/capability/node/emission/artifact/protocol/HTTP/manager/otherteam writes. Need extrafile -> reportprecisepath beforewrite. Parent permits internal trusted result_accepted_at if no suitable existingobservedtime, no newpublicfield.

## Frozenamendment
- store.TaskResult adds ExitCode *int, StartedAt/FinishedAt time.Time with camelCase JSON, exact existingwire facts. Result JSON/read/task-attemptfactprojection/replay compares entire nullableexit/precisenanosecondtime alongside Phase/Reason/Summary/Error/typedmanifest. nil!=0; before-spawn no actualstart/exit合法，不伪造过程开始。
- UTCnormalize andstripmonotonic compareequalinstants semantic; do not truncateUnixsecondsourceforequality. Indexedsecondcolumns are conveniences, completeJSONretainsprecision. No newwiretoken/protocolframe.
- Trusted serverNow remains independentlycurrent transaction auth clock forcredential/ns/leaseexpiry/cancel/register/quota/GC. Never passold/futureNodeFinishedAt asnow or leaseextension. Actualprocessfinish/exit projection not uploadacceptclock; trustedfirst resultaccepttime separatelykept existingcolumn orinternal result_accepted_at(firstwriteonly, hiddenpublic) for TTL. Old/futurereportedtimecannotprematureGC/delayfactsindefinitely/reviveexpiredlease. Basicstart<=finish onlywhenbothpresent, no hardcodedclockskewwindows.
- SamecurrentactualNodeAuth/currenttaskattemptlease + COMPLETEsamefacts only may no-write duplicate disposition. Differentexit/nilvs0/startednanos/finishednanos/phase/error/artifacts conflict0write; GCmissingfacts cannot infer ACKphase. Firstacceptserverobservation immutable onrepeat, newerattempt untouched. ExistingC3a/M3/budgets/typedregistry/ref/force rules retained.

## Testredgreen
- Newfieldreopen/JSONnanosecondnullableexit/timeoffsetequalinstantnormalize; 0 vsnil differs. ActualFinishedAt atprocessend earlier than75s upload completion retained, authserverNowcurrent independentlychecksrenewedvalidlease.
- Samefullfactsduplicate wholeTask/attempt/manifest/slot/acceptedTime0write; changedexit/start/finish precision orbefore-spawnboundary rejectsfullfacts0write, rejectedcurrentauth/oldlease/newattempt invariant remains.
- Veryoldreportedfinish can'tbypassexpiredserverlease; old/futurereportedtimesdon'tdrive30dayfacts/7daybytesGC; active/unreaped guardsretained. Before-spawnstartzero/exitnil nofakeprocessstart/time.
- Focusedtemporarymutationdropfieldfromcomparison oruseprocessfinishauthorization -> actualfunctionalred; restoreexactSHA green. Existing5e registry/GC/namespaceforce tests staygreen, no weak/skips/stubs.

Fixed4extension unchanged correctWT everypwd+absolutecd: gofmtstore/artifact; store/artifactbuild; v4test/artifactvet; fullv4test/artifact/protocol-count1 officialGoenv. Logsfullnamed/tmp/anotify-core-next-c3b-facts-*.log pwdHEADcmdexit. Shorttimeout60, true-longnamedasync+collect orfrozenreadonlyextension ifnoadmission, nohack/testcoveragecut. Sameproblem2retrymaxthenparentstoploss. Commit fix(store): 保存完整执行结果与独立接收时钟。Reportonlyactualchangedfiles/newtests/redgreen/SHArestore/4actualresults/HEAD/Gointerfaces/fullpendingwhiteboxG2-4. NoPushMergeMR/nestedwriter/settings/env/node_modules.
