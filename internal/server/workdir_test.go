package server

import (
	"strings"
	"testing"
)

// normalizeWorkDir 的验收（迁移 19）。
//
// 中枢只做**形状**校验：它看不到节点文件系统，也不该看到。真正的权限边界在
// 节点侧白名单（cmd/agent 的 workDirs）。但明显的错必须在这里就报——
// 「点部署」的那一刻失败，比等任务跑到节点上才失败有用得多。
func TestNormalizeWorkDir(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string // 非空 = 必须报错，且错误里要出现这个关键词
	}{
		{"空 = 用默认目录", "", "", ""},
		{"空白也是空", "   ", "", ""},
		{"普通绝对路径", "/home/me/proj", "/home/me/proj", ""},
		{"去掉重复斜杠", "/srv//app/", "/srv/app", ""},
		{"相对路径必须拒绝", "proj/sub", "", "必须是绝对路径"},
		{".. 必须拒绝", "/home/me/../etc", "", "不能包含 .."},
		{"含 NUL 必须拒绝", "/home/\x00bad", "", "NUL"},
		{"超长必须拒绝", "/" + strings.Repeat("a", maxWorkDirLen), "", "过长"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeWorkDir(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("%q 应该被拒绝，却返回 %q", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误串应包含 %q，实际：%v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q 不该报错：%v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeWorkDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
