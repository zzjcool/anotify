package schema

import "testing"

func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref    string
		name   string
		ver    int
		pinned bool
	}{
		{"a", "a", 0, false},
		{" a ", "a", 0, false},   // 前后空白不影响判据
		{"a@2", "a", 2, true},    // 钉版
		{"a@0", "a", -1, true},   // 0 不是版本：钉死但非法（fail closed）
		{"a@", "a", -1, true},    // 空版本
		{"a@02", "a", -1, true},  // 前导零
		{"a@x", "a", -1, true},   // 非数字
		{"a@b@2", "a", -1, true}, // 第二个 @ 落在版本串里
	}
	for _, tc := range cases {
		name, ver, pinned := SplitRef(tc.ref)
		if name != tc.name || ver != tc.ver || pinned != tc.pinned {
			t.Fatalf("SplitRef(%q) = (%q,%d,%v)，want (%q,%d,%v)",
				tc.ref, name, ver, pinned, tc.name, tc.ver, tc.pinned)
		}
	}
}

func TestDeclaredRef(t *testing.T) {
	cases := []struct {
		next []string
		to   string
		want string
		ok   bool
	}{
		{[]string{"a"}, "a", "a", true},
		{[]string{"a@2"}, "a", "a@2", true},
		{[]string{"a", "a@2"}, "a", "", false},   // 裸名 + 钉版混写：无法唯一选择
		{[]string{"a@1", "a@2"}, "a", "", false}, // 同名多钉版：不偷偷挑
		{[]string{"a@1", "a@2"}, "a@2", "a@2", true},
		{[]string{"a"}, "a@2", "", false}, // 显式钉版不能因有裸名就放行
		{[]string{"a"}, "a@0", "", false}, // 坏版本写法永不匹配
		{[]string{"a@0"}, "a", "", false}, // 声明侧的坏写法也不匹配裸名
		{[]string{"b"}, "a", "", false},
		{nil, "", "", false},
	}
	for _, tc := range cases {
		got, ok := DeclaredRef(tc.next, tc.to)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("DeclaredRef(%v, %q) = (%q,%v)，want (%q,%v)", tc.next, tc.to, got, ok, tc.want, tc.ok)
		}
	}
}
