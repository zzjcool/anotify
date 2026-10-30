package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// 本文件覆盖 #142 的存储侧：剪枝事件怎么落、怎么被查。
//
// 为什么不 import route 包来拿 PruneEventSource / PruneEventType：
// route 依赖 store，store 反向 import 会成环。所以这里写**字面量**，
// 顺便充当一份独立的断言——route 那边把 source/type 拼错了，这里就会红。

// mkPruneEvent 造一条剪枝事件（source='route'、type='pruned.<reason>'）。
func mkPruneEvent(orgID, traceID, reason, payload string) *Event {
	return &Event{
		OrgID:   orgID,
		TraceID: traceID,
		Source:  "route",
		Type:    "pruned." + reason,
		Payload: json.RawMessage(payload),
	}
}

// prunePayloadNoEdge 是一条典型的 no_edge payload（四字段里 targetCapability 缺省）。
const prunePayloadNoEdge = `{"fromCapability":"github","port":"pull_request.closed","detail":"no edge from github port \"pull_request.closed\""}`

// countEvents 数 events 表里满足条件的行数。
func countEvents(t *testing.T, db *DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count events (%s): %v", where, err)
	}
	return n
}

// TestInsertPruneEventIdempotent 同一条剪枝写两次只有 1 行；任何一处不同就是新行。
//
// 为什么测这个：剪枝会在扇出里被重复判定（同一条 trace 里同一类剪枝可能触发多次），
// 不去重的话 events 表会被同一条证据刷屏，「为什么没跑」反而更难看清。
// 去重靠 store 层的存在性判断（不是 idem_key——剪枝不占幂等键语义）。
func TestInsertPruneEventIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)

	first := mkPruneEvent(orgID, "trace_1", "no_edge", prunePayloadNoEdge)
	inserted, err := db.InsertPruneEvent(ctx, first)
	if err != nil {
		t.Fatalf("insert prune event: %v", err)
	}
	if !inserted {
		t.Fatal("首次写剪枝 inserted=false，want true")
	}
	if first.ID == "" {
		t.Error("ID 没被自动填上（应与 InsertEvent 的默认值口径一致）")
	}
	if first.CreatedAt == 0 {
		t.Error("CreatedAt 没被填成 Now()")
	}

	// 同一条再写一次（新的对象、同一个键）→ 不重复写，且不报错
	again := mkPruneEvent(orgID, "trace_1", "no_edge", prunePayloadNoEdge)
	inserted, err = db.InsertPruneEvent(ctx, again)
	if err != nil {
		t.Fatalf("第二次写同一条剪枝: %v（要求静默去重，不是报错）", err)
	}
	if inserted {
		t.Fatal("同一条剪枝写出了第二行（去重失效）")
	}
	if n := countEvents(t, db, `org_id = ? AND source = 'route'`, orgID); n != 1 {
		t.Fatalf("剪枝行数 = %d, want 1", n)
	}

	// 以下每种差异都代表**另一条**剪枝，各得一行
	others := []struct {
		name string
		e    *Event
	}{
		{"不同原因", mkPruneEvent(orgID, "trace_1", "no_node", prunePayloadNoEdge)},
		{"不同 trace", mkPruneEvent(orgID, "trace_2", "no_edge", prunePayloadNoEdge)},
		{"同 trace 同原因但证据不同", mkPruneEvent(orgID, "trace_1", "no_edge",
			`{"fromCapability":"github","port":"pull_request.closed","detail":"另一条边的证据"}`)},
	}
	for _, c := range others {
		ins, err := db.InsertPruneEvent(ctx, c.e)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !ins {
			t.Fatalf("%s: inserted=false，它应该是一行新剪枝", c.name)
		}
	}
	if n := countEvents(t, db, `org_id = ? AND source = 'route'`, orgID); n != 4 {
		t.Fatalf("剪枝行数 = %d, want 4（首条 + 三种差异）", n)
	}

	// idem_key 恒为 NULL：剪枝是观察记录，不是一次投递
	if n := countEvents(t, db, `source = 'route' AND idem_key IS NOT NULL`); n != 0 {
		t.Fatalf("%d 条剪枝带了 idem_key，want 0", n)
	}

	// 落进去的行要能被原样读回（类型与 payload 都不能被改写）
	got, err := db.ListEvents(ctx, orgID, "trace_2", "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("trace_2 的事件数 = %d, want 1", len(got))
	}
	if got[0].Source != "route" || got[0].Type != "pruned.no_edge" {
		t.Fatalf("source/type = %q/%q, want route/pruned.no_edge", got[0].Source, got[0].Type)
	}
	if got[0].IdemKey != nil {
		t.Fatalf("idem_key = %v, want NULL", *got[0].IdemKey)
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if payload["port"] != "pull_request.closed" {
		t.Fatalf("payload.port = %v, want pull_request.closed", payload["port"])
	}
}

// TestInsertPruneEventDefaultsAndValidation 校验/默认值与 InsertEvent 同一口径。
//
// 为什么必须一致：两条路径写的是同一张表。口径分叉的后果是
// 「事件写不进去的输入，剪枝却写得进去」——那等于给脏数据开了一扇侧门。
func TestInsertPruneEventDefaultsAndValidation(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)

	t.Run("非法 payload 归成 {}", func(t *testing.T) {
		e := mkPruneEvent(orgID, "trace_bad", "no_edge", `{not json`)
		if _, err := db.InsertPruneEvent(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got, err := db.ListEvents(ctx, orgID, "trace_bad", "", 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || string(got[0].Payload) != "{}" {
			t.Fatalf("payload = %s, want {}", got[0].Payload)
		}
	})

	bad := []struct {
		name string
		e    *Event
	}{
		{"nil", nil},
		{"空 org_id", &Event{TraceID: "t", Source: "route", Type: "pruned.no_edge"}},
		{"空 source", &Event{OrgID: orgID, TraceID: "t", Type: "pruned.no_edge"}},
		{"空 type", &Event{OrgID: orgID, TraceID: "t", Source: "route"}},
		{"空 trace_id", &Event{OrgID: orgID, Source: "route", Type: "pruned.no_edge"}},
	}
	for _, c := range bad {
		t.Run(c.name+" → error", func(t *testing.T) {
			if _, err := db.InsertPruneEvent(ctx, c.e); err == nil {
				t.Fatalf("%s 应该报错（不能静默写一行没有归属的剪枝）", c.name)
			}
		})
	}
	if n := countEvents(t, db, `source = 'route' AND trace_id = 't'`); n != 0 {
		t.Fatalf("非法输入写了 %d 行", n)
	}
}

// TestListEventsHidesPrunesFromStream 事件流不混剪枝，trace 视图一定看得到。
//
// 这是 #142 对外的关键语义：
//   - GET /v1/events（不带 traceId）回答「外部世界发生了什么」→ 不含剪枝；
//   - GET /v1/events?traceId=x / GET /v1/traces/{id} 回答「这条链为什么这样」→ 含剪枝；
//   - 显式 source=route → 只听调用方的（主动来看剪枝的那一类）。
func TestListEventsHidesPrunesFromStream(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)

	const traceID = "trace_mixed"

	root := &Event{
		OrgID: orgID, TraceID: traceID, Source: "github", Type: "pull_request.opened",
		Payload: json.RawMessage(`{"number":1}`),
	}
	if _, err := db.InsertEvent(ctx, root); err != nil {
		t.Fatalf("insert root event: %v", err)
	}
	prune := mkPruneEvent(orgID, traceID, "no_edge", prunePayloadNoEdge)
	if _, err := db.InsertPruneEvent(ctx, prune); err != nil {
		t.Fatalf("insert prune event: %v", err)
	}

	t.Run("事件流（无 traceId、无 source）只回普通事件", func(t *testing.T) {
		got, err := db.ListEvents(ctx, orgID, "", "", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("事件流 = %d 条, want 1（剪枝不该混进事件流）", len(got))
		}
		if got[0].Source == "route" {
			t.Fatalf("事件流里出现了剪枝行: %+v", got[0])
		}
	})

	t.Run("按 trace 查（trace 视图）回根事件 + 剪枝", func(t *testing.T) {
		got, err := db.ListEvents(ctx, orgID, traceID, "", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("trace 视图 = %d 条, want 2（含剪枝）", len(got))
		}
		var prunes int
		for _, e := range got {
			if e.Source == "route" {
				prunes++
			}
		}
		if prunes != 1 {
			t.Fatalf("trace 视图里剪枝 = %d 条, want 1", prunes)
		}
	})

	t.Run("显式 source=route 只回剪枝", func(t *testing.T) {
		got, err := db.ListEvents(ctx, orgID, "", "route", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("source=route = %d 条, want 1", len(got))
		}
		if got[0].Source != "route" || got[0].Type != "pruned.no_edge" {
			t.Fatalf("source/type = %q/%q, want route/pruned.no_edge", got[0].Source, got[0].Type)
		}
	})

	t.Run("显式 source=github 不受剪枝过滤影响", func(t *testing.T) {
		got, err := db.ListEvents(ctx, orgID, "", "github", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].Source != "github" {
			t.Fatalf("source=github = %d 条, want 1 条 github 事件", len(got))
		}
	})
}

// TestInsertEventRejectsReservedSource 业务事件不许占用保留来源名 route（#142 §9.2）。
//
// 为什么必须拦：source='route' 是剪枝记录的保留值。若业务事件用了它，
// 它会被 ListEvents 的事件流过滤掉（**静默消失**），并在 trace 视图里被当成剪枝。
// 两种错误都不可观测 —— 只能在这里（业务事件唯一的落库入口）当场拒绝。
func TestInsertEventRejectsReservedSource(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)

	e := &Event{
		OrgID: orgID, TraceID: "trace_reserved", Source: "route",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
	}
	inserted, err := db.InsertEvent(ctx, e)
	if err == nil {
		t.Fatal(`InsertEvent(source="route") 应该被拒绝，但它成功了`)
	}
	if !errors.Is(err, ErrReservedEventSource) {
		t.Fatalf("错误不可判（调用方无法区分它和随机 DB 故障）: %v", err)
	}
	if inserted {
		t.Fatal("inserted = true，保留来源名的行不该落库")
	}
	if n := countEvents(t, db, `source = 'route'`); n != 0 {
		t.Fatalf("库里出现了 %d 行 source='route'（业务事件静默进库了）", n)
	}

	// 剪枝自己走 InsertPruneEvent，**不受**这条拦截影响（它本来就该用 route）
	if _, err := db.InsertPruneEvent(ctx, mkPruneEvent(orgID, "trace_reserved", "no_edge", prunePayloadNoEdge)); err != nil {
		t.Fatalf("剪枝落库不该被保留来源名校验波及: %v", err)
	}
	if n := countEvents(t, db, `source = 'route'`); n != 1 {
		t.Fatalf("剪枝行数 = %d, want 1", n)
	}
}
