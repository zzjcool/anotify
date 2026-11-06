package route

import (
	"reflect"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件钉住「有效出口集合」这条推导（#383：以边为准）。
//
// 缺口是真实的：用户在能力编辑里给一个建时声明 `outputs: []`（终端）的能力加了
// 下游 —— 边建好了，但派发只下发 `outputs`（空）→ 节点拿到的端口契约里没有那个
// 出口 → emit 当场报「终端能力」，那条新边永远不触发。

// TestMergePortsUnion 并集（不是替换）：声明过的端口一个都不能被边吞掉。
func TestMergePortsUnion(t *testing.T) {
	cases := []struct {
		name     string
		declared []string
		fromEdge []string
		want     []string
	}{
		{
			name:     "outputs 为空 + 一条出边 → 那个出口自动可用（#383 的核心）",
			declared: []string{},
			fromEdge: []string{store.ImplicitPort},
			want:     []string{"out"},
		},
		{
			name:     "什么都没有 → 空集合（终端能力行为不变）",
			declared: nil,
			fromEdge: nil,
			want:     []string{},
		},
		{
			name:     "有声明、没边 → 只剩声明（老行为）",
			declared: []string{"out"},
			fromEdge: nil,
			want:     []string{"out"},
		},
		{
			name:     "历史多出口：只有 a 接了边，b 不能被吞掉",
			declared: []string{"a", "b"},
			fromEdge: []string{"a"},
			want:     []string{"a", "b"},
		},
		{
			name:     "边上的新出口要补进来",
			declared: []string{"out"},
			fromEdge: []string{"extra", "out"},
			want:     []string{"extra", "out"},
		},
		{
			name:     "去重 + 排序稳定（帧字节必须可复现）",
			declared: []string{"b", "a", "b"},
			fromEdge: []string{"a", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "空串不是可上报的出口名，丢掉",
			declared: []string{"", "a"},
			fromEdge: []string{""},
			want:     []string{"a"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergePorts(c.declared, c.fromEdge)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MergePorts(%v, %v) = %v, want %v", c.declared, c.fromEdge, got, c.want)
			}
			if got == nil {
				// 空列表是 [] 不是 nil（I9）：它会直接进 assign 帧与 BuildExpects。
				t.Fatal("MergePorts 返回 nil，want 非 nil 的 []")
			}
		})
	}
}

// TestMergePortsDoesNotMutateInputs 纯函数：输入切片不能被改写（调用方常把
// capDef.Outputs 直接传进来，改了它等于改只读的能力定义缓存）。
func TestMergePortsDoesNotMutateInputs(t *testing.T) {
	declared := []string{"b", "a"}
	fromEdge := []string{"c"}
	_ = MergePorts(declared, fromEdge)
	if !reflect.DeepEqual(declared, []string{"b", "a"}) || !reflect.DeepEqual(fromEdge, []string{"c"}) {
		t.Fatalf("MergePorts 改写了入参：declared=%v fromEdge=%v", declared, fromEdge)
	}
}

// TestEffectivePortsScenario 把 #383 的验收场景写成一条推导：
//
//	outputs: []  + 1 条出边        → ports 含 out
//	outputs: []  + 无出边          → ports 仍为空（终端）
//	outputs: [out] + 无出边        → ports 仍是 [out]（保持现状）
//	outputs: [a,b] + 只有 a 有边   → ports 仍含两个
func TestEffectivePortsScenario(t *testing.T) {
	cases := []struct {
		name     string
		declared []string
		shape    *store.EdgeShape
		want     []string
	}{
		{"终端 + 新边", []string{}, &store.EdgeShape{OutPorts: []string{"out"}, HasIn: true}, []string{"out"}},
		{"终端无边", []string{}, &store.EdgeShape{}, []string{}},
		{"声明 out 无边", []string{"out"}, &store.EdgeShape{}, []string{"out"}},
		{"多出口历史数据", []string{"a", "b"}, &store.EdgeShape{OutPorts: []string{"a"}}, []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergePorts(c.declared, c.shape.OutPorts)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ports = %v, want %v", got, c.want)
			}
		})
	}
}
