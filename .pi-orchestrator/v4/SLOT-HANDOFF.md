# V4 物理槽位与历史回收接缝 · A3（父技术裁决）

目的：补齐 D4 的物理槽位/历史 attempt/回收事实。不增加新帧名、bootId/occupancySeq 控制框架或业务配额；Node limits.total/perCapability 与能力 maxPerNode 不变。T3 为 protocol/节点唯一作者，T1 为存储/中枢消费者。逻辑 Task 仍一行。

## 1. 标识与职责

- 执行帧中已有 `node` 字符串指已登记的集群 `metadata.name`（即 anode.yaml.name），不是数据库内部 Node UID；中枢由认证会话映射数据库 nodeId，不能让正文 node 自由选择身份或记录。
- 使用现有 Node 实例槽号 `slot`：整数 **0..N-1**，每槽最多一个能力进程；数据库 instanceId 是内部键，不是 wire slot。新分配必须符合本机当前有效预算，缩小预算时仍报告超出新范围的存量占用直至回收。
- Node在 `task.accept` 前原子预留真实空闲槽，accept回报slot；中枢候选容量只是已知事实提示，grant事务必须复核该slot未绑定其它执行、Node/ns权限与预算、assignment当前有效。
- 不在 dispatch 授权或让中枢自行猜另一个槽；只有 `lease.grant` 才持久计attempt/授权启动。旧 attempt 的真实槽必须由 task_attempts 与节点物理事实保留，不被当前 task 行覆盖。

## 2. 既有帧增补（无新帧）

- `task.dispatch`：保持 assignmentId/proposedAttempt，新增中枢给出的 `assignmentExpiresAt`（统一有界派发窗口，初值30秒）；不含执行lease/token/slot授权。Node和中枢以这同一窗口判pending reservation，不分别猜30秒起点。
- `task.accept`：新增**必填有presence的 `slot`**，可用 `*int` JSON类型表达，0有效、缺失或负数无效；保留taskId/assignmentId/proposedAttempt/node。
- `lease.grant`：回同一 `slot`，并带本次 `assignmentId`；保留taskId/attempt/node/leaseId/attemptToken/expiresAt。Node必须有匹配的pending reservation（taskId+assignmentId+proposedAttempt+slot）才允许启动，不能收到任意grant就执行。
- `task.cancel` 与 `cancel.ack`：增加 `leaseId` 与有presence的 `slot`，保留taskId/attempt与既有reason/graceSec/processGroupReaped。Node取消必须按完整历史绑定定位，不停止同task的新attempt。
- `task.cancel` / `cancel.ack` 的显式校验必须拒绝缺失 / null slot 或负数；present slot=0 合法，不得因条件只检查“非nil负数”而让缺绑定回执通过。
- `cancel.ack`在进程组真正回收后发送。中枢从连接验证当前Node身份，再按**历史 task_attempts**的node+taskId+attempt+leaseId+slot核验；旧attempt当前执行token已无效也可报告物理回收，但不能因此让旧result/emit恢复授权。
- 同时绑定Node槽账本到attempt/lease或等效唯一执行标识，释放旧ACK必须CAS匹配旧occupancy，不能仅检查busy_task==同一个logicaltask而清掉新attempt。重复ACK幂等；无匹配绑定不能清任何槽。该物理回收事实也可在自然结束、lease失效或重连恢复后补报；不是重新授权的任务结果，不要求旧执行仍running或必须收到一条当前cancel请求才能回收。

## 3. 启动前容量拒绝与预留生命周期

- 正式扩 `task.reject.reason` 封闭集合为 **`stale_config | slot_unavailable`**。missing_requirement/workdir/home/export错误仍在grant后计attempt失败，不进入reject。
- Node无真实空槽、预算吃满或本地已有占用时：`slot_unavailable`，不执行、不授lease、不计attempt，Task回pending/waiting_for_slot，带有界退避；不能用stale_config伪装容量不一致。unknown reject reason fail-closed。
- 同一有效assignment重复dispatch/accept幂等回同一reservation，不占第二槽；不同assignment不能共享reservation。
- pending reservation从accept到grant必须有有界期限（使用dispatch.assignmentExpiresAt，中枢窗口初值30秒）。未授执行权且本机确知从未创建进程的reservation，可在窗口过期/连接关闭/assignment失败后释放；不能把这个规则用来释放已授权/启动的进程槽。
- 中枢grant与assignment过期/取消/slot绑定检查在同一事务，窗口已失效不得新grant。晚到/不匹配grant在Node不执行；若中枢已grant计attempt而Node无法使用已失效reservation，按现有结构化基础设施失败/lease回收处理，不倒减attempt、不伪装成accept前拒绝。

## 4. Hello/applied 的显式实例事实

- `HelloInstance.slot`有presence（0有效）；`busy`改为**可区分缺失的 bool**（例如 `*bool`）。缺busy、缺instances、空默认数组均为未知或无新断言，不能当已有进程已退出；重复slot或非法记录不更新安全账本。
- `busy=true`的已执行/回收中占用给 `taskId/attempt/leaseId`；尚未grant的reservation可给taskId/assignmentId/proposedAttempt，但不伪造leaseId或运行attempt授权。该项同样不能计成空闲。
- `busy=false`是具体slot的本机无进程断言。若中枢该slot尚有绑定，必须带上相符的历史taskId/attempt/leaseId或单独真实cancel.ack，才允许回收**该旧绑定**；无匹配引用的idle记录不能清旧busy。
- 现有HelloInstance `concurrency`是旧实例一次多份语义，v4每任务一槽不以它作为额外预算；本次移除该wire字段，容量只由limits和真实slot记录决定。
- `config.applied`允许附同类型 `instances` 快照，使配置应用后本机真实预算/slot状态可被上报；未附不更新实例账本。hello与apply都不能仅依据数据库phase/本地Task对象被删就报idle。
- Node重启/重连必须恢复或核查本机持久执行记录与进程组，未确认reaped保持busy/unknown；不能因为新进程内存为空或新hello到达就假物理空。

## 5. 存储、候选与离线

- T1 `GrantLease`消费accepted slot，原子映射node+slot到instance，记录attempt/lease/snapshot；不再次 `LeaseSlot`挑另一个。Node不存在、超预算、该slot已有其它未回收occupancy或assignment失效不得grant。
- machine snapshot在grant事务固定，namespace仍logicaltask首次pin；HTTP诊断不返回完整环境值。task_attempts供历史日志、回收、产物引用和GC使用，不新增logicaltask。
- 不用ReclaimStaleSlots的taskphase判定当物理证据。离线可撤销授权/停止派发，容量账本在该节点回online前必须依真实实例/停止确认对齐；不能上线立即复用还活旧进程槽。

## 6. 交接与验收

- T3更新共享Go定义、JSON标签、presence/合法值校验、方向和fixture，再给 **已冻结63570df→最新protocol** 的纯增量patch与HANDOFF（包含此前A2 EnvArtifactDir/路径headerguard时清楚标全范围）；T1不私改镜像、不重复N1。
- T1 G1-5b writer先settle，串行G1-6S/R2消费新接口并完成attempt/slot/snapshot；T3当前N2B同writer接新字段，不开并发writer。
- 必测：slot0 present与missing区分、缺busy不reap、explicit false+匹配旧refs方可回收；accept预留未grant不执行/过期可安全释放；重复assignment不多占/无槽reject不计attempt；grant echo slot匹配；取消/旧ACK错误lease或slot拒且不清新槽；旧ACK重复幂等；rehello phase终态仍有活process不能idle；配置变化同attempt固定、新attempt新snapshot；预算缩小不假释放存量。
- 本补充是必要数据相关性与容量拒绝，不增加 paths lock、全局at-most-once承诺或新权限模型。原E1授权/业务id事务与A1/A2产物语义不变。
