package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 issue 003 的**真实驱动器**测试：验证「已删能力（tombstone）的后台物理回收」
// 在真 sqlite 上真的按判据删/不删，而不是只测「函数被调用过」。
//
// 判据本身（tasks / task_attempts / task_artifact_manifests / deployments /
// playbook_installs 任一引用即不删）由 store 侧的 v4test/reference_* 覆盖；
// 这里验的是驱动器这一层的三件事，因为它们**只能**在这里被观测到：
//   - 多批直到清零、以及单轮预算用尽时剩余留给下一轮（否则一次补扫会攥着写锁不放）；
//   - 「有引用不删」在驱动路径上同样成立（0 不是因为驱动坏了，而是判据挡住了）；
//   - ctx 取消立即停，RunOnce 一行都不删（关停期间不该再开写事务）。

// newQuietCapabilityGC 用一个丢弃日志的 logger 造 GC：这些用例故意制造
// 「已删 N 行」之外还要制造失败轮次，日志不该在测试输出里制造噪声。
func newQuietCapabilityGC(db *store.DB) *capabilityGC {
	return newCapabilityGC(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
}

// openCapabilityGCDB 在给定路径打开库并注册一个用户，返回 (库, 默认命名空间 id)。
//
// 用真用户而不是手工插命名空间：RegisterUser 是「用户 + 默认 ns」的唯一铸造点，
// 手工插行会在 schema 变更时静默漂移。
func openCapabilityGCDB(t *testing.T, path string) (*store.DB, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ns, err := db.RegisterUser(ctx, &store.User{
		ID:          store.NewUserID(),
		Username:    "cap-gc",
		DisplayName: "能力 GC 测试用户",
		Role:        store.RoleAdmin,
		CreatedAt:   store.Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	return db, ns.ID
}

// newCapabilityGCTestDB 造一个临时目录里的真 sqlite + 默认命名空间。
func newCapabilityGCTestDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, nsID := openCapabilityGCDB(t, filepath.Join(t.TempDir(), "cap-gc.db"))
	t.Cleanup(func() { _ = db.Close() })
	return db, nsID
}

// createGCAbility 写一个最小可用能力版本（显式 version=1，正文带可执行的入口）。
func createGCAbility(t *testing.T, db *store.DB, nsID, name string) *store.CapabilityVersion {
	t.Helper()
	ctx := context.Background()
	c := &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability %q: %v", name, err)
	}
	return c
}

// softDeleteGCAbility 用生产路径（DeleteCapability + CAS 令牌）打 tombstone。
func softDeleteGCAbility(t *testing.T, db *store.DB, c *store.CapabilityVersion) {
	t.Helper()
	ctx := context.Background()
	if err := db.DeleteCapability(ctx, c.NamespaceID, c.Name, store.ResourceVersion(c.ID, c.ResourceVersion)); err != nil {
		t.Fatalf("soft delete capability %q: %v", c.Name, err)
	}
	if !capabilityRowExists(t, db, c.ID) {
		t.Fatalf("软删把 %q 整行删掉了（本卡要求的是软删 + 后台回收，不是删除即硬删）", c.Name)
	}
	var deletedAt *int64
	if err := db.QueryRowContext(ctx, `SELECT deleted_at FROM capability_versions WHERE id=?`, c.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Fatalf("%q 的 deleted_at 仍为 NULL：夹具没有造出 tombstone，后面的断言会假绿", c.Name)
	}
}

// capabilityRowExists 报告能力行是否**物理**存在（不看 deleted_at）。
func capabilityRowExists(t *testing.T, db *store.DB, capID string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions WHERE id=?`, capID).Scan(&n); err != nil {
		t.Fatalf("count capability row %s: %v", capID, err)
	}
	return n == 1
}

// TestCapabilityGCRunOnceOnCleanDatabase：一轮空跑必须返回 0 且无错。
//
// 这条钉住的是「进门就 BeginTx」的那个原语不该在没有 tombstone 时被驱动成错误
// （它每轮都会开一次写事务，有错就等于每轮都留一条假告警）。
func TestCapabilityGCRunOnceOnCleanDatabase(t *testing.T) {
	db, nsID := newCapabilityGCTestDB(t)
	// 一条**活**能力：purge 只认 deleted_at IS NOT NULL，活行必须原样留着。
	live := createGCAbility(t, db, nsID, "still-live")
	gc := newQuietCapabilityGC(db)

	n, err := gc.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("RunOnce on clean db = (%d, %v), want (0, nil)", n, err)
	}
	if !capabilityRowExists(t, db, live.ID) {
		t.Fatal("purge 删掉了活能力行（判据必须只认 deleted_at IS NOT NULL）")
	}
}

// TestCapabilityGCPurgesUnreferencedTombstone：无引用的 tombstone 被删，且再跑一轮返回 0。
//
// 「再跑一轮返回 0」是幂等性的观测面：回收不是一次性批量，而是每轮都会重复问
// 「还有没有可回收的」。若实现把「上一轮删过」记成状态，第二轮就会误导运维。
func TestCapabilityGCPurgesUnreferencedTombstone(t *testing.T) {
	db, nsID := newCapabilityGCTestDB(t)
	c := createGCAbility(t, db, nsID, "gc-me")
	softDeleteGCAbility(t, db, c)
	gc := newQuietCapabilityGC(db)

	n, err := gc.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("first RunOnce = (%d, %v), want (1, nil)", n, err)
	}
	if capabilityRowExists(t, db, c.ID) {
		t.Fatal("无引用的 tombstone 没被物理删除")
	}
	if n, err := gc.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("second RunOnce = (%d, %v), want (0, nil)（幂等：清干净后不该再删）", n, err)
	}
}

// TestCapabilityGCKeepsReferencedTombstone：被 deployments 钉住的 tombstone 一个字节都不许动。
//
// 为什么用 deployment 而不是 task 作代表：两者的失效模式不对称——`tasks` 是
// ON DELETE RESTRICT（漏判会硬报错），`deployments` 是 ON DELETE CASCADE（漏判会**静默**
// 删掉部署期望行，节点侧表现为「内容突然不推了」）。静默的那一类才需要驱动层显式钉住。
// 释放引用后再跑一轮必须删掉：证明前面那个 0 来自判据，而不是驱动器根本没跑。
func TestCapabilityGCKeepsReferencedTombstone(t *testing.T) {
	ctx := context.Background()
	db, nsID := newCapabilityGCTestDB(t)
	c := createGCAbility(t, db, nsID, "deployed-once")
	node := &store.Node{Name: "gc-node", DeclaredNamespaces: []string{"*"}}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID}); err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	softDeleteGCAbility(t, db, c)

	gc := newQuietCapabilityGC(db)
	if n, err := gc.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunOnce with a deployment reference = (%d, %v), want (0, nil)", n, err)
	}
	if !capabilityRowExists(t, db, c.ID) {
		t.Fatal("被部署钉住的 tombstone 被删了（deployments 是 CASCADE：漏判会静默少推内容）")
	}

	// 可信控制面故障夹具（与 store/v4test/reference_force_test.go 同口径）：
	// 直接退掉这条引用，用来证明「驱动器本身是通的」，而不是永远返回 0。
	if _, err := db.ExecContext(ctx, `DELETE FROM deployments WHERE capability_id=?`, c.ID); err != nil {
		t.Fatalf("drop deployment reference: %v", err)
	}
	if n, err := gc.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce after releasing the reference = (%d, %v), want (1, nil)", n, err)
	}
	if capabilityRowExists(t, db, c.ID) {
		t.Fatal("引用释放后 tombstone 仍未被回收")
	}
}

// TestCapabilityGCRunOnceHonorsCanceledContext：ctx 已取消时立即返回、一行都不删。
//
// 关停期间再开一个 `BEGIN IMMEDIATE` 写事务，正是 #340 r3 那类收尾竞态的来源
// （db.Close() 与后台写者并发 → `sql: database is closed`）。
func TestCapabilityGCRunOnceHonorsCanceledContext(t *testing.T) {
	db, nsID := newCapabilityGCTestDB(t)
	c := createGCAbility(t, db, nsID, "keep-on-shutdown")
	softDeleteGCAbility(t, db, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gc := newQuietCapabilityGC(db)
	n, err := gc.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce(canceled ctx) err = %v, want context.Canceled", err)
	}
	if n != 0 {
		t.Fatalf("RunOnce(canceled ctx) deleted = %d, want 0", n)
	}
	if !capabilityRowExists(t, db, c.ID) {
		t.Fatal("ctx 取消后仍然删了行（关停窗口里不该有写事务落地）")
	}
}

// TestCapabilityGCBatchesUntilNothingLeft：一批删不完时继续下一批，直到某批返回 0。
//
// 把 batch 调到 1 让「多批」成为可观测量：两条 tombstone 必须在一轮里都清掉。
// 若实现只跑一批，返回 1 且留一条，本用例立刻红。
func TestCapabilityGCBatchesUntilNothingLeft(t *testing.T) {
	db, nsID := newCapabilityGCTestDB(t)
	first := createGCAbility(t, db, nsID, "batch-a")
	second := createGCAbility(t, db, nsID, "batch-b")
	softDeleteGCAbility(t, db, first)
	softDeleteGCAbility(t, db, second)

	gc := newQuietCapabilityGC(db)
	gc.batch = 1
	n, err := gc.RunOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("RunOnce(batch=1, 2 tombstones) = (%d, %v), want (2, nil)", n, err)
	}
	if capabilityRowExists(t, db, first.ID) || capabilityRowExists(t, db, second.ID) {
		t.Fatal("一轮里没有把所有可回收的 tombstone 清掉（多批循环没生效）")
	}
}

// TestCapabilityGCStopsAtBatchBudget：单轮预算用尽就收手，剩下的留给下一轮。
//
// 这条钉住的是**写锁上限**：purge 是全表扫 + 写锁，「宁可少删」是刻意的设计取舍。
// 同时它顺带钉住「已删数量按批累加」的记账：预算用尽（非失败）的轮次也要把
// 已删数如实返回，否则运维看到 0 会以为没删。
func TestCapabilityGCStopsAtBatchBudget(t *testing.T) {
	db, nsID := newCapabilityGCTestDB(t)
	first := createGCAbility(t, db, nsID, "budget-a")
	second := createGCAbility(t, db, nsID, "budget-b")
	softDeleteGCAbility(t, db, first)
	softDeleteGCAbility(t, db, second)

	gc := newQuietCapabilityGC(db)
	gc.batch = 1
	gc.maxBatches = 1
	n, err := gc.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RunOnce(budget=1 batch) = (%d, %v), want (1, nil)（宁可少删，不要长写锁）", n, err)
	}
	if n, err := gc.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("second RunOnce = (%d, %v), want (1, nil)（剩余积压由下一轮收）", n, err)
	}
	if n, err := gc.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("third RunOnce = (%d, %v), want (0, nil)", n, err)
	}
}

// TestCapabilityGCRunOnceWrapsStoreError：库不可用时错误必须被包装并向上抛。
//
// 单批失败**不能吞**：吞掉错误会让后台循环看起来一直正常，而 tombstone 其实一行没删。
func TestCapabilityGCRunOnceWrapsStoreError(t *testing.T) {
	db, _ := newCapabilityGCTestDB(t)
	gc := newQuietCapabilityGC(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	n, err := gc.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce on a closed db returned nil error（失败被吞掉了）")
	}
	if n != 0 {
		t.Fatalf("RunOnce on a closed db deleted = %d, want 0", n)
	}
	if !strings.Contains(err.Error(), "capability gc") {
		t.Fatalf("error %q 没有带上驱动层的前缀，运维无法定位是谁在回收", err)
	}
}

// TestCapabilityGCWiredByNewApp 是**真实装配**的接线证明 + 关停屏障：
//
//  1. 先在库文件里落一条无引用 tombstone；
//  2. NewApp 把周期设成 1s（默认 3600s，测试里不能等）；
//  3. 轮询到那行**真的消失** —— 这证明 mux 里的 `if cfg.CapabilityGCInterval() > 0`
//     分支不是死代码（没接线的话这里会等到超时）；
//  4. 再按硬要求的关停顺序 cancel → WaitBackground → closeDBForTempDir：
//     若循环没走 runBackgroundLoop，WaitBackground 会提前放行，关库就会与一轮回收
//     并发（postclose 护栏会把 `sql: database is closed` 变成确定性红）。
func TestCapabilityGCWiredByNewApp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")

	seedDB, nsID := openCapabilityGCDB(t, dbPath)
	c := createGCAbility(t, seedDB, nsID, "wired-gc")
	softDeleteGCAbility(t, seedDB, c)
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	cfg := FromEnv()
	cfg.DBPath = dbPath
	cfg.AgentBin = filepath.Join(dir, "no-agent-binary")
	cfg.RPDisplay = "Anotify Capability GC Test"
	cfg.RPID = "localhost"
	cfg.RPOrigin = "http://localhost"
	cfg.Pprof = false
	cfg.CapabilityGCIntervalSec = 1

	ctx, cancel := context.WithCancel(context.Background())
	app := NewApp(ctx, cfg)
	defer func() {
		cancel()
		app.WaitBackground()
		// 回收事务提交后，连接归还到池可能比 `WaitBackground` 晚一拍（曾以 1/5 概率
		// 撞到 closeDBForTempDir 的「关库前无在用连接」断言）。这里用**条件等待**而不是
		// 固定 sleep：真泄漏时 5s 后仍然失败，断言强度不变；只是不再赌时序。
		waitUntil(t, "后台回收归还 DB 连接", func() bool { return app.DB.Stats().InUse == 0 }, 5*time.Second)
		closeDBForTempDir(t, app.DB, dir)
	}()

	// 等后台 GC 真的把 tombstone 回收掉：用同包的 `waitUntil` 轮询（条件等待 +
	// 超时上限），不要再写裸 `time.Sleep(50ms)` 的固定间隔循环 —— 那是
	// scripts/check-test-discipline.mjs 的计数口径要清零的历史形态：固定间隔既拖慢一次通过
	// 的用例，又会在慢机上把「还没回收」当成「没装配」。
	waitUntil(t, "tombstone 被后台回收（能力 GC 循环被 NewApp 装配）", func() bool {
		var n int
		if err := app.DB.QueryRow(`SELECT count(*) FROM capability_versions WHERE id=?`, c.ID).Scan(&n); err != nil {
			t.Fatalf("poll capability row: %v", err)
		}
		return n == 0
	}, 20*time.Second)
}

// TestCapabilityGCPurgeFlipsNamespaceDeleteFromBlockedToAllowed 钉住本卡唯一一个
// **对外可见的语义变化**：tombstone 现在会计入 `DELETE /v1/namespaces/{ns}` 的阻塞计数
// （store/namespace.go 的 `capabilities` 探针不过滤 `deleted_at`），回收之后同一条请求
// 就不再被挡住（409 → 200）。
//
// 为什么必须显式覆盖而不是当附带效果：这条变化只在「回收恰好发生在两次请求之间」时可见，
// 谁也不会在手工验收里撞上；而它一旦变成「已删能力永远挡住命名空间删除」，运维就只能
// 手工改库。`capability_gc.go` 已写明它**不是**契约保证。
//
// 断言分两段（先红后绿），否则「删掉能力之后能删命名空间」会因为根本没有 tombstone 而假绿。
func TestCapabilityGCPurgeFlipsNamespaceDeleteFromBlockedToAllowed(t *testing.T) {
	ctx := context.Background()
	// 默认命名空间留着：store 拒绝删「最后一个命名空间」（ErrLastNamespace），
	// 那是另一条不变量，不该混进本用例。
	db, _ := newCapabilityGCTestDB(t)
	ns, err := db.CreateNamespace(ctx, "gc-ns", "回收测试命名空间")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	c := createGCAbility(t, db, ns.ID, "blocking-tombstone")
	softDeleteGCAbility(t, db, c)

	// ① 回收之前：tombstone 计入阻塞计数 → 非 force 删除被拒（HTTP 409 的来源）。
	var blocked *store.NamespaceBlockedError
	err = db.DeleteNamespace(ctx, ns.ID, false, ns.ResourceVersion)
	if !errors.As(err, &blocked) {
		t.Fatalf("tombstone 存在时删命名空间 err = %v, want NamespaceBlockedError（现状必须仍然阻塞）", err)
	}
	hasCapabilities := false
	for _, reason := range blocked.Blocking {
		if strings.HasPrefix(reason, "capabilities=") {
			hasCapabilities = true
		}
	}
	if !hasCapabilities {
		t.Fatalf("阻塞清单里没有 capabilities 计数：%v（说明夹具没造出「tombstone 挡住命名空间」的现状）", blocked.Blocking)
	}

	// ② 回收之后：同一请求不再被 tombstone 挡住。
	gc := newQuietCapabilityGC(db)
	if n, err := gc.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = (%d, %v), want (1, nil)", n, err)
	}
	if err := db.DeleteNamespace(ctx, ns.ID, false, ns.ResourceVersion); err != nil {
		t.Fatalf("tombstone 回收后命名空间仍被挡住: %v（本卡声明的语义变化没有发生）", err)
	}
}
