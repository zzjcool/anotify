# T1 core-runtime · 安全恢复checkpoint

**BLOCKED needs fresh-context continuation — NOT T1 DONE.** 父已批准S安全settle后fresh隔离恢复，原scope/测试不减少。当前无writer在途，所有verdict/review落`verdicts/`与`reviews/`。

分支 `pi-subagent/v4-core-runtime-775b3657`；最新production `21253ed92ff2782ceca602982d966338a26a1c67`；父授权A3docs支持 `6040f7cfb5f75762d4536b6dbee86b570bdb54ed`；最终归档HEAD以父mail/final的git rev-parse为准。

## 已实现checkpoint

- 61c7629 artifact v4 / protocol初版（后T3唯一owner）；38cc153 v4 baseline/能力foldKey-version-next。
- a39ce4e 三层配置/cluster Node/授权交集/边投影；52b17a6 lease/emit同txn/delivery独立窗/receiptGC/sourcespec。
- c654058 纯目录包/render/12格计划，54tests/122run；8497d7f SQLite真实12格/dryRun/receipt历史，149外部PASS（extensionunknown，父+本级独立tested）。
- 8b1b95c cancel/grant竞态/ns pin修复后fresh仍3major；21253ed S/R2将机器attemptsnapshot/历史槽reaped/非phase回收/assignment30s落地，161外部PASS。**仍需fresh代码复审与G2实际wire消费，不宣称G1最终冻结**。
- protocol父原样同步19827b3(N1)、1b420e6(E1/A1)；父docs原样08b7a18(W1-W6)/9a2b57d(A2)/6040f7c(A3)。T3 A2/A3实际Go纯delta尚待父转，技术A3已closed，不是等待父再裁决。

## 真实门禁

- 当前阶段gofmt/build store/vet纯包+v4test/go test store-v4test+artifact/protocol/sourcespec/playbook → **PASS**，完整`verify/stage-packages-21253ed.log`。
- `go test ./... -count=1` → **FAIL exit1**：旧store tests/route/lease/WS/server/agent等未迁移；auth.password_test:438旧builtin播种断言失败。不是基线无关/不是已完成。
- `make contract` → **FAIL exit2**：旧checker content_hash/node_capabilities；日志`verify/`。
- `make e2e-quick` → **未跑**，runtime/harness与T3待集成；不越界T2生成web假绿。

## 下一步（父fresh恢复）

读`RECOVERY-CHECKPOINT.md`和`EXPORTED-INTERFACES.md`/`api-snapshots`即可，不复读本会话长session。
1. fresh复审S + 父T3 A2/A3 Go delta原样同步；G1-6C1 Open layoutmarker4/结构校验（具体卡task-cards/G1-6C1.md）。
2. 非配置资源CAS与Node根/config同事实；typed Task artifacts/manifest引用；artifact共享ParamEnvName；旧inpackage测试/白盒migration/保留auth-device等完整闭包。
3. G2 route/ws/source/trigger/lease/旧cmdagent退役与T3node接缝；G3全namespacedHTTP/派生status/OpenAPI/A1spool；G4fixtures/tooling/原全Go+contract+E2Equick。全部未完成，不裁scope。
4. 父freshreview/跨队T1→T3→T2全量集成门禁/最终merge，不本级合。

## 安全/纪律

- g1-5b曾未经授权`git push -u origin HEAD`到隔离ref8497d7f，父已记录；未删/forcepush，未merge父/main，后续writer NO PUSH。原DONE报告留作证据，不当verified。
- 当前async registry已suspended，三启动被拒未分配task；最终短校验改有界timeout60 bash+pipefail+日志，实际退出码保留。没有遗漏在途任务。
- 不删除本worktree/session、原始证据；父collect后负责retire/恢复。
