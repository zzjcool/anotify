package v4test

// dispatch_failure_visibility_test.go 是 S1-D1/D2 的**公开面**验收：
// 未解析失败 Task（downstream_missing）必须能从 GetTask / ListTasks / trace 读路径
// 看到（NULL capability ref 不隐藏、不 scan 失败），并且不伪造 version/digest/结果事实。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TestDispatchFailureTaskVisibleThroughPublicReads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "dispatch-failure")
	src := newSource(t, db, ns, "entry", store.SourceKindManual, "")

	raw, first, err := db.AcceptSourceEvent(ctx, store.SourceEventInputs{
		NamespaceID: ns, SourceID: src.ID,
		ExpectedSourceResourceVersion: src.ResourceVersion,
		TraceID:                       "trace-public-failure",
		Type:                          "entry",
		Now:                           store.Now(),
		Failures: []store.DispatchFailureInputs{
			{Capability: "ghost@3", Version: 3, Detail: "下游 ghost@3 解析不到"},
			{Capability: "ghost", Detail: "未声明版本"},
		},
	})
	if err != nil || !first {
		t.Fatalf("accept: first=%v err=%v", first, err)
	}
	var receipt store.SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if len(receipt.FailedTasks) != 2 || receipt.FailedTasks[0].TaskID == "" {
		t.Fatalf("receipt failedTasks = %+v", receipt.FailedTasks)
	}
	// wire 投影输入：显式版本保留 3；未声明版本内部是 0（wire 省略，不是公开 0）。
	if receipt.FailedTasks[0].Version != 3 || receipt.FailedTasks[1].Version != 0 {
		t.Fatalf("版本事实 = %d/%d", receipt.FailedTasks[0].Version, receipt.FailedTasks[1].Version)
	}

	for i, id := range []string{receipt.FailedTasks[0].TaskID, receipt.FailedTasks[1].TaskID} {
		got, err := db.GetTask(ctx, ns, id)
		if err != nil {
			t.Fatalf("GetTask(%d): %v", i, err)
		}
		if got.CapabilityID != "" || got.Phase != store.TaskFailed || got.Attempt != 0 ||
			got.Digest != "" || got.Error != protocol.ReasonDownstreamMissing {
			t.Fatalf("failed task %d = %+v", i, got)
		}
		if got.ExitCode != nil || got.StartedAt != nil {
			t.Fatalf("failed task %d 伪造了 Node 过程事实: %+v", i, got)
		}
	}

	list, err := db.ListTasks(ctx, ns, store.TaskFilter{TraceID: "trace-public-failure"})
	if err != nil || len(list) != 2 {
		t.Fatalf("ListTasks = %d, %v", len(list), err)
	}
	byTrace, err := db.ListTasksByTrace(ctx, ns, "trace-public-failure")
	if err != nil || len(byTrace) != 2 {
		t.Fatalf("ListTasksByTrace = %d, %v", len(byTrace), err)
	}
	if dispatchable, err := db.ListDispatchableTasks(ctx, store.Now()+60, 100); err != nil {
		t.Fatalf("list dispatchable: %v", err)
	} else {
		for _, task := range dispatchable {
			if task.ID == receipt.FailedTasks[0].TaskID || task.ID == receipt.FailedTasks[1].TaskID {
				t.Fatalf("失败任务出现在派发候选里: %s", task.ID)
			}
		}
	}
	if _, err := db.GetTask(ctx, ns, receipt.FailedTasks[0].TaskID+"_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 task 应 ErrNotFound，得到 %v", err)
	}
}
