package route

// P0-a「任务钉 digest」的 route 侧验收（docs/design/typed-pipeline.md §7 P0.1）。
//
// 缺口：cap_hash 只钉住能力定义（params_schema + ports），真正被执行的字节在
// artifacts 里。任务行没有记录 digest，事后就无法证明「这次跑的是哪一份内容」。
//
// 本文件钉住两件事：
//  1. 有产物 → task.Digest 等于该能力最新产物的 digest（存进去的就是要跑的那份）；
//  2. 没产物 → 空串，且**派发照常成功**（中枢执行的内置能力、没登记产物的能力都是常态，
//     不能因为「钉不住」就把整条链丢掉）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// testDigest 是内容寻址标识的形状（sha256: + 64 hex），与 artifacts.digest 一致。
const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// setupReviewFixChain 构造：review --fix--> fix，fix 由 nd_fix（2 槽位）声明。
//
// 历史：这个夹具随「三步接力派发」那套用例从 dispatch_next_test.go 搬过来——
// 那套用例已随 P1.5 收尾整条删除，夹具本身仍有别的用例在用。
func setupReviewFixChain(t *testing.T) (*fakeStore, *Dispatcher, *store.CapabilityVersion, *store.CapabilityVersion) {
	t.Helper()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 2)
	return f, New(f), review, target
}

// TestPlanPinsLatestArtifactDigest 有产物：plan 建出的任务行带上该产物的 digest。
//
// 直接断言 plan 的返回值（而不是落库后的行）：digest 是**建行时**算出来的字段，
// 在这一步就错了的话，落库只是把错值存进去。
//
// 走的是 Dispatch（emit 帧那条真路径）：它的 ①②③ 段就是 plan；接力派发那套用例
// 删除之后，这是唯一能从外面触发 plan 的入口。
func TestPlanPinsLatestArtifactDigest(t *testing.T) {
	f, d, review, target := setupReviewFixChain(t)
	f.addArtifact(testNS, target.ID, testDigest)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "trace_pin_digest", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"k": "v"}, Path: []string{review.ID},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("dispatch created %d tasks, want 1", len(out.Tasks))
	}
	tasks := out.Tasks
	if got := tasks[0].Digest; got != testDigest {
		t.Fatalf("task digest = %q, want %q（必须等于 LatestArtifact 的 digest）", got, testDigest)
	}
	// cap_hash 与 digest 是两个维度，都要在：前者钉定义，后者钉内容
	if tasks[0].CapHash != target.ContentHash {
		t.Errorf("cap_hash = %q, want %q（钉定义的那一半不能被 digest 顶掉）",
			tasks[0].CapHash, target.ContentHash)
	}
}

// TestDispatchKeepsDigestThroughCreate 全链路：Dispatch 交给 CreateTasks 的那批任务行带 digest。
//
// 与上一条的区别：这条经过 Dispatch → plan → CreateTasks，验证 digest 在**建行路径**上
// 没被丢掉（fakeStore 记录的正是交给 CreateTasks 的指针）。
// 真正“写进列里再读回来”的验证在 store 包（TestTaskDigestRoundTrip）。
//
// 候选选择（契约 §3）之后每次派发只建 1 个任务，所以这里连续派发直到三个候选
// 都出现（同级轮盘），逐条验证「每条任务的 digest 是它自己那个能力的产物」。
func TestDispatchKeepsDigestThroughCreate(t *testing.T) {
	f, d, review, target := setupFanout(t)
	// 每个目标能力各钉一份不同的 digest：值要能区分，否则「串台」看不出来。
	want := map[string]string{}
	for i, c := range target {
		digest := fmt.Sprintf("%s%02d", testDigest, i)
		f.addArtifact(testNS, c.ID, digest)
		want[c.ID] = digest
	}

	checked := 0
	for round := 0; round < 3*len(target); round++ {
		out, err := d.Dispatch(context.Background(), DispatchInput{
			NamespaceID: testNS, TraceID: "trace_digest_fanout", FromCap: review.ID, Out: "fix",
			Params: map[string]any{},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(out.Tasks) != 1 {
			t.Fatalf("round %d created %d tasks, want 1（1:1 选一个）", round+1, len(out.Tasks))
		}
		task := out.Tasks[0]
		// 每条任务的 digest 必须是**它自己那个能力**的产物（不能串台）
		if got := task.Digest; got != want[task.CapabilityID] {
			t.Errorf("task %s digest = %q, want %q（必须是本能力的产物）",
				task.Capability, got, want[task.CapabilityID])
		}
		if task.Digest == "" {
			t.Errorf("task %s digest 为空：这条用例里每个能力都登记了产物", task.Capability)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("一次都没派发成功")
	}
}

// TestDispatchNoArtifactKeepsDigestEmpty 没有产物：digest 留空串，派发**仍然成功**。
//
// 两种「没产物」都要覆盖：能力没登记过产物（常态，纯配置能力），
// 以及产物查询本身失败（DB 抖动）。两者都不该让派发失败——digest 是审计字段。
func TestDispatchNoArtifactKeepsDigestEmpty(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeStore)
	}{
		{
			name:  "没登记过产物",
			setup: func(f *fakeStore) {},
		},
		{
			name: "产物查询失败",
			setup: func(f *fakeStore) {
				f.errLatestArtifact = errors.New("simulated store outage")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, review, target := setupReviewFixChain(t)
			tc.setup(f)

			out, err := d.Dispatch(context.Background(), DispatchInput{
				NamespaceID: testNS, TraceID: "trace_no_digest", FromCap: review.ID, Out: "fix",
				Params: map[string]any{},
			})
			if err != nil {
				t.Fatalf("dispatch = %v, want nil（取不到 digest 不能让派发失败）", err)
			}
			if len(out.Tasks) != 1 {
				t.Fatalf("dispatch created %d tasks, want 1（任务必须照常建出来）", len(out.Tasks))
			}
			task := out.Tasks[0]
			if task.Digest != "" {
				t.Fatalf("task digest = %q, want \"\"（没有产物就没有可钉的字节）", task.Digest)
			}
			// 除了 digest，其它钉住字段照旧：空 digest 不是「任务残缺」
			if task.Status != store.TaskAssigned {
				t.Errorf("task status = %q, want assigned", task.Status)
			}
			if task.CapabilityID != target.ID {
				t.Errorf("task capability = %q, want %q", task.CapabilityID, target.ID)
			}
			if task.CapHash != target.ContentHash {
				t.Errorf("task cap_hash = %q, want %q", task.CapHash, target.ContentHash)
			}
		})
	}
}
