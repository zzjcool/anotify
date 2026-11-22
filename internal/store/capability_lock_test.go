package store

// #534 依赖锁的数据层验收：安装写依赖索引、删除能力被已生效安装挡住、卸载后放行。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// clCap 造一个可被 requires 解析到的能力（v1/v2 正文不同：同内容会折叠成一行，v2 就不存在了）。
func clCap(nsID, name string, version int) *CapabilityVersion {
	c := mkCap(nsID, name, version, nil)
	c.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho v" + itoa64(int64(version)) + "\n"
	return c
}

// clBundle 是只声明外部依赖、自己什么都不建的包；requires 是 spec.requires 那几行 YAML。
func clBundle(slug, requires string) map[string]string {
	return map[string]string{"playbook.yaml": pbEntry(slug, 1, nil, nil) + requires}
}

// clConsumers 读 capabilityConsumersOn 的结果（走 store 的正式查询路径，不另写一份 SQL）。
func clConsumers(t *testing.T, db *DB, nsID, name string) []DependencyConsumer {
	t.Helper()
	consumers, err := capabilityConsumersOn(context.Background(), db, nsID, name)
	if err != nil {
		t.Fatalf("capabilityConsumersOn(%s): %v", name, err)
	}
	return consumers
}

// TestPlaybookInstallRequiresWritesIndex：requires 解析出的依赖与回执同一次事务落进索引表，
// dryRun 一条都不留（回滚）。索引行的 version 是**解析到的最高可见版本**，不是声明里的 minVersion。
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

// TestDeleteCapabilityLockedByRequires：被已生效安装依赖 → *CapabilityLockedError 且 0 写（能力仍在）；
// 卸载那个 release 之后再删就成功。
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
	// 名字与消费者都出现在 Error() 里：上层日志只看这一句就能定位。
	for _, want := range []string{"inventory", "consumes", "blue"} {
		if !strings.Contains(locked.Error(), want) {
			t.Fatalf("Error() = %q，缺少 %q", locked.Error(), want)
		}
	}
	// 0 写：能力仍可见，令牌也没被顺手推进。
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

// TestCapabilityConsumersDedupAndSort：同一个 (slug, release) 装过多次只算一个消费者（去重），
// 消费者按 (slug, release) 排序（判据稳定，不依赖插入顺序）。
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
	// 别的命名空间看不见这条依赖：锁判定必须带 namespace_id。
	otherNS := newTestExtraNS(t, db, "other")
	if got := clConsumers(t, db, otherNS, "inventory"); len(got) != 0 {
		t.Fatalf("跨命名空间读到了消费者: %+v", got)
	}
}

// TestGetPlaybookInstallLegacyReceiptGetsEmptyDependencies：requires 之前写的回执没有 dependencies 键，
// 读出来归一成 `[]playbook.Dependency{}`（空列表纪律不因写入时间而破）。
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
