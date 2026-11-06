package store

// #264 图版本落版：**图没变时不许写**。
//
// 病灶（issue #264 的一半）：`EnsureGraphVersion` 原来是无条件
// `INSERT ... ON CONFLICT DO NOTHING` + 回查，于是「图根本没变」这个稳态
// （每一次派发都是它）也要拿一次写锁、走一次写事务。池 = 8、DSN `_txlock=immediate`，
// 第二个写者排 busy handler(5s) —— 这就是 issue 里说的那部分串行化。
//
// 验收口径不是「行数没变」（ON CONFLICT DO NOTHING 本来就不加行，改不改都读得出 1 行），
// 而是**有没有碰写锁**。所以这里用一条外部连接持写锁（BEGIN IMMEDIATE）当探针：
//   - 图没变 → 调用必须直接成功（它只需要读）；
//   - 图变了 → 同一把锁下调用必须失败（它必须写）—— 这是防止上一条变成"空断言"的
//     负样本：证明那把锁真的挡得住这个函数的写路径。
//
// 负样本要等 busy_timeout(5000) 走完（约 4s），`-short` 下跳过。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
)

// holdWriteLock 用一条**独立连接**拿住整库的写锁，返回释放函数。
//
// 为什么不用被测的 *DB：那是一个连接池，`Exec("BEGIN IMMEDIATE")` 会落在池里某条
// 连接上、之后的语句却可能走另一条，锁就"跑"了。独立连接 = 锁的作用域确定。
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	ctx := context.Background()
	// busy_timeout(0)：探针自己抢锁时不要等——抢不到就是测试自己写错了，立刻红。
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open observer connection: %v", err)
	}
	c, err := conn.Conn(ctx)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("pin observer connection: %v", err)
	}
	if _, err := c.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = c.Close()
		_ = conn.Close()
		t.Fatalf("take write lock: %v", err)
	}
	return func() {
		// 回滚而不是提交：探针不该往库里留任何字节。
		_, _ = c.ExecContext(ctx, `ROLLBACK`)
		_ = c.Close()
		_ = conn.Close()
	}
}

// TestEnsureGraphVersionUnchangedTakesNoWriteLock 图没变 → 一次写锁都不碰。
func TestEnsureGraphVersionUnchangedTakesNoWriteLock(t *testing.T) {
	db, path := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	// fixer 声明 done 出口：后面那条「图变了」的边要用它（CreateEdge 会校验
	// from_port 必须在能力声明的 outputs 里，否则边直接被拒）。
	fixer := mkCap(nsID, "fixer", 1, []string{"done"})
	mustCreateCap(t, db, review, fixer)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	// 先把这一版图落下去（这一笔当然要写）。
	first, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph version: %v", err)
	}

	// —— ① 图没变：写锁被别人占着也必须成功 ——
	//
	// 这里断言的是**错误**而不是耗时：拿不到写锁的 INSERT 会等到 busy_timeout(5000)
	// 之后返回 "database is locked"，所以「返回 nil」直接等价于「没有尝试写」。
	// 用时序（例如"必须快于 100ms"）在负载高的机器上会假红。
	release := holdWriteLock(t, path)
	got, err := db.EnsureGraphVersion(ctx, nsID)
	release()
	if err != nil {
		t.Fatalf("图没变时 EnsureGraphVersion 仍去抢写锁了（写锁被占时报错）：%v", err)
	}
	if got.ID != first.ID || got.ContentHash != first.ContentHash {
		t.Fatalf("返回的图版本变了：%s(%s) → %s(%s)，want 同一行",
			first.ID, first.ContentHash, got.ID, got.ContentHash)
	}

	// —— ② 负样本：图变了就必须写，而写锁仍被占着 → 必须失败 ——
	//
	// 少了这一条，① 可能是「锁根本没用」造成的假绿（比如 BEGIN IMMEDIATE 在 WAL 下
	// 并不挡写者），那 ① 就什么都没证明。
	if testing.Short() {
		t.Log("跳过负样本（-short：要等 busy_timeout 走完约 4s）")
		return
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "fixer", FromPort: "done", ToRef: "review",
	}); err != nil {
		t.Fatalf("create second edge: %v", err)
	}
	release = holdWriteLock(t, path)
	_, err = db.EnsureGraphVersion(ctx, nsID)
	release()
	if err == nil {
		t.Fatal("图变了却还是没写（写锁被占着也成功了）——①的断言是空断言")
	}

	// —— ③ 锁放开后：新图正常落版，同一份图仍然只有一行 ——
	second, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph version after unlock: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("加了边却算回同一版图（id=%s）", second.ID)
	}
	again, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph version (repeat): %v", err)
	}
	if again.ID != second.ID {
		t.Fatalf("同一份图返回了两个 id：%s vs %s（按需落版会堆垃圾）", again.ID, second.ID)
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM graph_versions WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count graph versions: %v", err)
	}
	if n != 2 {
		t.Fatalf("graph_versions = %d 行, want 2（两版图，各一行）", n)
	}
}

// TestEnsureGraphVersionSnapshotRoundTrip 存的快照 = 读回来的快照（先查后写不能读错行）。
//
// 「先读后写」的读走的是 (namespace_id, content_hash)；这条用例钉住它读到的确实是
// **这一版**图的快照，而不是"某一版"（多版共存时才看得出串行）。
func TestEnsureGraphVersionSnapshotRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := mkCap(nsID, "review", 1, []string{"fix"})
	b := mkCap(nsID, "fixer", 1, []string{"done"})
	mustCreateCap(t, db, a, b)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	v1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph v1: %v", err)
	}

	// 加一条**只有新版本才有**的边。
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "fixer", FromPort: "done", ToRef: "review",
	}); err != nil {
		t.Fatalf("create edge 2: %v", err)
	}
	v2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph v2: %v", err)
	}

	// 逐版按 id 读回：内容必须与 hash 对得上，且两版内容不同。
	back1, err := db.GetGraphVersion(ctx, nsID, v1.ID)
	if err != nil {
		t.Fatalf("get graph v1: %v", err)
	}
	back2, err := db.GetGraphVersion(ctx, nsID, v2.ID)
	if err != nil {
		t.Fatalf("get graph v2: %v", err)
	}
	if back1.ContentHash != v1.ContentHash || string(back1.Snapshot) != string(v1.Snapshot) {
		t.Fatalf("v1 读回来变了：hash %s→%s", v1.ContentHash, back1.ContentHash)
	}
	if string(back1.Snapshot) == string(back2.Snapshot) {
		t.Fatal("两版图的快照一样（先查后写读到了错的行）")
	}
	if !back1.GraphHasEdge("review", "fix", "fixer") || back1.GraphHasEdge("fixer", "done", "review") {
		t.Fatalf("v1 快照的内容不是「只有第一条边」：%s", back1.Snapshot)
	}
	if !back2.GraphHasEdge("fixer", "done", "review") {
		t.Fatalf("v2 快照里没有第二条边：%s", back2.Snapshot)
	}
	// 快照的字节 = hash 的输入口径：content_hash 必须能从快照复算出来。
	if h, err := hashOfSnapshot(back2.Snapshot); err != nil || h != v2.ContentHash {
		t.Fatalf("content_hash 与快照对不上：snapshot→%s, 行里 %s (err=%v)", h, v2.ContentHash, err)
	}
}

// hashOfSnapshot 复算快照列的 sha256。
//
// 口径就是内容定版的定义本身：`content_hash = sha256(snapshot 列的字节)`。
// 所以直接哈希落库的那份字节，不用把快照解析回 Edge 再归一（那条路会漏掉
// literals/mapping，等于换了个口径去"验证"）。
func hashOfSnapshot(snapshot []byte) (string, error) {
	sum := sha256.Sum256(snapshot)
	return hex.EncodeToString(sum[:]), nil
}

// TestGraphSnapshotPayloadMatchesContentHash graphSnapshotPayload 的口径与 graphContentHash 一致。
//
// 这是 #264 那次重构（hash 与 snapshot 合成一次算）的等价性守卫：口径一变，
// 全仓库已经落过的 graph_versions 都会变成「算不回自己 hash」的老数据。
func TestGraphSnapshotPayloadMatchesContentHash(t *testing.T) {
	edges := []*Edge{
		{FromRef: "review", FromPort: "fix", ToRef: "fixer", Priority: 10},
		{FromIsSource: true, FromRef: "github", FromPort: "pull_request.opened", ToRef: "review"},
	}
	hash, payload, err := graphSnapshotPayload(edges)
	if err != nil {
		t.Fatalf("graph snapshot payload: %v", err)
	}
	want, err := graphContentHash(edges)
	if err != nil {
		t.Fatalf("graph content hash: %v", err)
	}
	if hash != want {
		t.Fatalf("graphSnapshotPayload 的 hash = %s, graphContentHash = %s", hash, want)
	}
	if got, err := hashOfSnapshot([]byte(payload)); err != nil || got != hash {
		t.Fatalf("快照复算 hash = %s (err=%v), want %s", got, err, hash)
	}
	// 空图也要有稳定的 hash（不能因为 nil/空切片算出两种）。
	empty, _, err := graphSnapshotPayload(nil)
	if err != nil {
		t.Fatalf("graph snapshot payload (nil): %v", err)
	}
	if empty == "" {
		t.Fatal("空图的 content_hash 是空串")
	}
}

// TestGraphVersionPathsUseSameNamespace graph_versions 按命名空间隔离（先查后写不能跨空间命中）。
func TestGraphVersionPathsUseSameNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns1, _ := newTestNS(t, db)
	ns2 := newTestExtraNS(t, db, "other")

	// 两个命名空间各一条**内容相同**的边（引用相同）→ 两份图版本，两个 id。
	for _, ns := range []string{ns1, ns2} {
		a := mkCap(ns, "review", 1, []string{"fix"})
		b := mkCap(ns, "fixer", 1, nil)
		mustCreateCap(t, db, a, b)
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID: ns, FromRef: "review", FromPort: "fix", ToRef: "fixer",
		}); err != nil {
			t.Fatalf("create edge in %s: %v", ns, err)
		}
	}
	g1, err := db.EnsureGraphVersion(ctx, ns1)
	if err != nil {
		t.Fatalf("ensure graph ns1: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, ns2)
	if err != nil {
		t.Fatalf("ensure graph ns2: %v", err)
	}
	if g1.ID == g2.ID {
		t.Fatalf("两个命名空间共用了一个图版本 id：%s（先查后写跨了命名空间）", g1.ID)
	}
	if g1.ContentHash != g2.ContentHash {
		t.Fatalf("同样的边在不同命名空间算出不同 hash：%s vs %s", g1.ContentHash, g2.ContentHash)
	}
	if g2.NamespaceID != ns2 {
		t.Fatalf("g2.NamespaceID = %s, want %s", g2.NamespaceID, ns2)
	}
}
