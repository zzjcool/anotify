package server

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// mergeEvents / mergeTasks 的「单命名空间快路径」（#243）。
//
// 为什么值得单独钉住：快路径把每请求一次的 sort.SliceStable 换成了直接透传，
// 换掉的是「顺序由谁保证」这个隐含契约 —— store.ListEvents / ListTasks 的 SQL
// 已经是 `ORDER BY created_at DESC, id DESC`。一旦有人改动其中一边（SQL 的排序列、
// 或快路径的前提），线上表现是「事件流顺序偶尔乱」这种最难查的回归。
// 所以这里既钉排序语义（跨组仍然要归并重排），也钉 I9（空列表给 [] 不给 null）。

func ev(id string, createdAt int64) *store.Event {
	return &store.Event{ID: id, NamespaceID: "ns_test", CreatedAt: createdAt}
}

func task(id string, createdAt int64) *store.Task {
	return &store.Task{ID: id, NamespaceID: "ns_test", CreatedAt: createdAt}
}

func eventIDs(evs []*store.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func taskIDs(ts []*store.Task) []string {
	out := make([]string, len(ts))
	for i, tt := range ts {
		out[i] = tt.ID
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMergeEventsSingleGroupKeepsStoreOrder 单命名空间：store 已排好的顺序原样透传，
// 且元素是同一批指针（不复制、不重排）。
func TestMergeEventsSingleGroupKeepsStoreOrder(t *testing.T) {
	in := []*store.Event{ev("e3", 300), ev("e2", 200), ev("e1", 100)}
	out := mergeEvents(10, in)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("第 %d 个元素不是入参的同一个指针：很可能又被复制/重排了一遍", i)
		}
	}
	if got, want := eventIDs(out), []string{"e3", "e2", "e1"}; !eqStrings(got, want) {
		t.Fatalf("顺序 = %v, want %v", got, want)
	}
}

// TestMergeEventsSingleGroupLimit 单命名空间的截断仍然生效。
func TestMergeEventsSingleGroupLimit(t *testing.T) {
	in := []*store.Event{ev("e3", 300), ev("e2", 200), ev("e1", 100)}
	out := mergeEvents(2, in)
	if got, want := eventIDs(out), []string{"e3", "e2"}; !eqStrings(got, want) {
		t.Fatalf("limit=2 结果 = %v, want %v", got, want)
	}
	// limit<=0 表示「不截断」（调用方语义在 console handler 里已钳过）
	if got := mergeEvents(0, in); len(got) != 3 {
		t.Fatalf("limit=0 不该截断，len = %d", len(got))
	}
}

// TestMergeEventsSingleGroupEmptyIsArray 空分组必须归一成非 nil 空切片（I9：[] 不是 null）。
func TestMergeEventsSingleGroupEmptyIsArray(t *testing.T) {
	for name, groups := range map[string][][]*store.Event{
		"nil 分组":   nil,
		"空切片分组":    {{}},
		"nil 元素分组": {nil},
	} {
		out := mergeEvents(10, groups...)
		if out == nil {
			t.Fatalf("%s：返回 nil，序列化会变成 null（I9）", name)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if string(b) != "[]" {
			t.Fatalf("%s: JSON = %s, want []", name, b)
		}
	}
}

// TestMergeEventsMultiGroupStillSorts 跨命名空间（`*`）仍然归并 + 按 store 口径重排。
func TestMergeEventsMultiGroupStillSorts(t *testing.T) {
	a := []*store.Event{ev("a2", 200), ev("a1", 100)}
	b := []*store.Event{ev("b3", 300), ev("b1", 100)}
	out := mergeEvents(10, a, b)
	// createdAt DESC，同 createdAt 用 id DESC 破平局（与 store 的 ORDER BY 同口径）：
	// a1 与 b1 同为 100，b1 > a1 ⇒ b1 在前。
	if got, want := eventIDs(out), []string{"b3", "a2", "b1", "a1"}; !eqStrings(got, want) {
		t.Fatalf("跨命名空间合并顺序 = %v, want %v", got, want)
	}
	if got := mergeEvents(2, a, b); len(got) != 2 {
		t.Fatalf("跨命名空间 limit=2 len = %d", len(got))
	}
}

// TestMergeTasksSingleGroupKeepsStoreOrder 任务列表同一条快路径。
func TestMergeTasksSingleGroupKeepsStoreOrder(t *testing.T) {
	in := []*store.Task{task("t3", 300), task("t2", 200), task("t1", 100)}
	out := mergeTasks(10, in)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("第 %d 个元素不是入参的同一个指针", i)
		}
	}
	if got, want := taskIDs(out), []string{"t3", "t2", "t1"}; !eqStrings(got, want) {
		t.Fatalf("顺序 = %v, want %v", got, want)
	}
	if out := mergeTasks(10); out == nil {
		t.Fatalf("空分组返回 nil（I9 要求 []）")
	}
	// 单组必须真的截断（量 2 条，而不是给一个大于长度的 limit 走个过场）
	if got := taskIDs(mergeTasks(2, in)); !eqStrings(got, []string{"t3", "t2"}) {
		t.Fatalf("单组 limit=2 结果 = %v, want [t3 t2]", got)
	}
	a := []*store.Task{task("a2", 200), task("a1", 100)}
	b := []*store.Task{task("b3", 300), task("b1", 100)}
	if got, want := taskIDs(mergeTasks(10, a, b)), []string{"b3", "a2", "b1", "a1"}; !eqStrings(got, want) {
		t.Fatalf("跨命名空间合并顺序 = %v, want %v", got, want)
	}
	// 跨组的 limit 截断是另一条独立逻辑（先归并重排、再截断），必须单独钉：
	// 变异「跨组截断失效」此前是全绿的（mergeEvents 有断言、mergeTasks 漏了）。
	if got := taskIDs(mergeTasks(2, a, b)); !eqStrings(got, []string{"b3", "a2"}) {
		t.Fatalf("跨命名空间 limit=2 结果 = %v, want [b3 a2]", got)
	}
}
