package main

// 本文件覆盖执行回看 CLI（runs_cmd.go）：四种输出模式 + 退出码。
//
// 全部走 `mainWith(args, stdout, stderr)`：**子命令注册**（isSubcommand / mainWith 的
// 分支）与**输出内容**是同一个被测对象——直接调 runRuns 会漏掉"根本没分派到"这类 bug
// （那种 bug 的症状是静默跑起一个常驻 agent）。
//
// 档案用真的临时目录 + 真 newRunStore(...).Save(...) 铺：CLI 读的就是 client 写的那份
// 字节（input.json / stdout.log / stderr.log / meta.json），中间不做任何替身。
//
// helper 统一带 runsCLI 前缀，避免与同包其它测试文件（runstore_test.go 的
// runStoreTest*、runpersist_test.go）撞名。

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runsCLIFixture 是铺好的档案库：临时目录 + 三条执行的 ref。
//
// 三条记录刻意覆盖三种维度：
//   - trc_alpha / digest@1：同 trace 同能力跑了两次（seq 1/2，attempt 1/2，一次成功一次失败）；
//   - trc_beta / notify@2：另一个 trace、另一个能力、另一个版本。
//
// 于是「按 trace」「按能力」「按版本」「倒序/正序」都能用同一份数据区分开。
type runsCLIFixture struct {
	dir  string
	refs map[string]string // 名字 → ref（<trace>/<capability>@<ver>/<seq>-a<n>）
}

// runsCLIQuietStore 造一个不打印的档案库（envInt 的 Warn 不该污染测试输出）。
func runsCLIQuietStore(dir string) *runStore {
	return newRunStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// runsCLISeed 铺三条执行档案，返回它们的 ref。
func runsCLISeed(t *testing.T) runsCLIFixture {
	t.Helper()

	dir := t.TempDir()
	s := runsCLIQuietStore(dir)
	base := time.Date(2025, 5, 1, 9, 0, 0, 0, time.UTC)
	refs := make(map[string]string, 3)

	save := func(name string, rec runRecord) {
		t.Helper()
		got, err := s.Save(rec)
		if err != nil {
			t.Fatalf("Save(%s): %v", name, err)
		}
		refs[name] = strings.TrimPrefix(got, filepath.Join(dir, runsSubdirName)+string(filepath.Separator))
	}

	// 成功的一次：有 stdout、有 emit。
	save("alpha1", runRecord{
		Input: runInput{
			TraceID: "trc_alpha", TaskID: "tsk_alpha_1", Capability: "digest", Version: 1,
			Attempt: 1, SeqInTrace: 1,
			Params:  map[string]any{"url": "https://example.test/x"},
			WorkDir: "/tmp/work/alpha-1",
		},
		Meta: runMeta{
			Status: "succeeded", ExitCode: 0, Summary: "exit 0", Emits: 3,
			StdoutBytes: int64(len("alpha stdout line\n")),
		},
		Stdout:   "alpha stdout line\n",
		Started:  base,
		Finished: base.Add(700 * time.Millisecond),
	})

	// 失败的一次：只有 stderr、退出码 2、attempt 2。
	save("alpha2", runRecord{
		Input: runInput{
			TraceID: "trc_alpha", TaskID: "tsk_alpha_2", Capability: "digest", Version: 1,
			Attempt: 2, SeqInTrace: 2,
			Params:  map[string]any{"url": "https://example.test/y"},
			WorkDir: "/tmp/work/alpha-2",
		},
		Meta: runMeta{
			Status: "failed", ExitCode: 2, Summary: "exit 2", Error: "exit status 2",
			StderrBytes: int64(len("alpha stderr boom\n")),
		},
		Stderr:   "alpha stderr boom\n",
		Started:  base.Add(time.Minute),
		Finished: base.Add(time.Minute + 250*time.Millisecond),
	})

	// 另一个 trace / 能力 / 版本：最新的一条。
	save("beta1", runRecord{
		Input: runInput{
			TraceID: "trc_beta", TaskID: "tsk_beta_1", Capability: "notify", Version: 2,
			Attempt: 1, SeqInTrace: 1,
			Params:  map[string]any{"to": "phone"},
			WorkDir: "/tmp/work/beta-1",
		},
		Meta: runMeta{
			Status: "succeeded", ExitCode: 0, Summary: "sent", Emits: 1,
			StdoutBytes: int64(len("beta stdout line\n")),
		},
		Stdout:   "beta stdout line\n",
		Started:  base.Add(2 * time.Minute),
		Finished: base.Add(2*time.Minute + 100*time.Millisecond),
	})

	return runsCLIFixture{dir: dir, refs: refs}
}

// runsCLIInvoke 走 mainWith（真实的子命令分派），返回退出码与两路输出。
func runsCLIInvoke(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = mainWith(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// runsCLIWant 断言 got 含全部子串（缺一个就报清楚是哪个）。
func runsCLIWant(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("%s 缺少 %q，实际输出:\n%s", what, want, got)
		}
	}
}

// runsCLINotWant 断言 got 不含任何子串。
func runsCLINotWant(t *testing.T, what, got string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(got, bad) {
			t.Fatalf("%s 不该含 %q，实际输出:\n%s", what, bad, got)
		}
	}
}

// TestRunsCLISeedRefs 钉住 fixture 的 ref 形状：其余用例都靠抄它们。
func TestRunsCLISeedRefs(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	want := map[string]string{
		"alpha1": "trc_alpha/digest@1/1-a1",
		"alpha2": "trc_alpha/digest@1/2-a2",
		"beta1":  "trc_beta/notify@2/1-a1",
	}
	for name, wantRef := range want {
		if f.refs[name] != wantRef {
			t.Errorf("refs[%s] = %q, want %q", name, f.refs[name], wantRef)
		}
	}
}

// ---------- runs：最近 N 条（时间倒序的表格） ----------

// TestRunsCLIRecentList：缺省视图必须是**对齐的表格** + 时间倒序，且六列齐全。
func TestRunsCLIRecentList(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir)
	if code != exitOK {
		t.Fatalf("runs 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}

	// 表头六列（契约 §4）。
	runsCLIWant(t, "表头", stdout, "TIME", "TRACE", "CAPABILITY", "SEQ", "ATT", "STATUS")
	// 三行都在，状态原样来自 meta.json。
	runsCLIWant(t, "表格", stdout, "trc_alpha", "trc_beta", "digest@1", "notify@2", "succeeded", "failed")

	// 倒序：最新的（trc_beta）排在 trc_alpha 之前。
	if i, j := strings.Index(stdout, "trc_beta"), strings.Index(stdout, "trc_alpha"); i > j {
		t.Fatalf("最近视图必须是时间倒序（trc_beta 应在 trc_alpha 之前），实际:\n%s", stdout)
	}

	// 行形状（对齐后 SEQ/ATT/STATUS 各占一列，attempt 2 那条也要在同一行里）。
	row := regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\s+trc_alpha\s+digest@1\s+2\s+2\s+failed$`)
	if !row.MatchString(stdout) {
		t.Fatalf("表格行形状不对（期望 TIME/TRACE/CAPABILITY/SEQ/ATT/STATUS 对齐成一行），实际:\n%s", stdout)
	}
}

// TestRunsCLIListLimit：--limit 截断最近 N 条；0 = 不限。
func TestRunsCLIListLimit(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)

	t.Run("--limit 1 只留最新一条", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--limit", "1")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
		}
		runsCLIWant(t, "表格", stdout, "trc_beta", "notify@2")
		runsCLINotWant(t, "表格", stdout, "trc_alpha", "digest@1")
	})

	t.Run("--limit 0 不限条数", func(t *testing.T) {
		code, stdout, _ := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--limit", "0")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want %d", code, exitOK)
		}
		runsCLIWant(t, "表格", stdout, "trc_alpha", "trc_beta")
	})
}

// ---------- runs --trace：按执行顺序（时间正序） ----------

func TestRunsCLITraceMode(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--trace", "trc_alpha")
	if code != exitOK {
		t.Fatalf("runs --trace 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}

	// 只有这条 trace 的两条执行。
	runsCLIWant(t, "表格", stdout, "trc_alpha", "digest@1")
	runsCLINotWant(t, "表格", stdout, "trc_beta", "notify@2")

	// 正序：seq 1（succeeded）先，seq 2（failed）后。
	if i, j := strings.Index(stdout, "succeeded"), strings.Index(stdout, "failed"); i < 0 || j < 0 || i > j {
		t.Fatalf("trace 视图必须是执行顺序（seq 1 成功在前、seq 2 失败在后），实际:\n%s", stdout)
	}
	runsCLIWant(t, "footer", stdout, "按执行顺序", "trc_alpha")
}

// ---------- runs --capability：该能力的历次执行（时间倒序） ----------

func TestRunsCLICapabilityMode(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)

	t.Run("不带版本 = 该能力所有版本", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--capability", "digest")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
		}
		runsCLIWant(t, "表格", stdout, "digest@1", "trc_alpha")
		runsCLINotWant(t, "表格", stdout, "trc_beta", "notify@2")
		// 倒序：后跑的 failed(seq 2) 在前。
		if i, j := strings.Index(stdout, "failed"), strings.Index(stdout, "succeeded"); i < 0 || j < 0 || i > j {
			t.Fatalf("能力视图必须是时间倒序（failed 在前），实际:\n%s", stdout)
		}
	})

	t.Run("带版本 = 精确到 x@2", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--capability", "notify@2")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
		}
		runsCLIWant(t, "表格", stdout, "notify@2", "trc_beta", "succeeded")
		runsCLINotWant(t, "表格", stdout, "digest@1", "trc_alpha")
	})

	t.Run("版本不存在 = 空结果不是崩溃", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", f.dir, "--capability", "digest@9")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d（空结果也是 1）", code, exitFailure)
		}
		if stdout != "" {
			t.Errorf("空结果不该往 stdout 写东西，实际 %q", stdout)
		}
		runsCLIWant(t, "stderr", stderr, "没有执行记录", "digest@9")
	})
}

// ---------- show：input + 两个流 + meta 摘要 ----------

func TestRunsCLIShow(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)

	t.Run("成功的一次", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, showCommand, f.refs["alpha1"], "--data-dir", f.dir)
		if code != exitOK {
			t.Fatalf("show 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
		}
		// input 段：缩进 JSON，params **原样**（不脱敏）。
		runsCLIWant(t, "show", stdout,
			"== trc_alpha/digest@1/1-a1 ==",
			"--- input ---",
			`"traceId": "trc_alpha"`,
			`"capability": "digest"`,
			"https://example.test/x",
			"--- stdout ---",
			"alpha stdout line",
			"--- stderr ---",
			"--- meta ---",
			"succeeded",
			"退出码 0",
			"emit: 3 次",
			"seq=1",
			"attempt=1",
			"字节: stdout",
		)
		// stdout 段必须在 stderr 段之前（顺序 = 契约里写的读法）。
		if i, j := strings.Index(stdout, "--- stdout ---"), strings.Index(stdout, "--- stderr ---"); i > j {
			t.Fatalf("show 的段顺序不对（stdout 应在 stderr 之前），实际:\n%s", stdout)
		}
	})

	t.Run("失败的一次", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, showCommand, "--data-dir", f.dir, f.refs["alpha2"])
		if code != exitOK {
			t.Fatalf("show 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
		}
		runsCLIWant(t, "show", stdout,
			"alpha stderr boom",
			"failed",
			"退出码 2",
			`"attempt": 2`,
			`"seqInTrace": 2`,
			"exit status 2",
		)
	})
}

// TestRunsCLIShowTruncatedHint：截断必须显眼（否则"少了一段"会被误读成"脚本没输出"）。
func TestRunsCLIShowTruncatedHint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := runsCLIQuietStore(dir)
	_, err := s.Save(runRecord{
		Input: runInput{
			TraceID: "trc_trunc", TaskID: "tsk_trunc", Capability: "dump", Version: 1,
			Attempt: 1, SeqInTrace: 1,
		},
		Meta: runMeta{
			Status: "succeeded", ExitCode: 0, Summary: "exit 0",
			StdoutBytes: runStreamMaxBytes * 100, StdoutTruncated: true,
		},
		Stdout:  "head only…\n\n…（输出已截断：共 819200 字节，只保留前 8192 字节）\n",
		Started: time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	code, stdout, stderr := runsCLIInvoke(t, showCommand, "trc_trunc/dump@1/1-a1", "--data-dir", dir)
	if code != exitOK {
		t.Fatalf("show 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	runsCLIWant(t, "show", stdout, "注意: stdout 已截断", "head only", "字节: stdout")
}

// TestRunsCLIShowHalfWritten：只落了半份（没有 meta.json）的档案**仍然能看**，
// 但状态必须说"未知"，不能把零值当成"退出码 0、成功"。
func TestRunsCLIShowHalfWritten(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runDir := filepath.Join(dir, runsSubdirName, "trc_half", "halfcap@1", "1-a1")
	if err := os.MkdirAll(runDir, runDirMode); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, runStdoutFile), []byte("half stdout\n"), runFileMode); err != nil {
		t.Fatalf("写 stdout.log: %v", err)
	}

	// 列表：STATUS 那一列打占位符（不是空字符串，否则看不出"这条没写完"）。
	code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", dir)
	if code != exitOK {
		t.Fatalf("runs 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	runsCLIWant(t, "表格", stdout, "trc_half", "halfcap@1", runStatusUnknown)

	code, stdout, stderr = runsCLIInvoke(t, showCommand, "trc_half/halfcap@1/1-a1", "--data-dir", dir)
	if code != exitOK {
		t.Fatalf("show 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	runsCLIWant(t, "show", stdout, "half stdout", "状态: 未知", "meta.json")
}

// ---------- 退出码 1 的三种理由 ----------

func TestRunsCLIEmptyAndNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // 真空目录：连 runs/ 都还没有

	t.Run("空目录 runs → 1 且有提示", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", dir)
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d", code, exitFailure)
		}
		if stdout != "" {
			t.Errorf("没有记录时 stdout 应为空，实际 %q", stdout)
		}
		runsCLIWant(t, "stderr", stderr, "没有执行记录")
	})

	t.Run("空目录 runs --trace → 1", func(t *testing.T) {
		code, _, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", dir, "--trace", "trc_nope")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d", code, exitFailure)
		}
		runsCLIWant(t, "stderr", stderr, "没有执行记录", "trc_nope")
	})

	t.Run("空目录 runs --capability → 1", func(t *testing.T) {
		code, _, stderr := runsCLIInvoke(t, runsCommand, "--data-dir", dir, "--capability", "digest")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d", code, exitFailure)
		}
		runsCLIWant(t, "stderr", stderr, "没有执行记录", "digest")
	})

	t.Run("show 不存在的 ref → 1 且说没找到", func(t *testing.T) {
		code, stdout, stderr := runsCLIInvoke(t, showCommand, "--data-dir", dir, "trc_nope/digest@1/1-a1")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d", code, exitFailure)
		}
		if stdout != "" {
			t.Errorf("没找到时 stdout 应为空，实际 %q", stdout)
		}
		runsCLIWant(t, "stderr", stderr, "没找到", "trc_nope/digest@1/1-a1")
	})

	t.Run("show 形状不对的 ref → 1 且说清哪段不对", func(t *testing.T) {
		code, _, stderr := runsCLIInvoke(t, showCommand, "--data-dir", dir, "../../etc/passwd")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want %d", code, exitFailure)
		}
		runsCLIWant(t, "stderr", stderr, "形状不对")
		// 它不是"没找到"：盘上就算真有那么个目录也不该让人这么走（路径安全在 Load 里）。
		runsCLINotWant(t, "stderr", stderr, "没找到")
	})
}

// TestRunsCLIBadArgs：参数非法也是 1，但要说清楚哪里错（而不是"没找到"）。
func TestRunsCLIBadArgs(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"limit 负数", []string{runsCommand, "--data-dir", f.dir, "--limit", "-3"}, "--limit"},
		{"capability 版本不是数字", []string{runsCommand, "--data-dir", f.dir, "--capability", "digest@abc"}, "版本部分"},
		{"runs 收多余位置参数", []string{runsCommand, "--data-dir", f.dir, "extra"}, "多余的参数"},
		{"show 没给 ref", []string{showCommand, "--data-dir", f.dir}, "用法"},
		{"show 给了两个 ref", []string{showCommand, "--data-dir", f.dir, f.refs["alpha1"], f.refs["beta1"]}, "多余的参数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runsCLIInvoke(t, tc.args...)
			if code != exitFailure {
				t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitFailure, stderr)
			}
			runsCLIWant(t, "stderr", stderr, tc.want)
		})
	}
}

// TestRunsCLIHelp：-h 是用户主动要帮助 → 退出码 0（与其它子命令一致）。
func TestRunsCLIHelp(t *testing.T) {
	t.Parallel()

	for _, sub := range []string{runsCommand, showCommand} {
		t.Run(sub+" -h → 0", func(t *testing.T) {
			code, _, _ := runsCLIInvoke(t, sub, "-h")
			if code != exitOK {
				t.Fatalf("%s -h 退出码 = %d, want %d", sub, code, exitOK)
			}
		})
	}
}

// TestRunsCLISubcommandRegistration 覆盖**注册**那一半：子命令出现在全局 flag 之后
// 也必须被认出来（`-data-dir DIR runs`），否则 mainWith 会把它当成节点模式参数、
// 静默跑起一个常驻 agent——那正是 subcommandAt 当初要解决的问题。
func TestRunsCLISubcommandRegistration(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := runsCLIInvoke(t, "-data-dir", f.dir, runsCommand)
	if code != exitOK {
		t.Fatalf("`-data-dir DIR runs` 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	runsCLIWant(t, "表格", stdout, "TIME", "TRACE", "trc_alpha")

	// show 同样要能从全局 flag 前缀里被认出来。
	code, stdout, stderr = runsCLIInvoke(t, "-data-dir", f.dir, showCommand, f.refs["beta1"])
	if code != exitOK {
		t.Fatalf("`-data-dir DIR show REF` 退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	runsCLIWant(t, "show", stdout, "beta stdout line")
}
