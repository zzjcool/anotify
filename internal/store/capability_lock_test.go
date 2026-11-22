package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

func clCap(nsID, name string, version int) *CapabilityVersion {
	c := mkCap(nsID, name, version, nil)
	c.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho v" + itoa64(int64(version)) + "\n"
	return c
}

func clBundle(slug, requires string) map[string]string {
	return map[string]string{"playbook.yaml": pbEntry(slug, 1, nil, nil) + requires}
}

func clConsumers(t *testing.T, db *DB, nsID, name string) []DependencyConsumer {
	t.Helper()
	consumers, err := capabilityConsumersOn(context.Background(), db, nsID, name)
	if err != nil {
		t.Fatalf("capabilityConsumersOn(%s): %v", name, err)
	}
	return consumers
}

func TestPlaybookInstallRequiresWritesIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	for _, v := range []int{1, 2} {
		if err := db.CreateCapability(ctx, clCap(nsID, "inventory", v)); err != nil {
			t.Fatalf("create inventory v%d: %v", v, err)
		}
	}
	pbCreate(t, db, nsID, "consumes", clBundle("consumes", "  requires:\n    - capability: inventory\n      minVersion: 2\n"))

	if _, err := db.InstallPlaybook(ctx, nsID, "consumes", "blue", pbValues("demo"), true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := countRows(t, db, "playbook_install_requires"); n != 0 {
		t.Fatalf("dryRun 留了 %d 条依赖行（应随事务回滚）", n)
	}

	rcpt, err := db.InstallPlaybook(ctx, nsID, "consumes", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	want := []playbook.Dependency{{Capability: "inventory", Version: 2}}
	if !reflect.DeepEqual(rcpt.Dependencies, want) {
		t.Fatalf("回执 dependencies = %+v, want %+v", rcpt.Dependencies, want)
	}

	var installID, capability string
	var version int
	rows, err := db.Query(`SELECT install_id, capability, version FROM playbook_install_requires
		WHERE namespace_id = ?`, nsID)
	if err != nil {
		t.Fatalf("read requires rows: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		if err := rows.Scan(&installID, &capability, &version); err != nil {
			t.Fatalf("scan requires row: %v", err)
		}
		got = append(got, installID+"\x00"+capability+"\x00"+itoa64(int64(version)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate requires rows: %v", err)
	}
	wantRow := rcpt.Install.ID + "\x00inventory\x002"
	if len(got) != 1 || got[0] != wantRow {
		t.Fatalf("依赖索引行 = %v, want [%q]", got, wantRow)
	}
}

func TestDeleteCapabilityLockedByRequires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	if err := db.CreateCapability(ctx, clCap(nsID, "inventory", 1)); err != nil {
		t.Fatalf("create inventory: %v", err)
	}
	pbCreate(t, db, nsID, "consumes", clBundle("consumes", "  requires:\n    - capability: inventory\n"))
	if _, err := db.InstallPlaybook(ctx, nsID, "consumes", "blue", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	cur, err := db.GetCapability(ctx, nsID, "inventory", 1)
	if err != nil {
		t.Fatalf("get inventory: %v", err)
	}
	token := ResourceVersion(cur.ID, cur.ResourceVersion)

	err = db.DeleteCapability(ctx, nsID, "inventory", token)
	var locked *CapabilityLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("应返回 *CapabilityLockedError, got %v", err)
	}
	wantConsumers := []DependencyConsumer{{Playbook: "consumes", Release: "blue"}}
	if !reflect.DeepEqual(locked.Consumers, wantConsumers) || locked.Capability != "inventory" {
		t.Fatalf("锁定信息 = %q / %+v, want inventory / %+v", locked.Capability, locked.Consumers, wantConsumers)
	}
	for _, want := range []string{"inventory", "consumes", "blue"} {
		if !strings.Contains(locked.Error(), want) {
			t.Fatalf("Error() = %q，缺少 %q", locked.Error(), want)
		}
	}
	if again, err := db.GetCapability(ctx, nsID, "inventory", 1); err != nil || again.ResourceVersion != cur.ResourceVersion {
		t.Fatalf("锁拒绝改动了能力行（应当 0 写）: %+v, %v", again, err)
	}

	if _, err := db.UninstallPlaybookRelease(ctx, nsID, "consumes", "blue", ""); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if err := db.DeleteCapability(ctx, nsID, "inventory", token); err != nil {
		t.Fatalf("卸载后仍删不掉: %v", err)
	}
	if _, err := db.GetCapability(ctx, nsID, "inventory", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后能力仍可见: %v", err)
	}
}

func TestCapabilityConsumersDedupAndSort(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	if err := db.CreateCapability(ctx, clCap(nsID, "inventory", 1)); err != nil {
		t.Fatalf("create inventory: %v", err)
	}
	requires := "  requires:\n    - capability: inventory\n"
	pbCreate(t, db, nsID, "zeta", clBundle("zeta", requires))
	pbCreate(t, db, nsID, "alpha", clBundle("alpha", requires))
	for _, slug := range []string{"zeta", "zeta", "alpha"} { // zeta@r1 装两次：两条回执，一个消费者。
		if _, err := db.InstallPlaybook(ctx, nsID, slug, "r1", pbValues("demo"), false); err != nil {
			t.Fatalf("install %s: %v", slug, err)
		}
	}
	if n := countRows(t, db, "playbook_install_requires"); n != 3 {
		t.Fatalf("依赖索引应有 3 行（每次安装一条回执各自一行），实际 %d", n)
	}
	want := []DependencyConsumer{{Playbook: "alpha", Release: "r1"}, {Playbook: "zeta", Release: "r1"}}
	if got := clConsumers(t, db, nsID, "inventory"); !reflect.DeepEqual(got, want) {
		t.Fatalf("consumers = %+v, want %+v（按 slug+release 去重并按 slug 排序）", got, want)
	}
	otherNS := newTestExtraNS(t, db, "other")
	if got := clConsumers(t, db, otherNS, "inventory"); len(got) != 0 {
		t.Fatalf("跨命名空间读到了消费者: %+v", got)
	}
}

func TestGetPlaybookInstallLegacyReceiptGetsEmptyDependencies(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	raw := `{"apiVersion":"anotify/v1","kind":"InstallReceipt","playbook":{"name":"legacy-deps","version":1},` +
		`"install":{"id":"ins_legacy_deps","at":"2026-11-01T00:00:00Z","dryRun":false},` +
		`"namespace":"demo","values":{},"valuesDigest":"sha256:legacy",` +
		`"created":{"capabilities":[],"sources":[]},"skipped":{"capabilities":[],"sources":[]},` +
		`"upgraded":[],"unapplied":[],"external":[],"pending":[],"notes":[]}`
	mustExec(t, db, `INSERT INTO playbook_installs
		   (id, namespace_id, namespace_slug, playbook_id, playbook_slug,
		    "values", values_digest, receipt, source, release, created_at)
		 VALUES ('ins_legacy_deps', ?, 'demo', 'pb_legacy', 'legacy-deps','{}','sha256:legacy',?,'','legacy-deps',1700000000)`,
		nsID, raw)

	got, err := db.GetPlaybookInstall(ctx, nsID, "legacy-deps", "ins_legacy_deps")
	if err != nil {
		t.Fatalf("get install: %v", err)
	}
	if got.Dependencies == nil || len(got.Dependencies) != 0 {
		t.Fatalf("旧回执的 Dependencies = %#v, want 非 nil 的空切片", got.Dependencies)
	}
	if !reflect.DeepEqual(got.Dependencies, []playbook.Dependency{}) {
		t.Fatalf("旧回执的 Dependencies = %#v, want []playbook.Dependency{}", got.Dependencies)
	}
}

func TestDeleteCapabilityUnlockedWhenConsumerManifestDeleted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	if err := db.CreateCapability(ctx, clCap(nsID, "inventory", 1)); err != nil {
		t.Fatalf("create inventory: %v", err)
	}
	pbCreate(t, db, nsID, "consumes", clBundle("consumes", "  requires:\n    - capability: inventory\n"))
	if _, err := db.InstallPlaybook(ctx, nsID, "consumes", "blue", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if consumers := clConsumers(t, db, nsID, "inventory"); len(consumers) != 1 {
		t.Fatalf("清单还在时消费者 = %+v，期望 1 条", consumers)
	}
	if _, err := db.DeletePlaybook(ctx, nsID, "consumes", pbVersionToken(t, db, nsID, "consumes")); err != nil {
		t.Fatalf("delete playbook manifest: %v", err)
	}
	if consumers := clConsumers(t, db, nsID, "inventory"); len(consumers) != 0 {
		t.Fatalf("清单已删后消费者 = %+v，期望空（那条回执既看不见也卸不掉）", consumers)
	}
	cur, err := db.GetCapability(ctx, nsID, "inventory", 1)
	if err != nil {
		t.Fatalf("get inventory: %v", err)
	}
	if err := db.DeleteCapability(ctx, nsID, "inventory", ResourceVersion(cur.ID, cur.ResourceVersion)); err != nil {
		t.Fatalf("清单删掉后能力仍被锁死: %v", err)
	}
}
