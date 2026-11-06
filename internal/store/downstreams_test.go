package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 本文件覆盖「在能力里整体替换下游」（CONTRACT-downstream-edit §2）的 store 语义：
//
//	replace（不是 append）/ 空数组=清空 / 只动这个能力的 out 边 /
//	改优先级与 literals 不换 created_at / 任一校验失败一个字节都不写。

// snapshotEdges 把命名空间里的**全部**边拍成可逐条比对的字符串快照。
//
// 为什么要字符串而不是结构体比较：失败路径上要证明的是「库里一个字节都没变」，
// 连 created_at / mapping 这种不参与比较的列也要一起钉住——否则「只改了 priority
// 又改回去」这种回归会被漏掉。
func snapshotEdges(t *testing.T, db *DB, namespaceID string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at
		   FROM edges WHERE namespace_id = ?
		  ORDER BY from_is_source, from_ref, from_port, to_ref`, namespaceID)
	if err != nil {
		t.Fatalf("snapshot edges: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var (
			isSrc               int
			fromRef, fromPort   string
			toRef, literals, mp string
			priority            int
			createdAt           int64
		)
		if err := rows.Scan(&isSrc, &fromRef, &fromPort, &toRef, &literals, &mp, &priority, &createdAt); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s|%s|%d|%d",
			isSrc, fromRef, fromPort, toRef, literals, mp, priority, createdAt))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// listEdgesFrom 取某个能力名下的全部出边（ListEdges 按左端**名字**过滤）。
func listEdgesFrom(t *testing.T, db *DB, namespaceID, name string) []*Edge {
	t.Helper()
	edges, err := db.ListEdges(context.Background(), namespaceID, name)
	if err != nil {
		t.Fatalf("list edges from %s: %v", name, err)
	}
	return edges
}

// downstreamTestFixture 是这些用例共用的一套小图：
//
//	flow --out--> a / b / c（存量三条，其中 a 带 priority 与 literals，用来验「原地改」）
//	other --out--> a（别人的边，必须一条都不动）
//	github --*--> a（入口边，必须一条都不动）
type downstreamFixture struct {
	db   *DB
	nsID string
	caps map[string]*CapabilityVersion
}

func newDownstreamFixture(t *testing.T) *downstreamFixture {
	t.Helper()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	f := &downstreamFixture{db: db, nsID: nsID, caps: map[string]*CapabilityVersion{}}
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)
	defs := []*CapabilityVersion{
		mkCap(nsID, "flow", 1, []string{ImplicitPort}),
		mkCap(nsID, "other", 1, []string{ImplicitPort}),
		mkCap(nsID, "a", 1, nil),
		mkCap(nsID, "b", 1, nil),
		mkCap(nsID, "c", 1, nil),
		{NamespaceID: nsID, Name: "strict", Version: 1, ParamsSchema: schema,
			Outputs: []string{}, Source: CapSourceCustom},
	}
	for _, c := range defs {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create %s@%d: %v", c.Name, c.Version, err)
		}
		f.caps[c.Name] = c
	}
	// 存量三条 flow 出边（用 CreateEdge 造数：它就是「老路径」的写入形状）。
	for _, spec := range []struct {
		to       string
		priority int
		literals string
	}{
		{"a", 7, EdgeLiteralsEmpty},
		{"b", 0, `{}`},
		{"c", 0, `{}`},
	} {
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID: nsID, FromRef: "flow", FromPort: ImplicitPort,
			ToRef: spec.to, Priority: spec.priority, Literals: json.RawMessage(spec.literals),
		}); err != nil {
			t.Fatalf("seed flow -> %s: %v", spec.to, err)
		}
	}
	// 别人的边 + 入口边：replace 的 WHERE 里只要少一个条件，它们就会被误删。
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "other", FromPort: ImplicitPort, ToRef: "a",
	}); err != nil {
		t.Fatalf("seed other -> a: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "*",
		ToRef: "strict", Mapping: json.RawMessage(`{"to":"{{x}}"}`),
	}); err != nil {
		t.Fatalf("seed entry edge: %v", err)
	}
	return f
}

// TestReplaceCapabilityDownstreams 整体替换：加 / 删 / 改优先级 / 重排 / literals 更新。
func TestReplaceCapabilityDownstreams(t *testing.T) {
	f := newDownstreamFixture(t)
	ctx := context.Background()

	before := listEdgesFrom(t, f.db, f.nsID, "flow")
	if len(before) != 3 {
		t.Fatalf("seed edges = %d, want 3", len(before))
	}
	var aCreatedAt int64
	for _, e := range before {
		if e.ToRef == "a" {
			aCreatedAt = e.CreatedAt
		}
	}
	if aCreatedAt == 0 {
		t.Fatal("seeded flow -> a has no created_at")
	}

	// ① 删掉 b、保留 a（改 priority）、新增 strict（带 literals）。
	links, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{
		{ToRef: "a", Priority: 3},
		{ToRef: "strict", Priority: 42, Literals: json.RawMessage(`{"to":"new"}`)},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(links) != 2 || links[0].ToRef != "a" || links[1].ToRef != "strict" {
		t.Fatalf("回执顺序/内容 = %+v, want [a strict]（与请求同序）", links)
	}
	// 回执带解析结果（前端回显用）：`a` 是名字型引用 → 解析到当前 latest。
	if links[0].ToCapabilityID != f.caps["a"].ID || links[0].ResolvedVersion != 1 {
		t.Fatalf("a 的解析结果 = %s@%d, want %s@1", links[0].ToCapabilityID, links[0].ResolvedVersion, f.caps["a"].ID)
	}
	if string(links[0].Literals) != EdgeLiteralsEmpty {
		t.Fatalf("缺省 literals = %s, want {}", links[0].Literals)
	}
	if string(links[1].Literals) != `{"to":"new"}` {
		t.Fatalf("回执 literals = %s, want {\"to\":\"new\"}", links[1].Literals)
	}

	got := listEdgesFrom(t, f.db, f.nsID, "flow")
	if len(got) != 2 {
		t.Fatalf("替换后 flow 出边 = %d, want 2（b 应被删掉）: %+v", len(got), got)
	}
	byRef := map[string]*Edge{}
	for _, e := range got {
		byRef[e.ToRef] = e
		if e.FromPort != ImplicitPort {
			t.Fatalf("from_port = %q, want %q（隐式出口）", e.FromPort, ImplicitPort)
		}
	}
	if e := byRef["a"]; e == nil || e.Priority != 3 || string(e.Literals) != EdgeLiteralsEmpty {
		t.Fatalf("a 的边 = %+v, want priority=3 literals={}", e)
	}
	// 改优先级/literals 是**原地改同一条线**：created_at 不刷新（它是接上这条线的时刻）。
	if e := byRef["a"]; e.CreatedAt != aCreatedAt {
		t.Fatalf("created_at 被刷新了：%d → %d（原地改不该换创建时刻）", aCreatedAt, e.CreatedAt)
	}
	if e := byRef["strict"]; e == nil || e.Priority != 42 || string(e.Literals) != `{"to":"new"}` {
		t.Fatalf("strict 的边 = %+v, want priority=42 literals={\"to\":\"new\"}", e)
	}

	// ② 重排 + 改优先级 + literals 更新（strict 的 literals 从 {"to":"new"} 改成 {}/新值）：
	// 提交顺序就是回执顺序（DB 里没有顺序这一列，排序是 UI 的事）。
	links, err = f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{
		{ToRef: "strict", Priority: 0, Literals: json.RawMessage(`{"to":"third"}`)},
		{ToRef: "a", Priority: 10},
		{ToRef: "c", Priority: 1},
	})
	if err != nil {
		t.Fatalf("replace reorder: %v", err)
	}
	if len(links) != 3 || links[0].ToRef != "strict" || links[1].ToRef != "a" || links[2].ToRef != "c" {
		t.Fatalf("重排回执 = %+v, want [strict a c]", links)
	}
	afterReorder := listEdgesFrom(t, f.db, f.nsID, "flow")
	if len(afterReorder) != 3 {
		t.Fatalf("重排后 flow 出边 = %d, want 3: %+v", len(afterReorder), afterReorder)
	}
	for _, e := range afterReorder {
		switch e.ToRef {
		case "strict":
			// literals 原地更新为这一轮提交的值（replace 的语义：提交的那份就是全部，
			// 不是「只更新提交了的字段」）。
			if string(e.Literals) != `{"to":"third"}` || e.Priority != 0 {
				t.Fatalf("strict 的边 = %+v, want priority=0 literals={\"to\":\"third\"}", e)
			}
		case "c":
			// 这一轮没提交 literals → 落库的是 {}。
			if string(e.Literals) != EdgeLiteralsEmpty || e.Priority != 1 {
				t.Fatalf("c 的边 = %+v, want priority=1 literals={}", e)
			}
		}
	}

	// ③ 空数组 = 清空（终端能力）。
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := listEdgesFrom(t, f.db, f.nsID, "flow"); len(got) != 0 {
		t.Fatalf("清空后 flow 出边 = %d, want 0: %+v", len(got), got)
	}

	// ④ 只动 flow 的出边：别人的边与入口边一条都不能少。
	var others int
	if err := f.db.QueryRow(`SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref IN ('other','github')`,
		f.nsID).Scan(&others); err != nil {
		t.Fatalf("count others: %v", err)
	}
	if others != 2 {
		t.Fatalf("别人的边/入口边被误删了：剩 %d 条，want 2", others)
	}
}

// TestReplaceCapabilityDownstreamsLeavesOtherPortsAlone 别的端口上的边不归这条路径管。
//
// 模型里一个能力只有一个隐式出口 `out`；但历史的多出口能力（`outputs` 长度 > 1）在界面上
// 是**只读保留**的（CONTRACT-hide-ports §5：不替用户做破坏性改写）。它们挂在 done/failed
// 上的边属于那个端口结构，不是「下游整体替换」的对象 —— 删掉它等于替用户改拓扑。
// 所以 DELETE 比契约 §2 的裸 `from_ref` 多一个 `from_port='out'`（当前数据上行为不变）。
func TestReplaceCapabilityDownstreamsLeavesOtherPortsAlone(t *testing.T) {
	f := newDownstreamFixture(t)
	ctx := context.Background()

	// 直插：`fix` 不在 flow 的 outputs 里（createEdgeTx 会拒），而历史多出口数据就是那个形状。
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
		 VALUES (?, 0, 'flow', 'fix', 'b', '{}', '{}', 0, ?)`,
		f.nsID, Now()); err != nil {
		t.Fatalf("seed legacy-port edge: %v", err)
	}

	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{{ToRef: "a"}}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows := snapshotEdges(t, f.db, f.nsID)
	var flowOut, legacyPort, otherOut, entry int
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r, "0|flow|out|"):
			flowOut++
		case strings.HasPrefix(r, "0|flow|fix|"):
			legacyPort++
		case strings.HasPrefix(r, "0|other|"):
			otherOut++
		case strings.HasPrefix(r, "1|github|"):
			entry++
		}
	}
	if flowOut != 1 || legacyPort != 1 || otherOut != 1 || entry != 1 {
		t.Fatalf("替换结果 flow/out=%d legacyPort=%d other=%d entry=%d（want 1/1/1/1）\n%v",
			flowOut, legacyPort, otherOut, entry, rows)
	}
}

// TestReplaceCapabilityDownstreamsIdempotent 同一份 payload 提交两次 = 同样的库状态。
func TestReplaceCapabilityDownstreamsIdempotent(t *testing.T) {
	f := newDownstreamFixture(t)
	ctx := context.Background()
	payload := []DownstreamSpec{{ToRef: "a", Priority: 1}, {ToRef: "strict", Priority: 2, Literals: json.RawMessage(`{"to":"x"}`)}}

	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", payload); err != nil {
		t.Fatalf("replace #1: %v", err)
	}
	first := snapshotEdges(t, f.db, f.nsID)
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", payload); err != nil {
		t.Fatalf("replace #2: %v", err)
	}
	second := snapshotEdges(t, f.db, f.nsID)
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("幂等性被破坏：\n第一次:\n%s\n第二次:\n%s", strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
}

// TestReplaceCapabilityDownstreamsValidation 每条校验都报自己的机器码，且**一条边都不写**。
func TestReplaceCapabilityDownstreamsValidation(t *testing.T) {
	f := newDownstreamFixture(t)
	ctx := context.Background()
	nsB := newTestExtraNS(t, f.db, "staging")
	foreign := mkCap(nsB, "foreign", 1, nil)
	if err := f.db.CreateCapability(ctx, foreign); err != nil {
		t.Fatalf("create foreign: %v", err)
	}

	tooMany := make([]DownstreamSpec, DownstreamsMaxLinks+1)
	for i := range tooMany {
		tooMany[i] = DownstreamSpec{ToRef: "a"}
	}

	cases := []struct {
		name   string
		links  []DownstreamSpec
		assert func(t *testing.T, err error)
	}{
		{
			name:  "unknown_target",
			links: []DownstreamSpec{{ToRef: "a"}, {ToRef: "ghost"}},
			assert: func(t *testing.T, err error) {
				codeErr(t, err, "unknown_target")
				if !strings.Contains(err.Error(), "ghost") {
					t.Fatalf("错误里没点名坏引用: %v", err)
				}
			},
		},
		{
			name:   "self_link",
			links:  []DownstreamSpec{{ToRef: "flow"}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "self_link") },
		},
		{
			name:   "duplicate_target",
			links:  []DownstreamSpec{{ToRef: "a"}, {ToRef: "a", Priority: 5}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "duplicate_target") },
		},
		{
			// 能力 id 与 name@version 归一之后是同一条边：重复也要拦住
			// （否则第二次会变成 ON CONFLICT DO UPDATE —— 「重复」悄悄变成「覆盖」）。
			name:   "duplicate_target_by_normalized_ref",
			links:  []DownstreamSpec{{ToRef: f.caps["a"].ID}, {ToRef: FormatRef("a", 1)}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "duplicate_target") },
		},
		{
			// `a@01` 与 `a@1` 解析到同一行：钉死引用归一之后再判重，不能靠字面量。
			name:   "duplicate_target_by_leading_zero",
			links:  []DownstreamSpec{{ToRef: FormatRef("a", 1)}, {ToRef: "a@01"}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "duplicate_target") },
		},
		{
			name:   "priority_out_of_range_low",
			links:  []DownstreamSpec{{ToRef: "a", Priority: -1}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "priority_out_of_range") },
		},
		{
			name:   "priority_out_of_range_high",
			links:  []DownstreamSpec{{ToRef: "a", Priority: 1001}},
			assert: func(t *testing.T, err error) { codeErr(t, err, "priority_out_of_range") },
		},
		{
			name:   "too_many_links",
			links:  tooMany,
			assert: func(t *testing.T, err error) { codeErr(t, err, "too_many_links") },
		},
		{
			// literals 与 CreateEdge 同一份校验（失败是 ErrEdgeRejected，HTTP 400 edge_rejected）。
			name:  "literals_not_in_schema",
			links: []DownstreamSpec{{ToRef: "strict", Literals: json.RawMessage(`{"nope":"x"}`)}},
			assert: func(t *testing.T, err error) {
				if !errors.Is(err, ErrEdgeRejected) {
					t.Fatalf("err = %v, want ErrEdgeRejected", err)
				}
				if !strings.Contains(err.Error(), "has no such field") {
					t.Fatalf("原因没有透传: %v", err)
				}
			},
		},
		{
			name:  "literals_wrong_type",
			links: []DownstreamSpec{{ToRef: "strict", Literals: json.RawMessage(`{"to":1}`)}},
			assert: func(t *testing.T, err error) {
				if !errors.Is(err, ErrEdgeRejected) {
					t.Fatalf("err = %v, want ErrEdgeRejected", err)
				}
			},
		},
		{
			// I11：别处存在同名的能力 → 明确的跨命名空间拒绝（不是「不存在」）。
			name:  "cross_namespace",
			links: []DownstreamSpec{{ToRef: "foreign"}},
			assert: func(t *testing.T, err error) {
				var cross *CrossNamespaceError
				if !errors.As(err, &cross) {
					t.Fatalf("err = %v (%T), want *CrossNamespaceError", err, err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotEdges(t, f.db, f.nsID)
			_, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", tc.links)
			if err == nil {
				t.Fatal("非法 payload 被接受了")
			}
			tc.assert(t, err)
			after := snapshotEdges(t, f.db, f.nsID)
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Fatalf("被拒的 payload 改动了库（必须一个字节都不写）：\n之前:\n%s\n之后:\n%s",
					strings.Join(before, "\n"), strings.Join(after, "\n"))
			}
		})
	}

	// 左端能力不存在 → ErrNotFound（handler 翻 404），不是「静默建一组边」。
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing capability err = %v, want ErrNotFound", err)
	}
	// 命名空间缺失 / `*` → ErrNamespaceRequired（写端点没有跨命名空间语义）。
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, "", "flow", nil); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("empty namespace err = %v, want ErrNamespaceRequired", err)
	}
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, AllNamespaces, "flow", nil); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("AllNamespaces err = %v, want ErrNamespaceRequired", err)
	}
}

// codeErr 断言 err 是带指定机器码的 DownstreamError。
func codeErr(t *testing.T, err error, want string) {
	t.Helper()
	var rejected *DownstreamError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v (%T), want *DownstreamError", err, err)
	}
	if rejected.Code != want {
		t.Fatalf("code = %q, want %q（err: %v）", rejected.Code, want, err)
	}
}

// TestReplaceCapabilityDownstreamsNamedPort：显式 fromPort 是给**具名端口**用的
// （历史多出口能力、以及 e2e 里 outputs:["go"] 那类能力）。
//
// 三条行为必须同时成立，缺一条就会把用户的拓扑改坏：
//   - 挂在 go 上的边真的落在 go（不是被压成 out）；
//   - 只提交 go 时，另一个端口（out）上的边**一条都不动**（replace 的删除范围
//     是按端口分组的，不是整个能力）；
//   - 声明里没有的端口名 → unknown_port（I2：出口封闭，不能凭空造端口）。
func TestReplaceCapabilityDownstreamsNamedPort(t *testing.T) {
	f := newDownstreamFixture(t)
	ctx := context.Background()

	// 先在这个能力的 out 上留一条（默认端口），后面验证它不会被 go 的提交带走。
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{{ToRef: "a"}}); err != nil {
		t.Fatalf("seed out: %v", err)
	}

	// `fix` 不在 flow 的 outputs 里，但库里已有这个端口的边（历史数据）→ 允许写。
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
		 VALUES (?, 0, 'flow', 'fix', 'a', '{}', '{}', 0, ?)`,
		f.nsID, Now()); err != nil {
		t.Fatalf("seed legacy port: %v", err)
	}

	got, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{
		{ToRef: "strict", FromPort: "fix", Priority: 7},
	})
	if err != nil {
		t.Fatalf("replace on named port: %v", err)
	}
	if len(got) != 1 || got[0].FromPort != "fix" || got[0].ToRef != "strict" || got[0].Priority != 7 {
		t.Fatalf("回执不对：%+v", got)
	}

	rows := snapshotEdges(t, f.db, f.nsID)
	var flowOut, fixPort int
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r, "0|flow|out|"):
			flowOut++
		case strings.HasPrefix(r, "0|flow|fix|"):
			fixPort++
		}
	}
	if flowOut != 1 {
		t.Fatalf("只提交 fix 端口时 out 上的边不该被动，实际 %d 条\n%v", flowOut, rows)
	}
	if fixPort != 1 {
		t.Fatalf("fix 端口上应当恰好 1 条（旧的 a 被替换成 strict），实际 %d\n%v", fixPort, rows)
	}

	// 声明里没有、库里也没有的端口 → 拒绝。
	_, err = f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "flow", []DownstreamSpec{
		{ToRef: "a", FromPort: "ghost"},
	})
	var rejected *DownstreamError
	if !errors.As(err, &rejected) || rejected.Code != "unknown_port" {
		t.Fatalf("err = %v, want unknown_port", err)
	}
}
