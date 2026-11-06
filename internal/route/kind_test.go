package route

import (
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件钉住 kind 推导（契约 §5）。**#383 之后以边为准**：`outputs` 是产物里的
// 端口声明，不再是拓扑的真相 —— 一个建时声明 `outputs: []` 的能力只要有了出边，
// 就不再是「链到这里结束」（那正是用户报了缺口的那一屏）。

// TestDeriveKind kind 推导三个分支全覆盖（纯函数，输入只有边的接线事实）。
func TestDeriveKind(t *testing.T) {
	cases := []struct {
		name   string
		hasOut bool
		hasIn  bool
		want   Kind
	}{
		{name: "无出边 → end（不管有没有入边）", hasOut: false, hasIn: false, want: KindEnd},
		{name: "无出边、有入边 → end", hasOut: false, hasIn: true, want: KindEnd},
		{name: "有出边、无入边 → origin（链的起点）", hasOut: true, hasIn: false, want: KindOrigin},
		{name: "有出边、有入边 → pipe（中间段）", hasOut: true, hasIn: true, want: KindPipe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveKind(c.hasOut, c.hasIn); got != c.want {
				t.Fatalf("DeriveKind(hasOut=%v, hasIn=%v) = %q, want %q",
					c.hasOut, c.hasIn, got, c.want)
			}
		})
	}
}

// TestKindForCapabilityFollowsEdges #383 的核心回归：
//
//	outputs: [] + 有出边  → **不再是 end**（有入边 → pipe；没有 → origin）
//	outputs: [out] + 无边 → end（声明过的端口不再单独让它变成 pipe）
func TestKindForCapabilityFollowsEdges(t *testing.T) {
	t.Run("终端声明 + 新边（有入边）→ pipe", func(t *testing.T) {
		c := &store.CapabilityVersion{Name: "sink", Version: 1, Outputs: []string{}}
		shape := &store.EdgeShape{OutPorts: []string{store.ImplicitPort}, HasIn: true}
		if got := KindForCapability(c, shape); got != KindPipe {
			t.Fatalf("kind = %q, want pipe（outputs 空但有出边，不能再是 end）", got)
		}
	})

	t.Run("终端声明 + 新边（无入边）→ origin", func(t *testing.T) {
		c := &store.CapabilityVersion{Name: "sink", Version: 1, Outputs: []string{}}
		shape := &store.EdgeShape{OutPorts: []string{store.ImplicitPort}}
		if got := KindForCapability(c, shape); got != KindOrigin {
			t.Fatalf("kind = %q, want origin（有出边、没有上游）", got)
		}
		if got := KindForCapability(c, shape); got == KindEnd {
			t.Fatal("outputs 为空 + 有出边的能力仍然被判成 end（#383 的缺口回来了）")
		}
	})

	t.Run("声明了端口但没有任何边 → end", func(t *testing.T) {
		c := &store.CapabilityVersion{Name: "declared", Version: 1, Outputs: []string{"fix"}}
		if got := KindForCapability(c, &store.EdgeShape{}); got != KindEnd {
			t.Fatalf("kind = %q, want end（没有出边 = 链在这里结束）", got)
		}
	})

	t.Run("什么都没有 → end", func(t *testing.T) {
		c := &store.CapabilityVersion{Name: "lonely", Version: 1}
		if got := KindForCapability(c, nil); got != KindEnd {
			t.Fatalf("kind = %q, want end", got)
		}
	})
}

// TestKindForCapabilityBuiltinOverrideWins 内置特例仍然优先于边推导：
//
//	notify.phone（中枢执行）→ end   中枢执行完就结束，不产生下游任务（executor 不 emit）
//	内置起点（已随 #110 删除，这里守的是「Origin 特例」这条分支本身）
//
// 为什么要钉住：kind 存不落库（I3），特例散落三处必然漂移——notify.phone 曾在
// 能力页显示成 origin，误导用户把它当事件来源。
func TestKindForCapabilityBuiltinOverrideWins(t *testing.T) {
	phone := &store.CapabilityVersion{Name: "notify.phone", Version: 1, Outputs: []string{}}
	// 即使（在坏数据里）被接了出边，中枢执行的能力仍然不产生下游。
	if got := KindForCapability(phone, &store.EdgeShape{OutPorts: []string{store.ImplicitPort}, HasIn: true}); got != KindEnd {
		t.Fatalf("notify.phone kind = %q, want end（内置中枢特例优先）", got)
	}
	// nil 能力定义不是 origin 就是 end 之外的东西都不该出现。
	if got := KindForCapability(nil, nil); got != KindOrigin {
		t.Fatalf("KindForCapability(nil) = %q, want origin", got)
	}
}

// TestNodeKindMixedCapabilities 多能力混挂消歧：任一能力有出边 → 节点有出口。
func TestNodeKindMixedCapabilities(t *testing.T) {
	cases := []struct {
		name    string
		shapes  []*store.EdgeShape
		want    Kind
		comment string
	}{
		{
			name:   "都没有边 → end",
			shapes: []*store.EdgeShape{{}, {}},
			want:   KindEnd,
		},
		{
			name:   "一个能力有出边、另一个没有 → 节点是 origin",
			shapes: []*store.EdgeShape{{OutPorts: []string{"fix"}}, {}},
			want:   KindOrigin,
		},
		{
			name:   "一个能力有入边、另一个有出边 → 节点是 pipe",
			shapes: []*store.EdgeShape{{HasIn: true}, {OutPorts: []string{"fix"}}},
			want:   KindPipe,
		},
		{
			name:   "nil 元素被跳过",
			shapes: []*store.EdgeShape{nil, {OutPorts: []string{"fix"}, HasIn: true}},
			want:   KindPipe,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NodeKind(c.shapes); got != c.want {
				t.Fatalf("NodeKind = %q, want %q（%s）", got, c.want, c.comment)
			}
		})
	}
}

// TestKindValues kind 字符串与契约 §5 一致（不新造 kind）。
func TestKindValues(t *testing.T) {
	want := map[Kind]string{
		KindOrigin: "origin",
		KindEnd:    "end",
		KindPipe:   "pipe",
	}
	for k, s := range want {
		if string(k) != s {
			t.Errorf("kind %q != %q", k, s)
		}
	}
	if len(want) != 3 {
		t.Fatalf("want exactly 3 kinds, got %d", len(want))
	}
}

// TestDeriveKindIsPure kind 推导不查库、不缓存：同样输入永远同样输出。
func TestDeriveKindIsPure(t *testing.T) {
	first := DeriveKind(true, true)
	for i := 0; i < 100; i++ {
		if got := DeriveKind(true, true); got != first {
			t.Fatalf("DeriveKind is not pure: %q vs %q", first, got)
		}
	}
}

// TestKindForCapabilityDoesNotMutateShape 纯函数不该改写推导输入（shape 常来自 store 缓存）。
func TestKindForCapabilityDoesNotMutateShape(t *testing.T) {
	c := &store.CapabilityVersion{Name: "cap", Version: 1, Outputs: []string{"fix"}}
	shape := &store.EdgeShape{OutPorts: []string{"fix"}, HasIn: true}
	_ = KindForCapability(c, shape)
	if len(shape.OutPorts) != 1 || shape.OutPorts[0] != "fix" || !shape.HasIn {
		t.Fatalf("KindForCapability 改写了 shape：%+v", shape)
	}
	if len(c.Outputs) != 1 || c.Outputs[0] != "fix" {
		t.Fatalf("KindForCapability 改写了能力定义：%v", c.Outputs)
	}
}
