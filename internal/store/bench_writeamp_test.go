package store

// #237 写放大的**可复现度量**（契约 §3 修正②）——留在仓库里，不用每次重造探针。
//
// 迁移 16 给 tasks 加了 (namespace_id, created_at, id) 索引：读路径因此能走覆盖索引
// 反向扫、没有 TEMP B-TREE（见 migrate16_test.go 的 EXPLAIN 断言）。代价在**写**路径——
// 每插一行都要多维护一棵 b-tree（插入 + 可能的页分裂）。契约要求把这份代价量出来、
// 并且**可复现**，而不是「应该不大」。
//
// ── 怎么用 ────────────────────────────────────────────────────────────────────
//
// 两条命令，除了 A/B 开关之外逐字相同（都跑 20 轮、5 个样本，取中位数）：
//
//	# A：带索引（生产形态）
//	taskset -c 8-11 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksWriteAmplification' -benchtime 20x -count=5
//
//	# B：不带索引（先 DROP INDEX idx_tasks_ns_created 再插）
//	WRITEAMP_DROP_INDEX=1 taskset -c 8-11 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksWriteAmplification' -benchtime 20x -count=5
//
// **判据（冻结）：带/不带索引的中位数差 > 10% ⇒ 写放大不可接受**，必须如实写进报告
// 并给取舍建议（不许调参掩盖、不许删索引了事）。
//
// ── 为什么这样量 ──────────────────────────────────────────────────────────────
//
//   - **1 个事务插 1000 行**是契约 §3 修正② 冻结的协议，也对应派发路径的真实形状
//     （一条 trace 扇出的一批任务在一个事务里落库）。1000 行足够让索引维护的常数项
//     淹没事务开启/提交的固定开销。
//   - 同一事务的 1000 行**共用同一个 created_at**（真派发的 Now() 是批次级的），
//     于是这台索引上的插入落在同一个键前缀里 —— 这恰恰是最坏也最真实的那种
//     tie-breaking 形状，比"均匀散开"更能暴露页分裂成本。
//   - 跑在 `:memory:` 上：要量的是**索引维护本身**（b-tree 插入/页分裂的 CPU 代价），
//     不想让 WAL 的 fsync 噪声混进来（同机器上 fsync 抖动常常比索引代价大一个量级）。
//     这个方向是保守的：磁盘上除了索引维护还要多写索引页，真实写放大 ≥ 这里的差值 ——
//     报告里写清这一点，别把内存上的数字当成磁盘代价的上界。
//   - 建行对象（`Task` 结构体）放在 b.StopTimer() 段：它是调用方的成本、在两臂里完全
//     相同，留在计时区只会稀释差值。计时区内**只有** CreateTasks。
//
// 这不是 CI 门禁（是 bench）：`-run '^$'` 不跑任何用例，`go test ./internal/store/` 也不含它。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

const (
	// writeAmpRows 是一次事务插入的行数（契约要求 ≥1000）。
	writeAmpRows = 1000

	// writeAmpDropIndexEnv 是 A/B 开关：=1 时先 DROP 掉列表索引，量「没有这条索引」的写成本。
	writeAmpDropIndexEnv = "WRITEAMP_DROP_INDEX"

	// writeAmpIndexName 必须与迁移 16 的索引名一致（migrate16_test.go 的 tasksListIndexName）。
	// 这里单独写一份常量是为了让本文件自解释；两侧是否漂了由下面 DROP 之后的存在性断言兜住。
	writeAmpIndexName = "idx_tasks_ns_created"
)

// BenchmarkCreateTasksWriteAmplification 量「一条索引在写路径上的纯代价」。
//
// 两条臂的行为差异**只有** DROP INDEX 这一步，其余（同一份代码、同一个夹具、同样的事务
// 形状、同样的行数）逐字相同，所以 ns/op 的差值就是这条索引的维护成本。
func BenchmarkCreateTasksWriteAmplification(b *testing.B) {
	ctx := context.Background()
	dropIndex := os.Getenv(writeAmpDropIndexEnv) == "1"

	db, err := Open(":memory:")
	if err != nil {
		b.Fatalf("open :memory:: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	// ---------- A/B 的前置断言（少了它，两组数字可能根本没差异） ----------
	// 两种失败方式都是静默的，所以必须在**开跑之前**断言掉：
	//   ① A 臂索引没建出来（迁移 16 没跑 / 名字改了）→ 两臂其实是同一组实验；
	//   ② B 臂 DROP 没生效（名字写错）→ 同上。
	if !writeAmpIndexExists(b, db) {
		b.Fatalf("迁移 16 的索引 %s 不存在（A 臂没有可量的对象；索引改名了？）", writeAmpIndexName)
	}
	if dropIndex {
		if _, err := db.Exec(`DROP INDEX ` + writeAmpIndexName); err != nil {
			b.Fatalf("DROP INDEX %s: %v", writeAmpIndexName, err)
		}
		if writeAmpIndexExists(b, db) {
			b.Fatalf("DROP INDEX 之后 %s 仍然存在（B 臂其实还带着索引）", writeAmpIndexName)
		}
		b.Logf("臂 B：已 DROP INDEX %s（不带列表索引的写路径）", writeAmpIndexName)
	} else {
		b.Logf("臂 A：保留 INDEX %s（生产形态）", writeAmpIndexName)
	}

	// ---------- 夹具：外键父行（tasks.namespace_id / capability_id 都是外键） ----------
	u := &User{ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "writeamp", CreatedAt: Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		b.Fatalf("register user: %v", err)
	}
	capDef := &CapabilityVersion{
		NamespaceID: ns.ID, Name: "writeamp.probe", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Outputs: []string{"ok"}, CreatedAt: Now(),
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		b.Fatalf("create capability: %v", err)
	}

	// ---------- 被测：每轮 = 1 个事务插 writeAmpRows 行 ----------
	base := Now()
	batch := make([]*Task, writeAmpRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 建行对象不算进计时（见文件头最后一条）。
		b.StopTimer()
		for j := range batch {
			batch[j] = &Task{
				NamespaceID:  ns.ID,
				TraceID:      fmt.Sprintf("tr_wa_%d_%d", i, j),
				CapabilityID: capDef.ID,
				Capability:   capDef.Name,
				Version:      1,
				// 同一事务共用 created_at（真派发是批次级的），不同事务不同。
				CreatedAt: base + int64(i),
			}
		}
		b.StartTimer()

		if err := db.CreateTasks(ctx, batch); err != nil {
			b.Fatalf("create %d tasks in one tx: %v", writeAmpRows, err)
		}
	}
	b.StopTimer()

	// 表行数随 b.N 线性增长（每轮 1000 行）。A/B 两臂用同样的 -benchtime / -count，
	// 增长曲线一致，所以差值仍是同口径的索引代价；把总量打出来便于核对两臂跑得一样多。
	b.Logf("累计插入 %d 行 = %d 个事务 × %d 行/事务", b.N*writeAmpRows, b.N, writeAmpRows)
}

// writeAmpIndexExists 报告 db 的 sqlite_master 里有没有名为 writeAmpIndexName 的索引。
func writeAmpIndexExists(b *testing.B, db *DB) bool {
	b.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, writeAmpIndexName).
		Scan(&n); err != nil {
		b.Fatalf("查索引 %s 是否存在: %v", writeAmpIndexName, err)
	}
	return n > 0
}
