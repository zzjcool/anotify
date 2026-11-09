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
