package store

// 按快照重装（#561）的领域验收：版本不漂移、冲突整包 0 写、源缺就报错、审计一行。
// 0 写用 #535 的 6 表指纹（`restoreFingerprint`）证明，不是「看到 409 就算过」。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// restoreBundle 是「1 能力 + 1 源」的包；script 让不同版本的正文可区分（断言复原的是哪一版）。
// 能力名**静态**（两个 release 才能长出同名的新版本），源名按 values 区分（同一个源不能属于两个 release）。
func restoreBundle(slug, script string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbEntry(slug, 1, []string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"}),
		"capabilities/review.yaml": pbCapabilityDoc("demo-review", nil, script),
		"sources/on-push.yaml":     pbSourceDoc("{{values.project}}-on-push", []string{"demo-review"}),
	}
}

// restoreRecreateSource 把卸载时被物理删掉的源按名字补回来（源正文不在回执里：复原只复用、不重建）。
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

// TestRestoreUsesPinnedVersionNotLatest 是验收 1：装 v1 → 把包升到 v2（同名能力长出 v2）→ 装 v2 →
// 两个实例都卸载 → 复原 v1 那一组：重建的是 **pinned 的 v1**（版本与正文），不是最新的 v2。
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
		restoreBundle(slug, "#!/bin/sh\necho v2\n"), ""); err != nil {
		t.Fatalf("升级包体: %v", err)
	}
	second := installForRestore(t, db, nsID, slug, "beta", pbValues("beta"))
	if second.Created.Pinned[0].Version != 2 {
		t.Fatalf("第二次安装的快照 = %+v, want demo-review@2", second.Created.Pinned)
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

	restoreRecreateSource(t, db, nsID, "alpha-on-push")
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
	if rcpt.Skipped.Sources == nil || len(rcpt.Skipped.Sources) != 1 || rcpt.Skipped.Sources[0] != "alpha-on-push" {
		t.Fatalf("卸载会物理删源，复原必须按名字重建它；skipped.sources = %v", rcpt.Skipped.Sources)
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
	// 直接落一条迁移 8 之前的回执形状：没有 pinned 行、receipt 里也没有 created.pinned。
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

// TestRestorePinnedGone 是验收 4：pinned 的能力版本被硬删 → 409 pinned_gone，causes 点名 {kind,name,version}，0 写。
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
	// 保留期 GC 的物理回收：行直接没了（正文一起没）。
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

// TestRestoreConflictAndReuse 是验收 5：同名同版本但内容不同 → 409 restore_conflict + 0 写；
// 内容相同 → 200 且记进 reused（既有行一个字节都不动）。
func TestRestoreConflictAndReuse(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "restore-conflict"
	pbCreate(t, db, nsID, slug, restoreBundle(slug, "#!/bin/sh\necho v1\n"))
	rcpt := installForRestore(t, db, nsID, slug, "alpha", pbValues("demo"))
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, slug, "alpha", ""); err != nil {
		t.Fatalf("卸载: %v", err)
	}

	restoreRecreateSource(t, db, nsID, "demo-on-push")
	// ① 同名同版本、正文不同：手工建一条 demo-review@1，脚本是 v9。
	foreign := mkCap(nsID, "demo-review", 1, nil)
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
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Name != "demo-review" || restoreErr.Causes[0].Version != 1 {
		t.Fatalf("causes = %+v, want 点名 demo-review@1", restoreErr.Causes)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("restore_conflict 必须整包 0 写（指纹变了）")
	}

	// ② 内容相同：换成与快照逐字节相同的正文，复原应复用（reused）而不是报错、也不覆盖。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-review' AND deleted_at IS NULL`,
		nsID); err != nil {
		t.Fatalf("清掉冲突行: %v", err)
	}
	same := mkCap(nsID, "demo-review", 1, nil)
	same.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho v1\n"
	if err := db.CreateCapability(ctx, same); err != nil {
		t.Fatalf("手工建同内容行: %v", err)
	}
	var sameID string
	var sameRV int64
	if err := db.QueryRow(`SELECT id, resource_version FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-review' AND version = 1 AND deleted_at IS NULL`,
		nsID).Scan(&sameID, &sameRV); err != nil {
		t.Fatalf("读同内容行: %v", err)
	}

	rcpt2, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	if err != nil {
		t.Fatalf("同内容复原应 200: %v", err)
	}
	if got := rcpt2.Skipped.Capabilities; len(got) != 1 || got[0] != "demo-review@1" {
		t.Fatalf("skipped.capabilities = %v, want [demo-review@1]（复用，不是重建）", got)
	}
	if got := rcpt2.Created.Capabilities; len(got) != 0 {
		t.Fatalf("created.capabilities = %v, want []（内容相同就没有要建的东西）", got)
	}
	var gotID string
	var gotRV int64
	if err := db.QueryRow(`SELECT id, resource_version FROM capability_versions
		 WHERE namespace_id = ? AND name = 'demo-review' AND version = 1 AND deleted_at IS NULL`,
		nsID).Scan(&gotID, &gotRV); err != nil {
		t.Fatalf("复用后读行: %v", err)
	}
	if gotID != sameID || gotRV != sameRV {
		t.Fatalf("复用不该覆盖既有行: id %s→%s, rv %d→%d", sameID, gotID, sameRV, gotRV)
	}
}

// TestRestoreMissingSource 是验收 6：pinned 的源不存在 → 409 pin_missing_source（不假装能重建源），0 写。
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

	before := restoreFingerprint(t, db)
	_, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, "")
	var restoreErr *PlaybookRestoreError
	if !errors.As(err, &restoreErr) || !errors.Is(err, ErrPlaybookPinMissingSource) {
		t.Fatalf("源被物理删后复原应 pin_missing_source, got %v", err)
	}
	if len(restoreErr.Causes) != 1 || restoreErr.Causes[0].Kind != "source" || restoreErr.Causes[0].Name != "demo-on-push" {
		t.Fatalf("causes = %+v, want 点名源 demo-on-push", restoreErr.Causes)
	}
	if after := restoreFingerprint(t, db); after != before {
		t.Fatalf("pin_missing_source 必须整包 0 写（指纹变了）")
	}

	// 用户先把源建起来（正文由人负责），复原就只剩能力要建。
	if err := db.CreateSource(ctx, &Source{NamespaceID: nsID, Name: "demo-on-push", Kind: "webhook",
		Config: json.RawMessage(`{"secretToken":"tok"}`)}); err != nil {
		t.Skipf("手工建源失败（不在本票范围）: %v", err)
	}
	if _, err := db.RestorePlaybookRelease(ctx, nsID, slug, "alpha", rcpt.ValuesDigest, ""); err != nil {
		t.Fatalf("源补上后复原应 200: %v", err)
	}
}

// TestRestoreAuditAndReceipt 是验收 5 / 7：审计一行 install.restored（actor、installId、计数），
// 新回执与首次安装同形（values / valuesDigest / pinned 都在）。
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
