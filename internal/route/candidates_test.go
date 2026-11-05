package route

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是契约 §3「候选选择」的验收用例。
//
// 一句话语义：一个 (能力, 端口) 的下游是**候选集合**，运行期默认 **1:1 选一个** ——
// 先滤不可达，再按 to_ref/priority 排序，取最小 priority 组，组内**轮盘**。

// TestDispatchPriorityPrefersLower p=10 与 p=20 都可达 → 永远选 p=10。
//
// 「永远」是字面意思：连着派 6 次，每次都必须是 p=10 那个。同级的轮盘只发生在
// **组内**，跨组不轮（否则优先级就只是「排序」而不是「优先」了）。
func TestDispatchPriorityPrefersLower(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	hot := f.addCap(testNS, "fix-hot", 1, nil, `{"type":"object"}`)
	cheap := f.addCap(testNS, "fix-cheap", 1, nil, `{"type":"object"}`)
	// 名字顺序刻意与优先级**反向**：fix-hot < fix-cheap（字典序），但 fix-cheap 的 p 更小。
	// 若实现先按名字取第一个（不排序 priority），这条用例会红。
	f.addEdgePriority(testNS, review.ID, "fix", hot.ID, 10)
	f.addEdgePriority(testNS, review.ID, "fix", cheap.ID, 20)
	f.addNode("nd_hot", hot.ID, 8)
	f.addNode("nd_cheap", cheap.ID, 8)

	d := New(f)
	for i := 0; i < 6; i++ {
		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t_prio", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if len(out.Tasks) != 1 {
			t.Fatalf("round %d: created %d tasks, want 1（1:1 选一个）", i+1, len(out.Tasks))
		}
		if got := out.Tasks[0].CapabilityID; got != hot.ID {
			t.Fatalf("round %d 选中了 %s，want %s（p=10 优先，且组内轮盘不会跨到 p=20）",
				i+1, out.Tasks[0].Capability, hot.Name)
		}
		if len(out.Pruned) != 0 {
			t.Fatalf("round %d: 落选**不是**剪枝（同级候选只是没轮到），got %v", i+1, out.Pruned)
		}
	}
}

// TestDispatchFallsBackWhenHigherPriorityUnreachable 高优先级不可达 → 落到下一组。
//
// 「不可达」沿用现有剪枝判定（这里用 no_node）：p=10 的目标没有节点声明它，
// 于是它被剪掉，候选集合里只剩 p=20 —— 派发照常落到 p=20，并留下 no_node 证据。
func TestDispatchFallsBackWhenHigherPriorityUnreachable(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	ghost := f.addCap(testNS, "fix-ghost", 1, nil, `{"type":"object"}`) // 没节点
	live := f.addCap(testNS, "fix-live", 1, nil, `{"type":"object"}`)
	f.addEdgePriority(testNS, review.ID, "fix", ghost.ID, 10)
	f.addEdgePriority(testNS, review.ID, "fix", live.ID, 20)
	f.addNode("nd_live", live.ID, 4)

	out, err := New(f).Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_fallback", FromCap: review.ID, Out: "fix",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != live.ID {
		t.Fatalf("out.Tasks = %+v, want 只有 fix-live（高优先级不可达时要落下一组）", out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonNoNode || out.Pruned[0].CapabilityID != ghost.ID {
		t.Fatalf("out.Pruned = %+v, want 一条 fix-ghost 的 no_node（它是候选被滤掉的证据）", out.Pruned)
	}
}

// TestDispatchAllCandidatesUnreachable 全部不可达 → 0 下游、每个候选各一条证据。
func TestDispatchAllCandidatesUnreachable(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	a := f.addCap(testNS, "fix-a", 1, nil, `{"type":"object"}`)
	b := f.addCap(testNS, "fix-b", 1, nil, `{"type":"object"}`)
	f.addEdgePriority(testNS, review.ID, "fix", a.ID, 10)
	f.addEdgePriority(testNS, review.ID, "fix", b.ID, 20)

	out, err := New(f).Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_none", FromCap: review.ID, Out: "fix",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("全部不可达却建了 %d 个任务", len(out.Tasks))
	}
	if len(out.Pruned) != 2 {
		t.Fatalf("剪枝记录 = %d，want 2（每条候选各一条 no_node）: %+v", len(out.Pruned), out.Pruned)
	}
	for _, p := range out.Pruned {
		if p.Reason != ReasonNoNode {
			t.Fatalf("reason = %q, want %q", p.Reason, ReasonNoNode)
		}
	}
}

// TestDispatchStartupRandomization 进程启动时，组内起点是**随机**的。
//
// 不随机的话，每个进程都从候选 0 开始（所有节点同时压第一个下游，
// 负载最不均衡的那种形状）。判据用统计而不是精确值：新建 20 个 Dispatcher
// （各自空的游标 = 各自一次「首次使用」），环 4 个同级候选派发一次，
// 只要出现两个不同的选中结果就算通过。
func TestDispatchStartupRandomization(t *testing.T) {
	ctx := context.Background()

	// 固定夹具：4 个同级候选，各自有节点。
	build := func() (*fakeStore, *store.CapabilityVersion, []*store.CapabilityVersion) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
		cands := make([]*store.CapabilityVersion, 0, 4)
		for _, name := range []string{"fix-a", "fix-b", "fix-c", "fix-d"} {
			c := f.addCap(testNS, name, 1, nil, `{"type":"object"}`)
			f.addEdge(testNS, review.ID, "fix", c.ID)
			f.addNode("nd_"+name, c.ID, 1)
			cands = append(cands, c)
		}
		return f, review, cands
	}

	picked := map[string]bool{}
	const tries = 20
	for i := 0; i < tries; i++ {
		f, review, _ := build()
		d := New(f) // 新 Dispatcher = 空游标 = 又一次「首次使用」
		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t_random", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if len(out.Tasks) != 1 {
			t.Fatalf("round %d: created %d tasks, want 1", i+1, len(out.Tasks))
		}
		picked[out.Tasks[0].Capability] = true
	}
	if len(picked) < 2 {
		t.Fatalf("%d 次「首次使用」都选中了同一个候选 %v：游标没有随机初始化", tries, picked)
	}
}

// TestDispatchBalanceAllFallsBackToPriority balance.mode=all 本期不实现：
// 派发侧按 priority 处理（**1:1**，不扇出），并留一次 WARN。
//
// 保存路径已经把 all 拒成 400，所以这里手工把定义改成 all（模拟手改库）：
// 派发**不能**因此变成扇出，否则「没实现的语义」就靠猜被实现了。
func TestDispatchBalanceAllFallsBackToPriority(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	// 直接改内存里的定义（fakeStore 的 ResolveRef 会原样返回它）。
	review.Balance = store.Balance{"fix": {Mode: "all"}}
	a := f.addCap(testNS, "fix-a", 1, nil, `{"type":"object"}`)
	b := f.addCap(testNS, "fix-b", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", a.ID)
	f.addEdge(testNS, review.ID, "fix", b.ID)
	f.addNode("nd_a", a.ID, 2)
	f.addNode("nd_b", b.ID, 2)

	out, err := New(f).Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_all", FromCap: review.ID, Out: "fix",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("balance=all 在派发侧仍然只能建 1 个任务（本期不实现扇出），got %d", len(out.Tasks))
	}
}

// TestEmitTwiceTwoIndependentTasks 两次 emit = 两个独立任务。
//
// 两次 emit 是两次独立派发：各自走一遍候选选择，游标 +1 让它们**可以**落到
// 不同候选（这里 2 个同级候选，所以必然落不同——轮盘的方向是确定的）。
func TestEmitTwiceTwoIndependentTasks(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	a := f.addCap(t, "fix-a", 1, nil, `{"type":"object"}`)
	b := f.addCap(t, "fix-b", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", a, "")
	f.addEdge(t, root, "fix", b, "")
	f.addNode(t, 2, a)
	f.addNode(t, 2, b)
	task := f.newTask(t, root)

	first := f.mustEmit(t, task.ID, 1, "fix", `{}`)
	if first.Routed != 1 {
		t.Fatalf("第一次 emit routed = %d, want 1（1:1 选一个）", first.Routed)
	}
	second := f.mustEmit(t, task.ID, 2, "fix", `{}`)
	if second.Routed != 1 {
		t.Fatalf("第二次 emit routed = %d, want 1", second.Routed)
	}
	if first.Duplicate || second.Duplicate {
		t.Fatalf("两次 emit 应该是两次独立派发（Duplicate = %v/%v）", first.Duplicate, second.Duplicate)
	}

	kids := f.childTasks(t, task.ID)
	if len(kids) != 2 {
		t.Fatalf("子任务 %d 条, want 2（两次 emit 各一条）", len(kids))
	}
	if kids[0].ID == kids[1].ID {
		t.Fatalf("两次 emit 落到了同一个任务 %s（不是两个独立任务）", kids[0].ID)
	}
	// 2 个同级候选 + 组内轮盘 → 两次必然落到不同候选（这就是「可以落到不同候选」）。
	if kids[0].CapabilityID == kids[1].CapabilityID {
		t.Fatalf("两次 emit 都选中了 %s：组内轮盘没有推进", kids[0].Capability)
	}
}

// TestDispatchCursorKeyedByPort 游标按 (命名空间, 能力, 端口, priority) 分槽：
// 两个**端口**各自轮盘，互不带偏。
//
// 为什么要单独钉：把 port 漏出游标键的话，`fix` 的两次派发会让 `clean` 的起点
// 跟着变——症状是「某个端口永远选不到它该选的那个」，而且只在多端口能力上出现。
func TestDispatchCursorKeyedByPort(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix", "clean"}, `{"type":"object"}`)
	fix := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	clean := f.addCap(testNS, "clean", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", fix.ID)
	f.addEdge(testNS, review.ID, "clean", clean.ID)
	f.addNode("nd_fix", fix.ID, 4)
	f.addNode("nd_clean", clean.ID, 4)

	d := New(f)
	for i := 0; i < 3; i++ {
		out, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t_ports", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("fix round %d: %v", i+1, err)
		}
		if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != fix.ID {
			t.Fatalf("fix round %d: %+v", i+1, out.Tasks)
		}
	}
	// clean 只有一个候选：它必须照样派得出去（游标没被 fix 的轮转占用）。
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_ports", FromCap: review.ID, Out: "clean",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("clean dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != clean.ID {
		t.Fatalf("clean 端口没派出去: %+v", out.Tasks)
	}
}
