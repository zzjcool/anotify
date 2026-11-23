package store

// 预案实例的分组读数（#548）与按组导出的读面。
//
// 这里只验两件事实：折叠键 `(release, valuesDigest)` 划出的组边界，以及
// 「全部卸载」与「从未存在」都能被调用方区分开（前者 404 走到另一条分支）。

import (
	"context"
	"errors"
	"testing"
)

// TestGetPlaybookReleaseGroupsFoldsByValues 一个 release 的同一套 values 装两次 = 1 组 attempts=2。
func TestGetPlaybookReleaseGroupsFoldsByValues(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-detail", pbBundle("pr-detail"))

	var lastID string
	for i := 0; i < 2; i++ {
		receipt, err := db.InstallPlaybook(ctx, nsID, "pr-detail", "blue", pbValues("demo"), false)
		if err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
		lastID = receipt.Install.ID
	}

	groups, err := db.GetPlaybookReleaseGroups(ctx, nsID, "pr-detail", "blue")
	if err != nil {
		t.Fatalf("get release groups: %v", err)
	}
	if groups.Release != "blue" {
		t.Fatalf("release = %q, want blue", groups.Release)
	}
	if groups.Uninstalled {
		t.Fatal("装过两次的 release 不该被判成已卸载")
	}
	if len(groups.Groups) != 1 {
		t.Fatalf("同一套 values 装两次应是 1 组, got %d", len(groups.Groups))
	}
	group := groups.Groups[0]
	if group.Attempts != 2 || len(group.Receipts) != 2 {
		t.Fatalf("组内应是 2 次安装, got attempts=%d receipts=%d", group.Attempts, len(group.Receipts))
	}
	if group.LastInstallID != lastID {
		t.Fatalf("lastInstallId = %q, want 最近一次 %q", group.LastInstallID, lastID)
	}
	if group.ValuesDigest == "" || group.FirstAt == "" || group.LastAt == "" {
		t.Fatalf("组的关键字段不该为空: %+v", group)
	}
	if group.Values["project"] != "demo" {
		t.Fatalf("values 应原样带回（打码后的那份）: %+v", group.Values)
	}

	// 另一套 values = 另一组（折叠键含 valuesDigest）
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-detail", "blue", pbValues("other"), false); err != nil {
		t.Fatalf("install other values: %v", err)
	}
	groups, err = db.GetPlaybookReleaseGroups(ctx, nsID, "pr-detail", "blue")
	if err != nil {
		t.Fatalf("get release groups again: %v", err)
	}
	if len(groups.Groups) != 2 {
		t.Fatalf("两套 values 应是 2 组, got %d", len(groups.Groups))
	}
	// 按最近安装降序：刚装的 other 在前。
	if groups.Groups[0].Values["project"] != "other" {
		t.Fatalf("第一组应是最近装的 other, got %+v", groups.Groups[0].Values)
	}
	if groups.Groups[1].Attempts != 2 {
		t.Fatalf("demo 组应是 2 次, got %d", groups.Groups[1].Attempts)
	}
}

// TestGetPlaybookReleaseGroupsNotReleased：别的 release 的回执不算这个 release 的。
func TestGetPlaybookReleaseGroupsReleaseIsBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-bound", pbBundle("pr-bound"))
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-bound", "blue", pbValues("demo"), false); err != nil {
		t.Fatalf("install blue: %v", err)
	}

	_, err := db.GetPlaybookReleaseGroups(ctx, nsID, "pr-bound", "green")
	if !errors.Is(err, ErrPlaybookReleaseNotFound) {
		t.Fatalf("不存在的 release 应 ErrPlaybookReleaseNotFound, got %v", err)
	}
}

// TestGetPlaybookReleaseGroupsUninstalled：全部回执都打了卸载标记 → Uninstalled（调用方据此回 404）。
func TestGetPlaybookReleaseGroupsUninstalled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-gone", pbBundle("pr-gone"))
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-gone", "blue", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, nsID, "pr-gone", "blue", ""); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	groups, err := db.GetPlaybookReleaseGroups(ctx, nsID, "pr-gone", "blue")
	if err != nil {
		t.Fatalf("已卸载的 release 仍应有回执可读: %v", err)
	}
	if !groups.Uninstalled {
		t.Fatal("全部回执已卸载 → Uninstalled 必须为真")
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Attempts != 1 {
		t.Fatalf("回执要保留作审计, got %+v", groups.Groups)
	}
}

// TestGetPlaybookReleaseExportGroupFiltersByDigest 按参数组导出：只拿这一组的回执。
func TestGetPlaybookReleaseExportGroupFiltersByDigest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-export", pbBundle("pr-export"))

	demo, err := db.InstallPlaybook(ctx, nsID, "pr-export", "blue", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install demo: %v", err)
	}
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-export", "blue", pbValues("other"), false); err != nil {
		t.Fatalf("install other: %v", err)
	}

	all, err := db.GetPlaybookReleaseExport(ctx, nsID, "pr-export", "blue")
	if err != nil {
		t.Fatalf("export all: %v", err)
	}
	if len(all.Receipts) != 2 {
		t.Fatalf("不带摘要的导出应是全量, got %d", len(all.Receipts))
	}

	only, err := db.GetPlaybookReleaseExportGroup(ctx, nsID, "pr-export", "blue", demo.ValuesDigest)
	if err != nil {
		t.Fatalf("export group: %v", err)
	}
	if len(only.Receipts) != 1 || only.Receipts[0].ValuesDigest != demo.ValuesDigest {
		t.Fatalf("按组导出应只回这一组, got %+v", only.Receipts)
	}
	if len(only.Receipts[0].Created.Capabilities) != 1 || only.Receipts[0].Created.Capabilities[0] != "demo-review@1" {
		t.Fatalf("这一组装出的能力应是 demo-review@1, got %+v", only.Receipts[0].Created.Capabilities)
	}

	// 不存在的摘要 → 与「没有这个 release」同一个 404 语义（不静默退回全量）。
	if _, err := db.GetPlaybookReleaseExportGroup(ctx, nsID, "pr-export", "blue", "sha256:nope"); !errors.Is(err, ErrPlaybookReleaseNotFound) {
		t.Fatalf("未知摘要应 ErrPlaybookReleaseNotFound, got %v", err)
	}
}
