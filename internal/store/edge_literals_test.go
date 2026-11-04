package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---------- 边的字面量覆盖（P0-c，设计 §3.1 / §3.3 / §7 P0.3） ----------

// mkLiteralsTarget 造一个**声明了字段字典**的下游能力：字面量的 key/type 都按它校验。
//
// 字段刻意覆盖全部六种 JSON 类型（含 type 为数组的可空字段），
// 这样「类型必须精确相等」这条规则在每种类型上都有对应的反例。
func mkLiteralsTarget(nsID, name string) *CapabilityVersion {
	c := mkCap(nsID, name, 1, nil)
	c.ParamsSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"to":    {"type":"string"},
			"tries": {"type":"integer"},
			"ratio": {"type":"number"},
			"dry":   {"type":"boolean"},
			"meta":  {"type":"object"},
			"tags":  {"type":"array"},
			"note":  {"type":["string","null"]}
		}
	}`)
	return c
}

// literalsFixture 建好「上游 review --fix--> 下游 notify（有字段字典）」。
func literalsFixture(t *testing.T) (db *DB, nsID string, up, down *CapabilityVersion) {
	t.Helper()
	db = newTestDB(t)
	ctx := context.Background()
	nsID, _ = newTestNS(t, db)

	up = mkCap(nsID, "review", 1, []string{"fix"})
	down = mkLiteralsTarget(nsID, "notify")
	for _, c := range []*CapabilityVersion{up, down} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability %s: %v", c.Name, err)
		}
	}
	return db, nsID, up, down
}

// TestEdgeLiteralsRoundTrip 字面量**字节级**往返一致（存什么读什么，AGENTS.md §3）。
//
// 断言的是**字节**而不是「JSON 语义相等」：库里存的就是用户给的串，
// 事后对账（trace / 审计）看到的必须和当初写进去的一字不差。
func TestEdgeLiteralsRoundTrip(t *testing.T) {
	db, nsID, up, down := literalsFixture(t)
	ctx := context.Background()

	// 故意写成「键乱序 + 带空格」：任何重排/重新 marshal 都会让它变样。
	raw := `{"to":"u_123", "tries": 3}`
	e := &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
		Literals: json.RawMessage(raw),
	}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	if string(e.Literals) != raw {
		t.Fatalf("CreateEdge 改写了 literals: %q, want %q", e.Literals, raw)
	}

	// ① ListEdges（控制台列表）原样带回
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("ListEdges 返回 %d 条，want 1", len(edges))
	}
	if string(edges[0].Literals) != raw {
		t.Fatalf("ListEdges literals = %q, want %q", edges[0].Literals, raw)
	}

	// ② EdgesFrom（派发第 ① 步）原样带回
	got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 || got[0].ToRef != capRef(down) {
		t.Fatalf("EdgesFrom = %+v, want 1 target %s", got, capRef(down))
	}
	if string(got[0].Literals) != raw {
		t.Fatalf("EdgesFrom literals = %q, want %q", got[0].Literals, raw)
	}

	// ③ 库里那一列也是同一串字节（没有二次 marshal）
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT literals FROM edges WHERE namespace_id = ? AND from_ref = ? AND from_port = ? AND to_ref = ?`,
		nsID, capRef(up), "fix", capRef(down)).Scan(&stored); err != nil {
		t.Fatalf("read raw literals: %v", err)
	}
	if stored != raw {
		t.Fatalf("库里的 literals = %q, want %q", stored, raw)
	}
}

// TestEdgeLiteralsDefaultEmpty 不传 literals = 没有覆盖：落库/读出都是 '{}'。
//
// 这一条是老数据的形状（迁移 21 补列的默认值）：行为与加列之前完全一致。
func TestEdgeLiteralsDefaultEmpty(t *testing.T) {
	db, nsID, up, down := literalsFixture(t)
	ctx := context.Background()

	e := &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	if string(e.Literals) != EdgeLiteralsEmpty {
		t.Fatalf("缺省 literals = %q, want %q", e.Literals, EdgeLiteralsEmpty)
	}

	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || string(edges[0].Literals) != EdgeLiteralsEmpty {
		t.Fatalf("ListEdges literals = %q, want {}", edges[0].Literals)
	}
	got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 || string(got[0].Literals) != EdgeLiteralsEmpty {
		t.Fatalf("EdgesFrom literals = %q, want {}", got[0].Literals)
	}
}

// TestCreateEdgeLiteralsRejected 三类硬拒：不是对象 / 键不在目标 schema / 类型不符。
//
// 全部必须返回 ErrEdgeRejected（handler 翻成 400，客户端可修），
// 且**一行都不许落库**（不能留半条边）。
func TestCreateEdgeLiteralsRejected(t *testing.T) {
	cases := []struct {
		name     string
		literals string
		wantMsg  string
	}{
		// a. 必须是 JSON 对象（数组 / 标量 / null / 坏 JSON 全拒）
		{"数组", `[1,2]`, "must be a JSON object"},
		{"标量字符串", `"nope"`, "must be a JSON object"},
		{"标量数字", `42`, "must be a JSON object"},
		{"坏 JSON", `{"to":`, "must be a JSON object"},
		// b. 键必须出现在目标 params_schema.properties 里（拼错当场拒）
		{"键不存在", `{"nope":"x"}`, "has no such field"},
		{"键大小写不符", `{"To":"u_1"}`, "has no such field"},
		// c. 类型必须精确相等（不做任何转换）
		{"integer 收 string", `{"tries":"3"}`, "want type integer, got string"},
		{"string 收 integer", `{"to":123}`, "want type string, got number"},
		{"boolean 收 object", `{"dry":{}}`, "want type boolean, got object"},
		{"object 收 array", `{"meta":[]}`, "want type object, got array"},
		{"number 收 string", `{"ratio":"1.5"}`, "want type number, got string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, nsID, up, down := literalsFixture(t)
			err := db.CreateEdge(context.Background(), &Edge{
				NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
				Literals: json.RawMessage(tc.literals),
			})
			if !errors.Is(err, ErrEdgeRejected) {
				t.Fatalf("literals=%s err = %v, want ErrEdgeRejected", tc.literals, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("literals=%s 的错误没说明原因: %v (want %q)", tc.literals, err, tc.wantMsg)
			}
			// 被拒的边不能留下半行
			edges, lerr := db.ListEdges(context.Background(), nsID, "")
			if lerr != nil {
				t.Fatalf("list edges: %v", lerr)
			}
			if len(edges) != 0 {
				t.Fatalf("被拒的边还是落库了: %+v", edges)
			}
		})
	}
}

// TestCreateEdgeLiteralsNullIsAbsent literals: null = 没有覆盖（不是错误）。
//
// 为什么这不是「少校验了一条」：可选字段的 null 在 JSON 语义里就是「没有值」，
// 与字段缺省同义（本仓 source.config.resultSchema 也是这个约定）。把它当错误拒掉，
// 会让 JS / Go 结构体客户端最容易产生的那个形状直接 400。
func TestCreateEdgeLiteralsNullIsAbsent(t *testing.T) {
	db, nsID, up, down := literalsFixture(t)
	ctx := context.Background()

	e := &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
		Literals: json.RawMessage(`null`),
	}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("literals=null 被拒: %v", err)
	}
	if string(e.Literals) != EdgeLiteralsEmpty {
		t.Fatalf("null 归一化后 = %q, want {}", e.Literals)
	}
	got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 || string(got[0].Literals) != EdgeLiteralsEmpty {
		t.Fatalf("EdgesFrom literals = %q, want {}", got[0].Literals)
	}
}

// TestCreateEdgeLiteralsAccepted 合法的字面量：每种类型都收，不转换、不改写。
func TestCreateEdgeLiteralsAccepted(t *testing.T) {
	cases := []struct{ name, literals string }{
		{"空对象", `{}`},
		{"string", `{"to":"u_123"}`},
		{"integer", `{"tries":3}`},
		{"integer 写成小数", `{"tries":3.0}`},
		{"number", `{"ratio":0.5}`},
		{"boolean", `{"dry":true}`},
		{"object", `{"meta":{"a":1}}`},
		{"array", `{"tags":["a","b"]}`},
		{"可空字段写 null", `{"note":null}`},
		{"多字段混合", `{"to":"u_1","tries":2,"dry":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, nsID, up, down := literalsFixture(t)
			ctx := context.Background()
			e := &Edge{
				NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
				Literals: json.RawMessage(tc.literals),
			}
			if err := db.CreateEdge(ctx, e); err != nil {
				t.Fatalf("literals=%s 被拒: %v", tc.literals, err)
			}
			got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
			if err != nil {
				t.Fatalf("EdgesFrom: %v", err)
			}
			if len(got) != 1 || string(got[0].Literals) != tc.literals {
				t.Fatalf("往返后 = %q, want %q（不做类型转换、不改写字节）", got[0].Literals, tc.literals)
			}
		})
	}
}

// TestCreateEdgeLiteralsIdempotent 同一 (from, port, to) 重复创建保持幂等：
// 不报错、不新增行，已有的字面量不被改写（PK 冲突 DO NOTHING）。
func TestCreateEdgeLiteralsIdempotent(t *testing.T) {
	db, nsID, up, down := literalsFixture(t)
	ctx := context.Background()

	first := &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
		Literals: json.RawMessage(`{"to":"u_1"}`),
	}
	if err := db.CreateEdge(ctx, first); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	second := &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
		Literals: json.RawMessage(`{"to":"u_2"}`),
	}
	if err := db.CreateEdge(ctx, second); err != nil {
		t.Fatalf("重复建同一条边不该报错: %v", err)
	}
	got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("重复边产生了 %d 行，want 1", len(got))
	}
	if string(got[0].Literals) != `{"to":"u_1"}` {
		t.Fatalf("重复创建改写了已存在的字面量: %q", got[0].Literals)
	}
}

// TestCreateEdgeLiteralsEntryEdge 入口边（fromIsSource）同样按目标 schema 校验——
// 字面量校验与边的两种左端无关，判据永远是**目标能力**的 params_schema。
func TestCreateEdgeLiteralsEntryEdge(t *testing.T) {
	db, nsID, _, down := literalsFixture(t)
	ctx := context.Background()

	err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "pull_request.opened",
		ToRef: down.ID, Literals: json.RawMessage(`{"tries":"many"}`),
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("入口边的非法字面量 err = %v, want ErrEdgeRejected", err)
	}

	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "pull_request.opened",
		ToRef: down.ID, Literals: json.RawMessage(`{"tries":2}`),
	}); err != nil {
		t.Fatalf("入口边的合法字面量被拒: %v", err)
	}
	got, err := db.EdgesFrom(ctx, nsID, true, "github", "pull_request.opened")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 || string(got[0].Literals) != `{"tries":2}` {
		t.Fatalf("入口边 EdgesFrom = %+v", got)
	}
}

// TestMigration21EdgeLiterals 老库（迁移 12 之前的形状）升级到最新：
// edges 补上 literals 列，既有行一律 '{}'（= 没有覆盖，行为与加列之前一致）。
//
// 走的是真实历史路径：schema.sql 的建表语句只对新库生效，老库只能靠
// migrationHooks[21]（migrateEdgeLiterals）补列——那条路径不测就不会被跑到。
func TestMigration21EdgeLiterals(t *testing.T) {
	path := legacyPre12DB(t, false)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("升级老库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if !columnNames(t, db, "edges")["literals"] {
		t.Fatal("升级后 edges 没有 literals 列（migrationHooks[21] 没跑到）")
	}
	if v := mustMaxVersion(t, db); v != len(mustMigrations(t)) {
		t.Fatalf("升级后迁移版本 = %d, want %d", v, len(mustMigrations(t)))
	}
	// 老行（预置的那条边）必须是 '{}'，一行不少、一字节不多。
	var stored string
	if err := db.QueryRow(`SELECT literals FROM edges`).Scan(&stored); err != nil {
		t.Fatalf("读老行的 literals: %v", err)
	}
	if stored != EdgeLiteralsEmpty {
		t.Fatalf("老行 literals = %q, want %q", stored, EdgeLiteralsEmpty)
	}
}

// TestGraphHashIncludesLiterals 守住「改常量 = 改图」这条：graph_versions 是事后
// 追责的底图，只改某条边的字面量同样要落新版本，否则「当时那条链路长什么样」查不出来。
func TestGraphHashIncludesLiterals(t *testing.T) {
	base := []*Edge{{FromRef: "a", FromPort: "ok", ToRef: "b"}}

	none, err := graphContentHash(base)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	withLit := []*Edge{{FromRef: "a", FromPort: "ok", ToRef: "b",
		Literals: json.RawMessage(`{"to":"u_1"}`)}}
	lit, err := graphContentHash(withLit)
	if err != nil {
		t.Fatalf("hash with literals: %v", err)
	}
	if lit == none {
		t.Fatal("加了字面量后 graph hash 没变：改常量不会产生新图版本")
	}

	// 键序不同、空白不同，但语义相同 → 同一个 hash（不堆垃圾版本）
	same1 := []*Edge{{FromRef: "a", FromPort: "ok", ToRef: "b",
		Literals: json.RawMessage(`{ "b": 2, "a": 1 }`)}}
	same2 := []*Edge{{FromRef: "a", FromPort: "ok", ToRef: "b",
		Literals: json.RawMessage(`{"a":1,"b":2}`)}}
	h1, err := graphContentHash(same1)
	if err != nil {
		t.Fatalf("hash same1: %v", err)
	}
	h2, err := graphContentHash(same2)
	if err != nil {
		t.Fatalf("hash same2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("语义相同的字面量得到了不同 hash：%s vs %s", h1, h2)
	}

	// 空覆盖等价于没有覆盖（老数据 DEFAULT '{}' 不该改变既有图版本）
	empty := []*Edge{{FromRef: "a", FromPort: "ok", ToRef: "b",
		Literals: json.RawMessage(`{}`)}}
	he, err := graphContentHash(empty)
	if err != nil {
		t.Fatalf("hash empty: %v", err)
	}
	if he != none {
		t.Fatal("空字面量改变了 graph hash：老数据升级后会凭空多出一个图版本")
	}
}
