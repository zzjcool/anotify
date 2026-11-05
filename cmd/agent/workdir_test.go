package main

// 能力工作目录在节点侧的验收。
//
// 这里的规则只有一条，但它是**产品决策**的落点：工作目录只有一个来源——
// 能力定义里的 `spec.run.workDir`。节点侧的 node.yaml `workDirs`、
// `ANOTIFY_WORK_DIRS`、中枢下发的 `work_dir` 全部作废（删掉了，不是改成兜底）。
//
// 于是要钉住的是三种情况：
//
//	① 没声明（空）  → cwd = 本次任务目录，与这个能力出现之前**逐字节一致**
//	② 声明了        → cwd = 那个目录（解符号链接后），且那里真的能写文件
//	③ 声明了但不可用 → 任务 **failed**，错误写清「哪个能力 / 哪个路径 / 该去改什么」
//
// ③ 必须失败而不是退回任务目录：静默退回会把「机器上没建这个目录」伪装成
// 「脚本莫名报错」——脚本在空目录里找不到工具链配置，报出来的错跟真实原因无关。
//
// 每条关键断言下面都写了「去掉修复会红成什么样」——没有这条注释的断言，
// 没法证明它真的在守东西。

import (
	"bytes"
	"log/slog"
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

// jobWithWorkDir 造一个「产物里声明了 run.workDir」的 job。
//
// 直接改产物结构体（而不是写 YAML 再 Parse）是为了能精确造出「解析层本来会拒绝」
// 的畸形输入（相对路径），那正是执行层第二道防线要挡的东西。
func jobWithWorkDir(t *testing.T, body, workDir string) job {
	t.Helper()
	j := newRunnerJob(t, body)
	j.Artifact.Spec.Run.WorkDir = workDir
	j.Digest = mustDigest(t, j.Artifact) // 产物改了，digest 跟着变（只是日志字段）
	return j
}

// TestResolveChdirDefaultsToTaskDir：能力没声明工作目录 → 用本次任务目录。
//
// 这是**旧行为**（这个能力出现之前所有任务的 cwd），必须逐字节保持：
// 去掉「空 = taskDir」这条会让每个没声明目录的能力都 failed。
func TestResolveChdirDefaultsToTaskDir(t *testing.T) {
	j := job{Artifact: testScriptArtifact("demo.prep", 1, bashScript("true"))}
	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("resolveChdir: %v", err)
	}
	if got != "/data/work/tsk_1" {
		t.Fatalf("空 workDir 应回落到任务目录，得到 %q", got)
	}

	// 历史上「中枢给了 work_dir」也会走到这里；节点已经不看那个字段了（job 里
	// 根本没有它），这条断言守的是「节点侧没有任何别的目录来源」。
	if got, err := resolveChdir(job{}, "/data/work/tsk_2"); err != nil || got != "/data/work/tsk_2" {
		t.Fatalf("没有产物时也应回落到任务目录，得到 (%q, %v)", got, err)
	}
}

// TestResolveChdirUsesDeclaredDir：声明了 → 用它（解链后）。
func TestResolveChdirUsesDeclaredDir(t *testing.T) {
	proj := t.TempDir()
	j := jobWithWorkDir(t, "true", proj)

	got, err := resolveChdir(j, "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("resolveChdir: %v", err)
	}
	// 去掉「非空就用产物目录」会红：got 会是任务目录。
	if got != realDir(t, proj) {
		t.Fatalf("cwd = %q, want %q（能力声明的目录）", got, realDir(t, proj))
	}
}

// TestResolveChdirRejects：不可用的目录一律失败，且**绝不**退回任务目录
// （那会把「机器上没建目录」伪装成「脚本莫名报错」）。
func TestResolveChdirRejects(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope")
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
		want []string // 错误里必须出现的片段
	}{
		// 任务卡钉死的文案：能力 X@Y + 路径 + 「不存在」+ 「这是能力定义里的 run.workDir」
		{"目录不存在", missing, []string{"不存在", missing, "run.workDir"}},
		{"是文件不是目录", file, []string{"不是目录", file}},
		// 产物解析层会拒绝相对路径；这条是执行层的防御（产物也可能来自缓存文件）。
		{"相对路径", "proj", []string{"不是绝对路径", "proj"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChdir(jobWithWorkDir(t, "true", tc.dir), "/data/work/tsk_1")
			if err == nil {
				t.Fatalf("应该失败，却返回 cwd=%q", got)
			}
			// 去掉存在性/目录检查会红：错误不会出现，任务会在一个不存在的地方 exec，
			// 报错指向脚本而不是能力定义。
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误串应包含 %q，实际：%v", want, err)
				}
			}
			// 错误里必须有能力身份（X@Y）：一个节点上跑着几十个能力，不说清是哪一个
			// 等于让人去猜。去掉 capabilityRef 会红在这里。
			if !strings.Contains(err.Error(), "demo.prep@1") {
				t.Fatalf("错误串应含能力身份 demo.prep@1，实际：%v", err)
			}
			if got != "" {
				t.Fatalf("被拒绝时不该返回目录，得到 %q", got)
			}
		})
	}
}

// TestResolveChdirSymlinkResolved：声明的路径是软链 → 用解链后的真路径。
//
// 判断的和使用的必须是同一个路径：不解链会让 ANOTIFY_WORKDIR / 日志里的目录
// 与实际执行目录不是同一个字符串（项目目录常常是软链，例如 /home/me/proj →
// /data/proj）。去掉 EvalSymlinks 会红在这里（返回的是 link 本身）。
func TestResolveChdirSymlinkResolved(t *testing.T) {
	root := t.TempDir()
	realProj := filepath.Join(root, "real-project")
	mkdirs(t, realProj)
	link := filepath.Join(root, "link-project")
	if err := os.Symlink(realProj, link); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}

	got, err := resolveChdir(jobWithWorkDir(t, "true", link), "/data/work/tsk_1")
	if err != nil {
		t.Fatalf("软链指向真目录时不该失败: %v", err)
	}
	if got != realProj {
		t.Fatalf("cwd = %q, want 解链后的 %q", got, realProj)
	}
	if got != realDir(t, link) {
		t.Fatalf("cwd = %q, want %q（解链后两者必须相等）", got, realDir(t, link))
	}
}

// TestRunJobWorkDirEndToEnd：端到端跑一次，验证
//   - 进程 cwd 就是能力声明的目录
//   - 那个目录里**真的能写文件**（不是个只读/假目录）
//   - $ANOTIFY_WORKDIR == cwd（历史不变式，脚本里普遍写着 cd "$ANOTIFY_WORKDIR"）
//   - $ANOTIFY_TASKDIR 仍指向任务目录（产物文件在那儿）
//   - 产物文件**没有**被写进用户的目录
func TestRunJobWorkDirEndToEnd(t *testing.T) {
	proj := t.TempDir()

	j := jobWithWorkDir(t, `
printf 'cwd=%s\n' "$(pwd)"
printf 'workdir=%s\n' "$ANOTIFY_WORKDIR"
printf 'taskdir=%s\n' "$ANOTIFY_TASKDIR"
printf 'artifact=%s\n' "$(ls run.sh)"   # 产物文件在任务目录里，这里该看不到
echo written > "$(pwd)/proof.txt"
`, proj)

	res := runJob(t.Context(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nstdout: %s\nstderr: %s", res.Status, res.Err, res.Stdout, res.Stderr)
	}
	wantDir := realDir(t, proj)
	for _, want := range []string{
		"cwd=" + wantDir,
		"workdir=" + wantDir, // 去掉 buildEnv 里 ANOTIFY_WORKDIR=cwd 会红
		"taskdir=" + workDirOf(j),
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("stdout 里缺少 %q：\n%s", want, res.Stdout)
		}
	}
	// 产物在任务目录，不在用户目录：列 run.sh 必须失败（ls 报错 → artifact= 空）。
	if strings.Contains(res.Stdout, "artifact=run.sh") {
		t.Fatalf("产物文件跑到了用户目录里：\n%s", res.Stdout)
	}

	// 「真的能写」：脚本写下的文件确实落在用户目录里。
	if b, err := os.ReadFile(filepath.Join(proj, "proof.txt")); err != nil {
		t.Fatalf("用户目录里没有写出的文件（cwd 可能不是它）：%v", err)
	} else if strings.TrimSpace(string(b)) != "written" {
		t.Fatalf("proof.txt 内容 = %q", string(b))
	}
}

// TestRunJobTaskDirWhenNoWorkDir：没声明目录时端到端仍是任务目录，且
// ANOTIFY_WORKDIR == cwd == ANOTIFY_TASKDIR（旧行为完整保留）。
func TestRunJobTaskDirWhenNoWorkDir(t *testing.T) {
	j := newRunnerJob(t, `
printf 'cwd=%s\n' "$(pwd)"
printf 'workdir=%s\n' "$ANOTIFY_WORKDIR"
printf 'taskdir=%s\n' "$ANOTIFY_TASKDIR"
printf 'artifact=%s\n' "$(ls run.sh)"
`)
	// 中枢历史上会下发 work_dir；job 里已经没有这个字段了，这里显式确认：
	// 产物的 workDir 留空时，即使节点上「碰巧」还有别的目录配置也不会生效。
	j.Artifact.Spec.Run.WorkDir = ""

	res := runJob(t.Context(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nstdout: %s\nstderr: %s", res.Status, res.Err, res.Stdout, res.Stderr)
	}
	wantDir := workDirOf(j)
	for _, want := range []string{
		"cwd=" + wantDir,
		"workdir=" + wantDir,
		"taskdir=" + wantDir,
		"artifact=run.sh",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("stdout 里缺少 %q：\n%s", want, res.Stdout)
		}
	}
	// 成功后会删掉任务目录，res.WorkDir 仍要指得出它（排障用）。
	if res.WorkDir != wantDir {
		t.Fatalf("res.WorkDir = %q, want %q", res.WorkDir, wantDir)
	}
}

// TestRunJobFailsWhenWorkDirMissing：目录不存在 → 任务 failed，脚本**不执行**，
// 错误串说得清「能力 / 路径 / 不存在」。
//
// 去掉「不可用就失败」而退回任务目录会红：status 会变成 succeeded、
// 输出里会出现 should-not-run。
func TestRunJobFailsWhenWorkDirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created-yet")
	j := jobWithWorkDir(t, "echo should-not-run", missing)

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed（目录不存在不能静默退回任务目录）", res.Status)
	}
	for _, want := range []string{"不存在", missing, "run.workDir", "demo.prep@1"} {
		if !strings.Contains(res.Err, want) {
			t.Fatalf("失败原因应包含 %q，实际：%q", want, res.Err)
		}
	}
	if strings.Contains(res.Stdout, "should-not-run") {
		t.Fatalf("目录不可用时脚本不该被执行，却看到了输出：%s", res.Stdout)
	}
}

// TestRunJobFailsWhenWorkDirIsFile：声明的是文件 → failed。
func TestRunJobFailsWhenWorkDirIsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := jobWithWorkDir(t, "echo should-not-run", file)

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Err, "不是目录") || !strings.Contains(res.Err, file) {
		t.Fatalf("失败原因应说明「%s 不是目录」，实际：%q", file, res.Err)
	}
	if strings.Contains(res.Stdout, "should-not-run") {
		t.Fatalf("目录不可用时脚本不该被执行，却看到了输出：%s", res.Stdout)
	}
}

// TestRunJobFailsWhenWorkDirRelative：相对路径（解析层本会拒绝，执行层兜底）→ failed。
func TestRunJobFailsWhenWorkDirRelative(t *testing.T) {
	j := jobWithWorkDir(t, "echo should-not-run", "relative/proj")

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed（相对路径的含义取决于 agent 的 cwd，不能用）", res.Status)
	}
	if !strings.Contains(res.Err, "不是绝对路径") {
		t.Fatalf("失败原因应说明必须是绝对路径，实际：%q", res.Err)
	}
}

// ---------- run.workDir 的 `~` 前缀 ----------
//
// `~` 只在**节点执行时**展开（中枢/解析期不展开：展开后的路径随机器变，不能进
// digest）。所以这里钉两件事：
//
//	① 展开对（用 agent 用户的家目录，不是 $HOME 字面量、不是 cwd）、展开后照样
//	   解链/stat/IsDir；
//	② 家目录查不到时**任务失败**且脚本一行未跑，不能拼出 `/x` 就去执行。

// TestResolveChdirExpandsHome：`~` / `~/…` 展开成 agent 用户的家目录。
func TestResolveChdirExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir 在 Unix 上就是读它
	mkdirs(t, filepath.Join(home, "proj"), filepath.Join(home, "real-proj"))
	if err := os.Symlink(filepath.Join(home, "real-proj"), filepath.Join(home, "link-proj")); err != nil {
		t.Skipf("symlink 不可用：%v", err)
	}

	cases := []struct {
		name string
		dir  string
		want string
	}{
		// 去掉展开会红：resolveChdir 返回「不是绝对路径」错误。
		{"~/proj", "~/proj", filepath.Join(home, "proj")},
		// `~` 自己 = 家目录（`~/` 同义）。
		{"~ 等于家目录", "~", home},
		{"~/ 等于家目录", "~/", home},
		// 展开后照样解链：判断的和使用的必须是同一个路径。
		{"~/link-proj 解链", "~/link-proj", filepath.Join(home, "real-proj")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChdir(jobWithWorkDir(t, "true", tc.dir), "/data/work/tsk_1")
			if err != nil {
				t.Fatalf("resolveChdir(%q): %v", tc.dir, err)
			}
			if want := realDir(t, tc.want); got != want {
				t.Fatalf("cwd = %q, want %q（~ 应展开成家目录再解链）", got, want)
			}
		})
	}
}

// TestResolveChdirExpandedHomeStillChecked：展开后仍要过全套检查——
// 目录不存在 / 是文件 / 展开后不是绝对路径，全部 failed（不降级）。
func TestResolveChdirExpandedHomeStillChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
		want []string
	}{
		// 展开后目录不存在 → 不存在（不是 "~ 不合法"）。
		{"~/nope 不存在", "~/nope", []string{"不存在", filepath.Join(home, "nope"), "run.workDir"}},
		// 展开后是文件 → 不是目录。
		{"~/a.txt 是文件", "~/a.txt", []string{"不是目录", filepath.Join(home, "a.txt")}},
		// `..` 必须在**展开之前**按字面量挡掉：展开后它就变成「家目录的父目录」。
		{"~/../etc 含 ..", "~/../etc", []string{`".."`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChdir(jobWithWorkDir(t, "true", tc.dir), "/data/work/tsk_1")
			if err == nil {
				t.Fatalf("应该失败，却返回 cwd=%q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误串应包含 %q，实际：%v", want, err)
				}
			}
			if !strings.Contains(err.Error(), "demo.prep@1") {
				t.Fatalf("错误串应含能力身份 demo.prep@1，实际：%v", err)
			}
		})
	}
}

// TestResolveChdirRejectsUnsupportedHomeForms：`~user/x` 与 `$HOME/x` 明确不支持。
//
// `~user` 取决于机器上有没有那个用户，`$VAR` 展开会把声明变成求值表达式
// （见 artifact.ExpandWorkDir 的注释）——两者都必须拒，而且不能悄悄“当绝对路径用”。
func TestResolveChdirRejectsUnsupportedHomeForms(t *testing.T) {
	for _, dir := range []string{"~user/x", "~root", "$HOME/x", "${HOME}/x"} {
		t.Run(dir, func(t *testing.T) {
			got, err := resolveChdir(jobWithWorkDir(t, "true", dir), "/data/work/tsk_1")
			if err == nil {
				t.Fatalf("%q 应该被拒，却返回 cwd=%q", dir, got)
			}
			if !strings.Contains(err.Error(), "不是绝对路径") {
				t.Fatalf("错误串应说明必须是绝对路径（不是 ~/… 的形式），实际：%v", err)
			}
		})
	}
}

// TestRunJobWorkDirHomeEndToEnd：端到端跑一次 `~/proj` 的能力。
//
//   - 进程 cwd 是家目录下的 proj（展开 + 解链后）
//   - $ANOTIFY_WORKDIR == cwd（脚本里普遍 cd "$ANOTIFY_WORKDIR"）
//   - outcome.Cwd / outcome.WorkDir 分别是「真跑的目录」与「产物所在的任务目录」
//   - 日志里同时留下 declared 与 resolved（排障要能看出 ~ 落在哪）
func TestRunJobWorkDirHomeEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "proj")
	mkdirs(t, proj)

	j := jobWithWorkDir(t, `
printf 'cwd=%s\n' "$(pwd)"
printf 'workdir=%s\n' "$ANOTIFY_WORKDIR"
printf 'taskdir=%s\n' "$ANOTIFY_TASKDIR"
echo written > "$(pwd)/proof.txt"
`, "~/proj")
	var logs bytes.Buffer
	j.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	res := runJob(t.Context(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, err = %q\nstdout: %s\nstderr: %s", res.Status, res.Err, res.Stdout, res.Stderr)
	}
	wantDir := realDir(t, proj)
	for _, want := range []string{
		"cwd=" + wantDir,
		"workdir=" + wantDir,
		"taskdir=" + workDirOf(j),
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("stdout 里缺少 %q：\n%s", want, res.Stdout)
		}
	}
	if res.Cwd != wantDir {
		t.Fatalf("res.Cwd = %q, want %q", res.Cwd, wantDir)
	}
	if res.WorkDir != workDirOf(j) {
		t.Fatalf("res.WorkDir = %q, want 任务目录 %q", res.WorkDir, workDirOf(j))
	}
	if b, err := os.ReadFile(filepath.Join(proj, "proof.txt")); err != nil {
		t.Fatalf("家目录下的 proj 里没有写出的文件（cwd 可能不是它）：%v", err)
	} else if strings.TrimSpace(string(b)) != "written" {
		t.Fatalf("proof.txt 内容 = %q", string(b))
	}

	// 启动日志必须同时给出声明值与展开值（requirement: `workdir declared=~/x resolved=...`）。
	wantLine := "workdir declared=~/proj resolved=" + wantDir
	if !strings.Contains(logs.String(), wantLine) {
		t.Fatalf("日志里缺少 %q，实际：\n%s", wantLine, logs.String())
	}
}

// TestRunJobFailsWhenHomeUnavailable：agent 进程查不到家目录 → 任务 failed，
// 脚本**一行都没跑**（不能把空家目录拼成 `/proj` 就去执行）。
//
// 去掉「查不到家目录就失败」会红：resolveChdir 会把 `~/x` 拼成 `/x` 或保持字面量，
// status 会变成 succeeded 并输出 should-not-run。
func TestRunJobFailsWhenHomeUnavailable(t *testing.T) {
	// HOME 为空 = os.UserHomeDir 返回 error（容器 / 系统用户的典型现场）。
	t.Setenv("HOME", "")
	j := jobWithWorkDir(t, "echo should-not-run", "~/proj")

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed（家目录不可知时不能在 /proj 里跑）", res.Status)
	}
	for _, want := range []string{"查不到家目录", "~/proj", "run.workDir", "demo.prep@1"} {
		if !strings.Contains(res.Err, want) {
			t.Fatalf("失败原因应包含 %q，实际：%q", want, res.Err)
		}
	}
	if strings.Contains(res.Stdout, "should-not-run") {
		t.Fatalf("家目录不可用时脚本不该被执行，却看到了输出：%s", res.Stdout)
	}
}

// TestRunJobFailsWhenHomeIsRelative：$HOME 被烧成相对路径（systemd 常见）同样不可用。
func TestRunJobFailsWhenHomeIsRelative(t *testing.T) {
	t.Setenv("HOME", "relative-home")
	j := jobWithWorkDir(t, "echo should-not-run", "~/proj")

	res := runJob(t.Context(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed（相对家目录拼不出可执行的绝对路径）", res.Status)
	}
	if !strings.Contains(res.Err, "查不到家目录") {
		t.Fatalf("失败原因应说明家目录不可用，实际：%q", res.Err)
	}
	if strings.Contains(res.Stdout, "should-not-run") {
		t.Fatalf("家目录不可用时脚本不该被执行，却看到了输出：%s", res.Stdout)
	}
}
