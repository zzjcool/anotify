## G1 只读审查结论：不通过

**Critical 0 / Major 2 / Minor 1。**
核对 HEAD=`5debf01f`；指定测试文件自 `b99db8a` 至 HEAD 无变化。以下是覆盖缺口，不是已复现的产品故障。

### Major findings

1. **非法结果相位的“零写”断言未覆盖完整槽位与 registry 事实。**
   - `internal/store/task_test.go:827-866` 比较 Task、当前 attempt，检查 attempt 数量；槽位仅比较 **busy 数量**。
   - 缺少 node_instances 的完整绑定，以及 artifact manifests/uploads/objects 快照。因此，同数量下改写 `BusyLeaseID` / `BusyAttempt`，或写入 registry，仍可能通过。
   - **缺失的不变量：拒绝非法相位后，wholeTask/attempt/ledger/result/registry/slot facts 全部不变**；这是 `.pi-orchestrator/v4/teams/core-runtime/task-cards/DELIVERY-B.md:32` 的明确要求。
   - 现有完整快照 helper 位于 `internal/store/v4test/artifact_registry_test.go:42-47`，但这个新增测试未使用等价检查。
   - 注意：当前相位检查在 `internal/store/execution_auth.go:689-692`，早于认证和 `BeginTx`（`:703-710`）。本测试确实调用真实入口并验证相位拒绝，**不证明事务回滚**。

2. **删除完整索引形状测试后，指定替代测试只覆盖部分列数。**
   - 原 `TestBaselineIndexes` 在 B 差异中删除；映射将其标为 retired：`.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md:668`。
   - 替代测试 `internal/store/v4test/schema_baseline_test.go:243-254` 对 events 只检查列数，对 tasks 只检查列数 ≥2，未固定这些索引的列名、顺序及 partial/unique 属性。
   - **仍然有效的原不变量：查询索引的有序前缀必须正确。** 当前 `internal/store/schema.sql:416-417` 仍明确要求 `idx_tasks_ns_created(namespace_id, created_at, id)`；替代断言却会接受其他两列。
   - `validateV4Layout` 的期望来自同一份 schema（`internal/store/migrate.go:490-507`），其索引匹配检查针对 unique（`:454-462`），不能补足普通查询索引的独立形状验收。
   - 这不是要求恢复 v3 `status`、旧表或迁移链；缺的是**当前 v4 索引形状**的等价断言。

### Minor finding

- **“MigrationCancelledContext”实际只是重复的驱动 BeginTx 检查。**
  - `internal/store/v4_whitebox_test.go:237-257` 先成功运行 migrate，再对裸 `*sql.DB.BeginTx` 提交已取消 context；没有带取消信息调用迁移入口。
  - 与同文件 `:411-418` 的取消 BeginTx 检查重复。
  - 它证明“取消 context 不开始事务”，不证明“迁移取消后 ledger 数据不变”；检查也只有 marker/object names，未比较 ledger 内容。属于覆盖命名夸大，不否定其他真实回滚测试。

## 已确认的事实与调用链

- **真实初始化回滚成立，非前置拒绝：**
  `v4_whitebox_test.go:121-190` 分别构造 DDL 中途失败、ledger INSERT trigger 拒绝、事务内 hook 失败。调用链为
  `applyBaselineMigration(db, migration{version,name,body}, hook)` → `applyMigrationTx(..., true)` → marker → ledger DDL → body → hook → ledger INSERT → commit；失败由 deferred rollback 收口（`migrate.go:793-834`）。失败后检查 marker=0、用户对象为空（`v4_whitebox_test.go:54-62`）。

- **连续/gap 解析确实执行生产解析器：**
  `v4_whitebox_test.go:65-118` 直接调用 `parseMigrations(src)`，检查连续序列、缺号、重复与逆序；实现校验位于 `migrate.go:105-140`。

- **oversized 混批测试有真实事务失败和有效边界断言：**
  `task_test.go:755-810` 检查错误详情、合法/超限两条 trace 均零行、精确上限接受及字节往返；调用真实 `CreateTasks` 事务入口（`task.go:254-268`）。不是空断言。

- **投影与 raw result 的分离确有判别力：**
  `task_test.go:885-908` 要求 `Task.error="exit_code"` 而 `ResultJSON.error=""`；`:953-1002` 检查成功清旧投影及保留非空 submitted Error。纳秒和 nullable 事实另有具体断言（`v4test/result_facts_test.go:33-58,131-225`），未因日志为绿而推定覆盖。

- **fixture 修正未削弱所读授权/保留期后置条件：**
  authz 使用真实 CAS 删除后仍要求立即失权（`authz_test.go:84-96,126-137`）；retention 通过真实 grant → lease recovery → 精确历史 reap，仍要求未 reap 零删除、reap 后恰好删除一行（`retention_test.go:187-223`）。

**纯 v3 退役不计问题：**旧 migration2/10、preMigration8 链及旧字段形状，可按批准的 v4 全量 baseline 替换规则退役；不要求旧库兼容。

## 待验证与验证记录

- 未运行 baseline、lint/vet/test；不把既有 exit=0 日志当覆盖证明。
- `git diff --check 5d44685..b99db8a -- <指定测试文件>` → **exit 0**。
- 改动文件：**无**；新增测试：**无**。
- `RESULT-ERROR-PROJECTION.md` 本地未找到；已向父会话报告，采用 `DELIVERY-B.md:32` 的冻结摘要。
- 两次补充索引引用 grep 返回 rejected（错误文本未保留）；停止重试，改用目标文件读取。未作全仓无遗漏声明。

{"ok": false, "reason": "G1 readonly review complete: 2 major coverage gaps (invalid-phase whole-facts zero-write assertions; retained v4 index-shape assertions), 1 minor redundant cancellation claim; real baseline transaction rollback confirmed"}
