package main

// 本文件钉住 CLI 的**机器面**（issue #318）：四种 `--json` 形状、`{error:{kind,hint}}`
// 契约、短 ref 的前缀消歧、以及读输出窗口。
//
// 为什么用**真实文件**当 stdout：默认 JSON 的判据是「stdout 不是字符设备」，而
// bytes.Buffer 判不出来（见 cliout.go 的 stdoutIsTerminal，判不出来时选文本）。
// 用临时文件才能同时钉住两条要求：「管道默认 JSON」与「TTY 保持表格」。
//
// 断言口径一律是**外部可见的东西**：stdout 的 JSON 形状（解出来断言字段）、stderr 的
// 单行错误 JSON、退出码。不碰内部函数——这一层的价值就在「agent 实际拿到什么」。

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- 断言与夹具 ----------

// cliJSONStdout 造一个**非终端**的 stdout（真实临时文件），CLI 因此默认走 JSON。
func cliJSONStdout(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("建临时 stdout: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, func() string {
		t.Helper()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("回绕 stdout: %v", err)
		}
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("读 stdout: %v", err)
		}
		return string(data)
	}
}

// cliJSONInvoke 走 mainWith（真分派）+ 非终端 stdout，返回 (退出码, stdout, stderr)。
func cliJSONInvoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	f, read := cliJSONStdout(t)
	var errBuf bytes.Buffer
	code := mainWith(args, f, &errBuf)
	return code, read(), errBuf.String()
}

// cliTextInvoke 走 mainWith + bytes.Buffer（判不出终端 → 文本那一路）。
func cliTextInvoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := mainWith(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// cliDecodeError 解 stderr 的 {"error":{…}}，并顺手钉住两条形状要求：
// 必须是**单行**（逐行读的调用方不会拿到半条），三个键都不能缺。
func cliDecodeError(t *testing.T, stderr string) cliError {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("JSON 错误必须是单行，实际 %d 行：%q", len(lines), stderr)
	}
	var env cliErrorEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("stderr 不是 {\"error\":{…}} 形状：%q（%v）", stderr, err)
	}
	if env.Error.Kind == "" || env.Error.Message == "" || env.Error.Hint == "" {
		t.Fatalf("error 的 kind/message/hint 都不许为空：%q", stderr)
	}
	return env.Error
}

// cliSeedRun 在 dir 下铺一条执行档案，返回 ref（与 runsCLISeed 同一套落盘路径）。
func cliSeedRun(t *testing.T, dir string, rec runRecord) string {
	t.Helper()
	got, err := runsCLIQuietStore(dir).Save(rec)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return strings.TrimPrefix(got, filepath.Join(dir, runsSubdirName)+string(filepath.Separator))
}

// cliRecord 造一条最小可用的执行记录（只关心流与身份字段）。
func cliRecord(trace, capability string, seq, attempt int, stdout, stderr string) runRecord {
	base := time.Date(2025, 5, 1, 9, 0, 0, 0, time.UTC)
	return runRecord{
		Input: runInput{
			TraceID: trace, TaskID: "tsk_" + trace, Capability: capability, Version: 1,
			Attempt: attempt, SeqInTrace: seq,
			Params:  map[string]any{"k": "v"},
			WorkDir: "/tmp/work/" + trace,
		},
		Meta: runMeta{
			Status: "succeeded", ExitCode: 0, Summary: "exit 0",
			StdoutBytes: int64(len(stdout)), StderrBytes: int64(len(stderr)),
		},
		Stdout:   stdout,
		Stderr:   stderr,
		Started:  base.Add(time.Duration(seq) * time.Minute),
		Finished: base.Add(time.Duration(seq)*time.Minute + time.Second),
	}
}

// cliReadJSONMap 把一个 JSON 文件读成 map（用于「磁盘上的形状 == 输出里的形状」）。
func cliReadJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s 不是 JSON 对象: %v", path, err)
	}
	return out
}

// cliAsMap 把结构体转成 map（走同一套 json tag，等于「CLI 实际吐出去的样子」）。
func cliAsMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// ---------- runs --json ----------

// TestRunsJSONShape 钉住 `{"runs":[{ref + runMeta 全字段}]}`。
//
// 关键断言是「**没有第二套字段名**」：每一条的键集合必须正好等于磁盘 meta.json 的
// 键集合 + `ref`，值逐个相等。少了这条，两套形状迟早会分叉（issue §1 的原话）。
func TestRunsJSONShape(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := cliJSONInvoke(t, runsCommand, "--data-dir", f.dir)
	if code != exitOK {
		t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}

	var raw struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout 不是 {\"runs\":[…]}: %q（%v）", stdout, err)
	}
	if len(raw.Runs) != 3 {
		t.Fatalf("runs 条数 = %d, want 3（stdout=%q）", len(raw.Runs), stdout)
	}
	// 倒序：最新的 beta1 在前。
	if got := raw.Runs[0]["ref"]; got != f.refs["beta1"] {
		t.Errorf("第一条 ref = %v, want %q（时间倒序）", got, f.refs["beta1"])
	}
	// 完整 id（不是 8 位前缀）：前缀是**人**的预算优化，机器要能直接拿去查询。
	if got := raw.Runs[0]["traceId"]; got != "trc_beta" {
		t.Errorf("traceId = %v, want 完整 id %q", got, "trc_beta")
	}

	for _, entry := range raw.Runs {
		ref, _ := entry["ref"].(string)
		if ref == "" {
			t.Fatalf("有一条没有 ref：%v", entry)
		}
		disk := cliReadJSONMap(t, filepath.Join(f.dir, runsSubdirName, filepath.FromSlash(ref), runMetaFile))
		if len(entry) != len(disk)+1 {
			t.Fatalf("%s 的键数 = %d，want meta.json 的 %d + ref（多/少字段说明又造了一套形状）：%v",
				ref, len(entry), len(disk), entry)
		}
		for key, want := range disk {
			got, ok := entry[key]
			if !ok {
				t.Fatalf("%s 缺字段 %q（meta.json 里有）：%v", ref, key, entry)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s 的 %q = %v, want %v（与磁盘不一致）", ref, key, got, want)
			}
		}
	}
}

// TestRunsJSONEmpty：空档案库仍然是 `{"runs":[]}` + 退出码 1（空不是崩溃，但也不是成功）。
//
// 这是 issue 的第一条验收：`runs --json | jq -e '.runs | type=="array"'` 在空目录下
// 必须成立——所以字段不能是 null，退出码不能变成 0。
func TestRunsJSONEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // 空目录：一次任务都没跑过
	for _, args := range [][]string{
		{runsCommand, "--data-dir", dir},           // 管道默认 JSON
		{runsCommand, "--data-dir", dir, "--json"}, // 显式
	} {
		code, stdout, stderr := cliJSONInvoke(t, args...)
		if code != exitFailure {
			t.Errorf("%v 退出码 = %d, want %d（空结果也是 1）", args, code, exitFailure)
		}
		if stdout != "{\"runs\":[]}\n" {
			t.Errorf("%v 的 stdout = %q, want %q", args, stdout, "{\"runs\":[]}\n")
		}
		e := cliDecodeError(t, stderr)
		if e.Kind != kindNotFound {
			t.Errorf("%v 的 kind = %q, want %q（空结果不是 usage 也不是崩溃）", args, e.Kind, kindNotFound)
		}
	}
}

// TestRunsOutputModeBothWays：同一个场景，TTY（表格）与管道（JSON）各走一条——
// 这正是「人的体验不牺牲、机器拿得到形状」的写法。
func TestRunsOutputModeBothWays(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)

	// 文本：表头 + 8 位前缀（完整 id 不在表里）。
	code, text, _ := cliTextInvoke(t, runsCommand, "--data-dir", f.dir)
	if code != exitOK {
		t.Fatalf("文本模式退出码 = %d", code)
	}
	if !strings.Contains(text, "TIME") || !strings.Contains(text, "TRACE") {
		t.Fatalf("判不出终端时必须保持表格：%q", text)
	}
	if strings.Contains(text, "trc_alpha/") {
		t.Fatalf("表格里不该有完整 ref：%q", text)
	}

	// 显式 --json 在「判不出终端」的 writer 上也必须切到 JSON。
	code, jsonOut, _ := cliTextInvoke(t, runsCommand, "--data-dir", f.dir, "--json")
	if code != exitOK {
		t.Fatalf("--json 退出码 = %d", code)
	}
	if !strings.HasPrefix(jsonOut, "{\"runs\":[") {
		t.Fatalf("--json 的 stdout 应当是 JSON：%q", jsonOut)
	}
}

// ---------- show --json ----------

// TestShowJSONMatchesDisk：`show --json` 的 meta / input 必须与盘上的 meta.json /
// input.json **逐字段相等**（issue 验收：防两套形状），stdout/stderr 是原文。
func TestShowJSONMatchesDisk(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	ref := f.refs["alpha1"]
	code, stdout, stderr := cliJSONInvoke(t, showCommand, ref, "--data-dir", f.dir)
	if code != exitOK {
		t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, exitOK, stderr)
	}
	var got showJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout 不是 show 的形状：%q（%v）", stdout, err)
	}
	if got.Ref != ref {
		t.Errorf("ref = %q, want %q", got.Ref, ref)
	}

	dir := filepath.Join(f.dir, runsSubdirName, filepath.FromSlash(ref))
	if want := cliReadJSONMap(t, filepath.Join(dir, runMetaFile)); !reflect.DeepEqual(cliAsMap(t, got.Meta), want) {
		t.Errorf("meta 与磁盘 meta.json 不一致：\n输出 = %v\n磁盘 = %v", cliAsMap(t, got.Meta), want)
	}
	if want := cliReadJSONMap(t, filepath.Join(dir, "input.json")); !reflect.DeepEqual(cliAsMap(t, got.Input), want) {
		t.Errorf("input 与磁盘 input.json 不一致：\n输出 = %v\n磁盘 = %v", cliAsMap(t, got.Input), want)
	}
	if got.Stdout != "alpha stdout line\n" {
		t.Errorf("stdout = %q, want 原文（不许改写）", got.Stdout)
	}
	if got.Stderr != "" {
		t.Errorf("stderr = %q, want 空串（这条执行只有 stdout）", got.Stderr)
	}
	// 失败的那条：stderr 原文要在，meta.status 要是 failed。
	code, stdout, _ = cliJSONInvoke(t, showCommand, f.refs["alpha2"], "--data-dir", f.dir)
	if code != exitOK {
		t.Fatalf("show alpha2 退出码 = %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout 不是 show 的形状：%q", stdout)
	}
	if got.Meta.Status != "failed" || got.Meta.ExitCode != 2 {
		t.Errorf("meta = %+v, want status=failed exitCode=2", got.Meta)
	}
	if got.Stderr != "alpha stderr boom\n" {
		t.Errorf("stderr = %q, want 原文", got.Stderr)
	}
}

// TestShowWindows 钉住读输出窗口：--tail N / --no-stdout / --no-stderr。
//
// 动机是上下文预算（issue §3）：单流上限 8 MiB，一次 dump 就能顶穿 agent 的窗口。
func TestShowWindows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ref := cliSeedRun(t, dir, cliRecord("trc_window", "digest", 1, 1,
		"l1\nl2\nl3\nl4\nl5\n", "e1\ne2\ne3\n"))

	fiveLines := "l1\nl2\nl3\nl4\nl5\n"

	t.Run("--tail 2 只留最后两行", func(t *testing.T) {
		code, stdout, stderr := cliJSONInvoke(t, showCommand, ref, "--data-dir", dir, "--tail", "2")
		if code != exitOK {
			t.Fatalf("退出码 = %d（stderr=%q）", code, stderr)
		}
		var got showJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout 不是 JSON：%q", stdout)
		}
		if got.Stdout != "l4\nl5\n" {
			t.Errorf("stdout = %q, want %q", got.Stdout, "l4\nl5\n")
		}
		if got.Stderr != "e2\ne3\n" {
			t.Errorf("stderr = %q, want %q", got.Stderr, "e2\ne3\n")
		}
		// 元信息不受窗口影响：--tail 只削正文。
		if got.Meta.StdoutBytes != int64(len(fiveLines)) {
			t.Errorf("meta.stdoutBytes = %d, want %d（meta 必须保持真实值）",
				got.Meta.StdoutBytes, len(fiveLines))
		}
	})

	t.Run("--no-stdout：stdout 空但元信息完整", func(t *testing.T) {
		code, stdout, _ := cliJSONInvoke(t, showCommand, ref, "--data-dir", dir, "--no-stdout")
		if code != exitOK {
			t.Fatalf("退出码 = %d", code)
		}
		var got showJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout 不是 JSON：%q", stdout)
		}
		if got.Stdout != "" {
			t.Errorf("--no-stdout 下 stdout 段 = %q, want 空", got.Stdout)
		}
		if got.Stderr == "" {
			t.Errorf("--no-stdout 不该连 stderr 一起砍：%q", got.Stderr)
		}
		if got.Meta.Capability != "digest" || got.Input.TraceID != "trc_window" {
			t.Errorf("元信息不完整：meta=%+v input=%+v", got.Meta, got.Input)
		}
	})

	t.Run("--no-stderr", func(t *testing.T) {
		code, stdout, _ := cliJSONInvoke(t, showCommand, ref, "--data-dir", dir, "--no-stderr")
		if code != exitOK {
			t.Fatalf("退出码 = %d", code)
		}
		var got showJSON
		_ = json.Unmarshal([]byte(stdout), &got)
		if got.Stderr != "" {
			t.Errorf("--no-stderr 下 stderr 段 = %q, want 空", got.Stderr)
		}
		if got.Stdout != fiveLines {
			t.Errorf("stdout 段 = %q, want 全文", got.Stdout)
		}
	})

	t.Run("文本模式同样生效并说明截断", func(t *testing.T) {
		code, text, _ := cliTextInvoke(t, showCommand, ref, "--data-dir", dir, "--tail", "2")
		if code != exitOK {
			t.Fatalf("退出码 = %d", code)
		}
		if !strings.Contains(text, "l5") || strings.Contains(text, "l1") {
			t.Fatalf("--tail 2 的正文应当是 l4/l5，实际：\n%s", text)
		}
		if !strings.Contains(text, "--tail 2") {
			t.Fatalf("截过的正文必须说明（否则读的人以为就这么多）：\n%s", text)
		}
	})

	t.Run("负数被拒", func(t *testing.T) {
		code, stdout, stderr := cliJSONInvoke(t, showCommand, ref, "--data-dir", dir, "--tail", "-1")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1", code)
		}
		if e := cliDecodeError(t, stderr); e.Kind != kindUsage {
			t.Errorf("kind = %q, want %q", e.Kind, kindUsage)
		}
		if stdout != "" {
			t.Errorf("参数错时 stdout 不该有任何东西：%q", stdout)
		}
	})
}

// ---------- 短 ref 与前缀消歧 ----------

// TestShowAcceptsTracePrefix：8 位前缀与全 id 的输出必须**逐字节相同**（issue 验收）。
func TestShowAcceptsTracePrefix(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	full := f.refs["alpha1"]
	prefix := shortTraceRef("trc_alpha")
	if prefix == "trc_alpha" {
		t.Fatalf("fixture 的 trace id 太短，测不出前缀（%q）", prefix)
	}
	shortRef := prefix + full[len("trc_alpha"):]

	for _, mode := range []string{"text", "json"} {
		invoke := cliTextInvoke
		if mode == "json" {
			invoke = cliJSONInvoke
		}
		codeFull, want, errFull := invoke(t, showCommand, full, "--data-dir", f.dir)
		codeShort, got, errShort := invoke(t, showCommand, shortRef, "--data-dir", f.dir)
		if codeFull != exitOK || codeShort != exitOK {
			t.Fatalf("%s: 退出码 full=%d short=%d（%q / %q）", mode, codeFull, codeShort, errFull, errShort)
		}
		if got != want {
			t.Errorf("%s: 前缀与全 id 的输出必须逐字节相同\n前缀 → %q\n全 id → %q", mode, got, want)
		}
	}

	// 表格里只出现 8 位前缀（完整 id 只在 --json 里）。
	_, table, _ := cliTextInvoke(t, runsCommand, "--data-dir", f.dir)
	if !strings.Contains(table, prefix) {
		t.Fatalf("表格里没有 8 位前缀 %q：\n%s", prefix, table)
	}
	if strings.Contains(table, "trc_alpha") {
		t.Fatalf("表格里不该有完整 trace id（那是 --json 的活）：\n%s", table)
	}
}

// TestRunsTracePrefix：`--trace` 也接受唯一前缀（同一套规则，不用记两遍）。
func TestRunsTracePrefix(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := cliJSONInvoke(t, runsCommand, "--data-dir", f.dir,
		"--trace", shortTraceRef("trc_alpha"))
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（stderr=%q）", code, stderr)
	}
	var out runsJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout 不是 JSON：%q", stdout)
	}
	if len(out.Runs) != 2 {
		t.Fatalf("trace 视图条数 = %d, want 2", len(out.Runs))
	}
	for _, e := range out.Runs {
		if e.TraceID != "trc_alpha" {
			t.Errorf("traceId = %q, want 解析后的完整 id", e.TraceID)
		}
	}
}

// TestAmbiguousTracePrefix：前缀撞车必须**不猜**——退 1、kind=ambiguous_ref、
// stderr 列候选，且 stdout 上**不能有任何日志正文**（issue 验收的原话）。
func TestAmbiguousTracePrefix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 两条 trace 的前 8 位相同（trc_shar）：这正是「前缀撞车」的形状。
	refA := cliSeedRun(t, dir, cliRecord("trc_shared_a", "digest", 1, 1, "A body\n", ""))
	refB := cliSeedRun(t, dir, cliRecord("trc_shared_b", "digest", 1, 1, "B body\n", ""))
	_ = refA
	_ = refB

	prefix := "trc_shar"
	if len(prefix) != traceRefWidth {
		t.Fatalf("测试前缀长度 = %d, want %d", len(prefix), traceRefWidth)
	}

	t.Run("show", func(t *testing.T) {
		code, stdout, stderr := cliJSONInvoke(t, showCommand,
			prefix+"/digest@1/1-a1", "--data-dir", dir)
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1（歧义也走 1，不新增退出码）", code)
		}
		if stdout != "" {
			t.Fatalf("歧义时不许输出任何正文，实际 %q", stdout)
		}
		e := cliDecodeError(t, stderr)
		if e.Kind != kindAmbiguousRef {
			t.Errorf("kind = %q, want %q", e.Kind, kindAmbiguousRef)
		}
		for _, want := range []string{"trc_shared_a", "trc_shared_b"} {
			if !strings.Contains(e.Message, want) {
				t.Errorf("message 里没有候选 %q：%q", want, e.Message)
			}
		}
	})

	t.Run("runs --trace", func(t *testing.T) {
		code, stdout, stderr := cliJSONInvoke(t, runsCommand, "--data-dir", dir, "--trace", prefix)
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1", code)
		}
		if stdout != "" {
			t.Errorf("歧义时 stdout 不该有形状（那是「查询成功」的位置）：%q", stdout)
		}
		if e := cliDecodeError(t, stderr); e.Kind != kindAmbiguousRef {
			t.Errorf("kind = %q, want %q", e.Kind, kindAmbiguousRef)
		}
	})

	t.Run("太短的前缀是 usage 而不是歧义", func(t *testing.T) {
		code, _, stderr := cliJSONInvoke(t, showCommand, "trc/digest@1/1-a1", "--data-dir", dir)
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1", code)
		}
		if e := cliDecodeError(t, stderr); e.Kind != kindUsage {
			t.Errorf("kind = %q, want %q（少于 %d 个字符就别猜）", e.Kind, kindUsage, minTracePrefixLen)
		}
	})
}

// ---------- emit：退出码 ↔ kind 整张表 ----------

// TestEmitExitKinds：emit 2..7 每一档**同时**断言退出码与 error.kind
// （此前只断言退出码，机器读不出「该修还是该重试」）。
func TestEmitExitKinds(t *testing.T) {
	cases := []struct {
		name string
		resp emitResponse
		want int
		kind string
	}{
		{"2 参数/形状错", emitResponse{Seq: 1, Rejected: emitRejectRate, Error: "x"},
			exitEmitBadRequest, kindBadRequest},
		{"3 出口不在契约里", emitResponse{Seq: 1, Rejected: emitRejectPort, Error: "x"},
			exitEmitPort, kindPortNotAllowed},
		{"4 data 不满足下游", emitResponse{Seq: 1, Rejected: emitRejectSchema, Error: "x"},
			exitEmitSchema, kindSchemaMismatch},
		{"5 任务已非运行态", emitResponse{Seq: 1, Rejected: emitRejectTask, Error: "x"},
			exitEmitTask, kindTaskGone},
		{"6 通信失败/token", emitResponse{Seq: 1, Rejected: emitRejectToken, Error: "x"},
			exitEmitComm, kindCommFailed},
		{"7 ack 兜底", emitResponse{Seq: 1, Error: "落盘失败"},
			exitEmitAckTimeout, kindAckTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := fakeEmitSocket(t, tc.resp)
			t.Setenv(envEmitSock, sock)
			t.Setenv(envTaskToken, "tok")
			t.Setenv(envEmitPorts, emitTestContract)

			code, stdout, stderr := cliJSONInvoke(t, emitCommand,
				"--to", "fixer", "--data", `{}`)
			if code != tc.want {
				t.Fatalf("退出码 = %d, want %d（stderr=%q）", code, tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("失败时 stdout 不该有成功形状：%q", stdout)
			}
			if e := cliDecodeError(t, stderr); e.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", e.Kind, tc.kind)
			}
		})
	}
}

// TestEmitJSONSuccessAndTextMode：成功形状 + 「同一场景 TTY 仍是中文」。
func TestEmitJSONSuccessAndTextMode(t *testing.T) {
	resp := emitResponse{OK: true, Seq: 3, Acked: emitAckedRouted}

	t.Run("管道：{ok,port,routed,note}", func(t *testing.T) {
		sock := fakeEmitSocket(t, resp)
		t.Setenv(envEmitSock, sock)
		t.Setenv(envTaskToken, "tok")
		t.Setenv(envEmitPorts, emitTestContract)

		code, stdout, stderr := cliJSONInvoke(t, emitCommand,
			"--to", "fixer", "--data", `{"issue":"#42"}`, "--note", "第 2 项")
		if code != exitOK {
			t.Fatalf("退出码 = %d（stderr=%q）", code, stderr)
		}
		var got emitJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout 不是 JSON：%q（%v）", stdout, err)
		}
		if !got.OK || got.Port != "fix" || !got.Routed || got.Note != "第 2 项" {
			t.Fatalf("形状不对：%+v（原始 %q）", got, stdout)
		}
	})

	t.Run("TTY：保留原来的中文一行", func(t *testing.T) {
		sock := fakeEmitSocket(t, emitResponse{OK: true, Seq: 1, Acked: emitAckedDurable})
		t.Setenv(envEmitSock, sock)
		t.Setenv(envTaskToken, "tok")
		t.Setenv(envEmitPorts, emitTestContract)

		code, stdout, _ := cliTextInvoke(t, emitCommand, "--to", "fixer", "--data", `{}`)
		if code != exitOK {
			t.Fatalf("退出码 = %d", code)
		}
		if !strings.Contains(stdout, "emit ok: port=fix") || !strings.Contains(stdout, "acked=durable") {
			t.Fatalf("文本模式必须保持原样：%q", stdout)
		}
	})

	t.Run("同一场景 TTY vs 管道：kind 相同、文本不回归", func(t *testing.T) {
		reject := emitResponse{Seq: 1, Rejected: emitRejectSchema, Error: "缺 issue"}

		// 文本那一路：仍是那句中文，不许出现 JSON。
		sock := fakeEmitSocket(t, reject)
		t.Setenv(envEmitSock, sock)
		t.Setenv(envTaskToken, "tok")
		t.Setenv(envEmitPorts, emitTestContract)
		textCode, _, textErr := cliTextInvoke(t, emitCommand, "--to", "fixer", "--data", `{}`)
		if textCode != exitEmitSchema {
			t.Fatalf("文本模式退出码 = %d, want %d", textCode, exitEmitSchema)
		}
		if !strings.HasPrefix(textErr, "anotify-agent emit: ") {
			t.Fatalf("文本模式的错误前缀必须保持原样：%q", textErr)
		}
		if strings.HasPrefix(strings.TrimSpace(textErr), "{") {
			t.Fatalf("TTY 上不该出现 JSON：%q", textErr)
		}

		// 管道那一路：同一个响应，退出码一致、kind 就是 schema_mismatch。
		sock = fakeEmitSocket(t, reject)
		t.Setenv(envEmitSock, sock)
		jsonCode, jsonOut, jsonErr := cliJSONInvoke(t, emitCommand, "--to", "fixer", "--data", `{}`)
		if jsonCode != textCode {
			t.Fatalf("同一场景两种形状的退出码不同：text=%d json=%d", textCode, jsonCode)
		}
		if jsonOut != "" {
			t.Errorf("失败时 stdout 不该有形状：%q", jsonOut)
		}
		if e := cliDecodeError(t, jsonErr); e.Kind != kindSchemaMismatch {
			t.Errorf("kind = %q, want %q", e.Kind, kindSchemaMismatch)
		}
	})
}

// ---------- upgrade --json ----------

// TestUpgradeJSON：形状与「restarted 与实际是否执行重启一致」（issue 验收）。
func TestUpgradeJSON(t *testing.T) {
	// 每个子用例各自起假中枢与文件系统，互不影响。
	runJSON := func(t *testing.T, args ...string) (int, upgradeReport, string, string) {
		t.Helper()
		f, read := cliJSONStdout(t)
		var errBuf bytes.Buffer
		code := upgradeMain(args, f, &errBuf, func(string) string { return "" })
		out := read()
		var rep upgradeReport
		if out != "" {
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("stdout 不是 upgrade 的形状：%q（%v）", out, err)
			}
		}
		return code, rep, out, errBuf.String()
	}

	t.Run("--check 有更新", func(t *testing.T) {
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, oldBinaryBody, 0o755)

		code, rep, out, stderr := runJSON(t, "-f", cfgPath, "--install-to", target, "--check")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want 0（out=%q stderr=%q）", code, out, stderr)
		}
		if rep.Latest != testRemoteVersion {
			t.Errorf("latest = %q, want %q", rep.Latest, testRemoteVersion)
		}
		if rep.Current == "" {
			t.Errorf("current 不许为空（那是「本机跑的是哪一版」）：%q", out)
		}
		if rep.Updated || rep.Restarted {
			t.Errorf("--check 什么都不做：updated=%v restarted=%v", rep.Updated, rep.Restarted)
		}
		if rep.Reason != "available" {
			t.Errorf("reason = %q, want %q", rep.Reason, "available")
		}
	})

	t.Run("--check 已最新：退 1 但不是错误（stderr 空）", func(t *testing.T) {
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, newBinaryBody, 0o600)

		code, rep, out, stderr := runJSON(t, "-f", cfgPath, "--install-to", target, "--check")
		if code != exitUpgradeUpToDate {
			t.Fatalf("退出码 = %d, want %d", code, exitUpgradeUpToDate)
		}
		if stderr != "" {
			t.Errorf("「已是最新」不是错误，stderr 必须空：%q", stderr)
		}
		if rep.Updated || rep.Restarted || rep.Reason != "up_to_date" {
			t.Errorf("形状不对：%+v（原始 %q）", rep, out)
		}
	})

	t.Run("--no-restart：updated=true 但 restarted=false", func(t *testing.T) {
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion, "")
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, oldBinaryBody, 0o755)

		code, rep, out, stderr := runJSON(t, "-f", cfgPath, "--install-to", target, "--no-restart")
		if code != exitOK {
			t.Fatalf("退出码 = %d, want 0（out=%q stderr=%q）", code, out, stderr)
		}
		if !rep.Updated {
			t.Errorf("文件已经换了，updated 必须是 true：%+v", rep)
		}
		if rep.Restarted {
			t.Errorf("--no-restart 下 restarted 必须是 false（否则脚本会以为服务已跑新版本）：%+v", rep)
		}
		if rep.Reason != "updated_no_restart" {
			t.Errorf("reason = %q, want %q", rep.Reason, "updated_no_restart")
		}
		// 二进制真的换了（形状不能在撒谎）。
		assertTarget(t, target, newBinaryBody, 0o755, out, stderr)
	})

	t.Run("失败仍走 {error:{kind}}，stdout 无形状", func(t *testing.T) {
		// 校验不符 → 4 / bad_sha。
		srv := binaryServer(t, []byte(newBinaryBody), testRemoteVersion,
			sha256Hex([]byte("another binary")))
		cfgPath := writeUpgradeNodeYAML(t, srv.URL, testNodeToken)
		dir := t.TempDir()
		target := writeTarget(t, dir, oldBinaryBody, 0o755)

		code, rep, out, stderr := runJSON(t, "-f", cfgPath, "--install-to", target)
		if code != exitUpgradeBadSHA {
			t.Fatalf("退出码 = %d, want %d", code, exitUpgradeBadSHA)
		}
		if out != "" {
			t.Errorf("失败时 stdout 不该有成功形状：%q", out)
		}
		if rep.Updated {
			t.Errorf("失败不该报告 updated：%+v", rep)
		}
		if e := cliDecodeError(t, stderr); e.Kind != kindBadSHA {
			t.Errorf("kind = %q, want %q", e.Kind, kindBadSHA)
		}
	})
}

// ---------- 配置类错误的 kind ----------

// TestShowMissingDataDirKind：数据目录不可读/不存在时的 kind 是配置级，不是 not_found
// ——「你指错目录」与「这个 trace 没跑过」要能分开。
func TestShowRunNotFoundKind(t *testing.T) {
	t.Parallel()

	f := runsCLISeed(t)
	code, stdout, stderr := cliJSONInvoke(t, showCommand,
		"trc_missing/digest@1/1-a1", "--data-dir", f.dir)
	if code != exitFailure {
		t.Fatalf("退出码 = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("没找到时 stdout 必须为空：%q", stdout)
	}
	if e := cliDecodeError(t, stderr); e.Kind != kindNotFound {
		t.Errorf("kind = %q, want %q", e.Kind, kindNotFound)
	}

	// ref 形状不对是 usage（用户抄错了），不是 not_found（盘上没有）。
	code, _, stderr = cliJSONInvoke(t, showCommand, "bogus-ref", "--data-dir", f.dir)
	if code != exitFailure {
		t.Fatalf("退出码 = %d, want 1", code)
	}
	if e := cliDecodeError(t, stderr); e.Kind != kindUsage {
		t.Errorf("形状错的 kind = %q, want %q", e.Kind, kindUsage)
	}
}

// TestRunArtifactKindReusesArtifactCode：`run` 的产物不合法时 kind 直接复用
// internal/artifact 的短码词表（invalid_yaml / unknown_field …），agent 不必解析中文。
func TestRunArtifactKindReusesArtifactCode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "capability.yaml")
	if err := os.WriteFile(bad, []byte("not: [a, valid artifact\n"), 0o600); err != nil {
		t.Fatalf("写坏产物: %v", err)
	}
	code, stdout, stderr := cliJSONInvoke(t, runCommand, bad)
	if code != exitFailure {
		t.Fatalf("退出码 = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("run 失败时 stdout 不该有东西：%q", stdout)
	}
	e := cliDecodeError(t, stderr)
	if e.Kind != "invalid_yaml" {
		t.Errorf("kind = %q, want %q（复用 artifact.Code 的词表）", e.Kind, "invalid_yaml")
	}
}

// ---------- -h 里的退出码表 ----------

// TestSubcommandHelpShowsExitCodes：每个子命令 -h 的尾部都要有退出码表与 kind。
//
// 这些码是契约（emit 2..7 / upgrade 0..5），此前只在源码注释与协议文档里；
// 拿到一台陌生机器上的二进制，`-h` 是唯一能看见它们的地方（issue §4）。
func TestSubcommandHelpShowsExitCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sub  string
		want []string
	}{
		{emitCommand, []string{"退出码", "kind: bad_request", "kind: port_not_allowed",
			"kind: schema_mismatch", "kind: task_gone", "kind: comm_failed", "kind: ack_timeout"}},
		{upgradeCommand, []string{"退出码", "kind: need_root", "kind: bad_sha", "kind: unavailable"}},
		{runsCommand, []string{"退出码", "kind: not_found", "ambiguous_ref"}},
		{showCommand, []string{"退出码", "--tail", "--no-stdout", "--no-stderr", "ambiguous_ref"}},
		{runCommand, []string{"退出码", "原样透传"}},
	}
	for _, tc := range cases {
		t.Run(tc.sub, func(t *testing.T) {
			code, _, stderr := cliTextInvoke(t, tc.sub, "-h")
			if code != exitOK {
				t.Fatalf("%s -h 退出码 = %d, want 0", tc.sub, code)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("%s -h 里没有 %q：\n%s", tc.sub, want, stderr)
				}
			}
		})
	}
}
