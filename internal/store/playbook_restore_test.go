package store

// 按快照重装（#561）的领域验收：版本不漂移、冲突整包 0 写（#535 的 6 表指纹）、源缺按钉住包体重建
// （#632：只在**重算不可能**时才 409）、审计一行。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// restoreBundle 是「1 能力 + 1 源」的包；script 区分版本正文。能力名**静态**（两个 release 才能长出同名新版本），
// 源名按 values 区分（同一个源不能属于两个 release）。
func restoreBundle(slug, script string) map[string]string {
	return restoreBundleV(slug, script, 1)
}

// restoreBundleV 是带包版本号的 restoreBundle：真正的版本升级必须是版本号也变（同一个版本号写不同正文
// 是**覆盖**那一版，见 recordPlaybookVersionOn——这时按 vN 复原不该拿新正文当旧正文用）。
func restoreBundleV(slug, script string, version int) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbEntry(slug, version, []string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"}),
		"capabilities/review.yaml": pbCapabilityDoc("demo-review", nil, script),
		"sources/on-push.yaml":     pbSourceDoc("{{values.project}}-on-push", []string{"demo-review"}),
	}
}

// restoreTwoCapBundle 是「2 能力、无源」的包：冲突用例需要「一个冲突 + 一个本该新建」——只有存在
// 本该新建的对象，0 写才有可证伪的靶子（「先写一半再报错」的变异会改指纹）。演示名 a / b 都不带模板。
func restoreTwoCapBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":       pbEntry(slug, 1, []string{"capabilities/a.yaml", "capabilities/b.yaml"}, nil),
		"capabilities/a.yaml": pbCapabilityDoc("demo-a", nil, "#!/bin/sh\necho a1\n"),
		"capabilities/b.yaml": pbCapabilityDoc("demo-b", nil, "#!/bin/sh\necho b1\n"),
	}
}

// restoreRecreateSource 手工把卸载时被物理删掉的源补回来（供“同名源已存在 → 复用”这条分支的用例）。
func restoreRecreateSource(t *testing.T, db *DB, nsID, name string) {
	t.Helper()
	if err := db.CreateSource(context.Background(), &Source{NamespaceID: nsID, Name: name, Kind: "webhook",
		Config: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("补建源 %s: %v", name, err)
	}
}

// restoreFingerprint 是 6 张表的逐行指纹（与 e2e / #535 同一组表）：任何一行被写都会变。
func restoreFingerprint(t *testing.T, db *DB) string {
	t.Helper()
	queries := []string{
		`SELECT * FROM capability_versions ORDER BY id`,
		`SELECT * FROM sources ORDER BY id`,
		`SELECT * FROM edges ORDER BY namespace_id, from_is_source, from_ref, to_ref`,
		`SELECT * FROM playbook_installs ORDER BY id`,
		`SELECT * FROM playbook_versions ORDER BY playbook_id, version`,
		`SELECT * FROM playbook_install_requires ORDER BY namespace_id, install_id, capability`,
	}
	var b strings.Builder
	for _, q := range queries {
		b.WriteString(restoreDump(t, db, q))
	}
	return b.String()
}

func restoreDump(t *testing.T, db *DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("fingerprint query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("fingerprint columns: %v", err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("fingerprint scan: %v", err)
		}
		for i, v := range vals {
			if raw, ok := v.(sql.RawBytes); ok {
				vals[i] = string(raw)
			}
		}
		b.WriteString(toJSON(t, vals))
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("fingerprint rows: %v", err)
	}
	return b.String()
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("fingerprint marshal: %v", err)
	}
	return string(raw)
}

// restoreAuditRows 数审计行：409 时它也必须是 0 增。
func restoreAuditRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM playbook_install_audit`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// installForRestore 装一次并返回回执（失败即 Fatal，测试只关心复原）。
func installForRestore(t *testing.T, db *DB, nsID, slug, release string, values map[string]any) *playbook.Receipt {
	t.Helper()
	rcpt, err := db.InstallPlaybook(context.Background(), nsID, slug, release, values, false)
	if err != nil {
		t.Fatalf("install %s/%s: %v", slug, release, err)
	}
	return rcpt
}

// 验收 1：装 v1 → 包升 v2 → 装 v2 → 两个实例都卸载 → 复原 v1 那组：重建的是 **pinned 的 v1**（版本与正文）。
func TestRestoreUsesPinnedVersionNotLatest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-version"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))

	first := installForRestore(t, db, nsID, slug, "alpha", pbValues("alpha"))
	if len(first.Created.Pinned) == 0 || first.Created.Pinned[0].Version != 1 {
		t.Fatalf("第一次安装的快照 = %+v, want demo-review@1", first.Created.Pinned)
	}
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug),
		restoreBundleV(slug, "#!/bin/sh\necho v2\n", 2), ""); err != nil {
		t.Fatalf("升级包体: %v", err)
	}
	second := installForRestore(t, db, nsID, slug, "beta", pbValues("beta"))
	if second.Created.Pinned[0].Version != 2 {
		t.Fatalf("第二次安装的快照 = %+v, want demo-review@2", second.Created.Pinned)
	}
	if second.Playbook.Version != 2 {
		t.Fatalf("第二次安装钉住的包版本 = %d, want 2", second.Playbook.Version)
	}
	for _, rel := range []string{"alpha", "beta"} {
		if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, rel, ""); err != nil {
			t.Fatalf("卸载 %s: %v", rel, err)
		}
	}
	var live int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-review' AND deleted_at IS NULL`, nsID).Scan(&live); err != nil {
		t.Fatalf("数可见能力行: %v", err)
	}
	if live != 0 {
		t.Fatalf("卸载后应有 0 条可见能力行, got %d", live)
	}

	rcpt, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", first.ValuesDigest, "usr_actor")
	if err != nil {
		t.Fatalf("复原 alpha: %v", err)
	}
	if rcpt.RestoredFrom != first.Install.ID {
		t.Fatalf("restoredFrom = %q, want %q", rcpt.RestoredFrom, first.Install.ID)
	}
	if rcpt.Playbook.Version != first.Playbook.Version {
		t.Fatalf("回执 playbook.version = %d, want 来源的 %d（包当前已是 %d）",
			rcpt.Playbook.Version, first.Playbook.Version, 2)
	}
	if rcpt.Playbook.FilesDigest != first.Playbook.FilesDigest {
		t.Fatalf("回执 playbook.filesDigest = %q, want 来源的 %q（钉住的是那一份正文）",
			rcpt.Playbook.FilesDigest, first.Playbook.FilesDigest)
	}
	if got := rcpt.Created.Capabilities; len(got) != 1 || got[0] != "demo-review@1" {
		t.Fatalf("created.capabilities = %v, want [demo-review@1]（按 pinned 版本，不是最新的 v2）", got)
	}
	var script string
	if err := db.QueryRow(`SELECT spec FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-review' AND version = 1 AND deleted_at IS NULL`,
		nsID).Scan(&script); err != nil {
		t.Fatalf("读复原出来的 v1 正文: %v", err)
	}
	if !strings.Contains(script, "echo v1") {
		t.Fatalf("复原出来的正文不是 v1: %s", script)
	}
	var v2 int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-review' AND version = 2 AND deleted_at IS NULL`,
		nsID).Scan(&v2); err != nil {
		t.Fatalf("数 v2 可见行: %v", err)
	}
	if v2 != 0 {
		t.Fatalf("复原 alpha 不该让 v2 复活（那是 beta 的快照）, got %d 行", v2)
	}
	// 卸载会物理删源，复原必须把它重建出来（钉住的是 v1 的包体，不是当前的 v2）——
	// 重建走的是「钉住包体 × 回执 values」的重算路径，所以记在 created，不是 skipped。
	if rcpt.Created.Sources == nil || len(rcpt.Created.Sources) != 1 || rcpt.Created.Sources[0] != "alpha-on-push" {
		t.Fatalf("created.sources = %v, want [alpha-on-push]（重建，不是复用）", rcpt.Created.Sources)
	}
	if len(rcpt.Skipped.Sources) != 0 {
		t.Fatalf("skipped.sources = %v, want 空（源是重建的）", rcpt.Skipped.Sources)
	}
}

// TestRestoreAlreadyInstalled 是验收 2：同一组参数仍在生效 → 409 already_installed，0 写。
func TestRestoreAlreadyInstalled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-live"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))

	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	if !errors.Is(err, ErrPlaybookAlreadyInstalled) {
		t.Fatalf("生效中的组复原应 already_installed, got %v", err)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("already_installed 必须整包 0 写（指纹变了）")
	}
}

// TestRestoreNoSnapshot 是验收 3：老回执没有 pinned → 409 no_snapshot（明说，不猜）。
func TestRestoreNoSnapshot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-legacy"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	// 直接落一条迁移 8 之前的回执形状（没有 pinned 行）。
	raw := `{"apiVersion":"anotify/v1","kind":"InstallReceipt","playbook":{"name":"restore-legacy","version":1},` +
		`"install":{"id":"ins_legacy","at":"2026-11-01T00:00:00Z","dryRun":false,"release":"alpha"},` +
		`"namespace":"demo","values":{"project":"demo"},"valuesDigest":"sha256:legacy",` +
		`"created":{"capabilities":["demo-review@1"],"sources":[]},` +
		`"skipped":{"capabilities":[],"sources":[]},"upgraded":[],"unapplied":[],` +
		`"external":[],"pending":[],"notes":[],"dependencies":[]}`
	var playbookID string
	if err := db.QueryRow(`SELECT id FROM playbooks WHERE namespace_id = ? AND slug = ?`, nsID, slug).Scan(&playbookID); err != nil {
		t.Fatalf("读包 id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO playbook_installs (id, namespace_id, namespace_slug, playbook_id, playbook_slug,
		   "values", values_digest, receipt, source, release, created_at, uninstalled_at)
		 VALUES ('ins_legacy', ?, 'demo', ?, ?, '{"project":"demo"}', 'sha256:legacy', ?, '', 'alpha', ?, ?)`,
		nsID, playbookID, slug, raw, Now(), Now()); err != nil {
		t.Fatalf("落老回执: %v", err)
	}

	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", "sha256:legacy", "")
	if !errors.Is(err, ErrPlaybookSnapshotMissing) {
		t.Fatalf("老回执复原应 no_snapshot, got %v", err)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("no_snapshot 必须整包 0 写（指纹变了）")
	}
}

// 验收 4：pinned 的能力版本被硬删 → 409 pinned_gone（causes 点名 {kind,name,version}）+ 0 写。
func TestRestorePinnedGone(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-gone"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	// 保留期 GC 的物理回收：行直接没了（正文一起没）
	if _, err := db.ExecContext(ctx,
		`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-review' AND version = 1`,
		nsID); err != nil {
		t.Fatalf("硬删能力行: %v", err)
	}

	before := restoreFingerprint(t, db)
	auditBefore := restoreAuditRows(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinnedGone) {
		t.Fatalf("硬删后复原应 pinned_gone, got %v", err)
	}
	if len(restoreErr.Causes) != 1 {
		t.Fatalf("causes = %+v, want 1 条", restoreErr.Causes)
	}
	if c := restoreErr.Causes[0]; c.Kind != "capability" || c.Name != "demo-review" || c.Version != 1 {
		t.Fatalf("cause = %+v, want {capability demo-review 1}", c)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("pinned_gone 必须整包 0 写（指纹变了）")
	}
	if after := restoreAuditRows(t, db); after != auditBefore {
		t.Fatalf("被拦下的复原不许写审计行: %d → %d", auditBefore, after)
	}
}

// 验收 5：同名同版本内容不同 → 409 restore_conflict + 0 写（同包另一个能力本该新建）；内容相同 → 200 且记进 reused。
func TestRestoreConflictAndReuse(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-conflict"
	pbCreate(t, db, nsID, slug, restoreTwoCapBundle(slug))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}

	// ① 同名同版本、正文不同：手工建一条 demo-a@1，脚本是 v9；demo-b 缺席（本该被新建）。
	foreign := mkCap(nsID, "demo-a", 1, nil)
	foreign.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho v9\n"
	if err := db.CreateCapability(ctx, foreign); err != nil {
		t.Fatalf("手工建冲突行: %v", err)
	}
	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookRestoreConflict) {
		t.Fatalf("内容不同应 restore_conflict, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Name != "demo-a" || restoreErr.Causes[0].Version != 1 {
		t.Fatalf("causes = %+v, want 点名 demo-a@1", restoreErr.Causes)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("restore_conflict 必须整包 0 写（demo-b 也不许先写进去）")
	}
	if n := liveCapabilityRows(t, db, nsID, "demo-b"); n != 0 {
		t.Fatalf("冲突时 demo-b 不该被写进库, got %d 行", n)
	}

	// ② 内容相同：把 demo-a 换成与快照逐字节相同的正文，复原应复用（demo-b 才是本次要建的）。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-a' AND deleted_at IS NULL`, nsID); err != nil {
		t.Fatalf("清掉冲突行: %v", err)
	}
	same := mkCap(nsID, "demo-a", 1, nil)
	same.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho a1\n"
	if err := db.CreateCapability(ctx, same); err != nil {
		t.Fatalf("手工建同内容行: %v", err)
	}
	var sameID string
	var sameRV int64
	if err := db.QueryRow(`SELECT id, resource_version FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-a' AND version = 1 AND deleted_at IS NULL`,
		nsID).Scan(&sameID, &sameRV); err != nil {
		t.Fatalf("读同内容行: %v", err)
	}

	rcpt2, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	if err != nil {
		t.Fatalf("同内容复原应 200: %v", err)
	}
	if got := rcpt2.Skipped.Capabilities; len(got) != 1 || got[0] != "demo-a@1" {
		t.Fatalf("skipped.capabilities = %v, want [demo-a@1]（复用，不是重建）", got)
	}
	if got := rcpt2.Created.Capabilities; len(got) != 1 || got[0] != "demo-b@1" {
		t.Fatalf("created.capabilities = %v, want [demo-b@1]（只有它该被建）", got)
	}
	var gotID string
	var gotRV int64
	if err := db.QueryRow(`SELECT id, resource_version FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-a' AND version = 1 AND deleted_at IS NULL`,
		nsID).Scan(&gotID, &gotRV); err != nil {
		t.Fatalf("复用后读行: %v", err)
	}
	if gotID != sameID || gotRV != sameRV {
		t.Fatalf("复用不该覆盖既有行: id %s→%s, rv %d→%d", sameID, gotID, sameRV, gotRV)
	}
}

// liveCapabilityRows 数某个名字当前可见的行数（0 写断言的第二道证词）。
func liveCapabilityRows(t *testing.T, db *DB, nsID, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions
		 WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL`, nsID, name).Scan(&n); err != nil {
		t.Fatalf("数 %s 可见行: %v", name, err)
	}
	return n
}

// cause 的 Index 是「类内下标」而不是「失败序号」：2 个 pinned 能力、坏的是第二个时要指到第二个。
func TestRestoreCauseIndexPointsAtPinnedSlot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-index"
	pbCreate(t, db, nsID, slug, restoreTwoCapBundle(slug))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	// demo-a 排在前（=0），demo-b 排在后（=1）：只硬删 demo-b，期望 Index=1。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-b'`, nsID); err != nil {
		t.Fatalf("硬删 demo-b: %v", err)
	}
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinnedGone) {
		t.Fatalf("应 pinned_gone, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Name != "demo-b" || restoreErr.Causes[0].Index != 1 {
		t.Fatalf("causes = %+v, want {demo-b Index:1}（不是 0 —— 0 会指到 demo-a）", restoreErr.Causes)
	}
}

// 验收 6（#632 改写）：源被物理删掉、**且重算不可能**（钉住版本行被硬删）→ 仍 409 pin_missing_source + 0 写。
// 这一档盖的是「不可重算」；能重算的那一档见 TestRestoreRecreatesOwnSourceAfterUninstall。
func TestRestoreMissingSource(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-source"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	// 包升到 v2（v1 的正文先留在版本历史里），再把 v1 那一行硬删：钉住的那份正文从此不可用 → 源无法重算。
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug),
		restoreBundleV(slug, "#!/bin/sh\necho v2\n", 2), ""); err != nil {
		t.Fatalf("升级包体: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM playbook_versions WHERE version = 1
		   AND playbook_id IN (SELECT id FROM playbooks WHERE namespace_id = ? AND slug = ?)`, nsID, slug); err != nil {
		t.Fatalf("硬删 v1 版本行: %v", err)
	}

	before := restoreFingerprint(t, db)
	auditBefore := restoreAuditRows(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinMissingSource) {
		t.Fatalf("钉住版本不可用后复原应 pin_missing_source, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Kind != "source" || restoreErr.Causes[0].Name != "demo-on-push" {
		t.Fatalf("causes = %+v, want 点名源 demo-on-push", restoreErr.Causes)
	}
	if msg := restoreErr.Causes[0].Message; !strings.Contains(msg, "v1") || !strings.Contains(msg, "已不可用") {
		t.Fatalf("cause.message = %q, want 点名钉住的包版本 v1 已不可用", msg)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("pin_missing_source 必须整包 0 写（指纹变了）")
	}
	if after := restoreAuditRows(t, db); after != auditBefore {
		t.Fatalf("被拦下的复原不许写审计行: %d → %d", auditBefore, after)
	}

	// 用户先把源建起来（正文由人负责），复原就只剩能力要建。
	if err := db.CreateSource(ctx, &Source{NamespaceID: nsID, Name: "demo-on-push", Kind: "webhook",
		Config: json.RawMessage(`{"token":"tok"}`)}); err != nil {
		t.Fatalf("手工建源: %v", err)
	}
	if _, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, ""); err != nil {
		t.Fatalf("源补上后复原应 200: %v", err)
	}
}

// #632 的主验收：带源的预案「卸载 → 按快照重装」不该 409。卸载物理删掉了本实例建的源，
// 复原按「钉住的包版本 + 回执 values」把它重算重建（与首次安装同一条渲染 / 写入路径）。
func TestRestoreRecreatesOwnSourceAfterUninstall(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-recreate-source"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))

	before, err := db.GetSourceByName(ctx, nsID, "demo-on-push")
	if err != nil {
		t.Fatalf("读安装出来的源: %v", err)
	}
	if before.Kind != "webhook" || !before.Enabled || len(before.Targets) != 1 {
		t.Fatalf("安装出来的源 = %+v, want webhook / 1 target", before)
	}
	beforeCfg := sourcespec.ParseConfig(before.Kind, before.Config)
	if beforeCfg.Token == "" {
		t.Fatalf("安装出来的源没有 token（服务端应生成一个）")
	}

	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	if _, err := db.GetSourceByName(ctx, nsID, "demo-on-push"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("卸载应物理删掉本实例建的源, got err=%v", err)
	}

	restored, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "usr_actor")
	if err != nil {
		t.Fatalf("源被卸载删掉后复原应按钉住包体重建，不是 409: %v", err)
	}
	if got := restored.Created.Sources; len(got) != 1 || got[0] != "demo-on-push" {
		t.Fatalf("created.sources = %v, want [demo-on-push]（重建，不是复用）", got)
	}
	if len(restored.Skipped.Sources) != 0 {
		t.Fatalf("skipped.sources = %v, want 空（源是重建的）", restored.Skipped.Sources)
	}
	var pinnedSource *playbook.PinnedRef
	for i, pin := range restored.Created.Pinned {
		if pin.Kind == pinnedRefKindSource && pin.Name == "demo-on-push" {
			pinnedSource = &restored.Created.Pinned[i]
		}
	}
	if pinnedSource == nil || pinnedSource.KindOf != "webhook" {
		t.Fatalf("created.pinned = %+v, want 一项 {kind:source name:demo-on-push kindOf:webhook}", restored.Created.Pinned)
	}

	after, err := db.GetSourceByName(ctx, nsID, "demo-on-push")
	if err != nil {
		t.Fatalf("复原后读源: %v", err)
	}
	if after.Kind != before.Kind || after.Enabled != before.Enabled {
		t.Fatalf("复原源的 kind/enabled = %q/%v, want %q/%v", after.Kind, after.Enabled, before.Kind, before.Enabled)
	}
	if !reflect.DeepEqual(after.Targets, before.Targets) {
		t.Fatalf("复原源的 targets = %+v, want %+v", after.Targets, before.Targets)
	}
	// webhook token 是服务端在**创建期**生成的：复原会重新生成一个（同一条 createInstalledSourceOn），
	// 所以只比除 token 之外的 config——拿旧 token 当“复原是否正确”的判据是个假靶子。
	afterCfg := sourcespec.ParseConfig(after.Kind, after.Config)
	if afterCfg.Token == "" || afterCfg.Token == beforeCfg.Token {
		t.Fatalf("复原源的 token = %q, want 服务端重新生成的另一个非空 token（原 %q）", afterCfg.Token, beforeCfg.Token)
	}
	beforeCfg.Token, afterCfg.Token = "", ""
	if !reflect.DeepEqual(afterCfg, beforeCfg) {
		t.Fatalf("复原源的 config（除 token）= %+v, want %+v", afterCfg, beforeCfg)
	}
	if after.InstalledFrom != restored.Install.ID {
		t.Fatalf("installed_from = %q, want 本次复原的 install id %q", after.InstalledFrom, restored.Install.ID)
	}

	audit, err := db.ListPlaybookInstallAudit(ctx, nsID, slug, "alpha")
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(audit))
	}
	var detail PlaybookRestoreAuditDetail
	if err := json.Unmarshal(audit[0].Detail, &detail); err != nil {
		t.Fatalf("decode 审计 detail: %v", err)
	}
	if len(detail.Recreated.Sources) != 1 || detail.Recreated.Sources[0] != "demo-on-push" {
		t.Fatalf("审计 recreated.sources = %v, want [demo-on-push]", detail.Recreated.Sources)
	}
	if len(detail.Reused.Sources) != 0 {
		t.Fatalf("审计 reused.sources = %v, want 空", detail.Reused.Sources)
	}
}

// 同一个版本号的正文可以被覆盖（recordPlaybookVersionOn）：回执的 filesDigest 是“当时那一份”的唯一证明，
// 不符就不许拿现在的内容冒充旧正文（否则就是凭空造一个源出来）。
func TestRestoreRefusesOverwrittenPinnedBody(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-overwritten"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	// 版本号不变、正文换了（同一个 v1 被覆盖）：这就是 digest 守卫存在的理由。
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug),
		restoreBundle(slug, "#!/bin/sh\necho v9\n"), ""); err != nil {
		t.Fatalf("覆盖 v1 正文: %v", err)
	}

	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinMissingSource) {
		t.Fatalf("钉住正文被覆盖后复原应 pin_missing_source, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Name != "demo-on-push" {
		t.Fatalf("causes = %+v, want 点名源 demo-on-push", restoreErr.Causes)
	}
	if msg := restoreErr.Causes[0].Message; !strings.Contains(msg, "覆盖") {
		t.Fatalf("cause.message = %q, want 说清“正文已被覆盖”", msg)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("pin_missing_source 必须整包 0 写（指纹变了）")
	}
	if _, err := db.GetSourceByName(ctx, nsID, "demo-on-push"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("被拦下的复原不许留下源行, got err=%v", err)
	}
}

// restoreMaskedBundle 是「writeOnly 字段进源 config」的包：回执里的 values 是打码后的，重算会拿 “***”
// 当成真实 token 写进源——打码守卫必须拦住它（宁可 409，不可把占位符当凭据）。
func restoreMaskedBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml": `apiVersion: anotify/v1
kind: Playbook
metadata:
  name: ` + slug + `
  version: 1
spec:
  values:
    type: object
    required: [project, token]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }
      token: { type: string, writeOnly: true }
  capabilities: []
  sources:
    - sources/on-push.yaml
`,
		"sources/on-push.yaml": `metadata:
  name: '{{values.project}}-on-push'
spec:
  kind: webhook
  enabled: true
  config:
    token: '{{values.token}}'
  targets: []
`,
	}
}

// 回执 values 里 writeOnly 字段已打码（"***"）→ 重算不可能 → 409 pin_missing_source，且库里不多出源行。
func TestRestoreRefusesMaskedSourceValues(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-masked"
	pbCreate(t, db, nsID, slug, restoreMaskedBundle(slug))
	values := map[string]any{"project": "demo", "token": "tok-real"}
	rcpt := installForRestore(t, db, nsID, slug, "alpha", values)
	if rcpt.Values["token"] != playbook.MaskedValue {
		t.Fatalf("回执 values.token = %v, want 打码值 %q", rcpt.Values["token"], playbook.MaskedValue)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}

	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinMissingSource) {
		t.Fatalf("values 已打码时复原应 pin_missing_source, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Kind != "source" || restoreErr.Causes[0].Name != "demo-on-push" {
		t.Fatalf("causes = %+v, want 点名源 demo-on-push", restoreErr.Causes)
	}
	if msg := restoreErr.Causes[0].Message; !strings.Contains(msg, "打码") {
		t.Fatalf("cause.message = %q, want 说清“writeOnly 字段已打码”", msg)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("pin_missing_source 必须整包 0 写（指纹变了）")
	}
	if _, err := db.GetSourceByName(ctx, nsID, "demo-on-push"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("打码值绝不许被当成源正文写进库, got err=%v", err)
	}
}

// 验收 5 / 7：审计一行 install.restored（actor、installId、计数）+ 新回执与首次安装同形。
func TestRestoreAuditAndReceipt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-audit"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	restoreRecreateSource(t, db, nsID, "demo-on-push")
	restored, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "usr_actor")
	if err != nil {
		t.Fatalf("复原: %v", err)
	}
	if restored.ValuesDigest != rcpt.ValuesDigest {
		t.Fatalf("回执 valuesDigest = %q, want 来源的 %q", restored.ValuesDigest, rcpt.ValuesDigest)
	}
	if restored.Values["project"] != "demo" {
		t.Fatalf("回执 values = %v, want 沿用来源回执", restored.Values)
	}
	// 这个用例手工把源提前建好：走的是「同名源已存在 → 复用」那条分支（不重算、不覆盖），
	// 所以源记在 skipped 而不是 created。
	if got := restored.Skipped.Sources; len(got) != 1 || got[0] != "demo-on-push" {
		t.Fatalf("skipped.sources = %v, want [demo-on-push]（已存在 → 复用）", got)
	}
	if len(restored.Created.Sources) != 0 {
		t.Fatalf("created.sources = %v, want 空（源是复用的，不是重算的）", restored.Created.Sources)
	}
	if len(restored.Created.Pinned) != 1 || restored.Created.Pinned[0].Digest == "" {
		t.Fatalf("created.pinned = %+v, want 一项带 digest", restored.Created.Pinned)
	}

	rows, err := db.ListPlaybookInstallAudit(ctx, nsID, slug, "alpha")
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Action != PlaybookInstallAuditRestored || got.ActorID != "usr_actor" || got.InstallID != restored.Install.ID {
		t.Fatalf("审计行 = %+v, want action/actor/installId 全对", got)
	}
	var detail PlaybookRestoreAuditDetail
	if err := json.Unmarshal(got.Detail, &detail); err != nil {
		t.Fatalf("decode 审计 detail: %v", err)
	}
	if detail.RestoredFrom != rcpt.Install.ID {
		t.Fatalf("detail.restoredFrom = %q, want %q", detail.RestoredFrom, rcpt.Install.ID)
	}
	if len(detail.Recreated.Capabilities) != 1 || len(detail.Reused.Sources) != 1 {
		t.Fatalf("detail = %+v, want 1 个重建能力 + 1 个复用源", detail)
	}
}

// TestRestoreUnknownGroup 是 404 面：没有该参数组的回执 → release_not_found（与 YAML 按组导出同口径）。
func TestRestoreUnknownGroup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-unknown"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}
	if _, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", "sha256:nope", ""); !errors.Is(err, ErrPlaybookReleaseNotFound) {
		t.Fatalf("未知参数组应 release_not_found, got %v", err)
	}
	if _, err := db.RestorePlaybookRelease(ctx, nsID, slug, "ghost", rcpt.ValuesDigest, ""); !errors.Is(err, ErrPlaybookReleaseNotFound) {
		t.Fatalf("未知 release 应 release_not_found, got %v", err)
	}
}
