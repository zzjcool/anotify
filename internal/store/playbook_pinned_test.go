package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// pbPinnedBundle 是「2 能力 + 1 源」的包：入口按 review、gate 的顺序声明（与名字升序相反），用来验回执里的 pinned 按 name 重排、不是照包内顺序抄的。
func pbPinnedBundle(slug string) map[string]string {
	gatePath := "capabilities/gate.yaml"
	reviewPath := "capabilities/review.yaml"
	srcPath := "sources/on-push.yaml"
	return map[string]string{
		"playbook.yaml": pbEntry(slug, 1, []string{reviewPath, gatePath}, []string{srcPath}),
		gatePath:        pbCapabilityDoc("{{values.project}}-gate", []string{"{{values.project}}-review"}, "#!/usr/bin/env bash\necho gate"),
		reviewPath:      pbCapabilityDoc("{{values.project}}-review", nil, "#!/usr/bin/env bash\necho review"),
		srcPath:         pbSourceDoc("{{values.project}}-on-push", []string{"{{values.project}}-review"}),
	}
}

// TestInstallReceiptPinned：装一个含 2 能力 + 1 源的包 → created.pinned 的能力在前、源在后，且每个能力的 digest 与那一版版本行的 digest 逐字相同（回读而不是新算）。
func TestInstallReceiptPinned(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pinned-pkg", pbPinnedBundle("pinned-pkg"))

	rcpt, err := db.InstallPlaybook(ctx, nsID, "pinned-pkg", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	got := rcpt.Created.Pinned
	want := []playbook.PinnedRef{
		{Kind: "capability", Name: "demo-gate", Version: 1},
		{Kind: "capability", Name: "demo-review", Version: 1},
		{Kind: "source", Name: "demo-on-push", KindOf: "webhook"},
	}
	if len(got) != len(want) {
		t.Fatalf("created.pinned = %+v, want %d 项（能力按名字升序在前、源在后）", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || g.Name != w.Name || g.Version != w.Version || g.KindOf != w.KindOf {
			t.Fatalf("created.pinned[%d] = %+v, want %+v", i, g, w)
		}
		if w.Kind != "capability" {
			continue
		}
		var rowDigest string
		if err := db.QueryRow(
			`SELECT digest FROM capability_versions WHERE namespace_id = ? AND name = ? AND version = ?`,
			nsID, w.Name, w.Version).Scan(&rowDigest); err != nil {
			t.Fatalf("读 %s@%d 的版本行: %v", w.Name, w.Version, err)
		}
		if rowDigest == "" || g.Digest != rowDigest {
			t.Fatalf("created.pinned[%d].digest = %q, want 版本行的 %q", i, g.Digest, rowDigest)
		}
	}
	// 源没有版本概念：那一行不许带 version / digest。
	src := got[len(got)-1]
	if src.Version != 0 || src.Digest != "" {
		t.Fatalf("源项 = %+v, want version=0 且 digest 为空", src)
	}
}

// TestInstallReceiptPinnedLegacyEmpty：老回执（没有 pinned 字段）读出来是 `[]`，且库里的原文一个字节都没变（审计快照不许被读路径改写）。
func TestInstallReceiptPinnedLegacyEmpty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "legacy-pinned", pbEmpty("legacy-pinned"))

	raw := `{"apiVersion":"anotify/v1","kind":"InstallReceipt","playbook":{"name":"legacy-pinned","version":1},` +
		`"install":{"id":"ins_nopinned","at":"2026-11-01T00:00:00Z","dryRun":false},` +
		`"namespace":"demo","values":{},"valuesDigest":"sha256:nopinned",` +
		`"created":{"capabilities":[],"sources":[]},"skipped":{"capabilities":[],"sources":[]},` +
		`"upgraded":[],"unapplied":[],"external":[],"pending":[],"notes":[]}`
	mustExec(t, db, `INSERT INTO playbook_installs
		   (id, namespace_id, namespace_slug, playbook_id, playbook_slug,
		    "values", values_digest, receipt, source, release, created_at)
		 VALUES ('ins_nopinned', ?, 'demo', 'pb_legacy', 'legacy-pinned','{}','sha256:nopinned',?,'','legacy-pinned',1700000000)`,
		nsID, raw)

	got, err := db.GetPlaybookInstall(ctx, nsID, "legacy-pinned", "ins_nopinned")
	if err != nil {
		t.Fatalf("get install: %v", err)
	}
	if got.Created.Pinned == nil || len(got.Created.Pinned) != 0 {
		t.Fatalf("老回执 created.pinned = %#v, want 非 nil 的空切片", got.Created.Pinned)
	}
	if !reflect.DeepEqual(got.Created.Pinned, []playbook.PinnedRef{}) {
		t.Fatalf("老回执 created.pinned = %#v, want []playbook.PinnedRef{}", got.Created.Pinned)
	}
	if got.Skipped.Pinned == nil || len(got.Skipped.Pinned) != 0 {
		t.Fatalf("老回执 skipped.pinned = %#v, want 非 nil 的空切片", got.Skipped.Pinned)
	}
	var stored string
	if err := db.QueryRow(`SELECT receipt FROM playbook_installs WHERE id = 'ins_nopinned'`).Scan(&stored); err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if stored != raw {
		t.Fatalf("读路径改了回执原文（审计快照必须原样）")
	}
}

// TestPlaybookInstallPinnedRows：索引表与回执双写一致性（行数、seq、每列都与 created.pinned 逐项相等）；dryRun 走同一条写路径但整包回滚，不留任何行。
func TestPlaybookInstallPinnedRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pinned-rows", pbPinnedBundle("pinned-rows"))

	rcpt, err := db.InstallPlaybook(ctx, nsID, "pinned-rows", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	rows, err := db.Query(
		`SELECT seq, ref_kind, name, obj_kind, version, digest
		   FROM playbook_install_pinned WHERE install_id = ? ORDER BY seq`, rcpt.Install.ID)
	if err != nil {
		t.Fatalf("list pinned rows: %v", err)
	}
	defer rows.Close()
	var stored []playbook.PinnedRef
	for rows.Next() {
		var (
			seq int
			pin playbook.PinnedRef
		)
		if err := rows.Scan(&seq, &pin.Kind, &pin.Name, &pin.KindOf, &pin.Version, &pin.Digest); err != nil {
			t.Fatalf("scan pinned row: %v", err)
		}
		if seq != len(stored) {
			t.Fatalf("第 %d 行的 seq = %d（seq 必须是切片下标）", len(stored), seq)
		}
		stored = append(stored, pin)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pinned rows: %v", err)
	}
	if !reflect.DeepEqual(stored, rcpt.Created.Pinned) {
		t.Fatalf("索引表与回执不一致:\n table %+v\n rcpt  %+v", stored, rcpt.Created.Pinned)
	}
	byInstall, err := db.PlaybookInstallPinned(ctx, nsID, []string{rcpt.Install.ID})
	if err != nil {
		t.Fatalf("PlaybookInstallPinned: %v", err)
	}
	if !reflect.DeepEqual(byInstall[rcpt.Install.ID], rcpt.Created.Pinned) {
		t.Fatalf("PlaybookInstallPinned 读回 %+v, want %+v", byInstall[rcpt.Install.ID], rcpt.Created.Pinned)
	}

	// dryRun 走同一路径再回滚：0 行，但回执里照样有 pinned。
	dryNS, _ := newTestNS(t, db)
	pbCreate(t, db, dryNS, "pinned-dry", pbPinnedBundle("pinned-dry"))
	dry, err := db.InstallPlaybook(ctx, dryNS, "pinned-dry", "blue", pbValues("demo"), true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dry.Created.Pinned) != 3 {
		t.Fatalf("dryRun 回执 created.pinned = %+v, want 3 项", dry.Created.Pinned)
	}
	if n := countRowsWhere(t, db, "playbook_install_pinned", "namespace_id = '"+dryNS+"'"); n != 0 {
		t.Fatalf("dryRun 留下 %d 行 pinned 索引（应随事务回滚）", n)
	}
}

// TestGetCapabilityForSnapshotIncludesDeleted：卸载/删除只 SET deleted_at、正文还在，所以「已卸载实例的安装时快照」仍能取到那一版（digest 与正文都对）——这是 as=installed 的依据。
func TestGetCapabilityForSnapshotIncludesDeleted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	soloPath := "capabilities/solo.yaml"
	pbCreate(t, db, nsID, "pinned-solo", map[string]string{
		"playbook.yaml": pbEntry("pinned-solo", 1, []string{soloPath}, nil),
		soloPath:        pbCapabilityDoc("{{values.project}}-solo", nil, "#!/usr/bin/env bash\necho solo"),
	})
	if _, err := db.InstallPlaybook(ctx, nsID, "pinned-solo", "blue", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	cur, err := db.GetCapability(ctx, nsID, "demo-solo", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if err := db.DeleteCapability(ctx, nsID, "demo-solo", ResourceVersion(cur.ID, cur.ResourceVersion)); err != nil {
		t.Fatalf("delete capability: %v", err)
	}

	if _, err := db.GetCapability(ctx, nsID, "demo-solo", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 GetCapability 应 ErrNotFound, got %v", err)
	}
	snap, err := db.GetCapabilityForSnapshot(ctx, nsID, "demo-solo", 1)
	if err != nil {
		t.Fatalf("卸载后仍应能按快照取到版本行: %v", err)
	}
	if snap.Digest != cur.Digest || !reflect.DeepEqual(snap.Spec, cur.Spec) {
		t.Fatalf("快照读到的行与删除前不一致:\n got  %+v\n want %+v", snap, cur)
	}
	if snap.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("快照应带上正文（entrypoint = %q）", snap.Spec.Run.Entrypoint)
	}
}

// TestPlaybookInstallPinnedEmptyIDs：空 installIDs 返回空 map 且不报错（不许拼出 `IN ()`）。
func TestPlaybookInstallPinnedEmptyIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	for _, ids := range [][]string{nil, {}} {
		got, err := db.PlaybookInstallPinned(ctx, nsID, ids)
		if err != nil {
			t.Fatalf("空 installIDs (%v) 不该报错: %v", ids, err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("空 installIDs (%v) 应返回空 map, got %#v", ids, got)
		}
	}
}

// TestInstallReceiptPinnedSkipped：只复用、没新建的安装也要记 pinned（否则导出它的快照会是空的），且第二次的 skipped.pinned 与第一次的 created.pinned 逐项相同。
func TestInstallReceiptPinnedSkipped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pinned-twice", pbPinnedBundle("pinned-twice"))

	first, err := db.InstallPlaybook(ctx, nsID, "pinned-twice", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install 1: %v", err)
	}
	second, err := db.InstallPlaybook(ctx, nsID, "pinned-twice", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install 2: %v", err)
	}
	if len(second.Created.Pinned) != 0 {
		t.Fatalf("重装不该新建对象，created.pinned = %+v", second.Created.Pinned)
	}
	if !reflect.DeepEqual(second.Skipped.Pinned, first.Created.Pinned) {
		t.Fatalf("重装的 skipped.pinned 与首次 created.pinned 不一致:\n got  %+v\n want %+v",
			second.Skipped.Pinned, first.Created.Pinned)
	}
	byInstall, err := db.PlaybookInstallPinned(ctx, nsID, []string{second.Install.ID})
	if err != nil {
		t.Fatalf("PlaybookInstallPinned: %v", err)
	}
	if !reflect.DeepEqual(byInstall[second.Install.ID], second.Skipped.Pinned) {
		t.Fatalf("索引表里第二次安装的行 %+v, want %+v", byInstall[second.Install.ID], second.Skipped.Pinned)
	}
}
