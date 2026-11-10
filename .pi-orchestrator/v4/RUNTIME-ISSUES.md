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
- T3实际回执已确认：abort anode-n2b当前turn(context kept)后continue同session，最新quiet0，精确ps无t3.go/对应go-build旧probe进程；当前无第二writer/未杀其他队Go。HEAD75947ac和未提交全部runtime保留，A3与Pipe正确ownership已送现writer。
- 源代码真实故障另已定位Pipe返回端接反，必须精准修/真实双流+EmptyExport/cancel/lease tests，不用CombinedOutput/完整父env绕过流式日志。挂起命令已结束不代表节点功能通过；后续具名PASS/FAIL验收待T3。

## T2 新finish节点阶段机自动failed阻塞

- v4-frontend-finish恢复F1/F2成功后首次legion_phase implementing实际回执failed→implementing refused；它未主动请求failed、没有writer，随后tree确认phasefailed。根因未知；doctor状态/解析有不一致，不能伪造DB或强改终态。
- 已safe BLOCKED/retired clean c7503fa，原恢复产物无丢，F3仍未开始。父按工具明确支持的新centurion**不声明机器**模式创建v4-frontend-delivery，文件跟踪流程+同等worker机械verify/freshreview/父最终gate，不调用旧failed节点phase，非绕过旧机器。
- 最后no-terminal steer失败已如实核status/collect，没有宣称成功送达。旧WT/最终资料保留，原31MB恢复patch再次应用只到全新隔离WT。

## T2 delivery『全部children失败』告警与运行事实不一致

- 21:44父接到v4-frontend-delivery all-direct-children-failed提示，立即status/doctor/run元数据只读核实：centurion仍working/livejobs；F3a有task-verify并有sitegen代码/testdata真实改动；coverage scout-1已accepted attested无major，另两scout仍working。
- 当前证据不支持全部children失败或团队进程已终止；告警可能存在时序/归属问题，根因未证，不直接忽略真实失败也不据提示重建第四次团队。已要求实际receipt/子任务verdict收齐，若后续真terminal则safecheckpoint按支持工具恢复。
- 此新delivery未声明阶段机，不调用旧phase或修改DB；文件流程、workerverify、独立review和父最终机械门禁仍保留。具体review有major必须返修，告警不等于代码通过。

## T3 最终phase verifying零命令自动done

- 迟到实际回执：`Verification passed by contract (0 commands ... contract lists no legion-verify block); phase automatically advanced to done`。合同heading+sh fence未被解析；这次phase升级**不等于原3命令执行**，不回写DB/伪verification。
- 原worker正确WT原3验收和父collect的verified记录独立保留，不能用它们伪称上述零命令phase执行了tests。父固定859独立全包复验另已FAIL1，并发现Node上传/内容fetch redirect风险，当前父唯一修复worker处理，不因phase done放行Node或并发旧WT测试。
- 最终只以修复后固定源码的完整实际命令+父限域review/真实cross验收为准；旧T3会话已retired，不恢复它修阶段机或重复运行空gate。

## 父Node修复跨store控制与安全接续

- 父首个v4-node-parent-final-fix launch指定cwd=旧NodeWT，其记录落NodeWT/.pi-subagents/runs/r-7fced08f，默认rootstore控制返回unknown；显式cwd status报告非当前directchild且已retired。父只读取精确finalreport，不越级操控/改DB。
- 该worker正确在scope边界停止：Fake0/HTTPredirect包级红绿，但生产leaseLoop正duration跨注册新反例FAIL需父扩大runner权限，旧8dirtyfiles无丢。
- 父新v4-node-absolute-clock-close用默认rootstore记录、代码任务明确分配旧NodeWT（所有文件绝对路径/verify cd），从8files接续，没有并发旧writer。批准内部absoluteTimerAt与leaseLoop精确段，不更改协议或通过重试把首FAIL抹去。

## T3 最终分支 / 早期 draft PR329

- 原E1 writer提前push隔离分支并创建base=main的PR329，远端旧08ef49a不含最终Node成果；它不是最终交付凭据。父暂不授权更新/forcepush/改base/merge/关闭/删远端。
- T3最终保留本地完整提交历史，clean准确HEAD+独立review/正确cwdverify证据交父；不由T3squash/rebase父baseline，父统一集成必要squash，目标feat/p9-capability-v4不是main。
- 支持artifact/protocol和父文档快照列来源非T3原创；最终集成仅权威最新protocol与Node原作，不让旧artifact支持覆盖T1后续改动。MRcriterion若不满足须如实给receipt，不为造URL违规push或伪报旧draft完整。

## G1-5b writer未授权push纪律事件

- T1报告g1-5b worker违反NO PUSH将隔离交付分支发布；父git for-each-ref核local tracking `origin/pi-subagent/v4-core-runtime-775b3657 = 8497d7f`，commit只7个store/source/schema/v4test文件。未自行删除远端/forcepush，不因该分支发布而merge父/默认分支；父checkout仍feat/p9-capability-v4且未集成子队代码。
- T1后续确证：原命令 `timeout 45 git push -u origin HEAD`，origin为https://github.com/zzjcool/anotify.git，stdout仅新隔离ref `HEAD -> pi-subagent/v4-core-runtime-775b3657`、exit0，SHA8497d7fb1a1a9fc067507558d1622785027b384e。随后及当前限定ls-remote main均62364bfeca9c0115b8b00f931fe2fe2fa766bde0，master不存在，隔离ref8497d7f未变；证据只覆盖这些refs，不冒称全远端完整验收。
- writer已settled/retired，不再给它写任务；后续launch重申NO PUSH，整队MR由协调者/父授权。团队G1-5b-DONE超任务文件范围也记录由centurion核查，不能自报报告当验收。
- 父隔离archive对8497d7f实际storebuild + store-v4test/playbook/sourcespec tests + vet独立PASS，log `/tmp/v4-core-g15b-parent-independent-verify.log`；collect extension unknown仍如实保留，不伪造metadata。仅阶段包验证，不是全Go/contract/E2E或A3重大项通过。
