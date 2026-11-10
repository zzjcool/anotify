# 核心 C2 后继续实际运行链（父批准）

当前安全阶段：C2 生产 `52d5870`，精修 `e42fe84`；旧recovery已safeBLOCKED/retired，最终clean HEAD **`2a3c7ef951980aa9d6096f3d3d724c6c454179dc`**，3MB父支持快照 `/tmp/anotify-v4-core-runtime-next/core-source-and-evidence.patch`+SHA/完整CHECKPOINT-2已保存。不使用上次f717f71旧表。C1格式/R2谓词/S3 row与历史安全/C2 UID CAS和tombstone已分阶段实现；**C3认证入口、三预算、typedmanifest、原白盒测试、G2–G4仍未完成**。

## 新上下文接续

旧recovery manager约330K，上次请求C2安全收口后fresh已批准。当前所有writer/review已收，C2 scopedmajor已闭合，不再老context派大writer。旧团队保clean本地branch/精确HEAD、CHECKPOINT-2/EXPORTED/各红绿和未通过的全量gate，安全BLOCKED不是DONE。父使用新受管centurion任务（无需修改旧phase/DB），只恢复现T1生产/证据，不重做C1/C2/此前row修复。

## 新队第一个实际任务

按已有 `G1-6C3` 卡切成单writer两闭包：
1. **认证与预算**：NodeAuthorization来自认证session，grant/renew/complete/历史reap同事务当前Node凭证/namespace/精确assignment-slot-attempt-lease/cancel-expiry；退役旧公开绕授权入口。预算total、跨namespace同名perCapability/*、同ns+name各版本maxPerNode并计所有未reaped/真实reservation，不重复本次slot。真实rotationrace、expiry、并发grant、旧ACK与ns/机器输入判别。
2. **运行产物与引用**：typed task/attempt manifest、上传两次授权的存储接口、path/hash/size/idempotence/已上传引用、cap tombstone引用保护与GC；actualNode HTTP handler是后G3，不让存储接口伪装端到端已跑。

阶段4条闭包verify必须正确WT、原旧测试仍需后续白盒完整恢复，不能只外部case数量通过称G1完成。

## 然后完成实际系统

- 原store白盒/migrate/transaction/retention/stats测试映射与真实闭包，保留auth/password/passkey/device/push，纯旧port/priority/handler/迁移链可以具名退役，不skip。
- G2 route/trigger/source/ws/lease，消费protocol33e与新Store安全entry，真实dispatch-accept-grant/renew/result/emit/cancel/rehello/部署/历史日志。
- G3 namespace资源信封/status明细/CAS、source动作/包回执/node配置/用户与Node产物通道，五api文件按API-SPEC/REF-HANDOFF；不用旧endpoint/字段fallback。
- G4 Makefile/Docker/安装/seed/harness/非前端套件与旧agent退役、真正原全Go/contract/e2equick。

## Node支持状态

T3已完整交8596964，但父独立full复验发现timing失败及redirect边界；父Node绝对clock close单writer保留修复中。protocol33e独立已过可用，Node-only最终source/binary+SHA必须等父修复验收后给；不stub/不把未验Node quick当green。此依赖不阻塞当前C3/G2/G3代码。

## 边界

所有权不变，protocol/cmdanode归T3/父Nodefix，UI/sitegen及5前端套件归T2，核心api/harness/tools归T1。父新RootRESOURCE-DELETION/API1/API2/RESULT-AUTHORITY/SLOT为已批准细则，原RootCORE-RECOVERY f717为历史不得又等待已闭合审批。

原3最终gate不减；进度只mail/file，安全blocked或完整DONE才final。每writerlaunch显式verify+files+真实cwd。管理器不逐包重扫/重复记录已批准问题、不要再以源码没写而延时提示替代实际C3任务。
