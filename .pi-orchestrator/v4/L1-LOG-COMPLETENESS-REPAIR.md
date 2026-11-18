# L1-R9 · 日志索引可证完整性精修（父授权）

## 接管与已确认事实

原 node-log-records centurion 已retired，无terminal/supervisor；SAFE_BLOCKED e3b030e 与所有失败/review证据已收。此次orphan通知不授权恢复旧manager，也不因此增加writer。父批准一个fresh worker在既有唯一Node-log WT接管有限精修，不另开队/WT、不merge/push。

WT：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-anode/.pi-subagents/runs/r-8d78ba7c/worktrees/node-log-records`；基线 `e3b030e1762bd753e94c943b27c54c5480cfde8a` clean。

原L1-R1–R8及R2真空澄清不变。三fresh审：correctness/simplicity true，tests false；唯一已证blocking major为零字节record写失败+marker写失败时EOF落在完整LF边界，reader误报complete/truncated=false；header仅format/version、没有收口事实，Close失败同族。不得两票/四gate绿盖major。

## 精确修复授权

### R9-A 可证完整性，不依赖“marker一定可写”

- 建立可验证的干净完成记录（footer/提交记录与真实记录count/lastsequence或等价，不用attempt.finishedAt代替索引完整性）。写入过程发生任何record/flush/finish错误，禁止制造成功完成记录。
- 读取已关闭/重启/无活跃记录者的索引，不能仅凭LF边界认complete：只有有效完成事实与记录一致才完整成功；未完成但有真实可证安全前缀可明确truncated返回，不能无truncated。没有任何可证数据且非有效活跃真空时logs_unreadable。
- 活跃正常0记录必须仍允许真实`lines:[]`成功；活跃索引与崩溃header-only不得混淆。允许**包内部**同process active writer状态/快照（由Store/Record或等价私有登记）作为正在写入的证据，读侧要检查同attempt/身份且writer健康、使用有界一致快照；不能新增全局wire ready协议/socket/collector进程或用“目录/meta存在”冒活跃。
- 错误锁存，active writer错误后即使marker失败也不能误报成功；正常关闭应注销live状态，异常关闭同族纳入。marker可保诊断但不是安全完成性唯一来源。
- 真实崩溃/重开时无live状态，按持久完成事实判，旧e3无footer索引不补假完结/不删除原档案；未发布索引格式可适当版本化，但exact声明与测试必须同步，旧raw-only/unsupported仍truthful不可读。
- raw先写/完整字节/真实进程exit事实保持。旁路错误不得通过MultiWriter/Write返回值截raw，不能改变lease/result/ACK/retry逻辑。

### R9-B 确定性故障回归

新增包内部测试可注入I/O层（小接口/局部函数hook/构造选项，不新增公开protocol），覆盖：
1. 先写一条真实完整record，再下一条Write返回0+err，marker同时err；读不得完整成功/truncatedfalse。
2. header-only后首record0+err+markererr；不可伪真空。
3. writer Close/flush失败+markererr；不可伪正常关闭。
4. 正常active0记录、正常closed0记录均真空成功[]；重开header-only无active不可伪空。
5. 写完一条/多条记录后正常收口，count/seq不匹配/损坏footer/半尾拒或truncated真实prefix；running追加读不把半JSON当record。
6. I/O错误时原raw字节SHA和过程exit不受损，现28新tests与原Node全包不弱化。

### R9-C 有界since语义和raw预算裁决

- 允许一并修 `SinceSeconds*time.Second` int64溢出（现只拒negative）。保持非负输入语义，不偷偷加“292年最大”API约束；对超过time.Duration可表示范围的since可安全饱和成“起点早于全部可表示正常记录/不限”，确保等价而非回绕未来误返回[]。加极大/临界值测试，不能加timeout。
- UTF8段落切片在64KiB边界产生标准JSON替代字符是R1刻意允许的文字投影；raw逐字节留存，**此次不修Unicode分片策略**、不改R1。
- records与raw竞争同1MiB时，冻结R3/R4并未承诺raw字段始终非空；本修复明确**records优先，raw用剩余预算，可以空，任何省略传播truncated；bytes仍回包raw长度**。不把e3的旧raw-tail条件化称“毫无断言改动”：保原测试改动历史及精确原因。
- 在有足够预算的独立测试仍必须钉两流各自真实尾部（非只"x"弱后缀），预算不足测试钉真实最新records/完整JSON<=1MiB/raw档案完整/SHA/truncated/bytes。不要删测试让两流正确性无覆盖，不复旧API/额外帧。

## 文件所有权

仅 `cmd/anode/internal/resultstore/logs_index.go`、`logs_index_test.go`及必要新内部completion/IOfault测试文件；如active登记必须用resultstore.Store/Record，允许 `cmd/anode/internal/resultstore/store.go` 最小私有状态与直接测试。允许 `cmd/anode/internal/ops/logs_ops_test.go`、`ops_test.go` 的本R9-C独立覆盖，不删除弱化已有断言。必要reader/ops交界**仅**日志response truncated/error，超出上述先报父。

`protocol`/runner/lease/resultACK/replay/upgrade/emit/取消/Go依赖/Store核心DB/G2/FE/用户文件全只读；不修改原rework2测试2s、不新增replay执行逻辑。经理只读证据不写源码，worker唯一writer，所有worker/scout不传model/preset。

## 门禁与重试账目

已固定diagnostic baseline5a/candidatee3各20轮target replay全部PASS，原11:31 gate3失败仍保，不证明preexisting/CPU噪声。不再跑相同20轮mode；修复freeze后的正常全四项是新源码验收，不是旧失败日志洗白。

worker launchverify固定：
1. 真canonical cwd/root/GOMOD、只读gofmt-l internal/protocol cmd/anode +gitdiffcheck。
2. go vet ./internal/protocol/... ./cmd/anode/...
3. go test ./internal/protocol/... ./cmd/anode/... -count=1
4. go test -race ./internal/protocol/... ./cmd/anode/... -count=1

GOdirect/sum/auto、每长命令900秒受管/完整log/真实exit，任何同根因最多两次失败后SAFE_BLOCKED，不循环到绿或求无限完备。sourcefreeze后父fresh有限审只R9 diff与doublefault/trueempty/since/raw断言边界，然后父正确路径独立四项验收。原R1–R8 review minor其它项不在此全盘修。

本地owner-only提交允许；不push/MR/merge/rebase/reset/squash、不写.env/settings/.git/node_modules、不清旧WT/session/失败日志/用户数据。报告精确HEAD、indexformat/finish证明、active0与crashed0区别、故障RED/GREEN真实exit/原全部tests、四门禁与未完成范围；完成只L1-R9，不是整体v4 DONE。
