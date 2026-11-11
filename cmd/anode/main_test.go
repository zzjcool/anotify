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
)

var builtOld, builtNew string

// shortTmpDir 是给 unix socket 这类受 `sun_path` 上限（Linux 上 108 字节）限制的路径用的**短**临时目录。
//
// 为什么不用 t.TempDir()：它把「测试名 + /001 序号」拼进路径。TMPDIR 一长（例如
// make ci 里 scripts/check-tmp-leaks.mjs 给的隔离 TMPDIR、或开发者自己设的
// 日志目录），`<tmp>/<测试名>/001/emit.sock` 就顶到上限，bind 直接报
// `invalid argument` —— 那是夹具对「TMPDIR 很短」的隐式假设，不是产品缺陷。
// socket 单独放这里（路径只多一个短前缀），其它文件仍用 t.TempDir()。
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
