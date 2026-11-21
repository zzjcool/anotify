package store

// 包版本历史的数据层验收：写侧「内容变了才追加」、读侧「按版本取 files」、回执「哪一版 + 那份文件的摘要」，以及老包（不传版本）的对照。 版本夹具只改 run.sh 正文（digest 因此一定不同），「按版本装」的判据落在装出来的能力脚本上，不是人眼比文件。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
)

// pbBundleAt 是 pbBundle 的第 n 版：只有能力脚本正文不同，名字带 `{{release.name}}`（多 release 并存要靠它）。
func pbBundleAt(slug string, version int, script string) map[string]string {
	capPath := "capabilities/review.yaml"
	srcPath := "sources/on-push.yaml"
	return map[string]string{
		"playbook.yaml": pbEntry(slug, version, []string{capPath}, []string{srcPath}),
		capPath:         pbCapabilityDoc("{{release.name}}-{{values.project}}-review", nil, "#!/bin/sh\n"+script),
		srcPath:         pbSourceDoc("{{release.name}}-{{values.project}}-on-push", []string{"{{release.name}}-{{values.project}}-review"}),
	}
}

// pbVersionRow 是历史表的一行（摘要 + 谁写的：用来判断「这一行有没有被重写」）。
type pbVersionRow struct {
	Digest    string
	CreatedBy string
}

func pbVersionRows(t *testing.T, db *DB, playbookID string) map[int]pbVersionRow {
	t.Helper()
	rows, err := db.Query(`SELECT version, digest, created_by, files FROM playbook_versions WHERE playbook_id = ? ORDER BY version`, playbookID)
	if err != nil {
		t.Fatalf("read playbook_versions: %v", err)
	}
	defer rows.Close()
	out := map[int]pbVersionRow{}
	for rows.Next() {
		var (
			version   int
			digest    string
			createdBy string
			files     string
		)
		if err := rows.Scan(&version, &digest, &createdBy, &files); err != nil {
			t.Fatalf("scan playbook_versions: %v", err)
		}
		var decoded map[string]string
		if err := json.Unmarshal([]byte(files), &decoded); err != nil {
			t.Fatalf("decode version %d files: %v", version, err)
		}
		if digest != playbook.FilesDigest(decoded) {
			t.Fatalf("版本 %d 的 digest 与 files 不同源: %s vs %s", version, digest, playbook.FilesDigest(decoded))
		}
		out[version] = pbVersionRow{Digest: digest, CreatedBy: createdBy}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate playbook_versions: %v", err)
	}
	return out
}

// TestPlaybookVersionsRecordedOnlyOnContentChange：建包记一行；同内容 PUT 不追加；内容变了追加；同版本号改了内容则覆盖那一行。
func TestPlaybookVersionsRecordedOnlyOnContentChange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "pr-review"

	first := pbBundleAt(slug, 1, `echo v1`)
	row, err := db.CreatePlaybookAs(ctx, nsID, slug, first, "", "usr_a")
	if err != nil {
		t.Fatalf("create playbook: %v", err)
	}
	if got := pbVersionRows(t, db, row.ID); len(got) != 1 {
		t.Fatalf("建包后历史行数 = %d, want 1（建包就是历史的第 1 行）: %v", len(got), got)
	}
	v1Digest := playbook.FilesDigest(first)

	// 换个人保存同一份内容：一行都不写，已有的那一行也不重写（作者仍是建包的人）。
	if _, err := db.ReplacePlaybookAs(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug), first, "", "usr_b"); err != nil {
		t.Fatalf("same-content PUT: %v", err)
	}
	if got := pbVersionRows(t, db, row.ID); len(got) != 1 || got[1].Digest != v1Digest || got[1].CreatedBy != "usr_a" {
		t.Fatalf("同内容 PUT 不该追加、也不该重写历史行: %v", got)
	}

	second := pbBundleAt(slug, 2, `echo v2`)
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug), second, ""); err != nil {
		t.Fatalf("changed PUT: %v", err)
	}
	rows := pbVersionRows(t, db, row.ID)
	if len(rows) != 2 || rows[2].Digest != playbook.FilesDigest(second) {
		t.Fatalf("内容变化必须追加历史: %v", rows)
	}
	if rows[1].Digest != v1Digest {
		t.Fatalf("v1 的 digest 被改写了: %v", rows)
	}

	third := pbBundleAt(slug, 2, `echo v2-fixed`)
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug), third, ""); err != nil {
		t.Fatalf("same-version PUT: %v", err)
	}
	rows = pbVersionRows(t, db, row.ID)
	if len(rows) != 2 {
		t.Fatalf("同版本号改内容不该长出新行: %v", rows)
	}
	if rows[2].Digest != playbook.FilesDigest(third) {
		t.Fatalf("同版本号改内容应覆盖该行的 digest: %v", rows)
	}
}

// TestInstallPlaybookVersionPinsFiles：v1 装出去之后包改成 v2，再按 v1 装另一份 release —— 两份实例各自拿到自己那一版的脚本。
func TestInstallPlaybookVersionPinsFiles(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "pinned"

	v1 := pbBundleAt(slug, 1, `echo v1`)
	pbCreate(t, db, nsID, slug, v1)
	firstInstall, err := db.InstallPlaybook(ctx, nsID, slug, "a", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install v1（当前版本）: %v", err)
	}
	if firstInstall.Playbook.Version != 1 || firstInstall.Playbook.FilesDigest != playbook.FilesDigest(v1) {
		t.Fatalf("v1 回执的包身份 = %+v", firstInstall.Playbook)
	}

	v2 := pbBundleAt(slug, 2, `echo v2`)
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug), v2, ""); err != nil {
		t.Fatalf("replace to v2: %v", err)
	}

	pinned, err := db.InstallPlaybookVersion(ctx, nsID, slug, "b", 1, pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install pinned v1: %v", err)
	}
	if pinned.Playbook.Version != 1 || pinned.Playbook.FilesDigest != playbook.FilesDigest(v1) {
		t.Fatalf("按 v1 装的回执包身份 = %+v（want version 1 + v1 digest）", pinned.Playbook)
	}
	if pinned.Playbook.FilesDigest == playbook.FilesDigest(v2) {
		t.Fatal("按 v1 装却拿到了 v2 的 files digest（版本没被钉住）")
	}
	current, err := db.InstallPlaybookVersion(ctx, nsID, slug, "c", 2, pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install pinned v2: %v", err)
	}
	if current.Playbook.Version != 2 || current.Playbook.FilesDigest != playbook.FilesDigest(v2) {
		t.Fatalf("按 v2 装的回执包身份 = %+v", current.Playbook)
	}

	v1Body := "echo v1"
	v2Body := "echo v2"
	for _, tc := range []struct{ release, body string }{{"b", v1Body}, {"c", v2Body}} {
		cap, err := db.GetCapability(ctx, nsID, tc.release+"-demo-review", 1)
		if err != nil {
			t.Fatalf("release %s 的能力行: %v", tc.release, err)
		}
		if got := cap.Spec.Run.Files["run.sh"]; !containsLine(got, tc.body) {
			t.Fatalf("release %s 装的是 %q，want 含 %q", tc.release, got, tc.body)
		}
	}

	for _, want := range []struct {
		id      string
		version int
		digest  string
	}{{pinned.Install.ID, 1, playbook.FilesDigest(v1)}, {current.Install.ID, 2, playbook.FilesDigest(v2)}} {
		doc, err := db.GetPlaybookInstall(ctx, nsID, slug, want.id)
		if err != nil {
			t.Fatalf("get install %s: %v", want.id, err)
		}
		if doc.Playbook.Version != want.version || doc.Playbook.FilesDigest != want.digest {
			t.Fatalf("单回执 %s 的包身份 = %+v，want v%d/%s", want.id, doc.Playbook, want.version, want.digest)
		}
	}
}

// TestInstallPlaybookVersionNotFound：钉一个不存在的版本必须报错，不许静默装当前版本。
func TestInstallPlaybookVersionNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "ghost-version", pbBundleAt("ghost-version", 1, `echo v1`))

	if _, err := db.InstallPlaybookVersion(ctx, nsID, "ghost-version", "", 9, pbValues("demo"), false); !errors.Is(err, ErrPlaybookVersionNotFound) {
		t.Fatalf("装不存在的版本应返回 ErrPlaybookVersionNotFound，实际 %v", err)
	}
	if _, err := db.RenderPlaybookVersion(ctx, nsID, "ghost-version", "", 9, pbValues("demo")); !errors.Is(err, ErrPlaybookVersionNotFound) {
		t.Fatalf("渲染不存在的版本应返回 ErrPlaybookVersionNotFound，实际 %v", err)
	}
	if got := pbInstalls(t, db); got != 0 {
		t.Fatalf("版本不存在的安装写了 %d 行回执（应 0 写）", got)
	}
}

// TestInstallPlaybookDefaultVersionMatchesCurrentPin：不传版本与「显式钉住当前版本」逐字同形。
// 这是「老包不回归」的可执行判据（不是「我记得没改」）：两份包体相同，比包身份与计划。
func TestInstallPlaybookDefaultVersionMatchesCurrentPin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// 两个命名空间各一份同名包：能力名相同也不会互相认领，所以两次安装的 created 可以逐字比较。
	bundle := func(slug string) map[string]string { return pbBundleAt(slug, 1, `echo same`) }
	nsLegacy, _ := newTestNS(t, db)
	nsPinned, _ := newTestNS(t, db)
	pbCreate(t, db, nsLegacy, "same-pkg", bundle("same-pkg"))
	pbCreate(t, db, nsPinned, "same-pkg", bundle("same-pkg"))

	legacy, err := db.InstallPlaybook(ctx, nsLegacy, "same-pkg", "", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install（不传版本）: %v", err)
	}
	pinned, err := db.InstallPlaybookVersion(ctx, nsPinned, "same-pkg", "", 1, pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install（钉住当前版本）: %v", err)
	}
	if legacy.Playbook.Version != pinned.Playbook.Version || legacy.Playbook.FilesDigest != pinned.Playbook.FilesDigest {
		t.Fatalf("包身份不同: %+v vs %+v", legacy.Playbook, pinned.Playbook)
	}
	if a, b := mustJSON(t, legacy.Created), mustJSON(t, pinned.Created); a != b {
		t.Fatalf("created 不同:\n legacy %s\n pinned %s", a, b)
	}
	if a, b := mustJSON(t, legacy.Values), mustJSON(t, pinned.Values); a != b {
		t.Fatalf("values 不同: %s vs %s", a, b)
	}
	if legacy.Install.Release != "same-pkg" || pinned.Install.Release != "same-pkg" {
		t.Fatalf("缺省 release 应等于包名: %q / %q", legacy.Install.Release, pinned.Install.Release)
	}
}

// TestListPlaybookVersionsMarksCurrent：读模型只回身份与摘要，当前版本那一行带标记；迁移前的老包没有历史行时 Total=0。
func TestListPlaybookVersionsMarksCurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	slug := "version-list"
	row := pbCreate(t, db, nsID, slug, pbBundleAt(slug, 1, `echo v1`))
	if _, err := db.ReplacePlaybook(ctx, nsID, slug, pbVersionToken(t, db, nsID, slug), pbBundleAt(slug, 2, `echo v2`), ""); err != nil {
		t.Fatalf("replace to v2: %v", err)
	}

	list, err := db.ListPlaybookVersions(ctx, nsID, row.ID, 2)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if list.CurrentVersion != 2 || list.Total != 2 || len(list.Versions) != 2 {
		t.Fatalf("版本列表 = %+v", list)
	}
	if list.Versions[0].Version != 2 || !list.Versions[0].Current {
		t.Fatalf("最新的那一行应是当前版本（version DESC）: %+v", list.Versions)
	}
	if list.Versions[1].Version != 1 || list.Versions[1].Current {
		t.Fatalf("v1 不该被标成当前: %+v", list.Versions)
	}
	if list.Versions[1].CreatedAt == "" {
		t.Fatalf("createdAt 必须是 RFC3339（空串会让前端显示 1970）: %+v", list.Versions[1])
	}

	other, _ := newTestNS(t, db)
	if foreign, err := db.ListPlaybookVersions(ctx, other, row.ID, 2); err != nil || foreign.Total != 0 {
		t.Fatalf("跨命名空间读历史 = %+v, %v", foreign, err)
	}

	if _, err := db.Exec(`INSERT INTO playbooks (id, namespace_id, slug, name, version, files, resource_version, created_at, updated_at)
		VALUES ('pb_legacy_rows', ?, 'legacy-noversions', 'legacy-noversions', 1, ?, 1, 1, 1)`,
		nsID, mustJSON(t, pbBundleAt("legacy-noversions", 1, `echo old`))); err != nil {
		t.Fatalf("insert legacy playbook: %v", err)
	}
	legacyList, err := db.ListPlaybookVersions(ctx, nsID, "pb_legacy_rows", 1)
	if err != nil || legacyList.Total != 0 {
		t.Fatalf("老包的版本列表 = %+v, %v（应为空）", legacyList, err)
	}
	if _, err := db.InstallPlaybookVersion(ctx, nsID, "legacy-noversions", "", 1, pbValues("demo"), false); err != nil {
		t.Fatalf("老包按当前版本号装应成功（回退到 playbooks.files）: %v", err)
	}
}

// mustJSON 把任意值拍成稳定字符串（比较回执片段用）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// containsLine 报告 body 里是否有一行等于 want（脚本按行比，避免子串误判）。
func containsLine(body, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
