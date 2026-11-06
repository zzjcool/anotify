package route

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件钉死 `--to`（emit 点名下游）在**中枢侧**的两条纪律：
//
//  1. 收敛发生在**逐条校验之前**：没被点名的候选不参与本次判定，也就不留 pruned
//     记录。以前放在校验之后，同一出口的候选各有各的必填字段时，点名的那个明明
//     合法，trace 里却凭空多一条 `param_schema(别人)`，看着像这条链挂了。
//  2. 点名仍然只是**在候选里选**，不是越界：不在候选里 → ErrEmitToNotCandidate
//     （带候选清单），点名的那个自己参数不过 → 照常剪枝，不改投别人。

// TestDispatchToNarrowsBeforeValidation 点名 script.echo 时，waoo 缺参不该留下任何痕迹。
func TestDispatchToNarrowsBeforeValidation(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"out"}, `{"type":"object"}`)
	// 两个候选的必填字段**不同名**（这正是开发环境 trace_1793986152223364273 的形状）。
	echo := f.addCap(testNS, "script.echo", 1, nil,
		`{"type":"object","properties":{"message":{"type":"string"}}}`)
	waoo := f.addCap(testNS, "waoo", 1, nil,
		`{"type":"object","properties":{"waoo":{"type":"string"}},"required":["waoo"]}`)
	f.addEdgePriority(testNS, review.ID, "out", echo.ID, 0)
	f.addEdgePriority(testNS, review.ID, "out", waoo.ID, 0)
	f.addNode("nd_echo", echo.ID, 2)
	f.addNode("nd_waoo", waoo.ID, 2)

	d := New(f)
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_to_echo", FromCap: review.ID, Out: "out",
		Params: map[string]any{"message": "hello"}, To: "script.echo",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Capability != "script.echo" {
		t.Fatalf("out.Tasks = %+v, want 只有 script.echo（--to 钉住的那一个）", out.Tasks)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("没被点名的 waoo 不该被校验、更不该留 pruned：%+v", out.Pruned)
	}

	// `name@version` 同一个口径（版本对得上 → 命中）。
	out, err = d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_to_ver", FromCap: review.ID, Out: "out",
		Params: map[string]any{"waoo": "ww"}, To: "waoo@1",
	})
	if err != nil {
		t.Fatalf("dispatch waoo@1: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Capability != "waoo" || len(out.Pruned) != 0 {
		t.Fatalf("waoo@1 应当命中且无剪枝：tasks=%+v pruned=%+v", out.Tasks, out.Pruned)
	}
}

// TestDispatchToNamedTargetStillValidated 点名不等于放行：它自己参数不过照常剪枝，
// 且**不改投**另一个候选。
func TestDispatchToNamedTargetStillValidated(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"out"}, `{"type":"object"}`)
	echo := f.addCap(testNS, "script.echo", 1, nil,
		`{"type":"object","properties":{"message":{"type":"string"}}}`)
	waoo := f.addCap(testNS, "waoo", 1, nil,
		`{"type":"object","properties":{"waoo":{"type":"string"}},"required":["waoo"]}`)
	f.addEdgePriority(testNS, review.ID, "out", echo.ID, 0)
	f.addEdgePriority(testNS, review.ID, "out", waoo.ID, 0)
	f.addNode("nd_echo", echo.ID, 2)
	f.addNode("nd_waoo", waoo.ID, 2)

	out, err := New(f).Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "t_to_bad", FromCap: review.ID, Out: "out",
		Params: map[string]any{"message": "hello"}, To: "waoo",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("点名 waoo 但缺必填字段，不该建任何任务（也不该改投 script.echo）：%+v", out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("out.Pruned = %+v, want 恰好一条 param_schema", out.Pruned)
	}
	if !strings.Contains(out.Pruned[0].Detail, "waoo@1") {
		t.Errorf("剪枝 detail 应当说明是哪个能力不过：%q", out.Pruned[0].Detail)
	}
}

// TestDispatchToNotCandidate 点名不在候选里 → 明确报错并列出候选（不改投、不静默发 0 条）。
func TestDispatchToNotCandidate(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"out"}, `{"type":"object"}`)
	echo := f.addCap(testNS, "script.echo", 1, nil, `{"type":"object"}`)
	f.addEdgePriority(testNS, review.ID, "out", echo.ID, 0)
	f.addNode("nd_echo", echo.ID, 2)

	d := New(f)
	for _, to := range []string{"nope", "script.echo@9"} {
		_, err := d.Dispatch(ctx, DispatchInput{
			NamespaceID: testNS, TraceID: "t_to_nope", FromCap: review.ID, Out: "out",
			Params: map[string]any{}, To: to,
		})
		if !errors.Is(err, ErrEmitToNotCandidate) {
			t.Fatalf("--to %s 不在候选里，err = %v, want ErrEmitToNotCandidate", to, err)
		}
		if !strings.Contains(err.Error(), "script.echo@1") {
			t.Errorf("--to %s 的报错应当列出可用候选（script.echo@1）：%v", to, err)
		}
	}
}
