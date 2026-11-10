package store

// capability_spec_test.go 覆盖 v4 能力正文（artifact.Spec）与 store 的落库口径：
//
//	正文是**唯一**输入（params/next/run/requires/lifecycle/concurrency），
//	派生列（digest/fold_key）由 artifact 包的计算写回；正文入 JSON 列、读回逐字段一致。
//
// v3 的 handler 分发 / builtin notify.phone / migration 9 backfill 随中枢执行器与
// 旧迁移链一起退役（映射见 TEST-MAPPING.md）；v4 只有脚本一种实现形式，
// 「没有 run」不是合法形状（写入期 artifact.Validate 就拒）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// TestCapabilitySpecRoundTrip 六段正文逐字段往返（含 requires / lifecycle / concurrency）。
func TestCapabilitySpecRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	maxAttempts := 5
	resultTTL := 120
	perNode := 3
	spec := artifact.Spec{
		Params: json.RawMessage(`{"type":"object","required":["diff"]}`),
		Next:   []string{"fix", "notify@2"},
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files: map[string]string{
				"run.sh": "#!/bin/sh\necho \"$DIFF\"\n",
				"lib.sh": "#!/bin/sh\nexit 0\n",
			},
			Env: map[string]artifact.EnvValue{
				"DIFF":  {Value: "static"},
				"HOME2": {FieldPath: "homeDir"},
			},
			WorkDir: "/tmp/$(vars.WORK)",
		},
		Requires: artifact.Requires{Commands: []string{"git"}, Values: []string{"WORK"}},
		Lifecycle: artifact.Lifecycle{
			Idempotent:   true,
			MaxAttempts:  &maxAttempts,
			ResultTTLSec: &resultTTL,
			TimeoutSec:   60,
		},
		Concurrency: artifact.Concurrency{MaxPerNode: &perNode},
	}
	c := &CapabilityVersion{NamespaceID: nsID, Name: "full-spec", Version: 1, Spec: spec}
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "full-spec", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if string(got.Spec.Params) != string(spec.Params) {
		t.Fatalf("params 往返 = %s, want %s", got.Spec.Params, spec.Params)
	}
	if len(got.Spec.Next) != 2 || got.Spec.Next[1] != "notify@2" {
		t.Fatalf("next 往返 = %v", got.Spec.Next)
	}
	if len(got.Spec.Run.Files) != 2 || got.Spec.Run.Files["run.sh"] == "" {
		t.Fatalf("run.files 往返 = %v", got.Spec.Run.Files)
	}
	if got.Spec.Run.Env["DIFF"].Value != "static" || got.Spec.Run.Env["HOME2"].FieldPath != "homeDir" {
		t.Fatalf("run.env 往返 = %+v", got.Spec.Run.Env)
	}
	if got.Spec.Run.WorkDir != "/tmp/$(vars.WORK)" {
		t.Fatalf("workDir 往返 = %q", got.Spec.Run.WorkDir)
	}
	if len(got.Spec.Requires.Commands) != 1 || got.Spec.Requires.Values[0] != "WORK" {
		t.Fatalf("requires 往返 = %+v", got.Spec.Requires)
	}
	if !got.Spec.Lifecycle.Idempotent || got.Spec.Lifecycle.EffectiveMaxAttempts() != 5 ||
		got.Spec.Lifecycle.EffectiveResultTTLSec() != 120 || got.Spec.Lifecycle.EffectiveTimeoutSec() != 60 {
		t.Fatalf("lifecycle 往返 = %+v", got.Spec.Lifecycle)
	}
	if got.Spec.Concurrency.EffectiveMaxPerNode() != 3 {
		t.Fatalf("concurrency 往返 = %+v", got.Spec.Concurrency)
	}
}

// TestCapabilitySpecInvalidRejected 正文校验在**写入事务之前**发生（一行都不留）。
func TestCapabilitySpecInvalidRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	bad := &CapabilityVersion{
		NamespaceID: nsID, Name: "bad-spec", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "echo no-shebang\n"}},
		},
	}
	if err := db.CreateCapability(ctx, bad); err == nil {
		t.Fatal("缺 shebang 的正文被接受")
	}
	if _, gerr := db.GetCapability(ctx, nsID, "bad-spec", 1); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("被拒的正文留下了行: %v", gerr)
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM capability_versions WHERE namespace_id=? AND name='bad-spec'`, nsID).Scan(&n); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("被拒正文留下 %d 行", n)
	}
}

// TestCapabilityArtifactDerivedNotPersisted 产物是正文的**派生值**：不落第二张表。
func TestCapabilityArtifactDerivedNotPersisted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "derived", 1, "fix")

	art, err := db.CurrentArtifact(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if art.Digest != c.Digest {
		t.Fatalf("artifact digest = %q, want 能力行 %q", art.Digest, c.Digest)
	}
	want, err := artifact.CanonicalJSON(c.Artifact())
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if string(art.Spec) != string(want) {
		t.Fatalf("产物正文与 CanonicalJSON(能力正文) 不一致:\n got %s\nwant %s", art.Spec, want)
	}
	if tableExists(t, db, "artifacts") {
		t.Fatal("v4 不该再有 artifacts 镜像表（产物是派生值）")
	}
}

// TestCapabilityVersionConflict 同版换内容 → ErrVersionConflict；改 next/displayName 不是换内容。
func TestCapabilityVersionConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	first := v4Cap(t, db, nsID, "conflict", 1)
	same := v4Cap(t, db, nsID, "conflict", 1)
	if same.ID != first.ID {
		t.Fatalf("同内容重复提交没折叠: %s vs %s", same.ID, first.ID)
	}
	nextOnly := v4Cap(t, db, nsID, "conflict", 1, "other")
	if nextOnly.ID != first.ID {
		t.Fatalf("改 next 不该长出新行: %s vs %s", nextOnly.ID, first.ID)
	}
	stored, err := db.GetCapability(ctx, nsID, "conflict", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored.Spec.Next) != 0 {
		t.Fatalf("create-only 幂等不得隐式改 next，得到 %v", stored.Spec.Next)
	}
	changed := mkCap(nsID, "conflict", 1, nil)
	changed.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexit 7\n"
	if err := db.CreateCapability(ctx, changed); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("同版换内容应报 ErrVersionConflict，得到 %v", err)
	}
}

// TestCapabilityCreateContentAddressed 省略 version = 内容寻址：同内容复用、新内容追加版本。
func TestCapabilityCreateContentAddressed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := mkCap(nsID, "addr", 0, nil)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b := mkCap(nsID, "addr", 0, nil)
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create b (same fold): %v", err)
	}
	if b.ID != a.ID || b.Version != 1 {
		t.Fatalf("同 foldKey 应复用 @1，得到 %s@%d", b.ID, b.Version)
	}
	c := mkCap(nsID, "addr", 0, nil)
	c.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexit 3\n"
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create c (new content): %v", err)
	}
	if c.Version != 2 || c.ID == a.ID {
		t.Fatalf("新内容应分配 @2 新行，得到 %s@%d", c.ID, c.Version)
	}
	if c.FoldKey == a.FoldKey {
		t.Fatal("不同正文算出同一 foldKey")
	}
}
