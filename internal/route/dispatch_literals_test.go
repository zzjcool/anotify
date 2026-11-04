package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------- 字面量覆盖（P0-c，设计 §3.3 / §7 P0.3） ----------

// literalsTargetSchema 是下游能力的 params_schema：to 必填，env 可选。
const literalsTargetSchema = `{
	"type":"object",
	"required":["to"],
	"properties":{"to":{"type":"string"},"env":{"type":"string"}}
}`

// TestDispatchLiteralsOverride 字面量优先：data 里有同名键时字面量赢，其余键照常留着。
func TestDispatchLiteralsOverride(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	notify := f.addCap(testNS, "notify", 1, nil, literalsTargetSchema)
	// 边上的常量：收件人写死在配置里（改收件人不该重新部署节点）
	f.addEdgeLiterals(testNS, review.ID, "fix", notify.ID, `{"to":"u_123","env":"prod"}`)
	f.addNode("nd_notify", notify.ID, 1)

	in := DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "u_from_data", "env": "staging", "job": "j1"},
	}
	out, err := New(f).Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("unexpected prunes: %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("created %d tasks, want 1", len(out.Tasks))
	}
	got := decodeParams(t, out.Tasks[0].Params)
	if got["to"] != "u_123" {
		t.Errorf("params.to = %v, want u_123（字面量优先）", got["to"])
	}
	if got["env"] != "prod" {
		t.Errorf("params.env = %v, want prod（字面量优先）", got["env"])
	}
	if got["job"] != "j1" {
		t.Errorf("params.job = %v, want j1（data 里其他键不能被字面量顶掉）", got["job"])
	}
	// 入参本身不能被就地改写（同一份 Params 会被扇出里的其他边复用）
	if in.Params["to"] != "u_from_data" || in.Params["env"] != "staging" {
		t.Errorf("DispatchInput.Params 被就地改写了: %v", in.Params)
	}
}

// TestDispatchLiteralsFanoutIsolation 扇出里只有带字面量的那条边受影响。
//
// 这是「合并必须产新 map」的回归用例：就地改 targetParams 会让 A 边的常量漏进 B 边，
// 而两条边在库里长得几乎一样，事后几乎查不出来。
func TestDispatchLiteralsFanoutIsolation(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	withLit := f.addCap(testNS, "speak", 1, nil, literalsTargetSchema)
	noLit := f.addCap(testNS, "speak2", 1, nil, literalsTargetSchema)
	f.addEdgeLiterals(testNS, review.ID, "fix", withLit.ID, `{"to":"u_literal"}`)
	f.addEdge(testNS, review.ID, "fix", noLit.ID) // 没有字面量
	f.addNode("nd_speak", withLit.ID, 1)
	f.addNode("nd_speak2", noLit.ID, 1)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "u_data"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 2 {
		t.Fatalf("created %d tasks, want 2", len(out.Tasks))
	}
	for _, task := range out.Tasks {
		got := decodeParams(t, task.Params)
		switch task.CapabilityID {
		case withLit.ID:
			if got["to"] != "u_literal" {
				t.Errorf("带字面量的目标 params.to = %v, want u_literal", got["to"])
			}
		case noLit.ID:
			if got["to"] != "u_data" {
				t.Errorf("没字面量的目标 params.to = %v, want u_data（不该被别的边的常量污染）", got["to"])
			}
		}
	}
}

// TestDispatchNoLiteralsUnchanged 没有字面量时行为与今天完全一致：
// params 原样（deep equal 到派发前的 data），不新增任何剪枝。
func TestDispatchNoLiteralsUnchanged(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	notify := f.addCap(testNS, "notify", 1, nil, literalsTargetSchema)
	f.addEdge(testNS, review.ID, "fix", notify.ID) // literals 缺省 = {}（老数据的形状）
	f.addNode("nd_notify", notify.ID, 1)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "u_data", "env": "staging"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("unexpected prunes: %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("created %d tasks, want 1", len(out.Tasks))
	}
	got := decodeParams(t, out.Tasks[0].Params)
	want := map[string]any{"to": "u_data", "env": "staging"}
	if len(got) != len(want) {
		t.Fatalf("params = %v, want %v（字面量为空时必须原样透传）", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("params.%s = %v, want %v", k, got[k], v)
		}
	}
}

// TestDispatchLiteralTypeCheckedAtDispatch 第二道闸：字面量类型写错时**派发期**也要被挡住。
//
// 配置期（store.CreateEdge）已经拒过一次；这里绕过它（模拟手改库 / 老数据），
// 断言派发仍然按目标的 params_schema 剪枝，而不是把错类型塞给下游。
func TestDispatchLiteralTypeCheckedAtDispatch(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	notify := f.addCap(testNS, "notify", 1, nil, literalsTargetSchema)
	f.addEdgeLiterals(testNS, review.ID, "fix", notify.ID, `{"to":123}`) // string 字段给了 number
	f.addNode("nd_notify", notify.ID, 1)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "u_ok"}, // 合法的 data 被非法字面量覆盖 → 必须剪
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("类型错的字面量产出了 %d 个任务", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("want 1 param_schema prune, got %v", out.Pruned)
	}
	if !strings.Contains(out.Pruned[0].Detail, "params.to") {
		t.Errorf("剪枝原因没说清是哪个字段: %q", out.Pruned[0].Detail)
	}
}

// TestDispatchBrokenLiteralsFailClosed 库里 literals 是坏 JSON（手改过）→ 只剪这一条边。
//
// 不整次派发失败：一条坏边不该把扇出里其他健康的边一起卖掉；
// 也不能静默忽略（那等于下游拿到少了一半参数的任务）。
func TestDispatchBrokenLiteralsFailClosed(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	broken := f.addCap(testNS, "broken", 1, nil, literalsTargetSchema)
	healthy := f.addCap(testNS, "healthy", 1, nil, literalsTargetSchema)
	f.addEdgeLiterals(testNS, review.ID, "fix", broken.ID, `{not json`)
	f.addEdge(testNS, review.ID, "fix", healthy.ID)
	f.addNode("nd_broken", broken.ID, 1)
	f.addNode("nd_healthy", healthy.ID, 1)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"to": "u_data"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != healthy.ID {
		t.Fatalf("want 1 task for the healthy edge, got %d tasks", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("want 1 param_schema prune, got %v", out.Pruned)
	}
	if !strings.Contains(out.Pruned[0].Detail, "edge literals") {
		t.Errorf("剪枝 detail 没标出是字面量坏了: %q", out.Pruned[0].Detail)
	}
}

// TestDispatchEntryEdgeLiteralsOverride 入口边：模板求值之后再叠字面量，**字面量优先**。
//
// 这条钉住两件事（P2.1 收敛后的唯一路径）：
//  1. 入口边的参数来自边上的 mapping（`{{x}}` 用事件 payload 求值），而不是整份 payload；
//  2. 求值结果仍然要被 literals 覆盖——位置与加这一列之前一致，
//     都在「拿到目标参数」与「validateParams」之间（设计 §3.3）。
func TestDispatchEntryEdgeLiteralsOverride(t *testing.T) {
	f := newFakeStore()
	notify := f.addCap(testNS, "notify", 1, nil, literalsTargetSchema)
	f.addNode("nd_notify", notify.ID, 1)
	// 入口边：模板给出收件人，边上常量再把它盖掉（改收件人不该动模板）。
	f.addEntryEdgeLiterals(testNS, "github", "pull_request.opened", notify.ID,
		`{"to":"u_from_template"}`, `{"to":"u_from_literal","env":"prod"}`)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "pull_request.opened",
		Payload: map[string]any{"pr": map[string]any{"user": "u_from_payload"}},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("unexpected prunes: %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("created %d tasks, want 1", len(out.Tasks))
	}
	got := decodeParams(t, out.Tasks[0].Params)
	if got["to"] != "u_from_literal" {
		t.Errorf("params.to = %v, want u_from_literal（字面量优先于模板）", got["to"])
	}
	if got["env"] != "prod" {
		t.Errorf("params.env = %v, want prod（字面量要进最终参数）", got["env"])
	}
}

// decodeParams 解出任务行上的 params（JSON 对象）。
func decodeParams(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode params %q: %v", raw, err)
	}
	return out
}
