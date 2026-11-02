package main

// 能力工作目录（迁移 19 → 能力级）在节点侧的验收。
//
// 这是**安全边界**的落点，用例的重点不在"能跑"，而在"越界必须失败且原因可读"：
// work_dir 来自中枢，节点必须按本机授权（node.yaml 的 workDirs / ANOTIFY_WORK_DIRS）
// 放行。放行错了 = 谁改了中枢，谁就能让本机以 agent 身份在任意目录读写。
//
// 解析顺序（契约 §2.3.1）：
//
//	① workDirs[<能力名>] 存在 → 用它（压过中枢的 work_dir）
//	② 否则兜底白名单（"*" / 旧列表）非空 → 用中枢的 work_dir，必须落在某个前缀内
//	③ 否则 → 帧里没 work_dir 就用任务目录；有就失败
//
// 每条关键断言下面都写明了「去掉修复会红成什么样」——没有这条注释的断言，
// 没法证明它真的在守东西。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realDir 返回 dir 解符号链接后的真实路径。
//
// resolveChdir 返回的是**解链后**的路径（判断的和使用的必须是同一个），断言必须
// 按同一口径比：macOS 的 /var → /private/var 会让字符串直接比较红掉。
func realDir(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return real
}

// mkdirs 建目录，失败即 Fatal（测试里的 mkdir 失败只可能是环境有问题）。
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// TestResolveChdirDefaultsToTaskDir：控制台没指定 → 用本次任务目录（旧行为不变）。
func TestResolveChdirDefaultsToTaskDir(t *testing.T) {
	got, err := resolveChdir(job{}, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("resolveChdir: %v", err)
	}
	if got != "/data/work/tsk_1" {
		t.Fatalf("空 work_dir 应回落到任务目录，得到 %q", got)
	}
}

// TestResolveChdirAllowed：兜底白名单内的目录被采用（含它下面的子目录）。
func TestResolveChdirAllowed(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	sub := filepath.Join(proj, "sub")
	mkdirs(t, proj, sub)

	allow := workDirsFrom(nil, []string{root})
	for _, want := range []string{proj, sub} {
		got, err := resolveChdir(job{WorkDir: want, WorkDirs: allow}, "/data/work/tsk_1")
		if err != nil {
			t.Fatalf("resolveChdir(%s): %v", want, err)
		}
		if got != realDir(t, want) {
			t.Fatalf("cwd = %q, want %q", got, realDir(t, want))
		}
	}
}

// TestResolveChdirFallbackOnlyKeepsOldBehavior：只有兜底白名单（"*" 或旧列表）时，
// 行为必须与能力级出现之前**逐条一致**。旧部署不会因为新特性改变语义。
func TestResolveChdirFallbackOnlyKeepsOldBehavior(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	mkdirs(t, proj)
	allow := workDirsFrom(nil, []string{root})
	taskDir := "/data/work/tsk_1"

	// ① 帧里有 work_dir，且在前缀内 → 用它。
	//    去掉 dirAllowed 的调用（改成无条件采用）时这条仍绿，但下面 ③ 会红。
	got, err := resolveChdir(job{Capability: "script.echo", WorkDir: proj, WorkDirs: allow}, taskDir)
	if err != nil {
		t.Fatalf("白名单内应放行: %v", err)
	}
	if got != realDir(t, proj) {
		t.Fatalf("cwd = %q, want %q", got, realDir(t, proj))
	}

	// ② 帧里没有 work_dir → 任务目录（与加 work_dir 之前完全一致）。
	got, err = resolveChdir(job{Capability: "script.echo", WorkDirs: allow}, taskDir)
	if err != nil {
		t.Fatalf("空 work_dir + 兜底白名单: %v", err)
	}
	if got != taskDir {
		t.Fatalf("cwd = %q, want %q（没给 work_dir 时不该被兜底白名单影响）", got, taskDir)
	}

	// ③ 帧里 work_dir 越界 → 失败，且**不退回**任务目录。
	//    去掉白名单校验会红在这条：它会返回一个 cwd 而不是错误。
	outside := t.TempDir()
	if got, err := resolveChdir(job{Capability: "script.echo", WorkDir: outside, WorkDirs: allow}, taskDir); err == nil {
		t.Fatalf("越界应失败，却返回 cwd=%q", got)
	} else if !strings.Contains(err.Error(), "不在本机的 workDirs 白名单内") {
		t.Fatalf("错误串应说明白名单，实际：%v", err)
	}

	// ④ 只有 "*" 的配置**不许**被当成「能力级目录」：
	//    For 必须 false，否则每个能力都会被钉死在兜底目录上（而不是用中枢给的子目录）。
	if dir, ok := allow.For("script.echo"); ok {
		t.Fatalf(`只配了 "*" 时 For(%q) 必须 false，却返回 %q`, "script.echo", dir)
	}
}

// TestResolveChdirRejects：越界 / 相对 / 不存在 / 不是目录 / 空白名单 一律失败，
// 且**绝不**退回任务目录（那会把"没授权"伪装成"跑了但报错很奇怪"）。
func TestResolveChdirRejects(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	mkdirs(t, allowed, outside)
	file := filepath.Join(allowed, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := workDirsFrom(nil, []string{allowed})

	cases := []struct {
		name string
		j    job
		want string // 错误里必须出现的关键词
	}{
		{"白名单外", job{WorkDir: outside, WorkDirs: allow}, "不在本机的 workDirs 白名单内"},
		{"白名单为空", job{WorkDir: allowed}, "未配置任何目录"},
		{"相对路径", job{WorkDir: "proj", WorkDirs: allow}, "必须是绝对路径"},
		{"不存在", job{WorkDir: filepath.Join(allowed, "nope"), WorkDirs: allow}, "不可用"},
		{"是文件不是目录", job{WorkDir: file, WorkDirs: allow}, "不是目录"},
		// 前缀匹配必须按路径分段：/a/bc 不能被 /a/b 放行
		{"前缀相似但不是子目录", job{WorkDir: outside + "x", WorkDirs: workDirsFrom(nil, []string{outside})}, "不在本机的 workDirs 白名单内"},
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
	mkdirs(t, allowed, outside)
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}

	got, err := resolveChdir(job{WorkDir: link, WorkDirs: workDirsFrom(nil, []string{allowed})}, "/data/work/tsk_1")
	if err == nil {
		t.Fatalf("软链逃逸必须被拒，却返回 cwd=%q", got)
	}
	if !strings.Contains(err.Error(), "不在本机的 workDirs 白名单内") {
		t.Fatalf("错误串应说明白名单，实际：%v", err)
	}
}

// TestResolveChdirCapabilityOverridesConsoleWorkDir：本机为这个能力配了目录 →
// 中枢的 work_dir 被**忽略**（哪怕它在白名单外、甚至根本不存在）。
//
// 这就是「机器上的 yaml 才是事实」：中枢表达意图，授权在节点。
func TestResolveChdirCapabilityOverridesConsoleWorkDir(t *testing.T) {
	host := t.TempDir()
	// 中枢想去的地方：没建，也不在兜底白名单里。
	consoleWant := filepath.Join(t.TempDir(), "console-wants-here")
	// 注意兜底白名单里**没有** host：能力级条目是独立授权，不该再被 "*" 的前缀卡一遍。
	j := job{
		Capability: "script.echo",
		WorkDir:    consoleWant,
		WorkDirs:   workDirsFrom(map[string][]string{"script.echo": {host}}, []string{"/srv/projects"}),
	}

	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("能力级目录应直接用，却报错：%v", err)
	}
	// 去掉分支①（能力级优先）会红：中枢 work_dir 不在白名单 → 返回错误而不是 host。
	if got != realDir(t, host) {
		t.Fatalf("cwd = %q, want %q（能力级目录必须压过中枢 work_dir）", got, realDir(t, host))
	}
}

// TestResolveChdirOtherCapabilityDoesNotApply：别的能力配了目录，不该影响这个能力。
//
// 查表必须是**精确匹配 + 原样 key**：脚本生成的 key 就是产物名（[a-z0-9._-]），
// 任何归一化（大小写/下划线）都会让「配了却不生效」变成查不出来的谜。
func TestResolveChdirOtherCapabilityDoesNotApply(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	mkdirs(t, proj)
	j := job{
		Capability: "build.repo",
		WorkDir:    proj,
		WorkDirs: workDirsFrom(map[string][]string{
			"script.echo": {t.TempDir()}, // 不是本能力
		}, []string{root}),
	}

	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("应走兜底白名单: %v", err)
	}
	// 去掉「按能力名精确查表」会红：若把 script.echo 的目录当成本次目录，
	// got 会是那个临时目录而不是中枢给的 proj。
	if got != realDir(t, proj) {
		t.Fatalf("cwd = %q, want %q（只应按本能力名查表）", got, realDir(t, proj))
	}
	// 反向自证：改成 script.echo 就该命中能力级条目。
	j.Capability = "script.echo"
	j.Artifact = nil
	if _, ok := j.WorkDirs.For("script.echo"); !ok {
		t.Fatal(`For("script.echo") 应为 true（yaml 里明明配了）`)
	}
}

// TestResolveChdirCapabilityNameFromArtifact：能力名以**产物**为准，与
// ANOTIFY_CAPABILITY 同源（assign 帧里的名字只是旁证）。
func TestResolveChdirCapabilityNameFromArtifact(t *testing.T) {
	host := t.TempDir()
	j := job{
		Capability: "console.alias", // 与产物不一致：产物名才是真的
		Artifact:   testScriptArtifact("demo.prep", 1, bashScript("true")),
		WorkDirs:   workDirsFrom(map[string][]string{"demo.prep": {host}}, nil),
	}

	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("resolveChdir: %v", err)
	}
	// 去掉 capabilityOf（直接拿 j.Capability 查表）会红：查不到 demo.prep → 回落到
	// 任务目录（帧里没 work_dir）→ got 会是 /data/work/tsk_1。
	if got != realDir(t, host) {
		t.Fatalf("cwd = %q, want %q（必须用产物里的能力名查表）", got, realDir(t, host))
	}
}

// TestResolveChdirCapabilityDirInvalid：能力级目录**本身**坏掉时必须 failed，
// 而且错误要点名 node.yaml 里的哪一条；绝不退回中枢给的目录（那会把配置错误藏起来）。
func TestResolveChdirCapabilityDirInvalid(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 中枢给的目录是**合格**的（在兜底白名单内）：它仍然不能成为退路。
	elsewhere := filepath.Join(root, "elsewhere")
	mkdirs(t, elsewhere)

	cases := []struct {
		name string
		dir  string
		want []string // 错误里必须出现的片段
	}{
		{"不存在", filepath.Join(root, "nope"), []string{`workDirs["demo.prep"]`, "不可用"}},
		{"是文件不是目录", file, []string{`workDirs["demo.prep"]`, "不是目录"}},
		{"相对路径", "proj", []string{`workDirs["demo.prep"]`, "必须是绝对路径"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := job{
				Capability: "demo.prep",
				WorkDir:    elsewhere,
				WorkDirs:   workDirsFrom(map[string][]string{"demo.prep": {tc.dir}}, []string{root}),
			}
			got, err := resolveChdir(j, "/data/work/tsk_1")
			if err == nil {
				t.Fatalf("应该失败，却返回 cwd=%q", got)
			}
			// 去掉 checkWorkDir 的存在性/目录检查会红在前两条：错误不会出现，
			// 任务会在一个不存在的地方 exec，报错指向脚本而不是 node.yaml。
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误串应包含 %q，实际：%v", want, err)
				}
			}
			if got != "" {
				t.Fatalf("失败时不该返回目录，得到 %q", got)
			}
		})
	}
}

// TestResolveChdirSymlinkCapabilityDir：能力级配置写成软链 → 解链后执行
// （判断的和使用的必须是同一个路径；项目目录常常是软链）。
func TestResolveChdirSymlinkCapabilityDir(t *testing.T) {
	root := t.TempDir()
	realProj := filepath.Join(root, "real-project")
	mkdirs(t, realProj)
	link := filepath.Join(root, "link-project")
	if err := os.Symlink(realProj, link); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}

	j := job{
		Capability: "script.echo",
		WorkDirs:   workDirsFrom(map[string][]string{"script.echo": {link}}, nil),
	}
	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("软链指向真目录时不该失败: %v", err)
	}
	// 返回未解链的 link 会红：调用方会把 cwd 报给中枢、也写进 ANOTIFY_WORKDIR，
	// 那和实际执行目录不是一个字符串。
	if got != realProj {
		t.Fatalf("cwd = %q, want 解链后的 %q", got, realProj)
	}
}

// TestRunJobUsesConfiguredWorkDir：端到端跑一次（兜底白名单路径），验证
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
	j.WorkDirs = workDirsFrom(nil, []string{filepath.Dir(proj)})

	res := runJob(t.Context(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nout: %s", res.Status, res.Err, res.Output)
	}
	out := res.Output
	wantDir := realDir(t, proj)
	for _, want := range []string{
		"cwd=" + wantDir,
		"workdir=" + wantDir,
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

// TestRunJobUsesCapabilityWorkDir：端到端 —— cwd 是 node.yaml 给这个能力的目录，
// 中枢帧里的 work_dir（一个不存在的地方）完全不生效。
func TestRunJobUsesCapabilityWorkDir(t *testing.T) {
	proj := t.TempDir()
	consoleWant := filepath.Join(t.TempDir(), "console-wants-here")

	j := newRunnerJob(t, `
printf 'cwd=%s\n' "$(pwd)"
printf 'workdir=%s\n' "$ANOTIFY_WORKDIR"
printf 'taskdir=%s\n' "$ANOTIFY_TASKDIR"
`)
	j.WorkDir = consoleWant
	j.WorkDirs = workDirsFrom(map[string][]string{"demo.prep": {proj}}, nil)

	res := runJob(t.Context(), j)
	// 去掉能力级分支会红满：中枢的 work_dir 不在白名单（白名单为空）→ failed。
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nout: %s", res.Status, res.Err, res.Output)
	}
	wantDir := realDir(t, proj)
	for _, want := range []string{
		"cwd=" + wantDir,
		"workdir=" + wantDir, // ANOTIFY_WORKDIR == cwd 的历史不变式继续成立
		"taskdir=" + workDirOf(j),
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("输出里缺少 %q：\n%s", want, res.Output)
		}
	}

	// 用户目录仍然干净：产物文件只在任务目录里展开。
	entries, err := os.ReadDir(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("用户目录被产物污染了：%v", names)
	}
}

// TestRunJobFailsWhenWorkDirNotAllowed：越界的 work_dir 必须让任务 failed，
// 而且错误原因要能看懂（运维据此知道是"没授权"，不是"脚本挂了"）。
func TestRunJobFailsWhenWorkDirNotAllowed(t *testing.T) {
	proj := t.TempDir()
	j := newRunnerJob(t, `echo should-not-run`)
	j.WorkDir = proj
	j.WorkDirs = workDirsFrom(nil, []string{filepath.Join(os.TempDir(), "somewhere-else")})

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Err, "workDirs 白名单") {
		t.Fatalf("失败原因应说明白名单，实际：%q", res.Err)
	}
	// 错误里要给出**可照抄的下一步**：按能力配置的写法（那是推荐做法）。
	// 去掉这段提示会红：运维只知道"不在白名单"，不知道该改哪一行。
	if !strings.Contains(res.Err, `workDirs: {demo.prep: <目录>}`) {
		t.Fatalf("失败原因应给出按能力配置的示例，实际：%q", res.Err)
	}
	if strings.Contains(res.Output, "should-not-run") {
		t.Fatalf("越界时脚本不该被执行，却看到了输出：%s", res.Output)
	}
}
