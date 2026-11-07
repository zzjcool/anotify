package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 本文件是 Capability Spec v3 §4/§6 的 store 侧门禁（P1-b1）：
//
//	① 正文（spec）是输入、派生列是输出：一个写者、方向单向（§4）；
//	② content_hash ≡ digest（裸 hex），且能从落库的那一行重算出来；
//	③ artifacts 写穿：脚本形式写（保 deployments 外键），handler 形式**不写**（§6.1）；
//	④ migration 8 的回填：三类老能力各得其所、重跑幂等、回填后能过 Validate（§6/§6.2）。

// TestCapabilitySpecRoundTrip 存什么读什么：正文与派生列都必须一模一样。
func TestCapabilitySpecRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := &CapabilityVersion{
		NamespaceID: nsID,
		Name:        "round.trip",
		DisplayName: "往返",
		Version:     3,
		Source:      CapSourceCustom,
		// 调用方只给正文；派生列（params_schema / outputs / lifecycle / sticky）
		// 故意填成**错的值**，用来证明写入路径把它们覆盖掉了。
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"wrong":{"type":"string"}}}`),
		Outputs:      []string{"bogus"},
		Sticky:       "bogus",
		Lifecycle:    Lifecycle{MaxAttempts: 99},
		Spec: artifact.Spec{
			Params:    json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`),
			Outputs:   []string{"needs-fix", "approved"},
			Requires:  []string{"git"},
			Lifecycle: json.RawMessage(`{"retryable":true,"maxAttempts":2,"idempotent":true,"resultTtlSec":600}`),
			Sticky:    "worktree",
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/bash\necho hi\n"},
				TimeoutSec: 42,
				Env:        map[string]string{"GH_TOKEN": ""},
			},
		},
	}
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetCapability(ctx, nsID, "round.trip", 3)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 正文：整份往返（含 run.files / requires / lifecycle / sticky / handler 缺省）。
	if got.Spec.Handler != "" {
		t.Fatalf("脚本形式不该有 handler，got %q", got.Spec.Handler)
	}
	if got.Spec.Run.Entrypoint != "run.sh" || got.Spec.Run.TimeoutSec != 42 {
		t.Fatalf("run 往返丢了: %+v", got.Spec.Run)
	}
	if body := got.Spec.Run.Files["run.sh"]; !strings.HasPrefix(body, "#!") {
		t.Fatalf("run.files 往返丢了: %q", body)
	}
	if len(got.Spec.Requires) != 1 || got.Spec.Requires[0] != "git" {
		t.Fatalf("requires 往返丢了: %v", got.Spec.Requires)
	}
	// 派生列必须等于正文算出来的那份（调用方给的错值一律被覆盖）。
	if string(got.ParamsSchema) != string(got.Spec.Params) {
		t.Fatalf("params_schema 不是正文派生的: %s vs %s", got.ParamsSchema, got.Spec.Params)
	}
	if strings.Join(got.Outputs, ",") != "needs-fix,approved" {
		t.Fatalf("outputs 不是正文派生的: %v", got.Outputs)
	}
	if got.Sticky != "worktree" {
		t.Fatalf("sticky 不是正文派生的: %q", got.Sticky)
	}
	if !got.Lifecycle.Retryable || got.Lifecycle.MaxAttempts != 2 || got.Lifecycle.ResultTTLSec != 600 {
		t.Fatalf("lifecycle 不是正文派生的: %+v", got.Lifecycle)
	}
	// balance 是死字段（P1-b 起恒写 '{}'）。
	if len(got.Balance) != 0 {
		t.Fatalf("balance 应恒为空（死字段），got %+v", got.Balance)
	}
	// 列表路径必须与详情一致（同一个 scanner）。
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range list {
		if item.Name != "round.trip" {
			continue
		}
		if item.Spec.Run.Entrypoint != "run.sh" || len(item.Spec.Outputs) != 2 {
			t.Fatalf("ListCapabilities 丢了正文: %+v", item.Spec)
		}
	}
}

// TestCapabilitySpecInvalidRejected 正文不合法 → ErrCapabilityInvalid（handler 翻 400）。
//
// 特别是 impl_missing：能力永远有实现（契约 R2），空壳在写入处就拒绝，
// 而不是等到第一次部署/派发才发现。
func TestCapabilitySpecInvalidRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	cases := []struct {
		name string
		spec artifact.Spec
	}{
		{"缺实现", artifact.Spec{Outputs: []string{"done"}}},
		{
			"两种实现都给",
			artifact.Spec{
				Outputs: []string{"done"},
				Handler: artifact.HandlerNoop,
				Run:     artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/bash\n"}},
			},
		},
		{"未知 handler", artifact.Spec{Outputs: []string{"done"}, Handler: "rm-rf"}},
	}
	for _, tc := range cases {
		c := &CapabilityVersion{
			NamespaceID: nsID, Name: "invalid." + strings.ReplaceAll(tc.name, " ", "-"),
			Version: 1, Source: CapSourceCustom, Spec: tc.spec,
		}
		err := db.CreateCapability(ctx, c)
		if !errors.Is(err, ErrCapabilityInvalid) {
			t.Fatalf("%s: err = %v, want ErrCapabilityInvalid", tc.name, err)
		}
		// 一行都不许留下（append-only 表里半条数据是不可撤的）。
		if n := countRows(t, db, "capability_versions",
			"namespace_id = ? AND name = ?", nsID, c.Name); n != 0 {
			t.Fatalf("%s: 被拒却写入了 %d 行", tc.name, n)
		}
	}
}

// TestCapabilityArtifactWriteThrough artifacts 写穿：脚本形式写、handler 形式跳过。
//
// 目的是保住 deployments.artifact_id 的外键（P1 期间 artifacts 只是镜像，P2 删）；
// 而 handler 形式没有 files，给它造一行会让部署面板把它推给节点（契约 §6.1）。
func TestCapabilityArtifactWriteThrough(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	script := &CapabilityVersion{
		NamespaceID: nsID, Name: "write.through", Version: 1, Source: CapSourceCustom,
		Spec: artifact.Spec{
			Outputs: []string{"done"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/bash\necho ok\n"},
			},
		},
	}
	if err := db.CreateCapability(ctx, script); err != nil {
		t.Fatalf("create script form: %v", err)
	}
	// digest 与 content_hash 同源：artifacts 行必须能被 capability 的 digest 命中。
	row, err := db.GetArtifactByDigest(ctx, nsID, script.Digest())
	if err != nil {
		t.Fatalf("脚本形式没有写出 artifacts 行（deployments 外键会断）: %v", err)
	}
	if row.CapabilityID != script.ID {
		t.Fatalf("写穿的产物挂错了能力: %s vs %s", row.CapabilityID, script.ID)
	}
	// 写穿的是 canonical 整份产物，能解回同一份正文。
	var doc artifact.Artifact
	if err := json.Unmarshal(row.Spec, &doc); err != nil {
		t.Fatalf("artifacts.spec 不是合法产物: %v", err)
	}
	if len(doc.Spec.Run.Files) != 1 || doc.Metadata.Version != 1 {
		t.Fatalf("写穿的产物正文不对: %+v", doc)
	}

	// handler 形式：**不写** artifacts 行。
	handler := &CapabilityVersion{
		NamespaceID: nsID, Name: "notify.only", Version: 1, Source: CapSourceCustom,
		Spec: HandlerSpec(artifact.HandlerNotifyPhone, json.RawMessage(`{"type":"object"}`), nil, nil),
	}
	if err := db.CreateCapability(ctx, handler); err != nil {
		t.Fatalf("create handler form: %v", err)
	}
	if arts, err := db.ListArtifacts(ctx, nsID, handler.ID); err != nil {
		t.Fatalf("list artifacts: %v", err)
	} else if len(arts) != 0 {
		t.Fatalf("handler 形式不该有 artifacts 行（契约 §6.1），got %d 行", len(arts))
	}
}

// TestCapabilityVersionConflict (ns,name,version) 已存在且内容不同 → ErrVersionConflict。
//
// 不许静默改写版本号、也不许覆盖旧行（版本行不可变，I8）。
func TestCapabilityVersionConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	first := mkCap(nsID, "conflict.cap", 1, []string{"done"})
	if err := db.CreateCapability(ctx, withSpec(first)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 同 name@version、换 outputs（= 换内容）
	second := mkCap(nsID, "conflict.cap", 1, []string{"done", "failed"})
	err := db.CreateCapability(ctx, withSpec(second))
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	// 旧行没被改写
	got, gerr := db.GetCapability(ctx, nsID, "conflict.cap", 1)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if got.ID != first.ID || strings.Join(got.Outputs, ",") != "done" {
		t.Fatalf("冲突路径改写了旧行: %+v", got)
	}
}

// TestBuiltinHandlersAreKnownHandlers 一致性测试（契约 §6.1 第 2 条）。
//
// 运行时认领仍然走 CentralCapabilityNames() 这张硬编码名字表（P1-b1 不动派发逻辑），
// 所以这里只钉住「种子数据与正文别分叉」：每个 Central 条目都必须是 handler 形式，
// 且 handler 在白名单里。P2 把运行时切到 spec.handler 之后，这条就是那个切换的前提。
func TestBuiltinHandlersAreKnownHandlers(t *testing.T) {
	known := artifact.KnownHandlers()
	sawCentral := false
	for _, bc := range BuiltinCapabilities {
		if !bc.Central {
			continue
		}
		sawCentral = true
		spec, err := builtinSpec(bc)
		if err != nil {
			t.Fatalf("%s: 播不出正文: %v", bc.Name, err)
		}
		if spec.Handler == "" {
			t.Fatalf("%s 是 Central 条目，正文必须是 handler 形式（spec.handler 为空）", bc.Name)
		}
		if !containsString(known, spec.Handler) {
			t.Fatalf("%s 的 handler %q 不在白名单 %v 里", bc.Name, spec.Handler, known)
		}
		if bc.Artifact != nil {
			t.Fatalf("%s 是 handler 形式，不该再带产物（会给节点推一份没有 files 的产物）", bc.Name)
		}
		// 正文本身必须立得住（播种出来的东西不该等部署时才发现不合法）。
		c := &CapabilityVersion{NamespaceID: "ns_probe", Name: bc.Name, Version: bc.Version, Spec: spec}
		a := c.Artifact()
		if err := artifact.Validate(&a); err != nil {
			t.Fatalf("%s 的正文不合法: %v", bc.Name, err)
		}
	}
	if !sawCentral {
		t.Skip("BuiltinCapabilities 里没有 Central 条目")
	}
}

// TestBuiltinNotifyPhoneHasNoArtifact 契约 §6.1 的落地点：notify.phone 只有正文、没有产物行。
func TestBuiltinNotifyPhoneHasNoArtifact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db) // 注册时已播种全部内置能力

	phone, err := db.GetCapability(ctx, nsID, NotifyPhoneCapability.Name, NotifyPhoneCapability.Version)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	if phone.Spec.Handler != artifact.HandlerNotifyPhone {
		t.Fatalf("notify.phone 正文的 handler = %q，want %q", phone.Spec.Handler, artifact.HandlerNotifyPhone)
	}
	if arts, err := db.ListArtifacts(ctx, nsID, phone.ID); err != nil {
		t.Fatalf("list artifacts: %v", err)
	} else if len(arts) != 0 {
		t.Fatalf("notify.phone 不该有产物行（会让部署面板推一份没有 files 的产物），got %d 行", len(arts))
	}
	// 反证：示例能力（脚本形式）有产物，否则「有产物」这条判据本身就没验到。
	echo, err := db.GetCapability(ctx, nsID, ExampleCapability.Name, ExampleCapability.Version)
	if err != nil {
		t.Fatalf("get script.echo: %v", err)
	}
	if arts, err := db.ListArtifacts(ctx, nsID, echo.ID); err != nil {
		t.Fatalf("list echo artifacts: %v", err)
	} else if len(arts) != 1 {
		t.Fatalf("script.echo 应有 1 行产物，got %d", len(arts))
	}
}

// ---------- migration 8 的回填（契约 §6 / §6.2） ----------

// TestMigration8Backfill 模拟「上一版（migration 7）建的库」升级到本版本：
//
//	① 有产物的能力 → 正文取产物的 spec；
//	② notify.phone    → handler: notify-phone，且**不**补产物行；
//	③ 其它零产物能力 → handler: noop 占位（原 schema/outputs 保留）；
//	④ content_hash 按新口径重算（≡ digest），回填后能过 Validate；
//	⑤ 重跑 hook 幂等：行内容与 hash 都不变。
func TestMigration8Backfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v7.db")

	// 只应用 baseline + migration 2..7 —— 等价于「P1-b1 发布之前」的库（没有 spec 列）。
	v7, err := openWith(path, func(set *migrationSet) { *set = (*set)[:7] })
	if err != nil {
		t.Fatalf("open v7 db: %v", err)
	}
	if got, verr := v7.SchemaVersion(); verr != nil || got != 7 {
		t.Fatalf("v7 schema version = %d (err %v), want 7", got, verr)
	}
	nsID := newRawNamespace(t, v7)

	scriptName := "old.script"
	phoneName := NotifyPhoneCapability.Name
	orphanName := "old.orphan"
	oldIDs := map[string]string{}
	for _, name := range []string{scriptName, phoneName, orphanName} {
		id := NewCapabilityID("legacy-" + name)
		oldIDs[name] = id
		// v7 的书写方式：只有派生列，没有 spec；content_hash 还是老口径（这里随便给一个，
		// 回填必须把它换成新口径的值）。
		if _, err := v7.Exec(
			`INSERT INTO capability_versions
			   (id, namespace_id, name, display_name, version, params_schema, outputs,
			    lifecycle, balance, sticky, source, content_hash, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, nsID, name, "", 1, `{"type":"object"}`, `["done"]`,
			`{}`, `{}`, "", CapSourceCustom, "legacyhash-"+name, Now()); err != nil {
			t.Fatalf("insert legacy %s: %v", name, err)
		}
	}
	// 给 old.script 造一行产物（老库里有实现的那种能力）。
	legacyArtifact := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: scriptName, Version: 1},
		Spec: artifact.Spec{
			Params:  json.RawMessage(`{"type":"object"}`),
			Outputs: []string{"done"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/bash\necho legacy\n"},
				TimeoutSec: 15,
			},
		},
	}
	legacyDigest, err := artifact.Digest(legacyArtifact)
	if err != nil {
		t.Fatalf("digest legacy artifact: %v", err)
	}
	legacySpec, err := artifact.CanonicalJSON(legacyArtifact)
	if err != nil {
		t.Fatalf("canonical legacy artifact: %v", err)
	}
	if _, err := v7.Exec(
		`INSERT INTO artifacts (id, namespace_id, capability_id, digest, spec, created_at)
		 VALUES (?,?,?,?,?,?)`,
		NewArtifactID(), nsID, oldIDs[scriptName], legacyDigest, string(legacySpec), Now()); err != nil {
		t.Fatalf("insert legacy artifact: %v", err)
	}
	if err := v7.Close(); err != nil {
		t.Fatalf("close v7: %v", err)
	}

	// 用本版本代码重开 → migration 8（ALTER + 回填钩子）。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (migrate) db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got, verr := db.SchemaVersion(); verr != nil || got != headVersion {
		t.Fatalf("升级后 schema version = %d (err %v), want %d", got, verr, headVersion)
	}

	// ① 有产物 → 正文取产物的 spec（run 里的 files 必须原样回来）。
	script, err := db.GetCapability(ctx, nsID, scriptName, 1)
	if err != nil {
		t.Fatalf("get %s: %v", scriptName, err)
	}
	if script.Spec.Handler != "" || script.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("有产物的能力应从产物取正文，got %+v", script.Spec)
	}
	if body := script.Spec.Run.Files["run.sh"]; !strings.Contains(body, "echo legacy") {
		t.Fatalf("回填的正文丢了文件内容: %q", body)
	}
	// ④ content_hash 按新口径重算 ≡ digest
	a := script.Artifact()
	wantDigest, err := artifact.Digest(&a)
	if err != nil {
		t.Fatalf("digest row: %v", err)
	}
	if script.Digest() != wantDigest {
		t.Fatalf("回填后 content_hash 与新口径不一致: %s vs %s", script.Digest(), wantDigest)
	}

	// ② notify.phone → notify-phone 占位，且没有产物行（§6.1 的獠坑）。
	phone, err := db.GetCapability(ctx, nsID, phoneName, 1)
	if err != nil {
		t.Fatalf("get %s: %v", phoneName, err)
	}
	if phone.Spec.Handler != artifact.HandlerNotifyPhone {
		t.Fatalf("%s 回填的 handler = %q，want %q", phoneName, phone.Spec.Handler, artifact.HandlerNotifyPhone)
	}
	if arts, aerr := db.ListArtifacts(ctx, nsID, phone.ID); aerr != nil {
		t.Fatalf("list phone artifacts: %v", aerr)
	} else if len(arts) != 0 {
		t.Fatalf("%s 回填不该产出 artifacts 行，got %d", phoneName, len(arts))
	}

	// ③ 零产物 → noop 占位，原 schema/outputs 保留。
	orphan, err := db.GetCapability(ctx, nsID, orphanName, 1)
	if err != nil {
		t.Fatalf("get %s: %v", orphanName, err)
	}
	if orphan.Spec.Handler != artifact.HandlerNoop {
		t.Fatalf("%s 回填的 handler = %q，want %q", orphanName, orphan.Spec.Handler, artifact.HandlerNoop)
	}
	if strings.Join(orphan.Outputs, ",") != "done" || !strings.Contains(string(orphan.ParamsSchema), "object") {
		t.Fatalf("回填丢了原 schema/outputs: params=%s outputs=%v", orphan.ParamsSchema, orphan.Outputs)
	}
	// 回填出来的正文必须立得住（§6.2：不许等到第一次部署才发现不合法）。
	for _, c := range []*CapabilityVersion{script, phone, orphan} {
		row := c.Artifact()
		if verr := artifact.Validate(&row); verr != nil {
			t.Fatalf("%s 回填出来的正文不合法: %v", c.Name, verr)
		}
	}

	// ⑤ 重跑 hook：幂等（只碰 spec='' 的行，重跑不改变任何值）。
	before := map[string]string{}
	for _, name := range []string{scriptName, phoneName, orphanName} {
		c, gerr := db.GetCapability(ctx, nsID, name, 1)
		if gerr != nil {
			t.Fatalf("get %s: %v", name, gerr)
		}
		before[name] = c.ContentHash + "|" + string(c.Spec.Handler)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := backfillCapabilitySpec(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("rerun backfill: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rerun: %v", err)
	}
	for _, name := range []string{scriptName, phoneName, orphanName} {
		c, gerr := db.GetCapability(ctx, nsID, name, 1)
		if gerr != nil {
			t.Fatalf("get %s: %v", name, gerr)
		}
		if got := c.ContentHash + "|" + string(c.Spec.Handler); got != before[name] {
			t.Fatalf("%s 重跑回填后变了: %s → %s", name, before[name], got)
		}
	}
}

// TestMigration8BackfillRejectsInvalidLegacyRow 老库里的坏行会让迁移**指名道姓**地失败。
//
// 为什么宁可失败也不跳过：回填出来的正文必须能过 Validate（§6.2），跳过等于把一个
// 坏正文留在库里，第一次部署/派发时才炸——那时已经看不出是迁移的锅。
func TestMigration8BackfillRejectsInvalidLegacyRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7bad.db")
	v7, err := openWith(path, func(set *migrationSet) { *set = (*set)[:7] })
	if err != nil {
		t.Fatalf("open v7 db: %v", err)
	}
	nsID := newRawNamespace(t, v7)
	// 坏行：params_schema 不是 JSON 对象（老库手改的痕迹）。
	if _, err := v7.Exec(
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, params_schema, outputs,
		    lifecycle, balance, sticky, source, content_hash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		NewCapabilityID("bad"), nsID, "bad.row", "", 1, `"not-an-object"`, `[]`,
		`{}`, `{}`, "", CapSourceCustom, "h", Now()); err != nil {
		t.Fatalf("insert bad legacy row: %v", err)
	}
	if err := v7.Close(); err != nil {
		t.Fatalf("close v7: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("坏正文应该让迁移失败并指出是哪一行")
	} else if !strings.Contains(err.Error(), "bad.row") {
		t.Fatalf("迁移错误必须指名道姓（got %v）", err)
	}
}

// TestSpecBackfillHelperShapes 纯函数口径：lifecycle / outputs 的归一。
func TestSpecBackfillHelperShapes(t *testing.T) {
	if got := lifecycleRaw(""); got != nil {
		t.Fatalf("空 lifecycle 应是 nil（规范字节里不出现这个键），got %s", got)
	}
	if got := lifecycleRaw("{}"); got != nil {
		t.Fatalf("`{}` 与没有这个键等价，got %s", got)
	}
	raw := lifecycleRaw(`{"retryable":true}`)
	if string(raw) != `{"retryable":true}` {
		t.Fatalf("非空 lifecycle 应原样搬进正文，got %s", raw)
	}
	if got := decodeStringList("not json"); len(got) != 0 || got == nil {
		t.Fatalf("坏 outputs 应退回空列表（不是 nil），got %#v", got)
	}
}

// 让 sql 包在本文件里被用到（*sql.Tx 的显式类型在测试里出现过）。
var _ = sql.Tx{}
