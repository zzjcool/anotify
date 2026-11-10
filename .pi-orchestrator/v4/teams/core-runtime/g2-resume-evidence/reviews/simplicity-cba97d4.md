补记（收口）：`g2-freeze-readonly-dependency-test` exit 0（store/v4test/artifact/protocol 全 ok，HEAD `cba97d4`）。四门禁现已全部独立回收：format / vet / runtime-test / dependency-test 均 0，且首项为只读 `test -z "$(gofmt -l …)" + git diff --check`，符合父纠正；owner 范围、证据保留、TEST-MAPPING 336+314 逐名计数亦已核对。

机械门禁全绿但仍不改变审查结论与阶段：本审查仅作为父裁决/rework 证据，不伪 reviewing/verified：

- major（5）：`route/emit.go:114` 剪枝事件先于 `EmitAuthorized(:140)` 写入（ns 撤销/终态/过期仍落事件）；同处吞掉 `recordPrune` 错误返回 200 缺 eventId（违背 `TEST-MAPPING.md:1234` R:V4_POLICY 与 `capability.md:596`）；`source/source.go:273`+`route/route.go:218-226` dry-run 逐目标 reason 被 `Pruned[0]` 兜底写错（违 `source.md:347`）；`route/route.go:308/258` 钉版 next 的裸 `to` 解析 latest（违 `capability.md:145`，需父裁决）；`ws/hub.go:742/732/739`（cap1 `:682`）读循环阻塞发送可永久卡死会话（违 bounded 门禁）。
- minor（7）：rehello 改身份不重排 sessions 键（`session.go:142`+`hub.go:270/180-181`）、负 attempt 当 current（`hub.go:662` vs 卡 `:299`）、显式 nodeIDs 部署不校验 ns 授权而重放过滤（`hub.go:556` vs `:597`）、detach 用 Background 无 deadline（`:203/:206`）、hello.appliedRevision 静默 vs config.applied 断连（`:291` vs `handlers.go:25-27`）、sweepErr 重复并入单源错误（`trigger.go:97`）、`route.go:224-226` 死代码。
- protected pending（3，不得当完成）：结构化日志逐条字段（父 Node-log L1-R1～R8）、事件+投递窗+任务无单 store 事务（`source.go:127-140`）、`downstream_missing` failed-task 形状；writer 已如实记录，G2 DONE 未声称。

{"ok": false, "reason": "all four independent read-only gates exit 0 at cba97d4, but 5 major correctness/authority findings (prune event before EmitAuthorized + swallowed trace-write error, dry-run per-target reason, pinned-next resolved to latest, blocking logs waiter send) and 3 protected pendings remain for parent adjudication/rework; verifying/DONE not claimed"}
