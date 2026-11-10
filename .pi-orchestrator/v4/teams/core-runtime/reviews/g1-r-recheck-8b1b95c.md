# G1-4R @ 8b1b95c 对抗复审（只读；行号均为该提交 `git show` 行号）

**判定：ok:false** —— 取消授权竞态类 major 确已真修；但 D2 机器层未钉版、旧 attempt 槽位无法按 reap 事实释放、ReclaimStaleSlots 仍是 phase 推断=假 reaped，三项 major 未清。

## 已确认真修（此前 major）
1. **cancel-vs-grant 同事务持久化** ✓：pending 取消一条 CAS `WHERE phase='pending' AND cancel_requested_at IS NULL`（task.go:1136-1141）；grant 的唯一入 running UPDATE 同条件（task.go:864-866），且事务内先重读 phase/cancel（task.go:786）。`_txlock=immediate` 使读改写原子（pool_test.go:8、graph_test.go:7 注释/测试断言）。竞态测试 8 轮（task_test.go:870-958）覆盖两种赢法。
2. **cancel 在飞 lease 过期不 regrant** ✓：三重守卫——grant 拒（task.go:786）、RenewLease 拒（task.go:910-914）、recoverOne cancel 分支直接终态不重排（task.go:1405-1422），派发查询排除 cancel_requested_at（task.go:598-599）。
3. **drain 不 lost** ✓：lost 判定只把非 online/draining 当失联（task.go:1316-1322）；grant 拒 draining（task.go:846）；running 可续租/写结果（task_test.go:987-1040）。
4. **预算耗尽槽位同事务释放** ✓：attempt+1 唯一增点（task.go:803），超预算在 grant 事务内先释放 busy_task 再 finish→abandoned（task.go:806-831），失败回滚走 defer ReleaseSlot（task.go:743-751）；测试 task_test.go:960-609。
5. **lease 过期 result 拒绝、过期 token 拒绝** ✓：attempt+lease+phase+未取消+lease_until>now 同一事务（task.go:1041-1051）；正常 result 释放本 attempt 槽（task.go:1065）；测试 task_test.go:1054-1107。

## Findings

**[MAJOR-1] D2 机器层（local/node）没有按 attempt 钉版，存在 grant 后漂移**
- 只钉了 ns 快照：createTasksTx 接受时写入 namespace_vars（task.go:301-305、328-331；schema.sql:352-353）；local/node **每次 TaskInputs 调都现读 node_configs**（task.go:403-418，SQL 在 :407）。
- grant 把 `config_revision` 也记成另一次独立读取（task.go:854、863），与 TaskInputs 的 vars 读取无事务耦合、TaskInputs 也不比对 revision。因此「grant 时 rev=5，attempt 未结束 hello 重报 local_vars/配置 apply 到 rev=6，再调 TaskInputs」会返回 rev6 值——同一 attempt 两套输入，且库内 rev 仍写 5。
- 「G2 应在 grant 时调一次」仅是注释（task.go:385），无 API 强制；测试只覆盖 ns 钉版与换节点重解析（task_inputs_test.go:46、123-131、65-140），**没有同 attempt 中途改配置的反例**。
- 最小建议：在 GrantLease 事务内解析机器层并入返回/落 per-attempt 快照（A1），或 TaskInputs 校验 `config_revision` 与当前 node_configs.revision 不一致即报 stale（fail-closed）。

**[MAJOR-2] lease_instance_id 单列只能在 regrant 前绑定旧 attempt 的 reap ACK；regrant 后旧槽既不能按事实释放、也不会「不泄漏」**
- AckCancel 先核 attempt，不匹配直接 ErrLeaseMismatch、**任何槽都不释放**（task.go:1226-1228；释放点 :1235/:1248/:1284）；而 attempt2 的 grant 已把 lease_instance_id 覆盖为本 attempt 槽（task.go:863）。旧 attempt 的 reaped=true ACK 在 regrant 后永远对不上（测试明确断言 busy=2 且拒绝，task_test.go:763-768）。
- 旧槽唯一后续出口：节点 offline 的会话死亡事实（task.go:1455-1461）或 ReclaimStaleSlots 的 phase 推断（见 MAJOR-3）。即「旧槽不永久泄漏」不成立：新 attempt 整个生命周期内旧槽被永久占用（N 槽节点每次重叠重试烧 1 槽；单槽节点重试直接卡 waiting_for_slot），只能等任务终态后的 hello 兜底。
- requeue 保留 lease_instance_id 是正确的一半（task.go:1429-1431 未清），但它只覆盖「pending 窗口」；测试 task_test.go:1169-1210 只测了这个容易窗口，没测 regrant 后。
- 最小建议：per-attempt 槽位历史（A1 `task_attempts(attempt, instance_id, node_id)`），AckCancel 按传入 attempt 查该 attempt 的实例释放，而非读任务行当前列。

**[MAJOR-3] ReclaimStaleSlots 只看 DB phase/归属就释放 = 假 reaped，与同函数注释自相矛盾**
- SQL 判据仅「busy_task 指向的任务不存在/不属本节点/phase≠running」（node.go:729-737），没有任何 processGroupReaped/实例代际事实；本提交新增注释却声称「槽位只在 cancel.ack.reaped=true（或租约回收）之后释放」（node.go:723-724），task.go:1306-1308 也声称取消终局保持占用。
- 效果：Cancel 终局或 abandon 终局后，节点下一次 hello 即可清掉「进程可能还在跑」的槽；测试把该行为固化为期望（task_test.go:637、789-795「终态后残影槽位应被回收」）。触发点是每次连接 hello（node_handshake.go:286），若 agent 断线重连不杀在跑子进程，就会同槽双任务。
- 最小建议：hello 只回收「节点自报已无该实例进程」的槽，或带 instance/attempt 代际（reaped fact）；不要把 phase≠running 当作物理回收依据。

**[MINOR-4] lease 过期后的 result 被拒，但在线节点没有「本 attempt 进程已退出」的释放入口**
- CompleteTaskAt 拒过期 result（task.go:1049-1051），释放只在被接受的 result（task.go:1065）；AckCancel 仅 cancel 语义；该提交内 `ReleaseSlot` 除 grant 回滚外无生产调用者（task.go:747）。节点跑完 attempt1 时租约已过期 → 无法申明「实例 X 已空」，只能等重连 hello（即 MAJOR-2/3 的泄漏面）。建议新增 attempt+instance 维度的 reaped 释放 API。

## 待验证
- WS 重连是否重发 hello 且不杀在跑子进程（anode 侧未读，超预算）；若是，MAJOR-3 的假 reaped 立即成立。
- ReclaimStaleSlots 是否只在进程启动 hello 调（node_handshake.go:286 在 applyHello 内，注释称「进程刚起来」信号，但每次连接都会走）。
- 生产 DSN 是否确含 `_txlock=immediate`（仅由 pool_test.go:8/164-168 与注释证明，未读 open 代码）。

## 未覆盖
- 按要求未重侦察稳定 source id/emit 事务；未跑 test/build；只读固定提交（git show），未看未提交文件。

{"ok": false, "reason": "3 major: machine-layer TaskInputs not per-attempt pinned (drifts after grant), lease_instance_id single column cannot release old-attempt slot after regrant (leaks whole new attempt), ReclaimStaleSlots frees slots on DB phase only = false reaped; cancel/drain/budget/expiry-result fixes verified"}
