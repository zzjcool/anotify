package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

var builtOld, builtNew string

// shortTmpDir 给 unix socket 这类受 `sun_path` 上限（Linux 108 字节）限制的路径用**短**临时目录：
// 不用 t.TempDir() 是因为它把「测试名 + 序号」拼进路径，TMPDIR 一长就顶到上限、bind 报
// `invalid argument`（那是夹具的环境假设，不是产品缺陷）。其它文件仍用 t.TempDir()；
// 前缀故意带 anotify-：万一没清掉，check-tmp-leaks 的零残留断言会点名它。
func shortTmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "anotify-s-")
	if err != nil {
		t.Fatalf("mkdtemp(socket): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "anode-n3-binaries-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	builtOld = filepath.Join(dir, "old/anode")
	builtNew = filepath.Join(dir, "new/anode")
	for i, path := range []string{builtOld, builtNew} {
		v, c := "old-v1", "old-commit"
		if i == 1 {
			v, c = "actual-v2", "new-commit"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-X main.version="+v+" -X main.commit="+c, "-o", path, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=direct", "GOSUMDB=sum.golang.org", "GOTOOLCHAIN=auto")
		b, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "real binary build: %v\n%s", err, b)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
func binaryRun(t *testing.T, bin string, env []string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("command timeout: %v stderr=%s", args, errOut.String())
	}
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return code, out.String(), errOut.String()
}
func TestBinaryOnlyThreeCommandsAndVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}, {"agent"}, {"serbe"}, {"version"}, {"--version", "extra"}} {
		code, _, stderr := binaryRun(t, builtOld, nil, args...)
		if code != 2 || stderr == "" {
			t.Fatalf("%v => %d %q", args, code, stderr)
		}
	}
	for _, cmd := range []string{"serve", "install", "emit"} {
		code, out, stderr := binaryRun(t, builtOld, nil, cmd, "--help")
		if code != 0 || out+stderr == "" {
			t.Fatalf("help %s: %d", cmd, code)
		}
	}
	code, out, _ := binaryRun(t, builtOld, nil, "--version")
	var got struct{ Version, Commit string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || got.Version != "old-v1" || got.Commit != "old-commit" {
		t.Fatalf("version: %d %s", code, out)
	}
}
func TestZeroSQLiteDependencyClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v %s", err, b)
	}
	for _, line := range strings.Fields(string(b)) {
		if strings.Contains(strings.ToLower(line), "sqlite") || strings.Contains(line, "anotify/internal/store") || strings.Contains(line, "anotify/internal/server") || strings.Contains(line, "anotify/internal/ws") || strings.Contains(line, "anotify/internal/lease") {
			t.Fatalf("forbidden dependency: %s", line)
		}
	}
}
func TestEmitEveryExitCodePrunedAndNoCredentialLeaks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, code int
		body         string
	}{
		{"task", 200, 0, `{"taskId":"down","to":"next","version":1}`},
		{"pruned", 200, 0, `{"taskId":null,"eventId":"ev","to":"next","version":1,"pruned":{"reason":"param_type"}}`},
		{"badrequest", 400, 2, `{"error":"target_not_declared"}`},
		{"paramsover", 413, 2, `{"error":"params_too_large","message":"params too large"}`},
		{"token", 401, 3, `{"error":"token_invalid"}`},
		{"late", 409, 4, `{"error":"task_not_running"}`},
		{"conflict", 409, 6, `{"error":"emit_id_conflict"}`},
		{"unknown", 502, 5, `{"error":"emit_unreachable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				b, _ := json.Marshal(payload)
				if r.URL.Path != "/emit" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fixture-secret" || strings.Contains(string(b), "fixture-secret") || len(payload) != 3 {
					t.Errorf("credential/payload boundary: %s %s", r.URL, b)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			t.Setenv("ANOTIFY_TASK_TOKEN", "fixture-secret")
			t.Setenv("ANOTIFY_EMIT_ENDPOINT", srv.URL)
			var out, stderr bytes.Buffer
			code := emit([]string{"next", "--id", "logical", "--data", `{"x":1}`}, &out, &stderr)
			if code != tc.code || calls.Load() != 1 || strings.Count(out.String(), "\n") != 1 || (code != 0 && strings.Count(stderr.String(), "\n") != 1) {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, calls.Load(), out.String(), stderr.String())
			}
			if strings.Contains(out.String()+stderr.String(), "fixture-secret") {
				t.Fatal("secret leaked")
			}
			if tc.name == "pruned" && !strings.Contains(out.String(), `"taskId":null`) {
				t.Fatal("pruned receipt lost")
			}
		})
	}
}

// TestEmitDataFileReadsFileAndStdin --data-file（文件 / stdin）避开 argv 的 ARG_MAX，
// 且 413 params_too_large 必须回 2（确定失败）而不是 5（结果未知）/#571。
func TestEmitDataFileReadsFileAndStdin(t *testing.T) {
	blob := strings.Repeat("a", 200<<10)
	payload := `{"blob":"` + blob + `"}`
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			To   string         `json:"to"`
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if got, _ := body.Data["blob"].(string); body.To != "sink" || len(got) != len(blob) {
			t.Errorf("--data-file 未原样上行：to=%q blob=%d", body.To, len(got))
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, `{"error":"params_too_large","message":"params too large: 205000 bytes, limit is 122880"}`)
	}))
	defer srv.Close()
	t.Setenv("ANOTIFY_TASK_TOKEN", "fixture-secret")
	t.Setenv("ANOTIFY_EMIT_ENDPOINT", srv.URL)

	file := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin bool
	}{
		{"file", []string{"sink", "--data-file", file}, false},
		{"stdin", []string{"sink", "--data-file", "-"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stdin {
				in, err := os.Open(file)
				if err != nil {
					t.Fatal(err)
				}
				saved := os.Stdin
				os.Stdin = in
				t.Cleanup(func() { os.Stdin = saved; _ = in.Close() })
			}
			var out, stderr bytes.Buffer
			code := emit(tc.args, &out, &stderr)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（确定性失败，不是 5）: %s%s", code, out.String(), stderr.String())
			}
			if !strings.Contains(out.String(), protocol.ReasonParamsTooLarge) || strings.Count(out.String(), "\n") != 1 {
				t.Fatalf("stdout 必须是一行 params_too_large: %q", out.String())
			}
			if strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("stderr 必须是一行人话: %q", stderr.String())
			}
		})
	}
	if calls.Load() != 2 {
		t.Fatalf("每个用例应当只发一次请求（不重试）：%d", calls.Load())
	}
}

func TestEmitDataFileConflictsAndReadErrors(t *testing.T) {
	t.Setenv("ANOTIFY_TASK_TOKEN", "fixture-secret")
	t.Setenv("ANOTIFY_EMIT_ENDPOINT", "http://127.0.0.1:1")
	for _, args := range [][]string{
		{"sink", "--data", "{}", "--data-file", "-"},
		{"sink", "--data-file", filepath.Join(t.TempDir(), "missing.json")},
	} {
		var out, stderr bytes.Buffer
		if code := emit(args, &out, &stderr); code != 2 {
			t.Fatalf("%v => 退出码 %d，期望 2: %s", args, code, out.String())
		}
		if !strings.Contains(out.String(), protocol.ReasonParamType) {
			t.Fatalf("%v => %q，期望 param_type", args, out.String())
		}
	}
}

func TestEmitInvalidDataReasonIsParamType(t *testing.T) {
	for _, data := range []string{"[1]", "1", "null", `"str"`, "{x", "{} {}"} {
		var out, stderr bytes.Buffer
		code := emit([]string{"sink", "--data", data}, &out, &stderr)
		if code != 2 {
			t.Fatalf("--data %q => 退出码 %d，期望 2", data, code)
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Error != protocol.ReasonParamType {
			t.Fatalf("--data %q => %q（解析错误 %v），期望 error=%s", data, out.String(), err, protocol.ReasonParamType)
		}
		if strings.Count(out.String(), "\n") != 1 || strings.Count(stderr.String(), "\n") != 1 {
			t.Fatalf("--data %q: stdout/stderr 各只能一行，实际 %q / %q", data, out.String(), stderr.String())
		}
	}
}
func TestEmitMissingTargetStrictObjectAndNetworkTimeout(t *testing.T) {
	t.Setenv("ANOTIFY_TASK_TOKEN", "secret")
	t.Setenv("ANOTIFY_EMIT_ENDPOINT", "http://127.0.0.1:1")
	for _, args := range [][]string{nil, {"--data", "{}"}, {"next", "--data", "null"}, {"next", "--data", "[]"}, {"next", "--data", "1"}, {"next", "--data", "{} {}"}} {
		var out, stderr bytes.Buffer
		code := emit(args, &out, &stderr)
		if code != 2 || strings.Count(out.String(), "\n") != 1 {
			t.Fatalf("%v: %d %s", args, code, out.String())
		}
	}
	var out, stderr bytes.Buffer
	if code := emit([]string{"--to", "next"}, &out, &stderr); code != 5 {
		t.Fatalf("network failure => %d", code)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer srv.Close()
	t.Setenv("ANOTIFY_EMIT_ENDPOINT", srv.URL)
	out.Reset()
	stderr.Reset()
	if code := emitWithTimeout([]string{"next"}, &out, &stderr, 20*time.Millisecond); code != 5 {
		t.Fatalf("timeout => %d", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("automatic retry: %d", calls.Load())
	}
}
func TestBinaryEmitUnix(t *testing.T) {
	socket := filepath.Join(shortTmpDir(t), "emit.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emit" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("Unix request boundary")
		}
		fmt.Fprint(w, `{"taskId":"unix-downstream","to":"next","version":1}`)
	})}
	go srv.Serve(l)
	defer srv.Close()
	code, out, stderr := binaryRun(t, builtOld, []string{"ANOTIFY_TASK_TOKEN=fixture-token", "ANOTIFY_EMIT_ENDPOINT=unix://" + socket}, "emit", "--to", "next", "--data", "{}")
	if code != 0 || !strings.Contains(out, "unix-downstream") || stderr != "" {
		t.Fatalf("Unix emit: %d %s %s", code, out, stderr)
	}
}
