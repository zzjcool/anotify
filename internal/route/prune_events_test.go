package route

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #142 的验收主战场：剪枝**一律留痕**。
//
// 之前 PrunedRecord 只在 HTTP 响应里出现一次，触发完就没了——
// 「这条分支为什么没跑」事后永远查不到。这里逐个钉死：
//   - 7 种原因各落恰好一行 events（source='route'、type='pruned.<reason>'）；
//   - 证据字段（fromCapability / port / targetCapability / detail）都在 payload 里；
//   - 同一条不重复写、DryRun 不写、「上游没回报出口」不写；
//   - 落库失败不打断派发（留痕是诊断，不能反过来弄坏业务）。

// pruneEventsFor 取某条 trace 的剪枝事件（fakeStore 里按 org+trace+source+type+payload 去重）。
func pruneEventsFor(f *fakeStore, traceID string) []*store.Event {
	out := make([]*store.Event, 0, len(f.pruneEvents))
	for _, e := range f.pruneEvents {
		if e.TraceID == traceID {
			out = append(out, e)
		}
	}
	return out
}

// decodePrunePayload 解析剪枝事件的 payload，顺带断言它是合法 JSON。
func decodePrunePayload(t *testing.T, e *store.Event) PruneEventPayload {
	t.Helper()
	var p PruneEventPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("payload %s 不是合法 JSON: %v", e.Payload, err)
	}
	return p
}

// TestDispatchRecordsEveryPruneReason 每一种剪枝原因各落**恰好一行** events（#142 + #114 A9）。
//
// 表驱动的理由：漏掉某一种原因是这个 bug 的典型形态（历史上 5 处 append 里
// 只要有 1 处没走留痕入口，就会出现「有的剪枝查得到、有的查不到」）。
// #114 新增的 param_missing / param_type 也必须真的落库：它们发生在**解析段**
// （参数还没进 params_schema），最容易被人当成「校验前的内部细节」而不留痕——
// 那就正好是「配完了、以为好了、触发却什么都没发生」的静默失败。
func TestDispatchRecordsEveryPruneReason(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		// setup 往 fakeStore 里铺场景，返回这次派发的输入与期望的被剪枝能力 id。
		setup      func(t *testing.T, f *fakeStore) (DispatchInput, string)
		reason     PruneReason
		wantPort   string
		wantTarget bool // true：targetCapability 必须等于被剪枝的能力 id
		// wantType 只在「想额外钉死字面量 type 字符串」时写：查询入口按 type 筛，
		// 所以新加的两种原因把 pruned.<reason> 的**字面量**也写进表里。
		wantType string
		// wantDetail 是要在 payload.detail 里按子串找到的证据（空 = 只要求非空）。
		wantDetail string
	}{
		{
			name:   "no_edge：查 edges 0 行",
			reason: ReasonNoEdge,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_no_edge",
					FromCap: review.ID, Out: "fix",
				}, ""
			},
			wantPort: "fix",
		},
		{
			name:   "param_schema：参数不过目标能力 schema",
			reason: ReasonParamSchema,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
				strict := f.addCap(testNS, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
				f.addEdge(testNS, review.ID, "fix", strict.ID)
				f.addNode("nd_strict", strict.ID, 1)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_param_schema",
					FromCap: review.ID, Out: "fix", Params: map[string]any{},
				}, strict.ID
			},
			wantPort:   "fix",
			wantTarget: true,
		},
		{
			name:   "no_node：有边有参数但没节点声明该能力",
			reason: ReasonNoNode,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
				orphan := f.addCap(testNS, "orphan", 1, nil, `{}`)
				f.addEdge(testNS, review.ID, "fix", orphan.ID)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_no_node",
					FromCap: review.ID, Out: "fix",
				}, orphan.ID
			},
			wantPort:   "fix",
			wantTarget: true,
		},
		{
			name:   "foreign_user：params.to 不是本组织成员",
			reason: ReasonForeignUser,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				review := f.addCap(testNS, "review", 1, []string{"notify"}, `{}`)
				phone := f.addCap(testNS, "notify.phone", 1, nil,
					`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
				f.addEdge(testNS, review.ID, "notify", phone.ID)
				f.addNode("nd_phone", phone.ID, 4)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_foreign_user",
					FromCap: review.ID, Out: "notify",
					Params: map[string]any{"to": "outsider_1"},
				}, phone.ID
			},
			wantPort:   "notify",
			wantTarget: true,
		},
		{
			name:   "path：目标已在路径里（环剪枝）",
			reason: ReasonPath,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
				fix := f.addCap(testNS, "fix", 1, []string{"review"}, `{}`)
				f.addEdge(testNS, fix.ID, "review", review.ID) // fix --review--> review（成环）
				f.addNode("nd_review", review.ID, 1)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_path",
					FromCap: fix.ID, Out: "review",
					Path: []string{review.ID, fix.ID},
				}, review.ID
			},
			wantPort:   "review",
			wantTarget: true,
		},
		{
			// 解析段剪枝（#114）：模板里的引用在 payload 里取不到。
			// 这一步发生在 params_schema 校验**之前**，但它同样是「链在这里断了」，
			// 同样要能在 events 表里按 pruned.param_missing 查出来。
			name:   "param_missing：模板引用的路径取不到",
			reason: ReasonParamMissing,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				loose := f.addCap(testNS, "loose", 1, nil, `{"type":"object"}`)
				f.addNode("nd_loose", loose.ID, 1)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_param_missing",
					FromCap: "github", FromIsSource: true, Out: "push",
					Targets:      []string{loose.ID},
					TargetParams: map[string]map[string]any{loose.ID: {"n": "{{pull_request.title}}"}},
					Payload:      map[string]any{}, // 这次事件没有 pull_request
				}, loose.ID
			},
			wantPort:   "push",
			wantTarget: true,
			wantType:   "pruned.param_missing",
			wantDetail: "参数 n 取不到 {{pull_request.title}}（payload 里没有这个字段）",
		},
		{
			// 解析段剪枝（#114）：取到了值但转不成 schema 要求的类型。
			name:   "param_type：取值转不成 schema 要求的类型",
			reason: ReasonParamType,
			setup: func(t *testing.T, f *fakeStore) (DispatchInput, string) {
				strict := f.addCap(testNS, "strict", 1, nil,
					`{"type":"object","properties":{"lines":{"type":"integer"}}}`)
				f.addNode("nd_strict", strict.ID, 1)
				return DispatchInput{
					OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_param_type",
					FromCap: "github", FromIsSource: true, Out: "push",
					Targets:      []string{strict.ID},
					TargetParams: map[string]map[string]any{strict.ID: {"lines": "{{lines}}"}},
					Payload:      map[string]any{"lines": "abc"},
				}, strict.ID
			},
			wantPort:   "push",
			wantTarget: true,
			wantType:   "pruned.param_type",
			wantDetail: `参数 lines 期望 integer，但求值得到 "abc"，转不了`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeStore()
			in, wantTarget := c.setup(t, f)
			d := New(f)

			out, err := d.Dispatch(ctx, in)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			// 返回值语义不变：HTTP 响应照旧看得到这条剪枝
			if len(out.Pruned) != 1 || out.Pruned[0].Reason != c.reason {
				t.Fatalf("out.Pruned = %+v, want 一条 %s", out.Pruned, c.reason)
			}

			got := pruneEventsFor(f, in.TraceID)
			if len(got) != 1 {
				t.Fatalf("%s 落了 %d 行剪枝事件, want 恰好 1 行", c.reason, len(got))
			}
			e := got[0]
			if e.OrgID != in.OrgID {
				t.Errorf("org_id = %q, want %q", e.OrgID, in.OrgID)
			}
			// events 是命名空间资源：剪枝留痕漏了 namespace 会直接写不进去
			// （store 报 ErrNamespaceRequired，剪枝静默消失）。
			if e.NamespaceID != in.NamespaceID {
				t.Errorf("namespace_id = %q, want %q", e.NamespaceID, in.NamespaceID)
			}
			if e.Source != PruneEventSource {
				t.Errorf("source = %q, want %q", e.Source, PruneEventSource)
			}
			// source 是 events 表里的「谁留的痕」，与 triggers 的来源名区分开。
			// 这里直接断**字面量**：查询/前端按这个字符串筛，常量定义改了而值没改是察觉不到的
			// （PruneEventSource 的取值本身另由 TestPruneEventTypeMapping 钉住）。
			if e.Source != "route" {
				t.Errorf("source = %q, want %q", e.Source, "route")
			}
			if want := "pruned." + string(c.reason); e.Type != want {
				t.Errorf("type = %q, want %q", e.Type, want)
			}
			if c.wantType != "" && e.Type != c.wantType {
				// 字面量再钉一遍：trace 视图按这个字符串筛剪枝行，改一下就查不到了。
				t.Errorf("type = %q, want 字面量 %q", e.Type, c.wantType)
			}
			if e.CreatedAt == 0 {
				t.Error("created_at = 0，剪枝事件没有时间戳（事后查不到是什么时候剪的）")
			}

			// payload 四字段：证据必须完整，否则查到了也答不出「为什么」
			p := decodePrunePayload(t, e)
			if p.FromCapability != in.FromCap {
				t.Errorf("payload.fromCapability = %q, want 上游标识 %q", p.FromCapability, in.FromCap)
			}
			if p.Port != c.wantPort {
				t.Errorf("payload.port = %q, want %q", p.Port, c.wantPort)
			}
			if p.Detail == "" {
				t.Error("payload.detail 为空（剪枝必须留证据）")
			}
			if c.wantDetail != "" && !strings.Contains(p.Detail, c.wantDetail) {
				t.Errorf("payload.detail = %q, want 含 %q", p.Detail, c.wantDetail)
			}
			if c.wantTarget {
				if p.TargetCapability != wantTarget {
					t.Errorf("payload.targetCapability = %q, want 被剪枝的能力 %q",
						p.TargetCapability, wantTarget)
				}
			} else if p.TargetCapability != "" {
				// no_edge：出口表 0 行，压根没有「目标能力」可言
				t.Errorf("no_edge 的 payload.targetCapability = %q, want 空", p.TargetCapability)
			}
			// payload 里不该出现空字段（camelCase + omitempty 的 json tag 生效证据）
			var raw map[string]any
			if err := json.Unmarshal(e.Payload, &raw); err != nil {
				t.Fatalf("payload 解析: %v", err)
			}
			for k, v := range raw {
				if s, ok := v.(string); ok && s == "" {
					t.Errorf("payload 里有空字段 %q 没被省略: %s", k, e.Payload)
				}
			}
		})
	}
}

// TestDispatchPruneNotDuplicated 同一 trace 同一原因派发两次 → 仍只有 1 行。
//
// 扇出里同一条边会被重复判定（重派、并行分支都会），
// 不去重的话 events 表被同一条证据刷屏，查 trace 时反而看不出发生了什么。
func TestDispatchPruneNotDuplicated(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	strict := f.addCap(testNS, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
	f.addEdge(testNS, review.ID, "fix", strict.ID)
	f.addNode("nd_strict", strict.ID, 1)
	d := New(f)

	in := DispatchInput{
		OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_dup",
		FromCap: review.ID, Out: "fix", Params: map[string]any{},
	}
	for round := 1; round <= 2; round++ {
		out, err := d.Dispatch(ctx, in)
		if err != nil {
			t.Fatalf("round %d dispatch: %v", round, err)
		}
		if len(out.Pruned) != 1 {
			t.Fatalf("round %d: out.Pruned = %d 条, want 1（返回值不该被去重影响）",
				round, len(out.Pruned))
		}
	}
	if got := pruneEventsFor(f, in.TraceID); len(got) != 1 {
		t.Fatalf("两次派发落了 %d 行剪枝事件, want 1 行", len(got))
	}
}

// TestDispatchDryRunWritesNoPruneEvent 试运行不落库（不能污染 events）。
//
// 用户改完参数会反复点「试运行」，每点一次就往 events 里塞一条假剪枝，
// 事后查 trace 时就分不清哪条是真实触发、哪条是试运行。
func TestDispatchDryRunWritesNoPruneEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("no_edge 也只在返回值里出现", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_dry_no_edge",
			FromCap: review.ID, Out: "fix", DryRun: true,
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(out.Pruned) != 1 {
			t.Fatalf("试运行应返回剪枝记录（用户要看「会剪什么」），got %d 条", len(out.Pruned))
		}
		if n := len(f.pruneEvents); n != 0 {
			t.Fatalf("试运行落了 %d 行剪枝事件, want 0", n)
		}
	})

	t.Run("param_schema 也不落库", func(t *testing.T) {
		f := newFakeStore()
		review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
		strict := f.addCap(testNS, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
		f.addEdge(testNS, review.ID, "fix", strict.ID)
		f.addNode("nd_strict", strict.ID, 1)
		d := New(f)

		out, err := d.Dispatch(ctx, DispatchInput{
			OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_dry_param",
			FromCap: review.ID, Out: "fix", Params: map[string]any{}, DryRun: true,
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
			t.Fatalf("out.Pruned = %+v, want 一条 param_schema", out.Pruned)
		}
		if n := len(f.pruneEvents); n != 0 {
			t.Fatalf("试运行落了 %d 行剪枝事件, want 0", n)
		}
	})
}

// TestDispatchNoOutWritesNoPruneEvent 「上游没回报出口」不是剪枝：一行都不许写。
//
// 它是正常结束的分支（节点 reported=0），不是「链在这里断了」。
// 记成剪枝会让每条正常收尾的链都带一条假告警。
func TestDispatchNoOutWritesNoPruneEvent(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	d := New(f)

	out, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_no_out",
		FromCap: review.ID, Out: "",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("Out==\"\" 产生了剪枝记录: %+v", out.Pruned)
	}
	if n := len(f.pruneEvents); n != 0 {
		t.Fatalf("Out==\"\" 落了 %d 行剪枝事件, want 0", n)
	}
}

// TestDispatchPruneRecordFailureDoesNotBreakDispatch 剪枝落库失败不能打断派发。
//
// 留痕是**诊断**：它挂了，业务该建的任务照建、该返回的剪枝记录照返回。
// 反过来（把落库错误的往上抛）会让「留痕失败」变成「事件派发失败」，
// 那是拿一个观察机制去弄坏主链路。
func TestDispatchPruneRecordFailureDoesNotBreakDispatch(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	f.errInsertPrune = errors.New("db is on fire")

	review := f.addCap(testNS, "review", 1, []string{"notify"}, `{}`)
	// 一条边被剪（参数不过），另一条边正常建任务：扇出部分成功
	strict := f.addCap(testNS, "strict", 1, nil, `{"type":"object","required":["diff"]}`)
	ok := f.addCap(testNS, "ok", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "notify", strict.ID)
	f.addEdge(testNS, review.ID, "notify", ok.ID)
	f.addNode("nd_strict", strict.ID, 1)
	f.addNode("nd_ok", ok.ID, 1)
	d := New(f)

	out, err := d.Dispatch(ctx, DispatchInput{
		OrgID: testOrg, NamespaceID: testNS, TraceID: "trace_insert_fail",
		FromCap: review.ID, Out: "notify", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("剪枝落库失败把派发弄挂了: %v", err)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("out.Pruned = %+v, want 一条 param_schema（返回值不受落库结果影响）", out.Pruned)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Capability != "ok" {
		t.Fatalf("out.Tasks = %+v, want 一条 ok 任务（另一条边照常派发）", out.Tasks)
	}
	if len(f.createdTasks) != 1 {
		t.Fatalf("createdTasks = %d, want 1（任务必须照建）", len(f.createdTasks))
	}
	if n := len(f.pruneEvents); n != 0 {
		t.Fatalf("注入错误后仍落了 %d 行剪枝事件（注入点没生效）", n)
	}
}

// TestPruneEventTypeMapping reason → type 的映射必须是 pruned.<reason>。
//
// 这个字符串是查询入口（trace 视图按 type 挑剪枝行），改一下就查不到了。
func TestPruneEventTypeMapping(t *testing.T) {
	reasons := []PruneReason{
		ReasonNoEdge, ReasonParamSchema, ReasonNoNode, ReasonForeignUser, ReasonPath,
		ReasonParamMissing, ReasonParamType,
	}
	for _, r := range reasons {
		want := "pruned." + string(r)
		if got := PruneEventType(r); got != want {
			t.Errorf("PruneEventType(%q) = %q, want %q", r, got, want)
		}
	}
	if PruneEventSource != "route" {
		t.Errorf("PruneEventSource = %q, want route（events 表的 source 值）", PruneEventSource)
	}
}
