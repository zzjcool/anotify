package route

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestEmitPortsAreIndependentAndShaped 逐条钉住用户提出的三条预期：
//
//	① 每个 port 有自己的载荷形状：port1(a,b)、port2(c,d)，互不合并
//	② 两次 port 不必一起 emit：可以只 emit 一个（下游只有那一条被派发）
//	③ 可以都不 emit：一个下游任务都不产生（"到此为止"是合法结局）
func TestEmitPortsAreIndependentAndShaped(t *testing.T) {
	f := newEmitFixture(t)
	// 上游声明两个出口。
	up := f.addCap(t, "up", 1, []string{"p1", "p2"}, `{"type":"object"}`)
	// 两个下游要**完全不同**的字段：p1 要 a,b —— p2 要 c,d。
	a := f.addCap(t, "down-a", 1, nil,
		`{"type":"object","required":["a","b"],"properties":{"a":{"type":"string"},"b":{"type":"string"}}}`)
	b := f.addCap(t, "down-b", 1, nil,
		`{"type":"object","required":["c","d"],"properties":{"c":{"type":"string"},"d":{"type":"string"}}}`)
	f.addEdge(t, up, "p1", a, "")
	f.addEdge(t, up, "p2", b, "")
	f.addNode(t, 4, up, a, b)

	ctx := context.Background()

	// ① 每 port 一份形状，互不掺和。
	exp, err := f.d.BuildExpects(ctx, f.ns, up.ID, up.Outputs)
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	var p1, p2 map[string]any
	_ = json.Unmarshal(exp["p1"].Schema, &p1)
	_ = json.Unmarshal(exp["p2"].Schema, &p2)
	propsOf := func(m map[string]any) map[string]bool {
		out := map[string]bool{}
		if p, ok := m["properties"].(map[string]any); ok {
			for k := range p {
				out[k] = true
			}
		}
		return out
	}
	if got := propsOf(p1); !got["a"] || !got["b"] || got["c"] || got["d"] {
		t.Fatalf("p1 的形状应当只有 a,b，实际 %v（schema=%s）", got, exp["p1"].Schema)
	}
	if got := propsOf(p2); !got["c"] || !got["d"] || got["a"] || got["b"] {
		t.Fatalf("p2 的形状应当只有 c,d，实际 %v（schema=%s）", got, exp["p2"].Schema)
	}

	// ② 只 emit p1 → 只有 downA 接到任务，downB 一条都没有。
	task := f.newTask(t, up)
	out := f.mustEmit(t, task.ID, 1, "p1", `{"a":"1","b":"2"}`)
	if out.Routed != 1 {
		t.Fatalf("只 emit p1 时 routed = %d, want 1（%+v）", out.Routed, out)
	}
	kids := f.childTasks(t, task.ID)
	if len(kids) != 1 || kids[0].CapabilityID != a.ID {
		t.Fatalf("只 emit p1 应当只派发 downA，实际 %+v", childCaps(kids))
	}

	// 再 emit p2（第二次，独立）→ 追加 downB，downA 不重复。
	out2 := f.mustEmit(t, task.ID, 2, "p2", `{"c":"3","d":"4"}`)
	if out2.Routed != 1 {
		t.Fatalf("只 emit p2 时 routed = %d, want 1", out2.Routed)
	}
	kids = f.childTasks(t, task.ID)
	if len(kids) != 2 {
		t.Fatalf("两个 port 各 emit 一次 → 2 个下游任务，实际 %v", childCaps(kids))
	}

	// ③ 一个都不 emit 的任务：没有下游（不报错、也不补空）。
	quiet := f.newTask(t, up)
	if kids := f.childTasks(t, quiet.ID); len(kids) != 0 {
		t.Fatalf("没 emit 时不该有下游任务，实际 %v", childCaps(kids))
	}
}

// TestEmitPortShapeRejectsWrongPayload：p1 的载荷缺 b（只有 a）→ 中枢第二道仍然拒，
// 并且**不留下**下游任务（节点侧第一道已按同一份 schema 拦过，这里证明第二道也一致）。
func TestEmitPortShapeRejectsWrongPayload(t *testing.T) {
	f := newEmitFixture(t)
	up := f.addCap(t, "up2", 1, []string{"p1"}, `{"type":"object"}`)
	a := f.addCap(t, "down-a2", 1, nil,
		`{"type":"object","required":["a","b"],"properties":{"a":{"type":"string"},"b":{"type":"string"}}}`)
	f.addEdge(t, up, "p1", a, "")
	f.addNode(t, 2, up, a)

	task := f.newTask(t, up)
	// 注意：中枢第二道的口径是"逐 link 校验"，缺 b 的那条 link 会被剪，
	// 但 emit 本身合法（可能 p1 还接了别的不要 b 的下游）。
	out := f.mustEmit(t, task.ID, 1, "p1", `{"a":"1"}`)
	if out.Routed != 0 {
		t.Fatalf("载荷缺 b 时不该派发 downA，routed=%d", out.Routed)
	}
	if out.Pruned != 1 || len(out.Reasons) != 1 || out.Reasons[0].Reason != "param_schema" {
		t.Fatalf("应当恰好剪掉一条 param_schema，实际 pruned=%d reasons=%+v", out.Pruned, out.Reasons)
	}
}

func childCaps(tasks []*store.Task) []string { panic("replaced") }
