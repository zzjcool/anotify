package route

import (
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestDeriveKind kind 推导四个分支全覆盖（#67 验收，契约 §5）。
func TestDeriveKind(t *testing.T) {
	cases := []struct {
		name              string
		outputs           []string
		declaredByAnyNode bool
		want              Kind
	}{
		{
			name:              "no node declares → origin",
			outputs:           []string{"fix"},
			declaredByAnyNode: false,
			want:              KindOrigin,
		},
		{
			name:              "origin wins regardless of outputs",
			outputs:           nil,
			declaredByAnyNode: false,
			want:              KindOrigin,
		},
		{
			name:              "declared, outputs empty → end",
			outputs:           nil,
			declaredByAnyNode: true,
			want:              KindEnd,
		},
		{
			name:              "declared, outputs is empty slice → end",
			outputs:           []string{},
			declaredByAnyNode: true,
			want:              KindEnd,
		},
		{
			name:              "declared, outputs all blank → end",
			outputs:           []string{"", ""},
			declaredByAnyNode: true,
			want:              KindEnd,
		},
		{
			name:              "declared, one output → pipe",
			outputs:           []string{"fix"},
			declaredByAnyNode: true,
			want:              KindPipe,
		},
		{
			name:              "declared, mixed blank and real → pipe",
			outputs:           []string{"", "fix"},
			declaredByAnyNode: true,
			want:              KindPipe,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveKind(c.outputs, c.declaredByAnyNode); got != c.want {
				t.Fatalf("DeriveKind(%v, %v) = %q, want %q", c.outputs, c.declaredByAnyNode, got, c.want)
			}
		})
	}
}

// TestNodeKindMixedCapabilities 多能力混挂消歧：任一非空 → 整个节点 pipe（#67 验收）。
func TestNodeKindMixedCapabilities(t *testing.T) {
	endOnly := &store.CapabilityVersion{Name: "notify.phone", Outputs: []string{}}
	pipeCap := &store.CapabilityVersion{Name: "review", Outputs: []string{"fix"}}
	endOnly2 := &store.CapabilityVersion{Name: "test", Outputs: nil}

	cases := []struct {
		name string
		caps []*store.CapabilityVersion
		want Kind
	}{
		{name: "no capabilities declared", caps: nil, want: KindOrigin},
		{name: "only end capabilities", caps: []*store.CapabilityVersion{endOnly, endOnly2}, want: KindEnd},
		{name: "mixed end + pipe → pipe", caps: []*store.CapabilityVersion{endOnly, pipeCap}, want: KindPipe},
		{name: "pipe first → pipe", caps: []*store.CapabilityVersion{pipeCap, endOnly}, want: KindPipe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NodeKind(c.caps, len(c.caps) > 0); got != c.want {
				t.Fatalf("NodeKind = %q, want %q", got, c.want)
			}
		})
	}
	// 空声明即使在 declaredByAnyNode=true 时也算 origin
	if got := NodeKind(nil, true); got != KindOrigin {
		t.Fatalf("NodeKind(nil, true) = %q, want origin", got)
	}
}

// TestKindValues kind 字符串与契约 §5 一致。
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
	outputs := []string{"fix", "clean"}
	first := DeriveKind(outputs, true)
	for i := 0; i < 100; i++ {
		if got := DeriveKind(outputs, true); got != first {
			t.Fatalf("DeriveKind is not pure: %q vs %q", first, got)
		}
	}
	// 传入的切片不被修改（纯函数不该有副作用）
	if len(outputs) != 2 || outputs[0] != "fix" || outputs[1] != "clean" {
		t.Fatalf("DeriveKind mutated its input: %v", outputs)
	}
}
