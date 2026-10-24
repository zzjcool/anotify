package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试夹具 ----------

// fixture 是一套带真 SQLite 的触发器环境。
type fixture struct {
	db         *store.DB
	orgID      string
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

// newFixture 建库 + 组织 + 两个 origin 能力（github@1、cron@1）。
func newFixture(t *testing.T, originCaps ...string) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(t.TempDir() + "/trigger.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	org, err := db.CreateOrg(ctx, "org-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// 默认建 github + cron 两个 origin 能力（无节点声明 = 天然 origin）
	if len(originCaps) == 0 {
		originCaps = []string{"github", "cron", "watch"}
	}
	for _, name := range originCaps {
		c := &store.CapabilityVersion{
			OrgID:        org.ID,
			Name:         name,
			Version:      1,
			ParamsSchema: json.RawMessage(`{"type":"object"}`),
			Outputs:      []string{"some.event"},
			Source:       store.CapSourceBuiltin,
		}
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create origin capability %s: %v", name, err)
		}
	}

	disp := &fakeDispatcher{}
	svc := New(db, disp)
	// 真实 trace_id 便于断言前缀
	return &fixture{db: db, orgID: org.ID, svc: svc, dispatcher: disp}
}

// ---------- #58 验收：幂等 ----------

// TestEventIdempotent 同一 idemKey 两次 → 只有一个 event，两次都返回同一 trace_id（#58 验收）。
func TestEventIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "delivery-abc-123"

	first, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "github", Type: "pull_request.opened",
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
		OrgID: f.orgID, Source: "github", Type: "pull_request.opened",
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

	// 库里只有一行
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE org_id = ? AND source = ?`, f.orgID, "github").Scan(&count); err != nil {
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
			OrgID: f.orgID, Source: "github", Type: "push",
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
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE org_id = ?`, f.orgID).Scan(&count); err != nil {
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
		OrgID: f.orgID, Source: "github", Type: "push", IdemKey: &empty,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Event.IdemKey != nil {
		t.Fatalf("whitespace idemKey was stored as %q, want NULL", *res.Event.IdemKey)
	}
	// 再来一条也不冲突
	res2, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "github", Type: "push", IdemKey: &empty,
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
		OrgID: f.orgID, Source: "github", Type: "push", IdemKey: &key,
	})
	if err != nil {
		t.Fatalf("github ingest: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "cron", Type: "push", IdemKey: &key,
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

// TestIdemKeyScopedToOrg 同一 idemKey 在不同组织下互不影响（I10：一切过 org_id）。
func TestIdemKeyScopedToOrg(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 第二个组织
	other, err := f.db.CreateOrg(ctx, "other-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	otherID := other.ID
	// 给新组织也建 github 能力（否则找不到 origin）
	if err := f.db.CreateCapability(ctx, &store.CapabilityVersion{
		OrgID: otherID, Name: "github", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Source: store.CapSourceBuiltin,
	}); err != nil {
		t.Fatalf("create capability for other org: %v", err)
	}

	key := "shared-key"
	r1, err := f.svc.Ingest(ctx, &IngestRequest{OrgID: f.orgID, Source: "github", Type: "push", IdemKey: &key})
	if err != nil {
		t.Fatalf("org1 ingest: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{OrgID: otherID, Source: "github", Type: "push", IdemKey: &key})
	if err != nil {
		t.Fatalf("org2 ingest: %v", err)
	}
	if !r1.Inserted || !r2.Inserted {
		t.Fatal("same idemKey across orgs must not collide (I10)")
	}
}

// ---------- #58 验收：trace_id 由触发器生成 ----------

// TestTraceIDFromTrigger 不给 traceId 时由触发器生成，且形如 trace_*（#58 验收）。
func TestTraceIDFromTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "github", Type: "push",
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
		OrgID: f.orgID, Source: "github", Type: "push",
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
		OrgID: f.orgID, Source: "cron", Type: "tick",
		IdemKey: &key, TraceID: CronTraceID("nightly", fire),
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "cron", Type: "tick",
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
		OrgID: f.orgID, Source: "cron", Type: "tick", IdemKey: &nextKey,
	})
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if !r3.Inserted {
		t.Fatal("a different fire time must produce a new event")
	}
}

// ---------- #58：接进派发 ----------

// TestOriginCapabilityUsedAsSource 派发时用 origin 能力的 ID + 事件 type 作为出口。
func TestOriginCapabilityUsedAsSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":42,"title":"fix bug"}`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.OriginCapability == nil || res.OriginCapability.Name != "github" {
		t.Fatalf("origin capability = %v, want github", res.OriginCapability)
	}

	calls := f.dispatcher.calls()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(calls))
	}
	in := calls[0]
	if in.FromCap != res.OriginCapability.ID {
		t.Fatalf("FromCap = %q, want %q", in.FromCap, res.OriginCapability.ID)
	}
	if in.Out != "pull_request.opened" {
		t.Fatalf("Out = %q, want the event type", in.Out)
	}
	if in.OrgID != f.orgID {
		t.Fatalf("OrgID = %q", in.OrgID)
	}
	// path 从 origin 能力开始（环剪枝的起点）
	if len(in.Path) != 1 || in.Path[0] != res.OriginCapability.ID {
		t.Fatalf("Path = %v, want [origin capability]", in.Path)
	}
	// payload 作为 params 透传（不求值、不改写）
	if in.Params["number"] != 42.0 {
		t.Fatalf("params.number = %v, want 42", in.Params["number"])
	}
	if in.Params["title"] != "fix bug" {
		t.Fatalf("params.title = %v", in.Params["title"])
	}
}

// TestUnknownOrigin 没有对应 origin 能力 → ErrUnknownOrigin，但事件已入库。
//
// 这是配置问题不是客户端错误：事件确实收下了（200），只是没法派发。
func TestUnknownOrigin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "jira", Type: "issue.created", // jira 没定义
	})
	if !errors.Is(err, ErrUnknownOrigin) {
		t.Fatalf("err = %v, want ErrUnknownOrigin", err)
	}
	if res == nil || res.Event == nil {
		t.Fatal("event should still be returned even when origin is unknown")
	}
	if !res.Inserted {
		t.Fatal("event should have been inserted")
	}
	// 库里确实有这一行
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE org_id = ? AND source = ?`, f.orgID, "jira").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("event not persisted: count=%d", count)
	}
	// 没有派发
	if len(f.dispatcher.calls()) != 0 {
		t.Fatal("dispatched despite unknown origin")
	}
}

// TestDispatchFailureKeepsEvent 派发失败不影响事件落库（重投也不该重复建事件）。
func TestDispatchFailureKeepsEvent(t *testing.T) {
	f := newFixture(t)
	f.dispatcher.failWith = fmt.Errorf("nodes gone")
	ctx := context.Background()

	res, err := f.svc.Ingest(ctx, &IngestRequest{
		OrgID: f.orgID, Source: "github", Type: "push",
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
				OrgID: f.orgID, Source: "github", Type: "push",
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
		OrgID: f.orgID, Source: "github", Type: "push",
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
func TestIngestValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		req  *IngestRequest
	}{
		{name: "nil request", req: nil},
		{name: "no org", req: &IngestRequest{Source: "github", Type: "push"}},
		{name: "no source", req: &IngestRequest{OrgID: f.orgID, Type: "push"}},
		{name: "no type", req: &IngestRequest{OrgID: f.orgID, Source: "github"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.svc.Ingest(context.Background(), c.req); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
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
				OrgID: f.orgID, Source: "github", Type: "push", IdemKey: &key,
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
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE org_id = ? AND idem_key = ?`, f.orgID, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("events with same idemKey = %d, want 1", count)
	}
	if insertedCount > 1 {
		t.Fatalf("%d goroutines reported inserted=true, want at most 1", insertedCount)
	}
}
