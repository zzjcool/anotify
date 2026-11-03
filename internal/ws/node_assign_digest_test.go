package ws

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestAssignFrameForCarriesDigest 是 P0-a 的回归钉子：任务行上钉住的 digest
// 必须**真的**出现在 assign 帧里。
//
// 回归背景（真踩过）：AssignFrame.Digest 这个字段和节点侧的按 digest 取产物路径
// （cmd/agent/client.go:549 → cache.getByDigest）早就有了，但唯一的构造点
// assignFrameFor 从来没填过它 —— 于是节点永远回落到 index/<name>@<version>，
// 而那份缓存是允许被用户手改的（"本地变体"）。结果是"中枢以为跑的是 A、
// 节点跑的是 A′"这种分叉在两侧都无法被发现。
//
// 只测 Encode 是不够的（TestAssignFrameCarriesDigestAndTraceID 测的就是这个，
// 它手工构造帧，绕过了唯一的生产路径）——所以这里直接从 store.Task 出发。
func TestAssignFrameForCarriesDigest(t *testing.T) {
	task := &store.Task{
		ID:           "tsk_1",
		TraceID:      "trc_1",
		Capability:   "review",
		Version:      2,
		Params:       json.RawMessage(`{"diff":"x"}`),
		Digest:       "sha256:abcdef",
		LeaseUntil:   int64Ptr(100),
		CapabilityID: "cap_1",
	}

	f := assignFrameFor(task)
	if f.Digest != "sha256:abcdef" {
		t.Fatalf("assign 帧 digest = %q, want sha256:abcdef（节点靠它选产物）", f.Digest)
	}
	if f.TraceID != "trc_1" {
		t.Fatalf("assign 帧 trace_id = %q, want trc_1", f.TraceID)
	}

	// 线上 JSON 必须带出来：字段存在但序列化丢了，等于没传。
	data, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("帧不是合法 JSON: %s", data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["digest"] != "sha256:abcdef" {
		t.Errorf("线上帧 digest = %v, want sha256:abcdef", got["digest"])
	}
	if got["trace_id"] != "trc_1" {
		t.Errorf("线上帧 trace_id = %v, want trc_1", got["trace_id"])
	}
}

// TestAssignFrameForOmitsEmptyDigest 守住兼容那一半：老任务行（没有 digest）
// 和老节点（不认识这个字段）都不该看到空字段。
func TestAssignFrameForOmitsEmptyDigest(t *testing.T) {
	f := assignFrameFor(&store.Task{ID: "tsk_2", Capability: "review", Version: 1})
	if f.Digest != "" || f.TraceID != "" {
		t.Fatalf("空任务不该产出 digest/trace_id，得到 %q / %q", f.Digest, f.TraceID)
	}
	data, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["digest"]; ok {
		t.Errorf("空 digest 被序列化出来了: %s", data)
	}
	if _, ok := got["trace_id"]; ok {
		t.Errorf("空 trace_id 被序列化出来了: %s", data)
	}
}

func int64Ptr(v int64) *int64 { return &v }
