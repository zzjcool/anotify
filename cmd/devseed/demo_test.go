package main

// demo_test.go 是 `devseed -demo` 的**离线**回归：不需要服务器，也不需要浏览器。
//
// 它钉住三件在 v4 里容易被写回 v3 形状的事：
//
//  1. 播种出来的是**真 v4 资源**：能力行过 `artifact.Validate`、`next` 派生出的边真在
//     `edges` 表里、源 targets 派生出的入口边也真在（devseed 自己**不写** edges）；
//  2. 播种**不伪造执行**：只跑 seed* 之后 tasks / deployments 必须是 0 行 —— 任务只能
//     由中枢的路由建出来，部署是人工动作；
//  3. 重开幂等：同一份正文再播一次不新增版本、不新增边、digest 不变（内容寻址折叠）。
//
// 事件侧用 `source.Service.Ingest`（**真实**落库路径，与 HTTP ingress 同一个函数，
// 只是少了 HTTP 那一层）验证示例场景的预期：4 条任务 + 1 条 `param_missing` 剪枝，
// 且同 deliveryId 再投一次只回 duplicate、不重复建任务。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// newDemoTestDB 开一个临时库并用真实注册路径拿到默认命名空间。
func newDemoTestDB(t *testing.T) (*store.DB, *store.Namespace) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "devseed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u := &store.User{
		ID:          store.NewUserID(),
		Username:    "devseed-test",
		DisplayName: "devseed-test",
		Role:        store.RoleMember,
		CreatedAt:   store.Now(),
	}
	ns, err := db.RegisterUser(context.Background(), u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	return db, ns
}

// countRows 直接数一张表（播种"不许伪造"的断言用；不经过任何领域读模型）。
func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestDemoCapabilityArtifactsValidate 钉住示例能力正文本身是合法的 v4 产物。
//
// 这一层不过，后面的 store 播种会以别处难以定位的错误失败（比如 run.files 没 shebang）。
func TestDemoCapabilityArtifactsValidate(t *testing.T) {
	known := make(map[string]bool, len(demoCapabilities))
	for _, d := range demoCapabilities {
		known[d.Name] = true
	}

	for _, d := range demoCapabilities {
		d := d
		t.Run(d.Name, func(t *testing.T) {
			if !strings.HasPrefix(d.Script, "#!") {
				t.Fatalf("run.sh 正文必须以 #! 开头（capability.md §2.3），实际以 %q 开头", firstLine(d.Script))
			}
			if !strings.Contains(d.Script, "\n") {
				t.Fatal("run.sh 正文只有一行：真实能力脚本不该这么写")
			}
			// next 引用必须指向示例里真实存在的能力（拼错名字在 v4 里是「永远解析不到」）。
			for _, ref := range d.Next {
				name := ref
				if i := strings.LastIndex(ref, "@"); i > 0 {
					name = ref[:i]
				}
				if !known[name] {
					t.Fatalf("next 引用了未播种的能力 %q（拼写/顺序问题会让它永远 DownstreamResolved=False）", ref)
				}
			}
			a := &artifact.Artifact{
				APIVersion: artifact.APIVersion,
				Kind:       artifact.KindCapability,
				Metadata:   artifact.Metadata{Name: d.Name, Version: 1},
				Spec: artifact.Spec{
					Params: d.Params,
					Next:   d.Next,
					Run: artifact.Run{
						Entrypoint: "run.sh",
						Files:      map[string]string{"run.sh": d.Script},
					},
					Requires: artifact.Requires{Commands: append([]string(nil), demoCapabilityRequires...)},
				},
			}
			if err := artifact.Validate(a); err != nil {
				t.Fatalf("示例能力过不了 artifact.Validate：%v", err)
			}
		})
	}
}

// TestSeedDemoWritesRealV4Resources 钉住播种结果：真资源 + 派生边 + 零伪造执行。
func TestSeedDemoWritesRealV4Resources(t *testing.T) {
	db, ns := newDemoTestDB(t)
	ctx := context.Background()

	caps, err := seedCapabilities(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed capabilities: %v", err)
	}
	if len(caps) != len(demoCapabilities) {
		t.Fatalf("能力行 %d，期望 %d", len(caps), len(demoCapabilities))
	}
	for _, c := range caps {
		if c.ID == "" || c.Version < 1 {
			t.Fatalf("能力 %s 没有拿到真实 id/version：id=%q version=%d", c.Name, c.ID, c.Version)
		}
		if !strings.HasPrefix(c.Digest, "sha256:") {
			t.Fatalf("能力 %s 的 digest 形状不对：%q", c.Name, c.Digest)
		}
		if len(c.Spec.Run.Files) == 0 {
			t.Fatalf("能力 %s 没有 run.files（v4 只有脚本一种实现）", c.Name)
		}
	}

	// 能力边 = spec.next 的派生投影。devseed 只写能力行，边必须是 store 写出来的。
	assertEdges(t, db, ns.ID, false, "analyze-pr", []string{"apply-fix", "deploy-preview"})
	assertEdges(t, db, ns.ID, false, "apply-fix", []string{"deploy-preview"})
	assertEdges(t, db, ns.ID, false, "deploy-preview", nil)

	srcs, err := seedSources(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	if len(srcs) != len(demoSources) {
		t.Fatalf("源行 %d，期望 %d", len(srcs), len(demoSources))
	}
	kinds := map[string]string{}
	for _, s := range srcs {
		kinds[s.Name] = s.Kind
		if !s.Enabled {
			t.Fatalf("源 %s 播种后应是启用的", s.Name)
		}
	}
	for _, want := range []struct{ name, kind string }{
		{"github", store.SourceKindWebhook},
		{"nightly", store.SourceKindCron},
		{"release-preview", store.SourceKindManual},
	} {
		if kinds[want.name] != want.kind {
			t.Fatalf("源 %s 的 kind=%q，期望 %q", want.name, kinds[want.name], want.kind)
		}
	}

	// 入口边 = source.targets 的派生投影（from_is_source=1，from_ref=源名）。
	assertEdges(t, db, ns.ID, true, "github", []string{"analyze-pr"})
	assertEdges(t, db, ns.ID, true, "nightly", []string{"analyze-pr"})
	assertEdges(t, db, ns.ID, true, "release-preview", []string{"deploy-preview"})

	// 节点：demo-node 建出来、拿得到明文 token（真 WS 握手要用它）。
	nodeID, token, err := seedNode(ctx, db, demoNodeName)
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if nodeID == "" || token == "" {
		t.Fatalf("节点 id/token 为空：id=%q token-empty=%v", nodeID, token == "")
	}
	if got := store.HashNodeToken(token); got == token {
		t.Fatal("节点 token 应当只在返回时给明文，库里存 hash")
	}

	// 源按名字可查（seedSources 的「已存在就收敛」分支靠的就是这个读路径）；
	// 不存在的名字必须回 store 哨兵错误，否则收敛会写在一条不存在的行上。
	if _, err := db.GetSourceByName(ctx, ns.ID, "ghost"); !errors.Is(err, store.ErrSourceNotFound) {
		t.Fatalf("查不存在的源应当回 ErrSourceNotFound，实际 %v", err)
	}

	// **不伪造执行**：播种只写资源，任务与部署都必须还是 0 行。
	for _, table := range []string{"tasks", "deployments"} {
		if n := countRows(t, db, table); n != 0 {
			t.Fatalf("播种写了 %s 表 %d 行：devseed 不许伪造任务/部署（那要靠真实事件与人工部署）", table, n)
		}
	}
}

// TestSeedDemoIsIdempotent 钉住「重开 devseed」不会长出新版本/新边/新节点。
func TestSeedDemoIsIdempotent(t *testing.T) {
	db, ns := newDemoTestDB(t)
	ctx := context.Background()

	first, err := seedCapabilities(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed capabilities #1: %v", err)
	}
	srcs1, err := seedSources(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed sources #1: %v", err)
	}
	nodeID1, _, err := seedNode(ctx, db, demoNodeName)
	if err != nil {
		t.Fatalf("seed node #1: %v", err)
	}
	capsBefore := countRows(t, db, "capability_versions")
	edgesBefore := countRows(t, db, "edges")
	nodesBefore := countRows(t, db, "nodes")

	second, err := seedCapabilities(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed capabilities #2: %v", err)
	}
	if _, err := seedSources(ctx, db, ns.ID); err != nil {
		t.Fatalf("seed sources #2: %v", err)
	}
	nodeID2, _, err := seedNode(ctx, db, demoNodeName)
	if err != nil {
		t.Fatalf("seed node #2: %v", err)
	}

	if got := countRows(t, db, "capability_versions"); got != capsBefore {
		t.Fatalf("第二次播种把能力版本从 %d 变成 %d：内容寻址应该折叠成同一行", capsBefore, got)
	}
	if got := countRows(t, db, "edges"); got != edgesBefore {
		t.Fatalf("第二次播种把边从 %d 变成 %d：边是派生投影，重复投影不该新增行", edgesBefore, got)
	}
	if got := countRows(t, db, "nodes"); got != nodesBefore {
		t.Fatalf("第二次播种把节点从 %d 变成 %d：同名节点应复用同一行、只轮换 token", nodesBefore, got)
	}
	if nodeID1 != nodeID2 {
		t.Fatalf("同名节点复用了两行：%q vs %q", nodeID1, nodeID2)
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Digest != second[i].Digest {
			t.Fatalf("第二次播种产生了新版本：%s %s@%d(digest %s) → %s@%d(digest %s)",
				first[i].Name, first[i].Name, first[i].Version, first[i].Digest,
				second[i].Name, second[i].Version, second[i].Digest)
		}
	}
	if len(srcs1) != len(demoSources) {
		t.Fatalf("源数量 %d，期望 %d", len(srcs1), len(demoSources))
	}

	// 收敛：库里源被人改成了别的 targets（例如上一版 devseed 留下的行），重播必须改回来。
	// 入口边跟着 targets 一起重建（source.md §4.2）——这也是不能用旧行自嘴的证据。
	changed, err := db.GetSourceByName(ctx, ns.ID, "github")
	if err != nil {
		t.Fatalf("读源 github: %v", err)
	}
	changed.Targets = []sourcespec.Target{{Capability: "deploy-preview"}}
	if err := db.UpdateSource(ctx, changed); err != nil {
		t.Fatalf("改源 github: %v", err)
	}
	assertEdges(t, db, ns.ID, true, "github", []string{"deploy-preview"})
	if _, err := seedSources(ctx, db, ns.ID); err != nil {
		t.Fatalf("seed sources #3（收敛）: %v", err)
	}
	assertEdges(t, db, ns.ID, true, "github", []string{"analyze-pr"})
}

// TestDemoEventsProduceRealTasksAndPrune 用**真实** Ingest 走一遍示例事件。
//
// 断言的数字就是运行 `-demo` 时应当看到的：4 条任务 + 1 条 param_missing 剪枝；
// 任务相位是 pending（devseed 不催熟），digest 钉在任务上。
func TestDemoEventsProduceRealTasksAndPrune(t *testing.T) {
	db, ns := newDemoTestDB(t)
	ctx := context.Background()

	if _, err := seedCapabilities(ctx, db, ns.ID); err != nil {
		t.Fatalf("seed capabilities: %v", err)
	}
	srcs, err := seedSources(ctx, db, ns.ID)
	if err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	byName := make(map[string]*store.Source, len(srcs))
	for _, s := range srcs {
		byName[s.Name] = s
	}

	svc := source.New(db, route.New(db))
	var (
		taskIDs  []string
		prunes   int
		pruneCap string
		pruneWhy string
	)
	for _, e := range demoEvents {
		src := byName[e.Source]
		if src == nil {
			t.Fatalf("事件指向的源 %q 没有被播种", e.Source)
		}
		var payload json.RawMessage
		if e.Payload != nil {
			raw, merr := json.Marshal(e.Payload)
			if merr != nil {
				t.Fatalf("编码事件载荷: %v", merr)
			}
			payload = raw
		}
		receipt, ierr := svc.Ingest(ctx, source.Input{
			Source: src, Type: e.Type, DeliveryID: e.DeliveryID, Payload: payload,
		})
		if ierr != nil {
			t.Fatalf("投递事件 %s: %v", e.DeliveryID, ierr)
		}
		if receipt.EventID == "" || receipt.TraceID == "" {
			t.Fatalf("事件 %s 的回执缺 eventId/traceId：%+v", e.DeliveryID, receipt)
		}
		for _, p := range receipt.Pruned {
			prunes++
			pruneCap, pruneWhy = p.Capability, p.Reason
		}
		for _, ref := range receipt.Tasks {
			taskIDs = append(taskIDs, ref.TaskID)
		}
	}

	if len(taskIDs) != 4 {
		t.Fatalf("示例事件建出 %d 条任务，期望 4（2 条小改/大改 + cron + manual）", len(taskIDs))
	}
	if prunes != 1 || pruneCap != "analyze-pr" || pruneWhy != protocol.ReasonParamMissing {
		t.Fatalf("期望 1 条 analyze-pr 的 %s 剪枝，实际 prunes=%d cap=%q reason=%q",
			protocol.ReasonParamMissing, prunes, pruneCap, pruneWhy)
	}

	// 剪枝**留痕**：events 表里要有一条 source='route' 的行（source.md §6）。
	routed, err := db.ListEvents(ctx, ns.ID, "", store.EventPruneSource, 50)
	if err != nil {
		t.Fatalf("读剪枝留痕: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("剪枝留痕 %d 行，期望 1 行（剪枝必须留痕）", len(routed))
	}

	for _, id := range taskIDs {
		task, terr := db.GetTask(ctx, ns.ID, id)
		if terr != nil {
			t.Fatalf("读任务 %s: %v", id, terr)
		}
		if task.Phase != store.TaskPending {
			t.Fatalf("任务 %s 相位 %q：devseed 不该让任务离开 pending（没有执行器）", id, task.Phase)
		}
		if !strings.HasPrefix(task.Digest, "sha256:") {
			t.Fatalf("任务 %s 没有钉住 digest：%q", id, task.Digest)
		}
		if len(task.Params) == 0 || string(task.Params) == "{}" {
			t.Fatalf("任务 %s 的参数是空的：源的 targets 模板没有生效", id)
		}
	}

	tasksBefore := countRows(t, db, "tasks")
	// 幂等：同一 deliveryId 再投一次只回 duplicate，不重复建任务/事件。
	for _, e := range demoEvents {
		var payload json.RawMessage
		if e.Payload != nil {
			raw, _ := json.Marshal(e.Payload)
			payload = raw
		}
		receipt, ierr := svc.Ingest(ctx, source.Input{
			Source: byName[e.Source], Type: e.Type, DeliveryID: e.DeliveryID, Payload: payload,
		})
		if ierr != nil {
			t.Fatalf("重投事件 %s: %v", e.DeliveryID, ierr)
		}
		if !receipt.Duplicate {
			t.Fatalf("重投事件 %s 没有被判成 duplicate：devseed 重开就会重复派发", e.DeliveryID)
		}
	}
	if got := countRows(t, db, "tasks"); got != tasksBefore {
		t.Fatalf("重投后任务从 %d 变成 %d 行", tasksBefore, got)
	}
}

// TestNodeStreamURL 钉住 base URL → 节点 WS 入口的换算（devseed 用 -demo-url 指向服务器）。
func TestNodeStreamURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080":  "ws://localhost:8080/v1/node/stream",
		"http://localhost:8080/": "ws://localhost:8080/v1/node/stream",
		"https://hub.example":    "wss://hub.example/v1/node/stream",
	}
	for in, want := range cases {
		if got := nodeStreamURL(in); got != want {
			t.Fatalf("nodeStreamURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// assertEdges 断言某个上游节点的派生下游恰好是 want（顺序无关，集合比较）。
func assertEdges(t *testing.T, db *store.DB, namespaceID string, fromIsSource bool, fromRef string, want []string) {
	t.Helper()
	targets, err := db.EdgesFrom(context.Background(), namespaceID, fromIsSource, fromRef)
	if err != nil {
		t.Fatalf("读 %q 的边: %v", fromRef, err)
	}
	got := make([]string, 0, len(targets))
	for _, target := range targets {
		got = append(got, target.ToRef)
	}
	if len(got) != len(want) {
		t.Fatalf("%q 的下游 = %v，期望 %v（边是 next/targets 的派生投影）", fromRef, got, want)
	}
	sortedGot := append([]string(nil), got...)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedGot)
	sort.Strings(sortedWant)
	for i := range sortedWant {
		if sortedGot[i] != sortedWant[i] {
			t.Fatalf("%q 的下游 = %v，期望 %v", fromRef, got, want)
		}
	}
}

// firstLine 取正文首行（错误信息里别把整段脚本打出来）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
