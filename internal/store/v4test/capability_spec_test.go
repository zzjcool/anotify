package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// 正文（artifact.Spec）的落库/往返测试（capability.md §2.3 / §3）。
//
// 与 capability_test.go 的分工：那边测版本分配与身份（foldKey/digest），
// 这边测「一份完整正文存进去、读出来逐字段相同」以及默认值/边界。

func intPtr(v int) *int { return &v }

// fullSpecCap 造一份字段填满的能力行。
func fullSpecCap(nsID, name string) *store.CapabilityVersion {
	return &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		DisplayName: "代码审查",
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"]}`),
			Next:   []string{"fixer", "notify@2"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files: map[string]string{
					"run.sh":    "#!/bin/sh\necho hi\n",
					"helper.sh": "#!/bin/sh\necho helper\n",
				},
				Env: map[string]artifact.EnvValue{
					"SRC":     {FieldPath: "vars.SOURCE_ROOT"},
					"LITERAL": {Value: "literal-value"},
				},
				WorkDir: "$(vars.SRC)",
			},
			Requires: artifact.Requires{
				Commands: []string{"git"},
				Values:   []string{"vars.SOURCE_ROOT"},
			},
			Lifecycle: artifact.Lifecycle{
				Idempotent:   true,
				MaxAttempts:  intPtr(5),
				ResultTTLSec: intPtr(0),
				TimeoutSec:   900,
			},
			Concurrency: artifact.Concurrency{MaxPerNode: intPtr(2)},
		},
	}
}

// TestCapabilitySpecRoundTrip 一份完整正文存进去、读出来逐字段相同。
func TestCapabilitySpecRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := fullSpecCap(nsID, "review")
	createCap(t, db, c)
	got, err := db.GetCapability(ctx, nsID, "review", c.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}

	if string(got.Spec.Params) != string(c.Spec.Params) {
		t.Fatalf("params 往返 mismatch:\n got %s\nwant %s", got.Spec.Params, c.Spec.Params)
	}
	if len(got.Spec.Next) != 2 || got.Spec.Next[0] != "fixer" || got.Spec.Next[1] != "notify@2" {
		t.Fatalf("next 往返 mismatch: %v", got.Spec.Next)
	}
	if got.Spec.Run.Entrypoint != "run.sh" || len(got.Spec.Run.Files) != 2 {
		t.Fatalf("run 往返 mismatch: %+v", got.Spec.Run)
	}
	if got.Spec.Run.WorkDir != "$(vars.SRC)" {
		t.Fatalf("workDir 往返 mismatch: %q", got.Spec.Run.WorkDir)
	}
	if v, ok := got.Spec.Run.Env["SRC"]; !ok || v.FieldPath != "vars.SOURCE_ROOT" || !v.IsValueFrom() {
		t.Fatalf("env 引用往返 mismatch: %+v", got.Spec.Run.Env)
	}
	if v, ok := got.Spec.Run.Env["LITERAL"]; !ok || v.Value != "literal-value" || v.IsValueFrom() {
		t.Fatalf("env 字面量往返 mismatch: %+v", got.Spec.Run.Env)
	}
	if len(got.Spec.Requires.Commands) != 1 || got.Spec.Requires.Commands[0] != "git" {
		t.Fatalf("requires.commands 往返 mismatch: %v", got.Spec.Requires.Commands)
	}
	if len(got.Spec.Requires.Values) != 1 || got.Spec.Requires.Values[0] != "vars.SOURCE_ROOT" {
		t.Fatalf("requires.values 往返 mismatch: %v", got.Spec.Requires.Values)
	}
	if !got.Spec.Lifecycle.Idempotent || got.Spec.Lifecycle.EffectiveMaxAttempts() != 5 ||
		got.Spec.Lifecycle.EffectiveResultTTLSec() != 0 || got.Spec.Lifecycle.TimeoutSec != 900 {
		t.Fatalf("lifecycle 往返 mismatch: %+v", got.Spec.Lifecycle)
	}
	if got.Spec.Concurrency.EffectiveMaxPerNode() != 2 {
		t.Fatalf("concurrency 往返 mismatch: %+v", got.Spec.Concurrency)
	}
	if got.DisplayName != "代码审查" {
		t.Fatalf("displayName 往返 mismatch: %q", got.DisplayName)
	}

	// 列表路径读到的是同一份正文（列表/详情不能有第二套解码）。
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list capabilities: %v %v", list, err)
	}
	if list[0].Spec.Run.Env["SRC"].FieldPath != "vars.SOURCE_ROOT" {
		t.Fatalf("列表路径解码丢了 env: %+v", list[0].Spec.Run.Env)
	}
}

// TestCapabilitySpecInvalidRejected 正文不合法 → ErrCapabilityInvalid，且不落库。
func TestCapabilitySpecInvalidRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	cases := []struct {
		name string
		spec artifact.Spec
	}{
		{
			name: "entrypoint 不在 files 里",
			spec: artifact.Spec{Run: artifact.Run{
				Entrypoint: "missing.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			}},
		},
		{
			name: "env 覆盖 PATH",
			spec: artifact.Spec{Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
				Env:        map[string]artifact.EnvValue{"PATH": {Value: "/bin"}},
			}},
		},
		{
			name: "workDir 引用未声明的 params",
			spec: artifact.Spec{
				Params: []byte(`{"type":"object"}`),
				Run: artifact.Run{
					Entrypoint: "run.sh",
					Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
					WorkDir:    "/data/$(params.project)",
				},
			},
		},
		{
			name: "next 超过上限",
			spec: artifact.Spec{
				Next: tooManyRefs(artifact.MaxNext + 1),
				Run: artifact.Run{
					Entrypoint: "run.sh",
					Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &store.CapabilityVersion{NamespaceID: nsID, Name: "broken", Spec: tc.spec}
			err := db.CreateCapability(ctx, c)
			if !errors.Is(err, store.ErrCapabilityInvalid) {
				t.Fatalf("应返回 ErrCapabilityInvalid，实际 %v", err)
			}
			if n := countCapRows(t, db, nsID, "broken"); n != 0 {
				t.Fatalf("非法正文被写进了库: %d 行", n)
			}
		})
	}
}

func tooManyRefs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "target"
	}
	return out
}

// TestCapabilitySpecDefaultsExplicitEquivalent 省略默认值 ≡ 显式写默认值（同一份内容）。
func TestCapabilitySpecDefaultsExplicitEquivalent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	// 省略：maxAttempts / resultTtlSec / maxPerNode / params / next / requires 全缺。
	implicit := &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        "review",
		Spec: artifact.Spec{Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		}},
	}
	if err := db.CreateCapability(ctx, implicit); err != nil {
		t.Fatalf("create implicit: %v", err)
	}

	// 显式：把默认值一个个写出来。
	explicit := &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        "review",
		Spec: artifact.Spec{
			Params:   json.RawMessage(`{}`),
			Next:     []string{},
			Run:      implicit.Spec.Run,
			Requires: artifact.Requires{Commands: []string{}, Values: []string{}},
			Lifecycle: artifact.Lifecycle{
				Idempotent:   false,
				MaxAttempts:  intPtr(artifact.DefaultMaxAttempts),
				ResultTTLSec: intPtr(artifact.DefaultResultTTLSec),
				TimeoutSec:   0,
			},
			Concurrency: artifact.Concurrency{MaxPerNode: intPtr(artifact.DefaultMaxPerNode)},
		},
	}
	if err := db.CreateCapability(ctx, explicit); err != nil {
		t.Fatalf("create explicit: %v", err)
	}

	if implicit.FoldKey != explicit.FoldKey || implicit.Digest != explicit.Digest {
		t.Fatalf("省略与显式默认算出了不同身份:\n implicit=%+v\n explicit=%+v", implicit, explicit)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("省略/显式默认建成了两行: %d", n)
	}
}

// TestCapabilitySpecNextNotInDigest next 不参与 digest / foldKey。
func TestCapabilitySpecNextNotInDigest(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	a := createCap(t, db, testCap(nsID, "review", 0, []string{"fixer"}))
	b := createCap(t, db, testCap(nsID, "review", 0, []string{"other"}))
	if a.FoldKey != b.FoldKey || a.Digest != b.Digest {
		t.Fatalf("next 参与了身份计算: a=%+v b=%+v", a, b)
	}
}

// TestCapabilitySpecRouteChangeKeepsDigest 改线只动路由事实：digest/version 不变。
func TestCapabilitySpecRouteChangeKeepsDigest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 0, nil))
	updated, err := db.UpdateNext(ctx, nsID, "review", c.Version, []string{"fixer"}, store.ResourceVersion(c.ID, c.ResourceVersion))
	if err != nil {
		t.Fatalf("UpdateNext: %v", err)
	}
	if updated.Digest != c.Digest || updated.Version != c.Version {
		t.Fatalf("改线改了内容身份: before=%+v after=%+v", c, updated)
	}
	// 再改回空 next 也应成功（整体替换是幂等的）。
	if _, err := db.UpdateNext(ctx, nsID, "review", c.Version, nil, store.ResourceVersion(updated.ID, updated.ResourceVersion)); err != nil {
		t.Fatalf("UpdateNext to empty: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "review", c.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if len(got.Spec.Next) != 0 || got.Spec.Next == nil {
		t.Fatalf("清空 next 后 = %v（wanted 空列表）", got.Spec.Next)
	}
}

// TestCapabilityVersionConflict 显式同版不同内容 → 稳定码 version_content_immutable。
func TestCapabilityVersionConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 3, nil))

	// 改 params（执行正文的一部分）而不换 version。
	dup := testCap(nsID, "review", 3, nil)
	dup.Spec.Params = []byte(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	err := db.CreateCapability(ctx, dup)
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("应返回 ErrVersionConflict，实际 %v", err)
	}
	var conflict *store.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("错误应是 *VersionConflictError，实际 %T", err)
	}
	if conflict.Name != "review" || conflict.Version != c.Version {
		t.Fatalf("冲突信息不完整: %+v", conflict)
	}
	if conflict.Code() != "version_content_immutable" {
		t.Fatalf("Code = %q", conflict.Code())
	}
}
