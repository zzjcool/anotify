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
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "",
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
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonNoEdge, "", "fix")
	})

	t.Run("③ param_schema", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		strict := f.addCap(testOrg, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
		f.addEdge(testOrg, review.ID, "fix", strict.ID)
		f.addNode("nd_strict", strict.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonParamSchema, strict.ID, "fix")
	})

	t.Run("④ no_node", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		orphan := f.addCap(testOrg, "orphan", 1, nil, `{}`)
		f.addEdge(testOrg, review.ID, "fix", orphan.ID)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonNoNode, orphan.ID, "fix")
	})

	t.Run("⑤ foreign_user", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		phone := f.addCap(testOrg, "notify.phone", 1, nil,
			`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
		f.addEdge(testOrg, review.ID, "fix", phone.ID)
		f.addNode("nd_phone", phone.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
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

	t.Run("⑥ path", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
		fix := f.addCap(testOrg, "fix", 1, []string{"review"}, `{}`)
		// fix --review--> review（成环）
		f.addEdge(testOrg, fix.ID, "review", review.ID)
		f.addNode("nd_review", review.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, TraceID: "t", FromCap: fix.ID, Out: "review",
			// review 已经在路径里
			Path: []string{review.ID, fix.ID},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		assertPrune(t, out.Pruned, ReasonPath, review.ID, "review")
		if !contains(out.Pruned[0].Detail, "cycle") {
			t.Fatalf("path detail should explain the cycle, got %q", out.Pruned[0].Detail)
		}
	})
}

// TestPrunePathCycle review→fix→review 跑两圈 → 第二圈 pruned/path（#66 验收）。
func TestPrunePathCycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	fix := f.addCap(testOrg, "fix", 1, []string{"review"}, `{}`)
	// review --fix--> fix ；fix --review--> review
	f.addEdge(testOrg, review.ID, "fix", fix.ID)
	f.addEdge(testOrg, fix.ID, "review", review.ID)
	f.addNode("nd_review", review.ID, 8)
	f.addNode("nd_fix", fix.ID, 8)

	d := New(f)

	// 第一圈：从 review 出发。
	// 注意：调用方把 path 传成「从根到本任务的能力序列」——起点能力也应在里面。
	// 这里模拟「根事件由 review 的 origin 出口触发」，所以 path 从空开始。
	out1, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, TraceID: "trace_cycle", FromCap: review.ID, Out: "fix",
		Path: []string{review.ID},
	})
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if len(out1.Tasks) != 1 {
		t.Fatalf("first round: want 1 task (fix), got %d", len(out1.Tasks))
	}
	if len(out1.Pruned) != 0 {
		t.Fatalf("first round should not prune, got %v", out1.Pruned)
	}
	pathAfterFix := out1.Tasks[0].Path // [review, fix]

	// 第二圈：fix 回报 review，目标是 review —— 已在路径里 → pruned/path
	out2, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, TraceID: "trace_cycle",
		ParentTaskID: out1.Tasks[0].ID,
		FromCap:      fix.ID, Out: "review",
		Path: pathAfterFix,
	})
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if len(out2.Tasks) != 0 {
		t.Fatalf("second round should create no task (cycle), got %d", len(out2.Tasks))
	}
	assertPrune(t, out2.Pruned, ReasonPath, review.ID, "review")
}

// TestForeignUser 用户维度能力：成员放行、非成员剪枝（#66 验收）。
func TestForeignUser(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"notify"}, `{}`)
	phone := f.addCap(testOrg, "notify.phone", 1, nil,
		`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
	f.addEdge(testOrg, review.ID, "notify", phone.ID)
	f.addNode("nd_phone", phone.ID, 4)
	d := New(f)

	// 成员：放行
	out, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, TraceID: "t1", FromCap: review.ID, Out: "notify",
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
		OrgID: testOrg, TraceID: "t2", FromCap: review.ID, Out: "notify",
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
	review := f.addCap(testOrg, "review", 1, []string{"notify"}, `{}`)
	phone := f.addCap(testOrg, "notify.phone", 1, nil, `{"type":"object"}`) // schema 没要求 to
	f.addEdge(testOrg, review.ID, "notify", phone.ID)
	f.addNode("nd_phone", phone.ID, 1)
	d := New(f)

	out, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "notify",
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
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "fix", 1, nil, `{"type":"object"}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)
	d := New(f)

	// params.to 是外人，但 fix 不是用户维度能力 → 照常派发
	out, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
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
		ReasonPath:         "path",
	}
	for r, s := range want {
		if string(r) != s {
			t.Errorf("reason %v != %q (contract §7 spelling)", r, s)
		}
	}
	// 一共七种（①「上游没回报」不是剪枝，不该有对应常量；
	// param_missing / param_type 是 #114 新增的取值/类型剪枝）
	if len(want) != 7 {
		t.Fatalf("want exactly 7 prune reasons, got %d", len(want))
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
