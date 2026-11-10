# Node 产品支持交接 · N-final（父门槛通过，仅供集成）

## 冻结版本与范围

- 本地来源 `pi-subagent/v4-anode-d0006ee5`，clean源码 **`5a3dae3879662a7487f7248ab83af0776178a9cd`**。完整提交历史保留；未push/改PR329/merge。
- 交接目录 `/tmp/anotify-v4-node-products-handoff/`，补丁base `21567b7`，只60个文件：`cmd/anode/**`、`Dockerfile.anode`、`scripts/install-anode.sh`、`docs/anode-install.md`。
- **不含** `internal/protocol`、`internal/artifact`、Go模块全量覆盖、T1/T2其它代码或父冻结文档。protocol目录在最终Node源与33e517a间字节无变更，T1现有权威已经同步。旧T3artifact support不得覆盖T1新typed/ref工作。
- patch SHA256：`1829d2487031b32a391d95db8d37a8aa49e53c897c39de4496ea750bfe523a27`，其它源清单/日志/审查及二进制均列 `SHA256SUMS`。

## 父实际验收

不是零命令phase done或self报告替代：
- 固定源码独立archive原 `go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1` PASS。
- 原全链App case **count30**、Hub排队case **count20**、Runner blocked-first **count10** 全部首次PASS。
- protocol/Node vet、source diffcheck PASS；Hub/App/Clock/Runner/Upload/Capability **6包race**首次PASS。
- Cgo0/trimpath集成二进制实际构建和version运行成功；`go list -deps ./cmd/anode`无SQLite/store依赖。
- 最后WS-ACK两个fresh limited实现/覆盖门槛均true/no findings，HTTP确认不回显单根fresh true；此前clock/HTTP/lease/执行/导出/安装等 scoped审查与红绿保留，不用旧scope的PASS掩新失败。
- 另外在T1已提交 **`b86457525d4e5145856478cd8b91a0f59b986148`** 独立archive中，Node-only patch check/apply+protocol/artifact/Node全包测试PASS；这只验证当前shared依赖编译与组件，不声称中枢实际WS/API已端到端通过。Go.mod与当时核心比较无差异（dependencies-go-mod.diff为空）。

日志与fresh原报告均在交接目录；更早859/5b/e64的失败、unknown、零命令gate异常不改写。确定性实际TaskResult→ACK正常cleanup→仍在返回中的Write旧ctx取消关闭全连接→logsEOF已RED/GREEN闭环；不伪称每份历史记录中的具体ctx身份都曾捕获。

## 辅助二进制

- `/tmp/anotify-v4-node-products-handoff/anode-linux-amd64`
- SHA256 `46ac697f0b05c2a15547fca65203261c847f4d5a597ee579e18aad96c0751850`
- CGO_ENABLED=0，版本 `4.0.0-integration.5a3dae3`，commit完整5a3dae3；精确JSON在BINARY-VERSION.json。
- 这是Linux amd64集成验证构建，**不是已发布版本/跨平台包**，不替代最终Makefile/Docker/发布版本接线。T1可直接用于当前真实quick，再按冻结源码重建最终版本。

## T1 原样同步授权与顺序

当前核心仍唯一C3b writer：先该writer安全收齐，无活writer后由一个明确授权support-import writer或协调者原样SHA→applycheck→apply。不能与store writer并发修改同WT或拿整个T3branch cherry-pick。

- 只同步上述4个路径。文件原作归T3/父Node修复，T1不在Node副本原创修改；发现真实integration bug给父精确证据再派Node writer。
- apply前确认目标cmd/anode尚不存在或与源匹配，不能覆盖其他活writer/旧工作成果。已同步的目录先byte/hash比较，禁止重复apply。
- 用**当前核心artifact/protocol**重新跑Node/protocol/artifact tests，不能为Node兼容倒退T1数据/API或拷贝旧artifact。当前Go.mod无新依赖，不需覆盖模块；若后续模块有差异给精确依赖再父定。
- G4 Makefile/旧cmdagent退役/harness/seed/实际E2E归T1，必须用真实Node而非stub；此交接解除Node source等待，不停止当前C3b/G2/G3实际代码。

## 仍未完成的全产品验收

两队实际中枢运行链、Namespace HTTP/OpenAPI、前端五套件/finalepoch/页面与核心白盒/CI/build/fullE2E仍需父统一集成验收。本次只通过Node当前已识别范围与机械门槛，**不表示Anotify v4整体完成或已合入父分支**。

执行仍非sandbox，线性化点后已发送字节不可撤；真实cancel/expiry立即停进程/HTTP上传及阻止后续发送，WS已准入写仅在原deadline/10s/连接Close下完成、服务端最终验权。历史失败与风险账本继续保留。
