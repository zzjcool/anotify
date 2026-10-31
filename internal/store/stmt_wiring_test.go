package store

// #242 · 生产调用点的**接线门禁**（独立审查 MAJOR-2 的返工）。
//
// 为什么需要这条测试（这是审查实证过的真实缺口，不是想象中的风险）：
//
//	stmt_test.go 钉住的是 cachedQueryContext / cachedQueryRowContext 这**两个函数自己**
//	的行为（计数驱动：没有 per-request 的 prepare）。但它对「12 个生产调用点有没有
//	真的用上缓存」一无所知——把任意一处（甚至全部 12 处）`d.cachedQuery*` 改回
//	`d.QueryContext` / `d.QueryRowContext`，stmt_test.go 全绿，
//	`go test ./internal/store/ ./internal/api/ ./internal/server/` 三包也全绿：
//	缓存被静默旁路、#242 的收益归零，而 CI 毫无反应。
//
// 本文件补的就是那一环：**逐调用点**跑真实读路径，要求每一处都让 `db.stmts.len()`
// 恰好 +1（首次）/ +0（第二次，命中）。于是：
//
//	任意一处回退成直查      → 该步增量变 0                              → 红
//	任何地方冒出意外缓存条目 → 增量 > 1                                  → 红
//	同一入口第二次仍然 +1    → 缓存键不稳定（或缓存被整表换掉）          → 红
//
// （「值被拼进 SQL 文本」这种**文本无界膨胀**不靠本文件抓：它由 stmt.go 的
//  maxCachedStmts 上限 + stmt_test.go 的 TestStmtCacheBoundedByMaxCachedStmts 守住。）
//
// ── 断言形状的选择（为什么逐入口增量 + 「总量 ≥ 下限」）─────────────────────
//
// 可选过滤会让同一个函数产生多条 SQL 文本（ListTasks 2^3、ListEvents 2^2），
// 所以既不能简单地「每个函数恰好 +1」，也不该偷懒到「每个函数至少 +1」
// （后者漏得掉「某个过滤分支绕过了缓存」）。
// 这里选的是**逐入口记录增量**（更精确）：每个入口断言自己的 delta == 1，
// 最后再断言总量 **>=`wiringMinDistinctSQL`**（去重后的常量 SQL 条数，当下限用）。
//
// ⚠️ 总量那条**故意是 `>=`，不要改回 `==`**：
//   - 将来新增一个读函数/一条新 SQL 文本是**常态**，`==` 会把它变成红灯；
//     那是噪音，不是回归，而且会逼着后来人「改数字让它变绿」——守卫就废了。
//   - 真正要抓的是「**已有**调用点被绕过」，那一面完全由逐入口的 delta == 1 断言守住
//     （它跟总量无关，新增读函数也不会影响它）。
//   - 代价：`>=` 不再能发现「某天多出一条不认识的缓存文本」。这是刻意的取舍——
//     总量只做**地板**（「至少覆盖到这么多」，防「整个缓存根本没被用上」这种整体退化），
//     精度交给逐入口断言。
//
// ── 「这条守卫真的会红吗」（不会失败的守卫等于没有守卫）─────────────────────
//
// 一个只能变绿的断言等于没有断言：所以本门禁的**反向证据**是必须验的，而且已验过两轮
// （副本里做，工作区不改）：
//
//	① 只把 ListTasks 改回 `d.QueryContext`   → 第 16 步 delta = 0 → 红
//	② 12 处全部改回直查（改造前的形状）       → 第 1 步就 delta = 0 → 红
//	   （同一轮里旧门禁 stmt_test.go 的 TestStmtCache* **仍然全绿**——这就是 MAJOR-2 的原话）
//
// 改过本文件（尤其是改那条总量断言、或改 wiringSteps 的调用形状）之后，**重做一次 ①**：
// 把任意一处 `d.cachedQuery*` 改回直查，确认这里变红。不会红的守卫不要留在仓储里。
//
// 关于稳定性（不依赖 map 迭代顺序、不依赖具体 SQL 文本）：
//   - 断言只用 stmtCache.len()（一个整数），不读 map 的键、不比对 SQL 文本；
//   - 每个入口跑两遍：第一遍 delta == 1、第二遍 delta == 0（命中），
//     两遍都断言，所以「第一次就命中」或「每次都重新 prepare」都会红。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// wiringMinDistinctSQL 是「12 个调用点 × 无过滤参数」下**去重后**的 SQL 文本条数。
//
// 名字里的 `Min` 说的是**断言口径**（`len() >= 它`，当下限用，见文件头「不要改回 ==」）；
// 数值本身是**本轮实测**的去重条数。
//
// 逐项（与下面 wiringSteps 一一对应；交付报告里的「12 个函数 → SQL 条数」映射表
// 就是照抄这里，两边不一致说明有一边漂了）：
//
//	auth.go:263        GetSession            1  （sessions WHERE id）
//	auth.go:295        ListSessionsByUser    1  （sessions WHERE user_id）
//	authz.go:38        AuthorizeNamespace    1  （EXISTS namespaces + EXISTS users，一次往返）
//	authz.go:87        namespaceExists       1  （EXISTS namespaces）
//	capability.go:309  scanCapability        3  （GetCapability / GetCapabilityByID / getCapabilityByHash 各一种文本）
//	capability.go:386  ListCapabilities      1
//	event.go:192       scanEvent             3  （GetEvent / GetEventByTrace / getEventByIdemKey 各一种文本）
//	event.go:252       ListEvents            1  ← 无过滤时的那一种（其余组合见第二条用例）
//	namespace.go:129   GetNamespace          1
//	namespace.go:144   ListNamespaces        1
//	task.go:377        GetTask               1
//	task.go:420        ListTasks             1  ← 同上
//
//	1+1+1+1+3+1+3+1+1+1+1+1 = 16
//
// 也就是说：**这 12 个只读函数在这条用例的参数下共用 16 条不同的 SQL 文本**
// （12 个调用点，但 scanCapability / scanEvent 是「一个调用点、三种文本」的共用实现）。
// 新增读函数后这个数字**不需要**跟着改（断言是 `>=`）；只有「已有调用点的文本集合变了」
// 才需要同步本表（那时逐入口断言会先告诉你哪一处变了）。
const wiringMinDistinctSQL = 16

// wiringEnv 是把 12 条读路径都喂饱的最小夹具：内存库 + 一个用户（含默认命名空间）
// + 一个会话 + 一个自定义能力 + 一条事件 + 一条任务。
//
// 为什么不用现成的 bench_test.go:newBenchEnv：那个在 `package store_test` 里
// （它要 import internal/route），本文件的断言要读 db.stmts.len()（包内字段），
// 只能在 `package store`。而 stmt_test.go 的 newStmtBenchEnv 会灌 2000 行任务，
// 对「接线」这件事是纯浪费。
type wiringEnv struct {
	db     *DB
	userID string
	nsID   string
	sessID string

	capID   string
	capName string
	capHash string

	eventID string
	traceID string
	idemKey string

	taskID string
}

// newWiringEnv 铺好父行，然后把语句缓存**清空**再交给调用方。
//
// 为什么必须清空：铺库路径**可能**自己走缓存版读路径（CreateCapability 在唯一键冲突时
// 会回查 getCapabilityByHash → scanCapability）。本夹具的铺库不冲突，所以实测清 0 条；
// 这一步是为了让「首次调用 delta == 1」在任何夹具改动（比如给能力造一次冲突）下依然成立，
// 而不是靠「刚好没命中」碰运气。顺带证明一件事：缓存只有 store() 一个写入点。
func newWiringEnv(t *testing.T) *wiringEnv {
	t.Helper()
	ctx := context.Background()

	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open wiring db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	u := &User{ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "wiring", CreatedAt: Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}

	sess := &Session{
		ID: NewSessionID(), UserID: u.ID, DeviceName: "wiring",
		CreatedAt: Now(), ExpiresAt: Now() + 3600, LastSeen: Now(),
	}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 自定义能力（内置名是保留的，必须换一个；内容由 CreateCapability 算 hash 并回填）。
	capDef := &CapabilityVersion{
		NamespaceID: ns.ID, Name: "wiring.probe", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Outputs: []string{"ok"}, CreatedAt: Now(),
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if capDef.ContentHash == "" {
		t.Fatalf("CreateCapability 没回填 ContentHash（getCapabilityByHash 的入参就没了）")
	}

	idemKey := "idem-" + NewUserID()
	ev := &Event{
		NamespaceID: ns.ID, TraceID: "tr_" + NewUserID(), Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
		IdemKey: &idemKey, CreatedAt: Now(),
	}
	inserted, err := db.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if !inserted {
		t.Fatalf("事件没有真的插进去（幂等键撞了？）")
	}

	task := &Task{
		NamespaceID: ns.ID, TraceID: ev.TraceID,
		CapabilityID: capDef.ID, Capability: capDef.Name, Version: 1, CreatedAt: Now(),
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 清空夹具自己塞进去的条目（见函数注释）。
	polluted := db.stmts.len()
	drainStmtCache(db)
	if got := db.stmts.len(); got != 0 {
		t.Fatalf("清空后缓存条数 = %d, want 0", got)
	}
	t.Logf("夹具铺好后清了 %d 条缓存条目（铺库路径自己用过的缓存版读路径）", polluted)

	return &wiringEnv{
		db: db, userID: u.ID, nsID: ns.ID, sessID: sess.ID,
		capID: capDef.ID, capName: capDef.Name, capHash: capDef.ContentHash,
		eventID: ev.ID, traceID: ev.TraceID, idemKey: idemKey,
		taskID: task.ID,
	}
}

// drainStmtCache 关闭并清空 d 的语句缓存（测试夹具用；生产代码没有这个入口）。
//
// 直接操作 c.m / c.mu 而不是「换一个新的 stmtCache 值」：后者的赋值会把
// 一个 sync.RWMutex 拷来拷去（go vet 的 copylocks 会盯上），也没必要。
func drainStmtCache(d *DB) {
	d.stmts.mu.Lock()
	for q, st := range d.stmts.m {
		delete(d.stmts.m, q)
		_ = st.Close()
	}
	d.stmts.mu.Unlock()
}

// wiringStep 是「一个生产调用点 + 一次真实调用」。
type wiringStep struct {
	// callSite 写成 `<文件>:<行> <函数>`，行号是改造当时的落点（漂了也无所谓：
	// 名字才是断言要用的东西，行号只是给后来人省一次跳转）。
	callSite string
	call     func(ctx context.Context, e *wiringEnv) error
}

// wiringSteps 是 12 个调用点的逐项清单（顺序固定，便于报告里核对映射表）。
func wiringSteps() []wiringStep {
	return []wiringStep{
		{"auth.go:263 GetSession", func(ctx context.Context, e *wiringEnv) error {
			s, err := e.db.GetSession(e.sessID)
			if err != nil {
				return err
			}
			if s.ID != e.sessID {
				return fmt.Errorf("拿到会话 %q, want %q", s.ID, e.sessID)
			}
			return nil
		}},
		{"auth.go:295 ListSessionsByUser", func(ctx context.Context, e *wiringEnv) error {
			ss, err := e.db.ListSessionsByUser(e.userID)
			if err != nil {
				return err
			}
			if len(ss) != 1 {
				return fmt.Errorf("列出 %d 个会话, want 1", len(ss))
			}
			return nil
		}},
		{"authz.go:38 AuthorizeNamespace", func(ctx context.Context, e *wiringEnv) error {
			return e.db.AuthorizeNamespace(ctx, e.userID, e.nsID)
		}},
		{"authz.go:87 namespaceExists（入口 AuthorizeNamespaceForNode）", func(ctx context.Context, e *wiringEnv) error {
			return e.db.AuthorizeNamespaceForNode(ctx, e.nsID)
		}},
		{"capability.go:309 scanCapability ← GetCapability", func(ctx context.Context, e *wiringEnv) error {
			c, err := e.db.GetCapability(ctx, e.nsID, e.capName, 1)
			if err != nil {
				return err
			}
			if c.ID != e.capID {
				return fmt.Errorf("拿到能力 %q, want %q", c.ID, e.capID)
			}
			return nil
		}},
		{"capability.go:309 scanCapability ← GetCapabilityByID", func(ctx context.Context, e *wiringEnv) error {
			c, err := e.db.GetCapabilityByID(ctx, e.nsID, e.capID)
			if err != nil {
				return err
			}
			if c.Name != e.capName {
				return fmt.Errorf("拿到能力名 %q, want %q", c.Name, e.capName)
			}
			return nil
		}},
		{"capability.go:309 scanCapability ← getCapabilityByHash", func(ctx context.Context, e *wiringEnv) error {
			c, err := e.db.getCapabilityByHash(ctx, e.nsID, e.capHash)
			if err != nil {
				return err
			}
			if c.ID != e.capID {
				return fmt.Errorf("按 hash 拿到能力 %q, want %q", c.ID, e.capID)
			}
			return nil
		}},
		{"capability.go:386 ListCapabilities", func(ctx context.Context, e *wiringEnv) error {
			cs, err := e.db.ListCapabilities(ctx, e.nsID)
			if err != nil {
				return err
			}
			// 只断言夹具建的那条在列表里（不数总数：内置能力的播种是 RegisterUser 的事，
			// 那条路径变了对本例没有意义）。
			for _, c := range cs {
				if c.ID == e.capID {
					return nil
				}
			}
			return fmt.Errorf("能力列表（%d 行）里没有夹具建的 %s", len(cs), e.capID)
		}},
		{"event.go:192 scanEvent ← GetEvent", func(ctx context.Context, e *wiringEnv) error {
			got, err := e.db.GetEvent(ctx, e.nsID, e.eventID)
			if err != nil {
				return err
			}
			if got.ID != e.eventID {
				return fmt.Errorf("拿到事件 %q, want %q", got.ID, e.eventID)
			}
			return nil
		}},
		{"event.go:192 scanEvent ← GetEventByTrace", func(ctx context.Context, e *wiringEnv) error {
			got, err := e.db.GetEventByTrace(ctx, e.nsID, e.traceID)
			if err != nil {
				return err
			}
			if got.TraceID != e.traceID {
				return fmt.Errorf("拿到 trace %q, want %q", got.TraceID, e.traceID)
			}
			return nil
		}},
		{"event.go:192 scanEvent ← getEventByIdemKey", func(ctx context.Context, e *wiringEnv) error {
			got, err := e.db.getEventByIdemKey(ctx, e.nsID, "github", e.idemKey)
			if err != nil {
				return err
			}
			if got.ID != e.eventID {
				return fmt.Errorf("按幂等键拿到事件 %q, want %q", got.ID, e.eventID)
			}
			return nil
		}},
		{"event.go:252 ListEvents（无过滤）", func(ctx context.Context, e *wiringEnv) error {
			evs, err := e.db.ListEvents(ctx, e.nsID, "", "", 50)
			if err != nil {
				return err
			}
			if len(evs) != 1 {
				return fmt.Errorf("事件列表 %d 行, want 1", len(evs))
			}
			return nil
		}},
		{"namespace.go:129 GetNamespace", func(ctx context.Context, e *wiringEnv) error {
			ns, err := e.db.GetNamespace(ctx, e.nsID)
			if err != nil {
				return err
			}
			if ns.ID != e.nsID {
				return fmt.Errorf("拿到命名空间 %q, want %q", ns.ID, e.nsID)
			}
			return nil
		}},
		{"namespace.go:144 ListNamespaces", func(ctx context.Context, e *wiringEnv) error {
			nss, err := e.db.ListNamespaces(ctx)
			if err != nil {
				return err
			}
			for _, ns := range nss {
				if ns.ID == e.nsID {
					return nil
				}
			}
			return fmt.Errorf("命名空间列表（%d 行）里没有夹具建的那个 %s", len(nss), e.nsID)
		}},
		{"task.go:377 GetTask", func(ctx context.Context, e *wiringEnv) error {
			task, err := e.db.GetTask(ctx, e.nsID, e.taskID)
			if err != nil {
				return err
			}
			if task.ID != e.taskID {
				return fmt.Errorf("拿到任务 %q, want %q", task.ID, e.taskID)
			}
			return nil
		}},
		{"task.go:420 ListTasks（无过滤）", func(ctx context.Context, e *wiringEnv) error {
			ts, err := e.db.ListTasks(ctx, e.nsID, "", "", "", 50)
			if err != nil {
				return err
			}
			if len(ts) != 1 {
				return fmt.Errorf("任务列表 %d 行, want 1", len(ts))
			}
			return nil
		}},
	}
}

// TestStmtWiringAllReadPathsCached 是 MAJOR-2 的门禁本体：
// 12 个只读调用点（经 16 个入口触达）逐个跑一遍，每一处都必须让缓存条数恰好 +1；
// 再跑一遍必须 +0。
func TestStmtWiringAllReadPathsCached(t *testing.T) {
	ctx := context.Background()
	env := newWiringEnv(t)
	steps := wiringSteps()

	// 前置：入口清单至少要盖住现有的 12 个 `d.cached*` 调用点。
	// 这是**下限**断言，和上面总量一样故意宽松：将来新增读路径、往 wiringSteps 里加入口，
	// 不需要改这个数字（那不是回归）；只有「入口被删/合并到不足 12」——真丢了覆盖面——才该红。
	const wiringMinCallSites = 12
	if got := len(steps); got < wiringMinCallSites {
		t.Fatalf("入口清单 %d 条, 少于下限 %d（= 现有 d.cached* 调用点个数：是不是删了一步？）",
			got, wiringMinCallSites)
	}

	// ---------- 第一遍：每个入口冷启动，delta 必须恰好 1 ----------
	for i, step := range steps {
		before := env.db.stmts.len()
		if err := step.call(ctx, env); err != nil {
			t.Fatalf("[%02d] %s: %v", i+1, step.callSite, err)
		}
		if delta := env.db.stmts.len() - before; delta != 1 {
			t.Fatalf("[%02d] %s: 缓存条数增量 = %d, want 1（这一处没有走 cachedQuery*：被改回直查了？）\n"+
				"提示：本文件的门禁意义见文件头；回退任意一处 d.cached* 都会在这里变红。",
				i+1, step.callSite, delta)
		}
	}
	if got := env.db.stmts.len(); got < wiringMinDistinctSQL {
		// `>=` 而不是 `==`（别改回去，理由见文件头「不要改回 ==」）：
		// 新增读函数是常态，`==` 会把它变成噪音红灯；本条只当**地板**，
		// 兜住的是「整个缓存根本没被用上」这类整体退化。精度在下面逐入口的 delta 断言里。
		t.Fatalf("全部读路径入口（%d 个）跑完后缓存条数 = %d, 少于下限 %d"+
			"（这些读函数至少应产生 %d 条不同的 SQL 文本：某处被绕过 / 缓存整体没生效？）",
			len(steps), got, wiringMinDistinctSQL, wiringMinDistinctSQL)
	}
	countAfterFirstPass := env.db.stmts.len()
	t.Logf("第一遍：%d 个入口（12 个调用点）→ %d 条 SQL 文本（逐项 delta 均为 1；下限 %d）",
		len(steps), countAfterFirstPass, wiringMinDistinctSQL)

	// ---------- 第二遍：全部命中，delta 必须为 0 ----------
	for i, step := range steps {
		before := env.db.stmts.len()
		if err := step.call(ctx, env); err != nil {
			t.Fatalf("[%02d] 第二遍 %s: %v", i+1, step.callSite, err)
		}
		if delta := env.db.stmts.len() - before; delta != 0 {
			t.Fatalf("[%02d] 第二遍 %s: 缓存条数增量 = %d, want 0（同一入口第二次调用应命中缓存，"+
				"不得再次 prepare/落表——每次 +1 说明缓存键不稳定，或缓存被人整表换掉了）",
				i+1, step.callSite, delta)
		}
	}
	// 第二遍的性质是「命中不改变条数」。这里比的是**第一遍结束时的条数**而不是常量：
	// 那个等值关系与「新增读函数」无关，所以允许它保持严格（不会变成噪音）。
	if got := env.db.stmts.len(); got != countAfterFirstPass {
		t.Fatalf("第二遍跑完后缓存条数 = %d, 第一遍结束时 %d（命中不该改变条数；"+
			"每一项 delta == 0 已单独断言，这里是对应的总数交叉验算）", got, countAfterFirstPass)
	}
	t.Logf("第二遍：%d 个入口全部命中（逐项 delta 均为 0），缓存条数仍为 %d", len(steps), env.db.stmts.len())
}

// TestStmtWiringOptionalFiltersAreDistinctSQLs 钉住「可选过滤 → 多条 SQL 文本」这半边事实。
//
// ListTasks 的 3 个可选过滤（trace/node/status）拼在**列名**上、值走占位符，
// 所以文本种类数 = 2^3；ListEvents 的 2 个（trace/source）同理 = 2^2
// （注意 source 还有一条「非空且没有 trace 时排除 route」的隐藏分支，
// 它让 (trace="", source="") 与 (trace!="", source="") 也各是一种文本，
// 于是组合数正好还是 4，见 event.go:ListEvents 的拼串逻辑）。
//
// 断言的是「每种组合各自 +1、重复调用 +0」——而不是「至少 +1」：
// 只有逐组合 +1 才能证明**没有哪个过滤分支绕过缓存**（比如将来有人给某条分支
// 单独写一句直查，就会在这里红）。
func TestStmtWiringOptionalFiltersAreDistinctSQLs(t *testing.T) {
	ctx := context.Background()
	env := newWiringEnv(t)

	type combo struct {
		name  string
		trace string
		other string // ListTasks 用 nodeID，ListEvents 用 source
		third string // ListTasks 用 status
	}

	var listTasks []combo
	for i := 0; i < 8; i++ {
		c := combo{name: fmt.Sprintf("combo=%03b", i)}
		if i&1 != 0 {
			c.trace = "tr_x"
		}
		if i&2 != 0 {
			c.other = "nd_x"
		}
		if i&4 != 0 {
			c.third = TaskPending
		}
		listTasks = append(listTasks, c)
	}

	var listEvents []combo
	for i := 0; i < 4; i++ {
		c := combo{name: fmt.Sprintf("combo=%02b", i)}
		if i&1 != 0 {
			c.trace = "tr_x"
		}
		if i&2 != 0 {
			c.other = "github"
		}
		listEvents = append(listEvents, c)
	}

	run := func(stepName string, calls []combo, call func(c combo) error) {
		t.Helper()
		for _, c := range calls {
			before := env.db.stmts.len()
			if err := call(c); err != nil {
				t.Fatalf("%s %s: %v", stepName, c.name, err)
			}
			if delta := env.db.stmts.len() - before; delta != 1 {
				t.Fatalf("%s %s: 缓存条数增量 = %d, want 1（每种过滤组合都是一种独立的 SQL 文本）",
					stepName, c.name, delta)
			}
		}
		// 再来一遍：全部命中。
		for _, c := range calls {
			before := env.db.stmts.len()
			if err := call(c); err != nil {
				t.Fatalf("%s %s 第二遍: %v", stepName, c.name, err)
			}
			if delta := env.db.stmts.len() - before; delta != 0 {
				t.Fatalf("%s %s 第二遍: 缓存条数增量 = %d, want 0（同一组合应命中）",
					stepName, c.name, delta)
			}
		}
	}

	run("ListTasks", listTasks, func(c combo) error {
		_, err := env.db.ListTasks(ctx, env.nsID, c.trace, c.other, c.third, 50)
		return err
	})
	if got, want := env.db.stmts.len(), len(listTasks); got != want {
		t.Fatalf("ListTasks 的 %d 种过滤组合应产生 %d 条文本，实际缓存 %d 条", want, want, got)
	}
	t.Logf("ListTasks：%d 种过滤组合 → %d 条 SQL 文本", len(listTasks), env.db.stmts.len())

	beforeEvents := env.db.stmts.len()
	run("ListEvents", listEvents, func(c combo) error {
		_, err := env.db.ListEvents(ctx, env.nsID, c.trace, c.other, 50)
		return err
	})
	if got, want := env.db.stmts.len()-beforeEvents, len(listEvents); got != want {
		t.Fatalf("ListEvents 的 %d 种过滤组合应新增 %d 条文本，实际新增 %d 条", want, want, got)
	}
	t.Logf("ListEvents：%d 种过滤组合 → 新增 %d 条 SQL 文本（累计 %d 条）",
		len(listEvents), len(listEvents), env.db.stmts.len())
}
