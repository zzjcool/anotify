package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 入口边派发（P2.1）。
//
// 「事件 → 第一跳」现在只有一条路：查 edges 里 from_is_source=1 的入口边，
// 参数 = ResolveParams(边上的 mapping, 事件 payload) ⊎ literals。
// 这组用例钉住这条路的三件事：模板从 payload 取值、`from_port='*'` 匹配任意事件类型、
// 以及扇出里一条边参数不过**只剪那一条**。

// TestDispatchEntryEdgeResolvesPayloadPath 入口边的模板用事件 payload 的点号路径求值。
//
// 同时钉住 `from_port='*'`：入口边写 "*" 表示「这个来源的任意事件类型都走这条边」
// （源能收什么类型由外部系统决定），所以 EntryFromPort 是具体类型也照样命中。
func TestDispatchEntryEdgeResolvesPayloadPath(t *testing.T) {
	f := newFakeStore()
	notify := f.addCap(testNS, "notify", 1, nil,
		`{"type":"object","required":["title","n"],
		  "properties":{"title":{"type":"string"},"n":{"type":"integer"}}}`)
	f.addNode("nd_notify", notify.ID, 1)
	f.addEntryEdge(testNS, "github", "*", notify.ID,
		`{"title":"{{pull_request.title}}","n":"{{pull_request.number}}"}`)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t_entry",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "pull_request.opened",
		Payload: map[string]any{
			"pull_request": map[string]any{"title": "修个 bug", "number": float64(42)},
		},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("不该有剪枝: %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(out.Tasks))
	}
	got := decodeTaskParams(t, out.Tasks[0])
	if got["title"] != "修个 bug" {
		t.Errorf("title = %#v, want 修个 bug（{{路径}} 要从事件 payload 取）", got["title"])
	}
	if got["n"] != float64(42) {
		t.Errorf("n = %#v, want 42（整值引用保留原始类型）", got["n"])
	}
}

// TestDispatchEntryEdgeFanoutPartialPrune 扇出里一条入口边参数不过 → 只剪那一条。
//
// 契约 §4 ②：扇出可部分成功。这条对入口边同样成立——一次 GitHub 投递同时喂
// 两个能力时，其中一个的模板写错了字段，不能把另一个健康的目标一起卖掉。
func TestDispatchEntryEdgeFanoutPartialPrune(t *testing.T) {
	f := newFakeStore()
	good := f.addCap(testNS, "good", 1, nil,
		`{"type":"object","properties":{"title":{"type":"string"}}}`)
	bad := f.addCap(testNS, "bad", 1, nil,
		`{"type":"object","properties":{"lines":{"type":"integer"}}}`)
	f.addNode("nd_good", good.ID, 1)
	f.addNode("nd_bad", bad.ID, 1)
	f.addEntryEdge(testNS, "github", "*", good.ID, `{"title":"{{pull_request.title}}"}`)
	// 这条的引用在 payload 里没有（这次事件是 pull_request，没有 lines 字段）
	f.addEntryEdge(testNS, "github", "*", bad.ID, `{"lines":"{{lines}}"}`)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t_fanout",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "pull_request.opened",
		Payload: map[string]any{"pull_request": map[string]any{"title": "x"}},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != good.ID {
		t.Fatalf("healthy 目标应照常建任务, got %+v", out.Tasks)
	}
	if got := decodeTaskParams(t, out.Tasks[0]); got["title"] != "x" {
		t.Errorf("healthy 目标的参数不对: %#v", got)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamMissing {
		t.Fatalf("want 1 条 param_missing, got %v", out.Pruned)
	}
	if out.Pruned[0].CapabilityID != bad.ID {
		t.Errorf("剪枝挂在 %q, want %q（只剪坏的那条）", out.Pruned[0].CapabilityID, bad.ID)
	}
	// 剪枝证据里要能说出「是哪次触发、哪个引用」——排查入口边模板就靠这两样。
	if out.Pruned[0].Port != "pull_request.opened" {
		t.Errorf("剪枝的 port = %q, want 事件类型", out.Pruned[0].Port)
	}
	if !strings.Contains(out.Pruned[0].Detail, "参数 lines 取不到 {{lines}}") {
		t.Errorf("剪枝 detail 没说清是哪个引用: %q", out.Pruned[0].Detail)
	}
}

// TestDispatchEntryEdgeRequiresEntryFields 入口边的两个字段必填。
//
// 少了左端会让 EdgesFrom 拿空 key 去查、结果是一条 no_edge——把「调用方 bug」
// 伪装成「用户没配边」，是最难查的那类静默失败。所以宁可当场报错。
func TestDispatchEntryEdgeRequiresEntryFields(t *testing.T) {
	f := newFakeStore()
	d := New(f)
	cases := []struct {
		name string
		in   DispatchInput
	}{
		{"缺来源名", DispatchInput{NamespaceID: testNS, TraceID: "t", FromIsSource: true, EntryFromPort: "push"}},
		{"缺事件类型", DispatchInput{NamespaceID: testNS, TraceID: "t", FromIsSource: true, EntryFromKey: "github"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := d.Dispatch(context.Background(), c.in); err == nil {
				t.Fatal("入口边缺字段时应报错")
			}
		})
	}
}

// TestDispatchEntryEdgeBrokenMappingFailClosed 库里的 mapping 坏了（手改）→ 只剪这一条边。
//
// 与 literals 同一口径：一条坏边不该把扇出里其他健康的边一起卖掉，也不能静默忽略
// （静默忽略等于下游拿到少了一半参数的任务）。
func TestDispatchEntryEdgeBrokenMappingFailClosed(t *testing.T) {
	f := newFakeStore()
	broken := f.addCap(testNS, "broken", 1, nil, `{"type":"object"}`)
	healthy := f.addCap(testNS, "healthy", 1, nil, `{"type":"object"}`)
	f.addNode("nd_broken", broken.ID, 1)
	f.addNode("nd_healthy", healthy.ID, 1)
	f.addEntryEdge(testNS, "github", "*", broken.ID, `{not json`)
	f.addEntryEdge(testNS, "github", "*", healthy.ID, `{"n":1}`)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "push",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != healthy.ID {
		t.Fatalf("健康的边应照常建任务, got %+v", out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("want 1 条 param_schema 剪枝, got %v", out.Pruned)
	}
	if !strings.Contains(out.Pruned[0].Detail, "edge mapping") {
		t.Errorf("剪枝 detail 没标出是 mapping 坏了: %q", out.Pruned[0].Detail)
	}
}

// TestDispatchEntryEdgeEmptyMappingIsIdentity 空模板 = **恒等**：事件原样就是目标参数。
//
// 为什么是这个语义（而不是「空模板 = 不传参」）：
//   - P2.1 之前 /v1/events 一直是这样：入口边没有 mapping 这一列，payload 直接当参数。
//   - P2.1 的迁移把 source_targets.params 搬成入口边 mapping，而多数源目标根本没填过参数
//     （params='{}'）。若空模板等于「不传参」，升级那一刻这些源会全部静默停摆
//     （payload 明明有值，下游却报 missing required）——真实踩到过（3 个 e2e 套件红）。
//   - 语义上也对：入口边的上游是外部系统，它不会替你裁剪字段；「不裁剪」就是恒等。
func TestDispatchEntryEdgeEmptyMappingIsIdentity(t *testing.T) {
	f := newFakeStore()
	target := f.addCap(testNS, "notify", 1, nil,
		`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
	f.addNode("nd_notify", target.ID, 1)
	f.addEntryEdge(testNS, "github", "*", target.ID, ``) // 空 mapping

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "push",
		Payload: map[string]any{"to": "u_1", "extra": "ignored"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("空模板应当把 payload 当参数, got tasks=%d pruned=%v", len(out.Tasks), out.Pruned)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Tasks[0].Params, &got); err != nil {
		t.Fatalf("params: %v", err)
	}
	if got["to"] != "u_1" {
		t.Fatalf("params.to = %v, want u_1（恒等）", got["to"])
	}
}

// TestDispatchEntryEdgeEmptyMappingStillAppliesLiterals 恒等之后**仍然**叠字面量，且字面量优先。
func TestDispatchEntryEdgeEmptyMappingStillAppliesLiterals(t *testing.T) {
	f := newFakeStore()
	target := f.addCap(testNS, "notify", 1, nil,
		`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
	f.addNode("nd_notify", target.ID, 1)
	f.addEntryEdgeLiterals(testNS, "github", "*", target.ID, ``, `{"to":"u_lit"}`)

	out, err := New(f).Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t",
		FromIsSource: true, EntryFromKey: "github", EntryFromPort: "push",
		Payload: map[string]any{"to": "u_payload"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d (pruned=%v)", len(out.Tasks), out.Pruned)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Tasks[0].Params, &got); err != nil {
		t.Fatalf("params: %v", err)
	}
	if got["to"] != "u_lit" {
		t.Fatalf("params.to = %v, want u_lit（字面量优先）", got["to"])
	}
}

// 保证 fake 的入口边 helper 与 store 的类型是同一份（改了 LinkTarget 这里会红）。
var _ = store.LinkTarget{ToRef: "", Mapping: nil, Literals: nil}
