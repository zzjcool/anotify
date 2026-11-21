package v4test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// 预案卸载（#510）的真实 SQLite 验收：只删本 release 装出的行、回执只打标记、第二次调用 0 写、同名他人行进 kept[]、
// 边投影按幸存的最新可见版本重算；每条断言都读库，不看响应自证。

// pbuSourceDoc 生成指定名字的 webhook 源文档（pbSourceDoc 的源名写死 values.project，不适用于 release 夹具）。
func pbuSourceDoc(name string, targets []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "metadata:\n  name: '%s'\nspec:\n  kind: webhook\n  enabled: true\n  config: {}\n", name)
	b.WriteString("  targets:\n")
	for _, ref := range targets {
		fmt.Fprintf(&b, "    - capability: '%s'\n      params: {}\n", ref)
	}
	return b.String()
}

// pbuPackage 造一个「能力名叫 x、正文由 values 决定」的包（正文差异 → upgraded 建新版本）。
func pbuPackage(slug string) *pbPackage {
	p := newPBPackage(slug)
	p.caps = []string{"capabilities/x.yaml"}
	p.srcs = []string{}
	p.files["capabilities/x.yaml"] = pbCapDoc(pbCapOpts{
		name:    "x",
		next:    []string{},
		runBody: "#!/usr/bin/env bash\necho '{{values.project}}'",
	})
	return p
}

func pbuInstall(t *testing.T, db *store.DB, nsID, slug, release string, values map[string]any) *playbook.Receipt {
	t.Helper()
	r, err := db.InstallPlaybook(context.Background(), nsID, slug, release, values, false)
	if err != nil {
		t.Fatalf("install %s release %s: %v", slug, release, err)
	}
	return r
}

func pbuUninstall(t *testing.T, db *store.DB, nsID, slug, release, pin string) *store.PlaybookUninstallResult {
	t.Helper()
	res, err := db.UninstallPlaybookRelease(context.Background(), nsID, slug, release, pin)
	if err != nil {
		t.Fatalf("uninstall %s release %s: %v", slug, release, err)
	}
	return res
}

func pbuCapDeleted(t *testing.T, db *store.DB, nsID, name string, version int) bool {
	t.Helper()
	var deleted sql.NullInt64
	if err := db.QueryRow(
		`SELECT deleted_at FROM capability_versions WHERE namespace_id = ? AND name = ? AND version = ?`,
		nsID, name, version).Scan(&deleted); err != nil {
		t.Fatalf("read capability %s@%d: %v", name, version, err)
	}
	return deleted.Valid
}

func pbuSourceExists(t *testing.T, db *store.DB, nsID, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sources WHERE namespace_id = ? AND name = ?`, nsID, name).Scan(&n); err != nil {
		t.Fatalf("probe source %s: %v", name, err)
	}
	return n > 0
}

func pbuUninstalledAt(t *testing.T, db *store.DB, installID string) int64 {
	t.Helper()
	var at int64
	if err := db.QueryRow(`SELECT uninstalled_at FROM playbook_installs WHERE id = ?`, installID).Scan(&at); err != nil {
		t.Fatalf("read uninstalled_at of %s: %v", installID, err)
	}
	return at
}

// pbuEdges 返回某能力名当前的出边（按 to_ref 排序）。
func pbuEdges(t *testing.T, db *store.DB, nsID, fromRef string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT to_ref FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ? ORDER BY to_ref`, nsID, fromRef)
	if err != nil {
		t.Fatalf("read edges of %s: %v", fromRef, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		out = append(out, ref)
	}
	return out
}

// pbuFingerprint 把卸载会碰的四张表的**全部列**按主键拍成稳定文本（幂等 = 逐字节相同）。
func pbuFingerprint(t *testing.T, db *store.DB) string {
	t.Helper()
	specs := []struct{ table, order string }{
		{"capability_versions", "namespace_id, name, version, id"},
		{"sources", "namespace_id, name, id"},
		{"edges", "namespace_id, from_is_source, from_ref, to_ref"},
		{"playbook_installs", "namespace_id, id"},
	}
	var b strings.Builder
	for _, spec := range specs {
		rows, err := db.Query(`SELECT * FROM ` + spec.table + ` ORDER BY ` + spec.order)
		if err != nil {
			t.Fatalf("fingerprint %s: %v", spec.table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatalf("fingerprint columns %s: %v", spec.table, err)
		}
		fmt.Fprintf(&b, "== %s\n", spec.table)
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				t.Fatalf("fingerprint scan %s: %v", spec.table, err)
			}
			for _, v := range values {
				fmt.Fprintf(&b, "%T:%#v|", v, v)
			}
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("fingerprint iterate %s: %v", spec.table, err)
		}
		_ = rows.Close()
	}
	return b.String()
}

// TestPlaybookUninstallRemovesReleaseObjectsOnly 装 a + b 两个 release，撤 a：a 的三行不可见、回执仍在且打上卸载时刻，
// b 的行一字不动、实例清单只剩 b。
func TestPlaybookUninstallRemovesReleaseObjectsOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-uninstall-basic")

	p := newPBPackage("pb-one")
	p.caps = []string{"capabilities/review.yaml", "capabilities/fixer.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "{{release.name}}-review", next: []string{"{{release.name}}-fixer"}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["capabilities/fixer.yaml"] = pbCapDoc(pbCapOpts{
		name: "{{release.name}}-fixer", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbuSourceDoc("{{release.name}}-on-push", []string{"{{release.name}}-review"})
	row := pbCreate(t, db, ns, p)

	rA := pbuInstall(t, db, ns, "pb-one", "a", pbValues("p"))
	rB := pbuInstall(t, db, ns, "pb-one", "b", pbValues("p"))
	bReview := pbLatestCap(t, db, ns, "b-review")
	bFixer := pbLatestCap(t, db, ns, "b-fixer")
	bSrc := pbSource(t, db, ns, "b-on-push")

	res := pbuUninstall(t, db, ns, "pb-one", "a", "")
	if res.AlreadyUninstalled || res.Release != "a" || res.Note == "" || res.UninstalledAt == "" {
		t.Fatalf("首次卸载响应形状不对：%+v", res)
	}
	if !reflect.DeepEqual(res.InstallIDs, []string{rA.Install.ID}) {
		t.Fatalf("installIds = %v", res.InstallIDs)
	}
	if !reflect.DeepEqual(res.Removed.Capabilities, []string{"a-fixer@1", "a-review@1"}) {
		t.Fatalf("removed.capabilities = %v", res.Removed.Capabilities)
	}
	if !reflect.DeepEqual(res.Removed.Sources, []string{"a-on-push"}) {
		t.Fatalf("removed.sources = %v", res.Removed.Sources)
	}
	if res.Kept == nil || len(res.Kept) != 0 {
		t.Fatalf("kept 应是空数组不是 null：%#v", res.Kept)
	}

	for _, ref := range []struct {
		name    string
		version int
	}{{"a-review", 1}, {"a-fixer", 1}} {
		if !pbuCapDeleted(t, db, ns, ref.name, ref.version) {
			t.Fatalf("%s@%d 应被软删（deleted_at 非空）", ref.name, ref.version)
		}
		if _, err := db.GetCapability(ctx, ns, ref.name, ref.version); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s@%d 卸载后应不可见，得到 %v", ref.name, ref.version, err)
		}
	}
	if pbuSourceExists(t, db, ns, "a-on-push") {
		t.Fatal("a 的源行应被删掉")
	}

	// 回执保留（审计快照），只多了卸载时刻。
	got, err := db.GetPlaybookInstall(ctx, ns, "pb-one", rA.Install.ID)
	if err != nil {
		t.Fatalf("卸载后单条回执应仍可读：%v", err)
	}
	if got.Install.ID != rA.Install.ID || got.Install.Release != "a" {
		t.Fatalf("回执内容被改写：%+v", got.Install)
	}
	if pbuUninstalledAt(t, db, rA.Install.ID) == 0 {
		t.Fatal("a 的回执应打上 uninstalled_at")
	}
	if pbuUninstalledAt(t, db, rB.Install.ID) != 0 {
		t.Fatal("b 的回执不该被标记")
	}

	// b 的行一行不动（digest + 令牌都不变）。
	if c := pbLatestCap(t, db, ns, "b-review"); c.Digest != bReview.Digest || c.ResourceVersion != bReview.ResourceVersion {
		t.Fatalf("b-review 被动了：%+v → %+v", bReview, c)
	}
	if c := pbLatestCap(t, db, ns, "b-fixer"); c.Digest != bFixer.Digest || c.ResourceVersion != bFixer.ResourceVersion {
		t.Fatalf("b-fixer 被动了：%+v → %+v", bFixer, c)
	}
	if s := pbSource(t, db, ns, "b-on-push"); s.ResourceVersion != bSrc.ResourceVersion || s.InstalledFrom != bSrc.InstalledFrom {
		t.Fatalf("b 的源被动了：%+v → %+v", bSrc, s)
	}

	// 实例清单不再有 a 的组（已卸载的实例不进清单），b 的组还在。
	list, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if list.Total != 1 || len(list.Installs) != 1 || list.Installs[0].Release != "b" {
		t.Fatalf("卸载后实例清单应只剩 b：%+v", list.Installs)
	}
}

// TestPlaybookUninstallKeepsManualRowOfSameName 同名手工行（installed_from 为空）不删、记 manual。
func TestPlaybookUninstallKeepsManualRowOfSameName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-uninstall-manual")
	pbCreate(t, db, ns, pbuPackage("pb-one"))
	pbuInstall(t, db, ns, "pb-one", "a", pbValues("one"))

	manual := createCap(t, db, testCap(ns, "x", 0, nil))
	if manual.Version != 2 || manual.InstalledFrom != "" {
		t.Fatalf("手工能力应是 x@2 且无 installed_from：%+v", manual)
	}

	res := pbuUninstall(t, db, ns, "pb-one", "a", "")
	want := []store.PlaybookUninstallKept{{Kind: "capability", Name: "x", Version: 2, Reason: "manual"}}
	if !reflect.DeepEqual(res.Kept, want) {
		t.Fatalf("kept = %+v（want %+v）", res.Kept, want)
	}
	if !reflect.DeepEqual(res.Removed.Capabilities, []string{"x@1"}) {
		t.Fatalf("removed.capabilities = %v", res.Removed.Capabilities)
	}
	if !pbuCapDeleted(t, db, ns, "x", 1) {
		t.Fatal("x@1 应被软删")
	}
	if pbuCapDeleted(t, db, ns, "x", 2) {
		t.Fatal("同名手工行 x@2 被删了")
	}
	if _, err := db.GetCapability(ctx, ns, "x", 2); err != nil {
		t.Fatalf("手工行仍应可读：%v", err)
	}
}

// TestPlaybookUninstallKeepsOtherReleaseRow 另一次安装长出的新版本（upgraded）不删、记 other_release。
func TestPlaybookUninstallKeepsOtherReleaseRow(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-uninstall-other")
	pbCreate(t, db, ns, pbuPackage("pb-one"))
	pbuInstall(t, db, ns, "pb-one", "a", pbValues("one"))
	// 同包同能力、正文由 values 决定 → 正文不同 → 走 upgraded 建 x@2，归属 release b。
	pbuInstall(t, db, ns, "pb-one", "b", pbValues("two"))

	res := pbuUninstall(t, db, ns, "pb-one", "a", "")
	want := []store.PlaybookUninstallKept{{Kind: "capability", Name: "x", Version: 2, Reason: "other_release", OwnerRelease: "b"}}
	if !reflect.DeepEqual(res.Kept, want) {
		t.Fatalf("kept = %+v（want %+v）", res.Kept, want)
	}
	if !reflect.DeepEqual(res.Removed.Capabilities, []string{"x@1"}) {
		t.Fatalf("removed.capabilities = %v", res.Removed.Capabilities)
	}
	if pbuCapDeleted(t, db, ns, "x", 2) {
		t.Fatal("别的 release 的 x@2 被删了")
	}
	if !pbuCapDeleted(t, db, ns, "x", 1) {
		t.Fatal("x@1 应被软删")
	}
}

// TestPlaybookUninstallKeepsRowReusedByOtherPlaybookRelease 两个包共用 release 名时，被对方记为 skipped（复用）的行不删、
// 记 shared_by_release。
func TestPlaybookUninstallKeepsRowReusedByOtherPlaybookRelease(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-uninstall-shared")
	pbCreate(t, db, ns, pbuPackage("pb-one"))
	pbCreate(t, db, ns, pbuPackage("pb-two"))

	rb := pbuInstall(t, db, ns, "pb-one", "prod", pbValues("one"))
	second := pbuInstall(t, db, ns, "pb-two", "prod", pbValues("one"))
	if !reflect.DeepEqual(second.Skipped.Capabilities, []string{"x@1"}) {
		t.Fatalf("夹具前提不成立：pb-two 应复用 x@1，skipped = %v", second.Skipped.Capabilities)
	}

	res := pbuUninstall(t, db, ns, "pb-one", "prod", rb.Install.ID)
	want := []store.PlaybookUninstallKept{{Kind: "capability", Name: "x", Version: 1, Reason: "shared_by_release", OwnerRelease: "prod"}}
	if !reflect.DeepEqual(res.Kept, want) {
		t.Fatalf("kept = %+v（want %+v）", res.Kept, want)
	}
	if len(res.Removed.Capabilities) != 0 {
		t.Fatalf("被复用的行不该出现在 removed：%v", res.Removed.Capabilities)
	}
	if pbuCapDeleted(t, db, ns, "x", 1) {
		t.Fatal("被另一个生效 release 复用的 x@1 被删了")
	}
}

// TestPlaybookUninstallSecondCallIsZeroWrite 第二次卸载：alreadyUninstalled、库指纹逐字节相同。
func TestPlaybookUninstallSecondCallIsZeroWrite(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-uninstall-idem")
	pbCreate(t, db, ns, pbuPackage("pb-one"))
	pbuInstall(t, db, ns, "pb-one", "a", pbValues("one"))

	first := pbuUninstall(t, db, ns, "pb-one", "a", "")
	before := pbuFingerprint(t, db)
	second := pbuUninstall(t, db, ns, "pb-one", "a", "")
	if !second.AlreadyUninstalled {
		t.Fatalf("第二次卸载应 alreadyUninstalled：%+v", second)
	}
	if !reflect.DeepEqual(second.InstallIDs, first.InstallIDs) {
		t.Fatalf("installIds 变了：%v → %v", first.InstallIDs, second.InstallIDs)
	}
	if second.UninstalledAt != first.UninstalledAt {
		t.Fatalf("uninstalledAt 应沿用第一次：%q → %q", first.UninstalledAt, second.UninstalledAt)
	}
	if len(second.Removed.Capabilities) != 0 || len(second.Removed.Sources) != 0 {
		t.Fatalf("already 时 removed 应空：%+v", second.Removed)
	}
	if second.Kept == nil || len(second.Kept) != 0 {
		t.Fatalf("already 时 kept 应是空数组：%#v", second.Kept)
	}
	if after := pbuFingerprint(t, db); after != before {
		t.Fatalf("第二次卸载改了库：\n before %s\n after  %s", before, after)
	}
}

// TestPlaybookUninstallErrors release 不存在 / pin 对不上 / release 形状非法都 0 写。
func TestPlaybookUninstallErrors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-uninstall-err")
	pbCreate(t, db, ns, pbuPackage("pb-one"))
	pbuInstall(t, db, ns, "pb-one", "a", pbValues("one"))
	rB := pbuInstall(t, db, ns, "pb-one", "b", pbValues("two"))
	before := pbuFingerprint(t, db)

	if _, err := db.UninstallPlaybookRelease(ctx, ns, "pb-one", "missing", ""); !errors.Is(err, store.ErrPlaybookReleaseNotFound) {
		t.Fatalf("release 不存在应 ErrPlaybookReleaseNotFound，得到 %v", err)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, ns, "pb-one", "a", rB.Install.ID); !errors.Is(err, store.ErrPlaybookInstallMismatch) {
		t.Fatalf("installId 不属于该 release 应 ErrPlaybookInstallMismatch，得到 %v", err)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, ns, "pb-one", "Bad_Release", ""); playbook.Code(err) != playbook.CodeInvalidPlaybook {
		t.Fatalf("release 形状非法应 invalid_playbook，得到 %v", err)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, ns, "no-such-playbook", "a", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("包不存在应 ErrNotFound，得到 %v", err)
	}
	if after := pbuFingerprint(t, db); after != before {
		t.Fatalf("报错路径写了库：\n before %s\n after  %s", before, after)
	}
}

// TestPlaybookUninstallRebuildsProjection 投影：该名字没有可见行 → 边消失；还有同名可见行 → 边按它的 next 重建。
func TestPlaybookUninstallRebuildsProjection(t *testing.T) {
	db := openTestDB(t)
	nsGone := newNS(t, db, "pb-uninstall-edge-gone")
	nsKept := newNS(t, db, "pb-uninstall-edge-kept")

	p := pbuPackage("pb-one")
	p.files["capabilities/x.yaml"] = pbCapDoc(pbCapOpts{
		name: "x", next: []string{"y"}, runBody: "#!/usr/bin/env bash\necho '{{values.project}}'",
	})
	pbCreate(t, db, nsGone, p)
	pbCreate(t, db, nsKept, p)

	// ① 只剩本 release 的 x@1（next y）：卸载后没有可见行 → 边消失。
	pbuInstall(t, db, nsGone, "pb-one", "a", pbValues("one"))
	if got := pbuEdges(t, db, nsGone, "x"); !reflect.DeepEqual(got, []string{"y"}) {
		t.Fatalf("夹具前提：x 的边应是 [y]，得到 %v", got)
	}
	pbuUninstall(t, db, nsGone, "pb-one", "a", "")
	if got := pbuEdges(t, db, nsGone, "x"); len(got) != 0 {
		t.Fatalf("没有可见版本时边投影应消失，得到 %v", got)
	}

	// ② 同名手工行 x@1（next z）先建，本 release 长出 x@2（next y）：卸载 x@2 后投影必须回到 x@1 的 next。
	createCap(t, db, testCap(nsKept, "x", 0, []string{"z"}))
	pbuInstall(t, db, nsKept, "pb-one", "a", pbValues("one"))
	if got := pbuEdges(t, db, nsKept, "x"); !reflect.DeepEqual(got, []string{"y"}) {
		t.Fatalf("夹具前提：x@2 的边应是 [y]，得到 %v", got)
	}
	pbuUninstall(t, db, nsKept, "pb-one", "a", "")
	if got := pbuEdges(t, db, nsKept, "x"); !reflect.DeepEqual(got, []string{"z"}) {
		t.Fatalf("边应按幸存的最新可见行（x@1 → z）重建，得到 %v", got)
	}
	if latest := pbLatestCap(t, db, nsKept, "x"); latest.Version != 1 || latest.InstalledFrom != "" {
		t.Fatalf("幸存行应是手工 x@1：%+v", latest)
	}
}
