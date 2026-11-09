package v4test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 G1-5b 的**实例清单 / 回执历史 / 删包保留 / 引用安全 GC** 验收。
//
// 核心裁决（playbook.md §4.10 / §7 / §7.3 + 父 GC 裁决）：
//   - 实例清单是读模型：按 (namespace, 原 playbook id, valuesDigest) 折叠回执行，不建实例表。
//   - 删包只删清单：回执与已装对象保留，单条回执仍可按**历史 ns + 原 slug + installId** 读。
//   - 同 slug 重建是**新清单（新 id）**：旧 group 不能出现在新包的实例里，来源也不能混。
//   - 回执不跟事件 30 天 TTL 一起删；显式 GC 只删「没有任何 installed_from 引用」的行。

// pbInsertRawInstall 直接插一行回执（造历史 / 上限夹具用；生产路径是 InstallPlaybook）。
func pbInsertRawInstall(t *testing.T, db *store.DB, nsID, playbookID, id, valuesDigest string, createdAt int64) {
	t.Helper()
	receipt := fmt.Sprintf(
		`{"apiVersion":"anotify/v1","kind":"InstallReceipt","playbook":{"name":"project-chain"},"install":{"id":%q,"at":"2026-11-01T00:00:00Z","dryRun":false},"namespace":"x","values":{},"valuesDigest":%q,"created":{"capabilities":[],"sources":[]},"skipped":{"capabilities":[],"sources":[]},"upgraded":[],"unapplied":[],"external":[],"pending":[],"notes":[]}`,
		id, valuesDigest)
	if _, err := db.Exec(
		`INSERT INTO playbook_installs
		   (id, namespace_id, namespace_slug, playbook_id, playbook_slug, "values", values_digest, receipt, source, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, nsID, "x", playbookID, "project-chain", "{}", valuesDigest, receipt, "", createdAt); err != nil {
		t.Fatalf("insert raw install %s: %v", id, err)
	}
}

// TestPlaybookInstallsFoldAttemptsAndUnion 验折叠读模型：
// 同 values 装两次 = 1 组 attempts=2，capabilities 是两次 install id 的 installed_from 并集。
func TestPlaybookInstallsFoldAttemptsAndUnion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-fold")

	// 第一个包只有 review + source（review 的 next 指向尚未存在的 fixer）。
	p := newPBPackage("project-chain")
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:        "{{values.project}}-review",
		displayName: "{{values.project}} 审查",
		params:      "    type: object\n    properties: { repo: { type: string } }",
		next:        []string{"{{values.project}}-fixer"},
		runBody:     "#!/usr/bin/env bash\necho \"project={{values.project}}\"",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "")
	row := pbCreate(t, db, ns, p)
	pbInstall(t, db, ns, p.slug, pbValues("a"))

	// 补上 fixer：同一组 values 再装 → 新回执 + 新行（补缺），旧行复用。
	p.caps = append(p.caps, "capabilities/fixer.yaml")
	p.files["capabilities/fixer.yaml"] = pbCapDoc(pbCapOpts{
		name: "{{values.project}}-fixer", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	pbReplace(t, db, ns, p, row.ResourceVersion)
	second := pbInstall(t, db, ns, p.slug, pbValues("a"))
	pbCreatedHas(t, second.Created.Capabilities, "a-fixer@1", "created.capabilities")

	list, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if list.Total != 1 || len(list.Installs) != 1 {
		t.Fatalf("同 values 装两次应折叠成 1 组：total=%d len=%d", list.Total, len(list.Installs))
	}
	g := list.Installs[0]
	if g.Attempts != 2 {
		t.Fatalf("attempts 应为 2，得到 %d", g.Attempts)
	}
	if g.LastInstallID != second.Install.ID {
		t.Fatalf("lastInstallId 应是最近一次：%q（want %q）", g.LastInstallID, second.Install.ID)
	}
	if g.FirstAt == "" || g.LastAt == "" {
		t.Fatalf("firstAt/lastAt 不能为空：%+v", g)
	}
	// 并集：review 来自第一份回执，fixer 来自第二份。
	if !reflect.DeepEqual(g.Capabilities, []string{"a-fixer@1", "a-review@1"}) {
		t.Fatalf("组内能力并集不对：%v", g.Capabilities)
	}
	if !reflect.DeepEqual(g.Sources, []string{"a-on-push"}) {
		t.Fatalf("组内源不对：%v", g.Sources)
	}
	if g.Values["project"] != "a" || g.Values["env"] != "prod" {
		t.Fatalf("组 values 应是打码后的有效 values：%v", g.Values)
	}
}

// TestPlaybookInstallsOrderAndLimit 验：组按 lastAt 降序、上限 200、total 是折叠组总数。
func TestPlaybookInstallsOrderAndLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-limit")
	p := newPBPackage("project-chain").withDefault()
	row := pbCreate(t, db, ns, p)

	base := store.Now() - 10000
	const groups = 205
	for i := 0; i < groups; i++ {
		id := fmt.Sprintf("ins_raw_%03d", i)
		pbInsertRawInstall(t, db, ns, row.ID, id, fmt.Sprintf("sha256:%064d", i), base+int64(i))
	}
	list, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if list.Total != groups {
		t.Fatalf("total 应是折叠组总数 %d，得到 %d", groups, list.Total)
	}
	if len(list.Installs) != 200 {
		t.Fatalf("一次最多 200 组，得到 %d", len(list.Installs))
	}
	// 最新（i=204）在最前。
	if list.Installs[0].LastInstallID != fmt.Sprintf("ins_raw_%03d", groups-1) {
		t.Fatalf("排序不是 lastAt 降序：首组 = %q", list.Installs[0].LastInstallID)
	}
	if list.Installs[199].LastInstallID != fmt.Sprintf("ins_raw_%03d", groups-200) {
		t.Fatalf("截断位置不对：第 200 组 = %q", list.Installs[199].LastInstallID)
	}
	for i, g := range list.Installs {
		if g.Attempts != 1 {
			t.Fatalf("每组 attempts 应为 1：第 %d 组 = %d", i, g.Attempts)
		}
	}
}

// TestPlaybookInstallsEmpty 验空列表是 [] 不是 null；未知回执是 ErrNotFound。
func TestPlaybookInstallsEmpty(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-empty")
	row := pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	list, err := db.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if list.Total != 0 || list.Installs == nil || len(list.Installs) != 0 {
		t.Fatalf("空实例清单应是 total=0 + installs=[]：%+v", list)
	}
	if _, err := db.GetPlaybookInstall(ctx, ns, "project-chain", "ins_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知回执应 ErrNotFound，得到 %v", err)
	}
}

// TestDeletePlaybookKeepsReceiptsAndResources 验删包：只删清单，kept/values 汇总正确，
// 回执与已装对象保留、单条回执仍可读（历史 ns + 原 slug）。
func TestDeletePlaybookKeepsReceiptsAndResources(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-del")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	secret := "slack-secret-value"
	rA1 := pbInstall(t, db, ns, "project-chain", map[string]any{"project": "a", "slackToken": secret})
	rA2 := pbInstall(t, db, ns, "project-chain", map[string]any{"project": "a", "slackToken": secret})
	rB := pbInstall(t, db, ns, "project-chain", pbValues("b"))
	capsBefore := pbCapCount(t, db, ns, "a-review") + pbCapCount(t, db, ns, "b-review")
	srcBefore := pbSourceCount(t, db, ns, "a-on-push") + pbSourceCount(t, db, ns, "b-on-push")

	// CAS 哨兵：缺 rv / 旧 rv 都不能删。
	if _, err := db.DeletePlaybook(ctx, ns, "project-chain", 0); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("缺 rv 应 ErrResourceVersionRequired，得到 %v", err)
	}
	if _, err := db.DeletePlaybook(ctx, ns, "project-chain", 9999); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("旧 rv 应 ErrResourceVersionConflict，得到 %v", err)
	}
	rv := pbCreateRV(t, db, ns, "project-chain")
	res, err := db.DeletePlaybook(ctx, ns, "project-chain", rv)
	if err != nil {
		t.Fatalf("delete playbook: %v", err)
	}
	if !res.Deleted || res.Note == "" {
		t.Fatalf("删除响应形状不对：%+v", res)
	}
	wantCaps := []string{"a-fixer@1", "a-review@1", "b-fixer@1", "b-review@1"}
	if !reflect.DeepEqual(res.Kept.Capabilities, wantCaps) {
		t.Fatalf("kept.capabilities = %v（want %v）", res.Kept.Capabilities, wantCaps)
	}
	if !reflect.DeepEqual(res.Kept.Sources, []string{"a-on-push", "b-on-push"}) {
		t.Fatalf("kept.sources = %v", res.Kept.Sources)
	}
	if len(res.Values) != 2 {
		t.Fatalf("values 应按 valuesDigest 去重汇总成 2 组：%v", res.Values)
	}
	if pbJSONHas(t, res.Values, secret) {
		t.Fatalf("kept values 泄露了 writeOnly 明文")
	}

	// 清单没了，对象与回执都在。
	if _, err := db.GetPlaybook(ctx, ns, "project-chain"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删后 GetPlaybook 应 ErrNotFound，得到 %v", err)
	}
	if got := pbCapCount(t, db, ns, "a-review") + pbCapCount(t, db, ns, "b-review"); got != capsBefore {
		t.Fatalf("删包不该删能力：%d → %d", capsBefore, got)
	}
	if got := pbSourceCount(t, db, ns, "a-on-push") + pbSourceCount(t, db, ns, "b-on-push"); got != srcBefore {
		t.Fatalf("删包不该删源：%d → %d", srcBefore, got)
	}
	for _, id := range []string{rA1.Install.ID, rA2.Install.ID, rB.Install.ID} {
		if !rowExists(t, db, "playbook_installs", id) {
			t.Fatalf("删包必须保留回执 %s", id)
		}
	}
	got, err := db.GetPlaybookInstall(ctx, ns, "project-chain", rA2.Install.ID)
	if err != nil {
		t.Fatalf("删包后单条回执应仍可读：%v", err)
	}
	if got.Values["slackToken"] != playbook.MaskedValue || got.Values["project"] != "a" {
		t.Fatalf("历史回执 values 应保持打码：%v", got.Values)
	}
	if got.Playbook.Name != "project-chain" {
		t.Fatalf("回执里的包身份应是快照：%+v", got.Playbook)
	}
	// 清单删了不能再删第二次。
	if _, err := db.DeletePlaybook(ctx, ns, "project-chain", rv); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("再次删除应 ErrNotFound，得到 %v", err)
	}
}

// TestPlaybookRecreateSameSlugDoesNotInheritHistory 验同 slug 重建（新 id）：
// 新包的实例清单看不到旧 group，旧回执仍按旧 slug 可读，来源不混。
func TestPlaybookRecreateSameSlugDoesNotInheritHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-recreate")

	oldPkg := newPBPackage("project-chain").withDefault()
	oldRow := pbCreate(t, db, ns, oldPkg)
	oldReceipt := pbInstall(t, db, ns, "project-chain", pbValues("a"))
	if _, err := db.DeletePlaybook(ctx, ns, "project-chain", oldRow.ResourceVersion); err != nil {
		t.Fatalf("delete old playbook: %v", err)
	}

	// 新包同 slug、不同包体（review 正文变了 → 装出新版本；fixer 内容相同 → 复用）。
	newPkg := newPBPackage("project-chain").withDefault()
	newPkg.version = 4
	newPkg.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:        "{{values.project}}-review",
		displayName: "{{values.project}} 审查",
		params:      "    type: object\n    properties: { repo: { type: string } }",
		next:        []string{"{{values.project}}-fixer"},
		runBody:     "#!/usr/bin/env bash\necho v2",
	})
	newRow := pbCreate(t, db, ns, newPkg)
	if newRow.ID == oldRow.ID {
		t.Fatalf("重建应得到新 id（旧 id 是历史，不是复用）")
	}

	list, err := db.ListPlaybookInstalls(ctx, ns, newRow.ID)
	if err != nil {
		t.Fatalf("list new installs: %v", err)
	}
	if list.Total != 0 || len(list.Installs) != 0 {
		t.Fatalf("新清单不该继承旧 group：%+v", list)
	}

	r := pbInstall(t, db, ns, "project-chain", pbValues("a"))
	pbCreatedHas(t, r.Created.Capabilities, "a-review@2", "created.capabilities")
	list, err = db.ListPlaybookInstalls(ctx, ns, newRow.ID)
	if err != nil {
		t.Fatalf("list new installs after install: %v", err)
	}
	if list.Total != 1 {
		t.Fatalf("新包应只有自己的 1 组，得到 %d", list.Total)
	}
	g := list.Installs[0]
	if !reflect.DeepEqual(g.Capabilities, []string{"a-review@2"}) {
		t.Fatalf("新组的来源只能来自新回执（fixer 是复用旧行、不出现）：%v", g.Capabilities)
	}
	for _, name := range g.Capabilities {
		if strings.HasSuffix(name, "@1") {
			t.Fatalf("新组混入了旧实例来源：%v", g.Capabilities)
		}
	}

	// 旧 group 仍在旧 id 下（历史不被删改），旧单回执仍可读。
	oldList, err := db.ListPlaybookInstalls(ctx, ns, oldRow.ID)
	if err != nil {
		t.Fatalf("list old installs: %v", err)
	}
	if oldList.Total != 1 || !reflect.DeepEqual(oldList.Installs[0].Capabilities,
		[]string{"a-fixer@1", "a-review@1"}) {
		t.Fatalf("旧实例清单被改动：%+v", oldList)
	}
	got, err := db.GetPlaybookInstall(ctx, ns, "project-chain", oldReceipt.Install.ID)
	if err != nil {
		t.Fatalf("旧回执应仍可按旧 slug 读：%v", err)
	}
	if got.Install.ID != oldReceipt.Install.ID {
		t.Fatalf("读到别的回执：%s", got.Install.ID)
	}
}

// TestPlaybookReceiptGCReferenceSafety 验 GC：自动 30 天 sweeper 不删回执；
// 显式 primitive 在 limit<=0 时是 no-op、被引用的回执永不删、未引用且过期才删。
func TestPlaybookReceiptGCReferenceSafety(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-gc")
	row := pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())
	r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

	// 自动 sweeper：event/task TTL 很小也不动回执。
	setRetention(t, db, 1, 1, 0)
	if _, err := db.RunRetention(ctx, store.Now()+100000); err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if !rowExists(t, db, "playbook_installs", r.Install.ID) {
		t.Fatalf("自动 sweeper 删掉了回执（父裁决禁止）")
	}

	// limit<=0 是 no-op（少删好于删全表）。
	if n, err := db.DeleteExpiredPlaybookInstalls(ctx, store.Now()+100000, 0); err != nil || n != 0 {
		t.Fatalf("limit=0 应 no-op：n=%d err=%v", n, err)
	}
	// 被 installed_from 引用的回执：即使 cutoff 覆盖也删不掉。
	if n, err := db.DeleteExpiredPlaybookInstalls(ctx, store.Now()+100000, 100); err != nil || n != 0 {
		t.Fatalf("被引用的回执不该删：n=%d err=%v", n, err)
	}

	// 插一条「未引用且过期」的回执：显式 GC 应删掉它（primitive 不是 no-op）。
	pbInsertRawInstall(t, db, ns, row.ID, "ins_orphan_old", "sha256:orphan", store.Now()-100000)
	n, err := db.DeleteExpiredPlaybookInstalls(ctx, store.Now()-1000, 100)
	if err != nil {
		t.Fatalf("delete expired installs: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删掉 1 条未引用的过期回执，得到 %d", n)
	}
	if rowExists(t, db, "playbook_installs", "ins_orphan_old") {
		t.Fatalf("未引用且过期的回执应被删")
	}
	if !rowExists(t, db, "playbook_installs", r.Install.ID) {
		t.Fatalf("被引用的回执被误删")
	}
}

// TestPlaybookGCConcurrentWithInstall 验「查引用 + 删除同一事务」在并发安装下的安全性：
// 结束后不存在指向不存在回执的 installed_from。
func TestPlaybookGCConcurrentWithInstall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-hist-gcrace")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	var wg sync.WaitGroup
	start := make(chan struct{})
	var installErr, gcErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, installErr = db.InstallPlaybook(ctx, ns, "project-chain", pbValues("a"), false)
	}()
	go func() {
		defer wg.Done()
		<-start
		// cutoff 覆盖「刚刚创建」的回执：只有引用保护能拦住删除。
		_, gcErr = db.DeleteExpiredPlaybookInstalls(ctx, store.Now()+3600, 100)
	}()
	close(start)
	wg.Wait()
	if installErr != nil {
		t.Fatalf("并发 install 失败：%v", installErr)
	}
	if gcErr != nil {
		t.Fatalf("并发 GC 失败：%v", gcErr)
	}
	// 不变式：所有 installed_from 都指向仍存在的回执。
	var dangling int
	if err := db.QueryRow(
		`SELECT (SELECT count(*) FROM capability_versions cv
		          WHERE cv.installed_from <> '' AND NOT EXISTS
		                (SELECT 1 FROM playbook_installs pi WHERE pi.id = cv.installed_from))
		      + (SELECT count(*) FROM sources s
		          WHERE s.installed_from <> '' AND NOT EXISTS
		                (SELECT 1 FROM playbook_installs pi WHERE pi.id = s.installed_from))`).Scan(&dangling); err != nil {
		t.Fatalf("probe dangling installed_from: %v", err)
	}
	if dangling != 0 {
		t.Fatalf("出现悬空 installed_from：%d", dangling)
	}
}
