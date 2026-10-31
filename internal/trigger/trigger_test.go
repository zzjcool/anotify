package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试夹具 ----------

// closeDBForTempDir 是夹具的确定性收口（#259，同 internal/auth / internal/server 的判据）：
// 关库前 InUse==0、关库后 OpenConnections==0、目录里无 *.db-wal|*.db-shm。
//
// 为什么断言而不是只 Close：CI 上出现过 `TempDir RemoveAll cleanup: ... directory not empty`
// （失败现场只剩一个 `*.db-shm`）——那是「关库之后还有活着的 SQLite 连接重建 WAL 文件」。
// 只做 db.Close() 的话，这个竞态会在 t.TempDir 的 RemoveAll 里随机炸掉某个用例；
// 在这里断言就变成确定性的「收尾竞态」失败，并且点名是哪个条件不成立。
func closeDBForTempDir(t *testing.T, db *store.DB, dir string) {
	t.Helper()
	// 断言失败会走 t.Fatalf（跳过余下语句）：defer 兜底关库，避免失败路径上再制造一层 TempDir 噪声。
	defer func() { _ = db.Close() }()
	if n := db.Stats().InUse; n != 0 {
		t.Fatalf("收尾竞态：%d 条 DB 连接仍被 goroutine 持有（应在 db.Close 前等它们返回）", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("收尾竞态：db.Close 失败（有 goroutine 在关库窗口里用库？）: %v", err)
	}
	if n := db.Stats().OpenConnections; n != 0 {
		t.Fatalf("收尾竞态：db.Close 返回后仍有 %d 条连接没释放（有并发使用者）", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读临时目录失败: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") {
			t.Fatalf("收尾竞态：db.Close 后仍残留 WAL 文件 %s（有连接在 RemoveAll 期间重建文件）", name)
		}
	}
}

// fixture 是一套带真 SQLite 的触发器环境。
type fixture struct {
	db *store.DB
	// nsID 是注册用户时自带的 default 命名空间：事件/能力/边都落在它里
	// （事件必须显式带命名空间）。组织层已随迁移 14 删除，命名空间是唯一隔离维度。
	nsID       string
	svc        *Service
	dispatcher *fakeDispatcher
}

// fakeDispatcher 记录收到的派发请求，返回可配置的结果。
type fakeDispatcher struct {
	mu       sync.Mutex
	inputs   []route.DispatchInput
	failWith error
	result   *route.DispatchOutput
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, in route.DispatchInput) (*route.DispatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.result != nil {
		return f.result, nil
	}
	return &route.DispatchOutput{
		Tasks:  []*store.Task{{ID: store.NewTaskID(), TraceID: in.TraceID}},
		Pruned: []route.PrunedRecord{},
	}, nil
}

func (f *fakeDispatcher) calls() []route.DispatchInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]route.DispatchInput, len(f.inputs))
	copy(out, f.inputs)
	return out
}

// newFixture 建库 + 一个用户（自带 default 命名空间）+ 两个 origin 能力（github@1、cron@1）。
func newFixture(t *testing.T, originCaps ...string) *fixture {
	t.Helper()
	ctx := context.Background()
	// 收尾走 closeDBForTempDir（#259）：关库钉在 t.TempDir 的 RemoveAll 之前，并断言无并发使用者。
	dir := t.TempDir()
	db, err := store.Open(dir + "/trigger.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	// 组织层已随迁移 14 删除（#204）：注册用户即建它的 default 命名空间，
	// 并在同一事务里播种内置能力（契约 §2.5）。返回的 ns.ID 就是默认命名空间 id。
	ns, err := db.RegisterUser(ctx, &store.User{
		ID:          store.NewUserID(),
		Username:    "u-" + store.NewUserID(),
		DisplayName: "测试",
		CreatedAt:   store.Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID := ns.ID

	// #110：不再需要"起点能力"。事件的来源由 source 名承担，
	// 派发靠入口边（from_is_source=1）。这里建几条入口边，
	// 让 Ingest 之后的派发有东西可查。
	//
	// 注意：测试关心的是"入口边被正确查询"，不是"能力存在"。
	sink := &store.CapabilityVersion{
		NamespaceID:  nsID,
		Name:         "sink",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{},
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, sink); err != nil {
		t.Fatalf("create sink: %v", err)
	}
	directInputs := len(originCaps) == 0
	if directInputs {
		originCaps = []string{"github", "cron", "watch"}
	}
	_ = directInputs
	for _, name := range originCaps {
		if err := db.CreateEdge(ctx, &store.Edge{
			NamespaceID:    nsID,
			FromIsSource:   true,
			FromCapability: name,
			FromPort:       "pull_request.opened",
			ToCapability:   sink.ID,
		}); err != nil {
			t.Fatalf("create entry edge for %s: %v", name, err)
		}
	}

	disp := &fakeDispatcher{}
	svc := New(db, disp)
	// 真实 trace_id 便于断言前缀
	return &fixture{db: db, nsID: nsID, svc: svc, dispatcher: disp}
}

// ---------- #58 验收：幂等 ----------

// TestEventIdempotent 同一 idemKey 两次 → 只有一个 event，两次都返回同一 trace_id（#58 验收）。
func TestEventIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "delivery-abc-123"

	first, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		IdemKey: &key,
		Payload: json.RawMessage(`{"number":42}`),
	})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first ingest should insert")
	}

	second, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		IdemKey: &key,
		Payload: json.RawMessage(`{"number":42}`),
	})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Inserted {
		t.Fatal("second ingest with same idemKey should NOT insert")
	}

	// 两此都返回同一 event_id + trace_id（不是 409）
	if second.Event.ID != first.Event.ID {
		t.Fatalf("event_id differs: %s vs %s", first.Event.ID, second.Event.ID)
	}
	if second.Event.TraceID != first.Event.TraceID {
		t.Fatalf("trace_id differs: %s vs %s", first.Event.TraceID, second.Event.TraceID)
	}
	// 命名空间跟着请求走（事件行上的 namespaceId 是隔离维度）
	if first.Event.NamespaceID != f.nsID {
		t.Fatalf("event.NamespaceID = %q, want %q（IngestRequest.NamespaceID 必须落库）",
			first.Event.NamespaceID, f.nsID)
	}

	// 库里只有一行
	var count int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE namespace_id = ? AND source = ?`, f.nsID, "github").Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("events table has %d rows, want 1", count)
	}

	// 幂等命中时**不重复派发**（只有第一次触发了一次）
	if got := len(f.dispatcher.calls()); got != 1 {
		t.Fatalf("dispatch called %d times, want 1 (idempotent hit must not re-dispatch)", got)
	}
}

// TestNoIdemKeyAllowsDuplicates 不带 idemKey → 允许重复（契约 §10.1）。
func TestNoIdemKeyAllowsDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		res, err := f.svc.Ingest(ctx, &IngestRequest{
			NamespaceID: f.nsID, Source: "github", Type: "push",
			Payload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		if !res.Inserted {
			t.Fatalf("ingest %d without idemKey should always insert", i)
		}
	}

	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE namespace_id = ?`, f.nsID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("events = %d, want 3 (no idemKey means no dedup)", count)
	}
	// 三次都派发了
	if got := len(f.dispatcher.calls()); got != 3 {
		t.Fatalf("dispatch called %d times, want 3", got)
	}
}

// TestEmptyIdemKeyTreatedAsAbsent 空字符串 idemKey 被当作「没有」。
//
// 空串若入库会让所有「带了空键」的事件互相认成同一条（静默丢事件）。
func TestEmptyIdemKeyTreatedAsAbsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	empty := "   "

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &empty,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Event.IdemKey != nil {
		t.Fatalf("whitespace idemKey was stored as %q, want NULL", *res.Event.IdemKey)
	}
	// 再来一条也不冲突
	res2, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &empty,
	})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if !res2.Inserted {
		t.Fatal("two empty idemKeys should not collide")
	}
}

// TestIdemKeyScopedToSource 同一 idemKey 在不同 source 下互不影响（唯一索引含 source）。
func TestIdemKeyScopedToSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "same-key-different-source"

	r1, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &key,
	})
	if err != nil {
		t.Fatalf("github ingest: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "cron", Type: "push", IdemKey: &key,
	})
	if err != nil {
		t.Fatalf("cron ingest: %v", err)
	}
	if !r1.Inserted || !r2.Inserted {
		t.Fatal("same idemKey under different sources must not collide (unique index includes source)")
	}
	if r1.Event.ID == r2.Event.ID {
		t.Fatal("different sources produced the same event_id")
	}
}

// TestIdemKeyScopedToDefaultNamespace 同一 idemKey 在两个 default 命名空间下互不影响。
//
// 原来叫 TestIdemKeyScopedToOrg：断言「同一投递 ID 在两个组织里各自插入」。
// 组织层已随迁移 14 删除（#204），隔离边界只剩命名空间——所以这里换成
// 「两个用户各自的 default 命名空间」，断言强度不变（仍然是跨隔离边界不冲突）：
// 幂等窗口永远锚在一个命名空间上，不会退化成部署级全局窗口。
func TestIdemKeyScopedToDefaultNamespace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 第二个用户自带的 default 命名空间：它是幂等窗口的第二个隔离锚点
	other, err := f.db.RegisterUser(ctx, &store.User{
		ID:          store.NewUserID(),
		Username:    "u-" + store.NewUserID(),
		DisplayName: "另一个用户",
		CreatedAt:   store.Now(),
	})
	if err != nil {
		t.Fatalf("register other user: %v", err)
	}
	otherNS := other.ID
	if otherNS == f.nsID {
		t.Fatalf("第二个用户的 default 命名空间与第一个相同（%q）", otherNS)
	}

	key := "shared-key"
	r1, err := f.svc.Ingest(ctx, &IngestRequest{NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &key})
	if err != nil {
		t.Fatalf("first ns ingest: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{NamespaceID: otherNS, Source: "github", Type: "push", IdemKey: &key})
	if err != nil {
		t.Fatalf("second ns ingest: %v", err)
	}
	if !r1.Inserted || !r2.Inserted {
		t.Fatal("same idemKey across namespaces must not collide (I10)")
	}
	if r1.Event.ID == r2.Event.ID {
		t.Fatal("两个命名空间产生了同一个 event_id")
	}
	if r1.Event.NamespaceID != f.nsID || r2.Event.NamespaceID != otherNS {
		t.Fatalf("事件落错命名空间：%q/%q，want %q/%q",
			r1.Event.NamespaceID, r2.Event.NamespaceID, f.nsID, otherNS)
	}
}

// TestIdemKeyScopedToNamespace 两个命名空间里，同一 idemKey 互不影响。
//
// 这是 #162 的核心语义（唯一索引是 (namespace_id, source, idem_key)）：
// 同一个 GitHub 投递 ID 在 default 与 staging 是两条不同的事件。
// 旧实现按 org_id 做幂等，这条会直接红（第二条被当幂等命中而静默丢掉）。
func TestIdemKeyScopedToNamespace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	second, err := f.db.CreateNamespace(ctx, "staging", "预发")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}

	key := "same-delivery-id"
	r1, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &key,
	})
	if err != nil {
		t.Fatalf("default ns ingest: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: second.ID, Source: "github", Type: "push", IdemKey: &key,
	})
	if err != nil {
		t.Fatalf("staging ns ingest: %v", err)
	}
	if !r1.Inserted || !r2.Inserted {
		t.Fatal("同一 idemKey 在两个命名空间里必须各自插入（幂等窗口按命名空间）")
	}
	if r1.Event.ID == r2.Event.ID {
		t.Fatal("两个命名空间产生了同一个 event_id")
	}
	if r2.Event.NamespaceID != second.ID {
		t.Fatalf("event.namespaceId = %q, want %q", r2.Event.NamespaceID, second.ID)
	}
	// 库里按命名空间各一行
	var n1, n2 int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE namespace_id = ? AND idem_key = ?`, f.nsID, key).Scan(&n1); err != nil {
		t.Fatalf("count default ns: %v", err)
	}
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE namespace_id = ? AND idem_key = ?`, second.ID, key).Scan(&n2); err != nil {
		t.Fatalf("count staging ns: %v", err)
	}
	if n1 != 1 || n2 != 1 {
		t.Fatalf("events per namespace = %d/%d, want 1/1", n1, n2)
	}
}

// ---------- #58 验收：trace_id 由触发器生成 ----------

// TestTraceIDFromTrigger 不给 traceId 时由触发器生成，且形如 trace_*（#58 验收）。
func TestTraceIDFromTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Event.TraceID == "" {
		t.Fatal("trace_id was not generated")
	}
	if !strings.HasPrefix(res.Event.TraceID, "trace_") {
		t.Fatalf("trace_id = %q, want trace_* prefix", res.Event.TraceID)
	}
	// 派发时带的就是这个 trace_id
	calls := f.dispatcher.calls()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %d", len(calls))
	}
	if calls[0].TraceID != res.Event.TraceID {
		t.Fatalf("dispatched trace_id = %q, want %q", calls[0].TraceID, res.Event.TraceID)
	}
}

// TestTraceIDUniqueness 并发下 trace_id 不重复。
//
// 并发部分只负责**收集**（通过 channel 交回主 goroutine），
// 判重与断言全在主 goroutine 上做——避免「goroutine 里写共享 map + 调 t.Errorf」
// 那种既有竞态风险又难断言的写法。
func TestTraceIDUniqueness(t *testing.T) {
	const n = 500
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- NewTraceID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, n)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate trace_id: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique trace_ids, want %d", len(seen), n)
	}
}

// TestExplicitTraceIDHonored 调用方给了 traceId 时沿用（同一 trace 串多个事件）。
func TestExplicitTraceIDHonored(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push",
		TraceID: "trace_caller_supplied",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Event.TraceID != "trace_caller_supplied" {
		t.Fatalf("trace_id = %q, want the supplied one", res.Event.TraceID)
	}
}

// TestCronDeterministicID cron 触发器用确定性 trace/idem key。
func TestCronDeterministicID(t *testing.T) {
	fire := int64(1730000000)
	if got := CronTraceID("nightly", fire); got != "cron:nightly:1730000000" {
		t.Fatalf("CronTraceID = %q", got)
	}
	// 同一 cron 同一时刻 → 同一个键（可被幂等挡住）
	if CronIdemKey("nightly", fire) != CronIdemKey("nightly", fire) {
		t.Fatal("cron idem key is not deterministic")
	}
	// 不同时刻 → 不同键
	if CronIdemKey("nightly", fire) == CronIdemKey("nightly", fire+60) {
		t.Fatal("cron idem key does not vary with fire time")
	}
	// 不同 cron 名 → 不同键
	if CronIdemKey("nightly", fire) == CronIdemKey("hourly", fire) {
		t.Fatal("cron idem key does not vary with name")
	}
}

// TestCronIdempotentAcrossFire 同一 cron 同一时刻重放 → 幂等。
func TestCronIdempotentAcrossFire(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	fire := int64(1730000000)
	key := CronIdemKey("nightly", fire)

	r1, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "cron", Type: "tick",
		IdemKey: &key, TraceID: CronTraceID("nightly", fire),
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "cron", Type: "tick",
		IdemKey: &key, TraceID: CronTraceID("nightly", fire),
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !r1.Inserted || r2.Inserted {
		t.Fatal("same cron fire time should be idempotent")
	}
	if r1.Event.ID != r2.Event.ID {
		t.Fatal("cron replay produced a different event")
	}
	// 下一分钟是新事件
	nextKey := CronIdemKey("nightly", fire+60)
	r3, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "cron", Type: "tick", IdemKey: &nextKey,
	})
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if !r3.Inserted {
		t.Fatal("a different fire time must produce a new event")
	}
}

// ---------- #58：接进派发 ----------

// TestEntryEdgeUsesSourceName 派发时用**来源名** + 事件 type 作为入口边。
//
// 旧模型要求先找到一个叫 github@1 的"起点能力"当锚点（OriginCapability +
// Path 从它开始）。#110 之后入口边左端就是来源名，不需要那个空壳。
func TestEntryEdgeUsesSourceName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":42,"title":"fix bug"}`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	calls := f.dispatcher.calls()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(calls))
	}
	in := calls[0]

	// 入口边：FromCap 是来源名，FromIsSource=true
	if !in.FromIsSource {
		t.Fatal("入口派发必须标记 FromIsSource（否则会去能力表里找 github@1）")
	}
	if in.FromCap != "github" {
		t.Fatalf("FromCap = %q, want the source name \"github\"", in.FromCap)
	}
	if in.Out != "pull_request.opened" {
		t.Fatalf("Out = %q, want the event type", in.Out)
	}
	// 旧断言曾经在这里检查派发的 OrgID（「事件属于哪个组织」）。
	// 组织层随迁移 14 删除（#204），DispatchInput 已无 OrgID 字段：
	// 隔离维度的断言就是紧接着这条 in.NamespaceID —— 不再另设一条平行断言。
	// 派发必须带命名空间：route 对空 NamespaceID 直接报错（不回退默认命名空间），
	// 而且查边/找节点/建任务全按它过滤。
	if in.NamespaceID != f.nsID {
		t.Fatalf("NamespaceID = %q, want %q（派发必须带命名空间）", in.NamespaceID, f.nsID)
	}
	if res.Event.NamespaceID != f.nsID {
		t.Fatalf("event.NamespaceID = %q, want %q", res.Event.NamespaceID, f.nsID)
	}
	// 来源不是能力，不参与「A→B→A」环检测
	if len(in.Path) != 0 {
		t.Fatalf("Path = %v, want empty（来源不是能力，不进环检测）", in.Path)
	}
	// payload 作为 params 透传（不求值、不改写）
	if in.Params["number"] != 42.0 {
		t.Fatalf("params.number = %v, want 42", in.Params["number"])
	}
	if in.Params["title"] != "fix bug" {
		t.Fatalf("params.title = %v", in.Params["title"])
	}
	_ = res
}

// TestUnknownSourceNoEntryEdge 来源没有任何入口边 → 事件照收，但记 pruned/no_edge。
//
// 这是 #110 之后的语义：来源不再需要"对应一个能力"。
// 一个从没配过的 source（比如 jira）会被接受，事件入库（200），
// 首轮派发时查不到入口边 → pruned reason=no_edge。
//
// 这比旧的 422「未知来源」更符合实际：用户可能先接事件、后配边
// （比如先让 GitHub webhook 打着，再回来建边）。事件不该丢。
func TestUnknownSourceNoEntryEdge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "jira", Type: "issue.created",
	})
	if err != nil {
		t.Fatalf("ingest 不该因来源未配置而失败: %v", err)
	}
	if res == nil || res.Event == nil {
		t.Fatal("event should be returned")
	}
	if !res.Inserted {
		t.Fatal("event should have been inserted")
	}
	// 库里有这一行
	var count int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE namespace_id = ? AND source = ?`, f.nsID, "jira",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("events row count = %d, want 1", count)
	}
	// trigger 的职责只是"把来源名 + 事件类型交给派发器"。
	// 查不到边 → pruned/no_edge 是 **Dispatcher** 的事（route 包负责），
	// 这里的 fakeDispatcher 不查库，所以不该断言 Pruned。
	// 断言它收到的入口参数是对的即可。
	calls := f.dispatcher.calls()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(calls))
	}
	in := calls[0]
	if !in.FromIsSource || in.FromCap != "jira" || in.Out != "issue.created" {
		t.Fatalf("dispatch input = {FromIsSource:%v FromCap:%q Out:%q}, want {true jira issue.created}",
			in.FromIsSource, in.FromCap, in.Out)
	}
} // TestDispatchFailureKeepsEvent 派发失败不影响事件落库（重投也不该重复建事件）。
func TestDispatchFailureKeepsEvent(t *testing.T) {
	f := newFixture(t)
	f.dispatcher.failWith = fmt.Errorf("nodes gone")
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push",
	})
	if err == nil {
		t.Fatal("expected dispatch error to propagate")
	}
	if res == nil || res.Event == nil || res.Event.ID == "" {
		t.Fatal("event should be returned even when dispatch fails")
	}
	// 事件已入库
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id = ?`, res.Event.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("event was rolled back by dispatch failure: count=%d", count)
	}
}

// TestPayloadPassThrough 非对象 payload 被包成 {"payload": ...}，对象原样透传。
func TestPayloadPassThrough(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantKey string
		wantVal any
	}{
		{name: "object passes through", payload: `{"a":1}`, wantKey: "a", wantVal: 1.0},
		{name: "array wrapped", payload: `[1,2]`, wantKey: "payload", wantVal: nil},
		{name: "string wrapped", payload: `"hello"`, wantKey: "payload", wantVal: "hello"},
		{name: "number wrapped", payload: `42`, wantKey: "payload", wantVal: 42.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Ingest(context.Background(), &IngestRequest{
				NamespaceID: f.nsID, Source: "github", Type: "push",
				Payload: json.RawMessage(c.payload),
			})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			calls := f.dispatcher.calls()
			if len(calls) != 1 {
				t.Fatalf("dispatch calls = %d", len(calls))
			}
			params := calls[0].Params
			if _, ok := params[c.wantKey]; !ok {
				t.Fatalf("params missing key %q: %v", c.wantKey, params)
			}
			if c.wantVal != nil && params[c.wantKey] != c.wantVal {
				t.Fatalf("params[%q] = %v, want %v", c.wantKey, params[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestInvalidPayloadDefaultsToEmpty 坏的 payload 被落成 {}，不报错。
func TestInvalidPayloadDefaultsToEmpty(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Ingest(context.Background(), &IngestRequest{
		NamespaceID: f.nsID, Source: "github", Type: "push",
		Payload: json.RawMessage(`{not json`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if string(res.Event.Payload) != "{}" {
		t.Fatalf("payload = %s, want {}", res.Event.Payload)
	}
}

// TestIngestValidation 入参校验。
//
// 必填项现在是 namespaceId / source / type。原来这里有一条 "no org" 用例，
// 对应 import("ingest: empty org_id") 那个分支——它随组织层一起被删除（迁移 14）。
// 缺 namespace 的用例本来就是同一个请求形状（NamespaceID 空、其余齐全），
// 所以两条合并成一条，并把断言从「err != nil」升级为哨兵错误检查：
// 缺 namespace 必须可 errors.Is(store.ErrNamespaceRequired)，不能只靠字符串匹配。
func TestIngestValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name    string
		req     *IngestRequest
		wantErr error // 非 nil 时额外断言 errors.Is
	}{
		{name: "nil request", req: nil},
		// 命名空间必填：缺了就是错误，不静默回退默认命名空间（契约 §6.2）
		{name: "no namespace", req: &IngestRequest{Source: "github", Type: "push"}, wantErr: store.ErrNamespaceRequired},
		{name: "no source", req: &IngestRequest{NamespaceID: f.nsID, Type: "push"}},
		{name: "no type", req: &IngestRequest{NamespaceID: f.nsID, Source: "github"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.Ingest(context.Background(), c.req)
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want errors.Is(%v)", err, c.wantErr)
			}
		})
	}
	// 缺命名空间必须能认出来（哨兵错误，不依赖字符串匹配）
	if _, err := f.svc.Ingest(context.Background(), &IngestRequest{
		Source: "github", Type: "push",
	}); !errors.Is(err, store.ErrNamespaceRequired) {
		t.Fatalf("缺 namespace 的错误应可 errors.Is(store.ErrNamespaceRequired)，实际 %v", err)
	}
	// 校验失败不该产生事件
	var count int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM events`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("validation failures created %d events", count)
	}
}

// TestConcurrentIngestSameIdemKey 并发同一 idemKey → 只有一行（唯一索引兜底）。
//
// 同样只让 goroutine 收集结果，统计与断言在主 goroutine 完成。
func TestConcurrentIngestSameIdemKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "concurrent-key"

	const n = 10
	inserted := make(chan bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.svc.Ingest(ctx, &IngestRequest{
				NamespaceID: f.nsID, Source: "github", Type: "push", IdemKey: &key,
			})
			if err != nil {
				// 并发下 SQLite 可能返回 busy；这不是被测行为
				inserted <- false
				return
			}
			inserted <- res.Inserted
		}()
	}
	wg.Wait()
	close(inserted)

	insertedCount := 0
	for ok := range inserted {
		if ok {
			insertedCount++
		}
	}

	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE namespace_id = ? AND idem_key = ?`, f.nsID, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("events with same idemKey = %d, want 1", count)
	}
	if insertedCount > 1 {
		t.Fatalf("%d goroutines reported inserted=true, want at most 1", insertedCount)
	}
}
