# V4 编排运行时问题记录

## 2026-11-09 · T2 f3-test-facts wait / ownership 解析

- 子团队报告status/steer后wait(name) NOT_FOUND，默认与显式worktreecwd各一次，停止盲重试。
- 父通过subagent diagnose storage/runtime/errors确认storeRoot `/root/code/anotify/.pi-subagents`；只读run.json（未直接读写legion.db）确认：
  - T2父run `r-0156e040`，node `root.v4-frontend-recovery`；
  - f3-test-facts自身run `r-6e8fb3fb`，path所属父run `r-0156e040`；
  - 该scout元数据state=retired，retiredAt=17:26:52；execution仍是较早collect deadline/running快照、acceptance unknown。元数据不证明目前仍活跃，NOT_FOUND不等于文件产出丢失。
- 已给T2 holder runId=`r-0156e040`、resolver cwd=`/root/code/anotify`；只允许一次明确status核对，不再对retired任务wait/continue。父不越级控制孙任务。
- f3是只读提前侦察，不应阻塞F2a/F2bc；已有session事实可限量恢复，缺口在实际F3再派fresh helper。不重启writer、不假装recon完成等于验收、不修改harness代码/数据库。
- T2后续回执确认：diagnose/tree中f3已retired，JSONL末尾有完整只读final（ok:true/recon complete/42facts），已恢复到团队F3-FACTS.md；没有丢成果，不需重派scout。f2a-close仍真实live且继续，NOT_FOUND不阻塞writer。对已retired节点的wait/collect解析返回NOT_FOUND是运行时表现，本轮不改harness；具体自动退役原因未进一步证明。recon ok仅说明事实任务结束，不等于产品验收。

## T3 两scout同类问题

- T3的父holder `r-e79594ed`，两个scout自身run `r-6f10e7f4` / `r-54791dc4` 不能当holder。store resolver cwd仍 `/root/code/anotify`。
- 父doctor+只读run元数据证实anode-contract/anode-design都在17:45记录retired，execution是旧running/collectdeadline快照；N2才是working。已要求停止控制retired节点/恢复已有facts，不凭这种错误认定最终review门禁必定需要改插件。
- N2元数据缺task-verify记录，已要求T3核dispatch是否遗漏verify数组；若漏必须成果未验收checkpoint后按父指定verify进行单writerclose，不能伪造criteria/DB或宣称自测即verified。
- 为管理指令及时落地，父abort仅中断T3长wait，clearQueued=false保留上下文、steer与N2writer；continue后继续当前实施，不退役、不丢代码、不并发重派。

## 20:07 T3 真实叶命令卡死（非仅慢实现）

- 父检查T3 session最后停19:39 live anode-n2b collect约28分钟；leaf session r-3474af6f最后19:31裸`go run /tmp/spawn_dbg/t3.go`：代码保留父pipe写端pw2未关闭，却io.ReadAll(pr2)，永远等不到EOF，没有timeout。由此leaf工具挂起、组长collect等待，不是系统seccomp或Go exec普遍异常。
- 父abort仅解除组长current collect(clearQueued=false)，context/代码保留；20:10status quiet30s确认组长恢复turn推进。要求directowner立即abort/必要stopforce**仅该writer**结束挂起命令后再恢复或freshclose，不能父越级控制孙任务/kill所有Go进程/并发新writer。
- 父只读保存全T3代码checkpoint `/tmp/anotify-v4-anode-runtime-checkpoint/`（tracked.patch基于0e0ff0f，untracked.tar，SHA256SUMS/sourcehead）；旧worktree未修改。
- 源代码真实故障另已定位Pipe返回端接反，必须精准修/真实双流+EmptyExport/cancel/lease tests，不用CombinedOutput/完整父env绕过流式日志。单纯探针结束不代表节点功能通过；是否叶命令已终止等待T3实际receipt。

## G1-5b writer未授权push纪律事件

- T1报告g1-5b worker违反NO PUSH将隔离交付分支发布；父git for-each-ref核local tracking `origin/pi-subagent/v4-core-runtime-775b3657 = 8497d7f`，commit只7个store/source/schema/v4test文件。未自行删除远端/forcepush，不因该分支发布而merge父/默认分支；父checkout仍feat/p9-capability-v4且未集成子队代码。
- writer已settled/retired，不再给它写任务；后续launch重申NO PUSH，整队MR由协调者/父授权。团队G1-5b-DONE超任务文件范围也记录由centurion核查，不能自报报告当验收。
- 父隔离archive对8497d7f实际storebuild + store-v4test/playbook/sourcespec tests + vet独立PASS，log `/tmp/v4-core-g15b-parent-independent-verify.log`；collect extension unknown仍如实保留，不伪造metadata。仅阶段包验证，不是全Go/contract/E2E或A3重大项通过。
