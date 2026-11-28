package store

// limits 的「显式 null = 本层没设这一项」归一（issue #570）。
//
// 读面 GET /v1/nodes/{name}/yaml 用 `total: null` 表达「不压住下层」，写面把 YAML 解成
// map[string]any 后 null 就是一个 nil 叶子。这些用例钉住三件事：
//   - NormalizeLimits 只丢「已知维度的空叶子」，不改入参，也不放过未知维度；
//   - ValidateLimits 放行 null，但仍拒绝 0 / 负数 / 字符串（不放松正整数校验）；
//   - 一次真实写库之后，读回来的 limits 里没有 nil（强类型读取不会撞上坏数据）。

import (
	"context"
	"reflect"
	"testing"
)

func TestNormalizeLimitsDropsExplicitNulls(t *testing.T) {
	cases := []struct {
		what string
		in   map[string]any
		want map[string]any
	}{
		{"nil 入参", nil, map[string]any{}},
		{"空映射", map[string]any{}, map[string]any{}},
		{"total: null", map[string]any{"total": nil}, map[string]any{}},
		{"total 有值", map[string]any{"total": 4}, map[string]any{"total": 4}},
		{
			"perCapability 里的 null 叶子",
			map[string]any{"total": 4, "perCapability": map[string]any{"*": nil, "fixer": 1}},
			map[string]any{"total": 4, "perCapability": map[string]any{"fixer": 1}},
		},
		{"perCapability 全是 null", map[string]any{"perCapability": map[string]any{"*": nil}}, map[string]any{}},
		{"perCapability: null", map[string]any{"perCapability": nil}, map[string]any{}},
		{"perCapability: {}", map[string]any{"perCapability": map[string]any{}}, map[string]any{}},
		// 未知维度连 null 都要留着，好让 ValidateLimits 照旧拒绝（不能靠写 null 绕过校验）。
		{"未知维度 + null", map[string]any{"bogus": nil}, map[string]any{"bogus": nil}},
		// 形状错误（不是对象）原样留着，同样交给 ValidateLimits 报。
		{"perCapability 不是对象", map[string]any{"perCapability": "x"}, map[string]any{"perCapability": "x"}},
		{"非正整数原样留着", map[string]any{"total": 0}, map[string]any{"total": 0}},
	}
	for _, c := range cases {
		if got := NormalizeLimits(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s：NormalizeLimits = %#v, want %#v", c.what, got, c.want)
		}
	}
	// 不改入参：调用方可能还要拿原始输入报错 / 回显。
	in := map[string]any{"total": nil, "perCapability": map[string]any{"*": nil, "fixer": 1}}
	_ = NormalizeLimits(in)
	if _, ok := in["total"]; !ok {
		t.Fatalf("NormalizeLimits 不该改入参（total 被删了）: %#v", in)
	}
	if obj := in["perCapability"].(map[string]any); len(obj) != 2 {
		t.Fatalf("NormalizeLimits 不该改入参的 perCapability: %#v", obj)
	}
}

func TestValidateLimitsAcceptsNullButRejectsOtherShapes(t *testing.T) {
	ok := []struct {
		what   string
		limits map[string]any
	}{
		{"空", map[string]any{}},
		{"total: null", map[string]any{"total": nil}},
		{"total: 1", map[string]any{"total": 1}},
		{"perCapability: null", map[string]any{"perCapability": nil}},
		{"perCapability 缺省档 + null 叶子", map[string]any{"perCapability": map[string]any{"*": 2, "fixer": nil}}},
	}
	for _, c := range ok {
		if err := ValidateLimits(c.limits); err != nil {
			t.Fatalf("%s 应通过校验，实际 %v", c.what, err)
		}
	}
	bad := []struct {
		what   string
		limits map[string]any
	}{
		{"total: 0", map[string]any{"total": 0}},
		{"total: -1", map[string]any{"total": -1}},
		{"total: \"3\"", map[string]any{"total": "3"}},
		{"total: 1.5", map[string]any{"total": 1.5}},
		{"total: true", map[string]any{"total": true}},
		{"perCapability.X: 0", map[string]any{"perCapability": map[string]any{"*": 0}}},
		{"perCapability.X: \"2\"", map[string]any{"perCapability": map[string]any{"*": "2"}}},
		{"perCapability 不是对象", map[string]any{"perCapability": "x"}},
		{"未知维度", map[string]any{"bogus": 1}},
		{"未知维度 + null", map[string]any{"bogus": nil}},
	}
	for _, c := range bad {
		if err := ValidateLimits(c.limits); err == nil {
			t.Fatalf("%s 必须被拒绝（不能放松正整数 / 未知维度校验）", c.what)
		}
	}
}

// TestPutNodeConfigDropsNullTotal 走一遍真实写库：显式 null 不进库，读回来可用强类型读取。
func TestPutNodeConfigDropsNullTotal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n := &Node{Name: "limits-node"}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	cfg, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if len(cfg.Limits) != 0 {
		t.Fatalf("新节点的 limits 应为空对象: %#v", cfg.Limits)
	}

	// 等价于「GET yaml 的正文原样贴回」：读面给出的就是 total: null + perCapability: {}。
	cfg.Limits = map[string]any{"total": nil, "perCapability": map[string]any{"*": nil}}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("写入显式 null 的 limits 应成功，实际 %v", err)
	}
	got, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if _, has := got.Limits["total"]; has {
		t.Fatalf("显式 null 不该落库成 total 键: %#v", got.Limits)
	}
	if len(got.Limits) != 0 {
		t.Fatalf("空叶子应被丢掉（与写前 {} 逐字相同）: %#v", got.Limits)
	}
	if _, err := TypedMachineLimits(got.Limits); err != nil {
		t.Fatalf("库里不能留 nil（强类型读取必须过得去）: %v", err)
	}

	// 反向对照：给了正整数就照旧落库。
	got.Limits = map[string]any{"total": 3, "perCapability": map[string]any{"*": 2}}
	if err := db.PutNodeConfig(ctx, got); err != nil {
		t.Fatalf("put limits: %v", err)
	}
	back, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	// limits 列用 UseNumber 解码（数字保留成 json.Number），所以按整数口径比对。
	if total, ok := positiveInt(back.Limits["total"]); !ok || total != 3 {
		t.Fatalf("total 往返丢了: %#v", back.Limits)
	}

	// 再写一次 null → 键被删掉（= 不压住下层），不影响 perCapability 之外的维度。
	back.Limits = map[string]any{"total": nil}
	if err := db.PutNodeConfig(ctx, back); err != nil {
		t.Fatalf("put null total: %v", err)
	}
	final, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if _, has := final.Limits["total"]; has {
		t.Fatalf("清空 total 失败: %#v", final.Limits)
	}
}
