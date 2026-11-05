package version

import "testing"

// TestString 钉住契约 §1 说的**两种形态**：
//
//	"1.5.0+3fd5a59"（正式构建，带 commit）
//	"dev"          （没注入 -X 的本地/裸 build）
//
// 这两种形态同时是中枢侧展示逻辑的输入（空串 = 老版本，dev = 开发构建），
// 所以字符串本身不能漂。
func TestString(t *testing.T) {
	cases := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "正式版本带 commit", version: "1.5.0", commit: "3fd5a59", want: "1.5.0+3fd5a59"},
		{name: "dev 无 commit", version: "dev", commit: "", want: "dev"},
		{name: "空 Version 归一为 dev", version: "", commit: "", want: "dev"},
		{name: "只有空格也算空", version: "   ", commit: "", want: "dev"},
		{name: "dev 带 commit 也要报出来", version: "dev", commit: "abc1234", want: "dev+abc1234"},
		{name: "两侧空白不影响结果", version: " 1.5.0 ", commit: " 3fd5a59 ", want: "1.5.0+3fd5a59"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 这两个是包级 var：每个 case 都要还原，否则后面所有测试都跟着漂。
			oldVersion, oldCommit := Version, Commit
			t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })

			Version, Commit = tc.version, tc.commit
			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q（Version=%q Commit=%q）",
					got, tc.want, tc.version, tc.commit)
			}
		})
	}
}

// TestDefaults：包级 var 的零配置形态必须正好是「没注入」的样子。
//
// 这条不只是文档：`go test ./...` 与 `go run` 都不带 -ldflags，
// 如果缺省被改成空串，agent 的 hello 就会报一个空 agent_version，
// 而中枢会把它当成「老版本 agent」。
func TestDefaults(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version 缺省 = %q, want \"dev\"", Version)
	}
	if Commit != "" {
		t.Errorf("Commit 缺省 = %q, want 空串", Commit)
	}
	if BuiltAt != "" {
		t.Errorf("BuiltAt 缺省 = %q, want 空串（运行期不许编一个时间）", BuiltAt)
	}
	if got := String(); got != "dev" {
		t.Errorf("String() = %q, want \"dev\"", got)
	}
}
