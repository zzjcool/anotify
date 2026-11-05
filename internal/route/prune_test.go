package route

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestPruneReasons 六种「未触发」原因逐个覆盖（契约 §7）。
//
// 断言两件事：
//  1. 每种 reason 都能被构造出来
//  2. pruned 记录带对了证据字段（reason / capabilityId / port / detail）
func TestPruneReasons(t *testing.T) {
	ctx := context.Background()

	t.Run("① 上游没回报不是剪枝", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(out.Pruned) != 0 {
			t.Fatalf("① must NOT produce a PrunedRecord, got %v", out.Pruned)
		}
		if len(out.Tasks) != 0 {
			t.Fatalf("① must not create tasks, got %d", len(out.Tasks))
		}
	})

	t.Run("② no_edge", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonNoEdge, "", "fix")
	})

	t.Run("③ param_schema", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		strict := f.addCap(testNS, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
		f.addEdge(testNS, review.ID, "fix", strict.ID)
		f.addNode("nd_strict", strict.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonParamSchema, strict.ID, "fix")
	})

	t.Run("④ no_node", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		orphan := f.addCap(testNS, "orphan", 1, nil, `{}`)
		f.addEdge(testNS, review.ID, "fix", orphan.ID)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonNoNode, orphan.ID, "fix")
	})

	t.Run("⑤ foreign_user", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		phone := f.addCap(testNS, "notify.phone", 1, nil,
			`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
		f.addEdge(testNS, review.ID, "fix", phone.ID)
		f.addNode("nd_phone", phone.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
			// "outsider_1" 不是本组织成员（fakeStore 约定 member* 才是）
			Params: map[string]any{"to": "outsider_1", "title": "x", "body": "y"},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonForeignUser, phone.ID, "fix")
		// 证据里要有那个用户 ID
		if !contains(out.Pruned[0].Detail, "outsider_1") {
			t.Fatalf("foreign_user detail should name the user, got %q", out.Pruned[0].Detail)
		}
	})

	t.Run("⑥ max_hops", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		fix := f.addCap(testNS, "fix", 1, []string{"review"}, `{}`)
		// fix --review--> review（成环：现在**合法**，契约 §2）
		f.addEdge(testNS, fix.ID, "review", review.ID)
		f.addNode("nd_review", review.ID, 1)
		d := New(f)
		d.MaxHops = 2

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t", FromCap: fix.ID, Out: "review",
			// depth = len(path) = 2 >= max_hops 2 → 剪 max_hops
			Path: []string{review.Name, fix.Name},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonMaxHops, "", "review")
		// 证据里要有**完整 path**：否则「它是怎么走到这么深的」答不出来。
		if !contains(out.Pruned[0].Detail, review.Name+" → "+fix.Name) {
			t.Fatalf("max_hops detail 里没有完整 path: %q", out.Pruned[0].Detail)
		}
	})

	t.Run("⑥' budget", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		target := f.addCap(testNS, "fix", 1, nil, `{}`)
		f.addEdge(testNS, review.ID, "fix", target.ID)
		f.addNode("nd_fix", target.ID, 4)
		d := New(f)
		d.TraceTaskBudget = 1
		// 同 trace 已经有 1 个任务（= 预算）→ 下一次派发被剪 budget
		if err := f.CreateTasks(ctx, []*store.Task{{
			ID: "tsk_seed", NamespaceID: testNS, TraceID: "t_budget", Status: store.TaskPending,
		}}); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t_budget", FromCap: review.ID, Out: "fix",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonBudget, "", "fix")
		if !contains(out.Pruned[0].Detail, "budget 1") {
			t.Fatalf("budget detail 里没有预算值: %q", out.Pruned[0].Detail)
		}
	})
}

// TestPruneMaxHopsCycle review→fix→review 这条环**前 N 圈放行、第 N+1 圈剪 max_hops**。
//
// 这是契约 §2 的核心语义：环是合法编排（用户拍板的 `review → fix → review`），
// 兜底靠 hop 预算而不是「名字判重」。所以这里的断言分两半：
//   - depth < maxHops 的每一圈都**必须建出任务**（环本身不剪）；
//   - depth >= maxHops 那一圈剪 max_hops，且 Detail 带完整 path。
func TestPruneMaxHopsCycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	fix := f.addCap(testNS, "fix", 1, []string{"review"}, `{}`)
	// review --fix--> fix ；fix --review--> review
	f.addEdge(testNS, review.ID, "fix", fix.ID)
	f.addEdge(testNS, fix.ID, "review", review.ID)
	f.addNode("nd_review", review.ID, 8)
	f.addNode("nd_fix", fix.ID, 8)

	d := New(f)
	const maxHops = 3
	d.MaxHops = maxHops

	// 第 1 圈：从 review 出发（path 从只有自己开始）。
	// 调用方传 path = 「从根到本任务的能力序列」，所以每绕一圈长 1。
	path := []string{review.Name}
	from := review
	port := "fix"
	for round := 1; ; round++ {
		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "trace_cycle", FromCap: from.ID, Out: port,
			Path: path,
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(path) >= maxHops {
			// 第 N+1 圈（depth 达到上限）：剪 max_hops，不再建任务。
			if len(out.Tasks) != 0 {
				t.Fatalf("round %d (depth %d): 超过 hop 预算仍建了 %d 个任务", round, len(path), len(out.Tasks))
			}
			assertPrune(t, out.Pruned, ReasonMaxHops, "", port)
			if !contains(out.Pruned[0].Detail, "max_hops 3") {
				t.Fatalf("detail 里没有预算值: %q", out.Pruned[0].Detail)
			}
			break
		}
		if len(out.Tasks) != 1 {
			t.Fatalf("round %d (depth %d): 还没到预算却建了 %d 个任务（环不该被剪）: %v",
				round, len(path), len(out.Tasks), out.Pruned)
		}
		if len(out.Pruned) != 0 {
			t.Fatalf("round %d: 不该有剪枝，got %v", round, out.Pruned)
		}
		// 下一圈：目标是刚才那一个任务，path = 它的 path（父 path + 本能力名）
		path = out.Tasks[0].Path
		if out.Tasks[0].CapabilityID == review.ID {
			from, port = review, "fix"
		} else {
			from, port = fix, "review"
		}
		if round > maxHops+2 {
			t.Fatalf("环没有被预算兜住（跑了 %d 圈）", round)
		}
	}
}

// TestForeignUser 用户维度能力：成员放行、非成员剪枝（#66 验收）。
func TestForeignUser(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"notify"}, `{}`)
	phone := f.addCap(testNS, "notify.phone", 1, nil,
		`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
	f.addEdge(testNS, review.ID, "notify", phone.ID)
	f.addNode("nd_phone", phone.ID, 4)
	d := New(f)

	// 成员：放行
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t1", FromCap: review.ID, Out: "notify",
		Params: map[string]any{"to": "member_alice"},
	})
	if err != nil {
		t.Fatalf("member dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("member should get a task, got %d tasks / %v prunes", len(out.Tasks), out.Pruned)
	}

	// 非成员：剪枝
	out, err = d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t2", FromCap: review.ID, Out: "notify",
		Params: map[string]any{"to": "stranger"},
	})
	if err != nil {
		t.Fatalf("stranger dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("stranger got %d tasks (invariant I10 broken)", len(out.Tasks))
	}
	assertPrune(t, out.Pruned, ReasonForeignUser, phone.ID, "notify")
}

// TestForeignUserEmptyTo 用户维度能力但 params.to 为空 → 剪枝（不能变成广播）。
func TestForeignUserEmptyTo(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"notify"}, `{}`)
	phone := f.addCap(testNS, "notify.phone", 1, nil, `{"type":"object"}`) // schema 没要求 to
	f.addEdge(testNS, review.ID, "notify", phone.ID)
	f.addNode("nd_phone", phone.ID, 1)
	d := New(f)

	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "notify",
		Params: map[string]any{}, // 没有 to
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatal("empty params.to created a task (would broadcast to everyone)")
	}
	assertPrune(t, out.Pruned, ReasonForeignUser, phone.ID, "notify")
}

// TestNonUserScopedCapabilitySkipsMemberCheck 非用户维度能力不查 org_members。
func TestNonUserScopedCapabilitySkipsMemberCheck(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)
	d := New(f)

	// params.to 是外人，但 fix 不是用户维度能力 → 照常派发
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "stranger"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("non-user-scoped capability was pruned by member check: %v", out.Pruned)
	}
}

// TestPruneReasonValues 原因字符串必须与契约 §7 完全一致（机器可读契约）。
func TestPruneReasonValues(t *testing.T) {
	want := map[PruneReason]string{
		ReasonNoEdge:       "no_edge",
		ReasonParamSchema:  "param_schema",
		ReasonParamMissing: "param_missing",
		ReasonParamType:    "param_type",
		ReasonNoNode:       "no_node",
		ReasonForeignUser:  "foreign_user",
		ReasonMaxHops:      "max_hops",
		ReasonBudget:       "budget",
		ReasonEventSchema:  "event_schema",
	}
	for r, s := range want {
		if string(r) != s {
			t.Errorf("reason %v != %q (contract §7 spelling)", r, s)
		}
	}
	// 一共九种（①「上游没回报」不是剪枝，不该有对应常量；
	// param_missing / param_type 是 #114 新增的取值/类型剪枝；
	// event_schema 是 P0.2 新增的入口剪枝，产生点在 source.Trigger 里；
	// max_hops / budget 取代 path（契约 §2：环靠 hop 预算兜底，另加 trace 任务预算））
	if len(want) != 9 {
		t.Fatalf("want exactly 9 prune reasons, got %d", len(want))
	}
}

// assertPrune 断言恰好一条剪枝记录，且证据字段正确。
func assertPrune(t *testing.T, pruned []PrunedRecord, reason PruneReason, capabilityID, port string) {
	t.Helper()
	if len(pruned) != 1 {
		t.Fatalf("want exactly 1 prune record, got %d: %v", len(pruned), pruned)
	}
	p := pruned[0]
	if p.Reason != reason {
		t.Fatalf("reason = %q, want %q", p.Reason, reason)
	}
	if capabilityID != "" && p.CapabilityID != capabilityID {
		t.Fatalf("capabilityId = %q, want %q", p.CapabilityID, capabilityID)
	}
	if port != "" && p.Port != port {
		t.Fatalf("port = %q, want %q", p.Port, port)
	}
	if p.Detail == "" {
		t.Fatal("prune record has empty detail (剪枝必须留痕)")
	}
}

var _ = store.TaskPending // keep store import used even if assertions change
