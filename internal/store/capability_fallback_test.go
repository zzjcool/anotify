package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// TestCapabilityOnFailureStoreRoundTrip 覆盖 #700 的持久化面：reasons / params / to
// 存进去什么读出来就是什么（store 只落 spec，但仍要证明归一化没吃掉新字段）。
func TestCapabilityOnFailureStoreRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	record := mkCap(nsID, "issue-record", 1, nil)
	record.Spec.Params = json.RawMessage(`{"type":"object","required":["repo","issue"],
		"properties":{"repo":{"type":"string"},"issue":{"type":"integer"},"outcome":{"type":"string"}}}`)
	if err := db.CreateCapability(ctx, record); err != nil {
		t.Fatalf("建 record: %v", err)
	}

	impl := mkCap(nsID, "issue-implement", 1, []string{"issue-record"})
	impl.Spec.Params = json.RawMessage(`{"type":"object","required":["repo","issue"],
		"properties":{"repo":{"type":"string"},"issue":{"type":"integer"}}}`)
	impl.Spec.Lifecycle.OnFailure = &artifact.Fallback{
		To:      "issue-record",
		Reasons: []string{protocol.ReasonTimeout, protocol.ReasonNoProgress},
		Params:  map[string]any{"outcome": "timeout", "findings": "因超时终止"},
	}
	if err := db.CreateCapability(ctx, impl); err != nil {
		t.Fatalf("建 implement: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "issue-implement", 1)
	if err != nil {
		t.Fatalf("读 implement: %v", err)
	}
	fb := got.Spec.Lifecycle.OnFailure
	if fb == nil || fb.To != "issue-record" || len(fb.Reasons) != 2 ||
		fb.Reasons[0] != protocol.ReasonTimeout || fb.Reasons[1] != protocol.ReasonNoProgress {
		t.Fatalf("onFailure 往返丢失：%+v", fb)
	}
	if fb.Params["outcome"] != "timeout" || fb.Params["findings"] != "因超时终止" {
		t.Fatalf("onFailure.params 往返丢失：%v", fb.Params)
	}
}

// TestCapabilityOnFailureParamsCheckedAtWriteTime 写入期就拦住「兜底下游 required
// 永远满足不了」的声明，而不是等真失败那天退化成一条 param_missing 剪枝。
func TestCapabilityOnFailureParamsCheckedAtWriteTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	record := mkCap(nsID, "issue-record", 1, nil)
	record.Spec.Params = json.RawMessage(`{"type":"object","required":["repo","issue"],
		"properties":{"repo":{"type":"string"},"issue":{"type":"integer"}}}`)
	if err := db.CreateCapability(ctx, record); err != nil {
		t.Fatalf("建 record: %v", err)
	}

	impl := mkCap(nsID, "issue-implement", 1, []string{"issue-record"})
	impl.Spec.Params = json.RawMessage(`{"type":"object","required":["repo"],
		"properties":{"repo":{"type":"string"}}}`)
	impl.Spec.Lifecycle.OnFailure = &artifact.Fallback{
		To:     "issue-record",
		Params: map[string]any{"outcome": "timeout"},
	}
	err := db.CreateCapability(ctx, impl)
	if !errors.Is(err, ErrCapabilityInvalid) {
		t.Fatalf("缺 issue 的兜底参数必须写入期拒绝，得到 %v", err)
	}
}
