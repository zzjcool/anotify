package store

// 中枢侧的语义钉子：`seq_in_trace` = 「同一 (namespace, trace, 能力名) 里的第几次执行」（1 起）。
//
// 它由 assignTaskTx 在**派发时**用一条 UPDATE 算出来（子查询按 rowid 数同一组里更早的行），
// 因此有四条必须成立的性质，正是本文件逐个钉住的东西：
//
//	① 同一批并发派发的 N 条任务拿到 {1..N}——不重不丢（写事务串行 + 单语句内的子查询）；
//	② 分组口径是能力**名字**：同一 trace 里 review@1 与 review@2 连续，different 名字互不干扰；
//	③ 定序口径是 **rowid**：与「谁先被派发」无关，同一秒扇出的任务也有确定先后；
//	④ 重派同一条任务 seq 不变、attempts 递增（它记的是「行在组内的位置」，不是派发次数）。
//
// 为什么这四条值得钉：节点把每次执行落到 runs/<trace>/<cap>@<ver>/<seq>-a<attempt>/，
// seq 一旦重号，两条执行的现场证据就互相覆盖；seq 一旦随派发漂移，同一个目录名会指向
// 两次不同的执行（回看 CLI 给出的就是错的那份日志）。

import (
	"context"
	"sync"
	"testing"
)

// seqTestLease 是测试里的租约时长（秒）；本文件不考租约，只要是个未来时刻即可。
const seqTestLease = int64(3600)

// mkSeqTaskBatch 在同一个 trace + 同一个能力下建 n 条 pending 任务。
//
// 一次 CreateTasks = 一个事务：这就是生产里「扇出」的形状（同一秒建、rowid 递增），
// 也正是 ③ 里 rowid 定序的用武之地。
func mkSeqTaskBatch(t *testing.T, db *DB, nsID, traceID string, cap *CapabilityVersion, n int) []*Task {
	t.Helper()
	batch := make([]*Task, n)
	for i := range batch {
		batch[i] = mkTask(nsID, traceID, cap)
	}
	if err := db.CreateTasks(context.Background(), batch); err != nil {
		t.Fatalf("create %d tasks under %s: %v", n, traceID, err)
	}
	return batch
}

// seqOf 读一条任务的 (attempts, seq_in_trace)（都从库里回读，不看内存值）。
func seqOf(t *testing.T, db *DB, nsID, taskID string) (attempts, seq int) {
	t.Helper()
	got, err := db.GetTask(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	return got.Attempts, got.SeqInTrace
}

// assertSeqSetExactly 断言这批任务拿到的 seq 恰好是 {1..n}（不重、不丢、不越界）。
func assertSeqSetExactly(t *testing.T, seqs []int) {
	t.Helper()
	owner := make(map[int]int, len(seqs))
	for i, seq := range seqs {
		if seq < 1 || seq > len(seqs) {
			t.Fatalf("任务 #%d 的 seq_in_trace = %d，超出 [1,%d]（1 起、且不超过组内行数）",
				i, seq, len(seqs))
		}
		if prev, dup := owner[seq]; dup {
			t.Fatalf("seq_in_trace = %d 被任务 #%d 与 #%d 同时占用（{1..%d} 必须不重不丢）",
				seq, prev, i, len(seqs))
		}
		owner[seq] = i
	}
	if len(owner) != len(seqs) {
		t.Fatalf("只收到 %d 个不同的 seq_in_trace，want %d", len(owner), len(seqs))
	}
}

// TestSeqInTraceConcurrentAssignIsUnique ①：同一 trace 同一能力的 N 条任务**并发**派发，
// seq 必须恰好是 {1..N}。
//
// 用文件库而不是 :memory:：内存库被强制成单连接（store.openWithOptions 的语义约束），
// 单连接下所有写者排成一队，测不出「两条任务同时读到同一个 count」这个窗口。
func TestSeqInTraceConcurrentAssignIsUnique(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	node, _ := mkNode(t, db, nsID, "mbp-seq-concurrent")

	const n = 16
	batch := mkSeqTaskBatch(t, db, nsID, "trace_seq_concurrent", capv, n)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 直接 AssignTask（不占槽位）：本用例只考 seq 的并发计算。
			errs[i] = db.AssignTask(ctx, nsID, batch[i].ID, node.ID, "", Now()+seqTestLease)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发派发第 %d 条失败: %v", i, err)
		}
	}

	seqs := make([]int, n)
	for i := range batch {
		attempts, seq := seqOf(t, db, nsID, batch[i].ID)
		if attempts != 1 {
			t.Errorf("任务 #%d 的 attempts = %d，want 1（首次派发）", i, attempts)
		}
		seqs[i] = seq
	}
	assertSeqSetExactly(t, seqs)
	// 更强的一条：seq 是 rowid 位置的函数 ⇒ 与建行顺序一一对应（同一批 CreateTasks 内 rowid 递增）。
	for i := range batch {
		if seqs[i] != i+1 {
			t.Errorf("第 %d 条（建行顺序）的 seq_in_trace = %d，want %d", i, seqs[i], i+1)
		}
	}
}

// TestSeqInTraceConcurrentLeaseAndAssignIsUnique ①（生产路径）：LeaseAndAssign
// （占槽 + pending→assigned 一个事务）并发派发同样得到 {1..N}。
//
// 与上一条的区别：这条走的是 route 真正用的函数（store.LeaseAndAssign），
// 且每条任务还各自占住一个不同的槽位——两条路径共用 assignTaskTx，必须一字不差。
func TestSeqInTraceConcurrentLeaseAndAssignIsUnique(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	const n = 8
	node, _ := mkNode(t, db, nsID, "mbp-seq-lease")
	specs := make([]InstanceSpec, n)
	for i := range specs {
		specs[i] = InstanceSpec{Slot: i, Concurrency: 1}
	}
	if _, err := db.UpsertInstances(ctx, nsID, node.ID, specs); err != nil {
		t.Fatalf("upsert %d instances: %v", n, err)
	}

	batch := mkSeqTaskBatch(t, db, nsID, "trace_seq_lease", capv, n)

	var wg sync.WaitGroup
	insts := make([]string, n)
	errs := make([]error, n)
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			insts[i], errs[i] = db.LeaseAndAssign(ctx, nsID, batch[i].ID, node.ID, Now()+seqTestLease)
		}(i)
	}
	wg.Wait()
	slots := make(map[string]int, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 LeaseAndAssign 第 %d 条失败: %v", i, err)
		}
		if prev, dup := slots[insts[i]]; dup {
			t.Fatalf("槽位 %s 被第 %d 与第 %d 条任务同时占住", insts[i], prev, i)
		}
		slots[insts[i]] = i
	}

	seqs := make([]int, n)
	for i := range batch {
		attempts, seq := seqOf(t, db, nsID, batch[i].ID)
		if attempts != 1 {
			t.Errorf("任务 #%d 的 attempts = %d，want 1", i, attempts)
		}
		seqs[i] = seq
	}
	assertSeqSetExactly(t, seqs)
}

// TestSeqInTraceGroupsByTraceAndCapabilityName ②：分组口径是
// (namespace, trace, 能力**名字**)——版本不同但同名属于同一个能力，不同能力互不干扰，
// 不同 trace 各自从 1 起。
func TestSeqInTraceGroupsByTraceAndCapabilityName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	review1 := mkCap(nsID, "review", 1, []string{"fix"})
	review2 := mkCap(nsID, "review", 2, []string{"fix"}) // 同名不同版本 = 同一个能力的两次执行
	notify := mkCap(nsID, "notify", 1, []string{"fix"})
	for _, c := range []*CapabilityVersion{review1, review2, notify} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create capability %s@%d: %v", c.Name, c.Version, err)
		}
	}
	node, _ := mkNode(t, db, nsID, "mbp-seq-groups")

	const trace = "trace_seq_groups"
	first := mkSeqTaskBatch(t, db, nsID, trace, review1, 3)  // review@1 ×3
	second := mkSeqTaskBatch(t, db, nsID, trace, review2, 2) // review@2 ×2（同一能力，seq 接着数）
	other := mkSeqTaskBatch(t, db, nsID, trace, notify, 2)   // notify ×2（另一个能力）
	// 另一条 trace 的同名能力：seq 必须重新从 1 起。
	foreign := mkSeqTaskBatch(t, db, nsID, "trace_seq_other", review1, 1)

	// 交叉派发（顺序刻意与建行顺序不同）：seq 只取决于组内 rowid 位置。
	for _, batch := range [][]*Task{other, second, first, foreign} {
		for _, task := range batch {
			if err := db.AssignTask(ctx, nsID, task.ID, node.ID, "", Now()+seqTestLease); err != nil {
				t.Fatalf("assign %s: %v", task.ID, err)
			}
		}
	}

	seqsOf := func(batch []*Task) []int {
		out := make([]int, len(batch))
		for i, task := range batch {
			_, out[i] = seqOf(t, db, nsID, task.ID)
		}
		return out
	}

	// review@1 ×3 → 1,2,3；review@2 在同一个 seq 空间里接着数 → 4,5。
	for i, seq := range seqsOf(first) {
		if seq != i+1 {
			t.Errorf("review@1 第 %d 条 seq = %d，want %d", i, seq, i+1)
		}
	}
	for i, seq := range seqsOf(second) {
		if want := 4 + i; seq != want {
			t.Errorf("review@2 第 %d 条 seq = %d，want %d（分组按能力**名字**：同 trace 的 review 应当连续）",
				i, seq, want)
		}
	}
	// notify 自成一档：1,2（不被 review 的行数推高）。
	assertSeqSetExactly(t, seqsOf(other))
	for i, seq := range seqsOf(other) {
		if seq != i+1 {
			t.Errorf("notify 第 %d 条 seq = %d，want %d（不同能力互不干扰）", i, seq, i+1)
		}
	}
	// 另一条 trace：从 1 起。
	if _, seq := seqOf(t, db, nsID, foreign[0].ID); seq != 1 {
		t.Errorf("另一条 trace 的 review seq = %d，want 1（trace 是分组维度之一）", seq)
	}
}

// TestSeqInTraceStableAcrossRedispatch ③④：逆序派发时 seq 仍按建行顺序（= rowid）给号；
// 重派同一条任务一次，seq 不变而 attempts 递增。
func TestSeqInTraceStableAcrossRedispatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	node, _ := mkNode(t, db, nsID, "mbp-seq-redispatch")

	batch := mkSeqTaskBatch(t, db, nsID, "trace_seq_redispatch", capv, 3)

	// 逆序派发：如果 seq 记的是「第几个被派发」，这里会得到 3,2,1 的反向映射。
	for i := len(batch) - 1; i >= 0; i-- {
		if err := db.AssignTask(ctx, nsID, batch[i].ID, node.ID, "", Now()+seqTestLease); err != nil {
			t.Fatalf("assign #%d: %v", i, err)
		}
	}
	for i := range batch {
		attempts, seq := seqOf(t, db, nsID, batch[i].ID)
		if attempts != 1 {
			t.Errorf("任务 #%d 首次派发后 attempts = %d，want 1", i, attempts)
		}
		if want := i + 1; seq != want {
			t.Fatalf("任务 #%d 的 seq = %d，want %d：seq 按 rowid 定序，"+
				"与派发顺序无关（否则同一秒扇出的任务在节点侧会撞目录）", i, seq, want)
		}
	}

	// 重派中间那条：assigned → lease_expired → pending → assigned。
	target := batch[1]
	if err := db.Transition(ctx, nsID, target.ID, TaskAssigned, TaskLeaseExpired); err != nil {
		t.Fatalf("transition to lease_expired: %v", err)
	}
	if err := db.Transition(ctx, nsID, target.ID, TaskLeaseExpired, TaskPending); err != nil {
		t.Fatalf("transition to pending: %v", err)
	}
	if err := db.AssignTask(ctx, nsID, target.ID, node.ID, "", Now()+seqTestLease); err != nil {
		t.Fatalf("re-assign: %v", err)
	}

	attempts, seq := seqOf(t, db, nsID, target.ID)
	if attempts != 2 {
		t.Errorf("重派后 attempts = %d，want 2（每次派发 +1）", attempts)
	}
	if seq != 2 {
		t.Errorf("重派后 seq = %d，want 2（seq 记的是行在组内的位置，重派不该让它漂移）", seq)
	}
	// 其余两条不受影响。
	for _, i := range []int{0, 2} {
		if _, got := seqOf(t, db, nsID, batch[i].ID); got != i+1 {
			t.Errorf("重派中间那条之后，任务 #%d 的 seq = %d，want %d", i, got, i+1)
		}
	}
}

// TestSeqInTraceRoundTrip 往返一致性：建行时这一列由库给默认值（0 = 从未派发），
// 派发后经 GetTask / ListTasks 两条读路径都必须读到同一个值。
//
// 同一条用例也钉住 §2.2 的冻结决定：createTasksTx 的 INSERT **不写** seq_in_trace，
// 所以建行方在 Task 上填的值会被忽略——它是派发时算出来的派生量，不是调用方的输入。
func TestSeqInTraceRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	node, _ := mkNode(t, db, nsID, "mbp-seq-roundtrip")

	handwritten := mkTask(nsID, "trace_seq_roundtrip", capv)
	handwritten.SeqInTrace = 7 // 建行时手填：必须被忽略（DEFAULT 0）
	if err := db.CreateTasks(ctx, []*Task{handwritten}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, seq := seqOf(t, db, nsID, handwritten.ID); seq != TaskSeqInTraceUnknown {
		t.Fatalf("建行后的 seq_in_trace = %d，want %d：这一列只由派发写入",
			seq, TaskSeqInTraceUnknown)
	}

	second := mkSeqTaskBatch(t, db, nsID, "trace_seq_roundtrip", capv, 1)[0]
	for _, task := range []*Task{handwritten, second} {
		if err := db.AssignTask(ctx, nsID, task.ID, node.ID, "", Now()+seqTestLease); err != nil {
			t.Fatalf("assign %s: %v", task.ID, err)
		}
	}

	// GetTask 路径
	got, err := db.GetTask(ctx, nsID, second.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.SeqInTrace != 2 {
		t.Errorf("GetTask seq_in_trace = %d，want 2", got.SeqInTrace)
	}
	// ListTasks 路径（共用 taskColumns + scanTask，列顺序漂了这里会读成别的值）
	list, err := db.ListTasks(ctx, nsID, "trace_seq_roundtrip", "", "", 10)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	byID := make(map[string]*Task, len(list))
	for _, task := range list {
		byID[task.ID] = task
	}
	for id, want := range map[string]int{handwritten.ID: 1, second.ID: 2} {
		task, ok := byID[id]
		if !ok {
			t.Fatalf("ListTasks 里找不到任务 %s", id)
		}
		if task.SeqInTrace != want {
			t.Errorf("ListTasks 里 %s 的 seq_in_trace = %d，want %d", id, task.SeqInTrace, want)
		}
	}
	if len(list) != 2 {
		t.Fatalf("ListTasks 返回 %d 条，want 2", len(list))
	}
}

// TestSeqInTraceZeroUntilDispatched 记录零值语义：没派发过的行是 0，不是 1。
//
// 节点侧据此退回到 attempts（旧中枢不带宽这个字段）。如果哪天把默认值改成 1，
// 「第一次执行」与「不知道」就再也分不开了。
func TestSeqInTraceZeroUntilDispatched(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(context.Background(), withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	batch := mkSeqTaskBatch(t, db, nsID, "trace_seq_zero", capv, 3)
	for i, task := range batch {
		if attempts, seq := seqOf(t, db, nsID, task.ID); seq != TaskSeqInTraceUnknown || attempts != 0 {
			t.Fatalf("第 %d 条未派发的任务 attempts/seq = %d/%d，want 0/%d",
				i, attempts, seq, TaskSeqInTraceUnknown)
		}
	}
}

// TestMigration5SeqInTraceWorksOnExistingDB 迁移 5 的升级路径：一个 v4 的库（tasks 还没有
// seq_in_trace 列）重开时必须把这一列加回来，且历史行读到的是 0（未知）。
//
// 为什么值得单独测：迁移是**按版本记账、只跑一次**的，全新库覆盖不到「ALTER 打在有数据的
// 老表上」这条路径——`NOT NULL DEFAULT 0` 在老行上是否会失败、是否会把老行写成别的值，
// 只有对一张已经有行的表执行才看得见。
func TestMigration5SeqInTraceWorksOnExistingDB(t *testing.T) {
	db, path := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(capv)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	// 迁移 5 之后建的任务行（模拟“老库里的历史数据”）。
	old := mkTask(nsID, "trace_migration5", capv)
	if err := db.CreateTasks(ctx, []*Task{old}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET seq_in_trace = 4 WHERE id = ?`, old.ID); err != nil {
		t.Fatalf("seed seq_in_trace: %v", err)
	}

	// 把它退回 v4 的形状：列没了、版本记账也删掉。
	mustExec(t, db, `ALTER TABLE tasks DROP COLUMN seq_in_trace`)
	mustExec(t, db, `DELETE FROM schema_version WHERE version = ?`, 5) // 迁移 5 = seq_in_trace；别用 headVersion（它随新增块漂移）
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a v4-shaped db: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if v, err := db2.SchemaVersion(); err != nil || v != headVersion {
		t.Fatalf("重开后 schema 版本 = %d (err=%v)，want %d", v, err, headVersion)
	}
	// 历史行拿到的是默认值 0（= 未知），不是 NULL、也不是 1。
	got, err := db2.GetTask(context.Background(), nsID, old.ID)
	if err != nil {
		t.Fatalf("get task after migration: %v", err)
	}
	if got.SeqInTrace != TaskSeqInTraceUnknown {
		t.Fatalf("老行的 seq_in_trace = %d，want %d（默认 0 = 未知）", got.SeqInTrace, TaskSeqInTraceUnknown)
	}
	// 升级后的库照常能派发（列真的在，且 seq 从 1 起）。
	if err := db2.AssignTask(context.Background(), nsID, old.ID, "", "", Now()+seqTestLease); err != nil {
		t.Fatalf("assign on migrated db: %v", err)
	}
	if _, seq := seqOf(t, db2, nsID, old.ID); seq != 1 {
		t.Errorf("迁移后派发的 seq = %d，want 1", seq)
	}
}
