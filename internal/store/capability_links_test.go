package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 本文件钉住「能力 + 下游边（+ 产物）一次提交」的**原子性**。
//
// 验收的核心不是「成功时能建出来」（那太容易），而是**失败时什么都没留下**。
// 所以每个失败用例都逐项断言事务回滚的三个面：
//
//	① capability_versions 里没有这一行（能力没建成）
//	② edges 里没有它发出去的边（边一条都没有）
//	③ 产物**不是独立行**（P2 删了 artifacts 表）：它由能力行算出来，所以①成立
//	   就等于③成立 —— 这里仍然显式断言「产物查不到」，防止有人重新引入一张镜像表。
//
// 只有三个面都为 0，才说明「要么全成，要么全不成」是真的。

// linksTarget 造一个下游能力并返回它（带一个可选 params schema，给 literals 用例）。
func linksTarget(t *testing.T, db *DB, nsID, name string, props string) *CapabilityVersion {
	t.Helper()
	c := mkCap(nsID, name, 1, nil)
	if props != "" {
		c.ParamsSchema = json.RawMessage(`{"type":"object","properties":` + props + `}`)
	}
	if err := db.CreateCapability(context.Background(), withSpec(c)); err != nil {
		t.Fatalf("create target capability %s: %v", name, err)
	}
	return c
}

// assertNothingPersisted 断言「这个能力名在三个面上全是 0」。
func assertNothingPersisted(t *testing.T, db *DB, nsID, capName string) {
	t.Helper()
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?",
		nsID, capName); n != 0 {
		t.Fatalf("回滚失败：capability_versions 里留下了 %d 行 %s", n, capName)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_ref = ?", nsID, capName); n != 0 {
		t.Fatalf("回滚失败：edges 里留下了 %d 条从 %s 出发的边", n, capName)
	}
	if tableExists(t, db, "artifacts") {
		t.Fatalf("artifacts 表又回来了：产物是能力行算出来的派生值（Capability Spec v3 §11）")
	}
}

// realBuild 是「按权威行构建产物」的标准 builder（与服务端 newStoreArtifact 同口径）。
//
// P2 起产物不落库，builder 的职责变成**用另一条路径再算一次**：所以它必须真的从
// 能力行算 digest 与 canonical 字节，编一个 digest 出来会被 verifyBuiltArtifact 拒掉。
func realBuild(t *testing.T) ArtifactBuilder {
	t.Helper()
	return func(cv *CapabilityVersion) (*Artifact, error) {
		a := cv.Artifact()
		spec, err := artifact.CanonicalJSON(&a)
		if err != nil {
			return nil, err
		}
		return &Artifact{
			NamespaceID:  cv.NamespaceID,
			CapabilityID: cv.ID,
			Digest:       cv.Digest(),
			Spec:         spec,
		}, nil
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
	impact, err := db.CreateCapabilityWithLinks(ctx, withSpec(c), []LinkSpec{
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
// 逐项覆盖 store 的边校验：目标不存在 / 端口越界 / literals 键不存在 / literals 类型不对。
// （自连与成环不再是「坏 link」：契约 §2 之后它们是合法编排。）
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
			// 自连/成环现在**合法**（契约 §2），所以这一条换成另一种配置期硬拒：
			// 字面量的类型与目标的 params_schema 不等（该拦在保存时，不是派发时）。
			name: "literals 的值的类型不对",
			links: func(nsID string, target *CapabilityVersion) []LinkSpec {
				return []LinkSpec{{FromPort: "done", ToRef: target.Name, Literals: json.RawMessage(`{"text":123}`)}}
			},
			wantIdx: 0,
			wantSub: "literals.text",
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

			// 产物也一起构建：素材与真实路径一致（从权威行算），失败用例同样带上它。
			build := realBuild(t)

			c := mkCap(nsID, "chain", 1, []string{"done", "failed"})
			_, err := db.CreateCapabilityWithLinksAndArtifact(ctx, withSpec(c), tc.links(nsID, target), build, false)
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
			assertNothingPersisted(t, db, nsID, "chain")
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
	_, err := db.CreateCapabilityWithLinksAndArtifact(ctx, withSpec(c),
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
	assertNothingPersisted(t, db, nsID, "chain")
}

// TestCreateCapabilityWithLinksArtifactInTx：成功时能力 / 边 / 产物三个面都在。
//
// 产物的「在」在 P2 之后意味着：**能力行的正文算得出它**，且回执里的 digest 与
// 能力行的 content_hash 是同一个值（verifyBuiltArtifact 在事务内就对过账）。
func TestCreateCapabilityWithLinksArtifactInTx(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	target := linksTarget(t, db, nsID, "notify.send", "")

	// 脚本形式（不是 handler 占位）：只有它才有可部署的产物（契约 §5）。
	c := mkCap(nsID, "chain", 1, []string{"done"})
	c.Spec = artifact.Spec{
		Params:  c.ParamsSchema,
		Outputs: c.Outputs,
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/bash\nexit 0\n"},
		},
	}
	inner := realBuild(t)
	res, err := db.CreateCapabilityWithLinksAndArtifact(ctx, withSpec(c),
		[]LinkSpec{{FromPort: "done", ToRef: target.Name}},
		func(cv *CapabilityVersion) (*Artifact, error) {
			// 回调拿到的必须是**归一后**的权威行（id 已分配、version 已定）
			if cv.ID == "" || cv.Version != 1 {
				t.Fatalf("回调拿到的能力行不是权威行: %+v", cv)
			}
			return inner(cv)
		}, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Artifact == nil || res.Artifact.CapabilityID != c.ID {
		t.Fatalf("回执里的产物不对: %+v", res.Artifact)
	}
	if res.Artifact.Digest != c.Digest() {
		t.Fatalf("回执里的 digest = %q, want 能力行的 %q", res.Artifact.Digest, c.Digest())
	}
	saved, err := db.GetArtifactByDigest(ctx, nsID, c.Digest())
	if err != nil {
		t.Fatalf("按能力 digest 取不到产物: %v", err)
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
	if _, err := db.CreateCapabilityWithLinks(ctx, withSpec(first), links, false); err != nil {
		t.Fatalf("第一次提交: %v", err)
	}

	// 同 name@version、同内容再提交一次（重试 / 双击）：content_hash 相同 →
	// 幂等折叠到已有行，边幂等（ON CONFLICT DO NOTHING）。
	//
	// 注意不能再拿「同内容换个版本号」来验幂等：v3 的 digest 吃 version（契约 §3.2），
	// 换版本号就是另一份内容，跨版本折叠已经取消。
	second := mkCap(nsID, "chain", 1, []string{"done", "failed"})
	impact, err := db.CreateCapabilityWithLinks(ctx, withSpec(second), links, false)
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
			_, err := db.CreateCapabilityWithLinks(ctx, withSpec(c), []LinkSpec{
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
	if _, err := db.CreateCapabilityWithLinks(ctx, withSpec(v1), nil, false); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "chain", FromPort: "fix", ToRef: target.Name}); err != nil {
		t.Fatalf("建已有 link: %v", err)
	}

	v2 := mkCap(nsID, "chain", 2, []string{"done"})
	_, err := db.CreateCapabilityWithLinks(ctx, withSpec(v2), []LinkSpec{{FromPort: "done", ToRef: target.Name}}, false)
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
	if _, err := db.CreateCapabilityWithLinks(ctx, withSpec(mkCap(nsID, "chain", 2, []string{"done"})),
		[]LinkSpec{{FromPort: "done", ToRef: target.Name}}, true); err != nil {
		t.Fatalf("force=true 应放行: %v", err)
	}
}
