package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---------- 入口边的引用模板（P2.1，设计 §11.5 选项 A） ----------
//
// 边界只有一条：入口边（from_is_source=1）允许 mapping，能力边（from_is_source=0）
// 保存时硬拒——外部世界不会替你拼参数，而能力边的参数怎么拼是实现的内部事务。

// TestEntryEdgeMappingRoundTrip 入口边带模板：建得出来、字节级往返（存什么读什么）。
func TestEntryEdgeMappingRoundTrip(t *testing.T) {
	db, nsID, _, down := literalsFixture(t)
	ctx := context.Background()

	// 故意写成「键乱序 + 带空格」：任何重排/重新 marshal 都会让它变样。
	raw := `{"to":"{{pull_request.user.login}}", "note": "{{pull_request.title}}"}`
	e := &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "*",
		ToRef: down.ID, Mapping: json.RawMessage(raw),
	}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create entry edge: %v", err)
	}
	if string(e.Mapping) != raw {
		t.Fatalf("CreateEdge 改写了 mapping: %q, want %q", e.Mapping, raw)
	}
	// 字面量也得一并归一（不让「只填了 mapping」变成 null）
	if string(e.Literals) != EdgeLiteralsEmpty {
		t.Fatalf("literals = %q, want %q", e.Literals, EdgeLiteralsEmpty)
	}

	// ① ListEdges（控制台列表）原样带回
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("ListEdges 返回 %d 条，want 1", len(edges))
	}
	if string(edges[0].Mapping) != raw {
		t.Fatalf("ListEdges 的 mapping = %q, want %q", edges[0].Mapping, raw)
	}

	// ② EdgesFrom（派发第 ① 步）也带上它
	targets, err := db.EdgesFrom(ctx, nsID, true, "github", "pull_request.opened")
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	if len(targets) != 1 || targets[0].ToRef != capRef(down) {
		t.Fatalf("入口边目标 = %+v, want 一条指向 %s", targets, capRef(down))
	}
	if string(targets[0].Mapping) != raw {
		t.Fatalf("EdgesFrom 的 mapping = %q, want %q", targets[0].Mapping, raw)
	}
}

// TestCapabilityEdgeMappingRejected 能力边带非空模板 → ErrEdgeRejected（硬拒，不是忽略）。
//
// 理由（设计 §8）：能力边的参数由实现 emit 的 data 提供，中间再插一层引用映射
// 只会制造晚发现的 param_missing / param_type。
func TestCapabilityEdgeMappingRejected(t *testing.T) {
	db, nsID, up, down := literalsFixture(t)
	ctx := context.Background()
	err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
		Mapping: json.RawMessage(`{"to":"{{x}}"}`),
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("err = %v, want ErrEdgeRejected", err)
	}
	if !strings.Contains(err.Error(), "入口边") {
		t.Errorf("错误没说清「只允许入口边」: %v", err)
	}
	// 什么都没写进去
	if n := countRows(t, db, "edges", "1 = 1"); n != 0 {
		t.Fatalf("被拒的边仍写进了库（%d 行）", n)
	}
}

// TestEdgeMappingUndeclaredKeyRejected 模板的键必须在目标 params_schema 里（拼错当场拒）。
func TestEdgeMappingUndeclaredKeyRejected(t *testing.T) {
	db, nsID, _, down := literalsFixture(t)
	ctx := context.Background()
	err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "*",
		ToRef: down.ID, Mapping: json.RawMessage(`{"titel":"{{pull_request.title}}"}`),
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("err = %v, want ErrEdgeRejected", err)
	}
	if !strings.Contains(err.Error(), "mapping.titel") {
		t.Errorf("错误没指出是哪个键: %v", err)
	}
}

// TestEdgeMappingNullNormalizedToEmpty null / 空 / 缺省一律归一成 '{}'。
//
// 为什么必须归一：写入侧统一了，读侧与哈希侧才不用区分「null 与空对象」；
// 前端/客户端也最容易产生 `"mapping": null` 这种形状（可选项的通用写法）。
func TestEdgeMappingNullNormalizedToEmpty(t *testing.T) {
	db, nsID, _, down := literalsFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		raw  string
	}{
		{"缺省", ""},
		{"显式 null", "null"},
		{"空对象", "{}"},
		{"带空白的空对象", " { } "},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Edge{
				NamespaceID: nsID, FromIsSource: true, FromRef: "github",
				FromPort: "pull_request." + strings.Repeat("x", i+1), // 端口各不相同（主键）
				ToRef:    down.ID, Mapping: json.RawMessage(c.raw),
			}
			if err := db.CreateEdge(ctx, e); err != nil {
				t.Fatalf("create edge: %v", err)
			}
			if string(e.Mapping) != EdgeMappingEmpty {
				t.Fatalf("mapping = %q, want %q", e.Mapping, EdgeMappingEmpty)
			}
			var stored string
			if err := db.QueryRow(`SELECT mapping FROM edges WHERE namespace_id = ? AND to_ref = ? AND from_port = ?`,
				nsID, capRef(down), e.FromPort).Scan(&stored); err != nil {
				t.Fatalf("read stored mapping: %v", err)
			}
			if stored != EdgeMappingEmpty {
				t.Fatalf("库里的 mapping = %q, want %q", stored, EdgeMappingEmpty)
			}
		})
	}
}

// TestEdgeMappingShapeRejected 模板值的形状判定（与 route 的配置期口径一致）。
//
// 必须拒：半截占位符、占位符里有运算符、路径写成 a..b、as 不在枚举里、
// {var:""}（变量名为空）、from+var 并存、只写 default 的映射声明、常量类型与 schema 不符。
// 两条必须放：只含占位符的值（类型要触发时才知道）、旧对象形态 {from}/▶{var}。
func TestEdgeMappingShapeRejected(t *testing.T) {
	db, nsID, _, down := literalsFixture(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		mapping string
		wantErr []string // 空 = 必须通过
	}{
		{name: "半截占位符", mapping: `{"to":"PR {{title"}`, wantErr: []string{"占位符没有闭合"}},
		{name: "占位符里带运算符", mapping: `{"to":"{{a + b}}"}`, wantErr: []string{"占位符不合法"}},
		{name: "路径形态不合法", mapping: `{"to":"{{a..b}}"}`, wantErr: []string{"路径不合法"}},
		{name: "as 不在枚举里", mapping: `{"tries":{"from":"n","as":"integer"}}`, wantErr: []string{"as 只能是"}},
		{name: "变量名为空", mapping: `{"to":{"var":""}}`, wantErr: []string{"变量名为空"}},
		{name: "from 与 var 并存", mapping: `{"to":{"from":"a","var":"b"}}`, wantErr: []string{"只能选一个"}},
		{name: "只写了 default 的映射声明", mapping: `{"to":{"default":"x"}}`, wantErr: []string{"缺少 from 或 var"}},
		{name: "常量类型与 schema 不符", mapping: `{"tries":"abc"}`, wantErr: []string{"want type integer"}},
		{name: "只含占位符的 integer 字段（配置期不判类型）", mapping: `{"tries":"{{n}}"}`},
		{name: "旧对象形态 from", mapping: `{"to":{"from":"pull_request.user.login"}}`},
		{name: "旧对象形态 var", mapping: `{"to":{"var":"author","default":"x"}}`},
		{name: "完整的模板组合", mapping: `{"to":"{{pr.user.login}}","note":"PR #{{pr.number}}: {{pr.title}}"}`},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := db.CreateEdge(ctx, &Edge{
				NamespaceID: nsID, FromIsSource: true, FromRef: "github",
				FromPort: "case-" + strings.Repeat("x", i+1), ToRef: down.ID,
				Mapping: json.RawMessage(c.mapping),
			})
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("不该报错, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（含 %v），但保存成功了", c.wantErr)
			}
			if !errors.Is(err, ErrEdgeRejected) {
				t.Fatalf("err = %v, want ErrEdgeRejected（客户端可修 → 400）", err)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误 %q 不含 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestEntryEdgeWildcardPortLookup 入口边的端口匹配：精确优先、"*" 兜底、同一目标只返回一次。
//
// 为什么要有 "*"：源能收什么事件类型由**外部系统**决定（GitHub 发什么 type 就是什么），
// 让用户为每个 type 手建一条边是多余的心智负担；源的目标（P2.1）就长这样。
// 为什么同一目标要去重：一条精确边 + 一条 "*" 边指向同一个下游时，那个下游
// 只能收到**一次**派发（否则一个事件变成两个任务）。
func TestEntryEdgeWildcardPortLookup(t *testing.T) {
	db, nsID, capA, capB := entryEdgeFixture(t)
	ctx := context.Background()

	create := func(port string, to *CapabilityVersion, mapping string) {
		t.Helper()
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID: nsID, FromIsSource: true, FromRef: "github",
			FromPort: port, ToRef: to.ID, Mapping: json.RawMessage(mapping),
		}); err != nil {
			t.Fatalf("create entry edge %s→%s: %v", port, to.Name, err)
		}
	}
	// capA 同时有精确边（首选）与 "*" 边（同目标旧配置）；capB 只有 "*"。
	create("pull_request.opened", capA, `{"title":"{{pr.title}}"}`)
	create("*", capA, `{"title":"FROM WILDCARD"}`)
	create("*", capB, `{}`)

	got, err := db.EdgesFrom(ctx, nsID, true, "github", "pull_request.opened")
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("目标数 = %d, want 2（capA 去重 + capB 从 '*' 来）: %+v", len(got), got)
	}
	byRef := map[string]LinkTarget{}
	for _, tg := range got {
		byRef[tg.ToRef] = tg
	}
	if string(byRef[capRef(capA)].Mapping) != `{"title":"{{pr.title}}"}` {
		t.Errorf("精确端口的边应该赢: %q", byRef[capRef(capA)].Mapping)
	}
	if _, ok := byRef[capRef(capB)]; !ok {
		t.Error("'*' 边没参与匹配（源收到别的事件类型时就派不出去了）")
	}

	// 另一个事件类型：两条目标都从 "*" 来（capA 用 "*" 那条的模板）
	got, err = db.EdgesFrom(ctx, nsID, true, "github", "issues.opened")
	if err != nil {
		t.Fatalf("edges from (other port): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("目标数 = %d, want 2（都从 '*' 来）", len(got))
	}
	for _, tg := range got {
		if tg.ToRef == capRef(capA) && !strings.Contains(string(tg.Mapping), "FROM WILDCARD") {
			t.Errorf("没命中精确端口时应该用 '*' 那条: %q", tg.Mapping)
		}
	}

	// 能力边不放宽：from_port='*' 不是能力的出口名，查不到任何边。
	if n, err := db.EdgesFrom(ctx, nsID, false, capA.Name, "*"); err != nil || len(n) != 0 {
		t.Fatalf("能力边不该匹配 '*': %v, %v", n, err)
	}
}

// entryEdgeFixture 建好「一个源 + 两个声明了字段字典的目标能力」（入口边用）。
func entryEdgeFixture(t *testing.T) (db *DB, nsID string, capA, capB *CapabilityVersion) {
	t.Helper()
	db = newTestDB(t)
	ctx := context.Background()
	nsID, _ = newTestNS(t, db)

	capA = mkCap(nsID, "notify", 1, nil)
	capA.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)
	capB = mkCap(nsID, "archive", 1, nil)
	capB.ParamsSchema = json.RawMessage(`{"type":"object"}`)
	for _, c := range []*CapabilityVersion{capA, capB} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability %s: %v", c.Name, err)
		}
	}
	return db, nsID, capA, capB
}

// TestGraphHashIncludesMapping 守住「改模板 = 改图」这条（P2.1）。
//
// graph_versions 是事后追责的底图：同一条入口边换一份模板，喂给下游的参数就变了，
// 必须落一个新版本；反过来，语义相同（键序/空白不同）或空模板不得改变既有 hash
// ——否则只会在 graph_versions 里堆垃圾（契约 §10.2 的「按需落版」失效）。
func TestGraphHashIncludesMapping(t *testing.T) {
	base := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b"}}

	none, err := graphContentHash(base)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	withMapping := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b",
		Mapping: json.RawMessage(`{"title":"{{pr.title}}"}`)}}
	mapped, err := graphContentHash(withMapping)
	if err != nil {
		t.Fatalf("hash with mapping: %v", err)
	}
	if mapped == none {
		t.Fatal("加了模板后 graph hash 没变：改模板不会产生新图版本")
	}

	// 换一份模板（同一个键，不同的取值来源）→ 依然是新版本
	other := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b",
		Mapping: json.RawMessage(`{"title":"{{head_commit.message}}"}`)}}
	otherHash, err := graphContentHash(other)
	if err != nil {
		t.Fatalf("hash other mapping: %v", err)
	}
	if otherHash == mapped {
		t.Fatal("改了模板内容但 graph hash 没变")
	}

	// 键序/空白不同、语义相同 → 同一 hash
	same1 := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b",
		Mapping: json.RawMessage(`{ "b": "{{y}}", "a": "{{x}}" }`)}}
	same2 := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b",
		Mapping: json.RawMessage(`{"a":"{{x}}","b":"{{y}}"}`)}}
	h1, err := graphContentHash(same1)
	if err != nil {
		t.Fatalf("hash same1: %v", err)
	}
	h2, err := graphContentHash(same2)
	if err != nil {
		t.Fatalf("hash same2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("语义相同的模板得到了不同 hash：%s vs %s", h1, h2)
	}

	// 空模板等价于没有模板（老数据补列后 DEFAULT '{}' 不该凭空改变图版本）
	empty := []*Edge{{FromIsSource: true, FromRef: "github", FromPort: "*", ToRef: "b",
		Mapping: json.RawMessage(`{}`)}}
	he, err := graphContentHash(empty)
	if err != nil {
		t.Fatalf("hash empty: %v", err)
	}
	if he != none {
		t.Fatal("空模板改变了 graph hash：老数据升级后会凭空多出一个图版本")
	}
}
