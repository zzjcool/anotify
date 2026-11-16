# R9-P1 · live 登记的初始化窗口精修

## 已确认与裁决

R9 `bdfe8cf` 已有双故障/完成事实/真空/重启/计数/Since/回包预算测试，worker launch四项与父独立四项实际全0。三fresh审正确性ok:false（register-before-lock初始化窗口），覆盖/简洁性ok:true亦指出同窗口。此问题是安全方向的瞬时不可读，不是旧doublefault假完整，但修正很小，不通过降severity放过。

`logs_index.go:209-212` 在获取writer互斥锁前登记live，reader可能观察到未建header的active/committed0；新注释“只见打开前或稳定初始化”不成立。修复须使live发布受初始化锁保护，读侧要么未发布，要么阻塞/等待该次有界初始化完成并获得有效快照。不能把未初始化状态假当有效0记录，也不采用poll/setTimeout扩大等待阈值。

## 唯一writer最小范围

既有Node-log WT，基线 `bdfe8cfd4ace6fdfa122baeb51f00cc04e430bdd` clean，所有旧writer/父机械carrier/review已结束。只 `resultstore/logs_index.go` 的open初始化/live登记顺序与对应 `logs_index_completion_test.go`（必要测试小seam可 `store.go` 私有registerhelper）；不要重写整个reader/完成协议，不改v2格式、done/raw/runner/protocol/replay/2s。

- 保证明锁序：store.liveMu不跨snapshot持有、Close先释放writer锁再unregister，不能为了修窗口引入mutex反转。
- 新确定性并发回归必须在登记边界可证明writer初始化锁已持有（小内部测试seam/互斥状态检查/同步通道等，不靠碰运气循环），真实读在初始化未完成时不能得到未建header的成功或瞬时伪不可读；发布正常后active0/closed0仍成功，失败初始化仍truthfulunreadable。
- 修前新测试应实际RED、修后GREEN，原全部R9/Node不删不弱。任何测试hook保持私有、不进入wire/外部global协议。

## 证据澄清

父亲读 `/tmp/r9keep/red.log` 与 `mutation-red.log`，原始失败确在，worker已有源码恢复/测试调用记录。部分scout说“raw日志不存在”与父read/ls事实不符，不据此补造或删历史；无需再次重复旧RED/mutation到绿。这次只新publication规则RED/GREEN。

接受其他非blocking边界：sameStore私有live、跨实例/重启靠done，旧v1 truthfulunreadable不迁移，目录fsync风险安全方向、64KiBUTF8投影/records优先保raw。此小修不趁机扩大旧28清单/全Node所有边界。

## 固定验收

canonicalNode-log root/GOMOD +只读gofmt-l internal/protocol cmd/anode/gitdiffcheck；protocol+cmd/anode全部vet、count1、racecount1，GOdirect/sum/auto，各900受管fullLog/真实exit。修后freeze父fresh只限publication新diff与原完整性未回归、并正确路径独立四项；旧bdfe四项0保为历史，不认证新源码。相同根因两失败SAFE_BLOCKED，不重试到绿。

本地owner-onlycommit允许；不push/MR/merge/rebase/reset/squash/env/settings/.git/node_modules/用户数据/旧WT/log/session清理。worker/scout不传model/preset。完成只是R9-P1，不是v4 DONE。
