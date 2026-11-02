package main

// 能力工作目录（迁移 19）在节点侧的验收。
//
// 这是**安全边界**的落点，用例的重点不在"能跑"，而在"越界必须失败且原因可读"：
// work_dir 来自中枢，节点必须按本机白名单（node.yaml 的 workDirs / ANOTIFY_WORK_DIRS）
// 授权。放行错了 = 谁改了中枢，谁就能让本机以 agent 身份在任意目录读写。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveChdirDefaultsToTaskDir：控制台没指定 → 行为与加这一列之前**完全一致**。
func TestResolveChdirDefaultsToTaskDir(t *testing.T) {
	got, err := resolveChdir(job{}, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("resolveChdir: %v", err)
	}
	if got != "/data/work/tsk_1" {
		t.Fatalf("空 work_dir 应回落到任务目录，得到 %q", got)
	}
}

// TestResolveChdirAllowed：白名单内的目录被采用（含它下面的子目录）。
func TestResolveChdirAllowed(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	sub := filepath.Join(proj, "sub")
	for _, d := range []string{proj, sub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{proj, sub} {
		got, err := resolveChdir(job{WorkDir: want, AllowedWorkDirs: []string{root}}, "/data/work/tsk_1")
		if err != nil {
			t.Fatalf("resolveChdir(%s): %v", want, err)
		}
		if got != want {
			t.Fatalf("cwd = %q, want %q", got, want)
		}
	}
}

// TestResolveChdirRejects：越界 / 相对 / 不存在 / 不是目录 / 空白名单 一律失败，
// 且**绝不**退回任务目录（那会把"没授权"伪装成"跑了但报错很奇怪"）。
func TestResolveChdirRejects(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(allowed, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		j    job
		want string // 错误里必须出现的关键词
	}{
		{"白名单外", job{WorkDir: outside, AllowedWorkDirs: []string{allowed}}, "不在本机的 workDirs 白名单内"},
		{"白名单为空", job{WorkDir: allowed}, "未配置任何目录"},
		{"相对路径", job{WorkDir: "proj", AllowedWorkDirs: []string{root}}, "必须是绝对路径"},
		{"不存在", job{WorkDir: filepath.Join(allowed, "nope"), AllowedWorkDirs: []string{allowed}}, "不可用"},
		{"是文件不是目录", job{WorkDir: file, AllowedWorkDirs: []string{allowed}}, "不是目录"},
		// 前缀匹配必须按路径分段：/a/bc 不能被 /a/b 放行
		{"前缀相似但不是子目录", job{WorkDir: outside + "x", AllowedWorkDirs: []string{outside}}, "不在本机的 workDirs 白名单内"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChdir(tc.j, "/data/work/tsk_1")
			if err == nil {
				t.Fatalf("应该拒绝，却返回了 cwd=%q", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误串应包含 %q，实际：%v", tc.want, err)
			}
			if got != "" {
				t.Fatalf("被拒绝时不该返回目录，得到 %q", got)
			}
		})
	}
}

// TestResolveChdirSymlinkEscape：白名单内放一个指向外部的软链 → 必须被拒。
//
// 这是最容易被忽略的绕过方式：`ln -s /etc /allowed/etc` 之后，
// 只做字符串前缀匹配的实现会把 /allowed/etc 当成"在 /allowed 底下"。
func TestResolveChdirSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}

	got, err := resolveChdir(job{WorkDir: link, AllowedWorkDirs: []string{allowed}}, "/data/work/tsk_1")
	if err == nil {
		t.Fatalf("软链逃逸必须被拒，却返回 cwd=%q", got)
	}
	if !strings.Contains(err.Error(), "不在本机的 workDirs 白名单内") {
		t.Fatalf("错误串应说明白名单，实际：%v", err)
	}
}

// TestRunJobUsesConfiguredWorkDir：端到端跑一次，验证
//   - 进程 cwd 就是控制台指定的目录
//   - ANOTIFY_WORKDIR == cwd（历史不变式）
//   - ANOTIFY_TASKDIR 仍指向任务目录（产物文件在那儿）
//   - 产物文件**没有**被写进用户的项目目录
func TestRunJobUsesConfiguredWorkDir(t *testing.T) {
	proj := t.TempDir()

	j := newRunnerJob(t, `
printf 'cwd=%s\n' "$(pwd)"
printf 'workdir=%s\n' "$ANOTIFY_WORKDIR"
printf 'taskdir=%s\n' "$ANOTIFY_TASKDIR"
`)
	j.WorkDir = proj
	j.AllowedWorkDirs = []string{filepath.Dir(proj)}

	res := runJob(t.Context(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nout: %s", res.Status, res.Err, res.Output)
	}
	out := res.Output
	for _, want := range []string{
		"cwd=" + proj,
		"workdir=" + proj,
		"taskdir=" + workDirOf(j),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出里缺少 %q：\n%s", want, out)
		}
	}

	// 项目目录必须干净：产物文件只落在任务目录里。
	entries, err := os.ReadDir(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("用户目录被产物污染了：%v（产物应只落在 ANOTIFY_TASKDIR）", names)
	}
}

// TestRunJobFailsWhenWorkDirNotAllowed：越界的 work_dir 必须让任务 failed，
// 而且错误原因要能看懂（运维据此知道是"没授权"，不是"脚本挂了"）。
func TestRunJobFailsWhenWorkDirNotAllowed(t *testing.T) {
	proj := t.TempDir()
	j := newRunnerJob(t, `echo should-not-run`)
	j.WorkDir = proj
	j.AllowedWorkDirs = []string{filepath.Join(os.TempDir(), "somewhere-else")}

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Err, "workDirs 白名单") {
		t.Fatalf("失败原因应说明白名单，实际：%q", res.Err)
	}
	if strings.Contains(res.Output, "should-not-run") {
		t.Fatalf("越界时脚本不该被执行，却看到了输出：%s", res.Output)
	}
}
