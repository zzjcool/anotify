package v4test

// #553：默认清单不含已卸载的组；历史清单带回来，且 uninstalledAt 是卸载时刻、对象引用为空。

import (
	"context"
	"testing"
	"time"
)

func TestPlaybookInstallsIncludeUninstalled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-inst-history")

	p := newPBPackage("pb-inst")
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "{{values.project}}-review", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "")
	row := pbCreate(t, db, ns, p)

	live, err := db.InstallPlaybook(ctx, ns, p.slug, "live", pbValues("a"), false)
	if err != nil {
		t.Fatalf("install live: %v", err)
	}
	gone, err := db.InstallPlaybook(ctx, ns, p.slug, "gone", pbValues("b"), false)
	if err != nil {
		t.Fatalf("install gone: %v", err)
	}
	if _, err := db.UninstallPlaybookRelease(ctx, ns, p.slug, "gone", ""); err != nil {
		t.Fatalf("uninstall gone: %v", err)
	}

	active, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if active.Total != 1 || len(active.Installs) != 1 || active.Installs[0].Release != "live" {
		t.Fatalf("默认清单应只剩生效中的 live：%+v", active.Installs)
	}
	if got := active.Installs[0].UninstalledAt; got != "" {
		t.Fatalf("生效中的组不得带 uninstalledAt，got %q", got)
	}
	if len(active.Installs[0].Capabilities) != 1 || len(active.Installs[0].Sources) != 1 {
		t.Fatalf("生效中的组仍要列对象引用：%+v", active.Installs[0])
	}

	all, err := db.ListPlaybookInstallsWithUninstalled(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs with uninstalled: %v", err)
	}
	if all.Total != 2 || len(all.Installs) != 2 {
		t.Fatalf("历史清单应有 live + gone 两组：%+v", all.Installs)
	}
	var seenLive, seenGone bool
	for _, group := range all.Installs {
		switch group.Release {
		case "live":
			seenLive = true
			if group.UninstalledAt != "" {
				t.Fatalf("live 是生效中的，不该有 uninstalledAt：%q", group.UninstalledAt)
			}
		case "gone":
			seenGone = true
			if _, err := time.Parse(time.RFC3339, group.UninstalledAt); err != nil {
				t.Fatalf("已卸载组的 uninstalledAt 应是 RFC3339，got %q: %v", group.UninstalledAt, err)
			}
			if group.LastInstallID != gone.Install.ID {
				t.Fatalf("已卸载组的 lastInstallId 应指向最后一次安装 %q，got %q", gone.Install.ID, group.LastInstallID)
			}
			if len(group.Capabilities) != 0 || len(group.Sources) != 0 {
				t.Fatalf("已卸载组的对象引用应如实为空，got caps=%v srcs=%v", group.Capabilities, group.Sources)
			}
		}
	}
	if !seenLive || !seenGone {
		t.Fatalf("历史清单缺组：live=%v gone=%v（%+v）", seenLive, seenGone, all.Installs)
	}
	if live.Install.ID == "" {
		t.Fatal("live 的回执应有 id")
	}

	groups, err := db.GetPlaybookReleaseGroups(ctx, ns, p.slug, "gone")
	if err != nil {
		t.Fatalf("release groups(gone): %v", err)
	}
	if !groups.Uninstalled || groups.UninstalledAt == 0 {
		t.Fatalf("gone 应被认成已卸载且带卸载时刻：%+v", groups)
	}
	if len(groups.Receipts) != 1 || groups.Receipts[0].Install.ID != gone.Install.ID {
		t.Fatalf("扁平回执列表应含那一次安装：%+v", groups.Receipts)
	}
}
