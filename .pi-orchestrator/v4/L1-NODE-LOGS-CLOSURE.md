# L1 + R9 + P1 · Node 真实日志记录补口独立收口

## 交付与批准边界

Node-log WT：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-anode/.pi-subagents/runs/r-8d78ba7c/worktrees/node-log-records`。

- 原Node5a3dae3基线只读不改；L1日志版本 e3b030e（最初9文件）在双故障复审major下rejected，原失败/重验/三审均保。
- R9 bdfe8cf（可证完成事实+私有live+fault injection/since/预算覆盖），初始化publication窗口在独立review中被指出，原四项0不盖major。
- 最终P1 code/delivery `4c822a7b49567641dfa61757757fe9fd2b5cf469`，branch `pi-subagent/node-log-records-c88408ff`。当前sourceclean，writer已retired。
- 相对5a实际11个Node/protocol日志源码/测试文件；原Node执行/lease/resultACK/cancel/emit/upgrade接口不重新定义，不动rework2的2s断言，不修改核心store/runtime/FE/依赖。

批准：**Node真实结构化日志补口可作为冻结依赖交付**。父尚未集成G2，整套v4不算DONE；G3HTTP/真实cross和最终build/e2e仍待。共有protocol日志requesttail/since/response真实LogLine经此源码作为唯一DTO，不造runtime镜像。

## 真正日志事实与错误语义

- 同attempt观察sequence跨流递增、真实Node采集at，LF/64KiB真实片段/EOF收尾；raw原字节档案与D6不缩，不假回填旧时间/原始跨流因果。
- Node先since再tail再完整1MiB编码截断，遗漏传播truncated，records优先raw可空、bytes为回包raw字段长度。极大since安全饱和，非负输入不duration回绕/新增秘密上限。
- index v2、`logs/index.done`真实完成facts（records/lastSequence/size），记录Sync/Close全部成功后write+rename；record/Sync/Close/done写/rename任一失败不会伪造完成。
- 同Store实例健康live快照给运行中的有效记录，健康active0成功[]；正常closed0有done成功[]；重启header-only无活跃证明报unreadable，有真实安全前缀可truncated返回；done count/seq/size不匹配不得假完整。
- P1使live发布受writer初始化锁保护，读侧发布之后等待完整header或真实锁存error，不观察未初始化committed0。store.liveMu不跨snapshot持有，Close解writer锁再注销，无mutex反转。
- 真实attempt档案不存在no_local_archive；存在但缺/坏/unsupportedindex logs_unreadable；e3旧v1索引不迁移、不造时间、不删除用户档案。跨Store实例活跃0可能安全方向unreadable，重开完整档案靠done。

## 真实验收与review

1. R9双故障old e3 RED、公有读取false-complete现场、修后GREEN及回退LF=完整规则mutationRED真实文件 `/tmp/r9keep/red.log/green.log/mutation-red.log`（父已亲读）；原case/file不是日志缺失。一些scout误称缺失记录不据此补造历史。
2. P1 RED-only commit `fb321015c80eec8efa4f76d6b454104d8f799226` 只newtests+nilseam/旧锁序，新2条发布TryLock/失败初始化回归真实红；GREEN4c同case真过。日志 `/tmp/anotify-v4-r9p1/red.log/green.log`；red-source-head.txt记录更早基线bdfe，命名可能误导，但实际redlog及gitshow定位REDfb32101、未把旧源假作新测试证据。
3. 最终三fresh默认scout（r9p1-review-correctness/test/scope）全ok:true、0critical/major；只审P1新diff，原R9三角major已精确修清，不以票数盖。scope3file、原tests0删弱、无poll/timer/wire变更、privatehook生产nil。
4. worker launchverify最终P1四项accepted(verified)。父另外固定新source独立四项carrier `r9p1-parent-gate-carrier` 同样真实accepted(verified)：canonical/root/GOMOD/readonlyfmt+diffcheck、protocol+cmd/anode全vet、全count1、全racecount1。
5. 父亲读 `/tmp/anotify-v4-r9p1-parent/gate-{1,2,3,4}-status.json`：全部commandExit0、head/sourceCheck0、绑定4c822a7；完成13:02:10～13:02:35。脚本 `run-gate.sh` 明定-race/-count1/GOdirect/sum/auto、每项900秒、完整tee/once日志；不是由耗时猜race或模型ready文字认证。
6. Node18包原测试回归全部执行，原日志28新增口径保历史，本R9新增10top+6sub、P1再2top；不拿仅fixture类型计数代全包验收。原replay2s11:31red完整保，父固定baseline5a/candidatee3各20轮同flags均绿但不认preexisting/CPU根因，不再重跑旧mode。

## 接受minor与后续风险

- P1 `waitReadBeforeRelease` 的defaultselect本身不是调度证明；真正确定性钉子是发布边界TryLock失败与生产snapshot同mutex，原RED在此可区分失败。无需为文案完美大改/放大超时。
- hook store=nil措辞略宽/未来测试hook若阻塞会延锁；生产nil不进入wire，不在本轮扩新外部协议。
- 读先取nil-live再发生重开属于原API并发重开边界（同key双writer不在当前生产路径），不宣全场景已证明；无活跃新档案仍fail-safe。
- done未目录fsync，断电可能安全方向unreadable；既有raw读取大档案头窗口不在本轮改变。

## 集成顺序与安全

S1 store独立writer当前须完成/冻结/独立审清major/真实门禁；此Node/protocol源码即使验收已过，也**不得竞改S1活writer依赖**。父在其安全边界精确集成5a..4c的11个日志产品文件/提交，再G2 runtime唯一writer消费新共享DTO与真实历史attempt/S1接口。父最终cross需实际Node日志/HTTP/W6/FE、全Go/contract/quick/CI/build/fullE2E，不能凭此局部收口宣v4全部可用。

所有worker/scout未指定model/preset；没有push/MR/merge/rebase/reset/squash/设置env或用户数据清理。原WT/session/失败记录均保，不恢复旧orphan manager。
