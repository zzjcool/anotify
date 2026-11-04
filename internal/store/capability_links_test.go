package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 本文件钉住「能力 + 下游边（+ 产物）一次提交」的**原子性**。
//
// 验收的核心不是「成功时能建出来」（那太容易），而是**失败时什么都没留下**。
// 所以每个失败用例都逐项断言事务回滚的三个面：
//
//	① capability_versions 里没有这一行（能力没建成）
//	② edges 里没有它发出去的边（边一条都没有）
//	③ artifacts 里没有它的产物（产物也没漏在事务外）
//
// 只有三个面都为 0，才说明「要么全成，要么全不成」是真的。

// linksTarget 造一个下游能力并返回它（带一个可选 params schema，给 literals 用例）。
func linksTarget(t *testing.T, db *DB, nsID, name string, props string) *CapabilityVersion {
	t.Helper()
	c := mkCap(nsID, name, 1, nil)
	if props != "" {
		c.ParamsSchema = json.RawMessage(`{"type":"object","properties":` + props + `}`)
	}
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create target capability %s: %v", name, err)
	}
	return c
}

// assertNothingPersisted 断言「这个能力名在三个面上全是 0」。
func assertNothingPersisted(t *testing.T, db *DB, nsID, capName, artifactDigest string) {
	t.Helper()
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?",
		nsID, capName); n != 0 {
		t.Fatalf("回滚失败：capability_versions 里留下了 %d 行 %s", n, capName)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_ref = ?", nsID, capName); n != 0 {
		t.Fatalf("回滚失败：edges 里留下了 %d 条从 %s 出发的边", n, capName)
	}
	if artifactDigest != "" {
		if n := countRows(t, db, "artifacts", "namespace_id = ? AND digest = ?",
			nsID, artifactDigest); n != 0 {
			t.Fatalf("回滚失败：artifacts 里留下了 %d 行 digest=%s", n, artifactDigest)
		}
	}
}

// TestCreateCapabilityWithLinksCreatesAll：能力 + 2 条边一次成功。
func TestCreateCapabilityWithLinksCreatesAll(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	notify := linksTarget(t, db, nsID, "notify.send", `{"text":{"type":"string"}}`)
	archive := linksTarget(t, db, nsID, "archive.logs", "")

	c := mkCap(nsID, "chain", 1, []string{"done", "failed"})
	impact, err := db.CreateCapabilityWithLinks(ctx, c, []LinkSpec{
		{FromPort: "done", ToRef: notify.Name, Literals: json.RawMessage(`{"text":"ok"}`)},
		{FromPort: "failed", ToRef: capRef(archive)}, // 显式带版本 = 钉死
	}, false)
	if err != nil {
		t.Fatalf("create capability with links: %v", err)
	}
	if impact == nil || impact.Breaking || impact.Links == nil {
		t.Fatalf("impact 形状不对（空影响面也必须是空数组）: %+v", impact)
	}

	// 能力行
	got, err := db.GetCapability(ctx, nsID, "chain", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("能力 id 与回执不一致: %s != %s", got.ID, c.ID)
	}

	// 边：左端恒为**裸名字**（跟随 latest），右端原样（裸名字 / name@version）
	edges, err := db.ListEdges(ctx, nsID, "chain")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("边数 = %d, want 2 (%+v)", len(edges), edges)
	}
	for _, e := range edges {
		if e.FromRef != "chain" {
			t.Fatalf("左端必须是裸名字（跟随 latest），got %q", e.FromRef)
		}
		if e.FromIsSource {
			t.Fatalf("能力定义时建的边不可能是入口边: %+v", e)
		}
	}
	if edges[0].ToRef != notify.Name || string(edges[0].Literals) != `{"text":"ok"}` {
		t.Fatalf("第一条边: toRef=%q literals=%s", edges[0].ToRef, edges[0].Literals)
	}
	if edges[1].ToRef != capRef(archive) || string(edges[1].Literals) != EdgeLiteralsEmpty {
		t.Fatalf("第二条边: toRef=%q literals=%s", edges[1].ToRef, edges[1].Literals)
	}
}

// TestCreateCapabilityWithLinksRollsBackOnBadLink：任何一条边不合法 → 三个面全 0。
//
// 逐项覆盖 store 的边校验：目标不存在 / 端口越界 / literals 键不存在 / 自连。
// 下标断言也在里面：错误必须带 `links[i]`，否则前端定位不到是哪一条。
func TestCreateCapabilityWithLinksRollsBackOnBadLink(t *testing.T) {
	cases := []struct {
		name    string
		links   func(nsID string, target *CapabilityVersion) []LinkSpec
		wantIdx int
		wantSub string
	}{
		{
			name: "目标不存在",
			links: func(nsID string, _ *CapabilityVersion) []LinkSpec {
				return []LinkSpec{{FromPort: "done", ToRef: "ghost.capability"}}
			},
			wantIdx: 0,
			wantSub: "edges.to_ref",
		},
		{
			name: "端口不在 outputs 里",
			links: func(nsID string, target *CapabilityVersion) []LinkSpec {
				return []LinkSpec{{FromPort: "not-a-port", ToRef: target.Name}}
			},
			wantIdx: 0,
			wantSub: "not in",
		},
		{
			name: "literals 的键不存在",
			links: func(nsID string, target *CapabilityVersion) []LinkSpec {
				return []LinkSpec{{FromPort: "done", ToRef: target.Name, Literals: json.RawMessage(`{"nope":true}`)}}
			},
			wantIdx: 0,
			wantSub: "literals.nope",
		},
		{
			name: "自连（出口接自己）",
			links: func(nsID string, _ *CapabilityVersion) []LinkSpec {
				return []LinkSpec{{FromPort: "done", ToRef: "chain"}}
			},
			wantIdx: 0,
			wantSub: "self loop",
		},
		{
			// 第二条才坏：前一条（合法）必须一起回滚，且下标要指到 1。
			name: "第二条坏 → 带上下标且第一条也不留",
			links: func(nsID string, target *CapabilityVersion) []LinkSpec {
				return []LinkSpec{
					{FromPort: "done", ToRef: target.Name},
					{FromPort: "failed", ToRef: "ghost.capability"},
				}
			},
			wantIdx: 1,
			wantSub: "edges.to_ref",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, _ := newTestNS(t, db)
			target := linksTarget(t, db, nsID, "notify.send", `{"text":{"type":"string"}}`)

			// 产物也一起提交：产物行漏在事务外就还是半成品，所以每个失败用例都带上它。
			digest := "sha256:" + strings.Repeat("ab", 32)
			build := func(cv *CapabilityVersion) (*Artifact, error) {
				return &Artifact{
					NamespaceID:  cv.NamespaceID,
					CapabilityID: cv.ID,
					Digest:       digest,
					Spec:         []byte(`{"kind":"capability"}`),
				}, nil
			}

			c := mkCap(nsID, "chain", 1, []string{"done", "failed"})
			_, err := db.CreateCapabilityWithLinksAndArtifact(ctx, c, tc.links(nsID, target), build, false)
			if !errors.Is(err, ErrEdgeRejected) {
				t.Fatalf("err = %v, want ErrEdgeRejected", err)
			}
			if want := fmt.Sprintf("links[%d]:", tc.wantIdx); !strings.Contains(err.Error(), want) {
				t.Fatalf("错误里没有下标 %q: %v", want, err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误里没有原因 %q: %v", tc.wantSub, err)
			}
			// 事务回滚的三个面
			assertNothingPersisted(t, db, nsID, "chain", digest)
		})
	}
}

// TestCreateCapabilityWithLinksArtifactBuildFailureRollsBack：事务内构建产物失败
// （run 写错）→ 整体回滚，且错误是 *ArtifactBuildError（handler 据此翻 400）。
func TestCreateCapabilityWithLinksArtifactBuildFailureRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	boom := errors.New("entrypoint_no_shebang: 入口文件必须以 #! 开头")
	build := func(cv *CapabilityVersion) (*Artifact, error) { return nil, boom }

	c := mkCap(nsID, "chain", 1, []string{"done"})
	_, err := db.CreateCapabilityWithLinksAndArtifact(ctx, c,
		[]LinkSpec{{FromPort: "done", ToRef: target.Name}}, build, false)
	if err == nil {
		t.Fatal("产物构建失败必须返回错误")
	}
	var buildErr *ArtifactBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("err = %v, want *ArtifactBuildError", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("原始错误必须可 unwrap: %v", err)
	}
	assertNothingPersisted(t, db, nsID, "chain", "")
}

// TestCreateCapabilityWithLinksArtifactInTx：产物与能力/边同一个事务，成功时三个面都在。
func TestCreateCapabilityWithLinksArtifactInTx(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	digest := "sha256:" + strings.Repeat("cd", 32)
	c := mkCap(nsID, "chain", 1, []string{"done"})
	res, err := db.CreateCapabilityWithLinksAndArtifact(ctx, c,
		[]LinkSpec{{FromPort: "done", ToRef: target.Name}},
		func(cv *CapabilityVersion) (*Artifact, error) {
			// 回调拿到的必须是**归一后**的权威行（id 已分配、version 已定）
			if cv.ID == "" || cv.Version != 1 {
				t.Fatalf("回调拿到的能力行不是权威行: %+v", cv)
			}
			return &Artifact{
				NamespaceID:  cv.NamespaceID,
				CapabilityID: cv.ID,
				Digest:       digest,
				Spec:         []byte(`{"kind":"capability"}`),
			}, nil
		}, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Artifact == nil || res.Artifact.Digest != digest || res.Artifact.CapabilityID != c.ID {
		t.Fatalf("回执里的产物不对: %+v", res.Artifact)
	}
	saved, err := db.GetArtifactByDigest(ctx, nsID, digest)
	if err != nil {
		t.Fatalf("产物没落库: %v", err)
	}
	if saved.CapabilityID != c.ID {
		t.Fatalf("产物挂错了能力: %s != %s", saved.CapabilityID, c.ID)
	}
	if len(res.Edges) != 1 || res.Edges[0].ToRef != target.Name {
		t.Fatalf("回执里的边不对: %+v", res.Edges)
	}
}

// TestCreateCapabilityWithLinksIdempotent：同内容重复提交不重复建边、不产生第二行。
func TestCreateCapabilityWithLinksIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	links := []LinkSpec{
		{FromPort: "done", ToRef: target.Name},
		{FromPort: "failed", ToRef: target.Name},
	}

	first := mkCap(nsID, "chain", 1, []string{"done", "failed"})
	if _, err := db.CreateCapabilityWithLinks(ctx, first, links, false); err != nil {
		t.Fatalf("第一次提交: %v", err)
	}

	// 同内容换个版本号再提交一次（自动开版本时客户端可能就是这么发的）：
	// content_hash 相同 → 折叠到已有行，边幂等（ON CONFLICT DO NOTHING）。
	second := mkCap(nsID, "chain", 2, []string{"done", "failed"})
	impact, err := db.CreateCapabilityWithLinks(ctx, second, links, false)
	if err != nil {
		t.Fatalf("第二次提交: %v", err)
	}
	if second.ID != first.ID || second.Version != first.Version {
		t.Fatalf("同内容没有折叠到已有行: first=%s@%d second=%s@%d",
			first.ID, first.Version, second.ID, second.Version)
	}
	// 折叠之后「不会成为新版本」→ 闸门不管，影响面必须是空的（不是 nil）
	if impact == nil || impact.Breaking || len(impact.Changes) != 0 || len(impact.Links) != 0 {
		t.Fatalf("幂等重放的影响面必须为空: %+v", impact)
	}

	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?",
		nsID, "chain"); n != 1 {
		t.Fatalf("capability_versions 行数 = %d, want 1", n)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_ref = ?", nsID, "chain"); n != 2 {
		t.Fatalf("edges 行数 = %d, want 2（不能因为重复提交而翻倍）", n)
	}
}

// TestCreateCapabilityWithLinksConcurrent：两个同内容请求并发 → 只有一份能力、边不重复。
//
// 靠的是唯一键（UNIQUE(namespace_id, content_hash) + edges 的幂等 PK）：
// 两个事务里一定有一个先提交，后到的那个在 content_hash 上冲突 → 折叠到已有行，
// 它的边则全部命中 `ON CONFLICT DO NOTHING`。
//
// 用**文件**库而不是 :memory:：内存库的连接池恒为 1（见 openWithOptions），
// 那样两个请求会在 database/sql 里排队，测不到「两个事务真的同时在飞」的形状。
func TestCreateCapabilityWithLinksConcurrent(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	caps := make([]*CapabilityVersion, workers)
	ready := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := mkCap(nsID, "chain", 1, []string{"done", "failed"})
			caps[i] = c
			<-ready // 尽量让两个事务同时起飞
			_, err := db.CreateCapabilityWithLinks(ctx, c, []LinkSpec{
				{FromPort: "done", ToRef: target.Name},
				{FromPort: "failed", ToRef: target.Name},
			}, false)
			errs[i] = err
		}(i)
	}
	close(ready)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?",
		nsID, "chain"); n != 1 {
		t.Fatalf("并发后 capability_versions 行数 = %d, want 1", n)
	}
	if caps[0].ID != caps[1].ID {
		t.Fatalf("两个请求落在了不同的能力行上: %s / %s", caps[0].ID, caps[1].ID)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_ref = ?", nsID, "chain"); n != 2 {
		t.Fatalf("并发后 edges 行数 = %d, want 2（边不能重复）", n)
	}
}

// TestCreateCapabilityWithLinksUpgradeGateStillGate：带 links 也不能绕过升级闸门。
func TestCreateCapabilityWithLinksUpgradeGateStillGate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	// v1：出口 fix；v2 删掉 fix → 破坏已有 name 型 link（v1 建的同一条）
	v1 := mkCap(nsID, "chain", 1, []string{"fix", "done"})
	if _, err := db.CreateCapabilityWithLinks(ctx, v1, nil, false); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "chain", FromPort: "fix", ToRef: target.Name}); err != nil {
		t.Fatalf("建已有 link: %v", err)
	}

	v2 := mkCap(nsID, "chain", 2, []string{"done"})
	_, err := db.CreateCapabilityWithLinks(ctx, v2, []LinkSpec{{FromPort: "done", ToRef: target.Name}}, false)
	var breaking *UpgradeBreakingError
	if !errors.As(err, &breaking) {
		t.Fatalf("删除被引用的出口必须被闸门拦住, err = %v", err)
	}
	// 闸门拒绝 = 整体回滚：v2 与它带来的边都不该存在
	if n := countRows(t, db, "capability_versions",
		"namespace_id = ? AND name = ? AND version = 2", nsID, "chain"); n != 0 {
		t.Fatalf("被闸门拒绝却写入了 v2")
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_port = ?", nsID, "done"); n != 0 {
		t.Fatalf("被闸门拒绝却写入了边")
	}
	// force=true 才放行
	if _, err := db.CreateCapabilityWithLinks(ctx, mkCap(nsID, "chain", 2, []string{"done"}),
		[]LinkSpec{{FromPort: "done", ToRef: target.Name}}, true); err != nil {
		t.Fatalf("force=true 应放行: %v", err)
	}
}
