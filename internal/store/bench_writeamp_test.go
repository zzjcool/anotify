package store

// #237 写放大的**可复现度量**（契约 §3 修正②）——留在仓库里，不用每次重造探针。
//
// baseline 的 tasks 上有 (namespace_id, created_at, id) 索引：读路径因此能走覆盖索引
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
// ── 两个口径，别混着用（#264 验收第 1 条）──────────────────────────────────────
//
// 同一条索引在两个形状下的代价完全不同，必须分别量、分别报：
//
//	A. 批量口径 BenchmarkCreateTasksWriteAmplification：1 个事务插 1000 行。
//	   提交开销被 1000 行摊薄，量到的是**行成本**（b-tree 插入/页分裂的 CPU）。
//	B. 单行口径 BenchmarkCreateTasksSingleRowTx：1 个事务插 **1** 行，b.N 轮。
//	   「提交次数 : 行数」= 1:1，量到的是**提交成本 + 1 行的行成本**——这正是
//	   webhook（`POST /v1/hooks/{token}` → 建 1 个任务）的真实形状（#264 验收
//	   第 1 条要的就是它；issue 正文里那个 −45.6% 也是这个口径）。
//
// 单行口径怎么跑（A/B 交错至少 3 组、每组 count=3，取中位并看极差）：
//
//	taskset -c 0-3 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksSingleRowTx$' -benchtime 2000x -count=3      # A：有索引
//	WRITEAMP_DROP_INDEX=1 taskset -c 0-3 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksSingleRowTx$' -benchtime 2000x -count=3      # B：无索引
//
// 内存库 vs 文件库：内存库量的是「索引维护 + 事务提交的 CPU」，**没有任何 fsync**
// （而且本仓库 DSN 是 WAL + `synchronous(NORMAL)`，文件库上单语句 COMMIT 同样不 fsync，
// 见 store.go 的 openWithOptions）。要连「WAL 帧追加」也量进去，就加 WRITEAMP_FILE_DB=1：
//
//	WRITEAMP_FILE_DB=1 taskset -c 0-3 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksSingleRowTx$' -benchtime 2000x -count=3      # 文件库 + WAL
//
// 注意文件库那组会撞上 SQLite 的 auto-checkpoint（WAL 过 1000 页自动回写），
// 极差比内存库大是正常的——那本来就是真部署里会发生的事，别当噪声剔掉。
//
// **单行口径的噪声带很宽，读法有讲究**（实测，见 #264 报告）：一屏 `-count=3` 内部
// 的极差就能到中位的 ±30%，而 tasks 上有 **5 条索引**（schema.sql: idx_tasks_trace /
// _parent / _lease / _node / _ns_created），删掉其中 1 条在**单行事务**里只可能改变
// 「1/6 的索引插入工作」，即个位数百分比——**落在噪声里**。所以：
//   - A/B 必须**交错**跑（同一时段各跑一组，抵消机器漂移），不要先跑完 A 再跑 B；
//   - 只看中位 + 极差，单组样本不足以下结论；
//   - 想收紧样本就抬 `-benchtime`（20000x 能把样本内波动压到 ±10% 量级）。
// 另外：`:memory:` 与文件库在本仓库都是「不 fsync」（DSN 是 WAL + synchronous(NORMAL)），
// 两者之差只是 WAL 帧追加，实测中位差约 15–30%，不是磁盘等待的数量级。
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
	"path/filepath"
	"testing"
)

const (
	// writeAmpRows 是一次事务插入的行数（契约要求 ≥1000）。
	writeAmpRows = 1000

	// writeAmpDropIndexEnv 是 A/B 开关：=1 时先 DROP 掉列表索引，量「没有这条索引」的写成本。
	writeAmpDropIndexEnv = "WRITEAMP_DROP_INDEX"

	// writeAmpIndexName 必须与 schema.sql 里的索引名一致（见 schema_baseline_test.go 的 baselineIndexes）。
	// 这里单独写一份常量是为了让本文件自解释；两侧是否漂了由下面 DROP 之后的存在性断言兜住。
	writeAmpIndexName = "idx_tasks_ns_created"

	// writeAmpFileDBEnv 是库类型开关：=1 时用**文件库**（store.Open 的 DSN 自带 WAL +
	// synchronous(NORMAL)）而不是 ":memory:"。给单行口径用：内存库上根本没有 WAL 帧
	// 追加这回事，量不到「提交到磁盘」的那一层。
	writeAmpFileDBEnv = "WRITEAMP_FILE_DB"
)

// openWriteAmpB 起一套两个口径**完全共用**的被测环境：库（含 A/B 臂）+ 外键父行。
//
// 两个基准共用它的理由与 A/B 判据同源：**两臂之间、两个口径之间**唯一的差异都只该是
// 被声明的那个变量（索引 / 事务形状）。夹具（用户+命名空间+能力，都是外键父行）
// 一旦各写一份，迟早会漂成两套行数/两套 schema 参数，ns/op 就不可比了。
//
// 返回 (库, 命名空间 id, 能力定义)。
func openWriteAmpB(b *testing.B) (*DB, string, *CapabilityVersion) {
	b.Helper()
	ctx := context.Background()
	dropIndex := os.Getenv(writeAmpDropIndexEnv) == "1"

	// 库类型：默认 ":memory:"（只量 CPU），WRITEAMP_FILE_DB=1 用文件库（带 WAL 帧追加）。
	db := openWriteAmpStore(b)

	// ---------- A/B 的前置断言（少了它，两组数字可能根本没差异） ----------
	// 两种失败方式都是静默的，所以必须在**开跑之前**断言掉：
	//   ① A 臂索引没建出来（索引没建 / 名字改了）→ 两臂其实是同一组实验；
	//   ② B 臂 DROP 没生效（名字写错）→ 同上。
	if !writeAmpIndexExists(b, db) {
		b.Fatalf("baseline 的索引 %s 不存在（A 臂没有可量的对象；索引改名了？）", writeAmpIndexName)
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
	return db, ns.ID, capDef
}

// openWriteAmpStore 按 WRITEAMP_FILE_DB 打开被测库。
//
// ":memory:" 与文件库的差别不是"快慢"，是**量到的东西不同**：
//   - 内存库：只有索引维护 + 事务提交的 CPU，没有任何 I/O；
//   - 文件库：多了 WAL 帧追加（本仓库 DSN 是 WAL + synchronous(NORMAL)，单语句 COMMIT
//     **不 fsync**，所以这里量到的仍然不是「等磁盘」，而是「写 WAL 的 CPU + 页拷贝」）。
//
// 两个都要跑、都要报：只给内存库就宣称"webhook 口径"是不诚实的。
func openWriteAmpStore(b *testing.B) *DB {
	b.Helper()
	path := ":memory:"
	if os.Getenv(writeAmpFileDBEnv) == "1" {
		path = filepath.Join(b.TempDir(), "writeamp.db")
	}
	db, err := Open(path)
	if err != nil {
		b.Fatalf("open %s: %v", path, err)
	}
	b.Cleanup(func() { _ = db.Close() })
	b.Logf("库类型：%s", writeAmpDBKind())
	return db
}

// writeAmpDBKind 是日志里的库类型标签（两臂/两个口径都打，便于对照报告）。
func writeAmpDBKind() string {
	if os.Getenv(writeAmpFileDBEnv) == "1" {
		return "文件 + WAL（synchronous=NORMAL，单语句 COMMIT 不 fsync）"
	}
	return ":memory:（无任何 I/O）"
}

// BenchmarkCreateTasksWriteAmplification 量「一条索引在写路径上的纯代价」。
//
// 两条臂的行为差异**只有** DROP INDEX 这一步，其余（同一份代码、同一个夹具、同样的事务
// 形状、同样的行数）逐字相同，所以 ns/op 的差值就是这条索引的维护成本。
func BenchmarkCreateTasksWriteAmplification(b *testing.B) {
	ctx := context.Background()
	db, nsID, capDef := openWriteAmpB(b)

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
				NamespaceID:  nsID,
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

// BenchmarkCreateTasksSingleRowTx 量 **webhook 形状**的写代价：每轮 **1 个事务插 1 行**。
//
// #264 验收第 1 条要的是这个口径：webhook（`POST /v1/hooks/{token}`）一次只建 1 个任务，
// 「提交次数 : 行数」= 1:1。批量口径（1 tx × 1000 行）把提交开销摊成 1/1000，
// 量到的只是行成本；两个数字不能互相替代，也不能拿批量口径去回答 webhook 的账。
//
// 两臂只差 DROP INDEX 一步（与批量口径同一套夹具、同一个开关），所以 ns/op 的差值
// 就是「这条索引在**单行事务**里的代价」= 行成本 + 这次提交多写的索引页。
//
// 运行命令（交错跑，见文件头「两个口径」段）：
//
//	taskset -c 0-3 go test ./internal/store/ -run '^$' \
//	  -bench 'CreateTasksSingleRowTx$' -benchtime 2000x -count=3
//	WRITEAMP_DROP_INDEX=1 taskset ...（同上）
//	WRITEAMP_FILE_DB=1 taskset ...（文件库 + WAL，A/B 同上）
func BenchmarkCreateTasksSingleRowTx(b *testing.B) {
	ctx := context.Background()
	db, nsID, capDef := openWriteAmpB(b)

	// 行对象在计时区**外**一次建好（与批量口径同一条纪律：计时区内只有 CreateTasks）。
	// 每轮必须是**不同的**行：CreateTasks 会回填 ID/Status，复用同一个对象第二轮就撞主键。
	// 所以这里按 b.N 预建（本基准按固定轮数用 -benchtime Nx 跑，不做时间制）。
	base := Now()
	one := make([][]*Task, b.N)
	for i := range one {
		one[i] = []*Task{{
			NamespaceID:  nsID,
			TraceID:      fmt.Sprintf("tr_sr_%d", i),
			CapabilityID: capDef.ID,
			Capability:   capDef.Name,
			Version:      1,
			// 每轮一个事件一个 created_at（webhook 的真实形状：一行一个事务）。
			CreatedAt: base + int64(i),
		}}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 计时区内**恰好一次**：1 个事务插 1 行。
		if err := db.CreateTasks(ctx, one[i]); err != nil {
			b.Fatalf("create 1 task in one tx: %v", err)
		}
	}
	b.StopTimer()
	b.Logf("累计 %d 个事务 × 1 行（库=%s）", b.N, writeAmpDBKind())
}
